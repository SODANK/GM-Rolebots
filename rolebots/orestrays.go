package rolebots

// The rock in the hangar that no ledger owns.
//
// strays.go fences ore out of its sweep, but a trip whose sale found no bidder
// stockpiles, a materials run leaves what its bill did not need, and a fleet's
// haul outlives the fleet. This sweep disposes of that rock: refine, then list,
// then dump. A rock is worth more as metal to anyone who can reprocess, so a
// pilot at a usable plant refines; only one that cannot sells the rock as rock.
// It never touches what another ledger holds: a commander's pooled fleet haul
// (rolebot_ore_hold) and the larder of a factory with a project still open
// (projectReservations).

import (
	"context"
	"sync"
)

// How often one pilot looks. Ore accumulates on the scale of mining trips and
// the common answer is an empty hangar, so this is one query an hour per pilot.
const hangarOreSweepSec = 3600

var (
	hangarOreMu sync.Mutex
	hangarOreAt = map[int64]int64{} // charID to unix of last sweep
)

// sellHangarOre disposes of the unledgered ore in the hangar this pilot is
// docked in. Cheap no-op for a hangar with none; throttled per pilot.
func (m *Mod) sellHangarOre(ctx context.Context, b *botRun) {
	if b.StationID == 0 {
		return
	}
	now := clock.Unix()
	hangarOreMu.Lock()
	if now-hangarOreAt[b.CharID] < hangarOreSweepSec {
		hangarOreMu.Unlock()
		return
	}
	hangarOreAt[b.CharID] = now
	hangarOreMu.Unlock()

	// A project's larder is off limits while the project is open. Checked up front
	// because the answer is about the pilot: a mid-build factory pilot stands in its own materials.
	d := b.State.Data
	if d.IndFactoryID == b.StationID && d.IndBPTypeID != 0 {
		return
	}
	// A commander's pooled haul is disposed of here, wherever it docks: the hold is
	// keyed (character, station), so skipping it would strand it after a home move.
	if m.hasFleetOre(ctx, b.CharID, b.StationID) {
		m.disposeFleetOre(ctx, b)
		return
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity)
		FROM entity e
		JOIN invTypes t  ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND e.singleton = 0
		  AND g.categoryID = 25
		GROUP BY e.typeID`, b.CharID, b.StationID)
	if err != nil {
		rlogf("rolebots: hangar ore char=%d: %v", b.CharID, err)
		return
	}
	var types []int64
	total := int64(0)
	for rows.Next() {
		var typeID, qty int64
		if err := rows.Scan(&typeID, &qty); err != nil {
			rows.Close()
			rlogf("rolebots: hangar ore scan char=%d: %v", b.CharID, err)
			return
		}
		if qty > 0 {
			types = append(types, typeID)
			total += qty
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		rlogf("rolebots: hangar ore char=%d: %v", b.CharID, err)
		return
	}
	if len(types) == 0 {
		return
	}

	// 1. Refine it, if this counter reprocesses and it pays here. refineHangarOre
	// books the flow; listRefinedMinerals carries the sale and trade-book entry.
	if worth, yield, why := m.miningRefineWorth(ctx, b, refineWorthEdge); worth {
		if unitsIn, got, rerr := m.refineHangarOre(ctx, b); rerr == nil && unitsIn > 0 && len(got) > 0 {
			listed, _, held, lerr := m.listRefinedMinerals(ctx, b, got,
				m.projectReservations(ctx, b), mineralHoldMaxSec)
			if lerr != nil {
				rlogf("rolebots: hangar ore list char=%d: %v", b.CharID, lerr)
			}
			m.logBot(b.CharID, logInfo, "market",
				"refined %d unit(s) of hangar ore at %s — %.0f%% yield makes it pay (%s); "+
					"%d mineral lot(s) listed, %d held back",
				unitsIn, m.stationLabel(ctx, b.StationID), yield*100, why, listed, held)
			// Whatever did not make a whole batch is still ore and is handled by the next
			// sweep, not listed at a price the refine declined.
			return
		}
	}

	// 2. List it, if a listing is worth an order slot here (oreask.go).
	// 3. Dump it otherwise: a bad price beats a pile no market can see.
	listedTypes, sold := 0, int64(0)
	for _, typeID := range types {
		if moved, _, lerr := m.listOreStack(ctx, b, typeID); lerr != nil {
			m.logBot(b.CharID, logError, "market", "listing hangar ore: %v", lerr)
		} else if moved > 0 {
			listedTypes++
			continue
		}
		took, _, err := m.sellOreStack(ctx, b, typeID)
		if err != nil {
			rlogf("rolebots: hangar ore sale char=%d type=%d: %v", b.CharID, typeID, err)
			continue
		}
		sold += took
	}
	if listedTypes == 0 && sold == 0 {
		// Nothing moved, and the stack-level paths have already said why; repeating it
		// would be a line an hour per pilot.
		return
	}
	m.logBot(b.CharID, logInfo, "market",
		"cleared the hangar ore at %s — %d type(s) listed, %d unit(s) sold into the bid",
		m.stationLabel(ctx, b.StationID), listedTypes, sold)
}

func init() {
	registerCharForget("hangarOreAt", func(id int64) {
		hangarOreMu.Lock()
		delete(hangarOreAt, id)
		hangarOreMu.Unlock()
	})
}
