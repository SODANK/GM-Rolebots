package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	evedb "gurimoon/db"
)

// Procurement: the shopping trip. When an activity has picked the fit it wants and no hull is built
// for it, the bot: (1) re-checks the fit against its skills (the selector already cut it to what the
// pilot can fit, so a red line here is a bug and the trip refuses to start); (2) if in a ship, flies
// home (profile home, else medical clone station) and leaves it; (3) flies to the nearest station
// selling the hull, buys, assembles and boards; (4) buys and fits everything sold there, then hops to
// the nearest station selling what is still missing; (5) resumes the activity.
// Anything the market cannot supply is not acquired: the trip abandons and procureAbandon's ladder
// takes over, with the miss booked to the unmet-demand ledger so the build book can answer it.
// "Nearest" means nearest anywhere (reach.go). A bot gives up only when nothing sells the item or its
// security limits wall it off from every seller; the log says which.

// startProcurement validates and kicks off the trip.
func (m *Mod) startProcurement(ctx context.Context, b *botRun, a *fitShip, t *fitTemplate, resumeActivity string) error {
	st := b.State

	chk, err := m.ValidateFitForChar(ctx, b.CharID, t.Fit)
	if err != nil {
		return err
	}
	if !chk.AllGreen {
		var reds []string
		for _, mc := range chk.Modules {
			if mc.Missing != "" {
				reds = append(reds, mc.TypeName+" (needs "+mc.Missing+")")
			}
		}
		if chk.OverGrid {
			reds = append(reds, "fit exceeds this pilot's CPU/PG")
		}
		return fmt.Errorf("fit %q has red lines for this pilot — refusing to buy: %s",
			t.Name, strings.Join(reds, "; "))
	}

	// Can it pay for what it is about to fly for? Wallets get spent between the plan and the trip, and
	// finding out at the till is expensive.
	st.Data.ProcureEscrowID, st.Data.ProcureEscrowStation, st.Data.ProcureClaimedHull = 0, 0, 0
	st.Data.ProcureEscrowTried = false
	cost, costErr := m.fitCost(ctx, b, t)
	if costErr == nil && cost > 0 && !t.HullOnly {
		if es := m.bestEscrowShip(ctx, b, t.Fit.HullTypeID, fitNeeds(t.Fit), cost,
			func() map[int64]int { return m.escrowShipReach(ctx, b) }); es != nil {
			cost = es.Outlay
			st.Data.ProcureEscrowID, st.Data.ProcureEscrowStation = es.EscrowID, es.StationID
			holdEscrowShip(es.EscrowID, b.CharID)
			m.logBot(b.CharID, logInfo, "procure",
				"the hull for %q is listed ready-fitted at %s (%d jumps) for %.0f ISK — %d unit(s) of the fit come in the crate, ~%.0f ISK all told",
				t.Name, m.stationLabel(ctx, es.StationID), es.Jumps, es.Price, es.Covered, es.Outlay)
		}
	}
	if costErr == nil && cost > 0 {
		bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
		if err != nil {
			return fmt.Errorf("wallet: %w", err)
		}
		// Against what the pilot may spend, the same measure outfit and findnext pay by: the raw balance
		// would let a trip start that the till refuses two stops later.
		if spendable(bal) < cost {
			releaseEscrowShip(st.Data.ProcureEscrowID)
			st.Data.ProcureEscrowID, st.Data.ProcureEscrowStation = 0, 0
			return &shortfallError{
				What: fmt.Sprintf("fit %q — not starting the trip", t.Name),
				Need: balanceNeededFor(cost), Have: bal,
			}
		}
	}

	// Shopping list.
	var list []ShopItem
	list = append(list, ShopItem{TypeID: t.Fit.HullTypeID, Name: t.Fit.HullName, Qty: 1, Dest: "hull"})
	for _, mod := range t.Fit.Modules {
		list = append(list, ShopItem{TypeID: mod.TypeID, Name: mod.TypeName, Qty: 1, Dest: "module", Flag: mod.Flag})
		if mod.ChargeTypeID != 0 && mod.ChargeQty > 0 {
			list = append(list, ShopItem{TypeID: mod.ChargeTypeID, Name: mod.ChargeName, Qty: mod.ChargeQty, Dest: "charge", Flag: mod.Flag})
		}
	}
	for _, c := range t.Fit.Cargo {
		list = append(list, ShopItem{TypeID: c.TypeID, Name: c.TypeName, Qty: c.Qty, Dest: "cargo"})
	}
	for _, d := range t.Fit.Drones {
		list = append(list, ShopItem{TypeID: d.TypeID, Name: d.TypeName, Qty: d.Qty, Dest: "drone"})
	}

	st.Activity = resumeActivity
	st.Data.ProcureFitID = t.FitID
	st.Data.ResumeActivity = resumeActivity
	st.Data.Shopping = procureOrder(list)
	st.Data.FitLines = fitLines(t.Fit)
	st.Data.ProcureSoft = false // a hull is never optional
	m.logBot(b.CharID, logInfo, "procure",
		"starting procurement of fit %q (%d items) for %s", t.Name, len(list), resumeActivity)

	// In a ship? Drop it off at home first.
	if b.ShipID != 0 {
		home, err := m.homeStation(ctx, b)
		if err != nil {
			return err
		}
		loc, err := evedb.GetStationLocation(ctx, m.db, home)
		if err != nil {
			return err
		}
		m.logBot(b.CharID, logInfo, "procure", "flying current ship home to station %d", home)
		return m.beginTravel(ctx, b, loc.SolarSystemID, home, "procure.dropship")
	}
	st.Phase = "procure.findhull"
	return saveState(ctx, m.db, st)
}

// pruneUnsoldConsumables drops consumable lines (charges, cargo, drones) that nothing the pilot's
// shopping can reach sells, before the trip rather than after it. Dropping a line sets the restock
// cooldown so the next plan does not re-add it. Hulls and modules stay: a hard line with no seller is
// a real dead end for procureAbandon. "Reach" is the buy cap's: the adjacent band (shopClimb) under the home loop, else the universe.
func (m *Mod) pruneUnsoldConsumables(ctx context.Context, b *botRun, list []ShopItem) []ShopItem {
	var ids []int64
	for _, it := range list {
		if it.Dest == "charge" || it.Dest == "cargo" || it.Dest == "drone" {
			ids = append(ids, it.TypeID)
		}
	}
	if len(ids) == 0 {
		return list
	}
	scopeJoin, args := "", int64Args(ids)
	if HomeLoop() {
		if scope := m.chargeShopScope(ctx, b.CharID); len(scope) > 0 {
			regions := make([]int64, 0, len(scope))
			for r := range scope {
				regions = append(regions, r)
			}
			scopeJoin = ` AND solarSystemID IN (
				SELECT solarSystemID FROM mapSolarSystems
				WHERE regionID IN (` + placeholders(len(regions)) + `))`
			args = append(args, int64Args(regions)...)
		}
	}
	sold := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT typeID FROM market_orders
		WHERE typeID IN (`+placeholders(len(ids))+`)
		  AND bid = 0 AND orderState = 1 AND volRemaining > 0`+scopeJoin, args...)
	if err != nil {
		return list // can't tell: let the trip find out
	}
	defer rows.Close()
	for rows.Next() {
		var t int64
		if rows.Scan(&t) == nil {
			sold[t] = true
		}
	}
	kept := list[:0:0]
	var dropped []ShopItem
	for _, it := range list {
		consumable := it.Dest == "charge" || it.Dest == "cargo" || it.Dest == "drone"
		if consumable && !sold[it.TypeID] {
			dropped = append(dropped, it)
			continue
		}
		kept = append(kept, it)
	}
	if len(dropped) > 0 {
		b.State.Data.RestockAfter = clock.Unix() + restockCooldownSec
		m.noteMarketDemand(ctx, b, dropped)
		m.logBot(b.CharID, logInfo, "procure",
			"nothing anywhere in the market sells %s — dropping it from the list instead of flying a doomed errand",
			shopSummary(dropped))
	}
	return kept
}

// procureCooldownSec is how long a fit sits out after a shopping trip for its hull or modules found no
// seller: long enough for the market to move, short enough that a restocked hub is noticed the same
// session. While it runs the selector will not propose buying the fit; a live ship of it still flies.
const procureCooldownSec = 3600

// noteFitProcureFail starts a fit's procurement cooldown, pruning expired
// entries as it goes so the map never grows past the fits actually failing.
func noteFitProcureFail(d *StateData, fitID int64) {
	if fitID == 0 {
		return
	}
	now := clock.Unix()
	if d.ProcureFailAt == nil {
		d.ProcureFailAt = map[int64]int64{}
	}
	for id, until := range d.ProcureFailAt {
		if until <= now {
			delete(d.ProcureFailAt, id)
		}
	}
	d.ProcureFailAt[fitID] = now + procureCooldownSec
}

// fitProcureCoolingDown reports whether a fit's last shopping trip recently dead-ended: the "do not
// re-fly this" gate.
func fitProcureCoolingDown(d *StateData, fitID int64) bool {
	return fitID != 0 && d.ProcureFailAt[fitID] > clock.Unix()
}

// isMoneyRefusal reports whether a failed buy was the wallet's doing (the
// order engine's MktNotEnoughCash refusal) rather than the market's. Pure.
func isMoneyRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), "enough ISK")
}

// noteMarketDemand records fit lines nothing in reach would sell. One row per type on the unmet-demand
// ledger a trading or production mechanism reads: a level, not a tally, throttled and decaying
// (demandledger.go). Best-effort: a bot mid-trip never fails over bookkeeping.
func (m *Mod) noteMarketDemand(ctx context.Context, b *botRun, items []ShopItem) {
	for _, it := range items {
		if err := m.noteDemandRow(ctx, b.CharID, b.SystemID, it.TypeID, it.Name, it.Qty); err != nil {
			m.logBot(b.CharID, logError, "procure", "demand ledger for %s: %v", it.Name, err)
			return
		}
	}
}

// noteDemandRow is one line on the unmet-demand ledger: at most once a day per (pilot, type), the stored
// want folded forward to now before the new quantity is added (demandledger.go), and held under the
// type's cap (demandCap). `wanted` is assigned before decayAt and lastAt because MariaDB evaluates the
// update left to right.
func (m *Mod) noteDemandRow(ctx context.Context, charID, systemID, typeID int64, name string, qty int64) error {
	now := clock.Unix()
	if !demandNoteDue(charID, typeID, now) {
		return nil
	}
	ceiling := int64(math.MaxInt64)
	if c := m.demandCap(ctx, typeID); c > 0 {
		ceiling = c
		qty = min(qty, c)
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_market_demand
			(typeID, typeName, wanted, asks, lastCharID, lastSystemID, lastRegionID, lastAt, decayAt)
		VALUES (?, ?, ?, 1, ?, ?,
		        COALESCE((SELECT s.regionID FROM mapSolarSystems s WHERE s.solarSystemID = ?), 0), ?, ?)
		ON DUPLICATE KEY UPDATE
			typeName = VALUES(typeName),
			wanted = LEAST(?, `+demandWantedSQL("", now)+` + VALUES(wanted)),
			asks = asks + 1,
			lastCharID = VALUES(lastCharID),
			lastSystemID = VALUES(lastSystemID),
			lastRegionID = IF(VALUES(lastRegionID) > 0, VALUES(lastRegionID), lastRegionID),
			lastAt = VALUES(lastAt),
			decayAt = VALUES(decayAt)`,
		typeID, name, qty, charID, systemID, systemID, now, now, ceiling)
	return err
}

