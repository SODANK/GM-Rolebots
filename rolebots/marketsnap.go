package rolebots

// Shared market snapshots.
//
// Three reads have no pilot in them: the gap pass behind the trade sweep, the
// procurement scan of what a set of types costs at every station, and the
// competition count of stations selling a set of types. A sim pass steps eight
// workers at once, so each was asked many times together over market_orders.
// This file computes each once and the roster reads the answer. Entries are
// snapshots, not a view: they share tradeScanCache's TTL, the housekeeping
// pass sweeps them, and they may be up to a sweep stale. The order engine
// re-checks every price at match time.

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// marketSnapTTL matches tradeScanTTL: these are the same sweep, split into
// the parts that differ per pilot and the parts that do not.
const marketSnapTTL = tradeScanTTL

// snapReadBudget bounds a shared read. It does not depend on the pilot that
// asked first, since the whole roster waits on one query, and it has a ceiling
// so a stuck read cannot hold every waiter and a pool connection.
const snapReadBudget = 30 * time.Second

// snapContext detaches a shared read from the calling pilot's clock.
func snapContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), snapReadBudget)
}

// snapStatementCap is snapReadBudget as the server enforces it, a little inside
// the client deadline. Without it MariaDB keeps running a statement whose client
// gave up.
const snapStatementCap = snapReadBudget - 2*time.Second

// capStatement prefixes a read with MariaDB's per-statement time limit. Works
// through server-side prepare, so bind parameters are unaffected. Reads only: an
// interrupted capped write rolls back.
func capStatement(q string) string { return capStatementFor(snapStatementCap, q) }

// capStatementFor is the same with the caller's own limit, for a read whose
// deadline is not the snapshot budget (the log pages run under a 15 s
// handler; the chokes fold under 90 s).
func capStatementFor(limit time.Duration, q string) string {
	secs := int(limit / time.Second)
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("SET STATEMENT max_statement_time=%d FOR %s", secs, q)
}

// snapFailBackoff is how long a failed shared read is answered from memory with
// its error before it is tried again, so the next pilot does not restart a slow
// read. Kept well under marketSnapTTL.
const snapFailBackoff = 30 * time.Second

var (
	snapMu      sync.Mutex
	snapCache   = map[string]snapRecord{}
	snapFlights = map[string]*snapCall{}
)

type snapRecord struct {
	at  time.Time
	ttl time.Duration // how long val stands; marketSnapTTL unless snapshotFor said otherwise
	val any
	err error // a failed read, held for snapFailBackoff
}

// snapCall is one read in flight. On a cold or just-expired entry all eight
// workers would otherwise start the same scan before any has an answer. The first
// caller runs the read; the rest wait on it and take its result.
type snapCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

// snapshot is the read-through with single-flight. load is called at most once
// per key per TTL across the roster. The stored value is shared by every waiter,
// so a loader returns a value its callers will not mutate, and the accessors
// below hand out copies.
func snapshot[T any](key string, load func() (T, error)) (T, error) {
	return snapshotFor(key, marketSnapTTL, load)
}

// snapshotFor is snapshot with its own TTL, for reads whose answer moves on a
// slower clock than the live book.
func snapshotFor[T any](key string, ttl time.Duration, load func() (T, error)) (T, error) {
	var zero T
	snapMu.Lock()
	if e, ok := snapCache[key]; ok && e.err != nil && clock.Now().Sub(e.at) < snapFailBackoff {
		snapMu.Unlock()
		return zero, e.err
	}
	if e, ok := snapCache[key]; ok && e.err == nil && clock.Now().Sub(e.at) < e.ttl {
		snapMu.Unlock()
		v, ok := e.val.(T)
		if !ok {
			return zero, fmt.Errorf("market snapshot %s: cached %T", key, e.val)
		}
		return v, nil
	}
	if c, ok := snapFlights[key]; ok {
		snapMu.Unlock()
		c.wg.Wait()
		if c.err != nil {
			return zero, c.err
		}
		v, ok := c.val.(T)
		if !ok {
			return zero, fmt.Errorf("market snapshot %s: in flight %T", key, c.val)
		}
		return v, nil
	}
	c := &snapCall{}
	c.wg.Add(1)
	snapFlights[key] = c
	snapMu.Unlock()

	v, err := load()
	c.val, c.err = v, err
	c.wg.Done()

	snapMu.Lock()
	delete(snapFlights, key)
	if err == nil {
		snapCache[key] = snapRecord{at: clock.Now(), ttl: ttl, val: v}
	} else {
		snapCache[key] = snapRecord{at: clock.Now(), err: err}
	}
	snapMu.Unlock()
	return v, err
}

// sellerCount is one (type, system) competition row, before the asking
// pilot's own travel horizon is applied to it.
type sellerCount struct {
	TypeID   int64
	SystemID int64
	Stations int
}

