package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	evedb "gurimoon/db"
)

// Corporations: the policy a rolebot follows when it chooses an employer.
//
// rolebot_corps holds one row beside each game corporation: whether bots may
// join it, its tags and how strict the door is. Defaults are seeded once
// (ensureCorpPolicies) and an operator edit is never overwritten.
// corpEligibility holds the hard rules and corpScore ranks what passes.
// Nothing here writes character_.corporationID, so every ranking is a dry run;
// the trigger that acts on it is in corpjoin.go.
// See docs/comment-notes-b06.md#corp-eligibility.

// ── tags ────────────────────────────────────────────────────────────────────

// corpTag is one kind of outfit. Appeal turns a pilot into how much it wants
// that kind of work, on the same ~1.0-is-indifferent scale as the appetite
// dispositions it is built from.
type corpTag struct {
	Key    string
	Label  string
	Blurb  string
	Appeal func(p pilotIdentity) float64
}

// corpTagCatalog is the vocabulary of outfit kinds. Adding one is a single entry:
// seeding, the page filter, the checkbox grid and scoring all read this list.
// Each Appeal peaks for its own kind of pilot. See docs/comment-notes-b06.md#tag-catalogue.
var corpTagCatalog = []corpTag{
	{"mining", "Mining", "Pulls ore out of belts and refines it.",
		func(p pilotIdentity) float64 { return p.Tune.MiningAppetite }},
	{"industry", "Manufacturing", "Runs factory lines and sells the output.",
		func(p pilotIdentity) float64 { return p.Tune.SupplyDrive * p.Tune.IndustryAppetite }},
	{"shipbuilding", "Shipbuilding", "Hulls and their components.",
		// You build what you would rather be flying.
		func(p pilotIdentity) float64 {
			return p.Tune.SupplyDrive * p.Tune.IndustryAppetite * (0.7 + 0.45*p.violentPole())
		}},
	{"construction", "Construction", "Stations, structures, heavy assembly.",
		// Jobs measured in weeks, in one place.
		func(p pilotIdentity) float64 {
			return p.Tune.SupplyDrive * p.Tune.IndustryAppetite *
				(0.6 + 0.35*p.unhurriedPole() + 0.25*p.settledPole())
		}},
	{"chemical", "Chemical", "Reactions, boosters, the trade nobody advertises.",
		func(p pilotIdentity) float64 {
			return p.Tune.IndustryAppetite * (0.5 + 0.45*p.outlawPole() + 0.25*p.unhurriedPole())
		}},
	{"research", "Research", "Blueprints, invention, laboratories.",
		// The builder's appetite spent at a desk: ResearchAppetite (tuning.go) is the
		// same expression as the research lane's gate (research.go), so tag and work agree.
		func(p pilotIdentity) float64 {
			return p.Tune.ResearchAppetite
		}},
	{"biotech", "Biotech", "Implants, cloning, medicine.",
		// Medicine, not money: peaceful and generous rather than supplying.
		func(p pilotIdentity) float64 {
			return p.Tune.IndustryAppetite * (0.4 + 0.5*p.pacifistPole() + 0.3*p.generousPole())
		}},
	{"trade", "Trade", "Buys low, sells high, keeps a book open.",
		func(p pilotIdentity) float64 { return p.Tune.TradeAppetite }},
	{"retail", "Retail", "Shopfronts and distribution to the customer.",
		func(p pilotIdentity) float64 {
			return p.Tune.TradeAppetite * (0.65 + 0.5*p.settledPole())
		}},
	{"hauling", "Hauling", "Moves other people's cargo for a fee.",
		// The hauler's own appetite (HaulAppetite), not the trader's.
		func(p pilotIdentity) float64 { return p.Tune.HaulAppetite }},
	{"finance", "Finance", "Banking, insurance, other people's money.",
		// Multiplied by the trade appetite: greed raises that appetite, so an additive
		// greed term would grow slower than the plain trade tag.
		func(p pilotIdentity) float64 {
			return p.Tune.TradeAppetite * (0.6 + 0.55*p.greedyPole())
		}},
	{"combat", "Combat", "Guns for a living, whoever is paying.",
		func(p pilotIdentity) float64 { return p.Tune.ViolenceAppetite }},
	{"mercenary", "Mercenary", "Guns for hire — fights other corporations' wars for pay.",
		// Combat appetite scaled by the mercenary pole: 1.25x at the mercenary end, under
		// half at the devoted end. No lawfulness term; doctrineFromTags reads the other tags.
		func(p pilotIdentity) float64 {
			return p.Tune.ViolenceAppetite * (0.45 + 0.8*p.mercenaryPole())
		}},
	{"security", "Security", "Policing, escort, the lawful end of violence.",
		func(p pilotIdentity) float64 { return p.Tune.ViolenceAppetite * (0.4 + p.lawfulPole()) }},
	{"cartelindustry", "Cartel industry", "The pirates' own factories, labs and shipyards.",
		// A builder outside the law: the outlaw pole multiplies it, so a lawful pilot
		// scores zero. The 1.4 offsets the distance penalty on one-station cartel wings.
		func(p pilotIdentity) float64 {
			return 1.4 * p.Tune.SupplyDrive * p.Tune.IndustryAppetite * p.outlawPole()
		}},
	{"outlaw", "Outlaw", "Piracy, raiding, and the security status that comes with it.",
		// 1.25x lets piracy beat plain combat. Zero for a lawful pilot, whom the
		// pirate rule refuses anyway.
		func(p pilotIdentity) float64 { return 1.25 * p.Tune.ViolenceAppetite * p.outlawPole() }},
	{"media", "Media", "Broadcast, entertainment, publicity.",
		// Hungry, hurried and happy to be anywhere.
		func(p pilotIdentity) float64 {
			return 0.35 + 0.45*p.greedyPole() + 0.6*p.hurriedPole() + 0.4*p.nomadPole()
		}},
	{"food", "Food", "Agriculture and provisioning.",
		func(p pilotIdentity) float64 {
			return p.Tune.IndustryAppetite * (0.4 + 0.5*p.settledPole() + 0.4*p.unhurriedPole())
		}},
	{"civil", "Civil service", "Government, bureaucracy, the courts.",
		// Built from the lawful, unhurried and devoted poles, not as a fraction of industry.
		func(p pilotIdentity) float64 {
			return 0.4 + 0.6*p.lawfulPole() + 0.35*p.unhurriedPole() + 0.3*p.devotedPole()
		}},
	{"education", "Education", "Schools and academies — where a capsuleer starts.",
		// Unhurried, peaceful and law-abiding. Not "settled": that pole belongs to the farm.
		func(p pilotIdentity) float64 {
			return 0.45 + 0.6*p.unhurriedPole() + 0.4*p.pacifistPole() + 0.3*p.lawfulPole()
		}},
}

func corpTagByKey(k string) (corpTag, bool) {
	for _, t := range corpTagCatalog {
		if t.Key == k {
			return t, true
		}
	}
	return corpTag{}, false
}

// corpTagLabel is the display name of a tag key. An unknown key is echoed back;
// an operator-made tag scores nothing but still filters.
func corpTagLabel(k string) string {
	if t, ok := corpTagByKey(k); ok {
		return t.Label
	}
	return k
}

// ── activities ──────────────────────────────────────────────────────────────
//
// crpNPCCorporations.mainActivityID has no shipped name table, so these labels
// are read off the corporations that carry each ID. They are display only.

var corpActivityLabel = map[int64]string{
	1:  "Agriculture",
	2:  "Construction",
	3:  "Mining",
	4:  "Chemical",
	5:  "Military & police",
	6:  "Biotech",
	7:  "Applied technology",
	8:  "Entertainment",
	9:  "Shipbuilding",
	10: "Transport",
	11: "Distribution",
	12: "Trade",
	13: "Bureaucracy",
	14: "Administration",
	15: "Law",
	17: "Finance",
	18: "Education & research",
	19: "Manufacturing",
}

