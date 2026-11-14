package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// Industry at the tower: the overflow for station lines that clog. A factory or lab tower's
// arrays carry real assembly lines while online (db.SyncStructureAssemblyLines), and a job
// there is a corporation job out of the array's own hangar divisions. A tower job is three legs:
//   - feed: when the work board's order (corpwork.go) is backed up at the station, its library
//     print and a batch of materials are hauled from the factory office into division 1.
//   - install: a contract builder (Factory Manager) flies to the tower and installs through
//     db.ResolveStructureCorpJob (the S&I window's rules), claiming the runs under its own name.
//   - return: the job delivers into the array (deliverReadyCorpJobs); the trader desk hauls the
//     output to the factory office's Production division, and the print goes home when done.
// One job per array at a time, tracked in rolebot_corp_towerjobs.

const (
	towerJobClaimSec = 6 * 3600
	// TaskTowerJob is a builder installing a job at a tower array.
	TaskTowerJob TaskKind = "starbase.towerjob"
)

func init() {
	registerTask(TaskDef{
		Kind: TaskTowerJob, Label: "Installing a job at its corporation's tower",
		Posture: PostureHold, Step: (*Mod).towerJobStep,
	})
}

// ensureTowerJobTable is called from ensureStarbaseTables.
const towerJobDDL = `CREATE TABLE IF NOT EXISTS rolebot_corp_towerjobs (
	arrayID       BIGINT NOT NULL,
	corporationID BIGINT NOT NULL,
	planID        BIGINT NOT NULL DEFAULT 0,
	workID        BIGINT NOT NULL DEFAULT 0,
	bpTypeID      BIGINT NOT NULL DEFAULT 0,
	activityID    INT NOT NULL DEFAULT 1,
	runs          INT NOT NULL DEFAULT 0,
	status        VARCHAR(10) NOT NULL DEFAULT 'feeding',
	builderID     BIGINT NOT NULL DEFAULT 0,
	claimedAt     BIGINT NOT NULL DEFAULT 0,
	jobID         BIGINT NOT NULL DEFAULT 0,
	postedAt      BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (arrayID),
	KEY idx_rbctj_corp (corporationID, status)
)`

type towerJob struct {
	ArrayID, CorpID, PlanID, WorkID, BPTypeID int64
	ActivityID, Runs                          int
	Status                                    string
	BuilderID, ClaimedAt, JobID, PostedAt     int64
}

