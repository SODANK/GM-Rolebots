package rolebots

import (
	"sync"
	"time"
)

// The roster pass's wake list: which pilots need a turn this pass.
//
// A turn that ends with the pilot partway through a timed leg changes nothing until the
// leg ends. Loading that pilot (state blob, profile, traits, ship) every pass only to
// reach the leg-wait check in advance() is most of what the pass costs on a large
// roster. So each roster turn ends by recording when the pilot can next have work
// (rosterWakeAt), and the pass loads and steps only pilots whose wake time has come.
//
// The wake is the earliest of everything advance() and stepBot act on by the clock: the
// leg's end, the end of an undock run (settleUndock), the next possible ring of a factory
// or lab job (ovengate.go), the next abstract exposure roll (combat_abstract.go) and a
// halt's retry. The rest of advance() is driven by events, and those wake the pilot:
// a gang member and a pilot serving a player are always due, a re-dock request makes it
// due, and any write of its state outside its own turn (saveState from another lane or
// the panel, resetBot) drops its record. A pilot with no record is due. As a backstop
// against anything not listed, every pilot still gets a turn at least every
// rosterSweepSec.

// rosterSweepSec is the longest a pilot goes without a roster turn.
const rosterSweepSec = 60

type rosterWakeRec struct {
	at     int64 // clock second the pilot can next have work
	turnAt int64 // clock second of its last roster turn
}

var (
	rosterWakeMu sync.Mutex
	rosterWake   = map[int64]rosterWakeRec{}
)

func init() {
	registerCharForget("rosterWake", rosterWakeClear)
}

// rosterWakeClear makes a pilot due next pass.
func rosterWakeClear(charID int64) {
	rosterWakeMu.Lock()
	delete(rosterWake, charID)
	rosterWakeMu.Unlock()
}

// noteRosterWake records the end of a roster turn.
func noteRosterWake(charID, at, now int64) {
	rosterWakeMu.Lock()
	rosterWake[charID] = rosterWakeRec{at: at, turnAt: now}
	rosterWakeMu.Unlock()
}

// rosterDue reports whether a roster run needs a turn this pass. It reads only what the
// roster query carries, so it runs before the load.
func rosterDue(b *botRun, now int64) bool {
	// Already loaded for another reason (logging back in, the cap's ranking): stepping it
	// costs no load, and a returning pilot's clocks were just moved (tzResume).
	if b.State != nil {
		return true
	}
	// Gang duty and a player's orders arrive between legs (gangCallUp, conscriptCallUp).
	if b.GangID != 0 || b.BossID != 0 {
		return true
	}
	if _, ok := redockRequests.Load(b.CharID); ok {
		return true
	}
	rosterWakeMu.Lock()
	r, ok := rosterWake[b.CharID]
	rosterWakeMu.Unlock()
	return !ok || now >= r.at || now-r.turnAt >= rosterSweepSec
}

// rosterWakeAt is when a pilot that has just had its turn can next have work. now means
// next pass.
func (m *Mod) rosterWakeAt(b *botRun, now int64) int64 {
	st := b.State
	at := now + rosterSweepSec
	soonest := func(t int64) {
		if t < at {
			at = t
		}
	}
	if st == nil {
		return now
	}
	if st.Halted {
		soonest(st.LastErrorAt + haltRetrySec)
		return max(at, now)
	}
	d := &st.Data
	// No timed leg: the phase polls, so it is stepped every pass as before.
	if d.LegEndsAt <= 0 {
		return now
	}
	soonest(d.LegEndsAt)
	if d.UndockStationID != 0 && b.StationID == 0 {
		soonest(d.UndockRunUntil)
	}
	if ids := indOvenIDs(d); len(ids) > 0 {
		soonest(ovenGateWake(ovenKindIndustry, b.CharID, ids, now))
	}
	if ids := labOvenIDs(d); len(ids) > 0 {
		soonest(ovenGateWake(ovenKindLab, b.CharID, ids, now))
	}
	if d.SiteID == 0 && m.exposedInSpace(b) {
		exposureMu.Lock()
		last, seen := exposureAt[b.CharID]
		exposureMu.Unlock()
		if !seen {
			soonest(now)
		} else {
			soonest(last.Add(abstractExposureStep).Add(time.Second - 1).Unix())
		}
	}
	return max(at, now)
}
