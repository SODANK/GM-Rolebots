package rolebots

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

// Bot insurance: Pend Insurance writes cover for a rolebot's hull the way it does for a player's.
//
// A lost hull costs its full price and pays nothing back (pod.go loseHull) unless insured. The core sells
// insurance (server/insurance) and pays it on a player's death (main.go death handler). A bot uses the same
// ladder, table and refType-19 journal, so its contract shows in the client's insurance window. The premium
// is bought when a pilot boards a hull while docked (boardShip) and paid out in loseHull before the hull dies.

const (
	settingHuntByBounty = "rolebots.combat.huntByBounty"
	settingInsurance    = "rolebots.insurance"

	// botInsureMinBase: hulls worth less than this are not worth a contract
	// (rookie ships, shuttles). Insurance base price is invTypes.basePrice.
	botInsureMinBase = 250_000.0
	// botInsureWalletShare caps a premium at this share of the wallet; the
	// tier steps down until one fits, and no tier fitting means no cover.
	botInsureWalletShare = 0.10
	// botInsureTermTicks is the core's 12-week term in Win32 FILETIME ticks
	// (server/insurance insuranceDuration).
	botInsureTermTicks = int64(12 * 7 * 86400 * 10_000_000)

	refTypeInsurance = 19
)

var (
	huntByBountyV atomic.Bool
	insuranceV    atomic.Bool
)

// HuntByBounty reports whether hunters rank winnable ground by what it pays
// (huntladder.go).
func HuntByBounty() bool { return huntByBountyV.Load() }

// BotInsurance reports whether rolebots buy and claim hull insurance.
func BotInsurance() bool { return insuranceV.Load() }

func init() {
	huntByBountyV.Store(true)
	insuranceV.Store(true)
}

// loadCareerSettings reads the two switches. Both default ON; "0" turns one
// off.
func (m *Mod) loadCareerSettings(ctx context.Context) {
	for _, sw := range []struct {
		key   string
		cache *atomic.Bool
	}{{settingHuntByBounty, &huntByBountyV}, {settingInsurance, &insuranceV}} {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, sw.key)
		if err != nil {
			rlogf("rolebots: read %s: %v (using the default)", sw.key, err)
			continue
		}
		if ok {
			sw.cache.Store(strings.TrimSpace(v) != "0")
		}
	}
}

// insureTier is one rung of the core's insurance ladder.
type insureTier struct {
	Payout, Premium float64 // fractions of the insurance base price
}

// botInsureLadder is server/insurance's insuranceLadder, best cover first.
var botInsureLadder = []insureTier{
	{1.0, 0.30}, {0.9, 0.25}, {0.8, 0.20}, {0.7, 0.15}, {0.6, 0.10}, {0.5, 0.05},
}

// pickInsureTier chooses the cover a pilot buys for a hull of this base price with this wallet. Nerve sets
// the ceiling: a cautious pilot (engage margin 2+) wants full cover, an ordinary one 80%, a reckless one
// (under 1) the basic 50%. The wallet steps it down until the premium fits. ok=false means not worth
// insuring or not able to. Pure.
func pickInsureTier(engageMargin, basePrice, wallet float64) (insureTier, bool) {
	if basePrice < botInsureMinBase || wallet <= 0 {
		return insureTier{}, false
	}
	start := 2 // 80%
	switch {
	case engageMargin >= 2.0:
		start = 0
	case engageMargin < 1.0:
		start = len(botInsureLadder) - 1
	}
	for _, t := range botInsureLadder[start:] {
		if basePrice*t.Premium <= wallet*botInsureWalletShare {
			return t, true
		}
	}
	return insureTier{}, false
}

// insurePayout is what a contract pays on loss, or 0 when it must not pay:
// a fraction outside (0, 1] (the core's ISK-printer guard), an expired term,
// or no base price. Pure.
func insurePayout(c *evedb.InsuranceContract, basePrice float64, nowTicks int64) float64 {
	if c == nil || basePrice <= 0 || c.Fraction <= 0 || c.Fraction > 1 {
		return 0
	}
	if c.EndDate > 0 && nowTicks > c.EndDate {
		return 0
	}
	return basePrice * c.Fraction
}

