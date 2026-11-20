package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
	"gurimoon/dogma"
)

// The fitting-tool stat engine: folds a Fit template (type attributes only, no entity rows) into
// the numbers the server computes for a real fitted ship. Formula sources: db/fittingdb.go
// (capacity, passive bonuses, resists, stacking penalty), db/destinydb.go (navigation, HP),
// docs/bindings/fitting.md. Basis: all skills at V and every module online; the per-pilot
// reality is the validation matrix.

// Attribute IDs beyond the ones in fitting.go (dogma/attributes.go).
const (
	attrStructureHP      = 9   // hull HP
	attrMass             = 4   // invTypes.mass actually, but attr fallback
	attrMaxVelocity      = 37  // m/s
	attrAgility          = 70  // inertia modifier
	attrRateOfFire       = 51  // ms per cycle ("speed")
	attrDamageMultTurret = 64  // turret damage multiplier (weapon + gyro etc.)
	attrCapacitorCap     = 482 // GJ
	attrScanResolution   = 564 // mm: how fast the ship locks
	attrCapRechargeMS    = 55  // ms
	attrShieldCapacity   = 263
	attrArmorHP          = 265
	attrDroneCapacity    = 283
	attrEmDamage         = 114
	attrExplosiveDamage  = 116
	attrKineticDamage    = 117
	attrThermalDamage    = 118
	attrSpeedMultMod     = 204 // damage-mod cycle multiplier
	attrMissileDmgMult   = 213 // BCS raw damage multiplier

	// Passive-bonus attrs (fittingdb.go).
	attrCapacitorBonusFlat = 67  // flat GJ (cap batteries, group 61)
	attrSpeedBonusPct      = 80  // ±% velocity
	attrCapRechMult        = 144 // × cap recharge time
	attrPGMult             = 145 // × powergrid
	attrShieldCapMult      = 146 // × shield capacity
	attrCapCapMult         = 147 // × cap capacity
	attrCargoMult          = 149 // × cargo
	attrStructMult         = 150 // × structure HP
	attrAgilityPct         = 169 // ±% inertia
	attrCPUMultMod         = 202 // × CPU output
	attrPGAddFlat          = 549 // flat MW
	attrChargeSize         = 128
	attrMaxRange           = 54  // module reach / turret optimal, m
	attrFalloff            = 158 // turret falloff, m
	attrExplosionDelay     = 281 // missile flight time, ms
	attrWarpScrambleStr    = 105 // warp scramble points applied to the target
	attrCargoScanRange     = 126 // cargo scanner reach, m
	attrSpeedFactor        = 20  // web: negative % speed change (prop mods: positive)

	groupCapBatteryID = 61
)

// Resonance attribute IDs, index order em/expl/kin/therm (matches the
// server's damage-type order everywhere).
var (
	shieldResonanceAttrs = [4]int64{271, 272, 273, 274}
	armorResonanceAttrs  = [4]int64{267, 268, 269, 270}
	hullResonanceAttrs4  = [4]int64{113, 111, 109, 110} // em, expl, kin, therm (non-contiguous!)

	// Resist-module bonus attrs (percent, negative = more resistance) and
	// damage-control direct hull resonances, same order.
	resistBonusAttrs4 = [4]int64{984, 985, 986, 987}
	dcHullResonance4  = [4]int64{974, 975, 976, 977}
	// The damage control's other two thirds. It is the only module in the
	// database that carries all three blocks, and the only one that resists
	// on the structure at all.
	dcArmorResonance4  = [4]int64{267, 268, 269, 270}
	dcShieldResonance4 = [4]int64{271, 272, 273, 274}
)

// Effect IDs (dgmTypeEffects).
const (
	effLoPower        = 11
	effHiPower        = 12
	effMedPower       = 13
	effLauncherFitted = 40
	effTurretFitted   = 42
	// A mining laser is a turret as far as fitting is concerned: it carries effTurretFitted like a
	// gun, and only effMiningLaser tells them apart. Anything asking whether the ship can shoot back
	// must subtract it.
	effMiningLaser          = 67
	effShieldExtender       = 21   // feeds attr 72 capacityBonus
	effArmorPlate           = 594  // feeds attr 335 armorHpBonus
	effModArmorResonance    = 2041 // passive coatings/platings
	effModShieldResonance   = 2052 // shield amps
	effArmorHardenerActive  = 2098
	effShieldHardenerActive = 2118
	effDamageControl        = 2302
)

const (
	attrShieldExtCapBonus = 72
	attrArmorPlateHPAdd   = 335
	// Remote repair. The shield family's rep amount rides attr 68 as a local booster's does; the armour
	// family carries 84. Ranges differ per family (87 for shield transporters, plain maxRange 54 for
	// armour projectors), so ComputeFitStats reads them per group.
	attrArmorRepAmount      = 84
	attrShieldTransferRange = 87
	// attrMiningDuration is the fallback cycle for a miner that states none. It should not be reached:
	// everything that mines here carries attribute 73. Read minerCycleMS, never a bare attribute,
	// because MiningM3PerMin is the whole of jobPower(JobMining).
	attrMiningDuration = 10000
	// droneMaxLaunched is how many drones can be in space at once: the Drones skill caps it at 5,
	// and the stat basis is all-V.
	droneMaxLaunched = 5
)

// minerCycleMS is how long one mining cycle takes, in milliseconds. Attribute 73 is the authority
// and 51 the fallback, the reverse of a gun (whose cycle is 51 and 73 its capacitor cadence). This
// states the order once.
func minerCycleMS(attrs map[int64]float64) float64 {
	if v := attrs[attrDuration]; v > 0 {
		return v
	}
	return attrs[attrRateOfFire]
}

