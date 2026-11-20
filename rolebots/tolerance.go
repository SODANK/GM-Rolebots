package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	mrand "math/rand"
	"sort"
	"strings"
	"sync/atomic"
)

// The tolerance trait: what race means to a pilot (tuning.go owns the dial; this file owns the population, the
// knowledge of who is what, and the rules). At zero race is nothing special. Away from zero it acts in stages, and
// every seam reads the same few functions here so the stages cannot disagree about who counts as what. The xenophobe
// has four stages (xenoPhobeStage; constants in tuning.go), the xenophile three tiers (xenoWary, xenoStrong,
// xenoExtreme). Hostility follows the empire's politics, not every difference, and the employer's liked factions are
// always exempt. Raceless hulls and products are never refused. See docs/comment-notes-b19.md#tolerance-stages.

// ── the population ──────────────────────────────────────────────────────────

const (
	// Wide and polarised, by the operator's choice: roughly a third of the
	// roster acts on race (|t| > 50) and one pilot in ten is at an extreme.
	toleranceMean = 0.0
	toleranceSD   = 55.0

	// The two traits that say the most about how a pilot sees strangers: a
	// devoted pilot (LOW loyalty) keeps to its own, a nomad (HIGH wanderlust)
	// has met everybody. Nudges, not replacements.
	toleranceLoyaltyLean    = 0.15
	toleranceWanderlustLean = 0.15

	// toleranceSeedMarker must fit rolebot_traits.trait, VARCHAR(16).
	toleranceSeedMarker = "_tolseed"
	toleranceSeedBatch  = 1000
)

// toleranceLean is the draw's offset from the two traits it leans on. Positive
// is the xenophobe end.
func toleranceLean(loyalty, wanderlust int) float64 {
	return -toleranceLoyaltyLean*float64(loyalty) - toleranceWanderlustLean*float64(wanderlust)
}

func toleranceFromNormal(z, offset float64) int {
	v := toleranceMean + offset + toleranceSD*z
	return int(math.Round(math.Max(-100, math.Min(100, v))))
}

// toleranceDraw is the generator's draw.
func toleranceDraw(rng *mrand.Rand, offset float64) int {
	return toleranceFromNormal(rng.NormFloat64(), offset)
}

// toleranceSeedValue is the existing roster's draw, deterministic in the
// character ID like leadershipSeedValue.
func toleranceSeedValue(charID int64, traits map[string]int) int {
	h := splitmix64(uint64(charID) ^ 0x746f6c6572616e63 + noiseGolden) // "toleranc"
	u1 := (float64(h>>11) + 0.5) / (1 << 53)
	h = splitmix64(h + noiseGolden)
	u2 := (float64(h>>11) + 0.5) / (1 << 53)
	z := math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2) // Box-Muller
	return toleranceFromNormal(z, toleranceLean(traits[TraitLoyalty], traits[TraitWanderlust]))
}

// toleranceSeedQuery reads one roster batch. Trait keys are literals: see migrateLeadershipSeed on the MariaDB
// CASE/GROUP BY placeholder fold.
var toleranceSeedQuery = fmt.Sprintf(`
	SELECT r.characterID,
	       COALESCE(MAX(CASE WHEN t.trait = '%s' THEN t.value END), 0),
	       COALESCE(MAX(CASE WHEN t.trait = '%s' THEN t.value END), 0),
	       COALESCE(MAX(CASE WHEN t.trait = '%s' THEN 1 END), 0)
	  FROM rolebot_characters r
	  LEFT JOIN rolebot_traits t ON t.characterID = r.characterID
	 WHERE r.characterID > ?
	 GROUP BY r.characterID
	 ORDER BY r.characterID
	 LIMIT ?`, TraitLoyalty, TraitWanderlust, TraitTolerance)

