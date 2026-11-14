package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/server/gangsta"
)

// A module fleet (rolebot_gangs) given a real gang so a human member of the corporation
// can sit in it. On a seat request from the Corporate Portal the commander opens a real
// gang with itself as leader, seats the fleet's bots and invites the human. The commander
// then narrates (narrateFleet) and takes orders from a Director aboard
// (fleetmirror_orders.go).
//
// The human is a rider, not a conscript (conscript.go is the opposite direction): the bots
// keep running gangPlan, and the undriven rider row in rolebot_gang_members lets haulers
// empty the human's cans. Nothing is persisted: gang.Registry restarts its IDs at 1 each
// boot, so a mirror is rebuilt from the next seat request and rider rows are purged.

// gangOpSettled is the phase the settlement round leaves behind (gangplan.go
// stamps it as a literal); the sweep deletes the gang right after.
const gangOpSettled = "settled"

const (
	settingFleetMirror     = "rolebots.fleet.mirror"
	settingFleetNarration  = "rolebots.fleet.narration"
	settingSeatRequestTTL  = "rolebots.fleet.seatRequestTTLSec"
	defaultSeatRequestTTL  = 600
	fleetMirrorEveryTicks  = 5
	fleetMirrorLaneTimeout = 60 * time.Second
)

var (
	fleetMirrorOn    atomic.Bool
	fleetNarrationOn atomic.Bool
	seatRequestTTL   atomic.Int64
	fleetMirrorBusy  atomic.Bool
	fleetMirrorOnce  sync.Once
)

func FleetMirror() bool      { return fleetMirrorOn.Load() }
func FleetNarration() bool   { return fleetNarrationOn.Load() }
func setFleetMirror(on bool) { fleetMirrorOn.Store(on) }
func setFleetNarration(on bool) {
	fleetNarrationOn.Store(on)
}

// loadFleetSettings reads the switches at boot. Fleet mirror and narration default on.
func (m *Mod) loadFleetSettings(ctx context.Context) {
	setFleetMirror(true)
	setFleetNarration(true)
	seatRequestTTL.Store(defaultSeatRequestTTL)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingFleetMirror); err == nil && ok {
		setFleetMirror(truthySetting(v))
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingFleetNarration); err == nil && ok {
		setFleetNarration(truthySetting(v))
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingFleetPointCalls); err == nil && ok {
		pointCallsOn.Store(truthySetting(v))
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingSeatRequestTTL); err == nil && ok {
		var n int64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n); err == nil && n > 0 {
			seatRequestTTL.Store(n)
		}
	}
}

// fleetMirror is one module fleet with a real gang open.
type fleetMirror struct {
	ModGangID   int64
	RealGangID  int64
	LeaderBotID int64
	CorpID      int64
	Bots        map[int64]bool  // bots the mirror put in the real gang
	Riders      map[int64]int64 // human charID to when they sat down
	Phase       string          // the last lifecycle stage narrated
	SiteKey     string          // the last site narrated
	LeftSaid    bool
	CreatedAt   int64
	LastSync    int64

	// HumanFC is the officer who took command (fleetmirror_command.go) and leads the real gang,
	// 0 while the bot commander leads. FCSince is when; FollowFC moves the op wherever the FC
	// goes; Benched are the bots the FC kicked, kept out until command is handed back. Written
	// under mirrorMu.
	HumanFC  int64
	FCSince  int64
	FollowFC bool
	Benched  map[int64]bool
}

// mirrorHumanFC is the human in command of a mirrored fleet, or 0.
func mirrorHumanFC(fm *fleetMirror) int64 {
	if fm == nil {
		return 0
	}
	mirrorMu.RLock()
	defer mirrorMu.RUnlock()
	return fm.HumanFC
}

// seatRequest is a human who asked for a seat and has been invited: the
// reconcile turns their acceptance into a rider row with this role.
type seatRequest struct {
	ModGangID int64
	Role      string
	At        int64
}

