package rolebots

import (
	"context"
	"math"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// A human commander's movement orders, flown on the grid: flee, align, warp to, orbit,
// approach, keep at range and defend (docs/FLEET_DOCTRINE.md §2.7).
//
// The order lives in memory per pilot (moveCalls), written when the line is parsed. Any other
// order but a fire order drops it, and so does a Warp Gang. Flee and warp to start once and
// hold where they land; align, orbit, approach and keep at range are re-asserted every escort
// tick, and the combat loop leaves the ship's motion alone while one stands (steersShip).

// Default ranges when the line gave none.
const (
	fcOrbitDefaultM    = 2500.0
	fcApproachDefaultM = 500.0
	fcKeepDefaultM     = 10000.0
)

// moveCall is one pilot's standing movement order.
type moveCall struct {
	Verb    string
	Subject int64   // a pilot named or linked: who to orbit, approach, guard
	DestID  int64   // a linked place: what to align or warp to
	RangeM  float64 // -1: none given
	At      int64
	Done    bool // a one-shot order has started
	Said    bool // the one refusal line has been said
}

var (
	moveCallMu sync.Mutex
	moveCalls  = map[int64]moveCall{}
	// lastAligned is the celestial a pilot last aligned to, for a bare "warp" after "align".
	lastAligned = map[int64]int64{}
)

func moveCallOf(charID int64) (moveCall, bool) {
	moveCallMu.Lock()
	defer moveCallMu.Unlock()
	c, ok := moveCalls[charID]
	return c, ok
}

func setMoveCall(charID int64, c moveCall) {
	moveCallMu.Lock()
	moveCalls[charID] = c
	moveCallMu.Unlock()
}

func markMoveCall(charID int64, f func(*moveCall)) {
	moveCallMu.Lock()
	defer moveCallMu.Unlock()
	if c, ok := moveCalls[charID]; ok {
		f(&c)
		moveCalls[charID] = c
	}
}

func dropMoveCall(charID int64) {
	moveCallMu.Lock()
	delete(moveCalls, charID)
	moveCallMu.Unlock()
}

func init() {
	registerCharForget("moveCalls", func(id int64) {
		moveCallMu.Lock()
		delete(moveCalls, id)
		delete(lastAligned, id)
		moveCallMu.Unlock()
	})
}

// isMoveVerb reports whether an order is one of the movement orders this file flies.
func isMoveVerb(verb string) bool {
	switch verb {
	case OrderFlee, OrderAlign, OrderWarpTo, OrderOrbit, OrderApproach, OrderKeepRange, OrderDefend:
		return true
	}
	return false
}

// steersShip reports whether a standing movement order owns the ship's motion, so the combat
// loop's range-keeping stays off it.
func (eb *embodiedBot) steersShip() bool {
	if eb == nil {
		return false
	}
	c, ok := moveCallOf(eb.charID)
	if !ok {
		return false
	}
	switch c.Verb {
	case OrderAlign, OrderOrbit, OrderApproach, OrderKeepRange:
		return true
	}
	return false
}

// fcMoveTick flies a standing movement order. It reports whether it owned this tick.
func (m *Mod) fcMoveTick(eb *embodiedBot, c moveCall) bool {
	if eb.ent == nil || eb.ss == nil {
		return true
	}
	eb.mu.Lock()
	warping := eb.warping
	eb.mu.Unlock()
	if warping {
		return true
	}
	switch c.Verb {
	case OrderFlee:
		m.fcFlee(eb, c)
	case OrderAlign:
		m.fcAlign(eb, c)
	case OrderWarpTo:
		m.fcWarpTo(eb, c)
	case OrderOrbit, OrderApproach, OrderKeepRange:
		m.fcKeep(eb, c)
	case OrderDefend:
		// Guard a named pilot: stay with them. Without one, the commander.
		who := c.Subject
		if who == 0 {
			eb.mu.Lock()
			who = eb.bossID
			eb.mu.Unlock()
		}
		if mate := m.pilotBall(eb, who); mate != nil {
			m.warpToPilot(eb, mate)
		}
	default:
		return false
	}
	return true
}

// fcFlee warps off grid once: a station first, else any celestial out of the commander's
// grid. A scrambled ship says so once and fights on.
func (m *Mod) fcFlee(eb *embodiedBot, c moveCall) {
	if c.Done {
		return
	}
	if eb.ent.WarpScrambleTotal() > 0 {
		if !c.Said {
			markMoveCall(eb.charID, func(mc *moveCall) { mc.Said = true })
			m.sayServing(eb, "Pointed — can't get out.")
		}
		return
	}
	cel := fleeCelestial(eb.ent.Pos(), eb.celestials)
	if cel == nil {
		return
	}
	if m.startEmbodyWarpTo(eb, *cel, warpMandatory) {
		markMoveCall(eb.charID, func(mc *moveCall) { mc.Done = true })
		m.logBot(eb.charID, logInfo, "conscript", "fleeing to %s", celestialLabel(*cel))
	}
}

// fleeCelestial is where a fleeing ship warps: the nearest station off grid, else the nearest
// celestial off grid. Pure.
func fleeCelestial(self destiny.Vec3, cels []evedb.CelestialEntity) *evedb.CelestialEntity {
	var station, other *evedb.CelestialEntity
	sd, od := math.MaxFloat64, math.MaxFloat64
	for i := range cels {
		c := &cels[i]
		d := destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}.Sub(self).Len()
		if d < escortWarpFloor || c.GroupID == 6 { // on grid, or the sun
			continue
		}
		if c.GroupID == groupStation && d < sd {
			station, sd = c, d
		}
		if d < od {
			other, od = c, d
		}
	}
	if station != nil {
		return station
	}
	return other
}

