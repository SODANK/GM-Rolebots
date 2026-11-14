package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"

	evedb "gurimoon/db"
)

// Item plumbing the starbase errands share: moving a quantity of a type out
// of an office division, a ship's hold or a structure bay into somewhere
// else, keeping ownership straight. Every move is one transaction with the
// source rows locked, so two carriers can never both take the last stack.

// posWorkMarks remembers which corporations have starbase errands posted, so
// the planner's pre-emption is a map read for everybody else.
var posWorkMarks sync.Map // corpID to struct{}

func markPosWork(corpID int64)         { posWorkMarks.Store(corpID, struct{}{}) }
func clearPosWork(corpID int64)        { posWorkMarks.Delete(corpID) }
func corpHasPosWork(corpID int64) bool { _, ok := posWorkMarks.Load(corpID); return ok }

// settleCorpBuyRemainder deals with what a corporation supply buy left in the buyer's own hangar
// (bought with the corporation's advance, but more than the hold took). It goes to the
// corporation's office at this station (Materials division), or, with no office here, onto the
// buyer's stock ledger so a sweep can find it.
func (m *Mod) settleCorpBuyRemainder(ctx context.Context, b *botRun, stationID, typeID, qty int64) {
	if qty <= 0 || stationID == 0 {
		return
	}
	if b.CorpID != 0 {
		if officeID, _, err := evedb.GetCorpOfficeAt(ctx, m.db, b.CorpID, stationID); err == nil &&
			officeID != 0 && officeID != stationID {
			if moved, err := m.moveUnits(ctx, b.CharID, stationID, flagHangar, typeID, qty,
				officeID, int64(corpDivMaterials), b.CorpID); err == nil && moved > 0 {
				qty -= moved
			}
		}
	}
	if qty > 0 {
		m.addStock(ctx, b.CharID, stationID, b.SystemID, typeID, m.typeNameOr(ctx, typeID), qty, 0)
	}
}

