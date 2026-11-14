package rolebots

// The tithe (rolebot_alliance_ledger) and the alliance book
// (rolebot_alliance_book).
//
// An alliance has no wallet, so what the executor fronts in its name (a war's CONCORD fee and weekly bill)
// is shared by titheShares (alliancevote.go) into one ledger row per member, a debt to the executor. The
// member's own think pays it (payTithes) with evedb.TransferISK under the canPay rule, and refuses it past
// its due date when it cannot; refusals are what the leave and kick verdicts count.

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

const (
	titheKindWar     = "war"
	titheKindProject = "project"

	titheStatusPending = "pending"
	titheStatusPaid    = "paid"
	titheStatusRefused = "refused"
	// titheStatusVoid closes a share its debtor no longer owes: it left the alliance (titheCloseOut).
	titheStatusVoid = "void"

	// titheDueSec is how long a member has to pay a tithe before refusing it counts against it: a week,
	// the cadence of the bills it covers.
	titheDueSec = 7 * 86400
	// titheRefusalWindowSec is how far back refusals are counted.
	titheRefusalWindowSec = 90 * 86400
)

// titheRow is one line of the ledger.
type titheRow struct {
	ID, AllianceID       int64
	Executor, Debtor     int64
	Kind                 string
	RefID                int64
	Amount               float64
	RequestedAt, DueAt   int64
	PaidAt               int64
	Status, Reason       string
	DebtorName, ExecName string
}

// titheIssue writes one ledger row per member share, the executor's own
// excepted (it fronted the sum; its share is its own books' allwar line).
func (m *Mod) titheIssue(ctx context.Context, allianceID, executor int64, shares map[int64]float64,
	kind string, refID int64, reason string) error {
	now := clock.Unix()
	for debtor, amt := range shares {
		if debtor == executor || amt <= 0 {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_alliance_ledger
			  (allianceID, executorCorpID, debtorCorpID, kind, refID, amount, requestedAt, dueAt, status, reason)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
			allianceID, executor, debtor, kind, refID, amt, now, now+titheDueSec, reason); err != nil {
			return fmt.Errorf("titheIssue: %w", err)
		}
	}
	return nil
}

// titheOpen lists a corporation's unpaid tithes to its current alliance, oldest first.
func (m *Mod) titheOpen(ctx context.Context, allianceID, debtor int64) ([]titheRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT titheID, allianceID, executorCorpID, debtorCorpID, kind, refID, amount,
		       requestedAt, dueAt, paidAt, status, reason
		  FROM rolebot_alliance_ledger
		 WHERE allianceID = ? AND debtorCorpID = ? AND status = 'pending'
		 ORDER BY requestedAt`, allianceID, debtor)
	if err != nil {
		return nil, fmt.Errorf("titheOpen: %w", err)
	}
	defer rows.Close()
	var out []titheRow
	for rows.Next() {
		var t titheRow
		if err := rows.Scan(&t.ID, &t.AllianceID, &t.Executor, &t.Debtor, &t.Kind, &t.RefID, &t.Amount,
			&t.RequestedAt, &t.DueAt, &t.PaidAt, &t.Status, &t.Reason); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// titheRefusals counts a member's refused war tithes to this alliance in the window, the number the leave
// and kick verdicts read. A refused project share is the consortium's business, not the bloc's.
func (m *Mod) titheRefusals(ctx context.Context, allianceID, debtor int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_alliance_ledger
		 WHERE allianceID = ? AND debtorCorpID = ? AND kind = ? AND status = 'refused' AND dueAt > ?`,
		allianceID, debtor, titheKindWar, clock.Unix()-titheRefusalWindowSec).Scan(&n)
	return n
}

// titheCloseOut voids a corporation's pending tithes to every alliance but keep (0: to all), with the
// cause on the row: a corporation that left owes the old executor nothing more, and its debts must not
// follow it into the next alliance.
func (m *Mod) titheCloseOut(ctx context.Context, debtor, keep int64, why string) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_alliance_ledger
		   SET status = ?, reason = LEFT(CONCAT(reason, ' — ', ?), 255)
		 WHERE debtorCorpID = ? AND status = 'pending' AND allianceID <> ?`,
		titheStatusVoid, why, debtor, keep); err != nil {
		rlogf("rolebots: tithe close-out corp %d: %v", debtor, err)
	}
}

// titheBalances sums an alliance's ledger per debtor: owed (pending) and
// paid, for the panel.
type titheBalance struct {
	Owed, Paid float64
	Refused    int
}

func (m *Mod) titheBalances(ctx context.Context, allianceID int64) map[int64]*titheBalance {
	out := map[int64]*titheBalance{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT debtorCorpID, status, COUNT(*), COALESCE(SUM(amount), 0)
		  FROM rolebot_alliance_ledger WHERE allianceID = ?
		 GROUP BY debtorCorpID, status`, allianceID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var debtor int64
		var status string
		var n int
		var sum float64
		if rows.Scan(&debtor, &status, &n, &sum) != nil {
			continue
		}
		b := out[debtor]
		if b == nil {
			b = &titheBalance{}
			out[debtor] = b
		}
		switch status {
		case titheStatusPending:
			b.Owed += sum
		case titheStatusPaid:
			b.Paid += sum
		case titheStatusRefused:
			b.Refused += n
		}
	}
	return out
}

