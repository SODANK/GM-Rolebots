package rolebots

// The refine arithmetic, in one place, for everybody who asks it.
//
// miningRefineWorth (mining.go) is the live decision and reads no prices; the
// per-ore view against the live book (oreEconomics, espore.go) feeds the panel.
// Bots need that second one: a miner pricing an ask, an industrialist choosing
// rock or metal, a trader judging a stack. So the panel and the bots share this
// arithmetic. Refined value is priced off mineral bids by default (what a
// refiner can realise today; the thin ask book would ratchet). Side is a
// parameter because oreSubstitutions (oresub.go) prices the metal it avoids
// buying at an ask.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// orePriceSide picks which side of the mineral book a refined value is worth.
// See the header: bid is the default and the safe one; ask exists for the one
// caller comparing against what it would otherwise have to pay.
type orePriceSide int

const (
	// oreValueBid is what a refiner can realise for what comes out of the plant.
	// The default for every "is refining worth it" question.
	oreValueBid orePriceSide = iota
	// oreValueAsk is what the same minerals would cost to buy instead: the
	// replacement-cost view for a buyer choosing between rock and metal.
	oreValueAsk
	// oreValueDig is what a miner chooses its rock by: the better of the live bid
	// and recent trades, then basePrice (mineralDigPrices).
	// See docs/comment-notes-b14.md#ore-dig-price.
	oreValueDig
)

// oreDigTradeWindowSec is how far back oreValueDig reads trades. Three days:
// the dig price is a feedback loop (miners dig what is dear, the metal floods,
// the price falls), and a fortnight's average answers a flood too late.
const oreDigTradeWindowSec = 3 * 86400

func (s orePriceSide) String() string {
	switch s {
	case oreValueAsk:
		return "ask"
	case oreValueDig:
		return "dig"
	}
	return "bid"
}

// oreHand is a pilot's refining hand at one counter: the station's own
// efficiency, the three skills that move the yield, and the owner corp's tax.
// Net() is the number every decision here is made against; refineHandAt gathers it.
type oreHand struct {
	StationEff  float64
	Refining    int
	RefineryEff int
	// OreSkill is the ore-specific processing skill's level, 0 when the rock is not
	// known yet. Leaving it out only understates a yield; a caller holding an actual
	// stack should pass it.
	OreSkill int
	Tax      float64
}

// Net is the after-tax share of a batch this hand actually keeps.
func (h oreHand) Net() float64 {
	return refineEffYield(h.StationEff, h.Refining, h.RefineryEff, h.OreSkill) * (1 - h.Tax)
}

// WithOreSkill is Net for a different rock than the hand was built for, so a
// caller walking a list of ores does not need a database round trip per row.
func (h oreHand) WithOreSkill(level int) oreHand {
	h.OreSkill = level
	return h
}

// oreValue is one ore against the live book, priced per unit throughout.
// PerUnit is mineral units per ore unit (amountPerBatch over portionSize at
// load time), so no caller has to remember to divide.
type oreValue struct {
	TypeID    int64
	Name      string
	Group     string
	Portion   int64
	GroupID   int64
	Volume    float64 // m³ per ore unit
	Intrinsic float64 // ISK per ore unit at 100% yield
	RefM3     float64 // m³ of what ONE BATCH refines into
	PerUnit   map[int64]float64
}

// RawM3 is one batch as rock, the other half of the compression pair.
func (v oreValue) RawM3() float64 { return v.Volume * float64(v.Portion) }

// oreRefinedValue is what one unit of this rock is worth once it has been
// through the plant at this net yield.
func oreRefinedValue(v oreValue, net float64) float64 { return v.Intrinsic * net }

// oreRefineMargin is the fraction a refiner gains by buying a unit at orePrice
// and reprocessing it. Negative means the rock costs more than it becomes.
// Zero on a zero price, which is a missing quote: otherwise every unpriced ore
// would top a ranking of margins.
func oreRefineMargin(v oreValue, net, orePrice float64) float64 {
	if orePrice <= 0 || v.Intrinsic <= 0 {
		return 0
	}
	return oreRefinedValue(v, net)/orePrice - 1
}

