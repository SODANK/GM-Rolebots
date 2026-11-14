package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The share desk: a managed corporation's stock, bought and sold at a price its
// CEO sets. The core keeps the register (crpcharshares) and the votes; this file is
// the market around it, with the corporation as its only market maker.
// Members buy from the treasury at the ask, holders sell back at the bid out of a
// weekly allowance, and dividends (corpdividend.go) pay profit out.
// See docs/comment-notes-b06.md#share-price.

const (
	settingSharesEnabled   = "rolebots.shares.enabled"
	settingSharesPar       = "rolebots.shares.parISK"
	settingSharesMaxHolder = "rolebots.shares.maxHolderPct"
	settingSharesMigrated  = "rolebots.shares.migrated"
	settingSharesCursor    = "rolebots.shares.journalCursor"

	shareParDefault       = 100_000.0 // the ground floor: the ask is par plus what a share is worth
	shareMaxHolderDefault = 20.0      // % of the issue one holder (not the CEO) may own

	// refShares is the journal refType the client prints as "Shares".
	refShares = 22
	// refPlayerDonation is one owner giving ISK to another: what the client's Give
	// Money writes, and so what a share order arrives as.
	refPlayerDonation = 10

	// NAV discounts: what the corporation's goods and prints would fetch
	// in a hurry, not what they cost.
	shareStockHaircut   = 0.7
	shareLibraryHaircut = 0.5
	// shareStockTypes bounds the office valuation to the biggest stacks.
	shareStockTypes = 150

	shareEarningsWindowSec = 28 * 86400
	shareQuoteTTLSec       = 300
	shareWeekSec           = 7 * 86400

	// The premium's weekly step while the stock sells, and its drift down
	// while it does not.
	sharePremiumStep  = 0.05
	sharePremiumDrift = 0.02
	// shareHotSoldFrac: a week that sold this share of what was for sale
	// is a week the stock was in demand.
	shareHotSoldFrac = 0.25

	// Issuing more shares when the treasury has nothing left to sell.
	shareIssueLowFrac  = 0.10         // sellable below this share of the issue
	shareIssueMin      = 100          // never propose fewer than this
	shareVoteSec       = 24 * 3600    // how long an issue stays open for voting
	shareIssueCooldown = 3 * 86400    // between two proposals
	shareBookKeepDays  = 60           // daily book-value snapshots kept
	shareDeskTicks     = 30           // the journal poller's cadence
	shareDeskBatch     = 200          // journal rows read per pass
	shareMinTradeISK   = 1.0          // anything smaller is not worth a row
	shareGrowthPeriods = 365.0 / 28.0 // 28-day windows in a year
	win32TicksPerSec   = 10_000_000   // FILETIME is in 100 ns ticks
)

// Members' appetite for their corporation's stock (investFor).
const (
	// investAboveISK: a member invests only out of what it holds above this, ten
	// times the comfort line (appetite.go needComfortISK).
	investAboveISK = 10 * needComfortISK
	// investFoundingAboveISK replaces that line while the corporation is short of
	// working capital (corpShortLine): a founding round, at the comfort line itself.
	investFoundingAboveISK = needComfortISK
	// investBaseFrac is the share of that surplus a member that is keen
	// invests per visit; a neutral pilot at zero yield is half keen.
	investBaseFrac = 0.20
	investMaxFrac  = 0.30
	// The CEO invests as the founder, twice as freely: it is its outfit.
	investFounderMult = 2.0
	investWeekCap     = 50_000_000.0
	investMinISK      = 100_000.0
	// investHurdle is the yearly return a member wants before the stock itself
	// justifies a purchase; investYieldK is how hard a return above or below it pulls.
	investHurdle = 0.08
	investYieldK = 4.0

	// An ex-member sells a tranche of its stake back a week, the less of it
	// the more devoted it is.
	divestTrancheFrac = 0.25
	divestEverySec    = 7 * 86400
)

var (
	sharesOn  atomic.Bool
	shareBusy atomic.Bool
)

func init() { sharesOn.Store(true) }

// sharesEnabled is the dial, read on the hot paths.
func sharesEnabled() bool { return sharesOn.Load() }

func (m *Mod) loadShareSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingSharesEnabled); err == nil && ok {
		sharesOn.Store(truthySetting(v))
	}
}

func (m *Mod) sharePar(ctx context.Context) float64 {
	if v := escrowSettingFloat(ctx, m.db, settingSharesPar, shareParDefault); v >= 1 {
		return v
	}
	return shareParDefault
}

func (m *Mod) shareMaxHolderPct(ctx context.Context) float64 {
	v := escrowSettingFloat(ctx, m.db, settingSharesMaxHolder, shareMaxHolderDefault)
	return math.Max(1, math.Min(100, v))
}

// ── the arithmetic (pure) ────────────────────────────────────────────────────

// shareQuote is a corporation's stock, priced.
type shareQuote struct {
	Issued, Treasury, Outstanding int64
	NAV, Earnings                 float64 // earnings per week
	Book, Fair                    float64
	Ask, Bid                      float64
	Premium, Par                  float64
}

// quoteShares prices a corporation's stock. Pure. Ask and bid are whole ISK:
// the ask rounded up and the bid down, so the spread never rounds against
// the corporation. The ask never falls below par; above it, it tracks NAV.
func quoteShares(nav, earnings float64, issued, treasury int64, par, premium, goodwillWeeks, haircut float64) shareQuote {
	q := shareQuote{Issued: issued, Treasury: treasury, NAV: nav, Earnings: earnings, Premium: premium, Par: par}
	q.Outstanding = issued - treasury
	if q.Outstanding <= 0 {
		q.Outstanding = 0
		q.Fair = par
		q.Ask = math.Ceil(par)
		return q
	}
	o := float64(q.Outstanding)
	q.Book = math.Max(0, nav) / o
	q.Fair = (math.Max(0, nav) + math.Max(0, earnings)*goodwillWeeks) / o
	q.Ask = math.Ceil(par + q.Fair*(1+premium))
	q.Bid = math.Floor(q.Book * (1 - haircut))
	return q
}

// treasurySellable is how much of the treasury the CEO will sell: all of it
// above the share of the issue it keeps for control.
func treasurySellable(issued, treasury int64, controlFloor float64) int64 {
	keep := int64(math.Ceil(controlFloor * float64(issued)))
	return max(0, treasury-keep)
}

// holderRoom is how many more shares one holder may buy under the per-holder
// cap. The CEO is not capped: it already votes the treasury.
func holderRoom(issued, held int64, maxPct float64, isCEO bool) int64 {
	if isCEO {
		return math.MaxInt64
	}
	limit := int64(math.Floor(maxPct / 100 * float64(issued)))
	return max(0, limit-held)
}

