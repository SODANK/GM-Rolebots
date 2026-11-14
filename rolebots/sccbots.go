//go:build scc

package rolebots

// Bots on the SCC Exchange. The exchange is the core's (db/sccengine.go,
// sccexchange.go, scclisting.go, sccfilings.go); the roster takes part by calling
// the Go engine directly. Listing CEO: a bot CEO weighs listing its corporation
// on every think (ceoShares), and the first think on which it qualifies
// (rolebot_scc_ceo) applies outright. Treasury: a listed managed corporation keeps
// one offering and one buy-back bid on the book, clamped into the day's band.
// Investors review a few listings on the visit home (sccinvest.go), makers quote
// the richest greedy traders' listings, and material events reach valuations as
// shocks (sccevents.go). scc.botsEnabled turns all of it off without closing the
// market to pilots.

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

const (
	// sccInvestAboveISK is the cash a pilot keeps before any of it goes into
	// stocks; it is lower than the share desk's investAboveISK.
	sccInvestAboveISK  = 2_000_000.0
	sccRulesTTL        = 60
	sccBoardTTL        = 300
	sccMakerTicks      = 900  // the maker pass; each maker re-quotes at most hourly
	sccMakerRequoteSec = 3600 // per maker
	sccMakerPickSec    = 3600 // the maker set is re-chosen hourly
	sccListCooldownSec = 3 * 86400
	sccCarefulMaxRisk  = 0.08 // BBB
	sccOfferDayFrac    = 0.02 // of the issue per treasury offering
	sccLotteryChance   = 0.05
	sccLotteryMinCash  = 50e6
	sccMaxPlacements   = 2
	// sccMinHoldSec: a pilot never sells a stock it bought in the last day, so the
	// visit home's two buyers (investInCorp, the portfolio review) cannot flip
	// the same shares.
	sccMinHoldSec = 86400

	// The treasury's buy-back programme (sccTreasury).
	sccBuybackDeepFrac     = 0.85 // under this share of book the reserve may be tapped
	sccBuybackReserveFrac  = 0.05 // of the wallet, when it is
	sccBlackoutSec         = 86400
	settingSCCLedgerCursor = "rolebots.scc.ledgerCursor"
)

var (
	sccBootstrapped atomic.Int64 // first-look listings filed since boot
	sccMakerBusy    atomic.Bool

	sccRulesMu   sync.Mutex
	sccRulesMemo evedb.SCCRules
	sccRulesAt   int64

	sccBoardMu   sync.Mutex
	sccBoardMemo []evedb.SCCBoardRow
	sccBoardAt   int64

	sccLastReview sync.Map // charID → unix

	sccMakersMu  sync.Mutex
	sccMakerSet  []int64
	sccMakerAt   int64
	sccMakerLast = map[int64]int64{}

	sccReviewsByStyle [sccStyleCount]atomic.Int64
	sccBotOrders      atomic.Int64
	sccListingsFiled  atomic.Int64
	sccRefiled        atomic.Int64 // listed corporations moved to another sector since boot
)

// startSCCBots installs the hooks, the bootstrap marker and the maker pass.
func (m *Mod) startSCCBots(ctx context.Context) {
	if _, err := m.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS rolebot_scc_conviction (
		characterID BIGINT NOT NULL,
		corpID      BIGINT NOT NULL,
		conviction  DOUBLE NOT NULL,
		week        INT NOT NULL,
		PRIMARY KEY (characterID, corpID)
	) ENGINE=InnoDB`); err != nil {
		rlogf("rolebots: scc conviction table: %v", err)
	}
	if _, err := m.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS rolebot_scc_ceo (
		corpID      BIGINT NOT NULL PRIMARY KEY,
		firstLookAt BIGINT NOT NULL
	) ENGINE=InnoDB`); err != nil {
		rlogf("rolebots: scc ceo table: %v", err)
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, evedb.SettingSCCBootstrapped); err == nil && (!ok || strings.TrimSpace(v) == "") {
		_ = evedb.SetServerSetting(ctx, m.db, evedb.SettingSCCBootstrapped, fmt.Sprint(clock.Unix()))
		rlogf("rolebots: SCC Exchange first seen — each bot CEO lists on its first think that qualifies")
	}

	evedb.SCCFundamentalsHook = func(ctx context.Context, corpID int64) (float64, float64, bool) {
		if _, ok := managedCEO(corpID); !ok {
			return 0, 0, false
		}
		return m.corpNAV(ctx, corpID), m.corpWeeklyEarnings(ctx, corpID), true
	}
	evedb.SCCBuyCapHook = func(ctx context.Context, corpID, buyerID int64) (int64, bool) {
		ceo, ok := managedCEO(corpID)
		if !ok {
			return 0, false
		}
		issued, _, err := m.corpShareCounts(ctx, corpID)
		if err != nil {
			return 0, false
		}
		held := evedb.ShareHolding(ctx, m.db, buyerID, corpID) + evedb.EscrowedShares(ctx, m.db, buyerID, corpID)
		return holderRoom(issued, held, m.shareMaxHolderPct(ctx), buyerID == ceo), true
	}
	evedb.SCCBarredHook = func(ctx context.Context, db evedb.DBTX, corpID int64) string {
		var tags string
		if err := db.QueryRowContext(ctx, `SELECT tags FROM rolebot_corps WHERE corporationID = ?`, corpID).Scan(&tags); err != nil {
			return ""
		}
		return sccBarredByTags(tags)
	}
	esp.SCCBotStatsHook = m.sccStats
	evedb.SubscribeSCCEvents(m.onSCCEvent)

	m.registerPeriodic("rolebot-scc-makers", sccMakerTicks, func(uint64) bool {
		if !Master() || !sccMakerBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("scc makers", func() {
			defer sccMakerBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.sccMakerPass(ctx)
		})
		return true
	})
}