// payTithes is the member's duty: every open tithe it can carry is paid to the executor. One it cannot
// is refused once past due, with the cause on the row. Both sides' books take the line.
// Rows to an alliance the corporation is no longer in are voided first (titheCloseOut).
func (m *Mod) payTithes(ctx context.Context, c *corpCEO, ch Charter, allianceID int64) error {
	m.titheCloseOut(ctx, c.CorpID, allianceID, "left the alliance")
	open, err := m.titheOpen(ctx, allianceID, c.CorpID)
	if err != nil || len(open) == 0 {
		return err
	}
	now := clock.Unix()
	for _, t := range open {
		if t.Executor == c.CorpID {
			// A row that outlived a change of executor: nothing to pay
			// ourselves. Close it.
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_alliance_ledger SET status = 'paid', paidAt = ?, reason = 'we hold the seat now' WHERE titheID = ?`, now, t.ID)
			continue
		}
		if m.canPay(ctx, c.CorpID, ch, t.Amount, 0) {
			reason := evedb.JournalReason(fmt.Sprintf("Alliance tithe: %s", t.Reason))
			if _, _, err := m.corpWithdraw(ctx, c.CorpID, t.Executor, t.Amount, reason); err != nil {
				m.logBot(c.CharID, logError, "ceo", "alliance tithe %d: %v", t.ID, err)
				continue
			}
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_alliance_ledger SET status = 'paid', paidAt = ? WHERE titheID = ?`, now, t.ID)
			m.corpBook(ctx, c.CorpID, lineTithe, -t.Amount, t.Executor, t.Reason)
			m.corpBook(ctx, t.Executor, lineTithe, t.Amount, c.CorpID, t.Reason)
			payee := "the executor"
			if t.Kind == titheKindSovTower {
				// The builder holds the money for the tower (alliancesovpledge.go).
				payee = "the builder"
				m.refreshSovPledgeCommit(ctx, t.Executor, t.RefID)
			}
			m.ceoLog(ctx, c, powerAlliance, "tithe", t.Executor, t.Amount,
				"paid %s to %s %s — %s", iskString(t.Amount), payee, ownerNameOf(ctx, m.db, t.Executor), t.Reason)
			m.allianceBook(ctx, t.AllianceID, c.CorpID, "tithe", t.Executor, t.Amount, "%s paid its %s share — %s",
				ownerNameOf(ctx, m.db, c.CorpID), iskString(t.Amount), t.Reason)
			continue
		}
		if now >= t.DueAt {
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_alliance_ledger SET status = 'refused', reason = CONCAT(reason, ' — the treasury cannot carry it') WHERE titheID = ?`, t.ID)
			m.ceoLog(ctx, c, powerAlliance, "refuse", t.Executor, t.Amount,
				"could not pay %s of alliance tithe — %s", iskString(t.Amount), t.Reason)
			m.allianceBook(ctx, t.AllianceID, c.CorpID, "refused", t.Executor, t.Amount, "%s could not pay its %s share — %s",
				ownerNameOf(ctx, m.db, c.CorpID), iskString(t.Amount), t.Reason)
		}
	}
	return nil
}

// ── the book ────────────────────────────────────────────────────────────────

// allianceBook records one decision taken in the alliance's name, for the
// panel's feed. Best effort, like corpBook: the thing has already happened.
func (m *Mod) allianceBook(ctx context.Context, allianceID, actorCorp int64, verb string, subjectID int64, amount float64, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	if len(detail) > 255 {
		detail = detail[:255]
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_alliance_book (allianceID, actorCorpID, at, verb, subjectID, amount, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, allianceID, actorCorp, clock.Unix(), verb, subjectID, amount, detail); err != nil {
		rlogf("rolebots: alliance book: %v", err)
	}
}

// allianceBookEntry is one row of the book as the panel reads it.
type allianceBookEntry struct {
	AllianceID, ActorCorp int64
	At                    int64
	Verb                  string
	SubjectID             int64
	Amount                float64
	Detail                string
	ActorName             string
}

// allianceBookRecent lists the newest entries of one alliance's book.
func (m *Mod) allianceBookRecent(ctx context.Context, allianceID int64, limit int) ([]allianceBookEntry, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT b.allianceID, b.actorCorpID, b.at, b.verb, b.subjectID, b.amount, b.detail,
		       COALESCE(c.corporationName, '')
		  FROM rolebot_alliance_book b
		  LEFT JOIN corporation c ON c.corporationID = b.actorCorpID
		 WHERE b.allianceID = ?
		 ORDER BY b.at DESC, b.entryID DESC
		 LIMIT ?`, allianceID, limit)
	if err != nil {
		return nil, fmt.Errorf("allianceBookRecent: %w", err)
	}
	defer rows.Close()
	var out []allianceBookEntry
	for rows.Next() {
		var e allianceBookEntry
		if err := rows.Scan(&e.AllianceID, &e.ActorCorp, &e.At, &e.Verb, &e.SubjectID, &e.Amount, &e.Detail, &e.ActorName); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