// adaptPremium is the CEO's weekly move on its price: up while the stock is
// in demand (a hot week, or buyers the desk turned away), down fast while it
// only flows back in, and down slowly while nothing moves. Pure.
func adaptPremium(premium, premiumMax float64, sold, sellable, redeemed int64, unmet float64) float64 {
	hot := unmet > 0 || (sold > 0 && float64(sold) >= shareHotSoldFrac*float64(sold+sellable))
	switch {
	case hot:
		premium += sharePremiumStep
	case sold == 0 && redeemed > 0:
		premium -= sharePremiumStep
	case sold == 0:
		premium -= sharePremiumDrift
	}
	return math.Max(0, math.Min(premium, premiumMax))
}

// investFor is what a member puts into its corporation's stock this visit. Loyalty
// and generosity buy whatever the stock does; yield (shareYield) above investHurdle
// makes anybody keen and below it cools the devoted. In a founding round the line
// drops to investFoundingAboveISK and yield is floored at the hurdle. Pure.
func investFor(balance float64, tn Tuning, founder, founding bool, investedThisWeek, yield float64) float64 {
	above := investAboveISK
	if founding {
		above = investFoundingAboveISK
		yield = math.Max(yield, investHurdle)
	}
	surplus := balance - above
	if surplus <= 0 {
		return 0
	}
	generosity := 1.0
	if gangShareFrac > 0 {
		generosity = tn.GangShareFrac / gangShareFrac
	}
	keen := 0.5*tn.Obedience*generosity + investYieldK*(yield-investHurdle)
	keen = math.Max(0, math.Min(keen, 1))
	frac := investBaseFrac * keen
	capISK := investWeekCap
	if founder {
		frac *= investFounderMult
		capISK *= investFounderMult
	}
	frac = math.Min(frac, investMaxFrac)
	amount := math.Min(surplus*frac, capISK-investedThisWeek)
	if amount < investMinISK {
		return 0
	}
	return math.Floor(amount)
}

// divestFor is how many shares an ex-member sells back this week: a tranche,
// smaller the more devoted it still is. Pure.
func divestFor(held int64, tn Tuning) int64 {
	if held <= 0 {
		return 0
	}
	keep := math.Max(0, math.Min(tn.Obedience/2, 1))
	n := int64(math.Ceil(float64(held) * divestTrancheFrac * (1 - keep)))
	return min(max(n, 0), held)
}

// ── reading a corporation ────────────────────────────────────────────────────

// corpShareCounts is the issue and the treasury.
func (m *Mod) corpShareCounts(ctx context.Context, corpID int64) (issued, treasury int64, err error) {
	if err = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(shares, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&issued); err != nil {
		return 0, 0, err
	}
	return issued, evedb.ShareHolding(ctx, m.db, corpID, corpID), nil
}

// corpNAV is what the corporation is worth: wallet less promises, plus office
// stock and library at a liquidity discount.
func (m *Mod) corpNAV(ctx context.Context, corpID int64) float64 {
	wallet, _ := evedb.GetCorpBalance(ctx, m.db, corpID)
	nav := wallet - m.corpCommitted(ctx, corpID)

	// Office stock, by type. Blueprints are the library's, valued below.
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(GREATEST(e.quantity, 1))
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE o.corporationID = ? AND e.ownerID = ? AND g.categoryID <> 9
		 GROUP BY e.typeID
		 ORDER BY SUM(GREATEST(e.quantity, 1)) DESC
		 LIMIT ?`, corpID, corpID, shareStockTypes)
	if err == nil {
		qty := map[int64]float64{}
		var ids []int64
		for rows.Next() {
			var typeID int64
			var n float64
			if rows.Scan(&typeID, &n) == nil {
				qty[typeID] = n
				ids = append(ids, typeID)
			}
		}
		rows.Close()
		if len(ids) > 0 {
			if vals, err := evedb.TypeValues(ctx, m.db, ids); err == nil {
				for id, n := range qty {
					nav += shareStockHaircut * n * vals[id].Value
				}
			}
		}
	}
	var library float64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost), 0) FROM rolebot_corp_library WHERE corporationID = ?`, corpID).Scan(&library)
	return nav + shareLibraryHaircut*library
}

// corpWeeklyEarnings is the books' trailing four weeks, capital flows left
// out, per week.
func (m *Mod) corpWeeklyEarnings(ctx context.Context, corpID int64) float64 {
	args := []any{corpID, clock.Unix() - shareEarningsWindowSec}
	for _, l := range capitalLines {
		args = append(args, l)
	}
	var net float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND at >= ? AND line NOT IN (`+placeholders(len(capitalLines))+`)`,
		args...).Scan(&net)
	return net / 4
}

// shareState is one rolebot_share_state row.
type shareState struct {
	Premium        float64
	WeekStart      int64
	WeekSold       int64
	WeekRedeemed   int64
	UnmetISK       float64
	FounderGranted bool
	LastDividendAt int64
}

func (m *Mod) loadShareState(ctx context.Context, corpID int64) shareState {
	var s shareState
	var fg int
	err := m.db.QueryRowContext(ctx, `
		SELECT premium, weekStart, weekSold, weekRedeemed, unmetISK, founderGranted, lastDividendAt
		  FROM rolebot_share_state WHERE corporationID = ?`, corpID).
		Scan(&s.Premium, &s.WeekStart, &s.WeekSold, &s.WeekRedeemed, &s.UnmetISK, &fg, &s.LastDividendAt)
	if errors.Is(err, sql.ErrNoRows) {
		now := clock.Unix()
		_, _ = m.db.ExecContext(ctx,
			`INSERT IGNORE INTO rolebot_share_state (corporationID, weekStart) VALUES (?, ?)`, corpID, now)
		s.WeekStart = now
	}
	s.FounderGranted = fg != 0
	return s
}

// noteShareFlow adds to this week's counters. Increments, so two desks
// writing at once cannot lose each other's numbers.
func (m *Mod) noteShareFlow(ctx context.Context, corpID, sold, redeemed int64, unmet float64) {
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_share_state (corporationID, weekStart, weekSold, weekRedeemed, unmetISK)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE weekSold = weekSold + VALUES(weekSold),
		  weekRedeemed = weekRedeemed + VALUES(weekRedeemed), unmetISK = unmetISK + VALUES(unmetISK)`,
		corpID, clock.Unix(), sold, redeemed, unmet)
}

// A quote is a few queries and an office valuation; the desk and the page ask
// for the same one many times a minute, and it moves only with a trade (which
// forgets it) or the books.
var (
	shareQuoteMu    sync.Mutex
	shareQuoteCache = map[int64]cachedQuote{}
)

type cachedQuote struct {
	q  shareQuote
	at int64
}

func forgetShareQuote(corpID int64) {
	shareQuoteMu.Lock()
	delete(shareQuoteCache, corpID)
	shareQuoteMu.Unlock()
}

