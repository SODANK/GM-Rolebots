package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"gurimoon/corp"
	evedb "gurimoon/db"
)

// Placement: who is in which arm (corparm.go), decided on the CEO's roles pass (ceoRoles) after
// every position is dealt. Three steps, the first two pure:
//
//  1. Justify (armJustified): an arm needs enough members and work. Below ceoHierarchyFrom the
//     CEO runs everything.
//  2. Target (armTargets): each justified line arm gets a share of the membership.
//  3. Deal (dealArms): position holders go to their arm; placed members stay unless their arm
//     is unjustified or over target past armKeepSec; the rest go where the deficit is deepest.
//
// Humans are never dealt; a human holding a position is recorded in its arm and keeps it.

const (
	// armKeepSec: a placed member is not moved for target balance until it has served this long.
	armKeepSec = 7 * 86400
	// armTargetSlack: an arm may run this many over target before anybody is moved out of it.
	armTargetSlack = 1
	// armFillKeepSec and armPullPrefer: an arm under target may pull an incumbent who has served
	// this long, not armKeepSec, if the member's fit for the new work is armPullPrefer times its
	// fit for the old. The old arm is never pulled under its own target.
	armFillKeepSec = 2 * 86400
	armPullPrefer  = 1.5
	// armFinanceWalletISK: a corporation keeps a Finance arm only with money worth an analyst.
	armFinanceWalletISK = 50e6
	// armHRApps and armHROpenings: work for a Personnel department, applications waiting or room.
	armHRApps     = 3
	armHROpenings = 5
	// armAuxIndustryFrac caps the Industry arm of a corporation that is not an industrial house
	// (no Industry Director, no industry tag) at this share of its membership. It holds the even
	// share only; a combat house's Sales trim (trimAuxSales) is added on top.
	armAuxIndustryFrac = 0.10
	// armAuxSalesKeep, armAuxSalesMin and armAuxSalesFocusShare: outside a trade house
	// (tradeDeskTagged) Sales keeps armAuxSalesKeep of its target, never under armAuxSalesMin
	// (purposeArmMin for ceoTradingPost). The rest goes to the focus arm (armFocus) and the second.
	armAuxSalesKeep       = 0.25
	armAuxSalesMin        = 2
	armAuxSalesFocusShare = 2.0 / 3
)

// armMinMembers is the membership at which an arm can be staffed at all.
var armMinMembers = map[corpArm]int{
	armSales: 6,
	// Infrastructure is small and early: one tower needs its fuel run whatever the size.
	armInfrastructure: 4,
	armSecurity:       8,
	armMining:         8,
	armIndustry:       10,
	armResearch:       10,
	armFinance:        12,
	armHR:             20,
}

// armSignals is everything justification and targets read. Gathered once per pass
// (gatherArmSignals); pure from there.
type armSignals struct {
	Members      int // the whole membership (CorpFacts.Members)
	Tags         []string
	Directive    map[string]float64 // activity → share, the CEO's directive
	Demand       map[string]float64 // portfolio → 0..1, the department heads'
	War          bool
	HasFactory   bool
	IndDirector  bool
	OpenApps     int
	FreeOpenings int64
	Unlimited    bool
	Wallet       float64
	// Towers is the corporation's active tower plans, planned to running.
	Towers int
}

// Tag sets justification reads (hasAnyTag, corpdoctrine.go).
var (
	armMiningTags = map[string]bool{"mining": true}
	// R&D exists where the corporation says it is a laboratory.
	armResearchTags = map[string]bool{"research": true}
	armSalesTags    = map[string]bool{"trade": true, "retail": true, "hauling": true, "industry": true, "shipbuilding": true}
)

func (s armSignals) directiveShare(a corpArm) float64 {
	v := 0.0
	for _, act := range armActivities(a) {
		v += s.Directive[act]
	}
	return v
}

