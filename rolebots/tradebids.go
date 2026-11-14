package rolebots

// The buy side: a trader that leaves a standing bid behind it. tradebook.go registers bids, and the sweep books
// their fills into inventory (bookFill, addStock) and refunds their escrow when they vanish (bookVanished).
//
// Without a bid a commodity nobody is selling is invisible to a trader, however badly it is wanted elsewhere. That
// is the shape of this server's mineral supply: no NPC corporation bids for a mineral (mod-rebuild.ps1 step 1g
// patches the seed clause out). A trader with real commerce interest, docked where the roster produces, works out
// what it could resell elsewhere and leaves a bid; when it fills, tradeStockPlan collects the goods and hauls them
// to the outlet that justified the price. Leaving a bid is a station action, not a leg: planning continues.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const (
	// How long a standing bid rests: the same fortnight a speculative listing gets (tradeListDays). What should end an
	// order is its owner deciding it was the wrong idea, not the market's expiry timer.
	bidDays = 14

	// How many standing bids one pilot runs at once. Each escrows its full value and the broker fee at placement
	// (db/marketorders.go), so this caps capital tied up in promises; the slot reserve is a separate, looser limit.
	bidMaxOpen = 3

	// The most of its tradable wallet a pilot will have committed to standing bids at one time. A trader that escrows
	// everything cannot take the sure arbitrage run it finds five minutes later.
	bidWalletFrac = 0.35

	// How far back the supply evidence looks. A station the roster sold at
	// last month is a station the roster sells at.
	bidEvidenceDays = 30

	// How many evidenced types get priced. The evidence query is already narrow (this station's own history), so
	// this is a backstop rather than a ranking cut.
	bidMaxCandidates = 40

	// How far under the local ask a bid may sit. A bid at or over the ask is matched on the spot by PlaceBuyOrder and
	// is simply a purchase at the counter, which is the arbitrage planner's job.
	bidUnderAsk = 0.95

	// What a bid pays over the standing bid it has to beat. A seller takes the best price on the book, so matching the
	// incumbent wins nothing: the whole order fills into the older bid first.
	bidOutbidBy = 1.05

	// How many stations per type the outlet scan reads before reachability
	// is applied in Go. Smaller than tradeStationsPerType because this scan
	// runs over a handful of evidenced types rather than the universe.
	bidOutletsPerType = 12

	// bidOutletMaxHops is the furthest outlet a standing bid may price off. A bid sits for days before it fills and its
	// haul is decided at placement, so the trader band reaches further than a bid can afford. It matches the
	// corporation board's reach (corpWorkMaxJumps).
	bidOutletMaxHops = 8
)

// bidPlan is one costed standing bid: what to bid for here, at what price,
// and the outlet elsewhere that justifies it.
type bidPlan struct {
	TypeID   int64
	TypeName string
	UnitVol  float64

	Price float64 // what the pilot bids here
	Units int64

	OutStation int64
	OutSystem  int64
	OutBid     float64 // the bid the pilot intends to sell into
	OutNet     float64 // ...net of that station's sales tax, per unit
	Jumps      int

	LocalAsk float64
	LocalBid float64

	Profit   float64
	Rank     float64
	Evidence string

	// Gear marks a bid priced off the demand ledger (gearbids.go): it has no
	// outlet, and it registers under tradeDealGear.
	Gear bool
}

// kind is the registry kind this bid is filed under.
func (p *bidPlan) kind() string {
	if p.Gear {
		return tradeDealGear
	}
	return tradeDealStock
}