// sccRules is the exchange's dials, memoised for a minute, since every bot review
// would otherwise read thirteen settings rows.
func (m *Mod) sccRules(ctx context.Context) evedb.SCCRules {
	now := clock.Unix()
	sccRulesMu.Lock()
	defer sccRulesMu.Unlock()
	if now-sccRulesAt < sccRulesTTL {
		return sccRulesMemo
	}
	sccRulesMemo, sccRulesAt = evedb.LoadSCCRules(ctx, m.db), now
	return sccRulesMemo
}

func (m *Mod) sccBotsOn(ctx context.Context) bool {
	r := m.sccRules(ctx)
	return r.Enabled && r.BotsEnabled && sharesEnabled()
}

// sccBoard is the listed board, memoised for five minutes.
func (m *Mod) sccBoard(ctx context.Context) []evedb.SCCBoardRow {
	now := clock.Unix()
	sccBoardMu.Lock()
	defer sccBoardMu.Unlock()
	if now-sccBoardAt < sccBoardTTL && sccBoardMemo != nil {
		return sccBoardMemo
	}
	b, err := evedb.SCCBoard(ctx, m.db, evedb.SCCListingListed)
	if err != nil {
		return sccBoardMemo
	}
	sccBoardMemo, sccBoardAt = b, now
	return b
}

// sccListed reports whether a corporation is listed (from the board memo).
func (m *Mod) sccListed(ctx context.Context, corpID int64) (evedb.SCCBoardRow, bool) {
	for _, r := range m.sccBoard(ctx) {
		if r.CorpID == corpID {
			return r, true
		}
	}
	return evedb.SCCBoardRow{}, false
}

func forgetSCCBoard() {
	sccBoardMu.Lock()
	sccBoardAt = 0
	sccBoardMu.Unlock()
}

// sccRoll is a deterministic 0..1 draw per (id, salt).
func sccRoll(id, salt int64) float64 {
	return float64(splitmix64(uint64(id)*0x9e3779b97f4a7c15+uint64(salt))%1_000_000) / 1_000_000
}

// ── the listing CEO ──────────────────────────────────────────────────────────

// sccListingAppetite is a CEO's chance of applying on one think. A hot sector
// draws listings and a cold one holds them back. Pure.
func sccListingAppetite(traits map[string]int, bootstrap bool, climate float64) float64 {
	if bootstrap {
		return 1
	}
	p := sccPoles(traits)
	return math.Max(0, math.Min(1, 0.15+0.35*p.Leader+0.25*p.Greedy+0.15*p.Hurried+0.2*climate))
}

// sccCEO is the listing CEO's think: weigh applying, or run the treasury's
// orders once listed.
func (m *Mod) sccCEO(ctx context.Context, c *corpCEO, ch Charter) {
	if !m.sccBotsOn(ctx) {
		return
	}
	l, has, err := evedb.GetSCCListing(ctx, m.db, c.CorpID)
	if err != nil {
		return
	}
	if has && l.Status == evedb.SCCListingListed {
		m.sccRefile(ctx, c, l)
		m.sccTreasury(ctx, c, ch, l)
		return
	}
	if has && l.Status == evedb.SCCListingApplied {
		return
	}
	now := clock.Unix()
	if has && now-max(l.AppliedAt, l.DelistedAt) < sccListCooldownSec {
		return
	}
	rules := m.sccRules(ctx)
	f, err := evedb.LoadSCCFundamentals(ctx, m.db, c.CorpID)
	if err != nil || len(evedb.SCCEligibility(f, rules)) > 0 {
		return
	}
	// The fee comes out of the wallet before the review re-checks NAV: a
	// corporation that only just qualifies would be rejected by its own fee.
	if f.Wallet < rules.ListingFee || f.NAV-rules.ListingFee < rules.MinNAV {
		return
	}
	traits, _ := getTraits(ctx, m.db, c.CharID)
	// The first think on which the corporation qualifies is the bootstrap: the CEO
	// lists outright. It is recorded whatever comes of it: one look per corporation.
	res, _ := m.db.ExecContext(ctx, `INSERT IGNORE INTO rolebot_scc_ceo (corpID, firstLookAt) VALUES (?, ?)`, c.CorpID, now)
	bootstrap := false
	if res != nil {
		if n, _ := res.RowsAffected(); n == 1 {
			bootstrap = true
		}
	}
	rating := evedb.SCCScore(f.RatingInput())
	if traitScale(traits[TraitCaution]) < -0.3 && rating.Risk > sccCarefulMaxRisk && !bootstrap {
		return // a careful CEO waits for BBB
	}
	sector := m.corpSectorFor(ctx, c.CorpID)
	if sccRoll(c.CorpID, now/3600) >= sccListingAppetite(traits, bootstrap, evedb.SCCSectorClimate(ctx, m.db)[sector]) {
		return
	}
	if err := evedb.ApplyForListing(ctx, m.db, rules, c.CorpID, c.CharID, sector); err != nil {
		m.logBot(c.CharID, logInfo, "ceo", "could not apply for an SCC listing: %v", err)
		return
	}
	sccListingsFiled.Add(1)
	if bootstrap {
		sccBootstrapped.Add(1)
	}
	name := corpNameOf(ctx, m.db, c.CorpID)
	_ = evedb.PostSCCBulletin(ctx, m.db, c.CorpID, evedb.SCCBulletinApply,
		fmt.Sprintf("%s has applied for a listing in the %s sector", name, sector), false, 0)
	m.ceoLog(ctx, c, powerShares, "scclist", 0, rules.ListingFee,
		"applied to list the corporation on the SCC Exchange (%s sector, rating %s)", sector, rating.Grade)
}