// armJustified reports whether a corporation has the people and the work for an arm. Pure.
func armJustified(a corpArm, s armSignals) bool {
	if s.Members < ceoHierarchyFrom || s.Members < armMinMembers[a] {
		return false
	}
	switch a {
	case armIndustry:
		return s.HasFactory || s.IndDirector || industryTagged(s.Tags) || s.directiveShare(a) > 0
	case armResearch:
		// A laboratory needs the research tag and a place to build: a factory, an Industry Director
		// or the industry tag.
		return hasAnyTag(s.Tags, armResearchTags) &&
			(s.HasFactory || s.IndDirector || industryTagged(s.Tags))
	case armMining:
		return hasAnyTag(s.Tags, armMiningTags) || s.directiveShare(a) > 0 || s.Demand[portOperations] > 0.2
	case armSecurity:
		return doctrineFromTags(s.Tags) != DoctrineNone || s.War || s.directiveShare(a) > 0 ||
			s.Demand[portSecurity] > 0.2
	case armSales:
		return hasAnyTag(s.Tags, armSalesTags) || s.HasFactory || s.directiveShare(a) > 0
	case armInfrastructure:
		return s.Towers > 0
	case armHR:
		return s.OpenApps >= armHRApps || s.Unlimited || s.FreeOpenings >= armHROpenings
	case armFinance:
		return s.Wallet >= armFinanceWalletISK
	}
	return false
}

// justifiedArms is every justified arm, in armOrder.
func justifiedArms(s armSignals) []corpArm {
	var out []corpArm
	for _, a := range armOrder {
		if armJustified(a, s) {
			out = append(out, a)
		}
	}
	return out
}

// industryCore reports whether a corporation is an industrial house: an Industry Director or
// an industry-type tag. Any other Industry arm is held to armAuxIndustryFrac (armTargets).
func industryCore(s armSignals) bool {
	return s.IndDirector || industryTagged(s.Tags)
}

// armTargets shares placeable rank-and-file members across the justified line arms. Pure. An
// arm's weight is 1 + 3 x its directive share + 2 x its head's demand (a laboratory takes part
// of Industry's directive, researchDirectiveSplit). Security is capped at the combat wing's
// fraction and an auxiliary Industry arm at armAuxIndustryFrac; trimAuxSales runs last.
func armTargets(s armSignals, justified []corpArm, placeable int) map[corpArm]int {
	out := armEvenTargets(s, justified, placeable)
	trimAuxSales(s, justified, out)
	return out
}

// armEvenTargets is armTargets before the Sales trim. Pure.
func armEvenTargets(s armSignals, justified []corpArm, placeable int) map[corpArm]int {
	out := map[corpArm]int{}
	var arms []corpArm
	weight := map[corpArm]float64{}
	total := 0.0
	split := 0.0
	for _, a := range justified {
		if a == armResearch {
			split = researchDirectiveSplit(s.Tags)
		}
	}
	for _, a := range justified {
		if len(armActivities(a)) == 0 {
			continue
		}
		// Industry has no Director-portfolio demand (its head's desk measures nothing into
		// deptDemand), so it is weighted by base and directive alone.
		w := 1 + 3*s.directiveShare(a) + 2*s.Demand[portfolioOfArm(a)]
		switch a {
		case armIndustry:
			w -= 3 * s.directiveShare(armIndustry) * split
		case armResearch:
			w += 3 * s.directiveShare(armIndustry) * split
			// A corporation that leads with the research tag is a laboratory first: R&D gets more than an
			// even split.
			if len(s.Tags) > 0 && s.Tags[0] == "research" {
				w += 1.5
			}
		}
		arms = append(arms, a)
		weight[a] = w
		total += w
	}
	if len(arms) == 0 || placeable <= 0 {
		return out
	}
	// Capped arms take their share first, up to the cap, in a fixed order; what a cap holds back
	// goes to the uncapped arms.
	caps := map[corpArm]int{}
	if weight[armSecurity] > 0 {
		secCap := int(math.Floor(wingFracFromTags(s.Tags) * float64(s.Members)))
		if s.War {
			secCap = int(math.Ceil(float64(secCap) * (1 + warSurgeFrac)))
		}
		caps[armSecurity] = secCap
	}
	if weight[armIndustry] > 0 && !industryCore(s) {
		caps[armIndustry] = max(1, int(math.Floor(armAuxIndustryFrac*float64(s.Members))))
	}
	if weight[armInfrastructure] > 0 {
		caps[armInfrastructure] = infraArmCap(s.Towers)
	}
	remaining := placeable
	for _, a := range []corpArm{armSecurity, armIndustry, armInfrastructure} {
		limit, ok := caps[a]
		if !ok {
			continue
		}
		want := int(math.Round(float64(remaining) * weight[a] / total))
		got := min(want, limit, remaining)
		out[a] = got
		remaining -= got
		total -= weight[a]
	}
	// Largest remainder over the other arms, so the targets sum exactly.
	type share struct {
		a    corpArm
		frac float64
	}
	var rest []share
	given := 0
	for _, a := range arms {
		if _, capped := caps[a]; capped {
			continue
		}
		if total <= 0 {
			break
		}
		exact := float64(remaining) * weight[a] / total
		n := int(math.Floor(exact))
		out[a] = n
		given += n
		rest = append(rest, share{a, exact - float64(n)})
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].frac > rest[j].frac })
	for i := 0; given < remaining && len(rest) > 0; i = (i + 1) % len(rest) {
		out[rest[i].a]++
		given++
	}
	return out
}

