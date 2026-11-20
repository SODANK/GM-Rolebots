package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	mrand "math/rand"
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

// Handing a generated Capsuleer to a player account.
//
// A generated pilot lives on a RoleBot<N> account with a discarded password, so
// the purge was its only exit. A migration is the other: the character survives
// whole, every rolebot_* row it owns is deleted, every shared claim is released,
// and it ends as an ordinary player character on an account somebody can log into.
// The RoleBot account keeps its slot-mates and a replacement is rolled into the
// hole. Its corporation (real game state) and its hangar (assets stay where they
// are) are not touched.

// MigrateResult is what the panel says happened.
type MigrateResult struct {
	CharacterID   int64
	CharacterName string
	AccountName   string
	CorpName      string
	// HomeName is where the pilot was put: its medical clone station.
	HomeName string
	// LeftBehindAt is the station it was standing in beforehand, when that is
	// somewhere other than home: the one thing an operator has to be told,
	// because that is where its ships and modules still are.
	LeftBehindAt string
	// FromAccount is the RoleBot account it came off; FromEmptied says it held
	// nothing else and was removed. Seq, Slot and FromAccountID are the hole the
	// caller rolls the replacement into, so a failed roll cannot undo the move.
	FromAccount   string
	FromAccountID int64
	FromEmptied   bool
	Seq           int
	Slot          int
	// Replacing is the slot the replacement is being rolled into, for the
	// message; the roll itself runs on the generator's own run gate.
	Replacing string
}

