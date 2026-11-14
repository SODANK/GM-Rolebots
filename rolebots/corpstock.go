package rolebots

import (
	"context"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// The business power, first part: the corporation sells what it holds.
//
// Mining fleets drop ore into corporation hangars (gangplan.go moveCargoToCorpHangar)
// and the business pass lists it on the corporation's own account. A corporation
// order needs no pilot at the counter: the core's ResolveMarketAccount asks for a
// player corporation, a trading role and an office at the station, so the CEO places
// it remotely under its own charID with useCorp set. Scope: ore in division 1 of an
// office, one stack at a time (see ceoListCorpStock). Every order is tracked in
// rolebot_corp_orders so the next pass can book fills as sales and reprice stale orders.

const (
	// corpOrderPatienceSec is how long a corporation sell order sits
	// untouched before the CEO undercuts it, and corpOrderStep how far.
	corpOrderPatienceSec = 3 * 86400
	corpOrderStep        = 0.05
	// corpListPerThink bounds one pass's new listings.
	corpListPerThink = 6
	// corpOrderSlotKeep is how many of the CEO's own order slots a corp listing never
	// takes. The core counts an order against the character that placed it
	// (countOpenOrders keys on charID), so each corp order is one the CEO cannot use itself.
	corpOrderSlotKeep = 2
	// corpStockAgedSec is when factory output still on its first listing counts as aged:
	// two patience periods. Its floor is then what its region pays (agedStockReprice),
	// not its cost or a fraction of basePrice.
	corpStockAgedSec = 2 * corpOrderPatienceSec
)

// ceoBusiness runs the business power: book what the corporation's orders
// sold, reprice the ones nobody is taking, list the stock it holds.
func (m *Mod) ceoBusiness(ctx context.Context, c *corpCEO, ch Charter) error {
	// Order pressure is counted afresh every think: this power and the
	// production power after it add what they could not place (corporders.go).
	noteCorpOrderPressure(c.CorpID, 0)
	if err := m.ceoSweepCorpOrders(ctx, c); err != nil {
		return err
	}
	// The ore desk first: it decides how much stock is kept for production rather than
	// listed (oredesk.go). A chair's own setting (corpcommanddesk_org.go) is left alone.
	if !m.humanHoldOn(ctx, c.CorpID, cmdKindOreDesk) {
		if err := m.ceoOreDesk(ctx, c, ch); err != nil {
			return err
		}
	}
	if err := m.ceoMiningBuyback(ctx, c, ch); err != nil {
		m.logBot(c.CharID, logError, "ceo", "mining buyback: %v", err)
	}
	if err := m.ceoListCorpStock(ctx, c); err != nil {
		return err
	}
	// Loot members handed in (corpspoils.go).
	if err := m.ceoListCorpSpoils(ctx, c); err != nil {
		m.logBot(c.CharID, logError, "ceo", "spoils: %v", err)
	}
	// The Trade desk (corptrade.go): a commerce outfit's own runs, on the
	// corporation's capital. A no-op for every other corporation.
	return m.tradeDesk(ctx, c, ch)
}

// ceoSweepCorpOrders books fills and reprices stale orders.
func (m *Mod) ceoSweepCorpOrders(ctx context.Context, c *corpCEO) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.orderID, r.stationID, r.typeID, r.price, r.volSeen, r.placedAt, r.repricedAt,
		       COALESCE(o.volRemaining, -1), COALESCE(o.orderState, 0), COALESCE(o.price, 0), r.kind, r.placedBy
		  FROM rolebot_corp_orders r
		  LEFT JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.bid = 0`, c.CorpID)
	if err != nil {
		return fmt.Errorf("corp orders: %w", err)
	}
	type tracked struct {
		orderID, stationID, typeID int64
		price                      float64
		volSeen, placedAt, repAt   int64
		remaining, state           int64
		livePrice                  float64
		kind                       string
		placedBy                   int64
	}
	var list []tracked
	for rows.Next() {
		var t tracked
		if err := rows.Scan(&t.orderID, &t.stationID, &t.typeID, &t.price, &t.volSeen,
			&t.placedAt, &t.repAt, &t.remaining, &t.state, &t.livePrice, &t.kind, &t.placedBy); err != nil {
			rows.Close()
			return err
		}
		list = append(list, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	now := clock.Unix()
	for _, t := range list {
		// A row the book no longer has was cancelled or expired: the goods
		// went back to the office and the next listing pass will find them.
		if t.remaining < 0 {
			m.forgetCorpOrder(ctx, t.orderID)
			continue
		}
		price := t.livePrice
		if price <= 0 {
			price = t.price
		}
		if sold := t.volSeen - t.remaining; sold > 0 {
			value := price * float64(sold)
			line := lineSales
			if t.kind == corpOrderStore {
				line = lineStore
			}
			m.corpBook(ctx, c.CorpID, line, value, t.typeID,
				fmt.Sprintf("%d× %s", sold, m.typeNameOr(ctx, t.typeID)))
			m.creditSalesSeat(ctx, c.CorpID, t.placedBy, value)
			m.ceoLog(ctx, c, powerBusiness, "sold", t.typeID, value,
				"the corporation sold %d× %s at %s for %s",
				sold, m.typeNameOr(ctx, t.typeID), m.stationLabel(ctx, t.stationID), iskString(value))
			if _, err := m.db.ExecContext(ctx,
				`UPDATE rolebot_corp_orders SET volSeen = ? WHERE orderID = ?`, t.remaining, t.orderID); err != nil {
				return err
			}
		}
		if t.state != 1 || t.remaining == 0 {
			m.forgetCorpOrder(ctx, t.orderID)
			continue
		}
		last := t.placedAt
		if t.repAt > last {
			last = t.repAt
		}
		if now-last < corpOrderPatienceSec {
			continue
		}
		// The company store sells at cost plus its markup and never chases the market down;
		// factory output never goes below what it cost to make (corpstore.go).
		if t.kind == corpOrderStore {
			continue
		}
		// Aged factory output goes to where its region's buyers actually pay.
		// What it cost is spent either way; the stock unsold is the loss.
		if t.kind == corpOrderProduct && now-t.placedAt >= corpStockAgedSec {
			if ref := m.regionTradeRef(ctx, t.stationID, t.typeID); ref.Trades >= corpTradeRefMinRegional {
				next, ok := agedStockReprice(price, ref.Avg, m.anchoredFloor(ctx, t.stationID, t.typeID))
				if !ok {
					continue // already at the market; it waits there
				}
				if err := m.repriceCorpOrder(ctx, c, t.orderID, t.typeID, price, next, now, last,
					fmt.Sprintf(" — its region has traded it at %s", iskString(ref.Avg))); err != nil {
					return err
				}
				continue
			}
		}
		floor := m.stableFloorPrice(ctx, t.typeID)
		if t.kind == corpOrderProduct {
			floor = math.Max(floor, m.corpUnitCost(ctx, c.CorpID, t.typeID))
		}
		if t.kind == corpOrderStock {
			// Floor: what the buyback paid for this rock (noteCorpBuyCost) plus broker and tax.
			// An order that never clears expires and is relisted at market (oreSellDays).
			floor = math.Max(floor, m.corpUnitCost(ctx, c.CorpID, t.typeID)*(1+corpResaleMargin))
		}
		if t.kind == corpOrderTrade {
			// The Trade desk never dumps under what the run cost it, wage
			// included (corptrade.go).
			floor = math.Max(floor, m.tradeLandedCost(ctx, c.CorpID, t.stationID, t.typeID))
		}
		floor = math.Max(floor, m.anchoredFloor(ctx, t.stationID, t.typeID))
		next := math.Round(price*(1-corpOrderStep)*100) / 100
		if next <= floor || next <= 0 {
			continue // it has reached the floor and waits there
		}
		if err := m.repriceCorpOrder(ctx, c, t.orderID, t.typeID, price, next, now, last, ""); err != nil {
			return err
		}
	}
	return nil
}

// repriceCorpOrder moves one tracked corporation sell order to next and
// says so. An order the market will not modify is left where it is.
func (m *Mod) repriceCorpOrder(ctx context.Context, c *corpCEO, orderID, typeID int64,
	price, next float64, now, last int64, why string) error {
	out, err := evedb.ModifyMarketOrder(ctx, m.db, m.corpOrderOwner(ctx, orderID, c.CharID), orderID, next)
	if err != nil || out.ErrorMsg != "" {
		// Most likely the order was placed by a previous CEO; it is left at
		// its price rather than cancelled out from under the book.
		return nil
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_orders SET price = ?, repricedAt = ? WHERE orderID = ?`,
		next, now, orderID); err != nil {
		return err
	}
	m.ceoLog(ctx, c, powerBusiness, "reprice", typeID, next,
		"nobody took the corporation's %s at %s for %s — asking %s now%s",
		m.typeNameOr(ctx, typeID), iskString(price), fmtDuration(now-last), iskString(next), why)
	return nil
}

