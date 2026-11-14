package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"

	evedb "gurimoon/db"
	"gurimoon/dogma"
)

// Implant wants: what one implant is worth to one pilot. Pure.
// implantValue scores a catalogue entry against an implantPilot: job weights (jobWeights),
// trained weapon families and tank, the learning attributes its skill queue leans on, and what
// is plugged. Scores share one unit, so one ranking covers enhancers, hardwirings and set pieces.
// The gain of plugging is the score less the score of the implant in that slot.
//
// Sets complete themselves: a piece scores times (1 + 2h) for h pieces held, the Omega is worth
// what it multiplies (nothing alone), and a piece of the other grade is worth nothing while any
// of this grade is held. Budget: a pod destroys every implant, so a pilot spends a fraction of
// its spendable money divided by its recent pod deaths.

// implantPilot is everything the scorer knows about a pilot.
type implantPilot struct {
	// Jobs is jobWeights(tn): how much each job pulls, 0 or more.
	Jobs map[string]float64
	// Weapons is trained level / 5 per turret family and launcher skill: the weapon kinds
	// the pilot can use, by how well.
	Weapons map[int64]float64
	// TankArmor / TankShield is trained Hull Upgrades / Shield Management
	// level over 5: which tank the pilot flies.
	TankArmor, TankShield float64
	// AttrDemand ranks the learning attributes 164-168 by how much training
	// the queue has riding on each: 1 for the busiest, down to 0.
	AttrDemand map[int64]float64
	// Held is what is plugged, by slot.
	Held map[int64]*implantType
}

// implantScore is one implant's value to the pilot, with its reasoning.
type implantScore struct {
	Type  *implantType
	Score float64 // worth to this pilot, 0 = nothing
	Gain  float64 // Score less what is in the slot now
	Why   string
}

// Scoring units. One point is roughly "one percent on a stat this pilot
// lives by"; an attribute point is worth a little under a percent-point
// because it speeds every skill rather than one fight.
const (
	implantAttrUnit  = 0.6
	implantChanUnit  = 1.0
	implantSetBoost  = 2.0 // per piece already held of the same set and grade
	implantMinWeight = 0.02
)

// jobMax is the larger of two job pulls.
func jobMax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// channelWeight is how much this pilot cares about a channel (0 and up) before the implant's
// percent is applied. Weapon channels are scoped by the implant's required-skill family: an
// all-turret line reads as the pilot's best turret family, a sized one as that family alone.
func (p implantPilot) channelWeight(e dogma.ImplantEffect) (float64, string) {
	combat := p.Jobs[JobCombat] + 0.5*p.Jobs[JobTackle] + 0.5*p.Jobs[JobEwar] + 0.5*p.Jobs[JobScout]
	weaponTerm := func() (float64, string) {
		if e.ReqSkill != 0 {
			return p.Weapons[e.ReqSkill], turretFamilyName(e.ReqSkill)
		}
		best := 0.0
		for _, s := range []int64{
			dogma.SkillSmallHybridTurret, dogma.SkillMediumHybridTurret, dogma.SkillLargeHybridTurret,
			dogma.SkillSmallProjectileTurret, dogma.SkillMediumProjectileTurret, dogma.SkillLargeProjectileTurret,
			dogma.SkillSmallEnergyTurret, dogma.SkillMediumEnergyTurret, dogma.SkillLargeEnergyTurret,
		} {
			if v := p.Weapons[s]; v > best {
				best = v
			}
		}
		return best, "its turrets"
	}
	switch e.Channel {
	case dogma.ICTurretDamage, dogma.ICTurretCycle:
		w, who := weaponTerm()
		return combat * w, "damage for " + who
	case dogma.ICTurretTracking, dogma.ICTurretOptimal, dogma.ICTurretFalloff:
		w, who := weaponTerm()
		return combat * w * 0.7, "range/tracking for " + who
	case dogma.ICTurretCapNeed:
		w, who := weaponTerm()
		return combat * w * 0.4, "cheaper cycles for " + who
	case dogma.ICLauncherCycle:
		return combat * p.Weapons[skillMissileLauncherOp], "its launchers"
	case dogma.ICMissileVelocity, dogma.ICMissileFlightTime:
		return combat * p.Weapons[skillMissileLauncherOp] * 0.7, "missile range"
	case dogma.ICArmorHP:
		return combat * p.TankArmor, "an armour tank"
	case dogma.ICShieldHP, dogma.ICShieldRecharge:
		return combat * p.TankShield, "a shield tank"
	case dogma.ICHullHP:
		return combat * 0.3, "hull"
	case dogma.ICShieldBoost:
		return combat * p.TankShield * 0.8, "shield boosting"
	case dogma.ICArmorRepAmount, dogma.ICRepairCycle:
		return combat * p.TankArmor * 0.8, "armour repair"
	case dogma.ICCapCapacity, dogma.ICCapRecharge:
		return combat * 0.5, "capacitor"
	case dogma.ICVelocity:
		return jobMax(combat, p.Jobs[JobTrade]) * 0.6, "speed"
	case dogma.ICAgility:
		return jobMax(combat, p.Jobs[JobTrade]) * 0.4, "agility"
	case dogma.ICSigRadius:
		return combat * 0.6, "a smaller signature"
	case dogma.ICTargetRange:
		return combat * 0.3, "lock range"
	case dogma.ICMiningYield, dogma.ICIceCycle:
		return p.Jobs[JobMining], "mining"
	case dogma.ICMiningDroneYield:
		return p.Jobs[JobMining] * 0.5, "mining drones"
	case dogma.ICNosDuration:
		return combat * 0.3, "capacitor warfare"
	case dogma.ICRemoteRepCapNeed:
		return p.Jobs[JobLogi], "logistics"
	}
	// Tag-only channels do nothing on this server; a bot pays nothing for
	// them and sells them on.
	return 0, ""
}

