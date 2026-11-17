package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
)

// A siege's own fleet: one alliance fleet raised for it, linked by rolebot_corp_siege.gangID and
// GangState.SiegeID. Its lead corporation's call to arms raises it (formCallToArms); it musters
// forward of the target (forwardStaging), recruits from every corporation the siege may draw on
// (siegeRecruitCorps), pulls the alliance's patrols in (pullPatrolsForSiege), and lives as long as
// the siege does (leaseSiegeGangs). The siege's stages read whether it is out (siegeFleetLive).

const (
	// siegeStagingJumps is how far from the target a siege fleet may stage.
	siegeStagingJumps = 5
	// siegeLeaseSec is how far ahead a siege fleet's expiry is pushed on each sweep while its siege
	// stands; siegeIssueGraceSec is how long after that a kit lent to it is due back.
	siegeLeaseSec      = 3600
	siegeIssueGraceSec = 6 * 3600
	// siegeMusterMinSec / siegeMusterMaxSec bound a siege fleet's muster: long enough for the
	// alliance to fly to a forward staging, short enough that a muster that will not fill ends.
	siegeMusterMinSec = 20 * 60
	siegeMusterMaxSec = 60 * 60
	// siegeLogiPer is the siege muster doctrine's one logistics seat per this many guns.
	siegeLogiPer = 5
)

// siegeFleetLive reports whether a siege's fleet is out: its gang enabled and past its muster. A
// siege with no fleet of its own (an older row, or one whose fleet formed outside the link) counts
// as live, so the attackers present decide, as before. A linked gang that is gone is not live.
func (m *Mod) siegeFleetLive(ctx context.Context, s *siegeRow) bool {
	if s == nil || s.GangID == 0 {
		return true
	}
	var state sql.NullString
	if err := m.db.QueryRowContext(ctx,
		`SELECT state FROM rolebot_gangs WHERE gangID = ? AND enabled = 1`, s.GangID).Scan(&state); err != nil {
		return false
	}
	var gs GangState
	if state.Valid && state.String != "" {
		_ = json.Unmarshal([]byte(state.String), &gs)
	}
	return gs.OpPhase == "" || gs.OpPhase == gangOpLive
}

// siegeGangAlive reports whether a siege's linked fleet still exists.
func (m *Mod) siegeGangAlive(ctx context.Context, s *siegeRow) bool {
	if s == nil || s.GangID == 0 {
		return false
	}
	var one int
	return m.db.QueryRowContext(ctx, `SELECT 1 FROM rolebot_gangs WHERE gangID = ? AND enabled = 1`, s.GangID).
		Scan(&one) == nil
}

// siegeForCall is the open attacking siege a call to arms by corpID in sysID is raised for, or nil.
func (m *Mod) siegeForCall(ctx context.Context, corpID, sysID int64) *siegeRow {
	for _, s := range m.openSieges(ctx, `systemID = ? AND attackerCorpID = ?`, sysID, corpID) {
		switch s.Stage {
		case "muster", "bash", "kill", "fight":
			return s
		}
	}
	return nil
}

// siegeRecruitCorps is every corporation a siege's fleet draws on: the whole alliance for a
// station, the lead and the mates mateJoinsTowerSiege lets in for a tower.
func (m *Mod) siegeRecruitCorps(ctx context.Context, s *siegeRow) []int64 {
	if s.isStation() {
		return append([]int64{s.AttackerCorpID}, m.allianceMemberCorps(ctx, s.AttackerCorpID)...)
	}
	sec := 0.0
	if u, err := m.universe(ctx); err == nil && u != nil {
		sec = u.meta[s.SystemID].Security
	}
	return m.towerSiegeCorps(ctx, s.AttackerCorpID, s.DefenderCorpID, sec)
}

// siegeLedByMate reports whether corpID's siege order in sysID is an alliance mate's attack, not
// its own: the mate's single alliance fleet takes the wing, so corpID raises no fleet or call of its
// own there. A siege corpID defends is its own business.
func (m *Mod) siegeLedByMate(ctx context.Context, corpID, sysID int64) bool {
	mates := m.allianceMemberCorps(ctx, corpID)
	if len(mates) == 0 {
		return false
	}
	own := m.openSieges(ctx, `systemID = ? AND (attackerCorpID = ? OR defenderCorpID = ?)`, sysID, corpID, corpID)
	if len(own) > 0 {
		return false
	}
	return len(m.openSieges(ctx, `systemID = ? AND attackerCorpID IN (`+int64CSV(mates)+`)`, sysID)) > 0
}