// fittedPower scores what is left of a fit once the missing lines come off: the ship as it sits. Positive
// means the short-fitted ship still does the job (a miner with two of three lasers mines slower; one with
// none does not mine at all).
func (m *Mod) fittedPower(ctx context.Context, t *fitTemplate, missing []ShopItem) float64 {
	if t == nil || t.Fit == nil {
		return 0
	}
	gone := map[[2]int64]bool{}
	droneShort := map[int64]int64{}
	for _, it := range missing {
		switch it.Dest {
		case "module":
			gone[[2]int64{it.TypeID, it.Flag}] = true
		case "drone":
			droneShort[it.TypeID] += it.Qty
		}
	}
	pruned := *t.Fit
	pruned.Modules = nil
	for _, mod := range t.Fit.Modules {
		if !gone[[2]int64{mod.TypeID, mod.Flag}] {
			pruned.Modules = append(pruned.Modules, mod)
		}
	}
	// Drones too, by quantity: a fit that mines with drones and has none would score as if it had them.
	// A part shortfall still works a rock, so the line is thinned, not dropped.
	pruned.Drones = nil
	for _, d := range t.Fit.Drones {
		if left := d.Qty - droneShort[d.TypeID]; left > 0 {
			d.Qty = left
			pruned.Drones = append(pruned.Drones, d)
		}
	}
	// FitID stays 0: the power cache keys on fitID and this pruned copy is not the fit.
	p, err := m.fitPower(ctx, &fitTemplate{Fit: &pruned, Job: t.Job, HullTypeID: t.HullTypeID})
	if err != nil {
		return 0
	}
	return p
}

// resumeOutfit finishes a half-built ship: the hull exists and is the right
// one, but fit lines are still outstanding (an earlier trip ran out of
// sellers). The bot keeps the hull, boards it and goes shopping for what is
// missing instead of flying an empty ship out to work.
func (m *Mod) resumeOutfit(ctx context.Context, b *botRun, a *fitShip, t *fitTemplate,
	missing []ShopItem, resumeActivity string) error {
	st := b.State

	chk, err := m.ValidateFitForChar(ctx, b.CharID, t.Fit)
	if err != nil {
		return err
	}
	if !chk.AllGreen {
		return fmt.Errorf("fit %q has red lines for this pilot — refusing to buy the rest", t.Name)
	}

	// Anything unobtainable comes off the list here, before any flying. When that empties the list, the
	// ship is as complete as this market can make it: back to work (the cooldown stops a re-add).
	if missing = m.pruneUnsoldConsumables(ctx, b, missing); len(missing) == 0 {
		m.logBot(b.CharID, logInfo, "procure",
			"the only outstanding lines were unobtainable — flying the ship as it is")
		return saveState(ctx, m.db, st)
	}

	st.Activity = resumeActivity
	st.Data.ProcureFitID = t.FitID
	st.Data.ResumeActivity = resumeActivity
	st.Data.Shopping = missing
	st.Data.FitLines = fitLines(t.Fit)
	// A ship short of a gun is not the fit and the trip has to finish; a
	// ship short of bullets is running an errand and can come home empty.
	st.Data.ProcureSoft = shopIsConsumablesOnly(missing)
	m.logBot(b.CharID, logInfo, "procure",
		"ship %d is missing %d fit line(s) — resuming the shopping trip for %s",
		a.ShipItemID, len(missing), shopSummary(missing))

	// procure.outfit fits what it buys onto the BOARDED hull, so get in it.
	if b.ShipID == a.ShipItemID {
		if b.StationID != 0 {
			st.Phase = "procure.outfit"
		} else {
			st.Phase = "procure.findnext" // in space: travel to a seller first
		}
		return saveState(ctx, m.db, st)
	}
	var shipLoc int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`, a.ShipItemID).Scan(&shipLoc); err != nil {
		return fmt.Errorf("ship %d location: %w", a.ShipItemID, err)
	}
	if shipLoc == b.StationID && b.StationID != 0 {
		if err := m.boardShip(ctx, b, a.ShipItemID); err != nil {
			return err
		}
		st.Phase = "procure.outfit"
		return saveState(ctx, m.db, st)
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, shipLoc)
	if err != nil {
		return fmt.Errorf("its ship %d is not in a reachable station: %w", a.ShipItemID, err)
	}
	return m.beginTravel(ctx, b, loc.SolarSystemID, shipLoc, "procure.board")
}

// startRestock sends the bot shopping for ammo. It is an outfit trip with one difference: the ship is
// already the fit and already works, so the trip is soft and strands nothing. No seller in reach means
// the bot goes back to work with a light hold and tries again next plan.
func (m *Mod) startRestock(ctx context.Context, b *botRun, a *fitShip, t *fitTemplate,
	restock []ShopItem, resumeActivity string) error {
	st := b.State

	// Ammo nobody sells is not worth a trip, and one query settles it. An emptied list means everything
	// wanted is unobtainable: the cooldown is set, go work with what is in the guns.
	if restock = m.pruneUnsoldConsumables(ctx, b, restock); len(restock) == 0 {
		return saveState(ctx, m.db, st)
	}

	st.Activity = resumeActivity
	st.Data.ProcureFitID = t.FitID
	st.Data.ResumeActivity = resumeActivity
	st.Data.Shopping = restock
	st.Data.FitLines = fitLines(t.Fit)
	st.Data.ProcureSoft = true
	m.logBot(b.CharID, logInfo, "procure",
		"low on ammo — buying %s for the %s run", shopSummary(restock), resumeActivity)

	// The bot must be in the ship being restocked: procure.outfit loads what
	// it buys into the BOARDED hull.
	if b.ShipID == a.ShipItemID {
		if b.StationID != 0 {
			st.Phase = "procure.outfit"
		} else {
			st.Phase = "procure.findnext"
		}
		return saveState(ctx, m.db, st)
	}
	var shipLoc int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`, a.ShipItemID).Scan(&shipLoc); err != nil {
		return fmt.Errorf("ship %d location: %w", a.ShipItemID, err)
	}
	if shipLoc == b.StationID && b.StationID != 0 {
		if err := m.boardShip(ctx, b, a.ShipItemID); err != nil {
			return err
		}
		st.Phase = "procure.outfit"
		return saveState(ctx, m.db, st)
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, shipLoc)
	if err != nil {
		return fmt.Errorf("its ship %d is not in a reachable station: %w", a.ShipItemID, err)
	}
	return m.beginTravel(ctx, b, loc.SolarSystemID, shipLoc, "procure.board")
}

