package rolebots

import (
	"context"
	"fmt"
	"sync/atomic"

	evedb "gurimoon/db"
)

// The floor under the planner: a pilot with an empty queue trains something.
// It queues the next levels of skills the pilot already owns (no book, wallet,
// fit or market) and writes no plan row; applySkillPlan clears it later.
const (
	// skillFloorMaxSteps is how much the floor lays down at once: enough to last an
	// hour, few enough to throw away when a real plan arrives.
	skillFloorMaxSteps = 8
	// skillFloorMaxLevel lets the floor take an owned skill to V; cheapest-step-first
	// ordering means low levels of everything come first.
	skillFloorMaxLevel = 5
	// skillFloorStepMaxMinutes drops any single step longer than a day: the floor is
	// a stopgap and must not hold the queue when the real plan wants it.
	skillFloorStepMaxMinutes = 24 * 60.0
	// skillFloorPerPass is how many idle pilots the floor picks up in one pass; the
	// cursor below keeps it from picking the same pilots twice while others wait.
	skillFloorPerPass = 24
)

// floorCursor is where the last sweep stopped. Ordering by characterID alone
// would starve the tail: a pilot with every owned skill at V would take a slot
// in every pass. The cursor wraps when a sweep comes back short.
var floorCursor atomic.Int64

// skillFloorPass gives the next batch of idle pilots something to train.
func (m *Mod) skillFloorPass(ctx context.Context) {
	ids, err := m.idleUnqueued(ctx, floorCursor.Load(), skillFloorPerPass)
	if err != nil {
		rlogf("rolebots: skill floor: %v", err)
		return
	}
	if len(ids) < skillFloorPerPass {
		floorCursor.Store(0) // end of the roster; round again next pass
	} else {
		floorCursor.Store(ids[len(ids)-1])
	}
	for _, charID := range ids {
		// The floor is the less important half of the pass. Stopping here leaves the
		// rest for the next pass instead of eating the planning budget.
		if ctx.Err() != nil {
			return
		}
		n, err := m.floorQueueFor(ctx, charID)
		if err != nil {
			m.logBot(charID, logError, "skills", "skill floor failed: %v", err)
			continue
		}
		if n == 0 {
			// Nothing to train that it already owns. An all-V pilot is finished, and one
			// waiting on a book it cannot afford is the planner's problem.
			continue
		}
		m.logBot(charID, logInfo, "skills",
			"queued %d level(s) of skills it already owns so it is not standing idle — its own plan replaces this when the planner reaches it", n)
	}
}

// idleUnqueued finds pilots that are enabled, automating their own skills,
// training nothing and holding an empty queue, starting after the cursor. It
// uses duePlanners' joins minus the plan table. chrSkillQueue is joined directly:
// the IS NULL test keeps only rows with no queue, which cannot duplicate.
func (m *Mod) idleUnqueued(ctx context.Context, after int64, limit int) ([]int64, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID
		FROM rolebot_characters c
		LEFT JOIN rolebot_profiles pr ON pr.characterID = c.characterID
		LEFT JOIN chrSkillTraining t ON t.characterID = c.characterID
		LEFT JOIN chrSkillQueue q ON q.characterID = c.characterID
		WHERE c.enabled = 1 AND COALESCE(pr.autoSkills, 1) = 1
		  AND c.characterID > ?
		  AND t.characterID IS NULL AND q.characterID IS NULL
		ORDER BY c.characterID
		LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("idleUnqueued: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var charID int64
		if err := rows.Scan(&charID); err != nil {
			return nil, fmt.Errorf("idleUnqueued scan: %w", err)
		}
		out = append(out, charID)
	}
	return out, rows.Err()
}

// floorQueueFor fills one idle pilot's queue from what it already owns.
// Returns how many entries were added.
func (m *Mod) floorQueueFor(ctx context.Context, charID int64) (int, error) {
	// nil baseline: the trained levels are the truth here, because this only
	// ever runs against a pilot with an empty queue and nothing training.
	catalogue, err := m.skillCatalogueFor(ctx, charID)
	if err != nil {
		return 0, fmt.Errorf("floor catalogue: %w", err)
	}
	plan, err := evedb.PlanMinMaxSkillQueueFrom(ctx, m.db, charID, catalogue, nil,
		skillFloorMaxLevel, skillFloorMaxSteps, skillFloorStepMaxMinutes)
	if err != nil {
		return 0, fmt.Errorf("floor plan: %w", err)
	}
	if len(plan) == 0 {
		return 0, nil
	}
	// The operator's bans still hold. A ban partway down a chain can strand later
	// steps, which the server's queue advance skips at the head.
	_, ban, err := skillPrefs(ctx, m.db, charID)
	if err != nil {
		return 0, err
	}
	steps := make([]evedb.MinMaxPlanStep, 0, len(plan))
	for _, s := range plan {
		if ban[s.TypeID] {
			continue
		}
		steps = append(steps, s)
	}
	if len(steps) == 0 {
		return 0, nil
	}
	// Appended, not cleared-and-written: there is nothing to clear, and
	// AppendSkillQueuePlan drops any step already trained or queued anyway.
	return evedb.AppendSkillQueuePlan(ctx, m.db, charID, steps)
}
