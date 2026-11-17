package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// Market liquidity: the one fact about a market that trade decisions read.
// It is what has changed hands at a station (market_transactions), decayed on a
// half-life, skipping trades with an NPC seeder on either side. Readers use it
// through liquidityValue, a log, so a busy market pulls without owning the rank.
// Nothing the roster does by living somewhere counts toward it.
//
// The sweep is incremental on the primary key: it walks forward from a stored
// cursor in bounded ID windows, weights each row by its age and adds it to a
// per-station ledger. A transaction committed behind the cursor is missed.

const (
	// liquidityHalfLifeSec is three and a half days: the ledger reads as about the
	// last week.
	liquidityHalfLifeSec = 3.5 * 86400.0
	// liquidityBootstrapSec is how far back a fresh ledger starts reading: four
	// half-lives, since older rows would weigh under 7%.
	liquidityBootstrapSec = 14 * 86400
	// liquidityWindowIDs is how many transactionIDs one batch covers, and
	// liquidityBatches how many batches one sweep runs, so a first boot catches up
	// over a few sweeps without one long scan.
	liquidityWindowIDs = 250_000
	liquidityBatches   = 4
	// liquidityTTL is how long a reader's copy stands. The ledger moves on a
	// five-minute sweep.
	liquidityTTL = 60 * time.Second
)

// liquidityExcluded is every NPC seeder character. A trade with one of them
// on either side is furniture, not a market.
var liquidityExcluded = fmt.Sprintf("%d, %d, %d",
	evedb.SeederCharID, evedb.AdminSeederCharID, evedb.ResaleSeederCharID)

// liquidityValue is what a reader does with a raw ISK figure: a log of the
// millions, so liquidity bends a ranking rather than owning it. Zero for a
// market nothing has traded in.
func liquidityValue(isk float64) float64 {
	if isk <= 0 {
		return 0
	}
	return math.Log1p(isk / 1e6)
}

// refreshLiquidity is the sweep's pass: age the ledger, then read forward.
func (m *Mod) refreshLiquidity(ctx context.Context) error {
	now := clock.Unix()
	factor := fmt.Sprintf("POWER(2, -GREATEST(0, %d - decayedAt) / %f)", now, liquidityHalfLifeSec)
	for _, q := range []string{
		`UPDATE rolebot_station_liquidity
		    SET isk = isk * ` + factor + `, trades = trades * ` + factor + `,
		        decayedAt = ` + fmt.Sprint(now),
		`DELETE FROM rolebot_station_liquidity WHERE isk < 1000 AND trades < 0.01`,
	} {
		if _, err := m.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("liquidity decay: %w", err)
		}
	}

	cursor, err := m.liquidityCursor(ctx, now)
	if err != nil {
		return err
	}
	var maxID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(transactionID), 0) FROM market_transactions`).Scan(&maxID); err != nil {
		return fmt.Errorf("liquidity head: %w", err)
	}
	// Each row is weighted by its age against the same half-life as the decay
	// above, so a row read late lands where it would have if read when written.
	nowFT := evedb.UnixToWin32(now)
	weight := fmt.Sprintf("POWER(2, -GREATEST(0, %d - CAST(t.transactionDateTime AS SIGNED)) / %f)",
		nowFT, liquidityHalfLifeSec*1e7)
	for i := 0; i < liquidityBatches && cursor < maxID; i++ {
		hi := cursor + liquidityWindowIDs
		if hi > maxID {
			hi = maxID
		}
		// transactionType 1 is the buyer's row: one row per trade, since counting both
		// sides would count every trade twice.
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_station_liquidity (stationID, systemID, isk, trades, decayedAt, lastAt)
			SELECT a.stationID, s.solarSystemID, a.addISK, a.addN, ?, ?
			  FROM (SELECT t.stationID,
			               SUM(t.price * t.quantity * `+weight+`) AS addISK,
			               SUM(`+weight+`) AS addN
			          FROM market_transactions t
			         WHERE t.transactionID > ? AND t.transactionID <= ?
			           AND t.transactionType = 1
			           AND t.characterID NOT IN (`+liquidityExcluded+`)
			           AND t.clientID NOT IN (`+liquidityExcluded+`)
			         GROUP BY t.stationID) a
			  JOIN staStations s ON s.stationID = a.stationID
			ON DUPLICATE KEY UPDATE
			    isk = rolebot_station_liquidity.isk + VALUES(isk),
			    trades = rolebot_station_liquidity.trades + VALUES(trades),
			    lastAt = VALUES(lastAt)`,
			now, now, cursor, hi); err != nil {
			return fmt.Errorf("liquidity batch %d..%d: %w", cursor, hi, err)
		}
		cursor = hi
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_liquidity_cursor (id, lastTxID) VALUES (1, ?)
			ON DUPLICATE KEY UPDATE lastTxID = VALUES(lastTxID)`, cursor); err != nil {
			return fmt.Errorf("liquidity cursor: %w", err)
		}
	}
	clearLiquidityCache()
	return nil
}

// liquidityCursor is the last transactionID the ledger has read. A fresh ledger
// starts liquidityBootstrapSec back, since older rows weigh almost nothing and
// reading millions of them is the cost incremental reading avoids.
func (m *Mod) liquidityCursor(ctx context.Context, now int64) (int64, error) {
	var cur int64
	err := m.db.QueryRowContext(ctx,
		`SELECT lastTxID FROM rolebot_liquidity_cursor WHERE id = 1`).Scan(&cur)
	if err == nil {
		return cur, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("liquidity cursor: %w", err)
	}
	// idx_mt_type_date_agg covers (transactionType, transactionDateTime).
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(transactionID), 0) FROM market_transactions
		 WHERE transactionType = 1 AND transactionDateTime >= ?`,
		evedb.UnixToWin32(now-liquidityBootstrapSec)).Scan(&cur); err != nil {
		return 0, fmt.Errorf("liquidity bootstrap: %w", err)
	}
	if cur > 0 {
		cur-- // the cursor is exclusive
	}
	return cur, nil
}

