package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// The copy tape: what blueprint copies of each type actually sold for on escrow, how many sold in a week
// and how many are listed now, over every bot's listings (rolebot_bpc_listings). The library prices and
// sizes its copies for sale by it. Before, a copy was listed at its catalogue price: copies that sold
// went for about 146k, the ones that came back unsold had asked about 526k, and the counter filled with
// types nobody was buying.

const (
	// copyTapeWindowSec is how far back the tape's prices reach.
	copyTapeWindowSec = 14 * 86400
	// copyTapeMinSales is how many sales a type needs before its tape price is trusted.
	copyTapeMinSales = 3
	// copyTapeCapWeeks is how many weeks of sales may sit listed at once.
	copyTapeCapWeeks = 2
	// copyReturnStep is the price cut each time a print came back unsold.
	copyReturnStep = 0.85
	// corpCopyMarketShare is the share of the roster's copy sales of a type a library counts on taking,
	// when it weighs research on that type (fillSoldRuns).
	corpCopyMarketShare = 0.25
)

// corpResearchSaleReserve is the share of a research house's production budget kept for copies for sale
// (corpproduction.go).
const corpResearchSaleReserve = 0.25

// researchSaleReserve is what a production think sets aside for copies for sale: corpResearchSaleReserve
// of a positive budget in a research-tagged house, nothing elsewhere. Pure.
func researchSaleReserve(budget float64, researchTagged bool) float64 {
	if !researchTagged || budget <= 0 {
		return 0
	}
	return math.Floor(budget * corpResearchSaleReserve)
}

// copyTape is one blueprint type's line on the tape.
type copyTape struct {
	Sold7  int     // copies sold in the last 7 days
	Sold14 int     // copies sold in the tape window
	PerRun float64 // median sold price per run over the window (0 = too few sales)
	Open   int     // copies listed now
}

// tapeRow is one listing as the tape reads it.
type tapeRow struct {
	TypeID   int64
	PerRun   float64
	Sold     bool
	SoldAt   int64
	Open     bool
	Returned bool
}

// foldCopyTape builds the tape from listings. Pure.
func foldCopyTape(rows []tapeRow, now int64) map[int64]copyTape {
	prices := map[int64][]float64{}
	out := map[int64]copyTape{}
	for _, r := range rows {
		t := out[r.TypeID]
		switch {
		case r.Open:
			t.Open++
		case r.Sold && now-r.SoldAt <= copyTapeWindowSec:
			t.Sold14++
			if now-r.SoldAt <= 7*86400 {
				t.Sold7++
			}
			if r.PerRun > 0 {
				prices[r.TypeID] = append(prices[r.TypeID], r.PerRun)
			}
		}
		out[r.TypeID] = t
	}
	for typeID, ps := range prices {
		if len(ps) < copyTapeMinSales {
			continue
		}
		sort.Float64s(ps)
		t := out[typeID]
		t.PerRun = ps[len(ps)/2]
		out[typeID] = t
	}
	return out
}

// tapeAsk is what a copy of runs runs asks: the tape's median per run when the type has enough sales,
// never above the catalogue price, cut copyReturnStep for each time the print came back unsold. Pure.
func tapeAsk(catalogue float64, t copyTape, runs int64, returns int) float64 {
	ask := catalogue
	if t.PerRun > 0 && runs > 0 {
		ask = math.Min(catalogue, t.PerRun*float64(runs))
		if catalogue <= 0 {
			ask = t.PerRun * float64(runs)
		}
	}
	for i := 0; i < returns; i++ {
		ask *= copyReturnStep
	}
	return math.Floor(ask)
}

// tapeCap is how many copies of a type may be listed at once: copyTapeCapWeeks of its sales, at least
// one. Pure.
func tapeCap(t copyTape) int {
	return max(1, copyTapeCapWeeks*t.Sold7)
}

