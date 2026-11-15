package rolebots

import (
	"context"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

// The frontier push into unclaimed 0.0 (unclaimedNullSystems), where one online tower of an alliance
// corporation wins the system (db/sovmajority.go). frontierAmbition says who reaches for it;
// huntForwardOffice rents the jumping-off station, which corpPlatformReach then stages and fuels from;
// considerNewTower keeps frontier survey orders out (surveyOpts) and plants in the alliance's systems
// first. Switches: rolebots.corp.forwardOffices, rolebots.alliance.frontier (the stake,
// alliancestake.go). Design note: docs/ALLIANCES.md, "The frontier".

const (
	// frontierAppetite is the PosAppetite at which a corporation that already runs a mining tower
	// reaches for the frontier on its own, alliance or none.
	frontierAppetite = 0.6
	// frontierWantUsable is how many usable unclaimed-0.0 moons an ambitious corporation wants
	// known; below it, it keeps frontier survey orders out.
	frontierWantUsable = 2
	// frontierSurveyOpenMax is how many frontier survey orders may be open at once, on top of the
	// ordinary surveyOpenMax.
	frontierSurveyOpenMax = 2
	// surveyPullMul is how much more a survey order in a system the alliance targets or stakes is
	// worth.
	surveyPullMul = 3.0

	// forwardReachJumps is how far from a forward office the unclaimed systems it serves may lie.
	forwardReachJumps = 5
	// forwardHQJumpCost discounts a forward station per jump from the headquarters, so the push goes
	// out from home rather than from wherever another office stands.
	forwardHQJumpCost = 0.1
	// forwardContestBonus favours a station inside contestable 0.0 over one in NPC null beside it.
	forwardContestBonus = 1.5
	// forwardServiceBonus favours a station with cloning or repair: somewhere a pilot comes back to.
	forwardServiceBonus = 1.25
	// forwardPullMul is how much more an unclaimed system the alliance targets or stakes counts.
	forwardPullMul = 2.0
	// forwardDangerTop is how many of the best stations have their danger measured (a query each).
	forwardDangerTop = 12
	// frontierUnknownWorth is what an unsurveyed unclaimed system counts for (forwardStationScore);
	// frontierPoorWorth one surveyed with nothing worth a harvester.
	frontierUnknownWorth = 1.0
	frontierPoorWorth    = 0.2
)

// frontierAmbition is whether a corporation reaches for unclaimed 0.0, and why.
func (m *Mod) frontierAmbition(ctx context.Context, c *corpCEO, ch Charter) (bool, string) {
	if !StarbasePower() || ch.PosAppetite < 0.4 {
		return false, ""
	}
	if allianceID := m.corpAllianceID(ctx, c.CorpID); allianceID != 0 {
		if len(m.allianceFrontierSystems(ctx, allianceID)) > 0 {
			return true, "its alliance has named systems to hold"
		}
		if m.allianceSovMotive(ctx, allianceID) {
			return true, "its alliance was founded to hold 0.0"
		}
	}
	if ch.PosAppetite < frontierAppetite {
		return false, ""
	}
	if m.corpMinesAMoon(ctx, c.CorpID) {
		return true, "it mines a moon already and wants more"
	}
	if v := m.corpNullView(ctx, c.CorpID, m.corpHQStation(ctx, c.CorpID)); v != nil &&
		v.NearestContest >= 0 && v.NearestContest <= localFrontierJumps {
		return true, fmt.Sprintf("unclaimed 0.0 lies %d jumps from its headquarters", v.NearestContest)
	}
	return false, ""
}

// corpLocalFrontier is the unclaimed 0.0 the corporation pushes into (localFrontierSet): near its
// headquarters, plus its alliance's targets and stake (pull).
func (m *Mod) corpLocalFrontier(ctx context.Context, corpID, hq int64, pull map[int64]bool) map[int64]bool {
	u, err := m.universe(ctx)
	v := m.corpNullView(ctx, corpID, hq)
	if err != nil || u == nil || v == nil {
		return m.unclaimedNullSystems(ctx)
	}
	return localFrontierSet(v, pull, func(sys int64) int64 { return u.meta[sys].ConstellationID })
}

// forwardStillLocal reports whether a forward office still serves the corporation's own frontier:
// its system lies within forwardReachJumps of it. A forward office rented under the old
// nearest-office rule, in NPC null across the map, does not, and is given up as forward.
func (m *Mod) forwardStillLocal(ctx context.Context, corpID, hq, station int64) bool {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return true
	}
	allianceID := m.corpAllianceID(ctx, corpID)
	frontier := m.corpLocalFrontier(ctx, corpID, hq, m.allianceFrontierSystems(ctx, allianceID))
	if len(frontier) == 0 {
		return true
	}
	sys := m.stationSystemID(ctx, station)
	for s := range bfsJumps(u, sys, forwardReachJumps) {
		if frontier[s] {
			return true
		}
	}
	return false
}

