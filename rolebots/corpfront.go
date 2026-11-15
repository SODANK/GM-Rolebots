package rolebots

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// The front and the capital. While its alliance holds 0.0, a corporation's field arms (security,
// mining, infrastructure) stage out to an office beside it, the front, a share growing with the days
// held (frontShare). Once it owns a 0.0 station, every arm lives there, the capital, with the
// headquarters and the factory. The Base drives clone, home and residency (corpposting.go).
// Front residents shop one band wider (frontBuyCap) and the front keeps a charge shelf (frontStock).
// Design notes: docs/ALLIANCES.md "The front and the capital", docs/CORPS.md §13.

const (
	frontKind   = "front"
	capitalKind = "capital"
	// frontReachJumps is how far from a held system a front office may stand.
	frontReachJumps = forwardReachJumps
	// frontHuntTop is how many front candidates have their danger measured and are tried.
	frontHuntTop = 6
	// frontStockEverySec is how often the front's charge shelf is looked at.
	frontStockEverySec = 6 * 3600
	// frontStockTypes is how many charge types the shelf keeps, the most carried first.
	frontStockTypes = 6
	// frontStockMinPilots is how many pilots a front needs before it keeps a shelf.
	frontStockMinPilots = 3
)

// corpFront is a corporation's front or capital (rolebot_corp_fronts). Zero when it has neither.
type corpFront struct {
	Kind        string
	StationID   int64
	SystemID    int64
	ClaimedBits int
	Since       int64
}

// fieldArm reports whether an arm stages out to a front. Pure.
func fieldArm(a corpArm) bool {
	return a == armSecurity || a == armMining || a == armInfrastructure
}

// stageBits is the office purposes an arm is posted to beside its own: a capital for every arm but
// sales, a front for the field arms. Sales stays at the empire market, where the goo is sold. Pure.
func stageBits(a corpArm) int {
	switch {
	case fieldArm(a):
		return officeCapital | officeFront
	case a == armSales:
		return 0
	}
	return officeCapital
}

// sovHomeJumps is how far from its front or capital a corporation's pilots treat the sov their
// alliance holds as home: ratting home ground (appendSovHomeGround) and the belt discount.
const sovHomeJumps = 5

// appendSovHomeGround adds to ground the held systems within near (jumps from the front), nearest
// first, skipping any already in it. Pure.
func appendSovHomeGround(ground []int64, held map[int64]bool, near map[int64]int) []int64 {
	seen := make(map[int64]bool, len(ground))
	for _, s := range ground {
		seen[s] = true
	}
	var add []int64
	for s := range held {
		if _, ok := near[s]; ok && !seen[s] {
			add = append(add, s)
		}
	}
	sort.Slice(add, func(i, j int) bool {
		if near[add[i]] != near[add[j]] {
			return near[add[i]] < near[add[j]]
		}
		return add[i] < add[j]
	})
	return append(ground, add...)
}

// sovMinePullJumps is how many jumps cheaper a belt in held sov near the front costs a pilot of a
// corporation living there (findMiningTargetIn).
const sovMinePullJumps = 2.0

// sovMineHome is the alliance-held systems within sovHomeJumps of the pilot's corporation's front
// or capital; empty for a corporation with neither.
func (m *Mod) sovMineHome(ctx context.Context, b *botRun, u *universe) map[int64]bool {
	out := map[int64]bool{}
	if b.CorpID == 0 || u == nil {
		return out
	}
	front := m.frontOfficeOf(ctx, b.CorpID)
	if front == 0 {
		return out
	}
	near := bfsJumps(u, m.stationSystemOf(ctx, front), sovHomeJumps)
	for s := range m.allianceHeldSystems(ctx, m.corpAllianceID(ctx, b.CorpID)) {
		if _, ok := near[s]; ok {
			out[s] = true
		}
	}
	return out
}

// frontShare is the share of a field arm posted to the front after it has stood heldSec. Pure.
func frontShare(heldSec int64) float64 {
	switch {
	case heldSec >= 7*86400:
		return 0.9
	case heldSec >= 3*86400:
		return 0.75
	}
	return 0.5
}