// moveUnits moves up to qty units of typeID owned by fromOwner at fromLoc/fromFlag (fromFlag < 0
// = any) to toLoc/toFlag, owned by toOwner. It merges into a loose stack at the destination and
// splits a source stack when it takes part of it. A locked item (corpvault.go) is never taken,
// nor a print in a job (flag 111) on an any-flag move. Returns the units moved.
func (m *Mod) moveUnits(ctx context.Context, fromOwner, fromLoc, fromFlag, typeID, qty, toLoc, toFlag, toOwner int64) (int64, error) {
	if qty <= 0 {
		return 0, nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT itemID, quantity, singleton, COALESCE(itemName, '') FROM entity
		 WHERE ownerID = ? AND locationID = ? AND typeID = ? AND (? < 0 OR flag = ?)
		   AND (? >= 0 OR flag <> 111)
		   AND NOT EXISTS (SELECT 1 FROM crpLockedItems l WHERE l.itemID = entity.itemID)
		 ORDER BY singleton, quantity DESC, itemID FOR UPDATE`, fromOwner, fromLoc, typeID, fromFlag, fromFlag, fromFlag)
	if err != nil {
		return 0, fmt.Errorf("moveUnits select: %w", err)
	}
	type stack struct {
		id, qty, singleton int64
		name               string
	}
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.id, &s.qty, &s.singleton, &s.name); err != nil {
			rows.Close()
			return 0, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	moved := int64(0)
	for _, s := range stacks {
		if moved >= qty {
			break
		}
		units := max(s.qty, 1)
		take := min(units, qty-moved)
		var dst int64
		mergeErr := sql.ErrNoRows
		if s.singleton == 0 {
			mergeErr = tx.QueryRowContext(ctx, `
				SELECT itemID FROM entity
				 WHERE ownerID = ? AND locationID = ? AND flag = ? AND typeID = ? AND singleton = 0 AND itemID <> ?
				 ORDER BY itemID LIMIT 1 FOR UPDATE`, toOwner, toLoc, toFlag, typeID, s.id).Scan(&dst)
		}
		switch {
		case mergeErr == nil:
			if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`, take, dst); err != nil {
				return 0, err
			}
			if take == units {
				if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.id); err != nil {
					return 0, err
				}
			} else if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, take, s.id); err != nil {
				return 0, err
			}
		case take == units:
			if _, err := tx.ExecContext(ctx, `
				UPDATE entity SET locationID = ?, flag = ?, ownerID = ? WHERE itemID = ?`,
				toLoc, toFlag, toOwner, s.id); err != nil {
				return 0, err
			}
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, take, s.id); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO entity (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
				VALUES (?, ?, ?, ?, ?, 0, 0, ?, 0, 0, 0, NULL)`, s.name, typeID, toOwner, toLoc, toFlag, take); err != nil {
				return 0, err
			}
		}
		moved += take
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return moved, nil
}

// takeFromOffice moves units out of a corporation office division.
func (m *Mod) takeFromOffice(ctx context.Context, corpID, officeID, flag, typeID, qty, toLoc, toFlag, toOwner int64) (int64, error) {
	return m.moveUnits(ctx, corpID, officeID, flag, typeID, qty, toLoc, toFlag, toOwner)
}

// takeCopyFromOffice moves one blueprint copy of typeID with runs left out of a corporation
// office (any division, never one in a job): preferItemID when it is still there, else the copy
// with the most runs. Never an original: a print errand must not pick one up by type. Returns
// the itemID moved, 0 when there was no copy to take.
func (m *Mod) takeCopyFromOffice(ctx context.Context, corpID, officeID, typeID, preferItemID, toLoc, toFlag, toOwner int64) (int64, error) {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `
		SELECT e.itemID FROM entity e
		  JOIN invBlueprints b ON b.blueprintID = e.itemID AND b.copy = 1
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.typeID = ? AND e.flag <> 111
		   AND b.licensedProductionRunsRemaining > 0
		   AND NOT EXISTS (SELECT 1 FROM crpLockedItems l WHERE l.itemID = e.itemID)
		 ORDER BY (e.itemID = ?) DESC, b.licensedProductionRunsRemaining DESC, e.itemID
		 LIMIT 1 FOR UPDATE`, corpID, officeID, typeID, preferItemID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("takeCopyFromOffice select: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = ?, ownerID = ? WHERE itemID = ?`,
		toLoc, toFlag, toOwner, id); err != nil {
		return 0, fmt.Errorf("takeCopyFromOffice move: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// cargoUnitsOwnedBy is how many units of a type an owner has in a ship's hold.
func (m *Mod) cargoUnitsOwnedBy(ctx context.Context, shipID, ownerID, typeID int64) int64 {
	if shipID == 0 {
		return 0
	}
	var n sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT SUM(GREATEST(quantity, 1)) FROM entity WHERE locationID = ? AND flag = 5 AND ownerID = ? AND typeID = ?`,
		shipID, ownerID, typeID).Scan(&n)
	return n.Int64
}

// consumeCargoUnits deletes n units of a type from a ship's hold (a probe
// launched, a charge fired). Reports whether there were n to take.
func (m *Mod) consumeCargoUnits(ctx context.Context, shipID, ownerID, typeID, n int64) bool {
	if m.cargoUnitsOwnedBy(ctx, shipID, ownerID, typeID) < n {
		return false
	}
	// Move them to location 0 (nowhere), then delete what landed there.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity WHERE locationID = ? AND flag = 5 AND ownerID = ? AND typeID = ?
		 ORDER BY quantity, itemID FOR UPDATE`, shipID, ownerID, typeID)
	if err != nil {
		return false
	}
	type st struct{ id, q int64 }
	var list []st
	for rows.Next() {
		var s st
		if rows.Scan(&s.id, &s.q) == nil {
			list = append(list, s)
		}
	}
	rows.Close()
	left := n
	for _, s := range list {
		if left <= 0 {
			break
		}
		q := max(s.q, 1)
		if q <= left {
			if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.id); err != nil {
				return false
			}
			left -= q
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, left, s.id); err != nil {
				return false
			}
			left = 0
		}
	}
	return left == 0 && tx.Commit() == nil
}

// officeAt is the corporation's office item at a station (0 for none).
func (m *Mod) officeAt(ctx context.Context, corpID, stationID int64) int64 {
	if stationID == 0 {
		return 0
	}
	id, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, corpID, stationID)
	if id == stationID {
		return 0
	}
	return id
}

