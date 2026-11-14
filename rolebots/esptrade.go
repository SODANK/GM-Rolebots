package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The trade surface of the Economy section: a map of where the roster's commerce happens,
// a system drill-down and the trade feed, built on the trade book (tradebook.go), the
// ISK-flow market ledger (iskflow.go) and the traffic ledger (traffic.go).
//
// ── map data ────────────────────────────────────────────────────────────────
//
// The front page (esphubs.go) draws the busiest systems at their real coordinates, sized by
// ISK traded. One region alone (/rolebots/trademap/region/<id>) is a lattice of its real
// gate graph with the hub score term by term.

// mapSystem is one dot on a REGION page.
type mapSystem struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	Region        string  `json:"region"`
	RegionID      int64   `json:"regionID"`
	Constellation string  `json:"const"`
	Sec           float64 `json:"sec"`

	// Where it is DRAWN: a lattice cell, not a place. See maplayout.go for
	// why the real coordinates do not survive to the page.
	Col int `json:"col"`
	Row int `json:"row"`

	Score   float64 `json:"score"`
	Base    float64 `json:"base"`
	Traffic float64 `json:"traffic"`
	Lane    float64 `json:"lane"`
	Market  float64 `json:"market"`
	Flow    float64 `json:"flow"`
	// Traded is measured liquidity (liquidity.go): decayed ISK that changed
	// hands here between non-seeders. The number trade decisions read.
	Traded   float64 `json:"traded"`
	StationT float64 `json:"stationT"`
	Topo     float64 `json:"topo"`
	SecT     float64 `json:"secT"`
	Gates    int     `json:"gates"`

	// The book is the orders players and rolebots wrote and what they are worth. The NPC
	// seeder's orders are absent from every page (see hubRow).
	PlayerOrders int64   `json:"orders"`
	PlayerISK    float64 `json:"isk"`
	Homes        int     `json:"homes"`
	Factories    int     `json:"factories"`
	Stations     int     `json:"stations"`
	School       bool    `json:"school"` // a chargen starter system
}

// mapLane is one road, with what the roster has done on it.
type mapLane struct {
	A      int64   `json:"a"`
	B      int64   `json:"b"`
	Jumps  float64 `json:"jumps"`
	Hauls  float64 `json:"hauls"`
	ISK    float64 `json:"isk"`
	Weight float64 `json:"w"` // 0..1 against the busiest lane
}

// mapPilot is one dot that moves.
type mapPilot struct {
	CharID   int64  `json:"charID"`
	Name     string `json:"name"`
	SystemID int64  `json:"sys"`
	RegionID int64  `json:"regionID"`
	Where    string `json:"where"`
	Doing    string `json:"doing"`
	Docked   bool   `json:"docked"`
	Bot      bool   `json:"bot"`
	Trading  bool   `json:"trading"`
}

const mapMaxLanes = 700

// xz is a raw EVE coordinate pair before normalisation. EVE's are in
// metres and run to 1e17, which no SVG viewBox wants to hear about.
type xz struct{ x, z float64 }

// normalise turns raw coordinates into 0..1 over their bounding box. The z axis is flipped
// to match a screen's y, and a degenerate span collapses to the middle.
func normalise(coords map[int64]xz) func(int64) (float64, float64) {
	minX, maxX, minY, maxY := 0.0, 0.0, 0.0, 0.0
	first := true
	for _, c := range coords {
		if first {
			minX, maxX, minY, maxY, first = c.x, c.x, c.z, c.z, false
			continue
		}
		minX, maxX = math.Min(minX, c.x), math.Max(maxX, c.x)
		minY, maxY = math.Min(minY, c.z), math.Max(maxY, c.z)
	}
	spanX, spanY := maxX-minX, maxY-minY
	return func(id int64) (float64, float64) {
		c, ok := coords[id]
		if !ok {
			return 0.5, 0.5
		}
		x, y := 0.5, 0.5
		if spanX > 0 {
			x = (c.x - minX) / spanX
		}
		if spanY > 0 {
			y = 1 - (c.z-minY)/spanY
		}
		return x, y
	}
}

