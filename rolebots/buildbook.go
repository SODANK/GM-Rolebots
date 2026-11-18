package rolebots

// The build book decides what a factory pilot makes next.
//
// Each candidate comes from an evidence tier (shelf, leak, owned, capability, ledger, bid) that
// sets its base weight. A live ledger or bid want boosts a candidate whatever tier found it.
// Competition is a local multiplier, not a veto; the one hard stop is the pilot's own unsold
// listing at its base. A type this factory never built gets a novelty bonus.
// See docs/comment-notes-b01.md#build-book.

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"

	evedb "gurimoon/db"
)

const (
	// industryCompeteJumps is how far from the factory a rival listing counts as competition.
	// Past it a buyer would fly by this factory to reach the rival.
	industryCompeteJumps = 8

	// Evidence weights, highest first: shelf, leak, owned, capability, ledger, bid. Ledger and bid
	// rank lowest because they are boosters over any tier, so an empty ledger still leaves a full book.
	evidenceWeightShelf      = 0.85
	// evidenceWeightLeak is the NPC gear leak (npcleak.go): the roster paid an NPC for this within
	// reach of the factory. It ranks just under the shelf; leakPull scales it down for a small leak.
	evidenceWeightLeak       = 0.84
	evidenceWeightOwned      = 0.80
	evidenceWeightCapability = 0.60
	evidenceWeightLedger     = 0.55
	evidenceWeightBid        = 0.50
	// evidenceWeightGap is the floor for a retired or unknown tier name, so a stored project never scores zero.
	evidenceWeightGap = 0.35

	// The demand boosters. A live unmet ask (LedgerWanted) or standing bid money (BidWanted) multiplies
	// the score by (1 + boost) over whichever tier sourced the type. The two add.
	demandBoostLedger = 0.75
	demandBoostBid    = 0.50

	// noveltyBonus multiplies a candidate this factory has never built (no shelf row; noteShelfBuilt
	// writes one per listing). At 1.2 a novel capability line still ranks under the shelf weight.
	noveltyBonus = 1.2

	// industrialUrgencyBoost lifts Industrial-class hulls above the general
	// hull weight when fewer than industrialUrgencyThreshold are listed
	// nearby. The economy needs haulers before it needs anything else.
	industrialUrgencyBoost     = 1.5
	industrialUrgencyThreshold = 3

	// combatHullUrgencyBoost is the same lever for Destroyers and Cruisers: smaller, with a tighter
	// threshold. Battlecruisers and battleships take it only where a print is available
	// (bigHullPrintsNear, buildbook_bighull.go).
	combatHullUrgencyBoost     = 1.25
	combatHullUrgencyThreshold = 2

	// headcountRef and headcountFloor shape headcountPull. Pull at or above headcountRef keeps the whole
	// score and none keeps the floor. They are fixed so a new census does not rescale the book.
	headcountRef   = 2000.0
	headcountFloor = 0.25
	// uncoveredBoost lifts a type that nobody sells anywhere and nobody on the roster is building.
	// A cluster-wide gap is the strongest signal the book reads without a buyer asking.
	uncoveredBoost = 1.5
	// industryPricelessCapableMin is the census headcount from which a type with no sell price is still
	// built, provided its bill can be priced. The cost-plus listing then sets the price.
	industryPricelessCapableMin = 50

	// industryShelfRestockCooldownSec stops a shelf line that cannot sell from being rebuilt every pass.
	// A week of unsold stock is the market's answer.
	industryShelfRestockCooldownSec = 3 * 86400
	// industryShelfIdleDropSec prunes an auto-added shelf row neither built nor sold in a month.
	// Operator pins never expire.
	industryShelfIdleDropSec = 30 * 86400
	// industryShelfMaxRows caps the standing stock lines one pilot keeps at one station.
	industryShelfMaxRows = 24
)

// Evidence tiers, stored on the project (StateData.IndEvidence) so the log,
// the panel and the settle path all know why this thing is being made.
const (
	evidenceLedger     = "ledger"
	evidenceShelf      = "shelf"
	evidenceOwned      = "owned"
	evidenceBid        = "bid"
	evidenceCapability = "capability"
	// evidenceGap names a retired tier, kept so a stored project still labels and scores.
	evidenceGap = "gap"
	// evidenceLeak: the roster bought it off an NPC shelf within reach.
	evidenceLeak = "leak"
)

// evidenceWeight prices one tier. An unknown tier scores as the weakest, never zero, so a stored
// project is not scored out of its own book.
func evidenceWeight(ev string) float64 {
	switch ev {
	case evidenceLedger:
		return evidenceWeightLedger
	case evidenceShelf:
		return evidenceWeightShelf
	case evidenceOwned:
		return evidenceWeightOwned
	case evidenceBid:
		return evidenceWeightBid
	case evidenceCapability:
		return evidenceWeightCapability
	case evidenceLeak:
		return evidenceWeightLeak
	default:
		return evidenceWeightGap
	}
}

// evidenceLabel is the prose the log and panel use.
func evidenceLabel(ev string) string {
	switch ev {
	case evidenceLedger:
		return "somebody tried to buy it and could not"
	case evidenceShelf:
		return "its own shelf has run down"
	case evidenceOwned:
		return "it already owns the blueprint"
	case evidenceBid:
		return "somebody nearby has money out for it"
	case evidenceCapability:
		return "the roster is trained to use it"
	case evidenceLeak:
		return "pilots nearby keep buying it off the NPC shelves"
	default:
		return "the roster burns it and nothing near the factory sold it"
	}
}

// bpBuyableJoin keeps only blueprints that can be obtained: a market group, a base price and no
// meta grade on the product. Faction and tech-two prints fail it; ownedCandidates does not use it.
// See docs/comment-notes-b01.md#bp-buyable-join.
const bpBuyableJoin = `JOIN invTypes bpt ON bpt.typeID = bt.blueprintTypeID
			  AND bpt.marketGroupID > 0 AND bpt.basePrice > 0
			  AND NOT EXISTS (SELECT 1 FROM invMetaTypes mt WHERE mt.typeID = bt.productTypeID)`