// corpShareQuote is the corporation's current price.
func (m *Mod) corpShareQuote(ctx context.Context, corpID int64, ch Charter) (shareQuote, error) {
	now := clock.Unix()
	shareQuoteMu.Lock()
	if c, ok := shareQuoteCache[corpID]; ok && now-c.at < shareQuoteTTLSec {
		shareQuoteMu.Unlock()
		return c.q, nil
	}
	shareQuoteMu.Unlock()
	issued, treasury, err := m.corpShareCounts(ctx, corpID)
	if err != nil {
		return shareQuote{}, err
	}
	st := m.loadShareState(ctx, corpID)
	q := quoteShares(m.corpNAV(ctx, corpID), m.corpWeeklyEarnings(ctx, corpID), issued, treasury,
		m.sharePar(ctx), st.Premium, ch.GoodwillWeeks, ch.Haircut)
	shareQuoteMu.Lock()
	shareQuoteCache[corpID] = cachedQuote{q, now}
	shareQuoteMu.Unlock()
	return q, nil
}

// shareDay is the day number the book snapshots are keyed on.
func shareDay(unix int64) int64 { return unix / 86400 }

// shareYield is the stock's trailing yearly return on its ask: four weeks of
// dividends per share and four weeks of book growth, annualised.
func (m *Mod) shareYield(ctx context.Context, corpID int64, q shareQuote) float64 {
	if q.Ask <= 0 || q.Outstanding <= 0 {
		return 0
	}
	var paid float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(-SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND line = ? AND at >= ?`,
		corpID, lineDividend, clock.Unix()-shareEarningsWindowSec).Scan(&paid)
	growth := 0.0
	var old float64
	if err := m.db.QueryRowContext(ctx, `
		SELECT book FROM rolebot_share_book
		 WHERE corporationID = ? AND day <= ? ORDER BY day DESC LIMIT 1`,
		corpID, shareDay(clock.Unix())-28).Scan(&old); err == nil && old > 0 {
		growth = q.Book - old
	}
	return (paid/float64(q.Outstanding) + growth) * shareGrowthPeriods / q.Ask
}

// ── moving stock ─────────────────────────────────────────────────────────────

// announceShares pushes a register change to every open client.
func announceShares(corpID int64, deltas []evedb.ShareDelta, oldTotal, newTotal int64) {
	forgetShareQuote(corpID)
	if h := modapi.Corp(); h.SharesChanged != nil {
		h.SharesChanged(corpID, deltas, oldTotal, newTotal)
	}
}

// pushWallet shows a pilot its new balance, if it is online to see it.
func (m *Mod) pushWallet(ctx context.Context, charID int64) {
	h := modapi.Corp()
	if h.AccountChanged == nil {
		return
	}
	if bal, err := evedb.GetCharBalance(ctx, m.db, charID); err == nil {
		h.AccountChanged(charID, bal)
	}
}

// pushBalance shows a human player its new balance from a transfer that already
// returned it, with no read. A bot has no client to show it to.
func (m *Mod) pushBalance(charID int64, bal float64) {
	if charID == 0 || m.isSynthetic(charID) {
		return
	}
	if h := modapi.Corp(); h.AccountChanged != nil {
		h.AccountChanged(charID, bal)
	}
}

// creditEquity records money going into (contributed) or out of (paidOut) a
// holder's stake.
func (m *Mod) creditEquity(ctx context.Context, corpID, holderID int64, in, out float64) {
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_equity (corporationID, characterID, contributed, paidOut, lastAt)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE contributed = contributed + VALUES(contributed),
		  paidOut = paidOut + VALUES(paidOut), lastAt = VALUES(lastAt)`,
		corpID, holderID, in, out, clock.Unix()); err != nil {
		rlogf("rolebots: corp equity corp=%d holder=%d: %v", corpID, holderID, err)
	}
}

// shareSaleSize is how many shares a buyer with this budget gets: what it can
// afford, what the treasury will sell, what the cap leaves it. short is the
// demand the TREASURY turned away (not the cap), in shares.
func shareSaleSize(budget, ask float64, sellable, room int64) (qty, short int64) {
	if ask <= 0 {
		return 0, 0
	}
	want := min(int64(math.Floor(budget/ask)), room)
	if want <= 0 {
		return 0, 0
	}
	qty = min(want, sellable)
	return qty, want - qty
}

// sellShares sells treasury stock to a buyer for ISK it has not paid yet (a bot on
// its visit home). One transaction moves both and nothing moves when the buyer
// cannot pay. Returns the shares and the ISK.
func (m *Mod) sellShares(ctx context.Context, c *corpCEO, ch Charter, buyerID int64, budget float64, who string) (int64, float64, error) {
	corpID := c.CorpID
	q, err := m.corpShareQuote(ctx, corpID, ch)
	if err != nil {
		return 0, 0, err
	}
	held := evedb.ShareHolding(ctx, m.db, buyerID, corpID)
	qty, short := shareSaleSize(budget, q.Ask,
		treasurySellable(q.Issued, q.Treasury, ch.ControlFloor),
		holderRoom(q.Issued, held, m.shareMaxHolderPct(ctx), buyerID == c.CharID))
	if short > 0 {
		m.noteShareFlow(ctx, corpID, 0, 0, float64(short)*q.Ask)
	}
	if qty <= 0 {
		return 0, 0, nil
	}
	cost := float64(qty) * q.Ask
	name := corpNameOf(ctx, m.db, corpID)
	mv, err := evedb.TradeShares(ctx, m.db, corpID, corpID, buyerID, qty, cost, refShares,
		fmt.Sprintf("%d shares of %s", qty, name))
	if err != nil {
		return 0, 0, err
	}
	announceShares(corpID, mv.Deltas(corpID, buyerID), 0, 0)
	m.creditEquity(ctx, corpID, buyerID, cost, 0)
	m.noteShareFlow(ctx, corpID, qty, 0, 0)
	m.corpBook(ctx, corpID, lineShares, cost, buyerID, fmt.Sprintf("%d shares to %s", qty, who))
	m.ceoLog(ctx, c, powerShares, "sharesale", buyerID, cost,
		"sold %d shares to %s at %s", qty, who, iskString(q.Ask))
	return qty, cost, nil
}

