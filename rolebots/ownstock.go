package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strings"

	evedb "gurimoon/db"
)

// Own stock is a seller.
//
// The pilot's own hangar is read in three places, always as the cheapest seller
// there is: the generator scores a module it already owns as sunk cost, the
// till takes from the hangar before it buys, and the next stop on a shopping
// trip counts a station holding the pilot's own stock as covering those lines at
// no price. Without it a ship flew with a mid slot empty while its pilot held a
// mid-slot module in a hangar, and bought the module again at the till.

// ownStockCategories are the kinds of thing a shopping list names: modules,
// charges and drones. Ships and blueprints are identities, never stock.
const ownStockCategories = "7, 8, 18"

// ownStockNominalPrice is what a line costs when the pilot's own hangar covers
// it: above zero only because the stop ranker skips unpriced rows.
const ownStockNominalPrice = 0.01

// ownStockStack is the largest stack of one type the pilot keeps in its own
// hangar at a station, and how many units it holds; 0, 0 when there is none.
func (m *Mod) ownStockStack(ctx context.Context, charID, stationID, typeID int64) (int64, int64) {
	var id, qty int64
	err := m.db.QueryRowContext(ctx, `
		SELECT e.itemID, e.quantity
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.typeID = ? AND e.flag = ?
		  AND g.categoryID IN (`+ownStockCategories+`)
		  AND e.quantity > 0
		ORDER BY e.quantity DESC, e.itemID ASC
		LIMIT 1`, charID, stationID, typeID, flagHangar).Scan(&id, &qty)
	if err != nil {
		return 0, 0
	}
	return id, qty
}

// ownStockByStation is what the pilot holds of these types in its hangars:
// stationID, then typeID, then units.
func (m *Mod) ownStockByStation(ctx context.Context, charID int64, typeIDs []int64) (map[int64]map[int64]int64, error) {
	out := map[int64]map[int64]int64{}
	if len(typeIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(typeIDs)), ",")
	args := make([]any, 0, len(typeIDs)+2)
	args = append(args, charID, flagHangar)
	for _, id := range typeIDs {
		args = append(args, id)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.locationID, e.typeID, SUM(e.quantity)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.flag = ? AND e.typeID IN (`+ph+`)
		  AND g.categoryID IN (`+ownStockCategories+`)
		  AND e.locationID BETWEEN 60000000 AND 63999999
		GROUP BY e.locationID, e.typeID`, args...)
	if err != nil {
		return nil, fmt.Errorf("ownStockByStation: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st, typeID, qty int64
		if err := rows.Scan(&st, &typeID, &qty); err != nil {
			return nil, err
		}
		if out[st] == nil {
			out[st] = map[int64]int64{}
		}
		out[st][typeID] += qty
	}
	return out, rows.Err()
}

// ownStockTotals folds ownStockByStation down to units per type, wherever
// they are.
func ownStockTotals(by map[int64]map[int64]int64) map[int64]int64 {
	out := map[int64]int64{}
	for _, types := range by {
		for typeID, q := range types {
			out[typeID] += q
		}
	}
	return out
}

// shoppingTypeIDs is the distinct types a list names.
func shoppingTypeIDs(list []ShopItem) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, it := range list {
		if it.Dest == "hull" || seen[it.TypeID] {
			continue
		}
		seen[it.TypeID] = true
		out = append(out, it.TypeID)
	}
	return out
}

// ownStockStops turns the pilot's own stock into stops the next-stop ranker
// can weigh beside the market's: a station where it keeps a line reads as a
// seller of that line at a nominal price. Stations it has no route to are
// left out.
func (m *Mod) ownStockStops(ctx context.Context, b *botRun, list []ShopItem) []marketStop {
	by, err := m.ownStockByStation(ctx, b.CharID, shoppingTypeIDs(list))
	if err != nil || len(by) == 0 {
		return nil
	}
	stations := make([]int64, 0, len(by))
	for st := range by {
		stations = append(stations, st)
	}
	sort.Slice(stations, func(i, j int) bool { return stations[i] < stations[j] })
	var out []marketStop
	for _, st := range stations {
		sys := m.stationSystemID(ctx, st)
		if sys == 0 {
			continue
		}
		j := m.jumpsTo(ctx, b, sys)
		if j < 0 {
			continue
		}
		for typeID, q := range by[st] {
			out = append(out, marketStop{
				TypeID: typeID, StationID: st, SystemID: sys,
				Price: ownStockNominalPrice, Volume: q, Jumps: j, Preferred: true,
			})
		}
	}
	return out
}

// takeUnits hands over exactly n units of a stack, splitting when it is bigger,
// and says how many it handed over. A market buy merges into the hangar's stack
// (deliverItem), so fitting that itemID directly would write quantity = 1 or the
// round count over every unit already kept, and a short fill would mint rounds.
func (m *Mod) takeUnits(ctx context.Context, itemID, n int64) (int64, int64, error) {
	if n <= 0 {
		return itemID, 0, nil
	}
	var qty int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT quantity FROM entity WHERE itemID = ?`, itemID).Scan(&qty); err != nil {
		return 0, 0, fmt.Errorf("takeUnits: %w", err)
	}
	if qty <= n {
		return itemID, qty, nil
	}
	id, err := evedb.SplitStackN(ctx, m.db, itemID, n)
	if err != nil {
		return 0, 0, err
	}
	return id, n, nil
}

