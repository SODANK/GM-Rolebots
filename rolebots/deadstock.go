package rolebots

// Dead stock is goods with no buyer, and the one rule for what to do with them.
// A module, charge, packaged hull or trade good that nobody bids for is not filed on
// rolebot_trade_stock to wait: filed stock is hidden from the other sweeps (the NOT EXISTS
// clauses in strays.go and metasell.go) and only a trader reads the ledger again.
// deadStockVerdict is the one decision, shared by every sweep that meets such a stack:
// sell into a bid in reach, list where the region buys, reprocess where nothing will take
// it, hold otherwise. Reprocessing reuses refineStack; minerals go out through
// listRefinedMinerals.
// Each visit is bounded by rolebots.deadstock.maxStacksPerVisit and the whole rule by
// rolebots.deadstock.enabled.

import (
	"context"
	"fmt"
	"math"
	"sync"

	evedb "gurimoon/db"
)

const (
	deadStockEnabledKey    = "rolebots.deadstock.enabled"
	deadStockMaxStacksKey  = "rolebots.deadstock.maxStacksPerVisit"
	deadStockBidFloorKey   = "rolebots.deadstock.bidFloorFrac"
	deadStockMinRefineKey  = "rolebots.deadstock.minRefineISK"
	deadStockAmmoMulKey    = "rolebots.deadstock.ammoReserveMul"
	deadStockModReserveKey = "rolebots.deadstock.moduleReserve"
	deadStockCostedAgeKey  = "rolebots.deadstock.costedAgeDays"

	deadStockMaxStacksDefault  = 10
	deadStockBidFloorDefault   = 0.5
	deadStockMinRefineDefault  = 500.0
	deadStockAmmoMulDefault    = 3
	deadStockModReserveDefault = 1
	deadStockCostedAgeDefault  = 14
)

// deadStockConfig holds the settings, read once per sweep.
type deadStockConfig struct {
	Enabled        bool
	MaxStacks      int
	BidFloorFrac   float64
	MinRefineISK   float64
	AmmoReserveMul int64
	ModuleReserve  int64
	CostedAgeDays  int64
}

func (m *Mod) deadStockSettings(ctx context.Context) deadStockConfig {
	return deadStockConfig{
		Enabled:        escrowSettingInt(ctx, m.db, deadStockEnabledKey, 1) != 0,
		MaxStacks:      max(escrowSettingInt(ctx, m.db, deadStockMaxStacksKey, deadStockMaxStacksDefault), 0),
		BidFloorFrac:   math.Max(escrowSettingFloat(ctx, m.db, deadStockBidFloorKey, deadStockBidFloorDefault), 0),
		MinRefineISK:   math.Max(escrowSettingFloat(ctx, m.db, deadStockMinRefineKey, deadStockMinRefineDefault), 0),
		AmmoReserveMul: int64(max(escrowSettingInt(ctx, m.db, deadStockAmmoMulKey, deadStockAmmoMulDefault), 0)),
		ModuleReserve:  int64(max(escrowSettingInt(ctx, m.db, deadStockModReserveKey, deadStockModReserveDefault), 0)),
		CostedAgeDays:  int64(max(escrowSettingInt(ctx, m.db, deadStockCostedAgeKey, deadStockCostedAgeDefault), 0)),
	}
}

// deadStockBudget is how many more stacks this visit may dispose of.
type deadStockBudget struct{ left int }

func (bg *deadStockBudget) ok() bool { return bg != nil && bg.left > 0 }
func (bg *deadStockBudget) spend() {
	if bg != nil {
		bg.left--
	}
}

// ── the verdict (pure) ──────────────────────────────────────────────────────

type dsKind uint8

const (
	dsGear      dsKind = iota // modules, charges, drones
	dsHull                    // a packaged ship
	dsCommodity               // a trade good: never reprocessed
)

type dsAction uint8

const (
	dsKeep dsAction = iota
	dsHold
	dsSellBid
	dsList
	dsReprocess
	dsRelocate // ammo the region does not buy but the cluster does: courier it to a buyer
)

