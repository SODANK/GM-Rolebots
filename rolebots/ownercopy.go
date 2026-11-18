package rolebots

import (
	"context"

	evedb "gurimoon/db"
)

// Owner copies: any pilot holding a hull original copies it and puts the copies on escrow.
//
// The copy lane (labwork.go, escrow_sell.go) belongs to research careers, and a research career copies a
// print only once it is fully researched or its copies are already selling. A hull print takes months of
// lab time to reach full research, and no cruiser copy has ever been listed, so none has ever sold: the
// lane never opens for hulls. Meanwhile hundreds of cruiser originals sit in the hangars of traders,
// miners and fighters who bought them on the way up, and the roster's builders, few of whom can carry an
// original, have nothing to build a cruiser from.
//
// So the housekeeping pass every solo pilot gets (labwork, startNew=false) also asks: is this pilot docked
// with a hull original, at a station with a copy line and a free lab slot, and short of copies of it? Then
// it installs a copy job on the spot, no trip, and the copies it gets back are listed by
// listLibraryProducts, which lets a non-career pilot list hull copies of its own make.

// ownerCopyKey switches the owner copy lane (server_settings, default on).
const ownerCopyKey = "rolebots.ownerCopy.hulls"

// ownerCopyBatchMax caps one owner copy job: a pilot whose calling is elsewhere spends a few lab hours, not
// a researcher's week.
const ownerCopyBatchMax = 5

// ownerCopyGroupIDs is the hull groups whose originals an owner copies: Cruiser (26), Battlecruiser (419)
// and Battleship (27). Frigates and destroyers already have copy supply from the researchers.
var ownerCopyGroupIDs = map[int64]bool{26: true, 419: true, 27: true}

func ownerCopyOn(ctx context.Context, m *Mod) bool {
	return escrowSettingInt(ctx, m.db, ownerCopyKey, 1) != 0
}

// isOwnerCopyHull reports whether a print's product is a hull the owner lane copies. Pure.
func isOwnerCopyHull(categoryID, groupID int64) bool {
	return categoryID == catShip && ownerCopyGroupIDs[groupID]
}

// ownerCopyPrint is a hull original in the hangar the owner lane could copy.
type ownerCopyPrint struct {
	ItemID, BPTypeID int64
	CategoryID       int64
	GroupID          int64
	CopyTime         int64 // invBlueprintTypes.researchCopyTime, for the fee quote
}

// pickOwnerCopy chooses which original to copy and how many copies: the first hull print, in the given
// order, not in use, not already on the copy line, and short of stock. Pure.
func pickOwnerCopy(prints []ownerCopyPrint, inUse, cooking map[int64]bool, stock, sold map[int64]int) (ownerCopyPrint, int, bool) {
	for _, p := range prints {
		if !isOwnerCopyHull(p.CategoryID, p.GroupID) || inUse[p.ItemID] || cooking[p.ItemID] {
			continue
		}
		n := copyBatch(copyStockTargetFor(sold[p.BPTypeID]), stock[p.BPTypeID])
		if n <= 0 {
			continue
		}
		return p, min(n, ownerCopyBatchMax), true
	}
	return ownerCopyPrint{}, 0, false
}