// withOwnStock is the generator's catalogue with the pilot's own stock in it,
// priced as sunk: a type it owns is carried with no ask, which priceFactor reads
// as free, as for an incumbent. Types the scoped catalogue already carries are
// replaced by that copy, not listed twice. Pure.
func withOwnStock(scoped []*marketModule, every map[int64]*marketModule, owned map[int64]int64) []*marketModule {
	if len(owned) == 0 {
		return scoped
	}
	out := make([]*marketModule, 0, len(scoped)+len(owned))
	have := map[int64]bool{}
	for _, mm := range scoped {
		if owned[mm.TypeID] > 0 {
			c := *mm
			c.Ask = 0
			out = append(out, &c)
		} else {
			out = append(out, mm)
		}
		have[mm.TypeID] = true
	}
	var extra []int64
	for typeID, q := range owned {
		if q > 0 && !have[typeID] && every[typeID] != nil {
			extra = append(extra, typeID)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	for _, typeID := range extra {
		c := *every[typeID]
		c.Ask = 0
		// Nobody in reach sells it, so the stock is all there is
		// (marketModule.MaxCopies).
		c.MaxCopies = owned[typeID]
		out = append(out, &c)
	}
	return out
}

// ownModulesInReach is the modules the pilot keeps in hangars inside the
// regions it shops in (reach; nil = anywhere), as typeID to units.
func (m *Mod) ownModulesInReach(ctx context.Context, charID int64, reach map[int64]bool) map[int64]int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.regionID, e.typeID, SUM(e.quantity)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 7
		JOIN staStations s ON s.stationID = e.locationID
		WHERE e.ownerID = ? AND e.flag = ?
		GROUP BY s.regionID, e.typeID`, charID, flagHangar)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var region, typeID, qty int64
		if rows.Scan(&region, &typeID, &qty) != nil {
			continue
		}
		if len(reach) > 0 && !reach[region] {
			continue
		}
		out[typeID] += qty
	}
	return out
}

// ── slot alignment ──────────────────────────────────────────────────────────

// shipSlotModules is what sits in each slot of a ship: flag to module typeID.
// Charges share their gun's flag and are left out.
func (m *Mod) shipSlotModules(ctx context.Context, shipID int64) (map[int64]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.flag, e.typeID
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 7
		WHERE e.locationID = ? AND e.flag BETWEEN ? AND ?`, shipID, flagLoFirst, flagHiFirst+7)
	if err != nil {
		return nil, fmt.Errorf("shipSlotModules: %w", err)
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var flag, typeID int64
		if err := rows.Scan(&flag, &typeID); err != nil {
			return nil, err
		}
		out[flag] = typeID
	}
	return out, rows.Err()
}

// alignedToShip is the template with its module flags moved onto the ship's
// own layout (alignFitToShip). A copy, since the template may be shared.
func (m *Mod) alignedToShip(ctx context.Context, t *fitTemplate, shipID int64) *fitTemplate {
	if t == nil || t.Fit == nil || shipID == 0 {
		return t
	}
	slots, err := m.shipSlotModules(ctx, shipID)
	if err != nil || len(slots) == 0 {
		return t
	}
	out := *t
	out.Fit = alignFitToShip(t.Fit, slots)
	return &out
}

// alignFitToShip renumbers a fit's slot flags, band by band, so every module the
// ship already carries is planned in the slot it sits in. shipMissing compares
// exact (type, flag), so a module in another slot would be unfitted and bought
// again. New modules take the band's remaining flags, empty first. Pure; a copy.
func alignFitToShip(fit *Fit, ship map[int64]int64) *Fit {
	out := *fit
	out.Modules = append([]FitModule(nil), fit.Modules...)
	if len(ship) == 0 {
		return &out
	}
	for _, band := range []int64{flagHiFirst, flagMedFirst, flagLoFirst} {
		var idx []int
		flagSet := map[int64]bool{}
		for i, mod := range out.Modules {
			if bandOf(mod.Flag) == band {
				idx = append(idx, i)
				flagSet[mod.Flag] = true
			}
		}
		if len(idx) == 0 {
			continue
		}
		var shipFlags []int64
		for flag := range ship {
			if bandOf(flag) == band {
				shipFlags = append(shipFlags, flag)
				flagSet[flag] = true
			}
		}
		sort.Slice(shipFlags, func(i, j int) bool { return shipFlags[i] < shipFlags[j] })

		used := map[int64]bool{}
		assigned := map[int]int64{}
		// Already right where it is.
		for _, i := range idx {
			f := out.Modules[i].Flag
			if ship[f] == out.Modules[i].TypeID && !used[f] {
				assigned[i], used[f] = f, true
			}
		}
		// The same module, in another slot of the band.
		for _, i := range idx {
			if _, ok := assigned[i]; ok {
				continue
			}
			for _, f := range shipFlags {
				if !used[f] && ship[f] == out.Modules[i].TypeID {
					assigned[i], used[f] = f, true
					break
				}
			}
		}
		// Everything else: empty slots first, then slots holding what the
		// plan does not keep.
		var free []int64
		for f := range flagSet {
			if !used[f] {
				free = append(free, f)
			}
		}
		sort.Slice(free, func(i, j int) bool {
			ei, ej := ship[free[i]] == 0, ship[free[j]] == 0
			if ei != ej {
				return ei
			}
			return free[i] < free[j]
		})
		k := 0
		for _, i := range idx {
			if _, ok := assigned[i]; ok {
				continue
			}
			assigned[i] = free[k]
			k++
		}
		for i, f := range assigned {
			out.Modules[i].Flag = f
		}
	}
	return &out
}
