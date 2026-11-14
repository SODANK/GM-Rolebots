package rolebots

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// Appearance generation for a bot pilot: everything the client needs to draw a face, assembled as the
// character creator would. The pose (head and eye rotation, camera position) comes whole from the curated
// library in poses.go; everything else is rolled here. Three rules, each a client error if broken:
//
//  1. Every item comes from the chrBL* rows for this pilot's (bloodlineID, gender), the creator's own filter.
//  2. accessory, beard, deco, lipstick and makeup are NULLABLE and absent, never 0: the client's face.py
//     DoSetItem raises KeyError on item ID 0. The rest are NOT NULL, so a bloodline lacking one fails.
//  3. The sixteen morphs are eight axis pairs with exactly one pole set and the other NULL; setting both
//     is a state the creator cannot produce.

const (
	genderFemale int64 = 0
	genderMale   int64 = 1
)

// ── options the client cannot draw ──────────────────────────────────────────
// A chrBL* row says an option is offered, not that it works. Three Jin-Mei male helmets (hair 208, 211, 215)
// reference a texture path with a doubled slash the client cannot resolve. See docs/comment-notes-b01.md#broken-appearance-options.
type appearanceKey struct {
	BloodlineID int64
	Gender      int64
	Slot        string // the key used in the option map: "hair", "costume", …
}

var brokenAppearance = map[appearanceKey][]int64{
	// Jin-Mei male: 208 Superbright Helmet, 211 Helmet 1, 215 Bronze helmet.
	{BloodlineID: 12, Gender: genderMale, Slot: "hair"}: {208, 211, 215},
}

// isBrokenOption reports whether an option is one the client cannot fully draw.
func isBrokenOption(bloodlineID, gender int64, slot string, id int64) bool {
	for _, bad := range brokenAppearance[appearanceKey{bloodlineID, gender, slot}] {
		if bad == id {
			return true
		}
	}
	return false
}

// requiredSlots are the appearance slots character_ declares NOT NULL. A bloodline missing options for
// any of them cannot be generated: refuse the pilot rather than write a zero the client chokes on.
var requiredSlots = []string{"costume", "eyebrows", "eyes", "hair", "skin", "background", "light"}

// nullableSlots may legitimately be absent. Their availability is gendered, so no options here is
// normal (a male pilot has no lipstick rows) and never an error.
var nullableSlots = []string{"accessory", "beard", "deco", "lipstick", "makeup"}

// appearanceCache memoises the option lists per (bloodline, gender). A 200-bot
// seed would otherwise run twelve queries per pilot for twenty-four distinct
// answers.
type appearanceCache struct {
	opts map[[2]int64]map[string][]evedb.ESPAppearanceOption
}

func newAppearanceCache() *appearanceCache {
	return &appearanceCache{opts: map[[2]int64]map[string][]evedb.ESPAppearanceOption{}}
}

func (ac *appearanceCache) get(ctx context.Context, m *Mod, bloodlineID, gender int64) map[string][]evedb.ESPAppearanceOption {
	key := [2]int64{bloodlineID, gender}
	if o, ok := ac.opts[key]; ok {
		return o
	}
	// The panel's own filter, shared with the agent portrait editor: the one place that knows which
	// chrBL* table backs which slot.
	o := evedb.ESPGetAllAppearanceOptions(ctx, m.db, bloodlineID, gender)
	ac.opts[key] = o
	return o
}

