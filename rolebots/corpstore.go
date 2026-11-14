package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"

	evedb "gurimoon/db"
)

// Where the factory's output goes: the ladder.
//
// Finished goods land in the Production division and the production power routes them
// each think, top first: (1) ship replacement (corpsrp.go), a hull owed to a member who
// lost one flying for it; (2) the company store, what members use (read off their own
// losses) moved to the Company Store division and sold at cost (rolebot_corp_cost) plus
// the charter's markup as a public corporation sell order; (3) the market, listed at
// the factory at market value (corptraderef.go) and never below cost. Output unsold past
// corpStockAgedSec is walked to just under its region's trades (corpstock.go). The
// store's targets feed the work board as internal demand (storeDemand).

const (
	corpOrderStock   = "stock"
	corpOrderProduct = "product"
	corpOrderStore   = "store"

	storeLossWindowSec = 30 * 86400
	storeTargetFrac    = 0.5 // keep half a month of losses on the shelf
	storeMaxTypes      = 10
	productListDays    = 30
	productListPerPass = 6
	storeRelistSec     = 24 * 3600
)

// corpUnitCost is what one unit of a product cost the corporation to make,
// 0 when it has never built one.
func (m *Mod) corpUnitCost(ctx context.Context, corpID, typeID int64) float64 {
	var units int64
	var cost float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT units, cost FROM rolebot_corp_cost WHERE corporationID = ? AND typeID = ?`,
		corpID, typeID).Scan(&units, &cost)
	if units <= 0 {
		return 0
	}
	return cost / float64(units)
}

// storeTarget is one type the store keeps, and how many.
type storeTarget struct {
	TypeID, BPTypeID int64
	Target           int64
}

// storeTargets reads a month of member losses into shelf targets, for types
// the library can build.
func (m *Mod) storeTargets(ctx context.Context, corpID int64) []storeTarget {
	cut := clock.Unix() - storeLossWindowSec
	rows, err := m.db.QueryContext(ctx, `
		SELECT x.typeID, bt.blueprintTypeID, SUM(x.n) AS lost
		  FROM (
		        SELECT k.shipTypeID AS typeID, 1 AS n
		          FROM killmails k
		         WHERE k.victimCorpID = ? AND k.killTime >= ? AND k.isPod = 0
		        UNION ALL
		        SELECT i.typeID, i.quantity
		          FROM killmails k
		          JOIN killmail_items i ON i.killID = k.killID
		         WHERE k.victimCorpID = ? AND k.killTime >= ?
		       ) x
		  JOIN invBlueprintTypes bt ON bt.productTypeID = x.typeID
		  JOIN rolebot_corp_library l ON l.corporationID = ? AND l.bpTypeID = bt.blueprintTypeID
		 GROUP BY x.typeID, bt.blueprintTypeID
		 ORDER BY lost DESC
		 LIMIT ?`, corpID, cut, corpID, cut, corpID, storeMaxTypes)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []storeTarget
	for rows.Next() {
		var t storeTarget
		var lost float64
		if rows.Scan(&t.TypeID, &t.BPTypeID, &lost) == nil {
			t.Target = int64(math.Ceil(lost * storeTargetFrac))
			if t.Target > 0 {
				out = append(out, t)
			}
		}
	}
	return out
}

// corpStockOf is how many of a type the corporation holds at an office in
// the given divisions, plus what its open sell orders of that kind still carry.
func (m *Mod) corpStockOf(ctx context.Context, corpID, officeID, typeID int64, kind string, flags ...int64) int64 {
	var n int64
	for _, f := range flags {
		q, _ := evedb.GetMaterialQuantityAt(ctx, m.db, corpID, officeID, f, typeID)
		n += q
	}
	var listed int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(o.volRemaining), 0) FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.typeID = ? AND r.bid = 0 AND r.kind = ? AND o.orderState = 1`,
		corpID, typeID, kind).Scan(&listed)
	return n + listed
}

// storeDemand is what the store is short of, as internal demand for the
// work board.
func (m *Mod) storeDemand(ctx context.Context, corpID int64) []buildPlanRow {
	_, officeID := m.corpFactoryOffice(ctx, corpID)
	if officeID == 0 {
		return nil
	}
	var out []buildPlanRow
	for _, t := range m.storeTargets(ctx, corpID) {
		have := m.corpStockOf(ctx, corpID, officeID, t.TypeID, corpOrderStore, corpDivStore, corpDivProduct)
		short := t.Target - have
		if short <= 0 {
			continue
		}
		portion, _ := evedb.GetTypePortionSize(ctx, m.db, t.TypeID)
		portion = max(portion, 1)
		out = append(out, buildPlanRow{BPTypeID: t.BPTypeID, ProductTypeID: t.TypeID,
			Wanted: int((short + portion - 1) / portion), Priority: 50, Purpose: "store"})
	}
	return out
}

