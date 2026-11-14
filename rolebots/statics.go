package rolebots

// Process-wide memory of static rows: a station's system, a type's name, a system's name. They never
// change under a running server, are read by primary key, and are asked for on nearly every bot step.
// The maps hold the answer for the life of the process, like staticTypeInfo (fitstats.go).
// A miss is never cached: a harness test creates its stand-in rows after the first lookup could have
// run, and a bad ID is rare. Character names can go stale (a purged roster slot is regenerated under
// a new name), so that map is registered for forget. testDB resets all of them (resetStatics).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

type stationRec struct {
	SystemID, RegionID, CorpID int64
	Name                       string
}

type typeRec struct {
	Name                    string
	GroupID                 int64
	Mass, Volume, BasePrice float64
}

var (
	stationStatic sync.Map // stationID to stationRec
	typeStatic    sync.Map // typeID to typeRec
	systemNames   sync.Map // solarSystemID to name
	regionNames   sync.Map // regionID to name
	charNameCache sync.Map // characterID to name
)

func init() {
	registerCharForget("charNameCache", func(id int64) { charNameCache.Delete(id) })
}

// resetStatics forgets every static row, for the harness.
func resetStatics() {
	for _, mp := range []*sync.Map{&stationStatic, &typeStatic, &systemNames, &regionNames, &charNameCache} {
		mp.Range(func(k, _ any) bool { mp.Delete(k); return true })
	}
	resetSkillCatalogue()
}

// stationStaticRec is the station's static row; ok is false when the station does not exist (or
// the read failed).
func (m *Mod) stationStaticRec(ctx context.Context, stationID int64) (stationRec, bool) {
	if stationID == 0 {
		return stationRec{}, false
	}
	if v, ok := stationStatic.Load(stationID); ok {
		return v.(stationRec), true
	}
	var r stationRec
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(solarSystemID, 0), COALESCE(regionID, 0), COALESCE(corporationID, 0),
		       COALESCE(stationName, '')
		  FROM staStations WHERE stationID = ?`, stationID).
		Scan(&r.SystemID, &r.RegionID, &r.CorpID, &r.Name); err != nil {
		// A harness stand-in may carry only some columns, so the one-row read can fail. Read the columns
		// one at a time before giving up.
		if errors.Is(err, sql.ErrNoRows) {
			return stationRec{}, false
		}
		var one int64
		if m.db.QueryRowContext(ctx, `SELECT 1 FROM staStations WHERE stationID = ?`, stationID).Scan(&one) != nil {
			return stationRec{}, false
		}
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(solarSystemID, 0) FROM staStations WHERE stationID = ?`, stationID).Scan(&r.SystemID)
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(regionID, 0) FROM staStations WHERE stationID = ?`, stationID).Scan(&r.RegionID)
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(corporationID, 0) FROM staStations WHERE stationID = ?`, stationID).Scan(&r.CorpID)
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(stationName, '') FROM staStations WHERE stationID = ?`, stationID).Scan(&r.Name)
	}
	stationStatic.Store(stationID, r)
	return r, true
}

// stationSystemOf is the system a station sits in, 0 when unknown.
func (m *Mod) stationSystemOf(ctx context.Context, stationID int64) int64 {
	r, _ := m.stationStaticRec(ctx, stationID)
	return r.SystemID
}

// stationRegionOf is the region a station sits in, 0 when unknown.
func (m *Mod) stationRegionOf(ctx context.Context, stationID int64) int64 {
	r, _ := m.stationStaticRec(ctx, stationID)
	return r.RegionID
}

// stationNameCached is the station's name, "" when unknown.
func (m *Mod) stationNameCached(ctx context.Context, stationID int64) string {
	r, _ := m.stationStaticRec(ctx, stationID)
	return r.Name
}

// stationNameOr is the station's name, or "station <id>".
func (m *Mod) stationNameOr(ctx context.Context, stationID int64) string {
	if n := m.stationNameCached(ctx, stationID); n != "" {
		return n
	}
	return fmt.Sprintf("station %d", stationID)
}

// typeStaticRec is the type's static row; ok is false when the type does not exist.
func (m *Mod) typeStaticRec(ctx context.Context, typeID int64) (typeRec, bool) {
	return typeRecFrom(ctx, m.db, typeID)
}

func typeRecFrom(ctx context.Context, db *sql.DB, typeID int64) (typeRec, bool) {
	if typeID == 0 {
		return typeRec{}, false
	}
	if v, ok := typeStatic.Load(typeID); ok {
		return v.(typeRec), true
	}
	var r typeRec
	var mass, vol, base sql.NullFloat64
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(typeName, ''), COALESCE(groupID, 0), mass, volume, basePrice
		  FROM invTypes WHERE typeID = ?`, typeID).
		Scan(&r.Name, &r.GroupID, &mass, &vol, &base); err != nil {
		// Column by column for a harness stand-in that carries only some of
		// them (see stationStaticRec).
		if errors.Is(err, sql.ErrNoRows) {
			return typeRec{}, false
		}
		var one int64
		if db.QueryRowContext(ctx, `SELECT 1 FROM invTypes WHERE typeID = ?`, typeID).Scan(&one) != nil {
			return typeRec{}, false
		}
		_ = db.QueryRowContext(ctx, `SELECT COALESCE(typeName, '') FROM invTypes WHERE typeID = ?`, typeID).Scan(&r.Name)
		_ = db.QueryRowContext(ctx, `SELECT COALESCE(groupID, 0) FROM invTypes WHERE typeID = ?`, typeID).Scan(&r.GroupID)
		_ = db.QueryRowContext(ctx, `SELECT mass FROM invTypes WHERE typeID = ?`, typeID).Scan(&mass)
		_ = db.QueryRowContext(ctx, `SELECT volume FROM invTypes WHERE typeID = ?`, typeID).Scan(&vol)
		_ = db.QueryRowContext(ctx, `SELECT basePrice FROM invTypes WHERE typeID = ?`, typeID).Scan(&base)
	}
	r.Mass, r.Volume, r.BasePrice = mass.Float64, vol.Float64, base.Float64
	typeStatic.Store(typeID, r)
	return r, true
}

