package rolebots

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
)

// Station sieges: an alliance that holds sov reaches for the conquerable stations around it. A
// conquerable changes hands when its shield reaches 0 (the core's conquerStation). The station is
// a milestone on the alliance's ladder (allianceladder.go), not a war: CONCORD, the stations' first
// holder, is a neutral nobody answers to, and a rival owner is fair game in 0.0.
//
// The executor opens one station siege at a time (allianceStationSiege) on the best conquerable
// within stationSiegeReachJumps of held sov whose quota (siegeQuota: its shield over the bash
// window plus the regeneration it must out-shoot) the alliance can field (siegeCapacityFor). The
// row is a rolebot_corp_siege row of kind 'station' (towerID = stationID), led by the member
// corporation nearest the station; every member's Security wing is called to it (armsping.go).
// Stages: muster, bash (siegeStageNext), closed taken / held / abandoned / gone. Attackers on a
// watched grid land real hits through modapi.PvP().StationHit; off-grid the desk grinds the shield
// net of its regeneration (stationBashStep) and captures through ConquerStation.

const (
	// stationSiegeReachJumps is how far from held sov a conquerable may be to be besieged.
	stationSiegeReachJumps = 5
	// stationSiegeRetrySec is how long the alliance leaves a station alone after a siege on it was
	// abandoned or held, and waits after a siege whose fleet never formed before opening another.
	stationSiegeRetrySec = 3 * 86400
)

// stationBashStep grinds elapsed seconds of the attackers' dps into a station shield net of its
// regeneration and reports whether the shield broke. Standing damage never falls below 0. Pure.
func stationBashStep(damage, dps, regen, elapsed, ehp float64) (float64, bool) {
	damage = max(0, damage+(dps-regen)*elapsed)
	return damage, ehp > 0 && damage >= ehp
}

// stationSiegeIn is what the executor reads about one conquerable.
type stationSiegeIn struct {
	InHeld   bool    // in a system the alliance holds
	Jumps    int     // from the nearest held system
	Services int     // cloning, repair, factory offered
	Defended bool    // its owner is a managed corporation that can call a defence
	Danger   float64 // towerDanger at its system
}

// stationSiegeScore is how much the alliance wants a station: one in its own sov most, one on the
// border next, the more services the better, an undefended one easier; distance and danger pull it
// down. Pure.
func stationSiegeScore(in stationSiegeIn) float64 {
	s := 1 + 0.25*float64(max(in.Services, 0))
	switch {
	case in.InHeld:
		s *= 3
	case in.Jumps <= 1:
		s *= 1.5
	}
	if !in.Defended {
		s *= 1.25
	}
	return s / (1 + 0.25*float64(max(in.Jumps, 0))) / (1 + max(in.Danger, 0))
}

// stationBall is a station's position and radius (mapDenormalize); ok false when it is no station.
func (m *Mod) stationBall(ctx context.Context, stationID int64) (x, y, z, radius float64, ok bool) {
	err := m.db.QueryRowContext(ctx, `
		SELECT x, y, z, COALESCE(radius, 0) FROM mapDenormalize WHERE itemID = ? AND groupID = 15`, stationID).
		Scan(&x, &y, &z, &radius)
	if err != nil || (x == 0 && y == 0 && z == 0) {
		return 0, 0, 0, 0, false
	}
	return x, y, z, radius, true
}

// allianceCorps is every corporation of an alliance.
func (m *Mod) allianceCorps(ctx context.Context, allianceID int64) []int64 {
	ids, _ := int64Column(ctx, m, `SELECT corporationID FROM corporation WHERE allianceID = ?`, allianceID)
	return ids
}

