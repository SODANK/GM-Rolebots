package rolebots

// Pricing a listing against the book that is already on the counter.
//
// Pricing from one global number (estimatePrice is the cheapest ask anywhere) made every pilot list the
// same price. Price against the local book and decide by depth:
//
//	thin queue ahead   the cheapest asks are a dribble that clears soon, so the pilot becomes the market. Price above them.
//	fat queue ahead    real stock sits at that price and waiting will not clear it. Undercut it.
//
// Thin and fat are measured against the pilot's own lot, so no demand model is needed. Steps are
// percentages, never 0.01 ISK, which would start a race to the bottom.

import (
	"context"
	"database/sql"
	"fmt"
)

const (
	// The share of its own lot a pilot lets sit in front of it before the queue stops being a dribble. At a
	// half, a listing of 20,000 ignores up to 10,000 cheaper units and a listing of 200 is walled by 100.
	askThinFraction = 0.5

	// Default steps for callers with no personality (ore ask). mineralAsk derives per-bot steps from
	// MineralMarkup so listings at one station do not converge.
	askUndercutStep = 0.03
	askOvercutStep  = 0.05

	// The hard floor under any competitive price, as a fraction of stableFloorPrice. It stops a contested
	// book walking to 1 ISK: below it the pilot keeps the goods and lets the order sweep decide.
	askFloorFraction = 0.6
)

// The personal floor: how far under basePrice this pilot chases a rival before pulling the goods.
// askFloorFraction (0.6) stays the stable anchor the ceilings divide out of. A shared floor ends every
// price race on a glutted commodity in a tie, so reservation prices differ per pilot.
const (
	askFloorFracLo = 0.52 // MineralMarkup 1.0 (generous)
	askFloorFracHi = 0.90 // MineralMarkup 1.9 (greedy)
)

// personalT normalises MineralMarkup (1.0–1.9) to 0..1: 0 generous, 1 greedy.
// The one axis every per-pilot price band in this file and oreask.go reads.
func personalT(tn Tuning) float64 {
	t := (tn.MineralMarkup - 1.0) / 0.9
	if t < 0 {
		return 0
	}
	if t > 1 {
		return 1
	}
	return t
}

// personalAskFloorFrac is this pilot's floor as a fraction of basePrice.
func personalAskFloorFrac(tn Tuning) float64 {
	return askFloorFracLo + personalT(tn)*(askFloorFracHi-askFloorFracLo)
}

// personalFloorPrice is stableFloorPrice at this pilot's own fraction.
func (m *Mod) personalFloorPrice(ctx context.Context, typeID int64, tn Tuning) float64 {
	sf := m.stableFloorPrice(ctx, typeID)
	if sf <= 0 {
		return 0
	}
	return sf / askFloorFraction * personalAskFloorFrac(tn)
}

// askSteps carries per-bot undercut and overcut percentages: greedy bots protect margin (small undercut,
// large overcut), generous bots compete on price. A zero value means use the default constant.
type askSteps struct {
	Undercut float64 // fraction to cut under a wall, e.g. 0.03
	Overcut  float64 // fraction to price over a thin book, e.g. 0.05
}

var askDefaultSteps = askSteps{Undercut: askUndercutStep, Overcut: askOvercutStep}

// askLevel is one price point of a station's sell book for one type.
type askLevel struct {
	Price float64
	Vol   int64
}

// askAgainstBook is the placement price: what to ask given who is already asking, how much they hold and
// how much the pilot lists. fallback is its own valuation with markup and scarcity applied; floor is the
// price below which it will not sell. It returns the price and a short reason for the log. Pure.
func askAgainstBook(book []askLevel, ownQty int64, fallback, floor float64, steps askSteps) (float64, string) {
	if fallback <= 0 {
		return 0, ""
	}
	if floor > fallback {
		// A floor above the pilot's own valuation would price it out of its listing. The valuation wins; the
		// floor protects only the competitive branches below.
		floor = fallback
	}
	if len(book) == 0 {
		return roundISK(fallback), "no competition here"
	}

	undercut := steps.Undercut
	if undercut <= 0 {
		undercut = askUndercutStep
	}
	overcut := steps.Overcut
	if overcut <= 0 {
		overcut = askOvercutStep
	}

	// How much cheaper stock the pilot is prepared to treat as a queue that
	// clears itself rather than as a wall.
	thin := float64(ownQty) * askThinFraction

	var cum float64
	for _, lvl := range book {
		cum += float64(lvl.Vol)
		if cum >= thin {
			// The first level whose depth with everything cheaper is comparable to the pilot's lot is the wall,
			// and the only way past it is under it.
			price := lvl.Price * (1 - undercut)
			if price < floor {
				price = floor
			}
			return roundISK(price), fmt.Sprintf(
				"undercutting %d unit(s) at %.2f by %.1f%%", lvl.Vol, lvl.Price, undercut*100)
		}
	}

	// The whole book ahead is thinner than half this lot: it sells out, so price over the top of it and
	// never under the pilot's own valuation.
	top := book[len(book)-1]
	price := top.Price * (1 + overcut)
	if price < fallback {
		price = fallback
	}
	return roundISK(price), fmt.Sprintf(
		"overcutting a thin book (%.0f unit(s) ahead of a %d-unit lot) by %.1f%%",
		cum, ownQty, overcut*100)
}

// localAskBook reads one station's sell book for one type, cheapest first, excluding the pilot's own
// orders. Without the exclusion a pilot with several lines on one dock would undercut its own cheapest
// order in a loop.
func (m *Mod) localAskBook(ctx context.Context, charID, stationID, typeID int64) ([]askLevel, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT price, SUM(volRemaining) AS vol
		  FROM market_orders
		 WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1
		   AND volRemaining > 0 AND charID <> ?
		 GROUP BY price
		 ORDER BY price ASC`, stationID, typeID, charID)
	if err != nil {
		return nil, fmt.Errorf("local ask book: %w", err)
	}
	defer rows.Close()
	var out []askLevel
	for rows.Next() {
		var l askLevel
		if err := rows.Scan(&l.Price, &l.Vol); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// stableFloorPrice is the anchor every competitive floor is measured from: invTypes.basePrice. A floor
// must not move with the market: one taken from the current price is re-derived from the number it just
// lowered and halves every sweep. For minerals basePrice comes from real series (sql/ore_prices.sql).
func (m *Mod) stableFloorPrice(ctx context.Context, typeID int64) float64 {
	var base sql.NullFloat64
	if err := m.db.QueryRowContext(ctx,
		`SELECT basePrice FROM invTypes WHERE typeID = ?`, typeID).Scan(&base); err != nil {
		return 0
	}
	if !base.Valid || base.Float64 <= 0 {
		return 0
	}
	return base.Float64 * askFloorFraction
}
