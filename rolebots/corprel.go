package rolebots

// Corporation politics: what one corporation thinks of another, for corporations
// with no faction behind them. A snapshot (relSnapshot) is built periodically from
// crpStandings, allianceRelationships and the wars table, then swapped in through an
// atomic pointer, so the combat tick reads it with no lock and no query.
// A miss is neutral, never hostile: a failed rebuild, a booting module or an
// undeclared corporation must read as no opinion.
// See docs/comment-notes-b06.md#corp-relations.

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// relSnapshot is one built view of the shard's corporate politics. Every field
// is written once at build and never mutated afterwards, which is what makes it
// safe to hand to the combat goroutine through an atomic.Value.
type relSnapshot struct {
	// at is when it was built (unix seconds), for the panel.
	at int64

	// corp is the resolved standing one corporation holds toward another, directed and
	// already symmetric when relationsMutual() was on at build. Only declared pairs
	// are present; everything else is absent and reads 0.
	corp map[[2]int64]float64

	// ally maps a corporation to its alliance, so a war declared at alliance level is
	// found from a member without a query. A corporation in no alliance is absent.
	ally map[int64]int64

	// wars is every active war, keyed by the unordered pair of its two owner IDs
	// (corporation or alliance), since a war is symmetric. The rows are kept, not a
	// precomputed boolean: War.Hostile has two 24-hour boundaries and relAtWar re-runs it.
	wars map[[2]int64][]evedb.War

	// enemySys maps each corporation that has an enemy to the systems where one of its
	// enemies works or keeps an address. Built for the viewer, so a read is one lookup.
	enemySys map[int64]map[int64]bool

	// enemies is who those enemies are, kept for the panel and for the war
	// opportunity term. Values are the resolved standing.
	enemies map[int64]map[int64]float64

	// warEnemies and warSys are the war half of the two above. A war enemy may shoot in
	// highsec (seclaw exempts the shot), a declared rival only outside it, so routing
	// and working bots weigh them differently (router.go, combat_embodied.go).
	warEnemies map[int64]map[int64]bool
	warSys     map[int64]map[int64]bool
}

// relCurrent holds the live snapshot. Nil until the first build; every reader
// tolerates that and answers neutral. An atomic.Pointer is type-safe and needs no
// assertion at the combat loop's read site.
var relCurrent atomic.Pointer[relSnapshot]

func loadRelSnapshot() *relSnapshot {
	if !relationsEnabled() {
		return nil
	}
	return relCurrent.Load()
}

// relSnapshotAt reports when the live snapshot was built, 0 if there is none.
func relSnapshotAt() int64 {
	if s := relCurrent.Load(); s != nil {
		return s.at
	}
	return 0
}

// relStaleCeilingSec is how long a snapshot may go unrefreshed before its war half
// stops being believed. A stale standing only changes avoidance, but a stale war
// flag makes a bot fire in highsec as a real crime if the war has ended.
const relStaleCeilingSec = 30 * 60

// relWarsUsable reports whether the snapshot is fresh enough to be trusted
// about wars.
func relWarsUsable(s *relSnapshot) bool {
	return s != nil && clock.Unix()-s.at <= relStaleCeilingSec
}

// ── the readers ─────────────────────────────────────────────────────────────
//
// All three are pure lookups over the loaded snapshot and all three are safe
// from the embodied tier. None of them queries anything, ever.

// relCorpStanding is what one corporation holds toward another: 0 when either side
// is unknown, they are the same corporation, or nobody has said anything. Like
// blocStanding (corps.go), no opinion is neutrality.
func relCorpStanding(fromCorp, toCorp int64) float64 {
	if fromCorp == 0 || toCorp == 0 || fromCorp == toCorp {
		return 0
	}
	s := loadRelSnapshot()
	if s == nil {
		return 0
	}
	return s.corp[[2]int64{fromCorp, toCorp}]
}

