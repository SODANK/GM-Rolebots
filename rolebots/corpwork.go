package rolebots

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The work board holds paid corporation work and is the one place labour is paid.
// The production power (corpproduction.go) posts work orders, each with a wage per run
// committed against the corporation's wallet (corpcommit.go).
// The wage sweep reads delivered corporation jobs from ramJobs and matches each to an open
// order by product, station and activity, so a module builder and a human using the S&I
// window are paid alike. rolebot_corp_wages records a job on first read, so it is paid once.
// A job not on the board pays nothing. corpOffPlanStrikes of them in a week benches the
// member for corpBenchSec. Humans hear of the board by mail, at most once a day.

const (
	corpWageSweepTicks  = 60
	corpWorkClaimSec    = 2 * 3600
	corpOffPlanStrikes  = 3
	corpOffPlanWindow   = 7 * 86400
	corpBenchSec        = 14 * 86400
	corpBoardMailMinSec = 3 * 86400 // the board is on /corp/industry; mail is the reminder
	corpBoardMaxLines   = 12

	workKindBuild = "build"
	workKindME    = "me"
	workKindPE    = "pe"
	// workKindCopy is a copy order (ramActivities 5): RunsWanted counts copies, each carrying
	// copyRunsFor runs (bpcopy.go). Copies go to the library.
	workKindCopy   = "copy"
	workKindRefine = "refine"

	workOpen = "open"
	workDone = "done"
	workVoid = "void"
)

// corpWork is one work order.
type corpWork struct {
	WorkID        int64
	CorpID        int64
	Kind          string
	ActivityID    int
	BPTypeID      int64
	ProductTypeID int64
	StationID     int64
	RunsWanted    int
	RunsDone      int
	WagePerRun    float64
	Purpose       string
	Priority      int
	Status        string
	PostedAt      int64
	Note          string
	// PostedBy and Source: the chair's own work (corpcommanddesk.go) names its poster and
	// carries source "human", which the production review's pruning skips.
	PostedBy int64
	Source   string
	// BPItemID names the one original a research or copy order is for (0 = any print of the
	// type). Two originals of a type run in parallel only when each order is pinned.
	BPItemID int64
	// Departments as customers (corprequest.go). RequesterArm is the arm the work is for
	// (empty: read from the purpose). DestFlag and DestStationID say where the output goes
	// (0 = default). ReqID is the requisition it fills.
	RequesterArm  string
	DestFlag      int64
	DestStationID int64
	ReqID         int64
}

// RunsLeft is how many runs the order still wants.
func (w corpWork) RunsLeft() int { return max(0, w.RunsWanted-w.RunsDone) }

// workActivity is the ramJobs activity a kind of work is carried out as.
func workActivity(kind string) int {
	switch kind {
	case workKindBuild:
		return 1
	case workKindME:
		return 4
	case workKindPE:
		return 3
	case workKindCopy:
		return ramActivityCopying
	}
	return 0
}

// postWork puts an order on the board and, for a cash-paying board, commits its wages.
func (m *Mod) postWork(ctx context.Context, w corpWork) (int64, error) {
	if w.Purpose == "" {
		w.Purpose = "market"
	}
	if w.Source == "" {
		w.Source = fleetOrderDesk
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_work
			(corporationID, kind, activityID, bpTypeID, productTypeID, stationID, runsWanted,
			 wagePerRun, purpose, priority, status, postedAt, note, postedBy, source, bpItemID,
			 requesterArm, destFlag, destStationID, reqID)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.CorpID, w.Kind, workActivity(w.Kind), w.BPTypeID, w.ProductTypeID, w.StationID, w.RunsWanted,
		w.WagePerRun, w.Purpose, w.Priority, clock.Unix(), clampText(w.Note, 255), w.PostedBy, w.Source, w.BPItemID,
		string(workRequesterArm(w)), w.DestFlag, w.DestStationID, w.ReqID)
	if err != nil {
		return 0, fmt.Errorf("postWork: %w", err)
	}
	id, _ := res.LastInsertId()
	// A bot-run corporation pays labour from the week's profit (corppayroll.go), so nothing
	// is held back. A human chair's board pays cash on delivery and keeps the commit.
	if payrollManaged(w.CorpID) {
		return id, nil
	}
	if err := m.openCommit(ctx, w.CorpID, "work", id, w.WagePerRun*float64(w.RunsWanted)); err != nil {
		return id, err
	}
	return id, nil
}

