package rolebots

// The research half of the server-industry glue, split from industry_ram.go
// because a research job is a second slot track on the same ramJobs protocol.
//
// It mirrors ramsvc.handleInstallJob for activities 3 and 4. Line resolution
// (assemblyLineAt), the activity constants and the duration arithmetic live in
// industry_ram.go, so both halves share every multiplier and rate knob.

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
	serverram "gurimoon/server/ram"
)

// installResearchJob installs `levels` levels (minimum one) of ME or PE research
// as one job in one transaction, like ramsvc.handleInstallJob. There is no
// material bill. The blueprint must be in the station hangar, since CompleteJob
// returns it to flag 4 without changing locationID. Keep the returned installedBP.
func (m *Mod) installResearchJob(ctx context.Context, b *botRun, bpItemID, bpTypeID, activityID int64, levels int) (jobID, installedBP int64, fees float64, endTime int64, err error) {
	if activityID != ramActivityMEResearch && activityID != ramActivityPEResearch {
		return 0, bpItemID, 0, 0, fmt.Errorf("activity %d is not a research activity", activityID)
	}
	return m.installLabJob(ctx, b, bpItemID, bpTypeID, activityID, max(levels, 1), 0)
}

// installCopyJob installs one copy job of `copies` copies, each with runsPerCopy
// runs. It follows the research recipe. The job's runs is the copy count and
// licensedProductionRuns is the runs per copy, as ramsvc stores a copy order.
func (m *Mod) installCopyJob(ctx context.Context, b *botRun, bpItemID, bpTypeID int64, copies int, runsPerCopy int64) (jobID, installedBP int64, fees float64, endTime int64, err error) {
	if copies < 1 || runsPerCopy < 1 {
		return 0, bpItemID, 0, 0, fmt.Errorf("a copy job needs at least one copy of one run (got %d x %d)", copies, runsPerCopy)
	}
	return m.installLabJob(ctx, b, bpItemID, bpTypeID, ramActivityCopying, copies, runsPerCopy)
}

