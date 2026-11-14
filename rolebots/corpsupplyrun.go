package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// factoryPostTryAt is the last factory-post hunt per corporation that came to
// nothing. In memory: a restart may look again.
var factoryPostTryAt sync.Map // corpID to unix seconds

func init() { registerCorpForget("factoryPostTryAt", func(id int64) { factoryPostTryAt.Delete(id) }) }

// Supply runs: the Trade desk stocking a factory.
//
// A supply run buys bulk minerals or materials cheaply near an office, carries them to
// an office at a busy factory station (factorydemand.go) and rests them in the Trade
// division on the corporation's account (ceoListTradeStock), priced just under what the
// industrials there would pay. It uses the desk's own requisition line, hauler draft,
// office delivery and listing pass, and ends on the shelf at a factory, never in a bid.
// The gate is the desk's own number: the margin discounted by confidence
// (tradeDeal.expected), less the wage, must beat the best sure run. Trade corporations
// also keep one office at a top factory station (ceoFactoryPost).

const (
	refFactorySupply = "factory" // rolebot_corp_requisition.refKind of a supply run

	// factoryPostWant is how many factory offices a trade corporation keeps.
	factoryPostWant = 1
	// factoryPostSystems is how many factory stations a hunt tries.
	factoryPostSystems = 8
)

// supplyRunBeats is the gate: a supply run is posted only when its expected value
// (confidence-discounted margin less the wage) clears the desk's minimum and beats the
// best sure run the scan found (nil = none). Pure.
func supplyRunBeats(sup, sure *tradeDeal) bool {
	if sup == nil || sup.expected() < tradeDeskMinProfitISK {
		return false
	}
	return sure == nil || sup.expected() > sure.expected()
}

// tradeDeskSupplyRun is the best supply run the desk could post: a factory
// station holding one of the corporation's offices that wants a type, the
// cheapest seller of it inside the desk's buy reach, sized to the want, the
// budget and the biggest hold. nil when nothing clears.
func (m *Mod) tradeDeskSupplyRun(ctx context.Context, c *corpCEO, scan *botRun, cfg tradeConfig,
	r travelRules, offices map[int64]bool, u *universe, budget, hold float64) *tradeDeal {

	rows, err := m.factoryDemandStations(ctx)
	if err != nil || len(rows) == 0 || u == nil {
		return nil
	}
	var wants []factoryDemand
	typeSet := map[int64]bool{}
	for _, d := range rows {
		if !offices[d.StationID] || d.Units <= 0 {
			continue
		}
		wants = append(wants, d)
		typeSet[d.TypeID] = true
	}
	if len(wants) == 0 {
		return nil
	}
	typeIDs := make([]int64, 0, len(typeSet))
	for t := range typeSet {
		typeIDs = append(typeIDs, t)
	}
	sort.Slice(typeIDs, func(i, j int) bool { return typeIDs[i] < typeIDs[j] })
	sources, err := m.specSources(ctx, scan, cfg, r, typeIDs)
	if err != nil || len(sources) == 0 {
		return nil
	}
	askMargin, floorMargin := scan.Tune.TradeMinMarginFrac, scan.Tune.TradeFloorMarginFrac
	if floorMargin > askMargin {
		floorMargin = askMargin
	}
	legs := map[int64]map[int64]int{}
	flows := m.supplyRunFlows(ctx, wants)
	var best *tradeDeal
	for _, w := range wants {
		// Stock already on the shelf there, or a run already in flight to
		// it, is the want being served.
		if m.reqPending(ctx, c.CorpID, w.TypeID, w.StationID, corpDivTrade) > 0 ||
			m.corpListingAt(ctx, c.CorpID, w.StationID, w.TypeID) {
			continue
		}
		// Other corporations' flows count too, or four desks reading one want would post four
		// runs: what is left is the want less every bound run and every resting ask.
		key := [2]int64{w.StationID, w.TypeID}
		if w.Units = supplyRunNet(w.Units, flows.dest[key]+flows.shelf[key]); w.Units <= 0 {
			continue
		}
		for _, src := range sources[w.TypeID] {
			// A seller's stock another run has already promised itself is
			// not there to buy.
			if src.Stock = supplyRunNet(src.Stock, flows.src[[2]int64{src.StationID, w.TypeID}]); src.Stock <= 0 {
				continue
			}
			if src.StationID == w.StationID || src.SystemID == w.SystemID {
				continue
			}
			if len(cfg.BuySystems) > 0 && !cfg.BuySystems[src.SystemID] {
				continue // no member would fly out that far to buy it
			}
			if w.AltPrice > 0 && src.Price >= w.AltPrice {
				continue // the industrial could buy it there itself for less
			}
			if tradeLaneCooling(c.CorpID, w.TypeID, src.StationID, w.StationID, clock.Unix()) {
				continue
			}
			from, ok := legs[src.SystemID]
			if !ok {
				from = bfsJumps(u, src.SystemID, 40)
				legs[src.SystemID] = from
			}
			jumps, ok := from[w.SystemID]
			if !ok {
				continue
			}
			d := m.priceSupplyRun(ctx, c, w, src, jumps, budget, hold, askMargin, floorMargin)
			if d == nil {
				continue
			}
			if best == nil || d.Rank > best.Rank {
				best = d
			}
		}
	}
	return best
}

