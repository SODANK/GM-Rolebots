package rolebots

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// The skills a capital program needs, and who trains them. The tree is read from the data:
// a hull's required skills (requiredSkills) and the catalogue's prerequisites
// (GetSkillPrereqMap), walked to the leaves, so freighters, dreadnoughts and carriers are
// one code path. The fit-driven planner never reaches these skills, so the program names
// them (programGoals) and the treasury buys the books (programBooks), each only once its
// prerequisites are trained, because PurchaseAndInjectSkill refuses otherwise after the
// money has moved.

const (
	goalProgram = "program"
	// programGoalWorth is a program step's value on the pool anchor: half the pool, above
	// every lever, and the step caps are waived for it (goals()) because a rank-8 skill to V
	// takes weeks whatever the planner thinks.
	programGoalWorth = 0.5
	programGoalWhy   = "its corporation's capital program"
)

// skillPrereqs is the catalogue's prerequisite map, shared and refreshed
// with the market snapshots.
func (m *Mod) skillPrereqs(ctx context.Context) map[int64][]evedb.SkillReq {
	out, _ := snapshot(snapKey("skill-prereqs"), func() (map[int64][]evedb.SkillReq, error) {
		return evedb.GetSkillPrereqMap(ctx, m.db)
	})
	return out
}

// skillTree walks a set of requirements to every skill they rest on,
// answering the highest level each is needed at. Pure over its inputs.
func skillTree(roots []evedb.SkillReq, prereqs map[int64][]evedb.SkillReq) map[int64]int {
	out := map[int64]int{}
	var walk func(id int64, lv int, depth int)
	walk = func(id int64, lv int, depth int) {
		if id == 0 || lv < 1 || depth > 12 {
			return
		}
		if out[id] >= lv {
			return
		}
		out[id] = lv
		for _, r := range prereqs[id] {
			walk(r.SkillID, r.Level, depth+1)
		}
	}
	for _, r := range roots {
		walk(r.SkillID, r.Level, 0)
	}
	return out
}

// pilotSkillTree is every skill, at its level, a pilot needs to board a hull.
func (m *Mod) pilotSkillTree(ctx context.Context, hullTypeID int64) map[int64]int {
	roots, err := m.requiredSkills(ctx, hullTypeID)
	if err != nil {
		return nil
	}
	return skillTree(roots, m.skillPrereqs(ctx))
}

// builderSkillTree is every skill a builder needs to install a print.
func (m *Mod) builderSkillTree(ctx context.Context, bpTypeID int64) map[int64]int {
	roots, err := evedb.GetActivitySkillReqs(ctx, m.db, bpTypeID, 1)
	if err != nil {
		return nil
	}
	return skillTree(roots, m.skillPrereqs(ctx))
}

// treeShort is the skills a sheet is still short of a tree, one level at a
// time: the next level of each, never the target in one jump. Pure.
func treeShort(tree map[int64]int, levels map[int64]int) []commandGoal {
	var out []commandGoal
	for id, want := range tree {
		if have := levels[id]; have < want {
			out = append(out, commandGoal{typeID: id, level: have + 1, why: programGoalWhy})
		}
	}
	return out
}

// bookReady reports whether a skill's book may be injected now: not held,
// and every prerequisite trained to its level. Pure over its inputs.
func bookReady(skill int64, prereqs map[int64][]evedb.SkillReq, levels map[int64]int, injected bool) bool {
	if injected {
		return false
	}
	for _, r := range prereqs[skill] {
		if levels[r.SkillID] < r.Level {
			return false
		}
	}
	return true
}

// programRoles is the open programs this pilot builds for or will fly.
type programRole struct {
	Program *corpProgram
	Builder bool
	Pilot   bool
}

func (m *Mod) programRolesOf(ctx context.Context, charID int64) []programRole {
	rows, err := m.db.QueryContext(ctx, `
		SELECT programID, corporationID, allianceID, kind, hullTypeID, hullBPTypeID, yardStationID, stage,
		       builderID, pilotID, hullItemID, openedAt, updatedAt, note
		  FROM rolebot_corp_program
		 WHERE (builderID = ? OR pilotID = ?) AND stage NOT IN (?, ?)`, charID, charID, programStageCrewed, programStageClosed)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []programRole
	for rows.Next() {
		p := &corpProgram{}
		if rows.Scan(&p.ProgramID, &p.CorpID, &p.AllianceID, &p.Kind, &p.HullTypeID, &p.HullBPTypeID, &p.YardStationID,
			&p.Stage, &p.BuilderID, &p.PilotID, &p.HullItemID, &p.OpenedAt, &p.UpdatedAt, &p.Note) == nil {
			out = append(out, programRole{Program: p, Builder: p.BuilderID == charID, Pilot: p.PilotID == charID})
		}
	}
	return out
}