// ceoOutput routes the Production division down the ladder.
func (m *Mod) ceoOutput(ctx context.Context, c *corpCEO, ch Charter) error {
	stationID, officeID := m.corpFactoryOffice(ctx, c.CorpID)
	if stationID == 0 {
		return nil
	}
	// 1. Replacements owed.
	if err := m.ceoSRP(ctx, c, ch, stationID, officeID); err != nil {
		return err
	}
	// 2. Stock the store up to its targets, then list the store.
	for _, t := range m.storeTargets(ctx, c.CorpID) {
		inStore := m.corpStockOf(ctx, c.CorpID, officeID, t.TypeID, corpOrderStore, corpDivStore)
		if move := t.Target - inStore; move > 0 {
			m.moveCorpStock(ctx, c.CorpID, officeID, t.TypeID, corpDivProduct, corpDivStore, move)
		}
	}
	ceo, err := m.loadRun(ctx, c.CharID)
	if err != nil || ceo == nil {
		return nil
	}
	listed := 0
	// A capital program's components are never for sale: they sit in
	// Production until the yard moves them to Materials (corpprogram.go).
	programParts := m.programPartTypes(ctx, c.CorpID)
	// Durables a counter elsewhere pulls harder for go there on the haul
	// board (corpoutlet.go); the factory leaves those lots for the carrier.
	factoryStation := stationID
	outbound := m.ceoOutletHauls(ctx, c, stationID, officeID)
	listWhere := func(stationID, officeID, flag int64, kind, cats string) error {
		rows, err := m.db.QueryContext(ctx, `
			SELECT e.itemID, e.typeID, e.quantity
			  FROM entity e
			  JOIN invTypes t ON t.typeID = e.typeID
			  JOIN invGroups g ON g.groupID = t.groupID
			 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ? AND e.quantity > 0
			   AND `+cats+`
			 ORDER BY e.typeID, e.quantity DESC`, c.CorpID, officeID, flag)
		if err != nil {
			return err
		}
		type lot struct{ itemID, typeID, qty int64 }
		var lots []lot
		seen := map[int64]bool{}
		leaving := stationID == factoryStation && flag == corpDivProduct
		for rows.Next() {
			var l lot
			if rows.Scan(&l.itemID, &l.typeID, &l.qty) == nil && !seen[l.typeID] && !programParts[l.typeID] &&
				!(leaving && outbound[l.typeID]) {
				seen[l.typeID] = true
				lots = append(lots, l)
			}
		}
		rows.Close()
		sort.Slice(lots, func(i, j int) bool { return lots[i].typeID < lots[j].typeID })
		for _, l := range lots {
			if listed >= productListPerPass {
				return nil
			}
			placer := m.corpOrderPlacerFor(ctx, c.CorpID, c.CharID, kind)
			if placer == 0 {
				addCorpOrderPressure(c.CorpID, len(lots)-listed)
				return nil
			}
			var open int
			_ = m.db.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM rolebot_corp_orders
				 WHERE corporationID = ? AND stationID = ? AND typeID = ? AND bid = 0 AND kind = ?`,
				c.CorpID, stationID, l.typeID, kind).Scan(&open)
			if open > 0 {
				continue
			}
			// The store's order is public (the client does not limit a sell order to members), so
			// a type is restocked at most once a day, capping what an outsider can take.
			if kind == corpOrderStore &&
				clock.Unix()-m.lastCEOActionOn(ctx, c.CorpID, "store", l.typeID) < storeRelistSec {
				continue
			}
			cost := m.corpUnitCost(ctx, c.CorpID, l.typeID)
			// What the type has really traded for where it is listed, not the
			// book's stale asks (corptraderef.go).
			value := m.productTradeValue(ctx, l.typeID, m.stationRegionID(ctx, stationID))
			price := corpListPrice(kind, cost, value, ch.StoreMarkup)
			// The store sells to members at cost-plus; everything else is priced for the market and
			// never under the regional NPC band (npcanchor.go), since TypeValueOf reads bot-dust asks.
			if kind != corpOrderStore {
				price, _ = liftToAnchor(price, m.askAnchor(ctx, stationID, l.typeID),
					npcAnchorFrac(defaultTuning()))
			}
			if price <= 0 {
				continue
			}
			out, err := evedb.PlaceSellOrder(ctx, m.db, placer, stationID, l.typeID, l.itemID, price, l.qty,
				-1, 1, productListDays, true)
			if err != nil {
				return fmt.Errorf("list corp %s: %w", m.typeNameOr(ctx, l.typeID), err)
			}
			if out.ErrorMsg != "" {
				continue
			}
			listed++
			line := lineSales
			if kind == corpOrderStore {
				line = lineStore
			}
			if filled := out.FilledQty; filled > 0 {
				v := 0.0
				for _, f := range out.Fills {
					v += f.Price * float64(f.Quantity)
				}
				m.corpBook(ctx, c.CorpID, line, v, l.typeID, fmt.Sprintf("%d× %s", filled, m.typeNameOr(ctx, l.typeID)))
				m.creditSalesSeat(ctx, c.CorpID, placer, v)
			}
			if out.RestingOrderID != 0 {
				if _, err := m.db.ExecContext(ctx, `
					INSERT INTO rolebot_corp_orders
						(orderID, corporationID, stationID, typeID, bid, price, volSeen, placedAt, kind, placedBy)
					VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
					out.RestingOrderID, c.CorpID, stationID, l.typeID, price, l.qty-out.FilledQty,
					clock.Unix(), kind, placer); err != nil {
					return err
				}
			}
			what, verb := "on the market", "list"
			if kind == corpOrderStore {
				what, verb = "in the company store, at cost", "store"
			}
			m.ceoLog(ctx, c, powerProduction, verb, l.typeID, price*float64(l.qty),
				"put %d× %s up %s for %s each", l.qty, m.typeNameOr(ctx, l.typeID), what, iskString(price))
		}
		return nil
	}
	list := func(stationID, officeID, flag int64, kind string) error {
		return listWhere(stationID, officeID, flag, kind, "g.categoryID NOT IN (9, 25)")
	}
	if err := list(stationID, officeID, corpDivStore, corpOrderStore); err != nil {
		return err
	}
	if err := list(stationID, officeID, corpDivProduct, corpOrderProduct); err != nil {
		return err
	}
	// Market builds are Sales' orders and land on Sales' Trade shelf (corprequest.go). A
	// trade house's desk lists that shelf itself (ceoListTradeStock); elsewhere this pass does.
	salesShelf := true
	if facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID); err == nil && facts != nil &&
		tradeDeskTagged(facts.Policy.Tags) {
		salesShelf = false
	}
	if salesShelf {
		if err := list(stationID, officeID, corpDivTrade, corpOrderProduct); err != nil {
			return err
		}
	}
	// What another Industry base built sits in its own Production division and is listed
	// from there (corpbuildbases.go). Replacements and the store are built at the factory.
	bases := map[int64]bool{stationID: true}
	for _, st := range m.industryBaseStations(ctx, c.CorpID) {
		if st == stationID {
			continue
		}
		bases[st] = true
		office, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, c.CorpID, st)
		if office == 0 || office == st {
			continue
		}
		if err := list(st, office, corpDivProduct, corpOrderProduct); err != nil {
			return err
		}
		if salesShelf {
			if err := list(st, office, corpDivTrade, corpOrderProduct); err != nil {
				return err
			}
		}
	}
	// Durables the haul board carried to an outlet office (corpoutlet.go) go on the market
	// there. Durables only: another office's Production division can hold staged tower output.
	for _, o := range m.corpOfficeSites(ctx, c.CorpID) {
		if bases[o.StationID] {
			continue
		}
		if err := listWhere(o.StationID, o.OfficeID, corpDivProduct, corpOrderProduct,
			fmt.Sprintf("g.categoryID IN (%d, %d, %d)", catModule, catCharge, catDrone)); err != nil {
			return err
		}
	}
	return nil
}