// implantValue scores one implant for a pilot. Pure.
func implantValue(t *implantType, p implantPilot) implantScore {
	out := implantScore{Type: t}
	if t == nil {
		return out
	}
	tag := t.Tag
	if tag.Dummy || tag.Mindlink {
		out.Why = "does nothing for it"
		return out
	}
	var why []string
	// Attribute bonus (attribute implants and set pieces both carry one).
	if id, b := tag.AttrID(); id != 0 {
		if d := p.AttrDemand[id]; d > 0 {
			out.Score += implantAttrUnit * b * d
			why = append(why, fmt.Sprintf("+%.0f %s its queue leans on", b, learningAttrName(id)))
		}
	}
	// Set pieces: the stat line scaled by the set's standing.
	if tag.Set != dogma.ISNone {
		held, heldOther := p.setStanding(tag.Set, tag.Grade)
		if heldOther > 0 && held == 0 {
			out.Score = 0
			out.Why = fmt.Sprintf("wrong grade — it is collecting the %s %s set", p.setGradeHeld(tag.Set), tag.Set)
			return out
		}
		boost := 1 + implantSetBoost*float64(held)
		if tag.Omega {
			// Worth what it multiplies: the held pieces' lines times the
			// Omega's share of the factor. Nothing alone.
			sum := 0.0
			for _, h := range p.Held {
				if h.Tag.Set == tag.Set && h.Tag.Grade == tag.Grade && !h.Tag.Omega && len(h.Tag.Effects) > 0 {
					w, _ := p.channelWeight(h.Tag.Effects[0])
					sum += implantChanUnit * math.Abs(h.Tag.Effects[0].Pct) * w
				}
			}
			out.Score += sum * (tag.SetMult - 1) * boost
			if held > 0 {
				why = append(why, fmt.Sprintf("completes %d held %s piece(s)", held, tag.Set))
			} else {
				why = append(why, "an Omega with no set to multiply")
			}
		} else if len(tag.Effects) > 0 {
			e := tag.Effects[0]
			w, who := p.channelWeight(e)
			out.Score += implantChanUnit * math.Abs(e.Pct) * w * boost
			if held > 0 {
				why = append(why, fmt.Sprintf("%s set %d/%d held — collecting it", tag.Set, held, dogma.ImplantSetPieces))
			} else if w > implantMinWeight {
				why = append(why, fmt.Sprintf("%+.0f%% %s for %s", e.Pct, e.Channel, who))
			}
		}
	} else {
		for _, e := range tag.Effects {
			w, who := p.channelWeight(e)
			if w <= implantMinWeight {
				continue
			}
			out.Score += implantChanUnit * math.Abs(e.Pct) * w
			why = append(why, fmt.Sprintf("%+.0f%% %s for %s", e.Pct, e.Channel, who))
		}
	}
	if out.Score <= 0 {
		out.Why = "nothing it does is anything this pilot uses"
		return out
	}
	out.Gain = out.Score
	if cur := p.Held[tag.Slot]; cur != nil {
		if cur.TypeID == t.TypeID {
			out.Gain = 0
			out.Why = "already plugged in"
			return out
		}
		// A set piece being collected is displaced only by its own set, so the next learning
		// implant cannot break up a half-built set.
		if cur.Tag.Set != dogma.ISNone && (cur.Tag.Set != tag.Set || cur.Tag.Grade != tag.Grade) {
			out.Gain = 0
			out.Why = fmt.Sprintf("slot %d holds a %s piece it is collecting", tag.Slot, cur.Tag.Set)
			return out
		}
		curScore := implantValue(cur, implantPilot{Jobs: p.Jobs, Weapons: p.Weapons,
			TankArmor: p.TankArmor, TankShield: p.TankShield, AttrDemand: p.AttrDemand,
			Held: p.heldWithout(tag.Slot)}).Score
		out.Gain = out.Score - curScore
		if out.Gain <= 0 {
			why = append(why, fmt.Sprintf("but slot %d already holds %s", tag.Slot, cur.Name))
		}
	}
	out.Why = joinWhy(why)
	return out
}

