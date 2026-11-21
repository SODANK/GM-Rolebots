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

// stateRewriteAfter is the longest an unchanged state goes unwritten. Every delete path forgets
// the fingerprint (forgetSavedState via the forget registry), so this only bounds a path nobody
// wired. At ten minutes it rewrote ~50 unchanged blobs a second on a 30k roster.
const stateRewriteAfter = time.Hour

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

// seedSavedState records the fingerprint of a row just read from the table, unless a save has
// already recorded one: a read must not extend the rewrite window a save started. The stored blob
// is the json.Marshal output of an earlier save, so an unchanged pilot re-marshals to the same
// bytes and its next save is skipped; a blob written by an older struct shape differs and is
// rewritten once.
func seedSavedState(charID int64, sum uint64) {
	if charID == 0 {
		return
	}
	savedStateMu.Lock()
	if _, ok := savedState[charID]; !ok {
		savedState[charID] = savedStateRec{sum: sum, at: clock.Now()}
	}
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