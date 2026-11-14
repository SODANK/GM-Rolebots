package rolebots

// The lab errand itself: the gate that opens it, the three-phase trip that runs it, and the pre-empt that
// fetches the print when the job rings. labwork.go holds the pure arithmetic; this file holds the DB writes
// and the state machine, as industry.go and industry_ram.go do.

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// labworkGate answers whether this pilot may put a new job in a lab now. It is about money and capacity;
// labPickPrint decides what to research and labFetchStray what is stranded. Research runs before the
// manufacturing pick: the slot pools are separate and a research job takes no materials. A pilot short of
// money stays out, because the fee is charged after the flight (labprints.go) and labWalletFloorISK sets the floor.
func (m *Mod) labworkGate(ctx context.Context, b *botRun) (bool, string) {
	d := b.State.Data
	if now := clock.Unix(); d.LabFailAt > now {
		return false, "nothing worth researching last time it looked"
	}
	// A lab errand the pilot is still flying owns the trip fields, so no second one starts; a job already
	// installed and left on a line does not (LabCooking). labFreeSlots, asked below from ramJobs, says if a slot is free.
	if d.LabJobID != 0 {
		return false, "its research job is still cooking"
	}
	// The wallet floor is checked before the pick and the station search, not after the flight. No cooldown
	// rides with this refusal: a pilot under the floor is one payday from being over it.
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return false, "wallet unreadable"
	}
	if bal < labWalletFloorISK {
		return false, "research comes out of surplus and it has none"
	}
	return true, "before it picks its next build"
}

// labworkStart is the whole research routine for one idle pilot: bring any stray blueprint home, then decide
// on a new science venture. planSolo runs it after the home loop and stray sweeps and before the appetite draw,
// when the pilot is home and has shopped. Reports handled=true only when it routed the pilot somewhere.
func (m *Mod) labworkStart(ctx context.Context, b *botRun) (bool, error) {
	return m.labwork(ctx, b, false)
}

// labworkTrip is the research sub-trip of the industry umbrella (industrytrip.go): the same routine with the
// new-job half switched on. The umbrella has already chosen research, so the career test below is a backstop.
func (m *Mod) labworkTrip(ctx context.Context, b *botRun) (bool, error) {
	return m.labwork(ctx, b, true)
}

