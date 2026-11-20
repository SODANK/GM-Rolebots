package rolebots

import "sync/atomic"

// Leg catch-up: a late turn does not cost the pilot its lateness.
//
// Every timed leg is set as "now + how long it takes" (126 sites, from travel jumps to
// station dwells). "now" is when the turn ran, not when the previous leg ended, so a
// pilot whose turn came round 40 s after its leg ended started its next leg 40 s late,
// every leg. On a roster large enough that the pass takes a minute, pilots ran at
// roughly half the speed their timers describe.
//
// The fix is one step, not 126 edits. advance() arms the turn when it was due because
// its leg ended (armLegCatchUp); the first save of that turn that carries a new leg pulls
// the leg back by the lateness (applyLegCatchUp), so the leg is timed from the moment the
// previous one ended. A leg pulled into the past is simply due next pass, and the chain
// catches up a step at a time.
//
// What is not caught up:
//   - time the pilot was not on the server: lateness is counted from when it last logged
//     in (popOnlineAt) and from when the sim started, so a cap bench, its hours away and
//     a server restart are never repaid (tzResume already moves clocks for its hours);
//   - more than legCatchUpMaxSec, so a stall is not repaid as a burst;
//   - the grid lane: a pilot a player can see is stepped every second and its legs are
//     what the player watches;
//   - a wait on a marker the leg was set to outlast (an undock run, a loiter, a tower
//     stay): the leg is never pulled in ahead of it.

// legCatchUpMaxSec is the most lateness one leg repays.
const legCatchUpMaxSec = 120

// legCatchUpSince is the clock second the sim started: lateness before it was downtime.
var legCatchUpSince atomic.Int64

// legCatchUp is what one turn may repay: the leg end it was late for, and by how much.
type legCatchUp struct {
	from, lag int64
}

// armLegCatchUp arms b's turn if it was due because its timed leg ended before now.
func armLegCatchUp(b *botRun, now int64) {
	st := b.State
	st.catchUp = legCatchUp{}
	from := st.Data.LegEndsAt
	if b.fastLane || from <= 0 || from >= now {
		return
	}
	lag := now - from
	if since := legCatchUpSince.Load(); since > 0 {
		lag = min(lag, now-since)
	} else {
		return // no start recorded (a harness Mod): nothing is known to be lateness
	}
	popMu.Lock()
	on := popOnlineAt[b.CharID]
	popMu.Unlock()
	if on > 0 {
		lag = min(lag, now-on)
	}
	lag = min(lag, legCatchUpMaxSec)
	if lag > 0 {
		st.catchUp = legCatchUp{from: from, lag: lag}
	}
}

// disarmLegCatchUp ends the turn's catch-up, applied or not.
func (st *SimState) disarmLegCatchUp() { st.catchUp = legCatchUp{} }

// applyLegCatchUp pulls a leg the armed turn just started back by the turn's lateness,
// once. A save that carries no new leg (an earlier write in the same turn, or a leg
// cleared to 0) leaves it armed for the next save.
func (st *SimState) applyLegCatchUp() {
	c := st.catchUp
	if c.lag <= 0 {
		return
	}
	d := &st.Data
	if d.LegEndsAt <= c.from {
		return
	}
	end := d.LegEndsAt - c.lag
	// A leg set to outlast a marker still does.
	for _, mark := range []int64{d.UndockRunUntil, d.UndockLoiterUntil, d.PosAtTowerUntil} {
		if mark > end && mark <= d.LegEndsAt {
			end = mark
		}
	}
	d.LegEndsAt = max(end, c.from)
	st.catchUp = legCatchUp{}
}