// maybeLeaveStandingBid is the feature from the planner's point of view: called while docked, it places at most one
// bid and never changes what the pilot does next. Errors are logged, not returned: a bid is opportunistic and a
// failure to place one must not fail the trade turn.
func (m *Mod) maybeLeaveStandingBid(ctx context.Context, b *botRun) {
	if b.StationID == 0 || b.ShipID == 0 {
		return
	}
	// The same gate speculation uses: a standing bid is speculation with the legs reversed, capital committed on a price
	// that may not be there on arrival. A pilot with no commerce interest flips existing orders and goes home.
	if tradeInterest(b.Tune) < specMinInterest {
		return
	}
	plan, why, err := m.planStandingBid(ctx, b)
	if err != nil {
		rlogf("rolebots: standing bid char=%d: %v", b.CharID, err)
		return
	}
	if plan == nil {
		if why != "" {
			m.logBot(b.CharID, logInfo, "trade", "not leaving a bid here — %s", why)
		}
		return
	}
	m.placeStandingBid(ctx, b, plan)
}

// planStandingBid picks the bid, or explains why there is none. The prose is returned rather than logged so the
// caller decides whether it is worth saying; "nothing the roster sells here resells anywhere" is not news every dock.
func (m *Mod) planStandingBid(ctx context.Context, b *botRun) (*bidPlan, string, error) {
	free := m.freeOrderSlots(ctx, b.CharID)
	if free <= 0 {
		return nil, "every order slot is committed", nil
	}
	open, escrowed, err := m.openBidExposure(ctx, b.CharID)
	if err != nil {
		return nil, "", err
	}
	if open >= bidMaxOpen {
		return nil, fmt.Sprintf("it already has %d standing bid(s) out", open), nil
	}

	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return nil, "", fmt.Errorf("wallet: %w", err)
	}
	// The escrow ceiling counts what is already committed, so three bids in a row cannot each spend a third of the
	// wallet the last one tied up.
	budget := balance*(1-tradeWalletReserveFrac)*bidWalletFrac - escrowed
	if budget < tradeMinProfitISK {
		return nil, "its wallet is already as committed as it is willing to be", nil
	}

	types, err := m.bidCandidateTypes(ctx, b)
	if err != nil {
		return nil, "", err
	}
	if len(types) == 0 {
		return nil, "", nil // nothing the roster is known to sell here
	}

	local, err := m.localBook(ctx, b.StationID, types)
	if err != nil {
		return nil, "", err
	}
	// A standing bid is priced off the outlets the pilot would carry the goods to, so it runs at the trader band
	// (residency.go). No ladder: a pass that finds no outlet worth pricing against simply does not bid.
	r := m.economicRules(ctx, b, bandAdjacent)
	jumps, err := m.reachFrom(ctx, b.SystemID, r)
	if err != nil {
		return nil, "", err
	}
	outlets, err := m.bidOutlets(ctx, b, types, r)
	if err != nil {
		return nil, "", err
	}

	hold, err := m.holdFreeM3(ctx, b.CharID, b.ShipID)
	if err != nil || hold <= 0 {
		return nil, "it has no free hold to bring anything home in", nil
	}
	var best *bidPlan
	for _, t := range types {
		for _, o := range outlets[t.TypeID] {
			hops, ok := jumps[o.SystemID]
			if !ok || hops > bidOutletMaxHops {
				continue // the pilot will not fly to the outlet it is pricing off
			}
			p := m.costStandingBid(ctx, b, t, local[t.TypeID], o, hops, budget, hold)
			if p == nil {
				continue
			}
			p.Rank = p.Profit / float64(1+hops)
			if best == nil || p.Rank > best.Rank {
				best = p
			}
		}
	}
	if best == nil {
		// Nothing has an outlet. For modules and ammunition no bid exists anywhere, so bidOutlets never finds one; gearbids.go
		// is the one arm allowed to price against the roster's own demand ledger instead of a live bid.
		gear, gerr := m.planGearBid(ctx, b, types, local, outlets, budget, hold)
		if gerr != nil {
			return nil, "", gerr
		}
		if gear == nil {
			return nil, "", nil
		}
		return gear, "", nil
	}
	return best, "", nil
}

