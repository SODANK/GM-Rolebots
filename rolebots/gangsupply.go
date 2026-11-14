package rolebots

// The corporation supplies its fleet's hauler.
//
// A co-op mining fleet's hauler seat prefers pilots that own an industrial
// (gangform.go seatKey). When none of a managed corporation's hauler recruits has
// one, the corporation hands over a hull from its own stock (handOverHull) or
// funds a purchase (corpFundsHull). The funded path leaves a memo
// (TradeMemory.HaulerGrant*) that haulerGrantResume spends on the next docked
// plan. It runs once per corporation per week (lastCEOActionOn "haulergrant")
// and only for a corporation with a bot CEO running its treasury. The grant goes
// to the hauler recruit, not the commander, who mines (gangLeaderRole).

import (
	"context"
	"errors"
	"fmt"

	evedb "gurimoon/db"
)

const (
	// corpHaulerGrantMaxISK caps a funded purchase at three times an industrial's
	// ask (0.4-1.3M), which is the selector's upgrade bar.
	corpHaulerGrantMaxISK = 3_000_000.0
	// corpHaulerGrantEverySec is one grant per corporation per week.
	corpHaulerGrantEverySec = 7 * 24 * 3600
	// haulerGrantStaleSec is how long a grant waits to be spent before the memo is
	// dropped. The pilot keeps the ISK.
	haulerGrantStaleSec = 48 * 3600
	// corpHaulerGrantVerb is the CEO log verb and the throttle key.
	corpHaulerGrantVerb = "haulergrant"
)

// industrialOwners is which of these pilots own an industrial-class hull
// (industrialGroupIDs), assembled or packaged, anywhere. One query for the
// whole candidate list.
func (m *Mod) industrialOwners(ctx context.Context, ids []int64) map[int64]bool {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.ownerID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.ownerID IN (`+placeholders(len(ids))+`)
		  AND t.groupID IN (`+placeholders(len(industrialGroupIDs))+`)`,
		append(int64Args(ids), int64Args(industrialGroupIDs)...)...)
	if err != nil {
		rlogf("rolebots: industrial owners: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// corpProvidesHauler puts an industrial in a hauler's hands when a managed
// corporation's co-op fleet has none. Best-effort; a fleet forms either way.
func (m *Mod) corpProvidesHauler(ctx context.Context, lb *botRun, corpID int64, activity string, recruits []gangRecruitPick) {
	if !fleetSharesOre(activity) || corpID == 0 {
		return
	}
	if _, ok := managedCEO(corpID); !ok {
		return
	}
	var haulers []int64
	for _, r := range recruits {
		if r.role == RoleHauler || r.role == RoleTrader {
			haulers = append(haulers, r.charID)
		}
	}
	if len(haulers) == 0 {
		return // no hauler seat was filled at all; nothing to equip
	}
	owners := m.industrialOwners(ctx, haulers)
	for _, id := range haulers {
		if owners[id] {
			return // the op has a real hauler
		}
	}
	now := clock.Unix()
	if last := m.lastCEOActionOn(ctx, corpID, corpHaulerGrantVerb, 0); last != 0 && now-last < corpHaulerGrantEverySec {
		return
	}
	c, err := getCEO(ctx, m.db, corpID)
	if err != nil || c == nil || c.Paused {
		return
	}
	target := haulers[0]
	who := m.charNameOr(ctx, target)

	// 1. A hull from stock.
	if stationID, officeID := m.corpFactoryOffice(ctx, corpID); officeID != 0 {
		var typeID int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT e.typeID FROM entity e
			JOIN invTypes t ON t.typeID = e.typeID
			WHERE e.ownerID = ? AND e.locationID = ? AND e.flag IN (?, ?) AND e.quantity > 0
			  AND t.groupID IN (`+placeholders(len(industrialGroupIDs))+`)
			ORDER BY COALESCE(t.capacity, 0) DESC LIMIT 1`,
			append([]any{corpID, officeID, corpDivProduct, corpDivStore}, int64Args(industrialGroupIDs)...)...).Scan(&typeID)
		if typeID != 0 {
			itemID, herr := m.handOverHull(ctx, corpID, officeID, stationID, target, typeID)
			if herr == nil && itemID != 0 {
				name := m.typeNameOr(ctx, typeID)
				value := evedb.TypeValueOf(ctx, m.db, typeID).Value
				m.corpBook(ctx, corpID, lineHulls, -value, target,
					fmt.Sprintf("%s handed to %s for a fleet's hauler seat", name, who))
				m.ceoLog(ctx, c, powerProduction, corpHaulerGrantVerb, target, value,
					"handed %s a %s from stock to haul for the corporation's mining op", who, name)
				m.noteHaulerGrant(ctx, lb, target, typeID)
				m.logBot(target, logInfo, "gang",
					"the corporation has put a %s in its hangar at %s to haul for its mining ops",
					name, m.stationLabel(ctx, stationID))
				return
			}
		}
	}

	// 2. A funded purchase.
	amount, typeID := m.corpFundsHull(ctx, c, target, who)
	if amount > 0 {
		m.noteHaulerGrant(ctx, lb, target, typeID)
	}
}