// installLabJob is the shared body of a research or copy install. copies is
// the job's runs: copies to make, or research levels to add.
func (m *Mod) installLabJob(ctx context.Context, b *botRun, bpItemID, bpTypeID, activityID int64,
	copies int, runsPerCopy int64) (jobID, installedBP int64, fees float64, endTime int64, err error) {
	bpType, err := evedb.GetBlueprintTypeInfo(ctx, m.db, bpTypeID)
	if err != nil {
		return 0, bpItemID, 0, 0, err
	}
	installedBP, _, _, err = evedb.EnsureBlueprintSingleton(ctx, m.db, bpItemID)
	if err != nil {
		return 0, bpItemID, 0, 0, fmt.Errorf("blueprint singleton: %w", err)
	}
	attrs, err := evedb.GetBlueprintAttributes(ctx, m.db, installedBP)
	if err != nil {
		return 0, installedBP, 0, 0, err
	}
	// A blueprint copy cannot be researched: ramsvc refuses activities 3, 4 and 5
	// on one, so refuse early.
	if attrs.Copy == 1 {
		return 0, installedBP, 0, 0, fmt.Errorf("a blueprint copy cannot be researched")
	}

	// The output of a research job is the blueprint type, which the line's detail
	// multipliers are read against.
	line, err := m.assemblyLineAt(ctx, b, b.StationID, activityID, bpTypeID)
	if err != nil {
		return 0, installedBP, 0, 0, err
	}

	// The research slot track has its own cap (the Laboratory Operation pair,
	// counted over activityID <> 1). The manufacturing half would refuse a free lab.
	baseSkill, _ := m.charSkillLevelFor(ctx, b.CharID, serverram.SkillLaboratoryOperation)
	advSkill, _ := m.charSkillLevelFor(ctx, b.CharID, serverram.SkillAdvancedLabOperation)
	maxJobs := serverram.MaxConcurrentJobs(baseSkill, advSkill)
	if active, cerr := evedb.CountActiveJobs(ctx, m.db, b.CharID, false); cerr == nil && active >= maxJobs {
		return 0, installedBP, 0, 0, fmt.Errorf("already running its %d allowed research job(s)", maxJobs)
	}
	if reqs, rerr := evedb.GetActivitySkillReqs(ctx, m.db, bpTypeID, int(activityID)); rerr == nil {
		for _, r := range reqs {
			if have, _ := m.charSkillLevelFor(ctx, b.CharID, r.SkillID); have < r.Level {
				return 0, installedBP, 0, 0, fmt.Errorf("missing required skill %d at level %d", r.SkillID, r.Level)
			}
		}
	}

	current, skillID := attrs.MaterialLevel, int64(skillMetallurgy)
	switch activityID {
	case ramActivityPEResearch:
		current, skillID = attrs.ProductivityLevel, int64(skillResearch)
	case ramActivityCopying:
		skillID = skillScience
		// ramsvc clamps runs per copy to the blueprint maximum; clamp here too so the
		// priced duration and fee match what is charged.
		if bpType.MaxProductionLimit > 0 && runsPerCopy > bpType.MaxProductionLimit {
			runsPerCopy = bpType.MaxProductionLimit
		}
	}
	skillLevel, _ := m.charSkillLevelFor(ctx, b.CharID, skillID)
	var durationSec float64
	if activityID == ramActivityCopying {
		durationSec = copyDurationSec(bpType, copies, runsPerCopy, skillLevel, line)
	} else {
		durationSec = researchDurationSec(bpType, activityID, current, copies, skillLevel, line)
	}
	if durationSec <= 0 {
		return 0, installedBP, 0, 0, fmt.Errorf("this blueprint carries no research time for %s",
			activityLineNoun(activityID))
	}

	now := win32Now()
	beginTime := now
	if line.Info.NextFreeTime > beginTime {
		beginTime = line.Info.NextFreeTime
	}
	endTime = beginTime + int64(durationSec*10_000_000)

	standing := m.npcCorpStanding(ctx, b.CharID, line.Info.OwnerID)
	fees = serverram.AdjustCostForStanding(
		line.Info.CostInstall+line.Info.CostPerHour*durationSec/3600,
		standing, line.Info.DiscountPerGoodStanding, line.Info.SurchargePerBadStanding)

	solarSystemID, _ := evedb.GetSolarSystemForStation(ctx, m.db, b.StationID)

	// Charge, flag and insert in one transaction, in ramsvc's order minus its
	// material consumption.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("research install begin: %w", err)
	}
	defer tx.Rollback()
	charged, newBalance, err := evedb.TryChargeISK(ctx, tx, b.CharID, fees)
	if err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("research install charge: %w", err)
	}
	if !charged {
		return 0, installedBP, 0, 0, fmt.Errorf("cannot afford the %.0f ISK installation fee", fees)
	}
	if err := evedb.MoveBlueprintToFlag(ctx, tx, installedBP, 111); err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("research install flag blueprint: %w", err)
	}
	// Research: runs = levels, licensedProductionRuns = 0. Copying: runs = copies,
	// licensedProductionRuns = runs per copy. outputTypeID is the blueprint type.
	jobRuns, licensed := copies, 0
	if activityID == ramActivityCopying {
		licensed = int(runsPerCopy)
	}
	jobID, err = evedb.InsertJob(ctx, tx, line.Info.AssemblyLineID, b.StationID, b.CharID, installedBP,
		now, beginTime, endTime, jobRuns, int(activityID), bpTypeID, licensed, solarSystemID)
	if err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("research install job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("research install commit: %w", err)
	}

	// Fee journal as ramsvc does it: the line owner is credited for the slot.
	// Best effort, because the job is already installed.
	if fees > 0 {
		refType := researchRefType(activityID)
		reason := "Research job installation"
		if activityID == ramActivityCopying {
			reason = "Copy job installation"
		}
		_ = evedb.InsertJournalEntry(ctx, m.db, b.CharID, refType, b.CharID, line.Info.OwnerID,
			-fees, newBalance, reason)
		if line.Info.OwnerID > 0 {
			if cerr := evedb.CreditCorpWallet(ctx, m.db, line.Info.OwnerID, fees); cerr == nil {
				if ownerBal, berr := evedb.GetCorpBalance(ctx, m.db, line.Info.OwnerID); berr == nil {
					_ = evedb.InsertJournalEntry(ctx, m.db, line.Info.OwnerID, refType,
						b.CharID, line.Info.OwnerID, fees, ownerBal, "Assembly line usage fee")
				}
			}
		}
	}
	return jobID, installedBP, fees, endTime, nil
}

// moveCargoItemToHangar unloads one item from the ship's cargo into the station
// hangar by itemID. A typeID lookup (trade.go moveCargoToHangar) cannot pick the
// right blueprint when several of one type exist. The WHERE clause pins owner
// and ship, so a stale itemID cannot move another pilot's item.
func (m *Mod) moveCargoItemToHangar(ctx context.Context, b *botRun, itemID int64) error {
	if b.StationID == 0 {
		return fmt.Errorf("not docked")
	}
	res, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 4
		WHERE itemID = ? AND ownerID = ? AND locationID = ?`,
		b.StationID, itemID, b.CharID, b.ShipID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("item %d is not in this ship's hold", itemID)
	}
	return nil
}
