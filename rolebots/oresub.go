package rolebots

// Buying rock to get metal.
//
// Ore is priced at 0.75 x the intrinsic value of what it refines into
// (sql/ore_prices.sql), so a factory that can reprocess pays a premium for
// metal it need not. The comparison is cost per unit of metal delivered, not
// "does refining pay": a builder must have the material either way. A poor
// yield is not a refusal; it usually loses the comparison. But if nothing in
// reach sells the metal, the alternative to buying rock is not building at
// all. Forced is that case.

import (
	"context"
	"fmt"
	"math"
	"sort"

	evedb "gurimoon/db"
)

const (
	// oreSubHoldFrac is how much of the free hold a substitution may fill. Rock is
	// bulky (210,000 Tritanium is 2,100 m3 as metal, about 5,700 m3 as Veldspar), so
	// the fill is capped; industryShortfall re-reads the bill, so a part load is progress.
	oreSubHoldFrac = 0.9

	// oreSubMinUnits ignores a substitution too small to be worth the trip.
	// A bill short a handful of Tritanium is rounding, not a shopping list.
	oreSubMinUnits = 100
)

// oreSubstitute is one mineral line answered with rock instead.
type oreSubstitute struct {
	MineralID   int64
	MineralName string
	OreTypeID   int64
	OreName     string
	// OreUnits is the rock needed to cover the shortfall at this pilot's yield, not
	// a reference yield: a pilot with poor refining needs more rock for the same metal.
	OreUnits int64
	// OreCost and DirectCost are both ISK per unit of mineral delivered, the only
	// basis on which the two are comparable.
	OreCost    float64
	DirectCost float64
	// MetalPerOre is units of the mineral one unit of this rock refines into at
	// this pilot's yield: what industryBuyMats subtracts from the metal line for
	// the rock it actually bought.
	MetalPerOre float64
	// Forced marks the case where nothing in reach sells the mineral, so the
	// comparison never ran. See the header.
	Forced bool
	Why    string
}

// oreSubChoice is the decision, pure. Returns the index of the winning candidate
// and whether it was forced, or -1 for "buy the metal". Split out so the harness
// can pin the forced arm, which is a policy rather than arithmetic.
func oreSubChoice(oreCosts []float64, directCost float64, edge float64, mineralOnSale bool) (int, bool) {
	best, bestCost := -1, math.Inf(1)
	for i, c := range oreCosts {
		if c <= 0 {
			continue
		}
		if c < bestCost {
			best, bestCost = i, c
		}
	}
	if best < 0 {
		return -1, false
	}
	// Nobody in reach sells the metal, so there is nothing to compare against and the
	// rock wins at any price: the alternative is a batch that does not get built.
	if !mineralOnSale || directCost <= 0 {
		return best, true
	}
	if edge <= 0 {
		edge = oreSubEdgeDefault
	}
	if bestCost < directCost*edge {
		return best, false
	}
	return -1, false
}

