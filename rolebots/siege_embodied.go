package rolebots

import (
	"context"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
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
		// No commander on this grid: the member's own copy of the siege hold (loadFleetSeat).
		return ownSiegeHold(eb)
	}
	lead.mu.Lock()
	cmd := lead.fleetCmd
	h := fleetHold{At: cmd.HoldAt, Ball: uint32(cmd.HoldBall), RangeM: cmd.HoldRangeM, Bash: cmd.HoldBash}
	busy := lead.call.engaged || lead.fighting || lead.call.retreat
	lead.mu.Unlock()
	if busy {
		return fleetHold{}, false
	}
	if h.At == 0 {
		// A commander embodied before the siege reached this system holds nothing; its line still does.
		return ownSiegeHold(eb)
	}
	return h, true
}

// ownSiegeHold is a member's own siege hold, when it has one.
func ownSiegeHold(eb *embodiedBot) (fleetHold, bool) {
	if eb.siegeHold.At == 0 {
		return fleetHold{}, false
	}
	return eb.siegeHold, true
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
		// A tower's batteries go first, then the tower (siegeShotTarget).
		m.siegeFire(eb, m.siegeShotTarget(eb, ball))
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
	eb       *embodiedBot // the projection the copy was loaded for; a new one reloads
	load     *combatLoadout
	loading  bool
	loadedAt time.Time // when load was read; an empty or old copy is read again (siegeFXStale)
	// The pilot's hits since its last log line (siegeHitLogEvery).
	hitDmg   float64
	hitShots int
	hitLogAt time.Time
}

const (
	// siegeFXEmptyRetry is how soon a copy with no guns in it is read again: the reload from the
	// hold (gunreload.go) runs on the read, so a pilot that came onto the grid dry picks its guns up.
	siegeFXEmptyRetry = time.Minute
	// siegeFXRefresh is how long any copy stands before it is read again, so a refit or a round
	// swapped in by a reload reaches the bash.
	siegeFXRefresh = 10 * time.Minute
	// siegeHitLogEvery spaces one pilot's "hitting the structure" log lines.
	siegeHitLogEvery = 2 * time.Minute
	// siegeGridLogEvery spaces the server log's per-structure summary of the grid's fire.
	siegeGridLogEvery = time.Minute
)

// siegeFXStale reports whether a copy of the guns should be read again. Pure.
func siegeFXStale(weapons int, loadedAt, now time.Time) bool {
	if weapons == 0 {
		return now.Sub(loadedAt) >= siegeFXEmptyRetry
	}
	return now.Sub(loadedAt) >= siegeFXRefresh
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
		siegeBashMu.Lock()
		delete(siegeParked, id)
		siegeBashMu.Unlock()
	})
}

// siegeFire cycles every gun on the tower and lands each cycle's damage on it. A gun short of
// capacitor skips a cycle; a gun out of ammunition stops for an hour, as in the combat loop. The
// first call loads the guns off the grid tick and fires on a later one.
func (m *Mod) siegeFire(eb *embodiedBot, tower *destiny.Entity) {
	now := time.Now()
	siegeFXMu.Lock()
	fx := siegeFXOf[eb.charID]
	if fx == nil || fx.eb != eb {
		fx = &siegeFX{eb: eb}
		siegeFXOf[eb.charID] = fx
	}
	if fx.load != nil && !fx.loading && siegeFXStale(len(fx.load.weapons), fx.loadedAt, now) {
		// Read the guns again: the read reloads empty ones from the hold first.
		old := fx.load
		fx.load = nil
		siegeFXMu.Unlock()
		for _, w := range old.weapons {
			m.stopWeaponFire(eb, w)
		}
		siegeFXMu.Lock()
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
			// The load ran out: reload from the hold and rest the gun a reload's length.
			if m.reloadGunFromHold(eb, w) {
				w.nextFireAt = now.Add(gunReloadDelay)
				continue
			}
			w.nextFireAt = now.Add(time.Hour)
			m.logBot(eb.charID, logWarn, "siege", "%s is out of ammunition, hold included", w.Name)
			continue
		}
		w.nextFireAt = now.Add(cycle)
		m.startWeaponFire(eb, w, tower.ID)
		em, exp, kin, therm := w.shotDamage()
		r := rates.PlayerDamage
		applied, destroyed := tower.ApplyDamage(em*r, exp*r, kin*r, therm*r)
		noteSiegeGridShot(eb.charID, clock.Unix())
		m.noteSiegeGridHit(eb, fx, tower, applied, now)
		if m.landDamage(eb, tower, applied, destroyed, w.TypeID) {
			m.siegeFXStop(eb)
			return
		}
	}
}

// siegeGridFire is the grid's fire on one structure since its last server log line.
type siegeGridFire struct {
	dmg      float64
	shooters map[int64]bool
	since    time.Time
}

var (
	siegeGridFireMu sync.Mutex
	siegeGridFireOf = map[uint32]*siegeGridFire{}
)

