package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The production power: the CEO's eighth, and the industry desk's planner. It runs for every
// corporation whose tags let its ore desk build (oreBoundsFor.Max > 0) and that has a factory office
// (corpfactory.go). Each think reconciles the library against the offices and sweeps stale purchase
// orders (corplibrary.go), then posts build orders for prints that make money, internal demand first
// (corpsrp.go, corpstore.go), sized to a stocking batch and the production budget and placed at one
// Industry base each (corpbuildbases.go). It then plans copies and research (corplibplan.go), buys one
// print for the library when the budget covers it, and mails the board to human members (corpwork.go).
// A corporation whose tags stop letting it build has its open orders voided and nothing new posted.

const (
	corpBuildOrdersBase = 6
	corpBuildOrdersPer  = 8 // one more open build order per this many members
	// corpResearchOrdersBase / corpResearchOrdersArmMax size the research
	// orders on one board (researchOrdersCap): the whole library is
	// researched, so the board is generous.
	corpResearchOrdersBase   = 6
	corpResearchOrdersArmMax = 24
	corpWorkStaleSec         = 14 * 86400
	corpPOEverySec           = 6 * 3600
	corpResearchWageFrac     = 0.01
	corpResearchWageMin      = 20000
	corpNoFactoryLogSec      = 12 * 3600
	// corpPOCandidates bounds the census the purchase scan prices (each one
	// is a bill of materials and a valuation); corpPOSpreadTolerance is how
	// much score a corporation gives up to avoid another's print.
	corpPOCandidates      = 25
	corpPOSpreadTolerance = 0.2
	// corpGlutLogSec is how often the board says what it left off for want
	// of buyers (dropGluttedRows).
	corpGlutLogSec = 6 * 3600
	// groupShuttle is invGroups 31. No corporation builds or buys one for the
	// market (dropGluttedRows, postLibraryPurchase).
	groupShuttle = 31
	// corpProductionSlowSec: a production think longer than this is logged.
	corpProductionSlowSec = 15

	// corpDemandPurpose / corpDemandPriority mark a build order answering the
	// roster's unmet-demand ledger (ledgerDemand): behind replacements owed
	// (100) and the store (50), ahead of the market.
	corpDemandPurpose  = "demand"
	corpDemandPriority = 40
	// corpLedgerDemandRows bounds how many ledger wants one review weighs.
	corpLedgerDemandRows = 40
	// corpLedgerMaxISK caps what one demand order may commit to materials and
	// wages, gearBidMaxValue's role for the trader: a want is a reading of
	// the roster, not a contract, and one type must not take the budget.
	corpLedgerMaxISK = 50_000_000.0
	// corpLedgerCensusCap is the most a ledger want counts as "pilots who
	// could use it" when it joins the library's purchase census (poScore's
	// log1p keeps it on the census's scale).
	corpLedgerCensusCap = 1000
	// corpLedgerScoreMul is the ledger term on a print's worth to the
	// library: x(1 + 0.75) for a type the roster has recorded itself unable
	// to buy near the corporation.
	corpLedgerScoreMul = 0.75

	// corpPlanIndustrySkill is the Industry level the planner times a run at. The worker is unknown
	// until a member claims the order, and a constant scales every row alike.
	corpPlanIndustrySkill = 4
	// corpJobOverheadHours is the labour one order costs beyond its runs (claiming, hauling, install,
	// delivery). It makes a small batch of cheap goods poor work (batchProfitPerHour).
	corpJobOverheadHours = 1.0
)

// buildPlanRow is one library print the planner is weighing.
type buildPlanRow struct {
	BPTypeID, ProductTypeID int64
	ValuePerRun             float64
	MatsPerRun              float64
	WagePerRun              float64
	MaxRuns                 int64
	Batch                   int
	Stock                   bool
	PerRunSec               float64
	Priority                int
	Purpose                 string
	Wanted                  int // runs internal demand asks for (0 = market sizing)
	// RunHours is one run's line time as a job install would stamp it
	// (manufactureDurationSec at corpPlanIndustrySkill, ME/PE 0, a 1.0 line);
	// Portion is units per run.
	RunHours float64
	Portion  int64
	// MktCapRuns is the most runs the board station's region will take
	// (mktdaily.go historyCapRuns), set by dropGluttedRows where the region
	// has a buying record; 0 = no record, no cap. Market sizing only.
	MktCapRuns int
	// CostPlus: a big hull the corporation builds for itself, valued at cost
	// plus the store markup because no tape prices it (corpcostplus.go).
	CostPlus bool
}

// runHours is RunHours, falling back to the print's raw production time.
func (r buildPlanRow) runHours() float64 {
	if r.RunHours > 0 {
		return r.RunHours
	}
	return r.PerRunSec / 3600
}

// profitPerHour is one run's margin per hour of line time: the board's
// measure of what a print's labour earns. Pure.
func (r buildPlanRow) profitPerHour() float64 {
	h := r.runHours()
	if h <= 0 {
		return 0
	}
	return r.Margin() / h
}

// batchProfitPerHour is an order of `runs` runs' margin per hour of labour,
// the job overhead included: a hundred runs of ammunition pay the hour it
// takes to set them up, three do not. Pure.
func batchProfitPerHour(r buildPlanRow, runs int) float64 {
	if runs < 1 {
		return 0
	}
	h := float64(runs)*r.runHours() + corpJobOverheadHours
	return float64(runs) * r.Margin() / h
}

// laborRow is one priced row with what the labour filter needs to judge it.
type laborRow struct {
	Row  buildPlanRow
	Hull bool
	Runs int // the order it would post (sizeBuildOrder at the full budget)
}

// hullMedianPPH is the median batch profit per labour-hour over the hull
// rows that would post at least one run; ok is false when there are none.
// Pure.
func hullMedianPPH(rows []laborRow) (float64, bool) {
	var v []float64
	for _, lr := range rows {
		if lr.Hull && lr.Runs >= 1 {
			v = append(v, batchProfitPerHour(lr.Row, lr.Runs))
		}
	}
	if len(v) == 0 {
		return 0, false
	}
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2], true
	}
	return (v[n/2-1] + v[n/2]) / 2, true
}

// worthTheLabour keeps every hull row, and a module, charge or drone row
// only when its batch earns at least the hull median an hour: bigger
// projects before small ones, unless the batch is big enough to pay for the
// labour. With no hull row to measure against, every row stays. Pure.
func worthTheLabour(rows []laborRow, median float64, haveHull bool) []buildPlanRow {
	out := make([]buildPlanRow, 0, len(rows))
	for _, lr := range rows {
		if lr.Hull || !haveHull || batchProfitPerHour(lr.Row, lr.Runs) >= median {
			out = append(out, lr.Row)
		}
	}
	return out
}

