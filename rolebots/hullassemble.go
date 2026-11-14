package rolebots

import (
	"context"
	"database/sql"
	"fmt"

	evedb "gurimoon/db"
)

// Assembling one hull out of a packaged stack.
//
// Setting singleton = 1, quantity = 1 on a stack row destroys the rest of the
// stack, because deliverItem merges bought hulls into any packaged stack of the
// type in the hangar. assembleOneHull takes one unit off the stack instead.

// assembleOneHull unpacks one hull from row itemID and returns the assembled
// hull's itemID: the row itself when already assembled or a single unit, else a
// new singleton split off the stack. One transaction; station hangar (flag 4) only.
func (m *Mod) assembleOneHull(ctx context.Context, itemID int64) (int64, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var typeID, ownerID, locationID, flag, qty, singleton int64
	if err := tx.QueryRowContext(ctx, `
		SELECT typeID, ownerID, locationID, flag, quantity, singleton
		  FROM entity WHERE itemID = ? FOR UPDATE`, itemID).
		Scan(&typeID, &ownerID, &locationID, &flag, &qty, &singleton); err != nil {
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("assembleOneHull: item %d is gone", itemID)
		}
		return 0, fmt.Errorf("assembleOneHull: %w", err)
	}
	if singleton == 1 {
		return itemID, tx.Commit()
	}
	if flag != flagHangar {
		return 0, fmt.Errorf("assembleOneHull: item %d is not in a station hangar (flag %d)", itemID, flag)
	}
	if qty <= 1 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET singleton = 1, quantity = 1 WHERE itemID = ?`, itemID); err != nil {
			return 0, fmt.Errorf("assembleOneHull: %w", err)
		}
		return itemID, tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE entity SET quantity = quantity - 1
		 WHERE itemID = ? AND singleton = 0 AND quantity = ?`, itemID, qty)
	if err != nil {
		return 0, fmt.Errorf("assembleOneHull: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, fmt.Errorf("assembleOneHull: stack %d changed under it", itemID)
	}
	newID, err := evedb.CreateItem(ctx, tx, typeID, ownerID, locationID, 1)
	if err != nil {
		return 0, fmt.Errorf("assembleOneHull: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return newID, nil
}
