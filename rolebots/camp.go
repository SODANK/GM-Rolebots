package rolebots

import (
	"context"
	"fmt"
	"math"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/seclaw"
)

// Gate camps with real seats: the ransom camp's tackle and demands, and the checkpoint.
// Ransom camp (a Predation pack's gate camp, gangplan.go gangPirateStep): the tackler holds
// inside its web and point (tackleBurn) and the demand is priced off a cargo scan when the
// pack can make one (ransomScan). A hold worth far more than the price is demanded outright.
// Checkpoint (a Security or Mercenary fleet, gangFocusCheckpoint): holds a gate under the
// rules of engagement (roe.go), waves spared pilots and, for a Mercenary, tolls neutrals.
// See docs/comment-notes-b02.md#camp.

// ── dials ───────────────────────────────────────────────────────────────────

var (
	dialCheckpointOn = &pvpDial{key: "rolebots.checkpoint.on", def: 0, lo: 0, hi: 1, step: 1,
		label: "Checkpoint fleets (1 = Security/Mercenary patrols may hold a gate, 0 = off)"}
	dialCheckpointShare = &pvpDial{key: "rolebots.checkpoint.share", def: 0.3, lo: 0, hi: 1, step: 0.05,
		label: "Share of security patrols raised as checkpoints"}
	dialCheckpointToll = &pvpDial{key: "rolebots.checkpoint.toll", def: 0, lo: 0, hi: 1, step: 1,
		label: "Mercenary checkpoints toll neutrals outside policed space (1 = on)"}
	dialTollCapISK = &pvpDial{key: "rolebots.checkpoint.tollCapISK", def: 2e6, lo: 0, hi: 1e9, step: 1e5,
		label: "Toll ceiling (ISK)"}
	dialDemandsAlwaysOn = &pvpDial{key: "rolebots.chatter.demandsAlwaysOn", def: 1, lo: 0, hi: 1, step: 1,
		label: "Toll demands are said in Local regardless of the chatter budget (1 = on)"}
)

func campDials() []*pvpDial {
	return []*pvpDial{dialCheckpointOn, dialCheckpointShare, dialCheckpointToll, dialTollCapISK, dialDemandsAlwaysOn}
}

func checkpointOn() bool    { return dialCheckpointOn.Get() >= 0.5 }
func checkpointTollOn() bool { return dialCheckpointToll.Get() >= 0.5 }
func demandsAlwaysOn() bool { return dialDemandsAlwaysOn.Get() >= 0.5 }

// checkpointDoctrine is who may hold a checkpoint: the doctrines whose job is the gate, a
// navy's and a mercenary outfit's. A pirate's gate is a camp.
func checkpointDoctrine(d Doctrine) bool {
	return d == DoctrineSecurity || d == DoctrineMercenary
}

const (
	// ransomCargoDemandRatio: a hold worth this many times the ISK price is
	// asked for outright.
	ransomCargoDemandRatio = 1.5
	// ransomCargoDropRadiusM is how near the mark a jettisoned container must
	// appear to count as compliance.
	ransomCargoDropRadiusM = 10_000.0
	// ransomBotAnswerSec is how long a bot mark takes to decide.
	ransomBotAnswerSec = 5
	// jettisonHullFrac: a bot jettisons when the cargo is worth less than this fraction of its
	// hull, because the ship is the bigger loss.
	jettisonHullFrac = 0.8
	// tollRansomFrac is a toll against the ransom the same mark would face.
	tollRansomFrac = 0.25
	// tollAmnesty is how long a paid toll buys free passage.
	tollAmnesty = time.Hour
	// checkpointStintSec is how long a fleet holds one gate.
	checkpointStintSec = piracyCampSec
	// checkpointWaveRangeM is how close a pilot passes before it is waved.
	checkpointWaveRangeM = 60_000.0
	// checkpointWaveGapSec: one wave per pilot per this.
	checkpointWaveGapSec = 15 * 60
	checkpointTickSec    = 5
)

// ── pure rules ──────────────────────────────────────────────────────────────

// ransomPriceFromScan is the demand priced off a scan: most of the hold and a
// fifth of the hull, never under the pirate's own base, rounded to a clean
// 10k so the line reads like a person said it.
func ransomPriceFromScan(cargoISK, hullISK, mul float64) float64 {
	price := 0.6*math.Max(cargoISK, 0) + 0.2*math.Max(hullISK, 0)
	if floor := ransomBaseISK * mul; price < floor {
		price = floor
	}
	return math.Round(price/10_000) * 10_000
}

