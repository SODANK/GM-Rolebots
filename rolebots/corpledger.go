package rolebots

import (
	"context"
	"math"
	"sort"
)

// The corporation's books: what came into a managed corporation's wallet and what went out, by line of
// business. The wallet journal (market_journal) is per transfer and speaks the client's refTypes, so it cannot
// say whether an ISK was earned by the buyback or spent on a skillbook. Every flow the module moves on a
// managed corporation's behalf books one signed row here (+ income, - cost) under a line name, and the CEO page
// reads the P&L off it. Only managed corporations are booked (corpBook checks the in-memory CEO index).
// Not everything reaches it: bounty tax on a player's kill and corp tax on any member's mission reward
// (db.PayMissionISK) are split by the core, and a bill a
// human director pays at the wallet is not booked. Office rent is booked when the CEO rents and when the
// treasury pays each monthly bill (ceoPayRent). The wallet balance is the truth; the ledger explains as much
// of it as the module caused.

const (
	lineTax  = "tax"  // bounty tax on a member's kill
	lineLevy = "levy" // harvest levy on a bot member's ore sales (corplevy.go)
	// lineTradeLevy ("tradelevy") is the trade levy on a bot member's
	// realised trading profit (corptradelevy.go, where it is declared).
	lineDonation = "donation" // retired: a member's gift, before shares (old rows only)
	lineShares   = "shares"   // members buying shares out of the treasury
	lineRedeem   = "redeem"   // the corporation buying shares back
	lineDividend = "dividend" // profit paid out to shareholders
	lineSales    = "sales"    // corp sell orders filled
	lineBooks    = "books"    // skillbooks the corporation paid for
	lineStipend  = "stipend"  // payments to members who ran dry
	lineRent     = "rent"     // offices the CEO rented
	lineWages    = "wages"    // cash paid per delivery: an unmanaged chair's work board, and courier contracts
	linePayroll  = "payroll"  // the weekly merit cheque a bot-run corporation pays its members (corppayroll.go)
	lineSalary   = "salary"   // the Industry Director's weekly salary
	lineBuyback  = "buyback"  // corp buy orders filled (ore, minerals)
	lineBPO      = "bpo"      // blueprints bought for the library
	lineSRP      = "srp"      // hulls replaced for members (valued, in kind)
	lineHulls    = "hulls"    // hulls put in members' hands for fleet seats (gangsupply.go)
	lineStore    = "store"    // company store sales to members
	lineJobFees  = "jobfees"  // installation fees on corporation jobs
	lineCapital  = "capital"  // the CEO's own ISK put in with no shares for it
	lineGankLoot = "gankloot" // what a gank fleet's scout brought home off the wrecks (gank_op.go)
	lineBounties = "bounties" // prices the corporation put on its killers' heads (pvplaw.go)
	// lineCapex is a starbase's structures bought through requisitions (posreq.go
	// reqLedgerLine): an asset the corporation keeps, not a running cost, so it is a
	// capital line and leaves earnings, payroll, dividends and the share price alone.
	lineCapex = "capex"
	// lineTradeBuy is the trade desk's purchases for resale (posreq.go
	// reqLedgerLine), apart from procure so the production governor sees them.
	lineTradeBuy = "tradebuy"
	// The alliance lines (alliance.go, alliancetithe.go): dues are the corporation's weekly maintenance bill to
	// CONCORD; a tithe is its share of what the executor fronted (negative on the member's books, positive on the
	// executor's); allwar is the fee and bills of a war the executor declared for the alliance.
	lineAllianceDues = "dues"
	lineTithe        = "tithe"
	lineAllianceWar  = "allwar"

	// corpLedgerRetentionSec is how long a row is kept. Share earnings read 28 days and dividends read since the
	// last payout, so it keeps about twice the longest window as margin for a corporation that skipped a period.
	corpLedgerRetentionSec = 60 * 86400
	corpLedgerPruneBatch   = 5000
)

