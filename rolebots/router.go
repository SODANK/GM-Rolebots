package rolebots

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/seclaw"
)

// The router: pathfinding over mapSolarSystemJumps honoring a rolebot's security
// limits. The universe graph (about 5k systems, 13k edges) is cached on first use.

type sysMeta struct {
	Security float64
	RegionID int64
	// ConstellationID is what the CONCORD gate is measured in (crimescene.go): a
	// crime is policeable when a real player is in the same constellation.
	ConstellationID int64
}

type universe struct {
	adj  map[int64][]int64 // solarSystemID → neighbours
	meta map[int64]sysMeta

	// Two more static map facts every outward search asks about every system:
	// asteroid belts and sov. They load whole, once, since per-system queries cost
	// a query per bot per pass. Lazy because the harness seeds map tables late.
	beltOnce sync.Once
	belts    map[int64]bool
	beltErr  error

	sovOnce sync.Once
	sov     map[int64]int64
	sovErr  error

	// Region adjacency, derived from the graph above rather than queried
	// (residency.go). Lazy because the harness has no map tables.
	radjOnce sync.Once
	radj     map[int64]map[int64]bool

	// The lowest security any belt-bearing system in each region has: what the
	// region's rock can be, as against what its pilots dare (mineralstock_db.go).
	// Derived from meta and belts, with no query.
	obandOnce sync.Once
	obands    map[int64]float64

	// The regions no gate route reaches from the rest of New Eden (the Jove
	// blocks, which have stations and NPC market orders). Derived lazily.
	maroonOnce sync.Once
	maroons    map[int64]bool
	// Gate components: which connected piece of the map each system is in,
	// and which regions each piece touches (componentRegionsOf).
	compOnce    sync.Once
	compOf      map[int64]int
	compRegions map[int]map[int64]bool
}

// componentOf is the id of the gate-connected piece of the map a system is in; 0
// for a system the graph does not know. maroonedRegions (below) treats the piece
// with the most systems as the main body, so market scans skip sealed regions
// whose NPC orders no pilot can reach.
func (u *universe) componentOf(sys int64) int {
	u.buildComponents()
	return u.compOf[sys]
}

// sameComponent reports whether one system can be reached from another by
// gate at all, security rules aside.
func (u *universe) sameComponent(a, b int64) bool {
	ca, cb := u.componentOf(a), u.componentOf(b)
	return ca != 0 && ca == cb
}

// componentRegionsOf is every region with a system in the same gate component as
// sys. For a pilot born in a sealed pocket (the Jove schools) it is the pocket,
// which maroonedRegions cannot express since it is measured from the main body.
func (u *universe) componentRegionsOf(sys int64) map[int64]bool {
	u.buildComponents()
	return u.compRegions[u.compOf[sys]]
}

func (u *universe) buildComponents() {
	u.compOnce.Do(func() {
		u.compOf = map[int64]int{}
		u.compRegions = map[int]map[int64]bool{}
		next := 0
		for start := range u.meta {
			if u.compOf[start] != 0 {
				continue
			}
			next++
			regions := map[int64]bool{}
			queue := []int64{start}
			u.compOf[start] = next
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				if rid := u.meta[cur].RegionID; rid != 0 {
					regions[rid] = true
				}
				for _, nb := range u.adj[cur] {
					if u.compOf[nb] == 0 {
						u.compOf[nb] = next
						queue = append(queue, nb)
					}
				}
			}
			u.compRegions[next] = regions
		}
	})
}

func (u *universe) maroonedRegions() map[int64]bool {
	u.maroonOnce.Do(func() {
		seen := map[int64]bool{}
		best := map[int64]bool(nil)
		for start := range u.meta {
			if seen[start] {
				continue
			}
			comp := map[int64]bool{start: true}
			queue := []int64{start}
			for len(queue) > 0 {
				cur := queue[0]
				queue = queue[1:]
				seen[cur] = true
				for _, nb := range u.adj[cur] {
					if !comp[nb] {
						comp[nb] = true
						queue = append(queue, nb)
					}
				}
			}
			if len(comp) > len(best) {
				best = comp
			}
		}
		// A region is marooned only when none of its systems is in the main body;
		// a region straddling the edge is reachable.
		live := map[int64]bool{}
		for sys := range best {
			live[u.meta[sys].RegionID] = true
		}
		out := map[int64]bool{}
		for _, ms := range u.meta {
			if ms.RegionID != 0 && !live[ms.RegionID] {
				out[ms.RegionID] = true
			}
		}
		u.maroons = out
	})
	return u.maroons
}