// migrateToleranceSeed gives every rostered pilot without a tolerance value
// one, once (marker row at characterID 0). Cursor-walked, never OFFSET.
func migrateToleranceSeed(ctx context.Context, db *sql.DB) error {
	defer invalidateAllTraits() // writes rolebot_traits (roster.go traits cache)
	var done int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_traits
		WHERE characterID = 0 AND trait = ?`, toleranceSeedMarker).Scan(&done); err != nil {
		return fmt.Errorf("migrateToleranceSeed probe: %w", err)
	}
	if done > 0 {
		return nil
	}
	seeded := 0
	var cursor int64
	for {
		rows, err := db.QueryContext(ctx, toleranceSeedQuery, cursor, toleranceSeedBatch)
		if err != nil {
			return fmt.Errorf("migrateToleranceSeed read: %w", err)
		}
		type pending struct {
			id    int64
			value int
		}
		var batch []pending
		n := 0
		for rows.Next() {
			var id int64
			var loy, wand, has int
			if err := rows.Scan(&id, &loy, &wand, &has); err != nil {
				rows.Close()
				return fmt.Errorf("migrateToleranceSeed scan: %w", err)
			}
			n++
			cursor = id
			if has != 0 {
				continue
			}
			batch = append(batch, pending{id, toleranceSeedValue(id,
				map[string]int{TraitLoyalty: loy, TraitWanderlust: wand})})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("migrateToleranceSeed rows: %w", err)
		}
		if len(batch) > 0 {
			var b strings.Builder
			args := make([]any, 0, len(batch)*3)
			b.WriteString(`INSERT IGNORE INTO rolebot_traits (characterID, trait, value) VALUES `)
			for i, p := range batch {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString("(?, ?, ?)")
				args = append(args, p.id, TraitTolerance, p.value)
			}
			if _, err := db.ExecContext(ctx, b.String(), args...); err != nil {
				return fmt.Errorf("migrateToleranceSeed write: %w", err)
			}
			seeded += len(batch)
		}
		if n < toleranceSeedBatch {
			break
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_traits (characterID, trait, value) VALUES (0, ?, 1)`,
		toleranceSeedMarker); err != nil {
		return fmt.Errorf("migrateToleranceSeed marker: %w", err)
	}
	if seeded > 0 {
		rlogf("rolebots: seeded the tolerance trait on %d pilot(s)", seeded)
	}
	return nil
}

// ── who is what ─────────────────────────────────────────────────────────────

// raceIndex is charID to raceID for every pilot with an account (the whole roster and every real player). A snapshot
// swapped atomically so the combat loop can ask about the pilot on the other side of a grid without a query. A miss
// answers 0, which is no opinion: a pilot created since the last refresh is not judged until the next one.
var raceIndex atomic.Pointer[map[int64]int64]

// bloodlineIndex is the same snapshot for bloodlines, which only the final
// xenophobe stage reads (fleets, corporations, the CEO's desk).
var bloodlineIndex atomic.Pointer[map[int64]int64]

func raceOf(charID int64) int64 {
	if p := raceIndex.Load(); p != nil && charID != 0 {
		return (*p)[charID]
	}
	return 0
}

func bloodlineOf(charID int64) int64 {
	if p := bloodlineIndex.Load(); p != nil && charID != 0 {
		return (*p)[charID]
	}
	return 0
}

// ── the xenophobe's stages ──────────────────────────────────────────────────

const (
	xenoStageNone      = 0
	xenoStagePrefer    = 1 // prefers its own race and allies
	xenoStageAllied    = 2 // only its own race and allies
	xenoStageOwnRace   = 3 // only its own race
	xenoStageBloodline = 4 // only its own race, and among people its own bloodline
)

// xenoPhobeStage is the stage a xenophobia (0..1) sits at.
func xenoPhobeStage(phobia float64) int {
	switch {
	case phobia <= 0:
		return xenoStageNone
	case phobia < xenoPhobeAllied:
		return xenoStagePrefer
	case phobia < xenoPhobeOwnRace:
		return xenoStageAllied
	case phobia < xenoPhobeBloodline:
		return xenoStageOwnRace
	}
	return xenoStageBloodline
}