// FitStats is the full pyfa-style readout for a template.
type FitStats struct {
	// Fitting.
	CPUUsed, CPUCap, PGUsed, PGCap float64
	HiTotal, MedTotal, LoTotal     int
	HiUsed, MedUsed, LoUsed        int
	TurretHard, LauncherHard       int
	TurretUsed, LauncherUsed       int

	// Capacity.
	CargoM3, CargoUsedM3    float64
	DroneBayM3, DroneUsedM3 float64

	// Navigation.
	MaxVelocity, Agility, Mass float64
	AlignSec                   float64
	// PropVelocity is the top speed with the fitted propulsion module running, the number kiting is
	// decided on (fitreach.go, the duel's stance, the tackle lens). MaxVelocity is the hull and passive
	// speed modules only. The boost is the server's formula (propBoost), slowed by plates' mass.
	PropVelocity float64
	// SigRadius is the hull's signature; SigRadiusMWD what it blooms to with
	// a microwarpdrive running (equal to SigRadius without one).
	SigRadius, SigRadiusMWD float64
	PropIsMWD               bool

	// Targeting. The hull's own scan resolution with no skill term; the pilot's Signature Analysis is
	// folded on at the point of use (combat_embodied.go for a fight, skillsupport.go for its worth).
	ScanRes float64

	// Tank (HP + resist %, em/expl/kin/therm).
	ShieldHP, ArmorHP, HullHP    float64
	ShieldRes, ArmorRes, HullRes [4]float64
	EHP                          float64

	// Capacitor.
	CapGJ, CapRechargeSec float64

	// Tackle: what the fit can do about a target that would rather leave. ScramStrength is total warp
	// scramble points, WebPct the strongest speed reduction (positive) and TackleRangeM the shortest
	// tackle reach. Only the piracy lens reads these (fitscore.go).
	ScramStrength float64
	WebPct        float64
	TackleRangeM  float64

	// WarpCoreStrength is the other end of that mechanic: points of scrambling the fit shrugs off, the
	// negation of attribute 105 (a scrambler is +1 or +2, a stabiliser -1 or -2). Kept apart from
	// ScramStrength: summed, a pirate with a stabiliser would read as unpointed. Read by escapeFactor.
	WarpCoreStrength float64

	// CargoScanRangeM is the longest cargo scanner fitted, in metres: the
	// scout lens's gate (jobPower JobScout). 0 = no scanner = not a scout.
	CargoScanRangeM float64

	// Logistics: what the fit can put back on somebody else's ship. HP a second across every remote
	// repairer, and the shortest reach among them. Only the logi lens reads these (fitscore.go).
	RemoteRepPerSec float64
	RemoteRepRangeM float64

	// LocalRepPerSec is what the fit's own boosters and repairers put back on its hull, HP a second,
	// after the Shield Boost Amplifiers; LocalRepCapGJps is what they drink. effectiveEHP
	// (fitlocalrep.go) is the one reader.
	LocalRepPerSec, LocalRepCapGJps float64

	// LockM is the fit's lock range on the nominal-spawn basis (standoffLockM) times what its signal
	// amplifiers and running sensor boosters add. The standoff lens reads it. standoffLockM when the
	// fit has none.
	LockM float64

	// EwarStrength is how much incoming fire the fitted electronics take off a fleet, summed per module
	// before any spawn division, using the shares the abstract duel credits (ewarabstract.go). Read by
	// the electronic warfare seat's lens only.
	EwarStrength float64

	// Offense.
	TurretDPS, LauncherDPS, VolleyDmg float64
	// WeaponReachM is how far this fit fights: the shortest armed weapon's reach, since opening at the
	// longest gun's range fires one gun. A turret is optimal + 2x falloff (as the NPC brain and
	// botWeapon.MaxRange fold it), a missile is velocity x flight time. Read by fitscore.go and fitreach.go.
	WeaponReachM float64
	// ReachOptimalM and ReachFalloffM are the optimal (tracking enhancers folded in) and falloff of the
	// turret that sets WeaponReachM, zero when the shortest weapon is a launcher. They let supportReach
	// lift each half by its own skill (Sharpshooter, Trajectory Analysis).
	ReachOptimalM, ReachFalloffM float64
	// ReachIsMissile reports whether any weapon is a launcher, so the support
	// model knows the missile range skills move WeaponReachM (skillsupport.go).
	ReachIsMissile bool
	MiningM3PerMin float64
	// Drones in the bay, counted as launched (up to droneMaxLaunched). They come from the drone
	// declaration, not a slot, but their damage is real and a fit scored without it looks unarmed.
	DroneDPS            float64
	DroneMiningM3PerMin float64

	Notes []string
}

// ResRow adapts a resist quad for the template (rendered as a grid).
type ResRow struct {
	Label                string
	HP                   float64
	EM, Expl, Kin, Therm float64
}

func (s *FitStats) ResRows() []ResRow {
	return []ResRow{
		{"Shield", s.ShieldHP, s.ShieldRes[0], s.ShieldRes[1], s.ShieldRes[2], s.ShieldRes[3]},
		{"Armor", s.ArmorHP, s.ArmorRes[0], s.ArmorRes[1], s.ArmorRes[2], s.ArmorRes[3]},
		{"Hull", s.HullHP, s.HullRes[0], s.HullRes[1], s.HullRes[2], s.HullRes[3]},
	}
}

// typeInfo caches one type's attributes + effects + identity.
type typeInfo struct {
	TypeID   int64
	GroupID  int64
	Volume   float64
	Capacity float64 // invTypes.capacity: cargo hold for hulls
	Attrs    map[int64]float64
	Effects  map[int64]bool
}

