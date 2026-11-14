package rolebots

import (
	"context"
	"math"

	evedb "gurimoon/db"
)

// Build bases: the production board spread over Industry's bases. A department
// that outgrows its lines takes another factory office (corpexpand.go), and
// work must be posted there:
//   - a market build order goes to one base (pickBuildBase): where a copy of its
//     print sits, else an original, else the least queued production per line;
//   - internal demand (SRP hulls, the store shelf) stays at the pinned factory;
//   - each base's Materials division feeds its orders, the buyback bids per base
//     (corpbuyback.go), and what it builds is listed from that station;
//   - copies move to the base that needs them (corpprintmove.go planCopyMoves)
//     and members are spread by queued production (corpposting.go postingQuotas).

// buildBase is one Industry base as the board's placement sees it.
type buildBase struct {
	StationID int64
	Factory   bool            // the pinned factory
	Lines     int             // manufacturing lines at the station
	LoadSec   float64         // production seconds of open build orders posted here
	Full      bool            // members turned away here for want of a line
	CopyRuns  map[int64]int64 // free copy runs by blueprint type
	Originals map[int64]bool  // free originals by blueprint type
}

// perLine is the base's queued production per manufacturing line. Pure.
func (b buildBase) perLine() float64 { return b.LoadSec / float64(max(b.Lines, 1)) }

// pickBuildBase is the index of the base a build order goes to, -1 for none.
// Internal demand goes to the pinned factory. Otherwise, among bases not full
// (all if every one is): most free copy runs of the print, else a free original,
// else least queued production per line; ties to the factory, then lowest station. Pure.
func pickBuildBase(bpTypeID int64, internal bool, bases []buildBase) int {
	if len(bases) == 0 {
		return -1
	}
	factory := -1
	for i, b := range bases {
		if b.Factory {
			factory = i
			break
		}
	}
	if internal || len(bases) == 1 {
		if factory >= 0 {
			return factory
		}
		return 0
	}
	var open []int
	for i, b := range bases {
		if !b.Full {
			open = append(open, i)
		}
	}
	if len(open) == 0 {
		for i := range bases {
			open = append(open, i)
		}
	}
	less := func(i, j int) bool { // tie-break: factory first, then station
		if bases[i].Factory != bases[j].Factory {
			return bases[i].Factory
		}
		return bases[i].StationID < bases[j].StationID
	}
	best := -1
	for _, i := range open {
		r := bases[i].CopyRuns[bpTypeID]
		if r <= 0 {
			continue
		}
		if best < 0 || r > bases[best].CopyRuns[bpTypeID] ||
			(r == bases[best].CopyRuns[bpTypeID] && less(i, best)) {
			best = i
		}
	}
	if best >= 0 {
		return best
	}
	for _, i := range open {
		if bases[i].Originals[bpTypeID] && (best < 0 || less(i, best)) {
			best = i
		}
	}
	if best >= 0 {
		return best
	}
	for _, i := range open {
		if best < 0 {
			best = i
			continue
		}
		pi, pb := bases[i].perLine(), bases[best].perLine()
		if pi < pb || (pi == pb && less(i, best)) {
			best = i
		}
	}
	return best
}

// buildBaseWeights is what Industry's postings are spread by: hours of
// production queued at each base, at least one so an idle base still draws a
// hand. Pure.
func buildBaseWeights(bases []buildBase) map[int64]int {
	out := make(map[int64]int, len(bases))
	for _, b := range bases {
		out[b.StationID] = max(1, int(math.Ceil(b.LoadSec/3600)))
	}
	return out
}

// industryBaseStations is Industry's bases, the pinned factory first even
// where its stored purposes have not caught up with a fresh pin.
func (m *Mod) industryBaseStations(ctx context.Context, corpID int64) []int64 {
	factory := m.corpFactoryStation(ctx, corpID)
	out := make([]int64, 0, 2)
	if factory != 0 {
		out = append(out, factory)
	}
	for _, st := range m.armBaseStations(ctx, corpID, armIndustry) {
		if st != factory {
			out = append(out, st)
		}
	}
	// A capital program's yard (corpprogram.go) is an Industry base while
	// the program runs: its parts and hull are built there.
	if yard := m.programYardStation(ctx, corpID); yard != 0 && !containsInt64(out, yard) {
		out = append(out, yard)
	}
	return out
}

// isIndustryBase reports whether a station is one of Industry's bases.
func (m *Mod) isIndustryBase(ctx context.Context, corpID, stationID int64) bool {
	return stationID != 0 && containsInt64(m.industryBaseStations(ctx, corpID), stationID)
}

// buildBases reads Industry's bases for the board's placement: lines,
// pressure, the free prints at each, and the production already queued there.
func (m *Mod) buildBases(ctx context.Context, corpID int64, open []corpWork) []buildBase {
	stations := m.industryBaseStations(ctx, corpID)
	if len(stations) == 0 {
		return nil
	}
	factory := stations[0]
	press, _ := m.basePressures(ctx, corpID, pressureMfg)
	idx := map[int64]int{}
	out := make([]buildBase, 0, len(stations))
	for _, st := range stations {
		b := buildBase{StationID: st, Factory: st == factory, Full: press[st].Full,
			CopyRuns: map[int64]int64{}, Originals: map[int64]bool{}}
		if use, err := m.stationLineUse(ctx, corpID, st); err == nil {
			b.Lines = use[ramActivityManufacturing].Lines
		}
		idx[st] = len(out)
		out = append(out, b)
	}
	// One base has nothing to choose between: the shelves are read only for
	// several. The queued load is read always, since expansion weighs it
	// (corpexpand.go).
	if len(out) > 1 {
		moving := m.movingItems(ctx, corpID)
		if cs, err := m.libraryCopiesAll(ctx, corpID); err == nil {
			for _, c := range cs {
				if i, ok := idx[c.StationID]; ok && c.Flag != 111 && !moving[c.ItemID] {
					out[i].CopyRuns[c.BPTypeID] += c.Runs
				}
			}
		}
		if ps, err := m.libraryPrintsAll(ctx, corpID); err == nil {
			for _, p := range ps {
				if i, ok := idx[p.StationID]; ok && p.Flag != 111 && !moving[p.ItemID] {
					out[i].Originals[p.BPTypeID] = true
				}
			}
		}
	}
	perRun := map[int64]float64{}
	for _, w := range open {
		if w.Kind != workKindBuild || w.RunsLeft() <= 0 {
			continue
		}
		i, ok := idx[orderStation(w, factory)]
		if !ok {
			continue
		}
		out[i].LoadSec += float64(w.RunsLeft()) * m.bpRunSec(ctx, w.BPTypeID, perRun)
	}
	return out
}

// bpRunSec is a print's production time a run, memoised in seen. 1 h when the
// type cannot be read, so an unknown order still weighs something.
func (m *Mod) bpRunSec(ctx context.Context, bpTypeID int64, seen map[int64]float64) float64 {
	if v, ok := seen[bpTypeID]; ok {
		return v
	}
	v := 3600.0
	if bp, err := evedb.GetBlueprintTypeInfo(ctx, m.db, bpTypeID); err == nil && bp.ProductionTime > 0 {
		v = float64(bp.ProductionTime)
	}
	seen[bpTypeID] = v
	return v
}
