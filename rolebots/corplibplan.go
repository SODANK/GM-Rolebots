package rolebots

// The library plan: what the corporation's R&D arm does with its originals, decided in one place.
// Originals are locked in the office (corpvault.go) and are the research and copy engine; builders
// work from copies, so the board keeps a shelf of copies stocked ahead of need. An original copies
// once its ME reaches half the charter target (copyThreshold), or at once for a starved type.
// Research goes only as far as it pays (paybackLevel). planLibraryWork is pure: libraryPlanInput
// gathers its input and postLibraryPlan posts the answer. See docs/comment-notes-b05.md#library-plan.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/rates"
	serverram "gurimoon/server/ram"
)

const (
	// corpCopyShelfFloor is the copies of a buildable type the shelf always
	// holds; corpCopyShelfMax bounds what demand can ask for.
	corpCopyShelfFloor = 3
	corpCopyShelfMax   = 60
	// corpCopyDemandWindowSec is how much delivered build demand the shelf is
	// stocked for; corpCopyBuildableSec is how recently a type must have been
	// built to count as one the board builds.
	corpCopyDemandWindowSec = 14 * 86400
	corpCopyBuildableSec    = 30 * 86400
	// corpResearchHorizonSec is how much future building one level of research is priced against: a
	// month, so a level that needs longer to pay back is not bought on a thin wallet.
	corpResearchHorizonSec = 30 * 86400

	// corpResearchFeeInstall / corpResearchFeePerHour stand in for a station
	// with no material-research line on record: the shipped averages.
	corpResearchFeeInstall = 8272.0
	corpResearchFeePerHour = 1550.0
	// corpResearchFeeSkill is the Metallurgy level a level's job time is
	// priced at.
	corpResearchFeeSkill = 4

	// Board priorities. A starved type's copies outrank the company store's builds (50) and sit under
	// replacements and the chair's orders (100). Shelf copies and research outrank market builds (0).
	corpCopyStarvedPriority = 60
	corpCopyShelfPriority   = 10
	corpResearchPriority    = 5

	// corpCopyOrdersBase / corpCopyOrdersPerArm / corpCopyOrdersArmMax size
	// the copy orders one board carries (copyOrdersCap).
	corpCopyOrdersBase   = 8
	corpCopyOrdersPerArm = 2
	corpCopyOrdersArmMax = 24
)

// copyThreshold is the material level an original must reach before it copies
// for the shelf: half the charter's target, rounded up. Pure.
func copyThreshold(meTarget int64) int64 {
	if meTarget <= 0 {
		return 0
	}
	return (meTarget + 1) / 2
}

// shelfTarget is how many copies of a type the shelf should hold: the most of the floor, enough
// copies for the demand window's runs, and, while a build order is open, one per line it could run on
// at once (laneWant). A copy is a lane (copylanes.go). Pure.
func shelfTarget(demandRuns, copyRuns int64, laneWant int) int {
	n := max(corpCopyShelfFloor, laneWant)
	if copyRuns > 0 && demandRuns > 0 {
		n = max(n, int((demandRuns+copyRuns-1)/copyRuns))
	}
	return min(n, corpCopyShelfMax)
}

// copyOrdersCap is how many copy orders a corporation's board carries.
func copyOrdersCap(corpID int64) int {
	return min(corpCopyOrdersBase+corpCopyOrdersPerArm*armCount(corpID, armResearch), corpCopyOrdersArmMax)
}

// libraryPlanIn is everything the plan reads.
type libraryPlanIn struct {
	Prints []libraryPrint // originals at the factory, those in a job included
	Copies []libraryCopy  // copies at the factory with runs left
	Open   []corpWork     // the open board
	// DemandRuns is build runs delivered per bp type over the demand window.
	DemandRuns map[int64]int64
	// Buildable is the bp types the board builds: priced worth building this
	// think, or built within corpCopyBuildableSec.
	Buildable map[int64]bool
	CopyRuns  map[int64]int64 // runs per copy, by bp type (printFacts.CopyRuns)
	// Lanes is the manufacturing lines the corporation's builders hold in all
	// (corpBuildLanes): how many copies of one type an open order can use at
	// once.
	Lanes    int
	Value    map[int64]float64 // product value a run, by bp type, for ranking
	METarget int64             // the charter's ceiling for research
	// Research is what researching each bp type is worth (paybackLevel); a
	// type missing here is never researched.
	Research map[int64]researchFacts
	// SoldRuns is the runs the corporation's sold prints of a bp type carried over the demand window,
	// weighted by bpcResearchShare: a buyer pays that share of the research saving (bpcPrice).
	SoldRuns map[int64]float64
	// ForSale: the house copies stock to sell (corpprintsale.go) once the shelf is full.
	ForSale bool
	// ResearchLed: a house leading with the research tag copies every fully researched print for sale,
	// demand or not. Any other house copies only what the roster buys.
	ResearchLed bool
	// SaleDemand is the roster's NPC spend over the demand window, per bp
	// type (corpcopydemand.go): what a copy for sale is aimed at.
	SaleDemand map[int64]float64
	// SaleBuysWeek is the roster's NPC purchases of each bp type a week
	// (corpcopydemand.go's Buys, a 14-day count, halved): it sizes the stock.
	SaleBuysWeek map[int64]float64
	// OutForSale is copies of each bp type marked for sale or listed that Copies no longer sees (a
	// listed copy is in escrow, one in transit is in a ship). Without them the stage copies again.
	OutForSale map[int64]int
	// SaleStale is a type whose copies sit unsold: listed over a week, or
	// returned unsold, with no sale in the demand window.
	SaleStale map[int64]bool
	// SaleBudget is what the production governor leaves this think (corppayback.go). Copies for sale
	// cost job fees; starved and shelf copies are builders' and stay ungated.
	SaleBudget  float64
	CopyCap     int // copy orders the board may carry in all
	ResearchCap int // research orders the board may carry in all
}

