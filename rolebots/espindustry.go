package rolebots

// The Industry board: /rolebots/industry.
//
// Other pages see manufacturing after a job exists (/industry/factories, /industry/blueprints,
// the Stats board, a pilot's Factory page). This page owns the pipeline before the install,
// the supply feeding it and the reasons, which live only as prose in rolebot_log. It does not
// list running jobs, lines or blueprint types.
//
// Nothing is stored except a short memo of the assembled board. Every figure is recomputed
// from the tables the roster acts on, so it cannot drift from what the bots obey.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// industryPageSize is how many project rows one page carries. The same
// reasoning as wantsPageSize: three thousand rows of a dozen columns is a
// megabyte of HTML and nobody reads past the interesting end.
const industryPageSize = 150

// industryMemoTTL is how long an assembled board is reused, in seconds.
const industryMemoTTL = 60

// The three shortlists. Each answers which handful of things is going wrong, not what is there.
const (
	industryBlockedRows = 25
	industryChokeRows   = 30
)

// ---------------------------------------------------------------------------
// Stages
// ---------------------------------------------------------------------------

// A project's stage is its effective phase: Data.NextPhase while the pilot is in transit.
// The rungs are not the phase list: detours outside industry.* (fitting a mining ship, a fleet
// secondment) still hold a project, so they get named rungs instead of folding into other.
const (
	indStageBuyBPO    = "buybpo"
	indStageGather    = "refine"
	indStageBuyMats   = "buymats"
	indStageMining    = "mining"
	indStageInstall   = "install"
	indStageFinish    = "finish"
	indStageProcure   = "procure"
	indStageGang      = "gang"
	indStageIdle      = "idle"
	indStageElsewhere = "elsewhere"
)

// indStageDef is one rung, in pipeline order.
type indStageDef struct{ Key, Label, Note string }

var indStageDefs = []indStageDef{
	{indStageBuyBPO, "Buying the blueprint", "at a seller's station, or flying to one"},
	{indStageGather, "At the factory, gathering", "unloading, refining, deciding dig or buy"},
	{indStageBuyMats, "Shopping for materials", "buying lines the belts cannot cover"},
	{indStageMining, "Mining its own materials", "trip cap " + strconv.Itoa(industryMaxMiningTrips)},
	{indStageInstall, "Installing", "at the line with the bill of materials"},
	{indStageFinish, "Collecting and listing", "job done, listing it for sale"},
	{indStageProcure, "Fitting out for the trip", "buying or fitting the hull the project needs"},
	{indStageGang, "On fleet duty", "seconded to a gang, project waits"},
	{indStageIdle, "Parked", "idle with a project open"},
	{indStageElsewhere, "Elsewhere", "doing something else entirely"},
}

// industryStageFor maps an effective phase onto a rung.
func industryStageFor(eff string) string {
	switch eff {
	case string(TaskIndBuyBPO):
		return indStageBuyBPO
	case string(TaskIndRefine):
		return indStageGather
	case string(TaskIndBuyMats):
		return indStageBuyMats
	case string(TaskIndInstall), string(TaskIndCorpJob):
		return indStageInstall
	case string(TaskIndCollect), string(TaskIndSell):
		return indStageFinish
	case "idle", "":
		return indStageIdle
	}
	switch {
	case strings.HasPrefix(eff, "mining."):
		return indStageMining
	case strings.HasPrefix(eff, "procure."):
		return indStageProcure
	case strings.HasPrefix(eff, "gang."):
		return indStageGang
	}
	return indStageElsewhere
}

// effectivePhase unfolds a travelling pilot onto the phase it is flying to.
func effectivePhase(phase string, d *StateData) string {
	if (phase == "travel" || strings.HasPrefix(phase, "travel.")) && d.NextPhase != "" {
		return d.NextPhase
	}
	return phase
}

// indEvidenceTiers is the build book's tier list in the order buildbook.go weights them. It is
// spelled out, not derived from the projects on screen, so a tier nobody builds on is selectable.
var indEvidenceTiers = []string{
	evidenceShelf, evidenceOwned, evidenceCapability, evidenceLedger, evidenceBid,
}