func joinWhy(parts []string) string {
	s := ""
	for i, p := range parts {
		if i > 0 {
			s += "; "
		}
		s += p
	}
	return s
}

// setStanding counts the pilot's held pieces of a set at this grade, and at
// the other grade.
func (p implantPilot) setStanding(set dogma.ImplantSet, grade dogma.ImplantGrade) (same, other int) {
	for _, h := range p.Held {
		if h.Tag.Set != set {
			continue
		}
		if h.Tag.Grade == grade {
			same++
		} else {
			other++
		}
	}
	return same, other
}

func (p implantPilot) setGradeHeld(set dogma.ImplantSet) dogma.ImplantGrade {
	for _, h := range p.Held {
		if h.Tag.Set == set {
			return h.Tag.Grade
		}
	}
	return dogma.IGNone
}

func (p implantPilot) heldWithout(slot int64) map[int64]*implantType {
	out := make(map[int64]*implantType, len(p.Held))
	for s, h := range p.Held {
		if s != slot {
			out[s] = h
		}
	}
	return out
}

// rankImplants scores every catalogue entry and returns the ones with a
// positive gain, best first. Pure.
func rankImplants(cat []*implantType, p implantPilot) []implantScore {
	var out []implantScore
	for _, t := range cat {
		if t.Slot() == 0 || t.Slot() > 10 {
			continue // the Aurora dev set and the slot-11 veterans
		}
		s := implantValue(t, p)
		if s.Gain > 0 {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Gain != out[j].Gain {
			return out[i].Gain > out[j].Gain
		}
		return out[i].Type.TypeID < out[j].Type.TypeID
	})
	return out
}

// implantBudget is what a pilot will put in its head: a fraction of what
// its wallet may commit, divided by the pod deaths it has suffered lately.
// Pure.
func implantBudget(balance float64, fracPct int64, podDeaths int) float64 {
	if fracPct <= 0 {
		return 0
	}
	return spendable(balance) * float64(fracPct) / 100 / float64(1+podDeaths)
}

// implantAskCap is the most a pilot pays for one implant: its budget, and never more than
// twice the catalogue price, since loot has no floor but basePrice.
func implantAskCap(budget, basePrice float64) float64 {
	cap := budget
	if basePrice > 0 && basePrice*2 < cap {
		cap = basePrice * 2
	}
	return cap
}

