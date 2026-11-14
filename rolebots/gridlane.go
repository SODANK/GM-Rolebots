package rolebots

import (
	"context"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// The grid lane: a one-second clock for the bots a player can see.
//
// The roster pass comes round about once a minute, which would stretch every
// five-second nav hold (navLegHoldSec) of a projection to a minute. Pilots with a
// ball on grid get their own lane; the roster pass keeps everyone else and every
// gang member (gridLaneOwns).
//
// The lane is a worker pool, since a hub holds thousands of projections. A
// projection whose leg is not due gets a full turn only every gridIdleStepSec,
// decided against a mirror of two state fields (griddue.go) before any load.

// gridLaneTicks is in tick.Manager ticks, which are one second.
const gridLaneTicks = 1

const (
	// gridIdleStepSec is how often a projection whose timed leg is not due still
	// gets a full turn, so the pre-leg sweeps in advance() run for it. Due, halted
	// and commanded pilots are stepped every pass.
	gridIdleStepSec = 30

	// gridOverdueReportSec is the threshold the log line counts legs against; a leg
	// 30 s past due has missed six turns.
	gridOverdueReportSec = 30

	// gridLaneLogEvery is how often one line about the lane reaches server.log, as
	// warpLogEvery does for warps.
	gridLaneLogEvery = time.Minute
)

// gridLogAt is when the lane last printed. gridPass is overlap-guarded, so
// this is only ever touched from one goroutine at a time.
var gridLogAt time.Time

// startGridLane registers the fast clock. Its overlap guard is its own:
// a slow pass must never be able to hold the visible bots still.
func (m *Mod) startGridLane() {
	m.registerPeriodic("rolebots-grid", gridLaneTicks, func(tickNum uint64) bool {
		if !Master() {
			return true
		}
		if !m.gridBusy.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer m.gridBusy.Store(false)
			m.gridPass(context.Background())
		}()
		return true
	})
}

// gridPass gives every projected bot a turn and then lets the projections move
// (embodyTick).
func (m *Mod) gridPass(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			rlogf("rolebots: grid lane panicked: %v\n%s", r, debug.Stack())
		}
	}()

	// Projections plus anybody serving in a player's fleet, so a conscript with no
	// ball yet does not wait for the roster pass.
	conscripts := conscriptCharIDs()
	ids := mergeCharIDs(liveProjectionCharIDs(), conscripts)
	if len(ids) > 0 {
		m.stepProjectedBots(ctx, ids, conscripts)
	}
	// Idle wander, belt hops and the rest of what a projection does off its own
	// clock. In memory only.
	m.embodyTick()
}

