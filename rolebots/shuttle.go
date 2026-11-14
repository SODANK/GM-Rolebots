package rolebots

import (
	"context"

	evedb "gurimoon/db"
)

// The runabout: the 5,000 ISK zero-slot hull that carries a pilot, not cargo.
// It is never a fit (nothing is written to rolebot_fit_ships).
// See docs/comment-notes-b17.md#runabout.
const (
	shuttleGroupID = 31

	// What a pilot will pay for a ride: the cap is mean enough that paying
	// frigate money for a slotless hull is worth a log line.
	runaboutMaxPrice = 200000.0

	// What a pilot keeps in the wallet regardless, so buying the ride never
	// leaves it unable to re-buy a real hull.
	runaboutKeepISK = 25000.0

	// How long a pilot that could not find a ride stops asking: a station's book
	// does not grow a shuttle in thirty seconds.
	rideFailCooldownSec = 1800
)

// shuttleByRace is each empire's shuttle, keyed like rookieHullByRace
// (character_.raceID). The four are identical in every attribute that matters,
// and a pilot flying its own empire's shuttle needs no explaining.
var shuttleByRace = map[int64]int64{
	1: 672,   // Caldari Shuttle
	2: 11132, // Minmatar Shuttle
	4: 11134, // Amarr Shuttle
	8: 11129, // Gallente Shuttle
}

// shuttleTypeFor is the hull this pilot buys when it needs a ride, with its
// name for the log. Zero when the character's race is not one of the four.
func (m *Mod) shuttleTypeFor(ctx context.Context, charID int64) (int64, string) {
	typeID, ok := shuttleByRace[m.charRace(ctx, charID)]
	if !ok {
		return 0, ""
	}
	name, err := typeName(ctx, m.db, typeID)
	if err != nil {
		return typeID, "shuttle"
	}
	return typeID, name
}

// ownedShuttleAt is a shuttle of any race this pilot already owns in that
// station's hangar, or 0. Assembled ones come first: a packaged hull must be
// unpacked before boarding, which the buy path does for its own.
func (m *Mod) ownedShuttleAt(ctx context.Context, charID, stationID int64) int64 {
	if stationID == 0 {
		return 0
	}
	var id int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT e.itemID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ? AND t.groupID = ?
		ORDER BY e.singleton DESC, e.itemID ASC
		LIMIT 1`, charID, stationID, flagHangar, shuttleGroupID).Scan(&id); err != nil {
		return 0
	}
	return id
}

// rideOwnedMaxJumps is how far a podded pilot flies for a shuttle of its own
// rather than buying one where it is.
const rideOwnedMaxJumps = 5

// ownedShuttleInReach is the nearest station within rideOwnedMaxJumps, other
// than the one the pilot is docked in, where it owns a shuttle: the station,
// its system and the jumps, or zeros.
func (m *Mod) ownedShuttleInReach(ctx context.Context, b *botRun) (int64, int64, int) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.locationID, s.solarSystemID FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN staStations s ON s.stationID = e.locationID
		 WHERE e.ownerID = ? AND e.flag = ? AND t.groupID = ? AND e.locationID <> ?
		 LIMIT 12`, b.CharID, flagHangar, shuttleGroupID, b.StationID)
	if err != nil {
		return 0, 0, 0
	}
	type at struct{ st, sys int64 }
	var cands []at
	for rows.Next() {
		var a at
		if rows.Scan(&a.st, &a.sys) == nil {
			cands = append(cands, a)
		}
	}
	rows.Close()
	bestSt, bestSys, bestJ := int64(0), int64(0), rideOwnedMaxJumps+1
	for _, a := range cands {
		j, err := m.jumpsBetween(ctx, b, b.SystemID, a.sys)
		if err != nil || j < 0 || j >= bestJ {
			continue
		}
		bestSt, bestSys, bestJ = a.st, a.sys, j
	}
	if bestSt == 0 {
		return 0, 0, 0
	}
	return bestSt, bestSys, bestJ
}

