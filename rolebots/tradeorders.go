package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// Trading with order slots. A pilot that cares about commerce buys where the goods are, hauls them where they
// are not, and lists them at its own asking price. That commits an order slot, and the order sweep
// (tradebook.go) books the profit when somebody takes the listing, reprices it or pulls it.
//
// How much of this a pilot does is its personality:
//
//	interest    from its commerce appetite. Zero means plain swaps between orders that already exist.
//	markup      from its envy. A content pilot lists a few percent over cost, a grasping one a third more.
//	patience    how long a listing stands before it undercuts, and when it gives up (orderPatience).

const (
	// specMinInterest is the commerce appetite (as a 0..1 interest, below)
	// at which a pilot starts speculating at all. Below it, trading means
	// flipping between orders that already exist.
	specMinInterest = 0.25

	// How wide the speculative search casts. Narrow beside the arbitrage scan: every candidate here is a
	// bet, and the ranking is only as good as the evidence behind it.
	specMaxHubs = 16 // destination markets considered
	// What one jump costs a candidate destination, against its measured liquidity (rankMarkets). Gentler than the
	// ranking's own distance term, which applies again to the actual run: this only decides which markets get costed.
	specDestJumpCost   = 0.25
	specMaxWantTypes   = 120 // types per evidence source
	specWantTypeCap    = 260 // types the whole search will cost, all sources together
	specSourcesPerType = 3   // cheapest asks per type
	specRefineTop      = 6   // runs given a real buy-to-sell route measurement

	// specWantStationsPerType is how many shortfall stations per type the
	// mineral ledger read keeps, the ledger being per (type, station).
	specWantStationsPerType = 8

	// How many of the best-stocked stations the supply-gap source reads its catalogue from, and how many the pilot
	// keeps once distance is priced in. The catalogue is what a trader knows is obtainable; the reachable few are
	// where it would go and get it.
	specStockedStationPool = 60
	specSupplyStations     = 8

	// Confidence in a listing actually selling. The floor is what a
	// listing at a hub is worth on the hub's own account; the rest is
	// bought with evidence.
	specConfBase    = 0.45
	specConfBidHere = 0.15 // somebody in the region is bidding for it
	specConfScarce  = 0.10 // nobody in the region is selling it
	specConfLedger  = 0.15 // a rolebot tried to buy it and could not
	specConfShopGap = 0.15 // shoppers came to this very counter and found it short
	specConfMax     = 0.85

	// specShopDests is how many shopper-gap counters (stationdemand.go) join
	// the destinations beside the liquidity hubs.
	specShopDests = 6

	// What scarcity is worth on top of the pilot's own asking margin.
	specScarcityMarkup = 0.15

	// A listing stands for a fortnight, longer than any pilot's patience: the owner deciding it is not selling
	// (nudgeOrder) should end an order, not the expiry timer, since only that puts the goods back where the planner finds them.
	tradeListDays = 14

	// stockMinValue is what a hangar full of leftovers has to be worth
	// before a pilot will make a trip of it. Below this it is not stock,
	// it is litter.
	stockMinValue = 20_000.0

	// stockCooldownSec is how long freshly acquired (or freshly failed) stock is left alone. It breaks the loop where
	// a holding that cannot be cleared returns to the planner as its best idea every time, and it keeps a pilot from
	// dropping everything for a bid that filled seconds ago.
	stockCooldownSec = 900
)

// tradeInterestBase is what an even-tempered pilot cares about commerce. It is above zero because findSpecRun,
// tradeBids and the mineral import duty all refuse below specMinInterest: at zero only pilots with an extreme
// temperament would speculate or haul minerals. The trait spread runs on top of it.
const tradeInterestBase = 0.4

// tradeInterest is how much this pilot cares about commerce, 0..1.
func tradeInterest(tn Tuning) float64 {
	// The consumer/supplier axis is the second pull: a pilot that reads the shortage ledger for a living cares
	// more about moving goods than one that makes them.
	v := tradeInterestBase + (tn.TradeAppetite-1.0)*tn.TradeSupplyBias
	switch {
	case v <= 0:
		return 0
	case v >= 1:
		return 1
	}
	return v
}

// specMarkup is what the pilot asks over what the goods cost it. Envy owns it: TradeMinMarginFrac is already the
// greed-bent asking spread (tuning.go), so a content pilot lists at 6% over cost and a grasping one at 35%.
func specMarkup(tn Tuning, scarce bool) float64 {
	mk := 1 + tn.TradeMinMarginFrac
	if scarce {
		mk += specScarcityMarkup
	}
	return mk
}

// ── listing a run ───────────────────────────────────────────────────────────

