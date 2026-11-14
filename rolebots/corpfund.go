package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// The print fund: how a shipyard saves toward a hull print it cannot buy in one think. The library buys a
// print only when print x poAdvanceMarkup fits one think's production budget (CapitalRisk x free capital),
// which a battlecruiser or battleship original exceeds.
// A fund is a commit (rolebot_corp_commit, refKind "fund") that grows: each production think moves
// rolebots.printFund.share of the governor's budget into it, shielding it from other desks without ISK
// moving. When it covers the print it posts a fund-backed purchase order and the deputy buys as usual.
// The target is the cheapest print of the lowest ladder rung the library lacks (hulltags.go). Kind "print" is
// the hull original; kind "bill" is one run's materials and wage for a big hull, spent when the order posts.
// A fund nobody fed for printFund.abandonDays, or whose corporation ran short (corpShortLine), is released. A
// bought print is remembered (status "bought") so the library plan researches and copies it first (corplibplan.go).

const (
	fundKindPrint = "print"
	fundKindBill  = "bill"

	fundStatusSaving  = "saving"
	fundStatusOrdered = "ordered"
	fundStatusBought  = "bought"
	fundStatusSpent   = "spent"
	fundStatusClosed  = "closed"

	// fundCommitKind is the commit refKind; refID is the fund row's fundID.
	fundCommitKind = "fund"

	printFundEnabledKey  = "rolebots.printFund.enabled"
	printFundShareKey    = "rolebots.printFund.share"
	printFundAbandonKey  = "rolebots.printFund.abandonDays"
	printFundSeedRunsKey = "rolebots.printFund.seedRuns"
	printFundKeepKey     = "rolebots.printFund.keepCopies"

	printFundShareDefault   = 0.5
	printFundAbandonDefault = 60
	printFundSeedDefault    = 6
	printFundKeepDefault    = 2

	// fundLogEverySec spaces the "saving toward" line.
	fundLogEverySec = 6 * 3600
)

// corpFund is one saving target.
type corpFund struct {
	FundID, CorpID, BPTypeID int64
	Kind                     string
	Target, Saved            float64
	OpenedAt, LastAddAt      int64
	Reason, Status           string
	POID                     int64
}

func printFundOn(ctx context.Context, db *sql.DB) bool {
	return escrowSettingInt(ctx, db, printFundEnabledKey, 1) != 0
}

func printFundShare(ctx context.Context, db *sql.DB) float64 {
	return min(max(escrowSettingFloat(ctx, db, printFundShareKey, printFundShareDefault), 0), 1)
}

func printFundSeedRuns(ctx context.Context, db *sql.DB) int64 {
	return int64(max(escrowSettingInt(ctx, db, printFundSeedRunsKey, printFundSeedDefault), 0))
}

func printFundKeepCopies(ctx context.Context, db *sql.DB) int {
	return max(escrowSettingInt(ctx, db, printFundKeepKey, printFundKeepDefault), 0)
}

// fundAddFor is what one think puts into a fund: the share of the budget, never
// past what the target still needs, never negative. Pure.
func fundAddFor(budget, share, target, saved float64) float64 {
	if budget <= 0 || share <= 0 || target <= saved {
		return 0
	}
	return math.Floor(math.Min(budget*share, target-saved))
}

// fundReady reports whether a fund covers its target. Pure.
func fundReady(saved, target float64) bool { return target > 0 && saved >= target }

// fundWeeksLeft estimates the weeks to the target at a weekly feed: the
// corporation's weekly inflow × CapitalRisk × share. 0 when nothing feeds it.
// Pure.
func fundWeeksLeft(target, saved, weeklyInflow, capitalRisk, share float64) float64 {
	feed := weeklyInflow * capitalRisk * share
	if feed <= 0 || target <= saved {
		return 0
	}
	return (target - saved) / feed
}

// openFundFor is the corporation's open fund of a kind (saving or ordered),
// nil when none.
func (m *Mod) openFundFor(ctx context.Context, corpID int64, kind string) *corpFund {
	f := &corpFund{}
	err := m.db.QueryRowContext(ctx, `
		SELECT fundID, corporationID, bpTypeID, kind, target, saved, openedAt, lastAddAt, reason, status, poID
		  FROM rolebot_corp_fund
		 WHERE corporationID = ? AND kind = ? AND status IN (?, ?)
		 ORDER BY openedAt LIMIT 1`, corpID, kind, fundStatusSaving, fundStatusOrdered).
		Scan(&f.FundID, &f.CorpID, &f.BPTypeID, &f.Kind, &f.Target, &f.Saved, &f.OpenedAt, &f.LastAddAt,
			&f.Reason, &f.Status, &f.POID)
	if err != nil {
		return nil
	}
	return f
}

