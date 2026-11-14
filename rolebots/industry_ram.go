package rolebots

// The server-industry glue: what the industry activity needs from the core's manufacturing and
// reprocessing stacks, driven as plain DB calls. The whole ramJobs lifecycle (install, cook, collect)
// is exported from gurimoon/db and gurimoon/server/ram, so installing a job here is the real
// thing: real waste math, the blueprint parked at flag 111, the fee journaled to the line's owner;
// the server's ram-job-sweep ripens it. Reprocessing math is unexported in server/reprocessing, so
// refineStack replicates the yield arithmetic against the same db helpers. If that service's formula
// changes this copy must follow (reprocessingsvc.go getReprocessingParams and calculateQuote are the
// reference). Both read the same reprocessing.* settings: the skill base here (refineSkillYieldBase),
// the tax in refineTaxAt (orerefine.go).

import (
	"context"
	"errors"
	"fmt"
	"math"

	evedb "gurimoon/db"
	"gurimoon/rates"
	serverram "gurimoon/server/ram"
)

// Industry skill typeIDs (RMR-era). The concurrency-limit pair lives in
// serverram (SkillMassProduction / SkillAdvancedMassProduction).
const (
	skillIndustry      = 3380 // -4% job time per level
	skillProductionEff = 3388 // -5% material waste per level
	skillIndRefining   = 3385 // refining yield term
	skillIndRefineryEf = 3389 // refining yield term
)

// Research skill typeIDs (RMR-era) and the ramActivities IDs the module installs. The numbering is
// the classic one: 3 is time (productivity) research and 4 is material research (sql/ramactivities.sql).
// Swapping them installs a job that raises the wrong level without any error.
const (
	skillScience    = 3402 // -5% copy time per level
	skillResearch   = 3403 // -5% PE (time) research per level
	skillMetallurgy = 3409 // -5% ME (material) research per level

	ramActivityManufacturing = 1
	ramActivityPEResearch    = 3
	ramActivityMEResearch    = 4
	ramActivityCopying       = 5
)

// researchRefType is the journal refType for one research activity's installation fee, matching
// ramsvc.ramActivityRefType exactly. The fee is the line owner's either way.
func researchRefType(activityID int64) int {
	switch activityID {
	case ramActivityPEResearch:
		return 58
	case ramActivityMEResearch:
		return 59
	case ramActivityCopying:
		return 60
	}
	return 56
}

// win32Now is the sim's clock in Win32 FILETIME ticks, the unit ramJobs speaks. It uses the module
// clock so the harness's fake clock drives it.
func win32Now() int64 { return evedb.UnixToWin32(clock.Unix()) }

// npcCorpStanding is the effective standing an NPC corporation holds toward the pilot: the corp's
// record folded with the faction's and the social-skill bonus, the chain ramsvc and reprocessingsvc
// walk. It gates assembly lines (RestrictByStanding) and sets the refinery tax.
func (m *Mod) npcCorpStanding(ctx context.Context, charID, corpID int64) float64 {
	factionID, _ := evedb.GetAgentCorpFactionID(ctx, m.db, corpID)
	corpStanding := evedb.GetNPCStandingToChar(ctx, m.db, charID, corpID)
	facStanding := evedb.GetNPCStandingToChar(ctx, m.db, charID, factionID)
	bonus := evedb.StandingSkillBonus(ctx, m.db, charID, corpStanding, facStanding, factionID)
	return evedb.ApplyStandingBonus(corpStanding, bonus)
}

// refineSkillYieldBase is the skill term's base before Refining and Refinery Efficiency, the
// reprocessing.skillYieldBase dial reprocessingsvc reads, with its default and range.
func refineSkillYieldBase() float64 {
	return evedb.CachedSettingFloat("reprocessing.skillYieldBase", 0.375, 0, 1)
}

// refineEffYield is the reprocessing yield expression, shared by refineStack and the mining disposal
// probe (miningRefineWorth, mining.go) so they agree on what a refine pays. The ore-specific skill
// folds in before the 100% cap; outside it the yield exceeds full recovery.
func refineEffYield(stationEff float64, refining, refineryEff, oreSkill int) float64 {
	return refineEffYieldFrom(refineSkillYieldBase(), stationEff, refining, refineryEff, oreSkill)
}

