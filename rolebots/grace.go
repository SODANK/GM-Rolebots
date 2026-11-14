package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	mrand "math/rand"
)

// Grace: forgiving (-100) <-> vindictive (+100). Resolves to Tuning.Vindictiveness or
// Tuning.Forgiveness, one pole 0..1, read against the grace* tiers (tuning.go). The ledger that
// acts on it is grace_ledger.go; docs/GRACE.md has the rules.

const (
	graceMean = 0.0
	graceSD   = 50.0
	// Hot and hurried pilots lean vindictive: HIGH patience is the hurried end (traitCatalog).
	graceAggressionLean = 0.20
	gracePatienceLean   = 0.15

	graceSeedMarker = "_graceseed" // fits rolebot_traits.trait, VARCHAR(16)
	graceSeedBatch  = 1000
	graceSeedNoise  = 0x6772616365 // "grace"
)

// Vindictive stages, by Tuning.Vindictiveness.
const (
	graceStageNone = iota
	graceStageSore
	graceStageGrudging
	graceStageVengeful
	graceStageImplacable
)

func graceStage(v float64) int {
	switch {
	case v <= 0:
		return graceStageNone
	case v < graceGrudging:
		return graceStageSore
	case v < graceVengeful:
		return graceStageGrudging
	case v < graceImplacable:
		return graceStageVengeful
	}
	return graceStageImplacable
}

// graceTierLabel names the pilot's pole for the panels; "even" when neither is set.
func graceTierLabel(tn Tuning) string {
	switch graceStage(tn.Vindictiveness) {
	case graceStageImplacable:
		return "implacable"
	case graceStageVengeful:
		return "vengeful"
	case graceStageGrudging:
		return "grudging"
	case graceStageSore:
		return "sore"
	}
	switch {
	case tn.Forgiveness >= graceMagnanimous:
		return "magnanimous"
	case tn.Forgiveness >= graceClement:
		return "clement"
	case tn.Forgiveness > 0:
		return "lenient"
	}
	return "even"
}

// graceLean is the draw's offset from the two traits it leans on. Positive is the vindictive end.
func graceLean(aggression, patience int) float64 {
	return graceAggressionLean*float64(aggression) + gracePatienceLean*float64(patience)
}

func graceFromNormal(z, offset float64) int {
	return clampTrait(graceMean + offset + graceSD*z)
}

// graceDraw is the generator's draw.
func graceDraw(rng *mrand.Rand, offset float64) int {
	return graceFromNormal(rng.NormFloat64(), offset)
}

// graceSeedValue is the existing roster's draw, deterministic in the character ID.
func graceSeedValue(charID int64, traits map[string]int) int {
	return graceFromNormal(hashNormal(charID, graceSeedNoise),
		graceLean(traits[TraitAggression], traits[TraitPatience]))
}

// graceSeedQuery reads one roster batch: the pilot, its aggression and patience, and whether it
// already has a Grace value. Trait keys are literals (see migrateLeadershipSeed on the CASE fold).
var graceSeedQuery = fmt.Sprintf(`
	SELECT r.characterID,
	       COALESCE((SELECT t.value FROM rolebot_traits t WHERE t.characterID = r.characterID AND t.trait = '%s'), 0),
	       COALESCE((SELECT t.value FROM rolebot_traits t WHERE t.characterID = r.characterID AND t.trait = '%s'), 0),
	       EXISTS (SELECT 1 FROM rolebot_traits t WHERE t.characterID = r.characterID AND t.trait = '%s')
	  FROM rolebot_characters r
	 WHERE r.characterID > ?
	 ORDER BY r.characterID
	 LIMIT ?`, TraitAggression, TraitPatience, TraitGrace)

// migrateGraceSeed gives every rostered pilot without a Grace value one, once. Cursor-walked.
func migrateGraceSeed(ctx context.Context, db *sql.DB) error {
	if done, err := traitMarkerSet(ctx, db, graceSeedMarker); err != nil {
		return fmt.Errorf("migrateGraceSeed probe: %w", err)
	} else if done {
		return nil
	}
	seeded := 0
	var cursor int64
	for {
		rows, err := db.QueryContext(ctx, graceSeedQuery, cursor, graceSeedBatch)
		if err != nil {
			return fmt.Errorf("migrateGraceSeed read: %w", err)
		}
		var batch [][3]any
		n := 0
		for rows.Next() {
			var id int64
			var aggr, pat int
			var has bool
			if err := rows.Scan(&id, &aggr, &pat, &has); err != nil {
				rows.Close()
				return fmt.Errorf("migrateGraceSeed scan: %w", err)
			}
			n++
			cursor = id
			if !has {
				v := graceSeedValue(id, map[string]int{TraitAggression: aggr, TraitPatience: pat})
				batch = append(batch, [3]any{id, TraitGrace, v})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("migrateGraceSeed rows: %w", err)
		}
		if err := upsertTraits(ctx, db, batch); err != nil {
			return fmt.Errorf("migrateGraceSeed write: %w", err)
		}
		seeded += len(batch)
		if n < graceSeedBatch {
			break
		}
	}
	if err := setTraitMarker(ctx, db, graceSeedMarker); err != nil {
		return fmt.Errorf("migrateGraceSeed marker: %w", err)
	}
	if seeded > 0 {
		rlogf("rolebots: seeded the grace trait on %d pilot(s)", seeded)
	}
	return nil
}
