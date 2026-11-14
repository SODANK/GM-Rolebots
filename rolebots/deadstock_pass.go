package rolebots

// The dead-stock passes that run after the stray sweep (strays.go): shelved
// stock no trader came back for, trade goods no ledger owns, and the minerals
// the reprocessing made. All run docked, at the pilot's own station, inside
// the same per-visit budget (deadstock.go).

import (
	"context"

	evedb "gurimoon/db"
)

// leaseGroupID is invGroups "Lease": Starbase Charters, bought only for a corporation's towers.
const leaseGroupID = 652

// disposeShelvedStock runs the verdict over the pilot's shelved modules and charges at this
// station. pilotStock prunes and heals the ledger first. A row with a real cost younger than
// costedAgeDays is still a trader's inventory and is left to tradeStockPlan.
func (m *Mod) disposeShelvedStock(ctx context.Context, b *botRun, cfg deadStockConfig, bg *deadStockBudget,
	reserve *gearReserve, got map[int64]int64, tally *deadStockOutcome) {

	stock, err := m.pilotStock(ctx, b.CharID)
	if err != nil || !bg.ok() {
		return
	}
	now := clock.Unix()
	for _, r := range stock {
		if !bg.ok() {
			return
		}
		if r.StationID != b.StationID {
			continue
		}
		if r.UnitCost > 0 && now-r.AcquiredAt < cfg.CostedAgeDays*86400 {
			continue
		}
		var itemID, qty, groupID, catID int64
		var base float64
		if m.db.QueryRowContext(ctx, `
			SELECT e.itemID, e.quantity, t.groupID, g.categoryID, COALESCE(t.basePrice, 0)
			  FROM entity e
			  JOIN invTypes t ON t.typeID = e.typeID
			  JOIN invGroups g ON g.groupID = t.groupID
			 WHERE e.ownerID = ? AND e.locationID = ? AND e.typeID = ? AND e.flag = 4 AND e.singleton = 0
			 ORDER BY e.quantity DESC LIMIT 1`, b.CharID, b.StationID, r.TypeID).
			Scan(&itemID, &qty, &groupID, &catID, &base) != nil {
			continue
		}
		if catID != catModule && catID != catCharge {
			continue
		}
		units := min(r.Units, qty)
		f := m.deadStockFactsFor(ctx, b, cfg, dsGear, r.TypeID, groupID, catID, units,
			reserve.of(r.TypeID, catID, groupID), m.strayAsk(ctx, b, r.TypeID, base))
		f.NoCost = r.UnitCost <= 0
		before := len(tally.Reloc)
		if n := m.applyDeadStock(ctx, b, bg, deadStockVerdict(f), r.TypeID, itemID, r.TypeName, got, tally); n > 0 {
			m.takeStock(ctx, b.CharID, b.StationID, r.TypeID, n)
			if len(tally.Reloc) > before {
				tally.Reloc[len(tally.Reloc)-1].FromLedger = true
			}
		}
	}
}

// ammoRelocateMaxJumps bounds how far a courier carries dead ammunition to a
// buyer, and ammoRelocateBuyers how many of the week's buying stations are
// routed to choose between.
const (
	ammoRelocateMaxJumps = 20
	ammoRelocateBuyers   = 5
)

// relocateDeadAmmo couriers the ammunition this pass set aside to the station that bought the
// most of it last week, discounted by distance (mktHistory Buyers), with one courier per
// destination (postCourierForStation). What no courier takes goes back on the stock ledger.
func (m *Mod) relocateDeadAmmo(ctx context.Context, b *botRun, tally *deadStockOutcome) {
	if len(tally.Reloc) == 0 {
		return
	}
	hist := m.marketHistory(ctx)
	bound := map[int64][]relocItem{} // destination station to stacks
	var unsent []relocItem
	for _, it := range tally.Reloc {
		dest := m.ammoBuyerFor(ctx, b, hist, it.TypeID)
		if dest == 0 {
			unsent = append(unsent, it)
			continue
		}
		bound[dest] = append(bound[dest], it)
	}
	for dest, items := range bound {
		ids := make([]int64, len(items))
		for i, it := range items {
			ids[i] = it.ItemID
		}
		posted, err := m.postCourierForStation(ctx, b, b.StationID, b.SystemID, dest, ids)
		if err != nil || posted == 0 {
			unsent = append(unsent, items...)
			continue
		}
		tally.Relocated += len(items)
		m.logBot(b.CharID, logInfo, "deadstock",
			"couriered %d stack(s) of ammunition nobody in this region buys to %s, where it sells",
			len(items), m.stationLabel(ctx, dest))
	}
	for _, it := range unsent {
		m.addStock(ctx, b.CharID, b.StationID, b.SystemID, it.TypeID, it.Name, it.Units, 0)
	}
	tally.Reloc = nil
}

// ammoBuyerFor is the station a courier should take one type of ammunition
// to: of the stations that bought it in the last week, the one with the most
// units per (1 + jump cost × jumps), within ammoRelocateMaxJumps. 0 for none.
func (m *Mod) ammoBuyerFor(ctx context.Context, b *botRun, hist *mktHistory, typeID int64) int64 {
	best, bestScore := int64(0), 0.0
	for i, buyer := range hist.Buyers[typeID] {
		if i >= ammoRelocateBuyers {
			break
		}
		if buyer.StationID == b.StationID {
			continue
		}
		sys := m.stationSystemOf(ctx, buyer.StationID)
		if sys == 0 {
			continue
		}
		j, err := m.jumpsBetween(ctx, b, b.SystemID, sys)
		if err != nil || j <= 0 || j > ammoRelocateMaxJumps {
			continue
		}
		if s := float64(buyer.Units) / (1 + warehouseJumpCost*float64(j)); s > bestScore {
			best, bestScore = buyer.StationID, s
		}
	}
	return best
}

