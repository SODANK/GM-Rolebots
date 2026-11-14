package rolebots

import (
	"context"
	"errors"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

// Poverty: what a pilot does when it cannot pay for the work it wanted.
//
// A money refusal repeats on every pass, because a hangar earns nothing, so retrying the appetite
// draw only ends in a halt. A refusal therefore asks what will pay: the pilot goes and earns, cheapest
// work first, whatever its traits think (brokeEarn). While botRun.Broke is set, selectFitForActivity
// falls back to the free rookie hull (rookie.go), ensureActivityShip grants the free starter lines of
// the fit, and anything still missing is left off. Nothing here spends ISK the pilot lacks or mints
// any; the one thing created is the starter kit character creation gives away, to a pilot with none.

// errCannotAfford marks a refusal whose cause is the wallet. It is a sentinel because planSolo must
// tell a poor pilot (fixed by earning) from an unlucky one.
var errCannotAfford = errors.New("cannot afford it")

// shortfallError is a money refusal that knows the number, for a gang whip-round that needs to know
// how poor a pilot is. Refusals with no figure stay plain %w wraps of the sentinel.
type shortfallError struct {
	What string  // what could not be paid for, in the operator's language
	Need float64 // what it costs
	Have float64 // what the wallet holds
	// Msg replaces the rendered sentence when a caller has better prose, as the build book's refusal
	// does. planSolo tests the type, not the text.
	Msg string
}

func (e *shortfallError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("cannot afford %s (~%.0f ISK, wallet holds %.0f)", e.What, e.Need, e.Have)
}

// Unwrap is what makes errors.Is(err, errCannotAfford) true for these.
func (e *shortfallError) Unwrap() error { return errCannotAfford }

// shortfallOf reads the gap out of a refusal, 0 when it does not carry one.
func shortfallOf(err error) float64 {
	var se *shortfallError
	if !errors.As(err, &se) {
		return 0
	}
	// A margin on the bare gap: the estimate is an estimate, and procurement re-checks the price at the till.
	if gap := se.Need*1.05 - se.Have; gap > 0 {
		return gap
	}
	return 0
}

// ── the wallet floor ────────────────────────────────────────────────────────
// One sill under every discretionary spend, so a pilot keeps the price of getting back to work.
// See docs/comment-notes-b01.md#wallet-floor.
const (
	// walletFloorISK is the sill: about a rookie frigate and its fit, plus room for the listings that
	// turn goods back into money.
	walletFloorISK = 250_000.0
	// walletFloorMaxShare caps the floor at a share of the wallet, as holdback (skills.go) does: a floor
	// that takes the whole balance stops a poor pilot doing the cheap things that would make it richer.
	// Above about 333k the full floor applies.
	walletFloorMaxShare = 0.75
)

// walletFloorFor is what this balance must keep back. It yields on small wallets so the floor is
// never a cage (walletFloorMaxShare).
func walletFloorFor(balance float64) float64 {
	if balance <= 0 {
		return 0
	}
	if max := balance * walletFloorMaxShare; walletFloorISK > max {
		return max
	}
	return walletFloorISK
}

// spendable is what a pilot holding this balance may commit to a discretionary purchase, never
// negative. A purchase the pilot chose is sized with it, not with the raw balance. Money owed (an
// installed job fee) does not go through it.
func spendable(balance float64) float64 {
	if v := balance - walletFloorFor(balance); v > 0 {
		return v
	}
	return 0
}

// balanceNeededFor is the smallest balance whose spendable half covers cost, the exact inverse of
// spendable. The floor moves with the wallet, so pricing the gap against today's balance under-asks.
// spendable(B) is max(B - walletFloorISK, B*(1-walletFloorMaxShare)); the answer is the cheaper arm.
func balanceNeededFor(cost float64) float64 {
	if cost <= 0 {
		return 0
	}
	flat := cost + walletFloorISK
	share := cost / (1 - walletFloorMaxShare)
	if share < flat {
		return share
	}
	return flat
}

// floorShortfall builds the refusal for a spend the floor will not allow. Need is the balance the
// pilot would have to hold, not the price of the thing: that is what a gang whip-round raises.
func floorShortfall(what string, cost, balance float64) *shortfallError {
	return &shortfallError{
		What: what,
		Need: balanceNeededFor(cost),
		Have: balance,
	}
}

// brokeEarners is the order a skint pilot tries work in, cheapest capital first. Mining leads: rock
// pays with no stake. Trading needs a float and combat needs a ship that wins, but both are worth asking.
var brokeEarners = []string{ActMining, ActTrade, ActCombat}

// freeStarterModules are the lines a destitute bot is granted: basePrice 0 and exempt from skill
// checks (fitting.go). Weapons only, never their charges, and it includes the civilian gun.
// See docs/comment-notes-b01.md#free-starter-modules.
var freeStarterModules = map[int64]bool{
	basicMinerTypeID:           true,
	civGatlingAutocannonTypeID: true,
	civGatlingPulseLaserTypeID: true,
	civGatlingRailgunTypeID:    true,
	civLightElectronBlasterID:  true,
}

// brokeEarn is planSolo's last move before the halt bookkeeping: the pilot could not pay for the
// work it wanted, so it makes money. Reports whether something was planned. b.Broke is set for the
// attempt, so the same call asks whether the pilot can fly what it can get.
func (m *Mod) brokeEarn(ctx context.Context, b *botRun, blocked []string) bool {
	st := b.State
	now := clock.Unix()

	b.Broke = true
	defer func() { b.Broke = false }()

	m.logBot(b.CharID, logInfo, "sim",
		"it cannot pay for the work it wanted (%s) — going to earn instead, whatever it thinks of the job",
		joinAnd(blocked))

	// Before earning anything new, recover what the pilot owns: a warehouse run needs only the flight
	// (warehouse.go) and selling held ore and minerals needs no operator (cashInjectionPlan, liquidate.go).
	if took, err := m.cashInjectionPlan(ctx, b, true); err != nil {
		rlogf("rolebots: broke cash injection char=%d: %v", b.CharID, err)
	} else if took {
		m.clearFailure(ctx, st)
		return true
	}
	if took, err := m.warehouseRun(ctx, b); err != nil {
		rlogf("rolebots: broke warehouse run char=%d: %v", b.CharID, err)
	} else if took {
		m.clearFailure(ctx, st)
		m.logBot(b.CharID, logInfo, "sim",
			"broke — but it owns goods it left somewhere; fetching those before it takes any job")
		return true
	}

	for _, slug := range brokeEarners {
		def, ok := activityDefFor(slug)
		if !ok {
			continue
		}
		if err := def.Plan(m, ctx, b); err != nil {
			m.logBot(b.CharID, logInfo, "sim",
				"broke: %s is no good either (%v)", def.Label, err)
			continue
		}
		// Stamp the session like any other, so the forced work wears thin and the pilot returns to its own
		// appetites once it can afford them.
		st.Data.LastActivity = slug
		st.Data.ActivityStartedAt = now
		m.clearFailure(ctx, st)
		m.logBot(b.CharID, logInfo, "sim",
			"broke — %s it is, until the wallet says otherwise", def.Label)
		return true
	}
	return false
}

// ── the gang whip-round ─────────────────────────────────────────────────────
//
// A money refusal inside a gang plan asks the pack first: members chip in what their envy allows, the
// pilot buys the ship and the op flies. Otherwise the member earns alone (brokeEarn), then halts.
//
//   - It is asked once per gangSubsidyCooldownSec per pilot, whatever the answer.
//   - A donor gives a share of its wallet (Tuning.GangShareFrac) and never dips below gangSubsidyDonorFloor.
//   - Nothing moves unless the pledges cover the whole shortfall.

const (
	// gangShareFrac is the default cut of its own wallet a pilot hands a
	// broke packmate. Envy bends it hard in both directions (tuning.go).
	gangShareFrac = 0.20
	// gangSubsidyDonorFloor is what a donor keeps back whatever it gives: enough to replace a frigate and
	// its fit, so helping never turns the helper into the next pilot asking.
	gangSubsidyDonorFloor = 2_000_000.0
	// gangSubsidyCooldownSec is how long a pilot waits before asking its
	// gang for money again.
	gangSubsidyCooldownSec = 1800
)

// gangWhipRound raises what a gangmate is short and moves the ISK. It reports whether the full
// shortfall was covered; a partial collection is refused outright.
func (m *Mod) gangWhipRound(ctx context.Context, b *botRun, g *Gang, refusal error) bool {
	need := shortfallOf(refusal)
	if need <= 0 || g == nil {
		return false // a refusal with no figure on it cannot be funded
	}
	now := clock.Unix()
	if b.State.Data.SubsidyAt > now-gangSubsidyCooldownSec {
		return false
	}
	b.State.Data.SubsidyAt = now

	mems, err := gangMembers(ctx, m.db, g.GangID)
	if err != nil {
		m.logBot(b.CharID, logError, "gang", "asking the gang for a hand: %v", err)
		return false
	}

	// Who can give what. Richest first, so the fewest wallets are touched.
	type pledge struct {
		charID int64
		amount float64
	}
	var pledges []pledge
	var raised float64
	for _, mem := range mems {
		if mem.CharacterID == b.CharID {
			continue
		}
		bal, err := evedb.GetCharBalance(ctx, m.db, mem.CharacterID)
		if err != nil || bal <= gangSubsidyDonorFloor {
			continue
		}
		share := bal * m.donorShareFrac(ctx, mem.CharacterID)
		if spare := bal - gangSubsidyDonorFloor; share > spare {
			share = spare
		}
		if share < 1 {
			continue
		}
		pledges = append(pledges, pledge{mem.CharacterID, share})
		raised += share
	}
	if raised < need {
		if len(pledges) > 0 {
			m.logBot(b.CharID, logInfo, "gang",
				"asked the gang for %s and only %s was going spare — nobody is being left short over it",
				iskString(need), iskString(raised))
		}
		return false
	}
	sort.Slice(pledges, func(i, j int) bool { return pledges[i].amount > pledges[j].amount })

	// Take exactly what is needed, biggest wallet first; the last donor
	// makes up the remainder rather than handing over its whole share.
	left := need
	var given float64
	for _, p := range pledges {
		if left <= 0 {
			break
		}
		amount := p.amount
		if amount > left {
			amount = left
		}
		if err := m.handISK(ctx, p.charID, b.CharID, amount); err != nil {
			m.logBot(b.CharID, logError, "gang", "a packmate's hand-out did not go through: %v", err)
			continue
		}
		left -= amount
		given += amount
		m.logBot(p.charID, logInfo, "gang",
			"put %s into %s's wallet — the gang needs it in a ship, not in a hangar",
			iskString(amount), evedb.GetCharacterName(ctx, m.db, b.CharID))
	}
	if given <= 0 {
		return false
	}
	m.logBot(b.CharID, logInfo, "gang",
		"the gang had a whip-round: %s from %d packmate(s), which covers what it was short for the op",
		iskString(given), len(pledges))
	return left <= 0
}

// donorShareFrac is how much of its wallet one prospective donor will part with. It is read from the
// donor's own tuning, not the recipient's: the generosity is the giver's.
func (m *Mod) donorShareFrac(ctx context.Context, charID int64) float64 {
	traits, err := getTraits(ctx, m.db, charID)
	if err != nil {
		return gangShareFrac
	}
	f := resolveTuning(traits).GangShareFrac
	if f <= 0 {
		return 0
	}
	if f > 1 {
		f = 1
	}
	return f
}

// handISK moves money between two pilots as a plain donation: the refType 10 (GiveCash) a player's
// Give ISK writes, which the ransom watcher reads (piracy_prey.go). The debit is conditional in SQL
// so racing whip-rounds cannot overdraw a donor, and the credit shares its transaction. Journal rows
// follow outside it, best effort.
func (m *Mod) handISK(ctx context.Context, fromID, toID int64, amount float64) error {
	return m.handISKReason(ctx, fromID, toID, amount, "Gang hand-out")
}

// handISKReason is handISK with the journal line named by the caller; the transfer is identical (gangindustry.go).
func (m *Mod) handISKReason(ctx context.Context, fromID, toID int64, amount float64, reason string) error {
	if amount <= 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = balance - ? WHERE characterID = ? AND balance >= ?`,
		amount, fromID, amount)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("donor %d no longer holds %.0f ISK", fromID, amount)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = balance + ? WHERE characterID = ?`,
		amount, toID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	fromBal, _ := evedb.GetCharBalance(ctx, m.db, fromID)
	toBal, _ := evedb.GetCharBalance(ctx, m.db, toID)
	_ = evedb.InsertJournalEntry(ctx, m.db, fromID, 10, fromID, toID,
		-amount, fromBal, reason)
	_ = evedb.InsertJournalEntry(ctx, m.db, toID, 10, fromID, toID,
		amount, toBal, reason)
	return nil
}

// grantStarterLines hands a destitute pilot the free lines of the fit it is trying to fly and
// installs them, returning what is still missing. Docked only, and only into an empty slot;
// otherwise the ship flies short-fitted.
func (m *Mod) grantStarterLines(ctx context.Context, b *botRun, a *fitShip, missing []ShopItem) []ShopItem {
	if b.StationID == 0 || a == nil || a.ShipItemID == 0 {
		return missing
	}
	// The ship has to be in the hangar the pilot is standing in.
	var shipLoc int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`, a.ShipItemID).Scan(&shipLoc); err != nil ||
		shipLoc != b.StationID {
		return missing
	}

	var remaining []ShopItem
	for _, it := range missing {
		if it.Dest != "module" || !freeStarterModules[it.TypeID] {
			remaining = append(remaining, it)
			continue
		}
		var occupied int64
		if err := m.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entity WHERE locationID = ? AND flag = ?`,
			a.ShipItemID, it.Flag).Scan(&occupied); err != nil || occupied > 0 {
			remaining = append(remaining, it)
			continue
		}
		itemID, err := evedb.CreateItem(ctx, m.db, it.TypeID, b.CharID, b.StationID, 1)
		if err != nil {
			m.logBot(b.CharID, logError, "procure", "issuing a %s: %v", it.Name, err)
			remaining = append(remaining, it)
			continue
		}
		if err := evedb.FitModuleToShip(ctx, m.db, itemID, a.ShipItemID, it.Flag); err != nil {
			m.logBot(b.CharID, logError, "procure", "fitting the issued %s: %v", it.Name, err)
			remaining = append(remaining, it)
			continue
		}
		if err := evedb.SetModuleOnlineState(ctx, m.db, itemID, true); err != nil {
			m.logBot(b.CharID, logError, "procure", "onlining the issued %s: %v", it.Name, err)
		}
		m.logBot(b.CharID, logInfo, "procure",
			"too poor to buy one — issued a %s and fitted it, the way every pilot starts (entity %d)",
			it.Name, itemID)
	}
	return remaining
}

// brokeFitFallback is selectFitForActivity's answer for a pilot that cannot afford the cheapest fit
// in its pool: the rookie floor, whose hull is free and whose starter lines are issued. Nil when the
// pool holds no rookie fit for the job.
func brokeFitFallback(cands []*fitCandidate) *fitCandidate {
	for _, c := range cands {
		if c.judge.Unusable || c.judge.Score <= 0 || c.t == nil || c.t.Fit == nil {
			continue
		}
		if isRookieHull(c.t.Fit.HullTypeID) {
			return c
		}
	}
	return nil
}

// brokeShortFitNote is the log line for flying a fit with lines missing
// because the wallet is empty rather than because nobody sells them.
func brokeShortFitNote(missing []ShopItem) string {
	return fmt.Sprintf("broke — flying it short of %s rather than shopping with an empty wallet",
		shopSummary(missing))
}
