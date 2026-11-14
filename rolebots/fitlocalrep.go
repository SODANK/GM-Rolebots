package rolebots

import "math"

// Local repair in the score: ComputeFitStats counts buffer, and effectiveEHP adds what repairers
// put back over a fight:
//
//	effectiveEHP = EHP + LocalRepPerSec x localRepWindowSec x sustain
//
// sustain is the share of time the capacitor keeps the repairers cycling. The window matches the
// abstract duel, which applies SelfRepPerSec per tick, so selection and simulation agree.
// Repair HP are added without resist scaling, as the duel does. Repairers take no stacking penalty.

const (
	// localRepWindowSec is how many seconds of repair a fit is credited with.
	localRepWindowSec = 45.0

	effShieldBoosting   = 4    // shieldBoosting: a local shield booster
	effStructureRepair  = 26   // structureRepair: a local hull repairer
	effArmorRepair      = 27   // armorRepair: a local armour repairer
	effShieldBoostAmp   = 1720 // shieldBoostAmplifier: +% shield booster amount
	grpShieldBoostAmp   = 338  // Shield Boost Amplifier
	attrStructRepAmount = 83   // hull repairer: HP per cycle
	attrShieldBoostPct  = 548  // Shield Boost Amplifier: +% booster amount (negative on a capacitor relay)

	// Targeting modules (fitstats.go): the passive signal amplifier and the running sensor booster
	// carry lock-range and scan-resolution multipliers that the server folds onto the ship
	// (db.GetShipPassiveBonuses, GetActiveSensorBoosterRangeMult in AddTarget).
	attrLockRangeMult  = 237 // x max targeting range (a penalty on a sensor damper, a bonus here)
	attrScanResMult    = 565 // x scan resolution
)

// localRepSustain is the share of time a fit's capacitor keeps its local repairers cycling:
// its peak recharge against what they drink, capped at 1. A fit with no repairer or no
// capacitor read answers 1.
func localRepSustain(capGJ, rechargeSec, drainGJps float64) float64 {
	if drainGJps <= 0 || capGJ <= 0 || rechargeSec <= 0 {
		return 1
	}
	peak := 0.5 * capGJ / rechargeSec
	if peak >= drainGJps {
		return 1
	}
	return math.Max(peak/drainGJps, 0)
}

// effectiveEHP is a fit's hitpoints plus what its local repairers put back over
// a fight. Equal to s.EHP for a fit that carries none, so every lens that reads
// it is unchanged for every fit that has no repairer.
func effectiveEHP(s *FitStats) float64 {
	if s == nil {
		return 0
	}
	if s.LocalRepPerSec <= 0 {
		return s.EHP
	}
	return s.EHP + s.LocalRepPerSec*localRepWindowSec*localRepSustain(s.CapGJ, s.CapRechargeSec, s.LocalRepCapGJps)
}

// shieldAmpFactor is the multiplier a rack of Shield Boost Amplifiers puts on a shield
// booster's amount: attribute 548 (+%), strongest first on the server's stacking curve
// (db.GetRepairModuleBonuses). Capacitor Power Relays carry a negative 548 and are ignored.
func shieldAmpFactor(pcts []float64) float64 {
	pos := make([]float64, 0, len(pcts))
	for _, p := range pcts {
		if p > 0 {
			pos = append(pos, p)
		}
	}
	// Strongest first.
	for i := 1; i < len(pos); i++ {
		for j := i; j > 0 && pos[j] > pos[j-1]; j-- {
			pos[j], pos[j-1] = pos[j-1], pos[j]
		}
	}
	f := 1.0
	for i, p := range pos {
		f *= 1 + p*stackingPenaltyLocal(i)/100
	}
	return f
}

// sbaTankScore is what a Shield Boost Amplifier is worth, in raw hitpoints like a shield
// extender: the extra hitpoints its percentage adds to the fitted booster over the fight.
// Zero with no booster fitted, because an amplifier multiplies the booster term. Pure.
func sbaTankScore(pct float64, fs *fitState) float64 {
	if pct <= 0 || fs == nil || fs.boosterHPPerSec <= 0 {
		return 0
	}
	return pct / 100 * fs.boosterHPPerSec * localRepWindowSec
}