// plannedCopy is one copy order to post.
type plannedCopy struct {
	Print    libraryPrint
	N        int
	Priority int
	Purpose  string
}

// plannedResearch is one research order to post.
type plannedResearch struct {
	Print  libraryPrint
	Kind   string // workKindME or workKindPE
	Levels int64
	To     int64
}

// libraryPlanOut is what the plan asks for.
type libraryPlanOut struct {
	Copies   []plannedCopy
	Research []plannedResearch
	Starved  int // types with a build order and no free copy
	// Retire is open research orders whose print has reached the level
	// research pays for: the board withdraws them.
	Retire []corpWork
}

// researchMat is one material a run of a print consumes in full, priced.
type researchMat struct {
	Quantity int64
	Unit     float64
}

// researchFacts is what researching one bp type's material level is worth:
// what a run consumes, its waste factor, and what one level costs: the wage,
// and the lab's installation fee for the one-level job.
type researchFacts struct {
	Mats        []researchMat
	WasteFactor int64
	Wage        float64
	Fee         float64
}

// levelCost is what one level of research costs the corporation. Pure.
func (f researchFacts) levelCost() float64 { return f.Wage + f.Fee }

// meSaving is the ISK one run saves at ME to over the same run at ME from,
// by the core's own waste formula (rounding included: a small quantity stops
// saving anything long before a large one). Pure.
func meSaving(f researchFacts, from, to int64) float64 {
	s := 0.0
	for _, m := range f.Mats {
		s += float64(evedb.CalcMaterialsNeeded(m.Quantity, f.WasteFactor, from, 5)-
			evedb.CalcMaterialsNeeded(m.Quantity, f.WasteFactor, to, 5)) * m.Unit
	}
	return s
}

// paybackLevel is the material level research should take a print at ME from to: the level in
// (from, ceiling] where the saving over runs most exceeds the wages and fees, or from itself when
// none pays. Every level is weighed because rounding makes the saving lumpy. Pure.
func paybackLevel(f researchFacts, runs float64, from, ceiling int64) int64 {
	best, bestNet := from, 0.0
	if runs <= 0 {
		return best
	}
	for l := from + 1; l <= ceiling; l++ {
		if net := meSaving(f, from, l)*runs - f.levelCost()*float64(l-from); net > bestNet {
			best, bestNet = l, net
		}
	}
	return best
}

// researchRuns is the build runs research on a type is expected to save on: the horizon's worth of
// what the board delivered and its sold prints carried (at a buyer's share), plus open build orders.
// A type with none of the three is worth no research. Pure.
func researchRuns(demand, buildLeft int64, soldRuns float64) float64 {
	return (float64(demand)+soldRuns)*float64(corpResearchHorizonSec)/float64(corpCopyDemandWindowSec) +
		float64(buildLeft)
}

// saleCopyOrder is the order the for-sale stage walks the library's types in:
// those the roster buys from NPCs for at least printDemandMinISK, biggest
// spend first; then, in a research-led house only, the rest in library order.
// Pure.
func saleCopyOrder(order []int64, demand map[int64]float64, researchLed bool) []int64 {
	var hot, rest []int64
	for _, t := range order {
		if demand[t] >= printDemandMinISK {
			hot = append(hot, t)
		} else if researchLed {
			rest = append(rest, t)
		}
	}
	sort.SliceStable(hot, func(i, j int) bool { return demand[hot[i]] > demand[hot[j]] })
	return append(hot, rest...)
}

// copySaleWeeks is how many weeks of the roster's NPC buying the for-sale
// stock is sized to.
const copySaleWeeks = 4

// copySaleTarget is how many copies of a type the house keeps for sale: what it would sell in
// copySaleWeeks at corpCopyDemandShare of the roster's NPC purchases, at least one batch and at most
// the stock ceiling. A type with no measured buying keeps one batch. Pure.
func copySaleTarget(saleISK, buysWeek float64) int {
	ceiling := copyStockTarget
	if saleISK >= printDemandDeepISK {
		ceiling = copyStockTargetSelling
	}
	n := int(math.Ceil(buysWeek * corpCopyDemandShare * copySaleWeeks))
	return min(max(n, copyBatchMin), ceiling)
}

