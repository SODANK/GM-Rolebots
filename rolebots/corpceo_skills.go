package rolebots

import (
	"context"

	evedb "gurimoon/db"
)

// Command skills in the skill planner (goalCommand). Two kinds of pilot want a
// skill no fit asks for:
//   - one that wants to lead a fleet: Leadership sizes the gang it can take out
//     and gates a fleet seat (corprank.go). Its want is Tuning.CommandDrive; it
//     is worth something once the pilot holds a seat or its corporation deals them;
//   - a CEO whose door is full: Corporation Management and the ladder above it
//     are the member limit (evedb.CorpAbilitiesFromSkills), and held applicants
//     (corpceo_hr.go, opening pressure) tell it what to train.
// Both are priced on the planner's pool anchor, like the R&D and lab goals,
// because an opening and a fleet slot are not denominated in ship score.

const (
	// commandLevelWorth is what a level of Leadership is worth to a pilot that wants
	// a fleet as badly as a born leader, on the pool anchor. Modest: it must never
	// outbid a hull the pilot cannot fly yet.
	commandLevelWorth = 0.25
	// commandOpenMul: the FIRST level is the gate on a seat at all, so it is
	// worth more than the levels that only widen a gang.
	commandOpenMul = 2.0
	// ceoOpeningWorth is what one usable extra opening is worth to a CEO holding
	// applicants at the door, on the pool anchor.
	ceoOpeningWorth = 0.05
	// ceoCappedOpeningWorth is what one extra opening is worth to a CEO whose
	// corporation has reached its member limit, on the pool anchor: three times a
	// waited-for opening, because a capped corporation cannot grow any other way.
	ceoCappedOpeningWorth = 0.15

	// The fixed why texts: the skills board classifies old rows by them
	// (espskillboard.go skillKindCase), so they are constants.
	commandWhyLead = "lets it lead one of its corporation's fleets"
	commandWhyCorp = "makes room for the applicants waiting at its door"
	// commandWhyCap: the corporation is at its member limit and nobody is
	// queueing, because a full corporation shuts its own door.
	commandWhyCap    = "lifts its corporation's member limit, which it has reached"
	commandWhyEthnic = "lets it work with, and hire, other peoples"
	commandWhyOrders = "gives its corporation the market orders it has been going without"

	// ethnicLevelWorth is what a level of Ethnic Relations is worth to a devoted
	// xenophile (tolerance.go), on the pool anchor. A CEO pays double: for it the
	// skill is the door playerCorpRefusal shuts on every foreign applicant.
	ethnicLevelWorth = 0.3

	// ceoOrderWorth is what one extra market order slot is worth to a CEO whose
	// corporation's listings and bids go unplaced, on the pool anchor; a little
	// under an opening.
	ceoOrderWorth = 0.04
)

// commandGoal is one goal the command half proposes.
type commandGoal struct {
	typeID int64
	level  int
	why    string
}

// commandGoals is what the command half proposes for this pilot.
func (p *skillPlanner) commandGoals() []commandGoal {
	var out []commandGoal
	// Leadership, one level at a time, for a pilot that wants to lead and has a
	// seat or a corporation that deals them.
	if p.tn.CommandDrive > commandDriveFollower && p.commandSeatValue() > 0 {
		out = append(out, commandGoal{skillLeadership, p.levels[skillLeadership] + 1, commandWhyLead})
	}
	// Ethnic Relations for a xenophile, one level at a time up to what its
	// tolerance asks for (xenoEthnicGoal), early.
	_, isCEO := managedCEOOf(p.charID)
	if want := xenoEthnicGoal(p.tn, isCEO); want > p.levels[skillEthnicRelations] {
		out = append(out, commandGoal{skillEthnicRelations, p.levels[skillEthnicRelations] + 1, commandWhyEthnic})
	}
	// Anchoring, for the CEO and directors of a managed corporation: the
	// officers carry and anchor its structures (posanchor.go).
	if ag, ok := p.anchorGoal(); ok {
		out = append(out, ag)
	}
	// The member-limit ladder, for a managed CEO holding applicants.
	if corpID, ok := managedCEOOf(p.charID); ok && corpWantsOpenings(corpID) {
		why := commandWhyCorp
		if corpOpeningPressure(corpID) <= 0 {
			why = commandWhyCap
		}
		for _, id := range []int64{skillCorpMgmt, skillMegacorp, skillEmpireControl} {
			out = append(out, commandGoal{id, p.levels[id] + 1, why})
		}
	}
	// Order slots, for a managed CEO or a Sales Agent (corpindseats.go) whose
	// corporation could not place orders (corporders.go): Trade, then Retail.
	if corpID, ok := orderPressureCorpOf(p.charID); ok && corpOrderPressure(corpID) > 0 {
		for _, id := range []int64{int64(evedb.SkillTrade), int64(evedb.SkillRetail)} {
			if p.levels[id] < 5 {
				out = append(out, commandGoal{id, p.levels[id] + 1, commandWhyOrders})
			}
		}
	}
	return out
}