// buybackAllowance is what the corporation will still spend buying shares
// back this week: its charter's share of free capital, less what it already
// has.
func (m *Mod) buybackAllowance(ctx context.Context, corpID int64, ch Charter) float64 {
	free, _, err := m.corpFreeCapital(ctx, corpID, ch)
	if err != nil {
		return 0
	}
	var spent float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(-SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND line = ? AND at >= ?`,
		corpID, lineRedeem, clock.Unix()-shareWeekSec).Scan(&spent)
	return math.Max(0, math.Floor(ch.BuybackWeekly*(free+spent)-spent))
}

// redeemShares buys up to qty of a holder's shares back into the treasury at
// the bid, as far as this week's allowance runs. Returns the shares and ISK.
func (m *Mod) redeemShares(ctx context.Context, c *corpCEO, ch Charter, holderID, qty int64, who string) (int64, float64, error) {
	corpID := c.CorpID
	q, err := m.corpShareQuote(ctx, corpID, ch)
	if err != nil || q.Bid < 1 {
		return 0, 0, err
	}
	held := evedb.ShareHolding(ctx, m.db, holderID, corpID)
	afford := int64(m.buybackAllowance(ctx, corpID, ch) / q.Bid)
	qty = min(qty, held, afford)
	if qty <= 0 {
		return 0, 0, nil
	}
	proceeds := float64(qty) * q.Bid
	mv, err := evedb.TradeShares(ctx, m.db, corpID, holderID, corpID, qty, proceeds, refShares,
		fmt.Sprintf("Buyback of %d shares of %s", qty, corpNameOf(ctx, m.db, corpID)))
	if err != nil {
		return 0, 0, err
	}
	announceShares(corpID, mv.Deltas(holderID, corpID), 0, 0)
	m.creditEquity(ctx, corpID, holderID, 0, proceeds)
	m.noteShareFlow(ctx, corpID, 0, qty, 0)
	m.corpBook(ctx, corpID, lineRedeem, -proceeds, holderID, fmt.Sprintf("%d shares from %s", qty, who))
	m.ceoLog(ctx, c, powerShares, "redeem", holderID, proceeds,
		"bought %d shares back from %s at %s", qty, who, iskString(q.Bid))
	return qty, proceeds, nil
}

// grantShares moves treasury stock to a holder at no cost: the founder's stake, a
// print paid in kind, the one-off conversion of old donations. It issues the
// shortfall when issue is set and the treasury is short.
func (m *Mod) grantShares(ctx context.Context, corpID, holderID, qty int64, issue bool) (int64, error) {
	if qty <= 0 || holderID == corpID {
		return 0, nil
	}
	treasury := evedb.ShareHolding(ctx, m.db, corpID, corpID)
	if treasury < qty {
		if !issue {
			qty = treasury
		} else {
			is, err := evedb.IssueCorpShares(ctx, m.db, corpID, qty-treasury)
			if err != nil {
				return 0, err
			}
			announceShares(corpID, []evedb.ShareDelta{{HolderID: corpID, Old: is.OldTreasury, New: is.NewTreasury}},
				is.OldTotal, is.NewTotal)
		}
	}
	if qty <= 0 {
		return 0, nil
	}
	mv, err := evedb.MoveShares(ctx, m.db, corpID, corpID, holderID, qty)
	if err != nil {
		return 0, err
	}
	announceShares(corpID, mv.Deltas(corpID, holderID), 0, 0)
	return qty, nil
}

// creditSharesInKind pays for a print given to the library in stock: its
// value at the ask, out of what the treasury will sell. Returns the shares.
func (m *Mod) creditSharesInKind(ctx context.Context, corpID, holderID int64, value float64, what string) int64 {
	if !sharesEnabled() || value <= 0 {
		return 0
	}
	c, _ := getCEO(ctx, m.db, corpID)
	if c == nil {
		return 0
	}
	ch := ceoCharterOf(ctx, m, c)
	q, err := m.corpShareQuote(ctx, corpID, ch)
	if err != nil {
		return 0
	}
	held := evedb.ShareHolding(ctx, m.db, holderID, corpID)
	qty, short := shareSaleSize(value, q.Ask,
		treasurySellable(q.Issued, q.Treasury, ch.ControlFloor),
		holderRoom(q.Issued, held, m.shareMaxHolderPct(ctx), holderID == c.CharID))
	if short > 0 {
		m.noteShareFlow(ctx, corpID, 0, 0, float64(short)*q.Ask)
	}
	got, err := m.grantShares(ctx, corpID, holderID, qty, false)
	if err != nil || got == 0 {
		return 0
	}
	cost := float64(got) * q.Ask
	m.creditEquity(ctx, corpID, holderID, cost, 0)
	m.noteShareFlow(ctx, corpID, got, 0, 0)
	m.ceoLog(ctx, c, powerShares, "sharesale", holderID, cost,
		"paid %d shares to %s for %s", got, m.charNameOr(ctx, holderID), what)
	return got
}

// ── members on their visit home ──────────────────────────────────────────────

// investInCorp is a docked member's visit-home purchase of its managed
// corporation's stock, out of whatever its shopping left over. Silent and
// cheap for everyone else: the CEO index is in memory.
func (m *Mod) investInCorp(ctx context.Context, b *botRun) {
	if b == nil || b.CorpID == 0 || !sharesEnabled() {
		return
	}
	ceoID, ok := managedCEO(b.CorpID)
	if !ok {
		return
	}
	c, _ := getCEO(ctx, m.db, b.CorpID)
	if c == nil || c.Paused || c.PowersOff[powerShares] {
		return
	}
	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil || balance <= investFoundingAboveISK {
		return
	}
	ch := ceoCharterOf(ctx, m, c)
	corpBal, err := evedb.GetCorpBalance(ctx, m.db, b.CorpID)
	if err != nil {
		return
	}
	founding := corpBal < m.corpShortLine(ctx, b.CorpID, ch)
	if !founding && balance <= investAboveISK {
		return
	}
	q, err := m.corpShareQuote(ctx, b.CorpID, ch)
	if err != nil {
		return
	}
	var spent float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND line = ? AND counterID = ? AND at >= ?`,
		b.CorpID, lineShares, b.CharID, clock.Unix()-shareWeekSec).Scan(&spent)
	founder := b.CharID == ceoID
	budget := investFor(balance, b.Tune, founder, founding, spent, m.shareYield(ctx, b.CorpID, q))
	if budget <= 0 {
		return
	}
	// A listed corporation does not sell at the desk: the member buys on
	// the SCC Exchange, where its treasury's offering is one of the asks.
	if row, listed := m.sccListed(ctx, b.CorpID); listed {
		m.sccBuyOwn(ctx, b, row, budget)
		return
	}
	who := "a member"
	if founder {
		who = "its founder"
	}
	qty, cost, err := m.sellShares(ctx, c, ch, b.CharID, budget, m.charNameOr(ctx, b.CharID))
	if err != nil {
		if !errors.Is(err, evedb.ErrInsufficientFunds) {
			m.logBot(b.CharID, logError, "corp", "buying its corporation's shares: %v", err)
		}
		return
	}
	if qty > 0 {
		m.logBot(b.CharID, logInfo, "corp", "bought %d shares of its corporation for %s as %s (%s each)",
			qty, iskString(cost), who, iskString(cost/float64(qty)))
	}
}

