package rolebots

import (
	"context"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// Commitments: money a corporation has promised and not yet paid. The industry
// desk promises pay before the work is done: a work order posts its wages, a
// buyback bid ties up escrow, a purchase order advances cash to a member. A
// wallet balance cannot see that, so two posts in one think would each believe
// the whole balance was theirs. Every promise is a row in rolebot_corp_commit and
// every spending decision reads free capital: the wallet, less the charter's
// reserve, less what is still owed on every open promise.

// corpCommitted is what the corporation still owes on its open promises.
func (m *Mod) corpCommitted(ctx context.Context, corpID int64) float64 {
	var owed float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(GREATEST(amount - spent, 0)), 0)
		  FROM rolebot_corp_commit WHERE corporationID = ? AND closedAt = 0`, corpID).Scan(&owed)
	return owed
}

// corpFreeCapital is what the corporation may commit to something new.
func (m *Mod) corpFreeCapital(ctx context.Context, corpID int64, ch Charter) (free, balance float64, err error) {
	balance, err = evedb.GetCorpBalance(ctx, m.db, corpID)
	if err != nil {
		return 0, 0, err
	}
	free = balance - m.corpReserve(ctx, corpID, ch) - m.corpCommitted(ctx, corpID)
	return math.Max(0, free), balance, nil
}

// openCommit records a promise. A second call for the same (kind, ref) replaces
// the amount of the open row, so a re-sized work order is one promise, not two.
func (m *Mod) openCommit(ctx context.Context, corpID int64, kind string, refID int64, amount float64) error {
	if amount < 0 {
		amount = 0
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_commit (corporationID, refKind, refID, amount, openedAt)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE amount = VALUES(amount), closedAt = 0`,
		corpID, kind, refID, amount, clock.Unix())
	if err != nil {
		return fmt.Errorf("openCommit %s/%d: %w", kind, refID, err)
	}
	return nil
}

// spendCommit records that part of a promise was paid.
func (m *Mod) spendCommit(ctx context.Context, kind string, refID int64, paid float64) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_commit SET spent = spent + ? WHERE refKind = ? AND refID = ?`,
		paid, kind, refID)
}

// closeCommit releases whatever is left of a promise.
func (m *Mod) closeCommit(ctx context.Context, kind string, refID int64) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_commit SET closedAt = ? WHERE refKind = ? AND refID = ? AND closedAt = 0`,
		clock.Unix(), kind, refID)
}
