package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	evedb "gurimoon/db"
)

// Office purposes and the offices a corporation rents for them. Members are posted to offices by the job their
// arm does (corpposting.go). This file says which office serves which job, and rents one when an arm with
// members has nowhere of its own.
// Mining: a station with a refinery among the richest belts near the headquarters, the fleet-ore hangar; only
// for a corporation that sells its ore. Staging: a station near the enemy while at war, else near the
// low-security belts its security arm rats in. Lab: a station with research lines near the factory. Sales:
// the trading post (corptradepost.go). Most purposes are derived every estate think from existing records
// (corporation.stationID is the HQ, corpFactoryStation the factory); only staging and a separate lab are stamped
// `claimed` when rented. An office serving none is a branch (corpbranch.go).
// The cap is one office per officeCapMembers members, between one and officeCapMax, plus the HQ and factory.

const (
	officeCapMembers = 8
	officeCapMax     = 8

	// purposeArmMin is how many members an arm needs before the CEO rents
	// an office for it.
	purposeArmMin = 3

	// purposeHuntRetrySec: a hunt that found nothing or could not afford it
	// does not look again for this long.
	purposeHuntRetrySec = 24 * 3600

	// miningOfficeMaxJumps bounds the mining-office search from the HQ;
	// miningOfficeGain is how much richer the best site must be than the
	// current fleet-ore station before the CEO rents a separate office.
	miningOfficeMaxJumps = 6
	miningOfficeGain     = 1.5

	// stagingMaxJumps bounds the staging search from the HQ. stagingMinJumps is how far out a staging office must
	// be to be worth having, since closer than that the HQ already serves.
	stagingMaxJumps = 12
	stagingMinJumps = 3

	// labOfficeMaxJumps bounds the lab search from the factory.
	labOfficeMaxJumps = 5

	serviceReprocessing = 16
	serviceRefinery     = 32
)

// officeCap is how many offices a corporation of this size may hold. Pure.
func officeCap(members int) int {
	n := int(math.Ceil(float64(members) / officeCapMembers))
	return min(max(n, 1), officeCapMax)
}

// purposeHuntAt is the last fruitless hunt per (corporation, purpose). In
// memory: a restart may look again.
var purposeHuntAt sync.Map

func init() {
	registerCorpForget("purposeHuntAt", func(id int64) {
		for _, p := range []int{officeMining, officeStaging, officeLab, officeYard, officeForward, officeFront} {
			purposeHuntAt.Delete(purposeHuntKey{id, p})
		}
	})
}

type purposeHuntKey struct {
	corpID  int64
	purpose int
}

func huntResting(corpID int64, purpose int) bool {
	v, ok := purposeHuntAt.Load(purposeHuntKey{corpID, purpose})
	return ok && clock.Unix()-v.(int64) < purposeHuntRetrySec
}

func huntRest(corpID int64, purpose int) {
	purposeHuntAt.Store(purposeHuntKey{corpID, purpose}, clock.Unix())
}

// purposeNames is how the ledger and the letters name a purpose.
var purposeNames = []struct {
	bit  int
	name string
}{
	{officeHQ, "headquarters"}, {officeFactory, "factory"}, {officeLab, "laboratory"},
	{officeMining, "mining office"}, {officeStaging, "staging office"},
	{officeSales, "trading post"}, {officeBranch, "branch office"}, {officeYard, "capital yard"},
	{officeForward, "forward office"}, {officeFront, "front"}, {officeCapital, "capital"},
}

// officePurposeLabel is a readable list of what an office is for. Pure.
func officePurposeLabel(bits int) string {
	var out []string
	for _, p := range purposeNames {
		if bits&p.bit != 0 {
			out = append(out, p.name)
		}
	}
	if len(out) == 0 {
		return "office"
	}
	return strings.Join(out, " and ")
}

