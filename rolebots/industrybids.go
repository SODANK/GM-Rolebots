package rolebots

// The factory's standing order for material the pilot can neither dig nor buy in reach. A bid at the
// factory is a statement the rest of the world acts on: miners sell into the best local bid on the spot
// (miningDispose, mining.go), traders see an arbitrage deal when the bid beats what the mineral costs
// elsewhere (trade.go, bestOutlet), and players see an order on the book. The ledger (minerals.go) still
// steers prospectors to the right rock; the bid adds escrow, which turns a want into a price.
//
// A bid belongs to the factory, not to the project that prompted it: fills land in the factory hangar,
// where industryShortfall reads them on the next project. Bids at a station the pilot left
// (dropStaleSupplyBids) and bids for lines now covered (dropFilledSupplyBids) are pulled; the rest age out.
// See docs/comment-notes-b12.md#supply-bids.

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

const (
	// How long a supply bid rests: the same fortnight tradebids.go uses for a speculative bid. An order
	// should end because its owner changed its mind, not on the market's expiry timer.
	supplyBidDays = 14

	// How many supply bids one factory runs at once. Each escrows its full value, so this caps committed
	// capital. Three matches the unreachable lines of a classic bill (Nocxium, Zydrine, Megacyte for a
	// highsec pilot).
	supplyBidMaxOpen = 3

	// The most of its wallet a factory ties up in supply bids. It is under trade's bidWalletFrac (0.35)
	// because blueprints and install fees already claim the wallet, and escrow comes off the balance
	// that industryStartProject's affordability gate reads.
	supplyBidWalletFrac = 0.20

	// How much more than the batch needs it bids for. Escrow is refunded when an order is pulled, so
	// asking for extra costs a broker fee, against a second order and another fortnight next time.
	supplyBidOverbuy = 1.25

	// How far under the most this build could pay the bid sits. Not zero: a bid at break-even makes the
	// build earn nothing, and bidPriceFor treats a price equal to its ceiling as no price.
	supplyBidMargin = 0.08

	// The most it will pay over the going rate for delivery to its door. A premium under about 1.4 never
	// reaches a hauler's candidate list, because the arbitrage scan needs the pilot's TradeMinMarginFrac
	// (0.15 to 0.35) plus fee and tax. The build headroom is the real guard (supplyBidCeiling takes the lower).
	supplyBidOverGoing = 1.60

	// Under this value the order is not worth its broker fee or slot: the module's usual 20,000 ISK line
	// between stock and litter.
	supplyBidMinValue = 20000.0

	// Lots of roster-wide ledger shortfall past which a mineral is in famine and "the pilot could dig it"
	// stops blocking a bid (supplyBidTargets). Counted in lots (mineralListFloors) so each mineral's bar
	// is worth similar money. See docs/comment-notes-b12.md#supply-bids.
	supplyBidFamineLots = 10
)

// supplyBidFamineUnits is the famine bar for one mineral, in its own units. A mineral with no lot
// size falls back to a flat 100,000.
func supplyBidFamineUnits(typeID int64) int64 {
	lot, ok := mineralListFloors[typeID]
	if !ok || lot <= 0 {
		return 100_000
	}
	return lot * supplyBidFamineLots
}

// supplyBidTarget is one material line the factory has given up on getting
// for itself, with the ceiling the build can pay for it.
type supplyBidTarget struct {
	TypeID   int64
	TypeName string
	Needed   int64
	Ceiling  float64
	Why      string
}

