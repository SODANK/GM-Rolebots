package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// The gank op: the sim side.
//
// A gank fleet (gangform.go, fcSeat.gank; doctrine kindGank) is one unarmed scout
// and a line of disposable destroyers. The scout sits the policed camp system's
// gate and the strikers stage in the lawless far system one jump down the lane.
// The phases are the TaskGangGank* tasks: stage, scout, strike, loot. The call
// arithmetic is gank.go and the lane is lane.go's gank band. Nothing here shoots.

const (
	// gankCallWindowSec is how fresh a call must be for a staged striker to answer
	// it; an older mark has aligned out.
	gankCallWindowSec = 90
	// gankCallExpireSec is how long the scout keeps a call the strikers did
	// not finish before dropping it and scanning again.
	gankCallExpireSec = 240
	// gankStrikeMaxSec is how long a striker that jumped in stays before
	// going back to stage with the mark gone.
	gankStrikeMaxSec = 180
	// gankStintSec is how long a fleet works one lane: a camp's stint.
	gankStintSec = piracyCampSec
	// gankSearchHops is how far from the commander the lane picker looks.
	gankSearchHops = piracySearchHops
	// gankLootRadiusM is how far from the scout the wrecks may lie.
	gankLootRadiusM = 25_000.0
	// gankLootMaxSec is how long the scout spends stripping before it goes.
	gankLootMaxSec = 240
	// gankScanRecheckSec is how long a scanned ball stays scanned.
	gankScanRecheckSec = 60
)

// gankCallFor reports whether a mark with cargoISK and victimEHP hitpoints, in a
// system of this security, is worth a strike by the staged strikers (strikerDPS),
// and how many ships it takes. The mean DPS is the per-ship figure because a line
// goes whole. capSec > 0 caps the window: an undock mark docks back (gank_undock.go).
func gankCallFor(cargoISK, minISK, victimEHP float64, strikerDPS []float64, sec, lockSec, marginPct, capSec float64) (ships int, ok bool) {
	if len(strikerDPS) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, d := range strikerDPS {
		sum += d
	}
	window, policed := gankWindow(sec, lockSec)
	if !policed {
		return 0, false
	}
	if capSec > 0 {
		window = math.Min(window, math.Max(capSec-math.Max(lockSec, 0), 0))
	}
	ships = gankShipsNeeded(victimEHP, sum/float64(len(strikerDPS)), window, marginPct)
	return ships, gankValid(cargoISK, minISK, ships, len(strikerDPS))
}

// ── the plan ────────────────────────────────────────────────────────────────

// gangPlanGank routes one member of a gank fleet: the scout to the camp gate,
// everyone else to the far system's stage.
func (m *Mod) gangPlanGank(ctx context.Context, b *botRun, g *Gang, mem *GangMember) error {
	st := b.State
	st.Activity = ActPiracy
	if until := m.piracyHeatUntil(b); until > clock.Unix() {
		return m.piracyGoToGround(ctx, b, until)
	}
	ready, err := m.gangSeatReady(ctx, b, seatActivityFor(g.Activity, mem.Role))
	if err != nil || !ready {
		return err
	}
	gs, err := m.gankSite(ctx, b, g)
	if err != nil {
		return err
	}
	if gs.PirSystemID == 0 || gs.GankFarSystemID == 0 {
		return m.gangWait(ctx, b, "no lane with traffic in reach for a gank")
	}
	st.Data = gangDutyData(st.Data, g)
	st.Data.PiracySystemID = gs.PirSystemID
	st.Data.PiracyGateID = gs.PirGateID
	st.Data.PiracyPickedAt = clock.Unix()
	if mem.Role == RoleScout {
		if gs.GankStationID != 0 {
			m.logBot(b.CharID, logInfo, "gank", "scouting the %s undock for gang %q",
				gs.GankStationName, g.Name)
		} else {
			m.logBot(b.CharID, logInfo, "gank", "scouting the %s gate in %s for gang %q",
				gs.GankFarSystemName, gs.PirSystemName, g.Name)
		}
		return m.beginTravel(ctx, b, gs.PirSystemID, 0, string(TaskGangGankScout))
	}
	if gs.GankStationID != 0 {
		m.logBot(b.CharID, logInfo, "gank", "lurking off the %s undock for gang %q",
			gs.GankStationName, g.Name)
		return m.beginTravel(ctx, b, gs.PirSystemID, 0, string(TaskGangGankStage))
	}
	m.logBot(b.CharID, logInfo, "gank", "staging in %s for gang %q's gank on %s",
		gs.GankFarSystemName, g.Name, gs.PirSystemName)
	return m.beginTravel(ctx, b, gs.GankFarSystemID, 0, string(TaskGangGankStage))
}

