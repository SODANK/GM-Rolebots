package rolebots

import (
	"context"
	"math"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
)

// Embodied navigation: how a projection flies. Everything a rolebot does on
// grid goes through here so it moves like a player's ship: it orbits what it
// is working on (weapon range picks the radius, with slack so drifting never
// breaks the beam) and it warps to the station or gate it is about to use
// instead of vanishing when the abstract tier moves it. The abstract sim still
// owns where a bot is; these calls hold its transition open while the ship
// flies the leg.

const (
	// Orbit radius as a fraction of the shortest weapon's optimal. The gap
	// up to 1.0 is the slack: an orbit wanders around its radius, and the
	// ship has to stay in range the whole way round.
	navOrbitFrac = 0.55
	// Hard ceiling on that radius: never circle at the edge of optimal.
	navOrbitMaxFrac = 0.8
	// Never orbit closer than this: rock and hull radii, plus jitter.
	navOrbitMinRange = 2_000.0
	// Re-issue the orbit once the ship is past this fraction of optimal (a rock
	// that shrank, a target that moved, an orbit knocked wide).
	navRangeSlack = 0.9

	// How long a projection gets to close on a target before it gives up
	// and takes the next one.
	navCloseMax  = 90 * time.Second
	navClosePoll = 2 * time.Second

	// The approach to a station or gate is budgeted from the hull's own speed, not
	// a flat clock: the burn is embodyLandingBuffer long, so a slow industrial needs
	// over two minutes. navApproachSlack covers spin-up and the arc a Follow flies.
	navApproachSlack = 2.5
	// Floor and ceiling on that budget: never give up faster than a fast
	// frigate needs, never wait out a ship that has plainly stopped.
	navApproachMin = 45 * time.Second
	navApproachMax = 240 * time.Second

	// The sim re-checks this often while it waits for a projection to
	// finish flying a leg.
	navLegHoldSec = 5
	// Pause on the far side of a gate someone can see, before aligning for
	// the next one. The abstract jumpLeg timers stand in only where nobody
	// is watching the ship actually fly.
	navGateDwellMinSec = 6
	navGateDwellMaxSec = 15

	// Warp drops a ship 15 km off its target's surface (embodyLandingBuffer is
	// added to the celestial's radius), as for a player. It burns in from there and
	// uses the gate or station only inside docking or jump range.
	navActivateRange = 2_500.0
	// Distance held while burning in: a surface distance, sent on the wire
	// unmodified, inside activation range so the approach settles.
	// See docs/comment-notes-b14.md#nav-surface-distance.
	navApproachHold = 1_800.0
	// Shortest warp anyone makes. Closer than this and the ship just burns
	// over, the same as a player who will not warp across a grid.
	navWarpMinRange = 150_000.0

	groupStar     = 6
	groupStargate = 10
)

// nav modes a projection can be commanded into (dedup key for the wire).
const (
	navModeNone = iota
	navModeOrbit
	navModeFollow
	// navModeGoto is a straight burn along a heading (embodyBurnHeading).
	navModeGoto
)

// navHeadingSameCos is how close two burn headings must be for the second to count as a repeat:
// cos 2°. A heading inside it is not re-sent, because every GotoDirection costs a stamped packet.
const navHeadingSameCos = 0.99939

// orbitRangeFor turns a weapon's optimal range into an orbit radius that
// keeps every shot inside optimal for the whole circle.
func orbitRangeFor(optimal float64) float64 {
	if optimal <= 0 {
		return navOrbitMinRange
	}
	r := optimal * navOrbitFrac
	if r < navOrbitMinRange {
		r = navOrbitMinRange
	}
	if ceiling := optimal * navOrbitMaxFrac; r > ceiling {
		r = ceiling
	}
	return r
}