// allianceStationSiege is the executor's station desk: it advances the alliance's open station
// siege, or opens one on the best conquerable around held sov once the alliance has sov.
func (m *Mod) allianceStationSiege(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat) error {
	corps := m.allianceCorps(ctx, seat.AllianceID)
	if len(corps) == 0 {
		return nil
	}
	open := m.openSieges(ctx, `targetKind = 'station' AND attackerCorpID IN (`+int64CSV(corps)+`)`)
	for _, s := range open {
		lead := c
		if lc, _ := getCEO(ctx, m.db, s.AttackerCorpID); lc != nil {
			lead = lc
		}
		if err := m.advanceStationSiege(ctx, lead, s); err != nil {
			m.logBot(c.CharID, logError, "ceo", "station siege %d: %v", s.SiegeID, err)
		}
	}
	m.defenderStationSieges(ctx)
	if len(open) > 0 || m.allianceStage(ctx, seat.AllianceID) < stageSov {
		return nil
	}
	// One attacking siege per alliance: a member's tower siege keeps the wing on that.
	if m.siegeSidesBusy(ctx, corps) {
		return nil
	}
	now := clock.Unix()
	var lastFailed int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(closedAt), 0) FROM rolebot_corp_siege
		 WHERE targetKind = 'station' AND result = 'abandoned' AND damage = 0
		   AND attackerCorpID IN (`+int64CSV(corps)+`)`).Scan(&lastFailed)
	if lastFailed > 0 && now-lastFailed < stationSiegeRetrySec {
		return nil
	}
	if m.siegeShortHold(ctx, corps, now) {
		return nil
	}
	picks := m.pickStationTargets(ctx, c, ch, seat, corps)
	if len(picks) == 0 {
		return nil
	}
	// The best station whose quota the alliance can field: a better one it cannot break is passed
	// over for one it can.
	capa, ps := m.siegeCapacityFor(ctx, corps)
	var (
		pick  stationTarget
		q     siegeQuotaOut
		found bool
	)
	for i, t := range picks {
		if i >= siegeOpenScan {
			break
		}
		tq := siegeQuota(m.structureHPOf(ctx, t.typeID, false), siegeWindowSec, siegeQuotaMargin)
		if capa.WithKits >= tq.OpenDPS() {
			pick, q, found = t, tq, true
			break
		}
	}
	if !found {
		tq := siegeQuota(m.structureHPOf(ctx, picks[0].typeID, false), siegeWindowSec, siegeQuotaMargin)
		what := stationNameOf(ctx, m, picks[0].stationID)
		// Hulls bought for the pilots without one may close the gap (siegekits.go); the siege opens
		// on a later pass once they are on the shelf.
		m.provisionSiegeKits(ctx, c, ps, tq.OpenDPS()-capa.WithKits, what)
		if now-m.lastCEOActionAt(ctx, c.CorpID, "stationsiege.short") >= 86400 {
			m.ceoLog(ctx, c, powerAlliance, "stationsiege.short", picks[0].stationID, tq.OpenDPS(), "%s",
				siegeShortLine(what, capa.WithKits, tq.OpenDPS()))
		}
		return nil
	}
	leader := m.stationSiegeLeader(ctx, corps, pick.sysID, c.CorpID)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_siege (towerID, systemID, attackerCorpID, defenderCorpID, phase, openedAt, lastAt, stageAt, targetKind,
		                                quotaDPS, regenEHP)
		VALUES (?, ?, ?, ?, 'muster', ?, ?, ?, 'station', ?, ?)`,
		pick.stationID, pick.sysID, leader, pick.owner, now, now, now, q.OpenDPS(), q.Regen); err != nil {
		return fmt.Errorf("open station siege: %w", err)
	}
	name := stationNameOf(ctx, m, pick.stationID)
	line := fmt.Sprintf("moves on %s, held by %s: %s to break the shield, the alliance can field ~%.0f",
		name, corpNameOf(ctx, m.db, pick.owner), siegeQuotaNote(q), capa.WithKits)
	m.ceoLog(ctx, c, powerAlliance, "stationsiege", pick.stationID, q.OpenDPS(), "%s", line)
	m.allianceBook(ctx, seat.AllianceID, leader, "stationsiege", pick.stationID, q.OpenDPS(), "%s", line)
	m.allianceBulletin(ctx, seat.AllianceID, c.CharID, fmt.Sprintf("%s: take %s", seat.Name, name),
		fmt.Sprintf("The alliance moves on %s in %s. %s leads; every member corporation's Security wing forms up there. "+
			"Its shield at zero makes the station ours.", name, systemNameOf(ctx, m, pick.sysID), corpNameOf(ctx, m.db, leader)))
	wakeCEO(leader)
	return nil
}

// stationTarget is the conquerable the executor picked.
type stationTarget struct {
	stationID, sysID, owner, typeID int64
	score                           float64
}

