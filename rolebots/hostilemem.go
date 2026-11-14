package rolebots

import (
	"sync"
)

// Hostile-encounter memory: which systems have bitten a bot lately.
//
// The spawner census says where rats could be; this says where a rolebot got
// jumped while working. The belt search prices it in, so a miner that fled a
// spawn does not fly straight back. In memory only: a restart forgets grudges.
//
// It is corp intel. A grudge belongs to a holder: the corporation that learned
// it, and separately the fleet if the pilot was flying with one. A read checks
// both. The fleet key keeps mixed-corp gangs working, since pilots on one op
// share what the op runs into.

const (
	// How long an encounter stays on the map, measured from the last bite.
	hostileMemorySec = 45 * 60
	// How long it may stay measured from the first bite, however often renewed.
	// Without it a system that keeps biting never ages out, and hunters steer toward
	// a bitten system (bounty.go), so it would become a permanent lure.
	hostileMemoryMaxSec = 90 * 60
	// What a recent mauling costs a candidate system, in jumps, on top of the
	// ordinary could-spawn-here cost: steep, but not a wall.
	beltEncounterCost = 12
)

// Patrol cover, the other half of the intel map: a cleared spawn buys the system
// a quiet spell. It clears the bite, lets the belt search treat the system as
// quiet, and suppresses (not zeroes) the ambush roll (cf. gangPatrolCovers).
const (
	// How long a cleared system stays covered. Shorter than the grudge window, since
	// rats respawn.
	patrolCoverSec = 30 * 60
	// What is left of a working miner's encounter chance under cover.
	patrolCoverEncounterMul = 0.25
)

// hostileMark is one system's grudge. It has two timestamps because the window
// must decay under load as well as under quiet (hostileMemoryMaxSec).
type hostileMark struct {
	firstAt int64
	lastAt  int64
}

// live reports whether the mark is still current at now.
func (h hostileMark) live(now int64) bool {
	return now-h.lastAt <= hostileMemorySec && now-h.firstAt <= hostileMemoryMaxSec
}

// intelScope is who a piece of field intel belongs to: the pilot's employer and
// the fleet it flies with. Either may be 0; a scope with neither learns and
// remembers nothing.
type intelScope struct{ corpID, gangID int64 }

func botScope(b *botRun) intelScope {
	if b == nil {
		return intelScope{}
	}
	return intelScope{corpID: b.CorpID, gangID: b.GangID}
}

func ebScope(eb *embodiedBot) intelScope {
	if eb == nil {
		return intelScope{}
	}
	return intelScope{corpID: eb.corpID, gangID: eb.gangID}
}

func gangScope(g *Gang) intelScope {
	if g == nil {
		return intelScope{}
	}
	return intelScope{corpID: g.CorpID, gangID: g.GangID}
}

// holders is the keys this scope writes to and reads from, in order.
func (s intelScope) holders() []int64 {
	var out []int64
	if s.corpID != 0 {
		out = append(out, s.corpID)
	}
	if s.gangID != 0 {
		// Negated so a gangID never collides with a corporationID: the two come from
		// different allocators.
		out = append(out, -s.gangID)
	}
	return out
}

// intelKey is one holder's opinion of one system.
type intelKey struct {
	holder int64
	sysID  int64
}

var (
	hostileMemMu sync.Mutex
	hostileMem   = map[intelKey]hostileMark{} // (holder, system) to grudge
	patrolCover  = map[intelKey]int64{}       // (holder, system) to unix of last cleared spawn
)

// noteHostileEncounter records that a bot came under attack working sysID.
func noteHostileEncounter(sc intelScope, sysID int64) {
	if sysID == 0 {
		return
	}
	now := clock.Unix()
	hostileMemMu.Lock()
	for _, holder := range sc.holders() {
		k := intelKey{holder, sysID}
		h, ok := hostileMem[k]
		if !ok {
			// A note never extends firstAt on an existing mark; that cap stops a system
			// that keeps biting from renewing its own window forever.
			h.firstAt = now
		}
		h.lastAt = now
		hostileMem[k] = h
	}
	hostileMemMu.Unlock()
}

// noteHostilePartial is what a fight that went well enough does to the map: it
// lifts the grudge without granting cover, so the system stops steering hunters
// in and miners away, but nobody is told the belt is clear.
func noteHostilePartial(sc intelScope, sysID int64) {
	if sysID == 0 {
		return
	}
	hostileMemMu.Lock()
	for _, holder := range sc.holders() {
		delete(hostileMem, intelKey{holder, sysID})
	}
	hostileMemMu.Unlock()
}

// notePatrolCover records a cleared spawn in sysID and lifts the system's
// standing grudge: a dead spawn answers a bite better than waiting for the memory
// to fade.
func notePatrolCover(sc intelScope, sysID int64) {
	if sysID == 0 {
		return
	}
	now := clock.Unix()
	hostileMemMu.Lock()
	for _, holder := range sc.holders() {
		k := intelKey{holder, sysID}
		patrolCover[k] = now
		delete(hostileMem, k)
	}
	hostileMemMu.Unlock()
}

// patrolCovered reports whether sysID has had its spawn cleared inside the
// cover window.
func patrolCovered(sc intelScope, sysID int64) bool {
	now := clock.Unix()
	hostileMemMu.Lock()
	defer hostileMemMu.Unlock()
	for _, holder := range sc.holders() {
		k := intelKey{holder, sysID}
		at, ok := patrolCover[k]
		if !ok {
			continue
		}
		if now-at > patrolCoverSec {
			delete(patrolCover, k)
			continue
		}
		return true
	}
	return false
}

// sweepHostileMem forgets encounters past the window. recentHostileEncounter
// drops an expired entry when asked, which is enough in the common case; this
// covers a system nobody flies back to.
func sweepHostileMem() {
	now := clock.Unix()
	hostileMemMu.Lock()
	for k, h := range hostileMem {
		if !h.live(now) {
			delete(hostileMem, k)
		}
	}
	for k, at := range patrolCover {
		if now-at > patrolCoverSec {
			delete(patrolCover, k)
		}
	}
	hostileMemMu.Unlock()
}

// recentHostileEncounter reports whether any bot has been attacked in sysID
// inside the memory window.
func recentHostileEncounter(sc intelScope, sysID int64) bool {
	now := clock.Unix()
	hostileMemMu.Lock()
	defer hostileMemMu.Unlock()
	for _, holder := range sc.holders() {
		k := intelKey{holder, sysID}
		h, ok := hostileMem[k]
		if !ok {
			continue
		}
		if !h.live(now) {
			delete(hostileMem, k)
			continue
		}
		return true
	}
	return false
}

func init() {
	// A holder is a corpID or -gangID; only the corporation side is ever purged,
	// since a gang disbands and its marks age out.
	registerCorpForget("hostileMem/patrolCover", func(id int64) {
		hostileMemMu.Lock()
		for k := range hostileMem {
			if k.holder == id {
				delete(hostileMem, k)
			}
		}
		for k := range patrolCover {
			if k.holder == id {
				delete(patrolCover, k)
			}
		}
		hostileMemMu.Unlock()
	})
	registerSweep("hostileMem", sweepHostileMem)
}