// embodyOrbit puts a projection into a real orbit around targetID at dist
// metres and has every client on grid draw it. A repeat of the current orbit
// is dropped: the client re-derives its orbit plane on every Orbit command.
// dist is a surface distance and targetRadius the target's hull, as in embodyApproach.
func (m *Mod) embodyOrbit(eb *embodiedBot, targetID uint32, dist, targetRadius float64) {
	dm := eb.ent.Manager
	if dm == nil || eb.ss == nil {
		return
	}
	eb.mu.Lock()
	same := eb.navMode == navModeOrbit && eb.navTarget == targetID && math.Abs(eb.navDist-dist) < 1
	eb.navMode, eb.navTarget, eb.navDist = navModeOrbit, targetID, dist
	eb.mu.Unlock()
	if same {
		return
	}

	dm.SetSpeedFraction(1)
	dm.Orbit(targetID, targetRadius+dist)
	dm.NextStamp()

	ball := marshal.PyInt{Value: uint64(eb.ent.ID)}
	speed := destiny.BuildSimpleAction("SetSpeedFraction", ball, marshal.PyReal{Value: 1.0})
	orbit := destiny.BuildSimpleAction("Orbit", ball,
		marshal.PyInt{Value: uint64(targetID)}, marshal.PyReal{Value: dist})
	// Queued, not sent: one stamped packet per AI tick per projection runs a busy
	// grid's sim clock fast. The "move" key is shared with embodyApproach.
	m.queueForBubble(eb.ss, eb.ent.ID, eb.ent.ID,
		keyedAction{"ssf", speed}, keyedAction{"move", orbit})
}

// undockLoiterCircle puts a ship that has just left a station into a slow
// orbit for the length of its loiter instead of warping off at once. A ship
// parked 15 km off a station reads as broken; one flying a circle reads as
// waiting. embodyOrbit drops repeats, so a per-tick call costs one command.
func (m *Mod) undockLoiterCircle(eb *embodiedBot, stationID int64) {
	if stationID == 0 {
		return
	}
	for _, c := range eb.celestials {
		if c.ItemID == stationID {
			m.embodyOrbit(eb, uint32(c.ItemID),
				stationOrbitDist(eb.charID, c.ItemID), c.Radius)
			return
		}
	}
}

// How near a station a projection has to be before stationHoldCircle claims it.
// Tight, so gate campers are not pulled in: no gate is within 150 km of a
// station. Both circles draw from the same band below.
const (
	stationHoldRangeM = 150_000.0

	// The band a ship killing time outside a station circles in, cut into discrete
	// shells so a hub reads as clumps at different ranges. Surface distances, as
	// navApproachHold. See docs/comment-notes-b14.md#nav-station-shells.
	stationHoldOrbitMinM   = 3_000.0
	stationHoldOrbitMaxM   = 10_000.0
	stationHoldOrbitLayers = 5

	// Spread inside a layer, either side of its nominal radius. Small, so two
	// ships on one shell are not on the same circle; keep it under half the layer
	// step or the layers overlap.
	stationHoldOrbitJitterM = 300.0

	// How far a forming fleet member circles its commander (gangFormUpCircle).
	// Tighter than the station shells, so the pack reads as one object.
	formUpOrbitM = 5_000.0
)

// stationOrbitDist is what a pilot holding at a station circles it at: its
// shell of the band plus jitter. Deterministic in (charID, stationID), because
// a changing distance makes embodyOrbit re-issue Orbit and the client re-derive
// its orbit plane. Keyed on the station too, so one pilot varies shells.
func stationOrbitDist(charID, stationID int64) float64 {
	layers := uint64(stationHoldOrbitLayers)
	if layers < 1 {
		layers = 1
	}
	h := uint64(charID)*0x9E3779B97F4A7C15 ^ uint64(stationID)*0xC2B2AE3D27D4EB4F
	h ^= h >> 29
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 32

	// Shell nominals are inset from the band by the jitter, so it never needs
	// clamping: a clamp would pile the edge layers' jitter onto the band edge.
	lo := stationHoldOrbitMinM + stationHoldOrbitJitterM
	hi := stationHoldOrbitMaxM - stationHoldOrbitJitterM
	dist := lo
	if layers > 1 && hi > lo {
		dist += float64(h%layers) * ((hi - lo) / float64(layers-1))
	}
	// A second slice of the hash for the offset inside the shell, so layer and
	// offset do not move together.
	dist += (float64((h>>20)%2001)/1000 - 1) * stationHoldOrbitJitterM

	// Guard only: the inset above already keeps the roll inside the band.
	if dist < stationHoldOrbitMinM {
		dist = stationHoldOrbitMinM
	}
	if dist > stationHoldOrbitMaxM {
		dist = stationHoldOrbitMaxM
	}
	return dist
}

