package rolebots

import (
	"context"
	"math"
	"sort"

	evedb "gurimoon/db"
)

// The fuel bay is one hold: every fuel line and the strontium share the tower type's
// invTypes.capacity (core AddToStarbaseBay, flag 0). Heavy Water and Liquid Ozone burn at the
// grid actually used, well under the full-grid batch they are bought by, so a bay topped up
// by whole batches fills with those and strontium and the isotopes cannot get in.
// So the bay is planned by volume: it keeps as many whole days as fit beside the strontium
// reserve (the office keeps the rest of the charter's reserve); a fuel leg carries each
// line's own shortfall, scarcest line first, only as much as there is room for; and the
// carrier takes out what a line holds over its days to make room (it rides back to the office).

// towerBay is one tower's fuel bay as the fueler plans it.
type towerBay struct {
	Batch    []fuelLine        // a full-grid day of fuel
	SR       []fuelLine        // the strontium reserve (PerDay = units for the charter's hours)
	Held     map[int64]int64   // the flag-0 bay
	Capacity float64           // m³; 0 = unbounded
	Used     float64           // m³ in the bay
	Days     int64             // whole days the bay keeps (bayFitDays)
	Vol      map[int64]float64 // m³ per unit of every type in the bay or the bill
}

// free is the m³ the bay has left (+Inf when unbounded).
func (tb towerBay) free() float64 {
	if tb.Capacity <= 0 {
		return math.Inf(1)
	}
	return math.Max(0, tb.Capacity-tb.Used)
}

// bayFitDays is how many whole days of the batch the bay keeps beside the
// strontium reserve: want (the charter's reserve) when they fit, fewer when
// they do not, never under one. Pure.
func bayFitDays(batch, sr []fuelLine, capacity float64, want int64) int64 {
	want = max(want, 1)
	day := batchVolume(batch)
	if capacity <= 0 || day <= 0 {
		return want
	}
	n := int64(math.Floor((capacity - batchVolume(sr)) / day))
	return min(max(n, 1), want)
}

// baySurplus is what each type in the bay holds over what the bay keeps of
// it: a line over its days, strontium over its reserve, and the whole of
// anything the tower does not burn at all. Pure.
func baySurplus(batch, sr []fuelLine, held map[int64]int64, days int64) map[int64]int64 {
	keep := map[int64]int64{}
	for _, l := range batch {
		keep[l.TypeID] += l.PerDay * days
	}
	for _, l := range sr {
		keep[l.TypeID] += l.PerDay
	}
	out := map[int64]int64{}
	for t, q := range held {
		if over := q - keep[t]; over > 0 {
			out[t] = over
		}
	}
	return out
}

// surplusVolume is the m³ baySurplus would free. Pure.
func surplusVolume(surplus map[int64]int64, vol map[int64]float64) float64 {
	v := 0.0
	for t, q := range surplus {
		v += float64(q) * vol[t]
	}
	return v
}

// fuelLegLines is what a fuel leg carries: each line's shortfall to `days` (strontium to its
// reserve), no more than the division holds, the line with the fewest days held first (the
// isotopes), strontium last, and only as much as `room` m3 takes. Pure.
func fuelLegLines(batch, sr []fuelLine, held, div map[int64]int64, days int64, room float64, flag int64) []haulItem {
	type want struct {
		line fuelLine
		days float64
		qty  int64
	}
	var burn []want
	for _, l := range batch {
		if l.PerDay <= 0 {
			continue
		}
		if q := l.PerDay*days - held[l.TypeID]; q > 0 {
			burn = append(burn, want{l, float64(held[l.TypeID]) / float64(l.PerDay), q})
		}
	}
	sort.SliceStable(burn, func(i, j int) bool {
		if burn[i].days != burn[j].days {
			return burn[i].days < burn[j].days
		}
		return burn[i].line.PerDay > burn[j].line.PerDay
	})
	for _, l := range sr {
		if q := l.PerDay - held[l.TypeID]; q > 0 {
			burn = append(burn, want{l, 0, q})
		}
	}
	var out []haulItem
	taken := map[int64]int64{} // a type both burned and reserved draws on one division stock
	for _, w := range burn {
		q := min(w.qty, div[w.line.TypeID]-taken[w.line.TypeID])
		if w.line.Volume > 0 && !math.IsInf(room, 1) {
			q = min(q, int64(math.Floor(room/w.line.Volume)))
		}
		if q <= 0 {
			continue
		}
		room -= float64(q) * w.line.Volume
		taken[w.line.TypeID] += q
		out = append(out, haulItem{TypeID: w.line.TypeID, Qty: q, Flag: flag})
	}
	return out
}