// tradeListRun ends a speculative run: the goods go on the market at the pilot's asking price and the pilot
// goes back to work. What happens next is somebody else's decision.
func (m *Mod) tradeListRun(ctx context.Context, b *botRun, itemID, qty int64) error {
	st := b.State
	d := &st.Data
	// The run has arrived: whatever it lists now stands on the book, where
	// the next pilot's marketRoom reads it as listed rather than in flight.
	specClaims.release(b.CharID)

	price := d.TradeListPrice
	if price <= 0 {
		price = d.TradeUnitCost * specMarkup(b.Tune, false)
	}
	// Goods bought cheap list near the usual price, unless this dock already
	// refused them and the relist was priced under that failure (deadshelf.go).
	if m.deadShelfOf(ctx, d.TradeTypeID, b.StationID).Laps == 0 {
		if lp := m.cheapListPrice(ctx, d.TradeUnitCost, b.StationID, d.TradeTypeID, b.Tune); lp > price {
			price = lp
		}
	}
	// The same caps the spec planner buys under apply to what this run lists at: a cost basis inherited from a
	// drifted book must not come back as a fresh ask. An ask nobody will hit is not a price.
	if base := m.typeBasePrice(ctx, d.TradeTypeID); base > 0 {
		if cap := base * specSpiralBaseMul; price > cap {
			price = cap
		}
	} else if price > specUnanchoredMaxUnit {
		price = specUnanchoredMaxUnit
	}
	// Never under the regional NPC band (npcanchor.go), since a zero cost basis would list at nothing. At a dock
	// that refused these goods the relist is lifted only to the upkeep floor (deadShelfRelist).
	if m.deadShelfOf(ctx, d.TradeTypeID, b.StationID).Laps > 0 {
		if nf := m.anchoredFloor(ctx, b.StationID, d.TradeTypeID); price < nf {
			price = nf
		}
	} else {
		price, _ = m.anchoredAsk(ctx, b.Tune, b.StationID, d.TradeTypeID, price)
	}
	// Somebody may have started selling here in transit: undercut them, but never below cost plus the thinnest
	// spread the pilot accepts. Below that the sweep decides to take the goods elsewhere.
	var rival sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MIN(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1
		  AND volRemaining > 0 AND charID <> ?`,
		b.StationID, d.TradeTypeID, b.CharID).Scan(&rival)
	floor := d.TradeUnitCost * (1 + b.Tune.TradeFloorMarginFrac)
	if nf := m.anchoredFloor(ctx, b.StationID, d.TradeTypeID); nf > floor {
		floor = nf
	}
	if hf := m.holdFloorAt(ctx, d.TradeUnitCost, b.StationID, d.TradeTypeID, b.Tune); hf > floor {
		floor = hf
	}
	if rival.Valid && rival.Float64*0.99 < price {
		if want := rival.Float64 * 0.99; want >= floor {
			price = want
		}
	}
	// At a factory that wants these goods, never over what its industrials would pay otherwise (factorydemand.go),
	// and never under the floor either.
	fc := m.factoryAskCap(ctx, b.StationID, d.TradeTypeID)
	if fc > 0 && price > fc {
		price = math.Max(fc, floor)
	}
	if price <= 0 {
		// The goods are in this hangar and the run that carried them is about to be wiped: back on the stock ledger
		// at cost, or nothing ever comes back for them.
		m.addStock(ctx, b.CharID, b.StationID, b.SystemID, d.TradeTypeID, d.TradeName, qty, d.TradeUnitCost)
		return m.tradeAbandon(ctx, b, "no sensible price to list at")
	}

	// List what the region buys (mktdaily.go): a week of its demand, and nothing where it bought none in a fortnight.
	// The rest stays in the hangar as stock; a carried print and a factory's own want are their own evidence.
	var rest int64 // units left in the hangar by this cap or a partial listing
	if hist := m.marketHistory(ctx); hist.Ready && d.TradeItemID == 0 && fc <= 0 {
		region := m.stationRegionOf(ctx, b.StationID)
		take := marketAbsorb(qty, hist.regionPerDay(region, d.TradeTypeID))
		if take < 1 {
			m.logBot(b.CharID, logInfo, "trade",
				"nobody in this region has bought %s in two weeks — not listing %d here, the goods wait in the hangar",
				d.TradeName, qty)
			m.restock(ctx, b, qty)
			st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
			st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
			return saveState(ctx, m.db, st)
		}
		if take < qty {
			m.logBot(b.CharID, logInfo, "trade",
				"this region buys about %d× %s a week — listing that many of %d, the rest waits in the hangar",
				take, d.TradeName, qty)
			rest, qty = qty-take, take
		}
	}

	if m.freeOrderSlots(ctx, b.CharID) <= 0 {
		m.logBot(b.CharID, logWarn, "trade",
			"wanted to list %d× %s here but every order slot is committed — the goods stay in the hangar",
			qty+rest, d.TradeName)
		m.restock(ctx, b, qty+rest)
		st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
		st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
		return saveState(ctx, m.db, st)
	}

	// A standing order of these goods on this dock absorbs the load instead of gaining a twin beside it. The order
	// keeps its own price and cost basis, so the run's profit books against the older basis.
	if topped, terr := m.topUpOwnSellOrder(ctx, b, d.TradeTypeID, itemID, qty); terr != nil {
		m.logBot(b.CharID, logError, "trade", "topping up its %s order: %v — listing instead",
			d.TradeName, terr)
	} else if topped {
		m.takeStock(ctx, b.CharID, b.StationID, d.TradeTypeID, qty)
		m.restock(ctx, b, rest)
		st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
		st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
		return saveState(ctx, m.db, st)
	}

	// A resting listing pays its broker fee at placement and the engine refuses it when the wallet cannot cover it, so a
	// destitute pilot sells into the standing bids instead (no fee), and a partial lot lists what the fee covers.
	rates := m.marketRates(ctx, b.CharID, b.StationID)
	if bal, balErr := evedb.GetCharBalance(ctx, m.db, b.CharID); balErr == nil &&
		bal < rates.brokerFee(price*float64(qty)) {
		k := affordableListQty(bal, price, qty, rates)
		if k < 1 || m.standingBidAt(ctx, b.StationID, d.TradeTypeID, b.CharID) {
			m.restock(ctx, b, rest)
			return m.tradeSellToBids(ctx, b, itemID, qty)
		}
		m.logBot(b.CharID, logInfo, "trade",
			"can pay the broker fee on %d of its %d× %s — listing those, the rest waits in the hangar",
			k, qty, d.TradeName)
		rest, qty = rest+qty-k, k
	}

	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID, d.TradeTypeID,
		itemID, price, qty, -1 /*station range*/, 1, tradeListDays, false)
	if err != nil {
		return fmt.Errorf("trade listing: %w", err)
	}
	if out.ErrorMsg != "" {
		// The engine has the last word on the fee: a wallet that moved since the pre-check can still be refused here.
		// Same fallback as industry.go and refinelist.go: clear against the standing bid, which pays no broker fee.
		if brokerFeeRefusal(out.ErrorMsg) {
			if sold, _ := m.distressSell(ctx, b, b.StationID, d.TradeTypeID, itemID, qty); sold > 0 {
				m.restock(ctx, b, qty-sold+rest)
				st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
				st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
				return saveState(ctx, m.db, st)
			}
		}
		m.logBot(b.CharID, logWarn, "trade",
			"listing refused (%s) — %d× %s stays in the hangar", out.ErrorMsg, qty+rest, d.TradeName)
		m.restock(ctx, b, qty+rest)
		st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
		st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
		return saveState(ctx, m.db, st)
	}

	// A listing can fill on the spot when somebody's bid was already above
	// the asking price. That part is an ordinary sale and books as one.
	if out.FilledQty > 0 {
		var gross float64
		for _, f := range out.Fills {
			gross += f.Price * float64(f.Quantity)
		}
		e := tradeEntry{
			CharID: b.CharID, Kind: tradeKindSell, TypeID: d.TradeTypeID, TypeName: d.TradeName,
			Qty: out.FilledQty, Price: gross / float64(out.FilledQty), Value: gross,
			// Net of sales tax (tradefees.go). The broker fee on the unfilled remainder is booked separately below.
			Profit:  m.netSaleAt(ctx, b.CharID, b.StationID, gross) - d.TradeUnitCost*float64(out.FilledQty),
			Station: b.StationID, System: b.SystemID, FromSys: d.TradeBuySystemID,
			Note: "the listing filled against a standing bid on the spot",
		}
		m.bookTrade(ctx, e)
		m.notePeerFills(ctx, e, out.Fills, false, "listing")
	}
	resting := qty - out.FilledQty
	if resting > 0 && out.RestingOrderID != 0 {
		kind := tradeDealSpec
		if d.TradeItemID != 0 {
			kind = tradeDealBPO // a carried print waits for a factory, not a passer-by
		}
		m.registerOrder(ctx, b.CharID, out.RestingOrderID, d.TradeTypeID, b.StationID, b.SystemID,
			d.TradeName, false, price, d.TradeUnitCost, resting, kind)
		m.bookTrade(ctx, tradeEntry{
			CharID: b.CharID, Kind: tradeKindList, TypeID: d.TradeTypeID, TypeName: d.TradeName,
			Qty: resting, Price: price, Value: price * float64(resting),
			Station: b.StationID, System: b.SystemID, FromSys: d.TradeBuySystemID,
			OrderID: out.RestingOrderID,
			Note: fmt.Sprintf("asking %.2f on goods that cost %.2f (%.0f%% over)",
				price, d.TradeUnitCost, (price/max(d.TradeUnitCost, 0.01)-1)*100),
		})
		m.bookBrokerFee(ctx, b, out.RestingOrderID, d.TradeTypeID, d.TradeName, price, resting)
		m.logBot(b.CharID, logInfo, "trade",
			"listed %d× %s at %.2f in %s — cost %.2f, so %.0f%% over, good for %d days",
			resting, d.TradeName, price, m.nameOfSystem(ctx, b.SystemID), d.TradeUnitCost,
			(price/max(d.TradeUnitCost, 0.01)-1)*100, tradeListDays)
	}
	// The goods are in an order now, carrying their cost, so they are no longer idle inventory; what a partial
	// listing left behind still is.
	m.takeStock(ctx, b.CharID, b.StationID, d.TradeTypeID, qty)
	m.restock(ctx, b, rest)

	st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return saveState(ctx, m.db, st)
}

// tradeSellToBids ends a run whose listing the pilot cannot afford to rest: a duration-0 sale into the best
// standing bid here, which pays no broker fee. The price is whatever is bid. What the bids do not take goes on
// the stock ledger like any other refused listing.
func (m *Mod) tradeSellToBids(ctx context.Context, b *botRun, itemID, qty int64) error {
	st := b.State
	d := &st.Data

	var bid sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MAX(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1
		  AND volRemaining > 0 AND charID <> ?`,
		b.StationID, d.TradeTypeID, b.CharID).Scan(&bid)

	var sold int64
	var gross float64
	if bid.Valid && bid.Float64 > 0 {
		out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID, d.TradeTypeID,
			itemID, bid.Float64, qty, -1 /*station range*/, 1, 0 /*immediate only*/, false)
		if err != nil {
			return fmt.Errorf("bid dump: %w", err)
		}
		if out.ErrorMsg == "" && out.FilledQty > 0 {
			sold = out.FilledQty
			for _, f := range out.Fills {
				gross += f.Price * float64(f.Quantity)
			}
			e := tradeEntry{
				CharID: b.CharID, Kind: tradeKindSell, TypeID: d.TradeTypeID, TypeName: d.TradeName,
				Qty: sold, Price: gross / float64(sold), Value: gross,
				Profit:  m.netSaleAt(ctx, b.CharID, b.StationID, gross) - d.TradeUnitCost*float64(sold),
				Station: b.StationID, System: b.SystemID, FromSys: d.TradeBuySystemID,
				Note: "sold into the standing bid — the wallet could not cover a listing's broker fee",
			}
			m.bookTrade(ctx, e)
			m.notePeerFills(ctx, e, out.Fills, false, "listing")
		}
	}
	if sold > 0 {
		m.logBot(b.CharID, logInfo, "trade",
			"could not afford the broker fee to list %d× %s — sold %d into the standing bid for %.2f ISK instead",
			qty, d.TradeName, sold, gross)
	} else {
		m.logBot(b.CharID, logInfo, "trade",
			"could not afford the broker fee to list %d× %s and nobody here bids on it — the goods stay in the hangar",
			qty, d.TradeName)
	}
	m.restock(ctx, b, qty-sold)
	st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return saveState(ctx, m.db, st)
}

