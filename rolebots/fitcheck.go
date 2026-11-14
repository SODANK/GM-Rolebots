package rolebots

import (
	"context"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

// Fit completeness: a hull can be bought and boarded with modules still outstanding (nothing in
// reach sold them). Every plan that needs the ship asks shipMissing first and finishes the shopping
// trip, so an empty hull is not flown to a belt.

// shipMissing compares an assembled ship against its fit template and returns the shopping items
// still outstanding, in fit order. Modules and charges match by (typeID, slot flag), so the wrong
// module in a slot counts the line as missing. Cargo lines count only when the type is absent.
// Drones count by quantity: the line is short once the bay holds less than one rack.
func (m *Mod) shipMissing(ctx context.Context, shipID int64, fit *Fit) ([]ShopItem, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, flag, quantity FROM entity WHERE locationID = ?`, shipID)
	if err != nil {
		return nil, fmt.Errorf("shipMissing: %w", err)
	}
	defer rows.Close()
	var items []shipItemRow
	for rows.Next() {
		var it shipItemRow
		if err := rows.Scan(&it.TypeID, &it.Flag, &it.Qty); err != nil {
			return nil, fmt.Errorf("shipMissing scan: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return missingFromItems(items, fit), nil
}

// shipItemRow is one entity row inside a ship.
type shipItemRow struct {
	TypeID, Flag, Qty int64
}

// missingFromItems is shipMissing without the database: the ship's contents
// against the fit.
func missingFromItems(items []shipItemRow, fit *Fit) []ShopItem {
	type slotKey struct {
		TypeID int64
		Flag   int64
	}
	inSlot := map[slotKey]int64{}  // (type, flag) to quantity
	haveCargo := map[int64]bool{}  // typeID present in cargo (flag 5)
	holdQty := map[int64]int64{}   // typeID to units in cargo, spent by charge lines below
	haveDrone := map[int64]int64{} // typeID to units in the drone bay (flag 87)
	for _, it := range items {
		switch {
		case it.Flag >= flagLoFirst && it.Flag <= 34:
			inSlot[slotKey{it.TypeID, it.Flag}] += it.Qty
		case it.Flag == 5:
			haveCargo[it.TypeID] = true
			holdQty[it.TypeID] += it.Qty
		case it.Flag == 87:
			haveDrone[it.TypeID] += it.Qty
		}
	}

	// A charge line the hold already carries is not missing (reloadFromCargo loads it), even for a
	// missing module. Each line spends the stock it counts on, so one stack cannot satisfy two guns.
	holdCovers := func(typeID, qty int64) bool {
		if holdQty[typeID] >= qty {
			holdQty[typeID] -= qty
			return true
		}
		return false
	}
	var missing []ShopItem
	for _, mod := range fit.Modules {
		if inSlot[slotKey{mod.TypeID, mod.Flag}] == 0 {
			missing = append(missing, ShopItem{
				TypeID: mod.TypeID, Name: mod.TypeName, Qty: 1, Dest: "module", Flag: mod.Flag})
			// A missing module's charge is bought with it on the same stop, unless the hold carries it.
			if mod.ChargeTypeID != 0 && mod.ChargeQty > 0 && !holdCovers(mod.ChargeTypeID, mod.ChargeQty) {
				missing = append(missing, ShopItem{
					TypeID: mod.ChargeTypeID, Name: mod.ChargeName, Qty: mod.ChargeQty,
					Dest: "charge", Flag: mod.Flag})
			}
			continue
		}
		if mod.ChargeTypeID != 0 && mod.ChargeQty > 0 &&
			inSlot[slotKey{mod.ChargeTypeID, mod.Flag}] == 0 &&
			!holdCovers(mod.ChargeTypeID, mod.ChargeQty) {
			missing = append(missing, ShopItem{
				TypeID: mod.ChargeTypeID, Name: mod.ChargeName, Qty: mod.ChargeQty,
				Dest: "charge", Flag: mod.Flag})
		}
	}
	for _, c := range fit.Cargo {
		if !haveCargo[c.TypeID] {
			missing = append(missing, ShopItem{
				TypeID: c.TypeID, Name: c.TypeName, Qty: c.Qty, Dest: "cargo"})
		}
	}
	for _, d := range fit.Drones {
		if short := droneShortfall(d.Qty, haveDrone[d.TypeID]); short > 0 {
			missing = append(missing, ShopItem{
				TypeID: d.TypeID, Name: d.TypeName, Qty: short, Dest: "drone"})
		}
	}
	return procureOrder(missing)
}

// procureOrder puts a shopping list in the order money should be spent: the hull, the rack
// (modules, then drones), the rounds for the guns, then the reload stacks. Stable, so within a kind
// the fit's order holds. Charges sort after the whole rack, so a thin wallet buys the shield
// extender before thousands of rockets.
func procureOrder(list []ShopItem) []ShopItem {
	rank := func(dest string) int {
		switch dest {
		case "hull":
			return 0
		case "module":
			return 1
		case "drone":
			return 2
		case "charge":
			return 3
		}
		return 4
	}
	out := append([]ShopItem(nil), list...)
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].Dest) < rank(out[j].Dest) })
	return out
}

// isRackLine reports whether a shopping line is part of the ship's rack (a module or a drone), as
// against the ammunition that feeds it.
func isRackLine(it ShopItem) bool { return it.Dest == "module" || it.Dest == "drone" }

// droneShortfall is how many drones of one fit line to buy, given how many
// are aboard: zero while the bay still holds at least one rack (the line's
// quantity without its spares), otherwise enough to fill the line again.
func droneShortfall(want, have int64) int64 {
	if want <= 0 || have >= want {
		return 0
	}
	rack := want / (1 + droneSpareSets)
	if rack < 1 {
		rack = 1
	}
	if have >= rack {
		return 0
	}
	return want - have
}

// shipHullMatches reports whether the assembled ship is the hull the fit calls for. A bot holding
// the wrong hull needs a fresh procurement, not an outfit run.
func (m *Mod) shipHullMatches(ctx context.Context, shipID int64, fit *Fit) (bool, error) {
	var typeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, shipID).Scan(&typeID); err != nil {
		return false, fmt.Errorf("shipHullMatches: %w", err)
	}
	return typeID == fit.HullTypeID, nil
}

// ── ammo ────────────────────────────────────────────────────────────────────
//
// The fit is the bot's ammo ledger: each charge line says how many rounds a full gun holds
// (ChargeQty) and the cargo lines add ammoReloadsFor(job) more, so what the ship should have
// aboard is known from the fit. Rounds in the hold are loaded for free; only a hold that is running
// out sends the bot shopping. The two are kept apart.

// ammoRestockFraction is how far the total complement of one charge type may fall before the bot
// buys more: a few rounds fired is not an errand, a ship down to its last third is.
const ammoRestockFraction = 0.34

// ammoTarget is what the fit says a full complement of one charge type
// looks like: a full load in every module that takes it, plus the reload
// stacks in cargo.
func ammoTarget(fit *Fit) map[int64]int64 {
	target := map[int64]int64{}
	for _, mod := range fit.Modules {
		if mod.ChargeTypeID != 0 && mod.ChargeQty > 0 {
			target[mod.ChargeTypeID] += mod.ChargeQty
		}
	}
	ammo := ammoTypes(fit)
	for _, c := range fit.Cargo {
		if ammo[c.TypeID] {
			target[c.TypeID] += c.Qty
		}
	}
	return target
}

// ammoAboard counts the rounds of each charge type the ship can use: loose in the hold, or loaded
// into a module that fires them. A round in a module that cannot fire it does not count.
func (m *Mod) ammoAboard(ctx context.Context, shipID int64, fit *Fit) (map[int64]int64, error) {
	want := ammoTypes(fit)
	if len(want) == 0 {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, e.flag, e.quantity, g.categoryID
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ?`, shipID)
	if err != nil {
		return nil, fmt.Errorf("ammoAboard: %w", err)
	}
	var items []aboardItem
	for rows.Next() {
		var it aboardItem
		if err := rows.Scan(&it.TypeID, &it.Flag, &it.Qty, &it.CategoryID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ammoAboard scan: %w", err)
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return countAmmoAboard(items, want, func(c, mod int64) bool { return m.chargeAccepts(ctx, c, mod) }), nil
}

// aboardItem is one entity row on a ship, as ammoAboard reads it.
type aboardItem struct {
	TypeID, Flag, Qty, CategoryID int64
}

// countAmmoAboard is ammoAboard's arithmetic: wanted charges in the hold
// (flag 5), plus wanted charges loaded in a slot whose module accepts them.
// Pure, so the harness pins it.
func countAmmoAboard(items []aboardItem, want map[int64]bool, accepts func(chargeTypeID, moduleTypeID int64) bool) map[int64]int64 {
	modules := map[int64]int64{}
	for _, it := range items {
		if it.CategoryID == 7 && it.Flag >= flagLoFirst && it.Flag <= flagHiFirst+7 {
			modules[it.Flag] = it.TypeID
		}
	}
	have := map[int64]int64{}
	for _, it := range items {
		if it.CategoryID != 8 || !want[it.TypeID] {
			continue
		}
		switch {
		case it.Flag == 5:
			have[it.TypeID] += it.Qty
		case it.Flag >= flagLoFirst && it.Flag <= flagHiFirst+7:
			if mod := modules[it.Flag]; mod != 0 && accepts(it.TypeID, mod) {
				have[it.TypeID] += it.Qty
			}
		}
	}
	return have
}

// slotChargeRoom is how many more units of chargeTypeID the module in slot flag can take, given
// it already holds loaded of them (capacity rule: evedb.ModuleChargeUnits, as client-driven loads).
// An empty slot, or a module that cannot fire the charge, has no room.
func (m *Mod) slotChargeRoom(ctx context.Context, shipID, flag, chargeTypeID, loaded int64) (int64, error) {
	modER, err := evedb.FindModuleInSlot(ctx, m.db, shipID, flag)
	if err != nil {
		return 0, err
	}
	if modER == nil || !m.chargeAccepts(ctx, chargeTypeID, modER.TypeID) {
		// ModuleChargeUnits floors at one unit, so a miner or remote repairer would otherwise have room
		// for a round it can never fire.
		return 0, nil
	}
	full, err := evedb.ModuleChargeUnits(ctx, m.db, modER.TypeID, chargeTypeID)
	if err != nil {
		return 0, err
	}
	if room := full - loaded; room > 0 {
		return room, nil
	}
	return 0, nil
}

// reloadFromCargo tops every charge line back up to a full load out of the ship's own hold and
// reports how many modules it refilled: no travel, no ISK. The plan is restated against the ship
// first (liveFit) and any round a slot cannot fire comes out (fitslotcharge.go). It runs on a
// boarded ship only.
func (m *Mod) reloadFromCargo(ctx context.Context, b *botRun, shipID int64, fit *Fit) (int, error) {
	if _, err := m.unloadRejectedCharges(ctx, b, shipID); err != nil {
		return 0, err
	}
	live, err := m.liveFit(ctx, b.CharID, shipID, fit)
	if err != nil {
		return 0, err
	}
	return m.reloadLive(ctx, b, shipID, live)
}

// reloadLive is reloadFromCargo against a fit already restated for the ship.
func (m *Mod) reloadLive(ctx context.Context, b *botRun, shipID int64, fit *Fit) (int, error) {
	reloaded := 0
	for _, mod := range fit.Modules {
		if mod.ChargeTypeID == 0 || mod.ChargeQty <= 0 {
			continue
		}
		var loaded int64
		lc, err := evedb.GetLoadedCharge(ctx, m.db, shipID, mod.Flag)
		if err != nil {
			return reloaded, fmt.Errorf("reloadFromCargo: %w", err)
		}
		if lc != nil {
			if lc.TypeID != mod.ChargeTypeID {
				continue // operator loaded something else; leave it alone
			}
			loaded = lc.Quantity
		}
		// Top up to a full load, not to ChargeQty: a durable line's spare crystal and a generated fit's
		// 200 rounds are what the ship carries, and whatever the gun cannot hold stays in the hold.
		room, err := m.slotChargeRoom(ctx, shipID, mod.Flag, mod.ChargeTypeID, loaded)
		if err != nil {
			return reloaded, fmt.Errorf("reloadFromCargo: %w", err)
		}
		short := mod.ChargeQty - loaded
		if short > room {
			short = room
		}
		if short <= 0 {
			continue
		}
		// Pull from the loose stacks of that charge in the hold.
		moved, err := m.takeFromCargo(ctx, shipID, mod.ChargeTypeID, short)
		if err != nil {
			return reloaded, err
		}
		if moved == 0 {
			continue
		}
		if lc != nil {
			if err := evedb.LoadChargeIntoModule(ctx, m.db, lc.ItemID, shipID, mod.Flag, loaded+moved); err != nil {
				return reloaded, err
			}
		} else {
			itemID, err := evedb.CreateItem(ctx, m.db, mod.ChargeTypeID, b.CharID, shipID, int(moved))
			if err != nil {
				return reloaded, fmt.Errorf("reloadFromCargo create: %w", err)
			}
			if err := evedb.LoadChargeIntoModule(ctx, m.db, itemID, shipID, mod.Flag, moved); err != nil {
				return reloaded, err
			}
		}
		reloaded++
		m.logBot(b.CharID, logInfo, "fit", "reloaded %d× %s into slot %d from the hold",
			moved, mod.ChargeName, mod.Flag)
	}
	return reloaded, nil
}

// takeFromCargo removes up to want units of typeID from the ship's hold
// (flag 5) and reports how many it actually got.
func (m *Mod) takeFromCargo(ctx context.Context, shipID, typeID, want int64) (int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE locationID = ? AND flag = 5 AND typeID = ?
		ORDER BY quantity ASC`, shipID, typeID)
	if err != nil {
		return 0, fmt.Errorf("takeFromCargo: %w", err)
	}
	type stack struct{ itemID, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.itemID, &s.qty); err != nil {
			rows.Close()
			return 0, fmt.Errorf("takeFromCargo scan: %w", err)
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var taken int64
	for _, s := range stacks {
		if taken >= want {
			break
		}
		take := want - taken
		if take >= s.qty {
			if _, err := m.db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.itemID); err != nil {
				return taken, err
			}
			taken += s.qty
			continue
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, take, s.itemID); err != nil {
			return taken, err
		}
		taken += take
	}
	return taken, nil
}

// shipRestock reports the ammo the ship should go and buy: every charge type whose whole
// complement (loaded plus hold) is under ammoRestockFraction of what the fit calls for, in the
// quantity that fills it back up. Call it after reloadFromCargo. The deficit is bought as cargo,
// and the reload pass moves it into the guns.
func (m *Mod) shipRestock(ctx context.Context, shipID int64, fit *Fit) ([]ShopItem, error) {
	target := ammoTarget(fit)
	if len(target) == 0 {
		return nil, nil
	}
	have, err := m.ammoAboard(ctx, shipID, fit)
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	for _, mod := range fit.Modules {
		if mod.ChargeTypeID != 0 {
			names[mod.ChargeTypeID] = mod.ChargeName
		}
	}
	var out []ShopItem
	for typeID, want := range target {
		if want <= 0 {
			continue
		}
		got := have[typeID]
		if float64(got) >= float64(want)*ammoRestockFraction {
			continue
		}
		out = append(out, ShopItem{
			TypeID: typeID, Name: names[typeID], Qty: want - got, Dest: "cargo"})
	}
	// Deterministic order: the same hold produces the same trip twice.
	sort.Slice(out, func(i, j int) bool { return out[i].TypeID < out[j].TypeID })
	return out, nil
}

// restockCooldownSec is how long a bot waits after an ammo run that found
// no seller before trying again.
const restockCooldownSec = 1800

// ammoCheck is the whole ammo pass in the order that costs least: reload out of the hold (free),
// then report what still has to be bought. An empty result means the ship is ready. Every step
// reads the plan restated against the ship (liveFit), so a stand-in gun is stocked for itself
// (fitslotcharge.go).
func (m *Mod) ammoCheck(ctx context.Context, b *botRun, a *fitShip, t *fitTemplate) ([]ShopItem, error) {
	// A round loaded into a module that cannot fire it comes out first, so
	// the hold sweep below judges it like any other loose stack.
	if _, err := m.unloadRejectedCharges(ctx, b, a.ShipItemID); err != nil {
		return nil, err
	}
	live, err := m.liveFit(ctx, b.CharID, a.ShipItemID, t.Fit)
	if err != nil {
		return nil, err
	}
	lt := *t
	lt.Fit = live
	// Rounds nothing aboard can fire come off next (fitstray.go): they take hold space the real
	// magazine needs, and the clamp below sizes the restock to what is free.
	if _, err := m.stowStrayAmmo(ctx, b, a.ShipItemID, &lt); err != nil {
		// Housekeeping, not a gate: a failed sweep must not stop the reload.
		m.logBot(b.CharID, logError, "fit", "stowing stray ammo: %v", err)
	}
	if len(ammoTypes(live)) == 0 {
		return nil, nil // nothing on this fit burns charges
	}
	if _, err := m.reloadLive(ctx, b, a.ShipItemID, live); err != nil {
		return nil, err
	}
	if b.State.Data.RestockAfter > clock.Unix() {
		return nil, nil // a recent run came back empty; do not re-fly it
	}
	list, err := m.shipRestock(ctx, a.ShipItemID, live)
	if err != nil {
		return nil, err
	}
	return m.clampRestockToHold(ctx, b, a.ShipItemID, list), nil
}

// clampRestockToHold trims a restock list to the ship's free hold. Reload depths are sized to the
// job (ammoReloadsFor), not the hull, so a small combat hull can be asked for more rounds than it
// carries: fill the hold and go, never fail the trip.
func (m *Mod) clampRestockToHold(ctx context.Context, b *botRun, shipID int64, list []ShopItem) []ShopItem {
	if len(list) == 0 {
		return list
	}
	free, err := m.holdFreeM3(ctx, b.CharID, shipID)
	if err != nil {
		return list // an unreadable hold is the till's problem, not a veto
	}
	var out []ShopItem
	for _, it := range list {
		vol, verr := evedb.GetTypeVolume(ctx, m.db, it.TypeID)
		if verr != nil || vol <= 0 {
			out = append(out, it)
			continue
		}
		maxQty := int64(free / vol)
		if maxQty <= 0 {
			continue
		}
		if it.Qty > maxQty {
			it.Qty = maxQty
		}
		free -= float64(it.Qty) * vol
		out = append(out, it)
	}
	return out
}

// shopIsConsumablesOnly reports whether a shopping list is nothing but ammo and cargo. Such a trip
// is an errand, not a build: failing to find a seller leaves the bot carrying on with what it has
// (procure.go treats it as a soft trip).
func shopIsConsumablesOnly(list []ShopItem) bool {
	if len(list) == 0 {
		return false
	}
	for _, it := range list {
		if it.Dest != "charge" && it.Dest != "cargo" {
			return false
		}
	}
	return true
}

// shopHardLines is a shopping list without its ammo and cargo: the modules and drones whose
// absence can keep a pilot out of a seat. A reason given to a reader names only these.
func shopHardLines(list []ShopItem) []ShopItem {
	var out []ShopItem
	for _, it := range list {
		if it.Dest != "charge" && it.Dest != "cargo" {
			out = append(out, it)
		}
	}
	return out
}

func shopSummary(list []ShopItem) string {
	s := ""
	for i, it := range list {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%d× %s", it.Qty, it.Name)
	}
	return s
}
