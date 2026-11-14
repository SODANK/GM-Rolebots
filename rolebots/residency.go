package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Residency: the region a pilot lives in, and how far its economic eye reaches
// out of it. Travel is never scoped (reach.go); the search is, meaning the
// regions whose markets and belts a pilot considers. shopLadder widens home,
// adjacent, universe, so the scope is tried first and never the only option.
// Relocation (moveout.go, corphome.go, clone.go, the router) stays unscoped.
// See docs/comment-notes-b16.md#residency.

// scopeBand is how far out of its home region a pilot's economic eye goes.
type scopeBand int

const (
	// bandHome is the home region alone. It carries no operator-preferred
	// regions; the stored list is gone (see homeRegionSet).
	bandHome scopeBand = iota
	// bandAdjacent adds every region sharing a gate with home. Every pilot opens
	// here.
	bandAdjacent
	// bandNear2 adds the regions one further hop out. Nobody opens here and the
	// ladder climbs through it; it exists for blueprint purchases (industry.go),
	// since a pilot in barren nullsec sees no blueprint orders at adjacent.
	bandNear2
	// bandUniverse is no scope at all, the last rung of the ladder.
	bandUniverse
)

func (b scopeBand) String() string {
	switch b {
	case bandHome:
		return "home region"
	case bandAdjacent:
		return "home and adjacent regions"
	case bandNear2:
		return "home and regions within two"
	}
	return "anywhere"
}

// residencyOn is the scope switch, always on in a running server: no setting,
// config key or panel control. An atomic only so the harness can flip it
// (residency_test.go).
var residencyOn atomic.Bool

// Residency reports whether economic scoping is switched on. True in every
// running server; only the harness makes it answer otherwise.
func Residency() bool { return residencyOn.Load() }

// ── the home loop ───────────────────────────────────────────────────────────
//
// The home loop is the pilot's rhythm: it returns to its home station between
// activities, shops there (homeshop.go), then decides what to do next. It also
// caps the buy side of shopLadder at the adjacent band. Always on: no setting,
// config key or panel control.

// homeLoopOn is the loop switch, held in memory like residencyOn since it is
// read on every planner pass of every pilot. The harness flips it to exercise
// the plan-where-you-stand path.
var homeLoopOn atomic.Bool

// init switches both on at process start: a zero atomic.Bool is false, and
// nothing in a running server may leave them there.
func init() {
	residencyOn.Store(true)
	homeLoopOn.Store(true)
}

// HomeLoop reports whether the return-home-and-shop loop is switched on. True
// in every running server; only the harness makes it answer otherwise.
func HomeLoop() bool { return homeLoopOn.Load() }

// ── region adjacency ────────────────────────────────────────────────────────

// regionAdj is region to the regions it shares a gate with. It is derived from
// the system graph in memory (one pass over about 13k edges, no query), so it
// also works for the harness, which hands the graph over ready-made (uniOverride).
func (u *universe) regionAdj() map[int64]map[int64]bool {
	u.radjOnce.Do(func() {
		adj := map[int64]map[int64]bool{}
		for from, nbs := range u.adj {
			fr := u.meta[from].RegionID
			if fr == 0 {
				continue
			}
			for _, to := range nbs {
				tr := u.meta[to].RegionID
				if tr == 0 || tr == fr {
					continue
				}
				if adj[fr] == nil {
					adj[fr] = map[int64]bool{}
				}
				adj[fr][tr] = true
			}
		}
		u.radj = adj
	})
	return u.radj
}

// ── a pilot's home region ───────────────────────────────────────────────────

var (
	homeRegionMu    sync.RWMutex
	homeRegionCache = map[int64]int64{} // charID → regionID
)

// forgetHomeRegion drops a cached home. Called from setHomeStation, the single
// funnel for a home move (clone.go), so the cache cannot outlive the fact.
func forgetHomeRegion(charID int64) {
	homeRegionMu.Lock()
	delete(homeRegionCache, charID)
	homeRegionMu.Unlock()
}

// homeRegionFor is the region of the pilot's home station (its medical clone
// station, homeStationFor in pod.go). It falls back to the region the pilot is
// standing in when the home row is missing: a zero would scope the pilot to an
// empty universe.
func (m *Mod) homeRegionFor(ctx context.Context, b *botRun) int64 {
	if region := m.homeRegionByChar(ctx, b.CharID); region != 0 {
		return region
	}
	// Standing-in region, uncached: a pilot in transit gives no stable answer.
	if u, err := m.universe(ctx); err == nil {
		return u.meta[b.SystemID].RegionID
	}
	return 0
}

