package rolebots

// Where the rock goes: the counter behind the Ore page.
//
// The disposal of a mining trip is decided once on State.Data (mining.go
// resolveMiningDisposal) and this table records the result, so the Ore page can
// show whether the roster's rock feeds a refiner or goes into an NPC bid. It is
// a day bucket, not a log: one upserted row per (day, typeID, stage), so the
// table stays a few thousand rows. It also holds the refine output (stage
// refined_out, whose typeIDs are minerals), since one event, what the rock
// became, needs one table. isk is what the units were worth at the time,
// recorded rather than re-derived on read.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// The stages a unit of rock can pass through. Kept short and stable: they are
// stored strings, so renaming one orphans every row already written under the
// old name rather than migrating it.
const (
	// oreStageMined is every unit a laser took out of a belt, whatever
	// happened to it afterwards. It is the denominator of every share the
	// page reports, so it is written at the belt and nowhere else.
	oreStageMined = "mined"
	// oreStageSoldNPC and oreStageSoldBot split a sale by who took the other
	// side. The NPC bid is a sink that removes the units from the economy, a bot
	// bid is a refiner about to put minerals back.
	oreStageSoldNPC = "sold_npc"
	oreStageSoldBot = "sold_bot"
	// oreStageListed is rock rested on the market as an ask. It is not a sale and
	// must never be counted as one: the fill, whenever it comes, is booked again
	// under sold_bot or sold_npc. It measures that an ore supply side exists.
	oreStageListed = "listed"
	// oreStageBought is rock a bot bought to refine (an industrialist taking ore
	// instead of metal, or a trader flipping a stack). Its own stage, since it does
	// not pair with sold_bot: a purchase off an NPC ask has no such partner.
	oreStageBought = "bought"
	// oreStageRefinedIn is ore consumed by a reprocess; oreStageRefinedOut is
	// the minerals it produced. The pair is what makes realised yield a
	// measurement rather than an assumption about skills.
	oreStageRefinedIn  = "refined_in"
	oreStageRefinedOut = "refined_out"
	// oreStageStockpiled is ore parked in a hangar as a project's materials or a
	// factory's larder. Not a loss and not a supply: the pile that has not decided.
	oreStageStockpiled = "stockpiled"
	// oreStageJetcan is ore jettisoned into a can: a fleet miner filling for
	// its hauler, or a conscript under a player's orders.
	oreStageJetcan = "jetcan"
	// oreStageFleet is a commander disposing of a fleet's pooled haul, which
	// is a different event from a solo pilot selling its own hold and is the
	// only way to tell organised mining's contribution from freelance.
	oreStageFleet = "fleet"
	// oreStageHandBack is a co-op hauler's load leaving its hold for the miners'
	// own hangars (fleetore.go handBackFleetOre), booked so what the belt canned
	// (jetcan) and what miners got back can be read side by side.
	oreStageHandBack = "handback"
	// oreStageExpired is ore destroyed with a cargo container that timed out
	// before any hauler emptied it (cans.go sweepCans). A loss, booked so it is
	// measured.
	oreStageExpired = "expired"
)

// oreFlowStages is the display order for the page: where it came from, where
// it went, what it became. Not the declaration order of the constants above.
var oreFlowStages = []string{
	oreStageMined, oreStageListed, oreStageSoldBot, oreStageSoldNPC,
	oreStageBought, oreStageRefinedIn, oreStageRefinedOut,
	oreStageStockpiled, oreStageJetcan, oreStageHandBack, oreStageExpired, oreStageFleet,
}

// oreFlowLabels is what each stage is called in prose.
var oreFlowLabels = map[string]string{
	oreStageMined:      "Mined",
	oreStageListed:     "Listed on the market",
	oreStageSoldBot:    "Sold to a bot bid",
	oreStageSoldNPC:    "Sold to an NPC bid",
	oreStageBought:     "Bought to refine",
	oreStageRefinedIn:  "Refined (ore in)",
	oreStageRefinedOut: "Refined (minerals out)",
	oreStageStockpiled: "Stockpiled",
	oreStageJetcan:     "Jettisoned to a can",
	oreStageHandBack:   "Handed back to miners",
	oreStageExpired:    "Lost to can expiry",
	oreStageFleet:      "Off mining fleets",
}

