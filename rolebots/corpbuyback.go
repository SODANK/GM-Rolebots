package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"

	evedb "gurimoon/db"
)

// Buyback: how a building corporation pays members for materials.
// The production power bids at each Industry base for what the build orders
// posted there are short of (corpbuildbases.go). A corp bid's fill lands in
// the office's Materials division (db/marketorders.go deliverItem). Minerals
// bid at BuybackFrac of value, deep metals bid up (corpdeep.go). Ore is bid
// only at the pinned factory while the ore desk leans to building (share >=
// 0.5) and a deputy can refine it (corpRefineChore, RefineCorpStack). Members
// selling into these bids are paid by the market like anyone else. Every bid
// is tracked in rolebot_corp_orders (bid = 1) and its fills booked as buyback.

const (
	corpBidDays          = 14
	corpBidPerThink      = 8
	// corpBidMinISK is the smallest bid worth an order slot, the same bar a
	// listing meets (oreListMinValue). A smaller shortfall is rounded up to it, up
	// to corpBidStockAhead times the shortfall.
	corpBidMinISK     = oreListMinValue
	corpBidStockAhead = 3
	corpBidOreMaxTypes   = 4
	corpRefineMinUnits   = 1
	corpRefinePerChore   = 6
	corpOreDeskBidsAbove = 0.5
	// corpBuybackBonus is added to the charter's BuybackFrac when a bid is
	// priced, capped at full value. It keeps bids at or above the market's own.
	corpBuybackBonus = 0.08
	// corpLoyalNudge is how far a member values its own corporation's bid up
	// per point of Obedience (loyalSell).
	corpLoyalNudge = 0.20
	// corpFlipShare is the share of free capital a mining outfit with no
	// factory may have tied up in ore bids it will resell (ceoMiningBuyback).
	corpFlipShare = 0.5
)

// buybackBid is the price a buyback bid opens at, as a share of value.
// Pure, for the test.
func buybackBid(frac float64) float64 { return math.Min(1.0, frac+corpBuybackBonus) }

// corpMaterialShortfall is what the open build orders still need that the
// Materials division does not hold, less what open bids are already buying:
// every order on the board, for the desks that measure the factory's hunger.
func (m *Mod) corpMaterialShortfall(ctx context.Context, corpID, stationID, officeID int64) (map[int64]int64, error) {
	return m.corpShortfallWithin(ctx, corpID, stationID, officeID, math.Inf(1), 0)
}

