package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// Bot CEOs: a rostered pilot that runs a player corporation.
// A CEO's personality resolves to a Charter (corpceo_charter.go) once per
// think, and each power it drives writes to a seam something else reads:
//   identity: rolebot_corps.tags; directive: rolebot_corp_directive;
//   hr: chrApplications.status; roles: chrCorporationRoles, rolebot_corp_ranks;
//   treasury: corporation.taxRate; estate: RentOffice, SetCorpHQ;
//   business: corp market orders (corpstock.go); diplomacy: crpStandings
//   (corprel.go); war: core wars and bills, merc market (corpceo_war.go).
// Every decision lands in the CEO ledger (rolebot_ceo_actions) with its reason.

// The powers, in think order. Identity runs first (its tags feed roles and
// diplomacy), diplomacy before HR, estate after treasury, business after
// estate, and shares last (paid out of what the rest left).
const (
	powerIdentity   = "identity"
	powerDirective  = "directive"
	powerHR         = "hr"
	powerRoles      = "roles"
	powerTreasury   = "treasury"
	powerEstate     = "estate"
	powerBusiness   = "business"
	powerDiplomacy  = "diplomacy"
	powerProduction = "production"
	powerShares     = "shares"
	powerWar        = "war"
	// powerStarbase is declared in posceo.go beside the power itself.
)

var ceoPowers = []string{powerIdentity, powerDirective, powerDiplomacy, powerAlliance, powerWar, powerHR, powerRoles, powerTreasury, powerEstate, powerBusiness, powerProduction, powerStarbase, powerShares}

// ceoPowerLabel is how the panel names each power.
var ceoPowerLabel = map[string]string{
	powerIdentity:   "Identity",
	powerDirective:  "Directive",
	powerHR:         "Applications",
	powerRoles:      "Roles & titles",
	powerTreasury:   "Treasury",
	powerEstate:     "Offices & HQ",
	powerBusiness:   "Business",
	powerDiplomacy:  "Diplomacy",
	powerProduction: "Production",
	powerShares:     "Shares & dividends",
	powerWar:        "War & mercenaries",
	powerStarbase:   "Starbases",
	powerAlliance:   "Alliance",
}

const (
	// ceoSweepEveryTicks is how often the sweep looks for a CEO whose think
	// is due. The think cadence itself is the CEO's own (Charter.ThinkEverySec).
	ceoSweepEveryTicks = 60
	// ceoThinksPerPass bounds one sweep, so a restart with every CEO overdue
	// does not run them all in one pass.
	ceoThinksPerPass = 8
	// ceoActionRetentionSec is how long the ledger keeps a row.
	ceoActionRetentionSec = 90 * 86400
	// ceoPruneEverySec is how often the sweep prunes the ledger.
	ceoPruneEverySec = 3600
	ceoPruneBatch    = 5000

	ceoOriginOperator   = "operator"
	ceoOriginFounded    = "founded"
	ceoOriginSuccession = "succession"
	// ceoOriginActivated is a CEO that took charge because its pilot is an
	// active bot sitting in a player corporation's chair (reconcileCEOSeats).
	ceoOriginActivated = "activated"
)

// corpCEO is one rolebot_ceos row.
type corpCEO struct {
	CorpID      int64
	CharID      int64
	Origin      string
	InstalledBy string
	InstalledAt int64
	Paused      bool
	PowersOff   map[string]bool
	LastThinkAt int64
	NextThinkAt int64
	// LastReviewAt is when the Corporate Review last ran: the roles, estate
	// and share-book powers, weekly (reviewDue). 0 = never, so due now.
	LastReviewAt int64
	CharterJSON  string
	// HQMovable is the operator's leave to move the headquarters. False, the
	// default, bans every HQ move; offices may still be rented.
	HQMovable bool
}

// powerOn reports whether a power may act this think.
func (c *corpCEO) powerOn(p string) bool { return !c.Paused && !c.PowersOff[p] }

// ceoReviewEverySec is the Corporate Review's cadence. The roles pass, the
// estate hunts and the share book are read-mostly and run on this weekly
// cycle instead of every think.
const ceoReviewEverySec = 7 * 86400

// reviewDue reports whether this think runs the Corporate Review: a week
// since the last one, or a department head asking for the seats to follow a
// change in demand (seatPassWanted).
func (c *corpCEO) reviewDue(now int64) bool {
	return now-c.LastReviewAt >= ceoReviewEverySec || seatPassRequested(c.CorpID)
}

func (c *corpCEO) powersOffCSV() string {
	var out []string
	for _, p := range ceoPowers {
		if c.PowersOff[p] {
			out = append(out, p)
		}
	}
	return strings.Join(out, ",")
}