// noteSiegeGridHit books one hit on a sieged structure: the pilot's own log line every
// siegeHitLogEvery, and the server log's summary of the whole grid's fire every
// siegeGridLogEvery. A bash on a watched grid otherwise leaves no trace but the shield bar.
func (m *Mod) noteSiegeGridHit(eb *embodiedBot, fx *siegeFX, target *destiny.Entity, applied float64, now time.Time) {
	if target == nil || applied <= 0 {
		return
	}
	name := target.CharName
	if name == "" {
		name = fmt.Sprintf("structure %d", target.ID)
	}
	shield := 100.0
	if target.HP.MaxShield > 0 {
		shield = 100 * target.HP.CurShield / target.HP.MaxShield
	}

	siegeFXMu.Lock()
	fx.hitDmg += applied
	fx.hitShots++
	var pilotLine string
	if fx.hitLogAt.IsZero() {
		fx.hitLogAt = now
	} else if now.Sub(fx.hitLogAt) >= siegeHitLogEvery {
		pilotLine = fmt.Sprintf("hitting %s: %d shot(s), %.0f damage in the last %s — shield at %.1f%%",
			name, fx.hitShots, fx.hitDmg, now.Sub(fx.hitLogAt).Round(time.Second), shield)
		fx.hitDmg, fx.hitShots, fx.hitLogAt = 0, 0, now
	}
	siegeFXMu.Unlock()
	if pilotLine != "" {
		m.logBot(eb.charID, logInfo, "siege", "%s", pilotLine)
	}

	siegeGridFireMu.Lock()
	g := siegeGridFireOf[target.ID]
	if g == nil {
		g = &siegeGridFire{shooters: map[int64]bool{}, since: now}
		siegeGridFireOf[target.ID] = g
	}
	g.dmg += applied
	g.shooters[eb.charID] = true
	var gridLine string
	if now.Sub(g.since) >= siegeGridLogEvery {
		gridLine = fmt.Sprintf("rolebots: siege grid %s (%d) in %d: %d shooter(s), %.0f damage in %s, shield %.1f%%",
			name, target.ID, eb.systemID, len(g.shooters), g.dmg, now.Sub(g.since).Round(time.Second), shield)
		delete(siegeGridFireOf, target.ID)
	}
	siegeGridFireMu.Unlock()
	if gridLine != "" {
		rlogf("%s", gridLine)
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
	fx.loadedAt = time.Now()
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

// ── the batteries first ─────────────────────────────────────────────────────

// siegeBatteryReachM is how far from a sieged tower an anchored battery counts as its defence.
const siegeBatteryReachM = 75_000.0

// siegeBatteryEvery is how long one pick of a tower's next battery stands, so the line keeps one
// primary instead of re-reading the structures on every gun cycle.
const siegeBatteryEvery = 5 * time.Second

// siegeBatteryRank is the order a siege line clears a tower's batteries in: what breaks its locks
// and holds it first (ECM, scram, web, damp), then the guns. -1 for anything that is not a
// battery (a silo, a lab, an array). Pure.
func siegeBatteryRank(groupID int64) int {
	switch groupID {
	case 439: // ECM battery
		return 0
	case 443: // warp scrambling battery
		return 1
	case 441: // stasis webification battery
		return 2
	case 440: // sensor dampening battery
		return 3
	}
	if evedb.IsSentryGroup(groupID) {
		return 4
	}
	return -1
}

// batteryCand is one anchored battery a siege line could shoot.
type batteryCand struct {
	ID    uint32
	Group int64
	DistM float64 // from the tower
}

// pickSiegeBattery is the battery the whole line shoots next: the highest rank (siegeBatteryRank)
// within reachM of the tower, the lower ID on a tie, so every member picks the same one. Pure.
func pickSiegeBattery(cands []batteryCand, reachM float64) (uint32, bool) {
	best := -1
	for i, c := range cands {
		r := siegeBatteryRank(c.Group)
		if r < 0 || c.DistM > reachM {
			continue
		}
		if best < 0 {
			best = i
			continue
		}
		br := siegeBatteryRank(cands[best].Group)
		if r < br || (r == br && c.ID < cands[best].ID) {
			best = i
		}
	}
	if best < 0 {
		return 0, false
	}
	return cands[best].ID, true
}

var (
	siegeBatteryMu  sync.Mutex
	siegeBatteryFor = map[uint32]struct {
		id uint32
		at time.Time
	}{}
)

// siegeShotTarget is what a siege line shoots on a watched tower grid: the tower owner's anchored
// batteries first (pickSiegeBattery), then the tower. A station, and a tower with no battery
// left in reach, is its own target.
func (m *Mod) siegeShotTarget(eb *embodiedBot, tower *destiny.Entity) *destiny.Entity {
	if eb == nil || eb.ss == nil || tower == nil || !eb.ss.IsStructureEntity(tower.ID) {
		return tower
	}
	now := time.Now()
	siegeBatteryMu.Lock()
	pick, ok := siegeBatteryFor[tower.ID]
	siegeBatteryMu.Unlock()
	if !ok || now.Sub(pick.at) >= siegeBatteryEvery {
		var cands []batteryCand
		for _, e := range eb.ss.SnapshotStructureEntities() {
			if e == nil || e.ID == tower.ID || e.IsDestroyed() || e.IsInvulnerable() ||
				e.Slim.OwnerID != tower.Slim.OwnerID {
				continue
			}
			cands = append(cands, batteryCand{ID: e.ID, Group: e.Slim.GroupID, DistM: e.Pos().Sub(tower.Pos()).Len()})
		}
		id, _ := pickSiegeBattery(cands, siegeBatteryReachM)
		pick = struct {
			id uint32
			at time.Time
		}{id, now}
		siegeBatteryMu.Lock()
		siegeBatteryFor[tower.ID] = pick
		siegeBatteryMu.Unlock()
	}
	if pick.id != 0 {
		if e := eb.ss.GetEntity(pick.id); e != nil && !e.IsDestroyed() {
			return e
		}
	}
	return tower
}