// deadStockFacts is everything the verdict reads about one stack.
type deadStockFacts struct {
	Kind           dsKind
	Units, Reserve int64
	NeverRefine    bool // named or graded gear (meta level > 0 or metaGroup > 1): sold, never scrapped
	Ammo           bool // a charge: never scrapped, moved to where it sells
	Wanted         bool // the roster has failed to buy it (typeIsWanted)
	Bids           []reachBid
	BidFloor       float64 // a bid under this per unit counts as no buyer
	HistReady      bool
	RegionPerDay   float64
	ClusterPerDay  float64 // bought anywhere in the cluster, per day
	ListUnit       float64 // the ask it would list at (0 = none)
	CanList        bool    // a free order slot
	MinListISK     float64
	RefineUnit     float64 // mineral value of one unit reprocessed here
	PortionOK      bool    // at least one whole reprocessing portion
	MinRefineISK   float64
	// NoCost marks a stack with no cost basis (loot or a stray). Only such a stack may be
	// scrapped while it could still sell: a stack with a cost may come off an NPC shelf priced
	// under its metal, and refining it would mint minerals.
	NoCost bool
}

// deadStockListNet is what a listing keeps after tax and the broker fee.
const deadStockListNet = 0.97

// scrapBeatsListing reports whether a stack without a cost basis is worth more as metal
// than listed. Pure.
func scrapBeatsListing(f deadStockFacts, surplus int64) bool {
	return f.NoCost && f.PortionOK && f.ListUnit > 0 && f.RefineUnit > f.ListUnit*deadStockListNet &&
		float64(surplus)*f.RefineUnit >= f.MinRefineISK
}

// deadStockPlan is what to do with a stack.
type deadStockPlan struct {
	Act   dsAction
	Units int64
	Price float64
	Why   string
}

// reachSellPlan returns how many of units the bids can take at or above floor, and the
// price one immediate sell order needs: the lowest bid used, since the core fills every bid
// at or above an ask, best first. Bids are in bidsReachingStation's order. Pure.
func reachSellPlan(bids []reachBid, units int64, floor float64) (int64, float64) {
	take, price := int64(0), 0.0
	for _, b := range bids {
		if units <= 0 || b.Price < floor || b.Price <= 0 {
			break
		}
		if b.MinVolume > units {
			continue
		}
		n := min(b.Volume, units)
		if n <= 0 {
			continue
		}
		take += n
		units -= n
		price = b.Price
	}
	return take, price
}

// deadStockVerdict decides one stack, first match wins: keep the pilot's own needs; sell
// into a bid in reach; hold while market history is not ready; list what the roster is short
// of; scrap a cost-free stack worth more as metal; list what the region buys; move ammunition
// to a buyer; else reprocess unless it is ammunition, a trade good, named gear or too small. Pure.
func deadStockVerdict(f deadStockFacts) deadStockPlan {
	surplus := f.Units - f.Reserve
	if surplus <= 0 {
		return deadStockPlan{Act: dsKeep, Why: "its own fits and projects need it"}
	}
	if take, price := reachSellPlan(f.Bids, surplus, f.BidFloor); take > 0 {
		return deadStockPlan{Act: dsSellBid, Units: take, Price: price, Why: "a bid in reach takes it"}
	}
	if !f.HistReady {
		return deadStockPlan{Act: dsHold, Why: "the market history is not ready"}
	}
	if f.Wanted && f.CanList && f.ListUnit > 0 {
		return deadStockPlan{Act: dsList, Units: surplus, Price: f.ListUnit, Why: "the roster is short of it"}
	}
	if scrapBeatsListing(f, surplus) {
		return deadStockPlan{Act: dsReprocess, Units: surplus, Why: "its metal is worth more than a listing"}
	}
	if f.RegionPerDay > 0 {
		n := marketAbsorb(surplus, f.RegionPerDay)
		if f.CanList && f.ListUnit > 0 && n > 0 && float64(n)*f.ListUnit >= f.MinListISK {
			return deadStockPlan{Act: dsList, Units: n, Price: f.ListUnit, Why: "the region buys it"}
		}
		return deadStockPlan{Act: dsHold, Why: "the region buys it; it waits for a listing"}
	}
	switch {
	case f.Ammo && f.ClusterPerDay > 0:
		return deadStockPlan{Act: dsRelocate, Units: surplus, Why: "the region buys none; the cluster does"}
	case f.Ammo:
		return deadStockPlan{Act: dsHold, Why: "nobody anywhere buys it, and ammunition is never scrapped"}
	case f.Kind == dsCommodity:
		return deadStockPlan{Act: dsHold, Why: "no buyer here, and a trade good is never scrapped"}
	case f.NeverRefine:
		return deadStockPlan{Act: dsHold, Why: "named and graded gear is sold, never scrapped"}
	case f.RefineUnit <= 0 || !f.PortionOK:
		return deadStockPlan{Act: dsHold, Why: "nothing to reprocess it into"}
	case float64(surplus)*f.RefineUnit < f.MinRefineISK:
		return deadStockPlan{Act: dsHold, Why: "worth too little as metal"}
	}
	return deadStockPlan{Act: dsReprocess, Units: surplus, Why: "nobody buys it anywhere in reach"}
}

