package rolebots

// The bid book for modules and ammunition.
//
// Nothing bids on categories 7 and 8, so goods stuck in a hangar can only exit
// through an ask, which costs a broker fee that stuck holders cannot pay.
// tradebids.go bidOutlets cannot start a book because it prices off a bid that
// somebody already pays.
//
// The outlet here is the demand ledger rolebot_market_demand: each row was written
// by a pilot that tried to buy the type and could not (typeIsWanted, strays.go).
// The price is lootAskPrice times gearBidDiscount. The bid lands where the seller is.

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

const (
	// gearBidDiscount is the fraction of the going rate a gear bid offers. It is
	// steeper than the trader's ordinary margin because the resale is a listing that
	// must be placed, carried and waited on.
	gearBidDiscount = 0.45

	// gearBidDemandAgeSec is how stale a demand row may be and still count: a
	// fortnight, so a type wanted all week does not drop out between passes.
	gearBidDemandAgeSec = 14 * 86400

	// gearBidMinWanted is the eligibility bar on the cumulative counter. Below it the
	// ledger records one pilot's bad afternoon, not a shortage.
	gearBidMinWanted = 500

	// gearBidMaxValue caps one gear bid. Ammunition is cheap, so units are the
	// natural denomination of a mistake.
	gearBidMaxValue = 250_000.0

	// gearBidMinValueFrac is the least share of the goods' worth (evedb.TypeValueOf)
	// a gear bid may offer. The discount applies to the pilot's regional ask, which in
	// a dumped region can be a small fraction of that worth; a bid that low is a fire sale.
	gearBidMinValueFrac = 0.25

	// gearBidMinUnitISK is the absolute floor per unit.
	gearBidMinUnitISK = 1.0
)

// gearBidSane is the floor on a priced gear bid, pure. An unknown value (0)
// does not condemn a bid, matching bidUnderValue.
func gearBidSane(price, value float64) bool {
	if price < gearBidMinUnitISK {
		return false
	}
	return value <= 0 || price >= gearBidMinValueFrac*value
}

// isGearCategory reports the two categories with no bid book at all. Ships (6)
// are excluded: a hull is a singleton bought by procurement, and its demand rows
// belong to the fit generator.
func isGearCategory(categoryID int64) bool {
	return categoryID == 7 || categoryID == 8
}

// gearDemand is one type the roster has recorded itself unable to buy.
type gearDemand struct {
	Wanted   int64
	SystemID int64
}

// gearBidPrice is the whole pricing rule, pure. It returns 0 when the type should
// not be bid for. The local book constrains the answer: a bid at or over the local
// ask is a purchase for the arbitrage planner, and a bid under the standing local
// bid wins nothing.
func gearBidPrice(going, localAsk, localBid float64) float64 {
	if going <= 0 {
		return 0
	}
	price := going * gearBidDiscount
	if localAsk > 0 && price >= localAsk*bidUnderAsk {
		price = localAsk * bidUnderAsk
	}
	if localBid > 0 && price <= localBid {
		return 0 // somebody here already pays this or better
	}
	if price < 0.01 {
		return 0
	}
	return roundISK(price)
}

// gearBidUnits is how many units to bid for, from the money and the hold.
// `wanted` is not consulted: a never-decremented running total would size every
// ammunition bid at tens of millions of rounds.
func gearBidUnits(price, budget, hold, unitVol float64) int64 {
	if price <= 0 || budget <= 0 {
		return 0
	}
	if budget > gearBidMaxValue {
		budget = gearBidMaxValue
	}
	units := int64(budget / price)
	if unitVol > 0 {
		if byHold := int64(hold / unitVol); byHold < units {
			units = byHold
		}
	}
	if units < 1 {
		return 0
	}
	return units
}

// gearDemandFor reads the demand ledger for the candidate types that are gear. It
// is scoped to the types in hand because a type nobody sells here is not worth a
// bid, however wanted elsewhere.
func (m *Mod) gearDemandFor(ctx context.Context, types []bidType) (map[int64]gearDemand, error) {
	ids := make([]int64, 0, len(types))
	for _, t := range types {
		if isGearCategory(t.CategoryID) {
			ids = append(ids, t.TypeID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]any{}, int64Args(ids)...)
	args = append(args, clock.Unix()-gearBidDemandAgeSec, int64(gearBidMinWanted))

	w := demandWantedSQL("", clock.Unix())
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, `+w+`, lastSystemID
		  FROM rolebot_market_demand
		 WHERE typeID IN (`+placeholders(len(ids))+`)
		   AND lastAt >= ? AND `+w+` >= ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("gear demand: %w", err)
	}
	defer rows.Close()
	out := map[int64]gearDemand{}
	for rows.Next() {
		var typeID int64
		var d gearDemand
		if err := rows.Scan(&typeID, &d.Wanted, &d.SystemID); err != nil {
			return nil, fmt.Errorf("gear demand scan: %w", err)
		}
		out[typeID] = d
	}
	return out, rows.Err()
}

// planGearBid is the fallback arm of the standing-bid pass: the best gear bid
// this pilot could leave here for types with no market outlet. It returns nil
// when there is nothing to bid for. It ranks on demand, not modelled profit,
// since there is no outlet to compute a spread against.
func (m *Mod) planGearBid(ctx context.Context, b *botRun, types []bidType,
	local map[int64]localQuote, outlets map[int64][]bidOutlet,
	budget, hold float64) (*bidPlan, error) {

	demand, err := m.gearDemandFor(ctx, types)
	if err != nil || len(demand) == 0 {
		return nil, err
	}

	var best *bidPlan
	var bestWanted int64
	for _, t := range types {
		d, ok := demand[t.TypeID]
		if !ok {
			continue
		}
		// A type with a real outlet belongs to tradebids.go's arm, which prices off a bid
		// somebody already pays. This arm is for the types that arm cannot see.
		if len(outlets[t.TypeID]) > 0 {
			continue
		}
		lb := local[t.TypeID]
		price := gearBidPrice(m.lootAskPrice(ctx, b, t.TypeID), lb.Ask, lb.Bid)
		if price <= 0 || !gearBidSane(price, evedb.TypeValueOf(ctx, m.db, t.TypeID).Value) {
			continue
		}
		units := gearBidUnits(price, budget, hold, t.UnitVol)
		if units <= 0 {
			continue
		}
		if !tradeCargoOK(t.CategoryID, units) {
			continue
		}
		value := price * float64(units)
		if value < tradeMinProfitISK {
			continue // an order slot costs more than this is worth committing
		}
		if d.Wanted <= bestWanted {
			continue
		}
		bestWanted = d.Wanted
		best = &bidPlan{
			TypeID: t.TypeID, TypeName: t.TypeName, UnitVol: t.UnitVol,
			Price: price, Units: units,
			LocalAsk: lb.Ask, LocalBid: lb.Bid,
			Evidence: fmt.Sprintf("%s — the roster has failed to buy %s of them and nobody bids here",
				t.Evidence, iskShort(float64(d.Wanted))),
			Gear: true,
		}
	}
	return best, nil
}
