package rolebots

// Undock ganks, the strikers' aftermath, and the prey's re-dock reflex.
//
// A gank fleet may camp a busy station's undock instead of a gate (gankSite,
// rolebots.gank.undockShare), picked by public traffic (scoreUndock). The scout
// parks on the undock; strikers lurk at a stage safe (safespot.go) and warp onto
// the call. The mark can dock back, so the call window is the CONCORD window
// capped by rolebots.gank.redockSec.
//
// Once the mark is down or CONCORD has a striker locked, each striker decides
// once (gankAftermath) to run or brawl. The abstract tier rolls the same verdict.

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// ── dials ───────────────────────────────────────────────────────────────────

var (
	// dialGankUndockShare is the share of gank stints worked on a station
	// undock rather than a gate, when both are on offer.
	dialGankUndockShare = &pvpDial{key: "rolebots.gank.undockShare", def: 0.5, lo: 0, hi: 1, step: 0.05,
		label: "Share of gank stints camped on a station undock instead of a gate"}
	// dialGankRedockSec is how long a mark on the undock needs to dock back:
	// the undock call's window is never longer.
	dialGankRedockSec = &pvpDial{key: "rolebots.gank.redockSec", def: 12, lo: 0, hi: 60, step: 1,
		label: "Seconds a mark on the undock needs to dock back (caps the undock window)"}
	// dialGankPreyRedockPct is the chance an unwatched undock mark docks back
	// before the line lands.
	dialGankPreyRedockPct = &pvpDial{key: "rolebots.gank.preyRedockPct", def: 40, lo: 0, hi: 100, step: 5,
		label: "Chance (%) an unwatched undock mark docks back before the strike lands"}
)

func gankUndockDials() []*pvpDial {
	return []*pvpDial{dialGankUndockShare, dialGankRedockSec, dialGankPreyRedockPct}
}

// ── picking the undock ──────────────────────────────────────────────────────

// undockStats is one station's public traffic.
type undockStats struct {
	StationID, SystemID    int64
	DockedReal, DockedBots int
	BotDocks               float64 // rolebot_traffic_stations.docks
	MapJumps               float64 // the system's jumps, last laneMapHours
	PreyKills              float64 // haulers and barges killed on this undock, 7 d
	Campers                int     // other camps in the system
}

type undockWeights struct{ Real, Bots, Docks, Jumps, Prey, Crowd float64 }

var defaultUndockWeights = undockWeights{Real: 3, Bots: 1, Docks: 2, Jumps: 1, Prey: 2.5, Crowd: 2}

// scoreUndock is how good a station is to camp. Pure; logs so a hub does not
// drown every other term.
func scoreUndock(s undockStats, w undockWeights) float64 {
	return w.Real*math.Log1p(float64(s.DockedReal)) +
		w.Bots*math.Log1p(float64(s.DockedBots)) +
		w.Docks*math.Log1p(s.BotDocks) +
		w.Jumps*math.Log1p(s.MapJumps) +
		w.Prey*math.Log1p(s.PreyKills) -
		w.Crowd*float64(s.Campers)
}

// pickUndock is the best-scoring station outside recent systems, or false
// when nothing scores above zero. Pure.
func pickUndock(cands []undockStats, w undockWeights, recent map[int64]bool) (undockStats, bool) {
	var best undockStats
	bestScore, found := 0.0, false
	for _, c := range cands {
		if recent[c.SystemID] {
			continue
		}
		if sc := scoreUndock(c, w); sc > bestScore {
			best, bestScore, found = c, sc, true
		}
	}
	return best, found
}

// pickGankSiteKind reports whether a stint is camped on an undock. Pure.
func pickGankSiteKind(laneOK, undockOK bool, roll, share float64) (undock bool) {
	switch {
	case undockOK && !laneOK:
		return true
	case laneOK && !undockOK:
		return false
	case !laneOK && !undockOK:
		return false
	}
	return roll < share
}

// gankPreyGroups are the hull groups an undock gank is for (killmails'
// shipGroupID): industrial, transport, freighter, barge, exhumer.
var gankPreyGroups = []int64{28, 380, 513, 463, 543}

