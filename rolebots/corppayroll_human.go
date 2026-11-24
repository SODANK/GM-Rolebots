package rolebots

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The Treasury's payroll for a human chair's corporation. A bot-run corporation shares its profit by
// merit once a week (corppayroll.go). A human chair's work board pays by the job, and until now each
// wage left the wallet the moment the work was done, by whichever bot finished it. Now the wage is
// written down as owed (corpowed.go, held against free capital like any promise) and the Treasury
// pays it: the bot head of Finance if there is one, else the chair's own pass. One cheque to each
// member for everything owed to them, with a payslip that lists the jobs. A wallet that runs short
// pays the oldest members' cheques first and leaves the rest owed for the next run.

const (
	chairPayrollEverySec = 900
	payrollPerRun        = 400
)

// treasuryPaysLine is said under a preview that carries a wage.
const treasuryPaysLine = "The wage is owed when the work is done and paid by the corporation's Treasury in its next payroll."

// wageNote says where a wage came from. Pure.
func wageNote(going bool) string {
	if going {
		return " (the going rate)"
	}
	return ""
}

// owedWage is one line of what the corporation owes a member.
type owedWage struct {
	ID     int64
	Amount float64
	Why    string
	At     int64
}

// payslipLines words a member's cheque. Pure.
func payslipLines(items []owedWage) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString(fmt.Sprintf("%s: %s<br>", esp.IGBText(it.Why), iskString(it.Amount)))
	}
	return b.String()
}

// groupOwed gathers owed wages by member, the member owed the longest first, each member's jobs
// oldest first. Pure.
func groupOwed(rows []owedRow) (order []int64, byChar map[int64][]owedWage) {
	byChar = map[int64][]owedWage{}
	first := map[int64]int64{}
	for _, r := range rows {
		if _, ok := first[r.CharID]; !ok {
			first[r.CharID] = r.ID
			order = append(order, r.CharID)
		}
		byChar[r.CharID] = append(byChar[r.CharID], owedWage{r.ID, r.Amount, r.Why, r.At})
	}
	sort.SliceStable(order, func(i, j int) bool { return first[order[i]] < first[order[j]] })
	return order, byChar
}

type owedRow struct {
	ID, CharID, At int64
	Amount         float64
	Why            string
}

// treasuryPayroll is the Treasury's turn: it pays what the corporation owes its members. issuer is
// the bot that signs the payslips and office the title they carry.
func (m *Mod) treasuryPayroll(ctx context.Context, corpID, issuer int64, office string) (members int, total float64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT owedID, characterID, amount, why, owedAt FROM rolebot_corp_owed
		 WHERE corporationID = ? ORDER BY owedID LIMIT ?`, corpID, payrollPerRun)
	if err != nil {
		return 0, 0
	}
	var list []owedRow
	for rows.Next() {
		var r owedRow
		if rows.Scan(&r.ID, &r.CharID, &r.Amount, &r.Why, &r.At) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	forget := func(items []owedWage) {
		for _, it := range items {
			if _, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_owed WHERE owedID = ?`, it.ID); err != nil {
				rlogf("rolebots: owed %d: %v", it.ID, err)
				continue
			}
			m.closeCommit(ctx, owedCommit, it.ID)
		}
	}
	var fresh []owedRow
	for _, r := range list {
		if clock.Unix()-r.At >= owedKeepMemory {
			forget([]owedWage{{ID: r.ID}}) // too old to mean anything: off the books
			continue
		}
		fresh = append(fresh, r)
	}
	order, byChar := groupOwed(fresh)
	for _, id := range order {
		if ctx.Err() != nil {
			return members, total
		}
		items := byChar[id]
		sum := 0.0
		for _, it := range items {
			sum += it.Amount
		}
		_, bal, err := m.corpWithdraw(ctx, corpID, id, sum, "Corporation payroll")
		if errors.Is(err, evedb.ErrInsufficientFunds) {
			return members, total // the oldest are paid; the rest wait for the wallet
		}
		if err != nil {
			rlogf("rolebots: payroll %d: %v", id, err)
			continue
		}
		m.pushBalance(id, bal)
		for _, it := range items {
			m.corpBook(ctx, corpID, lineWages, -it.Amount, id, it.Why+" — "+m.charNameOr(ctx, id))
		}
		forget(items)
		members++
		total += sum
		if !m.isSynthetic(id) && issuer != 0 {
			m.mailPilot(ctx, id, issuer, "Payroll: "+iskString(sum),
				"The corporation's Treasury has paid you <b>"+iskString(sum)+"</b> for work done:<br><br>"+payslipLines(items)+
					m.signature(ctx, issuer, esp.IGBText(office), m.corpNameOf(ctx, corpID)))
		}
	}
	return members, total
}

// chairPayroll is the human chair's Treasury, on its own cadence. The bot head of Finance issues it
// when there is one; the chair's acting bot does otherwise.
func (m *Mod) chairPayroll(ctx context.Context, c *corpCEO, heads map[corpArm]corpHead) {
	if !deskDue(c.CorpID, "chair.payroll", chairPayrollEverySec) {
		return
	}
	issuer, office := c.CharID, "Treasury"
	if h, ok := heads[armFinance]; ok && m.isSynthetic(h.CharID) && !popIsOffline(h.CharID) {
		issuer, office = h.CharID, "head of Finance"
	}
	if n, total := m.treasuryPayroll(ctx, c.CorpID, issuer, office); n > 0 {
		m.humanLog(ctx, c.CorpID, m.corpCEOID(ctx, c.CorpID), "payroll", c.CorpID, total, "the Treasury paid %d member(s) %s", n, iskString(total))
	}
}
