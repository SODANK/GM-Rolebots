package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
)

// The online population cap: how many pilots this server carries at once, and which
// rolebots log off to keep it there.
// The cap counts every pilot, not just bots: a roster's seats are `cap - real players
// connected`, so the number an operator sets is the number the star map, the dashboard and
// Local report.
// A benched pilot is logged off, not disabled. Its roster row, profile, traits, state
// machine, market orders and skill training are untouched; it takes no sim turn, holds no
// presence (session registry, Local, corp chat, station guest list) and projects no ball.
// The cap does not stop the roster-wide sweeps (skills, wants, resting market orders, hub
// scores): a logged-off player still trains and their orders still fill.

// settingMaxOnline is the cap, persisted in server_settings. 0 (the default) is unlimited.
const settingMaxOnline = "rolebots.maxOnline"

const (
	// Anti-flap dwells: how long a pilot stays benched before it may return, and how long a
	// returned one is left alone before it may be benched again.
	popMinOfflineSec = 90
	popMinOnlineSec  = 60

	// The most pilots that may change state in one pass, so a large cap drop empties Local
	// gradually instead of in one frame.
	popMaxChurnPerPass = 20

	// Staleness is worth at most this many points when ranking who logs off first, so a
	// day-idle pilot does not outrank one that is idle, docked and alone.
	popMaxStalePoints = 120
)

var (
	popMu sync.Mutex
	// popOffline maps charID to the unix time it was benched.
	popOffline = map[int64]int64{}
	// popOnlineAt maps charID to the unix time it was last seen holding a seat. Seeded on
	// first sight, so a never-capped roster is not treated as freshly logged in.
	popOnlineAt = map[int64]int64{}
	// tzOff is the subset of popOffline the timezone gate put there (timezone.go), charID to
	// the unix time it logged off.
	tzOff = map[int64]int64{}

	// popCapValue is the cached setting: the sim reads it every pass.
	popCapValue atomic.Int64
)

// MaxOnline reports the configured cap (0 = unlimited).
func MaxOnline() int { return int(popCapValue.Load()) }

func setMaxOnline(n int) {
	if n < 0 {
		n = 0
	}
	popCapValue.Store(int64(n))
}

// loadMaxOnline reads the cap at boot. A missing or unparseable value means unlimited.
func (m *Mod) loadMaxOnline(ctx context.Context) {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingMaxOnline)
	if err != nil {
		rlogf("rolebots: read online cap: %v (leaving it unlimited)", err)
		return
	}
	if !ok {
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return
	}
	setMaxOnline(n)
}

// popIsOffline reports whether a pilot is currently benched by the cap.
func popIsOffline(charID int64) bool {
	popMu.Lock()
	defer popMu.Unlock()
	_, off := popOffline[charID]
	return off
}

// popOfflineCount is how many pilots the cap currently holds off the server.
func popOfflineCount() int {
	popMu.Lock()
	defer popMu.Unlock()
	return len(popOffline)
}

// realSessionCount is how many actual players are connected. Bot sessions carry the Bot
// flag and are excluded, or the roster would budget against itself and converge on half
// the cap.
func (m *Mod) realSessionCount() int {
	if m.sessions == nil {
		return 0
	}
	n := 0
	for _, s := range m.sessions.Snapshot() {
		if !s.Bot {
			n++
		}
	}
	return n
}

// popGroup is one benchable unit: a solo pilot or a whole gang. Gangs move together:
// members share one blackboard (rolebot_gangs.state) and one op, so benching part of a gang
// strands the rest.
type popGroup struct {
	key   int64
	bots  []*botRun
	score int
}

