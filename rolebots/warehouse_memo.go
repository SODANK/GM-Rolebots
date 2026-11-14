package rolebots

// The warehouse run's memory of its far stations, and what a hoard there is really worth.
//
// The memo is persisted (StickyState.WarehouseMemo) so a restart keeps it. It backs a station off by
// doubling after each visit that disposed of nothing, and reopens one early only when its hoard has
// grown. warehouseTypeValue prices a hoard at what the far-end sweeps would actually get, not basePrice.

const (
	warehouseNoopBackoffKey     = "rolebots.warehouse.noopBackoffMaxDays"
	warehouseValueGrowthKey     = "rolebots.warehouse.valueGrowth"
	warehouseNoopBackoffDefault = 7
	warehouseValueGrowthDefault = 1.25

	// warehouseMemoMaxAge and warehouseMemoMax bound the memo per pilot.
	warehouseMemoMaxAge = 30 * 86400
	warehouseMemoMax    = 32

	// warehouseRealStations and warehouseRealTypes bound the survey's
	// second stage: the dearest stations by basePrice, and their dearest
	// types, are re-priced at what they would really fetch.
	warehouseRealStations = 3
	warehouseRealTypes    = 12
)

// warehouseMemo is one far station's entry.
type warehouseMemo struct {
	Until   int64 `json:"u,omitempty"` // not tried again before this (unix)
	Strikes int   `json:"n,omitempty"` // visits in a row that disposed of nothing
	Value   int64 `json:"v,omitempty"` // the hoard's value at the last look
	At      int64 `json:"a,omitempty"` // when the entry was last written
}

// warehouseMemoHolds reports whether a station is still resting: before Until,
// unless its hoard has grown by growth since the last look. Pure.
func warehouseMemoHolds(e warehouseMemo, now int64, value, growth float64) bool {
	if now >= e.Until {
		return false
	}
	if e.Value > 0 && growth > 1 && value >= float64(e.Value)*growth {
		return false
	}
	return true
}

// warehouseMemoDepart is the entry stamped as the pilot sets off: the station
// rests one retry period so an interrupted trip is not re-drawn at once.
// Strikes carry over until the visit reports. Pure.
func warehouseMemoDepart(e warehouseMemo, now int64, value float64) warehouseMemo {
	e.Until, e.Value, e.At = now+warehouseRetrySec, int64(value), now
	return e
}

// warehouseMemoAfter is the entry once the far end has run: a visit that
// disposed of something resets the strikes and rests one retry period; an
// empty one adds a strike and rests retry × 2^strikes, capped at maxDays.
// Pure.
func warehouseMemoAfter(e warehouseMemo, now int64, disposed bool, maxDays int) warehouseMemo {
	e.At = now
	if disposed {
		e.Strikes = 0
		e.Until = now + warehouseRetrySec
		return e
	}
	e.Strikes++
	wait := int64(warehouseRetrySec)
	for i := 0; i < e.Strikes && wait < int64(maxDays)*86400; i++ {
		wait *= 2
	}
	if cap := int64(maxDays) * 86400; maxDays > 0 && wait > cap {
		wait = cap
	}
	e.Until = now + wait
	return e
}

// pruneWarehouseMemo drops entries older than warehouseMemoMaxAge that have
// nothing left to hold, and keeps at most warehouseMemoMax (the newest). Pure;
// returns nil for an empty memo so omitempty drops the key.
func pruneWarehouseMemo(memo map[int64]warehouseMemo, now int64) map[int64]warehouseMemo {
	for st, e := range memo {
		if now-e.At > warehouseMemoMaxAge && now >= e.Until {
			delete(memo, st)
		}
	}
	for len(memo) > warehouseMemoMax {
		oldest, at := int64(0), int64(-1)
		for st, e := range memo {
			if at < 0 || e.At < at {
				oldest, at = st, e.At
			}
		}
		delete(memo, oldest)
	}
	if len(memo) == 0 {
		return nil
	}
	return memo
}

// warehouseTypeValue is what units of one type at a far station would really fetch through the sweeps
// there: the bids that reach it (bidGross), else the region's absorption at the tape once history is
// ready, else scrap metal (refineUnit; 0 for named gear), half the tape for ammunition the cluster buys
// elsewhere, or nothing. Half the lower of tape and base before history is ready. Pure.
func warehouseTypeValue(units int64, bidGross, tape, base, regionPerDay float64, ready, commodity bool,
	refineUnit float64, ammo bool, clusterPerDay float64) float64 {

	if units <= 0 {
		return 0
	}
	if bidGross > 0 {
		return bidGross
	}
	price := tape
	if price <= 0 {
		price = base
	}
	if !ready {
		if base > 0 && base < price {
			price = base
		}
		return 0.5 * price * float64(units)
	}
	if regionPerDay > 0 {
		return price * float64(marketAbsorb(units, regionPerDay))
	}
	if ammo {
		if clusterPerDay > 0 {
			return 0.5 * price * float64(units)
		}
		return 0
	}
	if commodity {
		return 0
	}
	return refineUnit * float64(units)
}

// hangarFingerprintEqual reports whether two hangar fingerprints (type to units at one station) match.
// They are read before and after the far-end sweeps to tell whether the sweeps did anything.
func hangarFingerprintEqual(a, b map[int64]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for t, n := range a {
		if b[t] != n {
			return false
		}
	}
	return true
}