func indStageLabel(key string) string {
	for _, d := range indStageDefs {
		if d.Key == key {
			return d.Label
		}
	}
	return key
}

// ---------------------------------------------------------------------------
// View types
// ---------------------------------------------------------------------------

// indProject is one in-flight project dressed for the page.
type indProject struct {
	CharID    int64
	Name      string
	TypeID    int64
	Product   string
	Evidence  string
	Why       string
	Runs      int
	StageKey  string
	Stage     string
	Doing     string
	AgeSec    int64
	Trips     int
	Spent     float64
	FactoryID int64
	Factory   string
	RegionID  int64
	Region    string
	Halted    bool
	Stalled   bool // past the abandonment clock; this ISK has a deadline on it
	TripCap   bool // at or over the per-project mining trip cap
}

func (p indProject) SpentISK() string { return iskShort(p.Spent) }
func (p indProject) Age() string      { return agoString(p.AgeSec) }
func (p indProject) TripsStr() string {
	return fmt.Sprintf("%d/%d", p.Trips, industryMaxMiningTrips)
}

// Flag is the one-glance warning column. A stalled project is the expensive
// case: industryResume abandons anything that has made no progress in a day,
// so every row marked here is sunk ISK about to be written off.
func (p indProject) Flag() string {
	switch {
	case p.Halted:
		return "halted"
	case p.Stalled:
		return "stalling"
	case p.TripCap:
		return "trip cap"
	}
	return ""
}

// indFunnelRow is one rung with the population standing on it.
type indFunnelRow struct {
	Key, Label, Note string
	Count            int
	MedianAgeSec     int64
	Over24           int
	Spent            float64
	AvgTrips         float64
}

func (f indFunnelRow) MedianAge() string {
	if f.Count == 0 {
		return "—"
	}
	return agoString(f.MedianAgeSec)
}
func (f indFunnelRow) SpentISK() string { return iskShort(f.Spent) }
func (f indFunnelRow) TripsStr() string { return fmt.Sprintf("%.1f", f.AvgTrips) }

// indOvenSummary is the pipeline half that has reached a line, kept to a few numbers: the job
// list belongs to /industry/factories. Delivered is a module-side flag set when industryOvenSweep
// takes goods off the line remotely: finished, unsold, waiting in a hangar.
type indOvenSummary struct {
	Baking            int
	Delivered         int
	Pilots            int
	HaltedWithProject int
}

// indDayRow is one day of throughput: the funnel's exit rate, not its contents.
type indDayRow struct {
	Day       string
	Installed int
	Delivered int
}

// indBidRow is one type the factories have standing money out for.
type indBidRow struct {
	TypeID   int64
	TypeName string
	Bids     int
	Stations int
	Escrow   float64
	Units    int64
	Entered  int64
	OldestAt int64
}

func (b indBidRow) EscrowISK() string { return iskShort(b.Escrow) }
func (b indBidRow) UnitsStr() string  { return fmtUnits(b.Units) }
func (b indBidRow) Oldest() string {
	if b.OldestAt == 0 {
		return "—"
	}
	return agoString(clock.Unix() - b.OldestAt)
}

// FillPct is how much of what was asked for has actually been flown in. A
// column of zeroes is the whole point of the card: the money is on the counter
// and nobody is answering it.
func (b indBidRow) FillPct() int {
	if b.Entered <= 0 {
		return 0
	}
	filled := b.Entered - b.Units
	if filled < 0 {
		filled = 0
	}
	return int(filled * 100 / b.Entered)
}

// indBlockedRow is one material line the projects are waiting on.
type indBlockedRow struct {
	TypeID   int64
	TypeName string
	Units    int64
	Stations int
	Asks     int64
	OnMarket int64
	BidOut   bool
	Mineral  bool
}

func (b indBlockedRow) UnitsStr() string  { return fmtUnits(b.Units) }
func (b indBlockedRow) MarketStr() string { return fmtUnits(b.OnMarket) }

