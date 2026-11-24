package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	evedb "gurimoon/db"
)

// Pay a human chair's corporation owes. It runs no payroll, so work is paid the moment it is done, and
// a wallet that runs short at that moment would otherwise keep the work and pay nothing. What could
// not be paid is written down, held against free capital like any promise (corpcommit.go), and paid in
// the order it was earned as soon as the wallet covers it (humanChairPass).

const (
	owedCommit     = "owed"
	owedPerPass    = 40
	owedKeepMemory = 90 * 86400
)

func ensureOwedTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rolebot_corp_owed (
			owedID        BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			amount        DOUBLE NOT NULL,
			why           VARCHAR(64) NOT NULL DEFAULT '',
			owedAt        BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (owedID),
			KEY idx_rbowed_corp (corporationID, owedID)
		)`)
	if err != nil {
		return fmt.Errorf("ensureOwedTable: %w", err)
	}
	return nil
}

// oweWage writes down pay a human chair's wallet could not cover. Reports whether it is accounted for;
// any other corporation owes nothing and the caller tries again.
func (m *Mod) oweWage(ctx context.Context, corpID, charID int64, amount float64, why string) bool {
	if _, human := humanChaired(corpID); !human {
		return false
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_owed (corporationID, characterID, amount, why, owedAt) VALUES (?, ?, ?, ?, ?)`,
		corpID, charID, amount, clampText(why, 64), clock.Unix())
	if err != nil {
		rlogf("rolebots: owe %d for %s: %v", charID, why, err)
		return false
	}
	id, _ := res.LastInsertId()
	if err := m.openCommit(ctx, corpID, owedCommit, id, amount); err != nil {
		rlogf("rolebots: %v", err)
	}
	return true
}

// payOwed pays what the wallet can now cover, oldest first, and stops at the first it cannot.
func (m *Mod) payOwed(ctx context.Context, corpID int64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT owedID, characterID, amount, why, owedAt FROM rolebot_corp_owed
		 WHERE corporationID = ? ORDER BY owedID LIMIT ?`, corpID, owedPerPass)
	if err != nil {
		return
	}
	type debt struct {
		id, charID, at int64
		amount         float64
		why            string
	}
	var list []debt
	for rows.Next() {
		var d debt
		if rows.Scan(&d.id, &d.charID, &d.amount, &d.why, &d.at) == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	for _, d := range list {
		if ctx.Err() != nil {
			return
		}
		if clock.Unix()-d.at < owedKeepMemory {
			_, bal, err := m.corpWithdraw(ctx, corpID, d.charID, d.amount, "Corporation pay: "+d.why)
			if errors.Is(err, evedb.ErrInsufficientFunds) {
				return
			}
			if err != nil {
				rlogf("rolebots: pay owed %d to %d: %v", d.id, d.charID, err)
				continue
			}
			m.pushBalance(d.charID, bal)
			m.corpBook(ctx, corpID, lineWages, -d.amount, d.charID, d.why+" — "+m.charNameOr(ctx, d.charID))
		}
		// Paid, or too old to mean anything: off the books either way.
		if _, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_owed WHERE owedID = ?`, d.id); err != nil {
			rlogf("rolebots: owed %d: %v", d.id, err)
			return
		}
		m.closeCommit(ctx, owedCommit, d.id)
	}
}
