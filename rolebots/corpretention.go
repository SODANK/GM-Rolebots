package rolebots

// Indexes and retention for the corporation ledgers.
//
// The corporation layer writes a row per labour credit, per wage, per closed
// work order, requisition, print move, survey, siege and purchase order, and
// nothing ever removed them: the payroll's window query ran over a labour
// history that only grew, and a corporation's whole past was read to find
// its last week. Closed rows are kept for a month, paid and settled ones
// for three; the indexes are the shapes the readers actually use.

import (
	"context"
	"database/sql"

	evedb "gurimoon/db"
)

// ensureCorpIndexes adds the keys the corporation readers range on. Each is
// non-fatal and INPLACE, like the log's (ensureLogIndexes).
func ensureCorpIndexes(ctx context.Context, db *sql.DB) {
	for _, ix := range []struct{ table, name, cols string }{
		{"rolebot_corp_labour", "idx_rbclab_corp_at", "(corporationID, at)"},
		{"rolebot_corp_labour", "idx_rbclab_corp_paid", "(corporationID, paidAt)"},
		{"rolebot_corp_assignments", "idx_rbcas_closed", "(closedAt)"},
		{"rolebot_corp_letters", "idx_rbcl_answered", "(answeredAt)"},
		{"rolebot_corp_issue", "idx_rbci_closed", "(closedAt)"},
		{"rolebot_corp_orders", "idx_rbco_corp_station_type", "(corporationID, stationID, typeID)"},
		{"rolebot_gangs", "idx_rbg_corp", "(corpID)"},
		{"rolebot_gangs", "idx_rbg_leader", "(leaderCharID)"},
		{"rolebot_gang_ore", "idx_rbgo_hauler", "(haulerCharID)"},
		{"rolebot_market_demand", "idx_rbdemand_claimed", "(claimedBy)"},
		// The library planner's sold-copy test reads one corporation's print
		// ledger (corplibplan.go); without this it scanned every listing ever.
		{"rolebot_bpc_listings", "idx_rbbl_corp", "(corporationID)"},
	} {
		have, err := haveIndex(ctx, db, ix.table, ix.name)
		if err != nil {
			rlogf("probe %s.%s: %v", ix.table, ix.name, err)
			continue
		}
		if have {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE `+ix.table+` ADD KEY IF NOT EXISTS `+
			ix.name+` `+ix.cols+`, ALGORITHM=INPLACE, LOCK=NONE`); err != nil {
			rlogf("add %s.%s: %v (its readers run without it)", ix.table, ix.name, err)
		}
	}
}

const (
	// corpSettledKeepSec is how long paid labour, paid wages, settled SRP
	// and closed commitments are kept: the payroll and the P&L look back a
	// month at most.
	corpSettledKeepSec = 90 * 86400
	// corpClosedKeepSec is how long closed work, orders, moves, surveys and sieges are kept:
	// the boards show a month of history.
	corpClosedKeepSec = 30 * 86400
	// corpPruneBatch rows per DELETE, so a backlog is taken in short bites
	// across sweeps rather than one long statement holding its locks.
	corpPruneBatch = 5000
)

// pruneCorpHistory drops settled and closed corporation rows past retention.
// Called beside pruneCEOActions from the CEO sweep, hourly; never fatal.
func (m *Mod) pruneCorpHistory(ctx context.Context) {
	now := clock.Unix()
	settled := now - corpSettledKeepSec
	closed := now - corpClosedKeepSec
	for _, p := range []struct {
		table, where string
		cutoff       int64
	}{
		{"rolebot_corp_labour", "paidAt > 0 AND paidAt < ?", settled},
		{"rolebot_corp_wages", "paidAt > 0 AND paidAt < ?", settled},
		{"rolebot_corp_srp", "status <> 'owed' AND lostAt > 0 AND lostAt < ?", settled},
		{"rolebot_corp_commit", "closedAt > 0 AND closedAt < ?", settled},
		{"rolebot_corp_letters", "status = 'accepted' AND answeredAt > 0 AND answeredAt < ?", settled},
		{"rolebot_corp_work", "status <> 'open' AND closedAt > 0 AND closedAt < ?", closed},
		{"rolebot_corp_commands", "closedAt > 0 AND closedAt < ?", closed},
		{"rolebot_corp_requisition", "closedAt > 0 AND closedAt < ?", closed},
		{"rolebot_corp_print_moves", "status NOT IN ('open', 'claimed') AND claimedAt > 0 AND claimedAt < ?", closed},
		{"rolebot_corp_survey", "closedAt > 0 AND closedAt < ?", closed},
		{"rolebot_corp_siege", "closedAt > 0 AND closedAt < ?", closed},
		{"rolebot_corp_po", "status <> 'open' AND closedAt > 0 AND closedAt < ?", closed},
	} {
		if ctx.Err() != nil {
			return
		}
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM `+p.table+` WHERE `+p.where+` LIMIT ?`, p.cutoff, corpPruneBatch); err != nil {
			rlogf("rolebots: prune %s: %v", p.table, err)
		}
	}
}

// botJobKeepDays is how long a bot's finished personal job stays in ramJobs. The longest
// reader looks back 30 days (donatePrintIdleSec); the core never deletes a delivered job.
const botJobKeepDays = 60

// pruneBotIndustryHistory deletes rolebots' delivered and aborted personal jobs past
// botJobKeepDays. Corporation jobs (ownerID > 0) stay, since the wage sweep pays a
// delivered one not yet paid (corpwork.go); a player's jobs belong to the core. It selects
// a batch then deletes by primary key, as MariaDB's multi-table DELETE takes no LIMIT. Hourly.
func (m *Mod) pruneBotIndustryHistory(ctx context.Context) {
	cutoff := evedb.UnixToWin32(clock.Unix() - botJobKeepDays*86400)
	for i := 0; i < 20 && ctx.Err() == nil; i++ {
		rows, err := m.db.QueryContext(ctx, capStatement(`
			SELECT j.jobID FROM ramJobs j
			  JOIN rolebot_characters rc ON rc.characterID = j.installerID
			 WHERE j.completedStatusID IN (101, 102) AND j.endProductionTime < ? AND j.ownerID = 0
			 LIMIT ?`), cutoff, corpPruneBatch)
		if err != nil {
			rlogf("rolebots: prune bot ramJobs: %v", err)
			return
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if len(ids) == 0 {
			return
		}
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM ramJobs WHERE completedStatusID IN (101, 102) AND jobID IN (`+placeholders(len(ids))+`)`,
			int64Args(ids)...); err != nil {
			rlogf("rolebots: prune bot ramJobs: %v", err)
			return
		}
		if len(ids) < corpPruneBatch {
			return
		}
	}
}
