package rolebots

import (
	"context"
	"errors"

	evedb "gurimoon/db"
)

// The home loop: a pilot lives somewhere, and each stretch of solo work closes
// by flying home, taking stock, and only then choosing the next activity.
//
// The shopping half runs docked at home before the draw: the ammo stock-up
// (fitting.go ammoReloadsFor) plus hangar housekeeping. Buying is capped at the
// home and adjacent regions (shopClimb); the gap stands in the demand ledger.
//
// It does not outfit for the drawn activity (ensureActivityShip), touch a locked
// industry bill (industryResume) or claim gang members mid-op.

// homeShopCooldownSec is how long a completed shopping pass holds. It is not a
// cadence: a pilot returning from a trip shops anyway. It stops a pilot idling at
// home from re-running the pass every two minutes.
const homeShopCooldownSec = 1200

// homeShopBudgetFrac is the slice of the wallet one shopping pass may spend. The
// stock-up is discretionary, so it must never be why a pilot cannot afford its
// next activity.
const homeShopBudgetFrac = 0.25

// homeShopMaxLines bounds one pass's shopping list; the next visit picks up where
// this one stopped.
const homeShopMaxLines = 8

// goHomeAndShop is the seam in planSolo: with the loop on, a pilot between
// activities goes home first and shops before the next draw. Reports
// handled=true when it has taken the pass (travelling home, or entering the
// shopping phase); false hands the pass back to the draw.
func (m *Mod) goHomeAndShop(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	home, err := m.homeStation(ctx, b)
	if err != nil || home == 0 {
		// A pilot with no home cannot run a home loop. Not an error: the draw proceeds
		// and the clone machinery gives it an address later.
		return false, nil
	}
	if b.StationID == home {
		if clock.Unix() < st.Data.HomeShoppedAt+homeShopCooldownSec {
			return false, nil // recently shopped: on to the draw
		}
		st.Phase = string(TaskShopping)
		return true, saveState(ctx, m.db, st)
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, home)
	if err != nil {
		// A home the map cannot place is one the pilot cannot fly to: same as no home.
		return false, nil
	}
	err = m.beginTravel(ctx, b, loc.SolarSystemID, home, string(TaskShopping))
	if errors.Is(err, errNoRoute) {
		// Home is unreachable (an outlaw whose address is in empire, a sealed pocket):
		// the draw has the pass. Returning the error would halt the pilot for five minutes.
		m.logBot(b.CharID, logInfo, "home", "cannot get home from here (%v) — on to the next job", err)
		return false, nil
	}
	if err == nil {
		m.logBot(b.CharID, logDebug, "home", "heading home to take stock before the next job")
	}
	return true, err
}

// unloadOreCargo moves every ore stack in the docked ship's hold to the
// hangar. Between jobs at home there is no trip that ore belongs to.
func (m *Mod) unloadOreCargo(ctx context.Context, b *botRun) {
	if b.StationID == 0 || b.ShipID == 0 {
		return
	}
	lots, err := m.cargoOreLots(ctx, b, 0)
	if err != nil || len(lots) == 0 {
		return
	}
	var units int64
	for _, lot := range lots {
		if err := m.moveCargoToHangar(ctx, b, lot.TypeID); err != nil {
			rlogf("rolebots: unload ore cargo char=%d type=%d: %v", b.CharID, lot.TypeID, err)
			continue
		}
		units += lot.Units
	}
	if units > 0 {
		m.stackHangar(ctx, b.CharID, b.StationID)
		m.logBot(b.CharID, logInfo, "home", "unloaded %d units of ore from the hold into its hangar", units)
	}
}