// stageQuotas lays a front or a capital over the department quotas. A capital takes every arm but
// sales whole (the corporation lives there; its headquarters stays in empire); a front takes
// frontShare of each field arm, the rest staying at the arm's own offices. Without either, base is
// returned as it is. Pure.
func stageQuotas(base map[corpArm]map[int64]int, members []postMember, offices []postOffice, since, now int64) map[corpArm]map[int64]int {
	var capitals, fronts []int64
	isFront := map[int64]bool{}
	for _, o := range offices {
		switch {
		case o.Dropping:
		case o.Purposes&officeCapital != 0:
			capitals = append(capitals, o.StationID)
		case o.Purposes&officeFront != 0:
			fronts = append(fronts, o.StationID)
			isFront[o.StationID] = true
		}
	}
	if len(capitals) == 0 && len(fronts) == 0 {
		return base
	}
	count := map[corpArm]int{}
	for _, mb := range members {
		count[mb.Arm]++
	}
	out := make(map[corpArm]map[int64]int, len(base)+len(count))
	for arm, q := range base {
		out[arm] = q
	}
	if len(capitals) > 0 {
		for arm, n := range count {
			if arm == armSales {
				continue // sells at the empire market
			}
			w := map[int64]int{}
			for _, st := range capitals {
				w[st] = 1
			}
			out[arm] = armQuota(n, w)
		}
		return out
	}
	share := frontShare(now - since)
	for arm, n := range count {
		if !fieldArm(arm) {
			continue
		}
		_, serving := postingTarget(arm, offices)
		var home []int64
		for _, o := range serving {
			if !isFront[o.StationID] {
				home = append(home, o.StationID)
			}
		}
		w := map[int64]int{}
		for _, st := range fronts {
			w[st] = max(1, int(share*100)/len(fronts))
		}
		for _, st := range home {
			w[st] = max(1, int((1-share)*100)/len(home))
		}
		out[arm] = armQuota(n, w)
	}
	return out
}

// loadCorpFront reads a corporation's front or capital; zero when it has none.
func (m *Mod) loadCorpFront(ctx context.Context, corpID int64) corpFront {
	var f corpFront
	err := m.db.QueryRowContext(ctx, `
		SELECT kind, stationID, systemID, claimedBits, since FROM rolebot_corp_fronts WHERE corporationID = ?`,
		corpID).Scan(&f.Kind, &f.StationID, &f.SystemID, &f.ClaimedBits, &f.Since)
	if err != nil {
		return corpFront{}
	}
	return f
}

// frontOfficeOf is the station of a corporation's front or capital, 0 for none.
func (m *Mod) frontOfficeOf(ctx context.Context, corpID int64) int64 {
	return m.loadCorpFront(ctx, corpID).StationID
}

// ── who lives out there ─────────────────────────────────────────────────────

// frontResidents is every pilot whose Base is its corporation's front or capital, refreshed by
// the front pass each think. In memory: empty after a restart until the first think.
var frontResidents sync.Map // charID -> corpID

func init() {
	registerCharForget("frontResidents", func(id int64) { frontResidents.Delete(id) })
	registerCorpForget("frontResidents", func(id int64) {
		frontResidents.Range(func(k, v any) bool {
			if v.(int64) == id {
				frontResidents.Delete(k)
			}
			return true
		})
	})
}

// frontResident reports whether a pilot lives at its corporation's front or capital.
func frontResident(charID int64) bool {
	_, ok := frontResidents.Load(charID)
	return ok
}

// frontBuyCap is the buy fence for a pilot: one band wider than capBand (bandNear2) for a front
// resident, whose own region may sell nothing. Pure over frontResidents.
func frontBuyCap(charID int64, capBand scopeBand) scopeBand {
	if capBand < bandNear2 && frontResident(charID) {
		return bandNear2
	}
	return capBand
}

// refreshFrontResidents marks the corporation's members based at station (0: none) as front
// residents and clears the rest. Reports how many live there.
func (m *Mod) refreshFrontResidents(ctx context.Context, corpID, station int64) int {
	frontResidents.Range(func(k, v any) bool {
		if v.(int64) == corpID {
			frontResidents.Delete(k)
		}
		return true
	})
	if station == 0 {
		return 0
	}
	ids, _ := int64Column(ctx, m, `
		SELECT r.characterID FROM chrCorporationRoles r
		  JOIN character_ c ON c.characterID = r.characterID AND c.corporationID = ?
		 WHERE r.baseID = ?`, corpID, station)
	for _, id := range ids {
		frontResidents.Store(id, corpID)
	}
	return len(ids)
}