// sortByProfitPerHour orders rows best labour first, ties to the lower print.
func sortByProfitPerHour(rows []buildPlanRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		pi, pj := rows[i].profitPerHour(), rows[j].profitPerHour()
		if pi != pj {
			return pi > pj
		}
		return rows[i].BPTypeID < rows[j].BPTypeID
	})
}

// ledgerDemandRuns sizes a demand order the way gearBidUnits sizes a gear
// bid: the want (in runs) is the ceiling, the money decides, and one order
// never commits more than corpLedgerMaxISK. Pure.
func ledgerDemandRuns(wantedRuns int, costPerRun, budget float64) int {
	if wantedRuns <= 0 || budget <= 0 {
		return 0
	}
	if budget > corpLedgerMaxISK {
		budget = corpLedgerMaxISK
	}
	runs := wantedRuns
	if costPerRun > 0 {
		runs = min(runs, int(budget/costPerRun))
	}
	return max(runs, 0)
}

// Margin is what one run leaves the corporation.
func (r buildPlanRow) Margin() float64 {
	return r.ValuePerRun*(1-oreDeskFeeFrac) - r.MatsPerRun - r.WagePerRun
}

// sizeBuildOrder is how many runs to post: internal demand's own count, else
// a stocking batch; never more than the budget pays wages and materials for.
// Pure, for the test.
func sizeBuildOrder(r buildPlanRow, budget float64) int {
	runs := r.Wanted
	if runs <= 0 {
		runs = industrySizeRuns(max(r.Batch, 1), max(r.Batch, 1), r.Stock, r.MaxRuns)
		if r.MktCapRuns > 0 {
			runs = min(runs, r.MktCapRuns)
		}
	}
	if r.MaxRuns > 0 && int64(runs) > r.MaxRuns {
		runs = int(r.MaxRuns)
	}
	if per := r.WagePerRun + r.MatsPerRun; per > 0 {
		runs = min(runs, int(budget/per))
	}
	return max(runs, 0)
}

// ceoProduction runs the power.
func (m *Mod) ceoProduction(ctx context.Context, c *corpCEO, ch Charter) error {
	started := clock.Now()
	defer func() {
		if d := clock.Now().Sub(started); d.Seconds() > corpProductionSlowSec {
			m.logBot(c.CharID, logInfo, "ceo", "the production review took %.0fs", d.Seconds())
		}
	}()
	w, err := m.prodOpen(ctx, c, ch)
	if err != nil || w == nil {
		return err
	}
	m.prodResearch(ctx, c, ch, w, m.prodBuild(ctx, c, ch, w))
	m.prodProcure(ctx, c, ch, w)
	p, err := m.loadCorpPolicy(ctx, c.CorpID)
	if err != nil {
		return err
	}
	m.corpBoardMail(ctx, c, &p, m.boardExtras(ctx, c.CorpID, ch))
	return m.saveCorpPolicyRow(ctx, p)
}

// prodWindow is one production review's state, handed from stage to stage: a bot CEO runs them all
// in a row, a human chair's Industry head builds and procures and its R&D head researches
// (corpchair_desks.go).
type prodWindow struct {
	facts       *CorpFacts
	open        []corpWork
	stationID   int64
	labBases    []int64
	budget      float64
	saleReserve float64
	researchLed bool
}

// prodOpen is the review's first stage: what is on the board and the shelf, and what may be spent.
// Nil when the corporation builds nothing.
func (m *Mod) prodOpen(ctx context.Context, c *corpCEO, ch Charter) (*prodWindow, error) {
	human := c.Origin == ceoOriginHuman
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return nil, err
	}
	open, err := m.openWorkFor(ctx, c.CorpID)
	if err != nil {
		return nil, err
	}
	// A human chair's desk is its appointment, not its tags.
	if !human && !hasIndustryDesk(facts.Policy.Tags) {
		for _, w := range open {
			if w.Source == fleetOrderHuman {
				continue // the chair's own order stands whatever the CEO's desks are
			}
			m.closeWork(ctx, w.WorkID, workVoid)
		}
		return nil, nil
	}
	stationID, _ := m.corpFactoryOffice(ctx, c.CorpID)
	if stationID == 0 {
		if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "nofactory") >= corpNoFactoryLogSec {
			m.ceoLog(ctx, c, powerProduction, "nofactory", 0, 0,
				"has no office at a station with manufacturing lines — nothing can be built for the corporation yet")
		}
		return nil, nil
	}
	if human {
		// The prints are where the members put them, and locked only by their vote.
		if _, err := m.indexLibrary(ctx, c.CorpID); err != nil {
			return nil, err
		}
	} else {
		if _, err := m.reconcileLibrary(ctx, c.CorpID); err != nil {
			return nil, err
		}
		// The originals just shelved are locked before anything plans on them
		// (corpvault.go); the staff-desk pass covers corporations with no factory.
		m.vaultDesk(ctx, c)
	}
	m.sweepPOs(ctx, c.CorpID)
	// Research and copies run at every R&D base (corpbases.go); builds at every Industry base
	// (corpbuildbases.go). A base with no research lines is not an R&D base.
	labBases := m.armBaseStations(ctx, c.CorpID, armResearch)
	m.sweepStaleWork(ctx, c, open, stationID, labBases, m.industryBaseStations(ctx, c.CorpID))
	// What the factory already made goes down the ladder before anything
	// new is planned: replacements owed, the store, the market (corpstore.go).
	// A human chair's stock is its own to give out.
	if !human {
		if err := m.ceoOutput(ctx, c, ch); err != nil {
			m.logBot(c.CharID, logError, "ceo", "output: %v", err)
		}
	}

	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return nil, err
	}
	// Turnover, not inflow, sizes what production may commit (corppayback.go).
	budget := m.productionGovernor(ctx, c, ch.CapitalRisk*free)
	// A research house sets a share aside for copies for sale before builds and the print fund spend it:
	// without it the for-sale stage met a budget of 0 and posted nothing (corplibplan.go).
	saleReserve := researchSaleReserve(budget, hasTag(facts.Policy.Tags, "research"))
	budget -= saleReserve
	return &prodWindow{
		facts: facts, open: open, stationID: stationID, labBases: labBases, budget: budget, saleReserve: saleReserve,
		// Every house with a copy line copies for sale what the roster buys from NPCs (corpcopydemand.go);
		// one that leads with the research tag also copies every researched print (corpprintsale.go).
		researchLed: len(facts.Policy.Tags) > 0 && facts.Policy.Tags[0] == "research",
	}, nil
}

