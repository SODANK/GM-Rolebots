package rolebots

import (
	"context"
	"time"
)

// Moving as one, the last of the fleet command layers (fleetcommand.go).
//
//   - The intercept waits for the slowest member: a committed hunt's fight is not resolved
//     before fleetArrivalETA unless the whole fleet is already there. HuntETA is unchanged.
//   - Hold, then warp: a commanded member on its commander's grid holds on the anchor while
//     the commander stands still and goes when it goes, bounded by fleetHoldMaxSec.
//   - The follow takes the formed-up line: a member holding in orbit of its commander
//     follows its warp like an idle one, landing in one lane (destiny.ClaimArrivalPoint).
//   - The convoy: a member waits at each gate for packmates behind it (convoyHolds).

const (
	// fleetHoldMaxSec bounds hold-then-warp. A commander standing on a quiet grid for longer
	// than this is not about to move, so the member flies its own plan.
	fleetHoldMaxSec = 90
)

// fleetArrivalETA is when a fleet whose members are jumps away arrives: the
// slowest member's route at secPerJump a gate, from now. A fleet already
// there arrives now. Pure.
func fleetArrivalETA(now int64, jumps []int, secPerJump int64) int64 {
	slowest := 0
	for _, j := range jumps {
		if j > slowest {
			slowest = j
		}
	}
	return now + int64(slowest)*secPerJump
}

// fleetMemberJumps is each member's own route length to sysID, for the arrival ETA. A
// member whose route cannot be read counts as already there, so an unreadable map cannot
// hold a fleet forever.
func (m *Mod) fleetMemberJumps(ctx context.Context, lb *botRun, side []*pvpCombatant, sysID int64) []int {
	out := make([]int, 0, len(side))
	for _, c := range side {
		if c == nil || c.SystemID == 0 {
			continue
		}
		j, err := m.jumpsBetween(ctx, lb, c.SystemID, sysID)
		if err != nil {
			j = 0
		}
		out = append(out, j)
	}
	return out
}

// followEligible is whether a packmate takes its commander's warp. Pure over the mate's
// flags: an idle mate always does; a mate holding in orbit of the commander (the form-up,
// the anchor) does too when the fleet has a command. A mate warping, fighting, fleeing,
// mining, mid-approach or already bound for the target stays.
func followEligible(dead, warping, fighting, fleeing, holding, mining, approaching, boundThere bool,
	orbitingLead, commanded bool) bool {
	if dead || warping || fighting || fleeing || mining || approaching || boundThere {
		return false
	}
	if holding {
		return commanded && orbitingLead
	}
	return true
}

// fleetHoldsForFC reports whether a commanded member should hold its planned leg for the
// commander's warp: the commander is projected on this grid under a command, not warping
// and not bound where this member is going, and the hold is under fleetHoldMaxSec.
// fleetHoldRelease clears the hold clock.
func (m *Mod) fleetHoldsForFC(eb *embodiedBot, itemID int64) bool {
	lead := fleetLeaderLive(eb)
	if lead == nil {
		m.fleetHoldRelease(eb)
		return false
	}
	lead.mu.Lock()
	warping, dest := lead.warping, lead.currentDest
	lead.mu.Unlock()
	if warping || dest == itemID {
		// The commander is going, or has gone, where this member is going:
		// the follow carries the member, or it is free to fly the same leg.
		m.fleetHoldRelease(eb)
		return false
	}
	now := clock.Now()
	eb.mu.Lock()
	if eb.fleetHoldSince.IsZero() {
		eb.fleetHoldSince = now
	}
	held := now.Sub(eb.fleetHoldSince)
	eb.mu.Unlock()
	if held.Seconds() > fleetHoldMaxSec {
		return false
	}
	return true
}

// fleetHoldRelease ends a member's hold clock.
func (m *Mod) fleetHoldRelease(eb *embodiedBot) {
	if eb == nil {
		return
	}
	eb.mu.Lock()
	eb.fleetHoldSince = time.Time{}
	eb.mu.Unlock()
}

// ── the convoy ──────────────────────────────────────────────────────────────

// convoyRetrySec is how soon a member held at its gate for the convoy asks again: much shorter
// than a gate-wave refusal, since the packmate it waits for is a jump or two away.
const convoyRetrySec = 10

// convoyMate is one packmate as the convoy rule reads it: where its trip goes, how many jumps it
// still has, and whether it is travelling now.
type convoyMate struct {
	Target    int64
	Hops      int
	Traveling bool
}

// convoyWaiting reports whether a member about to jump with hops jumps left to target should
// wait: a travelling packmate bound for the same system has more jumps left, so it is behind on
// the road. A mate ahead, or on another trip, is not waited for. Pure.
func convoyWaiting(target int64, hops int, mates []convoyMate) bool {
	if target == 0 {
		return false
	}
	for _, c := range mates {
		if c.Traveling && c.Target == target && c.Hops > hops {
			return true
		}
	}
	return false
}

// convoyHoldExpired reports a convoy hold that has run past fleetHoldMaxSec: the member goes
// without the straggler. Pure.
func convoyHoldExpired(since, now int64) bool {
	return since != 0 && now-since > fleetHoldMaxSec
}

// convoyHolds is travelStep's convoy question at the gate: does this fleet member wait for
// packmates behind it on the same trip? Haulers neither wait nor are waited for, and a packmate
// the population cap holds offline is not waited for (popIsOffline). The wait is bounded by
// fleetHoldMaxSec, timed from ConvoyHoldSince, which the caller clears at the jump.
func (m *Mod) convoyHolds(ctx context.Context, b *botRun) bool {
	d := &b.State.Data
	if b.GangID == 0 || d.TargetSystemID == 0 || len(d.Path) == 0 {
		return false
	}
	now := clock.Unix()
	if convoyHoldExpired(d.ConvoyHoldSince, now) {
		return false
	}
	mems, err := gangMembers(ctx, m.db, b.GangID)
	if err != nil {
		return false
	}
	var ids []int64
	for _, mem := range mems {
		if mem.CharacterID == b.CharID {
			if mem.Role == RoleHauler {
				return false
			}
			continue
		}
		if mem.Driven && mem.Role != RoleHauler && !popIsOffline(mem.CharacterID) {
			ids = append(ids, mem.CharacterID)
		}
	}
	if len(ids) == 0 {
		return false
	}
	states, err := loadStatesBulk(ctx, m.db, ids)
	if err != nil {
		return false
	}
	mates := make([]convoyMate, 0, len(states))
	for _, st := range states {
		mates = append(mates, convoyMate{Target: st.Data.TargetSystemID, Hops: len(st.Data.Path),
			Traveling: st.Phase == "travel"})
	}
	if !convoyWaiting(d.TargetSystemID, len(d.Path), mates) {
		return false
	}
	if d.ConvoyHoldSince == 0 {
		d.ConvoyHoldSince = now
	}
	return true
}