// procureStep advances the procure.* phases.
func (m *Mod) procureStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	switch st.Phase {

	case "procure.dropship":
		// Docked at home: step out of the ship (into the capsule).
		if b.StationID == 0 {
			return false, fmt.Errorf("procure.dropship while not docked")
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET locationID = ? WHERE itemID = ?`, b.StationID, b.CharID); err != nil {
			return false, err
		}
		m.maybeEscrowSurplusShip(ctx, b, b.ShipID)
		b.ShipID = 0
		m.logBot(b.CharID, logInfo, "procure", "left ship at home station %d, proceeding in capsule", b.StationID)
		st.Phase = "procure.findhull"
		return false, saveState(ctx, m.db, st)

	case "procure.board":
		// Arrived at the station holding a half-built ship: get in it and carry on shopping for the missing lines.
		if b.StationID == 0 {
			return false, fmt.Errorf("procure.board while not docked")
		}
		var shipID int64
		err := m.db.QueryRowContext(ctx, `
			SELECT shipItemID FROM rolebot_fit_ships
			WHERE characterID = ? AND fitID = ?`, b.CharID, st.Data.ProcureFitID).Scan(&shipID)
		// The hull this trip flew back for can be gone by the time it lands (pod.go deletes the row with the
		// ship). That is a dead end like any other: abandon and re-plan, not a hard error.
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return false, m.procureAbandon(ctx, b,
				"the hull it flew back for is not there any more")
		case err != nil:
			return false, fmt.Errorf("procure.board ship row: %w", err)
		}
		if shipID == 0 {
			return false, m.procureAbandon(ctx, b, "no hull is recorded against this fit")
		}
		if err := m.boardShip(ctx, b, shipID); err != nil {
			return false, err
		}
		st.Phase = "procure.outfit"
		return false, saveState(ctx, m.db, st)

	case "procure.findhull":
		hull := shoppingFirst(st.Data.Shopping, "hull")
		if hull == nil {
			st.Phase = "procure.outfit"
			return false, saveState(ctx, m.db, st)
		}
		// Already owns one, packaged, in some hangar (a filled hauler bid delivers exactly that, and so does a
		// purchase whose run died before boarding): go and take it; buyhull's resume guard adopts it on arrival.
		if at := m.ownedHullElsewhere(ctx, b.CharID, hull.TypeID); at != 0 {
			// A hull it already owns beats one it would pay for.
			releaseEscrowShip(st.Data.ProcureEscrowID)
			st.Data.ProcureEscrowID, st.Data.ProcureEscrowStation = 0, 0
			if at == b.StationID {
				st.Phase = "procure.buyhull"
				return false, saveState(ctx, m.db, st)
			}
			if loc, lerr := evedb.GetStationLocation(ctx, m.db, at); lerr == nil {
				m.logBot(b.CharID, logInfo, "procure",
					"it already owns a %s at %s — flying over to take it instead of buying one",
					hull.Name, m.stationLabel(ctx, at))
				return false, m.beginTravel(ctx, b, loc.SolarSystemID, at, "procure.buyhull")
			}
		}
		// Then a listed ship (escrow_ship.go): the one the trip started for, or any listed in reach. Once per
		// trip: the shared board lags a claim that fell through and would offer the same crate back.
		if st.Data.ProcureEscrowID == 0 && !st.Data.ProcureEscrowTried && b.StationID != 0 {
			if es := m.bestEscrowShip(ctx, b, hull.TypeID, shopNeeds(st.Data.Shopping),
				m.shoppingMarketCost(ctx, b, st.Data.Shopping),
				func() map[int64]int { return m.escrowShipReach(ctx, b) }); es != nil {
				st.Data.ProcureEscrowID, st.Data.ProcureEscrowStation = es.EscrowID, es.StationID
				holdEscrowShip(es.EscrowID, b.CharID)
				m.logBot(b.CharID, logInfo, "procure",
					"a %s is listed ready-fitted at %s (%d jumps) for %.0f ISK — %d unit(s) of the fit come in the crate",
					hull.Name, m.stationLabel(ctx, es.StationID), es.Jumps, es.Price, es.Covered)
			}
		}
		if st.Data.ProcureEscrowID != 0 {
			if b.StationID == st.Data.ProcureEscrowStation {
				st.Phase = "procure.escrowship"
				return false, saveState(ctx, m.db, st)
			}
			if loc, lerr := evedb.GetStationLocation(ctx, m.db, st.Data.ProcureEscrowStation); lerr == nil {
				return false, m.beginTravel(ctx, b, loc.SolarSystemID, st.Data.ProcureEscrowStation, "procure.escrowship")
			}
			releaseEscrowShip(st.Data.ProcureEscrowID)
			st.Data.ProcureEscrowID, st.Data.ProcureEscrowStation = 0, 0
		}
		prefer := m.homeRegionSet(ctx, b)
		// It shops at home first and only widens when its own region has no hull for it (residency.go); within
		// a rung it buys from the disliked only when nobody friendlier stocks the hull, and never from the despised.
		var pick *marketStop
		look, err := m.shopLadder(ctx, b, []int64{hull.TypeID}, true, prefer, defaultBand(b),
			fmt.Sprintf("every seller of %s it can reach belongs to a faction it dislikes — "+
				"buying anyway, but never from the despised", hull.Name),
			func(scan marketScan) bool {
				pick = bestBuy(scan.Stops, b.Tune)
				return pick != nil
			})
		if err != nil {
			return false, err
		}
		scan := look.Scan
		if pick == nil {
			// Ledger first: an unbuyable hull is unmet demand that the build book answers. Nothing is created for it.
			m.noteMarketDemand(ctx, b, []ShopItem{*hull})
			return false, m.procureAbandon(ctx, b, m.noSellerReason(ctx, scan, "sells the hull "+hull.Name))
		}
		m.logBot(b.CharID, logInfo, "procure", "hull %s on sale at %.2f ISK, station %d in %s — traveling",
			hull.Name, pick.Price, pick.StationID, m.travelNote(ctx, b, pick.SystemID, pick.Jumps))
		return false, m.beginTravel(ctx, b, pick.SystemID, pick.StationID, "procure.buyhull")

	case "procure.escrowship":
		// Docked beside the listing it flew for. Taken or unaffordable is not a dead end, so either way it goes
		// back to findhull, which with the listing cleared shops the till.
		hull := shoppingFirst(st.Data.Shopping, "hull")
		escrowID := st.Data.ProcureEscrowID
		st.Data.ProcureEscrowID, st.Data.ProcureEscrowStation = 0, 0
		st.Data.ProcureEscrowTried = true
		if hull == nil || escrowID == 0 || b.StationID == 0 {
			releaseEscrowShip(escrowID)
			st.Phase = "procure.findhull"
			return false, saveState(ctx, m.db, st)
		}
		shipID, paid, err := m.claimEscrowShip(ctx, b, escrowID, hull.TypeID)
		if err != nil {
			m.logBot(b.CharID, logInfo, "procure",
				"the listed %s it came for is not to be had (%v) — shopping the market for the hull instead", hull.Name, err)
			st.Phase = "procure.findhull"
			return false, saveState(ctx, m.db, st)
		}
		st.Data.ProcureClaimedHull = shipID
		m.logBot(b.CharID, logInfo, "procure",
			"bought a ready-fitted %s off escrow for %.0f ISK at %s — its rack is in the hangar for the outfit",
			hull.Name, paid, m.stationLabel(ctx, b.StationID))
		st.Phase = "procure.buyhull"
		return false, saveState(ctx, m.db, st)

	case "procure.buyhull":
		hull := shoppingFirst(st.Data.Shopping, "hull")
		if hull == nil {
			st.Phase = "procure.outfit"
			return false, saveState(ctx, m.db, st)
		}
		// Resume guard, before the purchase: a kill between the four unwrapped writes (buy, assemble, board,
		// assign) would re-buy the hull. See docs/comment-notes-b15.md#procure-resume-guard.
		hullID, spent, how := int64(0), 0.0, "market"
		// The hull procure.escrowship just claimed, by ID: ownedHullFor would
		// prefer a packaged one of the same type if the hangar held both.
		claimed := st.Data.ProcureClaimedHull
		st.Data.ProcureClaimedHull = 0
		if claimed != 0 && m.hullHereFor(ctx, b, claimed, hull.TypeID) {
			hullID, how = claimed, "escrow"
		} else if owned := m.ownedHullFor(ctx, b, hull.TypeID); owned != 0 {
			hullID, how = owned, hullFromHand
			m.logBot(b.CharID, logInfo, "procure",
				"%s (entity %d) is already in hand here — taking that one instead of buying, "+
					"which is what a shutdown mid-purchase leaves behind", hull.Name, owned)
		} else {
			// Market only. Quantity 1: a part load cannot happen, so the filled count is dropped.
			var err error
			hullID, _, spent, err = m.buyAtStation(ctx, b.CharID, b.StationID, hull.TypeID, 1, "hull")
			if err == nil || !isMoneyRefusal(err) {
				line := shopLine{TypeID: hull.TypeID, Asked: 1}
				if err == nil {
					line.Filled = 1
				}
				m.noteShopStop(ctx, b.StationID, []shopLine{line})
			}
			if err != nil {
				return false, m.procureAbandon(ctx, b, "hull purchase failed: "+err.Error())
			}
		}
		// One hull off the row: a bought hull merges into a packaged stack of
		// its type, and an owned one may be the factory's stock (hullassemble.go).
		hullID, err := m.assembleOneHull(ctx, hullID)
		if err != nil {
			return false, err
		}
		if err := m.boardShip(ctx, b, hullID); err != nil {
			return false, err
		}
		if err := setFitShip(ctx, m.db, b.CharID, st.Data.ProcureFitID, hullID); err != nil {
			return false, err
		}
		if how != "market" {
			m.logBot(b.CharID, logInfo, "procure", "%s assembled and boarded (entity %d, %s)",
				hull.Name, hullID, how)
		} else {
			m.logBot(b.CharID, logInfo, "procure", "bought %s (%s, %.2f ISK), assembled and boarded (entity %d)",
				hull.Name, how, spent, hullID)
		}
		st.Data.Shopping = shoppingRemove(st.Data.Shopping, hull)
		st.Phase = "procure.outfit"
		return false, saveState(ctx, m.db, st)

	case "procure.outfit":
		if b.ShipID == 0 || b.StationID == 0 {
			return false, fmt.Errorf("procure.outfit needs a boarded ship in a station")
		}
		// A refit is a ship that already carries modules; a new hull carries none. The difference decides what
		// comes off first and whether the list must be affordable whole (below).
		refit := m.shipHasFittedModules(ctx, b.ShipID)
		// Leftovers of an earlier fit come off before anything new goes on (the template's grid budget assumed
		// an empty hull), except those a pending line replaces: they stay until applyShopItem's clearSlot swaps them.
		if err := m.stripOffFitModules(ctx, b, pendingSlotFlags(st.Data.Shopping)); err != nil {
			return false, err
		}
		// Atomic fit: a new hull's whole list must be affordable before any line is bought; a refit is exempt.
		// See docs/comment-notes-b15.md#procure-atomic-fit.
		if !refit {
			if err := m.procureAffordable(ctx, b); err != nil {
				return false, m.procureAbandonMoney(ctx, b, err)
			}
		}
		// The live balance, carried down the loop. The check above is priced on estimates and this one on
		// receipts; an estimate that drifts low would otherwise walk the wallet past the floor.
		bal, balErr := evedb.GetCharBalance(ctx, m.db, b.CharID)
		if balErr != nil {
			return false, fmt.Errorf("procure.outfit wallet: %w", balErr)
		}
		var remaining []ShopItem
		var refused []string
		// Lines the till itself failed (no seller, nothing filled, an order refused for a reason other than
		// money). The stall below writes these to the demand ledger.
		var unsupplied []ShopItem
		// Whether anything was bought at this station. The anti-livelock guard below cannot infer it from the
		// list length: a part load re-queues its remainder, so half a magazine is progress.
		progress := false
		// The rack before the rounds: each rack line keeps back what the later rack lines cost, and reload
		// stacks keep back what every unbought rack line costs. The rounds in the guns are exempt.
		shopping := st.Data.Shopping
		// Lines crafted stock a few jumps off beats the shelf here for: left on
		// the list for findnext, which will send the pilot there (craftedpick.go).
		deferHere := m.craftedDeferLines(ctx, b, shopping)
		var deferred []string
		est := make([]float64, len(shopping))
		for i, it := range shopping {
			if isRackLine(it) {
				if p, perr := m.estimatePriceFor(ctx, b, it.TypeID); perr == nil && p > 0 {
					est[i] = p * float64(it.Qty)
				}
			}
		}
		rackAhead := make([]float64, len(shopping)+1)
		for i := len(shopping) - 1; i >= 0; i-- {
			rackAhead[i] = rackAhead[i+1] + est[i]
		}
		// Teeth first (procureTeethFirst): on a ship with no gun, a weapon or drone line keeps back only what
		// the teeth after it cost, so a wallet short of the whole rack still arms the ship.
		isWeapon := m.weaponLine(ctx)
		toothless := refit && m.shipHasNoTeeth(ctx, b.ShipID)
		teethAhead := make([]float64, len(shopping)+1)
		for i := len(shopping) - 1; i >= 0; i-- {
			teethAhead[i] = teethAhead[i+1]
			if isTeethLine(shopping[i], isWeapon) {
				teethAhead[i] += est[i]
			}
		}
		rackOwed := 0.0 // rack lines passed over this visit; they still need paying for
		// What this counter was asked for and gave, for the shopper-demand ledger (stationdemand.go). Only
		// lines that reached the till: the pilot's own stock, a deferral and a wallet refusal say nothing here.
		var stop []shopLine
		for i, it := range shopping {
			// The pilot's own hangar first: it is here and paid for, and buying another beside it is how hangars
			// filled (ownstock.go). What it covers comes off the line.
			if it.Dest != "hull" {
				if stackID, have := m.ownStockStack(ctx, b.CharID, b.StationID, it.TypeID); stackID != 0 {
					part := it
					if have < part.Qty {
						part.Qty = have
					}
					if err := m.applyShopItem(ctx, b, part, stackID); err != nil {
						m.logBot(b.CharID, logError, "procure", "%s from its own hangar: %v", it.Name, err)
					} else {
						progress = true
						m.logBot(b.CharID, logInfo, "procure",
							"fitted %d× %s from its own hangar", part.Qty, it.Name)
						if part.Qty >= it.Qty {
							continue
						}
						it.Qty -= part.Qty
						if est[i] > 0 {
							est[i] = est[i] * float64(it.Qty) / float64(it.Qty+part.Qty)
						}
					}
				}
			}
			if deferHere[it.TypeID] {
				deferred = append(deferred, it.Name)
				remaining = append(remaining, it)
				rackOwed += est[i]
				continue
			}
			// The floor stops the loop, not the wallet. What is left goes back on the list and is bought on the
			// proceeds of the work this pilot can still afford.
			if spendable(bal) <= 0 {
				refused = append(refused, fmt.Sprintf("%s: wallet down to its floor", it.Name))
				remaining = append(remaining, it)
				rackOwed += est[i]
				continue
			}
			keep := 0.0
			switch {
			case toothless && isRackLine(it) && isTeethLine(it, isWeapon):
				keep = teethAhead[i+1]
			case refit && isRackLine(it):
				keep = rackAhead[i+1]
			case it.Dest == "cargo":
				keep = rackOwed + rackAhead[i+1]
			}
			// A refit skipped the whole-list gate, so each line answers to the floor on its own: one it cannot
			// cover waits, and the cheaper lines behind it are still bought.
			if refit || keep > 0 {
				if p, perr := m.estimatePriceFor(ctx, b, it.TypeID); perr == nil && p > 0 &&
					p*float64(it.Qty) > spendable(bal)-keep {
					if keep > 0 {
						refused = append(refused, fmt.Sprintf("%s: ~%.0f ISK, keeping ~%.0f back for the rest of the rack",
							it.Name, p*float64(it.Qty), keep))
					} else {
						refused = append(refused, fmt.Sprintf("%s: ~%.0f ISK is over what the wallet may commit",
							it.Name, p*float64(it.Qty)))
					}
					remaining = append(remaining, it)
					rackOwed += est[i]
					continue
				}
			}
			// Market only, as in procure.buyhull.
			itemID, filled, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID, it.TypeID, it.Qty, "fitting")
			bal -= spent
			switch {
			case err == nil:
				stop = append(stop, shopLine{TypeID: it.TypeID, Asked: it.Qty, Filled: filled})
			case !isMoneyRefusal(err):
				stop = append(stop, shopLine{TypeID: it.TypeID, Asked: it.Qty})
			}
			how := "market"
			if err != nil {
				// The refusal cause is kept: "not sold here" is one of several (an empty wallet and an order-engine
				// refusal are the others), and a pilot looping on this station would otherwise look like one shopping.
				refused = append(refused, fmt.Sprintf("%s: %v", it.Name, err))
				remaining = append(remaining, it)
				if !isMoneyRefusal(err) {
					unsupplied = append(unsupplied, it)
				}
				// The rest of the roster is told, so it stops being sent here for this type while the book is empty
				// (emptytill.go). Only this place knows what a till actually did, versus the shared market snapshot.
				if errors.Is(err, errNoSellerHere) {
					noteTillEmpty(botScope(b), it.TypeID, b.StationID)
				}
				rackOwed += est[i]
				continue
			}
			bought := it
			if filled < bought.Qty {
				bought.Qty = filled // never load rounds that were not delivered
			}
			if err := m.applyShopItem(ctx, b, bought, itemID); err != nil {
				m.logBot(b.CharID, logError, "procure", "%s: apply failed: %v", it.Name, err)
				remaining = append(remaining, it)
				continue
			}
			progress = true
			// A short fill is fitted and the rest goes back on the list. Half a magazine beats none; hull and
			// module lines are quantity 1, so this only bites on charges.
			if filled < it.Qty {
				short := it
				short.Qty = it.Qty - filled
				remaining = append(remaining, short)
				m.logBot(b.CharID, logInfo, "procure", "bought + installed %d of %d× %s (%s, %.2f ISK) — %d still wanted",
					filled, it.Qty, it.Name, how, spent, short.Qty)
				continue
			}
			m.logBot(b.CharID, logInfo, "procure", "bought + installed %d× %s (%s, %.2f ISK)",
				it.Qty, it.Name, how, spent)
		}
		m.noteShopStop(ctx, b.StationID, stop)
		if len(deferred) > 0 {
			m.logBot(b.CharID, logInfo, "procure",
				"passed over the NPC shelf here for %s — crafted stock nearby is cheaper", strings.Join(deferred, ", "))
		}
		if len(remaining) == 0 {
			st.Data.Shopping = remaining
			return false, m.procureComplete(ctx, b)
		}
		// Incumbents were kept on the slots still pending. That is only sound while the ship is inside its
		// own grid: the new lines were budgeted against an empty slot. Over budget, the kept ones come off.
		m.shedIncumbentsIfOverGrid(ctx, b, pendingSlotFlags(remaining))

		// Anti-livelock guard: if standing here bought nothing and the last attempt was also here and bought
		// nothing, the trip abandons. See docs/comment-notes-b15.md#procure-anti-livelock.
		if !progress && len(remaining) == len(st.Data.Shopping) && st.Data.ProcureStuckAt == b.StationID {
			st.Data.Shopping = remaining
			why := "nothing on the list can be bought here after two attempts"
			if len(refused) > 0 {
				why = "nothing on the list can be bought here: " + strings.Join(refused, "; ")
			}
			// The one abandon that is a genuine supply failure: the pilot stood at the counter twice and could buy
			// none of it. The ledger is what tells an industrialist.
			m.noteMarketDemand(ctx, b, unsupplied)
			return false, m.procureAbandon(ctx, b, why)
		}
		if !progress {
			st.Data.ProcureStuckAt = b.StationID
			if len(refused) > 0 {
				m.logBot(b.CharID, logInfo, "procure",
					"bought nothing at %s — %s", m.stationLabel(ctx, b.StationID), strings.Join(refused, "; "))
			}
		} else {
			// Progress. Any future stall starts its two strikes over.
			st.Data.ProcureStuckAt = 0
		}
		st.Data.Shopping = remaining
		st.Phase = "procure.findnext"
		return false, saveState(ctx, m.db, st)

	case "procure.findnext":
		// Can it pay? Asked before the stop is picked, so a pilot that cannot afford its list does not loop
		// between the scan and a till that refuses it. See docs/comment-notes-b15.md#procure-affordable.
		if err := m.procureAffordable(ctx, b); err != nil && !m.procureTeethFirst(ctx, b) {
			return false, m.procureAbandonMoney(ctx, b, err)
		}
		prefer := m.homeRegionSet(ctx, b)
		typeIDs := make([]int64, 0, len(st.Data.Shopping))
		// Quantities too, so a station is weighed on what it can hand over, not on how many of the list's
		// names it lists. Modules and hulls are quantity 1; a charge line is where a thin book reads as full coverage.
		want := make(map[int64]int64, len(st.Data.Shopping))
		for _, it := range st.Data.Shopping {
			typeIDs = append(typeIDs, it.TypeID)
			want[it.TypeID] += it.Qty
		}
		// One query per rung, every station in the pilot's economic scope. The next stop clears the most of the
		// list for the least flying; a region with nothing left to sell widens the search instead of stranding the trip.
		var next *supplyStop
		// A soft run (a top-up, a home stock run) is capped at shopSoftMaxJumps real gates, because the stop was
		// ranked on the router's cost, which counts a lowsec system as several. A fit or a hull keeps the full reach.
		soft := st.Data.ProcureSoft || st.Data.ShopStockRun
		tooFar := false
		ownStops := m.ownStockStops(ctx, b, st.Data.Shopping)
		look, err := m.shopLadder(ctx, b, typeIDs, true, prefer, defaultBand(b),
			"the remaining fit items are only sold by factions it dislikes — buying anyway, but never from the despised",
			func(scan marketScan) bool {
				// An NPC shelf that crafted stock beats, once the flying and craftedEdge are priced in, is not a stop for
				// that line (craftedpick.go).
				scan.Stops = dropCraftedDominated(scan.Stops, want, b.Tune)
				// Its own hangars stock the list too, at no price (ownstock.go).
				if len(ownStops) > 0 {
					scan.Stops = append(append([]marketStop(nil), scan.Stops...), ownStops...)
				}
				if !soft {
					next = bestSupplyStopCapped(scan.Stops, want, m.shopCaps(ctx, scan.Stops, want, false), b.Tune)
					return next != nil
				}
				next = m.softSupplyStop(ctx, b, scan.Stops, want)
				if next == nil && len(scan.Stops) > 0 {
					tooFar = true
				}
				return next != nil
			})
		if err != nil {
			return false, err
		}
		scan := look.Scan
		if next == nil && tooFar {
			// Somebody sells it, just not within a top-up's range. That is not unmet demand for the build book, so
			// the ledger is left alone.
			return false, m.procureAbandon(ctx, b, fmt.Sprintf(
				"nothing within %d jumps sells the remaining items (%s) — a top-up is not worth a longer trip",
				shopSoftMaxJumps, shopSummary(st.Data.Shopping)))
		}
		if next == nil {
			// Ledger first: an unbuyable remainder is unmet demand for the build book. The abandon ladder below (fit
			// cooldown, consumables-only, short-fitted, an owned hull) answers a region that sells no Bantams.
			m.noteMarketDemand(ctx, b, st.Data.Shopping)
			return false, m.procureAbandon(ctx, b,
				m.noSellerReason(ctx, scan, "sells the remaining items ("+shopSummary(st.Data.Shopping)+")"))
		}
		m.logBot(b.CharID, logInfo, "procure",
			"%d item(s) still missing — next stop station %d in %s, which stocks %.1f of them",
			len(st.Data.Shopping), next.StationID,
			m.travelNote(ctx, b, next.SystemID, next.Jumps), next.Covered)
		return false, m.beginTravel(ctx, b, next.SystemID, next.StationID, "procure.outfit")
	}
	return false, fmt.Errorf("unknown procure phase %q", st.Phase)
}

// shopSoftMaxJumps is the furthest a soft shopping run flies, in real gates.
const shopSoftMaxJumps = 10

// softSupplyStop is bestSupplyStopFor for a soft run: only stops the pilot can reach in shopSoftMaxJumps
// gates on the route it would fly. A BFS over the gate graph drops stops whose shortest path is already too
// long; the survivors are routed (router.go) and a stop whose real route runs long is dropped and the
// next best tried.
func (m *Mod) softSupplyStop(ctx context.Context, b *botRun, stops []marketStop, want map[int64]int64) *supplyStop {
	u, err := m.universe(ctx)
	if err != nil {
		return bestSupplyStopCapped(stops, want, m.shopCaps(ctx, stops, want, true), b.Tune)
	}
	near := gangReachMap(u, b.SystemID, shopSoftMaxJumps)
	pool := make([]marketStop, 0, len(stops))
	for _, st := range stops {
		if _, ok := near[st.SystemID]; ok {
			pool = append(pool, st)
		}
	}
	rules := rulesForBot(b)
	caps := m.shopCaps(ctx, stops, want, true)
	for tries := 0; tries < 6 && len(pool) > 0; tries++ {
		next := bestSupplyStopCapped(pool, want, caps, b.Tune)
		if next == nil {
			return nil
		}
		if next.SystemID == b.SystemID {
			return next
		}
		if path, rerr := m.route(ctx, b.SystemID, next.SystemID, rules); rerr == nil && len(path) <= shopSoftMaxJumps {
			return next
		}
		kept := pool[:0]
		for _, st := range pool {
			if st.StationID != next.StationID {
				kept = append(kept, st)
			}
		}
		pool = kept
	}
	return nil
}

// applyShopItem installs one acquired item onto the boarded ship.
func (m *Mod) applyShopItem(ctx context.Context, b *botRun, it ShopItem, itemID int64) error {
	// Exactly the units this line is for, never the pile they landed in (see takeUnits).
	if it.Dest != "hull" {
		id, got, err := m.takeUnits(ctx, itemID, it.Qty)
		if err != nil {
			return err
		}
		if got <= 0 {
			return fmt.Errorf("item %d holds nothing to apply", itemID)
		}
		itemID, it.Qty = id, got
	}
	switch it.Dest {
	case "module":
		// The slot may already hold something, usually because the fit was edited after the ship was built.
		// Two entities sharing one slot flag is a broken ship, so the occupant goes back to the hangar first.
		if err := m.clearSlot(ctx, b, it.Flag); err != nil {
			return err
		}
		if err := evedb.FitModuleToShip(ctx, m.db, itemID, b.ShipID, it.Flag); err != nil {
			return err
		}
		return evedb.SetModuleOnlineState(ctx, m.db, itemID, true)
	case "charge":
		// Topping a gun up is the normal case: rounds are added to the partial stack already in the slot and the
		// bought row goes away. If the slot holds another module, the rounds go in the hold (fitslotcharge.go).
		if occ, err := evedb.FindModuleInSlot(ctx, m.db, b.ShipID, it.Flag); err != nil {
			return err
		} else if occ == nil || !m.chargeAccepts(ctx, it.TypeID, occ.TypeID) {
			_, err := m.stowToCargo(ctx, b, itemID, it.TypeID, it.Qty)
			return err
		}
		lc, err := evedb.GetLoadedCharge(ctx, m.db, b.ShipID, it.Flag)
		if err != nil {
			return err
		}
		var loaded int64
		if lc != nil {
			if lc.TypeID == it.TypeID {
				loaded = lc.Quantity
			} else {
				// A different charge is loaded: the fit changed under the ship. It goes to the hold, not destroyed.
				if err := evedb.UnloadChargeToCargoOrHangar(ctx, m.db, lc.ItemID, b.ShipID, 5); err != nil {
					return err
				}
				lc = nil
			}
		}
		// The gun takes a full load and no more (one crystal in a laser, one magazine in a gun). A durable line
		// buys a spare and a generated fit buys 200 rounds whatever the gun holds; the rest rides in the hold.
		room, err := m.slotChargeRoom(ctx, b.ShipID, it.Flag, it.TypeID, loaded)
		if err != nil {
			return err
		}
		load := it.Qty
		if load > room {
			load = room
		}
		if spill := it.Qty - load; spill > 0 {
			if load == 0 {
				_, err := m.stowToCargo(ctx, b, itemID, it.TypeID, spill)
				return err
			}
			// The spill comes off the bought row first, so what the hold cannot take stays in the hangar.
			spillID, err := evedb.SplitStackN(ctx, m.db, itemID, spill)
			if err != nil {
				return err
			}
			if _, err := m.stowToCargo(ctx, b, spillID, it.TypeID, spill); err != nil {
				return err
			}
		}
		if lc != nil {
			if err := evedb.LoadChargeIntoModule(ctx, m.db, lc.ItemID, b.ShipID, it.Flag, loaded+load); err != nil {
				return err
			}
			_, err := m.db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, itemID)
			return err
		}
		return evedb.LoadChargeIntoModule(ctx, m.db, itemID, b.ShipID, it.Flag, load)
	case "cargo":
		_, err := m.stowToCargo(ctx, b, itemID, it.TypeID, it.Qty)
		return err
	case "drone":
		return evedb.MoveStackToDroneBay(ctx, m.db, itemID, b.ShipID)
	case "implant":
		// Into the pilot, not the ship: the core's own rule (slot conflict,
		// skills) and its training retime (implantupkeep.go).
		return m.plugImplant(ctx, b, itemID, "bought on a shopping list")
	}
	return fmt.Errorf("unknown shop destination %q", it.Dest)
}

// stowToCargo moves as much of the qty-unit row itemID into the ship's hold as the hold has room for,
// splitting it if it does not all fit; the rest stays where it is (the station hangar; procurement takes
// from the pilot's own hangar before it buys). Reports the units stowed.
func (m *Mod) stowToCargo(ctx context.Context, b *botRun, itemID, typeID, qty int64) (int64, error) {
	units := holdCapUnits(qty, m.shipHoldRoom(ctx, b), m.typeVolume(ctx, typeID))
	if units <= 0 {
		return 0, nil
	}
	id := itemID
	if units < qty {
		var err error
		if id, err = evedb.SplitStackN(ctx, m.db, itemID, units); err != nil {
			return 0, err
		}
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = 5 WHERE itemID = ?`, b.ShipID, id); err != nil {
		return 0, err
	}
	// Reload stacks arrive again and again as ammo is spent; fold them
	// into one row so the hold does not fill with slivers.
	_, err := m.mergeCargoStacks(ctx, b.ShipID, typeID)
	return units, err
}

