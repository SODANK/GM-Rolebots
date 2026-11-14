package rolebots

import (
	"context"
	"time"

	evedb "gurimoon/db"
)

// The corporation's factory: the one office its industry runs out of. A
// corporation job needs the blueprint in the office's library division, the
// materials in its Materials division and an assembly line at the station, so
// the desk picks one office at a station with manufacturing lines, preferring
// the headquarters, then the most lines. With no office there is no factory and
// nothing on the desk runs until estate rents one (corpceo_estate.go).

// Corporation hangar divisions the desk uses, as entity flags. Division 1 is
// flag 4, where every corporation buy order delivers, so it is Materials.
const (
	corpDivMaterials = 4   // division 1
	corpDivLibrary   = 116 // division 2
	corpDivProduct   = 117 // division 3
	corpDivStore     = 118 // division 4
	// corpDivTrade is the Trade desk's shelf (corptrade.go): goods a member hauled
	// on the corporation's money, waiting to be listed. It is not the store's shelf,
	// which corpstore.go sells to members at cost-plus.
	corpDivTrade = 119 // division 5
)

// corpDivisionNames is what the desk names the divisions it uses, by number.
var corpDivisionNames = map[int]string{
	1: "Materials",
	2: "Blueprint Library",
	3: "Production",
	4: "Company Store",
	5: "Trade",
}

// deskDivisionNames is what a corporation's desks name, from its tags: the
// industry desk names all five; a Trade desk names 5; a mining or hauling outfit
// names 1 (where fleet ore, buy orders and deliveries arrive). Starbase
// divisions 6 and 7 are named by the starbase desk (posceo.go). Pure.
func deskDivisionNames(tags []string) map[int]string {
	if hasIndustryDesk(tags) {
		return corpDivisionNames
	}
	out := map[int]string{}
	if tradeDeskTagged(tags) {
		out[5] = corpDivisionNames[5]
	}
	for _, t := range tags {
		if t == "mining" || t == "hauling" {
			out[1] = corpDivisionNames[1]
		}
	}
	return out
}

// corpFactoryStation is the station of the corporation's factory office, 0 when
// it has no office at a station with manufacturing lines. The pinned office
// (factoryStationID, corpfacility.go) wins while it has lines, then the HQ, then
// the most lines. Line counts are correlated subqueries: this runs on every plan.
func (m *Mod) corpFactoryStation(ctx context.Context, corpID int64) int64 {
	// Two minutes per corporation (47 call sites, several on every
	// builder's plan); a pin or an office change forgets it (forgetCorpFactory).
	id, _ := snapshotFor(corpFactoryKey(corpID), 2*time.Minute, func() (int64, error) {
		return m.readCorpFactoryStation(ctx, corpID), nil
	})
	return id
}

func corpFactoryKey(corpID int64) string { return snapKey("corp-factory", corpID) }

// forgetCorpFactory drops the shared factory answer after the desk is
// pinned or an office changes hands.
func forgetCorpFactory(corpID int64) {
	snapMu.Lock()
	delete(snapCache, corpFactoryKey(corpID))
	snapMu.Unlock()
}

// corpIDsWhere runs a one-column query and answers its IDs as a set: the shape
// of every who-has-one snapshot on the pilot path.
func (m *Mod) corpIDsWhere(ctx context.Context, q string, args ...any) (map[int64]bool, error) {
	rows, err := m.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil && id != 0 {
			out[id] = true
		}
	}
	return out, rows.Err()
}

func (m *Mod) readCorpFactoryStation(ctx context.Context, corpID int64) int64 {
	var stationID int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT o.stationID
		  FROM crpoffices o
		  JOIN corporation c ON c.corporationID = o.corporationID
		  LEFT JOIN rolebot_corp_policy p ON p.corporationID = o.corporationID
		 WHERE o.corporationID = ?
		   AND EXISTS (SELECT 1 FROM ramassemblylines l WHERE l.containerID = o.stationID AND l.activityID = 1)
		 ORDER BY (o.stationID = COALESCE(p.factoryStationID, 0)) DESC,
		          (o.stationID = COALESCE(c.stationID, 0)) DESC,
		          (SELECT COUNT(*) FROM ramassemblylines l WHERE l.containerID = o.stationID AND l.activityID = 1) DESC,
		          o.stationID
		 LIMIT 1`, corpID).Scan(&stationID)
	return stationID
}

// corpFactoryOffice is the factory's station and office item, zeros when the
// corporation has no factory.
func (m *Mod) corpFactoryOffice(ctx context.Context, corpID int64) (stationID, officeID int64) {
	stationID = m.corpFactoryStation(ctx, corpID)
	if stationID == 0 {
		return 0, 0
	}
	officeID, _, _ = evedb.GetCorpOfficeAt(ctx, m.db, corpID, stationID)
	if officeID == 0 || officeID == stationID {
		// No office, or a corporation that owns the station itself: not a hangar the
		// desk can stock.
		return 0, 0
	}
	return stationID, officeID
}
