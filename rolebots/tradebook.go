package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	evedb "gurimoon/db"
)

// The trade book: what the roster's commerce actually did. It is a ledger with one row per movement of goods and
// the money on it, behind the trade feed (esptrade.go), the flow term of the hub score (traffic.go) and a resting
// order's profit. It also holds what a market-making pilot needs:
//
//	rolebot_orders       the orders it has out, and why
//	rolebot_trade_stock  goods it is holding as inventory, and what they cost
//
// The hangar is the truth about what a pilot has; the stock table says which of it is trade inventory rather
// than a mining stockpile, a bill of materials or ammunition.

// Ledger kinds. Short and stable: the feed filters on them.
const (
	tradeKindBuy    = "buy"    // filled a sell order to acquire goods
	tradeKindSell   = "sell"   // hit a bid to unload
	tradeKindList   = "list"   // put a resting order on the market
	tradeKindFill   = "fill"   // somebody took a resting order
	tradeKindCancel = "cancel" // pulled a resting order
	tradeKindPrice  = "price"  // repriced a resting order
	tradeKindHaul   = "haul"   // a completed run, both legs, with its margin
	tradeKindFee    = "fee"    // broker fee paid to leave an order resting
)

const (
	// orderSweepEveryTicks is how often the order sweep runs: resting orders fill when somebody else buys them,
	// independent of whose turn it is. Once a minute.
	orderSweepEveryTicks = 60

	// tradeBookRetentionDays is how long the feed keeps rows: 30 days, because the bid evidence reads list and
	// sell rows that far back (bidEvidenceDays).
	tradeBookRetentionDays = 30

	// tradeChatterRetentionDays: fee, price and cancel rows go sooner. The longest reader looks back a week
	// (the ESP feed summary; dead shelves read 3 days of cancels).
	tradeChatterRetentionDays = 7

	// closedBotOrderKeepDays is how long a closed rolebot_orders row is kept. Every query asks for closedAt = 0 and
	// re-registering an order ID reopens its row through the upsert, so a day covers a fill the sweep books just
	// after marking the order closed.
	closedBotOrderKeepDays = 1

	// peerTradeRetentionDays is how long the bot-to-bot ledger is kept. It records whether the roster's internal
	// economy exists, a question about months, and has one row per trade between two bots.
	peerTradeRetentionDays = 30

	// orderSlotReserve is how many order slots a trader leaves alone. A manufacturing pilot lists its product
	// through the same cap (industry.go), so a trader filling every slot would refuse a finished production run.
	orderSlotReserve = 2
)

// ── the ledger ──────────────────────────────────────────────────────────────

// tradeEntry is one line of the book.
type tradeEntry struct {
	CharID   int64
	Kind     string
	TypeID   int64
	TypeName string
	Qty      int64
	Price    float64
	Value    float64
	Profit   float64
	Station  int64
	System   int64
	FromSys  int64
	OrderID  int64
	Note     string
	// Resale marks a leg that acquired goods to sell on rather than to use:
	// the trade tape does not count it as a sale (tradeenduse.go).
	Resale bool
}

// bookTrade records one leg and feeds the map. Best-effort: a sale that
// happened is not un-happened by a ledger write failing, and the caller
// has already moved real ISK by the time it gets here.
func (m *Mod) bookTrade(ctx context.Context, e tradeEntry) {
	if e.Value == 0 {
		e.Value = e.Price * float64(e.Qty)
	}
	resale := 0
	if e.Resale {
		resale = 1
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_trades
			(ts, characterID, kind, typeID, typeName, quantity, price, value, profit,
			 stationID, systemID, fromSystemID, orderID, note, resale)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		clock.Unix(), e.CharID, e.Kind, e.TypeID, e.TypeName, e.Qty, e.Price, e.Value,
		e.Profit, e.Station, e.System, e.FromSys, e.OrderID, e.Note, resale); err != nil {
		rlogf("rolebots: trade book: %v", err)
		return
	}
	switch e.Kind {
	case tradeKindBuy:
		m.recordMarketFlow(ctx, e.System, true, e.Value)
	case tradeKindSell, tradeKindFill:
		m.recordMarketFlow(ctx, e.System, false, e.Value)
	case tradeKindHaul:
		m.recordHaul(ctx, e.FromSys, e.System, e.Value)
	}
	m.countTrade(e)
}

// countTrade folds one leg into the pilot's lifetime trade counters (statcount.go). The book prunes at 30 days;
// these feed the Stats page, so they outlive it. A haul row repeats its sell leg's Value and Profit (it exists to
// draw the lane), so it is not counted, nor are cancel and price rows. A fee row counts into profit (Profit = -fee)
// but not turnover. bestTrade is the best single leg, a maximum through the whole path including the upsert's GREATEST.
func (m *Mod) countTrade(e tradeEntry) {
	if e.CharID == 0 {
		return
	}
	switch e.Kind {
	case tradeKindBuy:
		bumpStats(e.CharID, func(d *statDelta) {
			d.Trades++
			d.Buys++
			d.Turnover += e.Value
		})
	case tradeKindSell, tradeKindFill:
		bumpStats(e.CharID, func(d *statDelta) {
			d.Trades++
			d.Sells++
			d.Turnover += e.Value
			d.Profit += e.Profit
			if e.Profit > d.BestTrade {
				d.BestTrade = e.Profit
			}
		})
	case tradeKindList:
		bumpStats(e.CharID, func(d *statDelta) { d.OrdersPlaced++ })
	case tradeKindFee:
		bumpStats(e.CharID, func(d *statDelta) {
			d.FeesPaid += e.Value
			d.Profit += e.Profit
		})
	}
}

