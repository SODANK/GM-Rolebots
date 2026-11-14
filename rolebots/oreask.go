package rolebots

// Ore rested on the market rather than fed to whatever bid is standing.
//
// A solvent miner prices its hold between two numbers and leaves it on the
// book: the floor (the best bid here plus a margin; below it, take the bid) and
// the ceiling (what a reference refiner could pay and still profit). Ore
// basePrice is 0.75 x refined value and the NPC bid is security-tiered
// (marketsec.go), so highsec has a spread and in nullsec the ceiling comes out
// under the floor: oreAsk returns 0 and the caller sells to the bid, with no
// rule naming a region. A listing is not a sale: noteOreFlow books it under
// oreStageListed, and the fill is booked again under sold_bot or sold_npc.

import (
	"context"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

const (
	// oreSellDays is how long an ore listing rests: half mineralSellDays (30). A
	// rock's only customer is a refiner near its belt, so an ask unsold in a
	// fortnight is at the wrong station and the order slot is better spent.
	oreSellDays = 14

	// oreAskMargin is what a miner asks over the best local bid. Wider than
	// mineralAskMargin (1.05) because the incumbent is an NPC bid, a floor that
	// never reprices against the listing.
	oreAskMargin = 1.08

	// oreAskMaxOverBase is the ceiling in multiples of invTypes.basePrice, tighter
	// than mineralAskMaxOverBase (4.0): an ore's basePrice is 0.75 x the refined
	// value, so 1.6x base is already past what any refiner can pay.
	oreAskMaxOverBase = 1.6

	// oreListMinValue is the litter line, as mineralListMinValue: an order slot is
	// a scarce shared resource (freeOrderSlots covers asks, bids and trade
	// listings together) and a handful of Veldspar is not worth one.
	oreListMinValue = 20_000.0

	// oreAskRefinerEdgeDefault is the margin an ore ask leaves for whoever buys it:
	// the ceiling is the reference refiner's break-even divided by this, so at 1.10
	// a refiner clears about 10%. Pricing at break-even gives nobody a reason to act.
	oreAskRefinerEdgeDefault = 1.10

	// oreAskNoRivalAnchor is where in the band an ask lands when there is no rival
	// ask: 0 is the floor, 1 the ceiling. The book is almost always empty (nothing
	// seeds an ore ask), so this sits low and the refiner keeps most of the spread.
	oreAskNoRivalAnchor = 0.25
)

// oreAskPrice is the whole pricing rule, pure: floor, ceiling, and the refusal
// when the standing bid already beats what a refiner could pay. Returns a zero
// price and a reason when the rock should not be listed.
func oreAskPrice(bestBid, refinedValue, edge, basePrice, booked float64, thin bool) (float64, string) {
	why := ""
	if thin && booked > 0 {
		why = "the book here is thinner than this hold — asking over it"
	}
	return oreAskPriceP(bestBid, refinedValue, edge, basePrice, booked, why, oreAskNoRivalAnchor, 1)
}

// The per-pilot band. Where in the band a pilot sits, and how far over the bid
// its floor is, follow the same greed axis as every other margin (personalT),
// so one dock's asks do not all land on one price.
const (
	oreAskAnchorLo   = 0.10 // generous: near the floor, priced to go
	oreAskAnchorHi   = 0.50 // greedy: half way to what a refiner could bear
	oreAskFloorMulLo = 0.95 // × the floor: bid × 1.03, or 0.95 × base
	oreAskFloorMulHi = 1.05
)

// oreAskFloorMul is this pilot's multiplier on oreAskFloor.
func oreAskFloorMul(tn Tuning) float64 {
	return oreAskFloorMulLo + personalT(tn)*(oreAskFloorMulHi-oreAskFloorMulLo)
}

// oreAskFloor is the price under which this pilot would rather take the bid.
// With no bid standing it is basePrice, which sql/ore_prices.sql derived as
// 0.75 × the intrinsic value of what the rock refines into.
func oreAskFloor(bestBid, basePrice, floorMul float64) float64 {
	if floorMul <= 0 {
		floorMul = 1
	}
	floor := bestBid * oreAskMargin
	if floor <= 0 {
		floor = basePrice
	}
	return floor * floorMul
}

// oreAskPriceP is oreAskPrice with the pilot's own band: anchor is where an
// uncontested ask sits (0 floor, 1 ceiling), floorMul scales the floor, and
// bookWhy is askAgainstBook's reason for booked.
func oreAskPriceP(bestBid, refinedValue, edge, basePrice, booked float64, bookWhy string, anchor, floorMul float64) (float64, string) {
	if refinedValue <= 0 {
		return 0, "nothing prices what this rock refines into"
	}
	floor := oreAskFloor(bestBid, basePrice, floorMul)
	if edge <= 0 {
		edge = oreAskRefinerEdgeDefault
	}
	ceiling := refinedValue / edge

	// The one refusal: an NPC bid already paying more than a refiner could leaves
	// no room for a listing (the nullsec case, from arithmetic not region rules).
	if ceiling <= floor {
		if bestBid <= 0 {
			// No bid at all: the floor is the base price, so say that rather than a
			// bid of 0.00.
			return 0, fmt.Sprintf(
				"nobody bids on it here, and its base price of %.2f is already over the %.2f a refiner could pay for it",
				floor, ceiling)
		}
		return 0, fmt.Sprintf(
			"the standing bid pays %.2f, already over the %.2f a refiner could pay for it",
			bestBid, ceiling)
	}

	price, why := booked, bookWhy
	if why == "" {
		why = "priced against the local book"
	}
	if price <= 0 {
		// No rival ask: sit low in the band (see oreAskNoRivalAnchor), since a rock
		// priced at what a refiner could just bear sits unsold.
		price = floor + (ceiling-floor)*anchor
		why = "nobody else is asking here — priced to be taken, not to be admired"
	}
	if price > ceiling {
		price, why = ceiling, "held at what a refiner could still pay for it"
	}
	if price < floor {
		price, why = floor, "held just over the standing bid"
	}
	// The same authored anchor mineralAsk clamps against: inputs above are
	// market-referential, so a book left to quote itself drifts without limit.
	if basePrice > 0 {
		if cap := basePrice * oreAskMaxOverBase; price > cap {
			price = cap
			why = fmt.Sprintf("clamped at %.1fx base — the local book has drifted past any real scarcity",
				oreAskMaxOverBase)
		}
	}
	// The clamp must never carry an ask under the standing bid: that listing would
	// be bought and sold straight into the bid. Refuse, and the caller sells to it.
	if bestBid > 0 && price < floor {
		return 0, fmt.Sprintf(
			"the standing bid pays %.2f, already over the %.2f this rock may be listed at — taking the bid",
			bestBid, price)
	}
	if price < 0.01 {
		return 0, "the rock is not worth a hundredth of an ISK"
	}
	return math.Round(price*100) / 100, why
}

// oreAsk prices one ore stack for the book at this station.
//
// Returns (0, why, nil) when the rock should not be listed at all. The caller
// dumps to the bid instead; that is not a failure, it is the answer.
func (m *Mod) oreAsk(ctx context.Context, b *botRun, stationID, typeID, qty int64) (float64, string, error) {
	return m.oreAskIn(ctx, b, m.valuationRegions(ctx, b), stationID, typeID, qty)
}

// oreListCeiling is the most an ore listing may ask, whatever the book: the
// refiner's ceiling, held under the base-price clamp. Pure. The corporation
// buyback bounds its resale bid by it (ceoMiningBuyback), so a corporation
// never pays more for a rock than it is allowed to list it at again.
func oreListCeiling(refinedValue, edge, basePrice float64) float64 {
	if refinedValue <= 0 {
		return 0
	}
	if edge <= 0 {
		edge = oreAskRefinerEdgeDefault
	}
	ceiling := refinedValue / edge
	if basePrice > 0 {
		ceiling = math.Min(ceiling, basePrice*oreAskMaxOverBase)
	}
	return ceiling
}

// oreAskIn is oreAsk valued in the given regions rather than the pilot's own.
// A corporation lists at an office, not where its CEO stands, and its buyback
// values the same rock at that office's region; both halves of a trade read one
// mineral book or a bid ends up above the ask it feeds.
func (m *Mod) oreAskIn(ctx context.Context, b *botRun, regions []int64, stationID, typeID, qty int64) (float64, string, error) {
	return m.oreAskInFor(ctx, b, regions, stationID, typeID, qty, false)
}

// oreAskFactoryAnchor is where in the band an uncontested ask sits at a
// factory station that wants this rock's minerals: the buyer is standing
// right there, and the ceiling is what its alternative leaves (oreAskInFor).
const oreAskFactoryAnchor = 0.5

// oreAskInFor is oreAskIn, optionally priced for the factory at the station:
// when a factory there is short of what the rock refines into
// (factorydemand.go), the ceiling is read at the factory's alternative for
// those minerals. Still floored by the local bid and clamped at base.
func (m *Mod) oreAskInFor(ctx context.Context, b *botRun, regions []int64, stationID, typeID, qty int64,
	forFactory bool) (float64, string, error) {
	vals, err := m.oreValues(ctx, regions, oreValueBid)
	if err != nil {
		return 0, "", err
	}
	v := vals[typeID]

	// The floor's input. The seeder is not excluded here, unlike mineralAsk: the
	// seeder's mineral bid is a fabricated anchor, but the NPC ore bid is the real
	// competition (db/marketimpact.go) and the number this miner declines to take.
	var bid float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(price), 0) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`,
		stationID, typeID).Scan(&bid)

	fallback := 0.0
	if bid <= 0 {
		if est, eerr := m.estimatePriceFor(ctx, b, typeID); eerr == nil {
			fallback = est
		}
	}

	// What the book here would have this pilot ask, before the ore floor and
	// ceiling. askAgainstBook is the shared rule (undercut a wall, overcut a thin book).
	levels, lerr := m.localAskBook(ctx, b.CharID, stationID, typeID)
	if lerr != nil {
		levels = nil
	}

	// The anchor both clamps read. stableFloorPrice is basePrice x 0.6, so
	// dividing it back out recovers the authored number itself.
	base := 0.0
	if sf := m.stableFloorPrice(ctx, typeID); sf > 0 {
		base = sf / askFloorFraction
	}
	if fallback > 0 && base <= 0 {
		base = fallback
	}

	// This pilot's band (oreAskAnchorLo..Hi, oreAskFloorMulLo..Hi).
	anchor := oreAskAnchorLo + personalT(b.Tune)*(oreAskAnchorHi-oreAskAnchorLo)
	floorMul := oreAskFloorMul(b.Tune)
	refined, edge := oreRefinedValue(v, oreReferenceYield()), oreAskRefinerEdge()
	factory := false
	if forFactory {
		if fv := m.oreFactoryValueAt(ctx, b, stationID, v); fv > refined {
			refined, factory = fv, true
			anchor = math.Max(anchor, oreAskFactoryAnchor)
		}
	}
	feed := func(price float64, why string) (float64, string) {
		if factory && price > 0 {
			why = "a factory here is short of what it refines into — " + why
		}
		return price, why
	}

	// The valuation: what this pilot would ask with the counter to itself.
	own, why := oreAskPriceP(bid, refined, edge, base, 0, "", anchor, floorMul)
	if own <= 0 || len(levels) == 0 {
		own, why = feed(own, why)
		return own, why, nil
	}

	// And against the book actually here. askAgainstBook answers 0 for a zero
	// fallback before reading any level, so it is given the valuation as fallback.
	booked, bookWhy := askAgainstBook(levels, qty, own,
		oreAskFloor(bid, base, floorMul), mineralAskSteps(b.Tune.MineralMarkup))
	price, why := oreAskPriceP(bid, refined, edge, base, booked, bookWhy, anchor, floorMul)
	price, why = feed(price, why)
	return price, why, nil
}

// listOreStack rests one ore type from the hangar on this station's book.
// Reports (0, 0, nil) when a listing is not the right answer (nothing in the
// hangar, no price worth asking, no order slot, too little value). The caller
// then falls through to sellOreStack: a miner that cannot list must still sell.
func (m *Mod) listOreStack(ctx context.Context, b *botRun, typeID int64) (int64, float64, error) {
	var itemID, qty int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
		ORDER BY quantity DESC LIMIT 1`,
		b.CharID, b.StationID, typeID).Scan(&itemID, &qty); err != nil || qty <= 0 {
		return 0, 0, nil
	}
	return m.listOreStackFrom(ctx, b, typeID, itemID, qty)
}