// trimAuxSales shrinks the Sales target of a corporation that is not a trade house to
// armAuxSalesKeep of itself and hands the rest to the focus and second arms (armFocus), 2:1.
// Pure; the targets keep their sum. Sales is justified by any factory office and is the only
// uncapped line arm, so cap overflow lands there. The transfer goes past both caps.
func trimAuxSales(s armSignals, justified []corpArm, out map[corpArm]int) {
	if tradeDeskTagged(s.Tags) || out[armSales] <= armAuxSalesMin {
		return
	}
	focus, second := armFocus(s.Tags, justified, out)
	if focus == "" && second == "" {
		return
	}
	keep := max(armAuxSalesMin, int(math.Round(armAuxSalesKeep*float64(out[armSales]))))
	freed := out[armSales] - keep
	if freed <= 0 {
		return
	}
	toFocus := int(math.Round(float64(freed) * armAuxSalesFocusShare))
	switch {
	case second == "":
		toFocus = freed
	case focus == "":
		toFocus = 0
	}
	out[armSales] = keep
	if toFocus > 0 {
		out[focus] += toFocus
	}
	if freed-toFocus > 0 {
		out[second] += freed - toFocus
	}
}

// armFocusTags maps a tag to the arm it makes a corporation's focus.
var armFocusTags = map[string]corpArm{
	tagCombat: armSecurity, tagSecurity: armSecurity, tagMercenary: armSecurity, tagOutlaw: armSecurity,
	"research": armResearch,
	"industry": armIndustry, "shipbuilding": armIndustry, "cartelindustry": armIndustry,
	"construction": armIndustry, "chemical": armIndustry,
	"mining": armMining,
}

// armFocus is the arm a corporation's spare Sales hands go to first, and the one that takes
// the rest. The first tag naming an arm decides; the second is Industry (Mining for an
// industrial house). With no such tag the focus is the biggest justified line arm that is not
// Sales or Infrastructure. An unjustified arm comes back "". Pure.
func armFocus(tags []string, justified []corpArm, out map[corpArm]int) (focus, second corpArm) {
	ok := map[corpArm]bool{}
	for _, a := range justified {
		ok[a] = true
	}
	for _, t := range tags {
		if a, hit := armFocusTags[t]; hit {
			focus = a
			break
		}
	}
	if focus == "" {
		best := -1
		for _, a := range lineArms {
			if !ok[a] || a == armSales || a == armInfrastructure {
				continue
			}
			if out[a] > best {
				focus, best = a, out[a]
			}
		}
	}
	second = armIndustry
	if focus == armIndustry {
		second = armMining
	}
	if !ok[focus] {
		focus = ""
	}
	if !ok[second] || second == focus {
		second = ""
	}
	return focus, second
}

// researchDirectiveSplit is the share of the industry directive a laboratory's R&D arm takes
// from its Industry arm (research is an industry trip, industrytrip.go). Pure. A laboratory
// splits it evenly; research beside another lead tag gives R&D a quarter.
func researchDirectiveSplit(tags []string) float64 {
	switch {
	case len(tags) > 0 && tags[0] == "research":
		return 0.5
	case hasAnyTag(tags, armResearchTags):
		return 0.25
	}
	return 0
}

// infraArmCap is the most rank-and-file Infrastructure keeps: a crew of two and a hand and a
// half per tower. Its work is legs, and a tower posts a few a week. Pure.
func infraArmCap(towers int) int {
	if towers <= 0 {
		return 0
	}
	return 2 + int(math.Ceil(1.5*float64(towers)))
}

