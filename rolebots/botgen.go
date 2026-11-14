package rolebots

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// Generation: turning a plan (genplan.go) into real accounts and characters.
//
// A character with no account is an NPC: accountID > 0 separates a pilot from an agent in every
// headcount, so generated pilots sit on real accounts named RoleBot<N>. The name identifies the
// population for range wipes and keeps it out of the hand-built roster; it never reaches the game.
// Order: account, character, identity, flags, traits, skills, roster row. The last step makes the
// pilot visible to the roster page, so a run that dies halfway leaves rows the purge can find.

// botAccountPrefix is the account-name prefix that defines the generated population. Changing it
// orphans every existing generated bot from the purge and the roster filter.
const botAccountPrefix = "RoleBot"

// botAccountRole matches the role every other player account carries (config.json
// authentication.role). Generated accounts are ordinary player accounts with no GM bits.
const botAccountRole int64 = 1073741826

// charsPerAccount is how many pilots share one RoleBot account: the slots of the client's character
// select screen. A batch always starts a fresh account, so a wipe range never takes pilots seeded later.
const charsPerAccount = 3

// defaultMaxBatch bounds one seed run when the operator has set no limit. It guards against a typo:
// one run creates real accounts and assets, and only one runs at a time. Raise it on the Bot
// Population page (server setting rolebots.maxSeedBatch).
const defaultMaxBatch = 500

// maxBatchLimit is the ceiling the setting can be raised to. A run this size is about an hour of
// database work.
const maxBatchLimit = 5000

// ── run status ──────────────────────────────────────────────────────────────

// genRun is the live status of a seed run. Generation runs in its own goroutine with its own context,
// so a run outlives any browser request.
type genRun struct {
	mu       sync.Mutex
	Active   bool
	Total    int
	Done     int
	Created  int
	Started  time.Time
	Finished time.Time
	Last     string
	Errs     []string
	// Label describes what is running, for the page's own banner.
	Label string
}

var currentRun = &genRun{}

func (g *genRun) snapshot() genRunView {
	g.mu.Lock()
	defer g.mu.Unlock()
	v := genRunView{
		Active: g.Active, Total: g.Total, Done: g.Done, Created: g.Created,
		Last: g.Last, Label: g.Label, Errs: append([]string(nil), g.Errs...),
	}
	if !g.Started.IsZero() {
		end := time.Now()
		if !g.Active && !g.Finished.IsZero() {
			end = g.Finished
		}
		v.Elapsed = end.Sub(g.Started).Round(time.Second).String()
	}
	if g.Total > 0 {
		v.Pct = g.Done * 100 / g.Total
	}
	return v
}

// genRunView is the template-facing snapshot, so the page never reads a field while the run goroutine writes it.
type genRunView struct {
	Active  bool
	Total   int
	Done    int
	Created int
	Pct     int
	Last    string
	Label   string
	Elapsed string
	Errs    []string
}

// begin claims the run slot and clears the previous run's counters. The fields are reset one by one:
// `*g = genRun{...}` would overwrite g.mu while it is held, and the deferred Unlock would kill the
// process with unlock of unlocked mutex.
func (g *genRun) begin(label string, total int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Active {
		return false
	}
	g.Active = true
	g.Total, g.Done, g.Created = total, 0, 0
	g.Started, g.Finished = time.Now(), time.Time{}
	g.Last, g.Label, g.Errs = "", label, nil
	return true
}

func (g *genRun) step(created bool, last string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Done++
	if created {
		g.Created++
		g.Last = last
	}
	if err != nil && len(g.Errs) < 40 {
		g.Errs = append(g.Errs, err.Error())
	}
}

func (g *genRun) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Active = false
	g.Finished = time.Now()
}

// ── sequence allocation ─────────────────────────────────────────────────────