// labwork is one pass of the research routine. startNew=false is the
// housekeeping every pilot gets before the appetite draw (bring a stray print
// home); startNew=true is the research trip, which may also put a NEW job in a
// lab.
func (m *Mod) labwork(ctx context.Context, b *botRun, startNew bool) (bool, error) {
	if b.State == nil || b.Profile == nil || b.StationID == 0 {
		return false, nil
	}
	d := &b.State.Data
	// An errand already in flight owns every Lab field on the blob. Nothing
	// below may touch them, and the step machine is what advances it.
	if d.LabJobID != 0 || d.LabBPItemID != 0 {
		return false, nil
	}
	// Both throttles are tested before the factory resolve (three or four queries), with no query between them:
	// this runs for every solo pilot on every planning pass, so steady state is two integer comparisons.
	now := clock.Unix()
	strayDue := now-d.LabCheckedAt >= labStrayCheckEverySec
	if !strayDue && d.LabFailAt > now {
		return false, nil
	}
	factoryID, factorySys, err := m.industryFactoryStation(ctx, b)
	if err != nil || factoryID == 0 {
		return false, nil
	}

	// Housekeeping first and ahead of the gate: a print in the wrong hangar is capital nothing in the module can
	// see (labprints.go), and fetching it costs no fee, so neither the wallet floor nor the cooldown applies.
	if handled, ferr := m.labFetchStray(ctx, b, factoryID, factorySys); ferr != nil {
		m.logBot(b.CharID, logError, "lab", "looking for its stray blueprints: %v", ferr)
	} else if handled {
		return true, nil
	}

	// A research career's product is its copies and the odd spare original: sell whatever it is docked with
	// (listLibraryProducts). A research house's member hands its copies to the library instead
	// (corpresearchdeposit.go). Throttled with the stray scan.
	if strayDue {
		if !m.depositResearchProducts(ctx, b) && researchCareer(b.Tune) {
			m.listLibraryProducts(ctx, b)
		}
	}

	// Everything above is housekeeping for anyone holding a print. Starting a speculative job is a calling, gated by
	// researchOpen. Corporation research orders do not come here: a builder takes them inside industryPlan (corpResearchArm).
	if !startNew || !researchOpen(b) {
		return false, nil
	}

	ok, why := m.labworkGate(ctx, b)
	if !ok {
		return false, nil
	}
	free, err := m.labFreeSlots(ctx, b)
	if err != nil || free == 0 {
		// No lab capacity is no failure worth a cooldown: it is the ordinary state of a pilot whose slot is in use.
		return false, nil
	}
	// The prints live in the factory hangar, often not where the pilot stands (homeStation and IndustryStationID
	// agree only sometimes, corphome.go). So the pick is factory-scoped and the trip collects the print on the way.
	cand, err := m.labPickPrint(ctx, b, factoryID)
	if err != nil {
		return false, err
	}
	if cand == nil {
		// A research career that has run through its stock buys more (labbuy.go): its living is the researched
		// original, and every sale spends a print. Anyone else holds off.
		bought, berr := m.researcherBuyPrint(ctx, b)
		if berr != nil {
			m.logBot(b.CharID, logError, "lab", "shopping for a blueprint to research: %v", berr)
		}
		if bought {
			if b.StationID == factoryID {
				// Bought at the factory itself: it can go straight in.
				cand, err = m.labPickPrint(ctx, b, factoryID)
				if err != nil {
					return false, err
				}
			} else {
				// Bought elsewhere: it is a stray now, and the housekeeping that
				// runs ahead of every draw carries it home on the next pass.
				d.LabCheckedAt = 0
				_ = saveState(ctx, m.db, b.State)
				return false, nil
			}
		}
		if cand == nil {
			// Nothing on this counter either: go to one that has something.
			if routed, serr := m.researcherShopTrip(ctx, b); serr != nil {
				m.logBot(b.CharID, logError, "lab", "looking for a blueprint counter: %v", serr)
			} else if routed {
				return true, nil
			}
			m.labHoldOff(ctx, b, "it owns no blueprint that can be researched")
			return false, nil
		}
	}

	labID, labSys, line, err := m.labStationFor(ctx, b, factoryID, factorySys, cand.ActivityID)
	if err != nil || labID == 0 {
		m.labHoldOff(ctx, b, fmt.Sprintf("no lab it can use for the %s: %v", cand.Name, err))
		return false, nil
	}

	// Price the job against the line it will sit on before the pilot leaves: installResearchJob charges at the
	// counter, and a refusal there comes after the flight and often after a shuttle was bought.
	fee, durationSec := m.labFeeQuote(ctx, b, line, cand)
	bal, berr := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if berr != nil {
		return false, nil
	}
	// A trip is needed when the lab is not the factory, and also when the
	// pilot is not standing in the factory it has to collect the print from.
	needsTrip := labID != factoryID || b.StationID != factoryID
	if !labAffordable(bal, fee, needsTrip) {
		m.labHoldOff(ctx, b, fmt.Sprintf(
			"%s on the %s would cost about %s and it holds %s — research is spent out of surplus",
			activityLineNoun(cand.ActivityID), cand.Name, iskString(fee), iskString(bal)))
		return false, nil
	}

	d.LabBPItemID, d.LabBPTypeID, d.LabBPName = cand.ItemID, cand.TypeID, cand.Name
	d.LabActivityID = cand.ActivityID
	d.LabCopies, d.LabCopyRuns = cand.Copies, cand.CopyRuns
	d.LabLevels = cand.Levels
	d.LabStationID, d.LabStationSysID = labID, labSys
	d.LabFactoryID, d.LabFactorySysID = factoryID, factorySys
	b.State.Activity = ActIndustry

	if cand.ActivityID == ramActivityCopying {
		m.logBot(b.CharID, logInfo, "lab",
			"%s — putting the %s in for copying at %s (%d copies of %d run(s) each, about %s, about %s in fees)",
			why, cand.Name, m.stationLabel(ctx, labID), cand.Copies, cand.CopyRuns,
			fmtDuration(int64(durationSec)), iskString(fee))
	} else {
		m.logBot(b.CharID, logInfo, "lab",
			"%s — putting the %s in for %s at %s (%d level(s), %d to %d, about %s, about %s in fees)",
			why, cand.Name, activityLineNoun(cand.ActivityID), m.stationLabel(ctx, labID),
			max(cand.Levels, 1), cand.Level, cand.Level+int64(max(cand.Levels, 1)),
			fmtDuration(int64(durationSec)), iskString(fee))
	}

	// Not at the factory: go and get the print first. The fetch leg does the rest and is also the resume path
	// when the pilot is interrupted or the process restarts mid-trip.
	if b.StationID != factoryID {
		// beginTravel saves the state blob itself, so the fields set above ride out with the route.
		return true, m.beginTravel(ctx, b, factorySys, factoryID, string(TaskLabFetch))
	}
	return true, m.labDepartForLab(ctx, b)
}