// armCand is one member as the deal reads it.
type armCand struct {
	CharID  int64
	Human   bool
	Fit     map[corpArm]float64 // how much it wants each line arm's work
	Current corpArm
	Since   int64
	// Pinned is the arm a position puts the member in, and PinTier its tier there; "" if none.
	Pinned  corpArm
	PinTier int
	// Lift is the biggest single unit the member can carry (haullift.go), which staffs
	// Infrastructure. 0 = unknown.
	Lift float64
}

// armPlacement is the deal's verdict on one member.
type armPlacement struct {
	Arm  corpArm
	Tier int
}

// dealArms places every member. Pure; see the file comment for the rules.
func dealArms(cands []armCand, justified []corpArm, targets map[corpArm]int, now int64) map[int64]armPlacement {
	out := map[int64]armPlacement{}
	ok := map[corpArm]bool{}
	for _, a := range justified {
		ok[a] = true
	}
	count := map[corpArm]int{}
	sorted := append([]armCand(nil), cands...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CharID < sorted[j].CharID })

	// 1. Positions. A position's arm need not be justified to be recorded.
	for _, c := range sorted {
		if c.Pinned != "" {
			out[c.CharID] = armPlacement{c.Pinned, c.PinTier}
			if c.PinTier == armTierMember {
				count[c.Pinned]++
			}
		}
	}
	// 1a. Infrastructure by lift: its rank-and-file seats go to the members who can carry the
	// most, whatever arm they stand in. Other seat holders return to the deal.
	liftSeated := infraByLift(sorted, out, ok, targets, count)
	for id := range liftSeated {
		out[id] = armPlacement{armInfrastructure, armTierMember}
		count[armInfrastructure]++
	}
	// 2. Incumbents, strongest fit first so the weakest are the ones who move.
	var inc []armCand
	for _, c := range sorted {
		if _, done := out[c.CharID]; done || c.Current == "" || !ok[c.Current] {
			continue
		}
		if liftSeated != nil && c.Current == armInfrastructure && !c.Human {
			continue // outlifted: free for the deal
		}
		if c.Human || len(armActivities(c.Current)) == 0 {
			out[c.CharID] = armPlacement{c.Current, armTierMember}
			continue
		}
		inc = append(inc, c)
	}
	// 2a. Vacancies pull: an arm short of its target takes incumbents who would rather be there
	// (armFillKeepSec, armPullPrefer).
	for _, c := range pullIntoVacancies(inc, ok, targets, count, now) {
		out[c.CharID] = armPlacement{c.Current, armTierMember}
		count[c.Current]++
	}
	kept := inc[:0]
	for _, c := range inc {
		if _, moved := out[c.CharID]; !moved {
			kept = append(kept, c)
		}
	}
	inc = kept
	sort.SliceStable(inc, func(i, j int) bool { return inc[i].Fit[inc[i].Current] > inc[j].Fit[inc[j].Current] })
	for _, c := range inc {
		a := c.Current
		if count[a] >= targets[a]+armTargetSlack && now-c.Since >= armKeepSec {
			continue // over target and long enough served: free to move
		}
		out[c.CharID] = armPlacement{a, armTierMember}
		count[a]++
	}
	// 3. Everybody else: the deepest deficit, weighted by fit.
	var lines []corpArm
	for _, a := range lineArms {
		if ok[a] {
			lines = append(lines, a)
		}
	}
	if len(lines) == 0 {
		return out
	}
	var free []armCand
	for _, c := range sorted {
		if _, done := out[c.CharID]; done || c.Human {
			continue
		}
		free = append(free, c)
	}
	// The most decided pilots choose first.
	sort.SliceStable(free, func(i, j int) bool { return maxFit(free[i].Fit, lines) > maxFit(free[j].Fit, lines) })
	for _, c := range free {
		var best corpArm
		bestScore := math.Inf(-1)
		for _, a := range lines {
			deficit := float64(targets[a]-count[a]) / float64(max(1, targets[a]))
			score := c.Fit[a] * (1 + deficit)
			if deficit <= 0 {
				score = c.Fit[a] * 0.25 * math.Pow(0.5, float64(count[a]-targets[a]))
			}
			if score > bestScore {
				best, bestScore = a, score
			}
		}
		out[c.CharID] = armPlacement{best, armTierMember}
		count[best]++
	}
	return out
}