// divestShares is an ex-member selling a tranche of an old corporation's
// stock back, a week at a time. A member keeps what it holds in its own.
func (m *Mod) divestShares(ctx context.Context, b *botRun) {
	if b == nil || !sharesEnabled() {
		return
	}
	for _, h := range m.botHoldings(ctx, b.CharID) {
		if h.corpID == b.CorpID || clock.Unix()-h.lastAt < divestEverySec {
			continue
		}
		c, _ := getCEO(ctx, m.db, h.corpID)
		if c == nil || c.Paused || c.PowersOff[powerShares] {
			continue
		}
		n := divestFor(h.shares, b.Tune)
		if n <= 0 {
			continue
		}
		if _, listed := m.sccListed(ctx, h.corpID); listed {
			// Listed: the portfolio review owns the decision (sccbots.go). This path reads
			// every holding as an ex-member's stake, which is wrong for an investor's.
			continue
		}
		ch := ceoCharterOf(ctx, m, c)
		qty, got, err := m.redeemShares(ctx, c, ch, b.CharID, n, m.charNameOr(ctx, b.CharID))
		// Stamp the week whether or not the corporation could pay: an
		// ex-member asks once a week, it does not queue at the door.
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_equity SET lastAt = ?
			WHERE corporationID = ? AND characterID = ?`, clock.Unix(), h.corpID, b.CharID)
		if err != nil {
			m.logBot(b.CharID, logError, "corp", "selling shares back to corp %d: %v", h.corpID, err)
			continue
		}
		if qty > 0 {
			m.logBot(b.CharID, logInfo, "corp", "sold %d shares of its old corporation %s back for %s",
				qty, corpNameOf(ctx, m.db, h.corpID), iskString(got))
		}
	}
}

// liquidateShares is the last thing a destitute pilot sells: its stakes, back
// to their corporations at the bid, until it is back to target.
func (m *Mod) liquidateShares(ctx context.Context, b *botRun, target float64) {
	if b == nil || !sharesEnabled() {
		return
	}
	for _, h := range m.botHoldings(ctx, b.CharID) {
		bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
		if err != nil || bal >= target {
			return
		}
		if row, listed := m.sccListed(ctx, h.corpID); listed && row.Price >= 1 {
			// Listed: sell into the book now, at whatever the band allows.
			n := min(h.shares, int64(math.Ceil((target-bal)/row.Price)))
			if got := m.sccSellHolding(ctx, b, row, n, true); got > 0 {
				m.logBot(b.CharID, logInfo, "market", "was down to %s — sold %d shares of [%s] on the SCC Exchange",
					iskString(bal), got, row.Ticker)
			}
			continue
		}
		c, _ := getCEO(ctx, m.db, h.corpID)
		if c == nil || c.Paused || c.PowersOff[powerShares] {
			continue
		}
		ch := ceoCharterOf(ctx, m, c)
		q, err := m.corpShareQuote(ctx, h.corpID, ch)
		if err != nil || q.Bid < 1 {
			continue
		}
		n := int64(math.Ceil((target - bal) / q.Bid))
		if qty, got, err := m.redeemShares(ctx, c, ch, b.CharID, n, m.charNameOr(ctx, b.CharID)); err == nil && qty > 0 {
			m.logBot(b.CharID, logInfo, "market",
				"was down to %s — sold %d shares of %s back to the corporation for %s",
				iskString(bal), qty, corpNameOf(ctx, m.db, h.corpID), iskString(got))
		}
	}
}

type botHolding struct {
	corpID, shares, lastAt int64
}

// botHoldings is every managed corporation's stock a pilot holds.
func (m *Mod) botHoldings(ctx context.Context, charID int64) []botHolding {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.corporationID, s.shares, COALESCE(e.lastAt, 0)
		  FROM crpcharshares s
		  LEFT JOIN rolebot_corp_equity e ON e.corporationID = s.corporationID AND e.characterID = s.characterID
		 WHERE s.characterID = ? AND s.corporationID <> s.characterID AND s.shares > 0`, charID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []botHolding
	for rows.Next() {
		var h botHolding
		if rows.Scan(&h.corpID, &h.shares, &h.lastAt) == nil {
			if _, ok := managedCEO(h.corpID); ok {
				out = append(out, h)
			}
		}
	}
	return out
}

// ── the CEO's power ──────────────────────────────────────────────────────────

// ceoShares is the share desk's review: the founder's stake, the day's book value,
// the premium, the IOUs, and more stock when the treasury runs dry. Only a review
// recomputes the quote (NAV and four weeks of ledger); between reviews the shared
// quote (corpShareQuote's memo, or the last review's) serves.
func (m *Mod) ceoShares(ctx context.Context, c *corpCEO, ch Charter, review bool) error {
	if !sharesEnabled() {
		return nil
	}
	corpID := c.CorpID
	st := m.loadShareState(ctx, corpID)
	now := clock.Unix()

	if !st.FounderGranted {
		if err := m.grantFounderStake(ctx, c, ch); err != nil {
			return err
		}
	}

	if review {
		forgetShareQuote(corpID)
	}
	q, err := m.corpShareQuote(ctx, corpID, ch)
	if err != nil {
		return err
	}
	if review && q.Outstanding > 0 {
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_share_book (corporationID, day, book) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE book = VALUES(book)`, corpID, shareDay(now), q.Book)
		_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_share_book WHERE corporationID = ? AND day < ?`,
			corpID, shareDay(now)-shareBookKeepDays)
	}

	sellable := treasurySellable(q.Issued, q.Treasury, ch.ControlFloor)
	if review && now-st.WeekStart >= shareWeekSec {
		p := adaptPremium(st.Premium, ch.PremiumMax, st.WeekSold, sellable, st.WeekRedeemed, st.UnmetISK)
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_share_state SET premium = ?, weekStart = ?, weekSold = 0, weekRedeemed = 0, unmetISK = 0
			 WHERE corporationID = ?`, p, now, corpID); err != nil {
			return err
		}
		if math.Abs(p-st.Premium) > 1e-9 {
			m.ceoLog(ctx, c, powerShares, "premium", 0, 0,
				"set the share premium to %.0f%% (sold %d, bought back %d, turned away %s this week)",
				p*100, st.WeekSold, st.WeekRedeemed, iskString(st.UnmetISK))
			forgetShareQuote(corpID)
		}
	}

	// What the corporation owes holders who sold back comes before what it
	// hands its owners.
	m.payRedemptions(ctx, c, ch)
	// The dividend is paid by the Treasurer when there is one
	// (corpstaff_treasurer.go), on the same cadence.
	if !m.staffCovers(ctx, c, postTreasurer, "") {
		if _, err := m.payDividend(ctx, c, ch, false); err != nil {
			m.logBot(c.CharID, logError, "ceo", "dividend: %v", err)
		}
	}
	// The SCC Exchange: weigh a listing, or keep the treasury's orders on
	// the book once listed (sccbots.go).
	m.sccCEO(ctx, c, ch)
	return m.shareIssuance(ctx, c, ch, q, sellable, st)
}

// grantFounderStake gives the CEO its founder's share, once per corporation:
// a successor inherits the chair, not the founding.
func (m *Mod) grantFounderStake(ctx context.Context, c *corpCEO, ch Charter) error {
	issued, _, err := m.corpShareCounts(ctx, c.CorpID)
	if err != nil {
		return err
	}
	if evedb.ShareHolding(ctx, m.db, c.CharID, c.CorpID) == 0 {
		qty := int64(math.Round(ch.FounderFrac * float64(issued)))
		got, err := m.grantShares(ctx, c.CorpID, c.CharID, qty, false)
		if err != nil {
			return err
		}
		if got > 0 {
			m.ceoLog(ctx, c, powerShares, "founder", c.CharID, 0,
				"kept %d shares as the founder's stake", got)
		}
	}
	_, err = m.db.ExecContext(ctx,
		`UPDATE rolebot_share_state SET founderGranted = 1 WHERE corporationID = ?`, c.CorpID)
	return err
}

// ── humans ───────────────────────────────────────────────────────────────────

// startShareDesk runs the journal poller: a player's Give Money to a managed
// corporation with "shares" in the journal text is a purchase order.
func (m *Mod) startShareDesk() {
	m.registerPeriodic("rolebot-share-desk", shareDeskTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		if !shareBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("share desk", func() {
			defer shareBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			m.loadShareSettings(ctx)
			if !sharesEnabled() {
				return
			}
			m.pollShareDeposits(ctx)
		})
		return true
	})
}

// isShareOrder is whether a Give Money reason asks for shares.
func isShareOrder(reason string) bool {
	return strings.Contains(strings.ToLower(reason), "share")
}

// pollShareDeposits reads the journal past its cursor for share orders.
func (m *Mod) pollShareDeposits(ctx context.Context) {
	var cursor int64
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingSharesCursor); err != nil {
		return
	} else if ok {
		cursor, _ = strconv.ParseInt(v, 10, 64)
	} else {
		// First run: start at the end. A Give Money sent before the desk
		// existed was a gift, and is not re-read as an order.
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(refID), 0) FROM market_journal`).Scan(&cursor)
		_ = evedb.SetServerSetting(ctx, m.db, settingSharesCursor, strconv.FormatInt(cursor, 10))
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT refID, characterID, ownerID1, amount, COALESCE(reason, '')
		  FROM market_journal
		 WHERE refID > ? AND refTypeID = ? AND amount > 0 AND characterID = ownerID2
		 ORDER BY refID LIMIT ?`, cursor, refPlayerDonation, shareDeskBatch)
	if err != nil {
		rlogf("rolebots: share desk journal: %v", err)
		return
	}
	type order struct {
		refID, corpID, charID int64
		amount                float64
	}
	var orders []order
	last := cursor
	for rows.Next() {
		var o order
		var reason string
		if rows.Scan(&o.refID, &o.corpID, &o.charID, &o.amount, &reason) != nil {
			continue
		}
		last = o.refID
		if _, ok := managedCEO(o.corpID); ok && isShareOrder(reason) {
			orders = append(orders, o)
		}
	}
	rows.Close()
	for _, o := range orders {
		if m.isRostered(ctx, o.charID) {
			continue // a bot buys through sellShares, never by giving money
		}
		m.fillShareDeposit(ctx, o.corpID, o.charID, o.amount)
		// Saved per order, so a crash mid-batch cannot fill one twice.
		_ = evedb.SetServerSetting(ctx, m.db, settingSharesCursor, strconv.FormatInt(o.refID, 10))
	}
	if last > cursor {
		_ = evedb.SetServerSetting(ctx, m.db, settingSharesCursor, strconv.FormatInt(last, 10))
	}
}

// fillShareDeposit turns a pilot's money, already in the corporation's
// wallet, into shares at the ask, and refunds the rest.
func (m *Mod) fillShareDeposit(ctx context.Context, corpID, charID int64, amount float64) {
	c, _ := getCEO(ctx, m.db, corpID)
	if c == nil {
		return
	}
	name := corpNameOf(ctx, m.db, corpID)
	refund := func(isk float64) {
		if isk < shareMinTradeISK {
			return
		}
		if _, _, err := evedb.TransferISK(ctx, m.db, corpID, charID, math.Floor(isk), refShares,
			"Share order refund: "+name); err != nil {
			rlogf("rolebots: share refund corp=%d char=%d: %v", corpID, charID, err)
			return
		}
		m.pushWallet(ctx, charID)
	}
	var member int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(corporationID, 0) FROM character_ WHERE characterID = ?`, charID).Scan(&member)
	if member != corpID {
		refund(amount)
		m.mailShareReceipt(ctx, c, charID, "Your share order was refunded",
			fmt.Sprintf("Only members of <b>%s</b> may buy its shares. Your %s has been returned.",
				name, iskString(amount)))
		return
	}
	if c.Paused || c.PowersOff[powerShares] {
		refund(amount)
		m.mailShareReceipt(ctx, c, charID, "Your share order was refunded",
			fmt.Sprintf("<b>%s</b> is not selling shares at the moment. Your %s has been returned.",
				name, iskString(amount)))
		return
	}
	if row, listed := m.sccListed(ctx, corpID); listed {
		refund(amount)
		m.mailShareReceipt(ctx, c, charID, "Your share order was refunded",
			fmt.Sprintf("<b>%s</b> is listed on the SCC Exchange [%s], so its shares are bought and sold there "+
				"now: open the Exchange page in the in-game browser. Your %s has been returned.",
				name, row.Ticker, iskString(amount)))
		return
	}
	ch := ceoCharterOf(ctx, m, c)
	forgetShareQuote(corpID)
	q, err := m.corpShareQuote(ctx, corpID, ch)
	if err != nil {
		return
	}
	// The deposit is already in the wallet the quote just valued; price the
	// order against the corporation as it was before it arrived.
	q = quoteShares(q.NAV-amount, q.Earnings, q.Issued, q.Treasury, q.Par, q.Premium, ch.GoodwillWeeks, ch.Haircut)
	held := evedb.ShareHolding(ctx, m.db, charID, corpID)
	qty, short := shareSaleSize(amount, q.Ask,
		treasurySellable(q.Issued, q.Treasury, ch.ControlFloor),
		holderRoom(q.Issued, held, m.shareMaxHolderPct(ctx), charID == c.CharID))
	if short > 0 {
		m.noteShareFlow(ctx, corpID, 0, 0, float64(short)*q.Ask)
	}
	var got int64
	if qty > 0 {
		if got, err = m.grantShares(ctx, corpID, charID, qty, false); err != nil {
			rlogf("rolebots: share order corp=%d char=%d: %v", corpID, charID, err)
			got = 0
		}
	}
	cost := float64(got) * q.Ask
	if got > 0 {
		m.creditEquity(ctx, corpID, charID, cost, 0)
		m.noteShareFlow(ctx, corpID, got, 0, 0)
		who := m.charNameOr(ctx, charID)
		m.corpBook(ctx, corpID, lineShares, cost, charID, fmt.Sprintf("%d shares to %s", got, who))
		m.ceoLog(ctx, c, powerShares, "sharesale", charID, cost, "sold %d shares to %s at %s", got, who, iskString(q.Ask))
	}
	refund(amount - cost)
	body := fmt.Sprintf("You bought <b>%d</b> shares of <b>%s</b> at %s each (%s).", got, name, iskString(q.Ask), iskString(cost))
	if got == 0 {
		body = fmt.Sprintf("No shares of <b>%s</b> could be sold to you at %s each.", name, iskString(q.Ask))
	}
	if left := amount - cost; left >= shareMinTradeISK {
		body += fmt.Sprintf("<br><br>%s has been refunded", iskString(math.Floor(left)))
		switch {
		case short > 0:
			body += " — the treasury has no more stock to sell right now."
		case qty < int64(math.Floor(amount/q.Ask)):
			body += fmt.Sprintf(" — no one but the CEO may hold more than %.0f%% of the corporation.", m.shareMaxHolderPct(ctx))
		default:
			body += " — the remainder of your order."
		}
	}
	m.mailShareReceipt(ctx, c, charID, "Share purchase: "+name, body)
}