// indCandidate is one thing a supplier could build, with the evidence that
// put it on the list.
type indCandidate struct {
	TypeID   int64
	TypeName string
	Wanted   int64
	BPTypeID int64

	Evidence string
	Rivals   int // live sellers within industryCompeteJumps of the factory
	Building int // other suppliers with this type in their oven right now
	Own      int // this pilot's own lines already baking it
	// The demand boosters, set after the merge whichever tier sourced the candidate. A live LedgerWanted
	// also claims the row (claimDemand) and skips the sell-above-cost gate.
	LedgerWanted int64 // live unmet ask on rolebot_market_demand
	BidWanted    int64 // standing buy-order volume within reach
	Novel        bool  // never built at this factory (no shelf row)
	// evidenceWeight × (1 + boosters) × scarcity(rivals) × scarcity(building)
	// × scarcity(own) × novelty × affordability
	Score float64
	// Afford is the affordability multiplier the candidate scored; 1 means the pilot can fund it outright.
	Afford float64
	// Capable is the census production headcount: pilots who fit, load or fly the type (wantsusers.go).
	// Pull is Capable times the category weight. Set after the merge for every tier; 0 means nobody does.
	Capable int64
	Pull    float64
	// GlobalSellers is how many stations anywhere in New Eden sell it; with
	// Building it decides the uncovered boost.
	GlobalSellers int
	// LeakISK and LeakAvg are the NPC gear leak this candidate was found on
	// (evidenceLeak only): ISK the roster paid NPCs for it within reach over
	// the window, and the average unit price it paid.
	LeakISK float64
	LeakAvg float64
	// MktPerDay and MktSupply are the market record behind the flood cut (historyFlood, mktdaily.go):
	// units the factory's region bought per day and the supply standing against it. Set only when the
	// region bought some; the batch is then sized to it (historyCapRuns).
	MktPerDay float64
	MktSupply int64
}

// headcountPull is the headcount term: it rises with the square root of the pull, from
// headcountFloor to 1 at headcountRef. Pure.
//
//	pull   16 -> 0.32     100 -> 0.42     500 -> 0.63     2000+ -> 1.00
func headcountPull(pull float64) float64 {
	if pull <= 0 {
		return headcountFloor
	}
	f := math.Sqrt(pull / headcountRef)
	if f > 1 {
		f = 1
	}
	return headcountFloor + (1-headcountFloor)*f
}

// uncovered reports whether a candidate is a cluster-wide gap: nothing listed
// anywhere and nobody building it.
func (c indCandidate) uncovered() bool {
	return c.GlobalSellers == 0 && c.Building == 0
}

// affordFloor is the least share of its pull a candidate keeps. affordability keeps all of it when
// the wallet covers the bill and scales it by wallet over cost otherwise: a slope, not a gate.
// See docs/comment-notes-b01.md#affordability.
const affordFloor = 0.02

// candidateUnitCosts prices one of each candidate at what it sells for: the best ask in the pilot's
// valuation regions, else invTypes.basePrice. A type with neither stays 0, which the priceless cut
// reads as unpriced. This is not the build cost; see candidateBuildCosts (buildcost.go).
func (m *Mod) candidateUnitCosts(ctx context.Context, b *botRun, cands []indCandidate) map[int64]float64 {
	out := make(map[int64]float64, len(cands))
	if len(cands) == 0 {
		return out
	}
	ids := make([]int64, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.TypeID)
	}
	asks, err := m.allAskByRegion(ctx)
	if err != nil {
		rlogf("rolebots: candidate unit costs: %v", err)
		return out
	}
	regions := m.valuationRegions(ctx, b)
	where, args := inClause(ids)
	rows, err := m.db.QueryContext(ctx,
		`SELECT typeID, COALESCE(basePrice, 0) FROM invTypes WHERE typeID IN `+where, args...)
	if err != nil {
		rlogf("rolebots: candidate unit costs: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var typeID int64
		var base float64
		if rows.Scan(&typeID, &base) != nil {
			continue
		}
		switch ask := scopedAsk(asks, typeID, regions); {
		case ask > 0:
			out[typeID] = ask
		case base > 0:
			out[typeID] = base
		}
	}
	return out
}

func affordability(perUnitCost, balance float64) float64 {
	if perUnitCost <= 0 || balance <= 0 {
		return 1 // nothing known about the cost, or nothing to spend: no opinion
	}
	budget := balance / industryAffordMultiple
	if budget >= perUnitCost {
		return 1
	}
	if f := budget / perUnitCost; f > affordFloor {
		return f
	}
	return affordFloor
}

// scarcity is the local-competition multiplier: one rival halves the pull, three quarters it.
// It is never zero: a contested type is a worse project, not an impossible one.
func scarcity(rivals int) float64 {
	if rivals < 0 {
		rivals = 0
	}
	return 1 / (1 + float64(rivals))
}

// industryActiveProjects counts what every other enabled supplier has a project on, from persisted
// state. A project counts from the moment it is chosen, because listings appear hours later.
// exceptCharID is left out.
func (m *Mod) industryActiveProjects(ctx context.Context, exceptCharID int64) map[int64]int {
	out := map[int64]int{}
	// Read off the roster's shared claims board (claims.go), not the table:
	// the same answer, one read per claimsTTL instead of one per pilot.
	board := m.claimsBoardNow(ctx)
	now := clock.Unix()
	forEachClaim(board, func(charID int64, c botClaim) {
		// Not filtered on activity: a supplier is free while its job bakes and may be doing something else.
		if charID != exceptCharID && !c.Halted && c.buildingNow(now) {
			out[c.IndTypeID]++
		}
	})
	// An installed job leaves the project fields for the cooking list, so the installed lines are
	// counted too. ramJobs also carries real players' production, per installer.
	for _, j := range board.jobs {
		if j.InstallerID != exceptCharID {
			out[j.TypeID] += j.N
		}
	}
	return out
}

// projectCountsAsBuilding reports whether another pilot's project counts as a supplier already on
// this type. A project uninstalled for half the stall window is waiting on something and
// industryStallSec will abandon it, so it does not count.
func projectCountsAsBuilding(d *StateData, now int64) bool {
	if d.IndTypeID == 0 {
		return false
	}
	if d.IndJobID == 0 && d.IndStartedAt > 0 && liveSince(d.IndStartedAt, now) > industryStallSec/2 {
		return false
	}
	return true
}

// cookingByType counts what this pilot already has on its own lines.
func cookingByType(jobs []IndCookingJob) map[int64]int {
	out := make(map[int64]int, len(jobs))
	for _, j := range jobs {
		out[j.TypeID]++
	}
	return out
}

// industryTopUpRuns shrinks a batch for every line this pilot already has baking the same thing:
// full batch, then a half, then a third. The first line takes the bulk run and the rest trickle,
// so the same product does not land on the market in one lump.
func industryTopUpRuns(runs, own int) int {
	if own < 1 || runs < 1 {
		return runs
	}
	if r := runs / (1 + own); r >= 1 {
		return r
	}
	return 1
}

// drawOrder is the order one pilot tries a book in, as opposed to the order it is ranked in.
// It is a softmax draw without replacement at the pilot's PickTemperature (appetite.go), so
// suppliers do not all take the same best candidate. The gate that follows is unchanged.
func drawOrder(cands []indCandidate, temperature float64) []indCandidate {
	return drawOrderN(cands, temperature, len(cands))
}