// buildAppearance produces the CreateCharacter appearance dict for one pilot. Absent keys mean SQL NULL
// (db.parseAppearance returns nil for a missing key), which is how a nullable slot and the unused pole
// of a morph axis stay empty, so this function omits rather than zeroes.
func (ac *appearanceCache) buildAppearance(ctx context.Context, m *Mod, rng *rand.Rand, bloodlineID, gender int64) (map[string]interface{}, pose, error) {
	opts := ac.get(ctx, m, bloodlineID, gender)
	app := map[string]interface{}{}

	// Options the client cannot draw are removed from the list, not redrawn past: a redraw would spin when
	// every option is broken and would bias the odds.
	pick := func(slot string) (int64, bool) {
		list := opts[slot]
		usable := make([]int64, 0, len(list))
		for _, o := range list {
			if o.ID != 0 && !isBrokenOption(bloodlineID, gender, slot, o.ID) {
				usable = append(usable, o.ID)
			}
		}
		if len(usable) == 0 {
			return 0, false
		}
		return usable[rng.Intn(len(usable))], true
	}

	for _, slot := range requiredSlots {
		id, ok := pick(slot)
		if !ok || id == 0 {
			return nil, pose{}, fmt.Errorf(
				"bloodline %d gender %d has no %s option — cannot build a portrait", bloodlineID, gender, slot)
		}
		app[slot+"ID"] = id
	}
	for _, slot := range nullableSlots {
		// A nullable slot is not always worn even when options exist: a population where every pilot has a deco
		// reads as costumed. About two in three wear each.
		if rng.Intn(3) == 0 {
			continue
		}
		if id, ok := pick(slot); ok && id != 0 {
			app[slot+"ID"] = id
		}
	}

	// Morphs: one pole per axis pair, never both, never neither.
	for group := 1; group <= 4; group++ {
		for _, axis := range [2][2]string{{"e", "w"}, {"n", "s"}} {
			pole := axis[rng.Intn(2)]
			// Observed values run the whole 0..1 range, so a uniform draw is the honest model. It is floored just
			// above zero because a 0.0 morph is the same face as an unset one.
			app[fmt.Sprintf("morph%d%s", group, pole)] = 0.02 + rng.Float64()*0.98
		}
	}

	p := pickPose(rng)
	app["headRotation1"], app["headRotation2"], app["headRotation3"] = p.Head[0], p.Head[1], p.Head[2]
	app["eyeRotation1"], app["eyeRotation2"], app["eyeRotation3"] = p.Eye[0], p.Eye[1], p.Eye[2]
	app["camPos1"], app["camPos2"], app["camPos3"] = p.Cam[0], p.Cam[1], p.Cam[2]

	return app, p, nil
}

// ── repairing pilots already generated ──────────────────────────────────────

// slotColumn maps an option-map key to its character_ column. Only the slots a
// blocklist entry can name need to be here.
var slotColumn = map[string]string{
	"hair": "hairID", "costume": "costumeID", "eyes": "eyesID",
	"eyebrows": "eyebrowsID", "skin": "skinID", "background": "backgroundID",
	"light": "lightID", "accessory": "accessoryID", "beard": "beardID",
	"deco": "decoID", "lipstick": "lipstickID", "makeup": "makeupID",
}

// repairBrokenAppearance re-rolls the affected slot on every generated pilot wearing an option the client
// cannot draw. Only that slot changes, and hand-made characters are left alone. It returns the changed
// names so the operator can drop their stored portraits: the client caches a rendered portrait by
// characterID (docs/character-profile.md).
func (m *Mod) repairBrokenAppearance(ctx context.Context) ([]string, error) {
	if len(brokenAppearance) == 0 {
		return nil, nil
	}
	ac := newAppearanceCache()
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	var fixed []string

	for key, badIDs := range brokenAppearance {
		col, ok := slotColumn[key.Slot]
		if !ok {
			return fixed, fmt.Errorf("blocklist names slot %q, which has no character_ column", key.Slot)
		}
		ph := make([]string, len(badIDs))
		args := []any{key.BloodlineID, key.Gender}
		for i, id := range badIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		rows, err := m.db.QueryContext(ctx, `
			SELECT c.characterID, c.characterName
			FROM rolebot_generated g
			JOIN character_ c ON c.characterID = g.characterID
			WHERE c.bloodlineID = ? AND c.gender = ?
			  AND c.`+col+` IN (`+strings.Join(ph, ",")+`)`, args...)
		if err != nil {
			return fixed, fmt.Errorf("repairBrokenAppearance find: %w", err)
		}
		type victim struct {
			id   int64
			name string
		}
		var victims []victim
		for rows.Next() {
			var v victim
			if err := rows.Scan(&v.id, &v.name); err != nil {
				rows.Close()
				return fixed, err
			}
			victims = append(victims, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fixed, err
		}
		if len(victims) == 0 {
			continue
		}

		opts := ac.get(ctx, m, key.BloodlineID, key.Gender)
		var usable []int64
		for _, o := range opts[key.Slot] {
			if o.ID != 0 && !isBrokenOption(key.BloodlineID, key.Gender, key.Slot, o.ID) {
				usable = append(usable, o.ID)
			}
		}
		if len(usable) == 0 {
			return fixed, fmt.Errorf("every %s option for bloodline %d gender %d is broken — nothing to swap to",
				key.Slot, key.BloodlineID, key.Gender)
		}
		for _, v := range victims {
			pick := usable[rng.Intn(len(usable))]
			if _, err := m.db.ExecContext(ctx,
				`UPDATE character_ SET `+col+` = ? WHERE characterID = ?`, pick, v.id); err != nil {
				return fixed, fmt.Errorf("repair %s: %w", v.name, err)
			}
			m.logBot(v.id, logInfo, "roster",
				"portrait repaired — its %s option could not be drawn by the client (a shipped bookmark with a malformed texture path), swapped to %d", key.Slot, pick)
			fixed = append(fixed, v.name)
		}
	}
	return fixed, nil
}