// infraLiftKeep is the incumbent's edge in the lift ranking: a challenger must out-carry a
// seated member by more than this, so near-equal pilots do not trade the seat each pass.
const infraLiftKeep = 1.05

// infraByLift picks Infrastructure's rank-and-file seats by lift: the unplaced non-human
// members who can carry the most, a seated member scored up by infraLiftKeep, temperament
// (Fit) breaking ties. Nil when the arm is unjustified, has no free seat, or no lift is known.
// Pure.
func infraByLift(sorted []armCand, placed map[int64]armPlacement, ok map[corpArm]bool,
	targets, count map[corpArm]int) map[int64]bool {
	seats := targets[armInfrastructure] - count[armInfrastructure]
	if !ok[armInfrastructure] || seats <= 0 {
		return nil
	}
	type cand struct {
		c     armCand
		score float64
	}
	var pool []cand
	known := false
	for _, c := range sorted {
		if _, done := placed[c.CharID]; done || c.Human {
			continue
		}
		if c.Lift > 0 {
			known = true
		}
		s := c.Lift
		if c.Current == armInfrastructure {
			s *= infraLiftKeep
		}
		pool = append(pool, cand{c, s})
	}
	if !known {
		return nil
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].score != pool[j].score {
			return pool[i].score > pool[j].score
		}
		return pool[i].c.Fit[armInfrastructure] > pool[j].c.Fit[armInfrastructure]
	})
	out := map[int64]bool{}
	for _, p := range pool {
		if len(out) >= seats || p.c.Lift <= 0 {
			break
		}
		out[p.c.CharID] = true
	}
	return out
}

// pullIntoVacancies picks incumbents to move into arms below target. Pure. Each answer carries
// its new arm in Current. base is the head count of positions already placed.
func pullIntoVacancies(inc []armCand, ok map[corpArm]bool, targets, base map[corpArm]int, now int64) []armCand {
	count := map[corpArm]int{}
	for a, n := range base {
		count[a] = n
	}
	for _, c := range inc {
		count[c.Current]++
	}
	var out []armCand
	moved := map[int64]bool{}
	for _, a := range lineArms {
		if !ok[a] || count[a] >= targets[a] {
			continue
		}
		var cands []armCand
		for _, c := range inc {
			if moved[c.CharID] || c.Current == a || now-c.Since < armFillKeepSec || c.Fit[a] <= c.Fit[c.Current] {
				continue
			}
			cands = append(cands, c)
		}
		// The strongest preference moves first.
		sort.SliceStable(cands, func(i, j int) bool {
			return cands[i].Fit[a]/math.Max(0.01, cands[i].Fit[cands[i].Current]) >
				cands[j].Fit[a]/math.Max(0.01, cands[j].Fit[cands[j].Current])
		})
		for _, c := range cands {
			if count[a] >= targets[a] {
				break
			}
			src := c.Current
			if count[src]-1 < targets[src] && c.Fit[a] < armPullPrefer*c.Fit[src] {
				continue
			}
			moved[c.CharID] = true
			count[src]--
			count[a]++
			c.Current = a
			out = append(out, c)
		}
	}
	return out
}

func maxFit(fit map[corpArm]float64, arms []corpArm) float64 {
	v := 0.0
	for _, a := range arms {
		v = math.Max(v, fit[a])
	}
	return v
}

// armFit is how much a pilot wants each line arm's work: the mean of its dispositions over
// the arm's activities. Pure.
func armFit(tn Tuning) map[corpArm]float64 {
	drive := map[string]float64{}
	for _, def := range activityCatalog {
		drive[def.Slug] = def.Disposition(tn)
	}
	// Research is a trip, not a catalogue entry (industrytrip.go): its key is the career appetite,
	// which sorts pilots into the laboratory by temperament.
	drive[ActResearch] = tn.ResearchAppetite
	// Tower work is hauling to a schedule and rigging structures: couriers and industrialists
	// both answer it.
	drive[armStarbaseKey] = (drive[ActCourier] + drive[ActIndustry]) / 2
	out := map[corpArm]float64{}
	for _, a := range lineArms {
		acts := armActivities(a)
		v := 0.0
		for _, act := range acts {
			v += drive[act]
		}
		out[a] = math.Max(0.01, v/float64(len(acts)))
	}
	return out
}

