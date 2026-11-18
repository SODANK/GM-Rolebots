package rolebots

// Blueprint research, the second industry slot track: Laboratory Operation buys a pool of slots separate from
// Mass Production, counted over every ramJobs activity but 1. A lane, not a phase of the industry activity: it
// fires in the gap the factory leaves and costs only the line's fee. research.go is the R&D-agent lottery; here is lab*.
//
//	gate  a free research slot and money above the floor
//	pick  a print it builds, the activity the ME/PE ladder wants, cheapest lab hours first
//	go    the factory's own lab, else the nearest lab station in reach, with the print in the hold
//	cook  install and go idle; the server counts the slot, so the pilot may open another
//	ring  labOvenSweep takes the job off the line from anywhere (labwork_oven.go)
//	home  a stray sweep returns the print left at the lab (labprints.go)

import (
	"context"
	"fmt"
	"time"

	evedb "gurimoon/db"
	serverram "gurimoon/server/ram"
)

// labSoftCapSec is the longest research job a pilot takes willingly: 48 hours of real duration, after
// line multipliers and rate divisors. A preference, not a limit (labPickPrint's second arm). It ranks
// and sizes research only; copy jobs are never trimmed by it (copyBatch, corpWorkFor).
const labSoftCapSec = 48 * 3600

// labResearchLevels is how many levels one personal research job takes: every level left to the lane's
// target, trimmed to what fits labSoftCapSec, never under one, so a batch fills the line instead of a
// round trip per level. The trim protects the build book (the print sits at flag 111 for the whole job);
// it is not a research cap. Pure.
func labResearchLevels(level, target int64, perLevelSec float64) int {
	n := max(target-level, 1)
	if perLevelSec > 0 {
		n = min(n, max(int64(labSoftCapSec/perLevelSec), 1))
	}
	return int(n)
}

// labFailCooldownSec holds the lane off after a pass that found nothing worth researching, like
// IndBookFailAt: the pick is a query per candidate and its answer does not change between passes.
const labFailCooldownSec = 30 * 60

// labCandidateLimit is how many owned prints the picker weighs; a pilot holds a handful.
const labCandidateLimit = 40

// labFreeSlots is how many research jobs this pilot could still install: its Laboratory Operation
// ceiling minus what ramJobs says is running. It mirrors industryFreeSlots with the Laboratory Operation
// skills and CountActiveJobs asked for activityID <> 1, so a pilot with every oven baking still has
// every lab slot free.
func (m *Mod) labFreeSlots(ctx context.Context, b *botRun) (int, error) {
	base, err := m.charSkillLevelFor(ctx, b.CharID, serverram.SkillLaboratoryOperation)
	if err != nil {
		return 0, err
	}
	adv, err := m.charSkillLevelFor(ctx, b.CharID, serverram.SkillAdvancedLabOperation)
	if err != nil {
		return 0, err
	}
	if free := serverram.MaxConcurrentJobs(base, adv) - m.labActiveJobs(ctx, b); free > 0 {
		return free, nil
	}
	return 0, nil
}

// labActiveJobs counts this pilot's undelivered non-manufacturing jobs from ramJobs and never from
// LabJobID, as industryFreeSlots does for IndCooking: the state blob is the pilot's memory, and the
// ESP factory page can cancel or deliver a job from outside it.
func (m *Mod) labActiveJobs(ctx context.Context, b *botRun) int {
	n, err := evedb.CountActiveJobs(ctx, m.db, b.CharID, false)
	if err != nil {
		return 0
	}
	return n
}

