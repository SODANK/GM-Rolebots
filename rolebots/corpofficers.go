package rolebots

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/corp"
)

// A bot CEO's human officers: which of their offices are its own to take back.
//
// The roles pass (ceoRoles) owns the officer bits of every bot. A human may hold one somebody else
// gave them, an operator or a person who had the chair before, and that one stands: the pass records
// what it granted each human and takes back only that. A human with no record was given everything
// they hold.

func ensureOfficerGrantTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rolebot_corp_officer_grants (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			mask          BIGINT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID)
		)`); err != nil {
		return fmt.Errorf("ensureOfficerGrantTable: %w", err)
	}
	return nil
}

// loadOfficerGrants is the officer bits the roles pass granted each human of a corporation.
func (m *Mod) loadOfficerGrants(ctx context.Context, corpID int64) (map[int64]uint64, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, mask FROM rolebot_corp_officer_grants WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("loadOfficerGrants: %w", err)
	}
	defer rows.Close()
	out := map[int64]uint64{}
	for rows.Next() {
		var id int64
		var mask uint64
		if err := rows.Scan(&id, &mask); err != nil {
			return nil, fmt.Errorf("loadOfficerGrants: %w", err)
		}
		out[id] = mask
	}
	return out, rows.Err()
}

// keepOfficerGrant records a member's grant when it differs from the one loaded; 0 forgets.
func (m *Mod) keepOfficerGrant(ctx context.Context, corpID, charID int64, grants map[int64]uint64, mask uint64) {
	if grants[charID] == mask {
		return
	}
	var err error
	if mask == 0 {
		_, err = m.db.ExecContext(ctx,
			`DELETE FROM rolebot_corp_officer_grants WHERE corporationID = ? AND characterID = ?`, corpID, charID)
	} else {
		_, err = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_officer_grants (corporationID, characterID, mask) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE mask = VALUES(mask)`, corpID, charID, mask)
	}
	if err != nil {
		rlogf("rolebots: officer grant of %d in corp %d: %v", charID, corpID, err)
		return
	}
	grants[charID] = mask
}

// forgetOfficerGrants lets go of every office granted in a corporation, when a bot takes the chair
// from a person: what the members hold is what the person left them with.
func (m *Mod) forgetOfficerGrants(ctx context.Context, corpID int64) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_corp_officer_grants WHERE corporationID = ?`, corpID); err != nil {
		rlogf("rolebots: forget officer grants of corp %d: %v", corpID, err)
	}
	// The Director of Industry's comes with the industry pass's bits (ceoIndustryStaff).
	if _, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_grants SET mask = mask & ~? WHERE corporationID = ?`, corp.RoleDirector, corpID); err != nil {
		rlogf("rolebots: forget industry grants of corp %d: %v", corpID, err)
	}
}

// officerUpdate is what a human's offices become, and which of them are the pass's to take back.
// cur is the offices held, grant what the pass gave before, want what it wants now. Pure.
func officerUpdate(cur, grant, want uint64) (next, granted uint64) {
	return industryRoleUpdate(cur, grant, want)
}

// headHoldsDirector reports whether a human holds Director for the division it heads. Every other
// division is headed by a Director already; Industry's head and the Treasurer are not, and Management
// and its key open to Directors only (commandScopeErr, issuePortalKey). Director opens the wallet, so
// the officer's tenure applies. want is the office dealt. Pure.
func headHoldsDirector(rm roleMember, want uint64, industryDirector int64) bool {
	if !rm.Human || rm.Untenured {
		return false
	}
	return rm.CharID == industryDirector || (rm.Treasurer && want&corp.RoleAccountant != 0)
}

// rebaseRoles applies the change from base to want onto what the row holds now. Pure.
func rebaseRoles(base, want, now uint64) uint64 {
	return (now | (want &^ base)) &^ (base &^ want)
}