// moveCorpStock moves up to qty of a type between two divisions of one
// office, whole stacks first, splitting the last.
func (m *Mod) moveCorpStock(ctx context.Context, corpID, officeID, typeID, from, to, qty int64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, quantity, singleton FROM entity
		 WHERE ownerID = ? AND locationID = ? AND flag = ? AND typeID = ?
		 ORDER BY quantity`, corpID, officeID, from, typeID)
	if err != nil {
		return
	}
	type st struct {
		id, qty   int64
		singleton bool
	}
	var stacks []st
	for rows.Next() {
		var s st
		if rows.Scan(&s.id, &s.qty, &s.singleton) == nil {
			stacks = append(stacks, s)
		}
	}
	rows.Close()
	for _, s := range stacks {
		if qty <= 0 {
			return
		}
		if s.qty <= qty || s.singleton {
			if _, err := m.db.ExecContext(ctx, `UPDATE entity SET flag = ? WHERE itemID = ?`, to, s.id); err == nil {
				qty -= s.qty
			}
			continue
		}
		tx, err := m.db.BeginTx(ctx, nil)
		if err != nil {
			return
		}
		if err := evedb.ReduceEntityQuantity(ctx, tx, s.id, qty); err != nil {
			tx.Rollback()
			return
		}
		id, err := evedb.CreateItem(ctx, tx, typeID, corpID, officeID, int(qty))
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE entity SET flag = ?, quantity = ? WHERE itemID = ?`, to, qty, id)
		}
		if err != nil {
			tx.Rollback()
			return
		}
		_ = tx.Commit()
		return
	}
}

// lastCEOActionOn is when the CEO last took a decision of this verb about
// one subject (a type, a pilot), 0 for never.
func (m *Mod) lastCEOActionOn(ctx context.Context, corpID int64, verb string, subjectID int64) int64 {
	var at int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(at), 0) FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND verb = ? AND subjectID = ?`, corpID, verb, subjectID).Scan(&at)
	return at
}

// corpListPrice is what the corporation asks for one unit: the company store
// sells at cost plus the markup (or just under market plus the markup for a
// type it has no cost record for); the market listing is market value, never
// below what the unit cost to make. Pure, for the test.
func corpListPrice(kind string, cost, value, markup float64) float64 {
	price := math.Max(value, cost*1.02)
	if kind == corpOrderStore {
		price = cost * (1 + markup)
		if cost <= 0 {
			price = value * (1 + markup) * 0.9
		}
	}
	return math.Round(price*100) / 100
}