// corpShortfallWithin is the shortfall of the orders a budget can feed, in the
// board's priority order (shortfallFor), each mineral priced at the bid
// buybackFrac opens at. An infinite budget is every order. It is counted
// order by order, not summed, so one mineral every order wants cannot take the budget.
func (m *Mod) corpShortfallWithin(ctx context.Context, corpID, stationID, officeID int64, budget, buybackFrac float64) (map[int64]int64, error) {
	bills, err := m.corpOrderBills(ctx, corpID, stationID)
	if err != nil || len(bills) == 0 {
		return nil, err
	}
	types := map[int64]bool{}
	for _, b := range bills {
		for t := range b {
			types[t] = true
		}
	}
	pool := map[int64]int64{}
	ids := make([]int64, 0, len(types))
	for typeID := range types {
		ids = append(ids, typeID)
		have, _ := evedb.GetMaterialQuantityAt(ctx, m.db, corpID, officeID, corpDivMaterials, typeID)
		// Only bids at this station fill this office's division.
		var bidding int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(o.volRemaining), 0) FROM rolebot_corp_orders r
			  JOIN market_orders o ON o.orderID = r.orderID
			 WHERE r.corporationID = ? AND r.bid = 1 AND r.typeID = ? AND r.stationID = ? AND o.orderState = 1`,
			corpID, typeID, stationID).Scan(&bidding)
		// And what a buy trip is already fetching for it (workboardSweep).
		pending := m.reqPending(ctx, corpID, typeID, stationID, corpDivMaterials)
		pool[typeID] = have + bidding + pending
	}
	unit := map[int64]float64{}
	if !math.IsInf(budget, 1) {
		vals, err := evedb.TypeValues(ctx, m.db, ids)
		if err != nil {
			return nil, err
		}
		for _, t := range ids {
			unit[t] = roundBidPrice(m.buybackUnitValue(ctx, stationID, t, vals[t].Value) * buybackBid(buybackFrac))
		}
	}
	return shortfallFor(bills, pool, unit, budget), nil
}

// corpOrderBills is what each open build order posted at one Industry base
// will use over its remaining runs, waste included, in the board's priority
// order. Priced as the builder checks it (industryMaterialNeeds): best ME held
// at that base, no Production Efficiency, a plain line.
func (m *Mod) corpOrderBills(ctx context.Context, corpID, stationID int64) ([]map[int64]int64, error) {
	open, err := m.openWorkFor(ctx, corpID)
	if err != nil {
		return nil, err
	}
	factory := m.corpFactoryStation(ctx, corpID)
	bestME := map[int64]int64{}
	prints, _ := m.libraryPrints(ctx, corpID, stationID)
	for _, p := range prints {
		if me, ok := bestME[p.BPTypeID]; !ok || p.ME > me {
			bestME[p.BPTypeID] = p.ME
		}
	}
	copies, _ := m.libraryCopies(ctx, corpID, stationID)
	for _, cp := range copies {
		if me, ok := bestME[cp.BPTypeID]; !ok || cp.ME > me {
			bestME[cp.BPTypeID] = cp.ME
		}
	}
	var bills []map[int64]int64
	for _, w := range open {
		if w.Kind != workKindBuild || w.RunsLeft() <= 0 || orderStation(w, factory) != stationID {
			continue
		}
		needs, err := m.industryMaterialNeeds(ctx, w.BPTypeID, bestME[w.BPTypeID], w.RunsLeft(), 0, 1.0)
		if err != nil {
			continue
		}
		bill := map[int64]int64{}
		for _, n := range needs {
			if n.Needed > 0 {
				bill[n.TypeID] += n.Needed
			}
		}
		if len(bill) > 0 {
			bills = append(bills, bill)
		}
	}
	return bills, nil
}

// shortfallFor feeds the bills in board order out of pool (division stock plus
// open bids) and returns what is still to buy. The first short order is always
// taken; later ones are taken while the running cost at unit prices stays in
// budget, and the walk stops at the first that does not fit. Pure.
func shortfallFor(bills []map[int64]int64, pool map[int64]int64, unit map[int64]float64, budget float64) map[int64]int64 {
	left := make(map[int64]int64, len(pool))
	for t, n := range pool {
		left[t] = n
	}
	out := map[int64]int64{}
	spent := 0.0
	taken := 0
	for _, bill := range bills {
		short := map[int64]int64{}
		cost := 0.0
		for t, need := range bill {
			if s := need - left[t]; s > 0 {
				short[t] = s
				cost += float64(s) * unit[t]
			}
			left[t] = max(0, left[t]-need)
		}
		if len(short) == 0 {
			continue
		}
		if taken > 0 && spent+cost > budget {
			break
		}
		for t, s := range short {
			out[t] += s
		}
		spent += cost
		taken++
	}
	return out
}

// ceoBuyback books what the corporation's bids bought, and bids for the
// shortfall at every Industry base. With one base it has the whole budget;
// with several the budget is split by what each base is short, valued at the
// bid price, and a base short of nothing bids for nothing.
func (m *Mod) ceoBuyback(ctx context.Context, c *corpCEO, ch Charter, budget float64) error {
	m.sweepCorpBids(ctx, c)
	m.tendDeepBids(ctx, c, ch)
	m.workboardSweep(ctx, c)
	factory, _ := m.corpFactoryOffice(ctx, c.CorpID)
	if factory == 0 {
		return nil
	}
	bases := m.industryBaseStations(ctx, c.CorpID)
	// A resting bid over today's price comes off before the shortfall counts
	// it as buying, whatever this think's budget.
	flips := m.corpFlipsOre(ctx, c.CorpID)
	for _, st := range bases {
		m.retireBuybackBids(ctx, c, ch, st, st == factory && !flips)
	}
	if budget < corpBidMinISK {
		return nil
	}
	type site struct {
		station, office int64
		want            float64
	}
	var sites []site
	total := 0.0
	for _, st := range bases {
		office, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, c.CorpID, st)
		if office == 0 || office == st {
			continue
		}
		s := site{station: st, office: office}
		if short, err := m.corpMaterialShortfall(ctx, c.CorpID, st, office); err == nil && len(short) > 0 {
			s.want = m.bidValueOf(ctx, st, short, ch.BuybackFrac)
		}
		sites = append(sites, s)
		total += s.want
	}
	for _, s := range sites {
		share := budget
		if len(sites) > 1 {
			if s.want <= 0 || total <= 0 {
				continue
			}
			share = budget * s.want / total
		}
		if err := m.buybackAt(ctx, c, ch, s.station, s.office, share, s.station == factory); err != nil {
			return err
		}
	}
	return nil
}

// bidValueOf is what a shortfall costs at the buyback's opening bid at one
// station.
func (m *Mod) bidValueOf(ctx context.Context, stationID int64, short map[int64]int64, buybackFrac float64) float64 {
	ids := make([]int64, 0, len(short))
	for t := range short {
		ids = append(ids, t)
	}
	vals, err := evedb.TypeValues(ctx, m.db, ids)
	if err != nil {
		return 0
	}
	v := 0.0
	for t, n := range short {
		v += m.buybackUnitValue(ctx, stationID, t, vals[t].Value) * buybackBid(buybackFrac) * float64(n)
	}
	return v
}

// buybackAt bids at one Industry base for what its own orders are short of;
// ores only where the deputy refines (the pinned factory).
func (m *Mod) buybackAt(ctx context.Context, c *corpCEO, ch Charter, stationID, officeID int64, budget float64, ores bool) error {
	if budget < corpBidMinISK {
		return nil
	}
	short, err := m.corpShortfallWithin(ctx, c.CorpID, stationID, officeID, budget, ch.BuybackFrac)
	if err != nil || len(short) == 0 {
		return err
	}
	bid := m.corpBidder(ctx, c, stationID, budget)

	// Minerals, biggest shortfall by value first.
	types := make([]int64, 0, len(short))
	for t := range short {
		types = append(types, t)
	}
	vals, err := evedb.TypeValues(ctx, m.db, types)
	if err != nil {
		return err
	}
	sort.Slice(types, func(i, j int) bool {
		vi, vj := vals[types[i]].Value*float64(short[types[i]]), vals[types[j]].Value*float64(short[types[j]])
		if vi != vj {
			return vi > vj
		}
		return types[i] < types[j]
	})
	for _, t := range types {
		// Ordered by book value above; priced off the tape (buybackUnitValue).
		unitValue := m.buybackUnitValue(ctx, stationID, t, vals[t].Value)
		price, what := roundBidPrice(unitValue*buybackBid(ch.BuybackFrac)), "short for the work board"
		if deepPriced(t) {
			// Deep metals are bid up so miners go deep for them (corpdeep.go).
			price, what = m.deepBidPrice(ctx, c, ch, stationID, t, unitValue), "deep metal for the work board"
		}
		if err := bid(t, short[t], price, what); err != nil {
			return err
		}
	}

	// Ore, when the desk is building and somebody can refine it.
	if !ores {
		return nil
	}
	p, _ := m.loadCorpPolicy(ctx, c.CorpID)
	if p.OreShare < corpOreDeskBidsAbove || p.DeputyID == 0 {
		return nil
	}
	yield := m.corpRefineYield(ctx, c.CorpID, p)
	for _, o := range m.oreBidPicks(ctx, stationID, short, yield) {
		if err := bid(o.typeID, o.units, roundBidPrice(o.unitValue*buybackBid(ch.BuybackFrac)), "ore for the refinery"); err != nil {
			return err
		}
	}
	return nil
}

// roundBidPrice keeps a bid to the market's two decimals.
func roundBidPrice(p float64) float64 { return math.Floor(p*100) / 100 }

// oreBid is one ore the corporation will bid for.
type oreBid struct {
	typeID    int64
	units     int64
	unitValue float64
}

// oreBidPicks chooses up to corpBidOreMaxTypes ores in the factory's region
// that carry the short minerals, most short-mineral per m³ first, sized to
// cover the shortfall at the refiner's yield.
func (m *Mod) oreBidPicks(ctx context.Context, stationID int64, short map[int64]int64, yield float64) []oreBid {
	regionID, _ := evedb.GetRegionForStation(ctx, m.db, stationID)
	ores, err := m.oreValues(ctx, []int64{regionID}, oreValueBid)
	if err != nil || yield <= 0 {
		return nil
	}
	type scored struct {
		v     oreValue
		score float64
	}
	var list []scored
	for _, v := range ores {
		if v.Volume <= 0 {
			continue
		}
		s := 0.0
		for mat := range short {
			s += v.PerUnit[mat]
		}
		if s > 0 {
			list = append(list, scored{v, s / v.Volume})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].v.TypeID < list[j].v.TypeID
	})
	var out []oreBid
	for _, s := range list {
		if len(out) >= corpBidOreMaxTypes {
			break
		}
		// Units to cover the largest short mineral this ore carries.
		units := int64(0)
		for mat, n := range short {
			if per := s.v.PerUnit[mat] * yield; per > 0 {
				units = max(units, int64(math.Ceil(float64(n)/per)))
			}
		}
		if units > 0 {
			out = append(out, oreBid{typeID: s.v.TypeID, units: units,
				unitValue: m.oreBuybackValue(ctx, stationID, s.v)})
		}
	}
	return out
}

// sweepCorpBids books what the corporation's bids bought and forgets the
// ones that are gone.
func (m *Mod) sweepCorpBids(ctx context.Context, c *corpCEO) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.orderID, r.typeID, r.price, r.volSeen,
		       COALESCE(o.volRemaining, -1), COALESCE(o.orderState, 0)
		  FROM rolebot_corp_orders r
		  LEFT JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.bid = 1`, c.CorpID)
	if err != nil {
		return
	}
	type row struct {
		id, typeID       int64
		price            float64
		seen, rem, state int64
	}
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.typeID, &r.price, &r.seen, &r.rem, &r.state) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	for _, r := range list {
		if r.rem >= 0 {
			if bought := r.seen - r.rem; bought > 0 {
				m.corpBook(ctx, c.CorpID, lineBuyback, -r.price*float64(bought), r.typeID,
					fmt.Sprintf("%d× %s", bought, m.typeNameOr(ctx, r.typeID)))
				m.noteCorpBuyCost(ctx, c.CorpID, r.typeID, bought, r.price*float64(bought))
				if deepPriced(r.typeID) {
					// A fill restarts the deep bid's climb clock (tendDeepBids reads repricedAt),
					// so a filling bid is not climbed.
					_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_orders SET volSeen = ?, repricedAt = ? WHERE orderID = ?`,
						r.rem, clock.Unix(), r.id)
				} else {
					_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_orders SET volSeen = ? WHERE orderID = ?`, r.rem, r.id)
				}
			}
		}
		if r.rem <= 0 || r.state != 1 {
			m.forgetCorpOrder(ctx, r.id)
		}
	}
}