// stationHoldCircle puts a projection sitting still near a station into the
// same slow circle a loitering departure flies. Reports whether it found a
// station to circle. It covers ships holding to dock and fleet members holding
// for their op, which nothing else steers (Posture.holding(), task.go).
func (m *Mod) stationHoldCircle(eb *embodiedBot) bool {
	if eb.ent == nil {
		return false
	}
	pos := eb.ent.Pos()
	var (
		nearest evedb.CelestialEntity
		best    float64
		found   bool
	)
	for _, c := range eb.celestials {
		if c.GroupID != groupStation {
			continue
		}
		d := destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}.Sub(pos).Len()
		if d > stationHoldRangeM {
			continue
		}
		if !found || d < best {
			nearest, best, found = c, d, true
		}
	}
	if !found {
		return false
	}
	m.embodyOrbit(eb, uint32(nearest.ItemID),
		stationOrbitDist(eb.charID, nearest.ItemID), nearest.Radius)
	return true
}

// embodyApproach burns the projection in toward targetID and holds dist metres
// off it (the client's Approach, FollowBall). dist is a surface distance as the
// client means it; targetRadius is added only to the server's own follow (see
// navApproachHold). Pass 0 for a target with negligible radius.
func (m *Mod) embodyApproach(eb *embodiedBot, targetID uint32, dist, targetRadius float64) {
	dm := eb.ent.Manager
	if dm == nil || eb.ss == nil {
		return
	}
	eb.mu.Lock()
	same := eb.navMode == navModeFollow && eb.navTarget == targetID && math.Abs(eb.navDist-dist) < 1
	eb.navMode, eb.navTarget, eb.navDist = navModeFollow, targetID, dist
	eb.mu.Unlock()
	if same {
		return
	}

	dm.SetSpeedFraction(1)
	dm.Follow(targetID, targetRadius+dist)
	dm.NextStamp()

	ball := marshal.PyInt{Value: uint64(eb.ent.ID)}
	speed := destiny.BuildSimpleAction("SetSpeedFraction", ball, marshal.PyReal{Value: 1.0})
	follow := destiny.BuildSimpleAction("FollowBall", ball,
		marshal.PyInt{Value: uint64(targetID)}, marshal.PyReal{Value: dist})
	// Same "move" key as embodyOrbit.
	m.queueForBubble(eb.ss, eb.ent.ID, eb.ent.ID,
		keyedAction{"ssf", speed}, keyedAction{"move", follow})
}

// embodyBurnHeading flies the projection straight along dir at full speed (the client's
// double-click in space, GotoDirection) and has every client on grid draw it. A heading within
// 2° of the last one is dropped. The far point is scaled as the warp align's is
// (docs/comment-notes-b07.md#warp-align).
func (m *Mod) embodyBurnHeading(eb *embodiedBot, dir destiny.Vec3) {
	dm := eb.ent.Manager
	if dm == nil || eb.ss == nil || dir.IsZero() {
		return
	}
	dir = dir.Normalize()
	eb.mu.Lock()
	same := eb.navMode == navModeGoto && eb.navHeading.Dot(dir) >= navHeadingSameCos
	eb.navMode, eb.navTarget, eb.navDist, eb.navHeading = navModeGoto, 0, 0, dir
	eb.mu.Unlock()
	if same {
		return
	}

	far := eb.ent.Pos().Add(dir.Scale(embodyAlignDirScale))
	dm.SetSpeedFraction(1)
	dm.GotoDirection(far)
	dm.NextStamp()

	ball := marshal.PyInt{Value: uint64(eb.ent.ID)}
	speed := destiny.BuildSimpleAction("SetSpeedFraction", ball, marshal.PyReal{Value: 1.0})
	burn := destiny.BuildSimpleAction("GotoDirection", ball,
		marshal.PyReal{Value: far.X}, marshal.PyReal{Value: far.Y}, marshal.PyReal{Value: far.Z})
	// Same "move" key as embodyOrbit and embodyApproach, so the burn replaces a pending circle.
	m.queueForBubble(eb.ss, eb.ent.ID, eb.ent.ID,
		keyedAction{"ssf", speed}, keyedAction{"move", burn})
}

