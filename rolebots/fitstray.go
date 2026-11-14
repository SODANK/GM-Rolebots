package rolebots

// Stray hold ammunition. Reload stacks are bought for the fit a ship was built under and
// stay in the hold when the guns change. stowStrayAmmo moves stacks nothing aboard can
// fire, and usable rounds past what the magazine needs, to the station hangar, where
// sellHangarStrays (strays.go) disposes of them.
//
// Not strays: rounds that any fitted module or module of the target fit accepts, the
// fit's own cargo lines, loot (rolebot_loot_hold), and everything on a trade hull or a
// bare-hull reading with no modules to measure against.

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// strayAmmoStack is one charge stack sitting in a ship's hold.
type strayAmmoStack struct {
	ItemID, TypeID, Qty int64
}

// strayAmmo filters hold stacks down to those no module in modules can load. Pure:
// accepts is the compatibility rule (evedb.ValidateChargeForModule in production) and
// keep holds type IDs that are never strays.
func strayAmmo(stacks []strayAmmoStack, modules []int64, keep map[int64]bool,
	accepts func(chargeTypeID, moduleTypeID int64) bool) []strayAmmoStack {
	var out []strayAmmoStack
	for _, s := range stacks {
		if keep[s.TypeID] {
			continue
		}
		usable := false
		for _, mod := range modules {
			if accepts(s.TypeID, mod) {
				usable = true
				break
			}
		}
		if !usable {
			out = append(out, s)
		}
	}
	return out
}

// ammoHoldAllowance is how many times a fit's own charge lines a hold may carry of one
// usable round before the rest is surplus: the line, plus one reload.
const ammoHoldAllowance = 2

// ammoSurplus is the part of one hold stack to take out: all of it (Whole) or the top
// Qty units.
type ammoSurplus struct {
	ItemID, TypeID, Qty int64
	Whole               bool
}

// surplusAmmo returns the part of the usable stacks past allowance (typeID to units the
// hold may keep). A type with no allowance entry is left alone: its gun is not on the
// fit, so its magazine size is unknown. Pure; stacks are taken in order.
func surplusAmmo(stacks []strayAmmoStack, allowance map[int64]int64) []ammoSurplus {
	left := make(map[int64]int64, len(allowance))
	for k, v := range allowance {
		left[k] = v
	}
	var out []ammoSurplus
	for _, s := range stacks {
		a, ok := left[s.TypeID]
		if !ok || s.Qty <= 0 {
			continue
		}
		if a >= s.Qty {
			left[s.TypeID] = a - s.Qty
			continue
		}
		if a < 0 {
			a = 0
		}
		left[s.TypeID] = 0
		out = append(out, ammoSurplus{ItemID: s.ItemID, TypeID: s.TypeID, Qty: s.Qty - a, Whole: a == 0})
	}
	return out
}

// ammoAllowance sizes surplusAmmo's allowance from the fit: for every usable type,
// ammoHoldAllowance times the charge quantity of each fit module that can fire it,
// plus the fit's own cargo line for it.
func ammoAllowance(types []int64, fit *Fit, accepts func(chargeTypeID, moduleTypeID int64) bool) map[int64]int64 {
	out := map[int64]int64{}
	for _, t := range types {
		var n int64
		for _, mod := range fit.Modules {
			if mod.ChargeQty > 0 && accepts(t, mod.TypeID) {
				n += mod.ChargeQty
			}
		}
		for _, c := range fit.Cargo {
			if c.TypeID == t {
				n += c.Qty
			}
		}
		if n > 0 {
			out[t] = n * ammoHoldAllowance
		}
	}
	return out
}

