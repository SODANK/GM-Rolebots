package rolebots

import (
	"context"
	"sync"
	"time"

	"gurimoon/destiny"
	"gurimoon/rates"
)

// The quiet-grid hold and the siege bash on a watched grid (docs/FLEET_DOCTRINE.md section 2.3,
// docs/STARBASES.md "Watched siege"). A fleet whose orders name a place in this system
// (FleetCommand.HoldAt: a siege's tower, a camp's gate) holds there while its commander has no
// fight on: every member but the scout warps to HoldAt and orbits HoldBall at HoldRangeM.
//
// A siege attacker shoots the tower while the siege is in its bash or kill stage (siegeBashLive).
// Every gun cycle lands its damage on the tower entity, so anyone on grid sees the shield fall and
// the tower reinforce at its floor. A hit lands at any range: it is the bash's damage made visible,
// not a turret roll. Pilots shooting here are left out of the off-grid bash (siegeGridShooting).
// The fight with defenders is the combat loop's, which takes the ship as soon as it engages.

const (
	// fleetHoldWarpM: a member further than this off the hold celestial's surface warps to it.
	fleetHoldWarpM = 150_000.0
	// fleetHoldTickGap spaces one member's hold checks.
	fleetHoldTickGap = 3 * time.Second
)

// fleetHold is a fleet's hold point as the grid reads it.
type fleetHold struct {
	At     int64  // the celestial to warp to
	Ball   uint32 // the ball to orbit; 0 orbits At
	RangeM float64
	Bash   bool
}

// fleetHoldPoint is the hold point of this member's fleet on this grid, read off the commander's
// projection (the commander reads its own). False when the fleet has no command here or no hold,
// when the commander is in a fight (the grid is not quiet), and for a scout, which sits its gate.
func fleetHoldPoint(eb *embodiedBot) (fleetHold, bool) {
	if eb == nil || eb.gangRole == RoleScout {
		return fleetHold{}, false
	}
	lead := eb
	if !eb.isFleetCommander() {
		lead = fleetLeaderLive(eb)
	}
	if lead == nil || lead.fleetCmd == nil {
		return fleetHold{}, false
	}
	lead.mu.Lock()
	cmd := lead.fleetCmd
	h := fleetHold{At: cmd.HoldAt, Ball: uint32(cmd.HoldBall), RangeM: cmd.HoldRangeM, Bash: cmd.HoldBash}
	busy := lead.call.engaged || lead.fighting || lead.call.retreat
	lead.mu.Unlock()
	if h.At == 0 || busy {
		return fleetHold{}, false
	}
	return h, true
}

// fleetHoldTick flies a member to its fleet's hold point and keeps it there. It reports whether it
// owned the projection this tick. Called from escortTick for a projection with no commander of its
// own, ahead of the form-up circle and the idle wander.
func (m *Mod) fleetHoldTick(eb *embodiedBot, now time.Time) bool {
	h, ok := fleetHoldPoint(eb)
	eb.mu.Lock()
	busy := eb.dead || eb.warping || eb.fighting || eb.fleeing ||
		eb.mineCancel != nil || eb.mineTarget != 0 || eb.approachID != 0
	seen := eb.escortSeenAt
	eb.mu.Unlock()
	if !ok || busy || eb.ent == nil || eb.ss == nil {
		m.siegeFXStop(eb)
		return false
	}
	if now.Sub(seen) < fleetHoldTickGap {
		return true
	}
	eb.mu.Lock()
	eb.escortSeenAt = now
	eb.mu.Unlock()

	c := celestialByID(eb.celestials, h.At)
	if c == nil {
		m.siegeFXStop(eb)
		return false
	}
	if (destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}).Sub(eb.ent.Pos()).Len()-c.Radius > fleetHoldWarpM {
		m.siegeFXStop(eb)
		if !m.startEmbodyWarpTo(eb, *c, warpDiscretionary) {
			m.deferWarp(eb)
		}
		return true
	}
	ballID, radius := uint32(c.ItemID), c.Radius
	var ball *destiny.Entity
	if h.Ball != 0 {
		if e := eb.ss.GetEntity(h.Ball); e != nil && !e.IsDestroyed() {
			ball, ballID, radius = e, e.ID, 0
			if e.Manager != nil {
				radius = e.Manager.Radius()
			}
		}
	}
	m.embodyOrbit(eb, ballID, h.RangeM, radius)
	if ball != nil && h.Bash && siegeBashLive(int64(h.Ball), clock.Unix()) {
		m.siegeFire(eb, ball)
	} else {
		m.siegeFXStop(eb)
	}
	return true
}

// ── the bash ────────────────────────────────────────────────────────────────

// siegeGridShotSec is how long after its last hit on a tower a pilot counts as shooting on grid.
const siegeGridShotSec = 120

// siegeGridShots is the unix second each pilot last hit a sieged tower on grid.
var (
	siegeGridShotMu sync.Mutex
	siegeGridShots  = map[int64]int64{}
)

// noteSiegeGridShot records a pilot's hit on a sieged tower.
func noteSiegeGridShot(charID, now int64) {
	siegeGridShotMu.Lock()
	siegeGridShots[charID] = now
	siegeGridShotMu.Unlock()
}