// clearNav forgets the last movement command, so the next one is sent even
// if it repeats the old one (a warp resets both ends).
func (eb *embodiedBot) clearNav() {
	eb.mu.Lock()
	eb.navMode, eb.navTarget, eb.navDist = navModeNone, 0, 0
	eb.navHeading = destiny.Vec3{}
	eb.mu.Unlock()
}

// closeTo orbits targetID and waits until it is within reach metres, which
// is where the modules can start working. Reports false when the loop was
// cancelled or the target never came into range.
func (m *Mod) closeTo(ctx context.Context, eb *embodiedBot, targetID uint32,
	targetPos destiny.Vec3, orbitDist, reach float64) bool {

	if eb.ent == nil || eb.ent.Manager == nil {
		return false
	}
	m.embodyOrbit(eb, targetID, orbitDist, 0)
	deadline := time.Now().Add(navCloseMax)
	for {
		if ctx.Err() != nil {
			return false
		}
		if eb.ent.Pos().Sub(targetPos).Len() <= reach {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		if !embodySleep(ctx, navClosePoll) {
			return false
		}
	}
}

// ── abstract-tier transitions, flown for real ───────────────────────────────

// embodyDockPrep flies a projection to the station its bot is about to dock
// at. Returns true while the ship is still on its way: the sim holds the dock
// open until then, so a watching player sees the bot arrive instead of
// blinking out where it was mining.
func (m *Mod) embodyDockPrep(charID, stationID int64) bool {
	eb := liveProjection(charID)
	if eb == nil {
		return false
	}
	return m.flyToCelestial(eb, stationID)
}

// embodyFlyToGate flies a projection to the stargate that leads to nextSysID.
// Returns true while it is still flying; false once parked at the gate, and
// false at once for a bot with no projection or gate (the abstract jump stands
// in). Split from embodyJumpFX so the gate-jump wave is asked between the two.
func (m *Mod) embodyFlyToGate(charID, nextSysID int64) bool {
	eb := liveProjection(charID)
	if eb == nil {
		return false
	}
	gate := gateTo(eb.celestials, nextSysID)
	if gate == nil {
		return false
	}
	return m.flyToCelestial(eb, gate.ItemID)
}

// embodyJumpFX plays the jump flash every client expects before a ball
// disappears through a gate. A no-op for a bot with no projection or no gate.
func (m *Mod) embodyJumpFX(charID, nextSysID int64) {
	eb := liveProjection(charID)
	if eb == nil {
		return
	}
	if gate := gateTo(eb.celestials, nextSysID); gate != nil {
		m.jumpOutFX(eb, uint32(gate.ItemID))
	}
}

// travelFightHoldMaxSec is the longest a trip waits on one fight. The grid clearing (attackers age
// out after combatAggroMemorySec) or the pilot breaking off ends nearly every fight long before it;
// the ceiling is for one that never settles, so it cannot strand the trip.
const travelFightHoldMaxSec = 180

// travelHoldsForFight is the pure rule behind projectionHoldsTravel: hold while the projection is
// engaged, which covers a break-off under fire too (the engagement stays up while it flees), but not
// past travelFightHoldMaxSec after the fight began. A pre-emptive flee, with nothing shooting, does
// not hold: its own latch (beltbail.go) settles it.
func travelHoldsForFight(fighting bool, since, now time.Time) bool {
	if !fighting {
		return false
	}
	if since.IsZero() {
		return true
	}
	return now.Sub(since) < travelFightHoldMaxSec*time.Second
}

// projectionHoldsTravel reports whether this pilot's projection is busy with a fight the trip
// should wait for. A pilot with no projection (abstract tier) never holds.
func (m *Mod) projectionHoldsTravel(charID int64) bool {
	eb := liveProjection(charID)
	if eb == nil {
		return false
	}
	eb.mu.Lock()
	fighting, since, dead := eb.fighting, eb.fightSince, eb.dead
	eb.mu.Unlock()
	if dead {
		return false
	}
	return travelHoldsForFight(fighting, since, time.Now())
}

// isEmbodied reports whether this bot currently has a projection on grid.
// The abstract tier uses it to leave the ship's coordinates alone: a live
// projection owns its own position and persists it when it lands.
func (m *Mod) isEmbodied(charID int64) bool {
	return liveProjection(charID) != nil
}

// embodyIfWatched gives this pilot a projection now if a real player is in the
// system to see it, and reports whether it ends up with one. It covers the
// approach to the dock station, so a system watched mid-trip does not see the
// ship appear at the door. Cheap when idle: no query.
func (m *Mod) embodyIfWatched(ctx context.Context, b *botRun) bool {
	if b == nil || b.ShipID == 0 {
		return false
	}
	if m.isEmbodied(b.CharID) {
		return true
	}
	// systemProjected, not systemWatched: a bot docking in a ring system has a ball
	// like any other, so the narrow predicate would leave its approach abstract.
	if !m.systemProjected(b.SystemID) {
		return false
	}
	m.embodyNow(ctx, b.CharID)
	return m.isEmbodied(b.CharID)
}

// systemWatched reports whether a real player has this system loaded, the only
// case where how a bot travels is something anyone can see. Narrower than
// systemProjected (preload.go); its one caller is the station dwell, which
// stretches so a departure a player could notice does not happen instantly.
func (m *Mod) systemWatched(sysID int64) bool {
	if m.systems == nil {
		return false
	}
	ss := m.systems.Get(sysID)
	return ss != nil && systemHasRealPlayer(ss)
}

func liveProjection(charID int64) *embodiedBot {
	embodyMu.Lock()
	defer embodyMu.Unlock()
	return embodyLive[charID]
}

// embodyDockClearanceWait files this projection's docking request with the
// station's traffic control (destiny/dockclearance.go, shared with players) and
// returns the seconds it still must wait; 0 means dock now. Only a landed
// projection asks, and filing again is idempotent.
func (m *Mod) embodyDockClearanceWait(charID, shipID, stationID int64) int64 {
	if shipID == 0 {
		return 0
	}
	eb := liveProjection(charID)
	if eb == nil {
		return 0
	}
	eb.mu.Lock()
	landed := !eb.warping && eb.currentDest == stationID
	eb.mu.Unlock()
	if !landed {
		return 0
	}
	now := time.Now()
	_, _, _, had := destiny.DockClearanceRequest(shipID)
	grant := destiny.RequestDockClearance(stationID, shipID, now)
	if !had {
		m.logBot(charID, logInfo, "embody", "docking clearance requested — granted in %.1fs",
			grant.Sub(now).Seconds())
	}
	if !grant.After(now) {
		return 0
	}
	return int64(math.Ceil(grant.Sub(now).Seconds()))
}

// flyToCelestial takes a projection to one celestial in its own system the way
// a player gets there: warp in (15 km off), then burn the rest and count as
// arrived inside activation range. Reports whether it is still travelling.
func (m *Mod) flyToCelestial(eb *embodiedBot, itemID int64) bool {
	if itemID == 0 {
		return false
	}
	target := celestialByID(eb.celestials, itemID)
	if target == nil {
		return false // not in this system's map: let the sim move it
	}

	eb.mu.Lock()
	warping, warped := eb.warping, eb.currentDest == itemID
	eb.mu.Unlock()
	if warping {
		return true
	}
	if !warped {
		center := destiny.Vec3{X: target.X, Y: target.Y, Z: target.Z}
		if eb.ent.Pos().Sub(center).Len() > navWarpMinRange {
			// Hold, then warp (fleetmove.go): a commanded member on its commander's grid
			// waits for the commander's warp. Still on its way: the leg holds and re-asks.
			if m.fleetHoldsForFC(eb, itemID) {
				return true
			}
			// Lasers off and the rock forgotten: this ship is leaving.
			m.stopEmbodyMining(eb)
			eb.setMineTarget(0)
			// The warp is warpDiscretionary: a refused warp is retried by the sim every
			// navLegHoldSec, after a jittered hold. See docs/comment-notes-b14.md#nav-warp-discretionary.
			eb.mu.Lock()
			hold := time.Now().Before(eb.warpAskAfter)
			eb.mu.Unlock()
			if hold {
				return true
			}
			if !m.startEmbodyWarpTo(eb, *target, warpDiscretionary) {
				// Nothing was commanded, so no warp is logged (that log feeds the traffic mix).
				// Still on its way: the leg holds and re-asks after a jittered delay.
				eb.mu.Lock()
				eb.warpAskAfter = time.Now().Add(warpRetryDelay())
				eb.mu.Unlock()
				return true
			}
			m.logBot(eb.charID, logInfo, "embody", "warping to %s", celestialLabel(*target))
			return true
		}
		// Already on top of it: nobody warps 40 km, they just burn over.
		m.stopEmbodyMining(eb)
		eb.setMineTarget(0)
		eb.mu.Lock()
		eb.currentDest = target.ItemID
		eb.mu.Unlock()
	}
	return m.burnToActivation(eb, *target)
}

// burnToActivation closes the last 15 km under propulsion and reports
// whether the ship is still short of the range it can dock or jump at.
func (m *Mod) burnToActivation(eb *embodiedBot, target evedb.CelestialEntity) bool {
	if eb.ent == nil {
		return false
	}
	center := destiny.Vec3{X: target.X, Y: target.Y, Z: target.Z}
	surface := eb.ent.Pos().Sub(center).Len() - target.Radius

	eb.mu.Lock()
	started := eb.approachID == target.ItemID
	if !started {
		eb.approachID, eb.approachSince = target.ItemID, time.Now()
	}
	since := eb.approachSince
	eb.mu.Unlock()

	if surface <= navActivateRange {
		eb.mu.Lock()
		eb.approachID = 0
		eb.mu.Unlock()
		return false
	}
	budget := approachBudget(eb)
	if started && time.Since(since) > budget {
		// Something keeps the ship off the object (webbed, wrong landing, zero
		// velocity). Let the abstract tier finish the move rather than wedge the bot.
		m.logBot(eb.charID, logError, "embody",
			"could not close on %s (%.1fkm off after %.0fs) — completing the move anyway",
			celestialLabel(target), surface/1000, budget.Seconds())
		eb.mu.Lock()
		eb.approachID = 0
		eb.mu.Unlock()
		return false
	}
	if !started {
		m.logBot(eb.charID, logInfo, "embody",
			"approaching %s — %.1fkm to go, activating inside %.1fkm",
			celestialLabel(target), surface/1000, navActivateRange/1000)
	}
	m.embodyApproach(eb, uint32(target.ItemID), navApproachHold, target.Radius)
	return true
}

// approachBudget is how long this hull is given to fly the landing buffer
// in. A flat clock is the wrong unit for it: the leg is a fixed distance and
// the ships flying it differ by an order of magnitude in speed.
func approachBudget(eb *embodiedBot) time.Duration {
	speed := 0.0
	if eb.ent != nil && eb.ent.Manager != nil {
		// Through the Entity, not the Manager: the system tick and any
		// active web are writing that number from their own goroutines.
		speed = eb.ent.EffectiveMaxVelocity()
	}
	if speed <= 0 {
		return navCloseMax
	}
	d := time.Duration((embodyLandingBuffer / speed) * navApproachSlack * float64(time.Second))
	if d < navApproachMin {
		return navApproachMin
	}
	if d > navApproachMax {
		return navApproachMax
	}
	return d
}

func celestialByID(cels []evedb.CelestialEntity, itemID int64) *evedb.CelestialEntity {
	for i := range cels {
		if cels[i].ItemID == itemID {
			return &cels[i]
		}
	}
	return nil
}

// gateTo finds the stargate in this system that leads to sysID.
func gateTo(cels []evedb.CelestialEntity, sysID int64) *evedb.CelestialEntity {
	for i := range cels {
		if cels[i].GroupID == groupStargate && cels[i].DestSolarSystemID == sysID {
			return &cels[i]
		}
	}
	return nil
}

func celestialLabel(c evedb.CelestialEntity) string {
	if c.Name != "" {
		return c.Name
	}
	return "celestial"
}

// gateActivityFX plays the jump-in flash on a gate for everyone on grid.
// effects.GateActivity animates the ball named by its FIRST argument, so
// that argument is the gate, not the arriving ship.
func (m *Mod) gateActivityFX(eb *embodiedBot, gateID uint32) {
	if eb.ss == nil || eb.ent == nil {
		return
	}
	action := destiny.BuildSimpleAction("OnSpecialFX",
		marshal.PyInt{Value: uint64(gateID)},
		marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
		marshal.PyList{},
		marshal.PyString{Value: "effects.GateActivity"},
		marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
	)
	for _, mate := range eb.ss.GetBubbleConnectedPlayersExcept(eb.ent.ID) {
		if mate.Conn == nil {
			continue
		}
		_ = mate.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionListEvents(mate.Conn, m.nodeID, mate.AccountID, stamp,
				nil, action)
		})
	}
}

