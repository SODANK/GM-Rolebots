package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Traffic, lanes and the trade map. Every gate a bot goes through is one arrival in the destination system and one
// crossing of the lane between the two (setSystem); every buy, sale and listing is money changing hands in a system.
// Both decay on a half-life.
//
// The map describes and never steers. The hub score is still computed and drawn on the ESP trade map, but nothing in
// the module reads it to decide anything: it is built from the roster's own behaviour, so reading it back as a reason
// to go somewhere is a closed loop. Decisions read measured liquidity (liquidity.go), this world's luck and the human
// book (fortune.go), and the employer picks where a pilot lives (corphome.go, moveout.go). A system with no station has
// no score at all, since nothing can be traded there.

const (
	// trafficHalfLifeSec is how fast the roster's movement is forgotten: two days, so a lane worked over an evening
	// still reads as a lane next morning and a route abandoned a week ago stops showing.
	trafficHalfLifeSec = 48 * 3600.0

	// hubSweepEveryTicks is the sweep interval in ticks of the 1 s tick manager. Each sweep costs one aggregate pass
	// over market_orders, and the numbers move slowly.
	hubSweepEveryTicks = 1800
)

// Weights of the map's terms. Each is a log of its own quantity, so a weight is what one e-fold of that quantity
// is worth against another. They decide nothing but the shading of the ESP map.
const (
	hubWeightTraffic = 26.0
	hubWeightLane    = 18.0
	hubWeightFlow    = 20.0

	// The book, split by who wrote it. A player or rolebot listing is somebody's own money committed; an NPC seed is a
	// fixture every station carries. Capped so the seeder can shade a station and never light one up.
	hubWeightPlayerOrders = 44.0
	hubWeightPlayerBook   = 26.0
	hubWeightNPCOrders    = 8.0
	hubWeightNPCBook      = 5.0
	hubNPCTermCap         = 6.0

	// Topology, penalty-only: a dead end loses about 21 points, a junction
	// gains nothing (it earns its standing through measured traffic).
	hubWeightGates = 16.0
	hubGateNeutral = 2.5

	// Docks: a log of the count up to the sweet spot, then a flat cost per
	// station beyond it for the split it forces on the book.
	hubWeightStations = 12.0
	hubStationSweet   = 5.0
	hubStationCrowd   = 5.0

	// Security: a step at the CONCORD line, where a hauler stops being able
	// to call the police.
	hubWeightSec = 24.0
	highsecLine  = 0.45
)

// laneKey normalises a pair of systems into the undirected lane between
// them. A route flown one way and the empty leg back are the same lane.
func laneKey(a, b int64) (int64, int64) {
	if a > b {
		return b, a
	}
	return a, b
}

// recordJump books one gate transit: an arrival in `to`, and one crossing of the lane. Best-effort: a bot's flight
// is not failed over a ledger write, and the ledger is a measurement, not a source of truth.
func (m *Mod) recordJump(ctx context.Context, from, to int64) {
	if from == 0 || to == 0 || from == to {
		return
	}
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_traffic_systems (systemID, jumps, decayedAt, lastAt)
		VALUES (?, 1, ?, ?)
		ON DUPLICATE KEY UPDATE jumps = jumps + 1, lastAt = VALUES(lastAt)`,
		to, now, now); err != nil {
		rlogf("rolebots: traffic system %d: %v", to, err)
		return
	}
	lo, hi := laneKey(from, to)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_traffic_lanes (fromSystemID, toSystemID, jumps, decayedAt, lastAt)
		VALUES (?, ?, 1, ?, ?)
		ON DUPLICATE KEY UPDATE jumps = jumps + 1, lastAt = VALUES(lastAt)`,
		lo, hi, now, now); err != nil {
		rlogf("rolebots: traffic lane %d-%d: %v", lo, hi, err)
	}
}

