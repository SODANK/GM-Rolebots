package rolebots

import (
	"context"
	"fmt"
)

// Fit stickiness. A generated fit is rebuilt from the live catalogue on every plan pass,
// and shipMissing (fitcheck.go) diffs the ship against it by exact (typeID, flag). An
// already-fitted module therefore keeps its slot unless a challenger is clearly better,
// so a seller running dry elsewhere does not send a ready bot shopping. The loadout is
// read from the ship itself and is not cached.

// fitGenStickiness is the share of the best score an already-fitted module must reach
// to keep its slot. It is upgradeScoreMargin (1.15) read from the other side.
const fitGenStickiness = 0.85

// ownedLoadout is what is fitted to a ship now: modules per slot band, and the round
// loaded beside each gun. Modules are counted per band because the generator fills a
// band in order and does not care which slot of it holds a given module.
type ownedLoadout struct {
	byBand    map[int64]map[int64]int // band flag -> typeID -> how many are fitted
	chargeFor map[int64]int64         // gun typeID -> the round beside it
	// The drone bay (flag 87) is not a slot band, so it cannot live in byBand.
	drones map[int64]int64 // drone typeID -> quantity in the bay
}

// has reports whether an unclaimed module of this type is still fitted in this band.
func (o *ownedLoadout) has(band, typeID int64) bool {
	if o == nil {
		return false
	}
	return o.byBand[band][typeID] > 0
}

// hasDrone reports whether this type is already in the bay. Nothing is claimed: the
// bay is filled in one decision, so an incumbent is consulted once.
func (o *ownedLoadout) hasDrone(typeID int64) bool {
	return o != nil && o.drones[typeID] > 0
}

// claim marks one fitted module as accounted for, so one fitted repairer cannot make
// two generated repairers both look like incumbents.
func (o *ownedLoadout) claim(band, typeID int64) {
	if o == nil {
		return
	}
	if n := o.byBand[band][typeID]; n > 0 {
		o.byBand[band][typeID] = n - 1
	}
}

// charge is the round loaded in a gun of this type, 0 for none.
func (o *ownedLoadout) charge(gunTypeID int64) int64 {
	if o == nil {
		return 0
	}
	return o.chargeFor[gunTypeID]
}

// bandOf is the slot band a fitting flag belongs to. Flags 11-18 are low, 19-26 mid,
// 27-34 high (docs/bindings/fitting.md).
func bandOf(flag int64) int64 {
	switch {
	case flag >= flagHiFirst:
		return flagHiFirst
	case flag >= flagMedFirst:
		return flagMedFirst
	default:
		return flagLoFirst
	}
}

// ownedFitLoadout reads what is fitted to the ship this pilot flies on this hull, or
// nil when they own none. A charge sits at the same flag as its gun, so one query
// returns both: category 7 is the module and category 8 its ammunition.
// the module and category 8 at that flag is its ammunition.
func (m *Mod) ownedFitLoadout(ctx context.Context, charID, hullTypeID int64) (*ownedLoadout, error) {
	ships, err := shipsForChar(ctx, m.db, charID)
	if err != nil {
		return nil, err
	}
	shipID := ships[generatedFitID(hullTypeID)]
	if shipID == 0 {
		return nil, nil
	}
	// The slot bands plus the drone bay (flag 87). The flags between are cargo, which is
	// not a loadout, so the query ORs the bay in instead of widening the BETWEEN.
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, e.flag, g.categoryID, e.quantity
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ?
		  AND ((e.flag BETWEEN ? AND ? AND g.categoryID IN (7, 8))
		    OR (e.flag = ? AND g.categoryID = ?))`,
		shipID, flagLoFirst, flagHiFirst+7, flagDroneBay, catDroneID)
	if err != nil {
		return nil, fmt.Errorf("ownedFitLoadout: %w", err)
	}
	defer rows.Close()

	type slotItem struct{ typeID, flag, cat, qty int64 }
	var items []slotItem
	for rows.Next() {
		var it slotItem
		if err := rows.Scan(&it.typeID, &it.flag, &it.cat, &it.qty); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	out := &ownedLoadout{
		byBand:    map[int64]map[int64]int{},
		chargeFor: map[int64]int64{},
		drones:    map[int64]int64{},
	}
	gunAt := map[int64]int64{}    // flag -> gun typeID
	chargeAt := map[int64]int64{} // flag -> charge typeID
	for _, it := range items {
		if it.flag == flagDroneBay {
			out.drones[it.typeID] += it.qty
			continue
		}
		if it.cat == 8 {
			chargeAt[it.flag] = it.typeID
			continue
		}
		band := bandOf(it.flag)
		if out.byBand[band] == nil {
			out.byBand[band] = map[int64]int{}
		}
		out.byBand[band][it.typeID]++
		gunAt[it.flag] = it.typeID
	}
	for flag, gun := range gunAt {
		if c := chargeAt[flag]; c != 0 {
			out.chargeFor[gun] = c
		}
	}
	return out, nil
}
