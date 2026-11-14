package rolebots

// The Mineral Supply page: what every region holds, what its buyers pay and what its rock is
// worth. It shows the inputs to two decisions: a miner digs the rock worth the most per m3 on its
// own markets (oreDigValues) and a trader carries what the destination has paid for
// (specRefPrices). Nothing is stored. Columns:
//
//	listed    units resting on the region's sell book
//	best bid  the highest standing buy price in the region
//	ore value the region's rock ranked by ISK per m3 at those bids
//	mined     what its miners planned trips for in the last hour

import (
	"context"
	"database/sql"
	"net/http"
	"regexp"
	"sort"
	"strconv"

	"gurimoon/destiny"
)

// mineralMaterialDemandRows is how much of the non-mineral ledger the page shows: a shortlist
// ordered by units wanted, to show which materials the market keeps failing to supply.
const mineralMaterialDemandRows = 25

// mineralMinedWindowSec is how far back the "mined" column reads trip plans.
const mineralMinedWindowSec = 3600

// mineralTopOres is how many ores each region's value and mined columns show.
const mineralTopOres = 3

// mineralCell is one region's market in one mineral.
type mineralCell struct {
	TypeID int64
	Listed int64
	Bid    float64 // best standing buy price in the region; 0 = nobody is bidding
}

// oreFigure is one ore on a region's row: its value, or how often it was mined.
type oreFigure struct {
	Name  string
	Value float64 // ISK per m³ at the region's bids (basePrice where none)
	Trips int
}

// mineralRegionRow is one region.
type mineralRegionRow struct {
	RegionID  int64
	Name      string
	Band      float64 // lowest security of any belt-bearing system in the region
	BandKnown bool
	Pilots    int
	Cells     []mineralCell
	BestOres  []oreFigure // dearest rock that spawns here, dearest first
	Mined     []oreFigure // what its miners planned trips for, commonest first
}

func (m *Mod) handleMineralSupply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fail := func(err error) { espError(w, "rolebot-minerals", err.Error()) }

	stock, err := m.mineralStockByRegion(ctx)
	if err != nil {
		fail(err)
		return
	}
	names, err := m.regionNames(ctx)
	if err != nil {
		fail(err)
		return
	}
	pilots, err := m.minersByHomeRegion(ctx)
	if err != nil {
		fail(err)
		return
	}
	bids, err := m.mineralBidsByRegion(ctx)
	if err != nil {
		fail(err)
		return
	}
	yields, err := m.oreYieldTable(ctx)
	if err != nil {
		fail(err)
		return
	}
	base := m.mineralBasePrices(ctx)
	mined := m.minedOresByRegion(ctx) // best-effort: an empty column, not a dead page

	var bands map[int64]float64
	if u, uerr := m.universe(ctx); uerr == nil {
		bands = u.regionOreBands(ctx, m.db)
	}

	// Regions worth a row: one with belts or one somebody lives in.
	want := map[int64]bool{}
	for id := range bands {
		want[id] = true
	}
	for id := range pilots {
		want[id] = true
	}

	rows := make([]mineralRegionRow, 0, len(want))
	for regionID := range want {
		band, known := bands[regionID]
		row := mineralRegionRow{
			RegionID: regionID, Name: names[regionID],
			Band: band, BandKnown: known, Pilots: pilots[regionID],
		}
		if row.Name == "" {
			row.Name = "region " + strconv.FormatInt(regionID, 10)
		}
		price := map[int64]float64{}
		for _, id := range mineralCascade {
			c := mineralCell{TypeID: id, Listed: stock[regionID][id], Bid: bids[regionID][id]}
			row.Cells = append(row.Cells, c)
			price[id] = c.Bid
			if price[id] <= 0 {
				price[id] = base[id]
			}
		}
		if known {
			row.BestOres = topOresByValue(oresSpawningFrom(band), yields, price, mineralTopOres)
		}
		row.Mined = mined[regionID]
		rows = append(rows, row)
	}
	// Busiest first, then by name so a refresh does not reshuffle the table.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Pilots != rows[j].Pilots {
			return rows[i].Pilots > rows[j].Pilots
		}
		return rows[i].Name < rows[j].Name
	})

	// Best-effort: the demand board is a second opinion on the same page, and
	// a failure to read it must not take the supply table down with it.
	demand, derr := m.mineralDemandBoard(ctx, stock)
	if derr != nil {
		demand = nil
	}
	materials, merr := m.materialDemand(ctx, mineralMaterialDemandRows)
	if merr != nil {
		materials = nil
	}

	espRender(w, "rolebots_minerals.html", map[string]any{
		"Page":      "rolebot-industry",
		"Tab":       "minerals",
		"Rows":      rows,
		"Demand":    demand,
		"Materials": materials,
		"Cascade":   m.cascadeNames(ctx),
		"Msg":       r.URL.Query().Get("msg"),
		"Err":       r.URL.Query().Get("err"),
	})
}

