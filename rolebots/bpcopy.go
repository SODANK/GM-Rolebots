package rolebots

// Blueprint copying: a researcher's product. The core supports it end to end (ramsvc installs it,
// CompleteJob turns one job into N singleton copies carrying the original's ME/PE; ramActivities 5).
//
//	researcher  keeps one original per type in its library (a station with lab and copy lines,
//	            industryFactoryStation), researches it to full, then copies it in bulk and sells the copies on
//	            escrow. A full-run copy costs a fraction of the print (bpcPriceFrac) plus a share of the research saving.
//	builder     buys a copy for the runs it needs (bestEscrowBPO compares per run); a pilot that builds a type
//	            constantly buys the original at the big price a fully researched spare commands (fullBPOPrice).
//
// Runs per copy are policy, not the blueprint maximum (copyRunsFor).

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/rates"
	serverram "gurimoon/server/ram"
)

const (
	// bpcPriceFracDefault is what a full-run copy costs as a fraction of the original's reference price,
	// before the research premium. At 0.30 the print is recovered in about four copies.
	bpcPriceFracDefault = 0.30
	// bpcResearchShare is the part of a copy's per-run research saving the
	// researcher charges for; the builder keeps the rest.
	bpcResearchShare = 0.5
	// bpcCostFloorMul: a copy never sells for less than this many times its own share of the copy job's
	// fee, so a researcher does not pay to sell.
	bpcCostFloorMul = 2.0
	// bpcBuyerCeilFrac is the most a builder pays for a full-run copy as a
	// fraction of the original's reference price (plus research). Above the
	// seller's bpcPriceFrac so a nudged listing still clears.
	bpcBuyerCeilFrac = 0.50

	// bpoFullMEDefault and bpoFullPEDefault are the research target, not a cap (research is uncapped): at
	// waste factor W a print wastes W/(1+ME), so ME 10 has taken 91% of what research can save.
	// Settings rolebots.bpoFullME/PE.
	bpoFullMEDefault = 10
	bpoFullPEDefault = 10
	// bpoFullMarkupT1/T2Default: a fully researched spare original sells for big money, this many times its
	// reference price plus the research.
	bpoFullMarkupT1Default = 3.0
	bpoFullMarkupT2Default = 6.0

	// copyBatchMin / copyBatchMax bound the copies one job makes. Bulk is the
	// point: one listing fee and one trip buy a stack of stock.
	copyBatchMin = 5
	copyBatchMax = 30
	// copyStockTarget is how many copies of one type a researcher keeps for
	// sale (listed plus in the hangar plus cooking); copyStockTargetSelling
	// when copies of that type have sold anywhere within copyDemandWindowSec.
	copyStockTarget        = 10
	copyStockTargetSelling = 20
	copyDemandWindowSec    = 7 * 86400
	// copyListingCap bounds one researcher's open copy listings: escrow is a
	// shared board and every listing is a fee.
	copyListingCap = 60

	// printRefDerivedFrac and printRefDerivedRuns price a print with no market ask and no catalogue price
	// (265 Tech II and over a thousand Tech I prints have basePrice 0) from what it makes: a quarter of one
	// printRefDerivedRuns-run batch of its product.
	printRefDerivedFrac = 0.25
	printRefDerivedRuns = 300
)

// Charge groups whose copies carry a fixed run count rather than the blueprint maximum (the maximums in
// sql/invblueprinttypes.sql match, so the clamp below does not bite).
var (
	copyRunsByChargeGroup = map[int64]int64{
		83:  10000, // projectile ammunition
		85:  5000,  // hybrid charges
		86:  100,   // frequency crystals
		482: 100,   // mining crystals
		663: 100,   // mercoxit mining crystals
		// missiles, rockets and torpedoes, Tech I
		88: 5000, 89: 5000, 384: 5000, 385: 5000, 386: 5000, 387: 5000,
		394: 5000, 395: 5000, 396: 5000, 476: 5000,
	}
	// copyRunsByShipGroup is the hull ladder: small hulls copy at ten runs,
	// cruisers at five, battlecruisers at two, battleships and anything bigger
	// at one.
	copyRunsByShipGroup = map[int64]int64{
		25: 10, 31: 10, 237: 10, 29: 10, 324: 10, 420: 10, 541: 10, // frigate .. destroyer
		26: 5, 28: 5, 358: 5, 380: 5, // cruiser, industrial
		419: 2, 540: 2, // battlecruiser
		27: 1, 381: 1, 463: 1, 543: 1, // battleship, mining barge
		30: 1, 485: 1, 513: 1, 547: 1, 659: 1, // titan, dread, freighter, carrier, mothership
	}
)

// copyDroneRuns caps a drone copy: a hundred drones is a lot of drones.
const copyDroneRuns = 100

