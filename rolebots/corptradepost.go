package rolebots

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// Trading posts are offices a trade corporation keeps where trade is.
// The Trade desk (corptrade.go) lists leftover goods only from an office, so a trade-tagged
// CEO keeps tradePostWant offices in liquid systems within reach of its headquarters,
// renting one at a time at the station with the most live orders.
// The liquidity cutoff is relative (tradePostCutoff), so a fresh map can still open posts.
// This runs beside the recruiting branch (corpbranch.go): a branch goes where pilots live,
// a post goes where the market is. Rent goes through ceoTryRentOffice.

const (
	// tradePostWant is how many market offices a trade corporation keeps.
	tradePostWant = 2
	// tradePostMinISK is the ceiling on the post cutoff, in decayed liquidity (ISK changing
	// hands between non-seeders, liquidity.go).
	tradePostMinISK = 500e6
	// tradePostTopN is how many of the busiest systems count as liquid while the map is
	// below the ceiling.
	tradePostTopN = 55
	// tradePostFloorISK is the floor under the cutoff, so a few stray trades are not a market.
	tradePostFloorISK = 25e6
	// tradePostKeepFrac: a post already held counts at this share of the cutoff, so a moving
	// cutoff does not trigger a replacement rental.
	tradePostKeepFrac = 0.5
	// tradePostMaxJumps is how far from the headquarters a post may be.
	tradePostMaxJumps = 15
	// tradePostEverySec is how often a corporation may open a post.
	tradePostEverySec = 2 * 24 * 3600
	// tradePostRetrySec is how long a hunt that found nowhere to rent waits before retrying.
	tradePostRetrySec = 6 * 3600
	// tradePostSystems is how many of the best systems are tried, and tradePostStations how
	// many stations in each.
	tradePostSystems  = 10
	tradePostStations = 3
)

// tradePostTryAt holds the time of the last post hunt per corporation that found nothing.
// It is in memory only.
var tradePostTryAt sync.Map // corpID to unix seconds

func init() { registerCorpForget("tradePostTryAt", func(id int64) { tradePostTryAt.Delete(id) }) }

// tradePostSite is one liquid system a post could open in.
type tradePostSite struct {
	SystemID  int64
	Liquidity float64 // decayed ISK traded there, seeders excluded
	Security  float64
	FromHQ    int // jumps from the headquarters
	HasOffice bool
}

// tradePostCutoff returns the liquidity a system needs to count as a trading post: the
// tradePostTopN-th busiest system's, capped at tradePostMinISK and floored at
// tradePostFloorISK. Pure.
func tradePostCutoff(system map[int64]float64) float64 {
	vals := make([]float64, 0, len(system))
	for _, v := range system {
		if v >= tradePostFloorISK {
			vals = append(vals, v)
		}
	}
	if len(vals) < tradePostTopN {
		return tradePostFloorISK
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(vals)))
	return max(min(vals[tradePostTopN-1], tradePostMinISK), tradePostFloorISK)
}

// tradePostsHeld counts the corporation's offices that are trading posts: one per liquid
// system it has an office in, at tradePostKeepFrac of the cutoff. Pure.
func tradePostsHeld(officeSystems map[int64]bool, liq map[int64]float64, cutoff float64) int {
	n := 0
	for sys := range officeSystems {
		if liq[sys] >= cutoff*tradePostKeepFrac {
			n++
		}
	}
	return n
}

// rankTradePostSites keeps the systems a post could open in: at the cutoff, in reach, at
// or above the CEO's security floor, with no office yet. Busiest first, then nearest. Pure.
func rankTradePostSites(sites []tradePostSite, secFloor, cutoff float64) []tradePostSite {
	var out []tradePostSite
	for _, s := range sites {
		if s.HasOffice || s.Liquidity < cutoff || s.Security < secFloor ||
			s.FromHQ < 0 || s.FromHQ > tradePostMaxJumps {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Liquidity != out[j].Liquidity {
			return out[i].Liquidity > out[j].Liquidity
		}
		if out[i].FromHQ != out[j].FromHQ {
			return out[i].FromHQ < out[j].FromHQ
		}
		return out[i].SystemID < out[j].SystemID
	})
	return out
}

