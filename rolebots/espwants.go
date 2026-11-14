package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The wants list: /rolebots/wants, one item at /rolebots/want/<typeID>. It shows the capability
// census from wants.go (buildbook.go feeds it to industrialists), in the census's own order:
//
//	capable      pilots trained to use it
//	users        pilots who fit, load or fly it: the ranking's headcount (wantsusers.go)
//	units        what the users add up to (capabilityUnitsPerPilot)
//	blueprints   industrialists who bought the BPO, the investment behind a want
//	suppliers    industrialists carrying it as a standing shelf line, and at how many stations
//	asks / bids  the market, split bot / player / NPC: bot means served by the roster, NPC only by
//	             the game, neither an opening

// wantsPageSize is how much of the roll one page carries. The census is
// several thousand rows and no one reads past the interesting end of it.
const wantsPageSize = 150

// wantRow is one line of the list.
type wantRow struct {
	Rank       int
	TypeID     int64
	TypeName   string
	CategoryID int64
	Category   string
	MetaLevel  int64
	BPTypeID   int64

	// Capable is the skill headcount; Users the production headcount the list is ranked and sized on
	// (wantsusers.go).
	Capable int64
	Users   int64
	Pull    string
	Qty     int64

	// The roster's answer to the want.
	BPOwners  int64
	Suppliers int64
	Stations  int64
	Target    int64
	Built     int64
	Sold      int64
	Where     string

	// The market as it stands.
	BotAsks, PlayerAsks, NPCAsks int64
	BotAskUnits                  int64
	BotBids, PlayerBids          int64

	// Served says somebody on the roster is answering this want: a shelf line, a blueprint or a live
	// listing. It drives the openings-only filter and the row tint, and is generous: a pilot who has
	// bought the blueprint and not built yet is working on it.
	Served bool
}

// categoryLabel names the four categories the census covers.
func categoryLabel(categoryID int64) string {
	switch categoryID {
	case catShip:
		return "ship"
	case catModule:
		return "module"
	case catCharge:
		return "charge"
	case catDrone:
		return "drone"
	case catImplant:
		return "implant"
	}
	return "item"
}

// wantsFilter is what the form narrows the roll by.
type wantsFilter struct {
	Category int64
	Text     string
	Openings bool
	Limit    int
}

func wantsFilterFrom(r *http.Request) wantsFilter {
	f := wantsFilter{Text: strings.TrimSpace(r.URL.Query().Get("q")),
		Openings: r.URL.Query().Get("open") == "1", Limit: wantsPageSize}
	f.Category, _ = strconv.ParseInt(r.URL.Query().Get("cat"), 10, 64)
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 1000 {
		f.Limit = n
	}
	return f
}

