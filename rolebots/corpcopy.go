package rolebots

// The corporation's R&D arm copies the library. A print can be on one line at a
// time and the originals are locked in the office (corpvault.go), so copies
// (ramActivities 5, bpcopy.go) are what builders work from. The library plan
// (corplibplan.go) keeps a shelf of copies ahead of the build orders and posts
// copy orders; corpWorkFor runs a build on a free copy, on an original only when
// no copy is free or coming. A house that leads with the research tag also
// copies for sale: markPrintsForSale puts copies the board does not need on the
// escrow board (corpprintsale.go).

import (
	"context"
	"fmt"
	"math"
	"sort"
)

const (
	// corpCopyWageFrac / corpCopyWageMin: a copy is paid a sliver of what it is
	// worth, since the job takes the R&D member's slot for hours.
	corpCopyWageFrac = 0.02
	corpCopyWageMin  = 2000
)

// copyAlloc is one copy order to post: n copies off one original.
type copyAlloc struct {
	Print libraryPrint
	N     int
}

// allocateCopyOrders spreads need copies of one type over its originals, one
// order per print and at most slots orders. A print with a research or copy
// order pinned to it (busy) is skipped; a free print beats one in a job, then one
// researched to meTarget, then higher ME. Pure.
func allocateCopyOrders(prints []libraryPrint, need int, busy map[int64]bool, slots int, meTarget int64) []copyAlloc {
	if need <= 0 || slots <= 0 {
		return nil
	}
	var cands []libraryPrint
	for _, p := range prints {
		if !busy[p.ItemID] {
			cands = append(cands, p)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if (a.Flag == 111) != (b.Flag == 111) {
			return a.Flag != 111
		}
		if (a.ME >= meTarget) != (b.ME >= meTarget) {
			return a.ME >= meTarget
		}
		return a.ME > b.ME
	})
	k := min(len(cands), slots, need)
	if k == 0 {
		return nil
	}
	out := make([]copyAlloc, k)
	for i := range out {
		out[i] = copyAlloc{Print: cands[i], N: need / k}
		if i < need%k {
			out[i].N++
		}
	}
	return out
}

// postCopyWork posts one copy order pinned to one original.
func (m *Mod) postCopyWork(ctx context.Context, c *corpCEO, stationID int64, p libraryPrint, n int, purpose string, priority int) error {
	f, err := m.printFactsFor(ctx, p.BPTypeID)
	if err != nil {
		return err
	}
	ref := m.printRefPrice(ctx, nil, p.BPTypeID)
	share := 1.0
	if f.MaxRuns > 0 {
		share = float64(f.CopyRuns) / float64(f.MaxRuns)
	}
	wage := math.Max(corpCopyWageMin, math.Floor(corpCopyWageFrac*ref*share))
	// Research copies for two customers (corprequest.go): Sales, when the copies
	// are for the shelf; Industry, the builders, otherwise.
	forArm := armIndustry
	if purpose == "for sale" {
		forArm = armSales
	}
	if _, err := m.postWork(ctx, corpWork{CorpID: c.CorpID, Kind: workKindCopy, BPTypeID: p.BPTypeID,
		ProductTypeID: p.BPTypeID, StationID: stationID, RunsWanted: n, WagePerRun: wage,
		Purpose: "research", Priority: priority, BPItemID: p.ItemID,
		Note:         fmt.Sprintf("%d-run copies %s", f.CopyRuns, purpose),
		RequesterArm: string(forArm)}); err != nil {
		return err
	}
	m.ceoLog(ctx, c, powerProduction, "post", p.BPTypeID, float64(n)*wage,
		"posted %d copies of its %s (%d runs each, %s)", n, m.typeNameOr(ctx, p.BPTypeID), f.CopyRuns, purpose)
	return nil
}

// corpCopyPrice is what the corporation's broker asks for one of its copies.
func (m *Mod) corpCopyPrice(ctx context.Context, b *botRun, bpTypeID, runs, me int64) float64 {
	f, err := m.printFactsFor(ctx, bpTypeID)
	if err != nil {
		return 0
	}
	skills, _ := m.charSkillsFor(ctx, b.CharID)
	ref := m.printRefPrice(ctx, b, bpTypeID)
	saving := printSavingPerRun(ctx, m.db, bpTypeID, me, skills[skillProductionEff])
	cost := m.copyFeePerCopy(ctx, b, bpTypeID, runs, skills[skillScience])
	return bpcPrice(ref, runs, f.MaxRuns, saving, cost, m.bpcPriceFrac(ctx))
}
