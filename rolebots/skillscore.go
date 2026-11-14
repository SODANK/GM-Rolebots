package rolebots

import (
	"context"
	"fmt"
	"sync"

	evedb "gurimoon/db"
)

// What a training step is worth: the arithmetic half of skills.go.
//
// There is no table of good skills. A skill is worth what it does to the ships
// this pilot would fly: a fit resolved against a skill sheet sheds the lines the
// sheet cannot online (fiteffective.go), so
//
//	value(step) = sum over fits F of weight(job of F) * [score(F | skills+step) - score(F | skills)]
//
// Job weights come from the appetite catalog. Learning skills touch no fit, so
// they are valued in minutes saved across foreseen work (skills.go).

// ── learning skills, derived ────────────────────────────────────────────────

const (
	// learningSkillTypeID is the Learning skill itself, the one learning skill that is
	// not a flat bonus: GetEffectiveCharAttr applies it as a multiplier over
	// everything else, so it is predicted differently.
	learningSkillTypeID = 3374
	// learningMultPerLevel mirrors the +2%/level in GetEffectiveCharAttr.
	learningMultPerLevel = 0.02
	// The five base learning attributes (charisma to willpower) and the offset to
	// their *Bonus counterparts, from the core's effective-attribute query.
	attrLearnFirst = int64(164)
	attrLearnLast  = int64(168)
	attrBonusShift = int64(11)
	// learningRelevantAttrs is how many of the five attributes a pilot bothers
	// raising: the primary and secondary its foreseen training runs on
	// (relevantLearningAttrs, skills.go).
	learningRelevantAttrs = 2
)

// learningBonus is one attribute-raising skill: which stat it lifts and by how
// much per level. Derived from static data so the module never disagrees with
// the server.
type learningBonus struct {
	TypeID   int64
	AttrID   int64 // 164..168, the BASE attribute this skill lifts
	PerLevel float64
}

var (
	learnOnce  sync.Once
	learnTable map[int64]learningBonus
	learnErr   error
)

