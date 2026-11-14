package rolebots

// The warehouse run: a pilot flies to a station where the roster has goods and runs the ordinary
// hangar sweeps there.
//
// Every hangar sweep (strays.go, mineralstrays.go, orestrays.go, liquidate.go, metasell.go, beltloot.go)
// filters locationID = b.StationID, so goods at other stations are out of reach. The pilot goes to the
// goods and nothing is hauled, which keeps pricing and disposal in the existing sweeps. The run also
// visits a commander's pooled fleet haul (rolebot_ore_hold, keyed by character and station).
//
// It never counts blueprints or skillbooks (categories 9 and 16, capital in liquidate.go) or a station
// the pilot uses as a factory, whose hangar is a bill of materials.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
)

const (
	// settingWarehouseRuns gates the whole file. Default off: the first pass over a large roster places many
	// orders at once, and an operator decides when. Same idiom as the ore dials: const key, atomic cache,
	// non-fatal boot loader, setting written before the cache.
	settingWarehouseRuns = "rolebots.warehouse.runs"

	// warehouseLookSec is how often one pilot asks. The answer is two grouped queries and is usually "nothing
	// worth a trip"; the interval also keeps a pilot that just made a run from drawing another.
	warehouseLookSec = 3600

	// warehouseRetrySec is how long one station is left alone after a visit. A sweep can dispose of nothing
	// (a hangar of ammunition at a dock with no bids), so without it the pilot would fly back every hour.
	// It is the base of the persisted memo's back-off (warehouse_memo.go).
	warehouseRetrySec = 12 * 3600

	// warehouseMinValue is what a hoard must be worth, after the distance discount, before a pilot flies
	// to it. It is twice metaRunMinValue and sized by reach rather than trip value: a higher floor would put
	// most hoards out of range past a few jumps.
	warehouseMinValue = 1_000_000.0

	// warehouseJumpCost is the distance discount, shared with the hunter's
	// fence and the meta run (lootHubJumpCost) so the three read as one idea
	// about what a jump is worth rather than as three opinions.
	warehouseJumpCost = lootHubJumpCost

	// warehouseCandidates bounds how many stations get a route solved for
	// them. Ranked by raw value first, because a hoard that cannot clear the
	// floor at zero jumps cannot clear it at four either.
	warehouseCandidates = 8
)

var (
	warehouseOn atomic.Bool

	warehouseMu sync.Mutex
	warehouseAt = map[int64]int64{} // charID to unix of last look
)

// warehouseRunsOn is the gate, read on the planner's hot path.
func warehouseRunsOn() bool { return warehouseOn.Load() }

// loadWarehouseSettings reads the dial at boot. Never fatal: an unreadable
// dial means the shipped default, not a module that refuses to start.
func (m *Mod) loadWarehouseSettings(ctx context.Context) {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingWarehouseRuns)
	if err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingWarehouseRuns, err)
		return
	}
	if ok {
		warehouseOn.Store(truthySetting(v))
	}
}

// truthySetting reads the handful of spellings an operator might type into a
// settings row.
func truthySetting(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
		return f > 0
	}
	return false
}

// ── the survey ──────────────────────────────────────────────────────────────

// warehouseStop is one station holding something of this pilot's.
type warehouseStop struct {
	StationID int64
	SystemID  int64
	Stacks    int64
	Units     int64
	Value     float64 // at basePrice: a trip heuristic, not a collateral quote
	FleetOre  bool    // a pooled mining haul waits here too
	Jumps     int
}

// Worth is the distance-discounted value the trip is judged on.
func (s warehouseStop) Worth() float64 {
	if s.Jumps < 0 {
		return 0
	}
	return s.Value / (1 + warehouseJumpCost*float64(s.Jumps))
}

// warehouseWorthIt is the trip test, pure so the harness can pin both ends of it: a fortune four jumps
// away is worth going for, a pittance next door is not.
func warehouseWorthIt(value float64, jumps int) bool {
	if jumps < 0 || value <= 0 {
		return false
	}
	return value/(1+warehouseJumpCost*float64(jumps)) >= warehouseMinValue
}

// warehouseSweepCategories is what a sweep at the far end can dispose of. Categories 9 and 16
// (blueprints and skillbooks) are absent: liquidate.go will not sell them because they are capital,
// and the courier batches them.
const warehouseSweepCategories = "4, 7, 8, 25"