// popProtected reports whether a pilot must not be logged off right now because somebody
// is looking at it: a ship vanishing from a belt or a station guest list reads as a
// deletion. The cap is enforced elsewhere, so a brief overshoot is allowed.
func (m *Mod) popProtected(b *botRun) bool {
	if liveProjection(b.CharID) != nil {
		return true
	}
	// A booked Mind Clash fighter near its bell is never benched
	// (clash_travel.go): a no-show is a forfeit and the bettors' night.
	if clashErrandActive(b.CharID) {
		return true
	}
	// Nor a pilot answering a call to arms (armsping.go): it logged in for
	// the fight.
	if armsHeld(b.CharID) {
		return true
	}
	// Nor a pilot inside its crimewatch PvP window (crimewatch.go): a player
	// cannot log out of a fight either.
	if botInPvP(b.CharID) {
		return true
	}
	// Nor a pilot of a siege that is bashing in its system (possiege.go): the fleet stays until the
	// bash ends.
	if siegeHoldsPilot(b.CorpID, b.SystemID, clock.Unix()) {
		return true
	}
	if b.StationID != 0 && m.stationHasRealGuest(b.StationID) {
		return true
	}
	// A fleet a capsuleer sat down in (fleetmirror.go) is player-visible however it was formed.
	if b.GangID != 0 && mirrorHasRiders(b.GangID) {
		return true
	}
	// systemProjected, not systemWatched: liveProjection above covers a ring bot with a ball,
	// this covers one about to get a ball.
	return m.systemProjected(b.SystemID)
}

// popScore ranks a pilot for logging off; higher goes first. The order is who would be
// missed least: a halted pilot, then one in a hangar with no plan, then docked, then flying
// alone, then mid-job. Staleness breaks ties so the longest-idle pilot goes first, not the
// lowest characterID.
func popScore(b *botRun, now int64) int {
	s := 0
	if b.State.Halted {
		s += 1000
	}
	if b.State.Phase == "" || b.State.Phase == "idle" {
		s += 400
	}
	if b.StationID != 0 {
		s += 200
	}
	if b.GangID == 0 {
		s += 50
	}
	if started := b.State.Data.ActivityStartedAt; started > 0 && now > started {
		mins := int((now - started) / 60)
		if mins > popMaxStalePoints {
			mins = popMaxStalePoints
		}
		s += mins
	}
	// Just came back: eligible but last in the queue, so a player count on the cap boundary
	// does not bounce the same pilot.
	popMu.Lock()
	on := popOnlineAt[b.CharID]
	popMu.Unlock()
	if on > 0 && now-on < popMinOnlineSec {
		s -= 500
	}
	return s
}

// applyPopCap decides who is on the server this pass and returns the pilots that get a sim
// turn. Pilots outside their hours are logged off first (applyTimezones), then the cap
// applies to the rest. Benches and restores go in the module's offline set, which
// presence.go and embody.go read.
func (m *Mod) applyPopCap(bots []*botRun) []*botRun {
	// The sim's clock, not the wall's: benching cadence must follow the
	// same time the rest of the pass runs on, and the harness fakes it.
	now := clock.Unix()

	// Forget pilots that left the roster entirely, so the offline set does not keep disabled
	// or purged characters.
	live := make(map[int64]bool, len(bots))
	for _, b := range bots {
		live[b.CharID] = true
	}
	popMu.Lock()
	for id := range popOffline {
		if !live[id] {
			delete(popOffline, id)
		}
	}
	for id := range popOnlineAt {
		if !live[id] {
			delete(popOnlineAt, id)
		}
	}
	for id := range tzOff {
		if !live[id] {
			delete(tzOff, id)
		}
	}
	popMu.Unlock()

	// Hours first (timezone.go): a pilot outside them is logged off whatever
	// the cap says, and the cap divides its seats among the rest.
	bots = m.applyTimezones(bots, now, false)

	limit := MaxOnline()
	if limit <= 0 {
		m.popRestoreAll(bots, now)
		return bots
	}

	budget := limit - m.realSessionCount()
	if budget < 0 {
		budget = 0
	}

	var online, benched []*botRun
	popMu.Lock()
	for _, b := range bots {
		if _, off := popOffline[b.CharID]; off && (clashErrandActive(b.CharID) || armsHeld(b.CharID)) {
			// A booked fighter (clash_travel.go) or a pilot answering a call to arms (armsping.go)
			// takes a seat ahead of any idle bot.
			delete(popOffline, b.CharID)
			popOnlineAt[b.CharID] = now
		}
		if _, off := popOffline[b.CharID]; off {
			benched = append(benched, b)
			continue
		}
		if popOnlineAt[b.CharID] == 0 {
			popOnlineAt[b.CharID] = now
		}
		online = append(online, b)
	}
	popMu.Unlock()

	switch {
	case len(online) > budget:
		online = m.popBench(online, len(online)-budget, budget, limit, now)
	case len(online) < budget && len(benched) > 0:
		online = append(online, m.popRestore(benched, budget-len(online), limit, now)...)
	}
	return online
}

