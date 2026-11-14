package rolebots

import (
	"sync"
	"time"
)

// Speculation claims record who is already carrying what to which region, so pilots sizing
// the same lot against the same market history do not all chase it. A claim is held from
// commitment until the pilot lists, abandons, or specClaimTTL passes. marketRoom subtracts
// the region's listings and every other pilot's claim from a week of its buying. Claims live
// in memory only; a restart drops every run in flight anyway.

// specClaimTTL is how long a claim stands with nobody releasing it: a long
// speculative haul, door to door.
const specClaimTTL = 3 * time.Hour

type specClaim struct {
	units int64
	until int64
}

type specClaimBook struct {
	mu sync.Mutex
	// (typeID, regionID) to charID to claim. One claim per pilot: a pilot's new run
	// replaces its old one.
	by map[[2]int64]map[int64]specClaim
	// charID to the key its claim sits under, so a release needs no key.
	of map[int64][2]int64
}

var specClaims = &specClaimBook{
	by: map[[2]int64]map[int64]specClaim{},
	of: map[int64][2]int64{},
}

// claim records charID carrying units of typeID to regionID, replacing any
// claim the pilot already held.
func (c *specClaimBook) claim(charID, typeID, regionID, units int64, now int64) {
	if charID == 0 || typeID == 0 || regionID == 0 || units <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(charID)
	k := [2]int64{typeID, regionID}
	if c.by[k] == nil {
		c.by[k] = map[int64]specClaim{}
	}
	c.by[k][charID] = specClaim{units: units, until: now + int64(specClaimTTL/time.Second)}
	c.of[charID] = k
}

// release drops whatever claim charID holds.
func (c *specClaimBook) release(charID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(charID)
}

func (c *specClaimBook) releaseLocked(charID int64) {
	k, ok := c.of[charID]
	if !ok {
		return
	}
	delete(c.of, charID)
	if m := c.by[k]; m != nil {
		delete(m, charID)
		if len(m) == 0 {
			delete(c.by, k)
		}
	}
}

// inFlight is the units every pilot but exceptChar is carrying of typeID to
// regionID. Expired claims are dropped as they are met.
func (c *specClaimBook) inFlight(typeID, regionID, exceptChar int64, now int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := [2]int64{typeID, regionID}
	var n int64
	for ch, cl := range c.by[k] {
		if cl.until <= now {
			delete(c.by[k], ch)
			delete(c.of, ch)
			continue
		}
		if ch != exceptChar {
			n += cl.units
		}
	}
	if len(c.by[k]) == 0 {
		delete(c.by, k)
	}
	return n
}

// marketRoom is how many of want units a speculation may carry into a
// region: a week of what the region buys (marketAbsorb), less what is
// already listed there and less what other pilots are already carrying in.
// Zero when the region has bought none or the week is spoken for. Pure.
func marketRoom(want int64, regionPerDay float64, listed, inFlight int64) int64 {
	room := marketAbsorb(want, regionPerDay)
	if room <= 0 {
		return 0
	}
	week := int64(regionPerDay*mktAbsorbDays) - max(listed, 0) - max(inFlight, 0)
	if week < room {
		room = week
	}
	return max(room, 0)
}

// specStationRoom is how many of want units a speculation may list at a dock
// other sellers already stock: what the dock is expected to take (a week of
// its own buying, or its factories' want) less the depth already resting
// there. Zero when the queue covers it. Pure.
func specStationRoom(want int64, demand float64, depth int64) int64 {
	room := int64(demand) - max(depth, 0)
	return max(min(want, room), 0)
}
