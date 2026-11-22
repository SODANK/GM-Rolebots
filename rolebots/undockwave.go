package rolebots

import (
	"context"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// Undocking, staged. Two problems at a trade hub: every ship leaving a station left from the same point, and
// nothing paced the departures.
//
// dockExit is a pure function of the station (the staStationTypes dockEntry offset the server's own undock reads),
// so bots undocking together were co-located and nothing in destiny checks occupancy. The answer is a lattice of
// slots at the door (claimUndockSlot) and a per-station release budget (admitUndock). The budget applies everywhere,
// not only where a player is watching: a hub's backlog is built by the bots nobody is looking at.

const (
	// Defaults. All four are server_settings, read at boot.
	defUndockWaveSize  = 6
	defUndockWaveSec   = 5
	defUndockRunSec    = 10
	defUndockLoiterPct = 25

	settingUndockWaveSize  = "rolebots.undockWaveSize"
	settingUndockWaveSec   = "rolebots.undockWaveSec"
	settingUndockRunSec    = "rolebots.undockRunSec"
	settingUndockLoiterPct = "rolebots.undockLoiterPct"

	// rolebots.undockHoldOnDryGrid: hold the door while the warp budget of the grid outside it is spent. 1 = on
	// (default), 0 = off. See shouldHoldUndock.
	defUndockHoldOnDryGrid     = 1
	settingUndockHoldOnDryGrid = "rolebots.undockHoldOnDryGrid"

	// rolebots.undockWatchedPerMin: how many pilots a minute may leave a station whose grid a connected player is on.
	// 0 = no cap (the wave gate alone). See watchedDoorTake.
	defUndockWatchedPerMin     = 12
	settingUndockWatchedPerMin = "rolebots.undockWatchedPerMin"
)

var (
	undockCfgMu         sync.RWMutex
	undockWaveSize      = defUndockWaveSize
	undockWaveSec       = int64(defUndockWaveSec)
	undockRunSec        = int64(defUndockRunSec)
	undockLoiterPct     = defUndockLoiterPct
	undockLoiterMin     = int64(defUndockLoiterMinSec)
	undockLoiterMax     = int64(defUndockLoiterMaxSec)
	undockHoldOnDryGrid = defUndockHoldOnDryGrid != 0
	undockWatchedPerMin = defUndockWatchedPerMin
)

// loadUndockSettings reads the dials at boot and again every minute (trafficsettings.go). Anything missing or
// unparseable keeps the default.
func (m *Mod) loadUndockSettings(ctx context.Context) {
	readInt := func(name string, def int) int {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, name)
		if err != nil || !ok {
			return def
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return def
		}
		return n
	}
	size := readInt(settingUndockWaveSize, defUndockWaveSize)
	sec := readInt(settingUndockWaveSec, defUndockWaveSec)
	run := readInt(settingUndockRunSec, defUndockRunSec)
	pct := readInt(settingUndockLoiterPct, defUndockLoiterPct)
	lmin := readInt(settingUndockLoiterMinSec, defUndockLoiterMinSec)
	lmax := readInt(settingUndockLoiterMaxSec, defUndockLoiterMaxSec)
	if sec < 1 {
		// A zero-length window would refill the budget on every call, which is no gate at all. A size of 0 turns the
		// pacing off, checked in admitUndock.
		sec = 1
	}
	if pct > 100 {
		pct = 100
	}
	lmin, lmax = clampLoiterSpan(lmin, lmax)
	hold := readInt(settingUndockHoldOnDryGrid, defUndockHoldOnDryGrid) != 0
	watchedPerMin := readInt(settingUndockWatchedPerMin, defUndockWatchedPerMin)
	undockCfgMu.Lock()
	undockWaveSize, undockWaveSec, undockRunSec, undockLoiterPct = size, int64(sec), int64(run), pct
	undockLoiterMin, undockLoiterMax = int64(lmin), int64(lmax)
	undockHoldOnDryGrid = hold
	undockWatchedPerMin = watchedPerMin
	undockCfgMu.Unlock()
}

// UndockWatchedPerMin reports the watched-door cap (0 = off).
func UndockWatchedPerMin() int {
	undockCfgMu.RLock()
	defer undockCfgMu.RUnlock()
	return undockWatchedPerMin
}