// prodBuild posts the build orders and answers the types the board can build. A human chair's head
// commits nothing that takes weeks: no print fund, no capital program, no ship replacements.
func (m *Mod) prodBuild(ctx context.Context, c *corpCEO, ch Charter, w *prodWindow) map[int64]bool {
	human := c.Origin == ceoOriginHuman
	// Internal demand first: replacements owed, the store's shelf, then what the roster near the bases
	// cannot buy (ledgerDemand). Ledger claims are renewed for open orders and released for the rest.
	m.keepCorpDemandClaims(ctx, c.CorpID, w.open)
	var internal []buildPlanRow
	if !human {
		internal = m.srpDemand(ctx, c.CorpID)
		internal = append(internal, m.storeDemand(ctx, c.CorpID)...)
	}
	internal = append(internal, m.ledgerDemand(ctx, c)...)
	if !human {
		// A capital program's components and hull (corpprogram.go).
		internal = append(internal, m.programDemand(ctx, c.CorpID)...)
	}
	committed, buildable := m.postBuildOrders(ctx, c, ch, w.facts, w.stationID, w.budget, internal)
	w.budget -= committed
	if human {
		return buildable
	}
	// The print fund saves its share of what is left before anything else spends it (corpfund.go).
	w.budget -= m.feedFunds(ctx, c, ch, w.budget)
	// The capital program walks a stage (corpprogram.go): prints through the
	// program fund, parts into Materials, the hull to its pilot.
	m.ceoProgram(ctx, c, ch, w.facts, w.stationID)
	return buildable
}

// prodResearch posts the library's research and copies, and sends the prints where they are wanted.
func (m *Mod) prodResearch(ctx context.Context, c *corpCEO, ch Charter, w *prodWindow, buildable map[int64]bool) {
	// Copies for sale spend job fees out of the same budget (corplibplan.go).
	m.postLibraryPlans(ctx, c, ch, w.stationID, w.labBases, buildable, w.researchLed, w.budget+w.saleReserve)
	// The library split over the R&D bases and copies for the factory
	// (corpprintmove.go).
	m.printMoveDesk(ctx, c, ch, w.stationID, w.labBases)
	m.notePrintBottlenecks(ctx, c, w.stationID)
	m.markPrintsForSale(ctx, c, ch, w.stationID, w.researchLed)
}

// prodProcure buys what the board is short of: materials, then a print.
func (m *Mod) prodProcure(ctx context.Context, c *corpCEO, ch Charter, w *prodWindow) {
	if err := m.ceoBuyback(ctx, c, ch, w.budget); err != nil {
		m.logBot(c.CharID, logError, "ceo", "buyback: %v", err)
	}
	if w.budget > 0 {
		m.postLibraryPurchase(ctx, c, ch, w.facts, w.stationID, w.budget)
		// An original to copy for the roster, when the product pass bought
		// nothing (one purchase order at a time; corpcopydemand.go).
		m.postCopyDemandPurchase(ctx, c, ch, w.stationID, w.budget)
	}
}

// sweepStaleWork voids orders that have sat a fortnight untouched, or whose
// print has left the library.
func (m *Mod) sweepStaleWork(ctx context.Context, c *corpCEO, open []corpWork, factory int64, labBases, mfgBases []int64) {
	now := clock.Unix()
	lab := map[int64]bool{}
	for _, st := range labBases {
		lab[st] = true
	}
	mfg := map[int64]bool{factory: true}
	for _, st := range mfgBases {
		mfg[st] = true
	}
	for _, w := range open {
		if w.Source == fleetOrderHuman {
			continue // the chair withdraws its own (corporders_igb.go)
		}
		// Where an order may stand: builds at any Industry base, research
		// and copies at the factory or any R&D base.
		build := w.Kind == workKindBuild || w.Kind == workKindRefine
		placed := w.StationID == 0 || w.StationID == factory ||
			(build && mfg[w.StationID]) || (!build && lab[w.StationID])
		switch {
		case !placed && !m.workHasClaims(ctx, w.WorkID):
			// Posted at a factory or lab the corporation moved off (corpfacility.go, corpoffices.go): nobody can
			// take it there, so void it and the board reposts where the print is. A claimed order waits.
			m.closeWork(ctx, w.WorkID, workVoid)
			m.ceoLog(ctx, c, powerProduction, "void", w.ProductTypeID, 0,
				"withdrew the order for %s — it was posted at an office that no longer works for it", m.typeNameOr(ctx, w.ProductTypeID))
		case w.RunsDone == 0 && now-w.PostedAt > corpWorkStaleSec && !m.workHasClaims(ctx, w.WorkID):
			// A claimed order is a job in the oven, however old: the first run lands only when the whole job
			// does, and voiding it mid-job would leave the builder paid as off-board work.
			m.closeWork(ctx, w.WorkID, workVoid)
			m.ceoLog(ctx, c, powerProduction, "void", w.ProductTypeID, 0,
				"withdrew the order for %s — nobody took it in two weeks", m.typeNameOr(ctx, w.ProductTypeID))
		case w.BPItemID != 0 && !m.printAtOffice(ctx, c.CorpID, w.BPItemID, orderStation(w, factory)) &&
			!m.workHasClaims(ctx, w.WorkID):
			// Pinned to one original that is no longer on the shelf here.
			m.closeWork(ctx, w.WorkID, workVoid)
			m.ceoLog(ctx, c, powerProduction, "void", w.BPTypeID, 0,
				"withdrew the %s order on one %s — that print is no longer in the library", w.Kind, m.typeNameOr(ctx, w.BPTypeID))
		case w.BPTypeID != 0 && !m.libraryCanBuild(ctx, c.CorpID, w.BPTypeID) && !m.workHasClaims(ctx, w.WorkID):
			// A one-run copy is consumed at install, so a claimed hull order reads as printless while its job
			// runs. The claim means the order waits.
			m.closeWork(ctx, w.WorkID, workVoid)
			m.ceoLog(ctx, c, powerProduction, "void", w.BPTypeID, 0,
				"withdrew the order for %s — the library no longer holds the print", m.typeNameOr(ctx, w.ProductTypeID))
		}
	}
}

// orderStation is where an order stands: its own station, or the factory for
// one posted nowhere in particular. Pure.
func orderStation(w corpWork, factory int64) int64 {
	if w.StationID != 0 {
		return w.StationID
	}
	return factory
}

// printAtOffice reports whether a corporation print sits in its office at the
// station (any division, or in a job there).
func (m *Mod) printAtOffice(ctx context.Context, corpID, itemID, stationID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity e
		  JOIN crpoffices o ON o.itemID = e.locationID AND o.corporationID = e.ownerID
		 WHERE e.itemID = ? AND e.ownerID = ? AND o.stationID = ?`, itemID, corpID, stationID).Scan(&n)
	return n > 0
}

// workHasClaims reports whether any builder still holds runs of an order.
func (m *Mod) workHasClaims(ctx context.Context, workID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_work_claims WHERE workID = ?`, workID).Scan(&n)
	return n > 0
}

// planRowFor prices one library print for the board, with the product valued
// at the cluster's trades (planRowAt).
func (m *Mod) planRowFor(ctx context.Context, ch Charter, bpTypeID int64) (*buildPlanRow, error) {
	return m.planRowAt(ctx, ch, bpTypeID, 0)
}