// maybeLeaveSupplyBids places up to the pilot's remaining bid allowance for the outstanding bill, at
// commit and again from the refine hub when a later shortfall shows. The order rests on the factory's
// book wherever the pilot stands. Errors are logged, never returned: a project does not fail over an
// order it could not place.
func (m *Mod) maybeLeaveSupplyBids(ctx context.Context, b *botRun, line *manufactureLine,
	needs, short []matNeed, reach map[int64]int) {

	factoryID := b.State.Data.IndFactoryID
	factorySysID := b.State.Data.IndFactorySysID
	if factoryID == 0 || factorySysID == 0 {
		return
	}
	targets, err := m.supplyBidTargets(ctx, b, line, factoryID, needs, short, reach)
	if err != nil {
		rlogf("rolebots: supply bid targets char=%d: %v", b.CharID, err)
		return
	}
	if len(targets) == 0 {
		return
	}

	room, budget, err := m.supplyBidAllowance(ctx, b)
	if err != nil {
		rlogf("rolebots: supply bid allowance char=%d: %v", b.CharID, err)
		return
	}
	if room <= 0 || budget < supplyBidMinValue {
		return
	}

	ids := make([]int64, 0, len(targets))
	for _, t := range targets {
		ids = append(ids, t.TypeID)
	}
	book, err := m.stationBook(ctx, factoryID, ids)
	if err != nil {
		rlogf("rolebots: supply bid book char=%d: %v", b.CharID, err)
		return
	}

	for _, t := range targets {
		if room <= 0 || budget < supplyBidMinValue {
			return
		}
		// The price is built down from what the build can pay, as a trader's is from what the outlet pays:
		// same function, same three rules (spread, under the local ask, over the local bid).
		price, ok := bidPriceFor(t.Ceiling, supplyBidMargin, book[t.TypeID])
		if !ok {
			continue
		}
		units := int64(float64(t.Needed) * supplyBidOverbuy)
		if byBudget := int64(budget / price); byBudget < units {
			units = byBudget
		}
		if units < 1 || price*float64(units) < supplyBidMinValue {
			continue
		}
		spent := m.placeSupplyBid(ctx, b, factoryID, factorySysID, t, price, units)
		if spent <= 0 {
			continue
		}
		budget -= spent
		room--
	}
}

// supplyBidTargets selects the outstanding lines this pilot can neither dig nor buy in reach, priced.
func (m *Mod) supplyBidTargets(ctx context.Context, b *botRun, line *manufactureLine,
	factoryID int64, needs, short []matNeed, reach map[int64]int) ([]supplyBidTarget, error) {

	// What it could dig, as industryFeedableRuns and prospectTarget ask it (oresReachable, industry.go): an ore
	// that yields this mineral and that the pilot may fly to. A non-mineral has no ore rows and falls out too.
	rules := rulesForBot(b)
	reachable := oresReachable(rules)
	var unreachable []matNeed
	for _, s := range short {
		if s.Needed <= 0 {
			continue
		}
		if _, canDig := m.mineralPerM3Reach(ctx, s.TypeID, reachable); canDig {
			// Diggable in theory, but belts deplete. When the roster-wide ledger says the mineral is starving
			// projects everywhere, bid anyway; the bid also takes a refiner's sub-lot surplus (miningRefineAndList).
			if m.mineralShortageUnits(ctx, s.TypeID) < supplyBidFamineUnits(s.TypeID) {
				continue
			}
		}
		unreachable = append(unreachable, s)
	}
	if len(unreachable) == 0 {
		return nil, nil
	}

	// What it could buy: anything already listed inside the pilot's travel reach is the shopping trip's
	// job, so bidding for it would pay a premium for an errand already under way.
	ids := make([]int64, 0, len(unreachable))
	for _, s := range unreachable {
		ids = append(ids, s.TypeID)
	}
	listed, err := m.askedForInReach(ctx, ids, reach)
	if err != nil {
		return nil, err
	}

	// A type this pilot already has a bid out for is skipped: one standing offer, not two bidding against
	// each other.
	held, err := m.openSupplyBidTypes(ctx, b.CharID, factoryID)
	if err != nil {
		return nil, err
	}

	var out []supplyBidTarget
	for _, s := range unreachable {
		if listed[s.TypeID] || held[s.TypeID] {
			continue
		}
		ceiling, why := m.supplyBidCeiling(ctx, b, line, needs, s)
		if ceiling <= 0 {
			continue
		}
		// A mineral whose ore spawns local to the factory is bid for as the rock instead, never as well as
		// (both answer one shortfall). Ore is local and minerals travel. See docs/comment-notes-b12.md#supply-bids.
		if m.mineralLocalToFactory(ctx, b, s.TypeID) {
			if t, ok := m.oreBidTargetFor(ctx, b, s, ceiling, held); ok {
				out = append(out, t)
				continue
			}
		}
		out = append(out, supplyBidTarget{
			TypeID: s.TypeID, TypeName: m.typeNameOr(ctx, s.TypeID),
			Needed: s.Needed, Ceiling: ceiling, Why: why,
		})
	}
	return out, nil
}