// ── the pass ────────────────────────────────────────────────────────────────

// ceoFront is the estate power's front step, every think: a capital when the corporation owns a
// station in 0.0, else a front while its alliance holds 0.0, else neither. Returns the headquarters
// station, moved when a capital took it.
func (m *Mod) ceoFront(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, hq int64) (int64, error) {
	cur := m.loadCorpFront(ctx, c.CorpID)
	if st, sys := m.ownedNullStation(ctx, c.CorpID); st != 0 {
		hq, err := m.ceoCapital(ctx, c, facts, hq, cur, st, sys)
		if err == nil && m.loadCorpFront(ctx, c.CorpID).StationID == st {
			m.keepStationPolicy(ctx, c, ch, st) // its doors, by the CEO's personality (stationpolicy.go)
		}
		return hq, err
	}
	if cur.Kind == capitalKind {
		m.retreatFront(ctx, c, cur, fmt.Sprintf("no longer owns %s", stationNameOf(ctx, m, cur.StationID)))
		cur = corpFront{}
	}
	held := m.allianceHeldSystems(ctx, m.corpAllianceID(ctx, c.CorpID))
	if len(held) == 0 {
		if cur.Kind == frontKind {
			m.retreatFront(ctx, c, cur, "its alliance holds no 0.0 any more")
		}
		return hq, nil
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return hq, err
	}
	if cur.Kind == frontKind {
		if m.officeAt(ctx, c.CorpID, cur.StationID) != 0 && nearAny(u, cur.SystemID, held, frontReachJumps) {
			m.keepFront(ctx, c, cur)
			return hq, nil
		}
		m.retreatFront(ctx, c, cur, fmt.Sprintf("%s no longer serves what its alliance holds", stationNameOf(ctx, m, cur.StationID)))
	}
	if huntResting(c.CorpID, officeFront) {
		return hq, nil
	}
	st, why, err := m.huntFrontOffice(ctx, c, ch, facts, hq, u, held)
	if err != nil || st == 0 {
		if err == nil {
			huntRest(c.CorpID, officeFront)
		}
		return hq, err
	}
	m.openFront(ctx, c, st, why)
	return hq, nil
}

// nearAny reports whether sys lies within maxJumps of any system in set. Pure over the universe.
func nearAny(u *universe, sys int64, set map[int64]bool, maxJumps int) bool {
	for s := range bfsJumps(u, sys, maxJumps) {
		if set[s] {
			return true
		}
	}
	return false
}

