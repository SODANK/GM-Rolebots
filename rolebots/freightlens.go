package rolebots

import (
	"context"
	"sync"
)

// The fit lens an Infrastructure-arm pilot hauls under. A corporation's starbase kit
// ships as single units no hold can split (a Ship Maintenance Array or a Large control
// tower is 8,000 m3). The trade lens (JobTrade, fitgen.go) spends warpCoreWishes(lo) lows
// on stabilisers, which suits a courier but leaves a hold too small for the kit;
// JobFreight fills every low with an expander.
//
// The lens belongs to the pilot, like the PvP lens (pvplens.go): the arm that keeps
// starbases standing judges every hauling activity on lift. Any other pilot keeps the
// trade lens. A lifter whose hulls cannot work freight keeps JobTrade, so no pilot is left
// with no ship to want.

const (
	// freightLensTTLSec caches the verdict per pilot: it reads the run and a fit-pool probe,
	// and the planner asks several times a pass.
	freightLensTTLSec = 10 * 60
)

// freightActivity reports whether an activity's flying is hauling, the activities whose
// ship the freight lens replaces. Pure.
func freightActivity(activity string) bool {
	switch activity {
	case ActTrade, ActIndustry, ActCourier:
		return true
	}
	return false
}

// freightJobFor is the lens decision with its inputs already read. Pure. base is
// jobFor's answer; it comes back unchanged for any activity that is not hauling, any
// pilot outside the Infrastructure arm, and a lifter that cannot fly a freight fit.
func freightJobFor(base, activity string, inInfra, canFly bool) string {
	if !freightActivity(activity) || !inInfra || !canFly {
		return base
	}
	return JobFreight
}

type freightLensEntry struct {
	freight bool
	at      int64
}

var freightLensCache sync.Map // charID -> freightLensEntry

// freightLensFor reports whether this pilot hauls under the freight lens:
// it sits in its corporation's Infrastructure arm and some freight fit is
// open to it.
func (m *Mod) freightLensFor(ctx context.Context, charID int64) bool {
	now := clock.Unix()
	if v, ok := freightLensCache.Load(charID); ok {
		if e := v.(freightLensEntry); now-e.at < freightLensTTLSec {
			return e.freight
		}
	}
	freight := false
	if b, err := m.loadRun(ctx, charID); err == nil {
		if seat, ok := armOf(charID, b.CorpID); ok && seat.Arm == armInfrastructure {
			if ok, err := m.canWorkJob(ctx, charID, JobFreight); err == nil && ok {
				freight = true
			}
		}
	}
	freightLensCache.Store(charID, freightLensEntry{freight: freight, at: now})
	return freight
}

func init() {
	registerCharForget("freightLensCache", func(id int64) { freightLensCache.Delete(id) })
}