// ── forward staging ────────────────────────────────────────────────────────

// stagingCand is a station a siege fleet could muster at.
type stagingCand struct {
	StationID int64
	Jumps     int
	Own       bool // a member corporation owns it
	Office    bool // a member corporation rents an office in it
}

// pickForwardStaging is the station a siege fleet musters at: the fewest jumps from the target
// within maxJumps; on a tie a member's own station, then one with a member's office, then the
// lower stationID. Pure.
func pickForwardStaging(cands []stagingCand, maxJumps int) (int64, bool) {
	best := -1
	rank := func(c stagingCand) int {
		switch {
		case c.Own:
			return 0
		case c.Office:
			return 1
		}
		return 2
	}
	for i, c := range cands {
		if c.Jumps < 0 || c.Jumps > maxJumps {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		b := cands[best]
		switch {
		case c.Jumps != b.Jumps:
			if c.Jumps < b.Jumps {
				best = i
			}
		case rank(c) != rank(b):
			if rank(c) < rank(b) {
				best = i
			}
		case c.StationID < b.StationID:
			best = i
		}
	}
	if best < 0 {
		return 0, false
	}
	return cands[best].StationID, true
}

// forwardStaging is the station a siege's fleet musters at (pickForwardStaging): a member's own
// station, one a member rents an office in, or an NPC station, within siegeStagingJumps of the
// target. Never the target itself, nor one held by the defender, a war enemy, or a player outfit
// that is not blue. 0 when there is none (the commander's home stands in).
func (m *Mod) forwardStaging(ctx context.Context, s *siegeRow, corps []int64) int64 {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return 0
	}
	near := bfsJumps(u, s.SystemID, siegeStagingJumps)
	if len(near) == 0 {
		return 0
	}
	offices := map[int64]bool{}
	if len(corps) > 0 {
		if ids, err := int64Column(ctx, m, `
			SELECT DISTINCT stationID FROM rolebot_corp_offices
			 WHERE corporationID IN (`+int64CSV(corps)+`) AND state = 'open'`); err == nil {
			for _, id := range ids {
				offices[id] = true
			}
		}
	}
	enemies := relWarEnemiesOf(s.AttackerCorpID)
	rows, err := m.db.QueryContext(ctx, `
		SELECT st.stationID, st.solarSystemID, st.corporationID,
		       EXISTS (SELECT 1 FROM crpNPCCorporations n WHERE n.corporationID = st.corporationID)
		  FROM staStations st
		 WHERE st.solarSystemID IN (`+int64CSV(mapKeys(near))+`)`)
	if err != nil {
		return 0
	}
	defer rows.Close()
	lead := s.AttackerCorpID
	leadAlliance := m.corpAllianceID(ctx, lead)
	var cands []stagingCand
	for rows.Next() {
		var stationID, sysID, owner int64
		var npc bool
		if rows.Scan(&stationID, &sysID, &owner, &npc) != nil {
			continue
		}
		if stationID == s.TowerID || owner == s.DefenderCorpID || slices.Contains(enemies, owner) {
			continue
		}
		own := slices.Contains(corps, owner)
		if !own && !npc && !relDeclaredBlue(lead, leadAlliance, owner, m.corpAllianceID(ctx, owner)) {
			continue
		}
		cands = append(cands, stagingCand{StationID: stationID, Jumps: near[sysID], Own: own, Office: offices[stationID]})
	}
	id, _ := pickForwardStaging(cands, siegeStagingJumps)
	return id
}

// siegeMusterDeadline is a siege fleet's muster window for a recruit spread that needs window
// seconds to gather, held between siegeMusterMinSec and siegeMusterMaxSec. Pure.
func siegeMusterDeadline(window int64) int64 {
	return min(max(window, siegeMusterMinSec), siegeMusterMaxSec)
}

// ── commanders ─────────────────────────────────────────────────────────────

