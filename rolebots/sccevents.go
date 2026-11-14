//go:build scc

package rolebots

// Bots and the SCC's material events: the fast half of the exchange. The core
// files material events (db/sccevents.go: a lost fleet, a war, an outpost) with a
// severity and halts a stock when one is grave. Every valuation carries the
// stock's shock (sccView, sccSentiment). A fast cohort reacts as the halt lifts:
// makers pull quotes at the halt and requote wider after it, the stock's bot
// holders decide whether to sell or add, and a sample of opportunists runs the
// ordinary review. Momentum and careful holders guard positions with stops
// (sccStopPolicy), so a sell-off can cascade within the day's band. Reactions are
// limited to one per stock per 15 minutes and 30 an hour.

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

const (
	sccReactCorpGapSec    = 900 // one reaction per stock per this
	sccReactPerHour       = 30  // and this many in all
	sccReactHolders       = 200 // the largest bot holders consulted
	sccReactOpportunists  = 40  // pilots with cash who look at the stock
	sccReactJitterSec     = 100 // spread on top of the halt, so not everyone moves in one second
	sccReactMakerShare    = 0.5 // of the makers requote a stock with news
	sccReactPlacementsCap = 60  // orders one reaction may place in all
)

var (
	sccEventsReacted atomic.Int64
	sccEventsSkipped atomic.Int64
	sccReactSells    atomic.Int64
	sccReactAdds     atomic.Int64
	sccReactOpps     atomic.Int64
	sccReactQuotes   atomic.Int64
	sccStopsArmed    atomic.Int64
	sccStopsDisarmed atomic.Int64

	sccReactMu     sync.Mutex
	sccReactLast   = map[int64]int64{} // corpID → sim time of the last reaction
	sccReactWindow []int64             // sim times of the reactions in the last hour
)

// sccReactAllowed is the rate limit: true takes a slot.
func sccReactAllowed(corpID, now int64) bool {
	sccReactMu.Lock()
	defer sccReactMu.Unlock()
	if now-sccReactLast[corpID] < sccReactCorpGapSec {
		return false
	}
	kept := sccReactWindow[:0]
	for _, at := range sccReactWindow {
		if now-at < 3600 {
			kept = append(kept, at)
		}
	}
	sccReactWindow = kept
	if len(sccReactWindow) >= sccReactPerHour {
		return false
	}
	sccReactWindow = append(sccReactWindow, now)
	sccReactLast[corpID] = now
	return true
}

// onSCCEvent is the subscriber: pull the makers off a halted stock, and
// schedule the reaction for when it reopens.
func (m *Mod) onSCCEvent(ev evedb.SCCEvent) {
	if !Master() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if !m.sccBotsOn(ctx) {
		return
	}
	forgetSCCBoard()
	if ev.HaltUntil > ev.At {
		m.sccPullMakerQuotes(ctx, ev.CorpID)
	}
	if !sccReactAllowed(ev.CorpID, clock.Unix()) {
		sccEventsSkipped.Add(1)
		return
	}
	// The halt is measured on the exchange's own clock (both instants are
	// its stamps), never against the sim's.
	delay := max(0, ev.HaltUntil-ev.At) + 20 + int64(sccRoll(ev.ID, 7)*sccReactJitterSec)
	time.AfterFunc(time.Duration(delay)*time.Second, func() {
		goSafe("scc event reaction", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.sccEventReact(ctx, ev)
		})
	})
}

// sccPullMakerQuotes withdraws every maker quote on a stock: a dealer does
// not leave a price standing into news it has not read.
func (m *Mod) sccPullMakerQuotes(ctx context.Context, corpID int64) {
	open, err := evedb.ListShareOrders(ctx, m.db, corpID, 0)
	if err != nil {
		return
	}
	for _, o := range open {
		if o.Source == evedb.SCCSourceMaker {
			_, _ = evedb.CancelShareOrder(ctx, m.db, o.OrderID, o.OwnerID)
		}
	}
}

