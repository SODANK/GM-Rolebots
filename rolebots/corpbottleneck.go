package rolebots

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	evedb "gurimoon/db"
)

// A second original of a type, when one print is the bottleneck.
//
// Originals stay locked in the office (corpvault.go) and drive copying and research; builds run
// on copies. One original is on one line at a time, so a second doubles the capacity. The CEO
// buys one when all of these hold: the corporation holds one or two originals of the type; the
// build orders want more runs than free and ordered copies cover; its originals spent at least
// bottleneckBusyFrac of the last week on a line; and a copy order for the type has waited at
// least bottleneckCopyWaitSec. The pick ranks by unmet runs times the product's value.
// postLibraryPurchase buys it ahead of a new type, and a donation or Tech II buy-out of a type
// the library already holds is accepted for a bottleneck type.

const (
	bottleneckMaxOriginals = 3
	bottleneckBusyFrac     = 0.8
	bottleneckWindowSec    = 7 * 86400
	bottleneckCopyWaitSec  = 86400
)

// printLoad is the pressure on one type's originals.
type printLoad struct {
	BPTypeID           int64
	Originals          int
	BuildRunsLeft      int64
	FreeCopyRuns       int64
	IncomingCopyRuns   int64
	Utilization        float64 // share of the window the originals spent on a line
	OldestCopyOrderAge int64   // seconds; 0 with no copy order open
	ProductValue       float64
}

// unmetRuns is the build runs no copy on the shelf or on order covers.
func (l printLoad) unmetRuns() int64 {
	return l.BuildRunsLeft - l.FreeCopyRuns - l.IncomingCopyRuns
}

// isPrintBottleneck is the buy rule above. Pure.
func isPrintBottleneck(l printLoad) bool {
	return l.Originals >= 1 && l.Originals < bottleneckMaxOriginals &&
		l.unmetRuns() > 0 &&
		l.Utilization >= bottleneckBusyFrac &&
		l.OldestCopyOrderAge >= bottleneckCopyWaitSec
}

// rankBottlenecks keeps the bottlenecks, most unmet value first. Pure.
func rankBottlenecks(loads []printLoad) []printLoad {
	var out []printLoad
	for _, l := range loads {
		if isPrintBottleneck(l) {
			out = append(out, l)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return float64(out[i].unmetRuns())*out[i].ProductValue > float64(out[j].unmetRuns())*out[j].ProductValue
	})
	return out
}

// printBottlenecks is corpID to []printLoad, ranked, from the last production review.
var printBottlenecks sync.Map

func init() {
	registerCorpForget("printBottleneck", func(corpID int64) { printBottlenecks.Delete(corpID) })
}

func corpBottlenecks(corpID int64) []printLoad {
	if v, ok := printBottlenecks.Load(corpID); ok {
		return v.([]printLoad)
	}
	return nil
}

// isBottleneckType reports whether the last review found the type a bottleneck.
func isBottleneckType(corpID, bpTypeID int64) bool {
	for _, l := range corpBottlenecks(corpID) {
		if l.BPTypeID == bpTypeID {
			return true
		}
	}
	return false
}

// libraryWantsTypeSQL is a WHERE condition on an entity aliased e: the library lacks e's type,
// or the type is a bottleneck (a second original is welcome).
func libraryWantsTypeSQL(corpID int64) (string, []any) {
	cond := `(NOT EXISTS (SELECT 1 FROM rolebot_corp_library lw WHERE lw.corporationID = ? AND lw.bpTypeID = e.typeID)`
	args := []any{corpID}
	if bn := corpBottlenecks(corpID); len(bn) > 0 {
		cond += ` OR e.typeID IN (` + strings.TrimSuffix(strings.Repeat("?,", len(bn)), ",") + `)`
		for _, l := range bn {
			args = append(args, l.BPTypeID)
		}
	}
	return cond + `)`, args
}

