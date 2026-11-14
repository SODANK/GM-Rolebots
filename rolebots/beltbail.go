package rolebots

import (
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// Leaving a grid and coming back to work, for the embodied tier.
//
// A projection that leaves a grid before anything has shot at it (assessSightedGrid,
// assessWarInSystem) never enters an engagement, so the engagement's stand-down never
// releases its flee latch. settlePreemptiveFlee releases it once the ship has landed. The
// belt it left is remembered for a while (bailedBelts) so the next hop does not take it
// straight back, and a gang's guns are called to that belt (the belt alarm). A gang miner's
// next hop prefers its op belt once that is workable again. See
// docs/comment-notes-b03.md#pre-emptive-flee.

const (
	// beltBailSec is how long a projection stays off a belt it left for its rats.
	beltBailSec = 15 * 60
	// A pre-emptive flee is not judged before preFleeMinSec (the warp is still starting) and
	// is released at preFleeCapSec even if the ship never reached its destination.
	preFleeMinSec = 10
	preFleeCapSec = 90
	// preFleeRestSec is the pause a ship takes at its bolt-hole before it acts again.
	preFleeRestSec = 30
	// gangBeltAlarmSec is how long a gang's guns keep converging on a belt its miners left.
	gangBeltAlarmSec = 10 * 60
	// gridMateReachM is how near a gangmate must be to count as on the same grid.
	gridMateReachM = 250_000.0
)

// preFleeOver reports whether a pre-emptive flee is finished: the ship is out of warp and
// either stands at its flee destination or has been trying for preFleeCapSec. Pure.
func preFleeOver(warping, landed bool, since time.Duration) bool {
	if warping || since < preFleeMinSec*time.Second {
		return false
	}
	return landed || since >= preFleeCapSec*time.Second
}

// notePreFlee marks the flee that has just been ordered as pre-emptive: nothing was
// shooting, so no engagement will stand down and release it. A refused hop left
// eb.fleeing false and is not marked.
func notePreFlee(eb *embodiedBot) {
	eb.mu.Lock()
	if eb.fleeing && eb.preFleeAt.IsZero() {
		eb.preFleeAt = clock.Now()
	}
	eb.mu.Unlock()
}

// settlePreemptiveFlee releases the flee latch of a ship that left a grid unprovoked and
// has arrived. It runs on the combat loop's quiet tick, so anything shooting at the ship
// has already taken the engagement path instead. An engaged flee is left to standDown.
func (m *Mod) settlePreemptiveFlee(eb *embodiedBot) {
	if !preFleeDue(eb) {
		return
	}
	m.clearFlee(eb)
	rest := clock.Now().Add(preFleeRestSec * time.Second)
	eb.mu.Lock()
	if eb.nextActAt.Before(rest) {
		eb.nextActAt = rest
	}
	eb.mu.Unlock()
	m.logBot(eb.charID, logInfo, "threat", "clear of the grid it left — back to work")
}

// preFleeDue reports whether this projection holds a pre-emptive flee latch that is ready
// to be released (preFleeOver). False for an engaged flee, which carries no preFleeAt.
func preFleeDue(eb *embodiedBot) bool {
	eb.mu.Lock()
	pre, fleeing, warping := eb.preFleeAt, eb.fleeing, eb.warping
	landed := eb.fleeDest != 0 && eb.currentDest == eb.fleeDest
	eb.mu.Unlock()
	if !fleeing || pre.IsZero() {
		return false
	}
	return preFleeOver(warping, landed, clock.Now().Sub(pre))
}

// beltUnderfoot is the asteroid belt this projection is at or flying to: its destination
// when that is a belt, or the nearest belt in grid reach for a ship with no destination (a
// projection born parked on one). 0 when it is not at a belt.
func beltUnderfoot(eb *embodiedBot) int64 {
	if eb == nil {
		return 0
	}
	eb.mu.Lock()
	dest := eb.currentDest
	eb.mu.Unlock()
	if dest != 0 {
		if c := celestialByID(eb.celestials, dest); c != nil && c.GroupID == 9 {
			return dest
		}
		return 0
	}
	if eb.ent == nil {
		return 0
	}
	pos := eb.ent.Pos()
	var beltID int64
	bestD := 0.0
	for _, c := range eb.celestials {
		if c.GroupID != 9 {
			continue
		}
		d := pos.Sub(destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}).Len()
		if d <= gridMateReachM && (beltID == 0 || d < bestD) {
			beltID, bestD = c.ItemID, d
		}
	}
	return beltID
}

// noteBeltBail records that this projection is leaving the belt it stands on because of
// what is there, and raises its gang's belt alarm. It reports the belt, 0 when the ship is
// not at one.
func noteBeltBail(eb *embodiedBot) int64 {
	beltID := beltUnderfoot(eb)
	if beltID == 0 {
		return 0
	}
	now := clock.Now()
	eb.mu.Lock()
	if eb.bailedBelts == nil {
		eb.bailedBelts = map[int64]time.Time{}
	}
	eb.bailedBelts[beltID] = now
	eb.mu.Unlock()
	if eb.gangID != 0 {
		raiseGangBeltAlarm(eb.gangID, beltID)
	}
	return beltID
}

