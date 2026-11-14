package rolebots

import (
	"sync"

	"gurimoon/destiny"
	"gurimoon/modapi"
)

// The global criminal flag travels with the pilot.
// The core keeps crimewatch per system and moves a player's flag on the jump path
// (carryCriminalFlag). A projection leaves through deEmbody and is born through
// installProjection, so deEmbody writes the flag's start time here and installProjection hands
// it back through CriminalEntered. The ledger also answers criminalSince where no table exists,
// which is every system the abstract tier fights in.

var (
	carriedCriminalMu sync.Mutex
	// carriedCriminal[charID] is the unix start of the global criminal flag
	// the pilot's last ball carried out of a loaded system.
	carriedCriminal = map[int64]int64{}
)

// rememberCriminal records a flag start for charID, keeping the newer of two.
func rememberCriminal(charID, since int64) {
	if charID <= 0 || since <= 0 {
		return
	}
	carriedCriminalMu.Lock()
	if since > carriedCriminal[charID] {
		carriedCriminal[charID] = since
	}
	carriedCriminalMu.Unlock()
}

// carriedCriminalSince is the remembered flag start for charID, or 0 when
// there is none or it has lapsed by now.
func carriedCriminalSince(charID, now int64) int64 {
	carriedCriminalMu.Lock()
	defer carriedCriminalMu.Unlock()
	since, ok := carriedCriminal[charID]
	if !ok {
		return 0
	}
	if now-since >= criminalFlagSec {
		delete(carriedCriminal, charID)
		return 0
	}
	return since
}

// noteCriminalLeaving writes down the flag a ball is carrying out of its
// system. Call before the ball leaves, while the system's table still holds it.
func noteCriminalLeaving(sysID, charID int64) {
	if since := coreCriminalSince(sysID, charID); since > 0 {
		rememberCriminal(charID, since)
	}
}

// raiseCarriedCriminal hands a remembered flag to the core for a ball that has
// just entered ss.
func raiseCarriedCriminal(e *destiny.Entity, ss *destiny.SolarSystem, charID int64) {
	enter := modapi.PvP().CriminalEntered
	if enter == nil {
		return
	}
	if since := carriedCriminalSince(charID, clock.Unix()); since > 0 {
		enter(e, ss, since)
	}
}

// criminalSince is the start of the pilot's global criminal flag: the core's crimewatch table
// for sysID or the flag carried out of another system, whichever is newer. Zero for a clean
// pilot, and zero on a core without the seam, which fails toward not shooting.
func criminalSince(sysID, charID int64) int64 {
	since := coreCriminalSince(sysID, charID)
	if carried := carriedCriminalSince(charID, clock.Unix()); carried > since {
		return carried
	}
	return since
}

// coreCriminalSince asks the core's crimewatch table for sysID only.
func coreCriminalSince(sysID, charID int64) int64 {
	h := modapi.PvP()
	if h.CriminalSince == nil {
		return 0
	}
	return h.CriminalSince(sysID, charID)
}

// sweepCarriedCriminal drops lapsed flags.
func sweepCarriedCriminal() {
	now := clock.Unix()
	carriedCriminalMu.Lock()
	for id, since := range carriedCriminal {
		if now-since >= criminalFlagSec {
			delete(carriedCriminal, id)
		}
	}
	carriedCriminalMu.Unlock()
}

func init() {
	registerCharForget("carriedCriminal", func(id int64) {
		carriedCriminalMu.Lock()
		delete(carriedCriminal, id)
		carriedCriminalMu.Unlock()
	})
	registerSweep("carriedCriminal", sweepCarriedCriminal)
}