// sccRefile moves a listed corporation to the sector its business is in now (a
// retag). No fee; the sector indices are chain-linked and move by neither side.
func (m *Mod) sccRefile(ctx context.Context, c *corpCEO, l evedb.SCCListing) {
	if c.Paused || c.PowersOff[powerShares] {
		return
	}
	want := m.corpSectorFor(ctx, c.CorpID)
	if want == l.Sector {
		return
	}
	old, changed, err := evedb.ReclassifySCCListing(ctx, m.db, c.CorpID, want)
	if err != nil || !changed {
		return
	}
	forgetSCCBoard()
	sccRefiled.Add(1)
	name := corpNameOf(ctx, m.db, c.CorpID)
	_ = evedb.PostSCCBulletin(ctx, m.db, c.CorpID, evedb.SCCBulletinApply,
		fmt.Sprintf("%s has moved its listing from the %s sector to %s", name, old, want), false, 0)
	m.ceoLog(ctx, c, powerShares, "sccsector", 0, 0,
		"refiled the corporation's SCC listing from the %s sector to %s", old, want)
}

// sccTreasury runs a listed corporation's own trading: an at-the-market offering
// of treasury stock at no less than 1.05 x book, and a buy-back programme that
// bids only while the stock trades under book. Prices are clamped into today's
// band. See docs/comment-notes-b16.md#scc-treasury.
func (m *Mod) sccTreasury(ctx context.Context, c *corpCEO, ch Charter, l evedb.SCCListing) {
	if c.Paused || c.PowersOff[powerShares] {
		return
	}
	corpID := c.CorpID
	orders, _ := evedb.ListShareOrders(ctx, m.db, corpID, corpID)
	var ask, bid *evedb.SCCOrderRow
	for i := range orders {
		if orders[i].IsBuy {
			bid = &orders[i]
		} else {
			ask = &orders[i]
		}
	}
	cancel := func(o *evedb.SCCOrderRow) {
		if o != nil {
			_, _ = evedb.CancelShareOrder(ctx, m.db, o.OrderID, corpID)
		}
	}
	if m.sccBlackout(ctx, corpID) {
		cancel(ask)
		cancel(bid)
		return
	}
	q, err := m.corpShareQuote(ctx, corpID, ch)
	if err != nil || q.Book <= 0 {
		return
	}
	rules := m.sccRules(ctx)
	band := evedb.SCCPriceBand(l.Reference(), rules.BandPct)
	st := m.loadShareState(ctx, corpID)
	place := func(isBuy bool, price float64, qty int64, days int) {
		if qty <= 0 || !band.Contains(price) {
			return
		}
		if _, err := evedb.PlaceShareOrder(ctx, m.db, rules.SCCConfig, evedb.SCCPlaceRequest{CorpID: corpID, OwnerID: corpID,
			IsBuy: isBuy, Qty: qty, Price: price, ExpiryDays: days, Source: evedb.SCCSourceTreasury}); err != nil {
			m.logBot(c.CharID, logInfo, "ceo", "treasury order on the SCC: %v", err)
		}
	}

	// Both prices are clamped into the band first (sccTreasuryPrices). Comparisons
	// below use the clamped prices, or a side pinned at band.Hi is re-placed daily.
	ceiling := evedb.SCCTreasuryCeiling(q.Book, sccVWAP(ctx, m, corpID, 7), st.Premium, l.IPOPrice)
	askPx, askOK, bidPx, bidOK := sccTreasuryPrices(ceiling, math.Ceil(q.Book*1.05),
		math.Floor(q.Book*(1-ch.Haircut)), band)

	// The offering.
	m.sccTreasuryNote(c, corpID, askOK, ceiling, band, q.Book)
	switch {
	case !askOK:
		// Today's ceiling is above the band: the stock is far under book. A standing
		// ask inside the band stays; one the band has left is pulled.
		if ask != nil && !band.Contains(ask.Price) {
			cancel(ask)
		}
	case ask == nil || evedbShouldReplace(ask.Price, askPx):
		cancel(ask)
		_, treasury, _ := m.corpShareCounts(ctx, corpID)
		sellable := treasurySellable(q.Issued, treasury, ch.ControlFloor)
		place(false, askPx, min(sellable, max(1, int64(float64(q.Issued)*sccOfferDayFrac))), 1)
	}

	// The market is the cheapest ask that is not the treasury's own offering (a
	// level at ask.Price holding one order), else the last trade.
	market := l.Price()
	if _, asks, _ := evedb.SCCDepth(ctx, m.db, corpID, 5); len(asks) > 0 {
		for _, a := range asks {
			if ask != nil && a.Price == ask.Price && a.Orders == 1 {
				continue
			}
			market = a.Price
			break
		}
	}
	var qty int64
	if bidOK {
		qty = sccBuybackSize(q.Book, market, bidPx, m.buybackAllowance(ctx, corpID, ch),
			m.corpWallet(ctx, corpID), q.Issued, sccWeekVolume(ctx, m, corpID), rules.BrokerFee)
	}
	switch {
	case qty <= 0:
		cancel(bid)
	case bid == nil || evedbShouldReplace(bid.Price, bidPx) || bid.QtyOpen < qty/2:
		cancel(bid)
		place(true, bidPx, qty, 3)
	}
}

// sccTreasuryWithheld is the set of listed corporations whose offering the band
// is holding back, so the log line is written once per transition, not per think.
var sccTreasuryWithheld sync.Map // corpID → struct{}