// labFetchStray opens a repatriation trip for one blueprint in a hangar that is not this pilot's factory, and
// reports whether it did. It reuses the lab errand's collect and haul legs (a trip with LabJobID zero and a print
// away from the factory); LabActivityID stays zero, which tells the panel this is a fetch. The throttle is
// LabCheckedAt: zero is due, so each pilot is swept once after a restart, then every half hour.
func (m *Mod) labFetchStray(ctx context.Context, b *botRun, factoryID, factorySys int64) (bool, error) {
	d := &b.State.Data
	now := clock.Unix()
	if now-d.LabCheckedAt < labStrayCheckEverySec {
		return false, nil
	}
	stray, err := m.labStrayPrint(ctx, b, factoryID, factorySys)
	d.LabCheckedAt = now
	if err != nil {
		// The stamp is still written: a scan that cannot run is not worth
		// re-running on every pass either.
		return false, saveState(ctx, m.db, b.State)
	}
	if stray == nil {
		return false, saveState(ctx, m.db, b.State)
	}

	d.LabBPItemID, d.LabBPTypeID, d.LabBPName = stray.ItemID, stray.TypeID, stray.Name
	d.LabActivityID = 0
	d.LabJobID, d.LabEndsAt = 0, 0
	d.LabStationID, d.LabStationSysID = stray.StationID, stray.SysID
	d.LabFactoryID, d.LabFactorySysID = factoryID, factorySys
	b.State.Activity = ActIndustry

	m.logBot(b.CharID, logInfo, "lab",
		"its %s is sitting at %s, %d jump(s) from its factory — going to bring it home before it decides anything else",
		stray.Name, m.stationLabel(ctx, stray.StationID), stray.Jumps)
	return true, m.beginTravel(ctx, b, stray.SysID, stray.StationID, string(TaskLabCollect))
}

// labDepartForLab is the step from standing in the factory with a print chosen to being on the way to the lab,
// or straight to the install when the factory is the lab (about half the roster).
func (m *Mod) labDepartForLab(ctx context.Context, b *botRun) error {
	d := &b.State.Data
	b.State.Activity = ActIndustry
	if d.LabStationID == d.LabFactoryID {
		b.State.Phase = string(TaskLabInstall)
		return saveState(ctx, m.db, b.State)
	}
	// The print has to travel, so it needs a hull: a blueprint is 1 m3 and a shuttle is 5,000 ISK, and a pilot in
	// a capsule would load it into a hull the next death deletes.
	if _, ok := m.ensureRunabout(ctx, b, "carrying the "+d.LabBPName+" to a lab"); !ok {
		m.labHoldOff(ctx, b, "nothing to carry the "+d.LabBPName+" to a lab in")
		clearLabTrip(d)
		b.State.Activity, b.State.Phase = "", string(TaskIdle)
		b.State.Data = idleData(b.State.Data)
		return saveState(ctx, m.db, b.State)
	}
	if err := m.moveItemToCargo(ctx, b, d.LabBPItemID); err != nil {
		m.labHoldOff(ctx, b, fmt.Sprintf("loading the %s: %v", d.LabBPName, err))
		clearLabTrip(d)
		b.State.Activity, b.State.Phase = "", string(TaskIdle)
		b.State.Data = idleData(b.State.Data)
		return saveState(ctx, m.db, b.State)
	}
	return m.beginTravel(ctx, b, d.LabStationSysID, d.LabStationID, string(TaskLabInstall))
}

// labHoldOff records that the lane found nothing and stands it down for labFailCooldownSec (see IndBookFailAt).
func (m *Mod) labHoldOff(ctx context.Context, b *botRun, why string) {
	b.State.Data.LabFailAt = clock.Unix() + labFailCooldownSec
	m.logBot(b.CharID, logInfo, "lab", "no research this time: %s", why)
	_ = saveState(ctx, m.db, b.State)
}

