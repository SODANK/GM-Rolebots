package rolebots

import "sync"

// The belt search ranks ore from destiny.OreDistribution (what can spawn at a
// system's security), while the rocks were rolled once from a stored seed
// (beltfield.go). A system that can carry Hemorphite need not have rolled any,
// so without a memory the plan keeps picking it and miningRealOre keeps finding
// none. This memory is about the system, not the pilot, so it is shared by the
// whole roster and holds no per-character state. It lapses on its own: fields
// are reseeded and rocks regrow on the asteroids.regenDays cycle, and a wrongly
// believed absence costs the search one trip's preference.

// oreAbsentTTLSec is how long an absence is believed. Short against the
// regeneration cycle (a day by default), long against a trip.
const oreAbsentTTLSec = 3 * 3600

// oreAbsentMax bounds the map: 5,322 belt systems exist, so this is generous.
const oreAbsentMax = 8000

type oreAbsence struct {
	names map[string]bool
	at    int64
}

var (
	oreAbsentMu sync.Mutex
	oreAbsent   = map[int64]oreAbsence{} // solar system to the ores its field does not hold
)

// learnOreAbsence records, for one system, every ore family that its field
// holds none of right now. avail is the field's units by family (familyUnits).
func learnOreAbsence(sysID int64, avail map[int64]int64) {
	absent := map[string]bool{}
	for familyID, name := range oreFamilyBases {
		if avail[familyID] <= 0 {
			absent[name] = true
		}
	}
	oreAbsentMu.Lock()
	defer oreAbsentMu.Unlock()
	if len(oreAbsent) >= oreAbsentMax {
		now := clock.Unix()
		for id, a := range oreAbsent {
			if now-a.at >= oreAbsentTTLSec {
				delete(oreAbsent, id)
			}
		}
		if len(oreAbsent) >= oreAbsentMax {
			return // nothing stale to make room with: stop learning rather than grow
		}
	}
	oreAbsent[sysID] = oreAbsence{names: absent, at: clock.Unix()}
}

// oreKnownAbsent returns the ores a system's field was recently found not to
// hold, or nil. The map it returns is read-only.
func oreKnownAbsent(sysID int64) map[string]bool {
	oreAbsentMu.Lock()
	defer oreAbsentMu.Unlock()
	a, ok := oreAbsent[sysID]
	if !ok {
		return nil
	}
	if clock.Unix()-a.at >= oreAbsentTTLSec {
		delete(oreAbsent, sysID)
		return nil
	}
	return a.names
}