// mergeCargoStacks folds every loose stack of typeID in a ship's hold into
// one row, returning its itemID (0 when the hold has none).
func (m *Mod) mergeCargoStacks(ctx context.Context, shipID, typeID int64) (int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE locationID = ? AND flag = 5 AND typeID = ?
		ORDER BY itemID ASC`, shipID, typeID)
	if err != nil {
		return 0, fmt.Errorf("mergeCargoStacks: %w", err)
	}
	var first, total int64
	var extras []int64
	for rows.Next() {
		var id, q int64
		if err := rows.Scan(&id, &q); err != nil {
			rows.Close()
			return 0, fmt.Errorf("mergeCargoStacks scan: %w", err)
		}
		total += q
		if first == 0 {
			first = id
		} else {
			extras = append(extras, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if first == 0 || len(extras) == 0 {
		return first, nil
	}
	for _, id := range extras {
		if _, err := m.db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, id); err != nil {
			return 0, err
		}
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET quantity = ? WHERE itemID = ?`, total, first); err != nil {
		return 0, err
	}
	return first, nil
}

// fitLines flattens a fit's slot contents, every module line and its charge, by (typeID, flag), for
// StateData.FitLines. Cargo and drones are not slot lines and never collide with the grid.
func fitLines(fit *Fit) []ShopItem {
	if fit == nil {
		return nil
	}
	var lines []ShopItem
	for _, mod := range fit.Modules {
		lines = append(lines, ShopItem{TypeID: mod.TypeID, Name: mod.TypeName, Dest: "module", Flag: mod.Flag})
		if mod.ChargeTypeID != 0 {
			lines = append(lines, ShopItem{TypeID: mod.ChargeTypeID, Name: mod.ChargeName, Dest: "charge", Flag: mod.Flag})
		}
	}
	return lines
}