// ownerHullCopy puts one hull original this pilot holds on a copy line. Docked at a station that holds the print
// and has a copy line, it installs on the spot. Otherwise it takes a print from its factory hangar, where the stray
// sweep keeps its prints, to the nearest copy line in reach (labStationFor) on the lab errand's own legs: fetch,
// carry, install (labworkStep). The job then goes on the cooking list; when it rings, the print is a stray at the lab,
// the sweep sends the pilot back for it, and the collect leg lists the copies there (ownerListAtLab). Reports
// whether it routed the pilot (true means the caller must stop and let the errand run). A research career is left to
// its own lane (labPickPrint), which copies hulls too.
func (m *Mod) ownerHullCopy(ctx context.Context, b *botRun, factoryID, factorySys int64) bool {
	if b.State == nil || b.StationID == 0 || researchCareer(b.Tune) || !ownerCopyOn(ctx, m) ||
		!escrowSellingEnabled(ctx, m.db) {
		return false
	}
	d := &b.State.Data
	if d.LabFailAt > clock.Unix() {
		return false
	}
	if free, ferr := m.labFreeSlots(ctx, b); ferr != nil || free < 1 {
		return false
	}
	sold, _ := m.copyDemand(ctx)
	inUse := map[int64]bool{d.IndBPItemID: true, d.IndFetchItemID: true, d.LabBPItemID: true}
	cooking := map[int64]bool{}
	for _, j := range d.LabCooking {
		cooking[j.BPItemID] = true
	}

	// Here and now: a print where it stands, and a copy line under it.
	if here := m.ownerCopyPrintsAt(ctx, b.CharID, b.StationID); len(here) > 0 &&
		m.labStationHasLine(ctx, b.StationID, ramActivityCopying) {
		if stock, open, serr := m.copyStockFor(ctx, b, b.StationID); serr == nil && open < copyListingCap {
			if pick, copies, ok := pickOwnerCopy(here, inUse, cooking, stock, sold); ok {
				m.ownerInstallHere(ctx, b, pick, copies, factoryID, factorySys)
				return false
			}
		}
	}

	// Its factory's prints, carried to the nearest copy line.
	if factoryID == 0 {
		return false
	}
	prints := m.ownerCopyPrintsAt(ctx, b.CharID, factoryID)
	if len(prints) == 0 {
		return false
	}
	labID, labSys, line, err := m.labStationFor(ctx, b, factoryID, factorySys, ramActivityCopying)
	if err != nil || labID == 0 || line == nil {
		d.LabFailAt = clock.Unix() + labFailCooldownSec
		_ = saveState(ctx, m.db, b.State)
		return false
	}
	stock, open, serr := m.copyStockFor(ctx, b, labID)
	if serr != nil || open >= copyListingCap {
		return false
	}
	pick, copies, ok := pickOwnerCopy(prints, inUse, cooking, stock, sold)
	if !ok {
		return false
	}
	f, ferr := m.printFactsFor(ctx, pick.BPTypeID)
	if ferr != nil || f.CopyRuns < 1 {
		return false
	}
	skills, _ := m.charSkillsFor(ctx, b.CharID)
	cand := &labCandidate{
		ItemID: pick.ItemID, TypeID: pick.BPTypeID, Name: f.Name, ActivityID: ramActivityCopying,
		BaseTime: pick.CopyTime, Skill: skills[skillScience],
		Copies: copies, CopyRuns: f.CopyRuns, MaxRuns: f.MaxRuns,
	}
	fee, durationSec := m.labFeeQuote(ctx, b, line, cand)
	needsTrip := labID != factoryID || b.StationID != factoryID
	balance, berr := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if berr != nil || !labAffordable(balance, fee, needsTrip) {
		return false
	}

	d.LabBPItemID, d.LabBPTypeID, d.LabBPName = cand.ItemID, cand.TypeID, cand.Name
	d.LabActivityID = ramActivityCopying
	d.LabCopies, d.LabCopyRuns = copies, f.CopyRuns
	d.LabLevels = 0
	d.LabStationID, d.LabStationSysID = labID, labSys
	d.LabFactoryID, d.LabFactorySysID = factoryID, factorySys
	b.State.Activity = ActIndustry
	m.logBot(b.CharID, logInfo, "lab",
		"copying its %s for the escrow board — taking it to %s (%d copies of %d run(s), about %s, about %s in fees) "+
			"so builders short of an original can build from them",
		f.Name, m.stationLabel(ctx, labID), copies, f.CopyRuns, fmtDuration(int64(durationSec)), iskString(fee))
	if b.StationID != factoryID {
		if err := m.beginTravel(ctx, b, factorySys, factoryID, string(TaskLabFetch)); err != nil {
			m.logBot(b.CharID, logError, "lab", "setting out for its %s: %v", f.Name, err)
		}
		return true
	}
	if err := m.labDepartForLab(ctx, b); err != nil {
		m.logBot(b.CharID, logError, "lab", "setting out with its %s: %v", f.Name, err)
	}
	return true
}

