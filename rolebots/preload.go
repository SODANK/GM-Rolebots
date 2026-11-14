package rolebots

import (
	"context"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The ring: the systems one gate jump from a player, loaded and populated before they get there.
// A player jumping into a cold system fires OnSystemLoaded and fillLoadedSystem (eight passes of
// twenty-five births, a second apart, embody.go), and gate traffic is never seen approaching
// because a bot in the next system is abstract. The ring keeps the systems adjacent to an
// occupied one loaded and projected, so a player arrives on a grid that is already alive.
// It is one jump, not the constellation (crimescene.go): a player's next jump is to an adjacent
// system, often in another constellation.
// Loading goes through modapi.EnsureSystemLoaded (core/main/rolebotpreload_mod.go). Never call
// SystemManager.GetOrCreate here: it registers an empty shell, the next real arrival is told
// created == false and skips the build, and the system keeps no celestials, belts or rats.

// settingPreloadRing switches the ring on. 0 (the default) is off: no extra systems loaded, no
// extra projections, and the embodiment gate answers as it did before.
const settingPreloadRing = "rolebots.preloadRing"

const (
	// preloadTicks is in tick.Manager ticks, one second each. It is not the roster pass (about a
	// minute at this roster size) or the grid lane: a system build is ten-odd queries plus an
	// asteroid field and does not belong on the clock that steps ships.
	preloadTicks = 5

	// preloadMaxLoadsPerPass is how many systems may be built in one pass, so somebody logging in
	// beside a six-gate hub spreads the cost over a few seconds.
	preloadMaxLoadsPerPass = 2

	// preloadMinHoldSec is how long a system stays loaded after it drops out of the ring, so a
	// player bouncing across one gate does not make it rebuild every few seconds.
	preloadMinHoldSec = 120
)

var (
	preloadMu sync.Mutex
	// preloadPinned is the set of systems this module loaded and is holding open. It is a set, not
	// a refcount: a system in two players' rings is pinned once and released when it is in nobody's.
	preloadPinned = map[int64]int64{} // sysID -> unix pinned
	// preloadCold is when a pinned system fell out of the ring. The hold below is measured from
	// here, and an entry is dropped when the system comes back into somebody's ring.
	preloadCold = map[int64]int64{} // sysID -> unix it went cold

	// preloadOn is the cached setting, read every pass.
	preloadOn atomic.Bool
	// Counters for the panel: what the ring has cost since boot.
	preloadLoads   atomic.Int64
	preloadUnloads atomic.Int64
)

// PreloadRing reports whether the ring is switched on.
func PreloadRing() bool { return preloadOn.Load() }

func setPreloadRing(on bool) { preloadOn.Store(on) }

// loadPreloadSetting reads the switch at boot. Off unless the setting says otherwise, and off
// on a core with no system-loading seam, where a dial could do nothing.
func (m *Mod) loadPreloadSetting(ctx context.Context) {
	if !modapi.CanLoadSystems() {
		rlogf("rolebots: ring preload unavailable — this core has no system-loading seam")
		return
	}
	// The docked pin defaults ON (preloaddocked.go): only an explicit "0" turns it off, and a
	// failed read leaves it on.
	setPreloadDocked(true)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingPreloadDocked); err != nil {
		rlogf("rolebots: read docked preload: %v (leaving it on)", err)
	} else if ok && strings.TrimSpace(v) == "0" {
		setPreloadDocked(false)
	}

	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingPreloadRing)
	if err != nil {
		rlogf("rolebots: read ring preload: %v (leaving it off)", err)
		return
	}
	if !ok {
		return
	}
	setPreloadRing(strings.TrimSpace(v) == "1")
}

// startPreloadLane registers the ring's own clock.
func (m *Mod) startPreloadLane() {
	m.registerPeriodic("rolebots-preload", preloadTicks, func(tickNum uint64) bool {
		if !Master() {
			return true
		}
		// Not gated on the switches: with both off the pass still has to contract, or systems pinned
		// before a switch-off stay held. An empty want with nothing pinned costs one map walk.
		if !m.preloadBusy.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer m.preloadBusy.Store(false)
			m.preloadPass()
		}()
		return true
	})
}

