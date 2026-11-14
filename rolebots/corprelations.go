package rolebots

// The relations ledger: what one corporation thinks of another, and why.
//
// rolebot_corp_relations is one directed opinion per pair (corprelations_math.go explains
// the columns). rolebot_corp_incidents is append-only, one row per event between two
// corporations from the viewer's side, written by the hooks (corprelations_hooks.go) from
// any goroutine and folded into the value. rolebot_corp_letters is the envoys' mail and
// rolebot_corp_pacts the agreements those letters produced, each a floor under both
// directions of a pair for its term. This file is the store: schema, the carry from
// rolebot_ceo_relations, loaders, writers and the prune. The pass is corpdiplomacy_pass.go.

import (
	"context"
	"database/sql"
	"fmt"
)

// relIncidentPruneBatch is the rows per DELETE, by the (at) index with no ORDER BY, to
// avoid a filesort.
const relIncidentPruneBatch = 5000

// relClosedRetentionSec is how long an answered letter or an ended pact is
// kept for the panel.
const relClosedRetentionSec = 90 * 86400

// ensureRelationTables creates the four tables and carries the old
// rolebot_ceo_relations rows over on the first boot that has them.
func ensureRelationTables(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS rolebot_corp_relations (
			corporationID BIGINT NOT NULL,
			toID          BIGINT NOT NULL,
			value         DOUBLE NOT NULL DEFAULT 0,
			baseline      DOUBLE NOT NULL DEFAULT 0,
			prevValue     DOUBLE NOT NULL DEFAULT 0,
			written       DOUBLE NOT NULL DEFAULT 0,
			tier          DOUBLE NOT NULL DEFAULT 0,
			tierAt        BIGINT NOT NULL DEFAULT 0,
			decayedAt     BIGINT NOT NULL DEFAULT 0,
			floorValue    DOUBLE NOT NULL DEFAULT 0,
			floorUntil    BIGINT NOT NULL DEFAULT 0,
			yielded       TINYINT NOT NULL DEFAULT 0,
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (corporationID, toID),
			KEY idx_rbcrel_to (toID)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_corp_incidents (
			incidentID    BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			toID          BIGINT NOT NULL,
			kind          VARCHAR(12) NOT NULL,
			side          VARCHAR(10) NOT NULL DEFAULT 'victim',
			weight        DOUBLE NOT NULL DEFAULT 0,
			isk           DOUBLE NOT NULL DEFAULT 0,
			systemID      BIGINT NOT NULL DEFAULT 0,
			subjectID     BIGINT NOT NULL DEFAULT 0,
			dedupeKey     VARCHAR(48) NULL,
			at            BIGINT NOT NULL,
			appliedAt     BIGINT NOT NULL DEFAULT 0,
			detail        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (incidentID),
			UNIQUE KEY uq_rbinc_dedupe (corporationID, toID, dedupeKey),
			KEY idx_rbinc_pending (appliedAt, corporationID),
			KEY idx_rbinc_pair (corporationID, toID, at),
			KEY idx_rbinc_at (at)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_corp_letters (
			letterID    BIGINT NOT NULL AUTO_INCREMENT,
			fromCorpID  BIGINT NOT NULL,
			toCorpID    BIGINT NOT NULL,
			kind        VARCHAR(12) NOT NULL,
			terms       VARCHAR(255) NOT NULL DEFAULT '',
			isk         DOUBLE NOT NULL DEFAULT 0,
			termDays    INT NOT NULL DEFAULT 0,
			warID       BIGINT NOT NULL DEFAULT 0,
			status      VARCHAR(12) NOT NULL DEFAULT 'drafted',
			envoyCharID BIGINT NOT NULL DEFAULT 0,
			postedBy    BIGINT NOT NULL DEFAULT 0,
			draftedAt   BIGINT NOT NULL DEFAULT 0,
			sentAt      BIGINT NOT NULL DEFAULT 0,
			deliveredAt BIGINT NOT NULL DEFAULT 0,
			answeredAt  BIGINT NOT NULL DEFAULT 0,
			answeredBy  BIGINT NOT NULL DEFAULT 0,
			expiresAt   BIGINT NOT NULL DEFAULT 0,
			mailID      BIGINT NOT NULL DEFAULT 0,
			reason      VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (letterID),
			KEY idx_rbltr_to (toCorpID, status),
			KEY idx_rbltr_from (fromCorpID, status),
			KEY idx_rbltr_envoy (envoyCharID)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_corp_pacts (
			pactID     BIGINT NOT NULL AUTO_INCREMENT,
			letterID   BIGINT NOT NULL DEFAULT 0,
			corpA      BIGINT NOT NULL,
			corpB      BIGINT NOT NULL,
			kind       VARCHAR(12) NOT NULL,
			floorValue DOUBLE NOT NULL DEFAULT 0,
			startAt    BIGINT NOT NULL DEFAULT 0,
			endAt      BIGINT NOT NULL DEFAULT 0,
			status     VARCHAR(12) NOT NULL DEFAULT 'standing',
			brokenBy   BIGINT NOT NULL DEFAULT 0,
			reason     VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (pactID),
			KEY idx_rbpact_pair (corpA, corpB, status),
			KEY idx_rbpact_end (status, endAt)
		)`,
		gankLogSchema,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("ensureRelationTables: %w", err)
		}
	}
	return migrateCEORelations(ctx, db)
}

// migrateCEORelations carries rolebot_ceo_relations (one row per declaration a CEO wrote)
// into rolebot_corp_relations and drops it. Probe-guarded: a fresh install has no old
// table. value, written and tier take the old standing, so a carried pair is still ours
// by relOwnership; the old score becomes the baseline until the first pass recomputes it.
func migrateCEORelations(ctx context.Context, db *sql.DB) error {
	ok, err := haveTable(ctx, db, "rolebot_ceo_relations")
	if err != nil || !ok {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO rolebot_corp_relations
			(corporationID, toID, value, baseline, prevValue, written, tier, tierAt, decayedAt, yielded, reason)
		SELECT corporationID, toID, standing, score, standing, standing, standing, setAt, setAt, yielded, reason
		  FROM rolebot_ceo_relations`); err != nil {
		return fmt.Errorf("migrateCEORelations carry: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE rolebot_ceo_relations`); err != nil {
		return fmt.Errorf("migrateCEORelations drop: %w", err)
	}
	return nil
}

// relRowsOf loads every pair one corporation holds an opinion on.
func (m *Mod) relRowsOf(ctx context.Context, corpID int64) (map[int64]relState, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT toID, value, baseline, prevValue, written, tier, tierAt, decayedAt,
		       floorValue, floorUntil, yielded, reason
		  FROM rolebot_corp_relations WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("relRowsOf: %w", err)
	}
	defer rows.Close()
	out := map[int64]relState{}
	for rows.Next() {
		var id, yielded int64
		var s relState
		if err := rows.Scan(&id, &s.Value, &s.Baseline, &s.PrevValue, &s.Written, &s.Tier, &s.TierAt,
			&s.DecayedAt, &s.FloorValue, &s.FloorUntil, &yielded, &s.Reason); err != nil {
			return nil, fmt.Errorf("relRowsOf scan: %w", err)
		}
		s.Yielded = yielded != 0
		out[id] = s
	}
	return out, rows.Err()
}

// relSaveRow writes one pair's whole state.
func (m *Mod) relSaveRow(ctx context.Context, corpID, toID int64, s relState) error {
	yielded := 0
	if s.Yielded {
		yielded = 1
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_relations
			(corporationID, toID, value, baseline, prevValue, written, tier, tierAt, decayedAt,
			 floorValue, floorUntil, yielded, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE value = VALUES(value), baseline = VALUES(baseline),
			prevValue = VALUES(prevValue), written = VALUES(written), tier = VALUES(tier),
			tierAt = VALUES(tierAt), decayedAt = VALUES(decayedAt), floorValue = VALUES(floorValue),
			floorUntil = VALUES(floorUntil), yielded = VALUES(yielded), reason = VALUES(reason)`,
		corpID, toID, s.Value, s.Baseline, s.PrevValue, s.Written, s.Tier, s.TierAt, s.DecayedAt,
		s.FloorValue, s.FloorUntil, yielded, truncate(s.Reason, 255)); err != nil {
		return fmt.Errorf("relSaveRow: %w", err)
	}
	return nil
}

// relDeleteRow drops a pair the corporation no longer holds an opinion on. A
// yielded row is never dropped: the yield is the memory that a human owns
// the pair.
func (m *Mod) relDeleteRow(ctx context.Context, corpID, toID int64) error {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_corp_relations WHERE corporationID = ? AND toID = ? AND yielded = 0`,
		corpID, toID); err != nil {
		return fmt.Errorf("relDeleteRow: %w", err)
	}
	return nil
}

// markRelationYielded takes a pair out of a CEO's hands for good. Called by the flush
// when it finds its row edited, and by the Relations / corporation page when the
// operator declares for a managed corporation. standing is what the human set, recorded
// as the written number so the panel can show it beside what the ledger would hold.
func markRelationYielded(ctx context.Context, db *sql.DB, corpID, toID int64, standing float64, why string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_relations (corporationID, toID, value, written, tierAt, decayedAt, yielded, reason)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?)
		ON DUPLICATE KEY UPDATE written = VALUES(written), tierAt = VALUES(tierAt),
			yielded = 1, reason = VALUES(reason)`,
		corpID, toID, standing, standing, clock.Unix(), clock.Unix(), truncate(why, 255))
	return err
}

// relHandBack gives a yielded pair back to the ledger: the next flush
// re-asserts the ledger's own number.
func relHandBack(ctx context.Context, db *sql.DB, corpID, toID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE rolebot_corp_relations SET yielded = 0, written = 0 WHERE corporationID = ? AND toID = ?`,
		corpID, toID)
	return err
}

// relPendingIncidents is every incident of one corporation's not yet folded,
// oldest first.
func (m *Mod) relPendingIncidents(ctx context.Context, corpID int64) (map[int64][]relIncident, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT incidentID, toID, kind, side, weight, isk, at, subjectID
		  FROM rolebot_corp_incidents
		 WHERE appliedAt = 0 AND corporationID = ?
		 ORDER BY at, incidentID`, corpID)
	if err != nil {
		return nil, fmt.Errorf("relPendingIncidents: %w", err)
	}
	defer rows.Close()
	out := map[int64][]relIncident{}
	for rows.Next() {
		var to int64
		var inc relIncident
		if err := rows.Scan(&inc.ID, &to, &inc.Kind, &inc.Side, &inc.Weight, &inc.ISK, &inc.At, &inc.SubjectID); err != nil {
			return nil, fmt.Errorf("relPendingIncidents scan: %w", err)
		}
		out[to] = append(out[to], inc)
	}
	return out, rows.Err()
}

// relMarkApplied stamps incidents as folded.
func (m *Mod) relMarkApplied(ctx context.Context, ids []int64, now int64) error {
	for _, id := range ids {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_corp_incidents SET appliedAt = ? WHERE incidentID = ? AND appliedAt = 0`, now, id); err != nil {
			return fmt.Errorf("relMarkApplied: %w", err)
		}
	}
	return nil
}