// corpLineLabel is how the CEO page names each line.
var corpLineLabel = map[string]string{
	lineTax:       "Bounty tax",
	lineLevy:      "Harvest levy",
	lineTradeLevy: "Trade levy",
	lineDonation:  "Member donations",
	lineShares:    "Share sales",
	lineRedeem:    "Share buybacks",
	lineDividend:  "Dividends",
	lineSales:     "Corp sales",
	lineBooks:     "Skillbooks",
	lineStipend:   "Stipends",
	lineRent:      "Office rent",
	lineWages:     "Work-board wages",
	linePayroll:   "Weekly payroll",
	lineSalary:    "Officer salaries",
	lineBuyback:   "Buyback purchases",
	lineBPO:       "Blueprints bought",
	lineSRP:       "Ships replaced",
	lineHulls:     "Hulls for fleet seats",
	lineStore:     "Company store",
	lineJobFees:   "Job fees",
	lineCapital:   "CEO capital",
	lineBounties:  "Bounties filed",
	lineCapex:     "Starbase structures",
	lineTradeBuy:  "Trade desk purchases",
	// lineConsortium ("consort", corpconsortium.go): components a yard
	// bought from an allied member, and hull-print pledges repaid.
	lineConsortium: "Consortium",

	lineResearchSales: "Researched prints sold",

	lineAllianceDues: "Alliance dues",
	lineTithe:        "Alliance tithes",
	lineAllianceWar:  "Alliance wars",
}

// capitalLines are the books' capital flows: money the owners put in or took out. They move the wallet but are
// not earnings, so the share desk reads earnings with them left out.
var capitalLines = []string{lineDonation, lineShares, lineRedeem, lineDividend, lineCapital, lineCapex}

// corpBook records one flow on the books of a corporation whose desks run (a bot CEO's or a human
// chair's). A no-op for any other. Best effort: the money has already moved
// by the time this is called, and a lost ledger row costs an explanation, not
// ISK.
func (m *Mod) corpBook(ctx context.Context, corpID int64, line string, amount float64, counterID int64, detail string) {
	if corpID == 0 || amount == 0 {
		return
	}
	if !corpDeskRun(corpID) {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_ledger (corporationID, at, line, amount, counterID, detail)
		VALUES (?, ?, ?, ?, ?, ?)`,
		corpID, clock.Unix(), line, amount, counterID, truncate(detail, 255)); err != nil {
		rlogf("rolebots: corp ledger corp %d: %v", corpID, err)
	}
}

// corpLine is one line of a P&L: what it brought in and paid out over the
// window. Net is In − Out.
type corpLine struct {
	Line  string
	Label string
	In    float64
	Out   float64
	Net   float64
}

// corpPnL sums a corporation's books since a time, one row per line, biggest
// absolute net first.
func (m *Mod) corpPnL(ctx context.Context, corpID, since int64) ([]corpLine, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT line,
		       COALESCE(SUM(CASE WHEN amount > 0 THEN amount ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN amount < 0 THEN -amount ELSE 0 END), 0)
		  FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND at >= ?
		 GROUP BY line`, corpID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []corpLine
	for rows.Next() {
		var l corpLine
		if err := rows.Scan(&l.Line, &l.In, &l.Out); err != nil {
			return nil, err
		}
		l.Net = l.In - l.Out
		l.Label = corpLineLabel[l.Line]
		if l.Label == "" {
			l.Label = l.Line
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool { return math.Abs(out[i].Net) > math.Abs(out[j].Net) })
	return out, rows.Err()
}

// pruneCorpLedger drops rows past retention with an indexed range delete in
// batches, the same shape as pruneCEOActions.
func (m *Mod) pruneCorpLedger(ctx context.Context) {
	cutoff := clock.Unix() - corpLedgerRetentionSec
	// The merit trail (corpmerit.go) and closed assignments (corpassign.go)
	// keep the ledger's season: they explain the same money.
	for _, q := range []string{
		`DELETE FROM rolebot_corp_merit_log WHERE at < ? LIMIT ?`,
		`DELETE FROM rolebot_corp_assignments WHERE status NOT IN ('offered', 'posted', 'accepted') AND closedAt < ? LIMIT ?`,
	} {
		for i := 0; i < 20 && ctx.Err() == nil; i++ {
			if i > 0 && !prunePause(ctx) {
				break
			}
			res, err := m.db.ExecContext(ctx, q, cutoff, corpLedgerPruneBatch)
			if err != nil {
				break
			}
			if n, _ := res.RowsAffected(); n < corpLedgerPruneBatch {
				break
			}
		}
	}
	for i := 0; i < 20 && ctx.Err() == nil; i++ {
		if i > 0 && !prunePause(ctx) {
			break
		}
		res, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_corp_ledger WHERE at < ? LIMIT ?`, cutoff, corpLedgerPruneBatch)
		if err != nil {
			rlogf("rolebots: prune corp ledger: %v", err)
			return
		}
		if n, _ := res.RowsAffected(); n < corpLedgerPruneBatch {
			return
		}
	}
}