// agedStockReprice is where aged factory output is asked next, pure: just under what
// its region has been paying (ref), never under the NPC band's repair line (anchorFloor,
// 0 = no NPC sells it there). ok is false when the order already asks that or less.
func agedStockReprice(price, ref, anchorFloor float64) (float64, bool) {
	if ref <= 0 {
		return price, false
	}
	next := math.Max(roundISK(ref*(1-corpOrderStep)), anchorFloor)
	if next <= 0 || next >= price {
		return price, false
	}
	return next, true
}

// cancelCorpOrdersOf takes every open order a pilot placed on its corporation's
// account off the book, returning goods to the office and escrow to the corporation
// wallet. Best effort: it runs ahead of a purge that deletes the rows regardless.
func (m *Mod) cancelCorpOrdersOf(ctx context.Context, charID int64) {
	ids, err := int64Column(ctx, m, `
		SELECT orderID FROM market_orders
		 WHERE charID = ? AND isCorp = 1 AND orderState = 1`, charID)
	if err != nil {
		rlogf("rolebots: corp orders of %d: %v", charID, err)
		return
	}
	for _, id := range ids {
		if _, err := evedb.CancelMarketOrder(ctx, m.db, charID, id); err != nil {
			rlogf("rolebots: cancel corp order %d of %d: %v", id, charID, err)
		}
		m.forgetCorpOrder(ctx, id)
	}
}