// listOreStackFrom is listOreStack for one named stack and quantity: the
// fleet-share disposal (fleetore.go) lists a miner's handed-back units and
// not the stockpile they may be sitting beside, so it splits the share off
// first and rests exactly that. Same refusals, same fall-through.
func (m *Mod) listOreStackFrom(ctx context.Context, b *botRun, typeID, itemID, qty int64) (int64, float64, error) {
	if qty <= 0 {
		return 0, 0, nil
	}
	name := m.typeNameOr(ctx, typeID)

	// A standing order of this rock on this dock absorbs the hold rather than
	// gaining a twin: there is no quantity-modify on the wire (mineralbatch.go).
	if topped, terr := m.topUpOwnSellOrder(ctx, b, typeID, itemID, qty); terr != nil {
		m.logBot(b.CharID, logError, "market", "topping up its %s order: %v — listing instead", name, terr)
	} else if topped {
		m.noteOreFlow(ctx, typeID, oreStageListed, qty, 0)
		return qty, 0, nil
	}

	// Priced for the factory when one here is short of what the rock refines into
	// (factorydemand.go): its alternative is what the rock is worth to industrials.
	price, why, err := m.oreAskInFor(ctx, b, m.valuationRegions(ctx, b), b.StationID, typeID, qty, true)
	if err != nil {
		return 0, 0, err
	}
	if price <= 0 {
		m.logBot(b.CharID, logInfo, "market", "not listing %d× %s — %s", qty, name, why)
		return 0, 0, nil
	}
	// A pilot short of cash lists at a lower floor (listfee.go): the shipped one
	// leaves a poor miner's whole load in the hangar for want of ~15k ISK more.
	if floor := listFloor(m.isHurting(ctx, b), oreListMinValue); price*float64(qty) < floor {
		m.logBot(b.CharID, logInfo, "market",
			"%d× %s is worth %.0f ISK — under the %.0f an order slot is worth, selling it instead",
			qty, name, price*float64(qty), floor)
		return 0, 0, nil
	}
	if m.freeOrderSlots(ctx, b.CharID) <= 0 {
		m.logBot(b.CharID, logInfo, "market",
			"no order slot free for %d× %s — selling into the bid instead", qty, name)
		return 0, 0, nil
	}
	// The trade gate (mktdaily.go): nobody here has lifted an ask for it in a
	// fortnight, so the ordinary sale takes it. A short factory is its own evidence.
	if m.factoryAskCap(ctx, b.StationID, typeID) <= 0 && m.regionBoughtNone(ctx, b.StationID, typeID) {
		m.logBot(b.CharID, logInfo, "market",
			"nobody in this region has bought %s off the book in two weeks — selling into the bid instead", name)
		return 0, 0, nil
	}

	// The fee is checked before the door is tried (listfee.go).
	var distressGross float64
	if blocked, sold := m.brokerFeeBlocks(ctx, b, price*float64(qty), func() int64 {
		s, g := m.distressSell(ctx, b, b.StationID, typeID, itemID, qty)
		distressGross = g
		return s
	}); blocked {
		if sold > 0 {
			m.noteOreFlow(ctx, typeID, oreStageSoldNPC, sold, distressGross)
			return sold, distressGross, nil
		}
		return 0, 0, nil
	}
	out, serr := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
		typeID, itemID, price, qty, -1 /*station*/, 1, oreSellDays, false)
	if serr != nil {
		return 0, 0, fmt.Errorf("list ore: %w", serr)
	}
	if out.ErrorMsg != "" {
		// A broker fee this pilot cannot pay is the illiquidity deadlock, not a market
		// condition (distress.go). Anything else falls through to the ordinary sale.
		if brokerFeeRefusal(out.ErrorMsg) {
			if sold, gross := m.distressSell(ctx, b, b.StationID, typeID, itemID, qty); sold > 0 {
				m.noteOreFlow(ctx, typeID, oreStageSoldNPC, sold, gross)
				return sold, gross, nil
			}
		}
		m.logBot(b.CharID, logWarn, "market",
			"ore listing refused (%s) — leaving it to the ordinary sale", out.ErrorMsg)
		return 0, 0, nil
	}

	// A listing priced at or under a standing bid clears against it on the spot.
	// Split the fill as sellOreStack does: an NPC bid is a sink, a player bid is a
	// refiner.
	var gross, npcISK, botISK float64
	var npcQty, botQty int64
	for _, f := range out.Fills {
		val := f.Price * float64(f.Quantity)
		gross += val
		if f.IsNPCOrder {
			npcQty, npcISK = npcQty+f.Quantity, npcISK+val
		} else {
			botQty, botISK = botQty+f.Quantity, botISK+val
		}
	}
	m.noteOreFlow(ctx, typeID, oreStageSoldNPC, npcQty, npcISK)
	m.noteOreFlow(ctx, typeID, oreStageSoldBot, botQty, botISK)
	m.notePeerFills(ctx, tradeEntry{
		CharID: b.CharID, TypeID: typeID, TypeName: name,
		Station: b.StationID, System: b.SystemID,
	}, out.Fills, false, "ore")

	resting := qty - out.FilledQty
	if resting > 0 && out.RestingOrderID != 0 {
		m.bookBrokerFee(ctx, b, out.RestingOrderID, typeID, name, price, resting)
		// Registering the order lets the sweep book its fills and reprice it, and feeds
		// bidCandidateTypes (tradebids.go), which reads rolebot_trades. unitCost is 0: mined.
		m.registerOrder(ctx, b.CharID, out.RestingOrderID, typeID, b.StationID, b.SystemID,
			name, false, price, 0, resting, tradeDealStock)
		m.bookTrade(ctx, tradeEntry{
			CharID: b.CharID, Kind: tradeKindList, TypeID: typeID, TypeName: name,
			Qty: resting, Price: price, Value: price * float64(resting),
			Station: b.StationID, System: b.SystemID, OrderID: out.RestingOrderID,
			Note: fmt.Sprintf("ore rested on the book — %s", why),
		})
		m.noteOreFlow(ctx, typeID, oreStageListed, resting, price*float64(resting))
		m.logBot(b.CharID, logInfo, "market", "listed %d× %s at %.2f ISK — %s",
			resting, name, price, why)
	}
	return out.FilledQty + resting, gross, nil
}