// sumCounts is the total of a count map. Pure.
func sumCounts(m map[int64]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// typeShelf is one bp type's shelf, as the plan sees it.
type typeShelf struct {
	BPTypeID  int64
	Prints    []libraryPrint // best ME first
	Free      int            // copies on the shelf, not on a line
	Held      int            // every copy, on a line or not
	Incoming  int            // copies still wanted by open copy orders
	Target    int
	Starved   bool
	Building  bool // a build order is open
	Demand    int64
	Score     float64
	CopyOrder bool // a copy order of the type is open
}

// planLibraryWork decides the board's copy and research orders. Pure.
func planLibraryWork(in libraryPlanIn) libraryPlanOut {
	var out libraryPlanOut
	busy := map[int64]bool{} // originals with an order pinned to them
	nCopy, nResearch := 0, 0
	buildLeft := map[int64]int64{}
	incoming := map[int64]int{}
	copyOrder := map[int64]bool{}
	researchedType := map[int64]bool{} // an older unpinned research order holds the type
	for _, w := range in.Open {
		switch w.Kind {
		case workKindBuild:
			buildLeft[w.BPTypeID] += int64(w.RunsLeft())
			continue
		case workKindCopy:
			nCopy++
			copyOrder[w.BPTypeID] = true
			incoming[w.BPTypeID] += w.RunsLeft()
		case workKindME, workKindPE:
			nResearch++
			if w.BPItemID == 0 {
				researchedType[w.BPTypeID] = true
			}
		default:
			continue
		}
		if w.BPItemID != 0 {
			busy[w.BPItemID] = true
		}
	}

	shelves := map[int64]*typeShelf{}
	var order []int64
	for _, p := range in.Prints {
		s, ok := shelves[p.BPTypeID]
		if !ok {
			s = &typeShelf{BPTypeID: p.BPTypeID}
			shelves[p.BPTypeID] = s
			order = append(order, p.BPTypeID)
		}
		s.Prints = append(s.Prints, p)
	}
	for _, c := range in.Copies {
		s, ok := shelves[c.BPTypeID]
		if !ok {
			continue // copies of a type with no original: nothing to copy from
		}
		s.Held++
		if c.Flag != 111 {
			s.Free++
		}
	}
	// What research pays for, per type: the runs it would save on, the ME a fresh print is worth
	// taking (typeME, also the level for-sale and best copies are judged by) and half that for PE.
	runs := map[int64]float64{}
	typeME := map[int64]int64{}
	meTo := func(p libraryPrint) int64 {
		f, ok := in.Research[p.BPTypeID]
		if !ok {
			return p.ME
		}
		return paybackLevel(f, runs[p.BPTypeID], p.ME, in.METarget)
	}
	baseThr := copyThreshold(in.METarget)
	thrOf := func(t int64) int64 { return min(baseThr, typeME[t]) }
	var stocked []*typeShelf // types the copy shelf is kept for
	for _, t := range order {
		s := shelves[t]
		sort.SliceStable(s.Prints, func(i, j int) bool { return s.Prints[i].ME > s.Prints[j].ME })
		s.Incoming = incoming[t]
		s.CopyOrder = copyOrder[t]
		s.Building = buildLeft[t] > 0
		s.Demand = in.DemandRuns[t] + buildLeft[t]
		built := in.Buildable[t] || s.Building || in.DemandRuns[t] > 0
		runs[t] = researchRuns(in.DemandRuns[t], buildLeft[t], in.SoldRuns[t])
		if f, ok := in.Research[t]; ok {
			typeME[t] = paybackLevel(f, runs[t], 0, in.METarget)
		}
		if !built {
			continue
		}
		laneWant := 0
		if s.Building {
			laneWant = int(min(int64(in.Lanes), buildLeft[t]))
		}
		s.Target = shelfTarget(s.Demand, in.CopyRuns[t], laneWant)
		s.Starved = s.Building && s.Free == 0
		if s.Starved {
			out.Starved++
		}
		s.Score = float64(max(s.Demand, 1)) * math.Max(in.Value[t], 1)
		stocked = append(stocked, s)
	}

	// Copies: starved types first, then the shelf by demand × value.
	sort.SliceStable(stocked, func(i, j int) bool {
		a, b := stocked[i], stocked[j]
		if a.Starved != b.Starved {
			return a.Starved
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.BPTypeID < b.BPTypeID
	})
	reserved := map[int64]bool{} // originals the shelf keeps for copying
	if in.CopyCap <= 0 {
		stocked = nil // the factory cannot copy: research takes every original
	}
	for _, s := range stocked {
		need := s.Target - s.Free - s.Incoming
		var eligible []libraryPrint
		for _, p := range s.Prints {
			if s.Starved || p.ME >= thrOf(s.BPTypeID) {
				eligible = append(eligible, p)
			}
		}
		if need <= 0 || len(eligible) == 0 {
			continue
		}
		for _, p := range eligible {
			reserved[p.ItemID] = true
		}
		prio, purpose := corpCopyShelfPriority, "for the shelf"
		if s.Starved {
			prio, purpose = corpCopyStarvedPriority, "for waiting builders"
		}
		for _, a := range allocateCopyOrders(eligible, need, busy, in.CopyCap-nCopy, typeME[s.BPTypeID]) {
			out.Copies = append(out.Copies, plannedCopy{Print: a.Print, N: a.N, Priority: prio, Purpose: purpose})
			busy[a.Print.ItemID] = true
			nCopy++
		}
	}
	// Stock for sale once the shelf is covered: NPC-bought types first, then every researched print in a
	// research-led house. Never past the sale cap or a closed budget, for an unsold type, or beyond demand.
	if in.ForSale && in.SaleBudget > 0 && sumCounts(in.OutForSale) < corpCopySalesMax {
		for _, t := range saleCopyOrder(order, in.SaleDemand, in.ResearchLed) {
			s := shelves[t]
			if nCopy >= in.CopyCap {
				break
			}
			best := s.Prints[0]
			if s.CopyOrder || busy[best.ItemID] || best.ME < typeME[t] ||
				(s.Target > 0 && s.Free+s.Incoming < s.Target) || in.SaleStale[t] {
				continue
			}
			target := copySaleTarget(in.SaleDemand[t], in.SaleBuysWeek[t])
			have := s.Held + in.OutForSale[t]
			if target-have < copyBatchMin {
				continue // a gap under one batch is not worth a job
			}
			n := copyBatch(target, have)
			if n <= 0 {
				continue
			}
			out.Copies = append(out.Copies, plannedCopy{Print: best, N: n, Priority: corpCopyShelfPriority, Purpose: "for sale"})
			busy[best.ItemID] = true
			nCopy++
		}
	}

	// Open research that has reached what it pays for is withdrawn. The chair's own orders stand.
	printOf := map[int64]libraryPrint{}
	for _, p := range in.Prints {
		printOf[p.ItemID] = p
	}
	for _, w := range in.Open {
		// A for-sale copy order the stage would no longer post is withdrawn
		// too: the governor closed the budget, or its type sits unsold.
		if w.Kind == workKindCopy && w.Source != fleetOrderHuman && strings.HasSuffix(w.Note, "for sale") &&
			(in.SaleBudget <= 0 || in.SaleStale[w.BPTypeID]) {
			out.Retire = append(out.Retire, w)
			continue
		}
		if (w.Kind != workKindME && w.Kind != workKindPE) || w.BPItemID == 0 || w.Source == fleetOrderHuman {
			continue
		}
		p, ok := printOf[w.BPItemID]
		if !ok {
			continue // pinned to a print at another base: that base's plan judges it
		}
		if (w.Kind == workKindME && meTo(p) <= p.ME) || (w.Kind == workKindPE && p.PE >= typeME[p.BPTypeID]/2) {
			out.Retire = append(out.Retire, w)
		}
	}

	// Research: every original the shelf does not hold for copying, as far as research pays (ME, then PE
	// half as far). Prints below the copy threshold go first, then by demand and value.
	var cands []plannedResearch
	rank := map[int64]float64{}
	for _, t := range order {
		s := shelves[t]
		if researchedType[t] {
			continue
		}
		for _, p := range s.Prints {
			if busy[p.ItemID] || reserved[p.ItemID] && p.ME >= thrOf(t) {
				continue
			}
			to := meTo(p)
			r := plannedResearch{Print: p, Kind: workKindME, Levels: to - p.ME, To: to}
			if r.Levels <= 0 {
				pe := typeME[t] / 2
				r = plannedResearch{Print: p, Kind: workKindPE, Levels: pe - p.PE, To: pe}
			}
			if r.Levels <= 0 {
				continue
			}
			cands = append(cands, r)
			rank[p.ItemID] = float64(s.Demand)*math.Max(in.Value[t], 1) + math.Max(in.Value[t], 1)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if lowA, lowB := a.Print.ME < thrOf(a.Print.BPTypeID), b.Print.ME < thrOf(b.Print.BPTypeID); lowA != lowB {
			return lowA
		}
		if rank[a.Print.ItemID] != rank[b.Print.ItemID] {
			return rank[a.Print.ItemID] > rank[b.Print.ItemID]
		}
		return a.Print.ItemID < b.Print.ItemID
	})
	for _, r := range cands {
		if nResearch >= in.ResearchCap {
			break
		}
		out.Research = append(out.Research, r)
		nResearch++
	}
	return out
}

// libraryPlanInput gathers the plan's input for one corporation's factory. buildable is what this
// think's build pass priced worth building. A base with no copy line gets CopyCap 0 from its caller,
// which turns the stage off.
func (m *Mod) libraryPlanInput(ctx context.Context, c *corpCEO, ch Charter, stationID int64,
	buildable map[int64]bool, researchLed bool) (libraryPlanIn, error) {
	in := libraryPlanIn{METarget: int64(ch.METarget), ForSale: true, ResearchLed: researchLed,
		SaleDemand: map[int64]float64{}, SaleBuysWeek: map[int64]float64{},
		OutForSale: map[int64]int{}, SaleStale: map[int64]bool{},
		CopyCap:    copyOrdersCap(c.CorpID), ResearchCap: researchOrdersCap(c.CorpID),
		Buildable: map[int64]bool{}, DemandRuns: map[int64]int64{},
		CopyRuns: map[int64]int64{}, Value: map[int64]float64{}, SoldRuns: map[int64]float64{}}
	var err error
	if in.Open, err = m.openWorkFor(ctx, c.CorpID); err != nil {
		return in, err
	}
	if in.Prints, err = m.libraryPrints(ctx, c.CorpID, stationID); err != nil {
		return in, err
	}
	// The shelf is the corporation's wherever its copies sit: a copy made at an R&D base is carried to
	// the factory (corpprintmove.go), so counting only a base's own office would copy a type twice.
	in.Copies, _ = m.libraryCopiesAll(ctx, c.CorpID)
	in.Lanes = m.corpBuildLanes(ctx, c.CorpID)
	for t := range buildable {
		in.Buildable[t] = true
	}
	now := clock.Unix()
	// The literal cut-offs are ints, never bind parameters: a placeholder
	// inside a grouped IF buckets wrongly on MariaDB.
	rows, qerr := m.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT w.bpTypeID, SUM(IF(g.paidAt > %d, g.runs, 0))
		  FROM rolebot_corp_wages g
		  JOIN rolebot_corp_work w ON w.workID = g.workID
		 WHERE g.corporationID = ? AND g.paidAt > ? AND w.kind = 'build'
		 GROUP BY w.bpTypeID`, now-corpCopyDemandWindowSec), c.CorpID, now-corpCopyBuildableSec)
	if qerr == nil {
		for rows.Next() {
			var t, runs int64
			if rows.Scan(&t, &runs) == nil {
				in.Buildable[t] = true
				in.DemandRuns[t] = runs
			}
		}
		rows.Close()
	}
	// A print the print fund bought (corpfund.go) has no build record, so seed it with the runs the fund
	// was opened for. The corporation then copies it for its board and for sale before the first build.
	if seed := printFundSeedRuns(ctx, m.db); seed > 0 {
		for bp := range m.fundBoughtPrints(ctx, c.CorpID) {
			in.Buildable[bp] = true
			in.DemandRuns[bp] = max(in.DemandRuns[bp], seed)
		}
	}
	var products []int64
	productOf := map[int64]int64{}
	for _, p := range in.Prints {
		if _, ok := in.CopyRuns[p.BPTypeID]; ok {
			continue
		}
		f, ferr := m.printFactsFor(ctx, p.BPTypeID)
		if ferr != nil {
			in.CopyRuns[p.BPTypeID] = 0
			continue
		}
		in.CopyRuns[p.BPTypeID] = f.CopyRuns
		if f.ProductTypeID != 0 {
			productOf[p.BPTypeID] = f.ProductTypeID
			products = append(products, f.ProductTypeID)
		}
	}
	if vals, verr := evedb.TypeValues(ctx, m.db, products); verr == nil {
		for bp, prod := range productOf {
			in.Value[bp] = vals[prod].Value
		}
	}
	in.Research = m.researchFactsFor(ctx, stationID, in.Prints)
	m.fillSoldRuns(ctx, c.CorpID, &in)
	if pd := m.printDemand(ctx); pd != nil {
		for _, p := range in.Prints {
			if buys, isk := pd.total(p.BPTypeID); isk > 0 {
				in.SaleDemand[p.BPTypeID] = isk
				in.SaleBuysWeek[p.BPTypeID] = float64(buys) / 2 // a 14-day count
			}
		}
	}
	m.fillSaleShelf(ctx, c.CorpID, &in)
	if tapes, err := m.copyTapes(ctx); err == nil {
		fillTapeDemand(&in, tapes, copyWantedWeek(clock.Unix()))
	}
	return in, nil
}

// copyStaleListedSec is how long a copy may sit listed before its type
// counts as not selling.
const copyStaleListedSec = 7 * 86400

// fillSaleShelf reads the for-sale side of the library: copies marked or listed that the shelf does
// not see (OutForSale), and types whose copies sit unsold (SaleStale). Best effort: a failed read
// leaves the maps empty.
func (m *Mod) fillSaleShelf(ctx context.Context, corpID int64, in *libraryPlanIn) {
	onShelf := map[int64]bool{}
	for _, c := range in.Copies {
		onShelf[c.ItemID] = true
	}
	now := clock.Unix()
	unsold := map[int64]bool{}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, bpTypeID, status, listedAt, returnedAt FROM rolebot_corp_print_sales
		 WHERE corporationID = ? AND status IN ('wanted', 'listed', 'returned')`, corpID); err == nil {
		for rows.Next() {
			var item, t, listedAt, returnedAt int64
			var status string
			if rows.Scan(&item, &t, &status, &listedAt, &returnedAt) != nil {
				continue
			}
			switch status {
			case "wanted", "listed":
				if !onShelf[item] {
					in.OutForSale[t]++
				}
				if status == "listed" && listedAt > 0 && now-listedAt > copyStaleListedSec {
					unsold[t] = true
				}
			case "returned":
				if returnedAt > now-corpCopyDemandWindowSec {
					unsold[t] = true
				}
			}
		}
		rows.Close()
	}
	if len(unsold) == 0 {
		return
	}
	sold := map[int64]bool{}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT l.bpTypeID FROM rolebot_bpc_listings l
		  JOIN escrows e ON e.escrowID = l.escrowID
		 WHERE l.corporationID = ? AND l.isCopy = 1 AND e.state = ? AND e.dateCompleted > ?`,
		corpID, evedb.CourierStateDelivered, now-corpCopyDemandWindowSec); err == nil {
		for rows.Next() {
			var t int64
			if rows.Scan(&t) == nil {
				sold[t] = true
			}
		}
		rows.Close()
	}
	for t := range unsold {
		if !sold[t] {
			in.SaleStale[t] = true
		}
	}
}

// fillTapeDemand adds the escrow copy market (corpcopytape.go) to the library plan's inputs. Copies that
// sell among the roster count as sale demand next to its NPC buying: ISK over the window, and the
// purchases a week scaled so copySaleTarget's corpCopyDemandShare yields corpCopyMarketShare of them.
// Research is weighed on them too (SoldRuns). A type whose listings already hold copyTapeCapWeeks of
// sales is stale: no copies for sale are posted for it. wanted is the builders' unmet looks a week
// (copyWantedWeek), counted like sales. Pure.
func fillTapeDemand(in *libraryPlanIn, tapes map[int64]copyTape, wanted map[int64]float64) {
	seen := map[int64]bool{}
	for _, p := range in.Prints {
		t := p.BPTypeID
		if seen[t] {
			continue
		}
		seen[t] = true
		tape, ok := tapes[t]
		if ok && tapeRoom(tape) == 0 && wanted[t] == 0 {
			in.SaleStale[t] = true
		}
		// Purchases a week: the tape's sales (a 14-day count) plus the unmet looks.
		buys := float64(tape.Sold14)/2 + wanted[t]
		if buys <= 0 {
			continue
		}
		cr := in.CopyRuns[t]
		if tape.PerRun > 0 && cr > 0 {
			in.SaleDemand[t] += tape.PerRun * float64(cr) * buys * 2
		}
		in.SaleBuysWeek[t] += buys * corpCopyMarketShare / corpCopyDemandShare
		if cr > 0 {
			in.SoldRuns[t] += buys * 2 * float64(cr) * bpcResearchShare * corpCopyMarketShare
		}
	}
}

// fillSoldRuns credits each bp type with the runs the corporation's sold
// prints of it carried over the demand window (libraryPlanIn.SoldRuns). The
// ledger names a sale by the print's type name; a sale counts the type's copy
// runs, which is what the library sells.
func (m *Mod) fillSoldRuns(ctx context.Context, corpID int64, in *libraryPlanIn) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT it.typeID, COUNT(*)
		  FROM rolebot_corp_ledger l
		  JOIN invTypes it ON it.typeName = l.detail
		 WHERE l.corporationID = ? AND l.line = ? AND l.amount > 0 AND l.at > ?
		 GROUP BY it.typeID`, corpID, lineResearchSales, clock.Unix()-corpCopyDemandWindowSec)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var t, n int64
		if rows.Scan(&t, &n) != nil {
			continue
		}
		if cr := in.CopyRuns[t]; cr > 0 {
			in.SoldRuns[t] = float64(n*cr) * bpcResearchShare
		}
	}
}