var (
	mirrorMu     sync.RWMutex
	mirrors      = map[int64]*fleetMirror{} // by module gang
	mirrorByReal = map[int64]int64{}        // real gang to module gang
	seatRequests = map[int64]seatRequest{}  // by human
)

// mirrorOf is the mirror for a module fleet, or nil. The pointer is shared;
// only the reconcile lane writes through it.
func mirrorOf(modGangID int64) *fleetMirror {
	mirrorMu.RLock()
	defer mirrorMu.RUnlock()
	return mirrors[modGangID]
}

// mirrorByRealGang is the mirror behind a real gang, or nil.
func mirrorByRealGang(realGangID int64) *fleetMirror {
	mirrorMu.RLock()
	defer mirrorMu.RUnlock()
	if id, ok := mirrorByReal[realGangID]; ok {
		return mirrors[id]
	}
	return nil
}

// mirrorHasRiders reports whether a human sits in this module fleet now. The population
// cap must honour it: a fleet a player is watching is player-visible however it formed.
func mirrorHasRiders(modGangID int64) bool {
	mirrorMu.RLock()
	defer mirrorMu.RUnlock()
	fm := mirrors[modGangID]
	return fm != nil && len(fm.Riders) > 0
}

// mirroredFleets is a snapshot for the ESP page: module gang → (real gang,
// riders aboard).
func mirroredFleets() map[int64][2]int {
	mirrorMu.RLock()
	defer mirrorMu.RUnlock()
	out := make(map[int64][2]int, len(mirrors))
	for id, fm := range mirrors {
		out[id] = [2]int{int(fm.RealGangID), len(fm.Riders)}
	}
	return out
}

func dropMirror(fm *fleetMirror) {
	mirrorMu.Lock()
	defer mirrorMu.Unlock()
	if cur := mirrors[fm.ModGangID]; cur == fm {
		delete(mirrors, fm.ModGangID)
	}
	if id, ok := mirrorByReal[fm.RealGangID]; ok && id == fm.ModGangID {
		delete(mirrorByReal, fm.RealGangID)
	}
}

// ── opening a fleet ─────────────────────────────────────────────────────────

// mirrorEnsure opens the real gang for a module fleet, or returns the one
// already open. The refusal is a sentence for the page.
func (m *Mod) mirrorEnsure(ctx context.Context, g *Gang) (*fleetMirror, string) {
	if !FleetMirror() {
		return nil, "Fleet seats are not being offered at the moment."
	}
	if g == nil || !g.Enabled || g.Activity == "" || g.LeaderCharID == 0 {
		return nil, "That fleet is standing down."
	}
	if fm := mirrorOf(g.GangID); fm != nil {
		if gangsta.GangOf(fm.LeaderBotID) == fm.RealGangID {
			return fm, ""
		}
		dropMirror(fm) // the registry dissolved it; open a fresh one below
	}
	if other := gangsta.GangOf(g.LeaderCharID); other != 0 {
		return nil, "The commander is already flying with someone else."
	}
	mems, err := gangMembers(ctx, m.db, g.GangID)
	if err != nil {
		return nil, "The fleet roster could not be read."
	}
	var driven []GangMember
	for _, mem := range mems {
		if mem.Driven {
			driven = append(driven, mem)
		}
	}
	if len(driven) < 2 {
		return nil, "That fleet has nobody aboard yet."
	}
	real := gangsta.BotCreateGang(g.LeaderCharID, g.CorpID)
	if real == 0 {
		return nil, "The fleet window could not be opened."
	}
	fm := &fleetMirror{
		ModGangID: g.GangID, RealGangID: real, LeaderBotID: g.LeaderCharID, CorpID: g.CorpID,
		Bots: map[int64]bool{g.LeaderCharID: true}, Riders: map[int64]int64{},
		CreatedAt: clock.Unix(),
	}
	m.joinFleetChat(ctx, real, g.LeaderCharID)
	for _, mem := range driven {
		if mem.CharacterID == g.LeaderCharID {
			continue
		}
		m.mirrorSeatBot(ctx, fm, mem.CharacterID)
	}
	mirrorMu.Lock()
	mirrors[g.GangID] = fm
	mirrorByReal[real] = g.GangID
	mirrorMu.Unlock()
	m.logBot(g.LeaderCharID, logInfo, "fleet",
		"opened the fleet window for %q (fleet %d) — %d pilots aboard", g.Name, real, len(fm.Bots))
	return fm, ""
}

