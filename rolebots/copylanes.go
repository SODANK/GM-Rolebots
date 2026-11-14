package rolebots

// Copy lanes: why a builder holds copies at all.
//
// A blueprint is one line. A pilot with five manufacturing slots and one print builds on one
// line; five copies build on five lines at once. The corporation library copies its originals so
// builders can fan an order out (corpjobs.go), and an independent copies its own original or buys
// copies on escrow for the same reason (industry.go). Two pure helpers decide the shape:
// splitChunk (runs one job takes) and selfCopyPlan (whether copying first finishes sooner).

import (
	"context"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

const (
	// selfCopyGain: copying first must finish the batch in at most this fraction of the
	// single-line time, or the original just builds. The copy job's fee, lab slot and extra hangar
	// trip are not free.
	selfCopyGain = 0.75
	// selfCopyMinRuns: below this a batch is not worth splitting.
	selfCopyMinRuns = 2
)

// splitChunk is the runs one job takes out of runsFree when they can spread over `prints` free
// prints and `freeSlots` free lines: the lanes are the smallest of the three and each takes an
// even share, rounded up. Recomputed every pass it stays stable (50 runs over 5 and 5 is 10,
// then 40/4/4 is 10). Never below 1. Pure.
func splitChunk(runsFree, prints, freeSlots int) int {
	if runsFree <= 1 {
		return max(runsFree, 1)
	}
	lanes := min(max(prints, 1), max(freeSlots, 1), runsFree)
	return (runsFree + lanes - 1) / lanes
}

// selfCopyPlan decides whether a pilot with one original and freeMfg free lines should copy it
// first: k copies of ceil(runs/k) runs, then the batch on k lines. runSec is one run's time;
// copySec(k, runsPer) is the copy job's time. It picks the k (2..freeMfg, at most the runs)
// that finishes soonest and answers ok only if that beats one line by selfCopyGain. Pure.
func selfCopyPlan(runs int, runSec float64, freeMfg int, copySec func(k, runsPer int) float64) (k, runsPer int, ok bool) {
	if runs < selfCopyMinRuns || runSec <= 0 || freeMfg < 2 || copySec == nil {
		return 0, 0, false
	}
	single := float64(runs) * runSec
	best := single
	for n := 2; n <= min(freeMfg, runs); n++ {
		per := (runs + n - 1) / n
		c := copySec(n, per)
		if c <= 0 {
			continue
		}
		if t := c + float64(per)*runSec; t < best {
			best, k, runsPer = t, n, per
		}
	}
	if k == 0 || best > single*selfCopyGain {
		return 0, 0, false
	}
	return k, runsPer, true
}

const (
	// industrySplitTTLSec: a split older than this is forgotten, since the want that sized it has
	// moved on.
	industrySplitTTLSec = 3 * 86400
	// industryExtraCopyBudgetFrac is the share of the project's spendable cushion (after the
	// primary print) the extra copies on one trip may take.
	industryExtraCopyBudgetFrac = 0.5
	// industryExtraCopyPriceMul: an extra copy costs at most this many times the primary per run.
	industryExtraCopyPriceMul = 1.5
)

// industrySplitPin is the candidate a pending split pins to the head of the draw, 0 for none.
// A split past its TTL, or whose type has left the book, is cleared here.
func industrySplitPin(d *StateData, cands []indCandidate) int64 {
	if d.IndSplitBPTypeID == 0 {
		return 0
	}
	if d.IndSplitRunsLeft <= 0 || clock.Unix()-d.IndSplitAt > industrySplitTTLSec {
		industryClearSplit(d)
		return 0
	}
	for _, c := range cands {
		if c.BPTypeID == d.IndSplitBPTypeID {
			return c.TypeID
		}
	}
	industryClearSplit(d)
	return 0
}

// industryClearSplit forgets the pending split.
func industryClearSplit(d *StateData) {
	d.IndSplitBPTypeID, d.IndSplitRunsLeft, d.IndSplitAt = 0, 0, 0
}

// industryNoteSplit records what a committed project left of its batch for the next lane: left
// runs of bpTypeID, kept only while another line is free. A pending split of another type is
// not displaced, since its copies may still be on the copy line.
func industryNoteSplit(d *StateData, bpTypeID int64, left, freeLines int) {
	if left > 0 && freeLines > 1 && (d.IndSplitBPTypeID == 0 || d.IndSplitBPTypeID == bpTypeID) {
		if d.IndSplitBPTypeID != bpTypeID {
			d.IndSplitAt = clock.Unix()
		}
		d.IndSplitBPTypeID, d.IndSplitRunsLeft = bpTypeID, left
		return
	}
	if d.IndSplitBPTypeID == bpTypeID {
		industryClearSplit(d)
	}
}

// usablePrintsAt counts the pilot's prints of a type in a station hangar that can still build:
// originals, and copies with runs left.
func (m *Mod) usablePrintsAt(ctx context.Context, charID, stationID, bpTypeID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity e
		  LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.typeID = ? AND e.flag = 4
		   AND (COALESCE(bp.copy, 0) = 0 OR bp.licensedProductionRunsRemaining > 0)`,
		charID, stationID, bpTypeID).Scan(&n)
	return n
}

// industrySelfCopy puts a pilot's only print of a type (an original, at the factory it stands
// in) on the copy line when copying first finishes the batch sooner (selfCopyPlan). The copies
// keep the original's levels and the split pins the type so later passes put them on lines. It
// reports a log note and whether the copy job went in.
func (m *Mod) industrySelfCopy(ctx context.Context, b *botRun, factoryID, factorySys int64, cand indCandidate,
	bpType *evedb.BlueprintTypeInfo, ownedBP int64, total int, pe int64, industrySkill, science int,
	line *manufactureLine, freeMfg int) (string, bool) {
	d := &b.State.Data
	if freeMfg < 2 || total < selfCopyMinRuns || bpType == nil || line == nil {
		return "", false
	}
	if d.IndSplitBPTypeID != 0 && d.IndSplitBPTypeID != cand.BPTypeID {
		return "", false // one fan-out at a time
	}
	if _, isCopy := m.printRunsLeft(ctx, ownedBP); isCopy {
		return "", false
	}
	if free, err := m.labFreeSlots(ctx, b); err != nil || free < 1 {
		return "", false
	}
	if !m.labStationHasLine(ctx, factoryID, ramActivityCopying) {
		return "", false
	}
	copyLine, err := m.assemblyLineAt(ctx, b, factoryID, ramActivityCopying, cand.BPTypeID)
	if err != nil {
		return "", false
	}
	runSec := manufactureDurationSec(bpType, pe, industrySkill, 1, line)
	k, per, ok := selfCopyPlan(total, runSec, freeMfg, func(n, runsPer int) float64 {
		return copyDurationSec(bpType, n, int64(runsPer), science, copyLine)
	})
	if !ok {
		return "", false
	}
	jobID, installedBP, fees, endTime, err := m.installCopyJob(ctx, b, ownedBP, cand.BPTypeID, k, int64(per))
	if err != nil {
		m.logBot(b.CharID, logInfo, "industry",
			"could not copy its %s print for %d lines: %v — building on the original", cand.TypeName, k, err)
		return "", false
	}
	d.LabCooking = append(d.LabCooking, LabCookingJob{
		JobID: jobID, BPItemID: installedBP, BPTypeID: cand.BPTypeID,
		BPName: m.typeNameOr(ctx, cand.BPTypeID), ActivityID: ramActivityCopying,
		StationID: factoryID, StationSysID: factorySys, FactoryID: factoryID, FactorySysID: factorySys,
		EndsAt: endTime, StartedAt: clock.Unix(), Copies: k, CopyRuns: int64(per),
	})
	d.IndSplitBPTypeID, d.IndSplitRunsLeft, d.IndSplitAt = cand.BPTypeID, total, clock.Unix()
	if err := saveState(ctx, m.db, b.State); err != nil {
		rlogf("rolebots: self-copy save for %d: %v", b.CharID, err)
	}
	note := fmt.Sprintf("copying its print: %d copies of %d run(s) for %d lines at once (job %d, %s)",
		k, per, k, jobID, iskString(fees))
	m.logBot(b.CharID, logInfo, "industry", "%s: %s", cand.TypeName, note)
	m.chatterNoteBot(ctx, b.CharID, "lab.install", map[string]string{
		"what": cand.TypeName, "activity": "copying"}, jobID)
	return note, true
}

// extraCopyListings are further copies of a type on escrow at the station the primary print is
// bought from: cheapest per run first, at most want, within budget, none dearer per run than
// industryExtraCopyPriceMul times the primary, never the buyer's own listing.
func (m *Mod) extraCopyListings(ctx context.Context, b *botRun, bpTypeID, stationID, primary int64,
	want int, budget float64) []escrowBPO {
	if want <= 0 || budget <= 0 || !escrowBuyingEnabled(ctx, m.db) {
		return nil
	}
	all, err := m.escrowBPOListings(ctx)
	if err != nil {
		return nil
	}
	var maxPerRun float64
	var pool []escrowBPO
	for _, l := range all {
		if l.EscrowID == primary && l.Runs > 0 {
			maxPerRun = l.Price / float64(l.Runs) * industryExtraCopyPriceMul
		}
		if l.TypeID != bpTypeID || l.StationID != stationID || !l.Copy || l.Runs <= 0 ||
			l.EscrowID == primary || l.IssuerID == b.CharID || l.Price <= 0 {
			continue
		}
		pool = append(pool, l)
	}
	return pickExtraCopies(pool, want, budget, maxPerRun)
}

// pickExtraCopies takes the cheapest-per-run copies out of a pool while the budget lasts;
// maxPerRun 0 is no ceiling. Pure.
func pickExtraCopies(pool []escrowBPO, want int, budget, maxPerRun float64) []escrowBPO {
	sort.SliceStable(pool, func(i, j int) bool {
		return pool[i].Price/float64(pool[i].Runs) < pool[j].Price/float64(pool[j].Runs)
	})
	var out []escrowBPO
	for _, l := range pool {
		if len(out) >= want {
			break
		}
		if maxPerRun > 0 && l.Price/float64(l.Runs) > maxPerRun {
			break
		}
		if l.Price > budget {
			continue
		}
		budget -= l.Price
		out = append(out, l)
	}
	return out
}

// claimExtraCopies buys the extra copies planned for this trip, at the station the pilot is
// standing in, into the hold. A lost or unaffordable one is skipped; the next pass looks again.
func (m *Mod) claimExtraCopies(ctx context.Context, b *botRun) {
	d := &b.State.Data
	ids := d.IndExtraEscrowIDs
	d.IndExtraEscrowIDs = nil
	if len(ids) == 0 || b.StationID == 0 {
		return
	}
	corpID, _ := evedb.GetCharCorpID(ctx, m.db, b.CharID)
	got := 0
	for _, escrowID := range ids {
		var price float64
		if m.db.QueryRowContext(ctx, `SELECT collateral FROM escrows WHERE escrowID = ? AND state = 0`,
			escrowID).Scan(&price) != nil {
			continue
		}
		if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err != nil || spendable(bal) < price {
			break
		}
		moved, err := evedb.ClaimEscrowMission(ctx, m.db, escrowID, b.CharID, corpID, b.StationID, true)
		if err != nil {
			continue
		}
		for _, id := range moved {
			var t int64
			if m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, id).Scan(&t) != nil ||
				t != d.IndBPTypeID {
				continue
			}
			if m.moveItemToCargo(ctx, b, id) == nil {
				d.IndCarryPrints = append(d.IndCarryPrints, id)
				got++
			}
			break
		}
	}
	if got > 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"bought %d more %s blueprint copies on the same counter, one line each at the factory", got, d.IndTypeName)
	}
}

// unloadCarryPrints puts the extra copies in the hold into the hangar of the station the pilot
// is docked at. One that will not move and is no longer in this ship is forgotten; one still
// aboard is kept for the next dock.
func (m *Mod) unloadCarryPrints(ctx context.Context, b *botRun) {
	d := &b.State.Data
	if len(d.IndCarryPrints) == 0 || b.StationID == 0 {
		return
	}
	var keep []int64
	for _, id := range d.IndCarryPrints {
		if m.moveCargoItemToHangar(ctx, b, id) == nil {
			continue
		}
		var loc int64
		if m.db.QueryRowContext(ctx, `SELECT locationID FROM entity WHERE itemID = ? AND ownerID = ?`,
			id, b.CharID).Scan(&loc) == nil && loc == b.ShipID {
			keep = append(keep, id)
		}
	}
	d.IndCarryPrints = keep
}