// costStandingBid turns one (type, outlet) pair into a priced bid, or nil when the arithmetic does not support one.
// The price is built down from the outlet, not up from the local book: what can I pay here and still make the spread
// I want selling it there. Everything else is a constraint on that number.
func (m *Mod) costStandingBid(ctx context.Context, b *botRun, t bidType, lb localQuote,
	o bidOutlet, hops int, budget, hold float64) *bidPlan {

	// What the outlet really pays, net of its own sales tax. Using the gross
	// bid here is how a trader books a margin it never keeps (tradefees.go).
	net := m.marketRates(ctx, b.CharID, o.StationID).netSale(o.Price)
	price, ok := bidPriceFor(net, b.Tune.TradeMinMarginFrac, lb)
	if !ok {
		return nil
	}

	units := int64(budget / price)
	if t.UnitVol > 0 {
		if byHold := int64(hold / t.UnitVol); byHold < units {
			units = byHold
		}
	}
	if o.Volume < units {
		units = o.Volume
	}
	if !tradeCargoOK(t.CategoryID, units) {
		return nil
	}

	value := price * float64(units)
	fee := m.marketRates(ctx, b.CharID, b.StationID).brokerFee(value)
	profit := (net-price)*float64(units) - fee
	if profit < tradeMinProfitISK {
		return nil
	}
	return &bidPlan{
		TypeID: t.TypeID, TypeName: t.TypeName, UnitVol: t.UnitVol,
		Price: price, Units: units,
		OutStation: o.StationID, OutSystem: o.SystemID, OutBid: o.Price, OutNet: net,
		Jumps:    hops,
		LocalAsk: lb.Ask, LocalBid: lb.Bid,
		Profit:   profit,
		Evidence: t.Evidence,
	}
}

// placeStandingBid commits it: the order, the registry row the sweep reads,
// the ledger, and the broker fee.
func (m *Mod) placeStandingBid(ctx context.Context, b *botRun, p *bidPlan) {
	out, err := evedb.PlaceBuyOrder(ctx, m.db, b.CharID, b.StationID, p.TypeID,
		p.Price, p.Units, -1 /*station range*/, 1, bidDays, false)
	if err != nil {
		rlogf("rolebots: place standing bid char=%d type=%d: %v", b.CharID, p.TypeID, err)
		return
	}
	if out.ErrorMsg != "" {
		m.logBot(b.CharID, logWarn, "trade",
			"standing bid refused (%s) — %d× %s at %.2f", out.ErrorMsg, p.Units, p.TypeName, p.Price)
		return
	}

	// A bid priced under the local ask should not fill on placement, but the book can move between pricing and
	// placing. Anything that did fill is an ordinary purchase and lands in the hangar as inventory.
	if out.FilledQty > 0 {
		var spent float64
		for _, f := range out.Fills {
			spent += f.Price * float64(f.Quantity)
		}
		unit := spent / float64(out.FilledQty)
		e := tradeEntry{
			CharID: b.CharID, Kind: tradeKindBuy, TypeID: p.TypeID, TypeName: p.TypeName,
			Qty: out.FilledQty, Price: unit, Value: spent,
			Station: b.StationID, System: b.SystemID,
			Note:   resaleNoteSpotBid + " — bought on the spot",
			Resale: true,
		}
		m.bookTrade(ctx, e)
		m.notePeerFills(ctx, e, out.Fills, true, "bid")
		m.addStock(ctx, b.CharID, b.StationID, b.SystemID, p.TypeID, p.TypeName, out.FilledQty, unit)
	}

	resting := p.Units - out.FilledQty
	if resting <= 0 || out.RestingOrderID == 0 {
		return
	}
	// unitCost on a bid is what the goods will cost when somebody hits it, which is the bid price. The sweep hands it to
	// addStock as the cost basis (bookFill), and tradeStockPlan's outlet search refuses to sell under it.
	m.registerOrder(ctx, b.CharID, out.RestingOrderID, p.TypeID, b.StationID, b.SystemID,
		p.TypeName, true, p.Price, p.Price, resting, p.kind())
	// A gear bid has no outlet: what it fills goes on the market from here (tradeStockPlan), so the note must not say it
	// resells into a 0.00 bid at station 0.
	note := fmt.Sprintf("standing bid — %s, resells at %.2f in %s",
		p.Evidence, p.OutBid, m.nameOfSystem(ctx, p.OutSystem))
	if p.Gear {
		note = fmt.Sprintf("standing bid — %s, to list what it buys", p.Evidence)
	}
	m.bookTrade(ctx, tradeEntry{
		CharID: b.CharID, Kind: tradeKindList, TypeID: p.TypeID, TypeName: p.TypeName,
		Qty: resting, Price: p.Price, Value: p.Price * float64(resting),
		Station: b.StationID, System: b.SystemID, OrderID: out.RestingOrderID,
		Note: note,
	})
	m.bookBrokerFee(ctx, b, out.RestingOrderID, p.TypeID, p.TypeName, p.Price, resting)
	if p.Gear {
		m.logBot(b.CharID, logInfo, "trade",
			"left a standing bid at %s — %d× %s at %.2f (%s), to list whatever it buys",
			m.stationLabel(ctx, b.StationID), resting, p.TypeName, p.Price, p.Evidence)
		return
	}
	m.logBot(b.CharID, logInfo, "trade",
		"left a standing bid at %s — %d× %s at %.2f (%s), to resell into a %.2f bid at %s, %d jump(s): ~%.0f ISK if it fills",
		m.stationLabel(ctx, b.StationID), resting, p.TypeName, p.Price, p.Evidence,
		p.OutBid, m.stationLabel(ctx, p.OutStation), p.Jumps, p.Profit)
}