// refineEffYieldFrom is refineEffYield with the skill base given, so a test can pin it.
func refineEffYieldFrom(skillBase, stationEff float64, refining, refineryEff, oreSkill int) float64 {
	skillTerm := skillBase * (1 + float64(refining)*0.02) * (1 + float64(refineryEff)*0.04)
	y := stationEff + skillTerm*(1+float64(oreSkill)*0.05)
	if y > 1.0 {
		y = 1.0
	}
	return y
}

// refineOutcome is what one refined stack became.
type refineOutcome struct {
	UnitsIn  int64
	Minerals map[int64]int64 // typeID to units delivered to the hangar
}

// refineStack reprocesses one non-singleton hangar stack at the bot's current station with the
// station's efficiency, this pilot's refining skills and the owner corp's tax, as a player at the
// counter would get. Whole portions only; the remainder stays as ore. Returns a nil outcome (no
// error) when the stack is not refinable here.
func (m *Mod) refineStack(ctx context.Context, b *botRun, itemID int64) (*refineOutcome, error) {
	item, err := evedb.GetHangarItem(ctx, m.db, itemID, b.CharID, b.StationID)
	if err != nil || item == nil {
		return nil, err
	}
	if item.Singleton != 0 {
		return nil, nil // assembled things are not this bot's business
	}
	portion, err := evedb.GetTypePortionSize(ctx, m.db, item.TypeID)
	if err != nil || portion <= 0 {
		portion = 1
	}
	runs := item.Quantity / portion
	if runs <= 0 {
		return nil, nil
	}
	mats, err := evedb.GetTypeMaterials(ctx, m.db, item.TypeID)
	if err != nil || len(mats) == 0 {
		return nil, err
	}
	// A station that cannot reprocess is an error here, not a refusal: the caller only reaches this
	// with a stack it means to refine. Read first so a bad station fails before any ore is destroyed.
	if _, err := evedb.GetStationReprocessingInfo(ctx, m.db, b.StationID); err != nil {
		return nil, err
	}
	// The station's efficiency, this pilot's three skills and the owner corp's tax, gathered by
	// refineHandAt (orerefine.go), which also reads the ore skill because a stack knows what rock it is.
	hand := m.refineHandAt(ctx, b, b.StationID, item.GroupID)
	tax := hand.Tax
	effYield := refineEffYield(hand.StationEff, hand.Refining, hand.RefineryEff, hand.OreSkill)

	qtyToProcess := runs * portion
	out := &refineOutcome{UnitsIn: qtyToProcess, Minerals: map[int64]int64{}}

	// Destroy ore and create minerals atomically: a failure midway must not eat the ore without
	// producing anything.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("refine begin: %w", err)
	}
	defer tx.Rollback()
	if item.Quantity-qtyToProcess <= 0 {
		if err := evedb.DeleteEntity(ctx, tx, itemID); err != nil {
			return nil, fmt.Errorf("refine consume: %w", err)
		}
	} else if err := evedb.ReduceEntityQuantity(ctx, tx, itemID, qtyToProcess); err != nil {
		return nil, fmt.Errorf("refine consume: %w", err)
	}
	for _, mat := range mats {
		total := int64(math.Floor(float64(runs) * float64(mat.Quantity) * effYield))
		stationTake := int64(math.Floor(float64(total) * tax))
		player := total - stationTake
		if player <= 0 {
			continue
		}
		existingID, existingQty, err := evedb.FindHangarItemByType(ctx, tx, mat.MaterialTypeID, b.CharID, b.StationID)
		if err != nil {
			return nil, fmt.Errorf("refine deposit: %w", err)
		}
		if existingID != 0 {
			if err := evedb.UpdateCargoItemQty(ctx, tx, existingID, existingQty+player); err != nil {
				return nil, fmt.Errorf("refine deposit: %w", err)
			}
		} else if _, err := evedb.CreateItem(ctx, tx, mat.MaterialTypeID, b.CharID, b.StationID, int(player)); err != nil {
			return nil, fmt.Errorf("refine deposit: %w", err)
		}
		out.Minerals[mat.MaterialTypeID] = player
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("refine commit: %w", err)
	}
	// Ore-flow counters (oreflow.go) are booked here, after the commit, and for ore only: dead-stock
	// reprocessing (deadstock.go) uses this function and oreFlowTotals sums every type it is given.
	if item.CategoryID == categoryAsteroid {
		m.noteOreFlow(ctx, item.TypeID, oreStageRefinedIn, qtyToProcess, 0)
		m.noteOreFlowLots(ctx, oreStageRefinedOut, out.Minerals, 0)
	}
	return out, nil
}

