package rolebots

import (
	"context"
	"fmt"
	"math"

	"gurimoon/corp"
	evedb "gurimoon/db"
	serverram "gurimoon/server/ram"
)

// Corporation jobs, bot side: a contract builder working the board. A builder the module flies takes
// corporation work the way a human does in the client: it docks at the factory office, picks an open
// order the library holds a print for and the Materials division can feed, and installs a corporation
// job (the corporation's print, materials and fee, output delivered to the office). It spends no ISK
// of its own, and the wage sweep (corpwork.go) pays it on delivery.
// The install runs the core's own rule (evedb.ResolveStationCorpJob), so a bot is refused for the
// reason a player would be. This arm is tried first whenever a builder plans an industry project
// (industryStartProject). It also runs the deputy's purchase orders (corplibrary.go): a PO becomes a
// blueprint-buying project whose factory is the corporation's, and the print goes onto the library shelf.

const (
	corpWorkMaxJumps     = 8
	corpJobDeliverGrace  = 72 * 3600 // a human's job left undelivered this long is delivered for them
	ramActivityMECorp    = 4
	corpJobDeliverPerRun = 50
)

// corpJobSpec is one corporation job to install.
type corpJobSpec struct {
	CorpID, OfficeID, StationID int64
	BPItemID, BPTypeID          int64
	ActivityID                  int64
	Runs                        int
	// OutFlag is the hangar division the output is delivered into (0 =
	// Production): the work order's destination (workInstallFlag).
	OutFlag int64
}