// bidPriceFor is the whole pricing rule, built down from the outlet. net is what the outlet pays per unit after its
// sales tax. Rules in order: pay at most net/(1+margin); stay under the local ask; beat the local bid or place no bid.
// See docs/comment-notes-b19.md#bid-price. Pure, so the harness can pin it without a market.
func bidPriceFor(net, margin float64, lb localQuote) (float64, bool) {
	if net <= 0 {
		return 0, false
	}
	price := net / (1 + margin)
	if lb.Ask > 0 && price > lb.Ask*bidUnderAsk {
		price = lb.Ask * bidUnderAsk
	}
	if lb.Bid > 0 {
		want := lb.Bid * bidOutbidBy
		if want > price {
			return 0, false
		}
		price = want
	}
	price = roundISK(price)
	// Rounding is the last thing that touches the price, so the invariants are re-checked on the number going on the
	// book: a bid that rounded up onto the ask fills instantly, and one past the outlet's net take hauls at a loss.
	if price <= 0 || price >= net {
		return 0, false
	}
	if lb.Ask > 0 && price >= lb.Ask {
		return 0, false
	}
	if lb.Bid > 0 && price <= lb.Bid {
		return 0, false
	}
	return price, true
}

// ── evidence ────────────────────────────────────────────────────────────────

// bidType is one candidate commodity and why the pilot thinks it can buy it
// here.
type bidType struct {
	TypeID     int64
	TypeName   string
	UnitVol    float64
	CategoryID int64
	Evidence   string
}

