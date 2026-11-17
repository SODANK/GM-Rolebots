package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
)

// Sieges: towers as war objectives. A corporation at war picks an enemy tower it can reach, opens
// a siege and points its combat wing there (siegeOrderFor). Stages: muster (fleet on its way,
// nothing shot, nobody told), bash (siegeMinAttackers online attackers in system, shield to the
// reinforcement floor), timer (both sides form up for the exit), fight, kill. Attackers on a
// watched grid land real hits (siege_embodied.go); the desk adds the rest and keeps the tower's
// hit points in step (syncSiegeTower). docs/STARBASES.md "Sieges" has the full rules.

const (
	siegeMaxOpen        = 1
	siegeFormUpSec      = 2 * 3600
	siegeInSystemSec    = 30 * 60
	siegeAfterExitSec   = 3600
	siegeMaxAgeSec      = 4 * 86400 // how long any siege may stay open, however its stages move
	siegeMinAttackers   = 3
	siegeMaxElapsedSec  = 1800
	siegeTowerResonance = 0.5 // a tower's shield resists, averaged, for the abstract bash
	siegeBreakFrac      = 0.3
	// siegeStageMaxSec is how long a bash, kill or fight stage may run before the attacker gives up
	// (siegeStalled). Without it a siege with a little damage and too few attackers never ends.
	siegeStageMaxSec = 48 * 3600
	// siegeRetrySec is how long an attacker leaves a tower alone after its siege there was
	// abandoned or held (maybeOpenSiege).
	siegeRetrySec = 3 * 86400
)

type siegeRow struct {
	SiegeID, TowerID, SystemID int64
	AttackerCorpID             int64
	DefenderCorpID             int64
	ExitAt                     int64
	Stage, Result              string
	Damage                     float64
	LastAt, OpenedAt           int64
	StageAt                    int64 // when Stage was entered; 0 on rows older than the column
	// Kind is siegeKindTower (TowerID is the tower) or siegeKindStation (TowerID is a conquerable
	// station, stationsiege.go).
	Kind string
	// The quota the siege opened on (siegequota.go): the DPS it takes, and the shield's peak
	// regeneration in EHP/s. 0 on rows older than the columns; read through siegeQuotaFor.
	QuotaDPS, RegenEHP float64
	// FleetDPS is the attackers' DPS at the last pass; LowSince when it last fell to or under the
	// regeneration in a bash (siegeStageNext), 0 while it is ahead.
	FleetDPS float64
	LowSince int64
	// GangID is the siege's own fleet, 0 when it has none (siegefleet.go).
	GangID int64
}

const (
	siegeKindTower   = "tower"
	siegeKindStation = "station"
)

const siegeCols = `siegeID, towerID, systemID, attackerCorpID, defenderCorpID, exitAt, phase, result, damage, lastAt, openedAt, stageAt, targetKind, quotaDPS, regenEHP, fleetDPS, lowSince, gangID`

func scanSiege(sc interface{ Scan(...any) error }) (*siegeRow, error) {
	var s siegeRow
	err := sc.Scan(&s.SiegeID, &s.TowerID, &s.SystemID, &s.AttackerCorpID, &s.DefenderCorpID, &s.ExitAt,
		&s.Stage, &s.Result, &s.Damage, &s.LastAt, &s.OpenedAt, &s.StageAt, &s.Kind,
		&s.QuotaDPS, &s.RegenEHP, &s.FleetDPS, &s.LowSince, &s.GangID)
	return &s, err
}

// isStation reports whether a siege is on a conquerable station.
func (s *siegeRow) isStation() bool { return s.Kind == siegeKindStation }

// setStage moves a siege to a stage and starts the stage clock siegeStalled reads.
func setStage(s *siegeRow, stage string, now int64) {
	if s.Stage != stage {
		s.Stage, s.StageAt = stage, now
	}
}

// siegeStalled reports whether a siege has sat in a bash, kill or fight stage longer than
// siegeStageMaxSec, or has been open longer than siegeMaxAgeSec. A row from before the stage clock
// counts from its opening. Pure.
func siegeStalled(s *siegeRow, now int64) bool {
	if s.OpenedAt > 0 && now-s.OpenedAt > siegeMaxAgeSec {
		return true
	}
	switch s.Stage {
	case "bash", "kill", "fight":
	default:
		return false
	}
	start := s.StageAt
	if start == 0 {
		start = s.OpenedAt
	}
	return start > 0 && now-start > siegeStageMaxSec
}

func (m *Mod) openSieges(ctx context.Context, where string, args ...any) []*siegeRow {
	rows, err := m.db.QueryContext(ctx, `SELECT `+siegeCols+` FROM rolebot_corp_siege WHERE closedAt = 0 AND `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*siegeRow
	for rows.Next() {
		if s, err := scanSiege(rows); err == nil {
			out = append(out, s)
		}
	}
	return out
}

func (m *Mod) saveSiege(ctx context.Context, s *siegeRow) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_siege SET exitAt = ?, phase = ?, result = ?, damage = ?, lastAt = ?, stageAt = ?,
		       fleetDPS = ?, lowSince = ? WHERE siegeID = ?`,
		s.ExitAt, s.Stage, s.Result, s.Damage, s.LastAt, s.StageAt, s.FleetDPS, s.LowSince, s.SiegeID)
}

func (m *Mod) closeSiege(ctx context.Context, s *siegeRow, result, note string) {
	s.Stage, s.Result = "done", result
	now := clock.Unix()
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_siege SET phase = 'done', result = ?, closedAt = ?, note = ? WHERE siegeID = ?`,
		result, now, truncate(note, 255), s.SiegeID)
	// The siege's own fleet goes with it: the sweep disbands it this pass (leaseSiegeGangs).
	if s.GangID != 0 {
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_gangs SET expiresAt = ? WHERE gangID = ? AND expiresAt > ?`,
			now, s.GangID, now)
	}
	// The calls to arms for the tower close with it, so their fleets wind down now instead of
	// waiting out the call's TTL (gangcalls.go).
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_fleet_help SET closedAt = ?
		 WHERE kind = ? AND systemID = ? AND corpID IN (?, ?) AND closedAt = 0`,
		now, helpCallToArms, s.SystemID, s.AttackerCorpID, s.DefenderCorpID)
	what := "the tower"
	if s.isStation() {
		what = stationNameOf(ctx, m, s.TowerID)
	}
	for _, corpID := range []int64{s.AttackerCorpID, s.DefenderCorpID} {
		if c, _ := getCEO(ctx, m.db, corpID); c != nil {
			m.ceoLog(ctx, c, powerWar, "siege", s.TowerID, 0, "the siege of %s in %s ended: %s — %s",
				what, systemNameOf(ctx, m, s.SystemID), result, note)
		}
	}
}

