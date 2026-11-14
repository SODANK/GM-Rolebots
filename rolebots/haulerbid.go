package rolebots

// A trader with money to burn bids for a hauler.
//
// The fit selector only offers hulls somebody sells, and nothing lists an
// industrial. So a trader rich enough to replace the hull several times over, with
// no industrial and none on sale in reach, leaves one standing bid for the biggest
// industrial it can fly, at its bill of materials plus a builder's margin. The
// build book's bid tier (buildbook.go bidCandidates) shows it to industrialists; a
// finished hull listed in the region at or under the bid fills it and the core
// delivers it to the trader's hangar; procure.findhull then boards it
// (ownedHullElsewhere). The bid is cancelled once the pilot owns an industrial.

import (
	"context"
	"sync"

	evedb "gurimoon/db"
)

const (
	// haulerBidWalletMultiple is how many times over the bid's price the pilot must be
	// able to spend before it ties up the escrow, the same bar an upgrade clears
	// (upgradeAffordMultiple).
	haulerBidWalletMultiple = upgradeAffordMultiple
	// haulerBidMarkup is the builder's margin on top of the bill: enough that
	// an industrialist pricing cost-plus sells into it rather than past it.
	haulerBidMarkup = 1.4
	// haulerBidDays is the order's life; an unfilled bid expires and the core
	// refunds the escrow.
	haulerBidDays = 14
	// haulerBidRange is region range: a hull finished anywhere in the region
	// fills it and is delivered at the bid's station.
	haulerBidRange = 32767
	// haulerBidCheckSec throttles the whole check per pilot. tradePlan runs on
	// every trade pass and the answer changes on the scale of hours.
	haulerBidCheckSec = 30 * 60
)

var (
	haulerBidMu      sync.Mutex
	haulerBidChecked = map[int64]int64{}
	// cruiserBidChecked is the mining cruiser bid's own throttle
	// (maybeBidForMiningCruiser); the two checks are due on their own clocks.
	cruiserBidChecked = map[int64]int64{}
)

// cruiserBidDue is haulerBidDue for the mining cruiser bid.
func cruiserBidDue(charID, now int64) bool {
	haulerBidMu.Lock()
	defer haulerBidMu.Unlock()
	if at, ok := cruiserBidChecked[charID]; ok && now-at < haulerBidCheckSec {
		return false
	}
	cruiserBidChecked[charID] = now
	return true
}

// haulerBidDue reports whether this pilot's check is due, stamping it.
func haulerBidDue(charID, now int64) bool {
	haulerBidMu.Lock()
	defer haulerBidMu.Unlock()
	if at, ok := haulerBidChecked[charID]; ok && now-at < haulerBidCheckSec {
		return false
	}
	haulerBidChecked[charID] = now
	return true
}

// haulerOption is one industrial a pilot could bid for.
type haulerOption struct {
	TypeID, BPTypeID, RaceID int64
	Name                     string
	CargoM3                  float64
	BuildCost                float64
}

// pickHaulerOption is the choice, pure: the biggest hold, scaled by the pilot's
// race term (fitGenRaceMul), so its own empire's hull is preferred and a
// xenophobe's refused races sink to the bottom. Options without a priced bill are
// not biddable.
func pickHaulerOption(opts []haulerOption, who pilotRace) *haulerOption {
	var best *haulerOption
	bestScore := 0.0
	for i := range opts {
		o := &opts[i]
		if o.BuildCost <= 0 || o.CargoM3 <= 0 {
			continue
		}
		score := o.CargoM3 * fitGenRaceMul(who, o.RaceID)
		if score > bestScore {
			best, bestScore = o, score
		}
	}
	return best
}

// haulerBidPrice is what the pilot offers, and whether its wallet clears the
// bar for offering it.
func haulerBidPrice(buildCost, balance float64) (float64, bool) {
	price := buildCost * haulerBidMarkup
	return price, price > 0 && spendable(balance) >= price*haulerBidWalletMultiple
}

