package rolebots

import (
	"math/rand"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/gang"
	"gurimoon/server/gangsta"
)

// The embodied half of serving in a player's fleet: staying with the commander, shooting what
// they pointed at, and stepping in when somebody opens up on the fleet unasked.
//
// This is the wall-clock tier. It runs in front of a person watching, so it is on the
// clockExemptFiles list beside embody.go and combat_embodied.go.

const (
	// escortHoldRange is how close a servant tries to stay: far enough not to bump the commander,
	// close enough to share a grid through a warp and land guns on the same target.
	escortHoldRange = 2500.0
	// escortWarpFloor is the distance past which closing means warping rather than burning. Below
	// it a sub-warp approach arrives sooner than the four-phase warp.
	escortWarpFloor = 150_000.0
	// escortTickGap paces the escort look. Re-deciding every second would cancel a warp already
	// in flight.
	escortTickGap = 4 * time.Second
	// escortLandingBuffer is how far short of the commander a warp lands. The protocol has no
	// member-relative landing, so this stops "warp to me" arriving inside the commander.
	escortLandingBuffer = 5000.0
)

// ── warp to a pilot ─────────────────────────────────────────────────────────

// warpToPilot is as close to "warp to fleet member" as this server has. Every module warp
// goes to an evedb.CelestialEntity, so a pilot is expressed as a synthetic one (position, radius
// 0, ID). It is the same warp a bot flies anywhere, and the landing buffer does the spacing.
func (m *Mod) warpToPilot(eb *embodiedBot, mate *destiny.Entity) bool {
	if eb == nil || mate == nil || eb.ent == nil {
		return false
	}
	pos := mate.Pos()
	dist := pos.Sub(eb.ent.Pos()).Len()

	// Already on the grid: burn, do not warp. A 40 km warp is drawn as a stutter.
	if dist < escortWarpFloor {
		m.embodyApproach(eb, mate.ID, escortHoldRange, 0)
		return true
	}
	// Skip the hop when already warping to this destination: without it each escortTickGap would
	// issue a fresh mandatory hop that forces a stamp on every witness.
	eb.mu.Lock()
	already := eb.warping || eb.currentDest == int64(mate.ID)
	eb.mu.Unlock()
	if already {
		return true
	}
	m.startEmbodyWarpTo(eb, evedb.CelestialEntity{
		// The commander's own ball ID. currentDest is compared against it above, so a real ID here
		// stops the escort tick re-commanding the same warp.
		ItemID: int64(mate.ID),
		X:      pos.X, Y: pos.Y, Z: pos.Z,
		Radius: escortLandingBuffer,
		Name:   mate.CharName,
		// warpMandatory: a direct response to an order a player typed, who pays the stamp.
	}, warpMandatory)
	return true
}

// ── gang warp ───────────────────────────────────────────────────────────────

// gangWarpGridRange is how far from the commander a servant may be and still be carried by
// their gang warp. The core uses the same bubble radius for its own pilots.
const gangWarpGridRange = destiny.BubbleRadius

// gangWarpStagger spreads the servants' warp starts over a moment, so a crew leaves as a
// stream (the gangFollowWarp convention).
const (
	gangWarpStaggerMinMs = 150
	gangWarpStaggerMaxMs = 900
)

// conscriptGangWarp is the module's half of a player's "Warp Gang" (gang.ModuleWarp). The core
// warps its connected pilots and hands over every gang member without a socket. Each servant
// on the commander's grid warps to the same destination at the same exit distance. It runs on
// the commander's connection goroutine: a snapshot plus a goroutine per servant, never a wait.
func (m *Mod) conscriptGangWarp(o gang.WarpOrder) {
	embodyMu.Lock()
	crew := make([]*embodiedBot, 0, len(o.Members))
	for _, id := range o.Members {
		// Only this gang's own crew: a listed bot serving nobody here is not the commander's to move.
		if k := conscriptOf(id); k == nil || k.GangID != o.GangID {
			continue
		}
		if eb := embodyLive[id]; eb != nil {
			crew = append(crew, eb)
		}
	}
	embodyMu.Unlock()
	m.warpCrewAlong(o, crew, func(eb *embodiedBot) bool {
		eb.mu.Lock()
		verb := eb.orderVerb
		eb.mu.Unlock()
		// Told to be somewhere else; the commander's warp is not that.
		return gangWarpCarries(verb)
	})
}