// pickCampStation finds the undock to camp within hops of the pilot.
func (m *Mod) pickCampStation(ctx context.Context, b *botRun, hops int,
	admit func(sysID int64) bool, recent map[int64]bool) (undockStats, bool) {

	u, err := m.universe(ctx)
	if err != nil {
		return undockStats{}, false
	}
	var systems []int64
	for sys := range systemsWithinHops(u, b.SystemID, hops) {
		if admit == nil || admit(sys) {
			systems = append(systems, sys)
		}
	}
	if len(systems) == 0 {
		return undockStats{}, false
	}
	cands := m.loadUndockStats(ctx, systems)
	return pickUndock(cands, defaultUndockWeights, recent)
}

// systemsWithinHops is every system within hops gates of from, from included.
func systemsWithinHops(u *universe, from int64, hops int) map[int64]bool {
	near := map[int64]bool{from: true}
	frontier := []int64{from}
	for h := 0; h < hops && len(frontier) > 0; h++ {
		var next []int64
		for _, s := range frontier {
			for _, nb := range u.adj[s] {
				if !near[nb] {
					near[nb] = true
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	return near
}

// loadUndockStats reads the public traffic of every NPC station in systems.
func (m *Mod) loadUndockStats(ctx context.Context, systems []int64) []undockStats {
	sort.Slice(systems, func(i, j int) bool { return systems[i] < systems[j] })
	args := make([]any, len(systems))
	for i, s := range systems {
		args[i] = s
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, solarSystemID FROM staStations
		 WHERE solarSystemID IN (`+placeholders(len(systems))+`) AND stationID < ?`,
		append(args, int64(gankOutpostFloor))...)
	if err != nil {
		return nil
	}
	var out []undockStats
	byStation := map[int64]int{}
	for rows.Next() {
		var s undockStats
		if rows.Scan(&s.StationID, &s.SystemID) == nil {
			byStation[s.StationID] = len(out)
			out = append(out, s)
		}
	}
	rows.Close()
	if len(out) == 0 {
		return nil
	}
	stArgs := make([]any, 0, len(out))
	for _, s := range out {
		stArgs = append(stArgs, s.StationID)
	}

	readPairs := func(q string, a []any, set func(i int, v float64)) {
		r, err := m.db.QueryContext(ctx, q, a...)
		if err != nil {
			return
		}
		defer r.Close()
		for r.Next() {
			var id int64
			var v float64
			if r.Scan(&id, &v) == nil {
				if i, ok := byStation[id]; ok {
					set(i, v)
				}
			}
		}
	}
	readPairs(`SELECT stationID, docks FROM rolebot_traffic_stations
		WHERE stationID IN (`+placeholders(len(stArgs))+`)`, stArgs,
		func(i int, v float64) { out[i].BotDocks = v })
	killArgs := append([]any{clock.Unix() - 7*86400}, stArgs...)
	groupArgs := make([]any, len(gankPreyGroups))
	for i, g := range gankPreyGroups {
		groupArgs[i] = g
	}
	readPairs(`SELECT locationID, COUNT(*) FROM killmails
		WHERE killTime > ? AND locationGroupID = 15
		  AND locationID IN (`+placeholders(len(stArgs))+`)
		  AND shipGroupID IN (`+placeholders(len(groupArgs))+`)
		GROUP BY locationID`, append(killArgs, groupArgs...),
		func(i int, v float64) { out[i].PreyKills = v })

	jumps := map[int64]float64{}
	if r, err := m.db.QueryContext(ctx, `
		SELECT solarSystemID, SUM(count) FROM mapHistory
		 WHERE hourBucket > ? AND statType = 4
		   AND solarSystemID IN (`+placeholders(len(systems))+`)
		 GROUP BY solarSystemID`, append([]any{clock.Now().Unix()/3600 - laneMapHours}, args...)...); err == nil {
		for r.Next() {
			var sys int64
			var n float64
			if r.Scan(&sys, &n) == nil {
				jumps[sys] = n
			}
		}
		r.Close()
	}
	campers := m.campersBySystem(ctx)
	for i := range out {
		out[i].MapJumps = jumps[out[i].SystemID]
		out[i].Campers = campers[out[i].SystemID]
		if m.sessions != nil {
			for _, s := range m.sessions.FindByStationID(out[i].StationID) {
				if s.Bot {
					out[i].DockedBots++
				} else {
					out[i].DockedReal++
				}
			}
		}
	}
	return out
}

// gankOutpostFloor is where player-built stations start in staStations: an
// outpost's undock is its owner's business, not a highsec camp.
const gankOutpostFloor = 61000000

// ── the stage and escape safes ──────────────────────────────────────────────

// gankSafeMaking keeps one maker per gang: gangID to unix of the claim.
var gankSafeMaking sync.Map

const gankSafeClaimSec = 90

// gankSafes returns the fleet's stage and escape safes for its undock,
// making whichever is missing (one maker per gang at a time) and sharing it
// with the fleet. ok=false while one is still being made.
func (m *Mod) gankSafes(ctx context.Context, b *botRun, g *Gang) (stage, escape safeSpot, ok bool) {
	gs := &g.State
	sys, station := gs.PirSystemID, gs.GankStationID
	stage, okS := m.pickSafe(ctx, b.CorpID, sys, safeStage, station)
	escape, okE := m.pickSafe(ctx, b.CorpID, sys, safeEscape, station)
	if okS && okE {
		if gs.GankStageSafeID != stage.SafeID || gs.GankEscapeSafeID != escape.SafeID {
			gs.GankStageSafeID, gs.GankEscapeSafeID = stage.SafeID, escape.SafeID
			_ = saveGangState(ctx, m.db, g.GangID, gs)
		}
		return stage, escape, true
	}
	now := clock.Unix()
	if v, held := gankSafeMaking.Load(g.GangID); held && now-v.(int64) < gankSafeClaimSec {
		return stage, escape, false
	}
	gankSafeMaking.Store(g.GangID, now)
	share := func(s safeSpot) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		bots, humans := m.shareSafeWithFleet(ctx, s, g.GangID)
		m.logBot(b.CharID, logInfo, "gank", "bookmarked a %s safe in %s for gang %q — %d bot and %d pilot copies",
			s.Purpose, m.nameOfSystem(ctx, s.SystemID), g.Name, bots, humans)
	}
	if !okS {
		// The lead flies the line into the station for real when it is on a
		// grid; otherwise the point it would have bookmarked is computed.
		made := false
		if eb := liveProjection(b.CharID); eb != nil && eb.systemID == sys {
			if c := celestialByID(eb.celestials, station); c != nil {
				made = m.makeSafeOnWarp(eb, *c, safeStage, station, warpMandatory, share)
			}
		}
		if !made {
			if s, made := m.makeSafeAbstract(ctx, b.CorpID, b.CharID, sys, station, safeStage, station); made {
				share(s)
			}
		}
	}
	if !okE {
		if s, made := m.makeSafeAbstract(ctx, b.CorpID, b.CharID, sys, station, safeEscape, station); made {
			share(s)
		}
	}
	return stage, escape, false
}

// gankLurkAt puts a striker on the stage safe: a warp for a projection, a
// position write otherwise. Reports whether it is there or on its way.
func (m *Mod) gankLurkAt(ctx context.Context, b *botRun, s safeSpot) bool {
	if eb := liveProjection(b.CharID); eb != nil {
		return m.warpToSafe(eb, s, warpDiscretionary)
	}
	m.writePosition(ctx, b.ShipID, s.pos())
	return true
}

// gankCallTargetBand keeps a call's synthetic warp target clear of the safes'.
const gankCallTargetBand = safeTargetBand * 4

// gankCallTarget is the point the scout called, as a warp target.
func gankCallTarget(call *GankCall) evedb.CelestialEntity {
	return evedb.CelestialEntity{
		ItemID: -(gankCallTargetBand + call.ShipID),
		X:      call.X, Y: call.Y, Z: call.Z,
		Name: "the call",
	}
}

// gankStrikeFrom sends a lurking striker onto the call.
func (m *Mod) gankStrikeFrom(ctx context.Context, b *botRun, call *GankCall) {
	if eb := liveProjection(b.CharID); eb != nil {
		m.startEmbodyWarpTo(eb, gankCallTarget(call), warpMandatory)
		return
	}
	m.writePosition(ctx, b.ShipID, gankVictimPos(call))
}

// gankScoutPark puts the scout on the undock: a projection flies there, an
// unwatched ship is parked at the door.
func (m *Mod) gankScoutPark(ctx context.Context, b *botRun, stationID int64) {
	if eb := liveProjection(b.CharID); eb != nil {
		m.flyToCelestial(eb, stationID)
		return
	}
	m.parkAtStationDoor(ctx, b, stationID)
}

// ── the aftermath ───────────────────────────────────────────────────────────

type gankVerdict uint8

const (
	gankUndecided gankVerdict = iota
	gankRun
	gankBrawl
)

// gankAfterIn is what one striker weighs when CONCORD lands.
type gankAfterIn struct {
	Caution, Aggression float64 // -1..1 (traitScale)
	LossShare           float64 // 0..1: how much of a loss this hull is
}

// gankRunChance is the odds a striker runs rather than brawls. Pure: an even
// coin moved by caution, aggression and the hull's worth.
func gankRunChance(in gankAfterIn) float64 {
	p := 0.5 + 0.35*in.Caution - 0.35*in.Aggression + 0.3*in.LossShare
	return math.Max(0.05, math.Min(0.95, p))
}

// gankAftermath is the striker's decision. Pure.
func gankAftermath(in gankAfterIn, roll float64) gankVerdict {
	if roll < gankRunChance(in) {
		return gankRun
	}
	return gankBrawl
}

// gankLossShare reads a hull's worth as how much its loss stings: nothing for
// a cheap kit, everything from gankLossFullISK up. Pure.
func gankLossShare(hullISK float64) float64 {
	return math.Max(0, math.Min(1, hullISK/gankLossFullISK))
}

const gankLossFullISK = 50e6

// gankPoliceKillOdds is the abstract chance a brawling striker takes a CONCORD
// frigate with it. Pure.
func gankPoliceKillOdds(dps float64) float64 {
	return math.Max(0, math.Min(0.5, dps/400))
}

// gankStrikerRuns is the embodied striker's aftermath, asked every combat tick:
// true when it should break off now. The verdict is taken once per call, when the
// mark is gone or CONCORD has it locked, and a runner that is pointed brawls
// until the point drops.
func (m *Mod) gankStrikerRuns(ctx context.Context, eb *embodiedBot) bool {
	if eb.ent == nil || eb.ss == nil {
		return false
	}
	gk := &eb.gank
	if gk.verdict == gankUndecided {
		if m.gankStrikeTarget(eb) != 0 && !m.concordOnMe(eb) {
			return false
		}
		t := m.chatterTraitsOf(ctx, eb.charID)
		hull := evedb.TypeValueOf(ctx, m.db, eb.ent.Slim.TypeID).Value
		gk.verdict = gankAftermath(gankAfterIn{
			Caution:    traitScale(t[TraitCaution]),
			Aggression: traitScale(t[TraitAggression]),
			LossShare:  gankLossShare(hull),
		}, rand.Float64())
		gk.escape = m.gankEscapeTarget(ctx, eb)
		if gk.verdict == gankRun {
			m.logBot(eb.charID, logInfo, "gank", "CONCORD is here — running for it")
			m.pirateSay(eb, evGankRun, 0, "", 0)
		} else {
			m.logBot(eb.charID, logInfo, "gank", "CONCORD is here — turning the guns on them")
			m.pirateSay(eb, evGankBrawl, 0, "", 0)
		}
	}
	if gk.verdict != gankRun {
		return false
	}
	eb.mu.Lock()
	warping, dest := eb.warping, eb.fleeDest
	eb.mu.Unlock()
	if !gk.saidEscaped && gk.escape != nil && dest == gk.escape.ItemID && !warping &&
		eb.ent.Pos().Sub(destiny.Vec3{X: gk.escape.X, Y: gk.escape.Y, Z: gk.escape.Z}).Len() < navWarpMinRange {
		gk.saidEscaped = true
		m.pirateSay(eb, evGankEscaped, 0, "", 0)
	}
	return eb.ent.WarpScrambleTotal() <= 0
}

// concordOnMe reports whether a CONCORD ship has this projection locked.
func (m *Mod) concordOnMe(eb *embodiedBot) bool {
	if m.npc == nil {
		return false
	}
	for _, id := range m.npc.GetTargetersOf(eb.ent.ID) {
		if e := eb.ss.GetEntity(id); e != nil && lawEnforcementGroup(e.Slim.GroupID) {
			return true
		}
	}
	return false
}

// gankEscapeTarget is where a running striker goes: the fleet's escape safe,
// else the farthest celestial that is not a station.
func (m *Mod) gankEscapeTarget(ctx context.Context, eb *embodiedBot) *evedb.CelestialEntity {
	if g := m.gankGang(ctx, eb); g != nil {
		if s, ok := m.pickSafe(ctx, eb.corpID, eb.systemID, safeEscape, g.State.GankStationID); ok {
			t := safeTarget(s)
			m.goSafeBot(eb.charID, "safe used", func() { m.markSafeUsed(s.SafeID) })
			return &t
		}
	}
	pos := eb.ent.Pos()
	var best *evedb.CelestialEntity
	bestD := 0.0
	for i := range eb.celestials {
		c := &eb.celestials[i]
		if c.GroupID == groupStation || c.GroupID == 6 {
			continue
		}
		if d := (destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}).Sub(pos).Len(); d > bestD {
			best, bestD = c, d
		}
	}
	return best
}

// ── the re-dock reflex ──────────────────────────────────────────────────────

// redockRangeM is how far past a station's hull a ship may be and still dock
// back rather than warp.
const redockRangeM = 10_000.0

// redockRequests maps charID to stationID, posted by the combat loop and taken
// by the sim's next pass (redockCallUp).
var redockRequests sync.Map

// gankSiteForce maps leaderCharID to "undock" or "gate", set by /rolebots gank
// and taken by the fleet's first site pick.
var gankSiteForce sync.Map

func init() {
	registerCharForget("redockRequests", func(id int64) { redockRequests.Delete(id) })
	registerCharForget("gankSiteForce", func(id int64) { gankSiteForce.Delete(id) })
}

// redockStation is the station this projection may dock back into, or nil.
func redockStation(eb *embodiedBot) *evedb.CelestialEntity {
	if eb.ent == nil {
		return nil
	}
	pos := eb.ent.Pos()
	for i := range eb.celestials {
		c := &eb.celestials[i]
		if c.GroupID != groupStation {
			continue
		}
		if (destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}).Sub(pos).Len() <= c.Radius+redockRangeM {
			return c
		}
	}
	return nil
}

// tryRedock is the flight half of the reflex: a projection under fire on an
// undock that may dock does, and reports true. Never for a gank striker.
func (m *Mod) tryRedock(eb *embodiedBot) bool {
	if eb.gankStrike || crimewatchLocked(eb.charID) {
		return false
	}
	st := redockStation(eb)
	if st == nil {
		return false
	}
	if _, already := redockRequests.LoadOrStore(eb.charID, st.ItemID); !already {
		m.logBot(eb.charID, logInfo, "combat", "shot on the undock at %.0f%% hull — docking back into %s",
			eb.hullFraction()*100, celestialLabel(*st))
	}
	eb.mu.Lock()
	eb.fleeing = true
	eb.mu.Unlock()
	m.embodyApproach(eb, uint32(st.ItemID), 0, st.Radius)
	return true
}

// redockCallUp is the sim half: a posted re-dock turns the pilot's step into
// the dock approach at that station. Runs ahead of the leg wait, like the
// other call-ups.
func (m *Mod) redockCallUp(ctx context.Context, b *botRun) bool {
	v, ok := redockRequests.LoadAndDelete(b.CharID)
	if !ok || b.StationID != 0 || b.ShipID == 0 || b.State == nil {
		return false
	}
	stationID := v.(int64)
	loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
	if err != nil || loc.SolarSystemID != b.SystemID {
		return false
	}
	st := b.State
	st.Activity = ""
	st.Phase = string(TaskTravelDock)
	st.Data.Path = nil
	st.Data.TargetSystemID = b.SystemID
	st.Data.TargetStationID = stationID
	st.Data.NextPhase = string(TaskIdle)
	st.Data.LegEndsAt = 0
	if err := saveState(ctx, m.db, st); err != nil {
		rlogf("rolebots: re-dock state for %d: %v", b.CharID, err)
	}
	return true
}

// gankUndockRedocks is the abstract re-dock roll for an undock mark. Pure.
func gankUndockRedocks(roll, pct float64) bool {
	return roll*100 < pct
}

// gankWindowCap is the call window's cap for this fleet's site: the re-dock
// time on an undock, none on a gate.
func gankWindowCap(gs *GangState) float64 {
	if gs == nil || gs.GankStationID == 0 {
		return 0
	}
	return dialGankRedockSec.Get()
}

// gankLurkStep is an undock striker's stage: on the stage safe off the
// station, warping onto a fresh call.
func (m *Mod) gankLurkStep(ctx context.Context, b *botRun, g *Gang) error {
	st, gs := b.State, &g.State
	stage, _, ok := m.gankSafes(ctx, b, g)
	if !ok {
		st.Data.LegEndsAt = clock.Unix() + 10
		return saveState(ctx, m.db, st)
	}
	if !st.Data.PiracyCampSet {
		st.Data.PiracyCampSet = m.gankLurkAt(ctx, b, stage)
	}
	if call := gs.GankCall; call != nil && clock.Unix()-call.CalledAt <= gankCallWindowSec {
		st.Data.PiracyCampSet = false
		m.logBot(b.CharID, logInfo, "gank", "warping in on the call — %s, %s in the hold, %d of us asked for",
			call.VictimName, fmtISK(call.ISK), call.Ships)
		m.gankStrikeFrom(ctx, b, call)
		st.Phase = string(TaskGangGankStrike)
		return saveState(ctx, m.db, st)
	}
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(10)
	return saveState(ctx, m.db, st)
}

// gankAftermathAbstract settles CONCORD's arrival on an abstract strike: each
// striker runs or brawls. A runner gets clear only if the strike (fightSec) and
// its leaving time beat the band's delay, the same stopwatch abstractConcord
// reads; a brawler may take a police frigate with it; every striker that did
// not get away dies.
func (m *Mod) gankAftermathAbstract(ctx context.Context, strikers []*pvpCombatant, sysID int64,
	sec, fightSec float64, call destiny.Vec3) (died, slipped, cops int) {
	for _, s := range strikers {
		t := m.chatterTraitsOf(ctx, s.CharID)
		in := gankAfterIn{
			Caution:    traitScale(t[TraitCaution]),
			Aggression: traitScale(t[TraitAggression]),
			LossShare:  gankLossShare(evedb.TypeValueOf(ctx, m.db, s.ShipTypeID).Value),
		}
		switch gankAftermath(in, rand.Float64()) {
		case gankRun:
			if !concordCatches(sec, fightSec, concordLeaveSec(s)) {
				slipped++
				// Still a criminal: the flag goes with it (criminalcarry.go),
				// so it lies low and CONCORD meets it wherever it surfaces.
				rememberCriminal(s.CharID, clock.Unix())
				m.logBot(s.CharID, logInfo, "gank", "ran from CONCORD in %s and got clear", m.nameOfSystem(ctx, sysID))
				continue
			}
		case gankBrawl:
			if rand.Float64() < gankPoliceKillOdds(s.DPS) {
				cops++
				m.logBot(s.CharID, logInfo, "gank", "turned on CONCORD in %s and took a police frigate down",
					m.nameOfSystem(ctx, sysID))
			}
		}
		m.gankStrikerToCall(ctx, s, sysID, call)
		m.concordKill(ctx, s, sysID, sec)
		died++
	}
	return died, slipped, cops
}

// gankCallJitterM is how far from the call point an abstract striker's wreck may land, on each
// axis. Well inside the scout's gankLootRadiusM.
const gankCallJitterM = 3_000

// jitterAround offsets p by up to r metres on each axis. Pure: u holds three rolls in [0, 1).
func jitterAround(p destiny.Vec3, r float64, u [3]float64) destiny.Vec3 {
	return destiny.Vec3{X: p.X + (2*u[0]-1)*r, Y: p.Y + (2*u[1]-1)*r, Z: p.Z + (2*u[2]-1)*r}
}

// gankStrikerToCall puts a striker that is about to die in the camp system, at the call point
// with a few km of jitter: it jumped or warped in to strike, so its wreck, can and capsule belong
// there, not at its staging coordinates in the far system. The character row moves with the hull,
// so the capsule loseHull makes and the pilot's system agree. A zero call point leaves it be.
func (m *Mod) gankStrikerToCall(ctx context.Context, s *pvpCombatant, sysID int64, call destiny.Vec3) {
	if s == nil || s.ShipID == 0 || call.IsZero() {
		return
	}
	if s.SystemID != sysID {
		constID, regID, err := cachedSolarSystemLocation(ctx, m.db, sysID)
		if err != nil {
			rlogf("rolebots: striker %d to call system %d: %v", s.CharID, sysID, err)
			return
		}
		if err := updateCharacterStation(ctx, m.db, s.CharID, 0, sysID, constID, regID); err != nil {
			rlogf("rolebots: striker %d character to %d: %v", s.CharID, sysID, err)
			return
		}
		if err := evedb.UpdateEntityLocation(ctx, m.db, s.ShipID, sysID); err != nil {
			rlogf("rolebots: striker %d ship to %d: %v", s.CharID, sysID, err)
			return
		}
		s.SystemID = sysID
	}
	m.writePosition(ctx, s.ShipID, jitterAround(call, gankCallJitterM,
		[3]float64{rand.Float64(), rand.Float64(), rand.Float64()}))
}
