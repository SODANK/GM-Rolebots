package rolebots

// saveState runs at the end of every sim step, and most steps change nothing: an idle pilot rewrites
// the same activity, phase and data blob. So the last successful save is fingerprinted per pilot and
// an identical save is skipped. Two guards: every path that deletes a state row forgets the
// fingerprint (forgetSavedState), or an unchanged pilot would never write its row back; and a
// fingerprint older than stateRewriteAfter forces a real write, bounding a delete path nobody wired.

import (
	"hash/fnv"
	"strconv"
	"sync"
	"time"
)

// stateRewriteAfter is the longest an unchanged state goes unwritten.
const stateRewriteAfter = 10 * time.Minute

type savedStateRec struct {
	sum uint64
	at  time.Time
}

var (
	savedStateMu sync.Mutex
	savedState   = map[int64]savedStateRec{}
)

// stateFingerprint is every column saveState writes except updatedAt.
func stateFingerprint(st *SimState, blob []byte, halted int) uint64 {
	h := fnv.New64a()
	for _, s := range []string{
		st.Activity, st.Phase, string(blob),
		strconv.Itoa(st.ErrCount), st.LastError,
		strconv.FormatInt(st.LastErrorAt, 10), strconv.Itoa(halted),
	} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// stateUnchanged reports whether this save would write exactly what the last
// successful one did, recently enough that it can be skipped.
func stateUnchanged(charID int64, sum uint64) bool {
	savedStateMu.Lock()
	defer savedStateMu.Unlock()
	r, ok := savedState[charID]
	return ok && r.sum == sum && clock.Now().Sub(r.at) < stateRewriteAfter
}

// noteStateSaved records a successful write.
func noteStateSaved(charID int64, sum uint64) {
	savedStateMu.Lock()
	savedState[charID] = savedStateRec{sum: sum, at: clock.Now()}
	savedStateMu.Unlock()
}

// forgetSavedState must be called wherever a rolebot_state row is deleted.
func forgetSavedState(charID int64) {
	savedStateMu.Lock()
	delete(savedState, charID)
	savedStateMu.Unlock()
	forgetClaim(charID)   // the claims overlay mirrors the same row (claims.go)
	forgetGridDue(charID) // so does the grid lane's due mirror (griddue.go)
}

// sweepSavedState drops fingerprints past the rewrite window: they would force a write on next use
// anyway, and a purged pilot's would otherwise be held for the life of the process.
func sweepSavedState() {
	cut := clock.Now().Add(-stateRewriteAfter)
	savedStateMu.Lock()
	for id, r := range savedState {
		if r.at.Before(cut) {
			delete(savedState, id)
		}
	}
	savedStateMu.Unlock()
}

func init() {
	// forgetSavedState chains forgetClaim (claims.go): the overlay mirrors
	// the same row.
	registerCharForget("savedState/claimsOverlay", forgetSavedState)
	registerSweep("savedState", sweepSavedState)
}