// jumpOutFX plays effects.JumpOut on the gate for everyone on grid, the same
// flash a player's ship sends just before its ball is removed. Without it the
// bot blinks out on their screens.
func (m *Mod) jumpOutFX(eb *embodiedBot, gateID uint32) {
	if eb.ss == nil || eb.ent == nil {
		return
	}
	action := destiny.BuildSimpleAction("OnSpecialFX",
		marshal.PyInt{Value: uint64(eb.ent.ID)}, // source: the departing ship
		marshal.PyNone{},                        // moduleID
		marshal.PyNone{},                        // moduleTypeID
		marshal.PyInt{Value: uint64(gateID)},    // target: the gate that flashes
		marshal.PyNone{},                        // otherTypeID
		marshal.PyList{},                        // area
		marshal.PyString{Value: "effects.JumpOut"},
		marshal.PyInt{Value: 0}, // isOffensive
		marshal.PyInt{Value: 1}, // start
		marshal.PyInt{Value: 0}, // active
	)
	for _, mate := range eb.ss.GetBubbleConnectedPlayersExcept(eb.ent.ID) {
		if mate.Conn == nil || !mate.KnowsBall(eb.ent.ID) {
			continue
		}
		_ = mate.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionListEvents(mate.Conn, m.nodeID, mate.AccountID, stamp,
				nil, action)
		})
	}
}

