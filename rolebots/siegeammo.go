package rolebots

import (
	"context"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// A siege seat's ammunition. A fit carries a trip's worth of rounds (ammoReloadsFor); a bash runs
// a whole siegeWindowSec on the same guns. A siege fleet's member, docked at its muster, buys the
// rounds its guns burn in that window (siegeRoundsPerGun) into its hold, as far as the hold holds
// them, once per fleet; the reload from the hold (gunreload.go) feeds them to the guns.

// siegeAmmoMargin is the deep load's head-room over a window of continuous fire.
const siegeAmmoMargin = 1.1

// siegeRoundsPerGun is how many rounds one gun of the given cycle fires over window seconds, with
// margin. A gun with no cycle reads 0. Pure.
func siegeRoundsPerGun(cycleSec, windowSec, margin float64) int64 {
	if cycleSec <= 0 || windowSec <= 0 {
		return 0
	}
	if margin <= 0 {
		margin = 1
	}
	return int64(math.Ceil(windowSec / cycleSec * margin))
}

// siegeAmmoShortfall is what to buy so the ship carries need of each charge type: the gap over
// what is aboard, cut to what freeM3 of hold holds (vol is each type's unit volume), the biggest
// gaps first, ties by typeID. A type with no known volume is skipped. Pure.
func siegeAmmoShortfall(need, aboard map[int64]int64, freeM3 float64, vol map[int64]float64) map[int64]int64 {
	type gap struct{ typeID, qty int64 }
	var gaps []gap
	for t, n := range need {
		if g := n - aboard[t]; g > 0 && vol[t] > 0 {
			gaps = append(gaps, gap{t, g})
		}
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].qty != gaps[j].qty {
			return gaps[i].qty > gaps[j].qty
		}
		return gaps[i].typeID < gaps[j].typeID
	})
	out := map[int64]int64{}
	left := freeM3
	for _, g := range gaps {
		fit := int64(left / vol[g.typeID])
		q := min(g.qty, fit)
		if q <= 0 {
			continue
		}
		out[g.typeID] = q
		left -= float64(q) * vol[g.typeID]
	}
	return out
}

// siegeAmmoDone is the fleet each pilot last deep-loaded for, so the shopping runs once per fleet.
var siegeAmmoDone sync.Map // charID → gangID

// siegeDeepLoad sends a docked siege-fleet member shopping for the window's rounds when its hold
// is short of them, once per fleet. Reports whether a restock trip began (the caller's step is
// then handled).
func (m *Mod) siegeDeepLoad(ctx context.Context, b *botRun, a *fitShip, t *fitTemplate, activity string) (bool, error) {
	gangID := gangIDOf(b)
	if gangID == 0 || b.StationID == 0 || a == nil || t == nil || t.Fit == nil || b.ShipID != a.ShipItemID {
		return false, nil
	}
	if done, ok := siegeAmmoDone.Load(b.CharID); ok && done.(int64) == gangID {
		return false, nil
	}
	g, _, err := gangForChar(ctx, m.db, b.CharID)
	if err != nil || g == nil || g.State.SiegeID == 0 || g.State.OpPhase != gangOpMuster {
		return false, nil
	}
	siegeAmmoDone.Store(b.CharID, gangID)

	var hullTypeID int64
	if err := m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, a.ShipItemID).Scan(&hullTypeID); err != nil {
		return false, nil
	}
	l, err := m.loadCombatLoadout(ctx, b.CharID, a.ShipItemID, hullTypeID)
	if err != nil || l == nil {
		return false, nil
	}
	need := map[int64]int64{}
	vol := map[int64]float64{}
	for _, w := range l.weapons {
		if w.ChargeTypeID == 0 {
			continue
		}
		if groupID, _, gerr := evedb.GetGroupAndCategory(ctx, m.db, w.ChargeTypeID); gerr == nil && durableChargeGroups[groupID] {
			continue // a crystal is not spent
		}
		need[w.ChargeTypeID] += siegeRoundsPerGun(w.CycleSec, siegeWindowSec, siegeAmmoMargin)
		if _, ok := vol[w.ChargeTypeID]; !ok {
			v, _ := evedb.GetTypeVolume(ctx, m.db, w.ChargeTypeID)
			vol[w.ChargeTypeID] = v
		}
	}
	if len(need) == 0 {
		return false, nil
	}
	aboard, err := m.ammoAboard(ctx, a.ShipItemID, t.Fit)
	if err != nil {
		return false, nil
	}
	free, err := m.holdFreeM3(ctx, b.CharID, a.ShipItemID)
	if err != nil {
		return false, nil
	}
	buy := siegeAmmoShortfall(need, aboard, free, vol)
	if len(buy) == 0 {
		return false, nil
	}
	var list []ShopItem
	for typeID, qty := range buy {
		name, _ := evedb.GetTypeName(ctx, m.db, typeID)
		list = append(list, ShopItem{TypeID: typeID, Name: name, Qty: qty, Dest: "cargo"})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].TypeID < list[j].TypeID })
	m.logBot(b.CharID, logInfo, "gang", "loading up for the siege — the bash burns more rounds than a patrol: %s",
		shopSummary(list))
	if err := m.startRestock(ctx, b, a, t, list, activity); err != nil {
		return false, err
	}
	return true, nil
}
