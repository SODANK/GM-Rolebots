package rolebots

import (
	"math"
	"strings"
)

// The reprocessing ladder: Science IV, Metallurgy, then <Ore> Processing.
//
// Every ore processing skill needs Metallurgy (I for the highsec rocks, up to IV
// for Crokite/Bistot/Arkonor), and Metallurgy needs Science IV. The lever arm
// prices only a processing skill's own +5% yield and the road is days long, so
// the chain is priced as a chain: opening a processing skill is a step
// (processingOpenWorth), and each stepping stone is worth the share of it that
// its minutes are of the road's. Careers that refine climb it: industrialists
// and traders; miners are left to the refinery pair (goalRefine).

const (
	// processingOpenWorth is what opening a processing skill is worth, on the factory
	// gates' anchor: between refineGateWorth (0.25) and industryGateWorth (1.0).
	// Opening one lets a pilot refine that family at a profit: a step, not a slope.
	processingOpenWorth = 0.35
	// processingMaxLevel is as far as one plan climbs a processing skill; V is days
	// on the higher rocks for one more 5%.
	processingMaxLevel = 4
	// processingHandledMax caps how many rocks the pilot already handles get
	// a goal; the ladder's next rung is always offered on top of these.
	processingHandledMax = 2
	// processingMinIndustry and processingMinTrade decide who is offered the ladder.
	// 0.45 keeps the consumer side out (wIndustry median is about 0.6); 0.5 is a
	// pilot that leans to trade (wTrade median is about 0.4).
	processingMinIndustry = 0.45
	processingMinTrade    = 0.5

	// skillScrapmetalProcessing is evedb.GetOreProcessingSkill's fallback answer, not a rock.
	skillScrapmetalProcessing = 12196

	processingScienceWhy = "Science IV opens Metallurgy, the gate to the ore processing skills"
)

// processingRung is one step up the ladder: the processing skills that open
// the next scarce mineral, and how much more those minerals are worth than
// the rung below. The order is the climb.
type processingRung struct {
	skills []string
	weight float64
}

// processingLadder climbs toward the minerals the roster cannot get: rung 1 is
// highsec Nocxium (Pyroxeres), 2 and 3 are the lowsec Zydrine rocks, 4 is Megacyte,
// which waits because its Refinery Efficiency V step is past overStepCap.
var processingLadder = []processingRung{
	{[]string{"Pyroxeres Processing"}, 1.0},
	{[]string{"Jaspet Processing", "Hemorphite Processing"}, 1.4},
	{[]string{"Hedbergite Processing"}, 1.6},
	{[]string{"Crokite Processing", "Bistot Processing", "Arkonor Processing"}, 1.8},
}

// processingRungWeight is the ladder weight of a processing skill: its
// rung's, or 1.0 for a rock off the ladder (Veldspar, Kernite, ...).
func processingRungWeight(name string) float64 {
	for _, r := range processingLadder {
		for _, s := range r.skills {
			if s == name {
				return r.weight
			}
		}
	}
	return 1.0
}

// processingGoalValue is the whole price rule, pure so the harness pins it.
// One level at a time: from 0 it is the opening step, above 0 the lever arm
// on the yield term that skill multiplies.
func processingGoalValue(before, after int, w, ref, rung float64) float64 {
	if after <= before || w <= 0 || ref <= 0 {
		return 0
	}
	if before == 0 {
		return w * ref * processingOpenWorth * rung
	}
	gain := (1+refinePerOreSkill*float64(after))/(1+refinePerOreSkill*float64(before)) - 1
	return w * ref * rung * gain
}

// procWeight is the two refining careers' claim on the ladder.
func (p *skillPlanner) procWeight() float64 {
	return p.wIndustry + refineTradeShare*p.wTrade
}

// processingEligible is whether this pilot is offered the ladder at all.
func (p *skillPlanner) processingEligible() bool {
	return p.wIndustry >= processingMinIndustry || p.wTrade >= processingMinTrade
}

// processingGoal is one goal the ladder wants emitted.
type processingGoal struct {
	typeID  int64
	level   int
	why     string
	leadsTo int64
}

// processingGoals is the ladder for this pilot: the next level of the rocks it
// handles, of the next unopened rung, and of the Science and Metallurgy standing
// in front of that rung, one level at a time.
func (p *skillPlanner) processingGoals() []processingGoal {
	if !p.processingEligible() {
		return nil
	}
	var out []processingGoal
	targets := append([]int64(nil), p.procOres...)
	next := p.nextProcessingRung()
	if next != 0 {
		targets = appendUniqueCapped(targets, next, len(targets)+1)
	}
	for _, id := range targets {
		lv := p.levels[id]
		if lv >= processingMaxLevel {
			continue
		}
		out = append(out, processingGoal{typeID: id, level: lv + 1,
			why: "refines " + processingOreName(p.rows[id].TypeName) + " at a better yield"})
	}
	if next == 0 {
		return out
	}
	// The road to the next rung, spelled out so it is a plan line of its own
	// and not only a prerequisite buried under the processing skill.
	needMet := 0
	for _, r := range p.prereqs[next] {
		if r.SkillID == skillMetallurgy {
			needMet = r.Level
		}
	}
	if lv := p.levels[skillMetallurgy]; lv < needMet {
		if sci := p.levels[skillScience]; sci < 4 {
			out = append(out, processingGoal{typeID: skillScience, level: sci + 1,
				why: processingScienceWhy, leadsTo: next})
		}
		out = append(out, processingGoal{typeID: skillMetallurgy, level: lv + 1,
			why: "Metallurgy opens " + p.rows[next].TypeName, leadsTo: next})
	}
	return out
}

// nextProcessingRung is the first processing skill up the ladder this pilot
// has not opened and could open within its step cap, or 0.
func (p *skillPlanner) nextProcessingRung() int64 {
	for _, r := range processingLadder {
		for _, name := range r.skills {
			id := p.skillByName(name)
			if id == 0 || p.levels[id] > 0 {
				continue
			}
			chain, ok := p.chainFor(id, 1)
			if !ok || p.overStepCap(chain) {
				continue
			}
			return id
		}
	}
	return 0
}

// evaluateProcessing prices a ladder goal (see processingGoalValue). A stepping
// stone is worth the opening value of the skill it leads to, times its own
// minutes over the whole road's.
func (p *skillPlanner) evaluateProcessing(g *skillGoal) {
	g.valued = true
	target := g.TypeID
	if g.leadsTo != 0 {
		target = g.leadsTo
	}
	before := p.levels[target]
	after := g.Level
	if g.leadsTo != 0 {
		after = before + 1
	}
	v := processingGoalValue(before, after, p.procWeight(), p.poolRef,
		processingRungWeight(p.rows[target].TypeName))
	if g.leadsTo != 0 && v > 0 {
		road, ok := p.chainFor(target, before+1)
		mins, _, _ := p.chainCost(road)
		if !ok || mins <= 0 {
			v = 0
		} else {
			v *= math.Min(1, g.minutes/mins)
		}
	}
	g.value = v
}

// processingOreName is the rock a processing skill is for.
func processingOreName(skillName string) string {
	return strings.TrimSuffix(skillName, " Processing")
}

// appendUniqueCapped appends id unless it is already there or the slice is at
// max.
func appendUniqueCapped(ids []int64, id int64, max int) []int64 {
	for _, x := range ids {
		if x == id {
			return ids
		}
	}
	if len(ids) >= max {
		return ids
	}
	return append(ids, id)
}