// researchMatsCache holds each bp type's static research inputs (the materials a run consumes in
// full, and the waste factor) by bpTypeID.
var researchMatsCache sync.Map

type researchMatsEntry struct {
	mats        []evedb.MaterialReq
	wasteFactor int64
	meTime      int64 // ResearchMaterialTime: seconds a level takes before skill
}

// researchFactsFor prices research on every type the prints are of: the
// materials at TypeValues, the wage a level of it is posted at, and the fee a
// one-level job pays the station's lab. A type whose blueprint data cannot be
// read is left out, which the plan reads as not worth researching.
func (m *Mod) researchFactsFor(ctx context.Context, stationID int64, prints []libraryPrint) map[int64]researchFacts {
	entries := map[int64]researchMatsEntry{}
	var matIDs []int64
	for _, p := range prints {
		if _, ok := entries[p.BPTypeID]; ok {
			continue
		}
		var e researchMatsEntry
		if v, ok := researchMatsCache.Load(p.BPTypeID); ok {
			e = v.(researchMatsEntry)
		} else {
			bt, err := evedb.GetBlueprintTypeInfo(ctx, m.db, p.BPTypeID)
			if err != nil {
				continue
			}
			reqs, err := evedb.GetRequiredMaterials(ctx, m.db, p.BPTypeID, 1)
			if err != nil {
				continue
			}
			e.wasteFactor = bt.WasteFactor
			e.meTime = bt.ResearchMaterialTime
			for _, r := range reqs {
				// The material half of research, as printSavingPerRun prices it:
				// only what a run eats whole.
				if r.DamagePerJob >= 1 {
					e.mats = append(e.mats, r)
				}
			}
			researchMatsCache.Store(p.BPTypeID, e)
		}
		entries[p.BPTypeID] = e
		for _, r := range e.mats {
			matIDs = append(matIDs, r.TypeID)
		}
	}
	vals, err := evedb.TypeValues(ctx, m.db, matIDs)
	if err != nil {
		return nil
	}
	install, perHour, timeMul := m.researchLineRates(ctx, stationID)
	out := make(map[int64]researchFacts, len(entries))
	for bp, e := range entries {
		f := researchFacts{WasteFactor: e.wasteFactor, Wage: m.researchWage(ctx, bp),
			Fee: researchLevelFee(e.meTime, install, perHour, timeMul)}
		for _, r := range e.mats {
			f.Mats = append(f.Mats, researchMat{Quantity: r.Quantity, Unit: vals[r.TypeID].Value})
		}
		out[bp] = f
	}
	return out
}