// siegeOrderFor is where a corporation's combat wing must be because of a
// siege: the attacker's target while it bashes or fights, either side's tower
// system from two hours before a reinforcement exit until an hour after.
func (m *Mod) siegeOrderFor(ctx context.Context, corpID int64) (int64, string) {
	now := clock.Unix()
	for _, s := range m.sidesSieges(ctx, corpID) {
		if ok, why := siegeArmsWindow(s, corpID, now, systemNameOf(ctx, m, s.SystemID)); ok {
			return s.SystemID, why
		}
	}
	return 0, ""
}

// sidesSieges is every open siege corpID fights in: its own, attacking or defending, and a station
// siege an alliance-mate leads or defends, which is the whole alliance's (stationsiege.go). An
// alliance-mate's station row comes back seen from corpID's side: corpID in the leader's or the
// owner's seat.
func (m *Mod) sidesSieges(ctx context.Context, corpID int64) []*siegeRow {
	out := m.openSieges(ctx, `(attackerCorpID = ? OR defenderCorpID = ?) ORDER BY exitAt`, corpID, corpID)
	mates := m.allianceMemberCorps(ctx, corpID)
	if len(mates) == 0 {
		return out
	}
	csv := int64CSV(mates)
	for _, s := range m.openSieges(ctx, `targetKind = 'station' AND (attackerCorpID IN (`+csv+`) OR defenderCorpID IN (`+csv+`))
		ORDER BY exitAt`) {
		v := *s
		if slices.Contains(mates, s.AttackerCorpID) {
			v.AttackerCorpID = corpID
		} else {
			v.DefenderCorpID = corpID
		}
		out = append(out, &v)
	}
	return out
}

// siegeHoldMarginM is how far outside a tower's force field a siege line orbits: a ship inside
// the field cannot lock the tower, and an attacker's line does not sit on the field's edge.
const siegeHoldMarginM = 5_000.0

// siegeStationHoldM is how far off a besieged station's hull its siege line orbits: inside a
// frigate's guns, outside the undock's traffic. A station has no force field to stand outside.
const siegeStationHoldM = 7_500.0

// siegeHoldFor is where a fleet of corpID holds in sysID because of a siege its side has open
// there: the tower's moon to warp to, the tower to orbit, the orbit outside the force field, and
// bash true for the attacking side. at is 0 when there is no such siege. It reads the database and
// runs at embodiment (loadFleetSeat), never on the grid tick.
func (m *Mod) siegeHoldFor(ctx context.Context, corpID, sysID int64) (at, ball int64, rangeM float64, bash bool) {
	if corpID == 0 || sysID == 0 {
		return 0, 0, 0, false
	}
	side := append([]int64{corpID}, m.allianceMemberCorps(ctx, corpID)...)
	corps := int64CSV(side)
	for _, s := range m.openSieges(ctx, `systemID = ? AND (attackerCorpID IN (`+corps+`) OR defenderCorpID IN (`+corps+`))`, sysID) {
		if s.isStation() {
			// A station: warp to it, orbit it siegeStationHoldM off its hull (stationsiege.go). The
			// range is a surface distance; embodyOrbit adds the hull, so the radius is not added here
			// (it was, and the line circled 39 km out, beyond its own guns).
			if _, _, _, _, ok := m.stationBall(ctx, s.TowerID); !ok {
				continue
			}
			return s.TowerID, s.TowerID, siegeStationHoldM, slices.Contains(side, s.AttackerCorpID)
		}
		rec, ok, err := evedb.GetStarbase(ctx, m.db, s.TowerID)
		if err != nil || !ok || rec.MoonID == 0 {
			continue
		}
		// The orbit is a surface distance off the tower's hull (embodyOrbit); measured from the
		// field's radius it stays outside the field whether or not the hull is counted.
		rangeM = evedb.TowerShieldRadius(ctx, m.db, rec.TypeID) + siegeHoldMarginM
		return rec.MoonID, s.TowerID, rangeM, slices.Contains(side, s.AttackerCorpID)
	}
	return 0, 0, 0, false
}

// siegeBashing is the towers an abstract bash is grinding, by the unix second it last ground,
// written by advanceSiege and read by the grid tick (siegeBashLive), which must not query.
var (
	siegeBashMu  sync.Mutex
	siegeBashing = map[int64]int64{}
)

// siegeBashStaleSec is how long a bash mark stands without a fresh desk pass. A desk that stops
// running (the module stopped, the corporation lost its CEO) must not leave guns cycling forever.
const siegeBashStaleSec = 3600

// noteSiegeBash marks or clears a tower as under the abstract bash.
func noteSiegeBash(towerID int64, on bool, now int64) {
	siegeBashMu.Lock()
	defer siegeBashMu.Unlock()
	if on {
		siegeBashing[towerID] = now
	} else {
		delete(siegeBashing, towerID)
	}
}

// siegeBashLive reports whether a tower is under the abstract bash now.
func siegeBashLive(towerID, now int64) bool {
	siegeBashMu.Lock()
	defer siegeBashMu.Unlock()
	at, ok := siegeBashing[towerID]
	return ok && now-at <= siegeBashStaleSec
}

// siegeParked is the pilots a siege fleet has parked on its target (gangSiegeStep), by system and
// the unix second the park lapses. A parked pilot sits off a tower or a station, not on a belt, so
// the abstract belt encounter leaves it alone (exposedInSpace). Guarded by siegeBashMu.
var siegeParked = map[int64]siegeParkMark{}

type siegeParkMark struct{ sysID, until int64 }

// noteSiegeParked marks a pilot parked on a siege target in sysID until the given unix second.
func noteSiegeParked(charID, sysID, until int64) {
	siegeBashMu.Lock()
	siegeParked[charID] = siegeParkMark{sysID: sysID, until: until}
	siegeBashMu.Unlock()
}

