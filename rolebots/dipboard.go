package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"
)

// The dip run buys an ask well under its usual price and lists it near that price. It runs
// through the spec machinery (kind tradeDealSpec, claims, the listing leg); this file is the
// search and the fill guard.
// An ask is a target only if a pilot's own stock is behind it. NPC shelves are never targets
// (some sit under their own metal, so buying them mints value). A corporation's store and a
// factory's shelf are not either: buying factory output reads as a sale and it builds more.
// One station per run; the claims spread a region across pilots.

const (
	// dipBoardTTL is how long one read of the crashed asks stands.
	dipBoardTTL = 5 * time.Minute
	// dipConfBase is the belief in a dip's listing selling before the market's
	// record for its category. The price is the region's usual one, so it
	// starts above a plain speculation (specConfBase).
	dipConfBase = 0.7
	// dipCostTop is how many stacks a pilot prices in full, best first.
	dipCostTop = 200
)

// dipCategories are what a dip buys: modules, ammunition and trade goods.
// Minerals move in lots under their own rules, and a hull needs a hold.
var dipCategories = []int64{catModule, catCharge, 17}

// dipAsk is one sell order on the board.
type dipAsk struct {
	TypeID, RegionID, StationID, SystemID, CharID int64

	TypeName   string
	UnitVol    float64
	CategoryID int64
	Price      float64
	Units      int64
	Protected  bool // a corp's or a factory's: never bought
}

// dipPair is one crashed market: the usual price, and the units a flip can
// count on selling.
type dipPair struct {
	Usual float64
	Room  int64
}

// dipBoard is every crashed ask, roster-wide. Read-only once shared.
type dipBoard struct {
	Asks  []dipAsk             // by type, then station, then price
	Pairs map[[2]int64]dipPair // (typeID, regionID)
}

// buildDipBoard picks the crashed asks out of the book. A market's room is a week of recent
// end-user buying less everything that would sell ahead of a flip (a crashed ask is not
// counted, since buying it removes it). No recent buying means no room. Pure.
func buildDipBoard(asks []dipAsk, h *mktHistory, entry float64) *dipBoard {
	ahead := map[[2]int64]int64{}
	var crashed []dipAsk
	for _, a := range asks {
		usual := h.norm(a.TypeID, a.RegionID)
		if usual <= 0 {
			continue
		}
		switch {
		case !a.Protected && a.Price < usual*entry:
			crashed = append(crashed, a)
		case a.Price < usual*marketListHi:
			ahead[[2]int64{a.TypeID, a.RegionID}] += a.Units
		}
	}
	out := &dipBoard{Pairs: map[[2]int64]dipPair{}}
	for _, a := range crashed {
		k := [2]int64{a.TypeID, a.RegionID}
		p, ok := out.Pairs[k]
		if !ok {
			room := int64(h.recentPerDay(a.TypeID, a.RegionID)*mktAbsorbDays) - ahead[k]
			if room <= 0 {
				continue
			}
			p = dipPair{Usual: h.norm(a.TypeID, a.RegionID), Room: room}
			out.Pairs[k] = p
		}
		out.Asks = append(out.Asks, a)
	}
	sort.Slice(out.Asks, func(i, j int) bool {
		x, y := out.Asks[i], out.Asks[j]
		if x.TypeID != y.TypeID {
			return x.TypeID < y.TypeID
		}
		if x.StationID != y.StationID {
			return x.StationID < y.StationID
		}
		return x.Price < y.Price
	})
	return out
}

// dipBoardOf is the shared board, empty when the history cannot support one.
func (m *Mod) dipBoardOf(ctx context.Context) (*dipBoard, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshotFor(snapKey("dip-board"), dipBoardTTL, func() (*dipBoard, error) {
		h := m.marketHistory(sctx)
		if !h.Ready {
			return &dipBoard{}, nil
		}
		asks, err := m.loadDipAsks(sctx)
		if err != nil {
			return nil, err
		}
		return buildDipBoard(asks, h, m.marketPlay(sctx).DipEntry), nil
	})
}