// tollPrice is a checkpoint's toll: a quarter of the ransom, capped (cap 0 =
// no cap), rounded to 10k, never under 10k.
func tollPrice(ransom, cap float64) float64 {
	t := ransom * tollRansomFrac
	if cap > 0 && t > cap {
		t = cap
	}
	t = math.Round(t/10_000) * 10_000
	if t < 10_000 {
		t = 10_000
	}
	return t
}

// victimWouldJettison is a bot mark's answer to a cargo demand: the hold goes
// when it is worth less than most of the ship.
func victimWouldJettison(cargoISK, hullISK float64) bool {
	return cargoISK > 0 && hullISK > 0 && cargoISK < hullISK*jettisonHullFrac
}

// checkpointAct is what a checkpoint does with one pilot passing.
type checkpointAct uint8

const (
	actWave   checkpointAct = iota // let through, say so
	actEngage                      // the rules of engagement allow it
	actToll                        // hold for a toll
)

// checkpointVerdict is the whole decision, pure: engage what roeAllows outright (no nerve
// roll), toll a neutral stranger under a Mercenary outside policed space when the dial is
// on, wave everything else.
func checkpointVerdict(st Stance, doc Doctrine, band SecBand, criminal, outlaw, atWar, tollOn bool) checkpointAct {
	if allow, nerve, _ := roeAllows(st, doc, band, criminal, outlaw, atWar); allow && !nerve {
		return actEngage
	}
	if tollOn && doc == DoctrineMercenary && band != BandPoliced && st == StanceNeutral && !criminal && !outlaw {
		return actToll
	}
	return actWave
}

// tackleBurnPlan is the tackler's manoeuvre: approach to inside the web while
// the mark is beyond it, then orbit inside the point. Pure.
func tackleBurnPlan(gap, webRangeM, scramRangeM float64) (approach bool, dist float64) {
	if webRangeM > 0 && gap > webRangeM*0.9 {
		return true, webRangeM * 0.9
	}
	if scramRangeM > 0 {
		return false, scramRangeM * 0.8
	}
	return false, webRangeM * 0.8
}

// ── 5a: the tackler ─────────────────────────────────────────────────────────

// tackleBurn flies a tackler onto its target. False when the hull carries
// neither a web nor a point, in which case the ordinary stance flies it.
func (m *Mod) tackleBurn(eb *embodiedBot, l *combatLoadout, target *destiny.Entity) bool {
	if eb.ent == nil || target == nil {
		return false
	}
	web, scram := 0.0, 0.0
	for _, ew := range l.ewar {
		switch ew.Kind {
		case ewWeb:
			if ew.MaxRange > web {
				web = ew.MaxRange
			}
		case ewScram:
			if ew.MaxRange > scram {
				scram = ew.MaxRange
			}
		}
	}
	if web == 0 && scram == 0 {
		return false
	}
	gap := target.Pos().Sub(eb.ent.Pos()).Len()
	approach, dist := tackleBurnPlan(gap, web, scram)
	// Best speed: the propulsion module stays lit through the burn and the orbit.
	m.runPropCycle(eb, l, true)
	radius := 0.0
	if target.Manager != nil {
		radius = target.Manager.Radius()
	}
	if approach {
		m.embodyApproach(eb, target.ID, dist, radius)
	} else {
		m.embodyOrbit(eb, target.ID, dist, 0)
	}
	return true
}

// ── 5a: the scan and the fleet hold ─────────────────────────────────────────