// siegeParkedIn reports whether a pilot is parked on a siege target in sysID now.
func siegeParkedIn(charID, sysID, now int64) bool {
	siegeBashMu.Lock()
	defer siegeBashMu.Unlock()
	p, ok := siegeParked[charID]
	if ok && now > p.until {
		delete(siegeParked, charID)
		return false
	}
	return ok && p.sysID == sysID
}

// siegeHolding is the systems where a siege is bashing, with the attacking side's corporations
// and the unix second the desk last saw it. Written by advanceSiege and read by popProtected,
// which must not query: a pilot of that side in that system stays online until the bash ends.
var siegeHolding = map[int64]siegeHoldMark{}

type siegeHoldMark struct {
	corps []int64
	at    int64
}

// noteSiegeHolding marks a system's bash with its attacking side, or clears it when corps is nil.
func noteSiegeHolding(sysID int64, corps []int64, now int64) {
	siegeBashMu.Lock()
	defer siegeBashMu.Unlock()
	if corps == nil {
		delete(siegeHolding, sysID)
		return
	}
	siegeHolding[sysID] = siegeHoldMark{corps: corps, at: now}
}

// siegeHoldsPilot reports whether a pilot of corpID in sysID is part of a bash there. A mark older
// than siegeBashStaleSec has lapsed.
func siegeHoldsPilot(corpID, sysID, now int64) bool {
	siegeBashMu.Lock()
	defer siegeBashMu.Unlock()
	h, ok := siegeHolding[sysID]
	return ok && now-h.at <= siegeBashStaleSec && slices.Contains(h.corps, corpID)
}

// ── events ──────────────────────────────────────────────────────────────────