// gankSite is the fleet's lane, picked once per stint by the first member to
// plan (lane.go, the gank band): the scout's gate in a policed system, the
// strikers' stage at the lawless far end. Posted on the blackboard.
func (m *Mod) gankSite(ctx context.Context, b *botRun, g *Gang) (*GangState, error) {
	gs := &g.State
	now := clock.Unix()
	if gs.PirSystemID != 0 && gs.GankFarSystemID != 0 && now-gs.PirPickedAt < gankStintSec {
		return gs, nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	r := m.economicRules(ctx, b, defaultBand(b))
	ceil := dialGankSecCeil.Get()
	admit := func(sysID int64) bool {
		ms, ok := u.meta[sysID]
		if !ok || !u.allowed(sysID, r) {
			return false
		}
		return dispSec(ms.Security) <= ceil
	}
	recent := map[int64]bool{}
	for _, id := range gs.HuntRecent {
		recent[id] = true
	}
	camp, gate, far, laneOK := m.pickCampLane(ctx, b, laneGank, gankSearchHops, nil, admit, recent)
	dock, undockOK := m.pickCampStation(ctx, b, gankSearchHops, admit, recent)
	undock := pickGankSiteKind(laneOK, undockOK, rand.Float64(), dialGankUndockShare.Get())
	// A GM's /rolebots gank <corp> undock|gate picks the kind once, when on offer.
	if v, forced := gankSiteForce.LoadAndDelete(g.LeaderCharID); forced {
		switch v.(string) {
		case "undock":
			undock = undockOK
		case "gate":
			undock = !laneOK && undockOK
		}
	}
	if !laneOK && !undockOK {
		gs.PirSystemID, gs.PirGateID, gs.GankFarSystemID, gs.GankStationID = 0, 0, 0, 0
		return gs, nil
	}
	if gs.PirSystemID != 0 {
		gs.HuntRecent = append([]int64{gs.PirSystemID}, gs.HuntRecent...)
		if len(gs.HuntRecent) > gangHuntRecentMax {
			gs.HuntRecent = gs.HuntRecent[:gangHuntRecentMax]
		}
	}
	gs.GankCall = nil
	gs.PirPickedAt = now
	gs.GankStageSafeID, gs.GankEscapeSafeID = 0, 0
	if undock {
		// The undock: one system, the scout on the door, the strikers on a
		// safe (gank_undock.go).
		gs.PirSystemID, gs.PirSystemName = dock.SystemID, m.nameOfSystem(ctx, dock.SystemID)
		gs.PirGateID = 0
		gs.GankFarSystemID, gs.GankFarSystemName = dock.SystemID, gs.PirSystemName
		gs.GankStationID, gs.GankStationName = dock.StationID, m.stationLabel(ctx, dock.StationID)
	} else {
		gs.PirSystemID, gs.PirSystemName = camp, m.nameOfSystem(ctx, camp)
		gs.PirGateID = gate
		gs.GankFarSystemID, gs.GankFarSystemName = far, m.nameOfSystem(ctx, far)
		gs.GankStationID, gs.GankStationName = 0, ""
	}
	if err := saveGangState(ctx, m.db, g.GangID, gs); err != nil {
		return nil, err
	}
	if undock {
		m.logBot(b.CharID, logInfo, "gank",
			"gang %q sets up a gank on the %s undock — %d pilots docked, %.0f jumps a day through %s",
			g.Name, gs.GankStationName, dock.DockedReal+dock.DockedBots, dock.MapJumps, gs.PirSystemName)
		return gs, nil
	}
	m.logBot(b.CharID, logInfo, "gank",
		"gang %q sets up a gank on the %s–%s lane — scout on %s's %s gate, strikers staged in %s",
		g.Name, gs.PirSystemName, gs.GankFarSystemName, gs.PirSystemName, gs.GankFarSystemName, gs.GankFarSystemName)
	return gs, nil
}

// ── the steps ───────────────────────────────────────────────────────────────

// gangGankStep advances one member through the op's phases.
func (m *Mod) gangGankStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	gs := &g.State
	now := clock.Unix()
	if gs.PirSystemID == 0 || gs.GankFarSystemID == 0 {
		st.Activity, st.Phase = "", string(TaskIdle)
		return saveState(ctx, m.db, st)
	}
	// Stint over with nothing in flight, or the ground moved under this pilot:
	// re-plan. The first planner rotates the lane for everyone.
	if (now-gs.PirPickedAt >= gankStintSec && gs.GankCall == nil) || st.Data.PiracySystemID != gs.PirSystemID {
		st.Activity, st.Phase = "", string(TaskIdle)
		return saveState(ctx, m.db, st)
	}
	switch TaskKind(st.Phase) {
	case TaskGangGankStage:
		return m.gankStageStep(ctx, b, g)
	case TaskGangGankScout:
		return m.gankScoutStep(ctx, b, g)
	case TaskGangGankStrike:
		return m.gankStrikeStep(ctx, b, g)
	case TaskGangGankLoot:
		return m.gankLootStep(ctx, b, g)
	}
	return fmt.Errorf("unknown gank phase %q", st.Phase)
}