// sccTreasuryNote logs the offering being withheld or resumed, on the
// change only.
func (m *Mod) sccTreasuryNote(c *corpCEO, corpID int64, askOK bool, ceiling float64, band evedb.SCCBand, book float64) {
	_, was := sccTreasuryWithheld.Load(corpID)
	switch {
	case !askOK && !was:
		sccTreasuryWithheld.Store(corpID, struct{}{})
		m.logBot(c.CharID, logInfo, "ceo", "SCC offering withheld: ceiling %s above today's band [%s..%s], book %s; the treasury buys instead",
			iskString(ceiling), iskString(band.Lo), iskString(band.Hi), iskString(book))
	case askOK && was:
		sccTreasuryWithheld.Delete(corpID)
		m.logBot(c.CharID, logInfo, "ceo", "SCC offering resumes at %s", iskString(math.Min(ceiling, band.Hi)))
	}
}

// sccBuybackSize is the buy-back programme's order. Pure. Nothing while the
// stock trades at or above book; otherwise what the allowance buys at the
// programme price (with no allowance and the stock under 85% of book, 5% of the
// wallet), capped at a quarter of the week's volume or 1% of the issue.
func sccBuybackSize(book, market, price, allowance, wallet float64, issued, weekVolume int64, fee float64) int64 {
	if book <= 0 || market <= 0 || market >= book || price < 1 {
		return 0
	}
	budget := allowance
	if budget <= 0 && market < sccBuybackDeepFrac*book {
		budget = sccBuybackReserveFrac * wallet
	}
	qty := int64(budget / (price * (1 + fee)))
	limit := weekVolume / 4
	if limit <= 0 {
		limit = max(1, issued/100)
	}
	return max(0, min(qty, limit))
}

// sccBlackout reports whether the corporation is inside a day of its own
// dividend or rating change.
func (m *Mod) sccBlackout(ctx context.Context, corpID int64) bool {
	bs, _ := evedb.RecentSCCBulletins(ctx, m.db, corpID, 10)
	for _, b := range bs {
		if clock.Unix()-b.At >= sccBlackoutSec {
			break
		}
		if b.Kind == evedb.SCCBulletinDividend || b.Kind == evedb.SCCBulletinRating {
			return true
		}
	}
	return false
}

func sccWeekVolume(ctx context.Context, m *Mod, corpID int64) int64 {
	cs, _ := evedb.SCCCandles(ctx, m.db, corpID, 7)
	var v int64
	for _, c := range cs {
		v += c.Volume
	}
	return v
}

func (m *Mod) corpWallet(ctx context.Context, corpID int64) float64 {
	bal, _ := evedb.GetCorpBalance(ctx, m.db, corpID)
	return bal
}

// sccBookTreasuryFills writes every treasury fill into the corporation's own
// books as a desk sale or redemption was: the ledger line (which the buy-back
// allowance counts from), the holder's equity and the week's flow counters. It
// runs on the maker pass from a cursor.
func (m *Mod) sccBookTreasuryFills(ctx context.Context) {
	var cursor int64
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingSCCLedgerCursor); err == nil && ok {
		fmt.Sscan(v, &cursor)
	}
	rows, err := m.db.QueryContext(ctx, `SELECT tradeID, corpID, buyerID, sellerID, qty, price, tax FROM sccTrades
		WHERE tradeID > ? AND isTreasury = 1 ORDER BY tradeID LIMIT 500`, cursor)
	if err != nil {
		return
	}
	type fill struct {
		id, corp, buyer, seller, qty int64
		price, tax                   float64
	}
	var fills []fill
	for rows.Next() {
		var f fill
		if rows.Scan(&f.id, &f.corp, &f.buyer, &f.seller, &f.qty, &f.price, &f.tax) == nil {
			fills = append(fills, f)
		}
	}
	rows.Close()
	for _, f := range fills {
		cursor = f.id
		if _, ok := managedCEO(f.corp); !ok {
			continue
		}
		gross := float64(f.qty) * f.price
		if f.seller == f.corp {
			who := m.charNameOr(ctx, f.buyer)
			m.corpBook(ctx, f.corp, lineShares, gross-f.tax, f.buyer, fmt.Sprintf("%d shares to %s on the SCC Exchange", f.qty, who))
			m.creditEquity(ctx, f.corp, f.buyer, gross, 0)
			m.noteShareFlow(ctx, f.corp, f.qty, 0, 0)
		} else {
			who := m.charNameOr(ctx, f.seller)
			m.corpBook(ctx, f.corp, lineRedeem, -gross, f.seller, fmt.Sprintf("bought back %d shares from %s on the SCC Exchange", f.qty, who))
			m.creditEquity(ctx, f.corp, f.seller, 0, gross-f.tax)
			m.noteShareFlow(ctx, f.corp, 0, f.qty, 0)
		}
		forgetShareQuote(f.corp)
	}
	if len(fills) > 0 {
		_ = evedb.SetServerSetting(ctx, m.db, settingSCCLedgerCursor, fmt.Sprint(cursor))
	}
}

func evedbShouldReplace(old, now float64) bool { return sccShouldReplace(old, now) }

// sccVWAP is a listing's volume-weighted average over the last n days (0
// when it has not traded).
func sccVWAP(ctx context.Context, m *Mod, corpID int64, days int) float64 {
	cs, _ := evedb.SCCCandles(ctx, m.db, corpID, days)
	var val float64
	var vol int64
	for _, c := range cs {
		val += c.Value
		vol += c.Volume
	}
	if vol == 0 {
		return 0
	}
	return val / float64(vol)
}

// ── members, ex-members and the broke ───────────────────────────────────────

