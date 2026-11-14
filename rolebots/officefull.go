package rolebots

import (
	"context"

	evedb "gurimoon/db"
	"gurimoon/syncx"
)

// Full stations: a CEO never tries to rent at a station with every office slot taken.
//
// Each office hunt picks its candidates, and a full station at the top of a list would
// be tried again on every pass (evedb.ErrNoFreeOffices). The pickers drop full stations
// (rankEstate, rankFacilities, pickBranchSite) and ceoTryRentOffice refuses one before
// it touches the wallet. A refusal from RentOffice itself (a slot the owner made
// private) marks the station full for officeFullRefusedSec.

const (
	// officeFullCacheSec is how long a slot count stays fresh: hunts run daily, so
	// a station freed by a release is seen on the next one.
	officeFullCacheSec = 600
	// officeFullRefusedSec is how long a station that refused a rent stays full.
	officeFullRefusedSec = 6 * 3600
)

type officeFullMemo struct {
	full  bool
	until int64
}

var officeFullAt syncx.Map[int64, officeFullMemo]

// stationOfficesFull reports whether a renter would find no free office slot at
// stationID: offices in use at or over the station type's capacity, or a recent refusal.
func (m *Mod) stationOfficesFull(ctx context.Context, stationID int64) bool {
	now := clock.Unix()
	if v, ok := officeFullAt.Load(stationID); ok && now < v.until {
		return v.full
	}
	capacity := evedb.StationOfficeCapacity(ctx, m.db, stationID)
	var used int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM crpoffices WHERE stationID = ?`, stationID).Scan(&used); err != nil {
		return false
	}
	full := officesFull(used, capacity)
	officeFullAt.Store(stationID, officeFullMemo{full: full, until: now + officeFullCacheSec})
	return full
}

// officesFull is the slot test. Pure.
func officesFull(used, capacity int64) bool {
	return capacity > 0 && used >= capacity
}

// noteOfficesFull marks a station full after RentOffice refused it.
func noteOfficesFull(stationID int64) {
	officeFullAt.Store(stationID, officeFullMemo{full: true, until: clock.Unix() + officeFullRefusedSec})
}

// forgetOfficesFull drops the memo after an office opens or closes at a station.
func forgetOfficesFull(stationID int64) {
	officeFullAt.Delete(stationID)
}

// markFullStations sets Full on every candidate where the corporation has no office
// and none is free.
func (m *Mod) markFullStations(ctx context.Context, cands []estateCandidate) {
	for i := range cands {
		if !cands[i].HasOffice {
			cands[i].Full = m.stationOfficesFull(ctx, cands[i].StationID)
		}
	}
}