// siegeGridShooting reports whether a pilot hit a sieged tower on grid in the last
// siegeGridShotSec. The off-grid bash leaves such a pilot's damage to the grid.
func siegeGridShooting(charID, now int64) bool {
	siegeGridShotMu.Lock()
	defer siegeGridShotMu.Unlock()
	at, ok := siegeGridShots[charID]
	return ok && now-at <= siegeGridShotSec
}

// siegeGridReachM is how far from a sieged tower a projection counts as on its grid.
const siegeGridReachM = 250_000.0

// siegeOnGrid reports whether a pilot has a live projection in sysID, within siegeGridReachM of
// the tower when tower is set. In a projected system a pilot in space with no projection is
// nowhere a watcher can see, so the desk does not count it (siegePresent).
func siegeOnGrid(charID, sysID int64, tower *destiny.Entity) bool {
	embodyMu.Lock()
	eb := embodyLive[charID]
	embodyMu.Unlock()
	if eb == nil || eb.systemID != sysID || eb.ent == nil {
		return false
	}
	eb.mu.Lock()
	gone := eb.dead
	eb.mu.Unlock()
	if gone {
		return false
	}
	return tower == nil || eb.ent.Pos().Sub(tower.Pos()).Len() <= siegeGridReachM
}

// siegeFX is one projection's own copy of its guns for the bash, apart from the combat loop's
// (eb.load), so the two never share a weapon's firing state.
type siegeFX struct {
	eb      *embodiedBot // the projection the copy was loaded for; a new one reloads
	load    *combatLoadout
	loading bool
}

var (
	siegeFXMu sync.Mutex
	siegeFXOf = map[int64]*siegeFX{}
)

func init() {
	registerCharForget("siegeFX", func(id int64) {
		siegeFXMu.Lock()
		delete(siegeFXOf, id)
		siegeFXMu.Unlock()
		siegeGridShotMu.Lock()
		delete(siegeGridShots, id)
		siegeGridShotMu.Unlock()
	})
}

// siegeFire cycles every gun on the tower and lands each cycle's damage on it. A gun short of
// capacitor skips a cycle; a gun out of ammunition stops for an hour, as in the combat loop. The
// first call loads the guns off the grid tick and fires on a later one.
func (m *Mod) siegeFire(eb *embodiedBot, tower *destiny.Entity) {
	siegeFXMu.Lock()
	fx := siegeFXOf[eb.charID]
	if fx == nil || fx.eb != eb {
		fx = &siegeFX{eb: eb}
		siegeFXOf[eb.charID] = fx
	}
	l, loading := fx.load, fx.loading
	if l == nil && !loading {
		fx.loading = true
	}
	siegeFXMu.Unlock()
	if l == nil {
		if !loading {
			m.goSafeBot(eb.charID, "siege fx", func() { m.loadSiegeFX(eb, fx) })
		}
		return
	}
	if tower.IsInvulnerable() || tower.IsDestroyed() {
		m.siegeFXStop(eb)
		return
	}
	now := time.Now()
	for _, w := range l.weapons {
		if now.Before(w.nextFireAt) {
			continue
		}
		cycle := time.Duration(w.CycleSec * float64(time.Second))
		if !l.drawCap(w.CapNeed) {
			m.stopWeaponFire(eb, w)
			w.nextFireAt = now.Add(cycle)
			continue
		}
		if !m.consumeCharge(eb, w) {
			m.stopWeaponFire(eb, w)
			w.nextFireAt = now.Add(time.Hour)
			continue
		}
		w.nextFireAt = now.Add(cycle)
		m.startWeaponFire(eb, w, tower.ID)
		em, exp, kin, therm := w.shotDamage()
		r := rates.PlayerDamage
		applied, destroyed := tower.ApplyDamage(em*r, exp*r, kin*r, therm*r)
		noteSiegeGridShot(eb.charID, clock.Unix())
		if m.landDamage(eb, tower, applied, destroyed, w.TypeID) {
			m.siegeFXStop(eb)
			return
		}
	}
}

// loadSiegeFX reads the projection's guns for the bash.
func (m *Mod) loadSiegeFX(eb *embodiedBot, fx *siegeFX) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var shipType int64
	if eb.ent != nil {
		shipType = eb.ent.Slim.TypeID
	}
	l, err := m.loadCombatLoadout(ctx, eb.charID, eb.shipID, shipType)
	siegeFXMu.Lock()
	defer siegeFXMu.Unlock()
	fx.loading = false
	if err == nil {
		fx.load = l
	}
}

// siegeFXStop silences the bash's guns, if any are cycling.
func (m *Mod) siegeFXStop(eb *embodiedBot) {
	if eb == nil {
		return
	}
	siegeFXMu.Lock()
	var l *combatLoadout
	if fx := siegeFXOf[eb.charID]; fx != nil && fx.eb == eb {
		l = fx.load
	}
	siegeFXMu.Unlock()
	if l == nil {
		return
	}
	for _, w := range l.weapons {
		m.stopWeaponFire(eb, w)
	}
}
