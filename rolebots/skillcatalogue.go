package rolebots

// The skill catalogue, split in two.
//
// evedb.ESPGetSkillCatalogue joins every published skill type to three
// dgmTypeAttributes rows and to the pilot's own skill items on each call. The
// type half never changes while the server runs, so it is read once per
// process. The pilot's half is one indexed read of its entity rows, merged
// in Go into the same rows the core hands out.

import (
	"context"
	"sync"

	evedb "gurimoon/db"
)

var (
	skillCatMu     sync.RWMutex
	skillCatStatic []evedb.ESPSkillCatalogueRow // Known/Level/SkillPoints zero
)

// resetSkillCatalogue forgets the static half (the harness; resetStatics).
func resetSkillCatalogue() {
	skillCatMu.Lock()
	skillCatStatic = nil
	skillCatMu.Unlock()
}

// skillCatalogueStatic is every published skill type with its rank and
// learning attributes, in the core's order, read once.
func (m *Mod) skillCatalogueStatic(ctx context.Context) ([]evedb.ESPSkillCatalogueRow, error) {
	skillCatMu.RLock()
	rows := skillCatStatic
	skillCatMu.RUnlock()
	if rows != nil {
		return rows, nil
	}
	q, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, COALESCE(t.description, ''),
		       g.groupID, g.groupName,
		       COALESCE(NULLIF(tc.valueFloat, 0), tc.valueInt, 1),
		       COALESCE(NULLIF(pa.valueFloat, 0), pa.valueInt, 0),
		       COALESCE(NULLIF(sa.valueFloat, 0), sa.valueInt, 0)
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 16
		LEFT JOIN dgmTypeAttributes tc ON tc.typeID = t.typeID AND tc.attributeID = 275
		LEFT JOIN dgmTypeAttributes pa ON pa.typeID = t.typeID AND pa.attributeID = 180
		LEFT JOIN dgmTypeAttributes sa ON sa.typeID = t.typeID AND sa.attributeID = 181
		WHERE t.published = 1
		ORDER BY g.groupName, t.typeName`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var out []evedb.ESPSkillCatalogueRow
	for q.Next() {
		var r evedb.ESPSkillCatalogueRow
		var rank, pri, sec float64
		if err := q.Scan(&r.TypeID, &r.TypeName, &r.Description, &r.GroupID, &r.GroupName,
			&rank, &pri, &sec); err != nil {
			return nil, err
		}
		r.Rank = int64(rank)
		if r.Rank < 1 {
			r.Rank = 1
		}
		r.PrimaryAttrID = int64(pri)
		r.SecondaryAttrID = int64(sec)
		out = append(out, r)
	}
	if err := q.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []evedb.ESPSkillCatalogueRow{}
	}
	skillCatMu.Lock()
	skillCatStatic = out
	skillCatMu.Unlock()
	return out, nil
}

// skillCatalogueFor is evedb.ESPGetSkillCatalogue's answer for one pilot: the
// static rows with Known / Level / SkillPoints filled from the pilot's own
// skill items (flags 7 and 61; attribute 280 the level, 276 the points). A
// fresh slice every call, so the planner may keep it.
func (m *Mod) skillCatalogueFor(ctx context.Context, charID int64) ([]evedb.ESPSkillCatalogueRow, error) {
	static, err := m.skillCatalogueStatic(ctx)
	if err != nil {
		return nil, err
	}
	type owned struct {
		level int
		sp    int64
	}
	have := map[int64]owned{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, COALESCE(lv.valueInt, 0), COALESCE(sp.valueInt, 0)
		  FROM entity e
		  LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		  LEFT JOIN entity_attributes sp ON sp.itemID = e.itemID AND sp.attributeID = 276
		 WHERE e.ownerID = ? AND e.flag IN (7, 61)`, charID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var typeID, sp int64
		var level int
		if rows.Scan(&typeID, &level, &sp) != nil {
			continue
		}
		if cur, ok := have[typeID]; !ok || level > cur.level {
			have[typeID] = owned{level: level, sp: sp}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]evedb.ESPSkillCatalogueRow, len(static))
	copy(out, static)
	for i := range out {
		if o, ok := have[out[i].TypeID]; ok {
			out[i].Known, out[i].Level, out[i].SkillPoints = true, o.level, o.sp
		}
	}
	return out, nil
}
