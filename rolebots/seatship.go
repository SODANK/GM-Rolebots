package rolebots

import "context"

// A logistics hull of its own.
//
// The logi seat is filled only by a pilot that already owns a logi ship
// (gangRecruitFrom: hasSeatShipIn), and the muster buys nothing. So the home
// shopping pass buys one for a pilot drawn to the seat and trained for it,
// through the ordinary outfit pipeline (selectFitForActivity).
//
// A ship record is per hull, not per job (generatedFitID), so only the rack
// says a ship is a logi ship: shipRack reads the high slots, the seat takes
// only a mending rack, and selectFit keeps one out of every other job.

// logiSeatMinReps is the fewest remote repairers a ship carries to sit in the
// logi seat. Every hull the seat admits has at least two highs (deriveRoles).
const logiSeatMinReps = 2

// rackKind is what a ship's high slots say the ship is for.
type rackKind int

const (
	rackOpen  rackKind = iota // bare, or part way through being fitted
	rackLogi                  // a mending rack
	rackOther                 // guns or miners: another job's ship
)

// rackKindOf classifies a high rack by its remote repairers and its turrets
// and launchers (mining lasers count, they are turrets). A rep or two beside
// a fuller rack of guns is backfill, not a logi. Pure.
func rackKindOf(reps, work int) rackKind {
	switch {
	case reps >= logiSeatMinReps && reps > work:
		return rackLogi
	case work > 0:
		return rackOther
	}
	return rackOpen
}

// shipRack reads a ship's high slots. An unreadable ship is rackOpen, which
// the seat refuses and no job is barred from.
func (m *Mod) shipRack(ctx context.Context, shipID int64) rackKind {
	slots, err := m.shipSlotModules(ctx, shipID)
	if err != nil {
		return rackOpen
	}
	reps, work := 0, 0
	for flag, typeID := range slots {
		if bandOf(flag) != flagHiFirst {
			continue
		}
		mi := m.moduleInfo(ctx, typeID)
		switch {
		case mi.groupID == grpShieldTransporter || mi.groupID == grpArmorRepProjector:
			reps++
		case mi.turret || mi.launcher:
			work++
		}
	}
	return rackKindOf(reps, work)
}

// rackReservedFor reports whether a ship with this rack is off limits to this
// job: the logi seat never takes another job's ship, and a pilot drawn to the
// seat keeps its mending rack out of every other job. Pure.
func rackReservedFor(job string, kind rackKind, logiPilot bool) bool {
	if job == JobLogi {
		return kind == rackOther
	}
	return logiPilot && kind == rackLogi
}

// seatRackOK is the rack half of the seat tests. Only the logi seat has one.
func (m *Mod) seatRackOK(ctx context.Context, activity string, shipID int64) bool {
	return activity != ActSeatLogi || m.shipRack(ctx, shipID) == rackLogi
}

// logiSeatDriveMin is the pull toward the seat (Tuning.LogiDrive) a pilot
// needs before it buys a hull it only flies in a fleet. Pacifists clear it
// alone; a following temperament needs a real lean too; violent pilots never.
const logiSeatDriveMin = 0.3

// wantsLogiHull is the temperament half of stockSeatShip.
func wantsLogiHull(tn Tuning) bool { return tn.LogiDrive >= logiSeatDriveMin }

// stockSeatShip starts the logistics-hull purchase when this pilot should
// own one and does not. It reports whether it took the pass (a procurement
// trip has begun). Failures are quiet: no fit, no money and no seller all
// mean "not this visit", and the next visit asks again.
func (m *Mod) stockSeatShip(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if b.Broke || b.StationID == 0 || st == nil || st.Data.GangID != 0 {
		return false, nil
	}
	if !wantsLogiHull(b.Tune) {
		return false, nil
	}
	if ok, err := m.canWorkJob(ctx, b.CharID, JobLogi); err != nil || !ok {
		return false, nil
	}
	if ok, err := m.hasSeatShip(ctx, b.CharID, ActSeatLogi); err != nil || ok {
		return false, nil
	}
	ready, err := m.ensureActivityShip(ctx, b, ActSeatLogi)
	if err != nil || ready {
		return false, nil
	}
	// Not ready and no error: the call began a shopping trip for the seat
	// (startProcurement / resumeOutfit name it as the activity to resume).
	if st.Data.ResumeActivity != ActSeatLogi || len(st.Data.Shopping) == 0 {
		return false, nil
	}
	st.Activity = ""
	st.Data.ResumeActivity = ""
	st.Data.ShopStockRun = true
	m.logBot(b.CharID, logInfo, "home",
		"buying a logistics ship of its own — the fleet's mending seat goes to a pilot who owns one")
	return true, saveState(ctx, m.db, st)
}