// gankStageStep holds a striker on the far gate facing the camp and jumps it
// in on a fresh call.
func (m *Mod) gankStageStep(ctx context.Context, b *botRun, g *Gang) error {
	st, gs := b.State, &g.State
	if until := m.piracyHeatUntil(b); until > clock.Unix() {
		return m.piracyGoToGround(ctx, b, until)
	}
	if b.SystemID != gs.GankFarSystemID {
		return m.beginTravel(ctx, b, gs.GankFarSystemID, 0, string(TaskGangGankStage))
	}
	if b.ShipID == 0 {
		return fmt.Errorf("gank striker has no ship boarded")
	}
	if gs.GankStationID != 0 {
		return m.gankLurkStep(ctx, b, g)
	}
	if !st.Data.PiracyCampSet {
		if gate := m.gateBetween(ctx, gs.GankFarSystemID, gs.PirSystemID); gate != 0 {
			m.parkAtGateArrival(ctx, b.ShipID, gate)
		}
		st.Data.PiracyCampSet = true
	}
	if call := gs.GankCall; call != nil && clock.Unix()-call.CalledAt <= gankCallWindowSec {
		st.Data.PiracyCampSet = false
		m.logBot(b.CharID, logInfo, "gank", "jumping in on the call — %s, %s in the hold, %d of us asked for",
			call.VictimName, fmtISK(call.ISK), call.Ships)
		return m.beginTravel(ctx, b, gs.PirSystemID, 0, string(TaskGangGankStrike))
	}
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(20)
	return saveState(ctx, m.db, st)
}

// gankScoutStep keeps the scout on the camp gate and moves it to the loot
// once the called mark is dead. The scanning itself is the grid's
// (gank_embodied.go) or the abstract resolver's (gank_abstract.go).
func (m *Mod) gankScoutStep(ctx context.Context, b *botRun, g *Gang) error {
	st, gs := b.State, &g.State
	if b.SystemID != gs.PirSystemID {
		return m.beginTravel(ctx, b, gs.PirSystemID, 0, string(TaskGangGankScout))
	}
	if b.ShipID == 0 {
		return fmt.Errorf("gank scout has no ship boarded")
	}
	if !st.Data.PiracyCampSet {
		if gs.GankStationID != 0 {
			m.gankScoutPark(ctx, b, gs.GankStationID)
		} else {
			m.parkAtGateArrival(ctx, b.ShipID, gs.PirGateID)
		}
		m.pirateSayRun(b, evCamp)
		st.Data.PiracyCampSet = true
	}
	if call := gs.GankCall; call != nil {
		switch {
		case !m.entityRowExists(ctx, call.ShipID):
			// The mark is dead: the wrecks are the scout's now.
			st.Phase = string(TaskGangGankLoot)
			st.Data.PiracyPickedAt = clock.Unix()
			gs.GankCall = nil
			if err := saveGangState(ctx, m.db, g.GangID, gs); err != nil {
				return err
			}
			m.logBot(b.CharID, logInfo, "gank", "%s is down — going for the wrecks", call.VictimName)
			return saveState(ctx, m.db, st)
		case clock.Unix()-call.CalledAt > gankCallExpireSec:
			gs.GankCall = nil
			if err := saveGangState(ctx, m.db, g.GangID, gs); err != nil {
				return err
			}
			m.logBot(b.CharID, logInfo, "gank", "%s got away — back to scanning", call.VictimName)
		}
	}
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(15)
	return saveState(ctx, m.db, st)
}