// corpActivityTags is the default tag set an activity implies. Activity 5 maps
// to "combat" only; defaultCorpPolicy adds "outlaw" or "security" by faction.
var corpActivityTags = map[int64][]string{
	1:  {"food", "hauling"},
	2:  {"construction", "industry"},
	3:  {"mining", "industry"},
	4:  {"chemical", "industry"},
	5:  {"combat"},
	6:  {"biotech", "research"},
	7:  {"industry", "research"},
	8:  {"media"},
	9:  {"shipbuilding", "industry"},
	10: {"hauling"},
	11: {"retail", "hauling"},
	12: {"trade"},
	13: {"civil"},
	14: {"civil"},
	15: {"civil", "security"},
	17: {"finance", "trade"},
	18: {"education", "research"},
	19: {"industry"},
}

func corpActivityName(id int64) string {
	if s, ok := corpActivityLabel[id]; ok {
		return s
	}
	if id == 0 {
		return "—"
	}
	return fmt.Sprintf("activity %d", id)
}

// ── blocs ───────────────────────────────────────────────────────────────────

const (
	blocEmpire = "empire" // an empire and its client states: the bloc rule applies
	blocFringe = "fringe" // unaligned: no race rule, standing still prices it
	blocOutlaw = "outlaw" // pirates: no race rule, character is the gate
)

// normFactionName drops the definite article this database puts on some
// faction names ("The Blood Raider Covenant") so a name-keyed set can be
// written the way people say them.
func normFactionName(s string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "The "))
}

// empireBlocFactions are the factions that recruit along political lines: the
// states, their client states, CONCORD and the Jove. Jove corporations carry
// raceID 16, so the strict race rule already keeps other races out.
var empireBlocFactions = map[string]bool{
	"Amarr Empire": true, "Caldari State": true, "Gallente Federation": true,
	"Minmatar Republic": true, "Ammatar Mandate": true, "Khanid Kingdom": true,
	"CONCORD Assembly": true, "Jove Empire": true,
}

// outlawBlocFactions are the ones that take anybody. Kept as names for the
// same reason seed.go does: no faction ID is hardcoded anywhere in the
// module.
var outlawBlocFactions = map[string]bool{
	"Angel Cartel": true, "Guristas Pirates": true, "Sansha's Nation": true,
	"Blood Raider Covenant": true, "Serpentis": true,
}

func corpBlocFor(factionName string) string {
	n := normFactionName(factionName)
	switch {
	case outlawBlocFactions[n]:
		return blocOutlaw
	case empireBlocFactions[n]:
		return blocEmpire
	default:
		return blocFringe
	}
}

// ── the policy row ──────────────────────────────────────────────────────────

// Race rules. Auto is the bloc-derived answer above; the other three are an
// operator overruling it for one corporation.
const (
	corpRaceAuto   = ""       // whatever the bloc implies
	corpRaceBloc   = "bloc"   // force the empire-standing rule
	corpRaceStrict = "strict" // this corporation's own race only
	corpRaceOpen   = "open"   // anyone, whatever their empire thinks
)

// CorpPolicy is the module's row beside one corporation.
type CorpPolicy struct {
	CorpID      int64
	Joinable    bool
	Restricted  bool
	Tags        []string
	RaceRule    string
	MinStanding float64
	Weight      float64
	Notes       string
	// Allegiance is the faction a player corporation flies for (allegiance.go), 0 unaligned.
	// Written only by setCorpAllegiance; saveCorpPolicy leaves the column alone.
	Allegiance int64
	UpdatedAt  int64
}

func (p CorpPolicy) TagCSV() string { return strings.Join(p.Tags, ",") }

func (p CorpPolicy) HasTag(k string) bool {
	for _, t := range p.Tags {
		if t == k {
			return true
		}
	}
	return false
}