// restock files goods a run brought to this dock and could not place back on the stock ledger, unless the run was
// a stock run, whose goods never left the ledger (tradeStockPlan lists where the goods are). A bought run is
// TradeFresh even when it lists where it bought. Adding a stock run's goods again would double the row each lap.
func (m *Mod) restock(ctx context.Context, b *botRun, units int64) {
	d := &b.State.Data
	if units <= 0 || (d.TradeBuyStationID == b.StationID && !d.TradeFresh) {
		return
	}
	m.addStock(ctx, b.CharID, b.StationID, b.SystemID, d.TradeTypeID, d.TradeName, units, d.TradeUnitCost)
}

// standingBidAt reports whether anybody else bids on this type at this dock.
func (m *Mod) standingBidAt(ctx context.Context, stationID, typeID, exceptChar int64) bool {
	var one int
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1
		  AND volRemaining > 0 AND charID <> ? LIMIT 1`,
		stationID, typeID, exceptChar).Scan(&one)
	return err == nil
}

// ── inventory the pilot is already holding ──────────────────────────────────

// tradeStockPlan makes a run out of goods the pilot already owns, left by a filled standing bid or a pulled
// listing. A planner that ignored them would let a trader turn its wallet into forgotten crates.
// Reports whether it planned something.
func (m *Mod) tradeStockPlan(ctx context.Context, b *botRun) (bool, error) {
	rows, err := m.pilotStock(ctx, b.CharID)
	if err != nil || len(rows) == 0 {
		return false, nil
	}
	// Unscoped: the loop below drops stock whose system is not in jumps, so a residency scope would write off crates
	// bought before a move (reach.go). Residency bounds where a bot looks, never where it collects.
	r := rulesForBot(b)
	jumps, err := m.reachFrom(ctx, b.SystemID, r)
	if err != nil {
		return false, nil
	}
	now := clock.Unix()
	bal := -1.0 // read on first need
	hist := m.marketHistory(ctx)
	for _, s := range rows {
		// The litter gate values the row at what it cost. Loot stock is filed at cost 0, so a zero-cost row is valued
		// at the going rate instead; profit booking still uses the zero basis.
		basis := s.UnitCost
		if basis <= 0 {
			if est, err := m.estimatePriceFor(ctx, b, s.TypeID); err == nil && est > 0 {
				basis = est
			}
		}
		if float64(s.Units)*basis < stockMinValue {
			continue
		}
		if now-s.AcquiredAt < stockCooldownSec {
			continue
		}
		if _, ok := jumps[s.SystemID]; !ok {
			continue // the pilot will not fly to where it left them
		}
		outStation, outSystem, bid, err := m.bestOutlet(ctx, b, s, r, jumps)
		if err != nil {
			return false, err
		}
		canList := m.freeOrderSlots(ctx, b.CharID) > 0
		if outStation == 0 && !canList {
			continue // nowhere that pays, and no slot to list it in
		}

		st := b.State
		st.Data = idleData(st.Data)
		st.Data.TradeKind = tradeDealStock
		st.Data.TradeTypeID = s.TypeID
		st.Data.TradeName = s.TypeName
		st.Data.TradeQty = s.Units
		st.Data.TradeUnitCost = s.UnitCost
		st.Data.TradeSpent = s.UnitCost * float64(s.Units)
		st.Data.TradeBuyStationID = s.StationID
		st.Data.TradeBuySystemID = s.SystemID

		switch {
		case outStation == 0:
			// No bid in reach pays what the goods cost: a pilot with a free slot lists at its own price and lets the order sweep
			// give ground (nudgeOrder). A sub-lot mineral holding (mineralListFloors) waits in stock for a bid.
			if _, isMineral := mineralListFloors[s.TypeID]; isMineral &&
				s.Units < mineralUnitFloor(s.TypeID, s.UnitCost, 1) {
				continue
			}
			// Where the goods sit lists them only if the region buys them (mktdaily.go). Otherwise the listing goes to the
			// dock in reach that buys the most, and with none in reach the goods wait for a bid.
			sellAt, sellSys := s.StationID, s.SystemID
			if hist.Ready && marketAbsorb(s.Units,
				hist.regionPerDay(m.stationRegionOf(ctx, s.StationID), s.TypeID)) < 1 {
				sellAt, sellSys = m.sellingStation(ctx, hist, s.TypeID, s.StationID, jumps, jumps[s.SystemID])
				if sellAt == 0 {
					continue // nobody in reach buys it; wait for a bid
				}
			}
			// A zero-basis row (salvage, anything filed at cost 0) has no cost to mark up; a zero ask would be abandoned
			// on arrival, so it is priced the way every other piece of loot is.
			listPrice := s.UnitCost * specMarkup(b.Tune, false)
			if listPrice <= 0 {
				listPrice = m.lootAskPrice(ctx, b, s.TypeID)
			}
			if lp := m.cheapListPrice(ctx, s.UnitCost, sellAt, s.TypeID, b.Tune); lp > listPrice {
				listPrice = lp
			}
			if listPrice <= 0 {
				continue // nothing to anchor an ask on; wait for a bid
			}
			// The listing branch relists where the goods are, often after a listing here was pulled unsold. Relisting at
			// the failed price only pays a broker fee again (deadshelf.go): go under it to a sunk-cost floor, then wait for a bid.
			floor := math.Max(s.UnitCost*deadShelfLossFloor,
				math.Max(m.stableFloorPrice(ctx, s.TypeID), m.anchoredFloor(ctx, sellAt, s.TypeID)))
			floor = math.Max(floor, m.holdFloorAt(ctx, s.UnitCost, sellAt, s.TypeID, b.Tune))
			relist, ok := deadShelfRelist(listPrice, floor, m.deadShelfOf(ctx, s.TypeID, sellAt))
			// Goods held for a recovery try another dock in the region before
			// they wait: the region buys them even if this dock did not.
			if !ok && boughtCheap(s.UnitCost, m.usualPriceAt(ctx, sellAt, s.TypeID)) {
				if alt, altSys := m.regionSellingStation(ctx, hist, s.TypeID, m.stationRegionOf(ctx, sellAt),
					sellAt, jumps, jumps[s.SystemID]); alt != 0 {
					if p, altOK := deadShelfRelist(listPrice, floor, m.deadShelfOf(ctx, s.TypeID, alt)); altOK {
						sellAt, sellSys, relist, ok = alt, altSys, p, true
					}
				}
			}
			if !ok {
				continue // this dock has refused it enough; wait for a bid
			}
			listPrice = relist
			// A pilot that cannot pay the fee to list even one unit is skipped, so the pass falls through to a run the
			// wallet can fund (tradecapital.go).
			if bal < 0 {
				bal = 0
				if v, berr := evedb.GetCharBalance(ctx, m.db, b.CharID); berr == nil {
					bal = v
				}
			}
			if affordableListQty(bal, listPrice, s.Units, m.marketRates(ctx, b.CharID, sellAt)) < 1 {
				continue
			}
			st.Data.TradeKind = tradeDealSpec
			st.Data.TradeSellStationID = sellAt
			st.Data.TradeSellSystemID = sellSys
			st.Data.TradeListPrice = listPrice
			if sellAt != s.StationID {
				m.logBot(b.CharID, logInfo, "trade",
					"has %d× %s sitting at %s where nobody buys it — collecting it to list at %s, which does",
					s.Units, s.TypeName, m.stationLabel(ctx, s.StationID), m.stationLabel(ctx, sellAt))
				return true, m.beginTravel(ctx, b, s.SystemID, s.StationID, string(TaskTradePickup))
			}
			m.logBot(b.CharID, logInfo, "trade",
				"has %d× %s sitting at %s and nobody in reach bids what they cost — going to list them itself",
				s.Units, s.TypeName, m.stationLabel(ctx, s.StationID))
			return true, m.beginTravel(ctx, b, s.SystemID, s.StationID, string(TaskTradeSell))

		case outStation == s.StationID:
			st.Data.TradeSellStationID = s.StationID
			st.Data.TradeSellSystemID = s.SystemID
			st.Data.TradeBid = bid
			m.logBot(b.CharID, logInfo, "trade",
				"has %d× %s sitting at %s with a %.2f bid on the same dock — going to clear it",
				s.Units, s.TypeName, m.stationLabel(ctx, s.StationID), bid)
			return true, m.beginTravel(ctx, b, s.SystemID, s.StationID, string(TaskTradeSell))

		default:
			st.Data.TradeSellStationID = outStation
			st.Data.TradeSellSystemID = outSystem
			st.Data.TradeBid = bid
			m.logBot(b.CharID, logInfo, "trade",
				"has %d× %s sitting at %s — collecting it and hauling it to a %.2f bid at %s",
				s.Units, s.TypeName, m.stationLabel(ctx, s.StationID), bid, m.stationLabel(ctx, outStation))
			return true, m.beginTravel(ctx, b, s.SystemID, s.StationID, string(TaskTradePickup))
		}
	}
	return false, nil
}

// bestOutlet finds where a holding is best unloaded: the bid that pays most once the flying is priced in, or
// nothing when no bid in reach beats what the goods cost. Returns (0, 0, localBid, nil) when the pilot's own
// station is the answer.
func (m *Mod) bestOutlet(ctx context.Context, b *botRun, s stockRow, r travelRules,
	jumps map[int64]int) (int64, int64, float64, error) {

	// Narrowed to the regions the pilot's horizon covers before the LIMIT: the dearest bids for a type are mostly
	// hub bids the jumps test would discard, which could hide a good local bid below the cut.
	args := []any{s.TypeID, b.CharID}
	scope := ""
	if regions := reachRegions(m, ctx, jumps); len(regions) > 0 {
		scope = ` AND o.regionID IN (` + placeholders(len(regions)) + `)`
		args = append(args, int64Args(regions)...)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.solarSystemID, MAX(o.price), SUM(o.volRemaining)
		  FROM market_orders o
		 WHERE o.typeID = ? AND o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		   AND o.charID <> ?`+scope+`
		 GROUP BY o.stationID, o.solarSystemID
		 ORDER BY MAX(o.price) DESC
		 LIMIT 60`, args...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("stock outlet: %w", err)
	}
	defer rows.Close()

	var bestStation, bestSystem int64
	var bestPrice, bestRank, localBid float64
	for rows.Next() {
		var station, system, vol int64
		var price float64
		if err := rows.Scan(&station, &system, &price, &vol); err != nil {
			return 0, 0, 0, err
		}
		if station == s.StationID {
			localBid = price
		}
		hops, ok := jumps[system]
		if !ok {
			continue
		}
		units := s.Units
		if vol < units {
			units = vol
		}
		profit := (price - s.UnitCost) * float64(units)
		if profit <= 0 {
			continue
		}
		// The flying is measured from where the goods are, since the pilot must fetch them either way.
		fromStock := hops
		if h, ok2 := jumps[s.SystemID]; ok2 && h < hops {
			fromStock = hops - h
			if fromStock < 0 {
				fromStock = 0
			}
		}
		rank := profit / float64(1+fromStock)
		if rank > bestRank {
			bestRank, bestStation, bestSystem, bestPrice = rank, station, system, price
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, err
	}
	// Selling on the spot beats a trip unless the trip is worth clearly more: the run has a hold and a fuel bill
	// either way.
	if localBid > 0 && bestStation != 0 {
		local := (localBid - s.UnitCost) * float64(s.Units)
		if local > 0 && local*1.5 > bestRank {
			return s.StationID, s.SystemID, localBid, nil
		}
	}
	if bestStation == 0 {
		return 0, 0, localBid, nil
	}
	return bestStation, bestSystem, bestPrice, nil
}