// tapeRoom is how many more copies of a type may go up now. Pure.
func tapeRoom(t copyTape) int {
	return max(0, tapeCap(t)-t.Open)
}

// copyWantedHalfLife is how fast a remembered miss fades.
const copyWantedHalfLife = 7 * 86400

// copyWantedDedupeSec keeps one builder's repeated look at the same type within a plan pass from counting
// more than once.
const copyWantedDedupeSec = 3600

type copyWantEntry struct {
	n  float64 // decayed misses at `at`
	at int64
}

var (
	copyWantedMu   sync.Mutex
	copyWanted     = map[int64]copyWantEntry{} // bpTypeID -> decayed count
	copyWantedSeen = map[[2]int64]int64{}      // (charID, bpTypeID) -> last noted
)

func init() {
	registerCharForget("copyWantedSeen", func(id int64) {
		copyWantedMu.Lock()
		defer copyWantedMu.Unlock()
		for k := range copyWantedSeen {
			if k[0] == id {
				delete(copyWantedSeen, k)
			}
		}
	})
}

// noteCopyWanted records that a builder looked for a copy of a type on escrow and found none it would
// take. Held in memory: a restart forgets it, and the tape carries the sold side.
func noteCopyWanted(charID, bpTypeID, now int64) {
	copyWantedMu.Lock()
	defer copyWantedMu.Unlock()
	key := [2]int64{charID, bpTypeID}
	if last, ok := copyWantedSeen[key]; ok && now-last < copyWantedDedupeSec {
		return
	}
	copyWantedSeen[key] = now
	e := copyWanted[bpTypeID]
	e.n = decayWanted(e.n, e.at, now) + 1
	e.at = now
	copyWanted[bpTypeID] = e
	if len(copyWantedSeen) > 50000 {
		for k, at := range copyWantedSeen {
			if now-at >= copyWantedDedupeSec {
				delete(copyWantedSeen, k)
			}
		}
	}
}

// decayWanted fades a count noted at `at` to `now`. Pure.
func decayWanted(n float64, at, now int64) float64 {
	if n <= 0 || now <= at {
		return n
	}
	return n * math.Pow(0.5, float64(now-at)/copyWantedHalfLife)
}

// copyWantedWeek is the decayed misses per type, read as a week's unmet purchases.
func copyWantedWeek(now int64) map[int64]float64 {
	copyWantedMu.Lock()
	defer copyWantedMu.Unlock()
	out := make(map[int64]float64, len(copyWanted))
	for t, e := range copyWanted {
		if v := decayWanted(e.n, e.at, now); v >= 0.5 {
			out[t] = v
		}
	}
	return out
}

// copyTapes reads the tape for every type, shared by the roster for a snapshot's life.
func (m *Mod) copyTapes(ctx context.Context) (map[int64]copyTape, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshot(snapKey("bpc-tape"), func() (map[int64]copyTape, error) {
		now := clock.Unix()
		rows, err := m.db.QueryContext(sctx, `
			SELECT l.bpTypeID, l.price / GREATEST(l.runs, 1), e.state, COALESCE(e.dateCompleted, 0)
			  FROM rolebot_bpc_listings l
			  JOIN escrows e ON e.escrowID = l.escrowID
			 WHERE l.isCopy = 1 AND (e.state = 0 OR (e.state = ? AND e.dateCompleted > ?))`,
			evedb.CourierStateDelivered, now-copyTapeWindowSec)
		if err != nil {
			return nil, fmt.Errorf("copy tape: %w", err)
		}
		defer rows.Close()
		var list []tapeRow
		for rows.Next() {
			var r tapeRow
			var state int
			if err := rows.Scan(&r.TypeID, &r.PerRun, &state, &r.SoldAt); err != nil {
				return nil, err
			}
			r.Open = state == 0
			r.Sold = state == evedb.CourierStateDelivered
			list = append(list, r)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return foldCopyTape(list, now), nil
	})
}
