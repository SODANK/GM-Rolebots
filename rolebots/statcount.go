package rolebots

// Lifetime counters for facts nothing else records: ore mined, jumps, docks and the trade
// record (rolebot_trades is a feed that prunes at 30 days). This file is the module's only
// accumulator, behind rolebot_stats. bumpStats folds each delta into a map under a mutex and a
// tick flushes it, because counter deltas are additive and merge (a queue suits distinct facts,
// not arithmetic). An unclean exit loses up to one flush interval, since modapi has no shutdown
// hook. The numbers are a leaderboard, not a ledger.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

const (
	// statsFlushEveryTicks is the flush period, 30 seconds. It also sets the loss window after an
	// unclean exit.
	statsFlushEveryTicks = 30

	// statsPendingMax caps how many distinct characters may wait on a flush, so a wedged flusher
	// costs accuracy rather than memory. A real roster does not reach it.
	statsPendingMax = 16384

	// statsUpsertChunk is how many characters go in one INSERT, so a flush is not a round trip per
	// bot.
	statsUpsertChunk = 200
)

// statDelta is one character's unflushed arithmetic. Every field is a sum except BestTrade,
// which is a maximum (see statUpsertTail).
type statDelta struct {
	Jumps        int64
	Docks        int64
	OreUnits     int64
	OreM3        float64
	Trades       int64
	Buys         int64
	Sells        int64
	Turnover     float64
	Profit       float64
	BestTrade    float64
	OrdersPlaced int64
	FeesPaid     float64

	firstAt int64
	lastAt  int64
}

var (
	statMu       sync.Mutex
	statPending  = map[int64]*statDelta{}
	statDropped  atomic.Int64
	statReported atomic.Int64
)

// bumpStats folds one event into the pending map. It never blocks and never touches the
// database (no context, no error, like queueLogRow), because sim workers, the grid lane and the
// embodied mining goroutine call it. A character beyond the cap is dropped and counted; the
// flusher reports the total.
func bumpStats(charID int64, f func(*statDelta)) {
	if charID == 0 || f == nil {
		return
	}
	now := clock.Unix()

	statMu.Lock()
	defer statMu.Unlock()

	d, ok := statPending[charID]
	if !ok {
		if len(statPending) >= statsPendingMax {
			statDropped.Add(1)
			return
		}
		d = &statDelta{firstAt: now}
		statPending[charID] = d
	}
	f(d)
	d.lastAt = now
}

// forgetStats drops a character's pending arithmetic. The purge paths call it: a flush after
// the DELETE would recreate the row, and a later batch that reuses the sequence number would
// inherit a dead bot's totals.
func forgetStats(charID int64) {
	statMu.Lock()
	delete(statPending, charID)
	statMu.Unlock()
}

// ── the flusher ─────────────────────────────────────────────────────────────

var statsFlushBusy = make(chan struct{}, 1)

// startStatsWriter registers the flush tick. It is not gated on Master(): counts gathered
// before the master switch went off are real work and are still flushed.
func (m *Mod) startStatsWriter() {
	m.registerPeriodic("rolebot-stats-flush", statsFlushEveryTicks, func(uint64) bool {
		select {
		case statsFlushBusy <- struct{}{}:
		default:
			return true
		}
		goSafe("stats flush", func() {
			defer func() { <-statsFlushBusy }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			m.flushStats(ctx)
		})
		return true
	})
}