// oreFlowKeepDays is how much history the table holds. A month: long enough
// to see whether a change to the disposal rules moved anything, short enough
// that the table never becomes something anyone has to think about.
const oreFlowKeepDays = 30

// oreFlowSweepEveryTicks is the retention pass cadence: hourly, since the sweep
// is a single ranged DELETE over a few thousand rows.
const oreFlowSweepEveryTicks = 3600

// oreFlowDay is the bucket a moment falls in. Whole days from the epoch, so
// the arithmetic in a range query is a subtraction rather than a date
// function, and the harness's fake clock buckets exactly as the real one does.
func oreFlowDay(unix int64) int32 { return int32(unix / 86400) }

// noteOreFlow records units of one type passing through one stage. Volume is
// derived from the type via evedb.GetTypeVolume, not destiny.UnitVolume (an ore
// table with a 1.0 fallback that would misrecord refine output). Best-effort:
// errors are logged and swallowed so a mining trip never fails on bookkeeping.
func (m *Mod) noteOreFlow(ctx context.Context, typeID int64, stage string, units int64, isk float64) {
	if typeID == 0 || units <= 0 || stage == "" {
		return
	}
	m3 := float64(units) * m.flowVolume(ctx, typeID)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_ore_flow (day, typeID, stage, units, m3, isk)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			units = units + VALUES(units),
			m3    = m3    + VALUES(m3),
			isk   = isk   + VALUES(isk)`,
		oreFlowDay(clock.Unix()), typeID, stage, units, m3, isk); err != nil {
		rlogf("rolebots: ore flow %s type=%d: %v", stage, typeID, err)
	}
}

// flowVolume is the m3 of one unit, memoised: a type's volume never changes, so
// there is no expiry and the map is bounded by the ore and mineral types that
// move. An unreadable volume answers 0, which records units and no m3.
func (m *Mod) flowVolume(ctx context.Context, typeID int64) float64 {
	flowVolMu.RLock()
	v, ok := flowVolCache[typeID]
	flowVolMu.RUnlock()
	if ok {
		return v
	}
	v, err := evedb.GetTypeVolume(ctx, m.db, typeID)
	if err != nil || v < 0 {
		v = 0
	}
	flowVolMu.Lock()
	flowVolCache[typeID] = v
	flowVolMu.Unlock()
	return v
}

var (
	flowVolMu    sync.RWMutex
	flowVolCache = map[int64]float64{}
)

// noteOreFlowLots is the same for a list of (typeID, units) lots, the shape a
// disposal holds. isk is the whole disposal's proceeds, apportioned by units
// because the per-lot price is not always known at the call site.
func (m *Mod) noteOreFlowLots(ctx context.Context, stage string, lots map[int64]int64, isk float64) {
	var total int64
	for _, u := range lots {
		if u > 0 {
			total += u
		}
	}
	if total <= 0 {
		return
	}
	for typeID, units := range lots {
		if units <= 0 {
			continue
		}
		m.noteOreFlow(ctx, typeID, stage, units, isk*float64(units)/float64(total))
	}
}

// startOreFlowSweep registers the retention pass. It runs regardless of the
// master switch (as cachesweep.go does), since turning the roster off is when
// nothing else would trim the table.
func (m *Mod) startOreFlowSweep() {
	m.registerPeriodic("rolebot-oreflow-sweep", oreFlowSweepEveryTicks, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_ore_flow WHERE day < ?`,
			oreFlowDay(clock.Unix())-oreFlowKeepDays); err != nil {
			rlogf("rolebots: ore flow sweep: %v", err)
		}
		return true
	})
}

// oreFlowTotal is one stage's total over a window.
type oreFlowTotal struct {
	Stage string
	Label string
	Units int64
	M3    float64
	ISK   float64
}

// Display helpers. Module pages are parsed with no funcMap, so every number a
// template prints is either a preformatted string or a method on the row.
func (t oreFlowTotal) UnitsStr() string { return iskShort(float64(t.Units)) }
func (t oreFlowTotal) M3Str() string    { return iskShort(t.M3) }
func (t oreFlowTotal) ISKStr() string {
	if t.ISK <= 0 {
		// Most stages carry no money: a zero would read as "sold for nothing" rather
		// than "not a sale".
		return "—"
	}
	return iskShort(t.ISK)
}

