package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
)

// Standing tiers on the +-10 scale (0.1 steps): devoted >= 7.5, liked >= 2.5, neutral between,
// disliked <= -2.5, despised <= -7.5. Values are derived when asked: a faction standing is the
// employer's (outlook.go) and a pilot-to-pilot opinion comes from employers' politics (standingToPilot).
const (
	StandingDevoted  = 7.5
	StandingLiked    = 2.5
	StandingDisliked = -2.5
	StandingDespised = -7.5

	// The lines a declared corporate standing (crpStandings) must reach before a bot acts on it: the
	// client's colours. At or below CorpRedLine a bot shoots the pilot outside policed space; at or
	// above CorpBlueLine it never fires on it. Between them the standing is wary, not an order.
	CorpRedLine  = -5.0
	CorpBlueLine = 5.0
)

// declaredCorpStanding is what a declared corporate standing is worth on the stance scale:
// itself past either line (both already outside stanceOf's +-2.5), nothing in between.
func declaredCorpStanding(d float64) float64 {
	if d > CorpRedLine && d < CorpBlueLine {
		return 0
	}
	return d
}

// declaredBlue reports a declared corporate standing at or above the blue line. It is the one
// opinion allowed to override the rest of a fold: a director who set another corporation to +5
// or better has given an order.
func declaredBlue(d float64) bool { return d >= CorpBlueLine }

// standingFaction is what toID names. The kind field stays because standingIDs filters on it
// and the employer outlook builds the rows.
const standingFaction = "faction"

// Standing is one entry of a pilot's outlook.
type Standing struct {
	ToID  int64
	Kind  string
	Value float64
}

// clampStanding holds a value inside the ±10 contact range at 0.1 steps
// (chrStandings clamps too, but the panel should echo back what it stored).
func clampStanding(v float64) float64 {
	v = math.Round(v*10) / 10
	if v > 10 {
		return 10
	}
	if v < -10 {
		return -10
	}
	return v
}

// standingTier is the coarse bucket a value falls in.
func standingTier(v float64) string {
	switch {
	case v >= StandingDevoted:
		return "devoted"
	case v >= StandingLiked:
		return "liked"
	case v <= StandingDespised:
		return "despised"
	case v <= StandingDisliked:
		return "disliked"
	default:
		return "neutral"
	}
}

// standingPhrase is how the sim log talks about a tier.
func standingPhrase(v float64) string {
	switch standingTier(v) {
	case "devoted":
		return "devoted to"
	case "liked":
		return "friendly with"
	case "disliked":
		return "wary of"
	case "despised":
		return "hostile to"
	default:
		return "neutral toward"
	}
}

// ── profile lookups ─────────────────────────────────────────────────────────

// StandingTo is the pilot's opinion of one faction (0 when unlisted). The outlook holds at
// most nineteen entries, so a linear scan is enough.
func (p *Profile) StandingTo(id int64) float64 {
	for _, s := range p.Standings {
		if s.ToID == id {
			return s.Value
		}
	}
	return 0
}

// outlookHasRivals reports whether the outlook holds anybody at disliked or worse, meaning the
// employer is on bad terms with somebody. It is the cheap gate before a per-entity standing walk
// (combat_embodied.go), asked alongside relHasEnemies: that is the corporate registry, this is
// the faction matrix.
func outlookHasRivals(p *Profile) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Standings {
		if s.Value <= StandingDisliked {
			return true
		}
	}
	return false
}

// TierTo is the tier name of StandingTo.
func (p *Profile) TierTo(id int64) string { return standingTier(p.StandingTo(id)) }

// standingToPilot is how one pilot regards another, decided by their employers: the worse of the
// corporate layer (corprel.go) and the faction layer, since neither cancels the other. The same
// corporation or employer faction is never a rival (as factionHostile). It is an opinion, not a
// firing order: factionHostile, gridNPCHostileTo and hostilesOnGrid decide who shoots.
func (m *Mod) standingToPilot(ctx context.Context, self standingSelf, otherCharID int64) float64 {
	return m.standingToPilotFor(ctx, self, otherCharID, false, DoctrineNone)
}

