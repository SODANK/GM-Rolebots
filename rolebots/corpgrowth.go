package rolebots

import "context"

// Growth before payouts. While free capital is under the tower line
// (towerStartISK), payroll and dividends are cut by rolebots.corp.growthSharePct.
// Nothing is committed or frozen: what is held back stays free capital. Payroll
// and dividends are paid a day apart (payoutSpacingSec), since their windows
// overlap. Why: docs/CORPS.md, "Growth before payouts".

// payoutSpacingSec is the least time between a payroll and a dividend.
const payoutSpacingSec = 24 * 3600

// growthMul is the share of a payout paid while free capital is under target.
// Pure.
func growthMul(free, target, share float64) float64 {
	if free >= target || share <= 0 {
		return 1
	}
	return max(1-share, 0)
}

// growthPayoutMul is growthMul against the tower line and the dial.
func growthPayoutMul(free float64) float64 {
	return growthMul(free, towerStartISK, growthShare())
}

// lastLedgerAt is when this corporation last booked a line, 0 for never.
func (m *Mod) lastLedgerAt(ctx context.Context, corpID int64, line string) int64 {
	var at int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(at), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND line = ?`, corpID, line).Scan(&at)
	return at
}
