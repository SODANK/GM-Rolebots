package rolebots

// Local fortune and the human book: the two things that make one server's trade geography differ
// from another's.
//
// Without them every input to where the roster lives and trades is static map data (schools, NPC
// corporation stations, amenities, security, gates) or a draw rolled per pilot. Per-pilot draws
// average out over a roster, so every server converged on the same expected hubs.
//
// Fortune is a draw per world: one number per system and per station, pure over the economy epoch
// seed (epoch.go), so it is stable for an epoch, differs between servers and is redrawn by the
// economy wipe. It is a nudge on decisions (homes, addresses, market picks, the dock contest), never
// a score read back from the map. A zero seed (failed read) is no fortune at all.
//
// The human book is the live order count real players (not rolebots, not the NPC seeders) have
// resting at each station. A human order says somebody chose to trade there, which is worth more
// than a bot listing: it counts humanOrderWeight bot orders on the scores, and pulls market picks
// toward its system, stacking on a log as more are placed.

import (
	"context"
	"math"
	"sync"
	"time"
)

const (
	// fortuneHomePts is how far luck moves a home or address score. Address softmax temperatures
	// run 1.2 to 5.6 points and a jump of commute costs 0.7 to 4, so this decides among nearby
	// near-equals without overriding an employer's pull (20 to 70).
	fortuneHomePts = 12.0

	// fortuneMarketPrior is added beside liquidityValue in rankMarkets. Liquidity is a log of
	// millions, so the prior seeds where trade starts and fades once real trade is measured.
	fortuneMarketPrior = 0.35

	// fortuneDockPts is how far luck moves a station in its own system's dock contest, against
	// amenities worth 5 to 30 points each.
	fortuneDockPts = 10.0

	// The salts keep a system's draw and a station's draw independent.
	fortuneSaltSystem  = 0x666f7274756e6573 // "fortunes"
	fortuneSaltStation = 0x666f727475646f63 // "fortudoc"

	// humanOrderWeight is what one resting order by a real player counts as, in bot orders, on
	// the hub and station scores.
	humanOrderWeight = 3.0

	// humanMarketPull is what the human book adds to a system in rankMarkets, per e-fold of
	// humanOrderWeight × orders: one order is worth about a million ISK of measured liquidity,
	// ten about six million.
	humanMarketPull = 0.5
)

// fortuneUnit is one world's luck for one id, uniform in [-1, 1). Pure; 0 when seed is 0.
func fortuneUnit(seed, id int64, salt uint64) float64 {
	if seed == 0 {
		return 0
	}
	return splitmixUnit(splitmix64(uint64(seed)^salt) + uint64(id)*noiseGolden)
}

// systemFortune reads the epoch once and answers a system's luck in [-1, 1). Homes and markets
// read the same draw, so pilots settle where trade is seeded and the two reinforce.
func (m *Mod) systemFortune(ctx context.Context) func(sysID int64) float64 {
	seed := m.epochSeed(ctx)
	return func(sysID int64) float64 { return fortuneUnit(seed, sysID, fortuneSaltSystem) }
}

// stationFortune is systemFortune for the dock contest inside one system.
func (m *Mod) stationFortune(ctx context.Context) func(stationID int64) float64 {
	seed := m.epochSeed(ctx)
	return func(stationID int64) float64 { return fortuneUnit(seed, stationID, fortuneSaltStation) }
}

// ── the human book ──────────────────────────────────────────────────────────

// humanOrdersFilter is the WHERE tail that keeps only real players' live orders on market_orders
// aliased o, with rolebot_characters LEFT JOINed as b.
var humanOrdersFilter = `o.orderState = 1 AND o.volRemaining > 0
	   AND o.charID NOT IN (` + liquidityExcluded + `) AND b.characterID IS NULL`

// humanBookMap is the readers' copy: live human order counts by station and by system.
type humanBookMap struct {
	Station map[int64]int64
	System  map[int64]int64
}

var (
	humanBookMu    sync.Mutex
	humanBookCache *humanBookMap
	humanBookAt    time.Time
)

// clearHumanBookCache drops the readers' copy. Called beside clearLiquidityCache.
func clearHumanBookCache() {
	humanBookMu.Lock()
	humanBookCache, humanBookAt = nil, time.Time{}
	humanBookMu.Unlock()
}

// humanBook is the readers' entry point, cached for liquidityTTL. An empty map (read error) means
// no human pull anywhere, which is the old behaviour.
func (m *Mod) humanBook(ctx context.Context) *humanBookMap {
	humanBookMu.Lock()
	if humanBookCache != nil && clock.Now().Sub(humanBookAt) < liquidityTTL {
		out := humanBookCache
		humanBookMu.Unlock()
		return out
	}
	humanBookMu.Unlock()

	out := &humanBookMap{Station: map[int64]int64{}, System: map[int64]int64{}}
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.solarSystemID, COUNT(*)
		  FROM market_orders o
		  LEFT JOIN rolebot_characters b ON b.characterID = o.charID
		 WHERE `+humanOrdersFilter+`
		 GROUP BY o.stationID, o.solarSystemID`)
	if err != nil {
		rlogf("rolebots: human book: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st, sys, n int64
		if err := rows.Scan(&st, &sys, &n); err != nil {
			rlogf("rolebots: human book scan: %v", err)
			return out
		}
		out.Station[st] += n
		out.System[sys] += n
	}
	if err := rows.Err(); err != nil {
		rlogf("rolebots: human book rows: %v", err)
		return out
	}
	humanBookMu.Lock()
	humanBookCache, humanBookAt = out, clock.Now()
	humanBookMu.Unlock()
	return out
}

// humanMarketTerm is what n live human orders add to a system in rankMarkets. Pure.
func humanMarketTerm(n int64) float64 {
	if n <= 0 {
		return 0
	}
	return humanMarketPull * math.Log1p(humanOrderWeight*float64(n))
}

// marketBonus is the bonus rankMarkets adds beside measured liquidity: this world's luck for the
// system plus the pull of the human book there.
func (m *Mod) marketBonus(ctx context.Context) func(sysID int64) float64 {
	luck := m.systemFortune(ctx)
	human := m.humanBook(ctx).System
	return func(sysID int64) float64 {
		return fortuneMarketPrior*luck(sysID) + humanMarketTerm(human[sysID])
	}
}

// humanWeighted folds the human share of a book into a bot-equivalent count: total already
// includes the human orders once, so they are added humanOrderWeight-1 more times. Pure.
func humanWeighted(total, human float64) float64 {
	return total + (humanOrderWeight-1)*human
}
