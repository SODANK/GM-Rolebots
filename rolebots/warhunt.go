package rolebots

import (
	"context"
	"fmt"
)

// Where a corporation at war sends its wing, and when a hunt may be called finished.
//
// The order goes where the enemy is: the system with the most of their pilots out in space, read
// from the exposed-pilot census the hunters use (legalcensus.go), and it stays there while it is still
// worth it. Enemy pilots move, so ordering the wing to the nearest enemy system would change every
// quarter hour and the wing would never meet anyone.

// warEnemyCensus counts the pilots of every corporation corpID is at war with,
// per system, among those out in space (the census holds nobody docked).
func (m *Mod) warEnemyCensus(ctx context.Context, corpID int64) map[int64]int {
	enemies := relWarEnemiesOf(corpID)
	if len(enemies) == 0 {
		return nil
	}
	set := make(map[int64]bool, len(enemies))
	for _, id := range enemies {
		set[id] = true
	}
	return warEnemyCounts(m.targetCensusNow(ctx).bySys, set)
}

// warEnemyCounts is warEnemyCensus over a census and an enemy set. Pure.
func warEnemyCounts(bySys map[int64][]censusTarget, enemies map[int64]bool) map[int64]int {
	out := map[int64]int{}
	for sysID, targets := range bySys {
		for _, t := range targets {
			if enemies[t.corpID] {
				out[sysID]++
			}
		}
	}
	return out
}

// warOrderLive reports whether any war enemy is out in sysID right now. The gang
// hunt asks it before calling a system "dry": a war order is not about rats.
func warOrderLive(bySys map[int64][]censusTarget, sysID int64, enemies map[int64]bool) bool {
	if sysID == 0 || len(enemies) == 0 {
		return false
	}
	for _, t := range bySys[sysID] {
		if enemies[t.corpID] {
			return true
		}
	}
	return false
}

// warSeatsFirst moves the seats of corporations at war to the front, keeping
// the order inside each half, and counts them. Pure over the predicate.
func warSeatsFirst(seats []fcSeat, atWar func(corpID int64) bool) ([]fcSeat, int) {
	out := make([]fcSeat, 0, len(seats))
	var rest []fcSeat
	for _, s := range seats {
		if atWar(s.corpID) {
			out = append(out, s)
		} else {
			rest = append(rest, s)
		}
	}
	n := len(out)
	return append(out, rest...), n
}

// warStickyShare is how much of the best system's enemy count the standing
// order's system must still hold for the wing to stay put.
const warStickyShare = 0.5

// pickWarTargetSystem chooses the system the wing is sent to: the one inside `near` (hop counts from
// headquarters) holding the most enemy pilots. The standing order's system `prev` is kept while it holds
// at least warStickyShare of the best count. Ties go to fewer hops, then the lower ID. 0 when no
// enemy is out in range. Pure.
func pickWarTargetSystem(counts map[int64]int, near map[int64]int, prev int64) (int64, string) {
	var best int64
	bestN := 0
	for sysID, n := range counts {
		hops, ok := near[sysID]
		if !ok || n <= 0 {
			continue
		}
		if best == 0 || n > bestN ||
			(n == bestN && (hops < near[best] || (hops == near[best] && sysID < best))) {
			best, bestN = sysID, n
		}
	}
	if best == 0 {
		return 0, ""
	}
	if prev != 0 && prev != best {
		if _, ok := near[prev]; ok {
			if n := counts[prev]; n > 0 && float64(n) >= float64(bestN)*warStickyShare {
				best, bestN = prev, n
			}
		}
	}
	return best, fmt.Sprintf("the enemy works there (%d of their pilots out)", bestN)
}