// wantsRoll reads the census in its own order and hangs everything else off it. The openings-only
// filter is applied after the aggregates, because openness is a fact about the shelf, blueprints
// and order book, which the census query does not touch. It over-fetches when ticked, bounded.
func (m *Mod) wantsRoll(ctx context.Context, f wantsFilter) ([]wantRow, error) {
	fetch := f.Limit
	if f.Openings {
		fetch = f.Limit * 6
		if fetch > 3000 {
			fetch = 3000
		}
	}

	// buildable = 1: this page is the production overview. Meta and faction gear (counted for the loot
	// fence, wants.go) has no blueprint and the join below would drop it.
	where := []string{"c.users > 0", "c.buildable = 1"}
	args := []any{catShip, capabilityHullWeight, catCharge, capabilityChargeWeight}
	if f.Category != 0 {
		where = append(where, "c.categoryID = ?")
		args = append(args, f.Category)
	}
	if f.Text != "" {
		where = append(where, "c.typeName LIKE ?")
		args = append(args, "%"+f.Text+"%")
	}
	args = append(args, fetch)

	rows, err := m.db.QueryContext(ctx, `
		SELECT c.typeID, c.typeName, c.categoryID, c.capable, c.users, c.metaLevel,
		       bt.blueprintTypeID
		  FROM rolebot_capability c
		  JOIN invBlueprintTypes bt ON bt.productTypeID = c.typeID
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY c.users * CASE c.categoryID WHEN ? THEN ? WHEN ? THEN ? ELSE 1 END DESC,
		          c.metaLevel ASC, c.typeName ASC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("wants roll: %w", err)
	}
	defer rows.Close()

	var out []wantRow
	var ids []int64
	for rows.Next() {
		var w wantRow
		if err := rows.Scan(&w.TypeID, &w.TypeName, &w.CategoryID, &w.Capable, &w.Users,
			&w.MetaLevel, &w.BPTypeID); err != nil {
			return nil, fmt.Errorf("wants roll scan: %w", err)
		}
		w.Category = categoryLabel(w.CategoryID)
		w.Pull = fmt.Sprintf("%.0f", float64(w.Users)*capabilityWeight(w.CategoryID))
		w.Qty = w.Users * capabilityUnitsPerPilot(w.CategoryID)
		out = append(out, w)
		ids = append(ids, w.TypeID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	shelves, err := m.wantShelfTotals(ctx, ids)
	if err != nil {
		return nil, err
	}
	orders, err := m.wantOrderTotals(ctx, ids)
	if err != nil {
		return nil, err
	}
	bpos, err := m.wantBlueprintOwners(ctx, ids)
	if err != nil {
		return nil, err
	}

	kept := out[:0]
	for i := range out {
		w := &out[i]
		if s, ok := shelves[w.TypeID]; ok {
			w.Suppliers, w.Stations = s.suppliers, s.stations
			w.Target, w.Built, w.Sold, w.Where = s.target, s.built, s.sold, s.where
		}
		if o, ok := orders[w.TypeID]; ok {
			w.BotAsks, w.PlayerAsks, w.NPCAsks = o.botAsks, o.playerAsks, o.npcAsks
			w.BotAskUnits = o.botAskUnits
			w.BotBids, w.PlayerBids = o.botBids, o.playerBids
		}
		w.BPOwners = bpos[w.TypeID]
		w.Served = w.Suppliers > 0 || w.BPOwners > 0 || w.BotAsks > 0
		if f.Openings && w.Served {
			continue
		}
		kept = append(kept, *w)
		if len(kept) >= f.Limit {
			break
		}
	}
	for i := range kept {
		kept[i].Rank = i + 1
	}
	return kept, nil
}

// shelfTotals is the supply side of one want.
type shelfTotals struct {
	suppliers, stations int64
	target, built, sold int64
	where               string
}

// wantShelfTotals counts the industrialists carrying each type as a standing line and names the
// stations they carry it at, since a count with no place says the roster is on the case, not where.
func (m *Mod) wantShelfTotals(ctx context.Context, ids []int64) (map[int64]shelfTotals, error) {
	out := map[int64]shelfTotals{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, COUNT(DISTINCT characterID), COUNT(DISTINCT stationID),
		       COALESCE(SUM(targetQty),0), COALESCE(SUM(builtQty),0), COALESCE(SUM(soldQty),0)
		  FROM rolebot_industry_shelf
		 WHERE typeID IN (`+placeholders(len(ids))+`)
		 GROUP BY typeID`, int64Args(ids)...)
	if err != nil {
		return nil, fmt.Errorf("want shelf totals: %w", err)
	}
	for rows.Next() {
		var typeID int64
		var s shelfTotals
		if err := rows.Scan(&typeID, &s.suppliers, &s.stations,
			&s.target, &s.built, &s.sold); err != nil {
			rows.Close()
			return nil, err
		}
		out[typeID] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// The stations, biggest first, two named per type and the rest counted.
	named := map[int64][]string{}
	rows, err = m.db.QueryContext(ctx, `
		SELECT s.typeID, COALESCE(sta.stationName, CONCAT('station ', s.stationID)),
		       COUNT(DISTINCT s.characterID) n
		  FROM rolebot_industry_shelf s
		  LEFT JOIN staStations sta ON sta.stationID = s.stationID
		 WHERE s.typeID IN (`+placeholders(len(ids))+`)
		 GROUP BY s.typeID, s.stationID, sta.stationName
		 ORDER BY s.typeID, n DESC`, int64Args(ids)...)
	if err != nil {
		return nil, fmt.Errorf("want shelf stations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID int64
		var name string
		var n int64
		if err := rows.Scan(&typeID, &name, &n); err != nil {
			return nil, err
		}
		named[typeID] = append(named[typeID], name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for typeID, names := range named {
		s := out[typeID]
		s.where = strings.Join(names[:min(2, len(names))], ", ")
		if len(names) > 2 {
			s.where += fmt.Sprintf(" +%d more", len(names)-2)
		}
		out[typeID] = s
	}
	return out, nil
}

// orderTotals is what is on the market for one want.
type orderTotals struct {
	botAsks, playerAsks, npcAsks int64
	botAskUnits                  int64
	botBids, playerBids          int64
}

// wantOrderTotals splits the live book three ways per type: bot, player and NPC. One count would
// mislead (thousands of NPC-seeded asks read as a well-served market). The owner is `charID`
// against the roster table, with the two seeder IDs (db.SeederCharID, db.AdminSeederCharID) removed first.
func (m *Mod) wantOrderTotals(ctx context.Context, ids []int64) (map[int64]orderTotals, error) {
	args := append([]any{}, int64Args(ids)...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, o.bid,
		       SUM(CASE WHEN rb.characterID IS NOT NULL THEN 1 ELSE 0 END),
		       SUM(CASE WHEN rb.characterID IS NOT NULL THEN o.volRemaining ELSE 0 END),
		       SUM(CASE WHEN rb.characterID IS NULL AND o.charID NOT IN (90000001, 90000002)
		                THEN 1 ELSE 0 END),
		       SUM(CASE WHEN o.charID IN (90000001, 90000002) THEN 1 ELSE 0 END)
		  FROM market_orders o
		  LEFT JOIN rolebot_characters rb ON rb.characterID = o.charID
		 WHERE o.orderState = 1 AND o.volRemaining > 0
		   AND o.typeID IN (`+placeholders(len(ids))+`)
		 GROUP BY o.typeID, o.bid`, args...)
	if err != nil {
		return nil, fmt.Errorf("want order totals: %w", err)
	}
	defer rows.Close()
	out := map[int64]orderTotals{}
	for rows.Next() {
		var typeID, bid, bots, botUnits, players, npcs int64
		if err := rows.Scan(&typeID, &bid, &bots, &botUnits, &players, &npcs); err != nil {
			return nil, err
		}
		o := out[typeID]
		if bid == 1 {
			o.botBids, o.playerBids = bots, players
		} else {
			o.botAsks, o.playerAsks, o.npcAsks = bots, players, npcs
			o.botAskUnits = botUnits
		}
		out[typeID] = o
	}
	return out, rows.Err()
}

// wantBlueprintOwners counts the rolebots holding a blueprint for each product, the investment
// behind a want (industryBuyBPO).
func (m *Mod) wantBlueprintOwners(ctx context.Context, ids []int64) (map[int64]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT bt.productTypeID, COUNT(DISTINCT e.ownerID)
		  FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  JOIN rolebot_characters rb ON rb.characterID = e.ownerID
		 WHERE bt.productTypeID IN (`+placeholders(len(ids))+`)
		 GROUP BY bt.productTypeID`, int64Args(ids)...)
	if err != nil {
		return nil, fmt.Errorf("want blueprint owners: %w", err)
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var typeID, n int64
		if err := rows.Scan(&typeID, &n); err != nil {
			return nil, err
		}
		out[typeID] = n
	}
	return out, rows.Err()
}

// wantsSummary is the arithmetic over the WHOLE census, not the page.
type wantsSummary struct {
	Types      int64
	Served     int64
	Openings   int64
	Suppliers  int64
	Shelves    int64
	Blueprints int64
	Swept      string
}

func (m *Mod) wantsSummary(ctx context.Context) (wantsSummary, error) {
	var s wantsSummary
	var swept sql.NullInt64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(updatedAt) FROM rolebot_capability
		  WHERE users > 0 AND buildable = 1`).
		Scan(&s.Types, &swept); err != nil {
		return s, fmt.Errorf("wants summary: %w", err)
	}
	s.Swept = "never — the census runs within fifteen minutes of boot"
	if swept.Valid && swept.Int64 > 0 {
		s.Swept = agoString(clock.Unix()-swept.Int64) + " ago"
	}
	// How much of the census anybody is answering: the shelf and the blueprint holdings, unioned by
	// type. A supplier holding a blueprint counts, since it has already paid for the want.
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT typeID FROM rolebot_industry_shelf
			UNION
			SELECT bt.productTypeID FROM entity e
			  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
			  JOIN rolebot_characters rb ON rb.characterID = e.ownerID
		) t JOIN rolebot_capability c ON c.typeID = t.typeID
		     AND c.users > 0 AND c.buildable = 1`).Scan(&s.Served)
	s.Openings = s.Types - s.Served
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT characterID), COUNT(*) FROM rolebot_industry_shelf`).
		Scan(&s.Suppliers, &s.Shelves)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  JOIN rolebot_characters rb ON rb.characterID = e.ownerID`).Scan(&s.Blueprints)
	return s, nil
}

func (m *Mod) handleWants(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()

	f := wantsFilterFrom(r)
	rows, err := m.wantsRoll(ctx, f)
	if err != nil {
		espError(w, "rolebot-wants", err.Error())
		return
	}
	sum, err := m.wantsSummary(ctx)
	if err != nil {
		espError(w, "rolebot-wants", err.Error())
		return
	}
	espRender(w, "rolebots_wants.html", map[string]any{
		"Page":     "rolebot-economy",
		"Tab":      "wants",
		"Rows":     rows,
		"Sum":      sum,
		"Cat":      f.Category,
		"Text":     f.Text,
		"Openings": f.Openings,
		"Limit":    f.Limit,
		"Cats": []struct {
			ID    int64
			Label string
		}{
			{catShip, "ships"}, {catModule, "modules"},
			{catCharge, "charges"}, {catDrone, "drones"},
		},
		"HullWeight":   fmt.Sprintf("%.1f×", capabilityHullWeight),
		"ChargeWeight": fmt.Sprintf("%.1f×", capabilityChargeWeight),
		"Rounds":       capabilityRoundsPerPilot,
	})
}

// ── one want ────────────────────────────────────────────────────────────────

// wantSupplier is one industrialist's standing line for this type.
type wantSupplier struct {
	CharID    int64
	Name      string
	Station   string
	System    string
	Target    int64
	Built     int64
	Sold      int64
	Source    string
	LastBuilt string
}

// wantOrder is one live order for this type, with who is behind it.
type wantOrder struct {
	Side    string
	Who     string
	CharID  int64
	IsBot   bool
	IsNPC   bool
	Station string
	System  string
	Region  string
	Price   string
	Units   int64
}

// wantHolder is one rolebot holding the blueprint.
type wantHolder struct {
	CharID int64
	Name   string
	Where  string
}

func (m *Mod) handleWantPage(w http.ResponseWriter, r *http.Request, typeID int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()

	var row wantRow
	err := m.db.QueryRowContext(ctx, `
		SELECT c.typeID, c.typeName, c.categoryID, c.capable, c.users, c.metaLevel,
		       COALESCE(bt.blueprintTypeID, 0)
		  FROM rolebot_capability c
		  LEFT JOIN invBlueprintTypes bt ON bt.productTypeID = c.typeID
		 WHERE c.typeID = ?`, typeID).
		Scan(&row.TypeID, &row.TypeName, &row.CategoryID, &row.Capable, &row.Users,
			&row.MetaLevel, &row.BPTypeID)
	if err == sql.ErrNoRows {
		espError(w, "rolebot-wants", fmt.Sprintf(
			"type %d is not on the capability census — nothing buildable, or nobody on the roster can use it",
			typeID))
		return
	} else if err != nil {
		espError(w, "rolebot-wants", err.Error())
		return
	}
	row.Category = categoryLabel(row.CategoryID)
	row.Pull = fmt.Sprintf("%.0f", float64(row.Users)*capabilityWeight(row.CategoryID))
	row.Qty = row.Users * capabilityUnitsPerPilot(row.CategoryID)

	suppliers, err := m.wantSuppliers(ctx, typeID)
	if err != nil {
		espError(w, "rolebot-wants", err.Error())
		return
	}
	orders, err := m.wantOrders(ctx, typeID)
	if err != nil {
		espError(w, "rolebot-wants", err.Error())
		return
	}
	holders, err := m.wantHolders(ctx, row.BPTypeID)
	if err != nil {
		espError(w, "rolebot-wants", err.Error())
		return
	}
	peers, err := m.peerRows(ctx, peerFilter{TypeID: typeID}, 40)
	if err != nil {
		espError(w, "rolebot-wants", err.Error())
		return
	}

	espRender(w, "rolebots_want.html", map[string]any{
		"Page":      "rolebot-economy",
		"Tab":       "wants",
		"W":         row,
		"Suppliers": suppliers,
		"Orders":    orders,
		"Holders":   holders,
		"Peers":     peers,
	})
}

func (m *Mod) wantSuppliers(ctx context.Context, typeID int64) ([]wantSupplier, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID, COALESCE(c.characterName, ''),
		       COALESCE(sta.stationName, CONCAT('station ', s.stationID)),
		       COALESCE(ms.solarSystemName, ''),
		       s.targetQty, s.builtQty, s.soldQty, s.source, s.lastBuiltAt
		  FROM rolebot_industry_shelf s
		  LEFT JOIN character_ c ON c.characterID = s.characterID
		  LEFT JOIN staStations sta ON sta.stationID = s.stationID
		  LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = sta.solarSystemID
		 WHERE s.typeID = ?
		 ORDER BY s.builtQty DESC, s.targetQty DESC`, typeID)
	if err != nil {
		return nil, fmt.Errorf("want suppliers: %w", err)
	}
	defer rows.Close()
	out := []wantSupplier{}
	for rows.Next() {
		var s wantSupplier
		var last int64
		if err := rows.Scan(&s.CharID, &s.Name, &s.Station, &s.System,
			&s.Target, &s.Built, &s.Sold, &s.Source, &last); err != nil {
			return nil, err
		}
		s.LastBuilt = "never"
		if last > 0 {
			s.LastBuilt = agoString(clock.Unix()-last) + " ago"
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// wantOrders lists the live book for one type, cheapest ask first then the bids, with a name
// against every order, since twelve sell orders say nothing without whose they are.
func (m *Mod) wantOrders(ctx context.Context, typeID int64) ([]wantOrder, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.bid, o.charID, COALESCE(c.characterName, ''),
		       rb.characterID IS NOT NULL,
		       COALESCE(sta.stationName, CONCAT('station ', o.stationID)),
		       COALESCE(ms.solarSystemName, ''), COALESCE(rg.regionName, ''),
		       o.price, o.volRemaining
		  FROM market_orders o
		  LEFT JOIN character_ c ON c.characterID = o.charID
		  LEFT JOIN rolebot_characters rb ON rb.characterID = o.charID
		  LEFT JOIN staStations sta ON sta.stationID = o.stationID
		  LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = sta.solarSystemID
		  LEFT JOIN mapRegions rg ON rg.regionID = ms.regionID
		 WHERE o.typeID = ? AND o.orderState = 1 AND o.volRemaining > 0
		 ORDER BY o.bid ASC, o.price ASC
		 LIMIT 300`, typeID)
	if err != nil {
		return nil, fmt.Errorf("want orders: %w", err)
	}
	defer rows.Close()
	out := []wantOrder{}
	for rows.Next() {
		var o wantOrder
		var bid int64
		var price float64
		if err := rows.Scan(&bid, &o.CharID, &o.Who, &o.IsBot, &o.Station,
			&o.System, &o.Region, &price, &o.Units); err != nil {
			return nil, err
		}
		o.Side = "ask"
		if bid == 1 {
			o.Side = "bid"
		}
		o.IsNPC = o.CharID == 90000001 || o.CharID == 90000002
		if o.IsNPC {
			o.Who = "NPC seeder"
		} else if o.Who == "" {
			o.Who = fmt.Sprintf("character %d", o.CharID)
		}
		o.Price = fmt.Sprintf("%.2f", price)
		out = append(out, o)
	}
	return out, rows.Err()
}

func (m *Mod) wantHolders(ctx context.Context, bpTypeID int64) ([]wantHolder, error) {
	if bpTypeID == 0 {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.ownerID, COALESCE(c.characterName, ''),
		       COALESCE(sta.stationName, '')
		  FROM entity e
		  JOIN rolebot_characters rb ON rb.characterID = e.ownerID
		  LEFT JOIN character_ c ON c.characterID = e.ownerID
		  LEFT JOIN staStations sta ON sta.stationID = e.locationID
		 WHERE e.typeID = ?
		 ORDER BY 2`, bpTypeID)
	if err != nil {
		return nil, fmt.Errorf("want holders: %w", err)
	}
	defer rows.Close()
	out := []wantHolder{}
	for rows.Next() {
		var h wantHolder
		if err := rows.Scan(&h.CharID, &h.Name, &h.Where); err != nil {
			return nil, err
		}
		if h.Where == "" {
			// A blueprint in a ship's hold has a ship for a location, not a station: it is in transit while
			// its buyer flies it home, and the page says so.
			h.Where = "in transit (aboard a ship)"
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