// stripOffFitModules unfits every slot occupant the fit being assembled does not claim, into the station
// hangar (as clearSlot does). Slots in keep are left alone: a pending line will swap them out
// (applyShopItem). A hull reused across fits keeps the old fit's modules on slots the new template never
// touches; they are unbudgeted grid load, so by procureComplete the ship must be the template alone.
func (m *Mod) stripOffFitModules(ctx context.Context, b *botRun, keep map[int64]bool) error {
	st := b.State
	if len(st.Data.FitLines) == 0 || b.ShipID == 0 || b.StationID == 0 {
		return nil
	}
	sanctioned := map[int64]map[int64]bool{} // flag → typeIDs the fit claims
	for _, ln := range st.Data.FitLines {
		if sanctioned[ln.Flag] == nil {
			sanctioned[ln.Flag] = map[int64]bool{}
		}
		sanctioned[ln.Flag][ln.TypeID] = true
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, typeID, flag FROM entity
		WHERE locationID = ? AND flag BETWEEN ? AND 34`, b.ShipID, flagLoFirst)
	if err != nil {
		return fmt.Errorf("stripOffFitModules: %w", err)
	}
	type occupant struct{ itemID, typeID, flag int64 }
	var strays []occupant
	for rows.Next() {
		var o occupant
		if err := rows.Scan(&o.itemID, &o.typeID, &o.flag); err != nil {
			rows.Close()
			return fmt.Errorf("stripOffFitModules scan: %w", err)
		}
		// A civilian starter weapon is never a stray: rearmToothless fits it because an unarmed hull is refused
		// every belt (threat.go), and stripping it would undo that. It comes off once procurement buys the real gun.
		if isStrayOccupant(o.flag, o.typeID, sanctioned, keep) {
			strays = append(strays, o)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range strays {
		if err := evedb.UnfitModuleToHangar(ctx, m.db, o.itemID, b.StationID); err != nil {
			return err
		}
		m.logBot(b.CharID, logInfo, "procure",
			"slot %d held item %d (type %d) from an earlier fit — unfitted to the hangar, the new fit does not budget for it",
			o.flag, o.itemID, o.typeID)
	}
	return nil
}

// shedStarterWeaponsIfArmed unfits the civilian starter weapons the fit does not claim, into the
// station hangar, when the ship carries a real weapon or combat drones as well. It is the
// over-budget completion's last resort: stripOffFitModules never touches a starter weapon, since an
// unarmed hull is refused every belt (rearmToothless), so it comes off only where that cannot follow.
func (m *Mod) shedStarterWeaponsIfArmed(ctx context.Context, b *botRun) error {
	st := b.State
	if b.ShipID == 0 || b.StationID == 0 {
		return nil
	}
	if real, err := m.shipTeeth(ctx, b.ShipID, true); err != nil || real == 0 {
		return err
	}
	claimed := map[int64]map[int64]bool{} // flag → typeIDs the fit claims
	for _, ln := range st.Data.FitLines {
		if claimed[ln.Flag] == nil {
			claimed[ln.Flag] = map[int64]bool{}
		}
		claimed[ln.Flag][ln.TypeID] = true
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, typeID, flag FROM entity
		WHERE locationID = ? AND flag BETWEEN ? AND 34`, b.ShipID, flagLoFirst)
	if err != nil {
		return fmt.Errorf("shedStarterWeaponsIfArmed: %w", err)
	}
	type occupant struct{ itemID, typeID, flag int64 }
	var guns []occupant
	for rows.Next() {
		var o occupant
		if err := rows.Scan(&o.itemID, &o.typeID, &o.flag); err != nil {
			rows.Close()
			return fmt.Errorf("shedStarterWeaponsIfArmed scan: %w", err)
		}
		if isUnclaimedStarterWeapon(o.flag, o.typeID, claimed) {
			guns = append(guns, o)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range guns {
		if err := evedb.UnfitModuleToHangar(ctx, m.db, o.itemID, b.StationID); err != nil {
			return err
		}
		m.logBot(b.CharID, logInfo, "procure",
			"slot %d held a %s the fit does not budget for — unfitted to the hangar, the ship is armed without it",
			o.flag, m.typeNameOr(ctx, o.typeID))
	}
	return nil
}

// isUnclaimedStarterWeapon reports whether a slot occupant is a civilian starter weapon the fit
// being assembled does not claim on that slot. Pure.
func isUnclaimedStarterWeapon(flag, typeID int64, claimed map[int64]map[int64]bool) bool {
	return civilianStarterWeapons[typeID] && !claimed[flag][typeID]
}

// isStrayOccupant decides whether one slot occupant comes off before the
// shopping loop: not claimed by the fit, not a civilian starter weapon (see
// stripOffFitModules), and not on a slot a pending line will swap it out of.
func isStrayOccupant(flag, typeID int64, sanctioned map[int64]map[int64]bool, keep map[int64]bool) bool {
	if sanctioned[flag][typeID] || civilianStarterWeapons[typeID] {
		return false
	}
	return !keep[flag]
}

// pendingSlotFlags is every slot flag a module or charge line on the list is
// headed for.
func pendingSlotFlags(list []ShopItem) map[int64]bool {
	out := map[int64]bool{}
	for _, it := range list {
		if (it.Dest == "module" || it.Dest == "charge") && it.Flag != 0 {
			out[it.Flag] = true
		}
	}
	return out
}

// shipHasFittedModules reports whether the ship carries any module at all: the line between a refit and
// a new hull.
func (m *Mod) shipHasFittedModules(ctx context.Context, shipID int64) bool {
	if shipID == 0 {
		return false
	}
	var n int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.locationID = ? AND e.flag BETWEEN ? AND 34 AND g.categoryID = 7`,
		shipID, flagLoFirst).Scan(&n); err != nil {
		return false // unreadable reads as a new hull: the stricter path
	}
	return n > 0
}

// shedIncumbentsIfOverGrid strips the modules kept on still-pending slots when
// the ship has gone over its CPU or powergrid. The ordinary case is a no-op.
func (m *Mod) shedIncumbentsIfOverGrid(ctx context.Context, b *botRun, pending map[int64]bool) {
	if len(pending) == 0 || b.ShipID == 0 || b.StationID == 0 {
		return
	}
	var hullTypeID int64
	if m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, b.ShipID).Scan(&hullTypeID) != nil {
		return
	}
	cpuCap, pgCap, err := evedb.GetShipCPUPGCapacityFitted(ctx, m.db, hullTypeID, b.ShipID)
	if err != nil {
		return
	}
	cpuLoad, pgLoad, err := evedb.GetShipCPUPowerLoad(ctx, m.db, b.ShipID)
	if err != nil || (cpuLoad <= cpuCap && pgLoad <= pgCap) {
		return
	}
	for flag := range pending {
		if err := m.clearSlot(ctx, b, flag); err != nil {
			m.logBot(b.CharID, logError, "procure", "shedding slot %d over grid: %v", flag, err)
		}
	}
	m.logBot(b.CharID, logInfo, "procure",
		"kept modules pushed the ship over its grid (CPU %.1f/%.1f, PG %.1f/%.1f) — unfitted the ones awaiting replacement",
		cpuLoad, cpuCap, pgLoad, pgCap)
}

// clearSlot unfits whatever occupies flag on the boarded ship, leaving it in the station hangar rather than
// destroying it (a swapped-out module is the operator's to sell). Any charge on the same flag goes with it.
func (m *Mod) clearSlot(ctx context.Context, b *botRun, flag int64) error {
	if flag == 0 || b.ShipID == 0 || b.StationID == 0 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT itemID, typeID FROM entity WHERE locationID = ? AND flag = ?`, b.ShipID, flag)
	if err != nil {
		return fmt.Errorf("clearSlot: %w", err)
	}
	defer rows.Close()
	type occupant struct{ itemID, typeID int64 }
	var found []occupant
	for rows.Next() {
		var o occupant
		if err := rows.Scan(&o.itemID, &o.typeID); err != nil {
			return fmt.Errorf("clearSlot scan: %w", err)
		}
		found = append(found, o)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range found {
		if err := evedb.UnfitModuleToHangar(ctx, m.db, o.itemID, b.StationID); err != nil {
			return err
		}
		m.logBot(b.CharID, logDebug, "fit",
			"slot %d already held item %d (type %d) — unfitted to the hangar to make room",
			flag, o.itemID, o.typeID)
	}
	return nil
}

// gridLoad is a ship's fitted CPU and powergrid load against what the hull gives this pilot.
type gridLoad struct {
	cpuLoad, cpuCap, pgLoad, pgCap float64
}

func (g gridLoad) over() bool { return g.cpuLoad > g.cpuCap || g.pgLoad > g.pgCap }

func (g gridLoad) String() string {
	return fmt.Sprintf("CPU %.1f/%.1f, PG %.1f/%.1f", g.cpuLoad, g.cpuCap, g.pgLoad, g.pgCap)
}

// shipGrid reads a ship's grid load. ok is false when any read fails, and the caller then
// leaves the ship alone.
func (m *Mod) shipGrid(ctx context.Context, shipID int64) (gridLoad, bool) {
	var g gridLoad
	var hullTypeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, shipID).Scan(&hullTypeID); err != nil {
		return g, false
	}
	var err error
	if g.cpuCap, g.pgCap, err = evedb.GetShipCPUPGCapacityFitted(ctx, m.db, hullTypeID, shipID); err != nil {
		return g, false
	}
	if g.cpuLoad, g.pgLoad, err = evedb.GetShipCPUPowerLoad(ctx, m.db, shipID); err != nil {
		return g, false
	}
	return g, true
}

// procureComplete verifies the fit and hands control back to the activity.
func (m *Mod) procureComplete(ctx context.Context, b *botRun) error {
	st := b.State
	// Self-check so boot reconciliation never strips the fit silently. Over budget, whatever the fit does
	// not claim comes off first (nothing is pending any more, so no slot is kept); only a ship that is
	// still over with the template alone is reported as a fault.
	if g, ok := m.shipGrid(ctx, b.ShipID); ok && g.over() {
		if err := m.stripOffFitModules(ctx, b, nil); err != nil {
			m.logBot(b.CharID, logError, "procure", "shedding leftovers from an over-budget fit: %v", err)
		}
		// Still over: a civilian starter weapon the fit does not claim is the one leftover that
		// survives the strip, and it goes too once the ship is armed without it.
		if mid, ok := m.shipGrid(ctx, b.ShipID); ok && mid.over() {
			if err := m.shedStarterWeaponsIfArmed(ctx, b); err != nil {
				m.logBot(b.CharID, logError, "procure", "shedding a starter weapon from an over-budget fit: %v", err)
			}
		}
		switch after, ok := m.shipGrid(ctx, b.ShipID); {
		case ok && !after.over():
			m.logBot(b.CharID, logWarn, "procure",
				"finished fit was over budget (%s) — unfitted what the fit does not claim, now %s", g, after)
		case ok:
			m.logBot(b.CharID, logError, "procure",
				"finished fit is over budget (%s) — boot would offline modules", after)
		}
	}
	if st.Data.ShopStockRun {
		// The home shopping pass's stock-up: there is no activity to resume. The visit is stamped, the pilot goes
		// idle, and the seam either hauls the goods home or lets the draw run where it stands.
		m.logBot(b.CharID, logInfo, "home", "stock-up complete — back to its own devices")
		st.Activity, st.Phase = "", "idle"
		st.Data.ProcureFitID, st.Data.ResumeActivity, st.Data.Shopping = 0, "", nil
		st.Data.FitLines = nil
		st.Data.ProcureSoft, st.Data.ShopStockRun = false, false
		st.Data.Path, st.Data.NextPhase = nil, ""
		st.Data.HomeShoppedAt = clock.Unix()
		return saveState(ctx, m.db, st)
	}
	m.logBot(b.CharID, logInfo, "procure", "ship complete (entity %d) — resuming %s",
		b.ShipID, st.Data.ResumeActivity)
	resume := st.Data.ResumeActivity
	st.Activity, st.Phase = resume, "idle"
	st.Data.ProcureFitID, st.Data.ResumeActivity, st.Data.Shopping = 0, "", nil
	st.Data.FitLines = nil
	st.Data.ProcureSoft = false
	st.Data.Path, st.Data.NextPhase = nil, ""
	return saveState(ctx, m.db, st)
}

// procureBreakOff ends a shopping trip early and puts the bot back to work with what it has. A run that
// finds no seller has broken nothing: the ship still flies, mines and shoots with what is aboard, and
// halting would take a working bot off the field.
func (m *Mod) procureBreakOff(ctx context.Context, b *botRun, why string) error {
	st := b.State
	if st.Data.ShopStockRun {
		// A stock-up that ran out of sellers is a finished visit, not a failed one: the cooldown below stops the
		// next pass re-flying the same errand, and the stamp stops the shopping phase re-entering.
		m.logBot(b.CharID, logInfo, "home",
			"gave up on the rest of the stock-up (%s) — its space sells what it sells", why)
		st.Activity, st.Phase = "", "idle"
		st.Data.ProcureFitID, st.Data.ResumeActivity, st.Data.Shopping = 0, "", nil
		st.Data.FitLines = nil
		st.Data.ProcureSoft, st.Data.ShopStockRun = false, false
		st.Data.RestockAfter = clock.Unix() + restockCooldownSec
		st.Data.Path, st.Data.NextPhase = nil, ""
		st.Data.HomeShoppedAt = clock.Unix()
		return saveState(ctx, m.db, st)
	}
	m.logBot(b.CharID, logInfo, "procure",
		"giving up on the rest of the shopping (%s) — carrying on with %s on what it has",
		why, st.Data.ResumeActivity)
	resume := st.Data.ResumeActivity
	st.Activity, st.Phase = resume, "idle"
	st.Data.ProcureFitID, st.Data.ResumeActivity, st.Data.Shopping = 0, "", nil
	st.Data.FitLines = nil
	st.Data.ProcureSoft = false
	st.Data.RestockAfter = clock.Unix() + restockCooldownSec
	st.Data.Path, st.Data.NextPhase = nil, ""
	return saveState(ctx, m.db, st)
}

// ── teeth first ─────────────────────────────────────────────────────────────
// A refit whose wallet cannot cover the whole list would buy nothing and leave a toothless hull at home.
// So the weapons, the rounds loaded in them and the drone bay go first and answer only to each other; the
// tank, the tackle and the rest of the rack wait for what the guns earn.

// teethOf is the part of a shopping list that arms a ship: weapon modules,
// charge lines (a gun with nothing loaded is scrap) and drones. isWeapon is
// the module test (weaponLine), passed in so the rule stays pure.
func teethOf(list []ShopItem, isWeapon func(typeID int64) bool) []ShopItem {
	var out []ShopItem
	for _, it := range list {
		switch it.Dest {
		case "charge", "drone":
			out = append(out, it)
		case "module":
			if isWeapon(it.TypeID) {
				out = append(out, it)
			}
		}
	}
	return out
}

// isTeethLine is teethOf's test for one line.
func isTeethLine(it ShopItem, isWeapon func(typeID int64) bool) bool {
	return len(teethOf([]ShopItem{it}, isWeapon)) == 1
}

// weaponLine reports whether a type is a turret or a launcher, the groups the starter substitute stands
// in for (brokestarter.go).
func (m *Mod) weaponLine(ctx context.Context) func(int64) bool {
	groups := starterWeaponGroups()
	return func(typeID int64) bool { return groups[m.typeGroupOf(ctx, typeID)] }
}

// listHasHull: a list carrying a hull line is a new ship, not a refit.
func listHasHull(list []ShopItem) bool {
	for _, it := range list {
		if it.Dest == "hull" {
			return true
		}
	}
	return false
}

// procureTeethFirst narrows an unaffordable refit trip to its teeth when the ship has none and the wallet
// covers them. It reports whether it did; the rest of the list is not lost, since the next plan pass diffs
// the ship against its fit again.
func (m *Mod) procureTeethFirst(ctx context.Context, b *botRun) bool {
	st := b.State
	list := st.Data.Shopping
	if listHasHull(list) || b.ShipID == 0 || !m.shipHasNoTeeth(ctx, b.ShipID) {
		return false
	}
	teeth := teethOf(list, m.weaponLine(ctx))
	if len(teeth) == 0 || len(teeth) == len(list) {
		return false
	}
	st.Data.Shopping = teeth
	if err := m.procureAffordable(ctx, b); err != nil {
		st.Data.Shopping = list
		return false
	}
	m.logBot(b.CharID, logInfo, "procure",
		"cannot pay for the whole rack — arming the ship first (%s); the rest waits for what the guns earn",
		shopSummary(teeth))
	return true
}

// procureAffordable reports whether the wallet covers what is still on the shopping list, as a
// shortfallError when it does not. It is priced off estimatePrice (the cheapest live ask anywhere), not the
// station the pilot is at, and is generous (an unpriced line is skipped): a false refusal sends a pilot to
// earn money it did not need, a false pass costs one wasted trip.
func (m *Mod) procureAffordable(ctx context.Context, b *botRun) error {
	list := b.State.Data.Shopping
	if len(list) == 0 {
		return nil
	}
	// What its own hangars hold costs nothing to collect (ownstock.go).
	var own map[int64]int64
	if by, err := m.ownStockByStation(ctx, b.CharID, shoppingTypeIDs(list)); err == nil {
		own = ownStockTotals(by)
	}
	need := 0.0
	for _, it := range list {
		qty := it.Qty
		if it.Dest != "hull" && own[it.TypeID] > 0 {
			covered := own[it.TypeID]
			if covered > qty {
				covered = qty
			}
			own[it.TypeID] -= covered
			qty -= covered
		}
		if qty <= 0 {
			continue
		}
		p, err := m.estimatePriceFor(ctx, b, it.TypeID)
		if err != nil || p <= 0 {
			continue
		}
		need += p * float64(qty)
	}
	if need <= 0 {
		return nil
	}
	have, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return nil // a wallet that cannot be read is not a refusal
	}
	// Measured against what the wallet may commit, not all of it (broke.go): a trip that ends with a complete
	// list and a pilot too poor to buy ammunition has not succeeded.
	if spendable(have) >= need {
		return nil
	}
	return floorShortfall(shopSummary(list), need, have)
}