// buybackSummary is the board's list of what the corporation is buying.
func (m *Mod) buybackSummary(ctx context.Context, corpID int64) []string {
	var out []string
	for _, r := range m.buybackRows(ctx, corpID) {
		out = append(out, fmt.Sprintf("• %s — %d wanted at %s each, %s",
			m.typeNameOr(ctx, r.TypeID), r.Remaining, iskString(r.Price), m.stationLabel(ctx, r.StationID)))
	}
	return out
}

// buybackRow is one live buy order of the corporation's buyback, for the
// board mail and the Work orders page.
type buybackRow struct {
	TypeID, Remaining, StationID int64
	Price                        float64
}

func (m *Mod) buybackRows(ctx context.Context, corpID int64) []buybackRow {
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.typeID, o.price, o.volRemaining, r.stationID
		  FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		 ORDER BY o.price * o.volRemaining DESC
		 LIMIT 8`, corpID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var list []buybackRow
	for rows.Next() {
		var r buybackRow
		if rows.Scan(&r.TypeID, &r.Price, &r.Remaining, &r.StationID) == nil {
			list = append(list, r)
		}
	}
	return list
}

// corpBidStations is where the corporation is bidding for any of these types:
// the stations a loyal miner prefers to sell at.
func (m *Mod) corpBidStations(ctx context.Context, corpID int64, typeIDs []int64) map[int64]bool {
	out := map[int64]bool{}
	if corpID == 0 || len(typeIDs) == 0 {
		return out
	}
	if _, ok := managedCEO(corpID); !ok {
		return out
	}
	for _, t := range typeIDs {
		ids, err := int64Column(ctx, m, `
			SELECT r.stationID FROM rolebot_corp_orders r
			  JOIN market_orders o ON o.orderID = r.orderID
			 WHERE r.corporationID = ? AND r.bid = 1 AND r.typeID = ? AND o.orderState = 1 AND o.volRemaining > 0`,
			corpID, t)
		if err == nil {
			for _, id := range ids {
				out[id] = true
			}
		}
	}
	return out
}

// loyalSell is bestSell with the member's own corporation's bid valued up by
// its obedience: a loyal member sells to its corporation at a small discount
// against the open market, a mercenary only when the corporation pays best.
func loyalSell(stops []marketStop, tn Tuning, corpStations map[int64]bool) *marketStop {
	if len(corpStations) == 0 {
		return bestSell(stops, tn)
	}
	nudge := 1 + corpLoyalNudge*math.Max(0, tn.Obedience)
	boosted := make([]marketStop, len(stops))
	copy(boosted, stops)
	for i := range boosted {
		if corpStations[boosted[i].StationID] {
			boosted[i].Price *= nudge
		}
	}
	pick := bestSell(boosted, tn)
	if pick == nil {
		return nil
	}
	for i := range stops {
		if stops[i].StationID == pick.StationID && stops[i].TypeID == pick.TypeID {
			return &stops[i]
		}
	}
	return pick
}

// corpWantsMemberOre reports whether a member's corporation is bidding for
// ore it would rather its members sold it than the market.
func (m *Mod) corpWantsMemberOre(ctx context.Context, corpID int64) bool {
	if _, ok := managedCEO(corpID); !ok {
		return false
	}
	if m.corpOreShare(ctx, corpID) < corpOreDeskBidsAbove && !m.corpFlipsOre(ctx, corpID) {
		return false
	}
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		  JOIN invTypes t ON t.typeID = r.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE r.corporationID = ? AND r.bid = 1 AND g.categoryID = 25 AND o.orderState = 1`, corpID).Scan(&n)
	return n > 0
}

