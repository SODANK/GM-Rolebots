package rolebots

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// Identity seeding: what a new rolebot is given at birth.
//
// A pilot arrives with a personality (rolebot_traits, written by botgen.go
// before anything reads it) and its skills. It arrives with no standings
// rows: a pilot's opinion of a faction is read from whoever employs it, at
// the moment it is asked (outlook.go), so nothing seeds or stores it.
//
// No preferred-region list is seeded either (see residency.go homeRegionSet).

// raceEmpireFaction names the empire faction of each generatable race, race 16
// (Jove, Modifier bloodline) included. corps.go resolves the names to factionIDs
// for the bloc rule; a race missing here leaves its pilots refused by their own state.
var raceEmpireFaction = map[int64]string{
	1:  "Caldari State",
	2:  "Minmatar Republic",
	4:  "Amarr Empire",
	8:  "Gallente Federation",
	16: "Jove Empire",
}

// racePirateFaction names the pirate faction native to each empire race, for a
// corporation that turns to piracy (allegiance.go deriveAllegiance). Names, like
// raceEmpireFaction, resolved through chrFactions.
var racePirateFaction = map[int64]string{
	1: "Guristas Pirates",
	2: "Angel Cartel",
	4: "Blood Raider Covenant",
	8: "Serpentis",
}

// ── skill-sheet repair ──────────────────────────────────────────────────────
//
// Some adopted characters hold skills whose prerequisites were never injected
// (Small Projectile Turret V with no Gunnery). effectiveFit drops every module
// gated on the missing skill, so the pilot scores zero with a gun. The sheet is
// repaired: holding a skill is evidence its prerequisites were met, so they
// are granted at exactly the required level, transitively, never above it.

// repairSkillPrereqs grants charID every prerequisite implied by the skills
// it already holds. Returns how many skills were granted. Idempotent.
func (m *Mod) repairSkillPrereqs(ctx context.Context, charID int64) (int, error) {
	skills, err := m.charSkillsFor(ctx, charID)
	if err != nil {
		return 0, err
	}
	granted := 0
	queue := make([]int64, 0, len(skills))
	for id := range skills {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		reqs, err := m.requiredSkills(ctx, id)
		if err != nil {
			return granted, err
		}
		for _, r := range reqs {
			if skills[r.SkillID] >= r.Level {
				continue
			}
			forgetCharSkills(charID) // the sheet just changed (skillcache.go)
			name, err := evedb.GiveSkill(ctx, m.db, charID, r.SkillID, r.Level)
			if err != nil {
				return granted, fmt.Errorf("grant skill %d: %w", r.SkillID, err)
			}
			skills[r.SkillID] = r.Level
			granted++
			// The granted skill has prerequisites of its own to satisfy.
			queue = append(queue, r.SkillID)
			m.logBot(charID, logInfo, "roster",
				"granted %s %d — a skill already held requires it, so it must have been trained once", name, r.Level)
		}
	}
	return granted, nil
}

// repairRosterSkills runs the skill-sheet repair over the whole roster.
// Runs at Init so predictions and procurement never argue with a character
// sheet the game itself could not have produced.
func (m *Mod) repairRosterSkills(ctx context.Context) {
	ids, err := int64Column(ctx, m, `SELECT characterID FROM rolebot_characters`)
	if err != nil {
		rlogf("rolebots: skill repair: %v", err)
		return
	}
	total := 0
	for _, id := range ids {
		n, err := m.repairSkillPrereqs(ctx, id)
		total += n
		if err != nil {
			rlogf("rolebots: skill repair char %d: %v", id, err)
		}
	}
	if total > 0 {
		rlogf("rolebots: granted %d missing prerequisite skill(s) across the roster", total)
	}
}

// int64Column runs a single-column query into a slice.
func int64Column(ctx context.Context, m *Mod, query string, args ...any) ([]int64, error) {
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
