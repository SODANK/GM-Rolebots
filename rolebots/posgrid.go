package rolebots

import (
	"context"
	"sort"
	"strings"
)

// A tower's fitting grid as the core enforces it (db/starbasegrid.go): the tower type's own
// powerOutput/cpuOutput (attrs 11 / 48) plus whatever its online boosters add. The planner
// keeps one grid per race and size, since a Caldari Large (2.75M MW, 7,500 tf) differs from
// the Amarr numbers.

// gridSpec is one tower's power (MW) and CPU (tf) budget.
type gridSpec struct{ Power, CPU float64 }

// raceGrid is every shipped player control tower's own grid, by race and size
// (dgmTypeAttributes 11 / 48 on the tower types in towerTypes).
var raceGrid = map[int64]map[string]gridSpec{
	raceAmarr: {
		towerSmall:  {1_250_000, 1375},
		towerMedium: {2_500_000, 2750},
		towerLarge:  {5_000_000, 5500},
	},
	raceCaldari: {
		towerSmall:  {687_500, 1875},
		towerMedium: {1_375_000, 3750},
		towerLarge:  {2_750_000, 7500},
	},
	raceGallente: {
		towerSmall:  {937_500, 1688},
		towerMedium: {1_875_000, 3375},
		towerLarge:  {3_750_000, 6750},
	},
	raceMinmatar: {
		towerSmall:  {1_093_750, 1500},
		towerMedium: {2_187_500, 3000},
		towerLarge:  {4_375_000, 6000},
	},
}

// Grid boosters: structures that carry an output of their own. Their draw is in structGrid;
// what they add is boosterOut, mirroring attrs 11 / 48 on the types in
// sql/dgmtypeattributes.sql (keep the two in step).
const (
	typeAuxSmall  = 16217 // Small Auxiliary Power Array  (+power)
	typeAuxMedium = 17172 // Medium Auxiliary Power Array (+power)
	typeAuxLarge  = 17173 // Large Auxiliary Power Array  (+power)
	typeShieldGen = 16223 // Shield Generation Array       (+CPU)
)

var boosterOut = map[int64]gridSpec{
	typeAuxSmall:  {Power: 250_000},
	typeAuxMedium: {Power: 500_000},
	typeAuxLarge:  {Power: 1_000_000},
	typeShieldGen: {CPU: 600},
}

// boosterDraw is what a booster itself takes from the grid (attrs 30 / 50: 50
// MW and 200 tf on all four).
var boosterDraw = structFit{Power: 50, CPU: 200}

func isBoosterType(t int64) bool { _, ok := boosterOut[t]; return ok }

// gridFor is a tower's own grid: its race's table, else the size-only table
// for a tower whose race is unknown.
func gridFor(race int64, size string) gridSpec {
	if sizes, ok := raceGrid[race]; ok {
		if g, ok := sizes[size]; ok {
			return g
		}
	}
	g, ok := towerGrid[size]
	if !ok {
		g = towerGrid[towerSmall]
	}
	return gridSpec{g.Power, g.CPU}
}

// foldOutputs sums boosters' outputs, strongest first, each weaker by the stacking penalty:
// the core's rule (db.FoldGridOutputs). Pure.
func foldOutputs(vals []float64) float64 {
	v := make([]float64, 0, len(vals))
	for _, x := range vals {
		if x > 0 {
			v = append(v, x)
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(v)))
	total := 0.0
	for i, x := range v {
		total += x * stackingPenaltyLocal(i)
	}
	return total
}

// boostedGrid is a tower's own grid plus every booster in its plan that is not
// offline or on its way out. A booster still on order counts, exactly as a
// piece on order counts against the draw (gridUse): the plan is a commitment,
// and boosters are ordered ahead of what leans on them. Pure.
func boostedGrid(race int64, size string, pieces []layoutPiece) gridSpec {
	g := gridFor(race, size)
	var pw, cp []float64
	for _, pc := range pieces {
		if pc.Off || pc.Strip {
			continue
		}
		if o, ok := boosterOut[pc.TypeID]; ok {
			pw = append(pw, o.Power)
			cp = append(cp, o.CPU)
		}
	}
	g.Power += foldOutputs(pw)
	g.CPU += foldOutputs(cp)
	return g
}