// mergeCharIDs unions two ID sets, dropping duplicates. Both sides are small
// (what is projected, and what is under orders), and a pilot in both would
// otherwise be loaded and stepped twice in one pass.
func mergeCharIDs(a, b []int64) []int64 {
	if len(b) == 0 {
		return a
	}
	seen := make(map[int64]bool, len(a)+len(b))
	out := make([]int64, 0, len(a)+len(b))
	for _, list := range [][]int64{a, b} {
		for _, id := range list {
			if id == 0 || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// gridLaneOwns reports whether a pilot's turn belongs to the grid lane rather
// than the roster pass: it has a ball on grid and is solo. A gang's blackboard is
// written whole by whichever member steps, and only the roster pass keeps a gang
// on one worker (simpool.go shardKey).
func gridLaneOwns(b *botRun, embodied bool) bool {
	return embodied && b.GangID == 0
}

// gridStepDue says whether a projected pilot needs a full turn this pass: its
// timed leg has expired, it is halted, or a player commands it. Otherwise a turn
// every gridIdleStepSec keeps the pre-leg sweeps running. A zero lastStep (never
// stepped) reads as overdue.
func gridStepDue(b *botRun, lastStep time.Time, now time.Time) bool {
	return stepDueAt(b.State.Halted, b.BossID != 0, b.State.Data.LegEndsAt, lastStep, now)
}

// legOverdueSec is how far past its end a timed leg is, for the log line. It is
// zero for a pilot with no leg: LegEndsAt 0 means no timed leg, not a leg that
// ended in 1970. gridStepDue still reads 0 as due.
func legOverdueSec(legEndsAt int64, now time.Time) int64 {
	if legEndsAt <= 0 {
		return 0
	}
	return now.Unix() - legEndsAt
}

// gridPassStats is what one pass counted, for the log line.
type gridPassStats struct {
	projected, loaded, solo, gang, due, overdue int
	worstOverdueSec                             int64
	took, load                                  time.Duration
}

// stepProjectedBots hydrates and steps the named pilots: the roster pass in
// miniature, with the same loader, per-bot turn, panic guard and worker pool.
// Only pilots the due mirror says need a turn are loaded, plus conscripts, whose
// BossID is not in the mirror. gridStepDue has the final say.
func (m *Mod) stepProjectedBots(ctx context.Context, ids, conscripts []int64) {
	start := clock.Now()
	var st gridPassStats
	st.projected = len(ids)

	commanded := make(map[int64]bool, len(conscripts))
	for _, id := range conscripts {
		commanded[id] = true
	}
	load := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !commanded[id] {
			var last time.Time
			if eb := liveProjection(id); eb != nil {
				eb.mu.Lock()
				last = eb.lastGridStepAt
				eb.mu.Unlock()
			}
			if due, known := gridDueHint(id, last, start); known && !due {
				continue
			}
		}
		load = append(load, id)
	}
	if len(load) == 0 {
		st.took = clock.Now().Sub(start)
		m.logGridPass(st, clock.Now())
		return
	}

	loadCtx, cancel := context.WithTimeout(ctx, simBotBudget)
	defer cancel()

	where, args := inClause(load)
	bots, err := m.loadRunsWhere(loadCtx, " AND c.characterID IN "+where, args)
	if err != nil {
		rlogf("rolebots: grid lane load: %v", err)
		return
	}
	st.load = clock.Now().Sub(start)

	now := clock.Now()
	st.loaded = len(bots)
	var turn []*botRun
	for _, b := range bots {
		// Seed the mirror for a pilot that has not saved since boot, only when absent: a
		// save that raced this read is newer.
		seedGridDue(b.CharID, b.State.Data.LegEndsAt, b.State.Halted)
		// Gang members belong to the roster pass. See gridLaneOwns.
		if b.GangID != 0 {
			st.gang++
			continue
		}
		st.solo++
		var last time.Time
		if eb := liveProjection(b.CharID); eb != nil {
			eb.mu.Lock()
			last = eb.lastGridStepAt
			eb.mu.Unlock()
		}
		if over := legOverdueSec(b.State.Data.LegEndsAt, now); over > gridOverdueReportSec {
			st.overdue++
			if over > st.worstOverdueSec {
				st.worstOverdueSec = over
			}
		}
		if !gridStepDue(b, last, now) {
			continue
		}
		st.due++
		b.fastLane = true
		turn = append(turn, b)
	}

	// One unit per pilot: every pilot here is solo, so no blackboard needs one
	// worker, and lockBotStep already serialises a pilot across lanes.
	var moved atomic.Bool
	step := func(b *botRun) {
		wasSys, wasStation := b.SystemID, b.StationID
		m.stepBot(ctx, b)
		if eb := liveProjection(b.CharID); eb != nil {
			eb.mu.Lock()
			eb.lastGridStepAt = now
			eb.mu.Unlock()
		}
		if b.SystemID != wasSys || b.StationID != wasStation {
			moved.Store(true)
		}
	}
	runUnits(turn, m.simWorkers(), step)

	// Local and station guest lists follow the database, not what a step said it did.
	// Only when something moved: syncPresence reads the whole roster in one query.
	if moved.Load() {
		m.syncPresence()
	}
	st.took = clock.Now().Sub(start)
	m.logGridPass(st, clock.Now())
}

// logGridPass prints at most one line per gridLaneLogEvery about the lane, to
// server.log beside the warp-budget line. A pass under a second on a few balls
// prints nothing, as admitBotWarp does.
func (m *Mod) logGridPass(st gridPassStats, now time.Time) {
	if st.projected == 0 || now.Sub(gridLogAt) < gridLaneLogEvery {
		return
	}
	if st.took < time.Second && st.overdue == 0 {
		return
	}
	gridLogAt = now
	log.Printf("[rolebots] grid lane — %d projection(s), %d loaded (%d solo, %d gang left to the pass), "+
		"%d stepped, pass %.1fs (load %.1fs), %d leg(s) overdue >%ds (worst %ds)",
		st.projected, st.loaded, st.solo, st.gang, st.due, st.took.Seconds(), st.load.Seconds(),
		st.overdue, gridOverdueReportSec, st.worstOverdueSec)
}

// liveProjectionCharIDs snapshots who currently has a ball on grid.
func liveProjectionCharIDs() []int64 {
	embodyMu.Lock()
	defer embodyMu.Unlock()
	if len(embodyLive) == 0 {
		return nil
	}
	out := make([]int64, 0, len(embodyLive))
	for charID := range embodyLive {
		out = append(out, charID)
	}
	return out
}

// ── one turn at a time per pilot ────────────────────────────────────────────

// botStepMu serialises turns for one character across the lanes that can give it
// one. It is sharded, not a map of mutexes: two pilots on one shard only take
// turns.
const botStepShards = 256

var botStepMu [botStepShards]sync.Mutex

func lockBotStep(charID int64) func() {
	i := int(uint64(charID) % botStepShards)
	botStepMu[i].Lock()
	return botStepMu[i].Unlock
}
