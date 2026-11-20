package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	mrand "math/rand"
	"strings"
)

// The leadership trait's population. tuning.go owns what the trait does; this
// file owns who has how much of it.
//
// The axis is drawn from N(leadershipMean, leadershipSD), clamped to +-100. The
// draw is follower-skewed: about one pilot in ten past +40 and two or three in a
// hundred past +75 (CEO material). New pilots draw it in blendTraits, with
// archetype and disposition offsets moving the mean. Existing pilots are seeded
// once by migrateLeadershipSeed, keyed on the character ID so a re-run lands on
// the same number.

const (
	leadershipMean = -25.0
	leadershipSD   = 52.0

	// The seeded half has no archetype to read, so it leans on two traits every
	// pilot has: a violent or hurried pilot pushes to the front. The leans nudge the
	// draw and do not replace it.
	leadershipAggressionLean = 0.15
	leadershipPatienceLean   = 0.10

	// leadershipSeedMarker is the marker row's trait key. rolebot_traits.trait
	// is VARCHAR(16), so it has to fit.
	leadershipSeedMarker = "_leaderseed"

	leadershipSeedBatch = 1000
)

// leadershipFromNormal turns a standard-normal draw into a trait value around
// the follower-skewed centre, shifted by offset.
func leadershipFromNormal(z, offset float64) int {
	v := leadershipMean + offset + leadershipSD*z
	if v > 100 {
		v = 100
	}
	if v < -100 {
		v = -100
	}
	return int(math.Round(v))
}

// leadershipDraw is the generator's draw: the archetype's and disposition's
// leadership offsets (already summed into offset) move the centre, and the
// spread is the population's.
func leadershipDraw(rng *mrand.Rand, offset float64) int {
	return leadershipFromNormal(rng.NormFloat64(), offset)
}

// leadershipSeedValue is the existing roster's draw, deterministic in the
// character ID so it does not depend on row order or on how often the migration
// ran.
func leadershipSeedValue(charID int64, traits map[string]int) int {
	h := splitmix64(uint64(charID) ^ 0x6c65616465727368 + noiseGolden) // "leadersh"; traffic.go
	u1 := (float64(h>>11) + 0.5) / (1 << 53)
	h = splitmix64(h + noiseGolden)
	u2 := (float64(h>>11) + 0.5) / (1 << 53)
	z := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2) // Box-Muller
	offset := leadershipAggressionLean*float64(traits[TraitAggression]) +
		leadershipPatienceLean*float64(traits[TraitPatience])
	return leadershipFromNormal(z, offset)
}

// leadershipSeedQuery reads one batch of the roster with the two traits the
// seed leans on and whether a leadership value already exists.
var leadershipSeedQuery = fmt.Sprintf(`
	SELECT r.characterID,
	       COALESCE(MAX(CASE WHEN t.trait = '%s' THEN t.value END), 0),
	       COALESCE(MAX(CASE WHEN t.trait = '%s' THEN t.value END), 0),
	       COALESCE(MAX(CASE WHEN t.trait = '%s' THEN 1 END), 0)
	  FROM rolebot_characters r
	  LEFT JOIN rolebot_traits t ON t.characterID = r.characterID
	 WHERE r.characterID > ?
	 GROUP BY r.characterID
	 ORDER BY r.characterID
	 LIMIT ?`, TraitAggression, TraitPatience, TraitLeadership)

// migrateLeadershipSeed gives every rostered pilot with no leadership value one.
// It runs once: a marker row at characterID 0 (as migrateTraitPolarity does)
// records it. It walks a characterID cursor, never LIMIT/OFFSET, which re-scans
// every row it skips.
func migrateLeadershipSeed(ctx context.Context, db *sql.DB) error {
	defer invalidateAllTraits() // writes rolebot_traits (roster.go traits cache)
	var done int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_traits
		WHERE characterID = 0 AND trait = ?`, leadershipSeedMarker).Scan(&done); err != nil {
		return fmt.Errorf("migrateLeadershipSeed probe: %w", err)
	}
	if done > 0 {
		return nil
	}

	seeded := 0
	var cursor int64
	for {
		// The trait keys are SQL literals, not placeholders: a placeholder in a CASE
		// under GROUP BY is folded early on MariaDB and answers one value for all rows.
		rows, err := db.QueryContext(ctx, leadershipSeedQuery, cursor, leadershipSeedBatch)
		if err != nil {
			return fmt.Errorf("migrateLeadershipSeed read: %w", err)
		}
		type pending struct {
			id    int64
			value int
		}
		var batch []pending
		n := 0
		for rows.Next() {
			var id int64
			var aggr, pat, has int
			if err := rows.Scan(&id, &aggr, &pat, &has); err != nil {
				rows.Close()
				return fmt.Errorf("migrateLeadershipSeed scan: %w", err)
			}
			n++
			cursor = id
			if has != 0 {
				continue // an operator (or the generator) already set one
			}
			batch = append(batch, pending{id, leadershipSeedValue(id,
				map[string]int{TraitAggression: aggr, TraitPatience: pat})})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("migrateLeadershipSeed rows: %w", err)
		}
		if len(batch) > 0 {
			var b strings.Builder
			args := make([]any, 0, len(batch)*3)
			b.WriteString(`INSERT IGNORE INTO rolebot_traits (characterID, trait, value) VALUES `)
			for i, p := range batch {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString("(?, ?, ?)")
				args = append(args, p.id, TraitLeadership, p.value)
			}
			if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
				return fmt.Errorf("migrateLeadershipSeed write: %w", err)
			}
			seeded += len(batch)
		}
		if n < leadershipSeedBatch {
			break
		}
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_traits (characterID, trait, value) VALUES (0, ?, 1)`,
		leadershipSeedMarker); err != nil {
		return fmt.Errorf("migrateLeadershipSeed marker: %w", err)
	}
	if seeded > 0 {
		rlogf("rolebots: seeded the leadership trait on %d pilot(s)", seeded)
	}
	return nil
}
