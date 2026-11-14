package rolebots

import (
	"context"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// The sim pass, parallelised.
//
// simPass holds simBusy for as long as the pass takes, so a serial loop makes the
// real cadence the sum of every bot's database round trips. The pass is a worker
// pool instead. A gang is not safe to run in parallel: its members share one JSON
// blackboard in rolebot_gangs.state, written back whole. So the roster is cut
// into units (a whole gang, or one solo pilot), one worker steps a unit in roster
// order, and workers pull the next unit off a shared counter.

const (
	// simDefaultWorkers is how many pilots may take a turn at once. Eight against
	// the core's SetMaxOpenConns(64) leaves the rest of the pool to the server.
	simDefaultWorkers = 8
	// simMaxWorkers caps whatever an operator puts in config.json, so the module
	// does not compete with the game for connections.
	simMaxWorkers = 32
)

// simWorkers resolves the configured pool size. One is a supported answer: the
// serial pass, the escape hatch and what the harness uses so a simulated week
// runs the same way twice.
func (m *Mod) simWorkers() int {
	n := m.cfg.SimWorkers
	if n <= 0 {
		n = simDefaultWorkers
	}
	if n > simMaxWorkers {
		n = simMaxWorkers
	}
	return n
}

// shardKey is what decides which unit a bot belongs to. Gangmates share a key;
// everyone else is alone with their own ID. A gangID and charID collision only
// puts two bots in one unit, which is the safe direction.
func shardKey(b *botRun) int64 {
	if b.GangID != 0 {
		return b.GangID
	}
	return b.CharID
}

// shardUnits cuts the roster into units that must not be split across
// workers, in order of each unit's first member, with roster order kept
// inside each unit.
func shardUnits(bots []*botRun) [][]*botRun {
	index := make(map[int64]int, len(bots))
	var units [][]*botRun
	for _, b := range bots {
		k := shardKey(b)
		i, ok := index[k]
		if !ok {
			i = len(units)
			index[k] = i
			units = append(units, nil)
		}
		units[i] = append(units[i], b)
	}
	return units
}

// runUnits steps every bot, up to workers units at a time. Each worker
// pulls the next unit off a shared counter until none are left.
func runUnits(bots []*botRun, workers int, step func(*botRun)) {
	if workers <= 1 || len(bots) <= 1 {
		for _, b := range bots {
			step(b)
		}
		return
	}
	units := shardUnits(bots)
	workers = min(workers, len(units))
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(units) {
					return
				}
				for _, b := range units[i] {
					step(b)
				}
			}
		}()
	}
	wg.Wait()
}
// runBotPass gives every enabled bot one turn, up to simWorkers() of them
// at a time.
func (m *Mod) runBotPass(ctx context.Context, bots []*botRun) {
	m.runBotPassN(ctx, bots, m.simWorkers())
}

// runBotPassN is the same pass at a caller's chosen width. The configured
// width is sized for a server with players on it: eight pilots at a time
// leaves the game the rest of the connection pool.
func (m *Mod) runBotPassN(ctx context.Context, bots []*botRun, workers int) {
	if workers <= 0 {
		workers = m.simWorkers()
	}
	runUnits(bots, workers, func(b *botRun) { m.stepBot(ctx, b) })
}

// stepBot is one bot's turn. The recover keeps a panic in a planner from
// killing the process or abandoning the rest of the unit mid-pass with a gang's
// blackboard half-written; it costs one bot its turn and logs the stack.
func (m *Mod) stepBot(ctx context.Context, b *botRun) {
	// A halted bot is frozen until haltRetrySec passes, then its trip is dropped and
	// it plans fresh; the state row stays as it wedged for diagnosis and the panel.
	if b.State.Halted {
		if clock.Unix()-b.State.LastErrorAt < haltRetrySec {
			return
		}
		last := b.State.LastError
		if err := resetBot(ctx, m.db, b.CharID); err != nil {
			rlogf("rolebots: releasing halt for %d: %v", b.CharID, err)
			return
		}
		// The next pass reads the row this just wrote; stepping on now would decide
		// from the wedged copy in hand.
		m.logBot(b.CharID, logInfo, "sim",
			"released from halt after %ds — trip dropped, planning fresh (was: %s)",
			haltRetrySec, last)
		return
	}
	// One turn at a time per pilot, across every lane (roster pass and grid lane):
	// both write the state row back whole, so concurrent turns lose a transition.
	unlock := lockBotStep(b.CharID)
	defer unlock()

	// Ownership, not just exclusion: a pilot with a ball on grid belongs to the grid
	// lane. Solo pilots only. See docs/comment-notes-b17.md#grid-lane-ownership.
	if !b.fastLane && gridLaneOwns(b, m.isEmbodied(b.CharID)) {
		return
	}

	botCtx, cancel := context.WithTimeout(ctx, simBotBudget)
	defer cancel()

	// A gangmate killed in an earlier pilot's duel this pass was reset to idle in a
	// pod; stepping the stale copy would write it back (stateepoch.go).
	if !m.refreshStaleRun(botCtx, b) {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			m.logBot(b.CharID, logError, "sim",
				"panicked during %s: %v\n%s", phaseLabel(b.State), r, debug.Stack())
			noteCtx, noteCancel := context.WithTimeout(ctx, simSaveBudget)
			m.noteFailure(noteCtx, b.State, "sim", "panic during its turn — see the log for the stack")
			noteCancel()
		}
	}()

	_, err := m.advance(botCtx, b)
	switch {
	case err != nil:
		m.logBot(b.CharID, logError, "sim", "%s failed: %v", phaseLabel(b.State), err)
		// The failure record is written on its own clock: a timed-out bot has an expired
		// context and saving through it would lose the error.
		noteCtx, noteCancel := context.WithTimeout(ctx, simSaveBudget)
		m.noteFailure(noteCtx, b.State, "sim", err.Error())
		noteCancel()
	case b.State.Phase != "idle":
		// Got somewhere: the failure streak is over.
		m.clearFailure(botCtx, b.State)
	}
	// Unwatched space is not safe: a bot on a belt draws the same spawns as on a
	// player's grid (combat_abstract.go), and acts on a refreshed copy of its state.
	if !m.refreshStaleRun(botCtx, b) {
		return
	}
	m.abstractCombatPass(botCtx, b)
}
