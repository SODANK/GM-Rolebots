package rolebots

// What is on each region's shelves, and how deep its belts go: the two reads
// behind the ESP Mineral Supply page. Both are shared rather than per-pilot,
// because neither question has a pilot in it.

import (
	"context"
	"database/sql"
	"fmt"
)

// ── what a region's belts can make ──────────────────────────────────────────

// regionOreBands is region to the lowest security of any belt-bearing system in
// it, the richest ore table its rock can draw from. oresAtOrAbove is region-blind,
// so alone it credits a region with no lowsec belts with lowsec ore. Derived from
// the graph and belt set in memory, with no query, as regionAdj does (residency.go).
func (u *universe) regionOreBands(ctx context.Context, db *sql.DB) map[int64]float64 {
	u.obandOnce.Do(func() {
		// One call through the accessor runs beltOnce and settles u.belts (nil on a failed
		// load); after that the map is read directly. hasBelts stays the single loader.
		for sysID := range u.meta {
			u.hasBelts(ctx, db, sysID)
			break
		}
		bands := map[int64]float64{}
		for sysID, meta := range u.meta {
			if meta.RegionID == 0 {
				continue
			}
			// A failed belt load leaves u.belts nil. Counting every system then overstates
			// reach, but the alternative would switch the whole duty off silently.
			if u.belts != nil && !u.belts[sysID] {
				continue
			}
			if cur, seen := bands[meta.RegionID]; !seen || meta.Security < cur {
				bands[meta.RegionID] = meta.Security
			}
		}
		u.obands = bands
	})
	return u.obands
}

// ── what is actually on the shelves ─────────────────────────────────────────

// mineralStockByRegion is region to mineral typeID to units resting on that
// region's sell book, from one snapshot entry that has no pilot in its key
// (marketsnap.go). Resting sell orders only; bids are demand. It derives from the
// order book, with no ledger of its own to disagree with it.
func (m *Mod) mineralStockByRegion(ctx context.Context) (map[int64]map[int64]int64, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()

	shared, err := snapshot(snapKey("mineralstock"), func() (map[int64]map[int64]int64, error) {
		rows, err := m.db.QueryContext(sctx, `
			SELECT o.regionID, o.typeID, SUM(o.volRemaining)
			  FROM market_orders o
			 WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
			   AND o.typeID IN (`+placeholders(len(mineralCascade))+`)
			 GROUP BY o.regionID, o.typeID`, int64Args(mineralCascade)...)
		if err != nil {
			return nil, fmt.Errorf("region mineral stock: %w", err)
		}
		defer rows.Close()
		out := map[int64]map[int64]int64{}
		for rows.Next() {
			var regionID, typeID, units int64
			if err := rows.Scan(&regionID, &typeID, &units); err != nil {
				return nil, fmt.Errorf("region mineral stock row: %w", err)
			}
			if out[regionID] == nil {
				out[regionID] = map[int64]int64{}
			}
			out[regionID][typeID] = units
		}
		return out, rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return shared, nil
}