// warehouseSurveyCategories is what the survey counts: the sweep categories plus trade goods (17,
// sellHangarCommodities) and packaged hulls (6, shipsurplus.go, only for a pilot over its hull cap).
// Drones (18) stay out: nothing at the far end disposes of a hangar drone.
const warehouseSurveyCategories = "4, 6, 7, 8, 17, 25"

// warehouseHoardFilter is what the survey counts, matching what the far-end sweeps touch. Args: whether
// packaged hulls count (the pilot is over its hull cap), and the cut-off before which a held sub-lot
// mineral stack is due to be listed (mineralHoldMaxSec). It skips loot-hold types, trader stock and held lots.
const warehouseHoardFilter = `g.categoryID IN (` + warehouseSurveyCategories + `)
		   AND (g.categoryID <> 6 OR ? = 1) AND t.groupID <> 29
		   AND NOT EXISTS (SELECT 1 FROM rolebot_loot_hold lh
		         WHERE lh.characterID = e.ownerID AND lh.typeID = e.typeID AND lh.units > 0)
		   AND NOT (g.categoryID = 17 AND EXISTS (SELECT 1 FROM rolebot_trade_stock ts
		         WHERE ts.characterID = e.ownerID AND ts.stationID = e.locationID AND ts.typeID = e.typeID))
		   AND NOT EXISTS (SELECT 1 FROM rolebot_mineral_hold mh
		         WHERE mh.characterID = e.ownerID AND mh.stationID = e.locationID
		           AND mh.typeID = e.typeID AND mh.sinceAt > ?)`

// hullsOverCap reports whether the pilot owns more hulls than it keeps (shipsurplus.go: the boarded
// ship, one shuttle and the spares), the only case packaged hulls are worth a trip.
func (m *Mod) hullsOverCap(ctx context.Context, charID int64) int {
	var n int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(IF(e.singleton = 1, 1, e.quantity)), 0) FROM entity e
		  JOIN invTypes t  ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 6
		 WHERE e.ownerID = ? AND e.flag = 4 AND t.groupID <> ?`, charID, capsuleGroupID).Scan(&n)
	if n > int64(max(escrowSettingInt(ctx, m.db, shipsSparesKey, shipsSparesDefault), 0))+2 {
		return 1
	}
	return 0
}

// nominalRefineCache is a type's metal per unit at a nominal 50% yield: the survey's estimate of a dead
// stack and the scrap floor's value (scrapValue).
var nominalRefineCache sync.Map

// mineralYield keeps the minerals of a yield list. The list also carries
// non-mineral rows (a Rifter's Frigate Construction skill line, R.A.M. tools)
// that are no metal and would value a hull at several times its minerals. Pure.
func mineralYield(mats []evedb.TypeMaterial) []evedb.TypeMaterial {
	out := make([]evedb.TypeMaterial, 0, len(mats))
	for _, mt := range mats {
		for _, id := range mineralCascade {
			if mt.MaterialTypeID == id {
				out = append(out, mt)
				break
			}
		}
	}
	return out
}

// typeCategoryCache is invGroups.categoryID by type.
var typeCategoryCache sync.Map

// typeCategoryOf is a type's category, 0 when it has none.
func (m *Mod) typeCategoryOf(ctx context.Context, typeID int64) int64 {
	if v, ok := typeCategoryCache.Load(typeID); ok {
		return v.(int64)
	}
	var cat int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT g.categoryID FROM invTypes t
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID = ?`, typeID).Scan(&cat); err != nil {
		return 0
	}
	typeCategoryCache.Store(typeID, cat)
	return cat
}

// scrapValue is a type's worth as metal for pricing, 0 when it has no real
// scrap exit: only plain modules and hulls are ever refined. Charges and
// drones are not, and neither is named or graded gear (deadStockVerdict).
func (m *Mod) scrapValue(ctx context.Context, typeID int64) float64 {
	if cat := m.typeCategoryOf(ctx, typeID); cat != catModule && cat != catShip {
		return 0
	}
	if m.typeMetaGroupOf(ctx, typeID) > 1 || m.typeMetaLevelOf(ctx, typeID) > 0 {
		return 0
	}
	return m.nominalRefineUnit(ctx, typeID)
}