// researchLevelFee is the installation fee of one level of material research:
// the line's install charge plus its hourly rate over the level's job time
// (researchDurationSec's arithmetic at corpResearchFeeSkill). Standing
// discounts are left out, so it errs high. Pure.
func researchLevelFee(meTime int64, install, perHour, timeMul float64) float64 {
	if meTime <= 0 {
		return install
	}
	sec := serverram.CalcMEResearchTime(meTime, 0, 1, corpResearchFeeSkill) * timeMul /
		rates.IndustrySpeed / rates.ResearchSpeed
	return install + perHour*sec/3600
}

// researchLineRates is what the station's material-research lines charge, on
// average, and their time multiplier; the shipped averages when the station
// has none on record.
func (m *Mod) researchLineRates(ctx context.Context, stationID int64) (install, perHour, timeMul float64) {
	install, perHour, timeMul = corpResearchFeeInstall, corpResearchFeePerHour, 1
	var i, h, t sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, `
		SELECT AVG(al.costInstall), AVG(al.costPerHour), AVG(COALESCE(alt.baseTimeMultiplier, 1))
		  FROM ramAssemblyLines al
		  LEFT JOIN ramAssemblyLineTypes alt ON alt.assemblyLineTypeID = al.assemblyLineTypeID
		 WHERE al.containerID = ? AND al.activityID = ?`,
		stationID, ramActivityMEResearch).Scan(&i, &h, &t); err != nil || !i.Valid {
		return
	}
	install, perHour = i.Float64, h.Float64
	if t.Valid && t.Float64 > 0 {
		timeMul = t.Float64
	}
	return
}