// copyRunsFor is how many runs each copy of a print carries: the policy above,
// never more than the blueprint allows and never less than one. Pure.
func copyRunsFor(categoryID, groupID, maxLimit int64) int64 {
	want := maxLimit
	switch categoryID {
	case catCharge:
		if r, ok := copyRunsByChargeGroup[groupID]; ok {
			want = r
		}
	case catDrone:
		want = copyDroneRuns
	case catShip:
		if r, ok := copyRunsByShipGroup[groupID]; ok {
			want = r
		} else {
			want = 1 // an unknown hull class is treated as a big one
		}
	case 2: // construction platforms (outposts) and other celestial structures
		want = 1
	case 22: // deployables
		want = 5
	}
	if maxLimit > 0 && want > maxLimit {
		want = maxLimit
	}
	if want < 1 {
		want = 1
	}
	return want
}

// copyDurationSec is the duration of one copy job on this line: the server's CalcCopyTime and the line's
// multiplier, on the research rate chain (IndustrySpeed then ResearchSpeed, never ProductionSpeed).
func copyDurationSec(bpType *evedb.BlueprintTypeInfo, copies int, runsPerCopy int64, science int, line *manufactureLine) float64 {
	if bpType == nil || copies < 1 || bpType.ResearchCopyTime <= 0 {
		return 0
	}
	d := serverram.CalcCopyTime(bpType.ResearchCopyTime, copies, runsPerCopy, bpType.MaxProductionLimit, science)
	d *= line.TimeMul
	d /= rates.IndustrySpeed
	d /= rates.ResearchSpeed
	return d
}

// copyBatch sizes one copy job: the gap to the stock target, at least copyBatchMin and at most
// copyBatchMax, with no time trim. It returns 0 when the stock is already full. Pure.
func copyBatch(target, stock int) int {
	n := target - stock
	if n <= 0 {
		return 0
	}
	return min(max(n, copyBatchMin), copyBatchMax)
}

// printFullyResearched reports whether a print has reached the module's research target
// (rolebots.bpoFullME/PE), where a pilot stops spending on research and starts copying. A spending
// rule, not the game's, so the target is never written on a listing. Pure.
func printFullyResearched(me, pe, fullME, fullPE int64) bool {
	return me >= fullME && pe >= fullPE
}

// bpcPrice is a copy's asking price: frac of the original's reference price scaled by the runs it
// carries, plus bpcResearchShare of the research saving on those runs, and never under bpcCostFloorMul
// times its share of the copy job's fee. Pure.
func bpcPrice(ref float64, runs, maxRuns int64, savingPerRun, copyCost, frac float64) float64 {
	share := 1.0
	if maxRuns > 0 && runs > 0 && runs < maxRuns {
		share = float64(runs) / float64(maxRuns)
	}
	price := ref*frac*share + savingPerRun*float64(runs)*bpcResearchShare
	if floor := copyCost * bpcCostFloorMul; price < floor {
		price = floor
	}
	if price < 1 {
		price = 1
	}
	return roundISK(price)
}

// bpcBuyerCeiling is the most a builder pays for a copy: bpcBuyerCeilFrac of
// the reference price scaled by runs, plus the whole research saving on those
// runs (the builder does the arithmetic, not the seller's discount). Pure.
func bpcBuyerCeiling(ref float64, runs, maxRuns int64, savingPerRun float64) float64 {
	share := 1.0
	if maxRuns > 0 && runs > 0 && runs < maxRuns {
		share = float64(runs) / float64(maxRuns)
	}
	return ref*bpcBuyerCeilFrac*share + savingPerRun*float64(runs)
}

// fullBPOPrice is what a fully researched spare original asks: its reference
// price times the tech level's markup, plus the research. Pure.
func fullBPOPrice(ref float64, techTwo bool, research, mulT1, mulT2 float64) float64 {
	mul := mulT1
	if techTwo {
		mul = mulT2
	}
	return roundISK(ref*mul + research)
}

// printListingDesc is the one wording every print listing uses, so a pilot
// reading the escrow board can tell a copy from an original, how many runs it
// carries and what research is on it. Never "fully researched": research has
// no ceiling, so the levels are the whole story. Pure.
func printListingDesc(name string, isCopy bool, runs, me, pe int64) string {
	research := "unresearched"
	if me > 0 || pe > 0 {
		research = "researched"
	}
	if isCopy {
		unit := "runs"
		if runs == 1 {
			unit = "run"
		}
		return fmt.Sprintf("BPC: %s - %d %s (ME %d / PE %d, %s)", name, runs, unit, me, pe, research)
	}
	return fmt.Sprintf("BPO: %s (ME %d / PE %d, %s)", name, me, pe, research)
}

// ── static facts about a print ──────────────────────────────────────────────

// printFacts is what the copy and pricing code needs to know about a
// blueprint type. Static data, cached for the life of the process.
type printFacts struct {
	BPTypeID      int64
	Name          string
	ProductTypeID int64
	CategoryID    int64 // the PRODUCT's
	GroupID       int64 // the PRODUCT's
	Portion       int64
	MaxRuns       int64
	TechLevel     int64
	CopyRuns      int64 // copyRunsFor applied
}

var printFactsCache sync.Map // bpTypeID -> *printFacts