// sccBuyOwn is a member's investment in its listed corporation: a buy near
// the market, out of the budget investFor already decided.
func (m *Mod) sccBuyOwn(ctx context.Context, b *botRun, row evedb.SCCBoardRow, budget float64) {
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
		return
	}
	res, err := evedb.PlaceShareOrder(ctx, m.db, rules.SCCConfig, evedb.SCCPlaceRequest{CorpID: row.CorpID, OwnerID: b.CharID,
		IsBuy: true, Qty: qty, Price: price, ExpiryDays: 3, Source: evedb.SCCSourceBot})
	if err != nil {
		return
	}
	sccBotOrders.Add(1)
	if n := res.FilledQty(); n > 0 {
		m.logBot(b.CharID, logInfo, "corp", "bought %d shares of its corporation [%s] on the SCC Exchange at %s",
			n, row.Ticker, iskString(price))
	}
}

// sccSellHolding sells n shares of a listed stock: at the best bid and left
// on the book when patient, at the band edge (immediate) when not.
func (m *Mod) sccSellHolding(ctx context.Context, b *botRun, row evedb.SCCBoardRow, n int64, now bool) int64 {
	rules := m.sccRules(ctx)
	req := evedb.SCCPlaceRequest{CorpID: row.CorpID, OwnerID: b.CharID, Qty: n, Source: evedb.SCCSourceBot}
	if !now {
		bids, _, _ := evedb.SCCDepth(ctx, m.db, row.CorpID, 1)
		price := row.Price
		if len(bids) > 0 {
			price = bids[0].Price
		}
		req.Price = sccClampBand(math.Floor(price), evedb.SCCPriceBand(row.Reference(), rules.BandPct))
		req.ExpiryDays = 3
	}
	res, err := evedb.PlaceShareOrder(ctx, m.db, rules.SCCConfig, req)
	if err != nil {
		return 0
	}
	sccBotOrders.Add(1)
	return res.FilledQty()
}

// ── the investor ─────────────────────────────────────────────────────────────

// rngFor is a deterministic stream per (pilot, listing, week): an opinion
// redrawn twice in one week comes out the same.
func rngFor(charID, corpID, week int64) *rand.Rand {
	return rand.New(rand.NewSource(int64(splitmix64(uint64(charID)*31 + uint64(corpID)*1_000_003 + uint64(week)))))
}

// sccConviction is a pilot's persistent opinion of one listing, redrawn
// weekly with mean reversion (sccNextConviction).
func (m *Mod) sccConviction(ctx context.Context, charID, corpID int64, sigma float64) float64 {
	week := int(clock.Unix() / (7 * 86400))
	var c float64
	var w int
	_ = m.db.QueryRowContext(ctx, `SELECT conviction, week FROM rolebot_scc_conviction WHERE characterID = ? AND corpID = ?`,
		charID, corpID).Scan(&c, &w)
	if c > 0 && w == week {
		return c
	}
	next := sccNextConviction(rngFor(charID, corpID, int64(week)), c, sigma)
	_, _ = m.db.ExecContext(ctx, `REPLACE INTO rolebot_scc_conviction (characterID, corpID, conviction, week) VALUES (?, ?, ?, ?)`,
		charID, corpID, next, week)
	return next
}

// sccView builds what a pilot sees of one listing. st is the board read once for
// the pass (sccBoardStatsOf): the small-cap line and the sector price levels.
func (m *Mod) sccView(ctx context.Context, b *botRun, inv sccInvestor, row evedb.SCCBoardRow, st sccBoardStats,
	held int64, avgCost float64) sccStockView {
	rules := m.sccRules(ctx)
	v := sccStockView{CorpID: row.CorpID, Last: row.Price, Own: row.CorpID == b.CorpID,
		Band: evedb.SCCPriceBand(row.Reference(), rules.BandPct), Held: held, CostBasis: avgCost,
		SmallCap:   row.MarketCap < st.MedianCap,
		Conviction: m.sccConviction(ctx, b.CharID, row.CorpID, inv.ConvictionSigma),
		Shock:      evedb.SCCEventShock(ctx, m.db, row.CorpID, evedb.SCCShockTauDefault),
		Climate:    evedb.SCCSectorClimate(ctx, m.db)[row.Sector],
		SectorRel:  st.Sectors.RelOf(row.Sector)}
	if row.HasFiling {
		f := row.Filing
		v.BookPS, v.EPSWeek, v.Risk = f.BookPS, f.EPSWeek, f.Risk
		if row.Price > 0 && row.MarketCap > 0 {
			v.YieldYear = f.Dividends28 * 13 / row.MarketCap
		}
		if prev, ok := evedb.SCCFilingOn(ctx, m.db, row.CorpID, f.Day-7); ok {
			v.RiskDelta = f.Risk - prev.Risk
		}
		if prev, ok := evedb.SCCFilingOn(ctx, m.db, row.CorpID, f.Day-28); ok && prev.Dividends28 > 0 {
			v.DividendCut = f.Dividends28 < 0.7*prev.Dividends28
		}
	}
	bids, asks, _ := evedb.SCCDepth(ctx, m.db, row.CorpID, 1)
	if len(bids) > 0 {
		v.BestBid = bids[0].Price
	}
	if len(asks) > 0 {
		v.BestAsk = asks[0].Price
	}
	if cs, _ := evedb.SCCCandles(ctx, m.db, row.CorpID, 8); len(cs) >= 2 {
		if first := cs[0].Close; first > 0 {
			v.Ret7 = cs[len(cs)-1].Close/first - 1
		}
		var a, p int64
		for i, c := range cs {
			if i >= len(cs)/2 {
				a += c.Volume
			} else {
				p += c.Volume
			}
		}
		v.VolumeRising = a > p
	}
	if bs, _ := evedb.RecentSCCBulletins(ctx, m.db, row.CorpID, 3); len(bs) > 0 && clock.Unix()-bs[0].At < 86400 {
		switch bs[0].Kind {
		case evedb.SCCBulletinIPO, evedb.SCCBulletinDividend:
			v.Bulletin = 1
		case evedb.SCCBulletinDelist:
			v.Bulletin = -1
		case evedb.SCCBulletinRating:
			if strings.Contains(bs[0].Text, "downgraded") {
				v.Bulletin = -1
			} else {
				v.Bulletin = 1
			}
		}
	}
	return v
}