// researchWage is what the board pays for one level of research on a print.
func (m *Mod) researchWage(ctx context.Context, bpTypeID int64) float64 {
	return math.Max(corpResearchWageMin, math.Floor(corpResearchWageFrac*evedb.TypeValueOf(ctx, m.db, bpTypeID).Value))
}

// researchShares splits the board's research cap over the R&D bases by how
// many originals each holds, every base with originals getting at least one.
// Pure.
func researchShares(cap int, originals map[int64]int) map[int64]int {
	total := 0
	for _, n := range originals {
		total += n
	}
	out := make(map[int64]int, len(originals))
	for st, n := range originals {
		if n <= 0 || total == 0 {
			out[st] = 0
			continue
		}
		out[st] = max(1, int(math.Ceil(float64(cap)*float64(n)/float64(total))))
	}
	return out
}

// postLibraryPlans runs the library plan at every R&D base, or at the factory alone while the
// corporation has none. Each base plans the originals on its own shelf against the one copy shelf.
func (m *Mod) postLibraryPlans(ctx context.Context, c *corpCEO, ch Charter, factory int64, labBases []int64,
	buildable map[int64]bool, researchLed bool, saleBudget float64) {
	if len(labBases) == 0 {
		m.postLibraryPlan(ctx, c, ch, factory, buildable, researchLed, -1, saleBudget)
		return
	}
	if len(labBases) == 1 {
		m.postLibraryPlan(ctx, c, ch, labBases[0], buildable, researchLed, -1, saleBudget)
		return
	}
	originals := map[int64]int{}
	for _, st := range labBases {
		ps, _ := m.libraryPrints(ctx, c.CorpID, st)
		originals[st] = len(ps)
	}
	shares := researchShares(researchOrdersCap(c.CorpID), originals)
	for _, st := range labBases {
		m.postLibraryPlan(ctx, c, ch, st, buildable, researchLed, shares[st], saleBudget)
	}
}