// warpCrewAlong warps every listed projection on the leader's grid to the gang warp's
// destination, staggered, skipping the dead, the elsewhere, the already warping, the
// scrambled and any carries refuses.
func (m *Mod) warpCrewAlong(o gang.WarpOrder, crew []*embodiedBot, carries func(*embodiedBot) bool) {
	leaderPos := destiny.Vec3{X: o.LeaderX, Y: o.LeaderY, Z: o.LeaderZ}
	// The bot warp lands Radius + embodyLandingBuffer short of the point, the commander's lands
	// ExitDist short. Folding the difference into Radius equalises them; negative is clamped.
	radius := o.ExitDist - embodyLandingBuffer
	if radius < 0 {
		radius = 0
	}
	target := evedb.CelestialEntity{
		ItemID: o.TargetID,
		X:      o.X, Y: o.Y, Z: o.Z,
		Radius: radius,
	}

	for _, eb := range crew {
		eb := eb
		eb.mu.Lock()
		dead, warping, sysID := eb.dead, eb.warping, eb.systemID
		eb.mu.Unlock()
		if dead || eb.ent == nil || sysID != o.SystemID {
			continue // abstract, elsewhere, or gone: not on this grid
		}
		if warping {
			continue // already on a warp of its own; re-commanding it cancels that
		}
		if !carries(eb) {
			continue
		}
		if eb.ent.Pos().Sub(leaderPos).Len() > gangWarpGridRange {
			continue
		}
		if eb.ent.WarpScrambleTotal() > 0 {
			m.logBot(eb.charID, logInfo, "conscript", "gang warp from %d: warp scrambled — staying", o.LeaderID)
			continue
		}
		delay := time.Duration(gangWarpStaggerMinMs+rand.Intn(gangWarpStaggerMaxMs-gangWarpStaggerMinMs)) * time.Millisecond
		m.goSafeBot(eb.charID, "gang warp", func() {
			time.Sleep(delay)
			eb.mu.Lock()
			mining := eb.mineCancel != nil || eb.mineTarget != 0
			eb.mu.Unlock()
			if mining {
				m.stopEmbodyMining(eb)
			}
			m.fleetHoldRelease(eb)
			// The commander moved the fleet: a standing align, orbit or flee is over.
			dropMoveCall(eb.charID)
			// Mandatory: the commander gave the order, the same class warpToPilot uses.
			if m.startEmbodyWarpTo(eb, target, warpMandatory) {
				m.logBot(eb.charID, logInfo, "conscript", "gang warp from %d to %d", o.LeaderID, o.TargetID)
			}
		})
	}
}

// gangWarpCarries reports whether a servant's standing order lets the commander's gang warp
// take it along: no order yet, or mining the commander's belt, goes with the fleet. Stay,
// dock, the errands and the flight home do not.
func gangWarpCarries(verb string) bool {
	switch verb {
	case OrderStay, OrderDock, OrderStockpile, OrderSell, OrderHaul, OrderGoHome:
		return false
	}
	return true
}

// ── the escort tick ─────────────────────────────────────────────────────────

// escortTick keeps a servant with its commander. Called from embodyTick for any projection
// carrying an order, ahead of the idle wander, which cannot move it (PostureEscort holds).
func (m *Mod) escortTick(eb *embodiedBot, now time.Time) bool {
	eb.mu.Lock()
	boss, verb := eb.bossID, eb.orderVerb
	busy := eb.dead || eb.warping || eb.fighting || eb.fleeing ||
		eb.mineCancel != nil || eb.mineTarget != 0
	seen := eb.escortSeenAt
	eb.mu.Unlock()

	// A movement order (fcmove_embodied.go) flies through a fight: orbit and flee are what a
	// commander says in one.
	if mc, ok := moveCallOf(eb.charID); ok && boss != 0 {
		if now.Sub(seen) < escortTickGap {
			return true
		}
		eb.mu.Lock()
		eb.escortSeenAt = now
		eb.mu.Unlock()
		return m.fcMoveTick(eb, mc)
	}

	if boss == 0 {
		// No commander of its own: a fleet's hold point may still own the ship (siege_embodied.go).
		return m.fleetHoldTick(eb, now)
	}
	if !escortVerbFollows(verb) {
		return false
	}
	if busy || now.Sub(seen) < escortTickGap {
		return busy || boss != 0
	}
	eb.mu.Lock()
	eb.escortSeenAt = now
	eb.mu.Unlock()

	mate := m.pilotBall(eb, boss)
	if mate == nil {
		// The commander is not in this system (docked, jumped out or logged off). The abstract escort
		// step re-routes and the release sweep gives up.
		return true
	}
	m.warpToPilot(eb, mate)
	return true
}