// installCorpJob installs a station corporation job for a bot, mirroring
// installManufactureJob / installResearchJob with the corporation's print,
// materials and wallet.
func (m *Mod) installCorpJob(ctx context.Context, b *botRun, s corpJobSpec) (jobID int64, fees float64, err error) {
	var bpOwner, bpLoc, bpFlag int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT ownerID, locationID, flag FROM entity WHERE itemID = ?`, s.BPItemID).Scan(&bpOwner, &bpLoc, &bpFlag); err != nil {
		return 0, 0, fmt.Errorf("the library print is gone")
	}
	// A library print runs on Materials and delivers to Production, or to the division the order was
	// asked for (corprequest.go).
	matFlag, outFlag := int64(corpDivMaterials), int64(corpDivProduct)
	if s.OutFlag != 0 && flagDiv(s.OutFlag) != 0 {
		outFlag = s.OutFlag
	}
	scj, why := evedb.ResolveStationCorpJob(ctx, m.db, b.CharID, s.StationID, bpOwner, bpLoc, bpFlag,
		matFlag, outFlag)
	if why != "" {
		return 0, 0, lineRefusal{why}
	}
	bpType, err := evedb.GetBlueprintTypeInfo(ctx, m.db, s.BPTypeID)
	if err != nil {
		return 0, 0, err
	}
	installedBP, _, _, err := evedb.EnsureBlueprintSingleton(ctx, m.db, s.BPItemID)
	if err != nil {
		return 0, 0, fmt.Errorf("blueprint singleton: %w", err)
	}
	attrs, err := evedb.GetBlueprintAttributes(ctx, m.db, installedBP)
	if err != nil {
		return 0, 0, err
	}
	outputType := bpType.ProductTypeID
	if s.ActivityID != 1 {
		outputType = s.BPTypeID
	}
	line, err := m.assemblyLineAt(ctx, b, s.StationID, s.ActivityID, outputType)
	if err != nil {
		return 0, 0, err
	}

	manufacturing := s.ActivityID == 1
	baseID, advID := int64(serverram.SkillLaboratoryOperation), int64(serverram.SkillAdvancedLabOperation)
	if manufacturing {
		baseID, advID = int64(serverram.SkillMassProduction), int64(serverram.SkillAdvancedMassProduction)
	}
	baseSkill, _ := m.charSkillLevelFor(ctx, b.CharID, baseID)
	advSkill, _ := m.charSkillLevelFor(ctx, b.CharID, advID)
	if active, cerr := evedb.CountActiveJobs(ctx, m.db, b.CharID, manufacturing); cerr == nil &&
		active >= serverram.MaxConcurrentJobs(baseSkill, advSkill) {
		return 0, 0, fmt.Errorf("every one of its %s slots is busy", activityLineNoun(s.ActivityID))
	}
	if reqs, rerr := evedb.GetActivitySkillReqs(ctx, m.db, s.BPTypeID, int(s.ActivityID)); rerr == nil {
		for _, r := range reqs {
			if have, _ := m.charSkillLevelFor(ctx, b.CharID, r.SkillID); have < r.Level {
				return 0, 0, lineRefusal{fmt.Sprintf("missing required skill %d at level %d", r.SkillID, r.Level)}
			}
		}
	}

	runs := s.Runs
	licensed := 0
	var durationSec float64
	var peSkill int
	switch s.ActivityID {
	case 1:
		if bpType.MaxProductionLimit > 0 && int64(runs) > bpType.MaxProductionLimit {
			runs = int(bpType.MaxProductionLimit)
		}
		// A copy builds at most what it has left (installManufactureJob's
		// backstop, for the corporation's prints).
		if attrs.Copy == 1 {
			if attrs.RunsRemaining <= 0 {
				return 0, 0, lineRefusal{"the library copy has no runs left"}
			}
			runs = min(runs, attrs.RunsRemaining)
			licensed = attrs.RunsRemaining
		}
		industrySkill, _ := m.charSkillLevelFor(ctx, b.CharID, skillIndustry)
		peSkill, _ = m.charSkillLevelFor(ctx, b.CharID, skillProductionEff)
		durationSec = manufactureDurationSec(bpType, attrs.ProductivityLevel, industrySkill, runs, line)
	case ramActivityCopying:
		if attrs.Copy == 1 {
			return 0, 0, lineRefusal{"a copy cannot be copied"}
		}
		f, ferr := m.printFactsFor(ctx, s.BPTypeID)
		if ferr != nil {
			return 0, 0, ferr
		}
		runs = max(1, runs)
		licensed = int(f.CopyRuns)
		science, _ := m.charSkillLevelFor(ctx, b.CharID, skillScience)
		durationSec = copyDurationSec(bpType, runs, f.CopyRuns, science, line)
		if durationSec <= 0 {
			return 0, 0, lineRefusal{"this print carries no copy time"}
		}
	default:
		// Research takes every level the order asked for in one job, however
		// long that holds the line: the corporation sized the order.
		runs = max(1, runs)
		current, skillID := attrs.MaterialLevel, int64(skillMetallurgy)
		if s.ActivityID == ramActivityPEResearch {
			current, skillID = attrs.ProductivityLevel, int64(skillResearch)
		}
		lvl, _ := m.charSkillLevelFor(ctx, b.CharID, skillID)
		durationSec = researchDurationSec(bpType, s.ActivityID, current, runs, lvl, line)
		if durationSec <= 0 {
			return 0, 0, lineRefusal{"this print carries no research time"}
		}
	}

	now := win32Now()
	beginTime := max(now, line.Info.NextFreeTime)
	endTime := beginTime + int64(durationSec*10_000_000)
	standing := m.npcCorpStanding(ctx, b.CharID, line.Info.OwnerID)
	fees = serverram.AdjustCostForStanding(
		line.Info.CostInstall+line.Info.CostPerHour*durationSec/3600,
		standing, line.Info.DiscountPerGoodStanding, line.Info.SurchargePerBadStanding)
	solarSystemID, _ := evedb.GetSolarSystemForStation(ctx, m.db, s.StationID)

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
	if manufacturing {
		if _, err := evedb.ConsumeMaterialsFrom(ctx, tx, scj.CorpID, scj.OfficeID, scj.MatFlag, s.BPTypeID,
			attrs.MaterialLevel, peSkill, runs, line.MatMul); err != nil {
			return 0, 0, err
		}
	}
	if err := evedb.MoveBlueprintToFlag(ctx, tx, installedBP, 111); err != nil {
		return 0, 0, err
	}
	jobID, err = evedb.InsertJobEx(ctx, tx, line.Info.AssemblyLineID, s.StationID, b.CharID, installedBP,
		now, beginTime, endTime, runs, int(s.ActivityID), outputType, licensed, solarSystemID,
		scj.OutFlag, scj.CorpID, scj.BPFlag)
	if err != nil {
		return 0, 0, err
	}
	if err := evedb.SetJobOutputLocation(ctx, tx, jobID, scj.OfficeID); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	if fees > 0 {
		refType := researchRefType(s.ActivityID)
		if manufacturing {
			refType = 56
		}
		_ = evedb.InsertJournalEntry(ctx, m.db, scj.CorpID, refType, b.CharID, line.Info.OwnerID,
			-fees, corpBal, "Corporation industry job installation")
		if line.Info.OwnerID > 0 {
			if cerr := evedb.CreditCorpWallet(ctx, m.db, line.Info.OwnerID, fees); cerr == nil {
				if ownerBal, berr := evedb.GetCorpBalance(ctx, m.db, line.Info.OwnerID); berr == nil {
					_ = evedb.InsertJournalEntry(ctx, m.db, line.Info.OwnerID, refType,
						b.CharID, line.Info.OwnerID, fees, ownerBal, "Assembly line usage fee")
				}
			}
		}
		m.corpBook(ctx, scj.CorpID, lineJobFees, -fees, b.CharID, "job fee")
	}
	return jobID, fees, nil
}

// ── the arm ──────────────────────────────────────────────────────────────────

// isCorpBuilder reports whether a pilot holds the Factory Manager role in its
// corporation (director, builder, or an officer who holds it anyway).
func (m *Mod) isCorpBuilder(ctx context.Context, charID int64) bool {
	info, err := evedb.GetCorporationMemberInfo(ctx, m.db, charID)
	return err == nil && info != nil && info.Corprole&(corp.RoleFactoryManager|corp.RoleDirector) != 0
}

// corpWorkChoice is one feasible piece of work for a builder.
type corpWorkChoice struct {
	Work  corpWork
	Print libraryPrint
	Runs  int
}

// pickWorkPrint chooses the print an order runs on from the free originals (best ME first) and copies
// (most runs first) of its type. A build takes the smallest copy that covers this job's chunk
// (splitChunk; 0 = the whole runs left), else the largest, and an original only when no copy is free
// or coming (copyPending). Research and copying need an original; a pinned order takes that print. Pure.
func pickWorkPrint(originals []libraryPrint, copies []libraryCopy, w corpWork, act int64, copyPending bool, chunk int) (libraryPrint, *libraryCopy, bool) {
	if act == 1 {
		if chunk <= 0 {
			chunk = w.RunsLeft()
		}
		if len(copies) > 0 {
			best := -1
			for i, c := range copies {
				if c.Runs >= int64(chunk) && (best < 0 || c.Runs < copies[best].Runs) {
					best = i
				}
			}
			if best < 0 {
				best = 0
				for i, c := range copies {
					if c.Runs > copies[best].Runs {
						best = i
					}
				}
			}
			c := copies[best]
			return libraryPrint{ItemID: c.ItemID, BPTypeID: c.BPTypeID, StationID: c.StationID,
				Flag: c.Flag, ME: c.ME, PE: c.PE}, &c, true
		}
		if len(originals) > 0 && !copyPending {
			return originals[0], nil, true
		}
		return libraryPrint{}, nil, false
	}
	if w.BPItemID != 0 {
		for _, p := range originals {
			if p.ItemID == w.BPItemID {
				return p, nil, true
			}
		}
		return libraryPrint{}, nil, false
	}
	if len(originals) > 0 {
		return originals[0], nil, true
	}
	return libraryPrint{}, nil, false
}

// corpWorkFor finds the first open order this builder can carry out at the
// factory right now: a free print, the skills, and materials for at least
// one run (research needs none).
func (m *Mod) corpWorkFor(ctx context.Context, b *botRun, stationID, officeID, onlyWorkID int64, researchOnly bool) (*corpWorkChoice, string) {
	work, err := m.openWorkFor(ctx, b.CorpID)
	if err != nil || len(work) == 0 {
		return nil, "nothing on the work board"
	}
	// A shelf of copies alone still builds, so an empty originals list is no
	// reason to stop here.
	prints, err := m.libraryPrints(ctx, b.CorpID, stationID)
	if err != nil {
		return nil, "an empty library"
	}
	// A print waiting to be carried to another base is not on offer here
	// (corpprintmove.go).
	moving := m.movingItems(ctx, b.CorpID)
	byType := map[int64][]libraryPrint{}
	for _, p := range prints {
		if p.Flag != 111 && !moving[p.ItemID] {
			byType[p.BPTypeID] = append(byType[p.BPTypeID], p)
		}
	}
	// Copies the R&D arm made: a build order runs on one first (pickWorkPrint), so builders can work a
	// type while its originals copy and research.
	copiesByType := map[int64][]libraryCopy{}
	if cs, cerr := m.libraryCopies(ctx, b.CorpID, stationID); cerr == nil {
		for _, c := range cs {
			if c.Flag != 111 && !moving[c.ItemID] {
				copiesByType[c.BPTypeID] = append(copiesByType[c.BPTypeID], c)
			}
		}
	}
	// A type with copies on order waits for them rather than putting its
	// original on a production line (pickWorkPrint).
	copyPending := map[int64]bool{}
	for _, w := range work {
		if w.Kind == workKindCopy && w.RunsLeft() > 0 {
			copyPending[w.BPTypeID] = true
		}
	}
	peSkill, _ := m.charSkillLevelFor(ctx, b.CharID, skillProductionEff)
	// This builder's free manufacturing lines, read once and only if a build
	// order gets that far: the lanes an order may fan out over (copylanes.go).
	freeSlots := -1
	factory := int64(-1)
	why := "no order it can feed"
	for _, w := range work {
		if onlyWorkID != 0 && w.WorkID != onlyWorkID {
			continue
		}
		if onlyWorkID == 0 && workHeld(w.WorkID) {
			continue
		}
		if researchOnly && w.Kind == workKindBuild {
			continue
		}
		// An order is taken where it was posted; one posted nowhere in
		// particular (a chair order) belongs to the factory.
		at := w.StationID
		if at == 0 {
			if factory < 0 {
				factory = m.corpFactoryStation(ctx, b.CorpID)
			}
			at = factory
		}
		if at != stationID || w.RunsLeft() <= 0 {
			continue
		}
		act := int64(workActivity(w.Kind))
		if act == 0 {
			continue
		}
		// A build spreads over the free copies of its type and the builder's free lines: one job takes one
		// lane's share, and the re-plan after the install takes the next share on the next copy.
		chunk := 0
		if act == 1 {
			if freeSlots < 0 {
				if n, ferr := m.industryFreeSlots(ctx, b); ferr == nil {
					freeSlots = n
				} else {
					freeSlots = 1
				}
			}
			chunk = splitChunk(max(m.workRunsFree(ctx, w, b.CharID), 1), len(copiesByType[w.BPTypeID]), freeSlots)
		}
		p, viaCopy, ok := pickWorkPrint(byType[w.BPTypeID], copiesByType[w.BPTypeID], w, act, copyPending[w.BPTypeID], chunk)
		if !ok {
			switch {
			case w.BPItemID != 0:
				why = "its print is busy"
			case act == 1 && copyPending[w.BPTypeID]:
				why = "waiting on copies from the lab"
			}
			continue
		}
		if reqs, rerr := evedb.GetActivitySkillReqs(ctx, m.db, w.BPTypeID, int(act)); rerr == nil {
			short := false
			for _, r := range reqs {
				if have, _ := m.charSkillLevelFor(ctx, b.CharID, r.SkillID); have < r.Level {
					short = true
				}
			}
			if short {
				why = "lacks the skills for what is posted"
				continue
			}
		}
		if act != 1 {
			// A queued-out lab is found here, before the flight, and the base tells its department it is full
			// (corpbasepressure.go).
			if _, lerr := m.assemblyLineAt(ctx, b, stationID, act, w.BPTypeID); lerr != nil {
				m.noteQueuedOut(ctx, b.CorpID, b.CharID, lerr)
				why = lerr.Error()
				continue
			}
		}
		if act == ramActivityCopying {
			// No time trim: a player may queue a copy job of any length, so a
			// bot takes the order's copies up to the bulk batch and no less.
			return &corpWorkChoice{Work: w, Print: p, Runs: min(w.RunsLeft(), copyBatchMax)}, ""
		}
		if act != 1 {
			// Research: every level the order asks for, in one job. The board sized it (planLibraryWork, or the chair).
			return &corpWorkChoice{Work: w, Print: p, Runs: w.RunsLeft()}, ""
		}
		line, lerr := m.manufactureLineAt(ctx, b, stationID, w.ProductTypeID)
		if lerr != nil {
			m.noteQueuedOut(ctx, b.CorpID, b.CharID, lerr)
			why = lerr.Error()
			continue
		}
		needs, nerr := m.industryMaterialNeeds(ctx, w.BPTypeID, p.ME, 1, peSkill, line.MatMul)
		if nerr != nil {
			continue
		}
		runs := w.RunsLeft()
		if chunk > 0 {
			runs = min(runs, chunk)
		}
		matFlag := int64(corpDivMaterials)
		for _, n := range needs {
			if n.Needed <= 0 {
				continue
			}
			have, _ := evedb.GetMaterialQuantityAt(ctx, m.db, b.CorpID, officeID, matFlag, n.TypeID)
			runs = min(runs, int(have/n.Needed))
		}
		if viaCopy != nil && int64(runs) > viaCopy.Runs {
			runs = int(viaCopy.Runs)
		}
		if runs >= 1 {
			return &corpWorkChoice{Work: w, Print: p, Runs: runs}, ""
		}
		why = "the Materials division cannot feed a single run"
	}
	return nil, why
}

// corpWorkArm is the head of industryStartProject: a builder in a managed
// corporation takes the board's work before its own. handled = true means
// the arm took the pass (travel, install, or a PO) and the caller returns.
func (m *Mod) corpWorkArm(ctx context.Context, b *botRun) (bool, error) {
	return m.corpWorkArmFor(ctx, b, false)
}

// corpResearchArm is the arm for a builder whose manufacturing lines are
// full but whose lab slots are not: it may still take the board's ME and PE
// orders (industryPlan calls it before refusing for want of a line).
func (m *Mod) corpResearchArm(ctx context.Context, b *botRun) (bool, error) {
	if free, err := m.labFreeSlots(ctx, b); err != nil || free <= 0 {
		return false, err
	}
	return m.corpWorkArmFor(ctx, b, true)
}

// corpWorkArmFor is the arm itself; researchOnly leaves build orders alone.
func (m *Mod) corpWorkArmFor(ctx context.Context, b *botRun, researchOnly bool) (bool, error) {
	if b.CorpID == 0 {
		return false, nil
	}
	if !m.corpHasWorkDesk(ctx, b.CorpID) {
		return false, nil
	}
	// The deputy's refinery chore costs no travel when it is already there.
	m.corpRefineChore(ctx, b)
	// A purchase order first: that money is already in this pilot's wallet.
	if po, _ := m.openPOFor(ctx, b.CharID); po != nil && po.CorpID == b.CorpID {
		return m.corpPOArm(ctx, b, po)
	}
	if !m.isCorpBuilder(ctx, b.CharID) || m.benchedBuilders(ctx, b.CorpID)[b.CharID] {
		return false, nil
	}
	// The member's own base: research and copies at an R&D base, builds at
	// the factory (corpbases.go). A member works where it is posted.
	stationID, officeID := m.corpWorkStation(ctx, b)
	if stationID == 0 {
		return false, nil
	}
	// At the counter, list what the CEO put up for sale from this base (corpprintsale.go): the factory or
	// an R&D or Industry base.
	m.corpPrintSaleChore(ctx, b, stationID, officeID)
	sysID, _ := evedb.GetSolarSystemForStation(ctx, m.db, stationID)
	if b.StationID != stationID {
		if j := m.jumpsTo(ctx, b, sysID); j < 0 || j > corpWorkMaxJumps {
			return false, nil
		}
	}
	// Nothing takeable is the ordinary case and says nothing worth logging on
	// every plan: the pilot simply goes on to its own book.
	choice, _ := m.corpWorkFor(ctx, b, stationID, officeID, 0, researchOnly)
	if choice == nil {
		return false, nil
	}
	got, err := m.claimWorkRuns(ctx, choice.Work, b.CharID, choice.Runs)
	if err != nil || got <= 0 {
		return false, err
	}
	m.chatterCorpClaim(ctx, choice.Work, b.CharID, got)
	st := b.State
	st.Data = idleData(st.Data)
	d := &st.Data
	d.IndCorpWork = choice.Work.WorkID
	d.IndTypeID = choice.Work.ProductTypeID
	if d.IndTypeID == 0 {
		d.IndTypeID = choice.Work.BPTypeID
	}
	d.IndTypeName = m.typeNameOr(ctx, d.IndTypeID)
	d.IndBPTypeID = choice.Work.BPTypeID
	d.IndBPItemID = choice.Print.ItemID
	d.IndFactoryID, d.IndFactorySysID = stationID, sysID
	d.IndRuns = got
	d.IndStartedAt = clock.Unix()
	m.logBot(b.CharID, logInfo, "industry",
		"taking corporation work: %d %s of %s at %s for %s each",
		got, map[string]string{workKindBuild: "run(s)", workKindCopy: "copies", workKindME: "research level(s)",
			workKindPE: "research level(s)"}[choice.Work.Kind],
		d.IndTypeName, m.stationLabel(ctx, stationID), iskString(choice.Work.WagePerRun))
	return true, m.beginTravel(ctx, b, sysID, stationID, string(TaskIndCorpJob))
}

// corpJobStep is the TaskIndCorpJob phase: at the factory, install.
func (m *Mod) corpJobStep(ctx context.Context, b *botRun) error {
	d := &b.State.Data
	workID := d.IndCorpWork
	finish := func(note string) error {
		m.releaseWorkClaim(ctx, workID, b.CharID)
		m.logBot(b.CharID, logInfo, "industry", "%s", note)
		m.industryClearProject(ctx, b, false)
		b.State.Activity, b.State.Phase = "", string(TaskIdle)
		b.State.Data = idleData(b.State.Data)
		return saveState(ctx, m.db, b.State)
	}
	if workID == 0 {
		return finish("its corporation job lost its work order")
	}
	// The base the work was taken at (corpWorkArmFor stamped it).
	stationID := d.IndFactoryID
	var officeID int64
	if stationID != 0 {
		officeID, _, _ = evedb.GetCorpOfficeAt(ctx, m.db, b.CorpID, stationID)
	}
	if stationID == 0 || officeID == 0 || officeID == stationID || b.StationID != stationID {
		return finish("its corporation no longer has an office where it docked")
	}
	ws, err := m.queryWork(ctx, `WHERE workID = ? AND status = 'open'`, workID)
	if err != nil || len(ws) == 0 {
		return finish("the work order it came for is closed")
	}
	w := ws[0]
	// Re-check the feed at the counter: another builder may have eaten the
	// materials while this one was flying.
	choice, why := m.corpWorkFor(ctx, b, stationID, officeID, workID, false)
	runs := d.IndRuns
	bpItem := d.IndBPItemID
	if choice != nil && choice.Work.WorkID == workID {
		runs = min(runs, choice.Runs)
		bpItem = choice.Print.ItemID
	} else {
		// Nothing it can install (print busy, materials eaten): the order waits instead of a doomed install.
		holdWork(workID)
		return finish("arrived to find the corporation's order unfeedable — " + why)
	}
	jobID, fees, err := m.installCorpJob(ctx, b, corpJobSpec{
		CorpID: b.CorpID, OfficeID: officeID, StationID: stationID,
		BPItemID: bpItem, BPTypeID: w.BPTypeID, ActivityID: int64(workActivity(w.Kind)), Runs: runs,
		OutFlag: workInstallFlag(w, stationID),
	})
	if err != nil {
		// A refusal at the counter (busy slot, missing material, the corporation's wallet) ends this piece of
		// work, not the pilot. Only a dead context is passed up.
		if ctx.Err() != nil {
			return err
		}
		holdWork(workID)
		m.noteQueuedOut(ctx, b.CorpID, b.CharID, err)
		return finish("the corporation job would not install: " + err.Error())
	}
	m.bindWorkClaim(ctx, workID, b.CharID, jobID)
	m.chatterCorpJob(ctx, b.CharID, w.Kind, runs, d.IndTypeName, m.typeNameOr(ctx, w.BPTypeID), jobID)
	return finish(fmt.Sprintf("installed a corporation job: %d run(s) of %s on the corporation's print (fee %s, paid by the corporation)",
		runs, d.IndTypeName, iskString(fees)))
}

// jumpsTo is the gate distance from the pilot to a system, -1 for no route.
func (m *Mod) jumpsTo(ctx context.Context, b *botRun, sysID int64) int {
	if b.SystemID == sysID {
		return 0
	}
	j, err := m.jumpsBetween(ctx, b, b.SystemID, sysID)
	if err != nil {
		return -1
	}
	return j
}

// ── purchase orders, the buying side ─────────────────────────────────────────

// corpPOArm turns a purchase order into a blueprint-buying project whose
// factory is the corporation's.
func (m *Mod) corpPOArm(ctx context.Context, b *botRun, po *corpPO) (bool, error) {
	stationID, _ := m.corpFactoryOffice(ctx, b.CorpID)
	if stationID == 0 {
		m.voidPO(ctx, po, "the corporation has no factory to deliver to")
		return false, nil
	}
	sysID, _ := evedb.GetSolarSystemForStation(ctx, m.db, stationID)
	// An order that may take a copy (a shipyard's hull order) looks at escrow first: a researcher's copy
	// is how a poor corporation gets a hull into production (postLibraryPurchase).
	if po.AllowCopy {
		if took, err := m.corpPOEscrowArm(ctx, b, po, stationID, sysID); took || err != nil {
			return took, err
		}
	}
	var stop *marketStop
	if _, err := m.shopLadder(ctx, b, []int64{po.BPTypeID}, true, m.homeRegionSet(ctx, b),
		industryBPOBand, "", func(scan marketScan) bool {
			stop = bestBuy(scan.Stops, b.Tune)
			return stop != nil
		}); err != nil {
		return true, err
	}
	if stop == nil {
		m.voidPO(ctx, po, "no seller")
		return false, nil
	}
	// Funded at the counter's price as the pilot commits to the trip (corplibrary.go fundPO). An order
	// funded earlier keeps its advance but must still cover the ask.
	if po.Advance == 0 {
		if err := m.fundPO(ctx, po, stop.Price); err != nil {
			m.voidPO(ctx, po, err.Error())
			return false, nil
		}
	}
	if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err != nil || bal < stop.Price {
		m.voidPO(ctx, po, "the buyer no longer holds enough of the advance to pay for it")
		return false, nil
	}
	return m.beginPOTrip(ctx, b, po, stationID, sysID, stop.StationID, stop.SystemID, 0, stop.Price)
}

// corpPOEscrowArm sends the buyer to the cheapest escrow listing of the order's print within
// corpWorkMaxJumps of the factory that the budget covers (pickPOEscrow). It reports false, with nothing
// changed, when there is none, so the caller falls through to the market. The claim happens on arrival
// (industry.go, TaskIndBuyBPO) and corpPODeliver shelves what came back.
func (m *Mod) corpPOEscrowArm(ctx context.Context, b *botRun, po *corpPO, stationID, sysID int64) (bool, error) {
	if !escrowBuyingEnabled(ctx, m.db) {
		return false, nil
	}
	budget := po.Advance
	if budget <= 0 {
		_ = m.db.QueryRowContext(ctx,
			`SELECT amount FROM rolebot_corp_commit WHERE refKind = 'po' AND refID = ?`, po.POID).Scan(&budget)
	}
	if budget <= 0 {
		return false, nil
	}
	reach, err := m.systemsWithin(ctx, sysID, rulesForBot(b), corpWorkMaxJumps)
	if err != nil || len(reach) == 0 {
		return false, nil
	}
	listings, err := m.escrowBPOListings(ctx)
	if err != nil {
		return false, nil
	}
	pick := pickPOEscrow(listings, po.BPTypeID, reach, budget, b.CharID)
	if pick == nil {
		return false, nil
	}
	if po.Advance == 0 {
		if err := m.fundPO(ctx, po, pick.Price); err != nil {
			return false, nil
		}
	}
	if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err != nil || bal < pick.Price {
		return false, nil
	}
	return m.beginPOTrip(ctx, b, po, stationID, sysID, pick.StationID, pick.SystemID, pick.EscrowID, pick.Price)
}

// pickPOEscrow is the escrow listing a purchase order buys: the print's type, within reach of the
// factory, not the buyer's own, a copy only with runs left, at a price the budget covers. It is the
// cheapest, an original winning a tie. nil when there is none. Pure.
func pickPOEscrow(listings []escrowBPO, bpTypeID int64, reach map[int64]int, budget float64, buyer int64) *escrowBPO {
	var best *escrowBPO
	for i := range listings {
		l := listings[i]
		if l.TypeID != bpTypeID || l.IssuerID == buyer || l.Price <= 0 || l.Price > budget {
			continue
		}
		if l.Copy && l.Runs <= 0 {
			continue
		}
		if _, ok := reach[l.SystemID]; !ok {
			continue
		}
		if best == nil || l.Price < best.Price || (l.Price == best.Price && best.Copy && !l.Copy) {
			pick := l
			best = &pick
		}
	}
	return best
}

// beginPOTrip turns a funded purchase order into the buying trip: to the
// seller's station, on the corporation's money, the print bound for the
// factory. escrowID is the listing to claim on arrival, 0 for a market buy.
func (m *Mod) beginPOTrip(ctx context.Context, b *botRun, po *corpPO, stationID, sysID,
	sellerStationID, sellerSysID, escrowID int64, price float64) (bool, error) {
	var product int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT productTypeID FROM invBlueprintTypes WHERE blueprintTypeID = ?`, po.BPTypeID).Scan(&product)
	st := b.State
	// Only now: a voided order above must leave the pilot's activity alone,
	// or a pilot with nothing to do carries ActIndustry into planSolo.
	st.Activity = ActIndustry
	st.Data = idleData(st.Data)
	d := &st.Data
	d.IndCorpPO = po.POID
	d.IndTypeID = product
	if d.IndTypeID == 0 {
		d.IndTypeID = po.BPTypeID
	}
	d.IndTypeName = m.typeNameOr(ctx, po.BPTypeID)
	d.IndBPTypeID = po.BPTypeID
	d.IndFactoryID, d.IndFactorySysID = stationID, sysID
	d.IndSellerStationID, d.IndSellerSysID = sellerStationID, sellerSysID
	d.IndEscrowID = escrowID
	d.IndStartedAt = clock.Unix()
	where := "market"
	if escrowID != 0 {
		where = "escrow"
	}
	m.logBot(b.CharID, logInfo, "industry",
		"buying a %s for its corporation's library at %s (%s, %s), on the corporation's money",
		d.IndTypeName, m.stationLabel(ctx, sellerStationID), where, iskString(price))
	return true, m.beginTravel(ctx, b, sellerSysID, sellerStationID, string(TaskIndBuyBPO))
}

