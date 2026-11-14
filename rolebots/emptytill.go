package rolebots

import "sync"

// What the roster has learned about tills that turned out to be empty.
// The market scan is a shared, cached read (marketsnap.go) of what stations stock. When many
// pilots pick the same nearest station for a type, the first few clear the book and the rest
// arrive at an empty till, then re-read the same snapshot and move on together.
// So a till that comes up empty is remembered per (type, station) for a short while and the
// scan stops offering it to the rest of the roster. This is the roster's own shorter-lived
// memory laid over the snapshot, not a correction to it.

// emptyTillSec is how long a station stays off the list for one type. It is long enough to
// disperse a wave and short enough to pick up a restock.
const emptyTillSec = 10 * 60

// tillKey is one holder's knowledge of one station's stock of one type. The holder scopes it
// to intel the pilot could actually have: the corporation finding out its buyer came back
// empty, and the fleet on the same shopping trip.
type tillKey struct {
	holder    int64
	typeID    int64
	stationID int64
}

var (
	emptyTillMu sync.Mutex
	emptyTills  = map[tillKey]int64{} // to unix expiry
)

// noteTillEmpty records that a pilot stood at this station and could not buy this type.
// Call it from the till, not from the scan: it is ground truth, not another read of the cache.
func noteTillEmpty(sc intelScope, typeID, stationID int64) {
	if typeID == 0 || stationID == 0 {
		return
	}
	until := clock.Unix() + emptyTillSec
	emptyTillMu.Lock()
	for _, holder := range sc.holders() {
		emptyTills[tillKey{holder, typeID, stationID}] = until
	}
	emptyTillMu.Unlock()
}

// tillIsEmpty reports whether anyone this pilot hears from recently found this station out
// of this type.
func tillIsEmpty(sc intelScope, typeID, stationID int64) bool {
	now := clock.Unix()
	emptyTillMu.Lock()
	defer emptyTillMu.Unlock()
	for _, holder := range sc.holders() {
		if until, ok := emptyTills[tillKey{holder, typeID, stationID}]; ok && until > now {
			return true
		}
	}
	return false
}

// sweepEmptyTills drops expired entries. It runs on the module's cache sweep (cachesweep.go);
// the map is keyed by (type, station), too large a product to leave to read-through expiry.
func sweepEmptyTills() {
	now := clock.Unix()
	emptyTillMu.Lock()
	for k, until := range emptyTills {
		if until <= now {
			delete(emptyTills, k)
		}
	}
	emptyTillMu.Unlock()
}

func init() { registerSweep("emptyTills", sweepEmptyTills) }