func (m *Mod) onTowerReinforced(ctx context.Context, ev evedb.StarbaseEvent, p *towerPlan) {
	rec, ok, _ := evedb.GetStarbase(ctx, m.db, ev.TowerID)
	if !ok {
		return
	}
	exit := evedb.Win32ToUnix(int64(rec.ReinforceUntil))
	if p != nil {
		p.ReinforcedAt = clock.Unix()
		_ = m.saveTowerPlan(ctx, p)
	}
	open := m.openSieges(ctx, `towerID = ?`, ev.TowerID)
	if len(open) > 0 {
		s := open[0]
		setStage(s, "timer", clock.Unix())
		s.ExitAt = exit
		m.saveSiege(ctx, s)
		return
	}
	// A member's tower under attack is the alliance's business too
	// (gangalliance.go): every bot member sends its security wing.
	m.allianceRaiseDefend(ctx, ev.OwnerCorpID, ev.TowerID, ev.SolarSystemID, exit)
	if _, managed := managedCEO(ev.OwnerCorpID); !managed {
		return
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_siege (towerID, systemID, attackerCorpID, defenderCorpID, exitAt, phase, openedAt, stageAt)
		VALUES (?, ?, 0, ?, ?, 'timer', ?, ?)`, ev.TowerID, ev.SolarSystemID, ev.OwnerCorpID, exit, clock.Unix(), clock.Unix())
	if c, _ := getCEO(ctx, m.db, ev.OwnerCorpID); c != nil {
		m.ceoLog(ctx, c, powerWar, "siege", ev.TowerID, 0,
			"its tower in %s was reinforced — the defence forms up two hours before the timer ends (%s)",
			systemNameOf(ctx, m, ev.SolarSystemID), fmtDuration(max(0, exit-clock.Unix())))
	}
}

func (m *Mod) onReinforceEnded(ctx context.Context, ev evedb.StarbaseEvent, p *towerPlan) {
	for _, s := range m.openSieges(ctx, `towerID = ?`, ev.TowerID) {
		if s.AttackerCorpID == 0 {
			m.closeSiege(ctx, s, "held", "the timer ran out with no bot attacker on grid")
			continue
		}
		setStage(s, "fight", clock.Unix())
		m.saveSiege(ctx, s)
	}
	if p != nil {
		p.ReinforcedAt = 0
		_ = m.saveTowerPlan(ctx, p)
	}
}

func (m *Mod) onTowerDestroyed(ctx context.Context, ev evedb.StarbaseEvent, _ *towerPlan) {
	for _, s := range m.openSieges(ctx, `towerID = ?`, ev.TowerID) {
		m.closeSiege(ctx, s, "killed", "the tower was destroyed")
	}
}

// ── the desk ────────────────────────────────────────────────────────────────

// siegeDesk runs a corporation's tower sieges, attacking and defending. Station sieges are the
// alliance's (allianceStationSiege).
func (m *Mod) siegeDesk(ctx context.Context, c *corpCEO, ch Charter, _ []*towerPlan) {
	m.maybeOpenSiege(ctx, c, ch)
	for _, s := range m.openSieges(ctx, `attackerCorpID = ? AND targetKind = 'tower'`, c.CorpID) {
		if err := m.advanceSiege(ctx, c, s); err != nil {
			m.logBot(c.CharID, logError, "ceo", "siege %d: %v", s.SiegeID, err)
		}
	}
	// A station siege this corporation leads advances on its desk too, not only on the alliance
	// executor's pass (allianceStationSiege), so a watched bash does not wait out that cadence.
	for _, s := range m.openSieges(ctx, `attackerCorpID = ? AND targetKind = 'station'`, c.CorpID) {
		if err := m.advanceStationSiege(ctx, c, s); err != nil {
			m.logBot(c.CharID, logError, "ceo", "station siege %d: %v", s.SiegeID, err)
		}
	}
}

// siegeReachJumps is how far from its HQ a corporation looks for a tower to besiege: the reach
// its call-to-arms fleet recruits from (gangRecruitMaxJumps).
const siegeReachJumps = gangRecruitMaxJumps

// siegeOpenScan caps the towers one opening pass sizes a fleet for, nearest first.
const siegeOpenScan = 8

// maybeOpenSiege picks an enemy tower to besiege: the nearest one whose quota (siegeQuota) the
// corporation and the mates that may join it (towerSiegeCorps) can field (siegeCapacityFor). One
// attacking siege per alliance at a time. It waits siegeRetrySec after a siege whose fleet never
// formed, on any tower.
func (m *Mod) maybeOpenSiege(ctx context.Context, c *corpCEO, ch Charter) {
	if !relCorpAtWar(c.CorpID) || len(m.openSieges(ctx, `attackerCorpID = ? AND targetKind = 'tower'`, c.CorpID)) >= siegeMaxOpen {
		return
	}
	enemies := relWarEnemiesOf(c.CorpID)
	if len(enemies) == 0 {
		return
	}
	if formBackedOff(c.CorpID, ActSecurity) > 0 && formBackedOff(c.CorpID, ActCombat) > 0 {
		return
	}
	if m.siegeSidesBusy(ctx, append([]int64{c.CorpID}, m.allianceMemberCorps(ctx, c.CorpID)...)) {
		return
	}
	now := clock.Unix()
	var lastFailed int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(closedAt), 0) FROM rolebot_corp_siege
		 WHERE attackerCorpID = ? AND result = 'abandoned' AND damage = 0 AND targetKind = 'tower'`, c.CorpID).Scan(&lastFailed)
	if lastFailed > 0 && now-lastFailed < siegeRetrySec {
		return
	}
	if m.siegeShortHold(ctx, []int64{c.CorpID}, now) {
		return
	}
	hq := m.stationSystemID(ctx, m.corpHQStation(ctx, c.CorpID))
	u, err := m.universe(ctx)
	if err != nil || u == nil || hq == 0 {
		return
	}
	reach := hopsWithin(u, hq, siegeReachJumps)
	// Towers this corporation gave up on or was beaten off lately are left alone for siegeRetrySec.
	resting := map[int64]bool{}
	if ids, err := int64Column(ctx, m, `
		SELECT towerID FROM rolebot_corp_siege
		 WHERE attackerCorpID = ? AND closedAt > ? AND result IN ('abandoned', 'held')`,
		c.CorpID, now-siegeRetrySec); err == nil {
		for _, id := range ids {
			resting[id] = true
		}
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.entityID, s.solarSystemID, s.ownerCorpID, COALESCE(e.typeID, 0) FROM starbase s
		  LEFT JOIN entity e ON e.itemID = s.entityID
		 WHERE s.state = 'online' AND s.ownerCorpID IN (`+int64CSV(enemies)+`)`)
	if err != nil {
		return
	}
	type target struct {
		tower, sys, owner, typeID int64
		j                         int
	}
	var ts []target
	for rows.Next() {
		var t target
		if rows.Scan(&t.tower, &t.sys, &t.owner, &t.typeID) == nil && !resting[t.tower] && t.typeID != 0 {
			if j, ok := reach[t.sys]; ok {
				t.j = j
				ts = append(ts, t)
			}
		}
	}
	rows.Close()
	if len(ts) == 0 {
		return
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].j < ts[j].j || (ts[i].j == ts[j].j && ts[i].tower < ts[j].tower) })
	// The nearest tower whose quota the side can field. A larger tower nearer by is passed over for
	// a smaller one it can break, which is how the bar comes down to what the wing has.
	var short struct {
		t           target
		have, quota float64
		ps          []siegePilot
		ok          bool
	}
	for i, t := range ts {
		if i >= siegeOpenScan {
			break
		}
		if len(m.openSieges(ctx, `towerID = ?`, t.tower)) > 0 {
			continue
		}
		q := siegeQuota(m.structureHPOf(ctx, t.typeID, true), siegeWindowSec, siegeQuotaMargin)
		capa, ps := m.siegeCapacityFor(ctx, m.towerSiegeCorps(ctx, c.CorpID, t.owner, u.meta[t.sys].Security))
		if capa.WithKits < q.OpenDPS() {
			if !short.ok {
				short.t, short.have, short.quota, short.ps, short.ok = t, capa.WithKits, q.OpenDPS(), ps, true
			}
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_siege (towerID, systemID, attackerCorpID, defenderCorpID, phase, openedAt, lastAt, stageAt,
			                                quotaDPS, regenEHP)
			VALUES (?, ?, ?, ?, 'muster', ?, ?, ?, ?, ?)`, t.tower, t.sys, c.CorpID, t.owner, now, now, now,
			q.OpenDPS(), q.Regen); err != nil {
			return
		}
		// A muster is the corporation's own plan: logged for the CEO, never announced. The siege is
		// announced when the fleet arrives (advanceSiege).
		m.logBot(c.CharID, logInfo, "ceo",
			"mustering the combat wing for a siege on %s's tower in %s (%d jumps out) — %s, the wing can field ~%.0f",
			corpNameOf(ctx, m.db, t.owner), systemNameOf(ctx, m, t.sys), t.j, siegeQuotaNote(q), capa.WithKits)
		return
	}
	if !short.ok {
		return
	}
	what := fmt.Sprintf("%s's tower in %s", corpNameOf(ctx, m.db, short.t.owner), systemNameOf(ctx, m, short.t.sys))
	// Short of the nearest tower's quota: hulls bought for the pilots without one may close it
	// (siegekits.go); the siege opens on a later pass once they are on the shelf.
	m.provisionSiegeKits(ctx, c, short.ps, short.quota-short.have, what)
	if now-m.lastCEOActionAt(ctx, c.CorpID, "siege.short") >= 86400 {
		m.ceoLog(ctx, c, powerWar, "siege.short", short.t.tower, short.quota, "%s",
			siegeShortLine(what, short.have, short.quota))
	}
}

// siegeAttackFloor is the fewest pilots a call-to-arms fleet may set out with: siegeMinAttackers
// when its call is for a siege its corporation or an alliance mate is attacking in a muster, bash,
// kill or fight stage (fewer never move the abstract bash, advanceSiege), else gangMinMembers.
func (m *Mod) siegeAttackFloor(ctx context.Context, g *Gang) int {
	if g.State.HelpCallID == 0 {
		return gangMinMembers
	}
	var sysID int64
	if err := m.db.QueryRowContext(ctx, `SELECT systemID FROM rolebot_fleet_help WHERE callID = ?`,
		g.State.HelpCallID).Scan(&sysID); err != nil || sysID == 0 {
		return gangMinMembers
	}
	corps := append([]int64{g.CorpID}, m.allianceMemberCorps(ctx, g.CorpID)...)
	if len(m.openSieges(ctx, `systemID = ? AND phase IN ('muster', 'bash', 'kill', 'fight') AND attackerCorpID IN (`+int64CSV(corps)+`)`,
		sysID)) == 0 {
		return gangMinMembers
	}
	return max(gangMinMembers, siegeMinAttackers)
}

