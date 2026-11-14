package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
)

// The diplomacy power: a CEO deciding who its corporation's friends, allies and
// rivals are. It writes crpStandings (db/corpstandingdb.go), which the relations
// snapshot (corprel.go) folds: +2.5 or better is never engaged, -2.5 or worse is
// engaged outside policed space. Every other player corporation is scored on one
// +-10 scale from signals the world records, weighted by Charter. Passive terms
// are damped for a corporation based in another region, direct ones are not. The
// CEO may only change a row it wrote and nobody has touched since
// (ceoRelOwnership); a human edit yields the pair for good.
// See docs/comment-notes-b04.md#diplomacy-power.

// The four declarations a CEO makes. Absolute values, written as-is: the ally
// value clears StandingDevoted, which is what relAllied reads, and the rival
// value clears StandingDisliked, which is what stanceOf reads.
const (
	ceoRelAlly   = 10.0
	ceoRelFriend = 5.0
	ceoRelRival  = -5.0
	ceoRelEnemy  = -10.0
)

const (
	// ceoRelWindowDays is how far back the killboard is read. Past it a
	// killing is forgotten outright; inside it, it decays by the CEO's
	// half-life.
	ceoRelWindowDays = 14
	// ceoRelAwayMul damps the passive terms for a corporation based in
	// another region.
	ceoRelAwayMul = 0.6
	// ceoRelGrievanceCap / ceoRelComradeCap bound the direct terms, so a
	// week of war does not produce a number no amount of anything can move.
	ceoRelGrievanceCap = 8.0
	ceoRelComradeWorth = 0.6
	ceoRelComradeCap   = 3.0
	// ceoRelComplementWorth is what complementary work is worth.
	ceoRelComplementWorth = 1.5
	// ceoRelNavyDislike is what an outlaw outfit holds against a police one, before
	// aggression. Smaller than the police's own weight: the navy hunts the pirate,
	// the pirate mostly avoids the navy.
	ceoRelNavyDislike = 2.0
	// ceoRelPrejudiceWeight is what a pole-strength prejudice is worth: at the
	// extreme a xenophobe's own people reach ally range alone and a hostile people
	// lands between rival and enemy.
	ceoRelPrejudiceWeight = 6.0
	// ceoRelWarTerm is what a live war with us adds on top of the war's own fold
	// (relFold): a CEO names the corporation it is fighting.
	ceoRelWarTerm = -4.0
	// ceoRelCriminalSec is the average member security status at or below
	// which a corporation reads as a pirate outfit whatever its tags say.
	ceoRelCriminalSec = -2.0
	// ceoRelHardCap is every declaration a CEO will hold at once, far under
	// the core's MaxCorpStandings (300), which a human director shares.
	ceoRelHardCap = 40
)

// ceoRelSoftTags are the trades that make a corporation worth robbing;
// ceoRelArmedTags the ones that make it a fight instead.
var (
	ceoRelSoftTags  = []string{"mining", "industry", "shipbuilding", "research", "trade", "hauling"}
	ceoRelArmedTags = []string{"combat", "security", "outlaw", "mercenary"}
	// ceoRelComplements are pairs of trades that feed each other.
	ceoRelComplements = [][2]string{
		{"mining", "industry"}, {"mining", "shipbuilding"},
		{"industry", "trade"}, {"industry", "hauling"},
		{"shipbuilding", "trade"}, {"shipbuilding", "hauling"},
		{"research", "industry"}, {"research", "shipbuilding"},
		{"security", "mining"}, {"security", "hauling"},
	}
	// ceoRelCompeteTags are the trades two corporations in one region fight
	// over.
	ceoRelCompeteTags = []string{"mining", "industry", "shipbuilding", "research", "trade"}
)

// ── the pure half ───────────────────────────────────────────────────────────

// ceoRelParty is what the arithmetic needs to know about one corporation.
type ceoRelParty struct {
	CorpID     int64
	Name       string
	Tags       []string
	AllianceID int64
	Region     string
	Race       int64
	AvgSec     float64
	Members    int
}

func (p ceoRelParty) has(tag string) bool { return containsStr(p.Tags, tag) }

func (p ceoRelParty) hasAny(tags []string) bool {
	for _, t := range tags {
		if p.has(t) {
			return true
		}
	}
	return false
}