// snapKey folds a query's identifying parts into a short key. The parts are
// hashed because the typeID lists behind these scans run to hundreds of entries
// and would otherwise be stored verbatim as keys.
func snapKey(parts ...any) string {
	h := fnv.New64a()
	for _, p := range parts {
		fmt.Fprintf(h, "%v\x00", p)
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

// sweepMarketSnaps drops expired snapshots. Registered on the module's cache
// sweep, because the read path's expiry test never frees an entry whose key
// nobody asks for again.
func sweepMarketSnaps() {
	now := clock.Now()
	snapMu.Lock()
	defer snapMu.Unlock()
	for k, e := range snapCache {
		if now.Sub(e.at) >= max(e.ttl, snapFailBackoff) {
			delete(snapCache, k)
		}
	}
}

// ── the gap pass ────────────────────────────────────────────────────────────

// gapTypes returns the typeIDs where the best bid anywhere beats the best ask by
// at least margin. The aggregate over all active orders is shared; each pilot
// applies its own margin (gapAbove). regions is the sorted residency scope
// (residency.go regionIDs), so residents of one region share a key; empty is the whole book.
func (m *Mod) gapTypes(ctx context.Context, margin float64, regions []int64) ([]int64, error) {
	// The margin is not in the key: it is pilot tuning (Tune.TradeMinMarginFrac),
	// and keying on it gave every trader its own copy. Each pilot applies it in Go.
	key := snapKey("gap", regions)
	shared, err := snapshot(key, func() ([]gapRatio, error) {
		book, err := m.gapBook(ctx)
		if err != nil {
			return nil, err
		}
		return gapFromBook(book, regions), nil
	})
	if err != nil {
		return nil, err
	}
	return gapAbove(shared, margin), nil
}

// gapBookRow is one (type, region) cell of the live book: its best bid and best ask.
type gapBookRow struct {
	TypeID, RegionID int64
	Bid, Ask         float64
	HasBid, HasAsk   bool
}

// gapBook is the best bid and ask per type and region over every active order: one scan of
// the book per snapshot TTL, from which every residency scope's gap pass is folded in Go
// (gapFromBook). Scoped in SQL, each distinct scope re-read the whole book, since the
// order-state index does not carry the region.
func (m *Mod) gapBook(ctx context.Context) ([]gapBookRow, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshot(snapKey("gapbook"), func() ([]gapBookRow, error) {
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT o.typeID, o.regionID,
			       MAX(CASE WHEN o.bid = 1 THEN o.price END),
			       MIN(CASE WHEN o.bid = 0 THEN o.price END)
			FROM market_orders o
			WHERE o.orderState = 1 AND o.volRemaining > 0
			GROUP BY o.typeID, o.regionID`))
		if err != nil {
			return nil, fmt.Errorf("gap pass: %w", err)
		}
		defer rows.Close()
		var out []gapBookRow
		for rows.Next() {
			var r gapBookRow
			var bid, ask sql.NullFloat64
			if err := rows.Scan(&r.TypeID, &r.RegionID, &bid, &ask); err != nil {
				return nil, fmt.Errorf("gap pass row: %w", err)
			}
			r.Bid, r.HasBid = bid.Float64, bid.Valid
			r.Ask, r.HasAsk = ask.Float64, ask.Valid
			out = append(out, r)
		}
		return out, rows.Err()
	})
}

// gapFromBook folds the per-region book into the gap pass for one scope (empty = every
// region): per type, the best bid and best ask across the scope, kept when the bid beats
// the ask. The ratio is bid/ask, or 1e18 for a zero ask (it passes any margin), as the
// scoped SQL computed it. Sorted by typeID, as the SQL's GROUP BY returned it.
func gapFromBook(book []gapBookRow, regions []int64) []gapRatio {
	var in map[int64]bool
	if len(regions) > 0 {
		in = make(map[int64]bool, len(regions))
		for _, r := range regions {
			in[r] = true
		}
	}
	type best struct {
		bid, ask       float64
		hasBid, hasAsk bool
	}
	byType := map[int64]*best{}
	for _, r := range book {
		if in != nil && !in[r.RegionID] {
			continue
		}
		b := byType[r.TypeID]
		if b == nil {
			b = &best{}
			byType[r.TypeID] = b
		}
		if r.HasBid && (!b.hasBid || r.Bid > b.bid) {
			b.bid, b.hasBid = r.Bid, true
		}
		if r.HasAsk && (!b.hasAsk || r.Ask < b.ask) {
			b.ask, b.hasAsk = r.Ask, true
		}
	}
	out := make([]gapRatio, 0, len(byType)/8)
	for id, b := range byType {
		if !b.hasBid || !b.hasAsk || b.bid <= b.ask {
			continue
		}
		ratio := 1e18
		if b.ask != 0 {
			ratio = b.bid / b.ask
		}
		out = append(out, gapRatio{TypeID: id, Ratio: ratio})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TypeID < out[j].TypeID })
	return out
}

// gapRatio is one type's best bid over its best ask, for every type where
// that exceeds 1.
type gapRatio struct {
	TypeID int64
	Ratio  float64
}

// gapAbove is the per-pilot half of the gap pass: the types whose ratio clears
// 1+margin, by a strict test.
func gapAbove(all []gapRatio, margin float64) []int64 {
	var out []int64
	for _, g := range all {
		if g.Ratio > 1+margin {
			out = append(out, g.TypeID)
		}
	}
	return out
}

// ── the station scan ────────────────────────────────────────────────────────

// scanStops is the raw half of scanMarket: best price and whole book per station
// for a set of types on one side, with no pilot in it. The key is the side, the
// station filter and the types, so bots with one standings profile share the
// read. The reach annotation (jumps, regions, blocked stations) stays per pilot.
func (m *Mod) scanStops(ctx context.Context, typeIDs []int64, buying bool,
	f stationFilter) ([]marketStop, error) {

	// Bots buy from sell orders (bid = 0, cheapest wins) and sell into
	// bids (bid = 1, dearest wins).
	side, agg := "1", "MAX"
	if buying {
		side, agg = "0", "MIN"
	}
	key := snapKey("scan", side, f.Clause, f.Args, typeIDs)
	sctx, cancel := snapContext(ctx)
	defer cancel()

	shared, err := snapshot(key, func() ([]marketStop, error) {
		// The buy side adds the cheapest crafted ask (any seller but the NPC seeders),
		// which marks a stop Crafted (reach.go craftedEdge). The sell side reads NULL.
		crafted := `NULL`
		var args []any
		if buying {
			crafted = `MIN(IF(o.charID NOT IN (?, ?, ?), o.price, NULL))`
			args = []any{evedb.SeederCharID, evedb.AdminSeederCharID, evedb.ResaleSeederCharID}
		}
		args = append(args, int64Args(typeIDs)...)
		args = append(args, f.Args...)
		var cats map[int64]int64
		if buying {
			// An unreadable catalogue marks nothing crafted.
			cats, _ = m.typeCategories(sctx)
		}
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT o.typeID, o.stationID, s.solarSystemID,
			       `+agg+`(o.price), SUM(o.volRemaining), `+crafted+`
			FROM market_orders o
			JOIN staStations s ON s.stationID = o.stationID
			WHERE o.typeID IN (`+placeholders(len(typeIDs))+`)
			  AND o.bid = `+side+` AND o.orderState = 1 AND o.volRemaining > 0`+f.Clause+`
			GROUP BY o.typeID, o.stationID, s.solarSystemID`), args...)
		if err != nil {
			return nil, fmt.Errorf("market scan: %w", err)
		}
		defer rows.Close()

		var stops []marketStop
		for rows.Next() {
			var s marketStop
			var craftedAsk sql.NullFloat64
			if err := rows.Scan(&s.TypeID, &s.StationID, &s.SystemID, &s.Price, &s.Volume, &craftedAsk); err != nil {
				return nil, fmt.Errorf("market scan row: %w", err)
			}
			s.Crafted = craftedBest(buying, craftedAsk, s.Price) && leakCategories[cats[s.TypeID]]
			stops = append(stops, s)
		}
		return stops, rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// A copy: the caller annotates each stop with its own jumps, region and
	// preference, and the shared rows must not carry one pilot's horizon to the next.
	out := make([]marketStop, len(shared))
	copy(out, shared)
	return out, nil
}

// craftedBest reports whether a station's best ask is a crafted one: the
// cheapest non-seeder ask exists and is no dearer than the cheapest ask of all.
// Buy side only, since a bid is never crafted. Pure.
func craftedBest(buying bool, craftedAsk sql.NullFloat64, best float64) bool {
	return buying && craftedAsk.Valid && craftedAsk.Float64 > 0 && craftedAsk.Float64 <= best
}

// ── the competition count ───────────────────────────────────────────────────

// sellerStations counts, per (type, system), how many stations carry a live sell
// order. It has no pilot in it: sellersNear applies the horizon test in Go, so
// factory bots asking about the same types share the rowset.
func (m *Mod) sellerStations(ctx context.Context, typeIDs []int64) ([]sellerCount, error) {
	key := snapKey("sellers", typeIDs)
	sctx, cancel := snapContext(ctx)
	defer cancel()

	// No copy on the way out: sellersNear only reads these rows. A caller that
	// annotates them takes a copy first.
	return snapshot(key, func() ([]sellerCount, error) {
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT o.typeID, s.solarSystemID, COUNT(DISTINCT o.stationID)
			FROM market_orders o
			JOIN staStations s ON s.stationID = o.stationID
			WHERE o.typeID IN (`+placeholders(len(typeIDs))+`)
			  AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
			GROUP BY o.typeID, s.solarSystemID`), int64Args(typeIDs)...)
		if err != nil {
			return nil, fmt.Errorf("sellers near: %w", err)
		}
		defer rows.Close()

		var out []sellerCount
		for rows.Next() {
			var c sellerCount
			if err := rows.Scan(&c.TypeID, &c.SystemID, &c.Stations); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, rows.Err()
	})
}

// ── galaxy-wide reads the build book shares ─────────────────────────────────

// sysStations is one system's count of stations selling a type, and how many
// units they have listed. Units feeds the flood rule (productFlooded): a station
// count says a type is sold nearby, the units say whether the shelf is glutted.
type sysStations struct {
	SystemID int64
	RegionID int64
	Stations int
	Units    int64
	// The same two over crafted asks only (every seller but the NPC seeders): the
	// leak tier's competition (buildbook.go). An NPC shelf is not a rival to it.
	CraftedStations int
	CraftedUnits    int64
}

// allSellerStations is sellerStations for every type at once, indexed by type. A
// build book's typeID list is its own, so no two suppliers shared that read; one
// aggregate over the whole sell book is shared by the roster per marketSnapTTL.
func (m *Mod) allSellerStations(ctx context.Context) (map[int64][]sysStations, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshot(snapKey("sellers-all"), func() (map[int64][]sysStations, error) {
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT o.typeID, s.solarSystemID, s.regionID, COUNT(DISTINCT o.stationID), SUM(o.volRemaining),
			       COUNT(DISTINCT IF(o.charID NOT IN (?, ?, ?), o.stationID, NULL)),
			       COALESCE(SUM(IF(o.charID NOT IN (?, ?, ?), o.volRemaining, 0)), 0)
			FROM market_orders o
			JOIN staStations s ON s.stationID = o.stationID
			WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
			GROUP BY o.typeID, s.solarSystemID, s.regionID`),
			evedb.SeederCharID, evedb.AdminSeederCharID, evedb.ResaleSeederCharID,
			evedb.SeederCharID, evedb.AdminSeederCharID, evedb.ResaleSeederCharID)
		if err != nil {
			return nil, fmt.Errorf("all sellers: %w", err)
		}
		defer rows.Close()
		out := map[int64][]sysStations{}
		for rows.Next() {
			var typeID int64
			var s sysStations
			if err := rows.Scan(&typeID, &s.SystemID, &s.RegionID, &s.Stations, &s.Units,
				&s.CraftedStations, &s.CraftedUnits); err != nil {
				return nil, err
			}
			out[typeID] = append(out[typeID], s)
		}
		return out, rows.Err()
	})
}