// loadDipAsks reads the pilots' asks in the dip categories.
func (m *Mod) loadDipAsks(ctx context.Context) ([]dipAsk, error) {
	args := append([]any{tradeDealBuild, tradeDealSupply}, int64Args(dipCategories)...)
	rows, err := m.db.QueryContext(ctx, capStatement(`
		SELECT o.typeID, o.regionID, o.stationID, o.solarSystemID, o.charID, t.typeName,
		       COALESCE(t.volume, 0.01), g.categoryID, o.price, o.volRemaining,
		       o.isCorp = 1 OR COALESCE(b.kind, '') IN (?, ?)
		  FROM market_orders o
		  JOIN invTypes t ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN rolebot_orders b ON b.orderID = o.orderID
		 WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0 AND o.price > 0
		   AND o.charID NOT IN (`+liquidityExcluded+`)
		   AND g.categoryID IN (`+placeholders(len(dipCategories))+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("dip asks: %w", err)
	}
	defer rows.Close()
	var out []dipAsk
	for rows.Next() {
		var a dipAsk
		if err := rows.Scan(&a.TypeID, &a.RegionID, &a.StationID, &a.SystemID, &a.CharID, &a.TypeName,
			&a.UnitVol, &a.CategoryID, &a.Price, &a.Units, &a.Protected); err != nil {
			return nil, fmt.Errorf("dip asks: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// protectedAskAt is the cheapest ask at a station a dip must never fill: an
// NPC's, a corporation's or a factory's. 0 when there is none.
func (m *Mod) protectedAskAt(ctx context.Context, stationID, typeID int64) float64 {
	var p sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MIN(o.price) FROM market_orders o
		  LEFT JOIN rolebot_orders b ON b.orderID = o.orderID
		 WHERE o.stationID = ? AND o.typeID = ? AND o.bid = 0 AND o.orderState = 1
		   AND o.volRemaining > 0
		   AND (o.charID IN (`+liquidityExcluded+`) OR o.isCorp = 1 OR b.kind IN (?, ?))`,
		stationID, typeID, tradeDealBuild, tradeDealSupply).Scan(&p)
	return p.Float64
}

// dipCeiling is the highest price a dip's buy order may carry: its cap, or a
// tick under the cheapest protected ask, because the fill takes every ask
// under the ceiling. false when that leaves nothing to buy. Pure.
func dipCeiling(buyCap, protected, ask float64) (float64, bool) {
	ceiling := buyCap
	if protected > 0 && protected-0.01 < ceiling {
		ceiling = protected - 0.01
	}
	return ceiling, ask > 0 && ask <= ceiling
}

// dipEntryCap is the most a pilot pays a unit: entry times the usual price, or what still
// leaves its own floor margin after the sale's costs, so a greedy pilot needs a deeper
// crash. Pure.
func dipEntryCap(usual, list float64, r marketRates, tn Tuning, entry float64) float64 {
	net := list * (1 - r.Tax - r.Fee)
	return math.Min(usual*entry, net/(1+tn.TradeFloorMarginFrac))
}

// dipLot is how much of one station's stack a run takes, cheapest first: only
// units at or under buyCap, within the budget, the hold and maxUnits. Pure.
func dipLot(stack []dipAsk, buyCap, budget, hold float64, maxUnits int64) (int64, float64) {
	var units int64
	var outlay float64
	for _, a := range stack {
		if a.Price > buyCap {
			break
		}
		vol := a.UnitVol
		if vol <= 0 {
			vol = 0.01
		}
		n := min(a.Units, maxUnits-units, int64((budget-outlay)/a.Price), int64(hold/vol)-units)
		if n <= 0 {
			break
		}
		units += n
		outlay += float64(n) * a.Price
	}
	return units, outlay
}

// dipStacks groups the board into (type, station) stacks, leaving out the
// pilot's own asks and stations it will not fly to. Largest gain first, cut to
// dipCostTop.
func dipStacks(board *dipBoard, charID int64, jumps map[int64]int) [][]dipAsk {
	var out [][]dipAsk
	gain := map[int]float64{}
	for i := 0; i < len(board.Asks); {
		j := i
		var stack []dipAsk
		g := 0.0
		for ; j < len(board.Asks) && board.Asks[j].TypeID == board.Asks[i].TypeID &&
			board.Asks[j].StationID == board.Asks[i].StationID; j++ {
			a := board.Asks[j]
			if a.CharID == charID {
				continue
			}
			stack = append(stack, a)
			g += float64(a.Units) * (board.Pairs[[2]int64{a.TypeID, a.RegionID}].Usual - a.Price)
		}
		if _, ok := jumps[board.Asks[i].SystemID]; ok && len(stack) > 0 {
			gain[len(out)] = g
			out = append(out, stack)
		}
		i = j
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(x, y int) bool { return gain[idx[x]] > gain[idx[y]] })
	if len(idx) > dipCostTop {
		idx = idx[:dipCostTop]
	}
	ranked := make([][]dipAsk, len(idx))
	for i, k := range idx {
		ranked[i] = out[k]
	}
	return ranked
}

// findDipRun is the best crashed ask this pilot can flip. (nil, nil) when it
// does not deal in crashes or nothing pays.
func (m *Mod) findDipRun(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules,
	jumps map[int64]int, budget, hold float64) (*tradeDeal, error) {

	play := m.marketPlay(ctx)
	if !play.Dip || tradeInterest(b.Tune) < specMinInterest || m.freeOrderSlots(ctx, b.CharID) <= 0 {
		return nil, nil
	}
	board, err := m.dipBoardOf(ctx)
	if err != nil {
		return nil, err
	}
	if len(board.Asks) == 0 {
		return nil, nil
	}
	hist := m.marketHistory(ctx)
	load := m.tradeLaneLoad(ctx, b.CharID)
	now := clock.Unix()

	type dock struct{ station, system int64 }
	docks := map[[2]int64]dock{}
	var deals []*tradeDeal
	for _, stack := range dipStacks(board, b.CharID, jumps) {
		a := stack[0]
		k := [2]int64{a.TypeID, a.RegionID}
		pair := board.Pairs[k]
		d, seen := docks[k]
		if !seen {
			d.station, d.system = m.regionSellingStation(ctx, hist, a.TypeID, a.RegionID, 0, jumps, jumps[a.SystemID])
			docks[k] = d
		}
		if d.station == 0 {
			continue
		}
		list := underNPC(marketListAt(pair.Usual, b.Tune), m.npcAskAnchor(ctx, d.station, a.TypeID))
		if overSpiralCap(list, m.typeBasePrice(ctx, a.TypeID)) {
			continue
		}
		rates := m.marketRates(ctx, b.CharID, d.station)
		buyCap := dipEntryCap(pair.Usual, list, rates, b.Tune, play.DipEntry)
		room := pair.Room - specClaims.inFlight(a.TypeID, a.RegionID, b.CharID, now)
		if left := dipStackUnits(stack, buyCap) - load.outbound[[2]int64{a.TypeID, a.StationID}]; left < room {
			room = left
		}
		units, outlay := dipLot(stack, buyCap, budget, hold, room)
		if units < 1 || !tradeCargoOK(a.CategoryID, units) {
			continue
		}
		profit := specNetProfit(list, outlay/float64(units), units, rates)
		if profit < tradeMinProfitISK {
			continue
		}
		deal := &tradeDeal{
			Kind: tradeDealSpec, TypeID: a.TypeID, TypeName: a.TypeName, UnitVol: a.UnitVol, CategoryID: a.CategoryID,
			BuyStationID: a.StationID, BuySystemID: a.SystemID, SellStationID: d.station, SellSystemID: d.system,
			Ask: a.Price, Units: units, Outlay: outlay, Profit: profit, ListPrice: list, BuyCap: buyCap,
			Confidence: specVelocityConf(m.specCalibrate(ctx, a.CategoryID, dipConfBase),
				hist.stationPerDay(d.station, a.TypeID), units),
			Evidence: fmt.Sprintf("a crashed ask at %.0f%% of the usual %.2f", a.Price/pair.Usual*100, pair.Usual),
			Jumps:    jumps[a.SystemID] + jumps[d.system],
		}
		deal.Rank = tradeRank(deal)
		deals = append(deals, deal)
	}
	return m.bestTimedRun(ctx, b, cfg, r, jumps, deals, 0)
}

// dipStackUnits is the units in a stack at or under buyCap. Pure.
func dipStackUnits(stack []dipAsk, buyCap float64) int64 {
	var n int64
	for _, a := range stack {
		if a.Price <= buyCap {
			n += a.Units
		}
	}
	return n
}

// overSpiralCap is findSpecRun's spiral brake on a list price: ten times base,
// or a flat cap for a type with no base. A usual price that high is roster
// ping-pong, not a market. Pure.
func overSpiralCap(price, base float64) bool {
	if base > 0 {
		return price > base*specSpiralBaseMul
	}
	return price > specUnanchoredMaxUnit
}

// bestTimedRun measures the top dip or surge runs for real and returns the
// best the pilot may fly: stations it deals at, no order of its own already at
// the sell end, a dock that has not just refused the goods, and a route.
// maxHaul bounds the buy-to-sell leg (0 = none).
func (m *Mod) bestTimedRun(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules,
	jumps map[int64]int, deals []*tradeDeal, maxHaul int) (*tradeDeal, error) {

	if len(deals) == 0 {
		return nil, nil
	}
	sort.Slice(deals, func(i, j int) bool { return deals[i].Rank > deals[j].Rank })
	if len(deals) > specRefineTop {
		deals = deals[:specRefineTop]
	}
	var stations, typeIDs []int64
	for _, d := range deals {
		stations = append(stations, d.BuyStationID, d.SellStationID)
		typeIDs = append(typeIDs, d.TypeID)
	}
	allowed, err := m.stationsPilotDeals(ctx, b, cfg, stations)
	if err != nil {
		return nil, err
	}
	mine, err := m.myOrderStations(ctx, b.CharID, typeIDs)
	if err != nil {
		return nil, err
	}
	var best *tradeDeal
	for _, d := range deals {
		if !allowed[d.BuyStationID] || !allowed[d.SellStationID] ||
			mine[[2]int64{d.TypeID, d.SellStationID}] || mine[[2]int64{d.TypeID, d.SellSystemID}] ||
			deadShelfBlocksSpec(d.ListPrice, m.deadShelfOf(ctx, d.TypeID, d.SellStationID)) {
			continue
		}
		if d.BuyCap > 0 {
			ceiling, ok := dipCeiling(d.BuyCap, m.protectedAskAt(ctx, d.BuyStationID, d.TypeID), d.Ask)
			if !ok {
				continue
			}
			d.BuyCap = ceiling
		}
		haul := 0
		if d.BuySystemID != d.SellSystemID {
			legs, lerr := m.reachFrom(ctx, d.BuySystemID, r)
			if lerr != nil {
				return nil, lerr
			}
			h, ok := legs[d.SellSystemID]
			if !ok {
				continue
			}
			haul = h
		}
		if maxHaul > 0 && haul > maxHaul {
			continue
		}
		d.Jumps = jumps[d.BuySystemID] + haul
		d.Rank = tradeRank(d)
		if best == nil || d.Rank > best.Rank {
			best = d
		}
	}
	return best, nil
}

// stationsPilotDeals is which of these stations the pilot trades at.
func (m *Mod) stationsPilotDeals(ctx context.Context, b *botRun, cfg tradeConfig, ids []int64) (map[int64]bool, error) {
	clause, args := m.tradeStationClause(ctx, b.Profile, b.Tune, cfg)
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID FROM staStations s
		 WHERE s.stationID IN (`+placeholders(len(ids))+`)`+clause,
		append(int64Args(ids), args...)...)
	if err != nil {
		return nil, fmt.Errorf("stations it deals at: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