// tradePickup collects stock out of a hangar into the hold, on the way to
// wherever it is being sold.
func (m *Mod) tradePickup(ctx context.Context, b *botRun) error {
	st := b.State
	d := &st.Data
	if b.StationID != d.TradeBuyStationID {
		return fmt.Errorf("trade.pickup at station %d, expected %d", b.StationID, d.TradeBuyStationID)
	}
	if b.ShipID == 0 {
		return fmt.Errorf("trade.pickup without a ship to load")
	}
	var qty int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(quantity), 0) FROM entity
		WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0`,
		b.CharID, b.StationID, d.TradeTypeID).Scan(&qty); err != nil {
		return fmt.Errorf("trade.pickup stock: %w", err)
	}
	if qty <= 0 {
		m.takeStock(ctx, b.CharID, b.StationID, d.TradeTypeID, d.TradeQty)
		return m.tradeAbandon(ctx, b, "the goods it came back for are not in the hangar any more")
	}
	if _, err := m.mergeStacks(ctx, b.CharID, b.StationID, d.TradeTypeID); err != nil {
		return err
	}
	// The hold takes what fits; the rest stays in this hangar, on the books.
	loaded, err := m.moveHangarToCargoN(ctx, b, d.TradeTypeID)
	if err != nil {
		return fmt.Errorf("loading the hold: %w", err)
	}
	if loaded <= 0 {
		return m.tradeAbandon(ctx, b, "the hold has no room for the goods it came back for")
	}
	qty = loaded
	d.TradeQty = qty
	d.TradeSpent = d.TradeUnitCost * float64(qty)
	m.takeStock(ctx, b.CharID, b.StationID, d.TradeTypeID, qty)
	m.logBot(b.CharID, logInfo, "trade", "collected %d× %s — hauling it to %s",
		qty, d.TradeName, m.stationLabel(ctx, d.TradeSellStationID))
	st.Phase = string(TaskTradeLoad)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return saveState(ctx, m.db, st)
}

// ── the speculative search ──────────────────────────────────────────────────

// The spiral brake's two constants (see findSpecRun). Ten times base is far above any organic scarcity (NPC
// dynamics cap peak under 2x base) and a million ISK a unit is more than any unanchored meta module is worth.
const (
	specSpiralBaseMul     = 10.0
	specUnanchoredMaxUnit = 1_000_000.0
)

// typeBasePrice is the authored anchor for one type, 0 when it has none.
func (m *Mod) typeBasePrice(ctx context.Context, typeID int64) float64 {
	r, ok := typeRecFrom(ctx, m.db, typeID)
	if !ok {
		return 0
	}
	base := sql.NullFloat64{Float64: r.BasePrice, Valid: r.BasePrice != 0}
	if !base.Valid {
		return 0
	}
	return base.Float64
}

// specWant is one type there is evidence somebody wants somewhere.
type specWant struct {
	TypeID   int64
	TypeName string
	Ledger   bool // a rolebot tried to buy it and nothing in reach sold it
	// Stations is where a named factory said it was short of the type (mineral shortfall ledger, factorydemand.go),
	// station to units. A destination in this map is not guessing: somebody there could not cover a bill.
	Stations map[int64]int64
}

// specSource is the cheapest place the pilot could buy that type.
type specSource struct {
	StationID int64
	SystemID  int64
	// RegionID is read for the import duty alone (mineralimport.go): an ordinary speculation owes the seller's region
	// nothing, but a duty run that empties one shelf to fill another has moved the shortage rather than served it.
	RegionID   int64
	Price      float64
	Stock      int64
	UnitVol    float64
	CategoryID int64
}

// findSpecRun looks for goods to move somewhere that has none, and prices them there itself. Returns (nil, nil)
// when this pilot does not speculate, which is not an error.
func (m *Mod) findSpecRun(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules,
	jumps map[int64]int, budget, hold float64) (*tradeDeal, error) {

	if tradeInterest(b.Tune) < specMinInterest {
		return nil, nil
	}
	if m.freeOrderSlots(ctx, b.CharID) <= 0 {
		return nil, fmt.Errorf("every one of its order slots is already committed")
	}
	dests := m.specDestinations(ctx, b, cfg, jumps)
	if len(dests) == 0 {
		return nil, fmt.Errorf("no market it can reach is worth carrying goods to")
	}
	wants, err := m.specWants(ctx, dests, jumps)
	if err != nil {
		return nil, err
	}
	if len(wants) == 0 {
		return nil, fmt.Errorf("nothing anywhere is short of anything it could carry")
	}

	typeIDs := make([]int64, 0, len(wants))
	for _, w := range wants {
		typeIDs = append(typeIDs, w.TypeID)
	}
	sources, err := m.specSources(ctx, b, cfg, r, typeIDs)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("nowhere it can fly to sells any of the goods that are short")
	}
	haveAsk, regionAsk, regionBid, err := m.specMarketPresence(ctx, b.CharID, dests, typeIDs)
	if err != nil {
		return nil, err
	}
	// What each destination region has actually been paying. A listing is priced against it, never above: a
	// markup is only a price if somebody there has paid that much.
	refPrice := m.specRefPrices(ctx, dests, typeIDs)
	// What each destination has been buying (mktdaily.go): the lot is sized to a week of its region's demand, the
	// price never exceeds what the dock itself cleared at, and a dock with its own buying record is the better bet.
	hist := m.marketHistory(ctx)
	// Where shoppers came for the goods and found the shelf short
	// (stationdemand.go).
	var shopGap map[[2]int64]float64
	if m.outletEnabled(ctx) {
		shopGap = m.stationDemand(ctx).Gap
	}
	// Speculation needs the raw authored basePrice as its anchor: derived prices are poisoned by pilots re-buying each
	// other's markups. An ask ten times basePrice is a spiral, and a type with no basePrice gets a flat cap.
	basePriceOf := map[int64]float64{}
	if len(typeIDs) > 0 {
		if rows, berr := m.db.QueryContext(ctx, `
			SELECT typeID, COALESCE(basePrice, 0) FROM invTypes
			WHERE typeID IN (`+placeholders(len(typeIDs))+`)`, int64Args(typeIDs)...); berr == nil {
			for rows.Next() {
				var id int64
				var bp float64
				if rows.Scan(&id, &bp) == nil {
					basePriceOf[id] = bp
				}
			}
			rows.Close()
		}
	}
	// One listing of one item per station: a second order competes with the first and burns a slot. A station the
	// pilot already sells this type at is off the list, so the search falls through to the next hub down.
	mine, err := m.myOrderStations(ctx, b.CharID, typeIDs)
	if err != nil {
		return nil, err
	}

	// What each destination region already lists of the wanted goods, read
	// once per region off the roster-wide seller snapshot.
	listedByRegion := map[int64]map[int64]int64{}
	listedIn := func(regionID int64) map[int64]int64 {
		if l, ok := listedByRegion[regionID]; ok {
			return l
		}
		l := m.regionListedUnits(ctx, regionID, typeIDs)
		listedByRegion[regionID] = l
		return l
	}

	var deals []*tradeDeal
	for _, w := range wants {
		for _, src := range sources[w.TypeID] {
			if _, ok := jumps[src.SystemID]; !ok {
				continue
			}
			for _, dst := range dests {
				if dst.SystemID == src.SystemID {
					continue // moving goods across a station is not a trade
				}
				// A factory destination (factorydemand.go): what its factories want and would otherwise pay. A dock added for
				// its factories alone is offered nothing else.
				var facWant int64
				var facAlt float64
				if dst.Factory != nil {
					facWant, facAlt = dst.Factory.Want[w.TypeID], dst.Factory.Alt[w.TypeID]
				}
				if dst.FactoryOnly && facWant <= 0 {
					continue
				}
				// An NPC seeder's shelf closes the dock. Other pilots' stock is competition: a dock whose buyers clear more than
				// the queue in a week has room for one more seller (specStationRoom), and with no history any seller closes it.
				dock, crowded := haveAsk[[2]int64{w.TypeID, dst.StationID}]
				if crowded && (dock.Seeded || !hist.Ready) {
					continue
				}
				if mine[[2]int64{w.TypeID, dst.StationID}] || mine[[2]int64{w.TypeID, dst.SystemID}] {
					continue // it already sells this here: try the next hub down
				}
				scarce := !regionAsk[[2]int64{w.TypeID, dst.RegionID}]
				conf := specConfBase
				if w.Ledger {
					conf += specConfLedger
				}
				if regionBid[[2]int64{w.TypeID, dst.RegionID}] {
					conf += specConfBidHere
				}
				if scarce {
					conf += specConfScarce
				}
				if facWant > 0 || w.Stations[dst.StationID] > 0 {
					conf += specConfFactory // a named factory here is short of it
				}
				if shopGap[[2]int64{w.TypeID, dst.StationID}] > 0 {
					conf += specConfShopGap
				}
				if conf > specConfMax {
					conf = specConfMax
				}
				// What the market has honoured for this kind of goods (tradecapital.go): the terms above rank bets against
				// each other, this is what any of them is worth against a sure run.
				conf = m.specCalibrate(ctx, src.CategoryID, conf)

				list := specListPrice(src.Price, specMarkup(b.Tune, scarce),
					refPrice[[2]int64{w.TypeID, dst.RegionID}])
				// Never over what the industrial standing there would pay otherwise: a feed above the alternative is a shelf
				// nobody at the factory has a reason to take from.
				list = factoryAskPrice(list, facAlt)
				if hist.Ready {
					if mp := hist.Station[[2]int64{dst.StationID, w.TypeID}].MaxPrice; mp > 0 && list > mp {
						list = mp
					}
				}
				// Goods bought cheap are priced off the region, past this
				// dock's own record: its best sale may be a crash price.
				if facAlt <= 0 {
					if lp := m.cheapListPrice(ctx, src.Price, dst.StationID, w.TypeID, b.Tune); lp > list {
						list = lp
					}
				}
				// An empty shelf is not a hungry one if the same goods were just pulled from it unsold (deadshelf.go).
				if deadShelfBlocksSpec(list, m.deadShelfOf(ctx, w.TypeID, dst.StationID)) {
					continue
				}
				vol := src.UnitVol
				if vol <= 0 {
					vol = 0.01
				}
				units := src.Stock
				if byISK := int64(budget / src.Price); byISK < units {
					units = byISK
				}
				if byVol := int64(hold / vol); byVol < units {
					units = byVol
				}
				// A factory-only dock is stocked to its want, not past it:
				// nobody else there is buying.
				if dst.FactoryOnly && facWant < units {
					units = facWant
				}
				// A week of what the region buys, none where it bought nothing in a fortnight (mktdaily.go); a named factory's want
				// is its own evidence. Less what the region lists and others carry there (specclaims.go); a crowded dock takes the leftover.
				if crowded {
					demand := hist.stationPerDay(dst.StationID, w.TypeID) * mktAbsorbDays
					if facWant > 0 {
						demand = float64(facWant)
					}
					if units = specStationRoom(units, demand, dock.Depth); units < 1 {
						continue
					}
				}
				if hist.Ready && facWant <= 0 {
					listed := listedIn(dst.RegionID)[w.TypeID]
					flying := specClaims.inFlight(w.TypeID, dst.RegionID, b.CharID, clock.Unix())
					if units = marketRoom(units, hist.regionPerDay(dst.RegionID, w.TypeID), listed, flying); units < 1 {
						continue
					}
				}
				// Ammunition and modules travel; hulls only do when the
				// hold is an industrial's (trade.go, tradeCargoOK).
				if !tradeCargoOK(src.CategoryID, units) {
					continue
				}
				// Minerals move in lots or not at all: the per-type floor mineralbatch.go puts under a miner's listing applies
				// here too, since a run too small to deliver a lot only fills the book with handful orders.
				if _, isMineral := mineralListFloors[w.TypeID]; isMineral &&
					units < mineralUnitFloor(w.TypeID, src.Price, 1) {
					continue
				}
				// The spiral brake: see basePriceOf above.
				if base := basePriceOf[w.TypeID]; base > 0 {
					if src.Price > base*specSpiralBaseMul {
						continue
					}
				} else if src.Price > specUnanchoredMaxUnit {
					continue
				}
				// Net of what the destination takes: its sales tax on every unit and the broker fee on the listing (tradefees.go).
				rates := m.marketRates(ctx, b.CharID, dst.StationID)
				profit := specNetProfit(list, src.Price, units, rates)
				if profit < tradeMinProfitISK {
					continue
				}
				if hist.Ready {
					conf = specVelocityConf(conf, hist.stationPerDay(dst.StationID, w.TypeID), units)
				}
				d := &tradeDeal{
					Kind: tradeDealSpec, TypeID: w.TypeID, TypeName: w.TypeName, UnitVol: vol,
					BuyStationID: src.StationID, BuySystemID: src.SystemID,
					SellStationID: dst.StationID, SellSystemID: dst.SystemID,
					Ask: src.Price, Units: units, Outlay: float64(units) * src.Price,
					Profit: profit, ListPrice: list, Confidence: conf,
					Evidence: specEvidence(w, scarce, regionBid[[2]int64{w.TypeID, dst.RegionID}]),
					Jumps:    jumps[src.SystemID] + jumps[dst.SystemID],
				}
				if facWant > 0 {
					d.Evidence = factoryEvidence(facWant, facAlt)
				}
				d.Rank = tradeRank(d)
				deals = append(deals, d)
			}
		}
	}
	if len(deals) == 0 {
		return nil, fmt.Errorf("nothing short anywhere is worth carrying at this pilot's markup")
	}
	sort.Slice(deals, func(i, j int) bool { return deals[i].Rank > deals[j].Rank })
	if len(deals) > specRefineTop {
		deals = deals[:specRefineTop]
	}

	// The winner gets its real buy-to-sell leg measured, as an arbitrage run does: the two ends of a speculative
	// run are usually far from the pilot, so the ranking estimate over-counts the flying by a leg.
	legCache := map[int64]map[int64]int{}
	var best *tradeDeal
	for _, d := range deals {
		legs, ok := legCache[d.BuySystemID]
		if !ok {
			legs, err = m.reachFrom(ctx, d.BuySystemID, r)
			if err != nil {
				return nil, err
			}
			legCache[d.BuySystemID] = legs
		}
		haul, ok := legs[d.SellSystemID]
		if !ok {
			continue
		}
		d.Jumps = jumps[d.BuySystemID] + haul
		d.Rank = tradeRank(d)
		if best == nil || d.Rank > best.Rank {
			best = d
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no shortage it could serve has a legal route from a supplier to the buyer")
	}
	return best, nil
}

// specRefWindowSec is how far back a destination's paid prices count.
const specRefWindowSec = 30 * 86400

// specRefPrices is what each destination region has actually paid for each type: the volume-weighted average of
// its buy transactions over specRefWindowSec, falling back to the cheapest live ask in the region. Keyed
// (typeID, regionID); a pair with neither is absent and the run is priced on the pilot's markup alone. Both halves
// come from roster-wide snapshots, and a failed read returns what it has.
func (m *Mod) specRefPrices(ctx context.Context, dests []specDest, typeIDs []int64) map[[2]int64]float64 {
	out := map[[2]int64]float64{}
	if len(dests) == 0 || len(typeIDs) == 0 {
		return out
	}
	// The market history's clearing price (mktdaily.go) once it covers its
	// window; the 30-day tape scan only until then.
	var paid map[[2]int64]float64
	if h := m.marketHistory(ctx); h.Ready {
		paid = h.Ref
	} else {
		paid, _ = m.allPaidByRegion(ctx)
	}
	asks, _ := m.allAskByRegion(ctx)
	set := map[int64]bool{}
	for _, d := range dests {
		if set[d.RegionID] {
			continue
		}
		set[d.RegionID] = true
		for _, t := range typeIDs {
			k := [2]int64{t, d.RegionID}
			if p := paid[k]; p > 0 {
				out[k] = p
			} else if p := asks[t][d.RegionID]; p > 0 {
				out[k] = p
			}
		}
	}
	return out
}

// specRefTTL is how long the 30-day paid-price table stands. A month's
// average does not move in ten minutes.
const specRefTTL = 10 * time.Minute

// allPaidByRegion is every (type, region)'s volume-weighted buy price over
// specRefWindowSec, for the whole roster in one read. Read-only once shared.
func (m *Mod) allPaidByRegion(ctx context.Context) (map[[2]int64]float64, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshotFor(snapKey("paid-by-region"), specRefTTL, func() (map[[2]int64]float64, error) {
		since := (clock.Unix() - specRefWindowSec + 11644473600) * 10000000
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT typeID, regionID, SUM(price*quantity)/SUM(quantity)
			  FROM market_transactions
			 WHERE transactionType = 1 AND quantity > 0
			   AND transactionDateTime >= ?
			 GROUP BY typeID, regionID`), since)
		if err != nil {
			return nil, fmt.Errorf("paid by region: %w", err)
		}
		defer rows.Close()
		out := map[[2]int64]float64{}
		for rows.Next() {
			var t, r int64
			var p sql.NullFloat64
			if err := rows.Scan(&t, &r, &p); err != nil {
				return nil, err
			}
			if p.Float64 > 0 {
				out[[2]int64{t, r}] = p.Float64
			}
		}
		return out, rows.Err()
	})
}

