package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
)

// Toil: the blue-collar / white-collar trait (tuning.go owns the dial and the curves; this file puts it on the
// roster). Three one-shot migrations, each behind a marker row at characterID 0 in rolebot_traits, in the order
// ensureTables runs them:
//
//  1. migrateToilSeed: every rostered pilot without a Toil value draws one, centred on its generated archetype
//     (a miner -70, a trader +70) with a wide spread, deterministic in the character ID.
//  2. migrateHaulerConversion: Envy is the line between a trader and a hauler, so a fifth of the traders (by hash,
//     never a CEO) become haulers: generous, mildly blue-collar, relabelled in rolebot_generated.
//  3. migrateCEOIdentity: a hand-checked pass over the bot CEOs. A CEO's personality is its corporation's identity
//     (corpceo_charter.go), so the CEO is nudged until its charter declares the corporation's plain business.

const (
	toilIndShiftMarker = "_toilind"
	// toilIndShift is how far the industrialist archetype's centre moved after the first seed (0 to -20);
	// migrateIndustrialistToilShift moves the pilots already seeded by the same amount, spread intact.
	toilIndShift      = -20
	toilSeedMarker    = "_toilseed"
	haulConvMarker    = "_haulconv"
	ceoIdentityMarker = "_ceoident"
	// ceoResearchMarker guards the second CEO identity pass: research added Patience to what a laboratory is, so the
	// research CEOs the first pass shaped no longer declare it.
	ceoResearchMarker  = "_ceoresearch"
	toilSeedBatch      = 1000
	toilSeedSD         = 35.0
	haulConvPercent    = 20
	haulConvGreed      = -40
	haulConvGreedSpan  = 10
	haulConvToil       = -35
	haulConvToilSpan   = 15
	ceoNudgeStep       = 10
	ceoNudgeMaxPasses  = 20
	toilArchetypeNoise = 0x746f696c // "toil"
)

// toilArchetypeCentre is where a generated archetype's Toil draw is centred.
// Combat pilots and pilots with no generated row centre on the middle.
var toilArchetypeCentre = map[string]float64{
	"miner":         -70,
	"industrialist": -20,
	"trader":        70,
	"hauler":        -35,
	"researcher":    35,
}

// hashNormal is a deterministic standard normal draw for (charID, salt).
func hashNormal(charID int64, salt uint64) float64 {
	h := splitmix64(uint64(charID) ^ salt + noiseGolden)
	u1 := (float64(h>>11) + 0.5) / (1 << 53)
	h = splitmix64(h + noiseGolden)
	u2 := (float64(h>>11) + 0.5) / (1 << 53)
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

// hashUnit is a deterministic 0..1 draw for (charID, salt).
func hashUnit(charID int64, salt uint64) float64 {
	return float64(splitmix64(uint64(charID)^salt+noiseGolden)>>11) / (1 << 53)
}

func clampTrait(v float64) int {
	return int(math.Max(-100, math.Min(100, math.Round(v))))
}

// toilSeedValue is one pilot's draw. Pure.
func toilSeedValue(charID int64, archetype string) int {
	return clampTrait(toilArchetypeCentre[archetype] + toilSeedSD*hashNormal(charID, toilArchetypeNoise))
}

// traitMarkerSet reports whether a one-shot migration has already run.
func traitMarkerSet(ctx context.Context, db *sql.DB, marker string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_traits WHERE characterID = 0 AND trait = ?`, marker).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func setTraitMarker(ctx context.Context, db *sql.DB, marker string) error {
	defer invalidateTraits(0) // a marker row in rolebot_traits (roster.go traits cache)
	_, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO rolebot_traits (characterID, trait, value) VALUES (0, ?, 1)`, marker)
	return err
}

// upsertTraits writes (charID, trait, value) rows in one statement.
func upsertTraits(ctx context.Context, db *sql.DB, rows [][3]any) error {
	// A bulk trait write: the traits cache is dropped before and after (roster.go).
	invalidateAllTraits()
	defer invalidateAllTraits()
	if len(rows) == 0 {
		return nil
	}
	var b strings.Builder
	args := make([]any, 0, len(rows)*3)
	b.WriteString(`INSERT INTO rolebot_traits (characterID, trait, value) VALUES `)
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?, ?, ?)")
		args = append(args, r[0], r[1], r[2])
	}
	b.WriteString(` ON DUPLICATE KEY UPDATE value = VALUES(value)`)
	_, err := db.ExecContext(ctx, b.String(), args...)
	return err
}