// hasBelts reports whether a system contains at least one asteroid belt
// (mapDenormalize groupID 9). A system missing from the map reads as no belts.
func (u *universe) hasBelts(ctx context.Context, db *sql.DB, sysID int64) bool {
	u.beltOnce.Do(func() {
		set := map[int64]bool{}
		rows, err := db.QueryContext(ctx,
			`SELECT DISTINCT solarSystemID FROM mapDenormalize WHERE groupID = 9 AND solarSystemID IS NOT NULL`)
		if err != nil {
			u.beltErr = fmt.Errorf("universe belts: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				u.beltErr = err
				return
			}
			set[id] = true
		}
		u.beltErr = rows.Err()
		u.belts = set
	})
	if u.beltErr != nil {
		// One failed load must not declare the universe beltless for the rest of the
		// process: fall back to the single-system query.
		var one int64
		return db.QueryRowContext(ctx,
			`SELECT 1 FROM mapDenormalize WHERE solarSystemID = ? AND groupID = 9 LIMIT 1`,
			sysID).Scan(&one) == nil
	}
	return u.belts[sysID]
}

// sovFaction is db.GetSystemSovFactionID for the whole map at once: the NPC
// faction holding a system, falling back to its constellation then its region,
// 0 for unclaimed. Ore and ice distribution key off it.
func (u *universe) sovFaction(ctx context.Context, db *sql.DB, sysID int64) int64 {
	u.sovOnce.Do(func() {
		m := map[int64]int64{}
		rows, err := db.QueryContext(ctx, `
			SELECT s.solarSystemID, COALESCE(s.factionID, c.factionID, r.factionID, 0)
			  FROM mapSolarSystems s
			  LEFT JOIN mapConstellations c ON c.constellationID = s.constellationID
			  LEFT JOIN mapRegions r ON r.regionID = s.regionID`)
		if err != nil {
			u.sovErr = fmt.Errorf("universe sov: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id, faction int64
			if err := rows.Scan(&id, &faction); err != nil {
				u.sovErr = err
				return
			}
			m[id] = faction
		}
		u.sovErr = rows.Err()
		u.sov = m
	})
	if u.sovErr != nil {
		f, err := evedb.GetSystemSovFactionID(ctx, db, sysID)
		if err != nil {
			return 0
		}
		return f
	}
	return u.sov[sysID]
}

var (
	uniOnce sync.Once
	uni     *universe
	uniErr  error
)

// uniOverride replaces the database-loaded universe when set. Only the harness
// sets it, so a test can lay out a few systems with known distances. It is
// checked before the sync.Once, which caches its answer for the process.
var uniOverride *universe

func (m *Mod) universe(ctx context.Context) (*universe, error) {
	if uniOverride != nil {
		return uniOverride, nil
	}
	uniOnce.Do(func() {
		u := &universe{adj: map[int64][]int64{}, meta: map[int64]sysMeta{}}
		rows, err := m.db.QueryContext(ctx,
			`SELECT solarSystemID, COALESCE(security, 0), COALESCE(regionID, 0),
			        COALESCE(constellationID, 0) FROM mapSolarSystems`)
		if err != nil {
			uniErr = fmt.Errorf("universe systems: %w", err)
			return
		}
		for rows.Next() {
			var id int64
			var ms sysMeta
			if err := rows.Scan(&id, &ms.Security, &ms.RegionID, &ms.ConstellationID); err != nil {
				rows.Close()
				uniErr = err
				return
			}
			u.meta[id] = ms
		}
		rows.Close()
		if uniErr = rows.Err(); uniErr != nil {
			return
		}
		jrows, err := m.db.QueryContext(ctx,
			`SELECT fromSolarSystemID, toSolarSystemID FROM mapSolarSystemJumps`)
		if err != nil {
			uniErr = fmt.Errorf("universe jumps: %w", err)
			return
		}
		for jrows.Next() {
			var from, to int64
			if err := jrows.Scan(&from, &to); err != nil {
				jrows.Close()
				uniErr = err
				return
			}
			u.adj[from] = append(u.adj[from], to)
		}
		jrows.Close()
		if uniErr = jrows.Err(); uniErr != nil {
			return
		}
		uni = u
	})
	return uni, uniErr
}

// dispSec is the security a player sees: mapSolarSystems.security is truesec,
// and operators and the client overview use it rounded to one decimal. Comparing
// a floor against raw truesec would ban systems the game calls high-sec (the
// 0.45 to 0.499 band) and shatter the route graph into pockets.
func dispSec(sec float64) float64 { return math.Round(sec*10) / 10 }

// secAversionJumps is what a pilot's comfort line is worth as a preference:
// jump-equivalents added per 1.0 of security shortfall on a system it enters.
// At 20, a pilot that likes 0.5 treats a 0.0 hop as about eleven jumps.
const secAversionJumps = 20.0

// travelRules are the constraints a leg must satisfy.
type travelRules struct {
	// SecFloor is the hard line: a system below it is never entered. Only an
	// activity's own secFloor override sets one (a mining or trade gang config).
	// -1 means none, the default.
	SecFloor float64

	// SecCeil is a hard ceiling: a system at or above it is never entered. 0 means
	// none; nothing sets one today, since an outlaw's policed space is priced
	// (OutlawDanger).
	SecCeil float64

	// PreferSec / SecAversion are the soft line: where the pilot would rather be.
	// They bend the path and ranking, never the reachable set.
	PreferSec   float64
	SecAversion float64

	// Regions is the economic scope (residency.go): the regions a search may
	// consider, or nil. Only economicRules sets it, never rulesForBot, so route()
	// and relocation run unscoped. Home and Band accompany it for logs.
	Regions map[int64]bool
	Home    int64
	Band    scopeBand

	// EnemyCorp / EnemyJumps are the corporate-politics soft line (corprel.go):
	// systems where an enemy works or keeps an address cost extra. A price, never
	// a gate: it stays out of allowed(). Set only by economicRules, not for a pilot out fighting.
	EnemyCorp  int64
	EnemyJumps int

	// GateDanger is the hauler's soft line (gatekills.go): jump-equivalents per log
	// unit of the week's kills at a lane's gates, read off the killboard. Set only
	// by travelRulesFor, for a hull that is prey. A price, never a gate. 0 = unread.
	GateDanger float64

	// OutlawDanger is the outlaw's soft line (outlawdanger.go): jump-equivalents per
	// log unit of pilots in space on a policed hop. Set by rulesForBot for a pilot
	// at or below seclaw.OutlawStatus. A price, never a gate. 0 = none.
	OutlawDanger float64
}

func rulesFor(p *Profile) travelRules {
	return travelRules{SecFloor: -1}
}

// rulesForBot decides where a bot may fly. There is no security fence: a pilot's
// comfort line (Tuning.PersonalSecFloor) prices the route instead of closing it.
// Per-activity secFloor overrides (gang configs) still apply after this returns.
// See docs/comment-notes-b16.md#no-security-fence.
func rulesForBot(b *botRun) travelRules {
	r := rulesFor(b.Profile)
	r.PreferSec = b.Tune.PersonalSecFloor
	r.SecAversion = secAversionJumps
	// An outlaw is a free target in policed space: priced, never closed
	// (outlawdanger.go).
	if b.SecStatus <= seclaw.OutlawStatus {
		r.OutlawDanger = outlawDangerJumps
	}
	return r
}

// allowed reports whether the bot may enter sysID under the rules.
func (u *universe) allowed(sysID int64, r travelRules) bool {
	ms, ok := u.meta[sysID]
	if !ok {
		return false
	}
	if dispSec(ms.Security) < r.SecFloor {
		return false
	}
	if r.SecCeil > 0 && dispSec(ms.Security) >= r.SecCeil {
		return false
	}
	if len(r.Regions) > 0 && !r.Regions[ms.RegionID] {
		return false
	}
	return true
}

// hopCost is what entering sysID from fromSys costs this pilot, in
// jump-equivalents: one for the gate, plus its aversion to the space on the far
// side, plus (for a hauler) the violence recorded at the two gates it crosses.
// Always at least 1, so a path is never free and the search terminates.
func (u *universe) hopCost(fromSys, sysID int64, r travelRules) int {
	c := 1
	// The hauler's term. gateDangerBetween reads the last board the caller primed
	// (primeGateDanger); additive and non-negative, so Dijkstra stays correct.
	if r.GateDanger > 0 {
		if d := gateDangerBetween(fromSys, sysID); d > 0 {
			c += int(math.Round(r.GateDanger * math.Log1p(d)))
		}
	}
	// The outlaw's term, primed like the hauler's (primeOutlawDanger).
	c += outlawHopCost(r.OutlawDanger, dispSec(u.meta[sysID].Security), outlawEyesIn(sysID))
	if r.SecAversion > 0 {
		if short := r.PreferSec - dispSec(u.meta[sysID].Security); short > 0 {
			c += int(math.Round(r.SecAversion * short))
		}
	}
	// The corporate term is additive and non-negative, so Dijkstra stays correct.
	if r.EnemyJumps > 0 && r.EnemyCorp != 0 && relEnemySystem(r.EnemyCorp, sysID) {
		// A war enemy's system costs warAvoidMul times as much: a rival may shoot
		// only outside policed space, a war enemy anywhere. Still a cost, never a gate.
		if relWarEnemySystem(r.EnemyCorp, sysID) {
			c += int(math.Round(float64(r.EnemyJumps) * warAvoidMul()))
		} else {
			c += r.EnemyJumps
		}
	}
	return c
}

// costQueue is the Dijkstra frontier: cheapest jump-equivalent cost first.
type costQueue struct {
	sys  []int64
	cost []int
}

func (q *costQueue) Len() int           { return len(q.sys) }
func (q *costQueue) Less(i, j int) bool { return q.cost[i] < q.cost[j] }
func (q *costQueue) Swap(i, j int) {
	q.sys[i], q.sys[j] = q.sys[j], q.sys[i]
	q.cost[i], q.cost[j] = q.cost[j], q.cost[i]
}
func (q *costQueue) Push(x any) {
	e := x.([2]int64)
	q.sys = append(q.sys, e[0])
	q.cost = append(q.cost, int(e[1]))
}
func (q *costQueue) Pop() any {
	n := len(q.sys) - 1
	e := [2]int64{q.sys[n], int64(q.cost[n])}
	q.sys, q.cost = q.sys[:n], q.cost[:n]
	return e
}

// route returns the system chain from `from` to `to`, excluding `from`, through
// allowed systems only (the destination must be allowed too). nil and an error
// when unreachable. The search is cheapest-first over hopCost, not shortest-hop.
func (m *Mod) route(ctx context.Context, from, to int64, r travelRules) ([]int64, error) {
	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	if from == to {
		return []int64{}, nil
	}
	m.primeGateDanger(ctx, r)
	m.primeOutlawDanger(ctx, r)
	parent := map[int64]int64{from: from}
	best := map[int64]int{from: 0}
	q := &costQueue{}
	heap.Push(q, [2]int64{from, 0})
	for q.Len() > 0 {
		e := heap.Pop(q).([2]int64)
		cur, cost := e[0], int(e[1])
		if cost > best[cur] {
			continue // stale queue entry
		}
		if cur == to {
			var path []int64
			for at := to; at != from; at = parent[at] {
				path = append([]int64{at}, path...)
			}
			return path, nil
		}
		for _, nb := range u.adj[cur] {
			if !u.allowed(nb, r) {
				continue
			}
			nc := cost + u.hopCost(cur, nb, r)
			if seen, ok := best[nb]; ok && seen <= nc {
				continue
			}
			best[nb], parent[nb] = nc, cur
			heap.Push(q, [2]int64{nb, int64(nc)})
		}
	}
	return nil, fmt.Errorf("%w from %d to %d%s", errNoRoute, from, to, u.whyNoRoute(from, to, r))
}

// errNoRoute is what route() wraps when the rules close every path off, so a
// caller whose destination is a place the pilot merely owns something at (a
// parked ship) can tell "cannot get there" from a broken bot.
var errNoRoute = errors.New("no route")

// whyNoRoute names what actually closed a search off, geography first: a
// destination in another sealed pocket of the map (the Jove blocks), a security
// ceiling, or else the rule why() names.
func (u *universe) whyNoRoute(from, to int64, r travelRules) string {
	if !u.sameComponent(from, to) {
		return " — no gate connects them (they lie in separate sealed pockets of the map)"
	}
	if r.SecCeil > 0 && len(r.Regions) == 0 && r.SecFloor <= -1 {
		return fmt.Sprintf(" — this pilot cannot enter space of %.1f security or more", r.SecCeil)
	}
	return r.why()
}

// why names the rule that closed a route off, for a log an operator can act
// on. Only hard rules appear: the pilot's own preference never blocks
// anything, so naming it would send the operator to the wrong page.
func (r travelRules) why() string {
	// Residency first: when a scope is on it is the narrowest rule in play and
	// almost always the one that closed the search off.
	if len(r.Regions) > 0 {
		return fmt.Sprintf(" inside the %d region(s) this pilot's economy is scoped to (%s)",
			len(r.Regions), r.Band)
	}
	if r.SecFloor > -1 {
		return fmt.Sprintf(" that stays in %.1f-or-safer space (this activity's own security floor)", r.SecFloor)
	}
	return " — the two are not connected by gates"
}

// limitNote describes the hard space limit for a diagnostic, or says there
// is none. A pilot with no limit that still cannot get anywhere has a gate
// problem, and the log should say so.
func (r travelRules) limitNote() string {
	return " " + r.limitSummary()
}

// limited reports whether any of the three gates allowed() tests (security
// floor, security ceiling, residency scope) is in force. False means an
// unreachable station is unreachable by geography. limitSummary names only the
// gates in force.
func (r travelRules) limited() bool {
	return r.SecFloor > -1 || r.SecCeil > 0 || len(r.Regions) > 0
}

func (r travelRules) limitSummary() string {
	var parts []string
	if r.SecFloor > -1 {
		parts = append(parts, fmt.Sprintf("it will not enter systems below %.1f", r.SecFloor))
	}
	if r.SecCeil > 0 {
		parts = append(parts, fmt.Sprintf("it will not enter systems at or above %.1f", r.SecCeil))
	}
	if len(r.Regions) > 0 {
		parts = append(parts, fmt.Sprintf("it is searching %s only", r.Band))
	}
	if len(parts) == 0 {
		return "(no limit is set — this pilot flies anywhere)"
	}
	return "(" + joinAnd(parts) + ")"
}

// nearestWhere runs an outward BFS from `from` (respecting rules) and returns
// the closest system satisfying pred, up to maxJumps. pred is tested on `from` too
// when allowed; a preferred region wins among equal distances. For database
// questions use reach.go: pred runs per system and forces a radius.
func (m *Mod) nearestWhere(ctx context.Context, from int64, r travelRules,
	preferRegions map[int64]bool, maxJumps int,
	pred func(sysID int64, meta sysMeta) bool) (int64, error) {

	u, err := m.universe(ctx)
	if err != nil {
		return 0, err
	}
	seen := map[int64]bool{from: true}
	frontier := []int64{from}
	for depth := 0; depth <= maxJumps; depth++ {
		var hit, preferredHit int64
		for _, s := range frontier {
			ms := u.meta[s]
			if (s == from || u.allowed(s, r)) && pred(s, ms) {
				if preferRegions[ms.RegionID] {
					preferredHit = s
					break
				}
				if hit == 0 {
					hit = s
				}
			}
		}
		if preferredHit != 0 {
			return preferredHit, nil
		}
		if hit != 0 {
			return hit, nil
		}
		var next []int64
		for _, s := range frontier {
			for _, nb := range u.adj[s] {
				if seen[nb] || !u.allowed(nb, r) {
					continue
				}
				seen[nb] = true
				next = append(next, nb)
			}
		}
		frontier = next
		if len(frontier) == 0 {
			break
		}
	}
	return 0, fmt.Errorf("nothing suitable within %d jumps", maxJumps)
}