// specListPrice is what a speculative run lists at: the pilot's markup over
// what the goods cost it, but never above just under what the destination
// region has been paying (ref). ref 0 means nobody there has priced it, and
// the markup stands alone.
func specListPrice(cost, markup, ref float64) float64 {
	list := cost * markup
	if ref > 0 && ref*specRefUndercut < list {
		list = ref * specRefUndercut
	}
	return list
}

// specRefUndercut is how far under the destination's going price a run lists.
const specRefUndercut = 0.99

// specNetProfit is a run's profit after the destination's sales tax on every
// unit and the broker fee on the listing.
func specNetProfit(list, cost float64, units int64, r marketRates) float64 {
	gross := list * float64(units)
	return r.netSale(gross) - r.brokerFee(gross) - cost*float64(units)
}

func specEvidence(w specWant, scarce, bidHere bool) string {
	switch {
	case w.Ledger && scarce:
		return "rolebots have been trying to buy it and the whole region is out"
	case w.Ledger:
		return "rolebots have been trying to buy it"
	case bidHere && scarce:
		return "somebody there is bidding for it and nobody is selling"
	case scarce:
		return "the whole region is out of it"
	default:
		return "the station has none"
	}
}

// specDest is one candidate destination: the best dock of a market system.
type specDest struct {
	StationID int64
	SystemID  int64
	RegionID  int64
	// Factory is set when the station is a factory-demand station (factorydemand.go): what its factories want and
	// would otherwise pay. FactoryOnly marks one added for that alone, not as a liquidity hub.
	Factory     *factoryDest
	FactoryOnly bool
}