// ransomScan prices the mark off a cargo scanner: this pilot's own, or a fleetmate's on the
// same grid (the pack's scout). The scanning ship's effect goes on the wire. ok=false when
// nobody in reach carries one.
func (m *Mod) ransomScan(ctx context.Context, eb *embodiedBot, victim *destiny.Entity) (cargoISK, hullISK float64, ok bool) {
	if eb == nil || victim == nil || eb.ss == nil {
		return 0, 0, false
	}
	scanner, sc := eb, m.gankScannerFor(ctx, eb)
	if sc == nil || !sc.found {
		scanner, sc = nil, nil
		if eb.gangID != 0 {
			embodyMu.Lock()
			var mates []*embodiedBot
			for _, o := range embodyLive {
				if o != eb && o.gangID == eb.gangID && o.systemID == eb.systemID && o.ent != nil {
					mates = append(mates, o)
				}
			}
			embodyMu.Unlock()
			for _, o := range mates {
				if s := m.gankScannerFor(ctx, o); s != nil && s.found {
					scanner, sc = o, s
					break
				}
			}
		}
	}
	if sc == nil {
		return 0, 0, false
	}
	m.moduleFX(scanner, sc.itemID, sc.typeID, sc.guid, victim.ID, true, sc.cycleMs, false)
	items, err := evedb.GetShipCargoByShip(ctx, m.db, victim.Slim.ItemID)
	if err != nil {
		return 0, 0, false
	}
	cargo := make([]gankCargoItem, 0, len(items))
	for _, it := range items {
		cargo = append(cargo, gankCargoItem{TypeID: it.TypeID, Qty: it.Quantity})
	}
	prices, _ := evedb.TypeValues(ctx, m.db, gankTypeIDs(cargo))
	cargoISK = gankCargoValue(cargo, prices)
	hullISK = evedb.TypeValueOf(ctx, m.db, victim.Slim.TypeID).Value
	return cargoISK, hullISK, true
}

// holdFleetForTerms puts the fleet on hold while a price stands, when this
// pilot's fleet has a commander on the grid and nobody has held it already.
func (m *Mod) holdFleetForTerms(eb *embodiedBot, rs *ransomState) {
	lead := eb
	if !eb.isFleetCommander() {
		lead = fleetLeaderLive(eb)
	}
	if lead == nil {
		return
	}
	lead.mu.Lock()
	if !lead.call.hold {
		lead.call.hold = true
		rs.heldFleet = true
	}
	lead.mu.Unlock()
}

// releaseFleetHold lifts the hold these terms placed.
func (m *Mod) releaseFleetHold(eb *embodiedBot, rs *ransomState) {
	if rs == nil || !rs.heldFleet {
		return
	}
	rs.heldFleet = false
	lead := eb
	if !eb.isFleetCommander() {
		lead = fleetLeaderLive(eb)
	}
	if lead == nil {
		return
	}
	lead.mu.Lock()
	lead.call.hold = false
	lead.mu.Unlock()
}

// ── 5a: the cargo demand ────────────────────────────────────────────────────

// containerSet is every container within radius of p, as a set.
func (m *Mod) containerSet(ctx context.Context, systemID int64, p destiny.Vec3, radiusM float64) map[int64]bool {
	out := map[int64]bool{}
	for _, id := range m.containersNear(ctx, systemID, p, radiusM) {
		out[id] = true
	}
	return out
}

// cargoDropped polls a cargo demand: a NEW container near the mark is
// compliance, scooped into the pirate's hold on the spot. A bot mark that has
// not answered yet decides here (victimWouldJettison) and jettisons.
func (m *Mod) cargoDropped(eb *embodiedBot, rs *ransomState, victim *destiny.Entity) bool {
	if eb == nil || rs == nil || victim == nil || eb.ss == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pos := victim.Pos()
	for _, id := range m.containersNear(ctx, eb.systemID, pos, ransomCargoDropRadiusM) {
		if rs.cansBefore[id] {
			continue
		}
		free := m.freeHoldOf(ctx, eb)
		if free <= 0 {
			// Nowhere to put it: the demand counts as met and the mark is released.
			return true
		}
		taken, perType, err := m.takeCanContents(ctx, eb.charID, eb.shipID, id, eb.systemID, free)
		if err != nil || taken <= 0 {
			continue
		}
		ids := make([]int64, 0, len(perType))
		for t := range perType {
			ids = append(ids, t)
		}
		prices, _ := evedb.TypeValues(ctx, m.db, ids)
		value := 0.0
		for t, n := range perType {
			m.noteLootTaken(ctx, eb.charID, t, m.typeNameOr(ctx, t), n)
			if p, ok := prices[t]; ok {
				value += p.Value * float64(n)
			}
		}
		if value > 0 {
			rs.demanded = value
		}
		return true
	}
	// A bot mark answers once, a few seconds in.
	if !rs.botAnswered && clock.Now().Sub(rs.demandedAt) >= ransomBotAnswerSec*time.Second && m.isSynthetic(rs.victimCharID) {
		rs.botAnswered = true
		if victimWouldJettison(rs.cargoISK, rs.hullISK) {
			if err := m.jettisonAllCargo(ctx, rs.victimCharID, rs.victimShip, eb.systemID, pos); err != nil {
				rlogf("rolebots: jettison under demand char=%d: %v", rs.victimCharID, err)
			} else {
				m.logBot(rs.victimCharID, logInfo, "piracy",
					"jettisoned the hold under the guns — %s of cargo against a %s hull", iskWords(rs.cargoISK), iskWords(rs.hullISK))
			}
		} else {
			m.logBot(rs.victimCharID, logInfo, "piracy",
				"refused to jettison %s of cargo — the hull is worth less", iskWords(rs.cargoISK))
		}
	}
	return false
}