// ── facts from the database ─────────────────────────────────────────────────

// metaGroupCache is invMetaTypes.metaGroupID by type (0 = none, tech 1).
var metaGroupCache sync.Map

// typeMetaGroupOf returns a type's meta group: 1 tech 1, 2 tech 2, 3 storyline, 4 faction,
// 5 officer, 6 deadspace; 0 when invMetaTypes has no row.
func (m *Mod) typeMetaGroupOf(ctx context.Context, typeID int64) int64 {
	if v, ok := metaGroupCache.Load(typeID); ok {
		return v.(int64)
	}
	var g int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(metaGroupID), 0) FROM invMetaTypes WHERE typeID = ?`, typeID).Scan(&g)
	metaGroupCache.Store(typeID, g)
	return g
}

// metaLevelCache is dgmTypeAttributes metaLevel (633) by type.
var metaLevelCache sync.Map

// typeMetaLevelOf returns a type's meta level: 0 for a plain tech-one item, 1 to 4 for
// the named variants rats drop, higher for graded gear.
func (m *Mod) typeMetaLevelOf(ctx context.Context, typeID int64) int64 {
	if v, ok := metaLevelCache.Load(typeID); ok {
		return v.(int64)
	}
	var lvl float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(COALESCE(valueInt, valueFloat)), 0) FROM dgmTypeAttributes
		 WHERE typeID = ? AND attributeID = ?`, typeID, attrMetaLevel).Scan(&lvl)
	v := int64(lvl)
	metaLevelCache.Store(typeID, v)
	return v
}

// refineUnitValue is what one unit of a type is worth as metal reprocessed at the pilot's
// station: its materials at this pilot's yield, net of tax, at the minerals' basePrice
// (orerefine.go mineralPrices). It also reports whether units makes a whole portion.
func (m *Mod) refineUnitValue(ctx context.Context, b *botRun, typeID, groupID, units int64) (float64, bool) {
	mats, err := evedb.GetTypeMaterials(ctx, m.db, typeID)
	if err != nil || len(mats) == 0 {
		return 0, false
	}
	portion, err := evedb.GetTypePortionSize(ctx, m.db, typeID)
	if err != nil || portion <= 0 {
		portion = 1
	}
	hand := m.refineHandAt(ctx, b, b.StationID, groupID)
	y := refineEffYield(hand.StationEff, hand.Refining, hand.RefineryEff, hand.OreSkill) * (1 - hand.Tax)
	v := 0.0
	for _, mt := range mineralYield(mats) {
		v += float64(mt.Quantity) * y * m.basePriceCached(ctx, mt.MaterialTypeID)
	}
	return v / float64(portion), units >= portion
}

// deadStockFactsFor gathers the verdict's inputs for one stack at the pilot's station.
// listUnit is the ask the caller would list at (0 = it will not).
func (m *Mod) deadStockFactsFor(ctx context.Context, b *botRun, cfg deadStockConfig, kind dsKind,
	typeID, groupID, catID, units, reserve int64, listUnit float64) deadStockFacts {

	f := deadStockFacts{
		Kind: kind, Units: units, Reserve: reserve, Ammo: catID == catCharge,
		ListUnit: listUnit, MinListISK: lootListMinValue, MinRefineISK: cfg.MinRefineISK,
	}
	if bids, err := m.bidsReachingStation(ctx, b.StationID, typeID, b.CharID); err == nil {
		f.Bids = bids
	}
	hist := m.marketHistory(ctx)
	f.HistReady = hist.Ready
	f.RegionPerDay = hist.regionPerDay(m.stationRegionOf(ctx, b.StationID), typeID)
	f.ClusterPerDay = hist.globalPerDay(typeID)
	f.Wanted = m.typeIsWanted(ctx, typeID)
	f.CanList = listUnit > 0 && m.freeOrderSlots(ctx, b.CharID) > 0
	if kind != dsCommodity && !f.Ammo {
		f.NeverRefine = m.typeMetaGroupOf(ctx, typeID) > 1 || m.typeMetaLevelOf(ctx, typeID) > 0
		if !f.NeverRefine {
			f.RefineUnit, f.PortionOK = m.refineUnitValue(ctx, b, typeID, groupID, units-reserve)
		}
	}
	// The floor a bid must meet: half the metal value or, for what is never scrapped, half
	// what it trades for.
	ref := f.RefineUnit
	if ref <= 0 {
		refs := m.tradeRefsOf(ctx, typeID)
		ref, _ = pickTradeValue(refs.Region[m.stationRegionOf(ctx, b.StationID)], refs.Cluster, m.basePriceCached(ctx, typeID))
	}
	f.BidFloor = ref * cfg.BidFloorFrac
	return f
}

