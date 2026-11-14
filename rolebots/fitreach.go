package rolebots

import "math"

// Kite or brawl: the generator prices a gun's reach because the abstract duel (rangeabstract.go)
// gives a hull that outranges its spawn less fire in the opening of a fight. Without that duel
// credit, a fit that traded hitpoints for reach would score worse than one that did not.
// roleWeapon alone is damage per second, so bestForWish always picked the shortest-ranged,
// highest-DPS gun of a family; reach in the gun's own score lets a long-ranged fit exist.
// The duel window is gap/closingSpeed (combatdoctrine.go), so a hull gains only if it has both the
// reach to stand off and a real speed margin over what it fights. Little in the roster clears that.

const (
	// reachWorthKnee is where a gun stops being paid for reaching further: 15 km, about where the
	// belt population's own guns end (the heaviest fire to 16.5 km, entityAttackRange 16500, the rest
	// to 5-9 km). rangeAdvantage caps the window at the length of the fight.
	reachWorthKnee = 15000.0

	// reachWorthMax is the most a long gun may be worth against a short one of equal damage. A
	// quarter: well under doctrineCousin's 45% cut, so reach never overturns a hull's weapon family,
	// and under the damage spread inside one family. fitLean decides which hull wants the nudge.
	reachWorthMax = 1.25
)

// reachWorth is the multiplier a weapon's reach earns it: 1.0 at point blank, reachWorthMax at
// the knee, flat after. Zero or unreadable reach gives exactly 1.0, so modules with no range
// score unchanged.
func reachWorth(reachM float64) float64 {
	if reachM <= 0 {
		return 1
	}
	return 1 + (reachWorthMax-1)*math.Min(reachM/reachWorthKnee, 1)
}

// weaponReachM is how far a catalogue weapon reaches, in the unit FitStats.WeaponReachM uses:
// optimal + 2x falloff for a turret, the charge's flight envelope for a launcher. A launcher has
// no range of its own, so scoring it by zero would make every missile boat read as point blank.
func weaponReachM(mm *marketModule) float64 {
	if mm == nil {
		return 0
	}
	if mm.Launcher {
		return mm.ChargeReachM
	}
	if mm.Turret {
		return mm.Attrs[attrMaxRange] + 2*mm.Attrs[attrFalloff]
	}
	return 0
}

// ── the archetype ───────────────────────────────────────────────────────────

// combatLean is how this hull intends to fight, once its guns are on.
type combatLean int

const (
	// leanUnknown is the state while the highs are placed: nothing is mounted yet. Every wish list
	// answers as it always did under it, so the two-phase placement in generateFitFor changes no fit
	// it should not.
	leanUnknown combatLean = iota
	leanBrawler
	leanKiter
)

func (l combatLean) String() string {
	switch l {
	case leanBrawler:
		return "brawler"
	case leanKiter:
		return "kiter"
	}
	return "unknown"
}

// fitLean reads the archetype off the guns actually mounted, not the hull: the same Rifter is a
// brawler with autocannons and a kiter with artillery. Reach at or over the knee is kiting,
// shorter is brawling, and an empty rack is leanUnknown.
func fitLean(h *HullProfile, fs *fitState) combatLean {
	if fs == nil || fs.weapons == 0 || fs.reachM <= 0 {
		return leanUnknown
	}
	if fs.reachM >= reachWorthKnee {
		return leanKiter
	}
	return leanBrawler
}

// ── the standoff lens ───────────────────────────────────────────────────────

// The nominal spawn a combat fit is judged against, as combatEHPBasis is a nominal tank: one
// number for the population the ship meets, taken from the belt tables. Ordinary Serpentis
// frigates fire to 9.5-16.5 km and run at 1120-1440 m/s.
const (
	standoffSpawnReachM  = 12000.0
	standoffSpawnSpeedMS = 1200.0
	// standoffFightSec is the nominal belt engagement the opening window is measured against (45 s,
	// as threat.go quotes). It is not abstractDuelMaxTicks (600), which is a runaway ceiling and not
	// a real fight.
	standoffFightSec = 45.0
	// The nominal spawn's lock range, so a hull with enormous guns is still
	// judged on what it can see. Matches combatDefaultTargetRange.
	standoffLockM = 30000.0
)

// standoffFactor is how much further a fit's hitpoints go for opening before the spawn can answer.
// It calls rangeAdvantage, the function the duel calls, so the selector and the simulation cannot
// disagree on what reach is worth. Speed counts with reach: fitSpeed takes the running prop module,
// and holdCombatRange (combatdoctrine.go) holds a range.
func standoffFactor(s *FitStats) float64 {
	if s == nil || s.WeaponReachM <= 0 {
		return 1
	}
	// fitSpeed, not MaxVelocity: it counts the prop module running (fitstats.go), so an MWD fit
	// gets the speed it has.
	speed := fitSpeed(s)
	st := doctrineStance(s.WeaponReachM, speed, standoffSpawnSpeedMS)
	lock := standoffLockM
	if s.LockM > 0 {
		lock = s.LockM
	}
	adv := rangeAdvantage(s.WeaponReachM, lock, speed, st,
		[]ratReach{{ReachM: standoffSpawnReachM, SpeedMS: standoffSpawnSpeedMS, DPS: 1}},
		standoffFightSec)
	if adv <= 0 {
		return 1
	}
	return 1 / (1 - adv)
}