// freeHoldOf is a projection's free cargo, in m³.
func (m *Mod) freeHoldOf(ctx context.Context, eb *embodiedBot) float64 {
	if eb.ent == nil {
		return 0
	}
	cap, err := evedb.GetShipCargoCapacityFitted(ctx, m.db, eb.ent.Slim.TypeID, eb.shipID)
	if err != nil {
		return 0
	}
	used, _ := m.holdUsedM3(ctx, eb.charID, eb.shipID)
	return cap - used
}

// jettisonAllCargo puts a ship's whole hold into a fresh container beside it: the entity
// rows move and the ball appears on a live grid (spawnCanAt). It is an ordinary container,
// so the pirate's scoop and the core's expiry handle it like any other.
func (m *Mod) jettisonAllCargo(ctx context.Context, charID, shipID, systemID int64, pos destiny.Vec3) error {
	if charID == 0 || shipID == 0 {
		return fmt.Errorf("no ship to jettison from")
	}
	at := pos
	at.X += canJettisonOffsetM
	canID, err := m.spawnCanAt(ctx, 0, charID, systemID, 0, at, 0)
	if err != nil {
		return err
	}
	_, err = m.db.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = 0 WHERE locationID = ? AND flag = 5`, canID, shipID)
	return err
}

// abstractCargoDemand is the unwatched system's cargo demand: the hold read
// off the database, the same ratio and the same bot rule, the rows moved
// straight into the leader's hold (the abstract fiction every other abstract
// movement uses). True when the demand was made and met.
func (m *Mod) abstractCargoDemand(ctx context.Context, lb, vb *botRun, victim *pvpCombatant, pirates []*pvpCombatant, sysID int64) bool {
	if lb == nil || vb == nil || victim == nil || victim.ShipID == 0 {
		return false
	}
	items, err := evedb.GetShipCargoByShip(ctx, m.db, victim.ShipID)
	if err != nil || len(items) == 0 {
		return false
	}
	cargo := make([]gankCargoItem, 0, len(items))
	for _, it := range items {
		cargo = append(cargo, gankCargoItem{TypeID: it.TypeID, Qty: it.Quantity})
	}
	prices, _ := evedb.TypeValues(ctx, m.db, gankTypeIDs(cargo))
	cargoISK := gankCargoValue(cargo, prices)
	hullISK := evedb.TypeValueOf(ctx, m.db, victim.ShipTypeID).Value
	price := ransomPriceFromScan(cargoISK, hullISK, lb.Tune.RansomMul)
	if cargoISK < price*ransomCargoDemandRatio {
		return false
	}
	if !victimWouldJettison(cargoISK, hullISK) {
		m.logBot(vb.CharID, logInfo, "piracy", "refused to hand over %s of cargo — the hull is worth less", iskWords(cargoISK))
		return false
	}
	if lb.ShipID == 0 {
		return false
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, ownerID = ? WHERE locationID = ? AND flag = 5`,
		lb.ShipID, lb.CharID, victim.ShipID); err != nil {
		return false
	}
	for _, it := range items {
		m.noteLootTaken(ctx, lb.CharID, it.TypeID, m.typeNameOr(ctx, it.TypeID), it.Quantity)
	}
	where := m.nameOfSystem(ctx, sysID)
	m.logBot(lb.CharID, logInfo, "piracy", "held up %s in %s and took the hold — %s of cargo", vb.Name, where, iskWords(cargoISK))
	m.logBot(vb.CharID, logInfo, "piracy", "handed over %s of cargo to keep the ship", iskWords(cargoISK))
	grantAmnesty(vb.CharID)
	m.piracyRecordVictim(ctx, lb.CharID, vb.CharID, false)
	_ = bumpPiracyTally(ctx, m.db, lb.CharID, false)
	m.fileVictimBounty(ctx, vb.CharID, pirates)
	m.abstractConcord(ctx, m.chargeAbstractRobbery(ctx, victim, pirates, sysID), sysID, 0, false)
	var pirateCorps []int64
	for _, p := range pirates {
		if p != nil {
			pirateCorps = append(pirateCorps, p.CorpID)
		}
	}
	m.noteRobberyIncident(ctx, vb.CharID, victim.CorpID, vb.Name, pirateCorps, cargoISK, sysID)
	return true
}