// allianceHeldSystems is every system an alliance holds; empty for none or no alliance.
func (m *Mod) allianceHeldSystems(ctx context.Context, allianceID int64) map[int64]bool {
	out := map[int64]bool{}
	if allianceID == 0 {
		return out
	}
	ids, _ := int64Column(ctx, m, `SELECT solarSystemID FROM solarSystemSov WHERE allianceID = ?`, allianceID)
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// ownedNullStation is the 0.0 station the corporation owns (the lowest ID when several), 0 for none.
func (m *Mod) ownedNullStation(ctx context.Context, corpID int64) (stationID, systemID int64) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT st.stationID, st.solarSystemID FROM staStations st
		  JOIN mapSolarSystems s ON s.solarSystemID = st.solarSystemID
		 WHERE st.corporationID = ? AND s.security < 0
		 ORDER BY st.stationID LIMIT 1`, corpID).Scan(&stationID, &systemID)
	return stationID, systemID
}

// huntFrontOffice finds the front: an office the corporation holds within frontReachJumps of a held
// system, else an alliance-mate's station there, else the best station there by
// forwardStationScore, rented as essential. Answers the station and why, 0 when none was had.
func (m *Mod) huntFrontOffice(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, hq int64,
	u *universe, held map[int64]bool) (int64, string, error) {
	src := make(map[int64]int64, len(held))
	for sys := range held {
		src[sys] = 0
	}
	area, _ := platformReach(u, src, frontReachJumps)
	var systems []int64
	for sys := range area {
		systems = append(systems, sys)
	}
	sort.Slice(systems, func(i, j int) bool { return systems[i] < systems[j] })
	// An office already held out there.
	if rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, s.solarSystemID FROM crpoffices o JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ? ORDER BY o.stationID`, c.CorpID); err == nil {
		var best int64
		bestD := 1 << 30
		for rows.Next() {
			var st, sys int64
			if rows.Scan(&st, &sys) == nil {
				if d, ok := area[sys]; ok && d < bestD {
					best, bestD = st, d
				}
			}
		}
		rows.Close()
		if best != 0 {
			return best, "an office it holds beside what its alliance holds", nil
		}
	}
	all, err := m.stationsIn(ctx, systems)
	if err != nil || len(all) == 0 {
		return 0, "", err
	}
	served, err := m.stationsWithService(ctx, systems, serviceCloning, serviceRepairFacilities)
	if err != nil {
		return 0, "", err
	}
	hasService := map[int64]bool{}
	for _, p := range served {
		hasService[p.StationID] = true
	}
	mates := map[int64]bool{}
	for _, id := range m.allianceMemberCorps(ctx, c.CorpID) {
		if id != c.CorpID {
			mates[id] = true
		}
	}
	npcNull := map[int64]bool{}
	if v := m.corpNullView(ctx, c.CorpID, hq); v != nil {
		npcNull = v.NPCNull
	}
	hqDist, _ := m.corpJumpMap(ctx, m.stationSystemID(ctx, hq))
	nearCache := map[int64]map[int64]int{}
	type cand struct {
		purposeStation
		in    forwardIn
		mate  bool
		score float64
	}
	var cands []cand
	for _, p := range all {
		near, ok := nearCache[p.SystemID]
		if !ok {
			near = map[int64]int{}
			for s, d := range bfsJumps(u, p.SystemID, frontReachJumps) {
				if held[s] {
					near[s] = d
				}
			}
			nearCache[p.SystemID] = near
		}
		if len(near) == 0 {
			continue
		}
		owner := evedb.StationOwnerCorp(ctx, m.db, p.StationID)
		cd := cand{purposeStation: p, mate: mates[owner],
			in: forwardIn{Near: near, HQJumps: hqDist[p.SystemID], Services: hasService[p.StationID],
				Contestable: p.Security < 0 && !npcNull[p.SystemID], Freeport: isFreeport(owner, p.Security)}}
		cd.score = forwardStationScore(cd.in)
		cands = append(cands, cd)
	}
	rank := func() {
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].mate != cands[j].mate {
				return cands[i].mate // an alliance-mate's own station first
			}
			if cands[i].score != cands[j].score {
				return cands[i].score > cands[j].score
			}
			return cands[i].StationID < cands[j].StationID
		})
	}
	rank()
	if len(cands) > frontHuntTop {
		cands = cands[:frontHuntTop]
	}
	for i := range cands {
		cands[i].in.Danger = m.towerDanger(ctx, c.CorpID, cands[i].SystemID, cands[i].Security, ch.PosDangerWeight)
		cands[i].score = forwardStationScore(cands[i].in)
	}
	rank()
	for _, cd := range cands {
		why := fmt.Sprintf("a front beside the %d system(s) its alliance holds within %d jumps", len(cd.in.Near), frontReachJumps)
		switch {
		case cd.mate:
			why = "a front at an alliance-mate's station beside what the alliance holds"
		case cd.in.Freeport:
			why = "a front at a CONCORD-held conquerable beside what the alliance holds: anyone may dock there, for now"
		}
		at := estateCandidate{StationID: cd.StationID, Name: cd.Name, RegionID: cd.RegionID, Security: cd.Security}
		rented, err := m.ceoTryRentOffice(ctx, c, facts, ch, at, false, true, why)
		if errors.Is(err, evedb.ErrNoFreeOffices) {
			continue
		}
		if err != nil {
			return 0, "", err
		}
		if rented {
			return cd.StationID, why, nil
		}
		return 0, "", nil // short of money: the hunt rests
	}
	return 0, "", nil
}

