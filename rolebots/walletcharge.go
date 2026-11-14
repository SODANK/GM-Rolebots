package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// chargeSpendableTx debits amount from a pilot's wallet inside tx for a discretionary spend (a
// courier reward, an escrow listing fee) and returns the balance after the debit, read in the same
// transaction so the journal row written with it is exact. The test and the debit are one
// conditional UPDATE: a credit or debit landing after any earlier balance read is kept, and the
// wallet floor (spendable, broke.go) holds even then. ok=false means the wallet fell short and
// nothing changed; the caller rolls back. A charge the pilot owes regardless of the floor (a
// broker fee) goes through evedb.TryChargeISK instead.
func chargeSpendableTx(ctx context.Context, tx *sql.Tx, charID int64, amount float64) (ok bool, after float64, err error) {
	if amount > 0 {
		res, err := tx.ExecContext(ctx,
			`UPDATE character_ SET balance = balance - ? WHERE characterID = ? AND balance >= ?`,
			amount, charID, spendableChargeNeed(amount))
		if err != nil {
			return false, 0, fmt.Errorf("debit wallet: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, 0, fmt.Errorf("debit wallet: %w", err)
		}
		if n == 0 {
			return false, 0, nil
		}
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&after); err != nil {
		return false, 0, fmt.Errorf("read wallet: %w", err)
	}
	return true, after, nil
}

// spendableChargeNeed is the least balance from which a discretionary spend of amount may be
// taken: the balance whose spendable share covers it (balanceNeededFor), never under the amount
// itself. It is the SQL guard of chargeSpendableTx, so it must agree with spendable exactly.
func spendableChargeNeed(amount float64) float64 {
	return math.Max(amount, balanceNeededFor(amount))
}