// corpPODeliver is the refine hub's first check for a purchase-order
// project: at the factory, shelve the print and settle.
func (m *Mod) corpPODeliver(ctx context.Context, b *botRun) error {
	d := &b.State.Data
	po, _ := m.openPOFor(ctx, b.CharID)
	done := func(note string) error {
		m.logBot(b.CharID, logInfo, "industry", "%s", note)
		m.industryClearProject(ctx, b, false)
		b.State.Activity, b.State.Phase = "", string(TaskIdle)
		b.State.Data = idleData(b.State.Data)
		return saveState(ctx, m.db, b.State)
	}
	if po == nil || po.POID != d.IndCorpPO {
		return done("its corporation's purchase order was closed while it was away")
	}
	stationID, officeID := m.corpFactoryOffice(ctx, b.CorpID)
	if stationID == 0 || b.StationID != stationID || d.IndBPItemID == 0 {
		return done("could not reach its corporation's factory with the print")
	}
	if err := m.fileIntoLibrary(ctx, b.CorpID, officeID, stationID, b.CharID, d.IndBPItemID,
		"purchase", d.IndBPOCost); err != nil {
		return done("could not shelve the print: " + err.Error())
	}
	m.settlePO(ctx, po, d.IndBPItemID, d.IndBPOCost)
	return done(fmt.Sprintf("delivered the %s to its corporation's library (%s of the %s advanced)",
		d.IndTypeName, iskString(d.IndBPOCost), iskString(po.Advance)))
}

