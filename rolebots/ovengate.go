package rolebots

import "sync"

// The oven gate: when a pilot's factory or lab jobs can next ring.
//
// industryOvenSweep and labOvenSweep run at the top of every turn, and each asks ramJobs
// about the pilot's running jobs. A job only rings at its end time, so between rings the
// answer is "nothing yet" on every pass, for every industrialist. The sweep remembers the
// earliest end time it read and skips the query until then. The one change that does not
// keep to the clock is a job taken off the line from outside (cancelled, or delivered from
// the ESP factory page), so the gate also lapses after ovenRecheckSec.
//
// A gate is keyed to the job list it was read for: a job added or dropped changes the
// signature and the next sweep reads again. A sweep with a cut (the off-hours claim,
// tzclaim.go) neither reads nor writes the gate.

const (
	// ovenRecheckSec is the longest a sweep trusts a quiet answer.
	ovenRecheckSec = 300

	ovenKindIndustry = 1
	ovenKindLab      = 2
)

type ovenGateKey struct {
	kind   uint8
	charID int64
}

type ovenGateRec struct {
	sig    uint64
	nextAt int64 // clock second; the sweep reads again at or after it
}

var (
	ovenGateMu sync.Mutex
	ovenGate   = map[ovenGateKey]ovenGateRec{}
)

func init() {
	registerCharForget("ovenGate", func(id int64) {
		ovenGateMu.Lock()
		delete(ovenGate, ovenGateKey{ovenKindIndustry, id})
		delete(ovenGate, ovenGateKey{ovenKindLab, id})
		ovenGateMu.Unlock()
	})
}

// ovenSig is an order-free signature of a job list.
func ovenSig(ids []int64) uint64 {
	var sum, x uint64
	for _, id := range ids {
		h := uint64(id) * 0x9E3779B97F4A7C15
		h ^= h >> 31
		sum += h
		x ^= h
	}
	return sum ^ (x << 1) ^ uint64(len(ids))
}

// ovenGateShut reports whether the sweep may skip its query: the job list is the one the
// gate was read for and nothing on it can have rung yet.
func ovenGateShut(kind uint8, charID int64, ids []int64, now int64) bool {
	ovenGateMu.Lock()
	r, ok := ovenGate[ovenGateKey{kind, charID}]
	ovenGateMu.Unlock()
	return ok && now < r.nextAt && r.sig == ovenSig(ids)
}

// ovenGateNote records a quiet sweep. nextEndWin32 is the earliest end time still to come
// on the list (Win32 time), or 0 when the read gave none, which leaves only the recheck.
func ovenGateNote(kind uint8, charID int64, ids []int64, nextEndWin32, now int64) {
	next := now + ovenRecheckSec
	if nextEndWin32 > 0 {
		if at := win32ToUnixCeil(nextEndWin32); at < next {
			next = at
		}
	}
	ovenGateMu.Lock()
	ovenGate[ovenGateKey{kind, charID}] = ovenGateRec{sig: ovenSig(ids), nextAt: next}
	ovenGateMu.Unlock()
}

// ovenGateWake is the clock second a pilot's sweep of this kind next has to read: the
// gate's time while it is shut for this job list, else now.
func ovenGateWake(kind uint8, charID int64, ids []int64, now int64) int64 {
	ovenGateMu.Lock()
	r, ok := ovenGate[ovenGateKey{kind, charID}]
	ovenGateMu.Unlock()
	if ok && now < r.nextAt && r.sig == ovenSig(ids) {
		return r.nextAt
	}
	return now
}

// indOvenIDs is the job list industryOvenSweep asks about: installed and not yet delivered.
func indOvenIDs(d *StateData) []int64 {
	var ids []int64
	for _, j := range d.IndCooking {
		if !j.Delivered && j.JobID != 0 {
			ids = append(ids, j.JobID)
		}
	}
	return ids
}

// labOvenIDs is the job list labOvenSweep asks about.
func labOvenIDs(d *StateData) []int64 {
	var ids []int64
	for _, j := range d.LabCooking {
		if j.JobID != 0 {
			ids = append(ids, j.JobID)
		}
	}
	return ids
}

// ovenRung is the ring test both sweeps apply to a ramJobs row: delivered by the server's
// status ticker (by readyCut), taken off the line from outside, or past its end time (by
// dueCut) before the ticker flagged it.
func ovenRung(status, end, readyCut, dueCut int64) bool {
	return (status == 1 && end <= readyCut) ||
		status >= 100 ||
		(status == 0 && end > 0 && end <= dueCut)
}

// win32ToUnixCeil is win32ToUnix rounded up, so a gate never opens before the end time.
func win32ToUnixCeil(ft int64) int64 {
	return (ft+9_999_999)/10_000_000 - 11644473600
}