// preloadPass expands the ring by up to preloadMaxLoadsPerPass systems and
// releases the ones that have been out of it long enough.
func (m *Mod) preloadPass() {
	defer func() {
		if r := recover(); r != nil {
			rlogf("rolebots: preload lane panicked: %v\n%s", r, debug.Stack())
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The docked systems go first, with their own build budget: a player is certain to see them
	// next, and a hub's six neighbours must not hold the hangar's own system back.
	docked := map[int64]bool{}
	if PreloadDocked() {
		for sysID := range m.dockedSystems() {
			docked[sysID] = true
		}
		m.preloadExpand(docked)
	}

	want := map[int64]bool{}
	if PreloadRing() {
		// nil is no graph yet or no players on the server. There is nothing to expand toward, but a
		// pinned ring still has to be let go, so the contraction below runs against what is left.
		if ring := m.ringSystems(ctx); ring != nil {
			m.preloadExpand(ring)
			want = ring
		}
	}
	for sysID := range docked {
		want[sysID] = true
	}
	m.preloadContract(want)
}

// ringSystems is the set of systems exactly one gate jump from a system a real player is
// standing in, minus the occupied systems. The gate graph is in memory (router.go), so the ring
// is a map lookup per occupied system: no query and no walk sized by the roster.
func (m *Mod) ringSystems(ctx context.Context) map[int64]bool {
	here := m.playerSystems()
	if len(here) == 0 {
		return nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		rlogf("rolebots: ring universe: %v", err)
		return nil
	}
	return ringAround(here, u.adj)
}

// ringAround is the set arithmetic on its own: every neighbour of an occupied system that is
// not itself occupied. It is split out so it can be tested without a loaded universe.
func ringAround(here map[int64]bool, adj map[int64][]int64) map[int64]bool {
	ring := map[int64]bool{}
	for sysID := range here {
		for _, next := range adj[sysID] {
			// An occupied system is not in its neighbours' ring: a player is standing in it, so it is
			// loaded and projected already, and pinning it would hold open a system the core can release.
			if here[next] {
				continue
			}
			ring[next] = true
		}
	}
	return ring
}

// preloadExpand loads ring systems that are not up yet.
func (m *Mod) preloadExpand(want map[int64]bool) {
	built := 0
	for sysID := range want {
		if built >= preloadMaxLoadsPerPass {
			return
		}
		preloadMu.Lock()
		_, pinned := preloadPinned[sysID]
		// Back in the ring: whatever cooling-off it had started is void.
		delete(preloadCold, sysID)
		preloadMu.Unlock()
		if pinned {
			continue
		}
		// Already up because a player is in it, or was, and it has not been released yet. Pin it
		// anyway: it stays only because this module holds it.
		if m.systems != nil && m.systems.Get(sysID) != nil {
			preloadMu.Lock()
			preloadPinned[sysID] = clock.Unix()
			preloadMu.Unlock()
			continue
		}
		if _, err := modapi.EnsureSystemLoaded(sysID); err != nil {
			rlogf("rolebots: preload system %d: %v", sysID, err)
			continue
		}
		preloadMu.Lock()
		preloadPinned[sysID] = clock.Unix()
		preloadMu.Unlock()
		preloadLoads.Add(1)
		built++
		rlogf("rolebots: preloaded system %d (ring of %d)", sysID, len(want))
	}
}

// preloadContract releases pinned systems that have been out of the ring for
// preloadMinHoldSec.
func (m *Mod) preloadContract(want map[int64]bool) {
	now := clock.Unix()

	var due []int64
	preloadMu.Lock()
	for sysID := range preloadPinned {
		if want[sysID] {
			delete(preloadCold, sysID)
			continue
		}
		went, ok := preloadCold[sysID]
		if !ok {
			preloadCold[sysID] = now
			continue
		}
		if now-went >= preloadMinHoldSec {
			due = append(due, sysID)
		}
	}
	preloadMu.Unlock()

	for _, sysID := range due {
		m.preloadRelease(sysID)
	}
}

// preloadRelease gives one pinned system back. Order matters: a projection is an IsPlayer
// entity, so the core's RemoveIfEmpty refuses to tear a system down while any of ours stands in
// it. The projections come out first, then the system goes; reversed, the release does nothing
// and the bots hold the system open forever.
func (m *Mod) preloadRelease(sysID int64) {
	// Somebody arrived while it was cooling. Keep the pin, drop the stamp:
	// the next pass will find it in the ring (or occupied) and leave it be.
	if m.systems != nil {
		if ss := m.systems.Get(sysID); ss != nil && systemHasRealPlayer(ss) {
			preloadMu.Lock()
			delete(preloadCold, sysID)
			preloadMu.Unlock()
			return
		}
	}

	n := m.withdrawProjectionsInSystem(sysID)

	// modapi.ReleaseSystem, not RemoveIfEmpty: the core pairs it with a mandatory flush of in-memory
	// belt state. It races deEmbody's own release, so "released" means the system is gone.
	released := modapi.ReleaseSystem(sysID) ||
		(m.systems != nil && m.systems.Get(sysID) == nil)

	preloadMu.Lock()
	delete(preloadPinned, sysID)
	delete(preloadCold, sysID)
	preloadMu.Unlock()

	if released {
		preloadUnloads.Add(1)
		rlogf("rolebots: released preloaded system %d (%d projection(s) withdrawn)", sysID, n)
		return
	}
	// Refused: something not ours is still in it (likely a player who arrived since the check
	// above). The pin is gone either way; if they stay, the system is occupied and the ring omits it.
	rlogf("rolebots: system %d would not release (%d projection(s) withdrawn) — something is still in it", sysID, n)
}

// withdrawProjectionsInSystem takes every projection in one system off the registry and tears it
// down, and reports how many. The system is still loaded (unlike dropProjectionsInSystem), so
// each leaves through deEmbody. It holds embodyWorkMu across every teardown and each persists hull
// damage: about a hundred writes for a hub-sized system, affordable once per release.
func (m *Mod) withdrawProjectionsInSystem(sysID int64) int {
	embodyWorkMu.Lock()
	defer embodyWorkMu.Unlock()

	embodyMu.Lock()
	var doomed []*embodiedBot
	for charID, eb := range embodyLive {
		if eb.systemID != sysID {
			continue
		}
		doomed = append(doomed, eb)
		delete(embodyLive, charID)
	}
	embodyMu.Unlock()

	for _, eb := range doomed {
		m.deEmbody(eb)
	}
	return len(doomed)
}

// systemProjected reports whether a system should carry projections: the gate embodiment's "a
// real player is standing here" (systemHasRealPlayer) widened by one hop, so a loaded and pinned
// ring system counts too. It is not a replacement for systemWatched, which still means somebody
// is watching right now.
func (m *Mod) systemProjected(sysID int64) bool {
	if m.systems == nil {
		return false
	}
	ss := m.systems.Get(sysID)
	if ss == nil {
		return false
	}
	if systemHasRealPlayer(ss) {
		return true
	}
	// Answered from the session registry, not only the pin: the pin lands on the next preload pass,
	// and a reconcile in that gap would withdraw a freshly docked player's grid. Only a loaded system gets here.
	if PreloadDocked() && m.systemHasDockedPlayer(sysID) {
		return true
	}
	if !PreloadRing() && !PreloadDocked() {
		return false
	}
	preloadMu.Lock()
	_, pinned := preloadPinned[sysID]
	preloadMu.Unlock()
	return pinned
}

// PreloadStatus is what the panel reports about the ring.
type PreloadStatus struct {
	On        bool
	DockedOn  bool // the docked pin (preloaddocked.go)
	Available bool // this core carries the system-loading seam
	Pinned    int  // systems held open by the ring right now
	Loads     int  // systems built by the ring since boot
	Unloads   int  // systems released by it
}

func preloadStatus() PreloadStatus {
	preloadMu.Lock()
	pinned := len(preloadPinned)
	preloadMu.Unlock()
	return PreloadStatus{
		On:        PreloadRing(),
		DockedOn:  PreloadDocked(),
		Available: modapi.CanLoadSystems(),
		Pinned:    pinned,
		Loads:     int(preloadLoads.Load()),
		Unloads:   int(preloadUnloads.Load()),
	}
}