// corpMinesAMoon is whether the corporation has a mining tower in space.
func (m *Mod) corpMinesAMoon(ctx context.Context, corpID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_towers
		 WHERE corporationID = ? AND purpose = ? AND status IN (?, ?, ?)`,
		corpID, purposeMining, towerAnchored, towerRigging, towerRunning).Scan(&n)
	return n > 0
}

// allianceFrontierSystems is every system an alliance names to hold or stakes (rolebot_alliance_targets,
// kinds 'sov' and 'stake').
func (m *Mod) allianceFrontierSystems(ctx context.Context, allianceID int64) map[int64]bool {
	out := map[int64]bool{}
	if allianceID == 0 {
		return out
	}
	ids, _ := int64Column(ctx, m, `
		SELECT systemID FROM rolebot_alliance_targets WHERE allianceID = ? AND kind IN ('sov', 'stake')`, allianceID)
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// usableFrontierCount is usableMoonCount over unclaimed 0.0 only. Pure.
func usableFrontierCount(intel []moonIntel, occupied map[int64]bool, floor float64, frontier map[int64]bool) int {
	n := 0
	for _, mi := range intel {
		if frontier[mi.SystemID] && mi.BestClass >= moonR4 && !occupied[mi.MoonID] && mi.Value >= floor {
			n++
		}
	}
	return n
}

// ── the forward office ──────────────────────────────────────────────────────

// forwardIn is what a candidate forward station is scored on.
type forwardIn struct {
	Near     map[int64]int     // unclaimed system to jumps from the station's system (<= forwardReachJumps)
	Worth    map[int64]float64 // unclaimed system to what it is worth (frontierSystemWorth); missing = unknown
	Pull     map[int64]bool    // systems the alliance targets or stakes
	HQJumps  int
	Services bool
	// Contestable is a station in contestable 0.0 (an outpost or conquerable), not NPC null.
	Contestable bool
	// Freeport: a conquerable CONCORD still holds, open to everyone. Alliances live out of it and
	// scheme to take it (forwardFreeportBonus).
	Freeport bool
	Danger   float64 // towerDanger at the station's system; 0 before it is measured
}

// forwardFreeportBonus is how much more a CONCORD-held conquerable counts as a forward office or a
// front: a 0.0 station anyone may dock at, clone and stage from.
const forwardFreeportBonus = 2.0

// isFreeport reports whether a station is a 0.0 conquerable CONCORD holds as the neutral first owner.
// Pure.
func isFreeport(ownerCorp int64, security float64) bool {
	return ownerCorp == evedb.OwnerCONCORD && security < 0
}

// forwardStationScore is how good a jumping-off point a station is: the worth of the unclaimed
// systems within forwardReachJumps, each divided by one plus its distance, the alliance's systems
// counting forwardPullMul more; then a bonus for services and discounts for the headquarters'
// distance and the danger. Pure.
func forwardStationScore(in forwardIn) float64 {
	s := 0.0
	for sys, d := range in.Near {
		w, ok := in.Worth[sys]
		if !ok {
			w = frontierUnknownWorth
		}
		if in.Pull[sys] {
			w *= forwardPullMul
		}
		s += w / float64(1+max(d, 0))
	}
	if in.Services {
		s *= forwardServiceBonus
	}
	if in.Contestable {
		s *= forwardContestBonus
	}
	if in.Freeport {
		s *= forwardFreeportBonus
	}
	s /= 1 + forwardHQJumpCost*float64(max(in.HQJumps, 0))
	s /= 1 + max(in.Danger, 0)
	return s
}

// frontierSystemWorth turns the best surveyed moon value in an unclaimed system into what the
// system counts for: a moon paying towerMiningNeedRef an hour counts 1, capped at 3; a surveyed
// system with nothing worth a harvester counts frontierPoorWorth. Pure.
func frontierSystemWorth(bestValue float64) float64 {
	return min(max(bestValue/towerMiningNeedRef, frontierPoorWorth), 3)
}

// frontierWorths is frontierSystemWorth for every unclaimed system the corporation and its friends
// have surveyed.
func (m *Mod) frontierWorths(ctx context.Context, corpID int64, frontier map[int64]bool) map[int64]float64 {
	out := map[int64]float64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT systemID, MAX(value) FROM rolebot_corp_moon_intel
		 WHERE corporationID IN (`+int64CSV(intelFriendsOf(corpID))+`) AND surveyedAt >= ?
		 GROUP BY systemID`, clock.Unix()-surveyIntelKeep)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var sys int64
		var v float64
		if rows.Scan(&sys, &v) == nil && frontier[sys] {
			out[sys] = frontierSystemWorth(v)
		}
	}
	return out
}