// sccEventReact is the fast cohort's reaction to one event, once the stock
// trades again.
func (m *Mod) sccEventReact(ctx context.Context, ev evedb.SCCEvent) {
	if !m.sccBotsOn(ctx) {
		return
	}
	forgetSCCBoard()
	row, ok := m.sccListed(ctx, ev.CorpID)
	if !ok || row.Halted(evedb.SCCNow()) {
		return // delisted, or halted again meanwhile: the next event reacts
	}
	sccEventsReacted.Add(1)
	rules := m.sccRules(ctx)
	board := m.sccBoard(ctx)
	placed := 0

	// 1. The makers requote it.
	sccMakersMu.Lock()
	makers := append([]int64(nil), sccMakerSet...)
	sccMakersMu.Unlock()
	for _, id := range makers {
		if sccRoll(id, ev.ID) < sccReactMakerShare {
			m.sccQuote(ctx, id, board, rules, ev.CorpID)
			sccReactQuotes.Add(1)
		}
	}

	// 2. Its bot holders.
	recentCut := clock.Unix() - sccMinHoldSec
	holders := m.sccBotHolders(ctx, ev.CorpID)
	for _, id := range holders {
		if placed >= sccReactPlacementsCap {
			break
		}
		traits, _ := getTraits(ctx, m.db, id)
		inv, poles := sccInvestorFor(traits), sccPoles(traits)
		free := evedb.ShareHolding(ctx, m.db, id, ev.CorpID)
		r := sccReactHolder(inv, poles, ev.Severity, sccRoll(id, ev.ID), free)
		b := &botRun{CharID: id}
		switch {
		case r.SellQty > 0:
			if m.sccBoughtSince(ctx, id, recentCut)[ev.CorpID] {
				continue
			}
			n := m.sccSellHolding(ctx, b, row, r.SellQty, r.AtMarket)
			placed++
			sccReactSells.Add(1)
			m.logBot(id, logInfo, "market", "SCC news (%s, %+.2f): sold %d of %d [%s] (%d filled)",
				evedb.SCCEventKindLabel(ev.Kind), ev.Severity, r.SellQty, free, row.Ticker, n)
		case r.Add:
			cash, err := evedb.GetCharBalance(ctx, m.db, id)
			if err != nil || cash <= sccInvestAboveISK {
				continue
			}
			if m.sccBuyAtAsk(ctx, id, row, inv.StockFrac*(cash-sccInvestAboveISK)*sccBuyTranche) {
				placed++
				sccReactAdds.Add(1)
				m.sccAfterBuy(ctx, id, inv, poles, row)
			}
		}
	}

	// 3. Opportunists with cash run their ordinary review on it.
	st := sccBoardStatsOf(board, evedb.SCCNow())
	isHolder := map[int64]bool{}
	for _, id := range holders {
		isHolder[id] = true
	}
	for _, o := range m.sccOpportunists(ctx, sccReactOpportunists) {
		if placed >= sccReactPlacementsCap || isHolder[o.charID] {
			continue
		}
		traits, _ := getTraits(ctx, m.db, o.charID)
		inv := sccInvestorFor(traits)
		if inv.StockFrac <= 0 {
			continue
		}
		b := &botRun{CharID: o.charID, CorpID: o.corpID}
		view := m.sccView(ctx, b, inv, row, st, 0, 0)
		view.StoppedOut = m.sccStoppedOut(ctx, o.charID)[row.CorpID]
		budget := inv.StockFrac * math.Max(0, o.cash-sccInvestAboveISK)
		in, ok := sccReview(inv, view, budget, o.cash)
		if !ok || !in.IsBuy {
			continue
		}
		res, err := evedb.PlaceShareOrder(ctx, m.db, rules.SCCConfig, evedb.SCCPlaceRequest{CorpID: row.CorpID,
			OwnerID: o.charID, IsBuy: true, Qty: in.Qty, Price: in.Price, ExpiryDays: in.ExpiryDays, Source: evedb.SCCSourceBot})
		if err != nil {
			continue
		}
		placed++
		sccReactOpps.Add(1)
		sccBotOrders.Add(1)
		m.logBot(o.charID, logInfo, "market", "SCC news (%s on [%s]): bid %d at %s (%s): %d filled",
			evedb.SCCEventKindLabel(ev.Kind), row.Ticker, in.Qty, iskString(in.Price), in.Why, res.FilledQty())
		if res.FilledQty() > 0 {
			m.sccAfterBuy(ctx, o.charID, inv, sccPoles(traits), row)
		}
	}
}

// sccBotHolders is the stock's largest holders that are bots, at most
// sccReactHolders of them.
func (m *Mod) sccBotHolders(ctx context.Context, corpID int64) []int64 {
	all, err := evedb.BeneficialShareHolders(ctx, m.db, corpID)
	if err != nil || len(all) == 0 {
		return nil
	}
	var ids []string
	for i, h := range all {
		if i >= sccReactHolders*2 {
			break
		}
		if h.HolderID != corpID {
			ids = append(ids, strconv.FormatInt(h.HolderID, 10))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `SELECT characterID FROM rolebot_generated WHERE characterID IN (`+
		strings.Join(ids, ",")+`)`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	bot := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			bot[id] = true
		}
	}
	var out []int64
	for _, h := range all { // keep the register's largest-first order
		if bot[h.HolderID] && len(out) < sccReactHolders {
			out = append(out, h.HolderID)
		}
	}
	return out
}

type sccOpportunist struct {
	charID, corpID int64
	cash           float64
}