// bidCandidateTypes is the supply question: what does the roster actually sell at this station? Two sources: the
// mineral hold (a miner standing here accumulating a lot, mineralbatch.go) and the trade book (goods listed or sold
// here before). It is not the market's ask book, since an ask is something the arbitrage planner can simply buy.
// A type this pilot already has a bid out for is excluded.
func (m *Mod) bidCandidateTypes(ctx context.Context, b *botRun) ([]bidType, error) {
	// The trade-book half comes from a per-station snapshot shared by every pilot docked there (stationSellers). Once the
	// market history covers its window (mktdaily.go) it answers instead: what sellers actually sold at this dock in a week.
	var sold []int64
	src := "the roster sells it here"
	if hist := m.marketHistory(ctx); hist.Ready {
		sold, src = stationSoldTypes(hist, b.StationID, bidMaxCandidates), "sellers sell it here"
	} else {
		sellers, err := m.stationSellers(ctx, b.StationID)
		if err != nil {
			return nil, fmt.Errorf("bid candidates: %w", err)
		}
		for typeID, s := range sellers {
			if s.otherThan(b.CharID) {
				sold = append(sold, typeID)
			}
		}
	}
	soldClause := ""
	if len(sold) > 0 {
		soldClause = `
			 UNION
			SELECT typeID, '` + src + `' AS src
			  FROM invTypes WHERE typeID IN (` + placeholders(len(sold)) + `)`
	}
	args := []any{b.StationID, b.CharID}
	args = append(args, int64Args(sold)...)
	args = append(args, b.CharID, b.StationID, bidMaxCandidates)
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, COALESCE(t.volume, 0.01), COALESCE(g.categoryID, 0), s.src
		  FROM (
			SELECT typeID, 'a miner is stockpiling it here' AS src
			  FROM rolebot_mineral_hold
			 WHERE stationID = ? AND characterID <> ?`+soldClause+`
		  ) s
		  JOIN invTypes t ON t.typeID = s.typeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE s.typeID NOT IN (
			SELECT typeID FROM rolebot_orders
			 WHERE characterID = ? AND stationID = ? AND bid = 1 AND closedAt = 0)
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("bid candidates: %w", err)
	}
	defer rows.Close()

	// The union can hand back both reasons for one type; the mineral hold wins because it is about now.
	seen := map[int64]int{}
	var out []bidType
	for rows.Next() {
		var t bidType
		if err := rows.Scan(&t.TypeID, &t.TypeName, &t.UnitVol, &t.CategoryID, &t.Evidence); err != nil {
			return nil, err
		}
		if i, ok := seen[t.TypeID]; ok {
			if strings.HasPrefix(t.Evidence, "a miner") {
				out[i].Evidence = t.Evidence
			}
			continue
		}
		seen[t.TypeID] = len(out)
		out = append(out, t)
	}
	return out, rows.Err()
}

// stationSeller is who has listed or sold one type at a station over the
// evidence window: the lowest and highest seller characterID, which is
// enough to answer "anybody but me?" without holding the whole list.
type stationSeller struct{ minChar, maxChar int64 }

func (s stationSeller) otherThan(charID int64) bool {
	return s.minChar != charID || s.maxChar != charID
}

// stationSellersTTL: the window is bidEvidenceDays long; ten minutes of
// staleness at its leading edge changes nothing a bid decides.
const stationSellersTTL = 10 * time.Minute

// stationSellers is the trade-book half of bidCandidateTypes for one station, shared by every pilot docked there.
// Read-only once shared. The WHERE and selected columns are exactly idx_rbtrade_station_kind (tables.go), an index-only
// read; adding a column to either sends it back to a clustered-row fetch per leg.
func (m *Mod) stationSellers(ctx context.Context, stationID int64) (map[int64]stationSeller, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshotFor(snapKey("station-sellers", stationID), stationSellersTTL,
		func() (map[int64]stationSeller, error) {
			rows, err := m.db.QueryContext(sctx, capStatement(`
				SELECT typeID, MIN(characterID), MAX(characterID)
				  FROM rolebot_trades
				 WHERE stationID = ? AND ts >= ? AND kind IN (?, ?)
				 GROUP BY typeID`),
				stationID, clock.Unix()-int64(bidEvidenceDays)*86400, tradeKindList, tradeKindSell)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := map[int64]stationSeller{}
			for rows.Next() {
				var typeID int64
				var s stationSeller
				if err := rows.Scan(&typeID, &s.minChar, &s.maxChar); err != nil {
					return nil, err
				}
				out[typeID] = s
			}
			return out, rows.Err()
		})
}

// ── the two books ───────────────────────────────────────────────────────────

// localQuote is what the station the pilot is standing in already offers for one type. Either side may be absent,
// and absent is not zero-priced: it is the ordinary case for a mineral on this server, and the case a bid fills.
type localQuote struct {
	Ask float64
	Bid float64
}