// ── the pass ────────────────────────────────────────────────────────────────

// armTargetsMemo is the last pass's targets per corporation, for the board.
var (
	armMemoMu sync.Mutex
	armMemo   = map[int64]armMemoRow{}
)

type armMemoRow struct {
	Justified []corpArm
	Targets   map[corpArm]int
	At        int64
}

func armMemoOf(corpID int64) (armMemoRow, bool) {
	armMemoMu.Lock()
	defer armMemoMu.Unlock()
	r, ok := armMemo[corpID]
	return r, ok
}

// gatherArmSignals reads what justification and targets need.
func (m *Mod) gatherArmSignals(ctx context.Context, c *corpCEO, facts *CorpFacts) armSignals {
	s := armSignals{Members: facts.Members, Tags: facts.Policy.Tags}
	s.Directive, _, _ = m.loadCorpDirective(ctx, c.CorpID)
	s.Demand, s.War = corpSeatDemand(c.CorpID)
	s.HasFactory = m.corpFactoryStation(ctx, c.CorpID) != 0
	s.IndDirector = m.corpDirectorOf(ctx, c.CorpID) != 0
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM chrApplications WHERE corporationID = ? AND deleted = 0 AND status = 0`,
		c.CorpID).Scan(&s.OpenApps)
	s.FreeOpenings, s.Unlimited, _ = m.corpFreeOpenings(ctx, c.CorpID)
	s.Wallet, _ = evedb.GetCorpBalance(ctx, m.db, c.CorpID)
	if StarbasePower() {
		if plans, err := m.corpTowerPlans(ctx, c.CorpID, true); err == nil {
			s.Towers = len(plans)
		}
	}
	return s
}

// armPositions is the arm and tier each position puts its holder in. Heads first: a member
// holding several positions is placed by the highest.
func (m *Mod) armPositions(ctx context.Context, corpID int64) map[int64]armPlacement {
	out := map[int64]armPlacement{}
	put := func(id int64, a corpArm, tier int) {
		if id == 0 || a == "" {
			return
		}
		if cur, ok := out[id]; ok && cur.Tier >= tier {
			return
		}
		out[id] = armPlacement{a, tier}
	}
	staff := m.loadStaffRows(ctx, corpID)
	for id, r := range staff {
		put(id, armOfPortfolio(r.Portfolio), armTierHead)
	}
	if p, err := m.loadCorpPolicy(ctx, corpID); err == nil {
		put(p.DirectorID, armIndustry, armTierHead)
		put(p.DeputyID, armIndustry, armTierJunior)
	}
	// The Treasurer and the Personnel Managers, off their role bits.
	if rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, COALESCE(r.corprole, 0)
		  FROM character_ ch JOIN chrCorporationRoles r ON r.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0`, corpID); err == nil {
		for rows.Next() {
			var id int64
			var roles uint64
			if rows.Scan(&id, &roles) != nil {
				continue
			}
			switch officerPost(roles) {
			case postTreasurer:
				put(id, armFinance, armTierHead)
			case postPersonnel:
				put(id, armHR, armTierJunior)
			}
		}
		rows.Close()
	}
	if ranks, err := m.listCorpRanks(ctx, corpID); err == nil {
		for _, r := range ranks {
			put(r.CharID, armOfSeatActivity(r.Activity), armTierJunior)
		}
	}
	for id, r := range m.indSeatsOf(ctx, corpID) {
		switch r.Seat {
		case seatAdmin:
			put(id, armIndustry, armTierJunior)
		case seatSales:
			put(id, armSales, armTierJunior)
		}
	}
	// The Fuel Technicians are Infrastructure's juniors; the Procurement Officer buys for the
	// whole corporation and stays in Sales.
	for id, seat := range m.posSeatsOf(ctx, corpID) {
		if seat == seatFueler {
			put(id, armInfrastructure, armTierJunior)
		} else {
			put(id, armSales, armTierJunior)
		}
	}
	return out
}