// planRowAt prices one library print for a factory in regionID. The product is valued at what it has
// traded for (corptraderef.go), the book's asks only for a type nobody bought. Materials stay at
// TypeValues, what the corporation pays for them.
func (m *Mod) planRowAt(ctx context.Context, ch Charter, bpTypeID, regionID int64) (*buildPlanRow, error) {
	bp, err := evedb.GetBlueprintTypeInfo(ctx, m.db, bpTypeID)
	if err != nil || bp.ProductTypeID == 0 {
		return nil, err
	}
	mats, err := evedb.GetRequiredMaterials(ctx, m.db, bpTypeID, 1)
	if err != nil {
		return nil, err
	}
	ids := []int64{bp.ProductTypeID}
	for _, mt := range mats {
		ids = append(ids, mt.TypeID)
	}
	vals, err := evedb.TypeValues(ctx, m.db, ids)
	if err != nil {
		return nil, err
	}
	portion, _ := evedb.GetTypePortionSize(ctx, m.db, bp.ProductTypeID)
	portion = max(portion, 1)
	r := &buildPlanRow{BPTypeID: bpTypeID, ProductTypeID: bp.ProductTypeID,
		ValuePerRun: m.productValueFrom(ctx, bp.ProductTypeID, regionID, vals[bp.ProductTypeID].Value) * float64(portion),
		MaxRuns:     bp.MaxProductionLimit, PerRunSec: float64(bp.ProductionTime), Purpose: "market",
		RunHours: manufactureDurationSec(bp, 0, corpPlanIndustrySkill, 1, &manufactureLine{TimeMul: 1}) / 3600,
		Portion:  portion}
	for _, mt := range mats {
		r.MatsPerRun += vals[mt.TypeID].Value * float64(mt.Quantity) * mt.DamagePerJob
	}
	r.WagePerRun = wageForRun(r.ValuePerRun, ch.WageFrac)
	cat, batch := m.industryBatchCap(ctx, bp.ProductTypeID, portion)
	r.Batch, r.Stock = batch, industryStocksToBatch(cat)
	return r, nil
}

// productValueFrom is productTradeValue with the book's value already read.
func (m *Mod) productValueFrom(ctx context.Context, typeID, regionID int64, book float64) float64 {
	refs := m.tradeRefsOf(ctx, typeID)
	v, _ := pickTradeValue(refs.Region[regionID], refs.Cluster, book)
	return v
}

// postBuildOrders posts the board's build orders and answers what they
// committed, and the bp types it found worth building (the library plan
// keeps a copy shelf for them).
func (m *Mod) postBuildOrders(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts,
	stationID int64, budget float64, internal []buildPlanRow) (float64, map[int64]bool) {
	open, err := m.openWorkFor(ctx, c.CorpID)
	if err != nil {
		return 0, nil
	}
	posted := map[int64]bool{}
	nOpen := 0
	for _, w := range open {
		if w.Kind == workKindBuild {
			posted[w.ProductTypeID] = true
			nOpen++
		}
	}
	maxOpen := corpBuildOrdersBase + facts.Members/corpBuildOrdersPer
	// The whole library, every office: a print split to an R&D base still builds, and its copy is
	// carried to the Industry base the order lands at (corpprintmove.go).
	prints, err := m.libraryPrintsAll(ctx, c.CorpID)
	if err != nil {
		return 0, nil
	}
	have := map[int64]bool{}
	for _, p := range prints {
		have[p.BPTypeID] = true
	}
	// Copies on the shelf count too: the board builds from a copy when no original is free (corpWorkFor).
	// A type with no original is capped at the runs its copies still carry.
	original := map[int64]bool{}
	for bp := range have {
		original[bp] = true
	}
	copyRuns := map[int64]int64{}
	if cs, cerr := m.libraryCopiesAll(ctx, c.CorpID); cerr == nil {
		for _, cp := range cs {
			if cp.Flag == 111 {
				continue // in a job: its runs are already spoken for
			}
			copyRuns[cp.BPTypeID] += cp.Runs
			have[cp.BPTypeID] = true
		}
	}
	capToCopies := func(r *buildPlanRow) {
		if original[r.BPTypeID] {
			return
		}
		if left := copyRuns[r.BPTypeID]; r.MaxRuns <= 0 || left < r.MaxRuns {
			r.MaxRuns = left
		}
	}

	// Valued where the factory sells (planRowAt).
	region := m.stationRegionID(ctx, stationID)
	var rows, demand []buildPlanRow
	seen := map[int64]bool{}
	// A big hull the corporation builds for itself is valued at cost-plus when no tape prices it
	// (corpcostplus.go). The product groups are read once for the whole library.
	cpScope := costPlusScope(ctx, m.db)
	var groupOf map[int64]typeKind
	if cpScope != costPlusScopeOff {
		var pids []int64
		for bp := range have {
			if f, ferr := m.printFactsFor(ctx, bp); ferr == nil && f.ProductTypeID != 0 {
				pids = append(pids, f.ProductTypeID)
			}
		}
		groupOf = m.typeKinds(ctx, pids)
	}
	costPlus := func(r *buildPlanRow) {
		if groupOf == nil {
			return
		}
		m.applyCostPlus(ctx, ch, r, groupOf[r.ProductTypeID].Group, cpScope)
	}
	for _, in := range internal {
		if !have[in.BPTypeID] || seen[in.BPTypeID] {
			continue
		}
		r, err := m.planRowAt(ctx, ch, in.BPTypeID, region)
		if err != nil || r == nil {
			continue
		}
		capToCopies(r)
		r.Priority, r.Purpose, r.Wanted = in.Priority, in.Purpose, in.Wanted
		costPlus(r)
		seen[in.BPTypeID] = true
		if r.Purpose == corpDemandPurpose {
			// The roster's want is not an obligation: it is built only at a profit, sized by the money, and
			// judged on labour with the market.
			if r.ValuePerRun <= 0 || r.Margin() <= 0 {
				continue
			}
			r.Wanted = ledgerDemandRuns(r.Wanted, r.WagePerRun+r.MatsPerRun, budget)
			if r.Wanted < 1 {
				continue
			}
			demand = append(demand, *r)
			continue
		}
		rows = append(rows, *r)
	}
	var market []buildPlanRow
	for bpType := range have {
		if seen[bpType] {
			continue
		}
		r, err := m.planRowAt(ctx, ch, bpType, region)
		if err != nil || r == nil {
			continue
		}
		costPlus(r) // market rows only under rolebots.costPlusHulls = all
		if r.ValuePerRun <= 0 || r.Margin() <= 0 {
			continue
		}
		capToCopies(r)
		market = append(market, *r)
	}
	market = m.dropGluttedRows(ctx, c, stationID, market)
	// ISK per labour-hour decides, not margin per run. A module, charge or drone order stays only when
	// its batch earns at least the median hull order an hour (worthTheLabour).
	demand, market = m.filterByLabour(ctx, demand, market, budget)
	sortByProfitPerHour(demand)
	sortByProfitPerHour(market)
	rows = append(rows, demand...)
	rows = append(rows, market...)
	buildable := make(map[int64]bool, len(rows))
	for _, r := range rows {
		buildable[r.BPTypeID] = true
	}

	// Each order goes to one Industry base (corpbuildbases.go): internal demand to the factory, the
	// market where its print is or the lines are least queued.
	bases := m.buildBases(ctx, c.CorpID, open)
	// A bill fund that covers one run of a big hull (corpfund.go) is outside this think's budget: the row
	// is sized to budget plus fund, and the fund is spent when the order posts.
	billFunds := m.readyBillFunds(ctx, c.CorpID)
	committed := 0.0
	for _, r := range rows {
		isDemand := r.Purpose == corpDemandPurpose
		if nOpen >= maxOpen && (r.Purpose == "market" || isDemand) {
			break
		}
		if posted[r.ProductTypeID] {
			continue
		}
		avail := budget - committed
		bill := billFunds[r.BPTypeID]
		if bill != nil {
			avail += bill.Saved
		}
		runs := sizeBuildOrder(r, avail)
		if runs < 1 {
			continue
		}
		note := fmt.Sprintf("%.0f ISK margin a run", r.Margin())
		switch {
		case isDemand:
			note = fmt.Sprintf("wanted by the roster, %.0f ISK an hour", batchProfitPerHour(r, runs))
		case r.Purpose != "market":
			note = r.Purpose
		}
		if r.CostPlus {
			note += " (cost-plus: no tape prices this hull)"
		}
		// A ledger want is claimed for the corporation (claimant -corpID) before posting, so a personal
		// industrialist does not build it. One a pilot already holds is left to that pilot.
		if isDemand {
			if ok, err := m.claimDemand(ctx, -c.CorpID, r.ProductTypeID); err != nil || !ok {
				continue
			}
		}
		at, where := stationID, ""
		if i := pickBuildBase(r.BPTypeID, r.Purpose != "market" && !isDemand, bases); i >= 0 {
			at = bases[i].StationID
			bases[i].LoadSec += float64(runs) * r.PerRunSec
			if len(bases) > 1 {
				where = " at " + stationNameOf(ctx, m, at)
			}
		}
		// A capital program's rows build at its yard, whose lines may build
		// the hull where the factory's may not (corpprogram.go).
		if r.Purpose == programPurpose {
			if yard := m.programYardStation(ctx, c.CorpID); yard != 0 && yard != at {
				at = yard
				where = " at " + stationNameOf(ctx, m, at)
			}
		}
		// Who it is for (corprequest.go): a market build is Sales' order and goes onto its Trade shelf.
		// Everything else keeps the board's default.
		w := corpWork{CorpID: c.CorpID, Kind: workKindBuild, BPTypeID: r.BPTypeID,
			ProductTypeID: r.ProductTypeID, StationID: at, RunsWanted: runs, WagePerRun: r.WagePerRun,
			Purpose: r.Purpose, Priority: r.Priority, Note: note}
		if r.Purpose == "market" {
			w.RequesterArm = string(armSales)
			// Unless a war chest borrowed the Trade slot: then Production, and the lister finds it there.
			if divisionArm(m.corpDivs(ctx, c.CorpID), flagDiv(corpDivTrade)) == armSales {
				w.DestFlag = corpDivTrade
			}
		}
		if _, err := m.postWork(ctx, w); err != nil {
			if isDemand {
				m.releaseDemand(ctx, -c.CorpID, r.ProductTypeID)
			}
			m.logBot(c.CharID, logError, "ceo", "posting work: %v", err)
			return committed, buildable
		}
		if isDemand {
			m.commitCorpDemand(ctx, -c.CorpID, r.ProductTypeID, int64(runs)*max(r.Portion, 1))
		}
		committed += float64(runs) * (r.WagePerRun + r.MatsPerRun)
		if bill != nil {
			m.spendBillFund(ctx, c, bill)
			delete(billFunds, r.BPTypeID)
			committed = math.Max(0, committed-bill.Saved)
		}
		posted[r.ProductTypeID] = true
		nOpen++
		m.ceoLog(ctx, c, powerProduction, "post", r.ProductTypeID, float64(runs)*r.WagePerRun,
			"posted %d run(s) of %s%s at %s a run (%s)", runs, m.typeNameOr(ctx, r.ProductTypeID), where,
			iskString(r.WagePerRun), note)
	}
	return committed, buildable
}

