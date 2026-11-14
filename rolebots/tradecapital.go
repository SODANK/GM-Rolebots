package rolebots

// Trader capital: what a pilot may bet, and when it must stick to sure runs. Trade-good arbitrage is the roster's
// working faucet; speculation sells far less often than its confidence assumed. Three rules:
//
//  1. Calibration. A bet's confidence is scaled by what listings of that category actually did (specSellThrough),
//     roster-wide over 7 days. Evidence terms rank bets against each other; the market sets their worth against a sure run.
//  2. A bet is a share of the float, never the float. A bet takes at most specBetShare of the budget, and a pilot under
//     specMinFloatISK does not bet at all: it runs sure arbitrage until it has a float worth risking a third of.
//  3. A pilot with goods nobody bids on and a wallet that cannot pay the broker fee must not keep flying to them:
//     tradeStockPlan skips a holding it cannot afford to list one unit of, tradeListRun lists the part of a lot the
//     wallet can pay the fee on, and needPull does not lift trading for a pilot without trading capital.

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
)

const (
	// specBetShare is the most of its trading budget a pilot puts into one
	// speculative run. A third leaves two more runs' worth of sure arbitrage
	// in the till while the bet rests.
	specBetShare = 0.33

	// specMinFloatISK is the trading budget under which a pilot does not speculate at all: the wallet floor (broke.go)
	// less the trade reserve. A pilot under the floor is earning its way back with the run whose margin is known up front.
	specMinFloatISK = walletFloorISK * (1 - tradeWalletReserveFrac)

	// tradeSeedFloatISK is the smallest wallet that can work the trade-good arbitrage at all: a hold's worth of the
	// cheap goods at the small-budget profit floor (tradefloor.go). Below it the need term must not send a pilot trading.
	tradeSeedFloatISK = 50_000.0

	// The calibration. A category whose listings sell specCalibRef of their units is believed at face value; one that
	// sells less is believed proportionally less, down to specCalibMin. Under specCalibMinEvents resolved listings there
	// is no evidence and no scaling.
	specCalibRef       = 0.5
	specCalibMin       = 0.10
	specCalibMinEvents = 50
	specCalibWindowSec = 7 * 86400
	// One range read of the trade book, shared by everybody: the answer moves on the timescale of listing patience.
	specCalibTTL = time.Hour
	// A failed read is retried after this, not on the next planner call: a
	// read that failed by running out of time would only fail again.
	specCalibRetry = 5 * time.Minute
)

// specBetBudget is what one speculative run may spend, out of the trading budget the run search was handed.
// Zero means no bets, sure runs only.
func specBetBudget(budget float64) float64 {
	if budget < specMinFloatISK {
		return 0
	}
	return budget * specBetShare
}

// affordableListQty is how many of qty units, listed at price, the balance
// can pay the broker fee on. The fee is a share of order value with a flat
// minimum, so the answer is 0 when even the minimum is out of reach.
func affordableListQty(balance, price float64, qty int64, r marketRates) int64 {
	if qty <= 0 || price <= 0 || balance < r.brokerFee(price) {
		return 0
	}
	if balance >= r.brokerFee(price*float64(qty)) {
		return qty
	}
	k := qty
	if r.Fee > 0 {
		k = int64(math.Floor(balance / (r.Fee * price)))
	}
	if k > qty {
		k = qty
	}
	// The minimum fee binds on a small lot: if k units' fee is under the minimum, the minimum is charged and the
	// balance covers it (checked above), so k stands.
	for k > 0 && r.brokerFee(price*float64(k)) > balance {
		k--
	}
	return k
}

// specCalibration scales a bet's confidence by its category's sell-through.
// rate < 0 means no evidence: the confidence stands.
func specCalibration(conf, rate float64) float64 {
	if rate < 0 {
		return conf
	}
	f := rate / specCalibRef
	if f > 1 {
		f = 1
	}
	if f < specCalibMin {
		f = specCalibMin
	}
	return conf * f
}

var (
	specCalibMu      sync.Mutex
	specCalibCur     map[int64]float64
	specCalibAt      time.Time
	specCalibLoading sync.Mutex
)

// specCalibrate applies the roster's measured sell-through for the category to conf. It never blocks a planner: a
// stale or cold table triggers a background reload and the caller uses what is there (nothing at a cold start, so no
// scaling).
func (m *Mod) specCalibrate(ctx context.Context, categoryID int64, conf float64) float64 {
	specCalibMu.Lock()
	cur, at := specCalibCur, specCalibAt
	specCalibMu.Unlock()
	if cur == nil || clock.Now().Sub(at) >= specCalibTTL {
		if specCalibLoading.TryLock() {
			goSafe("spec-calibration", func() {
				defer specCalibLoading.Unlock()
				m.loadSpecCalibration(ctx)
			})
		}
	}
	rate, ok := cur[categoryID]
	if !ok {
		return conf
	}
	return specCalibration(conf, rate)
}

