package rolebots

// When a refined stack is worth an order slot. A trip's minerals land in a fresh
// hangar stack (the wire has no quantity-modify, db/marketorders.go), so listing
// each trip would fill the book with small identical lines, one broker fee each.
// Minerals wait for a lot and go out as one order. Ways past the gate:
//
//	unit floor   a lot of this mineral (mineralListFloors)
//	value floor  fallback for a type with no floor; the litter line under the age-out
//	age-out      a stack that never reaches its floor still reaches the market
//
// Ore sold to an NPC bid skips the gate: it fills at once and uses no order slot.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// The eight refined minerals, by typeID. Named because the floors below are
// per type and a bare 11399 in a map literal is not reviewable.
const (
	typeTritanium = 34
	typePyerite   = 35
	typeMexallon  = 36
	typeIsogen    = 37
	typeNocxium   = 38
	typeZydrine   = 39
	typeMegacyte  = 40
	typeMorphite  = 11399
)

// mineralListFloors is how many units of each mineral make a lot worth listing
// for an even-tempered pilot. Each row is worth roughly the same money (19k-85k
// ISK); Morphite sits above the band, as deep-null Mercoxit yields it.
var mineralListFloors = map[int64]int64{
	typeTritanium: 10000,
	typePyerite:   5000,
	typeMexallon:  2000,
	typeIsogen:    500,
	typeNocxium:   200,
	typeZydrine:   100,
	typeMegacyte:  50,
	typeMorphite:  10,
}

const (
	// mineralListMinValue is the litter line: what a stack must be worth to be a lot
	// at all. It stands in for the unit floor on a type with none, and sits under the
	// age-out. Same number as lootListMinValue and stockMinValue.
	mineralListMinValue = 20_000.0

	// mineralHoldMaxDays is how long a stack short of its floor waits before it goes
	// out anyway. It is the safety valve: without it a prospector on a thin pocket
	// never reaches the floor and the ledger shortage (minerals.go) is never filled.
	mineralHoldMaxDays = 5

	mineralHoldMaxSec = mineralHoldMaxDays * 86400
)

// mineralBatchMulDefault is the even-tempered pilot's multiplier on every floor
// above: 1.0. Patience raises it and nothing lowers it (tuning.go), since the
// table is the minimum lot.
const mineralBatchMulDefault = 1.0

// Why a stack was listed or held. Returned beside the verdict so the log says
// which rule fired.
type mineralBatchReason string

const (
	mineralBatchFloor  = mineralBatchReason("floor")  // a proper lot
	mineralBatchAged   = mineralBatchReason("aged")   // waited long enough
	mineralBatchFamine = mineralBatchReason("famine") // short everywhere; a part load beats none
	mineralBatchHold   = mineralBatchReason("hold")   // not yet
	mineralBatchLitter = mineralBatchReason("litter") // aged out and still not worth an order
)

// mineralUnitFloor is the unit count this pilot calls a lot of one mineral. A
// type with no row is floored by value: the unit count that clears
// mineralListMinValue at the going ask, so a new refined type does not list in
// threes.
func mineralUnitFloor(typeID int64, price, mul float64) int64 {
	// Clamped at 1: the table is a minimum lot and no personality, config or
	// arithmetic slip may list under it.
	if mul < 1 {
		mul = 1
	}
	if base, ok := mineralListFloors[typeID]; ok {
		f := int64(math.Round(float64(base) * mul))
		if f < 1 {
			f = 1
		}
		return f
	}
	if price <= 0 {
		return math.MaxInt64 // unpriceable: never a lot, hold it
	}
	f := int64(math.Ceil(mineralListMinValue * mul / price))
	if f < 1 {
		f = 1
	}
	return f
}

// mineralBatchReady decides whether a hangar stack goes on the market now. Pure.
// heldSec is how long the pilot has held the stack here (0 means never held, not
// timed out). maxHoldSec is the age-out, which a bare region shortens
// (mineralHoldSecUnder). famine skips the unit floor but not the litter line.
func mineralBatchReady(typeID, qty int64, price, mul float64, heldSec, maxHoldSec int64, famine bool) (bool, mineralBatchReason) {
	if qty <= 0 {
		return false, mineralBatchHold
	}
	if qty >= mineralUnitFloor(typeID, price, mul) {
		return true, mineralBatchFloor
	}
	if famine && price*float64(qty) >= mineralListMinValue*mul {
		return true, mineralBatchFamine
	}
	if maxHoldSec <= 0 {
		maxHoldSec = mineralHoldMaxSec
	}
	if heldSec < maxHoldSec {
		return false, mineralBatchHold
	}
	// Timed out. It still has to be worth an order slot: the age-out exists
	// to stop a real supply being stranded, not to put litter on the book.
	if price*float64(qty) >= mineralListMinValue*mul {
		return true, mineralBatchAged
	}
	return false, mineralBatchLitter
}