// outlaw reports whether a corporation reads as a pirate outfit: it says so,
// or its members' records say so.
func (p ceoRelParty) outlaw() bool {
	return p.has("outlaw") || (p.Members > 0 && p.AvgSec <= ceoRelCriminalSec)
}

// ceoRelInputs is every signal about one target, from one viewer.
type ceoRelInputs struct {
	Self, Target ceoRelParty
	// RaceStanding is the viewer's empire's standing toward the target's
	// (blocStanding: 10 for the same people). RaceKnown is false when a people
	// cannot be resolved to an empire, which is not a standing of 0.
	RaceStanding float64
	RaceKnown    bool
	Grievance    float64 // decayed count of our members they killed
	Comrades     float64 // decayed count of fights fought on the same side
	TheirView    float64 // what they have declared about us
	AtWar        bool
}

// ceoRelTerm is one signal's contribution, kept for the ledger text.
type ceoRelTerm struct {
	Label string
	V     float64
}

// ceoRelScore is what this CEO thinks of the target, and why. Pure: no clock,
// no database, no randomness, so every cell of it is a test.
func ceoRelScore(in ceoRelInputs, ch Charter) (float64, []ceoRelTerm) {
	var terms []ceoRelTerm
	add := func(v float64, format string, args ...any) {
		if v != 0 {
			terms = append(terms, ceoRelTerm{fmt.Sprintf(format, args...), v})
		}
	}
	self, them := in.Self, in.Target
	near := ceoRelAwayMul
	if self.Region != "" && self.Region == them.Region {
		near = 1
	}

	// Race: prejudice first (the CEO's tolerance), then the empires' politics (loyalty).
	if self.Race != 0 && them.Race != 0 {
		same := self.Race == them.Race
		switch {
		case ch.Xenophobia > 0:
			// Not its kind: any other race from the own-race stage, a race its
			// empire is not allied with (standing not above zero) before that.
			hostile := !same && (xenoPhobeStage(ch.Xenophobia) >= xenoStageOwnRace ||
				(in.RaceKnown && in.RaceStanding <= 0))
			switch {
			case same:
				add(ch.PrejudiceWeight*ch.Xenophobia*near, "a fellow %s outfit", raceName(them.Race))
			case hostile:
				add(-ch.PrejudiceWeight*ch.Xenophobia*near, "%s — not its kind", raceName(them.Race))
			}
		case ch.Xenophilia > 0 && !same:
			add(ch.PrejudiceWeight*ch.Xenophilia*near, "%s — a people it wants to know", raceName(them.Race))
		}
		if !same && in.RaceKnown {
			v := ch.PoliticsWeight * in.RaceStanding / 10 * near
			if v > 0 {
				add(v, "%s, old friends of the %s", raceName(them.Race), raceName(self.Race))
			} else {
				add(v, "%s, old enemies of the %s", raceName(them.Race), raceName(self.Race))
			}
		}
	}

	selfOutlaw, themOutlaw := self.outlaw(), them.outlaw()

	// Predation: an outlaw outfit marks soft targets, never a fellow pirate outfit.
	if selfOutlaw && !themOutlaw && them.hasAny(ceoRelSoftTags) {
		soft := 1.0
		what := "industrial"
		for _, t := range ceoRelSoftTags {
			if them.has(t) {
				what = corpTagLabel(t)
				break
			}
		}
		if them.hasAny(ceoRelArmedTags) {
			// Low enough that no CEO's rival line (-2.0 against a PreyWeight up to ~9) is
			// crossed on this alone: an outfit that shoots back is a fight, not a mark.
			soft = 0.2
			add(-ch.PreyWeight*soft*near, "an armed %s outfit — a poor mark", strings.ToLower(what))
		} else {
			add(-ch.PreyWeight*soft*near, "a soft %s target", strings.ToLower(what))
		}
	}
	if selfOutlaw && them.has("security") {
		add(-ceoRelNavyDislike*near, "a police outfit")
	}

	// ── law: a lawful outfit marks the pirates ──────────────────────────
	if !selfOutlaw && themOutlaw {
		w := ch.LawWeight
		if self.has("security") && w < 4 {
			w = 4 // a police outfit hunts pirates whatever its CEO is like
		}
		if them.has("outlaw") {
			add(-w*near, "a pirate outfit")
		} else {
			add(-w*near, "a criminal record (average %.1f)", them.AvgSec)
		}
	}

	// ── trade ───────────────────────────────────────────────────────────
	if !selfOutlaw && !themOutlaw {
		for _, pair := range ceoRelComplements {
			a, b := pair[0], pair[1]
			if (self.has(a) && them.has(b)) || (self.has(b) && them.has(a)) {
				add(ceoRelComplementWorth*near, "our trades feed each other")
				break
			}
		}
		if near == 1 {
			for _, t := range ceoRelCompeteTags {
				if self.has(t) && them.has(t) {
					add(-ch.CompeteWeight, "competes with us for %s in %s", t, them.Region)
					break
				}
			}
		}
	}

	// ── what they have done ─────────────────────────────────────────────
	if in.Grievance > 0 {
		add(-math.Min(in.Grievance*ch.GrievanceWeight, ceoRelGrievanceCap),
			"killed %s of ours lately", countWord(in.Grievance))
	}
	if in.Comrades > 0 {
		add(math.Min(in.Comrades*ceoRelComradeWorth, ceoRelComradeCap), "fought beside us")
	}
	switch {
	case in.TheirView > 0:
		add(in.TheirView*ch.ReciprocityWarm, "they count us friends")
	case in.TheirView < 0:
		add(in.TheirView*ch.ReciprocityCold, "they count us enemies")
	}
	if in.AtWar {
		add(ceoRelWarTerm, "at war with us")
	}

	total := 0.0
	for _, t := range terms {
		total += t.V
	}
	if in.AtWar && total > 0 {
		total = 0 // no friendship with a corporation shooting at us
	}
	total = clampStanding(total)
	sort.SliceStable(terms, func(i, j int) bool { return math.Abs(terms[i].V) > math.Abs(terms[j].V) })
	return total, terms
}

