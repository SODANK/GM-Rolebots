package rolebots

// The weekly payroll: how a bot-run corporation pays its members. Work done for the corporation (a delivered
// job in corpwork.go, a haul in poshaul.go, a moon survey in posintel.go, a carried print, a trade run in
// posreq.go, an assignment in corpassign.go) is written down as labour credit (rolebot_corp_labour) and no ISK
// moves. The credit is what the work would have earned as a wage, and the merit pass reads it as evidence.
// Once a week the corporation splits a share of the week's operating profit (capital flows, salaries and the
// payroll itself left out) between members by the merit they earned that week, each weighed by its rank's pay
// multiplier. The share is Charter.PayrollShare. A week that lost money pays nothing, and the stipend
// (corpceo_treasury.go) stays the safety net. The pot is capped at half the free capital.
// A corporation the module does not run (a human chair posting work, corpcommanddesk.go) has no payroll and
// keeps paying cash on delivery.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	evedb "gurimoon/db"
)

const (
	payrollEverySec   = 7 * 86400
	payrollMinCheque  = 1000 // a cheque under this is not worth a journal line
	payrollCapitalCap = 0.5  // the pot never exceeds this share of free capital
	// meritLabourWeight is merit points per million ISK of labour credit: the weight a million of wages carried
	// when paid in cash.
	meritLabourWeight = 10
)

// payrollManaged reports whether a corporation pays its members by the
// weekly payroll (bot-run) rather than cash per delivery.
func payrollManaged(corpID int64) bool {
	_, ok := managedCEO(corpID)
	return ok
}