// ownsShuttleAnywhere reports whether the pilot has a runabout parked
// somewhere already. It is what stops a fleet of shuttles accumulating: the
// stock-up pass buys one only for a pilot that owns none at all.
func (m *Mod) ownsShuttleAnywhere(ctx context.Context, charID int64) bool {
	var one int64
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.ownerID = ? AND t.groupID = ?
		LIMIT 1`, charID, shuttleGroupID).Scan(&one)
	return err == nil
}

// ensureRunabout answers "is this pilot in something that can carry a
// blueprint", and buys the cheapest possible yes when it is not. A real hull
// settles it; a capsule does not, because a blueprint loaded into a pod rides in
// a ship the next death deletes. Docked only. Reports the hull now boarded.
func (m *Mod) ensureRunabout(ctx context.Context, b *botRun, why string) (int64, bool) {
	if b.ShipID != 0 && !m.isCapsule(ctx, b.ShipID) {
		return b.ShipID, true
	}
	if b.StationID == 0 {
		return 0, false
	}
	if owned := m.ownedShuttleAt(ctx, b.CharID, b.StationID); owned != 0 {
		// One shuttle off the row, never the whole packaged stack (hullassemble.go).
		if one, err := m.assembleOneHull(ctx, owned); err != nil {
			rlogf("rolebots: assemble owned runabout %d: %v", owned, err)
			return 0, false
		} else {
			owned = one
		}
		if err := m.boardRunabout(ctx, b, owned); err != nil {
			rlogf("rolebots: board runabout %d: %v", owned, err)
			return 0, false
		}
		m.logBot(b.CharID, logInfo, "procure",
			"took its shuttle (entity %d) out of the hangar for %s", owned, why)
		return owned, true
	}
	typeID, name := m.shuttleTypeFor(ctx, b.CharID)
	if typeID == 0 {
		return 0, false
	}
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil || bal < runaboutKeepISK {
		return 0, false
	}
	itemID, _, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID, typeID, 1, "shuttle")
	if err != nil {
		return 0, false
	}
	if spent > runaboutMaxPrice {
		// Paid already (PlaceBuyOrder commits before this line), so the cap is a thing
		// to say rather than undo.
		m.logBot(b.CharID, logInfo, "procure",
			"paid %s for a %s, well over what a hull with no slots is worth", iskString(spent), name)
	}
	// Out of the crate, like every other hull this module buys: one unit off the
	// row, which the purchase may have merged into a packaged stack.
	if one, err := m.assembleOneHull(ctx, itemID); err != nil {
		rlogf("rolebots: assemble runabout %d: %v", itemID, err)
		return 0, false
	} else {
		itemID = one
	}
	if err := m.boardRunabout(ctx, b, itemID); err != nil {
		rlogf("rolebots: board bought runabout %d: %v", itemID, err)
		return 0, false
	}
	m.logBot(b.CharID, logInfo, "procure",
		"bought a %s for %s — %s (entity %d)", name, iskString(spent), why, itemID)
	return itemID, true
}

// boardRunabout gets the pilot into the shuttle. boardShip handles capsule
// cleanup; this is a named wrapper for call sites that mean the shuttle just bought.
func (m *Mod) boardRunabout(ctx context.Context, b *botRun, shipID int64) error {
	return m.boardShip(ctx, b, shipID)
}

// rideHome is the plan-time half: a pilot that has just lost its ship buys its
// way out of the capsule instead of flying the pod across the cluster. It reports
// handled=true only when it has taken the pass; every refusal hands the pass
// back to the pod path, and the expensive work sits behind the capsule test.
func (m *Mod) rideHome(ctx context.Context, b *botRun) (bool, error) {
	if b.ShipID == 0 || !m.isCapsule(ctx, b.ShipID) {
		return false, nil
	}
	if clock.Unix() < b.State.Data.RideFailAt {
		return false, nil
	}
	// A pilot standing in its own home hangar needs no ride: procurement puts it in
	// a real hull where it is.
	if home, err := m.homeStation(ctx, b); err == nil && home != 0 && home == b.StationID {
		return false, nil
	}
	// A shuttle it already owns a few jumps off beats buying another. One parked
	// right here is ensureRunabout's own first choice.
	if b.StationID == 0 || m.ownedShuttleAt(ctx, b.CharID, b.StationID) == 0 {
		if st, sys, j := m.ownedShuttleInReach(ctx, b); st != 0 {
			m.logBot(b.CharID, logInfo, "procure",
				"in a capsule with no ship — its own shuttle is parked %d jump(s) away at %s. Going to take that one.",
				j, m.stationLabel(ctx, st))
			return true, m.beginTravel(ctx, b, sys, st, string(TaskRideBuy))
		}
	}
	// Docked somewhere else: the ride is on sale here or it is not. Either way the
	// pass carries on; with a shuttle under it the home loop flies the pilot home.
	if b.StationID != 0 {
		if _, ok := m.ensureRunabout(ctx, b, "a ride home after losing its ship"); !ok {
			b.State.Data.RideFailAt = clock.Unix() + rideFailCooldownSec
			m.logBot(b.CharID, logInfo, "procure",
				"nothing here sells a shuttle — flying the capsule on rather than waiting for one")
			return false, saveState(ctx, m.db, b.State)
		}
		return false, nil
	}

	// In space, in a pod: find the nearest station that stocks one. The nearest
	// station may be an empty outpost with nothing to buy.
	typeID, name := m.shuttleTypeFor(ctx, b.CharID)
	if typeID == 0 {
		return false, nil
	}
	if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err != nil || bal < runaboutKeepISK {
		return false, nil
	}
	var pick *marketStop
	if _, err := m.shopLadder(ctx, b, []int64{typeID}, true, m.homeRegionSet(ctx, b), defaultBand(b),
		"every shuttle it can reach is sold by a faction it dislikes — buying anyway, it is a ride",
		func(scan marketScan) bool {
			pick = bestBuy(scan.Stops, b.Tune)
			return pick != nil
		}); err != nil {
		return false, nil
	}
	if pick == nil {
		b.State.Data.RideFailAt = clock.Unix() + rideFailCooldownSec
		m.logBot(b.CharID, logInfo, "procure",
			"no station in reach sells a %s — the capsule flies home the hard way", name)
		return false, saveState(ctx, m.db, b.State)
	}
	m.logBot(b.CharID, logInfo, "procure",
		"in a capsule with no ship — a %s is on sale at %.0f ISK, station %d in %s. Going to buy a ride home.",
		name, pick.Price, pick.StationID, m.travelNote(ctx, b, pick.SystemID, pick.Jumps))
	return true, m.beginTravel(ctx, b, pick.SystemID, pick.StationID, string(TaskRideBuy))
}

// rideBuyStep runs when the capsule has docked where the shuttle is. It
// always ends at idle: with the ride bought the home loop takes the pilot
// home, and without it the pod carries on exactly as it did before.
func (m *Mod) rideBuyStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if b.StationID == 0 {
		// Not docked: the trip was interrupted. Re-plan rather than buy a hull into space.
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	if _, ok := m.ensureRunabout(ctx, b, "a ride home after losing its ship"); !ok {
		st.Data.RideFailAt = clock.Unix() + rideFailCooldownSec
		m.logBot(b.CharID, logInfo, "procure",
			"the shuttle it flew here for is gone — the capsule goes on")
	}
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return false, saveState(ctx, m.db, st)
}

// stockRunabout is the proactive half, run from the home shopping pass: a pilot
// whose work involves blueprints keeps a shuttle at home for them. It does not
// consult the shopping budget, only whether the pilot already owns one.
func (m *Mod) stockRunabout(ctx context.Context, b *botRun) {
	if b.Broke || b.StationID == 0 {
		return
	}
	// Who wants one: a pilot with a factory of its own, or one enrolled in an
	// R&D project. Everybody else buys on demand at the far end.
	if b.Profile.IndustryStationID == 0 && b.State.Data.RndFieldID == 0 {
		return
	}
	if m.ownsShuttleAnywhere(ctx, b.CharID) {
		return
	}
	typeID, name := m.shuttleTypeFor(ctx, b.CharID)
	if typeID == 0 {
		return
	}
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil || bal < runaboutKeepISK {
		return
	}
	itemID, _, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID, typeID, 1, "shuttle")
	if err != nil {
		return
	}
	if one, err := m.assembleOneHull(ctx, itemID); err != nil {
		rlogf("rolebots: assemble stocked runabout %d: %v", itemID, err)
	} else {
		itemID = one
	}
	m.logBot(b.CharID, logInfo, "home",
		"keeping a %s at home for blueprint runs — %s (entity %d)", name, iskString(spent), itemID)
}