// homeRegionByChar is homeRegionFor for callers with no run, such as fit
// generation, which plans wherever the pilot stands. An unknown home answers 0,
// never a guess.
func (m *Mod) homeRegionByChar(ctx context.Context, charID int64) int64 {
	homeRegionMu.RLock()
	id, ok := homeRegionCache[charID]
	homeRegionMu.RUnlock()
	if ok && id != 0 {
		return id
	}

	var region int64
	if st := m.homeStationFor(ctx, charID); st != 0 {
		region = m.stationRegionOf(ctx, st)
	}
	if region == 0 {
		return 0
	}
	homeRegionMu.Lock()
	homeRegionCache[charID] = region
	homeRegionMu.Unlock()
	return region
}

// homeRegionSet is a pilot's preferred region: exactly one, the region it lives
// in. It is derived because a list stored at birth never followed a corp join or
// a home move, and fought corphome.go. Nil when the home is unknown, which
// consumers read as no preference.
func (m *Mod) homeRegionSet(ctx context.Context, b *botRun) map[int64]bool {
	home := m.homeRegionFor(ctx, b)
	if home == 0 {
		return nil
	}
	return map[int64]bool{home: true}
}

// ── bands and scopes ────────────────────────────────────────────────────────

// defaultBand is how far this pilot's economy reaches when nothing widens it:
// everyone opens at the adjacent band. Keeping buyers from flattening regional
// spreads is shopLadder's buy cap, not a narrower horizon.
func defaultBand(b *botRun) scopeBand { return defaultBandFor(b.Tune) }

// defaultBandFor is the same decision from the traits alone, so the panel can
// show a pilot's scope without hydrating a run. The Tuning argument is unused.
func defaultBandFor(Tuning) scopeBand { return bandAdjacent }

// regionScope is the set of regions a pilot's economy may consider, or nil for
// no scope. Every consumer tests len(scope) == 0, so an unscoped pilot pays nothing.
func (m *Mod) regionScope(ctx context.Context, b *botRun, band scopeBand) map[int64]bool {
	return m.scopeRegions(ctx, m.homeRegionFor(ctx, b), band)
}

// scopeRegions is that set built from the parts, so the panel can show an
// operator a pilot's scope without hydrating a sim run for it.
func (m *Mod) scopeRegions(ctx context.Context, home int64, band scopeBand) map[int64]bool {
	if !Residency() || band == bandUniverse {
		return nil
	}
	if home == 0 {
		return nil // unknown home is not a reason to fence a pilot in
	}
	scope := map[int64]bool{home: true}
	if band >= bandAdjacent {
		u, err := m.universe(ctx)
		if err == nil {
			adj := u.regionAdj()
			// hop widens the scope by one gate. The ring is copied first: ranging
			// over the map being added to would flood-fill.
			hop := func() {
				ring := make([]int64, 0, len(scope))
				for r := range scope {
					ring = append(ring, r)
				}
				for _, r := range ring {
					for n := range adj[r] {
						scope[n] = true
					}
				}
			}
			hop()
			if band >= bandNear2 {
				hop()
			}
		}
	}
	if len(scope) == 0 {
		return nil
	}
	return scope
}