// drawOrderN is drawOrder stopped after n picks. The draw is quadratic in the book size and only
// the first picks reach the gate.
func drawOrderN(cands []indCandidate, temperature float64, n int) []indCandidate {
	if n > len(cands) {
		n = len(cands)
	}
	if len(cands) < 2 {
		return cands
	}
	if temperature < 0.01 {
		temperature = 0.01
	}
	pool := append([]indCandidate(nil), cands...)
	out := make([]indCandidate, 0, n)
	for len(pool) > 0 && len(out) < n {
		best := pool[0].Score
		for _, c := range pool[1:] {
			if c.Score > best {
				best = c.Score
			}
		}
		weights := make([]float64, len(pool))
		total := 0.0
		for i, c := range pool {
			weights[i] = math.Exp((c.Score - best) / temperature)
			total += weights[i]
		}
		pick := len(pool) - 1
		roll := rand.Float64() * total
		for i, w := range weights {
			if roll < w {
				pick = i
				break
			}
			roll -= w
		}
		out = append(out, pool[pick])
		pool = append(pool[:pick], pool[pick+1:]...)
	}
	return out
}

// ── the capability roll ─────────────────────────────────────────────────────

// capabilityCandidates is what the roster could use, most-served first (wants.go). It needs no event
// to fire, so a factory has work on an empty market. Nothing is claimed; the rival count applied
// to every tier keeps suppliers from all building the same thing.
func (m *Mod) capabilityCandidates(ctx context.Context, limit int) ([]indCandidate, error) {
	rows, err := m.capabilityLedger(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]indCandidate, 0, len(rows))
	for _, r := range rows {
		// Wanted is units, not pilots: the run count downstream is ceil(Wanted / portion) (wants.go).
		out = append(out, indCandidate{
			TypeID: r.TypeID, TypeName: r.TypeName,
			Wanted: r.Qty, BPTypeID: r.BPTypeID,
			Evidence: evidenceCapability,
		})
	}
	return out, nil
}

// tierRowCap turns a tier's limit into the row count its SQL asks for. A limit of 0 means uncapped
// and must not reach SQL as LIMIT 0, which returns no rows.
// See docs/comment-notes-b01.md#tier-row-cap.
func tierRowCap(limit int) int {
	if limit <= 0 {
		return math.MaxInt32
	}
	return limit
}

// ── tier 1: the demand ledger ───────────────────────────────────────────────