// toilSeedQuery reads one roster batch: the pilot, its generated archetype
// ("" when it has none) and whether it already has a Toil value.
var toilSeedQuery = fmt.Sprintf(`
	SELECT r.characterID, COALESCE(g.archetype, ''),
	       EXISTS (SELECT 1 FROM rolebot_traits t
	                WHERE t.characterID = r.characterID AND t.trait = '%s')
	  FROM rolebot_characters r
	  LEFT JOIN rolebot_generated g ON g.characterID = r.characterID
	 WHERE r.characterID > ?
	 ORDER BY r.characterID
	 LIMIT ?`, TraitToil)

// migrateToilSeed gives every rostered pilot without a Toil value one, once.
// Cursor-walked, never OFFSET.
func migrateToilSeed(ctx context.Context, db *sql.DB) error {
	if done, err := traitMarkerSet(ctx, db, toilSeedMarker); err != nil {
		return fmt.Errorf("migrateToilSeed probe: %w", err)
	} else if done {
		return nil
	}
	seeded := 0
	var cursor int64
	for {
		rows, err := db.QueryContext(ctx, toilSeedQuery, cursor, toilSeedBatch)
		if err != nil {
			return fmt.Errorf("migrateToilSeed read: %w", err)
		}
		var batch [][3]any
		n := 0
		for rows.Next() {
			var id int64
			var arch string
			var has bool
			if err := rows.Scan(&id, &arch, &has); err != nil {
				rows.Close()
				return fmt.Errorf("migrateToilSeed scan: %w", err)
			}
			n++
			cursor = id
			if !has {
				batch = append(batch, [3]any{id, TraitToil, toilSeedValue(id, arch)})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("migrateToilSeed rows: %w", err)
		}
		if err := upsertTraits(ctx, db, batch); err != nil {
			return fmt.Errorf("migrateToilSeed write: %w", err)
		}
		seeded += len(batch)
		if n < toilSeedBatch {
			break
		}
	}
	if err := setTraitMarker(ctx, db, toilSeedMarker); err != nil {
		return fmt.Errorf("migrateToilSeed marker: %w", err)
	}
	if seeded > 0 {
		rlogf("rolebots: seeded the toil trait on %d pilot(s)", seeded)
	}
	return nil
}

// haulConvPicked reports whether a trader is one of the fifth that become
// haulers. Pure.
func haulConvPicked(charID int64) bool {
	return hashUnit(charID, 0x6861756c) < haulConvPercent/100.0 // "haul"
}

// haulConvTraits is a converted trader's new Envy and Toil. Pure.
func haulConvTraits(charID int64) (greed, toil int) {
	g := float64(haulConvGreed) + float64(haulConvGreedSpan)*(2*hashUnit(charID, 0x67726565)-1)
	t := float64(haulConvToil) + float64(haulConvToilSpan)*(2*hashUnit(charID, 0x746f6c32)-1)
	return clampTrait(g), clampTrait(t)
}

// migrateHaulerConversion turns a fifth of the trader-archetype roster into
// haulers, once. CEOs are left alone: their corporation's identity is the
// CEO pass's business.
func migrateHaulerConversion(ctx context.Context, db *sql.DB) error {
	if done, err := traitMarkerSet(ctx, db, haulConvMarker); err != nil {
		return fmt.Errorf("migrateHaulerConversion probe: %w", err)
	} else if done {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT g.characterID
		  FROM rolebot_generated g
		  JOIN rolebot_characters r ON r.characterID = g.characterID
		 WHERE g.archetype = 'trader'
		   AND NOT EXISTS (SELECT 1 FROM rolebot_ceos c WHERE c.characterID = g.characterID)`)
	if err != nil {
		return fmt.Errorf("migrateHaulerConversion read: %w", err)
	}
	var picked []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("migrateHaulerConversion scan: %w", err)
		}
		if haulConvPicked(id) {
			picked = append(picked, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for start := 0; start < len(picked); start += toilSeedBatch {
		end := min(start+toilSeedBatch, len(picked))
		chunk := picked[start:end]
		var tr [][3]any
		for _, id := range chunk {
			g, t := haulConvTraits(id)
			tr = append(tr, [3]any{id, TraitGreed, g}, [3]any{id, TraitToil, t})
		}
		if err := upsertTraits(ctx, db, tr); err != nil {
			return fmt.Errorf("migrateHaulerConversion traits: %w", err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE rolebot_generated SET archetype = 'hauler' WHERE characterID IN (`+placeholders(len(chunk))+`)`,
			int64Args(chunk)...); err != nil {
			return fmt.Errorf("migrateHaulerConversion relabel: %w", err)
		}
	}
	if err := setTraitMarker(ctx, db, haulConvMarker); err != nil {
		return fmt.Errorf("migrateHaulerConversion marker: %w", err)
	}
	if len(picked) > 0 {
		rlogf("rolebots: %d trader(s) became haulers (generous, blue-collar)", len(picked))
	}
	return nil
}