// allAskByRegion is the cheapest live ask per (type, region), for the whole book
// in one read. A pilot's valuation is the minimum over its own regions, as
// estimatePriceRefIn computes one type at a time, so the book's price and the
// gate's price share a scope.
func (m *Mod) allAskByRegion(ctx context.Context) (map[int64]map[int64]float64, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshot(snapKey("asks-by-region"), func() (map[int64]map[int64]float64, error) {
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT typeID, regionID, MIN(price)
			FROM market_orders
			WHERE bid = 0 AND orderState = 1 AND volRemaining > 0 AND price > 0
			GROUP BY typeID, regionID`))
		if err != nil {
			return nil, fmt.Errorf("asks by region: %w", err)
		}
		defer rows.Close()
		out := map[int64]map[int64]float64{}
		for rows.Next() {
			var typeID, regionID int64
			var p float64
			if err := rows.Scan(&typeID, &regionID, &p); err != nil {
				return nil, err
			}
			byRegion := out[typeID]
			if byRegion == nil {
				byRegion = map[int64]float64{}
				out[typeID] = byRegion
			}
			byRegion[regionID] = p
		}
		return out, rows.Err()
	})
}

// scopedAsk is the cheapest ask for one type inside a region set, read off the
// allAskByRegion snapshot. An empty set reads the whole map. 0 means no ask.
func scopedAsk(asks map[int64]map[int64]float64, typeID int64, regions []int64) float64 {
	byRegion := asks[typeID]
	if len(byRegion) == 0 {
		return 0
	}
	best := 0.0
	take := func(p float64) {
		if p > 0 && (best == 0 || p < best) {
			best = p
		}
	}
	if len(regions) == 0 {
		for _, p := range byRegion {
			take(p)
		}
		return best
	}
	for _, r := range regions {
		take(byRegion[r])
	}
	return best
}

func init() { registerSweep("marketSnaps", sweepMarketSnaps) }
