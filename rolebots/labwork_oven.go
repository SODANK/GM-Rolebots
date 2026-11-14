package rolebots

// The lab half of the oven sweep: a finished research job comes off its line
// from wherever the pilot is. industryOvenSweep does the same for factory jobs.
//
// It runs remotely because labCollectDue is only reached from the idle phase,
// and a pilot can stay out of idle for an hour. CompleteJob checks the installer
// and the clock, never the location.
//
// The blueprint returns to flag 4 at the lab. When the lab is not the pilot's
// own factory, labStrayPrint finds the print and the repatriation sweep brings
// it home; the throttle reset below makes that happen on the next idle pass.

import (
	"context"
	"math"
	"strings"

	evedb "gurimoon/db"
)

// labOvenSweep takes every finished research job off its line, wherever the
// pilot is, and drops it from the cooking list. A pilot with nothing cooking
// returns before any query. One query covers the list and returns rows only for
// jobs that need work; a job past its end time counts as finished.
func (m *Mod) labOvenSweep(ctx context.Context, b *botRun) {
	m.labOvenSweepBefore(ctx, b, 0)
}

// labOvenSweepBefore is labOvenSweep limited to jobs that finished at or before
// the Win32 time cut; 0 means any finished job. The off-hours claim pass
// (tzclaim.go) passes a cut in the past. Returns how many jobs it took off the
// line.
func (m *Mod) labOvenSweepBefore(ctx context.Context, b *botRun, cut int64) int {
	st := b.State
	if st == nil {
		return 0
	}
	d := &st.Data
	adopted := labAdoptTripJob(st)
	if len(d.LabCooking) == 0 {
		if adopted {
			_ = saveState(ctx, m.db, st)
		}
		return 0
	}
	ids := make([]int64, 0, len(d.LabCooking))
	for _, j := range d.LabCooking {
		if j.JobID != 0 {
			ids = append(ids, j.JobID)
		}
	}
	if len(ids) == 0 {
		return 0
	}
	place := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	for i, id := range ids {
		place[i] = "?"
		args = append(args, id)
	}
	// One clock reading serves the query and the staleness test, so an end time
	// between two readings cannot make an entry look missing.
	now := win32Now()
	readyCut := int64(math.MaxInt64)
	if cut > 0 {
		now, readyCut = cut, cut
	}
	args = append(args, readyCut, now)
	rows, err := m.db.QueryContext(ctx, `
		SELECT jobID, completedStatusID FROM ramJobs
		 WHERE jobID IN (`+strings.Join(place, ",")+`)
		   AND ((completedStatusID = 1 AND endProductionTime <= ?)
		     OR completedStatusID >= 100
		     OR (completedStatusID = 0 AND endProductionTime > 0 AND endProductionTime <= ?))`, args...)
	if err != nil {
		if adopted {
			_ = saveState(ctx, m.db, st)
		}
		return 0
	}
	rung := make(map[int64]int64, len(ids))
	for rows.Next() {
		var jobID, status int64
		if err := rows.Scan(&jobID, &status); err != nil {
			break
		}
		rung[jobID] = status
	}
	rows.Close()
	// No early return on an empty answer: an entry whose ramJobs row was deleted
	// matches no arm of the query, and only the loop below removes it.

	kept := d.LabCooking[:0:0]
	changed, stray, delivered := false, false, 0
	for _, j := range d.LabCooking {
		status, ok := rung[j.JobID]
		if !ok {
			// Not finished, or the row is gone. An entry past its end time that the query
			// did not return has lost its ramJobs row, so it is dropped from the list.
			if j.EndsAt > 0 && j.EndsAt <= now {
				m.logBot(b.CharID, logInfo, "lab",
					"research job %d is no longer on any line — dropping it from the list", j.JobID)
				changed = true
				continue
			}
			kept = append(kept, j)
			continue
		}
		switch {
		case status >= 100:
			// Delivered or cancelled from outside, such as the ESP factory page. The
			// print is back at flag 4 at the lab either way.
			m.logBot(b.CharID, logInfo, "lab",
				"research job %d on the %s was taken off the line from outside", j.JobID, labOrphanName(j.BPName))
		default:
			if !m.labDeliverRemote(ctx, b, j) {
				// Still running: the 30s status ticker is racing the end time. The next
				// pass settles it.
				kept = append(kept, j)
				continue
			}
			delivered++
		}
		changed = true
		if j.FactoryID != 0 && j.StationID != j.FactoryID {
			stray = true
		}
	}
	if !changed {
		if adopted {
			_ = saveState(ctx, m.db, st)
		}
		return 0
	}
	d.LabCooking = kept
	if stray {
		// The print sits in a hangar that is not its factory, which labStrayPrint
		// looks for. Clearing the throttle stamp runs that scan on the next idle pass.
		d.LabCheckedAt = 0
	}
	if err := saveState(ctx, m.db, st); err != nil {
		rlogf("rolebots: lab oven sweep save for %d: %v", b.CharID, err)
	}
	return delivered
}