// sccBoughtSince is every listing a pilot has bought on the exchange since the
// given instant, the stocks still in their holding period.
func (m *Mod) sccBoughtSince(ctx context.Context, charID, since int64) map[int64]bool {
	out := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `SELECT DISTINCT corpID FROM sccTrades WHERE buyerID = ? AND at >= ?`, charID, since)
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

// sccBotReview is a pilot's portfolio review on its visit home.
func (m *Mod) sccBotReview(ctx context.Context, b *botRun) {
	if b == nil || !m.sccBotsOn(ctx) {
		return
	}
	board := m.sccBoard(ctx)
	if len(board) == 0 {
		return
	}
	traits, _ := m.botTraits(ctx, b)
	inv := sccInvestorFor(traits)
	now := clock.Unix()
	if last, ok := sccLastReview.Load(b.CharID); ok && float64(now-last.(int64)) < inv.ReviewHours*3600 {
		return
	}
	sccLastReview.Store(b.CharID, now)
	cash, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return
	}
	holdings, _ := evedb.SCCHoldings(ctx, m.db, b.CharID)
	var port float64
	held := map[int64]int64{}
	for _, h := range holdings {
		port += float64(h.Free+h.Escrowed) * h.LastPrice
		held[h.CorpID] = h.Free
	}
	rules := m.sccRules(ctx)

	// The lottery ticket: a pilot who keeps nothing in stocks, flush after a
	// big bounty, now and then buys a little of a mercenary outfit at market.
	if inv.StockFrac <= 0 {
		if cash < sccLotteryMinCash || sccRoll(b.CharID, now/86400) >= sccLotteryChance {
			return
		}
		var sec []evedb.SCCBoardRow
		for _, r := range board {
			if r.Sector == "Security" {
				sec = append(sec, r)
			}
		}
		if len(sec) == 0 {
			return
		}
		row := sec[int(sccRoll(b.CharID, now)*float64(len(sec)))%len(sec)]
		px := evedb.SCCPriceBand(row.Reference(), rules.BandPct).MarketPrice(true)
		if qty := int64(sccLotteryFrac * (cash - sccInvestAboveISK) / (px * (1 + rules.BrokerFee))); qty > 0 {
			if _, err := evedb.PlaceShareOrder(ctx, m.db, rules.SCCConfig, evedb.SCCPlaceRequest{CorpID: row.CorpID,
				OwnerID: b.CharID, IsBuy: true, Qty: qty, Source: evedb.SCCSourceBot}); err == nil {
				sccBotOrders.Add(1)
				m.logBot(b.CharID, logInfo, "market", "bought a lottery ticket: %d shares of [%s] at market", qty, row.Ticker)
			}
		}
		return
	}

	open, _ := evedb.ListShareOrders(ctx, m.db, 0, b.CharID)
	if len(open) >= sccMaxOpenOrders {
		return
	}
	sccReviewsByStyle[inv.Dominant].Add(1)
	budget := math.Max(0, inv.StockFrac*(cash+port-sccInvestAboveISK)-port)
	st := sccBoardStatsOf(board, evedb.SCCNow())
	pos, _ := evedb.SCCPositions(ctx, m.db, b.CharID)
	avg := map[int64]float64{}
	for _, p := range pos {
		if p.Shares > 0 {
			avg[p.CorpID] = p.Cost / float64(p.Shares)
		}
	}

	byID := map[int64]evedb.SCCBoardRow{}
	for _, r := range board {
		byID[r.CorpID] = r
	}
	// Own corporation, holdings, the rotation pick, one random listing
	// (sccCandidates, shared with the harness).
	heldIDs := make([]int64, 0, len(held))
	for id := range held {
		heldIDs = append(heldIDs, id)
	}
	cands := sccCandidates(b.CorpID, heldIDs, st.Inputs, st.Sectors,
		[2]float64{sccRoll(b.CharID, now), sccRoll(b.CharID, now+1)})
	recent := m.sccBoughtSince(ctx, b.CharID, clock.Unix()-sccMinHoldSec)
	poles := sccPoles(traits)
	stops := m.sccArmedStops(ctx, b.CharID)
	// Stops follow the holdings: armed where the pilot's policy wants one,
	// resized when the holding moved, disarmed when it is gone.
	for id, row := range byID {
		if held[id] > 0 || stops[id] != nil {
			m.sccKeepStop(ctx, b.CharID, inv, poles, row, held[id], avg[id], stops[id])
		}
	}
	stops = m.sccArmedStops(ctx, b.CharID)
	stoppedOut := m.sccStoppedOut(ctx, b.CharID)
	placed := 0
	for _, id := range cands {
		if placed >= sccMaxPlacements || len(open)+placed >= sccMaxOpenOrders {
			break
		}
		row := byID[id]
		view := m.sccView(ctx, b, inv, row, st, held[id], avg[id])
		view.HasStop = stops[id] != nil
		view.StoppedOut = stoppedOut[id]
		in, ok := sccReview(inv, view, budget, cash)
		if !ok {
			// The pilot no longer wants this stock: a bid it left on the
			// book from an earlier opinion comes down with the opinion.
			for _, o := range open {
				if o.CorpID == id && o.IsBuy && o.Source == evedb.SCCSourceBot {
					_, _ = evedb.CancelShareOrder(ctx, m.db, o.OrderID, b.CharID)
				}
			}
			continue
		}
		if !in.IsBuy && recent[id] {
			continue
		}
		keep := false
		for _, o := range open {
			if o.CorpID == id && o.IsBuy == in.IsBuy && o.Source != evedb.SCCSourceMaker {
				if !sccShouldReplace(o.Price, in.Price) {
					keep = true
				} else {
					_, _ = evedb.CancelShareOrder(ctx, m.db, o.OrderID, b.CharID)
				}
			}
		}
		if keep {
			continue
		}
		if !in.IsBuy {
			in.Qty = min(in.Qty, evedb.ShareHolding(ctx, m.db, b.CharID, id))
			if in.Qty <= 0 {
				continue
			}
		}
		res, err := evedb.PlaceShareOrder(ctx, m.db, rules.SCCConfig, evedb.SCCPlaceRequest{CorpID: id, OwnerID: b.CharID,
			IsBuy: in.IsBuy, Qty: in.Qty, Price: in.Price, ExpiryDays: in.ExpiryDays, Source: evedb.SCCSourceBot})
		if err != nil {
			continue
		}
		placed++
		sccBotOrders.Add(1)
		if in.IsBuy {
			budget -= float64(in.Qty) * in.Price
		}
		side := "sell"
		if in.IsBuy {
			side = "buy"
		}
		m.logBot(b.CharID, logInfo, "market", "SCC %s %d [%s] at %s (%s): %d filled",
			side, in.Qty, row.Ticker, iskString(in.Price), in.Why, res.FilledQty())
		if in.IsBuy && res.FilledQty() > 0 {
			m.sccAfterBuy(ctx, b.CharID, inv, poles, row)
		}
	}
}