// labStationHasLine reports whether a station has a line for a research
// activity. The factory half of an address is ramassemblylines rather than a
// station service bit, and the lab half is the same table with a different
// activityID (corphome.go stationHasLine is the manufacturing twin).
func (m *Mod) labStationHasLine(ctx context.Context, stationID, activityID int64) bool {
	var one int64
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM ramassemblylines
		 WHERE containerID = ? AND activityID = ? LIMIT 1`, stationID, activityID).Scan(&one)
	return err == nil
}

// labCandidate is one print this pilot could put in a lab, priced.
type labCandidate struct {
	ItemID     int64
	TypeID     int64 // the blueprint type, also the job's output type
	Name       string
	ActivityID int64 // 4 (material) or 3 (time)
	Level      int64 // the level it currently sits at for that activity
	// Levels is a research candidate's batch: how many levels the one job
	// adds (labResearchLevels).
	Levels int
	// Evidence is how much this factory depends on the print: 2 for one it has built here, 1 for one the
	// demand ledger asks for, 0 for one it merely owns. Read forwards under the soft cap and backwards
	// above it (labPickPrint).
	Evidence int
	// PreferPE is which research activity the pick wants for this print, decided per print by the ME/PE
	// ladder and per pilot by what it already has cooking (labPrefersPE, labSlotBiasPE). Both candidates
	// for one print carry the same value.
	PreferPE bool
	Seconds  float64
	// BaseTime and Skill are what the fee quote re-derives the duration from, kept so labFeeQuote costs
	// no query of its own.
	BaseTime int64
	Skill    int
	// A copy candidate (ActivityID 5) also carries the order: copies, runs on each, and the print's run
	// maximum, which the copy-time formula scales against (bpcopy.go).
	Copies   int
	CopyRuns int64
	MaxRuns  int64
}

// labPickPrint chooses the print and the activity for one research trip: the print it builds, then the
// activity the ladder wants, then cheapest lab hours. ME leads by labMELead levels. When nothing fits
// under labSoftCapSec it inverts the evidence ranking and takes the print the factory needs least, since
// a long job holds the blueprint at flag 111 where ownedCandidates (buildbook.go) cannot see it.
func (m *Mod) labPickPrint(ctx context.Context, b *botRun, factoryID int64) (*labCandidate, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, t.typeName,
		       COALESCE(bp.materialLevel, 0), COALESCE(bp.productivityLevel, 0),
		       COALESCE(bt.researchMaterialTime, 0), COALESCE(bt.researchProductivityTime, 0),
		       COALESCE(bt.researchCopyTime, 0), COALESCE(bt.maxProductionLimit, 10),
		       CASE WHEN sh.typeID IS NOT NULL THEN 2
		            WHEN dm.typeID IS NOT NULL THEN 1
		            ELSE 0 END,
		       COALESCE(pg.categoryID, 0), COALESCE(pg.groupID, 0)
		  FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  JOIN invTypes t ON t.typeID = e.typeID
		  LEFT JOIN invTypes pt ON pt.typeID = bt.productTypeID
		  LEFT JOIN invGroups pg ON pg.groupID = pt.groupID
		  LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
		  LEFT JOIN rolebot_industry_shelf sh
		         ON sh.characterID = e.ownerID AND sh.stationID = e.locationID
		        AND sh.typeID = bt.productTypeID
		  LEFT JOIN rolebot_market_demand dm ON dm.typeID = bt.productTypeID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4
		   AND COALESCE(bp.copy, 0) = 0
		 ORDER BY COALESCE(pt.basePrice, 0) DESC
		 LIMIT ?`, b.CharID, factoryID, labCandidateLimit)
	if err != nil {
		return nil, fmt.Errorf("lab candidates: %w", err)
	}
	defer rows.Close()

	type raw struct {
		itemID, typeID   int64
		name             string
		meLevel, peLevel int64
		meTime, peTime   int64
		copyTime, maxRun int64
		evidence         int
		category, group  int64
	}
	var pool []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.itemID, &r.typeID, &r.name, &r.meLevel, &r.peLevel,
			&r.meTime, &r.peTime, &r.copyTime, &r.maxRun, &r.evidence, &r.category, &r.group); err != nil {
			return nil, err
		}
		pool = append(pool, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		return nil, nil
	}

	// Ranking durations use the pilot's skills with no line multiplier (labBaseSeconds), since the lab is
	// not chosen yet. Skills are read once, not per candidate.
	metal, _ := m.charSkillLevelFor(ctx, b.CharID, skillMetallurgy)
	research, _ := m.charSkillLevelFor(ctx, b.CharID, skillResearch)
	science, _ := m.charSkillLevelFor(ctx, b.CharID, skillScience)
	fullME, fullPE := m.fullResearchLevels(ctx)

	// Copying is a research career's business, and only where the copies land in the library itself: a copy
	// job at another lab would leave copies in a hangar nothing sells from (the stray sweep moves one print a trip).
	var copyStock, copySold map[int64]int
	canCopy := researchCareer(b.Tune) && m.labStationHasLine(ctx, factoryID, ramActivityCopying)
	if canCopy {
		stock, open, serr := m.copyStockFor(ctx, b, factoryID)
		sold, derr := m.copyDemand(ctx)
		copyStock, copySold = stock, sold
		if serr != nil || derr != nil || open >= copyListingCap {
			canCopy = false
		}
	}

	// What this pilot is allowed to research, asked once for the whole pool (labprints.go); otherwise the
	// refusal arrives at the counter after the flight.
	bpTypes := make([]int64, 0, len(pool))
	for _, r := range pool {
		bpTypes = append(bpTypes, r.typeID)
	}
	allowed, err := m.labResearchSkillsMet(ctx, b.CharID, bpTypes)
	if err != nil {
		return nil, err
	}

	// Which pool this pilot's next slot should lean towards, decided once from what it already has on lines.
	biasPE := labSlotBiasPE(b.State)
	// A hull print (ownercopy.go) is copied without waiting for full research or a sale: research takes months at hull times, and
	// no hull copy can sell before one is listed.
	hullCopies := ownerCopyOn(ctx, m)

	var under, over, copies []labCandidate
	for _, r := range pool {
		// A print committed to this pilot's live manufacturing project is not available: installing it would
		// take the blueprint the project builds from.
		if b.State != nil && b.State.Data.IndBPItemID == r.itemID {
			continue
		}
		full := printFullyResearched(r.meLevel, r.peLevel, fullME, fullPE)
		// A copy order for this print when its stock is short. A full print is always worth copying; one still
		// being researched is copied only when copies are selling, the factory leans on its product, or it is a hull.
		if canCopy && r.copyTime > 0 && allowed[ramActivityCopying][r.typeID] &&
			(full || copySold[r.typeID] > 0 || r.evidence >= 1 || (hullCopies && isOwnerCopyHull(r.category, r.group))) {
			if f, ferr := m.printFactsFor(ctx, r.typeID); ferr == nil {
				perCopy := labBaseCopySeconds(r.copyTime, r.maxRun, f.CopyRuns, science)
				target := copyStockTargetFor(copySold[r.typeID])
				if n := copyBatch(target, copyStock[r.typeID]); n > 0 {
					copies = append(copies, labCandidate{
						ItemID: r.itemID, TypeID: r.typeID, Name: r.name,
						ActivityID: ramActivityCopying, Evidence: r.evidence + copySold[r.typeID],
						Seconds:  perCopy * float64(n),
						BaseTime: r.copyTime, Skill: science,
						Copies: n, CopyRuns: f.CopyRuns, MaxRuns: r.maxRun,
					})
				}
			}
		}
		preferPE := biasPE || labPrefersPE(r.meLevel, r.peLevel)
		for _, opt := range []struct {
			activity int64
			level    int64
			base     int64
			skill    int
			full     int64
		}{
			{ramActivityMEResearch, r.meLevel, r.meTime, metal, fullME},
			{ramActivityPEResearch, r.peLevel, r.peTime, research, fullPE},
		} {
			if opt.base <= 0 {
				continue
			}
			// The lane stops at its target (rolebots.bpoFullME/PE): past it each level costs the same lab time for a
			// shrinking saving. The game has no cap; this is the pilot's spending rule, and a research career copies instead.
			if opt.level >= opt.full {
				continue
			}
			if !allowed[opt.activity][r.typeID] {
				continue
			}
			perLevel := labBaseSeconds(opt.activity, opt.base, opt.level, opt.skill)
			levels := labResearchLevels(opt.level, opt.full, perLevel)
			c := labCandidate{
				ItemID: r.itemID, TypeID: r.typeID, Name: r.name,
				ActivityID: opt.activity, Level: opt.level, Levels: levels, Evidence: r.evidence,
				PreferPE: preferPE,
				Seconds:  perLevel * float64(levels),
				BaseTime: opt.base, Skill: opt.skill,
			}
			if c.Seconds <= labSoftCapSec {
				under = append(under, c)
			} else {
				over = append(over, c)
			}
		}
	}

	// Copy or research. With no copy job cooking the next slot copies, which is the career's income; with one
	// cooking, research takes the slot and copying is the fallback.
	copyFirst := b.State == nil || !labCookingHas(b.State.Data.LabCooking, ramActivityCopying)
	if copyFirst {
		if best := pickLabCopy(copies); best != nil {
			return best, nil
		}
	}
	if best := pickLabUnderCap(under); best != nil {
		return best, nil
	}
	if best := pickLabCopy(copies); best != nil {
		return best, nil
	}
	return pickLabOverCap(over), nil
}

