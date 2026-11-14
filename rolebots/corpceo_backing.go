package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// Backing: a CEO putting its own ISK into a corporation that has run short.
// Every other treasury decision stops at the reserve, so a corporation below it
// does nothing. When it cannot cover its reserve plus what it has promised
// (corpCommitted), the CEO tops it up to working capital (reserve, promises and
// a week of actual spend) from its own spare ISK, within Charter.BackingFrac a
// week. It buys treasury shares at the ask where there are any (no control floor
// or per-holder cap); the rest goes in as plain capital (a Player Donation, the
// capital ledger line, a contribution on the CEO's equity row). Both are capital
// lines, so a bail-out never reads as earnings. The money lands in the one
// corporation wallet (the 3913 client has no wallet divisions).

const (
	// ceoBackEverySec is how often a CEO will put money in at all.
	ceoBackEverySec = 86400
	// ceoBackWeekSec is the window BackingFrac is counted over.
	ceoBackWeekSec = 7 * 86400
	// ceoBackMinISK: below this the gap is not worth a transfer.
	ceoBackMinISK = 250_000.0
	// ceoBackKeepISK is what a CEO keeps for itself however short its corporation
	// is: the same line a member clears in a founding round (investFoundingAboveISK).
	ceoBackKeepISK = investFoundingAboveISK
	// corpSeedISK is the least working capital a corporation counts as not short: a
	// first office, a first buyback, a week of stipends. Reserve plus promises alone
	// is rent-sized, which would leave a new outfit never backed.
	corpSeedISK = 2_000_000.0
)

// corpShortLine is the balance under which a corporation is short: its
// reserve and open promises, and never less than seed capital. The CEO backs
// it below this, and members fund it as a founding round (investFor).
func (m *Mod) corpShortLine(ctx context.Context, corpID int64, ch Charter) float64 {
	return math.Max(m.corpReserve(ctx, corpID, ch)+m.corpCommitted(ctx, corpID), corpSeedISK)
}

// ceoBackingFor is what a CEO puts into its corporation this think. The
// corporation is short below `short` and is filled toward `need`; spare is
// what the CEO holds over its own keep. Pure, so the harness can sweep it.
func ceoBackingFor(corpBal, short, need, ceoBal, frac, backedThisWeek float64) float64 {
	if corpBal >= short || need <= corpBal || frac <= 0 {
		return 0
	}
	spare := ceoBal - ceoBackKeepISK
	if spare <= 0 {
		return 0
	}
	amount := math.Min(need-corpBal, spare*frac-backedThisWeek)
	if amount < ceoBackMinISK {
		return 0
	}
	return math.Floor(amount)
}

// corpWeeklyOutflow is what the corporation's operations spent over the last
// week by its own books: every cost line, the capital lines left out (a share
// buyback or a dividend is not the cost of running the outfit).
func (m *Mod) corpWeeklyOutflow(ctx context.Context, corpID int64) float64 {
	args := []any{corpID, clock.Unix() - ceoBackWeekSec}
	for _, l := range capitalLines {
		args = append(args, l)
	}
	var out float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(-SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND at >= ? AND amount < 0
		   AND line NOT IN (`+placeholders(len(capitalLines))+`)`, args...).Scan(&out)
	return math.Max(0, out)
}

// ceoBackCorp is the CEO topping up a corporation that has run short.
func (m *Mod) ceoBackCorp(ctx context.Context, c *corpCEO, ch Charter) error {
	now := clock.Unix()
	if now-m.lastCEOActionAt(ctx, c.CorpID, "backing") < ceoBackEverySec {
		return nil
	}
	corpBal, err := evedb.GetCorpBalance(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	reserve := m.corpReserve(ctx, c.CorpID, ch)
	short := m.corpShortLine(ctx, c.CorpID, ch)
	if corpBal >= short {
		return nil
	}
	ceoBal, err := evedb.GetCharBalance(ctx, m.db, c.CharID)
	if err != nil {
		return err
	}
	// Working capital: at least a month of rent over the short line, so a
	// corporation that has spent nothing yet still gets enough to start.
	need := short + math.Max(m.corpWeeklyOutflow(ctx, c.CorpID), m.corpMonthlyRent(ctx, c.CorpID))
	amount := ceoBackingFor(corpBal, short, need, ceoBal, ch.BackingFrac,
		m.ceoSpentSince(ctx, c.CorpID, "backing", now-ceoBackWeekSec))
	if amount <= 0 {
		return nil
	}

	qty, cost, err := m.ceoBuyStock(ctx, c, ch, amount)
	if err != nil && !errors.Is(err, evedb.ErrInsufficientFunds) {
		return err
	}
	rest := math.Floor(amount - cost)
	var given float64
	if rest >= ceoBackMinISK {
		if _, _, err := evedb.TransferISK(ctx, m.db, c.CharID, c.CorpID, rest, refPlayerDonation,
			"CEO capital injection"); err != nil {
			if !errors.Is(err, evedb.ErrInsufficientFunds) {
				return err
			}
		} else {
			given = rest
			m.creditEquity(ctx, c.CorpID, c.CharID, given, 0)
			m.corpBook(ctx, c.CorpID, lineCapital, given, c.CharID, "the CEO's own capital")
		}
	}
	total := cost + given
	if total <= 0 {
		return nil
	}
	m.pushWallet(ctx, c.CharID)

	what := iskString(given) + " as capital"
	switch {
	case qty > 0 && given > 0:
		what = fmt.Sprintf("%d shares for %s and %s as capital", qty, iskString(cost), iskString(given))
	case qty > 0:
		what = fmt.Sprintf("%d shares for %s", qty, iskString(cost))
	}
	m.ceoLog(ctx, c, powerTreasury, "backing", c.CharID, total,
		"put its own money in — %s — with the wallet at %s against a reserve of %s",
		what, iskString(corpBal), iskString(reserve))
	m.logBot(c.CharID, logInfo, "ceo", "backed its corporation with %s of its own ISK (%s)", iskString(total), what)
	return nil
}

// ceoBuyStock buys treasury shares for the CEO with up to budget ISK.
// Returns the shares and what they cost; 0, 0 with shares switched off or
// nothing in the treasury.
func (m *Mod) ceoBuyStock(ctx context.Context, c *corpCEO, ch Charter, budget float64) (int64, float64, error) {
	if !sharesEnabled() {
		return 0, 0, nil
	}
	q, err := m.corpShareQuote(ctx, c.CorpID, ch)
	if err != nil {
		return 0, 0, err
	}
	// The whole treasury is for sale to the CEO: the floor keeps control
	// with the CEO, and the CEO holding the shares keeps it just the same.
	qty, _ := shareSaleSize(budget, q.Ask, q.Treasury, math.MaxInt64)
	if qty <= 0 {
		return 0, 0, nil
	}
	cost := float64(qty) * q.Ask
	name := corpNameOf(ctx, m.db, c.CorpID)
	mv, err := evedb.TradeShares(ctx, m.db, c.CorpID, c.CorpID, c.CharID, qty, cost, refShares,
		fmt.Sprintf("%d shares of %s", qty, name))
	if err != nil {
		return 0, 0, err
	}
	announceShares(c.CorpID, mv.Deltas(c.CorpID, c.CharID), 0, 0)
	forgetShareQuote(c.CorpID)
	m.creditEquity(ctx, c.CorpID, c.CharID, cost, 0)
	m.corpBook(ctx, c.CorpID, lineShares, cost, c.CharID, fmt.Sprintf("%d shares to its CEO", qty))
	return qty, cost, nil
}