// maybeBidForHauler is the planner's hook. Station action, best effort: a
// failure is logged and never fails the trade turn.
func (m *Mod) maybeBidForHauler(ctx context.Context, b *botRun) {
	if b.StationID == 0 || !haulerBidDue(b.CharID, clock.Unix()) {
		return
	}
	industrial := map[int64]bool{}
	for _, g := range industrialGroupIDs {
		industrial[g] = true
	}
	// Owns one already (or one has arrived off the bid): nothing to want, and
	// any bid still resting is escrow for a second hull nobody needs.
	owned, err := m.ownedHullTypes(ctx, b.CharID)
	if err != nil {
		return
	}
	ownsHauler := false
	if len(owned) > 0 {
		ids := make([]int64, 0, len(owned))
		for id := range owned {
			ids = append(ids, id)
		}
		var n int64
		if m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM invTypes WHERE typeID IN (`+placeholders(len(ids))+`)
			AND groupID IN (`+placeholders(len(industrialGroupIDs))+`)`,
			append(int64Args(ids), int64Args(industrialGroupIDs)...)...).Scan(&n) == nil && n > 0 {
			ownsHauler = true
		}
	}
	openBid, openType := m.openHaulerBid(ctx, b.CharID)
	if ownsHauler {
		if openBid != 0 {
			if out, cerr := evedb.CancelMarketOrder(ctx, m.db, b.CharID, openBid); cerr == nil && out != nil {
				m.logBot(b.CharID, logInfo, "trade",
					"has an industrial now — cancelled its standing bid for another (type %d)", openType)
			}
		}
		return
	}
	if openBid != 0 {
		return // one bid at a time; it is already waiting
	}
	// On sale in reach: the upgrade path buys it outright, no bid needed.
	if sold, serr := m.hullsSoldWithin(ctx, m.chargeShopScope(ctx, b.CharID)); serr == nil {
		for id := range sold {
			if industrial[m.typeGroupOf(ctx, id)] {
				return
			}
		}
	}

	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return
	}
	opts, err := m.haulerOptions(ctx, b)
	if err != nil || len(opts) == 0 {
		return
	}
	pick := pickHaulerOption(opts, m.charPilotRace(ctx, b.CharID))
	if pick == nil {
		return
	}
	price, ok := haulerBidPrice(pick.BuildCost, balance)
	if !ok {
		return // not money to burn: the frigate hold will do for now
	}
	out, err := evedb.PlaceBuyOrder(ctx, m.db, b.CharID, b.StationID, pick.TypeID,
		price, 1, haulerBidRange, 1, haulerBidDays, false)
	if err != nil {
		rlogf("rolebots: hauler bid char=%d type=%d: %v", b.CharID, pick.TypeID, err)
		return
	}
	if out.ErrorMsg != "" {
		m.logBot(b.CharID, logWarn, "trade", "hauler bid refused (%s) — %s at %.0f", out.ErrorMsg, pick.Name, price)
		return
	}
	if out.FilledQty > 0 {
		m.logBot(b.CharID, logInfo, "trade",
			"bought a %s outright at its hauler bid — it is in the hangar at %s", pick.Name, m.stationLabel(ctx, b.StationID))
		return
	}
	m.bookBrokerFee(ctx, b, out.RestingOrderID, pick.TypeID, pick.Name, price, 1)
	m.logBot(b.CharID, logInfo, "trade",
		"bidding %.0f ISK for a %s (%.0f m3 hold, ~%.0f ISK to build) at %s — money to burn and a frigate's hold; "+
			"nobody in reach sells an industrial, so it is asking the builders for one",
		price, pick.Name, pick.CargoM3, pick.BuildCost, m.stationLabel(ctx, b.StationID))
}

// maybeBidForMiningCruiser is the same bid for a miner: a mining-career pilot
// that can fly its empire's mining cruiser (skills_minecruiser.go), owns none,
// finds none for sale in reach and has money to burn leaves a standing bid at its
// bill plus the builder's margin. The upgrade arm boards the filled hull.
func (m *Mod) maybeBidForMiningCruiser(ctx context.Context, b *botRun) {
	if b.StationID == 0 || !careerSeat(b.Tune, ActMining) || !cruiserBidDue(b.CharID, clock.Unix()) {
		return
	}
	hull, ok := miningCruiserByRace[raceOf(b.CharID)]
	if !ok {
		return
	}
	owned, err := m.ownedHullTypes(ctx, b.CharID)
	if err != nil {
		return
	}
	ownsCruiser := false
	for id := range owned {
		if grp := m.typeGroupOf(ctx, id); grp == groupCruiser || grp == groupMiningBarge || grp == groupExhumer {
			ownsCruiser = true
			break
		}
	}
	openBid, _ := m.openBidFor(ctx, b.CharID, hull.TypeID)
	if ownsCruiser {
		if openBid != 0 {
			if out, cerr := evedb.CancelMarketOrder(ctx, m.db, b.CharID, openBid); cerr == nil && out != nil {
				m.logBot(b.CharID, logInfo, "trade", "flies a cruiser now — cancelled its standing bid for a %s", hull.Name)
			}
		}
		return
	}
	if openBid != 0 {
		return
	}
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return
	}
	if ok, uerr := m.pilotCanUse(ctx, hull.TypeID, skills); uerr != nil || !ok {
		return // the road (skills_minecruiser.go) is still being walked
	}
	if sold, serr := m.hullsSoldWithin(ctx, m.chargeShopScope(ctx, b.CharID)); serr == nil && sold[hull.TypeID] {
		return // on sale in reach: the upgrade arm buys it outright
	}
	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return
	}
	var bpTypeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT blueprintTypeID FROM invBlueprintTypes WHERE productTypeID = ?`, hull.TypeID).Scan(&bpTypeID); err != nil {
		return
	}
	cost := m.candidateBuildCosts(ctx, b, []indCandidate{{TypeID: hull.TypeID, TypeName: hull.Name, BPTypeID: bpTypeID}})[hull.TypeID]
	price, ok := haulerBidPrice(cost, balance)
	if !ok {
		return
	}
	out, err := evedb.PlaceBuyOrder(ctx, m.db, b.CharID, b.StationID, hull.TypeID,
		price, 1, haulerBidRange, 1, haulerBidDays, false)
	if err != nil {
		rlogf("rolebots: mining cruiser bid char=%d type=%d: %v", b.CharID, hull.TypeID, err)
		return
	}
	if out.ErrorMsg != "" {
		m.logBot(b.CharID, logWarn, "trade", "mining cruiser bid refused (%s) — %s at %.0f", out.ErrorMsg, hull.Name, price)
		return
	}
	if out.FilledQty > 0 {
		m.logBot(b.CharID, logInfo, "trade",
			"bought a %s outright at its bid — it is in the hangar at %s", hull.Name, m.stationLabel(ctx, b.StationID))
		return
	}
	m.bookBrokerFee(ctx, b, out.RestingOrderID, hull.TypeID, hull.Name, price, 1)
	m.logBot(b.CharID, logInfo, "trade",
		"bidding %.0f ISK for a %s (~%.0f ISK to build) at %s — it can fly one, nobody in reach sells one, "+
			"so it is asking the builders for one",
		price, hull.Name, cost, m.stationLabel(ctx, b.StationID))
}