// ceoArms is the placement pass, run by ceoRoles with the signals it already gathered.
func (m *Mod) ceoArms(ctx context.Context, c *corpCEO, sig armSignals, justified []corpArm) error {
	members, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return err
	}
	cur, err := m.loadArmRows(ctx, c.CorpID)
	if err != nil {
		return err
	}
	pos := m.armPositions(ctx, c.CorpID)
	benched := m.benchedBuilders(ctx, c.CorpID)

	var cands []armCand
	placeable := 0
	inCorp := map[int64]corpMember{}
	onBadge := map[int64]corpMember{}
	for _, cm := range members {
		onBadge[cm.CharID] = cm
		if !cm.Eligible() {
			continue
		}
		inCorp[cm.CharID] = cm
		ac := armCand{CharID: cm.CharID, Human: cm.Human()}
		if r, ok := cur[cm.CharID]; ok {
			ac.Current, ac.Since = r.Arm, r.Since
		}
		if p, ok := pos[cm.CharID]; ok {
			ac.Pinned, ac.PinTier = p.Arm, p.Tier
		}
		if !ac.Human {
			ac.Fit = armFit(m.memberIdentity(ctx, cm).Tune)
			if sig.Towers > 0 {
				ac.Lift = m.liftOf(ctx, cm.CharID)
			}
			if ac.Pinned == "" || ac.PinTier == armTierMember {
				placeable++
			}
		}
		cands = append(cands, ac)
	}
	targets := armTargets(sig, justified, placeable)
	deal := dealArms(cands, justified, targets, clock.Unix())

	armMemoMu.Lock()
	armMemo[c.CorpID] = armMemoRow{Justified: justified, Targets: targets, At: clock.Unix()}
	armMemoMu.Unlock()

	assigner := c.CharID
	if s := m.staffFor(ctx, c); s != nil {
		if h := s.head(portHR); h != nil {
			assigner = h.CharID
		}
	}
	now := clock.Unix()
	var news []string
	for id, cm := range inCorp {
		next, placed := deal[id]
		prev, had := cur[id]
		if had && prev.SetBy == postSetHuman {
			// The chair placed them (corpcommanddesk_org.go), or a human member chose the arm by mail
			// (corparm_human.go); the deal leaves it.
			continue
		}
		switch {
		case !placed && had:
			if _, err := m.applyArmRoles(ctx, cm, prev.GrantMask, armPlacement{}, false); err != nil {
				return err
			}
			m.deleteArmRow(ctx, id)
			m.ceoLog(ctx, c, powerRoles, "arm", id, 0, "released %s from %s to the pool", cm.Name, armLabel(prev.Arm))
		case placed:
			row := prev
			if !had {
				row = armRow{CharID: id, CorpID: c.CorpID}
			}
			moved := !had || prev.Arm != next.Arm
			if moved {
				row.Since, row.AssignedBy = now, assigner
			}
			row.CorpID, row.Arm, row.Tier = c.CorpID, next.Arm, next.Tier
			grant, err := m.applyArmRoles(ctx, cm, prev.GrantMask, next, benched[id])
			if err != nil {
				return err
			}
			row.GrantMask = grant
			if moved || prev.Tier != next.Tier || prev.CorpID != c.CorpID || row.GrantMask != prev.GrantMask {
				if err := m.saveArmRow(ctx, row); err != nil {
					return err
				}
			}
			if moved {
				verb := "placed"
				if had {
					verb = "moved"
				}
				m.ceoLog(ctx, c, powerRoles, "arm", id, 0, "%s %s into %s", verb, cm.Name, armLabel(next.Arm))
				if had {
					news = append(news, fmt.Sprintf("%s moves from %s to %s.", cm.Name, armLabel(prev.Arm), armLabel(next.Arm)))
				}
			}
		}
	}
	// Rows for pilots no longer eligible members here: a disabled bot on the badge has its arm
	// bits taken back; one that left has nothing to take.
	for id, r := range cur {
		if _, ok := inCorp[id]; ok {
			continue
		}
		if cm, ok := onBadge[id]; ok {
			if _, err := m.applyArmRoles(ctx, cm, r.GrantMask, armPlacement{}, false); err != nil {
				return err
			}
		}
		m.deleteArmRow(ctx, id)
	}
	if len(news) > 0 {
		sort.Strings(news)
		if len(news) > 6 {
			news = append(news[:6], fmt.Sprintf("...and %d more reassignment(s).", len(news)-6))
		}
		m.queueCorpNews(ctx, c.CorpID, newsRanks, strings.Join(news, " "))
	}
	m.refreshArmIndex(ctx)
	return nil
}