// boostersToCover picks the boosters that make `need` fit a plan whose grid is
// short, cheapest-sufficient first: one array at a time (the smallest that
// closes the gap, else the largest), at most maxBoostersPerOrder, each re-judged
// against the stacking penalty. nil when no booster closes the gap. Pure.
func boostersToCover(race int64, size string, pieces []layoutPiece, need structFit) []int64 {
	const maxBoostersPerOrder = 4
	work := append([]layoutPiece(nil), pieces...)
	var picked []int64
	fits := func() bool {
		g := boostedGrid(race, size, work)
		u := gridUse(work)
		return u.Power+need.Power <= g.Power && u.CPU+need.CPU <= g.CPU
	}
	for len(picked) < maxBoostersPerOrder && !fits() {
		g := boostedGrid(race, size, work)
		u := gridUse(work)
		var cand []int64
		if u.CPU+need.CPU > g.CPU {
			cand = []int64{typeShieldGen}
		} else {
			cand = []int64{typeAuxSmall, typeAuxMedium, typeAuxLarge}
		}
		choice := cand[len(cand)-1]
		for _, t := range cand {
			trial := append(append([]layoutPiece(nil), work...), layoutPiece{TypeID: t})
			tg, tu := boostedGrid(race, size, trial), gridUse(trial)
			gain := (tg.Power - g.Power) - (tu.Power - u.Power)
			deficit := u.Power + need.Power - g.Power
			if t == typeShieldGen {
				gain = (tg.CPU - g.CPU) - (tu.CPU - u.CPU)
				deficit = u.CPU + need.CPU - g.CPU
			}
			if gain >= deficit {
				choice = t
				break
			}
		}
		trial := append(append([]layoutPiece(nil), work...), layoutPiece{TypeID: choice})
		tg, tu := boostedGrid(race, size, trial), gridUse(trial)
		// A booster that does not raise the headroom of the short resource is
		// no help (a fully stacked wall of arrays); stop rather than order it.
		if choice == typeShieldGen {
			if (tg.CPU-tu.CPU) <= (g.CPU-u.CPU) {
				return nil
			}
		} else if (tg.Power - tu.Power) <= (g.Power - u.Power) {
			return nil
		}
		work = trial
		picked = append(picked, choice)
	}
	if !fits() {
		return nil
	}
	return picked
}

// planDraw is what a set of pieces would draw from the grid. Pure.
func planDraw(pieces []layoutPiece) structFit { return gridUse(pieces) }

// ── online order ────────────────────────────────────────────────────────────

// The rank a structure comes online in (advanceRigging): grid boosters first, because they
// make the room everything else needs; then the pieces that earn; then hardeners; batteries
// last.
const (
	rankBooster = iota
	rankWork
	rankHardener
	rankBattery
)

// onlineRank is a structure type's place in the online order. Pure.
func onlineRank(t int64) int {
	switch {
	case isBoosterType(t):
		return rankBooster
	case isDefenseType(t):
		return rankBattery
	case isHardener(t):
		return rankHardener
	}
	return rankWork
}

// onlineOrder is the layout's indices in the order their pieces come online: by onlineRank,
// layout order within a rank. Pure.
func onlineOrder(pieces []layoutPiece) []int {
	idx := make([]int, len(pieces))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return onlineRank(pieces[idx[a]].TypeID) < onlineRank(pieces[idx[b]].TypeID)
	})
	return idx
}

// isGridRefusal is whether the core refused to online a structure for want of power or CPU
// (db.StructureGridRefusal's sentences). Pure.
func isGridRefusal(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "control tower cannot power") || strings.Contains(s, "does not have the CPU for")
}

// roomFor picks which online pieces of a later rank than `rank` to take offline so a piece
// drawing `need` fits a grid with `free` headroom: batteries before hardeners, the biggest draw
// first within each, the fewest pieces that do it. nil when even all of them free too little
// (the layout itself is over its grid, and taking defences down would not help). online[i]
// marks the pieces drawing on the grid now. Pure.
func roomFor(pieces []layoutPiece, online []bool, rank int, free, need structFit) []int {
	fits := func() bool { return need.Power <= free.Power && need.CPU <= free.CPU }
	if fits() {
		return nil
	}
	var cand []int
	for i, pc := range pieces {
		if i < len(online) && online[i] && onlineRank(pc.TypeID) > rank {
			cand = append(cand, i)
		}
	}
	sort.SliceStable(cand, func(a, b int) bool {
		ra, rb := onlineRank(pieces[cand[a]].TypeID), onlineRank(pieces[cand[b]].TypeID)
		if ra != rb {
			return ra > rb
		}
		fa, fb := pieceFit(pieces[cand[a]].TypeID), pieceFit(pieces[cand[b]].TypeID)
		if fa.CPU != fb.CPU {
			return fa.CPU > fb.CPU
		}
		return fa.Power > fb.Power
	})
	var off []int
	for _, i := range cand {
		f := pieceFit(pieces[i].TypeID)
		free.Power += f.Power
		free.CPU += f.CPU
		off = append(off, i)
		if fits() {
			return off
		}
	}
	return nil
}

// typeNames names a list of types for a log line.
func (m *Mod) typeNames(ctx context.Context, ts []int64) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, m.typeNameOr(ctx, t))
	}
	return out
}