// bailedNow is the set of belts this projection is still keeping off: bailed inside
// beltBailSec and not cleared by its gang's guns since.
func bailedNow(eb *embodiedBot) map[int64]bool {
	now := clock.Now()
	eb.mu.Lock()
	defer eb.mu.Unlock()
	var out map[int64]bool
	for beltID, at := range eb.bailedBelts {
		if now.Sub(at) >= beltBailSec*time.Second || gangBeltClearedSince(eb.gangID, beltID, at) {
			delete(eb.bailedBelts, beltID)
			continue
		}
		if out == nil {
			out = map[int64]bool{}
		}
		out[beltID] = true
	}
	return out
}

// The gang's shared belt memos, all in memory: they describe a grid a player is watching
// right now and are worth nothing after a restart.
type gangBeltMark struct {
	beltID int64
	at     time.Time
}

var (
	gangBeltMu      sync.Mutex
	gangBeltAlarms  = map[int64]gangBeltMark{}        // gangID → belt its miners left
	gangBeltCleared = map[int64]map[int64]time.Time{} // gangID → belt → when its guns cleared it
	gangOpBelts     = map[int64]int64{}               // gangID → the op belt (gangMineStep)
)

// raiseGangBeltAlarm calls a gang's guns to a belt its miners have just left.
func raiseGangBeltAlarm(gangID, beltID int64) {
	gangBeltMu.Lock()
	gangBeltAlarms[gangID] = gangBeltMark{beltID: beltID, at: clock.Now()}
	gangBeltMu.Unlock()
}

// gangBeltAlarmFor is the belt a gang's guns should be on, 0 when no alarm is live.
func gangBeltAlarmFor(gangID int64) int64 {
	if gangID == 0 {
		return 0
	}
	gangBeltMu.Lock()
	defer gangBeltMu.Unlock()
	a, ok := gangBeltAlarms[gangID]
	if !ok {
		return 0
	}
	if clock.Now().Sub(a.at) >= gangBeltAlarmSec*time.Second {
		delete(gangBeltAlarms, gangID)
		return 0
	}
	return a.beltID
}

// noteGangBeltCleared records that a gang's guns have just cleared a belt: the alarm for
// it ends and the gang's miners may come back at once.
func noteGangBeltCleared(gangID, beltID int64) {
	if gangID == 0 || beltID == 0 {
		return
	}
	gangBeltMu.Lock()
	defer gangBeltMu.Unlock()
	if gangBeltAlarms[gangID].beltID == beltID {
		delete(gangBeltAlarms, gangID)
	}
	if gangBeltCleared[gangID] == nil {
		gangBeltCleared[gangID] = map[int64]time.Time{}
	}
	gangBeltCleared[gangID][beltID] = clock.Now()
}

// gangBeltClearedSince reports whether the gang's guns cleared this belt after the given time.
func gangBeltClearedSince(gangID, beltID int64, since time.Time) bool {
	if gangID == 0 {
		return false
	}
	gangBeltMu.Lock()
	defer gangBeltMu.Unlock()
	at, ok := gangBeltCleared[gangID][beltID]
	return ok && at.After(since)
}

// noteGangOpBelt remembers a mining op's belt for its projections (gangMineStep); 0 forgets it.
func noteGangOpBelt(gangID, beltID int64) {
	if gangID == 0 {
		return
	}
	gangBeltMu.Lock()
	defer gangBeltMu.Unlock()
	if beltID == 0 {
		delete(gangOpBelts, gangID)
		return
	}
	gangOpBelts[gangID] = beltID
}

// gangOpBeltFor is the op belt of a mining gang, 0 when unknown.
func gangOpBeltFor(gangID int64) int64 {
	if gangID == 0 {
		return 0
	}
	gangBeltMu.Lock()
	defer gangBeltMu.Unlock()
	return gangOpBelts[gangID]
}

// forgetGangBelts drops every belt memo of a gang (deleteGang).
func forgetGangBelts(gangID int64) {
	gangBeltMu.Lock()
	delete(gangBeltAlarms, gangID)
	delete(gangBeltCleared, gangID)
	delete(gangOpBelts, gangID)
	gangBeltMu.Unlock()
}