// postLibraryPlan plans and posts the library's research and copy orders at
// one base. researchShare is the research orders this base may carry (-1 =
// the board's whole cap): the cap counts the board's open research
// everywhere, so a base's share is added to what the other bases hold.
func (m *Mod) postLibraryPlan(ctx context.Context, c *corpCEO, ch Charter, stationID int64,
	buildable map[int64]bool, researchLed bool, researchShare int, saleBudget float64) {
	canCopy := m.labStationHasLine(ctx, stationID, ramActivityCopying)
	in, err := m.libraryPlanInput(ctx, c, ch, stationID, buildable, researchLed)
	if err != nil || len(in.Prints) == 0 {
		return
	}
	in.SaleBudget = saleBudget
	if !canCopy {
		in.CopyCap = 0
	}
	if researchShare >= 0 {
		elsewhere := 0
		for _, w := range in.Open {
			if (w.Kind == workKindME || w.Kind == workKindPE) && w.StationID != stationID {
				elsewhere++
			}
		}
		in.ResearchCap = elsewhere + researchShare
	}
	plan := planLibraryWork(in)
	for _, w := range plan.Retire {
		if m.workHasClaims(ctx, w.WorkID) {
			continue // a job is running on it: the level it is paying for lands first
		}
		m.closeWork(ctx, w.WorkID, workVoid)
		if w.Kind == workKindCopy {
			m.ceoLog(ctx, c, powerProduction, "void", w.BPTypeID, 0,
				"withdrew the copies for sale of %s — its copies sit unsold, or the production budget is spent",
				m.typeNameOr(ctx, w.BPTypeID))
			continue
		}
		m.ceoLog(ctx, c, powerProduction, "void", w.BPTypeID, 0,
			"withdrew the %s research on one %s — further levels save less than their wages",
			map[string]string{workKindME: "material", workKindPE: "time"}[w.Kind], m.typeNameOr(ctx, w.BPTypeID))
	}
	copies, posted := 0, 0
	for _, a := range plan.Copies {
		if err := m.postCopyWork(ctx, c, stationID, a.Print, a.N, a.Purpose, a.Priority); err != nil {
			break
		}
		copies += a.N
		posted++
	}
	research := 0
	for _, r := range plan.Research {
		act := int64(ramActivityMECorp)
		if r.Kind == workKindPE {
			act = ramActivityPEResearch
		}
		if !m.labStationHasLine(ctx, stationID, act) {
			continue
		}
		if err := m.postResearchWork(ctx, c, stationID, r); err != nil {
			break
		}
		research++
	}
	if posted > 0 || research > 0 {
		m.ceoLog(ctx, c, powerProduction, "library", 0, 0,
			"library plan: %d copy order(s) (%d copies), %d research order(s), %d type(s) waiting on copies",
			posted, copies, research, plan.Starved)
	}
}

