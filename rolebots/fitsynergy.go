package rolebots

import "math"

// What is already on the ship and what it does to the next slot. Two rules pull in
// opposite directions: redundancy (a module already fitted makes another like it worth
// less, per role: redundancyFactor) and synergy (a fitted module makes its complement
// worth more: synergyFactor). Both run after the CPU/PG gate and only reorder.

// Attribute IDs this file and the generator's scoring need. IDs already declared in
// fitstats.go are reused, not redeclared.
const (
	// attrPropMass (massAddition) is the authority on a propulsion module's size. Names
	// mislead: a "5MN Afterburner I" carries 500000 and is a frigate module.
	attrPropMass = 796
	attrCapNeed  = 6  // capacitorNeed, GJ per cycle
	attrDuration = 73 // cycle time, ms

	// attrMaxGroupActive (maxGroupActive) is the server's ceiling on how many of a group
	// run at once. Every propulsion module carries 1: a second one cannot be switched on.
	attrMaxGroupActive = 763
)

// Resistance bonus attributes in em/expl/kin/therm order, matching the resonance arrays
// in fitstats.go. Negative is better (a coating reads -8).
var resistBonusAttrs = [4]int64{984, 985, 986, 987}

// Propulsion mass classes from attr 796. These are the only values in the database.
const (
	propMass1MN   = 500000
	propMass10MN  = 5000000
	propMass100MN = 50000000
)

const (
	// dupActiveTankFactor: a second active repairer or booster is nearly worthless since
	// the capacitor cannot run two, but it is not refused, so a slot nothing else serves
	// is not left empty.
	dupActiveTankFactor = 0.05

	// capStarvedFloor is how far a cap-starved propulsion module may fall. Without a floor
	// a tiny-capacitor hull scores every prop mod at zero and gets no engine.
	capStarvedFloor = 0.05

	// capPressureCeiling caps the capacitor-pressure synergy, so a cap-starved fit cannot
	// make a cap recharger outscore a weapon in a shared wish.
	capPressureCeiling = 2.0

	// roleYieldFloor is the factor below which a role yields its slot to the next role in
	// the wish. bestForWish returns on the first role with any candidate, so only crossing
	// this line changes a pick. 0.35 sits between the 3rd and 4th stacked copy (0.571, 0.283).
	roleYieldFloor = 0.35

	// roleAbandonFloor is the factor below which the slot is left empty. A role that
	// yields is normally kept as the fallback, but past this line a module is not worth
	// its ISK or grid: 0.20 sits between the 4th copy (0.283) and the 5th (0.106).
	roleAbandonFloor = 0.20
)

// fitState is the ship as assembled so far, for one generateFitFor call.
type fitState struct {
	byGroup     map[int64]int
	byRole      map[modRole]int
	resistCount [4]int  // per damage type: how many fitted modules resist it
	capUse      float64 // GJ/s drawn by the active modules already fitted
	weapons     int     // turrets and launchers placed
	// turrets and launchers count placements per hardpoint family. The budgets are
	// separate and bestForWish can fill a launcher wish with turrets (doctrineStranger).
	// Mining lasers count as turrets; strip miners do not.
	turrets   int
	launchers int
	// reachM is the shortest reach among the weapons placed so far, which fitLean reads
	// (fitreach.go). Zero until a gun goes on.
	reachM float64
	// kiteGuns counts weapons tagged Kite so far (modtags.go). A Brawl gun placed after
	// one drags the fit's shortest reach down; see dragsKiteReach.
	kiteGuns int
	// miners and minerCPU are the mining lasers placed and the CPU they draw: what a
	// mining upgrade multiplies and costs (fitminingupgrade.go).
	miners   int
	minerCPU float64
	// gridRefused is which budget (CPU, powergrid) refused a module that would have won
	// its slot; it triggers the fitting-capacity pass in fitgrid.go.
	gridRefused gridRefusal
	// boosterHPPerSec is the strongest shield booster placed so far, in HP a second: what
	// a Shield Boost Amplifier multiplies (fitlocalrep.go).
	boosterHPPerSec float64
}