// ── 5b: the checkpoint fleet ────────────────────────────────────────────────

// campGridState is a checkpoint member's grid memory: whom it has waved.
type campGridState struct {
	tickAt time.Time
	waved  map[int64]time.Time
}

// gangPlanCheckpoint routes one member of a checkpoint fleet onto its gate. seat is the
// member's own seat activity (seatActivityFor), the fit its seat check holds it to.
func (m *Mod) gangPlanCheckpoint(ctx context.Context, b *botRun, g *Gang, seat string) error {
	st := b.State
	st.Activity = ActCombat // a checkpoint flies its bounty-hunting fit, like the patrol
	ready, err := m.gangSeatReady(ctx, b, seat)
	if err != nil || !ready {
		return err
	}
	gs, err := m.checkpointSite(ctx, b, g)
	if err != nil {
		return err
	}
	if gs.CheckpointSystemID == 0 || gs.CheckpointGateID == 0 {
		return m.gangWait(ctx, b, "no lane on home ground worth a checkpoint — standing by")
	}
	st.Data = gangDutyData(st.Data, g)
	st.Data.CombatGateID = gs.CheckpointGateID
	m.logBot(b.CharID, logInfo, "gang", "holding the %s gate in %s with %q",
		gs.CheckpointFarName, gs.CheckpointSystemName, g.Name)
	return m.beginTravel(ctx, b, gs.CheckpointSystemID, 0, string(TaskGangCheckpoint))
}