// ── the deputy's refinery chore ──────────────────────────────────────────────

// corpRefineChore refines the corporation's held ore into its minerals, when
// the deputy is docked at the factory. Kept ore only: stock the ore desk is
// selling stays whole for the market.
func (m *Mod) corpRefineChore(ctx context.Context, b *botRun) {
	if b == nil || b.CorpID == 0 {
		return
	}
	c, _ := getCEO(ctx, m.db, b.CorpID)
	if c == nil {
		return
	}
	p, err := m.loadCorpPolicy(ctx, b.CorpID)
	if err != nil || p.DeputyID != b.CharID || p.OreShare <= 0 {
		return
	}
	stationID, officeID := m.corpFactoryOffice(ctx, b.CorpID)
	if stationID == 0 || b.StationID != stationID {
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.quantity, t.groupID
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ? AND e.singleton = 0 AND g.categoryID = 25
		 ORDER BY e.quantity DESC
		 LIMIT ?`, b.CorpID, officeID, corpDivMaterials, corpRefinePerChore)
	if err != nil {
		return
	}
	type stack struct{ itemID, typeID, qty, groupID int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if rows.Scan(&s.itemID, &s.typeID, &s.qty, &s.groupID) == nil {
			stacks = append(stacks, s)
		}
	}
	rows.Close()
	holds := m.loadOreHolds(ctx, b.CorpID)
	for _, s := range stacks {
		// Only what the desk is keeping: the kept quantity of this stack.
		keep := holds[[2]int64{stationID, s.typeID}].keep
		if keep < corpRefineMinUnits {
			continue
		}
		yield := m.refineHandAt(ctx, b, stationID, s.groupID).Net()
		if yield <= 0 {
			continue
		}
		out, consumed, err := evedb.RefineCorpStack(ctx, m.db, b.CorpID, officeID, corpDivMaterials, s.itemID, yield)
		if err != nil {
			m.logBot(b.CharID, logError, "corp", "refining the corporation's %s: %v", m.typeNameOr(ctx, s.typeID), err)
			continue
		}
		if consumed <= 0 {
			continue
		}
		m.saveOreHold(ctx, b.CorpID, stationID, s.typeID, max(0, keep-consumed), clock.Unix())
		m.noteOreFlow(ctx, s.typeID, oreStageRefinedIn, consumed, 0)
		m.noteOreFlowLots(ctx, oreStageRefinedOut, out, 0)
		m.ceoLog(ctx, c, powerProduction, "refine", b.CharID, 0,
			"%s refined %d× of the corporation's %s at %.0f%% into its Materials",
			m.charNameOr(ctx, b.CharID), consumed, m.typeNameOr(ctx, s.typeID), yield*100)
	}
}

// bidQtyForMinimum rounds a bid's quantity up so the order is worth at least
// minISK, never past stockAhead times what was asked. Pure, for the test.
func bidQtyForMinimum(qty int64, price, minISK float64, stockAhead int64) int64 {
	if qty <= 0 || price <= 0 || price*float64(qty) >= minISK {
		return qty
	}
	need := int64(math.Ceil(minISK / price))
	return min(need, qty*stockAhead)
}

// corpBiddingOn is every (station, type) the corporation has an open buy
// order for.
func (m *Mod) corpBiddingOn(ctx context.Context, corpID int64) map[[2]int64]bool {
	out := map[[2]int64]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.stationID, r.typeID FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0`, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st, ty int64
		if rows.Scan(&st, &ty) == nil {
			out[[2]int64{st, ty}] = true
		}
	}
	return out
}