// ── market makers ────────────────────────────────────────────────────────────

// sccPickMakerSet chooses the makers: Trader-archetype pilots greedy enough
// to want the spread, richest first, capped by scc.botMakers.
func (m *Mod) sccPickMakerSet(ctx context.Context, cap int) []int64 {
	rows, err := m.db.QueryContext(ctx, `SELECT g.characterID, c.balance FROM rolebot_generated g
		JOIN character_ c ON c.characterID = g.characterID WHERE g.archetype = 'trader'
		ORDER BY c.balance DESC LIMIT ?`, cap*4)
	if err != nil {
		return nil
	}
	wealth := map[int64]float64{}
	var ids []int64
	for rows.Next() {
		var id int64
		var bal float64
		if rows.Scan(&id, &bal) == nil {
			wealth[id] = bal
			ids = append(ids, id)
		}
	}
	rows.Close()
	traits, _ := loadTraitsBulk(ctx, m.db, ids)
	cands := map[int64]float64{}
	for _, id := range ids {
		if sccIsMakerMaterial("trader", traits[id]) {
			cands[id] = wealth[id]
		}
	}
	return sccPickMakers(cands, cap)
}

func (m *Mod) sccMakerPass(ctx context.Context) {
	if !m.sccBotsOn(ctx) {
		return
	}
	m.sccBookTreasuryFills(ctx)
	rules := m.sccRules(ctx)
	board := m.sccBoard(ctx)
	if len(board) == 0 || rules.BotMakers <= 0 {
		return
	}
	now := clock.Unix()
	sccMakersMu.Lock()
	if now-sccMakerAt >= sccMakerPickSec || sccMakerSet == nil {
		sccMakerSet, sccMakerAt = m.sccPickMakerSet(ctx, rules.BotMakers), now
	}
	makers := append([]int64(nil), sccMakerSet...)
	sccMakersMu.Unlock()

	for _, id := range makers {
		sccMakersMu.Lock()
		last := sccMakerLast[id]
		sccMakersMu.Unlock()
		// A logged-off maker quotes nothing; its resting asks stay put.
		if now-last < sccMakerRequoteSec || popIsOffline(id) {
			continue
		}
		sccMakersMu.Lock()
		sccMakerLast[id] = now
		sccMakersMu.Unlock()
		m.sccQuote(ctx, id, board, rules, 0)
	}
}

