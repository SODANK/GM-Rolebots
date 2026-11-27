package rolebots

// Crimewatch for bots: the core's per-character PvP timers (core pvptimers.go), read and
// written through modapi.PvP() so a bot lives under the same dock lock, gate lock and logoff
// window a player does.
// Any PvP act locks the aggressor out of docks and stargates for crimewatch.aggressionLockSec
// (60 s). dockAt and travelStep ask crimewatchLocked first and hold while it runs.
// A pilot in PvP within crimewatch.pvpLogoffMin (15 min) may not leave the game (tzReady).
// Embodied shots stamp the timers through NotePlayerAggression; the abstract tier stamps
// through stampAbstractPvP. departCommit marks a projection departing and stands its guns
// down; actOnPilot wraps every act on a pilot.

import (
	"gurimoon/destiny"
	"gurimoon/modapi"
)

// crimewatchRetrySec is how long a locked bot waits before asking again.
const crimewatchRetrySec = 5

// crimewatchLocked reports whether charID may not dock or jump yet.
func crimewatchLocked(charID int64) bool {
	if f := modapi.PvP().AggressionLocked; f != nil {
		return f(charID)
	}
	return false
}

// botInPvP reports whether charID was in PvP within the logoff window.
func botInPvP(charID int64) bool {
	if f := modapi.PvP().InPvP; f != nil {
		return f(charID)
	}
	return false
}

// departCommit is the last check before a bot docks or jumps. Under the
// projection's aggrMu it asks the lock again and, if clear, marks the ball
// departing, stops its combat loop and puts its drones back in the bay.
// Reports false while locked. An unprojected bot only asks the lock.
func (m *Mod) departCommit(charID int64) bool {
	eb := liveProjection(charID)
	if eb == nil {
		return !crimewatchLocked(charID)
	}
	eb.aggrMu.Lock()
	if crimewatchLocked(charID) {
		eb.aggrMu.Unlock()
		return false
	}
	eb.mu.Lock()
	eb.departing = true
	eb.mu.Unlock()
	eb.aggrMu.Unlock()
	m.stopEmbodyCombat(eb)
	m.recallDronesInstant(eb)
	return true
}

// departAborted undoes departCommit after a dock or jump that failed: the
// mark comes off and the combat guard goes back on watch.
func (m *Mod) departAborted(charID int64) {
	eb := liveProjection(charID)
	if eb == nil {
		return
	}
	eb.mu.Lock()
	was := eb.departing
	eb.departing = false
	eb.mu.Unlock()
	if was {
		m.startEmbodyGuard(eb)
	}
}

// actOnPilot runs one aggressive act against a pilot (a hit, a point, a
// web) unless the projection is departing, and reports whether it ran. The
// act's own crimewatch stamp happens inside, under aggrMu, so departCommit
// sees it or the act sees the departure, never neither.
func (eb *embodiedBot) actOnPilot(act func()) bool {
	eb.aggrMu.Lock()
	defer eb.aggrMu.Unlock()
	eb.mu.Lock()
	departing := eb.departing
	eb.mu.Unlock()
	if departing {
		return false
	}
	act()
	return true
}

// notePilotAggression is the law for one aggressive act of a projection on a pilot (bot or
// player): the core's crimewatch entry, through the function a player's guns use, then its
// legality ladder, which raises the global criminal flag, the gate and station guns and CONCORD
// for an unlawful act. Called when the act starts (a gun cycle, a drone order, an ewar cycle),
// not when it lands, so the victim may answer at once; repeat calls are cheap (the core
// throttles the rebroadcast and charges once per pair). The abstract tier's engagement ledger
// hears it too, so a victim who later meets this bot off-grid may still answer lawfully.
// Callers on a pilot hold actOnPilot.
func (m *Mod) notePilotAggression(eb *embodiedBot, target *destiny.Entity) {
	if eb == nil || eb.ent == nil || eb.ss == nil || target == nil || !target.IsPlayer {
		return
	}
	h := modapi.PvP()
	if h.NotePlayerAggression != nil {
		h.NotePlayerAggression(eb.ent, target, eb.ss)
	}
	if h.PoliceCrime != nil {
		h.PoliceCrime(eb.ent, target, eb.ss)
	}
	noteEngagement(eb.charID, target.Slim.CharID, target.Slim.CorpID)
}

// stampAbstractPvP records an abstract-tier PvP act: aggressor's lock, both
// pilots' PvP time. victimCharID may be 0.
func stampAbstractPvP(aggressorCharID, victimCharID int64) {
	if aggressorCharID == 0 {
		return
	}
	if f := modapi.PvP().StampPvP; f != nil {
		f(aggressorCharID, victimCharID)
	}
}