// specDestinations picks where goods are worth carrying to: the best market systems this pilot can reach, one
// station each, the best dock. "Best" is measured liquidity discounted by distance (rankMarkets), so a quiet
// market near the pilot outranks a deep one across the map, since the busiest markets are already stocked.
// The pilot's own system is a candidate like any other.
func (m *Mod) specDestinations(ctx context.Context, b *botRun, cfg tradeConfig,
	jumps map[int64]int) []specDest {

	markets, err := m.marketSystems(ctx)
	if err != nil || len(markets) == 0 {
		return m.factorySpecDests(ctx, b, cfg, jumps, nil)
	}
	ids := rankMarkets(markets, m.liquidity(ctx).System, jumps, m.marketBonus(ctx),
		specDestJumpCost, 0, specMaxHubs)
	if len(ids) == 0 {
		return m.factorySpecDests(ctx, b, cfg, jumps, nil)
	}

	stationClause, stationArgs := m.tradeStationClause(ctx, b.Profile, b.Tune, cfg)
	args := append(int64Args(ids), stationArgs...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.solarSystemID, s.regionID, COUNT(o.orderID) AS book
		  FROM staStations s
		  LEFT JOIN market_orders o ON o.stationID = s.stationID
		       AND o.orderState = 1 AND o.volRemaining > 0
		 WHERE s.solarSystemID IN (`+placeholders(len(ids))+`)`+stationClause+`
		 GROUP BY s.stationID, s.solarSystemID, s.regionID`, args...)
	if err != nil {
		rlogf("rolebots: spec destinations: %v", err)
		return m.factorySpecDests(ctx, b, cfg, jumps, nil)
	}
	defer rows.Close()
	var cands []dockCandidate
	region := map[int64]int64{} // station to its region, for the answer
	for rows.Next() {
		var c dockCandidate
		var reg int64
		if err := rows.Scan(&c.StationID, &c.SystemID, &reg, &c.Book); err != nil {
			break
		}
		region[c.StationID] = reg
		cands = append(cands, c)
	}
	// One dock per system, the one the station score picks (amenities, trade and footfall, with the book only breaking
	// ties). Stocking the biggest book in a system stocks where the competition already is.
	best := bestDockPerSystem(m.stationScores(ctx), cands)
	var out []specDest
	for _, sysID := range ids { // ranked order, best market first
		station := best[sysID]
		if station == 0 {
			continue
		}
		out = append(out, specDest{StationID: station, SystemID: sysID,
			RegionID: region[station]})
	}
	// The counters shoppers walked away from short (stationdemand.go): a buyer who found the shelf bare says
	// something about a dock wherever its system ranks on liquidity.
	out = m.shopGapSpecDests(ctx, b, cfg, jumps, out)
	// The factories beside the hubs: a busy factory short of a bill is a
	// buyer standing at a known dock, which is better evidence than footfall.
	return m.factorySpecDests(ctx, b, cfg, jumps, out)
}

// shopGapSpecDests adds up to specShopDests stations in reach where shoppers found the shelf short, most short
// types first, then most units. Stations already listed and ones this pilot would not trade at (tradeStationClause)
// are left out. Off with rolebots.outlet.enabled = 0.
func (m *Mod) shopGapSpecDests(ctx context.Context, b *botRun, cfg tradeConfig,
	jumps map[int64]int, out []specDest) []specDest {

	if !m.outletEnabled(ctx) {
		return out
	}
	have := map[int64]bool{}
	for _, d := range out {
		have[d.StationID] = true
	}
	type gapSite struct {
		dest  specDest
		types int
		units float64
	}
	sites := map[int64]*gapSite{}
	for _, rows := range m.stationDemand(ctx).ByType {
		for _, r := range rows {
			if r.Gap < 1 || have[r.StationID] {
				continue
			}
			if _, ok := jumps[r.SystemID]; !ok {
				continue
			}
			s := sites[r.StationID]
			if s == nil {
				s = &gapSite{dest: specDest{StationID: r.StationID, SystemID: r.SystemID, RegionID: r.RegionID}}
				sites[r.StationID] = s
			}
			s.types++
			s.units += r.Gap
		}
	}
	if len(sites) == 0 {
		return out
	}
	ranked := make([]*gapSite, 0, len(sites))
	for _, s := range sites {
		ranked = append(ranked, s)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].types != ranked[j].types {
			return ranked[i].types > ranked[j].types
		}
		if ranked[i].units != ranked[j].units {
			return ranked[i].units > ranked[j].units
		}
		return ranked[i].dest.StationID < ranked[j].dest.StationID
	})
	if len(ranked) > specShopDests*3 {
		ranked = ranked[:specShopDests*3]
	}
	ids := make([]int64, 0, len(ranked))
	for _, s := range ranked {
		ids = append(ids, s.dest.StationID)
	}
	stationClause, stationArgs := m.tradeStationClause(ctx, b.Profile, b.Tune, cfg)
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID FROM staStations s
		 WHERE s.stationID IN (`+placeholders(len(ids))+`)`+stationClause,
		append(int64Args(ids), stationArgs...)...)
	if err != nil {
		rlogf("rolebots: shop-gap destinations: %v", err)
		return out
	}
	allowed := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			allowed[id] = true
		}
	}
	rows.Close()
	added := 0
	for _, s := range ranked {
		if added >= specShopDests {
			break
		}
		if allowed[s.dest.StationID] {
			out = append(out, s.dest)
			added++
		}
	}
	return out
}