// forwardCandidate is one station a forward office could open at, with its score.
type forwardCandidate struct {
	purposeStation
	Unclaimed int // unclaimed systems within forwardReachJumps
	in        forwardIn
}

// huntForwardOffice rents a forward office: among the 0.0 stations within forwardReachJumps of the
// corporation's own frontier (corpLocalFrontier: near its headquarters, or its alliance's targets
// and stake), the best by forwardStationScore. An
// office the corporation already holds among them is claimed instead, renting nothing. The hunt
// rests a day when nothing is found or affordable.
func (m *Mod) huntForwardOffice(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter,
	hq int64, purposes map[int64]int) (bool, error) {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return false, err
	}
	allianceID := m.corpAllianceID(ctx, c.CorpID)
	pull := m.allianceFrontierSystems(ctx, allianceID)
	frontier := m.corpLocalFrontier(ctx, c.CorpID, hq, pull)
	if len(frontier) == 0 {
		huntRest(c.CorpID, officeForward)
		return false, nil
	}
	hqDist, err := m.corpJumpMap(ctx, m.stationSystemID(ctx, hq))
	if err != nil {
		return false, err
	}
	// Every 0.0 system within forwardReachJumps of unclaimed space that the headquarters reaches.
	src := make(map[int64]int64, len(frontier))
	for sys := range frontier {
		src[sys] = 0
	}
	nearFrontier, _ := platformReach(u, src, forwardReachJumps)
	var systems []int64
	for sys := range nearFrontier {
		if _, ok := hqDist[sys]; ok && u.meta[sys].Security < 0 {
			systems = append(systems, sys)
		}
	}
	if len(systems) == 0 {
		huntRest(c.CorpID, officeForward)
		return false, nil
	}
	worth := m.frontierWorths(ctx, c.CorpID, frontier)
	nearOf := func(sys int64) map[int64]int {
		out := map[int64]int{}
		for s, d := range bfsJumps(u, sys, forwardReachJumps) {
			if frontier[s] {
				out[s] = d
			}
		}
		return out
	}
	// An office already held out there becomes the forward office.
	inArea := map[int64]bool{}
	for _, s := range systems {
		inArea[s] = true
	}
	var held []int64
	for st := range purposes {
		if inArea[m.stationSystemID(ctx, st)] {
			held = append(held, st)
		}
	}
	if len(held) > 0 {
		sort.Slice(held, func(i, j int) bool { return held[i] < held[j] })
		best, bestScore := int64(0), -1.0
		for _, st := range held {
			sys := m.stationSystemID(ctx, st)
			s := forwardStationScore(forwardIn{Near: nearOf(sys), Worth: worth, Pull: pull, HQJumps: hqDist[sys]})
			if s > bestScore {
				best, bestScore = st, s
			}
		}
		m.claimOfficePurpose(ctx, c.CorpID, best, officeForward)
		m.ceoLog(ctx, c, powerEstate, "forward", best, 0,
			"made its office at %s its forward office for the push into unclaimed 0.0", stationNameOf(ctx, m, best))
		return false, nil
	}
	all, err := m.stationsIn(ctx, systems)
	if err != nil {
		return false, err
	}
	served, err := m.stationsWithService(ctx, systems, serviceCloning, serviceRepairFacilities)
	if err != nil {
		return false, err
	}
	hasService := map[int64]bool{}
	for _, p := range served {
		hasService[p.StationID] = true
	}
	npcNull := map[int64]bool{}
	if v := m.corpNullView(ctx, c.CorpID, hq); v != nil {
		npcNull = v.NPCNull
	}
	nearCache := map[int64]map[int64]int{}
	cands := make([]forwardCandidate, 0, len(all))
	for _, p := range all {
		near, ok := nearCache[p.SystemID]
		if !ok {
			near = nearOf(p.SystemID)
			nearCache[p.SystemID] = near
		}
		if len(near) == 0 {
			continue
		}
		p.Jumps = hqDist[p.SystemID]
		fc := forwardCandidate{purposeStation: p, Unclaimed: len(near),
			in: forwardIn{Near: near, Worth: worth, Pull: pull, HQJumps: p.Jumps, Services: hasService[p.StationID],
				Contestable: p.Security < 0 && !npcNull[p.SystemID],
				Freeport:    isFreeport(evedb.StationOwnerCorp(ctx, m.db, p.StationID), p.Security)}}
		fc.Score = forwardStationScore(fc.in)
		cands = append(cands, fc)
	}
	rank := func() {
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].Score != cands[j].Score {
				return cands[i].Score > cands[j].Score
			}
			return cands[i].StationID < cands[j].StationID
		})
	}
	rank()
	// Danger is a query per system: measured for the best few, which are then ranked again.
	for i := 0; i < len(cands) && i < forwardDangerTop; i++ {
		cands[i].in.Danger = m.towerDanger(ctx, c.CorpID, cands[i].SystemID, cands[i].Security, ch.PosDangerWeight)
		cands[i].Score = forwardStationScore(cands[i].in)
	}
	if len(cands) > forwardDangerTop {
		cands = cands[:forwardDangerTop]
	}
	rank()
	if len(cands) == 0 || cands[0].Score <= 0 {
		huntRest(c.CorpID, officeForward)
		return false, nil
	}
	ranked := make([]purposeStation, 0, 3)
	byID := map[int64]forwardCandidate{}
	for _, fc := range cands[:min(len(cands), 3)] {
		ranked = append(ranked, fc.purposeStation)
		byID[fc.StationID] = fc
	}
	return m.rentForPurpose(ctx, c, facts, ch, ranked, officeForward, func(p purposeStation) string {
		return fmt.Sprintf("a forward office for the push into unclaimed 0.0: %d unclaimed system(s) within %d jumps, %d jump(s) from the headquarters",
			byID[p.StationID].Unclaimed, forwardReachJumps, p.Jumps)
	})
}

