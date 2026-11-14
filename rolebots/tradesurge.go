package rolebots

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// The surge run: carry goods into a region paying well over its usual price
// and list just under what it is paying. Like a dip it flies as a spec run.
//
// Bots cannot talk a surge up. The signal is end-user buying only, net of the
// roster's resale, and a listing goes up under the recent price, so a bot
// reselling to a bot adds nothing and each listing pulls the price down.

const (
	// surgeMinDays and surgeMinUnits are what makes a surge and not one
	// buyer's bad afternoon.
	surgeMinDays  = 2
	surgeMinUnits = 7
	// surgeUndercut is how far under the recent price a run lists.
	surgeUndercut = 0.97
	// surgeRoomDays is how many days of recent buying a lot counts on. Shorter
	// than a listing's usual week, because a surge fades.
	surgeRoomDays = 3
	// surgeConfBase is the belief in a surge listing before the market's
	// record for its category.
	surgeConfBase = 0.6
	// surgeBoardTTL is how long one read of the surges stands.
	surgeBoardTTL = 5 * time.Minute
	// surgeCostTop is how many surges a pilot prices, biggest first.
	surgeCostTop = 40
)

// surging reports whether recent end-user buying makes a surge. Pure.
func surging(r mktRecent, usual, trigger float64) bool {
	return usual > 0 && r.Days >= surgeMinDays && r.Units >= surgeMinUnits && r.Avg >= usual*trigger
}

// surgeListPrice is what a surge run lists at: under the recent price, and
// never past maxMul of the usual one. Pure.
func surgeListPrice(recent, usual, maxMul float64) float64 {
	return roundISK(min(recent*surgeUndercut, usual*maxMul))
}

// surgeRoom is how many of want units a surge lot may carry: a few days of
// recent buying, less what is listed and what other pilots are carrying in.
// Pure.
func surgeRoom(want int64, recentPerDay float64, listed, inFlight int64) int64 {
	return max(min(want, int64(recentPerDay*surgeRoomDays)-listed-inFlight), 0)
}

// surgeLine is one surging market.
type surgeLine struct {
	TypeID, RegionID int64
	Usual, Recent    float64
	PerDay           float64
}

// surgeLines is every surge in the history, biggest premium first. A region an
// NPC sells the type in is left out: its own price impact moves what buyers
// pay there, and that is no shortage. Pure.
func surgeLines(h *mktHistory, trigger float64, seeded func(typeID, regionID int64) bool) []surgeLine {
	var out []surgeLine
	for k, r := range h.Recent {
		usual := h.norm(k[0], k[1])
		if !surging(r, usual, trigger) || seeded(k[0], k[1]) {
			continue
		}
		out = append(out, surgeLine{TypeID: k[0], RegionID: k[1], Usual: usual, Recent: r.Avg,
			PerDay: h.recentPerDay(k[0], k[1])})
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		px, py := (x.Recent-x.Usual)*x.PerDay, (y.Recent-y.Usual)*y.PerDay
		if px != py {
			return px > py
		}
		if x.TypeID != y.TypeID {
			return x.TypeID < y.TypeID
		}
		return x.RegionID < y.RegionID
	})
	return out
}

// surgeBoard is the shared list of surges in the goods a trader carries.
func (m *Mod) surgeBoard(ctx context.Context) ([]surgeLine, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshotFor(snapKey("surge-board"), surgeBoardTTL, func() ([]surgeLine, error) {
		h := m.marketHistory(sctx)
		t := m.npcAnchorTable(sctx)
		if !h.Ready || t == nil {
			return nil, nil
		}
		carried := map[int64]bool{}
		for _, c := range specSupplyCategories {
			carried[c] = true
		}
		lines := surgeLines(h, m.marketPlay(sctx).SurgeTrigger, func(typeID, regionID int64) bool {
			return t.asks[typeID][regionID] > 0
		})
		out := lines[:0:0]
		for _, l := range lines {
			if carried[m.typeCategoryOf(sctx, l.TypeID)] {
				out = append(out, l)
			}
		}
		return out, nil
	})
}

