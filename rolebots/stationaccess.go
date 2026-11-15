package rolebots

import (
	"context"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/syncx"
)

// Bots keep to a station owner's doors. The core refuses a player at Dock (docking.go) through the
// owner's access rules (evedb.StationServiceRefusal); a bot docks by writing rows, so the module asks
// the same question itself: before a trip ends at a station (beginTravel, travelDockStep), in every
// station search (stationFilterPasses), and for clones, respawns and office rent. Only a station with a
// rule that can refuse anyone is restricted (stationRestricted); with none, every check is free.

const (
	// stationRestrictedEvery is how often the restricted-station set is read again.
	stationRestrictedEvery = 5 * time.Minute
	// stationRefusedTTL is how long one pilot's refused list stands.
	stationRefusedTTL = 10 * time.Minute
)

// stationAccessMod is the module the station-search filter reads through (stationFilterPasses is
// pure in its arguments and has no handle of its own). Set at start.
var stationAccessMod atomic.Pointer[Mod]

// stationRestrictedSet is the stations with a rule that can refuse someone, and when it was read.
type stationRestrictedSet struct {
	ids map[int64]bool
	at  time.Time
}

var stationRestricted atomic.Pointer[stationRestrictedSet]

// stationRefusedEntry is one pilot's refused stations, by service-agnostic docking, and when read.
type stationRefusedEntry struct {
	ids []int64
	at  time.Time
}

var stationRefusedByChar syncx.Map[int64, stationRefusedEntry]

// forgetStationAccess drops the cached sets after an owner changes its doors.
func forgetStationAccess() {
	stationRestricted.Store(nil)
	stationRefusedByChar.Clear()
}

// restrictedStations is the stations with a rule that can refuse someone: a minimum standing above
// -10 or a security bound inside -10..10 on any service.
func (m *Mod) restrictedStations(ctx context.Context) map[int64]bool {
	if cur := stationRestricted.Load(); cur != nil && clock.Now().Sub(cur.at) < stationRestrictedEvery {
		return cur.ids
	}
	ids := map[int64]bool{}
	if rows, err := int64Column(ctx, m, `
		SELECT DISTINCT stationID FROM stationServiceAccessRules
		 WHERE minimumStanding > -10 OR minimumCharSecurity > -10 OR maximumCharSecurity < 10`); err == nil {
		for _, id := range rows {
			ids[id] = true
		}
	}
	stationRestricted.Store(&stationRestrictedSet{ids: ids, at: clock.Now()})
	return ids
}

// stationRefuses reports whether a station's owner refuses a pilot a service.
func (m *Mod) stationRefuses(ctx context.Context, stationID, charID, serviceID int64) bool {
	if stationID == 0 || charID == 0 || !m.restrictedStations(ctx)[stationID] {
		return false
	}
	return evedb.StationServiceRefusal(ctx, m.db, stationID, charID, serviceID) != ""
}

// stationDockRefused reports whether a station's owner refuses a pilot docking.
func (m *Mod) stationDockRefused(ctx context.Context, stationID, charID int64) bool {
	return m.stationRefuses(ctx, stationID, charID, evedb.StationServiceDocking)
}

// stationsRefusing is every restricted station that refuses a pilot docking, cached per pilot for
// stationRefusedTTL.
func (m *Mod) stationsRefusing(ctx context.Context, charID int64) []int64 {
	restricted := m.restrictedStations(ctx)
	if len(restricted) == 0 || charID == 0 {
		return nil
	}
	if e, ok := stationRefusedByChar.Load(charID); ok && clock.Now().Sub(e.at) < stationRefusedTTL {
		return e.ids
	}
	var ids []int64
	for st := range restricted {
		if evedb.StationServiceRefusal(ctx, m.db, st, charID, evedb.StationServiceDocking) != "" {
			ids = append(ids, st)
		}
	}
	stationRefusedByChar.Store(charID, stationRefusedEntry{ids: ids, at: clock.Now()})
	return ids
}

// refusedStationClause is the station-search clause that keeps a pilot out of stations whose owner
// refuses it docking: "" when there are none.
func refusedStationClause(charID int64) (string, []any) {
	m := stationAccessMod.Load()
	if m == nil || charID == 0 {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ids := m.stationsRefusing(ctx, charID)
	if len(ids) == 0 {
		return "", nil
	}
	return ` AND s.stationID NOT IN (` + placeholders(len(ids)) + `)`, int64Args(ids)
}

// stationOrSpace names a station, or "staying in space" for 0.
func stationOrSpace(ctx context.Context, m *Mod, stationID int64) string {
	if stationID == 0 {
		return "staying in space"
	}
	return stationNameOf(ctx, m, stationID)
}

// stationInstead is a station in the same system that lets the pilot dock, the lowest ID first; 0
// when there is none.
func (m *Mod) stationInstead(ctx context.Context, b *botRun, refused int64) int64 {
	sys := m.stationSystemID(ctx, refused)
	if sys == 0 {
		return 0
	}
	ids, _ := int64Column(ctx, m, `SELECT stationID FROM staStations WHERE solarSystemID = ? AND stationID <> ? ORDER BY stationID`,
		sys, refused)
	for _, st := range ids {
		if !m.stationDockRefused(ctx, st, b.CharID) {
			return st
		}
	}
	return 0
}