// openFund opens a fund toward a print (or a bill), or re-targets the open
// one of that kind to a cheaper or different print, keeping what it has saved.
func (m *Mod) openFund(ctx context.Context, c *corpCEO, kind string, bpTypeID int64, target float64, reason string) {
	if target <= 0 || bpTypeID == 0 {
		return
	}
	now := clock.Unix()
	if f := m.openFundFor(ctx, c.CorpID, kind); f != nil {
		if f.Status == fundStatusOrdered {
			return // the order is out; nothing to re-target
		}
		if f.BPTypeID == bpTypeID && math.Abs(f.Target-target) < 1 {
			return
		}
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_fund SET bpTypeID = ?, target = ?, reason = ? WHERE fundID = ?`,
			bpTypeID, target, reason, f.FundID)
		m.ceoLog(ctx, c, powerProduction, "fund", bpTypeID, target,
			"re-aimed its %s fund at a %s (%s; %s saved so far)", kind, m.typeNameOr(ctx, bpTypeID),
			iskString(target), iskString(f.Saved))
		return
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_fund (corporationID, bpTypeID, kind, target, saved, openedAt, lastAddAt, reason, status)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`, c.CorpID, bpTypeID, kind, target, now, now, reason, fundStatusSaving)
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "opening a %s fund: %v", kind, err)
		return
	}
	id, _ := res.LastInsertId()
	_ = m.openCommit(ctx, c.CorpID, fundCommitKind, id, 0)
	m.ceoLog(ctx, c, powerProduction, "fund", bpTypeID, target,
		"opened a %s fund toward a %s — %s needed; %s", kind, m.typeNameOr(ctx, bpTypeID), iskString(target), reason)
}

// closeFund releases a fund's commit and marks it.
func (m *Mod) closeFund(ctx context.Context, f *corpFund, status string) {
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_fund SET status = ? WHERE fundID = ?`, status, f.FundID)
	m.closeCommit(ctx, fundCommitKind, f.FundID)
}

// feedFunds is the production think's saving step: the open print fund
// first, then the open bill fund, each fed its share of what is left of the
// budget. Answers what it took. A fund that reaches its target triggers.
func (m *Mod) feedFunds(ctx context.Context, c *corpCEO, ch Charter, budget float64) float64 {
	if !printFundOn(ctx, m.db) {
		return 0
	}
	m.sweepFunds(ctx, c, ch)
	share := printFundShare(ctx, m.db)
	taken := 0.0
	for _, kind := range fundKinds {
		f := m.openFundFor(ctx, c.CorpID, kind)
		if f == nil || f.Status != fundStatusSaving {
			continue
		}
		add := fundAddFor(budget-taken, share, f.Target, f.Saved)
		if add > 0 {
			f.Saved += add
			now := clock.Unix()
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_fund SET saved = ?, lastAddAt = ? WHERE fundID = ?`,
				f.Saved, now, f.FundID)
			_ = m.openCommit(ctx, c.CorpID, fundCommitKind, f.FundID, f.Saved)
			taken += add
		}
		if fundReady(f.Saved, f.Target) {
			if kind != fundKindBill {
				m.triggerPrintFund(ctx, c, ch, f)
			}
			// A ready bill fund waits for the board (postBuildOrders spends it).
			continue
		}
		if add > 0 && clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "fund"+kind) >= fundLogEverySec {
			weeks := fundWeeksLeft(f.Target, f.Saved, m.corpWeeklyInflow(ctx, c.CorpID), ch.CapitalRisk, share)
			m.ceoLog(ctx, c, powerProduction, "fund"+kind, f.BPTypeID, f.Saved,
				"saving toward a %s %s — %s of %s put by, about %.0f week(s) to go", m.typeNameOr(ctx, f.BPTypeID),
				fundKindNoun[kind], iskString(f.Saved), iskString(f.Target), math.Ceil(weeks))
		}
	}
	return taken
}

// fundKinds is the order funds are fed in: the shipyard's ladder print, the
// capital program's prints (corpprogram.go), then a hull run's bill.
var fundKinds = []string{fundKindPrint, fundKindProgram, fundKindBill}

