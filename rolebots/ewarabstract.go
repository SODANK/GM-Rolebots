package rolebots

// What electronic warfare is worth in an unwatched fight.
//
// The embodied tier runs ewar through the entity API (runEwarCycle, combat_embodied.go). The abstract
// duel reads only weapons and hitpoints, so this file gives it an expected suppression: a fraction of
// each rat's output removed while the module runs, divided across the spawn (one jammer against four
// rats is a quarter of the relief).
//
//   - Jam: ewarJamDuty is the expected share of a cycle a rat cannot shoot, kept under a coin flip.
//   - Tracking disruption reads attribute 244, halved again (ewarTurretShare) for non-turret rats.
//   - Sensor damping reads attribute 237. Painting, webs and scrams add nothing (no hit roll or tackle).

import "gurimoon/dogma"

const (
	// ewarJamDuty is the expected share of a cycle a jammed rat spends unable
	// to shoot. See the note above: this is a stand-in for a strength-vs-
	// sensor roll the abstract tier cannot make.
	ewarJamDuty = 0.45

	// ewarTurretShare is how much of a spawn's damage is assumed to come out
	// of turrets, which is all a tracking disruptor touches. Belt rats are a
	// mix of turret and missile hulls and the sim does not separate them.
	ewarTurretShare = 0.5

	// ewarSuppressionCap bounds the whole effect however deep the rack. A hull that neutralised its
	// spawn outright would never have to leave, which the embodied tier would not reproduce.
	ewarSuppressionCap = 0.60
)

// ewarSuppression is the fraction of a spawn's incoming DPS this loadout neutralises. Pure in the
// per-module effect and the division across the spawn, so the harness can pin both. spawnSize is
// how many rats are shooting; zero or one reads as one, so a small spawn never scales it up.
func ewarSuppression(mods []ewarEffect, spawnSize int) float64 {
	if len(mods) == 0 {
		return 0
	}
	if spawnSize < 1 {
		spawnSize = 1
	}
	total := 0.0
	for _, e := range mods {
		total += e.suppression()
	}
	total /= float64(spawnSize)
	if total > ewarSuppressionCap {
		return ewarSuppressionCap
	}
	if total < 0 {
		return 0
	}
	return total
}

// ewarEffect is one fitted electronic module reduced to what the duel needs:
// its class, and the multiplier its own attribute carries.
type ewarEffect struct {
	Kind int
	// Mult is the victim-side multiplier the module applies (attribute 244 for a tracking disruptor,
	// 237 for a damp). A value at or above 1, or at or below 0, means the attribute was missing and the
	// module contributes nothing.
	Mult float64
}

// suppression is one module's share, before the spawn division.
func (e ewarEffect) suppression() float64 {
	switch e.Kind {
	case ewJam:
		return ewarJamDuty
	case ewTrackDisrupt:
		if e.Mult <= 0 || e.Mult >= 1 {
			return 0
		}
		return (1 - e.Mult) * ewarTurretShare
	case ewDamp:
		if e.Mult <= 0 || e.Mult >= 1 {
			return 0
		}
		return 1 - e.Mult
	}
	// Paint, web, scram: see the file note. They contribute nothing.
	return 0
}

// ewarEffectsOf reduces a resolved loadout to what ewarSuppression reads.
func ewarEffectsOf(l *combatLoadout) []ewarEffect {
	if l == nil || len(l.ewar) == 0 {
		return nil
	}
	out := make([]ewarEffect, 0, len(l.ewar))
	for _, ew := range l.ewar {
		e := ewarEffect{Kind: ew.Kind}
		switch ew.Kind {
		case ewTrackDisrupt:
			e.Mult = ew.Attrs[dogma.AttrTrackingSpeedMultiplier]
		case ewDamp:
			e.Mult = ew.Attrs[dogma.AttrMaxTargetRangeMultiplier]
		}
		out = append(out, e)
	}
	return out
}