func parsePowersOff(s string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

// ── the index ───────────────────────────────────────────────────────────────

// ceoIndex maps a managed corporation to its CEO's charID. Hot paths read it
// (the directive term runs on every bot's planning pass), so it is rebuilt on
// every change and every sweep instead of queried.
var ceoIndex atomic.Pointer[map[int64]int64]

// ceoByChar is the reverse of ceoIndex: charID to corporationID. Rebuilt beside
// it, because the skill planner asks managedCEOOf per pilot.
var ceoByChar atomic.Pointer[map[int64]int64]

// ceoWakes are corporations whose CEO thinks on the next sweep whatever its
// NextThinkAt says, for events between thinks (a tower planned or anchored).
// Memory only: a restart loses it.
var ceoWakes sync.Map // corpID to struct{}

// wakeCEO asks for corpID's CEO to think on the next sweep.
func wakeCEO(corpID int64) {
	if corpID != 0 {
		ceoWakes.Store(corpID, struct{}{})
	}
}

func ceoWoken(corpID int64) bool { _, ok := ceoWakes.Load(corpID); return ok }

// managedCEO answers the CEO of a managed corporation.
func managedCEO(corpID int64) (int64, bool) {
	p := ceoIndex.Load()
	if p == nil || corpID == 0 {
		return 0, false
	}
	id, ok := (*p)[corpID]
	return id, ok
}

func (m *Mod) refreshCEOIndex(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT corporationID, characterID FROM rolebot_ceos`)
	if err != nil {
		rlogf("rolebots: CEO index: %v", err)
		return
	}
	defer rows.Close()
	idx := map[int64]int64{}
	rev := map[int64]int64{}
	for rows.Next() {
		var corpID, charID int64
		if rows.Scan(&corpID, &charID) == nil {
			idx[corpID] = charID
			rev[charID] = corpID
		}
	}
	ceoIndex.Store(&idx)
	ceoByChar.Store(&rev)
}

// ── the table ───────────────────────────────────────────────────────────────

const ceoCols = `corporationID, characterID, origin, installedBy, installedAt,
	paused, powersOff, lastThinkAt, nextThinkAt, COALESCE(charterJSON, ''), hqMovable, lastReviewAt`

func scanCEO(sc interface{ Scan(...any) error }) (*corpCEO, error) {
	c := &corpCEO{}
	var paused, hqMovable int64
	var off string
	if err := sc.Scan(&c.CorpID, &c.CharID, &c.Origin, &c.InstalledBy, &c.InstalledAt,
		&paused, &off, &c.LastThinkAt, &c.NextThinkAt, &c.CharterJSON, &hqMovable, &c.LastReviewAt); err != nil {
		return nil, err
	}
	c.Paused = paused != 0
	c.HQMovable = hqMovable != 0
	c.PowersOff = parsePowersOff(off)
	return c, nil
}

func getCEO(ctx context.Context, db *sql.DB, corpID int64) (*corpCEO, error) {
	c, err := scanCEO(db.QueryRowContext(ctx,
		`SELECT `+ceoCols+` FROM rolebot_ceos WHERE corporationID = ?`, corpID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getCEO: %w", err)
	}
	return c, nil
}

func listCEOs(ctx context.Context, db *sql.DB) ([]*corpCEO, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+ceoCols+` FROM rolebot_ceos ORDER BY corporationID`)
	if err != nil {
		return nil, fmt.Errorf("listCEOs: %w", err)
	}
	defer rows.Close()
	var out []*corpCEO
	for rows.Next() {
		c, err := scanCEO(rows)
		if err != nil {
			return nil, fmt.Errorf("listCEOs scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func saveCEO(ctx context.Context, db *sql.DB, c *corpCEO) error {
	paused, hqMovable := 0, 0
	if c.Paused {
		paused = 1
	}
	if c.HQMovable {
		hqMovable = 1
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_ceos
			(corporationID, characterID, origin, installedBy, installedAt,
			 paused, powersOff, lastThinkAt, nextThinkAt, charterJSON, hqMovable, lastReviewAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			characterID = VALUES(characterID), origin = VALUES(origin),
			installedBy = VALUES(installedBy), installedAt = VALUES(installedAt),
			paused = VALUES(paused), powersOff = VALUES(powersOff),
			lastThinkAt = VALUES(lastThinkAt), nextThinkAt = VALUES(nextThinkAt),
			charterJSON = VALUES(charterJSON), hqMovable = VALUES(hqMovable),
			lastReviewAt = VALUES(lastReviewAt)`,
		c.CorpID, c.CharID, c.Origin, truncate(c.InstalledBy, 64), c.InstalledAt,
		paused, c.powersOffCSV(), c.LastThinkAt, c.NextThinkAt, c.CharterJSON, hqMovable, c.LastReviewAt)
	if err != nil {
		return fmt.Errorf("saveCEO: %w", err)
	}
	return nil
}

// migrateCorpTitleColumn adds rolebot_corps.manageTitles, the operator's
// opt-in to title sync on a player corporation that has no CEO.
func migrateCorpTitleColumn(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_corps'
		  AND COLUMN_NAME = 'manageTitles'`).Scan(&have); err != nil {
		return fmt.Errorf("migrateCorpTitleColumn probe: %w", err)
	}
	if have > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE rolebot_corps ADD COLUMN manageTitles TINYINT NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("migrateCorpTitleColumn: %w", err)
	}
	return nil
}

// migrateCEOHQMovableColumn adds rolebot_ceos.hqMovable, DEFAULT 0: a CEO may
// not move its headquarters until the operator allows it.
func migrateCEOHQMovableColumn(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_ceos'
		  AND COLUMN_NAME = 'hqMovable'`).Scan(&have); err != nil {
		return fmt.Errorf("migrateCEOHQMovableColumn probe: %w", err)
	}
	if have > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE rolebot_ceos ADD COLUMN hqMovable TINYINT NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("migrateCEOHQMovableColumn: %w", err)
	}
	return nil
}