// pruneTradeBook drops entries past the retention window, called from the order sweep. It deletes in chunks,
// oldest first (see log.go's prune): one unbounded DELETE holds its row locks for a whole day's expiry and keeps
// running on the server after a client-side cancel. Whatever one sweep leaves, the next takes.
func (m *Mod) pruneTradeBook(ctx context.Context) {
	now := clock.Unix()
	cutoff := now - int64(tradeBookRetentionDays)*86400
	m.pruneChunked(ctx, "trade book",
		`DELETE FROM rolebot_trades WHERE ts < ? ORDER BY ts LIMIT ?`, cutoff)
	// One statement per kind, not an IN list: (kind, ts) gives each its order
	// straight off idx_rbtrade_kind_ts, where an IN list filesorts every match.
	chatterCutoff := now - int64(tradeChatterRetentionDays)*86400
	for _, kind := range []string{tradeKindFee, tradeKindPrice, tradeKindCancel} {
		m.pruneChunked(ctx, "trade book "+kind,
			`DELETE FROM rolebot_trades WHERE kind = '`+kind+`' AND ts < ? ORDER BY ts LIMIT ?`, chatterCutoff)
	}
	m.pruneChunked(ctx, "order registry",
		`DELETE FROM rolebot_orders WHERE closedAt > 0 AND closedAt < ? ORDER BY closedAt LIMIT ?`,
		now-int64(closedBotOrderKeepDays)*86400)
	peerCutoff := now - int64(peerTradeRetentionDays)*86400
	m.pruneChunked(ctx, "peer ledger",
		`DELETE FROM rolebot_peer_trades WHERE ts < ? ORDER BY ts LIMIT ?`, peerCutoff)
	m.pruneClosedBotOrders(ctx)
	m.pruneBPCListings(ctx)
}

// closedOrderKeepSec is how long a filled-out bot order stays in
// market_orders after it was issued. The order sweep only needs the row until
// it has seen the order close; two days is generous for that.
const closedOrderKeepSec = 2 * 86400