// derivePurposes works out what each office is for from the records that
// already say so. Pure: the inputs are read by syncOfficePurposes.
func derivePurposes(offices []int64, claimed map[int64]int, hq, factory, factoryLab, oreAt int64,
	liquid map[int64]bool) map[int64]int {
	out := make(map[int64]int, len(offices))
	for _, s := range offices {
		bits := claimed[s]
		if s == hq {
			bits |= officeHQ
		}
		if s == factory {
			bits |= officeFactory
			if factoryLab == s {
				bits |= officeLab
			}
		}
		if s == oreAt {
			bits |= officeMining
		}
		if liquid[s] {
			bits |= officeSales
		}
		if bits == 0 {
			bits = officeBranch
		}
		out[s] = bits
	}
	return out
}

// syncOfficePurposes re-derives and stores every office's purposes, and
// returns them by station.
func (m *Mod) syncOfficePurposes(ctx context.Context, corpID int64) (map[int64]int, error) {
	m.syncCorpOffices(ctx, corpID)
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.stationID, r.claimed, s.solarSystemID
		  FROM rolebot_corp_offices r
		  JOIN staStations s ON s.stationID = r.stationID
		 WHERE r.corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("syncOfficePurposes: %w", err)
	}
	var offices []int64
	claimed := map[int64]int{}
	sysOf := map[int64]int64{}
	for rows.Next() {
		var st, sys int64
		var cl int
		if rows.Scan(&st, &cl, &sys) == nil {
			offices = append(offices, st)
			claimed[st] = cl
			sysOf[st] = sys
		}
	}
	rows.Close()

	var hq int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(stationID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&hq)
	factory := m.corpFactoryStation(ctx, corpID)
	var factoryLab int64
	if factory != 0 && m.stationHasLab(ctx, factory) {
		factoryLab = factory
	}
	lm := m.liquidity(ctx)
	liquid := map[int64]bool{}
	for _, s := range offices {
		if lm.System[sysOf[s]] >= lm.PostCutoff {
			liquid[s] = true
		}
	}
	out := derivePurposes(offices, claimed, hq, factory, factoryLab, m.corpFleetOreStation(ctx, corpID), liquid)
	for s, bits := range out {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_offices SET purposes = ? WHERE corporationID = ? AND stationID = ?`,
			bits, corpID, s); err != nil {
			return out, err
		}
	}
	return out, nil
}

// claimOfficePurpose stamps a purpose nothing else records on an office.
func (m *Mod) claimOfficePurpose(ctx context.Context, corpID, stationID int64, bit int) {
	m.syncCorpOffices(ctx, corpID)
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_offices SET claimed = claimed | ? WHERE corporationID = ? AND stationID = ?`,
		bit, corpID, stationID); err != nil {
		rlogf("rolebots: claim office purpose %d/%d: %v", corpID, stationID, err)
	}
}

// unclaimOfficePurpose takes back a purpose claimOfficePurpose stamped.
func (m *Mod) unclaimOfficePurpose(ctx context.Context, corpID, stationID int64, bit int) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_offices SET claimed = claimed & ~? WHERE corporationID = ? AND stationID = ?`,
		bit, corpID, stationID); err != nil {
		rlogf("rolebots: unclaim office purpose %d/%d: %v", corpID, stationID, err)
	}
}

// officeCount is how many offices the corporation holds that it is not
// giving up.
func (m *Mod) officeCount(ctx context.Context, corpID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM crpoffices o
		 WHERE o.corporationID = ?
		   AND NOT EXISTS (SELECT 1 FROM rolebot_corp_offices r
		                    WHERE r.corporationID = o.corporationID AND r.stationID = o.stationID
		                      AND r.state = 'dropping')`, corpID).Scan(&n)
	return n
}

// hasPurpose reports whether any office serves a purpose.
func hasPurpose(purposes map[int64]int, bit int) (int64, bool) {
	var best int64
	for s, b := range purposes {
		if b&bit != 0 && (best == 0 || s < best) {
			best = s
		}
	}
	return best, best != 0
}