// Basis says on the row why the two halves cannot be compared. rolebot_mineral_demand is
// last-known (an upsert per station, TTL-pruned). rolebot_market_demand is a decaying level
// (demandledger.go): each failed purchase adds at most once a day and the total halves weekly.
func (b indBlockedRow) Basis() string {
	if b.Mineral {
		return "short now"
	}
	return "unmet asks, 7-day half-life"
}

// Verdict is the reading, not the numbers. "Nothing on sale anywhere" and
// "plenty on sale, nobody flying it" are opposite problems with opposite
// fixes, and the raw columns do not tell them apart at a glance.
func (b indBlockedRow) Verdict() string {
	switch {
	case b.OnMarket == 0 && b.BidOut:
		return "nothing on sale; a factory has money out for it"
	case b.OnMarket == 0:
		return "nothing on sale anywhere in the game"
	case b.BidOut:
		return "on sale somewhere; a factory is bidding for it"
	}
	return "on sale somewhere, but not where it is wanted"
}

// indBoard is everything the page draws, assembled once and memoised.
type indBoard struct {
	Projects   []indProject
	Funnel     []indFunnelRow
	Oven       indOvenSummary
	Throughput []indDayRow
	Bids       []indBidRow
	Blocked    []indBlockedRow
	TotalSpent float64
	Stalled    int
	Roster     int
	BuiltAt    int64

	// Soft failures. A board missing one card is still a board; a board that
	// 500s because a shard predates one of these tables is nothing.
	ThroughputErr string
	BidsErr       string
	BlockedErr    string
}

func (b *indBoard) SpentISK() string { return iskShort(b.TotalSpent) }

// ---------------------------------------------------------------------------
// The spine
// ---------------------------------------------------------------------------