// supplyFlows is what the market already does about the factory wants: units every
// corporation's open supply runs carry to each (factory, type) and promise out of each
// (seller, type), and the asks resting at each (factory, type) from anyone.
type supplyFlows struct {
	dest, src, shelf map[[2]int64]int64
}

// supplyRunNet is a quantity less what is already spoken for, never below
// zero. Pure.
func supplyRunNet(qty, taken int64) int64 {
	return max(qty-taken, 0)
}

// supplyRunFlows reads supplyFlows for a set of wants. Best effort: a failed
// read leaves a map empty, so only the corporation's own flows count.
func (m *Mod) supplyRunFlows(ctx context.Context, wants []factoryDemand) supplyFlows {
	f := supplyFlows{dest: map[[2]int64]int64{}, src: map[[2]int64]int64{}, shelf: map[[2]int64]int64{}}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT destStationID, srcStationID, typeID, SUM(GREATEST(qty - qtyDone, 0))
		  FROM rolebot_corp_requisition
		 WHERE refKind = ? AND status IN ('open', 'bidding', 'buying')
		 GROUP BY destStationID, srcStationID, typeID`, refFactorySupply); err == nil {
		for rows.Next() {
			var dst, src, t, n int64
			if rows.Scan(&dst, &src, &t, &n) == nil {
				f.dest[[2]int64{dst, t}] += n
				f.src[[2]int64{src, t}] += n
			}
		}
		rows.Close()
	}
	stations := map[int64]bool{}
	want := map[[2]int64]bool{}
	for _, w := range wants {
		stations[w.StationID] = true
		want[[2]int64{w.StationID, w.TypeID}] = true
	}
	ids := make([]int64, 0, len(stations))
	for id := range stations {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return f
	}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, typeID, SUM(volRemaining) FROM market_orders
		 WHERE bid = 0 AND orderState = 1 AND volRemaining > 0
		   AND stationID IN (`+placeholders(len(ids))+`)
		 GROUP BY stationID, typeID`, int64Args(ids)...); err == nil {
		for rows.Next() {
			var st, t, n int64
			if rows.Scan(&st, &t, &n) == nil && want[[2]int64{st, t}] {
				f.shelf[[2]int64{st, t}] += n
			}
		}
		rows.Close()
	}
	return f
}

// priceSupplyRun costs one (want, seller) pair as a desk run. nil when it
// does not clear the desk's floor.
func (m *Mod) priceSupplyRun(ctx context.Context, c *corpCEO, w factoryDemand, src specSource, jumps int,
	budget, hold, askMargin, floorMargin float64) *tradeDeal {

	vol := src.UnitVol
	if vol <= 0 {
		vol = 0.01
	}
	units := supplyRunUnits(src.Stock, w.Units, budget, src.Price, hold, vol)
	if units <= 0 || !tradeCargoOK(src.CategoryID, units) {
		return nil
	}
	if _, isMineral := mineralListFloors[w.TypeID]; isMineral &&
		units < mineralUnitFloor(w.TypeID, src.Price, 1) {
		return nil // a handful of minerals is not a lot a factory can plan on
	}
	// The spiral brake every speculation buys under.
	if base := m.typeBasePrice(ctx, w.TypeID); base > 0 {
		if src.Price > base*specSpiralBaseMul {
			return nil
		}
	} else if src.Price > specUnanchoredMaxUnit {
		return nil
	}
	wage := haulWageFor(float64(units)*vol, jumps)
	landed := src.Price + wage/float64(units)
	list := factoryListPrice(0, 0, landed, w.AltPrice, askMargin, floorMargin)
	if w.AltPrice > 0 && list > w.AltPrice*factoryAskUndercut {
		return nil // the floor alone is over what the factory pays otherwise
	}
	rates := m.marketRates(ctx, c.CharID, w.StationID)
	profit := specNetProfit(list, src.Price, units, rates)
	conf := specConfBase + specConfLedger + specConfFactory
	if conf > specConfMax {
		conf = specConfMax
	}
	conf = m.specCalibrate(ctx, src.CategoryID, conf)
	d := &tradeDeal{
		Kind: tradeDealSpec, TypeID: w.TypeID, TypeName: m.typeNameOr(ctx, w.TypeID), UnitVol: vol,
		CategoryID:   src.CategoryID,
		BuyStationID: src.StationID, BuySystemID: src.SystemID,
		SellStationID: w.StationID, SellSystemID: w.SystemID,
		Ask: src.Price, Bid: list, Units: units, Outlay: float64(units) * src.Price,
		Profit: profit, ListPrice: list, Confidence: conf, HaulWage: wage, Jumps: jumps,
		Evidence: factoryEvidence(w.Units, w.AltPrice),
	}
	if d.expected() < tradeDeskMinProfitISK {
		return nil
	}
	d.Rank = tradeRank(d)
	return d
}

