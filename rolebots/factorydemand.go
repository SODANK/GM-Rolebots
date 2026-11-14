package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"
)

// Factory demand: where the roster builds, and what it is short of there.
//
// An industrial that cannot cover a bill flies out to buy it or rests a supply bid. This file
// is the demand signal that lets a trader, a trade desk or a miner rest a sell order at a busy
// factory station, priced just under the industrial's alternative. One roster-wide read,
// memoised for factoryDemandTTL, gathers over factoryDemandWindowSec per station: jobs
// (ramJobs.containerID), shortfall (rolebot_mineral_demand and the material rows of
// rolebot_market_demand) and bids (open rolebot_orders of kind 'supply', industrybids.go).
// For each (station, type) the alternative is the cheaper of the nearest deep-enough ask
// within factoryAltReachJumps plus hauling, or the supply bid. Feed prices stay under it.

const (
	factoryDemandTTL       = 10 * time.Minute
	factoryDemandWindowSec = 7 * 86400
	// factoryDemandMaxStations bounds the stations whose alternative is priced (a BFS each);
	// the busiest factories carry nearly all the demand.
	factoryDemandMaxStations = 200
	// factoryAltReachJumps is how far an industrial would fly to buy instead
	// (shopSoftMaxJumps, the shopping trip's soft cap).
	factoryAltReachJumps = shopSoftMaxJumps
	// factoryAltMinShare is the least share of the factory's want an ask's
	// depth must cover to count as an alternative: ten units of Tritanium
	// are not an answer to a three-million-unit bill.
	factoryAltMinShare = 0.10
	// factoryAskUndercut is how far under the alternative a feed lists.
	factoryAskUndercut = 0.98
	// factoryDestMax is how many factory destinations a speculative search
	// costs beside the liquidity hubs.
	factoryDestMax = 6
	// factoryPostMinJobs is the bot job count (in the window) that makes a
	// station a factory worth an office for a trade corporation.
	factoryPostMinJobs = 50
	// specConfFactory is what a named factory's shortfall at the very
	// station adds to a listing's confidence: somebody is standing there
	// with a bill it could not cover.
	specConfFactory = 0.15
)

// factoryDemand is one (station, type) a factory there wants.
type factoryDemand struct {
	StationID int64
	SystemID  int64
	RegionID  int64
	TypeID    int64
	Units     int64   // units wanted
	AltPrice  float64 // the industrial's alternative per unit; 0 = none known
	BidPrice  float64 // its own standing supply bid; 0 = none
	Ref       float64 // AltPrice, else basePrice: what the want is worth, for ranking
	Jobs      int     // bot industry jobs installed at the station in the window
}

// factoryDemandBook is the memoised answer with its lookup.
type factoryDemandBook struct {
	Rows  []factoryDemand
	index map[[2]int64]int // (station, type) → Rows
}

func (fb *factoryDemandBook) at(stationID, typeID int64) (factoryDemand, bool) {
	if fb == nil {
		return factoryDemand{}, false
	}
	i, ok := fb.index[[2]int64{stationID, typeID}]
	if !ok {
		return factoryDemand{}, false
	}
	return fb.Rows[i], true
}

// factoryDemandStations is every (station, type) a bot factory wants, the
// busiest stations first. Shared and read-only.
func (m *Mod) factoryDemandStations(ctx context.Context) ([]factoryDemand, error) {
	fb, err := m.factoryDemandBook(ctx)
	if err != nil || fb == nil {
		return nil, err
	}
	return fb.Rows, nil
}

func (m *Mod) factoryDemandBook(ctx context.Context) (*factoryDemandBook, error) {
	return snapshotFor(snapKey("factory-demand"), factoryDemandTTL, func() (*factoryDemandBook, error) {
		sctx, cancel := snapContext(ctx)
		defer cancel()
		return m.loadFactoryDemand(sctx)
	})
}

// factoryAltAt is the industrial's alternative price for a type at a station,
// 0 when the station is not a factory that wants it.
func (m *Mod) factoryAltAt(ctx context.Context, stationID, typeID int64) float64 {
	fb, err := m.factoryDemandBook(ctx)
	if err != nil {
		return 0
	}
	if d, ok := fb.at(stationID, typeID); ok {
		return d.AltPrice
	}
	return 0
}