// popRestoreAll brings everybody back, which is what an unlimited cap means. It ignores the
// anti-flap dwell because it answers to a setting change, not a seat count.
func (m *Mod) popRestoreAll(bots []*botRun, now int64) {
	popMu.Lock()
	if len(popOffline) == 0 {
		popMu.Unlock()
		return
	}
	var back []int64
	for _, b := range bots {
		if _, off := popOffline[b.CharID]; off {
			delete(popOffline, b.CharID)
			popOnlineAt[b.CharID] = now
			back = append(back, b.CharID)
		}
	}
	popMu.Unlock()
	for _, id := range back {
		m.logBot(id, logInfo, "presence", "logged back in — the online cap is off")
	}
}

// popGroupBots buckets pilots into benchable units, gangs whole.
func popGroupBots(bots []*botRun, now int64) []popGroup {
	byKey := map[int64]*popGroup{}
	var order []int64
	for _, b := range bots {
		k := shardKey(b)
		g, ok := byKey[k]
		if !ok {
			g = &popGroup{key: k}
			byKey[k] = g
			order = append(order, k)
		}
		g.bots = append(g.bots, b)
	}
	out := make([]popGroup, 0, len(order))
	for _, k := range order {
		g := byKey[k]
		// A unit is ranked by its members' average idleness, not their sum, so headcount alone
		// does not rank a busy gang above an idle solo pilot.
		total := 0
		for _, b := range g.bots {
			total += popScore(b, now)
		}
		g.score = total / len(g.bots)
		out = append(out, *g)
	}
	return out
}

// popBench logs off `need` pilots (or as near as the protections allow) and
// returns what is left online.
func (m *Mod) popBench(online []*botRun, need, budget, limit int, now int64) []*botRun {
	if need > popMaxChurnPerPass {
		need = popMaxChurnPerPass
	}
	groups := popGroupBots(online, now)

	// Protected groups are out entirely: one watched member protects its gang.
	eligible := make([]popGroup, 0, len(groups))
	for _, g := range groups {
		safe := true
		for _, b := range g.bots {
			if m.popProtected(b) {
				safe = false
				break
			}
		}
		if safe {
			eligible = append(eligible, g)
		}
	}
	// Idlest first; among equals the smaller unit, so shedding one seat does not take a
	// five-man gang when a solo pilot would do.
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].score != eligible[j].score {
			return eligible[i].score > eligible[j].score
		}
		return len(eligible[i].bots) < len(eligible[j].bots)
	})

	gone := map[int64]bool{}
	taken := 0
	var fallback *popGroup
	for i := range eligible {
		g := &eligible[i]
		if taken >= need {
			break
		}
		if len(g.bots) > need-taken {
			// Too big for the seats still to free. Keep the best one in case nothing smaller turns up:
			// overshooting the cap downward is allowed, missing it is not.
			if fallback == nil {
				fallback = g
			}
			continue
		}
		for _, b := range g.bots {
			gone[b.CharID] = true
		}
		taken += len(g.bots)
	}
	if taken == 0 && fallback != nil {
		for _, b := range fallback.bots {
			gone[b.CharID] = true
		}
		taken = len(fallback.bots)
	}
	if taken == 0 {
		return online
	}

	kept := make([]*botRun, 0, len(online)-taken)
	popMu.Lock()
	for _, b := range online {
		if gone[b.CharID] {
			popOffline[b.CharID] = now
			delete(popOnlineAt, b.CharID)
			continue
		}
		kept = append(kept, b)
	}
	popMu.Unlock()

	for _, b := range online {
		if gone[b.CharID] {
			m.logBot(b.CharID, logInfo, "presence",
				"logged off — %s (cap %d, %d seat(s) for bots)",
				popReason(b), limit, budget)
		}
	}
	return kept
}

