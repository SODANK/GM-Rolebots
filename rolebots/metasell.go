package rolebots

// The meta run: a hangar of named gear taken somewhere it is worth something.
//
// sellHangarStrays (strays.go) sells at the dock the pilot stands in, which suits
// plain tech one but not named, tech two or faction gear. The shopping pass grades
// the hangar first (attr 633) and flies gear worth the distance to the best trade
// dock in reach (lootMarketFor, as the hunter's fence does). It runs before
// sellHangarStrays, which filters on category alone. A trip and a buy cannot both
// own the pass, and finishShopping is not called on the way out, since stamping
// HomeShoppedAt would cool down a visit never made. The gear rides on the
// pilot's rolebot_loot_hold.

import (
	"context"
	"fmt"
	"sort"
)

const (
	// metaRunMinMeta is the grade floor: 1 and up, so plain tech one keeps going to
	// the local ladder.
	metaRunMinMeta = 1.0

	// metaRunMinValue is what a hangar of gear must be worth, after the distance
	// discount (lootHubJumpCost), before a pilot flies it. It sits far above
	// lootListMinValue because it decides a whole round trip.
	metaRunMinValue = 500_000.0

	// metaRunMaxLines bounds one trip. A pilot with a museum in its hangar
	// clears it over several visits; the next pass picks up where this stopped.
	metaRunMaxLines = 12
)

// metaStray is one gradeable hangar stack.
type metaStray struct {
	itemID   int64
	typeID   int64
	typeName string
	qty      int64
	volume   float64
	value    float64
}

// metaRunWorthIt is the trip test, pure so the harness can pin it: a marginal
// haul must fail at the far end of the map and pass at the near one.
func metaRunWorthIt(value float64, jumps int) bool {
	if jumps < 0 {
		return false
	}
	return value/(1+lootHubJumpCost*float64(jumps)) >= metaRunMinValue
}

// metaSellRun takes the shopping pass when the hangar holds enough graded gear
// to be worth a trip. Reports whether it took it.
func (m *Mod) metaSellRun(ctx context.Context, b *botRun) (bool, error) {
	if b.StationID == 0 || b.ShipID == 0 {
		return false, nil
	}
	strays, err := m.gradedHangarStrays(ctx, b)
	if err != nil || len(strays) == 0 {
		return false, err
	}

	// Where it is worth going, and how far. With no hub in reach there is no trip;
	// the local ladder gets the gear.
	station, system, mkt := m.lootMarketFor(ctx, b)
	if station == 0 || station == b.StationID {
		return false, nil
	}
	jumps, err := m.jumpsBetween(ctx, b, b.SystemID, system)
	if err != nil {
		return false, nil
	}

	// Most valuable first, and only what the hold will take: a trip that leaves the
	// best stack behind for a cheap bulky one is wasted.
	sort.Slice(strays, func(i, j int) bool { return strays[i].value > strays[j].value })
	free, err := m.freeHoldM3(ctx, b)
	if err != nil {
		return false, err
	}
	var take []metaStray
	total := 0.0
	for _, s := range strays {
		if len(take) >= metaRunMaxLines {
			break
		}
		vol := s.volume * float64(s.qty)
		if vol > free {
			continue // the next one down may still fit
		}
		free -= vol
		total += s.value
		take = append(take, s)
	}
	if len(take) == 0 || !metaRunWorthIt(total, jumps) {
		return false, nil
	}

	// The gear moves before the pass is claimed. A run that reported handled without
	// emptying the hangar would be found again and the pilot would commute forever.
	moved := 0
	for _, s := range take {
		if err := m.moveHangarToCargo(ctx, b, s.typeID); err != nil {
			rlogf("rolebots: meta run load char=%d type=%d: %v", b.CharID, s.typeID, err)
			continue
		}
		m.noteLootTaken(ctx, b.CharID, s.typeID, s.typeName, s.qty)
		moved++
	}
	if moved == 0 {
		return false, nil
	}

	st := b.State
	st.Activity = ""
	st.Data.TargetStationID = station
	st.Data.TargetSystemID = system
	hubName := ""
	if mkt != nil {
		hubName = fmt.Sprintf(" — %s, %s traded there lately", mkt.Name, iskShort(mkt.Liquidity))
	}
	m.logBot(b.CharID, logInfo, "market",
		"taking %d stack(s) of named gear worth about %.0f ISK to %s%s (%d jumps)",
		moved, total, m.stationLabel(ctx, station), hubName, jumps)
	return true, m.beginTravel(ctx, b, system, station, string(TaskHomeSell))
}