// stationsIn lists every station in some systems.
func (m *Mod) stationsIn(ctx context.Context, systems []int64) ([]purposeStation, error) {
	if len(systems) == 0 {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.stationName, s.solarSystemID, s.regionID, COALESCE(s.security, 0)
		  FROM staStations s
		 WHERE s.solarSystemID IN (`+int64CSV(systems)+`)`)
	if err != nil {
		return nil, fmt.Errorf("stationsIn: %w", err)
	}
	defer rows.Close()
	var out []purposeStation
	for rows.Next() {
		var p purposeStation
		if err := rows.Scan(&p.StationID, &p.Name, &p.SystemID, &p.RegionID, &p.Security); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// forwardOfficeOf is the corporation's forward office station, 0 when it has none.
func (m *Mod) forwardOfficeOf(ctx context.Context, corpID int64) int64 {
	var st int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT stationID FROM rolebot_corp_offices
		 WHERE corporationID = ? AND (claimed & ?) <> 0 AND state <> 'dropping'
		 ORDER BY stationID LIMIT 1`, corpID, officeForward).Scan(&st)
	return st
}

// allianceSovMotive is whether a bot founded the alliance to hold 0.0 (rolebot_founded_alliances.motive).
func (m *Mod) allianceSovMotive(ctx context.Context, allianceID int64) bool {
	if allianceID == 0 {
		return false
	}
	var motive string
	_ = m.db.QueryRowContext(ctx, `SELECT motive FROM rolebot_founded_alliances WHERE allianceID = ?`, allianceID).Scan(&motive)
	return motive == foundMotiveSov
}