// ── building the pilot ──────────────────────────────────────────────────────

var turretFamilySkills = []int64{
	dogma.SkillSmallHybridTurret, dogma.SkillMediumHybridTurret, dogma.SkillLargeHybridTurret,
	dogma.SkillSmallProjectileTurret, dogma.SkillMediumProjectileTurret, dogma.SkillLargeProjectileTurret,
	dogma.SkillSmallEnergyTurret, dogma.SkillMediumEnergyTurret, dogma.SkillLargeEnergyTurret,
}

// implantPilotFor reads what the scorer needs about one pilot: its job
// weights from its personality, its trained weapon and tank skills, the
// attributes its skill queue leans on, and what is already plugged.
func (m *Mod) implantPilotFor(ctx context.Context, charID int64, tn Tuning) (implantPilot, error) {
	p := implantPilot{
		Jobs:       jobWeights(tn),
		Weapons:    map[int64]float64{},
		AttrDemand: map[int64]float64{},
	}
	skills, err := m.charSkillsFor(ctx, charID)
	if err != nil {
		return p, err
	}
	for _, s := range turretFamilySkills {
		if lvl := skills[s]; lvl > 0 {
			p.Weapons[s] = float64(lvl) / 5
		}
	}
	if lvl := skills[skillMissileLauncherOp]; lvl > 0 {
		p.Weapons[skillMissileLauncherOp] = float64(lvl) / 5
	}
	p.TankArmor = float64(skills[int64(dogma.SkillHullUpgrades)]) / 5
	p.TankShield = float64(skills[int64(dogma.SkillShieldManagement)]) / 5
	// An untrained pilot still has a tank: whichever it has not chosen yet
	// reads as half, so a fresh hunter values plating and shield alike.
	if p.TankArmor == 0 && p.TankShield == 0 {
		p.TankArmor, p.TankShield = 0.5, 0.5
	}
	p.AttrDemand = m.implantAttrDemand(ctx, charID)
	held, err := m.pluggedImplants(ctx, charID)
	if err != nil {
		return p, err
	}
	p.Held = map[int64]*implantType{}
	for slot, h := range held {
		p.Held[slot] = h.Type
	}
	return p, nil
}

// implantAttrDemand ranks the five learning attributes by the training the pilot's queue has
// riding on each (primary in full, secondary half, weighted by skill rank). The busiest reads 1.
// An empty queue gives a flat 0.5, so an attribute implant is still worth something to it.
func (m *Mod) implantAttrDemand(ctx context.Context, charID int64) map[int64]float64 {
	demand := map[int64]float64{}
	add := func(typeID int64, weight float64) {
		attrs, err := evedb.TypeAttrs(ctx, m.db, typeID)
		if err != nil {
			return
		}
		rank := attrs[275]
		if rank <= 0 {
			rank = 1
		}
		if pa := int64(attrs[180]); pa >= 164 && pa <= 168 {
			demand[pa] += weight * rank
		}
		if sa := int64(attrs[181]); sa >= 164 && sa <= 168 {
			demand[sa] += weight * rank / 2
		}
	}
	if st, err := evedb.GetSkillTraining(ctx, m.db, charID); err == nil && st != nil {
		add(st.TypeID, 1)
	}
	if q, err := evedb.GetSkillQueue(ctx, m.db, charID); err == nil {
		for _, e := range q {
			add(e.TypeID, 1)
		}
	}
	out := map[int64]float64{}
	best := 0.0
	for _, v := range demand {
		if v > best {
			best = v
		}
	}
	if best <= 0 {
		for id := int64(164); id <= 168; id++ {
			out[id] = 0.5
		}
		return out
	}
	for id, v := range demand {
		out[id] = v / best
	}
	return out
}

// implantPodDeaths counts the pilot's pod losses in the last 14 days, the
// number its implant budget is divided by.
func (m *Mod) implantPodDeaths(ctx context.Context, charID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM killmails
		WHERE victimCharID = ? AND isPod = 1 AND killTime >= ?`,
		charID, clock.Unix()-14*86400).Scan(&n)
	return n
}