// UndockHoldOnDryGrid reports whether the door is coupled to the warp budget.
func UndockHoldOnDryGrid() bool {
	undockCfgMu.RLock()
	defer undockCfgMu.RUnlock()
	return undockHoldOnDryGrid
}

func undockWave() (size int, window int64) {
	undockCfgMu.RLock()
	defer undockCfgMu.RUnlock()
	return undockWaveSize, undockWaveSec
}

// UndockRunSec is the baseline burn: how long a ship owns the helm out of a door whose station is not known to the
// caller. Read by both tiers. It is a floor and a fallback; see UndockRunSecFor.
func UndockRunSec() int64 {
	undockCfgMu.RLock()
	defer undockCfgMu.RUnlock()
	return undockRunSec
}

// UndockRunSecFor is how long this ship burns out of this door: the run the geometry asks for at the speed the hull
// leaves at. It is clamped, and the ceiling matters: undockGraceSec must stay strictly longer than the run (a test
// pins it) and must not be widened to fit; a hull too slow to finish has settleUndock write its standoff row while the
// ball still flies. This is the control window only; undockFreshSec, keyed on the undock time, decides replay.
func UndockRunSecFor(runM, maxVelocity float64) int64 {
	base := UndockRunSec()
	if runM <= 0 || maxVelocity <= 0 {
		return base
	}
	sec := int64(math.Ceil(runM / maxVelocity))
	if sec < base {
		return base
	}
	if ceiling := int64(undockGraceSec) - undockRunGraceMarginSec; sec > ceiling {
		return ceiling
	}
	return sec
}

// undockRunGraceMarginSec keeps UndockRunSecFor's ceiling strictly inside undockGraceSec. The two are compared
// with > in one place and >= in another, so a run landing exactly on the grace would be a coin toss.
const undockRunGraceMarginSec = 30

// UndockLoiterPct is the share of departures that hang about outside the
// station for a while instead of leaving straight away.
func UndockLoiterPct() int {
	undockCfgMu.RLock()
	defer undockCfgMu.RUnlock()
	return undockLoiterPct
}