func (m *Mod) nominalRefineUnit(ctx context.Context, typeID int64) float64 {
	if v, ok := nominalRefineCache.Load(typeID); ok {
		return v.(float64)
	}
	v := 0.0
	if mats, err := evedb.GetTypeMaterials(ctx, m.db, typeID); err == nil && len(mats) > 0 {
		portion, perr := evedb.GetTypePortionSize(ctx, m.db, typeID)
		if perr != nil || portion <= 0 {
			portion = 1
		}
		for _, mt := range mineralYield(mats) {
			v += float64(mt.Quantity) * 0.5 * m.basePriceCached(ctx, mt.MaterialTypeID)
		}
		v /= float64(portion)
	}
	nominalRefineCache.Store(typeID, v)
	return v
}

// realHoardValues re-prices the dearest stations of the survey at what the
// sweeps there would really get (warehouseTypeValue): their dearest
// warehouseRealTypes types by basePrice, the rest counted at nothing. The
// other stations keep their basePrice value and rank behind.
func (m *Mod) realHoardValues(ctx context.Context, b *botRun, by map[int64]*warehouseStop, hulls int) {
	if len(by) == 0 {
		return
	}
	list := make([]*warehouseStop, 0, len(by))
	for _, s := range by {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Value != list[j].Value {
			return list[i].Value > list[j].Value
		}
		return list[i].StationID < list[j].StationID
	})
	hist := m.marketHistory(ctx)
	for i, s := range list {
		if i >= warehouseRealStations {
			break
		}
		rows, err := m.db.QueryContext(ctx, `
			SELECT e.typeID, g.categoryID, SUM(e.quantity), COALESCE(MAX(t.basePrice), 0)
			  FROM entity e
			  JOIN invTypes t  ON t.typeID = e.typeID
			  JOIN invGroups g ON g.groupID = t.groupID
			 WHERE e.ownerID = ? AND e.flag = 4 AND e.singleton = 0 AND e.locationID = ?
			   AND `+warehouseHoardFilter+`
			 GROUP BY e.typeID, g.categoryID
			 ORDER BY SUM(e.quantity) * COALESCE(MAX(t.basePrice), 0) DESC
			 LIMIT ?`, b.CharID, s.StationID, hulls, clock.Unix()-mineralHoldMaxSec, warehouseRealTypes)
		if err != nil {
			continue
		}
		type line struct {
			typeID, cat, units int64
			base               float64
		}
		var lines []line
		for rows.Next() {
			var l line
			if rows.Scan(&l.typeID, &l.cat, &l.units, &l.base) == nil {
				lines = append(lines, l)
			}
		}
		rows.Close()
		region := m.stationRegionOf(ctx, s.StationID)
		value := 0.0
		for _, l := range lines {
			bidGross := 0.0
			if bids, err := m.bidsReachingStation(ctx, s.StationID, l.typeID, b.CharID); err == nil {
				bidGross = bidsRaise(bids, l.units)
			}
			refs := m.tradeRefsOf(ctx, l.typeID)
			tape, src := pickTradeValue(refs.Region[region], refs.Cluster, 0)
			if src == "book" {
				tape = 0
			}
			refine := m.nominalRefineUnit(ctx, l.typeID)
			if m.typeMetaLevelOf(ctx, l.typeID) > 0 || m.typeMetaGroupOf(ctx, l.typeID) > 1 {
				refine = 0 // named and graded gear is held, never scrapped
			}
			value += warehouseTypeValue(l.units, bidGross, tape, l.base, hist.regionPerDay(region, l.typeID),
				hist.Ready, l.cat == 17, refine, l.cat == catCharge, hist.globalPerDay(l.typeID))
		}
		s.Value = value
	}
}