// postResearchWork posts one research order pinned to one original.
func (m *Mod) postResearchWork(ctx context.Context, c *corpCEO, stationID int64, r plannedResearch) error {
	p := r.Print
	wage := m.researchWage(ctx, p.BPTypeID)
	if _, err := m.postWork(ctx, corpWork{CorpID: c.CorpID, Kind: r.Kind, BPTypeID: p.BPTypeID,
		ProductTypeID: p.BPTypeID, StationID: stationID, RunsWanted: int(r.Levels), WagePerRun: wage,
		Purpose: "research", Priority: corpResearchPriority, BPItemID: p.ItemID,
		Note: fmt.Sprintf("to level %d", r.To)}); err != nil {
		return err
	}
	m.ceoLog(ctx, c, powerProduction, "post", p.BPTypeID, float64(r.Levels)*wage,
		"posted %d level(s) of %s research on its %s", r.Levels,
		map[string]string{workKindME: "material", workKindPE: "time"}[r.Kind], m.typeNameOr(ctx, p.BPTypeID))
	return nil
}

// corpBuildLanes is the manufacturing lines the corporation's builders hold in all: every member
// with Factory Manager or Director, bot or human, at 1 + Mass Production + Advanced Mass Production
// each (MaxConcurrentJobs). Busy lines count, since the shelf is stocked for the factory floor.
func (m *Mod) corpBuildLanes(ctx context.Context, corpID int64) int {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID
		  FROM character_ ch JOIN chrCorporationRoles r ON r.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0 AND (r.corprole & ?) <> 0`,
		corpID, int64(corp.RoleFactoryManager|corp.RoleDirector))
	if err != nil {
		return 0
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	lanes := 0
	for _, id := range ids {
		base, _ := m.charSkillLevelFor(ctx, id, serverram.SkillMassProduction)
		adv, _ := m.charSkillLevelFor(ctx, id, serverram.SkillAdvancedMassProduction)
		lanes += serverram.MaxConcurrentJobs(base, adv)
	}
	return lanes
}