// closeWork ends an order and releases what is left of its wages.
func (m *Mod) closeWork(ctx context.Context, workID int64, status string) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_work SET status = ?, closedAt = ? WHERE workID = ? AND status = 'open'`,
		status, clock.Unix(), workID)
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_work_claims WHERE workID = ?`, workID)
	m.closeCommit(ctx, "work", workID)
}

// openWorkFor reads a corporation's open orders, highest priority first.
func (m *Mod) openWorkFor(ctx context.Context, corpID int64) ([]corpWork, error) {
	return m.queryWork(ctx, `WHERE corporationID = ? AND status = 'open'`, corpID)
}

func (m *Mod) queryWork(ctx context.Context, where string, args ...any) ([]corpWork, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT workID, corporationID, kind, activityID, bpTypeID, productTypeID, stationID,
		       runsWanted, runsDone, wagePerRun, purpose, priority, status, postedAt, note,
		       postedBy, source, bpItemID, requesterArm, destFlag, destStationID, reqID
		  FROM rolebot_corp_work `+where+`
		 ORDER BY priority DESC, postedAt, workID`, args...)
	if err != nil {
		return nil, fmt.Errorf("queryWork: %w", err)
	}
	defer rows.Close()
	var out []corpWork
	for rows.Next() {
		var w corpWork
		if err := rows.Scan(&w.WorkID, &w.CorpID, &w.Kind, &w.ActivityID, &w.BPTypeID, &w.ProductTypeID,
			&w.StationID, &w.RunsWanted, &w.RunsDone, &w.WagePerRun, &w.Purpose, &w.Priority,
			&w.Status, &w.PostedAt, &w.Note, &w.PostedBy, &w.Source, &w.BPItemID,
			&w.RequesterArm, &w.DestFlag, &w.DestStationID, &w.ReqID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ── claims (bots only) ───────────────────────────────────────────────────────

// claimWorkRuns reserves up to want runs of an order for a bot and returns how many it got.
// A bound claim counts only until its job is paid (workClaimedExceptOpen). The caller's own
// bound claims count against it, so a builder fanning one order over several lines
// (copylanes.go) does not re-claim runs already building.
func (m *Mod) claimWorkRuns(ctx context.Context, w corpWork, charID int64, want int) (int, error) {
	free := w.RunsLeft() - m.workClaimedExceptOpen(ctx, w.WorkID, charID)
	if free <= 0 {
		return 0, nil
	}
	got := min(want, free)
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_work_claims (workID, characterID, runs, claimedAt)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE runs = VALUES(runs), jobID = 0, claimedAt = VALUES(claimedAt)`,
		w.WorkID, charID, got, clock.Unix())
	if err != nil {
		return 0, fmt.Errorf("claimWorkRuns: %w", err)
	}
	return got, nil
}

// bindWorkClaim records the job a claim became.
func (m *Mod) bindWorkClaim(ctx context.Context, workID, charID, jobID int64) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_work_claims SET jobID = ? WHERE workID = ? AND characterID = ? AND jobID = 0`,
		jobID, workID, charID)
}

// workClaimedExceptOpen returns the runs of an order held by anyone but the caller's own
// unbound claim: another pilot's unbound claim inside corpWorkClaimSec, or any bound one
// whose runs are not yet in runsDone (job on a line, or delivered and not yet paid).
func (m *Mod) workClaimedExceptOpen(ctx context.Context, workID, charID int64) int {
	var claimed int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(c.runs), 0) FROM rolebot_corp_work_claims c
		  LEFT JOIN ramJobs j ON j.jobID = c.jobID
		  LEFT JOIN rolebot_corp_wages g ON g.jobID = c.jobID
		 WHERE c.workID = ? AND (c.characterID <> ? OR c.jobID <> 0)
		   AND ((c.jobID <> 0 AND (j.completedStatusID IN (0, 1) OR
		                           (j.completedStatusID = 101 AND g.jobID IS NULL)))
		        OR (c.jobID = 0 AND c.claimedAt > ?))`,
		workID, charID, clock.Unix()-corpWorkClaimSec).Scan(&claimed)
	return claimed
}