// buildIndustryBoard decodes every state blob on the roster and assembles the board. The full
// decode is the only read of a project: its progress lives in rolebot_state.data as JSON, and
// industryActiveProjects (buildbook.go) scans it the same way on every commit.
func (m *Mod) buildIndustryBoard(ctx context.Context) (*indBoard, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID, s.phase, s.data, s.halted,
		       COALESCE(c.characterName, '')
		  FROM rolebot_state s
		  JOIN rolebot_characters b ON b.characterID = s.characterID AND b.enabled = 1
		  LEFT JOIN character_ c ON c.characterID = s.characterID`)
	if err != nil {
		return nil, fmt.Errorf("industry board spine: %w", err)
	}
	defer rows.Close()

	now := clock.Unix()
	b := &indBoard{BuiltAt: now}
	stationIDs := map[int64]bool{}
	ovenPilots := map[int64]bool{}

	for rows.Next() {
		var charID, halted int64
		var phase, name string
		var blob sql.NullString
		if err := rows.Scan(&charID, &phase, &blob, &halted, &name); err != nil {
			return nil, fmt.Errorf("industry board scan: %w", err)
		}
		b.Roster++
		if !blob.Valid || blob.String == "" {
			continue
		}
		var d StateData
		if json.Unmarshal([]byte(blob.String), &d) != nil {
			continue
		}

		// A pilot can hold a project and have jobs baking: an installed job moves to the cooking
		// list and frees the slot, so the oven is counted independently of the funnel.
		for _, j := range d.IndCooking {
			ovenPilots[charID] = true
			if j.Delivered {
				b.Oven.Delivered++
			} else {
				b.Oven.Baking++
			}
		}
		if d.IndTypeID == 0 {
			continue
		}
		if halted != 0 {
			b.Oven.HaltedWithProject++
		}

		eff := effectivePhase(phase, &d)
		stage := industryStageFor(eff)
		// Server time, the clock industryStalled reads (downtime.go).
		age := liveSince(d.IndStartedAt, now)
		p := indProject{
			CharID: charID, Name: name,
			TypeID: d.IndTypeID, Product: d.IndTypeName,
			Evidence: d.IndEvidence, Why: evidenceLabel(d.IndEvidence),
			Runs:     d.IndRuns,
			StageKey: stage, Stage: indStageLabel(stage),
			// The module's own prose: detailIndustry answers for industry.* phases and returns "" on a
			// detour, where the stage label takes over. Do not restate the sentences here.
			Doing:     detailIndustry(nil, nil, &SimState{Phase: eff, Data: d}),
			AgeSec:    age,
			Trips:     d.IndMiningTrips,
			Spent:     d.IndSpentISK,
			FactoryID: d.IndFactoryID,
			Halted:    halted != 0,
			Stalled:   age > industryStallSec,
			TripCap:   d.IndMiningTrips >= industryMaxMiningTrips,
		}
		if p.Name == "" {
			p.Name = fmt.Sprintf("character %d", charID)
		}
		if p.Product == "" {
			p.Product = fmt.Sprintf("type %d", d.IndTypeID)
		}
		if p.Stalled {
			b.Stalled++
		}
		b.TotalSpent += p.Spent
		if p.FactoryID != 0 {
			stationIDs[p.FactoryID] = true
		}
		b.Projects = append(b.Projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("industry board spine: %w", err)
	}
	b.Oven.Pilots = len(ovenPilots)

	// Station names in one query: stationLabel is a query per call and this page has thousands of rows.
	m.fillFactoryLabels(ctx, b.Projects, stationIDs)

	// Oldest first: the top of this table is the ISK with a deadline on it.
	sort.SliceStable(b.Projects, func(i, j int) bool {
		return b.Projects[i].AgeSec > b.Projects[j].AgeSec
	})
	b.Funnel = industryFunnel(b.Projects)

	if tp, terr := m.industryThroughput(ctx); terr != nil {
		b.ThroughputErr = terr.Error()
		m.logBot(0, logError, "industry", "industry board throughput: %v", terr)
	} else {
		b.Throughput = tp
	}
	bids, berr := m.industrySupplyBids(ctx)
	if berr != nil {
		b.BidsErr = berr.Error()
		m.logBot(0, logError, "industry", "industry board supply bids: %v", berr)
	} else {
		b.Bids = bids
	}
	if blocked, lerr := m.industryBlockedMaterials(ctx, bids); lerr != nil {
		b.BlockedErr = lerr.Error()
		m.logBot(0, logError, "industry", "industry board blocked lines: %v", lerr)
	} else {
		b.Blocked = blocked
	}
	return b, nil
}

// fillFactoryLabels resolves every distinct factory station in one pass.
func (m *Mod) fillFactoryLabels(ctx context.Context, ps []indProject, ids map[int64]bool) {
	if len(ids) == 0 {
		return
	}
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for id := range ids {
		ph = append(ph, "?")
		args = append(args, id)
	}
	type stn struct {
		name     string
		regionID int64
	}
	names := map[int64]stn{}
	rows, err := m.db.QueryContext(ctx,
		`SELECT stationID, stationName, COALESCE(regionID, 0)
		   FROM staStations WHERE stationID IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		m.logBot(0, logError, "industry", "industry board station names: %v", err)
		return
	}
	for rows.Next() {
		var id, region int64
		var name string
		if rows.Scan(&id, &name, &region) == nil {
			names[id] = stn{name, region}
		}
	}
	rows.Close()

	regions, _ := m.regionNames(ctx)
	for i := range ps {
		s, ok := names[ps[i].FactoryID]
		if !ok {
			if ps[i].FactoryID != 0 {
				ps[i].Factory = fmt.Sprintf("station %d", ps[i].FactoryID)
			}
			continue
		}
		ps[i].Factory = s.name
		ps[i].RegionID = s.regionID
		ps[i].Region = regions[s.regionID]
	}
}