// ownerInstallHere installs an owner copy job on a print in the hangar where the pilot stands.
func (m *Mod) ownerInstallHere(ctx context.Context, b *botRun, pick ownerCopyPrint, copies int, factoryID, factorySys int64) {
	f, ferr := m.printFactsFor(ctx, pick.BPTypeID)
	if ferr != nil || f.CopyRuns < 1 {
		return
	}
	skills, _ := m.charSkillsFor(ctx, b.CharID)
	fee := m.copyFeePerCopy(ctx, b, pick.BPTypeID, f.CopyRuns, skills[skillScience]) * float64(copies)
	balance, berr := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if berr != nil || !labAffordable(balance, fee, false) {
		return
	}
	jobID, installedBP, fees, endTime, err := m.installCopyJob(ctx, b, pick.ItemID, pick.BPTypeID, copies, f.CopyRuns)
	if err != nil {
		m.logBot(b.CharID, logDebug, "lab", "could not copy its %s: %v", f.Name, err)
		return
	}
	d := &b.State.Data
	d.LabCooking = append(d.LabCooking, LabCookingJob{
		JobID:        jobID,
		BPItemID:     installedBP,
		BPTypeID:     pick.BPTypeID,
		BPName:       f.Name,
		ActivityID:   ramActivityCopying,
		StationID:    b.StationID,
		StationSysID: b.SystemID,
		FactoryID:    factoryID,
		FactorySysID: factorySys,
		EndsAt:       endTime,
		StartedAt:    clock.Unix(),
		Copies:       copies,
		CopyRuns:     f.CopyRuns,
	})
	_ = saveState(ctx, m.db, b.State)
	m.logBot(b.CharID, logInfo, "lab",
		"copying its %s: %d copies of %d run(s) for the escrow board (job %d, %.0f ISK) — builders short of an original can build from them",
		f.Name, copies, f.CopyRuns, jobID, fees)
}

// ownerCopyPrintsAt is the hull originals (ownerCopyGroupIDs) this pilot holds in a station hangar, cheapest
// hull first.
func (m *Mod) ownerCopyPrintsAt(ctx context.Context, charID, stationID int64) []ownerCopyPrint {
	if stationID == 0 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, g.categoryID, g.groupID, COALESCE(bt.researchCopyTime, 0)
		  FROM entity e
		  JOIN invBlueprints bp ON bp.blueprintID = e.itemID AND bp.copy = 0
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  JOIN invTypes p ON p.typeID = bt.productTypeID
		  JOIN invGroups g ON g.groupID = p.groupID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4
		   AND g.categoryID = ? AND g.groupID IN (26, 419, 27)
		 ORDER BY p.basePrice`, charID, stationID, catShip)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ownerCopyPrint
	for rows.Next() {
		var p ownerCopyPrint
		if rows.Scan(&p.ItemID, &p.BPTypeID, &p.CategoryID, &p.GroupID, &p.CopyTime) == nil && p.CopyTime > 0 {
			out = append(out, p)
		}
	}
	return out
}

// ownerListAtLab lists the hull copies of its own make a non-career pilot finds where it stands. The collect leg
// calls it at the lab, where the copies came off the line and where nothing else would ever list them.
func (m *Mod) ownerListAtLab(ctx context.Context, b *botRun) {
	if b == nil || researchCareer(b.Tune) || !ownerCopyOn(ctx, m) {
		return
	}
	m.listLibraryProducts(ctx, b)
}

// ownerCopyOriginals is the blueprint types of the hull originals (ownerCopyGroupIDs) this pilot holds anywhere,
// cargo and lab lines included: the types whose copies are its own make. The copies come off the line at a lab
// that is often not where the original lives.
func (m *Mod) ownerCopyOriginals(ctx context.Context, charID int64) map[int64]bool {
	out := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.typeID
		  FROM entity e
		  JOIN invBlueprints bp ON bp.blueprintID = e.itemID AND bp.copy = 0
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  JOIN invTypes p ON p.typeID = bt.productTypeID
		  JOIN invGroups g ON g.groupID = p.groupID
		 WHERE e.ownerID = ?
		   AND g.categoryID = ? AND g.groupID IN (26, 419, 27)`, charID, catShip)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t int64
		if rows.Scan(&t) == nil {
			out[t] = true
		}
	}
	return out
}

// ownerCopyKeepsType reports whether the pilot's own build needs copies of this blueprint type right now:
// the project in hand or the batch it is fanning over its lines (copylanes.go). Those copies are working
// prints, not stock. Pure over the state.
func ownerCopyKeepsType(b *botRun, bpTypeID int64) bool {
	if b == nil || b.State == nil {
		return false
	}
	d := b.State.Data
	return d.IndBPTypeID == bpTypeID || (d.IndSplitBPTypeID == bpTypeID && d.IndSplitRunsLeft > 0)
}