// clampLoiterSpan makes an operator-supplied loiter range safe to roll inside. A zero or negative floor books a leg of
// no length; a crossed pair makes the unsigned modulo span roughly 2^64 seconds, a ship that never leaves again.
func clampLoiterSpan(lo, hi int) (int, int) {
	if lo < 1 {
		lo = 1
	}
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

// UndockLoiterSpan is how long such a departure hangs about, in seconds.
// loadUndockSettings guarantees 1 <= lo <= hi, which undockLoiterFor relies on:
// its span is unsigned.
func UndockLoiterSpan() (lo, hi int64) {
	undockCfgMu.RLock()
	defer undockCfgMu.RUnlock()
	return undockLoiterMin, undockLoiterMax
}

// ── the release budget ──────────────────────────────────────────────────────

type stationWave struct {
	windowStart int64
	released    int
	// gangID of the fleet currently being let out, and how many of it are
	// still expected. A fleet is admitted as a unit: see admitUndock.
	gangID   int64
	gangLeft int
}

var (
	undockGateMu sync.Mutex
	undockGates  = map[int64]*stationWave{}
)

// admitUndock reports whether this pilot may leave the station now. A refusal costs nothing: beginTravel leaves the
// bot docked with its leg timer pushed out one window. Gangs are never split: the first member to ask inside a window
// claims the station for the whole fleet and is let out even if that overruns the budget. Fleet members are pinned to
// one sim worker (simpool.go), so they ask back to back inside one pass, which is what makes the claim land.
func admitUndock(stationID, gangID int64, gangSize int) bool {
	size, window := undockWave()
	if size <= 0 || stationID == 0 {
		return true // pacing disabled
	}
	now := clock.Unix()

	undockGateMu.Lock()
	defer undockGateMu.Unlock()

	w := undockGates[stationID]
	if w == nil || now-w.windowStart >= window {
		w = &stationWave{windowStart: now}
		undockGates[stationID] = w
	}

	if gangID != 0 {
		// Already the fleet holding this window: let it through regardless of
		// the budget, until its headcount is spent.
		if w.gangID == gangID && w.gangLeft > 0 {
			w.gangLeft--
			w.released++
			return true
		}
		// A different fleet already holds the window: wait for the next one rather than interleaving two fleets out of one door.
		if w.gangID != 0 && w.gangID != gangID && w.gangLeft > 0 {
			return false
		}
		if w.released >= size {
			return false
		}
		if gangSize < 1 {
			gangSize = 1
		}
		w.gangID = gangID
		w.gangLeft = gangSize - 1
		w.released++
		return true
	}

	if w.released >= size {
		return false
	}
	w.released++
	return true
}

// admitUndockFor asks the gate on behalf of one pilot, resolving its fleet's headcount first so a whole fleet can be
// claimed at once. The headcount query runs only for a bot in a gang, at most once per pilot per window.
func (m *Mod) admitUndockFor(ctx context.Context, b *botRun) bool {
	// Whether a player is on this station's grid costs a scan, and both checks below want it: asked at most once.
	watchedKnown, watched := false, false
	gridWatched := func() bool {
		if !watchedKnown {
			watched, watchedKnown = m.stationGridWatched(ctx, b.SystemID, b.StationID), true
		}
		return watched
	}
	// The grid outside the door first, and before the wave is claimed: a refusal here must not spend a window slot, or a
	// dry grid also starves the fleet that would have been next.
	if shouldHoldUndock(b.SystemID, gridWatched) {
		return false
	}
	// The watched-door cap, also before the wave: a door with no token must not spend a window slot either. A fleet
	// already holding the window rides its claim without a token, so it still leaves as a unit.
	capped := UndockWatchedPerMin() > 0 && b.StationID != 0 && gridWatched() &&
		!fleetHoldsWindow(b.StationID, b.GangID)
	if capped && !watchedDoorReady(b.StationID, clock.Now()) {
		return false
	}
	size := 0
	if b.GangID != 0 {
		if n, err := gangSize(ctx, m.db, b.GangID); err == nil {
			size = n
		}
	}
	if !admitUndock(b.StationID, b.GangID, size) {
		return false
	}
	if capped {
		watchedDoorSpend(b.StationID, clock.Now())
	}
	return true
}

// ── the watched door ────────────────────────────────────────────────────────

// The wave gate releases undockWaveSize per undockWaveSec per station and the dry-grid hold shuts only once warps are
// refused, so a watched door gets its own small bucket, refilled at undockWatchedPerMin, independent of the warp
// gate's reading. Unwatched doors are untouched: nobody is there to see a queue.
const watchedDoorBurst = 3.0

type watchedDoor struct {
	tokens     float64
	lastRefill time.Time
}

var (
	watchedDoorMu sync.Mutex
	watchedDoors  = map[int64]*watchedDoor{}
)

// watchedDoorRefill brings a station's bucket up to now. watchedDoorMu held.
func watchedDoorRefill(stationID int64, now time.Time) *watchedDoor {
	d := watchedDoors[stationID]
	if d == nil {
		d = &watchedDoor{tokens: watchedDoorBurst, lastRefill: now}
		watchedDoors[stationID] = d
		return d
	}
	if el := now.Sub(d.lastRefill).Seconds(); el > 0 {
		d.tokens += el * float64(UndockWatchedPerMin()) / 60
		if d.tokens > watchedDoorBurst {
			d.tokens = watchedDoorBurst
		}
		d.lastRefill = now
	}
	return d
}

// watchedDoorReady reports whether the watched door has a slot now. It does
// not spend one: the wave gate still has to agree first.
func watchedDoorReady(stationID int64, now time.Time) bool {
	watchedDoorMu.Lock()
	defer watchedDoorMu.Unlock()
	return watchedDoorRefill(stationID, now).tokens >= 1
}

// watchedDoorSpend takes the slot once the pilot is actually let out. Floored
// so a fleet's claim racing a solo pilot cannot dig a hole longer than a burst.
func watchedDoorSpend(stationID int64, now time.Time) {
	watchedDoorMu.Lock()
	defer watchedDoorMu.Unlock()
	d := watchedDoorRefill(stationID, now)
	if d.tokens--; d.tokens < -watchedDoorBurst {
		d.tokens = -watchedDoorBurst
	}
}

// fleetHoldsWindow reports whether gangID is the fleet currently being let
// out of this station's wave window with members still to come.
func fleetHoldsWindow(stationID, gangID int64) bool {
	if gangID == 0 {
		return false
	}
	_, window := undockWave()
	now := clock.Unix()
	undockGateMu.Lock()
	defer undockGateMu.Unlock()
	w := undockGates[stationID]
	return w != nil && now-w.windowStart < window && w.gangID == gangID && w.gangLeft > 0
}

// sweepWatchedDoors drops buckets that have refilled to full and sat idle.
func sweepWatchedDoors() {
	cutoff := clock.Now().Add(-10 * time.Minute)
	watchedDoorMu.Lock()
	for id, d := range watchedDoors {
		if d.lastRefill.Before(cutoff) {
			delete(watchedDoors, id)
		}
	}
	watchedDoorMu.Unlock()
}

// ── the door and the warp budget ────────────────────────────────────────────

// shouldHoldUndock reports whether a pilot should stay in the hangar because the grid outside the door cannot take
// another departure: the only way off is a warp, and the stamp budget (warpbudget.go) admits warpBudgetPerMin of them
// grid-wide. It fires only on a fresh, dry gate reading and only when a player is on the station's grid (gridWatched,
// last because it costs a scan); departureWitnessed admits other departures unbilled. travelStep asks again later.
func shouldHoldUndock(systemID int64, gridWatched func() bool) bool {
	if !UndockHoldOnDryGrid() || systemID == 0 {
		return false
	}
	dry, fresh := peekWarpDrained(systemID)
	if !fresh || !dry {
		return false
	}
	return gridWatched()
}

// stationGridWatched reports whether a connected player is within grid range of the station, i.e. whether a ball
// leaving its door would be witnessed, the only case a departure is billed. Anything missing (no loaded system, no
// station row) reads as unwatched, so a door is never held on a question that could not be answered.
func (m *Mod) stationGridWatched(ctx context.Context, systemID, stationID int64) bool {
	return m.celestialGridWatched(ctx, systemID, stationID)
}

// celestialGridWatched reports whether a connected player is within BubbleRadius of a celestial (anything
// CachedSystemCelestials lists) in a loaded system. The gate wave asks it of the far-side gate; an unloaded system
// or an unknown itemID reads as unwatched.
func (m *Mod) celestialGridWatched(ctx context.Context, systemID, itemID int64) bool {
	if m.systems == nil || itemID == 0 {
		return false
	}
	ss := m.systems.Get(systemID)
	if ss == nil {
		return false
	}
	cels, err := evedb.CachedSystemCelestials(ctx, m.db, systemID)
	if err != nil {
		return false
	}
	var pos destiny.Vec3
	found := false
	for _, c := range cels {
		if c.ItemID == itemID {
			pos, found = destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}, true
			break
		}
	}
	if !found {
		return false
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil {
			continue
		}
		if p.Pos().Sub(pos).LenSq() <= destiny.BubbleRadius*destiny.BubbleRadius {
			return true
		}
	}
	return false
}