// shoppingStep is the home.shop phase: docked at home, taking stock. Nothing
// worth buying goes straight to idle. A stock list hands over to the procure
// machinery as a soft trip flagged ShopStockRun, which changes only the ending
// (procureComplete and procureBreakOff stamp the visit instead of resuming).
func (m *Mod) shoppingStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State

	home, err := m.homeStation(ctx, b)
	if err != nil || home == 0 {
		return false, m.finishShopping(ctx, b, "it has no home to shop from")
	}
	// Not at home (a restart mid-phase, or home moved): resume the trip rather than
	// shopping in a stranger's hangar.
	if b.StationID != home {
		loc, lerr := evedb.GetStationLocation(ctx, m.db, home)
		if lerr != nil {
			return false, m.finishShopping(ctx, b, "its home station is off the map")
		}
		err = m.beginTravel(ctx, b, loc.SolarSystemID, home, string(TaskShopping))
		if errors.Is(err, errNoRoute) {
			return false, m.finishShopping(ctx, b, "it cannot get home from here")
		}
		return false, err
	}

	// A mining commander's haul comes first (the backstop for settleGangOp).
	// Step order matters. See docs/comment-notes-b11.md#home-shop-order.
	m.unloadOreCargo(ctx, b)
	if m.hasFleetOre(ctx, b.CharID, b.StationID) {
		m.disposeFleetOre(ctx, b)
	}

	// The meta run (metasell.go) goes before the strays sweep. A trip takes the
	// whole pass, so finishShopping is not called on that branch.
	if took, err := m.metaSellRun(ctx, b); err != nil || took {
		return false, err
	}

	// The warehouse run (warehouse.go): goods owned at stations the pilot is not
	// standing in. A trip, so it takes the pass.
	if took, err := m.warehouseRun(ctx, b); err != nil || took {
		return false, err
	}
	// The fleet-share fetch (fleetorefetch.go): a miner's handed-back rock waiting at
	// a staging station it does not live at.
	if took, err := m.fleetOreFetchRun(ctx, b); err != nil || took {
		return false, err
	}

	// Housekeeping earns money, so it comes before the spending below.
	m.sellHangarStrays(ctx, b)
	m.resellSurplusPrints(ctx, b)
	// The rock before the refined pile (orestrays.go, mineralstrays.go): the ore
	// sweep's first arm refines, so its minerals are on the floor for the next sweep.
	m.sellHangarOre(ctx, b)
	m.sellHangarMinerals(ctx, b)
	// Last resort (liquidate.go), before the spending so a destitute pilot raises
	// money first.
	m.liquidateForCash(ctx, b)

	// The runabout (shuttle.go): a pilot whose work involves blueprints keeps a
	// shuttle at home, bought at most once and outside the ammunition budget.
	m.stockRunabout(ctx, b)

	// A logistics hull for a pilot drawn to the fleet's mending seat (seatship.go).
	// A trip, so it takes the pass.
	if took, err := m.stockSeatShip(ctx, b); err != nil || took {
		return false, err
	}

	// The stock-up: the first candidate job with a live ship in this hangar takes the
	// pass; ships parked elsewhere belong to the outfit pass.
	for _, activity := range m.shoppingCandidates(ctx, b) {
		a, t, err := m.shipForActivity(ctx, b.CharID, activity)
		if err != nil || a == nil || t == nil || t.Fit == nil {
			continue
		}
		if b.ShipID != a.ShipItemID {
			var shipLoc int64
			if err := m.db.QueryRowContext(ctx,
				`SELECT locationID FROM entity WHERE itemID = ?`, a.ShipItemID).Scan(&shipLoc); err != nil ||
				shipLoc != b.StationID {
				continue
			}
			if err := m.boardShip(ctx, b, a.ShipItemID); err != nil {
				continue
			}
		}
		list, err := m.ammoCheck(ctx, b, a, t)
		if err != nil {
			return false, err
		}
		list = m.pruneUnsoldConsumables(ctx, b, list)
		list = m.clampShoppingBudget(ctx, b, list)
		if len(list) == 0 {
			continue
		}
		st.Activity = ""
		st.Data.ProcureFitID = t.FitID
		st.Data.ResumeActivity = ""
		st.Data.Shopping = list
		st.Data.FitLines = fitLines(t.Fit)
		st.Data.ProcureSoft = true
		st.Data.ShopStockRun = true
		st.Phase = "procure.outfit"
		m.logBot(b.CharID, logInfo, "home",
			"stocking up at home — buying %s for its %s ship", shopSummary(list), activity)
		return false, saveState(ctx, m.db, st)
	}

	// Last, with the hold stocked: surplus may go into the corporation's stock
	// (corpshares.go), and an ex-member sells a tranche of an old corporation's back.
	m.investInCorp(ctx, b)
	m.divestShares(ctx, b)
	// A review of its stock portfolio on the SCC Exchange (sccbots.go).
	m.sccBotReview(ctx, b)
	// A print the corporation's library lacks (corplibrary.go).
	m.donatePrintToCorp(ctx, b)
	// Whatever its corporation's resting bids at the factory are buying (corpdeep.go).
	m.sellInToCorpBids(ctx, b)
	// A lot of a deep metal its corporation bids for a few jumps away, carried there
	// (corpdeepcarry.go). The errand ends the pass.
	if started, err := m.startDeepCarry(ctx, b); started || err != nil {
		return false, err
	}
	// The corporation deputy's refinery chore (corpbuyback.go).
	m.corpRefineChore(ctx, b)

	return false, m.finishShopping(ctx, b, "")
}

