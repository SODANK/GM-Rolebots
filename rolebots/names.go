package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
)

// Name generation: first x last, drawn from the bloodline's own pool
// (namedata.go) and checked against the universe before use. Constraints:
//
//  1. character_.characterName is UNIQUE, so the name is resolved before
//     anything is written and reserved in memory for the run.
//  2. Names are shared cluster-wide, so the check covers every character and
//     eveStaticOwners; a bot named like an agent resolves to the wrong owner
//     in client lookups.
//  3. Pools are finite: after enough failed draws the generator falls back to
//     a suffixed variant instead of looping.

// ── the bloodlines that are not one culture, one pool ──────────────────────

// joveModifierBloodline is the only Jove bloodline a pilot can be generated
// as: the client has face art for exactly one Jove combination (bloodline 10,
// gender 1). Static (9) is not generatable and Modifier is male only.
const joveModifierBloodline int64 = 10

// maleOnlyBloodlines can only be generated as men: the client has a mesh for
// no other gender, so buildPlan overrides the female share rather than
// producing a pilot with no face (genplan.go).
var maleOnlyBloodlines = map[int64]bool{joveModifierBloodline: true}

// borrowedNamePools are the bloodlines named from every other bloodline's lists
// pooled together. The Jove have no naming register of their own, and a
// made-up one would be a made-up culture.
var borrowedNamePools = map[int64]bool{joveModifierBloodline: true}

// mononymBloodlines carry one name, not two. The borrowed pool takes given
// names only and the minter never pairs one with a surname, because the
// Modifier caste has no family line.
var mononymBloodlines = map[int64]bool{joveModifierBloodline: true}

// offByDefaultBloodlines load at 0% on the seed form and are left out of the
// even split. They are offered, not included: an operator picks a Jove for a
// handful of pilots.
var offByDefaultBloodlines = map[int64]bool{joveModifierBloodline: true}