// ledgerCandidates lists claimable, blueprint-backed demand rows that are still wanted, biggest
// want first. The claim rule keeps two suppliers off one shared want.
func (m *Mod) ledgerCandidates(ctx context.Context, charID int64, limit int) ([]indCandidate, error) {
	now := clock.Unix()
	// The want as it stands now, not as last written (demandledger.go).
	w := demandWantedSQL("d", now)
	rows, err := m.db.QueryContext(ctx, `
		SELECT d.typeID, d.typeName, `+w+` AS w, bt.blueprintTypeID
		FROM rolebot_market_demand d
		JOIN invBlueprintTypes bt ON bt.productTypeID = d.typeID
		`+bpBuyableJoin+`
		WHERE `+w+` > 0
		  AND (d.claimedBy = 0 OR d.claimedBy = ? OR d.claimedAt < ?)
		  AND d.lastAt > ?
		ORDER BY w DESC, d.lastAt DESC
		LIMIT ?`,
		charID, now-industryClaimTTLSec, now-industryDemandMaxAgeSec, tierRowCap(limit))
	if err != nil {
		return nil, fmt.Errorf("industry candidates: %w", err)
	}
	defer rows.Close()
	var out []indCandidate
	for rows.Next() {
		c := indCandidate{Evidence: evidenceLedger}
		if err := rows.Scan(&c.TypeID, &c.TypeName, &c.Wanted, &c.BPTypeID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ── tier 2: this pilot's own shelf ──────────────────────────────────────────

// shelfCandidates lists this pilot's standing stock lines at its base that have run down: hangar
// stock plus open listings are under the target.
func (m *Mod) shelfCandidates(ctx context.Context, charID, stationID int64, limit int) ([]indCandidate, error) {
	if stationID == 0 {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.typeID, s.typeName, s.targetQty, bt.blueprintTypeID,
		       COALESCE((SELECT SUM(e.quantity) FROM entity e
		                  WHERE e.ownerID = s.characterID AND e.locationID = s.stationID
		                    AND e.typeID = s.typeID AND e.flag = 4), 0) AS onHand,
		       COALESCE((SELECT SUM(o.volRemaining) FROM market_orders o
		                  WHERE o.charID = s.characterID AND o.stationID = s.stationID
		                    AND o.typeID = s.typeID AND o.bid = 0 AND o.orderState = 1), 0) AS listed
		FROM rolebot_industry_shelf s
		JOIN invBlueprintTypes bt ON bt.productTypeID = s.typeID
		`+bpBuyableJoin+`
		WHERE s.characterID = ? AND s.stationID = ? AND s.targetQty > 0
		  AND s.lastBuiltAt < ?
		ORDER BY s.targetQty DESC
		LIMIT ?`,
		charID, stationID, clock.Unix()-industryShelfRestockCooldownSec, tierRowCap(limit))
	if err != nil {
		return nil, fmt.Errorf("shelf candidates: %w", err)
	}
	defer rows.Close()
	var out []indCandidate
	for rows.Next() {
		var target, onHand, listed int64
		c := indCandidate{Evidence: evidenceShelf}
		if err := rows.Scan(&c.TypeID, &c.TypeName, &target, &c.BPTypeID, &onHand, &listed); err != nil {
			return nil, err
		}
		if short := target - onHand - listed; short > 0 {
			c.Wanted = short
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// ── tier 3: live bids in reach ──────────────────────────────────────────────

// bidCandidates finds buildable types somebody within reach is bidding for. The seeder's standing
// bids are excluded. The region scope is in the SQL so the LIMIT is not spent on rows outside the
// pilot's reach; the exact per-system reach test still runs below.
func (m *Mod) bidCandidates(ctx context.Context, reach map[int64]int, regions []int64, limit int) ([]indCandidate, error) {
	scope, args := "", []any{evedb.SeederCharID, evedb.AdminSeederCharID}
	if len(regions) > 0 {
		scope = ` AND o.regionID IN (` + placeholders(len(regions)) + `)`
		args = append(args, int64Args(regions)...)
	}
	bidRows := tierRowCap(limit)
	if limit > 0 {
		bidRows = limit * 8
	}
	args = append(args, bidRows)
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.typeID, t.typeName, bt.blueprintTypeID, o.solarSystemID, SUM(o.volRemaining)
		FROM market_orders o
		JOIN invTypes t ON t.typeID = o.typeID
		JOIN invBlueprintTypes bt ON bt.productTypeID = o.typeID
		`+bpBuyableJoin+`
		WHERE o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		  AND o.charID NOT IN (?, ?)`+scope+`
		GROUP BY o.typeID, t.typeName, bt.blueprintTypeID, o.solarSystemID
		ORDER BY SUM(o.volRemaining) DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("bid candidates: %w", err)
	}
	defer rows.Close()
	byType := map[int64]*indCandidate{}
	for rows.Next() {
		var typeID, bpID, sysID, vol int64
		var name string
		if err := rows.Scan(&typeID, &name, &bpID, &sysID, &vol); err != nil {
			return nil, err
		}
		if _, ok := reach[sysID]; !ok {
			continue
		}
		if c, ok := byType[typeID]; ok {
			c.Wanted += vol
			continue
		}
		byType[typeID] = &indCandidate{TypeID: typeID, TypeName: name, Wanted: vol,
			BPTypeID: bpID, Evidence: evidenceBid}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]indCandidate, 0, len(byType))
	for _, c := range byType {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Wanted > out[j].Wanted })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ── tier: the NPC gear leak ─────────────────────────────────────────────────

// leakCandidates lists what the roster bought off NPC shelves within shopSoftMaxJumps of the
// factory (npcleak.go). Each carries its leak (LeakISK, LeakAvg); buildBook runs the margin test.
// Wanted is the weekly leak rate in units.
func (m *Mod) leakCandidates(ctx context.Context, leaks *npcLeakBook, reach map[int64]int) ([]indCandidate, error) {
	if leaks == nil || len(leaks.byType) == 0 {
		return nil, nil
	}
	found := map[int64]leakInReach{}
	ids := make([]int64, 0, len(leaks.byType))
	for typeID := range leaks.byType {
		if l := leaks.inReach(typeID, reach, shopSoftMaxJumps); l.ISK >= leakMinISK {
			found[typeID] = l
			ids = append(ids, typeID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	where, args := inClause(ids)
	rows, err := m.db.QueryContext(ctx, `
		SELECT bt.productTypeID, t.typeName, bt.blueprintTypeID
		FROM invBlueprintTypes bt
		JOIN invTypes t ON t.typeID = bt.productTypeID
		`+bpBuyableJoin+`
		WHERE bt.productTypeID IN `+where+`
		ORDER BY bt.productTypeID, bt.blueprintTypeID`, args...)
	if err != nil {
		return nil, fmt.Errorf("leak candidates: %w", err)
	}
	defer rows.Close()
	var out []indCandidate
	seen := map[int64]bool{}
	for rows.Next() {
		var typeID, bpID int64
		var name string
		if err := rows.Scan(&typeID, &name, &bpID); err != nil {
			return nil, err
		}
		if seen[typeID] {
			continue
		}
		seen[typeID] = true
		l := found[typeID]
		out = append(out, indCandidate{TypeID: typeID, TypeName: name, BPTypeID: bpID,
			Evidence: evidenceLeak, Wanted: leakWeeklyUnits(l.Units),
			LeakISK: l.ISK, LeakAvg: l.AvgPrice()})
	}
	return out, rows.Err()
}

// leakWeeklyUnits turns the window's units into a week's worth, never under
// one. Pure.
func leakWeeklyUnits(units int64) int64 {
	w := units * 7 / npcLeakWindowDays
	if w < 1 {
		w = 1
	}
	return w
}

// leakCheck is the leak tier's money test, run once the bill is priced: a unit built for buildCost
// and sold at leakUndercut times the NPC average price, net of fees, must earn something. Pure.
func leakCheck(c indCandidate, buildCost float64, r marketRates) bool {
	return leakUnitMargin(c.LeakAvg, buildCost, r) > 0
}

// resolveLeak applies leakCheck: a passing candidate is kept, a failing one is replaced by the
// type's pre-leak candidate in base, or dropped (false) when it had none. Pure.
func resolveLeak(c indCandidate, base map[int64]indCandidate, buildCost float64, r marketRates) (indCandidate, bool) {
	if leakCheck(c, buildCost, r) {
		return c, true
	}
	if prev, ok := base[c.TypeID]; ok {
		return prev, true
	}
	return c, false
}

// craftedNear is sellersNear counted over crafted asks only. The NPC shelf a leak candidate
// undercuts is neither a rival nor a flood.
func craftedNear(all map[int64][]sysStations, typeID int64, reach map[int64]int) (stations int, units int64) {
	for _, c := range all[typeID] {
		if j, ok := reach[c.SystemID]; ok && j <= industryCompeteJumps {
			stations += c.CraftedStations
			units += c.CraftedUnits
		}
	}
	return stations, units
}

// ── tier: blueprints already in the hangar ──────────────────────────────────

// ownedCandidates lists the blueprints in this pilot's hangars. A print in hand is sunk capital, so
// it skips the market trip and the blueprint bill. It does not use bpBuyableJoin: owning the print
// proves it is obtainable. The shelf restock cooldown is read at this factory.
func (m *Mod) ownedCandidates(ctx context.Context, charID, stationID int64, limit int) ([]indCandidate, error) {
	if stationID == 0 {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT bt.productTypeID, t.typeName, bt.blueprintTypeID, COALESCE(g.categoryID, 0)
		FROM entity e
		JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		JOIN invTypes t ON t.typeID = bt.productTypeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN rolebot_industry_shelf s ON s.characterID = e.ownerID
		     AND s.stationID = ? AND s.typeID = bt.productTypeID
		WHERE e.ownerID = ? AND e.flag = 4
		  AND e.locationID >= 60000000 AND e.locationID < 64000000
		  AND (s.typeID IS NULL OR s.lastBuiltAt < ?)
		GROUP BY bt.productTypeID, t.typeName, bt.blueprintTypeID, g.categoryID
		LIMIT ?`,
		stationID, charID, clock.Unix()-industryShelfRestockCooldownSec, tierRowCap(limit))
	if err != nil {
		return nil, fmt.Errorf("owned candidates: %w", err)
	}
	defer rows.Close()
	var out []indCandidate
	var cats []int64
	for rows.Next() {
		var catID int64
		c := indCandidate{Evidence: evidenceOwned}
		if err := rows.Scan(&c.TypeID, &c.TypeName, &c.BPTypeID, &catID); err != nil {
			return nil, err
		}
		out = append(out, c)
		cats = append(cats, catID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Wanted comes from the census where it knows the type, in units of users, and floors at 1.
	// Batch sizing raises charges and modules to a stocking batch (industryStartProject).
	if len(out) > 0 {
		ids := make([]int64, len(out))
		for i, c := range out {
			ids[i] = c.TypeID
		}
		users := m.productionCounts(ctx, ids)
		for i := range out {
			out[i].Wanted = users[out[i].TypeID] * capabilityUnitsPerPilot(cats[i])
			if out[i].Wanted < 1 {
				out[i].Wanted = 1
			}
		}
	}
	return out, nil
}

// ── competition ─────────────────────────────────────────────────────────────

// sellersNear counts, per type, the stations within industryCompeteJumps of the factory with a live
// sell order, the seeder included. It also returns the global count (uncovered boost) and the
// units listed in reach (productFlooded), all from one shared snapshot (allSellerStations).
func (m *Mod) sellersNear(ctx context.Context, typeIDs []int64, reach map[int64]int) (near, global map[int64]int, nearUnits map[int64]int64, err error) {
	near, global, nearUnits = map[int64]int{}, map[int64]int{}, map[int64]int64{}
	if len(typeIDs) == 0 {
		return near, global, nearUnits, nil
	}
	all, err := m.allSellerStations(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, id := range typeIDs {
		for _, c := range all[id] {
			global[id] += c.Stations
			if j, ok := reach[c.SystemID]; ok && j <= industryCompeteJumps {
				near[id] += c.Stations
				nearUnits[id] += c.Units
			}
		}
	}
	return near, global, nearUnits, nil
}

// productFlooded reports whether the units already listed cover everything wanted. A want of zero
// or less floods nothing. Pure. scarcity only discounts, so this is a cut: a census want the
// size of the roster would outweigh any discount.
func productFlooded(listedUnits, wantedUnits int64) bool {
	return wantedUnits >= 1 && listedUnits >= wantedUnits
}

// heldCountsAgainst reports whether the roster's held stock is supply against a candidate's want.
// It is only so for a census want (capability and owned tiers); a shelf or leak want is one
// factory's own and the stock is already beside it. Pure.
func heldCountsAgainst(evidence string) bool {
	return evidence == evidenceCapability || evidence == evidenceOwned
}

// clusterFlood is the flood cut off the cluster's record (mktdaily.go globalFlood) for a candidate
// whose region has no record: cluster buying against everything listed, plus held stock where
// heldCountsAgainst says so. A leak candidate never asks. decided is false with no record. Pure.
// thinHull switches it to the hull rule (historyFloodHullCover).
func clusterFlood(hist *mktHistory, c indCandidate, globalListed map[int64]int64, heads map[int64]censusHead, thinHull bool) (flooded, decided bool) {
	if hist == nil || c.Evidence == evidenceLeak {
		return false, false
	}
	listed := globalListed[c.TypeID]
	supply := listed
	if heldCountsAgainst(c.Evidence) {
		supply = floodSupply(listed, heldOf(heads, c.TypeID))
	}
	return floodCover(thinHull, hist.Ready, hist.globalPerDay(c.TypeID), listed, supply, c.Wanted, mktGlobalCoverDays)
}

// ownListingAt reports what this pilot still has resting on the market at its own factory, per
// type. It is the one hard veto: no second batch of what sits unsold.
func (m *Mod) ownListingAt(ctx context.Context, charID, stationID int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if stationID == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, SUM(volRemaining) FROM market_orders
		WHERE charID = ? AND stationID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0
		GROUP BY typeID`, charID, stationID)
	if err != nil {
		return nil, fmt.Errorf("own listings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, vol int64
		if err := rows.Scan(&typeID, &vol); err != nil {
			return nil, err
		}
		out[typeID] = vol
	}
	return out, rows.Err()
}

// ── the book ────────────────────────────────────────────────────────────────

// industryCandidates is the cheap book: the tiers that need no market scan (ledger, own shelf,
// hangar prints). The appetite probe and the skill planner (skills.go) call it far more often
// than a project starts.
func (m *Mod) industryCandidates(ctx context.Context, charID int64, limit int) ([]indCandidate, error) {
	out, err := m.ledgerCandidates(ctx, charID, limit)
	if err != nil {
		return nil, err
	}
	stationID := int64(0)
	if prof, perr := m.getProfile(ctx, charID); perr == nil && prof != nil {
		stationID = prof.IndustryStationID
	}
	shelf, err := m.shelfCandidates(ctx, charID, stationID, limit)
	if err != nil {
		return out, nil // a shelf read failure keeps the ledger rows
	}
	out = mergeCandidates(out, shelf, limit)
	owned, err := m.ownedCandidates(ctx, charID, stationID, limit)
	if err != nil {
		return out, nil // a hangar read failure keeps the rest
	}
	return mergeCandidates(out, owned, limit), nil
}

// buildBook is the full book for a pilot at a known factory: every tier, deduplicated by type
// keeping the strongest evidence, boosted by live demand, priced by local competition, best first.
func (m *Mod) buildBook(ctx context.Context, b *botRun, factoryID, factorySysID int64, limit int) ([]indCandidate, error) {
	// Scoped (residency.go): a builder is undercut by its own region's shelves, not by a distant hub.
	r := m.economicRules(ctx, b, defaultBand(b))
	reach, err := m.reachFrom(ctx, factorySysID, r)
	if err != nil {
		return nil, err
	}

	ledger, err := m.ledgerCandidates(ctx, b.CharID, limit)
	if err != nil {
		return nil, err
	}
	shelf, err := m.shelfCandidates(ctx, b.CharID, factoryID, limit)
	if err != nil {
		return nil, err
	}
	owned, err := m.ownedCandidates(ctx, b.CharID, factoryID, limit)
	if err != nil {
		return nil, err
	}
	bids, err := m.bidCandidates(ctx, reach, regionIDs(r.Regions), limit)
	if err != nil {
		return nil, err
	}
	// The whole census, since headcount is a score term (headcountPull) and the tail is reached by weight.
	capable, err := m.capabilityCandidates(ctx, 0)
	if err != nil {
		return nil, err
	}

	base := mergeCandidates(
		mergeCandidates(mergeCandidates(mergeCandidates(ledger, shelf, 0), owned, 0), bids, 0),
		capable, 0)
	// The NPC gear leak (npcleak.go), merged last so base keeps each type's pre-leak candidate for the
	// fallback when the margin test fails. An unreadable ledger adds no tier.
	var leak []indCandidate
	if leaks, lerr := m.npcLeaks(ctx); lerr != nil {
		rlogf("rolebots: npc leak ledger: %v", lerr)
	} else if leak, err = m.leakCandidates(ctx, leaks, reach); err != nil {
		return nil, err
	}
	baseByType := make(map[int64]indCandidate, len(base))
	for _, c := range base {
		baseByType[c.TypeID] = c
	}
	merged := mergeCandidates(base, leak, 0)

	// Competition is measured once, over everything on the list.
	ids := make([]int64, 0, len(merged))
	for _, c := range merged {
		ids = append(ids, c.TypeID)
	}
	rivals, globalSellers, listedNear, err := m.sellersNear(ctx, ids, reach)
	if err != nil {
		return nil, err
	}
	// The factory region's buying and listings per type: where there is a record it decides the flood cut.
	hist := m.marketHistory(ctx)
	histRegion := m.stationRegionOf(ctx, factoryID)
	var regionListed, globalListed map[int64]int64
	if hist.Ready && histRegion != 0 {
		regionListed = m.regionListedUnits(ctx, histRegion, ids)
	}
	// And the cluster's, for a type the region has no record of (globalFlood).
	if hist.Ready {
		globalListed = m.globalListedUnits(ctx, ids)
	}

	// The one hard veto: what this factory still has listed itself.
	own, err := m.ownListingAt(ctx, b.CharID, factoryID)
	if err != nil {
		return nil, err
	}
	// Other suppliers' projects count by type, not place: they all reach the market together
	// whatever their factories, so a rival project is a rival.
	building := m.industryActiveProjects(ctx, b.CharID)
	// This pilot's own lines are not a veto; see industryTopUpRuns.
	mine := cookingByType(b.State.Data.IndCooking)

	// The demand boosters ride on whichever tier sourced the type, so they are annotated back after the merge.
	ledgerWant := make(map[int64]int64, len(ledger))
	for _, c := range ledger {
		ledgerWant[c.TypeID] = c.Wanted
	}
	bidWant := make(map[int64]int64, len(bids))
	for _, c := range bids {
		bidWant[c.TypeID] = c.Wanted
	}
	// Novelty: a type with a shelf row here has been built here before (noteShelfBuilt writes one per listing).
	built, err := m.builtHere(ctx, b.CharID, factoryID)
	if err != nil {
		return nil, err
	}

	// unitCost is what one sells for (priceless cut); buildCost is what one costs to make
	// (buildcost.go) and feeds affordability, which falls back to unitCost when the bill is unpriced.
	balance, berr := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if berr != nil {
		balance = 0 // unknown wallet: affordability abstains
	}
	unitCost := m.candidateUnitCosts(ctx, b, merged)
	heads := m.capabilityHeads(ctx)
	buildCost := m.candidateBuildCosts(ctx, b, merged)
	industrials := m.hullsInGroups(ctx, merged, industrialGroupIDs)
	combatHulls := m.hullsInGroups(ctx, merged, combatHullGroupIDs)
	// A battlecruiser or battleship whose print this pilot can get: an escrow copy in reach, or its own.
	var bigHulls map[int64]bool
	if bigHullBookOn(ctx, m.db) {
		bigHulls = m.bigHullPrintsNear(ctx, merged, m.hullsInGroups(ctx, merged, bigHullGroupIDs), reach)
	}

	// Hulls above a frigate are thin markets: one unsold unit in a region that has never bought one is not a glut (hullgates.go).
	var thinHulls map[int64]bool
	if hullGateOn(ctx, m, hullGateThinFloodKey) {
		thinHulls = m.hullsInGroups(ctx, merged, hullLadderGroupIDs)
	}

	out := merged[:0]
	priceless, foreign, flooded, leakLoss, unused := 0, 0, 0, 0, 0
	covered, refused := 0, 0 // the history's two flood cuts
	clusterCut := 0          // the cluster-record flood cut
	who := pilotRace{Race: raceOf(b.CharID), Phobia: b.Tune.Xenophobia}
	if who.Race == 0 {
		who.Race = m.charRace(ctx, b.CharID)
	}
	// The leak tier's market reads: listing fees at this factory (margin test) and the crafted-only shelf.
	rates := m.marketRates(ctx, b.CharID, factoryID)
	var allSell map[int64][]sysStations
	if len(leak) > 0 {
		if allSell, err = m.allSellerStations(ctx); err != nil {
			return nil, err
		}
	}
	for _, c := range merged {
		if own[c.TypeID] > 0 {
			continue
		}
		// The leak tier's profit test: a unit built at the bill and sold at 0.95 times the NPC price, net of
		// fees, must earn. A loser falls back to its pre-leak evidence.
		if c.Evidence == evidenceLeak {
			var keep bool
			if c, keep = resolveLeak(c, baseByType, buildCost[c.TypeID], rates); !keep {
				leakLoss++
				continue
			}
		}
		if h, ok := heads[c.TypeID]; ok {
			c.Capable = h.Users
			c.Pull = float64(h.Users) * capabilityWeight(h.CategoryID)
		}
		// Nobody on the roster fits, loads or flies it (wantsusers.go) and nobody asked for it: that is dead
		// stock. A live ledger want, a bid or an NPC leak is a real buyer and stays.
		if censusUnused(heads, c.TypeID) && ledgerWant[c.TypeID] == 0 &&
			bidWant[c.TypeID] == 0 && c.Evidence != evidenceLeak {
			unused++
			continue
		}
		// The priceless cut drops a type nothing prices. See docs/comment-notes-b01.md#priceless-cut.
		if unitCost[c.TypeID] <= 0 && ledgerWant[c.TypeID] == 0 &&
			!(c.Capable >= industryPricelessCapableMin && buildCost[c.TypeID] > 0) {
			priceless++
			continue
		}
		c.Rivals = rivals[c.TypeID]
		c.GlobalSellers = globalSellers[c.TypeID]
		c.Building = building[c.TypeID]
		c.Own = mine[c.TypeID]
		c.LedgerWanted = ledgerWant[c.TypeID]
		c.BidWanted = bidWant[c.TypeID]
		listed := listedNear[c.TypeID]
		if c.Evidence == evidenceLeak {
			// An NPC shelf is what a leak candidate undercuts; only crafted asks compete and only crafted units flood.
			c.Rivals, listed = craftedNear(allSell, c.TypeID, reach)
		}
		// The flood cut: shelves in reach already hold more than the roster could use. A ledger or bid want
		// is a real buyer and stays. A census want also counts the roster's unsold stock (wantsusers.go rateHeld).
		supply := listed
		if heldCountsAgainst(c.Evidence) {
			supply = floodSupply(listed, heldOf(heads, c.TypeID))
		}
		if c.LedgerWanted == 0 && c.BidWanted == 0 {
			// The region's record first (mktdaily.go historyFlood): supply against what it buys. A leak
			// candidate's offer is the crafted shelf only.
			offered := regionListed[c.TypeID]
			if c.Evidence == evidenceLeak {
				offered = listed
			}
			perDay := hist.regionPerDay(histRegion, c.TypeID)
			thin := thinHulls[c.TypeID]
			if fl, decided := floodCover(thin, hist.Ready && histRegion != 0, perDay, offered, supply, c.Wanted,
				mktBuildCoverDays); decided {
				if fl && perDay > 0 {
					covered++
					continue
				}
				if fl {
					refused++
					continue
				}
				c.MktPerDay, c.MktSupply = perDay, supply
			} else if gfl, gdecided := clusterFlood(hist, c, globalListed, heads, thin); gdecided {
				// No region record: the cluster's decides. Batch sizing stays regional, so MktPerDay is left unset.
				if gfl {
					clusterCut++
					continue
				}
			} else if productFlooded(supply, c.Wanted) {
				flooded++
				continue
			}
		}
		c.Novel = !built[c.TypeID]
		boost := 1.0
		if c.LedgerWanted > 0 {
			boost += demandBoostLedger
		}
		if c.BidWanted > 0 {
			boost += demandBoostBid
		}
		costToMake := buildCost[c.TypeID]
		if costToMake <= 0 {
			costToMake = unitCost[c.TypeID]
		}
		c.Afford = affordability(costToMake, balance)
		// The maker's race feeling (tolerance.go xenoBuildMul): a xenophobe builds its own people's ships
		// and, past the first stage, no foreign race's. Raceless products are untouched.
		raceMul, raceOK := xenoBuildMul(who, m.hullRace(ctx, c.TypeID))
		if !raceOK {
			foreign++
			continue
		}
		c.Score = evidenceWeight(c.Evidence) * boost * scarcity(c.Rivals) *
			scarcity(c.Building) * scarcity(c.Own) * c.Afford * raceMul
		if c.Novel {
			c.Score *= noveltyBonus
		}
		// Headcount orders the book; the floor keeps the tail in reach.
		c.Score *= headcountPull(c.Pull)
		if c.Evidence == evidenceLeak {
			c.Score *= leakPull(c.LeakISK)
		}
		if c.uncovered() {
			c.Score *= uncoveredBoost
		}
		// Industrial hulls haul for the economy, so they are boosted while the local market is undersupplied.
		// Destroyers and Cruisers take the same lever one rung down.
		switch {
		case industrials[c.TypeID] && c.Rivals < industrialUrgencyThreshold:
			c.Score *= industrialUrgencyBoost
		case combatHulls[c.TypeID] && c.Rivals < combatHullUrgencyThreshold:
			c.Score *= combatHullUrgencyBoost
		case bigHulls[c.TypeID] && c.Rivals < combatHullUrgencyThreshold:
			c.Score *= combatHullUrgencyBoost
		}
		out = append(out, c)
	}
	// Log when it bites hard: the census offered plenty and the market priced almost none of it.
	if priceless > 0 && priceless >= len(out) {
		m.logBot(b.CharID, logInfo, "industry",
			"%d of %d candidate(s) have no market price at all and were left out of the book — "+
				"nothing sells them and nothing has ever bid on them",
			priceless, priceless+len(out))
	}
	if unused > 0 && len(out) == 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"%d candidate(s) were left out of the book — nobody on the roster fits, loads or flies them "+
				"and nobody has asked for one", unused)
	}
	if flooded > 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"%d candidate(s) were left out of the book — the shelves within %d jumps already hold more of them "+
				"than the whole roster could use", flooded, industryCompeteJumps)
	}
	if covered > 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"%d candidate(s) were left out of the book — the supply already holds %.0f days of what the region buys",
			covered, mktBuildCoverDays)
	}
	if refused > 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"%d candidate(s) were left out of the book — listed in the region and nobody has bought one in %d days",
			refused, mktRegionWindowDays)
	}
	if clusterCut > 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"%d candidate(s) were left out of the book — the region has no record of them, and the cluster "+
				"already holds %.0f days of what it buys or bought none in %d days",
			clusterCut, mktGlobalCoverDays, mktRegionWindowDays)
	}
	if leakLoss > 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"%d type(s) the roster buys off NPC shelves nearby were left out of the book — "+
				"built here they would not sell under the NPC at a profit", leakLoss)
	}
	if foreign > 0 && len(out) == 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"%d candidate(s) were a foreign people's make, and this pilot builds only its own race's and its allies'",
			foreign)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Wanted > out[j].Wanted
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// builtHere is the novelty test's memory: every type with a shelf row at this station. Absence means
// never made here, until pruneShelf drops a line idle for a month.
func (m *Mod) builtHere(ctx context.Context, charID, stationID int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if stationID == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID FROM rolebot_industry_shelf
		WHERE characterID = ? AND stationID = ?`, charID, stationID)
	if err != nil {
		return nil, fmt.Errorf("built-here: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID int64
		if err := rows.Scan(&typeID); err != nil {
			return nil, err
		}
		out[typeID] = true
	}
	return out, rows.Err()
}

// mergeCandidates folds b into a, keeping the strongest evidence per type and, at equal evidence,
// the larger want. limit 0 means no cap.
func mergeCandidates(a, b []indCandidate, limit int) []indCandidate {
	seen := make(map[int64]int, len(a))
	out := make([]indCandidate, 0, len(a)+len(b))
	for _, c := range a {
		if i, ok := seen[c.TypeID]; ok {
			if betterEvidence(c, out[i]) {
				out[i] = c
			}
			continue
		}
		seen[c.TypeID] = len(out)
		out = append(out, c)
	}
	for _, c := range b {
		if i, ok := seen[c.TypeID]; ok {
			if betterEvidence(c, out[i]) {
				out[i] = c
			}
			continue
		}
		seen[c.TypeID] = len(out)
		out = append(out, c)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// The hull classes the book pushes, by invGroups.groupID.
var (
	// Industrial and Transport Ship: what hauls.
	industrialGroupIDs = []int64{28, 380}
	// Destroyer and Cruiser: the rung above frigate. Battlecruiser (419) and Battleship (27) are
	// absent; combatHullUrgencyBoost covers them.
	combatHullGroupIDs = []int64{420, 26}
	// Every hull the module will promise a mining campaign for, Frigate (25) included. A hull needs many
	// mineral lines, so the scarcest decides (industryFeedableRuns). Big hulls join through
	// campaignHullGroups while rolebots.hullBook.bigHulls is on (buildbook_bighull.go).
	campaignHullGroupIDs = []int64{28, 380, 420, 26, 25}
	// Every hull above a frigate, for the owned-print pin (pickOwnedHullPin). Big hulls are here because
	// the pin only fires for a pilot already holding the print.
	hullLadderGroupIDs = []int64{28, 380, 420, 26, 419, 27}
	// The ship groups that are not a shipyard hull: Capsule (29), Shuttle (31) and Rookie ship (237).
	notRealHullGroupIDs = map[int64]bool{29: true, 31: true, 237: true}
)

// isRealHull reports whether a type of this category and group is a ship a shipyard should build:
// anything in the ship category but a capsule, a shuttle or a rookie ship. Pure.
func isRealHull(categoryID, groupID int64) bool {
	return categoryID == catShip && !notRealHullGroupIDs[groupID]
}

// hullsInGroups identifies which candidates sit in the given invGroups, in one query.
func (m *Mod) hullsInGroups(ctx context.Context, cands []indCandidate, groups []int64) map[int64]bool {
	out := map[int64]bool{}
	if len(cands) == 0 || len(groups) == 0 {
		return out
	}
	ids := make([]int64, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.TypeID)
	}
	where, args := inClause(ids)
	args = append(args, int64Args(groups)...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeID IN `+where+` AND g.groupID IN (`+placeholders(len(groups))+`)`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var typeID int64
		if rows.Scan(&typeID) == nil {
			out[typeID] = true
		}
	}
	return out
}

func betterEvidence(a, b indCandidate) bool {
	wa, wb := evidenceWeight(a.Evidence), evidenceWeight(b.Evidence)
	if wa != wb {
		return wa > wb
	}
	return a.Wanted > b.Wanted
}

// ── the shelf ───────────────────────────────────────────────────────────────

// shelfRow is one standing stock line, for the panel.
type shelfRow struct {
	CharacterID int64
	StationID   int64
	TypeID      int64
	TypeName    string
	TargetQty   int64
	Source      string
	BuiltQty    int64
	SoldQty     int64
	LastBuiltAt int64
	AddedAt     int64
	OnHand      int64
	Listed      int64
}

// Short returns how far under target this line is sitting right now.
func (r shelfRow) Short() int64 {
	if s := r.TargetQty - r.OnHand - r.Listed; s > 0 {
		return s
	}
	return 0
}

// noteShelfBuilt records a finished batch against the shelf, creating the line on the first make
// here. A new line's target is that batch; an existing line keeps its target, an operator's included.
func (m *Mod) noteShelfBuilt(ctx context.Context, charID, stationID, typeID int64, name, source string, qty int64) {
	if charID == 0 || stationID == 0 || typeID == 0 || qty <= 0 {
		return
	}
	if source == "" {
		source = evidenceGap
	}
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_industry_shelf
			(characterID, stationID, typeID, typeName, targetQty, source, builtQty, lastBuiltAt, addedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			typeName    = VALUES(typeName),
			targetQty   = GREATEST(targetQty, VALUES(targetQty)),
			builtQty    = builtQty + VALUES(builtQty),
			lastBuiltAt = VALUES(lastBuiltAt)`,
		charID, stationID, typeID, name, qty, source, qty, now, now); err != nil {
		m.logBot(charID, logError, "industry", "shelf line for %s: %v", name, err)
		return
	}
	m.pruneShelf(ctx, charID, stationID)
}

// noteShelfSold credits a sale against the line, so a fast seller is not pruned as idle.
func (m *Mod) noteShelfSold(ctx context.Context, charID, stationID, typeID, qty int64) {
	if qty <= 0 {
		return
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_industry_shelf SET soldQty = soldQty + ?
		WHERE characterID = ? AND stationID = ? AND typeID = ?`,
		qty, charID, stationID, typeID)
}

// pruneShelf drops auto-added lines that have gone quiet and trims the shelf to industryShelfMaxRows,
// worst first. Operator pins survive both.
func (m *Mod) pruneShelf(ctx context.Context, charID, stationID int64) {
	_, _ = m.db.ExecContext(ctx, `
		DELETE FROM rolebot_industry_shelf
		WHERE characterID = ? AND stationID = ? AND source <> 'operator'
		  AND soldQty = 0 AND lastBuiltAt < ?`,
		charID, stationID, clock.Unix()-industryShelfIdleDropSec)
	_, _ = m.db.ExecContext(ctx, `
		DELETE FROM rolebot_industry_shelf
		WHERE characterID = ? AND stationID = ? AND source <> 'operator'
		  AND typeID NOT IN (
			SELECT typeID FROM (
				SELECT typeID FROM rolebot_industry_shelf
				WHERE characterID = ? AND stationID = ?
				ORDER BY soldQty DESC, lastBuiltAt DESC
				LIMIT ?) keep)`,
		charID, stationID, charID, stationID, industryShelfMaxRows)
}

// shelfFor reads a pilot's whole shelf at one station with live stock and listing counts per line.
// Only the panel calls it, so the two correlated subqueries are affordable.
func (m *Mod) shelfFor(ctx context.Context, charID, stationID int64) ([]shelfRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID, s.stationID, s.typeID, s.typeName, s.targetQty, s.source,
		       s.builtQty, s.soldQty, s.lastBuiltAt, s.addedAt,
		       COALESCE((SELECT SUM(e.quantity) FROM entity e
		                  WHERE e.ownerID = s.characterID AND e.locationID = s.stationID
		                    AND e.typeID = s.typeID AND e.flag = 4), 0),
		       COALESCE((SELECT SUM(o.volRemaining) FROM market_orders o
		                  WHERE o.charID = s.characterID AND o.stationID = s.stationID
		                    AND o.typeID = s.typeID AND o.bid = 0 AND o.orderState = 1), 0)
		FROM rolebot_industry_shelf s
		WHERE s.characterID = ? AND (? = 0 OR s.stationID = ?)
		ORDER BY s.soldQty DESC, s.typeName`, charID, stationID, stationID)
	if err != nil {
		return nil, fmt.Errorf("shelf: %w", err)
	}
	defer rows.Close()
	var out []shelfRow
	for rows.Next() {
		var r shelfRow
		if err := rows.Scan(&r.CharacterID, &r.StationID, &r.TypeID, &r.TypeName, &r.TargetQty,
			&r.Source, &r.BuiltQty, &r.SoldQty, &r.LastBuiltAt, &r.AddedAt, &r.OnHand, &r.Listed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// setShelfTarget writes an operator's stock line. A target of 0 removes the line; any other value
// pins it against the pruner.
func (m *Mod) setShelfTarget(ctx context.Context, charID, stationID, typeID int64, name string, target int64) error {
	if target <= 0 {
		_, err := m.db.ExecContext(ctx, `
			DELETE FROM rolebot_industry_shelf
			WHERE characterID = ? AND stationID = ? AND typeID = ?`, charID, stationID, typeID)
		return err
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_industry_shelf
			(characterID, stationID, typeID, typeName, targetQty, source, addedAt)
		VALUES (?, ?, ?, ?, ?, 'operator', ?)
		ON DUPLICATE KEY UPDATE
			typeName  = VALUES(typeName),
			targetQty = VALUES(targetQty),
			source    = 'operator'`,
		charID, stationID, typeID, name, target, clock.Unix())
	return err
}

// bookSummary is the one-line log of what the book looked like this pass.
func bookSummary(cands []indCandidate) string {
	if len(cands) == 0 {
		return "nothing"
	}
	parts := make([]string, 0, 4)
	for i, c := range cands {
		if i == 4 {
			parts = append(parts, fmt.Sprintf("+%d more", len(cands)-4))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %d rival(s), %d building, %d of its own, %.2f)",
			c.TypeName, c.Evidence, c.Rivals, c.Building, c.Own, c.Score))
	}
	return strings.Join(parts, ", ")
}
