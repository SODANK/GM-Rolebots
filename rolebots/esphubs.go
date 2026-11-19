package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Trade Hubs: /rolebots/trademap, New Eden by region.
//
// The Economy section's Hubs tab. It reads the hub sweep's scores (rolebot_hub_scores, traffic.go):
// each region is a chip on a lattice laid out on the gate borders, coloured by its best system's
// score with the depth behind it, with the roster's lanes drawn over the roads. Clicking a region
// opens /rolebots/trademap/region/<id>, the gate-graph drill-down (esptrade.go). The map polls
// /rolebots/trademap/api. The economy wipe panel (?purge=1) lives here.

// mapRegion is one dot on the front page. A region's standing is its best system, not the sum of
// its systems. The sum is carried as depth, so one big station and one with a working hinterland
// can be told apart.
type mapRegion struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Col  int    `json:"col"`
	Row  int    `json:"row"`

	Score  float64 `json:"score"`  // the best system in it
	Depth  float64 `json:"depth"`  // every system's score added up
	BestID int64   `json:"bestID"` // and which system that was
	Best   string  `json:"best"`
	Sec    float64 `json:"sec"` // the best system's security

	Systems      int     `json:"systems"` // systems with a station
	PlayerOrders int64   `json:"orders"`  // players and rolebots only
	PlayerISK    float64 `json:"isk"`
	Homes        int     `json:"homes"`
	Factories    int     `json:"factories"`
	Pilots       int     `json:"pilots"`
	Bots         int     `json:"bots"`
}

// mapRegionLink is one road between two regions. Static adjacency is drawn whether or not the
// roster has used it, so the map shows New Eden and not only the lanes bots flew.
type mapRegionLink struct {
	A     int64   `json:"a"`
	B     int64   `json:"b"`
	Jumps float64 `json:"jumps"`
	Hauls float64 `json:"hauls"`
	ISK   float64 `json:"isk"`
	W     float64 `json:"w"` // 0..1 against the busiest pair

	// Road is true for two regions with a stargate between them, false for a trade route between
	// non-adjacent regions. Only roads lay out the map and are drawn solid, so a long haul cannot
	// bend the lattice.
	Road bool `json:"road"`
}