// openBidFor is the pilot's resting bid for one type, if any.
func (m *Mod) openBidFor(ctx context.Context, charID, typeID int64) (orderID int64, price float64) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT orderID, price FROM market_orders
		 WHERE charID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0
		 LIMIT 1`, charID, typeID).Scan(&orderID, &price)
	return orderID, price
}

// openHaulerBid is the pilot's resting bid for an industrial hull, if any.
func (m *Mod) openHaulerBid(ctx context.Context, charID int64) (orderID, typeID int64) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT o.orderID, o.typeID FROM market_orders o
		  JOIN invTypes t ON t.typeID = o.typeID
		 WHERE o.charID = ? AND o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		   AND t.groupID IN (`+placeholders(len(industrialGroupIDs))+`)
		 LIMIT 1`, append([]any{charID}, int64Args(industrialGroupIDs)...)...).Scan(&orderID, &typeID)
	return orderID, typeID
}

// haulerOptions is every published industrial this pilot can board, with its
// hold and its bill priced where the pilot trades.
func (m *Mod) haulerOptions(ctx context.Context, b *botRun) ([]haulerOption, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, COALESCE(t.raceID, 0), COALESCE(t.capacity, 0), bt.blueprintTypeID
		  FROM invTypes t
		  JOIN invBlueprintTypes bt ON bt.productTypeID = t.typeID
		 WHERE t.published = 1 AND t.groupID IN (`+placeholders(len(industrialGroupIDs))+`)`,
		int64Args(industrialGroupIDs)...)
	if err != nil {
		return nil, err
	}
	var opts []haulerOption
	for rows.Next() {
		var o haulerOption
		if err := rows.Scan(&o.TypeID, &o.Name, &o.RaceID, &o.CargoM3, &o.BPTypeID); err != nil {
			rows.Close()
			return nil, err
		}
		opts = append(opts, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return nil, err
	}
	boardable := opts[:0]
	cands := make([]indCandidate, 0, len(opts))
	for _, o := range opts {
		if ok, uerr := m.pilotCanUse(ctx, o.TypeID, skills); uerr != nil || !ok {
			continue
		}
		boardable = append(boardable, o)
		cands = append(cands, indCandidate{TypeID: o.TypeID, TypeName: o.Name, BPTypeID: o.BPTypeID})
	}
	costs := m.candidateBuildCosts(ctx, b, cands)
	for i := range boardable {
		boardable[i].BuildCost = costs[boardable[i].TypeID]
	}
	return boardable, nil
}

// ownedHullElsewhere is a packaged hull of this type the pilot owns in some
// station hangar: where a filled hauler bid leaves it, and where a hull bought by
// a run that died before boarding sits. Packaged only, since an assembled hull may
// be another fit's ship (rolebot_fit_ships).
func (m *Mod) ownedHullElsewhere(ctx context.Context, charID, typeID int64) (stationID int64) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT locationID FROM entity
		 WHERE ownerID = ? AND typeID = ? AND flag = ? AND singleton = 0
		   AND locationID >= 60000000 AND locationID < 64000000
		 ORDER BY itemID ASC LIMIT 1`, charID, typeID, flagHangar).Scan(&stationID)
	return stationID
}

func init() {
	registerCharForget("haulerBidChecked", func(id int64) {
		haulerBidMu.Lock()
		delete(haulerBidChecked, id)
		haulerBidMu.Unlock()
	})
	registerCharForget("cruiserBidChecked", func(id int64) {
		haulerBidMu.Lock()
		delete(cruiserBidChecked, id)
		haulerBidMu.Unlock()
	})
}