// oreBidCeiling converts what a factory can pay per unit of metal into what it can pay per unit of
// rock, at a given net yield. Pure, because it decides whether an ore bid is solvent.
func oreBidCeiling(mineralCeiling, perUnit, net float64) float64 {
	if mineralCeiling <= 0 || perUnit <= 0 || net <= 0 {
		return 0
	}
	return mineralCeiling * perUnit * net
}

// oreBidTargetFor picks the rock a factory should bid for instead of a mineral, or reports that it
// should not. The densest rock wins (most wanted metal per m3), because the hold limits a seller.
func (m *Mod) oreBidTargetFor(ctx context.Context, b *botRun, s matNeed,
	mineralCeiling float64, held map[int64]bool) (supplyBidTarget, bool) {

	vals, err := m.oreValues(ctx, m.valuationRegions(ctx, b), oreValueBid)
	if err != nil {
		return supplyBidTarget{}, false
	}
	base := m.refineHandAt(ctx, b, b.State.Data.IndFactoryID, 0)

	best := supplyBidTarget{}
	bestDens, bestCoValue := 0.0, 0.0
	for id, v := range vals {
		per := v.PerUnit[s.TypeID]
		if per <= 0 || held[id] {
			continue
		}
		net := base.WithOreSkill(m.oreSkillLevelFor(ctx, b.CharID, v.GroupID)).Net()
		ceiling := oreBidCeiling(mineralCeiling, per, net)
		if ceiling <= 0 {
			continue
		}
		// Rank on density, not metal per ISK of ceiling (the same for every ore). Ties go to the rock whose other
		// minerals are worth more per m3, then to typeID, since vals is a map and a tie would move the bid every pass.
		if v.Volume <= 0 {
			continue
		}
		dens, coValue := per*net/v.Volume, v.Intrinsic/v.Volume
		if oreBidBetter(dens, coValue, id, bestDens, bestCoValue, best.TypeID) {
			bestCoValue = coValue
			units := int64(math.Ceil(float64(s.Needed) / (per * net)))
			best, bestDens = supplyBidTarget{
				TypeID: id, TypeName: v.Name, Needed: units, Ceiling: ceiling,
				Why: fmt.Sprintf("rock for its %s line — %d× at up to %.2f, refining at %.0f%%",
					m.typeNameOr(ctx, s.TypeID), units, ceiling, net*100),
			}, dens
		}
	}
	return best, best.TypeID != 0
}

// oreBidBetter is oreBidTargetFor's ranking, pure: denser in the wanted metal
// first, then the better co-product per m³, then the lower typeID.
func oreBidBetter(dens, coValue float64, id int64, bestDens, bestCoValue float64, bestID int64) bool {
	switch {
	case bestID == 0:
		return true
	case dens != bestDens:
		return dens > bestDens
	case coValue != bestCoValue:
		return coValue > bestCoValue
	}
	return id < bestID
}

// mineralLocalToFactory reports whether the metal's ore is local to this pilot's factory: a family
// spawning in the factory's own space is one of the mineral's best sources (mineralIsLocal). Anything
// unreadable answers true.
func (m *Mod) mineralLocalToFactory(ctx context.Context, b *botRun, mineralID int64) bool {
	sysID := b.State.Data.IndFactorySysID
	if sysID == 0 {
		return true
	}
	u, err := m.universe(ctx)
	if err != nil {
		return true
	}
	ms, ok := u.meta[sysID]
	if !ok {
		return true
	}
	vals, err := m.oreValues(ctx, m.valuationRegions(ctx, b), oreValueBid)
	if err != nil || len(vals) == 0 {
		return true
	}
	local := map[string]bool{}
	for _, w := range destiny.OreDistribution(ms.Security, u.sovFaction(ctx, m.db, sysID)) {
		local[w.Name] = true
	}
	return mineralIsLocal(local, mineralDensityByFamily(vals, mineralID), mineralLocalTopN)
}

// mineralLocalTopN is how many of a mineral's densest ore families count as a real source. Three:
// Zydrine's are Crokite, Bistot and Gneiss (all nullsec), so a trace in Hedbergite does not make lowsec
// Zydrine country, while Isogen's three include Omber and Kernite and keep it a highsec metal.
const mineralLocalTopN = 3