// openFront makes station the corporation's front: claims the purpose (and the mining office where
// it can refine), records it, and asks for an early posting pass.
func (m *Mod) openFront(ctx context.Context, c *corpCEO, station int64, why string) {
	bits := officeFront
	if m.stationOffers(ctx, station, serviceReprocessing) {
		bits |= officeMining
	}
	claimed := m.claimNewBits(ctx, c.CorpID, station, bits)
	m.saveCorpFront(ctx, c.CorpID, corpFront{Kind: frontKind, StationID: station,
		SystemID: m.stationSystemID(ctx, station), ClaimedBits: claimed, Since: clock.Unix()})
	if bits&officeMining != 0 {
		// The fleets mine out there now: their ore goes to the front, not back to empire.
		_ = m.pointFleetOre(ctx, c, station, "its front in 0.0, where the fleets now mine")
	}
	m.frontChanged(c.CorpID)
	m.ceoLog(ctx, c, powerEstate, "front", station, 0,
		"opened its front at %s (%s): the field arms stage out there", stationNameOf(ctx, m, station), why)
}

// keepFront is a standing front's upkeep: who lives there, and their shelf. For a day after the
// front's share steps up (frontShare) it asks for an early posting pass, so the step is posted
// without waiting for the weekly review.
func (m *Mod) keepFront(ctx context.Context, c *corpCEO, f corpFront) {
	if held := clock.Unix() - f.Since; f.Kind == frontKind && frontShare(held) != frontShare(held-86400) {
		requestSeatPass(c.CorpID)
	}
	n := m.refreshFrontResidents(ctx, c.CorpID, f.StationID)
	m.frontStock(ctx, c, f.StationID, n)
}

// retreatFront gives up a front or capital: the bits it claimed are taken back, so postings revert
// at the next pass ("its office is going" for a member based there).
func (m *Mod) retreatFront(ctx context.Context, c *corpCEO, f corpFront, why string) {
	for _, p := range purposeNames {
		if f.ClaimedBits&p.bit != 0 {
			m.unclaimOfficePurpose(ctx, c.CorpID, f.StationID, p.bit)
		}
	}
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_fronts WHERE corporationID = ?`, c.CorpID)
	if m.corpFleetOreStation(ctx, c.CorpID) == f.StationID {
		// The estate pass picks a drop-off nearer home again (ceoEstate).
		_ = m.setCorpFleetOreStation(ctx, c.CorpID, 0)
	}
	m.refreshFrontResidents(ctx, c.CorpID, 0)
	m.frontChanged(c.CorpID)
	m.ceoLog(ctx, c, powerEstate, f.Kind+".end", f.StationID, 0,
		"gave up its %s at %s: %s", f.Kind, stationNameOf(ctx, m, f.StationID), why)
}

// ceoCapital moves the corporation into a 0.0 station it owns: a free owner's office, every purpose
// the station's services allow, and the factory. The headquarters stays where it is: a conquerable
// is office space, never a headquarters, and the empire home stays the fallback and the market.
// The capital is home by attendance instead (stageQuotas posts every arm but sales there). A
// standing capital only keeps its residents and shelf. Returns the headquarters.
func (m *Mod) ceoCapital(ctx context.Context, c *corpCEO, facts *CorpFacts, hq int64, cur corpFront, station, sys int64) (int64, error) {
	if cur.Kind == capitalKind && cur.StationID == station {
		m.keepFront(ctx, c, cur)
		return hq, nil
	}
	if cur.Kind != "" {
		m.retreatFront(ctx, c, cur, fmt.Sprintf("it owns %s now", stationNameOf(ctx, m, station)))
	}
	if _, err := m.ceoOpenOwnerOffice(ctx, c, station, "a station it owns in 0.0"); err != nil {
		return hq, err
	}
	if m.officeAt(ctx, c.CorpID, station) == 0 {
		return hq, nil // no office slot yet: the next think tries again
	}
	bits := officeCapital | officeFront | officeStaging
	if m.stationOffers(ctx, station, serviceReprocessing) {
		bits |= officeMining
	}
	if m.stationOffers(ctx, station, serviceLaboratory) {
		bits |= officeLab
	}
	claimed := m.claimNewBits(ctx, c.CorpID, station, bits)
	m.saveCorpFront(ctx, c.CorpID, corpFront{Kind: capitalKind, StationID: station, SystemID: sys,
		ClaimedBits: claimed, Since: clock.Unix()})
	if bits&officeMining != 0 {
		_ = m.pointFleetOre(ctx, c, station, "its capital in 0.0")
	}
	name := stationNameOf(ctx, m, station)
	m.ceoLog(ctx, c, powerEstate, "capital.move", station, 0,
		"moves the whole corporation to %s, a station it owns in 0.0; its headquarters stays in empire", name)
	if m.stationHasLine(ctx, station) {
		old := m.corpFactoryStation(ctx, c.CorpID)
		if old != station {
			site := facilitySite{estateCandidate: estateCandidate{StationID: station, Name: name}, SystemID: sys}
			if err := m.pinFactory(ctx, c, old, site, "its capital in 0.0"); err != nil {
				return hq, err
			}
		}
	}
	m.frontChanged(c.CorpID)
	return hq, nil
}

// frontChanged tells what reads the front that it moved: the 0.0 view, the addresses, and the
// posting pass, asked to run early.
func (m *Mod) frontChanged(corpID int64) {
	forgetNullView(corpID)
	forgetCorpAnchors(corpID)
	requestSeatPass(corpID)
}

// claimNewBits claims the bits an office does not already carry and answers those, so a retreat
// takes back only what the front added.
func (m *Mod) claimNewBits(ctx context.Context, corpID, station int64, bits int) int {
	var had int
	_ = m.db.QueryRowContext(ctx, `
		SELECT claimed FROM rolebot_corp_offices WHERE corporationID = ? AND stationID = ?`,
		corpID, station).Scan(&had)
	added := bits &^ had
	for _, p := range purposeNames {
		if added&p.bit != 0 {
			m.claimOfficePurpose(ctx, corpID, station, p.bit)
		}
	}
	return added
}

// saveCorpFront stores the corporation's front or capital.
func (m *Mod) saveCorpFront(ctx context.Context, corpID int64, f corpFront) {
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_fronts (corporationID, kind, stationID, systemID, claimedBits, since)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE policy = IF(stationID = VALUES(stationID), policy, ''),
		                        kind = VALUES(kind), stationID = VALUES(stationID), systemID = VALUES(systemID),
		                        claimedBits = VALUES(claimedBits), since = VALUES(since)`,
		corpID, f.Kind, f.StationID, f.SystemID, f.ClaimedBits, f.Since); err != nil {
		rlogf("rolebots: save front %d: %v", corpID, err)
	}
}