// relWarHostile is War.Hostile with a guard for a future-dated war: Hostile's
// unsigned subtraction underflows when timeDeclared is after now and would read the
// war as hostile at once. The harness's fake clock can produce that. Every reader
// of a war goes through here.
func relWarHostile(w evedb.War, now uint64) bool {
	if w.TimeDeclared <= 0 || uint64(w.TimeDeclared) > now {
		return false
	}
	return w.Hostile(now)
}

// relPairKey is the unordered key a war is stored under.
func relPairKey(a, b int64) [2]int64 {
	if a > b {
		return [2]int64{b, a}
	}
	return [2]int64{a, b}
}

// relAtWar reports whether these two pilots' owners are in a war that is hostile
// now. Four corp and alliance combinations are tried, as the core's WarShotIsLegal
// does: a corp-level war between corporations in alliances is still a war.
func relAtWar(aCorp, aAlly, bCorp, bAlly int64) bool {
	s := loadRelSnapshot()
	if !relWarsUsable(s) || len(s.wars) == 0 {
		return false
	}
	if aAlly == 0 {
		aAlly = s.ally[aCorp]
	}
	if bAlly == 0 {
		bAlly = s.ally[bCorp]
	}
	// Same owner on both sides is not a war with itself, at either level.
	now := uint64(win32Now())
	for _, a := range [2]int64{aCorp, aAlly} {
		if a == 0 {
			continue
		}
		for _, b := range [2]int64{bCorp, bAlly} {
			if b == 0 || a == b {
				continue
			}
			for _, w := range s.wars[relPairKey(a, b)] {
				if relWarHostile(w, now) {
					return true
				}
			}
		}
	}
	return false
}

// relEnemySystem reports whether a corporation this one counts as an enemy works in
// sysID or keeps an address there. Addresses cover the enemy that is docked or
// logged off.
func relEnemySystem(corpID, sysID int64) bool {
	if corpID == 0 || sysID == 0 {
		return false
	}
	s := loadRelSnapshot()
	if s == nil {
		return false
	}
	return s.enemySys[corpID][sysID]
}

// relWarEnemySystem reports whether a corporation this one is at war with works in
// sysID or keeps an address there: the subset of relEnemySystem that is dangerous
// in highsec. Stale snapshots answer no, as relAtWar does.
func relWarEnemySystem(corpID, sysID int64) bool {
	if corpID == 0 || sysID == 0 {
		return false
	}
	s := loadRelSnapshot()
	if !relWarsUsable(s) {
		return false
	}
	return s.warSys[corpID][sysID]
}