func (m *Mod) loadTowerJob(ctx context.Context, arrayID int64) (*towerJob, error) {
	var j towerJob
	err := m.db.QueryRowContext(ctx, `
		SELECT arrayID, corporationID, planID, workID, bpTypeID, activityID, runs, status, builderID, claimedAt, jobID, postedAt
		  FROM rolebot_corp_towerjobs WHERE arrayID = ?`, arrayID).
		Scan(&j.ArrayID, &j.CorpID, &j.PlanID, &j.WorkID, &j.BPTypeID, &j.ActivityID, &j.Runs, &j.Status,
			&j.BuilderID, &j.ClaimedAt, &j.JobID, &j.PostedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &j, err
}

// towerArrays lists a tower's online assembly arrays and labs.
func (m *Mod) towerArrays(ctx context.Context, p *towerPlan) []evedb.POSInstallation {
	var out []evedb.POSInstallation
	for _, pc := range p.Layout.Pieces {
		if pc.ItemID == 0 {
			continue
		}
		if inst, ok, err := evedb.GetPOSInstallation(ctx, m.db, pc.ItemID); err == nil && ok && inst.State == evedb.StarbaseStateOnline {
			out = append(out, inst)
		}
	}
	return out
}

// ceoTowerIndustry feeds idle arrays from the backed-up work board and clears
// finished jobs. Run from the CEO's starbase pass.
func (m *Mod) ceoTowerIndustry(ctx context.Context, c *corpCEO, ch Charter, plans []*towerPlan) {
	stationID, officeID := m.corpFactoryOffice(ctx, c.CorpID)
	if officeID == 0 {
		return
	}
	for _, p := range plans {
		if (p.Purpose != purposeFactory && p.Purpose != purposeLab) || p.Status != towerRunning {
			continue
		}
		for _, arr := range m.towerArrays(ctx, p) {
			tj, _ := m.loadTowerJob(ctx, arr.ArrayID)
			if tj != nil {
				m.advanceTowerJob(ctx, c, p, arr, tj)
				continue
			}
			// A laboratory runs the urgent research rotation (postowerlab.go): a resident original goes
			// back to work while urgent orders name it, and home once none does.
			if arr.GroupID == evedb.GroupMobileLaboratory && m.openBackhaulFrom(ctx, arr.ArrayID) == nil &&
				m.openFeedLeg(ctx, arr.ArrayID) == nil && m.feedTowerLab(ctx, c, p, arr, stationID, officeID) {
				continue
			}
			if m.arrayHoldsOriginal(ctx, c.CorpID, arr.ArrayID) {
				if m.openBackhaulFrom(ctx, arr.ArrayID) == nil {
					m.returnArrayOutput(ctx, c, p, arr)
				}
				continue
			}
			m.feedTowerArray(ctx, c, p, arr, stationID, officeID)
		}
	}
}

// feedTowerArray picks the oldest backed-up build order the array can run and posts a
// blueprint copy and the materials out to it: a type with no free copy gets a copy order
// "for the tower". A laboratory is fed by feedTowerLab instead (urgent research and copying,
// its original in a Director's hold).
func (m *Mod) feedTowerArray(ctx context.Context, c *corpCEO, p *towerPlan, arr evedb.POSInstallation, stationID, officeID int64) {
	if arr.GroupID == evedb.GroupMobileLaboratory {
		return
	}
	work, err := m.openWorkFor(ctx, c.CorpID)
	if err != nil {
		return
	}
	copies, _ := m.libraryCopies(ctx, c.CorpID, stationID)
	idle := map[int64]libraryCopy{}
	for _, cp := range copies { // most runs first within a type
		if cp.Flag != 111 {
			if _, ok := idle[cp.BPTypeID]; !ok {
				idle[cp.BPTypeID] = cp
			}
		}
	}
	copyOrdered := map[int64]bool{}
	for _, w := range work {
		if w.Kind == workKindCopy {
			copyOrdered[w.BPTypeID] = true
		}
	}
	var lineType int64
	_ = m.db.QueryRowContext(ctx, `SELECT assemblyLineTypeID FROM ramassemblylines WHERE containerID = ? LIMIT 1`, arr.ArrayID).Scan(&lineType)
	// A station with no free manufacturing line sends its work out at once:
	// the tower is the corporation's factory space when the office has none.
	stationFull := m.freeStationLines(ctx, stationID, 1) == 0
	for _, w := range work {
		act := workActivity(w.Kind)
		if act == 0 || w.RunsLeft() <= 0 || !towerTakesWork(stationFull, clock.Unix()-w.PostedAt) {
			continue
		}
		if act != 1 {
			continue // research and copying need the original, at the station
		}
		pr, ok := idle[w.BPTypeID]
		if !ok {
			if !copyOrdered[w.BPTypeID] {
				copyOrdered[w.BPTypeID] = true
				m.copyForTower(ctx, c, stationID, w.BPTypeID)
			}
			continue
		}
		if lineType != 0 && !evedb.LineCanBuild(ctx, m.db, lineType, w.ProductTypeID) {
			continue
		}
		// No bot-only batch: the order's runs left, held to the copy's runs, the print's own run limit
		// (the law a player is under) and what the Materials division can feed.
		runs := min(w.RunsLeft(), int(pr.Runs))
		items := []haulItem{{TypeID: w.BPTypeID, Qty: 1, Flag: pr.Flag, ItemID: pr.ItemID, CopyOnly: true}}
		if bt, berr := evedb.GetBlueprintTypeInfo(ctx, m.db, w.BPTypeID); berr == nil &&
			bt.MaxProductionLimit > 0 && int64(runs) > bt.MaxProductionLimit {
			runs = int(bt.MaxProductionLimit)
		}
		perRun, err := m.industryMaterialNeeds(ctx, w.BPTypeID, pr.ME, 1, 0, 1.0)
		if err != nil {
			continue
		}
		mats := m.divisionHeld(ctx, c.CorpID, officeID, corpDivMaterials)
		for _, n := range perRun {
			if n.Needed > 0 {
				runs = min(runs, int(mats[n.TypeID]/n.Needed))
			}
		}
		if runs < 1 {
			continue
		}
		needs, err := m.industryMaterialNeeds(ctx, w.BPTypeID, pr.ME, runs, 0, 1.0)
		if err != nil {
			continue
		}
		for _, n := range needs {
			if n.Needed > 0 {
				items = append(items, haulItem{TypeID: n.TypeID, Qty: n.Needed, Flag: corpDivMaterials})
			}
		}
		// Reserve the runs under the array so the station's builders do not
		// take the same batch while it is on its way out.
		if got, err := m.claimWorkRuns(ctx, w, arr.ArrayID, runs); err != nil || got <= 0 {
			continue
		}
		if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindFeed, PlanID: p.PlanID, FromKind: haulFromOffice,
			FromID: officeID, FromStationID: stationID, FromFlag: -1, ToKind: haulToTower, ToID: arr.ArrayID,
			ToSystemID: p.SystemID, ToFlag: evedb.FlagCorpHangarDiv1, Items: items, PostedBy: "industry",
			Note: fmt.Sprintf("%d run(s) of %s for the tower", runs, m.typeNameOr(ctx, w.BPTypeID))}); err != nil {
			m.releaseWorkClaim(ctx, w.WorkID, arr.ArrayID)
			continue
		}
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_towerjobs (arrayID, corporationID, planID, workID, bpTypeID, activityID, runs, status, postedAt)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'feeding', ?)`,
			arr.ArrayID, c.CorpID, p.PlanID, w.WorkID, w.BPTypeID, act, runs, clock.Unix())
		why := "the station lines are backed up"
		if stationFull {
			why = "its factory office has no free line"
		}
		m.ceoLog(ctx, c, powerStarbase, "towerjob", w.BPTypeID, 0,
			"sent %d run(s) of %s out to its tower at %s — %s",
			runs, m.typeNameOr(ctx, w.BPTypeID), moonNameOf(ctx, m, p.MoonID), why)
		return
	}
}

// towerWaitSec is how long a work order waits on the station's lines before a
// tower array takes it, when the station has free lines at all.
const towerWaitSec = 86400

// towerTakesWork is whether a tower array may take a build order: at once
// when the factory office's station has no free manufacturing line, else
// only after the station has had towerWaitSec to get to it. Pure.
func towerTakesWork(stationFull bool, age int64) bool {
	return stationFull || age >= towerWaitSec
}

// advanceTowerJob moves an array's job along: fed → ready for a builder;
// installed → done when its job has delivered, the output hauled home.
func (m *Mod) advanceTowerJob(ctx context.Context, c *corpCEO, p *towerPlan, arr evedb.POSInstallation, tj *towerJob) {
	switch tj.Status {
	case "feeding":
		if m.arrayHasPrint(ctx, arr.ArrayID, tj.BPTypeID) && m.openFeedLeg(ctx, arr.ArrayID) == nil {
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_towerjobs SET status = 'ready' WHERE arrayID = ?`, arr.ArrayID)
			markPosWork(c.CorpID)
		} else if m.openFeedLeg(ctx, arr.ArrayID) == nil && clock.Unix()-tj.PostedAt > 2*86400 {
			m.dropTowerJob(ctx, tj, "the feed never arrived")
		}
	case "ready":
		if tj.BuilderID != 0 && clock.Unix()-tj.ClaimedAt > towerJobClaimSec {
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_towerjobs SET builderID = 0, claimedAt = 0 WHERE arrayID = ?`, arr.ArrayID)
		}
		markPosWork(c.CorpID)
	case "installed":
		var status int64
		err := m.db.QueryRowContext(ctx, `SELECT completedStatusID FROM ramJobs WHERE jobID = ?`, tj.JobID).Scan(&status)
		if err != nil || status >= 101 {
			m.dropTowerJob(ctx, tj, "")
			m.returnArrayOutput(ctx, c, p, arr)
		}
	}
}

func (m *Mod) dropTowerJob(ctx context.Context, tj *towerJob, why string) {
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_towerjobs WHERE arrayID = ?`, tj.ArrayID)
	m.releaseWorkClaim(ctx, tj.WorkID, tj.ArrayID)
	if why != "" {
		if c, _ := getCEO(ctx, m.db, tj.CorpID); c != nil {
			m.ceoLog(ctx, c, powerStarbase, "towerjob", tj.BPTypeID, 0, "dropped a tower job: %s", why)
		}
	}
}