// ceoOpeningGoalValue prices a step up the member-limit ladder. Openings somebody
// waits for are worth ceoOpeningWorth each. A corporation at its limit cannot grow
// (applicants never queue at a full door), so each opening is worth
// ceoCappedOpeningWorth, counted up to limitBefore openings.
func ceoOpeningGoalValue(poolRef float64, gain, limitBefore int64, pressure int, capped bool) float64 {
	if gain <= 0 {
		return 0
	}
	useful := gain
	if int64(pressure) < useful {
		useful = int64(max(pressure, 0))
	}
	v := poolRef * ceoOpeningWorth * float64(useful)
	if capped {
		grow := gain
		if limitBefore > 0 && limitBefore < grow {
			grow = limitBefore
		}
		v += poolRef * ceoCappedOpeningWorth * float64(grow)
	}
	return v
}

// Corporation Management's ladder, beside skillCorpMgmt (corpceo_treasury.go).
const (
	skillMegacorp      = int64(3731)
	skillEmpireControl = int64(3732)
)

// openingLadderGoal reports whether a goal is a step up the member-limit ladder.
// Those are exempt from the step and group caps (skills.go plan): Corporation
// Management V is a long step a hurried CEO's cap would discard. Only emitted
// for a CEO whose corporation wants openings.
func openingLadderGoal(g *skillGoal) bool {
	if g.Kind != goalCommand {
		return false
	}
	return isLadderSkill(g.TypeID)
}

// isLadderSkill reports whether a skill raises the member limit. Pure.
func isLadderSkill(typeID int64) bool {
	switch typeID {
	case skillCorpMgmt, skillMegacorp, skillEmpireControl:
		return true
	}
	return false
}

// ladderFirst reports whether this pilot is the CEO of a full managed corporation.
// Its next member-limit rung is committed ahead of anything priced (skills.go plan):
// a full corporation cannot grow any other way.
func (p *skillPlanner) ladderFirst() bool {
	corpID, ok := managedCEOOf(p.charID)
	return ok && corpAtCap(corpID)
}

// firstLadderGoal is the live member-limit rung with the shortest chain, or nil. Pure.
func firstLadderGoal(live []*skillGoal) *skillGoal {
	var best *skillGoal
	for _, g := range live {
		if openingLadderGoal(g) && (best == nil || g.minutes < best.minutes) {
			best = g
		}
	}
	return best
}

// ladderPreemptSec: a full corporation's CEO sets aside a training with more than
// this left for the next member-limit rung.
const ladderPreemptSec = 24 * 3600

// ladderPreempts decides whether the active training gives way to the plan's head.
// headTypeID is the first applied step, trainingTypeID the active training (0 for
// none), leftSec what that training has to go. Pure.
func ladderPreempts(capped bool, headTypeID, trainingTypeID, leftSec int64) bool {
	return capped && isLadderSkill(headTypeID) && trainingTypeID != 0 &&
		!isLadderSkill(trainingTypeID) && leftSec > ladderPreemptSec
}

// preemptForLadder stops a full corporation's CEO's long training when the plan just
// queued opens with a member-limit rung, and starts the queue. The SP trained so far
// is banked (evedb.StopTrainingSkill); the next plan picks the skill up again.
// Switch: rolebots.corp.ladderPreempt.
func (m *Mod) preemptForLadder(ctx context.Context, charID int64, applied []skillStep) {
	if len(applied) == 0 || !ladderPreemptOn() {
		return
	}
	corpID, ok := managedCEOOf(charID)
	if !ok || !corpAtCap(corpID) {
		return
	}
	st, err := evedb.GetSkillTraining(ctx, m.db, charID)
	if err != nil || st == nil {
		return
	}
	left := (int64(st.EndOfTraining) - win32Now()) / 10_000_000
	if !ladderPreempts(true, applied[0].TypeID, st.TypeID, left) {
		return
	}
	if _, err := evedb.StopTrainingSkill(ctx, m.db, charID, st.SkillItemID); err != nil {
		m.logBot(charID, logError, "skills", "could not set training aside for %s: %v", applied[0].Name, err)
		return
	}
	forgetCharSkills(charID)
	if _, err := evedb.AdvanceSkillQueue(ctx, m.db, charID); err != nil {
		m.logBot(charID, logError, "skills", "could not start %s: %v", applied[0].Name, err)
		return
	}
	m.logBot(charID, logInfo, "skills",
		"set its training aside with %s to go — its corporation is full, so %s comes first",
		fmtDuration(left), applied[0].Name)
}

