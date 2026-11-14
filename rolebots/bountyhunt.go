package rolebots

import "context"

// ── the gang bounty hunt's intercept ────────────────────────────────────────
//
// A gang hunt commits to a mark and the fleet flies there. Committing points the pack's sweep site
// (GangState.HuntSystemID) at the mark's system: the members' gangHuntStep already beginTravel()s toward it
// and every site picker keeps the current site while its sweep clock runs. The deadline is that clock
// (HuntPickedAt + gangHuntSweepSec), so the pin needs no picker code and releases itself. The fight resolves
// with the members actually in the mark's system, so the law and killmails (killBotShip) run where it happens.

// huntInterceptMaxJumps is how far a fleet will fly at a mark: as far as it can
// reasonably get inside one sweep, so a commitment is not a promise the clock
// breaks for it.
const huntInterceptMaxJumps = 8

// huntCommitMaxJumps is how far from where the fleet stands a mark may be for it to commit, measured from
// the fleet's centre rather than its leader. huntInterceptMaxJumps still bounds a chase once committed.
const huntCommitMaxJumps = 3

// huntFleetCentre is the system most of the fleet stands in, and how many of
// it are there. Ties go to the lowest system ID, so the answer is stable. Pure.
func huntFleetCentre(side []*pvpCombatant) (int64, int) {
	count := map[int64]int{}
	for _, c := range side {
		if c != nil && c.SystemID != 0 {
			count[c.SystemID]++
		}
	}
	var best int64
	n := 0
	for sys, k := range count {
		if k > n || (k == n && sys < best) {
			best, n = sys, k
		}
	}
	return best, n
}

// huntFleetGathered is the muster's quorum applied to a hunt: enough of the
// fleet in one place to fly at a mark together. Pure.
func huntFleetGathered(present, total int) bool {
	if total <= 0 || present <= 0 {
		return false
	}
	return musterRelease(present, total-present, 0, 0, MusterGraceSec(), MusterQuorumFrac())
}

// sideMaxAlign is the slowest align on a side, which sets its pace.
func sideMaxAlign(side []*pvpCombatant) float64 {
	var slow float64
	for _, c := range side {
		if c != nil && c.AlignSec > slow {
			slow = c.AlignSec
		}
	}
	return slow
}

// huntStep is what a fleet with a committed mark does this pass.
type huntStep int

const (
	huntWait     huntStep = iota // still closing in
	huntResolve                  // on the mark with a point: fight
	huntRetarget                 // the mark moved, still in reach: follow
	huntAbandon                  // the trail is cold: drop the mark
)

func (s huntStep) String() string {
	switch s {
	case huntWait:
		return "wait"
	case huntResolve:
		return "resolve"
	case huntRetarget:
		return "retarget"
	case huntAbandon:
		return "abandon"
	}
	return "?"
}

// huntView is everything huntInterceptStep weighs, read by the caller.
type huntView struct {
	Now, Deadline int64
	// Priced and Legal are the reasons to be there at all: a price still on the
	// mark's head, and a licence to collect it where it now stands (huntLegal).
	Priced, Legal bool
	// Docked: the mark is in a station. Nobody shoots it there; the fleet waits
	// it out until the deadline.
	Docked bool
	// Moved: the mark is no longer in the system the pack is flying to.
	// MovedInReach: its new system is still inside huntInterceptMaxJumps of it.
	Moved, MovedInReach bool
	// TacklePresent: at least one member with a point is in the mark's system.
	// No point on the field, no fight, the same rule the commit applies.
	TacklePresent bool
	// ArriveAt is when the slowest member reaches the mark (fleetmove.go fleetArrivalETA); AllPresent that
	// every member already has. The fight waits for the fleet unless it is all there.
	ArriveAt   int64
	AllPresent bool
}

// huntInterceptStep is the intercept's whole decision. Pure. A move is followed only while the deadline
// holds and never extends it, or a chase would follow a mark across the region. Resolving outranks the
// deadline: a pack on the mark with a point fights.
func huntInterceptStep(v huntView) huntStep {
	if !v.Priced || !v.Legal {
		return huntAbandon
	}
	live := v.Now < v.Deadline
	if v.Moved {
		if live && v.MovedInReach {
			return huntRetarget
		}
		return huntAbandon
	}
	if !v.Docked && v.TacklePresent {
		// The fleet fights when the fleet is there: before the slowest member's ETA only a complete pack resolves.
		// Past the deadline the point on the field decides.
		if live && v.Now < v.ArriveAt && !v.AllPresent {
			return huntWait
		}
		return huntResolve
	}
	if live {
		return huntWait
	}
	return huntAbandon
}

// huntSideInSystem keeps the members standing in sysID. Pure.
func huntSideInSystem(side []*pvpCombatant, sysID int64) []*pvpCombatant {
	var out []*pvpCombatant
	for _, c := range side {
		if c != nil && c.SystemID == sysID {
			out = append(out, c)
		}
	}
	return out
}

// huntPinned reports whether a committed intercept still owns the pack's site.
// The dry-belt rule (expireGangHunt) asks it before moving the pack on: a
// lowsec system with no rats in it says nothing about the pilot it came for.
func huntPinned(gs *GangState, now int64) bool {
	return gs != nil && gs.HuntTargetCharID != 0 && now < gs.HuntETA
}

// pinHuntSite points the pack's sweep at sysID. The site it leaves goes onto
// HuntRecent the way every other re-pick records it. pickedAt is the sweep's
// start: the commit passes now, a retarget passes the original so the deadline
// and the sweep clock stay one clock.
func pinHuntSite(gs *GangState, sysID int64, name string, pickedAt int64) {
	if gs.HuntSystemID != 0 && gs.HuntSystemID != sysID {
		gs.HuntRecent = append([]int64{gs.HuntSystemID}, gs.HuntRecent...)
		if len(gs.HuntRecent) > gangHuntRecentMax {
			gs.HuntRecent = gs.HuntRecent[:gangHuntRecentMax]
		}
	}
	gs.HuntSystemID = sysID
	gs.HuntSystemName = name
	gs.HuntPickedAt = pickedAt
}

// claimHuntMark drops a mark into the pass's claimed set.
func claimHuntMark(claimed map[int64]bool, charID int64) {
	if claimed != nil && charID != 0 {
		claimed[charID] = true
	}
}

// writeHuntState applies one change to the gang's current blackboard and saves it. huntOnce holds a copy
// read at the top of the pass and the members' plans write the same row (gangPackSite, expireGangHunt),
// so saving the stale copy would undo their writes. gs is refreshed to what was saved.
func (m *Mod) writeHuntState(ctx context.Context, gangID int64, gs *GangState, apply func(*GangState)) error {
	fresh, err := getGang(ctx, m.db, gangID)
	if err != nil {
		return err
	}
	st := *gs
	if fresh != nil {
		st = fresh.State
	}
	apply(&st)
	if err := saveGangState(ctx, m.db, gangID, &st); err != nil {
		return err
	}
	*gs = st
	return nil
}