// migrateIndustrialistToilShift moves every industrialist-archetype pilot's Toil by toilIndShift, once. The first
// seed centred industrialists mid-axis, where trading out-pulled mining as filler. A shift rather than a re-draw
// keeps each pilot's place in the spread; CEOs are left alone, their identity is the CEO pass's.
func migrateIndustrialistToilShift(ctx context.Context, db *sql.DB) error {
	defer invalidateAllTraits() // writes rolebot_traits (roster.go traits cache)
	if done, err := traitMarkerSet(ctx, db, toilIndShiftMarker); err != nil {
		return fmt.Errorf("migrateIndustrialistToilShift probe: %w", err)
	} else if done {
		return nil
	}
	res, err := db.ExecContext(ctx, `
		UPDATE rolebot_traits t
		  JOIN rolebot_generated g ON g.characterID = t.characterID
		   SET t.value = GREATEST(-100, LEAST(100, t.value + ?))
		 WHERE t.trait = ? AND g.archetype = 'industrialist'
		   AND NOT EXISTS (SELECT 1 FROM rolebot_ceos c WHERE c.characterID = t.characterID)`,
		toilIndShift, TraitToil)
	if err != nil {
		return fmt.Errorf("migrateIndustrialistToilShift: %w", err)
	}
	if err := setTraitMarker(ctx, db, toilIndShiftMarker); err != nil {
		return fmt.Errorf("migrateIndustrialistToilShift marker: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		rlogf("rolebots: shifted %d industrialist(s) %d toward blue collar", n, toilIndShift)
	}
	return nil
}

// ── the CEO identity pass ───────────────────────────────────────────────────

// ceoIntent is what one corporation is, checked by hand against its name. Lead is the tag its charter must put first,
// Second the tag that must follow it, Pure means no other tag may be declared. Set is applied first; Push is the
// levers walked ceoNudgeStep at a time until the charter agrees.
type ceoIntent struct {
	Lead   []string // any of these first (two tags filing one sector)
	Second string
	Pure   bool
	Set    map[string]int
	Push   []ceoLever
}

type ceoLever struct {
	Trait string
	Dir   int
}

var (
	// A generous nomad at the blue-collar pole would rather haul than mine,
	// so a mining CEO that is both is walked toward the middle of Envy and
	// Wanderlust until the rocks win.
	ceoMiningIntent = ceoIntent{Lead: []string{"mining"},
		Set:  map[string]int{TraitToil: -100},
		Push: []ceoLever{{TraitAggression, -1}, {TraitSupply, -1}, {TraitGreed, +1}, {TraitWanderlust, -1}}}
	ceoTradeIntent = ceoIntent{Lead: []string{"trade"},
		Set:  map[string]int{TraitToil: 75},
		Push: []ceoLever{{TraitGreed, +1}, {TraitAggression, -1}}}
	ceoIndustryIntent = ceoIntent{Lead: []string{"industry", "shipbuilding"},
		Set:  map[string]int{TraitToil: 0},
		Push: []ceoLever{{TraitSupply, +1}, {TraitWanderlust, -1}, {TraitAggression, -1}}}
	// A laboratory is run by someone who will wait on it: unhurried and
	// settled as well as white-collar (ResearchAppetite).
	ceoResearchIntent = ceoIntent{Lead: []string{"research"},
		Set:  map[string]int{TraitToil: 35, TraitPatience: -60, TraitWanderlust: -40},
		Push: []ceoLever{{TraitSupply, +1}, {TraitAggression, -1}, {TraitPatience, -1}, {TraitWanderlust, -1}}}
)

// ceoIntents is the hand table, by corporation name. Corporations not in it (every fighting outfit) keep their CEO
// exactly as it is.
var ceoIntents = map[string]ceoIntent{
	"Tribeworks Mining":     ceoMiningIntent,
	"Bedrock LLC.":          ceoMiningIntent,
	"Lucrative Excavations": ceoMiningIntent,
	"Roughneck Industries":  ceoMiningIntent,
	"Gentle Outlook":        ceoMiningIntent,

	"Doubletime Shipping": {Lead: []string{"hauling"}, Pure: true,
		Set:  map[string]int{TraitToil: -20, TraitGreed: -60},
		Push: []ceoLever{{TraitGreed, -1}, {TraitWanderlust, +1}}},
	"Fullsend Deliveries": {Lead: []string{"hauling"}, Second: "trade",
		Set:  map[string]int{TraitToil: 10, TraitGreed: 0},
		Push: []ceoLever{{TraitWanderlust, +1}}},
	"Yulai Cooperative": {Lead: []string{"trade"}, Second: "hauling",
		Set:  map[string]int{TraitToil: 20, TraitGreed: 15},
		Push: []ceoLever{{TraitWanderlust, +1}}},

	"Boystin Exchange":   ceoTradeIntent,
	"Bazaar Guild":       ceoTradeIntent,
	"Broker Club":        ceoTradeIntent,
	"Merchants of Grace": ceoTradeIntent,
	"Heath Commerce":     ceoTradeIntent,
	"Keepsake Kliq":      ceoTradeIntent,

	"Christened Munitions": ceoIndustryIntent,
	"Hexcraft":             ceoIndustryIntent,
	"Makkogi Industries":   ceoIndustryIntent,
	"Solid Industires":     ceoIndustryIntent,
	"Kresh Root Society":   ceoIndustryIntent,

	"Daminen Corporation":  ceoResearchIntent,
	"Idama Institute":      ceoResearchIntent,
	"Insight Laboratories": ceoResearchIntent,
	"Pious Conclave":       ceoResearchIntent,
	"Reborn Technologies":  ceoResearchIntent,
	"Scion Collaborative":  ceoResearchIntent,
	"Crowned Biotech":      ceoResearchIntent,
	"Dynamic Theory":       ceoResearchIntent,

	"LiveWire Inc.": {Lead: []string{"research"}, Second: "industry",
		// Research first but industry still within reach of it, so the lab's edge over the factory floor stays small
		// (Toil 20, mildly unhurried): push Patience or Toil further and industry drops out of the declared tags.
		Set:  map[string]int{TraitToil: 20, TraitPatience: -20},
		Push: []ceoLever{{TraitSupply, +1}}},

	"Bastard Skunkworks": {Lead: []string{"cartelindustry"},
		Set:  map[string]int{TraitToil: 0, TraitSupply: 90},
		Push: []ceoLever{{TraitAggression, -1}, {TraitWanderlust, -1}}},
}

// charterTags is the tag list a personality declares. Pure.
func charterTags(charID int64, traits map[string]int) []string {
	return resolveCharter(pilotIdentity{CharID: charID, Traits: traits, Tune: resolveTuning(traits)}).Tags
}

// intentMet reports whether a declared tag list is what the intent asks for.
// The mercenary tag rides on top of a fighting outfit and is ignored. Pure.
func intentMet(in ceoIntent, tags []string) bool {
	var t []string
	for _, k := range tags {
		if k != tagMercenary {
			t = append(t, k)
		}
	}
	if len(t) == 0 || !containsStr(in.Lead, t[0]) {
		return false
	}
	if in.Pure && len(t) != 1 {
		return false
	}
	if in.Second != "" && (len(t) < 2 || t[1] != in.Second) {
		return false
	}
	return true
}

// nudgeTraitsToIntent applies the intent to a copy of a CEO's traits and walks
// its levers until the charter agrees. ok=false when the levers ran out first
// (the traits returned are still the closest it got). Pure.
func nudgeTraitsToIntent(charID int64, in ceoIntent, traits map[string]int) (map[string]int, bool) {
	out := make(map[string]int, len(traits)+len(in.Set))
	for k, v := range traits {
		out[k] = v
	}
	for k, v := range in.Set {
		out[k] = v
	}
	for pass := 0; pass < ceoNudgeMaxPasses; pass++ {
		if intentMet(in, charterTags(charID, out)) {
			return out, true
		}
		for _, l := range in.Push {
			out[l.Trait] = clampTrait(float64(out[l.Trait] + l.Dir*ceoNudgeStep))
		}
	}
	return out, intentMet(in, charterTags(charID, out))
}

// migrateCEOIdentity runs the hand table over the installed bot CEOs, once.
func migrateCEOIdentity(ctx context.Context, db *sql.DB) error {
	return runCEOIdentity(ctx, db, ceoIdentityMarker, nil)
}

// migrateCEOResearchIdentity re-runs the table for the research intents only, once. Research gained Patience and a
// lower Toil floor, so a research CEO nudged by the first pass can be a hurried one that no longer declares the tag,
// and its corporation would drop its laboratory on the next charter read.
func migrateCEOResearchIdentity(ctx context.Context, db *sql.DB) error {
	return runCEOIdentity(ctx, db, ceoResearchMarker, func(in ceoIntent) bool {
		return containsStr(in.Lead, "research")
	})
}

// runCEOIdentity is the shared body. only, when set, narrows the table to the
// intents it accepts; the marker names which pass has run.
func runCEOIdentity(ctx context.Context, db *sql.DB, marker string, only func(ceoIntent) bool) error {
	if done, err := traitMarkerSet(ctx, db, marker); err != nil {
		return fmt.Errorf("migrateCEOIdentity probe: %w", err)
	} else if done {
		return nil
	}
	// The names live in the game's corporation table. A schema without it (scratch, or a boot before sql/ is loaded)
	// is skipped, unmarked, for a later boot.
	if ok, err := haveColumn(ctx, db, "corporation", "corporationName"); err != nil || !ok {
		return err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.characterID, co.corporationName
		  FROM rolebot_ceos c
		  JOIN corporation co ON co.corporationID = c.corporationID`)
	if err != nil {
		return fmt.Errorf("migrateCEOIdentity read: %w", err)
	}
	type ceoRow struct {
		charID int64
		name   string
	}
	var ceos []ceoRow
	for rows.Next() {
		var r ceoRow
		if err := rows.Scan(&r.charID, &r.name); err != nil {
			rows.Close()
			return fmt.Errorf("migrateCEOIdentity scan: %w", err)
		}
		ceos = append(ceos, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	changed, missed := 0, 0
	for _, r := range ceos {
		in, ok := ceoIntents[r.name]
		if !ok || (only != nil && !only(in)) {
			continue
		}
		traits, err := getTraits(ctx, db, r.charID)
		if err != nil {
			return fmt.Errorf("migrateCEOIdentity traits %d: %w", r.charID, err)
		}
		next, met := nudgeTraitsToIntent(r.charID, in, traits)
		var write [][3]any
		for k, v := range next {
			if traits[k] != v {
				write = append(write, [3]any{r.charID, k, v})
			}
		}
		if err := upsertTraits(ctx, db, write); err != nil {
			return fmt.Errorf("migrateCEOIdentity write %d: %w", r.charID, err)
		}
		if len(write) > 0 {
			changed++
		}
		tags := charterTags(r.charID, next)
		if met {
			rlogf("rolebots: CEO of %s now declares %s", r.name, strings.Join(tags, ", "))
		} else {
			missed++
			rlogf("rolebots: CEO of %s could not be nudged to %s (declares %s) — check its traits on the panel",
				r.name, strings.Join(in.Lead, "/"), strings.Join(tags, ", "))
		}
	}
	if err := setTraitMarker(ctx, db, marker); err != nil {
		return fmt.Errorf("migrateCEOIdentity marker: %w", err)
	}
	if changed > 0 || missed > 0 {
		rlogf("rolebots: CEO identity pass: %d CEO(s) adjusted, %d not met", changed, missed)
	}
	return nil
}