// loadSpecCalibration reads, per item category, the share of units on resale listings that sold rather than being
// pulled unsold. Every listing counts, not only the trader's: it measures whether anybody buys this kind of thing
// off a shelf.
func (m *Mod) loadSpecCalibration(ctx context.Context) {
	failed := func(format string, err error) {
		rlogf(format, err)
		specCalibMu.Lock()
		if specCalibCur == nil {
			specCalibCur = map[int64]float64{}
		}
		specCalibAt = clock.Now().Add(specCalibRetry - specCalibTTL)
		specCalibMu.Unlock()
	}
	sctx, cancel := snapContext(ctx)
	defer cancel()
	// The week is never re-read: foldSpecCalib carries the trade book forward into day buckets by a tradeID cursor
	// (rolebot_trade_calib) and the calibration is a sum over seven of them.
	if err := m.foldSpecCalib(sctx); err != nil {
		failed("rolebots: spec calibration fold: %v", err)
		return
	}
	rows, err := m.db.QueryContext(sctx, capStatement(`
		SELECT categoryID, SUM(sold), SUM(total), SUM(events)
		  FROM rolebot_trade_calib
		 WHERE day >= ?
		 GROUP BY categoryID`),
		specDay(clock.Unix())-specCalibWindowSec/86400)
	if err != nil {
		failed("rolebots: spec calibration: %v", err)
		return
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var cat, events int64
		var sold, total float64
		if err := rows.Scan(&cat, &sold, &total, &events); err != nil {
			failed("rolebots: spec calibration scan: %v", err)
			return
		}
		if events < specCalibMinEvents || total <= 0 {
			continue
		}
		out[cat] = sold / total
	}
	if err := rows.Err(); err != nil {
		failed("rolebots: spec calibration: %v", err)
		return
	}
	specCalibMu.Lock()
	specCalibCur, specCalibAt = out, clock.Now()
	specCalibMu.Unlock()
}

// specDay is the rollup's day bucket.
func specDay(unix int64) int64 { return unix / 86400 }

const (
	// settingSpecCalibCursor is the last tradeID folded into the rollup.
	settingSpecCalibCursor = "rolebots.specCalib.tradeCursor"
	// specCalibChunk is how many tradeIDs one fold statement spans: a
	// primary-key range, so each is short whatever the table holds.
	specCalibChunk = int64(200_000)
)

// foldSpecCalib carries the trade book forward into rolebot_trade_calib: every fill off a listing and every unsold
// cancel since the cursor, by day and item category. The cursor is saved per chunk, so a fold cut short by the read
// budget resumes where it stopped. The first fold ever starts at the window's edge, not the table's.
func (m *Mod) foldSpecCalib(ctx context.Context) error {
	cursor := int64(0)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingSpecCalibCursor); err != nil {
		return err
	} else if ok {
		cursor, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	}
	if cursor == 0 {
		_ = m.db.QueryRowContext(ctx,
			`SELECT tradeID FROM rolebot_trades WHERE ts > ? ORDER BY ts LIMIT 1`,
			clock.Unix()-specCalibWindowSec).Scan(&cursor)
		if cursor == 0 {
			return nil
		}
		cursor--
	}
	var maxID int64
	_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(tradeID), 0) FROM rolebot_trades`).Scan(&maxID)
	type bucket struct {
		sold, total float64
		events      int64
	}
	for cursor < maxID {
		upto := cursor + specCalibChunk
		if upto > maxID {
			upto = maxID
		}
		acc := map[[2]int64]*bucket{}
		rows, err := m.db.QueryContext(ctx, `
			SELECT t.ts, t.kind, t.quantity, g.categoryID
			  FROM rolebot_trades t
			  JOIN invTypes i ON i.typeID = t.typeID
			  JOIN invGroups g ON g.groupID = i.groupID
			 WHERE t.tradeID > ? AND t.tradeID <= ?
			   AND ((t.kind = ? AND t.note LIKE 'somebody bought off its listing%')
			     OR (t.kind = ? AND t.note LIKE 'nobody was buying%'))`,
			cursor, upto, tradeKindFill, tradeKindCancel)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ts, qty, cat int64
			var kind string
			if err := rows.Scan(&ts, &kind, &qty, &cat); err != nil {
				rows.Close()
				return err
			}
			k := [2]int64{specDay(ts), cat}
			b := acc[k]
			if b == nil {
				b = &bucket{}
				acc[k] = b
			}
			if kind == tradeKindFill {
				b.sold += float64(qty)
			}
			b.total += float64(qty)
			b.events++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for k, b := range acc {
			if _, err := m.db.ExecContext(ctx, `
				INSERT INTO rolebot_trade_calib (day, categoryID, sold, total, events)
				VALUES (?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE sold = sold + VALUES(sold), total = total + VALUES(total),
				                        events = events + VALUES(events)`,
				k[0], k[1], b.sold, b.total, b.events); err != nil {
				return err
			}
		}
		cursor = upto
		if err := evedb.SetServerSetting(ctx, m.db, settingSpecCalibCursor, strconv.FormatInt(cursor, 10)); err != nil {
			return err
		}
	}
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_trade_calib WHERE day < ?`,
		specDay(clock.Unix())-specCalibWindowSec/86400-1)
	return nil
}
