package rolebots

// Blueprint trade: prints as merchandise, not only as factory capital.
//
//	arbitrage  a trader buys an untouched print where the NPC seeder stocks it and lists it, at a mark-up,
//	           in a region that has none. A print is 0.01 m3, so carrying one is the cheapest haul in the game.
//	resale     a pilot's surplus prints go back on the market: untouched originals as sell orders, researched
//	           ones through escrow, because a market fill destroys and recreates the print and burns the research
//	           (db/marketorders.go, loadResearchedBlueprints).
//	sourcing   an industrialist hunting a print reads escrow across its reach, not only where it is docked.
//
// Buyers bound what they pay (bpoEscrowAcceptable), so the mark-up is a price, not a trap.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

const (
	// bpoArbPremium is what a blueprint run asks over the pilot's ordinary
	// speculative mark-up (specMarkup with scarcity): the region has no print
	// at all, and the buyer's alternative is a trip of several regions.
	bpoArbPremium = 0.25
	// bpoArbConfDemand is the confidence a region's own factories add: a
	// print listed where industrialists are based has named customers.
	bpoArbConfDemand = 0.15
	// bpoArbRefine is how many candidate runs get a real source station and
	// haul measured.
	bpoArbRefine = 6
	// bpoScarcityCap bounds what a buyer with no market seller in reach will
	// pay for a print off escrow, as a multiple of its catalogue price.
	bpoScarcityCap = 2.0
	// bpoResaleSweepSec is how often one pilot looks for surplus prints.
	bpoResaleSweepSec = 6 * 3600
	// bpoResaleMaxPerSweep bounds the listings one sweep writes: each costs a
	// fee and, on the market, an order slot.
	bpoResaleMaxPerSweep = 3
	// bpoIdleDays is how long a print away from its owner's factory sits
	// unused by any job before it is surplus.
	bpoIdleDays = 14
	// bpoListPatienceMul stretches a print listing's patience (nudgeOrder):
	// a print's customer is a factory choosing its next project, which is
	// days, not the hours a crate of ammunition waits.
	bpoListPatienceMul = 8
)

func bpoArbEnabled(ctx context.Context, db *sql.DB) bool {
	return escrowSettingInt(ctx, db, "rolebots.bpoArb.enabled", 1) != 0
}

func bpoResaleEnabled(ctx context.Context, db *sql.DB) bool {
	return escrowSettingInt(ctx, db, "rolebots.bpoResale.enabled", 1) != 0
}

// sellOriginalsEnabled is the master switch over every bot sale of a blueprint original (arbitrage,
// surplus resale, a library's spare). It is off by default, so bots sell copies. A corporation never
// sells originals whatever it says (corpprintsale.go).
func sellOriginalsEnabled(ctx context.Context, db *sql.DB) bool {
	return escrowSettingInt(ctx, db, "rolebots.sellOriginals", 0) != 0
}

// ── type classification ─────────────────────────────────────────────────────

var bpTypeCache sync.Map // typeID -> bool

// isBlueprintType reports whether a type is category 9. Static data, cached.
func (m *Mod) isBlueprintType(ctx context.Context, typeID int64) bool {
	if v, ok := bpTypeCache.Load(typeID); ok {
		return v.(bool)
	}
	var cat int64
	err := m.db.QueryRowContext(ctx, `
		SELECT g.categoryID FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeID = ?`, typeID).Scan(&cat)
	if err != nil {
		return false
	}
	is := cat == 9
	bpTypeCache.Store(typeID, is)
	return is
}

// ── pure pricing ────────────────────────────────────────────────────────────

// bpoArbListPrice is what a carried print is listed at: the pilot's scarce
// speculative mark-up plus the blueprint premium, over what it paid.
func bpoArbListPrice(paid float64, tn Tuning) float64 {
	return roundISK(paid * (specMarkup(tn, true) + bpoArbPremium))
}