// checkpointSite is the fleet's gate for the stint: the busiest lane in the
// employer's region in reach (lane.go, the checkpoint band), picked once by
// the first member to plan.
func (m *Mod) checkpointSite(ctx context.Context, b *botRun, g *Gang) (*GangState, error) {
	gs := &g.State
	now := clock.Unix()
	if gs.CheckpointSystemID != 0 && gs.CheckpointGateID != 0 && now-gs.CheckpointPickedAt < checkpointStintSec {
		return gs, nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	r := rulesForBot(b)
	admit := func(sysID int64) bool { return u.allowed(sysID, r) }
	recent := map[int64]bool{}
	for _, id := range gs.HuntRecent {
		recent[id] = true
	}
	camp, gate, far, ok := m.pickCampLane(ctx, b, laneCheckpoint, gankSearchHops, m.homeRegionSet(ctx, b), admit, recent)
	if !ok {
		gs.CheckpointSystemID, gs.CheckpointGateID = 0, 0
		return gs, nil
	}
	if gs.CheckpointSystemID != 0 {
		gs.HuntRecent = append([]int64{gs.CheckpointSystemID}, gs.HuntRecent...)
		if len(gs.HuntRecent) > gangHuntRecentMax {
			gs.HuntRecent = gs.HuntRecent[:gangHuntRecentMax]
		}
	}
	gs.CheckpointSystemID, gs.CheckpointSystemName = camp, m.nameOfSystem(ctx, camp)
	gs.CheckpointGateID, gs.CheckpointFarName = gate, m.nameOfSystem(ctx, far)
	gs.CheckpointPickedAt = now
	if err := saveGangState(ctx, m.db, g.GangID, gs); err != nil {
		return nil, err
	}
	m.logBot(b.CharID, logInfo, "gang", "gang %q sets up a checkpoint on the %s gate in %s",
		g.Name, gs.CheckpointFarName, gs.CheckpointSystemName)
	return gs, nil
}

// gangCheckpointStep keeps a member on the gate for the stint.
func (m *Mod) gangCheckpointStep(ctx context.Context, b *botRun, g *Gang) error {
	st, gs := b.State, &g.State
	if gs.CheckpointSystemID == 0 || gs.CheckpointGateID == 0 ||
		clock.Unix()-gs.CheckpointPickedAt >= checkpointStintSec ||
		st.Data.CombatGateID != gs.CheckpointGateID {
		st.Activity, st.Phase = "", string(TaskIdle)
		return saveState(ctx, m.db, st)
	}
	if b.SystemID != gs.CheckpointSystemID {
		return m.beginTravel(ctx, b, gs.CheckpointSystemID, 0, string(TaskGangCheckpoint))
	}
	if b.ShipID == 0 {
		return fmt.Errorf("checkpoint member has no ship boarded")
	}
	if !st.Data.CombatCampSet {
		m.parkAtGateArrival(ctx, b.ShipID, gs.CheckpointGateID)
		st.Data.CombatCampSet = true
	}
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(60)
	return saveState(ctx, m.db, st)
}

// checkpointOpNote is the Op Board's line for a checkpoint fleet.
func checkpointOpNote(gs *GangState) string {
	if gs.CheckpointSystemID == 0 {
		return "checkpoint — looking for a lane on home ground"
	}
	note := fmt.Sprintf("checkpoint on the %s gate in %s", gs.CheckpointFarName, gs.CheckpointSystemName)
	if gs.TollISK > 0 {
		note += fmt.Sprintf("; %s in tolls this op", fmtISK(gs.TollISK))
	}
	return note
}

// ── 5b: on the grid ─────────────────────────────────────────────────────────

// checkpointTick waves the pilots the rules of engagement spare: one line in
// Local per pilot per quarter hour, through the budgeted chatSay. Runs on the
// quiet branch; the line itself is said off the combat goroutine.
func (m *Mod) checkpointTick(ctx context.Context, eb *embodiedBot) {
	if eb.ent == nil || eb.ss == nil {
		return
	}
	now := clock.Now()
	if now.Sub(eb.camp.tickAt) < checkpointTickSec*time.Second {
		return
	}
	eb.camp.tickAt = now
	if eb.camp.waved == nil {
		eb.camp.waved = map[int64]time.Time{}
	}
	me := eb.ent.Pos()
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if !e.IsPlayer || e.Slim.CharID == 0 || !m.mayTargetPilot(eb, e) {
			continue
		}
		if e.Pos().Sub(me).Len() > checkpointWaveRangeM {
			continue
		}
		if at, ok := eb.camp.waved[e.Slim.CharID]; ok && now.Sub(at) < checkpointWaveGapSec*time.Second {
			continue
		}
		if m.wouldEngagePilot(eb, e) || m.tollable(eb, e) {
			continue // engaged or tolled, not waved
		}
		eb.camp.waved[e.Slim.CharID] = now
		speaker, about := partyFromEmbodied(eb), partyFromID(e.Slim.CharID, e.CharName, e.Slim.CorpID, e.Slim.TypeID, m.isSynthetic(e.Slim.CharID))
		sysID := eb.systemID
		goSafe("checkpoint wave", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			m.chatSay(ctx, sysID, false, speaker, about, evWaveThrough, motiveNone, nil, false, false)
		})
	}
}

// tollable reports whether this checkpoint would hold that pilot for a toll:
// the verdict's toll arm, read off the grid.
func (m *Mod) tollable(eb *embodiedBot, e *destiny.Entity) bool {
	if !eb.checkpoint || !checkpointTollOn() || eb.doctrine != DoctrineMercenary {
		return false
	}
	if !m.mayTargetPilot(eb, e) || underAmnesty(e.Slim.CharID) {
		return false
	}
	criminal := false
	if since := modapi.PvP().CriminalSince; since != nil {
		criminal = since(eb.systemID, e.Slim.CharID) > 0
	}
	st := stanceOf(m.gridEngageStandingToPilot(eb, e), false)
	band := secBand(loadedSystemSecurity(eb.systemID))
	return checkpointVerdict(st, eb.doctrine, band, criminal, e.Slim.SecurityStatus <= seclaw.OutlawStatus, false, true) == actToll
}