// remoteHoards is every station holding disposable goods of this pilot's, other than the one it is
// standing in. One grouped query values them in SQL at invTypes.basePrice, since the trip test needs an
// order of magnitude, not a quote. The join to staStations is required: an item's locationID is not
// always a station, and a station the map cannot place cannot be a destination.
func (m *Mod) remoteHoards(ctx context.Context, b *botRun) ([]warehouseStop, error) {
	hulls := m.hullsOverCap(ctx, b.CharID)
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.locationID, s.solarSystemID, COUNT(*), SUM(e.quantity),
		       SUM(e.quantity * COALESCE(NULLIF(t.basePrice, 0), 0))
		  FROM entity e
		  JOIN invTypes t    ON t.typeID = e.typeID
		  JOIN invGroups g   ON g.groupID = t.groupID
		  JOIN staStations s ON s.stationID = e.locationID
		 WHERE e.ownerID = ? AND e.flag = 4 AND e.singleton = 0
		   AND e.locationID <> ?
		   AND `+warehouseHoardFilter+`
		 GROUP BY e.locationID, s.solarSystemID`, b.CharID, b.StationID, hulls, clock.Unix()-mineralHoldMaxSec)
	if err != nil {
		return nil, fmt.Errorf("remote hoards: %w", err)
	}
	defer rows.Close()

	skip := warehouseLarders(b)
	by := map[int64]*warehouseStop{}
	for rows.Next() {
		var s warehouseStop
		if err := rows.Scan(&s.StationID, &s.SystemID, &s.Stacks, &s.Units, &s.Value); err != nil {
			return nil, fmt.Errorf("remote hoards scan: %w", err)
		}
		if skip[s.StationID] || s.SystemID == 0 {
			continue
		}
		st := s
		by[s.StationID] = &st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("remote hoards: %w", err)
	}
	m.realHoardValues(ctx, b, by, hulls)

	// A commander's pooled fleet haul is its own reason to travel, whether or not the ordinary categories
	// at that dock clear the floor.
	if err := m.warehouseFleetStops(ctx, b, by, skip); err != nil {
		return nil, err
	}

	out := make([]warehouseStop, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	// Raw value first: the route solve is the expensive half, and a hoard that cannot clear the floor at
	// zero jumps cannot clear it at four.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return out[i].StationID < out[j].StationID
	})
	return out, nil
}

// warehouseFleetStops folds the pilot's undisposed fleet-ore holds into the
// survey, so a commander travels for a pooled haul the ordinary category scan
// would otherwise have valued at whatever the rock's basePrice happens to be.
func (m *Mod) warehouseFleetStops(ctx context.Context, b *botRun,
	by map[int64]*warehouseStop, skip map[int64]bool) error {

	rows, err := m.db.QueryContext(ctx, `
		SELECT h.stationID, s.solarSystemID, SUM(h.units),
		       SUM(h.units * COALESCE(NULLIF(t.basePrice, 0), 0))
		  FROM rolebot_ore_hold h
		  JOIN staStations s   ON s.stationID = h.stationID
		  LEFT JOIN invTypes t ON t.typeID = h.typeID
		 WHERE h.characterID = ? AND h.units > 0 AND h.stationID <> ?
		 GROUP BY h.stationID, s.solarSystemID`,
		b.CharID, b.StationID)
	if err != nil {
		return fmt.Errorf("remote fleet holds: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var stationID, systemID, units int64
		var value float64
		if err := rows.Scan(&stationID, &systemID, &units, &value); err != nil {
			return fmt.Errorf("remote fleet holds scan: %w", err)
		}
		if skip[stationID] || systemID == 0 {
			continue
		}
		if s := by[stationID]; s != nil {
			// The same rock is already counted by the category scan; the hold is a ledger over hangar stacks, so
			// only the flag is new.
			s.FleetOre = true
			continue
		}
		by[stationID] = &warehouseStop{
			StationID: stationID, SystemID: systemID,
			Units: units, Value: value, FleetOre: true,
		}
	}
	return rows.Err()
}

// warehouseLarders is every station whose hangar is this pilot's own bill of materials rather than
// residue. Selling a factory's stock out from under it is the failure strays.go's scope note warns about.
func warehouseLarders(b *botRun) map[int64]bool {
	out := map[int64]bool{}
	if b.Profile != nil && b.Profile.IndustryStationID != 0 {
		out[b.Profile.IndustryStationID] = true
	}
	d := b.State.Data
	if d.IndFactoryID != 0 {
		out[d.IndFactoryID] = true
	}
	if d.StandbyFactoryID != 0 {
		out[d.StandbyFactoryID] = true
	}
	return out
}

// ── the run ─────────────────────────────────────────────────────────────────

// warehouseRun takes the pass when a pilot has goods somewhere worth the trip.
// Reports whether it took it.
func (m *Mod) warehouseRun(ctx context.Context, b *botRun) (bool, error) {
	if !warehouseRunsOn() || b.StationID == 0 || b.ShipID == 0 {
		return false, nil
	}
	now := clock.Unix()
	warehouseMu.Lock()
	if now-warehouseAt[b.CharID] < warehouseLookSec {
		warehouseMu.Unlock()
		return false, nil
	}
	warehouseAt[b.CharID] = now
	warehouseMu.Unlock()

	stops, err := m.remoteHoards(ctx, b)
	if err != nil || len(stops) == 0 {
		return false, err
	}

	growth := escrowSettingFloat(ctx, m.db, warehouseValueGrowthKey, warehouseValueGrowthDefault)
	memo := b.State.Data.WarehouseMemo
	best, bestWorth, tried := warehouseStop{}, 0.0, 0
	for _, s := range stops {
		if tried >= warehouseCandidates {
			break
		}
		if e, ok := memo[s.StationID]; ok && warehouseMemoHolds(e, now, s.Value, growth) {
			continue
		}
		tried++
		jumps, jerr := m.jumpsBetween(ctx, b, b.SystemID, s.SystemID)
		if jerr != nil {
			continue // unroutable under this pilot's rules: not a stop for it
		}
		s.Jumps = jumps
		if w := s.Worth(); w > bestWorth && warehouseWorthIt(s.Value, jumps) {
			best, bestWorth = s, w
		}
	}
	if best.StationID == 0 {
		return false, nil
	}
	st := b.State
	if st.Data.WarehouseMemo == nil {
		st.Data.WarehouseMemo = map[int64]warehouseMemo{}
	}
	st.Data.WarehouseMemo[best.StationID] = warehouseMemoDepart(st.Data.WarehouseMemo[best.StationID], now, best.Value)
	st.Data.WarehouseMemo = pruneWarehouseMemo(st.Data.WarehouseMemo, now)

	// Far enough away that a courier contract is cheaper than hauling? Only a posted contract ends the run:
	// the poster answers nothing posted, with no error, when the goods were under the courier floor or the wallet ran out.
	if m.shouldPostCourier(ctx, b, best) {
		if posted, err := m.issueCourierContract(ctx, b, best); err != nil {
			m.logBot(b.CharID, logInfo, "home",
				"wanted to post a courier for %s but: %v — hauling instead",
				m.stationLabel(ctx, best.StationID), err)
		} else if posted > 0 {
			return true, saveState(ctx, m.db, st)
		}
	}

	st.Activity = ""
	st.Data.TargetStationID = best.StationID
	st.Data.TargetSystemID = best.SystemID
	note := ""
	if best.FleetOre {
		note = ", including a mining fleet's haul it never disposed of"
	}
	m.logBot(b.CharID, logInfo, "home",
		"it has %d stack(s) worth about %.0f ISK%s sitting at %s (%d jumps) — going to clear them",
		best.Stacks, best.Value, note, m.stationLabel(ctx, best.StationID), best.Jumps)
	return true, m.beginTravel(ctx, b, best.SystemID, best.StationID, string(TaskWarehouse))
}

// warehouseArriveMu guards warehouseMissed: pilots that already docked somewhere other than the
// warehouse once, so the next miss gives up. It is in process only; a restart costs one more re-route.
var (
	warehouseArriveMu sync.Mutex
	warehouseMissed   = map[int64]bool{}
)

// warehouseStep is the far end: docked at the warehouse, run the sweeps, and
// remember whether they disposed of anything (warehouse_memo.go).
func (m *Mod) warehouseStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if b.StationID == 0 {
		if st.Data.TargetStationID == 0 {
			st.Activity, st.Phase = "", string(TaskIdle)
			st.Data.LegEndsAt = clock.Unix() + idleRetrySec
			return false, saveState(ctx, m.db, st)
		}
		return false, m.beginTravel(ctx, b, st.Data.TargetSystemID, st.Data.TargetStationID,
			string(TaskWarehouse))
	}
	target := st.Data.TargetStationID
	now := clock.Unix()
	maxDays := escrowSettingInt(ctx, m.db, warehouseNoopBackoffKey, warehouseNoopBackoffDefault)
	// Docked, but not at the warehouse: route on once, then give up and count
	// it as a visit that disposed of nothing.
	if target != 0 && b.StationID != target {
		warehouseArriveMu.Lock()
		missed := warehouseMissed[b.CharID]
		warehouseMissed[b.CharID] = !missed
		warehouseArriveMu.Unlock()
		if !missed {
			return false, m.beginTravel(ctx, b, st.Data.TargetSystemID, target, string(TaskWarehouse))
		}
		if st.Data.WarehouseMemo != nil {
			st.Data.WarehouseMemo[target] = warehouseMemoAfter(st.Data.WarehouseMemo[target], now, false, maxDays)
		}
		st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
		st.Data.LegEndsAt = now + idleRetrySec
		return false, saveState(ctx, m.db, st)
	}
	warehouseArriveMu.Lock()
	delete(warehouseMissed, b.CharID)
	warehouseArriveMu.Unlock()
	// A deep metal its corporation bids for a few jumps away goes there first (corpdeepcarry.go). The
	// errand takes the pass, and the rest of the hoard waits for the next run.
	if started, err := m.startDeepCarry(ctx, b); started || err != nil {
		return false, err
	}
	before := m.hangarFingerprint(ctx, b.CharID, b.StationID)
	if m.sweepHangarNow(ctx, b) {
		// The meta run took the pass and the pilot is on its way to a hub with the graded half. Stamping a
		// phase over that would strand it; the memo is written on the next visit.
		return false, nil
	}
	disposed := !hangarFingerprintEqual(before, m.hangarFingerprint(ctx, b.CharID, b.StationID))
	memo := st.Data.WarehouseMemo
	if memo == nil {
		memo = map[int64]warehouseMemo{}
	}
	memo[b.StationID] = warehouseMemoAfter(memo[b.StationID], now, disposed, maxDays)
	if !disposed {
		m.logBot(b.CharID, logInfo, "home",
			"nothing here would sell or reprocess — leaving %s alone for a while", m.stationLabel(ctx, b.StationID))
	}
	st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
	st.Data.WarehouseMemo = pruneWarehouseMemo(memo, now)
	st.Data.LegEndsAt = now + m.stationDwell(b)
	return false, saveState(ctx, m.db, st)
}

// hangarFingerprint is the pilot's packaged goods at one station in the survey's categories, type to units.
func (m *Mod) hangarFingerprint(ctx context.Context, charID, stationID int64) map[int64]int64 {
	out := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity) FROM entity e
		  JOIN invTypes t  ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4
		   AND g.categoryID IN (`+warehouseSurveyCategories+`)
		 GROUP BY e.typeID`, charID, stationID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t, n int64
		if rows.Scan(&t, &n) == nil {
			out[t] = n
		}
	}
	return out
}