// postLibraryPurchase buys one print at a time for the library, scored on what the print earns
// (margin a run times pilots who could use the product) with its price only a budget gate. A
// shipyard buys real hulls only, and no corporation buys a shuttle print.
// See docs/comment-notes-b05.md#library-purchase.
func (m *Mod) postLibraryPurchase(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, stationID int64, budget float64) {
	if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "po") < corpPOEverySec {
		return
	}
	var openPOs int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_po WHERE corporationID = ? AND status = 'open'`,
		c.CorpID).Scan(&openPOs)
	if openPOs >= poMaxOpen {
		return
	}
	p, err := m.loadCorpPolicy(ctx, c.CorpID)
	if err != nil || p.DeputyID == 0 {
		return
	}
	// A second original of a type whose one print holds the board up comes
	// before a new type (corpbottleneck.go).
	if m.buyBottleneckPrint(ctx, c, p.DeputyID, stationID, budget) {
		return
	}
	shipyard := containsStr(facts.Policy.Tags, "shipbuilding")
	limit := corpPOCandidates
	if shipyard {
		limit = 0 // the whole census: hulls are rarely in its head
	}
	cands, err := m.capabilityCandidates(ctx, limit)
	if err != nil {
		return
	}
	// What the roster near the corporation cannot buy joins the census at its
	// head, and scores the ledger term (poLedgerMul).
	demanded := map[int64]bool{}
	{
		inCensus := map[int64]bool{}
		for _, cd := range cands {
			inCensus[cd.TypeID] = true
		}
		var lead []indCandidate
		for _, w := range m.corpLedgerWants(ctx, c.CorpID) {
			demanded[w.TypeID] = true
			if !inCensus[w.TypeID] {
				lead = append(lead, indCandidate{TypeID: w.TypeID, BPTypeID: w.BPTypeID,
					Wanted: min(w.Units, corpLedgerCensusCap), Evidence: evidenceLedger})
			}
		}
		cands = append(lead, cands...)
	}
	if len(cands) == 0 {
		return
	}
	ids := make([]int64, 0, len(cands))
	for _, cd := range cands {
		ids = append(ids, cd.TypeID)
	}
	kinds := m.typeKinds(ctx, ids)
	if shipyard {
		kept := cands[:0]
		for _, cd := range cands {
			if k := kinds[cd.TypeID]; isRealHull(k.Category, k.Group) {
				kept = append(kept, cd)
			}
		}
		cands = kept
	}
	if len(cands) > corpPOCandidates*4 {
		cands = cands[:corpPOCandidates*4] // the census is ordered by headcount
	}
	// One valuation pass for every product and print up front: planRowFor's
	// own TypeValues calls are then cache hits (typevalue.go, 60 s).
	var warm []int64
	for _, cd := range cands {
		warm = append(warm, cd.TypeID, cd.BPTypeID)
	}
	_, _ = evedb.TypeValues(ctx, m.db, warm)
	elsewhere := m.printsHeldElsewhere(ctx, c.CorpID)
	// The library is the CEO's to stock, so its race feeling decides what the corporation builds
	// (tolerance.go xenoBuildMul).
	ceo := pilotRace{Race: raceOf(c.CharID), Phobia: ch.Xenophobia}
	if ceo.Race == 0 {
		ceo.Race = m.charRace(ctx, c.CharID)
	}
	region := m.stationRegionID(ctx, stationID)

	var picks []poPick
	var over []fundPick
	// Hull prints the budget reached but the margin or the payback refused, named in the "saving" line instead of vanishing.
	var hullRefused []string
	cpScope := costPlusScope(ctx, m.db)
	for _, cd := range cands {
		k := kinds[cd.TypeID]
		if k.Group == groupShuttle {
			continue
		}
		if cd.BPTypeID == 0 || m.libraryCanBuild(ctx, c.CorpID, cd.BPTypeID) || m.metaGroupOf(ctx, cd.TypeID) != 0 {
			continue
		}
		raceMul, raceOK := xenoBuildMul(ceo, m.hullRace(ctx, cd.TypeID))
		if !raceOK {
			continue
		}
		value := evedb.TypeValueOf(ctx, m.db, cd.BPTypeID).Value
		if shipyard {
			if cheapest := m.shipyardPrintPrice(ctx, cd.BPTypeID, region); cheapest > 0 && (value <= 0 || cheapest < value) {
				value = cheapest
			}
		}
		if value <= 0 || value*poAdvanceMarkup > budget {
			// A hull print the budget cannot reach is what the print fund
			// saves toward (corpfund.go), when nothing else fits below.
			if shipyard && value > 0 && isRealHull(k.Category, k.Group) {
				over = append(over, fundPick{BPTypeID: cd.BPTypeID, ProductTypeID: cd.TypeID, Group: k.Group,
					Value: value, Wanted: cd.Wanted})
			}
			continue
		}
		hull := isRealHull(k.Category, k.Group)
		r, err := m.planRowAt(ctx, ch, cd.BPTypeID, region)
		if err != nil || r == nil {
			continue
		}
		// A hull with no tape is valued as the board will value its market row (corpcostplus.go), not at the stale catalogue.
		if hull {
			m.applyCostPlus(ctx, ch, r, k.Group, cpScope)
		}
		if r.Margin() <= 0 {
			if hull {
				hullRefused = append(hullRefused, m.typeNameOr(ctx, cd.TypeID)+" (no margin)")
			}
			continue
		}
		// Sold, not just buildable: the margin must earn the price back over what the product's market actually takes, or, for a
		// hull that has never traded, what the roster would fly (corppayback.go).
		var want int64
		if hull {
			want = cd.Wanted
		}
		if !m.printPaysBack(ctx, cd.BPTypeID, region, r.Margin(), value*poAdvanceMarkup, want) {
			if hull {
				hullRefused = append(hullRefused, m.typeNameOr(ctx, cd.TypeID)+" (would not pay back)")
			}
			continue
		}
		picks = append(picks, poPick{BPType: cd.BPTypeID, Value: value,
			Score:         poScore(r.Margin(), cd.Wanted, hull) * raceMul * poLedgerMul(demanded[cd.TypeID]),
			HeldElsewhere: elsewhere[cd.BPTypeID]})
	}
	best := choosePOPick(picks)
	if best == nil {
		// Nothing fits this think: aim the fund at the cheapest print of
		// the lowest rung the library lacks, and let it save (corpfund.go).
		if m.fundTargetFrom(ctx, c, over) {
			return
		}
		if shipyard && clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "posave") >= corpPOEverySec {
			note := ""
			if len(hullRefused) > 0 {
				shown := hullRefused
				if len(shown) > 4 {
					shown = append(shown[:4:4], fmt.Sprintf("%d more", len(hullRefused)-4))
				}
				note = " (passed over: " + strings.Join(shown, ", ") + ")"
			}
			m.ceoLog(ctx, c, powerProduction, "posave", 0, budget,
				"saving for a ship print — no hull print it can build at a profit fits the %s it can spend on one%s",
				iskString(budget), note)
		}
		return
	}
	if err := m.postPO(ctx, c, p.DeputyID, best.BPType, stationID, best.Value, shipyard); err != nil &&
		!errors.Is(err, errPOOverCap) {
		m.logBot(c.CharID, logError, "ceo", "purchase order: %v", err)
	}
}

// shipyardPrintPrice is the cheapest a print of the type can be had for in
// one region: the lowest market ask for it, or the cheapest escrow copy with
// runs left there. 0 when neither exists. Both reads are roster-wide
// snapshots (allAskByRegion, escrowBPOListings), so this costs no query.
func (m *Mod) shipyardPrintPrice(ctx context.Context, bpTypeID, regionID int64) float64 {
	best := 0.0
	if asks, err := m.allAskByRegion(ctx); err == nil {
		if p := asks[bpTypeID][regionID]; p > 0 {
			best = p
		}
	}
	if ls, err := m.escrowBPOListings(ctx); err == nil {
		for _, l := range ls {
			if l.TypeID != bpTypeID || l.RegionID != regionID || !l.Copy || l.Runs <= 0 || l.Price <= 0 {
				continue
			}
			if best == 0 || l.Price < best {
				best = l.Price
			}
		}
	}
	return best
}

// typeKind is a type's category and group.
type typeKind struct{ Category, Group int64 }

// typeKinds reads category and group for a set of types in one query.
func (m *Mod) typeKinds(ctx context.Context, ids []int64) map[int64]typeKind {
	out := make(map[int64]typeKind, len(ids))
	if len(ids) == 0 {
		return out
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, g.categoryID, g.groupID FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeID IN (`+placeholders(len(ids))+`)`, int64Args(ids)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var k typeKind
		if rows.Scan(&id, &k.Category, &k.Group) == nil {
			out[id] = k
		}
	}
	return out
}

