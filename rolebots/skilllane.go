package rolebots

import (
	"context"
	"runtime/debug"
	"time"
)

// The skill lane: its own clock, apart from the sim pass, since planning touches
// no solar system or connection. The batch is sized against the lane's measured
// pass duration (skillPace), not the tick it is registered under.
const (
	// skillEveryTicks is the lane's clock, the sim lane's five seconds, but nothing
	// else shares this guard.
	skillEveryTicks = 5

	// skillPassBudget is what the planning half of one pass may spend: long enough
	// for several rounds of workers at eight to ten seconds a plan.
	skillPassBudget = 60 * time.Second
	// skillFloorBudget is the floor's share of the pass (skillfloor.go). It is
	// small so the floor never delays the real planning.
	skillFloorBudget = 20 * time.Second
)

// startSkills registers the skill lane.
func (m *Mod) startSkills() {
	m.registerPeriodic("rolebots-skills", skillEveryTicks, func(tickNum uint64) bool {
		if !Master() {
			return true
		}
		// Same shape as the sim lane: the tick manager runs on one goroutine, so the
		// database work goes off it and the pass guards its own overlap.
		if !m.skillBusy.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer m.skillBusy.Store(false)
			defer func() {
				if r := recover(); r != nil {
					rlogf("rolebots: skill lane panicked: %v\n%s", r, debug.Stack())
				}
			}()
			m.skillLanePass(context.Background())
		}()
		return true
	})
}

// skillLanePass is one turn of the lane: the floor first, then the planner. The
// floor runs first on the smaller budget: an empty queue loses SP that cannot be
// earned back, and the planner replaces what the floor laid down in its turn.
func (m *Mod) skillLanePass(ctx context.Context) {
	started := clock.Unix()

	floorCtx, cancel := context.WithTimeout(ctx, skillFloorBudget)
	m.skillFloorPass(floorCtx)
	cancel()

	planCtx, cancel := context.WithTimeout(ctx, skillPassBudget)
	m.skillPass(planCtx)
	cancel()

	// What the next batch is sized against. A pass with no measurable time keeps the
	// previous reading, since a zero would size the next batch at one.
	if d := clock.Unix() - started; d > 0 {
		m.skillPace.Store(d)
	}
}

// skillPaceSec is the lane's measured pass duration in seconds, falling back
// to the tick it is registered at until a pass has actually been timed.
func (m *Mod) skillPaceSec() float64 {
	if d := m.skillPace.Load(); d > 0 {
		return float64(d)
	}
	return skillEveryTicks
}
