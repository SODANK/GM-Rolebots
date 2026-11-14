package rolebots

import (
	"context"
	"fmt"
	"time"
)

// Where a miner can sell: the belt search's view of the ore buyers.
//
// A miner's hold is worth something only once sold, and miningDispose hauls to a
// bid no further than oreSellMaxJumps away; past that the load is stockpiled.
// beltCost charges a belt with no buyer in reach (beltNoBidCost), and this is
// the set it reads: every system holding a live ore bid (NPC ore demand, corp
// buybacks, bot bids), then a multi-source walk out to oreSellMaxJumps. The
// answer is pilot-independent (the standings filter narrows it a little; the
// penalty is a nudge, not a gate) and cached for oreBidReachTTL.

const oreBidReachTTL = 15 * time.Minute

// oreBidReach is the set of systems within oreSellMaxJumps of a live ore bid.
// The returned map is shared by every caller and must not be modified.
func (m *Mod) oreBidReach(ctx context.Context, u *universe) (map[int64]bool, error) {
	if u == nil {
		return nil, nil
	}
	return snapshotFor(snapKey("orebidreach"), oreBidReachTTL, func() (map[int64]bool, error) {
		sctx, cancel := snapContext(ctx)
		defer cancel()
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT DISTINCT s.solarSystemID
			FROM market_orders o
			JOIN staStations s ON s.stationID = o.stationID
			JOIN invTypes t ON t.typeID = o.typeID
			JOIN invGroups g ON g.groupID = t.groupID
			WHERE o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
			  AND g.categoryID = ?`), catAsteroid)
		if err != nil {
			return nil, fmt.Errorf("ore bid systems: %w", err)
		}
		defer rows.Close()
		var seeds []int64
		for rows.Next() {
			var sysID int64
			if err := rows.Scan(&sysID); err != nil {
				return nil, err
			}
			seeds = append(seeds, sysID)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return systemsNear(u.adj, seeds, oreSellMaxJumps), nil
	})
}

// systemsNear is every system within maxJumps of any seed, the seeds
// included. A breadth-first walk from all seeds at once. Pure.
func systemsNear(adj map[int64][]int64, seeds []int64, maxJumps int) map[int64]bool {
	out := make(map[int64]bool, len(seeds))
	frontier := make([]int64, 0, len(seeds))
	for _, s := range seeds {
		if !out[s] {
			out[s] = true
			frontier = append(frontier, s)
		}
	}
	for depth := 0; depth < maxJumps && len(frontier) > 0; depth++ {
		var next []int64
		for _, sys := range frontier {
			for _, nb := range adj[sys] {
				if !out[nb] {
					out[nb] = true
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	return out
}

// searchHoldFillSec is how long this pilot's hold takes to fill on ordinary
// rock, read for the belt search (beltCost, oreJumpWorthFor). Veldspar is the
// reference because every mining laser works it at full rate. Returns 0 for a
// pilot with no ship or no miner fitted, or on any failure.
func (m *Mod) searchHoldFillSec(ctx context.Context, b *botRun) float64 {
	if b == nil || b.ShipID == 0 {
		return 0
	}
	oreID, _, err := m.oreType(ctx, "Veldspar")
	if err != nil {
		return 0
	}
	_, fillSec, _, err := m.miningYield(ctx, b, oreID)
	if err != nil || fillSec <= 0 {
		return 0
	}
	return float64(fillSec)
}
