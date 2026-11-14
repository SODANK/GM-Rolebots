package rolebots

// One read of a pilot's skill sheet, shared by everything that asks for it in
// the same breath.
//
// evedb.GetCharacterSkillLevels is called from many places in one planning pass
// and is a join over entity and entity_attributes. A timeout on it halts a pilot
// with no economic cause, so a stale sheet answers a timeout. The cache is a TTL
// keyed on the character, not a per-pass map; forgetCharSkills is called where a
// skill is injected. The singular GetCharacterSkillLevel is not served from this
// map: the plural COALESCEs an unset level to 0, the singular to 1, so it has its
// own sheet (charSkillLevelFor).

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// charSkillsTTL is how long one sheet is reused. Skills complete on the core's
// tick and are noticed on the next planning pass, minutes apart, so a minute
// collapses a pass's repeated reads without hiding anything.
const charSkillsTTL = 60 * time.Second

type charSkillRec struct {
	at     time.Time
	levels map[int64]int
}

var (
	charSkillsMu sync.RWMutex
	charSkills   = map[int64]charSkillRec{}
)

// charSkillsFor is the read. The returned map is shared and must not be
// mutated by a caller; every consumer reads it and none writes to it.
func (m *Mod) charSkillsFor(ctx context.Context, charID int64) (map[int64]int, error) {
	if charID == 0 {
		return nil, nil
	}
	charSkillsMu.RLock()
	rec, ok := charSkills[charID]
	charSkillsMu.RUnlock()
	if ok && clock.Now().Sub(rec.at) < charSkillsTTL {
		return rec.levels, nil
	}

	levels, err := evedb.GetCharacterSkillLevels(ctx, m.db, charID)
	if err != nil {
		// A stale sheet beats a refusal: the failure absorbed here is a timeout, and
		// the older map is still right. With nothing cached the error travels on.
		if ok {
			return rec.levels, nil
		}
		return nil, err
	}
	charSkillsMu.Lock()
	charSkills[charID] = charSkillRec{at: clock.Now(), levels: levels}
	charSkillsMu.Unlock()
	return levels, nil
}

// forgetCharSkills drops one pilot's sheet. Called wherever the module changes
// what a character knows, so a purchase is visible on the next read.
func forgetCharSkills(charID int64) {
	charSkillsMu.Lock()
	delete(charSkills, charID)
	delete(charSkillRaw, charID)
	charSkillsMu.Unlock()
}

// ── the singular lookup ─────────────────────────────────────────────────────
//
// evedb.GetCharacterSkillLevel is not served from charSkillsFor: an injected
// skill with no level attribute reads 1 here and is missing there. This sheet
// is one read of every skill the pilot holds with COALESCE(level, 1); an absent
// skill is 0, as the singular answers. Same TTL and keep-the-old-sheet-on-timeout
// rule, cleared by forgetCharSkills.

var charSkillRaw = map[int64]charSkillRec{}

// charSkillLevelFor is evedb.GetCharacterSkillLevel through the sheet.
func (m *Mod) charSkillLevelFor(ctx context.Context, charID, skillID int64) (int, error) {
	if charID == 0 {
		return 0, nil
	}
	charSkillsMu.RLock()
	rec, ok := charSkillRaw[charID]
	charSkillsMu.RUnlock()
	if ok && clock.Now().Sub(rec.at) < charSkillsTTL {
		return rec.levels[skillID], nil
	}
	levels, err := loadRawSkillSheet(ctx, m.db, charID)
	if err != nil {
		if ok {
			return rec.levels[skillID], nil
		}
		return 0, fmt.Errorf("GetCharacterSkillLevel: %w", err)
	}
	charSkillsMu.Lock()
	charSkillRaw[charID] = charSkillRec{at: clock.Now(), levels: levels}
	charSkillsMu.Unlock()
	return levels[skillID], nil
}

// loadRawSkillSheet is GetCharacterSkillLevel's query for every skill at once.
// Two rows of one skill (an injected book beside a trained one) take the
// higher level.
func loadRawSkillSheet(ctx context.Context, db *sql.DB, charID int64) (map[int64]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.typeID, MAX(COALESCE(ea.valueInt, 1))
		FROM entity e
		LEFT JOIN entity_attributes ea ON ea.itemID = e.itemID AND ea.attributeID = 280
		WHERE e.ownerID = ? AND e.flag IN (7, 61)
		GROUP BY e.typeID`, charID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var typeID int64
		var lvl int
		if err := rows.Scan(&typeID, &lvl); err != nil {
			return nil, err
		}
		out[typeID] = lvl
	}
	return out, rows.Err()
}

// sweepCharSkills drops entries nothing has read for a while, so the map keyed
// by character does not keep every skill sheet in the game. Called from the
// cache sweep.
func sweepCharSkills() {
	cut := clock.Now().Add(-charSkillsTTL)
	charSkillsMu.Lock()
	for id, rec := range charSkills {
		if rec.at.Before(cut) {
			delete(charSkills, id)
		}
	}
	for id, rec := range charSkillRaw {
		if rec.at.Before(cut) {
			delete(charSkillRaw, id)
		}
	}
	charSkillsMu.Unlock()
}

func init() {
	registerCharForget("charSkills/charSkillRaw", forgetCharSkills)
	registerSweep("charSkills", sweepCharSkills)
}