// stowStrayAmmo moves every unusable charge stack out of the ship's hold into the
// hangar of the station the ship is docked in, and returns how many stacks it moved.
// A ship that is not docked at the pilot's station is left alone.
func (m *Mod) stowStrayAmmo(ctx context.Context, b *botRun, shipID int64, t *fitTemplate) (int, error) {
	if shipID == 0 || b.StationID == 0 || t == nil || t.Fit == nil ||
		t.Job == JobTrade || t.Job == JobFreight || t.HullOnly || len(t.Fit.Modules) == 0 {
		return 0, nil
	}
	var loc int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`, shipID).Scan(&loc); err != nil || loc != b.StationID {
		return 0, nil
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.quantity
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 8
		WHERE e.locationID = ? AND e.flag = 5 AND e.ownerID = ?
		  AND NOT EXISTS (SELECT 1 FROM rolebot_loot_hold lh
			WHERE lh.characterID = e.ownerID AND lh.typeID = e.typeID AND lh.units > 0)`,
		shipID, b.CharID)
	if err != nil {
		return 0, fmt.Errorf("stowStrayAmmo: %w", err)
	}
	var stacks []strayAmmoStack
	for rows.Next() {
		var s strayAmmoStack
		if err := rows.Scan(&s.ItemID, &s.TypeID, &s.Qty); err != nil {
			rows.Close()
			return 0, fmt.Errorf("stowStrayAmmo scan: %w", err)
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(stacks) == 0 {
		return 0, nil
	}

	// Modules that could load a charge: the fit's, plus those fitted now (the ship may
	// not be refitted yet).
	seen := map[int64]bool{}
	var modules []int64
	for _, mod := range t.Fit.Modules {
		if !seen[mod.TypeID] {
			seen[mod.TypeID] = true
			modules = append(modules, mod.TypeID)
		}
	}
	mrows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.typeID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 7
		WHERE e.locationID = ? AND e.flag BETWEEN ? AND 34`, shipID, flagLoFirst)
	if err != nil {
		return 0, fmt.Errorf("stowStrayAmmo modules: %w", err)
	}
	for mrows.Next() {
		var id int64
		if err := mrows.Scan(&id); err != nil {
			mrows.Close()
			return 0, fmt.Errorf("stowStrayAmmo modules scan: %w", err)
		}
		if !seen[id] {
			seen[id] = true
			modules = append(modules, id)
		}
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return 0, err
	}

	keep := map[int64]bool{}
	for _, c := range t.Fit.Cargo {
		keep[c.TypeID] = true
	}
	accepts := func(chargeTypeID, moduleTypeID int64) bool {
		return evedb.ValidateChargeForModule(ctx, m.db, chargeTypeID, moduleTypeID) == nil
	}

	moved := 0
	stray := map[int64]bool{}
	for _, s := range strayAmmo(stacks, modules, keep, accepts) {
		if err := evedb.UnloadChargeToCargoOrHangar(ctx, m.db, s.ItemID, b.StationID, 4); err != nil {
			return moved, err
		}
		stray[s.ItemID] = true
		moved++
		m.logBot(b.CharID, logInfo, "fit",
			"hold carried %d× %s that nothing aboard can fire — stowed in the hangar",
			s.Qty, m.typeNameOr(ctx, s.TypeID))
	}

	// Then the rounds it can fire, past what the magazine needs. They go to the hangar,
	// not the market, as the next restock's stock.
	var usable []strayAmmoStack
	var types []int64
	seenType := map[int64]bool{}
	for _, s := range stacks {
		if stray[s.ItemID] {
			continue
		}
		usable = append(usable, s)
		if !seenType[s.TypeID] {
			seenType[s.TypeID] = true
			types = append(types, s.TypeID)
		}
	}
	stowed := map[int64]int64{}
	for _, x := range surplusAmmo(usable, ammoAllowance(types, t.Fit, accepts)) {
		id := x.ItemID
		if !x.Whole {
			nid, err := evedb.SplitStackN(ctx, m.db, x.ItemID, x.Qty)
			if err != nil {
				return moved, err
			}
			id = nid
		}
		if err := evedb.UnloadChargeToCargoOrHangar(ctx, m.db, id, b.StationID, 4); err != nil {
			return moved, err
		}
		moved++
		stowed[x.TypeID] += x.Qty
	}
	for typeID, n := range stowed {
		m.logBot(b.CharID, logInfo, "fit",
			"hold carried %d× type %d more than the fit can use — stowed in the hangar", n, typeID)
	}
	return moved, nil
}