// mineralBidsByRegion is the best standing buy price per (region, mineral).
func (m *Mod) mineralBidsByRegion(ctx context.Context) (map[int64]map[int64]float64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT regionID, typeID, MAX(price) FROM market_orders
		 WHERE bid = 1 AND orderState = 1 AND volRemaining > 0
		   AND typeID IN (`+placeholders(len(mineralCascade))+`)
		 GROUP BY regionID, typeID`, int64Args(mineralCascade)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[int64]float64{}
	for rows.Next() {
		var region, typeID int64
		var p float64
		if rows.Scan(&region, &typeID, &p) != nil {
			continue
		}
		if out[region] == nil {
			out[region] = map[int64]float64{}
		}
		out[region][typeID] = p
	}
	return out, rows.Err()
}

// mineralBasePrices is each mineral's catalogue price, the fallback a region with no bid is
// valued at (the same fallback mineralPrices uses for a miner).
func (m *Mod) mineralBasePrices(ctx context.Context) map[int64]float64 {
	out := map[int64]float64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, COALESCE(basePrice, 0) FROM invTypes
		 WHERE typeID IN (`+placeholders(len(mineralCascade))+`)`, int64Args(mineralCascade)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p float64
		if rows.Scan(&id, &p) == nil {
			out[id] = p
		}
	}
	return out
}

// oreYield is one base ore's reprocessing: mineral units per ore unit, and m³
// per ore unit.
type oreYield struct {
	Volume  float64
	PerUnit map[int64]float64
}