// ── delivery ─────────────────────────────────────────────────────────────────

// deliverReadyCorpJobs hands back every corporation job a BOT installed that
// has finished, the way industryOvenSweep does a bot's own jobs, and any job
// a human left undelivered past corpJobDeliverGrace. The output lands in the
// office either way; delivery is what lets the wage sweep pay for it.
func (m *Mod) deliverReadyCorpJobs(ctx context.Context, corps []int64) {
	if len(corps) == 0 {
		return
	}
	now := win32Now()
	grace := int64(corpJobDeliverGrace) * 10_000_000
	// One read for every managed corporation, not one per corporation per
	// minute: ninety corporations was ninety ramJobs scans a minute.
	where, args := inClause(corps)
	rows, err := m.db.QueryContext(ctx, `
			SELECT j.jobID, j.installerID, (b.characterID IS NOT NULL)
			  FROM ramJobs j
			  LEFT JOIN rolebot_characters b ON b.characterID = j.installerID
			 WHERE j.ownerID IN `+where+` AND j.completedStatusID IN (0, 1) AND j.endProductionTime > 0
			   AND j.endProductionTime <= ?
			 LIMIT ?`, append(args, now, corpJobDeliverPerRun*len(corps))...)
	if err != nil {
		return
	}
	{
		type job struct {
			id, installer int64
			bot           bool
		}
		var jobs []job
		for rows.Next() {
			var j job
			if rows.Scan(&j.id, &j.installer, &j.bot) == nil {
				jobs = append(jobs, j)
			}
		}
		rows.Close()
		for _, j := range jobs {
			if !j.bot {
				var end int64
				_ = m.db.QueryRowContext(ctx,
					`SELECT endProductionTime FROM ramJobs WHERE jobID = ?`, j.id).Scan(&end)
				if now-end < grace {
					continue
				}
			}
			if _, err := evedb.CompleteJob(ctx, m.db, j.id, j.installer, false); err != nil {
				rlogf("rolebots: deliver corp job %d: %v", j.id, err)
			}
		}
	}
}

// wageForRun is the charter's wage for one run of a product.
func wageForRun(productValuePerRun, wageFrac float64) float64 {
	return math.Max(1000, math.Floor(productValuePerRun*wageFrac))
}

// corpPODue is the planner's pre-emption for a purchase order: a pilot with
// no project of its own in hand goes and buys the corporation's print before
// anything else.
func (m *Mod) corpPODue(ctx context.Context, b *botRun) (bool, error) {
	if b == nil || b.CorpID == 0 || b.State == nil || b.State.Data.IndTypeID != 0 {
		return false, nil
	}
	if !m.corpHasWorkDesk(ctx, b.CorpID) {
		return false, nil
	}
	po, _ := m.openPOFor(ctx, b.CharID)
	if po == nil || po.CorpID != b.CorpID {
		return false, nil
	}
	return m.corpPOArm(ctx, b, po)
}