// commandSeatValue is how much a fleet seat is within reach: 1 for a pilot that
// holds one, 1/2 for one in a corporation that deals seats, 0 otherwise. Read
// off the corp-rank table once per plan.
func (p *skillPlanner) commandSeatValue() float64 {
	if p.cmdSeat != nil {
		return *p.cmdSeat
	}
	v := 0.0
	if p.m == nil || p.m.db == nil || p.ctx == nil {
		// A planner built for arithmetic alone (the harness) has no roster
		// to read a seat from.
		p.cmdSeat = &v
		return v
	}
	var seats, corpID int64
	_ = p.m.db.QueryRowContext(p.ctx,
		`SELECT COUNT(*) FROM rolebot_corp_ranks WHERE characterID = ?`, p.charID).Scan(&seats)
	if seats > 0 {
		v = 1
	} else if err := p.m.db.QueryRowContext(p.ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, p.charID).Scan(&corpID); err == nil {
		if _, managed := managedCEO(corpID); managed {
			v = 0.5
		} else if npc, err := isNPCCorp(p.ctx, p.m.db, corpID); err == nil && npc {
			v = 0.5
		}
	}
	p.cmdSeat = &v
	return v
}

// evaluateCommand prices a command goal.
func (p *skillPlanner) evaluateCommand(g *skillGoal) {
	g.valued = true
	g.value = 0
	switch g.TypeID {
	case skillAnchoring:
		p.evaluateAnchoring(g)
	case skillLeadership:
		want := (p.tn.CommandDrive - commandDriveFollower) / (commandDriveLeader - commandDriveFollower)
		if want <= 0 {
			return
		}
		v := p.poolRef * commandLevelWorth * want * p.commandSeatValue()
		if p.levels[g.TypeID] == 0 {
			v *= commandOpenMul
		}
		g.value = v
	case skillCorpMgmt, skillMegacorp, skillEmpireControl:
		corpID, ok := managedCEOOf(p.charID)
		if !ok {
			return
		}
		pressure := corpOpeningPressure(corpID)
		capped := corpAtCap(corpID)
		if pressure <= 0 && !capped {
			return
		}
		before := evedb.CorpMgmtSkills{
			CorpMgmt:      p.levels[skillCorpMgmt],
			Megacorp:      p.levels[skillMegacorp],
			EmpireControl: p.levels[skillEmpireControl],
		}
		after := before
		// Level V of a rung is worth the first level of the rung it opens as well as its
		// own: Corporation Management V adds 10 openings and is the door to Megacorp's +50.
		switch g.TypeID {
		case skillCorpMgmt:
			after.CorpMgmt = g.Level
			if g.Level >= 5 {
				after.Megacorp = max(after.Megacorp, 1)
			}
		case skillMegacorp:
			after.Megacorp = g.Level
			if g.Level >= 5 {
				after.EmpireControl = max(after.EmpireControl, 1)
			}
		case skillEmpireControl:
			after.EmpireControl = g.Level
		}
		limitBefore, _ := evedb.CorpAbilitiesFromSkills(before, 0)
		limitAfter, _ := evedb.CorpAbilitiesFromSkills(after, 0)
		g.value = ceoOpeningGoalValue(p.poolRef, limitAfter-limitBefore, limitBefore, pressure, capped)
	case int64(evedb.SkillTrade), int64(evedb.SkillRetail):
		corpID, ok := orderPressureCorpOf(p.charID)
		if !ok {
			return
		}
		pressure := corpOrderPressure(corpID)
		if pressure <= 0 {
			return
		}
		gain := 4 // Trade: four orders a level
		if g.TypeID == int64(evedb.SkillRetail) {
			gain = 8
		}
		g.value = p.poolRef * ceoOrderWorth * float64(min(gain, pressure))
	case skillEthnicRelations:
		_, isCEO := managedCEOOf(p.charID)
		if xenoEthnicGoal(p.tn, isCEO) < g.Level {
			return
		}
		v := p.poolRef * ethnicLevelWorth * p.tn.Xenophilia
		if isCEO {
			v *= 2
		}
		if p.levels[g.TypeID] == 0 {
			v *= commandOpenMul
		}
		g.value = v
	}
}