// stationRegionID is the region a station sits in, 0 when unknown.
func (m *Mod) stationRegionID(ctx context.Context, stationID int64) int64 {
	return m.stationRegionOf(ctx, stationID)
}

// dropGluttedRows takes the board's market rows and drops products the corporation should not build
// more of: shuttles, products nobody fits or flies, a batch already unsold, and floods in the region
// or cluster (productFlooded, historyFlood, globalFlood). Passing rows are capped at MktCapRuns.
// It logs what it left off at most once per corpGlutLogSec.
func (m *Mod) dropGluttedRows(ctx context.Context, c *corpCEO, stationID int64, rows []buildPlanRow) []buildPlanRow {
	if len(rows) == 0 {
		return rows
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ProductTypeID)
	}
	kinds := m.typeKinds(ctx, ids)
	users := m.productionCounts(ctx, ids)
	heads := m.capabilityHeads(ctx)
	region := m.stationRegionID(ctx, stationID)
	listed := m.regionListedUnits(ctx, region, ids)
	unsold := m.corpUnsoldUnits(ctx, c.CorpID, ids)
	hist := m.marketHistory(ctx)
	var globalListed map[int64]int64
	if hist.Ready {
		globalListed = m.globalListedUnits(ctx, ids)
	}

	thinOn := hullGateOn(ctx, m, hullGateThinFloodKey)

	keep := rows[:0]
	var why []string
	for _, r := range rows {
		k := kinds[r.ProductTypeID]
		name := m.typeNameOr(ctx, r.ProductTypeID)
		supply := floodSupply(listed[r.ProductTypeID], heldOf(heads, r.ProductTypeID))
		perDay := hist.regionPerDay(region, r.ProductTypeID)
		// A hull above a frigate is a thin market (hullgates.go): judged against the roster's headcount for it.
		thin := thinOn && k.Category == catShip && isThinHullGroup(k.Group)
		wanted := users[r.ProductTypeID] * capabilityUnitsPerPilot(k.Category)
		histFlooded, histDecided := floodCover(thin, hist.Ready && region != 0, perDay,
			listed[r.ProductTypeID], supply, wanted, mktBuildCoverDays)
		// No record in the region: the cluster's (mktdaily.go globalFlood).
		var globFlooded, globDecided bool
		var globPerDay float64
		var globSupply int64
		if !histDecided {
			globPerDay = hist.globalPerDay(r.ProductTypeID)
			globSupply = floodSupply(globalListed[r.ProductTypeID], heldOf(heads, r.ProductTypeID))
			globFlooded, globDecided = floodCover(thin, hist.Ready, globPerDay, globalListed[r.ProductTypeID], globSupply,
				wanted, mktGlobalCoverDays)
		}
		switch {
		case k.Group == groupShuttle:
			why = append(why, name+" (nobody buys a shuttle)")
		case censusUnused(heads, r.ProductTypeID):
			why = append(why, name+" (nobody fits, loads or flies one)")
		case unsold[r.ProductTypeID] >= int64(max(r.Batch, 1)):
			why = append(why, fmt.Sprintf("%s (%d still unsold)", name, unsold[r.ProductTypeID]))
		case histFlooded && perDay > 0:
			why = append(why, fmt.Sprintf("%s (%d listed or held, %.0f days of the region's buying)",
				name, supply, float64(supply)/perDay))
		case histFlooded:
			why = append(why, fmt.Sprintf("%s (%d listed in the region, none bought in %d days)",
				name, listed[r.ProductTypeID], mktRegionWindowDays))
		case globFlooded && globPerDay > 0:
			why = append(why, fmt.Sprintf("%s (%d listed or held across the cluster, %.0f days of its buying)",
				name, globSupply, float64(globSupply)/globPerDay))
		case globFlooded:
			why = append(why, fmt.Sprintf("%s (%d listed across the cluster, none bought anywhere in %d days)",
				name, globalListed[r.ProductTypeID], mktRegionWindowDays))
		case !histDecided && !globDecided && productFlooded(supply,
			users[r.ProductTypeID]*capabilityUnitsPerPilot(k.Category)):
			why = append(why, fmt.Sprintf("%s (%d listed in the region, %d held by the roster)",
				name, listed[r.ProductTypeID], heldOf(heads, r.ProductTypeID)))
		default:
			if histDecided {
				r.MktCapRuns = historyCapRuns(math.MaxInt32, true, perDay, supply, max(r.Portion, 1))
			}
			keep = append(keep, r)
		}
	}
	if len(why) > 0 && clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "glut") >= corpGlutLogSec {
		shown := why
		if len(shown) > 5 {
			shown = append(shown[:5:5], fmt.Sprintf("%d more", len(why)-5))
		}
		m.ceoLog(ctx, c, powerProduction, "glut", 0, 0,
			"left %d print(s) off the board — %s", len(why), strings.Join(shown, "; "))
	}
	return keep
}