// orderSiegeCommanders puts a siege's commander candidates in the order to try: a corporation's
// CEO last (it has a desk to run and wanders off the grid it was meant to hold), then nearest
// the staging system. Stable otherwise.
func (m *Mod) orderSiegeCommanders(ctx context.Context, seats []fcSeat, stagingSys int64) []fcSeat {
	if len(seats) < 2 {
		return seats
	}
	u, _ := m.universe(ctx)
	var near map[int64]int
	if u != nil && stagingSys != 0 {
		near = bfsJumps(u, stagingSys, 40)
	}
	type keyed struct {
		s    fcSeat
		ceo  bool
		dist int
	}
	ks := make([]keyed, len(seats))
	for i, s := range seats {
		var ceoID int64
		_ = m.db.QueryRowContext(ctx, `SELECT ceoID FROM corporation WHERE corporationID = ?`, s.corpID).Scan(&ceoID)
		dist := 1 << 20
		if near != nil {
			var sysID int64
			_ = m.db.QueryRowContext(ctx, `SELECT solarSystemID FROM character_ WHERE characterID = ?`, s.charID).Scan(&sysID)
			if j, ok := near[sysID]; ok {
				dist = j
			}
		}
		ks[i] = keyed{s: s, ceo: ceoID == s.charID, dist: dist}
	}
	sort.SliceStable(ks, func(i, j int) bool {
		if ks[i].ceo != ks[j].ceo {
			return !ks[i].ceo
		}
		return ks[i].dist < ks[j].dist
	})
	out := make([]fcSeat, len(ks))
	for i, k := range ks {
		out[i] = k.s
	}
	return out
}

// ── the siege muster doctrine ──────────────────────────────────────────────

// siegeMusterDoctrine is the line a siege fleet flies: whatever combat hull a pilot owns or is
// lent, frigate to battleship, holding on the target; no tackle or EWAR (a structure neither runs
// nor locks back), one logistics seat per siegeLogiPer guns. Built at formation and never stored,
// so no corporation needs a battlecruiser band to raise one. Pure.
func siegeMusterDoctrine(corpID int64, tank TankPref, ceiling float64) *FleetDoctrine {
	return &FleetDoctrine{
		CorpID: corpID, Kind: kindSiege, Tank: tank, Style: stanceHold,
		HullMin: ClassFrigate, HullMax: ClassBattleship, CostCeiling: ceiling,
		Seats: []seatSpec{
			{Role: RoleLogi, Job: JobLogi, HullMin: ClassFrigate, HullMax: ClassBattleship, Tank: tank,
				Style: stanceHold, Per: siegeLogiPer, Priority: 0},
			{HullMin: ClassFrigate, HullMax: ClassBattleship, Tank: tank, Style: stanceHold, Priority: 3},
		},
	}
}

// excessSpecialists is the indexes of the role seats beyond what the line's guns call for, one
// per per guns (the leader counted among the guns) and never fewer than one: the last ones picked
// first. Pure.
func excessSpecialists(roles []string, role string, per int) []int {
	if per <= 0 {
		return nil
	}
	guns, have := 1, 0
	for _, r := range roles {
		switch r {
		case role:
			have++
		case RoleTackler, RoleEwar, RoleScout, RoleLogi:
		default:
			guns++
		}
	}
	allowed := max(1, guns/per)
	if have <= allowed {
		return nil
	}
	var out []int
	for i := len(roles) - 1; i >= 0 && have > allowed; i-- {
		if roles[i] == role {
			out = append(out, i)
			have--
		}
	}
	return out
}

// seatFillOrderFor is seatFillOrder, or for a siege line the guns first: a siege is won on DPS,
// so the gun seats take the pick of the pool and the logistics seats what is left.
func seatFillOrderFor(slots []gangSlot, gunsFirst bool) []int {
	if !gunsFirst {
		return seatFillOrder(slots)
	}
	out := seatFillOrder(slots)
	sort.SliceStable(out, func(a, b int) bool {
		ga := slots[out[a]].job == JobCombat || slots[out[a]].job == JobPiracy
		gb := slots[out[b]].job == JobCombat || slots[out[b]].job == JobPiracy
		return ga && !gb
	})
	return out
}

// ── the lease ──────────────────────────────────────────────────────────────

// siegeGangLease is what the sweep does with a siege fleet: keep it out while its siege stands in
// a muster, bash, kill or fight; disband it once the siege is closed or waits out a reinforcement
// timer (the exit's call to arms raises a fresh one two hours before). Pure.
func siegeGangLease(stage string, open bool) (extend, disband bool) {
	if !open {
		return false, true
	}
	switch stage {
	case "muster", "bash", "kill", "fight":
		return true, false
	}
	return false, true
}