// sccQuote is one maker's refresh: up to three listings, two sides each, or
// only focus when news on that listing has the maker requoting it now. While
// news is fresh the quote is wider. A halted listing is not quoted.
func (m *Mod) sccQuote(ctx context.Context, charID int64, board []evedb.SCCBoardRow, rules evedb.SCCRules, focus int64) {
	traits, _ := getTraits(ctx, m.db, charID)
	inv := sccInvestorFor(traits)
	cash, err := evedb.GetCharBalance(ctx, m.db, charID)
	if err != nil {
		return
	}
	n := min(sccMakerMaxLists, len(board))
	budget := inv.StockFrac * math.Max(0, cash-sccInvestAboveISK)
	open, _ := evedb.ListShareOrders(ctx, m.db, 0, charID)
	rows := make([]evedb.SCCBoardRow, 0, n)
	if focus != 0 {
		for _, r := range board {
			if r.CorpID == focus {
				rows = append(rows, r)
			}
		}
	} else {
		for k := 0; k < n; k++ {
			rows = append(rows, board[int(splitmix64(uint64(charID)+uint64(k)*7919)%uint64(len(board)))])
		}
	}
	now := evedb.SCCNow()
	sectors := sccBoardStatsOf(board, now).Sectors
	for _, row := range rows {
		if !row.HasFiling || row.Halted(now) {
			continue
		}
		f := row.Filing
		shock := evedb.SCCEventShock(ctx, m.db, row.CorpID, evedb.SCCShockTauDefault)
		v := sccValueOf(f.BookPS, f.EPSWeek, inv.HorizonWeeks, f.Risk, m.sccConviction(ctx, charID, row.CorpID, inv.ConvictionSigma)) *
			sccSentiment(inv, shock, evedb.SCCSectorClimate(ctx, m.db)[row.Sector], sectors.RelOf(row.Sector))
		if v <= 0 {
			continue
		}
		qinv := inv
		qinv.MakerHalfSpread *= 1 + sccMakerShockWiden*math.Min(sccShockCap, math.Abs(shock))
		held := evedb.ShareHolding(ctx, m.db, charID, row.CorpID)
		target := int64(budget / float64(n) / 2 / v)
		band := evedb.SCCPriceBand(row.Reference(), rules.BandPct)
		bp, ap, bq, aq := sccMakerQuote(qinv, v, held, target, budget/float64(n), band)
		for _, side := range []struct {
			buy bool
			px  float64
			q   int64
		}{{true, bp, bq}, {false, ap, aq}} {
			var cur *evedb.SCCOrderRow
			for i := range open {
				if open[i].CorpID == row.CorpID && open[i].IsBuy == side.buy && open[i].Source == evedb.SCCSourceMaker {
					cur = &open[i]
				}
			}
			// A side the maker no longer quotes (its value has left the
			// band) is withdrawn, not left standing at the old price.
			if cur != nil && side.q > 0 && !sccShouldReplace(cur.Price, side.px) {
				continue
			}
			if cur != nil {
				_, _ = evedb.CancelShareOrder(ctx, m.db, cur.OrderID, charID)
			}
			if side.q <= 0 {
				continue
			}
			if _, err := evedb.PlaceShareOrder(ctx, m.db, rules.SCCConfig, evedb.SCCPlaceRequest{CorpID: row.CorpID,
				OwnerID: charID, IsBuy: side.buy, Qty: side.q, Price: side.px, ExpiryDays: 1, Source: evedb.SCCSourceMaker}); err == nil {
				sccBotOrders.Add(1)
			}
		}
	}
}

// ── the admin page ───────────────────────────────────────────────────────────

func (m *Mod) sccStats(ctx context.Context) []esp.SCCStatLine {
	r := m.sccRules(ctx)
	state := "on"
	if !r.BotsEnabled {
		state = "off (scc.botsEnabled)"
	}
	sccMakersMu.Lock()
	makers := len(sccMakerSet)
	sccMakersMu.Unlock()
	var botOpen, makerOpen int
	_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(source = 1), 0), COALESCE(SUM(source = 3), 0)
		FROM sccOrders WHERE state = 0`).Scan(&botOpen, &makerOpen)
	var styles []string
	for i := sccStyle(0); i < sccStyleCount; i++ {
		styles = append(styles, fmt.Sprintf("%s %d", i, sccReviewsByStyle[i].Load()))
	}
	var looked, armedStops int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_scc_ceo`).Scan(&looked)
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sccStops WHERE state = 0 AND source = ?`, evedb.SCCSourceBot).Scan(&armedStops)
	boot := fmt.Sprintf("%d corporations have had their first look; %d listed on it since boot", looked, sccBootstrapped.Load())
	return []esp.SCCStatLine{
		{Label: "Bots on the exchange", Value: state},
		{Label: "Listing bootstrap", Value: boot},
		{Label: "Market makers", Value: fmt.Sprintf("%d of %d allowed", makers, r.BotMakers)},
		{Label: "Open bot orders", Value: fmt.Sprintf("%d investor, %d maker", botOpen, makerOpen)},
		{Label: "Bot orders placed since boot", Value: fmt.Sprint(sccBotOrders.Load())},
		{Label: "Listings filed by bot CEOs since boot", Value: fmt.Sprint(sccListingsFiled.Load())},
		{Label: "Listings refiled to another sector since boot", Value: fmt.Sprint(sccRefiled.Load())},
		{Label: "Portfolio reviews since boot, by style", Value: strings.Join(styles, " · ")},
		{Label: "Material events reacted to since boot", Value: fmt.Sprintf("%d (%d skipped by the rate limit)",
			sccEventsReacted.Load(), sccEventsSkipped.Load())},
		{Label: "News reactions since boot", Value: fmt.Sprintf("%d holder sales, %d holder adds, %d opportunist orders, %d maker requotes",
			sccReactSells.Load(), sccReactAdds.Load(), sccReactOpps.Load(), sccReactQuotes.Load())},
		{Label: "Bot stops", Value: fmt.Sprintf("%d armed now; %d armed, %d disarmed since boot", armedStops,
			sccStopsArmed.Load(), sccStopsDisarmed.Load())},
	}
}

// sccBarredByTags is the Commission's standing bar, read off a corporation's
// tags: an outfit in the piracy trade (outlaw) or a cartel's industrial wing
// (cartelindustry) is never listed. The bar follows a CEO who retags. "" when
// nothing bars it; the wording is the Commission's.
func sccBarredByTags(tags string) string {
	for _, t := range strings.Split(tags, ",") {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case tagOutlaw:
			return "the Commission does not list corporations engaged in piracy"
		case "cartelindustry":
			return "the Commission does not list cartel enterprises"
		}
	}
	return ""
}

func init() {
	registerCharForget("sccMakerLast", func(id int64) {
		sccMakersMu.Lock()
		delete(sccMakerLast, id)
		sccMakersMu.Unlock()
	})
	registerCharForget("sccLastReview", func(id int64) { sccLastReview.Delete(id) })
}