// siegeSide is a corporation's armed combatants in space in a system. A logged-off pilot is not
// counted (popcap.go).
func (m *Mod) siegeSide(ctx context.Context, corpID, sysID int64, role string) []*pvpCombatant {
	if corpID == 0 {
		return nil
	}
	ids, _ := int64Column(ctx, m, `
		SELECT ch.characterID FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID AND b.enabled = 1
		 WHERE ch.corporationID = ? AND ch.solarSystemID = ? AND COALESCE(ch.stationID, 0) = 0
		 LIMIT 60`, corpID, sysID)
	var out []*pvpCombatant
	for _, id := range ids {
		if popIsOffline(id) {
			continue
		}
		if pc := m.pvpCombatantFor(ctx, id, role); pc != nil && pc.Armed {
			out = append(out, pc)
		}
	}
	return out
}

// siegeAttackers is the attacking side's siege fleet in the system: armed, in space there, and
// flying in a fleet whose orders are this system (siegeFleetPilots). A corporation member who is
// only passing through, running a mission or roaming is not part of the bash.
func (m *Mod) siegeAttackers(ctx context.Context, corpID, sysID int64) []*pvpCombatant {
	side := append([]int64{corpID}, m.allianceMemberCorps(ctx, corpID)...)
	fleet := m.siegeFleetPilots(ctx, side, sysID)
	var out []*pvpCombatant
	for _, a := range m.siegeSideOwner(ctx, corpID, sysID, "dps") {
		if fleet[a.CharID] {
			out = append(out, a)
		}
	}
	return out
}

// siegePresent drops the pilots a watcher could not see. In a projected system a pilot counts only
// as a live projection there, on the tower's grid when tower is set (siegeOnGrid). Off-grid, the
// list is returned as it is.
func siegePresent(side []*pvpCombatant, sysID int64, projected bool, tower *destiny.Entity) []*pvpCombatant {
	if !projected {
		return side
	}
	var out []*pvpCombatant
	for _, p := range side {
		if siegeOnGrid(p.CharID, sysID, tower) {
			out = append(out, p)
		}
	}
	return out
}

