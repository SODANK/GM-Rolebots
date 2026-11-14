package rolebots

import (
	"context"
	"fmt"
	"strings"

	evedb "gurimoon/db"
)

// The researcher's purchase: buy a blueprint for the library, research it, copy it. A research
// career lives on the copies it sells (bpcopy.go, escrow_sell.go listLibraryProducts); the original
// stays in the library. It runs when the research trip finds nothing in the hangar worth researching:
// it buys at the current station if the counter has a print worth the lab hours, otherwise flies to
// the best counter within labShopMaxJumps (researcherShopTrip, phase TaskLabBuy).
//
// Tech I only, with skills the lab counter would accept (labResearchSkillsMet), never a type already
// owned. Priced on labBuyCopyHorizon full-run copies at ME labBuyTargetME (labBuyExpect), which must
// clear labBuyMinMargin over the ask. Paid from surplus only (labWalletFloorISK plus a fee cushion):
// research must not come out of working capital.

const (
	// labBuyTargetME is the level the copies are priced at: "fully researched"
	// (bpoFullMEDefault), where the library takes a print before copying it in
	// earnest.
	labBuyTargetME = bpoFullMEDefault
	// labBuyCopyHorizon is how many full-run copies a purchase is expected to earn back against;
	// a print that does not pay for itself in this many sales is not worth a library slot.
	labBuyCopyHorizon = 4
	// labBuyMinMargin is the expected sale over the ask below which a purchase
	// is not worth the lab hours it will tie up.
	labBuyMinMargin = 1.25
	// labBuyFeeCushion is the allowance for lab fees on the way to the target, as a multiple of
	// the ask. It is rough: the true quote (labFeeQuote) needs a line and a pick.
	labBuyFeeCushion = 0.15
	// labBuyScan bounds the sell orders weighed at one station.
	labBuyScan = 80
)

// labBuyOption is one blueprint on offer at the station.
type labBuyOption struct {
	TypeID    int64
	Name      string
	Ask       float64
	ProductID int64
	Wanted    int64 // builders' unmet demand for the product (rolebot_market_demand)
	Expect    float64
	Score     float64
}

// labBuyScore ranks an option: expected margin over the ask, lifted by whether
// a builder is asking for the product. Zero when the sale does not clear the
// bar. Pure.
func labBuyScore(ask, expect float64, wanted int64) float64 {
	if ask <= 0 || expect < ask*labBuyMinMargin {
		return 0
	}
	score := expect - ask
	if wanted > 0 {
		score *= 2
	}
	return score
}

// labBuyExpect is what a print earns the library over labBuyCopyHorizon
// copies of copyRuns runs at the research target. Pure.
func labBuyExpect(ref float64, copyRuns, maxRuns int64, savingPerRun, frac float64) float64 {
	return bpcPrice(ref, copyRuns, maxRuns, savingPerRun, 0, frac) * labBuyCopyHorizon
}

// labBuyAffordable is the wallet test for a purchase. Pure.
func labBuyAffordable(balance, ask float64) bool {
	return balance >= ask*(1+labBuyFeeCushion)+labWalletFloorISK
}

// researcherBuyPrint buys one blueprint worth researching at the pilot's
// current station and reports whether it did. The print lands in the pilot's
// hangar here; the stray sweep (labFetchStray) carries it to the factory if it
// is not already there.
func (m *Mod) researcherBuyPrint(ctx context.Context, b *botRun) (bool, error) {
	bal, ok := m.researcherShopReady(ctx, b)
	if !ok {
		return false, nil
	}
	best, err := m.labBuyPickAt(ctx, b, b.StationID, bal)
	if err != nil || best == nil {
		return false, err
	}
	_, _, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID, best.TypeID, 1, "blueprint")
	if err != nil {
		// The shelf moved between the scan and the buy: not a fault.
		m.logBot(b.CharID, logInfo, "lab", "the %s was gone before it could buy it (%v)", best.Name, err)
		return false, nil
	}
	m.logBot(b.CharID, logInfo, "lab",
		"bought a %s for %s at %s for its library - it expects about %s from its first copies at ME %d%s",
		best.Name, iskString(spent), m.stationLabel(ctx, b.StationID), iskString(best.Expect), labBuyTargetME,
		map[bool]string{true: ", and a builder is asking for the product", false: ""}[best.Wanted > 0])
	return true, nil
}