// fundKindNoun words a fund's kind in a log line.
var fundKindNoun = map[string]string{fundKindPrint: "print", fundKindProgram: "program print", fundKindBill: "run's bill"}

// corpWeeklyInflow is what came into the corporation's books over the last
// week, capital lines excluded: what a fund can expect to be fed from.
func (m *Mod) corpWeeklyInflow(ctx context.Context, corpID int64) float64 {
	args := []any{corpID, clock.Unix() - 7*86400}
	for _, l := range capitalLines {
		args = append(args, l)
	}
	var in float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND at >= ? AND amount > 0
		   AND line NOT IN (`+placeholders(len(capitalLines))+`)`, args...).Scan(&in)
	return math.Max(0, in)
}

// sweepFunds releases funds nobody has fed for abandonDays, or whose
// corporation has run short of its reserve and promises.
func (m *Mod) sweepFunds(ctx context.Context, c *corpCEO, ch Charter) {
	abandon := int64(max(escrowSettingInt(ctx, m.db, printFundAbandonKey, printFundAbandonDefault), 1)) * 86400
	now := clock.Unix()
	bal, err := evedb.GetCorpBalance(ctx, m.db, c.CorpID)
	short := err == nil && bal < m.corpShortLine(ctx, c.CorpID, ch)
	for _, kind := range fundKinds {
		f := m.openFundFor(ctx, c.CorpID, kind)
		if f == nil || f.Status != fundStatusSaving {
			continue
		}
		why := ""
		switch {
		case short && f.Saved > 0:
			why = "the corporation has run short"
		case now-f.LastAddAt > abandon:
			why = fmt.Sprintf("nothing has been put by for %d days", (now-f.LastAddAt)/86400)
		}
		if why == "" {
			continue
		}
		m.closeFund(ctx, f, fundStatusClosed)
		m.ceoLog(ctx, c, powerProduction, "fundclose", f.BPTypeID, f.Saved,
			"closed its %s fund toward a %s — %s", kind, m.typeNameOr(ctx, f.BPTypeID), why)
	}
}

// triggerPrintFund turns a full print fund into a purchase order. The print
// is re-quoted first: an escrow copy or original may have come up cheaper
// since the fund opened, and the fund buys that instead.
func (m *Mod) triggerPrintFund(ctx context.Context, c *corpCEO, ch Charter, f *corpFund) {
	var openPOs int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_po WHERE corporationID = ? AND status = 'open'`,
		c.CorpID).Scan(&openPOs)
	if openPOs >= poMaxOpen {
		return
	}
	p, err := m.loadCorpPolicy(ctx, c.CorpID)
	if err != nil || p.DeputyID == 0 {
		return
	}
	stationID, _ := m.corpFactoryOffice(ctx, c.CorpID)
	if stationID == 0 {
		return
	}
	value := evedb.TypeValueOf(ctx, m.db, f.BPTypeID).Value
	if cheapest := m.shipyardPrintPrice(ctx, f.BPTypeID, m.stationRegionID(ctx, stationID)); cheapest > 0 &&
		(value <= 0 || cheapest < value) {
		value = cheapest
	}
	if value <= 0 {
		return
	}
	need := math.Ceil(value * poAdvanceMarkup)
	if need > f.Saved {
		// The print got dearer than the fund: keep saving toward the new price.
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_fund SET target = ? WHERE fundID = ?`, need, f.FundID)
		return
	}
	// The fund's commit becomes the order's: released here, re-opened by
	// postPO under the order, so free capital is never counted twice.
	m.closeCommit(ctx, fundCommitKind, f.FundID)
	poID, err := m.postPOWith(ctx, c, p.DeputyID, f.BPTypeID, stationID, value, true, true)
	if err != nil {
		_ = m.openCommit(ctx, c.CorpID, fundCommitKind, f.FundID, f.Saved)
		if !errors.Is(err, errPOOverCap) {
			m.logBot(c.CharID, logError, "ceo", "fund purchase order: %v", err)
		}
		return
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_fund SET status = ?, poID = ? WHERE fundID = ?`,
		fundStatusOrdered, poID, f.FundID)
	m.ceoLog(ctx, c, powerProduction, "fund", f.BPTypeID, f.Saved,
		"its fund covers a %s — ordered one for the library at up to %s", m.typeNameOr(ctx, f.BPTypeID), iskString(need))
}