// ceoPurposeOffices is the estate power's purpose step: at most one new
// office per think, for the first arm with members and nowhere of its own.
// Reports whether it rented.
func (m *Mod) ceoPurposeOffices(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, hq int64) (bool, error) {
	if hq == 0 || facts == nil {
		return false, nil
	}
	purposes, err := m.syncOfficePurposes(ctx, c.CorpID)
	if err != nil {
		return false, err
	}
	// A forward office far from the corporation's own frontier is given up as forward, so the hunt
	// below may find one nearer home (frontier.go).
	if st, ok := hasPurpose(purposes, officeForward); ok && !m.forwardStillLocal(ctx, c.CorpID, hq, st) {
		m.unclaimOfficePurpose(ctx, c.CorpID, st, officeForward)
		purposes[st] &^= officeForward
		forgetNullView(c.CorpID)
		m.ceoLog(ctx, c, powerEstate, "forward.reaim", st, 0,
			"no longer stages the push into 0.0 from %s: it lies far from the 0.0 nearest its headquarters",
			stationNameOf(ctx, m, st))
	}
	if m.officeCount(ctx, c.CorpID) >= officeCap(facts.Members) {
		return false, nil
	}
	steps := []struct {
		arm corpArm
		bit int
		run func() (bool, error)
	}{
		{armMining, officeMining, func() (bool, error) { return m.huntMiningOffice(ctx, c, facts, ch, hq, purposes) }},
		{armSecurity, officeStaging, func() (bool, error) { return m.huntStagingOffice(ctx, c, facts, ch, hq) }},
		{armResearch, officeLab, func() (bool, error) { return m.huntLabOffice(ctx, c, facts, ch) }},
	}
	for _, s := range steps {
		if armCount(c.CorpID, s.arm) < purposeArmMin || huntResting(c.CorpID, s.bit) {
			continue
		}
		// A corporation that keeps its ore to build from refines it at the factory (corpRefineChore), so a separate
		// mining office would strand the rock. Only an outfit whose ore desk sells most of its ore gets one.
		if s.bit == officeMining && oreBoundsFor(facts.Policy.Tags).Max >= 0.5 {
			continue
		}
		if s.bit != officeMining {
			if _, ok := hasPurpose(purposes, s.bit); ok {
				continue
			}
		}
		acted, err := s.run()
		if err != nil || acted {
			return acted, err
		}
	}
	// The frontier needs no arm: a corporation reaching for unclaimed 0.0 rents a jumping-off point
	// there (frontier.go).
	if _, ok := hasPurpose(purposes, officeForward); !ok && !huntResting(c.CorpID, officeForward) {
		if want, _ := m.frontierAmbition(ctx, c, ch); want {
			return m.huntForwardOffice(ctx, c, facts, ch, hq, purposes)
		}
	}
	return false, nil
}

// purposeStation is one station a purpose office could open at.
type purposeStation struct {
	StationID int64
	Name      string
	SystemID  int64
	RegionID  int64
	Security  float64
	Jumps     int
	Score     float64
}