// industryFunnel counts the population on each rung. Age is reported as a
// MEDIAN, not a mean: one project stuck for a fortnight drags a mean clean off
// the rung everybody else is actually standing on.
func industryFunnel(ps []indProject) []indFunnelRow {
	byStage := map[string][]indProject{}
	for _, p := range ps {
		byStage[p.StageKey] = append(byStage[p.StageKey], p)
	}
	out := make([]indFunnelRow, 0, len(indStageDefs))
	for _, def := range indStageDefs {
		group := byStage[def.Key]
		row := indFunnelRow{Key: def.Key, Label: def.Label, Note: def.Note, Count: len(group)}
		if len(group) == 0 {
			out = append(out, row)
			continue
		}
		ages := make([]int64, 0, len(group))
		trips := 0
		for _, p := range group {
			ages = append(ages, p.AgeSec)
			row.Spent += p.Spent
			trips += p.Trips
			if p.AgeSec > industryStallSec {
				row.Over24++
			}
		}
		sort.Slice(ages, func(i, j int) bool { return ages[i] < ages[j] })
		row.MedianAgeSec = ages[len(ages)/2]
		row.AvgTrips = float64(trips) / float64(len(group))
		out = append(out, row)
	}
	return out
}

// industryThroughput is the funnel's exit: installs and deliveries per day over the last
// week, as a rate and not a job list. ramJobs times are Win32 FILETIME ticks, so the
// conversion happens in SQL for the GROUP BY to land on a day.
func (m *Mod) industryThroughput(ctx context.Context) ([]indDayRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT DATE(FROM_UNIXTIME(installTime/10000000 - 11644473600)) AS d,
		       COUNT(*),
		       SUM(CASE WHEN completedStatusID = 101 THEN 1 ELSE 0 END)
		  FROM ramJobs
		 WHERE activityID = 1
		   AND installTime > (? + 11644473600) * 10000000
		 GROUP BY d ORDER BY d DESC`, clock.Unix()-7*86400)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []indDayRow
	for rows.Next() {
		var r indDayRow
		var day sql.NullString
		if err := rows.Scan(&day, &r.Installed, &r.Delivered); err != nil {
			return nil, err
		}
		r.Day = day.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// industrySupplyBids reads the standing buy orders industrybids.go leaves on a factory's book
// at commit: bill lines the pilot can neither dig nor buy in reach. The escrow join is the one
// minerals.go and espminerals.go share: rolebot_orders is what the module placed, market_orders
// is what is resting.
func (m *Mod) industrySupplyBids(ctx context.Context) ([]indBidRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.typeID, MAX(r.typeName),
		       COUNT(*), COUNT(DISTINCT r.stationID),
		       COALESCE(SUM(mo.price * mo.volRemaining), 0),
		       COALESCE(SUM(mo.volRemaining), 0),
		       COALESCE(SUM(r.volEntered), 0),
		       COALESCE(MIN(r.placedAt), 0)
		  FROM rolebot_orders r
		  JOIN market_orders mo ON mo.orderID = r.orderID AND mo.orderState = 1
		 WHERE r.bid = 1 AND r.closedAt = 0 AND r.kind = ?
		 GROUP BY r.typeID
		 ORDER BY 5 DESC`, tradeDealSupply)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []indBidRow
	for rows.Next() {
		var r indBidRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.Bids, &r.Stations,
			&r.Escrow, &r.Units, &r.Entered, &r.OldestAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// industryBlockedMaterials is what the live projects are waiting on, scoped to materials.
// Minerals come from rolebot_mineral_demand and the rest from rolebot_market_demand. The
// non-mineral half is filtered to real manufacturing inputs, which legitimately include ships
// (a Coercer is the hull a Heretic blueprint consumes).
func (m *Mod) industryBlockedMaterials(ctx context.Context, bids []indBidRow) ([]indBlockedRow, error) {
	bidOut := map[int64]bool{}
	for _, b := range bids {
		bidOut[b.TypeID] = true
	}

	byType := map[int64]*indBlockedRow{}
	var order []int64

	mins, err := m.db.QueryContext(ctx, `
		SELECT typeID, MAX(typeName), SUM(wanted), COUNT(DISTINCT stationID)
		  FROM rolebot_mineral_demand
		 GROUP BY typeID ORDER BY 3 DESC LIMIT ?`, industryBlockedRows)
	if err != nil {
		return nil, err
	}
	for mins.Next() {
		r := &indBlockedRow{Mineral: true}
		if mins.Scan(&r.TypeID, &r.TypeName, &r.Units, &r.Stations) == nil {
			byType[r.TypeID] = r
			order = append(order, r.TypeID)
		}
	}
	mins.Close()
	if err := mins.Err(); err != nil {
		return nil, err
	}

	w := demandWantedSQL("d", clock.Unix())
	mats, err := m.db.QueryContext(ctx, `
		SELECT d.typeID, d.typeName, `+w+` AS w, d.asks
		  FROM rolebot_market_demand d
		 WHERE `+w+` > 0
		   AND EXISTS (SELECT 1 FROM tl2materialsfortypewithactivity mt
		                WHERE mt.requiredTypeID = d.typeID AND mt.activity = 1)
		 ORDER BY w DESC LIMIT ?`, industryBlockedRows)
	if err != nil {
		return nil, err
	}
	for mats.Next() {
		var r indBlockedRow
		if mats.Scan(&r.TypeID, &r.TypeName, &r.Units, &r.Asks) == nil {
			if have, ok := byType[r.TypeID]; ok {
				have.Asks += r.Asks
				continue
			}
			cp := r
			byType[cp.TypeID] = &cp
			order = append(order, cp.TypeID)
		}
	}
	mats.Close()
	if err := mats.Err(); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return nil, nil
	}

	// Is any of it on sale anywhere? That column separates nobody makes this from nobody hauls it.
	ph := make([]string, 0, len(order))
	args := make([]any, 0, len(order))
	for _, id := range order {
		ph = append(ph, "?")
		args = append(args, id)
	}
	live, err := m.db.QueryContext(ctx, `
		SELECT typeID, COALESCE(SUM(volRemaining), 0)
		  FROM market_orders
		 WHERE bid = 0 AND orderState = 1 AND volRemaining > 0
		   AND typeID IN (`+strings.Join(ph, ",")+`)
		 GROUP BY typeID`, args...)
	if err != nil {
		return nil, err
	}
	for live.Next() {
		var id, vol int64
		if live.Scan(&id, &vol) == nil {
			if r, ok := byType[id]; ok {
				r.OnMarket = vol
			}
		}
	}
	live.Close()

	// Banded, not ranked. The halves are counted differently (see Basis), so one ORDER BY would
	// let the cumulative half take the whole table. Minerals lead because a bill cannot be
	// rewritten around them; each half ranks within itself and has its own share of the row budget.
	var minerals, others []indBlockedRow
	for _, id := range order {
		r := byType[id]
		r.BidOut = bidOut[id]
		if r.Mineral {
			minerals = append(minerals, *r)
		} else {
			others = append(others, *r)
		}
	}
	byUnits := func(rs []indBlockedRow) {
		sort.SliceStable(rs, func(i, j int) bool { return rs[i].Units > rs[j].Units })
	}
	byUnits(minerals)
	byUnits(others)
	half := industryBlockedRows / 2
	if len(minerals) > half {
		minerals = minerals[:half]
	}
	// Whatever the mineral half did not use goes to the other one rather than
	// being lost: a shard with every mineral stocked should still fill the card.
	if cap := industryBlockedRows - len(minerals); len(others) > cap {
		others = others[:cap]
	}
	return append(minerals, others...), nil
}