// typeNameOf is the type's name, "" when unknown.
func (m *Mod) typeNameOf(ctx context.Context, typeID int64) string {
	r, _ := m.typeStaticRec(ctx, typeID)
	return r.Name
}

// typeGroupOf is the type's group, 0 when unknown.
func (m *Mod) typeGroupOf(ctx context.Context, typeID int64) int64 {
	r, _ := m.typeStaticRec(ctx, typeID)
	return r.GroupID
}

// typeMassOf is invTypes.mass, 0 when unknown.
func (m *Mod) typeMassOf(ctx context.Context, typeID int64) float64 {
	r, _ := m.typeStaticRec(ctx, typeID)
	return r.Mass
}

// systemNameCached is the solar system's name, "" when unknown.
func (m *Mod) systemNameCached(ctx context.Context, sysID int64) string {
	if sysID == 0 {
		return ""
	}
	if v, ok := systemNames.Load(sysID); ok {
		return v.(string)
	}
	var s string
	if err := m.db.QueryRowContext(ctx,
		`SELECT solarSystemName FROM mapSolarSystems WHERE solarSystemID = ?`, sysID).Scan(&s); err != nil {
		return ""
	}
	systemNames.Store(sysID, s)
	return s
}

// regionNameCached is the region's name, "" when unknown.
func (m *Mod) regionNameCached(ctx context.Context, regionID int64) string {
	if regionID == 0 {
		return ""
	}
	if v, ok := regionNames.Load(regionID); ok {
		return v.(string)
	}
	var s string
	if err := m.db.QueryRowContext(ctx,
		`SELECT regionName FROM mapRegions WHERE regionID = ?`, regionID).Scan(&s); err != nil {
		return ""
	}
	regionNames.Store(regionID, s)
	return s
}

// charNameOf is the character's name, "" when unknown. Names do not change
// under a running server; a purge forgets the entry (forgetreg.go).
func (m *Mod) charNameOf(ctx context.Context, charID int64) string {
	if charID == 0 {
		return ""
	}
	if v, ok := charNameCache.Load(charID); ok {
		return v.(string)
	}
	var s string
	if err := m.db.QueryRowContext(ctx,
		`SELECT characterName FROM character_ WHERE characterID = ?`, charID).Scan(&s); err != nil || s == "" {
		// An NPC CEO has no character_ row, only its name (npcchair.go).
		if err := m.db.QueryRowContext(ctx,
			`SELECT itemName FROM evenames WHERE itemID = ?`, charID).Scan(&s); err != nil || s == "" {
			return ""
		}
	}
	charNameCache.Store(charID, s)
	return s
}