// stationsWithService lists the stations in some systems offering any of the
// given services.
func (m *Mod) stationsWithService(ctx context.Context, systems []int64, services ...int) ([]purposeStation, error) {
	if len(systems) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(systems)+len(services))
	for _, s := range systems {
		args = append(args, s)
	}
	for _, s := range services {
		args = append(args, s)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT s.stationID, s.stationName, s.solarSystemID, s.regionID, COALESCE(s.security, 0)
		  FROM staStations s
		  JOIN staOperationServices os ON os.operationID = s.operationID
		 WHERE s.solarSystemID IN (`+placeholders(len(systems))+`)
		   AND os.serviceID IN (`+placeholders(len(services))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("stationsWithService: %w", err)
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

// rentForPurpose rents the best of a ranked list, claiming the purpose on
// the office. Full stations are skipped; a corporation short of the rent
// rests the hunt.
func (m *Mod) rentForPurpose(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter,
	ranked []purposeStation, bit int, why func(purposeStation) string) (bool, error) {
	for _, p := range ranked {
		at := estateCandidate{StationID: p.StationID, Name: p.Name, RegionID: p.RegionID, Security: p.Security}
		rented, err := m.ceoTryRentOffice(ctx, c, facts, ch, at, false, false, why(p))
		if errors.Is(err, evedb.ErrNoFreeOffices) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !rented {
			huntRest(c.CorpID, bit)
			return false, nil
		}
		if bit == officeStaging || bit == officeLab || bit == officeYard || bit == officeForward || bit == officeFront {
			m.claimOfficePurpose(ctx, c.CorpID, p.StationID, bit)
		}
		return true, nil
	}
	huntRest(c.CorpID, bit)
	return false, nil
}

// yardOfficeMaxJumps is how far from the factory a capital yard may be.
const yardOfficeMaxJumps = 10

// huntYardOffice rents a capital program's yard: the nearest station to the factory, within
// yardOfficeMaxJumps, with a manufacturing line whose type may build the hull (FindAssemblyLineForProduct).
// It answers the station rented, or 0 when none was (the hunt rests a day).
func (m *Mod) huntYardOffice(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, hullTypeID int64) (int64, error) {
	factory := m.corpFactoryStation(ctx, c.CorpID)
	if factory == 0 || huntResting(c.CorpID, officeYard) {
		return 0, nil
	}
	near, err := m.corpJumpMap(ctx, m.stationSystemID(ctx, factory))
	if err != nil {
		return 0, err
	}
	var systems []int64
	for sys, d := range near {
		if d <= yardOfficeMaxJumps {
			systems = append(systems, sys)
		}
	}
	cands, err := m.stationsWithService(ctx, systems, serviceFactory)
	if err != nil {
		return 0, err
	}
	var yards []purposeStation
	for _, p := range cands {
		if line, lerr := evedb.FindAssemblyLineForProduct(ctx, m.db, p.StationID, 1, hullTypeID); lerr == nil && line != 0 {
			p.Jumps = near[p.SystemID]
			yards = append(yards, p)
		}
	}
	sort.SliceStable(yards, func(i, j int) bool {
		if yards[i].Jumps != yards[j].Jumps {
			return yards[i].Jumps < yards[j].Jumps
		}
		return yards[i].StationID < yards[j].StationID
	})
	if len(yards) == 0 {
		huntRest(c.CorpID, officeYard)
		return 0, nil
	}
	hull := m.typeNameOr(ctx, hullTypeID)
	for _, p := range yards[:min(len(yards), 3)] {
		rented, err := m.rentForPurpose(ctx, c, facts, ch, []purposeStation{p}, officeYard,
			func(p purposeStation) string {
				return fmt.Sprintf("a yard whose lines may build a %s, %d jump(s) from the factory", hull, p.Jumps)
			})
		if err != nil {
			return 0, err
		}
		if rented {
			return p.StationID, nil
		}
	}
	return 0, nil
}

// serviceFactory is the staServices row for manufacturing lines.
const serviceFactory = 8192

// ── mining ──────────────────────────────────────────────────────────────────

// beltSecWeight is how much a belt system's ore is worth for its security:
// the lower the security, the richer the rock. Pure.
func beltSecWeight(sec float64) float64 {
	return min(max(1+2*(0.5-sec), 0.5), 3)
}

// huntMiningOffice puts the mining arm where the ore is: every station with a refinery within
// miningOfficeMaxJumps of the HQ is scored by the belts in its own and neighbouring systems. When the best
// beats the current fleet-ore station by miningOfficeGain, the ore hangar moves there, to an office already
// held or else a new one.
func (m *Mod) huntMiningOffice(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter,
	hq int64, purposes map[int64]int) (bool, error) {
	hqSys := m.stationSystemID(ctx, hq)
	near, err := m.corpJumpMap(ctx, hqSys)
	if err != nil {
		return false, err
	}
	u, err := m.universe(ctx)
	if err != nil {
		return false, err
	}
	full, _ := m.systemFullnessMap(ctx)
	sysScore := func(sys int64) float64 {
		score := 0.0
		for _, s := range append([]int64{sys}, u.adj[sys]...) {
			if !u.hasBelts(ctx, m.db, s) {
				continue
			}
			f, ok := full[s]
			if !ok {
				f = 1 // untouched
			}
			score += f * beltSecWeight(u.meta[s].Security)
		}
		return score
	}
	var systems []int64
	for sys, d := range near {
		if d <= miningOfficeMaxJumps {
			systems = append(systems, sys)
		}
	}
	cands, err := m.stationsWithService(ctx, systems, serviceReprocessing, serviceRefinery)
	if err != nil {
		return false, err
	}
	for i := range cands {
		cands[i].Jumps = near[cands[i].SystemID]
		cands[i].Score = sysScore(cands[i].SystemID) / (1 + float64(cands[i].Jumps)/4)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return cands[i].StationID < cands[j].StationID
	})
	if len(cands) == 0 || cands[0].Score <= 0 {
		huntRest(c.CorpID, officeMining)
		return false, nil
	}
	oreAt := m.corpFleetOreStation(ctx, c.CorpID)
	cur := 0.0
	if oreAt != 0 {
		cur = sysScore(m.stationSystemID(ctx, oreAt))
	}
	best := cands[0]
	if best.StationID == oreAt || best.Score < cur*miningOfficeGain {
		huntRest(c.CorpID, officeMining)
		return false, nil
	}
	// An office the corporation already holds there: move the hangar, rent
	// nothing.
	for s := range purposes {
		if m.stationSystemID(ctx, s) == best.SystemID {
			return false, m.pointFleetOre(ctx, c, s, "an office it already holds among the belts")
		}
	}
	rented, err := m.rentForPurpose(ctx, c, facts, ch, cands[:min(len(cands), 3)], officeMining,
		func(p purposeStation) string {
			return fmt.Sprintf("a mining office — the richest belts within %d jumps of the headquarters", miningOfficeMaxJumps)
		})
	if err != nil || !rented {
		return rented, err
	}
	// Whichever of the three took the office.
	for _, p := range cands[:min(len(cands), 3)] {
		if m.officeAt(ctx, c.CorpID, p.StationID) != 0 {
			return true, m.pointFleetOre(ctx, c, p.StationID, "its new mining office")
		}
	}
	return true, nil
}

// pointFleetOre makes a station the corporation's fleet-ore hangar.
func (m *Mod) pointFleetOre(ctx context.Context, c *corpCEO, stationID int64, why string) error {
	if m.humanHoldOn(ctx, c.CorpID, cmdKindOreDrop) {
		return nil // the chair said where the ore goes (corpcommanddesk_org.go)
	}
	if err := m.setCorpFleetOreStation(ctx, c.CorpID, stationID); err != nil {
		return err
	}
	m.ceoLog(ctx, c, powerEstate, "hangar", stationID, 0,
		"told the mining fleets to deliver their ore to %s (%s)", stationNameOf(ctx, m, stationID), why)
	return nil
}

// ── staging ─────────────────────────────────────────────────────────────────

// enemyHQSystems is the systems the corporation's live war enemies keep
// their headquarters in.
func (m *Mod) enemyHQSystems(ctx context.Context, corpID int64) []int64 {
	var alliance int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(allianceID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&alliance)
	rows, err := m.db.QueryContext(ctx, `
		SELECT IF(declaredByID IN (?, ?), againstID, declaredByID)
		  FROM wars
		 WHERE timeFinished IS NULL
		   AND (declaredByID IN (?, ?) OR againstID IN (?, ?))`,
		corpID, alliance, corpID, alliance, corpID, alliance)
	if err != nil {
		return nil
	}
	var enemies []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil && id != 0 {
			enemies = append(enemies, id)
		}
	}
	rows.Close()
	var out []int64
	for _, e := range enemies {
		var sys int64
		// A corporation's own HQ, else an alliance's executor's.
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(s.solarSystemID, 0)
			  FROM corporation c
			  JOIN staStations s ON s.stationID = c.stationID
			 WHERE c.corporationID = COALESCE(
			         (SELECT executorCorpID FROM alliances WHERE allianceID = ?), ?)`, e, e).Scan(&sys)
		if sys != 0 {
			out = append(out, sys)
		}
	}
	return out
}

