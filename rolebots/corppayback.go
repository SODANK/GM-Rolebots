package rolebots

import "context"

// Does a print pay for itself? The library purchase (postLibraryPurchase) gates a print on its price fitting the
// budget and its product building at a positive margin a run. Neither says whether the corporation can sell
// enough of the product to earn the print back, and on a thin market it cannot.
// So a print is bought only when its margin, over the product's own trade tape, earns the price back inside
// corpPOPaybackSec at a share of the market the corporation can plausibly take. The tape is every buyer's row, so
// units the roster bought from NPC sellers count: that is the demand a factory exists to capture (npcleak.go).

const (
	// corpPOPaybackSec is how long a print may take to earn back its price.
	corpPOPaybackSec = 30 * 86400
	// corpPOMarketShare is the share of a product's regional trade a new
	// entrant's factory is counted on taking.
	corpPOMarketShare = 0.25
	// corpPOClusterShare is the share of the cluster's trade a factory
	// standing in a region that is not a market for the product is counted on
	// reaching: it sells where it stands, and buyers elsewhere have their own.
	corpPOClusterShare = 0.05
)

// poPaysBack reports whether a print earns its price inside the payback window
// at the margin a run leaves, over the units the tape says sell per window.
// regionUnits and clusterUnits are the product's units traded over the tape
// window (corpTradeRefWindowSec). Pure.
func poPaysBack(marginPerRun float64, portion int64, regionUnits, clusterUnits int64, price float64) bool {
	if marginPerRun <= 0 || price <= 0 {
		return marginPerRun > 0
	}
	if portion < 1 {
		portion = 1
	}
	units := max(float64(regionUnits)*corpPOMarketShare, float64(clusterUnits)*corpPOClusterShare)
	runs := units * float64(corpPOPaybackSec) / float64(corpTradeRefWindowSec) / float64(portion)
	return runs*marginPerRun >= price
}

// The production governor. Free capital (corpcommit.go) nets every open promise, but for a corporation that is
// not a trading house the inflow is the trade levy, which went into minerals faster than product went out. So a
// think may commit to production only what the corporation's own turnover carries: product sold over
// corpProdWindowSec, times corpProdGrowth, plus corpProdFloor to start a business with no record, less what it
// already spent on materials in the same window. Over that the board posts no new build, bids for no material
// and buys no print until sales catch up; open orders still build from what is on hand.

const (
	corpProdWindowSec = 14 * 86400
	// corpProdGrowth lets materials outrun sales by a quarter: a growing
	// factory buys ahead of what it sells, a flooded one does not.
	corpProdGrowth = 1.25
	// corpProdFloor is the window's material allowance with no sales at all.
	corpProdFloor = 15e6
	// corpProdLogSec spaces the governor's board note.
	corpProdLogSec = 6 * 3600
)

// productionAllowance is what the window still lets production commit: the
// sales-carried allowance less the materials already bought in it, never
// negative. Pure.
func productionAllowance(sold, bought float64) float64 {
	return max(corpProdFloor+corpProdGrowth*sold-bought, 0)
}

// productionGovernor caps one production think's budget by the corporation's
// turnover (productionAllowance), and notes on the board when it binds.
func (m *Mod) productionGovernor(ctx context.Context, c *corpCEO, budget float64) float64 {
	var sold, bought float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(IF(line IN (?, ?, ?), amount, 0)), 0),
		       COALESCE(-SUM(IF(line IN (?, ?, ?), amount, 0)), 0)
		  FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND at > ?`,
		// Copy sales are turnover and job fees are spend: a research house
		// copying behind a full counter paid fees the governor never saw. The trade
		// desk's buys are spend too: its sales count as turnover.
		lineSales, lineStore, lineResearchSales, lineBuyback, lineJobFees, lineTradeBuy,
		c.CorpID, clock.Unix()-corpProdWindowSec).Scan(&sold, &bought)
	allow := productionAllowance(sold, bought)
	if allow >= budget {
		return budget
	}
	if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "prodcap") >= corpProdLogSec {
		m.ceoLog(ctx, c, powerProduction, "prodcap", 0, allow,
			"held production to %s — it bought %s of materials in a fortnight and sold %s of product",
			iskString(allow), iskString(bought), iskString(sold))
	}
	return allow
}

// printPaysBack is poPaysBack for one print, bought for a factory in regionID.
func (m *Mod) printPaysBack(ctx context.Context, bpTypeID, regionID int64, marginPerRun, price float64) bool {
	f, err := m.printFactsFor(ctx, bpTypeID)
	if err != nil || f.ProductTypeID == 0 {
		return false
	}
	refs := m.tradeRefsOf(ctx, f.ProductTypeID)
	return poPaysBack(marginPerRun, f.Portion, refs.Region[regionID].Units, refs.Cluster.Units, price)
}

// corpCopyDemandShare is the share of the roster's NPC purchases of a print a
// corporation's copy service is counted on taking: its copies sell from a few
// counters, and every other library may be selling the same type.
const corpCopyDemandShare = 0.10

// copySalesPayBack is poPaysBack for an original bought to be COPIED for sale
// (corpcopydemand.go): the copies its share of the roster's NPC purchases
// would sell inside corpPOPaybackSec, at copyPrice each, must earn its price.
// buys is the NPC purchases over printDemandWindowDays. Pure.
func copySalesPayBack(buys int64, copyPrice, price float64) bool {
	if buys <= 0 || copyPrice <= 0 {
		return false
	}
	if price <= 0 {
		return true
	}
	sold := float64(buys) * corpCopyDemandShare * float64(corpPOPaybackSec) / float64(printDemandWindowDays*86400)
	return sold*copyPrice >= price
}