// recordMarketFlow books money changing hands in a system: which side of
// the book it was and what it was worth. Called from the trade book, so
// every leg that reaches the ledger reaches the map.
func (m *Mod) recordMarketFlow(ctx context.Context, systemID int64, buy bool, isk float64) {
	if systemID == 0 || isk <= 0 {
		return
	}
	col := "sells"
	if buy {
		col = "buys"
	}
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_traffic_systems (systemID, `+col+`, iskMoved, decayedAt, lastAt)
		VALUES (?, 1, ?, ?, ?)
		ON DUPLICATE KEY UPDATE `+col+` = `+col+` + 1,
			iskMoved = iskMoved + VALUES(iskMoved), lastAt = VALUES(lastAt)`,
		systemID, isk, now, now); err != nil {
		rlogf("rolebots: traffic flow %d: %v", systemID, err)
	}
}

// recordHaul books a completed haul against the lane it was flown over:
// the two ENDS of the run, not every gate between them. A run's endpoints
// are what makes a trade route; the gates in the middle already counted
// themselves as jumps on the way past.
func (m *Mod) recordHaul(ctx context.Context, fromSys, toSys int64, isk float64) {
	if fromSys == 0 || toSys == 0 || fromSys == toSys {
		return
	}
	lo, hi := laneKey(fromSys, toSys)
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_traffic_lanes (fromSystemID, toSystemID, hauls, iskMoved, decayedAt, lastAt)
		VALUES (?, ?, 1, ?, ?, ?)
		ON DUPLICATE KEY UPDATE hauls = hauls + 1,
			iskMoved = iskMoved + VALUES(iskMoved), lastAt = VALUES(lastAt)`,
		lo, hi, isk, now, now); err != nil {
		rlogf("rolebots: traffic haul %d-%d: %v", lo, hi, err)
	}
}

// ── decay ───────────────────────────────────────────────────────────────────

// decayTraffic ages every row toward zero by its own elapsed time. Done in
// SQL over the whole table rather than per row on read: a decayed value
// read twice must be the same number, and lazily decaying on read makes it
// depend on who looked and when.
func (m *Mod) decayTraffic(ctx context.Context) error {
	now := clock.Unix()
	// POWER(2, -(now - decayedAt)/halfLife). A row written this instant
	// (decayedAt = now) is untouched, which keeps the sweep idempotent.
	factor := fmt.Sprintf("POWER(2, -GREATEST(0, %d - decayedAt) / %f)", now, trafficHalfLifeSec)
	for _, q := range []string{
		`UPDATE rolebot_traffic_systems
		    SET jumps = jumps * ` + factor + `, docks = docks * ` + factor + `,
		        buys = buys * ` + factor + `, sells = sells * ` + factor + `,
		        iskMoved = iskMoved * ` + factor + `, decayedAt = ` + fmt.Sprint(now),
		`UPDATE rolebot_traffic_lanes
		    SET jumps = jumps * ` + factor + `, hauls = hauls * ` + factor + `,
		        iskMoved = iskMoved * ` + factor + `, decayedAt = ` + fmt.Sprint(now),
		// Footfall ages on the same half-life as everything else: a dock
		// busy last month is not a busy dock (stationscore.go).
		`UPDATE rolebot_traffic_stations
		    SET docks = docks * ` + factor + `, decayedAt = ` + fmt.Sprint(now),
		// Rows that have faded to nothing are noise on the map and rows in
		// the way of the next aggregate.
		`DELETE FROM rolebot_traffic_systems WHERE jumps < 0.01 AND buys < 0.01 AND sells < 0.01 AND iskMoved < 1`,
		`DELETE FROM rolebot_traffic_lanes WHERE jumps < 0.01 AND hauls < 0.01 AND iskMoved < 1`,
		`DELETE FROM rolebot_traffic_stations WHERE docks < 0.01`,
	} {
		if _, err := m.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("decayTraffic: %w", err)
		}
	}
	return nil
}

// ── the score ───────────────────────────────────────────────────────────────