// huntStagingOffice puts the security arm where its fights are: the station nearest the enemy while at war,
// else among the lowest-security belts in reach. Never closer than stagingMinJumps to the HQ, which already
// serves anything that near.
func (m *Mod) huntStagingOffice(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, hq int64) (bool, error) {
	hqSys := m.stationSystemID(ctx, hq)
	near, err := m.corpJumpMap(ctx, hqSys)
	if err != nil {
		return false, err
	}
	u, err := m.universe(ctx)
	if err != nil {
		return false, err
	}
	var systems []int64
	for sys, d := range near {
		if d >= stagingMinJumps && d <= stagingMaxJumps {
			systems = append(systems, sys)
		}
	}
	cands, err := m.stationsWithService(ctx, systems, serviceCloning, serviceRepairFacilities)
	if err != nil {
		return false, err
	}
	why := "a staging office for the security arm, among the low-security belts"
	if enemies := m.enemyHQSystems(ctx, c.CorpID); len(enemies) > 0 {
		toEnemy, err := m.corpJumpMap(ctx, enemies...)
		if err != nil {
			return false, err
		}
		for i := range cands {
			d, ok := toEnemy[cands[i].SystemID]
			if !ok {
				continue
			}
			cands[i].Jumps = near[cands[i].SystemID]
			cands[i].Score = 100 / (1 + float64(d))
		}
		why = "a staging office for the war, as near the enemy as the headquarters reaches"
	} else {
		for i := range cands {
			sys := cands[i].SystemID
			cands[i].Jumps = near[sys]
			score := 0.0
			for _, s := range append([]int64{sys}, u.adj[sys]...) {
				if u.hasBelts(ctx, m.db, s) {
					score += max(0, 1.5-u.meta[s].Security)
				}
			}
			cands[i].Score = score / (1 + float64(cands[i].Jumps)/6)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return cands[i].StationID < cands[j].StationID
	})
	if len(cands) == 0 || cands[0].Score <= 0 {
		huntRest(c.CorpID, officeStaging)
		return false, nil
	}
	return m.rentForPurpose(ctx, c, facts, ch, cands[:min(len(cands), 3)], officeStaging,
		func(purposeStation) string { return why })
}