// researcherShopReady is the researcher's case for shopping at all: a research career, money
// over the floor, and no unresearched original anywhere (a print in another hangar is a stray
// that housekeeping carries home, labFetchStray, not a reason to buy). Answers the balance.
func (m *Mod) researcherShopReady(ctx context.Context, b *botRun) (float64, bool) {
	if !researchCareer(b.Tune) || b.StationID == 0 || b.State == nil {
		return 0, false
	}
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil || bal < labWalletFloorISK {
		return 0, false
	}
	var held int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  `+bpBuyableJoin+`
		  LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
		 WHERE e.ownerID = ? AND e.flag = 4 AND COALESCE(bp.copy, 0) = 0
		   AND COALESCE(bp.materialLevel, 0) < ?`, b.CharID, labBuyTargetME).Scan(&held)
	// A print still short of full research is the next lab job, not a reason
	// to buy another.
	return bal, held == 0
}

// labBuyPickAt is the best blueprint to buy, research and sell at one
// station, or nil. Read-only.
func (m *Mod) labBuyPickAt(ctx context.Context, b *botRun, stationID int64, bal float64) (*labBuyOption, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, t.typeName, MIN(o.price), bt.productTypeID,
		       COALESCE(MAX(`+demandWantedSQL("d", clock.Unix())+`), 0)
		  FROM market_orders o
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = o.typeID
		  `+bpBuyableJoin+`
		  JOIN invTypes t ON t.typeID = o.typeID
		  LEFT JOIN rolebot_market_demand d ON d.typeID = bt.productTypeID AND d.wanted > 0
		 WHERE o.stationID = ? AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		   AND NOT EXISTS (SELECT 1 FROM entity e WHERE e.ownerID = ? AND e.typeID = o.typeID)
		 GROUP BY o.typeID, t.typeName, bt.productTypeID
		 LIMIT ?`, stationID, b.CharID, labBuyScan)
	if err != nil {
		return nil, fmt.Errorf("blueprints on offer: %w", err)
	}
	var opts []labBuyOption
	for rows.Next() {
		var o labBuyOption
		if rows.Scan(&o.TypeID, &o.Name, &o.Ask, &o.ProductID, &o.Wanted) == nil && labBuyAffordable(bal, o.Ask) {
			opts = append(opts, o)
		}
	}
	rows.Close()
	if len(opts) == 0 {
		return nil, nil
	}

	// Only what this pilot may research: the same refusal the lab counter gives,
	// asked once for the whole list.
	ids := make([]int64, 0, len(opts))
	for _, o := range opts {
		ids = append(ids, o.TypeID)
	}
	allowed, err := m.labResearchSkillsMet(ctx, b.CharID, ids)
	if err != nil {
		return nil, err
	}
	skills, _ := m.charSkillsFor(ctx, b.CharID)
	peSkill := skills[skillProductionEff]
	frac := m.bpcPriceFrac(ctx)
	var best *labBuyOption
	for i := range opts {
		o := &opts[i]
		if !allowed[ramActivityMEResearch][o.TypeID] || !allowed[ramActivityCopying][o.TypeID] {
			continue
		}
		f, ferr := m.printFactsFor(ctx, o.TypeID)
		if ferr != nil {
			continue
		}
		ref := o.Ask
		if r := m.printRefPrice(ctx, b, o.TypeID); r > 0 {
			ref = r
		}
		saving := printSavingPerRun(ctx, m.db, o.TypeID, labBuyTargetME, peSkill)
		o.Expect = labBuyExpect(ref, f.CopyRuns, f.MaxRuns, saving, frac)
		o.Score = labBuyScore(o.Ask, o.Expect, o.Wanted)
		if o.Score > 0 && (best == nil || o.Score > best.Score ||
			(o.Score == best.Score && o.TypeID < best.TypeID)) {
			best = o
		}
	}
	return best, nil
}

// ── the shopping trip ────────────────────────────────────────────────────────
// A researcher whose own station sells nothing worth researching flies to one that does
// (TaskLabBuy). The trip reuses the lab errand's station fields; on arrival researcherBuyPrint
// buys at that counter and the print becomes a stray the housekeeping carries to the factory.