// MigrateToPlayerAccount moves one generated Capsuleer onto a player account,
// strips it of everything this module owns, and docks it at its own medical clone
// station. It does not roll the replacement; the caller does (rollReplacement),
// so a failed roll cannot cost a completed move.
func (m *Mod) MigrateToPlayerAccount(ctx context.Context, charID, targetAccountID int64) (MigrateResult, error) {
	var res MigrateResult

	// ── refusals, all of them before anything is written ──────────────────

	// A generation or wipe run holds the gate the replacement needs; migrating
	// against a busy gate would leave a hole nothing fills.
	if currentRun.snapshot().Active {
		return res, errors.New("a generation or wipe run is in progress — wait for it to finish, " +
			"then migrate (the replacement is rolled on the same run gate)")
	}

	// The Capsuleer must be off, as for the purge: an active bot takes sim turns
	// while its rows are deleted, and anything it writes back mid-move is an orphan.
	if err := m.purgeGate(ctx, charID, "migrate"); err != nil {
		return res, err
	}

	// Nobody may be flying it. A live session would keep writing the location
	// columns underneath the move.
	if m.sessions != nil {
		if sess := m.sessions.FindByCharID(charID); sess != nil {
			return res, fmt.Errorf(
				"a player is currently logged in on this character (%s) — log them out first", sess.CharName)
		}
	}

	// It has to be one of ours to hand over. A Main Cast character is already
	// on a player account and has nothing to migrate.
	var seq, slot int
	var fromAcctID int64
	var fromAcctName string
	err := m.db.QueryRowContext(ctx, `
		SELECT seq, slot, accountID, accountName FROM rolebot_generated WHERE characterID = ?`,
		charID).Scan(&seq, &slot, &fromAcctID, &fromAcctName)
	if errors.Is(err, sql.ErrNoRows) {
		return res, fmt.Errorf("character %d is not part of the generated population — "+
			"only a Capsuleer the generator made can be handed over", charID)
	}
	if err != nil {
		return res, fmt.Errorf("migrate lookup: %w", err)
	}

	// The target has to be a real account, somebody's, with room.
	var toAcctName string
	if err := m.db.QueryRowContext(ctx,
		`SELECT accountName FROM account WHERE accountID = ?`, targetAccountID).Scan(&toAcctName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return res, fmt.Errorf("no account %d", targetAccountID)
		}
		return res, fmt.Errorf("migrate target lookup: %w", err)
	}
	if _, isBotAcct := botSeqOf(toAcctName); isBotAcct {
		return res, fmt.Errorf("%s is a RoleBot account — moving a bot from one to another is not a migration",
			toAcctName)
	}
	var held int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE accountID = ?`, targetAccountID).Scan(&held); err != nil {
		return res, fmt.Errorf("migrate slot count: %w", err)
	}
	// The server enforces no slot cap (handleCreateCharacter never counts), so this
	// is the only gate: the 3913 client's character-select screen has three slots.
	if held >= charsPerAccount {
		return res, fmt.Errorf("%s already holds %d characters — the character-select screen has %d slots, "+
			"so there is nowhere to put this one", toAcctName, held, charsPerAccount)
	}

	// Everything the message needs, read while the rows still say it.
	var name, corpName, atStation string
	_ = m.db.QueryRowContext(ctx, `
		SELECT c.characterName, COALESCE(corp.corporationName, ''), COALESCE(st.stationName, '')
		  FROM character_ c
		  LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		  LEFT JOIN staStations st ON st.stationID = c.stationID
		 WHERE c.characterID = ?`, charID).Scan(&name, &corpName, &atStation)
	if name == "" {
		name = strconv.FormatInt(charID, 10)
	}

	// ── the move ──────────────────────────────────────────────────────────

	// Stop the sim owning it and take its ball out of space, in that order, as
	// PurgeOneCharacter does.
	_ = setRolebotEnabled(ctx, m.db, charID, false)
	m.withdrawProjection(charID)

	// Point of no return. Above is refusals; below is best effort, collected, since
	// the pilot is already on a login screen.
	if _, err := m.db.ExecContext(ctx,
		`UPDATE character_ SET accountID = ? WHERE characterID = ?`, targetAccountID, charID); err != nil {
		return res, fmt.Errorf("%s: move to account: %w", name, err)
	}

	var problems []string

	// The character's own entity row carries ownerID = accountID (db.CreateCharacter).
	// Left alone it would name an account that a later purge may delete.
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET ownerID = ? WHERE itemID = ?`, targetAccountID, charID); err != nil {
		problems = append(problems, "its entity row still names the old account: "+err.Error())
	}

	// Put it where its clone is, the address every other one is measured from
	// (pod.go). Its ship follows; its hangar does not, and the caller says so.
	homeID := m.homeStationFor(ctx, charID)
	homeName := m.stationLabel(ctx, homeID)
	if err := m.dockCharacterAt(ctx, charID, homeID); err != nil {
		problems = append(problems, "it could not be docked at its clone base: "+err.Error())
		// It stays wherever the sim left it, so the result must not claim otherwise.
		homeName += " (not reached)"
	}

	// Everything this module owns about it and every shared claim it held:
	// moduleCharacterSteps, the purge's own list minus the character (botpurge.go).
	forgetSavedState(charID) // rolebot_state is in the steps (statesave.go)
	// And everything else the module remembers (forgetreg.go): the stats counter's
	// next flush would otherwise re-create the rolebot_stats row deleted below.
	forgetCharacter(charID)
	for _, step := range moduleCharacterSteps {
		args := make([]any, argCount(step.query))
		for i := range args {
			args[i] = charID
		}
		if err := execTolerant(ctx, m.db, step, args...); err != nil {
			problems = append(problems, err.Error())
		}
	}
	for _, step := range characterDetachSteps {
		args := make([]any, argCount(step.query))
		for i := range args {
			args[i] = charID
		}
		if err := execTolerant(ctx, m.db, step, args...); err != nil {
			problems = append(problems, err.Error())
		}
	}

	// Off the roster the pilot is a person again, so their mail must be written
	// (as removeRolebot invalidates).
	invalidateBotMailboxes()

	// The RoleBot account normally keeps its two slot-mates. If this was the last,
	// it is an empty shell nothing will fill, so it is deleted.
	emptied := false
	if fromAcctID != 0 {
		var left int64
		_ = m.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM character_ WHERE accountID = ?`, fromAcctID).Scan(&left)
		if left == 0 {
			if _, err := m.db.ExecContext(ctx,
				`DELETE FROM account WHERE accountID = ?`, fromAcctID); err == nil {
				emptied = true
			}
		}
	}

	m.syncPresence()

	res = MigrateResult{
		CharacterID:   charID,
		CharacterName: name,
		AccountName:   toAcctName,
		CorpName:      corpName,
		HomeName:      homeName,
		FromAccount:   fromAcctName,
		FromAccountID: fromAcctID,
		FromEmptied:   emptied,
		Seq:           seq,
		Slot:          slot,
		Replacing:     fmt.Sprintf("%s slot %d", fromAcctName, slot+1),
	}
	if atStation != "" && atStation != homeName {
		res.LeftBehindAt = atStation
	}

	rlogf("rolebots: %s (%d) migrated off %s slot %d onto %s, docked at %s",
		name, charID, fromAcctName, slot+1, toAcctName, homeName)

	if len(problems) > 0 {
		// The move happened but the caller's next step (rollReplacement) does not run,
		// so say so: the slot stays empty until somebody generates one.
		return res, fmt.Errorf("%s was moved to %s, but not cleanly: %s. "+
			"No replacement was rolled into %s — generate one from the form below once this is sorted out",
			name, toAcctName, strings.Join(problems, "; "), res.Replacing)
	}
	return res, nil
}

// dockCharacterAt parks a character inside a station: the location columns on
// character_ and the ship it sits in (moving only the first would split pilot and
// hull across regions). It moves an already-docked pilot too, and leaves hangar
// items alone, since assets do not teleport with their pilot.
func (m *Mod) dockCharacterAt(ctx context.Context, charID, stationID int64) error {
	loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
	if err != nil {
		return fmt.Errorf("dockCharacterAt station %d: %w", stationID, err)
	}

	// The ship the character sits in, if any: its own entity row's locationID,
	// which is a ship it owns rather than the station.
	var shipID int64
	err = m.db.QueryRowContext(ctx, `
		SELECT ship.itemID
		  FROM entity ce
		  JOIN entity ship ON ship.itemID = ce.locationID AND ship.ownerID = ce.itemID
		 WHERE ce.itemID = ?`, charID).Scan(&shipID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("dockCharacterAt ship lookup: %w", err)
	}
	if shipID != 0 {
		if err := evedb.UpdateEntityLocation(ctx, m.db, shipID, stationID); err != nil {
			return err
		}
	}
	return updateCharacterStation(ctx, m.db, charID, stationID,
		loc.SolarSystemID, loc.ConstellationID, loc.RegionID)
}

// ── the replacement ─────────────────────────────────────────────────────────

// rollReplacement mints one pilot into the (seq, slot) a migration vacated, on
// the account already there. It is the one place that tops up an existing
// account: StartGeneration starts a fresh one (botgen.go) so a wipe range stays
// exact. It runs on the generator's own run gate and goroutine.
func (m *Mod) rollReplacement(ctx context.Context, seq, slot int, accountID int64, accountName string) error {
	if accountID == 0 {
		return errors.New("no account to roll a replacement onto")
	}
	spec, _, err := m.lastGenSpec(ctx)
	if err != nil {
		return err
	}
	spec.Count = 1

	rng := mrand.New(mrand.NewSource(clock.Now().UnixNano()))
	plan, err := m.buildPlan(ctx, spec, rng, seq)
	if err != nil {
		return err
	}
	if len(plan) == 0 {
		return errors.New("the current mix produced no pilot to roll")
	}
	// buildPlan packs from startSeq at slot 0; the hole is wherever the
	// migrated pilot was sitting.
	pb := plan[0]
	pb.Seq, pb.Slot, pb.AccountName = seq, slot, accountName

	if !currentRun.begin(fmt.Sprintf("replacing the Capsuleer handed off %s slot %d",
		accountName, slot+1), 1) {
		return errors.New("a generation or wipe run started before the replacement could — " +
			"roll one from the form below when it finishes")
	}

	goSafe("replacement roll", func() {
		defer currentRun.end()
		nm := newNameMinter(m.db, rng)
		ac := newAppearanceCache()
		charID, err := m.createBot(context.Background(), pb, accountID, nm, ac, rng, clock.Unix())
		currentRun.step(charID != 0, fmt.Sprintf("%s slot %d (%s)", accountName, slot+1, pb.Archetype.Label), err)
		if err != nil {
			rlogf("rolebots: replacement on %s slot %d: %v", accountName, slot+1, err)
		}
		m.syncPresence()
	})
	return nil
}

// ── the mix a replacement is rolled from ────────────────────────────────────

// lastGenSpec is the mix the generate form was last submitted with, stored beside
// the count (settingGenTarget). A server that never generated falls back to
// defaultGenSpec. The second return says which was used, shown on the migration card.
func (m *Mod) lastGenSpec(ctx context.Context) (GenSpec, string, error) {
	raw, ok, err := evedb.GetServerSetting(ctx, m.db, settingGenSpec)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		spec, derr := m.defaultGenSpec(ctx)
		return spec, "the shipped defaults — generate once and your own mix is used instead", derr
	}
	var spec GenSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		spec, derr := m.defaultGenSpec(ctx)
		return spec, "the shipped defaults — the stored mix could not be read", derr
	}
	return spec, "the mix you last generated with", nil
}

// saveGenSpec records the mix a generation run was started with. Failing to
// remember a mix must never stop a confirmed run, so callers treat it as soft.
func (m *Mod) saveGenSpec(ctx context.Context, spec GenSpec) error {
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return evedb.SetServerSetting(ctx, m.db, settingGenSpec, string(b))
}

// defaultGenSpec is the form's own defaults, in Go, so a replacement on a server
// that never generated rolls from the numbers the form shows.
func (m *Mod) defaultGenSpec(ctx context.Context) (GenSpec, error) {
	spec := GenSpec{
		Count:           1,
		GenderFemalePct: 50,
		WalletMin:       1000000,
		WalletMax:       30000000,
	}
	// Archetypes: the form's opening split (defaultArchetypeWeights), an even hand except where
	// an archetype carries its own default share.
	weights := defaultArchetypeWeights()
	for _, k := range archetypeKeys() {
		spec.Archetypes = append(spec.Archetypes, ratio{Key: k, Weight: weights[k]})
	}
	// Disposition: mostly inside the law.
	for _, k := range dispositionKeys() {
		w := 900
		if k == "criminal" {
			w = 100
		}
		spec.Dispositions = append(spec.Dispositions, ratio{Key: k, Weight: w})
	}
	// Maturity: mostly green.
	for _, k := range maturityKeys() {
		w := 50
		switch k {
		case "green":
			w = 700
		case "seasoned":
			w = 250
		}
		spec.Maturities = append(spec.Maturities, ratio{Key: k, Weight: w})
	}
	// Timezones: the form's defaults, in tenths of a percent.
	for _, o := range tzWindowOpts {
		spec.Timezones = append(spec.Timezones, ratio{Key: o.Key, Weight: int(o.Default*10 + 0.5)})
	}
	// Bloodlines: an even split over whatever the name pools can actually
	// mint, which is what the form's "Even split" button produces.
	bls := nameableBloodlines()
	if len(bls) == 0 {
		return spec, errors.New("no bloodline has a name pool — nothing can be generated")
	}
	even := 1000 / len(bls)
	if even <= 0 {
		even = 1
	}
	for _, bl := range bls {
		spec.Bloodlines = append(spec.Bloodlines,
			ratio{Key: strconv.FormatInt(bl, 10), Weight: even})
	}
	return spec, nil
}

// ── the panel's side ────────────────────────────────────────────────────────

// migrationTarget is one account the migration form may hand a pilot to.
type migrationTarget struct {
	AccountID   int64
	AccountName string
	Held        int
	Free        int
}

// listMigrationTargets is every player account, with how full it is. Full
// accounts are returned, not filtered, so an operator can tell full from a broken page.
func (m *Mod) listMigrationTargets(ctx context.Context) ([]migrationTarget, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT a.accountID, a.accountName, COUNT(c.characterID)
		  FROM account a
		  LEFT JOIN character_ c ON c.accountID = a.accountID
		 GROUP BY a.accountID, a.accountName
		 ORDER BY a.accountName`)
	if err != nil {
		return nil, fmt.Errorf("listMigrationTargets: %w", err)
	}
	defer rows.Close()
	var out []migrationTarget
	for rows.Next() {
		var t migrationTarget
		if err := rows.Scan(&t.AccountID, &t.AccountName, &t.Held); err != nil {
			return nil, fmt.Errorf("listMigrationTargets scan: %w", err)
		}
		// botSeqOf, not a SQL REGEXP: the generator, the wipe range and this
		// list have to agree about what a RoleBot account is.
		if _, isBotAcct := botSeqOf(t.AccountName); isBotAcct {
			continue
		}
		t.Free = charsPerAccount - t.Held
		if t.Free < 0 {
			t.Free = 0
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// migrationCandidate is the Capsuleer the form resolved, echoed back so the
// operator confirms a name rather than a number.
type migrationCandidate struct {
	CharacterID   int64
	CharacterName string
	AccountName   string
	Archetype     string
	CorpName      string
}

// resolveMigrationSubject turns what the operator typed, a characterID or a name,
// into one generated Capsuleer. An ambiguous name is an error naming the
// candidates, never a silent pick, because near-collisions are ordinary.
func (m *Mod) resolveMigrationSubject(ctx context.Context, q string) (migrationCandidate, error) {
	var out migrationCandidate
	q = strings.TrimSpace(q)
	if q == "" {
		return out, errors.New("name or character ID of the Capsuleer to hand over?")
	}

	where, args := `c.characterName LIKE CONCAT('%', ?, '%')`, []any{q}
	if id, err := strconv.ParseInt(q, 10, 64); err == nil && id > 0 {
		where, args = `g.characterID = ?`, []any{id}
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT g.characterID, COALESCE(c.characterName, ''), g.accountName, g.archetype,
		       COALESCE(corp.corporationName, '')
		  FROM rolebot_generated g
		  JOIN character_ c ON c.characterID = g.characterID
		  LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		 WHERE `+where+`
		 ORDER BY c.characterName
		 LIMIT 11`, args...)
	if err != nil {
		return out, fmt.Errorf("resolveMigrationSubject: %w", err)
	}
	defer rows.Close()
	var found []migrationCandidate
	for rows.Next() {
		var c migrationCandidate
		if err := rows.Scan(&c.CharacterID, &c.CharacterName, &c.AccountName,
			&c.Archetype, &c.CorpName); err != nil {
			return out, fmt.Errorf("resolveMigrationSubject scan: %w", err)
		}
		found = append(found, c)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("resolveMigrationSubject: %w", err)
	}
	switch {
	case len(found) == 0:
		return out, fmt.Errorf("no generated Capsuleer matches %q — "+
			"only the population on this tab can be handed over", q)
	case len(found) == 1:
		return found[0], nil
	}
	names := make([]string, 0, len(found))
	for i, c := range found {
		if i == 10 {
			names = append(names, "…")
			break
		}
		names = append(names, fmt.Sprintf("%s (%d)", c.CharacterName, c.CharacterID))
	}
	return out, fmt.Errorf("%q matches %d Capsuleers — say which by character ID: %s",
		q, len(found), strings.Join(names, ", "))
}