// notePrintBottlenecks measures every type the board builds from and keeps the ranked
// bottlenecks for this corporation. Run from the production review after the copy orders are
// posted.
func (m *Mod) notePrintBottlenecks(ctx context.Context, c *corpCEO, stationID int64) {
	open, err := m.openWorkFor(ctx, c.CorpID)
	if err != nil {
		return
	}
	now := clock.Unix()
	loads := map[int64]*printLoad{}
	get := func(t int64) *printLoad {
		if l, ok := loads[t]; ok {
			return l
		}
		l := &printLoad{BPTypeID: t}
		loads[t] = l
		return l
	}
	copyOrders := map[int64][]corpWork{}
	for _, w := range open {
		switch w.Kind {
		case workKindBuild:
			l := get(w.BPTypeID)
			l.BuildRunsLeft += int64(w.RunsLeft())
			if l.ProductValue == 0 && w.ProductTypeID != 0 {
				l.ProductValue = evedb.TypeValueOf(ctx, m.db, w.ProductTypeID).Value
			}
		case workKindCopy:
			copyOrders[w.BPTypeID] = append(copyOrders[w.BPTypeID], w)
		}
	}
	if len(loads) == 0 {
		printBottlenecks.Delete(c.CorpID)
		return
	}
	prints, err := m.libraryPrints(ctx, c.CorpID, stationID)
	if err != nil {
		return
	}
	items := map[int64][]int64{}
	for _, p := range prints {
		if l, ok := loads[p.BPTypeID]; ok {
			l.Originals++
			items[p.BPTypeID] = append(items[p.BPTypeID], p.ItemID)
		}
	}
	copies, _ := m.libraryCopies(ctx, c.CorpID, stationID)
	for _, cp := range copies {
		if l, ok := loads[cp.BPTypeID]; ok && cp.Flag != 111 {
			l.FreeCopyRuns += cp.Runs
		}
	}
	for t, l := range loads {
		if l.Originals == 0 || l.Originals >= bottleneckMaxOriginals {
			continue
		}
		f, ferr := m.printFactsFor(ctx, t)
		if ferr != nil {
			continue
		}
		for _, w := range copyOrders[t] {
			l.IncomingCopyRuns += int64(w.RunsLeft()) * f.CopyRuns
			if age := now - w.PostedAt; age > l.OldestCopyOrderAge {
				l.OldestCopyOrderAge = age
			}
		}
		if l.unmetRuns() <= 0 || l.OldestCopyOrderAge < bottleneckCopyWaitSec {
			continue // no need to measure the lines
		}
		l.Utilization = m.printUtilization(ctx, items[t], now)
	}
	all := make([]printLoad, 0, len(loads))
	for _, l := range loads {
		all = append(all, *l)
	}
	if ranked := rankBottlenecks(all); len(ranked) > 0 {
		printBottlenecks.Store(c.CorpID, ranked)
	} else {
		printBottlenecks.Delete(c.CorpID)
	}
}

// printUtilization is the share of the last bottleneckWindowSec the prints spent on a line,
// over all of them (1 = every print busy all week). The statuses are listed, not written
// `<> 102`: the list is a range on idx_ramjobs_status_end, while `<>` reads the whole job
// history (installedItemID has no index).
func (m *Mod) printUtilization(ctx context.Context, itemIDs []int64, now int64) float64 {
	if len(itemIDs) == 0 {
		return 0
	}
	from := evedb.UnixToWin32(now - bottleneckWindowSec)
	to := evedb.UnixToWin32(now)
	args := []any{to, from, from, to}
	for _, id := range itemIDs {
		args = append(args, id)
	}
	var busy float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(GREATEST(0, LEAST(endProductionTime, ?) - GREATEST(beginProductionTime, ?))), 0)
		  FROM ramJobs
		 WHERE completedStatusID IN (0, 1, 101) AND endProductionTime > ? AND beginProductionTime < ?
		   AND installedItemID IN (`+strings.TrimSuffix(strings.Repeat("?,", len(itemIDs)), ",")+`)`,
		args...).Scan(&busy)
	window := float64(to-from) * float64(len(itemIDs))
	if window <= 0 {
		return 0
	}
	return busy / window
}

// buyBottleneckPrint posts a purchase order for a second original of the top bottleneck that
// fits the budget. It reports whether it posted one.
func (m *Mod) buyBottleneckPrint(ctx context.Context, c *corpCEO, deputyID, stationID int64, budget float64) bool {
	for _, l := range corpBottlenecks(c.CorpID) {
		value := evedb.TypeValueOf(ctx, m.db, l.BPTypeID).Value
		if value <= 0 || value*poAdvanceMarkup > budget {
			continue
		}
		m.ceoLog(ctx, c, powerProduction, "bottleneck", l.BPTypeID, value,
			"wants a second original of its %s — its %d print(s) were on a line %.0f%% of the week and %d build run(s) wait on copies",
			m.typeNameOr(ctx, l.BPTypeID), l.Originals, l.Utilization*100, l.unmetRuns())
		if err := m.postPO(ctx, c, deputyID, l.BPTypeID, stationID, value, false); err != nil {
			if !errors.Is(err, errPOOverCap) {
				m.logBot(c.CharID, logError, "ceo", "purchase order: %v", err)
			}
			return false
		}
		return true
	}
	return false
}