// finishShopping stamps the visit and goes idle; the draw runs next pass, from home.
func (m *Mod) finishShopping(ctx context.Context, b *botRun, note string) error {
	st := b.State
	if note != "" {
		m.logBot(b.CharID, logInfo, "home", "shopping pass skipped — %s", note)
	}
	st.Activity, st.Phase = "", "idle"
	st.Data = idleData(st.Data)
	st.Data.HomeShoppedAt = clock.Unix()
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return saveState(ctx, m.db, st)
}

// shoppingCandidates is the jobs this pilot is likeliest to fly next, most likely
// first: the activity it just finished, then its appetite ranking. Kept short:
// the till serves the working ships.
func (m *Mod) shoppingCandidates(ctx context.Context, b *botRun) []string {
	var out []string
	seen := map[string]bool{}
	add := func(slug string) {
		if slug != "" && !seen[slug] {
			seen[slug] = true
			out = append(out, slug)
		}
	}
	add(b.State.Data.LastActivity)
	scores := m.scoreActivities(ctx, b)
	// Two best by score. The slice is small, so a full sort is not worth it.
	for range [2]struct{}{} {
		best, bestScore := "", 0.0
		for _, s := range scores {
			if !seen[s.Def.Slug] && s.Score > bestScore {
				best, bestScore = s.Def.Slug, s.Score
			}
		}
		add(best)
	}
	return out
}

// clampShoppingBudget trims a stock list to the pass's budget: a slice of the
// wallet and a line cap. Cheapest lines survive, since the pass wants depth
// across the hold, not one expensive stack.
func (m *Mod) clampShoppingBudget(ctx context.Context, b *botRun, list []ShopItem) []ShopItem {
	if len(list) == 0 {
		return list
	}
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return nil // a wallet that cannot be read buys nothing
	}
	budget := bal * homeShopBudgetFrac
	type priced struct {
		it   ShopItem
		cost float64
	}
	pl := make([]priced, 0, len(list))
	for _, it := range list {
		p, err := m.estimatePriceFor(ctx, b, it.TypeID)
		if err != nil || p <= 0 {
			p = 0 // unpriced is kept: the till re-check is the real gate
		}
		pl = append(pl, priced{it, p * float64(it.Qty)})
	}
	// Cheapest first, stable enough for a list this size.
	for i := 1; i < len(pl); i++ {
		for j := i; j > 0 && pl[j].cost < pl[j-1].cost; j-- {
			pl[j], pl[j-1] = pl[j-1], pl[j]
		}
	}
	var out []ShopItem
	spent := 0.0
	for _, p := range pl {
		if len(out) >= homeShopMaxLines || spent+p.cost > budget {
			break
		}
		out = append(out, p.it)
		spent += p.cost
	}
	return out
}