// mirrorSeatBot puts one bot in the real gang and its chat.
func (m *Mod) mirrorSeatBot(ctx context.Context, fm *fleetMirror, botID int64) {
	if gangsta.GangOf(botID) != 0 {
		return // flying for a human already (conscripted); it keeps that seat
	}
	_, corpID := m.charNameCorp(ctx, botID)
	if err := gangsta.BotJoin(fm.RealGangID, botID, corpID); err != nil {
		rlogf("rolebots: fleet %d seat %d: %v", fm.RealGangID, botID, err)
		return
	}
	m.joinFleetChat(ctx, fm.RealGangID, botID)
	fm.Bots[botID] = true
}

// requestSeat is the portal's "Request a seat": the commander invites the
// human into the fleet's real gang. The answer is a sentence for the page;
// empty means the invitation went out.
func (m *Mod) requestSeat(ctx context.Context, g *Gang, humanID int64, role string) string {
	if humanID == 0 {
		return "Open this page from your own terminal to request a seat."
	}
	if m.isSynthetic(humanID) {
		return "That seat is for a capsuleer."
	}
	if cur := gangsta.GangOf(humanID); cur != 0 {
		if fm := mirrorOf(g.GangID); fm != nil && fm.RealGangID == cur {
			return "You are already aboard."
		}
		return "Leave your current fleet first."
	}
	if role == "" || !validRole(role) {
		role = defaultRiderRole(g.Activity)
	}
	fm, refusal := m.mirrorEnsure(ctx, g)
	if refusal != "" {
		return refusal
	}
	leaderName, _ := m.charNameCorp(ctx, fm.LeaderBotID)
	if !gangsta.BotInvite(fm.RealGangID, fm.LeaderBotID, humanID, leaderName) {
		return "You are not at a terminal the invitation can reach — undock or dock and try again."
	}
	mirrorMu.Lock()
	seatRequests[humanID] = seatRequest{ModGangID: g.GangID, Role: role, At: clock.Unix()}
	mirrorMu.Unlock()
	m.logBot(fm.LeaderBotID, logInfo, "fleet", "invited %s to %q as %s",
		m.charNameOr(ctx, humanID), g.Name, role)
	return ""
}

// defaultRiderRole is the seat a human takes when they named none: what the
// op is doing.
func defaultRiderRole(activity string) string {
	switch activity {
	case ActMining, ActIndMining:
		return RoleMiner
	}
	return RoleCombat
}

// ── the lane ────────────────────────────────────────────────────────────────

// startFleetMirror registers the reconcile lane: every few ticks each open
// mirror is checked against the module fleet it stands for and the registry
// gang it opened, and the two are brought into line.
func (m *Mod) startFleetMirror() {
	if m.tick == nil {
		return
	}
	fleetMirrorOnce.Do(func() {
		m.registerPeriodic("rolebots-fleet-mirror", fleetMirrorEveryTicks, func(uint64) bool {
			if !Master() {
				return true
			}
			if !fleetMirrorBusy.CompareAndSwap(false, true) {
				return true
			}
			go func() {
				defer fleetMirrorBusy.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), fleetMirrorLaneTimeout)
				defer cancel()
				m.fleetMirrorPass(ctx)
			}()
			return true
		})
	})
}