func (m *Mod) printFactsFor(ctx context.Context, bpTypeID int64) (*printFacts, error) {
	if v, ok := printFactsCache.Load(bpTypeID); ok {
		return v.(*printFacts), nil
	}
	f := &printFacts{BPTypeID: bpTypeID}
	err := m.db.QueryRowContext(ctx, `
		SELECT bt.typeName, b.productTypeID, g.categoryID, p.groupID,
		       COALESCE(p.portionSize, 1), COALESCE(b.maxProductionLimit, 10), COALESCE(b.techLevel, 1)
		  FROM invBlueprintTypes b
		  JOIN invTypes bt ON bt.typeID = b.blueprintTypeID
		  JOIN invTypes p ON p.typeID = b.productTypeID
		  JOIN invGroups g ON g.groupID = p.groupID
		 WHERE b.blueprintTypeID = ?`, bpTypeID).
		Scan(&f.Name, &f.ProductTypeID, &f.CategoryID, &f.GroupID, &f.Portion, &f.MaxRuns, &f.TechLevel)
	if err != nil {
		return nil, fmt.Errorf("print facts %d: %w", bpTypeID, err)
	}
	if f.Portion < 1 {
		f.Portion = 1
	}
	f.CopyRuns = copyRunsFor(f.CategoryID, f.GroupID, f.MaxRuns)
	printFactsCache.Store(bpTypeID, f)
	return f, nil
}

// ── settings ────────────────────────────────────────────────────────────────

func (m *Mod) fullResearchLevels(ctx context.Context) (me, pe int64) {
	return int64(escrowSettingInt(ctx, m.db, "rolebots.bpoFullME", bpoFullMEDefault)),
		int64(escrowSettingInt(ctx, m.db, "rolebots.bpoFullPE", bpoFullPEDefault))
}

func (m *Mod) bpcPriceFrac(ctx context.Context) float64 {
	return escrowSettingFloat(ctx, m.db, "rolebots.bpcPriceFrac", bpcPriceFracDefault)
}

func (m *Mod) fullBPOMarkups(ctx context.Context) (t1, t2 float64) {
	return escrowSettingFloat(ctx, m.db, "rolebots.bpoFullMarkupT1", bpoFullMarkupT1Default),
		escrowSettingFloat(ctx, m.db, "rolebots.bpoFullMarkupT2", bpoFullMarkupT2Default)
}

// ── valuation ───────────────────────────────────────────────────────────────

// printRefPrice is what an original of this type is worth to the pricing on
// both sides of the copy trade: the cheapest ask in the pilot's valuation
// regions, else its catalogue price, else a value derived from its product.
func (m *Mod) printRefPrice(ctx context.Context, b *botRun, bpTypeID int64) float64 {
	if asks, err := m.allAskByRegion(ctx); err == nil {
		var regions []int64
		if b != nil {
			regions = m.valuationRegions(ctx, b)
		}
		if ask := scopedAsk(asks, bpTypeID, regions); ask > 0 {
			return ask
		}
	}
	if base := m.typeBasePrice(ctx, bpTypeID); base > 0 {
		return base
	}
	f, err := m.printFactsFor(ctx, bpTypeID)
	if err != nil {
		return 0
	}
	vals, err := evedb.TypeValues(ctx, m.db, []int64{f.ProductTypeID})
	if err != nil {
		return 0
	}
	runs := f.MaxRuns
	if runs <= 0 || runs > printRefDerivedRuns {
		runs = printRefDerivedRuns
	}
	return math.Round(vals[f.ProductTypeID].Value * float64(f.Portion*runs) * printRefDerivedFrac)
}

// printSavingPerRun is the ISK one run of this print saves at ME me over the same run at ME 0: the
// material half of research, priced at TypeValues.
func printSavingPerRun(ctx context.Context, db *sql.DB, bpTypeID, me int64, peSkill int) float64 {
	if me <= 0 {
		return 0
	}
	bpType, err := evedb.GetBlueprintTypeInfo(ctx, db, bpTypeID)
	if err != nil {
		return 0
	}
	reqs, err := evedb.GetRequiredMaterials(ctx, db, bpTypeID, 1)
	if err != nil {
		return 0
	}
	typeIDs := make([]int64, 0, len(reqs))
	for _, r := range reqs {
		if r.DamagePerJob >= 1 {
			typeIDs = append(typeIDs, r.TypeID)
		}
	}
	prices, _ := evedb.TypeValues(ctx, db, typeIDs)
	saving := 0.0
	for _, r := range reqs {
		if r.DamagePerJob < 1 {
			continue
		}
		unit := prices[r.TypeID].Value
		atZero := evedb.CalcMaterialsNeeded(r.Quantity, bpType.WasteFactor, 0, peSkill)
		atME := evedb.CalcMaterialsNeeded(r.Quantity, bpType.WasteFactor, me, peSkill)
		saving += float64(atZero-atME) * unit
	}
	if saving < 0 {
		return 0
	}
	return saving
}