// manufactureLine is the assembly line an install (or a quote) would use at a station, checked
// without committing money: the line exists, its owner will deal with this pilot, and its queue is
// not backed up past the horizon a bot will wait out.
type manufactureLine struct {
	Info    *evedb.AssemblyLineInfo
	TimeMul float64
	MatMul  float64
}

// industryQueueCapSec is the longest a line's queue may run before a bot looks elsewhere, so a
// demand-ledger claim does not sit behind someone's week of jobs.
const industryQueueCapSec = 24 * 3600

// manufactureLineAt resolves the least-busy manufacturing line at a station
// for this pilot and product, or explains why there is none it can use.
func (m *Mod) manufactureLineAt(ctx context.Context, b *botRun, stationID, productTypeID int64) (*manufactureLine, error) {
	return m.assemblyLineAt(ctx, b, stationID, 1, productTypeID)
}

// activityLineNoun names an activity the way a refusal should read. Only the
// activities this module installs are listed; anything else is reported by
// number rather than guessed at.
func activityLineNoun(activityID int64) string {
	switch activityID {
	case 1:
		return "manufacturing"
	case ramActivityMEResearch:
		return "material research"
	case ramActivityPEResearch:
		return "time research"
	case ramActivityCopying:
		return "copying"
	}
	return fmt.Sprintf("activity-%d", activityID)
}

// lineRefusal is a station saying no for a reason that will still hold next pass: no line of the
// kind, a standing the pilot lacks, a corp restriction. Anything else assemblyLineAt returns is
// transient: a queue that will clear in hours, or a database that did not answer.
type lineRefusal struct{ msg string }

func (e lineRefusal) Error() string { return e.msg }

// isLineRefusal reports whether err is a lasting refusal (lineRefusal).
func isLineRefusal(err error) bool {
	var r lineRefusal
	return errors.As(err, &r)
}

func (m *Mod) assemblyLineAt(ctx context.Context, b *botRun, stationID, activityID, typeID int64) (*manufactureLine, error) {
	// assemblyLineAt resolves the least-busy line of any activity; its gates are the line's, not the job's.
	// typeID is the output type (the blueprint itself for research). Manufacturing picks the line by
	// product: the core refuses a hull on a line type that may not build it (LineCanBuild).
	var lineID int64
	var err error
	if activityID == 1 {
		lineID, err = evedb.FindAssemblyLineForProduct(ctx, m.db, stationID, activityID, typeID)
	} else {
		lineID, err = evedb.FindAssemblyLineForActivity(ctx, m.db, stationID, activityID)
	}
	if err != nil {
		return nil, err
	}
	if lineID == 0 {
		if activityID == 1 && typeID != 0 {
			return nil, lineRefusal{fmt.Sprintf("no line at station %d may build %s", stationID, m.typeNameOr(ctx, typeID))}
		}
		return nil, lineRefusal{fmt.Sprintf("station %d has no %s line", stationID, activityLineNoun(activityID))}
	}
	info, err := evedb.GetAssemblyLineInfo(ctx, m.db, lineID)
	if err != nil {
		return nil, err
	}
	if info.RestrictionMask&serverram.RestrictByStanding != 0 {
		if s := m.npcCorpStanding(ctx, b.CharID, info.OwnerID); s < info.MinimumStanding {
			return nil, lineRefusal{fmt.Sprintf("the line's owner will not deal with this pilot (standing %.1f, needs %.1f)",
				s, info.MinimumStanding)}
		}
	}
	if info.RestrictionMask&(serverram.RestrictByCorp|serverram.RestrictByAlliance) != 0 && info.OwnerID != b.CorpID {
		return nil, lineRefusal{"the line is restricted to its owning corporation"}
	}
	if info.NextFreeTime > win32Now()+industryQueueCapSec*10_000_000 {
		return nil, lineQueuedOut{StationID: stationID, ActivityID: activityID}
	}
	tMul, mMul := evedb.GetLineDetailMultipliers(ctx, m.db, info.AssemblyLineTypeID, typeID)
	return &manufactureLine{
		Info:    info,
		TimeMul: info.BaseTimeMultiplier * tMul,
		MatMul:  info.BaseMaterialMultiplier * mMul,
	}, nil
}