// relWarEnemiesOf lists the corporations corpID is in a hostile war with,
// sorted. Alliance-level wars are already expanded onto member corporations.
func relWarEnemiesOf(corpID int64) []int64 {
	if corpID == 0 {
		return nil
	}
	s := loadRelSnapshot()
	if !relWarsUsable(s) {
		return nil
	}
	out := make([]int64, 0, len(s.warEnemies[corpID]))
	for id := range s.warEnemies[corpID] {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// relWarEnemySystemsOf lists the systems where a war enemy of corpID works or keeps
// an address: relWarEnemySystem's set, sorted. The Director of Security's hunt
// orders are drawn from it (corpstaff_dept.go).
func relWarEnemySystemsOf(corpID int64) []int64 {
	if corpID == 0 {
		return nil
	}
	s := loadRelSnapshot()
	if !relWarsUsable(s) {
		return nil
	}
	out := make([]int64, 0, len(s.warSys[corpID]))
	for id := range s.warSys[corpID] {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// relEnemySystemsOf lists every system where an enemy of corpID (at war or a
// declared rival) works or keeps an address: relEnemySystem's set, sorted. The fleet
// tier reads it for roams (gangtier.go) and filters to low and null itself.
func relEnemySystemsOf(corpID int64) []int64 {
	if corpID == 0 {
		return nil
	}
	s := loadRelSnapshot()
	if s == nil {
		return nil
	}
	out := make([]int64, 0, len(s.enemySys[corpID]))
	for id := range s.enemySys[corpID] {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// relFold folds the corporate layer into an opinion that already carries the pilot's
// own grudge. Both tiers (standingToPilot, gridStandingToPilot) end here so they
// never differ. Same corporation or alliance never folds; a declared standing folds
// with worseStanding; war folds as StandingDespised. See docs/comment-notes-b06.md#corp-relations.
func relFold(v float64, selfCorp, selfAlly, otherCorp, otherAlly int64) float64 {
	if selfCorp == 0 || otherCorp == 0 || selfCorp == otherCorp {
		return v
	}
	if selfAlly != 0 && selfAlly == otherAlly {
		return v
	}
	// A declaration counts on the client's colour scale (declaredCorpStanding): red folds
	// in, the wary bands fold nothing, and blue is applied last by relBlueFloor at each fold site.
	if d := declaredCorpStanding(relCorpStanding(selfCorp, otherCorp)); d < 0 {
		v = worseStanding(v, d)
	}
	if relAtWar(selfCorp, selfAlly, otherCorp, otherAlly) {
		v = worseStanding(v, StandingDespised)
	}
	return v
}

// relDeclaredBlue reports whether selfCorp's directors set otherCorp (or its
// alliance) at or above CorpBlueLine, "don't kill blues". A war between the two wins.
func relDeclaredBlue(selfCorp, selfAlly, otherCorp, otherAlly int64) bool {
	if selfCorp == 0 || otherCorp == 0 || selfCorp == otherCorp {
		return false
	}
	if !declaredBlue(relCorpStanding(selfCorp, otherCorp)) {
		return false
	}
	return !relAtWar(selfCorp, selfAlly, otherCorp, otherAlly)
}

// relBlueFloor lifts a finished fold to the blue line when the pair is
// declared blue, so stanceOf reads it friendly whatever else was folded.
func relBlueFloor(v float64, blue bool) float64 {
	if blue && v < CorpBlueLine {
		return CorpBlueLine
	}
	return v
}

// relMarkWorth is what a system where a declared enemy of this corporation works adds
// to a hunter's choice of ground (piracySite, roamSite): two prey's worth. Miners and
// haulers route around the enemy; fighters (residency.go) go to it.
const relMarkWorth = 2 * piracyPreyWorth

// relMarkFor is what sysID adds to this corporation's hunters' choice of ground:
// relMarkWorth where a declared enemy works, warMarkMul times that where a war enemy does.
func relMarkFor(corpID, sysID int64) float64 {
	if !relEnemySystem(corpID, sysID) {
		return 0
	}
	if relWarEnemySystem(corpID, sysID) {
		return relMarkWorth * warMarkMul()
	}
	return relMarkWorth
}

// relAllied reports whether two corporations are mutual allies: each holds the other
// at StandingDevoted or better. Both directions are required, since the perks gated
// (escort lending, fleet seats: gangescort.go, gangform.go) put one corporation's
// pilots under the other's command. The same corporation is kin, not allied.
func relAllied(a, b int64) bool {
	if a == 0 || b == 0 || a == b {
		return false
	}
	s := loadRelSnapshot()
	if s == nil {
		return false
	}
	return s.corp[[2]int64{a, b}] >= StandingDevoted && s.corp[[2]int64{b, a}] >= StandingDevoted
}

// relAlliesOf lists corpID's mutual allies, sorted so a caller's query text is
// stable. Walks the declared pairs, which is the whole of the snapshot's corp
// map and small (only declared pairs are present).
func relAlliesOf(corpID int64) []int64 {
	if corpID == 0 {
		return nil
	}
	s := loadRelSnapshot()
	if s == nil {
		return nil
	}
	var out []int64
	for k, v := range s.corp {
		if k[0] != corpID || v < StandingDevoted {
			continue
		}
		if s.corp[[2]int64{k[1], corpID}] >= StandingDevoted {
			out = append(out, k[1])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// relHasEnemies reports whether this corporation is party to any hostile relation:
// the cheap gate before per-system work.
func relHasEnemies(corpID int64) bool {
	if corpID == 0 {
		return false
	}
	s := loadRelSnapshot()
	if s == nil {
		return false
	}
	return len(s.enemies[corpID]) > 0
}

// relCorpAtWar reports whether this corporation is in any hostile war right
// now, at its own level or its alliance's. Used by the combat opportunity term:
// a corporation at war pushes its wing out, whoever the war is with.
func relCorpAtWar(corpID int64) bool {
	if corpID == 0 {
		return false
	}
	s := loadRelSnapshot()
	if !relWarsUsable(s) || len(s.wars) == 0 {
		return false
	}
	ally := s.ally[corpID]
	now := uint64(win32Now())
	for key, list := range s.wars {
		if key[0] != corpID && key[1] != corpID &&
			(ally == 0 || (key[0] != ally && key[1] != ally)) {
			continue
		}
		for _, w := range list {
			if relWarHostile(w, now) {
				return true
			}
		}
	}
	return false
}

// ── the build ───────────────────────────────────────────────────────────────

// relBuildTimeout bounds one rebuild. Generous: it is four small queries and a
// map walk, and a rebuild that runs long must fail rather than pile up behind
// the next one.
const relBuildTimeout = 30 * time.Second

// buildRelSnapshot reads the shard's declarations and wars and resolves them
// into the shape the readers want. It never installs a partial snapshot: a
// failed query leaves the previous one in place, because a corporation that is
// hostile today is not neutral because a SELECT timed out.
func (m *Mod) buildRelSnapshot(ctx context.Context) (*relSnapshot, error) {
	s := &relSnapshot{
		at:       clock.Unix(),
		corp:     map[[2]int64]float64{},
		ally:     map[int64]int64{},
		wars:     map[[2]int64][]evedb.War{},
		enemySys: map[int64]map[int64]bool{},
		enemies:  map[int64]map[int64]float64{},

		warEnemies: map[int64]map[int64]bool{},
		warSys:     map[int64]map[int64]bool{},
	}

	if err := m.relLoadAlliances(ctx, s); err != nil {
		return nil, err
	}
	decl, err := m.relLoadDeclarations(ctx)
	if err != nil {
		return nil, err
	}
	if err := m.relLoadWars(ctx, s); err != nil {
		return nil, err
	}

	relResolveDeclarations(s, decl, relationsMutual())
	relSeedWarEnemies(s)

	if err := m.relLoadEnemySystems(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// relLoadAlliances fills the corporationID to allianceID map.
func (m *Mod) relLoadAlliances(ctx context.Context, s *relSnapshot) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, allianceID FROM corporation
		 WHERE allianceID IS NOT NULL AND allianceID > 0`)
	if err != nil {
		return fmt.Errorf("relLoadAlliances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var corpID, allyID int64
		if err := rows.Scan(&corpID, &allyID); err != nil {
			return fmt.Errorf("relLoadAlliances scan: %w", err)
		}
		s.ally[corpID] = allyID
	}
	return rows.Err()
}

// relLoadDeclarations reads every declaration, keyed by (declaring owner, target
// owner), either of which may be a corporation or an alliance; relResolveDeclarations
// turns them into corporation pairs. The corporate arm is the core's crpStandings table.
func (m *Mod) relLoadDeclarations(ctx context.Context) (map[[2]int64]float64, error) {
	out := map[[2]int64]float64{}

	rows, err := m.db.QueryContext(ctx, `
		SELECT s.corporationID, s.toID, s.standing
		  FROM crpStandings s
		 WHERE s.standing <> 0`)
	if err != nil {
		return nil, fmt.Errorf("relLoadDeclarations corps: %w", err)
	}
	for rows.Next() {
		var from, to int64
		var v float64
		if err := rows.Scan(&from, &to, &v); err != nil {
			rows.Close()
			return nil, fmt.Errorf("relLoadDeclarations corps scan: %w", err)
		}
		if from != 0 && to != 0 && from != to {
			out[[2]int64{from, to}] = clampStanding(v)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relLoadDeclarations corps: %w", err)
	}

	// The alliance layer. A missing table is not an error, as in playerCorpOutlook:
	// a shard with no alliances has no alliance politics.
	arows, err := m.db.QueryContext(ctx,
		`SELECT allianceID, toID, relationship FROM allianceRelationships`)
	if err != nil {
		return out, nil
	}
	defer arows.Close()
	for arows.Next() {
		var from, to, rel int64
		if err := arows.Scan(&from, &to, &rel); err != nil {
			return nil, fmt.Errorf("relLoadDeclarations alliances scan: %w", err)
		}
		v := allianceRelStanding(rel)
		if v == 0 || from == 0 || to == 0 || from == to {
			continue
		}
		// A corporation's own position outranks its alliance's: the alliance row must not
		// overwrite a corp row, and this guard stops an alliance ID colliding with a corp ID.
		key := [2]int64{from, to}
		if _, seen := out[key]; !seen {
			out[key] = v
		}
	}
	return out, arows.Err()
}

// relLoadWars reads every active war into the unordered-pair index.
func (m *Mod) relLoadWars(ctx context.Context, s *relSnapshot) error {
	wars, err := evedb.ListWars(ctx, m.db)
	if err != nil {
		// A shard whose wars table cannot be read has no wars here; failing the snapshot
		// would take the corp-standings half down with it.
		rlogf("rolebots: relations: read wars: %v (no wars this pass)", err)
		return nil
	}
	for _, v := range wars {
		if !v.Active || v.DeclaredByID == 0 || v.AgainstID == 0 {
			continue
		}
		key := relPairKey(v.DeclaredByID, v.AgainstID)
		s.wars[key] = append(s.wars[key], v.War)
	}
	// The relations ledger hears war changes here, human and bot alike
	// (corprelations_hooks.go); it needs the alliance map loaded first.
	m.relDiffWars(ctx, s, wars)
	return nil
}

// relResolveDeclarations turns owner-level declarations into corporation pairs.
// Four lookups per pair in precedence order: corp about corp, corp about the other's
// alliance, this corp's alliance about the other corp, alliance about alliance.
// mutual is read once so the standing fold and the enemy-system index agree.
func relResolveDeclarations(s *relSnapshot, decl map[[2]int64]float64, mutual bool) {
	if len(decl) == 0 {
		return
	}
	// alliance to member corporations, to expand an alliance-level declaration onto
	// the corporations it binds.
	members := map[int64][]int64{}
	for corpID, allyID := range s.ally {
		members[allyID] = append(members[allyID], corpID)
	}
	expand := func(ownerID int64) []int64 {
		if list := members[ownerID]; len(list) > 0 {
			return list
		}
		return []int64{ownerID}
	}

	// resolve is the precedence ladder for one ordered corporation pair.
	resolve := func(from, to int64) (float64, bool) {
		fa, ta := s.ally[from], s.ally[to]
		for _, k := range [][2]int64{
			{from, to},
			{from, ta},
			{fa, to},
			{fa, ta},
		} {
			if k[0] == 0 || k[1] == 0 || k[0] == k[1] {
				continue
			}
			if v, ok := decl[k]; ok {
				return v, true
			}
		}
		return 0, false
	}

	// Candidate pairs: every corporation either side of every declaration.
	type pair struct{ from, to int64 }
	cand := map[pair]bool{}
	for k := range decl {
		for _, f := range expand(k[0]) {
			for _, t := range expand(k[1]) {
				if f != 0 && t != 0 && f != t {
					cand[pair{f, t}] = true
					// The reverse is a candidate too: a mutual fold needs both directions resolved.
					cand[pair{t, f}] = true
				}
			}
		}
	}

	raw := map[[2]int64]float64{}
	for p := range cand {
		if v, ok := resolve(p.from, p.to); ok {
			raw[[2]int64{p.from, p.to}] = v
		}
	}

	for p := range cand {
		key := [2]int64{p.from, p.to}
		v, ok := raw[key]
		if mutual {
			// A red reverse declaration floors this one at CorpRedLine; it is not mirrored, so
			// the other side holds no grudge it never declared. Positive ones are floored too.
			if back, hasBack := raw[[2]int64{p.to, p.from}]; hasBack && back <= CorpRedLine {
				if !ok || v > CorpRedLine {
					v, ok = CorpRedLine, true
				}
			}
		}
		if !ok || v == 0 {
			continue
		}
		s.corp[key] = v
		if v <= StandingDisliked {
			if s.enemies[p.from] == nil {
				s.enemies[p.from] = map[int64]float64{}
			}
			s.enemies[p.from][p.to] = v
		}
	}
}

// relSeedWarEnemies adds both sides of every hostile war to the enemy sets: a war is
// an enemy relation whether or not a standing was set, and the only one hostile in
// policed space. Hostility is tested at build time, unlike relAtWar which re-tests
// every read: a firing decision must be exact, an avoidance preference may lag.
func relSeedWarEnemies(s *relSnapshot) {
	if len(s.wars) == 0 {
		return
	}
	if s.warEnemies == nil {
		s.warEnemies = map[int64]map[int64]bool{}
	}
	members := map[int64][]int64{}
	for corpID, allyID := range s.ally {
		members[allyID] = append(members[allyID], corpID)
	}
	expand := func(ownerID int64) []int64 {
		if list := members[ownerID]; len(list) > 0 {
			return list
		}
		return []int64{ownerID}
	}
	now := uint64(win32Now())
	for key, list := range s.wars {
		hostile := false
		for _, w := range list {
			if relWarHostile(w, now) {
				hostile = true
				break
			}
		}
		if !hostile {
			continue
		}
		a, b := expand(key[0]), expand(key[1])
		mark := func(from, to []int64) {
			for _, f := range from {
				for _, t := range to {
					if f == 0 || t == 0 || f == t {
						continue
					}
					if s.enemies[f] == nil {
						s.enemies[f] = map[int64]float64{}
					}
					if s.warEnemies[f] == nil {
						s.warEnemies[f] = map[int64]bool{}
					}
					s.warEnemies[f][t] = true
					// A war does not overwrite a declared standing that is
					// already worse; it floors the pair at despised.
					if cur, ok := s.enemies[f][t]; !ok || cur > StandingDespised {
						s.enemies[f][t] = StandingDespised
					}
				}
			}
		}
		mark(a, b)
		mark(b, a)
	}
}

// relLoadEnemySystems fills, for each corporation with enemies, the systems those
// enemies work in or keep an address in. Both queries are bounded by the enemy set,
// so a shard with no declarations does no I/O.
func (m *Mod) relLoadEnemySystems(ctx context.Context, s *relSnapshot) error {
	if len(s.enemies) == 0 {
		return nil
	}
	// Every corporation that is somebody's enemy, deduped and sorted so the
	// query text is stable (and so a test can assert on it).
	seen := map[int64]bool{}
	for _, set := range s.enemies {
		for corpID := range set {
			seen[corpID] = true
		}
	}
	targets := make([]int64, 0, len(seen))
	for corpID := range seen {
		targets = append(targets, corpID)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i] < targets[j] })

	// corporationID to the systems it is in or anchors.
	where := map[int64]map[int64]bool{}
	add := func(corpID, sysID int64) {
		if corpID == 0 || sysID == 0 {
			return
		}
		if where[corpID] == nil {
			where[corpID] = map[int64]bool{}
		}
		where[corpID][sysID] = true
	}

	// Presence: pilots of those corporations in space now, humans as well as rolebots.
	prows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, COALESCE(solarSystemID, 0)
		  FROM character_
		 WHERE corporationID IN (`+placeholders(len(targets))+`)
		   AND COALESCE(stationID, 0) = 0
		   AND COALESCE(solarSystemID, 0) <> 0`, int64Args(targets)...)
	if err != nil {
		return fmt.Errorf("relLoadEnemySystems presence: %w", err)
	}
	for prows.Next() {
		var corpID, sysID int64
		if err := prows.Scan(&corpID, &sysID); err != nil {
			prows.Close()
			return fmt.Errorf("relLoadEnemySystems presence scan: %w", err)
		}
		add(corpID, sysID)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return fmt.Errorf("relLoadEnemySystems presence: %w", err)
	}

	// Addresses: the three arms corpAnchorStations (corphome.go) walks for one
	// corporation (stations owned, offices rented, headquarters), asked for the whole enemy set.
	in := placeholders(len(targets))
	args := append(append(append([]any{}, int64Args(targets)...), int64Args(targets)...), int64Args(targets)...)
	arows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, solarSystemID FROM staStations
		 WHERE corporationID IN (`+in+`)
		UNION
		SELECT o.corporationID, s.solarSystemID
		  FROM crpoffices o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID IN (`+in+`)
		UNION
		SELECT c.corporationID, s.solarSystemID
		  FROM corporation c
		  JOIN staStations s ON s.stationID = c.stationID
		 WHERE c.corporationID IN (`+in+`)`, args...)
	if err != nil {
		return fmt.Errorf("relLoadEnemySystems anchors: %w", err)
	}
	defer arows.Close()
	for arows.Next() {
		var corpID, sysID int64
		if err := arows.Scan(&corpID, &sysID); err != nil {
			return fmt.Errorf("relLoadEnemySystems anchors scan: %w", err)
		}
		add(corpID, sysID)
	}
	if err := arows.Err(); err != nil {
		return fmt.Errorf("relLoadEnemySystems anchors: %w", err)
	}

	for viewer, set := range s.enemies {
		for enemy := range set {
			for sysID := range where[enemy] {
				if s.enemySys[viewer] == nil {
					s.enemySys[viewer] = map[int64]bool{}
				}
				s.enemySys[viewer][sysID] = true
				if s.warEnemies[viewer][enemy] {
					if s.warSys == nil {
						s.warSys = map[int64]map[int64]bool{}
					}
					if s.warSys[viewer] == nil {
						s.warSys[viewer] = map[int64]bool{}
					}
					s.warSys[viewer][sysID] = true
				}
			}
		}
	}
	return nil
}

// ── the periodic ────────────────────────────────────────────────────────────

// startRelationsSweep registers the rebuild and performs the first one inline, so the
// first sim pass already has politics. It is gated on relationsEnabled() but not on
// the master switch: a snapshot costs four small queries whether or not anybody flies.
func (m *Mod) startRelationsSweep() {
	m.refreshRelations(context.Background())
	// The period is a dial, and registerPeriodic takes a fixed tick count, so
	// the callback checks the clock itself rather than being re-registered.
	m.registerPeriodic("rolebot-relations-sweep", 30, func(uint64) bool {
		if !relationsEnabled() {
			return true
		}
		if last := relSnapshotAt(); last != 0 && clock.Unix()-last < int64(relRefreshSec()) {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), relBuildTimeout)
		defer cancel()
		m.refreshRelations(ctx)
		return true
	})
}

// refreshRelations rebuilds and installs the snapshot. A failure is logged and keeps
// the previous snapshot (see buildRelSnapshot).
func (m *Mod) refreshRelations(ctx context.Context) {
	if !relationsEnabled() {
		return
	}
	s, err := m.buildRelSnapshot(ctx)
	if err != nil {
		rlogf("rolebots: relations snapshot: %v (keeping the previous one)", err)
		return
	}
	relCurrent.Store(s)
}

// relStats is the panel's summary of the live snapshot.
type relStats struct {
	At         int64
	Pairs      int
	Wars       int
	HostileNow int
	Viewers    int
	EnemySys   int
}

func relSnapshotStats() relStats {
	s := relCurrent.Load()
	if s == nil {
		return relStats{}
	}
	out := relStats{At: s.at, Pairs: len(s.corp), Viewers: len(s.enemies)}
	now := uint64(win32Now())
	for _, list := range s.wars {
		out.Wars += len(list)
		for _, w := range list {
			if relWarHostile(w, now) {
				out.HostileNow++
			}
		}
	}
	for _, set := range s.enemySys {
		out.EnemySys += len(set)
	}
	return out
}