// serviceRepairFacilities is the staServices row for a repair shop; a staging office is somewhere a fleet
// comes back to.
const serviceRepairFacilities = 4096

// ── laboratory ──────────────────────────────────────────────────────────────

// huntLabOffice gives the R&D arm research lines when the factory has none:
// the nearest station to the factory that does.
func (m *Mod) huntLabOffice(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter) (bool, error) {
	factory := m.corpFactoryStation(ctx, c.CorpID)
	if factory == 0 {
		return false, nil // the facility hunt comes first (corpfacility.go)
	}
	if m.stationHasLab(ctx, factory) {
		return false, nil
	}
	near, err := m.corpJumpMap(ctx, m.stationSystemID(ctx, factory))
	if err != nil {
		return false, err
	}
	var systems []int64
	for sys, d := range near {
		if d <= labOfficeMaxJumps {
			systems = append(systems, sys)
		}
	}
	cands, err := m.stationsWithService(ctx, systems, serviceLaboratory)
	if err != nil {
		return false, err
	}
	var labs []purposeStation
	for _, p := range cands {
		if m.stationHasLab(ctx, p.StationID) {
			p.Jumps = near[p.SystemID]
			labs = append(labs, p)
		}
	}
	sort.SliceStable(labs, func(i, j int) bool {
		if labs[i].Jumps != labs[j].Jumps {
			return labs[i].Jumps < labs[j].Jumps
		}
		return labs[i].StationID < labs[j].StationID
	})
	if len(labs) == 0 {
		huntRest(c.CorpID, officeLab)
		return false, nil
	}
	return m.rentForPurpose(ctx, c, facts, ch, labs[:min(len(labs), 3)], officeLab,
		func(p purposeStation) string {
			return fmt.Sprintf("a laboratory for the R&D arm, %d jump(s) from the factory, which has no research lines", p.Jumps)
		})
}

// serviceLaboratory is the staServices row for research.
const serviceLaboratory = 16384