// specSupplyCategories are the goods worth carrying somewhere that has none: modules, ammunition, drones, hulls
// (gated by hold, tradeCargoOK), trade goods and minerals. An allowlist because blueprints and skillbooks dominate
// the book but are not a hauler's goods: industrialists own blueprints and skillbooks sell at a fixed markup.
var specSupplyCategories = []int64{catMaterial, 6, 7, 8, 17, 18}

// specWants is the evidence side: what somebody would actually buy here. Sources, most direct first: the shopping
// ledger (procure.go), standing bids at the destination hub, and supply (goods a well-stocked station sells that this
// one lacks). Supply must stay: without it the search needs a bid or ledger row first and finds almost nothing.
func (m *Mod) specWants(ctx context.Context, dests []specDest, jumps map[int64]int) ([]specWant, error) {
	byType := map[int64]*specWant{}

	rows, err := m.db.QueryContext(ctx, `
		SELECT d.typeID, d.typeName FROM rolebot_market_demand d
		 WHERE d.wanted > 0 AND d.lastAt > ?
		 ORDER BY d.wanted DESC LIMIT ?`,
		clock.Unix()-industryDemandMaxAgeSec, specMaxWantTypes)
	if err != nil {
		return nil, fmt.Errorf("spec wants (ledger): %w", err)
	}
	for rows.Next() {
		var w specWant
		if err := rows.Scan(&w.TypeID, &w.TypeName); err != nil {
			rows.Close()
			return nil, err
		}
		w.Ledger = true
		byType[w.TypeID] = &w
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The mineral shortage ledger (minerals.go) is a fourth source, as direct as the first: a named factory could not
	// cover a bill line. It is per station, so a trader can carry minerals to that dock (factorydemand.go).
	mrows, err := m.db.QueryContext(ctx, `
		SELECT typeID, typeName, stationID, wanted FROM rolebot_mineral_demand
		 WHERE lastAt >= ? AND wanted > 0
		 ORDER BY wanted DESC LIMIT ?`,
		clock.Unix()-mineralDemandTTLSec, specMaxWantTypes*specWantStationsPerType)
	if err != nil {
		return nil, fmt.Errorf("spec wants (minerals): %w", err)
	}
	mineralTypes := 0
	for mrows.Next() {
		var typeID, stationID, want int64
		var name string
		if err := mrows.Scan(&typeID, &name, &stationID, &want); err != nil {
			mrows.Close()
			return nil, err
		}
		w, ok := byType[typeID]
		if !ok {
			if mineralTypes >= specMaxWantTypes {
				continue
			}
			mineralTypes++
			w = &specWant{TypeID: typeID, TypeName: name, Ledger: true}
			byType[typeID] = w
		}
		if w.Stations == nil {
			w.Stations = map[int64]int64{}
		}
		w.Stations[stationID] += want
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	// What the factory destinations want, by name: the goods worth carrying there, and why those destinations are listed.
	var factoryTypes []int64
	for _, d := range dests {
		if d.Factory == nil {
			continue
		}
		for typeID, units := range d.Factory.Want {
			w, ok := byType[typeID]
			if !ok {
				w = &specWant{TypeID: typeID, Ledger: true}
				byType[typeID] = w
				factoryTypes = append(factoryTypes, typeID)
			}
			if w.Stations == nil {
				w.Stations = map[int64]int64{}
			}
			if units > w.Stations[d.StationID] {
				w.Stations[d.StationID] = units
			}
		}
	}
	if len(factoryTypes) > 0 {
		if nrows, nerr := m.db.QueryContext(ctx, `
			SELECT typeID, typeName FROM invTypes WHERE typeID IN (`+placeholders(len(factoryTypes))+`)`,
			int64Args(factoryTypes)...); nerr == nil {
			for nrows.Next() {
				var id int64
				var name string
				if nrows.Scan(&id, &name) == nil {
					byType[id].TypeName = name
				}
			}
			nrows.Close()
		}
	}

	stations := make([]int64, 0, len(dests))
	for _, d := range dests {
		stations = append(stations, d.StationID)
	}
	brows, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, t.typeName, SUM(o.price * o.volRemaining) AS want
		  FROM market_orders o
		  JOIN invTypes t ON t.typeID = o.typeID
		 WHERE o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		   AND o.stationID IN (`+placeholders(len(stations))+`)
		 GROUP BY o.typeID, t.typeName
		 ORDER BY want DESC LIMIT ?`,
		append(int64Args(stations), specMaxWantTypes)...)
	if err != nil {
		return nil, fmt.Errorf("spec wants (bids): %w", err)
	}
	defer brows.Close()
	for brows.Next() {
		var id int64
		var name string
		var want float64
		if err := brows.Scan(&id, &name, &want); err != nil {
			return nil, err
		}
		if _, ok := byType[id]; !ok {
			byType[id] = &specWant{TypeID: id, TypeName: name}
		}
	}
	if err := brows.Err(); err != nil {
		return nil, err
	}

	// The supply side: the catalogue of a well-stocked station the pilot could fly to. Whether any of it is missing
	// at a destination is decided later by specMarketPresence.
	supply, err := m.specSupplyCatalogue(ctx, jumps)
	if err != nil {
		return nil, err
	}
	for _, w := range supply {
		if len(byType) >= specWantTypeCap {
			break
		}
		if _, ok := byType[w.TypeID]; !ok {
			c := w
			byType[w.TypeID] = &c
		}
	}

	out := make([]specWant, 0, len(byType))
	for _, w := range byType {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TypeID < out[j].TypeID })
	return out, nil
}

// stockedStation is one of the game's better-supplied docks.
type stockedStation struct {
	StationID int64
	SystemID  int64
	Types     int64
}

var (
	stockedMu    sync.Mutex
	stockedAt    time.Time
	stockedCache []stockedStation
)

// stockedStationTTL: which stations carry a broad catalogue is a fact about the seeded NPC market, not any pilot,
// so every trader shares one answer, refreshed on the market-dynamics tick.
const stockedStationTTL = 5 * time.Minute

// stockedStations ranks the game's best-supplied stations by how many distinct types they offer, counting only the
// categories a trader would carry (specSupplyCategories). Pilot-independent and cached. The category filter matters:
// ranked by raw type count the top is the skillbook and blueprint depots, which would leave the catalogue near empty.
func (m *Mod) stockedStations(ctx context.Context) ([]stockedStation, error) {
	stockedMu.Lock()
	if stockedCache != nil && clock.Now().Sub(stockedAt) < stockedStationTTL {
		out := stockedCache
		stockedMu.Unlock()
		return out, nil
	}
	stockedMu.Unlock()

	args := append(int64Args(specSupplyCategories), specStockedStationPool)
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.solarSystemID, COUNT(DISTINCT o.typeID) AS types
		  FROM market_orders o
		  JOIN invTypes t ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		   AND g.categoryID IN (`+placeholders(len(specSupplyCategories))+`)
		 GROUP BY o.stationID, o.solarSystemID
		 ORDER BY types DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("stocked stations: %w", err)
	}
	defer rows.Close()
	var out []stockedStation
	for rows.Next() {
		var s stockedStation
		if err := rows.Scan(&s.StationID, &s.SystemID, &s.Types); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	stockedMu.Lock()
	stockedCache, stockedAt = out, clock.Now()
	stockedMu.Unlock()
	return out, nil
}

// specSupplyCatalogue is what the pilot could go and buy: the goods on offer at the best-stocked stations it can
// reach, breadth discounted by distance the way a destination's score is.
func (m *Mod) specSupplyCatalogue(ctx context.Context, jumps map[int64]int) ([]specWant, error) {
	all, err := m.stockedStations(ctx)
	if err != nil {
		return nil, err
	}
	type ranked struct {
		station int64
		weight  float64
	}
	var reachable []ranked
	for _, s := range all {
		hops, ok := jumps[s.SystemID]
		if !ok {
			continue
		}
		reachable = append(reachable, ranked{s.StationID,
			float64(s.Types) / (1 + specDestJumpCost*float64(hops))})
	}
	if len(reachable) == 0 {
		return nil, nil
	}
	sort.Slice(reachable, func(i, j int) bool { return reachable[i].weight > reachable[j].weight })
	if len(reachable) > specSupplyStations {
		reachable = reachable[:specSupplyStations]
	}
	ids := make([]int64, 0, len(reachable))
	for _, r := range reachable {
		ids = append(ids, r.station)
	}

	args := append(int64Args(ids), int64Args(specSupplyCategories)...)
	args = append(args, specWantTypeCap)
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, t.typeName, SUM(o.price * o.volRemaining) AS onSale
		  FROM market_orders o
		  JOIN invTypes t ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		   AND o.stationID IN (`+placeholders(len(ids))+`)
		   AND g.categoryID IN (`+placeholders(len(specSupplyCategories))+`)
		 GROUP BY o.typeID, t.typeName
		 ORDER BY onSale DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("spec supply catalogue: %w", err)
	}
	defer rows.Close()
	var out []specWant
	for rows.Next() {
		var w specWant
		var onSale float64
		if err := rows.Scan(&w.TypeID, &w.TypeName, &onSale); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// specSources finds the cheapest few places this pilot could buy each
// wanted type, under its own travel and station rules.
func (m *Mod) specSources(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules,
	typeIDs []int64) (map[int64][]specSource, error) {
	return m.specSourcesIn(ctx, b, cfg, r, typeIDs, nil)
}

// specSourcesIn is specSources inside the given regions (nil = everywhere), so
// the cheapest few it keeps are ones a short run can actually reach.
func (m *Mod) specSourcesIn(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules,
	typeIDs, regions []int64) (map[int64][]specSource, error) {

	if len(typeIDs) == 0 {
		return nil, nil
	}
	stationClause, stationArgs := m.tradeStationClause(ctx, b.Profile, b.Tune, cfg)
	reachClause := ` AND ROUND(ms.security, 1) >= ?`
	reachArgs := []any{r.SecFloor}
	if len(regions) > 0 {
		reachClause += ` AND ms.regionID IN (` + placeholders(len(regions)) + `)`
		reachArgs = append(reachArgs, int64Args(regions)...)
	}

	query := `
		SELECT typeID, stationID, solarSystemID, regionID, price, vol, unitVol, categoryID FROM (
			SELECT o.typeID, o.stationID, o.solarSystemID, o.regionID,
			       MIN(o.price) AS price, SUM(o.volRemaining) AS vol,
			       COALESCE(t.volume, 0.01) AS unitVol, COALESCE(g.categoryID, 0) AS categoryID,
			       ROW_NUMBER() OVER (PARTITION BY o.typeID ORDER BY MIN(o.price) ASC) AS rn
			  FROM market_orders o
			  JOIN invTypes t ON t.typeID = o.typeID
			  LEFT JOIN invGroups g ON g.groupID = t.groupID
			  JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
			  JOIN staStations s ON s.stationID = o.stationID
			 WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
			   AND o.charID <> ?
			   AND o.typeID IN (` + placeholders(len(typeIDs)) + `)` +
		reachClause + stationClause + `
			 GROUP BY o.typeID, o.stationID, o.solarSystemID, o.regionID, t.volume, g.categoryID
		) src WHERE rn <= ` + fmt.Sprint(specSourcesPerType)

	args := []any{b.CharID}
	args = append(args, int64Args(typeIDs)...)
	args = append(args, reachArgs...)
	args = append(args, stationArgs...)

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("spec sources: %w", err)
	}
	defer rows.Close()
	out := map[int64][]specSource{}
	for rows.Next() {
		var typeID int64
		var s specSource
		if err := rows.Scan(&typeID, &s.StationID, &s.SystemID, &s.RegionID, &s.Price, &s.Stock,
			&s.UnitVol, &s.CategoryID); err != nil {
			return nil, err
		}
		if s.Price <= 0 || s.Stock <= 0 {
			continue
		}
		out[typeID] = append(out[typeID], s)
	}
	return out, rows.Err()
}

// myOrderStations is where this pilot already has an order out for each of these types, keyed by both station and
// system: two stations in one system are one place to a buyer standing in it. Read from market_orders so an order
// placed by any path counts.
func (m *Mod) myOrderStations(ctx context.Context, charID int64, typeIDs []int64) (map[[2]int64]bool, error) {
	out := map[[2]int64]bool{}
	if len(typeIDs) == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT typeID, stationID, solarSystemID FROM market_orders
		 WHERE charID = ? AND orderState = 1 AND volRemaining > 0
		   AND typeID IN (`+placeholders(len(typeIDs))+`)`,
		append([]any{charID}, int64Args(typeIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("own orders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t, s, sys int64
		if err := rows.Scan(&t, &s, &sys); err != nil {
			return nil, err
		}
		out[[2]int64{t, s}] = true
		out[[2]int64{t, sys}] = true
	}
	return out, rows.Err()
}

// specDock is what already rests on a destination's sell book for one type,
// the asking pilot's own orders left out: whether an NPC seeder stocks it,
// and how many units other sellers have up.
type specDock struct {
	Seeded bool
	Depth  int64
}

// specMarketPresence answers the three questions the confidence is built
// from, in one pass each: what is already sold AT the destination station
// (and by whom), is it sold anywhere in that region, and is anyone in that
// region bidding.
func (m *Mod) specMarketPresence(ctx context.Context, charID int64, dests []specDest, typeIDs []int64) (
	haveAsk map[[2]int64]specDock, regionAsk map[[2]int64]bool, regionBid map[[2]int64]bool, err error) {

	haveAsk, regionAsk, regionBid = map[[2]int64]specDock{}, map[[2]int64]bool{}, map[[2]int64]bool{}
	if len(dests) == 0 || len(typeIDs) == 0 {
		return
	}
	stations := make([]int64, 0, len(dests))
	regionSet := map[int64]bool{}
	for _, d := range dests {
		stations = append(stations, d.StationID)
		regionSet[d.RegionID] = true
	}
	regions := make([]int64, 0, len(regionSet))
	for id := range regionSet {
		regions = append(regions, id)
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, o.stationID,
		       MAX(o.charID IN (`+liquidityExcluded+`)) AS seeded,
		       COALESCE(SUM(CASE WHEN o.charID IN (`+liquidityExcluded+`) THEN 0
		                         ELSE o.volRemaining END), 0) AS depth
		  FROM market_orders o
		 WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0 AND o.charID <> ?
		   AND o.stationID IN (`+placeholders(len(stations))+`)
		   AND o.typeID IN (`+placeholders(len(typeIDs))+`)
		 GROUP BY o.typeID, o.stationID`,
		append(append([]any{charID}, int64Args(stations)...), int64Args(typeIDs)...)...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("spec presence (station asks): %w", err)
	}
	for rows.Next() {
		var t, s int64
		var dock specDock
		if err := rows.Scan(&t, &s, &dock.Seeded, &dock.Depth); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		haveAsk[[2]int64{t, s}] = dock
	}
	rows.Close()

	for _, side := range []struct {
		bid int
		out map[[2]int64]bool
	}{{0, regionAsk}, {1, regionBid}} {
		r, err := m.db.QueryContext(ctx, `
			SELECT DISTINCT o.typeID, o.regionID FROM market_orders o
			 WHERE o.bid = ? AND o.orderState = 1 AND o.volRemaining > 0
			   AND o.regionID IN (`+placeholders(len(regions))+`)
			   AND o.typeID IN (`+placeholders(len(typeIDs))+`)`,
			append(append([]any{side.bid}, int64Args(regions)...), int64Args(typeIDs)...)...)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("spec presence (region): %w", err)
		}
		for r.Next() {
			var t, g int64
			if err := r.Scan(&t, &g); err != nil {
				r.Close()
				return nil, nil, nil, err
			}
			side.out[[2]int64{t, g}] = true
		}
		r.Close()
	}
	return haveAsk, regionAsk, regionBid, nil
}