func (m *Mod) fleetMirrorPass(ctx context.Context) {
	mirrorMu.RLock()
	list := make([]*fleetMirror, 0, len(mirrors))
	for _, fm := range mirrors {
		list = append(list, fm)
	}
	now := clock.Unix()
	ttl := seatRequestTTL.Load()
	var stale []int64
	for h, req := range seatRequests {
		if now-req.At > ttl {
			stale = append(stale, h)
		}
	}
	mirrorMu.RUnlock()
	if len(stale) > 0 {
		mirrorMu.Lock()
		for _, h := range stale {
			delete(seatRequests, h)
		}
		mirrorMu.Unlock()
	}
	for _, fm := range list {
		if ctx.Err() != nil {
			return
		}
		m.reconcileMirror(ctx, fm)
	}
}

// reconcileMirror is one mirror brought into line, in the order that matters:
// a fleet that is gone closes its window; a window the registry closed drops
// the mirror; a new commander takes the seat; the bot roster is diffed; the
// riders are diffed; then what changed is said.
func (m *Mod) reconcileMirror(ctx context.Context, fm *fleetMirror) {
	now := clock.Unix()
	g, err := getGang(ctx, m.db, fm.ModGangID)
	if err != nil {
		return
	}
	if g == nil || !g.Enabled || g.Activity == "" || g.State.OpPhase == gangOpSettled {
		if !fm.LeftSaid && len(gangsta.HumanMembersOnline(fm.RealGangID)) > 0 {
			m.speakFleet(ctx, fm.RealGangID, fm.LeaderBotID, true, "Settled up — standing down. Thanks, all.")
			fm.LeftSaid = true
		}
		m.closeMirror(ctx, fm, "the fleet stood down")
		return
	}
	if gangsta.GangOf(fm.LeaderBotID) != fm.RealGangID {
		// The registry dissolved it (the last human left, or the commander was conscripted
		// away). The module fleet is untouched; a new seat request opens a fresh window.
		for id := range fm.Bots {
			m.leaveFleetChat(fm.RealGangID, id)
		}
		dropMirror(fm)
		m.logBot(fm.LeaderBotID, logInfo, "fleet", "fleet window %d closed by the registry", fm.RealGangID)
		return
	}

	// A human in command who is no longer aboard has handed it back (fleetmirror_command.go).
	if fc := mirrorHumanFC(fm); fc != 0 && gangsta.GangOf(fc) != fm.RealGangID {
		m.mirrorHandBack(ctx, fm, "the FC left the fleet")
	}

	// A new commander. While a human commands, the real gang stays theirs.
	if g.LeaderCharID != 0 && g.LeaderCharID != fm.LeaderBotID {
		old := fm.LeaderBotID
		if !fm.Bots[g.LeaderCharID] {
			m.mirrorSeatBot(ctx, fm, g.LeaderCharID)
		}
		if mirrorHumanFC(fm) != 0 {
			if fm.Bots[g.LeaderCharID] {
				fm.LeaderBotID = g.LeaderCharID
			}
		} else if fm.Bots[g.LeaderCharID] && gangsta.BotMakeLeader(fm.RealGangID, g.LeaderCharID) {
			fm.LeaderBotID = g.LeaderCharID
			m.narrateMirror(ctx, fm, g.LeaderCharID, true, "Taking command — %s stood down.", m.charNameOr(ctx, old))
		}
	}
	m.mirrorFollowFC(ctx, fm, g)

	// The bot roster.
	mems, err := gangMembers(ctx, m.db, fm.ModGangID)
	if err != nil {
		return
	}
	want := map[int64]GangMember{}
	for _, mem := range mems {
		if mem.Driven {
			want[mem.CharacterID] = mem
		}
	}
	for id := range fm.Bots {
		if _, ok := want[id]; ok {
			continue
		}
		if gangsta.GangOf(id) == fm.RealGangID {
			gangsta.BotLeave(id)
		}
		m.leaveFleetChat(fm.RealGangID, id)
		delete(fm.Bots, id)
	}
	for id := range want {
		if !fm.Bots[id] {
			m.mirrorSeatBot(ctx, fm, id)
		}
	}
	if gangsta.GangOf(fm.LeaderBotID) != fm.RealGangID {
		dropMirror(fm) // the diff dissolved it; the next pass sees a clean slate
		return
	}

	// The riders: whoever is in the real gang and is not one of our bots.
	aboard := map[int64]bool{}
	for _, id := range gangsta.BotMembers(fm.RealGangID) {
		if fm.Bots[id] || m.isSynthetic(id) {
			continue
		}
		aboard[id] = true
		if _, ok := fm.Riders[id]; ok {
			continue
		}
		role := defaultRiderRole(g.Activity)
		mirrorMu.Lock()
		if req, ok := seatRequests[id]; ok && req.ModGangID == fm.ModGangID {
			role = req.Role
		}
		delete(seatRequests, id)
		mirrorMu.Unlock()
		if err := addGangRider(ctx, m.db, fm.ModGangID, id, role); err != nil {
			rlogf("rolebots: seat rider %d in gang %d: %v", id, fm.ModGangID, err)
		}
		fm.Riders[id] = now
		m.logBot(fm.LeaderBotID, logInfo, "fleet", "%s sat down in %q as %s", m.charNameOr(ctx, id), g.Name, role)
		m.narrateMirror(ctx, fm, fm.LeaderBotID, true, "Welcome aboard, %s. %s", m.charNameOr(ctx, id), m.fleetSitrep(ctx, g))
		// How the fleet works, by mail, once per kind and version (fcmanual.go). An officer gets
		// the chair's version, which covers taking command.
		kind := manualRider
		if m.isCorpOfficer(ctx, fm.CorpID, id) {
			kind = manualDirector
		}
		m.sendFCManual(ctx, fm.LeaderBotID, id, kind, false)
	}
	for id := range fm.Riders {
		if aboard[id] {
			continue
		}
		if err := removeGangMember(ctx, m.db, fm.ModGangID, id); err != nil {
			rlogf("rolebots: unseat rider %d from gang %d: %v", id, fm.ModGangID, err)
		}
		delete(fm.Riders, id)
		m.logBot(fm.LeaderBotID, logInfo, "fleet", "%s left %q", m.charNameOr(ctx, id), g.Name)
	}

	// What changed since the last look.
	if key := fleetPhaseKey(g); key != fm.Phase {
		if fm.Phase != "" {
			m.narratePhase(ctx, fm, g)
		}
		fm.Phase = key
	}
	if key := fleetSiteKey(g); key != fm.SiteKey {
		if fm.SiteKey != "" || fm.Phase == gangOpLive {
			m.narrateSite(ctx, fm, g)
		}
		fm.SiteKey = key
	}
	fm.LastSync = now
}