// pruneClosedBotOrders deletes bot orders that filled out (orderState 3). The core never deletes a filled order and
// nothing reads a bot's (every core reader of state 3 filters on the Seeder), so they pile up in market_orders. A row
// goes only when no open rolebot_orders entry names it: a row the sweep has not yet seen close would be booked as
// an expiry (orderSweep). A corporation order (rolebot_corp_orders) is left until its own sweep forgets it, for the
// same reason: the corporation sweeps read a missing row as cancelled or expired (sweepReqBid, sweepCorpBids).
func (m *Mod) pruneClosedBotOrders(ctx context.Context) {
	cutoff := evedb.UnixToWin32(clock.Unix() - closedOrderKeepSec)
	for i := 0; i < tradePruneMaxChunks && ctx.Err() == nil; i++ {
		if i > 0 && !prunePause(ctx) {
			return
		}
		rows, err := m.db.QueryContext(ctx, capStatement(`
			SELECT mo.orderID FROM market_orders mo
			  JOIN rolebot_characters rc ON rc.characterID = mo.charID
			 WHERE mo.orderState = 3 AND mo.issued < ?
			   AND NOT EXISTS (SELECT 1 FROM rolebot_orders o
			                    WHERE o.orderID = mo.orderID AND o.closedAt = 0)
			   AND NOT EXISTS (SELECT 1 FROM rolebot_corp_orders co WHERE co.orderID = mo.orderID)
			 LIMIT ?`), cutoff, tradePruneChunk)
		if err != nil {
			rlogf("rolebots: closed order prune: %v", err)
			return
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if len(ids) == 0 {
			return
		}
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM market_orders WHERE orderState = 3 AND orderID IN (`+placeholders(len(ids))+`)`,
			int64Args(ids)...); err != nil {
			rlogf("rolebots: closed order prune: %v", err)
			return
		}
		if len(ids) < tradePruneChunk {
			return
		}
	}
}

// The prune pace. These sweeps run every minute, so 5 x 2,000 rows per table per
// sweep is 600k rows an hour per table, far above any steady insert rate. The old
// 40 x 5,000 (200k rows per statement per minute, across six statements) let a
// retention backlog, as after downtime, delete over a million indexed rows a minute
// and drive the database to ~1 GB/min of writes until it was gone. A backlog now
// drains over hours instead of minutes, which nobody notices.
const (
	tradePruneChunk     = 2000
	tradePruneMaxChunks = 5
	// tradePruneGap is the pause between chunks, so InnoDB purge and the page
	// cleaner keep up instead of the deletes queueing undo and dirty pages.
	tradePruneGap = 200 * time.Millisecond
)

// prunePause waits tradePruneGap, or returns false if ctx ends first.
func prunePause(ctx context.Context) bool {
	t := time.NewTimer(tradePruneGap)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pruneChunked runs a `... < ? ORDER BY <indexed col> LIMIT ?` delete until a
// chunk comes back short, the per-sweep cap is reached, or ctx is done.
func (m *Mod) pruneChunked(ctx context.Context, what, q string, cutoff int64) {
	for i := 0; i < tradePruneMaxChunks && ctx.Err() == nil; i++ {
		if i > 0 && !prunePause(ctx) {
			return
		}
		res, err := m.db.ExecContext(ctx, q, cutoff, tradePruneChunk)
		if err != nil {
			rlogf("rolebots: %s prune: %v", what, err)
			return
		}
		if n, err := res.RowsAffected(); err != nil || n < tradePruneChunk {
			return
		}
	}
}

// ── bot-to-bot ──────────────────────────────────────────────────────────────
//
// A rolebot buying a hull off another rolebot's listing makes the roster an economy rather than a crowd trading
// with the NPC shelf. The taker is the only side that sees the fills (each MarketFill carries the counterparty and
// whether it was an NPC seeder), so the pair is written from there once; the resting side is left alone, since
// writing from both would double every row.

// isRolebot reports whether a character is on the roster at all, enabled or not. A bot switched off after a
// trade did not un-make the trade.
func (m *Mod) isRolebot(_ context.Context, charID int64) bool {
	return charID > 0 && m.isSynthetic(charID)
}

// stationSystemID resolves the system a station sits in. Zero when the station is unknown, which callers treat
// as unplaced rather than an error: a ledger row with no system is still a ledger row.
func (m *Mod) stationSystemID(ctx context.Context, stationID int64) int64 {
	return m.stationSystemOf(ctx, stationID)
}

// typeNameOr names a type for a ledger row, falling back to its ID rather than an empty column, since a peer
// row with no name is unreadable.
func (m *Mod) typeNameOr(ctx context.Context, typeID int64) string {
	if name := m.typeNameOf(ctx, typeID); name != "" {
		return name
	}
	return fmt.Sprintf("type %d", typeID)
}

// notePeerFills writes one peer row per fill that landed against another rolebot. e supplies the taker's side;
// weBought says which end of each fill the taker was on. The fill's own station wins over the caller's: a buy
// order with a range wider than the station matches wherever the ask is.
func (m *Mod) notePeerFills(ctx context.Context, e tradeEntry, fills []evedb.MarketFill, weBought bool, purpose string) {
	for _, f := range fills {
		if f.IsNPCOrder || f.Quantity <= 0 {
			continue
		}
		other := f.CounterpartyID
		if other == e.CharID || !m.isRolebot(ctx, other) {
			continue
		}
		stationID := f.StationID
		if stationID == 0 {
			stationID = e.Station
		}
		systemID := e.System
		if stationID != e.Station {
			systemID = m.stationSystemID(ctx, stationID)
		}
		buyer, seller := e.CharID, other
		if !weBought {
			buyer, seller = other, e.CharID
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_peer_trades
				(ts, buyerID, sellerID, takerID, typeID, typeName, quantity,
				 price, value, stationID, systemID, purpose)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			clock.Unix(), buyer, seller, e.CharID, e.TypeID, e.TypeName, f.Quantity,
			f.Price, f.Price*float64(f.Quantity), stationID, systemID, purpose); err != nil {
			rlogf("rolebots: peer ledger: %v", err)
			return
		}
	}
}

// ── stock ───────────────────────────────────────────────────────────────────

// stockRow is inventory a pilot is holding somewhere.
type stockRow struct {
	StationID  int64
	SystemID   int64
	TypeID     int64
	TypeName   string
	Units      int64
	UnitCost   float64
	AcquiredAt int64
}

// addStock books goods into inventory at a station, averaging the cost over what is already there: a hundred
// bought at 10 and another hundred at 20 are two hundred at 15, or the next sale reports a profit it did not make.
func (m *Mod) addStock(ctx context.Context, charID, stationID, systemID, typeID int64,
	typeName string, units int64, unitCost float64) {
	if units <= 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_trade_stock
			(characterID, stationID, systemID, typeID, typeName, units, unitCost, acquiredAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			unitCost = CASE WHEN units + VALUES(units) > 0
			                THEN (unitCost * units + VALUES(unitCost) * VALUES(units))
			                     / (units + VALUES(units))
			                ELSE VALUES(unitCost) END,
			units = units + VALUES(units),
			typeName = VALUES(typeName),
			systemID = VALUES(systemID),
			acquiredAt = VALUES(acquiredAt)`,
		charID, stationID, systemID, typeID, typeName, units, unitCost, clock.Unix()); err != nil {
		rlogf("rolebots: add stock char %d type %d: %v", charID, typeID, err)
	}
}

// takeStock removes units from inventory and reports what they cost. A
// missing row answers zero cost, which is honest: goods nothing recorded
// buying were not bought as inventory.
func (m *Mod) takeStock(ctx context.Context, charID, stationID, typeID, units int64) float64 {
	var have int64
	var cost float64
	err := m.db.QueryRowContext(ctx, `
		SELECT units, unitCost FROM rolebot_trade_stock
		WHERE characterID = ? AND stationID = ? AND typeID = ?`,
		charID, stationID, typeID).Scan(&have, &cost)
	if err != nil {
		return 0
	}
	left := have - units
	if left <= 0 {
		_, _ = m.db.ExecContext(ctx, `
			DELETE FROM rolebot_trade_stock
			WHERE characterID = ? AND stationID = ? AND typeID = ?`,
			charID, stationID, typeID)
	} else {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_trade_stock SET units = ?
			WHERE characterID = ? AND stationID = ? AND typeID = ?`,
			left, charID, stationID, typeID)
	}
	return cost
}

// pilotStock lists everything a pilot is holding as trade inventory, biggest holding first. Rows whose goods are
// no longer in that hangar (an operator moved them, a fit consumed them) are dropped as found: the hangar is the truth.
func (m *Mod) pilotStock(ctx context.Context, charID int64) ([]stockRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.systemID, s.typeID, s.typeName, s.units, s.unitCost, s.acquiredAt,
		       COALESCE((SELECT SUM(e.quantity) FROM entity e
		                 WHERE e.ownerID = s.characterID AND e.locationID = s.stationID
		                   AND e.typeID = s.typeID AND e.flag = 4 AND e.singleton = 0), 0)
		  FROM rolebot_trade_stock s
		 WHERE s.characterID = ?
		 ORDER BY s.units * s.unitCost DESC`, charID)
	if err != nil {
		return nil, fmt.Errorf("pilotStock: %w", err)
	}
	defer rows.Close()
	var out, heal []stockRow
	var stale [][2]int64
	for rows.Next() {
		var r stockRow
		var inHangar int64
		if err := rows.Scan(&r.StationID, &r.SystemID, &r.TypeID, &r.TypeName,
			&r.Units, &r.UnitCost, &r.AcquiredAt, &inHangar); err != nil {
			return nil, err
		}
		if inHangar <= 0 {
			stale = append(stale, [2]int64{r.StationID, r.TypeID})
			continue
		}
		if inHangar < r.Units {
			r.Units = inHangar
			heal = append(heal, r)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, s := range stale {
		_, _ = m.db.ExecContext(ctx, `
			DELETE FROM rolebot_trade_stock
			WHERE characterID = ? AND stationID = ? AND typeID = ?`, charID, s[0], s[1])
	}
	// A row claiming more than the hangar holds is written back down to the hangar, and the order is re-taken on the
	// clamped value: the SQL ranked by the claimed figure, so an inflated crate would sort first.
	for _, r := range heal {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_trade_stock SET units = ?
			WHERE characterID = ? AND stationID = ? AND typeID = ?`, r.Units, charID, r.StationID, r.TypeID)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return float64(out[i].Units)*out[i].UnitCost > float64(out[j].Units)*out[j].UnitCost
	})
	return out, nil
}

// ── resting orders ──────────────────────────────────────────────────────────

// registerOrder remembers a resting order this module placed, with the
// cost basis of the goods behind it.
func (m *Mod) registerOrder(ctx context.Context, charID, orderID, typeID, stationID, systemID int64,
	typeName string, bid bool, price, unitCost float64, vol int64, kind string) {
	if orderID == 0 {
		return
	}
	b := 0
	if bid {
		b = 1
	}
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_orders
			(orderID, characterID, typeID, typeName, bid, stationID, systemID,
			 price, unitCost, volEntered, volRemaining, kind, placedAt, lastSeenAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE price = VALUES(price), volRemaining = VALUES(volRemaining),
			lastSeenAt = VALUES(lastSeenAt), closedAt = 0`,
		orderID, charID, typeID, typeName, b, stationID, systemID,
		price, unitCost, vol, vol, kind, now, now); err != nil {
		rlogf("rolebots: register order %d: %v", orderID, err)
	}
	if !bid {
		m.chatterMarketListed(charID, typeID, stationID, systemID, typeName, price, vol)
	}
}

// openOrders counts what a character currently has resting on the market, all of it and not just this module's,
// because the skill cap does not care who placed them.
func (m *Mod) openOrders(ctx context.Context, charID int64) (int64, error) {
	var n int64
	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM market_orders WHERE charID = ? AND orderState = 1`, charID).Scan(&n)
	return n, err
}

// freeOrderSlots is how many more orders this pilot may place before it is
// crowding out its own other business. Never negative.
func (m *Mod) freeOrderSlots(ctx context.Context, charID int64) int64 {
	limits, err := evedb.GetMarketSkillLimits(ctx, m.db, charID)
	if err != nil {
		return 0
	}
	open, err := m.openOrders(ctx, charID)
	if err != nil {
		return 0
	}
	free := limits.MaxOrders - open - orderSlotReserve
	if free < 0 {
		return 0
	}
	return free
}

// ── the order sweep ─────────────────────────────────────────────────────────

var orderSweepBusy = make(chan struct{}, 1)

func (m *Mod) startOrderSweep() {
	m.registerPeriodic("rolebot-order-sweep", orderSweepEveryTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		select {
		case orderSweepBusy <- struct{}{}:
		default:
			return true
		}
		goSafe("order sweep", func() {
			defer func() { <-orderSweepBusy }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			m.orderSweep(ctx)
			m.pruneTradeBook(ctx)
		})
		return true
	})
}

// orderRow is one registered resting order joined against the live market.
type orderRow struct {
	OrderID   int64
	CharID    int64
	TypeID    int64
	TypeName  string
	Bid       bool
	StationID int64
	SystemID  int64
	Price     float64
	UnitCost  float64
	Booked    int64 // volRemaining as of the last sweep
	PlacedAt  int64
	// Kind is why the order was placed (trade.go). The sweep must not treat goods as merchandise that were never
	// bought to be sold on: a tradeDealSupply bid buys materials for a build, and a tradeDealBuild ask is the
	// factory's own output, whose fills are the only sales evidence the build book's shelf gets.
	Kind string

	// Exists says the market_orders row is still there at all; State is what it says. They are different questions,
	// and conflating them books a cancelled order as a sale (see orderSweep).
	Exists    bool
	State     int64
	LivePrice float64
	LiveVol   int64
}

// Live reports whether the order is still open on the market.
func (o orderRow) Live() bool { return o.Exists && o.State == 1 }

// orderSweep reconciles every registered order against the market: books what filled, closes what is gone, and
// pushes the ones going nowhere (repriced, or pulled with the goods handed back to inventory). A vanished row is
// not a sale: a filled order stays at orderState 3 while an expired or cancelled one is deleted, so the join asks
// whether the row exists separately from its state.
func (m *Mod) orderSweep(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.orderID, o.characterID, o.typeID, o.typeName, o.bid, o.stationID, o.systemID,
		       o.price, o.unitCost, o.volRemaining, o.placedAt, o.kind,
		       mo.orderID IS NOT NULL, COALESCE(mo.orderState, 0),
		       COALESCE(mo.price, 0), COALESCE(mo.volRemaining, 0)
		  FROM rolebot_orders o
		  LEFT JOIN market_orders mo ON mo.orderID = o.orderID
		 WHERE o.closedAt = 0`)
	if err != nil {
		rlogf("rolebots: order sweep: %v", err)
		return
	}
	var open []orderRow
	for rows.Next() {
		var o orderRow
		var bid int64
		if err := rows.Scan(&o.OrderID, &o.CharID, &o.TypeID, &o.TypeName, &bid,
			&o.StationID, &o.SystemID, &o.Price, &o.UnitCost, &o.Booked, &o.PlacedAt, &o.Kind,
			&o.Exists, &o.State, &o.LivePrice, &o.LiveVol); err != nil {
			rows.Close()
			rlogf("rolebots: order sweep scan: %v", err)
			return
		}
		o.Bid = bid != 0
		open = append(open, o)
	}
	rows.Close()

	// One trait read per pilot per sweep, not one per order.
	tunes := map[int64]Tuning{}
	tuneOf := func(charID int64) (Tuning, bool) {
		if t, ok := tunes[charID]; ok {
			return t, true
		}
		t, err := m.tuneFor(ctx, charID)
		if err != nil {
			return t, false
		}
		tunes[charID] = t
		return t, true
	}

	for _, o := range open {
		if !o.Exists {
			m.bookVanished(ctx, o)
			continue
		}
		if filled := o.Booked - o.LiveVol; filled > 0 {
			m.bookFill(ctx, o, filled)
		}
		if !o.Live() {
			// Still on the books but closed: the ordinary end of a listing that sold out.
			m.closeOrder(ctx, o)
			continue
		}
		if orderNeedsWrite(o) {
			if _, err := m.db.ExecContext(ctx,
				`UPDATE rolebot_orders SET volRemaining = ?, price = ?, lastSeenAt = ?
				 WHERE orderID = ?`, o.LiveVol, o.LivePrice, clock.Unix(), o.OrderID); err != nil {
				rlogf("rolebots: order sweep update %d: %v", o.OrderID, err)
			}
		}
		if tune, ok := tuneOf(o.CharID); ok {
			m.nudgeOrder(ctx, o, tune)
		}
	}
}

// orderNeedsWrite reports whether the registry row is behind the live order. A fill moves LiveVol and a reprice
// moves LivePrice, so those are written; lastSeenAt is not refreshed since nothing reads it.
func orderNeedsWrite(o orderRow) bool {
	return o.LiveVol != o.Booked || o.LivePrice != o.Price
}

// bookVanished handles an order whose row is gone: expired, or cancelled by an operator. The core returned the
// unsold goods to the hangar, so for an ask they are inventory again and the planner is told where they are; for
// a bid the escrow went back to the wallet and there is nothing to collect.
func (m *Mod) bookVanished(ctx context.Context, o orderRow) {
	note := "the order left the market before it sold — goods are back in the hangar"
	if o.Bid {
		note = "the standing bid left the market — escrow refunded"
	} else if o.Booked > 0 {
		m.addStock(ctx, o.CharID, o.StationID, o.SystemID, o.TypeID, o.TypeName,
			o.Booked, o.UnitCost)
	}
	m.bookTrade(ctx, tradeEntry{
		CharID: o.CharID, Kind: tradeKindCancel, TypeID: o.TypeID, TypeName: o.TypeName,
		Qty: o.Booked, Price: o.Price, Value: 0,
		Station: o.StationID, System: o.SystemID, OrderID: o.OrderID, Note: note,
	})
	m.closeOrder(ctx, o)
}

// bookFill records units taken off one of our resting orders.
func (m *Mod) bookFill(ctx context.Context, o orderRow, qty int64) {
	value := o.Price * float64(qty)
	profit := 0.0
	note := resaleNoteBidFill
	// A standing bid that fills into trade stock bought goods to sell on.
	resale := false
	if !o.Bid {
		// Net of sales tax (tradefees.go): the wallet received less than o.Price. The broker fee was booked as its own
		// line when the order was placed, and this fill may be one of several.
		profit = m.netSaleAt(ctx, o.CharID, o.StationID, o.Price*float64(qty)) -
			o.UnitCost*float64(qty)
		note = "somebody bought off its listing"
		if o.Kind == tradeDealBuild {
			// A factory's own output: the goods were never trade stock, so takeStock must not run. The sale is the build
			// book's only evidence that the line is worth restocking.
			note = "somebody bought its factory's output"
			m.noteShelfSold(ctx, o.CharID, o.StationID, o.TypeID, qty)
		} else {
			m.takeStock(ctx, o.CharID, o.StationID, o.TypeID, qty)
		}
	} else if o.Kind == tradeDealSupply {
		// A factory's bid: the goods are a bill of materials, not merchandise. industryShortfall reads them from the hangar
		// (industrybids.go); booking them as trade stock would put the pilot's hauler on them.
		note = "somebody filled its supply bid — materials are in the factory hangar"
	} else {
		// A bid that filled leaves goods in that station's hangar: inventory bought to be sold on, which the planner
		// collects (tradeStockPlan).
		m.addStock(ctx, o.CharID, o.StationID, o.SystemID, o.TypeID, o.TypeName, qty, o.Price)
		resale = true
	}
	m.bookTrade(ctx, tradeEntry{
		CharID: o.CharID, Kind: tradeKindFill, TypeID: o.TypeID, TypeName: o.TypeName,
		Qty: qty, Price: o.Price, Value: value, Profit: profit,
		Station: o.StationID, System: o.SystemID, OrderID: o.OrderID, Note: note,
		Resale: resale,
	})
	side := "listing"
	if o.Bid {
		side = "standing bid"
	}
	m.logBot(o.CharID, logInfo, "market",
		"%d× %s off its %s at %.2f (%+.0f ISK)", qty, o.TypeName, side, o.Price, profit)
}

func (m *Mod) closeOrder(ctx context.Context, o orderRow) {
	if _, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_orders SET volRemaining = 0, closedAt = ? WHERE orderID = ?`,
		clock.Unix(), o.OrderID); err != nil {
		rlogf("rolebots: close order %d: %v", o.OrderID, err)
	}
}

// nudgeBid is the upkeep on a standing BUY order that has been outbid. bidPriceFor places a bid only at the front of
// the book, but the book moves afterwards (a rival outbids, or the NPC seeder's reprice tick walks its bid up), and a
// beaten bid is dead escrow for fourteen days. It is pulled, not repriced: repricing needs the outlet's net take and
// the pilot's margin, which are not on the order row, and the planner re-bids next pass from the live book.
func (m *Mod) nudgeBid(ctx context.Context, o orderRow, age int64) {
	var rival sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, `
		SELECT MAX(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1
		  AND volRemaining > 0 AND charID <> ?`,
		o.StationID, o.TypeID, o.CharID).Scan(&rival); err != nil {
		return
	}
	// Still at the front of the book: patience, so it is left alone.
	if !rival.Valid || rival.Float64 < o.LivePrice {
		return
	}
	out, err := evedb.CancelMarketOrder(ctx, m.db, o.CharID, o.OrderID)
	if err != nil || out == nil || out.ErrorMsg != "" {
		return
	}
	m.bookTrade(ctx, tradeEntry{
		CharID: o.CharID, Kind: tradeKindCancel, TypeID: o.TypeID, TypeName: o.TypeName,
		Qty: o.LiveVol, Price: o.LivePrice, Value: 0, Station: o.StationID, System: o.SystemID,
		OrderID: o.OrderID, Note: "outbid — pulled the standing bid rather than hold the escrow",
	})
	m.logBot(o.CharID, logInfo, "market",
		"pulled its %s bid at station %d — %.2f was outbid at %.2f and had been resting %s "+
			"behind it with the escrow tied up",
		o.TypeName, o.StationID, o.LivePrice, rival.Float64, agoString(age))
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_orders SET closedAt = ?, volRemaining = 0 WHERE orderID = ?`,
		clock.Unix(), o.OrderID)
}

// bidStaleFloorFrac is the share of a type's value (evedb.TypeValueOf) a
// trader's speculative standing bid must be worth to stay on the book.
const bidStaleFloorFrac = 0.5

// bidUnderValue is the pure test behind pullUnderValueBid. An unknown value
// (0) never condemns a bid.
func bidUnderValue(price, value float64) bool {
	return value > 0 && price < bidStaleFloorFrac*value
}

// sweepsUnderValue reports whether an order is one pullUnderValueBid judges: speculative standing bids (kind stock)
// only. Gear bids (tradeDealGear) are excluded: they are discounted to 0.45 of the pilot's regional ask by design
// (gearbids.go), so they would always read under half and be pulled and re-placed in a loop. gearBidSane sets their
// floor at placement, and nudgeBid still pulls one a rival outbids.
func sweepsUnderValue(kind string, bid bool) bool {
	return bid && kind == tradeDealStock
}

// pullUnderValueBid cancels a speculative standing bid (kind stock) priced under bidStaleFloorFrac of what the goods
// are worth, and reports whether it did. Such bids are residue of bidOutlets pricing bids off one another, so the
// chain only ever ran down; the outlet rule stops new links and this clears the old ones, returning the escrow.
func (m *Mod) pullUnderValueBid(ctx context.Context, o orderRow, age int64) bool {
	value := evedb.TypeValueOf(ctx, m.db, o.TypeID).Value
	if !bidUnderValue(o.LivePrice, value) {
		return false
	}
	out, err := evedb.CancelMarketOrder(ctx, m.db, o.CharID, o.OrderID)
	if err != nil || out == nil || out.ErrorMsg != "" {
		return false
	}
	m.bookTrade(ctx, tradeEntry{
		CharID: o.CharID, Kind: tradeKindCancel, TypeID: o.TypeID, TypeName: o.TypeName,
		Qty: o.LiveVol, Price: o.LivePrice, Value: 0, Station: o.StationID, System: o.SystemID,
		OrderID: o.OrderID, Note: "under value — pulled a standing bid priced at a sliver of what the goods are worth",
	})
	m.logBot(o.CharID, logInfo, "market",
		"pulled its %s bid at station %d — %.2f is under half the %.2f the goods are worth, after resting %s",
		o.TypeName, o.StationID, o.LivePrice, value, agoString(age))
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_orders SET closedAt = ?, volRemaining = 0 WHERE orderID = ?`,
		clock.Unix(), o.OrderID)
	return true
}

// orderPatience is how long a resting order is left alone before its owner reconsiders it, in seconds. An unhurried
// pilot leaves an ask for the best part of a day, a hurried one reprices inside an hour. It reuses DwellMul, so
// patience means one thing across the module.
func orderPatience(tn Tuning) int64 {
	return tn.dwell(6 * 3600)
}

// nudgeOverBidMargin is how far over a standing local bid an ask with no cost
// basis may be cut to. Just over the bid: under it the goods should go to the
// bid, not wait beside it.
const nudgeOverBidMargin = 1.02

// nudgeOrder is the upkeep on an order that has not moved: undercut the competition if there is any, and pull the
// order once it is plainly not going to sell. A pilot will not sell below cost plus a slice of its asking margin, so a
// greedy pilot gives up less ground than a content one.
func (m *Mod) nudgeOrder(ctx context.Context, o orderRow, tune Tuning) {
	age := clock.Unix() - o.PlacedAt
	patience := orderPatience(tune)
	if o.Kind == tradeDealBPO {
		patience *= bpoListPatienceMul
	}
	if !o.Bid && boughtCheap(o.UnitCost, m.usualPriceAt(ctx, o.StationID, o.TypeID)) {
		patience *= holdPatienceMul
	}

	// An ask already under the NPC band is repaired whatever its age: every sweep it waits lets a buyer take the goods
	// at a fraction of what the NPC beside it charges (npcanchor.go).
	npcFloor := 0.0
	if !o.Bid {
		npcFloor = m.anchoredFloor(ctx, o.StationID, o.TypeID)
		if npcFloor > 0 && o.LivePrice < npcFloor {
			m.liftUnderAnchor(ctx, o, tune)
			return
		}
	}

	// A speculative bid priced at a sliver of what the goods are worth is
	// pulled whatever its age, as an ask under the NPC band is lifted above.
	if sweepsUnderValue(o.Kind, o.Bid) && m.pullUnderValueBid(ctx, o, age) {
		return
	}

	if age < patience {
		return
	}
	if o.Bid {
		m.nudgeBid(ctx, o, age)
		return
	}

	// What else is on offer here. Every order of this pilot is excluded, not just this one: otherwise a pilot holding
	// two lines of one commodity in one station reads its own cheapest line as the competition and undercuts itself.
	var rival sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, `
		SELECT MIN(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1
		  AND volRemaining > 0 AND charID <> ?`,
		o.StationID, o.TypeID, o.CharID).Scan(&rival); err != nil {
		return
	}

	// The floor: cost plus the thinnest spread the pilot accepts. With no cost basis (minerals, salvage) it is the type's
	// stable reference (askbook.go), never a share of the order's current price, which would halve every sweep.
	floor := o.UnitCost * (1 + tune.TradeFloorMarginFrac)
	if o.UnitCost <= 0 {
		// The pilot's own reservation price, not one shared by the roster: a shared floor ends every race on a glutted
		// rock or mineral as one tied number (personalAskFloorFrac).
		floor = m.personalFloorPrice(ctx, o.TypeID, tune)
		// And never under a bid already standing on this dock: an ask below it is a sale to that bid with a broker fee
		// and a wait attached, so the order should be pulled, not cut.
		var bid sql.NullFloat64
		_ = m.db.QueryRowContext(ctx, `
			SELECT MAX(price) FROM market_orders
			WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1
			  AND volRemaining > 0`,
			o.StationID, o.TypeID).Scan(&bid)
		if bid.Valid && bid.Float64*nudgeOverBidMargin > floor {
			floor = bid.Float64 * nudgeOverBidMargin
		}
		// Rock is never cut under the floor its own placement priced from (oreAskFloor), 0.75 of what it refines into;
		// under that the miner should haul it to a bid.
		var cat sql.NullInt64
		_ = m.db.QueryRowContext(ctx, `
			SELECT g.categoryID FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID
			WHERE t.typeID = ?`, o.TypeID).Scan(&cat)
		if cat.Valid && cat.Int64 == 25 {
			of := oreAskFloor(bid.Float64, m.stableFloorPrice(ctx, o.TypeID)/askFloorFraction,
				oreAskFloorMul(tune))
			if of > floor {
				floor = of
			}
		}
	}
	// And never under the market's own tick: a type with no cost basis and no basePrice has a stable floor of zero,
	// and the undercut below is multiplicative, so without this it has no bottom.
	if floor < 0.01 {
		floor = 0.01
	}
	// And never under the NPC band (npcanchor.go): a rival bot at 600 ISK beside an NPC at 15,000 is not the market.
	// The competitive line is the pilot's own point in the band; npcFloor above is the repair line every pilot shares.
	if pf := m.anchoredFloorFor(ctx, tune, o.StationID, o.TypeID); pf > floor {
		floor = pf
	}
	// And goods bought cheap are held near the usual price: a rival's crash
	// ask is what they were bought out of, not a price to follow.
	if hf := m.holdFloorAt(ctx, o.UnitCost, o.StationID, o.TypeID, tune); hf > floor {
		floor = hf
	}

	// A rival at this price is competition too, not only one under it. With strict "<" two pilots who placed at the
	// same number never moved again, leaving one tied line until both orders were pulled unsold.
	if rival.Valid && rival.Float64 <= o.LivePrice {
		// Per-bot nudge step derived from MineralMarkup (1.0 to 1.9).
		// Generous bots (1.0) undercut by 2.0%; greedy bots (1.9) by 0.5%.
		nudgeFrac := 0.02 - (tune.MineralMarkup-1.0)/0.9*0.015
		if nudgeFrac < 0.005 {
			nudgeFrac = 0.005
		}
		if nudgeFrac > 0.02 {
			nudgeFrac = 0.02
		}
		// Truncated to the cent: a sub-cent price is not one the market ticks
		// in, and a rival AT the floor then truncates below it and is left be.
		want := float64(int64(rival.Float64*(1-nudgeFrac)*100)) / 100
		if want >= floor && want < o.LivePrice*(1-nudgeFrac*0.5) {
			out, err := evedb.ModifyMarketOrder(ctx, m.db, o.CharID, o.OrderID, want)
			if err != nil || out == nil || out.ErrorMsg != "" {
				return
			}
			m.bookTrade(ctx, tradeEntry{
				CharID: o.CharID, Kind: tradeKindPrice, TypeID: o.TypeID, TypeName: o.TypeName,
				Qty: o.LiveVol, Price: want, Value: 0, Station: o.StationID, System: o.SystemID,
				OrderID: o.OrderID,
				Note:    fmt.Sprintf("undercut a %.2f rival (was %.2f)", rival.Float64, o.LivePrice),
			})
			m.logBot(o.CharID, logInfo, "market",
				"undercut the competition on %s at station %d — %.2f → %.2f",
				o.TypeName, o.StationID, o.LivePrice, want)
			_, _ = m.db.ExecContext(ctx,
				`UPDATE rolebot_orders SET price = ?, placedAt = ? WHERE orderID = ?`,
				want, clock.Unix(), o.OrderID)
			return
		}
	}

	// Three patiences with nothing happening and no room left to move: the goods go back in the hangar, where the
	// planner finds them as stock and hauls them somewhere that wants them.
	if age > patience*3 {
		out, err := evedb.CancelMarketOrder(ctx, m.db, o.CharID, o.OrderID)
		if err != nil || out == nil || out.ErrorMsg != "" {
			return
		}
		// CancelMarketOrder put the unsold goods back in that station's hangar. They are inventory again, with the cost
		// they were listed against, and the planner collects them (tradeStockPlan).
		m.addStock(ctx, o.CharID, o.StationID, o.SystemID, o.TypeID, o.TypeName,
			o.LiveVol, o.UnitCost)
		m.bookTrade(ctx, tradeEntry{
			CharID: o.CharID, Kind: tradeKindCancel, TypeID: o.TypeID, TypeName: o.TypeName,
			Qty: o.LiveVol, Price: o.LivePrice, Value: 0, Station: o.StationID, System: o.SystemID,
			OrderID: o.OrderID, Note: "nobody was buying — pulled the listing",
		})
		m.logBot(o.CharID, logInfo, "market",
			"pulled its %s listing at station %d — %d unit(s) unsold after %s",
			o.TypeName, o.StationID, o.LiveVol, agoString(age))
		_, _ = m.db.ExecContext(ctx,
			`UPDATE rolebot_orders SET closedAt = ?, volRemaining = 0 WHERE orderID = ?`,
			clock.Unix(), o.OrderID)
	}
}

// tuneFor resolves one character's personality outside a sim pass: the sweeps run on their own clock and have
// no botRun to read it from.
func (m *Mod) tuneFor(ctx context.Context, charID int64) (Tuning, error) {
	traits, err := getTraits(ctx, m.db, charID)
	if err != nil {
		return defaultTuning(), err
	}
	return resolveTuning(traits), nil
}