// towerBayOf reads a tower's bay and plans it against the charter's reserve
// (target whole days) and strontium hours.
func (m *Mod) towerBayOf(ctx context.Context, p *towerPlan, corpID int64, target, srHours int64) (towerBay, error) {
	tb := towerBay{Vol: map[int64]float64{}}
	batch, err := m.towerFuelBatch(ctx, p.TowerID, p.TowerTypeID, p.SystemID, corpID, m.corpAllianceOf(ctx, corpID))
	if err != nil {
		return tb, err
	}
	tb.Batch = batch
	tb.SR = m.strontiumLine(ctx, p.TowerID, p.TowerTypeID, p.SystemID, corpID, 0, srHours)
	tb.Held = m.bayHeld(ctx, p.TowerID)
	if c, used, err := evedb.StructureBayCapacity(ctx, m.db, p.TowerID, 0); err == nil {
		tb.Capacity, tb.Used = c, used
	}
	tb.Days = bayFitDays(tb.Batch, tb.SR, tb.Capacity, target)
	for _, l := range append(append([]fuelLine{}, tb.Batch...), tb.SR...) {
		tb.Vol[l.TypeID] = l.Volume
	}
	for t := range tb.Held {
		if _, ok := tb.Vol[t]; !ok {
			tb.Vol[t] = m.typeVolume(ctx, t)
		}
	}
	return tb, nil
}

// charterFuelTargets is the charter's bay reserve in whole days and its
// strontium hours, with the defaults a charter leaves unset.
func charterFuelTargets(ch Charter) (target, srHours int64) {
	target, srHours = int64(ch.FuelWeekReserve), int64(ch.StrontiumHours)
	if target <= 0 {
		target = fuelTargetBatches
	}
	if srHours <= 0 {
		srHours = strontiumHoursDefault
	}
	return target, srHours
}

// makeBayRoom is the carrier's half at the tower: when what it carries will not fit, it
// takes the bay's surplus, largest first, into its own hold until it does. What it took
// rides home with the leftovers (haulSettle). Returns the units taken out.
func (m *Mod) makeBayRoom(ctx context.Context, b *botRun, l *haulLeg) int64 {
	p, _ := m.loadTowerPlan(ctx, l.PlanID)
	if p == nil || p.TowerID == 0 || p.TowerID != l.ToID {
		return 0
	}
	c, _ := getCEO(ctx, m.db, b.CorpID)
	if c == nil {
		return 0
	}
	ch, err := m.ceoCharterFor(ctx, c)
	if err != nil {
		return 0
	}
	target, srHours := charterFuelTargets(ch)
	tb, err := m.towerBayOf(ctx, p, b.CorpID, target, srHours)
	if err != nil {
		return 0
	}
	need := -tb.free()
	for _, cr := range l.Carrying {
		v, ok := tb.Vol[cr.TypeID]
		if !ok {
			v = m.typeVolume(ctx, cr.TypeID)
		}
		need += float64(cr.Qty) * v
	}
	if need <= 0 {
		return 0
	}
	surplus := baySurplus(tb.Batch, tb.SR, tb.Held, tb.Days)
	// A line being delivered is not taken out to make room for itself.
	for _, cr := range l.Carrying {
		delete(surplus, cr.TypeID)
	}
	types := make([]int64, 0, len(surplus))
	for t := range surplus {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		vi, vj := float64(surplus[types[i]])*tb.Vol[types[i]], float64(surplus[types[j]])*tb.Vol[types[j]]
		if vi != vj {
			return vi > vj
		}
		return types[i] < types[j]
	})
	var out int64
	for _, t := range types {
		if need <= 0 {
			break
		}
		v := tb.Vol[t]
		take := surplus[t]
		if v > 0 {
			take = min(take, int64(math.Ceil(need/v)))
		}
		got, err := evedb.TakeFromStarbaseBay(ctx, m.db, p.TowerID, t, take, 0, b.ShipID, 5, b.CorpID)
		if err != nil || got <= 0 {
			continue
		}
		out += got
		need -= float64(got) * v
	}
	return out
}