// xenoBloodlineBars reports whether a final-stage xenophobe of one bloodline shuts out a pilot of another. Same race
// only (a different race is the race rules' business), and never on an unknown bloodline.
func xenoBloodlineBars(phobia float64, selfRace, selfBloodline, otherRace, otherBloodline int64) bool {
	return xenoPhobeStage(phobia) >= xenoStageBloodline &&
		selfRace != 0 && selfRace == otherRace &&
		selfBloodline != 0 && otherBloodline != 0 && selfBloodline != otherBloodline
}

// raceView is the politics between peoples, read once from the game's own
// tables. Immutable after build.
type raceView struct {
	empire   map[int64]int64      // raceID to its empire faction
	mx       map[[2]int64]float64 // npcStandings
	factions []int64              // every faction, sorted
	peoples  map[int64]int64      // factionID to chrFactions.raceIDs bitmask
	hostile  map[int64][]int64    // raceID to the factions its empire stands against
	foreign  map[int64][]int64    // raceID to the factions not of its people
	// unallied is raceID to the factions whose peoples include neither this race nor any race its empire is allied
	// with: the own+allied stage's refusals.
	unallied map[int64][]int64
	// alliedMask is raceID to the race bits it counts as allied (its own included).
	alliedMask map[int64]int64
	empires    map[int64]bool // the empire factions themselves
}

var raceViewCur atomic.Pointer[raceView]

func loadRaceView() *raceView { return raceViewCur.Load() }

// hostileRace reports whether a pilot of race self counts race other as hostile: its empire stands against theirs,
// or, for the turbo xenophobe, they are simply not its people. Unknown or empire-less peoples (Jove) are never
// hostile short of turbo. Nil-receiver safe: before the view loads only the turbo rule answers.
func (v *raceView) hostileRace(self, other int64, turbo bool) bool {
	if self == 0 || other == 0 || self == other {
		return false
	}
	if turbo {
		return true
	}
	if v == nil {
		return false
	}
	ea, eb := v.empire[self], v.empire[other]
	if ea == 0 || eb == 0 {
		return false
	}
	return blocStanding(v.mx, ea, eb) < 0
}

// alliedRace reports whether race `other` is allied to race `self`: the same
// race, or a race whose empire stands above zero with self's. Unknown or
// empire-less peoples are not allied. Nil-receiver safe (only "same race"
// answers before the view loads).
func (v *raceView) alliedRace(self, other int64) bool {
	if self == 0 || other == 0 {
		return false
	}
	if self == other {
		return true
	}
	if v == nil {
		return false
	}
	return v.alliedMask[self]&other != 0
}

// stationFactions is what a pilot of this race, at this phobia, will not live
// or trade with: soft (avoided while anything else will do) and hard (never).
// Empty at zero and for a people with no empire.
func (v *raceView) stationFactions(race int64, phobia float64) (soft, hard []int64) {
	if v == nil || race == 0 {
		return nil, nil
	}
	switch xenoPhobeStage(phobia) {
	case xenoStagePrefer:
		return v.unallied[race], nil
	case xenoStageAllied:
		// Only its own people and their allies, and among those it would still rather stay with its own.
		return v.foreign[race], v.unallied[race]
	case xenoStageOwnRace, xenoStageBloodline:
		return v.foreign[race], v.foreign[race]
	}
	return nil, nil
}

// factionForeign reports whether a faction is foreign to a people at this stage, which is what a xenophobe's home
// veto asks about a station owner: from the own-race stage up any faction not of its people, at the own+allied stage
// a faction of no allied people, below that nothing.
func (v *raceView) factionForeign(race, factionID int64, stage int) bool {
	if v == nil || race == 0 || factionID == 0 {
		return false
	}
	switch {
	case stage >= xenoStageOwnRace:
		return v.peoples[factionID]&race == 0
	case stage == xenoStageAllied:
		return v.peoples[factionID]&v.alliedMask[race] == 0
	}
	return false
}

