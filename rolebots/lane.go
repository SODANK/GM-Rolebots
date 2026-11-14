package rolebots

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	"gurimoon/destiny"
)

// Where a camp goes: the lane picker.
//
// A camp reads two ledgers to find the lanes haulers fly: rolebot_traffic_lanes
// (traffic.go: bot gate transits and completed hauls per lane, 48 h half-life)
// and the core's mapHistory (statType 4: gate jumps per system per hour).
//
// Three bands share the picker (laneBand): the gank wants a policed lane with a
// lawless end to stage strikers, the pirate camp wants the lawless end, and the
// checkpoint wants a lane in the employer's region. scoreLane and pickLane are
// pure; the two queries and the graph walk are the only database use.

// laneStats is one undirected lane as the ledgers record it.
type laneStats struct {
	A, B int64 // laneKey order
	// From rolebot_traffic_lanes, decayed: completed hauls ending at this lane's
	// two ends, bot gate transits across it, ISK carried.
	Hauls, Jumps, ISK float64
	// From mapHistory: gate jumps at both ends over the last laneMapHours, players
	// and bots together. The module's own ledger cannot see this half.
	MapJumps float64
	// From the killboard (gatekills.go), the week's kills at the lane's two gates.
	// Prey is industrial hulls that died crossing, the best proof a lane carries
	// cargo. Contested is every other hull: a fight at this gate, so a gang works it.
	Prey, Contested float64
	SecA, SecB      float64
	RegA, RegB      int64
}

// laneBand is what a camp is for, which decides which end it sits on.
type laneBand int

const (
	// laneGank: the camp (the scout) sits on a policed end. A lane whose other end
	// is lawless is preferred, since strikers stage there.
	laneGank laneBand = iota
	// lanePirate: the camp sits on a lawless end, where no police answer a
	// robbery. A lane with two lawless ends offers both.
	lanePirate
	// laneCheckpoint: the camp sits in the employer's own region, at any security.
	laneCheckpoint
)

// laneWeights are the terms of scoreLane, each a log of its quantity so one busy
// lane cannot dominate. Hauls lead, then map jumps, then the roster's own
// transits, then ISK as a tie-break. Crowd is a flat cost per camper on the lane.
type laneWeights struct {
	Haul, MapJump, Jump, ISK, Crowd float64
	// Staging is the gank's bonus for a lawless far end.
	Staging float64
	// Prey is the killboard's proof of cargo, just under Hauls: kills span a week
	// where hauls decay on 48 h. Contested is the cost of the fights at the gates.
	Prey, Contested float64
}

var defaultLaneWeights = laneWeights{Haul: 3, MapJump: 2, Jump: 1, ISK: 0.5, Crowd: 2, Staging: 1.5,
	Prey: 2.5, Contested: 1.5}

// scoreLane is the lane's worth to a camp, before the band decides which end.
func scoreLane(l laneStats, w laneWeights, campers int) float64 {
	return w.Haul*math.Log1p(math.Max(l.Hauls, 0)) +
		w.Prey*math.Log1p(math.Max(l.Prey, 0)) +
		w.MapJump*math.Log1p(math.Max(l.MapJumps, 0)) +
		w.Jump*math.Log1p(math.Max(l.Jumps, 0)) +
		w.ISK*math.Log1p(math.Max(l.ISK, 0)/1e6) -
		w.Contested*math.Log1p(math.Max(l.Contested, 0)) -
		w.Crowd*float64(campers)
}

// laneCamp is one admissible (camp end, far end) of a lane with its score.
type laneCamp struct {
	CampSys, FarSys int64
	Score           float64
}