// countWord renders a decayed count for prose: "one", "two", "several".
func countWord(v float64) string {
	switch n := int(v + 0.5); {
	case n <= 1:
		return "one"
	case n == 2:
		return "two"
	case n <= 5:
		return "several"
	}
	return "many"
}

// ceoRelReason is the one-line why for the ledger: the two strongest terms
// that point the same way as the decision.
func ceoRelReason(terms []ceoRelTerm, want float64) string {
	var parts []string
	for _, t := range terms {
		if (want > 0 && t.V > 0) || (want < 0 && t.V < 0) || want == 0 {
			parts = append(parts, t.Label)
		}
		if len(parts) == 2 {
			break
		}
	}
	if len(parts) == 0 {
		return "nothing between us any more"
	}
	return strings.Join(parts, "; ")
}

// ceoRelVerb names a declaration for the ledger.
func ceoRelVerb(v float64) string {
	switch v {
	case ceoRelAlly:
		return "ally"
	case ceoRelFriend:
		return "friend"
	case ceoRelRival:
		return "rival"
	case ceoRelEnemy:
		return "enemy"
	}
	return "withdraw"
}

// ceoRelPhrase is how the ledger says what the CEO did.
func ceoRelPhrase(prev, want float64, name string) string {
	switch want {
	case ceoRelAlly:
		return "declared " + name + " an ally"
	case ceoRelFriend:
		if prev == ceoRelAlly {
			return "cooled " + name + " from ally to friend"
		}
		return "declared " + name + " a friend"
	case ceoRelRival:
		if prev == ceoRelEnemy {
			return "eased " + name + " from enemy to rival"
		}
		if prev > 0 {
			return "turned on " + name + " — now a rival"
		}
		return "declared " + name + " a rival"
	case ceoRelEnemy:
		if prev > 0 {
			return "turned on " + name + " — now an enemy"
		}
		return "declared " + name + " an enemy"
	}
	return "withdrew its declaration on " + name
}

// ceoConsiderAlliance is where a CEO with mutual allies founds a real in-game
// alliance (alliancefound.go), behind the founding switch and its cap. Joining
// one its allies are in is the alliance power's (allianceSeek).
func (m *Mod) ceoConsiderAlliance(ctx context.Context, c *corpCEO, ch Charter, allies []int64) error {
	if aid := m.corpAllianceID(ctx, c.CorpID); aid != 0 {
		return nil
	}
	return m.allianceConsiderFounding(ctx, c, ch, allies)
}