// gankStrikeStep is a striker in the camp system. The shooting is the grid's
// (gank_embodied.go gankStrikeTarget); here the striker stays until the call is
// spent, then goes back to stage, or re-plans for a hull if podded.
func (m *Mod) gankStrikeStep(ctx context.Context, b *botRun, g *Gang) error {
	st, gs := b.State, &g.State
	if b.ShipID == 0 || m.isCapsule(ctx, b.ShipID) {
		st.Activity, st.Phase = "", string(TaskIdle)
		return saveState(ctx, m.db, st)
	}
	if b.SystemID != gs.PirSystemID {
		return m.beginTravel(ctx, b, gs.PirSystemID, 0, string(TaskGangGankStrike))
	}
	call := gs.GankCall
	if call == nil || clock.Unix()-call.CalledAt > gankStrikeMaxSec {
		st.Data.PiracyCampSet = false
		if until := m.piracyHeatUntil(b); until > clock.Unix() {
			return m.piracyGoToGround(ctx, b, until)
		}
		if gs.GankStationID != 0 {
			// The stage is in this system: back to the safe.
			m.logBot(b.CharID, logInfo, "gank", "the strike is over — back to the safe off %s", gs.GankStationName)
			st.Phase = string(TaskGangGankStage)
			return saveState(ctx, m.db, st)
		}
		m.logBot(b.CharID, logInfo, "gank", "the strike is over — back to the stage in %s", gs.GankFarSystemName)
		return m.beginTravel(ctx, b, gs.GankFarSystemID, 0, string(TaskGangGankStage))
	}
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(10)
	return saveState(ctx, m.db, st)
}

// gankLootStep is the scout stripping the wrecks within reach and carrying
// the take to the corporation's Armory office. One container per pass.
func (m *Mod) gankLootStep(ctx context.Context, b *botRun, g *Gang) error {
	st, gs := b.State, &g.State
	if b.SystemID != gs.PirSystemID {
		return m.beginTravel(ctx, b, gs.PirSystemID, 0, string(TaskGangGankLoot))
	}
	if b.ShipID == 0 {
		return fmt.Errorf("gank scout has no ship to load the take into")
	}
	done := func(why string) error {
		m.logBot(b.CharID, logInfo, "gank", "%s — leaving the wrecks", why)
		return m.gankDeliver(ctx, b, g)
	}
	if clock.Unix()-st.Data.PiracyPickedAt > gankLootMaxSec {
		return done("out of time")
	}
	free, err := m.freeHoldM3(ctx, b)
	if err != nil {
		return err
	}
	if free <= 0 {
		return done("hold is full")
	}
	pos, ok := m.shipPosition(ctx, b.ShipID)
	if !ok {
		return done("cannot place the ship")
	}
	ids := m.containersNear(ctx, b.SystemID, pos, gankLootRadiusM)
	if len(ids) == 0 {
		return done("nothing left to take")
	}
	for _, id := range ids {
		if !m.isEmbodied(b.CharID) {
			m.parkAtCelestialEntity(ctx, b.ShipID, id)
		}
		taken, perType, err := m.takeCanContents(ctx, b.CharID, b.ShipID, id, b.SystemID, free)
		if err != nil {
			return err
		}
		if taken <= 0 {
			continue
		}
		value := m.gankBookLoot(ctx, b, g, perType)
		m.logBot(b.CharID, logInfo, "gank", "stripped %.1f m³ worth %s out of a wreck", taken, fmtISK(value))
		if eb := m.liveProjection(b.CharID); eb != nil {
			m.pirateSay(eb, evGankLoot, 0, "", value)
		}
		st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(10)
		return saveState(ctx, m.db, st)
	}
	return done("the wrecks are empty")
}

// gankBookLoot records a take in the scout's loot ledger (so the donation
// step hands exactly these units over) and on the corporation's books.
func (m *Mod) gankBookLoot(ctx context.Context, b *botRun, g *Gang, perType map[int64]int64) float64 {
	ids := make([]int64, 0, len(perType))
	for t := range perType {
		ids = append(ids, t)
	}
	prices, _ := evedb.TypeValues(ctx, m.db, ids)
	value := 0.0
	for t, n := range perType {
		m.noteLootTaken(ctx, b.CharID, t, m.typeNameOr(ctx, t), n)
		if p, ok := prices[t]; ok {
			value += p.Value * float64(n)
		}
	}
	if value > 0 {
		g.State.GankLootISK += value
		_ = saveGangState(ctx, m.db, g.GangID, &g.State)
		m.corpBook(ctx, b.CorpID, lineGankLoot, value, b.CharID,
			fmt.Sprintf("gank loot from %s", g.State.PirSystemName))
	}
	return value
}