// mineralDensityByFamily is the most of one mineral a m³ of each ore family
// yields, across its grades, keyed by family name (the belt table's key).
func mineralDensityByFamily(vals map[int64]oreValue, mineralID int64) map[string]float64 {
	out := map[string]float64{}
	for _, v := range vals {
		per := v.PerUnit[mineralID]
		if per <= 0 || v.Volume <= 0 || v.Group == "" {
			continue
		}
		if d := per / v.Volume; d > out[v.Group] {
			out[v.Group] = d
		}
	}
	return out
}

// mineralIsLocal is the rule, pure: one of the families spawning here is among the topN densest
// sources of the mineral. A mineral no ore yields is never local, as there is no rock to bid for.
func mineralIsLocal(local map[string]bool, density map[string]float64, topN int) bool {
	type fam struct {
		name string
		d    float64
	}
	fams := make([]fam, 0, len(density))
	for n, d := range density {
		fams = append(fams, fam{n, d})
	}
	sort.Slice(fams, func(i, j int) bool {
		if fams[i].d != fams[j].d {
			return fams[i].d > fams[j].d
		}
		return fams[i].name < fams[j].name
	})
	for i, f := range fams {
		if i >= topN {
			break
		}
		if local[f.name] {
			return true
		}
	}
	return false
}

// supplyBidCeiling is the most this factory can pay per unit, and why. The lower of two ceilings wins:
// the going rate plus a delivery premium (anchored to the cheapest ask, estimatePrice, which only falls
// as supply appears) and the build headroom (the batch's sale price worked back through industryUnitPrice).
// A product with no market price has only the going rate, as in industryStartProject's sell-above-cost gate.
func (m *Mod) supplyBidCeiling(ctx context.Context, b *botRun, line *manufactureLine,
	needs []matNeed, target matNeed) (float64, string) {

	if target.Needed <= 0 {
		return 0, ""
	}
	going, err := m.estimatePriceFor(ctx, b, target.TypeID)
	if err != nil || going <= 0 {
		return 0, ""
	}
	ceiling, why := going*supplyBidOverGoing, "the going rate plus a delivery premium"

	d := &b.State.Data
	market, err := m.estimatePriceFor(ctx, b, d.IndTypeID)
	if err != nil || market <= 0 || d.IndRuns < 1 || line == nil {
		return ceiling, why
	}
	bpType, err := evedb.GetBlueprintTypeInfo(ctx, m.db, d.IndBPTypeID)
	if err != nil {
		return ceiling, why
	}
	portion, err := evedb.GetTypePortionSize(ctx, m.db, bpType.ProductTypeID)
	if err != nil || portion <= 0 {
		portion = 1
	}
	units := int64(d.IndRuns) * portion
	if units <= 0 {
		return ceiling, why
	}
	margin := b.Tune.IndustryMargin
	if margin <= 0 {
		return ceiling, why
	}
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return ceiling, why
	}
	// The print's own researched levels: PE shortens the duration, so quoting a researched print at PE 0
	// overstates the fee.
	_, bidPE := m.blueprintLevels(ctx, d.IndBPItemID)
	fees := line.Info.CostInstall +
		line.Info.CostPerHour*manufactureDurationSec(bpType, bidPE, skills[skillIndustry], d.IndRuns, line)/3600

	// Everything else the bill wants, at what it would cost, over the whole bill and not just the shortfall:
	// minerals already in the hangar are still part of what the batch is worth.
	others := 0.0
	for _, n := range needs {
		if n.TypeID == target.TypeID || n.Needed <= 0 {
			continue
		}
		if p, perr := m.estimatePriceFor(ctx, b, n.TypeID); perr == nil {
			others += p * float64(n.Needed)
		}
	}
	// industryUnitPrice inverted: ask = (matValue + fees + bpo/amort)/units x margin. The blueprint term must
	// match what that function charges (industryBPOAmortProjects).
	maxMat := market*float64(units)/margin - fees -
		d.IndBPOCost/float64(industryBPOAmortProjects)
	if head := (maxMat - others) / float64(target.Needed); head < ceiling {
		ceiling, why = head, "what this batch can pay and still clear at market"
	}
	if ceiling <= 0 {
		return 0, ""
	}
	return ceiling, why
}