func newFitState() *fitState {
	return &fitState{byGroup: map[int64]int{}, byRole: map[modRole]int{}}
}

// add records a placed module and the role its slot was filled for.
func (fs *fitState) add(mm *marketModule, role modRole) {
	fs.byGroup[mm.GroupID]++
	fs.byRole[role]++
	if mm.Turret || mm.Launcher {
		fs.weapons++
		// The shortest armed weapon's reach is the range the fit will fight at. fitLean needs
		// it before the rest of the rack is decided.
		if r := weaponReachM(mm); r > 0 && (fs.reachM == 0 || r < fs.reachM) {
			fs.reachM = r
		}
	}
	if mm.Turret {
		fs.turrets++
	}
	if mm.Launcher {
		fs.launchers++
	}
	if (mm.Turret || mm.Launcher) && mm.Style == styleKite {
		fs.kiteGuns++
	}
	if isMiningModule(mm.GroupID) {
		fs.miners++
		fs.minerCPU += mm.CPU
	}
	if mm.GroupID == grpShieldBooster {
		if dur := mm.Attrs[attrDuration]; dur > 0 {
			if hps := mm.Attrs[attrShieldBonus] / (dur / 1000); hps > fs.boosterHPPerSec {
				fs.boosterHPPerSec = hps
			}
		}
	}
	for d, attrID := range resistBonusAttrs {
		if -mm.Attrs[attrID] > 0 {
			fs.resistCount[d]++
		}
	}
	fs.capUse += moduleCapDrain(mm)
}

// dragsKiteReach reports whether placing mm would pull a kiting battery's reach down:
// mm is a Brawl weapon and a Kite weapon is already mounted. Reach reads the shortest
// gun, so the slot is left empty instead. Pure; false for anything not a weapon.
func dragsKiteReach(fs *fitState, mm *marketModule) bool {
	return fs != nil && fs.kiteGuns > 0 && (mm.Turret || mm.Launcher) && mm.Style == styleBrawl
}

// moduleCapDrain is GJ per second, 0 for anything passive.
func moduleCapDrain(mm *marketModule) float64 {
	need := mm.Attrs[attrCapNeed]
	if need <= 0 {
		return 0 // passive: nothing to feed
	}
	// Cycle time is in duration (73), or in rateOfFire (51) for weapons, which carry no
	// duration. Reading only 73 gives every gun zero drain.
	dur := mm.Attrs[attrDuration]
	if dur <= 0 {
		dur = mm.Attrs[attrRateOfFire]
	}
	if dur <= 0 {
		return 0
	}
	return need / (dur / 1000)
}

// capPeak is the hull's peak capacitor recharge in GJ/s. Recharge is
// dC/dt = (2*Cmax/tau)*(sqrt(x) - x), which peaks at x = 0.25, so the best a hull does
// is half its capacity over its recharge time.
func capPeak(h *HullProfile) float64 {
	if h.CapGJ <= 0 || h.CapRechargeSec <= 0 {
		return 0
	}
	return 0.5 * h.CapGJ / h.CapRechargeSec
}

// capBudget is what is left of that peak after everything fitted. Negative means the
// ship spends more than it makes.
func capBudget(h *HullProfile, fs *fitState) float64 {
	return capPeak(h) - fs.capUse
}

// capFeasibility is how much of an active module this hull can afford to run, in
// [capStarvedFloor, 1].
func capFeasibility(mm *marketModule, h *HullProfile, fs *fitState) float64 {
	drain := moduleCapDrain(mm)
	if drain <= 0 {
		return 1 // passive: nothing to feed
	}
	budget := capBudget(h, fs)
	if budget <= 0 {
		return capStarvedFloor
	}
	if drain <= budget {
		return 1
	}
	return math.Max(budget/drain, capStarvedFloor)
}

// The capacitor constrains a fit through fs.capUse, not a per-gun factor; see the
// roleWeapon arm of moduleRoleScoreBase (fitgen.go).

// ── what a module is, for redundancy purposes ───────────────────────────────

