package rolebots

import (
	"context"
	"math"
	"sync/atomic"

	evedb "gurimoon/db"
)

// How a bot fights, and whether it fights at all.
//
// How: the stance (brawl, hold or kite) is read from the fit and the hull, so reach is something
// a fit can use. Whether: the decision to stay in a fight follows what the pilot came out to do
// (Posture), not whether a gun is fitted.
//
// Everything here is pure (no database, no destiny, no *Mod). Both combat tiers call it, so one
// rule serves both and the harness pins it without a server.

// combatStance is how a hull holds its distance in a fight. It has three values, not the two of
// fitLean (fitreach.go): a long-gunned hull that cannot outrun its target wants to sit at
// optimal, neither diving inside it nor trying to keep a range it cannot hold.
type combatStance int

const (
	// stanceBrawl closes and orbits inside optimal, the right answer for short guns. It is the
	// zero value so anything that fails to classify flies a brawler's circle.
	stanceBrawl combatStance = iota
	// stanceHold sits at optimal without closing further: long guns on a hull too slow to dictate
	// the range.
	stanceHold
	// stanceKite holds a range outside the spawn's reach. It needs a hull genuinely faster than
	// what it fights (stanceFor).
	stanceKite
)

func (s combatStance) String() string {
	switch s {
	case stanceHold:
		return "hold"
	case stanceKite:
		return "kite"
	}
	return "brawl"
}

const (
	// stanceKiteSpeedMargin is how much faster than the spawn a hull must be to kite. The hull
	// has to reopen the gap every time the target turns inside it, and 1.25 is the smallest ratio
	// where it recovers over a belt engagement. Few hulls clear it; raise speed, not this number.
	stanceKiteSpeedMargin = 1.25

	// stanceKiteHoldFrac is where inside its own reach a kiter sits. Reach is optimal plus two
	// falloffs, so the last fifth is weak falloff, and an orbit wanders around its radius.
	stanceKiteHoldFrac = 0.8

	// stanceHoldFrac is where a long-gunned hull that cannot kite sits: at optimal, with
	// navOrbitMaxFrac's slack against the wander. It is the ceiling, not orbitRangeFor's fraction,
	// which would throw away standing the guns could keep.
	stanceHoldFrac = navOrbitMaxFrac
)

// stanceFor reads the manoeuvre off the fit and the hull. reachM is FitStats.WeaponReachM, the
// shortest armed weapon's reach. spawnSpeedMS is what it fights; zero means unknown and
// resolves to the slow answer, so a hull never kites something unmeasured.
func stanceFor(reachM, hullSpeedMS, spawnSpeedMS float64) combatStance {
	// No guns, or a rack whose range could not be read: the brawl default.
	if reachM <= 0 {
		return stanceBrawl
	}
	// Short guns are a brawler however fast the hull. reachWorthKnee is the 15 km line
	// fitreach.go draws, where the belt population's own guns end.
	if reachM < reachWorthKnee {
		return stanceBrawl
	}
	if spawnSpeedMS > 0 && hullSpeedMS >= spawnSpeedMS*stanceKiteSpeedMargin {
		return stanceKite
	}
	return stanceHold
}

// holdRangeFor is the distance a stance wants to keep, in metres off the target's hull.
// optimalM is the shortest armed weapon's optimal, reachM its full envelope. A brawler is
// orbitRangeFor(optimal), which is what holdCombatRange flies.
func holdRangeFor(st combatStance, reachM, optimalM float64) float64 {
	switch st {
	case stanceKite:
		if reachM > 0 {
			return reachM * stanceKiteHoldFrac
		}
	case stanceHold:
		if optimalM > 0 {
			// Never closer than the brawler's answer: a short autocannon with long falloff can clear the
			// kite knee on 1.2 km of optimal, and 0.8 of that is inside navOrbitMinRange.
			return math.Max(orbitRangeFor(optimalM), optimalM*stanceHoldFrac)
		}
		if reachM > 0 {
			// A missile boat has no optimal, the envelope is all of it: hold it as a kiter would.
			return math.Max(navOrbitMinRange, reachM*stanceKiteHoldFrac)
		}
	}
	return orbitRangeFor(optimalM)
}