// tradeMapRegion builds one region's page: every system in it, the real
// gate graph between them, and the whole score breakdown per system.
func (m *Mod) tradeMapRegion(ctx context.Context, regionID int64, withPilots bool) (map[string]any, error) {
	var regionName string
	if err := m.db.QueryRowContext(ctx,
		`SELECT regionName FROM mapRegions WHERE regionID = ?`, regionID).Scan(&regionName); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("no such region")
		}
		return nil, fmt.Errorf("region: %w", err)
	}

	// Every system in the region, scored or not: a stationless system is the road between two
	// hubs, and leaving it out would split the gate graph into islands.
	rows, err := m.db.QueryContext(ctx, `
		SELECT ms.solarSystemID, ms.solarSystemName, COALESCE(c.constellationName, ''),
		       COALESCE(ms.security, 0), COALESCE(ms.x, 0), COALESCE(ms.z, 0),
		       COALESCE(h.score, 0), COALESCE(h.base, 0),
		       COALESCE(h.trafficTerm, 0), COALESCE(h.laneTerm, 0), COALESCE(h.marketTerm, 0),
		       COALESCE(h.flowTerm, 0),
		       COALESCE(h.stationTerm, 0), COALESCE(h.topoTerm, 0),
		       COALESCE(h.secTerm, 0), COALESCE(h.gates, 0),
		       COALESCE(h.playerOrders, 0), COALESCE(h.playerISK, 0),
		       COALESCE(h.homes, 0), COALESCE(h.factories, 0),
		       COALESCE(h.stations, 0), COALESCE(h.school, 0),
		       COALESCE((SELECT SUM(l.isk) FROM rolebot_station_liquidity l
		                  WHERE l.systemID = ms.solarSystemID), 0)
		  FROM mapSolarSystems ms
		  LEFT JOIN mapConstellations c ON c.constellationID = ms.constellationID
		  LEFT JOIN rolebot_hub_scores h ON h.systemID = ms.solarSystemID
		 WHERE ms.regionID = ?`, regionID)
	if err != nil {
		return nil, fmt.Errorf("region systems: %w", err)
	}
	sys := map[int64]*mapSystem{}
	coords := map[int64]xz{}
	for rows.Next() {
		s := &mapSystem{RegionID: regionID, Region: regionName}
		var x, z float64
		if err := rows.Scan(&s.ID, &s.Name, &s.Constellation, &s.Sec, &x, &z,
			&s.Score, &s.Base,
			&s.Traffic, &s.Lane, &s.Market, &s.Flow, &s.StationT, &s.Topo, &s.SecT, &s.Gates,
			&s.PlayerOrders, &s.PlayerISK,
			&s.Homes, &s.Factories, &s.Stations,
			&s.School, &s.Traded); err != nil {
			rows.Close()
			return nil, err
		}
		sys[s.ID], coords[s.ID] = s, xz{x, z}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sys) == 0 {
		return nil, fmt.Errorf("no such region")
	}

	// The gate graph, both ends inside the region. A link leaving it is drawn on the other
	// region's page.
	grows, err := m.db.QueryContext(ctx, `
		SELECT fromSolarSystemID, toSolarSystemID
		  FROM mapSolarSystemJumps
		 WHERE fromRegionID = ? AND toRegionID = ?`, regionID, regionID)
	if err != nil {
		return nil, fmt.Errorf("region gates: %w", err)
	}
	type gate struct {
		A int64 `json:"a"`
		B int64 `json:"b"`
	}
	seen := map[[2]int64]bool{}
	gates := []gate{}
	for grows.Next() {
		var a, b int64
		if err := grows.Scan(&a, &b); err != nil {
			grows.Close()
			return nil, err
		}
		if a > b {
			a, b = b, a
		}
		if seen[[2]int64{a, b}] || sys[a] == nil || sys[b] == nil {
			continue
		}
		seen[[2]int64{a, b}] = true
		gates = append(gates, gate{a, b})
	}
	grows.Close()
	if err := grows.Err(); err != nil {
		return nil, err
	}

	// And what the roster has flown, over the top of the roads.
	lanes, err := m.tradeMapLanes(ctx, sys)
	if err != nil {
		return nil, err
	}

	// The lattice is laid out on the gate graph alone. Roster lanes are drawn over it and
	// never shape it.
	edges := make([][2]int64, 0, len(gates))
	for _, g := range gates {
		edges = append(edges, [2]int64{g.A, g.B})
	}
	grid := cachedGridLayout(regionID, coords, edges)

	out := make([]*mapSystem, 0, len(sys))
	for _, s := range sys {
		c := grid.At[s.ID]
		s.Col, s.Row = c.Col, c.Row
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })

	data := map[string]any{
		"region":   regionName,
		"regionID": regionID,
		"systems":  out,
		"gates":    gates,
		"lanes":    lanes,
		"cols":     grid.Cols,
		"rows":     grid.Rows,
		"now":      time.Now().UTC().Format("15:04:05 UTC"),
	}
	if withPilots {
		pilots, err := m.tradeMapPilots(ctx, regionID)
		if err != nil {
			return nil, err
		}
		data["pilots"] = pilots
	}
	return data, nil
}