// copyForTower posts one copy order off the type's best free original, so a
// backed-up build order can go out to a tower next pass.
func (m *Mod) copyForTower(ctx context.Context, c *corpCEO, stationID, bpTypeID int64) {
	if !m.labStationHasLine(ctx, stationID, ramActivityCopying) {
		return
	}
	prints, err := m.libraryPrints(ctx, c.CorpID, stationID)
	if err != nil {
		return
	}
	var ps []libraryPrint
	for _, p := range prints {
		if p.BPTypeID == bpTypeID {
			ps = append(ps, p)
		}
	}
	if a := allocateCopyOrders(ps, 1, nil, 1, 0); len(a) == 1 {
		_ = m.postCopyWork(ctx, c, stationID, a[0].Print, 1, "for the tower", corpCopyShelfPriority)
	}
}

// arrayHoldsOriginal reports whether a corporation blueprint original sits in an array
// outside a job.
func (m *Mod) arrayHoldsOriginal(ctx context.Context, corpID, arrayID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE e.locationID = ? AND e.ownerID = ? AND e.flag <> 111 AND COALESCE(b.copy, 0) = 0`,
		arrayID, corpID).Scan(&n)
	return n > 0
}

func (m *Mod) openBackhaulFrom(ctx context.Context, arrayID int64) *haulLeg {
	legs, err := m.queryHauls(ctx, `WHERE fromID = ? AND kind = ? AND status IN ('open','claimed') LIMIT 1`, arrayID, haulKindBackhaul)
	if err != nil || len(legs) == 0 {
		return nil
	}
	return legs[0]
}

func (m *Mod) arrayHasPrint(ctx context.Context, arrayID, bpTypeID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity WHERE locationID = ? AND typeID = ? AND flag <> 111`, arrayID, bpTypeID).Scan(&n)
	return n > 0
}