// labworkStep advances the install / collect / haul errand.
func (m *Mod) labworkStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data

	switch TaskKind(st.Phase) {

	case TaskLabFetch:
		if d.LabBPItemID == 0 || d.LabFactoryID == 0 {
			return false, m.finishLabTrip(ctx, b, "")
		}
		if b.StationID != d.LabFactoryID {
			return false, m.beginTravel(ctx, b, d.LabFactorySysID, d.LabFactoryID, string(TaskLabFetch))
		}
		return false, m.labDepartForLab(ctx, b)

	case TaskLabInstall:
		if d.LabBPItemID == 0 {
			return false, m.finishLabTrip(ctx, b, "")
		}
		if b.StationID != d.LabStationID {
			// A restart mid-phase, or the pilot was moved.
			return false, m.beginTravel(ctx, b, d.LabStationSysID, d.LabStationID, string(TaskLabInstall))
		}
		// The print must be in the station hangar first: CompleteJob returns a blueprint to flag 4 without moving it, so
		// one installed from the cargo hold would be delivered into the ship. A no-op when the lab is the factory.
		if err := m.moveCargoItemToHangar(ctx, b, d.LabBPItemID); err != nil {
			if !m.labPrintInHangarHere(ctx, b, d.LabBPItemID) {
				m.logBot(b.CharID, logError, "lab", "unloading the %s at the lab: %v", d.LabBPName, err)
				return false, m.finishLabTrip(ctx, b, "")
			}
		}
		var jobID, installedBP, endTime int64
		var fees float64
		var err error
		if d.LabActivityID == ramActivityCopying {
			jobID, installedBP, fees, endTime, err = m.installCopyJob(ctx, b, d.LabBPItemID, d.LabBPTypeID, d.LabCopies, d.LabCopyRuns)
		} else {
			jobID, installedBP, fees, endTime, err = m.installResearchJob(ctx, b, d.LabBPItemID, d.LabBPTypeID, d.LabActivityID, d.LabLevels)
		}
		if err != nil {
			// A cooldown rides with this refusal. It is nearly always the fee (quoted before the trip, but a wallet can
			// drain in flight); without a hold-off the next pass picks the same print and is refused again.
			m.labHoldOff(ctx, b, fmt.Sprintf("could not install %s on the %s: %v",
				activityLineNoun(d.LabActivityID), d.LabBPName, err))
			// The print is in a hangar it may not belong in: hand the trip to the haul leg so it is carried home.
			d.LabBPItemID = installedBP
			return false, m.labHaulHomeOrFinish(ctx, b)
		}
		// The job goes onto the cooking list and the trip ends: LabJobID is the trip's field and would hold all research
		// capacity while the job cooks. EnsureBlueprintSingleton may split a stack, so the recorded item is the one on the line.
		d.LabCooking = append(d.LabCooking, LabCookingJob{
			JobID:        jobID,
			BPItemID:     installedBP,
			BPTypeID:     d.LabBPTypeID,
			BPName:       d.LabBPName,
			ActivityID:   d.LabActivityID,
			StationID:    d.LabStationID,
			StationSysID: d.LabStationSysID,
			FactoryID:    d.LabFactoryID,
			FactorySysID: d.LabFactorySysID,
			EndsAt:       endTime,
			StartedAt:    clock.Unix(),
			Copies:       d.LabCopies,
			CopyRuns:     d.LabCopyRuns,
		})
		m.logBot(b.CharID, logInfo, "lab",
			"installed %s on the %s (job %d, %.0f ISK) — labOvenSweep takes it off the line when it rings",
			activityLineNoun(d.LabActivityID), d.LabBPName, jobID, fees)
		m.chatterNoteBot(ctx, b.CharID, "lab.install", map[string]string{
			"what": d.LabBPName, "activity": activityLineNoun(d.LabActivityID)}, jobID)
		// The pilot is free now: a research job cooks for hours with nobody at the counter, and the next planning pass
		// may open another against the Laboratory Operation slots left. idleData carries LabCooking.
		clearLabTrip(d)
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return false, saveState(ctx, m.db, st)

	case TaskLabCollect:
		if d.LabJobID == 0 {
			// No job, but possibly a print still at the lab (the state labHaulHomeOrFinish leaves when it finds no hull, which
			// labCollectDue re-opens). Finishing here would clear LabBPItemID and abandon the blueprint.
			if d.LabBPItemID != 0 {
				if b.StationID != d.LabStationID {
					return false, m.beginTravel(ctx, b, d.LabStationSysID, d.LabStationID, string(TaskLabCollect))
				}
				return false, m.labHaulHomeOrFinish(ctx, b)
			}
			return false, m.finishLabTrip(ctx, b, "")
		}
		if b.StationID != d.LabStationID {
			return false, m.beginTravel(ctx, b, d.LabStationSysID, d.LabStationID, string(TaskLabCollect))
		}
		status, _, _, serr := m.industryJobStatus(ctx, d.LabJobID)
		switch {
		case serr != nil:
			// The row is gone, cancelled or swept from outside. The print is back at flag 4 either way; go and get it.
			m.logBot(b.CharID, logInfo, "lab", "its research job %d is gone: %v", d.LabJobID, serr)
			d.LabJobID = 0
			return false, m.labHaulHomeOrFinish(ctx, b)
		case status >= 100:
			// Already delivered, by the ESP panel or a previous pass.
			d.LabJobID = 0
		default:
			if _, err := evedb.CompleteJob(ctx, m.db, d.LabJobID, b.CharID, false); err != nil {
				m.logBot(b.CharID, logInfo, "lab", "collecting the %s: %v", d.LabBPName, err)
				return false, m.finishLabTrip(ctx, b, "")
			}
			m.logBot(b.CharID, logInfo, "lab", "collected the %s — %s done",
				d.LabBPName, activityLineNoun(d.LabActivityID))
			d.LabJobID = 0
		}
		return false, m.labHaulHomeOrFinish(ctx, b)

	case TaskLabHaul:
		if d.LabBPItemID == 0 || d.LabFactoryID == 0 {
			return false, m.finishLabTrip(ctx, b, "")
		}
		if b.StationID != d.LabFactoryID {
			return false, m.beginTravel(ctx, b, d.LabFactorySysID, d.LabFactoryID, string(TaskLabHaul))
		}
		if err := m.moveCargoItemToHangar(ctx, b, d.LabBPItemID); err != nil {
			// Not in the hold is usually no fault: the oven sweep (labwork_oven.go) moves prints without touching this trip's
			// fields, so a resumed haul can name a print already in this hangar or back in a job (flag 111). The install leg has the same guard.
			if !m.labPrintInHangarHere(ctx, b, d.LabBPItemID) {
				m.logBot(b.CharID, logInfo, "lab",
					"the %s is no longer in its hold — something else already put it away; closing the errand",
					d.LabBPName)
				return false, m.finishLabTrip(ctx, b, "")
			}
		}
		return false, m.finishLabTrip(ctx, b,
			fmt.Sprintf("stowed the %s back at its factory", d.LabBPName))
	}
	return false, m.finishLabTrip(ctx, b, "")
}