// corpWorkHoldSec is how long an order the counter refused (every slot busy, or the
// corporation short of the fee) is passed over before anyone tries it again.
const corpWorkHoldSec = 30 * 60

// workHolds is workID to the unix time the hold lifts. It is in memory only.
var workHolds sync.Map

// holdWork passes an order over for corpWorkHoldSec.
func holdWork(workID int64) {
	workHolds.Store(workID, clock.Unix()+corpWorkHoldSec)
}

func init() {
	// A hold on a closed order is never read again (workHeld deletes on read), so lifted
	// ones are swept.
	registerSweep("workHoldsSweep", func() {
		now := clock.Unix()
		workHolds.Range(func(k, v any) bool {
			if now >= v.(int64) {
				workHolds.Delete(k)
			}
			return true
		})
	})
}

// workHeld reports whether an order is on hold.
func workHeld(workID int64) bool {
	v, ok := workHolds.Load(workID)
	if !ok {
		return false
	}
	if clock.Unix() >= v.(int64) {
		workHolds.Delete(workID)
		return false
	}
	return true
}

// dropJobClaim drops the claim a delivered job carried, once it is paid.
func (m *Mod) dropJobClaim(ctx context.Context, jobID int64) {
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_work_claims WHERE jobID = ?`, jobID)
}

// pruneDeadClaims drops every bound claim whose job is gone, aborted, or delivered and
// already paid. A delivered job the sweep could not pay yet keeps its claim, so a second
// bot does not build the same runs.
func (m *Mod) pruneDeadClaims(ctx context.Context) {
	if _, err := m.db.ExecContext(ctx, `
		DELETE c FROM rolebot_corp_work_claims c
		  LEFT JOIN ramJobs j ON j.jobID = c.jobID
		  LEFT JOIN rolebot_corp_wages g ON g.jobID = c.jobID
		 WHERE c.jobID <> 0
		   AND (j.jobID IS NULL OR j.completedStatusID NOT IN (0, 1, 101) OR
		        (j.completedStatusID = 101 AND g.jobID IS NOT NULL))`); err != nil {
		rlogf("rolebots: pruning dead corp work claims: %v", err)
	}
}

// releaseWorkClaim drops a claim that never became a job.
func (m *Mod) releaseWorkClaim(ctx context.Context, workID, charID int64) {
	_, _ = m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_work_claims WHERE workID = ? AND characterID = ? AND jobID = 0`,
		workID, charID)
}

// ── the wage sweep ───────────────────────────────────────────────────────────

var (
	corpWageSweepBusy   atomic.Bool
	deadClaimsPrunedAt  atomic.Int64
	printSalesSettledAt atomic.Int64
)

// startCorpWageSweep registers the pass that pays delivered corporation jobs.
func (m *Mod) startCorpWageSweep() {
	m.registerPeriodic("rolebot-corp-wages", corpWageSweepTicks, func(uint64) bool {
		if !Master() || !corpWageSweepBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("corp wage sweep", func() {
			defer corpWageSweepBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := m.corpWageSweep(ctx); err != nil {
				rlogf("rolebots: corp wage sweep: %v", err)
			}
		})
		return true
	})
}