// buildRaceView reads the empires, the faction matrix and each faction's
// peoples. Built once at boot (static seed data) and retried by the race
// sweep until it succeeds.
func (m *Mod) buildRaceView(ctx context.Context) (*raceView, error) {
	emp, err := m.empireFactionIDs(ctx)
	if err != nil {
		return nil, err
	}
	mx, ok := m.standingsMatrix(ctx)
	if !ok {
		return nil, fmt.Errorf("buildRaceView: faction standings unavailable")
	}
	empires := map[int64]int64{}
	for race, e := range emp {
		empires[race] = e.ID
	}
	rows, err := m.db.QueryContext(ctx, `SELECT factionID, COALESCE(raceIDs, 0) FROM chrFactions`)
	if err != nil {
		return nil, fmt.Errorf("buildRaceView factions: %w", err)
	}
	peoples := map[int64]int64{}
	for rows.Next() {
		var id, mask int64
		if err := rows.Scan(&id, &mask); err != nil {
			rows.Close()
			return nil, fmt.Errorf("buildRaceView scan: %w", err)
		}
		peoples[id] = mask
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return newRaceView(empires, mx, peoples), nil
}

// newRaceView derives the per-race faction lists. Pure, so the harness can
// build one from the shipped numbers without a database.
func newRaceView(empires map[int64]int64, mx map[[2]int64]float64, peoples map[int64]int64) *raceView {
	v := &raceView{
		empire: empires, mx: mx, peoples: peoples,
		hostile: map[int64][]int64{}, foreign: map[int64][]int64{}, empires: map[int64]bool{},
		unallied: map[int64][]int64{}, alliedMask: map[int64]int64{},
	}
	for _, e := range empires {
		v.empires[e] = true
	}
	for race, own := range v.empire {
		mask := race
		for other, theirs := range v.empire {
			if other != race && blocStanding(mx, own, theirs) > 0 {
				mask |= other
			}
		}
		v.alliedMask[race] = mask
	}
	for id := range peoples {
		v.factions = append(v.factions, id)
	}
	sort.Slice(v.factions, func(i, j int) bool { return v.factions[i] < v.factions[j] })
	for race, own := range v.empire {
		for _, f := range v.factions {
			if f == own {
				continue
			}
			if blocStanding(mx, own, f) < 0 {
				v.hostile[race] = append(v.hostile[race], f)
			}
			// A mask of 0 is a faction nobody recorded a people for; it is
			// nobody's, which to a turbo xenophobe is foreign.
			if v.peoples[f]&race == 0 {
				v.foreign[race] = append(v.foreign[race], f)
			}
			if v.peoples[f]&v.alliedMask[race] == 0 {
				v.unallied[race] = append(v.unallied[race], f)
			}
		}
	}
	return v
}

// refreshRaceIndex reloads charID → race.
func (m *Mod) refreshRaceIndex(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, COALESCE(raceID, 0) FROM character_ WHERE accountID > 0`)
	if err != nil {
		return fmt.Errorf("refreshRaceIndex: %w", err)
	}
	defer rows.Close()
	idx := map[int64]int64{}
	for rows.Next() {
		var id, race int64
		if err := rows.Scan(&id, &race); err != nil {
			return fmt.Errorf("refreshRaceIndex scan: %w", err)
		}
		if race != 0 {
			idx[id] = race
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	raceIndex.Store(&idx)
	// Bloodlines are their own read and never fail the race index: only the
	// final xenophobe stage asks, and a miss is simply "no opinion".
	if blx, err := m.loadBloodlineIndex(ctx); err != nil {
		rlogf("rolebots: bloodline index: %v (keeping the previous one)", err)
	} else {
		bloodlineIndex.Store(&blx)
	}
	return nil
}

// raceRefreshSec is how often the index is reloaded.
const raceRefreshSec = 10 * 60

var (
	raceRefreshedAt atomic.Int64
	raceRefreshBusy atomic.Bool
)

// startRaceSweep loads both halves inline, so the first pass after boot already knows who is what, and keeps the
// index fresh.
func (m *Mod) startRaceSweep() {
	ctx := context.Background()
	m.refreshRaceKnowledge(ctx)
	m.registerPeriodic("rolebot-race-index", 30, func(uint64) bool {
		if clock.Unix()-raceRefreshedAt.Load() < raceRefreshSec && loadRaceView() != nil {
			return true
		}
		if !raceRefreshBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("race index refresh", func() {
			defer raceRefreshBusy.Store(false)
			m.refreshRaceKnowledge(context.Background())
		})
		return true
	})
}

func (m *Mod) refreshRaceKnowledge(ctx context.Context) {
	raceRefreshedAt.Store(clock.Unix())
	if loadRaceView() == nil {
		if v, err := m.buildRaceView(ctx); err != nil {
			rlogf("rolebots: race politics: %v (tolerance acts on nothing until it loads)", err)
		} else {
			raceViewCur.Store(v)
		}
	}
	if err := m.refreshRaceIndex(ctx); err != nil {
		rlogf("rolebots: race index: %v (keeping the previous one)", err)
	}
}

// ── the rules each seam reads ───────────────────────────────────────────────

// xenoStanding is what a xenophobe's prejudice makes of a pilot of another race. From the own+allied stage a race its
// empire stands against is disliked; from the own-race stage that race is despised and every other foreign race is
// disliked. Another bloodline of its own race is never made hostile here (the final stage only shuts it out of
// fleets, corporations and the CEO's desk). Callers fold it in with worseStanding; kin still wins (stanceOf).
func xenoStanding(selfRace int64, phobia float64, otherRace int64) float64 {
	if selfRace == 0 || otherRace == 0 || selfRace == otherRace {
		return 0
	}
	stage := xenoPhobeStage(phobia)
	if stage < xenoStageAllied {
		return 0
	}
	hostile := loadRaceView().hostileRace(selfRace, otherRace, false)
	switch {
	case stage >= xenoStageOwnRace && hostile:
		return StandingDespised
	case stage >= xenoStageOwnRace, hostile:
		return StandingDisliked
	}
	return 0
}

// xenoStationFactions is the pilot's refused factions for the station
// filters, with the employer's friends taken back out (see the file header).
func xenoStationFactions(race int64, tn Tuning, exempt []int64) (soft, hard []int64) {
	soft, hard = loadRaceView().stationFactions(race, tn.Xenophobia)
	if len(exempt) == 0 || (len(soft) == 0 && len(hard) == 0) {
		return soft, hard
	}
	ex := make(map[int64]bool, len(exempt))
	for _, id := range exempt {
		ex[id] = true
	}
	drop := func(in []int64) []int64 {
		var out []int64
		for _, id := range in {
			if !ex[id] {
				out = append(out, id)
			}
		}
		return out
	}
	return drop(soft), drop(hard)
}

// mergeIDs is the union of two ID lists, sorted so the SQL a filter builds
// from it is stable.
func mergeIDs(lists ...[]int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, l := range lists {
		for _, id := range l {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// xenoHomeVeto reports whether a xenophobe refuses to LIVE at a station owned
// by this faction: from the own+allied stage a faction of no allied people,
// from the own-race stage any foreign people's. The employer (exempt) never.
func xenoHomeVeto(race int64, tn Tuning, ownerFaction int64, exempt []int64) bool {
	stage := xenoPhobeStage(tn.Xenophobia)
	if stage < xenoStageAllied || ownerFaction == 0 {
		return false
	}
	for _, id := range exempt {
		if id == ownerFaction {
			return false
		}
	}
	return loadRaceView().factionForeign(race, ownerFaction, stage)
}

// xenoHomeJumps is the soft half of the home choice, in jump-equivalents (positive = a better home): a wary xenophobe
// marks a hostile empire's station down, a xenophile marks any foreign empire's up, and the further it leans the further
// it will go for one.
func xenoHomeJumps(race int64, tn Tuning, ownerFaction int64) float64 {
	v := loadRaceView()
	if v == nil || race == 0 || ownerFaction == 0 {
		return 0
	}
	own := v.empire[race]
	switch {
	case tn.Xenophobia > 0:
		// Every stage prefers home: the soft half runs from the first point of
		// the trait, the hard half (xenoHomeVeto) from the own+allied stage.
		if v.peoples[ownerFaction]&v.alliedMask[race] == 0 {
			return -6 * tn.Xenophobia
		}
		if ownerFaction == own {
			return 2 * tn.Xenophobia
		}
	case tn.Xenophilia >= xenoWary:
		if v.empires[ownerFaction] && ownerFaction != own {
			return 6 * tn.Xenophilia
		}
	}
	return 0
}

// xenoCorpMul is the corporation score's race term and the text that explains it. A xenophobe favours its own people
// and marks a hostile race's corporation down; a xenophile favours foreigners. corpRace 0 (a corporation of no
// recorded people) is 1.
func xenoCorpMul(race int64, tn Tuning, corpRace int64) (float64, string) {
	if race == 0 || corpRace == 0 {
		return 1, ""
	}
	switch {
	case tn.Xenophobia > 0:
		p := tn.Xenophobia
		if corpRace == race {
			return 1 + 0.5*p, "its own people"
		}
		if !loadRaceView().alliedRace(race, corpRace) || xenoPhobeStage(p) >= xenoStageOwnRace {
			return math.Max(0.05, 1-0.7*p), raceName(corpRace) + " — not its kind"
		}
		return math.Max(0.05, 1-0.2*p), raceName(corpRace) + " — allies, but not its own"
	case tn.Xenophilia > 0:
		q := tn.Xenophilia
		if corpRace != race {
			return 1 + 0.8*q, raceName(corpRace) + " — a people it wants to know"
		}
		return 1 - 0.3*q, "its own people, which it finds dull"
	}
	return 1, ""
}

// xenoCorpRefusal is the xenophobe's hard door on a corporation: a race that is not allied from the own+allied stage,
// any other race from the own-race stage, and at the final stage a corporation of its own race but another bloodline
// (corpBloodline: a player corporation's CEO's, a tribe's; 0 when none). "" is no refusal.
func xenoCorpRefusal(race, bloodline int64, tn Tuning, corpRace, corpBloodline int64) string {
	stage := xenoPhobeStage(tn.Xenophobia)
	if stage < xenoStageAllied || race == 0 || corpRace == 0 {
		return ""
	}
	if corpRace == race {
		if xenoBloodlineBars(tn.Xenophobia, race, bloodline, corpRace, corpBloodline) {
			return "of another bloodline — this pilot will work for nobody but its own blood"
		}
		return ""
	}
	if stage >= xenoStageOwnRace {
		return fmt.Sprintf("a %s corporation — this pilot will work for nobody but its own people", raceName(corpRace))
	}
	if !loadRaceView().alliedRace(race, corpRace) {
		return fmt.Sprintf("a %s corporation — this pilot works only for its own people and their allies", raceName(corpRace))
	}
	return ""
}

// xenoApplicant is a CEO's race door on one applicant.
type xenoApplicant struct {
	Reject  bool
	Rule    string // "race" or "bloodline" when rejected
	Why     string
	BarMove float64 // added to the fit bar (negative lowers it)
}

// judgeXenoApplicant is the CEO's tolerance applied at the desk. A xenophobe raises the bar for a non-allied race at
// the prefer stage, rejects it from own+allied, rejects every other race from own-race and every other bloodline at
// the final stage. A xenophile lowers the bar for a foreigner, and a devoted one raises it a little for its own kind.
func judgeXenoApplicant(ceoRace, ceoBloodline int64, tn Tuning, appRace, appBloodline int64) xenoApplicant {
	if ceoRace == 0 || appRace == 0 {
		return xenoApplicant{}
	}
	if p := tn.Xenophobia; p > 0 {
		if xenoBloodlineBars(p, ceoRace, ceoBloodline, appRace, appBloodline) {
			return xenoApplicant{Reject: true, Rule: "bloodline",
				Why: "not of the CEO's own bloodline, and this CEO hires no other"}
		}
		if appRace == ceoRace {
			return xenoApplicant{}
		}
		stage := xenoPhobeStage(p)
		allied := loadRaceView().alliedRace(ceoRace, appRace)
		switch {
		case stage >= xenoStageOwnRace:
			return xenoApplicant{Reject: true, Rule: "race",
				Why: fmt.Sprintf("%s, and this CEO hires only its own people", raceName(appRace))}
		case stage == xenoStageAllied && !allied:
			return xenoApplicant{Reject: true, Rule: "race",
				Why: fmt.Sprintf("%s, not a people this CEO's empire counts as allies", raceName(appRace))}
		case stage == xenoStagePrefer && !allied:
			return xenoApplicant{BarMove: 0.3 * xenoPhobeAllied,
				Why: fmt.Sprintf("%s — it has to be better than one of its own", raceName(appRace))}
		}
		return xenoApplicant{}
	}
	if q := tn.Xenophilia; q >= xenoWary {
		if appRace != ceoRace {
			return xenoApplicant{BarMove: -0.4 * q,
				Why: fmt.Sprintf("%s — a foreigner, which this CEO wants", raceName(appRace))}
		}
		if q >= xenoExtreme {
			return xenoApplicant{BarMove: 0.1, Why: "its own people, which this CEO has plenty of"}
		}
	}
	return xenoApplicant{}
}

// skillEthnicRelations is typeID 3368, the skill that lets a player corporation recruit outside its founder's race
// (playerCorpRefusal).
const skillEthnicRelations = 3368

// xenoEthnicGoal is the Ethnic Relations level a xenophile wants: II when it seeks foreigners out, IV when devoted
// to them, and at least II for a xenophile of any strength who runs a corporation. 0 for everybody else.
func xenoEthnicGoal(tn Tuning, isCEO bool) int {
	q := tn.Xenophilia
	switch {
	case q >= xenoExtreme:
		return 4
	case q >= xenoStrong:
		return 2
	case isCEO && q >= xenoWary:
		return 2
	}
	return 0
}

// xenoTierLabel names where a pilot sits on the axis, for the panels.
func xenoTierLabel(tn Tuning) string {
	switch xenoPhobeStage(tn.Xenophobia) {
	case xenoStageBloodline:
		return "bloodline purist"
	case xenoStageOwnRace:
		return "own race only"
	case xenoStageAllied:
		return "own race and allies only"
	case xenoStagePrefer:
		return "prefers its own"
	}
	switch {
	case tn.Xenophilia >= xenoExtreme:
		return "devoted xenophile"
	case tn.Xenophilia >= xenoStrong:
		return "xenophile"
	case tn.Xenophilia >= xenoWary:
		return "curious about strangers"
	}
	return xenoNeutralLabel
}

// xenoNeutralLabel is xenoTierLabel's answer at zero, and below the curious
// line on the xenophile pole.
const xenoNeutralLabel = "race is nothing special"

// ── the home seams (moveout.go, corphome.go) ────────────────────────────────

// refusesHome is despisesStationOwner plus the xenophobe's veto: a station
// owned by a people the pilot will not live among.
func (m *Mod) refusesHome(ctx context.Context, b *botRun, corpID int64) bool {
	if b == nil {
		return false
	}
	if m.despisesStationOwner(ctx, b.Profile, corpID) {
		return true
	}
	if xenoPhobeStage(b.Tune.Xenophobia) < xenoStageAllied || corpID == 0 {
		return false
	}
	var liked []int64
	if b.Profile != nil {
		liked = b.Profile.LikedFactions()
	}
	return xenoHomeVeto(raceOf(b.CharID), b.Tune, m.corpFactionID(ctx, corpID), liked)
}

// xenoHomeScore is the soft half, in jump-equivalents for the caller to price
// in its own units. 0 unless the pilot is at least wary or curious, and 0 for
// a station of a faction its employer likes.
func (m *Mod) xenoHomeScore(ctx context.Context, b *botRun, corpID int64) float64 {
	if b == nil || corpID == 0 || (b.Tune.Xenophobia <= 0 && b.Tune.Xenophilia < xenoWary) {
		return 0
	}
	f := m.corpFactionID(ctx, corpID)
	if f == 0 {
		return 0
	}
	if b.Profile != nil {
		for _, id := range b.Profile.LikedFactions() {
			if id == f && b.Tune.Xenophobia > 0 {
				return 0
			}
		}
	}
	return xenoHomeJumps(raceOf(b.CharID), b.Tune, f)
}

// ── ships and products (fits, the build book, corp production) ──────────────

// xenoItemMul is what a pilot's race feeling does to a hull it might fly or a product it might build, by the item's
// invTypes.raceID. allowed=false is a refusal (a fit falls back, the build book builds something else). A raceless
// item and a pilot of unknown race are always x1 and allowed. Per-stage values: docs/comment-notes-b19.md#tolerance-stages.
func xenoItemMul(selfRace int64, phobia float64, itemRace int64) (float64, bool) {
	if itemRace == 0 || selfRace == 0 {
		return 1, true
	}
	own := itemRace == selfRace
	stage := xenoPhobeStage(phobia)
	if stage == xenoStageNone {
		if own {
			return 1 + raceAffinityBonus, true
		}
		return 1, true
	}
	allied := own || loadRaceView().alliedRace(selfRace, itemRace)
	switch stage {
	case xenoStagePrefer:
		switch {
		case own:
			return 1 + raceAffinityBonus + 2*phobia, true
		case allied:
			return 1 + phobia, true
		}
		return math.Max(0.05, 1-1.6*phobia), true
	case xenoStageAllied:
		switch {
		case own:
			return xenoOwnItemMul, true
		case allied:
			return xenoAlliedItemMul, true
		}
		return 0, false
	}
	if own {
		return xenoOwnItemMul, true
	}
	return 0, false
}

const (
	// xenoOwnItemMul / xenoAlliedItemMul are the prefer stage's values at its
	// top end, held flat from there: past 25 the hard rule does the work and
	// the bonus only orders what is left.
	xenoOwnItemMul    = 1 + raceAffinityBonus + 2*xenoPhobeAllied
	xenoAlliedItemMul = 1 + xenoPhobeAllied
)

// xenoBuildMul is xenoItemMul for something a pilot would build rather than fly: the prejudice alone, without the
// own-race affinity a neutral pilot has for its own hulls, so a builder at tolerance zero builds what it always did.
// allowed=false is a product its maker will not put its name to.
func xenoBuildMul(who pilotRace, itemRace int64) (float64, bool) {
	mul, ok := xenoItemMul(who.Race, who.Phobia, itemRace)
	if !ok {
		return 0, false
	}
	base, _ := xenoItemMul(who.Race, 0, itemRace)
	return mul / base, true
}

// loadBloodlineIndex reads charID → bloodlineID for every pilot with an
// account.
func (m *Mod) loadBloodlineIndex(ctx context.Context) (map[int64]int64, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, COALESCE(bloodlineID, 0) FROM character_ WHERE accountID > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, bl int64
		if err := rows.Scan(&id, &bl); err != nil {
			return nil, err
		}
		if bl != 0 {
			out[id] = bl
		}
	}
	return out, rows.Err()
}