// labHaulHomeOrFinish takes the print home when the lab is not the factory,
// and stops where it is when it is.
func (m *Mod) labHaulHomeOrFinish(ctx context.Context, b *botRun) error {
	d := &b.State.Data
	if d.LabFactoryID == 0 || d.LabFactoryID == b.StationID {
		return m.finishLabTrip(ctx, b, "")
	}
	if _, ok := m.ensureRunabout(ctx, b, "carrying the "+d.LabBPName+" home"); !ok {
		// The print is safe where it is, and the trip is dropped, not held: holding it loops (labCollectDue re-opens off
		// LabBPItemID). labStrayPrint finds the print again, so the stray scan re-opens the errand once the pilot has a hull.
		m.logBot(b.CharID, logInfo, "lab",
			"leaving the %s where it is until it has a ship to carry it in", d.LabBPName)
		return m.finishLabTrip(ctx, b, "")
	}
	if err := m.moveItemToCargo(ctx, b, d.LabBPItemID); err != nil {
		m.logBot(b.CharID, logError, "lab", "loading the %s for the trip home: %v", d.LabBPName, err)
		return m.finishLabTrip(ctx, b, "")
	}
	b.State.Activity = ActIndustry
	return m.beginTravel(ctx, b, d.LabFactorySysID, d.LabFactoryID, string(TaskLabHaul))
}