// foundMotiveSov marks an alliance founded to hold 0.0.
const foundMotiveSov = "sov"

// frontierFirstTowers is how many towers a corporation must hold in the alliance's systems or in
// unclaimed 0.0 before a site there stops being taken ahead of every other (considerNewTower).
const frontierFirstTowers = 2

// frontierTowerCount is how many of a corporation's live plans stand in the alliance's systems
// (pull) or in unclaimed 0.0. Pure.
func frontierTowerCount(plans []*towerPlan, pull, frontier map[int64]bool) int {
	n := 0
	for _, p := range plans {
		if p.active() && (pull[p.SystemID] || frontier[p.SystemID]) {
			n++
		}
	}
	return n
}

// sitesInFirst keeps the candidates in the pulled systems when there are any. With none, it
// returns them all, or nothing when strict: a corporation holding its slot for the alliance's
// systems (frontierHoldLeft) plants nowhere else. Pure.
func sitesInFirst(cands []siteCandidate, pull map[int64]bool, strict bool) []siteCandidate {
	var in []siteCandidate
	for _, sc := range cands {
		if pull[sc.SystemID] {
			in = append(in, sc)
		}
	}
	if len(in) == 0 && !strict {
		return cands
	}
	return in
}

// frontierHoldSec is how long an ambitious alliance member keeps its mining slot for the alliance's
// targets and stake while nobody has surveyed a usable moon there. Counted from the oldest target's
// setAt (allianceFrontierSince); after it, sitesInFirst falls back to every site again.
const frontierHoldSec = 5 * 86400

// frontierHoldLeft is the hold still to run, in seconds, 0 when it is over or never began. Pure.
func frontierHoldLeft(since, now int64) int64 {
	if since <= 0 {
		return 0
	}
	return max(0, since+frontierHoldSec-now)
}

// allianceFrontierSince is when the alliance's oldest standing target or stake was named, 0 with
// none.
func (m *Mod) allianceFrontierSince(ctx context.Context, allianceID int64) int64 {
	if allianceID == 0 {
		return 0
	}
	var since int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(setAt), 0) FROM rolebot_alliance_targets
		 WHERE allianceID = ? AND kind IN ('sov', 'stake') AND setAt > 0`, allianceID).Scan(&since)
	return since
}

// frontierBorderJumps is how far from held sov an unclaimed system still takes the full frontier
// bonus (siteScore) once the alliance holds sov; further out it takes frontierFarBonus.
const frontierBorderJumps = 2

// allianceBorder is gate jumps from the nearest system the alliance holds, for every system within
// frontierBorderJumps+1 of one (held systems are 0). Empty when it holds none.
func (m *Mod) allianceBorder(ctx context.Context, allianceID int64, u *universe) map[int64]int {
	out := map[int64]int{}
	if u == nil {
		return out
	}
	for h := range m.allianceHeldSystems(ctx, allianceID) {
		for sys, j := range bfsJumps(u, h, frontierBorderJumps+1) {
			if cur, ok := out[sys]; !ok || j < cur {
				out[sys] = j
			}
		}
	}
	return out
}

// holdsSovFor is whether the corporation's alliance holds sovereignty in a system: a tower there
// may be what holds it, so its fuel is urgent sooner (fuelerDesk).
func (m *Mod) holdsSovFor(ctx context.Context, corpID, systemID int64) bool {
	allianceID := m.corpAllianceID(ctx, corpID)
	if allianceID == 0 || systemID == 0 {
		return false
	}
	var holder int64
	_ = m.db.QueryRowContext(ctx, `SELECT allianceID FROM solarSystemSov WHERE solarSystemID = ?`, systemID).Scan(&holder)
	return holder == allianceID
}