// noteMineralHold records that this pilot is sitting on a stack, and since when.
// sinceAt is written once and left alone: the clock runs from the first trip that
// could not fill a lot, so topping up the same stack does not reset the age-out.
func (m *Mod) noteMineralHold(ctx context.Context, b *botRun, typeID, units int64) {
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_mineral_hold
			(characterID, stationID, typeID, typeName, units, sinceAt, lastAt)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			units    = VALUES(units),
			typeName = VALUES(typeName),
			lastAt   = VALUES(lastAt)`,
		b.CharID, b.StationID, typeID, m.typeNameOr(ctx, typeID),
		units, clock.Unix(), clock.Unix())
	if err != nil {
		m.logBot(b.CharID, logError, "market", "mineral hold ledger: %v", err)
	}
}

// mineralHeldSince is how long this pilot has held this mineral at this station,
// in seconds; zero when there is no row.
func (m *Mod) mineralHeldSince(ctx context.Context, charID, stationID, typeID int64) int64 {
	var since int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT sinceAt FROM rolebot_mineral_hold
		 WHERE characterID = ? AND stationID = ? AND typeID = ?`,
		charID, stationID, typeID).Scan(&since); err != nil || since <= 0 {
		return 0
	}
	held := clock.Unix() - since
	if held < 0 {
		return 0
	}
	return held
}

// clearMineralHold drops the row once the stack has gone to market. The next
// stack of the same mineral starts its own clock from its own first refine.
func (m *Mod) clearMineralHold(ctx context.Context, charID, stationID, typeID int64) {
	_, _ = m.db.ExecContext(ctx, `
		DELETE FROM rolebot_mineral_hold
		 WHERE characterID = ? AND stationID = ? AND typeID = ?`,
		charID, stationID, typeID)
}

// topUpOwnSellOrder folds a fresh stack into the pilot's own resting sell order
// for the same type at this dock, so supply cycles do not stack duplicates, and
// reports whether it did. Only the first listing meets the lot gates. It pays the
// broker fee a fresh order of the same size would, or does not happen (false, the
// stack stays put); the price stays the order's (nudgeOrder reprices).
func (m *Mod) topUpOwnSellOrder(ctx context.Context, b *botRun, typeID, itemID, qty int64) (bool, error) {
	if qty <= 0 {
		return false, nil
	}
	var orderID int64
	var price float64
	err := m.db.QueryRowContext(ctx, `
		SELECT orderID, price FROM market_orders
		WHERE charID = ? AND stationID = ? AND typeID = ?
		  AND bid = 0 AND orderState = 1
		ORDER BY volRemaining DESC LIMIT 1`,
		b.CharID, b.StationID, typeID).Scan(&orderID, &price)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// What a fresh order of qty at this price pays to rest (marketRates, tradefees.go).
	fee := m.marketRates(ctx, b.CharID, b.StationID).brokerFee(price * float64(qty))

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	// Consume the hangar stack: decrement, or delete an exact match. Zero rows both
	// ways means the stack moved, so list normally instead.
	res, err := tx.ExecContext(ctx,
		`UPDATE entity SET quantity = quantity - ? WHERE itemID = ? AND quantity > ?`,
		qty, itemID, qty)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		res, err = tx.ExecContext(ctx,
			`DELETE FROM entity WHERE itemID = ? AND quantity = ?`, itemID, qty)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return false, nil
		}
	}
	// Grow the order, guarded on it still being open, or the units would vanish
	// into a closed row.
	res, err = tx.ExecContext(ctx, `
		UPDATE market_orders
		   SET volRemaining = volRemaining + ?, volEntered = volEntered + ?
		 WHERE orderID = ? AND orderState = 1`,
		qty, qty, orderID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // filled or pulled since the read; the rollback returns the stack
	}

	// The broker fee, in the same transaction: a wallet that cannot cover it
	// rolls the top-up back and the stack stays in the hangar. As in
	// PlaceSellOrder, the raw balance pays it.
	paid, bal, err := evedb.TryChargeISK(ctx, tx, b.CharID, fee)
	if err != nil {
		return false, err
	}
	if !paid {
		_ = tx.Rollback()
		m.logBot(b.CharID, logInfo, "market",
			"cannot pay the %.0f ISK broker fee to add %d× %s to its standing order — the stack stays in the hangar",
			fee, qty, m.typeNameOr(ctx, typeID))
		return false, nil
	}
	// The core's broker-fee row (journalRef in db/marketorders.go): refType 46, the
	// pilot as row owner and ownerID1, the station as ownerID2.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		b.CharID, win32Now(), evedb.RefTypeBrokerFee, b.CharID, b.StationID,
		-fee, bal, "Broker fee"); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	m.bookTrade(ctx, tradeEntry{
		CharID: b.CharID, Kind: tradeKindFee, TypeID: typeID,
		TypeName: m.typeNameOr(ctx, typeID),
		Qty:      qty, Price: price, Value: fee, Profit: -fee,
		Station: b.StationID, System: b.SystemID, OrderID: orderID,
		Note: fmt.Sprintf("broker fee on %d× added to a standing order", qty),
	})
	// Keep the order sweep's ledger in step with the row it watches.
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_orders
		   SET volEntered = volEntered + ?, volRemaining = volRemaining + ?
		 WHERE orderID = ?`, qty, qty, orderID)

	m.logBot(b.CharID, logInfo, "market",
		"added %d× %s to its standing order at %.2f ISK — one order, not a second one beside it",
		qty, m.typeNameOr(ctx, typeID), price)
	return true, nil
}