func (m *Mod) localBook(ctx context.Context, stationID int64, types []bidType) (map[int64]localQuote, error) {
	ids := make([]int64, 0, len(types))
	for _, t := range types {
		ids = append(ids, t.TypeID)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID,
		       MIN(CASE WHEN bid = 0 THEN price END),
		       MAX(CASE WHEN bid = 1 THEN price END)
		  FROM market_orders
		 WHERE stationID = ? AND orderState = 1 AND volRemaining > 0
		   AND typeID IN (`+placeholders(len(ids))+`)
		 GROUP BY typeID`,
		append([]any{stationID}, int64Args(ids)...)...)
	if err != nil {
		return nil, fmt.Errorf("local book: %w", err)
	}
	defer rows.Close()
	out := map[int64]localQuote{}
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

// bidOutlet is somewhere else that is already paying for the goods.
type bidOutlet struct {
	StationID int64
	SystemID  int64
	Price     float64
	Volume    int64
}

// bidOutlets reads the best bids elsewhere for the candidate types. The pilot quotes a price somebody already pays,
// discounted by its own spread and the tax it lands at. Its own orders are excluded, and so is every other trader's
// standing bid (kinds stock and gear): a speculative bid is a reseller, and pricing off one chains guesses downward.
// NPC seeder bids, factory supply bids and players' bids stay outlets.
func (m *Mod) bidOutlets(ctx context.Context, b *botRun, types []bidType, r travelRules) (map[int64][]bidOutlet, error) {
	ids := make([]int64, 0, len(types))
	for _, t := range types {
		ids = append(ids, t.TypeID)
	}
	args := []any{b.CharID, b.StationID}
	args = append(args, int64Args(ids)...)
	args = append(args, r.SecFloor)

	rows, err := m.db.QueryContext(ctx, `
		WITH best AS (
			SELECT o.typeID, o.stationID, o.solarSystemID,
			       MAX(o.price) AS price, SUM(o.volRemaining) AS vol,
			       ROW_NUMBER() OVER (PARTITION BY o.typeID ORDER BY MAX(o.price) DESC) AS rn
			  FROM market_orders o
			  JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
			  LEFT JOIN rolebot_orders ro ON ro.orderID = o.orderID AND ro.closedAt = 0
			 WHERE o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
			   AND o.charID <> ? AND o.stationID <> ?
			   AND (ro.kind IS NULL OR ro.kind NOT IN ('`+tradeDealStock+`', '`+tradeDealGear+`'))
			   AND o.typeID IN (`+placeholders(len(ids))+`)
			   AND ROUND(ms.security, 1) >= ?
			 GROUP BY o.typeID, o.stationID, o.solarSystemID
		)
		SELECT typeID, stationID, solarSystemID, price, vol
		  FROM best WHERE rn <= `+fmt.Sprint(bidOutletsPerType), args...)
	if err != nil {
		return nil, fmt.Errorf("bid outlets: %w", err)
	}
	defer rows.Close()
	out := map[int64][]bidOutlet{}
	for rows.Next() {
		var typeID int64
		var o bidOutlet
		if err := rows.Scan(&typeID, &o.StationID, &o.SystemID, &o.Price, &o.Volume); err != nil {
			return nil, err
		}
		out[typeID] = append(out[typeID], o)
	}
	return out, rows.Err()
}

// openBidExposure is how many standing bids a pilot has out and what they have tied up, read off this module's own
// registry rather than market_orders.escrow, since the sweep reconciles the registry.
func (m *Mod) openBidExposure(ctx context.Context, charID int64) (int64, float64, error) {
	var n int64
	var escrow sql.NullFloat64
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(o.price * o.volRemaining)
		  FROM rolebot_orders o
		  JOIN market_orders mo ON mo.orderID = o.orderID AND mo.orderState = 1
		 WHERE o.characterID = ? AND o.bid = 1 AND o.closedAt = 0`, charID).Scan(&n, &escrow)
	if err != nil {
		return 0, 0, fmt.Errorf("open bids: %w", err)
	}
	return n, escrow.Float64, nil
}

// roundISK snaps a price to the hundredth the client and the matching
// engine both work in. PlaceBuyOrder's own comparisons carry a half-cent
// tolerance; a price with more precision than that quietly fails to be the
// number the book compares against.
func roundISK(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