// factoryAskCap is the most a feed of this type may ask at this station:
// just under the industrial's alternative. 0 = no cap known.
func (m *Mod) factoryAskCap(ctx context.Context, stationID, typeID int64) float64 {
	if alt := m.factoryAltAt(ctx, stationID, typeID); alt > 0 {
		return alt * factoryAskUndercut
	}
	return 0
}

type factoryStation struct {
	systemID, regionID int64
	jobs               int
}

func (m *Mod) loadFactoryDemand(ctx context.Context) (*factoryDemandBook, error) {
	now := clock.Unix()
	since := now - factoryDemandWindowSec
	stations := map[int64]*factoryStation{}
	station := func(id, sys, reg int64) *factoryStation {
		s := stations[id]
		if s == nil {
			s = &factoryStation{systemID: sys, regionID: reg}
			stations[id] = s
		}
		if s.systemID == 0 {
			s.systemID, s.regionID = sys, reg
		}
		return s
	}

	// How busy each station's factory is: the roster's own jobs there.
	rows, err := m.db.QueryContext(ctx, capStatement(`
		SELECT j.containerID, s.solarSystemID, s.regionID, COUNT(*)
		  FROM ramJobs j
		  JOIN rolebot_characters b ON b.characterID = j.installerID
		  JOIN staStations s ON s.stationID = j.containerID
		 WHERE j.installTime >= ?
		 GROUP BY j.containerID, s.solarSystemID, s.regionID`), (since+11644473600)*10000000)
	if err != nil {
		return nil, fmt.Errorf("factory jobs: %w", err)
	}
	for rows.Next() {
		var id, sys, reg int64
		var n int
		if err := rows.Scan(&id, &sys, &reg, &n); err != nil {
			rows.Close()
			return nil, err
		}
		station(id, sys, reg).jobs = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	type want struct {
		short, bid int64
		bidPrice   float64
	}
	wants := map[[2]int64]*want{}
	wantOf := func(st, typeID int64) *want {
		k := [2]int64{st, typeID}
		w := wants[k]
		if w == nil {
			w = &want{}
			wants[k] = w
		}
		return w
	}

	// The mineral shortfall ledger: a named factory's uncovered bill.
	mrows, err := m.db.QueryContext(ctx, `
		SELECT d.typeID, d.stationID, COALESCE(s.solarSystemID, d.systemID), COALESCE(s.regionID, d.regionID), d.wanted
		  FROM rolebot_mineral_demand d
		  LEFT JOIN staStations s ON s.stationID = d.stationID
		 WHERE d.lastAt >= ? AND d.wanted > 0`, since)
	if err != nil {
		return nil, fmt.Errorf("factory shortfalls: %w", err)
	}
	for mrows.Next() {
		var typeID, st, sys, reg, n int64
		if err := mrows.Scan(&typeID, &st, &sys, &reg, &n); err != nil {
			mrows.Close()
			return nil, err
		}
		station(st, sys, reg)
		wantOf(st, typeID).short += n
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	// The open supply bids: a factory with money on its own counter.
	brows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, s.solarSystemID, s.regionID, o.typeID, SUM(o.volRemaining), MAX(o.price)
		  FROM rolebot_orders o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.kind = ? AND o.bid = 1 AND o.closedAt = 0 AND o.volRemaining > 0
		 GROUP BY o.stationID, s.solarSystemID, s.regionID, o.typeID`, tradeDealSupply)
	if err != nil {
		return nil, fmt.Errorf("factory supply bids: %w", err)
	}
	for brows.Next() {
		var st, sys, reg, typeID, n int64
		var price float64
		if err := brows.Scan(&st, &sys, &reg, &typeID, &n, &price); err != nil {
			brows.Close()
			return nil, err
		}
		station(st, sys, reg)
		w := wantOf(st, typeID)
		w.bid += n
		if price > w.bidPrice {
			w.bidPrice = price
		}
	}
	brows.Close()
	if err := brows.Err(); err != nil {
		return nil, err
	}

	// The material rows of the shopping ledger carry a system, not a station:
	// they go to the busiest bot factory in that system, or nowhere.
	busiest := map[int64]int64{} // system → station
	for id, s := range stations {
		if s.jobs == 0 {
			continue
		}
		if cur := busiest[s.systemID]; cur == 0 || s.jobs > stations[cur].jobs ||
			(s.jobs == stations[cur].jobs && id < cur) {
			busiest[s.systemID] = id
		}
	}
	drows, err := m.db.QueryContext(ctx, `
		SELECT d.typeID, d.lastSystemID, d.wanted
		  FROM rolebot_market_demand d
		  JOIN invTypes t ON t.typeID = d.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE g.categoryID = ? AND d.wanted > 0 AND d.lastAt >= ?`, catMaterial, since)
	if err != nil {
		return nil, fmt.Errorf("factory material demand: %w", err)
	}
	for drows.Next() {
		var typeID, sys, n int64
		if err := drows.Scan(&typeID, &sys, &n); err != nil {
			drows.Close()
			return nil, err
		}
		if st := busiest[sys]; st != 0 {
			wantOf(st, typeID).short += n
		}
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return nil, err
	}

	if len(wants) == 0 {
		return &factoryDemandBook{index: map[[2]int64]int{}}, nil
	}

	// The type facts every row needs: authored price and unit volume.
	typeSet := map[int64]bool{}
	for k := range wants {
		typeSet[k[1]] = true
	}
	typeIDs := make([]int64, 0, len(typeSet))
	for t := range typeSet {
		typeIDs = append(typeIDs, t)
	}
	sort.Slice(typeIDs, func(i, j int) bool { return typeIDs[i] < typeIDs[j] })
	base, vol := map[int64]float64{}, map[int64]float64{}
	trows, err := m.db.QueryContext(ctx, `
		SELECT typeID, COALESCE(basePrice, 0), COALESCE(volume, 0.01) FROM invTypes
		 WHERE typeID IN (`+placeholders(len(typeIDs))+`)`, int64Args(typeIDs)...)
	if err != nil {
		return nil, fmt.Errorf("factory demand types: %w", err)
	}
	for trows.Next() {
		var t int64
		var bp, v float64
		if trows.Scan(&t, &bp, &v) == nil {
			base[t], vol[t] = bp, v
		}
	}
	trows.Close()

	var out []factoryDemand
	for k, w := range wants {
		s := stations[k[0]]
		if s == nil || s.systemID == 0 {
			continue
		}
		out = append(out, factoryDemand{
			StationID: k[0], SystemID: s.systemID, RegionID: s.regionID, TypeID: k[1],
			Units: max(w.short, w.bid), BidPrice: w.bidPrice, Jobs: s.jobs,
			Ref: base[k[1]],
		})
	}
	out = keepBusiestFactories(out, factoryDemandMaxStations)

	// The alternative: the cheapest deep-enough ask in reach plus the trip.
	asks, err := m.factoryAskSystems(ctx, typeIDs)
	if err != nil {
		return nil, err
	}
	u, _ := m.universe(ctx)
	reachOf := map[int64]map[int64]int{}
	for i := range out {
		d := &out[i]
		best := 0.0
		if u != nil {
			reach, ok := reachOf[d.SystemID]
			if !ok {
				reach = bfsJumps(u, d.SystemID, factoryAltReachJumps)
				reachOf[d.SystemID] = reach
			}
			best = bestAskPlusTravel(asks[d.TypeID], reach, d.Units, vol[d.TypeID])
		}
		d.AltPrice = factoryAltPrice(best, d.BidPrice)
		if d.AltPrice > 0 {
			d.Ref = d.AltPrice
		}
	}
	sortFactoryDemand(out)
	fb := &factoryDemandBook{Rows: out, index: make(map[[2]int64]int, len(out))}
	for i, d := range out {
		fb.index[[2]int64{d.StationID, d.TypeID}] = i
	}
	return fb, nil
}

// sysAsk is one system's cheapest ask for a type and the depth behind it.
type sysAsk struct {
	SystemID int64
	Price    float64
	Vol      int64
}

func (m *Mod) factoryAskSystems(ctx context.Context, typeIDs []int64) (map[int64][]sysAsk, error) {
	out := map[int64][]sysAsk{}
	if len(typeIDs) == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, capStatement(`
		SELECT typeID, solarSystemID, MIN(price), SUM(volRemaining)
		  FROM market_orders
		 WHERE bid = 0 AND orderState = 1 AND volRemaining > 0 AND price > 0
		   AND typeID IN (`+placeholders(len(typeIDs))+`)
		 GROUP BY typeID, solarSystemID`), int64Args(typeIDs)...)
	if err != nil {
		return nil, fmt.Errorf("factory alternatives: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t int64
		var a sysAsk
		if err := rows.Scan(&t, &a.SystemID, &a.Price, &a.Vol); err != nil {
			return nil, err
		}
		out[t] = append(out[t], a)
	}
	return out, rows.Err()
}

// bestAskPlusTravel is the cheapest landed price an industrial could fetch
// a type for: over every system in its reach whose asks are deep enough
// (factoryAltMinShare of the want), the ask plus the hauling one unit costs
// at the board rate. 0 when nothing in reach will do. Pure.
func bestAskPlusTravel(asks []sysAsk, reach map[int64]int, units int64, unitVol float64) float64 {
	if unitVol <= 0 {
		unitVol = 0.01
	}
	need := int64(math.Ceil(factoryAltMinShare * float64(units)))
	best := 0.0
	for _, a := range asks {
		j, ok := reach[a.SystemID]
		if !ok || a.Price <= 0 || a.Vol < need {
			continue
		}
		landed := a.Price + haulISKPerM3Jump*unitVol*float64(j)
		if best == 0 || landed < best {
			best = landed
		}
	}
	return best
}

// factoryAltPrice is what the industrial would otherwise pay: the cheaper of
// fetching it and waiting on its own bid. 0 when it has neither. Pure.
func factoryAltPrice(askPlusTravel, supplyBid float64) float64 {
	switch {
	case askPlusTravel > 0 && supplyBid > 0:
		return math.Min(askPlusTravel, supplyBid)
	case askPlusTravel > 0:
		return askPlusTravel
	case supplyBid > 0:
		return supplyBid
	}
	return 0
}

// factoryAskPrice caps a feed's listing just under the industrial's
// alternative. alt 0 leaves the price alone. Pure.
func factoryAskPrice(list, alt float64) float64 {
	if alt > 0 {
		if cap := alt * factoryAskUndercut; list > cap {
			return math.Floor(cap*100) / 100
		}
	}
	return list
}

// factoryListPrice is what landed goods rest at in a factory station: a
// standing bid at or over the floor is taken; otherwise the asking margin,
// under any rival ask, capped under the alternative, and never under the
// floor margin. Pure.
func factoryListPrice(bestBid, bestAsk, landed, alt, askMargin, floorMargin float64) float64 {
	floor := math.Round(landed*(1+floorMargin)*100) / 100
	if bestBid > 0 && bestBid >= floor {
		return bestBid
	}
	price := landed * (1 + askMargin)
	if bestAsk > 0 && bestAsk*0.99 < price {
		price = bestAsk * 0.99
	}
	price = factoryAskPrice(price, alt)
	if price < floor {
		price = floor
	}
	return math.Round(price*100) / 100
}

// factoryStationValue is a station's want, valued, for ranking. Pure.
func factoryStationValue(d factoryDemand) float64 { return float64(d.Units) * d.Ref }

// factoryBusyWeight is how much a station's job count lifts its want. Pure.
func factoryBusyWeight(jobs int) float64 { return 1 + math.Log1p(float64(max(jobs, 0))) }

// keepBusiestFactories keeps the rows of the limit best stations by valued
// want × busyness. Pure.
func keepBusiestFactories(rows []factoryDemand, limit int) []factoryDemand {
	value, jobs := map[int64]float64{}, map[int64]int{}
	for _, d := range rows {
		value[d.StationID] += factoryStationValue(d)
		jobs[d.StationID] = d.Jobs
	}
	if len(value) <= limit {
		return rows
	}
	score := make(map[int64]float64, len(value))
	ids := make([]int64, 0, len(value))
	for id, v := range value {
		score[id] = v * factoryBusyWeight(jobs[id])
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if score[ids[i]] != score[ids[j]] {
			return score[ids[i]] > score[ids[j]]
		}
		return ids[i] < ids[j]
	})
	keep := map[int64]bool{}
	for _, id := range ids[:limit] {
		keep[id] = true
	}
	out := rows[:0:0]
	for _, d := range rows {
		if keep[d.StationID] {
			out = append(out, d)
		}
	}
	return out
}

// sortFactoryDemand orders rows busiest station first, then by valued want.
func sortFactoryDemand(rows []factoryDemand) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Jobs != b.Jobs {
			return a.Jobs > b.Jobs
		}
		if a.StationID != b.StationID {
			return a.StationID < b.StationID
		}
		if va, vb := factoryStationValue(a), factoryStationValue(b); va != vb {
			return va > vb
		}
		return a.TypeID < b.TypeID
	})
}

// factoryDest is one factory station a seller could stock, its rows folded.
type factoryDest struct {
	StationID int64
	SystemID  int64
	RegionID  int64
	Jobs      int
	Value     float64           // Σ units × Ref
	Want      map[int64]int64   // typeID → units wanted
	Alt       map[int64]float64 // typeID → the industrial's alternative
}

// groupFactoryDests folds demand rows into one destination per station, in
// first-seen order. Pure.
func groupFactoryDests(rows []factoryDemand) []factoryDest {
	idx := map[int64]int{}
	var out []factoryDest
	for _, d := range rows {
		i, ok := idx[d.StationID]
		if !ok {
			i = len(out)
			idx[d.StationID] = i
			out = append(out, factoryDest{StationID: d.StationID, SystemID: d.SystemID, RegionID: d.RegionID,
				Jobs: d.Jobs, Want: map[int64]int64{}, Alt: map[int64]float64{}})
		}
		f := &out[i]
		f.Want[d.TypeID] += d.Units
		if d.AltPrice > 0 {
			f.Alt[d.TypeID] = d.AltPrice
		}
		f.Value += factoryStationValue(d)
	}
	return out
}

// factoryDestScore ranks a factory destination for one seller: its valued
// want, lifted by how busy it is, discounted by the flying. Pure.
func factoryDestScore(value float64, jobs, jumps int) float64 {
	return value * factoryBusyWeight(jobs) / (1 + specDestJumpCost*float64(max(jumps, 0)))
}

// rankFactoryDests keeps the reachable destinations, best first, at most
// limit of them. Pure.
func rankFactoryDests(dests []factoryDest, jumps map[int64]int, limit int) []factoryDest {
	type scored struct {
		d     factoryDest
		score float64
	}
	var s []scored
	for _, d := range dests {
		j, ok := jumps[d.SystemID]
		if !ok || d.Value <= 0 {
			continue
		}
		s = append(s, scored{d, factoryDestScore(d.Value, d.Jobs, j)})
	}
	sort.Slice(s, func(i, j int) bool {
		if s[i].score != s[j].score {
			return s[i].score > s[j].score
		}
		return s[i].d.StationID < s[j].d.StationID
	})
	if len(s) > limit {
		s = s[:limit]
	}
	out := make([]factoryDest, len(s))
	for i := range s {
		out[i] = s[i].d
	}
	return out
}

// factorySpecDests adds the best factory-demand stations this pilot can
// reach to its speculative destinations, beside the liquidity hubs. A hub
// that is also a factory keeps its place and gains the factory's wants.
// Stations the pilot will not trade at (tradeStationClause) are left out.
func (m *Mod) factorySpecDests(ctx context.Context, b *botRun, cfg tradeConfig,
	jumps map[int64]int, have []specDest) []specDest {

	rows, err := m.factoryDemandStations(ctx)
	if err != nil || len(rows) == 0 {
		return have
	}
	ranked := rankFactoryDests(groupFactoryDests(rows), jumps, factoryDestMax*2)
	if len(ranked) == 0 {
		return have
	}
	ids := make([]int64, len(ranked))
	for i, d := range ranked {
		ids[i] = d.StationID
	}
	stationClause, stationArgs := m.tradeStationClause(ctx, b.Profile, b.Tune, cfg)
	allowed := map[int64]bool{}
	if srows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID FROM staStations s
		 WHERE s.stationID IN (`+placeholders(len(ids))+`)`+stationClause,
		append(int64Args(ids), stationArgs...)...); err == nil {
		for srows.Next() {
			var id int64
			if srows.Scan(&id) == nil {
				allowed[id] = true
			}
		}
		srows.Close()
	}
	at := map[int64]int{}
	for i, d := range have {
		at[d.StationID] = i
	}
	added := 0
	for _, f := range ranked {
		if !allowed[f.StationID] {
			continue
		}
		fd := f
		if i, ok := at[f.StationID]; ok {
			have[i].Factory = &fd
			continue
		}
		if added >= factoryDestMax {
			continue
		}
		have = append(have, specDest{StationID: f.StationID, SystemID: f.SystemID, RegionID: f.RegionID,
			Factory: &fd, FactoryOnly: true})
		added++
	}
	return have
}

// factoryEvidence words a factory feed for the log.
func factoryEvidence(units int64, alt float64) string {
	if alt > 0 {
		return fmt.Sprintf("a factory there is short of %d and would otherwise pay %s a unit", units, iskString(alt))
	}
	return fmt.Sprintf("a factory there is short of %d and nothing in its reach sells it", units)
}

// ── miners ──────────────────────────────────────────────────────────────────

// oreFactoryValue is what one unit of an ore is worth to the factory at this
// station, at the reference refiner's yield: every mineral the factory wants
// at the factory's alternative, the rest at the ordinary mineral price
// (other). 0 when the factory wants none of them. Pure.
func oreFactoryValue(v oreValue, yield float64, alt, other map[int64]float64) float64 {
	val, wanted := 0.0, false
	for mid, per := range v.PerUnit {
		p := other[mid]
		if a := alt[mid]; a > 0 {
			p, wanted = a, true
		}
		val += per * yield * p
	}
	if !wanted {
		return 0
	}
	return val
}

// oreFactoryValueAt is oreFactoryValue for an ore at a station, read off the
// pilot's own valuation regions. 0 when no factory there wants its minerals.
func (m *Mod) oreFactoryValueAt(ctx context.Context, b *botRun, stationID int64, v oreValue) float64 {
	alt := m.oreFactoryAlt(ctx, stationID, v)
	if len(alt) == 0 {
		return 0
	}
	other, err := m.mineralPrices(ctx, m.valuationRegions(ctx, b), oreValueBid)
	if err != nil {
		return 0
	}
	return oreFactoryValue(v, oreReferenceYield(), alt, other)
}

// oreFactoryAlt is the factory's alternative per mineral at a station, for
// the minerals this ore refines into. nil when the station wants none.
func (m *Mod) oreFactoryAlt(ctx context.Context, stationID int64, v oreValue) map[int64]float64 {
	fb, err := m.factoryDemandBook(ctx)
	if err != nil {
		return nil
	}
	var out map[int64]float64
	for mid := range v.PerUnit {
		if d, ok := fb.at(stationID, mid); ok && d.AltPrice > 0 {
			if out == nil {
				out = map[int64]float64{}
			}
			out[mid] = d.AltPrice
		}
	}
	return out
}

// oreFactoryOutlet picks where a hold of this ore is worth more than the local bid: the
// factory station within oreSellMaxJumps that wants its minerals most, valued at the factory's
// alternative less hauling. (0, 0) when none beats localBid. The travel lives in miningDispose.
func (m *Mod) oreFactoryOutlet(ctx context.Context, b *botRun, typeID int64, localBid float64) (int64, float64) {
	regions := m.valuationRegions(ctx, b)
	vals, err := m.oreValues(ctx, regions, oreValueBid)
	if err != nil {
		return 0, 0
	}
	v, ok := vals[typeID]
	if !ok {
		return 0, 0
	}
	other, err := m.mineralPrices(ctx, regions, oreValueBid)
	if err != nil {
		return 0, 0
	}
	fb, err := m.factoryDemandBook(ctx)
	if err != nil || fb == nil || len(fb.Rows) == 0 {
		return 0, 0
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return 0, 0
	}
	reach := bfsJumps(u, b.SystemID, oreSellMaxJumps)
	var cands []oreOutletCand
	seen := map[int64]bool{}
	for _, d := range fb.Rows {
		if seen[d.StationID] || d.StationID == b.StationID {
			continue
		}
		j, ok := reach[d.SystemID]
		if !ok {
			continue
		}
		seen[d.StationID] = true
		val := oreFactoryValue(v, oreReferenceYield(), m.oreFactoryAlt(ctx, d.StationID, v), other) / oreAskRefinerEdge()
		if val <= 0 {
			continue
		}
		cands = append(cands, oreOutletCand{StationID: d.StationID, Value: val, Jumps: j})
	}
	return pickOreFactoryOutlet(cands, localBid, v.Volume)
}

// oreOutletCand is one factory a miner could carry a hold to.
type oreOutletCand struct {
	StationID int64
	Value     float64 // per ore unit, what the factory's alternative leaves a lister
	Jumps     int
}

// pickOreFactoryOutlet is the factory whose value per unit, less the hauling
// at the board rate, beats the local bid by the most. Pure.
func pickOreFactoryOutlet(cands []oreOutletCand, localBid, unitVol float64) (int64, float64) {
	if unitVol <= 0 {
		unitVol = 0.01
	}
	bestID, best := int64(0), localBid*oreAskMargin
	for _, c := range cands {
		net := c.Value - haulISKPerM3Jump*unitVol*float64(c.Jumps)
		if net > best || (net == best && bestID != 0 && c.StationID < bestID) {
			bestID, best = c.StationID, net
		}
	}
	if bestID == 0 {
		return 0, 0
	}
	return bestID, best
}