// leaseSiegeGangs runs ahead of the gang sweep: every siege fleet's expiry is pushed on while its
// siege stands, and set to now once the siege is over, so the sweep disbands it this pass. A kit
// lent to the fleet stays due a grace after the lease.
func (m *Mod) leaseSiegeGangs(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT g.gangID, COALESCE(s.phase, ''), COALESCE(s.closedAt, 1)
		  FROM rolebot_gangs g
		  LEFT JOIN rolebot_corp_siege s ON s.siegeID = JSON_VALUE(g.state, '$.siegeID')
		 WHERE g.enabled = 1 AND g.autoFormed = 1 AND COALESCE(JSON_VALUE(g.state, '$.siegeID'), 0) > 0`)
	if err != nil {
		rlogf("rolebots: siege lease: %v", err)
		return
	}
	type row struct {
		gangID, closedAt int64
		stage            string
	}
	var rs []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.gangID, &r.stage, &r.closedAt) == nil {
			rs = append(rs, r)
		}
	}
	rows.Close()
	now := clock.Unix()
	for _, r := range rs {
		extend, disband := siegeGangLease(r.stage, r.closedAt == 0)
		switch {
		case extend:
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_gangs SET expiresAt = GREATEST(expiresAt, ?) WHERE gangID = ?`,
				now+siegeLeaseSec, r.gangID)
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_issue SET dueAt = GREATEST(dueAt, ?) WHERE gangID = ? AND closedAt = 0`,
				now+siegeLeaseSec+siegeIssueGraceSec, r.gangID)
		case disband:
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_gangs SET expiresAt = ? WHERE gangID = ? AND expiresAt > ?`,
				now, r.gangID, now)
		}
	}
}

// ── pulling the alliance in ────────────────────────────────────────────────