// undockHoldSec is how long the abstract tier leaves a freshly undocked bot
// alone. The burn out of the door owns that window and nothing else may steer
// the ship inside it, so the hold is whichever is longer: the dock dwell the
// sim has always taken, or the run.
func undockHoldSec() int64 {
	if run := UndockRunSec(); run > dockSec {
		return run
	}
	return dockSec
}

// ── hanging about outside ───────────────────────────────────────────────────

const (
	// How long a departure that drew the loiter hangs about, in minutes rather than seconds, so a station has ships
	// doing something other than leaving. Both are server_settings, read at boot beside the four undock dials.
	defUndockLoiterMinSec = 60
	defUndockLoiterMaxSec = 300

	settingUndockLoiterMinSec = "rolebots.undockLoiterMinSec"
	settingUndockLoiterMaxSec = "rolebots.undockLoiterMaxSec"
)

// undockLoiterFor decides whether this departure hangs about outside the station, and for how long; 0 means it
// leaves straight away. It is rolled from (charID, undockAt) rather than a random source so both tiers give the same
// answer without storing a decision, and it varies per departure. A circling ship costs nothing while it circles.
func undockLoiterFor(charID, undockAt int64) int64 {
	pct := UndockLoiterPct()
	if pct <= 0 {
		return 0
	}
	// A cheap integer hash of the pair: successive undocks by one pilot, and neighbouring character IDs undocking
	// together, must not correlate.
	h := uint64(charID)*0x9E3779B97F4A7C15 ^ uint64(undockAt)*0xC2B2AE3D27D4EB4F
	h ^= h >> 29
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 32
	if int(h%100) >= pct {
		return 0
	}
	lo, hi := UndockLoiterSpan()
	span := uint64(hi - lo + 1)
	return lo + int64((h>>7)%span)
}

