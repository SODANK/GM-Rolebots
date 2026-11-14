package rolebots

import (
	"context"
	"database/sql"
	"math"
	"sort"

	evedb "gurimoon/db"
)

// Fuel, counted in fuel-day batches. One batch is 24 cycles of every line on the tower's
// bill: isotopes, uranium, oxygen, parts, coolant, robotics, the charter in faction 0.4+
// space, and Heavy Water and Liquid Ozone. The bill is the core's own
// (db.StarbaseFuelNeedsFullGrid), so what is counted is what the fuel sweep burns; the two
// grid fuels are taken at a full grid, as /fuel loads them.
// A tower's stock is the minimum over its lines of whole days held: the scarcest line
// decides, never a total.
// The Fueler keeps every tower at fuelTargetBatches in its bay and one more in the staging
// office's Fuel division (one in transit). Strontium is a separate line, kept in hours of
// reinforcement, not in batches.

const (
	// fuelTargetBatches is the charter's FuelWeekReserve default: a week.
	fuelTargetBatches = 7
	// fuelInTransitBatches is kept at the staging office on top.
	fuelInTransitBatches = 1
	// fuelUrgentBatches: a tower at or below this is run to first.
	fuelUrgentBatches = 2
	// fuelDarkUrgentSec: a tower that went dark is run to urgently for this
	// long after (towerPlan.UrgentAt).
	fuelDarkUrgentSec = 2 * 86400
	// haulUrgentSec: a fuel or ammo leg due inside this is anybody's to carry,
	// reserve or not (haulClaimOK).
	haulUrgentSec = 12 * 3600
	// strontiumHoursDefault is the reinforcement a tower keeps.
	strontiumHoursDefault = 24
	// fuelReserveWeeks is how many weeks of fuel per tower the affordability
	// test reserves (posplan.go): a corporation that cannot keep a tower
	// fuelled for a month cannot afford the tower.
	fuelReserveWeeks = 4
)

// fuelLine is one line of a tower's bill, per day.
type fuelLine struct {
	TypeID int64
	PerDay int64
	Volume float64 // m³ per unit
}

// fuelBatch turns a per-cycle bill into a day's batch. cyclesPerDay is 24 for
// every shipped tower (posControlTowerPeriod 1 h). Pure.
func fuelBatch(needs []evedb.FuelNeed, cyclesPerDay int64) []fuelLine {
	if cyclesPerDay <= 0 {
		cyclesPerDay = 24
	}
	out := make([]fuelLine, 0, len(needs))
	for _, n := range needs {
		if n.Quantity > 0 {
			out = append(out, fuelLine{TypeID: n.TypeID, PerDay: n.Quantity * cyclesPerDay})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TypeID < out[j].TypeID })
	return out
}

// batchesHeld is the scarcest line's whole days. A tower with no bill holds
// "forever" (math.MaxInt32). Pure.
func batchesHeld(batch []fuelLine, held map[int64]int64) int64 {
	if len(batch) == 0 {
		return math.MaxInt32
	}
	best := int64(math.MaxInt64)
	for _, l := range batch {
		if l.PerDay <= 0 {
			continue
		}
		if d := held[l.TypeID] / l.PerDay; d < best {
			best = d
		}
	}
	if best == math.MaxInt64 {
		return math.MaxInt32
	}
	return best
}

// batchUnits is n batches of every line. Pure.
func batchUnits(batch []fuelLine, n int64) map[int64]int64 {
	out := map[int64]int64{}
	if n <= 0 {
		return out
	}
	for _, l := range batch {
		out[l.TypeID] += l.PerDay * n
	}
	return out
}

// topUpUnits is what brings `held` up to n whole batches on every line: n*perDay - held per
// line, floored at zero. Topping up line by line, not by whole batches, means a tower with
// ten days of isotopes and two of robotics gets robotics, not five more days of everything. Pure.
func topUpUnits(batch []fuelLine, held map[int64]int64, n int64) map[int64]int64 {
	out := map[int64]int64{}
	for _, l := range batch {
		if want := l.PerDay*n - held[l.TypeID]; want > 0 {
			out[l.TypeID] = want
		}
	}
	return out
}

