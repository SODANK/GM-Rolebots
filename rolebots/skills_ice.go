package rolebots

import (
	"math"
)

// The barge and ice road (goalIce): Mining Barge first, then Ice Harvesting,
// then the levers that make a barge pilot's harvest bigger. Priced like the deep
// core road (skills_deepcore.go): one level per goal, exempt from the step and
// group caps. In order:
//  1. the barge: Astrogeology III and Industry V, then Mining Barge I. Not scaled
//     by ice demand, since a barge is a strip-mining hull first.
//  2. the ice: Mining IV, then Ice Harvesting I. Scaled by iceDemandIndex but
//     floored, not gated, so a world with no towers still trains a few.
//  3. the levers, once the pilot owns a barge: Ice Harvesting, Mining Barge,
//     Mining Upgrades I to III. Tank and fitting skills are the support levers'.

const (
	skillMiningUpgrades  = int64(22578)
	iceBargeOpenWorth    = 0.70 // opening a barge: strip mining, and the door to ice
	iceHarvestOpenWorth  = 0.70 // opening ice harvesting, at demand index 1
	iceDemandFloor       = 0.9  // ice demand never prices the road below this
	iceLeverWorth        = 2.0  // per unit of fractional harvest gain
	iceUpgradeGateGain   = 0.10 // Mining Upgrades I: two -5% upgrades in a Procurer's lows
	iceUpgradeCPUGain    = 0.01 // Mining Upgrades II-III: CPU only
	iceHarvestLeverCap   = 5
	iceBargeLeverCap     = 5
	iceUpgradeLeverCap   = 3
	iceRoadWhyBarge      = "the road to a mining barge, and the ice beyond it"
	iceRoadWhyIce        = "towers burn what ice refines into"
	iceLeverWhy          = "a bigger harvest out of its barge"
	iceHarvestCycleBonus = 0.05
	iceBargeCycleBonus   = 0.03
)

// iceGoal is one goal the ice half proposes.
type iceGoal struct {
	typeID  int64
	level   int
	leadsTo int64 // the road's gate for a stepping stone, 0 otherwise
	why     string
}

// iceGoals is the next level on the barge road, the ice road, and each lever.
func (p *skillPlanner) iceGoals() []iceGoal {
	if p.wMine <= 0 {
		return nil
	}
	var out []iceGoal
	// 1. The barge.
	if p.levels[skillMiningBarge] < 1 {
		for _, rung := range []struct {
			id  int64
			lvl int
		}{{skillAstrogeology, 3}, {skillIndustry, 5}} {
			if lv := p.levels[rung.id]; lv < rung.lvl {
				return append(out, iceGoal{rung.id, lv + 1, skillMiningBarge, iceRoadWhyBarge})
			}
		}
		return append(out, iceGoal{skillMiningBarge, 1, 0, iceRoadWhyBarge})
	}
	// 2. The ice.
	if p.levels[skillIceHarvesting] < 1 {
		if lv := p.levels[skillMining]; lv < 4 {
			return append(out, iceGoal{skillMining, lv + 1, skillIceHarvesting, iceRoadWhyIce})
		}
		return append(out, iceGoal{skillIceHarvesting, 1, 0, iceRoadWhyIce})
	}
	// 3. The levers, for a pilot with a barge to pull them on.
	if !p.ownsBarge() {
		return nil
	}
	for _, lv := range []struct {
		id  int64
		cap int
	}{{skillIceHarvesting, iceHarvestLeverCap}, {skillMiningBarge, iceBargeLeverCap}, {skillMiningUpgrades, iceUpgradeLeverCap}} {
		if cur := p.levels[lv.id]; cur < lv.cap {
			out = append(out, iceGoal{lv.id, cur + 1, 0, iceLeverWhy})
		}
	}
	return out
}

// ownsBarge reports whether the pilot owns a mining barge or exhumer
// anywhere. Memoised for the plan; a planner with no database (the harness)
// owns none.
func (p *skillPlanner) ownsBarge() bool {
	if p.hasBarge != nil {
		return *p.hasBarge
	}
	v := false
	if p.m != nil && p.m.db != nil && p.ctx != nil {
		var n int
		_ = p.m.db.QueryRowContext(p.ctx, `
			SELECT COUNT(*) FROM entity e JOIN invTypes t ON t.typeID = e.typeID
			 WHERE e.ownerID = ? AND t.groupID IN (?, ?)`,
			p.charID, groupMiningBarge, groupExhumer).Scan(&n)
		v = n > 0
	}
	p.hasBarge = &v
	return v
}

// iceDemandMul is the ice road's demand factor: the world's pull toward ice,
// floored so no tower is a reason never to start.
func iceDemandMul() float64 {
	return math.Max(iceDemandFloor, math.Min(2, iceDemandIndex()))
}

// iceRoadValue is the road price rule, pure so the harness pins it: the
// opening worth, scaled by the share of the road this goal's own minutes are.
func iceRoadValue(w, ref, open, demand, stepMinutes, roadMinutes float64) float64 {
	if w <= 0 || ref <= 0 || stepMinutes <= 0 || roadMinutes <= 0 {
		return 0
	}
	return w * ref * open * demand * math.Min(1, stepMinutes/roadMinutes)
}

// cycleLeverGain is the fractional harvest gain of one more level of a
// cycle-time skill worth `per` a level: harvest rate is 1/cycle.
func cycleLeverGain(per float64, from int) float64 {
	before := 1 - per*float64(from)
	after := 1 - per*float64(from+1)
	if after <= 0 || before <= 0 {
		return 0
	}
	return before/after - 1
}

// iceLeverGain is what one more level of an ice lever adds to the harvest.
func iceLeverGain(typeID int64, from int) float64 {
	switch typeID {
	case skillIceHarvesting:
		return cycleLeverGain(iceHarvestCycleBonus, from)
	case skillMiningBarge:
		// The ice cycle and the strip yield move together on an ORE hull;
		// counted once, since a barge does one or the other on a given day.
		return cycleLeverGain(iceBargeCycleBonus, from)
	case skillMiningUpgrades:
		if from == 0 {
			return iceUpgradeGateGain
		}
		return iceUpgradeCPUGain
	}
	return 0
}

// evaluateIce prices a goal on the barge and ice road, or one of its levers.
func (p *skillPlanner) evaluateIce(g *skillGoal) {
	g.valued = true
	g.value = 0
	if p.wMine <= 0 || p.poolRef <= 0 {
		return
	}
	if g.Why == iceLeverWhy {
		gain := iceLeverGain(g.TypeID, g.Level-1)
		g.value = p.wMine * p.poolRef * iceLeverWorth * gain * iceDemandMul()
		return
	}
	// The barge leg is worth having with no tower anywhere (a barge strip-mines),
	// so demand only raises it, never lowers it.
	gate, open, demand := int64(skillMiningBarge), iceBargeOpenWorth, math.Max(1, iceDemandMul())
	if g.Why == iceRoadWhyIce {
		gate, open, demand = skillIceHarvesting, iceHarvestOpenWorth, iceDemandMul()
	}
	road, ok := p.chainFor(gate, 1)
	if !ok {
		return
	}
	roadMins, _, _ := p.chainCost(road)
	g.value = iceRoadValue(p.wMine, p.poolRef, open, demand, g.minutes, roadMins)
}