// HubScore is one system's standing on the ESP trade map. It is descriptive: nothing in the module reads it to
// decide anything.
type HubScore struct {
	SystemID  int64
	Name      string
	RegionID  int64
	Security  float64
	Traffic   float64 // decayed arrivals
	LaneW     float64 // decayed weight of the gates either side
	Flow      float64 // ISK the roster has moved here
	Homes     int     // for the map only, not in the score
	Factories int     // for the map only, not in the score
	Stations  int
	Gates     int  // stargate degree, 1 is a dead end
	School    bool // a chargen starter system, for the map only

	// The book, split by who wrote it. Orders/BookISK are the totals and
	// exist for display; the score reads the halves.
	PlayerOrders int64
	PlayerISK    float64
	NPCOrders    int64
	NPCISK       float64
	Orders       int64
	BookISK      float64
	// The real players' share of PlayerOrders/PlayerISK (fortune.go); weighs humanOrderWeight.
	HumanOrders int64
	HumanISK    float64

	// Luck is this world's draw for the system (systemFortune), stored in the noise column.
	// It is what the decisions read, and is not part of the score: the map stays descriptive.
	Luck float64

	TrafficTerm float64
	LaneTerm    float64
	MarketTerm  float64
	FlowTerm    float64
	StationTerm float64
	TopoTerm    float64
	SecTerm     float64

	Base  float64 // the sum of the terms, floored at zero
	Score float64 // 0..100, base scaled against the best system
}

// secBoost is what a system's security does to its shading on the map. A
// step at the CONCORD line rather than a slope through it.
func secBoost(sec float64) float64 {
	s := dispSec(sec)
	switch {
	case s >= highsecLine:
		return 0.6 + 0.4*math.Min(1, (s-highsecLine)/0.55)
	case s > 0:
		return 0.25 * (s / highsecLine)
	default:
		return 0
	}
}

// hubRaw is the map arithmetic, kept separate so the harness can assert it without a database. Every measured term
// is a log of its quantity, so one busy system cannot flatten the map. Topology, security and the dock count are
// the only terms that can be negative.
func hubRaw(h *HubScore) {
	h.Orders = h.PlayerOrders + h.NPCOrders
	h.BookISK = h.PlayerISK + h.NPCISK

	h.TrafficTerm = hubWeightTraffic * math.Log1p(h.Traffic)
	h.LaneTerm = hubWeightLane * math.Log1p(h.LaneW)
	npc := hubWeightNPCOrders*math.Log1p(float64(h.NPCOrders)) +
		hubWeightNPCBook*math.Log1p(h.NPCISK/1e6)
	if npc > hubNPCTermCap {
		npc = hubNPCTermCap
	}
	// A real player's order counts humanOrderWeight bot orders (fortune.go).
	h.MarketTerm = hubWeightPlayerOrders*math.Log1p(humanWeighted(float64(h.PlayerOrders), float64(h.HumanOrders))) +
		hubWeightPlayerBook*math.Log1p(humanWeighted(h.PlayerISK, h.HumanISK)/1e6) + npc
	h.FlowTerm = hubWeightFlow * math.Log1p(h.Flow/1e6)
	docks := math.Min(float64(h.Stations), hubStationSweet)
	h.StationTerm = hubWeightStations * math.Log1p(docks)
	if over := float64(h.Stations) - hubStationSweet; over > 0 {
		h.StationTerm -= hubStationCrowd * over
	}
	// A system with no recorded gates is one the map data does not cover;
	// it is not a dead end, so it is floored at one.
	h.TopoTerm = hubWeightGates * math.Log2(math.Max(float64(h.Gates), 1)/hubGateNeutral)
	if h.TopoTerm > 0 {
		h.TopoTerm = 0
	}
	h.SecTerm = hubWeightSec * secBoost(h.Security)

	h.Base = h.TrafficTerm + h.LaneTerm + h.MarketTerm +
		h.FlowTerm + h.StationTerm + h.TopoTerm + h.SecTerm
	if h.Base < 0 {
		h.Base = 0
	}
}