// laneEnds is the pure band rule: which ends of a lane a camp of this band may
// sit on, and the far end for each. A lawless end is below highsecLine. home is
// the checkpoint's region set (nil = anywhere).
func laneEnds(l laneStats, band laneBand, home map[int64]bool) [][2]int64 {
	lawlessA, lawlessB := dispSec(l.SecA) < highsecLine, dispSec(l.SecB) < highsecLine
	var out [][2]int64
	switch band {
	case laneGank:
		if !lawlessA {
			out = append(out, [2]int64{l.A, l.B})
		}
		if !lawlessB {
			out = append(out, [2]int64{l.B, l.A})
		}
	case lanePirate:
		if lawlessA {
			out = append(out, [2]int64{l.A, l.B})
		}
		if lawlessB {
			out = append(out, [2]int64{l.B, l.A})
		}
	case laneCheckpoint:
		if len(home) == 0 || home[l.RegA] {
			out = append(out, [2]int64{l.A, l.B})
		}
		if len(home) == 0 || home[l.RegB] {
			out = append(out, [2]int64{l.B, l.A})
		}
	}
	return out
}

// pickLane is the whole decision, pure: every admissible end of every lane,
// scored with the band's staging bonus, filtered by admit (travel rules, crime
// band, reach) and recent (where the pilot just worked), best first. Ties keep
// the lower system ID so equal questions get equal answers.
func pickLane(lanes []laneStats, band laneBand, w laneWeights, campers map[int64]int,
	home map[int64]bool, admit func(sysID int64) bool, recent map[int64]bool) []laneCamp {

	var out []laneCamp
	for _, l := range lanes {
		for _, e := range laneEnds(l, band, home) {
			camp, far := e[0], e[1]
			if recent[camp] || (admit != nil && !admit(camp)) {
				continue
			}
			s := scoreLane(l, w, campers[camp])
			if band == laneGank {
				farSec := l.SecB
				if far == l.A {
					farSec = l.SecA
				}
				if dispSec(farSec) < highsecLine {
					s += w.Staging
				}
			}
			out = append(out, laneCamp{CampSys: camp, FarSys: far, Score: s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].CampSys != out[j].CampSys {
			return out[i].CampSys < out[j].CampSys
		}
		return out[i].FarSys < out[j].FarSys
	})
	return out
}

// ── the ledgers ─────────────────────────────────────────────────────────────

const (
	// laneMapHours is the mapHistory window, a day long.
	laneMapHours = 24
	// laneStatsTTL is how long one read of both ledgers stands, since every camper
	// re-planning in a pass asks the same question.
	laneStatsTTL = 5 * time.Minute
	// laneMinWeight is the floor under a lane worth considering: less decayed
	// traffic than this is noise.
	laneMinWeight = 0.5
)

var (
	laneStatsMu sync.Mutex
	laneStatsAt time.Time
	laneStatsV  []laneStats
)

// loadLaneStats reads both ledgers once and joins them to the map's security and
// region. mapHistory may be missing on a scratch schema; a failed read leaves
// MapJumps zero instead of failing the pick.
func (m *Mod) loadLaneStats(ctx context.Context) ([]laneStats, error) {
	laneStatsMu.Lock()
	if laneStatsV != nil && clock.Now().Sub(laneStatsAt) < laneStatsTTL {
		v := laneStatsV
		laneStatsMu.Unlock()
		return v, nil
	}
	laneStatsMu.Unlock()

	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT fromSystemID, toSystemID, jumps, hauls, iskMoved
		  FROM rolebot_traffic_lanes
		 WHERE jumps + hauls * 3 >= ?`, laneMinWeight)
	if err != nil {
		return nil, err
	}
	var lanes []laneStats
	for rows.Next() {
		var l laneStats
		if err := rows.Scan(&l.A, &l.B, &l.Jumps, &l.Hauls, &l.ISK); err != nil {
			rows.Close()
			return nil, err
		}
		ma, okA := u.meta[l.A]
		mb, okB := u.meta[l.B]
		if !okA || !okB {
			continue
		}
		l.SecA, l.SecB, l.RegA, l.RegB = ma.Security, mb.Security, ma.RegionID, mb.RegionID
		lanes = append(lanes, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	jumps := map[int64]float64{}
	minBucket := clock.Now().Unix()/3600 - laneMapHours
	if r, err := m.db.QueryContext(ctx, `
		SELECT solarSystemID, SUM(count) FROM mapHistory
		 WHERE hourBucket > ? AND statType = 4 GROUP BY solarSystemID`, minBucket); err == nil {
		for r.Next() {
			var sys int64
			var n float64
			if r.Scan(&sys, &n) == nil {
				jumps[sys] = n
			}
		}
		r.Close()
	}
	for i := range lanes {
		lanes[i].MapJumps = jumps[lanes[i].A] + jumps[lanes[i].B]
	}

	// Add lanes from the killboard (gatekills.go) that have prey kills and no ledger
	// row. A gate where only combat hulls die is a battleground, so it is not added.
	kills := m.loadGateKills(ctx)
	seen := make(map[[2]int64]bool, len(lanes))
	for i := range lanes {
		k := [2]int64{lanes[i].A, lanes[i].B}
		seen[k] = true
		if v, ok := kills[k]; ok {
			lanes[i].Prey, lanes[i].Contested = v.Prey, v.Contested
		}
	}
	for k, v := range kills {
		if seen[k] || v.Prey <= 0 {
			continue
		}
		ma, okA := u.meta[k[0]]
		mb, okB := u.meta[k[1]]
		if !okA || !okB {
			continue
		}
		lanes = append(lanes, laneStats{A: k[0], B: k[1], Prey: v.Prey, Contested: v.Contested,
			MapJumps: jumps[k[0]] + jumps[k[1]],
			SecA:     ma.Security, SecB: mb.Security, RegA: ma.RegionID, RegB: mb.RegionID})
	}

	laneStatsMu.Lock()
	laneStatsV, laneStatsAt = lanes, clock.Now()
	laneStatsMu.Unlock()
	return lanes, nil
}

// forgetLaneStats drops the cached read (tests, and a ledger decay sweep).
func forgetLaneStats() {
	laneStatsMu.Lock()
	laneStatsV, laneStatsAt = nil, time.Time{}
	laneStatsMu.Unlock()
	forgetGateKills()
}

// campersBySystem counts the roster's bots sitting a gate, per system. It is the
// crowd term that spreads camps. Only camp phases count: a miner is prey.
func (m *Mod) campersBySystem(ctx context.Context) map[int64]int {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.solarSystemID, COUNT(*)
		  FROM rolebot_state s
		  JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		  JOIN character_ c ON c.characterID = s.characterID
		 WHERE s.phase IN (?, ?, ?)
		 GROUP BY c.solarSystemID`,
		string(TaskPiracyCamp), "gang.pirate.camp", string(TaskCombatGate))
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var sys int64
		var n int
		if rows.Scan(&sys, &n) == nil {
			out[sys] = n
		}
	}
	return out
}

