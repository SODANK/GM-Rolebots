package rolebots

import "math"

// The lifter's skills (goalLift): what an Infrastructure member trains so the
// corporation's heavy pieces (a Ship Maintenance Array, or a Large tower at
// 8,000 m³) go aboard one hull (haullift.go). One level per goal, so
// overStepCap never throws a chain away. A goal is priced by the lift it adds.
//
//   - the pilot's own racial Industrial, to liftIndCap: +5% hold a level, and
//     the three-low hull (III for the Badger Mark II, Iteron Mark III) is the big step;
//   - General Freight, to liftFreightCap: cargoCapacityBonus a level;
//   - Hull Upgrades I, for a pilot without it: the gate on Expanded Cargohold I.

const (
	liftIndCap     = 4
	liftFreightCap = 4
	// liftLevelWorth is a level that adds a tenth to the pilot's lift, on the
	// planner's pool anchor, about a level of Anchoring.
	liftLevelWorth = 0.25
	// liftGainCap caps the relative gain priced, so the step into a bigger
	// hull is worth a lot, not everything.
	liftGainCap = 0.3
	// liftShortMul prices the skills while the corporation holds a line too
	// big for any of its haulers; liftIdleMul while it has no starbase work.
	liftShortMul = 2.0
	liftIdleMul  = 0.4

	liftWhy = "lifts its corporation's heavy starbase pieces"
)

// liftCorp is the corporation whose Infrastructure arm this pilot sits in, 0
// for none. Memoised for the plan.
func (p *skillPlanner) liftCorp() int64 {
	if p.liftCorpID != nil {
		return *p.liftCorpID
	}
	v := int64(0)
	defer func() { p.liftCorpID = &v }()
	if !StarbasePower() || p.m == nil || p.m.db == nil || p.ctx == nil {
		return v
	}
	var corpID int64
	if err := p.m.db.QueryRowContext(p.ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, p.charID).Scan(&corpID); err != nil {
		return v
	}
	if infraRigger(p.charID, corpID) {
		v = corpID
	}
	return v
}

// liftGoals is the next level of each lifter skill still short of its cap.
func (p *skillPlanner) liftGoals() []commandGoal {
	if p.liftCorp() == 0 {
		return nil
	}
	var out []commandGoal
	if p.levels[skillHullUpgrades] < 1 {
		out = append(out, commandGoal{skillHullUpgrades, 1, liftWhy})
	}
	if ind := p.skillByName(racialShip(p.raceID, "Industrial")); ind != 0 && p.levels[ind] < liftIndCap {
		out = append(out, commandGoal{ind, p.levels[ind] + 1, liftWhy})
	}
	if p.levels[skillGeneralFreight] < liftFreightCap {
		out = append(out, commandGoal{skillGeneralFreight, p.levels[skillGeneralFreight] + 1, liftWhy})
	}
	return out
}

// evaluateLift prices a lifter goal: the lift the level adds, relative to
// what the pilot carries now.
func (p *skillPlanner) evaluateLift(g *skillGoal) {
	g.valued = true
	g.value = 0
	corpID := p.liftCorp()
	if corpID == 0 || p.poolRef <= 0 {
		return
	}
	book := p.m.liftCatalogue(p.ctx)
	before := liftPotential(p.levels, book)
	after := make(map[int64]int, len(p.levels)+1)
	for k, v := range p.levels {
		after[k] = v
	}
	after[g.TypeID] = g.Level
	mul := liftIdleMul
	if p.corpStarbaseActive(corpID) {
		mul = 1
		if p.corpHoldShort(corpID) {
			mul = liftShortMul
		}
	}
	g.value = liftGoalValue(p.poolRef, before, liftPotential(after, book), mul)
}

// liftGoalValue is the price rule, pure for the harness.
func liftGoalValue(ref, before, after, mul float64) float64 {
	if ref <= 0 || after <= before {
		return 0
	}
	gain := 1.0 // from nothing to a hold: the whole step
	if before > 0 {
		gain = (after - before) / before
	}
	return ref * liftLevelWorth * math.Min(gain, liftGainCap) * 10 * mul
}

// corpHoldShort reports whether the corporation has a requisition line
// waiting because no hauler in it can take one unit (posreq_hold.go).
func (p *skillPlanner) corpHoldShort(corpID int64) bool {
	var n int
	_ = p.m.db.QueryRowContext(p.ctx, `
		SELECT COUNT(*) FROM rolebot_corp_requisition
		 WHERE corporationID = ? AND status = 'open' AND note LIKE 'too big to haul%'`, corpID).Scan(&n)
	return n > 0
}
