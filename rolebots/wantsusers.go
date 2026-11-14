package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
)

// The users column: what the roster actually fits and loads, beside what it is merely trained for.
//
// `capable` (wants.go) is the loot fence's question; skills say nothing about whether the fit
// generator puts a thing on a hull. Production reads `users`:
//
//	ships, drones  users = capable
//	modules        pilots with one fitted, floored for groups the fit generator reaches
//	charges        pilots with a fitted module that loads the charge (group, size); no floor
//
// With no fitted module on the roster, every row uses its skill headcount. See docs/comment-notes-b20.md#users-column.

// capabilityLatentShare is the share of a reachable module's skill headcount counted as demand when
// fewer pilots than that have one fitted: worth stocking, not worth outranking the gear in use.
const capabilityLatentShare = 0.10

// capabilityCarryGridFrac is the most of a hull's base powergrid one module may need for the hull to
// count as carrying it. Engineering V adds a quarter, so 75% of the base grid is under 60% of a trained
// pilot's, leaving room for the rest of the rack.
const capabilityCarryGridFrac = 0.75

// fittedFlagLast is the last high slot (dogma/flags.go): 11-18 low, 19-26
// mid, 27-34 high.
const fittedFlagLast = flagHiFirst + 7

// fitReachableGroups is every module group the fit generator can choose for some hull: the role table
// plus the per-hull families resolved by tank and weapon. It is built from those tables, so a new group
// reaches the census without a second list.
func fitReachableGroups() map[int64]bool {
	out := map[int64]bool{}
	for _, gs := range roleGroups {
		for _, g := range gs {
			out[g] = true
		}
	}
	for _, t := range []TankPref{TankShield, TankArmor} {
		for _, gs := range [][]int64{tankGroups(t), hardenerGroups(t), remoteRepGroups(t)} {
			for _, g := range gs {
				out[g] = true
			}
		}
	}
	for g := range starterWeaponGroups() {
		out[g] = true
	}
	// Whole-family grid modules. Group 57 is absent: it also holds flux coils and relays with no grid
	// credit (fitgrid.go) that no fit takes, so it earns users only by what is fitted.
	out[grpCPUEnhancer] = true
	out[grpAuxPowerCore] = true
	return out
}

// moduleUsers is one module's production headcount. carriers is how many pilots own a hull that can
// carry it (gridCarriers), or negative when the module is not gated. The floor is a tenth of the capable
// pilots who could mount it, rounded up, so a module with one possible customer is still worth one. Pure.
func moduleUsers(fitted, capable int64, reachable bool, carriers int64) int64 {
	u := fitted
	if !reachable {
		return u
	}
	pool := capable
	if carriers >= 0 && carriers < pool {
		pool = carriers
	}
	if pool <= 0 {
		return u
	}
	return max(u, int64(math.Ceil(float64(pool)*capabilityLatentShare)))
}

// gridCarriers is how many pilots can carry a module needing pg MW: those whose
// biggest assembled hull has pg within capabilityCarryGridFrac of its grid.
// grids is one entry per pilot, sorted ascending. Pure.
func gridCarriers(grids []float64, pg float64) int64 {
	need := pg / capabilityCarryGridFrac
	i := sort.SearchFloat64s(grids, need)
	// SearchFloat64s finds the first grid >= need, which is the rule exactly.
	return int64(len(grids) - i)
}

