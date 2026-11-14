package rolebots

import (
	"context"
	"fmt"
	"sync"
)

// The chargen schools: where the roster is born.
//
// Character creation puts a pilot in its school corporation's headquarters
// station with its clone installed there. Left alone, every bot keeps that
// station as its home. This file knows which stations and systems are schools,
// so pilot-facing paths can push a pilot out of one.
//
// A corporation is a school if either chrSchools (db.GetSchoolInfo) or the
// chargen activity on crpNPCCorporations (corpjoin.go) says so. The data is
// static and loads once on first use; a load failure means no schools known.

// schoolMap holds the school corporations, every station they own, and the
// systems of their headquarters only. Do not merge the two sets.
// See docs/comment-notes-b17.md#school-map.
type schoolMap struct {
	Corps    map[int64]bool
	Stations map[int64]bool
	Systems  map[int64]bool
}

var (
	schoolsOnce sync.Once
	schoolsData *schoolMap
	schoolsErr  error
)

// schools loads the school corporations, stations and systems.
func (m *Mod) schools(ctx context.Context) (*schoolMap, error) {
	schoolsOnce.Do(func() {
		s := &schoolMap{
			Corps:    map[int64]bool{},
			Stations: map[int64]bool{},
			Systems:  map[int64]bool{},
		}
		rows, err := m.db.QueryContext(ctx, fmt.Sprintf(`
			SELECT c.corporationID, COALESCE(c.stationID, 0),
			       COALESCE(st.solarSystemID, 0)
			  FROM corporation c
			  LEFT JOIN staStations st ON st.stationID = c.stationID
			 WHERE c.corporationID IN (SELECT corporationID FROM chrSchools)
			    OR c.corporationID IN (SELECT corporationID FROM crpNPCCorporations
			                            WHERE mainActivityID = %d)`, corpActivitySchool))
		if err != nil {
			schoolsErr = fmt.Errorf("schools: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var corpID, stationID, sysID int64
			if err := rows.Scan(&corpID, &stationID, &sysID); err != nil {
				schoolsErr = err
				return
			}
			s.Corps[corpID] = true
			if stationID != 0 {
				s.Stations[stationID] = true
			}
			if sysID != 0 {
				s.Systems[sysID] = true
			}
		}
		if err := rows.Err(); err != nil {
			schoolsErr = err
			return
		}

		// Every other station these corporations own, in a separate query keyed on the
		// station (the first query is keyed on the corporation).
		if len(s.Corps) > 0 {
			ids := make([]int64, 0, len(s.Corps))
			for id := range s.Corps {
				ids = append(ids, id)
			}
			err = forEachChunk(ids, func(where string, args []any) error {
				r, qerr := m.db.QueryContext(ctx,
					`SELECT stationID FROM staStations WHERE corporationID IN `+where, args...)
				if qerr != nil {
					return fmt.Errorf("schools stations: %w", qerr)
				}
				defer r.Close()
				for r.Next() {
					var stationID int64
					if serr := r.Scan(&stationID); serr != nil {
						return serr
					}
					s.Stations[stationID] = true
				}
				return r.Err()
			})
			if err != nil {
				schoolsErr = err
				return
			}
		}
		schoolsData = s
	})
	if schoolsErr != nil {
		return nil, schoolsErr
	}
	return schoolsData, nil
}

// schoolsSoft is the best-effort read: an empty map rather than an error,
// for the callers where not knowing about schools is a missed nudge and
// nothing worse. Never nil.
func (m *Mod) schoolsSoft(ctx context.Context) *schoolMap {
	s, err := m.schools(ctx)
	if err != nil || s == nil {
		return &schoolMap{
			Corps:    map[int64]bool{},
			Stations: map[int64]bool{},
			Systems:  map[int64]bool{},
		}
	}
	return s
}
