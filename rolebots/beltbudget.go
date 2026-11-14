package rolebots

import (
	"context"
	"fmt"
	"time"

	"gurimoon/destiny"
)

// Belt depletion, as the world sees it.
//
// The abstract tier reads the real field (beltfield.go): destiny seeds one per system and db/asteroiddb.go
// persists the seed plus a sparse row per mined rock. What is left here is the search's half:
//
//	systemFullnessMap   how worked-over each system is, for the belt walk
//	beltWithMostOre     which belt in a system to fly to
//
// There is no regeneration timer: the world's regrowth is asteroids.regenDays (the hourly expiry sweep
// re-rolls a seed and clears depletion), and a second timer would grow belts on a schedule they do not keep.

const (
	// beltDepletionMaxCost is what a completely stripped system costs the belt search, in jumps (the units
	// of scoring.go). It is steep enough to push a bot past several intact systems but not a wall.
	beltDepletionMaxCost = 14

	// beltFieldSelect matches a recording to its field by seed, not by age. A reseed rolls a new seed and
	// clears depletion together, so the join drops stale rows and the system reads untouched, as a reseed
	// leaves it. A staleness timer is wrong in both directions.
	beltFieldSelect = `
		SELECT b.solarSystemID, b.m3Left, b.m3Total
		  FROM rolebot_belt_field b
		  JOIN asteroidFields f
		    ON f.solarSystemID = b.solarSystemID AND f.seed = b.seed
		 WHERE b.m3Total > 0`
)

// recordBeltFieldTotals writes what a reconstructed field holds against what its seed put there, for the
// belt search to read: rebuilding a field per system is too costly for findMiningTargetIn. A system with
// no row reads as untouched. Live fields are not recorded, since their scan cannot see mined-out rocks.
func (m *Mod) recordBeltFieldTotals(ctx context.Context, f *beltField) {
	if f == nil || f.Live || f.SysID == 0 {
		return
	}
	left, full := f.totalM3()
	if full <= 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_belt_field
			(solarSystemID, seed, m3Total, m3Left, checkedAt)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			seed = VALUES(seed), m3Total = VALUES(m3Total),
			m3Left = VALUES(m3Left), checkedAt = VALUES(checkedAt)`,
		f.SysID, f.Seed, full, left, clock.Unix()); err != nil {
		// Losing a totals row costs the search one system's worth of
		// preference, which is not worth failing a mining pass over.
		m.logBot(0, logError, "mining", "recording belt field for system %d: %v", f.SysID, err)
	}
}

// systemBeltFullness is how much ore one system has left, 0..1, where 1 is untouched. The planner reads
// the whole map (systemFullnessMap); this single-system form is what the harness asserts against.
func (m *Mod) systemBeltFullness(ctx context.Context, sysID int64) float64 {
	var sys int64
	var left, total float64
	err := m.db.QueryRowContext(ctx,
		beltFieldSelect+` AND b.solarSystemID = ?`, sysID).Scan(&sys, &left, &total)
	if err != nil || total <= 0 {
		return 1
	}
	return clampFullness(left / total)
}

// systemFullnessMap reads fullness for every system the roster has looked at, in one query, shared
// across the roster for a minute. A failure is reported and swallowed, not returned: an unreadable
// recording costs the search one preference, while an error would stop every miner planning.
func (m *Mod) systemFullnessMap(ctx context.Context) (map[int64]float64, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	shared, err := snapshotFor(snapKey("belt-field"), time.Minute, func() (map[int64]float64, error) {
		out := map[int64]float64{}
		rows, err := m.db.QueryContext(sctx, beltFieldSelect)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var sysID int64
			var left, total float64
			if err := rows.Scan(&sysID, &left, &total); err != nil {
				return nil, fmt.Errorf("systemFullnessMap: %w", err)
			}
			if total <= 0 {
				continue
			}
			out[sysID] = clampFullness(left / total)
		}
		return out, rows.Err()
	})
	if err != nil {
		m.logBot(0, logError, "mining", "reading belt depletion: %v", err)
		return map[int64]float64{}, nil
	}
	return shared, nil
}

func clampFullness(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// beltWithMostOre picks which belt in a system to work: the one holding the most of the ore the pilot
// came for, else the fullest belt when familyID is 0 or nothing carries that ore. It reads the real
// rocks. It returns 0 when the system has no belts or the field cannot be read: park nowhere in particular.
func (m *Mod) beltWithMostOre(ctx context.Context, sysID, familyID int64) int64 {
	f, err := m.beltFieldFor(ctx, sysID)
	if err != nil {
		m.logBot(0, logError, "mining", "reading belts in system %d: %v", sysID, err)
	}
	if f != nil {
		if beltID := f.bestBeltFor(familyID); beltID != 0 {
			return beltID
		}
	}
	// No field to read (no seed row yet, or belts the generator was not given): any belt beats none, since
	// the pilot has to park somewhere.
	var beltID int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT itemID FROM mapDenormalize
		WHERE solarSystemID = ? AND groupID = 9
		ORDER BY itemID LIMIT 1`, sysID).Scan(&beltID); err != nil {
		return 0
	}
	return beltID
}

// bestBeltFor is beltWithMostOre's arithmetic over an already-loaded field: most units of the wanted
// ore, or the most ore of any kind. Ties break on the lower itemID, not randomly, so a re-planning
// pilot does not warp to a different belt for no reason.
func (f *beltField) bestBeltFor(familyID int64) int64 {
	if f == nil {
		return 0
	}
	best, bestScore := int64(0), float64(0)
	if familyID != 0 {
		per := map[int64]int64{}
		for _, r := range f.Rocks {
			if r.Qty <= 0 || destiny.BaseOreType(r.TypeID) != familyID {
				continue
			}
			per[r.BeltID] += r.Qty
		}
		for beltID, units := range per {
			s := float64(units)
			if s > bestScore || (s == bestScore && best != 0 && beltID < best) {
				best, bestScore = beltID, s
			}
		}
		if best != 0 {
			return best
		}
	}
	for beltID, m3 := range f.beltM3() {
		if m3 > bestScore || (m3 == bestScore && best != 0 && beltID < best) {
			best, bestScore = beltID, m3
		}
	}
	return best
}