// posKitOfficeNear is the office whose Starbase Kit division holds a type nearest the pilot, within
// maxJumps; 0, 0 when none is. Ties go to the bigger stack.
func (m *Mod) posKitOfficeNear(ctx context.Context, b *botRun, typeID int64, maxJumps int) (stationID, officeID int64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.itemID FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		 WHERE o.corporationID = ? AND e.flag = ? AND e.typeID = ?
		 GROUP BY o.stationID, o.itemID
		 ORDER BY SUM(e.quantity) DESC`, b.CorpID, corpDivStarbase, typeID)
	if err != nil {
		return 0, 0
	}
	type kitAt struct{ station, office int64 }
	var kits []kitAt
	for rows.Next() {
		var k kitAt
		if rows.Scan(&k.station, &k.office) == nil {
			kits = append(kits, k)
		}
	}
	rows.Close()
	best := -1
	for _, k := range kits {
		j := m.jumpsTo(ctx, b, m.stationSystemID(ctx, k.station))
		if j < 0 || j > maxJumps || (best >= 0 && j >= best) {
			continue
		}
		best, stationID, officeID = j, k.station, k.office
	}
	return stationID, officeID
}

// payCorpWage pays a member for a trip, a survey or a carried print. A
// bot-run corporation credits the member's labour record instead (paid in the
// weekly payroll, corppayroll.go) and reports true: the work is accounted
// for. Anywhere else the ISK moves now.
func (m *Mod) payCorpWage(ctx context.Context, corpID, charID int64, amount float64, why string) bool {
	amount = math.Floor(amount)
	if amount < 1 || corpID == 0 || charID == 0 {
		return false
	}
	if payrollManaged(corpID) {
		m.creditLabour(ctx, corpID, charID, labourKindOf(why), amount, why+" — "+m.charNameOr(ctx, charID))
		return true
	}
	_, bal, err := m.corpWithdraw(ctx, corpID, charID, amount, "Corporation pay: "+why)
	if err != nil {
		if !errors.Is(err, evedb.ErrInsufficientFunds) {
			rlogf("rolebots: pay %d for %s: %v", charID, why, err)
		}
		return false
	}
	m.pushBalance(charID, bal)
	m.corpBook(ctx, corpID, lineWages, -amount, charID, why+" — "+m.charNameOr(ctx, charID))
	return true
}

// shipHoldRoom is the free m³ in the pilot's current ship.
func (m *Mod) shipHoldRoom(ctx context.Context, b *botRun) float64 {
	return math.Max(0, m.shipCargoCapacity(ctx, b)-m.cargoUsed(ctx, b.ShipID))
}

// typeVolume is one unit's m³ (packaged volume for a structure).
func (m *Mod) typeVolume(ctx context.Context, typeID int64) float64 {
	r, _ := m.typeStaticRec(ctx, typeID)
	return r.Volume
}

// boardHauler puts the pilot in its best cargo hull docked here: the ship its
// trade fit lives in when that is parked at this station, else whatever it
// is in, else a runabout. Reports the hold's free room.
func (m *Mod) boardHauler(ctx context.Context, b *botRun, why string) float64 {
	if b.StationID != 0 {
		if a, _, err := m.shipForActivity(ctx, b.CharID, ActTrade); err == nil && a != nil && a.ShipExists &&
			a.ShipItemID != b.ShipID {
			var loc int64
			_ = m.db.QueryRowContext(ctx, `SELECT locationID FROM entity WHERE itemID = ?`, a.ShipItemID).Scan(&loc)
			if loc == b.StationID {
				cur := m.shipHoldRoom(ctx, b)
				if err := m.boardShip(ctx, b, a.ShipItemID); err == nil {
					if room := m.shipHoldRoom(ctx, b); room > cur {
						m.logBot(b.CharID, logInfo, "corp", "boarded its hauler for %s", why)
					}
				}
			}
		}
		if _, ok := m.ensureRunabout(ctx, b, why); !ok {
			return 0
		}
	}
	return m.shipHoldRoom(ctx, b)
}

func init() { registerCorpForget("posWorkMarks", clearPosWork) }