// liquidityMap is the readers' copy: raw decayed ISK by station and by
// system, and the trading-post cutoff over the systems (tradePostCutoff,
// corptradepost.go), worked out once per load.
type liquidityMap struct {
	Station    map[int64]float64
	System     map[int64]float64
	PostCutoff float64
}

var (
	liquidityMu    sync.Mutex
	liquidityCache *liquidityMap
	liquidityAt    time.Time
)

// clearLiquidityCache drops the readers' copy so the next read goes to the
// table. Called by the sweep and by the economy purge (econpurge.go).
func clearLiquidityCache() {
	liquidityMu.Lock()
	liquidityCache, liquidityAt = nil, time.Time{}
	liquidityMu.Unlock()
}

// liquidity is the readers' entry point. An empty map (never swept, or a
// read error) is a legitimate answer: every reader treats a missing station
// as zero, which ranks a market on its distance alone.
func (m *Mod) liquidity(ctx context.Context) *liquidityMap {
	liquidityMu.Lock()
	if liquidityCache != nil && clock.Now().Sub(liquidityAt) < liquidityTTL {
		out := liquidityCache
		liquidityMu.Unlock()
		return out
	}
	liquidityMu.Unlock()

	out := &liquidityMap{Station: map[int64]float64{}, System: map[int64]float64{},
		PostCutoff: tradePostFloorISK}
	rows, err := m.db.QueryContext(ctx,
		`SELECT stationID, systemID, isk FROM rolebot_station_liquidity`)
	if err != nil {
		rlogf("rolebots: liquidity: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st, sys int64
		var isk float64
		if err := rows.Scan(&st, &sys, &isk); err != nil {
			rlogf("rolebots: liquidity scan: %v", err)
			return out
		}
		out.Station[st] = isk
		out.System[sys] += isk
	}
	if err := rows.Err(); err != nil {
		rlogf("rolebots: liquidity rows: %v", err)
		return out
	}
	out.PostCutoff = tradePostCutoff(out.System)
	liquidityMu.Lock()
	liquidityCache, liquidityAt = out, clock.Now()
	liquidityMu.Unlock()
	return out
}

// ── choosing a market ───────────────────────────────────────────────────────

// marketPick is a chosen market system, for the caller's log line.
type marketPick struct {
	SystemID  int64
	Name      string
	Liquidity float64 // decayed ISK traded there, seeders excluded
}

var (
	marketSysOnce sync.Once
	marketSysMap  map[int64]string
	marketSysErr  error
)

// marketSystems is every system with at least one station, by name. Static
// data, loaded once: the candidate set for any "where do I sell this" pick.
func (m *Mod) marketSystems(ctx context.Context) (map[int64]string, error) {
	marketSysOnce.Do(func() {
		rows, err := m.db.QueryContext(ctx, `
			SELECT DISTINCT s.solarSystemID, COALESCE(ms.solarSystemName, '')
			  FROM staStations s
			  LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = s.solarSystemID`)
		if err != nil {
			marketSysErr = fmt.Errorf("market systems: %w", err)
			return
		}
		defer rows.Close()
		out := map[int64]string{}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				marketSysErr = err
				return
			}
			out[id] = name
		}
		if marketSysErr = rows.Err(); marketSysErr == nil {
			marketSysMap = out
		}
	})
	return marketSysMap, marketSysErr
}

// rankMarkets orders the reachable market systems best first: liquidityValue
// plus bonus, discounted by flying. Pure. bonus is this world's luck and the
// human book (marketBonus, fortune.go); nil means none. With no liquidity
// measured the bonus decides among nearby candidates, so hubs nucleate in
// different places on different servers. maxJumps <= 0 means no ceiling beyond
// the reach map.
func rankMarkets(markets map[int64]string, liq map[int64]float64, jumps map[int64]int,
	bonus func(sysID int64) float64, jumpCost float64, maxJumps, limit int) []int64 {

	type cand struct {
		sys   int64
		hops  int
		score float64
	}
	var ranked []cand
	for sysID := range markets {
		hops, ok := jumps[sysID]
		if !ok || (maxJumps > 0 && hops > maxJumps) {
			continue
		}
		worth := 1 + liquidityValue(liq[sysID])
		if bonus != nil {
			worth += bonus(sysID)
		}
		v := worth / (1 + jumpCost*float64(hops))
		ranked = append(ranked, cand{sysID, hops, v})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].hops != ranked[j].hops {
			return ranked[i].hops < ranked[j].hops
		}
		return ranked[i].sys < ranked[j].sys // stable against map order
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]int64, 0, len(ranked))
	for _, c := range ranked {
		out = append(out, c.sys)
	}
	return out
}
