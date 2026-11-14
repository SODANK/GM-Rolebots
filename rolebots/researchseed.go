package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
)

// The research seed: a one-time nudge that makes the corporations tagged
// "research" the places researchers are. Their members are suppliers, but with
// Toil at the blue-collar end, far from the laboratory curve (tuning.go
// toilResearch peaks at +35). The nudge walks a share of each such corporation's
// bot members towards the laboratory profile until they clear the career bar.
// Only members already most of the way there move, a consumer is skipped, and
// CEOs are the identity pass's (toil.go).

const (
	researchSeedMarker = "_resschool"

	// The share of a corporation's bot members that should be research careers
	// afterwards: a corporation whose first tag is research is a laboratory, one
	// that merely lists it is a department.
	researchSeedShareFirst = 0.35
	researchSeedShareLater = 0.15

	// researchSeedTarget is the appetite a nudged pilot is walked up to: a
	// little over the bar, so a one-point trait drift does not drop it out.
	researchSeedTarget = researchCareerMin * 1.1

	researchSeedPasses     = 10
	researchSeedToilStep   = 15
	researchSeedPatience   = -10
	researchSeedWander     = -10
	researchSeedSupply     = 8
	researchSeedMinSupply  = -20 // below this the pilot is a buyer, not a maker
	researchSeedWanderStop = -60
	researchSeedSupplyStop = 60
)

// nudgeToResearch walks a copy of a pilot's traits towards the laboratory
// profile until ResearchAppetite clears researchSeedTarget. ok=false when the
// pilot is ineligible or the passes ran out first (the copy returned is the
// closest it got; callers write nothing in that case). Pure.
func nudgeToResearch(traits map[string]int) (map[string]int, bool) {
	out := make(map[string]int, len(traits)+4)
	for k, v := range traits {
		out[k] = v
	}
	if out[TraitSupply] < researchSeedMinSupply {
		return out, false
	}
	for pass := 0; pass < researchSeedPasses; pass++ {
		if resolveTuning(out).ResearchAppetite >= researchSeedTarget {
			return out, true
		}
		// Toil walks towards the laboratory peak from whichever side it is on.
		peak := int(math.Round(researchToilPeak * 100))
		switch d := peak - out[TraitToil]; {
		case d > researchSeedToilStep:
			out[TraitToil] += researchSeedToilStep
		case d < -researchSeedToilStep:
			out[TraitToil] -= researchSeedToilStep
		default:
			out[TraitToil] = peak
		}
		if out[TraitPatience] > -100 {
			out[TraitPatience] = clampTrait(float64(out[TraitPatience] + researchSeedPatience))
		}
		if out[TraitWanderlust] > researchSeedWanderStop {
			out[TraitWanderlust] = clampTrait(float64(out[TraitWanderlust] + researchSeedWander))
		}
		if out[TraitSupply] < researchSeedSupplyStop {
			out[TraitSupply] = clampTrait(float64(out[TraitSupply] + researchSeedSupply))
		}
	}
	return out, resolveTuning(out).ResearchAppetite >= researchSeedTarget
}

// researchSeedPicks chooses which of a corporation's members to nudge: the
// share it should end with, less the careers it already has, taken from the
// highest current appetite down. Pure; ties break on the pilot id so a re-run
// of the same data picks the same pilots.
func researchSeedPicks(appetite map[int64]float64, share float64) []int64 {
	if len(appetite) == 0 {
		return nil
	}
	want := int(math.Ceil(share * float64(len(appetite))))
	have := 0
	var pool []int64
	for id, a := range appetite {
		if a >= researchCareerMin {
			have++
			continue
		}
		pool = append(pool, id)
	}
	need := want - have
	if need <= 0 {
		return nil
	}
	sort.Slice(pool, func(i, j int) bool {
		if appetite[pool[i]] != appetite[pool[j]] {
			return appetite[pool[i]] > appetite[pool[j]]
		}
		return pool[i] < pool[j]
	})
	if need < len(pool) {
		pool = pool[:need]
	}
	return pool
}

// migrateResearchSeed runs the nudge over every research-tagged corporation's
// bot members, once.
func migrateResearchSeed(ctx context.Context, db *sql.DB) error {
	if done, err := traitMarkerSet(ctx, db, researchSeedMarker); err != nil {
		return fmt.Errorf("migrateResearchSeed probe: %w", err)
	} else if done {
		return nil
	}
	// A schema without the game's character table has nothing to say: skip,
	// unmarked. Both columns are probed, since a stand-in with one fails ensureTables.
	for _, col := range []string{"corporationID", "accountID"} {
		if ok, err := haveColumn(ctx, db, "character_", col); err != nil || !ok {
			return err
		}
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.corporationID,
		       SUBSTRING_INDEX(c.tags, ',', 1) = 'research' AS lead,
		       ch.characterID
		  FROM rolebot_corps c
		  JOIN character_ ch ON ch.corporationID = c.corporationID AND ch.accountID > 0
		  JOIN rolebot_characters r ON r.characterID = ch.characterID
		 WHERE FIND_IN_SET('research', c.tags)
		   AND NOT EXISTS (SELECT 1 FROM rolebot_ceos ce WHERE ce.characterID = ch.characterID)`)
	if err != nil {
		return fmt.Errorf("migrateResearchSeed read: %w", err)
	}
	type corpGroup struct {
		lead    bool
		members []int64
	}
	groups := map[int64]*corpGroup{}
	var ids []int64
	for rows.Next() {
		var corpID, charID int64
		var lead bool
		if err := rows.Scan(&corpID, &lead, &charID); err != nil {
			rows.Close()
			return fmt.Errorf("migrateResearchSeed scan: %w", err)
		}
		g := groups[corpID]
		if g == nil {
			g = &corpGroup{lead: lead}
			groups[corpID] = g
		}
		g.members = append(g.members, charID)
		ids = append(ids, charID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		// Nothing tagged yet: leave unmarked so a later boot, once the corps
		// exist, still gets its turn.
		return nil
	}
	traits, err := loadTraitsBulk(ctx, db, ids)
	if err != nil {
		return fmt.Errorf("migrateResearchSeed traits: %w", err)
	}

	moved, skipped := 0, 0
	var write [][3]any
	for corpID, g := range groups {
		appetite := make(map[int64]float64, len(g.members))
		for _, id := range g.members {
			appetite[id] = resolveTuning(traits[id]).ResearchAppetite
		}
		share := researchSeedShareLater
		if g.lead {
			share = researchSeedShareFirst
		}
		for _, id := range researchSeedPicks(appetite, share) {
			next, ok := nudgeToResearch(traits[id])
			if !ok {
				skipped++
				continue
			}
			for k, v := range next {
				if traits[id][k] != v {
					write = append(write, [3]any{id, k, v})
				}
			}
			moved++
		}
		_ = corpID
	}
	// Written in bounded chunks: a statement with every changed trait of the
	// roster in it is a very long statement.
	const chunk = 500
	for i := 0; i < len(write); i += chunk {
		j := min(i+chunk, len(write))
		if err := upsertTraits(ctx, db, write[i:j]); err != nil {
			return fmt.Errorf("migrateResearchSeed write: %w", err)
		}
	}
	if err := setTraitMarker(ctx, db, researchSeedMarker); err != nil {
		return fmt.Errorf("migrateResearchSeed marker: %w", err)
	}
	rlogf("rolebots: research seed: %d member(s) of %d research-tagged corporation(s) moved towards the laboratory, %d could not be",
		moved, len(groups), skipped)
	return nil
}
