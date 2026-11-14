package rolebots

// Reach in an unwatched fight. The embodied tier flies to each weapon's optimal
// and falloff; the abstract duel credits reach as an opening window, the seconds
// before a rat closes the gap, and never as a standoff. The bot's own tracking and
// falloff are application.go's; the rats' guns are not applied.
// A brawler's window uses the rat's speed, a kiter's the speed difference.
// See docs/comment-notes-b16.md#range-advantage.

const (
	// rangeAdvantageCap bounds a brawler's advantage. It stays well under
	// ewarSuppressionCap because the advantage ends when the rat arrives.
	rangeAdvantageCap = 0.35

	// rangeAdvantageKiteCap bounds a kiter's advantage. A kiter's window can be
	// most of the fight, so it matches ewarSuppressionCap. The two multiply, so
	// a jammer on a kiter reaches 0.84.
	rangeAdvantageKiteCap = 0.60
)

// ratReach is one rat reduced to the three numbers the range model needs: how
// far it shoots, how fast it closes and its share of the spawn's damage. It is
// separate from ratThreat so the arithmetic tests without a database.
type ratReach struct {
	ReachM  float64
	SpeedMS float64
	DPS     float64
}

// rangeAdvantage is the fraction of a spawn's incoming damage this hull avoids
// by outranging it. Pure. Per rat: the reach gap over its closing speed, as a
// share of fightSec (EHP over incoming DPS), weighted by its share of spawn DPS.
// A brawler's stance gives the same result at any speed (combatdoctrine.go).
func rangeAdvantage(botReachM, botLockM, botSpeedMS float64, st combatStance,
	spawn []ratReach, fightSec float64) float64 {

	if len(spawn) == 0 || fightSec <= 0 {
		return 0
	}
	// The opening range is the shorter of weapon reach and lock range: a hull
	// cannot shoot what it cannot lock.
	open := botReachM
	if botLockM > 0 && botLockM < open {
		open = botLockM
	}
	if open <= 0 {
		return 0
	}

	spawnDPS := 0.0
	for _, r := range spawn {
		if r.DPS > 0 {
			spawnDPS += r.DPS
		}
	}
	if spawnDPS <= 0 {
		return 0 // nothing is shooting; there is nothing to avoid
	}

	total := 0.0
	for _, r := range spawn {
		if r.DPS <= 0 || r.SpeedMS <= 0 {
			// An unarmed rat throws nothing to avoid, and a sentry never closes.
			continue
		}
		gap := open - r.ReachM
		if gap <= 0 {
			continue // it reaches you at least as far as you reach it
		}
		// How fast this rat shuts the gap: the speed difference for a kiter, the
		// rat's own speed otherwise (closingSpeed). Computed per rat.
		window := gap / closingSpeed(st, botSpeedMS, r.SpeedMS)
		if window > fightSec {
			window = fightSec
		}
		total += (r.DPS / spawnDPS) * (window / fightSec)
	}
	ceiling := rangeAdvantageCap
	if st == stanceKite {
		ceiling = rangeAdvantageKiteCap
	}
	if total > ceiling {
		return ceiling
	}
	return total
}
