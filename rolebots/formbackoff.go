package rolebots

import "sync"

// The formation back-off. A fleet that falls apart minutes after forming says its
// corporation cannot field that fleet right now; without a memo the next formation pass
// raises it again with other pilots and breaks off their errands for nothing. Each young
// death makes the corporation wait before it forms that activity again, and the wait
// doubles while the deaths keep coming. Chair and player orders are not held back
// (formFleetAround). Held in memory: formation runs on one node, and a restart forgetting
// the waits costs one extra attempt. See docs/comment-notes-b10.md#formation-back-off.

const (
	// fleetYoungSec is how soon after forming a fleet's end counts as a young death.
	fleetYoungSec = 30 * 60
	// formBackoffCapSec is the longest wait, and how long a quiet spell clears the streak.
	formBackoffCapSec = 2 * 3600
)

type formBackoffKey struct {
	corpID   int64
	activity string
}

type formBackoffHold struct {
	until  int64 // unix: no new fleet before this
	lastAt int64 // unix: the last young death
	streak int   // young deaths in a row
}

var (
	formBackoffMu sync.Mutex
	formBackoffs  = map[formBackoffKey]formBackoffHold{}
)

// formBackoffWait is the wait after the streak-th young death in a row (0 = the first):
// base, doubled each time, capped. Pure.
func formBackoffWait(base int64, streak int) int64 {
	if base <= 0 {
		return 0
	}
	wait := base
	for i := 0; i < streak && wait < formBackoffCapSec; i++ {
		wait *= 2
	}
	return min(wait, formBackoffCapSec)
}

// noteFleetDiedYoung records that a corporation's fleet ended without doing its job.
// bornAt is the fleet's creation time; a fleet older than fleetYoungSec is not counted.
func noteFleetDiedYoung(corpID int64, activity string, bornAt int64) {
	base := FormBackoffSec()
	if corpID == 0 || base <= 0 {
		return
	}
	now := clock.Unix()
	if bornAt > 0 && now-bornAt >= fleetYoungSec {
		return
	}
	k := formBackoffKey{corpID, activity}
	formBackoffMu.Lock()
	defer formBackoffMu.Unlock()
	h := formBackoffs[k]
	if h.lastAt != 0 && now-h.lastAt > formBackoffCapSec {
		h.streak = 0
	}
	h.until = now + formBackoffWait(base, h.streak)
	h.lastAt = now
	h.streak++
	formBackoffs[k] = h
}

// formBackedOff reports how many seconds a corporation must still wait before forming
// this activity again; 0 when it may form.
func formBackedOff(corpID int64, activity string) int64 {
	now := clock.Unix()
	formBackoffMu.Lock()
	defer formBackoffMu.Unlock()
	if h, ok := formBackoffs[formBackoffKey{corpID, activity}]; ok && now < h.until {
		return h.until - now
	}
	return 0
}

// sweepFormBackoff drops holds whose streak has lapsed (registered on the cache sweep).
func sweepFormBackoff() {
	now := clock.Unix()
	formBackoffMu.Lock()
	defer formBackoffMu.Unlock()
	for k, h := range formBackoffs {
		if now-h.lastAt > formBackoffCapSec {
			delete(formBackoffs, k)
		}
	}
}

func init() {
	registerCorpForget("formBackoffs", func(corpID int64) {
		formBackoffMu.Lock()
		defer formBackoffMu.Unlock()
		for k := range formBackoffs {
			if k.corpID == corpID {
				delete(formBackoffs, k)
			}
		}
	})
	registerSweep("formBackoff", sweepFormBackoff)
}