// findSurgeRun is the best surge this pilot can carry goods into. (nil, nil)
// when it does not chase surges or nothing pays.
func (m *Mod) findSurgeRun(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules,
	jumps map[int64]int, budget, hold float64) (*tradeDeal, error) {

	play := m.marketPlay(ctx)
	if !play.Surge || tradeInterest(b.Tune) < specMinInterest || m.freeOrderSlots(ctx, b.CharID) <= 0 {
		return nil, nil
	}
	lines, err := m.surgeBoard(ctx)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	near := map[int64]bool{} // regions inside the run's reach
	for sys, hops := range jumps {
		if hops <= play.SurgeMaxJumps {
			if rg := u.meta[sys].RegionID; rg != 0 {
				near[rg] = true
			}
		}
	}
	hist := m.marketHistory(ctx)
	now := clock.Unix()
	type target struct {
		surgeLine
		station, system int64
	}
	var targets []target
	var typeIDs []int64
	for _, l := range lines {
		if len(targets) >= surgeCostTop {
			break
		}
		if !near[l.RegionID] {
			continue
		}
		st, sys := m.regionSellingStation(ctx, hist, l.TypeID, l.RegionID, 0, jumps, 0)
		if st == 0 {
			continue
		}
		targets = append(targets, target{surgeLine: l, station: st, system: sys})
		typeIDs = append(typeIDs, l.TypeID)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	regions := make([]int64, 0, len(near))
	for rg := range near {
		regions = append(regions, rg)
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i] < regions[j] })
	sources, err := m.specSourcesIn(ctx, b, cfg, r, typeIDs, regions)
	if err != nil {
		return nil, err
	}

	var deals []*tradeDeal
	for _, t := range targets {
		list := surgeListPrice(t.Recent, t.Usual, play.SurgeMaxMul)
		base := m.typeBasePrice(ctx, t.TypeID)
		if overSpiralCap(list, base) {
			continue
		}
		listed := m.regionListedUnits(ctx, t.RegionID, []int64{t.TypeID})[t.TypeID]
		flying := specClaims.inFlight(t.TypeID, t.RegionID, b.CharID, now)
		rates := m.marketRates(ctx, b.CharID, t.station)
		for _, src := range sources[t.TypeID] {
			if _, ok := jumps[src.SystemID]; !ok || src.StationID == t.station {
				continue
			}
			vol := src.UnitVol
			if vol <= 0 {
				vol = 0.01
			}
			units := surgeRoom(min(src.Stock, int64(budget/src.Price), int64(hold/vol)), t.PerDay, listed, flying)
			if units < 1 || !tradeCargoOK(src.CategoryID, units) {
				continue
			}
			if _, isMineral := mineralListFloors[t.TypeID]; isMineral &&
				units < mineralUnitFloor(t.TypeID, src.Price, 1) {
				continue
			}
			profit := specNetProfit(list, src.Price, units, rates)
			if profit < tradeMinProfitISK {
				continue
			}
			d := &tradeDeal{
				Kind: tradeDealSpec, TypeID: t.TypeID, TypeName: m.typeNameOr(ctx, t.TypeID), UnitVol: vol,
				CategoryID: src.CategoryID, BuyStationID: src.StationID, BuySystemID: src.SystemID,
				SellStationID: t.station, SellSystemID: t.system,
				Ask: src.Price, Units: units, Outlay: float64(units) * src.Price, Profit: profit, ListPrice: list,
				Confidence: specVelocityConf(m.specCalibrate(ctx, src.CategoryID, surgeConfBase),
					hist.stationPerDay(t.station, t.TypeID), units),
				Evidence: fmt.Sprintf("a surge at %.0f%% of the usual %.2f", t.Recent/t.Usual*100, t.Usual),
				Jumps:    jumps[src.SystemID] + jumps[t.system],
			}
			d.Rank = tradeRank(d)
			deals = append(deals, d)
		}
	}
	return m.bestTimedRun(ctx, b, cfg, r, jumps, deals, play.SurgeMaxJumps)
}