// systemGraph reads the whole stargate topology once: system to its deduplicated neighbours. A pair of systems
// joined by two gates counts once, since a degree that counts gates rather than neighbours would call a corridor
// a crossroads.
func (m *Mod) systemGraph(ctx context.Context) (map[int64][]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT fromSolarSystemID, toSolarSystemID FROM mapSolarSystemJumps`)
	if err != nil {
		return nil, fmt.Errorf("system graph: %w", err)
	}
	defer rows.Close()
	adj := map[int64][]int64{}
	seen := map[[2]int64]bool{}
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		if a == 0 || b == 0 || a == b || seen[[2]int64{a, b}] {
			continue
		}
		seen[[2]int64{a, b}] = true
		adj[a] = append(adj[a], b)
		// The dump is symmetric, but a half-populated one would make every system a dead end, so the reverse edge is
		// written here too and the seen set keeps it from doubling.
		if !seen[[2]int64{b, a}] {
			seen[[2]int64{b, a}] = true
			adj[b] = append(adj[b], a)
		}
	}
	return adj, rows.Err()
}

// computeHubScores builds the whole table in memory. Station systems only:
// a system nobody can dock in is not a place to trade whatever crosses it,
// and letting one into the set would also move the scale every other
// system is drawn against.
func (m *Mod) computeHubScores(ctx context.Context) ([]*HubScore, error) {
	byID := map[int64]*HubScore{}

	// The candidate set, and the two static facts the map needs anyway.
	rows, err := m.db.QueryContext(ctx, `
		SELECT ms.solarSystemID, ms.solarSystemName, COALESCE(ms.regionID, 0),
		       COALESCE(ms.security, 0), COUNT(s.stationID)
		  FROM mapSolarSystems ms
		  JOIN staStations s ON s.solarSystemID = ms.solarSystemID
		 GROUP BY ms.solarSystemID, ms.solarSystemName, ms.regionID, ms.security`)
	if err != nil {
		return nil, fmt.Errorf("hub candidates: %w", err)
	}
	for rows.Next() {
		h := &HubScore{}
		if err := rows.Scan(&h.SystemID, &h.Name, &h.RegionID, &h.Security, &h.Stations); err != nil {
			rows.Close()
			return nil, err
		}
		byID[h.SystemID] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(byID) == 0 {
		return nil, nil
	}

	// Each fact is one aggregate over the whole database, applied to the candidate set in Go; asked per system it
	// would be thousands of round trips per sweep.
	scan := func(query string, apply func(h *HubScore, a, b float64)) error {
		r, err := m.db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("hub scan: %w", err)
		}
		defer r.Close()
		for r.Next() {
			var sys int64
			var a, b float64
			if err := r.Scan(&sys, &a, &b); err != nil {
				return err
			}
			if h, ok := byID[sys]; ok {
				apply(h, a, b)
			}
		}
		return r.Err()
	}

	if err := scan(`
		SELECT systemID, jumps, iskMoved FROM rolebot_traffic_systems`,
		func(h *HubScore, jumps, isk float64) { h.Traffic, h.Flow = jumps, isk }); err != nil {
		return nil, err
	}
	// A lane counts for BOTH its ends: being on the way somewhere is what
	// the term measures, and a lane has two ways.
	if err := scan(`
		SELECT sysID, SUM(w), 0 FROM (
			SELECT fromSystemID AS sysID, jumps + hauls * 3 AS w FROM rolebot_traffic_lanes
			UNION ALL
			SELECT toSystemID AS sysID, jumps + hauls * 3 AS w FROM rolebot_traffic_lanes
		) t GROUP BY sysID`,
		func(h *HubScore, w, _ float64) { h.LaneW = w }); err != nil {
		return nil, err
	}
	// The book, split by author: a player or rolebot order is a person with money out, while the NPC seeder
	// characters (all three, liquidityExcluded: the resale shelf too) own the furniture every station
	// carries, which says nothing about anywhere.
	if err := scan(`
		SELECT solarSystemID, COUNT(*), COALESCE(SUM(price * volRemaining), 0)
		  FROM market_orders
		 WHERE orderState = 1 AND volRemaining > 0 AND charID NOT IN (`+liquidityExcluded+`)
		 GROUP BY solarSystemID`,
		func(h *HubScore, n, isk float64) { h.PlayerOrders, h.PlayerISK = int64(n), isk }); err != nil {
		return nil, err
	}
	if err := scan(`
		SELECT solarSystemID, COUNT(*), COALESCE(SUM(price * volRemaining), 0)
		  FROM market_orders
		 WHERE orderState = 1 AND volRemaining > 0 AND charID IN (`+liquidityExcluded+`)
		 GROUP BY solarSystemID`,
		func(h *HubScore, n, isk float64) { h.NPCOrders, h.NPCISK = int64(n), isk }); err != nil {
		return nil, err
	}
	// The real players' share of the player book (fortune.go).
	if err := scan(`
		SELECT o.solarSystemID, COUNT(*), COALESCE(SUM(o.price * o.volRemaining), 0)
		  FROM market_orders o
		  LEFT JOIN rolebot_characters b ON b.characterID = o.charID
		 WHERE `+humanOrdersFilter+`
		 GROUP BY o.solarSystemID`,
		func(h *HubScore, n, isk float64) { h.HumanOrders, h.HumanISK = int64(n), isk }); err != nil {
		return nil, err
	}
	// Where the roster lives and builds, for the map to show and not in the score. A pilot has two addresses: its
	// home, which is its medical clone station (character_.homeStationID), and its industry base.
	if err := scan(`
		SELECT s.solarSystemID, COUNT(*), 0
		  FROM character_ c
		  JOIN rolebot_characters b ON b.characterID = c.characterID
		  JOIN staStations s ON s.stationID = c.homeStationID
		 GROUP BY s.solarSystemID`,
		func(h *HubScore, n, _ float64) { h.Homes = int(n) }); err != nil {
		return nil, err
	}
	if err := scan(`
		SELECT s.solarSystemID, COUNT(*), 0
		  FROM rolebot_profiles p
		  JOIN rolebot_characters b ON b.characterID = p.characterID
		  JOIN staStations s ON s.stationID = p.industryStationID
		 GROUP BY s.solarSystemID`,
		func(h *HubScore, n, _ float64) { h.Factories = int(n) }); err != nil {
		return nil, err
	}

	// The chargen starter systems, marked on the map (schools.go).
	sch, err := m.schools(ctx)
	if err != nil {
		return nil, err
	}
	for sys := range sch.Systems {
		if h, ok := byID[sys]; ok {
			h.School = true
		}
	}

	// Topology.
	adj, err := m.systemGraph(ctx)
	if err != nil {
		return nil, err
	}
	for id, h := range byID {
		h.Gates = len(adj[id])
	}

	luck := m.systemFortune(ctx)
	out := make([]*HubScore, 0, len(byID))
	best := 0.0
	for _, h := range byID {
		h.Luck = luck(h.SystemID)
		hubRaw(h)
		if h.Base > best {
			best = h.Base
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SystemID < out[j].SystemID })
	// Normalised against the best system in the universe, so score 100 always means the busiest place there is.
	for _, h := range out {
		if best > 0 {
			h.Score = 100 * h.Base / best
		}
	}
	return out, nil
}

// hubStoreBatch is how many systems go in one INSERT. Every station system in the universe is rewritten each
// sweep, so one statement per row would be thousands of round trips for numbers nobody is waiting on.
const hubStoreBatch = 250

// storeHubScores replaces the stored table with a freshly computed one.
func (m *Mod) storeHubScores(ctx context.Context, scores []*HubScore) error {
	now := clock.Unix()
	const cols = 27
	row := "(" + strings.TrimSuffix(strings.Repeat("?,", cols), ",") + ")"
	for start := 0; start < len(scores); start += hubStoreBatch {
		end := start + hubStoreBatch
		if end > len(scores) {
			end = len(scores)
		}
		chunk := scores[start:end]
		vals := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*cols)
		for _, h := range chunk {
			vals = append(vals, row)
			// anchorTerm, secUnit, shadow, shadowBy and schoolCut are inert since the score stopped steering anything.
			// The columns stay and are written zero so an old row cannot read as a live one; raw carries base.
			// noise carries this world's luck for the system in home points (fortune.go): what the decisions
			// read, shown for the operator, and not added to base or score.
			args = append(args,
				h.SystemID, h.TrafficTerm, h.LaneTerm, h.MarketTerm, 0.0, h.FlowTerm,
				h.StationTerm, h.TopoTerm, h.SecTerm, 0.0, h.Base, 0.0, int64(0),
				h.Base, h.Score, h.Stations, h.Gates, h.Orders, h.BookISK,
				h.PlayerOrders, h.PlayerISK,
				h.Homes, h.Factories, h.School, 0.0, fortuneHomePts*h.Luck, now)
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_hub_scores
				(systemID, trafficTerm, laneTerm, marketTerm, anchorTerm, flowTerm,
				 stationTerm, topoTerm, secTerm, secUnit, base, shadow, shadowBy,
				 raw, score, stations, gates, orders, bookISK, playerOrders, playerISK,
				 homes, factories, school, schoolCut, noise, updatedAt)
			VALUES `+strings.Join(vals, ",")+`
			ON DUPLICATE KEY UPDATE
				trafficTerm = VALUES(trafficTerm), laneTerm = VALUES(laneTerm),
				marketTerm = VALUES(marketTerm), anchorTerm = VALUES(anchorTerm),
				flowTerm = VALUES(flowTerm), stationTerm = VALUES(stationTerm),
				topoTerm = VALUES(topoTerm),
				secTerm = VALUES(secTerm), secUnit = VALUES(secUnit),
				base = VALUES(base), shadow = VALUES(shadow), shadowBy = VALUES(shadowBy),
				raw = VALUES(raw), score = VALUES(score),
				stations = VALUES(stations), gates = VALUES(gates),
				orders = VALUES(orders), bookISK = VALUES(bookISK),
				playerOrders = VALUES(playerOrders), playerISK = VALUES(playerISK),
				homes = VALUES(homes), factories = VALUES(factories),
				school = VALUES(school), schoolCut = VALUES(schoolCut),
				noise = VALUES(noise),
				updatedAt = VALUES(updatedAt)`, args...); err != nil {
			return fmt.Errorf("storeHubScores: %w", err)
		}
	}
	// A station that stopped existing (or a system that lost its last one)
	// must lose its score rather than keep the last one it was given.
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_hub_scores WHERE updatedAt < ?`, now); err != nil {
		return fmt.Errorf("storeHubScores prune: %w", err)
	}
	return nil
}

// ── the sweep ───────────────────────────────────────────────────────────────

var hubSweepBusy = make(chan struct{}, 1)

// startTradeSweeps registers the two background passes the trade economy
// needs: the map + liquidity recompute, and the resting-order
// reconciliation (tradebook.go). Both run off the tick goroutine behind
// their own self-overlap guard, because periodics fire synchronously.
func (m *Mod) startTradeSweeps() {
	m.registerPeriodic("rolebot-hub-sweep", hubSweepEveryTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		select {
		case hubSweepBusy <- struct{}{}:
		default:
			return true
		}
		goSafe("hub sweep", func() {
			defer func() { <-hubSweepBusy }()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			m.hubSweep(ctx)
		})
		return true
	})
	m.startOrderSweep()
}

// hubSweep ages the ledger, then runs three independent passes: the map (descriptive), measured liquidity (what
// decisions read, liquidity.go) and the dock contest inside each system (stationscore.go). A failure in one leaves
// the others running; the dock contest reads liquidity, so it runs after it.
func (m *Mod) hubSweep(ctx context.Context) {
	if err := m.decayTraffic(ctx); err != nil {
		rlogf("rolebots: %v", err)
	}
	if scores, err := m.computeHubScores(ctx); err != nil {
		rlogf("rolebots: hub sweep: %v", err)
	} else if err := m.storeHubScores(ctx, scores); err != nil {
		rlogf("rolebots: %v", err)
	}
	if err := m.refreshLiquidity(ctx); err != nil {
		rlogf("rolebots: liquidity sweep: %v", err)
	}
	if err := m.decayStationDemand(ctx); err != nil {
		rlogf("rolebots: %v", err)
	}
	stations, err := m.computeStationScores(ctx)
	if err != nil {
		rlogf("rolebots: station sweep: %v", err)
		return
	}
	if err := m.storeStationScores(ctx, stations); err != nil {
		rlogf("rolebots: %v", err)
		return
	}
	stationCacheMu.Lock()
	stationCache, stationCacheAt = indexStations(stations), clock.Now()
	stationCacheMu.Unlock()
}