// standingToPilotForEngage is standingToPilot for the question that opens fire: the prejudice
// term goes through engageStanding (roe.go) under the deciding corporation's doctrine, so a navy
// never reads a hated race as hostile. Friendship vetoes, escort and conscript checks keep the
// full opinion. The abstract twin of gridEngageStandingToPilot.
func (m *Mod) standingToPilotForEngage(ctx context.Context, self standingSelf, otherCharID int64, doc Doctrine) float64 {
	return m.standingToPilotFor(ctx, self, otherCharID, true, doc)
}

func (m *Mod) standingToPilotFor(ctx context.Context, self standingSelf, otherCharID int64, engage bool, doc Doctrine) float64 {
	return m.standingMotiveToPilotFor(ctx, self, otherCharID, engage, doc).Standing
}

// standingMotiveToPilot is standingToPilot with the deciding layer named, for chatter
// (chatter_motive.go): the abstract tier's twin of gridMotiveToPilot.
func (m *Mod) standingMotiveToPilot(ctx context.Context, self standingSelf, otherCharID int64) motiveRead {
	return m.standingMotiveToPilotFor(ctx, self, otherCharID, false, DoctrineNone)
}

func (m *Mod) standingMotiveToPilotFor(ctx context.Context, self standingSelf, otherCharID int64, engage bool, doc Doctrine) motiveRead {
	var r motiveRead
	if otherCharID == 0 || self.CharID == otherCharID {
		return r
	}
	// A pilot who insulted this bot in Local (chatter_retort.go): the only personal opinion of a
	// named pilot the module keeps, in memory for a couple of hours.
	if g := insultGrudge(self.CharID, otherCharID); g != 0 {
		r = foldWhy(r, g, motivePersonal)
	}
	// The grace ledger (grace_ledger.go): a grudge folds, a regard is only a starting value.
	if g := gracePersonal(self.CharID, otherCharID); g < 0 {
		r = foldWhy(r, g, motivePersonal)
	} else if g > 0 && r.Standing == 0 {
		r.Standing, r.Why = g, motivePersonal
	}

	otherCorp := m.charCorpID(ctx, otherCharID)
	if otherCorp == 0 || otherCorp == self.CorpID {
		return r
	}

	// The corporate layer (corprel.go) runs before the faction short-circuit: a player corporation
	// has no faction, so only this layer lets a director's declaration reach a bot.
	selfAlly, otherAlly := m.corpAllianceID(ctx, self.CorpID), m.corpAllianceID(ctx, otherCorp)
	corpWhy := motiveCorpRival
	if relAtWar(self.CorpID, selfAlly, otherCorp, otherAlly) {
		corpWhy = motiveWar
	}
	r = foldWhy(r, relFold(r.Standing, self.CorpID, selfAlly, otherCorp, otherAlly), corpWhy)
	// "Don't kill blues": a declared blue is applied after everything else
	// below has had its say (relBlueFloor, corprel.go).
	blue := relDeclaredBlue(self.CorpID, selfAlly, otherCorp, otherAlly)

	// The pilot's own prejudice (tolerance.go): a strong xenophobe reads a pilot of a people its
	// empire stands against as a rival. Kin still wins at the stance (stanceOf).
	if x := xenoStanding(self.RaceID, self.Xenophobia, raceOf(otherCharID)); x != 0 {
		r = foldPrejudice(r, x, engage, doc)
	}

	otherFaction := m.corpFactionID(ctx, otherCorp)
	// A side without a faction is a player corporation with no politics unless its directors declared
	// some, so absence is neutral here (factionHostile instead reads faction 0 as hostile).
	if self.FactionID == 0 || otherFaction == 0 || otherFaction == self.FactionID {
		return blueFloorWhy(r, blue)
	}
	mx, ok := m.standingsMatrix(ctx)
	if !ok {
		return blueFloorWhy(r, blue)
	}
	return blueFloorWhy(foldWhy(r, blocStanding(mx, self.FactionID, otherFaction), motiveFaction), blue)
}

// worseStanding is the rule itself, kept apart from the two lookups so it can be tested alone. Both
// tiers (standingToPilot and gridStandingToPilot) end here, so the abstract sim and the embodied
// projection form the same opinion. It takes the worse of the two because averaging would let a
// warm corporate relationship launder an enemy faction.
func worseStanding(corporate, political float64) float64 {
	if political < corporate {
		return political
	}
	return corporate
}