// undockBoosterFX lights a bot's engines at undock (the core's sendBoosterFX
// for a projection), since the client's own booster trigger never fires for a
// server-sim undock. Ship balls only. shipBallCategoryID is invCategories 6.
const shipBallCategoryID = 6

func (m *Mod) undockBoosterFX(eb *embodiedBot) {
	if eb == nil || eb.ss == nil || eb.ent == nil || eb.ent.Slim.CategoryID != shipBallCategoryID {
		return
	}
	// Same 10-param OnSpecialFX shape as sendModuleFX. targetID and otherTypeID
	// must be None, never 0: fxSequencer.AddTrigger drops the trigger otherwise.
	action := destiny.BuildSimpleAction("OnSpecialFX",
		marshal.PyInt{Value: uint64(eb.ent.ID)},
		marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
		marshal.PyList{},
		marshal.PyString{Value: dogma.FXGUIDBoosterShort},
		marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
	)
	for _, mate := range eb.ss.GetBubbleConnectedPlayersExcept(eb.ent.ID) {
		if mate.Conn == nil || !mate.KnowsBall(eb.ent.ID) {
			continue
		}
		_ = mate.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionListEvents(mate.Conn, m.nodeID, mate.AccountID, stamp,
				nil, action)
		})
	}
}