// pullPatrolsForSiege disbands the alliance's Security and Combat patrols as a siege fleet forms,
// so their pilots are free for its recruiting: the siege is the alliance's first call. A fleet that
// answers a call of its own (a defend, another siege), a mining or trade op, and a fleet with a
// member fighting on a watched grid are left alone. Reports how many were disbanded.
func (m *Mod) pullPatrolsForSiege(ctx context.Context, corps []int64, siegeName string) int {
	if len(corps) == 0 {
		return 0
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT g.gangID, g.name, g.leaderCharID FROM rolebot_gangs g
		 WHERE g.enabled = 1 AND g.autoFormed = 1 AND g.activity IN (?, ?)
		   AND g.corpID IN (`+int64CSV(corps)+`)
		   AND COALESCE(JSON_VALUE(g.state, '$.siegeID'), 0) = 0
		   AND COALESCE(JSON_VALUE(g.state, '$.helpCallID'), 0) = 0`, ActSecurity, ActCombat)
	if err != nil {
		return 0
	}
	type gang struct {
		id, leader int64
		name       string
	}
	var gs []gang
	for rows.Next() {
		var g gang
		if rows.Scan(&g.id, &g.name, &g.leader) == nil {
			gs = append(gs, g)
		}
	}
	rows.Close()
	pulled := 0
	for _, g := range gs {
		mems, err := gangMembers(ctx, m.db, g.id)
		if err != nil || gangFightingOnGrid(mems) {
			continue
		}
		if err := deleteGang(ctx, m.db, g.id); err != nil {
			rlogf("rolebots: pull patrol %d into siege: %v", g.id, err)
			continue
		}
		pulled++
		m.logBot(g.leader, logInfo, "gang", "gang %q disbanded — pulled into the siege muster for %s", g.name, siegeName)
	}
	return pulled
}

// gangFightingOnGrid reports whether any of a fleet's members is in a fight on a watched grid.
func gangFightingOnGrid(mems []GangMember) bool {
	for _, mm := range mems {
		embodyMu.Lock()
		eb := embodyLive[mm.CharacterID]
		embodyMu.Unlock()
		if eb == nil {
			continue
		}
		eb.mu.Lock()
		busy := eb.fighting || eb.call.engaged
		eb.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

// siegeFleetName is the name the siege fleet flies. Pure.
func siegeFleetName(corpName string) string {
	return fmt.Sprintf("%s Siege Fleet", corpName)
}

// ── the muster verdict ─────────────────────────────────────────────────────

const (
	// siegeEnRouteWeight is how much of the DPS still flying in a siege muster counts at its
	// deadline: most of it arrives, not all.
	siegeEnRouteWeight = 0.8
	// siegeFormFirepowerFrac is the share of the quota a siege fleet's recruits must bring on paper
	// before it forms at all (formFleetAround): a muster that cannot fill is not flown to.
	siegeFormFirepowerFrac = 0.9
	// siegeShortBackoffBase and siegeShortBackoffMax space the tries after a short muster.
	siegeShortBackoffBase = 6 * 3600
	siegeShortBackoffMax  = 3 * 86400
)

// siegeMusterVerdict decides a siege fleet's muster on DPS: go as soon as the DPS present meets
// the quota; at the deadline go if the present and most of what is flying in meet it, else stand
// down. A quota of 0 (an older row) does not hold the fleet. Pure.
func siegeMusterVerdict(presentDPS, enRouteDPS, quota float64, deadline bool, enRouteWeight float64) (release, standDown bool) {
	if quota <= 0 || presentDPS >= quota {
		return true, false
	}
	if !deadline {
		return false, false
	}
	if presentDPS+enRouteWeight*enRouteDPS >= quota {
		return true, false
	}
	return false, true
}

// siegeShortBackoff is how long a side waits before its next siege after consecutive short
// musters: siegeShortBackoffBase, doubling, held at siegeShortBackoffMax. Pure.
func siegeShortBackoff(consecutive int) int64 {
	if consecutive <= 0 {
		return 0
	}
	d := int64(siegeShortBackoffBase)
	for i := 1; i < consecutive && d < siegeShortBackoffMax; i++ {
		d *= 2
	}
	return min(d, siegeShortBackoffMax)
}

// siegeShortHold reports whether the given corporations' last sieges ended short recently enough
// that the next waits (siegeShortBackoff): the consecutive short results counted back from the
// latest closed attacking siege.
func (m *Mod) siegeShortHold(ctx context.Context, corps []int64, now int64) bool {
	if len(corps) == 0 {
		return false
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT result, closedAt FROM rolebot_corp_siege
		 WHERE closedAt > 0 AND attackerCorpID IN (`+int64CSV(corps)+`)
		 ORDER BY closedAt DESC LIMIT 6`)
	if err != nil {
		return false
	}
	defer rows.Close()
	n := 0
	var last int64
	for rows.Next() {
		var result string
		var at int64
		if rows.Scan(&result, &at) != nil || result != "short" {
			break
		}
		if n == 0 {
			last = at
		}
		n++
	}
	return n > 0 && now-last < siegeShortBackoff(n)
}

// siegeByID is an open siege by its id, nil when it is closed or gone.
func (m *Mod) siegeByID(ctx context.Context, siegeID int64) *siegeRow {
	if ss := m.openSieges(ctx, `siegeID = ?`, siegeID); len(ss) > 0 {
		return ss[0]
	}
	return nil
}

// siegeQuotaFor is a siege's quota DPS: the one it opened on, or, for an older row, sized now.
func (m *Mod) siegeQuotaFor(ctx context.Context, s *siegeRow) float64 {
	if s.QuotaDPS > 0 {
		return s.QuotaDPS
	}
	var typeID int64
	if s.isStation() {
		_ = m.db.QueryRowContext(ctx, `SELECT stationTypeID FROM staStations WHERE stationID = ?`, s.TowerID).Scan(&typeID)
	} else {
		_ = m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, s.TowerID).Scan(&typeID)
	}
	if typeID == 0 {
		return 0
	}
	return siegeQuota(m.structureHPOf(ctx, typeID, !s.isStation()), siegeWindowSec, siegeQuotaMargin).OpenDPS()
}