// ---------------------------------------------------------------------------
// The memo
// ---------------------------------------------------------------------------

var industryMemo struct {
	mu      sync.Mutex
	board   *indBoard
	builtAt int64
}

// industryBoard answers from the memo unless it is stale or the caller forced
// a rebuild. On a failed rebuild it keeps whatever the last good pass produced
// rather than blanking the page: a stale answer labelled with its age beats no
// answer at all.
func (m *Mod) industryBoard(ctx context.Context, fresh bool) (*indBoard, int64) {
	industryMemo.mu.Lock()
	defer industryMemo.mu.Unlock()
	now := clock.Unix()
	if !fresh && industryMemo.board != nil && now-industryMemo.builtAt < industryMemoTTL {
		return industryMemo.board, industryMemo.builtAt
	}
	b, err := m.buildIndustryBoard(ctx)
	if err != nil {
		m.logBot(0, logError, "industry", "industry board: %v", err)
		return industryMemo.board, industryMemo.builtAt
	}
	industryMemo.board, industryMemo.builtAt = b, now
	return b, now
}

// clearIndustryMemo drops the cached board. Nothing on this page writes, so it
// exists for the roster-purge paths, which invalidate every figure at once.
func clearIndustryMemo() {
	industryMemo.mu.Lock()
	industryMemo.board, industryMemo.builtAt = nil, 0
	industryMemo.mu.Unlock()
	clearChokeMemo()
}