// bpoResalePrice prices a surplus print. ref is the cheapest ask in the seller's scope, or the catalogue
// price when nobody sells one; scarce says the seller's region has none on the book; research is what the
// print's ME saves a builder. An untouched original lists a shade under the book, never below the stable
// floor; with no book it takes the scarce mark-up. Research is charged on top at the research mark-up.
func bpoResalePrice(ref, base, research float64, scarce bool, tn Tuning, researchMarkup float64) float64 {
	price := ref
	if scarce {
		price = ref * specMarkup(tn, true)
	} else {
		price = ref * 0.98
		if floor := base * askFloorFraction; price < floor {
			price = floor
		}
	}
	if research > 0 {
		price += research * researchMarkup
	}
	if price < 0.01 {
		price = 0.01
	}
	return roundISK(price)
}

// bpcEscrowAcceptable is the buyer's bound on an escrowed COPY: no more than
// bpcBuyerCeiling over the reference price, and never more than the whole
// original would cost on the market. Pure.
func bpcEscrowAcceptable(price, ref float64, runs, maxRuns int64, savingPerRun, marketAsk float64) bool {
	if ref <= 0 || price <= 0 {
		return false
	}
	if marketAsk > 0 && price >= marketAsk {
		return false
	}
	return price <= bpcBuyerCeiling(ref, runs, maxRuns, savingPerRun)
}

// bpoEscrowAcceptable is the buyer's bound on an escrowed print: the price of
// the alternative (a market seller in reach, else the catalogue price times
// bpoScarcityCap) plus what the research is worth at the buyer's premium.
func bpoEscrowAcceptable(price, marketAsk, base, research, premium float64) bool {
	ref := marketAsk
	if ref <= 0 {
		ref = base * bpoScarcityCap
	}
	if ref <= 0 {
		return false
	}
	return price <= ref+research*premium
}

// ── shared reads ────────────────────────────────────────────────────────────

// regionBases is where a region's industrialists are based.
type regionBases struct {
	Bases      int
	TopStation int64 // the station most of them build at
	TopSystem  int64
	topCount   int
}

// industryBasesByRegion counts enabled pilots' factory bases per region. It is
// the demand side of a blueprint run: a print listed where nobody builds sits.
func (m *Mod) industryBasesByRegion(ctx context.Context) (map[int64]*regionBases, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshot(snapKey("industry-bases"), func() (map[int64]*regionBases, error) {
		rows, err := m.db.QueryContext(sctx, `
			SELECT s.regionID, p.industryStationID, s.solarSystemID, COUNT(*)
			FROM rolebot_profiles p
			JOIN rolebot_characters rc ON rc.characterID = p.characterID AND rc.enabled = 1
			JOIN staStations s ON s.stationID = p.industryStationID
			WHERE p.industryStationID > 0
			GROUP BY s.regionID, p.industryStationID, s.solarSystemID`)
		if err != nil {
			return nil, fmt.Errorf("industry bases: %w", err)
		}
		defer rows.Close()
		out := map[int64]*regionBases{}
		for rows.Next() {
			var reg, st, sys int64
			var n int
			if err := rows.Scan(&reg, &st, &sys, &n); err != nil {
				return nil, err
			}
			rb := out[reg]
			if rb == nil {
				rb = &regionBases{}
				out[reg] = rb
			}
			rb.Bases += n
			if n > rb.topCount {
				rb.TopStation, rb.TopSystem, rb.topCount = st, sys, n
			}
		}
		return out, rows.Err()
	})
}

// escrowBPO is one live escrow listing holding exactly one blueprint, an
// original or a copy.
type escrowBPO struct {
	EscrowID  int64
	IssuerID  int64
	StationID int64
	SystemID  int64
	RegionID  int64
	Price     float64
	TypeID    int64
	ItemID    int64
	ME, PE    int64
	Copy      bool
	Runs      int64 // runs left on a copy; -1 on an original
}