// manufactureDurationSec is the real job duration for this pilot on this line: ramsvc's arithmetic,
// including the server's rate knobs.
func manufactureDurationSec(bpType *evedb.BlueprintTypeInfo, peLevel int64, industrySkill, runs int, line *manufactureLine) float64 {
	d := serverram.CalcProductionTime(bpType.ProductionTime, bpType.ProductivityModifier, peLevel, industrySkill)
	d *= float64(runs) * line.TimeMul
	d /= rates.IndustrySpeed
	d /= rates.ProductionSpeed
	return d
}

// researchDurationSec is the real duration of one research job on this line, ramsvc's arithmetic
// for activities 3 and 4. The rate chain divides by IndustrySpeed and then ResearchSpeed, not
// ProductionSpeed. The level argument is the current one; the server prices every level the same,
// so it only keeps the signature matching the formulas.
func researchDurationSec(bpType *evedb.BlueprintTypeInfo, activityID int64,
	currentLevel int64, runs int, skillLevel int, line *manufactureLine) float64 {
	var d float64
	switch activityID {
	case ramActivityMEResearch:
		d = serverram.CalcMEResearchTime(bpType.ResearchMaterialTime, currentLevel, runs, skillLevel)
	case ramActivityPEResearch:
		d = serverram.CalcPEResearchTime(bpType.ResearchProductivityTime, currentLevel, runs, skillLevel)
	default:
		return 0
	}
	d *= line.TimeMul
	d /= rates.IndustrySpeed
	d /= rates.ResearchSpeed
	return d
}