// pickCampLane gathers the picker's inputs: lanes whose camp end is within hops
// of the pilot and passes admit, scored and picked. It returns the camp system,
// the gate facing the far end, the far system, and ok=false when no lane
// qualifies.
func (m *Mod) pickCampLane(ctx context.Context, b *botRun, band laneBand, hops int,
	home map[int64]bool, admit func(sysID int64) bool, recent map[int64]bool) (campSys, gateID, farSys int64, ok bool) {

	lanes, err := m.loadLaneStats(ctx)
	if err != nil || len(lanes) == 0 {
		return 0, 0, 0, false
	}
	u, err := m.universe(ctx)
	if err != nil {
		return 0, 0, 0, false
	}
	// The neighbourhood: every system within hops of here, by the graph.
	near := map[int64]bool{b.SystemID: true}
	frontier := []int64{b.SystemID}
	for h := 0; h < hops && len(frontier) > 0; h++ {
		var next []int64
		for _, s := range frontier {
			for _, nb := range u.adj[s] {
				if !near[nb] {
					near[nb] = true
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	within := func(sysID int64) bool {
		return near[sysID] && (admit == nil || admit(sysID))
	}
	picks := pickLane(lanes, band, defaultLaneWeights, m.campersBySystem(ctx), home, within, recent)
	for _, p := range picks {
		// The gate facing the far end. A lane joined by haul ends may span several
		// jumps with no gate between; it is a route, not a camp, so try the next.
		if g := m.gateBetween(ctx, p.CampSys, p.FarSys); g != 0 {
			return p.CampSys, g, p.FarSys, true
		}
	}
	return 0, 0, 0, false
}

// gateBetween is the stargate in inSys whose jump leads to toSys, or 0 when
// the two are not adjacent. The same join parkAtGate resolves its landing by.
func (m *Mod) gateBetween(ctx context.Context, inSys, toSys int64) int64 {
	if inSys == 0 || toSys == 0 || inSys == toSys {
		return 0
	}
	var gateID int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT d.itemID
		  FROM mapDenormalize d
		  JOIN mapJumps mj ON mj.stargateID = d.itemID
		  JOIN mapDenormalize src ON src.itemID = mj.celestialID
		 WHERE d.solarSystemID = ? AND src.solarSystemID = ?
		 LIMIT 1`, inSys, toSys).Scan(&gateID); err != nil {
		return 0
	}
	return gateID
}

// ── the arrival side of the gate ────────────────────────────────────────────

// gateArrivalGeom is a gate's position and its jump line: the direction a ship
// jumping in through this gate travels (from the paired gate toward this one),
// which is the axis the core scatters arrivals around (destiny/gatearrival.go).
// ok=false when the gate is not in the map or has no pair.
func (m *Mod) gateArrivalGeom(ctx context.Context, gateID int64) (gate, dir destiny.Vec3, radius float64, ok bool) {
	var x, y, z, sx, sy, sz float64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(d.x,0), COALESCE(d.y,0), COALESCE(d.z,0), COALESCE(d.radius,1),
		       COALESCE(src.x,0), COALESCE(src.y,0), COALESCE(src.z,0)
		  FROM mapDenormalize d
		  JOIN mapJumps mj ON mj.stargateID = d.itemID
		  JOIN mapDenormalize src ON src.itemID = mj.celestialID
		 WHERE d.itemID = ?
		 LIMIT 1`, gateID).Scan(&x, &y, &z, &radius, &sx, &sy, &sz); err != nil {
		return destiny.Vec3{}, destiny.Vec3{}, 0, false
	}
	gate = destiny.Vec3{X: x, Y: y, Z: z}
	dir = gate.Sub(destiny.Vec3{X: sx, Y: sy, Z: sz})
	if dir.IsZero() {
		dir = destiny.Vec3{X: 1}
	}
	return gate, dir.Normalize(), radius, true
}

// gateCampPoint is where a camper sits to cover a gate's arrivals: on the jump
// line, GateArrivalRangeM out, so the whole arrival cap is inside a scrambler's
// reach. Pure. The park lane offset keeps a pack off one coordinate.
func gateCampPoint(gate, dir destiny.Vec3, gateID, shipID int64) destiny.Vec3 {
	return gate.Add(dir.Scale(destiny.GateArrivalRangeM)).Add(parkLaneOffset(gateID, shipID, dir))
}

// parkAtGateArrival puts a camper on the arrival side of gateID. The core lands
// a jump-in on the jump line, not sunward, so parkAtCelestial would leave the
// camp out of scrambler range. Falls back to the sunward standoff when the
// gate's pair cannot be resolved.
func (m *Mod) parkAtGateArrival(ctx context.Context, shipID, gateID int64) {
	gate, dir, _, ok := m.gateArrivalGeom(ctx, gateID)
	if !ok {
		m.parkAtCelestial(ctx, shipID, gateID)
		return
	}
	p := gateCampPoint(gate, dir, gateID, shipID)
	m.writePosition(ctx, shipID, p)
}