// labPrintInHangarHere reports whether the print is already in this station's hangar, as it is whenever the lab
// is the factory and the blueprint never went into a hold.
func (m *Mod) labPrintInHangarHere(ctx context.Context, b *botRun, itemID int64) bool {
	var one int64
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM entity
		 WHERE itemID = ? AND ownerID = ? AND locationID = ? AND flag = 4`,
		itemID, b.CharID, b.StationID).Scan(&one)
	return err == nil
}

// labCollectDue is the pre-empt: a print left at a lab takes the pilot over. labOvenSweep delivers a finished
// job from wherever the pilot is, so this covers what a remote delivery cannot: a blueprint in a lab's hangar that
// the trip fields still name. The LabJobID arm is a backstop for a failed save after labAdoptTripJob. Free on the
// common pass: two integer comparisons.
func (m *Mod) labCollectDue(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if st == nil {
		return false, nil
	}
	d := &st.Data

	// A print stranded at a lab with no job on it (the ensureRunabout refusal above, or a job that vanished): fetching
	// it is the errand.
	if d.LabJobID == 0 {
		if d.LabBPItemID != 0 && d.LabFactoryID != 0 && d.LabStationID != d.LabFactoryID {
			st.Activity, st.Phase = ActIndustry, string(TaskLabCollect)
			return true, saveState(ctx, m.db, st)
		}
		return false, nil
	}
	if d.LabEndsAt > win32Now() {
		return false, nil
	}
	status, ready, _, err := m.industryJobStatus(ctx, d.LabJobID)
	if err != nil {
		// The row is gone. Do not strand the print: route to the collect
		// phase, which handles a missing job by going to fetch it.
		st.Activity, st.Phase = ActIndustry, string(TaskLabCollect)
		return true, saveState(ctx, m.db, st)
	}
	if !ready && status < 100 {
		return false, nil
	}
	st.Activity, st.Phase = ActIndustry, string(TaskLabCollect)
	return true, saveState(ctx, m.db, st)
}

// finishLabTrip drops the errand and returns the pilot to the draw. The cooldown and lookup stamp, which belong
// to the pilot, survive (as in finishPatentTrip).
func (m *Mod) finishLabTrip(ctx context.Context, b *botRun, note string) error {
	st := b.State
	if note != "" {
		m.logBot(b.CharID, logInfo, "lab", "%s", note)
	}
	clearLabTrip(&st.Data)
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	return saveState(ctx, m.db, st)
}

// clearLabTrip drops the errand but keeps LabFailAt and LabCheckedAt, which
// are about the pilot rather than this trip.
func clearLabTrip(d *StateData) {
	d.LabJobID = 0
	d.LabBPItemID = 0
	d.LabBPTypeID = 0
	d.LabBPName = ""
	d.LabActivityID = 0
	d.LabStationID = 0
	d.LabStationSysID = 0
	d.LabFactoryID = 0
	d.LabFactorySysID = 0
	d.LabEndsAt = 0
	d.LabCopies = 0
	d.LabCopyRuns = 0
	d.LabLevels = 0
}

// detailLabShop is the panel's line for a researcher's shopping trip.
func detailLabShop(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.LabBPName == "" {
		return ""
	}
	return fmt.Sprintf("going to buy a %s to research", st.Data.LabBPName)
}

// detailLabwork is the panel's one-line description of a lab errand.
func detailLabwork(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	if d.LabBPName == "" {
		return ""
	}
	switch st.Phase {
	case string(TaskLabFetch):
		return fmt.Sprintf("fetching the %s from its factory", d.LabBPName)
	case string(TaskLabInstall):
		return fmt.Sprintf("putting the %s in for %s", d.LabBPName, activityLineNoun(d.LabActivityID))
	case string(TaskLabCollect):
		// A zero activity is a repatriation, not a research run: the trip labFetchStray opens over the same two legs.
		if d.LabActivityID == 0 {
			return fmt.Sprintf("fetching the %s back to its factory", d.LabBPName)
		}
		return fmt.Sprintf("collecting the researched %s", d.LabBPName)
	case string(TaskLabHaul):
		return fmt.Sprintf("hauling the %s back to its factory", d.LabBPName)
	}
	return ""
}