// relCorpsWithPending is every managed corporation with an incident waiting to be
// folded: what the periodic fold walks.
func (m *Mod) relCorpsWithPending(ctx context.Context) ([]int64, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT DISTINCT corporationID FROM rolebot_corp_incidents WHERE appliedAt = 0`)
	if err != nil {
		return nil, fmt.Errorf("relCorpsWithPending: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("relCorpsWithPending scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// pruneCorpRelations drops incidents past retention and closed letters and
// pacts past relClosedRetentionSec, in batches. Called beside pruneCEOActions
// from the CEO sweep; never fatal.
func (m *Mod) pruneCorpRelations(ctx context.Context) {
	now := clock.Unix()
	cutoff := now - int64(relIncidentDays())*86400
	for i := 0; i < 20; i++ {
		res, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_corp_incidents WHERE at < ? LIMIT ?`, cutoff, relIncidentPruneBatch)
		if err != nil {
			rlogf("rolebots: relations incident prune: %v", err)
			return
		}
		if n, _ := res.RowsAffected(); n < relIncidentPruneBatch {
			break
		}
	}
	closed := now - relClosedRetentionSec
	if _, err := m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_letters
		 WHERE status NOT IN ('drafted', 'sent', 'delivered', 'accepted') AND answeredAt > 0 AND answeredAt < ? LIMIT ?`,
		closed, relIncidentPruneBatch); err != nil {
		rlogf("rolebots: relations letter prune: %v", err)
	}
	if _, err := m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_pacts WHERE status <> 'standing' AND endAt > 0 AND endAt < ? LIMIT ?`,
		closed, relIncidentPruneBatch); err != nil {
		rlogf("rolebots: relations pact prune: %v", err)
	}
}

// forgetCorpRelations drops everything the ledger holds for a corporation whose CEO is
// removed: its opinions (the standings stay, as the corporation's), unanswered letters
// and pending incidents. Pacts stay: the other party gave its word too.
func (m *Mod) forgetCorpRelations(ctx context.Context, corpID int64) error {
	for _, q := range []string{
		`DELETE FROM rolebot_corp_relations WHERE corporationID = ?`,
		`DELETE FROM rolebot_corp_incidents WHERE corporationID = ? AND appliedAt = 0`,
		`UPDATE rolebot_corp_letters SET status = 'withdrawn' WHERE fromCorpID = ? AND status IN ('drafted', 'sent', 'delivered')`,
	} {
		if _, err := m.db.ExecContext(ctx, q, corpID); err != nil {
			return fmt.Errorf("forgetCorpRelations: %w", err)
		}
	}
	return nil
}