// corpBidder returns a function that places the corporation's ore and
// mineral bids at one station, holding them to budget and to
// corpBidPerThink orders a think.
func (m *Mod) corpBidder(ctx context.Context, c *corpCEO, stationID int64, budget float64) func(typeID, qty int64, price float64, what string) error {
	placed := 0
	spend := 0.0
	return func(typeID, qty int64, price float64, what string) error {
		if placed >= corpBidPerThink || qty <= 0 || price <= 0 {
			return nil
		}
		// A cheap mineral's whole shortfall can be worth less than an order slot;
		// stock ahead instead, up to corpBidStockAhead times the shortfall.
		qty = bidQtyForMinimum(qty, price, corpBidMinISK, corpBidStockAhead)
		if cost := price * float64(qty); spend+cost > budget {
			qty = int64((budget - spend) / price)
		}
		if qty <= 0 || price*float64(qty) < corpBidMinISK {
			return nil
		}
		var open int
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_corp_orders WHERE corporationID = ? AND stationID = ? AND typeID = ? AND bid = 1`,
			c.CorpID, stationID, typeID).Scan(&open)
		if open > 0 {
			return nil
		}
		placer := m.corpOrderPlacer(ctx, c.CorpID, c.CharID)
		if placer == 0 {
			addCorpOrderPressure(c.CorpID, 1)
			return nil
		}
		out, err := evedb.PlaceBuyOrder(ctx, m.db, placer, stationID, typeID, price, qty, -1, 1, corpBidDays, true)
		if err != nil {
			return fmt.Errorf("corp bid %s: %w", m.typeNameOr(ctx, typeID), err)
		}
		if out.ErrorMsg != "" {
			m.logBot(c.CharID, logInfo, "ceo", "the market refused the corporation's bid for %s: %s",
				m.typeNameOr(ctx, typeID), out.ErrorMsg)
			return nil
		}
		placed++
		spend += price * float64(qty)
		if filled := out.FilledQty; filled > 0 {
			m.corpBook(ctx, c.CorpID, lineBuyback, -price*float64(filled), typeID,
				fmt.Sprintf("%d× %s", filled, m.typeNameOr(ctx, typeID)))
			m.noteCorpBuyCost(ctx, c.CorpID, typeID, filled, price*float64(filled))
		}
		if out.RestingOrderID != 0 {
			if _, err := m.db.ExecContext(ctx, `
				INSERT INTO rolebot_corp_orders (orderID, corporationID, stationID, typeID, bid, price, volSeen, placedAt, placedBy)
				VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?)`,
				out.RestingOrderID, c.CorpID, stationID, typeID, price, qty-out.FilledQty, clock.Unix(), placer); err != nil {
				return err
			}
		}
		m.ceoLog(ctx, c, powerProduction, "bid", typeID, price*float64(qty),
			"bid for %d× %s at %s for %s each (%s)", qty, m.typeNameOr(ctx, typeID),
			m.stationLabel(ctx, stationID), iskString(price), what)
		return nil
	}
}

// corpFlipsOre reports whether a corporation is a mining outfit that buys its
// members' ore to resell: mining-tagged with no industry desk to build it into.
func (m *Mod) corpFlipsOre(ctx context.Context, corpID int64) bool {
	facts, err := getAnyCorpFacts(ctx, m.db, corpID)
	if err != nil {
		return false
	}
	return flipsOre(facts.Policy.Tags)
}

// flipsOre is the tag rule behind corpFlipsOre. Pure, for the test.
func flipsOre(tags []string) bool {
	mine := false
	for _, t := range tags {
		if t == "mining" {
			mine = true
		}
	}
	return mine && !hasIndustryDesk(tags)
}

// ceoMiningBuyback is a mining outfit's buyback: it bids at its headquarters
// for the ores its region digs so members are paid on the spot, and the
// business pass lists what arrives (ceoListCorpStock). Open bids are held to
// corpFlipShare of free capital.
func (m *Mod) ceoMiningBuyback(ctx context.Context, c *corpCEO, ch Charter) error {
	m.sweepCorpBids(ctx, c)
	if !m.corpFlipsOre(ctx, c.CorpID) {
		return nil
	}
	var stationID int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT o.stationID FROM crpoffices o JOIN corporation c ON c.corporationID = o.corporationID
		 WHERE o.corporationID = ? ORDER BY (o.stationID = c.stationID) DESC, o.stationID LIMIT 1`,
		c.CorpID).Scan(&stationID)
	if stationID == 0 {
		return nil
	}
	regionID, _ := evedb.GetRegionForStation(ctx, m.db, stationID)
	ores, err := m.oreValues(ctx, []int64{regionID}, oreValueBid)
	if err != nil {
		return err
	}
	// What this outfit may pay for each rock: never more than its own lister
	// could ask for it again (resaleBidPrice).
	bound := map[int64]float64{}
	for _, v := range ores {
		if v.Volume <= 0 || v.Intrinsic <= 0 {
			continue
		}
		base := 0.0
		if sf := m.stableFloorPrice(ctx, v.TypeID); sf > 0 {
			base = sf / askFloorFraction
		}
		if p := roundBidPrice(resaleBidPrice(v.Intrinsic*oreIntrinsicSpread*buybackBid(ch.BuybackFrac),
			oreListCeiling(oreRefinedValue(v, oreReferenceYield()), oreAskRefinerEdge(), base))); p > 0 {
			bound[v.TypeID] = p
		}
	}
	// A standing bid over that bound would lose money on resale, so it is
	// cancelled before the budget is counted.
	m.retireResaleBids(ctx, c, stationID, bound)

	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return err
	}
	var tied float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(o.price * o.volRemaining), 0) FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.bid = 1 AND o.orderState = 1`, c.CorpID).Scan(&tied)
	budget := corpFlipShare*free - tied
	if budget < corpBidMinISK {
		return nil
	}
	outside := m.outsideBidsAt(ctx, c.CorpID, stationID)
	type ranked struct {
		v     oreValue
		price float64
	}
	var list []ranked
	for typeID, price := range bound {
		// A bid under another bid at the same dock buys nothing and holds a slot and
		// escrow.
		if outside[typeID] >= price {
			continue
		}
		list = append(list, ranked{ores[typeID], price})
	}
	// Dearest per cubic metre first: the hold is the miner's limit.
	sort.Slice(list, func(i, j int) bool {
		di, dj := list[i].price/list[i].v.Volume, list[j].price/list[j].v.Volume
		if di != dj {
			return di > dj
		}
		return list[i].v.TypeID < list[j].v.TypeID
	})
	if len(list) > corpBidOreMaxTypes {
		list = list[:corpBidOreMaxTypes]
	}
	if len(list) == 0 {
		return nil
	}
	bid := m.corpBidder(ctx, c, stationID, budget)
	each := budget / float64(len(list))
	for _, o := range list {
		price := roundBidPrice(o.price)
		if price <= 0 {
			continue
		}
		if err := bid(o.v.TypeID, int64(each/price), price, "ore for resale"); err != nil {
			return err
		}
	}
	return nil
}

// corpResaleMargin is the least a resale outfit must clear between its bid
// and the lowest ask its own lister would place, after broker fee and sales
// tax.
const corpResaleMargin = 0.02

// resaleBidPrice is what a resale outfit may bid for a rock: its charter's
// price, capped so its own listing (asking at least oreAskMargin times the
// best local bid, up to oreAskFloorMulHi) still fits under the list ceiling
// with corpResaleMargin to spare. 0 when nothing can be listed. Pure.
func resaleBidPrice(want, listCeiling float64) float64 {
	if want <= 0 || listCeiling <= 0 {
		return 0
	}
	return math.Min(want, listCeiling/(oreAskMargin*oreAskFloorMulHi*(1+corpResaleMargin)))
}

// bestOutsideBid is the best live bid at a station for a type that is not one
// of this corporation's own orders. 0 when there is none.
func (m *Mod) bestOutsideBid(ctx context.Context, corpID, stationID, typeID int64) float64 {
	var p float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(o.price), 0) FROM market_orders o
		 WHERE o.stationID = ? AND o.typeID = ? AND o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		   AND NOT EXISTS (SELECT 1 FROM rolebot_corp_orders r WHERE r.orderID = o.orderID AND r.corporationID = ?)`,
		stationID, typeID, corpID).Scan(&p)
	return p
}