// tollTarget is the nearest pilot this checkpoint would toll, or 0.
func (m *Mod) tollTarget(eb *embodiedBot) uint32 {
	if eb.ent == nil || eb.ss == nil || !checkpointTollOn() {
		return 0
	}
	eb.mu.Lock()
	rs := eb.ransom
	eb.mu.Unlock()
	if rs != nil && rs.toll && !rs.settled {
		// Holding somebody already: stay on them.
		for _, e := range eb.ss.GetAllPlayerEntities() {
			if e.Slim.CharID == rs.victimCharID && !e.IsDestroyed() {
				return e.ID
			}
		}
		m.clearRansom(eb)
		return 0
	}
	me := eb.ent.Pos()
	var best uint32
	bestDist := math.MaxFloat64
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if !e.IsPlayer || e.IsDestroyed() || e.IsInWarp() || !m.tollable(eb, e) {
			continue
		}
		if d := e.Pos().Sub(me).Len(); d < bestDist {
			best, bestDist = e.ID, d
		}
	}
	return best
}

// tollEngage is piracyEngage for a checkpoint: point on, a price in Local, guns cold. It
// reports whether the caller holds fire this tick, which for a toll mark is always, until
// the mark is released and the amnesty keeps it off the list.
func (m *Mod) tollEngage(eb *embodiedBot, l *combatLoadout, victim *destiny.Entity) bool {
	if victim == nil || !eb.checkpoint {
		return false
	}
	if m.wouldEngagePilot(eb, victim) {
		return false // a legal target: the ordinary loop's business
	}
	victimCharID := victim.Slim.CharID
	eb.mu.Lock()
	rs := eb.ransom
	eb.mu.Unlock()
	if rs != nil && rs.victimCharID != victimCharID {
		return true // somebody else's terms are out; never fire on this one either
	}
	if rs == nil {
		if !m.tollable(eb, victim) {
			return true // not ours to shoot and not ours to toll: hold fire
		}
		if !m.tackleHolds(eb, l, victim) {
			return true // closing: fire never opens
		}
		m.openToll(eb, victim)
		return true
	}
	if rs.settled {
		return true
	}
	if clock.Now().Sub(rs.demandedAt) >= time.Duration(ransomPollSec)*time.Second && m.ransomPaid(eb, rs) {
		m.closeToll(eb, rs, true)
		return true
	}
	window := time.Duration(eb.tune.dwell(ransomWaitSec)) * time.Second
	if clock.Now().Sub(rs.demandedAt) < window {
		return true
	}
	m.closeToll(eb, rs, false)
	return true
}

// openToll puts the toll in Local and starts the clock.
func (m *Mod) openToll(eb *embodiedBot, victim *destiny.Entity) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base := m.ransomPrice(eb, victim)
	if cargoISK, hullISK, ok := m.ransomScan(ctx, eb, victim); ok {
		base = ransomPriceFromScan(cargoISK, hullISK, 1)
	}
	price := tollPrice(base, dialTollCapISK.Get())
	bal, _ := evedb.GetCharBalance(ctx, m.db, eb.charID)
	rs := &ransomState{
		victimCharID: victim.Slim.CharID, victimName: m.preyLink(victim),
		demanded: price, demandedAt: clock.Now(), baseline: bal, toll: true,
	}
	eb.mu.Lock()
	eb.ransom = rs
	eb.mu.Unlock()
	m.holdFleetForTerms(eb, rs)
	// The demand is the only way the mark learns the price: unbudgeted while
	// the dial says so, else a budgeted line like any other.
	if demandsAlwaysOn() {
		m.pirateSay(eb, evTollDemand, rs.victimCharID, rs.victimName, price)
	} else {
		speaker, about := partyFromEmbodied(eb), partyFromID(victim.Slim.CharID, victim.CharName, victim.Slim.CorpID, victim.Slim.TypeID, m.isSynthetic(victim.Slim.CharID))
		m.chatSay(ctx, eb.systemID, false, speaker, about, evTollDemand, motivePiracy, chatSlots{"isk": iskWords(price)}, true, false)
	}
	m.logBot(eb.charID, logInfo, "security", "holding %s at the checkpoint for a %s toll", victim.CharName, iskWords(price))
}