// escrowBPOListings is every public, single-print escrow listing, shared by the roster: originals and
// the copies researchers sell (bpcopy.go).
func (m *Mod) escrowBPOListings(ctx context.Context) ([]escrowBPO, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshot(snapKey("escrow-bpos"), func() ([]escrowBPO, error) {
		rows, err := m.db.QueryContext(sctx, `
			SELECT e.escrowID, e.issuerID, e.pickupStationID, e.pickupSolarSystemID,
			       e.pickupRegionID, e.collateral, i.typeID, i.itemID,
			       COALESCE(bp.materialLevel, 0), COALESCE(bp.productivityLevel, 0),
			       COALESCE(bp.copy, 0), COALESCE(bp.licensedProductionRunsRemaining, -1)
			FROM escrows e
			JOIN entity i ON i.locationID = e.crateContainerID
			JOIN invTypes t ON t.typeID = i.typeID
			JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 9
			LEFT JOIN invBlueprints bp ON bp.blueprintID = i.itemID
			WHERE e.state = 0 AND e.dateExpires > UNIX_TIMESTAMP()
			  AND e.assigneeID IS NULL
			  AND (COALESCE(bp.copy, 0) = 0 OR bp.licensedProductionRunsRemaining > 0)
			  AND (SELECT COUNT(*) FROM entity x WHERE x.locationID = e.crateContainerID) = 1`)
		if err != nil {
			return nil, fmt.Errorf("escrow prints: %w", err)
		}
		defer rows.Close()
		var out []escrowBPO
		for rows.Next() {
			var l escrowBPO
			var cp int
			if err := rows.Scan(&l.EscrowID, &l.IssuerID, &l.StationID, &l.SystemID, &l.RegionID,
				&l.Price, &l.TypeID, &l.ItemID, &l.ME, &l.PE, &cp, &l.Runs); err != nil {
				return nil, err
			}
			l.Copy = cp != 0
			out = append(out, l)
		}
		return out, rows.Err()
	})
}

// ── sourcing: escrow in the industrialist's blueprint hunt ──────────────────

// escrowBPOStop is an escrowed print the hunt can fly to and claim.
type escrowBPOStop struct {
	escrowBPO
	Jumps    int
	Research float64
}

// bpoStapleBuilds is how many manufacturing jobs on one blueprint type in bpoStapleWindowSec make it a
// staple: a type a builder builds constantly, for which owning the original beats renting copy runs.
const (
	bpoStapleBuilds    = 3
	bpoStapleWindowSec = 30 * 86400
)

// bestEscrowBPO finds the best escrowed print of a type within reach for a project of wantRuns runs,
// priced against the market offer (may be nil). A copy is the default: the cheapest per usable run net
// of research, if cheaper than the original. An original wins for a staple builder (bpoStapleBuilds jobs
// lately, wallet covers the print) or when no acceptable copy is in reach, and only if it beats the market.
func (m *Mod) bestEscrowBPO(ctx context.Context, b *botRun, bpTypeID int64,
	reach map[int64]int, market *marketStop, wantRuns int) *escrowBPOStop {
	if !escrowBuyingEnabled(ctx, m.db) {
		return nil
	}
	all, err := m.escrowBPOListings(ctx)
	if err != nil {
		return nil
	}
	marketAsk := 0.0
	if market != nil {
		marketAsk = market.Price
	}
	base := m.typeBasePrice(ctx, bpTypeID)
	skills, _ := m.charSkillsFor(ctx, b.CharID)
	peSkill := skills[skillProductionEff]
	premium := escrowSettingFloat(ctx, m.db, "escrow.bpoBotPremium", escrowBPOMarkup)
	balance, _ := evedb.GetCharBalance(ctx, m.db, b.CharID)
	fullME, fullPE := m.fullResearchLevels(ctx)
	mulT1, mulT2 := m.fullBPOMarkups(ctx)
	facts, _ := m.printFactsFor(ctx, bpTypeID)
	ref := 0.0 // resolved on the first listing that needs it
	if wantRuns < 1 {
		wantRuns = 1
	}

	var bestCopy, bestOrig *escrowBPOStop
	bestCopyPer, bestOrigNet := 0.0, 0.0
	for _, l := range all {
		if l.TypeID != bpTypeID || l.IssuerID == b.CharID {
			continue
		}
		j, ok := reach[l.SystemID]
		if !ok {
			continue
		}
		if spendable(balance) < l.Price {
			continue
		}
		if ref == 0 {
			ref = m.printRefPrice(ctx, b, bpTypeID)
		}
		if l.Copy {
			if facts == nil || l.Runs <= 0 {
				continue
			}
			saving := printSavingPerRun(ctx, m.db, bpTypeID, l.ME, peSkill)
			if !bpcEscrowAcceptable(l.Price, ref, l.Runs, facts.MaxRuns, saving, marketAsk) {
				continue
			}
			usable := l.Runs
			if usable > int64(wantRuns) {
				usable = int64(wantRuns)
			}
			per := (l.Price - saving*float64(usable)) / float64(usable)
			if bestCopy == nil || per < bestCopyPer {
				bestCopy = &escrowBPOStop{escrowBPO: l, Jumps: j, Research: saving * float64(usable)}
				bestCopyPer = per
			}
			continue
		}
		research := escrowBPOResearchValue(ctx, m.db, bpTypeID, l.ME, l.PE, peSkill)
		acceptable := bpoEscrowAcceptable(l.Price, marketAsk, base, research, premium)
		if !acceptable && printFullyResearched(l.ME, l.PE, fullME, fullPE) && ref > 0 {
			// A fully researched original asks big money (fullBPOPrice); a
			// builder that can carry it may pay that much.
			techTwo := facts != nil && facts.TechLevel >= 2
			acceptable = l.Price <= fullBPOPrice(ref, techTwo, research, mulT1, mulT2)
		}
		if !acceptable {
			continue
		}
		net := l.Price - research
		if bestOrig == nil || net < bestOrigNet {
			bestOrig = &escrowBPOStop{escrowBPO: l, Jumps: j, Research: research}
			bestOrigNet = net
		}
	}
	if bestOrig != nil && market != nil && bestOrigNet > marketAsk {
		bestOrig = nil
	}
	// A builder that looked and found no copy it would take is demand the tape cannot see (corpcopytape.go).
	if bestCopy == nil {
		noteCopyWanted(b.CharID, bpTypeID, clock.Unix())
	}
	if bestOrig != nil && (bestCopy == nil || bestOrig.Price <= bestCopy.Price ||
		(balance >= bestOrig.Price*industryAffordMultiple && m.isStaplePrint(ctx, b.CharID, facts))) {
		return bestOrig
	}
	return bestCopy
}

