//go:build scc

package rolebots

// The SCC Exchange's sector-level feedback and the treasury's band clamp:
// pure functions over the board, shared by the live code (sccbots.go) and
// the simulation harness (sccsim_run_test.go). Two channels: each sector's
// cap-weighted price/fair against the board's, as the view's SectorRel
// (sccSectorAggregates, read by sccSentiment), and one review candidate per
// pilot drawn from the cheapest sector (sccCandidates). Fair value is the
// tape's consensus, SCCFairValue over 16 weeks. Why climate alone was not
// enough, and the live numbers: docs/CORPS.md, "Sector valuation".

import (
	"math"
	"sort"

	evedb "gurimoon/db"
)

// sccConsensusWeeks is the horizon every sector sum values a listing at.
const sccConsensusWeeks = 16

// sccSectorInput is the slice of a board row the sector sums need. The live
// code builds it from an SCCBoardRow (sccSectorInputOf), the harness from
// its simCorp.
type sccSectorInput struct {
	CorpID      int64
	Sector      string
	Price       float64 // last trade, else the reference
	Outstanding int64
	Fair        float64 // SCCFairValue(BookPS, EPSWeek, 16, Risk); 0 = no filing
	Skip        bool    // halted: counted in the sums, never a rotation pick
}

// sccSectorAgg is one sector's (or the whole board's) price level.
type sccSectorAgg struct {
	Members int     // rows with a filing, a price, a fair value and shares out
	Cap     float64 // Σ price × outstanding
	FairCap float64 // Σ fair × outstanding
	PF      float64 // Cap / FairCap; 0 when FairCap is 0
	NEff    float64 // 1 / Σ (cap_i / Cap)²: how many names the sector really has
	Rel     float64 // damped clamp(±1, ln(PF / board PF)); 0 when undefined
}

// sccSectorAggs is the board's price level by sector.
type sccSectorAggs struct {
	Composite sccSectorAgg
	BySector  map[string]sccSectorAgg // every evedb.SCCSectors key, zero when empty
}

// sccSectorDamp is how much of a sector's rel counts, by how many names it
// really has: (n_eff - 1) / 2 clamped to 0..1. One listing has no distinct
// opinion, two equal names count half, three in full, and a sector where one
// giant holds 90% of the cap cannot mark itself down through the sector. Pure.
func sccSectorDamp(nEff float64) float64 {
	return math.Max(0, math.Min(1, (nEff-1)/2))
}

// sccSectorAggregates sums the board by sector and sets each sector's Rel
// against the whole board. Pure.
func sccSectorAggregates(rows []sccSectorInput) sccSectorAggs {
	out := sccSectorAggs{BySector: map[string]sccSectorAgg{}}
	for _, s := range evedb.SCCSectors {
		out.BySector[s] = sccSectorAgg{}
	}
	caps := map[string][]float64{}
	for _, r := range rows {
		if r.Fair <= 0 || r.Price <= 0 || r.Outstanding <= 0 {
			continue
		}
		n := float64(r.Outstanding)
		mcap, fair := r.Price*n, r.Fair*n
		a := out.BySector[r.Sector]
		a.Members++
		a.Cap += mcap
		a.FairCap += fair
		out.BySector[r.Sector] = a
		caps[r.Sector] = append(caps[r.Sector], mcap)
		out.Composite.Members++
		out.Composite.Cap += mcap
		out.Composite.FairCap += fair
	}
	finish := func(a *sccSectorAgg, caps []float64) {
		if a.FairCap > 0 {
			a.PF = a.Cap / a.FairCap
		}
		if a.Cap > 0 {
			var hh float64
			for _, c := range caps {
				w := c / a.Cap
				hh += w * w
			}
			if hh > 0 {
				a.NEff = 1 / hh
			}
		}
	}
	var all []float64
	for _, c := range caps {
		all = append(all, c...)
	}
	finish(&out.Composite, all)
	for s, a := range out.BySector {
		finish(&a, caps[s])
		if a.PF > 0 && out.Composite.PF > 0 {
			rel := math.Max(-1, math.Min(1, math.Log(a.PF/out.Composite.PF)))
			a.Rel = sccSectorDamp(a.NEff) * rel
		}
		out.BySector[s] = a
	}
	return out
}

// RelOf is a sector's Rel; 0 for a sector the board does not know.
func (a sccSectorAggs) RelOf(sector string) float64 {
	return a.BySector[sector].Rel
}

// Cheapest is the sector with the lowest price/fair among those with at
// least two priced names (one name is a stock, not a sector). Ties go to
// the first in evedb.SCCSectors, so the pick is deterministic. Pure.
func (a sccSectorAggs) Cheapest() (string, bool) {
	best, ok := "", false
	for _, s := range evedb.SCCSectors {
		g := a.BySector[s]
		if g.Members < 2 || g.PF <= 0 {
			continue
		}
		if !ok || g.PF < a.BySector[best].PF {
			best, ok = s, true
		}
	}
	return best, ok
}

// sccRotationTop is how many of the cheapest sector's names a rotation pick
// is drawn from: one would herd every pilot into one ticker.
const sccRotationTop = 3