// poFundBacked reports whether a purchase order was posted by a fund: its
// reservation was saved for over weeks and does not have to fit one think's
// CapitalRisk × free capital at the counter.
func (m *Mod) poFundBacked(ctx context.Context, poID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_fund WHERE poID = ? AND status = ?`, poID, fundStatusOrdered).Scan(&n)
	return n > 0
}

// fundPODelivered marks the fund whose order delivered its print, and opens
// the bill fund for one run of it when the hull is a big one.
func (m *Mod) fundPODelivered(ctx context.Context, po *corpPO) {
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_fund SET status = ? WHERE poID = ? AND status = ?`,
		fundStatusBought, po.POID, fundStatusOrdered)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	c, _ := getCEO(ctx, m.db, po.CorpID)
	if c == nil {
		return
	}
	ch := poCharterOf(ctx, m, c)
	m.openBillFund(ctx, c, ch, po.BPTypeID)
}

// fundPOVoided puts a fund whose order was cancelled back to saving: the
// advance came back to the wallet, so the saving starts over from what is
// free rather than claiming money the order never spent.
func (m *Mod) fundPOVoided(ctx context.Context, po *corpPO) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_fund SET status = ?, saved = 0, poID = 0, lastAddAt = ? WHERE poID = ? AND status = ?`,
		fundStatusSaving, clock.Unix(), po.POID, fundStatusOrdered)
}

// openBillFund opens a bill fund for one run of a big hull the library holds a
// print of, sized at the board's own bill (planRowAt, materials + wage).
func (m *Mod) openBillFund(ctx context.Context, c *corpCEO, ch Charter, bpTypeID int64) {
	f, err := m.printFactsFor(ctx, bpTypeID)
	if err != nil || f.ProductTypeID == 0 {
		return
	}
	if k := m.typeKinds(ctx, []int64{f.ProductTypeID})[f.ProductTypeID]; !costPlusHullGroups[k.Group] {
		return
	}
	stationID, _ := m.corpFactoryOffice(ctx, c.CorpID)
	row, err := m.planRowAt(ctx, ch, bpTypeID, m.stationRegionID(ctx, stationID))
	if err != nil || row == nil {
		return
	}
	bill := math.Ceil(row.MatsPerRun + row.WagePerRun)
	if bill <= 0 {
		return
	}
	m.openFund(ctx, c, fundKindBill, bpTypeID, bill, "one run's materials and wage, so the board can post the order")
}

// readyBillFunds is the bill funds that cover their target, by bp type, with
// what each holds. postBuildOrders adds it to the budget a row is sized to.
func (m *Mod) readyBillFunds(ctx context.Context, corpID int64) map[int64]*corpFund {
	out := map[int64]*corpFund{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT fundID, corporationID, bpTypeID, kind, target, saved, openedAt, lastAddAt, reason, status, poID
		  FROM rolebot_corp_fund WHERE corporationID = ? AND kind = ? AND status = ? AND saved >= target`,
		corpID, fundKindBill, fundStatusSaving)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		f := &corpFund{}
		if rows.Scan(&f.FundID, &f.CorpID, &f.BPTypeID, &f.Kind, &f.Target, &f.Saved, &f.OpenedAt, &f.LastAddAt,
			&f.Reason, &f.Status, &f.POID) == nil {
			out[f.BPTypeID] = f
		}
	}
	return out
}

// spendBillFund releases a bill fund into the order that spent it.
func (m *Mod) spendBillFund(ctx context.Context, c *corpCEO, f *corpFund) {
	m.closeFund(ctx, f, fundStatusSpent)
	m.ceoLog(ctx, c, powerProduction, "fundspent", f.BPTypeID, f.Saved,
		"spent its bill fund — %s put by — on a run of %s", iskString(f.Saved), m.typeNameOr(ctx, f.BPTypeID))
}

