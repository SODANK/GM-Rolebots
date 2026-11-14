package rolebots

import "context"

// Trade runs on the ask ladder (sweep.go). The trade scan reads each station as (cheapest ask, whole
// book), so a run would be sized to every unit on the book and priced as if all cost the cheapest ask.
// Where a book has tiers that oversizes the run and overstates profit, and tradeBuy stops climbing at
// ask x 1.1, so the pilot hauls a short load. The shortlisted runs are therefore re-sized on the
// station's real ladder, keeping every unit that still clears the bid, and the counter climbs to the
// same line.

// tradeLadderEdge is the most a unit may cost as a fraction of the bid it is
// hauled to: a unit bought within 2% of what it sells for is not worth the
// hold space or the sales tax.
const tradeLadderEdge = 0.98

// tradeRunOnLadder re-sizes an arbitrage run on the buy station's real ask ladder: units under
// tradeLadderEdge x the bid, within the budget, with Outlay and Profit at the prices paid. False
// drops the run (nothing pays). A run that is not an arbitrage, an unreadable book or a switched-off
// sweep leaves the scan's figures alone.
func (m *Mod) tradeRunOnLadder(ctx context.Context, b *botRun, d *tradeDeal, budget float64) bool {
	if d.Kind != tradeDealArb || d.Bid <= 0 || !m.sweepSettings(ctx).Enabled {
		return true
	}
	book, err := m.localAskBook(ctx, b.CharID, d.BuyStationID, d.TypeID)
	if err != nil {
		return true
	}
	units, outlay := affordableQty(book, d.Units, d.Bid*tradeLadderEdge, budget)
	if units <= 0 || !tradeCargoOK(d.CategoryID, units) {
		return false
	}
	d.Units = units
	d.Outlay = outlay
	d.Profit = float64(units)*d.Bid - outlay
	return true
}

// tradeBuyCeiling is the most the trade counter pays a unit: the line a run was sized on
// (tradeLadderEdge of the bid) when the run is hauled to a bid and the sweep is on, else ask x 1.1
// as a drift allowance. Never under the ask itself, which the caller has checked is under the bid.
func (m *Mod) tradeBuyCeiling(ctx context.Context, ask, bid float64) float64 {
	if bid > 0 && m.sweepSettings(ctx).Enabled {
		if c := bid * tradeLadderEdge; c > ask {
			return c
		}
		return ask
	}
	return ask * 1.1
}