// pickStationTargets is the conquerables within stationSiegeReachJumps of held sov that no member
// or blue holds, nobody is besieging, and the alliance has not lately failed on, best first by
// stationSiegeScore.
func (m *Mod) pickStationTargets(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, corps []int64) []stationTarget {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return nil
	}
	held := m.allianceHeldSystems(ctx, seat.AllianceID)
	border := map[int64]int{}
	for h := range held {
		for sys, j := range bfsJumps(u, h, stationSiegeReachJumps) {
			if cur, ok := border[sys]; !ok || j < cur {
				border[sys] = j
			}
		}
	}
	if len(border) == 0 {
		return nil
	}
	now := clock.Unix()
	resting := map[int64]bool{}
	if ids, err := int64Column(ctx, m, `
		SELECT towerID FROM rolebot_corp_siege
		 WHERE targetKind = 'station' AND closedAt > ? AND result IN ('abandoned', 'held')
		   AND attackerCorpID IN (`+int64CSV(corps)+`)`, now-stationSiegeRetrySec); err == nil {
		for _, id := range ids {
			resting[id] = true
		}
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT st.stationID, st.solarSystemID, st.corporationID, st.stationTypeID
		  FROM staStations st
		  JOIN staStationTypes t ON t.stationTypeID = st.stationTypeID AND t.conquerable = 1
		 WHERE st.solarSystemID IN (`+int64CSV(mapKeys(border))+`)`)
	if err != nil {
		return nil
	}
	var cands []stationTarget
	for rows.Next() {
		var t stationTarget
		if rows.Scan(&t.stationID, &t.sysID, &t.owner, &t.typeID) != nil || resting[t.stationID] {
			continue
		}
		cands = append(cands, t)
	}
	rows.Close()
	var out []stationTarget
	for _, t := range cands {
		if slices.Contains(corps, t.owner) || u.meta[t.sysID].Security >= 0 {
			continue
		}
		if len(m.openSieges(ctx, `towerID = ? AND targetKind = 'station'`, t.stationID)) > 0 {
			continue
		}
		if relDeclaredBlue(c.CorpID, seat.AllianceID, t.owner, m.corpAllianceID(ctx, t.owner)) || relAllied(c.CorpID, t.owner) {
			continue
		}
		services := 0
		for _, svc := range []int{serviceCloning, serviceRepairFacilities, serviceFactory} {
			if m.stationOffers(ctx, t.stationID, svc) {
				services++
			}
		}
		_, managed := managedCEO(t.owner)
		t.score = stationSiegeScore(stationSiegeIn{InHeld: held[t.sysID], Jumps: border[t.sysID], Services: services,
			Defended: managed, Danger: m.towerDanger(ctx, c.CorpID, t.sysID, u.meta[t.sysID].Security, ch.PosDangerWeight)})
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].stationID < out[j].stationID
	})
	return out
}

// mapKeys is a set's keys.
func mapKeys[V any](mp map[int64]V) []int64 {
	out := make([]int64, 0, len(mp))
	for k := range mp {
		out = append(out, k)
	}
	return out
}

// stationSiegeLeader is the member corporation that leads a station siege: the one whose front or
// forward office lies nearest the station's system, the larger Security arm on a tie; fallback when
// none has either.
func (m *Mod) stationSiegeLeader(ctx context.Context, corps []int64, sysID, fallback int64) int64 {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return fallback
	}
	near := bfsJumps(u, sysID, 30)
	best, bestJ, bestArm := fallback, math.MaxInt32, -1
	for _, corp := range corps {
		if _, ok := managedCEO(corp); !ok {
			continue
		}
		st := m.frontOfficeOf(ctx, corp)
		if st == 0 {
			st = m.forwardOfficeOf(ctx, corp)
		}
		if st == 0 {
			continue
		}
		j, ok := near[m.stationSystemID(ctx, st)]
		if !ok {
			continue
		}
		arm := armCount(corp, armSecurity)
		if j < bestJ || (j == bestJ && arm > bestArm) {
			best, bestJ, bestArm = corp, j, arm
		}
	}
	return best
}

// advanceStationSiege moves one station siege along for its leading corporation c.
func (m *Mod) advanceStationSiege(ctx context.Context, c *corpCEO, s *siegeRow) error {
	now := clock.Unix()
	bashing, holding := false, false
	side := append([]int64{c.CorpID}, m.allianceMemberCorps(ctx, c.CorpID)...)
	defer func() {
		// On a watched grid the guns stay live while the fleet holds the system (possiege.go).
		noteSiegeBash(s.TowerID, (bashing || holding) && s.Stage == "bash", now)
		var mark []int64
		if bashing || (holding && s.Stage != "done") {
			mark = side
		}
		noteSiegeHolding(s.SystemID, mark, now)
	}()
	var owner, typeID int64
	if err := m.db.QueryRowContext(ctx, `SELECT corporationID, stationTypeID FROM staStations WHERE stationID = ?`,
		s.TowerID).Scan(&owner, &typeID); err != nil {
		m.closeSiege(ctx, s, "gone", "the station is no longer there")
		return nil
	}
	if slices.Contains(side, owner) {
		m.closeSiege(ctx, s, "taken", "the station is the alliance's")
		return nil
	}
	if owner != s.DefenderCorpID {
		// It changed hands to somebody else while the fleet gathered: still fair game unless blue.
		if relDeclaredBlue(c.CorpID, m.corpAllianceID(ctx, c.CorpID), owner, m.corpAllianceID(ctx, owner)) {
			m.closeSiege(ctx, s, "abandoned", "the station went to a friend")
			return nil
		}
		s.DefenderCorpID = owner
	}
	if siegeStalled(s, now) {
		m.closeSiege(ctx, s, "abandoned", "the siege stalled without breaking the shield")
		return nil
	}
	if s.Stage != "muster" && s.Stage != "bash" {
		setStage(s, "muster", now)
	}
	gridFight := m.systemProjected(s.SystemID)
	elapsed := float64(min(now-s.LastAt, siegeMaxElapsedSec))
	s.LastAt = now
	ss, station := m.siegeTowerEntity(s)
	fleet := m.siegeAttackers(ctx, c.CorpID, s.SystemID)
	att := siegePresent(fleet, s.SystemID, gridFight, station)
	holding = gridFight && len(fleet) > 0
	hp := m.structureHPOf(ctx, typeID, false)
	q := siegeQuota(hp, siegeWindowSec, siegeQuotaMargin)
	res := resOr(hp.SRes, 1)
	ehp, regen := q.BashEHP, q.Regen
	s.FleetDPS = siegeFleetDPS(att)
	was := s.Stage
	next := siegeStageNext(siegeStageIn{Stage: s.Stage, StageAt: s.StageAt, OpenedAt: s.OpenedAt, Now: now,
		LowSince: s.LowSince, FleetLive: m.siegeFleetLive(ctx, s), FleetDPS: s.FleetDPS, Regen: regen,
		Window: siegeWindowSec})
	s.LowSince = next.LowSince
	if next.Abandon != "" {
		m.closeSiege(ctx, s, siegeAbandonResult(was), next.Abandon)
		return nil
	}
	setStage(s, next.Stage, now)
	name := stationNameOf(ctx, m, s.TowerID)
	if was == "muster" && s.Stage == "bash" && s.Damage == 0 {
		m.ceoLog(ctx, c, powerWar, "stationsiege", s.TowerID, s.FleetDPS,
			"opened fire on %s, held by %s — %.0f DPS on it against %.0f/s of shield regeneration",
			name, corpNameOf(ctx, m.db, owner), s.FleetDPS, regen)
		if aid := m.corpAllianceID(ctx, c.CorpID); aid != 0 {
			m.allianceBook(ctx, aid, c.CorpID, "stationsiege", s.TowerID, float64(len(att)),
				"%d pilot(s) opened fire on %s with %.0f DPS", len(att), name, s.FleetDPS)
		}
	}
	if s.Stage != "bash" || len(att) == 0 {
		// Mustering, or a bash with nobody present: the low-DPS clock (siegeStageNext) runs it down.
		m.saveSiege(ctx, s)
		return nil
	}
	// A managed owner's defenders on grid: the grid fights it out when projected, else the abstract
	// team fight runs before any damage lands.
	if _, managed := managedCEO(owner); managed {
		if def := siegePresent(m.siegeSideOwner(ctx, owner, s.SystemID, "dps"), s.SystemID, gridFight, station); len(def) > 0 {
			if siegeFightOnGrid(gridFight, len(def)) {
				m.saveSiege(ctx, s)
				return nil
			}
			out := resolveTeamFight(att, def, siegeBreakFrac, siegeBreakFrac, false)
			for _, v := range out.ADead {
				m.killBotShip(ctx, v, def, false)
			}
			for _, v := range out.BDead {
				m.killBotShip(ctx, v, att, false)
			}
			if len(out.ADead) >= len(att) {
				m.closeSiege(ctx, s, "held", fmt.Sprintf("the defence broke the siege (%d attackers lost)", len(out.ADead)))
				return nil
			}
			if !out.BEscaped && len(out.BDead) < len(def) {
				m.saveSiege(ctx, s)
				return nil
			}
		}
	}
	bashing = true
	if station != nil {
		// The ball holds the true shield. A freshly loaded ball is full (station HP is memory only), so
		// the desk's standing damage is carried onto it once.
		taken := max(station.HP.MaxShield-station.HP.CurShield, 0) / res
		if s.Damage > taken && !gridFight {
			if m.stationHit(ss, station, s.Damage-taken, c.CorpID, att[0].CharID) {
				m.closeSiege(ctx, s, "taken", "the shield broke")
				return nil
			}
		}
		s.Damage = max(station.HP.MaxShield-station.HP.CurShield, 0) / res
	}
	dps := 0.0
	for _, a := range att {
		if !gridFight && !siegeGridShooting(a.CharID, now) {
			dps += a.DPS
		}
	}
	before := s.Damage
	var broken bool
	if station != nil {
		// A loaded ball regenerates on its own (the core's shield regen); the desk adds raw fire.
		s.Damage, broken = stationBashStep(s.Damage, dps, 0, elapsed, ehp)
	} else {
		s.Damage, broken = stationBashStep(s.Damage, dps, regen, elapsed, ehp)
	}
	leader := att[0].CharID
	switch {
	case station != nil && s.Damage > before:
		if m.stationHit(ss, station, s.Damage-before, c.CorpID, leader) {
			m.closeSiege(ctx, s, "taken", "the shield broke")
			return nil
		}
	case station == nil && broken:
		conquer := modapi.PvP().ConquerStation
		if conquer == nil {
			return fmt.Errorf("no station conquest seam")
		}
		if err := conquer(s.TowerID, c.CorpID, leader); err != nil {
			return fmt.Errorf("conquer %d: %w", s.TowerID, err)
		}
		m.closeSiege(ctx, s, "taken", "the shield broke")
		return nil
	}
	m.saveSiege(ctx, s)
	return nil
}

// stationHit lands dmg (before resists, as the desk counts it) on a loaded station ball, spread over
// the four damage types, then runs the core's station rule (modapi.PvP().StationHit): the damage
// state broadcast and the capture once the shield is gone. Reports whether the station changed hands.
func (m *Mod) stationHit(ss *destiny.SolarSystem, station *destiny.Entity, dmg float64, corpID, charID int64) bool {
	if ss == nil || station == nil || dmg <= 0 {
		return false
	}
	q := dmg / 4
	station.ApplyDamage(q, q, q, q)
	hit := modapi.PvP().StationHit
	return hit != nil && hit(ss, station, corpID, charID)
}

// stationSiegeEvents subscribes to the core's station events: a capture closes the siege and books
// the milestone; a hit on a managed corporation's station calls its alliance to defend it.
func (m *Mod) stationSiegeEvents() {
	q := make(chan evedb.WorldEvent, 64)
	evedb.SubscribeWorldEvents(func(ev evedb.WorldEvent) {
		switch ev.(type) {
		case evedb.StationConqueredEvent, evedb.StationUnderAttackEvent:
			if !Master() {
				return
			}
			select {
			case q <- ev:
			default:
			}
		}
	})
	go func() {
		for ev := range q {
			m.stationEventOne(ev)
		}
	}()
}

// stationEventOne handles one station event off the feed. A panic loses that event, not the worker.
func (m *Mod) stationEventOne(ev evedb.WorldEvent) {
	defer func() {
		if r := recover(); r != nil {
			rlogf("rolebots: station event panicked: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch e := ev.(type) {
	case evedb.StationConqueredEvent:
		m.onStationConquered(ctx, e)
	case evedb.StationUnderAttackEvent:
		m.onStationUnderAttack(ctx, e)
	}
}

// onStationConquered closes the sieges on a captured station, books the new owner's alliance its
// milestone and tells both alliances, and wakes both CEOs so the capital moves (ceoCapital).
func (m *Mod) onStationConquered(ctx context.Context, e evedb.StationConqueredEvent) {
	for _, s := range m.openSieges(ctx, `towerID = ? AND targetKind = 'station'`, e.StationID) {
		result := "taken"
		if s.AttackerCorpID != e.NewCorp && !slices.Contains(m.allianceMemberCorps(ctx, s.AttackerCorpID), e.NewCorp) {
			result = "gone"
		}
		m.closeSiege(ctx, s, result, fmt.Sprintf("%s took the station", corpNameOf(ctx, m.db, e.NewCorp)))
	}
	name := stationNameOf(ctx, m, e.StationID)
	if aid := m.corpAllianceID(ctx, e.NewCorp); aid != 0 {
		m.noteMilestone(ctx, aid, milestoneStation, e.SystemID, e.StationID,
			fmt.Sprintf("%s took %s", corpNameOf(ctx, m.db, e.NewCorp), name))
	}
	if aid := m.corpAllianceID(ctx, e.OldCorp); aid != 0 {
		m.allianceBook(ctx, aid, e.OldCorp, "station.lost", e.StationID, 0, "lost %s to %s", name, corpNameOf(ctx, m.db, e.NewCorp))
	}
	wakeCEO(e.NewCorp)
	wakeCEO(e.OldCorp)
}

// onStationUnderAttack calls a managed owner's defence: a defender row the arms ping reads, and the
// alliance's defend call. An attack by the owner's own side, or a stray hit already answered, is
// ignored.
func (m *Mod) onStationUnderAttack(ctx context.Context, e evedb.StationUnderAttackEvent) {
	if _, managed := managedCEO(e.OwnerCorp); !managed || e.AttackerCorp == e.OwnerCorp {
		return
	}
	if slices.Contains(m.allianceMemberCorps(ctx, e.OwnerCorp), e.AttackerCorp) {
		return
	}
	now := clock.Unix()
	m.defenderStationSieges(ctx)
	if open := m.openSieges(ctx, `towerID = ? AND targetKind = 'station'`, e.StationID); len(open) > 0 {
		for _, s := range open {
			if s.AttackerCorpID == 0 {
				s.LastAt = now // still under fire: the defender row stands
				m.saveSiege(ctx, s)
			}
		}
	} else {
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_siege (towerID, systemID, attackerCorpID, defenderCorpID, phase, openedAt, lastAt, stageAt, targetKind)
			VALUES (?, ?, 0, ?, 'bash', ?, ?, ?, 'station')`, e.StationID, e.SystemID, e.OwnerCorp, now, now, now)
		if c, _ := getCEO(ctx, m.db, e.OwnerCorp); c != nil {
			m.ceoLog(ctx, c, powerWar, "stationsiege", e.StationID, e.ShieldFrac,
				"%s is under fire from %s — shield at %.0f%%", stationNameOf(ctx, m, e.StationID),
				corpNameOf(ctx, m.db, e.AttackerCorp), e.ShieldFrac*100)
		}
	}
	m.allianceRaiseDefendNote(ctx, e.OwnerCorp, e.StationID, e.SystemID, 0,
		fmt.Sprintf("%s's station %s is under fire", corpNameOf(ctx, m.db, e.OwnerCorp), stationNameOf(ctx, m, e.StationID)))
}

// defenderStationSieges closes the defender-only station rows (attacker 0) whose attack has gone
// quiet: no StationUnderAttackEvent refreshed them for stationDefendQuietSec.
func (m *Mod) defenderStationSieges(ctx context.Context) {
	cut := clock.Unix() - stationDefendQuietSec
	for _, s := range m.openSieges(ctx, `targetKind = 'station' AND attackerCorpID = 0 AND lastAt < ?`, cut) {
		m.closeSiege(ctx, s, "held", "the attack went quiet")
	}
}

// stationDefendQuietSec is how long a defender-only station row stands after the last hit it heard of.
const stationDefendQuietSec = 30 * 60
