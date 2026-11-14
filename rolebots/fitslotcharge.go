package rolebots

import (
	"context"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// A charge is planned, bought, loaded or counted for a slot only when the module actually in
// that slot accepts it (evedb.ValidateChargeForModule: charge group and size). The generator's
// plan and the ship differ, for example a stand-in gun in a planned slot (rearmToothless,
// plannedGunSlot) or alignFitToShip giving a line to another module. So every ammunition
// decision reads liveFit, the plan restated against the ship, never the plan's own flags.

// slotLive is what one slot of a real ship holds.
type slotLive struct {
	Module     int64 // module typeID, 0 = empty slot
	ModuleName string
	Charge     int64 // loaded charge typeID, 0 = none
}

// slotCharge is a round picked for a module: type, name and how many make one
// fit line (chargeQtyFor). Type 0 means the module takes nothing.
type slotCharge struct {
	TypeID int64
	Name   string
	Qty    int64
}

// reconcileSlotCharges returns a copy of fit restated against the ship's slots. A line whose
// slot holds a module becomes that module and keeps its charge if accepted; otherwise pick
// chooses a round for the fitted module (none for a miner or remote repairer). A line with an
// empty slot keeps its charge, and reload stacks follow the magazine. Pure.
func reconcileSlotCharges(fit *Fit, slots map[int64]slotLive,
	accepts func(chargeTypeID, moduleTypeID int64) bool,
	pick func(moduleTypeID, incumbent int64) slotCharge) *Fit {
	if fit == nil {
		return nil
	}
	out := *fit
	out.Modules = append([]FitModule(nil), fit.Modules...)
	for i := range out.Modules {
		line := &out.Modules[i]
		s, ok := slots[line.Flag]
		if !ok || s.Module == 0 {
			continue
		}
		changed := s.Module != line.TypeID
		if changed {
			line.TypeID = s.Module
			if s.ModuleName != "" {
				line.TypeName = s.ModuleName
			}
		}
		if line.ChargeTypeID != 0 && accepts(line.ChargeTypeID, s.Module) {
			continue
		}
		if line.ChargeTypeID == 0 && !changed {
			continue // planned with nothing in it, and that is what is fitted
		}
		incumbent := int64(0)
		if s.Charge != 0 && accepts(s.Charge, s.Module) {
			incumbent = s.Charge
		}
		c := pick(s.Module, incumbent)
		if c.TypeID != 0 && !accepts(c.TypeID, s.Module) {
			c = slotCharge{}
		}
		line.ChargeTypeID, line.ChargeName, line.ChargeQty = c.TypeID, c.Name, c.Qty
		if c.TypeID == 0 {
			line.ChargeName, line.ChargeQty = "", 0
		}
	}
	out.Cargo = reconcileReloads(fit, &out)
	return &out
}

// reconcileReloads rebuilds the reload stacks for a reconciled magazine. Cargo that is not
// ammunition is kept, a reload line stays only while some line still loads its round, and each
// round the plan did not load gets a reload line at the plan's own depth (reload per loaded round).
func reconcileReloads(plan, live *Fit) []FitCargo {
	oldAmmo, newAmmo := ammoTypes(plan), ammoTypes(live)
	var loadedOld, reloadOld int64
	for _, mod := range plan.Modules {
		if mod.ChargeTypeID != 0 && mod.ChargeQty > 0 {
			loadedOld += mod.ChargeQty
		}
	}
	for _, c := range plan.Cargo {
		if oldAmmo[c.TypeID] {
			reloadOld += c.Qty
		}
	}
	depth := 0.0
	if loadedOld > 0 {
		depth = float64(reloadOld) / float64(loadedOld)
	}

	out := make([]FitCargo, 0, len(plan.Cargo))
	for _, c := range plan.Cargo {
		if !oldAmmo[c.TypeID] || newAmmo[c.TypeID] {
			out = append(out, c)
		}
	}
	added := map[int64]int{}
	for _, mod := range live.Modules {
		if mod.ChargeTypeID == 0 || mod.ChargeQty <= 0 || oldAmmo[mod.ChargeTypeID] {
			continue
		}
		qty := int64(math.Round(float64(mod.ChargeQty) * depth))
		if qty <= 0 {
			continue
		}
		if i, ok := added[mod.ChargeTypeID]; ok {
			out[i].Qty += qty
			continue
		}
		added[mod.ChargeTypeID] = len(out)
		out = append(out, FitCargo{TypeID: mod.ChargeTypeID, TypeName: mod.ChargeName, Qty: qty})
	}
	return out
}

// chargeAccepts is the one compatibility test every ammunition path here asks. The core caches
// its lookups, so asking per slot and per shot is cheap.
func (m *Mod) chargeAccepts(ctx context.Context, chargeTypeID, moduleTypeID int64) bool {
	if chargeTypeID == 0 || moduleTypeID == 0 {
		return false
	}
	return evedb.ValidateChargeForModule(ctx, m.db, chargeTypeID, moduleTypeID) == nil
}

// shipSlotsLive reads what each slot of a ship holds: its module and the
// charge loaded into it.
func (m *Mod) shipSlotsLive(ctx context.Context, shipID int64) (map[int64]slotLive, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.flag, e.typeID, t.typeName, g.categoryID
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID IN (7, 8)
		WHERE e.locationID = ? AND e.flag BETWEEN ? AND ?`, shipID, flagLoFirst, flagHiFirst+7)
	if err != nil {
		return nil, fmt.Errorf("shipSlotsLive: %w", err)
	}
	defer rows.Close()
	out := map[int64]slotLive{}
	for rows.Next() {
		var flag, typeID, cat int64
		var name string
		if err := rows.Scan(&flag, &typeID, &name, &cat); err != nil {
			return nil, fmt.Errorf("shipSlotsLive scan: %w", err)
		}
		s := out[flag]
		if cat == 7 {
			s.Module, s.ModuleName = typeID, name
		} else {
			s.Charge = typeID
		}
		out[flag] = s
	}
	return out, rows.Err()
}

// liveFit is fit restated against its ship (reconcileSlotCharges): each slot as it is, loaded
// with a round its module can fire. A stand-in gets the best round the pilot can use and buy
// (chargeFor), with the loaded round as incumbent so a working magazine is kept.
func (m *Mod) liveFit(ctx context.Context, charID, shipID int64, fit *Fit) (*Fit, error) {
	if fit == nil || shipID == 0 || len(fit.Modules) == 0 {
		return fit, nil
	}
	slots, err := m.shipSlotsLive(ctx, shipID)
	if err != nil {
		return nil, err
	}
	accepts := func(c, mod int64) bool { return m.chargeAccepts(ctx, c, mod) }

	var skills map[int64]int
	var reach map[int64]bool
	loaded := false
	pick := func(module, incumbent int64) slotCharge {
		if !loaded {
			loaded = true
			skills, _ = m.charSkillsFor(ctx, charID)
			reach = m.chargeShopScope(ctx, charID)
		}
		mod := FitModule{TypeID: module}
		m.chargeFor(ctx, &mod, skills, incumbent, reach)
		return slotCharge{TypeID: mod.ChargeTypeID, Name: mod.ChargeName, Qty: mod.ChargeQty}
	}
	return reconcileSlotCharges(fit, slots, accepts, pick), nil
}

// unloadRejectedCharges takes every charge out of a slot whose module cannot
// fire it and puts it in the ship's hold, and reports how many it moved. The
// hold sweep (stowStrayAmmo) and the restock then judge those rounds like any
// other loose stack.
func (m *Mod) unloadRejectedCharges(ctx context.Context, b *botRun, shipID int64) (int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.itemID, c.typeID, mo.typeID
		FROM entity c
		JOIN invTypes ct ON ct.typeID = c.typeID
		JOIN invGroups cg ON cg.groupID = ct.groupID AND cg.categoryID = 8
		JOIN entity mo ON mo.locationID = c.locationID AND mo.flag = c.flag AND mo.itemID <> c.itemID
		JOIN invTypes mt ON mt.typeID = mo.typeID
		JOIN invGroups mg ON mg.groupID = mt.groupID AND mg.categoryID = 7
		WHERE c.locationID = ? AND c.flag BETWEEN ? AND ?`, shipID, flagLoFirst, flagHiFirst+7)
	if err != nil {
		return 0, fmt.Errorf("unloadRejectedCharges: %w", err)
	}
	type loaded struct{ itemID, charge, module int64 }
	var all []loaded
	for rows.Next() {
		var l loaded
		if err := rows.Scan(&l.itemID, &l.charge, &l.module); err != nil {
			rows.Close()
			return 0, fmt.Errorf("unloadRejectedCharges scan: %w", err)
		}
		all = append(all, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	moved := 0
	for _, l := range all {
		if m.chargeAccepts(ctx, l.charge, l.module) {
			continue
		}
		if err := evedb.UnloadChargeToCargoOrHangar(ctx, m.db, l.itemID, shipID, 5); err != nil {
			return moved, err
		}
		moved++
	}
	if moved > 0 && b != nil {
		m.logBot(b.CharID, logInfo, "fit",
			"unloaded %d charge(s) its modules cannot fire into the hold", moved)
	}
	return moved, nil
}
