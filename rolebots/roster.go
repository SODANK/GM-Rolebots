package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Activity names: the rolebot activity slugs the state rows, gangs and fit roles
// speak. What a bot may do is not a checklist (appetite.go).
const (
	ActMining = "mining"
	// ActCombat is a combat wing out looking for action (combatplan.go), PvE or
	// PvP: the planner draws a trip and the employer's doctrine decides which it
	// may fly. migrateCombatRename moves older stored rows.
	ActCombat   = "combat"
	ActTrade    = "trade"
	ActIndustry = "industry"
	// Robbing other pilots (piracy.go). Reachable only by a pilot that is both
	// violent and lawless (Tuning.PiracyAppetite).
	ActPiracy = "piracy"
	// The industry mining fleet (gangindustry.go): a corp mining op whose haul
	// lands in the commander's factory hangar, paid per delivery. A gang activity
	// only; a corp's industry tag seats its commander (corpFleetActivities).
	ActIndMining = "indmining"
	// The corporate security patrol (gangplan.go): an employer's own guns working
	// its home region, clearing its miners' belts and answering the distress board.
	// A gang activity only, not a fit lens: a member sets st.Activity = ActCombat.
	ActSecurity = "security"
)

// Rolebot is one roster row.
type Rolebot struct {
	CharacterID int64
	Enabled     bool
	AddedDate   int64
}

// Profile is a rolebot's global flags.
type Profile struct {
	CharacterID int64
	// Standings is the pilot's fluid -10..+10 opinion of factions and pilots
	// (standings.go), read through the tier helpers. There is no place fence on a
	// profile: where a pilot flies is priced by Tuning.PersonalSecFloor, never a wall.
	Standings []Standing
	// Home is character_.homeStationID (homeStationFor), so the profile keeps no
	// copy. IndustryStationID pins where this pilot manufactures (industry.go);
	// 0 = it picks. The rolebots.industryBase setting overrides it unless HomePinned.
	IndustryStationID int64
	// HomePinned is the operator pinning both addresses (clone and factory):
	// corphome.go, moveout.go, clone.go and industry.go return early on it.
	// Written by setHomePinned, never saveProfile: the form does not carry it.
	HomePinned bool
	// AutoSkills is whether this pilot buys and trains its own skills (skills.go);
	// default on. Written by setAutoSkills, never saveProfile, whose form does not
	// carry it.
	AutoSkills bool
}