// placeSupplyBid commits one bid: the order, the registry row the sweep reads, the ledger and the broker
// fee. It returns what it tied up, 0 when nothing rested. stationID and systemID are the factory's, not
// necessarily where the pilot stands.
func (m *Mod) placeSupplyBid(ctx context.Context, b *botRun, stationID, systemID int64,
	t supplyBidTarget, price float64, units int64) float64 {

	out, err := evedb.PlaceBuyOrder(ctx, m.db, b.CharID, stationID, t.TypeID,
		price, units, -1 /*station range*/, 1, supplyBidDays, false)
	if err != nil {
		rlogf("rolebots: supply bid char=%d type=%d: %v", b.CharID, t.TypeID, err)
		return 0
	}
	if out.ErrorMsg != "" {
		m.logBot(b.CharID, logWarn, "industry",
			"supply bid refused (%s) — %d× %s at %.2f", out.ErrorMsg, units, t.TypeName, price)
		return 0
	}

	// A bid priced under the local ask should not fill on placement, but the book can move between pricing
	// and placing. A fill is material in the factory hangar, which industryShortfall finds on the next pass.
	if out.FilledQty > 0 {
		spent := 0.0
		for _, f := range out.Fills {
			spent += f.Price * float64(f.Quantity)
		}
		e := tradeEntry{
			CharID: b.CharID, Kind: tradeKindBuy, TypeID: t.TypeID, TypeName: t.TypeName,
			Qty: out.FilledQty, Price: spent / float64(out.FilledQty), Value: spent,
			Station: stationID, System: systemID,
			Note: "an ask was sitting under the supply bid it meant to leave — bought on the spot",
		}
		m.bookTrade(ctx, e)
		m.notePeerFills(ctx, e, out.Fills, true, "materials")
	}

	resting := units - out.FilledQty
	if resting <= 0 || out.RestingOrderID == 0 {
		return 0
	}
	m.registerOrder(ctx, b.CharID, out.RestingOrderID, t.TypeID, stationID, systemID,
		t.TypeName, true, price, price, resting, tradeDealSupply)
	m.bookTrade(ctx, tradeEntry{
		CharID: b.CharID, Kind: tradeKindList, TypeID: t.TypeID, TypeName: t.TypeName,
		Qty: resting, Price: price, Value: price * float64(resting),
		Station: stationID, System: systemID, OrderID: out.RestingOrderID,
		Note: fmt.Sprintf("supply bid for its %s build — ceiling was %s",
			b.State.Data.IndTypeName, t.Why),
	})
	m.bookBrokerFee(ctx, b, out.RestingOrderID, t.TypeID, t.TypeName, price, resting)
	m.logBot(b.CharID, logInfo, "industry",
		"bidding %.2f for %d× %s at %s — it can neither mine it nor buy it in reach, so somebody else can fly it in (ceiling: %s)",
		price, resting, t.TypeName, m.stationLabel(ctx, stationID), t.Why)
	return price * float64(resting)
}

// ── allowance, book, upkeep ─────────────────────────────────────────────────

