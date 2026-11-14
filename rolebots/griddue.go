package rolebots

import (
	"sync"
	"time"
)

// Pilot state the grid lane reads before it loads a pilot.
//
// gridStepDue needs the halt flag and the end of the timed leg, both kept in
// rolebot_state. saveState writes that row and also keeps this mirror. The lane
// reads the mirror and loads only pilots that are due or unknown. A wrong "not
// due" delays a pilot by at most gridIdleStepSec.

type gridDueRec struct {
	legEndsAt int64
	halted    bool
}

var (
	gridDueMu sync.Mutex
	gridDue   = map[int64]gridDueRec{}
)

func init() {
	registerCharForget("gridDue", forgetGridDue)
}

// noteGridDue records the fields of a pilot's state that decide its turn.
// Called from saveState.
func noteGridDue(charID, legEndsAt int64, halted bool) {
	gridDueMu.Lock()
	gridDue[charID] = gridDueRec{legEndsAt: legEndsAt, halted: halted}
	gridDueMu.Unlock()
}

// seedGridDue is noteGridDue for a value read rather than written: it never
// replaces an existing record, which a concurrent save may have made newer.
func seedGridDue(charID, legEndsAt int64, halted bool) {
	gridDueMu.Lock()
	if _, ok := gridDue[charID]; !ok {
		gridDue[charID] = gridDueRec{legEndsAt: legEndsAt, halted: halted}
	}
	gridDueMu.Unlock()
}

// forgetGridDue drops a pilot's record; called beside forgetSavedState.
func forgetGridDue(charID int64) {
	gridDueMu.Lock()
	delete(gridDue, charID)
	gridDueMu.Unlock()
}

// gridDueHint is gridStepDue read from the mirror, for a solo pilot that is not
// a conscript (the lane loads conscripts unconditionally). known is false when
// there is no record, so the caller must load to find out.
func gridDueHint(charID int64, lastStep, now time.Time) (due, known bool) {
	gridDueMu.Lock()
	r, ok := gridDue[charID]
	gridDueMu.Unlock()
	if !ok {
		return true, false
	}
	return stepDueAt(r.halted, false, r.legEndsAt, lastStep, now), true
}

// stepDueAt is the rule gridStepDue and gridDueHint share, so the filter
// before the load and the one after it cannot disagree.
func stepDueAt(halted, commanded bool, legEndsAt int64, lastStep, now time.Time) bool {
	if halted || commanded {
		return true
	}
	if legEndsAt <= now.Unix() {
		return true
	}
	return now.Sub(lastStep) >= gridIdleStepSec*time.Second
}