// isStaplePrint reports whether this pilot has built from the type at least
// bpoStapleBuilds times in the last bpoStapleWindowSec.
func (m *Mod) isStaplePrint(ctx context.Context, charID int64, facts *printFacts) bool {
	if facts == nil {
		return false
	}
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ramJobs
		 WHERE installerID = ? AND activityID = 1 AND outputTypeID = ? AND installTime > ?`,
		charID, facts.ProductTypeID, evedb.UnixToWin32(clock.Unix()-bpoStapleWindowSec)).Scan(&n)
	return n >= bpoStapleBuilds
}

// ── arbitrage: a print carried to a region with none ────────────────────────

// findBPOArbRun is the blueprint half of the speculative search: the best print this trader could buy in
// reach and list in a region with none on its market or escrow board. Candidates are the buildable census
// (capabilityLedger). One print per run: a region needs one seller, not ten.
func (m *Mod) findBPOArbRun(ctx context.Context, b *botRun, cfg tradeConfig, r travelRules,
	jumps map[int64]int, budget float64) (*tradeDeal, error) {
	if !bpoArbEnabled(ctx, m.db) || !sellOriginalsEnabled(ctx, m.db) || tradeInterest(b.Tune) < specMinInterest {
		return nil, nil
	}
	if m.freeOrderSlots(ctx, b.CharID) <= 0 {
		return nil, nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	reachRegion := map[int64]bool{}
	for sys := range jumps {
		reachRegion[u.meta[sys].RegionID] = true
	}
	ledger, err := m.capabilityLedger(ctx, 0)
	if err != nil {
		return nil, err
	}
	asks, err := m.allAskByRegion(ctx)
	if err != nil {
		return nil, err
	}
	bases, err := m.industryBasesByRegion(ctx)
	if err != nil {
		return nil, err
	}
	escrowed := map[[2]int64]bool{}
	if listings, lerr := m.escrowBPOListings(ctx); lerr == nil {
		for _, l := range listings {
			escrowed[[2]int64{l.TypeID, l.RegionID}] = true
		}
	}
	byType, _ := m.tradeActiveRuns(ctx, b.CharID)

	type cand struct {
		row       CapabilityRow
		srcAsk    float64
		dstRegion int64
		value     float64
	}
	var cands []cand
	for _, row := range ledger {
		bp := row.BPTypeID
		if bp == 0 || byType[bp] > 0 {
			continue
		}
		byRegion := asks[bp]
		if len(byRegion) == 0 {
			continue
		}
		src := 0.0
		for reg, p := range byRegion {
			if reachRegion[reg] && p <= budget && (src == 0 || p < src) {
				src = p
			}
		}
		if src == 0 {
			continue
		}
		var dst int64
		dstBases := 0
		for reg, rb := range bases {
			if !reachRegion[reg] || byRegion[reg] > 0 || escrowed[[2]int64{bp, reg}] {
				continue
			}
			if rb.Bases > dstBases || (rb.Bases == dstBases && reg < dst) {
				dst, dstBases = reg, rb.Bases
			}
		}
		if dst == 0 {
			continue
		}
		margin := bpoArbListPrice(src, b.Tune) - src
		cands = append(cands, cand{row: row, srcAsk: src, dstRegion: dst,
			value: margin * headcountPull(row.Pull)})
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("no print it can afford is missing from a region in reach that builds")
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].value > cands[j].value })
	if len(cands) > bpoArbRefine {
		cands = cands[:bpoArbRefine]
	}

	var best *tradeDeal
	for _, c := range cands {
		bp := c.row.BPTypeID
		srcStation, srcSys, srcPrice := m.cheapestReachableAsk(ctx, bp, jumps, budget)
		if srcStation == 0 {
			continue
		}
		rb := bases[c.dstRegion]
		if rb == nil || rb.TopStation == 0 {
			continue
		}
		if _, ok := jumps[rb.TopSystem]; !ok {
			continue
		}
		legs, lerr := m.reachFrom(ctx, srcSys, r)
		if lerr != nil {
			continue
		}
		haul, ok := legs[rb.TopSystem]
		if !ok {
			continue
		}
		list := bpoArbListPrice(srcPrice, b.Tune)
		if base := m.typeBasePrice(ctx, bp); base > 0 && list > base*specSpiralBaseMul {
			list = roundISK(base * specSpiralBaseMul)
		}
		rates := m.marketRates(ctx, b.CharID, rb.TopStation)
		profit := rates.netSale(list) - rates.brokerFee(list) - srcPrice
		if profit < tradeMinProfitISK {
			continue
		}
		conf := specConfBase + specConfScarce + bpoArbConfDemand
		if conf > specConfMax {
			conf = specConfMax
		}
		conf = m.specCalibrate(ctx, 9, conf) // tradecapital.go
		d := &tradeDeal{
			Kind: tradeDealSpec, TypeID: bp, TypeName: m.typeNameOr(ctx, bp), UnitVol: 0.01,
			CategoryID:   9,
			BuyStationID: srcStation, BuySystemID: srcSys,
			SellStationID: rb.TopStation, SellSystemID: rb.TopSystem,
			Ask: srcPrice, Units: 1, Outlay: srcPrice, Profit: profit,
			ListPrice: list, Confidence: conf,
			Evidence: fmt.Sprintf("no %s print for sale in %s, where %d factor(ies) are based",
				c.row.TypeName, m.nameOfRegion(ctx, c.dstRegion), rb.Bases),
			Jumps: jumps[srcSys] + haul,
		}
		d.Rank = tradeRank(d)
		if best == nil || d.Rank > best.Rank {
			best = d
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no blueprint run in reach clears its costs")
	}
	return best, nil
}

// cheapestReachableAsk is the best station selling a type that the pilot can reach and afford: among the
// 50 cheapest asks, the one with the lowest price once flying is priced in (jumpPriceCost a jump).
// The price returned is the ask.
func (m *Mod) cheapestReachableAsk(ctx context.Context, typeID int64, jumps map[int64]int,
	budget float64) (stationID, systemID int64, price float64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, solarSystemID, MIN(price) p FROM market_orders
		WHERE typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0
		GROUP BY stationID, solarSystemID ORDER BY p LIMIT 50`, typeID)
	if err != nil {
		return 0, 0, 0
	}
	defer rows.Close()
	best := math.Inf(1)
	for rows.Next() {
		var st, sys int64
		var p float64
		if rows.Scan(&st, &sys, &p) != nil {
			continue
		}
		j, ok := jumps[sys]
		if !ok || p > budget {
			continue
		}
		if cost := p * (1 + jumpPriceCost*float64(j)); cost < best {
			best, stationID, systemID, price = cost, st, sys, p
		}
	}
	return stationID, systemID, price
}