// tradeMapRegions builds the front page: New Eden by region.
func (m *Mod) tradeMapRegions(ctx context.Context, withPilots bool) (map[string]any, error) {
	regions := map[int64]*mapRegion{}
	coords := map[int64]xz{}

	// Every region with a name, whether or not anything in it scores, so the picture does not change
	// shape when a region gets its first bot.
	rrows, err := m.db.QueryContext(ctx, `
		SELECT regionID, regionName, COALESCE(x, 0), COALESCE(z, 0) FROM mapRegions`)
	if err != nil {
		return nil, fmt.Errorf("trade map regions: %w", err)
	}
	for rrows.Next() {
		r := &mapRegion{}
		var x, z float64
		if err := rrows.Scan(&r.ID, &r.Name, &x, &z); err != nil {
			rrows.Close()
			return nil, err
		}
		regions[r.ID], coords[r.ID] = r, xz{x, z}
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return nil, err
	}

	// The roll-up. Best system per region comes out of the same pass rather
	// than a second query with an ORDER BY per region.
	srows, err := m.db.QueryContext(ctx, `
		SELECT ms.regionID, h.systemID, COALESCE(ms.solarSystemName, ''),
		       COALESCE(ms.security, 0), h.score, h.playerOrders, h.playerISK,
		       h.homes, h.factories
		  FROM rolebot_hub_scores h
		  JOIN mapSolarSystems ms ON ms.solarSystemID = h.systemID`)
	if err != nil {
		return nil, fmt.Errorf("trade map rollup: %w", err)
	}
	for srows.Next() {
		var regionID, sysID, pOrders int64
		var name string
		var sec, score, book float64
		var homes, factories int
		if err := srows.Scan(&regionID, &sysID, &name, &sec, &score, &pOrders,
			&book, &homes, &factories); err != nil {
			srows.Close()
			return nil, err
		}
		r := regions[regionID]
		if r == nil {
			continue
		}
		r.Systems++
		r.Depth += score
		r.PlayerOrders += pOrders
		r.PlayerISK += book
		r.Homes += homes
		r.Factories += factories
		if score > r.Score || r.BestID == 0 {
			r.Score, r.BestID, r.Best, r.Sec = score, sysID, name, sec
		}
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}

	links, err := m.tradeMapRegionLinks(ctx)
	if err != nil {
		return nil, err
	}

	// Laid out on the lattice from the roads only (see mapRegionLink.Road).
	roads := make([][2]int64, 0, len(links))
	for _, l := range links {
		if l.Road {
			roads = append(roads, [2]int64{l.A, l.B})
		}
	}
	grid := cachedGridLayout(0, coords, roads)

	out := make([]*mapRegion, 0, len(regions))
	for _, r := range regions {
		c := grid.At[r.ID]
		r.Col, r.Row = c.Col, c.Row
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	data := map[string]any{
		"regions": out,
		"links":   links,
		"cols":    grid.Cols,
		"rows":    grid.Rows,
		"now":     clock.Now().UTC().Format("15:04:05 UTC"),
	}
	if withPilots {
		pilots, err := m.tradeMapPilots(ctx, 0)
		if err != nil {
			return nil, err
		}
		// A region page has no room for one dot per pilot, and would not be
		// readable if it did: the count is what is legible at this zoom.
		byRegion := map[int64]*mapRegion{}
		for _, r := range out {
			byRegion[r.ID] = r
		}
		for _, p := range pilots {
			if r := byRegion[p.RegionID]; r != nil {
				r.Pilots++
				if p.Bot {
					r.Bots++
				}
			}
		}
		data["pilots"] = len(pilots)
	}
	return data, nil
}

// tradeMapRegionLinks is the road network between regions: the static
// adjacency, weighted by whatever the roster has actually flown over it.
func (m *Mod) tradeMapRegionLinks(ctx context.Context) ([]mapRegionLink, error) {
	byPair := map[[2]int64]*mapRegionLink{}
	key := func(a, b int64) [2]int64 {
		if a > b {
			a, b = b, a
		}
		return [2]int64{a, b}
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT fromRegionID, toRegionID
		  FROM mapSolarSystemJumps
		 WHERE fromRegionID IS NOT NULL AND toRegionID IS NOT NULL
		   AND fromRegionID <> toRegionID`)
	if err != nil {
		return nil, fmt.Errorf("region links: %w", err)
	}
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			rows.Close()
			return nil, err
		}
		k := key(a, b)
		if byPair[k] == nil {
			byPair[k] = &mapRegionLink{A: k[0], B: k[1]}
		}
		byPair[k].Road = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// What the roster has done on those roads. Lanes inside one region are dropped (nowhere to draw
	// them at this zoom); a haul between two non-adjacent regions is kept as its own link.
	lrows, err := m.db.QueryContext(ctx, `
		SELECT COALESCE(a.regionID, 0), COALESCE(b.regionID, 0),
		       SUM(l.jumps), SUM(l.hauls), SUM(l.iskMoved)
		  FROM rolebot_traffic_lanes l
		  JOIN mapSolarSystems a ON a.solarSystemID = l.fromSystemID
		  JOIN mapSolarSystems b ON b.solarSystemID = l.toSystemID
		 WHERE a.regionID <> b.regionID
		 GROUP BY a.regionID, b.regionID`)
	if err != nil {
		return nil, fmt.Errorf("region lane weight: %w", err)
	}
	defer lrows.Close()
	for lrows.Next() {
		var a, b int64
		var jumps, hauls, isk float64
		if err := lrows.Scan(&a, &b, &jumps, &hauls, &isk); err != nil {
			return nil, err
		}
		k := key(a, b)
		l := byPair[k]
		if l == nil {
			l = &mapRegionLink{A: k[0], B: k[1]}
			byPair[k] = l
		}
		l.Jumps += jumps
		l.Hauls += hauls
		l.ISK += isk
	}
	if err := lrows.Err(); err != nil {
		return nil, err
	}

	out := make([]mapRegionLink, 0, len(byPair))
	heaviest := 0.0
	for _, l := range byPair {
		if w := l.Jumps + l.Hauls*3; w > heaviest {
			heaviest = w
		}
	}
	for _, l := range byPair {
		if heaviest > 0 {
			l.W = (l.Jumps + l.Hauls*3) / heaviest
		}
		out = append(out, *l)
	}
	return out, nil
}

type bestSystem struct {
	name string
	sec  float64
}

func (m *Mod) bestSystemPerRegion(ctx context.Context) (map[int64]bestSystem, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ms.regionID, ms.solarSystemName, COALESCE(ms.security, 0), h.score
		  FROM rolebot_hub_scores h
		  JOIN mapSolarSystems ms ON ms.solarSystemID = h.systemID
		 ORDER BY h.score DESC`)
	if err != nil {
		return nil, fmt.Errorf("best system per region: %w", err)
	}
	defer rows.Close()
	out := map[int64]bestSystem{}
	for rows.Next() {
		var region int64
		var name string
		var sec, score float64
		if err := rows.Scan(&region, &name, &sec, &score); err != nil {
			return nil, err
		}
		if _, seen := out[region]; !seen { // sorted, so the first is the best
			out[region] = bestSystem{name, sec}
		}
	}
	return out, rows.Err()
}

func (m *Mod) handleTradeMap(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if r.Method == http.MethodPost && r.FormValue("action") == "purgeecon" {
		m.handleEconPurge(w, r, ctx)
		return
	}

	// The table is server-rendered so the page says something before any JavaScript runs: one row
	// per region, ranked by its best system, with the depth alongside.
	rows, err := m.db.QueryContext(ctx, `
		SELECT ms.regionID, COALESCE(rg.regionName, ''),
		       COUNT(*), MAX(h.score), SUM(h.score),
		       SUM(h.playerOrders), SUM(h.playerISK),
		       SUM(h.homes), SUM(h.factories), MAX(h.updatedAt)
		  FROM rolebot_hub_scores h
		  JOIN mapSolarSystems ms ON ms.solarSystemID = h.systemID
		  LEFT JOIN mapRegions rg ON rg.regionID = ms.regionID
		 GROUP BY ms.regionID, rg.regionName
		 ORDER BY MAX(h.score) DESC`)
	if err != nil {
		espError(w, "rolebot-trademap", err.Error())
		return
	}
	defer rows.Close()
	var top []hubRow
	var updated int64
	for rows.Next() {
		var h hubRow
		var best, depth, book float64
		var at int64
		if err := rows.Scan(&h.ID, &h.Name, &h.Systems, &best, &depth,
			&h.PlayerOrders, &book,
			&h.Homes, &h.Factories, &at); err != nil {
			espError(w, "rolebot-trademap", err.Error())
			return
		}
		if at > updated {
			updated = at
		}
		h.Score = fmt.Sprintf("%.0f", best)
		h.Base = fmt.Sprintf("%.0f", depth)
		h.Stock = iskShort(book)
		h.ScoreV, h.DepthV, h.BookV = best, depth, book
		h.AnchorV = h.Homes + h.Factories
		top = append(top, h)
	}
	if err := rows.Err(); err != nil {
		espError(w, "rolebot-trademap", err.Error())
		return
	}

	// The best system in each region in one pass, not a correlated subquery per row; the name is
	// what makes the row mean anything.
	best, err := m.bestSystemPerRegion(ctx)
	if err != nil {
		espError(w, "rolebot-trademap", err.Error())
		return
	}
	for i := range top {
		if b, ok := best[top[i].ID]; ok {
			top[i].Where, top[i].Sec = b.name, fmt.Sprintf("%.1f", dispSec(b.sec))
			top[i].SecV = dispSec(b.sec)
		}
	}

	swept := "not yet"
	if updated > 0 {
		swept = agoString(clock.Unix()-updated) + " ago"
	}
	learnedRows, historyRows, restingOrders := m.econPurgeCounts(ctx)
	espRender(w, "rolebots_trademap.html", map[string]any{
		"Page":   "rolebot-economy",
		"Tab":    "hubs",
		"Top":    top,
		"Swept":  swept,
		"HasAny": len(top) > 0,
		// The purge panel. Counts first: a confirmation box in front of an
		// unknown quantity is not a confirmation.
		"ShowPurge":     r.URL.Query().Get("purge") == "1",
		"PurgeConfirm":  econPurgeConfirm,
		"LearnedRows":   learnedRows,
		"HistoryRows":   historyRows,
		"RestingOrders": restingOrders,
	})
}