// regionListedUnits is the units of each type on sell orders in one region, from the roster-wide
// seller snapshot. Crafted units only: NPC seeder asks restock every tick, so they are not supply
// against demand. Empty on any error, so a failed read never looks like a flood.
func (m *Mod) regionListedUnits(ctx context.Context, regionID int64, ids []int64) map[int64]int64 {
	out := map[int64]int64{}
	if regionID == 0 {
		return out
	}
	all, err := m.allSellerStations(ctx)
	if err != nil {
		return out
	}
	for _, id := range ids {
		for _, s := range all[id] {
			if s.RegionID == regionID {
				out[id] += s.CraftedUnits
			}
		}
	}
	return out
}

// globalListedUnits is regionListedUnits over every region: the crafted units
// of each type on sell orders anywhere, for the cluster's flood cut
// (mktdaily.go globalFlood). Empty on any error, for the same reason.
func (m *Mod) globalListedUnits(ctx context.Context, ids []int64) map[int64]int64 {
	out := map[int64]int64{}
	all, err := m.allSellerStations(ctx)
	if err != nil {
		return out
	}
	for _, id := range ids {
		for _, s := range all[id] {
			out[id] += s.CraftedUnits
		}
	}
	return out
}

// corpUnsoldUnits is what the corporation holds of each type and has not
// sold: units in any of its offices (outside a job) plus units still on its
// open sell orders.
func (m *Mod) corpUnsoldUnits(ctx context.Context, corpID int64, ids []int64) map[int64]int64 {
	out := map[int64]int64{}
	if len(ids) == 0 {
		return out
	}
	args := append([]any{corpID}, int64Args(ids)...)
	if rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity)
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		 WHERE o.corporationID = ? AND e.flag <> 111 AND e.typeID IN (`+placeholders(len(ids))+`)
		 GROUP BY e.typeID`, args...); err == nil {
		for rows.Next() {
			var id, n int64
			if rows.Scan(&id, &n) == nil {
				out[id] += n
			}
		}
		rows.Close()
	}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, SUM(o.volRemaining)
		  FROM rolebot_corp_orders co
		  JOIN market_orders o ON o.orderID = co.orderID
		 WHERE co.corporationID = ? AND o.bid = 0 AND o.orderState = 1
		   AND o.typeID IN (`+placeholders(len(ids))+`)
		 GROUP BY o.typeID`, args...); err == nil {
		for rows.Next() {
			var id, n int64
			if rows.Scan(&id, &n) == nil {
				out[id] += n
			}
		}
		rows.Close()
	}
	return out
}

// poPick is one print weighed for the library.
type poPick struct {
	BPType        int64
	Value, Score  float64
	HeldElsewhere bool
}