// programGoals is what the skill planner proposes for a program's builder
// and pilot: the next level of every skill the print or the hull rests on.
func (p *skillPlanner) programGoals() []commandGoal {
	if p.m == nil || p.m.db == nil || p.charID == 0 {
		return nil // a planner built in a test, or one with no pilot behind it
	}
	var out []commandGoal
	for _, r := range p.m.programRolesOf(p.ctx, p.charID) {
		if r.Builder {
			out = append(out, treeShort(p.m.builderSkillTree(p.ctx, r.Program.HullBPTypeID), p.levels)...)
		}
		if r.Pilot {
			out = append(out, treeShort(p.m.pilotSkillTree(p.ctx, r.Program.HullTypeID), p.levels)...)
		}
	}
	return out
}

// evaluateProgram prices a program step.
func (p *skillPlanner) evaluateProgram(g *skillGoal) {
	g.valued = true
	g.value = p.poolRef * programGoalWorth
}

// programBooks is the treasury's half: for the builder, the print's skill tree; for the
// pilot, the hull's. Each book is bought the think its prerequisites are trained, one book
// per person per think. Returns what it spent.
func (m *Mod) programBooks(ctx context.Context, c *corpCEO, budget float64) float64 {
	p := m.openProgramFor(ctx, c.CorpID)
	if p == nil || p.Stage == programStageSaving {
		return 0
	}
	prereqs := m.skillPrereqs(ctx)
	hull := m.typeNameOr(ctx, p.HullTypeID)
	spent := 0.0
	buyOne := func(charID int64, tree map[int64]int, why string) {
		if charID == 0 || len(tree) == 0 {
			return
		}
		levels, err := evedb.TrainedSkillLevels(ctx, m.db, charID)
		if err != nil {
			return
		}
		for skill, want := range tree {
			if levels[skill] >= want {
				continue
			}
			injected, ierr := m.skillInjected(ctx, charID, skill)
			if ierr != nil || !bookReady(skill, prereqs, levels, injected) {
				continue
			}
			if cost, ok := m.corpBuysBook(ctx, c, charID, skill, budget-spent, why); ok {
				spent += cost
				return // one book a think: the queue takes time anyway
			}
		}
	}
	buyOne(p.BuilderID, m.builderSkillTree(ctx, p.HullBPTypeID), "to build the corporation's "+hull)
	// The pilot's kit from the moment parts are being built: a rank-8 skill
	// to V is weeks, and the hull should not wait on it.
	if p.Stage != programStagePrint {
		buyOne(p.PilotID, m.pilotSkillTree(ctx, p.HullTypeID), "to fly the corporation's "+hull)
	}
	return spent
}

// programPilotFor picks who will fly the hull: a member already able to
// board it; else the member nearest to it by trained prerequisites (the most
// of the hull's tree already at level); else the Director of Industry.
func (m *Mod) programPilotFor(ctx context.Context, corpID, directorID, hullTypeID int64) int64 {
	tree := m.pilotSkillTree(ctx, hullTypeID)
	if len(tree) == 0 {
		return directorID
	}
	roots, _ := m.requiredSkills(ctx, hullTypeID)
	need := map[int64]int{}
	for _, r := range roots {
		need[r.SkillID] = r.Level
	}
	if id := m.memberWithSkills(ctx, corpID, need); id != 0 {
		return id
	}
	// The deepest prerequisites first: a racial Battleship V, an Industrial
	// V, Spaceship Command V. Whoever holds the most of the tree's leaves.
	leaves := map[int64]int{}
	prereqs := m.skillPrereqs(ctx)
	for id, lv := range tree {
		if len(prereqs[id]) == 0 {
			leaves[id] = lv
		}
	}
	if id := m.memberWithSkills(ctx, corpID, leaves); id != 0 {
		return id
	}
	return directorID
}

// programSkillSummary words what a program's people still lack, for the
// CEO page.
func (m *Mod) programSkillSummary(ctx context.Context, p *corpProgram) string {
	if p == nil {
		return ""
	}
	short := func(charID int64, tree map[int64]int) int {
		if charID == 0 {
			return len(tree)
		}
		levels, err := evedb.TrainedSkillLevels(ctx, m.db, charID)
		if err != nil {
			return len(tree)
		}
		return len(treeShort(tree, levels))
	}
	b := short(p.BuilderID, m.builderSkillTree(ctx, p.HullBPTypeID))
	f := short(p.PilotID, m.pilotSkillTree(ctx, p.HullTypeID))
	if b == 0 && f == 0 {
		return ""
	}
	return fmt.Sprintf("builder %d skill level(s) short, pilot %d", b, f)
}