// memberSeatDPS is what one siege fleet member brings: the kit it was lent, else the combat hull
// it sits in, read through the real fit and skills; else its best own combat hull (pilotSiegeDPS).
func (m *Mod) memberSeatDPS(ctx context.Context, charID int64) float64 {
	var shipID int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT shipItemID FROM rolebot_corp_issue WHERE characterID = ? AND closedAt = 0
		 ORDER BY issuedAt DESC LIMIT 1`, charID).Scan(&shipID)
	if shipID == 0 {
		_ = m.db.QueryRowContext(ctx, `SELECT locationID FROM entity WHERE itemID = ?`, charID).Scan(&shipID)
	}
	if shipID != 0 {
		var typeID int64
		if m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, shipID).Scan(&typeID) == nil {
			if byID, _, err := m.loadHullProfiles(ctx); err == nil {
				if h := byID[typeID]; h != nil && siegeHullClass(h.Class) {
					if l, err := m.loadCombatLoadout(ctx, charID, shipID, typeID); err == nil {
						if dps := loadoutDPS(l); dps > 0 {
							return dps
						}
					}
				}
			}
		}
	}
	return m.pilotSiegeDPS(ctx, charID)
}

// siegeMusterResolve is musterResolve for a siege's fleet: the release is on DPS against the
// siege's quota, not a head count (siegeMusterVerdict). A muster that comes up short stands the
// fleet down without the formation back-off (the call was the siege's, not the fleet's failure)
// and closes the siege as short, so the side waits siegeShortBackoff and tries again.
func (m *Mod) siegeMusterResolve(ctx context.Context, g *Gang, present, enRoute, stuck []int64) error {
	now := clock.Unix()
	s := m.siegeByID(ctx, g.State.SiegeID)
	if s == nil {
		m.narrateFleet(ctx, g.GangID, true, "The siege is off — standing down.")
		return m.quietStandDown(ctx, g, "the siege was called off")
	}
	quota := m.siegeQuotaFor(ctx, s)
	var presentDPS, enRouteDPS float64
	for _, id := range present {
		presentDPS += m.memberSeatDPS(ctx, id)
	}
	for _, id := range enRoute {
		enRouteDPS += m.memberSeatDPS(ctx, id)
	}
	deadline := now >= g.State.MusterDeadline
	release, standDown := siegeMusterVerdict(presentDPS, enRouteDPS, quota, deadline, siegeEnRouteWeight)
	if !release && !standDown {
		return nil // still gathering
	}
	for _, charID := range stuck {
		if err := removeGangMember(ctx, m.db, g.GangID, charID); err != nil {
			rlogf("rolebots: muster drop %d from gang %d: %v", charID, g.GangID, err)
		}
	}
	if standDown {
		why := fmt.Sprintf("muster short — %.0f DPS in and %.0f on the way of the %.0f it takes", presentDPS, enRouteDPS, quota)
		m.narrateFleet(ctx, g.GangID, true, "Not enough guns to break it — standing down. We go again another time.")
		if err := m.quietStandDown(ctx, g, why); err != nil {
			return err
		}
		m.closeSiege(ctx, s, "short", why)
		return nil
	}
	g.State.OpPhase = gangOpLive
	if err := saveGangState(ctx, m.db, g.GangID, &g.State); err != nil {
		return err
	}
	m.stampFleetLive(ctx, g)
	m.logBot(g.LeaderCharID, logInfo, "gang",
		"siege fleet %q is formed up at %s — %d ship(s), %.0f DPS of the %.0f it takes (%.0f more on the way); sending it",
		g.Name, m.stationLabel(ctx, g.State.StagingStationID), len(present), presentDPS, quota, enRouteDPS)
	m.narrateFleet(ctx, g.GangID, false, "We have the guns. Moving out.")
	return nil
}

// quietStandDown is standFleetDown without the formation back-off: a siege fleet that came up
// short says nothing about the corporation's next patrol.
func (m *Mod) quietStandDown(ctx context.Context, g *Gang, why string) error {
	release, ok, err := m.claimGangEnd(ctx, g.GangID)
	if err != nil || !ok {
		return err
	}
	defer release()
	m.mailFleetReport(ctx, g, why)
	if err := deleteGang(ctx, m.db, g.GangID); err != nil {
		return err
	}
	m.logBot(g.LeaderCharID, logInfo, "gang", "gang %q disbanded — %s; members return to their own rotations",
		g.Name, why)
	return nil
}

// siegeAbandonResult is how an abandoned stage closes a siege: a muster that never formed is
// short (retried on siegeShortBackoff), a bash or kill that failed is abandoned. Pure.
func siegeAbandonResult(stage string) string {
	if stage == "muster" {
		return "short"
	}
	return "abandoned"
}