// flushStats swaps the pending map out under the lock and writes outside it, so a bot taking
// its turn during a slow flush accumulates into the new map.
func (m *Mod) flushStats(ctx context.Context) {
	statMu.Lock()
	pending := statPending
	statPending = map[int64]*statDelta{}
	statMu.Unlock()

	if len(pending) == 0 {
		return
	}
	if n := statDropped.Load(); n > statReported.Load() {
		statReported.Store(n)
		rlogf("rolebots: stat counters — %d character(s) dropped so far", n)
	}

	ids := make([]int64, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	for start := 0; start < len(ids); start += statsUpsertChunk {
		end := start + statsUpsertChunk
		if end > len(ids) {
			end = len(ids)
		}
		if err := m.writeStatDeltas(ctx, ids[start:end], pending); err != nil {
			rlogf("rolebots: stat flush (%d rows): %v", end-start, err)
		}
	}
}

// statUpsertTail is the on-duplicate-key clause. Every counter adds; bestTrade takes the larger
// of the two (a single trade's profit is not cumulative); firstAt is written once and lastAt only
// moves forward.
const statUpsertTail = ` ON DUPLICATE KEY UPDATE
	jumps        = jumps        + VALUES(jumps),
	docks        = docks        + VALUES(docks),
	oreUnits     = oreUnits     + VALUES(oreUnits),
	oreM3        = oreM3        + VALUES(oreM3),
	trades       = trades       + VALUES(trades),
	buys         = buys         + VALUES(buys),
	sells        = sells        + VALUES(sells),
	turnover     = turnover     + VALUES(turnover),
	profit       = profit       + VALUES(profit),
	bestTrade    = GREATEST(bestTrade, VALUES(bestTrade)),
	ordersPlaced = ordersPlaced + VALUES(ordersPlaced),
	feesPaid     = feesPaid     + VALUES(feesPaid),
	firstAt      = IF(firstAt = 0, VALUES(firstAt), firstAt),
	lastAt       = GREATEST(lastAt, VALUES(lastAt))`

func (m *Mod) writeStatDeltas(ctx context.Context, ids []int64, pending map[int64]*statDelta) error {
	if len(ids) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`INSERT INTO rolebot_stats
		(characterID, jumps, docks, oreUnits, oreM3, trades, buys, sells,
		 turnover, profit, bestTrade, ordersPlaced, feesPaid, firstAt, lastAt)
		VALUES `)
	args := make([]any, 0, len(ids)*15)
	// Counted rather than indexed off the range: a skipped id must not leave a leading comma, and
	// an all-skipped chunk must not produce an empty VALUES clause.
	wrote := 0
	for _, id := range ids {
		d := pending[id]
		if d == nil {
			continue
		}
		if wrote > 0 {
			b.WriteString(",")
		}
		b.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
		args = append(args, id, d.Jumps, d.Docks, d.OreUnits, d.OreM3,
			d.Trades, d.Buys, d.Sells, d.Turnover, d.Profit, d.BestTrade,
			d.OrdersPlaced, d.FeesPaid, d.firstAt, d.lastAt)
		wrote++
	}
	if wrote == 0 {
		return nil
	}
	b.WriteString(statUpsertTail)
	if _, err := m.db.ExecContext(ctx, b.String(), args...); err != nil {
		return err
	}
	return nil
}

// ── the one-shot trade seed ─────────────────────────────────────────────────

// seedTradeCounters folds what is left of the trade feed into the lifetime trade columns, once.
// It uses bookTrade's arithmetic, skipping 'haul': that kind is booked in addition to its sell leg
// and would double turnover. It must run after ensureTables and before the sim, the trade sweeps
// and the stats flusher start, or a leg booked between the SELECT and the INSERT counts twice.
func (m *Mod) seedTradeCounters(ctx context.Context) {
	if _, ok, err := evedb.GetServerSetting(ctx, m.db, settingStatsSeed); err != nil {
		rlogf("rolebots: read stats seed marker: %v (skipping seed)", err)
		return
	} else if ok {
		return
	}

	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_stats
			(characterID, trades, buys, sells, turnover, profit, bestTrade,
			 ordersPlaced, feesPaid, firstAt, lastAt)
		SELECT t.characterID,
		       COALESCE(SUM(t.kind IN ('buy','sell','fill')), 0),
		       COALESCE(SUM(t.kind =  'buy'), 0),
		       COALESCE(SUM(t.kind IN ('sell','fill')), 0),
		       COALESCE(SUM(CASE WHEN t.kind IN ('buy','sell','fill') THEN t.value  END), 0),
		       COALESCE(SUM(CASE WHEN t.kind IN ('sell','fill','fee')  THEN t.profit END), 0),
		       COALESCE(MAX(CASE WHEN t.kind IN ('sell','fill')        THEN t.profit END), 0),
		       COALESCE(SUM(t.kind = 'list'), 0),
		       COALESCE(SUM(CASE WHEN t.kind = 'fee' THEN t.value END), 0),
		       COALESCE(MIN(t.ts), 0),
		       COALESCE(MAX(t.ts), 0)
		  FROM rolebot_trades t
		  JOIN rolebot_characters rc ON rc.characterID = t.characterID
		 GROUP BY t.characterID`+statUpsertTail)
	if err != nil {
		rlogf("rolebots: seed trade counters: %v", err)
		return
	}

	// The marker stores the newest ts the seed consumed, so what the one-shot covered can be
	// answered later. An absent feed marks 0.
	var newest sql.NullInt64
	if err := m.db.QueryRowContext(ctx,
		`SELECT MAX(ts) FROM rolebot_trades`).Scan(&newest); err != nil {
		rlogf("rolebots: seed trade counters: read horizon: %v", err)
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingStatsSeed,
		fmt.Sprintf("%d", newest.Int64)); err != nil {
		// Left unmarked, the seed runs again next boot and double-counts.
		// Loud, because the fix is a hand-written server_settings row.
		rlogf("rolebots: seed trade counters: MARKER NOT WRITTEN (%v) — "+
			"set %s to %d by hand or the next boot will double-count",
			err, settingStatsSeed, newest.Int64)
		return
	}
	if res != nil {
		if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
			rlogf("rolebots: seeded lifetime trade counters from the feed (%d row(s) touched)", n)
		}
	}
}

func init() { registerCharForget("statPending", forgetStats) }