// managedCorpIDs returns every corporation a bot CEO runs.
func managedCorpIDs() []int64 {
	p := ceoIndex.Load()
	if p == nil {
		return nil
	}
	out := make([]int64, 0, len(*p))
	for corpID := range *p {
		out = append(out, corpID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// deliveredCorpJob is one corporation job the sweep has not read yet.
type deliveredCorpJob struct {
	JobID, CorpID, InstallerID, StationID, OutputTypeID int64
	BPTypeID                                            int64 // the print's type, 0 when it is gone
	BPItemID                                            int64 // the print the job ran on
	ActivityID, Runs                                    int
}

// matchWork finds the open order a delivered job was carrying out, or nil. An order pinned
// to one print (BPItemID) is paid only by a job on that print and is matched first; an
// unpinned order takes any print of its type. Pure.
func matchWork(j deliveredCorpJob, open []corpWork) *corpWork {
	for pass := 0; pass < 2; pass++ {
		for i := range open {
			w := &open[i]
			if w.Status != workOpen || w.RunsLeft() <= 0 || workActivity(w.Kind) != j.ActivityID {
				continue
			}
			if w.StationID != 0 && w.StationID != j.StationID {
				continue
			}
			pinned := w.BPItemID != 0
			if pinned && w.BPItemID != j.BPItemID {
				continue // another print's order
			}
			if pinned != (pass == 0) {
				continue // pinned orders on the first pass, the rest on the second
			}
			switch w.Kind {
			case workKindBuild:
				if w.ProductTypeID == j.OutputTypeID {
					return w
				}
			case workKindME, workKindPE, workKindCopy:
				if w.BPTypeID == j.OutputTypeID {
					return w
				}
			}
		}
	}
	return nil
}

// wageFor is what a delivered job earns against its order: the runs still wanted, at the
// order's rate. A research job's runs are the levels it researched (postResearchWork,
// researchWage).
func wageFor(w corpWork, jobRuns int) (runs int, wage float64) {
	runs = min(jobRuns, w.RunsLeft())
	return runs, float64(runs) * w.WagePerRun
}

// corpWageSweep reads every newly delivered corporation job and credits or pays it.
func (m *Mod) corpWageSweep(ctx context.Context) error {
	corps := managedCorpIDs()
	// Plus any corporation whose chair has work posted (corpcommanddesk.go).
	seen := map[int64]bool{}
	for _, id := range corps {
		seen[id] = true
	}
	for _, id := range m.workDeskCorpIDs(ctx) {
		if !seen[id] {
			corps = append(corps, id)
		}
	}
	if len(corps) == 0 {
		return nil
	}
	// Deliver first: a finished job is only paid once it is handed back.
	m.deliverReadyCorpJobs(ctx, corps)
	ph := strings.TrimSuffix(strings.Repeat("?,", len(corps)), ",")
	args := make([]any, 0, len(corps))
	for _, id := range corps {
		args = append(args, id)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT j.jobID, j.ownerID, j.installerID, j.containerID, j.outputTypeID, j.activityID, j.runs,
		       COALESCE(e.typeID, 0), j.installedItemID
		  FROM ramJobs j
		  LEFT JOIN entity e ON e.itemID = j.installedItemID
		  LEFT JOIN rolebot_corp_wages w ON w.jobID = j.jobID
		 WHERE j.completedStatusID = 101 AND j.ownerID IN (`+ph+`) AND w.jobID IS NULL
		 ORDER BY j.jobID
		 LIMIT 200`, args...)
	if err != nil {
		return fmt.Errorf("delivered corp jobs: %w", err)
	}
	var jobs []deliveredCorpJob
	for rows.Next() {
		var j deliveredCorpJob
		if rows.Scan(&j.JobID, &j.CorpID, &j.InstallerID, &j.StationID, &j.OutputTypeID, &j.ActivityID, &j.Runs,
			&j.BPTypeID, &j.BPItemID) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	open := map[int64][]corpWork{}
	var treasurers sync.Map
	for _, j := range jobs {
		jctx := m.treasuryActorCtx(ctx, j.CorpID, &treasurers)
		if _, ok := open[j.CorpID]; !ok {
			ws, err := m.openWorkFor(ctx, j.CorpID)
			if err != nil {
				return err
			}
			open[j.CorpID] = ws
		}
		ws := open[j.CorpID]
		w := matchWork(j, ws)
		if err := m.payCorpJob(jctx, j, w); err != nil {
			if errors.Is(err, evedb.ErrInsufficientFunds) {
				continue // the wallet recovers; the job is read again next pass
			}
			return err
		}
		if w != nil {
			runs, _ := wageFor(*w, j.Runs)
			w.RunsDone += runs
		}
	}
	// Delivery runs every minute; the claim prune and the print-sale settlement need not.
	sweepNow := clock.Unix()
	if sweepNow-deadClaimsPrunedAt.Load() >= 600 {
		deadClaimsPrunedAt.Store(sweepNow)
		m.pruneDeadClaims(ctx)
	}
	if sweepNow-printSalesSettledAt.Load() >= 300 {
		printSalesSettledAt.Store(sweepNow)
		m.settlePrintSales(ctx)
	}
	return nil
}

// payCorpJob credits or pays one delivered job, or records it as off the board.
func (m *Mod) payCorpJob(ctx context.Context, j deliveredCorpJob, w *corpWork) error {
	c, _ := getCEO(ctx, m.db, j.CorpID)
	if w == nil {
		if _, err := m.db.ExecContext(ctx, `
			INSERT IGNORE INTO rolebot_corp_wages (jobID, corporationID, workID, characterID, runs, wage, paidAt)
			VALUES (?, ?, 0, ?, ?, 0, ?)`, j.JobID, j.CorpID, j.InstallerID, j.Runs, clock.Unix()); err != nil {
			return err
		}
		m.dropJobClaim(ctx, j.JobID)
		// Only the corporation's own library print, run at one of its Industry bases, is policed.
		// Other corporation jobs never earn a strike.
		if m.isIndustryBase(ctx, j.CorpID, j.StationID) && j.BPTypeID != 0 &&
			m.libraryHas(ctx, j.CorpID, j.BPTypeID) &&
			!m.assignmentCovers(ctx, j.CorpID, j.InstallerID, j.OutputTypeID) {
			m.strikeOffPlan(ctx, c, j)
		}
		return nil
	}
	runs, wage := wageFor(*w, j.Runs)
	name := m.charNameOr(ctx, j.InstallerID)
	what := fmt.Sprintf("%s: %d run(s) of %s", name, runs, m.typeNameOr(ctx, j.OutputTypeID))
	managed := payrollManaged(j.CorpID)
	if managed {
		// A bot-run corporation pays by the weekly payroll (corppayroll.go): the run is credited
		// to the member's labour record and no ISK moves. The rank multiplier applies to the cheque.
		wage = math.Floor(wage)
		if wage >= 1 && j.InstallerID != 0 {
			m.creditLabour(ctx, j.CorpID, j.InstallerID, w.Kind, wage, what)
		}
		// Releases what an order posted before the payroll still holds.
		m.spendCommit(ctx, "work", w.WorkID, wage)
	} else {
		// A human chair's board pays cash on delivery. Rank pays (corpmerit.go), 1 with the
		// ladder off.
		wage = math.Floor(wage * m.meritPayFor(ctx, j.CorpID, j.InstallerID))
		if wage >= 1 && j.InstallerID != 0 {
			_, bal, err := m.corpWithdraw(ctx, j.CorpID, j.InstallerID, wage,
				"Corporation work: "+m.typeNameOr(ctx, j.OutputTypeID))
			if err != nil {
				return err
			}
			m.pushBalance(j.InstallerID, bal)
			m.spendCommit(ctx, "work", w.WorkID, wage)
			m.corpBook(ctx, j.CorpID, lineWages, -wage, j.InstallerID, what)
		}
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT IGNORE INTO rolebot_corp_wages (jobID, corporationID, workID, characterID, runs, wage, paidAt)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, j.JobID, j.CorpID, w.WorkID, j.InstallerID, runs, wage, clock.Unix()); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_work SET runsDone = runsDone + ? WHERE workID = ?`, runs, w.WorkID); err != nil {
		return err
	}
	// The runs are in runsDone now; the claim would count them a second time.
	m.dropJobClaim(ctx, j.JobID)
	// File the output where it was asked for.
	m.fileWorkOutput(ctx, c, j, w, runs)
	m.noteAssignmentBuild(ctx, j.CorpID, j.InstallerID, w.WorkID, runs)
	if w.Kind == workKindBuild {
		m.noteCorpBuildCost(ctx, j, w, wage)
	}
	if w.RunsDone+runs >= w.RunsWanted {
		m.closeWork(ctx, w.WorkID, workDone)
	}
	if c != nil {
		verb := "paid"
		if managed {
			verb = "credited"
		}
		m.ceoLog(ctx, c, powerProduction, "wage", j.InstallerID, wage,
			"%s %s %s for %d run(s) of %s", verb, name, iskString(wage), runs, m.typeNameOr(ctx, j.OutputTypeID))
	}
	return nil
}

// strikeOffPlan counts a job the board never asked for and benches a member who keeps
// doing it.
func (m *Mod) strikeOffPlan(ctx context.Context, c *corpCEO, j deliveredCorpJob) {
	if j.InstallerID == 0 {
		return
	}
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_wages
		 WHERE corporationID = ? AND characterID = ? AND workID = 0 AND paidAt > ?`,
		j.CorpID, j.InstallerID, clock.Unix()-corpOffPlanWindow).Scan(&n)
	if n < corpOffPlanStrikes {
		return
	}
	why := fmt.Sprintf("%d corporation jobs in a week that were not on the work board", n)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_bench (corporationID, characterID, untilAt, why) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE untilAt = VALUES(untilAt), why = VALUES(why)`,
		j.CorpID, j.InstallerID, clock.Unix()+corpBenchSec, why); err != nil {
		return
	}
	if c != nil {
		m.ceoLog(ctx, c, powerProduction, "bench", j.InstallerID, 0,
			"benched %s from corporation work for two weeks — %s", m.charNameOr(ctx, j.InstallerID), why)
	}
}

// noteCorpBuildCost folds one delivered build into the per-type cost the company store
// prices from: materials at market value, the wage and the fee allowance.
func (m *Mod) noteCorpBuildCost(ctx context.Context, j deliveredCorpJob, w *corpWork, wage float64) {
	mats, err := evedb.GetRequiredMaterials(ctx, m.db, w.BPTypeID, 1)
	if err != nil {
		return
	}
	ids := []int64{w.ProductTypeID}
	for _, mt := range mats {
		ids = append(ids, mt.TypeID)
	}
	vals, err := evedb.TypeValues(ctx, m.db, ids)
	if err != nil {
		return
	}
	var portion int64 = 1
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(portionSize, 1) FROM invTypes WHERE typeID = ?`, w.ProductTypeID).Scan(&portion)
	units := int64(j.Runs) * max(portion, 1)
	cost := wage + oreDeskFeeFrac*vals[w.ProductTypeID].Value*float64(units)
	for _, mt := range mats {
		cost += vals[mt.TypeID].Value * float64(mt.Quantity) * mt.DamagePerJob * float64(j.Runs)
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_cost (corporationID, typeID, units, cost) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE units = units + VALUES(units), cost = cost + VALUES(cost)`,
		j.CorpID, w.ProductTypeID, units, cost)
}

// ── the Production Board mail ────────────────────────────────────────────────

// corpBoardText renders the board. EVE-HTML wants <br>, never a newline (the inbox
// flattens those). Pure.
func corpBoardText(corpName, factory string, work []corpWork, names map[int64]string, extra []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s — Production Board</b><br>", corpName)
	if factory != "" {
		fmt.Fprintf(&b, "Factory: %s<br>", factory)
	}
	b.WriteString("<br>Work on this board is credited to your labour record at the rate shown, and the corporation ")
	b.WriteString("pays its members once a week out of the week's profit, split by the merit they earned. ")
	b.WriteString("Blueprints are in the <b>Blueprint Library</b> division and materials in <b>Materials</b>; ")
	b.WriteString("install as a corporation job and deliver to <b>Production</b>.<br><br>")
	if len(work) == 0 {
		b.WriteString("Nothing is posted right now.<br>")
	}
	for i, w := range work {
		if i >= corpBoardMaxLines {
			fmt.Fprintf(&b, "… and %d more.<br>", len(work)-i)
			break
		}
		what := names[w.ProductTypeID]
		switch w.Kind {
		case workKindME:
			what = "research ME on " + names[w.BPTypeID]
		case workKindPE:
			what = "research PE on " + names[w.BPTypeID]
		case workKindCopy:
			what = "copy " + names[w.BPTypeID]
		case workKindRefine:
			what = "refine " + names[w.ProductTypeID]
		}
		unit := "run"
		switch w.Kind {
		case workKindME, workKindPE:
			unit = "level"
		case workKindCopy:
			unit = "copy"
		}
		fmt.Fprintf(&b, "• %s — %d %s(s) left, credited %s per %s", what, w.RunsLeft(), unit, iskString(w.WagePerRun), unit)
		if w.Purpose == "srp" {
			b.WriteString(" <i>(replacing a member's loss)</i>")
		}
		b.WriteString("<br>")
	}
	for _, e := range extra {
		b.WriteString(e)
		b.WriteString("<br>")
	}
	return b.String()
}

// corpBoardMail mails the board to the corporation's human members when it changed, at
// most once a day.
func (m *Mod) corpBoardMail(ctx context.Context, c *corpCEO, p *corpPolicyRow, extra []string) {
	humans, err := int64Column(ctx, m, `
		SELECT ch.characterID FROM character_ ch
		  LEFT JOIN rolebot_characters b ON b.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0 AND b.characterID IS NULL`, c.CorpID)
	if err != nil || len(humans) == 0 {
		return
	}
	work, err := m.openWorkFor(ctx, c.CorpID)
	if err != nil {
		return
	}
	names := map[int64]string{}
	for _, w := range work {
		names[w.ProductTypeID] = m.typeNameOr(ctx, w.ProductTypeID)
		names[w.BPTypeID] = m.typeNameOr(ctx, w.BPTypeID)
	}
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return
	}
	factory := ""
	if st := m.corpFactoryStation(ctx, c.CorpID); st != 0 {
		factory = m.stationLabel(ctx, st)
	}
	body := corpBoardText(facts.Name, factory, work, names, extra)
	sum := sha1.Sum([]byte(body))
	hash := hex.EncodeToString(sum[:])
	if hash == p.BoardHash || clock.Unix()-p.BoardMailAt < corpBoardMailMinSec {
		return
	}
	sender := p.DirectorID
	if sender == 0 {
		sender = c.CharID
	}
	subject := mailSubject(facts.Name + " - Production Board")
	created := win32From(clock.Now())
	sent := 0
	for _, to := range humans {
		if to == sender || m.isSynthetic(to) {
			continue // a human Industry Director wrote it; an orphan bot reads nothing
		}
		msgID, err := evedb.SendEveMail(ctx, m.db, to, sender, subject, body, created)
		if err != nil {
			rlogf("rolebots: production board to %d: %v", to, err)
			continue
		}
		sent++
		if msgID != 0 {
			if h := modapi.Gangs(); h.PushMail != nil {
				h.PushMail(to, msgID, sender, subject, created)
			}
		}
	}
	// Stamped only when a letter went out, so a pass where every send failed tries again.
	if sent > 0 {
		p.BoardHash, p.BoardMailAt = hash, clock.Unix()
	}
}