// activeTank is a repairer or a booster: one is a tank, two are a slower way to run
// out of capacitor.
func activeTank(mm *marketModule) bool {
	return mm.GroupID == grpArmorRepairer || mm.GroupID == grpShieldBooster
}

// resistModule reports a resistance bonus on any damage type (coatings, platings,
// amplifiers). It reads attributes because a group says where a module fits, not what
// it does.
func resistModule(mm *marketModule) bool {
	for _, attrID := range resistBonusAttrs {
		if -mm.Attrs[attrID] > 0 {
			return true
		}
	}
	return false
}

// resistScore is what a resist module is worth on a bare hull: the summed resistance
// it brings. The four damage types are weighted equally; weighting by the hull's weakest
// resonance would need the resonance attributes on HullProfile.
func resistScore(mm *marketModule) float64 {
	score := 0.0
	for _, attrID := range resistBonusAttrs {
		if bonus := -mm.Attrs[attrID]; bonus > 0 {
			score += bonus
		}
	}
	return score
}

// dcResonanceAttrs is every resonance a damage control carries (shield, armour, hull).
// It stays apart from fitstats.go's arrays because this file's unit is percentage
// points and those are resonance multipliers: do not sum the two.
var dcResonanceAttrs = [12]int64{
	271, 272, 273, 274, // shield em/exp/kin/therm
	267, 268, 269, 270, // armour
	974, 975, 976, 977, // hull
}

// damageControlScore is what a damage control brings, in the percentage points
// resistScore uses. A resonance r is a (1-r)*100 resist, summed across all three layers.
// No stacking discount: maxGroupActive is 1, so redundancyFactor already returns 0 for
// a second one.
func damageControlScore(mm *marketModule) float64 {
	score := 0.0
	for _, attrID := range dcResonanceAttrs {
		if r := mm.Attrs[attrID]; r > 0 && r < 1 {
			score += (1 - r) * 100
		}
	}
	return score
}

// armorRepWorthCycles is how many cycles of an active repairer a plate's flat hitpoints
// are judged against, so buffer and active tank share one unit in the roleTank arm. Ten
// is about one belt fight of repair; only the order of magnitude matters.
const armorRepWorthCycles = 10.0

// resistCovered is how much of resistScore is left given what the ship already resists:
// each damage type is discounted by how many fitted modules cover it, on the sim's
// stacking curve. 1.0 on a bare hull. It is a factor, not part of the raw score, so it
// can cross roleYieldFloor and hand the slot to the next role.
func resistCovered(mm *marketModule, fs *fitState) float64 {
	bare := resistScore(mm)
	if bare <= 0 {
		return 1
	}
	left := 0.0
	for d, attrID := range resistBonusAttrs {
		if bonus := -mm.Attrs[attrID]; bonus > 0 {
			left += bonus * stackingPenaltyLocal(fs.resistCount[d])
		}
	}
	return left / bare
}

// ── redundancy ──────────────────────────────────────────────────────────────