// undockLegEnd is when the abstract tier may next look at a bot that has just
// left a station: after the burn, and after the loiter if it drew one. The sim
// must not warp a ship away from a circle it is still flying.
func undockLegEnd(sd *StateData) int64 {
	end := clock.Unix() + dockSec
	if sd.UndockRunUntil > end {
		end = sd.UndockRunUntil
	}
	if sd.UndockLoiterUntil > end {
		end = sd.UndockLoiterUntil
	}
	return end
}

// undockRetryDelay is how long a pilot the gate turned away waits before asking again: one window plus up to one
// more, so the bots refused by one busy window do not all come back in the same instant.
func undockRetryDelay() int64 {
	_, window := undockWave()
	return window + rand.Int63n(window+1)
}

// sweepUndockGates drops windows nothing has used for a while, so the registry does not only grow.
func sweepUndockGates() {
	_, window := undockWave()
	cutoff := clock.Unix() - window*10
	undockGateMu.Lock()
	for stationID, w := range undockGates {
		if w.windowStart < cutoff {
			delete(undockGates, stationID)
		}
	}
	undockGateMu.Unlock()
	// Slot holds are pruned by destiny.ClaimUndockSlot, so there is nothing to sweep here.
}

// ── the slot lattice ────────────────────────────────────────────────────────
//
// The slot occupancy map is destiny's (destiny/undocklane.go); see claimUndockSlot.

// claimUndockSlot picks a slot at a station's door that no other ship is using and holds it for the undock run. It is
// destiny.ClaimUndockSlot, one registry shared with the server's own undock. A holder renews rather than taking a
// second slot, which both tiers rely on to agree on the undock point. The holder is the ship, so parking anywhere
// frees the door it left. hold is in seconds; destiny takes a time.Duration.
func claimUndockSlot(objectID, shipID int64, hold int64) int {
	if hold < 1 {
		hold = 1
	}
	return destiny.ClaimUndockSlot(objectID, shipID, time.Duration(hold)*time.Second)
}

// claimUndockSlotWhere is claimUndockSlot with destiny.ClaimUndockSlotWhere's veto: fits
// (nil accepts all) runs under the registry lock and must be pure.
func claimUndockSlotWhere(objectID, shipID int64, hold int64, fits func(slot int) bool) int {
	if hold < 1 {
		hold = 1
	}
	return destiny.ClaimUndockSlotWhere(objectID, shipID, time.Duration(hold)*time.Second, fits)
}

// releaseUndockSlot hands a slot back early, for a bot that docked again or
// lost its ship before the run was over.
func releaseUndockSlot(objectID, shipID int64) {
	destiny.ReleaseUndockSlot(objectID, shipID)
}

func undockSlotCount() int { return destiny.UndockLaneMaxSlots }

// undockSlotOffset turns a slot number into a position offset from the door. Slot 0 is the door, the point the
// server's own undock uses. Beyond that it is destiny.UndockLaneOffset, a lane stepping down the undock line, so
// every slot is further clear of the station than the last.
func undockSlotOffset(dir destiny.Vec3, slot int) destiny.Vec3 {
	return destiny.UndockLaneOffset(dir, slot)
}

func init() {
	registerSweep("undockGates", sweepUndockGates)
	registerSweep("watchedDoors", sweepWatchedDoors)
}