// gangGunOnGrid reports whether this pilot's gang's fighting ships on its grid, or in warp to
// the belt it stands on, can cover it. A miner with its escort beside it leaves the rats to
// the escort instead of leaving the belt.
//
// th is the spawn the miner is looking at. With it, the escorts' damage and hit points are
// pooled and the veto stands only when they win that fight (escortCovers): one frigate does
// not hold a belt a cruiser pack has taken over. An escort whose loadout is not resolved yet
// (it has not fired a shot, combat_embodied.go) is taken as cover, as it was before th
// existed. A nil th skips the strength test and counts any escort on the grid.
func gangGunOnGrid(eb *embodiedBot, th *spawnThreat) bool {
	if eb == nil || eb.gangID == 0 || eb.ent == nil {
		return false
	}
	pos := eb.ent.Pos()
	belt := beltUnderfoot(eb)
	embodyMu.Lock()
	mates := make([]*embodiedBot, 0, len(embodyLive))
	for _, mate := range embodyLive {
		mates = append(mates, mate)
	}
	embodyMu.Unlock()
	var dps, ehp float64
	found := false
	for _, mate := range mates {
		if mate == eb || mate.gangID != eb.gangID || mate.systemID != eb.systemID || !mate.patrol {
			continue
		}
		mate.mu.Lock()
		gone := mate.dead || mate.fleeing
		inbound := mate.warping && belt != 0 && mate.currentDest == belt
		load := mate.load
		mate.mu.Unlock()
		if gone || mate.ent == nil || mate.ent.IsDestroyed() {
			continue
		}
		if !inbound && mate.ent.Pos().Sub(pos).Len() > gridMateReachM {
			continue
		}
		if th == nil || load == nil {
			return true
		}
		found = true
		dps += load.dps()
		hp := mate.ent.HPSnapshot()
		ehp += hp.CurShield + hp.CurArmor + hp.CurStructure
	}
	return found && escortCovers(dps, ehp, *th)
}

// escortCovers reports whether a gang's pooled escort, dps and hit points together, wins the
// fight against th (threatMargin at or above 1). The pool is optimistic, since rats pick one
// target and not the sum of them; it is the same reading a lone gunned hull gets. Pure.
func escortCovers(dps, ehp float64, th spawnThreat) bool {
	return threatMargin(dps, ehp, th) >= 1
}

// wanderCandidates is where an idle projection may warp next. A miner hops belts only,
// skipping the ones it found dry, and takes its preferred belt (a gang's op belt) when that
// is open to it. Nobody is sent to a belt it has just bailed from, not even by the
// fall-backs that revisit dry belts or wander anywhere. Pure.
func wanderCandidates(cels []evedb.CelestialEntity, current int64, mining bool,
	dry, bailed map[int64]bool, prefer int64) []evedb.CelestialEntity {
	if mining && prefer != 0 && prefer != current && !dry[prefer] && !bailed[prefer] {
		if c := celestialByID(cels, prefer); c != nil && c.GroupID == 9 {
			return []evedb.CelestialEntity{*c}
		}
	}
	var out []evedb.CelestialEntity
	for _, c := range cels {
		if c.ItemID == current || !wanderable(c.GroupID) || bailed[c.ItemID] {
			continue
		}
		if mining && (c.GroupID != 9 || dry[c.ItemID]) {
			continue // miners hop belts only, skipping ones they found empty
		}
		out = append(out, c)
	}
	if len(out) == 0 && mining {
		// Every belt has come up dry: revisit them anyway (rocks respawn).
		for _, c := range cels {
			if c.ItemID != current && c.GroupID == 9 && !bailed[c.ItemID] {
				out = append(out, c)
			}
		}
	}
	if len(out) == 0 {
		// A miner in a beltless system (or all filtered): wander anything.
		for _, c := range cels {
			if c.ItemID != current && wanderable(c.GroupID) && !bailed[c.ItemID] {
				out = append(out, c)
			}
		}
	}
	return out
}

// nearestFleeTarget picks where a ship at pos runs to: the nearest station that is a real
// warp away (farther than minRange), else the nearest other celestial that is. The grid
// being left (from) and stars are never picked. When nothing is a warp away it falls back
// to the first station, then the first other celestial, in list order. Nil when the
// system offers nothing. Pure.
func nearestFleeTarget(cels []evedb.CelestialEntity, pos destiny.Vec3, from int64, minRange float64) *evedb.CelestialEntity {
	var station, other, anyStation, anyOther *evedb.CelestialEntity
	var stationD, otherD float64
	for i := range cels {
		c := &cels[i]
		if c.ItemID == from || c.GroupID == 6 {
			continue
		}
		isStation := c.GroupID == groupStation
		if isStation && anyStation == nil {
			anyStation = c
		}
		if anyOther == nil {
			anyOther = c
		}
		d := pos.Sub(destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}).Len()
		if d <= minRange {
			continue
		}
		if isStation {
			if station == nil || d < stationD {
				station, stationD = c, d
			}
			continue
		}
		if other == nil || d < otherD {
			other, otherD = c, d
		}
	}
	switch {
	case station != nil:
		return station
	case other != nil:
		return other
	case anyStation != nil:
		return anyStation
	}
	return anyOther
}

func init() {
	registerSweep("gangBeltMemos", func() {
		now := clock.Now()
		gangBeltMu.Lock()
		defer gangBeltMu.Unlock()
		for gangID, a := range gangBeltAlarms {
			if now.Sub(a.at) >= gangBeltAlarmSec*time.Second {
				delete(gangBeltAlarms, gangID)
			}
		}
		for gangID, belts := range gangBeltCleared {
			for beltID, at := range belts {
				if now.Sub(at) >= beltBailSec*time.Second {
					delete(belts, beltID)
				}
			}
			if len(belts) == 0 {
				delete(gangBeltCleared, gangID)
			}
		}
	})
}