// maybeInsureHull buys cover for a hull the pilot just boarded while docked,
// if it owns the hull, the hull is worth insuring and no live contract stands.
// Never fails the board: every error is logged and swallowed.
func (m *Mod) maybeInsureHull(ctx context.Context, b *botRun, shipID int64) {
	if !BotInsurance() || b == nil || b.StationID == 0 || shipID == 0 {
		return
	}
	var typeID, ownerID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID, ownerID FROM entity WHERE itemID = ?`, shipID).Scan(&typeID, &ownerID); err != nil {
		return
	}
	if ownerID != b.CharID || typeID == capsuleTypeID {
		return // a corp hull is the corporation's to insure
	}
	now := win32Now()
	if c, err := evedb.GetContractForShip(ctx, m.db, shipID); err != nil {
		rlogf("rolebots: insurance lookup ship %d: %v", shipID, err)
		return
	} else if c != nil && (c.EndDate == 0 || c.EndDate > now) {
		return
	}
	base, err := evedb.GetInsurancePrice(ctx, m.db, typeID)
	if err != nil {
		return
	}
	wallet, err := evedb.GetCharacterBalance(ctx, m.db, b.CharID)
	if err != nil {
		return
	}
	tier, ok := pickInsureTier(b.Tune.EngageMargin, base, wallet)
	if !ok {
		return
	}
	premium := base * tier.Premium
	paid, bal, err := evedb.TryChargeISK(ctx, m.db, b.CharID, premium)
	if err != nil || !paid {
		return
	}
	if _, err := evedb.InsureShip(ctx, m.db, shipID, b.CharID, tier.Payout, now, now+botInsureTermTicks); err != nil {
		// The premium is gone and the contract is not: put the money back.
		_ = evedb.GiveISK(ctx, m.db, b.CharID, premium)
		rlogf("rolebots: insure ship %d for char %d: %v", shipID, b.CharID, err)
		return
	}
	_ = evedb.InsertJournalEntry(ctx, m.db, b.CharID, refTypeInsurance,
		b.CharID, evedb.OwnerPendInsurance, -premium, bal, "")
	m.logBot(b.CharID, logInfo, "insurance", "insured %s for %.0f%% cover — premium %s",
		m.shipLabel(ctx, shipID), tier.Payout*100, iskString(premium))
}

// claimHullInsurance pays out the contract on a hull being destroyed. Called
// from loseHull while the entity row still exists. The contract row is
// deleted FIRST and the payout made only if this call deleted it, so a death
// handled twice pays once.
func (m *Mod) claimHullInsurance(ctx context.Context, charID, shipID, shipTypeID int64) {
	if !BotInsurance() || shipID == 0 {
		return
	}
	c, err := evedb.GetContractForShip(ctx, m.db, shipID)
	if err != nil || c == nil {
		return
	}
	base, err := evedb.GetInsurancePrice(ctx, m.db, shipTypeID)
	if err != nil {
		return
	}
	payout := insurePayout(c, base, win32Now())
	res, err := m.db.ExecContext(ctx, `DELETE FROM insurance WHERE contractID = ?`, c.ContractID)
	if err != nil {
		rlogf("rolebots: close insurance contract %d: %v", c.ContractID, err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 || payout <= 0 {
		return
	}
	payee := c.OwnerID
	if payee == 0 {
		payee = charID
	}
	var bal float64
	if payee != charID {
		if err := evedb.CreditCorpWallet(ctx, m.db, payee, payout); err != nil {
			rlogf("rolebots: insurance payout to corp %d: %v", payee, err)
			return
		}
		bal, _ = evedb.GetCorpBalance(ctx, m.db, payee)
	} else {
		if err := evedb.GiveISK(ctx, m.db, charID, payout); err != nil {
			rlogf("rolebots: insurance payout to char %d: %v", charID, err)
			return
		}
		bal, _ = evedb.GetCharacterBalance(ctx, m.db, charID)
	}
	_ = evedb.InsertJournalEntry(ctx, m.db, payee, refTypeInsurance,
		evedb.OwnerPendInsurance, payee, payout, bal, "")
	m.logBot(charID, logInfo, "insurance", "%s",
		fmt.Sprintf("insurance paid out %s on the lost hull", iskString(payout)))
}