// stationOffers reports whether a station offers a service (staOperationServices).
func (m *Mod) stationOffers(ctx context.Context, station int64, service int) bool {
	sys := m.stationSystemID(ctx, station)
	if sys == 0 {
		return false
	}
	list, err := m.stationsWithService(ctx, []int64{sys}, service)
	if err != nil {
		return false
	}
	for _, p := range list {
		if p.StationID == station {
			return true
		}
	}
	return false
}

// ── the shelf ───────────────────────────────────────────────────────────────

// frontStock keeps the charges the front's pilots carry on the front office's Production shelf,
// where ceoOutput lists them (corpstore.go): the frontStockTypes most carried, half again what the
// residents hold of each. Bought through requisitions (posreq.go); every frontStockEverySec.
func (m *Mod) frontStock(ctx context.Context, c *corpCEO, station int64, residents int) {
	if residents < frontStockMinPilots || clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "frontstock") < frontStockEverySec {
		return
	}
	office := m.officeAt(ctx, c.CorpID, station)
	if office == 0 {
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		  JOIN chrCorporationRoles r ON r.characterID = e.ownerID
		 WHERE r.corporationID = ? AND r.baseID = ? AND g.categoryID = ? AND e.quantity > 0
		 GROUP BY e.typeID ORDER BY SUM(e.quantity) DESC LIMIT ?`,
		c.CorpID, station, catCharge, frontStockTypes)
	if err != nil {
		return
	}
	type line struct{ typeID, qty int64 }
	var lines []line
	for rows.Next() {
		var l line
		if rows.Scan(&l.typeID, &l.qty) == nil {
			lines = append(lines, l)
		}
	}
	rows.Close()
	shelf := m.divisionHeld(ctx, c.CorpID, office, corpDivProduct)
	posted := 0
	for _, l := range lines {
		want := l.qty/2 - shelf[l.typeID] - m.reqPending(ctx, c.CorpID, l.typeID, station, corpDivProduct)
		if want <= 0 {
			continue
		}
		if _, err := m.postRequisition(ctx, c, requisition{TypeID: l.typeID, Qty: want, DestStationID: station,
			DestFlag: corpDivProduct, Requester: "ceo", RefKind: "front", Deadline: clock.Unix() + 3*86400,
			Note: "charges for the front"}); err == nil {
			posted++
		}
	}
	m.ceoLog(ctx, c, powerEstate, "frontstock", station, float64(posted),
		"looked at the front's charge shelf at %s for %d resident(s): %d line(s) asked for",
		stationNameOf(ctx, m, station), residents, posted)
	// A station with no market (a conquerable) cannot sell the shelf: it is handed out instead.
	if !evedb.StationHasService(ctx, m.db, station, evedb.ServiceMarket) {
		types := make([]int64, 0, len(lines))
		for _, l := range lines {
			types = append(types, l.typeID)
		}
		m.issueFrontShelf(ctx, c, station, office, types, residents)
	}
}

// frontShelfHangar is the item flag a pilot's station hangar uses.
const frontShelfHangar = 4

// frontShareOf is what one resident is topped up to from the shelf: an equal share of what the
// corporation holds there, never more than what the pilot would carry on average (avg). Pure.
func frontShareOf(shelf int64, residents int, avg int64) int64 {
	if shelf <= 0 || residents <= 0 {
		return 0
	}
	return min(shelf/int64(residents), avg)
}

// issueFrontShelf hands the charge shelf of a market-less front to the residents docked there: each
// is topped up toward the residents' average holding of each type, from the corporation's Production
// division, so pilots living at a conquerable restock without a market.
func (m *Mod) issueFrontShelf(ctx context.Context, c *corpCEO, station, office int64, types []int64, residents int) {
	pilots, err := int64Column(ctx, m, `
		SELECT ch.characterID FROM character_ ch
		  JOIN chrCorporationRoles r ON r.characterID = ch.characterID
		 WHERE r.corporationID = ? AND r.baseID = ? AND ch.stationID = ?`, c.CorpID, station, station)
	if err != nil || len(pilots) == 0 {
		return
	}
	issued := 0
	for _, typeID := range types {
		var total int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(e.quantity), 0) FROM entity e
			  JOIN chrCorporationRoles r ON r.characterID = e.ownerID
			 WHERE r.corporationID = ? AND r.baseID = ? AND e.typeID = ?`, c.CorpID, station, typeID).Scan(&total)
		avg := total / int64(max(residents, 1))
		for _, charID := range pilots {
			shelf := m.divisionHeld(ctx, c.CorpID, office, corpDivProduct)[typeID]
			var have int64
			_ = m.db.QueryRowContext(ctx, `
				SELECT COALESCE(SUM(quantity), 0) FROM entity WHERE ownerID = ? AND locationID = ? AND typeID = ?`,
				charID, station, typeID).Scan(&have)
			want := frontShareOf(shelf, len(pilots), avg) - have
			if want <= 0 {
				continue
			}
			if m.moveShelfToPilot(ctx, c.CorpID, office, typeID, want, charID, station) > 0 {
				issued++
			}
		}
	}
	if issued > 0 {
		m.ceoLog(ctx, c, powerEstate, "frontissue", station, float64(issued),
			"handed out the charge shelf at %s: %d issue(s) to residents (no market out here)",
			stationNameOf(ctx, m, station), issued)
	}
}

// moveShelfToPilot moves up to qty of a type from the corporation's Production division at an
// office into a pilot's hangar at the station, splitting the last stack. Returns the units moved.
func (m *Mod) moveShelfToPilot(ctx context.Context, corpID, office, typeID, qty, charID, station int64) int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		 WHERE ownerID = ? AND locationID = ? AND flag = ? AND typeID = ? AND singleton = 0
		 ORDER BY quantity`, corpID, office, corpDivProduct, typeID)
	if err != nil {
		return 0
	}
	type stack struct{ id, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if rows.Scan(&s.id, &s.qty) == nil {
			stacks = append(stacks, s)
		}
	}
	rows.Close()
	moved := int64(0)
	for _, s := range stacks {
		if moved >= qty {
			break
		}
		id := s.id
		take := min(s.qty, qty-moved)
		if take < s.qty {
			var err error
			if id, err = evedb.SplitStackN(ctx, m.db, s.id, take); err != nil {
				return moved
			}
		}
		if _, err := evedb.MoveItemFollowingContents(ctx, m.db, id, station, charID, frontShelfHangar); err != nil {
			return moved
		}
		moved += take
	}
	return moved
}