// closeMirror disbands the real gang and forgets the mirror.
func (m *Mod) closeMirror(ctx context.Context, fm *fleetMirror, why string) {
	gangsta.BotDisband(fm.RealGangID)
	for id := range fm.Bots {
		m.leaveFleetChat(fm.RealGangID, id)
	}
	for id := range fm.Riders {
		_ = removeGangMember(ctx, m.db, fm.ModGangID, id)
	}
	dropMirror(fm)
	m.logBot(fm.LeaderBotID, logInfo, "fleet", "closed fleet window %d — %s", fm.RealGangID, why)
}

// handleFleetMirrorPost saves the Fleet Seats card on /rolebots/gangs. It stores each
// value and applies it in the same call, since the atomics are only read by the loader.
func (m *Mod) handleFleetMirrorPost(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	mirror := r.FormValue("mirror") == "on"
	narration := r.FormValue("narration") == "on"
	pointCalls := r.FormValue("pointCalls") == "on"
	ttl := int64(defaultSeatRequestTTL)
	if n, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("seatTTL")), 10, 64); err == nil && n >= 60 {
		ttl = n
	}
	bit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	orders := r.FormValue("orders") == "on"
	dial := func(field string, def, min int64) int64 {
		n, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(field)), 10, 64)
		if err != nil || n < min {
			return def
		}
		return n
	}
	ttlH := dial("orderTTL", defaultOrderTTLHours, 1)
	retries := dial("orderRetries", defaultOrderRetries, 1)
	maxOpen := dial("fleetMaxOpen", defaultHumanFleetMaxOpen, 1)
	holdMax := dial("holdMax", defaultDirectiveHoldMax, 1)
	for _, kv := range [][2]string{
		{settingFleetMirror, bit(mirror)},
		{settingFleetNarration, bit(narration)},
		{settingFleetPointCalls, bit(pointCalls)},
		{settingSeatRequestTTL, strconv.FormatInt(ttl, 10)},
		{settingCorpOrders, bit(orders)},
		{settingOrderTTLHours, strconv.FormatInt(ttlH, 10)},
		{settingOrderRetries, strconv.FormatInt(retries, 10)},
		{settingHumanFleetMaxOpen, strconv.FormatInt(maxOpen, 10)},
		{settingDirectiveHoldMax, strconv.FormatInt(holdMax, 10)},
	} {
		if err := evedb.SetServerSetting(ctx, m.db, kv[0], kv[1]); err != nil {
			redirect(w, r, "/rolebots/gangs", "", err.Error())
			return
		}
	}
	setFleetMirror(mirror)
	setFleetNarration(narration)
	pointCallsOn.Store(pointCalls)
	seatRequestTTL.Store(ttl)
	corpOrdersOn.Store(orders)
	orderTTLHours.Store(ttlH)
	orderRetries.Store(retries)
	humanFleetMaxOpen.Store(maxOpen)
	directiveHoldMax.Store(holdMax)
	redirect(w, r, "/rolebots/gangs", "fleet seat and order settings saved", "")
}