// parseTagList normalises an operator-typed or seeded tag list: trimmed,
// lower-cased, de-duplicated, order preserved.
func parseTagList(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(s, ",") {
		t := strings.ToLower(strings.TrimSpace(part))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func saveCorpPolicy(ctx context.Context, db *sql.DB, p *CorpPolicy) error {
	joinable, restricted := 0, 0
	if p.Joinable {
		joinable = 1
	}
	if p.Restricted {
		restricted = 1
	}
	if p.Weight <= 0 {
		p.Weight = 1
	}
	// seeded = 0 marks the row as the operator's: every edit takes this write path,
	// and a later change of defaults leaves such a row alone.
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_corps
			(corporationID, joinable, restricted, tags, raceRule, minStanding, weight, notes, seeded, updatedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON DUPLICATE KEY UPDATE
			joinable = VALUES(joinable), restricted = VALUES(restricted),
			tags = VALUES(tags), raceRule = VALUES(raceRule),
			minStanding = VALUES(minStanding), weight = VALUES(weight),
			notes = VALUES(notes), seeded = 0, updatedAt = VALUES(updatedAt)`,
		p.CorpID, joinable, restricted, p.TagCSV(), p.RaceRule,
		p.MinStanding, p.Weight, p.Notes, clock.Unix())
	if err != nil {
		return fmt.Errorf("saveCorpPolicy: %w", err)
	}
	// Tags are the combat doctrine (corpdoctrine.go), memoised per process: drop the
	// memo so a retag takes effect without a restart.
	invalidateCorpDoctrine(p.CorpID)
	forgetCorpCatalogue()
	return nil
}

// ── what a corporation IS ───────────────────────────────────────────────────

// CorpFacts is one row of the catalogue: the game's own facts about a
// corporation, the module's policy row beside it, and the counts that make
// the page worth reading.
type CorpFacts struct {
	CorpID      int64
	Name        string
	Ticker      string
	FactionID   int64
	FactionName string
	RaceID      int64
	ActivityID  int64
	Activity2ID int64
	Size        string
	Extent      string
	StationID   int64
	StationName string
	SystemID    int64
	SystemName  string
	RegionName  string
	Stations    int
	Members     int // player pilots (accountID > 0), not NPC agents
	Bots        int // rostered rolebots currently in this corporation
	// Pending is the rolebots with an application out to it (player
	// corporations only): they are on their way in, and both the queue door
	// (playerCorpRefusal) and the crowding term count them.
	Pending int

	// Tribe is the Minmatar tribe of this corporation, empty for most.
	// TribeBloodlineID is the tribe's playable bloodline, 0 when it has none.
	// TribeRaceID is the race a tribal corporation requires. Set by resolveTribes.
	Tribe            string
	TribeBloodlineID int64
	TribeRaceID      int64

	// School is set for a chargen corporation (activity 18). A pilot starts at a
	// school and never signs with one (corpEligibility).
	School bool

	// Player is set for a corporation the game did not ship (playercorps.go). Its
	// faction, activity, stations and tribe are empty; it is scored on tags and address.
	// MemberLimit and AllowedRaceMask are the game's own doors (playerCorpRefusal).
	Player          bool
	CEOID           int64
	CEOName         string
	AllianceID      int64
	AllianceName    string
	MemberLimit     int
	TaxRate         float64
	AllowedRaceMask int64
	Offices         int // rented offices: a player corp's address besides its HQ
	// CEOEthnicRelations is the CEO's trained level of Ethnic Relations (typeID 3368),
	// the skill that lets a player corporation recruit outside its founder's race.
	// Read with the row, never stored.
	CEOEthnicRelations int
	// DeskBar is the charter fit bar a bot CEO's desk published on its last pass
	// (corpceo_hr.go deskBars); 0 for a human-run corporation or none read yet.
	// corpDeskRefusal scales it by how full the corporation is.
	DeskBar float64

	Policy CorpPolicy
}

func (c CorpFacts) Bloc() string { return corpBlocFor(c.FactionName) }

// BlocLabel is the bloc name the page shows. A player corporation gets its own
// label so a mixed list shows which half of the catalogue a row came from.
func (c CorpFacts) BlocLabel() string {
	if c.Player {
		return "player"
	}
	switch c.Bloc() {
	case blocEmpire:
		return "empire"
	case blocOutlaw:
		return "outlaw"
	}
	return "unaligned"
}

func (c CorpFacts) ActivityName() string  { return corpActivityName(c.ActivityID) }
func (c CorpFacts) Activity2Name() string { return corpActivityName(c.Activity2ID) }

// corpFactsQuery is the whole catalogue in one statement. The two counts are
// derived tables, not correlated subqueries, so character_ and staStations are
// scanned once rather than once per corporation.
const corpFactsQuery = `
	SELECT n.corporationID, c.corporationName, c.tickerName,
	       COALESCE(n.factionID, 0), COALESCE(f.factionName, ''),
	       COALESCE(c.raceID, 0), COALESCE(n.mainActivityID, 0), COALESCE(n.secondaryActivityID, 0),
	       COALESCE(n.size, ''), COALESCE(n.extent, ''),
	       c.stationID, COALESCE(sta.stationName, ''),
	       COALESCE(sta.solarSystemID, 0), COALESCE(ms.solarSystemName, ''), COALESCE(mr.regionName, ''),
	       COALESCE(st.stations, 0), COALESCE(mem.members, 0), COALESCE(rb.bots, 0),
	       COALESCE(p.joinable, 0), COALESCE(p.restricted, 0), COALESCE(p.tags, ''),
	       COALESCE(p.raceRule, ''), COALESCE(p.minStanding, 0), COALESCE(p.weight, 1),
	       COALESCE(p.notes, ''), COALESCE(p.allegiance, 0), COALESCE(p.updatedAt, 0)
	FROM crpNPCCorporations n
	JOIN corporation c ON c.corporationID = n.corporationID
	LEFT JOIN chrFactions f ON f.factionID = n.factionID
	LEFT JOIN staStations sta ON sta.stationID = c.stationID
	LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = sta.solarSystemID
	LEFT JOIN mapRegions mr ON mr.regionID = ms.regionID
	LEFT JOIN (SELECT corporationID, COUNT(*) AS stations FROM staStations GROUP BY corporationID) st
	       ON st.corporationID = n.corporationID
	LEFT JOIN (SELECT corporationID, COUNT(*) AS members FROM character_ WHERE accountID > 0 GROUP BY corporationID) mem
	       ON mem.corporationID = n.corporationID
	LEFT JOIN (SELECT ch.corporationID, COUNT(*) AS bots
	             FROM rolebot_characters r JOIN character_ ch ON ch.characterID = r.characterID
	            GROUP BY ch.corporationID) rb
	       ON rb.corporationID = n.corporationID
	LEFT JOIN rolebot_corps p ON p.corporationID = n.corporationID`

func scanCorpFacts(rows *sql.Rows) ([]CorpFacts, error) {
	var out []CorpFacts
	for rows.Next() {
		var c CorpFacts
		var joinable, restricted int64
		var tags string
		if err := rows.Scan(&c.CorpID, &c.Name, &c.Ticker, &c.FactionID, &c.FactionName,
			&c.RaceID, &c.ActivityID, &c.Activity2ID, &c.Size, &c.Extent,
			&c.StationID, &c.StationName, &c.SystemID, &c.SystemName, &c.RegionName,
			&c.Stations, &c.Members, &c.Bots,
			&joinable, &restricted, &tags, &c.Policy.RaceRule,
			&c.Policy.MinStanding, &c.Policy.Weight, &c.Policy.Notes, &c.Policy.Allegiance,
			&c.Policy.UpdatedAt); err != nil {
			return nil, err
		}
		c.Policy.CorpID = c.CorpID
		c.School = c.ActivityID == corpActivitySchool
		c.Policy.Joinable = joinable != 0
		c.Policy.Restricted = restricted != 0
		c.Policy.Tags = parseTagList(tags)
		if c.Policy.Weight <= 0 {
			c.Policy.Weight = 1
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// listCorpFacts reads the whole NPC catalogue, ordered the way the page wants it.
// Filtering is done in Go because the catalogue is small and every page filter
// (tag, bloc, eligibility) is computed rather than stored.
func listCorpFacts(ctx context.Context, db *sql.DB) ([]CorpFacts, error) {
	rows, err := db.QueryContext(ctx, corpFactsQuery+`
		ORDER BY COALESCE(f.factionName, 'zzz'), c.corporationName`)
	if err != nil {
		return nil, fmt.Errorf("listCorpFacts: %w", err)
	}
	defer rows.Close()
	out, err := scanCorpFacts(rows)
	if err != nil {
		return nil, err
	}
	return out, resolveTribes(ctx, db, out)
}

func getCorpFacts(ctx context.Context, db *sql.DB, corpID int64) (*CorpFacts, error) {
	rows, err := db.QueryContext(ctx, corpFactsQuery+`
		WHERE n.corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("getCorpFacts: %w", err)
	}
	defer rows.Close()
	list, err := scanCorpFacts(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("corporation %d is not an NPC corporation", corpID)
	}
	if err := resolveTribes(ctx, db, list); err != nil {
		return nil, err
	}
	return &list[0], nil
}

// ── tribes ──────────────────────────────────────────────────────────────────
//
// A Minmatar tribe is a people, and a tribal corporation recruits from its own.
// A corporation belongs to a tribe when its own or its faction's name ends in
// "tribe". Brutor, Sebiestor and Vherokior are playable bloodlines, so those
// corporations take only their own bloodline; Krusual and Thukker have none and
// take any Minmatar. Nothing is enumerated: a new bloodline row is picked up on
// the next page load.

// tribalRaceName is the one name this rule needs. Named rather than an ID,
// the same convention every faction and bloodline set here follows.
const tribalRaceName = "Minmatar"

// bloodline is one playable origin, with the race it belongs to so the tribe
// rule can ask both of its questions off a single row.
type bloodline struct {
	ID     int64
	Name   string
	RaceID int64
}

// tribeSuffix reads "Brutor tribe" as "Brutor" and everything else as "".
// Case-insensitive because the corporations are shipped lower-case ("Brutor
// tribe") and the faction upper ("Thukker Tribe").
func tribeSuffix(name string) string {
	f := strings.Fields(strings.TrimSpace(name))
	if len(f) < 2 || !strings.EqualFold(f[len(f)-1], "tribe") {
		return ""
	}
	return strings.Join(f[:len(f)-1], " ")
}

// tribeOf is the corporation's own name first, then its faction's: Thukker Mix
// carries no tribe name and flies the Thukker Tribe's flag.
func tribeOf(corpName, factionName string) string {
	if t := tribeSuffix(corpName); t != "" {
		return t
	}
	return tribeSuffix(normFactionName(factionName))
}

// resolveTribes fills the tribe fields of a catalogue in place. It runs two small
// queries for the whole list and skips both when no corporation names a tribe.
func resolveTribes(ctx context.Context, db *sql.DB, corps []CorpFacts) error {
	names := make([]string, len(corps))
	any := false
	for i := range corps {
		names[i] = tribeOf(corps[i].Name, corps[i].FactionName)
		if names[i] != "" {
			any = true
		}
	}
	if !any {
		return nil
	}

	blood := map[string]bloodline{}
	rows, err := db.QueryContext(ctx,
		`SELECT bloodlineID, bloodlineName, raceID FROM chrBloodlines`)
	if err != nil {
		return fmt.Errorf("resolveTribes bloodlines: %w", err)
	}
	for rows.Next() {
		var b bloodline
		if err := rows.Scan(&b.ID, &b.Name, &b.RaceID); err != nil {
			rows.Close()
			return fmt.Errorf("resolveTribes bloodlines: %w", err)
		}
		blood[strings.ToLower(b.Name)] = b
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("resolveTribes bloodlines: %w", err)
	}

	// The race a tribe belongs to when it has no bloodline of its own.
	var tribalRaceID int64
	if err := db.QueryRowContext(ctx,
		`SELECT raceID FROM chrRaces WHERE raceName = ?`, tribalRaceName).
		Scan(&tribalRaceID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resolveTribes race: %w", err)
	}

	for i := range corps {
		if names[i] == "" {
			continue
		}
		corps[i].Tribe = names[i]
		corps[i].TribeRaceID = tribalRaceID
		if b, ok := blood[strings.ToLower(names[i])]; ok {
			corps[i].TribeBloodlineID = b.ID
			// A bloodline knows its own race, so a tribe that has one does
			// not need the name lookup to have worked.
			corps[i].TribeRaceID = b.RaceID
		}
	}
	return nil
}

// A name for corporation.raceID, so a refusal reads as a sentence. Includes Jove
// and Pirate, which nobody is born into. The Ammatar and Thukker corporations
// carry raceID 2, so they are Minmatar here with no special case.
var raceNames = map[int64]string{
	1: "Caldari", 2: "Minmatar", 4: "Amarr", 8: "Gallente", 16: "Jove", 32: "Pirate",
}

// raceName is how a refusal names a race. An unknown ID is echoed back so the
// refusal stays readable.
func raceName(id int64) string {
	if n, ok := raceNames[id]; ok {
		return n
	}
	if id == 0 {
		return "no race"
	}
	return fmt.Sprintf("race %d", id)
}

// ── defaults ────────────────────────────────────────────────────────────────

// defaultCorpPolicy is the policy a corporation gets before an operator edits it.
// Everything seeds open except what closedByDefault lists. The hard rules in
// corpEligibility apply either way.
func defaultCorpPolicy(c CorpFacts) CorpPolicy {
	p := CorpPolicy{CorpID: c.CorpID, Weight: 1}

	tags := append([]string{}, corpActivityTags[c.ActivityID]...)
	// The second activity contributes tags too: the mining arm of a manufacturer,
	// the trade wing of a shipyard.
	tags = append(tags, corpActivityTags[c.Activity2ID]...)

	// Violence, sorted by who is paying for it: the same activity ID is
	// police work in an empire and piracy in the Cartel.
	if c.ActivityID == 5 || c.Activity2ID == 5 {
		switch c.Bloc() {
		case blocOutlaw:
			tags = append(tags, "outlaw")
		default:
			tags = append(tags, "security")
		}
	}
	if c.Bloc() == blocOutlaw {
		tags = append(tags, "outlaw")
	}
	// Cartel industrial wings: several file under trade or biotech and are found
	// only through the second activity (see cartelIndustryActivity).
	if c.Bloc() == blocOutlaw && (cartelIndustryActivity[c.ActivityID] || cartelIndustryActivity[c.Activity2ID]) {
		tags = append(tags, "cartelindustry", "industry")
		p.Weight = cartelIndustryWeight(c.Stations)
	}

	p.Tags = parseTagList(strings.Join(tags, ","))

	p.Restricted = closedByDefault(c)
	p.Joinable = !p.Restricted
	return p
}

// cartelIndustryActivity marks the crpNPCCorporations activities that make a
// cartel corporation a place where things are made. Hauling, distribution and
// mining are left out.
var cartelIndustryActivity = map[int64]bool{
	4:  true, // Chemical: Serpentis Corporation, Salvation Angels' second
	6:  true, // Biotech: Serpentis Inquest
	7:  true, // Applied technology: Guristas Production
	9:  true, // Shipbuilding: True Creations, Guristas Production's second
	19: true, // Manufacturing
}

// cartelIndustryWeight lifts a cartel wing that owns few stations: home distance
// lowers the score, so a one-station employer is near nobody. Applied at seeding only.
// See docs/comment-notes-b06.md#cartel-wings.
func cartelIndustryWeight(stations int) float64 {
	switch {
	case stations <= 0:
		return 1 // nowhere to walk in at all; corpHomeMul already says so
	case stations <= 2:
		return 2.0
	case stations <= 8:
		return 1.5
	}
	return 1
}

// closedByDefault lists the corporations a fresh roster is not turned loose on.
// It is a starting position: the race, tribe and pirate rules in corpEligibility
// apply whatever is ticked.
func closedByDefault(c CorpFacts) bool {
	// A school is where a pilot starts. It is refused outright as a
	// destination (corpEligibility) and seeds shut so the page says so.
	if c.School {
		return true
	}

	switch c.ActivityID {
	case 13, 14, 15:
		// Government, bureaucracy and the courts: the ruling families, the presidents,
		// the senates and the tribunals.
		return true
	case 8:
		// The newsrooms are the state's own voice, like a ministry.
		return true
	}
	// Activity 5 (the armed services) and the Jove stay open. See
	// docs/comment-notes-b06.md#closed-by-default.

	// A tribe is a people, decided by birth (the tribe rule in corpEligibility), so
	// tribes start shut and an operator opens them.
	if c.Tribe != "" {
		return true
	}

	if normFactionName(c.FactionName) == "CONCORD Assembly" {
		return true
	}
	return false
}

// ensureCorpPolicies seeds a policy row for every NPC corporation that has
// none. Idempotent and non-destructive: INSERT IGNORE, so an operator's
// edits are never re-imposed on, and a corporation added to the database
// later picks its defaults up on the next boot.
func (m *Mod) ensureCorpPolicies(ctx context.Context) error {
	corps, err := listCorpFacts(ctx, m.db)
	if err != nil {
		return err
	}
	// Which corporations already carry a row, in one query rather than a
	// probe per corporation at every boot.
	have := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `SELECT corporationID FROM rolebot_corps`)
	if err != nil {
		return fmt.Errorf("ensureCorpPolicies existing: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("ensureCorpPolicies existing: %w", err)
		}
		have[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ensureCorpPolicies existing: %w", err)
	}

	seeded, open := 0, 0
	for _, c := range corps {
		if have[c.CorpID] {
			continue
		}
		p := defaultCorpPolicy(c)
		if _, err := m.db.ExecContext(ctx, `
			INSERT IGNORE INTO rolebot_corps
				(corporationID, joinable, restricted, tags, raceRule, minStanding, weight, notes, seeded, updatedAt)
			VALUES (?, ?, ?, ?, '', 0, 1, '', 1, ?)`,
			p.CorpID, boolInt(p.Joinable), boolInt(p.Restricted), p.TagCSV(), clock.Unix()); err != nil {
			return fmt.Errorf("ensureCorpPolicies seed %d: %w", c.CorpID, err)
		}
		seeded++
		if p.Joinable {
			open++
		}
	}
	if seeded > 0 {
		rlogf("rolebots: seeded corporation policy for %d NPC corporation(s) — %d open to bots, %d shut (schools, the ministries and courts, the newsrooms, the tribes and CONCORD)",
			seeded, open, seeded-open)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// applyCorpDefaultGeneration re-seeds default rows once per corpDefaultsGeneration.
// Rows still marked seeded = 1 are recomputed; a row an operator saved is never touched.
// See docs/comment-notes-b06.md#default-generation.
func (m *Mod) applyCorpDefaultGeneration(ctx context.Context) error {
	have, _, err := evedb.GetServerSetting(ctx, m.db, settingCorpDefaults)
	if err != nil {
		return fmt.Errorf("applyCorpDefaultGeneration: %w", err)
	}
	if have == corpDefaultsGeneration {
		return nil
	}

	// Generation 1 seeded every row shut and had no seeded column, so an open row was
	// opened by hand: mark it the operator's before re-seeding.
	if have == "" {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_corps SET seeded = 0 WHERE joinable = 1`); err != nil {
			return fmt.Errorf("applyCorpDefaultGeneration adopt: %w", err)
		}
	}

	corps, err := listCorpFacts(ctx, m.db)
	if err != nil {
		return err
	}
	changed := 0
	for _, c := range corps {
		p := defaultCorpPolicy(c)
		// Tags and weight are re-seeded with the two switches, gated on seeded = 1:
		// an operator-saved row keeps its tags and weight.
		res, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corps SET joinable = ?, restricted = ?, tags = ?, weight = ?
			WHERE corporationID = ? AND seeded = 1
			  AND (joinable <> ? OR restricted <> ? OR tags <> ? OR weight <> ?)`,
			boolInt(p.Joinable), boolInt(p.Restricted), p.TagCSV(), p.Weight, c.CorpID,
			boolInt(p.Joinable), boolInt(p.Restricted), p.TagCSV(), p.Weight)
		if err != nil {
			return fmt.Errorf("applyCorpDefaultGeneration %d: %w", c.CorpID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed++
		}
	}
	// Re-seeding rewrites tags, so every memoised corporation doctrine may be stale.
	invalidateCorpDoctrine(0)
	if err := evedb.SetServerSetting(ctx, m.db, settingCorpDefaults, corpDefaultsGeneration); err != nil {
		return fmt.Errorf("applyCorpDefaultGeneration marker: %w", err)
	}
	if changed > 0 {
		rlogf("rolebots: corporation defaults generation %s — %d untouched policy row(s) re-seeded; anything you had edited was left alone",
			corpDefaultsGeneration, changed)
	}
	return nil
}

// ── the pilot side ──────────────────────────────────────────────────────────

// pilotIdentity is everything the corporation arithmetic reads about one
// pilot. Assembled in bulk (loadPilotIdentities) so ranking a roster is
// three queries rather than three per bot.
type pilotIdentity struct {
	CharID  int64
	Name    string
	Enabled bool
	RaceID  int64
	// BloodlineID is the pilot's own people, finer than its race: the only thing a
	// tribal corporation asks about (three bloodlines share the Minmatar race).
	BloodlineID   int64
	BloodlineName string
	EmpireID      int64 // the pilot's own empire faction (0 for a race with none)
	EmpireName    string
	CorpID        int64
	CorpName      string
	HomeSystemID  int64
	// HomeCorpID owns the station this pilot lives in: its home station, else the
	// station it is docked at. For a new pilot that is its school's station.
	HomeCorpID int64
	// HomeStationID is the station this pilot lives in, by the same rule. A player
	// corporation headquartered there is home too (scoreCorpForPilot).
	HomeStationID int64
	// Profile is partial: only the pilot's standings, which is all the corporation
	// rules read. It is built in bulk rather than per pilot to keep the page cheap.
	Profile *Profile
	Traits  map[string]int
	Tune    Tuning
	// Favour is the pilot's regard for its own bot CEO, 0..1 (grace_ledger.go): the loyalist bloc.
	Favour float64
}

func (p pilotIdentity) trait(key string) float64 { return traitScale(p.Traits[key]) }

// outlawPole and lawfulPole are the two halves of the lawfulness axis, each 0
// at the middle and 1 at its own end. High lawfulness is the outlaw end (traitCatalog).
func (p pilotIdentity) outlawPole() float64 { return posPole(p.trait(TraitLawfulness)) }
func (p pilotIdentity) lawfulPole() float64 { return posPole(-p.trait(TraitLawfulness)) }

// The other poles the tag catalogue reads, named with traitCatalog's word for each
// end of an axis. Most are the low end of their trait, so a call site reads
// p.unhurriedPole() rather than a negated posPole.
func (p pilotIdentity) pacifistPole() float64 { return posPole(-p.trait(TraitAggression)) }
func (p pilotIdentity) unhurriedPole() float64 {
	return posPole(-p.trait(TraitPatience))
}
func (p pilotIdentity) settledPole() float64   { return posPole(-p.trait(TraitWanderlust)) }
func (p pilotIdentity) devotedPole() float64   { return posPole(-p.trait(TraitLoyalty)) }
func (p pilotIdentity) mercenaryPole() float64 { return posPole(p.trait(TraitLoyalty)) }
func (p pilotIdentity) generousPole() float64  { return posPole(-p.trait(TraitGreed)) }
func (p pilotIdentity) greedyPole() float64    { return posPole(p.trait(TraitGreed)) }
func (p pilotIdentity) hurriedPole() float64   { return posPole(p.trait(TraitPatience)) }
func (p pilotIdentity) nomadPole() float64     { return posPole(p.trait(TraitWanderlust)) }
func (p pilotIdentity) violentPole() float64   { return posPole(p.trait(TraitAggression)) }

// posPole clamps a −1..+1 trait to its positive half.
func posPole(t float64) float64 {
	if t < 0 {
		return 0
	}
	if t > 1 {
		return 1
	}
	return t
}

// loadPilotIdentities reads the roster (or one pilot of it) ready to score.
// onlyEnabled leaves out bots the operator has parked, which is what the
// panel wants by default: a disabled pilot is not looking for work.
func (m *Mod) loadPilotIdentities(ctx context.Context, charID int64, onlyEnabled bool) ([]pilotIdentity, error) {
	where := ""
	var args []any
	if charID != 0 {
		where = " AND r.characterID = ?"
		args = append(args, charID)
	}
	if onlyEnabled {
		where += " AND r.enabled = 1"
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.characterID, c.characterName, r.enabled,
		       COALESCE(bl.raceID, 0), COALESCE(c.bloodlineID, 0), COALESCE(bl.bloodlineName, ''),
		       c.corporationID, COALESCE(corp.corporationName, ''),
		       c.solarSystemID,
		       COALESCE(hs.solarSystemID, 0), COALESCE(hs.corporationID, 0),
		       COALESCE(ds.corporationID, 0), COALESCE(hs.stationID, 0), COALESCE(c.stationID, 0)
		FROM rolebot_characters r
		JOIN character_ c ON c.characterID = r.characterID
		LEFT JOIN chrBloodlines bl ON bl.bloodlineID = c.bloodlineID
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		LEFT JOIN staStations hs ON hs.stationID = c.homeStationID
		LEFT JOIN staStations ds ON ds.stationID = c.stationID
		WHERE 1 = 1`+where+`
		ORDER BY c.characterName`, args...)
	if err != nil {
		return nil, fmt.Errorf("loadPilotIdentities: %w", err)
	}
	defer rows.Close()

	var out []pilotIdentity
	for rows.Next() {
		var p pilotIdentity
		var enabled int64
		var currentSystemID, homeSystemID, homeStationCorpID, dockedCorpID, homeStationID, dockedStationID int64
		if err := rows.Scan(&p.CharID, &p.Name, &enabled, &p.RaceID,
			&p.BloodlineID, &p.BloodlineName, &p.CorpID, &p.CorpName,
			&currentSystemID, &homeSystemID, &homeStationCorpID, &dockedCorpID,
			&homeStationID, &dockedStationID); err != nil {
			return nil, err
		}
		p.Enabled = enabled != 0
		p.Profile = &Profile{CharacterID: p.CharID}
		// Home is the home station when set, otherwise where the pilot stands, which
		// for a new pilot is its school's station.
		p.HomeSystemID = homeSystemID
		if p.HomeSystemID == 0 {
			p.HomeSystemID = currentSystemID
		}
		p.HomeCorpID = homeStationCorpID
		if p.HomeCorpID == 0 {
			p.HomeCorpID = dockedCorpID
		}
		p.HomeStationID = homeStationID
		if p.HomeStationID == 0 {
			p.HomeStationID = dockedStationID
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Traits and standings in one pass each, then resolved per pilot.
	traits, err := m.loadRosterTraits(ctx)
	if err != nil {
		return nil, err
	}
	standings, err := m.loadRosterStandings(ctx)
	if err != nil {
		return nil, err
	}
	empires, err := m.empireFactionIDs(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Traits = traits[out[i].CharID]
		if out[i].Traits == nil {
			out[i].Traits = map[string]int{}
		}
		out[i].Tune = resolveTuning(out[i].Traits)
		out[i].Profile.Standings = standings[out[i].CharID]
		if e, ok := empires[out[i].RaceID]; ok {
			out[i].EmpireID, out[i].EmpireName = e.ID, e.Name
		}
	}
	return out, nil
}

// loadRosterStandings returns each rostered pilot's faction opinions, which come
// from its employer's outlook (outlook.go). Only faction rows: a corporation is
// judged by the flag it flies. One query for employers, one memo read each.
func (m *Mod) loadRosterStandings(ctx context.Context) (map[int64][]Standing, error) {
	ids, err := int64Column(ctx, m, `SELECT characterID FROM rolebot_characters`)
	if err != nil {
		return nil, fmt.Errorf("loadRosterStandings: %w", err)
	}
	return m.outlooksFor(ctx, ids)
}

// loadRosterTraits reads every rostered pilot's personality in one query.
func (m *Mod) loadRosterTraits(ctx context.Context) (map[int64]map[string]int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.characterID, t.trait, t.value
		FROM rolebot_traits t
		JOIN rolebot_characters r ON r.characterID = t.characterID`)
	if err != nil {
		return nil, fmt.Errorf("loadRosterTraits: %w", err)
	}
	defer rows.Close()
	out := map[int64]map[string]int{}
	for rows.Next() {
		var charID int64
		var trait string
		var value int
		if err := rows.Scan(&charID, &trait, &value); err != nil {
			return nil, err
		}
		if out[charID] == nil {
			out[charID] = map[string]int{}
		}
		out[charID][trait] = value
	}
	return out, rows.Err()
}

// empireFactionIDs resolves raceID to its empire faction by name (seed.go's
// raceEmpireFaction is the same map the identity seeder uses).
func (m *Mod) empireFactionIDs(ctx context.Context) (map[int64]namedID, error) {
	out := map[int64]namedID{}
	for raceID, name := range raceEmpireFaction {
		var id int64
		if err := m.db.QueryRowContext(ctx,
			`SELECT factionID FROM chrFactions WHERE factionName = ?`, name).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("empireFactionIDs: %w", err)
		}
		out[raceID] = namedID{ID: id, Name: name}
	}
	return out, nil
}

// factionStandings loads the static faction-to-faction matrix, the table seed.go
// copies a pilot's starting outlook from. It is small enough to load whole.
func factionStandings(ctx context.Context, db *sql.DB) (map[[2]int64]float64, error) {
	rows, err := db.QueryContext(ctx, `SELECT fromID, toID, standing FROM npcStandings`)
	if err != nil {
		return nil, fmt.Errorf("factionStandings: %w", err)
	}
	defer rows.Close()
	out := map[[2]int64]float64{}
	for rows.Next() {
		var from, to int64
		var v float64
		if err := rows.Scan(&from, &to, &v); err != nil {
			return nil, err
		}
		out[[2]int64{from, to}] = v
	}
	return out, rows.Err()
}

// blocStanding is what faction `from` thinks of faction `to` statically.
// A faction's opinion of itself is +10 whether or not a row says so.
func blocStanding(mx map[[2]int64]float64, from, to int64) float64 {
	if from == 0 || to == 0 {
		return 0
	}
	if from == to {
		return 10
	}
	return mx[[2]int64{from, to}]
}

// ── the hard rules ──────────────────────────────────────────────────────────

// corpVerdict answers "may this pilot be in this corporation" and, on a no, the
// sentence the page shows.
type corpVerdict struct {
	OK   bool
	Why  string
	Rule string // which gate answered: switch | bloc | character | standing
}

// corpEligibility applies the hard rules, in the order an operator would
// read them. Nothing here is a preference: every no is absolute until
// somebody changes a setting or the pilot changes its mind about a faction.
func corpEligibility(p pilotIdentity, c CorpFacts, mx map[[2]int64]float64) corpVerdict {
	if !c.Policy.Joinable {
		return corpVerdict{Rule: "switch",
			Why: "the operator has not opened this corporation to bots"}
	}

	// A school is somewhere a pilot starts: it is refused whatever the settings say,
	// so a graduate cannot move from one academy to another.
	if c.School {
		return corpVerdict{Rule: "school",
			Why: "a school is somewhere a pilot leaves, never somewhere it signs"}
	}

	// A player corporation's own doors are checked here: they are absolute and the
	// corporation's. The faction, activity and tribe rules below do not apply to it.
	if c.Player {
		if v := playerCorpRefusal(p, c); v != nil {
			return *v
		}
		// The desk's own doors, so a pilot does not apply where the CEO would refuse it.
		if v := corpDeskRefusal(p, c); v != nil {
			return *v
		}
	}

	own := 0.0
	if c.FactionID != 0 && p.Profile != nil {
		own = p.Profile.StandingTo(c.FactionID)
	}

	// The tribe rule runs before the race rule so the refusal names the tribe. It tests
	// the stored race rule: `auto` resolves to open outside an empire bloc.
	if c.Tribe != "" && c.Policy.RaceRule != corpRaceOpen {
		if c.TribeRaceID != 0 && p.RaceID != c.TribeRaceID {
			return corpVerdict{Rule: "tribe", Why: fmt.Sprintf(
				"the %s are a %s tribe and this pilot is not %s",
				c.Tribe, tribalRaceName, tribalRaceName)}
		}
		if c.TribeBloodlineID != 0 && p.BloodlineID != c.TribeBloodlineID {
			who := p.BloodlineName
			if who == "" {
				who = "of another tribe"
			} else {
				who = who + ", not " + c.Tribe
			}
			return corpVerdict{Rule: "tribe", Why: fmt.Sprintf(
				"the %s recruit their own tribespeople and this pilot is %s", c.Tribe, who)}
		}
	}

	rule := c.Policy.RaceRule
	if rule == corpRaceAuto {
		switch c.Bloc() {
		case blocEmpire:
			// A state's corporation employs that state's people, with no exception for client
			// states: the Ammatar corporations carry raceID 2 (see raceNames).
			rule = corpRaceStrict
		default:
			rule = corpRaceOpen
		}
	}

	switch rule {
	case corpRaceStrict:
		if c.RaceID != 0 && p.RaceID != c.RaceID {
			return corpVerdict{Rule: "race", Why: fmt.Sprintf(
				"%s is a %s corporation and this pilot is %s",
				c.Name, raceName(c.RaceID), raceName(p.RaceID))}
		}
	case corpRaceBloc:
		// Both empires must accept a foreigner: either standing below zero refuses.
		if p.EmpireID == 0 {
			return corpVerdict{Rule: "bloc",
				Why: "this pilot's race has no empire, so an empire corporation has nothing to judge it by"}
		}
		if c.FactionID != 0 && p.EmpireID != c.FactionID {
			theirs := blocStanding(mx, c.FactionID, p.EmpireID)
			ours := blocStanding(mx, p.EmpireID, c.FactionID)
			if theirs < 0 || ours < 0 {
				return corpVerdict{Rule: "bloc", Why: fmt.Sprintf(
					"%s and %s stand %+.1f / %+.1f toward each other — this bloc does not take each other's people",
					c.FactionName, p.EmpireName, theirs, ours)}
			}
		}
	}

	// A xenophobe's own door (tolerance.go): by stage, the pilot refuses badges outside
	// its tolerated people. This is the pilot's rule, beside the corporation's above.
	corpBloodline := c.TribeBloodlineID
	if c.Player {
		corpBloodline = bloodlineOf(c.CEOID)
	}
	if why := xenoCorpRefusal(p.RaceID, p.BloodlineID, p.Tune, c.RaceID, corpBloodline); why != "" {
		return corpVerdict{Rule: "race", Why: c.Name + " is " + why}
	}

	// Pirates check character instead of passport: a pilot at the lawful
	// end of its own axis never signs with them, whatever the money.
	if c.Bloc() == blocOutlaw && p.lawfulPole() > 0.25 {
		return corpVerdict{Rule: "character", Why: fmt.Sprintf(
			"this pilot is lawful (%+d on the lawfulness axis) and will not ride with pirates",
			p.Traits[TraitLawfulness])}
	}

	// Hatred is a wall, except toward outlaw corporations: a defector walks toward the
	// pirates its flag hates (corpDefectionMul, docs/comment-notes-b06.md#defection).
	if c.FactionID != 0 && own <= StandingDespised && c.Bloc() != blocOutlaw {
		return corpVerdict{Rule: "standing", Why: fmt.Sprintf(
			"this pilot despises %s (%+.1f)", c.FactionName, own)}
	}
	if c.Policy.MinStanding != 0 && own < c.Policy.MinStanding {
		return corpVerdict{Rule: "standing", Why: fmt.Sprintf(
			"this corporation asks %+.1f toward %s and the pilot holds %+.1f",
			c.Policy.MinStanding, c.FactionName, own)}
	}

	return corpVerdict{OK: true}
}

// ── the score ───────────────────────────────────────────────────────────────

// corpScore is one (pilot, corporation) pairing priced, with the terms kept
// separate so the page can show why.
type corpScore struct {
	Fit      float64 // tags against appetites
	Standing float64 // the pilot's own opinion of the faction
	Home     float64 // how near the corporation is to where the pilot lives
	Crowd    float64 // how many rolebots are already wearing this badge
	Weight   float64 // the operator's thumb
	Race     float64 // the pilot's tolerance against the corporation's people
	RaceWhy  string
	TZ       float64 // how much of the pilot's hours the bot CEO plays (timezone.go)
	Score    float64
	Jumps    int  // to the nearest station of this corporation (−1 unreachable)
	Bots     int  // rostered rolebots already in this corporation
	SameHome bool // the corporation owns the station the pilot lives in
	Defector bool // outlaw outfit its current flag holds a grudge against
	Why      string
	Verdict  corpVerdict
}

// corpDefectionGain is what a maximal grudge is worth to a maximal outlaw, as a
// multiple on top of neutral: enough to beat a better-fitting outfit, not to drown
// the tag fit. A pilot halfway down the axis gets half.
const corpDefectionGain = 1.4

// corpDefectionMul is corpStandingMul's mirror for the outlaw bloc: a standing of 0
// scores 1.0 and a negative standing pays, scaled by the pilot's own lawlessness.
// Positive standings stay flat.
func corpDefectionMul(v, outlaw float64) float64 {
	if v >= 0 {
		return 1
	}
	if v < -10 {
		v = -10
	}
	return 1 + corpDefectionGain*outlaw*(-v/10)
}

// corpStandingMul turns the pilot's ±10 opinion into a multiplier, for
// everything that is not an outlaw corporation. Despised never reaches here
// (it is a hard rule for these blocs), so the bottom of this range is a
// pilot that merely dislikes the flag it would be flying.
func corpStandingMul(v float64) float64 {
	switch standingTier(v) {
	case "devoted":
		return 1.6
	case "liked":
		return 1.3
	case "disliked":
		return 0.5
	}
	return 1.0
}

// corpTagFit is how much this pilot wants this kind of outfit: the mean appeal of
// the corporation's tags with the best tag counted twice, so a precise tag list is
// not punished and one incidental tag cannot carry an outfit.
func corpTagFit(p pilotIdentity, tags []string) float64 {
	if len(tags) == 0 {
		return 0.5 // untagged: plausible, uninteresting
	}
	sum, best := 0.0, 0.0
	for _, k := range tags {
		v := 0.0
		if t, ok := corpTagByKey(k); ok {
			v = t.Appeal(p)
		}
		sum += v
		if v > best {
			best = v
		}
	}
	return 0.5*(sum/float64(len(tags))) + 0.5*best
}

// corpHomeMul prices distance from where the pilot lives. Being in the very
// station is the strongest single pull there is, and everything past that
// decays with jumps rather than cutting off: a corporation is a place you
// go to, and going four jumps for the right one is nothing.
func corpHomeMul(jumps int, sameStation bool) float64 {
	if sameStation {
		return 1.75
	}
	switch {
	case jumps < 0:
		return 0.15 // no route, or nowhere to walk in at all
	case jumps == 0:
		return 1.35
	}
	return 0.25 + 1.1/(1+float64(jumps)/6)
}

// ── crowding ────────────────────────────────────────────────────────────────
//
// Crowding is the one term that changes as bots arrive; the other terms belong to
// the pairing. It is measured against the average over the open catalogue, falls
// with the square of the ratio and never reaches zero.

const (
	// How hard being at exactly a fair share costs: mul = 1/(1+bite·r²),
	// so r = 1 scores 1/(1+corpCrowdBite).
	corpCrowdBite = 0.5
	// Nothing crowds below this multiplier, whatever the pile-up.
	corpCrowdFloor = 0.05
)

// corpCrowdBaseline is the average rolebot headcount per open corporation: one
// corporation's fair share of the roster. Shut corporations are left out of both
// halves, since they cannot be joined and would dilute the average.
func corpCrowdBaseline(corps []CorpFacts) float64 {
	bots, open := 0, 0
	for _, c := range corps {
		if !c.Policy.Joinable {
			continue
		}
		open++
		bots += c.Bots
	}
	if open == 0 {
		return 1
	}
	b := float64(bots) / float64(open)
	if b < 1 {
		b = 1
	}
	return b
}

// corpFillFree is the share of its member limit a player corporation fills before
// crowding bites (corpFillMul).
const corpFillFree = 0.8

// corpFillMul prices a player corporation's crowding against its own member limit:
// nothing up to corpFillFree of the limit, then falling to corpCrowdFloor at the
// limit. Applicants count as a third each, since every pilot applies to up to
// corpApplyMax doors. A big corporation with room is not crowded, however many
// bots it holds. Pure.
func corpFillMul(members, pending, limit int) float64 {
	if limit <= 0 {
		return 1
	}
	fill := (float64(max(members, 0)) + float64(max(pending, 0))/corpApplyMax) / float64(limit)
	switch {
	case fill <= corpFillFree:
		return 1
	case fill >= 1:
		return corpCrowdFloor
	}
	return 1 - (1-corpCrowdFloor)*(fill-corpFillFree)/(1-corpFillFree)
}

// corpCrowdMul prices how full a corporation already is, relative to that
// fair share. NPC corporations only; a player corporation has a member limit
// (corpFillMul).
func corpCrowdMul(bots int, baseline float64) float64 {
	if baseline < 1 {
		baseline = 1
	}
	if bots <= 0 {
		return 1
	}
	r := float64(bots) / baseline
	v := 1 / (1 + corpCrowdBite*r*r)
	if v < corpCrowdFloor {
		v = corpCrowdFloor
	}
	return v
}

// scoreCorpForPilot prices one pairing. jumps is the hop distance from the pilot's
// home to the nearest station of the corporation, -1 when unreachable. baseline is
// the crowding yardstick (corpCrowdBaseline).
func scoreCorpForPilot(p pilotIdentity, c CorpFacts, mx map[[2]int64]float64, jumps int, baseline float64) corpScore {
	s := corpScore{Weight: c.Policy.Weight, Jumps: jumps, Bots: c.Bots}
	if s.Weight <= 0 {
		s.Weight = 1
	}
	s.Verdict = corpEligibility(p, c, mx)

	s.Fit = corpTagFit(p, c.Policy.Tags)
	own := 0.0
	if c.FactionID != 0 && p.Profile != nil {
		own = p.Profile.StandingTo(c.FactionID)
	}
	// For pirates a bad standing is the pitch, for everyone else an objection
	// (corpDefectionMul, and the standing rule in corpEligibility).
	s.Defector = c.Bloc() == blocOutlaw && own < 0 && p.outlawPole() > 0
	if c.Bloc() == blocOutlaw {
		s.Standing = corpDefectionMul(own, p.outlawPole())
	} else {
		s.Standing = corpStandingMul(own)
	}

	s.SameHome = p.HomeCorpID != 0 && p.HomeCorpID == c.CorpID ||
		c.Player && c.StationID != 0 && p.HomeStationID == c.StationID
	s.Home = corpHomeMul(s.Jumps, s.SameHome)

	// A pilot does not crowd itself out of its own employer: its badge comes off the
	// count. A player corporation is crowded by its own member limit, queued
	// applicants included; an NPC one by the catalogue's fair share.
	others := s.Bots + c.Pending
	if p.CorpID == c.CorpID && others > 0 {
		others--
	}
	if c.Player && c.MemberLimit > 0 {
		members := c.Members
		if p.CorpID == c.CorpID {
			members--
		}
		s.Crowd = corpFillMul(members, c.Pending, c.MemberLimit)
	} else {
		s.Crowd = corpCrowdMul(others, baseline)
	}
	s.Race, s.RaceWhy = xenoCorpMul(p.RaceID, p.Tune, c.RaceID)
	s.TZ = tzCorpMul(p.CharID, c.CorpID)

	s.Score = s.Fit * s.Standing * s.Home * s.Crowd * s.Weight * s.Race * s.TZ
	if !s.Verdict.OK {
		s.Score = 0
	}

	switch {
	case !s.Verdict.OK:
		s.Why = s.Verdict.Why
	case s.SameHome && c.Player:
		s.Why = fmt.Sprintf("%s work, headquartered in the station this pilot lives in",
			corpBestTagLabel(p, c.Policy.Tags))
	case s.SameHome:
		s.Why = fmt.Sprintf("%s work, and it runs the station this pilot lives in",
			corpBestTagLabel(p, c.Policy.Tags))
	case s.Jumps == 0:
		s.Why = fmt.Sprintf("%s work, in this pilot's own system",
			corpBestTagLabel(p, c.Policy.Tags))
	case s.Jumps > 0:
		s.Why = fmt.Sprintf("%s work, %d jump(s) from home",
			corpBestTagLabel(p, c.Policy.Tags), s.Jumps)
	default:
		s.Why = fmt.Sprintf("%s work, but nowhere this pilot can reach",
			corpBestTagLabel(p, c.Policy.Tags))
	}
	// A defection is named on the page, since a multiplier above 1 would otherwise
	// look like a contradiction.
	if s.Verdict.OK && s.Defector {
		s.Why += fmt.Sprintf(" — and %s are who its own flag hates (%+.1f, x%.2f to it)",
			c.FactionName, own, s.Standing)
	}
	// Crowding is named only when it bites; the corporation's facts do not show it.
	if s.Verdict.OK && s.Crowd < 0.8 {
		if c.Player && c.MemberLimit > 0 {
			s.Why += fmt.Sprintf(" — but it is nearly full, %d of %d members (x%.2f)", c.Members, c.MemberLimit, s.Crowd)
		} else {
			s.Why += fmt.Sprintf(" — but %d rolebot(s) already wear this badge (x%.2f)", others, s.Crowd)
		}
	}
	if s.Verdict.OK && s.TZ < 0.9 {
		s.Why += fmt.Sprintf(" — its CEO plays other hours (x%.2f)", s.TZ)
	}
	if s.Verdict.OK && s.Race != 1 && s.RaceWhy != "" {
		s.Why += fmt.Sprintf(" — %s (x%.2f)", s.RaceWhy, s.Race)
	}
	return s
}

// corpBestTagLabel names the tag this pilot likes most about a corporation, for
// the Why text.
func corpBestTagLabel(p pilotIdentity, tags []string) string {
	best, bestV := "", -1.0
	for _, k := range tags {
		t, ok := corpTagByKey(k)
		if !ok {
			continue
		}
		if v := t.Appeal(p); v > bestV {
			best, bestV = t.Label, v
		}
	}
	if best == "" {
		return "unclassified"
	}
	return strings.ToLower(best)
}

// ── distances ───────────────────────────────────────────────────────────────

// corpStationSystems maps every NPC corporation to the systems it has a station
// with a clone bay in: a pilot can live there. A station without one is no home,
// so it neither draws a pilot nor counts as reach. One query for the whole
// catalogue instead of one per corporation.
func corpStationSystems(ctx context.Context, db *sql.DB) (map[int64][]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT s.corporationID, s.solarSystemID
		FROM staStations s
		JOIN crpNPCCorporations n ON n.corporationID = s.corporationID
		JOIN staOperationServices os ON os.operationID = s.operationID AND os.serviceID = ?`, serviceCloning)
	if err != nil {
		return nil, fmt.Errorf("corpStationSystems: %w", err)
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var corpID, sysID int64
		if err := rows.Scan(&corpID, &sysID); err != nil {
			return nil, err
		}
		out[corpID] = append(out[corpID], sysID)
	}
	return out, rows.Err()
}

// corpJumpMap is a hop-count BFS from one or more systems over the gate graph,
// giving jumps to the nearest source. It is not the priced routing (router.go): the
// distance to a corporation must not depend on one pilot's nerves. Many sources
// run in one walk.
func (m *Mod) corpJumpMap(ctx context.Context, sources ...int64) (map[int64]int, error) {
	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	dist := map[int64]int{}
	var frontier []int64
	for _, s := range sources {
		if s == 0 {
			continue
		}
		if _, seen := dist[s]; seen {
			continue
		}
		dist[s] = 0
		frontier = append(frontier, s)
	}
	for d := 1; len(frontier) > 0; d++ {
		var next []int64
		for _, s := range frontier {
			for _, nb := range u.adj[s] {
				if _, seen := dist[nb]; seen {
					continue
				}
				dist[nb] = d
				next = append(next, nb)
			}
		}
		frontier = next
	}
	return dist, nil
}

// nearestJump is the distance to the closest of a corporation's systems, from a BFS
// map measured at the pilot's home. -1 when none is reachable or it owns none.
func nearestJump(dist map[int64]int, systems []int64) int {
	best := -1
	for _, s := range systems {
		if d, ok := dist[s]; ok && (best < 0 || d < best) {
			best = d
		}
	}
	return best
}

// ── rankings ────────────────────────────────────────────────────────────────

// corpCandidate is one pilot ranked against one corporation (the corporation
// page's readout). The reverse listing uses the same struct.
type corpCandidate struct {
	Pilot pilotIdentity
	Corp  CorpFacts
	Score corpScore
}

// rankPilotsForCorp answers "who would walk in here": every enabled rostered
// pilot priced against one corporation, best first, refusals last with their
// reason. Bounded by limit because a roster can be thousands.
func (m *Mod) rankPilotsForCorp(ctx context.Context, c CorpFacts, limit int) ([]corpCandidate, error) {
	pilots, err := m.loadPilotIdentities(ctx, 0, true)
	if err != nil {
		return nil, err
	}
	mx, err := factionStandings(ctx, m.db)
	if err != nil {
		return nil, err
	}
	corpSystems, err := m.corpSystemsAll(ctx)
	if err != nil {
		return nil, err
	}
	// Measured once from the corporation outward, which answers every pilot at once.
	jumps, err := m.corpJumpMap(ctx, corpSystems[c.CorpID]...)
	if err != nil {
		return nil, err
	}
	// Crowding is measured against the whole open catalogue, so even the
	// one-corporation page has to read it.
	catalogue, err := m.corpRankingCatalogue(ctx)
	if err != nil {
		return nil, err
	}
	baseline := corpCrowdBaseline(catalogue)
	out := make([]corpCandidate, 0, len(pilots))
	for _, p := range pilots {
		out = append(out, corpCandidate{
			Pilot: p, Corp: c,
			Score: scoreCorpForPilot(p, c, mx, nearestJump(jumps, []int64{p.HomeSystemID}), baseline),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score.Verdict.OK != out[j].Score.Verdict.OK {
			return out[i].Score.Verdict.OK
		}
		return out[i].Score.Score > out[j].Score.Score
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// rankCorpsForPilot is the reverse: which corporations this pilot would choose,
// best first. The panel calls it to show the answer without acting on it (docs/CORPS.md).
func (m *Mod) rankCorpsForPilot(ctx context.Context, p pilotIdentity, limit int) ([]corpCandidate, error) {
	corps, err := m.corpRankingCatalogue(ctx)
	if err != nil {
		return nil, err
	}
	mx, err := factionStandings(ctx, m.db)
	if err != nil {
		return nil, err
	}
	corpSystems, err := m.corpSystemsAll(ctx)
	if err != nil {
		return nil, err
	}
	jumps := map[int64]int{}
	if p.HomeSystemID != 0 {
		if jumps, err = m.corpJumpMap(ctx, p.HomeSystemID); err != nil {
			return nil, err
		}
	}
	baseline := corpCrowdBaseline(corps)
	out := make([]corpCandidate, 0, len(corps))
	for _, c := range corps {
		out = append(out, corpCandidate{
			Pilot: p, Corp: c,
			Score: scoreCorpForPilot(p, c, mx, nearestJump(jumps, corpSystems[c.CorpID]), baseline),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score.Verdict.OK != out[j].Score.Verdict.OK {
			return out[i].Score.Verdict.OK
		}
		return out[i].Score.Score > out[j].Score.Score
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// eligibleCounts is the cheap half of the ranking for the list page: how many
// enabled pilots pass the hard rules for each corporation, with no distance or tags.
func (m *Mod) eligibleCounts(ctx context.Context, corps []CorpFacts) (map[int64]int, error) {
	pilots, err := m.loadPilotIdentities(ctx, 0, true)
	if err != nil {
		return nil, err
	}
	mx, err := factionStandings(ctx, m.db)
	if err != nil {
		return nil, err
	}
	out := map[int64]int{}
	for _, c := range corps {
		// Count who would pass if the switch were open, so the number is a reason to open it.
		open := c
		open.Policy.Joinable = true
		n := 0
		for _, p := range pilots {
			if corpEligibility(p, open, mx).OK {
				n++
			}
		}
		out[c.CorpID] = n
	}
	return out, nil
}

// pctOf is a small display helper: a score as a rounded percentage of the
// best score in a list, so the page can draw bars without the template
// doing arithmetic.
func pctOf(v, best float64) int {
	if best <= 0 {
		return 0
	}
	return int(math.Round(100 * v / best))
}