// onShareMoved is esp.ShareMovedHook: a pilot giving shares to the
// corporation that issued them is selling them back. Off the RPC goroutine.
func (m *Mod) onShareMoved(stockCorp, from, to, qty int64) {
	if to != stockCorp || from == stockCorp || qty <= 0 || !sharesEnabled() {
		return
	}
	if _, ok := managedCEO(stockCorp); !ok {
		return
	}
	goSafe("share buyback", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		m.buyBackGiven(ctx, stockCorp, from, qty)
	})
}

// buyBackGiven pays a pilot for shares it has already given the corporation:
// what this week's allowance covers now, the rest as an IOU.
func (m *Mod) buyBackGiven(ctx context.Context, corpID, holderID, qty int64) {
	if evedb.ShareOwnerKind(ctx, m.db, holderID) != "character" || m.isRostered(ctx, holderID) {
		return // a corporation or a bot giving shares: a gift
	}
	c, _ := getCEO(ctx, m.db, corpID)
	if c == nil || c.Paused || c.PowersOff[powerShares] {
		return
	}
	ch := ceoCharterOf(ctx, m, c)
	forgetShareQuote(corpID)
	q, err := m.corpShareQuote(ctx, corpID, ch)
	if err != nil {
		return
	}
	// The shares are already back in the treasury; price them as the
	// holding they were.
	q = quoteShares(q.NAV, q.Earnings, q.Issued, q.Treasury-qty, q.Par, q.Premium, ch.GoodwillWeeks, ch.Haircut)
	name := corpNameOf(ctx, m.db, corpID)
	total := float64(qty) * q.Bid
	pay := math.Min(total, m.buybackAllowance(ctx, corpID, ch))
	who := m.charNameOr(ctx, holderID)
	if pay >= shareMinTradeISK {
		if _, _, err := evedb.TransferISK(ctx, m.db, corpID, holderID, pay, refShares,
			fmt.Sprintf("Buyback of %d shares of %s", qty, name)); err != nil {
			rlogf("rolebots: share buyback corp=%d holder=%d: %v", corpID, holderID, err)
			pay = 0
		} else {
			m.pushWallet(ctx, holderID)
			m.creditEquity(ctx, corpID, holderID, 0, pay)
			m.corpBook(ctx, corpID, lineRedeem, -pay, holderID, fmt.Sprintf("%d shares from %s", qty, who))
		}
	} else {
		pay = 0
	}
	owed := math.Floor(total - pay)
	if owed >= shareMinTradeISK {
		res, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_share_redemptions (corporationID, holderID, shares, price, owed, createdAt)
			VALUES (?, ?, ?, ?, ?, ?)`, corpID, holderID, qty, q.Bid, owed, clock.Unix())
		if err == nil {
			if id, err := res.LastInsertId(); err == nil {
				_ = m.openCommit(ctx, corpID, "shareredeem", id, owed)
			}
		} else {
			rlogf("rolebots: share IOU corp=%d holder=%d: %v", corpID, holderID, err)
		}
	}
	m.noteShareFlow(ctx, corpID, 0, qty, 0)
	m.ceoLog(ctx, c, powerShares, "redeem", holderID, total,
		"bought %d shares back from %s at %s (%s paid now)", qty, who, iskString(q.Bid), iskString(pay))
	body := fmt.Sprintf("<b>%s</b> bought back your %d shares at %s each (%s).", name, qty, iskString(q.Bid), iskString(total))
	switch {
	case total < shareMinTradeISK:
		body = fmt.Sprintf("<b>%s</b> took back your %d shares. Its book value is nothing, so there was nothing to pay.", name, qty)
	case owed >= shareMinTradeISK:
		body += fmt.Sprintf("<br><br>%s has been paid now. The corporation owes you the other %s and will pay it "+
			"as its buyback budget allows.", iskString(pay), iskString(owed))
	}
	m.mailShareReceipt(ctx, c, holderID, "Share sale: "+name, body)
}

// payRedemptions pays the corporation's IOUs, oldest first, out of this
// week's buyback allowance.
func (m *Mod) payRedemptions(ctx context.Context, c *corpCEO, ch Charter) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT redemptionID, holderID, shares, owed FROM rolebot_share_redemptions
		 WHERE corporationID = ? AND paidAt = 0 ORDER BY redemptionID`, c.CorpID)
	if err != nil {
		return
	}
	type iou struct {
		id, holderID, shares int64
		owed                 float64
	}
	var ious []iou
	for rows.Next() {
		var r iou
		if rows.Scan(&r.id, &r.holderID, &r.shares, &r.owed) == nil {
			ious = append(ious, r)
		}
	}
	rows.Close()
	name := corpNameOf(ctx, m.db, c.CorpID)
	for _, r := range ious {
		pay := math.Floor(math.Min(r.owed, m.buybackAllowance(ctx, c.CorpID, ch)))
		if pay < shareMinTradeISK {
			return
		}
		if _, _, err := evedb.TransferISK(ctx, m.db, c.CorpID, r.holderID, pay, refShares,
			fmt.Sprintf("Buyback of %d shares of %s", r.shares, name)); err != nil {
			rlogf("rolebots: share IOU %d: %v", r.id, err)
			return
		}
		m.pushWallet(ctx, r.holderID)
		m.creditEquity(ctx, c.CorpID, r.holderID, 0, pay)
		m.corpBook(ctx, c.CorpID, lineRedeem, -pay, r.holderID, "buyback owed to "+m.charNameOr(ctx, r.holderID))
		m.spendCommit(ctx, "shareredeem", r.id, pay)
		if r.owed-pay < shareMinTradeISK {
			_, _ = m.db.ExecContext(ctx,
				`UPDATE rolebot_share_redemptions SET owed = 0, paidAt = ? WHERE redemptionID = ?`, clock.Unix(), r.id)
			m.closeCommit(ctx, "shareredeem", r.id)
		} else {
			_, _ = m.db.ExecContext(ctx,
				`UPDATE rolebot_share_redemptions SET owed = owed - ? WHERE redemptionID = ?`, pay, r.id)
		}
		m.ceoLog(ctx, c, powerShares, "redeem", r.holderID, pay, "paid %s of a buyback owed to %s",
			iskString(pay), m.charNameOr(ctx, r.holderID))
	}
}