// sccOpportunists samples pilots with money to put to work.
func (m *Mod) sccOpportunists(ctx context.Context, n int) []sccOpportunist {
	rows, err := m.db.QueryContext(ctx, `SELECT g.characterID, c.corporationID, c.balance FROM rolebot_generated g
		JOIN character_ c ON c.characterID = g.characterID WHERE c.balance > ? ORDER BY RAND() LIMIT ?`,
		2*sccInvestAboveISK, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []sccOpportunist
	for rows.Next() {
		var o sccOpportunist
		if rows.Scan(&o.charID, &o.corpID, &o.cash) == nil {
			out = append(out, o)
		}
	}
	return out
}

// sccBuyAtAsk spends up to budget on a stock at its best ask (within the
// band), resting a day if nothing fills. Reports whether an order went in.
func (m *Mod) sccBuyAtAsk(ctx context.Context, charID int64, row evedb.SCCBoardRow, budget float64) bool {
	rules := m.sccRules(ctx)
	band := evedb.SCCPriceBand(row.Reference(), rules.BandPct)
	_, asks, _ := evedb.SCCDepth(ctx, m.db, row.CorpID, 1)
	price := row.Price
	if len(asks) > 0 {
		price = asks[0].Price
	}
	price = sccClampBand(math.Ceil(price), band)
	qty := int64(budget / (price * (1 + rules.BrokerFee)))
	if qty <= 0 {
		return false
	}
	if _, err := evedb.PlaceShareOrder(ctx, m.db, rules.SCCConfig, evedb.SCCPlaceRequest{CorpID: row.CorpID, OwnerID: charID,
		IsBuy: true, Qty: qty, Price: price, ExpiryDays: 1, Source: evedb.SCCSourceBot}); err != nil {
		return false
	}
	sccBotOrders.Add(1)
	return true
}

// ── stops ────────────────────────────────────────────────────────────────────

// sccArmedStops is a pilot's armed stops by listing.
func (m *Mod) sccArmedStops(ctx context.Context, charID int64) map[int64]*evedb.SCCStop {
	out := map[int64]*evedb.SCCStop{}
	stops, err := evedb.ListShareStops(ctx, m.db, 0, charID, true, 50)
	if err != nil {
		return out
	}
	for i := range stops {
		if stops[i].Source == evedb.SCCSourceBot {
			out[stops[i].CorpID] = &stops[i]
		}
	}
	return out
}

// sccStoppedOut is every listing a stop of the pilot's fired on lately.
func (m *Mod) sccStoppedOut(ctx context.Context, charID int64) map[int64]bool {
	out := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `SELECT DISTINCT corpID FROM sccStops
		WHERE ownerID = ? AND state = ? AND triggeredAt >= ?`, charID, evedb.SCCStopTriggered, evedb.SCCNow()-sccStopReentrySec)
	if err != nil {
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

// sccAfterBuy guards a fresh position with the pilot's stop.
func (m *Mod) sccAfterBuy(ctx context.Context, charID int64, inv sccInvestor, poles sccTraitPoles, row evedb.SCCBoardRow) {
	held := evedb.ShareHolding(ctx, m.db, charID, row.CorpID)
	var cost float64
	if pos, err := evedb.SCCPositions(ctx, m.db, charID); err == nil {
		for _, p := range pos {
			if p.CorpID == row.CorpID && p.Shares > 0 {
				cost = p.Cost / float64(p.Shares)
			}
		}
	}
	m.sccKeepStop(ctx, charID, inv, poles, row, held, cost, m.sccArmedStops(ctx, charID)[row.CorpID])
}

// sccKeepStop brings one holding's stop in line with the pilot's policy:
// armed when wanted, re-armed when the holding has moved by a fifth, kind or
// trail changed, disarmed when the holding is gone. Stops cost nothing to
// hold, but every re-arm is a write, so a stop within tolerance is kept.
func (m *Mod) sccKeepStop(ctx context.Context, charID int64, inv sccInvestor, poles sccTraitPoles,
	row evedb.SCCBoardRow, held int64, cost float64, cur *evedb.SCCStop) {
	plan := sccStopPolicy(inv, poles)
	disarm := func() {
		if cur != nil && evedb.CancelShareStop(ctx, m.db, cur.StopID, charID) == nil {
			sccStopsDisarmed.Add(1)
		}
	}
	if held <= 0 || plan.Kind == sccStopNone {
		disarm()
		return
	}
	if cur != nil {
		sameKind := (plan.Kind == sccStopTrail) == (cur.TrailPct > 0)
		if sameKind && math.Abs(float64(cur.Qty-held)) <= 0.2*float64(held) {
			return
		}
	}
	req, ok := sccStopRequest(plan, row.CorpID, charID, held, cost, row.Price)
	if !ok {
		return // keep whatever stands: a fixed floor already crossed is not re-armed lower
	}
	if cur == nil {
		var armed int
		_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sccStops WHERE ownerID = ? AND state = 0`, charID).Scan(&armed)
		if armed >= sccMaxBotStops {
			return
		}
	}
	disarm()
	if _, err := evedb.PlaceShareStop(ctx, m.db, req); err == nil {
		sccStopsArmed.Add(1)
	}
}

func init() {
	registerCorpForget("sccReactLast", func(id int64) {
		sccReactMu.Lock()
		delete(sccReactLast, id)
		sccReactMu.Unlock()
	})
}