// learningSkills maps skill typeID to the attribute it raises. Static data, loaded
// once. It mirrors GetEffectiveCharAttr: a skill carries the bonus for attribute N
// as attribute N+11, so every dgmTypeAttributes row with an attributeID in the
// bonus band belongs to a learning skill.
func (m *Mod) learningSkills(ctx context.Context) (map[int64]learningBonus, error) {
	learnOnce.Do(func() {
		out := map[int64]learningBonus{}
		rows, err := m.db.QueryContext(ctx, `
			SELECT ta.typeID, ta.attributeID - ?,
			       COALESCE(NULLIF(ta.valueFloat, 0), ta.valueInt, 0)
			FROM dgmTypeAttributes ta
			JOIN invTypes t ON t.typeID = ta.typeID
			JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 16
			WHERE t.published = 1 AND ta.attributeID BETWEEN ? AND ?`,
			attrBonusShift, attrLearnFirst+attrBonusShift, attrLearnLast+attrBonusShift)
		if err != nil {
			learnErr = fmt.Errorf("learningSkills: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var lb learningBonus
			if err := rows.Scan(&lb.TypeID, &lb.AttrID, &lb.PerLevel); err != nil {
				learnErr = fmt.Errorf("learningSkills scan: %w", err)
				return
			}
			if lb.PerLevel <= 0 {
				continue
			}
			out[lb.TypeID] = lb
		}
		if learnErr = rows.Err(); learnErr != nil {
			return
		}
		// The Learning skill carries no *Bonus attribute (it is the multiplier), so it is
		// added by ID. AttrID 0 marks it.
		out[learningSkillTypeID] = learningBonus{TypeID: learningSkillTypeID}
		learnTable = out
	})
	return learnTable, learnErr
}

// predictStats returns the effective learning attributes the pilot would have
// with one learning skill moved from from to to. It inverts the core's
// composition, (base + flat bonuses) * the Learning multiplier, so a flat bonus
// lands inside the multiplier; an error here makes the planner over-value learning.
func predictStats(stats map[int64]float64, lb learningBonus, learnLevel, from, to int) map[int64]float64 {
	out := make(map[int64]float64, len(stats))
	for k, v := range stats {
		out[k] = v
	}
	if to == from {
		return out
	}
	if lb.TypeID == learningSkillTypeID {
		// The multiplier moved: every attribute rescales together.
		oldMul := 1 + learningMultPerLevel*float64(from)
		newMul := 1 + learningMultPerLevel*float64(to)
		if oldMul <= 0 {
			return out
		}
		for k, v := range out {
			out[k] = v / oldMul * newMul
		}
		return out
	}
	// A flat bonus, applied inside the Learning multiplier.
	mul := 1 + learningMultPerLevel*float64(learnLevel)
	out[lb.AttrID] += lb.PerLevel * float64(to-from) * mul
	return out
}

// ── step timing ─────────────────────────────────────────────────────────────

// stepMinutes is how long one level of one skill takes at a given training rate.
// Banked SP only counts for the level immediately above the trained one. It must
// stay the same rule as PlanMinMaxSkillQueue's: the two plans are read side by
// side on the skill window.
func stepMinutes(rank int64, bankedSP float64, level int, rate float64) float64 {
	if rate <= 0 {
		return 0
	}
	start := bankedSP
	if prev := evedb.ESPSkillSPForLevel(rank, level-1); prev > start {
		start = prev
	}
	need := evedb.ESPSkillSPForLevel(rank, level) - start
	if need < 1 {
		need = 1
	}
	return need / rate
}

// ── the pool, judged against a sheet the pilot does not have ────────────────

// fitBase is one (fit, job) pair in the running, with the score it holds under
// the pilot's current skills. The requirement set lets a candidate find the
// pairs it could move without re-judging the pool.
type fitBase struct {
	stored *fitTemplate
	job    string
	weight float64
	reqs   map[int64]int // skillID to the level this pair asks for
	score  float64
}

// jobWeights is what each job is worth to this pilot, summed from the appetite
// catalog, so a new activity arrives already weighted. Satiation is left out: it
// is the anti-monotony clock on what to do this afternoon (appetite.go), and a
// skill plan runs for days.
func jobWeights(tn Tuning) map[string]float64 {
	w := map[string]float64{}
	for _, d := range activityCatalog {
		w[d.Job] += d.Disposition(tn)
	}
	// Ore has to be carried home: half the mining appetite counts as hauling, so a
	// miner's plan includes an industrial. Skill-planner only; what a bot does is unchanged.
	w[JobTrade] += miningHaulShare * w[JobMining]
	// Specialist seats, planner-only: no appetite draws a pilot into a seat, but a
	// pilot that never trains for one cannot be seated. See docs/comment-notes-b17.md#seat-weights.
	w[JobLogi] += tn.LogiDrive
	w[JobTackle] += tackleSeatShare * w[JobCombat] * tn.TackleDrive
	w[JobEwar] += ewarSeatShare * w[JobCombat] * tn.EwarDrive
	// The gank scout, from the greedy pilot's combat appetite: it is the seat
	// that goes home with the loot. Zero at zero traits like the others.
	w[JobScout] += scoutSeatShare * w[JobCombat] * tn.ScoutDrive
	return w
}

// scoutSeatShare is the share of a combat appetite the scout seat borrows
// for the skill planner. Small: the seat trains Electronics and a stabiliser's
// skill, both cheap, and nothing a combat pilot does not want anyway.
const scoutSeatShare = 0.15

// miningHaulShare is how much of a miner's appetite counts as hauling.
const miningHaulShare = 0.5

// tackleSeatShare / ewarSeatShare are how much of a temperament-matched
// gunner's combat appetite backs the seat's skills. Half, as miningHaulShare:
// the seat is the second ship, never the first.
const (
	tackleSeatShare = 0.5
	ewarSeatShare   = 0.5
)

// judgeHypothetical scores one fit for one job against a skill sheet that may not
// be the pilot's real one. It cannot go through judgeFit: fitPower caches by (fit,
// pilot, job), and a hypothetical sheet would write an answer the sim then
// believes. The rest is judgeFit's own arithmetic.
func (m *Mod) judgeHypothetical(ctx context.Context, charID int64, job string,
	stored *fitTemplate, skills map[int64]int, raceID int64) (float64, error) {

	eff, err := m.effectiveFitWithSkills(ctx, charID, job, stored, skills)
	if err != nil {
		return 0, err
	}
	if eff == nil {
		return 0, nil // cannot board the hull on this sheet
	}
	s, err := m.ComputeFitStats(ctx, eff.Fit)
	if err != nil {
		return 0, err
	}
	power := jobPower(job, s)
	if power <= 0 {
		return 0, nil
	}
	reqs, err := m.fitSkillRequirements(ctx, eff)
	if err != nil {
		return 0, err
	}
	score := power * (1 + skillAffinityWeight*skillAffinity(reqs, skills))
	if race := m.hullRace(ctx, eff.Fit.HullTypeID); race != 0 && race == raceID {
		score *= 1 + raceAffinityBonus
	}
	return score, nil
}

// ── industry ────────────────────────────────────────────────────────────────
//
// A factory is not a ship, so the fit pool scores its skills at zero. Industry gets
// its own throughput reading from the server's formulas, three terms multiplied:
//
//	runs per hour   Industry, -4% job time per level (industry_ram.go)
//	minerals kept   Production Efficiency, the 0.25 - 0.05*PE waste term in CalcMaterialsNeeded
//	ore recovered   Refining x Refinery Efficiency x the ore-specific skill (refineStack)
//
// Mass Production is not modelled. See docs/comment-notes-b17.md#mass-production.

const (
	// industryTimePerLevel is Industry's −4% job time per level.
	industryTimePerLevel = 0.04
	// industryWasteBase / industryWastePerLevel are the PE waste term out of
	// CalcMaterialsNeeded: an untrained pilot pays 25% extra materials, and
	// each level takes 5 points off that.
	industryWasteBase     = 0.25
	industryWastePerLevel = 0.05
	// The refining yield expression from refineStack. skillTerm is
	// 0.375 × (1+0.02·Refining) × (1+0.04·RefineryEfficiency), and the
	// ore-specific skill folds in before the 100% cap.
	refineSkillBase      = 0.375
	refinePerRefining    = 0.02
	refinePerRefineryEff = 0.04
	refinePerOreSkill    = 0.05
	// refineStationEffDefault stands in when the pilot has no station to read an
	// efficiency off: the common NPC refinery figure.
	refineStationEffDefault = 0.5
	// miningRefineShare is how much of the mining disposition backs the refinery
	// levers (skills.go, wRefine): refining is the miner's second trade.
	miningRefineShare = 0.4
)

// industryLevers is the throughput model's input: the skills it reads and the
// station it reads them against.
type industryLevers struct {
	StationEff float64
	OreSkillID int64 // the ore this pilot actually mines; 0 = none known
}

// industryPower is runs per hour x minerals kept per run x ore recovered per unit
// mined. Only the ratio of two readings is used, so the scale carries no unit.
func industryPower(levels map[int64]int, lv industryLevers) float64 {
	timeMul := 1 - industryTimePerLevel*float64(levels[skillIndustry])
	if timeMul < 0.05 {
		timeMul = 0.05
	}
	runsPerHour := 1 / timeMul

	waste := industryWasteBase - industryWastePerLevel*float64(levels[skillProductionEff])
	if waste < 0 {
		waste = 0
	}
	keep := 1 / (1 + waste)

	skillTerm := refineSkillBase *
		(1 + refinePerRefining*float64(levels[skillIndRefining])) *
		(1 + refinePerRefineryEff*float64(levels[skillIndRefineryEf]))
	if lv.OreSkillID != 0 {
		skillTerm *= 1 + refinePerOreSkill*float64(levels[lv.OreSkillID])
	}
	yield := lv.StationEff + skillTerm
	if yield > 1 {
		yield = 1
	}
	if yield <= 0 {
		yield = 0.01
	}
	return runsPerHour * keep * yield
}

// fitRequirementLevels is the requirement set of a fit as authored (hull,
// modules, charges and drones), keyed skill to hardest level asked. Authored, not
// effective: the effective fit has the lines this pilot cannot use taken off, so
// it would answer none you do not already have.
func (m *Mod) fitRequirementLevels(ctx context.Context, t *fitTemplate) (map[int64]int, error) {
	out := map[int64]int{}
	add := func(typeID int64) error {
		if typeID == 0 {
			return nil
		}
		reqs, err := m.requiredSkills(ctx, typeID)
		if err != nil {
			return err
		}
		for _, r := range reqs {
			if r.Level > out[r.SkillID] {
				out[r.SkillID] = r.Level
			}
		}
		return nil
	}
	if err := add(t.Fit.HullTypeID); err != nil {
		return nil, err
	}
	for _, mod := range t.Fit.Modules {
		if err := add(mod.TypeID); err != nil {
			return nil, err
		}
		if err := add(mod.ChargeTypeID); err != nil {
			return nil, err
		}
	}
	for _, d := range t.Fit.Drones {
		if err := add(d.TypeID); err != nil {
			return nil, err
		}
	}
	for _, c := range t.Fit.Cargo {
		if err := add(c.TypeID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