func (m *Mod) forgetCorpOrder(ctx context.Context, orderID int64) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_corp_orders WHERE orderID = ?`, orderID); err != nil {
		rlogf("rolebots: forget corp order %d: %v", orderID, err)
	}
}

// ceoListCorpStock lists the ore sitting in the corporation's office hangars.
func (m *Mod) ceoListCorpStock(ctx context.Context, c *corpCEO) error {
	// One division-1 stack per listing, never a sum: the core's sale walks every division
	// the CEO may take from, so a sum would be made up from a human director's divisions 2-7.
	if offices, err := int64Column(ctx, m,
		`SELECT itemID FROM crpoffices WHERE corporationID = ?`, c.CorpID); err == nil {
		for _, office := range offices {
			if _, err := evedb.StackAllItems(ctx, m.db, c.CorpID, office, flagHangar); err != nil {
				rlogf("rolebots: stacking corp %d office %d: %v", c.CorpID, office, err)
			}
		}
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, e.typeID, e.quantity, e.itemID
		  FROM crpoffices o
		  JOIN entity e     ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		                   AND e.flag = ? AND e.singleton = 0
		  JOIN invTypes t   ON t.typeID = e.typeID
		  JOIN invGroups g  ON g.groupID = t.groupID
		 WHERE o.corporationID = ? AND g.categoryID = 25 AND e.quantity > 0
		 ORDER BY o.stationID, e.typeID, e.quantity DESC, e.itemID`, flagHangar, c.CorpID)
	if err != nil {
		return fmt.Errorf("corp stock: %w", err)
	}
	type lot struct{ stationID, typeID, qty, itemID int64 }
	var lots []lot
	seen := map[[2]int64]bool{}
	for rows.Next() {
		var l lot
		if err := rows.Scan(&l.stationID, &l.typeID, &l.qty, &l.itemID); err != nil {
			rows.Close()
			return err
		}
		// The biggest stack of each commodity at each station; the rest (if
		// a merge failed) waits for the next pass like any second lot.
		key := [2]int64{l.stationID, l.typeID}
		if seen[key] {
			continue
		}
		seen[key] = true
		lots = append(lots, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// The ore desk decides how much of each stack is kept for production
	// (oredesk.go); a hold whose stack is gone is forgotten.
	share := m.corpOreShare(ctx, c.CorpID)
	holds := m.loadOreHolds(ctx, c.CorpID)
	// Ore the corporation is buying at a station is kept whole there: its own bid fills
	// into division 1, and listing a share would resell what the buyback just paid for.
	bidding := m.corpBiddingOn(ctx, c.CorpID)
	for key := range holds {
		if !seen[key] {
			m.dropOreHold(ctx, c.CorpID, key[0], key[1])
		}
	}
	if len(lots) == 0 {
		return nil
	}

	ceo, err := m.loadRun(ctx, c.CharID)
	if err != nil {
		return nil // a CEO that cannot be hydrated cannot price; try next think
	}
	listed := 0
	for _, l := range lots {
		if listed >= corpListPerThink || ctx.Err() != nil {
			return nil
		}
		placer := m.corpOrderPlacer(ctx, c.CorpID, c.CharID)
		if placer == 0 {
			addCorpOrderPressure(c.CorpID, len(lots)-listed)
			if listed == 0 && clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "slots") >= 86400 {
				m.ceoLog(ctx, c, powerBusiness, "slots", 0, 0,
					"has corporation stock to list but no market order slots to spare — neither it nor its Industry Director has one")
			}
			return nil
		}
		// One order per commodity per station, as elsewhere in the module: there is no
		// quantity-modify on the wire and a twin order cannot be undone. The stock waits.
		var open int
		if err := m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_corp_orders
			 WHERE corporationID = ? AND stationID = ? AND typeID = ? AND bid = 0`,
			c.CorpID, l.stationID, l.typeID).Scan(&open); err != nil || open > 0 {
			continue
		}
		key := [2]int64{l.stationID, l.typeID}
		h := holds[key]
		lotShare := share
		if bidding[key] && share > 0 {
			lotShare = 1 // a resale outfit (share 0) lists what its bid bought
		}
		listQty, keep, release := oreHoldPlan(l.qty, h.keep, lotShare, h.since, clock.Unix())
		since := h.since
		if since == 0 {
			since = clock.Unix()
		}
		if listQty <= 0 {
			m.saveOreHold(ctx, c.CorpID, l.stationID, l.typeID, keep, since)
			continue
		}
		l.qty = listQty
		// Valued at the office's own region, where the buyback priced its bid, not where the
		// CEO is flying.
		regionID, _ := evedb.GetRegionForStation(ctx, m.db, l.stationID)
		price, why, err := m.oreAskIn(ctx, ceo, []int64{regionID}, l.stationID, l.typeID, l.qty)
		intoBid := false
		if err == nil && price <= 0 {
			// The ask was refused because a bid here already pays more than the rock may be
			// listed at. That bid is the sale, if it is somebody else's and not the corporation's.
			if bp, n := m.outsideBidTake(ctx, c.CorpID, l.stationID, l.typeID, l.qty); bp > 0 && n > 0 {
				price, l.qty, intoBid = bp, n, true
			}
		}
		if err != nil || price <= 0 || (!intoBid && price*float64(l.qty) < oreListMinValue) {
			// A priced lot that is merely too small comes back with the pricing note in why;
			// name the size instead so the line does not read as a refusal.
			if err == nil && price > 0 {
				why = fmt.Sprintf("worth %s at %.2f each, under the %s a listing is worth an order slot for",
					iskString(price*float64(l.qty)), price, iskString(oreListMinValue))
			}
			if err == nil && why != "" {
				m.logBot(c.CharID, logInfo, "ceo", "not listing the corporation's %d× %s — %s",
					l.qty, m.typeNameOr(ctx, l.typeID), why)
			}
			continue
		}
		out, err := evedb.PlaceSellOrder(ctx, m.db, placer, l.stationID,
			l.typeID, l.itemID, price, l.qty, -1 /*station*/, 1, oreSellDays, true)
		if err != nil {
			return fmt.Errorf("list corp %s: %w", m.typeNameOr(ctx, l.typeID), err)
		}
		if out.ErrorMsg != "" {
			m.logBot(c.CharID, logInfo, "ceo", "the market refused the corporation's %s listing: %s",
				m.typeNameOr(ctx, l.typeID), out.ErrorMsg)
			continue
		}
		listed++
		if release || keep <= 0 {
			m.dropOreHold(ctx, c.CorpID, l.stationID, l.typeID)
		} else {
			m.saveOreHold(ctx, c.CorpID, l.stationID, l.typeID, keep, since)
		}
		// A listing at or under a standing bid clears on the spot, and that
		// is a sale like any other.
		filled := 0.0
		for _, f := range out.Fills {
			filled += f.Price * float64(f.Quantity)
		}
		if filled > 0 {
			m.corpBook(ctx, c.CorpID, lineSales, filled, l.typeID,
				fmt.Sprintf("%d× %s", out.FilledQty, m.typeNameOr(ctx, l.typeID)))
		}
		if out.RestingOrderID != 0 {
			remaining := l.qty - out.FilledQty
			if _, err := m.db.ExecContext(ctx, `
				INSERT INTO rolebot_corp_orders
					(orderID, corporationID, stationID, typeID, bid, price, volSeen, placedAt, placedBy)
				VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`,
				out.RestingOrderID, c.CorpID, l.stationID, l.typeID, price, remaining, clock.Unix(), placer); err != nil {
				return err
			}
		}
		if intoBid {
			m.ceoLog(ctx, c, powerBusiness, "sold", l.typeID, filled,
				"sold %d× of the corporation's %s into the %s bid at %s — more than it may be listed at",
				out.FilledQty, m.typeNameOr(ctx, l.typeID), iskString(price), m.stationLabel(ctx, l.stationID))
			continue
		}
		m.ceoLog(ctx, c, powerBusiness, "list", l.typeID, price*float64(l.qty),
			"listed the corporation's %d× %s at %s for %s each%s",
			l.qty, m.typeNameOr(ctx, l.typeID), m.stationLabel(ctx, l.stationID), iskString(price),
			map[bool]string{true: fmt.Sprintf(" (%d sold on the spot)", out.FilledQty)}[out.FilledQty > 0])
	}
	return nil
}

// outsideBidTake is the bid a refused ore lot can be sold into: the best live bid at
// the station that is not one of the corporation's own orders, and how many units the
// bids at that price take (never more than qty). Returns (0, 0) when there is none or
// the corporation's own bid is as good, since selling to itself costs broker and tax.
func (m *Mod) outsideBidTake(ctx context.Context, corpID, stationID, typeID, qty int64) (float64, int64) {
	if qty <= 0 {
		return 0, 0
	}
	var own float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(o.price), 0) FROM market_orders o
		  JOIN rolebot_corp_orders r ON r.orderID = o.orderID AND r.corporationID = ?
		 WHERE o.stationID = ? AND o.typeID = ? AND o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0`,
		corpID, stationID, typeID).Scan(&own)
	best := m.bestOutsideBid(ctx, corpID, stationID, typeID)
	if best <= 0 || best <= own {
		return 0, 0
	}
	var vol int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(volRemaining), 0) FROM market_orders
		 WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0 AND price >= ?`,
		stationID, typeID, best).Scan(&vol)
	return best, min(qty, vol)
}
