package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// The mission deck: which enabled, non-storyline templates exist per agent level, read in one
// query and held for missionDeckTTL. The agent pick (missionAgentPick) asks it how likely an agent
// is to offer this pilot work it would take: a template the bots can run, a pocket its ship would
// finish, a destination in its security band. A level, faction or division whose odds fall under
// missionOfferOddsMin is never flown to, and a template enabled or disabled in ESP reaches the
// pick within the TTL. An agent that answers "nothing on offer" drops the cache at once
// (missionFetchStep).

// missionDeckTTL is how long the deck is trusted: the delay between an ESP edit and the bots.
const missionDeckTTL = 2 * time.Minute

// deckCard is one template as the pick weighs it.
type deckCard struct {
	TemplateID int64
	Type       string  // kill, courier, trade, mining
	Factions   []int64 // allowedFactions; empty = any faction
	Runnable   bool    // missionKindOf accepts it
	DungeonVID int64   // the pocket a kill template sends the pilot to
	// The core draws a kill system, and a courier's "nearby" pickup or drop-off, from the
	// systems within NearbyJumpBands-1 jumps of the agent inside this security range.
	MinSec, MaxSec              sql.NullFloat64
	PickupNearby, DropoffNearby bool
}

// missionDeckState is one read of the deck.
type missionDeckState struct {
	levels  map[int][]deckCard
	weights map[int64]evedb.DivisionWeights // agtDivisionMissionWeights by divisionID
	at      time.Time
}

var (
	missionDeckMu  sync.Mutex
	missionDeckNow *missionDeckState
)

// invalidateMissionDeck makes the next pick read the deck afresh.
func invalidateMissionDeck() {
	missionDeckMu.Lock()
	missionDeckNow = nil
	missionDeckMu.Unlock()
}

