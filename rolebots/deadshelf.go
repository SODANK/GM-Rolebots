package rolebots

// Dead shelves: the pull-and-relist loop.
// nudgeOrder pulls a listing nobody bought after three patiences and files the goods back as
// stock at cost. Without a memory of the pull, tradeStockPlan relists at the same station at
// the price that just failed, and findSpecRun can send a fresh run of the same goods to the
// same dock. Each lap costs a broker fee.
// A pull is the market saying not here, not at that price. This file remembers it roster-wide
// for deadShelfWindowSec. findSpecRun skips a destination where the goods were just pulled at
// or under the price the run would list at. tradeStockPlan relists there only under the
// lowest failed price and above a sunk-cost floor; past the floor, or after deadShelfMaxLaps,
// the goods wait for a bid.

import (
	"context"
	"math"
	"sync"
	"time"
)

const (
	// deadShelfWindowSec is how long a pull counts as evidence: the three-patience life of a
	// listing plus the stock cooldown, short enough to retry a dead shelf next week.
	deadShelfWindowSec = 3 * 86400

	// deadShelfTTL is how long the dead-shelf table is cached: one range read of the trade book
	// shared across the roster, instead of per-row queries.
	deadShelfTTL = 10 * time.Minute

	// deadShelfRelistStep: a relist at a dock that refused the goods goes this far under the
	// cheapest price that failed there.
	deadShelfRelistStep = 0.10
	// deadShelfMaxLaps: a dock is given up on after this many pulls in the window.
	deadShelfMaxLaps = 3
	// deadShelfLossFloor is the lowest fraction of its cost a pilot relists salvaged goods at.
	// Under half, the goods are better held for a bid.
	deadShelfLossFloor = 0.5

	// deadShelfSpecMargin: a spec run at a dead shelf is allowed only this far under the price
	// that failed.
	deadShelfSpecMargin = 0.97
)

// deadShelf is the pull evidence for one (type, station).
type deadShelf struct {
	Laps      int     // pulls in the window, any pilot
	FailPrice float64 // the cheapest price a pull happened at
}

var (
	deadShelfMu     sync.Mutex
	deadShelfCur    map[[2]int64]deadShelf
	deadShelfAt     time.Time
	deadShelfLoadMu sync.Mutex
)

// deadShelfOf reads the evidence for one (type, station). Zero value = none.
func (m *Mod) deadShelfOf(ctx context.Context, typeID, stationID int64) deadShelf {
	t := m.deadShelfTable(ctx)
	if t == nil {
		return deadShelf{}
	}
	return t[[2]int64{typeID, stationID}]
}

// deadShelfTable returns the cached table, reloading when stale. A stale
// table keeps serving while one caller reloads; only a cold start waits.
func (m *Mod) deadShelfTable(ctx context.Context) map[[2]int64]deadShelf {
	deadShelfMu.Lock()
	cur, at := deadShelfCur, deadShelfAt
	deadShelfMu.Unlock()
	if cur != nil && clock.Now().Sub(at) < deadShelfTTL {
		return cur
	}
	if cur != nil {
		if !deadShelfLoadMu.TryLock() {
			return cur
		}
	} else {
		deadShelfLoadMu.Lock()
	}
	defer deadShelfLoadMu.Unlock()
	deadShelfMu.Lock()
	if deadShelfCur != nil && clock.Now().Sub(deadShelfAt) < deadShelfTTL {
		cur = deadShelfCur
		deadShelfMu.Unlock()
		return cur
	}
	deadShelfMu.Unlock()

	// A failed read keeps serving what it had and is not retried for a whole TTL, so concurrent
	// callers do not start the same scan again.
	failed := func(format string, err error) map[[2]int64]deadShelf {
		rlogf(format, err)
		keep := cur
		if keep == nil {
			keep = map[[2]int64]deadShelf{}
		}
		deadShelfMu.Lock()
		deadShelfCur, deadShelfAt = keep, clock.Now()
		deadShelfMu.Unlock()
		return keep
	}

	sctx, cancel := snapContext(ctx)
	defer cancel()
	rows, err := m.db.QueryContext(sctx, capStatement(`
		SELECT typeID, stationID, COUNT(*), MIN(price)
		  FROM rolebot_trades
		 WHERE ts > ? AND kind = ? AND note LIKE 'nobody was buying%'
		 GROUP BY typeID, stationID`),
		clock.Unix()-deadShelfWindowSec, tradeKindCancel)
	if err != nil {
		return failed("rolebots: dead shelves: %v", err)
	}
	defer rows.Close()
	out := map[[2]int64]deadShelf{}
	for rows.Next() {
		var typeID, stationID int64
		var d deadShelf
		if err := rows.Scan(&typeID, &stationID, &d.Laps, &d.FailPrice); err != nil {
			return failed("rolebots: dead shelves scan: %v", err)
		}
		out[[2]int64{typeID, stationID}] = d
	}
	if err := rows.Err(); err != nil {
		return failed("rolebots: dead shelves: %v", err)
	}
	deadShelfMu.Lock()
	deadShelfCur, deadShelfAt = out, clock.Now()
	deadShelfMu.Unlock()
	return out
}

// deadShelfRelist is the relist rule, pure. want is what the pilot would ask
// with no history; floor is the lowest it will go (sunk-cost and anchor
// floors already folded in). Returns the price and false when the dock has
// refused these goods enough that it should not be tried again.
func deadShelfRelist(want, floor float64, d deadShelf) (float64, bool) {
	if d.Laps <= 0 {
		return want, true
	}
	if d.Laps >= deadShelfMaxLaps {
		return 0, false
	}
	if d.FailPrice > 0 {
		want = math.Min(want, d.FailPrice*(1-deadShelfRelistStep))
	}
	if want < floor || want <= 0 {
		return 0, false
	}
	return roundISK(want), true
}

// deadShelfBlocksSpec reports whether a spec run listing at list should skip
// a destination the goods were just pulled from.
func deadShelfBlocksSpec(list float64, d deadShelf) bool {
	if d.Laps <= 0 {
		return false
	}
	if d.Laps >= deadShelfMaxLaps {
		return true
	}
	return list >= d.FailPrice*deadShelfSpecMargin
}