// ── the pilot's own needs ───────────────────────────────────────────────────

// gearReserve is what the pilot keeps of each type: a spare of every module fitted on its
// ships, its drone bays' worth, the rounds its fitted modules can load and its project's
// bill. Charges are reserved lazily (charge): compatibility is per module pair.
type gearReserve struct {
	fixed   map[int64]int64
	modules []int64 // fitted module typeIDs, one entry per fitted module
	mul     int64
	m       *Mod
	ctx     context.Context
	charges map[int64]int64
}

// pilotGearReserve reads the pilot's fitted modules, drone bays and project.
func (m *Mod) pilotGearReserve(ctx context.Context, b *botRun, cfg deadStockConfig) *gearReserve {
	r := &gearReserve{fixed: map[int64]int64{}, mul: cfg.AmmoReserveMul, m: m, ctx: ctx, charges: map[int64]int64{}}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, e.flag FROM entity e
		  JOIN entity s ON s.itemID = e.locationID AND s.ownerID = ?
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.ownerID = ? AND ((e.flag BETWEEN ? AND ? AND g.categoryID = 7) OR e.flag = ?)`,
		b.CharID, b.CharID, flagLoFirst, flagHiFirst+7, flagDroneBay)
	if err == nil {
		for rows.Next() {
			var typeID, flag int64
			if rows.Scan(&typeID, &flag) != nil {
				continue
			}
			if flag == flagDroneBay {
				r.fixed[typeID]++
				continue
			}
			if r.fixed[typeID] < cfg.ModuleReserve {
				r.fixed[typeID] = cfg.ModuleReserve
			}
			r.modules = append(r.modules, typeID)
		}
		rows.Close()
	}
	for t, n := range m.projectReservations(ctx, b) {
		r.fixed[t] += n
	}
	return r
}

// of is the units of typeID the pilot keeps.
func (r *gearReserve) of(typeID, categoryID, groupID int64) int64 {
	if r == nil {
		return 0
	}
	n := r.fixed[typeID]
	if categoryID == catCharge {
		n += r.charge(typeID, groupID)
	}
	return n
}

// charge is ammoReserve for one charge type: a full load for every fitted
// module that can fire it, times the reserve multiple.
func (r *gearReserve) charge(typeID, groupID int64) int64 {
	if v, ok := r.charges[typeID]; ok {
		return v
	}
	loaders := 0
	for _, mod := range r.modules {
		if r.m.chargeAccepts(r.ctx, typeID, mod) {
			loaders++
		}
	}
	v := ammoReserve(chargeQtyFor(groupID), loaders, r.mul)
	r.charges[typeID] = v
	return v
}

// ammoReserve is the rounds kept for one charge type. Pure.
func ammoReserve(perLoad int64, loaders int, mul int64) int64 {
	if perLoad <= 0 || loaders <= 0 || mul <= 0 {
		return 0
	}
	return perLoad * int64(loaders) * mul
}

// ── acting on a verdict ─────────────────────────────────────────────────────

// reprocessDeadStack reprocesses units of the stack itemID at the pilot's station and adds
// the minerals to got. It returns the units consumed.
func (m *Mod) reprocessDeadStack(ctx context.Context, b *botRun, itemID, units int64, name string,
	got map[int64]int64) int64 {

	id, n, err := m.takeUnits(ctx, itemID, units)
	if err != nil || n <= 0 {
		return 0
	}
	out, err := m.refineStack(ctx, b, id)
	if err != nil || out == nil {
		if err != nil {
			rlogf("rolebots: dead stock reprocess char=%d item=%d: %v", b.CharID, id, err)
		}
		return 0
	}
	for t, q := range out.Minerals {
		got[t] += q
	}
	m.logBot(b.CharID, logInfo, "deadstock",
		"reprocessed %d× %s into %s — nobody buys it anywhere in reach", out.UnitsIn, name, mineralList(ctx, m, out.Minerals))
	return out.UnitsIn
}

// mineralList names a refine's output for the log, biggest first by count.
func mineralList(ctx context.Context, m *Mod, got map[int64]int64) string {
	if len(got) == 0 {
		return "nothing"
	}
	s := ""
	for _, t := range mineralCascade {
		if q := got[t]; q > 0 {
			if s != "" {
				s += ", "
			}
			s += fmt.Sprintf("%d× %s", q, m.typeNameOr(ctx, t))
		}
	}
	if s == "" {
		return fmt.Sprintf("%d material line(s)", len(got))
	}
	return s
}

// sellIntoReachingBids sells units of a stack into the bids that reach the pilot's station
// (bidReach.go) with one immediate order at price. dumpToBestBid reads only the station's
// own book. It returns the units sold and the gross.
func (m *Mod) sellIntoReachingBids(ctx context.Context, b *botRun, typeID, itemID, units int64,
	price float64, name, note string) (int64, float64) {

	if units <= 0 || price <= 0 {
		return 0, 0
	}
	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
		typeID, itemID, price, units, -1 /*station*/, 1, 0 /*immediate*/, false)
	if err != nil || out == nil || out.ErrorMsg != "" || out.FilledQty <= 0 {
		return 0, 0
	}
	gross := 0.0
	for _, f := range out.Fills {
		gross += f.Price * float64(f.Quantity)
	}
	m.notePeerFills(ctx, tradeEntry{
		CharID: b.CharID, TypeID: typeID, TypeName: name, Station: b.StationID, System: b.SystemID,
	}, out.Fills, false, note)
	return out.FilledQty, gross
}

// listDeadStock lists units of a stack at price at the pilot's station on the loot terms
// (lootListDays), booked like a stray listing. It returns the units put on the market
// (resting plus filled on the spot).
func (m *Mod) listDeadStock(ctx context.Context, b *botRun, typeID, itemID, units int64,
	price float64, name, note string) int64 {

	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
		typeID, itemID, price, units, -1 /*station range*/, 1, lootListDays, false)
	if err != nil || out == nil || out.ErrorMsg != "" {
		return 0
	}
	if resting := units - out.FilledQty; resting > 0 && out.RestingOrderID != 0 {
		m.registerOrder(ctx, b.CharID, out.RestingOrderID, typeID, b.StationID,
			b.SystemID, name, false, price, 0, resting, tradeDealStock)
		m.bookTrade(ctx, tradeEntry{
			CharID: b.CharID, Kind: tradeKindList, TypeID: typeID,
			TypeName: name, Qty: resting, Price: price,
			Station: b.StationID, System: b.SystemID, OrderID: out.RestingOrderID, Note: note,
		})
		m.bookBrokerFee(ctx, b, out.RestingOrderID, typeID, name, price, resting)
	}
	return units
}

// deadStockOutcome tallies one pass for its log line and carries the ammunition set aside
// for a courier to a buyer (relocateDeadAmmo).
type deadStockOutcome struct {
	Reprocessed, Sold, Listed, Relocated int
	UnitsScrapped                        int64
	Reloc                                []relocItem
}

// relocItem is one split-off stack of ammunition waiting for a courier.
type relocItem struct {
	ItemID, TypeID, Units int64
	Name                  string
	FromLedger            bool // it came off the stock ledger: put back there if no courier goes
}

// applyDeadStock carries out a verdict on units of a stack, spending one budget unit per
// action. It returns the units disposed of (0 for keep and hold).
func (m *Mod) applyDeadStock(ctx context.Context, b *botRun, bg *deadStockBudget, p deadStockPlan,
	typeID, itemID int64, name string, got map[int64]int64, tally *deadStockOutcome) int64 {

	switch p.Act {
	case dsSellBid:
		n, _ := m.sellIntoReachingBids(ctx, b, typeID, itemID, p.Units, p.Price, name, "dead stock sold into a bid in reach")
		if n > 0 {
			bg.spend()
			tally.Sold++
		}
		return n
	case dsList:
		n := m.listDeadStock(ctx, b, typeID, itemID, p.Units, p.Price, name, "dead stock listed where the region buys it")
		if n > 0 {
			bg.spend()
			tally.Listed++
		}
		return n
	case dsReprocess:
		n := m.reprocessDeadStack(ctx, b, itemID, p.Units, name, got)
		if n > 0 {
			bg.spend()
			tally.Reprocessed++
			tally.UnitsScrapped += n
		}
		return n
	case dsRelocate:
		// Split the surplus off so the pilot's own reserve stays in the hangar; the courier goes
		// at the end of the pass (relocateDeadAmmo).
		id, n, err := m.takeUnits(ctx, itemID, p.Units)
		if err != nil || n <= 0 {
			return 0
		}
		bg.spend()
		tally.Reloc = append(tally.Reloc, relocItem{ItemID: id, TypeID: typeID, Units: n, Name: name})
		return n
	}
	return 0
}