// escortVerbFollows reports whether this order means "be where the commander is". The
// exceptions send the pilot elsewhere, and warping back mid-delivery would stop the errand.
func escortVerbFollows(verb string) bool {
	switch verb {
	case "", OrderStay, OrderDock, OrderStockpile, OrderSell, OrderHaul,
		OrderMine, OrderPickOre, OrderJetcan, OrderGoHome, OrderFlee,
		OrderAlign, OrderWarpTo, OrderOrbit, OrderApproach, OrderKeepRange:
		return false
	}
	return true
}

// pilotBall finds a character's ship on this projection's own grid.
func (m *Mod) pilotBall(eb *embodiedBot, charID int64) *destiny.Entity {
	if eb == nil || eb.ss == nil || charID == 0 {
		return nil
	}
	e := eb.ss.GetPlayerByCharID(charID)
	if e == nil || e.IsDestroyed() {
		return nil
	}
	return e
}

// ── who a servant shoots ────────────────────────────────────────────────────

// commandedTarget is the target the commander called, if it is on grid and shootable
// (pickCommanded, fctags.go). It overrides the scorer. It returns 0 when nothing was called or
// the target is gone, and the ordinary scorer takes over.
func (m *Mod) commandedTarget(eb *embodiedBot, hostiles []uint32) uint32 {
	return pickCommanded(m.commandInputsFor(eb, hostiles))
}

// commandInputsFor gathers what the commander has called for this pilot. Tags apply under
// every order but the errands; a linked pilot, a named NPC, the commander's locks and the
// fleet's attackers only under attack or tackle (the "select it and say attack" order: the
// lock list is the only place that intent exists).
func (m *Mod) commandInputsFor(eb *embodiedBot, hostiles []uint32) commandInputs {
	eb.mu.Lock()
	boss, verb, subject := eb.bossID, eb.orderVerb, eb.orderTargetChar
	eb.mu.Unlock()
	var in commandInputs
	k := conscriptOf(eb.charID)
	if boss == 0 || k == nil || eb.ss == nil || !verbTakesTags(verb) {
		return in
	}
	call := fireCallOf(eb.charID)
	in.Slot = call.Slot
	may := func(e *destiny.Entity) bool {
		return e != nil && !e.IsDestroyed() && !e.IsCloaked() && m.conscriptMayEngage(eb, e)
	}
	keep := func(ids []uint32) []uint32 {
		var out []uint32
		for _, id := range ids {
			if id != 0 && may(eb.ss.GetEntity(id)) {
				out = append(out, id)
			}
		}
		return out
	}
	in.Tagged = keep(fcTagged(k.GangID))
	if verb != OrderAttack && verb != OrderTackle {
		return in
	}
	// A linked pilot wins, when they are here.
	if subject != 0 {
		if e := m.pilotBall(eb, subject); e != nil {
			in.Subject = e.ID
		}
	}
	in.Named = m.namedOnGrid(eb, call.Name, may)
	in.Locked = keep(bossLockedTargets(boss))
	in.Aggressors = keep(hostiles)
	return in
}

// commandedHostiles is every ball the commander called on this grid, so a call starts a fight
// the projection would not have started itself (hostilesOnGrid).
func (m *Mod) commandedHostiles(eb *embodiedBot) []uint32 {
	return m.commandInputsFor(eb, nil).called()
}