// siegeFleetPilots is the pilots of an enabled fleet of the given corporations whose orders are
// sysID: raised for or answering a call to arms there (HelpCallID), or posted there
// (OrderSystemID).
func (m *Mod) siegeFleetPilots(ctx context.Context, corps []int64, sysID int64) map[int64]bool {
	out := map[int64]bool{}
	if len(corps) == 0 || sysID == 0 {
		return out
	}
	calls := map[int64]bool{}
	if ids, err := int64Column(ctx, m, `
		SELECT callID FROM rolebot_fleet_help WHERE systemID = ? AND kind = ? AND postedAt > ?`,
		sysID, helpCallToArms, clock.Unix()-siegeMaxAgeSec); err == nil {
		for _, id := range ids {
			calls[id] = true
		}
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT gm.characterID, g.state FROM rolebot_gang_members gm
		  JOIN rolebot_gangs g ON g.gangID = gm.gangID AND g.enabled = 1
		 WHERE g.corpID IN (`+int64CSV(corps)+`)`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var charID int64
		var state sql.NullString
		if rows.Scan(&charID, &state) != nil {
			continue
		}
		var gs GangState
		if state.Valid {
			_ = json.Unmarshal([]byte(state.String), &gs)
		}
		if siegeFleetBound(&gs, sysID, calls) {
			out[charID] = true
		}
	}
	return out
}

// siegeFleetBound reports whether a fleet's orders are sysID: a posting there, or a call to arms
// posted for it (calls). Pure.
func siegeFleetBound(gs *GangState, sysID int64, calls map[int64]bool) bool {
	if gs == nil || sysID == 0 {
		return false
	}
	return gs.OrderSystemID == sysID || (gs.HelpCallID != 0 && calls[gs.HelpCallID])
}

// siegePost is the tower a fleet's orders put it at: the siege's system and tower, and whether
// the fleet's side is the attacker.
type siegePost struct {
	SysID, TowerID int64
	Attack         bool
}

// siegePostStage reports whether a fleet of one side holds the tower in a siege stage. The
// attacker holds it from the muster on: its fleet gathers on the tower and the bash counts it
// there. The defender holds it once the tower is under fire or the exit fight is on; a muster is
// the attacker's secret. Pure.
func siegePostStage(stage string, attack bool) bool {
	switch stage {
	case "bash", "kill", "fight":
		return true
	case "muster":
		return attack
	}
	return false
}

// gangSiegePost is the siege a fleet's orders put it at: an open siege in the system the fleet is
// posted to (OrderSystemID) or called to (HelpCallID), on a side the fleet's corporation or an
// alliance mate holds, in a stage that side holds the tower (siegePostStage). Zero when none.
func (m *Mod) gangSiegePost(ctx context.Context, g *Gang) siegePost {
	if g == nil || g.CorpID == 0 {
		return siegePost{}
	}
	sysID := g.State.OrderSystemID
	if sysID == 0 && g.State.HelpCallID != 0 {
		_ = m.db.QueryRowContext(ctx, `SELECT systemID FROM rolebot_fleet_help WHERE callID = ? AND kind = ?`,
			g.State.HelpCallID, helpCallToArms).Scan(&sysID)
	}
	if sysID == 0 {
		return siegePost{}
	}
	side := append([]int64{g.CorpID}, m.allianceMemberCorps(ctx, g.CorpID)...)
	corps := int64CSV(side)
	for _, s := range m.openSieges(ctx, `systemID = ? AND (attackerCorpID IN (`+corps+`) OR defenderCorpID IN (`+corps+`))`, sysID) {
		attack := slices.Contains(side, s.AttackerCorpID)
		if siegePostStage(s.Stage, attack) {
			return siegePost{SysID: s.SystemID, TowerID: s.TowerID, Attack: attack}
		}
	}
	return siegePost{}
}

// gangPlanSiege sends a fleet member to the tower its orders hold (gangSiegePost) in place of a
// patrol or hunt site. phase is the member's working phase, which gangSiegeStep runs under.
func (m *Mod) gangPlanSiege(ctx context.Context, b *botRun, g *Gang, post siegePost, phase string) error {
	m.fleetHasWork(ctx, g)
	b.State.Data = gangDutyData(b.State.Data, g)
	verb := "holding the line at"
	if !post.Attack {
		verb = "defending"
	}
	m.logBot(b.CharID, logInfo, "gang", "with %q — %s the tower in %s", g.Name, verb, m.nameOfSystem(ctx, post.SysID))
	return m.beginTravel(ctx, b, post.SysID, 0, phase)
}

// gangSiegeStep keeps a fleet member at the tower its orders hold: it travels to the siege's
// system, then sits on the tower's grid outside the force field (parkAtSiege), so the off-grid
// bash counts a pilot that is where a watcher would find it and a projection is born there. A
// projected member is flown by the grid (fleetHoldTick), not parked.
func (m *Mod) gangSiegeStep(ctx context.Context, b *botRun, post siegePost, phase string) error {
	st := b.State
	if b.SystemID != post.SysID {
		return m.beginTravel(ctx, b, post.SysID, 0, phase)
	}
	if b.ShipID == 0 {
		return fmt.Errorf("siege fleet member has no ship boarded")
	}
	m.parkAtSiege(ctx, b, post.TowerID)
	st.Data.LegEndsAt = clock.Unix() +
		int64(gangPatrolDwellMinSec+rand.Intn(gangPatrolDwellMaxSec-gangPatrolDwellMinSec+1))
	// Off the belts until the next step re-parks it: the patrol task is exposed to belt rats, a
	// pilot holding a tower or a station is not (exposedInSpace).
	noteSiegeParked(b.CharID, post.SysID, st.Data.LegEndsAt+600)
	return saveState(ctx, m.db, st)
}

// parkAtSiege puts an unprojected ship on a tower's grid, off its force field by the hold margin,
// where the fleet's hold orbits it (siegeHoldFor).
func (m *Mod) parkAtSiege(ctx context.Context, b *botRun, towerID int64) {
	if b.ShipID == 0 || m.isEmbodied(b.CharID) {
		return
	}
	if x, y, z, radius, ok := m.stationBall(ctx, towerID); ok {
		// A station siege: the line sits off the station's hull.
		m.persistPosition(ctx, b.ShipID, towerID, x, y, z, max(radius+siegeHoldMarginM-parkStandoff, 0))
		return
	}
	rec, ok, err := evedb.GetStarbase(ctx, m.db, towerID)
	if err != nil || !ok {
		return
	}
	var x, y, z float64
	if err := m.db.QueryRowContext(ctx, `SELECT x, y, z FROM entity WHERE itemID = ?`, towerID).Scan(&x, &y, &z); err != nil ||
		(x == 0 && y == 0 && z == 0) {
		return
	}
	// persistPosition adds parkStandoff off the radius it is given; the line sits at the hold margin.
	radius := max(evedb.TowerShieldRadius(ctx, m.db, rec.TypeID)+siegeHoldMarginM-parkStandoff, 0)
	m.persistPosition(ctx, b.ShipID, towerID, x, y, z, radius)
}

// layerRes is the tower's resonance per layer for the bash: the type's own, or the old flat
// siegeTowerResonance when a layer was never read. Pure.
func (hp siegeHP) layerRes() (s, a, h float64) {
	return resOr(hp.SRes, siegeTowerResonance), resOr(hp.ARes, siegeTowerResonance), resOr(hp.HRes, siegeTowerResonance)
}

// ehp is what the bash must deal: to the reinforcement floor (the shield above it), and to kill
// (the rest of the shield, armor and hull), each layer through its own resonance. Pure.
func (hp siegeHP) ehp() (toFloor, toKill float64) {
	sr, ar, hr := hp.layerRes()
	toFloor = hp.Shield * (1 - hp.Floor) / sr
	toKill = hp.Shield*hp.Floor/sr + hp.Armor/ar + hp.Hull/hr
	return toFloor, toKill
}

// killShieldEHP is the part of the kill stage that is still shield, where regeneration works. Pure.
func (hp siegeHP) killShieldEHP() float64 {
	sr, _, _ := hp.layerRes()
	return hp.Shield * hp.Floor / sr
}

// siegeFractions is the tower's remaining shield, armor and hull, as fractions of max, after a
// stage's bash damage (EHP). A bash stops at the floor; a kill takes the rest of the shield, then
// the armor, then the hull, each through its own resonance. Pure.
func siegeFractions(stage string, damage float64, hp siegeHP) (shield, armor, hull float64) {
	sr, ar, hr := hp.layerRes()
	damage = max(damage, 0)
	frac := func(left, total float64) float64 {
		if total <= 0 {
			return 1
		}
		return min(max(left/total, 0), 1)
	}
	if stage != "kill" {
		return frac(max(hp.Shield-damage*sr, hp.Shield*hp.Floor), hp.Shield), 1, 1
	}
	s, a, h := hp.Shield*hp.Floor, hp.Armor, hp.Hull
	if left := damage - s/sr; left <= 0 {
		s -= damage * sr
	} else {
		s = 0
		if left2 := left - a/ar; left2 <= 0 {
			a -= left * ar
		} else {
			a = 0
			h -= left2 * hr
		}
	}
	return frac(s, hp.Shield), frac(a, hp.Armor), frac(h, hp.Hull)
}

// siegeDamageOf is the stage's bash damage (EHP) that leaves a tower at the given fractions, the
// reverse of siegeFractions. Pure.
func siegeDamageOf(stage string, shield, armor, hull float64, hp siegeHP) float64 {
	sr, ar, hr := hp.layerRes()
	clamp := func(v float64) float64 { return min(max(v, 0), 1) }
	shield, armor, hull = clamp(shield), clamp(armor), clamp(hull)
	if stage != "kill" {
		return max((1-shield)*hp.Shield/sr, 0)
	}
	return max((hp.Floor-shield)*hp.Shield/sr, 0) + (1-armor)*hp.Armor/ar + (1-hull)*hp.Hull/hr
}

// siegeTowerEntity is the sieged tower's entity when its system is loaded, else nil.
func (m *Mod) siegeTowerEntity(s *siegeRow) (*destiny.SolarSystem, *destiny.Entity) {
	if m.systems == nil {
		return nil, nil
	}
	ss := m.systems.Get(s.SystemID)
	if ss == nil {
		return nil, nil
	}
	if s.isStation() {
		return ss, ss.GetStationEntity(uint32(s.TowerID))
	}
	e := ss.StructureEntity(uint32(s.TowerID))
	if e == nil || e.IsDestroyed() {
		return ss, nil
	}
	return ss, e
}

// syncSiegeTower lands a bash pass's added damage on the tower. A loaded tower takes it on its
// entity, through the core's structure rules, so witnesses see its hit points fall. An unloaded
// tower gets its saved damage row rewritten, which the entity reads when the system loads. It
// reports whether the hit destroyed a loaded tower.
func (m *Mod) syncSiegeTower(ctx context.Context, s *siegeRow, ss *destiny.SolarSystem, tower *destiny.Entity,
	hp siegeHP, added float64, now int64) bool {

	if tower != nil {
		if added <= 0 || tower.IsInvulnerable() {
			return false
		}
		quarter := added / 4
		applied, destroyed := tower.ApplyDamage(quarter, quarter, quarter, quarter)
		if hit := modapi.PvP().StructureHit; hit != nil {
			hit(ss, tower, nil, 0, applied, destroyed)
		}
		if moved := modapi.PvP().HPChanged; moved != nil && !destroyed {
			moved(ss, tower)
		}
		return destroyed
	}
	if s.Damage <= 0 && added >= 0 {
		return false
	}
	sh, ar, hu := siegeFractions(s.Stage, s.Damage, hp)
	if err := evedb.SaveStructureDamage(ctx, m.db, []evedb.StructureDamageRow{{
		EntityID: s.TowerID, Shield: sh, Armor: ar, Hull: hu, SavedAt: uint64(evedb.UnixToWin32(now)),
	}}); err != nil {
		rlogf("rolebots: siege %d tower damage: %v", s.SiegeID, err)
	}
	return false
}

// siegeBashNext is what one bash tick did to the tower.
type siegeBashNext int

const (
	siegeBashHolds siegeBashNext = iota // still taking fire
	siegeBashFloor                      // the shield reached the reinforcement floor
	siegeBashKill                       // the tower's last hitpoints are gone
)

// siegeBashStep adds elapsed seconds of the attackers' dps to a bash or kill stage, net of the
// shield's regeneration while there is shield to regenerate (the whole bash; the kill stage's
// shield part, killShield), and reports whether the tower reached its floor or died. Standing
// damage never falls below 0. regen is 0 for a loaded tower, whose entity regenerates itself.
// Pure.
func siegeBashStep(stage string, damage, dps, regen, elapsed, toFloor, toKill, killShield float64) (float64, siegeBashNext) {
	if stage == "bash" || (stage == "kill" && damage < killShield) {
		damage = max(0, damage+(dps-regen)*elapsed)
	} else {
		damage += dps * elapsed
	}
	switch {
	case stage == "bash" && damage >= toFloor:
		return damage, siegeBashFloor
	case stage == "kill" && damage >= toKill:
		return damage, siegeBashKill
	}
	return damage, siegeBashHolds
}

// siegeFightOnGrid reports whether the fight at a reinforcement exit belongs to the grid: the
// system is projected, so both fleets have balls and shoot for real, and defenders are still in
// space. The abstract team fight would kill ships that are still flying there. Pure.
func siegeFightOnGrid(projected bool, defenders int) bool {
	return projected && defenders > 0
}

// advanceSiege moves one attacking siege along.
func (m *Mod) advanceSiege(ctx context.Context, c *corpCEO, s *siegeRow) error {
	now := clock.Unix()
	// The grid's siege seats show fire while this pass ground the tower (siegeBashLive).
	// holding: the siege fleet has pilots in the system, so its seats are built first and kept
	// online (siegeHoldsPilot) while it gathers on the tower, not only once the bash is live.
	bashing, holding := false, false
	defer func() {
		// On a watched grid the guns stay live while the fleet holds the system: the desk's count of
		// pilots on grid must not switch off the very fire that makes them count (siegeBashLive).
		live := (bashing || holding) && (s.Stage == "bash" || s.Stage == "kill")
		noteSiegeBash(s.TowerID, live, now)
		var side []int64
		if live || (holding && s.Stage != "done") {
			side = append([]int64{c.CorpID}, m.allianceMemberCorps(ctx, c.CorpID)...)
		}
		noteSiegeHolding(s.SystemID, side, now)
	}()
	rec, ok, err := evedb.GetStarbase(ctx, m.db, s.TowerID)
	if err != nil {
		return err
	}
	if !ok {
		m.closeSiege(ctx, s, "gone", "the tower is no longer there")
		return nil
	}
	if !relCorpAtWar(c.CorpID) {
		m.closeSiege(ctx, s, "abandoned", "the war is over")
		return nil
	}
	// A stage that has not moved in siegeStageMaxSec ends the siege, watched system or not: a
	// player parked in the system would otherwise hold it open for good.
	if siegeStalled(s, now) {
		start := s.StageAt
		if start == 0 {
			start = s.OpenedAt
		}
		m.closeSiege(ctx, s, "abandoned", fmt.Sprintf("the siege stalled — %dh in %s without breaking the tower",
			(now-start)/3600, s.Stage))
		return nil
	}
	// Pilots on grid can shoot a loaded tower into reinforcement for real (posreinforce.go); the
	// row follows the tower.
	if rec.State == evedb.StarbaseStateReinforced && s.Stage == "bash" {
		setStage(s, "timer", now)
		s.ExitAt, s.Damage = evedb.Win32ToUnix(int64(rec.ReinforceUntil)), 0
	}
	// Attackers shooting the tower on a watched grid deal their own damage (siege_embodied.go);
	// this pass adds the damage of the attackers who are not, and keeps the tower's hit points in
	// step with the bash (syncSiegeTower). The ship fight is the grid's when the system is
	// projected (siegeFightOnGrid).
	gridFight := m.systemProjected(s.SystemID)
	elapsed := float64(min(now-s.LastAt, siegeMaxElapsedSec))
	s.LastAt = now
	switch s.Stage {
	case "muster", "bash", "kill":
		ss, tower := m.siegeTowerEntity(s)
		fleet := m.siegeAttackers(ctx, c.CorpID, s.SystemID)
		att := siegePresent(fleet, s.SystemID, gridFight, tower)
		// Seat priority for the fleet still flying in: anyone of it in the system, on grid or not.
		holding = gridFight && len(fleet) > 0
		hp := m.structureHPOf(ctx, rec.TypeID, true)
		q := siegeQuota(hp, siegeWindowSec, siegeQuotaMargin)
		s.FleetDPS = siegeFleetDPS(att)
		// The stage reads the fleet's DPS against the regeneration it must out-shoot: the whole bash,
		// and the kill stage while there is shield left in it.
		stageRegen := q.Regen
		if s.Stage == "kill" && s.Damage >= hp.killShieldEHP() {
			stageRegen = 0
		}
		was := s.Stage
		next := siegeStageNext(siegeStageIn{Stage: s.Stage, StageAt: s.StageAt, OpenedAt: s.OpenedAt, Now: now,
			LowSince: s.LowSince, FleetLive: m.siegeFleetLive(ctx, s), FleetDPS: s.FleetDPS, Regen: stageRegen,
			Window: siegeWindowSec})
		s.LowSince = next.LowSince
		if next.Abandon != "" {
			// A muster that never formed is short, not a failed bash: the side retries on the
			// short backoff (siegeShortHold) and the tower is not rested.
			m.closeSiege(ctx, s, siegeAbandonResult(was), next.Abandon)
			return nil
		}
		setStage(s, next.Stage, now)
		if was == "muster" && s.Stage == "bash" && s.Damage == 0 {
			m.ceoLog(ctx, c, powerWar, "siege", s.TowerID, s.FleetDPS,
				"laid siege to %s's tower in %s — %.0f DPS on it against %.0f/s of shield regeneration",
				corpNameOf(ctx, m.db, s.DefenderCorpID), systemNameOf(ctx, m, s.SystemID), s.FleetDPS, q.Regen)
		}
		if s.Stage == "muster" {
			m.saveSiege(ctx, s)
			return nil
		}
		bashing = true
		if tower != nil {
			// The tower is loaded: its entity holds the true hit points, grid hits included.
			d := tower.DamageStateFrac()
			s.Damage = siegeDamageOf(s.Stage, d.Shield, d.Armor, d.Structure, hp)
		}
		// On a projected system the damage is the grid's: only guns cycling on the tower move its
		// hit points (siegeFire). The desk adds nothing a watcher would not see fired.
		dps := 0.0
		for _, a := range att {
			if !gridFight && !siegeGridShooting(a.CharID, now) {
				dps += a.DPS
			}
		}
		toFloor, toKill := hp.ehp()
		// A loaded tower regenerates on its own entity; the desk nets the regeneration out of an
		// unloaded one's bash.
		bashRegen := q.Regen
		if tower != nil {
			bashRegen = 0
		}
		before := s.Damage
		var step siegeBashNext
		s.Damage, step = siegeBashStep(s.Stage, s.Damage, dps, bashRegen, elapsed, toFloor, toKill, hp.killShieldEHP())
		if destroyed := m.syncSiegeTower(ctx, s, ss, tower, hp, s.Damage-before, now); destroyed {
			return nil // the tower's death closes the siege (onTowerDestroyed)
		}
		switch step {
		case siegeBashFloor:
			hours, until, err := evedb.HeadlessReinforce(ctx, m.db, s.TowerID, nil)
			if err != nil && !errors.Is(err, evedb.ErrTowerNotOnline) {
				return err
			}
			if hours > 0 {
				setStage(s, "timer", now)
				s.ExitAt, s.Damage = evedb.Win32ToUnix(int64(until)), 0
				m.ceoLog(ctx, c, powerWar, "siege", s.TowerID, 0,
					"shot the tower in %s into reinforcement — %d hour(s) of strontium", systemNameOf(ctx, m, s.SystemID), hours)
				m.noteSiegeIncident(ctx, s, incSiege, 0)
			} else {
				setStage(s, "kill", now) // no strontium: it keeps taking fire
				s.Damage = 0
			}
		case siegeBashKill:
			m.killSiegedTower(ctx, c, s)
			return nil
		}
		m.saveSiege(ctx, s)
	case "timer":
		if now < s.ExitAt {
			m.saveSiege(ctx, s)
			return nil
		}
		setStage(s, "fight", now)
		fallthrough
	case "fight":
		att := siegePresent(m.siegeAttackers(ctx, c.CorpID, s.SystemID), s.SystemID, gridFight, nil)
		def := siegePresent(m.siegeSideOwner(ctx, s.DefenderCorpID, s.SystemID, "dps"), s.SystemID, gridFight, nil)
		if len(att) == 0 {
			m.closeSiege(ctx, s, "held", "no attacker was on grid at the exit")
			return nil
		}
		if siegeFightOnGrid(gridFight, len(def)) {
			// The projections fight it out with real shots; the row moves on once the defence
			// is gone from space.
			m.saveSiege(ctx, s)
			return nil
		}
		if len(def) > 0 {
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
				// Neither side broke: the defence still stands on grid.
				m.saveSiege(ctx, s)
				return nil
			}
		}
		// The field is the attackers': finish the tower.
		setStage(s, "kill", now)
		s.Damage = 0
		m.saveSiege(ctx, s)
	}
	return nil
}

// killSiegedTower destroys the tower off-grid and closes the siege.
func (m *Mod) killSiegedTower(ctx context.Context, c *corpCEO, s *siegeRow) {
	canID, err := evedb.DestroyStarbaseFor(ctx, m.db, s.TowerID, nil)
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "destroying the tower in %s: %v", systemNameOf(ctx, m, s.SystemID), err)
		return
	}
	note := "destroyed it"
	if canID != 0 {
		note += " — its bays spilled into a container"
	}
	m.closeSiege(ctx, s, "killed", note)
	m.noteSiegeIncident(ctx, s, incTowerKill, 0)
}