// ceoTradingPost is the estate power's trading-post step. It reports whether it rented one.
func (m *Mod) ceoTradingPost(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, hq int64) (bool, error) {
	if facts == nil || hq == 0 {
		return false, nil
	}
	// A trade corporation keeps tradePostWant posts. Any other corporation whose sales arm
	// has members keeps one (the sales arm is posted there, corpposting.go).
	want := tradePostWant
	if !tradeDeskTagged(facts.Policy.Tags) {
		if armCount(c.CorpID, armSales) < purposeArmMin {
			return false, nil
		}
		want = 1
	}
	// A trade corporation also keeps a shelf where the factories are, for the desk's supply
	// runs (corpsupplyrun.go), with its own count and cadence.
	if tradeDeskTagged(facts.Policy.Tags) {
		if rented, err := m.ceoFactoryPost(ctx, c, facts, ch, hq); err != nil || rented {
			return rented, err
		}
	}
	now := clock.Unix()
	if now-m.lastCEOActionAt(ctx, c.CorpID, "tradepost") < tradePostEverySec {
		return false, nil
	}
	if v, ok := tradePostTryAt.Load(c.CorpID); ok && now-v.(int64) < tradePostRetrySec {
		return false, nil
	}
	anchors, err := m.corpAnchorStations(ctx, c.CorpID)
	if err != nil {
		return false, err
	}
	officeSys := map[int64]bool{}
	for stationID, a := range anchors {
		if m.officeAt(ctx, c.CorpID, stationID) != 0 {
			officeSys[a.SystemID] = true
		}
	}
	lm := m.liquidity(ctx)
	liq := lm.System
	if tradePostsHeld(officeSys, liq, lm.PostCutoff) >= want {
		return false, nil
	}
	fromHQ, err := m.corpJumpMap(ctx, m.stationSystemID(ctx, hq))
	if err != nil {
		return false, err
	}
	u, err := m.universe(ctx)
	if err != nil {
		return false, err
	}
	var sites []tradePostSite
	for sys, isk := range liq {
		d, ok := fromHQ[sys]
		if !ok {
			continue
		}
		sites = append(sites, tradePostSite{SystemID: sys, Liquidity: isk, Security: u.meta[sys].Security,
			FromHQ: d, HasOffice: officeSys[sys]})
	}
	ranked := rankTradePostSites(sites, ch.HQSecFloor, lm.PostCutoff)
	if len(ranked) > tradePostSystems {
		ranked = ranked[:tradePostSystems]
	}
	for _, s := range ranked {
		stations, err := m.tradePostStations(ctx, s.SystemID)
		if err != nil {
			return false, err
		}
		for _, at := range stations {
			at.Security = s.Security
			why := fmt.Sprintf("a trading post — %s traded in the system lately, %d live order(s) on the station's book, %d jump(s) from the headquarters",
				iskShort(s.Liquidity), at.Members, s.FromHQ)
			at.Members = 0
			rented, err := m.ceoTryRentOffice(ctx, c, facts, ch, at, false, false, why)
			if errors.Is(err, evedb.ErrNoFreeOffices) {
				continue // full: the next station
			}
			if err != nil {
				return false, err
			}
			if !rented {
				// Short of the rent and its reserve (ceoTryRentOffice logs it once a day), or the
				// office appeared meanwhile.
				tradePostTryAt.Store(c.CorpID, now)
				return false, nil
			}
			tradePostTryAt.Delete(c.CorpID)
			m.ceoLog(ctx, c, powerEstate, "tradepost", at.StationID, 0,
				"opened a trading post at %s so the trade desk has a shelf where the market is", at.Name)
			return true, nil
		}
	}
	tradePostTryAt.Store(c.CorpID, now)
	return false, nil
}

// tradePostStations returns the busiest stations in a system by live orders, best first.
// The order count rides in Members for the log line.
func (m *Mod) tradePostStations(ctx context.Context, systemID int64) ([]estateCandidate, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.stationName, s.regionID, COUNT(o.orderID) AS live
		  FROM staStations s
		  LEFT JOIN market_orders o ON o.stationID = s.stationID AND o.orderState = 1
		 WHERE s.solarSystemID = ?
		 GROUP BY s.stationID, s.stationName, s.regionID
		 ORDER BY live DESC, s.stationID
		 LIMIT ?`, systemID, tradePostStations)
	if err != nil {
		return nil, fmt.Errorf("tradePostStations: %w", err)
	}
	defer rows.Close()
	var out []estateCandidate
	for rows.Next() {
		var at estateCandidate
		if err := rows.Scan(&at.StationID, &at.Name, &at.RegionID, &at.Members); err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	return out, rows.Err()
}
