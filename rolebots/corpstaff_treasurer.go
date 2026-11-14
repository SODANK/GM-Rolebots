package rolebots

import (
	"context"
	"sync"

	evedb "gurimoon/db"
)

// The Treasurer: the Accountant the roles power appoints, paying out what the CEO's
// charter says the corporation pays. The CEO sets policy on its own think (the tax rate,
// the charter's reserve, stipend share, dividend ratio and cadence, the Industry
// Director's salary); the Treasurer executes it on the staff desk's cadence, in its own
// name: due-dated payments every pass (that salary, the weekly payroll in corppayroll.go,
// the dividend) and discretionary spending (skillbooks, stipends) at its own pace
// (treasurerStyle.LagSec). Its temperament bends how, never what: every bend spends less
// or later than the charter allows (TestTreasurerBendNeverSpendsMore). Caution keeps extra
// reserve, envy shaves the stipend budget, patience delays, loyalty pays directive workers
// first. Cash wages survive only on a human chair's board (corpwork.go).

// treasurerStyle is a Treasurer's temperament applied to the charter.
type treasurerStyle struct {
	Cushion        float64 // extra reserve, as a fraction of the CEO's
	StipendMul     float64 // share of the CEO's stipend budget it will spend
	LagSec         int64   // least time between discretionary payouts
	DirectiveFirst bool    // stipends to directive workers first
}

// neutralTreasury is the CEO paying out itself: the charter exactly.
var neutralTreasury = treasurerStyle{StipendMul: 1}

// treasurerBend is a Treasurer's style. Pure.
func treasurerBend(p pilotIdentity) treasurerStyle {
	return treasurerStyle{
		Cushion:        0.5 * posPole(-p.trait(TraitCaution)),
		StipendMul:     1 - 0.4*posPole(p.trait(TraitGreed)),
		LagSec:         int64(bend(p.trait(TraitPatience), 7200, 3600, 0)),
		DirectiveFirst: posPole(-p.trait(TraitLoyalty)) > 0.3,
	}
}

// treasurerDesk is one pass of the Treasurer's desk.
func (m *Mod) treasurerDesk(ctx context.Context, c *corpCEO, ch Charter, t *staffMember) error {
	st := treasurerBend(t.Ident)

	// Due-dated first: a salary and a dividend have dates, not moods.
	if p, err := m.loadCorpPolicy(ctx, c.CorpID); err == nil && p.DirectorID != 0 {
		before := p.SalaryAt
		if err := m.payDirectorSalary(ctx, c, ch, &p); err != nil {
			return err
		}
		if p.SalaryAt != before {
			if err := m.saveCorpPolicyRow(ctx, p); err != nil {
				return err
			}
		}
	}
	// The weekly payroll (corppayroll.go): before the dividend, because
	// members are paid out of the week's profit before owners are.
	if err := m.payrollDue(ctx, c, ch); err != nil {
		m.logBot(c.CharID, logError, "ceo", "payroll: %v", err)
	}
	if sharesEnabled() && c.powerOn(powerShares) {
		if _, err := m.payDividend(ctx, c, ch, false); err != nil {
			m.logBot(c.CharID, logError, "ceo", "dividend: %v", err)
		}
	}

	if !deskDue(c.CorpID, "treasury", st.LagSec) {
		return nil
	}
	return m.treasuryDisburse(ctx, c, ch, st)
}

// treasuryDisburse is the discretionary half of the treasury: the books a position needs,
// then stipends, out of the surplus over the reserve and every open commitment. The CEO
// runs it with neutralTreasury when no Treasurer is appointed.
func (m *Mod) treasuryDisburse(ctx context.Context, c *corpCEO, ch Charter, st treasurerStyle) error {
	// The week's split between the arms (corpenvelope.go), before anything
	// is paid out of what is left over.
	m.refreshEnvelopes(ctx, c, ch)
	balance, err := evedb.GetCorpBalance(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	// Money already promised (wages on posted work, buyback escrow, purchase advances:
	// corpcommit.go) is not surplus.
	surplus := balance - m.corpReserve(ctx, c.CorpID, ch)*(1+st.Cushion) - m.corpCommitted(ctx, c.CorpID)
	if surplus <= 0 {
		return nil
	}

	// ── books for positions it needs filled ─────────────────────────────
	if spent, err := m.ceoFundBooks(ctx, c, surplus); err != nil {
		return err
	} else {
		surplus -= spent
	}
	if surplus <= 0 {
		return nil
	}

	// ── stipends ────────────────────────────────────────────────────────
	// No stipends from a thin treasury: the weekly payroll and the members' work carry them.
	if balance < ceoStipendFloorISK {
		return nil
	}
	budget := surplus*ch.StipendShare*st.StipendMul -
		m.ceoSpentSince(ctx, c.CorpID, "stipend", clock.Unix()-ceoStipendWeek)
	if budget <= 0 {
		return nil
	}
	return m.ceoStipends(ctx, c, budget, st.DirectiveFirst)
}

// treasuryActorCtx attributes a payment made outside the desk (the wage
// sweep) to the corporation's Treasurer, when it has one. cache holds one
// answer per corporation per sweep.
func (m *Mod) treasuryActorCtx(ctx context.Context, corpID int64, cache *sync.Map) context.Context {
	if v, ok := cache.Load(corpID); ok {
		if a, ok := v.(staffActor); ok && a.CharID != 0 {
			return withStaffActor(ctx, a)
		}
		return ctx
	}
	var a staffActor
	if c, err := getCEO(ctx, m.db, corpID); err == nil && c != nil {
		if s, err := m.loadCorpStaff(ctx, corpID, c.CharID); err == nil && s.Treasurer != nil {
			a = s.executor(s.Treasurer, "Treasurer")
		}
	}
	cache.Store(corpID, a)
	if a.CharID != 0 {
		return withStaffActor(ctx, a)
	}
	return ctx
}