// installManufactureJob runs the real install recipe (charge, consume, flag the blueprint, insert
// the job) in one transaction, in ramsvc.handleInstallJob's order. It returns the jobID, the fee and
// the market value of the materials consumed. A stacked blueprint is split by EnsureBlueprintSingleton
// and the caller must keep the returned itemID, or the whole stack would be installed.
func (m *Mod) installManufactureJob(ctx context.Context, b *botRun, bpItemID, bpTypeID int64, runs int) (jobID, installedBP int64, fees, matValue float64, err error) {
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
	if bpType.MaxProductionLimit > 0 && int64(runs) > bpType.MaxProductionLimit {
		runs = int(bpType.MaxProductionLimit)
	}
	// A copy builds at most the runs it has left: without this the core delivers every run and then
	// deletes the copy. The caller trims IndRuns first (industryInstall); this is the backstop.
	if attrs.Copy == 1 {
		if attrs.RunsRemaining <= 0 {
			return 0, installedBP, 0, 0, fmt.Errorf("the blueprint copy has no runs left")
		}
		if runs > attrs.RunsRemaining {
			runs = attrs.RunsRemaining
		}
	}

	line, err := m.manufactureLineAt(ctx, b, b.StationID, bpType.ProductTypeID)
	if err != nil {
		return 0, installedBP, 0, 0, err
	}

	// The same gates the client path enforces: concurrent-job cap from the
	// Mass Production pair, and the blueprint's own skill requirements.
	baseSkill, _ := m.charSkillLevelFor(ctx, b.CharID, serverram.SkillMassProduction)
	advSkill, _ := m.charSkillLevelFor(ctx, b.CharID, serverram.SkillAdvancedMassProduction)
	maxJobs := serverram.MaxConcurrentJobs(baseSkill, advSkill)
	if active, cerr := evedb.CountActiveJobs(ctx, m.db, b.CharID, true); cerr == nil && active >= maxJobs {
		return 0, installedBP, 0, 0, fmt.Errorf("already running its %d allowed manufacturing job(s)", maxJobs)
	}
	if reqs, rerr := evedb.GetActivitySkillReqs(ctx, m.db, bpTypeID, 1); rerr == nil {
		for _, r := range reqs {
			if have, _ := m.charSkillLevelFor(ctx, b.CharID, r.SkillID); have < r.Level {
				return 0, installedBP, 0, 0, fmt.Errorf("missing required skill %d at level %d", r.SkillID, r.Level)
			}
		}
	}

	industrySkill, _ := m.charSkillLevelFor(ctx, b.CharID, skillIndustry)
	peSkill, _ := m.charSkillLevelFor(ctx, b.CharID, skillProductionEff)
	durationSec := manufactureDurationSec(bpType, attrs.ProductivityLevel, industrySkill, runs, line)

	now := win32Now()
	beginTime := now
	if line.Info.NextFreeTime > beginTime {
		beginTime = line.Info.NextFreeTime
	}
	endTime := beginTime + int64(durationSec*10_000_000)

	standing := m.npcCorpStanding(ctx, b.CharID, line.Info.OwnerID)
	fees = serverram.AdjustCostForStanding(
		line.Info.CostInstall+line.Info.CostPerHour*durationSec/3600,
		standing, line.Info.DiscountPerGoodStanding, line.Info.SurchargePerBadStanding)

	solarSystemID, _ := evedb.GetSolarSystemForStation(ctx, m.db, b.StationID)
	licensedRuns := 0
	if attrs.Copy == 1 {
		licensedRuns = attrs.RunsRemaining
	}

	// Charge, consume, flag, insert in one transaction, in ramsvc's order.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("install begin: %w", err)
	}
	defer tx.Rollback()
	charged, newBalance, err := evedb.TryChargeISK(ctx, tx, b.CharID, fees)
	if err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("install charge: %w", err)
	}
	if !charged {
		return 0, installedBP, 0, 0, fmt.Errorf("cannot afford the %.0f ISK installation fee", fees)
	}
	consumed, err := evedb.ConsumeMaterials(ctx, tx, b.CharID, b.StationID, bpTypeID,
		attrs.MaterialLevel, peSkill, runs, line.MatMul)
	if err != nil {
		return 0, installedBP, 0, 0, err
	}
	if err := evedb.MoveBlueprintToFlag(ctx, tx, installedBP, 111); err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("install flag blueprint: %w", err)
	}
	jobID, err = evedb.InsertJob(ctx, tx, line.Info.AssemblyLineID, b.StationID, b.CharID, installedBP,
		now, beginTime, endTime, runs, 1, bpType.ProductTypeID, licensedRuns, solarSystemID)
	if err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("install job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, installedBP, 0, 0, fmt.Errorf("install commit: %w", err)
	}

	// Price the consumed materials at market value, the cost the sale's cost-plus arithmetic recovers.
	// Read after the commit so a rollback never leaves half a valuation.
	for _, cs := range consumed {
		if p, perr := m.estimatePriceFor(ctx, b, cs.TypeID); perr == nil {
			matValue += p * float64(cs.OldQty-cs.NewQty)
		}
	}

	// Fee journaling as ramsvc does it (refManufacturing = 56, line owner credited). Best effort:
	// the job is already real.
	if fees > 0 {
		_ = evedb.InsertJournalEntry(ctx, m.db, b.CharID, 56, b.CharID, line.Info.OwnerID,
			-fees, newBalance, "Industry job installation")
		if line.Info.OwnerID > 0 {
			if cerr := evedb.CreditCorpWallet(ctx, m.db, line.Info.OwnerID, fees); cerr == nil {
				if ownerBal, berr := evedb.GetCorpBalance(ctx, m.db, line.Info.OwnerID); berr == nil {
					_ = evedb.InsertJournalEntry(ctx, m.db, line.Info.OwnerID, 56,
						b.CharID, line.Info.OwnerID, fees, ownerBal, "Assembly line usage fee")
				}
			}
		}
	}
	return jobID, installedBP, fees, matValue, nil
}

// industryJobStatus reads one job's state: ready means the bot can collect
// (CompleteJob itself accepts a past end time, so the 30s sweep's lag never
// blocks a collection).
func (m *Mod) industryJobStatus(ctx context.Context, jobID int64) (status int64, ready bool, endTime int64, err error) {
	err = m.db.QueryRowContext(ctx, `
		SELECT completedStatusID, COALESCE(endProductionTime, 0)
		FROM ramJobs WHERE jobID = ?`, jobID).Scan(&status, &endTime)
	if err != nil {
		return 0, false, 0, err
	}
	ready = status == 1 || (status == 0 && endTime <= win32Now())
	return status, ready, endTime, nil
}

// moveItemToCargo loads one item into the ship's cargo by itemID. The stack mover (trade.go
// moveHangarToCargo) skips singletons, and a blueprint is a singleton: this is how the BPO rides
// to the factory.
func (m *Mod) moveItemToCargo(ctx context.Context, b *botRun, itemID int64) error {
	if b.ShipID == 0 {
		return fmt.Errorf("not in a ship")
	}
	_, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 5
		WHERE itemID = ? AND ownerID = ?`, b.ShipID, itemID, b.CharID)
	return err
}
