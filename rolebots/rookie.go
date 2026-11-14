package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	evedb "gurimoon/db"
)

// The floor under the roster: the ship a pilot can always fly. Opportunity is
// the appetite draw's hard gate (appetite.go): when no activity has a fit the
// pilot can take, planSolo logs nothing and sleeps. A green pilot holds only its
// racial Frigate I, so every authored fit is out of reach until the skill
// trainer comes round. The four rookie ships ask for racial Frigate I, carry
// 120 m3 and two high slots, and can haul and (with the skill-exempt Basic
// Miner) mine, badly. Nothing is tagged: a rookie fit joins the untagged pool
// (fitselect.go) and loses to any real fit. The hull is granted, not bought,
// since rookie ships are unpublished and sell nowhere.

// rookieHullByRace is each empire's starter hull. Race IDs are the
// character_ ones (the same map seed.go builds its empire outlook from).
var rookieHullByRace = map[int64]int64{
	1: 601, // Caldari: Ibis
	2: 588, // Minmatar: Reaper
	4: 596, // Amarr: Impairor
	8: 606, // Gallente: Velator
}

// isRookieHull reports whether a hull is one of the four starter ships. It
// reads the map, not a group query, since the group also holds the four Polaris
// GM hulls.
func isRookieHull(typeID int64) bool {
	for _, id := range rookieHullByRace {
		if id == typeID {
			return true
		}
	}
	return false
}

// Rookie hulls are not seeded as authored fits; the generator picks them up like
// any other hull (hulltags.go). The two functions below are the unconditional
// starter-hull guarantee and the free grant behind it, since nothing sells one.

// ensureStarterHull is the floor under the whole roster and is unconditional: a
// docked pilot that owns no ship is given its empire's rookie hull, free, before
// it plans. It is checked from plan() on the pilot's own terms, not the job's.
// It costs one exists query for a pilot with no ship and none for one with a hull.
func (m *Mod) ensureStarterHull(ctx context.Context, b *botRun) {
	if b.StationID == 0 {
		return // in space; if it has no ship it is in a pod, and it will dock
	}
	var one int64
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND g.categoryID = 6 AND t.groupID <> 29
		LIMIT 1`, b.CharID).Scan(&one)
	if err == nil {
		return // it owns a ship of some kind; not this function's business
	}
	if !errors.Is(err, sql.ErrNoRows) {
		rlogf("rolebots: starter hull check for %d: %v", b.CharID, err)
		return
	}
	hullTypeID, ok := rookieHullByRace[m.charRace(ctx, b.CharID)]
	if !ok {
		return
	}
	name, err := typeName(ctx, m.db, hullTypeID)
	if err != nil {
		return
	}
	itemID, err := evedb.CreateItem(ctx, m.db, hullTypeID, b.CharID, b.StationID, 1)
	if err != nil {
		rlogf("rolebots: starter hull for %d: %v", b.CharID, err)
		return
	}
	if err := setFitShip(ctx, m.db, b.CharID, generatedFitID(hullTypeID), itemID); err != nil {
		rlogf("rolebots: starter hull ownership for %d: %v", b.CharID, err)
	}
	m.logBot(b.CharID, logInfo, "procure",
		"had no ship at all — issued a %s, free, the way every pilot starts (entity %d)", name, itemID)
}

// grantRookieHull hands a pilot the starter ship it is entitled to and reports
// whether a now names a live hull. Called from ensureActivityShip when the fit
// selector has settled on a rookie fit and the pilot holds no hull for it. It is
// free, as at character creation: a charge would fail the pilots the floor is for.
func (m *Mod) grantRookieHull(ctx context.Context, b *botRun, a *fitShip, t *fitTemplate) (bool, error) {
	return m.grantRookieHullAt(ctx, b, a, t, false)
}

// grantRookieHullAt is grantRookieHull with the choice of where an owned hull
// counts. here=false: any owned hull settles it. here=true (stranded pilot,
// abandonStrandedHull): only a hull in the hangar the pilot stands in counts,
// since re-adopting an unreachable one repeats the refused trip.
func (m *Mod) grantRookieHullAt(ctx context.Context, b *botRun, a *fitShip, t *fitTemplate, here bool) (bool, error) {
	if a == nil || t == nil || t.Fit == nil || !isRookieHull(t.Fit.HullTypeID) {
		return false, nil
	}
	if here && b.StationID == 0 {
		return false, nil // in space: nowhere to put one; procurement has its say
	}

	// One it already owns settles it, so a second is not handed out when an earlier
	// grant's fit row was cleared. The one in the pilot's own hangar wins.
	var itemID int64
	query := `
		SELECT itemID FROM entity
		WHERE ownerID = ? AND typeID = ? ORDER BY (locationID = ?) DESC, itemID LIMIT 1`
	args := []any{b.CharID, t.Fit.HullTypeID, b.StationID}
	if here {
		query = `
		SELECT itemID FROM entity
		WHERE ownerID = ? AND typeID = ? AND locationID = ? ORDER BY itemID LIMIT 1`
	}
	err := m.db.QueryRowContext(ctx, query, args...).Scan(&itemID)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		// Where it appears: the hangar the pilot is docked in, so it can board this
		// pass, otherwise its home (ensureActivityShip flies it from there).
		station := b.StationID
		if station == 0 {
			station = m.homeStationFor(ctx, b.CharID)
		}
		if station == 0 {
			return false, nil // nowhere to put it: let procurement have its say
		}
		if itemID, err = evedb.CreateItem(ctx, m.db, t.Fit.HullTypeID, b.CharID, station, 1); err != nil {
			return false, fmt.Errorf("grant rookie hull: %w", err)
		}
		m.logBot(b.CharID, logInfo, "procure",
			"issued a %s at station %d — the starter hull every pilot is entitled to, free (entity %d)",
			t.Fit.HullName, station, itemID)
	default:
		return false, fmt.Errorf("look for an owned rookie hull: %w", err)
	}

	if err := setFitShip(ctx, m.db, b.CharID, t.FitID, itemID); err != nil {
		return false, err
	}
	a.ShipItemID, a.ShipExists = itemID, true
	return true, nil
}