// pickLabCopy takes the copy order with the most evidence behind it (copies
// selling, the factory building it), then the bigger batch, then the shorter
// job. Pure.
func pickLabCopy(in []labCandidate) *labCandidate {
	var best *labCandidate
	for i := range in {
		c := &in[i]
		switch {
		case best == nil:
			best = c
		case c.Evidence != best.Evidence:
			if c.Evidence > best.Evidence {
				best = c
			}
		case c.Copies != best.Copies:
			if c.Copies > best.Copies {
				best = c
			}
		case c.Seconds < best.Seconds:
			best = c
		}
	}
	return best
}

// labBaseCopySeconds is the ranking duration of one copy, the copy twin of labBaseSeconds: the server's
// formula and rate knobs, a line multiplier of 1.
func labBaseCopySeconds(copyTime, maxRuns, runsPerCopy int64, science int) float64 {
	bp := &evedb.BlueprintTypeInfo{ResearchCopyTime: copyTime, MaxProductionLimit: maxRuns}
	return copyDurationSec(bp, 1, runsPerCopy, science, &manufactureLine{TimeMul: 1})
}

// labBaseSeconds is the research duration a candidate is ranked on: the server's formula with the pilot's
// skill and the rate knobs, without a line's multipliers (the pick happens before a lab is chosen). The
// install re-derives the real number (researchDurationSec), so this is an ordering key and a cap test,
// never a quote.
func labBaseSeconds(activityID, baseTime, currentLevel int64, skill int) float64 {
	line := &manufactureLine{TimeMul: 1}
	bp := &evedb.BlueprintTypeInfo{
		ResearchMaterialTime:     baseTime,
		ResearchProductivityTime: baseTime,
	}
	return researchDurationSec(bp, activityID, currentLevel, 1, skill, line)
}