// settingFoundedHQMovable marks the one-time grant below as done.
const settingFoundedHQMovable = "rolebots.corp.foundedHQMovableGranted"

// grantFoundedHQMovable lets every bot-founded corporation's CEO move its
// headquarters, once: the corporations founded before installCEO granted it.
// An operator who bans one again afterwards is not overruled.
func grantFoundedHQMovable(ctx context.Context, db *sql.DB) error {
	if _, done, err := evedb.GetServerSetting(ctx, db, settingFoundedHQMovable); err != nil || done {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE rolebot_ceos SET hqMovable = 1 WHERE origin = ?`, ceoOriginFounded); err != nil {
		return fmt.Errorf("grantFoundedHQMovable: %w", err)
	}
	return evedb.SetServerSetting(ctx, db, settingFoundedHQMovable, "1")
}

// ── the ledger ──────────────────────────────────────────────────────────────

// ceoLog records one decision: a ledger row for the CEO pages and a line in
// the CEO's activity log. It writes synchronously so no decision is lost in a
// batch.
func (m *Mod) ceoLog(ctx context.Context, c *corpCEO, power, verb string, subjectID int64, amount float64, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	// An officer doing the work (corpstaff.go) is named on the line and
	// recorded as its actor; the CEO's own lines carry actor 0.
	var actorID int64
	if a, ok := staffActorOf(ctx); ok {
		detail = a.label() + " " + detail
		actorID = a.CharID
	}
	at := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_ceo_actions
			(corporationID, ceoID, actorID, at, power, verb, subjectID, amount, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CorpID, c.CharID, actorID, at, power, verb, subjectID, amount, truncate(detail, 255)); err != nil {
		rlogf("rolebots: CEO ledger corp %d: %v", c.CorpID, err)
	}
	if tc := thinkCacheOf(ctx, c.CorpID); tc != nil {
		tc.mu.Lock()
		if tc.actionsOK {
			tc.actions[verb] = at
		}
		tc.mu.Unlock()
	}
	m.logBot(c.CharID, logInfo, "ceo", "%s", detail)
	if actorID != 0 && actorID != c.CharID {
		m.logBot(actorID, logInfo, "corp", "%s", detail)
	}
	// An NPC chair (npcchair.go) keeps its ledger and its chat line, but
	// sends no bulletins and has no alliance.
	if c.Origin == ceoOriginNPC {
		m.chatterCEO(ctx, c, actorID, power, verb, subjectID, amount, fmt.Sprintf(format, args...))
		return
	}
	if kind := ceoNewsKind(power, verb); kind != "" && corpNewsOn() {
		if actorID == 0 {
			detail = m.charNameOr(ctx, c.CharID) + " " + detail
		}
		var about int64
		if (kind == newsOfficers || kind == newsSeats) && m.corpHasHumans(ctx, c.CorpID) && m.isCapsuleer(ctx, subjectID) {
			about = subjectID
		}
		m.queueCorpNews(ctx, c.CorpID, kind, detail, about)
	}
	// The same decision, said aloud in corp chat if a person is there to
	// hear it (chatter.go). The officer who did it speaks; the CEO for its own.
	m.chatterCEO(ctx, c, actorID, power, verb, subjectID, amount, fmt.Sprintf(format, args...))
	// And the alliance layer's decisions in the alliance channel, answered by
	// a pilot of another member corporation (chatter_alliance.go).
	m.chatterAllianceEvent(ctx, c, actorID, power, verb, subjectID, amount)
	// And, when it is news, on the Intergalactic Summit (summit_alliance.go).
	m.summitCEO(ctx, c, power, verb, subjectID, amount, fmt.Sprintf(format, args...))
}

// ceoNewsKind is which ledger lines a corporation's members are told about in
// the bulletin (corpnews.go), and under which heading. Everything else stays
// in the operator's ledger.
func ceoNewsKind(power, verb string) string {
	switch verb {
	case "directive":
		return newsDirective
	case "promote", "demote", "department":
		return newsOfficers
	case "seat", "unseat":
		// Fleet commands and seats change with the roster; routine.
		return newsSeats
	case "order":
		return newsFleet
	case "hq", "rent.drop", "rent.release":
		return newsEstate
	case "dividend":
		return newsMoney
	}
	if power == powerWar {
		switch verb {
		case "declare", "retract", "warover", "lapse":
			return newsWar
		}
	}
	if power == powerDiplomacy {
		switch verb {
		case "ally", "friend", "rival", "enemy", "withdraw",
			"letter", "answer", "pact", "pactbreak", "peace", "reparations", "envoylost":
			return newsRelations
		}
	}
	if power == powerAlliance {
		switch verb {
		case "found", "apply", "joined", "leave", "kick", "accept", "reject", "support", "executor":
			return newsRelations
		case "declare", "refuse":
			return newsWar
		}
	}
	return ""
}

// ceoAction is one ledger row as the panel reads it.
type ceoAction struct {
	ActionID    int64
	CorpID      int64
	CEOID       int64
	At          int64
	Power       string
	Verb        string
	SubjectID   int64
	SubjectName string
	Amount      float64
	Detail      string
	// ActorID is the officer that did the work (corpstaff.go), 0 for the
	// CEO; ActorName is its name.
	ActorID   int64
	ActorName string
}

func (a ceoAction) PowerLabel() string {
	if l, ok := ceoPowerLabel[a.Power]; ok {
		return l
	}
	return a.Power
}

func (a ceoAction) AgeText() string { return fmtDuration(clock.Unix()-a.At) + " ago" }

// listCEOActions reads the ledger newest first. corpID 0 is every
// corporation; power and verb narrow it when set.
func (m *Mod) listCEOActions(ctx context.Context, corpID int64, power, verb string, limit, offset int) ([]ceoAction, error) {
	where := []string{"1 = 1"}
	var args []any
	if corpID != 0 {
		where = append(where, "a.corporationID = ?")
		args = append(args, corpID)
	}
	if power != "" {
		where = append(where, "a.power = ?")
		args = append(args, power)
	}
	if verb != "" {
		where = append(where, "a.verb = ?")
		args = append(args, verb)
	}
	args = append(args, limit, offset)
	rows, err := m.db.QueryContext(ctx, `
		SELECT a.actionID, a.corporationID, a.ceoID, a.at, a.power, a.verb,
		       a.subjectID, COALESCE(c.characterName, co.corporationName, ''),
		       a.amount, a.detail, a.actorID, COALESCE(ac.characterName, '')
		  FROM rolebot_ceo_actions a
		  LEFT JOIN character_ c ON c.characterID = a.subjectID
		  LEFT JOIN character_ ac ON ac.characterID = a.actorID
		  LEFT JOIN corporation co ON co.corporationID = a.subjectID
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY a.actionID DESC
		 LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("listCEOActions: %w", err)
	}
	defer rows.Close()
	var out []ceoAction
	for rows.Next() {
		var a ceoAction
		if err := rows.Scan(&a.ActionID, &a.CorpID, &a.CEOID, &a.At, &a.Power, &a.Verb,
			&a.SubjectID, &a.SubjectName, &a.Amount, &a.Detail, &a.ActorID, &a.ActorName); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// pruneCEOActions drops ledger rows past retention, in batches, by the (at)
// index. It has no ORDER BY: a filesort over the whole table would hold the
// connection.
func (m *Mod) pruneCEOActions(ctx context.Context) {
	cutoff := clock.Unix() - ceoActionRetentionSec
	for i := 0; i < 20; i++ {
		res, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_ceo_actions WHERE at < ? LIMIT ?`, cutoff, ceoPruneBatch)
		if err != nil {
			rlogf("rolebots: CEO ledger prune: %v", err)
			return
		}
		if n, _ := res.RowsAffected(); n < ceoPruneBatch {
			return
		}
	}
}

// ── install, found, remove ──────────────────────────────────────────────────

// installCEO puts a rostered bot in a player corporation's chair and hands it
// the corporation. The bot joins first if it is not a member, bypassing the
// member limit like any GM join.
func (m *Mod) installCEO(ctx context.Context, charID, corpID int64, origin, by string) error {
	var onRoster int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_characters WHERE characterID = ?`, charID).Scan(&onRoster); err != nil {
		return fmt.Errorf("installCEO roster: %w", err)
	}
	if onRoster == 0 {
		return fmt.Errorf("character %d is not on the rolebot roster", charID)
	}
	npc, err := isNPCCorp(ctx, m.db, corpID)
	if err != nil {
		return err
	}
	if npc {
		return fmt.Errorf("corporation %d is an NPC corporation — only a player corporation can have a bot CEO", corpID)
	}
	if other, ok := managedCEOOf(charID); ok && other != corpID {
		return fmt.Errorf("that pilot already runs corporation %d", other)
	}

	var curCorp int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&curCorp); err != nil {
		return fmt.Errorf("installCEO character: %w", err)
	}
	if curCorp != corpID {
		if corpIsSansha(ctx, m.db, curCorp) {
			return fmt.Errorf("character %d belongs to Sansha's Nation and does not leave it", charID)
		}
		if _, err := evedb.JoinCorporation(ctx, m.db, charID, corpID, curCorp); err != nil {
			return fmt.Errorf("installCEO join: %w", err)
		}
	}
	oldCEO, err := evedb.SetCorpCEO(ctx, m.db, corpID, charID)
	if err != nil {
		return fmt.Errorf("installCEO seat: %w", err)
	}

	// Keep heldCorpID equal to the new corporation: a mismatch reads as another
	// mover and the displacement check would send the new CEO to its school corp.
	if rev, err := getCorpReview(ctx, m.db, charID); err == nil {
		if curCorp != corpID {
			rev.FromCorpID = curCorp
			rev.LastMoveAt = clock.Unix()
			rev.Moves++
		}
		rev.HeldCorpID = corpID
		if err := saveCorpReview(ctx, m.db, rev); err != nil {
			return fmt.Errorf("installCEO review: %w", err)
		}
	}

	// Open to bots: a bot CEO recruits from the roster, so the door must be open.
	if facts, err := getAnyCorpFacts(ctx, m.db, corpID); err == nil {
		p := facts.Policy
		p.CorpID = corpID
		p.Joinable = true
		if err := saveCorpPolicy(ctx, m.db, &p); err != nil {
			return err
		}
	}

	now := clock.Unix()
	c := &corpCEO{
		CorpID: corpID, CharID: charID, Origin: origin, InstalledBy: by,
		InstalledAt: now, NextThinkAt: now, PowersOff: map[string]bool{},
	}
	if prev, err := getCEO(ctx, m.db, corpID); err == nil && prev != nil {
		c.Paused, c.PowersOff, c.HQMovable = prev.Paused, prev.PowersOff, prev.HQMovable
	} else if origin == ceoOriginFounded {
		// A corporation a bot founded is the bot's to move; the ban is for chairs
		// the operator hands a bot in someone else's corporation.
		c.HQMovable = true
	}
	if err := saveCEO(ctx, m.db, c); err != nil {
		return err
	}
	m.refreshCEOIndex(ctx)
	forgetCorpHome(charID)
	m.syncPresence()

	name := corpNameOf(ctx, m.db, corpID)
	switch {
	case curCorp != corpID:
		m.ceoLog(ctx, c, powerRoles, "install", charID, 0,
			"joined %s and took the chair as CEO (%s)", name, origin)
	case oldCEO != charID:
		m.ceoLog(ctx, c, powerRoles, "install", charID, 0,
			"took the chair of %s as CEO (%s)", name, origin)
	default:
		m.ceoLog(ctx, c, powerRoles, "install", charID, 0,
			"now runs %s (%s)", name, origin)
	}
	// Titles wait for the first think, which re-deals tags and seats. Only a CEO
	// whose roles power is off (and never syncs) gets them here.
	if !c.powerOn(powerRoles) {
		m.syncCorpTitles(ctx, corpID)
	}
	return nil
}

// managedCEOOf answers which managed corporation charID runs, if any.
func managedCEOOf(charID int64) (int64, bool) {
	p := ceoByChar.Load()
	if p == nil || charID == 0 {
		return 0, false
	}
	corpID, ok := (*p)[charID]
	return corpID, ok
}

// removeCEO hands a corporation back to the operator. The bot keeps its
// in-game chair and only stops deciding. Reached when the pilot is deactivated,
// leaves the roster or loses the chair.
func (m *Mod) removeCEO(ctx context.Context, corpID int64, why string) error {
	c, err := getCEO(ctx, m.db, corpID)
	if err != nil || c == nil {
		return err
	}
	m.ceoLog(ctx, c, powerRoles, "release", c.CharID, 0,
		"stopped running %s — %s", corpNameOf(ctx, m.db, corpID), why)
	if _, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_ceos WHERE corporationID = ?`, corpID); err != nil {
		return fmt.Errorf("removeCEO: %w", err)
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_directive WHERE corporationID = ?`, corpID); err != nil {
		return fmt.Errorf("removeCEO directive: %w", err)
	}
	forgetCorp(corpID) // every corp-keyed cache, the directive's included (forgetreg.go)
	// The declarations stay as the corporation's standings, for the operator to
	// keep or clear.
	if err := m.forgetCorpRelations(ctx, corpID); err != nil {
		return fmt.Errorf("removeCEO relations: %w", err)
	}
	// An envoy on the road comes home with nothing to carry (corpenvoy.go).
	if err := m.savePosts(ctx, corpID, famDiplomacy, nil); err != nil {
		return fmt.Errorf("removeCEO envoys: %w", err)
	}
	// A tender nobody can close is withdrawn. Running contracts stay: the merc
	// side ends one whose client stops settling (merc.go mercOrphanedContracts).
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_merc_jobs SET status = 'withdrawn', closedAt = ?, reason = 'the CEO left'
		 WHERE clientCorpID = ? AND status IN ('open', 'haggling')`, clock.Unix(), corpID); err != nil {
		return fmt.Errorf("removeCEO merc jobs: %w", err)
	}
	m.refreshCEOIndex(ctx)
	return nil
}

// ceoSuccessor picks the rostered member who takes a managed corporation's
// chair when its CEO leaves the roster: the one that most wants to lead,
// weighted by how long it has served. 0 when nobody is left.
func (m *Mod) ceoSuccessor(ctx context.Context, corpID, exclude int64) int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, COALESCE(c.corporationDateTime, 0)
		  FROM character_ c
		  JOIN rolebot_characters b ON b.characterID = c.characterID
		 WHERE c.corporationID = ? AND c.characterID <> ? AND b.enabled = 1`, corpID, exclude)
	if err != nil {
		return 0
	}
	type cand struct {
		id    int64
		since int64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.since) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()
	best, bestScore := int64(0), -1.0
	now := win32Now()
	for _, c := range cands {
		traits, err := getTraits(ctx, m.db, c.id)
		if err != nil {
			continue
		}
		days := 1.0
		if c.since > 0 && now > c.since {
			days += float64(now-c.since) / (864000000000.0) // FILETIME ticks per day
		}
		if s := resolveTuning(traits).CommandDrive * days; s > bestScore {
			best, bestScore = c.id, s
		}
	}
	return best
}