// fights reports whether a pilot in this posture came out to have a fight. It is a question
// about intent, not armament: a hauler with a rocket launcher is still a hauler. PostureEscort
// fights, since a bodyguard that warped off when its charge was shot is no bodyguard.
// PostureLieLow does not: standing and fighting would lengthen its criminal timer.
func (p Posture) fights() bool {
	switch p {
	case PostureCombatBelt, PostureCombatPatrol, PostureCombatGate,
		PostureAmbushBelt, PostureAmbushGate, PostureEscort:
		return true
	}
	return false
}

// spawnTopSpeed is the fastest thing in a pack, what a hull has to beat to hold a range. It is
// the fastest, not the average, because the first rat to arrive breaks the standoff.
func spawnTopSpeed(spawn []ratReach) float64 {
	top := 0.0
	for _, r := range spawn {
		// A rat that throws nothing is not what the standoff is held against (as rangeAdvantage).
		if r.DPS <= 0 {
			continue
		}
		if r.SpeedMS > top {
			top = r.SpeedMS
		}
	}
	return top
}

// ── the switch ──────────────────────────────────────────────────────────────

// settingCombatDoctrine turns the whole of this file's effect on and off, so an operator can
// put the live combat loop back without a rebuild (as settingThreatGuard). Default on.
const settingCombatDoctrine = "rolebots.combatDoctrine"

var combatDoctrine atomic.Bool

func init() { combatDoctrine.Store(true) }

func combatDoctrineEnabled() bool { return combatDoctrine.Load() }

// setCombatDoctrine is the ESP page's setter.
func setCombatDoctrine(on bool) { combatDoctrine.Store(on) }

// loadCombatDoctrine reads the persisted setting at Init.
func (m *Mod) loadCombatDoctrine(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingCombatDoctrine); err == nil && ok {
		combatDoctrine.Store(v == "1")
	}
}

// doctrineStance is stanceFor behind the switch and is what every caller uses. stanceFor stays
// pure; off, every hull is a brawler, so there is no second code path.
func doctrineStance(reachM, hullSpeedMS, spawnSpeedMS float64) combatStance {
	if !combatDoctrineEnabled() {
		return stanceBrawl
	}
	return stanceFor(reachM, hullSpeedMS, spawnSpeedMS)
}

// postureFights is Posture.fights() behind the same switch. Off, everything fights, which
// hands the decision back to the armed/unarmed split.
func postureFights(p Posture) bool {
	if !combatDoctrineEnabled() {
		return true
	}
	return p.fights()
}

// closingSpeed is how fast the gap between a hull and a rat shuts, given the stance. A brawler
// holds still relative to the spawn, so the gap closes at the rat's speed; a kiter is running,
// so it closes at the difference. It is floored at 1 m/s so a matched kiter never divides by
// zero.
func closingSpeed(st combatStance, hullSpeedMS, ratSpeedMS float64) float64 {
	if st != stanceKite {
		return ratSpeedMS
	}
	return math.Max(1, ratSpeedMS-hullSpeedMS)
}

// controlReachM is how far this fit can make a target pay or hold it: the guns' reach
// (loadoutReachM), or a stasis web's or scrambler's range when that is longer. A target sitting
// outside it is out of the pilot's hands, which is what the slingshot reads (slingshot.go).
func controlReachM(l *combatLoadout) float64 {
	if l == nil {
		return 0
	}
	r := loadoutReachM(l)
	for _, e := range l.ewar {
		if (e.Kind == ewWeb || e.Kind == ewScram) && e.MaxRange > r {
			r = e.MaxRange
		}
	}
	return r
}
