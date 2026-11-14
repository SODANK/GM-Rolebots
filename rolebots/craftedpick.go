package rolebots

import "context"

// Crafted stock in the shopping trip, where craftedEdge has to apply.
// craftedEdge (reach.go) gives bot- and player-made gear an 8% edge in buyCost, but the
// shopping trip does not ask buyCost: findnext picks stops with pickSupplyStop (coverage and
// distance), and procure.outfit buys every line the current dock sells at the NPC price.
// One rule serves both phases: an NPC stop for a gear line is dominated when crafted stock
// that can fill the whole line costs less by buyCost. findnext drops dominated stops before
// it scores, and outfit leaves a dominated line on the list so findnext sends the pilot on.

// craftedDeferJumps is how far outfit looks for crafted stock before buying a line at the
// NPC shelf it stands at. At the default slope the edge is worth about four jumps.
const craftedDeferJumps = 3

// dropCraftedDominated removes the NPC stops a buyer should pass over: for each type with
// crafted stops whose stock covers the whole line (want; any stock if the size is unknown),
// every non-crafted stop costing more by buyCost than the cheapest crafted one. The pilot's
// own stock is never dropped and the input is not modified. Pure.
func dropCraftedDominated(stops []marketStop, want map[int64]int64, tn Tuning) []marketStop {
	bestCrafted := map[int64]float64{}
	craftedVol := map[int64]int64{}
	for _, s := range stops {
		if !s.Crafted || s.Price <= 0 || s.Volume <= 0 {
			continue
		}
		craftedVol[s.TypeID] += s.Volume
		if c := s.buyCost(tn); bestCrafted[s.TypeID] == 0 || c < bestCrafted[s.TypeID] {
			bestCrafted[s.TypeID] = c
		}
	}
	if len(bestCrafted) == 0 {
		return stops
	}
	out := make([]marketStop, 0, len(stops))
	for _, s := range stops {
		best, ok := bestCrafted[s.TypeID]
		if ok && !s.Crafted && s.Price != ownStockNominalPrice &&
			craftedVol[s.TypeID] >= want[s.TypeID] && s.buyCost(tn) > best {
			continue
		}
		out = append(out, s)
	}
	return out
}

// craftedDeferLines is the set of shopping lines outfit should not buy at the current
// station because crafted stock within craftedDeferJumps dominates the shelf here. The scan
// uses findnext's first station filter and residency scope. It returns nil on any failure,
// and at a station where the last visit bought nothing (ProcureStuckAt).
func (m *Mod) craftedDeferLines(ctx context.Context, b *botRun, shopping []ShopItem) map[int64]bool {
	if b.StationID == 0 || b.Profile == nil || b.State.Data.ProcureStuckAt == b.StationID {
		return nil
	}
	want := map[int64]int64{}
	var typeIDs []int64
	for _, it := range shopping {
		if it.Dest == "hull" {
			continue
		}
		if _, ok := want[it.TypeID]; !ok {
			typeIDs = append(typeIDs, it.TypeID)
		}
		want[it.TypeID] += it.Qty
	}
	if len(typeIDs) == 0 {
		return nil
	}
	// The same rules and jump measure findnext's scan uses, cut to craftedDeferJumps.
	r := m.economicRules(ctx, b, startBand(b.CharID, defaultBand(b)))
	reach, err := m.systemsWithin(ctx, b.SystemID, r, craftedDeferJumps)
	if err != nil {
		return nil
	}
	passes := stationFilterPasses(b.Profile, b.Tune)
	if len(passes) == 0 {
		return nil
	}
	scan, err := m.scanMarket(ctx, typeIDs, true, passes[0].scoped(r), reach, m.homeRegionSet(ctx, b), botScope(b))
	if err != nil {
		return nil
	}
	kept := map[[2]int64]bool{}
	for _, s := range dropCraftedDominated(scan.Stops, want, b.Tune) {
		kept[[2]int64{s.StationID, s.TypeID}] = true
	}
	var out map[int64]bool
	for _, s := range scan.Stops {
		if s.StationID == b.StationID && !s.Crafted && !kept[[2]int64{s.StationID, s.TypeID}] {
			if out == nil {
				out = map[int64]bool{}
			}
			out[s.TypeID] = true
		}
	}
	return out
}