// closeToll ends the hold: paid or not, the mark is released and may pass. A paid toll buys
// an hour's amnesty and costs the checkpoint the SecRobberyPenalty a robbery would.
func (m *Mod) closeToll(eb *embodiedBot, rs *ransomState, paid bool) {
	eb.mu.Lock()
	rs.settled, rs.released = true, true
	eb.mu.Unlock()
	m.releaseFleetHold(eb, rs)
	m.releaseVictim(eb)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !paid {
		grantAmnesty(rs.victimCharID) // five minutes: not held twice on the way through
		m.logBot(eb.charID, logInfo, "security", "%s did not pay the toll — waved through anyway", rs.victimName)
		return
	}
	grantAmnestyFor(rs.victimCharID, tollAmnesty)
	m.logBot(eb.charID, logInfo, "security", "%s paid the %s toll — passage for the hour", rs.victimName, iskWords(rs.demanded))
	speaker := partyFromEmbodied(eb)
	about := partyFromID(rs.victimCharID, "", 0, 0, m.isSynthetic(rs.victimCharID))
	m.chatSay(ctx, eb.systemID, false, speaker, about, evTollPaid, motiveNone, chatSlots{"isk": iskWords(rs.demanded)}, false, true)
	m.chargeEmbodiedRobbery(eb, rs)
	victimCorpID, _ := m.affiliationOf(rs.victimCharID)
	m.noteRobberyIncident(ctx, rs.victimCharID, victimCorpID, rs.victimName, []int64{eb.corpID}, rs.demanded, eb.systemID)
	if eb.gangID != 0 {
		if g, err := getGang(ctx, m.db, eb.gangID); err == nil && g != nil {
			g.State.TollISK += rs.demanded
			_ = saveGangState(ctx, m.db, g.GangID, &g.State)
		}
	}
}

// checkpointTollAbstract is the unwatched system's toll: a Mercenary
// checkpoint fleet with nothing to engage holds one neutral bot for a toll;
// it pays if it can and is released either way. True when a hold happened.
func (m *Mod) checkpointTollAbstract(ctx context.Context, lb *botRun, sysID int64, g *Gang, ours []*pvpCombatant, doc Doctrine) bool {
	if g == nil || g.State.Focus != gangFocusCheckpoint || !checkpointTollOn() || doc != DoctrineMercenary {
		return false
	}
	sec := 0.0
	if u, err := m.universe(ctx); err == nil {
		sec = u.meta[sysID].Security
	}
	if secBand(sec) == BandPoliced {
		return false
	}
	victimID, _ := m.pickAbstractPrey(ctx, sysID, ours, false)
	if victimID == 0 {
		return false
	}
	vb, err := m.loadRun(ctx, victimID)
	if err != nil {
		return false
	}
	victim := m.pvpCombatantFor(ctx, victimID, RoleCombat)
	if victim == nil {
		return false
	}
	base := float64(ransomBaseISK)
	if cargo, err := m.holdUsedM3(ctx, victimID, victim.ShipID); err == nil && cargo > 0 {
		base *= 1 + math.Sqrt(cargo/100)
	}
	price := tollPrice(base, dialTollCapISK.Get())
	bal, _ := evedb.GetCharBalance(ctx, m.db, victimID)
	where := m.nameOfSystem(ctx, sysID)
	if bal < price*1.2 || m.handISK(ctx, victimID, lb.CharID, price) != nil {
		grantAmnesty(victimID)
		m.logBot(lb.CharID, logInfo, "security", "held %s at the %s checkpoint — could not pay the %s toll, waved through", vb.Name, where, iskWords(price))
		return true
	}
	grantAmnestyFor(victimID, tollAmnesty)
	g.State.TollISK += price
	_ = saveGangState(ctx, m.db, g.GangID, &g.State)
	m.logBot(lb.CharID, logInfo, "security", "collected a %s toll from %s at the %s checkpoint", iskWords(price), vb.Name, where)
	m.logBot(victimID, logInfo, "security", "paid a %s toll to pass %s's checkpoint in %s", iskWords(price), lb.Name, where)
	m.abstractConcord(ctx, m.chargeAbstractRobbery(ctx, victim, ours, sysID), sysID, 0, false)
	var corps []int64
	for _, p := range ours {
		if p != nil {
			corps = append(corps, p.CorpID)
		}
	}
	m.noteRobberyIncident(ctx, victimID, victim.CorpID, vb.Name, corps, price, sysID)
	return true
}