// rosterHullGrids is, per enabled pilot, the base powergrid of the biggest assembled hull it owns,
// sorted ascending. It is driven from the ship types so the typeID key does the work. A packaged hull
// does not count: it is merchandise, not a ship anybody flies.
func (m *Mod) rosterHullGrids(ctx context.Context) ([]float64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT STRAIGHT_JOIN e.ownerID, MAX(COALESCE(NULLIF(a.valueFloat, 0), a.valueInt))
		FROM invGroups g
		JOIN invTypes t ON t.groupID = g.groupID
		JOIN dgmTypeAttributes a ON a.typeID = t.typeID AND a.attributeID = ?
		JOIN entity e ON e.typeID = t.typeID AND e.singleton = 1
		JOIN rolebot_characters b ON b.characterID = e.ownerID AND b.enabled = 1
		WHERE g.categoryID = ?
		GROUP BY e.ownerID`, attrPGOutput, catShip)
	if err != nil {
		return nil, fmt.Errorf("roster hull grids: %w", err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var owner int64
		var pg float64
		if err := rows.Scan(&owner, &pg); err != nil {
			return nil, fmt.Errorf("roster hull grids scan: %w", err)
		}
		out = append(out, pg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Float64s(out)
	return out, nil
}

// moduleCarriers is the carrier count for every module the census holds, or nil when the hull or module
// read fails, in which case the floor falls back to the skill headcount alone.
func (m *Mod) moduleCarriers(ctx context.Context, tallies []capabilityTally) map[int64]int64 {
	grids, err := m.rosterHullGrids(ctx)
	if err != nil {
		rlogf("rolebots: capability census: %v — module floor not held to hulls this time", err)
		return nil
	}
	mods, err := m.allModulesByID(ctx)
	if err != nil {
		rlogf("rolebots: capability census: %v — module floor not held to hulls this time", err)
		return nil
	}
	out := map[int64]int64{}
	for _, t := range tallies {
		if t.item.CategoryID != catModule {
			continue
		}
		if mm := mods[t.item.TypeID]; mm != nil && mm.PG > 0 {
			out[t.item.TypeID] = gridCarriers(grids, mm.PG)
		}
	}
	return out
}

// chargeAccept is one module type accepting one charge group at one size.
type chargeAccept struct{ module, group, size int64 }

// moduleChargeAccepts reads which module accepts which charge group, and at what size (the five
// chargeGroup columns, wants.go chargeGroupAttrs).
func (m *Mod) moduleChargeAccepts(ctx context.Context) ([]chargeAccept, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ta.typeID, CAST(COALESCE(NULLIF(ta.valueFloat,0), ta.valueInt) AS SIGNED) grp,
		       COALESCE(cs.sz, 0)
		FROM dgmTypeAttributes ta
		JOIN invTypes t ON t.typeID = ta.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = ?
		LEFT JOIN (SELECT typeID, COALESCE(NULLIF(valueFloat,0), valueInt) sz
		           FROM dgmTypeAttributes WHERE attributeID = ?) cs ON cs.typeID = t.typeID
		WHERE ta.attributeID IN (`+placeholders(len(chargeGroupAttrs))+`)
		  AND COALESCE(NULLIF(ta.valueFloat,0), ta.valueInt) > 0`,
		append([]any{catModule, attrChargeSize}, int64Args(chargeGroupAttrs)...)...)
	if err != nil {
		return nil, fmt.Errorf("charge accepts: %w", err)
	}
	defer rows.Close()
	var out []chargeAccept
	for rows.Next() {
		var a chargeAccept
		if err := rows.Scan(&a.module, &a.group, &a.size); err != nil {
			return nil, fmt.Errorf("charge accepts scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// rosterFittedOwners is, per module type, the enabled pilots with one fitted in a slot, on the ship
// they fly or on a spare in the hangar. It is one DISTINCT pass over the roster's slots.
func (m *Mod) rosterFittedOwners(ctx context.Context) (map[int64][]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.typeID, e.ownerID
		FROM entity e
		JOIN rolebot_characters b ON b.characterID = e.ownerID AND b.enabled = 1
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = ?
		WHERE e.flag BETWEEN ? AND ?`, catModule, flagLoFirst, fittedFlagLast)
	if err != nil {
		return nil, fmt.Errorf("roster fitted modules: %w", err)
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var typeID, owner int64
		if err := rows.Scan(&typeID, &owner); err != nil {
			return nil, fmt.Errorf("roster fitted modules scan: %w", err)
		}
		out[typeID] = append(out[typeID], owner)
	}
	return out, rows.Err()
}

// chargeLoaders folds fitted modules into the pilots able to load each (charge group, size): the union
// of every fitted module's owners that accepts it. Pure.
func chargeLoaders(accepts []chargeAccept, fitted map[int64][]int64) map[chargeAccept]map[int64]bool {
	out := map[chargeAccept]map[int64]bool{}
	for _, a := range accepts {
		owners := fitted[a.module]
		if len(owners) == 0 {
			continue
		}
		k := chargeAccept{group: a.group, size: a.size}
		set := out[k]
		if set == nil {
			set = map[int64]bool{}
			out[k] = set
		}
		for _, o := range owners {
			set[o] = true
		}
	}
	return out
}

// chargeUsers is how many pilots can load a charge of this group and size.
// Sizeless ammunition (scripts and the like) is loaded by any module taking
// the group, so its pilots are the union across sizes. Pure.
func chargeUsers(loaders map[chargeAccept]map[int64]bool, group, size int64) int64 {
	if size != 0 {
		return int64(len(loaders[chargeAccept{group: group, size: size}]))
	}
	union := map[int64]bool{}
	for k, set := range loaders {
		if k.group != group {
			continue
		}
		for o := range set {
			union[o] = true
		}
	}
	return int64(len(union))
}

// rateUsers fills every tally's production headcount (see the file comment). It runs after
// rateCharges, so a charge's skill headcount is already its own.
func (m *Mod) rateUsers(ctx context.Context, tallies []capabilityTally, accepts []chargeAccept, sizes map[int64]int64) error {
	fitted, err := m.rosterFittedOwners(ctx)
	if err != nil {
		return err
	}
	if len(fitted) == 0 {
		// No fit anywhere on the roster: nothing to read, so every row keeps its skill headcount.
		for i := range tallies {
			tallies[i].users = tallies[i].capable
		}
		return nil
	}
	reach := fitReachableGroups()
	loaders := chargeLoaders(accepts, fitted)
	carriers := m.moduleCarriers(ctx, tallies)
	for i := range tallies {
		t := &tallies[i]
		switch t.item.CategoryID {
		case catModule:
			c, gated := carriers[t.item.TypeID]
			if !gated {
				c = -1
			}
			t.users = moduleUsers(int64(len(fitted[t.item.TypeID])), t.capable, reach[t.item.GroupID], c)
		case catCharge:
			t.users = chargeUsers(loaders, t.item.GroupID, sizes[t.item.TypeID])
		default:
			t.users = t.capable
		}
	}
	return nil
}

// productionCounts is capabilityCounts for the production side: the users headcount for a set of
// types. Absent means the census does not know the type, which is not zero users, so callers that
// veto on "nobody uses it" read capabilityHeads instead.
func (m *Mod) productionCounts(ctx context.Context, typeIDs []int64) map[int64]int64 {
	out := make(map[int64]int64, len(typeIDs))
	if len(typeIDs) == 0 {
		return out
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT typeID, users FROM rolebot_capability WHERE typeID IN (`+
			placeholders(len(typeIDs))+`)`, int64Args(typeIDs)...)
	if err != nil {
		rlogf("rolebots: production counts: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, users int64
		if err := rows.Scan(&typeID, &users); err != nil {
			return out
		}
		out[typeID] = users
	}
	return out
}

// censusUnused reports whether the census knows this type and nobody on the roster fits, loads or
// flies it. A type the census never counted (a category it does not cover: components, structures,
// fuel) is not unused.
func censusUnused(heads map[int64]censusHead, typeID int64) bool {
	h, ok := heads[typeID]
	return ok && h.Users == 0
}

// ── what the roster already holds ───────────────────────────────────────────
//
// The flood test (productFlooded) counts the roster's packaged stock beside the listings: units in an
// enabled pilot's station hangar and in the offices of every corporation a rolebot runs. Assembled
// ships, fitted racks, jobs, cargo holds and sell orders are not counted. rolebots.floodHeldShare
// scales how much of it counts.

// floodHeldShareKey is the server setting that scales held stock.
const floodHeldShareKey = "rolebots.floodHeldShare"

// floodHeldShare is the share of the roster's held stock the flood test
// counts: 0 (off) .. 1 (all of it), default 1.
func floodHeldShare(ctx context.Context, db *sql.DB) float64 {
	return min(max(escrowSettingFloat(ctx, db, floodHeldShareKey, 1), 0), 1)
}

// scaleHeld is held stock at the share. Pure.
func scaleHeld(held int64, share float64) int64 {
	if held <= 0 || share <= 0 {
		return 0
	}
	return int64(math.Round(float64(held) * share))
}

// floodSupply is what the flood test weighs against the want: the listings
// plus the roster's own held stock. Pure.
func floodSupply(listed, held int64) int64 {
	return max(listed, 0) + max(held, 0)
}

// heldOf is a type's held stock from the census heads, 0 when the census does
// not count the type.
func heldOf(heads map[int64]censusHead, typeID int64) int64 {
	return heads[typeID].Held
}

// rateHeld fills every tally's held stock with two reads grouped by type: the roster's hangars
// (idx_entity_owner_type) and the bot-run corporations' offices. Both scan the whole roster, so this
// runs in the census, off the plan path.
func (m *Mod) rateHeld(ctx context.Context, tallies []capabilityTally) error {
	held := map[int64]int64{}
	add := func(what, q string, args ...any) error {
		rows, err := m.db.QueryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("roster %s stock: %w", what, err)
		}
		defer rows.Close()
		for rows.Next() {
			var typeID int64
			var n sql.NullInt64
			if err := rows.Scan(&typeID, &n); err != nil {
				return fmt.Errorf("roster %s stock scan: %w", what, err)
			}
			held[typeID] += n.Int64
		}
		return rows.Err()
	}
	if err := add("hangar", `
		SELECT e.typeID, SUM(e.quantity)
		FROM rolebot_characters b
		JOIN entity e ON e.ownerID = b.characterID
		WHERE b.enabled = 1 AND e.flag = ? AND e.singleton = 0
		GROUP BY e.typeID`, flagHangar); err != nil {
		return err
	}
	if err := add("office", `
		SELECT e.typeID, SUM(e.quantity)
		FROM rolebot_ceos c
		JOIN crpoffices o ON o.corporationID = c.corporationID
		JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		WHERE e.singleton = 0 AND e.flag <> 111
		GROUP BY e.typeID`); err != nil {
		return err
	}
	for i := range tallies {
		tallies[i].held = held[tallies[i].item.TypeID]
	}
	return nil
}