// missionDeck returns the current deck, reading it when it is older than missionDeckTTL.
func (m *Mod) missionDeck(ctx context.Context) (*missionDeckState, error) {
	missionDeckMu.Lock()
	defer missionDeckMu.Unlock()
	if d := missionDeckNow; d != nil && clock.Now().Sub(d.at) < missionDeckTTL {
		return d, nil
	}
	d := &missionDeckState{levels: map[int][]deckCard{}, weights: map[int64]evedb.DivisionWeights{}, at: clock.Now()}
	rows, err := m.db.QueryContext(ctx, `
		SELECT templateID, agentLevel, missionType, COALESCE(allowedFactions, ''),
		       COALESCE(completionType, 'killAll'), COALESCE(dungeonVID, 0), COALESCE(dungeonRoomIdx, 1),
		       COALESCE(killItemTypeID, 0), COALESCE(killItemQty, 1), COALESCE(killItemDropoffMode, 'agent'),
		       COALESCE(courierTypeID, 0), COALESCE(courierQty, 1),
		       COALESCE(tradeTypeID, 0), COALESCE(tradeQty, 1),
		       COALESCE(miningTypeID, 0), COALESCE(miningQty, 1),
		       COALESCE(pickupMode, 'agent'), COALESCE(dropoffMode, 'nearby'),
		       minSecurity, maxSecurity
		  FROM missionTemplates
		 WHERE enabled = 1 AND COALESCE(storyline, 0) = 0`)
	if err != nil {
		return nil, fmt.Errorf("mission deck: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t evedb.MissionTemplate
		var factions string
		if err := rows.Scan(&t.TemplateID, &t.AgentLevel, &t.MissionType, &factions,
			&t.CompletionType, &t.DungeonVID, &t.DungeonRoomIdx,
			&t.KillItemTypeID, &t.KillItemQty, &t.KillItemDropoffMode,
			&t.CourierTypeID, &t.CourierQty, &t.TradeTypeID, &t.TradeQty,
			&t.MiningTypeID, &t.MiningQty, &t.PickupMode, &t.DropoffMode,
			&t.MinSecurity, &t.MaxSecurity); err != nil {
			return nil, fmt.Errorf("mission deck scan: %w", err)
		}
		kind, _ := missionKindOf(&t)
		d.levels[t.AgentLevel] = append(d.levels[t.AgentLevel], deckCard{
			TemplateID: t.TemplateID, Type: t.MissionType,
			Factions: parseFactionCSV(factions), Runnable: kind != "",
			DungeonVID: t.DungeonVID, MinSec: t.MinSecurity, MaxSec: t.MaxSecurity,
			PickupNearby:  t.MissionType == "courier" && t.PickupMode == "nearby",
			DropoffNearby: t.MissionType == "courier" && t.DropoffMode == "nearby",
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	wrows, err := m.db.QueryContext(ctx, `
		SELECT divisionID, killWeight, courierWeight, tradeWeight, miningWeight
		  FROM agtDivisionMissionWeights`)
	if err != nil {
		return nil, fmt.Errorf("mission deck weights: %w", err)
	}
	defer wrows.Close()
	for wrows.Next() {
		var w evedb.DivisionWeights
		if err := wrows.Scan(&w.DivisionID, &w.KillWeight, &w.CourierWeight, &w.TradeWeight, &w.MiningWeight); err != nil {
			return nil, fmt.Errorf("mission deck weights scan: %w", err)
		}
		d.weights[w.DivisionID] = w
	}
	if err := wrows.Err(); err != nil {
		return nil, err
	}
	missionDeckNow = d
	return d, nil
}

// weightsFor is a division's mission mix, with the core's default for a division with no row
// (GetDivisionWeights: kill and courier, half each).
func (d *missionDeckState) weightsFor(divisionID int64) evedb.DivisionWeights {
	if w, ok := d.weights[divisionID]; ok {
		return w
	}
	return evedb.DivisionWeights{DivisionID: divisionID, KillWeight: 50, CourierWeight: 50}
}

// odds is the chance an agent of this level, faction and division offers this pilot work it
// takes, cardOdds being the chance the pilot takes one template's offer (0 for a template it
// cannot run). An unread deck answers 1: the agent's own answer decides.
func (d *missionDeckState) odds(level int, factionID, divisionID int64, cardOdds func(deckCard) float64) float64 {
	if d == nil {
		return 1
	}
	return deckOfferOdds(d.levels[level], factionID, d.weightsFor(divisionID), cardOdds)
}

// deckOffersRunnable reports whether some offer the core can draw is a template the bots can run.
func deckOffersRunnable(cards []deckCard, factionID int64, w evedb.DivisionWeights) bool {
	return deckOfferOdds(cards, factionID, w, func(deckCard) float64 { return 1 }) > 0
}

// deckOfferOdds mirrors the core's draw (generateMissionOffer): roll a type by the division's
// weights (all zero rolls kill), take one template of that type allowed for the agent's faction
// at random, and fall back to kill when the type has none. It answers the chance the drawn
// template is one the pilot takes: each rolled type's share times the mean of cardOdds over its
// templates, an unrunnable template counting 0.
func deckOfferOdds(cards []deckCard, factionID int64, w evedb.DivisionWeights, cardOdds func(deckCard) float64) float64 {
	byType := map[string][]deckCard{}
	for _, c := range cards {
		if deckAllows(c.Factions, factionID) {
			byType[c.Type] = append(byType[c.Type], c)
		}
	}
	mean := func(typ string) float64 {
		list := byType[typ]
		if len(list) == 0 {
			if typ == "kill" {
				return 0
			}
			list = byType["kill"] // an empty rolled type falls back to kill
			if len(list) == 0 {
				return 0
			}
		}
		sum := 0.0
		for _, c := range list {
			if c.Runnable {
				sum += cardOdds(c)
			}
		}
		return sum / float64(len(list))
	}
	weights := map[string]int{"kill": w.KillWeight, "courier": w.CourierWeight,
		"trade": w.TradeWeight, "mining": w.MiningWeight}
	total := 0
	for _, v := range weights {
		if v > 0 {
			total += v
		}
	}
	if total <= 0 {
		return mean("kill")
	}
	odds := 0.0
	for typ, wt := range weights {
		if wt > 0 {
			odds += float64(wt) / float64(total) * mean(typ)
		}
	}
	return odds
}

// destInBandShare is the chance a system the core draws near the agent (PickNearbySystem: a jump
// band rolled by missions.jumpWeights, then a system in it at random, both inside the template's
// security range) lies inside [secMin, secMax]. With no system in range the core uses the agent's
// own system, which the pick already holds in the band, so the answer is 1. A courier's station
// walk skips systems without a station; the share counts every system, which is close enough.
func destInBandShare(bands [][]int64, meta map[int64]sysMeta, tplMin, tplMax sql.NullFloat64, secMin, secMax float64) float64 {
	weights := coreJumpWeights()
	total, share := 0.0, 0.0
	for depth, band := range bands {
		if depth >= len(weights) {
			break
		}
		n, in := 0, 0
		for _, sys := range band {
			ms, ok := meta[sys]
			if !ok {
				continue
			}
			if (tplMin.Valid && ms.Security < tplMin.Float64) || (tplMax.Valid && ms.Security > tplMax.Float64) {
				continue
			}
			n++
			if ms.Security >= secMin && ms.Security <= secMax {
				in++
			}
		}
		if n == 0 || weights[depth] <= 0 {
			continue
		}
		total += float64(weights[depth])
		share += float64(weights[depth]) * float64(in) / float64(n)
	}
	if total == 0 {
		return 1
	}
	return share / total
}

// coreJumpWeights is the core's jump-band weighting (db missionJumpWeights): the
// missions.jumpWeights setting, else the defaults, all-zero falling back to the defaults.
func coreJumpWeights() []int {
	def := make([]float64, evedb.NearbyJumpBands)
	for i, w := range evedb.MissionJumpWeights {
		def[i] = float64(w)
	}
	out := make([]int, evedb.NearbyJumpBands)
	sum := 0
	for i, f := range evedb.CachedSettingFloats("missions.jumpWeights", def, 0, 1e6) {
		if i < len(out) {
			out[i] = int(f)
			sum += out[i]
		}
	}
	if sum == 0 {
		for i, w := range evedb.MissionJumpWeights {
			out[i] = w
		}
	}
	return out
}

// jumpBands is the systems within NearbyJumpBands-1 jumps of a system, by distance, the way
// GetNearbySystemsByJumps buckets them before its security filter.
func jumpBands(u *universe, from int64) [][]int64 {
	bands := make([][]int64, evedb.NearbyJumpBands)
	for sys, d := range hopsWithin(u, from, evedb.NearbyJumpBands-1) {
		bands[d] = append(bands[d], sys)
	}
	return bands
}

// deckAllows is the core's allowedFactions filter: an empty list or a factionless agent passes.
func deckAllows(factions []int64, factionID int64) bool {
	if len(factions) == 0 || factionID <= 0 {
		return true
	}
	for _, f := range factions {
		if f == factionID {
			return true
		}
	}
	return false
}

// parseFactionCSV reads missionTemplates.allowedFactions.
func parseFactionCSV(s string) []int64 {
	var out []int64
	for _, part := range strings.Split(s, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}