// gankDeliver sends the scout to the corporation's Armory office with the take,
// through the hunter's donation step (lootdonate.go). With no Armory it keeps the
// take and goes back to scanning; the take sells with the rest when the op ends.
func (m *Mod) gankDeliver(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	station, office := m.kitStation(ctx, b.CorpID, kitArmory)
	if office == 0 || station == 0 {
		st.Phase = string(TaskGangGankScout)
		st.Data.PiracyCampSet = false
		return saveState(ctx, m.db, st)
	}
	if rows, err := m.lootLedger(ctx, b.CharID); err != nil || len(rows) == 0 {
		st.Phase = string(TaskGangGankScout)
		st.Data.PiracyCampSet = false
		return saveState(ctx, m.db, st)
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, station)
	if err != nil {
		return fmt.Errorf("armory station %d: %w", station, err)
	}
	st.Data.LootDonateOffice, st.Data.LootDonateStation = office, station
	m.logBot(b.CharID, logInfo, "gank", "taking the wrecks' take to the corporation's %s at %s",
		kitLabel(kitArmory), m.stationLabel(ctx, station))
	return m.beginTravel(ctx, b, loc.SolarSystemID, station, string(TaskCombatDonate))
}

// ── helpers ─────────────────────────────────────────────────────────────────

// gankStagedStrikers is every member of the fleet standing staged in the far
// system with a measurable ship: the line a call can send.
func (m *Mod) gankStagedStrikers(ctx context.Context, g *Gang) []*pvpCombatant {
	if g == nil || g.State.GankFarSystemID == 0 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID
		  FROM rolebot_state s
		  JOIN rolebot_gang_members gm ON gm.characterID = s.characterID
		 WHERE gm.gangID = ? AND s.phase = ?`, g.GangID, string(TaskGangGankStage))
	if err != nil {
		return nil
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	var out []*pvpCombatant
	for _, id := range ids {
		if pc := m.pvpCombatantFor(ctx, id, RoleCombat); pc != nil && pc.SystemID == g.State.GankFarSystemID && pc.DPS > 0 {
			out = append(out, pc)
		}
	}
	return out
}

// callGank posts the strike on the blackboard, and logs the call so the kill
// that follows is read as a gank by the relations ledger (gankrel.go).
func (m *Mod) callGank(ctx context.Context, g *Gang, call GankCall) error {
	g.State.GankCall = &call
	m.noteGankCalled(ctx, g.GangID, call.VictimCharID, g.State.PirSystemID, call.ISK)
	return saveGangState(ctx, m.db, g.GangID, &g.State)
}

// containersNear is every cargo container within radius of p, nearest first:
// the wrecks of a strike, the mark's and the strikers' alike.
func (m *Mod) containersNear(ctx context.Context, systemID int64, p destiny.Vec3, radiusM float64) []int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID FROM entity
		 WHERE locationID = ? AND typeID = ?
		   AND POWER(x - ?, 2) + POWER(y - ?, 2) + POWER(z - ?, 2) < POWER(?, 2)
		 ORDER BY POWER(x - ?, 2) + POWER(y - ?, 2) + POWER(z - ?, 2)
		 LIMIT 12`,
		systemID, evedb.TypeCargoContainer,
		p.X, p.Y, p.Z, radiusM,
		p.X, p.Y, p.Z)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// entityRowExists reports whether an entity row still exists: a ship that
// died is gone.
func (m *Mod) entityRowExists(ctx context.Context, itemID int64) bool {
	if itemID == 0 {
		return false
	}
	var one int64
	err := m.db.QueryRowContext(ctx, `SELECT 1 FROM entity WHERE itemID = ?`, itemID).Scan(&one)
	if err == nil {
		return true
	}
	// A read that failed for any reason but "no row" is not evidence of a
	// death: the scout must not go for wrecks that are not there yet.
	return !errors.Is(err, sql.ErrNoRows)
}

// liveProjection is the pilot's projection if it has one on a grid.
func (m *Mod) liveProjection(charID int64) *embodiedBot {
	embodyMu.Lock()
	defer embodyMu.Unlock()
	return embodyLive[charID]
}

// gankOpNote is the Op Board's line for a gank fleet.
func (m *Mod) gankOpNote(ctx context.Context, g *Gang) string {
	gs := &g.State
	if gs.PirSystemID == 0 {
		return "gank fleet — looking for a lane with traffic"
	}
	staged := len(m.gankStagedStrikers(ctx, g))
	note := fmt.Sprintf("gank fleet — scout on the %s gate in %s, %d striker(s) staged in %s",
		gs.GankFarSystemName, gs.PirSystemName, staged, gs.GankFarSystemName)
	if gs.GankCall != nil {
		note += fmt.Sprintf("; strike called on %s (%s)", gs.GankCall.VictimName, fmtISK(gs.GankCall.ISK))
	}
	if gs.GankLootISK > 0 {
		note += fmt.Sprintf("; %s looted this op", fmtISK(gs.GankLootISK))
	}
	return note
}
