package rolebots

// Boot-time repair for what an unclean shutdown leaves behind.
// The module writes through (saveState at every FSM transition; setSystem, setInSpace and
// dockAt write character_ and entity synchronously), and modapi.Module has no stop hook, so
// the code assumes the process can die between any two statements.
// This pass repairs side-ledgers whose write and delete are on different code paths, and
// world rows a step had committed. It complements the core's boot repairs (TruncateLiveNPCs,
// ClearAllBeltRatEntities, CleanupSpaceLootContainers) and runs once at Init, before the
// lanes start. It is not gated on the master switch.

import (
	"context"
	"time"

	evedb "gurimoon/db"
)

// crashSweepTimeout bounds the whole pass.
const crashSweepTimeout = 3 * time.Minute

// channelPruneBatch is how many (channelID, charID) pairs go into one DELETE. The pairs are
// the table's primary key, so batch size only trades statement count against packet size.
const channelPruneBatch = 500

// runCrashSweep is the whole boot repair. Errors are logged, never fatal: a
// shard that cannot tidy up still has to come up.
func (m *Mod) runCrashSweep(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, crashSweepTimeout)
	defer cancel()

	m.pruneStaleChannelChars(ctx)
	m.pruneDeadGangCans(ctx)
	m.adoptOrphanJobs(ctx)
	m.adoptOrphanLabJobs(ctx)
	// The last two do not repair anything: they report how much there is to do and nudge a won
	// patent into the first planning pass (the per-pilot routine is labwork_lane.go labFetchStray).
	m.reportStrayBlueprints(ctx)
	m.sweepUnclaimedPatents(ctx)
	m.unloadRejectedChargesAll(ctx)
}

