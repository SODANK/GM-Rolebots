package rolebots

// The trader's ore flip: buy rock, refine it here, list the metal. The
// counterparty to a miner's ore ask (oreask.go).
//
// It is a station action, not a deal. Ore compresses 3x to 400x, so rock is
// refined where it is bought and the metal travels (specSupplyCategories,
// tradeorders.go). tradeScan is a gap pass over one typeID, and a spread across
// two type sets would need a two-type deal shape through tradeBuy, tradeSell,
// tradeRank and expected(). So it is shaped like maybeLeaveStandingBid
// (tradebids.go): called from tradePlan while docked, at most one act, no order
// slot committed (only the mineral listing rests, via listRefinedMinerals).

import (
	"context"
	"sort"

	evedb "gurimoon/db"
)

const (
	// oreArbWalletFrac is how much of the wallet one flip may commit. Kept under
	// bidWalletFrac (0.35): a flip is an opportunity noticed in passing, and the
	// wallet must stay free for the next run.
	oreArbWalletFrac = 0.20

	// oreArbMaxStacks caps how many ore types one visit will take. The point
	// is to clear a mispriced stack, not to buy out a station's ore book in
	// one pass and leave the miners who supplied it nothing to sell into.
	oreArbMaxStacks = 3
)

// maybeRefineArbitrage buys rock at this station, refines it, and lists the
// metal when the spread is worth it at this pilot's yield. Best-effort and
// silent when there is nothing to do: it runs on every docked trade pass and
// the common answer is no.
func (m *Mod) maybeRefineArbitrage(ctx context.Context, b *botRun) {
	if b.StationID == 0 || b.ShipID == 0 {
		return
	}
	// The same gate speculation and standing bids use. A flip is speculation
	// with a reprocessing step in the middle.
	if tradeInterest(b.Tune) < specMinInterest {
		return
	}
	// One query answers the whole thing for a station with no plant, which is
	// most of them: no reprocessing, no flip, and the pass ends here.
	if _, err := evedb.GetStationReprocessingInfo(ctx, m.db, b.StationID); err != nil {
		return
	}
	hand := m.refineHandAt(ctx, b, b.StationID, 0)

	vals, err := m.oreValues(ctx, m.valuationRegions(ctx, b), oreValueBid)
	if err != nil || len(vals) == 0 {
		return
	}

	// The ore asks standing on this dock: the local book, not a regional one,
	// because the rock is refined where it is bought.
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, MIN(o.price), SUM(o.volRemaining)
		  FROM market_orders o
		  JOIN invTypes t  ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE o.stationID = ? AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		   AND g.categoryID = 25
		 GROUP BY o.typeID`, b.StationID)
	if err != nil {
		rlogf("rolebots: ore arb book char=%d: %v", b.CharID, err)
		return
	}
	type offer struct {
		typeID int64
		name   string
		ask    float64
		vol    int64
		margin float64
	}
	var offers []offer
	for rows.Next() {
		var o offer
		if err := rows.Scan(&o.typeID, &o.ask, &o.vol); err != nil {
			break
		}
		v, ok := vals[o.typeID]
		if !ok || o.ask <= 0 || o.vol <= 0 {
			continue
		}
		net := hand.WithOreSkill(m.oreSkillLevelFor(ctx, b.CharID, v.GroupID)).Net()
		o.margin, o.name = oreRefineMargin(v, net, o.ask), v.Name
		if o.margin >= b.Tune.TradeMinMarginFrac {
			offers = append(offers, o)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(offers) == 0 {
		return
	}
	// Best margin first, tie-broken on typeID so two equal offers do not swap
	// places between passes.
	sort.Slice(offers, func(i, j int) bool {
		if offers[i].margin != offers[j].margin {
			return offers[i].margin > offers[j].margin
		}
		return offers[i].typeID < offers[j].typeID
	})

	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil || bal <= 0 {
		return
	}
	budget := bal * oreArbWalletFrac
	free, ferr := m.holdFreeM3(ctx, b.CharID, b.ShipID)
	if ferr != nil {
		free = 0
	}

	got := map[int64]int64{}
	spentTotal, bought := 0.0, 0
	for _, o := range offers {
		if bought >= oreArbMaxStacks || budget <= 0 {
			break
		}
		v := vals[o.typeID]
		units := o.vol
		if byISK := int64(budget / o.ask); byISK < units {
			units = byISK
		}
		// The hold only bounds the buy, to stop one flip swallowing a whole book:
		// buyAtStation lands the goods in the hangar, not the hold.
		if free > 0 && v.Volume > 0 {
			if byHold := int64(free / v.Volume); byHold > 0 && byHold < units {
				units = byHold
			}
		}
		if units < v.Portion {
			continue // short of a whole reprocessing batch: nothing would come out
		}
		if o.ask*float64(units) < tradeMinProfitISK {
			continue
		}
		_, filled, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID, o.typeID, units, purposeOreFlip)
		if err != nil || filled <= 0 {
			continue
		}
		budget -= spent
		spentTotal += spent
		bought++
		m.noteOreFlow(ctx, o.typeID, oreStageBought, filled, spent)
		m.logBot(b.CharID, logInfo, "trade",
			"bought %d× %s at %.2f to refine here — %.0f%% over what the rock cost",
			filled, o.name, o.ask, o.margin*100)
	}
	if bought == 0 {
		return
	}

	// Refine everything the hangar now holds and put the metal on the book
	// (refineHangarOre, then listRefinedMinerals for the sale and registration).
	unitsIn, minerals, rerr := m.refineHangarOre(ctx, b)
	if rerr != nil {
		m.logBot(b.CharID, logError, "trade", "refining the rock it just bought: %v", rerr)
		return
	}
	if unitsIn <= 0 || len(minerals) == 0 {
		return
	}
	for id, q := range minerals {
		got[id] += q
	}
	listed, _, held, lerr := m.listRefinedMinerals(ctx, b, got, nil, mineralHoldMaxSec)
	if lerr != nil {
		rlogf("rolebots: ore arb listing char=%d: %v", b.CharID, lerr)
	}
	m.logBot(b.CharID, logInfo, "trade",
		"flipped %d unit(s) of rock into metal at %s for %s — %d lot(s) listed, %d held back",
		unitsIn, m.stationLabel(ctx, b.StationID), iskString(spentTotal), listed, held)
}