// succeedCEO is the purge's call when a managed corporation's CEO is about to
// be deleted and no human member remains to inherit: another rostered member
// takes the chair. Reports whether it found one.
func (m *Mod) succeedCEO(ctx context.Context, corpID, leaving int64) bool {
	if _, ok := managedCEO(corpID); !ok {
		return false
	}
	heir := m.ceoSuccessor(ctx, corpID, leaving)
	if heir == 0 {
		return false
	}
	if err := m.installCEO(ctx, heir, corpID, ceoOriginSuccession, "succession"); err != nil {
		rlogf("rolebots: CEO succession corp %d → %d: %v", corpID, heir, err)
		return false
	}
	return true
}

// corpNameOf is a best-effort name for log lines.
func corpNameOf(ctx context.Context, db *sql.DB, corpID int64) string {
	var name string
	if err := db.QueryRowContext(ctx,
		`SELECT corporationName FROM corporation WHERE corporationID = ?`, corpID).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("corporation %d", corpID)
	}
	return name
}

// ── the chair follows the roster switch ─────────────────────────────────────

// ceoAdoptFailed remembers the last install error per corporation, so a
// chair that cannot be taken logs once rather than every sweep.
var ceoAdoptFailed sync.Map // corpID to error string

// reconcileCEOSeats ties running a corporation to the pilot being an active
// bot: an enabled rostered pilot in a player corporation's chair
// (corporation.ceoID) runs it, and a deactivated or departed one hands it
// back. Runs at the head of every CEO sweep.
func (m *Mod) reconcileCEOSeats(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT co.corporationID, co.ceoID
		  FROM corporation co
		  JOIN rolebot_characters b ON b.characterID = co.ceoID AND b.enabled = 1
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = co.corporationID
		  LEFT JOIN rolebot_ceos rc ON rc.corporationID = co.corporationID
		 WHERE n.corporationID IS NULL AND COALESCE(co.deleted, 0) = 0
		   AND (rc.corporationID IS NULL OR rc.characterID <> co.ceoID)`)
	if err != nil {
		rlogf("rolebots: CEO seats: %v", err)
		return
	}
	type seat struct{ corpID, charID int64 }
	var adopt []seat
	for rows.Next() {
		var s seat
		if rows.Scan(&s.corpID, &s.charID) == nil {
			adopt = append(adopt, s)
		}
	}
	rows.Close()

	var drop []int64
	if dr, err := m.db.QueryContext(ctx, `
		SELECT rc.corporationID
		  FROM rolebot_ceos rc
		  LEFT JOIN rolebot_characters b ON b.characterID = rc.characterID
		 WHERE b.characterID IS NULL OR b.enabled = 0`); err == nil {
		for dr.Next() {
			var id int64
			if dr.Scan(&id) == nil {
				drop = append(drop, id)
			}
		}
		dr.Close()
	} else {
		rlogf("rolebots: CEO seats: %v", err)
	}

	for _, corpID := range drop {
		if ctx.Err() != nil {
			return
		}
		if err := m.removeCEO(ctx, corpID, "the CEO was deactivated"); err != nil {
			rlogf("rolebots: CEO release corp %d: %v", corpID, err)
		}
	}
	for _, s := range adopt {
		if ctx.Err() != nil {
			return
		}
		m.adoptCEOSeat(ctx, s.corpID, s.charID)
	}
}

// adoptCEOSeat hands a corporation to the active bot in its chair.
func (m *Mod) adoptCEOSeat(ctx context.Context, corpID, charID int64) {
	if err := m.installCEO(ctx, charID, corpID, ceoOriginActivated, "roster"); err != nil {
		msg := err.Error()
		if prev, ok := ceoAdoptFailed.Load(corpID); !ok || prev.(string) != msg {
			rlogf("rolebots: CEO take charge corp %d (pilot %d): %v", corpID, charID, err)
		}
		ceoAdoptFailed.Store(corpID, msg)
		return
	}
	ceoAdoptFailed.Delete(corpID)
}

// syncCEOSeat applies the roster switch to one pilot's chair right away: an
// activated CEO takes charge, a deactivated one hands the corporation back.
// Nothing happens for a pilot that is not a player corporation's CEO.
func (m *Mod) syncCEOSeat(ctx context.Context, charID int64) {
	if corpID, ok := managedCEOOf(charID); ok {
		if rb, err := getRolebot(ctx, m.db, charID); err == nil && (rb == nil || !rb.Enabled) {
			if err := m.removeCEO(ctx, corpID, "the CEO was deactivated"); err != nil {
				rlogf("rolebots: CEO release corp %d: %v", corpID, err)
			}
		}
		return
	}
	var corpID int64
	err := m.db.QueryRowContext(ctx, `
		SELECT co.corporationID
		  FROM corporation co
		  JOIN rolebot_characters b ON b.characterID = co.ceoID AND b.enabled = 1
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = co.corporationID
		 WHERE co.ceoID = ? AND n.corporationID IS NULL AND COALESCE(co.deleted, 0) = 0
		 LIMIT 1`, charID).Scan(&corpID)
	if err != nil || corpID == 0 {
		return
	}
	m.adoptCEOSeat(ctx, corpID, charID)
}

// ── the think loop ──────────────────────────────────────────────────────────

var (
	ceoSweepBusy atomic.Bool
	ceoPrunedAt  atomic.Int64
)

// startCEOSweep registers the pass that runs every CEO whose think is due, off
// the tick goroutine behind an overlap guard. It is gated on the master switch
// only. Each pass first runs reconcileCEOSeats.
func (m *Mod) startCEOSweep() {
	m.refreshCEOIndex(context.Background())
	m.registerPeriodic("rolebot-ceo-think", ceoSweepEveryTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		if !ceoSweepBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("CEO think sweep", func() {
			defer ceoSweepBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.ceoThinkPass(ctx)
		})
		return true
	})
}

func (m *Mod) ceoThinkPass(ctx context.Context) {
	now := clock.Unix()
	// The indexes and seat reconcile are full-table reads: run them after a think
	// wrote or every ten minutes. A roster toggle syncs its own seat (syncCEOSeat).
	if ceoPassWrote.Swap(false) || now-ceoIndexesAt.Load() >= 600 {
		ceoIndexesAt.Store(now)
		m.refreshCEOIndex(ctx)
		m.refreshArmIndex(ctx)
		m.refreshPostIndex(ctx)
		m.reconcileCEOSeats(ctx)
	}
	if now-ceoPrunedAt.Load() >= ceoPruneEverySec {
		ceoPrunedAt.Store(now)
		m.pruneCEOActions(ctx)
		m.pruneCorpRelations(ctx)
		m.pruneCorpLedger(ctx)
		m.pruneCorpInvites(ctx)
		m.pruneCorpBench(ctx)
		m.pruneCorpHistory(ctx)
		m.pruneBotIndustryHistory(ctx)
	}
	ceos, err := listCEOs(ctx, m.db)
	if err != nil {
		rlogf("rolebots: CEO sweep: %v", err)
		return
	}
	// A woken corporation goes first, then the most overdue.
	sort.Slice(ceos, func(i, j int) bool {
		wi, wj := ceoWoken(ceos[i].CorpID), ceoWoken(ceos[j].CorpID)
		if wi != wj {
			return wi
		}
		return ceos[i].NextThinkAt < ceos[j].NextThinkAt
	})
	ran := 0
	for _, c := range ceos {
		if ran >= ceoThinksPerPass || ctx.Err() != nil {
			return
		}
		if c.Paused || (c.NextThinkAt > now && !ceoWoken(c.CorpID)) {
			continue
		}
		// A logged-off CEO decides nothing (timezone.go, popcap.go); a wake
		// raised meanwhile waits for it to log in.
		if popIsOffline(c.CharID) {
			continue
		}
		// Cleared before the think, so a wake raised during it (planTower)
		// brings the CEO back on the next sweep.
		ceoWakes.Delete(c.CorpID)
		ran++
		ceoPassWrote.Store(true)
		if err := m.ceoThink(ctx, c); err != nil {
			rlogf("rolebots: CEO think corp %d: %v", c.CorpID, err)
		}
	}
}

// ceoThink runs one CEO's whole pass: check it still holds the chair, resolve
// its charter, run each power that is switched on, and set the next think.
func (m *Mod) ceoThink(ctx context.Context, c *corpCEO) error {
	var ceoID, inRoster, active int64
	err := m.db.QueryRowContext(ctx, `
		SELECT co.ceoID, COUNT(b.characterID), COALESCE(MAX(b.enabled), 0)
		  FROM corporation co
		  LEFT JOIN rolebot_characters b ON b.characterID = co.ceoID
		 WHERE co.corporationID = ?
		 GROUP BY co.ceoID`, c.CorpID).Scan(&ceoID, &inRoster, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return m.removeCEO(ctx, c.CorpID, "the corporation no longer exists")
	}
	if err != nil {
		return fmt.Errorf("ceoThink seat: %w", err)
	}
	if ceoID != c.CharID {
		// Somebody else took the chair (a player, the panel, a vote); the bot does
		// not fight it.
		return m.removeCEO(ctx, c.CorpID, "somebody else is CEO now")
	}
	if inRoster == 0 {
		return m.removeCEO(ctx, c.CorpID, "the CEO left the roster")
	}
	if active == 0 {
		return m.removeCEO(ctx, c.CorpID, "the CEO was deactivated")
	}

	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	traits, err := getTraits(ctx, m.db, c.CharID)
	if err != nil {
		return err
	}
	p := pilotIdentity{CharID: c.CharID, Traits: traits, Tune: resolveTuning(traits)}
	ch := m.chairCharter(ctx, c.CorpID, resolveCharter(p))

	// One hand on the corporation at a time: staff desks (corpstaff.go) skip a
	// corporation whose CEO is thinking. Staff is read once; roles re-reads it.
	lk := corpOpLock(c.CorpID)
	lk.Lock()
	defer lk.Unlock()
	ctx = withOpLockHeld(ctx)
	// One read of the membership, its traits, the facts row and the action
	// ledger for the whole think (thinkctx.go).
	ctx = withThinkCache(ctx, c.CorpID)
	m.preloadThink(ctx, c.CorpID, c.CharID, facts)
	if s, err := m.loadCorpStaff(ctx, c.CorpID, c.CharID); err == nil {
		ctx = withCorpStaff(ctx, s)
	}
	// The Corporate Review (reviewDue): weekly, the roles pass, the estate
	// hunts and the share book.
	review := c.reviewDue(clock.Unix())

	run := func(power string, fn func() error) {
		if !c.powerOn(power) || ctx.Err() != nil {
			return
		}
		if err := fn(); err != nil {
			m.logBot(c.CharID, logError, "ceo", "%s: %v", ceoPowerLabel[power], err)
		}
	}
	// The war chest first (corpdivisions.go): an Armory standing in a
	// division a desk now needs moves out before any desk writes there.
	run(powerRoles, func() error { return m.ensureCorpDivisions(ctx, c, facts) })
	run(powerIdentity, func() error { return m.ceoIdentity(ctx, c, facts, ch) })
	run(powerDirective, func() error { return m.ceoDirective(ctx, c, ch) })
	// Diplomacy is the Director of Diplomacy's when there is one
	// (corpstaff_dept.go diplomacyDesk).
	run(powerDiplomacy, func() error {
		if m.staffCovers(ctx, c, postDirector, portDiplomacy) {
			return nil
		}
		return m.ceoDiplomacy(ctx, c, ch)
	})
	// The alliance runs after diplomacy (its allies) and before war: it files the
	// request the war power would otherwise be refused on (alliancewar.go).
	run(powerAlliance, func() error { return m.ceoAlliance(ctx, c, ch) })
	// After diplomacy, whose declarations a war is made of; before treasury, so a
	// war's commitments are booked when free capital is read.
	run(powerWar, func() error { return m.ceoWar(ctx, c, ch) })
	run(powerHR, func() error { return m.ceoHR(ctx, c, ch) })
	// The roles pass deals the departments (arms, portfolios) and the fleet seats. Weekly with
	// the review, and between reviews whenever the membership has moved (rolesRecheckDue): a
	// corporation founded with two members must not wait a week to organise its twentieth.
	run(powerRoles, func() error {
		if !review && !rolesRecheckDue(c.CorpID, facts.Members) {
			return nil
		}
		return m.ceoRoles(ctx, c, ch)
	})
	// The offices may have changed hands: the powers below ask the new staff.
	if s, err := m.loadCorpStaff(ctx, c.CorpID, c.CharID); err == nil {
		ctx = withCorpStaff(ctx, s)
	}
	run(powerTreasury, func() error { return m.ceoTreasury(ctx, c, ch) })
	run(powerEstate, func() error { return m.ceoEstate(ctx, c, ch, review) })
	// The business is the Director of Commerce's when there is one
	// (corpstaff_dept.go commerceDesk).
	run(powerBusiness, func() error {
		if m.staffCovers(ctx, c, postDirector, portCommerce) {
			return nil
		}
		return m.ceoBusiness(ctx, c, ch)
	})
	run(powerProduction, func() error { return m.ceoProduction(ctx, c, ch) })
	// After production (which reconciles the library the T2 desk reads) and
	// before shares: towers are capital the dividend must not have spent.
	run(powerStarbase, func() error { return m.ceoStarbase(ctx, c, ch) })
	// The Armory and Fleet Yard (corparmory.go): kits for the doctrine's seats
	// and the mining op's hauler, bought through the requisition desk.
	run(powerProduction, func() error { return m.quartermasterChore(ctx, c, ch, facts) })
	// What capsuleers took from the war chest: back, lost, or stolen (corpsignout.go).
	run(powerProduction, func() error { return m.signoutChore(ctx, c) })
	// Last: what the owners are paid comes out of what every other power left.
	run(powerShares, func() error { return m.ceoShares(ctx, c, ch, review) })

	now := clock.Unix()
	c.LastThinkAt = now
	if review {
		c.LastReviewAt = now
	}
	c.NextThinkAt = now + ch.ThinkEverySec + int64(splitmix64(uint64(c.CorpID)+uint64(now))%uint64(ch.ThinkEverySec/5+1))
	if b, err := json.Marshal(ch); err == nil {
		c.CharterJSON = string(b)
	}
	return saveCEO(ctx, m.db, c)
}

func init() { registerCorpForget("ceoAdoptFailed", func(id int64) { ceoAdoptFailed.Delete(id) }) }

var (
	// ceoPassWrote is set by a pass that ran a think, so the next pass
	// rebuilds the indexes; ceoIndexesAt is when they were last rebuilt.
	ceoPassWrote atomic.Bool
	ceoIndexesAt atomic.Int64
)
