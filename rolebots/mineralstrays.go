package rolebots

// Hangar minerals: the refined half of strays.go.
//
// A mineral reaches the market through one door, listRefinedMinerals, called by
// a pilot that just refined a hold, for what that refine made, at the station it
// stands in. strays.go skips minerals, so a stack left at another station, or a
// hoard beyond any project, was never listed. This sweep disposes of them as a
// refine would: what the pilot's own uninstalled job needs stays
// (projectReservations) and the surplus goes through listRefinedMinerals, which
// holds the only copy of the listing rules.

import (
	"context"
	"fmt"
	"sync"
)

// How often one pilot bothers to look at the minerals in the hangar it is
// standing in. Sized like hangarStraySweepSec: a pile grows on the scale of
// trips, not minutes, and the common case is a pilot with nothing to do here.
const hangarMineralSweepSec = 3600

var (
	hangarMineralMu sync.Mutex
	hangarMineralAt = map[int64]int64{} // charID to unix of last sweep
)

// sellHangarMinerals puts the surplus minerals in this pilot's hangar on the
// market. A cheap no-op for a hangar with none; throttled per pilot. Called from
// the same two seams as sellHangarStrays (the between-jobs planner and the
// home-loop shopping stop), where the pilot is docked with nothing committed.
func (m *Mod) sellHangarMinerals(ctx context.Context, b *botRun) {
	if b.StationID == 0 {
		return
	}
	now := clock.Unix()
	hangarMineralMu.Lock()
	if now-hangarMineralAt[b.CharID] < hangarMineralSweepSec {
		hangarMineralMu.Unlock()
		return
	}
	hangarMineralAt[b.CharID] = now
	hangarMineralMu.Unlock()

	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND e.singleton = 0
		  AND t.groupID = ?
		GROUP BY e.typeID`, b.CharID, b.StationID, groupMineral)
	if err != nil {
		rlogf("rolebots: hangar minerals char=%d: %v", b.CharID, err)
		return
	}
	got := map[int64]int64{}
	for rows.Next() {
		var typeID, qty int64
		if err := rows.Scan(&typeID, &qty); err != nil {
			rows.Close()
			rlogf("rolebots: hangar minerals scan char=%d: %v", b.CharID, err)
			return
		}
		if qty > 0 {
			got[typeID] = qty
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		rlogf("rolebots: hangar minerals char=%d: %v", b.CharID, err)
		return
	}
	if len(got) == 0 {
		return
	}

	// The ordinary listing path and age-out. The short clock (mineralHoldSecUnder)
	// belongs to a trip flown to fill a regional shelf; this sweep is not a trip.
	listed, kept, held, err := m.listRefinedMinerals(ctx, b, got,
		m.projectReservations(ctx, b), mineralHoldMaxSec)
	if err != nil {
		rlogf("rolebots: hangar minerals list char=%d: %v", b.CharID, err)
		return
	}
	if listed == 0 {
		// Nothing moved. Held stacks already log inside the listing path.
		return
	}
	m.logBot(b.CharID, logInfo, "market", fmt.Sprintf(
		"minerals were piling up in the hangar — put %d line(s) on the market%s%s",
		listed,
		map[bool]string{true: " (holding back what its own job needs)"}[kept > 0],
		map[bool]string{true: fmt.Sprintf(", %d stack(s) still short of a listable lot", held)}[held > 0]))
}

func init() {
	registerCharForget("hangarMineralAt", func(id int64) {
		hangarMineralMu.Lock()
		delete(hangarMineralAt, id)
		hangarMineralMu.Unlock()
	})
}
