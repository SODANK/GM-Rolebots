package rolebots

import (
	"context"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// Reloading in space. A gun holds one module's worth of rounds (evedb.ModuleChargeUnits) and the
// rest of what the fit carries rides in the hold (ammoReloadsFor). Without a reload in space the
// gun went quiet when its load ran out, and buildWeapon drops a gun with nothing in it, so a ship
// with thousands of rounds in the hold read as unarmed: no fight, no siege fire. A pilot reloads
// from its own hold the way a player does: no travel, no ISK, a reload's pause.

// gunReloadDelay is how long a gun is quiet while a fresh load goes in from the hold.
const gunReloadDelay = 10 * time.Second

// reloadShipLocks serialises reloads per ship: the combat loop, the siege guns and a loadout
// build may all reach for the same hold, and takeFromCargo is a read then a write.
// Striped rather than one mutex per ship kept forever: every hull ever reloaded in left an entry.
// No caller holds one ship's lock while taking another's, so sharing a stripe cannot deadlock.
var reloadShipLocks [256]sync.Mutex

func reloadShipLock(shipID int64) *sync.Mutex {
	return &reloadShipLocks[uint64(shipID)%uint64(len(reloadShipLocks))]
}

// holdCharges is the charges loose in a ship's hold, typeID to units.
func (m *Mod) holdCharges(ctx context.Context, shipID int64) (map[int64]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 8
		 WHERE e.locationID = ? AND e.flag = 5
		 GROUP BY e.typeID`, shipID)
	if err != nil {
		return nil, fmt.Errorf("holdCharges: %w", err)
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var typeID, qty int64
		if rows.Scan(&typeID, &qty) == nil && qty > 0 {
			out[typeID] = qty
		}
	}
	return out, rows.Err()
}

// loadedHighFlags is the high slots of a ship that hold a charge, in one read.
func (m *Mod) loadedHighFlags(ctx context.Context, shipID int64) (map[int64]bool, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.flag
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 8
		 WHERE e.locationID = ? AND e.flag BETWEEN ? AND ?`, shipID, flagHiFirst, flagHiFirst+7)
	if err != nil {
		return nil, fmt.Errorf("loadedHighFlags: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var flag int64
		if rows.Scan(&flag) == nil {
			out[flag] = true
		}
	}
	return out, rows.Err()
}

// pickHoldCharge is the hold charge to load into a module: prefer if it is aboard and accepted,
// else the accepted charge with the most units aboard, ties to the lower typeID. 0 when the hold
// has nothing the module fires.
func pickHoldCharge(hold map[int64]int64, prefer int64, accepts func(int64) bool) int64 {
	if prefer != 0 && hold[prefer] > 0 && accepts(prefer) {
		return prefer
	}
	var best, bestQty int64
	for typeID, qty := range hold {
		if qty <= 0 || !accepts(typeID) {
			continue
		}
		if qty > bestQty || (qty == bestQty && typeID < best) {
			best, bestQty = typeID, qty
		}
	}
	return best
}

// reloadSlotLocked fills the module in one slot from the hold, the caller holding the ship's
// reload lock. A slot already holding a charge is topped up with that charge only; an empty slot
// takes pickHoldCharge's round. A durable charge (a crystal) already loaded is never topped up.
// Reports the units moved.
func (m *Mod) reloadSlotLocked(ctx context.Context, charID, shipID, flag, moduleTypeID, prefer int64,
	hold map[int64]int64) (int64, error) {

	lc, err := evedb.GetLoadedCharge(ctx, m.db, shipID, flag)
	if err != nil {
		return 0, err
	}
	var chargeTypeID, loaded int64
	if lc != nil {
		if groupID, _, gerr := evedb.GetGroupAndCategory(ctx, m.db, lc.TypeID); gerr == nil && durableChargeGroups[groupID] {
			return 0, nil
		}
		chargeTypeID, loaded = lc.TypeID, lc.Quantity
		if hold[chargeTypeID] <= 0 {
			return 0, nil
		}
	} else {
		chargeTypeID = pickHoldCharge(hold, prefer, func(t int64) bool { return m.chargeAccepts(ctx, t, moduleTypeID) })
		if chargeTypeID == 0 {
			return 0, nil
		}
	}
	room, err := m.slotChargeRoom(ctx, shipID, flag, chargeTypeID, loaded)
	if err != nil || room <= 0 {
		return 0, err
	}
	if room > hold[chargeTypeID] {
		room = hold[chargeTypeID]
	}
	moved, err := m.takeFromCargo(ctx, shipID, chargeTypeID, room)
	if err != nil || moved == 0 {
		return 0, err
	}
	hold[chargeTypeID] -= moved
	if lc != nil {
		err = evedb.LoadChargeIntoModule(ctx, m.db, lc.ItemID, shipID, flag, loaded+moved)
	} else {
		var itemID int64
		if itemID, err = evedb.CreateItem(ctx, m.db, chargeTypeID, charID, shipID, int(moved)); err == nil {
			err = evedb.LoadChargeIntoModule(ctx, m.db, itemID, shipID, flag, moved)
		}
	}
	if err != nil {
		return 0, fmt.Errorf("reload slot %d: %w", flag, err)
	}
	return moved, nil
}

// reloadEmptyGuns fills every high-slot module of shipID that holds no charge out of the ship's
// own hold. A slot with rounds in it is left for the empty-gun reload (reloadGunFromHold). Run
// before a loadout is read, so a ship whose guns ran dry on the last fight comes out armed.
// Reports the slots filled.
func (m *Mod) reloadEmptyGuns(ctx context.Context, charID, shipID int64, fitted []evedb.FittedModule) int {
	if shipID == 0 || len(fitted) == 0 {
		return 0
	}
	mu := reloadShipLock(shipID)
	mu.Lock()
	defer mu.Unlock()
	loaded, err := m.loadedHighFlags(ctx, shipID)
	if err != nil {
		return 0
	}
	var hold map[int64]int64
	filled, units := 0, int64(0)
	for _, fm := range fitted {
		if fm.Flag < flagHiFirst || fm.Flag > flagHiFirst+7 || loaded[fm.Flag] {
			continue
		}
		if hold == nil {
			var err error
			if hold, err = m.holdCharges(ctx, shipID); err != nil || len(hold) == 0 {
				return filled
			}
		}
		moved, err := m.reloadSlotLocked(ctx, charID, shipID, fm.Flag, fm.TypeID, 0, hold)
		if err != nil {
			rlogf("rolebots: reload ship=%d slot=%d: %v", shipID, fm.Flag, err)
			continue
		}
		if moved > 0 {
			filled++
			units += moved
		}
	}
	if filled > 0 {
		m.logBot(charID, logInfo, "fit", "reloaded %d empty gun(s) from the hold (%d rounds)", filled, units)
	}
	return filled
}

// reloadGunFromHold refills one gun that has just run dry, from the projection's own hold,
// preferring the round it was firing. Reports whether anything went in; the caller rests the gun
// gunReloadDelay. It runs on the grid tick, as consumeCharge does, with the same short deadline.
func (m *Mod) reloadGunFromHold(eb *embodiedBot, w *botWeapon) bool {
	if eb == nil || w == nil || eb.shipID == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mu := reloadShipLock(eb.shipID)
	mu.Lock()
	defer mu.Unlock()
	hold, err := m.holdCharges(ctx, eb.shipID)
	if err != nil || len(hold) == 0 {
		return false
	}
	moved, err := m.reloadSlotLocked(ctx, eb.charID, eb.shipID, w.SlotFlag, w.TypeID, w.ChargeTypeID, hold)
	if err != nil {
		rlogf("rolebots: reload %s slot=%d: %v", eb.charName, w.SlotFlag, err)
		return false
	}
	if moved > 0 {
		m.logBot(eb.charID, logInfo, "combat", "%s reloaded from the hold (%d rounds)", w.Name, moved)
	}
	return moved > 0
}
