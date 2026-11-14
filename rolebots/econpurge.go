package rolebots

import (
	"context"
	"fmt"
	"strings"

	evedb "gurimoon/db"
)

// Wiping the economy the roster has built.
// The bot wipe (botpurge.go) takes the pilots. This takes what they learned and did: hub
// scores, the traffic census, the trade book, the demand ledgers and the wallet history their
// trading wrote. It is a separate verb so an operator can retune the economy without losing
// pilots, or replace the population and keep the map's traffic history.
// Taken: everything the module derived. It regenerates as the sweeps run.
// Taken on request: the bots' own market_transactions and market_journal rows. Those tables
// also hold every real player's history, so the delete is keyed on the roster.
// Never taken: live market orders (the market itself; NPC orders belong to the seeder) and
// killmails (db/killretention.go).

// econPurgeConfirm is what an operator types. It differs from wipeAllConfirm so that one
// destructive verb's confirmation never authorises the other.
const econPurgeConfirm = "WIPE ECONOMY"

// EconPurgeScope is what the operator asked to wipe.
type EconPurgeScope struct {
	// Learned is the derived data: hub scores, traffic, trade book, ledgers, shelf, stock.
	// It rebuilds itself.
	Learned bool
	// History is the rostered characters' own market_transactions and market_journal rows.
	// It does not come back.
	History bool
	// Orders cancels every order the roster has resting. It goes through CancelMarketOrder, so
	// escrow is refunded and unsold goods return to the hangar.
	Orders bool
}

// EconPurgeResult counts what went, per table, so the page can report it.
type EconPurgeResult struct {
	Rows  map[string]int64
	Total int64
}

// econLearnedTables is every derived table, in the order they are emptied. None carries a
// foreign key to another; the order is kept stable so the report reads the same each time.
var econLearnedTables = []string{
	// Where trade happens, as measured.
	"rolebot_hub_scores",
	"rolebot_station_scores",
	"rolebot_station_liquidity",
	"rolebot_station_demand",
	"rolebot_traffic_systems",
	"rolebot_traffic_lanes",
	"rolebot_traffic_stations",
	// What the roster has done: the trade book behind the Trade Feed and the bot-to-bot half.
	"rolebot_trades",
	"rolebot_peer_trades",
	"rolebot_trade_stock",
	// The roster's own order ledger. See PurgeEconomy for why it needs the live orders dealt with.
	"rolebot_orders",
	// What it has been asked for, and what it holds toward a project.
	"rolebot_market_demand",
	"rolebot_mineral_demand",
	"rolebot_mineral_hold",
	"rolebot_industry_shelf",
	// Where the miners were refused belts (beltdistress.go), rebuilt within a planning pass.
	"rolebot_belt_distress",
	// The ISK-flow ledger (iskflow.go) and both cursors, emptied together so the next sweep
	// re-bootstraps from thirty days back.
	"rolebot_isk_flow",
	"rolebot_isk_flow_corp",
	"rolebot_isk_flow_cursor",
	"rolebot_mkt_flow",
	"rolebot_mkt_system",
	"rolebot_mkt_group",
	// The market history the trade logic reads (mktdaily.go); its cursor is row 3 of
	// rolebot_mkt_flow_cursor.
	"rolebot_mkt_daily",
	"rolebot_mkt_flow_cursor",
}