// supplyBidAllowance is how many more bids this factory may leave and what it may commit. The escrow
// ceiling counts everything already tied up across every standing bid, trade and supply alike: there is
// one wallet.
func (m *Mod) supplyBidAllowance(ctx context.Context, b *botRun) (int, float64, error) {
	slots := int(m.freeOrderSlots(ctx, b.CharID))
	if slots <= 0 {
		return 0, 0, nil
	}
	_, escrowed, err := m.openBidExposure(ctx, b.CharID)
	if err != nil {
		return 0, 0, err
	}
	var mine int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_orders
		 WHERE characterID = ? AND bid = 1 AND closedAt = 0 AND kind = ?`,
		b.CharID, tradeDealSupply).Scan(&mine); err != nil {
		return 0, 0, fmt.Errorf("open supply bids: %w", err)
	}
	room := supplyBidMaxOpen - int(mine)
	if slots < room {
		room = slots
	}
	if room <= 0 {
		return 0, 0, nil
	}
	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return 0, 0, fmt.Errorf("wallet: %w", err)
	}
	budget := balance*supplyBidWalletFrac - escrowed
	if budget < 0 {
		budget = 0
	}
	return room, budget, nil
}

// openSupplyBidTypes is what this pilot already has bid for at this station, of any kind: a trade bid and
// a supply bid for one mineral at one counter would outbid itself.
func (m *Mod) openSupplyBidTypes(ctx context.Context, charID, stationID int64) (map[int64]bool, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID FROM rolebot_orders
		 WHERE characterID = ? AND stationID = ? AND bid = 1 AND closedAt = 0`,
		charID, stationID)
	if err != nil {
		return nil, fmt.Errorf("open bids here: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// stationBook is localBook for a bare list of type IDs: the same two-sided quote, without a []bidType.
func (m *Mod) stationBook(ctx context.Context, stationID int64, typeIDs []int64) (map[int64]localQuote, error) {
	out := map[int64]localQuote{}
	if len(typeIDs) == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID,
		       MIN(CASE WHEN bid = 0 THEN price END),
		       MAX(CASE WHEN bid = 1 THEN price END)
		  FROM market_orders
		 WHERE stationID = ? AND orderState = 1 AND volRemaining > 0
		   AND typeID IN (`+placeholders(len(typeIDs))+`)
		 GROUP BY typeID`,
		append([]any{stationID}, int64Args(typeIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("station book: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID int64
		var ask, bid sql.NullFloat64
		if err := rows.Scan(&typeID, &ask, &bid); err != nil {
			return nil, err
		}
		out[typeID] = localQuote{Ask: ask.Float64, Bid: bid.Float64}
	}
	return out, rows.Err()
}

// askedForInReach reports which of these types somebody is already selling somewhere the pilot would fly
// to. Reach is the pilot's own travel horizon, so a bot that ranges six regions has a bigger shopping list
// than one that stays in its constellation.
func (m *Mod) askedForInReach(ctx context.Context, typeIDs []int64, reach map[int64]int) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(typeIDs) == 0 || len(reach) == 0 {
		return out, nil
	}
	// Narrow in SQL to the regions the reach covers, then test per system in Go. The answer is shared per
	// region set for two minutes rather than queried per pilot.
	regions := reachRegions(m, ctx, reach)
	sctx, cancel := snapContext(ctx)
	defer cancel()
	asked, err := snapshotFor(snapKey("asked-in", regions), 2*time.Minute, func() (map[int64][]int64, error) {
		scope := ""
		var args []any
		if len(regions) > 0 {
			scope = ` AND o.regionID IN (` + placeholders(len(regions)) + `)`
			args = int64Args(regions)
		}
		rows, err := m.db.QueryContext(sctx, `
			SELECT DISTINCT o.typeID, o.solarSystemID
			  FROM market_orders o
			 WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0`+scope, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		by := map[int64][]int64{}
		for rows.Next() {
			var typeID, sysID int64
			if err := rows.Scan(&typeID, &sysID); err != nil {
				return nil, err
			}
			by[typeID] = append(by[typeID], sysID)
		}
		return by, rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("asks in reach: %w", err)
	}
	for _, typeID := range typeIDs {
		for _, sysID := range asked[typeID] {
			if _, ok := reach[sysID]; ok {
				out[typeID] = true
				break
			}
		}
	}
	return out, nil
}

// dropStaleSupplyBids pulls supply bids at stations this pilot no longer builds at, refunding their escrow.
// A bid is a statement about a counter, so it survives the project that prompted it and later projects at
// the same factory; only a change of factory ends it, since a hauler answering a bid at an abandoned base
// finds nobody waiting.
func (m *Mod) dropStaleSupplyBids(ctx context.Context, charID, factoryID int64) {
	if factoryID == 0 {
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT orderID, typeName FROM rolebot_orders
		 WHERE characterID = ? AND bid = 1 AND closedAt = 0
		   AND kind = ? AND stationID <> ?`,
		charID, tradeDealSupply, factoryID)
	if err != nil {
		rlogf("rolebots: stale supply bids char=%d: %v", charID, err)
		return
	}
	list := collectStaleBids(rows)
	for _, s := range list {
		if !m.cancelSupplyBid(ctx, charID, s.id) {
			continue
		}
		m.logBot(charID, logInfo, "industry",
			"pulled its standing supply bid for %s — it does not build there any more", s.name)
	}
}

// dropFilledSupplyBids pulls this factory's open supply bids for lines the current bill reads as covered
// (mined, bought, or finished by the bid's own fills): escrow on the counter for material already in the
// hangar does nothing, and the next project bids again. Only fully covered lines arrive (industryRefine
// passes needs minus short); a partly covered line keeps its bid.
func (m *Mod) dropFilledSupplyBids(ctx context.Context, b *botRun, factoryID int64, covered []int64) {
	if factoryID == 0 || len(covered) == 0 {
		return
	}
	covered = m.withOreBidsFor(ctx, b, covered)
	rows, err := m.db.QueryContext(ctx, `
		SELECT orderID, typeName FROM rolebot_orders
		 WHERE characterID = ? AND bid = 1 AND closedAt = 0
		   AND kind = ? AND stationID = ?
		   AND typeID IN (`+placeholders(len(covered))+`)`,
		append([]any{b.CharID, tradeDealSupply, factoryID}, int64Args(covered)...)...)
	if err != nil {
		rlogf("rolebots: filled supply bids char=%d: %v", b.CharID, err)
		return
	}
	list := collectStaleBids(rows)
	for _, s := range list {
		if !m.cancelSupplyBid(ctx, b.CharID, s.id) {
			continue
		}
		m.logBot(b.CharID, logInfo, "industry",
			"pulled its standing supply bid for %s — the bill is covered, it got there itself", s.name)
	}
}

// withOreBidsFor widens a covered-lines list to the ore bids left standing for those lines. Without it an
// ore bid could never be released, because the caller has mineral typeIDs and an ore bid is keyed on the
// rock. An ore is dropped only when every mineral it answers is covered, since most rock yields several.
func (m *Mod) withOreBidsFor(ctx context.Context, b *botRun, covered []int64) []int64 {
	vals, err := m.oreValues(ctx, m.valuationRegions(ctx, b), oreValueBid)
	if err != nil || len(vals) == 0 {
		return covered
	}
	coveredSet := make(map[int64]bool, len(covered))
	for _, id := range covered {
		coveredSet[id] = true
	}
	// Which minerals this bill actually wants, so a rock is not held open by a
	// mineral the project never asked for.
	wanted := map[int64]bool{}
	if needs, nerr := m.industryBillMinerals(ctx, b); nerr == nil {
		wanted = needs
	}
	out := append([]int64{}, covered...)
	for id, v := range vals {
		relevant, allCovered := false, true
		for mineralID, per := range v.PerUnit {
			if per <= 0 || (len(wanted) > 0 && !wanted[mineralID]) {
				continue
			}
			relevant = true
			if !coveredSet[mineralID] {
				allCovered = false
				break
			}
		}
		if relevant && allCovered {
			out = append(out, id)
		}
	}
	return out
}

// industryBillMinerals is the mineral half of the project's current bill, as a set. Empty (not an error)
// for a pilot with no project, which withOreBidsFor reads as no restriction.
func (m *Mod) industryBillMinerals(ctx context.Context, b *botRun) (map[int64]bool, error) {
	d := b.State.Data
	if d.IndBPTypeID == 0 {
		return nil, nil
	}
	meLevel, _ := m.blueprintLevels(ctx, d.IndBPItemID)
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return nil, err
	}
	line, err := m.manufactureLineAt(ctx, b, d.IndFactoryID, 0)
	if err != nil {
		return nil, err
	}
	needs, err := m.industryMaterialNeeds(ctx, d.IndBPTypeID, meLevel, d.IndRuns,
		skills[skillProductionEff], line.MatMul)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(needs))
	for _, n := range needs {
		out[n.TypeID] = true
	}
	return out, nil
}

type staleBid struct {
	id   int64
	name string
}

func collectStaleBids(rows *sql.Rows) []staleBid {
	defer rows.Close()
	var list []staleBid
	for rows.Next() {
		var s staleBid
		if err := rows.Scan(&s.id, &s.name); err != nil {
			break
		}
		list = append(list, s)
	}
	return list
}

// cancelSupplyBid cancels one order, reporting whether this call took it off the book. Escrow is
// refunded by the engine; the order registry row is left for the sweep (bookVanished), because
// closing it here would race the sweep's own read of the missing market row.
func (m *Mod) cancelSupplyBid(ctx context.Context, charID, orderID int64) bool {
	out, err := evedb.CancelMarketOrder(ctx, m.db, charID, orderID)
	if err != nil {
		rlogf("rolebots: cancel supply bid %d: %v", orderID, err)
		return false
	}
	// Non-empty ErrorMsg: already gone from the market. The sweep will
	// notice the missing row and book it as vanished.
	return out.ErrorMsg == ""
}