// ---------------------------------------------------------------------------
// The handler
// ---------------------------------------------------------------------------

func (m *Mod) handleIndustryBoard(w http.ResponseWriter, r *http.Request) {
	// A full state decode plus a log fold is a slow page on a big shard, and
	// the request context dies with the browser tab.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	q := r.URL.Query()
	board, builtAt := m.industryBoard(ctx, q.Get("fresh") == "1")
	if board == nil {
		espError(w, "rolebot-industry", "could not read the roster's industry state")
		return
	}

	stage, evidence := q.Get("stage"), q.Get("evidence")
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	atRisk := q.Get("stalled") == "1"
	regionID, _ := strconv.ParseInt(q.Get("region"), 10, 64)

	filtered := make([]indProject, 0, len(board.Projects))
	for _, p := range board.Projects {
		if stage != "" && p.StageKey != stage {
			continue
		}
		if evidence != "" && p.Evidence != evidence {
			continue
		}
		if atRisk && !p.Stalled && !p.Halted && !p.TripCap {
			continue
		}
		if regionID != 0 && p.RegionID != regionID {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(p.Product), needle) &&
			!strings.Contains(strings.ToLower(p.Name), needle) {
			continue
		}
		filtered = append(filtered, p)
	}

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 0 {
		page = 0
	}
	start := page * industryPageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + industryPageSize
	if end > len(filtered) {
		end = len(filtered)
	}

	win := industryChokeWindowFor(q.Get("win"))
	chokes, chokeLines, chokeErr, chokeAt, chokeBuilding := m.chokeBoard(win)
	chokeAge := ""
	if chokeAt != 0 {
		chokeAge = agoString(clock.Unix() - chokeAt)
	}

	espRender(w, "rolebots_industry.html", map[string]any{
		"Page":       "rolebot-industry",
		"Tab":        "pipeline",
		"Board":      board,
		"Rows":       filtered[start:end],
		"Matched":    len(filtered),
		"Total":      len(board.Projects),
		"PageNo":     page,
		"HasPrev":    page > 0,
		"HasNext":    end < len(filtered),
		"PrevQ":      industryPageQuery(q, page-1),
		"NextQ":      industryPageQuery(q, page+1),
		"Stages":     indStageDefs,
		"Evidences":  indEvidenceTiers,
		"Stage":      stage,
		"Evidence":   evidence,
		"Query":      q.Get("q"),
		"AtRisk":     atRisk,
		"Region":     regionID,
		"Chokes":     chokes,
		"ChokeLines": chokeLines,
		"ChokeErr":   chokeErr,
		// Age of the fold and whether a fresh one is running (chokeBoard).
		"ChokeAge":      chokeAge,
		"ChokeBuilding": chokeBuilding,
		"Win":           win.Key,
		"Windows":       industryChokeWindows,
		"AgeSecs":       clock.Unix() - builtAt,
		"Msg":           q.Get("msg"),
		"Err":           q.Get("err"),
	})
}

// industryPageQuery rebuilds the query string with a new page number, keeping
// every filter. A pager that dropped them would page through a different list
// than the one on screen.
func industryPageQuery(q url.Values, page int) string {
	out := make([]string, 0, 8)
	for _, k := range []string{"stage", "evidence", "q", "stalled", "region", "win"} {
		if v := q.Get(k); v != "" {
			out = append(out, k+"="+url.QueryEscape(v))
		}
	}
	out = append(out, "page="+strconv.Itoa(page))
	return "?" + strings.Join(out, "&")
}