// sccRotationPool is the cheapest sector's best upsides: its priced,
// unhalted names by fair/price, best first, at most sccRotationTop of them.
// Pure.
func sccRotationPool(rows []sccSectorInput, aggs sccSectorAggs) []sccSectorInput {
	sector, ok := aggs.Cheapest()
	if !ok {
		return nil
	}
	var pool []sccSectorInput
	for _, r := range rows {
		if r.Sector == sector && !r.Skip && r.Fair > 0 && r.Price > 0 {
			pool = append(pool, r)
		}
	}
	sort.Slice(pool, func(i, j int) bool {
		ui, uj := pool[i].Fair/pool[i].Price, pool[j].Fair/pool[j].Price
		if ui != uj {
			return ui > uj
		}
		return pool[i].CorpID < pool[j].CorpID
	})
	if len(pool) > sccRotationTop {
		pool = pool[:sccRotationTop]
	}
	return pool
}

// sccPickRow is one row of list by a 0..1 roll. Pure.
func sccPickRow(list []sccSectorInput, roll float64) int64 {
	i := int(roll * float64(len(list)))
	if i < 0 {
		i = 0
	}
	return list[i%len(list)].CorpID
}

// sccRotationPick is the rotation candidate for one review: one of the
// cheapest sector's best upsides, by roll. ok is false on a board with no
// cheapest sector. Pure.
func sccRotationPick(rows []sccSectorInput, aggs sccSectorAggs, roll float64) (int64, bool) {
	pool := sccRotationPool(rows, aggs)
	if len(pool) == 0 {
		return 0, false
	}
	return sccPickRow(pool, roll), true
}

// sccCandidates is a review's list, in work order: the pilot's own corporation,
// every holding (ascending corpID), the rotation pick (by roll[0]) and one random
// listing (roll[1]). Only ids on the board, none twice. With no cheapest sector,
// roll[0] draws a second random listing. Pure.
func sccCandidates(home int64, held []int64, rows []sccSectorInput, aggs sccSectorAggs, roll [2]float64) []int64 {
	if len(rows) == 0 {
		return nil
	}
	on := map[int64]bool{}
	for _, r := range rows {
		on[r.CorpID] = true
	}
	seen := map[int64]bool{}
	var out []int64
	add := func(id int64) {
		if on[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(home)
	held = append([]int64(nil), held...)
	sort.Slice(held, func(i, j int) bool { return held[i] < held[j] })
	for _, id := range held {
		add(id)
	}
	if id, ok := sccRotationPick(rows, aggs, roll[0]); ok {
		add(id)
	} else {
		add(sccPickRow(rows, roll[0]))
	}
	add(sccPickRow(rows, roll[1]))
	return out
}

// sccBoardStats is what one review or quote pass derives from the board,
// once: the median cap (the small-cap line) and the sector price levels.
type sccBoardStats struct {
	MedianCap float64
	Sectors   sccSectorAggs
	Inputs    []sccSectorInput
}

// sccSectorInputOf is a board row as the sector sums read it: no filing, no
// fair; a halted listing is skipped as a rotation pick. Pure.
func sccSectorInputOf(r evedb.SCCBoardRow, now int64) sccSectorInput {
	in := sccSectorInput{CorpID: r.CorpID, Sector: r.Sector, Price: r.Price, Skip: r.Halted(now)}
	if r.HasFiling {
		f := r.Filing
		in.Outstanding = f.Outstanding
		in.Fair = evedb.SCCFairValue(f.BookPS, f.EPSWeek, sccConsensusWeeks, f.Risk)
	}
	return in
}

// sccBoardStatsOf reads the board once for a pass. Pure.
func sccBoardStatsOf(rows []evedb.SCCBoardRow, now int64) sccBoardStats {
	st := sccBoardStats{Inputs: make([]sccSectorInput, 0, len(rows))}
	caps := make([]float64, 0, len(rows))
	for _, r := range rows {
		caps = append(caps, r.MarketCap)
		st.Inputs = append(st.Inputs, sccSectorInputOf(r, now))
	}
	sort.Float64s(caps)
	if len(caps) > 0 {
		st.MedianCap = caps[len(caps)/2]
	}
	st.Sectors = sccSectorAggregates(st.Inputs)
	return st
}

// ── the treasury's band clamp ───────────────────────────────────────────────

// sccTreasuryPrices clamps the day's two treasury prices into the band. The ask
// never falls under bookFloor (book x 1.05), so a ceiling the band cannot take
// withholds the offering; the bid is capped at the band top. Invariant: bidPx <=
// book(1-haircut) < book < bookFloor <= askPx, so the sides never cross. Pure.
func sccTreasuryPrices(ceiling, bookFloor, buybackPx float64, band evedb.SCCBand) (askPx float64, askOK bool, bidPx float64, bidOK bool) {
	askPx = math.Min(ceiling, band.Hi)
	askOK = askPx >= bookFloor && band.Contains(askPx)
	bidPx = math.Min(buybackPx, band.Hi)
	bidOK = bidPx >= band.Lo && band.Contains(bidPx)
	return
}