// hullFromHand is the provenance ownedHullFor stands for: the hull was not
// bought on this trip. Kept as a constant because the log line branches on it.
const hullFromHand = "already in hand"

// ownedHullFor answers with a hull of this type the pilot already has here, or 0. It is the buyhull phase's
// resume guard: a hull bought by a run killed before it saved its phase is still the pilot's, and taking it
// beats buying a second. The boarded ship is checked first (a kill after boardShip leaves nothing in the
// hangar); the hangar query then prefers a packaged hull (singleton = 0), as a fresh purchase looks.
func (m *Mod) ownedHullFor(ctx context.Context, b *botRun, typeID int64) int64 {
	if b.ShipID != 0 {
		var t int64
		if err := m.db.QueryRowContext(ctx,
			`SELECT typeID FROM entity WHERE itemID = ? AND ownerID = ?`,
			b.ShipID, b.CharID).Scan(&t); err == nil && t == typeID {
			return b.ShipID
		}
	}
	if b.StationID == 0 {
		return 0
	}
	var id int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT itemID FROM entity
		WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = ?
		ORDER BY singleton ASC, itemID ASC
		LIMIT 1`, b.CharID, b.StationID, typeID, flagHangar).Scan(&id); err != nil {
		return 0
	}
	return id
}

// procureAbandonMoney ends a trip the wallet cannot pay for through the usual two endings: break off and
// fly on with what it has, or abandon the fit and keep the half-built ship. It returns nil, not the
// shortfall: an error out of an FSM step counts toward the halt streak (simpool.go), the wrong answer for a
// pilot that is only poor. The pilot goes idle and planSolo's errCannotAfford refusals reach brokeEarn.
func (m *Mod) procureAbandonMoney(ctx context.Context, b *botRun, short error) error {
	m.logBot(b.CharID, logInfo, "procure",
		"stopping the shopping trip — %v; going to earn instead", short)
	return m.procureAbandon(ctx, b, short.Error())
}

// procureAbandon gives up on the trip. A trip that ran out of sellers is a dead end until the world changes,
// but the bot is not: the fit goes on cooldown, the gap is in the demand ledger, and the bot flies what it
// managed to build or does other work. The half-built ship is kept: the next trip for this fit tops it up.
func (m *Mod) procureAbandon(ctx context.Context, b *botRun, why string) error {
	st := b.State
	// Soft trips never dead-end (see procureBreakOff). This is the single gate, so every dead end inherits it.
	if st.Data.ProcureSoft {
		return m.procureBreakOff(ctx, b, why)
	}
	// The market has spoken: this fit cannot be bought right now. Remember that per fit, so the selector
	// stops proposing to buy it until the cooldown runs out.
	noteFitProcureFail(&st.Data, st.Data.ProcureFitID)

	// Does the trip already have its hull? Recorded the moment it is bought,
	// so a dead end past that point leaves a ship worth looking at.
	var shipID int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT s.shipItemID FROM rolebot_fit_ships s
		JOIN entity e ON e.itemID = s.shipItemID
		WHERE s.characterID = ? AND s.fitID = ?`,
		b.CharID, st.Data.ProcureFitID).Scan(&shipID)
	if shipID != 0 && len(st.Data.Shopping) > 0 {
		// A trip can soften along the way: one that set out for a module and some ammo is hard, but once the
		// module is bought, what is outstanding is an errand, and a ship that flies.
		if shopIsConsumablesOnly(st.Data.Shopping) {
			return m.procureBreakOff(ctx, b, why)
		}
		// Modules are outstanding, but does the ship still do the job without them (a miner short one of three
		// lasers mines slower)? When what is fitted still scores, fly it as it is. Read the ship, not a stored fit.
		if a, _, err := m.shipForActivity(ctx, b.CharID, st.Data.ResumeActivity); err == nil && a != nil {
			t := m.ownedShipTemplate(ctx, b.CharID, a.ShipItemID,
				hullOfGeneratedFit(st.Data.ProcureFitID))
			if t == nil || t.Fit == nil {
				t = nil
			}
			if t != nil {
				t.Job = m.jobForPilot(ctx, b.CharID, st.Data.ResumeActivity)
				// Judged against the plan (what is fitted plus what is still on the list), not against whether one gun
				// fires (fitshort.go). A different ship the selector would fly has no lines outstanding on this trip.
				planned, missing := t, []ShopItem(nil)
				if a.FitID == st.Data.ProcureFitID {
					planned, missing = plannedFromShip(t, st.Data.Shopping), st.Data.Shopping
				}
				ok, short := m.shortFitVerdict(ctx, planned, missing)
				if ok {
					m.logBot(b.CharID, logInfo, "procure",
						"ship is short-fitted but still does the job — flying it as it is")
					return m.procureBreakOff(ctx, b, why)
				}
				m.logBot(b.CharID, logInfo, "procure",
					"refusing to fly half-fitted — %s; the fit waits for sellers or money", short)
			}
		}
	}
	// Does it still own a ship it could be working in? An upgrade trip that dead-ends leaves a perfectly good
	// hull at home.
	if fell, err := m.procureFallBack(ctx, b, why); err != nil || fell {
		return err
	}
	// Nothing to fly and nothing to fall back to: go idle and let the next plan pass draw other work. The
	// cooldown above turns this activity's pick into "no fit available", which the appetite draw passes over.
	m.logBot(b.CharID, logWarn, "procure",
		"abandoning procurement: %s — the fit sits out %d min while the bot looks for other work",
		why, procureCooldownSec/60)
	st.Activity, st.Phase = "", "idle"
	st.Data.ProcureFitID, st.Data.ResumeActivity, st.Data.Shopping = 0, "", nil
	st.Data.FitLines = nil
	st.Data.ProcureSoft = false
	st.Data.Path, st.Data.NextPhase = nil, ""
	st.Data.LegEndsAt = clock.Unix() + idleRetrySec
	return saveState(ctx, m.db, st)
}