// creditLabour records work a member did for a bot-run corporation.
func (m *Mod) creditLabour(ctx context.Context, corpID, charID int64, kind string, credit float64, detail string) {
	credit = math.Floor(credit)
	if corpID == 0 || charID == 0 || credit < 1 {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_labour (corporationID, characterID, at, kind, credit, detail)
		VALUES (?, ?, ?, ?, ?, ?)`,
		corpID, charID, clock.Unix(), truncate(kind, 24), credit, truncate(detail, 255)); err != nil {
		rlogf("rolebots: labour credit %d/%d: %v", corpID, charID, err)
	}
}

// labourRow is one credit for creditLabourBatch.
type labourRow struct {
	CharID int64
	Kind   string
	Credit float64
	Detail string
}

// creditLabourBatch is creditLabour for many rows in one statement: an ore drop credits every member of a
// fleet per ore type.
func (m *Mod) creditLabourBatch(ctx context.Context, corpID int64, rows []labourRow) {
	if corpID == 0 || len(rows) == 0 {
		return
	}
	q := `INSERT INTO rolebot_corp_labour (corporationID, characterID, at, kind, credit, detail) VALUES `
	args := make([]any, 0, 6*len(rows))
	now := clock.Unix()
	n := 0
	for _, r := range rows {
		credit := math.Floor(r.Credit)
		if r.CharID == 0 || credit < 1 {
			continue
		}
		if n > 0 {
			q += ", "
		}
		q += "(?, ?, ?, ?, ?, ?)"
		args = append(args, corpID, r.CharID, now, truncate(r.Kind, 24), credit, truncate(r.Detail, 255))
		n++
	}
	if n == 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx, q, args...); err != nil {
		rlogf("rolebots: labour credit batch for %d: %v", corpID, err)
	}
}

// labourKindOf names a payCorpWage reason as a labour kind: the reasons are
// "trade run", "moon survey", "<leg> haul" and "carrying <print> to …".
func labourKindOf(why string) string {
	switch {
	case why == "trade run":
		return "traderun"
	case why == "moon survey":
		return "survey"
	case strings.HasSuffix(why, " haul"):
		return "haul"
	case strings.HasPrefix(why, "carrying "):
		return "printmove"
	}
	return "work"
}

// payrollOperatingProfit is what the corporation earned from its business
// over the window: every line but the capital flows, the officer salary and
// the payroll itself. Pure.
func payrollOperatingProfit(lines []corpLine) float64 {
	skip := map[string]bool{lineSalary: true, linePayroll: true}
	for _, l := range capitalLines {
		skip[l] = true
	}
	sum := 0.0
	for _, l := range lines {
		if !skip[l.Line] {
			sum += l.Net
		}
	}
	return sum
}

// payrollPot is the week's payroll: the CEO's share of a positive operating
// profit, never more than payrollCapitalCap of free capital. Pure.
func payrollPot(profit, share, free float64) float64 {
	if profit <= 0 || share <= 0 || free <= 0 {
		return 0
	}
	return math.Floor(math.Min(profit*share, free*payrollCapitalCap))
}

// payrollSplit divides the pot by weight, floored to whole ISK; a share under
// payrollMinCheque stays in the treasury. Pure.
func payrollSplit(pot float64, weights map[int64]float64) map[int64]float64 {
	total := 0.0
	for _, w := range weights {
		if w > 0 {
			total += w
		}
	}
	if total <= 0 || pot < payrollMinCheque {
		return nil
	}
	out := map[int64]float64{}
	for id, w := range weights {
		if w <= 0 {
			continue
		}
		if amt := math.Floor(pot * w / total); amt >= payrollMinCheque {
			out[id] = amt
		}
	}
	return out
}

// payrollStagger spreads first paydays over the week, so every corporation in
// the game does not sign its cheques in the same hour. Pure.
func payrollStagger(corpID int64) int64 {
	if corpID < 0 {
		corpID = -corpID
	}
	return (corpID % 7) * 86400
}

// payrollFirstRow opens a corporation's first payroll window: it starts now
// and its payday falls 1-7 days out, staggered by corpID. Pure.
func payrollFirstRow(now, corpID int64) (last, next int64) {
	return now, now + payrollEverySec - payrollStagger(corpID)
}

// payrollLegacyRow re-opens a row written before nextPayAt existed, whose
// lastPayrollAt was backdated to stagger the payday: the payday stays where
// it was, and a never-paid row's window opens now instead of in the past.
// Pure.
func payrollLegacyRow(last, now int64, paid int) (newLast, next int64) {
	next = last + payrollEverySec
	if paid == 0 {
		return now, max(next, now)
	}
	return last, next
}

// payrollMember is one member's claim on a week's payroll.
type payrollMember struct {
	CharID int64
	Name   string
	Human  bool
	Merit  float64 // points earned in the window
	Credit float64 // labour credited in the window
	Rank   int
	Weight float64
}

// payrollWeights reads what each eligible member earned between from and to.
// Merit is the weight; with the ladder off (no merit at all) labour credit is.
func (m *Mod) payrollWeights(ctx context.Context, c *corpCEO, from, to int64) (map[int64]*payrollMember, error) {
	members, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return nil, err
	}
	out := map[int64]*payrollMember{}
	for _, cm := range members {
		if cm.Eligible() {
			out[cm.CharID] = &payrollMember{CharID: cm.CharID, Name: cm.Name, Human: cm.Human()}
		}
	}
	sum := func(q string, set func(*payrollMember, float64)) error {
		rows, err := m.db.QueryContext(ctx, q, c.CorpID, from, to)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var v float64
			if rows.Scan(&id, &v) == nil {
				if pm := out[id]; pm != nil {
					set(pm, v)
				}
			}
		}
		return rows.Err()
	}
	if err := sum(`
		SELECT characterID, SUM(points) FROM rolebot_corp_merit_log
		 WHERE corporationID = ? AND at >= ? AND at < ? GROUP BY characterID`,
		func(pm *payrollMember, v float64) { pm.Merit = v }); err != nil {
		return nil, fmt.Errorf("payroll merit: %w", err)
	}
	if err := sum(`
		SELECT characterID, SUM(credit) FROM rolebot_corp_labour
		 WHERE corporationID = ? AND at >= ? AND at < ? GROUP BY characterID`,
		func(pm *payrollMember, v float64) { pm.Credit = v }); err != nil {
		return nil, fmt.Errorf("payroll labour: %w", err)
	}
	ranks := m.meritRanksOf(ctx, c.CorpID)
	anyMerit := false
	for _, pm := range out {
		pm.Rank = ranks[pm.CharID]
		if pm.Merit > 0 {
			anyMerit = true
		}
	}
	for _, pm := range out {
		if anyMerit {
			pm.Weight = math.Max(0, pm.Merit) * meritPayMult(pm.Rank)
		} else {
			pm.Weight = pm.Credit
		}
	}
	return out, nil
}

// payrollDue runs the weekly payroll when it is due. The Treasurer's to run
// (corpstaff_treasurer.go), the CEO's when there is none (corpceo_treasury.go).
func (m *Mod) payrollDue(ctx context.Context, c *corpCEO, ch Charter) error {
	now := clock.Unix()
	var last, next int64
	var paidBefore int
	err := m.db.QueryRowContext(ctx,
		`SELECT lastPayrollAt, nextPayAt, lastPaid FROM rolebot_corp_payroll WHERE corporationID = ?`,
		c.CorpID).Scan(&last, &next, &paidBefore)
	if errors.Is(err, sql.ErrNoRows) {
		// First sight of this corporation: the window opens now and only the first payday is staggered, since a
		// window reaching back would count wages already paid and merit already earned.
		last, next = payrollFirstRow(now, c.CorpID)
		_, err = m.db.ExecContext(ctx, `
			INSERT IGNORE INTO rolebot_corp_payroll (corporationID, lastPayrollAt, nextPayAt) VALUES (?, ?, ?)`,
			c.CorpID, last, next)
		return err
	}
	if err != nil {
		return fmt.Errorf("payroll row: %w", err)
	}
	if next == 0 {
		// A row from before nextPayAt existed: its window was opened in the
		// past to stagger the payday. Keep the payday, open the window now.
		last, next = payrollLegacyRow(last, now, paidBefore)
		_, err = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_payroll SET lastPayrollAt = ?, nextPayAt = ? WHERE corporationID = ?`,
			last, next, c.CorpID)
		return err
	}
	if now < next {
		return nil
	}
	// A dividend paid in the last day leaves the payroll for tomorrow
	// (corpgrowth.go): the two would otherwise count the same profit.
	if now-m.lastLedgerAt(ctx, c.CorpID, lineDividend) < payoutSpacingSec {
		return nil
	}
	if ctx.Value(opLockHeldKey{}) == nil {
		lk := corpOpLock(c.CorpID)
		lk.Lock()
		defer lk.Unlock()
	}

	lines, err := m.corpPnL(ctx, c.CorpID, last)
	if err != nil {
		return err
	}
	profit := payrollOperatingProfit(lines)
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return err
	}
	// Under the tower line part of the share is kept to grow on (corpgrowth.go).
	pot := payrollPot(profit, ch.PayrollShare*growthPayoutMul(free), free)
	members, err := m.payrollWeights(ctx, c, last, now)
	if err != nil {
		return err
	}
	weights := map[int64]float64{}
	totalMerit := 0.0
	for id, pm := range members {
		weights[id] = pm.Weight
		totalMerit += pm.Merit
	}
	cheques := payrollSplit(pot, weights)

	// Biggest cheque first: if the wallet runs short part-way, the members
	// who carried the week are the ones paid.
	ids := make([]int64, 0, len(cheques))
	for id := range cheques {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if cheques[ids[i]] != cheques[ids[j]] {
			return cheques[ids[i]] > cheques[ids[j]]
		}
		return ids[i] < ids[j]
	})
	corpName := m.corpNameOf(ctx, c.CorpID)
	paid, paidN := 0.0, 0
	for _, id := range ids {
		amt := cheques[id]
		pm := members[id]
		_, bal, err := m.corpWithdraw(ctx, c.CorpID, id, amt, "Weekly payroll")
		if err != nil {
			if errors.Is(err, evedb.ErrInsufficientFunds) {
				break
			}
			return err
		}
		m.pushBalance(id, bal)
		m.corpBook(ctx, c.CorpID, linePayroll, -amt, id,
			fmt.Sprintf("weekly payroll — %s (%.0f merit)", pm.Name, pm.Merit))
		paid += amt
		paidN++
		if pm.Human {
			m.mailPayslip(ctx, c, corpName, pm, amt, profit, ch.PayrollShare, totalMerit)
		}
	}

	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_labour SET paidAt = ? WHERE corporationID = ? AND paidAt = 0 AND at <= ?`,
		now, c.CorpID, now); err != nil {
		return fmt.Errorf("payroll stamp: %w", err)
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_payroll (corporationID, lastPayrollAt, nextPayAt, lastProfit, lastPot, lastPaid)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE lastPayrollAt = VALUES(lastPayrollAt), nextPayAt = VALUES(nextPayAt),
		  lastProfit = VALUES(lastProfit), lastPot = VALUES(lastPot), lastPaid = VALUES(lastPaid)`,
		c.CorpID, now, now+payrollEverySec, profit, pot, paidN); err != nil {
		return fmt.Errorf("payroll save: %w", err)
	}

	var text string
	switch {
	case profit <= 0:
		text = fmt.Sprintf("No payroll this week: the corporation's business ran %s at a loss.", iskString(-profit))
	case paidN == 0:
		text = fmt.Sprintf("No payroll this week: %s of operating profit left nothing to share once the reserve was kept.",
			iskString(profit))
	default:
		text = fmt.Sprintf("Weekly payroll: %s to %d member(s) — %.0f%% of %s operating profit, split by merit.",
			iskString(paid), paidN, ch.PayrollShare*100, iskString(profit))
	}
	m.ceoLog(ctx, c, powerTreasury, "payroll", 0, paid, "%s", text)
	m.queueCorpNews(ctx, c.CorpID, newsMoney, text)
	return nil
}

// mailPayslip tells a human member what the week paid and why.
func (m *Mod) mailPayslip(ctx context.Context, c *corpCEO, corpName string, pm *payrollMember, amt, profit, share, totalMerit float64) {
	var b strings.Builder
	line(&b, "%s,", pm.Name)
	b.WriteString("<br>")
	line(&b, "Your share of this week's payroll from %s: %s.", corpName, iskString(amt))
	line(&b, "The corporation's business made %s this week and pays %.0f%% of it to its members.",
		iskString(profit), share*100)
	if totalMerit > 0 {
		line(&b, "You earned %.0f of the %.0f merit the members earned, weighed ×%.2f for your rank (%s).",
			pm.Merit, totalMerit, meritPayMult(pm.Rank), meritRankName(pm.Rank))
	}
	if pm.Credit > 0 {
		line(&b, "Work credited to you this week: %s.", iskString(pm.Credit))
	}
	line(&b, "Your record: %s.", igbLink(deskViewPath(c.CorpID, pm.CharID), "your desk"))
	from, hat := m.letterFrom(ctx, c, mailFromTreasury)
	b.WriteString(m.signature(ctx, from, hat, corpName))
	m.mailPilot(ctx, pm.CharID, from, corpName+": weekly payroll", b.String())
}