// applyArmRoles moves a member's arm role bits from what its old placement granted
// (prevGrant) to what its new one wants, and answers the new grant.
func (m *Mod) applyArmRoles(ctx context.Context, cm corpMember, prevGrant uint64, next armPlacement, benched bool) (uint64, error) {
	want := uint64(0)
	if next.Arm != "" {
		want = armRoles(next.Arm, next.Tier, benched)
		// Every member may look in its arm's own hangars (corpdeptdivs.go).
		if corpID := m.charCorpID(ctx, cm.CharID); corpID != 0 {
			want |= armLookRoles(m.corpDivs(ctx, corpID), next.Arm)
		}
	}
	if cm.Human() {
		// A capsuleer's job roles may be held at its Base only (corpbaseroles.go); with the switch
		// off this takes any back.
		atBase := (next.Arm == armIndustry || next.Arm == armResearch) && m.baseRolesActive(ctx)
		global, base := splitBaseRoles(want, atBase)
		want = global
		corpID, _ := evedb.GetCharCorpID(ctx, m.db, cm.CharID)
		if err := m.applyBaseRoles(ctx, corpID, cm.CharID, base); err != nil {
			rlogf("rolebots: %v", err)
		}
	}
	want = m.stripGuardedTake(ctx, m.charCorpID(ctx, cm.CharID), cm.CharID, want)
	roles, grant := armRoleDelta(cm.Roles, prevGrant, want)
	if roles == cm.Roles {
		return grant, nil
	}
	if err := m.setMemberRoles(ctx, cm.CharID, roles); err != nil {
		return prevGrant, fmt.Errorf("arm roles %d: %w", cm.CharID, err)
	}
	return grant, nil
}

// armOfficerQuota is how many Directors a corporation with arms keeps: one per justified arm
// a Director heads (Security, Mining, Sales, HR, Infrastructure, Research), at least one once
// there are officers. Industry is headed by the Industry Director and Finance by the Treasurer,
// neither a Director-bit post.
func armOfficerQuota(justified []corpArm, members int) int {
	if members < ceoHierarchyFrom {
		return 0
	}
	n := 0
	for _, a := range justified {
		if portfolioOfArm(a) != "" {
			n++
		}
	}
	return max(1, n)
}

// armNeededPortfolios is the departments a corporation with arms staffs: one per justified
// Director-headed arm, in armOrder.
func armNeededPortfolios(justified []corpArm) []string {
	var out []string
	for _, a := range justified {
		if p := portfolioOfArm(a); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// armRoleWords says, for a letter or the board, what an arm's bundle opens.
func armRoleWords(mask uint64) []string {
	var out []string
	if mask&corp.RoleFactoryManager != 0 {
		out = append(out, "install corporation jobs from the library against its materials (Factory Manager)")
	}
	if mask&corp.RoleSecurityOfficer != 0 {
		out = append(out, "inspect members' hangars (Security Officer)")
	}
	if mask&corp.RoleStationManager != 0 {
		out = append(out, "administer the corporation's stations (Station Manager)")
	}
	if mask&corp.RoleCanRentOffice != 0 {
		out = append(out, "rent offices for the corporation")
	}
	if mask&corp.RoleChatManager != 0 {
		out = append(out, "speak for the corporation (Communications Officer)")
	}
	if mask&corp.RoleJuniorAccountant != 0 {
		out = append(out, "read the corporation's books (Junior Accountant)")
	}
	if mask&corp.RoleAuditor != 0 {
		out = append(out, "audit the corporation (Auditor)")
	}
	if mask&corp.RoleEquipmentConfig != 0 {
		out = append(out, "configure corporation equipment in space")
	}
	if mask&corp.RoleHangarCanTake7 != 0 {
		out = append(out, "load the starbase kit and fuel")
	}
	if mask&corp.RoleStarbaseConfig != 0 {
		out = append(out, "configure the corporation's starbases")
	}
	return out
}

func init() {
	registerCorpForget("armMemo", func(id int64) {
		armMemoMu.Lock()
		delete(armMemo, id)
		armMemoMu.Unlock()
	})
}