// noteCorpBuyCost adds a buyback fill to the corporation's running unit cost
// (rolebot_corp_cost, the table the factory's products already use), so the
// repricer knows what an ore listing cost and does not walk it under that.
func (m *Mod) noteCorpBuyCost(ctx context.Context, corpID, typeID, units int64, cost float64) {
	if units <= 0 || cost <= 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_cost (corporationID, typeID, units, cost) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE units = units + VALUES(units), cost = cost + VALUES(cost)`,
		corpID, typeID, units, cost); err != nil {
		rlogf("rolebots: corp buy cost %d/%d: %v", corpID, typeID, err)
	}
}

// outsideBidsAt is the best live bid per type at a station, the
// corporation's own orders left out. One query for the whole ore list.
func (m *Mod) outsideBidsAt(ctx context.Context, corpID, stationID int64) map[int64]float64 {
	out := map[int64]float64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, MAX(o.price) FROM market_orders o
		 WHERE o.stationID = ? AND o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		   AND NOT EXISTS (SELECT 1 FROM rolebot_corp_orders r WHERE r.orderID = o.orderID AND r.corporationID = ?)
		 GROUP BY o.typeID`, stationID, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var typeID int64
		var p float64
		if rows.Scan(&typeID, &p) == nil {
			out[typeID] = p
		}
	}
	return out
}