// scopeSummary is the pilot page's readout: which band this pilot's economy
// runs at and the regions it works out to, named. The band label is empty when
// residency is off.
func (m *Mod) scopeSummary(ctx context.Context, prof *Profile, tn Tuning, homeStationID int64) (band, regions string) {
	if !Residency() {
		return "", ""
	}
	var home int64
	if homeStationID != 0 {
		_ = m.db.QueryRowContext(ctx,
			`SELECT regionID FROM staStations WHERE stationID = ?`, homeStationID).Scan(&home)
	}
	scope := m.scopeRegions(ctx, home, defaultBandFor(tn))
	if len(scope) == 0 {
		return "", ""
	}
	names := make([]string, 0, len(scope))
	for _, id := range regionIDs(scope) {
		if n := m.nameOfRegion(ctx, id); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return defaultBandFor(tn).String(), strings.Join(names, ", ")
}

// economicRules are the travel rules a search runs under: rulesForBot plus the
// residency scope. Never hand them to route() or a relocation decision:
// movement is not scoped, and a pilot that cannot look outside its region
// cannot choose to move house.
func (m *Mod) economicRules(ctx context.Context, b *botRun, band scopeBand) travelRules {
	r := rulesForBot(b)
	// Rival-held systems cost extra to fly through (corprel.go, router.go
	// hopCost), except for a pilot under arms (bounty.go, piracy.go).
	if b != nil && (b.State == nil || (b.State.Activity != ActCombat &&
		b.State.Activity != ActSecurity && b.State.Activity != ActPiracy)) {
		if relHasEnemies(b.CorpID) {
			r.EnemyCorp = b.CorpID
			r.EnemyJumps = relEnemyHopJumps()
		}
	}
	if !Residency() || band == bandUniverse {
		// Short-circuited before homeRegionFor, a query on a cold cache: this runs
		// on every planner stage of every pilot.
		r.Band = bandUniverse
		return r
	}
	r.Home = m.homeRegionFor(ctx, b)
	r.Regions = m.scopeRegions(ctx, r.Home, band)
	r.Band = band
	return r
}

// widen returns the same rules one band further out, and whether there was
// anywhere further to go. It is the ladder's single step, so the escalation
// order lives in one place instead of in every planner that climbs it.
func (m *Mod) widen(ctx context.Context, b *botRun, r travelRules) (travelRules, bool) {
	if r.Band >= bandUniverse || !Residency() {
		return r, false
	}
	next := r.Band + 1
	out := r
	out.Regions = m.regionScope(ctx, b, next)
	out.Band = next
	// Widening that adds no region is not a rung: skip it so the same scan does
	// not run twice.
	if next < bandUniverse && len(out.Regions) == len(r.Regions) {
		return m.widen(ctx, b, out)
	}
	return out, true
}

// ── remembering a band that came up empty ───────────────────────────────────
//
// A band that produced nothing usable is remembered briefly and the next climb
// starts above it: a pilot re-plans the same trip every pass, and its home
// region does not start stocking a module thirty seconds later. The TTL keeps
// the memo from becoming a permanent blinder. It is keyed per (pilot, band)
// because it records that the pilot's own space is thin.

// scopeFailTTL is how long a failed band is skipped, in seconds.
const scopeFailTTL = 300

var (
	scopeFailMu    sync.Mutex
	scopeFailUntil = map[[2]int64]int64{} // (charID, band) → unix
)

// noteScopeFail remembers that this band had nothing for this pilot and reports
// whether that is news (false if it was already known empty). The log line rides
// that return, so shoppers do not write a row per pilot per pass.
func noteScopeFail(charID int64, band scopeBand) bool {
	key := [2]int64{charID, int64(band)}
	now := clock.Unix()
	scopeFailMu.Lock()
	defer scopeFailMu.Unlock()
	fresh := now >= scopeFailUntil[key]
	scopeFailUntil[key] = now + scopeFailTTL
	return fresh
}

// startBand is the band a climb should open at: the requested one, unless it
// is already known empty, in which case the first one above it that is not.
func startBand(charID int64, band scopeBand) scopeBand {
	now := clock.Unix()
	scopeFailMu.Lock()
	defer scopeFailMu.Unlock()
	for b := band; b < bandUniverse; b++ {
		if now >= scopeFailUntil[[2]int64{charID, int64(b)}] {
			return b
		}
	}
	return bandUniverse
}

// sweepScopeFails drops expired entries (registered on the cache sweep). A pilot
// whose region can now supply never asks about these keys again, so the read
// path's own expiry never frees them.
func sweepScopeFails() {
	now := clock.Unix()
	scopeFailMu.Lock()
	defer scopeFailMu.Unlock()
	for k, until := range scopeFailUntil {
		if now >= until {
			delete(scopeFailUntil, k)
		}
	}
}

// ── SQL and diagnostics ─────────────────────────────────────────────────────

// regionIDs is the scope as a sorted slice, so two pilots with the same scope
// produce the same SQL clause and share a market snapshot key (marketsnap.go).
func regionIDs(scope map[int64]bool) []int64 {
	if len(scope) == 0 {
		return nil
	}
	out := make([]int64, 0, len(scope))
	for id := range scope {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// residencyClause narrows a stationFilter (alias `s`, staStations) to the scope.
// It is folded into the filter so it rides the snapshot key and every station
// pass is scoped the same way.
func residencyClause(r travelRules) (string, []any) {
	ids := regionIDs(r.Regions)
	if len(ids) == 0 {
		return "", nil
	}
	return ` AND s.regionID IN (` + placeholders(len(ids)) + `)`, int64Args(ids)
}

// scoped returns f with the residency clause appended.
func (f stationFilter) scoped(r travelRules) stationFilter {
	clause, args := residencyClause(r)
	if clause == "" {
		return f
	}
	out := f
	out.Clause = f.Clause + clause
	out.Args = append(append([]any{}, f.Args...), args...)
	return out
}

// scopeNote names the pilot's economic horizon for a log line or a panel
// readout, empty when it has none.
func (m *Mod) scopeNote(ctx context.Context, r travelRules) string {
	if len(r.Regions) == 0 {
		return ""
	}
	// Named from the pilot's home: the sorted slice's first entry is an
	// arbitrary neighbour as often as not.
	home := m.nameOfRegion(ctx, r.Home)
	if home == "" {
		home = "its home region"
	}
	if n := len(r.Regions); n > 1 {
		return fmt.Sprintf("%s and %d neighbouring region(s)", home, n-1)
	}
	return home
}

func init() {
	registerCharForget("homeRegionCache", forgetHomeRegion)
	registerCharForget("scopeFailUntil", func(id int64) {
		scopeFailMu.Lock()
		for k := range scopeFailUntil {
			if k[0] == id {
				delete(scopeFailUntil, k)
			}
		}
		scopeFailMu.Unlock()
	})
	registerSweep("scopeFails", sweepScopeFails)
}