// fcAlign turns toward a linked celestial, or away from the hostiles ("align out").
func (m *Mod) fcAlign(eb *embodiedBot, c moveCall) {
	var cel *evedb.CelestialEntity
	if c.DestID != 0 {
		if cel = celestialByID(eb.celestials, c.DestID); cel == nil {
			if !c.Said {
				markMoveCall(eb.charID, func(mc *moveCall) { mc.Said = true })
				m.sayServing(eb, "That's not in this system.")
			}
			return
		}
	} else {
		cel = alignOutPick(eb.ent.Pos(), m.hostileCentroid(eb), eb.celestials)
	}
	if cel == nil {
		return
	}
	moveCallMu.Lock()
	lastAligned[eb.charID] = cel.ItemID
	moveCallMu.Unlock()
	m.embodyBurnHeading(eb, destiny.Vec3{X: cel.X, Y: cel.Y, Z: cel.Z}.Sub(eb.ent.Pos()))
}

// hostileCentroid is the mean position of what is shooting the fleet, or the ship's own
// position when nothing is.
func (m *Mod) hostileCentroid(eb *embodiedBot) destiny.Vec3 {
	self := eb.ent.Pos()
	var sum destiny.Vec3
	n := 0
	for _, id := range m.fleetAggressors(eb) {
		if e := eb.ss.GetEntity(id); e != nil {
			sum = sum.Add(e.Pos())
			n++
		}
	}
	if n == 0 {
		return self
	}
	return sum.Scale(1 / float64(n))
}

// alignOutPick is the off-grid celestial most nearly opposite the hostiles. With no hostiles
// (centroid on the ship) it is the nearest off-grid celestial. Pure.
func alignOutPick(self, hostile destiny.Vec3, cels []evedb.CelestialEntity) *evedb.CelestialEntity {
	away := self.Sub(hostile)
	var best *evedb.CelestialEntity
	bestScore := math.Inf(-1)
	for i := range cels {
		c := &cels[i]
		to := destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}.Sub(self)
		d := to.Len()
		if d < escortWarpFloor || c.GroupID == 6 {
			continue
		}
		var score float64
		if away.IsZero() {
			score = -d
		} else {
			score = to.Normalize().Dot(away.Normalize())
		}
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}