// oreSubstitutions works out, for each outstanding mineral line, whether buying
// rock and reprocessing it beats buying the metal. Keyed by mineral typeID; a
// line with no answer is absent and the caller shops for the metal.
func (m *Mod) oreSubstitutions(ctx context.Context, b *botRun, short []matNeed,
	reach map[int64]int) map[int64]oreSubstitute {

	if len(short) == 0 {
		return nil
	}
	regions := m.valuationRegions(ctx, b)
	// Priced on the ask side, unlike other callers: the question here is what the
	// metal would cost instead, not what a refiner can realise. See orerefine.go.
	vals, err := m.oreValues(ctx, regions, oreValueAsk)
	if err != nil {
		rlogf("rolebots: ore substitution values char=%d: %v", b.CharID, err)
		return nil
	}

	// Which of these minerals anybody in reach is selling; the forced arm turns on
	// it, so it is asked once for the whole bill.
	mineralIDs := make([]int64, 0, len(short))
	for _, s := range short {
		if s.Needed > 0 {
			mineralIDs = append(mineralIDs, s.TypeID)
		}
	}
	onSale, err := m.askedForInReach(ctx, mineralIDs, reach)
	if err != nil {
		rlogf("rolebots: ore substitution reach char=%d: %v", b.CharID, err)
		onSale = map[int64]bool{}
	}

	// The hand this pilot would refine with at its own factory. The ore skill is
	// filled in per candidate below, since this function is choosing the rock.
	base := m.refineHandAt(ctx, b, b.State.Data.IndFactoryID, 0)

	// Every ore's reachability, price and skill is resolved once, not inside the
	// per-mineral loop (sixty ores times seven lines of queries; see reach.go).
	oreIDs := make([]int64, 0, len(vals))
	for id := range vals {
		oreIDs = append(oreIDs, id)
	}
	oreAsks, err := m.askedForInReach(ctx, oreIDs, reach)
	if err != nil {
		rlogf("rolebots: ore substitution asks char=%d: %v", b.CharID, err)
		return nil
	}
	orePrice := map[int64]float64{}
	oreNet := map[int64]float64{}
	for id, v := range vals {
		if !oreAsks[id] {
			continue // a catalogue price is not an offer
		}
		p, _, perr := m.estimatePriceRefIn(ctx, regions, id)
		if perr != nil || p <= 0 {
			continue
		}
		orePrice[id] = p
		oreNet[id] = base.WithOreSkill(m.oreSkillLevelFor(ctx, b.CharID, v.GroupID)).Net()
	}

	out := map[int64]oreSubstitute{}
	for _, s := range short {
		if s.Needed < oreSubMinUnits {
			continue
		}
		type cand struct {
			v      oreValue
			cost   float64 // ISK per unit of MINERAL delivered
			units  int64   // rock needed
			perOre float64 // mineral per unit of rock, at this pilot's yield
		}
		var cands []cand
		var costs []float64
		for id, v := range vals {
			per, price := v.PerUnit[s.TypeID], orePrice[id]
			if per <= 0 || price <= 0 {
				continue
			}
			// Metal delivered per unit of rock bought, at this pilot's yield for the family.
			// A poor refiner needs more rock for the same metal, so it usually loses.
			perOre := per * oreNet[id]
			if perOre <= 0 {
				continue
			}
			cands = append(cands, cand{
				v:      v,
				cost:   price / perOre,
				units:  int64(math.Ceil(float64(s.Needed) / perOre)),
				perOre: perOre,
			})
		}
		if len(cands) == 0 {
			continue
		}
		// vals is a map, so sort by cost then typeID: equal-priced ores must not win
		// alternately and send the pilot shopping for a different rock every pass.
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].cost != cands[j].cost {
				return cands[i].cost < cands[j].cost
			}
			return cands[i].v.TypeID < cands[j].v.TypeID
		})
		costs = costs[:0]
		for _, c := range cands {
			costs = append(costs, c.cost)
		}
		direct, _, derr := m.estimatePriceRefIn(ctx, regions, s.TypeID)
		if derr != nil {
			direct = 0
		}
		pick, forced := oreSubChoice(costs, direct, oreSubEdge(), onSale[s.TypeID])
		if pick < 0 {
			continue
		}
		c := cands[pick]

		// The hold cap. A part load is progress (the shortfall is re-read next pass),
		// so this trims rather than refusing.
		if free, ferr := m.holdFreeM3(ctx, b.CharID, b.ShipID); ferr == nil && free > 0 && c.v.Volume > 0 {
			if maxUnits := int64(free * oreSubHoldFrac / c.v.Volume); maxUnits < c.units {
				c.units = maxUnits
			}
		}
		if c.units < 1 {
			continue
		}

		why := fmt.Sprintf("%.2f ISK per unit of metal at its own %.0f%% yield, against %.2f on the counter",
			c.cost, base.Net()*100, direct)
		if forced {
			why = fmt.Sprintf("nothing in reach sells it, so it is buying rock at %.2f per unit of metal — "+
				"the batch does not move without it", c.cost)
		}
		out[s.TypeID] = oreSubstitute{
			MineralID: s.TypeID, MineralName: m.typeNameOr(ctx, s.TypeID),
			OreTypeID: c.v.TypeID, OreName: c.v.Name,
			OreUnits: c.units, OreCost: c.cost, DirectCost: direct,
			MetalPerOre: c.perOre,
			Forced:      forced, Why: why,
		}
	}
	return out
}

// oreSkillLevelFor is this pilot's processing skill level for one ore group.
// 0 when the group has no skill or the pilot has not trained it.
func (m *Mod) oreSkillLevelFor(ctx context.Context, charID, groupID int64) int {
	if groupID == 0 {
		return 0
	}
	id, err := evedb.GetOreProcessingSkill(ctx, m.db, groupID)
	if err != nil || id == 0 {
		return 0
	}
	lv, _ := m.charSkillLevelFor(ctx, charID, id)
	return lv
}