// labMELead is how many levels a print's material research may run ahead of its productivity research
// before the next trip goes to a productivity line. Material leads because minerals are scarce and lab
// time is not; one level keeps the separate PE line pool in use. See docs/comment-notes-b12.md#lab-me-lead.
const labMELead = 1

// labPrefersPE reports whether the NEXT research level on one print should be
// productivity rather than material: it should once material has taken its
// labMELead lead.
func labPrefersPE(meLevel, peLevel int64) bool {
	return meLevel >= peLevel+labMELead
}

// labSlotBiasPE reports whether this pilot's next lab slot should lean towards productivity research
// whatever the per-print ladder says. It does when everything the pilot already has on a line is material
// research: two slots would otherwise both go on ME lines on the same pass, because the picks are
// independent. A pilot with nothing cooking is not biased.
func labSlotBiasPE(st *SimState) bool {
	if st == nil || len(st.Data.LabCooking) == 0 {
		return false
	}
	return !labCookingHas(st.Data.LabCooking, ramActivityPEResearch)
}

// labActivityRank orders the two research activities against each other when both are on offer for one
// print: the wanted one first. It is a rank, not a filter, so a print whose preferred activity passed
// labSoftCapSec still offers the other.
func labActivityRank(activityID int64, preferPE bool) int {
	want := int64(ramActivityMEResearch)
	if preferPE {
		want = ramActivityPEResearch
	}
	if activityID == want {
		return 0
	}
	return 1
}

// pickLabUnderCap takes the print the factory leans on hardest, breaking ties
// on the shorter job.
func pickLabUnderCap(in []labCandidate) *labCandidate {
	var best *labCandidate
	for i := range in {
		c := &in[i]
		if best == nil || labBetterUnderCap(c, best) {
			best = c
		}
	}
	return best
}

// pickLabOverCap is the inverted arm: nothing fits under the cap, so take the shortest job, and among
// equals the print this factory depends on least, since the blueprint is unavailable for a long time.
func pickLabOverCap(in []labCandidate) *labCandidate {
	var best *labCandidate
	for i := range in {
		c := &in[i]
		if best == nil || c.Seconds < best.Seconds ||
			(c.Seconds == best.Seconds && c.Evidence < best.Evidence) {
			best = c
		}
	}
	return best
}

