package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Dividends: how a managed corporation's profit reaches its owners
// (corpshares.go). On its review the CEO pays out a share of what the corporation
// earned since its last dividend: the books (corpledger.go) without the capital
// lines, so money the owners put in is never handed back as profit. The share and
// cadence are the charter's (PayoutRatio, DividendEverySec) unless the operator
// set an override on the CEO page. The payout is capped at half the free capital,
// never out of the reserve or promises. It is the client's own dividend
// (evedb.PayDividend, the corp Home tab's Dividends button): pro rata over every
// holder but the treasury, one debit and credit per payee committed together,
// journalled as refType 38.

const (
	// dividendMinISK: a pool smaller than this is carried to the next
	// review rather than split into dust.
	dividendMinISK = 250_000.0
	// dividendFreeCap is the most of its free capital one dividend takes.
	dividendFreeCap = 0.5
)

// dividendPolicy is the payout share and cadence in force for a corporation:
// the operator's override where set, else the charter's.
type dividendPolicy struct {
	Ratio      float64
	EverySec   int64
	RatioSet   bool // the operator overrode the ratio
	EverySet   bool // the operator overrode the cadence
	LastPaidAt int64
}

func (m *Mod) loadDividendPolicy(ctx context.Context, corpID int64, ch Charter) dividendPolicy {
	p := dividendPolicy{Ratio: ch.PayoutRatio, EverySec: ch.DividendEverySec}
	var pct, days float64
	if err := m.db.QueryRowContext(ctx, `
		SELECT payoutPct, dividendDays, lastDividendAt FROM rolebot_share_state WHERE corporationID = ?`,
		corpID).Scan(&pct, &days, &p.LastPaidAt); err != nil {
		return p
	}
	if pct >= 0 {
		p.Ratio, p.RatioSet = math.Min(pct, 100)/100, true
	}
	if days > 0 {
		p.EverySec, p.EverySet = int64(days*86400), true
	}
	return p
}

// setDividendPolicy stores the operator's override. pct < 0 hands the ratio
// back to the charter, days <= 0 the cadence.
func (m *Mod) setDividendPolicy(ctx context.Context, corpID int64, pct, days float64) error {
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = -1
	}
	if days < 0 {
		days = 0
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_share_state (corporationID, weekStart, payoutPct, dividendDays) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE payoutPct = VALUES(payoutPct), dividendDays = VALUES(dividendDays)`,
		corpID, clock.Unix(), pct, days)
	return err
}

// earnedSince is the corporation's earnings since a time: the books with the
// capital lines (share sales, buybacks, dividends, old donations) left out.
func (m *Mod) earnedSince(ctx context.Context, corpID, since int64) float64 {
	args := []any{corpID, since}
	for _, l := range capitalLines {
		args = append(args, l)
	}
	var net float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND at > ? AND line NOT IN (`+placeholders(len(capitalLines))+`)`,
		args...).Scan(&net)
	return net
}

// dividendPool is what a dividend pays: the ratio of what was earned, capped
// at the share of free capital a dividend may take. Pure.
func dividendPool(earned, ratio, free float64) float64 {
	if earned <= 0 || ratio <= 0 || free <= 0 {
		return 0
	}
	return math.Floor(math.Min(earned*ratio, free*dividendFreeCap))
}

// dividendDue is whether this review pays: the cadence has run, or the
// operator asked for it now.
func (m *Mod) dividendDue(p dividendPolicy, force bool) bool {
	return force || clock.Unix()-p.LastPaidAt >= p.EverySec
}

// payDividend is the CEO's dividend. force (the panel's Pay now) skips the
// cadence and the minimum, not the cap. Returns what was paid.
func (m *Mod) payDividend(ctx context.Context, c *corpCEO, ch Charter, force bool) (float64, error) {
	corpID := c.CorpID
	pol := m.loadDividendPolicy(ctx, corpID, ch)
	if !m.dividendDue(pol, force) {
		return 0, nil
	}
	// A payroll paid in the last day leaves the dividend for tomorrow
	// (corpgrowth.go); the operator's Pay now does not wait.
	if !force && clock.Unix()-m.lastLedgerAt(ctx, corpID, linePayroll) < payoutSpacingSec {
		return 0, nil
	}
	since := pol.LastPaidAt
	if since == 0 {
		// Never paid: count one period back, not the corporation's life.
		since = clock.Unix() - pol.EverySec
	}
	earned := m.earnedSince(ctx, corpID, since)
	free, _, err := m.corpFreeCapital(ctx, corpID, ch)
	if err != nil {
		return 0, err
	}
	// Under the tower line part of the payout is kept to grow on (corpgrowth.go).
	ratio := pol.Ratio * growthPayoutMul(free)
	pool := dividendPool(earned, ratio, free)
	if pool < 1 || (!force && pool < dividendMinISK) {
		// Carried: the earnings stay counted until the next review.
		return 0, nil
	}
	holders, err := evedb.BeneficialShareHolders(ctx, m.db, corpID)
	if err != nil {
		return 0, err
	}
	payouts := evedb.SplitDividendByShares(pool, corpID, holders)
	if len(payouts) == 0 {
		return 0, nil // nobody outside the treasury holds any stock
	}
	name := corpNameOf(ctx, m.db, corpID)
	total, paid, _, err := evedb.PayDividend(ctx, m.db, corpID, payouts, "Dividend: "+name)
	if errors.Is(err, evedb.ErrInsufficientFunds) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	now := clock.Unix()
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_share_state (corporationID, weekStart, lastDividendAt) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE lastDividendAt = VALUES(lastDividendAt)`, corpID, now, now)
	m.corpBook(ctx, corpID, lineDividend, -total, 0,
		fmt.Sprintf("%.0f%% of %s earned, to %d holder(s)", ratio*100, iskString(earned), len(paid)))

	var outstanding int64
	for _, p := range payouts {
		for _, h := range holders {
			if h.HolderID == p.PayeeID {
				outstanding += h.Shares
			}
		}
	}
	perShare := 0.0
	if outstanding > 0 {
		perShare = total / float64(outstanding)
	}
	hooks := modapi.Corp()
	for _, p := range paid {
		m.creditEquity(ctx, corpID, p.PayeeID, 0, p.Amount)
		if !p.IsChar {
			continue
		}
		if hooks.AccountChanged != nil {
			hooks.AccountChanged(p.PayeeID, p.Balance)
		}
		// A person is told; a bot's mailbox is suppressed anyway.
		if !m.isRostered(ctx, p.PayeeID) {
			m.mailShareReceipt(ctx, c, p.PayeeID, "Dividend: "+name, fmt.Sprintf(
				"<b>%s</b> has paid a dividend of %s a share. Your holding earned <b>%s</b>.",
				name, iskString(perShare), iskString(p.Amount)))
		}
	}
	m.ceoLog(ctx, c, powerShares, "dividend", 0, total,
		"paid a dividend of %s (%s a share) — %.0f%% of %s earned", iskString(total), iskString(perShare),
		pol.Ratio*100, iskString(earned))
	forgetShareQuote(corpID)
	return total, nil
}