func addRolebot(ctx context.Context, db *sql.DB, charID int64) error {
	var exists int64
	err := db.QueryRowContext(ctx,
		`SELECT characterID FROM character_ WHERE characterID = ?`, charID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("character %d not found", charID)
	}
	if err != nil {
		return fmt.Errorf("addRolebot lookup: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO rolebot_characters (characterID, enabled, addedDate)
		VALUES (?, 0, ?)`, charID, time.Now().Unix()); err != nil {
		return fmt.Errorf("addRolebot: %w", err)
	}
	// Seed the profile row so the profile page always has one to edit.
	if _, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO rolebot_profiles (characterID) VALUES (?)`, charID); err != nil {
		return fmt.Errorf("addRolebot profile: %w", err)
	}
	// The mail gate reads this roster (mailsuppress.go); a pilot added and
	// immediately war-dec'd should not collect a minute's worth of mail first.
	invalidateBotMailboxes()
	return nil
}

func removeRolebot(ctx context.Context, db *sql.DB, charID int64) error {
	forgetSavedState(charID) // rolebot_state is deleted below (statesave.go)
	forgetCharacter(charID)  // the character survives; its bot memory does not (forgetreg.go)
	for _, q := range []string{
		`DELETE FROM rolebot_characters WHERE characterID = ?`,
		`DELETE FROM rolebot_profiles WHERE characterID = ?`,
		`DELETE FROM rolebot_traits WHERE characterID = ?`,
		`DELETE FROM rolebot_state WHERE characterID = ?`,
	} {
		if _, err := db.ExecContext(ctx, q, charID); err != nil {
			return fmt.Errorf("removeRolebot: %w", err)
		}
	}
	// Off the roster is back to being a person: their mail must be written again.
	invalidateBotMailboxes()
	return nil
}

// getRolebot returns the roster row, or nil when not assigned.
func getRolebot(ctx context.Context, db *sql.DB, charID int64) (*Rolebot, error) {
	var rb Rolebot
	var en int64
	err := db.QueryRowContext(ctx, `
		SELECT characterID, enabled, addedDate
		FROM rolebot_characters WHERE characterID = ?`, charID,
	).Scan(&rb.CharacterID, &en, &rb.AddedDate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getRolebot: %w", err)
	}
	rb.Enabled = en != 0
	return &rb, nil
}

func setRolebotEnabled(ctx context.Context, db *sql.DB, charID int64, enabled bool) error {
	en := 0
	if enabled {
		en = 1
	}
	res, err := db.ExecContext(ctx,
		`UPDATE rolebot_characters SET enabled = ? WHERE characterID = ?`, en, charID)
	if err != nil {
		return fmt.Errorf("setRolebotEnabled: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var one int64
		if err := db.QueryRowContext(ctx,
			`SELECT 1 FROM rolebot_characters WHERE characterID = ?`, charID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("character %d is not on the rolebot roster", charID)
		}
	}
	return nil
}

// profileCols is the column list every read of rolebot_profiles selects, in the
// order scanProfileInto expects (see stateCols for why both exist).
const profileCols = `industryStationID, autoSkills, homePinned`

// newProfile is what a pilot with no profile row is. AutoSkills defaults ON.
func newProfile(charID int64) *Profile {
	return &Profile{CharacterID: charID, AutoSkills: true}
}

// scanProfileInto decodes one profileCols row into p. The outlook is not part of
// it: it is the employer's and is derived alongside (outlook.go).
func scanProfileInto(scan func(dest ...any) error, p *Profile) error {
	var autoSkills, homePinned int64
	if err := scan(&p.IndustryStationID, &autoSkills, &homePinned); err != nil {
		return err
	}
	p.AutoSkills = autoSkills != 0
	p.HomePinned = homePinned != 0
	return nil
}

// getProfile is a *Mod method because a pilot's standings are its employer's and
// are derived (outlook.go) from state the module holds. A free function would let
// a caller skip the outlook and get a Profile neutral to every faction. A missing
// row is a valid pilot with default flags.
func (m *Mod) getProfile(ctx context.Context, charID int64) (*Profile, error) {
	p := newProfile(charID)
	row := m.db.QueryRowContext(ctx,
		`SELECT `+profileCols+` FROM rolebot_profiles WHERE characterID = ?`, charID)
	err := scanProfileInto(row.Scan, p)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("getProfile: %w", err)
	}
	employer, err := m.outlookFor(ctx, charID)
	if err != nil {
		return nil, err
	}
	p.Standings = employer
	return p, nil
}

// saveProfile writes the flags row. A pilot's outlook is its employer's and is
// stored nowhere; the legacy faction CSV columns stay empty
// (migrateDropPilotStandings drains them).
func saveProfile(ctx context.Context, db *sql.DB, p *Profile) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_profiles
			(characterID, industryStationID)
		VALUES (?, ?)
		ON DUPLICATE KEY UPDATE
			industryStationID = VALUES(industryStationID)`,
		p.CharacterID,
		p.IndustryStationID)
	if err != nil {
		return fmt.Errorf("saveProfile: %w", err)
	}
	return nil
}

// setHomePinned flips one pilot's address pin. It is not part of saveProfile,
// like setAutoSkills: the profile form does not carry this field and must not
// clear it.
func setHomePinned(ctx context.Context, db *sql.DB, charID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_profiles (characterID, homePinned) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE homePinned = VALUES(homePinned)`, charID, v)
	if err != nil {
		return fmt.Errorf("setHomePinned: %w", err)
	}
	return nil
}

// clearHomePins releases the pin for every rostered pilot in one corporation,
// so they go back to choosing their own address. Reports how many rows moved.
func clearHomePins(ctx context.Context, db *sql.DB, corpID int64) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE rolebot_profiles p
		  JOIN character_ c ON c.characterID = p.characterID
		   SET p.homePinned = 0
		 WHERE c.corporationID = ? AND p.homePinned <> 0`, corpID)
	if err != nil {
		return 0, fmt.Errorf("clearHomePins: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ── traits ──────────────────────────────────────────────────────────────────

// getTraits reads a character's personality as key to -100..+100. A trait with
// no row is 0, which resolveTuning treats as default behaviour, so an
// unconfigured bot behaves as it did before traits.
func getTraits(ctx context.Context, db *sql.DB, charID int64) (map[string]int, error) {
	// A CEO think carries every member's traits (thinkctx.go).
	if tc := thinkCacheAny(ctx); tc != nil {
		if t, ok := tc.traitsOf(charID); ok {
			return t, nil
		}
	}
	rows, err := db.QueryContext(ctx,
		`SELECT trait, value FROM rolebot_traits WHERE characterID = ?`, charID)
	if err != nil {
		return nil, fmt.Errorf("getTraits: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var v int
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("getTraits scan: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// saveTraits replaces a character's traits. Values are clamped and zeroes
// are deleted rather than stored, so "no opinion" and "explicitly average"
// are the same thing in the database as they are in the sim.
func saveTraits(ctx context.Context, db *sql.DB, charID int64, traits map[string]int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("saveTraits begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rolebot_traits WHERE characterID = ?`, charID); err != nil {
		return fmt.Errorf("saveTraits clear: %w", err)
	}
	for _, def := range traitCatalog {
		v, ok := traits[def.Key]
		if !ok || v == 0 {
			continue
		}
		if v < -100 {
			v = -100
		}
		if v > 100 {
			v = 100
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rolebot_traits (characterID, trait, value) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE value = VALUES(value)`, charID, def.Key, v); err != nil {
			return fmt.Errorf("saveTraits %s: %w", def.Key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("saveTraits commit: %w", err)
	}
	return nil
}