func (m *Mod) openFeedLeg(ctx context.Context, arrayID int64) *haulLeg {
	legs, err := m.queryHauls(ctx, `WHERE toID = ? AND kind = ? AND status IN ('open','claimed') LIMIT 1`, arrayID, haulKindFeed)
	if err != nil || len(legs) == 0 {
		return nil
	}
	return legs[0]
}

// bayHeldAll sums a structure's contents across every flag but jobs (111).
func (m *Mod) bayHeldAll(ctx context.Context, structureID int64) map[int64]int64 {
	out := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, SUM(GREATEST(quantity,1)) FROM entity WHERE locationID = ? AND flag <> 111 GROUP BY typeID`, structureID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t, q int64
		if rows.Scan(&t, &q) == nil {
			out[t] = q
		}
	}
	return out
}

// ── the builder ─────────────────────────────────────────────────────────────

// posTowerJobDue sends a contract builder to install a ready tower job.
func (m *Mod) posTowerJobDue(ctx context.Context, b *botRun) (bool, error) {
	if b.StationID == 0 || !m.isCorpBuilder(ctx, b.CharID) {
		return false, nil
	}
	now := clock.Unix()
	var arrayID, planID int64
	err := m.db.QueryRowContext(ctx, `
		SELECT arrayID, planID FROM rolebot_corp_towerjobs
		 WHERE corporationID = ? AND status = 'ready' AND (builderID = 0 OR claimedAt < ?)
		 ORDER BY postedAt LIMIT 1`, b.CorpID, now-towerJobClaimSec).Scan(&arrayID, &planID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	p, _ := m.loadTowerPlan(ctx, planID)
	if p == nil {
		return false, nil
	}
	if j := m.jumpsTo(ctx, b, p.SystemID); j < 0 || j > corpWorkMaxJumps {
		return false, nil
	}
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_towerjobs SET builderID = ?, claimedAt = ?
		 WHERE arrayID = ? AND status = 'ready' AND (builderID = 0 OR claimedAt < ?)`, b.CharID, now, arrayID, now-towerJobClaimSec)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	st := b.State
	st.Activity = ActIndustry
	st.Data = idleData(st.Data)
	st.Data.PosTowerJobArray = arrayID
	m.logBot(b.CharID, logInfo, "industry", "flying out to install a job at its corporation's tower on %s", moonNameOf(ctx, m, p.MoonID))
	return true, m.beginTravel(ctx, b, p.SystemID, 0, string(TaskTowerJob))
}