// unloadRejectedChargesAll takes every charge out of a roster ship's slot whose module
// cannot fire it and puts it in that ship's hold. It is the bulk form of
// unloadRejectedCharges. The test is the core's chargeAccepts, run in Go over the candidates
// so it matches the loads. Idempotent.
func (m *Mod) unloadRejectedChargesAll(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.itemID, c.typeID, mo.typeID
		  FROM entity c
		  JOIN entity  sh ON sh.itemID = c.locationID
		  JOIN rolebot_characters r ON r.characterID = sh.ownerID
		  JOIN invTypes ct ON ct.typeID = c.typeID
		  JOIN invGroups cg ON cg.groupID = ct.groupID AND cg.categoryID = 8
		  JOIN entity  mo ON mo.locationID = c.locationID AND mo.flag = c.flag AND mo.itemID <> c.itemID
		  JOIN invTypes mt ON mt.typeID = mo.typeID
		  JOIN invGroups mg ON mg.groupID = mt.groupID AND mg.categoryID = 7
		 WHERE c.flag BETWEEN ? AND ?`, flagLoFirst, flagHiFirst+7)
	if err != nil {
		rlogf("rolebots: rejected-charge sweep (scan): %v", err)
		return
	}
	var bad []int64
	for rows.Next() {
		var itemID, charge, module int64
		if err := rows.Scan(&itemID, &charge, &module); err != nil {
			rows.Close()
			rlogf("rolebots: rejected-charge sweep (scan row): %v", err)
			return
		}
		if !m.chargeAccepts(ctx, charge, module) {
			bad = append(bad, itemID)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		rlogf("rolebots: rejected-charge sweep (scan): %v", err)
		return
	}
	moved := 0
	for i := 0; i < len(bad); i += channelPruneBatch {
		batch := bad[i:min(i+channelPruneBatch, len(bad))]
		// UnloadChargeToCargoOrHangar's write, batched: the charge stays on its ship and moves to
		// the hold.
		res, err := m.db.ExecContext(ctx,
			`UPDATE entity SET flag = 5, singleton = 0 WHERE itemID IN (`+placeholders(len(batch))+`)`,
			int64Args(batch)...)
		if err != nil {
			rlogf("rolebots: rejected-charge sweep (unload): %v", err)
			return
		}
		if n, err := res.RowsAffected(); err == nil {
			moved += int(n)
		}
	}
	if moved > 0 {
		rlogf("rolebots: rejected-charge sweep moved %d charge(s) out of slots that cannot fire them", moved)
	}
}

// pruneStaleChannelChars removes channelChars rows for system channels a bot is no longer in.
// The core's CleanupStalePrivateChannels only takes channelID >= 140000000 and a system
// channel's ID is its solarSystemID. The mapSolarSystems join keeps corp and private channels
// out; the character_ join keeps the bot's current system.
func (m *Mod) pruneStaleChannelChars(ctx context.Context) {
	type pair struct{ channelID, charID int64 }

	// Read the whole worklist and close the cursor before deleting: a DELETE issued while the
	// SELECT streams takes another pooled connection and waits on rows this one holds.
	rows, err := m.db.QueryContext(ctx, `
		SELECT cc.channelID, cc.charID
		  FROM channelChars cc
		  JOIN rolebot_characters r ON r.characterID = cc.charID
		  JOIN mapSolarSystems  s  ON s.solarSystemID = cc.channelID
		  LEFT JOIN character_  c  ON c.characterID = cc.charID
		 WHERE c.characterID IS NULL
		    OR cc.channelID <> c.solarSystemID`)
	if err != nil {
		rlogf("rolebots: channelChars prune (scan): %v", err)
		return
	}
	var stale []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.channelID, &p.charID); err != nil {
			continue
		}
		stale = append(stale, p)
	}
	rows.Close()
	if len(stale) == 0 {
		return
	}

	deleted := 0
	for start := 0; start < len(stale); start += channelPruneBatch {
		end := start + channelPruneBatch
		if end > len(stale) {
			end = len(stale)
		}
		batch := stale[start:end]
		q := `DELETE FROM channelChars WHERE (channelID, charID) IN (`
		args := make([]any, 0, len(batch)*2)
		for i, p := range batch {
			if i > 0 {
				q += ","
			}
			q += "(?,?)"
			args = append(args, p.channelID, p.charID)
		}
		q += ")"
		res, err := m.db.ExecContext(ctx, q, args...)
		if err != nil {
			rlogf("rolebots: channelChars prune (delete): %v", err)
			return
		}
		if n, err := res.RowsAffected(); err == nil {
			deleted += int(n)
		}
	}
	rlogf("rolebots: channelChars prune removed %d stale Local membership row(s)", deleted)
}

// pruneDeadGangCans drops jetcan registry rows whose container no longer exists. The core
// deletes the entity rows at every boot (CleanupSpaceLootContainers); openCanFor and
// sweepCans prune lazily but sit behind the master switch.
func (m *Mod) pruneDeadGangCans(ctx context.Context) {
	res, err := m.db.ExecContext(ctx, `
		DELETE c FROM rolebot_gang_cans c
		LEFT JOIN entity e ON e.itemID = c.canItemID
		WHERE e.itemID IS NULL`)
	if err != nil {
		rlogf("rolebots: gang can prune: %v", err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		rlogf("rolebots: gang can prune removed %d row(s) for containers the core cleared", n)
	}
}

// adoptOrphanJobs hands a pilot back a manufacturing job the world says is its own but its
// state does not list (a kill between the install and the saveState on StateData.IndCooking).
// Adopting the entry lets industryOvenSweep deliver it. Manufacturing only (activityID 1),
// the one activity IndCooking models.
func (m *Mod) adoptOrphanJobs(ctx context.Context) {
	type orphan struct {
		charID    int64
		job       IndCookingJob
		installed int64 // win32 FILETIME
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT j.installerID, j.jobID, j.outputTypeID, COALESCE(t.typeName,''),
		       j.installedItemID, COALESCE(bp.typeID, 0),
		       j.containerID, j.installedInSolarSystemID, j.runs, j.installTime
		  FROM ramJobs j
		  JOIN rolebot_characters r ON r.characterID = j.installerID
		  LEFT JOIN invTypes t  ON t.typeID = j.outputTypeID
		  LEFT JOIN entity   bp ON bp.itemID = j.installedItemID
		 WHERE j.activityID = 1
		   AND j.completedStatusID IN (0, 1)`)
	if err != nil {
		rlogf("rolebots: orphan job sweep (scan): %v", err)
		return
	}
	byChar := map[int64][]orphan{}
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.charID, &o.job.JobID, &o.job.TypeID, &o.job.TypeName,
			&o.job.BPItemID, &o.job.BPTypeID, &o.job.FactoryID,
			&o.job.FactorySysID, &o.job.Runs, &o.installed); err != nil {
			continue
		}
		byChar[o.charID] = append(byChar[o.charID], o)
	}
	rows.Close()
	if len(byChar) == 0 {
		return
	}

	adopted := 0
	for charID, jobs := range byChar {
		st, err := loadState(ctx, m.db, charID)
		if err != nil {
			rlogf("rolebots: orphan job sweep charID=%d: %v", charID, err)
			continue
		}
		d := &st.Data
		// Known means some part of this pilot's state already points at it: the cooking list or
		// the project's in-flight job.
		known := make(map[int64]bool, len(d.IndCooking)+1)
		for _, j := range d.IndCooking {
			known[j.JobID] = true
		}
		if d.IndJobID != 0 {
			known[d.IndJobID] = true
		}

		dirty := false
		for _, o := range jobs {
			if known[o.job.JobID] {
				continue
			}
			j := o.job
			j.StartedAt = evedb.Win32ToUnix(o.installed)
			d.IndCooking = append(d.IndCooking, j)
			known[j.JobID] = true
			dirty = true
			adopted++
			m.logBot(charID, logInfo, "industry",
				"adopted job %d (%s ×%d) — it was on the line with nothing to collect it, "+
					"which is what an unclean shutdown mid-install leaves behind",
				j.JobID, j.TypeName, j.Runs)
		}
		if !dirty {
			continue
		}
		if err := saveState(ctx, m.db, st); err != nil {
			rlogf("rolebots: orphan job sweep save charID=%d: %v", charID, err)
		}
	}
	if adopted > 0 {
		rlogf("rolebots: orphan job sweep adopted %d abandoned manufacturing job(s)", adopted)
	}
}