// supplyRunUnits sizes a run: the seller's stock, the factory's want, what
// the budget buys and what the hold carries, whichever is least. Pure.
func supplyRunUnits(stock, want int64, budget, price, hold, unitVol float64) int64 {
	if price <= 0 || unitVol <= 0 {
		return 0
	}
	units := min(stock, want)
	if byISK := int64(budget / price); byISK < units {
		units = byISK
	}
	if byVol := int64(hold / unitVol); byVol < units {
		units = byVol
	}
	return max(units, 0)
}

// corpListingAt reports whether the corporation already rests an ask for a
// type at a station.
func (m *Mod) corpListingAt(ctx context.Context, corpID, stationID, typeID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_orders
		 WHERE corporationID = ? AND stationID = ? AND typeID = ? AND bid = 0`,
		corpID, stationID, typeID).Scan(&n)
	return n > 0
}

// postSupplyRun writes a supply run as a desk requisition line (the shape postTradeReq
// writes, ending at the factory office's Trade division) and hands it to a hauler.
func (m *Mod) postSupplyRun(ctx context.Context, c *corpCEO, d *tradeDeal) error {
	outlay := d.Ask * float64(d.Units)
	budget := math.Ceil(outlay*(1+tradeDeskBudgetSlack)) + d.HaulWage
	maxPrice := math.Round(d.Ask*(1+tradeDeskBudgetSlack)*100) / 100
	note := truncate(fmt.Sprintf("supply run: buy at %s, list at the factory at %s (%d jumps)",
		m.stationLabel(ctx, d.BuyStationID), m.stationLabel(ctx, d.SellStationID), d.Jumps), 255)
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_requisition
			(corporationID, typeID, qty, destStationID, destFlag, requester, refKind, refID, deadline, budget,
			 openedAt, note, srcStationID, maxPrice, wage)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CorpID, d.TypeID, d.Units, d.SellStationID, corpDivTrade, reqTrade, refFactorySupply, d.BuyStationID,
		clock.Unix()+tradeDeskDeadlineSec, budget, clock.Unix(), note, d.BuyStationID, maxPrice, d.HaulWage)
	if err != nil {
		return fmt.Errorf("postSupplyRun: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := m.openCommit(ctx, c.CorpID, "req", id, budget); err != nil {
		return err
	}
	m.ceoLog(ctx, c, powerBusiness, "supplyrun", d.TypeID, budget,
		"posted a supply run: %d× %s, buy %s at %s, list near %s at the factory at %s — %s expected after a %s wage (%s)",
		d.Units, d.TypeName, iskString(d.Ask), m.stationLabel(ctx, d.BuyStationID),
		iskString(d.ListPrice), m.stationLabel(ctx, d.SellStationID), iskString(d.expected()),
		iskString(d.HaulWage), d.Evidence)
	markPosWork(c.CorpID)
	if reqs, err := m.queryReqs(ctx, `WHERE reqID = ?`, id); err == nil && len(reqs) == 1 {
		m.tradeDeskAssign(ctx, c, &reqs[0])
	}
	return nil
}

// ── the factory office ─────────────────────────────────────────────────────

// factoryPostSite is one factory station a trade corporation could open a
// shelf at.
type factoryPostSite struct {
	StationID int64
	SystemID  int64
	RegionID  int64
	Value     float64
	Jobs      int
	FromHQ    int
	Security  float64
}

// rankFactoryPostSites orders the factory stations a post could open at:
// busy ones only (factoryPostMinJobs), inside reach, at or above the security
// floor, not already an office; the best valued-and-busy first, then the
// nearest. Pure.
func rankFactoryPostSites(sites []factoryPostSite, offices map[int64]bool, secFloor float64) []factoryPostSite {
	var out []factoryPostSite
	for _, s := range sites {
		if offices[s.StationID] || s.Jobs < factoryPostMinJobs || s.Value <= 0 || s.Security < secFloor ||
			s.FromHQ < 0 || s.FromHQ > tradePostMaxJumps {
			continue
		}
		out = append(out, s)
	}
	score := func(s factoryPostSite) float64 { return factoryDestScore(s.Value, s.Jobs, s.FromHQ) }
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := score(out[i]), score(out[j]); a != b {
			return a > b
		}
		if out[i].FromHQ != out[j].FromHQ {
			return out[i].FromHQ < out[j].FromHQ
		}
		return out[i].StationID < out[j].StationID
	})
	return out
}

// factoryPostsHeld counts the corporation's offices at busy factory
// stations. Pure.
func factoryPostsHeld(offices map[int64]bool, dests []factoryDest) int {
	n := 0
	for _, d := range dests {
		if offices[d.StationID] && d.Jobs >= factoryPostMinJobs {
			n++
		}
	}
	return n
}

// ceoFactoryPost rents a trade corporation an office at a top factory
// station within reach of its headquarters, so the desk's supply runs have
// a shelf to end on. Reports whether it rented one.
func (m *Mod) ceoFactoryPost(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, hq int64) (bool, error) {
	now := clock.Unix()
	if now-m.lastCEOActionAt(ctx, c.CorpID, "factorypost") < tradePostEverySec {
		return false, nil
	}
	if v, ok := factoryPostTryAt.Load(c.CorpID); ok && now-v.(int64) < tradePostRetrySec {
		return false, nil
	}
	anchors, err := m.corpAnchorStations(ctx, c.CorpID)
	if err != nil {
		return false, err
	}
	offices := map[int64]bool{}
	for stationID := range anchors {
		if m.officeAt(ctx, c.CorpID, stationID) != 0 {
			offices[stationID] = true
		}
	}
	rows, err := m.factoryDemandStations(ctx)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	dests := groupFactoryDests(rows)
	if factoryPostsHeld(offices, dests) >= factoryPostWant {
		return false, nil
	}
	fromHQ, err := m.corpJumpMap(ctx, m.stationSystemID(ctx, hq))
	if err != nil {
		return false, err
	}
	u, err := m.universe(ctx)
	if err != nil {
		return false, err
	}
	var sites []factoryPostSite
	for _, d := range dests {
		j, ok := fromHQ[d.SystemID]
		if !ok {
			continue
		}
		sites = append(sites, factoryPostSite{StationID: d.StationID, SystemID: d.SystemID, RegionID: d.RegionID,
			Value: d.Value, Jobs: d.Jobs, FromHQ: j, Security: u.meta[d.SystemID].Security})
	}
	ranked := rankFactoryPostSites(sites, offices, ch.HQSecFloor)
	if len(ranked) > factoryPostSystems {
		ranked = ranked[:factoryPostSystems]
	}
	for _, s := range ranked {
		at := estateCandidate{StationID: s.StationID, Name: m.stationLabel(ctx, s.StationID),
			RegionID: s.RegionID, Security: s.Security}
		why := fmt.Sprintf("a factory shelf — %d bot jobs there in a week and %s of materials short, %d jump(s) from the headquarters",
			s.Jobs, iskShort(s.Value), s.FromHQ)
		rented, err := m.ceoTryRentOffice(ctx, c, facts, ch, at, false, false, why)
		if errors.Is(err, evedb.ErrNoFreeOffices) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !rented {
			factoryPostTryAt.Store(c.CorpID, now)
			return false, nil
		}
		factoryPostTryAt.Delete(c.CorpID)
		m.ceoLog(ctx, c, powerEstate, "factorypost", s.StationID, 0,
			"opened a factory shelf at %s so the trade desk can stock what its factories are short of", at.Name)
		return true, nil
	}
	factoryPostTryAt.Store(c.CorpID, now)
	return false, nil
}