// batchVolume is the m³ of one batch. Pure.
func batchVolume(batch []fuelLine) float64 {
	v := 0.0
	for _, l := range batch {
		v += float64(l.PerDay) * l.Volume
	}
	return v
}

// towerFuelBatch is a tower type's daily batch where it stands (or would
// stand: a planned tower is priced in the system it is planned for).
func (m *Mod) towerFuelBatch(ctx context.Context, towerID, towerTypeID, systemID, corpID, allianceID int64) ([]fuelLine, error) {
	needs, err := evedb.StarbaseFuelNeedsFullGrid(ctx, m.db, evedb.StarbaseFuelRow{
		EntityID: towerID, TypeID: towerTypeID, SolarSystemID: systemID,
		OwnerCorpID: corpID, OwnerAllianceID: allianceID,
	})
	if err != nil {
		return nil, err
	}
	cycleMs := evedb.StarbaseFuelCycleMs(ctx, m.db, towerTypeID)
	perDay := int64(math.Round(86_400_000 / cycleMs))
	batch := fuelBatch(needs, perDay)
	m.fillLineVolumes(ctx, batch)
	return batch, nil
}

// fillLineVolumes reads each line's unit volume.
func (m *Mod) fillLineVolumes(ctx context.Context, batch []fuelLine) {
	for i := range batch {
		var v sql.NullFloat64
		_ = m.db.QueryRowContext(ctx, `SELECT volume FROM invTypes WHERE typeID = ?`, batch[i].TypeID).Scan(&v)
		batch[i].Volume = v.Float64
	}
}

// strontiumLine is the reinforcement line for `hours` of reinforcement.
func (m *Mod) strontiumLine(ctx context.Context, towerID, towerTypeID, systemID, corpID, allianceID int64, hours int64) []fuelLine {
	needs, err := evedb.StarbaseReinforceNeeds(ctx, m.db, evedb.StarbaseFuelRow{
		EntityID: towerID, TypeID: towerTypeID, SolarSystemID: systemID,
		OwnerCorpID: corpID, OwnerAllianceID: allianceID,
	})
	if err != nil {
		return nil
	}
	var out []fuelLine
	for _, n := range needs {
		if n.Quantity > 0 {
			out = append(out, fuelLine{TypeID: n.TypeID, PerDay: n.Quantity * hours})
		}
	}
	m.fillLineVolumes(ctx, out)
	return out
}

// bayHeld sums each type's units in a structure's flag-0 bay.
func (m *Mod) bayHeld(ctx context.Context, structureID int64) map[int64]int64 {
	out := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, SUM(quantity) FROM entity WHERE locationID = ? AND flag = 0 GROUP BY typeID`, structureID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t, q int64
		if rows.Scan(&t, &q) == nil {
			out[t] = q
		}
	}
	return out
}

// divisionHeld sums each type's units in one office division.
func (m *Mod) divisionHeld(ctx context.Context, corpID, officeID int64, flag int64) map[int64]int64 {
	out := map[int64]int64{}
	if officeID == 0 {
		return out
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, SUM(quantity) FROM entity
		 WHERE ownerID = ? AND locationID = ? AND flag = ? GROUP BY typeID`, corpID, officeID, flag)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t, q int64
		if rows.Scan(&t, &q) == nil {
			out[t] = q
		}
	}
	return out
}

// batchCost is what n batches cost at value (TypeValues), for the
// affordability test and the P&L.
func (m *Mod) batchCost(ctx context.Context, batch []fuelLine, n float64) float64 {
	if len(batch) == 0 || n <= 0 {
		return 0
	}
	ids := make([]int64, 0, len(batch))
	for _, l := range batch {
		ids = append(ids, l.TypeID)
	}
	vals, err := evedb.TypeValues(ctx, m.db, ids)
	if err != nil {
		return 0
	}
	total := 0.0
	for _, l := range batch {
		total += vals[l.TypeID].Value * float64(l.PerDay)
	}
	return total * n
}