// standingSelf is the judging pilot's side of the standing question for the abstract tier. The
// embodied tier asks off its projection (gridStandingToPilot); both end at worseStanding.
type standingSelf struct {
	CharID    int64
	CorpID    int64
	FactionID int64
	// RaceID and Xenophobia are the pilot's own prejudice (tolerance.go), derived from its trait and
	// never stored. Both are zero for a judge built without a botRun (escortSelf): no prejudice.
	RaceID     int64
	Xenophobia float64
}

func (b *botRun) standingSelf() standingSelf {
	if b == nil {
		return standingSelf{}
	}
	return standingSelf{CharID: b.CharID, CorpID: b.CorpID, FactionID: b.FactionID,
		RaceID: raceOf(b.CharID), Xenophobia: b.Tune.Xenophobia}
}

// charCorpID is which corporation a character works for. Uncached: a bot
// changes employer on its own schedule, and this is asked about the pilot on
// the other side of a grid rather than about the roster.
func (m *Mod) charCorpID(ctx context.Context, charID int64) int64 {
	var corpID int64
	if charID == 0 {
		return 0
	}
	// A rostered pilot's corporation comes from the roster pass's query (rosterCorpOf, sim.go), at
	// most five seconds old.
	if v, ok := rosterCorpOf.Load(charID); ok {
		return v.(int64)
	}
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(corporationID, 0) FROM character_ WHERE characterID = ?`,
		charID).Scan(&corpID)
	return corpID
}

// standingIDs collects the ids of one kind whose value satisfies want.
func (p *Profile) standingIDs(kind string, want func(float64) bool) []int64 {
	var out []int64
	for _, s := range p.Standings {
		if s.Kind == kind && want(s.Value) {
			out = append(out, s.ToID)
		}
	}
	return out
}

// DespisedFactions are refused outright: their stations are never docked
// at, their agents never worked for, their goods never bought.
func (p *Profile) DespisedFactions() []int64 {
	return p.standingIDs(standingFaction, func(v float64) bool { return v <= StandingDespised })
}

// despisedFactionIDs is DespisedFactions over a bare standings list, for
// callers holding an employer outlook rather than a Profile.
func despisedFactionIDs(st []Standing) []int64 {
	return (&Profile{Standings: st}).DespisedFactions()
}

// AvoidedFactions are everyone disliked or worse: skipped whenever something else is within
// reach, used rather than sit idle.
func (p *Profile) AvoidedFactions() []int64 {
	return p.standingIDs(standingFaction, func(v float64) bool { return v <= StandingDisliked })
}

// LikedFactions are friends or better: preferred for agents and trade.
func (p *Profile) LikedFactions() []int64 {
	return p.standingIDs(standingFaction, func(v float64) bool { return v >= StandingLiked })
}

// Pilot-side accessors are not provided: ask standingToPilot, which weighs the employer's
// politics. A bare accessor over stored rows would answer half the question.

// ── station filtering ───────────────────────────────────────────────────────

// stationFilter is one SQL pass over staStations (alias `s`).
type stationFilter struct {
	Clause string
	Args   []any
	// Soft marks a pass that may be abandoned: it also skips merely
	// disliked owners, which is a preference, not a rule.
	Soft bool
}

// factionStationClause excludes stations owned by NPC corps of the given
// factions.
func factionStationClause(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "", nil
	}
	clause := ` AND s.corporationID NOT IN (
		SELECT corporationID FROM crpNPCCorporations
		WHERE factionID IN (` + placeholders(len(ids)) + `))`
	return clause, int64Args(ids)
}

// stationFilterPasses returns the station filters to try in order: avoid everyone the pilot
// dislikes, then only the despised if nothing was found. One pass is returned when the two would
// be identical, and for a mercenary pilot (HonourSoftStationPass false). A xenophobe's race rule
// (tolerance.go) joins both passes; the employer's friends are exempt.
func stationFilterPasses(p *Profile, tn Tuning) []stationFilter {
	xSoft, xHard := xenoStationFactions(raceOf(p.CharacterID), tn, p.LikedFactions())
	// A station whose owner refuses this pilot docking is out of every pass (stationaccess.go).
	doorClause, doorArgs := refusedStationClause(p.CharacterID)
	hardClause, hardArgs := factionStationClause(mergeIDs(p.DespisedFactions(), xHard))
	hard := stationFilter{Clause: hardClause + doorClause, Args: append(hardArgs, doorArgs...)}
	if !tn.HonourSoftStationPass {
		return []stationFilter{hard}
	}
	softClause, softArgs := factionStationClause(mergeIDs(p.AvoidedFactions(), xSoft, xHard))
	if softClause == hardClause {
		return []stationFilter{hard}
	}
	return []stationFilter{{Clause: softClause + doorClause, Args: append(softArgs, doorArgs...), Soft: true}, hard}
}

// ── retired pilot standings ───────────────────────────────

// migrateDropPilotStandings drops the retired per-pilot tables (rolebot_standings,
// rolebot_gang_bonds) and the chrStandings contacts mirrored from them. One-way, no backup.
// See docs/comment-notes-b18.md#migrate-drop-pilot-standings.
func migrateDropPilotStandings(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_standings'`,
	).Scan(&have); err != nil || have == 0 {
		return nil
	}
	const pilotsPerBatch = 200
	for cursor := int64(-1); ; {
		rows, err := db.QueryContext(ctx, `
			SELECT DISTINCT characterID FROM rolebot_standings
			 WHERE characterID > ?
			 ORDER BY characterID LIMIT ?`, cursor, pilotsPerBatch)
		if err != nil {
			return fmt.Errorf("migrateDropPilotStandings page: %w", err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if serr := rows.Scan(&id); serr != nil {
				rows.Close()
				return fmt.Errorf("migrateDropPilotStandings scan: %w", serr)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("migrateDropPilotStandings page: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		cursor = ids[len(ids)-1]
		if _, err := db.ExecContext(ctx, `
			DELETE c FROM chrStandings c
			 JOIN rolebot_standings s
			   ON s.characterID = c.characterID AND s.toID = c.toID
			 WHERE c.characterID IN (`+placeholders(len(ids))+`)`,
			int64Args(ids)...); err != nil {
			// chrStandings is the core's table: a schema without it has no bot contacts to clean and must
			// not fail init. The tables below are the module's own and still go.
			break
		}
	}
	for _, t := range []string{"rolebot_standings", "rolebot_gang_bonds"} {
		if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+t); err != nil {
			return fmt.Errorf("migrateDropPilotStandings drop %s: %w", t, err)
		}
	}
	// Empty the legacy CSV columns on rolebot_profiles, so a database that never ran the old
	// migration keeps no list that nothing reads.
	if _, err := db.ExecContext(ctx, `
		UPDATE rolebot_profiles SET factionsSupported = '', factionsHated = ''
		 WHERE COALESCE(factionsSupported, '') <> '' OR COALESCE(factionsHated, '') <> ''`); err != nil {
		return fmt.Errorf("migrateDropPilotStandings legacy lists: %w", err)
	}
	return nil
}

// ── panel views ─────────────────────────────────────────────────────────────

// Pilot pages have no per-faction input: standings are the employer's (outlook.go), so an edit
// to them could not be honoured.

// standingName resolves the faction a standing points at, falling back to its kind and ID.
func (m *Mod) standingName(ctx context.Context, s Standing) string {
	var name string
	if err := m.db.QueryRowContext(ctx,
		`SELECT factionName FROM chrFactions WHERE factionID = ?`, s.ToID).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("%s %d", s.Kind, s.ToID)
	}
	return name
}

// standingsNarrative describes the pilot's politics in plain words, using the tiers the sim acts
// on. It narrates the employer's outlook, the only outlook a pilot has.
func (m *Mod) standingsNarrative(ctx context.Context, p *Profile) string {
	byTier := map[string][]string{}
	for _, s := range p.Standings {
		t := standingTier(s.Value)
		if t == "neutral" {
			continue
		}
		byTier[t] = append(byTier[t],
			fmt.Sprintf("%s (%+0.1f)", m.standingName(ctx, s), s.Value))
	}
	// One representative value per tier drives the wording.
	tiers := []struct {
		name string
		at   float64
	}{{"devoted", 10}, {"liked", 5}, {"disliked", -5}, {"despised", -10}}
	var parts []string
	for _, t := range tiers {
		if len(byTier[t.name]) == 0 {
			continue
		}
		parts = append(parts, standingPhrase(t.at)+" "+strings.Join(byTier[t.name], ", "))
	}
	return strings.Join(parts, "; ")
}