// retireResaleBids cancels the outfit's own ore bids at its station that pay
// more than bound allows for their type (or any type with no bound). The
// escrow returns to the corporation wallet; the next think re-bids at the
// bound.
func (m *Mod) retireResaleBids(ctx context.Context, c *corpCEO, stationID int64, bound map[int64]float64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.orderID, o.typeID, o.price, o.volRemaining, o.charID FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		  JOIN invTypes t ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE r.corporationID = ? AND r.bid = 1 AND r.stationID = ? AND g.categoryID = 25
		   AND o.orderState = 1 AND o.volRemaining > 0`, c.CorpID, stationID)
	if err != nil {
		return
	}
	type bidRow struct {
		orderID, typeID, rem, placer int64
		price                        float64
	}
	var over []bidRow
	for rows.Next() {
		var b bidRow
		if rows.Scan(&b.orderID, &b.typeID, &b.price, &b.rem, &b.placer) != nil {
			continue
		}
		if b.price > bound[b.typeID]*1.001 {
			over = append(over, b)
		}
	}
	rows.Close()
	for _, b := range over {
		if _, err := evedb.CancelMarketOrder(ctx, m.db, b.placer, b.orderID); err != nil {
			rlogf("rolebots: retire resale bid %d corp %d: %v", b.orderID, c.CorpID, err)
			continue
		}
		m.forgetCorpOrder(ctx, b.orderID)
		why := "it can no longer be listed at a profit"
		if lim := bound[b.typeID]; lim > 0 {
			why = fmt.Sprintf("its own listing could not ask more than the %s a bid may pay for it", iskString(lim))
		}
		m.ceoLog(ctx, c, powerProduction, "unbid", b.typeID, b.price*float64(b.rem),
			"withdrew the corporation's bid for %d× %s at %s each — %s",
			b.rem, m.typeNameOr(ctx, b.typeID), iskString(b.price), why)
	}
}

// corpBuybackRetireAbove is how far over today's opening price a building
// buyback's resting bid may stand before it is withdrawn. The margin keeps an
// ordinary day's drift from cancelling every bid.
const corpBuybackRetireAbove = 1.10

// buybackOverpaid reports whether a resting bid pays more than
// corpBuybackRetireAbove times what a bid would open at today. A type with
// no price today (bound 0) is left alone: nothing says it is overpaid. Pure.
func buybackOverpaid(price, bound float64) bool {
	return bound > 0 && price > bound*corpBuybackRetireAbove
}

// oreBuybackValue is what the building buyback values one unit of ore at:
// the refiner's share of what it refines into, never above what the ore
// itself trades for on the tape (buybackUnitValue).
func (m *Mod) oreBuybackValue(ctx context.Context, stationID int64, v oreValue) float64 {
	return m.buybackUnitValue(ctx, stationID, v.TypeID, v.Intrinsic*oreIntrinsicSpread)
}

// retireBuybackBids cancels the building buyback's resting bids at one
// Industry base that pay over corpBuybackRetireAbove times what buybackAt
// would open them at today. Ore bids are judged only where the buyback bids
// for ore (ores). The escrow returns to the wallet and the next pass re-bids.
func (m *Mod) retireBuybackBids(ctx context.Context, c *corpCEO, ch Charter, stationID int64, ores bool) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.orderID, o.typeID, o.price, o.volRemaining, o.charID, g.categoryID FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		  JOIN invTypes t ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE r.corporationID = ? AND r.bid = 1 AND r.stationID = ?
		   AND o.orderState = 1 AND o.volRemaining > 0`, c.CorpID, stationID)
	if err != nil {
		return
	}
	type bidRow struct {
		orderID, typeID, rem, placer, cat int64
		price, bound                      float64
	}
	var list []bidRow
	for rows.Next() {
		var b bidRow
		if rows.Scan(&b.orderID, &b.typeID, &b.price, &b.rem, &b.placer, &b.cat) == nil {
			list = append(list, b)
		}
	}
	rows.Close()
	if len(list) == 0 {
		return
	}
	var oreVals map[int64]oreValue
	if ores {
		regionID, _ := evedb.GetRegionForStation(ctx, m.db, stationID)
		oreVals, _ = m.oreValues(ctx, []int64{regionID}, oreValueBid)
	}
	bound := map[int64]float64{}
	var over []bidRow
	for _, b := range list {
		lim, ok := bound[b.typeID]
		if !ok {
			switch {
			case b.cat == 25:
				if v, found := oreVals[b.typeID]; found && v.Intrinsic > 0 {
					lim = roundBidPrice(m.oreBuybackValue(ctx, stationID, v) * buybackBid(ch.BuybackFrac))
				}
			default:
				uv := m.buybackUnitValue(ctx, stationID, b.typeID, evedb.TypeValueOf(ctx, m.db, b.typeID).Value)
				if uv > 0 {
					lim = roundBidPrice(uv * buybackBid(ch.BuybackFrac))
					if deepPriced(b.typeID) {
						// The same limit tendDeepBids climbs to, off the same tape.
						lim = m.deepBidLimit(ctx, c, ch, stationID, b.typeID, uv)
					}
				}
			}
			bound[b.typeID] = lim
		}
		if b.placer != 0 && buybackOverpaid(b.price, lim) {
			b.bound = lim
			over = append(over, b)
		}
	}
	for _, b := range over {
		if _, err := evedb.CancelMarketOrder(ctx, m.db, b.placer, b.orderID); err != nil {
			rlogf("rolebots: retire buyback bid %d corp %d: %v", b.orderID, c.CorpID, err)
			continue
		}
		m.forgetCorpOrder(ctx, b.orderID)
		m.ceoLog(ctx, c, powerProduction, "unbid", b.typeID, b.price*float64(b.rem),
			"withdrew the corporation's bid for %d× %s at %s each — it trades for less now; today's bid would be %s",
			b.rem, m.typeNameOr(ctx, b.typeID), iskString(b.price), iskString(b.bound))
	}
}
