package rolebots

// The lab half of the boot repair. It is split from crashsweep.go because it adopts into
// different state fields than adoptOrphanJobs.

import "context"

// adoptOrphanLabJobs re-attaches research jobs the module lost track of, the same gap
// adoptOrphanJobs covers on the other slot track: a kill between the install and the
// saveState on LabCooking leaves a job nothing will collect. Research and copying only
// (activityID <> 1). Every orphan of a pilot is adopted, soonest to finish first.
func (m *Mod) adoptOrphanLabJobs(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT j.installerID, j.jobID, j.activityID, j.installedItemID,
		       COALESCE(bp.typeID, 0), COALESCE(t.typeName, ''),
		       j.containerID, j.installedInSolarSystemID,
		       COALESCE(j.endProductionTime, 0),
		       COALESCE(p.industryStationID, 0), COALESCE(fs.solarSystemID, 0)
		  FROM ramJobs j
		  JOIN rolebot_characters r ON r.characterID = j.installerID
		  LEFT JOIN entity bp ON bp.itemID = j.installedItemID
		  LEFT JOIN invTypes t ON t.typeID = bp.typeID
		  LEFT JOIN rolebot_profiles p ON p.characterID = j.installerID
		  LEFT JOIN staStations fs ON fs.stationID = p.industryStationID
		 WHERE j.activityID <> 1
		   AND j.completedStatusID IN (0, 1)
		 ORDER BY j.endProductionTime ASC`)
	if err != nil {
		rlogf("rolebots: orphan lab sweep (scan): %v", err)
		return
	}
	type orphan struct {
		charID, jobID, activityID  int64
		bpItemID, bpTypeID         int64
		bpName                     string
		stationID, stationSysID    int64
		endTime                    int64
		factoryID, factorySystemID int64
	}
	var found []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.charID, &o.jobID, &o.activityID, &o.bpItemID,
			&o.bpTypeID, &o.bpName, &o.stationID, &o.stationSysID, &o.endTime,
			&o.factoryID, &o.factorySystemID); err != nil {
			continue
		}
		found = append(found, o)
	}
	rows.Close()
	if len(found) == 0 {
		return
	}

	// Loaded and saved once per pilot, not once per orphan.
	adopted := 0
	states := map[int64]*SimState{}
	dirty := map[int64]bool{}
	order := []int64{}
	for _, o := range found {
		st, ok := states[o.charID]
		if !ok {
			var err error
			st, err = loadState(ctx, m.db, o.charID)
			if err != nil {
				rlogf("rolebots: orphan lab sweep charID=%d: %v", o.charID, err)
				states[o.charID] = nil
				continue
			}
			states[o.charID] = st
			order = append(order, o.charID)
		}
		if st == nil {
			continue
		}
		d := &st.Data
		if d.LabJobID == o.jobID || labCookingHasJob(d.LabCooking, o.jobID) {
			continue // the pilot already knows about this one
		}
		d.LabCooking = append(d.LabCooking, LabCookingJob{
			JobID:        o.jobID,
			BPItemID:     o.bpItemID,
			BPTypeID:     o.bpTypeID,
			BPName:       o.bpName,
			ActivityID:   o.activityID,
			StationID:    o.stationID,
			StationSysID: o.stationSysID,
			// Where to take it afterwards. A pilot with no factory leaves the print where it is; the
			// sweep reads a zero factory as already home.
			FactoryID:    o.factoryID,
			FactorySysID: o.factorySystemID,
			EndsAt:       o.endTime,
		})
		dirty[o.charID] = true
		adopted++
		m.logBot(o.charID, logInfo, "lab",
			"adopted research job %d on the %s — it was on a lab line with nothing to collect it, "+
				"which is what an unclean shutdown mid-install leaves behind",
			o.jobID, labOrphanName(o.bpName))
	}
	for _, charID := range order {
		if !dirty[charID] {
			continue
		}
		if err := saveState(ctx, m.db, states[charID]); err != nil {
			rlogf("rolebots: orphan lab sweep save charID=%d: %v", charID, err)
		}
	}
	if adopted > 0 {
		rlogf("rolebots: orphan lab sweep adopted %d abandoned research job(s)", adopted)
	}
}

// labCookingHasJob reports whether this job is already on the pilot's cooking list. It
// makes the boot repair idempotent.
func labCookingHasJob(cooking []LabCookingJob, jobID int64) bool {
	for _, j := range cooking {
		if j.JobID == jobID {
			return true
		}
	}
	return false
}

// labOrphanName is the blueprint's name, or a stand-in when its entity row has gone.
func labOrphanName(name string) string {
	if name == "" {
		return "blueprint"
	}
	return name
}