// sweepHangarNow runs every hangar sweep against the dock the pilot is standing in, ignoring their
// per-pilot hourly throttles, since a pilot that flew here to empty a hangar has earned the pass. The order
// is homeshop.go's: the meta run precedes the strays sweep (no grade test) and the ore sweep precedes the
// mineral sweep (it refines). It reports whether a sweep sent the pilot elsewhere; the caller then stamps no phase.
func (m *Mod) sweepHangarNow(ctx context.Context, b *botRun) bool {
	clearHangarSweepThrottles(b.CharID)

	if m.hasFleetOre(ctx, b.CharID, b.StationID) {
		m.disposeFleetOre(ctx, b)
	}
	if took, err := m.metaSellRun(ctx, b); err != nil {
		rlogf("rolebots: warehouse meta run char=%d: %v", b.CharID, err)
	} else if took {
		return true
	}
	m.sellHangarStrays(ctx, b)
	m.resellSurplusPrints(ctx, b)
	m.sellHangarOre(ctx, b)
	m.sellHangarMinerals(ctx, b)
	m.liquidateForCash(ctx, b)
	return false
}

// clearHangarSweepThrottles drops this pilot's stamp in each sweep's own map. Each sweep owns its own
// cadence, so no shared map or force flag fits; this is the one caller that forgets all four at once.
func clearHangarSweepThrottles(charID int64) {
	hangarStrayMu.Lock()
	delete(hangarStrayAt, charID)
	hangarStrayMu.Unlock()

	hangarOreMu.Lock()
	delete(hangarOreAt, charID)
	hangarOreMu.Unlock()

	hangarMineralMu.Lock()
	delete(hangarMineralAt, charID)
	hangarMineralMu.Unlock()

	liquidateMu.Lock()
	delete(liquidateAt, charID)
	liquidateMu.Unlock()
}

// detailWarehouse is the bot board's second line for the run.
func detailWarehouse(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.TargetStationID == 0 {
		return "clearing a hangar it left behind"
	}
	return "clearing its hangar at " + m.stationLabel(ctx, st.Data.TargetStationID)
}

func init() {
	registerCharForget("warehouseAt", func(id int64) {
		warehouseMu.Lock()
		delete(warehouseAt, id)
		warehouseMu.Unlock()
		warehouseArriveMu.Lock()
		delete(warehouseMissed, id)
		warehouseArriveMu.Unlock()
	})
}