// oreBreakEvenYield is the net yield at which refining a unit is worth exactly
// what orePrice pays for it. Above it the refiner gains. It is a ratio of two
// prices from one book, so a scoped bot and a whole-game panel cannot disagree
// about it, and the Ore page leads with it.
func oreBreakEvenYield(v oreValue, orePrice float64) float64 {
	if v.Intrinsic <= 0 {
		return 0
	}
	return orePrice / v.Intrinsic
}

// refineHandAt reads a pilot's refining hand at one station. A zero stationID
// answers the default counter (refineStationEffDefault, the reprocessing.taxBase
// tax at no standing); a zero oreGroupID leaves OreSkill at 0, which never
// overstates the yield. Every failed read keeps its conservative default rather
// than failing the call.
func (m *Mod) refineHandAt(ctx context.Context, b *botRun, stationID, oreGroupID int64) oreHand {
	h := oreHand{StationEff: refineStationEffDefault, Tax: npcRefineTax(0)}
	if stationID != 0 {
		if sri, err := evedb.GetStationReprocessingInfo(ctx, m.db, stationID); err == nil {
			h.StationEff = sri.Efficiency
			h.Tax = m.refineTaxAt(ctx, b.CharID, stationID, sri)
		}
	}
	h.Refining, _ = m.charSkillLevelFor(ctx, b.CharID, skillIndRefining)
	h.RefineryEff, _ = m.charSkillLevelFor(ctx, b.CharID, skillIndRefineryEf)
	if oreGroupID != 0 {
		if id, err := evedb.GetOreProcessingSkill(ctx, m.db, oreGroupID); err == nil && id != 0 {
			h.OreSkill, _ = m.charSkillLevelFor(ctx, b.CharID, id)
		}
	}
	return h
}

// refineTaxAt is the share of a refine the station keeps from this pilot, the tax
// reprocessingsvc getReprocessingParams charges a player at the same counter. An
// NPC owner charges reprocessing.taxBase less reprocessing.taxPerStanding per point
// of its effective standing toward the pilot (npcRefineTax). A player owner charges
// its own take (Station Details), scaled by its Reprocessing cost modifier for this
// pilot (ownerRefineTax).
func (m *Mod) refineTaxAt(ctx context.Context, charID, stationID int64, sri *evedb.StationReprocessingInfo) float64 {
	if npc, _ := evedb.IsNPCCorp(ctx, m.db, sri.CorporationID); !npc && sri.CorporationID != 0 {
		return ownerRefineTax(sri.StationTake,
			evedb.StationServiceFeeFactor(ctx, m.db, stationID, charID, evedb.StationServiceReprocessing))
	}
	return npcRefineTax(m.npcCorpStanding(ctx, charID, sri.CorporationID))
}

// npcRefineTax is an NPC station's tax at this effective standing, off the dials
// reprocessingsvc reads, with the same defaults and ranges.
func npcRefineTax(standing float64) float64 {
	return npcRefineTaxFrom(
		evedb.CachedSettingFloat("reprocessing.taxBase", 0.05, 0, 1),
		evedb.CachedSettingFloat("reprocessing.taxPerStanding", 0.0075, 0, 1),
		standing)
}

// npcRefineTaxFrom is npcRefineTax with the dials given: base less perStanding per
// point of standing, never under 0. Pure.
func npcRefineTaxFrom(base, perStanding, standing float64) float64 {
	return math.Max(0, base-perStanding*standing)
}

// ownerRefineTax is a player-owned station's tax: its take times the pilot's
// cost factor, held to [0, 1]. Pure.
func ownerRefineTax(take, feeFactor float64) float64 {
	return math.Max(0, math.Min(1, take*feeFactor))
}

