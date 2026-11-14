package rolebots

import (
	"context"
	"fmt"
)

// The sale half of a mining trip counted distance differently from the half
// that chose the belt. findMiningTargetIn walks the map with miningRules, which
// zeroes the pilot's low-security aversion, while shopLadder (the buyer search
// once the hold is full) uses economicRules, where each hop below the pilot's
// comfort line costs about eight jump-equivalents (hopCost). Two lowsec gates
// then read as sixteen against oreSellMaxJumps = 10 and the miner stockpiled
// its load. saleRules gives the sale the belt search's rules.

// saleRules are the rules the belt search used, applied to the sale: the same
// hard lines (an outlaw's ceiling, floor, scope) and no comfort-line pricing.
func saleRules(r travelRules) travelRules {
	r.SecAversion = 0
	return r
}

// sellWithinGates re-prices the stops the ladder already found by plain gate
// count and returns the best buyer inside oreSellMaxJumps of them, or nil. It
// only ever ADDS buyers: a stop the ladder already accepted never reaches it.
func (m *Mod) sellWithinGates(ctx context.Context, b *botRun, look marketLook, scan marketScan,
	corpBids map[int64]bool) *marketStop {

	if len(scan.Stops) == 0 {
		return nil
	}
	gates, err := m.systemsWithin(ctx, b.SystemID, saleRules(look.Rules), oreSellMaxJumps)
	if err != nil {
		return nil
	}
	return pickWithinGates(scan.Stops, gates, b.Tune, corpBids)
}

// pickWithinGates is the pure core of sellWithinGates: restate each stop's
// distance as its gate cost and let loyalSell choose among those inside the cap.
func pickWithinGates(stops []marketStop, gates map[int64]int, tn Tuning, corpBids map[int64]bool) *marketStop {
	near := make([]marketStop, 0, len(stops))
	for _, s := range stops {
		if j, ok := gates[s.SystemID]; ok && j <= oreSellMaxJumps {
			s.Jumps = j
			near = append(near, s)
		}
	}
	return loyalSell(near, tn, corpBids)
}

// dockBidsOnLots reports whether a bid on any of these types reaches the dock
// - the question sellOreStack asks on arrival, asked before choosing where to
// go. NPC ore bids are region-wide, so a dock in a region with a refinery
// buyer pays for the rock where a dock without one only stores it.
func (m *Mod) dockBidsOnLots(ctx context.Context, b *botRun, stationID int64, typeIDs []int64) bool {
	if stationID == 0 {
		return false
	}
	for _, t := range typeIDs {
		if bids, err := m.bidsReachingStation(ctx, stationID, t, b.CharID); err == nil && len(bids) > 0 {
			return true
		}
	}
	return false
}

// noSellerReasonWithin is noSellerReason for a sale with a distance cap. When
// stops exist but all lie past the cap the old text blamed "space it will not
// enter" and told the operator to soften the pilot's personality, which sent
// the investigation to the wrong page: the buyers are reachable, just far.
func (m *Mod) noSellerReasonWithin(ctx context.Context, scan marketScan, what string, capJumps int) string {
	if len(scan.Stops) > 0 {
		nearest := scan.Stops[0].Jumps
		for _, s := range scan.Stops[1:] {
			if s.Jumps < nearest {
				nearest = s.Jumps
			}
		}
		return fmt.Sprintf("%d station(s) %s, but the nearest is %d jump(s) away on its route against a cap of %d",
			len(scan.Stops), what, nearest, capJumps)
	}
	return m.noSellerReason(ctx, scan, what)
}
