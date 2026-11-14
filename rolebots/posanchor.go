package rolebots

import (
	"context"
	"database/sql"

	"gurimoon/corp"
)

// Anchoring: whose job it is to put a corporation's structures in space.
// No tower or structure in the game data requires the skill, so the core anchors for anybody.
// The module gives the work to officers: the CEO, Directors and every member of the
// Infrastructure arm (infraRigger). They train Anchoring (corpceo_skills.go) and own the last
// hop of the haul board (poshaul.go), the leg that ends in anchoring:
//   - a deploy leg (the tower itself) wants Anchoring I;
//   - a rig leg (guns, arrays, harvesters, silos around an online tower) wants Anchoring II.
// For anchorReserveSec after posting only an officer with that level may claim the leg, until
// anchorOfficerSec any officer, then anybody. Hauling the kit to the staging office
// (postStagingMoves) stays everybody's work.

const (
	skillAnchoring = int64(11584)

	anchorDeployLevel = 1
	anchorRigLevel    = 2
	anchorGoalCap     = anchorRigLevel

	anchorReserveSec = 24 * 3600
	anchorOfficerSec = 72 * 3600

	// anchorLevelWorth is a level of Anchoring to an officer of a corporation running
	// starbases, on the planner's pool anchor: about a level of Leadership to a born leader.
	// The first level gates the tower itself and is worth anchorOpenMul more.
	anchorLevelWorth = 0.25
	anchorOpenMul    = 2.0
	// anchorIdleMul prices the skill for an officer of a corporation that
	// has no tower, survey or kit on the boards yet: still worth learning
	// before the first tower is bought, at a fraction.
	anchorIdleMul = 0.4

	commandWhyAnchor = "anchors its corporation's starbase structures"
)

// anchorNeedFor is the Anchoring level a haul leg's kind asks of the officer
// who carries it, 0 for a leg that anchors nothing.
func anchorNeedFor(kind string) int {
	switch kind {
	case haulKindDeploy:
		return anchorDeployLevel
	case haulKindRig:
		return anchorRigLevel
	}
	return 0
}

// isCorpOfficer reports whether charID is its corporation's CEO or holds the
// Director role in it.
func (m *Mod) isCorpOfficer(ctx context.Context, corpID, charID int64) bool {
	var ceo int64
	var roles sql.NullInt64
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(c.ceoID, 0), r.corprole
		  FROM corporation c
		  LEFT JOIN chrCorporationRoles r ON r.characterID = ?
		 WHERE c.corporationID = ?`, charID, corpID).Scan(&ceo, &roles)
	if err != nil {
		return false
	}
	return ceo == charID || (roles.Valid && uint64(roles.Int64)&corp.RoleDirector != 0)
}

// anchorClaimOK is the deploy/rig claim rule: whether this pilot may take a
// leg of this kind posted `age` seconds ago. Pure past the two lookups, so
// the harness pins the windows.
func anchorClaimOK(need int, officer bool, anchoring int, age int64) bool {
	switch {
	case need <= 0 || age >= anchorOfficerSec:
		return true
	case age >= anchorReserveSec:
		return officer
	default:
		return officer && anchoring >= need
	}
}

// mayClaimAnchorLeg applies anchorClaimOK to a live pilot.
func (m *Mod) mayClaimAnchorLeg(ctx context.Context, b *botRun, kind string, age int64) bool {
	need := anchorNeedFor(kind)
	if need == 0 || age >= anchorOfficerSec {
		return true
	}
	officer := m.isCorpOfficer(ctx, b.CorpID, b.CharID) || infraRigger(b.CharID, b.CorpID)
	if !officer {
		return false
	}
	lvl := 0
	if skills, err := m.charSkillsFor(ctx, b.CharID); err == nil {
		lvl = skills[skillAnchoring]
	}
	return anchorClaimOK(need, officer, lvl, age)
}

// ── the skill goal ──────────────────────────────────────────────────────────

// anchorCorp is the managed corporation this pilot is an officer of, 0 when
// it is none (or starbases are switched off). Memoised for the plan.
func (p *skillPlanner) anchorCorp() int64 {
	if p.anchorCorpID != nil {
		return *p.anchorCorpID
	}
	v := int64(0)
	defer func() { p.anchorCorpID = &v }()
	if !StarbasePower() {
		return v
	}
	if corpID, ok := managedCEOOf(p.charID); ok {
		v = corpID
		return v
	}
	if p.m == nil || p.m.db == nil || p.ctx == nil {
		return v
	}
	var corpID int64
	var roles sql.NullInt64
	if err := p.m.db.QueryRowContext(p.ctx, `
		SELECT ch.corporationID, r.corprole FROM character_ ch
		  LEFT JOIN chrCorporationRoles r ON r.characterID = ch.characterID
		 WHERE ch.characterID = ?`, p.charID).Scan(&corpID, &roles); err != nil {
		return v
	}
	if _, managed := managedCEO(corpID); managed &&
		((roles.Valid && uint64(roles.Int64)&corp.RoleDirector != 0) || infraRigger(p.charID, corpID)) {
		v = corpID
	}
	return v
}

// infraRigger reports whether a pilot anchors for its corporation as a member of
// Infrastructure, at any tier. The arm is capped (infraArmCap), so the cost is bounded.
func infraRigger(charID, corpID int64) bool {
	s, ok := armOf(charID, corpID)
	return ok && s.Arm == armInfrastructure
}

// anchorGoal is the next level of Anchoring for an officer, ok=false when
// the pilot is not one or already holds anchorGoalCap.
func (p *skillPlanner) anchorGoal() (commandGoal, bool) {
	if p.anchorCorp() == 0 || p.levels[skillAnchoring] >= anchorGoalCap {
		return commandGoal{}, false
	}
	return commandGoal{skillAnchoring, p.levels[skillAnchoring] + 1, commandWhyAnchor}, true
}

// corpStarbaseActive reports whether a corporation has a tower, a survey or
// a starbase requisition on its boards.
func (p *skillPlanner) corpStarbaseActive(corpID int64) bool {
	if p.m == nil || p.m.db == nil || p.ctx == nil {
		return false
	}
	var n int
	_ = p.m.db.QueryRowContext(p.ctx, `
		SELECT (SELECT COUNT(*) FROM rolebot_corp_towers WHERE corporationID = ? AND status <> 'gone')
		     + (SELECT COUNT(*) FROM rolebot_corp_survey WHERE corporationID = ? AND status IN ('open','claimed'))`,
		corpID, corpID).Scan(&n)
	return n > 0
}

// evaluateAnchoring prices a level of Anchoring for an officer.
func (p *skillPlanner) evaluateAnchoring(g *skillGoal) {
	corpID := p.anchorCorp()
	if corpID == 0 {
		return
	}
	g.value = anchorGoalValue(p.poolRef, p.levels[skillAnchoring], p.corpStarbaseActive(corpID))
}

// anchorGoalValue is the price rule, pure for the harness.
func anchorGoalValue(ref float64, from int, active bool) float64 {
	if ref <= 0 || from >= anchorGoalCap {
		return 0
	}
	v := ref * anchorLevelWorth
	if from == 0 {
		v *= anchorOpenMul
	}
	if !active {
		v *= anchorIdleMul
	}
	return v
}