// conscriptMayEngage is the law, applied per ball. NPCs are always fair game. A player is
// not: opening on a clean pilot in empire space brings CONCORD and flags the fleet.
func (m *Mod) conscriptMayEngage(eb *embodiedBot, e *destiny.Entity) bool {
	if e == nil {
		return false
	}
	if !e.IsPlayer {
		return true
	}
	// Fleetmates are never targets, whatever the commander typed.
	charID := e.Slim.CharID
	if charID == eb.charID || charID == eb.bossID {
		return false
	}
	if k := conscriptOf(eb.charID); k != nil {
		if crew := crewOf(k.GangID); crew != nil {
			for _, mate := range crew.Members {
				if mate.CharID == charID {
					return false
				}
			}
		}
		// Nor is any human in the fleet.
		if gangsta.GangOf(charID) == k.GangID {
			return false
		}
	}
	return mayShootPilot(loadedSystemSecurity(eb.systemID),
		criminalSince(eb.systemID, charID), false)
}

// holdingFire reports whether this projection's guns have been ordered cold. The fire state
// set when the order was parsed (fctags.go) wins over the state blob's flag.
func (eb *embodiedBot) holdingFire() bool {
	if eb == nil {
		return false
	}
	eb.mu.Lock()
	boss, held := eb.bossID, eb.orderHoldFire
	eb.mu.Unlock()
	if boss == 0 {
		return false
	}
	if c := fireCallOf(eb.charID); c.Known {
		return c.HoldFire
	}
	return held
}

// orderedTackleOnly reports whether this projection is holding a target down rather than
// killing it. The guns are cold, as under hold-fire, because a tackler is doing its job.
func (eb *embodiedBot) orderedTackleOnly() bool {
	if eb == nil {
		return false
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	return eb.bossID != 0 && eb.orderVerb == OrderTackle
}

// ── defending the fleet ─────────────────────────────────────────────────────

// fleetAggressors is everything shooting at somebody this projection serves with: the
// autonomous half of the feature. The evidence is the damage ledger, read off the fleetmate's
// entity. The roster comes from the in-memory cache and the commander's entity, never a query,
// because this runs inside the one-second combat loop.
func (m *Mod) fleetAggressors(eb *embodiedBot) []uint32 {
	if eb == nil || eb.ss == nil {
		return nil
	}
	eb.mu.Lock()
	boss := eb.bossID
	eb.mu.Unlock()
	if boss == 0 || !ConscriptDefence() {
		return nil
	}

	k := conscriptOf(eb.charID)
	if k == nil {
		return nil
	}
	mates := []int64{k.BossCharID}
	// "Guard <pilot>" puts that pilot's attackers first.
	if mc, ok := moveCallOf(eb.charID); ok && mc.Verb == OrderDefend && mc.Subject != 0 {
		mates = append([]int64{mc.Subject}, mates...)
	}
	if crew := crewOf(k.GangID); crew != nil {
		for _, other := range crew.Members {
			if other.CharID != eb.charID {
				mates = append(mates, other.CharID)
			}
		}
	}

	seen := map[uint32]bool{}
	var out []uint32
	for _, mateID := range mates {
		mate := m.pilotBall(eb, mateID)
		if mate == nil {
			continue
		}
		for _, hit := range mate.KillHitsSnapshot() {
			if hit.Time < time.Now().Unix()-combatAggroMemorySec {
				continue
			}
			// attackerEntityID matches the ledger entry back onto this grid, the only way an NPC hit
			// resolves to a ball (a KillHit has the ship type and character, no entity ID).
			id, ok := eb.attackerEntityID(hit)
			if !ok || id == 0 || id == eb.ent.ID || seen[id] {
				continue
			}
			// A fleetmate's own guns are not an aggressor, and neither is anything the law forbids
			// touching: an order to defend does not suspend CONCORD.
			e := eb.ss.GetEntity(id)
			if e == nil || !m.conscriptMayEngage(eb, e) {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
		if m.npc != nil {
			for _, id := range m.npc.GetTargetersOf(mate.ID) {
				if id == 0 || seen[id] || id == eb.ent.ID {
					continue
				}
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// loadedSystemSecurity is a system's truesec, read from the universe graph only if it is
// already in memory. It avoids m.universe(ctx), which loads five thousand rows on a miss, since
// this runs in the one-second combat loop. A miss answers 1.0, the strictest reading, which
// fails toward not shooting a player.
func loadedSystemSecurity(sysID int64) float64 {
	u := uniOverride
	if u == nil {
		u = uni
	}
	if u == nil {
		return 1.0
	}
	ms, ok := u.meta[sysID]
	if !ok {
		return 1.0
	}
	return dispSec(ms.Security)
}