// borrowedPool is the union of every written pool's given names (male), and
// of their surnames when the borrower uses surnames at all, in ascending
// bloodline order so the list is the same on every boot. Female is left
// empty: every bloodline that borrows is male-only.
func borrowedPool(withSurnames bool) parsedPool {
	keys := make([]int64, 0, len(namePools))
	for bl := range namePools {
		keys = append(keys, bl)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	var out parsedPool
	seenGiven, seenFamily := map[string]bool{}, map[string]bool{}
	for _, bl := range keys {
		for _, n := range parsePoolText(namePools[bl].Male) {
			if !seenGiven[n] {
				seenGiven[n] = true
				out.male = append(out.male, n)
			}
		}
		if !withSurnames {
			continue
		}
		for _, n := range parsePoolText(namePools[bl].Last) {
			if !seenFamily[n] {
				seenFamily[n] = true
				out.last = append(out.last, n)
			}
		}
	}
	return out
}

// parsedPool is one bloodline's pools after parsing: deduplicated, with blank
// lines and comments dropped.
type parsedPool struct {
	male   []string
	female []string
	last   []string
}

var (
	poolsOnce   sync.Once
	parsedPools map[int64]parsedPool
)

// parsePoolText splits a raw pool constant into names. Blank lines are
// skipped and a line whose first non-space character is '#' is a comment,
// so the lists can be annotated in place without a second format to learn.
func parsePoolText(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		n := strings.TrimSpace(line)
		if n == "" || strings.HasPrefix(n, "#") {
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func pools() map[int64]parsedPool {
	poolsOnce.Do(func() {
		parsedPools = make(map[int64]parsedPool, len(namePools))
		for bl, raw := range namePools {
			parsedPools[bl] = parsedPool{
				male:   parsePoolText(raw.Male),
				female: parsePoolText(raw.Female),
				last:   parsePoolText(raw.Last),
			}
		}
		// The borrowers are built from the written pools above, so they are
		// filled in after all of them are parsed and never before.
		for bl := range borrowedNamePools {
			parsedPools[bl] = borrowedPool(!mononymBloodlines[bl])
		}
	})
	return parsedPools
}

// nameableBloodlines lists the bloodlines a pilot can be generated as, in
// ascending ID order: the twelve empire bloodlines plus Jove Modifier (male
// only, borrowed names, 0% on the form). Jove Static (9) has no working face
// and is absent.
func nameableBloodlines() []int64 {
	p := pools()
	var out []int64
	for _, bl := range []int64{1, 2, 3, 4, 5, 6, 7, 8, joveModifierBloodline, 11, 12, 13, 14} {
		pp, ok := p[bl]
		if !ok || (len(pp.male) == 0 && len(pp.female) == 0) {
			continue
		}
		if len(pp.last) > 0 || mononymBloodlines[bl] {
			out = append(out, bl)
		}
	}
	return out
}

// defaultMixBloodlines is nameableBloodlines minus the ones an unattended run
// must never reach for. It is what a fallback draws from, so a mix that names
// nothing usable lands on an empire pilot rather than silently on a Jove.
func defaultMixBloodlines() []int64 {
	var out []int64
	for _, bl := range nameableBloodlines() {
		if offByDefaultBloodlines[bl] {
			continue
		}
		out = append(out, bl)
	}
	return out
}

// nameMinter draws unique names for one generation run. It holds the names
// already taken so a batch cannot collide with itself; the database is
// consulted per candidate, one indexed lookup per bot.
type nameMinter struct {
	db    *sql.DB
	rng   *rand.Rand
	taken map[string]bool
}

func newNameMinter(db *sql.DB, rng *rand.Rand) *nameMinter {
	return &nameMinter{db: db, rng: rng, taken: map[string]bool{}}
}

// free reports whether a name is available anywhere in the universe.
func (nm *nameMinter) free(ctx context.Context, name string) (bool, error) {
	if nm.taken[strings.ToLower(name)] {
		return false, nil
	}
	var one int64
	err := nm.db.QueryRowContext(ctx,
		`SELECT 1 FROM character_ WHERE characterName = ? LIMIT 1`, name).Scan(&one)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, fmt.Errorf("name check: %w", err)
	}
	// eveStaticOwners carries agents, NPC corporations and factions. A pilot with
	// one of those names would make owner lookups in the client ambiguous.
	err = nm.db.QueryRowContext(ctx,
		`SELECT 1 FROM eveStaticOwners WHERE ownerName = ? LIMIT 1`, name).Scan(&one)
	if err == nil {
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, fmt.Errorf("name check owners: %w", err)
	}
	return true, nil
}

// maxNameDraws bounds the search before the minter starts disambiguating. It
// is well below the pool product, so reaching it means the pool is exhausted
// for this bloodline and gender.
const maxNameDraws = 200

// Mint returns an unused "First Last" for a bloodline and gender (0/1).
// The gender split matters: a Ni-Kunni woman drawn from the male list is
// exactly the tell that gives a generated population away.
func (nm *nameMinter) Mint(ctx context.Context, bloodlineID, gender int64) (string, error) {
	pp, ok := pools()[bloodlineID]
	if !ok {
		return "", fmt.Errorf("no name pool for bloodline %d", bloodlineID)
	}
	firsts := pp.male
	if gender == 0 {
		firsts = pp.female
	}
	if len(firsts) == 0 {
		return "", fmt.Errorf("name pool for bloodline %d is empty for gender %d", bloodlineID, gender)
	}
	if len(pp.last) == 0 {
		// A bloodline with no surnames is a mononym one, not a broken one.
		return nm.mintMononym(ctx, bloodlineID, firsts)
	}

	for i := 0; i < maxNameDraws; i++ {
		given := firsts[nm.rng.Intn(len(firsts))]
		family := pp.last[nm.rng.Intn(len(pp.last))]
		// Pools may share a word between given and family lists (Jin-Mei has twelve).
		// Drawing both from one word reads as a bug ("Wei Wei"), so redraw.
		if strings.EqualFold(given, family) {
			continue
		}
		name := given + " " + family
		ok, err := nm.free(ctx, name)
		if err != nil {
			return "", err
		}
		if ok {
			nm.taken[strings.ToLower(name)] = true
			return name, nil
		}
	}

	// The pool is spent: fall back to a middle initial, which stays in register.
	// characterName is varchar(43); a name plus " X." cannot overflow it.
	for _, initial := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		for i := 0; i < 40; i++ {
			given := firsts[nm.rng.Intn(len(firsts))]
			family := pp.last[nm.rng.Intn(len(pp.last))]
			if strings.EqualFold(given, family) {
				continue
			}
			name := fmt.Sprintf("%s %c %s", given, initial, family)
			ok, err := nm.free(ctx, name)
			if err != nil {
				return "", err
			}
			if ok {
				nm.taken[strings.ToLower(name)] = true
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("name pool for bloodline %d gender %d is exhausted — add names to namedata.go", bloodlineID, gender)
}

// mintMononym draws a single unused name. With no cross product to fall back
// on, the fallback is a numbered designation, which keeps the name one word.
func (nm *nameMinter) mintMononym(ctx context.Context, bloodlineID int64, firsts []string) (string, error) {
	for i := 0; i < maxNameDraws; i++ {
		name := firsts[nm.rng.Intn(len(firsts))]
		ok, err := nm.free(ctx, name)
		if err != nil {
			return "", err
		}
		if ok {
			nm.taken[strings.ToLower(name)] = true
			return name, nil
		}
	}
	for n := 2; n <= 999; n++ {
		for i := 0; i < 40; i++ {
			name := fmt.Sprintf("%s-%d", firsts[nm.rng.Intn(len(firsts))], n)
			ok, err := nm.free(ctx, name)
			if err != nil {
				return "", err
			}
			if ok {
				nm.taken[strings.ToLower(name)] = true
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("name pool for bloodline %d is exhausted — add names to namedata.go", bloodlineID)
}

// poolStats reports pool sizes for the seed page, so an operator asking for
// 5000 bots of one bloodline can see the ceiling before pressing Accept
// rather than after.
type poolStat struct {
	BloodlineID   int64
	BloodlineName string
	RaceID        int64
	Male, Female  int
	Last          int
	// MaleOnly is a bloodline the client can only draw as a man; the form
	// shows it and the generator enforces it whatever the female share says.
	MaleOnly bool
	// Borrowed names this bloodline out of every other pool rather than one
	// of its own.
	Borrowed bool
	// OffByDefault loads the form at 0% and sits out the even split.
	OffByDefault bool
	// Mononym carries one name and no surname, so Combos is the given-name
	// count itself rather than a cross product.
	Mononym bool
	// Combos is the distinct first x last count across both genders: the headroom
	// figure, before first==last redraws. For a mononym bloodline it is the number
	// of given names.
	Combos int
}

func namePoolStats(ctx context.Context, db *sql.DB) ([]poolStat, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT bloodlineID, bloodlineName, raceID FROM chrBloodlines ORDER BY raceID, bloodlineID`)
	if err != nil {
		return nil, fmt.Errorf("namePoolStats: %w", err)
	}
	defer rows.Close()
	p := pools()
	var out []poolStat
	for rows.Next() {
		var s poolStat
		if err := rows.Scan(&s.BloodlineID, &s.BloodlineName, &s.RaceID); err != nil {
			return nil, fmt.Errorf("namePoolStats scan: %w", err)
		}
		pp, ok := p[s.BloodlineID]
		if !ok {
			continue // no pool: Jove Static, and nothing else
		}
		s.MaleOnly = maleOnlyBloodlines[s.BloodlineID]
		s.Borrowed = borrowedNamePools[s.BloodlineID]
		s.OffByDefault = offByDefaultBloodlines[s.BloodlineID]
		s.Mononym = mononymBloodlines[s.BloodlineID]
		s.Male, s.Female, s.Last = len(pp.male), len(pp.female), len(pp.last)
		s.Combos = (s.Male + s.Female) * s.Last
		if s.Mononym {
			s.Combos = s.Male + s.Female
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