// poScore is a print's worth to the library: margin a run × log(1 + pilots
// who could use it), ×1.5 for a hull when the outfit builds ships. Pure.
func poScore(margin float64, wanted int64, hull bool) float64 {
	s := margin * math.Log1p(float64(max(wanted, 0)))
	if hull {
		s *= 1.5
	}
	return s
}

// poLedgerMul is the ledger term on poScore: x(1 + corpLedgerScoreMul) for a
// type the roster near the corporation has recorded itself unable to buy.
// Pure.
func poLedgerMul(demanded bool) float64 {
	if demanded {
		return 1 + corpLedgerScoreMul
	}
	return 1
}

// choosePOPick takes the best-scoring print, passing over one another
// managed corporation's library holds when a fresh one scores within
// corpPOSpreadTolerance of it. Pure, for the test.
func choosePOPick(picks []poPick) *poPick {
	var best, fresh *poPick
	for i := range picks {
		pk := &picks[i]
		if best == nil || pk.Score > best.Score {
			best = pk
		}
		if !pk.HeldElsewhere && (fresh == nil || pk.Score > fresh.Score) {
			fresh = pk
		}
	}
	if best != nil && best.HeldElsewhere && fresh != nil && fresh.Score >= best.Score*(1-corpPOSpreadTolerance) {
		return fresh
	}
	return best
}

// printsHeldElsewhere is every print type some other managed corporation's library holds or has an
// open order for, so two houses do not buy the same print.
func (m *Mod) printsHeldElsewhere(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	ids, err := int64Column(ctx, m, `
		SELECT bpTypeID FROM rolebot_corp_library WHERE corporationID <> ?
		UNION
		SELECT bpTypeID FROM rolebot_corp_po WHERE corporationID <> ? AND status = 'open'`, corpID, corpID)
	if err == nil {
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}

// boardExtras are the non-order lines of the Production Board: what the
// corporation is buying, and what it has replaced lately.
func (m *Mod) boardExtras(ctx context.Context, corpID int64, ch Charter) []string {
	var out []string
	if bids := m.buybackSummary(ctx, corpID); len(bids) > 0 {
		out = append(out, "<br><b>The corporation buys</b> (sell into its buy orders at the factory):")
		out = append(out, bids...)
	}
	out = append(out, `<br>The live board is on your Corporate Portal: <a href="`+esp.IGBHost()+`/corp/industry">Industry</a>.`)
	if ch.SRPCap > 0 {
		out = append(out, fmt.Sprintf("<br>Ship replacement: hulls up to %s lost flying for the corporation are replaced from its own production.", iskString(ch.SRPCap)))
	}
	return out
}

// ── the roster's unmet demand ───────────────────────────────────────────────

// ledgerWant is one row of the unmet-demand ledger a corporation may answer.
type ledgerWant struct {
	TypeID, BPTypeID int64
	Units            int64 // the decayed want (demandledger.go)
	Portion          int64
}

// corpLedgerWants reads the ledger rows a corporation may take: blueprint-backed, still wanted,
// asked for recently, unclaimed (or claimed by this corporation, claimant -corpID), and last asked
// for in a region it works in (srpRegions). A row with no region counts everywhere. Biggest first.
func (m *Mod) corpLedgerWants(ctx context.Context, corpID int64) []ledgerWant {
	now := clock.Unix()
	w := demandWantedSQL("d", now)
	args := []any{-corpID, now - industryClaimTTLSec, now - industryDemandMaxAgeSec}
	where := "d.lastRegionID = 0"
	if regions := m.srpRegions(ctx, corpID); len(regions) > 0 {
		ids := make([]int64, 0, len(regions))
		for r := range regions {
			ids = append(ids, r)
		}
		where = "(d.lastRegionID = 0 OR d.lastRegionID IN (" + placeholders(len(ids)) + "))"
		args = append(args, int64Args(ids)...)
	}
	args = append(args, corpLedgerDemandRows)
	rows, err := m.db.QueryContext(ctx, `
		SELECT d.typeID, bt.blueprintTypeID, `+w+` AS w, COALESCE(t.portionSize, 1)
		  FROM rolebot_market_demand d
		  JOIN invBlueprintTypes bt ON bt.productTypeID = d.typeID
		  JOIN invTypes t ON t.typeID = d.typeID
		 WHERE `+w+` >= 1
		   AND (d.claimedBy = 0 OR d.claimedBy = ? OR d.claimedAt < ?)
		   AND d.lastAt > ?
		   AND `+where+`
		 ORDER BY w DESC, d.typeID
		 LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ledgerWant
	seen := map[int64]bool{}
	for rows.Next() {
		var lw ledgerWant
		if rows.Scan(&lw.TypeID, &lw.BPTypeID, &lw.Units, &lw.Portion) != nil || seen[lw.TypeID] {
			continue
		}
		seen[lw.TypeID] = true
		lw.Portion = max(lw.Portion, 1)
		out = append(out, lw)
	}
	return out
}

// ledgerDemand is the roster's unmet demand near the corporation as internal
// demand for the work board: Purpose "demand", priority 40, the want in runs.
// postBuildOrders prices it, sizes it to the money (ledgerDemandRuns), judges
// it on its labour beside the market, and claims each row it posts.
func (m *Mod) ledgerDemand(ctx context.Context, c *corpCEO) []buildPlanRow {
	var out []buildPlanRow
	for _, w := range m.corpLedgerWants(ctx, c.CorpID) {
		runs := int((w.Units + w.Portion - 1) / w.Portion)
		if runs < 1 {
			continue
		}
		out = append(out, buildPlanRow{BPTypeID: w.BPTypeID, ProductTypeID: w.TypeID,
			Wanted: runs, Priority: corpDemandPriority, Purpose: corpDemandPurpose})
	}
	return out
}

// filterByLabour applies worthTheLabour to the demand and market rows
// together: the hull median is taken over both, so a wanted module is held to
// the same bar as a market one.
func (m *Mod) filterByLabour(ctx context.Context, demand, market []buildPlanRow, budget float64) ([]buildPlanRow, []buildPlanRow) {
	ids := make([]int64, 0, len(demand)+len(market))
	for _, r := range demand {
		ids = append(ids, r.ProductTypeID)
	}
	for _, r := range market {
		ids = append(ids, r.ProductTypeID)
	}
	kinds := m.typeKinds(ctx, ids)
	label := func(rs []buildPlanRow) []laborRow {
		out := make([]laborRow, 0, len(rs))
		for _, r := range rs {
			k := kinds[r.ProductTypeID]
			out = append(out, laborRow{Row: r, Hull: isRealHull(k.Category, k.Group), Runs: sizeBuildOrder(r, budget)})
		}
		return out
	}
	ld, lm := label(demand), label(market)
	median, ok := hullMedianPPH(append(append([]laborRow{}, ld...), lm...))
	return worthTheLabour(ld, median, ok), worthTheLabour(lm, median, ok)
}
