package rolebots

// What share of a wallet one skillbook may be.
//
// Skillbooks are the largest single sink in the bot economy. applySkillPlan
// already refuses a book that would take the pilot to or below its holdback
// (a solvency rule, which stays). This is the second question, asked after
// solvency: is one book a sensible fraction of what the pilot has. The share is
// taken of spendable/walletFloorFor (broke.go), not of the balance.
//
// A book refused here is deferred, not cancelled: applySkillPlan truncates its
// plan at the first book it cannot have and the next replan resumes there.

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

// skillBookFracV caches the dial in hundredths, so the hot path is an atomic
// load. 0 means the shipped default, never zero percent.
var skillBookFracV atomic.Int64

const (
	// settingSkillBookWalletFrac is the operator's dial on the share.
	settingSkillBookWalletFrac = "rolebots.skills.bookWalletFrac"

	// skillBookWalletFracDefault is the share of spendable one book may cost: a
	// third, so two books' worth of headroom remains after any purchase and the
	// plan can continue.
	skillBookWalletFracDefault = 0.33

	// The bounds. At 1.0 the rule is off. Below 0.05 no pilot could buy anything
	// and the planner would stop working.
	skillBookWalletFracMin = 0.05
	skillBookWalletFracMax = 1.00

	// skillBookAlwaysAffordable is the price under which the rule does not apply,
	// so cheap rank-1 books never trap a poor pilot. It equals lootListMinValue,
	// the module's threshold for an amount of money that is a decision.
	skillBookAlwaysAffordable = 20_000.0
)

// skillBookWalletFrac is the live share, clamped.
func skillBookWalletFrac() float64 {
	if v := skillBookFracV.Load(); v > 0 {
		return clampRange(float64(v)/100, skillBookWalletFracMin, skillBookWalletFracMax)
	}
	return skillBookWalletFracDefault
}

// bookAffordable is the rule: may a pilot holding balance buy a book at price?
// Pure. It says nothing about solvency and is asked after the holdback check in
// applySkillPlan, never instead of it.
func bookAffordable(balance, price float64) bool {
	if price <= 0 {
		return true
	}
	if price <= skillBookAlwaysAffordable {
		return true
	}
	return price <= spendable(balance)*skillBookWalletFrac()
}

// bookDeferralNote is the line the truncation logs, in the voice of the
// solvency refusal beside it: what it wanted, what it holds, what would let it through.
func bookDeferralNote(name string, price, balance float64) string {
	need := price / skillBookWalletFrac()
	return "the " + name + " book is " + iskString(price) +
		" against " + iskString(spendable(balance)) +
		" it can spend; one book is not allowed to be more than a " +
		iskString(spendable(balance)*skillBookWalletFrac()) +
		" slice of that, so it waits until it can spend about " + iskString(need)
}

// loadSkillBudgetSettings reads the dial at boot. Never fatal: an unreadable
// dial means the shipped default rather than a module that refuses to start.
func (m *Mod) loadSkillBudgetSettings(ctx context.Context) {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingSkillBookWalletFrac)
	if err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingSkillBookWalletFrac, err)
		return
	}
	if !ok {
		return
	}
	f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if perr != nil || f <= 0 {
		return
	}
	skillBookFracV.Store(int64(clampRange(f, skillBookWalletFracMin, skillBookWalletFracMax) * 100))
}
