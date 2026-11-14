package rolebots

import (
	"context"
	"math"
)

// Fit taste. bestForWish is a pure argmax over inputs every pilot in a station shares,
// so without a per-pilot draw the same hull and job give a byte-identical fit to every
// pilot. fitsynergy.go varies slots within one fit and fitsticky.go damps change over
// time; neither varies one pilot against another. Taste is one draw per (pilot, module
// type), stable for an epoch. Price gating is separate (fitTaste.priceFactor).

// fitTasteAmp is how far a pilot's preference may move a module's score. It sits under
// the stickiness ratio, far under the doctrine step and over the meta step.
// See docs/comment-notes-b10.md#fit-taste-amp.
const fitTasteAmp = 0.08

// fitTaste is one pilot's preferences for this epoch. The zero value is the identity:
// every module scores as if taste did not exist, so the aspirational library and tests
// opt out by passing nothing.
type fitTaste struct {
	seed int64
	// budget is what the pilot can comfortably spend on one module line (fitBudgetFor).
	// 0 means price-blind.
	budget float64
	// ammo is the most the whole reload stack may cost (fitAmmoBudgetFor). 0 means uncapped.
	ammo float64
	// style is the pilot's engagement intent this epoch (fitstyle.go). engageAuto, the
	// zero value, changes nothing.
	style engageStyle
}

// fitTasteFor draws one pilot's taste. Pure over the epoch seed and the character, so
// nothing is stored and planning order does not matter. The pilot is run through
// splitmix64 before the type is mixed in (docs/comment-notes-b10.md#fit-taste-seeding).
// A zero seed returns the zero taste, so one failed epochSeed read repaints no fit.
func (m *Mod) fitTasteFor(ctx context.Context, charID int64) fitTaste {
	seed := m.epochSeed(ctx)
	if seed == 0 || charID == 0 {
		return fitTaste{}
	}
	return fitTaste{seed: int64(splitmix64(uint64(seed) ^ uint64(charID)*noiseGolden))}
}

// forType is this pilot's multiplier on one module type: uniform in [1-fitTasteAmp,
// 1+fitTasteAmp), and exactly 1.0 for the zero taste. It is keyed on the type alone and
// is two-sided, so the roster mean stays 1.0 for the demand wants.go aggregates.
func (t fitTaste) forType(typeID int64) float64 {
	if t.seed == 0 {
		return 1
	}
	return 1 + fitTasteAmp*splitmixUnit(uint64(t.seed)+uint64(typeID)*noiseGolden)
}

// Price scaling: a module's score is multiplied by 1/(1 + ask/budget), where budget is
// a share of the wallet. A poor pilot reads value per ISK and a rich one capability;
// without it a poor pilot is handed a rack procurement refuses to buy.
const (
	// fitModuleBudgetShare is the share of the wallet one module line may comfortably
	// cost. A frigate fit is 6-10 lines, so spending it on every line takes about half the
	// wallet and leaves the replacement fund fitselect.go demands.
	fitModuleBudgetShare = 0.05
	// fitModuleBudgetFloor keeps a near-empty wallet from pricing every module
	// at zero; ordering by price still holds below it.
	fitModuleBudgetFloor = 50.0
	// fitWeaponPriceElasticity softens the factor for the modules that are the job (guns,
	// launchers, mining lasers): swapping them for cheap ones saves ISK and loses the work.
	fitWeaponPriceElasticity = 0.5
)

// fitBudgetFor is the per-line budget for a wallet. The wallet is bucketed to its power
// of two so the rack changes only when wealth changes by a factor, not on every sale.
func fitBudgetFor(balance float64) float64 {
	if balance < 1 {
		return fitModuleBudgetFloor
	}
	b := math.Exp2(math.Floor(math.Log2(balance))) * fitModuleBudgetShare
	if b < fitModuleBudgetFloor {
		return fitModuleBudgetFloor
	}
	return b
}

// fitAmmoBudgetShare is the share of what a pilot may spend that its ammunition (loaded
// rounds plus hold reloads) may take, so ammunition cannot starve the tank.
const fitAmmoBudgetShare = 0.15

// fitAmmoBudgetFor is the ammunition budget for what a pilot may spend (spendable,
// broke.go), bucketed like fitBudgetFor so the reload stack moves only when wealth
// moves by a factor.
func fitAmmoBudgetFor(spend float64) float64 {
	if spend < 1 {
		return fitModuleBudgetFloor
	}
	b := math.Exp2(math.Floor(math.Log2(spend))) * fitAmmoBudgetShare
	if b < fitModuleBudgetFloor {
		return fitModuleBudgetFloor
	}
	return b
}

// priceFactor is the multiplier a module's asking price puts on its score for
// this pilot. Exactly 1 for a price-blind taste or an unpriced module (an
// incumbent read from the static catalogue carries no ask).
func (t fitTaste) priceFactor(ask float64, role modRole) float64 {
	if t.budget <= 0 || ask <= 0 {
		return 1
	}
	f := 1 / (1 + ask/t.budget)
	if role == roleWeapon || role == roleMiner || role == roleDeepCore {
		f = math.Pow(f, fitWeaponPriceElasticity)
	}
	return f
}