// oreYieldTable reads the reprocessing table for the base ores the belts spawn, Mercoxit
// included. Which pilots may dig it is canMineMercoxit's question, not the page's.
func (m *Mod) oreYieldTable(ctx context.Context) (map[string]oreYield, error) {
	names := make([]any, 0, len(defaultOrePriority)+1)
	for _, n := range defaultOrePriority {
		names = append(names, n)
	}
	names = append(names, oreMercoxit)
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeName, t.volume, t.portionSize, r.mineralTypeID, r.amountPerBatch
		  FROM invTypes t
		  JOIN invorereprocessing r ON r.oreTypeID = t.typeID
		 WHERE t.typeName IN (`+placeholders(len(names))+`)`, names...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]oreYield{}
	for rows.Next() {
		var name string
		var vol float64
		var portion sql.NullInt64
		var mineral, amt int64
		if rows.Scan(&name, &vol, &portion, &mineral, &amt) != nil || !portion.Valid || portion.Int64 <= 0 {
			continue
		}
		y := out[name]
		if y.PerUnit == nil {
			y.PerUnit = map[int64]float64{}
		}
		y.Volume = vol
		y.PerUnit[mineral] += float64(amt) / float64(portion.Int64)
		out[name] = y
	}
	return out, rows.Err()
}

// oresSpawningFrom is every base ore that spawns between a region's lowest belt security and
// 1.0: the rock its belts can hold.
func oresSpawningFrom(band float64) map[string]bool {
	out := map[string]bool{}
	for sec := band; sec <= 1.0001; sec += 0.1 {
		for _, w := range destiny.OreDistribution(sec, 0) {
			out[w.Name] = true
		}
	}
	return out
}

// topOresByValue ranks the ores present by ISK per m3 at the given mineral prices: the
// arithmetic oreDigValues does for a pilot, over one region's book.
func topOresByValue(present map[string]bool, yields map[string]oreYield, price map[int64]float64, n int) []oreFigure {
	var out []oreFigure
	for name := range present {
		y, ok := yields[name]
		if !ok || y.Volume <= 0 {
			continue
		}
		v := 0.0
		for id, per := range y.PerUnit {
			v += per * price[id]
		}
		out = append(out, oreFigure{Name: name, Value: v / y.Volume})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Value != out[j].Value {
			return out[i].Value > out[j].Value
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// minedPlanRe reads the belt search's own log line (mining.go): plan, a dash, then
// <ore> in <system> (<n> jump(s), security <s>).
var minedPlanRe = regexp.MustCompile(`^plan — (.+?) in (.+?) \(`)

// minedOresByRegion counts the trips miners planned in the last mineralMinedWindowSec, by the
// region of the system. It reads the log because only a plan records what a pilot set out for;
// depletion rows say what was taken.
func (m *Mod) minedOresByRegion(ctx context.Context) map[int64][]oreFigure {
	out := map[int64][]oreFigure{}
	sysRegion := map[string]int64{}
	if rows, err := m.db.QueryContext(ctx,
		`SELECT solarSystemName, regionID FROM mapSolarSystems`); err == nil {
		for rows.Next() {
			var name string
			var region int64
			if rows.Scan(&name, &region) == nil {
				sysRegion[name] = region
			}
		}
		rows.Close()
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT message FROM rolebot_log
		 WHERE category = 'mining' AND ts >= ? AND message LIKE 'plan %'`,
		clock.Unix()-mineralMinedWindowSec)
	if err != nil {
		return out
	}
	defer rows.Close()
	counts := map[int64]map[string]int{}
	for rows.Next() {
		var msg string
		if rows.Scan(&msg) != nil {
			continue
		}
		mm := minedPlanRe.FindStringSubmatch(msg)
		if mm == nil {
			continue
		}
		region, ok := sysRegion[mm[2]]
		if !ok {
			continue
		}
		if counts[region] == nil {
			counts[region] = map[string]int{}
		}
		counts[region][mm[1]]++
	}
	for region, byOre := range counts {
		var figs []oreFigure
		for name, n := range byOre {
			figs = append(figs, oreFigure{Name: name, Trips: n})
		}
		sort.Slice(figs, func(i, j int) bool {
			if figs[i].Trips != figs[j].Trips {
				return figs[i].Trips > figs[j].Trips
			}
			return figs[i].Name < figs[j].Name
		})
		if len(figs) > mineralTopOres {
			figs = figs[:mineralTopOres]
		}
		out[region] = figs
	}
	return out
}

// ── the demand board ────────────────────────────────────────────────────────
//
// The demand half of the page: rolebot_mineral_demand, a factory naming an exact shortfall at an
// exact station (minerals.go). It shows whether anyone is asking and paying, which a thin shelf
// does not: many stations wanting a mineral with no ISK escrowed behind it is a different
// failure from nobody wanting any.

// mineralDemandRow is one mineral's whole standing on the board.
type mineralDemandRow struct {
	TypeID   int64
	Name     string
	Stations int     // how many factories are asking
	Wanted   int64   // units, summed over them
	Escrow   float64 // ISK actually committed to standing bids for it
	Listed   int64   // units resting on every sell book in the game
	Famine   int64   // the bar past which "it could be dug" stops blocking a bid
	Hungry   bool    // over that bar
}