// mailShareReceipt is a letter from the CEO about a trade.
func (m *Mod) mailShareReceipt(ctx context.Context, c *corpCEO, charID int64, subject, body string) {
	from, _ := m.letterFrom(ctx, c, mailFromTreasury)
	m.mailPilot(ctx, charID, from, subject, body)
}

// ── more stock ───────────────────────────────────────────────────────────────

// shareIssuance puts a share issue to the vote when the treasury has nothing left
// to sell and buyers were turned away, and enacts any issue that passed. The vote
// is the client's voteShares case, weighed in shares: the CEO votes the treasury,
// so it carries the vote alone until members hold more than the treasury.
func (m *Mod) shareIssuance(ctx context.Context, c *corpCEO, ch Charter, q shareQuote, sellable int64, st shareState) error {
	now := evedb.UnixToWin32(clock.Unix())
	actions, err := evedb.GetCorpSanctionedActionRows(ctx, m.db, c.CorpID, now)
	if err != nil {
		return err
	}
	for _, a := range actions {
		if a.VoteType != evedb.VoteShares || a.ActedUpon != 0 {
			continue
		}
		m.enactShareIssue(ctx, c, a.VoteCaseID)
	}

	if q.Issued <= 0 || float64(sellable) >= shareIssueLowFrac*float64(q.Issued) || st.UnmetISK <= 0 {
		return nil
	}
	if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "shareissue") < shareIssueCooldown {
		return nil
	}
	var open int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM crpVoteCases
		 WHERE corporationID = ? AND voteType = ? AND endDateTime > ?`,
		c.CorpID, evedb.VoteShares, now).Scan(&open)
	if open > 0 {
		return nil
	}
	n := shareIssueSize(q.Issued, q.Treasury, ch.ControlFloor, shareIssueStep(ch))
	text := fmt.Sprintf("Create %d shares", n)
	desc := fmt.Sprintf("The treasury has %d shares left to sell and members have asked for more. "+
		"New shares go into the treasury and are sold at the current price (%s).", sellable, iskString(q.Ask))
	caseID, err := evedb.InsertVoteCaseRow(ctx, m.db, c.CorpID, text, desc, evedb.VoteShares,
		now, now+shareVoteSec*win32TicksPerSec, []evedb.VoteCaseOption{
			{Text: text, Parameter: sql.NullInt64{Int64: n, Valid: true}},
			{Text: "Do not create shares"},
		})
	if err != nil {
		return fmt.Errorf("share issue vote: %w", err)
	}
	if err := evedb.InsertVoteRow(ctx, m.db, caseID, c.CharID, 0); err != nil && !errors.Is(err, evedb.ErrAlreadyVoted) {
		return fmt.Errorf("share issue ballot: %w", err)
	}
	announceShares(c.CorpID, nil, 0, 0) // re-weighs the new case's options
	m.ceoLog(ctx, c, powerShares, "shareissue", caseID, float64(n),
		"put an issue of %d shares to the vote (%s turned away this week)", n, iskString(st.UnmetISK))
	return nil
}

// shareIssueStep is how big an issue the CEO asks for, as a share of the
// current issue.
func shareIssueStep(ch Charter) float64 {
	return math.Max(0.10, math.Min(0.5, 0.25*(1+ch.PremiumMax)))
}

// shareIssueSize is how many shares an issue creates so the treasury has step x
// issued left to sell afterwards. The control floor keeps its share of every new
// share too, so an issue sized off the issue alone could leave the treasury
// under the floor with nothing to sell. Pure.
func shareIssueSize(issued, treasury int64, controlFloor, step float64) int64 {
	target := math.Round(float64(issued) * step)
	n := target
	if controlFloor > 0 && controlFloor < 1 {
		// (treasury + n) − floor·(issued + n) ≥ target
		cover := (target + controlFloor*float64(issued) - float64(treasury)) / (1 - controlFloor)
		n = math.Max(n, math.Ceil(cover))
	}
	return max(int64(n), shareIssueMin)
}

// enactShareIssue carries out a passed share vote: the shares go into the
// treasury and the case is marked acted upon.
func (m *Mod) enactShareIssue(ctx context.Context, c *corpCEO, caseID int64) {
	_, param, _, _, ok := evedb.WinningOption(ctx, m.db, caseID)
	if !ok || !param.Valid || param.Int64 <= 0 {
		return
	}
	is, err := evedb.IssueCorpShares(ctx, m.db, c.CorpID, param.Int64)
	if err != nil {
		rlogf("rolebots: enact share issue case %d: %v", caseID, err)
		return
	}
	if err := evedb.MarkSanctionedActionDone(ctx, m.db, caseID, evedb.UnixToWin32(clock.Unix())); err != nil {
		rlogf("rolebots: mark share issue case %d: %v", caseID, err)
	}
	announceShares(c.CorpID, []evedb.ShareDelta{{HolderID: c.CorpID, Old: is.OldTreasury, New: is.NewTreasury}},
		is.OldTotal, is.NewTotal)
	m.ceoLog(ctx, c, powerShares, "shareissue", caseID, float64(param.Int64),
		"issued %d new shares into the treasury (now %d issued)", param.Int64, is.NewTotal)
}

// ── the one-off conversion ───────────────────────────────────────────────────

// migrateEquityToShares converts donations made before shares existed into stock at
// par, once: the founder's stake first, then every donor's, issuing any shortfall.
// It must run at boot before any pilot trades: afterwards rolebot_corp_equity also
// holds the cost of bought shares and a donation cannot be told from a purchase.
func (m *Mod) migrateEquityToShares(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingSharesMigrated); err != nil || (ok && truthySetting(v)) {
		return
	}
	par := m.sharePar(ctx)

	// Founders of managed corporations first, so their stake comes out of
	// the treasury before the donors'.
	if ceos, err := listCEOs(ctx, m.db); err == nil {
		for _, c := range ceos {
			if st := m.loadShareState(ctx, c.CorpID); !st.FounderGranted {
				if err := m.grantFounderStake(ctx, c, ceoCharterOf(ctx, m, c)); err != nil {
					rlogf("rolebots: share migration founder corp %d: %v", c.CorpID, err)
				}
			}
		}
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT e.corporationID, e.characterID, e.contributed
		  FROM rolebot_corp_equity e
		  JOIN corporation co ON co.corporationID = e.corporationID
		 WHERE e.contributed > 0
		 ORDER BY e.corporationID, e.contributed DESC`)
	if err != nil {
		rlogf("rolebots: share migration: %v", err)
		return
	}
	type stake struct {
		corpID, charID int64
		in             float64
	}
	var stakes []stake
	for rows.Next() {
		var s stake
		if rows.Scan(&s.corpID, &s.charID, &s.in) == nil {
			stakes = append(stakes, s)
		}
	}
	rows.Close()
	converted := 0
	for _, s := range stakes {
		if evedb.ShareOwnerKind(ctx, m.db, s.charID) == "" {
			continue // the donor is gone
		}
		qty := int64(math.Floor(s.in / par))
		if qty <= 0 {
			continue
		}
		// A stake that fails is logged and skipped rather than retried: a
		// retry cannot tell which stakes already converted.
		if _, err := m.grantShares(ctx, s.corpID, s.charID, qty, true); err != nil {
			rlogf("rolebots: share migration corp=%d char=%d: %v", s.corpID, s.charID, err)
			continue
		}
		converted++
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingSharesMigrated, "1"); err == nil {
		rlogf("rolebots: converted %d donation stake(s) into shares at %s each", converted, iskString(par))
	}
}

func init() { registerCorpForget("shareQuoteCache", forgetShareQuote) }