func (m *Mod) typeInfo(ctx context.Context, cache map[int64]*typeInfo, typeID int64) (*typeInfo, error) {
	if ti, ok := cache[typeID]; ok {
		return ti, nil
	}
	staticTypeInfoMu.RLock()
	ti, ok := staticTypeInfo[typeID]
	staticTypeInfoMu.RUnlock()
	if ok {
		cache[typeID] = ti
		return ti, nil
	}
	ti, err := m.loadTypeInfo(ctx, typeID)
	if err != nil {
		return nil, err
	}
	staticTypeInfoMu.Lock()
	staticTypeInfo[typeID] = ti
	staticTypeInfoMu.Unlock()
	cache[typeID] = ti
	return ti, nil
}

// staticTypeInfo is typeInfo for the life of the process: invTypes and dgmTypeEffects rows are
// static and are read thousands of times a minute. Entries are never mutated once built and the set
// is bounded by invTypes, so there is no expiry.
var (
	staticTypeInfoMu sync.RWMutex
	staticTypeInfo   = map[int64]*typeInfo{}
)

// typeInfoLoads counts loadTypeInfo calls, for the harness.
var typeInfoLoads atomic.Int64

func (m *Mod) loadTypeInfo(ctx context.Context, typeID int64) (*typeInfo, error) {
	typeInfoLoads.Add(1)
	ti := &typeInfo{TypeID: typeID, Effects: map[int64]bool{}}
	if err := m.db.QueryRowContext(ctx, `
		SELECT groupID, COALESCE(volume, 0), COALESCE(capacity, 0) FROM invTypes WHERE typeID = ?`,
		typeID).Scan(&ti.GroupID, &ti.Volume, &ti.Capacity); err != nil {
		return nil, fmt.Errorf("typeInfo %d: %w", typeID, err)
	}
	attrs, err := evedb.GetTypeAttributes(ctx, m.db, typeID)
	if err != nil {
		return nil, err
	}
	ti.Attrs = attrs
	rows, err := m.db.QueryContext(ctx,
		`SELECT effectID FROM dgmTypeEffects WHERE typeID = ?`, typeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var eff int64
		if err := rows.Scan(&eff); err != nil {
			return nil, err
		}
		ti.Effects[eff] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ti, nil
}

// allV skill multipliers (level 5 basis).
const (
	allVFitting     = 1.25        // Electronics / Engineering (+5%/lvl)
	allVShieldHP    = 1.25        // Shield Management
	allVArmorHP     = 1.25        // Hull Upgrades
	allVHullHP      = 1.25        // Mechanic
	allVVelocity    = 1.25        // Navigation (+5%/lvl)
	allVAgiSpCmd    = 0.90        // Spaceship Command (−2%/lvl)
	allVAgiEvasive  = 0.75        // Evasive Maneuvering (−5%/lvl)
	allVCapCapacity = 1.25        // Energy Management
	allVCapRecharge = 0.75        // Energy Systems Operation (−5%/lvl)
	allVMining      = 1.25 * 1.25 // Mining × Astrogeology
	// Missile Projection (+10% velocity/lvl) × Missile Bombardment (+10% flight time/lvl): the server's
	// MissileRangeMults, which both a player's launcher and a bot's (combat_embodied.go) read.
	allVMissileRange = 1.5 * 1.5
)

// hullMiningAllV caches hullMiningMultAllV per hull type: the stat engine runs
// for every candidate fit, and the answer is static data.
var hullMiningAllV sync.Map // int64 to float64

// hullMiningMultAllV is the hull's mining-laser bonus at every skill V, the fit-time twin of
// Mod.hullMiningMult (mining.go). It resolves as db.GetShipHullBonuses does (effects select
// dogma.HullBonusSpecs, the first hull attribute present is the per-level percentage) at level 5.
func (m *Mod) hullMiningMultAllV(ctx context.Context, hullTypeID int64) float64 {
	if v, ok := hullMiningAllV.Load(hullTypeID); ok {
		return v.(float64)
	}
	mult := 1.0
	effects, err := evedb.GetTypeEffects(ctx, m.db, hullTypeID)
	if err != nil {
		return 1 // not cached: a failed read must not pin a wrong answer
	}
	var attrs map[int64]float64
	for _, te := range effects {
		for _, sp := range dogma.HullBonusSpecs[te.EffectName] {
			if sp.Target != dogma.HBMiningYield {
				continue
			}
			if attrs == nil {
				if attrs, err = evedb.TypeAttrs(ctx, m.db, hullTypeID); err != nil {
					return 1
				}
			}
			for _, a := range sp.Attrs {
				if v := attrs[a]; v != 0 {
					// The core's own fold (per level × 5, flat, or a
					// Factor), so a spec shape added there is read here.
					if pct, ok := dogma.HullBonusPct(sp, v, 5); ok {
						mult *= 1 + pct/100
					}
					break
				}
			}
		}
	}
	hullMiningAllV.Store(hullTypeID, mult)
	return mult
}

// ComputeFitStats folds the template. Every module counts as online.
func (m *Mod) ComputeFitStats(ctx context.Context, fit *Fit) (*FitStats, error) {
	cache := map[int64]*typeInfo{}
	hull, err := m.typeInfo(ctx, cache, fit.HullTypeID)
	if err != nil {
		return nil, err
	}
	// invTypes.mass from the static cache (statics.go): it was a query per fit costed, a few
	// percent of the database's time. invTypes carries no NULL mass, so the query's 1e6
	// fallback never applied to a real hull.
	hullMass := m.typeMassOf(ctx, fit.HullTypeID)

	s := &FitStats{
		HiTotal:      int(hull.Attrs[attrHiSlots]),
		MedTotal:     int(hull.Attrs[attrMedSlots]),
		LoTotal:      int(hull.Attrs[attrLowSlots]),
		TurretHard:   int(hull.Attrs[attrTurretHard]),
		LauncherHard: int(hull.Attrs[attrLauncherHard]),
		DroneBayM3:   hull.Attrs[attrDroneCapacity],
		ScanRes:      hull.Attrs[attrScanResolution],
		Mass:         hullMass,
		SigRadius:    hull.Attrs[attrSignatureRadius],
	}

	// ── collect per-module contributions ───────────────────────────────
	var (
		cpuMult, pgMult, pgAdd, capAdd = 1.0, 1.0, 0.0, 0.0
		cargoMults                     []float64 // no penalty (fitting outputs exempt; cargo mults unpenalized in server)
		shieldExtAdd, armorPlateAdd    float64
		shieldCapMults, structMults    []float64
		capCapMults, capRechMults      []float64
		velPcts, agiPcts               []float64
		props                          []propSpec
		trackRangeMults                []float64       // tracking enhancers / computers on turret optimal
		massAdd                        float64         // plates: what a prop module has to push
		resistPct                      [3][4][]float64 // shield/armor layers via effects; damage control direct below
		// The damage control's own resonances, one row per layer in the shield/armour/hull order resistPct
		// uses. Direct multipliers outside the stacking penalty (see the effDamageControl branch).
		dcResonanceDirect            = [3][4]float64{{1, 1, 1, 1}, {1, 1, 1, 1}, {1, 1, 1, 1}}
		turretDmgMods, turretSpdMods []float64
		missileDmgMods               []float64
	)

	type weapon struct {
		ti     *typeInfo
		charge *typeInfo
		turret bool
	}
	var weapons []weapon
	var miners []*typeInfo
	// Mining upgrades: the flat yield product the server takes
	// (db.GetMiningYieldBonus) and the CPU penalty they put on the miners.
	miningUpgradeMult, minerCPUMult := 1.0, 1.0
	// Local repair: HP a second from each repairer, split so the amplifiers can
	// multiply only the shield half, and the cap they drink.
	var shieldBoostHPS, otherRepHPS, repCapDrain float64
	var sbaPcts []float64
	var lockRangeMults, scanResMults []float64

	for _, mod := range fit.Modules {
		ti, err := m.typeInfo(ctx, cache, mod.TypeID)
		if err != nil {
			return nil, err
		}
		s.CPUUsed += ti.Attrs[attrCPUUse]
		s.PGUsed += ti.Attrs[attrPGUse]

		switch {
		case mod.Flag >= flagHiFirst && mod.Flag <= flagHiFirst+7:
			s.HiUsed++
		case mod.Flag >= flagMedFirst && mod.Flag <= flagMedFirst+7:
			s.MedUsed++
		case mod.Flag >= flagLoFirst && mod.Flag <= flagLoFirst+7:
			s.LoUsed++
		}
		if ti.Effects[effTurretFitted] {
			s.TurretUsed++
		}
		if ti.Effects[effLauncherFitted] {
			s.LauncherUsed++
		}

		if ti.GroupID == grpMiningUpgrade {
			if v := ti.Attrs[attrMiningAmountBonus]; v > 0 {
				miningUpgradeMult *= 1 + v/100
			}
			if v := ti.Attrs[attrCPUPenaltyPct]; v > 0 {
				minerCPUMult *= 1 + v/100
			}
		}

		// Fitting/capacity passives.
		if v := ti.Attrs[attrCPUMultMod]; v > 0 {
			cpuMult *= v
		}
		if v := ti.Attrs[attrPGMult]; v > 0 {
			pgMult *= v
		}
		pgAdd += ti.Attrs[attrPGAddFlat]
		if v := ti.Attrs[attrCargoMult]; v > 0 {
			cargoMults = append(cargoMults, v)
		}
		if v := ti.Attrs[attrCapacitorBonusFlat]; v > 0 && ti.GroupID == groupCapBatteryID {
			capAdd += v
		}
		if v := ti.Attrs[attrCapCapMult]; v > 0 {
			capCapMults = append(capCapMults, v)
		}
		if v := ti.Attrs[attrCapRechMult]; v > 0 {
			capRechMults = append(capRechMults, v)
		}
		if v := ti.Attrs[attrShieldCapMult]; v > 0 {
			shieldCapMults = append(shieldCapMults, v)
		}
		if v := ti.Attrs[attrStructMult]; v > 0 {
			structMults = append(structMults, v)
		}
		if v := ti.Attrs[attrSpeedBonusPct]; v != 0 {
			velPcts = append(velPcts, v)
		}
		if v := ti.Attrs[attrAgilityPct]; v != 0 {
			agiPcts = append(agiPcts, v)
		}
		// Propulsion and its mass: the group marks a prop module (a web also carries attribute 20), the
		// plate group adds mass, and a prop module's own mass rides in its propSpec.
		if ti.GroupID == grpAfterburner {
			props = append(props, propSpecOf(ti.Attrs))
		}
		if ti.GroupID == grpArmorReinforcer {
			massAdd += ti.Attrs[attrPropMass]
		}
		// Tracking enhancers (passive) and computers (counted running, as the bot runs them) on turret
		// optimal: db.GetTurretTrackingMults' fold.
		if ti.GroupID == grpTrackingEnhancer || ti.GroupID == grpTrackingComputer {
			if v := ti.Attrs[attrTrackingRangeMult]; v > 0 {
				trackRangeMults = append(trackRangeMults, v)
			}
		}

		// Tank adds.
		if ti.Effects[effShieldExtender] {
			shieldExtAdd += ti.Attrs[attrShieldExtCapBonus]
		}
		if ti.Effects[effArmorPlate] {
			armorPlateAdd += ti.Attrs[attrArmorPlateHPAdd]
		}

		// Resists. Hardeners count at full (active) strength, since the bot runs them when it fights.
		addResist := func(layer int) {
			for d, attrID := range resistBonusAttrs4 {
				if b := ti.Attrs[attrID]; b != 0 {
					resistPct[layer][d] = append(resistPct[layer][d], b)
				}
			}
		}
		if ti.Effects[effModShieldResonance] || ti.Effects[effShieldHardenerActive] {
			addResist(0)
		}
		if ti.Effects[effModArmorResonance] || ti.Effects[effArmorHardenerActive] {
			addResist(1)
		}
		// A damage control resists on all three layers and is folded as direct resonance multipliers.
		// See docs/comment-notes-b09.md#damage-control-layers.
		if ti.Effects[effDamageControl] {
			for layer, attrs := range [3][4]int64{
				dcShieldResonance4, dcArmorResonance4, dcHullResonance4,
			} {
				for d, attrID := range attrs {
					if r := ti.Attrs[attrID]; r > 0 && r < 1 {
						dcResonanceDirect[layer][d] *= r
					}
				}
			}
		}

		// Tackle. Read off the group, not the attribute alone: a cloaking device also carries a negative
		// speedFactor, and a fit scored as able to web with a cloak would be a lie.
		if ti.GroupID == grpWarpScrambler {
			s.ScramStrength += ti.Attrs[attrWarpScrambleStr]
			if r := ti.Attrs[attrMaxRange]; r > 0 && (s.TackleRangeM == 0 || r < s.TackleRangeM) {
				s.TackleRangeM = r
			}
		}
		// The counter-tackle is read off its own group and is not summed into ScramStrength. The base
		// stabilisers carry attribute 105 = 0. See docs/comment-notes-b09.md#warp-core-stabilisers.
		if ti.GroupID == grpWarpCoreStab {
			if v := ti.Attrs[attrWarpScrambleStr]; v < 0 {
				s.WarpCoreStrength += -v
			}
		}
		if ti.GroupID == grpCargoScanner {
			if r := ti.Attrs[attrCargoScanRange]; r > s.CargoScanRangeM {
				s.CargoScanRangeM = r
			}
		}
		if ti.GroupID == grpStasisWeb {
			if pct := -ti.Attrs[attrSpeedFactor]; pct > s.WebPct {
				s.WebPct = pct
			}
			if r := ti.Attrs[attrMaxRange]; r > 0 && (s.TackleRangeM == 0 || r < s.TackleRangeM) {
				s.TackleRangeM = r
			}
		}

		// Remote repair, read per family because the attributes differ: the shield transporter reps attr 68
		// at range 87, the armour projector attr 84 at maxRange 54. Cycle time is 73 for both.
		if ti.GroupID == grpShieldTransporter || ti.GroupID == grpArmorRepProjector {
			amount := ti.Attrs[attrShieldBonus]
			reach := ti.Attrs[attrShieldTransferRange]
			if ti.GroupID == grpArmorRepProjector {
				amount = ti.Attrs[attrArmorRepAmount]
				reach = ti.Attrs[attrMaxRange]
			}
			if cycle := ti.Attrs[attrDuration]; cycle > 0 && amount > 0 {
				s.RemoteRepPerSec += amount / (cycle / 1000)
			}
			if reach > 0 && (s.RemoteRepRangeM == 0 || reach < s.RemoteRepRangeM) {
				s.RemoteRepRangeM = reach
			}
		}

		// Local repair, read per effect so a remote rep never double-counts: shield booster 4 (attr 68),
		// armour repairer 27 (attr 84), hull repairer 26 (attr 83), cycle 73. Amplifiers (1720) carry +% in 548.
		if cycle := ti.Attrs[attrDuration]; cycle > 0 {
			var hp float64
			switch {
			case ti.Effects[effShieldBoosting]:
				if hp = ti.Attrs[attrShieldBonus]; hp > 0 {
					shieldBoostHPS += hp / (cycle / 1000)
				}
			case ti.Effects[effArmorRepair]:
				if hp = ti.Attrs[attrArmorRepAmount]; hp > 0 {
					otherRepHPS += hp / (cycle / 1000)
				}
			case ti.Effects[effStructureRepair]:
				if hp = ti.Attrs[attrStructRepAmount]; hp > 0 {
					otherRepHPS += hp / (cycle / 1000)
				}
			}
			if hp > 0 {
				repCapDrain += ti.Attrs[attrCapNeed] / (cycle / 1000)
			}
		}
		if ti.Effects[effShieldBoostAmp] {
			sbaPcts = append(sbaPcts, ti.Attrs[attrShieldBoostPct])
		}
		// Signal amplifiers and sensor boosters (the bot runs a booster whenever it
		// fights, so it counts as running, as a tracking computer does).
		if ti.GroupID == grpSignalAmp || ti.GroupID == grpSensorBooster {
			if v := ti.Attrs[attrLockRangeMult]; v > 0 {
				lockRangeMults = append(lockRangeMults, v)
			}
			if v := ti.Attrs[attrScanResMult]; v > 0 {
				scanResMults = append(scanResMults, v)
			}
		}

		if isEwarGroup(ti.GroupID) {
			s.EwarStrength += ewarSeatWorth(ti.GroupID, ti.Attrs)
		}

		// Weapons / miners / damage mods.
		isMiner := isMiningModule(ti.GroupID)
		switch {
		case isMiner:
			miners = append(miners, ti)
		case ti.Effects[effTurretFitted] || ti.Effects[effLauncherFitted]:
			w := weapon{ti: ti, turret: ti.Effects[effTurretFitted]}
			if mod.ChargeTypeID != 0 {
				if ci, err := m.typeInfo(ctx, cache, mod.ChargeTypeID); err == nil {
					w.charge = ci
				}
			}
			weapons = append(weapons, w)
		default:
			// Non-weapon module carrying damage multipliers = damage mod.
			if v := ti.Attrs[attrDamageMultTurret]; v > 0 && v < 3 {
				turretDmgMods = append(turretDmgMods, v)
			}
			if v := ti.Attrs[attrMissileDmgMult]; v > 0 {
				missileDmgMods = append(missileDmgMods, v)
			}
			if v := ti.Attrs[attrSpeedMultMod]; v > 0 {
				turretSpdMods = append(turretSpdMods, v)
			}
		}
	}

	// ── fitting ────────────────────────────────────────────────────────
	s.CPUCap = hull.Attrs[attrCPUOutput] * allVFitting * cpuMult
	s.PGCap = hull.Attrs[attrPGOutput]*allVFitting*pgMult + pgAdd

	// ── capacity ───────────────────────────────────────────────────────
	// Cargo hold is invTypes.capacity (db.GetShipCargoCapacity); no ship carries attr 38.
	s.CargoM3 = hull.Capacity
	for _, v := range cargoMults {
		s.CargoM3 *= v
	}
	for _, c := range fit.Cargo {
		if ti, err := m.typeInfo(ctx, cache, c.TypeID); err == nil {
			s.CargoUsedM3 += ti.Volume * float64(c.Qty)
		}
	}
	for _, d := range fit.Drones {
		if ti, err := m.typeInfo(ctx, cache, d.TypeID); err == nil {
			s.DroneUsedM3 += ti.Volume * float64(d.Qty)
		}
	}

	// ── navigation ─────────────────────────────────────────────────────
	s.MaxVelocity = hull.Attrs[attrMaxVelocity] * allVVelocity * stackPercentsLocal(velPcts)
	s.Agility = hull.Attrs[attrAgility] * allVAgiSpCmd * allVAgiEvasive * stackPercentsLocal(agiPcts)
	if s.Mass > 0 && s.Agility > 0 {
		// Time to reach 75% velocity (warp-align): −ln(0.25) × M × agi / 1e6.
		s.AlignSec = 1.3863 * s.Mass * s.Agility / 1e6
	}
	s.PropVelocity, s.PropIsMWD = propVelocity(s.MaxVelocity, s.Mass+massAdd, props)
	s.SigRadiusMWD = s.SigRadius
	if s.PropIsMWD {
		s.SigRadiusMWD = s.SigRadius * mwdBloom(props)
	}

	// ── tank ───────────────────────────────────────────────────────────
	s.ShieldHP = (hull.Attrs[attrShieldCapacity] + shieldExtAdd) * allVShieldHP * stackMultipliersLocal(shieldCapMults)
	s.ArmorHP = (hull.Attrs[attrArmorHP] + armorPlateAdd) * allVArmorHP
	s.HullHP = hull.Attrs[attrStructureHP] * allVHullHP * stackMultipliersLocal(structMults)

	resonance := func(base [4]int64) [4]float64 {
		var out [4]float64
		for d, attrID := range base {
			r, ok := hull.Attrs[attrID]
			if !ok || r == 0 {
				r = 1.0
			}
			out[d] = r
		}
		return out
	}
	shieldRes := resonance(shieldResonanceAttrs)
	armorRes := resonance(armorResonanceAttrs)
	hullRes := resonance(hullResonanceAttrs4)
	for layer, res := range []*[4]float64{&shieldRes, &armorRes} {
		for d := 0; d < 4; d++ {
			bs := resistPct[layer][d]
			sort.Float64s(bs) // strongest (most negative) first
			for i, b := range bs {
				res[d] *= 1.0 + b*stackingPenaltyLocal(i)/100.0
			}
		}
	}
	for d := 0; d < 4; d++ {
		shieldRes[d] *= dcResonanceDirect[0][d]
		armorRes[d] *= dcResonanceDirect[1][d]
		hullRes[d] *= dcResonanceDirect[2][d]
		s.ShieldRes[d] = (1 - shieldRes[d]) * 100
		s.ArmorRes[d] = (1 - armorRes[d]) * 100
		s.HullRes[d] = (1 - hullRes[d]) * 100
	}
	// Uniform-damage EHP.
	avg := func(res [4]float64) float64 { return (res[0] + res[1] + res[2] + res[3]) / 4 }
	s.EHP = s.ShieldHP/math.Max(avg(shieldRes), 0.01) +
		s.ArmorHP/math.Max(avg(armorRes), 0.01) +
		s.HullHP/math.Max(avg(hullRes), 0.01)

	// ── capacitor ──────────────────────────────────────────────────────
	s.CapGJ = hull.Attrs[attrCapacitorCap]*allVCapCapacity*stackMultipliersLocal(capCapMults) + capAdd
	s.CapRechargeSec = hull.Attrs[attrCapRechargeMS] / 1000 * allVCapRecharge * stackMultipliersLocal(capRechMults)

	// ── offense ────────────────────────────────────────────────────────
	turretRangeFactor := stackMultipliersLocal(trackRangeMults)
	turretDmgFactor := stackDeviations(turretDmgMods)
	turretSpdFactor := stackDeviations(turretSpdMods)
	missileDmgFactor := stackDeviations(missileDmgMods)
	for _, w := range weapons {
		if w.charge == nil {
			continue
		}
		dmg := w.charge.Attrs[attrEmDamage] + w.charge.Attrs[attrExplosiveDamage] +
			w.charge.Attrs[attrKineticDamage] + w.charge.Attrs[attrThermalDamage]
		cycleMS := w.ti.Attrs[attrRateOfFire]
		if cycleMS <= 0 {
			continue
		}
		// How far this weapon reaches, in one unit for both families: a turret's envelope is optimal + 2x
		// falloff (as the NPC brain applies it to a rat), a missile's is how far it flies before it expires.
		reach := 0.0
		if w.turret {
			reach = w.ti.Attrs[attrMaxRange]*turretRangeFactor + 2*w.ti.Attrs[attrFalloff]
		} else {
			reach = w.charge.Attrs[attrMaxVelocity] * w.charge.Attrs[attrExplosionDelay] / 1000 * allVMissileRange
			s.ReachIsMissile = true
		}
		if reach > 0 && (s.WeaponReachM == 0 || reach < s.WeaponReachM) {
			s.WeaponReachM = reach
			// The two halves of the SHORTEST turret's envelope, so the support
			// model can lift each by its own skill. A launcher has neither.
			s.ReachOptimalM, s.ReachFalloffM = 0, 0
			if w.turret {
				s.ReachOptimalM = w.ti.Attrs[attrMaxRange] * turretRangeFactor
				s.ReachFalloffM = w.ti.Attrs[attrFalloff]
			}
		}

		if w.turret {
			volley := dmg * math.Max(w.ti.Attrs[attrDamageMultTurret], 1) * turretDmgFactor
			s.VolleyDmg += volley
			s.TurretDPS += volley / (cycleMS / 1000 * turretSpdFactor)
		} else {
			volley := dmg * missileDmgFactor
			s.VolleyDmg += volley
			s.LauncherDPS += volley / (cycleMS / 1000)
		}
	}
	minerCPU := 0.0
	for _, ti := range miners {
		amount := ti.Attrs[attrMiningAmount]
		cycleMS := minerCycleMS(ti.Attrs)
		if cycleMS <= 0 {
			cycleMS = attrMiningDuration
		}
		if amount > 0 {
			s.MiningM3PerMin += amount * allVMining / (cycleMS / 1000) * 60
		}
		minerCPU += ti.Attrs[attrCPUUse]
	}
	// The lasers' yield, and only the lasers': a mining drone is not multiplied by an upgrade. The
	// upgrades' CPU cost to the miners is charged so a fit is not judged to fit on grid a client refuses.
	s.MiningM3PerMin *= miningUpgradeMult
	// And the hull's own laser bonus, on the same all-V basis as the rest of this readout.
	s.MiningM3PerMin *= m.hullMiningMultAllV(ctx, fit.HullTypeID)
	s.CPUUsed += minerCPU * (minerCPUMult - 1)

	// Local repair, with the amplifiers on the shield half.
	s.LocalRepPerSec = shieldBoostHPS*shieldAmpFactor(sbaPcts) + otherRepHPS
	s.LocalRepCapGJps = repCapDrain
	s.ScanRes *= stackMultipliersLocal(scanResMults)
	s.LockM = standoffLockM * stackMultipliersLocal(lockRangeMults)

	// Drones. The bay may hold spares; only what can be in space at once
	// counts toward output.
	launched := 0
	for _, d := range fit.Drones {
		if launched >= droneMaxLaunched {
			break
		}
		ti, err := m.typeInfo(ctx, cache, d.TypeID)
		if err != nil {
			continue
		}
		n := int(d.Qty)
		if launched+n > droneMaxLaunched {
			n = droneMaxLaunched - launched
		}
		launched += n
		// A combat drone's cycle is attribute 51 and a mining drone's is 73, as for the modules; reading
		// both on 51 leaves DroneMiningM3PerMin at zero.
		if amount := ti.Attrs[attrMiningAmount]; amount > 0 {
			if cycleMS := minerCycleMS(ti.Attrs); cycleMS > 0 {
				s.DroneMiningM3PerMin += float64(n) * amount / (cycleMS / 1000) * 60
			}
		}
		cycleMS := ti.Attrs[attrRateOfFire]
		if cycleMS <= 0 {
			continue
		}
		dmg := (ti.Attrs[attrEmDamage] + ti.Attrs[attrExplosiveDamage] +
			ti.Attrs[attrKineticDamage] + ti.Attrs[attrThermalDamage]) *
			math.Max(ti.Attrs[attrDamageMultTurret], 1)
		if dmg > 0 {
			s.DroneDPS += float64(n) * dmg / (cycleMS / 1000)
		}
	}

	if s.CPUUsed > s.CPUCap || s.PGUsed > s.PGCap {
		s.Notes = append(s.Notes, "OVER CPU/PG at all-V skills — the fit cannot go online as-is")
	}
	if s.TurretUsed > s.TurretHard {
		s.Notes = append(s.Notes, fmt.Sprintf("%d turrets fitted but only %d hardpoints", s.TurretUsed, s.TurretHard))
	}
	if s.LauncherUsed > s.LauncherHard {
		s.Notes = append(s.Notes, fmt.Sprintf("%d launchers fitted but only %d hardpoints", s.LauncherUsed, s.LauncherHard))
	}
	if s.DroneUsedM3 > s.DroneBayM3 {
		s.Notes = append(s.Notes, fmt.Sprintf("drone bay over capacity (%.0f/%.0f m³)", s.DroneUsedM3, s.DroneBayM3))
	}
	if s.CargoUsedM3 > s.CargoM3 {
		s.Notes = append(s.Notes, fmt.Sprintf("declared cargo exceeds the hold (%.0f/%.0f m³)", s.CargoUsedM3, s.CargoM3))
	}
	return s, nil
}

// stackingPenaltyLocal mirrors db.StackingPenalty: exp(-(i/2.67)²).
func stackingPenaltyLocal(i int) float64 {
	x := float64(i) / 2.67
	return math.Exp(-x * x)
}

// stackMultipliersLocal folds ≈1.0±x multipliers, strongest first, penalized.
func stackMultipliersLocal(vals []float64) float64 {
	sort.Slice(vals, func(i, j int) bool {
		return math.Abs(vals[i]-1.0) > math.Abs(vals[j]-1.0)
	})
	out := 1.0
	for i, v := range vals {
		out *= 1.0 + (v-1.0)*stackingPenaltyLocal(i)
	}
	return out
}

// stackPercentsLocal folds signed percentages, strongest first, penalized.
func stackPercentsLocal(vals []float64) float64 {
	sort.Slice(vals, func(i, j int) bool {
		return math.Abs(vals[i]) > math.Abs(vals[j])
	})
	out := 1.0
	for i, v := range vals {
		out *= 1.0 + v/100.0*stackingPenaltyLocal(i)
	}
	return out
}

// stackDeviations is stackMultipliersLocal for damage-mod style factors.
func stackDeviations(vals []float64) float64 { return stackMultipliersLocal(vals) }

// ── propulsion ──────────────────────────────────────────────────────────────

// attrSignatureRadius is the hull's signature (552).
const attrSignatureRadius = 552

// propSpec is one propulsion module reduced to what its boost needs.
type propSpec struct {
	SpeedFactor float64 // attribute 20, percent
	Thrust      float64 // attribute 567, speedBoostFactor
	MassAdd     float64 // attribute 796
	SigBloom    float64 // attribute 554, percent; non-zero only on an MWD
}

func propSpecOf(attrs map[int64]float64) propSpec {
	return propSpec{
		SpeedFactor: attrs[attrSpeedFactor],
		Thrust:      attrs[attrSpeedBoostFactor],
		MassAdd:     attrs[attrPropMass],
		SigBloom:    attrs[attrSigRadiusBonus],
	}
}

// propBoost is the multiplier a running prop module puts on top speed: the server's classic
// formula (cmd/gurimoon/main.go, the speedBoost arm of bound.Activate), 1 + speedFactor% x thrust /
// (mass + massAddition). Stated once so the fit lens, the abstract duel and the embodied ball agree
// on how fast a kiter is. 1 for anything that boosts nothing.
func propBoost(p propSpec, mass float64) float64 {
	if p.SpeedFactor <= 0 {
		return 1
	}
	if p.Thrust > 0 && mass+p.MassAdd > 0 {
		return 1 + (p.SpeedFactor/100)*(p.Thrust/(mass+p.MassAdd))
	}
	return 1 + p.SpeedFactor/100
}

// propVelocity is top speed with the best fitted prop module running, and
// whether that module is a microwarpdrive. One prop module runs at a time, so
// the best one is the one a pilot switches on.
func propVelocity(base, mass float64, props []propSpec) (float64, bool) {
	best, mwd := 1.0, false
	for _, p := range props {
		if b := propBoost(p, mass); b > best {
			best, mwd = b, p.SigBloom > 0
		}
	}
	return base * best, mwd
}

// mwdBloom is the signature multiplier of the best-boosting microwarpdrive.
func mwdBloom(props []propSpec) float64 {
	bloom := 1.0
	for _, p := range props {
		if p.SigBloom > 0 && 1+p.SigBloom/100 > bloom {
			bloom = 1 + p.SigBloom/100
		}
	}
	return bloom
}

// fitSpeed is the speed every kite decision reads: prop module running where
// one is fitted, the hull's own otherwise.
func fitSpeed(s *FitStats) float64 {
	if s == nil {
		return 0
	}
	if s.PropVelocity > s.MaxVelocity {
		return s.PropVelocity
	}
	return s.MaxVelocity
}

// ewarPainterSeatWorth is what a target painter is worth to the electronic warfare seat. The
// abstract duel credits a painter nothing (no hit roll), but a seat is judged for the fleet, whose
// guns land more often on a painted target. An approximation kept under a damp's typical share.
const ewarPainterSeatWorth = 0.15

// ewarSeatWorth is one EWAR module's share of the incoming fire it removes,
// on ewarEffect.suppression's arithmetic.
func ewarSeatWorth(groupID int64, attrs map[int64]float64) float64 {
	switch groupID {
	case grpECM:
		return ewarEffect{Kind: ewJam}.suppression()
	case grpTrackingDisruptor:
		return ewarEffect{Kind: ewTrackDisrupt, Mult: attrs[dogma.AttrTrackingSpeedMultiplier]}.suppression()
	case grpSensorDamper:
		return ewarEffect{Kind: ewDamp, Mult: attrs[dogma.AttrMaxTargetRangeMultiplier]}.suppression()
	case grpTargetPainter:
		return ewarPainterSeatWorth
	}
	return 0
}

// attrTrackingRangeMult is a tracking enhancer's or computer's multiplier on turret optimal (243),
// the attribute db.GetTurretTrackingMults reads.
const attrTrackingRangeMult = 243