// corpFundsHull moves the price of the cheapest listed industrial, at the
// selector's upgrade bar, from the corporation to the pilot. Returns what
// moved and the hull it was priced on; (0, 0) when nothing is listed or the
// treasury refuses.
func (m *Mod) corpFundsHull(ctx context.Context, c *corpCEO, charID int64, who string) (float64, int64) {
	var typeID int64
	var ask float64
	if err := m.db.QueryRowContext(ctx, `
		SELECT o.typeID, o.price FROM market_orders o
		JOIN invTypes t ON t.typeID = o.typeID
		WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		  AND t.groupID IN (`+placeholders(len(industrialGroupIDs))+`)
		ORDER BY o.price LIMIT 1`,
		int64Args(industrialGroupIDs)...).Scan(&typeID, &ask); err != nil || ask <= 0 {
		return 0, 0
	}
	amount := ask * upgradeAffordMultiple * 1.1
	if amount > corpHaulerGrantMaxISK {
		amount = corpHaulerGrantMaxISK
	}
	name := m.typeNameOr(ctx, typeID)
	if _, _, err := m.corpWithdraw(ctx, c.CorpID, charID, amount,
		"Corporation-funded hauler hull: "+name); err != nil {
		if !errors.Is(err, evedb.ErrInsufficientFunds) {
			m.logBot(c.CharID, logError, "ceo", "funding a %s for %s: %v", name, who, err)
		}
		return 0, 0
	}
	m.corpBook(ctx, c.CorpID, lineHulls, -amount, charID,
		fmt.Sprintf("%s funded for %s's hauler seat", name, who))
	m.ceoLog(ctx, c, powerTreasury, corpHaulerGrantVerb, charID, amount,
		"paid %s for %s to buy a %s and haul for the corporation's mining ops", iskString(amount), who, name)
	m.logBot(charID, logInfo, "gang",
		"the corporation paid it %s towards a %s to haul for its mining ops", iskString(amount), name)
	return amount, typeID
}

// noteHaulerGrant stamps the memo haulerGrantResume acts on. The target may
// be the commander whose run is in hand, or a recruit whose state is loaded
// for the write.
func (m *Mod) noteHaulerGrant(ctx context.Context, lb *botRun, charID, typeID int64) {
	b := lb
	if b == nil || b.CharID != charID {
		r, err := m.loadRun(ctx, charID)
		if err != nil {
			return
		}
		b = r
	}
	b.State.Data.HaulerGrantAt = clock.Unix()
	b.State.Data.HaulerGrantTypeID = typeID
	if err := saveState(ctx, m.db, b.State); err != nil {
		rlogf("rolebots: note hauler grant char=%d: %v", charID, err)
	}
}

// haulerGrantResume spends an open grant: a docked pilot with one runs the
// trade activity's procurement (ensureActivityShip) so the upgrade arm buys
// or boards the industrial. Reports whether it took the plan. The memo is
// cleared once an industrial is owned, or when it has gone stale.
func (m *Mod) haulerGrantResume(ctx context.Context, b *botRun) (bool, error) {
	d := &b.State.Data
	if d.HaulerGrantAt == 0 {
		return false, nil
	}
	clear := func(why string) {
		d.HaulerGrantAt, d.HaulerGrantTypeID = 0, 0
		m.logBot(b.CharID, logInfo, "gang", "hauler grant closed — %s", why)
		_ = saveState(ctx, m.db, b.State)
	}
	if m.industrialOwners(ctx, []int64{b.CharID})[b.CharID] {
		clear("it owns an industrial now")
		return false, nil
	}
	if clock.Unix()-d.HaulerGrantAt > haulerGrantStaleSec {
		clear("nothing to buy within reach for two days; the ISK stays with the pilot")
		return false, nil
	}
	if b.StationID == 0 {
		return false, nil // the upgrade arm needs a dock; the home loop brings it in
	}
	ready, err := m.ensureActivityShip(ctx, b, ActTrade)
	if err != nil {
		return false, err
	}
	if !ready {
		return true, nil // a procurement trip was started
	}
	// Ready and still no industrial: the selector chose a frigate because nothing
	// better is listed in reach. Leave the memo for a later dock.
	return false, nil
}