// ── resale: a pilot's surplus prints back on the market ─────────────────────

var (
	bpoResaleMu sync.Mutex
	bpoResaleAt = map[int64]int64{} // charID -> unix of last sweep
)

// heldPrint is one print in a station hangar, as the surplus rule sees it.
type heldPrint struct {
	ItemID, TypeID int64
	ME, PE         int64
	LastUsed       int64 // unix of the latest job it was installed in, 0 = never
}

// surplusPrints applies the rule, pure so the harness pins it. At the factory,
// every copy of a type but the best (highest ME, then PE) is surplus. Away
// from it, a print is surplus once no job has used it for idleSec. inUse are
// prints some trip or project has claimed and are never surplus.
func surplusPrints(prints []heldPrint, atFactory bool, inUse map[int64]bool, now, idleSec int64) []heldPrint {
	best := map[int64]heldPrint{}
	for _, p := range prints {
		if cur, ok := best[p.TypeID]; !ok || p.ME > cur.ME || (p.ME == cur.ME && p.PE > cur.PE) {
			best[p.TypeID] = p
		}
	}
	var out []heldPrint
	for _, p := range prints {
		if inUse[p.ItemID] {
			continue
		}
		isBest := best[p.TypeID].ItemID == p.ItemID
		switch {
		case !isBest:
			out = append(out, p)
		case !atFactory && now-p.LastUsed >= idleSec:
			out = append(out, p)
		}
	}
	return out
}

