package rolebots

import "sync"

// The call-up memo. gangCallUp (gangplan.go) drops a busy member to idle so plan() can route
// it to the muster. plan() runs its ladder first (plan_ladder.go), and a ladder errand, such
// as a capsule pilot's ride to a hull, outranks gang duty: the pilot is busy again on the
// next pass. Without a memo the call-up breaks that leg off every pass and the ladder
// restarts it, for as long as the muster lasts.

// gangCallUpAgainSec is how long a member the muster has claimed is left alone when it
// turns up busy again, in seconds. A member that did reach the muster is in a muster phase
// and is never asked twice.
const gangCallUpAgainSec = 600

var (
	gangCalledMu    sync.Mutex
	gangCalledUntil = map[int64]int64{} // charID → unix
)

// gangCalledRecently reports whether the muster claimed this pilot inside gangCallUpAgainSec.
func gangCalledRecently(charID int64) bool {
	now := clock.Unix()
	gangCalledMu.Lock()
	defer gangCalledMu.Unlock()
	return now < gangCalledUntil[charID]
}

// noteGangCalled remembers that the muster has just claimed this pilot.
func noteGangCalled(charID int64) {
	until := clock.Unix() + gangCallUpAgainSec
	gangCalledMu.Lock()
	defer gangCalledMu.Unlock()
	gangCalledUntil[charID] = until
}

// sweepGangCalled drops expired entries (registered on the cache sweep): a member that
// reached its muster never asks again, so the read path would keep its entry.
func sweepGangCalled() {
	now := clock.Unix()
	gangCalledMu.Lock()
	defer gangCalledMu.Unlock()
	for id, until := range gangCalledUntil {
		if now >= until {
			delete(gangCalledUntil, id)
		}
	}
}

func init() {
	registerCharForget("gangCalledUntil", func(id int64) {
		gangCalledMu.Lock()
		defer gangCalledMu.Unlock()
		delete(gangCalledUntil, id)
	})
	registerSweep("gangCalled", sweepGangCalled)
}
