package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// Process-lifetime caches for the map lookups every gate jump makes. mapDenormalize, mapJumps
// and mapSolarSystems never change while the server runs, so a lookup answered once is the
// answer for good. "No such row" is an answer and is kept; a failed query is not, so the next
// caller asks the database again. Each cache is keyed exactly as the query was, and holds what
// the query returned, so a caller sees the same value it would have read.

type gatePairKey struct{ inSys, toSys int64 }

type gateGeom struct {
	gate, src destiny.Vec3
	radius    float64
	ok        bool
}

type anyGate struct {
	itemID          int64
	x, y, z, radius float64
	ok              bool
}

type sysLocation struct{ constID, regionID int64 }

var (
	gatePairCache  sync.Map // gatePairKey -> int64 (0 = not adjacent)
	gateGeomCache  sync.Map // gateID -> gateGeom
	anyGateCache   sync.Map // solarSystemID -> anyGate
	sysLocCache    sync.Map // solarSystemID -> sysLocation
	typeByNameMu   sync.Mutex
	typeGroupByNam = map[string]typeGroupHit{}
)

type typeGroupHit struct {
	groupID int64
	ok      bool
}

// cachedGateBetween is the gateBetween query, cached. found is false on a query error.
func cachedGateBetween(ctx context.Context, db *sql.DB, inSys, toSys int64) (int64, bool) {
	k := gatePairKey{inSys, toSys}
	if v, ok := gatePairCache.Load(k); ok {
		return v.(int64), true
	}
	var gateID int64
	err := db.QueryRowContext(ctx, `
		SELECT d.itemID
		  FROM mapDenormalize d
		  JOIN mapJumps mj ON mj.stargateID = d.itemID
		  JOIN mapDenormalize src ON src.itemID = mj.celestialID
		 WHERE d.solarSystemID = ? AND src.solarSystemID = ?
		 LIMIT 1`, inSys, toSys).Scan(&gateID)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		gateID = 0
	default:
		return 0, false
	}
	gatePairCache.Store(k, gateID)
	return gateID, true
}

// cachedGateArrivalGeom is the gateArrivalGeom query, cached: the gate's position and radius and
// its paired gate's position. ok is false when the gate has no pair; err is non-nil on a failure.
func cachedGateArrivalGeom(ctx context.Context, db *sql.DB, gateID int64) (gateGeom, error) {
	if v, ok := gateGeomCache.Load(gateID); ok {
		return v.(gateGeom), nil
	}
	var g gateGeom
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(d.x,0), COALESCE(d.y,0), COALESCE(d.z,0), COALESCE(d.radius,1),
		       COALESCE(src.x,0), COALESCE(src.y,0), COALESCE(src.z,0)
		  FROM mapDenormalize d
		  JOIN mapJumps mj ON mj.stargateID = d.itemID
		  JOIN mapDenormalize src ON src.itemID = mj.celestialID
		 WHERE d.itemID = ?
		 LIMIT 1`, gateID).Scan(&g.gate.X, &g.gate.Y, &g.gate.Z, &g.radius, &g.src.X, &g.src.Y, &g.src.Z)
	switch {
	case err == nil:
		g.ok = true
	case errors.Is(err, sql.ErrNoRows):
		g = gateGeom{}
	default:
		return gateGeom{}, err
	}
	gateGeomCache.Store(gateID, g)
	return g, nil
}

// cachedAnyGate is parkAtGate's fallback query, cached: some stargate in the system.
func cachedAnyGate(ctx context.Context, db *sql.DB, sysID int64) (anyGate, error) {
	if v, ok := anyGateCache.Load(sysID); ok {
		return v.(anyGate), nil
	}
	var g anyGate
	err := db.QueryRowContext(ctx, `
		SELECT itemID, COALESCE(x,0), COALESCE(y,0), COALESCE(z,0), COALESCE(radius,1)
		FROM mapDenormalize WHERE solarSystemID = ? AND groupID = 10 LIMIT 1`,
		sysID).Scan(&g.itemID, &g.x, &g.y, &g.z, &g.radius)
	switch {
	case err == nil:
		g.ok = true
	case errors.Is(err, sql.ErrNoRows):
		g = anyGate{}
	default:
		return anyGate{}, err
	}
	anyGateCache.Store(sysID, g)
	return g, nil
}

// cachedSolarSystemLocation is evedb.GetSolarSystemLocation, cached. Errors are not cached.
func cachedSolarSystemLocation(ctx context.Context, db *sql.DB, sysID int64) (constID, regionID int64, err error) {
	if v, ok := sysLocCache.Load(sysID); ok {
		l := v.(sysLocation)
		return l.constID, l.regionID, nil
	}
	constID, regionID, err = evedb.GetSolarSystemLocation(ctx, db, sysID)
	if err != nil {
		return 0, 0, err
	}
	sysLocCache.Store(sysID, sysLocation{constID, regionID})
	return constID, regionID, nil
}

// cachedGroupIDByTypeName is `SELECT groupID FROM invTypes WHERE typeName = ?`, cached. ok is
// false when no type has the name; err is non-nil on a failure (not cached).
func cachedGroupIDByTypeName(ctx context.Context, db *sql.DB, name string) (groupID int64, ok bool, err error) {
	typeByNameMu.Lock()
	h, hit := typeGroupByNam[name]
	typeByNameMu.Unlock()
	if hit {
		return h.groupID, h.ok, nil
	}
	err = db.QueryRowContext(ctx, `SELECT groupID FROM invTypes WHERE typeName = ?`, name).Scan(&groupID)
	switch {
	case err == nil:
		h = typeGroupHit{groupID: groupID, ok: true}
	case errors.Is(err, sql.ErrNoRows):
		h = typeGroupHit{}
	default:
		return 0, false, err
	}
	typeByNameMu.Lock()
	typeGroupByNam[name] = h
	typeByNameMu.Unlock()
	return h.groupID, h.ok, nil
}