// ── riders in the roster table ──────────────────────────────────────────────

// addGangRider seats a human in a module fleet's roster as an undriven
// member, marked so the boot purge can tell it from a row the operator made.
func addGangRider(ctx context.Context, db *sql.DB, gangID, charID int64, role string) error {
	if !validRole(role) {
		role = RoleCombat
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_gang_members (gangID, characterID, role, recruitCorpID, addedDate, rider)
		VALUES (?, ?, ?, 0, ?, 1)
		ON DUPLICATE KEY UPDATE role = VALUES(role), rider = 1`,
		gangID, charID, role, clock.Unix()); err != nil {
		return fmt.Errorf("addGangRider: %w", err)
	}
	return nil
}

// purgeFleetRiders removes every portal-seated rider at boot: the real gangs
// they sat in died with the process, and a rider row with no gang behind it
// would hold a seat (and a share of the haul) for somebody who is not there.
func (m *Mod) purgeFleetRiders(ctx context.Context) {
	res, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_gang_members WHERE rider = 1`)
	if err != nil {
		rlogf("rolebots: boot rider purge: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		rlogf("rolebots: boot rider purge: %d seat(s) released", n)
	}
}

// ── narration ───────────────────────────────────────────────────────────────

// narrateMirror says one line in the mirror's fleet chat as the given pilot,
// only while a human is listening and the switch is on.
func (m *Mod) narrateMirror(ctx context.Context, fm *fleetMirror, speaker int64, urgent bool, format string, args ...any) bool {
	if fm == nil || !FleetNarration() {
		return false
	}
	if len(gangsta.HumanMembersOnline(fm.RealGangID)) == 0 {
		return false
	}
	line := fmt.Sprintf(format, args...)
	if !m.speakFleet(ctx, fm.RealGangID, speaker, urgent, line) {
		return false
	}
	m.logBot(speaker, logInfo, "fleet", "said: %s", line)
	return true
}

// narrateFleet is the commander's voice for a module fleet, from anywhere in the gang
// planners: a no-op for a fleet with no window open, at the cost of one map read. Callers
// pass the fleet; the speaker is its commander.
func (m *Mod) narrateFleet(ctx context.Context, gangID int64, urgent bool, format string, args ...any) bool {
	fm := mirrorOf(gangID)
	if fm == nil {
		return false
	}
	return m.narrateMirror(ctx, fm, fm.LeaderBotID, urgent, format, args...)
}

// narrateMember is one member's own line (a hauler loaded, a miner's can full, a seat
// excused). It is never urgent, so it yields to the commander.
func (m *Mod) narrateMember(ctx context.Context, gangID, charID int64, format string, args ...any) bool {
	if gangID == 0 || charID == 0 {
		return false
	}
	fm := mirrorOf(gangID)
	if fm == nil || !fm.Bots[charID] {
		return false
	}
	return m.narrateMirror(ctx, fm, charID, false, format, args...)
}

// fleetPhaseKey / fleetSiteKey are what the reconcile compares to notice a
// change it did not see happen (a planner that moved the site without
// narrating, a sweep that wound the fleet down).
func fleetPhaseKey(g *Gang) string {
	if g.State.OpPhase == "" {
		return gangOpLive
	}
	return g.State.OpPhase
}

func fleetSiteKey(g *Gang) string {
	switch g.Activity {
	case ActMining, ActIndMining:
		return fmt.Sprintf("mine:%d:%d:%d", g.State.SystemID, g.State.BeltItemID, g.State.OreTypeID)
	case ActCombat, ActSecurity:
		return fmt.Sprintf("hunt:%d", g.State.HuntSystemID)
	case ActPiracy:
		return fmt.Sprintf("raid:%d:%d", g.State.PirSystemID, g.State.PirGateID)
	}
	return ""
}

func (m *Mod) narratePhase(ctx context.Context, fm *fleetMirror, g *Gang) {
	switch g.State.OpPhase {
	case gangOpLive:
		m.narrateMirror(ctx, fm, fm.LeaderBotID, true, "Fleet's formed up at %s — undocking. %s",
			m.stationLabel(ctx, g.State.StagingStationID), m.fleetSitrep(ctx, g))
	case gangOpWindDown:
		m.narrateMirror(ctx, fm, fm.LeaderBotID, true, "Winding down — back to %s to settle up.",
			m.stationLabel(ctx, g.State.StagingStationID))
	}
}

func (m *Mod) narrateSite(ctx context.Context, fm *fleetMirror, g *Gang) {
	switch g.Activity {
	case ActMining, ActIndMining:
		if g.State.SystemID != 0 {
			m.narrateMirror(ctx, fm, fm.LeaderBotID, true, "Warping to %s — %s, cans on belt %d.",
				g.State.SystemName, g.State.OreName, g.State.BeltItemID)
		}
	case ActCombat, ActSecurity:
		if g.State.HuntSystemID != 0 {
			verb := "sweep"
			if g.Activity == ActCombat {
				verb = "hunt"
			}
			m.narrateMirror(ctx, fm, fm.LeaderBotID, true, "Moving the %s to %s.", verb, g.State.HuntSystemName)
		}
	case ActPiracy:
		if g.State.PirSystemID != 0 {
			m.narrateMirror(ctx, fm, fm.LeaderBotID, true, "Hunting ground is %s.", g.State.PirSystemName)
		}
	}
}

// fleetSitrep is the one-line state of a fleet, for a welcome and for
// "status": what it is doing, where, with how many.
func (m *Mod) fleetSitrep(ctx context.Context, g *Gang) string {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_gang_members gm
		  JOIN rolebot_characters b ON b.characterID = gm.characterID AND b.enabled = 1
		 WHERE gm.gangID = ?`, g.GangID).Scan(&n)
	where := m.gangOpWhere(ctx, g)
	what := strings.ToLower(activityLabel(g.Activity))
	switch g.State.OpPhase {
	case gangOpMuster:
		return fmt.Sprintf("We're mustering at %s for a %s, %d pilots so far.",
			m.stationLabel(ctx, g.State.StagingStationID), what, n)
	case gangOpWindDown:
		return fmt.Sprintf("We're winding down and heading back to %s.", m.stationLabel(ctx, g.State.StagingStationID))
	}
	if where == "" {
		return fmt.Sprintf("%d pilots on a %s, picking a site.", n, what)
	}
	return fmt.Sprintf("%d pilots on a %s in %s.", n, what, where)
}