// procureFallBack looks for another fit of the same role the bot already has a live ship for, and puts it
// back to work in that instead of halting. Reports whether it found one. Nothing is given up: the fit it
// could not buy is still in the pool and scored on the next pass.
func (m *Mod) procureFallBack(ctx context.Context, b *botRun, why string) (bool, error) {
	st := b.State
	activity := st.Data.ResumeActivity
	if activity == "" {
		return false, nil
	}
	s, t, err := m.shipForActivity(ctx, b.CharID, activity)
	if err != nil || s == nil || s.FitID == st.Data.ProcureFitID {
		return false, err
	}
	// This is the line the fallback prints when it worked: the pilot goes back to work in a hull it already
	// owns. The failure worth an error level is the caller's, for a pilot nothing was found for.
	m.logBot(b.CharID, logInfo, "procure",
		"could not finish the %s trip (%s) — falling back to %q, which it already has a ship for",
		activity, why, t.Name)
	st.Activity, st.Phase = activity, "idle"
	st.Data.ProcureFitID, st.Data.ResumeActivity, st.Data.Shopping = 0, "", nil
	st.Data.FitLines = nil
	st.Data.ProcureSoft = false
	st.Data.Path, st.Data.NextPhase = nil, ""
	st.Data.LegEndsAt = clock.Unix() + idleRetrySec
	return true, saveState(ctx, m.db, st)
}

// homeStation is where this pilot parks a hull it is not flying: its home, which is its medical clone
// station and nothing else (homeStationFor). Where it is standing is the last resort, for a pilot whose
// clone station has been deleted.
func (m *Mod) homeStation(ctx context.Context, b *botRun) (int64, error) {
	if home := m.homeStationFor(ctx, b.CharID); home != 0 {
		return home, nil
	}
	if b.StationID != 0 {
		return b.StationID, nil
	}
	return 0, fmt.Errorf("no home station: this pilot has no medical clone")
}

func shoppingFirst(list []ShopItem, dest string) *ShopItem {
	for i := range list {
		if list[i].Dest == dest {
			return &list[i]
		}
	}
	return nil
}

func shoppingRemove(list []ShopItem, it *ShopItem) []ShopItem {
	out := make([]ShopItem, 0, len(list))
	for i := range list {
		if &list[i] == it || (list[i].Dest == it.Dest && list[i].TypeID == it.TypeID && list[i].Flag == it.Flag) {
			continue
		}
		out = append(out, list[i])
	}
	return out
}