// labBetterUnderCap is the under-cap ordering: what the factory leans on
// hardest, then the activity the ladder wants for that print, then the
// shorter job.
func labBetterUnderCap(c, best *labCandidate) bool {
	if c.Evidence != best.Evidence {
		return c.Evidence > best.Evidence
	}
	if ca, ba := labActivityRank(c.ActivityID, c.PreferPE), labActivityRank(best.ActivityID, best.PreferPE); ca != ba {
		return ca < ba
	}
	return c.Seconds < best.Seconds
}

// labStationFor resolves where this pilot's research job goes: its own factory when that station has a
// line for the activity, otherwise the nearest station in reach that does (no school penalty). It returns
// the line too, because the caller prices the job before setting off (labFeeQuote).
func (m *Mod) labStationFor(ctx context.Context, b *botRun, factoryID, factorySys, activityID int64) (int64, int64, *manufactureLine, error) {
	if factoryID != 0 && m.labStationHasLine(ctx, factoryID, activityID) {
		if line, err := m.assemblyLineAt(ctx, b, factoryID, activityID, 0); err == nil {
			loc, lerr := evedb.GetStationLocation(ctx, m.db, factoryID)
			if lerr == nil {
				return factoryID, loc.SolarSystemID, line, nil
			}
		}
	}
	// Reach is measured from the factory, not from the pilot: the round trip is factory, lab, factory, so a
	// lab two jumps from the pilot and twenty from the factory is the wrong answer.
	origin := factorySys
	if origin == 0 {
		origin = b.SystemID
	}
	reach, err := m.reachFrom(ctx, origin, m.economicRules(ctx, b, defaultBand(b)))
	if err != nil {
		return 0, 0, nil, err
	}
	for _, f := range stationFilterPasses(b.Profile, b.Tune) {
		lines, qerr := m.lineStations(ctx, activityID, "", f)
		if qerr != nil {
			return 0, 0, nil, fmt.Errorf("lab scan: %w", qerr)
		}
		type fc struct {
			stationID, sysID int64
			jumps            int
		}
		var finalists []fc
		for _, ln := range lines {
			j, ok := reach[ln.sysID]
			if !ok {
				continue
			}
			finalists = append(finalists, fc{stationID: ln.stationID, sysID: ln.sysID, jumps: j})
		}
		for tries := 0; tries < 5 && len(finalists) > 0; tries++ {
			best := 0
			for i := range finalists {
				if finalists[i].jumps < finalists[best].jumps {
					best = i
				}
			}
			c := finalists[best]
			finalists = append(finalists[:best], finalists[best+1:]...)
			if line, lerr := m.assemblyLineAt(ctx, b, c.stationID, activityID, 0); lerr == nil {
				return c.stationID, c.sysID, line, nil
			}
		}
	}
	return 0, 0, nil, fmt.Errorf("no reachable station with a %s line will deal with this pilot",
		activityLineNoun(activityID))
}

// lineStation is one station with an assembly line of the asked activity.
type lineStation struct{ stationID, sysID int64 }

// lineStations is every station carrying a line of one activity under one station filter, shared across
// the roster for ten minutes: the set changes only when a structure's lines are synced (a POS array
// onlining). copyClause is the factory scan's extra EXISTS on a copying line ("" for none).
func (m *Mod) lineStations(ctx context.Context, activityID int64, copyClause string, f stationFilter) ([]lineStation, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshotFor(snapKey("line-stations", activityID, copyClause, f.Clause, f.Args), 10*time.Minute,
		func() ([]lineStation, error) {
			rows, err := m.db.QueryContext(sctx, `
				SELECT DISTINCT al.containerID, s.solarSystemID
				FROM ramassemblylines al
				JOIN staStations s ON s.stationID = al.containerID
				WHERE al.activityID = ?`+copyClause+f.Clause,
				append([]any{activityID}, f.Args...)...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []lineStation
			for rows.Next() {
				var ln lineStation
				if err := rows.Scan(&ln.stationID, &ln.sysID); err != nil {
					return nil, err
				}
				out = append(out, ln)
			}
			return out, rows.Err()
		})
}