// ── the reads ───────────────────────────────────────────────────────────────

// ceoRelPeople is who a corporation's members are.
type ceoRelPeople struct {
	Race    int64
	AvgSec  float64
	Members int
}

// ceoRelPeoples reads, per corporation, its dominant race (a strict majority
// of its members; 0 for a mixed crew) and its members' average security
// status. One grouped query for every candidate.
func (m *Mod) ceoRelPeoples(ctx context.Context, corpIDs []int64) (map[int64]ceoRelPeople, error) {
	out := map[int64]ceoRelPeople{}
	if len(corpIDs) == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, COALESCE(raceID, 0), COUNT(*), SUM(COALESCE(securityRating, 0))
		  FROM character_
		 WHERE corporationID IN (`+placeholders(len(corpIDs))+`)
		 GROUP BY corporationID, raceID`, int64Args(corpIDs)...)
	if err != nil {
		return nil, fmt.Errorf("ceoRelPeoples: %w", err)
	}
	defer rows.Close()
	type acc struct {
		n, best  int
		bestRace int64
		sec      float64
	}
	agg := map[int64]*acc{}
	for rows.Next() {
		var corpID, race int64
		var n int
		var sec float64
		if err := rows.Scan(&corpID, &race, &n, &sec); err != nil {
			return nil, fmt.Errorf("ceoRelPeoples scan: %w", err)
		}
		a := agg[corpID]
		if a == nil {
			a = &acc{}
			agg[corpID] = a
		}
		a.n += n
		a.sec += sec
		if n > a.best {
			a.best, a.bestRace = n, race
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ceoRelPeoples: %w", err)
	}
	for corpID, a := range agg {
		p := ceoRelPeople{Members: a.n}
		if a.n > 0 {
			p.AvgSec = a.sec / float64(a.n)
			if 2*a.best > a.n {
				p.Race = a.bestRace
			}
		}
		out[corpID] = p
	}
	return out, nil
}

// ceoRelPartyOf assembles one side of the arithmetic. The corporation's own
// raceID (its founder's people) stands in for a crew with no majority.
func ceoRelPartyOf(f CorpFacts, p ceoRelPeople) ceoRelParty {
	race := p.Race
	if race == 0 {
		race = f.RaceID
	}
	return ceoRelParty{
		CorpID: f.CorpID, Name: f.Name, Tags: f.Policy.Tags,
		AllianceID: f.AllianceID, Region: f.RegionName,
		Race: race, AvgSec: p.AvgSec, Members: p.Members,
	}
}

// ceoRelTheirView is what every other corporation has declared about us. A
// bot-run corporation's declared pair reads at its ledger value, not the capped
// written number (relApplyWriteCaps), or one cap rotation would swing the other's
// baseline and back. Warm side only; relSeatMargin damps rivals.
func (m *Mod) ceoRelTheirView(ctx context.Context, corpID int64) (map[int64]float64, error) {
	return m.ceoRelStandingMap(ctx, `
		SELECT s.corporationID, COALESCE(r.value, s.standing) FROM crpStandings s
		  LEFT JOIN rolebot_corp_relations r
		         ON r.corporationID = s.corporationID AND r.toID = s.toID AND r.yielded = 0
		        AND s.standing > 0 AND r.value > s.standing
		 WHERE s.toID = ?`, corpID)
}

// ceoRelLive is what our corporation has declared, whoever declared it.
func (m *Mod) ceoRelLive(ctx context.Context, corpID int64) (map[int64]float64, error) {
	return m.ceoRelStandingMap(ctx,
		`SELECT toID, standing FROM crpStandings WHERE corporationID = ?`, corpID)
}

func (m *Mod) ceoRelStandingMap(ctx context.Context, q string, corpID int64) (map[int64]float64, error) {
	rows, err := m.db.QueryContext(ctx, q, corpID)
	if err != nil {
		return nil, fmt.Errorf("ceo diplomacy standings: %w", err)
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var v float64
		if err := rows.Scan(&id, &v); err != nil {
			return nil, fmt.Errorf("ceo diplomacy standings scan: %w", err)
		}
		out[id] = v
	}
	return out, rows.Err()
}
