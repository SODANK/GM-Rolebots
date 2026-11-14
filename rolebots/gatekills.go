package rolebots

import (
	"context"
	"math"
	"sync"
	"time"
)

// Violence at the gates: the killboard as a lane ledger.
//
// Each killmail records the celestial nearest the wreck (db/killlocation.go); at a
// gate that is groupID 10, and mapJumps resolves it to the far system, a lane.
// Two readings stay apart because they mean opposite things to a camp:
//
//   - an industrial victim (industrial, barge, freighter) is prey proven to pass;
//   - a combat victim is a fight: somebody already works this gate.
//
// The window is a week, the life of synthetic kill detail (killboard.botRetentionDays).

// gateViolence is one lane's kills over the window, by what died.
type gateViolence struct {
	Prey, Contested float64
}

// gateKillRow is one grouped row of the killboard read: kills at the gate in
// Sys that leads to Far, of hulls in ShipGroupID.
type gateKillRow struct {
	Sys, Far, ShipGroupID int64
	IsPod, NPCOnly        bool
	N                     float64
}

// hullIsPrey is the camp's and the hauler's shared reading of a hull: the
// classes that carry cargo and cannot fight for it.
func hullIsPrey(c HullClass) bool {
	return c == ClassIndustrial || c == ClassBarge || c == ClassFreighter
}

// foldGateKills turns the grouped killboard rows into per-lane violence, keyed by
// laneKey (undirected). Pods are skipped because a pod follows its ship onto the
// board. A kill with only NPCs on it still proves an industrial passed but is no
// fight: it counts as prey, never contested.
func foldGateKills(rows []gateKillRow) map[[2]int64]gateViolence {
	out := map[[2]int64]gateViolence{}
	for _, r := range rows {
		if r.IsPod || r.Sys == 0 || r.Far == 0 || r.Sys == r.Far || r.N <= 0 {
			continue
		}
		a, b := laneKey(r.Sys, r.Far)
		v := out[[2]int64{a, b}]
		if hullIsPrey(hullClassByGroup[r.ShipGroupID]) {
			v.Prey += r.N
		} else if !r.NPCOnly {
			v.Contested += r.N
		}
		out[[2]int64{a, b}] = v
	}
	return out
}

// laneDanger is what a hauler reads off a lane's violence: every industrial loss
// in full and half of every fight, since a gate where ships fight is one where a
// hauler is shot too.
func laneDanger(v gateViolence) float64 {
	return math.Max(v.Prey, 0) + 0.5*math.Max(v.Contested, 0)
}

const (
	// gateKillDays is the killboard window: a week, the life of synthetic kill
	// detail (killboard.botRetentionDays).
	gateKillDays = 7
	// gateKillTTL is how long one read stands, the same cadence as the lane stats it
	// feeds.
	gateKillTTL = 5 * time.Minute
)

var (
	gateKillsMu sync.Mutex
	gateKillsAt time.Time
	gateKillsV  map[[2]int64]gateViolence
)

// loadGateKills reads the week's gate kills once and folds them per lane. Errors
// read as an empty board: a lane pick or a route must not fail over a missing
// killboard table.
func (m *Mod) loadGateKills(ctx context.Context) map[[2]int64]gateViolence {
	gateKillsMu.Lock()
	if gateKillsV != nil && clock.Now().Sub(gateKillsAt) < gateKillTTL {
		v := gateKillsV
		gateKillsMu.Unlock()
		return v
	}
	gateKillsMu.Unlock()

	folded := map[[2]int64]gateViolence{}
	if m.db != nil {
		since := clock.Unix() - gateKillDays*86400
		// idx_km_time bounds the scan; the gate to far-system join is the one gateBetween
		// makes, run once for the whole board.
		rows, err := m.db.QueryContext(ctx, `
			SELECT k.solarSystemID, src.solarSystemID, k.shipGroupID, k.isPod, k.npcOnly, COUNT(*)
			  FROM killmails k
			  JOIN mapJumps mj ON mj.stargateID = k.locationID
			  JOIN mapDenormalize src ON src.itemID = mj.celestialID
			 WHERE k.killTime >= ? AND k.locationGroupID = 10
			 GROUP BY k.solarSystemID, src.solarSystemID, k.shipGroupID, k.isPod, k.npcOnly`, since)
		if err == nil {
			var in []gateKillRow
			for rows.Next() {
				var r gateKillRow
				var pod, npc int
				if rows.Scan(&r.Sys, &r.Far, &r.ShipGroupID, &pod, &npc, &r.N) == nil {
					r.IsPod, r.NPCOnly = pod != 0, npc != 0
					in = append(in, r)
				}
			}
			rows.Close()
			folded = foldGateKills(in)
		}
	}

	gateKillsMu.Lock()
	gateKillsV, gateKillsAt = folded, clock.Now()
	gateKillsMu.Unlock()
	return folded
}

// gateDangerBetween is the hauler's read of lane a to b from the last board read.
// The router's hop cost has no context to load one, so route() primes the cache
// first (primeGateDanger). Zero for an unread or quiet lane.
func gateDangerBetween(a, b int64) float64 {
	ka, kb := laneKey(a, b)
	gateKillsMu.Lock()
	v, ok := gateKillsV[[2]int64{ka, kb}]
	gateKillsMu.Unlock()
	if !ok {
		return 0
	}
	return laneDanger(v)
}

// primeGateDanger refreshes the board before a search that will price it.
// A search whose rules do not read danger skips the read.
func (m *Mod) primeGateDanger(ctx context.Context, r travelRules) {
	if r.GateDanger > 0 {
		m.loadGateKills(ctx)
	}
}

// setGateKillsForTest installs a board read as if just taken.
func setGateKillsForTest(v map[[2]int64]gateViolence) {
	gateKillsMu.Lock()
	gateKillsV, gateKillsAt = v, clock.Now()
	gateKillsMu.Unlock()
}

// forgetGateKills drops the cached read (tests, and alongside the lane stats).
func forgetGateKills() {
	gateKillsMu.Lock()
	gateKillsV, gateKillsAt = nil, time.Time{}
	gateKillsMu.Unlock()
}

// haulGateDangerJumps is the hauler's price, in jump-equivalents per log unit of
// lane violence, to cross a gate with a record. A price, never a ban. Zero for
// hulls that can fight.
const haulGateDangerJumps = 2.0

// travelRulesFor is rulesForBot with the hauler's danger term: a pilot in a
// hull that is prey (hullIsPrey) prices gate violence into its route; every
// other hull flies the plain rules, because a hunter must not be routed
// away from a fight.
func (m *Mod) travelRulesFor(ctx context.Context, b *botRun) travelRules {
	r := rulesForBot(b)
	if b.ShipID != 0 && hullIsPrey(hullClassByGroup[m.typeGroupOf(ctx, m.activeShipType(ctx, b.ShipID))]) {
		r.GateDanger = haulGateDangerJumps
	}
	return r
}