// sellHangarCommodities disposes of trade goods (category 17) in this hangar that no ledger
// owns: leftovers of a buy, a partial sale, an abandoned run or a corporation supply buy. A
// trade good is never reprocessed: it sells into a bid in reach, lists where the region buys,
// or goes on the ledger. A Starbase Charter goes to the corporation's office instead.
func (m *Mod) sellHangarCommodities(ctx context.Context, b *botRun, cfg deadStockConfig, bg *deadStockBudget,
	tally *deadStockOutcome) {

	if !bg.ok() {
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, t.typeName, e.quantity, COALESCE(t.basePrice, 0), t.groupID
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 17
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND e.singleton = 0
		  AND NOT EXISTS (SELECT 1 FROM rolebot_trade_stock ts
			WHERE ts.characterID = e.ownerID AND ts.stationID = ? AND ts.typeID = e.typeID)`,
		b.CharID, b.StationID, b.StationID)
	if err != nil {
		rlogf("rolebots: hangar commodities char=%d: %v", b.CharID, err)
		return
	}
	type lot struct {
		itemID, typeID, qty, groupID int64
		name                         string
		base                         float64
	}
	var lots []lot
	for rows.Next() {
		var l lot
		if rows.Scan(&l.itemID, &l.typeID, &l.name, &l.qty, &l.base, &l.groupID) == nil {
			lots = append(lots, l)
		}
	}
	rows.Close()
	if len(lots) == 0 {
		return
	}
	reserved := m.projectReservations(ctx, b)
	shelved := 0
	for _, l := range lots {
		if !bg.ok() {
			break
		}
		units := l.qty - reserved[l.typeID]
		if units <= 0 {
			continue
		}
		if l.groupID == leaseGroupID && m.charterToCorp(ctx, b, l.typeID, units) {
			bg.spend()
			continue
		}
		f := m.deadStockFactsFor(ctx, b, cfg, dsCommodity, l.typeID, l.groupID, 17, units, 0,
			m.strayAsk(ctx, b, l.typeID, l.base))
		if n := m.applyDeadStock(ctx, b, bg, deadStockVerdict(f), l.typeID, l.itemID, l.name, nil, tally); n > 0 {
			units -= n
		}
		if units > 0 {
			m.addStock(ctx, b.CharID, b.StationID, b.SystemID, l.typeID, l.name, units, 0)
			shelved++
		}
	}
	if shelved > 0 {
		m.logBot(b.CharID, logInfo, "deadstock",
			"put %d lot(s) of trade goods nobody had a record of on its stock ledger — a trader can come back for them", shelved)
	}
}

// charterToCorp hands Starbase Charters to the pilot's corporation's office at this station,
// where its towers' fuel runs draw from. It reports whether it did.
func (m *Mod) charterToCorp(ctx context.Context, b *botRun, typeID, units int64) bool {
	if b.CorpID == 0 {
		return false
	}
	if npc, err := isNPCCorp(ctx, m.db, b.CorpID); err != nil || npc {
		return false // an NPC corporation has no towers to fuel
	}
	officeID, _, err := evedb.GetCorpOfficeAt(ctx, m.db, b.CorpID, b.StationID)
	if err != nil || officeID == 0 || officeID == b.StationID {
		return false
	}
	moved, err := m.moveUnits(ctx, b.CharID, b.StationID, flagHangar, typeID, units, officeID, int64(corpDivMaterials), b.CorpID)
	if err != nil || moved <= 0 {
		return false
	}
	m.logBot(b.CharID, logInfo, "deadstock", "handed %d× %s to its corporation's office here — towers burn them",
		moved, m.typeNameOr(ctx, typeID))
	return true
}

// listDeadStockMinerals puts what this pass reprocessed on the market through the ordinary
// mineral door (refinelist.go listRefinedMinerals). A managed corporation's member keeps
// Zydrine and Megacyte in the hangar for the deep carry (corpdeep.go).
func (m *Mod) listDeadStockMinerals(ctx context.Context, b *botRun, got map[int64]int64) {
	if len(got) == 0 {
		return
	}
	if _, managed := managedCEO(b.CorpID); managed {
		for t := range got {
			if isDeepMineral(t) {
				delete(got, t)
			}
		}
	}
	if len(got) == 0 {
		return
	}
	if _, _, _, err := m.listRefinedMinerals(ctx, b, got, m.projectReservations(ctx, b), mineralHoldMaxSec); err != nil {
		rlogf("rolebots: dead stock minerals char=%d: %v", b.CharID, err)
	}
}

// logDeadStock is the pass's one summary line.
func (m *Mod) logDeadStock(b *botRun, t deadStockOutcome) {
	if t.Reprocessed+t.Sold+t.Listed == 0 {
		return
	}
	m.logBot(b.CharID, logInfo, "deadstock",
		"cleared dead stock — %d stack(s) reprocessed (%d units), %d sold into bids in reach, %d listed",
		t.Reprocessed, t.UnitsScrapped, t.Sold, t.Listed)
}
