package rolebots

// The starter kit as a substitute, not only an exact match.
//
// grantStarterLines (broke.go) matches the free starter lines by typeID, but the line a fit is short of is
// almost never one of those. Substitution is by invGroups group, the game's own taxonomy:
//
//	a missing mining laser  becomes the Basic Miner (same group 54, same job).
//	a missing weapon        becomes a civilian gun, across families: all four civilian guns are group 53 here,
//	                        and the belt refusal tests only whether the hull can shoot (shipHasNoTeeth, threat.go).
//
// Nothing else is substituted. Charges never are: grantStarterLines fits into an empty slot and a charge shares its gun's slot flag.

import (
	"context"

	evedb "gurimoon/db"
)

// starterWeaponGroups are the groups a missing line has to be in for the
// civilian gun to stand in for it: the three turret families plus every
// launcher band. Built from hulldoctrine.go's own tables rather than a fresh
// list, so a group added there cannot be forgotten here.
func starterWeaponGroups() map[int64]bool {
	out := make(map[int64]bool, 8)
	for _, g := range turretGroups {
		out[g] = true
	}
	for _, groups := range launcherBands {
		for _, g := range groups {
			out[g] = true
		}
	}
	return out
}

// starterMiningGroups are the groups the Basic Miner stands in for. The strip miner and deep-core groups
// are included: a barge short of a strip miner is better off cutting rock slowly than reporting no usable fit.
func starterMiningGroups() map[int64]bool {
	out := make(map[int64]bool, 3)
	for _, g := range roleGroups[roleMiner] {
		out[g] = true
	}
	return out
}

// starterSubstituteFor is the free module that will stand in for this one, or 0 when nothing should. The
// typeID is looked up because ShopItem carries the type, not its group, and widening it would add a field
// to every shopping line.
func (m *Mod) starterSubstituteFor(ctx context.Context, typeID int64) int64 {
	if typeID == 0 || freeStarterModules[typeID] {
		return 0 // already free: grantStarterLines' exact match handles it
	}
	// A deep core miner is never stood in for: it is the pilot's only way into Mercoxit (minercap.go), and a
	// Basic Miner would lose that.
	if attrs, err := evedb.GetTypeAttributes(ctx, m.db, typeID); err == nil && minerKindOf(attrs) == minerDeepCore {
		return 0
	}
	groupID := m.typeGroupOf(ctx, typeID)
	if groupID == 0 {
		return 0
	}
	switch {
	case starterMiningGroups()[groupID]:
		return basicMinerTypeID
	case starterWeaponGroups()[groupID]:
		// One gun for every family: the civilian guns are all group 53 here and the belt refusal asks only whether
		// the hull is armed. The autocannon needs no charge or capacitor.
		return civGatlingAutocannonTypeID
	}
	return 0
}

// substituteStarterLines rewrites a shopping list so lines a destitute pilot cannot buy become the free
// lines it can be given. It runs before grantStarterLines and issues nothing itself, so one place creates
// and fits a free module. A line with no substitute passes through.
func (m *Mod) substituteStarterLines(ctx context.Context, b *botRun, missing []ShopItem) []ShopItem {
	out := make([]ShopItem, 0, len(missing))
	for _, it := range missing {
		sub := int64(0)
		if it.Dest == "module" {
			sub = m.starterSubstituteFor(ctx, it.TypeID)
		}
		if sub == 0 {
			out = append(out, it)
			continue
		}
		was := it.Name
		it.TypeID = sub
		it.Name = m.typeNameOr(ctx, sub)
		it.Qty = 1
		m.logBot(b.CharID, logInfo, "procure",
			"it cannot buy a %s — taking the free %s instead, which does the same job",
			was, it.Name)
		out = append(out, it)
	}
	return out
}
