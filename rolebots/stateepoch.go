package rolebots

import (
	"context"
	"sync"
	"sync/atomic"
)

// ── stale roster copies ─────────────────────────────────────────────────────
//
// The roster pass loads every pilot once (loadRuns) and then spends up to a minute stepping them. A
// pilot whose state row is rewritten by someone else in that window still holds the old copy at its
// turn and writes it back whole. For example, a gangmate killed in the commander's duel is reset to
// idle (loseHull, resetBot), then steps from its pre-death copy and is put back in a fight in its pod.
// The fix is a sequence, not a lock: resetBot, the one funnel for external rewrites, stamps the pilot
// with the next number; the roster load stamps each run with the number it was read under; a run
// whose pilot was reset since is re-read in place before it steps (refreshStaleRun).

var (
	stateSeq      atomic.Uint64
	stateResetMu  sync.Mutex
	stateResetSeq = map[int64]uint64{} // charID to the sequence number of its last reset
)

func init() {
	registerCharForget("stateResetSeq", func(id int64) {
		stateResetMu.Lock()
		delete(stateResetSeq, id)
		stateResetMu.Unlock()
	})
}

// noteStateReset records that charID's state row was just rewritten from
// outside its own turn's copy.
func noteStateReset(charID int64) {
	rosterWakeClear(charID) // the new state is stepped next pass (rosterwake.go)
	n := stateSeq.Add(1)
	stateResetMu.Lock()
	stateResetSeq[charID] = n
	stateResetMu.Unlock()
}

// rosterLoadSeq is the stamp a run read now carries: every reset already recorded is below it,
// every later reset is at or above it. Taken before the roster query, so a reset landing during
// the query counts as after the read, the safe direction. Never 0, which means "not stamped".
func rosterLoadSeq() uint64 { return stateSeq.Load() + 1 }

// runIsStale reports whether b's pilot has been reset since b was loaded.
// A run with no stamp (built outside loadRunsWhere) is never stale.
func runIsStale(b *botRun) bool {
	if b == nil || b.loadedSeq == 0 {
		return false
	}
	stateResetMu.Lock()
	n := stateResetSeq[b.CharID]
	stateResetMu.Unlock()
	return n >= b.loadedSeq
}

// refreshStaleRun re-reads b in place when its pilot was reset since b was loaded. False means the
// copy is stale and could not be replaced, so the caller must not step it this turn. The lane stays
// the one b was sharded to.
func (m *Mod) refreshStaleRun(ctx context.Context, b *botRun) bool {
	if !runIsStale(b) {
		return true
	}
	fresh, err := m.loadRun(ctx, b.CharID)
	if err != nil || fresh == nil {
		return false
	}
	fast := b.fastLane
	*b = *fresh
	b.fastLane = fast
	return true
}