// PurgeEconomy empties the roster's economic memory. It is not transactional across
// tables: each statement stands alone, a partial run leaves a smaller version of what was
// asked, and re-running finishes the job. One transaction would lock the busiest tables.
func (m *Mod) PurgeEconomy(ctx context.Context, scope EconPurgeScope) (EconPurgeResult, error) {
	out := EconPurgeResult{Rows: map[string]int64{}}
	if !scope.Learned && !scope.History && !scope.Orders {
		return out, fmt.Errorf("nothing selected — tick the learned data, the resting orders, the wallet history, or any combination")
	}

	// Live orders come down first: emptying rolebot_orders while they stand would orphan them
	// with their escrow, so cancelling is part of emptying the ledger.
	if scope.Learned || scope.Orders {
		n, err := m.cancelRestingOrders(ctx)
		if err != nil {
			return out, err
		}
		out.Rows["market orders cancelled"] = n
		out.Total += n
	}

	if scope.Learned {
		for _, table := range econLearnedTables {
			// DELETE rather than TRUNCATE: TRUNCATE needs the DROP privilege, which the shipped
			// database user lacks.
			res, err := m.db.ExecContext(ctx, "DELETE FROM "+table)
			if err != nil {
				return out, fmt.Errorf("purge %s: %w", table, err)
			}
			n, _ := res.RowsAffected()
			out.Rows[table] = n
			out.Total += n
		}
		// The lifetime trade counters go with the trade book (statcount.go); jumps, docks and ore
		// stay. The seed marker stays too, since clearing it would re-run a one-shot over an empty table.
		res, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_stats
			   SET trades = 0, buys = 0, sells = 0, turnover = 0, profit = 0,
			       bestTrade = 0, ordersPlaced = 0, feesPaid = 0`)
		if err != nil {
			return out, fmt.Errorf("purge rolebot_stats trade counters: %w", err)
		}
		n, _ := res.RowsAffected()
		out.Rows["lifetime trade counters"] = n
		out.Total += n

		// A new epoch redraws every per-epoch stateless draw (epoch.go, fittaste.go). On error the
		// purge fails and can be re-run.
		if err := m.redrawEpoch(ctx); err != nil {
			return out, fmt.Errorf("purge epoch redraw: %w", err)
		}
		out.Rows["economy epoch reseeded"] = 1
		out.Total++

		// The in-memory caches read from what was just emptied, so they are cleared too.
		clearLiquidityCache()
		clearStationDemandCache()
		sweepMarketSnaps()
		// The Stats board memo (espstats.go) holds an assembled copy for a minute.
		clearStatsMemo()
		// The Industry board reads the demand ledgers and the order registry this scope emptied.
		clearIndustryMemo()
		// The Economy pages (especonomy.go) read the same ledgers.
		clearEconomyMemo()
	}

	if scope.History {
		// Rostered characters only: these tables hold every real player's wallet, so the delete
		// is keyed on the roster.
		for _, spec := range []struct{ table, col string }{
			{"market_transactions", "characterID"},
			{"market_journal", "characterID"},
		} {
			res, err := m.db.ExecContext(ctx,
				"DELETE FROM "+spec.table+" WHERE "+spec.col+
					" IN (SELECT characterID FROM rolebot_characters)")
			if err != nil {
				return out, fmt.Errorf("purge %s: %w", spec.table, err)
			}
			n, _ := res.RowsAffected()
			out.Rows[spec.table] = n
			out.Total += n
		}
	}
	return out, nil
}

// cancelRestingOrders takes every order the roster has on the market down through the
// core's CancelMarketOrder and returns how many came down. It refunds a bid's escrow and
// returns an ask's goods to the hangar (a DELETE would not), as the module's order sweep does
// (tradebook.go). It reads market_orders, not rolebot_orders, and one failure does not stop the rest.
func (m *Mod) cancelRestingOrders(ctx context.Context) (int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.orderID, o.charID FROM market_orders o
		 WHERE o.orderState = 1
		   AND o.charID IN (SELECT characterID FROM rolebot_characters)`)
	if err != nil {
		return 0, fmt.Errorf("resting orders: %w", err)
	}
	type ord struct{ orderID, charID int64 }
	var live []ord
	for rows.Next() {
		var o ord
		if rows.Scan(&o.orderID, &o.charID) == nil {
			live = append(live, o)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("resting orders: %w", err)
	}

	var done int64
	var failed int
	for _, o := range live {
		if _, cerr := evedb.CancelMarketOrder(ctx, m.db, o.charID, o.orderID); cerr != nil {
			failed++
			continue
		}
		done++
	}
	if failed > 0 {
		rlogf("rolebots: economy purge cancelled %d order(s), %d refused", done, failed)
	}
	return done, nil
}

// econPurgeSummary renders the per-table counts for the page banner, biggest first, with
// zero rows omitted.
func econPurgeSummary(res EconPurgeResult) string {
	if res.Total == 0 {
		return "nothing to purge — the roster's economic memory was already empty"
	}
	type row struct {
		name string
		n    int64
	}
	var rows []row
	for name, n := range res.Rows {
		if n > 0 {
			rows = append(rows, row{name, n})
		}
	}
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].n > rows[j-1].n; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s %d", strings.TrimPrefix(r.name, "rolebot_"), r.n))
	}
	return fmt.Sprintf("purged %d row(s) — %s. The sweeps rebuild scores and traffic within minutes",
		res.Total, strings.Join(parts, ", "))
}

// econPurgeCounts is what the page shows before the operator commits: how much is there.
func (m *Mod) econPurgeCounts(ctx context.Context) (learned, history, orders int64) {
	for _, table := range econLearnedTables {
		var n int64
		if err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err == nil {
			learned += n
		}
	}
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM market_orders
		  WHERE orderState = 1 AND charID IN (SELECT characterID FROM rolebot_characters)`).Scan(&orders)
	var tx, jr int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM market_transactions
		  WHERE characterID IN (SELECT characterID FROM rolebot_characters)`).Scan(&tx)
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM market_journal
		  WHERE characterID IN (SELECT characterID FROM rolebot_characters)`).Scan(&jr)
	return learned, tx + jr, orders
}