// resellSurplusPrints lists this pilot's surplus prints at the station it is
// docked in. Rides the same seams as sellHangarStrays: a station action that
// does not consume the turn.
func (m *Mod) resellSurplusPrints(ctx context.Context, b *botRun) {
	if b.StationID == 0 || b.State == nil || !bpoResaleEnabled(ctx, m.db) || !sellOriginalsEnabled(ctx, m.db) {
		return
	}
	now := clock.Unix()
	bpoResaleMu.Lock()
	if now-bpoResaleAt[b.CharID] < bpoResaleSweepSec {
		bpoResaleMu.Unlock()
		return
	}
	bpoResaleAt[b.CharID] = now
	bpoResaleMu.Unlock()

	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, COALESCE(bp.materialLevel, 0), COALESCE(bp.productivityLevel, 0),
		       COALESCE((SELECT MAX(j.installTime) FROM ramJobs j
		                  WHERE j.installerID = e.ownerID AND j.installedItemID = e.itemID), 0)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 9
		LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND COALESCE(bp.copy, 0) = 0`,
		b.CharID, b.StationID)
	if err != nil {
		return
	}
	var prints []heldPrint
	for rows.Next() {
		var p heldPrint
		var installWin int64
		if rows.Scan(&p.ItemID, &p.TypeID, &p.ME, &p.PE, &installWin) == nil {
			if installWin > 0 {
				p.LastUsed = installWin/10_000_000 - 11644473600
			}
			prints = append(prints, p)
		}
	}
	rows.Close()
	if len(prints) == 0 {
		return
	}
	d := &b.State.Data
	inUse := map[int64]bool{d.IndBPItemID: true, d.IndFetchItemID: true, d.LabBPItemID: true}
	for _, c := range d.LabCooking {
		inUse[c.BPItemID] = true
	}
	atFactory := b.Profile != nil && b.Profile.IndustryStationID == b.StationID
	surplus := surplusPrints(prints, atFactory, inUse, now, bpoIdleDays*86400)
	if len(surplus) == 0 {
		return
	}

	asks, _ := m.allAskByRegion(ctx)
	regions := m.valuationRegions(ctx, b)
	stationRegion := m.stationRegionOf(ctx, b.StationID)
	skills, _ := m.charSkillsFor(ctx, b.CharID)
	peSkill := skills[skillProductionEff]
	researchMarkup := escrowSettingFloat(ctx, m.db, "escrow.bpoBotPremium", escrowBPOMarkup)
	expiry := escrowSettingInt(ctx, m.db, "escrow.bpoDaysToExpire", escrowBPODefaultExpiry)
	fullME, fullPE := m.fullResearchLevels(ctx)
	mulT1, mulT2 := m.fullBPOMarkups(ctx)

	listed := 0
	for _, p := range surplus {
		if listed >= bpoResaleMaxPerSweep {
			break
		}
		base := m.typeBasePrice(ctx, p.TypeID)
		ref := scopedAsk(asks, p.TypeID, regions)
		if ref <= 0 {
			ref = base
		}
		if ref <= 0 {
			continue
		}
		scarce := asks[p.TypeID][stationRegion] <= 0
		name := m.typeNameOr(ctx, p.TypeID)
		researched := p.ME > 0 || p.PE > 0
		research := 0.0
		if researched {
			research = escrowBPOResearchValue(ctx, m.db, p.TypeID, p.ME, p.PE, peSkill)
		}
		price := bpoResalePrice(ref, base, research, scarce, b.Tune, researchMarkup)
		if researched {
			// Only a spare at the research target is sold, and for big money (bpcopy.go fullBPOPrice). A partly
			// researched print is capital: worth researching on and copying.
			if !printFullyResearched(p.ME, p.PE, fullME, fullPE) {
				continue
			}
			techTwo := false
			if f, ferr := m.printFactsFor(ctx, p.TypeID); ferr == nil {
				techTwo = f.TechLevel >= 2
			}
			price = fullBPOPrice(ref, techTwo, research, mulT1, mulT2)
			desc := printListingDesc(name, false, 0, p.ME, p.PE)
			if _, err := m.postPrintListing(ctx, b, p.ItemID, p.TypeID, price, desc, expiry, false, 0, p.ME, p.PE, 0); err != nil {
				m.logBot(b.CharID, logInfo, "escrow", "could not list its surplus %s (ME %d): %v", name, p.ME, err)
				continue
			}
			m.logBot(b.CharID, logInfo, "escrow",
				"listed a surplus %s (ME %d / PE %d) on escrow for %.0f ISK (research worth %.0f)",
				name, p.ME, p.PE, price, research)
			listed++
			continue
		}
		if m.listSurplusPrint(ctx, b, p.ItemID, p.TypeID, name, price, base) {
			listed++
		}
	}
}

// listSurplusPrint rests one untouched original on the market and registers
// it as a blueprint order, so the upkeep sweep gives it a print's patience.
func (m *Mod) listSurplusPrint(ctx context.Context, b *botRun, itemID, typeID int64, name string,
	price, base float64) bool {
	if m.freeOrderSlots(ctx, b.CharID) <= 0 {
		return false
	}
	price, _ = m.anchoredAsk(ctx, b.Tune, b.StationID, typeID, price)
	rates := m.marketRates(ctx, b.CharID, b.StationID)
	if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err != nil || bal < rates.brokerFee(price) {
		return false
	}
	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID, typeID,
		itemID, price, 1, -1 /*station range*/, 1, tradeListDays, false)
	if err != nil || out == nil || out.ErrorMsg != "" {
		return false
	}
	unitCost := base * askFloorFraction
	if out.RestingOrderID != 0 {
		m.registerOrder(ctx, b.CharID, out.RestingOrderID, typeID, b.StationID, b.SystemID,
			name, false, price, unitCost, 1, tradeDealBPO)
		m.bookBrokerFee(ctx, b, out.RestingOrderID, typeID, name, price, 1)
	}
	m.logBot(b.CharID, logInfo, "market",
		"listed a surplus %s it no longer builds from at %.0f ISK in %s",
		name, price, m.nameOfSystem(ctx, b.SystemID))
	m.chatterNoteBot(ctx, b.CharID, "pers.sale", map[string]string{"what": name, "isk": iskWords(price)}, typeID)
	return true
}

func init() {
	registerCharForget("bpoResaleAt", func(id int64) {
		bpoResaleMu.Lock()
		delete(bpoResaleAt, id)
		bpoResaleMu.Unlock()
	})
}