// redundancyFactor discounts a module by what is already on the ship, in [0, 1]. The
// policy is per role, because "another one" means different things by slot purpose.
func redundancyFactor(fs *fitState, mm *marketModule, role modRole) float64 {
	// The server's group ceiling applies whatever the role thinks: a module the client
	// refuses to switch on is worth nothing.
	if cap := int(mm.Attrs[attrMaxGroupActive]); cap > 0 && fs.byGroup[mm.GroupID] >= cap {
		return 0
	}
	n := fs.byRole[role]
	if n == 0 {
		return 1
	}
	switch role {
	case roleWeapon, roleMiner, roleDeepCore:
		// A second gun is a second gun; the hardpoints already cap these.
		return 1

	case roleMiningUpgrade:
		// Not stack-penalised: the server multiplies upgrade yields flat
		// (db.GetMiningYieldBonus), and miningUpgradeCount already decided how many.
		return 1

	case roleCargo:
		// Cargo is not stack-penalised: ComputeFitStats multiplies cargo multipliers flat
		// (fitstats.go), and jobPower pays a hauler for every m3.
		return 1

	case roleSpeed:
		// maxGroupActive is 1 on the whole propulsion group: a second engine is worth nothing.
		return 0
	case roleTackle, roleWeb, roleDamageControl:
		// Structurally one already (the Count: 1 wishes in wishList), and attribute 763 is 1 on
		// every damage control. This is a backstop.
		return 0

	case roleHarden:
		// By damage type on the stacking curve, as the resist arm of roleTank does. As a
		// factor it can cross roleYieldFloor, so a rack stops at the hardeners that pay.
		return resistCovered(mm, fs)

	case roleRemoteRep:
		// Repairers stack (a second transporter is a second patient); the highs cap the rack.
		return 1
	case roleEwar:
		// A second module of the same EWAR group mostly overlaps; another flavour does not.
		// The discount is per group so leftovers spread across painter, damp and disruptor.
		if fs.byGroup[mm.GroupID] > 0 {
			return 0.4
		}
		return 1

	case roleTank:
		switch {
		case mm.GroupID == grpShieldBoostAmp:
			// Amplifiers stack on the server's curve, strongest first.
			return stackingPenaltyLocal(fs.byGroup[grpShieldBoostAmp])
		case activeTank(mm):
			return dupActiveTankFactor
		case resistModule(mm):
			// By damage type, finer than a role counter: a second explosive coating beside an
			// adaptive is cheap, the first explosive one beside three thermals is dear.
			return resistCovered(mm, fs)
		default:
			// Buffer (extenders, plates) is additive. The sim stacks it, so the generator picks
			// under the same curve.
			return stackingPenaltyLocal(n)
		}

	default: // roleDamage, roleCap
		return stackingPenaltyLocal(n)
	}
}

// fitAdjust is everything the half-built ship does to a module's raw score in one
// number: down for what it has, up for what it makes work. 1.0 on a bare hull.
// bestForWish reads it to scale the score and against roleYieldFloor.
func fitAdjust(fs *fitState, mm *marketModule, h *HullProfile, role modRole) float64 {
	r := redundancyFactor(fs, mm, role)
	// The hull's own EWAR family is exempt from the same-group discount: on a Blackbird a
	// rack of jammers is the ship, and each holds another rat off it (ewarSuppression).
	if r > 0 && baseRole(role) == roleEwar && h != nil && h.HasSpecialty(ewarSpecialtyByGroup[mm.GroupID]) {
		r = 1
	}
	return r * synergyFactor(fs, h, role)
}

// ── synergy ─────────────────────────────────────────────────────────────────

// synergyFactor is what the rest of the ship does for this module, at least 1. There
// is one currency, capacitor: a running repairer raises fs.capUse, which makes a cap
// recharger worth more, and the same holds for any cycling module.
func synergyFactor(fs *fitState, h *HullProfile, role modRole) float64 {
	if role != roleCap {
		return 1
	}
	peak := capPeak(h)
	if peak <= 0 || fs.capUse <= 0 {
		// Nothing running: a cap module regenerates for nobody and should lose its slot.
		return 1
	}
	return 1 + math.Min(fs.capUse/peak, capPressureCeiling)
}

// ── propulsion sizing ───────────────────────────────────────────────────────

// propMassClass reads a propulsion module's size off attr 796. 0 means the
// module carries no mass addition and is not a prop mod at all.
func propMassClass(mm *marketModule) int {
	switch mm.Attrs[attrPropMass] {
	case propMass1MN:
		return 1
	case propMass10MN:
		return 2
	case propMass100MN:
		return 3
	}
	return 0
}

// hullPropClass is the prop size a hull class takes: 1MN on frigate-sized hulls, 10MN
// on cruiser-sized, 100MN on battleship-sized. 0 means no opinion and lets anything
// through.
func hullPropClass(c HullClass) int {
	switch c {
	case ClassRookie, ClassShuttle, ClassFrigate, ClassDestroyer:
		return 1
	case ClassCruiser, ClassBattlecruiser, ClassIndustrial, ClassBarge:
		return 2
	case ClassBattleship, ClassFreighter, ClassCapital:
		return 3
	}
	return 0
}
