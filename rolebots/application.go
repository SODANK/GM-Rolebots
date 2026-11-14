package rolebots

import (
	"math"

	"gurimoon/dogma"
)

// Weapon application in an unwatched fight. The embodied tier rolls every turret shot against
// tracking, range and signature (turretShot, combat_embodied.go); the abstract tier used to land
// every gun's nominal damage. This file is the abstract tier's expectation of the same rolls, per
// gun, at the range and transverse the embodied loop would fly. Drones land at full damage in both
// tiers. See docs/ROLEBOT_DESIGN.md §21.2.

// gunProfile is one fitted weapon reduced to what application needs. DPS is its nominal damage a
// second as botCombatant counts it (cycleDamage over CycleSec at the server rate).
type gunProfile struct {
	DPS       float64
	Missile   bool
	Optimal   float64
	Falloff   float64
	Tracking  float64
	WeaponSig float64 // the gun's signature resolution
	ExpRadius float64 // a missile's explosion radius (attribute 654), 0 when unread
	ExpVel    float64 // a missile's explosion velocity (attribute 653), 0 when unread
}

// gunProfileOf reads one loaded weapon. dps is the caller's nominal figure for it.
func gunProfileOf(w *botWeapon, dps float64) gunProfile {
	return gunProfile{
		DPS: dps, Missile: w.IsMissile,
		Optimal: w.Optimal, Falloff: w.Falloff, Tracking: w.Tracking, WeaponSig: w.WeaponSig,
		ExpRadius: w.ExpRadius, ExpVel: w.ExpVel,
	}
}

// turretExpectedMod is the mean of dogma.TurretDamageMod over a uniform roll at hit chance p, the
// roll turretShot makes. Pure. A hundredth of the hits wreck at x3, the rest land roll + 0.49:
// 0.03p + 0.49(0.99p) + 0.5(p² − (0.01p)²), about 0.515p + 0.5p², and 1.015 at p = 1.
func turretExpectedMod(p float64) float64 {
	p = math.Max(0, math.Min(1, p))
	wreck := 0.01 * p
	return 3*wreck + 0.49*(p-wreck) + 0.5*(p*p-wreck*wreck)
}

// turretApplication is the expected damage modifier of one turret at dist metres against a target
// of targetSig crossing at transverse m/s. Pure. The chance is the core's dogma.TurretHitChance,
// the formula turretShot restates.
func turretApplication(g gunProfile, dist, transverse, targetSig float64) float64 {
	angular := 0.0
	if dist > 0 && transverse > 0 {
		angular = transverse / dist
	}
	p := dogma.TurretHitChance(dist, g.Optimal, g.Falloff, angular, g.Tracking, targetSig, g.WeaponSig)
	return turretExpectedMod(p)
}

// missileApplication is the era's missile damage formula: min(1, sig/expRad, (sig/expRad) ×
// (expVel/speed)). Pure. An unread explosion radius or target signature applies in full, and a
// still target loses only to the radius term.
func missileApplication(g gunProfile, targetSig, targetSpeed float64) float64 {
	if g.ExpRadius <= 0 || targetSig <= 0 {
		return 1
	}
	r := targetSig / g.ExpRadius
	app := math.Min(1, r)
	if targetSpeed > 0 && g.ExpVel > 0 {
		app = math.Min(app, r*g.ExpVel/targetSpeed)
	}
	return app
}

// fightGeometry is where the abstract tier puts a hull against its target: the range the embodied
// loop holds (holdRangeFor, and holdCombatRange's two-thirds of flight range for a missile
// brawler) and the transverse it sees. Pure. holdOptM is loadoutHoldOptimal's figure. A brawler
// or holder orbits, so the target's whole speed is transverse; a kiter keeps range in a line, so
// only the speed difference is.
func fightGeometry(st combatStance, reachM, holdOptM float64, allMissile bool, selfSpeed, targetSpeed float64) (dist, transverse float64) {
	optimal := holdOptM
	if allMissile {
		optimal = 0
	}
	dist = holdRangeFor(st, reachM, optimal)
	if allMissile && st == stanceBrawl && holdOptM > 0 {
		dist = holdOptM * 0.66
	}
	transverse = math.Max(targetSpeed, 0)
	if st == stanceKite {
		transverse = math.Abs(selfSpeed - targetSpeed)
	}
	return dist, transverse
}

// loadoutHoldOptimal is the figure the embodied loop's holdCombatRange flies on: the shortest
// turret optimal or missile flight range, 10 km when nothing reads, and whether every weapon is a
// launcher. It restates that function's loop (combat_embodied.go); change both together.
func loadoutHoldOptimal(l *combatLoadout) (shortest float64, allMissile bool) {
	shortest, allMissile = math.MaxFloat64, true
	if l == nil {
		return 10_000, true
	}
	for _, w := range l.weapons {
		if !w.IsMissile {
			allMissile = false
			if w.Optimal > 0 && w.Optimal < shortest {
				shortest = w.Optimal
			}
		}
		if w.IsMissile && w.MaxRange < shortest {
			shortest = w.MaxRange
		}
	}
	if shortest == math.MaxFloat64 {
		shortest = 10_000
	}
	return shortest, allMissile
}

// appliedDPS is this hull's damage a second against a target of targetSig moving at targetSpeed,
// flying stance st. Each gun lands its nominal damage times its application; whatever else DPS
// holds (drones) lands in full. Without gun profiles (a test fixture, or the figure already
// applied) it is DPS.
func (c *abstractCombatant) appliedDPS(st combatStance, targetSig, targetSpeed float64) float64 {
	if c == nil {
		return 0
	}
	if len(c.Guns) == 0 {
		return c.DPS
	}
	dist, transverse := fightGeometry(st, c.ReachM, c.HoldOptM, c.AllMissile, c.SpeedMS, targetSpeed)
	rest := c.DPS
	applied := 0.0
	for _, g := range c.Guns {
		rest -= g.DPS
		if g.Missile {
			applied += g.DPS * missileApplication(g, targetSig, targetSpeed)
		} else {
			applied += g.DPS * turretApplication(g, dist, transverse, targetSig)
		}
	}
	return math.Max(rest, 0) + applied
}

// dpsOn is c's applied damage a second on t in a bot-versus-bot fight, flying the stance it would
// take against t's speed.
func (c *pvpCombatant) dpsOn(t *pvpCombatant) float64 {
	if c == nil || c.abstractCombatant == nil {
		return 0
	}
	if t == nil || t.abstractCombatant == nil {
		return c.DPS
	}
	st := doctrineStance(c.ReachM, c.SpeedMS, t.SpeedMS)
	return c.appliedDPS(st, t.SigRadius, t.SpeedMS)
}

// applyAgainstSpawn folds the hull's guns into DPS against a rat spawn, once, before a duel: the
// application against each living rat weighted by its share of the spawn's hitpoints, since that
// is where the damage goes. The gun profiles are cleared so the figure is not applied twice.
func (c *abstractCombatant) applyAgainstSpawn(spawn []*ratThreat) {
	if c == nil || len(c.Guns) == 0 {
		return
	}
	totalEHP, sum := 0.0, 0.0
	for _, r := range spawn {
		if r == nil || r.EHP <= 0 {
			continue
		}
		totalEHP += r.EHP
		sum += r.EHP * c.appliedDPS(c.Stance, r.SigRadius, r.SpeedMS)
	}
	if totalEHP > 0 {
		c.DPS = sum / totalEHP
	}
	c.Guns = nil
}
