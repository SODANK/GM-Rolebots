package rolebots

import (
	"math"

	evedb "gurimoon/db"
)

// The high-sec suicide gank: the arithmetic and its dials.
//
// A gank is decided by the police delay in the band (concordDelayFor), the
// strikers' lock time and the mark's hitpoints. This file holds that arithmetic
// only, so the embodied and abstract ops size a strike alike. gank_test.go pins
// the reference table.

// ── dials ───────────────────────────────────────────────────────────────────

var (
	// dialGankOn is the master switch for raising gank fleets. Off: the doctrine,
	// seat and arithmetic exist and nothing acts on them.
	dialGankOn = &pvpDial{key: "rolebots.gank.on", def: 0, lo: 0, hi: 1, step: 1,
		label: "Gank fleets (1 = Predation corps raise them, 0 = off)"}
	// dialGankMinCargoISK is the cargo value a mark must carry before a strike is
	// worth the ships.
	dialGankMinCargoISK = &pvpDial{key: "rolebots.gank.minCargoISK", def: 5e6, lo: 0, hi: 1e10, step: 1e6,
		label: "Minimum scanned cargo value to call a gank (ISK)"}
	// dialGankMarginPct is the DPS margin over the bare kill, for misses, mark
	// repairs and late locks.
	dialGankMarginPct = &pvpDial{key: "rolebots.gank.marginPct", def: 25, lo: 0, hi: 200, step: 5,
		label: "Damage margin over the bare kill (%)"}
	// dialGankSecCeil is the highest security a gank fleet works. 1.0 is every band;
	// 0.6 keeps it to lower empire.
	dialGankSecCeil = &pvpDial{key: "rolebots.gank.secCeil", def: 1.0, lo: 0.45, hi: 1.0, step: 0.1,
		label: "Highest system security a gank fleet works"}
	// dialGankLockSec is the seconds a striker spends locking before its first
	// volley. No gun can use that part of the police window.
	dialGankLockSec = &pvpDial{key: "rolebots.gank.lockSec", def: 3, lo: 0, hi: 15, step: 0.5,
		label: "Seconds to lock before the first volley"}
)

// gankDials is every dial in this file, appended to pvpDials() for the loader.
func gankDials() []*pvpDial {
	return append([]*pvpDial{dialGankOn, dialGankMinCargoISK, dialGankMarginPct, dialGankSecCeil, dialGankLockSec},
		gankUndockDials()...)
}

// gankOn reports the master switch.
func gankOn() bool { return dialGankOn.Get() >= 0.5 }

// ── the arithmetic ──────────────────────────────────────────────────────────

// gankWindow is the seconds of fire a striker gets at this security: the band's
// police delay minus the lock time, never below 0. policed is false below the
// police band, where there is no clock and piracyStrikeWindow sizes the strike.
func gankWindow(sec, lockSec float64) (window float64, policed bool) {
	delay, policed := concordDelayFor(sec)
	if !policed {
		return 0, false
	}
	return math.Max(delay-math.Max(lockSec, 0), 0), true
}

// gankShipsNeeded is how many strikers of dpsPerShip put victimEHP on the mark
// inside window seconds with marginPct over the bare kill. It is 0 when there is
// no window, damage or victim. The count is ceilinged, not rounded.
func gankShipsNeeded(victimEHP, dpsPerShip, window, marginPct float64) int {
	if victimEHP <= 0 || dpsPerShip <= 0 || window <= 0 {
		return 0
	}
	need := victimEHP * (1 + math.Max(marginPct, 0)/100) / (dpsPerShip * window)
	return int(math.Ceil(need - 1e-9))
}

// gankStrikeSec is how long an abstract strike takes from the first lock: the
// lock time, then the mark's hitpoints over the line's summed damage. Pure. A
// line with no damage reads as the lock alone. CONCORD's stopwatch reads it.
func gankStrikeSec(victimEHP float64, dps []float64, lockSec float64) float64 {
	sum := 0.0
	for _, d := range dps {
		if d > 0 {
			sum += d
		}
	}
	lock := math.Max(lockSec, 0)
	if sum <= 0 || victimEHP <= 0 {
		return lock
	}
	return lock + victimEHP/sum
}

// gankValid reports whether the call stands: a hold worth at least minISK and
// enough staged strikers. ships is gankShipsNeeded's answer.
func gankValid(cargoISK, minISK float64, ships, staged int) bool {
	return ships > 0 && staged >= ships && cargoISK >= minISK && cargoISK > 0
}

// gankCargoItem is one stack a cargo scan returned.
type gankCargoItem struct {
	TypeID int64
	Qty    int64
}

// gankCargoValue prices a scanned hold with the killboard valuation
// (evedb.TypeValues), so the call uses the figure the killmail will show.
// Unpriced types count nothing.
func gankCargoValue(items []gankCargoItem, prices map[int64]evedb.TypeValue) float64 {
	total := 0.0
	for _, it := range items {
		if it.Qty <= 0 {
			continue
		}
		if p, ok := prices[it.TypeID]; ok && p.Value > 0 {
			total += p.Value * float64(it.Qty)
		}
	}
	return total
}

// gankTypeIDs is the distinct types of a scan, for one TypeValues call.
func gankTypeIDs(items []gankCargoItem) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(items))
	for _, it := range items {
		if it.TypeID > 0 && !seen[it.TypeID] {
			seen[it.TypeID] = true
			out = append(out, it.TypeID)
		}
	}
	return out
}