// towerJobStep is TaskTowerJob: at the tower, install.
func (m *Mod) towerJobStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	done := func(note string) (bool, error) {
		if note != "" {
			m.logBot(b.CharID, logInfo, "industry", "%s", note)
		}
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	tj, err := m.loadTowerJob(ctx, d.PosTowerJobArray)
	if err != nil {
		return false, err
	}
	if tj == nil || tj.Status != "ready" || tj.BuilderID != b.CharID {
		return done("the tower job it came for was taken or closed")
	}
	p, _ := m.loadTowerPlan(ctx, tj.PlanID)
	if p == nil {
		return done("the tower is gone")
	}
	if b.SystemID != p.SystemID || b.StationID != 0 {
		return false, m.beginTravel(ctx, b, p.SystemID, 0, string(TaskTowerJob))
	}
	if m.embodyTowerPrep(b.CharID, p.MoonID) {
		d.LegEndsAt = clock.Unix() + navLegHoldSec
		return false, saveState(ctx, m.db, st)
	}
	// An unwatched ship is put at the tower it is working at, so a projection built afterwards
	// appears there and not on the gate it came in by.
	m.parkAtTower(ctx, b, &haulLeg{PlanID: p.PlanID, ToID: p.TowerID})
	jobID, fees, err := m.installTowerJob(ctx, b, tj)
	if err != nil {
		if ctx.Err() != nil {
			return false, err
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_towerjobs SET builderID = 0, claimedAt = 0 WHERE arrayID = ?`, tj.ArrayID)
		return done("the tower job would not install: " + err.Error())
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_towerjobs SET status = 'installed', jobID = ? WHERE arrayID = ?`, jobID, tj.ArrayID)
	if Chatter() {
		kind, product := workKindOfActivity(tj.ActivityID), tj.BPTypeID
		if kind == workKindBuild {
			_ = m.db.QueryRowContext(ctx, `SELECT productTypeID FROM invBlueprintTypes WHERE blueprintTypeID = ?`,
				tj.BPTypeID).Scan(&product)
		}
		m.chatterCorpJob(ctx, b.CharID, kind, tj.Runs, m.typeNameOr(ctx, product), m.typeNameOr(ctx, tj.BPTypeID), jobID)
	}
	return done(fmt.Sprintf("installed %d run(s) of %s at its corporation's tower (fee %s, paid by the corporation)",
		tj.Runs, m.typeNameOr(ctx, tj.BPTypeID), iskString(fees)))
}

// installTowerJob is installCorpJob for an array: the S&I rules through
// db.ResolveStructureCorpJob, the array's own line, the corporation's wallet.
func (m *Mod) installTowerJob(ctx context.Context, b *botRun, tj *towerJob) (int64, float64, error) {
	var bpItem int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT e.itemID FROM entity e LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE e.locationID = ? AND e.typeID = ? AND e.flag <> 111
		 ORDER BY COALESCE(b.copy, 0) DESC, e.itemID LIMIT 1`,
		tj.ArrayID, tj.BPTypeID).Scan(&bpItem)
	if bpItem == 0 {
		return 0, 0, errors.New("the print is not in the array")
	}
	scj, why := evedb.ResolveStructureCorpJob(ctx, m.db, b.CharID, tj.ArrayID, bpItem, evedb.FlagCorpHangarDiv1, evedb.FlagCorpHangarDiv1)
	if why != "" {
		return 0, 0, lineRefusal{why}
	}
	bpType, err := evedb.GetBlueprintTypeInfo(ctx, m.db, tj.BPTypeID)
	if err != nil {
		return 0, 0, err
	}
	installedBP, _, _, err := evedb.EnsureBlueprintSingleton(ctx, m.db, bpItem)
	if err != nil {
		return 0, 0, err
	}
	attrs, err := evedb.GetBlueprintAttributes(ctx, m.db, installedBP)
	if err != nil {
		return 0, 0, err
	}
	act := int64(tj.ActivityID)
	outputType := bpType.ProductTypeID
	if act != 1 {
		outputType = tj.BPTypeID
	}
	line, err := m.assemblyLineAt(ctx, b, tj.ArrayID, act, outputType)
	if err != nil {
		return 0, 0, err
	}
	runs := tj.Runs
	licensed := 0
	var durationSec float64
	peSkill := 0
	if act == 1 && attrs.Copy == 1 {
		// A copy builds at most what it has left (installCorpJob's rule).
		if attrs.RunsRemaining <= 0 {
			return 0, 0, lineRefusal{"the tower's copy has no runs left"}
		}
		runs = min(runs, attrs.RunsRemaining)
		licensed = attrs.RunsRemaining
	} else if act != 1 && attrs.Copy == 1 {
		return 0, 0, lineRefusal{"a copy cannot be researched or copied"}
	}
	if act == 1 {
		industrySkill, _ := m.charSkillLevelFor(ctx, b.CharID, skillIndustry)
		peSkill, _ = m.charSkillLevelFor(ctx, b.CharID, skillProductionEff)
		durationSec = manufactureDurationSec(bpType, attrs.ProductivityLevel, industrySkill, runs, line)
	} else {
		current, skillID := attrs.MaterialLevel, int64(skillMetallurgy)
		if act == ramActivityPEResearch {
			current, skillID = attrs.ProductivityLevel, int64(skillResearch)
		}
		lvl, _ := m.charSkillLevelFor(ctx, b.CharID, skillID)
		durationSec = researchDurationSec(bpType, act, current, max(runs, 1), lvl, line)
	}
	if durationSec <= 0 {
		return 0, 0, lineRefusal{"no production time"}
	}
	now := win32Now()
	beginTime := max(now, line.Info.NextFreeTime)
	endTime := beginTime + int64(durationSec*10_000_000)
	fees := math.Max(0, line.Info.CostInstall+line.Info.CostPerHour*durationSec/3600)

	// The runs go under the builder's name, where the wage sweep reads them.
	if w, err := m.queryWork(ctx, `WHERE workID = ?`, tj.WorkID); err == nil && len(w) > 0 {
		m.releaseWorkClaim(ctx, tj.WorkID, tj.ArrayID)
		_, _ = m.claimWorkRuns(ctx, w[0], b.CharID, runs)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	charged, corpBal, err := evedb.TryChargeCorpISK(ctx, tx, scj.CorpID, fees)
	if err != nil {
		return 0, 0, err
	}
	if !charged {
		return 0, 0, fmt.Errorf("the corporation cannot afford the %.0f ISK installation fee", fees)
	}
	if act == 1 {
		if _, err := evedb.ConsumeMaterialsFrom(ctx, tx, scj.CorpID, scj.ArrayID, scj.MatFlag, tj.BPTypeID,
			attrs.MaterialLevel, peSkill, runs, line.MatMul); err != nil {
			return 0, 0, err
		}
	}
	if err := evedb.MoveBlueprintToFlag(ctx, tx, installedBP, 111); err != nil {
		return 0, 0, err
	}
	jobID, err := evedb.InsertJobEx(ctx, tx, line.Info.AssemblyLineID, scj.ArrayID, b.CharID, installedBP,
		now, beginTime, endTime, runs, int(act), outputType, licensed, scj.SolarSystemID, scj.OutFlag, scj.CorpID, scj.BPFlag)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	m.bindWorkClaim(ctx, tj.WorkID, b.CharID, jobID)
	if fees > 0 {
		refType := researchRefType(act)
		if act == 1 {
			refType = 56
		}
		_ = evedb.InsertJournalEntry(ctx, m.db, scj.CorpID, refType, b.CharID, scj.CorpID,
			-fees, corpBal, "Corporation industry job installation")
		m.corpBook(ctx, scj.CorpID, lineJobFees, -fees, b.CharID, "tower job fee")
	}
	return jobID, fees, nil
}