// fcWarpTo warps once: to a linked place at the range asked, to a named pilot, or, with
// nothing named, to what the pilot last aligned to.
func (m *Mod) fcWarpTo(eb *embodiedBot, c moveCall) {
	if c.Done {
		return
	}
	if c.Subject != 0 {
		if mate := m.pilotBall(eb, c.Subject); mate != nil {
			m.warpToPilot(eb, mate)
			markMoveCall(eb.charID, func(mc *moveCall) { mc.Done = true })
		}
		return
	}
	dest := c.DestID
	if dest == 0 {
		moveCallMu.Lock()
		dest = lastAligned[eb.charID]
		moveCallMu.Unlock()
	}
	cel := celestialByID(eb.celestials, dest)
	if cel == nil {
		if !c.Said {
			markMoveCall(eb.charID, func(mc *moveCall) { mc.Said = true })
			if dest == 0 {
				m.sayServing(eb, "Warp where? Link it in chat.")
			} else {
				m.sayServing(eb, "That's not in this system.")
			}
		}
		return
	}
	target := *cel
	if c.RangeM >= 0 {
		target.Radius = warpRadius(cel.Radius, c.RangeM)
	}
	if m.startEmbodyWarpTo(eb, target, warpMandatory) {
		markMoveCall(eb.charID, func(mc *moveCall) { mc.Done = true })
		m.logBot(eb.charID, logInfo, "conscript", "warping to %s at %.0fkm", celestialLabel(*cel), c.RangeM/1000)
	}
}

// warpRadius is the Radius a bot warp needs to land rangeM off a celestial's surface: the warp
// stops Radius + embodyLandingBuffer from the centre. Clamped at 0. Pure.
func warpRadius(celRadius, rangeM float64) float64 {
	r := celRadius + rangeM - embodyLandingBuffer
	if r < 0 {
		return 0
	}
	return r
}

// fcKeep is orbit, approach and keep at range. The ball is the pilot named, else the
// commander's primary tag, else a linked place, else the commander.
func (m *Mod) fcKeep(eb *embodiedBot, c moveCall) {
	eb.mu.Lock()
	boss := eb.bossID
	eb.mu.Unlock()
	var ball uint32
	var radius float64
	if c.Subject != 0 {
		if e := m.pilotBall(eb, c.Subject); e != nil {
			ball = e.ID
		}
	}
	if ball == 0 && c.DestID != 0 {
		if cel := celestialByID(eb.celestials, c.DestID); cel != nil {
			ball, radius = uint32(cel.ItemID), cel.Radius
		}
	}
	if ball == 0 && c.Subject == 0 && c.DestID == 0 {
		if k := conscriptOf(eb.charID); k != nil {
			for _, id := range fcTagged(k.GangID) {
				if e := eb.ss.GetEntity(id); e != nil && !e.IsDestroyed() {
					ball = id
					break
				}
			}
		}
		if ball == 0 {
			if e := m.pilotBall(eb, boss); e != nil {
				ball = e.ID
			}
		}
	}
	if ball == 0 {
		return
	}
	dist := c.RangeM
	switch c.Verb {
	case OrderOrbit:
		if dist < 0 {
			dist = fcOrbitDefaultM
		}
		m.embodyOrbit(eb, ball, dist, radius)
	case OrderApproach:
		if dist < 0 {
			dist = fcApproachDefaultM
		}
		m.embodyApproach(eb, ball, dist, radius)
	case OrderKeepRange:
		if dist < 0 {
			dist = fcKeepDefaultM
		}
		// Keep at range is an approach that holds at the distance; the nav call stops closing there.
		m.embodyApproach(eb, ball, dist, radius)
	}
}

// sayServing is a one-line answer from a pilot to the fleet it serves.
func (m *Mod) sayServing(eb *embodiedBot, line string) {
	k := conscriptOf(eb.charID)
	if k == nil {
		return
	}
	charID, gangID := eb.charID, k.GangID
	goSafe("fc move say", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.sayFleet(ctx, gangID, charID, true, "%s", line)
	})
}