// popReason words why this pilot was chosen, in the terms the ranking used, so the log
// needs no reverse-engineering of the score.
func popReason(b *botRun) string {
	switch {
	case b.State.Halted:
		return "halted, so it was doing nothing"
	case b.State.Phase == "" || b.State.Phase == "idle":
		if b.StationID != 0 {
			return "idle in a hangar"
		}
		return "idle"
	case b.StationID != 0:
		return "docked"
	default:
		return "the least busy pilot left"
	}
}

// popRestore brings benched pilots back, oldest bench first, so a pilot benched early is
// not benched forever. A gang comes back whole, as it went.
func (m *Mod) popRestore(benched []*botRun, seats, limit int, now int64) []*botRun {
	if seats > popMaxChurnPerPass {
		seats = popMaxChurnPerPass
	}

	popMu.Lock()
	type cand struct {
		b    *botRun
		when int64
	}
	var queue []cand
	for _, b := range benched {
		when := popOffline[b.CharID]
		if now-when < popMinOfflineSec {
			continue
		}
		queue = append(queue, cand{b, when})
	}
	popMu.Unlock()
	if len(queue) == 0 {
		return nil
	}
	sort.SliceStable(queue, func(i, j int) bool { return queue[i].when < queue[j].when })

	// Gangmates share a key, so a restored pilot pulls its benched gang back with it. That can
	// overshoot the free seats; the next pass corrects it.
	groups := map[int64][]*botRun{}
	var order []int64
	for _, c := range queue {
		k := shardKey(c.b)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], c.b)
	}

	var back []*botRun
	for _, k := range order {
		if len(back) >= seats {
			break
		}
		back = append(back, groups[k]...)
	}
	if len(back) == 0 {
		return nil
	}

	popMu.Lock()
	for _, b := range back {
		delete(popOffline, b.CharID)
		popOnlineAt[b.CharID] = now
	}
	popMu.Unlock()

	for _, b := range back {
		m.logBot(b.CharID, logInfo, "presence", "logged back in — a seat opened up under the cap of %d", limit)
	}
	return back
}

// PopStatus is what the panel reports: the cap, who is holding its seats,
// and how many pilots it is keeping off the server.
type PopStatus struct {
	Cap         int
	Players     int
	BotsOnline  int
	BotsBenched int
	Enabled     int // enabled rolebots on the roster
	Seats       int // seats currently available to bots
}

// popStatus counts the live picture for the panel. The enabled figure comes
// from the database rather than from the sim's last pass, so the page reads
// the same before the first pass as after it.
func (m *Mod) popStatus(ctx context.Context) (PopStatus, error) {
	st := PopStatus{Cap: MaxOnline(), Players: m.realSessionCount()}
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_characters WHERE enabled = 1`).Scan(&st.Enabled); err != nil {
		return st, fmt.Errorf("popStatus: %w", err)
	}
	st.BotsBenched = popOfflineCount()
	st.BotsOnline = st.Enabled - st.BotsBenched
	if st.BotsOnline < 0 {
		st.BotsOnline = 0
	}
	if !Master() {
		st.BotsOnline = 0
	}
	if st.Cap > 0 {
		st.Seats = st.Cap - st.Players
		if st.Seats < 0 {
			st.Seats = 0
		}
	}
	return st, nil
}

func init() {
	registerCharForget("popOffline/popOnlineAt", func(id int64) {
		popMu.Lock()
		delete(popOffline, id)
		delete(popOnlineAt, id)
		delete(tzOff, id)
		popMu.Unlock()
	})
}