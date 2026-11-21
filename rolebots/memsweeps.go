package rolebots

// Housekeeping for caches whose read path checks an age but nothing ever deleted: an entry whose
// key is never asked for again (a corporation set that changed, a gang that disbanded, a victim
// long gone, a purged pilot) was held for the life of the process.

import (
	"sync"
	"time"
)

func init() {
	registerSweep("siegeCapacityOf", func() {
		siegeCapacityMu.Lock()
		for k, e := range siegeCapacityOf {
			if time.Since(e.at) >= siegeCapacityTTL {
				delete(siegeCapacityOf, k)
			}
		}
		siegeCapacityMu.Unlock()
	})
	registerSweep("siegePilotDPSOf", func() {
		siegePilotDPSMu.Lock()
		for k, e := range siegePilotDPSOf {
			if time.Since(e.at) >= siegePilotDPSTTL {
				delete(siegePilotDPSOf, k)
			}
		}
		siegePilotDPSMu.Unlock()
	})
	registerCharForget("siegePilotDPSOf/siegeAmmoDone", func(charID int64) {
		siegePilotDPSMu.Lock()
		delete(siegePilotDPSOf, charID)
		siegePilotDPSMu.Unlock()
		siegeAmmoDone.Delete(charID)
	})
	registerCharForget("tzRows", func(charID int64) {
		tzMu.Lock()
		delete(tzRows, charID)
		tzMu.Unlock()
	})
	registerSweep("gankStamps", func() {
		now := clock.Unix()
		sweepUnixStamps(&gankAbstractRefused, now, 600)
		sweepUnixStamps(&gankSafeMaking, now, gankSafeClaimSec)
	})
	registerSweep("beltFieldCache", func() {
		now := clock.Unix()
		beltFieldMu.Lock()
		for k, f := range beltFieldCache {
			if now-f.builtAt >= beltFieldCacheSec {
				delete(beltFieldCache, k)
			}
		}
		beltFieldMu.Unlock()
	})
}

// sweepUnixStamps deletes the entries of a sync.Map of unix-second stamps that are at least
// maxAge old.
func sweepUnixStamps(m *sync.Map, now, maxAge int64) {
	m.Range(func(k, v any) bool {
		if at, ok := v.(int64); ok && now-at >= maxAge {
			m.Delete(k)
		}
		return true
	})
}