// tradeMapLanes reads the roster's own lanes, keeping only the ones with
// both ends on the page in hand.
func (m *Mod) tradeMapLanes(ctx context.Context, sys map[int64]*mapSystem) ([]mapLane, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT fromSystemID, toSystemID, jumps, hauls, iskMoved
		  FROM rolebot_traffic_lanes
		 ORDER BY (jumps + hauls * 3) DESC
		 LIMIT ?`, mapMaxLanes)
	if err != nil {
		return nil, fmt.Errorf("trade map lanes: %w", err)
	}
	defer rows.Close()
	lanes := []mapLane{}
	heaviest := 0.0
	for rows.Next() {
		var l mapLane
		if err := rows.Scan(&l.A, &l.B, &l.Jumps, &l.Hauls, &l.ISK); err != nil {
			return nil, err
		}
		if sys[l.A] == nil || sys[l.B] == nil {
			continue
		}
		w := l.Jumps + l.Hauls*3
		if w > heaviest {
			heaviest = w
		}
		l.Weight = w
		lanes = append(lanes, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range lanes {
		if heaviest > 0 {
			lanes[i].Weight /= heaviest
		}
	}
	return lanes, nil
}

// systemPlaces resolves a set of system IDs to name + region in one query.
func (m *Mod) systemPlaces(ctx context.Context, ids []int64) (map[int64]systemPlace, error) {
	out := map[int64]systemPlace{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT solarSystemID, solarSystemName, COALESCE(regionID, 0)
		  FROM mapSolarSystems WHERE solarSystemID IN (`+placeholders(len(ids))+`)`,
		int64Args(ids)...)
	if err != nil {
		return nil, fmt.Errorf("system places: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p systemPlace
		if err := rows.Scan(&id, &p.name, &p.regionID); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

type systemPlace struct {
	name     string
	regionID int64
}

// tradeMapPilots is the where-is-everybody overlay: every rostered rolebot and every real
// player with a live session, as one row kind with a flag for which is which.
// regionID 0 means everywhere; a region page asks for its own.
func (m *Mod) tradeMapPilots(ctx context.Context, regionID int64) ([]mapPilot, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName, COALESCE(c.solarSystemID, 0),
		       COALESCE(ms.regionID, 0),
		       COALESCE(ms.solarSystemName, ''), COALESCE(c.stationID, 0),
		       COALESCE(s.activity, ''), COALESCE(s.phase, '')
		  FROM rolebot_characters b
		  JOIN character_ c ON c.characterID = b.characterID
		  LEFT JOIN rolebot_state s ON s.characterID = c.characterID
		  LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = c.solarSystemID
		 WHERE b.enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("trade map pilots: %w", err)
	}
	defer rows.Close()
	seen := map[int64]bool{}
	out := []mapPilot{}
	for rows.Next() {
		var p mapPilot
		var station int64
		var activity, phase string
		if err := rows.Scan(&p.CharID, &p.Name, &p.SystemID, &p.RegionID, &p.Where, &station,
			&activity, &phase); err != nil {
			return nil, err
		}
		p.Bot, p.Docked = true, station != 0
		p.Trading = activity == ActTrade
		if d, ok := taskDef(phase); ok {
			p.Doing = d.Label
		}
		// Marked seen either way: a bot outside the region asked for is
		// still a bot, and must not come back a second time as a "player".
		seen[p.CharID] = true
		if regionID != 0 && p.RegionID != regionID {
			continue
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if m.sessions == nil {
		return out, nil
	}
	// A session carries only a system ID, so the connected set is resolved in one query
	// instead of one per player.
	var live []mapPilot
	var wantSys []int64
	for _, s := range m.sessions.Snapshot() {
		if s.CharID == 0 || seen[s.CharID] || s.SolarSystemID == 0 {
			continue
		}
		live = append(live, mapPilot{
			CharID: s.CharID, Name: s.CharName, SystemID: s.SolarSystemID,
			Docked: s.StationID != 0, Doing: "player", Bot: false,
		})
		wantSys = append(wantSys, s.SolarSystemID)
	}
	if len(live) == 0 {
		return out, nil
	}
	where, err := m.systemPlaces(ctx, wantSys)
	if err != nil {
		return nil, err
	}
	for _, p := range live {
		pl := where[p.SystemID]
		p.Where, p.RegionID = pl.name, pl.regionID
		if regionID != 0 && p.RegionID != regionID {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ── the trade feed ──────────────────────────────────────────────────────────

// feedRow is one line of the trade feed.
type feedRow struct {
	ID       int64   `json:"id"`
	When     string  `json:"when"`
	Full     string  `json:"full"`
	CharID   int64   `json:"charID"`
	Who      string  `json:"who"`
	Kind     string  `json:"kind"`
	TypeID   int64   `json:"typeID"`
	TypeName string  `json:"typeName"`
	Qty      int64   `json:"qty"`
	Price    string  `json:"price"`
	Value    string  `json:"value"`
	Profit   float64 `json:"profitRaw"`
	ProfitS  string  `json:"profit"`
	// Where is the system and Station the dock inside it. A system with four stations has four
	// separate markets, so the feed carries both.
	Where   string `json:"where"`
	Station string `json:"station"`
	From    string `json:"from"`
	Note    string `json:"note"`
}

type feedFilter struct {
	Kind     string
	CharID   int64
	Who      string // the pilot name typed, resolved into CharID
	Text     string
	SystemID int64
	// Paging on the row ID. After is newer-than (the live poll, and the
	// Newer link when Newer is set); Before is older-than (the Older link).
	After  int64
	Before int64
	Newer  bool
}

// feedPageSize is one page of the ledger; a page an operator can read is short.
const feedPageSize = 100

func (m *Mod) feedFilterFrom(ctx context.Context, r *http.Request) feedFilter {
	q := r.URL.Query()
	f := feedFilter{Kind: q.Get("kind"), Text: strings.TrimSpace(q.Get("q")),
		Who: strings.TrimSpace(q.Get("who"))}
	f.CharID, _ = strconv.ParseInt(q.Get("char"), 10, 64)
	if f.CharID == 0 && f.Who != "" {
		f.CharID = m.resolveCharName(ctx, f.Who)
		if f.CharID == 0 {
			f.CharID = -1 // a name nobody has: match nothing rather than everything
		}
	}
	f.SystemID, _ = strconv.ParseInt(q.Get("system"), 10, 64)
	f.After, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	f.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	f.Newer = q.Get("page") == "newer"
	return f
}

// query is the filter's own parameters, without the paging ones, so a
// pager link can add its own.
func (f feedFilter) query() string {
	v := make([]string, 0, 4)
	if f.Kind != "" {
		v = append(v, "kind="+url.QueryEscape(f.Kind))
	}
	if f.Who != "" {
		v = append(v, "who="+url.QueryEscape(f.Who))
	} else if f.CharID > 0 {
		v = append(v, fmt.Sprintf("char=%d", f.CharID))
	}
	if f.Text != "" {
		v = append(v, "q="+url.QueryEscape(f.Text))
	}
	if f.SystemID != 0 {
		v = append(v, fmt.Sprintf("system=%d", f.SystemID))
	}
	return strings.Join(v, "&")
}

// pagerLinks builds the Older / Newer links for a page of rows whose IDs
// descend. Empty when there is nothing that way.
func pagerLinks(path, query string, firstID, lastID int64, n, pageSize int, paged bool) (older, newer string) {
	sep := "?"
	if query != "" {
		sep = "?" + query + "&"
	}
	if n >= pageSize && lastID > 0 {
		older = fmt.Sprintf("%s%sbefore=%d", path, sep, lastID)
	}
	if paged && firstID > 0 {
		newer = fmt.Sprintf("%s%safter=%d&page=newer", path, sep, firstID)
	}
	return
}

func (m *Mod) queryFeed(ctx context.Context, f feedFilter, limit int) ([]feedRow, error) {
	where := []string{"1=1"}
	var args []any
	if f.Kind != "" {
		where = append(where, "t.kind = ?")
		args = append(args, f.Kind)
	}
	if f.CharID != 0 {
		where = append(where, "t.characterID = ?")
		args = append(args, f.CharID)
	}
	if f.Text != "" {
		where = append(where, "(t.typeName LIKE ? OR t.note LIKE ?)")
		args = append(args, "%"+f.Text+"%", "%"+f.Text+"%")
	}
	if f.SystemID != 0 {
		where = append(where, "t.systemID = ?")
		args = append(args, f.SystemID)
	}
	if f.After > 0 {
		where = append(where, "t.tradeID > ?")
		args = append(args, f.After)
	}
	if f.Before > 0 {
		where = append(where, "t.tradeID < ?")
		args = append(args, f.Before)
	}
	args = append(args, limit)
	// A Newer page walks up from its cursor and is reversed below so the
	// page still reads newest-first; everything else walks down.
	order := "DESC"
	if f.Newer {
		order = "ASC"
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT t.tradeID, t.ts, t.characterID, COALESCE(c.characterName, ''), t.kind,
		       t.typeID, t.typeName, t.quantity, t.price, t.value, t.profit,
		       COALESCE(ms.solarSystemName, ''), COALESCE(fs.solarSystemName, ''),
		       COALESCE(sta.stationName, ''), t.note
		  FROM rolebot_trades t
		  LEFT JOIN character_ c ON c.characterID = t.characterID
		  LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = t.systemID
		  LEFT JOIN mapSolarSystems fs ON fs.solarSystemID = t.fromSystemID
		  LEFT JOIN staStations sta ON sta.stationID = t.stationID
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY t.tradeID `+order+` LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("trade feed: %w", err)
	}
	defer rows.Close()
	out := []feedRow{}
	for rows.Next() {
		var r feedRow
		var ts int64
		var price, value float64
		if err := rows.Scan(&r.ID, &ts, &r.CharID, &r.Who, &r.Kind, &r.TypeID, &r.TypeName,
			&r.Qty, &price, &value, &r.Profit, &r.Where, &r.From, &r.Station, &r.Note); err != nil {
			return nil, err
		}
		t := time.Unix(ts, 0).UTC()
		r.When, r.Full = t.Format("15:04:05"), t.Format("2006-01-02 15:04:05 UTC")
		r.Price, r.Value = iskShort(price), iskShort(value)
		if r.Profit != 0 {
			r.ProfitS = iskSigned(r.Profit)
		}
		out = append(out, r)
	}
	if f.Newer {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, rows.Err()
}

// feedTotals is one window's arithmetic over the book: what the roster bought, sold,
// listed and made.
type feedTotals struct {
	Bought  string `json:"bought"`
	Sold    string `json:"sold"`
	Listed  string `json:"listed"`
	Profit  string `json:"profit"`
	Runs    int64  `json:"runs"`
	Fills   int64  `json:"fills"`
	Resting string `json:"resting"`
	Orders  int64  `json:"orders"`
}

// feedSummary is the two windows the tiles show, memoised a minute: it is the same for every filter, and a
// seven-day sum is too slow to recompute on every poll.
type feedSummary struct {
	Day, Week feedTotals
	BuiltAt   int64
}

func (m *Mod) feedSummary(ctx context.Context, fresh bool) (*feedSummary, error) {
	v, at, err := econMemoGet("feed", fresh, func() (*feedSummary, error) {
		return m.buildFeedSummary(ctx)
	})
	if v != nil {
		v.BuiltAt = at
	}
	return v, err
}

func (m *Mod) buildFeedSummary(ctx context.Context) (*feedSummary, error) {
	now := clock.Unix()
	day, week := now-86400, now-7*86400
	// A `haul` row summarises a completed run and carries the same money as the `sell` row
	// that produced it. It counts toward the run count only; summing profit or value over
	// every kind would double them.
	var out feedSummary
	var dB, dS, dL, dP, wB, wS, wL, wP sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN ts > ? AND kind = 'buy' THEN value END), 0),
		       COALESCE(SUM(CASE WHEN ts > ? AND kind IN ('sell','fill') THEN value END), 0),
		       COALESCE(SUM(CASE WHEN ts > ? AND kind = 'list' THEN value END), 0),
		       COALESCE(SUM(CASE WHEN ts > ? AND kind <> 'haul' THEN profit END), 0),
		       COALESCE(SUM(CASE WHEN ts > ? AND kind = 'haul' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ts > ? AND kind = 'fill' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN kind = 'buy' THEN value END), 0),
		       COALESCE(SUM(CASE WHEN kind IN ('sell','fill') THEN value END), 0),
		       COALESCE(SUM(CASE WHEN kind = 'list' THEN value END), 0),
		       COALESCE(SUM(CASE WHEN kind <> 'haul' THEN profit END), 0),
		       COALESCE(SUM(CASE WHEN kind = 'haul' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN kind = 'fill' THEN 1 ELSE 0 END), 0)
		  FROM rolebot_trades WHERE ts > ?`,
		day, day, day, day, day, day, week).
		Scan(&dB, &dS, &dL, &dP, &out.Day.Runs, &out.Day.Fills,
			&wB, &wS, &wL, &wP, &out.Week.Runs, &out.Week.Fills); err != nil {
		return nil, fmt.Errorf("feed summary: %w", err)
	}
	out.Day.Bought, out.Day.Sold, out.Day.Listed = iskShort(nullISK(dB)), iskShort(nullISK(dS)), iskShort(nullISK(dL))
	out.Day.Profit = iskSigned(nullISK(dP))
	out.Week.Bought, out.Week.Sold, out.Week.Listed = iskShort(nullISK(wB)), iskShort(nullISK(wS)), iskShort(nullISK(wL))
	out.Week.Profit = iskSigned(nullISK(wP))

	var resting sql.NullFloat64
	var orders int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(o.price * o.volRemaining), 0)
		  FROM rolebot_orders o WHERE o.closedAt = 0`).Scan(&orders, &resting); err != nil {
		return nil, fmt.Errorf("feed resting: %w", err)
	}
	out.Day.Orders, out.Week.Orders = orders, orders
	out.Day.Resting, out.Week.Resting = iskShort(nullISK(resting)), iskShort(nullISK(resting))
	return &out, nil
}

// iskShort renders money the way a trader reads it.
func iskShort(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	var s string
	switch {
	case v >= 1e9:
		s = fmt.Sprintf("%.2fb", v/1e9)
	case v >= 1e6:
		s = fmt.Sprintf("%.2fm", v/1e6)
	case v >= 1e3:
		s = fmt.Sprintf("%.1fk", v/1e3)
	default:
		s = fmt.Sprintf("%.0f", v)
	}
	if neg {
		return "-" + s
	}
	return s
}

func iskSigned(v float64) string {
	if v >= 0 {
		return "+" + iskShort(v)
	}
	return iskShort(v)
}

// ── handlers ────────────────────────────────────────────────────────────────

// hubRow is one line of either page's table: the front page reads per region, the region
// page per system. Numbers are pre-rendered here. It has no total order count or book,
// because the NPC seeder's rows sit at every station; hubRaw gives it a small market share.
type hubRow struct {
	ID                                          int64
	Name, Where, Sec                            string
	Score, Traffic, Lane, Market, Flow          string
	StationT, Topo, SecT, Base                  string
	Stock, Traded                               string
	School                                      string // "school" for a chargen starter system
	Gates                                       int
	PlayerOrders                                int64
	Homes, Factories, Stations, Systems, Pilots int

	// The same numbers unformatted, for the data-v sort keys. Security is displayed rounded and
	// the book abbreviated, so sorting on the text would order them wrong.
	SecV, ScoreV, DepthV, BookV float64
	AnchorV                     int // homes + factories, the sort key behind "2 home 1 factory"
}

// handleEconPurge is the destructive half of the Trade Hubs page. It sits behind ?purge=1
// because its counts are ten unfiltered COUNT(*) queries over the module's busiest tables.
func (m *Mod) handleEconPurge(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	const self = "/rolebots/trademap?purge=1"

	got := strings.ToUpper(strings.Join(strings.Fields(r.FormValue("confirm")), " "))
	if got != econPurgeConfirm {
		redirect(w, r, self, "", fmt.Sprintf(
			"not confirmed — type %s exactly into the confirmation box", econPurgeConfirm))
		return
	}
	scope := EconPurgeScope{
		Learned: r.FormValue("learned") != "",
		History: r.FormValue("history") != "",
		Orders:  r.FormValue("orders") != "",
	}
	res, err := m.PurgeEconomy(ctx, scope)
	if err != nil {
		redirect(w, r, self, "", err.Error())
		return
	}
	rlogf("rolebots: economy purge (learned=%v history=%v): %d row(s)",
		scope.Learned, scope.History, res.Total)
	redirect(w, r, self, econPurgeSummary(res), "")
}

// handleTradeMapRegion is the drill-down: one region, its real gate graph,
// and the score breakdown per system.
func (m *Mod) handleTradeMapRegion(w http.ResponseWriter, r *http.Request, regionID int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var regionName string
	err := m.db.QueryRowContext(ctx,
		`SELECT regionName FROM mapRegions WHERE regionID = ?`, regionID).Scan(&regionName)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	} else if err != nil {
		espError(w, "rolebot-trademap", err.Error())
		return
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT h.systemID, ms.solarSystemName, COALESCE(c.constellationName, ''),
		       COALESCE(ms.security, 0), h.score, h.base,
		       h.trafficTerm, h.laneTerm, h.marketTerm, h.flowTerm,
		       h.stationTerm, h.topoTerm, h.secTerm, h.gates, h.playerOrders, h.playerISK,
		       h.homes, h.factories, h.stations, h.school,
		       COALESCE((SELECT SUM(l.isk) FROM rolebot_station_liquidity l
		                  WHERE l.systemID = h.systemID), 0),
		       h.updatedAt
		  FROM rolebot_hub_scores h
		  JOIN mapSolarSystems ms ON ms.solarSystemID = h.systemID
		  LEFT JOIN mapConstellations c ON c.constellationID = ms.constellationID
		 WHERE ms.regionID = ?
		 ORDER BY h.score DESC`, regionID)
	if err != nil {
		espError(w, "rolebot-trademap", err.Error())
		return
	}
	defer rows.Close()
	var top []hubRow
	var updated int64
	for rows.Next() {
		var h hubRow
		var sec, score, traffic, lane, market, flow, stationt, topo, sect, base, book, traded float64
		var school bool
		var at int64
		if err := rows.Scan(&h.ID, &h.Name, &h.Where, &sec, &score, &base,
			&traffic, &lane, &market, &flow, &stationt, &topo, &sect, &h.Gates,
			&h.PlayerOrders, &book,
			&h.Homes, &h.Factories, &h.Stations, &school, &traded,
			&at); err != nil {
			espError(w, "rolebot-trademap", err.Error())
			return
		}
		if at > updated {
			updated = at
		}
		h.Sec = fmt.Sprintf("%.1f", dispSec(sec))
		h.Score = fmt.Sprintf("%.0f", score)
		h.Traffic = fmt.Sprintf("%.0f", traffic)
		h.Lane = fmt.Sprintf("%.0f", lane)
		h.Market = fmt.Sprintf("%.0f", market)
		h.Flow = fmt.Sprintf("%.0f", flow)
		h.StationT = fmt.Sprintf("%+.0f", stationt)
		h.Topo = fmt.Sprintf("%+.0f", topo)
		h.SecT = fmt.Sprintf("%.0f", sect)
		h.Base = fmt.Sprintf("%.0f", base)
		h.Stock = iskShort(book)
		h.Traded = iskShort(traded)
		if school {
			h.School = "school"
		}
		top = append(top, h)
	}
	if err := rows.Err(); err != nil {
		espError(w, "rolebot-trademap", err.Error())
		return
	}

	swept := "never — the first sweep runs within five minutes of boot"
	if updated > 0 {
		swept = agoString(clock.Unix()-updated) + " ago"
	}
	docks, err := m.regionDockRows(ctx, regionID)
	if err != nil {
		espError(w, "rolebot-trademap", err.Error())
		return
	}

	espRender(w, "rolebots_trademap_region.html", map[string]any{
		"Page":     "rolebot-economy",
		"Tab":      "hubs",
		"Region":   regionName,
		"RegionID": regionID,
		"Top":      top,
		"Docks":    docks,
		"Swept":    swept,
		"HasAny":   len(top) > 0,
	})
}

// dockRow is one station's standing in its own system's contest.
type dockRow struct {
	Name, System           string
	Score, Amenity, Traded string
	Guest, Book, Services  string
	ShopGap                string // types shoppers found short here (stationdemand.go)
	Homes, Factories       int
	Winner                 bool
}

// regionDockRows lists every scored station in a region, its system's
// winner first within each system so the table reads as the contest it is.
func (m *Mod) regionDockRows(ctx context.Context, regionID int64) ([]dockRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, st.stationName, COALESCE(ms.solarSystemName, ''), s.systemID,
		       s.score, s.amenityTerm, s.liquidityTerm, s.guestTerm, s.bookTerm,
		       s.services, s.homes, s.factories, s.raw
		  FROM rolebot_station_scores s
		  JOIN staStations st ON st.stationID = s.stationID
		  LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = s.systemID
		 WHERE st.regionID = ?
		 ORDER BY s.systemID, s.raw DESC`, regionID)
	if err != nil {
		return nil, fmt.Errorf("region docks: %w", err)
	}
	defer rows.Close()
	// Display only: how many types shoppers came for here and found short.
	short := map[int64]int{}
	for key, gap := range m.stationDemand(ctx).Gap {
		if gap >= 1 {
			short[key[1]]++
		}
	}
	var out []dockRow
	lastSystem := int64(-1)
	for rows.Next() {
		var d dockRow
		var stationID, systemID, services int64
		var score, amenity, traded, guest, book, raw float64
		if err := rows.Scan(&stationID, &d.Name, &d.System, &systemID,
			&score, &amenity, &traded, &guest, &book,
			&services, &d.Homes, &d.Factories, &raw); err != nil {
			return nil, err
		}
		// Ordered by (system, raw DESC), so the first row of each system is its winner: the dock
		// bestDockPerSystem would pick for a pilot with no preference.
		d.Winner = systemID != lastSystem
		lastSystem = systemID
		d.Score = fmt.Sprintf("%.0f", score)
		d.Amenity = fmt.Sprintf("%.0f", amenity)
		d.Traded = fmt.Sprintf("%.0f", traded)
		d.Guest = fmt.Sprintf("%.0f", guest)
		d.Book = fmt.Sprintf("%.0f", book)
		d.Services = amenityLabel(services)
		if n := short[stationID]; n > 0 {
			d.ShopGap = fmt.Sprintf("%d", n)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// amenityLabel names the exclusive services a station carries. Universal ones are left out
// because they would print on every row.
func amenityLabel(services int64) string {
	var have []string
	for _, a := range stationAmenities {
		if services&a.Bit != 0 {
			have = append(have, a.Name)
		}
	}
	return strings.Join(have, ", ")
}

func (m *Mod) handleTradeMapAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	pilots := r.URL.Query().Get("pilots") == "1"

	var data map[string]any
	var err error
	if q := r.URL.Query().Get("region"); q != "" {
		var regionID int64
		regionID, err = strconv.ParseInt(q, 10, 64)
		if err != nil {
			http.Error(w, "bad region", http.StatusBadRequest)
			return
		}
		data, err = m.tradeMapRegion(ctx, regionID, pilots)
	} else {
		data, err = m.tradeMapRegions(ctx, pilots)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(data)
}

func (m *Mod) handleTradeFeed(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	f := m.feedFilterFrom(ctx, r)
	rows, err := m.queryFeed(ctx, f, feedPageSize)
	if err != nil {
		espError(w, "rolebot-trades", err.Error())
		return
	}
	sum, err := m.feedSummary(ctx, r.URL.Query().Get("refresh") == "1")
	if err != nil && sum == nil {
		espError(w, "rolebot-trades", err.Error())
		return
	}
	var firstID, lastID int64
	if len(rows) > 0 {
		firstID, lastID = rows[0].ID, rows[len(rows)-1].ID
	}
	paged := f.Before > 0 || f.After > 0
	older, newer := pagerLinks("/rolebots/trades", f.query(), firstID, lastID, len(rows), feedPageSize, paged)
	espRender(w, "rolebots_trades.html", map[string]any{
		"Page":    "rolebot-economy",
		"Tab":     "feed",
		"Rows":    rows,
		"Summary": sum,
		"Kind":    f.Kind,
		"Who":     f.Who,
		"Text":    f.Text,
		"Query":   f.query(),
		"Older":   older,
		"Newer":   newer,
		"Paged":   paged,
		"Kinds": []struct{ Key, Label string }{
			{tradeKindBuy, "bought"}, {tradeKindSell, "sold"}, {tradeKindList, "listed"},
			{tradeKindFill, "filled"}, {tradeKindPrice, "repriced"},
			{tradeKindCancel, "pulled"}, {tradeKindHaul, "runs"},
			{tradeKindFee, "broker fees"},
		},
	})
}

func (m *Mod) handleTradeFeedAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// Rows only. The tiles are memoised and drawn at page load; the poll is for new rows.
	f := m.feedFilterFrom(ctx, r)
	f.Newer = false
	rows, err := m.queryFeed(ctx, f, feedPageSize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"rows": rows,
		"now":  time.Now().UTC().Format("15:04:05 UTC"),
	})
}