// mineralDemandBoard reads the ledger whole, per mineral. mineralWants answers for one pilot and
// is capped at mineralWantRows, ordered for dispatch; a report needs the totals. The escrow
// sub-select has the same shape as that function's, so the printed number is the one miners sort on.
func (m *Mod) mineralDemandBoard(ctx context.Context, stock map[int64]map[int64]int64) ([]mineralDemandRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT d.typeID, COUNT(*) AS stations, SUM(d.wanted) AS wanted,
		       COALESCE(SUM(o.escrow), 0) AS escrow
		  FROM rolebot_mineral_demand d
		  LEFT JOIN (
			SELECT r.typeID, r.stationID, SUM(mo.price * mo.volRemaining) AS escrow
			  FROM rolebot_orders r
			  JOIN market_orders mo ON mo.orderID = r.orderID AND mo.orderState = 1
			 WHERE r.bid = 1 AND r.closedAt = 0 AND r.kind = ?
			 GROUP BY r.typeID, r.stationID
		  ) o ON o.typeID = d.typeID AND o.stationID = d.stationID
		 WHERE d.lastAt >= ?
		 GROUP BY d.typeID`, tradeDealSupply, clock.Unix()-mineralDemandTTLSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byType := map[int64]mineralDemandRow{}
	for rows.Next() {
		var r mineralDemandRow
		if rows.Scan(&r.TypeID, &r.Stations, &r.Wanted, &r.Escrow) == nil {
			byType[r.TypeID] = r
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Cascade order, and every rung whether or not anybody is asking: a list that prints only what
	// it found cannot tell an unwanted mineral from one nothing is built from.
	out := make([]mineralDemandRow, 0, len(mineralCascade))
	for _, id := range mineralCascade {
		r := byType[id]
		r.TypeID, r.Name = id, m.typeNameOr(ctx, id)
		for _, have := range stock {
			r.Listed += have[id]
		}
		r.Famine = supplyBidFamineUnits(id)
		r.Hungry = r.Wanted >= r.Famine
		out = append(out, r)
	}
	return out, nil
}

// materialDemandRow is one non-mineral line nothing in reach would sell.
type materialDemandRow struct {
	Name   string
	Wanted int64
	Asks   int
}

// materialDemand is the other ledger, rolebot_market_demand, filtered to what is not a mineral:
// what noteMaterialShortfall routes there (minerals.go) and what a shopping trip writes on a dead
// end (procure.go). It sits on this page because it answers the same question from the far
// side: the part of a bill of materials no asteroid will answer.
func (m *Mod) materialDemand(ctx context.Context, limit int) ([]materialDemandRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT d.typeName, `+demandWantedSQL("d", clock.Unix())+` AS w, d.asks
		  FROM rolebot_market_demand d
		  JOIN invTypes t ON t.typeID = d.typeID
		 WHERE d.lastAt >= ? AND t.groupID <> ?
		 ORDER BY w DESC
		 LIMIT ?`, clock.Unix()-industryDemandMaxAgeSec, groupMineral, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []materialDemandRow
	for rows.Next() {
		var r materialDemandRow
		if rows.Scan(&r.Name, &r.Wanted, &r.Asks) == nil {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// cascadeNames is the column header row, in cascade order.
func (m *Mod) cascadeNames(ctx context.Context) []string {
	out := make([]string, 0, len(mineralCascade))
	for _, id := range mineralCascade {
		out = append(out, m.typeNameOr(ctx, id))
	}
	return out
}

// regionNames is regionID → name for every region with a name.
func (m *Mod) regionNames(ctx context.Context) (map[int64]string, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT regionID, regionName FROM mapRegions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if rows.Scan(&id, &name) == nil {
			out[id] = name
		}
	}
	return out, rows.Err()
}

// minersByHomeRegion counts the rostered pilots living in each region. Home is the medical clone
// station (pod.go), which homeRegionFor resolves, so it counts where pilots live, not where they
// stand today.
func (m *Mod) minersByHomeRegion(ctx context.Context) (map[int64]int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.regionID, COUNT(*)
		  FROM rolebot_characters rc
		  JOIN character_ c ON c.characterID = rc.characterID
		  JOIN staStations s ON s.stationID = c.homeStationID
		 GROUP BY s.regionID`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if rows.Scan(&id, &n) == nil {
			out[id] = n
		}
	}
	return out, rows.Err()
}

// ListedShort is the listed units, compact.
func (c mineralCell) ListedShort() string { return iskShort(float64(c.Listed)) }

// BidShort is the best bid, with the decimals a cheap mineral needs.
func (c mineralCell) BidShort() string {
	if c.Bid < 100 {
		return strconv.FormatFloat(c.Bid, 'f', 2, 64)
	}
	return iskShort(c.Bid)
}
