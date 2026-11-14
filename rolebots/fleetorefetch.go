package rolebots

// The fleet-share fetch: a miner goes back for the rock a hauler handed it at a staging
// station it does not live at. rolebot_ore_hold is keyed (character, station) and
// disposed of only where the pilot is docked. The settlement round disposes of each
// member's share at staging (gangplan.go gangSettleStep) and disposeFleetOre carries home
// what it cannot list or sell, so this run is for shares already stranded: a fleet killed
// before settling, or a member that left early.
//
// It is the warehouse run (warehouse.go) narrowed to one kind of remote hoard, with no dial
// because the module created the hoard. It never hauls: the pilot flies to the station and
// the ordinary docked sweeps run there (warehouseStep, sweepHangarNow, disposeFleetOre).

import (
	"context"
	"sort"
	"sync"
)

const (
	// fleetOreFetchLookSec is how often one pilot bothers to look.
	fleetOreFetchLookSec = 3600
	// fleetOreFetchMinISK is the least a remote share must be worth (at
	// basePrice) to be worth the trip at zero jumps.
	fleetOreFetchMinISK = 100_000.0
	// fleetOreFetchMaxJumps is how far a pilot goes for its share, the same reach a solo
	// miner has for a buyer.
	fleetOreFetchMaxJumps = oreSellMaxJumps
)

var (
	fleetOreFetchMu sync.Mutex
	fleetOreFetchAt = map[int64]int64{} // charID to unix of last look
)

// fleetOreFetchRun takes the home-loop pass when a share worth the trip is
// waiting somewhere else. Reports whether it took it.
func (m *Mod) fleetOreFetchRun(ctx context.Context, b *botRun) (bool, error) {
	if b.StationID == 0 || b.ShipID == 0 {
		return false, nil
	}
	now := clock.Unix()
	fleetOreFetchMu.Lock()
	if now-fleetOreFetchAt[b.CharID] < fleetOreFetchLookSec {
		fleetOreFetchMu.Unlock()
		return false, nil
	}
	fleetOreFetchAt[b.CharID] = now
	fleetOreFetchMu.Unlock()

	by := map[int64]*warehouseStop{}
	if err := m.warehouseFleetStops(ctx, b, by, warehouseLarders(b)); err != nil {
		return false, err
	}
	if len(by) == 0 {
		return false, nil
	}
	stops := make([]warehouseStop, 0, len(by))
	for _, s := range by {
		jumps, err := m.jumpsBetween(ctx, b, b.SystemID, s.SystemID)
		if err != nil {
			continue // unroutable under this pilot's rules: not a stop for it
		}
		s.Jumps = jumps
		stops = append(stops, *s)
	}
	best, ok := pickFleetOreFetch(stops, fleetOreFetchMaxJumps, fleetOreFetchMinISK)
	if !ok {
		return false, nil
	}
	st := b.State
	st.Activity = ""
	st.Data.TargetStationID = best.StationID
	st.Data.TargetSystemID = best.SystemID
	m.logBot(b.CharID, logInfo, "home",
		"a mining fleet handed it %d units of rock at %s (%d jumps) that it never collected — going to dispose of it",
		best.Units, m.stationLabel(ctx, best.StationID), best.Jumps)
	return true, m.beginTravel(ctx, b, best.SystemID, best.StationID, string(TaskWarehouse))
}

// pickFleetOreFetch chooses the share worth going for: inside the jump
// reach, worth at least the floor, and then the best distance-discounted
// value (warehouseStop.Worth). Ties go to the lower stationID so the choice
// is stable. Pure.
func pickFleetOreFetch(stops []warehouseStop, maxJumps int, minISK float64) (warehouseStop, bool) {
	var out []warehouseStop
	for _, s := range stops {
		if s.Jumps < 0 || s.Jumps > maxJumps || s.Value < minISK {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return warehouseStop{}, false
	}
	sort.Slice(out, func(i, j int) bool {
		wi, wj := out[i].Worth(), out[j].Worth()
		if wi != wj {
			return wi > wj
		}
		return out[i].StationID < out[j].StationID
	})
	return out[0], true
}

func init() {
	registerCharForget("fleetOreFetchAt", func(id int64) {
		fleetOreFetchMu.Lock()
		delete(fleetOreFetchAt, id)
		fleetOreFetchMu.Unlock()
	})
}