// oreFlowTotals sums every stage over the last `days` whole day-buckets,
// inclusive of today. Ordered by oreFlowStages rather than by size: this table
// is read as a pipeline, and a pipeline sorted by volume stops being one.
func (m *Mod) oreFlowTotals(ctx context.Context, days int) ([]oreFlowTotal, error) {
	if days < 1 {
		days = 1
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT stage, SUM(units), SUM(m3), SUM(isk)
		  FROM rolebot_ore_flow
		 WHERE day > ?
		 GROUP BY stage`, oreFlowDay(clock.Unix())-int32(days))
	if err != nil {
		return nil, fmt.Errorf("ore flow totals: %w", err)
	}
	defer rows.Close()

	got := map[string]oreFlowTotal{}
	for rows.Next() {
		var t oreFlowTotal
		var units sql.NullInt64
		var m3, isk sql.NullFloat64
		if err := rows.Scan(&t.Stage, &units, &m3, &isk); err != nil {
			return nil, fmt.Errorf("ore flow totals scan: %w", err)
		}
		t.Units, t.M3, t.ISK = units.Int64, m3.Float64, isk.Float64
		got[t.Stage] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ore flow totals: %w", err)
	}

	// Every stage gets a row even with nothing in it, so a missing line is not
	// mistaken for a page that has not finished loading.
	out := make([]oreFlowTotal, 0, len(oreFlowStages))
	for _, s := range oreFlowStages {
		t := got[s]
		t.Stage, t.Label = s, oreFlowLabels[s]
		out = append(out, t)
	}
	return out, nil
}

// oreFlowTypeRow is one ore type's movement over the window.
type oreFlowTypeRow struct {
	TypeID int64
	Name   string
	Mined  int64
	Listed int64
	ByBot  int64
	ByNPC  int64
	Bought int64
	Refine int64
	Other  int64
	M3     float64
}

// oreFlowByType is the per-type breakdown, ordered by units mined. Limited,
// because the point is which rocks the roster actually works, not an inventory.
func (m *Mod) oreFlowByType(ctx context.Context, days, limit int) ([]oreFlowTypeRow, error) {
	if days < 1 {
		days = 1
	}
	if limit < 1 {
		limit = 25
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT f.typeID, COALESCE(t.typeName, ''), f.stage, SUM(f.units), SUM(f.m3)
		  FROM rolebot_ore_flow f
		  LEFT JOIN invTypes t ON t.typeID = f.typeID
		 WHERE f.day > ? AND f.stage <> ?
		 GROUP BY f.typeID, t.typeName, f.stage`,
		oreFlowDay(clock.Unix())-int32(days), oreStageRefinedOut)
	if err != nil {
		return nil, fmt.Errorf("ore flow by type: %w", err)
	}
	defer rows.Close()

	by := map[int64]*oreFlowTypeRow{}
	for rows.Next() {
		var typeID, units int64
		var name, stage string
		var m3 float64
		if err := rows.Scan(&typeID, &name, &stage, &units, &m3); err != nil {
			return nil, fmt.Errorf("ore flow by type scan: %w", err)
		}
		r := by[typeID]
		if r == nil {
			r = &oreFlowTypeRow{TypeID: typeID, Name: name}
			by[typeID] = r
		}
		switch stage {
		case oreStageMined:
			r.Mined += units
			r.M3 += m3
		case oreStageListed:
			r.Listed += units
		case oreStageSoldBot:
			r.ByBot += units
		case oreStageSoldNPC:
			r.ByNPC += units
		case oreStageBought:
			r.Bought += units
		case oreStageRefinedIn:
			r.Refine += units
		default:
			r.Other += units
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ore flow by type: %w", err)
	}

	out := make([]oreFlowTypeRow, 0, len(by))
	for _, r := range by {
		if r.Name == "" {
			r.Name = fmt.Sprintf("type %d", r.TypeID)
		}
		out = append(out, *r)
	}
	sortOreFlowRows(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// sortOreFlowRows orders by units mined, then by name so a tie is stable
// across refreshes rather than reshuffling out of a map walk.
func sortOreFlowRows(rows []oreFlowTypeRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Mined != rows[j].Mined {
			return rows[i].Mined > rows[j].Mined
		}
		return rows[i].Name < rows[j].Name
	})
}
