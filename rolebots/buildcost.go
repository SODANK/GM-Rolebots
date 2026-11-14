package rolebots

// Build cost: what a candidate costs this pilot to make, as opposed to what it costs to buy.
// candidateUnitCosts (buildbook.go) prices the buy side: cheapest live ask, else
// invTypes.basePrice. No hull above a frigate is ever listed, so hulls fall to basePrice, a
// retail figure far above the real bill, while ammunition gets a real ask.
// The bill here uses ideal materials (no waste, no ME level). That is cheap enough to run
// over a 200-row book, and affordability only needs the order right.
// industryStartProject prices the real bill for the chosen candidate.

import (
	"context"
)

// candidateBuildCosts prices one unit of each candidate at its bill of materials, keyed by
// product typeID like candidateUnitCosts. A candidate whose bill cannot be priced is
// absent, and the caller falls back to the retail number.
func (m *Mod) candidateBuildCosts(ctx context.Context, b *botRun, cands []indCandidate) map[int64]float64 {
	out := make(map[int64]float64, len(cands))
	if len(cands) == 0 {
		return out
	}

	// Blueprint to product, so bill rows fold back onto the candidate the caller keys on.
	product := make(map[int64]int64, len(cands))
	bpIDs := make([]int64, 0, len(cands))
	prodIDs := make([]int64, 0, len(cands))
	for _, c := range cands {
		if c.BPTypeID == 0 {
			continue
		}
		product[c.BPTypeID] = c.TypeID
		bpIDs = append(bpIDs, c.BPTypeID)
		prodIDs = append(prodIDs, c.TypeID)
	}
	if len(bpIDs) == 0 {
		return out
	}

	// The filter is ConsumeMaterials' own (db/ramdb.go): it skips recycle rows, rows with
	// damagePerJob < 1 and categoryID 16 skill requirements.
	where, args := inClause(bpIDs)
	rows, err := m.db.QueryContext(ctx, `
		SELECT m.typeID, m.requiredTypeID, m.quantity
		  FROM tl2materialsfortypewithactivity m
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE m.activity = 1 AND m.recycle = 0 AND m.damagePerJob >= 1
		   AND COALESCE(g.categoryID, 0) <> 16
		   AND m.typeID IN `+where, args...)
	if err != nil {
		rlogf("rolebots: candidate build costs: %v", err)
		return out
	}
	bill := make(map[int64][]billLine, len(bpIDs))
	mats := map[int64]bool{}
	for rows.Next() {
		var bpID, matID, qty int64
		if rows.Scan(&bpID, &matID, &qty) != nil {
			continue
		}
		if qty <= 0 {
			continue
		}
		bill[bpID] = append(bill[bpID], billLine{TypeID: matID, Qty: qty})
		mats[matID] = true
	}
	rows.Close()
	if len(bill) == 0 {
		return out
	}

	// A run makes portionSize units (100 for most charges, 1 for a hull) and the bill is per
	// run, so the unit cost divides by it.
	portion := make(map[int64]int64, len(prodIDs))
	pwhere, pargs := inClause(prodIDs)
	prows, perr := m.db.QueryContext(ctx,
		`SELECT typeID, COALESCE(portionSize, 1) FROM invTypes WHERE typeID IN `+pwhere, pargs...)
	if perr != nil {
		rlogf("rolebots: candidate portion sizes: %v", perr)
		return out
	}
	for prows.Next() {
		var id, size int64
		if prows.Scan(&id, &size) == nil && size > 0 {
			portion[id] = size
		}
	}
	prows.Close()

	// One price per distinct material through estimatePriceRefIn, the anchor supplyBidCeiling
	// also uses (industrybids.go). A guessed line stays out so buildCostPerUnit abstains.
	regions := m.valuationRegions(ctx, b)
	price := make(map[int64]float64, len(mats))
	for id := range mats {
		if p, src, err := m.estimatePriceSourceIn(ctx, regions, id); err == nil && p > 0 && src != priceGuess {
			price[id] = p
		}
	}

	for bpID, lines := range bill {
		prodID := product[bpID]
		if cost, ok := buildCostPerUnit(lines, price, portion[prodID]); ok {
			out[prodID] = cost
		}
	}
	return out
}

// billLine is one material a run consumes.
type billLine struct {
	TypeID int64
	Qty    int64
}

// buildCostPerUnit folds a bill into a per-unit cost. It is pure so a test can pin the
// portion-size division and the abstention. It reports ok=false when any line has no
// price, because a partial sum is too low and the caller falls back to the retail proxy.
func buildCostPerUnit(lines []billLine, price map[int64]float64, portion int64) (float64, bool) {
	if len(lines) == 0 {
		return 0, false
	}
	if portion < 1 {
		portion = 1
	}
	total := 0.0
	for _, l := range lines {
		p := price[l.TypeID]
		if p <= 0 || l.Qty <= 0 {
			return 0, false
		}
		total += p * float64(l.Qty)
	}
	if total <= 0 {
		return 0, false
	}
	return total / float64(portion), true
}