// labDeliverRemote takes one job off its line and reports whether it is off.
// It does not check the pilot's location, as in industryDeliverRemote.
// "Already delivered" counts as done; "still running" returns false so the next
// pass settles it.
func (m *Mod) labDeliverRemote(ctx context.Context, b *botRun, j LabCookingJob) bool {
	if _, err := evedb.CompleteJob(ctx, m.db, j.JobID, b.CharID, false); err != nil {
		switch {
		case strings.Contains(err.Error(), "already delivered"):
			return true
		case strings.Contains(err.Error(), "still running"):
			return false
		case strings.Contains(err.Error(), "not found"):
			// The row is gone. Nothing to collect; a surviving blueprint is a stray that
			// the repatriation sweep finds.
			return true
		}
		m.logBot(b.CharID, logError, "lab", "taking research job %d off the line: %v", j.JobID, err)
		return false
	}
	if j.ActivityID == ramActivityCopying {
		m.logBot(b.CharID, logInfo, "lab",
			"copy job %d rang — %d copies of the %s (%d runs each) and the original are in the hangar at %s",
			j.JobID, j.Copies, labOrphanName(j.BPName), j.CopyRuns, m.stationLabel(ctx, j.StationID))
		return true
	}
	m.logBot(b.CharID, logInfo, "lab",
		"research job %d rang — %s on the %s is done and the print is back in the hangar at %s",
		j.JobID, activityLineNoun(j.ActivityID), labOrphanName(j.BPName), m.stationLabel(ctx, j.StationID))
	return true
}

// labCookingHas reports whether the pilot has a job cooking on the given
// research activity. labSlotBiasPE uses it to favour the other pool.
func labCookingHas(cooking []LabCookingJob, activityID int64) bool {
	for _, j := range cooking {
		if j.ActivityID == activityID {
			return true
		}
	}
	return false
}

// labAdoptTripJob moves a job held in the trip fields onto the cooking list and
// reports whether it moved one. It runs on every sweep, because older installs
// and the boot repair (crashsweep_lab.go) leave jobs in LabJobID. It acts only
// from the idle phase, since labworkStep reads LabJobID while a trip runs.
func labAdoptTripJob(st *SimState) bool {
	d := &st.Data
	if d.LabJobID == 0 || st.Phase != string(TaskIdle) {
		return false
	}
	for _, j := range d.LabCooking {
		if j.JobID == d.LabJobID {
			// Already on the list: drop the duplicate trip field and keep the entry.
			clearLabTrip(d)
			return true
		}
	}
	d.LabCooking = append(d.LabCooking, LabCookingJob{
		JobID:        d.LabJobID,
		BPItemID:     d.LabBPItemID,
		BPTypeID:     d.LabBPTypeID,
		BPName:       d.LabBPName,
		ActivityID:   d.LabActivityID,
		StationID:    d.LabStationID,
		StationSysID: d.LabStationSysID,
		FactoryID:    d.LabFactoryID,
		FactorySysID: d.LabFactorySysID,
		EndsAt:       d.LabEndsAt,
	})
	clearLabTrip(d)
	return true
}