// nextBotSeq returns the lowest sequence number safe to start a batch at. It reads the account table,
// not rolebot_generated: a half-purged or hand-made RoleBot99 account must keep its number.
func nextBotSeq(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT accountName FROM account WHERE accountName LIKE ?`, botAccountPrefix+"%")
	if err != nil {
		return 0, fmt.Errorf("nextBotSeq: %w", err)
	}
	defer rows.Close()
	max := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, err
		}
		if n, ok := botSeqOf(name); ok && n > max {
			max = n
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return max + 1, nil
}

// botSeqOf extracts N from RoleBotN. The prefix is case-insensitive and the number strict, so
// RoleBotStaging is never swept up by a range wipe.
func botSeqOf(accountName string) (int, bool) {
	if len(accountName) <= len(botAccountPrefix) {
		return 0, false
	}
	if !strings.EqualFold(accountName[:len(botAccountPrefix)], botAccountPrefix) {
		return 0, false
	}
	rest := accountName[len(botAccountPrefix):]
	n := 0
	for _, c := range rest {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// randomPassword makes a generated account unusable rather than guessable. The sim drives the
// character through the database, so no bot logs in; the operator resets the password to fly one.
func randomPassword() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Never observed; a time-seeded fallback is still better than a
		// constant, which would make every bot account share one password.
		return fmt.Sprintf("rb-%d-%d", time.Now().UnixNano(), mrand.Int63())
	}
	return hex.EncodeToString(b)
}

// ── skill helpers ───────────────────────────────────────────────────────────

// skillIDByName resolves a skill by name. Category 16 is Skill; without that
// join a name that also exists as an item (several do) resolves to the item's
// typeID and GiveSkill writes a non-skill into the character's skill flag.
func (m *Mod) skillIDByName(ctx context.Context, name string) (int64, bool) {
	var id int64
	err := m.db.QueryRowContext(ctx, `
		SELECT t.typeID FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeName = ? AND g.categoryID = 16 LIMIT 1`, name).Scan(&id)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// applyMaturity trains a new Capsuleer up to its tier from the sheet (buildSheet, sheets.go). Skills
// are granted by name and a name this database lacks is skipped. Levels are never lowered: a skill
// chargen already granted at a higher level keeps it.
func (m *Mod) applyMaturity(ctx context.Context, charID int64, sheet []skillGrant) (int, []string) {
	if len(sheet) == 0 {
		return 0, nil
	}
	have, err := m.charSkillsFor(ctx, charID)
	if err != nil {
		return 0, []string{fmt.Sprintf("read skills: %v", err)}
	}
	granted := 0
	var missing []string

	for _, g := range sheet {
		if g.Level <= 0 {
			continue
		}
		id, ok := m.skillIDByName(ctx, g.Name)
		if !ok {
			missing = append(missing, g.Name)
			continue
		}
		if have[id] >= g.Level {
			continue
		}
		forgetCharSkills(charID) // the sheet just changed (skillcache.go)
		if _, err := evedb.GiveSkill(ctx, m.db, charID, id, g.Level); err != nil {
			missing = append(missing, fmt.Sprintf("%s (%v)", g.Name, err))
			continue
		}
		have[id] = g.Level
		granted++
	}

	// A granted skill can imply prerequisites the pilot lacks; repair them here, not at the next boot (seed.go).
	if n, err := m.repairSkillPrereqs(ctx, charID); err == nil {
		granted += n
	} else {
		missing = append(missing, fmt.Sprintf("prereq repair: %v", err))
	}
	return granted, missing
}

// ── traits ──────────────────────────────────────────────────────────────────

// blendTraits combines the archetype's personality with the disposition's, then jitters by 14 either
// way, so a population does not carry identical numbers and make identical scoring decisions.
func blendTraits(rng *mrand.Rand, a archetype, b disposition, mt maturity) map[string]int {
	out := map[string]int{TraitLeadership: mt.LeadershipLean}
	add := func(src map[string]int) {
		for k, v := range src {
			out[k] += v
		}
	}
	add(a.Traits)
	add(b.Traits)
	for _, def := range traitCatalog {
		if def.Key == TraitLeadership {
			// Leadership draws from its own follower-skewed population (leadership.go); archetype and
			// disposition only move its centre.
			out[def.Key] = leadershipDraw(rng, float64(out[def.Key]))
			continue
		}
		if def.Key == TraitTolerance {
			// Tolerance is wide and polarised (tolerance.go) and leans on loyalty and wanderlust, so it sits last
			// in the catalogue.
			out[def.Key] = toleranceDraw(rng, float64(out[def.Key])+
				toleranceLean(out[TraitLoyalty], out[TraitWanderlust]))
			continue
		}
		if def.Key == TraitGrace {
			// Grace leans on aggression and patience (grace.go), so it sits after both in the catalogue.
			out[def.Key] = graceDraw(rng, float64(out[def.Key])+
				graceLean(out[TraitAggression], out[TraitPatience]))
			continue
		}
		jitter := 14
		if def.Key == TraitToil {
			// Toil decides a peaceful pilot's career, so a wider jitter lets some miners haul and some traders research.
			jitter = 25
		}
		v := out[def.Key] + rng.Intn(2*jitter+1) - jitter
		if v > 100 {
			v = 100
		}
		if v < -100 {
			v = -100
		}
		out[def.Key] = v
	}
	// The jitter can pull a researcher's draw under the career bar (Toil swings 25 either way
	// of a narrow peak). A draw that cannot be walked back keeps its jittered traits.
	if a.Key == "researcher" {
		if walked, ok := nudgeToResearch(out); ok {
			out = walked
		}
	}
	return out
}

// ── the generator ───────────────────────────────────────────────────────────

// createBot executes one planned pilot onto an account that already exists.
// The account is created by the run loop, not here, because several pilots
// share one and this function must not care which of them it is.
func (m *Mod) createBot(ctx context.Context, pb plannedBot, accountID int64, nm *nameMinter, ac *appearanceCache, rng *mrand.Rand, batchID int64) (int64, error) {
	name, err := nm.Mint(ctx, pb.BloodlineID, pb.Gender)
	if err != nil {
		return 0, err
	}

	app, p, err := ac.buildAppearance(ctx, m, rng, pb.BloodlineID, pb.Gender)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}

	in := &evedb.CharCreationInput{
		Name:         name,
		BloodlineID:  pb.BloodlineID,
		GenderID:     pb.Gender,
		AncestryID:   pb.AncestryID,
		SchoolID:     pb.SchoolID,
		DepartmentID: pb.DepartmentID,
		FieldID:      pb.FieldID,
		SpecialityID: pb.SpecialityID,
		Intelligence: pb.Attrs["intelligence"],
		Charisma:     pb.Attrs["charisma"],
		Perception:   pb.Attrs["perception"],
		Memory:       pb.Attrs["memory"],
		Willpower:    pb.Attrs["willpower"],
		Appearance:   app,
	}
	// The server's own creation path, unmodified: entity row, capsule, character_, chargen skills,
	// starting corporation, employment record and racial standing.
	charID, err := evedb.CreateCharacter(ctx, m.db, accountID, in, pb.Balance)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	// The starting wallet is ISK that did not exist a moment ago and writes
	// no journal row, so the ISK-flow ledger (iskflow.go) is told directly.
	m.noteBotISKFlow(ctx, charID, "birth", pb.Balance)

	// From here the character exists and is kept even if a later step fails: the purge can clean it.
	// Errors are collected, not fatal.
	var soft []string

	if err := addRolebot(ctx, m.db, charID); err != nil {
		soft = append(soft, err.Error())
	}
	// Its login window from the form's mix. A pilot whose own draw already
	// starts in that window gets no row (timezone.go keeps the table sparse).
	tzSlotNow := tzDraw(charID)
	if w, ok := tzWindowByKey(pb.TZWindow); ok {
		s, store := tzWindowSlot(charID, w)
		tzSlotNow = s
		if store {
			if err := storeTZSlot(ctx, m.db, charID, s, "gen"); err != nil {
				soft = append(soft, "timezone: "+err.Error())
			}
		}
	}
	tzWord := tzHoursText(tzSlotNow)
	// Its personality is the only identity given at birth: a pilot's opinion of a faction is read from
	// its employer (outlook.go).
	if err := saveTraits(ctx, m.db, charID, blendTraits(rng, pb.Archetype, pb.Disposition, pb.Maturity)); err != nil {
		soft = append(soft, "traits: "+err.Error())
	}

	// The disposition writes no profile column; it is entirely in the traits blended above (genplan.go).

	trained, missing := m.applyMaturity(ctx, charID,
		buildSheet(pb.Archetype, pb.Maturity, pb.RaceID, rng))
	if len(missing) > 0 {
		soft = append(soft, "skills not granted: "+strings.Join(missing, ", "))
	}

	// A past to go with the sheet: years of career, employers in order, a home at the last (history.go).
	// It follows the skills because the sheet is its evidence.
	if err := m.applyEmploymentHistory(ctx, charID, pb.RaceID, pb.Maturity,
		pb.Disposition.Key == "criminal", rng); err != nil {
		soft = append(soft, "history: "+err.Error())
	}

	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_generated
			(characterID, accountID, accountName, seq, slot, archetype, lawfulness, maturity,
			 bloodlineID, gender, poseSource, batchID, createdDate)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		charID, accountID, pb.AccountName, pb.Seq, pb.Slot, pb.Archetype.Key, pb.Disposition.Key,
		pb.Maturity.Key, pb.BloodlineID, pb.Gender, p.ID, batchID, time.Now().Unix(),
	); err != nil {
		soft = append(soft, "generated marker: "+err.Error())
	}

	// Every generated pilot is active from birth; the timezone gate and the
	// online cap decide when it is actually logged in.
	if err := setRolebotEnabled(ctx, m.db, charID, true); err != nil {
		soft = append(soft, "enable: "+err.Error())
	}

	m.logBot(charID, logInfo, "roster",
		"generated on %s — %s %s, %s career out of %s, %s sheet (%d skill grants), %s, plays %s, portrait %s",
		pb.AccountName, pb.BloodlineName, genderWord(pb.Gender), strings.TrimSpace(pb.FieldName),
		pb.SchoolName, pb.Maturity.Label, trained, pb.Disposition.Label, tzWord, p.ID)

	if len(soft) > 0 {
		return charID, fmt.Errorf("%s created with problems: %s", name, strings.Join(soft, "; "))
	}
	return charID, nil
}

func genderWord(g int64) string {
	if g == genderFemale {
		return "woman"
	}
	return "man"
}

// accountsFor is how many accounts a given number of pilots needs.
func accountsFor(pilots int) int {
	if pilots <= 0 {
		return 0
	}
	return (pilots + charsPerAccount - 1) / charsPerAccount
}

// StartGeneration starts a seed run in the background. Count is pilots; the accounts to hold them are
// worked out here. It errors only when the run cannot start (a run going, a spec that cannot produce
// a legal pilot), before anything exists.
func (m *Mod) StartGeneration(spec GenSpec, maxBatch int) error {
	if spec.Count <= 0 {
		return errors.New("count must be at least 1")
	}
	if maxBatch <= 0 {
		maxBatch = defaultMaxBatch
	}
	if spec.Count > maxBatch {
		return fmt.Errorf("one run generates at most %d Capsuleers (asked for %d) — raise the run limit on this page, or run the seed twice",
			maxBatch, spec.Count)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(60+spec.Count*8)*time.Second)

	seed := time.Now().UnixNano()
	rng := mrand.New(mrand.NewSource(seed))

	startSeq, err := nextBotSeq(ctx, m.db)
	if err != nil {
		cancel()
		return err
	}
	plan, err := m.buildPlan(ctx, spec, rng, startSeq)
	if err != nil {
		cancel()
		return err
	}
	nAcct := accountsFor(len(plan))
	if !currentRun.begin(fmt.Sprintf("seeding %d Capsuleers on %d account(s), %s%d–%s%d",
		len(plan), nAcct, botAccountPrefix, startSeq, botAccountPrefix, startSeq+nAcct-1), len(plan)) {
		cancel()
		return errors.New("a generation or wipe run is already in progress")
	}

	goSafe("generation run", func() {
		defer cancel()
		defer currentRun.end()
		nm := newNameMinter(m.db, rng)
		ac := newAppearanceCache()
		batchID := time.Now().Unix()
		rlogf("rolebots: seeding %d generated pilots on %d account(s) from %s%d",
			len(plan), nAcct, botAccountPrefix, startSeq)

		// The account is created once per slot-0 pilot and reused by the two behind it. An account whose
		// whole slate failed is deleted.
		var acctID int64
		var acctName string
		acctPilots := 0
		dropEmptyAccount := func() {
			if acctID != 0 && acctPilots == 0 {
				_, _ = m.db.ExecContext(ctx, `DELETE FROM account WHERE accountID = ?`, acctID)
				rlogf("rolebots: %s produced no pilots — account removed", acctName)
			}
		}

		for _, pb := range plan {
			if ctx.Err() != nil {
				currentRun.step(false, "", fmt.Errorf("run timed out after %d pilots", currentRun.snapshot().Created))
				break
			}
			if pb.Slot == 0 {
				dropEmptyAccount()
				acctPilots, acctName = 0, pb.AccountName
				id, err := evedb.CreateAccount(ctx, m.db, pb.AccountName, randomPassword(), botAccountRole)
				if err != nil {
					acctID = 0
					currentRun.step(false, "", fmt.Errorf("account %s: %w", pb.AccountName, err))
					rlogf("rolebots: seed account %s: %v", pb.AccountName, err)
					continue
				}
				acctID = id
			}
			if acctID == 0 {
				// Its account failed; the pilots behind it have nowhere to go.
				currentRun.step(false, "", fmt.Errorf("%s slot %d skipped — its account was not created", pb.AccountName, pb.Slot))
				continue
			}
			charID, err := m.createBot(ctx, pb, acctID, nm, ac, rng, batchID)
			if charID != 0 {
				acctPilots++
			}
			currentRun.step(charID != 0, fmt.Sprintf("%s slot %d (%s)", pb.AccountName, pb.Slot+1, pb.Archetype.Label), err)
			if err != nil {
				rlogf("rolebots: seed %s slot %d: %v", pb.AccountName, pb.Slot+1, err)
			}
		}
		dropEmptyAccount()

		v := currentRun.snapshot()
		rlogf("rolebots: seed finished — %d/%d pilots created, %d problem(s)", v.Created, v.Total, len(v.Errs))
		m.syncPresence()
	})
	return nil
}
