package rolebots

import (
	"math"

	"gurimoon/dogma"
)

// The deep core road: Mining V, Astrogeology V, then Deep Core Mining I.
//
// Mercoxit is the dearest rock per m³ and the only source of Morphite, and only
// a deep core miner can take anything out of it (minercap.go). The road is
// priced as a chain like the reprocessing ladder (skills_processing.go):
// opening Deep Core Mining is a step (deepCoreOpenWorth), and each stepping
// stone is worth the share of it that its minutes are of the road's. It is
// exempt from the step and group caps, one level per goal, because one level
// of Astrogeology V is about two weeks. Any pilot that mines climbs it, weighed
// by how much it mines; the fit side is roleDeepCore (fitgen.go).

const (
	// deepCoreOpenWorth is what opening Deep Core Mining is worth on the factory
	// gates' anchor. It sits above processingOpenWorth (0.35) because it unlocks a
	// rock a pilot could not touch.
	deepCoreOpenWorth = 0.5

	deepCoreWhy = "the road to Deep Core Mining, and the Mercoxit only a deep core miner can touch"
)

// deepCoreGoal is the next level on the road for this pilot, or ok=false when
// it is not a miner, already holds Deep Core Mining, or the road cannot be
// read. leadsTo is set on the stepping stones.
func (p *skillPlanner) deepCoreGoal() (typeID int64, level int, leadsTo int64, ok bool) {
	if p.wMine <= 0 || p.levels[dogma.SkillDeepCoreMining] >= 1 {
		return 0, 0, 0, false
	}
	if _, known := p.rows[dogma.SkillDeepCoreMining]; !known {
		return 0, 0, 0, false
	}
	for _, rung := range []int64{skillMining, skillAstrogeology} {
		if lv := p.levels[rung]; lv < 5 {
			return rung, lv + 1, dogma.SkillDeepCoreMining, true
		}
	}
	return dogma.SkillDeepCoreMining, 1, 0, true
}

// deepCoreGoalValue is the price rule, pure so the harness pins it: the
// opening step, scaled by the share of the road this goal's own minutes are.
func deepCoreGoalValue(w, ref, stepMinutes, roadMinutes float64) float64 {
	if w <= 0 || ref <= 0 || stepMinutes <= 0 || roadMinutes <= 0 {
		return 0
	}
	return w * ref * deepCoreOpenWorth * math.Min(1, stepMinutes/roadMinutes)
}

// evaluateDeepCore prices a road goal (see deepCoreGoalValue). The road is
// measured from where the pilot stands to Deep Core Mining I, so the share
// is the same whichever rung it is on.
func (p *skillPlanner) evaluateDeepCore(g *skillGoal) {
	g.valued = true
	road, ok := p.chainFor(dogma.SkillDeepCoreMining, 1)
	if !ok {
		g.value = 0
		return
	}
	roadMins, _, _ := p.chainCost(road)
	g.value = deepCoreGoalValue(p.wMine, p.poolRef, g.minutes, roadMins)
}