// gradedHangarStrays is sellHangarStrays' enumeration with a grade. It grades by
// invmetatypes.metaGroupID first and attribute 633 only inside the tech-one band,
// since many faction and officer types have no attribute row (lootgrade.go). The
// NOT EXISTS clauses, from strays.go, skip hauls in the pipeline and trade stock.
func (m *Mod) gradedHangarStrays(ctx context.Context, b *botRun) ([]metaStray, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, t.typeName, e.quantity, COALESCE(t.volume, 0.01)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID IN (7, 8)
		LEFT JOIN invmetatypes mt ON mt.typeID = e.typeID
		LEFT JOIN dgmTypeAttributes a ON a.typeID = e.typeID AND a.attributeID = ?
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND e.singleton = 0
		  AND e.quantity > 0
		  AND (COALESCE(mt.metaGroupID, 0) > 1
		       OR COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DOUBLE), 0) >= ?)
		  AND NOT EXISTS (SELECT 1 FROM rolebot_loot_hold lh
			WHERE lh.characterID = e.ownerID AND lh.typeID = e.typeID AND lh.units > 0)
		  AND NOT EXISTS (SELECT 1 FROM rolebot_trade_stock ts
			WHERE ts.characterID = e.ownerID AND ts.stationID = ? AND ts.typeID = e.typeID)`,
		attrMetaLevel, b.CharID, b.StationID, metaRunMinMeta, b.StationID)
	if err != nil {
		return nil, fmt.Errorf("graded hangar strays: %w", err)
	}
	defer rows.Close()
	var out []metaStray
	for rows.Next() {
		var s metaStray
		if err := rows.Scan(&s.itemID, &s.typeID, &s.typeName, &s.qty, &s.volume); err != nil {
			return nil, fmt.Errorf("graded hangar strays scan: %w", err)
		}
		// Priced as the hunter's fence prices salvage (the going rate bent by the
		// pilot's markup), so the trip test and the eventual ask agree.
		if p := m.lootAskPrice(ctx, b, s.typeID); p > 0 {
			s.value = p * float64(s.qty)
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

// homeSellStep is the far end of the run: sell what was flown, then idle. It does
// not call finishShopping: this is a disposal trip, not a completed visit, and the
// pilot goes home on the next plan for the ammunition buy it skipped.
func (m *Mod) homeSellStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if b.StationID == 0 {
		if st.Data.TargetStationID == 0 {
			// Nowhere to go and cargo aboard: the loot ledger the cargo rides on takes over
			// from here.
			st.Activity, st.Phase = "", string(TaskIdle)
			st.Data.LegEndsAt = clock.Unix() + idleRetrySec
			return false, saveState(ctx, m.db, st)
		}
		return false, m.beginTravel(ctx, b, st.Data.TargetSystemID, st.Data.TargetStationID,
			string(TaskHomeSell))
	}
	if _, _, _, _, err := m.sellLootLedgerHere(ctx, b, "market"); err != nil {
		return false, err
	}
	m.clearLootLedger(ctx, b.CharID)
	st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return false, saveState(ctx, m.db, st)
}

// detailHomeSell is the bot board's second line for the run.
func detailHomeSell(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.TargetStationID == 0 {
		return "selling named gear"
	}
	return "selling named gear at " + m.stationLabel(ctx, st.Data.TargetStationID)
}