const (
	// labShopMaxJumps is how far a researcher will fly to buy a print.
	labShopMaxJumps = 5
	// labShopStations bounds how many candidate counters are priced per scan:
	// each is a pick (a query and a valuation per print on offer).
	labShopStations = 5
	// labShopJumpCost discounts an option's score per jump of travel.
	labShopJumpCost = 0.15
)

// labShopValue is an option's score net of the trip to it. Pure.
func labShopValue(score float64, jumps int) float64 {
	return score / (1 + labShopJumpCost*float64(jumps))
}

// researcherShopTrip sends a researcher with nothing to research to the best
// blueprint counter within labShopMaxJumps. Reports whether it routed the
// pilot.
func (m *Mod) researcherShopTrip(ctx context.Context, b *botRun) (bool, error) {
	bal, ok := m.researcherShopReady(ctx, b)
	if !ok || b.SystemID == 0 {
		return false, nil
	}
	reach, err := m.systemsWithin(ctx, b.SystemID, rulesForBot(b), labShopMaxJumps)
	if err != nil || len(reach) == 0 {
		return false, err
	}
	args := []any{b.StationID}
	ph := ""
	for id := range reach {
		args = append(args, id)
		ph += "?,"
	}
	args = append(args, b.CharID, labShopStations)
	// The counters with the most blueprint asks the pilot does not already own.
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.solarSystemID, COUNT(DISTINCT o.typeID) n
		  FROM market_orders o
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = o.typeID
		  `+bpBuyableJoin+`
		 WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0 AND o.stationID <> ?
		   AND o.solarSystemID IN (`+strings.TrimSuffix(ph, ",")+`)
		   AND NOT EXISTS (SELECT 1 FROM entity e WHERE e.ownerID = ? AND e.typeID = o.typeID)
		 GROUP BY o.stationID, o.solarSystemID
		 ORDER BY n DESC
		 LIMIT ?`, args...)
	if err != nil {
		return false, fmt.Errorf("blueprint counters in reach: %w", err)
	}
	type counter struct{ station, sys int64 }
	var counters []counter
	for rows.Next() {
		var c counter
		var n int
		if rows.Scan(&c.station, &c.sys, &n) == nil {
			counters = append(counters, c)
		}
	}
	rows.Close()
	var best *labBuyOption
	var bestAt counter
	bestVal := 0.0
	for _, c := range counters {
		o, perr := m.labBuyPickAt(ctx, b, c.station, bal)
		if perr != nil || o == nil {
			continue
		}
		if v := labShopValue(o.Score, reach[c.sys]); v > bestVal {
			best, bestAt, bestVal = o, c, v
		}
	}
	if best == nil {
		return false, nil
	}
	d := &b.State.Data
	clearLabTrip(d)
	d.LabStationID, d.LabStationSysID = bestAt.station, bestAt.sys
	d.LabBPTypeID, d.LabBPName = best.TypeID, best.Name
	m.logBot(b.CharID, logInfo, "lab",
		"nothing here worth researching - heading to %s (%d jump(s)) to buy a %s for about %s to research and sell",
		m.stationLabel(ctx, bestAt.station), reach[bestAt.sys], best.Name, iskString(best.Ask))
	b.State.Activity = ActIndustry
	return true, m.beginTravel(ctx, b, bestAt.sys, bestAt.station, string(TaskLabBuy))
}

// labShopStep is the TaskLabBuy phase: at the counter, buy.
func (m *Mod) labShopStep(ctx context.Context, b *botRun) (bool, error) {
	d := &b.State.Data
	if d.LabStationID == 0 {
		return false, m.finishLabTrip(ctx, b, "")
	}
	if b.StationID != d.LabStationID {
		return false, m.beginTravel(ctx, b, d.LabStationSysID, d.LabStationID, string(TaskLabBuy))
	}
	bought, err := m.researcherBuyPrint(ctx, b)
	if err != nil && ctx.Err() != nil {
		return false, err
	}
	if !bought {
		d.LabFailAt = clock.Unix() + labFailCooldownSec
		return false, m.finishLabTrip(ctx, b, "arrived to find nothing on the counter worth researching any more")
	}
	// A stray now: the housekeeping ahead of the next draw carries it home.
	d.LabCheckedAt = 0
	return false, m.finishLabTrip(ctx, b, "")
}