// fundBoughtPrints is the bp types the corporation's funds bought: the
// library plan researches and copies them ahead of any build record, and the
// copy sale keeps copies of them back for the board.
func (m *Mod) fundBoughtPrints(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT bpTypeID FROM rolebot_corp_fund WHERE corporationID = ? AND kind = ? AND status IN (?, ?)`,
		corpID, fundKindPrint, fundStatusBought, fundStatusSpent)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var bp int64
		if rows.Scan(&bp) == nil {
			out[bp] = true
		}
	}
	return out
}

// fundPick is a hull print the purchase scan could not afford this think.
type fundPick struct {
	BPTypeID, ProductTypeID int64
	Group                   int64
	Value                   float64
	Wanted                  int64
}

// chooseFundTarget picks the fund's target from the prints the budget could
// not reach: the cheapest print of the LOWEST hull class the library does not
// yet hold (held = a class with an original or a copy on the shelf). Classes
// below a destroyer are never a fund's business. Pure, for the test.
func chooseFundTarget(over []fundPick, held map[HullClass]bool) *fundPick {
	var best *fundPick
	bestClass := ClassUnknown
	for i := range over {
		p := &over[i]
		cls := hullClassByGroup[p.Group]
		if cls < ClassDestroyer || held[cls] || p.Value <= 0 || p.Wanted <= 0 {
			continue
		}
		if best == nil || cls < bestClass || (cls == bestClass && p.Value < best.Value) {
			best, bestClass = p, cls
		}
	}
	return best
}

// libraryHullClasses is the hull classes the library already holds a print
// (original or copy with runs) of.
func (m *Mod) libraryHullClasses(ctx context.Context, corpID int64) map[HullClass]bool {
	out := map[HullClass]bool{}
	var bps []int64
	if ps, err := m.libraryPrintsAll(ctx, corpID); err == nil {
		for _, p := range ps {
			bps = append(bps, p.BPTypeID)
		}
	}
	if cs, err := m.libraryCopiesAll(ctx, corpID); err == nil {
		for _, c := range cs {
			bps = append(bps, c.BPTypeID)
		}
	}
	var pids []int64
	for _, bp := range bps {
		if f, err := m.printFactsFor(ctx, bp); err == nil && f.ProductTypeID != 0 {
			pids = append(pids, f.ProductTypeID)
		}
	}
	for _, k := range m.typeKinds(ctx, pids) {
		if cls, ok := hullClassByGroup[k.Group]; ok && k.Category == catShip {
			out[cls] = true
		}
	}
	return out
}

// fundTargetFrom is the purchase scan's saving branch: nothing fit the budget,
// so the fund is aimed at the best print the budget could not reach. Reports
// whether a fund is open afterwards.
func (m *Mod) fundTargetFrom(ctx context.Context, c *corpCEO, over []fundPick) bool {
	if !printFundOn(ctx, m.db) || len(over) == 0 {
		return false
	}
	pick := chooseFundTarget(over, m.libraryHullClasses(ctx, c.CorpID))
	if pick == nil {
		return false
	}
	target := math.Ceil(pick.Value * poAdvanceMarkup)
	m.openFund(ctx, c, fundKindPrint, pick.BPTypeID, target,
		fmt.Sprintf("the %s rung the library lacks; %d pilot(s) can fly one",
			hullClassByGroup[pick.Group], pick.Wanted))
	return true
}

// fundSummary is the CEO page's one line on the corporation's open funds:
// each kind's target, what is put by, and about how long to go at the last
// week's inflow. Empty with none open.
func (m *Mod) fundSummary(ctx context.Context, corpID int64) string {
	c, _ := getCEO(ctx, m.db, corpID)
	if c == nil {
		return ""
	}
	ch := poCharterOf(ctx, m, c)
	share := printFundShare(ctx, m.db)
	inflow := m.corpWeeklyInflow(ctx, corpID)
	out := ""
	for _, kind := range fundKinds {
		f := m.openFundFor(ctx, corpID, kind)
		if f == nil {
			continue
		}
		if out != "" {
			out += " · "
		}
		what := m.typeNameOr(ctx, f.BPTypeID)
		switch kind {
		case fundKindBill:
			what = "a run of " + what
		case fundKindProgram:
			what = "program print " + what
		}
		switch {
		case f.Status == fundStatusOrdered:
			out += fmt.Sprintf("%s — on order (%s saved)", what, iskString(f.Saved))
		case fundReady(f.Saved, f.Target):
			out += fmt.Sprintf("%s — %s saved, ready", what, iskString(f.Saved))
		default:
			weeks := fundWeeksLeft(f.Target, f.Saved, inflow, ch.CapitalRisk, share)
			out += fmt.Sprintf("%s — %s of %s saved", what, iskString(f.Saved), iskString(f.Target))
			if weeks > 0 {
				out += fmt.Sprintf(" (about %.0f week(s) to go)", math.Ceil(weeks))
			}
		}
	}
	return out
}