// oreValues is every reprocessable ore in the game against the live book.
// Cached across the roster (marketsnap.go): the question has no pilot in it
// beyond the region list. An empty region set prices from the whole game, the
// panel's view and not a bot's, as estimatePriceRefIn does.
func (m *Mod) oreValues(ctx context.Context, regions []int64, side orePriceSide) (map[int64]oreValue, error) {
	key := snapKey("orevalue", regions, int(side))
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshot(key, func() (map[int64]oreValue, error) {
		return m.loadOreValues(sctx, regions, side)
	})
}

func (m *Mod) loadOreValues(ctx context.Context, regions []int64, side orePriceSide) (map[int64]oreValue, error) {
	minPrice, err := m.mineralPrices(ctx, regions, side)
	if err != nil {
		return nil, err
	}
	minVol := make(map[int64]float64, len(minPrice))
	for typeID := range minPrice {
		minVol[typeID] = m.flowVolume(ctx, typeID)
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, g.groupName, g.groupID, t.volume, t.portionSize
		  FROM invTypes t
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE g.categoryID = 25 AND t.portionSize > 0
		   AND EXISTS (SELECT 1 FROM invorereprocessing r WHERE r.oreTypeID = t.typeID)
		 ORDER BY t.typeName`)
	if err != nil {
		return nil, fmt.Errorf("ore catalogue: %w", err)
	}
	out := map[int64]oreValue{}
	for rows.Next() {
		var v oreValue
		if err := rows.Scan(&v.TypeID, &v.Name, &v.Group, &v.GroupID, &v.Volume, &v.Portion); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ore catalogue scan: %w", err)
		}
		v.PerUnit = map[int64]float64{}
		out[v.TypeID] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ore catalogue: %w", err)
	}

	// The yields. One pass over the whole table rather than a subquery per
	// ore: 60 ores times seven minerals is a few hundred rows.
	yr, err := m.db.QueryContext(ctx, `
		SELECT oreTypeID, mineralTypeID, amountPerBatch FROM invorereprocessing`)
	if err != nil {
		return nil, fmt.Errorf("ore yields: %w", err)
	}
	for yr.Next() {
		var oreID, minID, amt int64
		if err := yr.Scan(&oreID, &minID, &amt); err != nil {
			yr.Close()
			return nil, fmt.Errorf("ore yields scan: %w", err)
		}
		v, ok := out[oreID]
		if !ok || amt <= 0 || v.Portion <= 0 {
			continue
		}
		// Intrinsic accumulates per batch and is divided at the end. PerUnit is per ore
		// unit from the start; RefM3 stays per batch, beside RawM3.
		v.Intrinsic += float64(amt) * minPrice[minID]
		v.RefM3 += float64(amt) * minVol[minID]
		v.PerUnit[minID] += float64(amt) / float64(v.Portion)
		out[oreID] = v
	}
	yr.Close()
	if err := yr.Err(); err != nil {
		return nil, fmt.Errorf("ore yields: %w", err)
	}

	for id, v := range out {
		if v.Portion > 0 {
			v.Intrinsic /= float64(v.Portion)
			out[id] = v
		}
	}
	return out, nil
}

// mineralPrices is the live price of each cascade mineral on the requested side
// of the book, scoped to a region set. A mineral nobody quotes falls back to its
// basePrice (sql/ore_prices.sql derived it as the intrinsic value), so a
// starved shard does not price deep ore at zero. The dig side: mineralDigPrices.
func (m *Mod) mineralPrices(ctx context.Context, regions []int64, side orePriceSide) (map[int64]float64, error) {
	if side == oreValueDig {
		return m.mineralDigPrices(ctx, regions)
	}
	agg, bid := "MAX", 1
	if side == oreValueAsk {
		agg, bid = "MIN", 0
	}
	where := ""
	var scope []any
	if len(regions) > 0 {
		where = ` AND regionID IN (` + placeholders(len(regions)) + `)`
		scope = int64Args(regions)
	}
	q := `SELECT COALESCE(` + agg + `(price), 0) FROM market_orders
	       WHERE typeID = ? AND bid = ` + fmt.Sprint(bid) + `
	         AND orderState = 1 AND volRemaining > 0` + where

	out := make(map[int64]float64, len(mineralCascade))
	for _, typeID := range mineralCascade {
		var price float64
		_ = m.db.QueryRowContext(ctx, q, append([]any{typeID}, scope...)...).Scan(&price)
		if price <= 0 {
			var base float64
			if err := m.db.QueryRowContext(ctx,
				`SELECT COALESCE(basePrice, 0) FROM invTypes WHERE typeID = ?`,
				typeID).Scan(&base); err != nil {
				return nil, fmt.Errorf("mineral base price %d: %w", typeID, err)
			}
			price = base
		}
		out[typeID] = price
	}
	return out, nil
}

// digClusterFloorFrac is the least share of the cluster's trade price a mineral
// can read at on the dig side. A region's VWAP includes fills into lowball
// speculative bids, which can drag it far under the metal's real worth.
const digClusterFloorFrac = 0.5

// mineralDigPrices is the dig side of mineralPrices: what a miner thinks the
// metal is worth when it chooses a rock: max(best bid, local trades,
// digClusterFloorFrac x cluster trades), basePrice when nothing is known.
// See docs/comment-notes-b14.md#ore-dig-price.
func (m *Mod) mineralDigPrices(ctx context.Context, regions []int64) (map[int64]float64, error) {
	ids := int64Args(mineralCascade)
	inIDs := `typeID IN (` + placeholders(len(mineralCascade)) + `)`
	inRegions := ""
	if len(regions) > 0 {
		inRegions = ` AND regionID IN (` + placeholders(len(regions)) + `)`
	}
	since := (clock.Unix() - oreDigTradeWindowSec + 11644473600) * 10000000

	grouped := func(q string, args ...any) map[int64]float64 {
		out := map[int64]float64{}
		rows, err := m.db.QueryContext(ctx, capStatement(q), args...)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var p sql.NullFloat64
			if rows.Scan(&id, &p) == nil && p.Float64 > 0 {
				out[id] = p.Float64
			}
		}
		return out
	}

	bids := grouped(`SELECT typeID, MAX(price) FROM market_orders
		 WHERE `+inIDs+` AND bid = 1 AND orderState = 1 AND volRemaining > 0`+inRegions+`
		 GROUP BY typeID`, append(ids, int64Args(regions)...)...)
	vwap := `SELECT typeID, SUM(price*quantity)/SUM(quantity) FROM market_transactions
		 WHERE transactionType = 1 AND transactionDateTime >= ? AND ` + inIDs + ` AND quantity > 0`
	// The cluster half has no region, so it is shared once (not per region key). An
	// empty answer returns an error so a failed read backs off, not cached as no trades.
	cluster, _ := snapshot(snapKey("digcluster"), func() (map[int64]float64, error) {
		c := grouped(vwap+` GROUP BY typeID`, append([]any{since}, ids...)...)
		if len(c) == 0 {
			return nil, errors.New("no cluster mineral trades")
		}
		return c, nil
	})
	local := cluster
	if len(regions) > 0 {
		local = grouped(vwap+inRegions+` GROUP BY typeID`,
			append(append([]any{since}, ids...), int64Args(regions)...)...)
	}

	out := make(map[int64]float64, len(mineralCascade))
	for _, typeID := range mineralCascade {
		price := digPrice(bids[typeID], local[typeID], cluster[typeID])
		if price <= 0 {
			var base float64
			if err := m.db.QueryRowContext(ctx,
				`SELECT COALESCE(basePrice, 0) FROM invTypes WHERE typeID = ?`,
				typeID).Scan(&base); err != nil {
				return nil, fmt.Errorf("mineral base price %d: %w", typeID, err)
			}
			price = base
		}
		out[typeID] = price
	}
	return out, nil
}

// digPrice is mineralDigPrices' rule for one mineral, pure so the harness can
// pin it. Zero means "nothing known" and sends the caller to basePrice.
func digPrice(bid, local, cluster float64) float64 {
	trade := local
	if trade <= 0 {
		trade = cluster
	}
	return math.Max(bid, math.Max(trade, digClusterFloorFrac*cluster))
}

// oreDigValues is what a miner reads to choose its rock: ISK per m3 of each base
// ore on the dig side (oreValues over valuationRegions), bent by oreTaste and
// keyed by the belt table's ore names. No scarcity multiplier is added on top
// (mineralstock.go). Mercoxit is priced only when canMerc is true.
func (m *Mod) oreDigValues(ctx context.Context, b *botRun, canMerc bool) map[string]float64 {
	vals, err := m.oreValues(ctx, m.valuationRegions(ctx, b), oreValueDig)
	if err != nil || len(vals) == 0 {
		return nil
	}
	base := make(map[string]bool, len(defaultOrePriority)+1)
	for _, n := range defaultOrePriority {
		base[n] = true
	}
	if canMerc {
		base[oreMercoxit] = true
	}
	taste := newOreTaste(b.CharID, clock.Unix())
	out := make(map[string]float64, len(base))
	for _, v := range vals {
		if !base[v.Name] || v.Volume <= 0 {
			continue
		}
		out[v.Name] = v.Intrinsic / v.Volume * taste.mul(v.Name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

const (
	// oreTasteAmp is how far one pilot's view of an ore may sit from the market's.
	// Wider than the ~7% gap between Plagioclase, Pyroxeres and Scordite, so
	// near-ties split the roster; short of a doubling, so real price order holds.
	oreTasteAmp = 0.15
	// oreTasteWindowSec is how long one pilot's taste holds before it is re-rolled:
	// long enough to span planning a trip and arriving, short enough that nobody is
	// a Kernite miner for life.
	oreTasteWindowSec = 6 * 3600
)

// oreTaste is one pilot's private bend on the ore values, for one window.
// Without it every miner ranks every rock identically and the roster moves as
// one herd. Stateless (splitmix, like fitTaste): a hash per ore, nothing stored.
type oreTaste struct{ seed uint64 }

func newOreTaste(charID, now int64) oreTaste {
	return oreTaste{seed: splitmix64(uint64(charID) ^ uint64(now/oreTasteWindowSec)*noiseGolden)}
}

// mul is the multiplier on one ore's value, in [1-oreTasteAmp, 1+oreTasteAmp).
func (t oreTaste) mul(name string) float64 {
	// FNV-1a over the name: the belt table keys ores by name, and a typeID
	// would make a grade variant taste different from its own family.
	h := uint64(14695981039346656037)
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	return 1 + oreTasteAmp*splitmixUnit(t.seed+h*noiseGolden)
}

// oreValueGapMax caps the value term in beltCost, in doublings. Six is 64×:
// no ore in the table is further from the best than that, and an unpriced
// one (value 0) has to cost something finite or the walk never prunes.
const oreValueGapMax = 6.0

// oreValueGap is how many doublings of value a system's best rock sits under
// the best rock this pilot could reach, the quantity beltCost prices at
// OreJumpWorth jumps each. Never negative, so beltSearchFloor stays a lower bound.
func oreValueGap(top, value float64) float64 {
	if value <= 0 || top <= 0 {
		return oreValueGapMax
	}
	g := math.Log2(top / value)
	switch {
	case g < 0:
		return 0
	case g > oreValueGapMax:
		return oreValueGapMax
	}
	return g
}

// topOreValue is the best value among the ores that spawn anywhere at or above
// secFloor, the reference oreValueGap measures from. It is measured against
// what the pilot may enter, not the whole table, or the walk would stop pruning.
func topOreValue(values map[string]float64, secFloor float64) float64 {
	top := 0.0
	for _, w := range destiny.OreDistribution(secFloor, 0) {
		if v := values[w.Name]; v > top {
			top = v
		}
	}
	if top > 0 {
		return top
	}
	for _, v := range values {
		if v > top {
			top = v
		}
	}
	return top
}

// oreNamesByValue orders the priced ores dearest first: the fallback order for a
// trip ranked by value when the planned rock is gone. Ties go by name.
func oreNamesByValue(values map[string]float64) []string {
	out := make([]string, 0, len(values))
	for n := range values {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if values[out[i]] != values[out[j]] {
			return values[out[i]] > values[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}
