package rolebots

import (
	"math"

	evedb "gurimoon/db"
)

// Support skills are the levers a fit's score cannot see: skills no module requires that
// still decide how well a ship works. ComputeFitStats folds every fit at the all-V basis
// (fitstats.go), so training them alone moves no fit score. Each lever is priced by rebasing
// the all-V stats onto the pilot's real sheet and taking the ratio of two jobPower readings
// (fitscore.go). Every per-level number is the one the server applies.
// Not priced here: Electronics and Engineering (fiteffective.go already counts them), the
// electronic warfare skills (the server applies no strength term), Motion Prediction,
// Controlled Bursts and Long Range Targeting (jobPower reads none of them), and drone damage
// skills (only Drones matters). Signature Analysis is priced through the lock share in
// engagementShare.

// Skill typeIDs. Each is hardcoded in the server or mirrored in the all-V basis of
// fitstats.go; the comment beside it names which.
const (
	// Turret support: db.GetWeaponCombatBonuses' turret `globals`.
	skillGunnery        = 3300
	skillRapidFiring    = 3310
	skillSurgicalStrike = 3315
	// Launcher support: the same function's launcher globals.
	skillMissileLauncherOp = 3319
	skillWarheadUpgrades   = 20315
	skillRapidLaunch       = 21071
	// Drones: one drone in space per level (cmd/gurimoon/drones.go).
	skillDrones = 3436
	// Tank: the three all-V constants in fitstats.go.
	skillShieldManagement = 3419
	skillHullUpgrades     = 3394
	skillMechanic         = 3392
	// Agility: db.GetShipStats applies both to inertia.
	skillSpaceshipCommand   = 3327
	skillEvasiveManeuvering = 3453
	// Targeting: dogma.SkillSignatureAnalysis, folded onto scan resolution
	// by the server's AddTarget and by the bot's fight loop.
	skillSignatureAnalysis = 3431
	// Navigation: hardcoded 3449 in db/destinydb.go, +5% top speed per level.
	skillNavigation = 3449
	// Missile range: dogma.SkillMissileProjection and SkillMissileBombardment,
	// read by db.MissileRangeMults off attributes 20 and 557.
	skillMissileProjection  = 12442
	skillMissileBombardment = 12441
	// Turret range: dogma.SkillSharpshooter and SkillTrajectoryAnalysis, read
	// by db.GetTurretSkillMults off attributes 294 and 349.
	skillSharpshooter       = 3311
	skillTrajectoryAnalysis = 3317
	// Capacitor: dogma.SkillEnergyManagement and SkillEnergySystemsOp, read
	// by db.GetCapacitorSkillMultipliers off attributes 1079 and 314.
	skillEnergyManagement = 3418
	skillEnergySystemsOp  = 3417
)

// The per-level attributes the range and capacitor levers read off the skills
// themselves, the same ones the server's folds read.
const (
	attrSkillMissileVelBonus    = 20   // Missile Projection, +% velocity
	attrSkillMissileFlightBonus = 557  // Missile Bombardment, +% flight time
	attrSkillRangeBonus         = 294  // Sharpshooter, +% turret optimal
	attrSkillFalloffBonus       = 349  // Trajectory Analysis, +% turret falloff
	attrSkillCapCapacityBonus   = 1079 // Energy Management, +% capacity
	attrSkillCapRechargeBonus   = 314  // Energy Systems Operation, % recharge time
)

// The per-level bonus attributes on the weapon skills, read from static data
// the way db.GetWeaponCombatBonuses reads them.
const (
	attrSkillDamageBonus      = 292 // +% damage per level
	attrSkillRofBonus         = 293 // % cycle time per level (negative = faster)
	attrSkillTurretSpeedBonus = 441 // Gunnery's % turret cycle time per level
	attrSkillScanResBonus     = 566 // Signature Analysis' +% scan resolution
)

// combatEngagementSec is the length of the fight a lock is measured against: one belt rat,
// lock to wreck. It turns lock seconds into a share of a fight, so keep it near a real
// engagement length.
const combatEngagementSec = 60.0

const (
	// supportHPPerLevel is the +5% per level behind allVShieldHP, allVArmorHP and allVHullHP:
	// Shield Management, Hull Upgrades and Mechanic.
	supportHPPerLevel = 0.05
	// supportMiningPerLevel is the +5%/level Mining and Astrogeology each add,
	// the pair behind allVMining and behind the server's own mining tick.
	supportMiningPerLevel = 0.05
	// The two agility skills, out of db.GetShipStats.
	agilitySpCmdPerLevel   = 0.02
	agilityEvasivePerLevel = 0.05
)

// turretSupportSkills / launcherSupportSkills are the weapon skills that no
// module requires and every weapon uses.
var (
	turretSupportSkills   = []int64{skillGunnery, skillRapidFiring, skillSurgicalStrike}
	launcherSupportSkills = []int64{skillMissileLauncherOp, skillWarheadUpgrades, skillRapidLaunch}
)

// supportLeverSkills lists the skills that make a ship better at one job without being in a
// fit's requirement list. Piracy shares combat's list: its tackle is modules, which are
// requirements already.
func supportLeverSkills(job string) []int64 {
	switch job {
	case JobMining:
		return []int64{skillMining, skillAstrogeology, skillDrones}
	case JobTrade:
		// A hauler is cargo carried at an align time, and align is agility.
		return []int64{skillSpaceshipCommand, skillEvasiveManeuvering}
	case JobFreight:
		// A lifter is cargo alone: no support skill moves an expander's hold, and the racial
		// Industrial hull skill is a requirement already.
		return nil
	case JobLogi:
		// Throughput is modules (requirements already); what the levers move
		// is how long the rack keeps cycling and how long the logi lives.
		return []int64{skillEnergyManagement, skillEnergySystemsOp,
			skillShieldManagement, skillHullUpgrades, skillMechanic}
	default:
		out := make([]int64, 0, 12)
		out = append(out, turretSupportSkills...)
		out = append(out, launcherSupportSkills...)
		return append(out, skillDrones,
			skillShieldManagement, skillHullUpgrades, skillMechanic,
			skillSpaceshipCommand, skillEvasiveManeuvering, skillSignatureAnalysis,
			skillNavigation, skillMissileProjection, skillMissileBombardment,
			skillSharpshooter, skillTrajectoryAnalysis)
	}
}

// skillLevel reads one level out of a projected sheet, clamped to the range
// a skill can actually hold.
func skillLevel(levels map[int64]int, typeID int64) float64 {
	l := levels[typeID]
	if l < 0 {
		l = 0
	}
	if l > 5 {
		l = 5
	}
	return float64(l)
}

// miningYieldMult is the laser multiplier the server applies when it counts
// rock: (1+5%·Mining) × (1+5%·Astrogeology), the same product mining.go and
// main.go's mining tick compute.
func miningYieldMult(levels map[int64]int) float64 {
	return (1 + supportMiningPerLevel*skillLevel(levels, skillMining)) *
		(1 + supportMiningPerLevel*skillLevel(levels, skillAstrogeology))
}

// agilityMult is what db.GetShipStats does to inertia: Spaceship Command
// −2% per level and Evasive Maneuvering −5%. Align time is proportional to
// agility, so this scales it directly.
func agilityMult(levels map[int64]int) float64 {
	m := (1 - agilitySpCmdPerLevel*skillLevel(levels, skillSpaceshipCommand)) *
		(1 - agilityEvasivePerLevel*skillLevel(levels, skillEvasiveManeuvering))
	if m < 0.05 {
		m = 0.05
	}
	return m
}

// droneShare is the fraction of the fit's drones this sheet can put in space
// at once. The stat engine counted an all-V launch (droneMaxLaunched); the
// Drones skill allows one per level, so a pilot at Drones II flies two of
// them however many the fit declares.
func droneShare(levels map[int64]int, launched int) float64 {
	if launched <= 0 {
		return 0
	}
	inSpace := math.Min(skillLevel(levels, skillDrones), float64(launched))
	return inSpace / float64(launched)
}

// weaponSkillScale is the DPS multiplier a sheet gets from the global weapon skills, as
// db.GetWeaponCombatBonuses computes it: attribute 292 raises damage per level, 293 and 441
// shorten cycle time. The stat engine bakes in no weapon skill, so an untrained sheet reads 1.
func weaponSkillScale(attrs map[int64]map[int64]float64, levels map[int64]int, ids []int64) float64 {
	dmg, cycle := 1.0, 1.0
	for _, id := range ids {
		lvl := skillLevel(levels, id)
		if lvl <= 0 {
			continue
		}
		a := attrs[id]
		if b := a[attrSkillDamageBonus]; b != 0 {
			dmg *= 1 + b/100*lvl
		}
		if b := a[attrSkillRofBonus]; b != 0 {
			cycle *= 1 + b/100*lvl
		}
		if b := a[attrSkillTurretSpeedBonus]; b != 0 {
			cycle *= 1 + b/100*lvl
		}
	}
	if cycle < 0.05 {
		cycle = 0.05
	}
	return dmg / cycle
}

// engagementShare is the fraction of a fight this sheet spends shooting rather than waiting
// for its lock, using the fight loop's lock formula against an ordinary subcapital signature.
// A hull with no scan resolution in the data answers 1.
func engagementShare(s *FitStats, attrs map[int64]map[int64]float64, levels map[int64]int) float64 {
	if s.ScanRes <= 0 {
		return 1
	}
	res := s.ScanRes
	if b := attrs[skillSignatureAnalysis][attrSkillScanResBonus]; b != 0 {
		res *= 1 + b/100*skillLevel(levels, skillSignatureAnalysis)
	}
	share := (combatEngagementSec - lockSecondsAt(res, combatDefaultTargetSig)) / combatEngagementSec
	if share < 0.05 {
		share = 0.05
	}
	if share > 1 {
		share = 1
	}
	return share
}

// supportEHP is the fit's effective hitpoints with the three tank skills rebased off the
// all-V basis of fitstats.go. Resist layers come from modules and hull and pass through
// unchanged; only the hit points under them move.
func supportEHP(s *FitStats, levels map[int64]int) float64 {
	resonance := func(res [4]float64) float64 {
		r := 1 - (res[0]+res[1]+res[2]+res[3])/4/100
		if r < 0.01 {
			r = 0.01
		}
		return r
	}
	hp := func(v, allV float64, skill int64) float64 {
		if allV <= 0 {
			return v
		}
		return v * (1 + supportHPPerLevel*skillLevel(levels, skill)) / allV
	}
	// The fit's own repairer output rides along unchanged: jobPower reads effectiveEHP
	// (fitlocalrep.go), so this model must include the same term.
	rep := effectiveEHP(s) - s.EHP
	return hp(s.ShieldHP, allVShieldHP, skillShieldManagement)/resonance(s.ShieldRes) +
		hp(s.ArmorHP, allVArmorHP, skillHullUpgrades)/resonance(s.ArmorRes) +
		hp(s.HullHP, allVHullHP, skillMechanic)/resonance(s.HullRes) + rep
}

// jobRef is the pilot's best ship at one job: the all-V readout the support
// model rebases, how many drones that fit can have out, and the score the
// value is denominated in.
type jobRef struct {
	Job     string
	Stats   *FitStats
	Drones  int     // drones counted as launched at the all-V basis
	Score   float64 // what this fit is worth to the pilot as it stands
	Weight  float64 // how much this pilot wants this kind of work
	FitName string
}

// supportPower is jobPower (fitscore.go) recomputed at a given skill sheet, term for term, so
// planning agrees with scoring. The one addition is the lock share on the combat lens, where
// the targeting skill lives; for every other lever it is a constant factor that cancels.
func supportPower(ref *jobRef, skillAttrs map[int64]map[int64]float64, levels map[int64]int) float64 {
	s := ref.Stats
	if s == nil {
		return 0
	}
	switch ref.Job {
	case JobMining:
		yield := s.MiningM3PerMin*miningYieldMult(levels)/allVMining +
			s.DroneMiningM3PerMin*droneShare(levels, ref.Drones)
		if yield <= 0 {
			return 0
		}
		return yield * (1 + s.CargoM3/miningHoldWorthM3)
	case JobTrade:
		if s.CargoM3 <= 0 {
			return 0
		}
		align := math.Max(s.AlignSec*agilityMult(levels)/(allVAgiSpCmd*allVAgiEvasive), 0)
		return s.CargoM3 * (1 + tradeAlignWorth/(tradeAlignWorth+align))
	case JobFreight:
		return math.Max(s.CargoM3, 0)
	case JobLogi:
		if s.RemoteRepPerSec <= 0 {
			return 0
		}
		capGJ, rechSec := supportCap(s, skillAttrs, levels)
		sustain := 1.0
		if rechSec > 0 && capGJ > 0 {
			sustain += math.Min(capGJ/rechSec/10, 1)
		}
		return s.RemoteRepPerSec * math.Sqrt(math.Max(supportEHP(s, levels), 1)/combatEHPBasis) * sustain
	}
	dps := s.TurretDPS*weaponSkillScale(skillAttrs, levels, turretSupportSkills) +
		s.LauncherDPS*weaponSkillScale(skillAttrs, levels, launcherSupportSkills) +
		s.DroneDPS*droneShare(levels, ref.Drones)
	surv := math.Sqrt(math.Max(supportEHP(s, levels), 1) / combatEHPBasis)
	switch ref.Job {
	case JobScout:
		// No skill in the planner's set moves a scanner's reach, a hold or a
		// stabiliser's strength, so the lens reads the same as the fit's own.
		return jobPower(JobScout, s)
	case JobTackle:
		if s.ScramStrength <= 0 {
			return 0
		}
		speed := math.Max(fitSpeed(supportLegs(s, levels)), 1) / 100
		return tackleFactor(s) * speed * surv * 100 * (1 + dps/200)
	case JobEwar:
		if s.EwarStrength <= 0 {
			return 0
		}
		return s.EwarStrength * 100 * surv * (1 + math.Min(dps/400, 0.5))
	}
	if dps <= 0 {
		return 0
	}
	dps *= engagementShare(s, skillAttrs, levels)
	// The standoff, rebased onto the pilot's own legs and missile reach: the one place
	// Navigation and the missile range skills move a fight.
	power := dps * surv * standoffFactor(supportReach(supportLegs(s, levels), skillAttrs, levels))
	if ref.Job == JobPiracy {
		power *= tackleFactor(s)
	}
	return power
}

// supportLegs is the fit's readout with its speeds rebased from the all-V Navigation of
// fitstats.go onto the pilot's own level. It returns a copy; the shared readout is never written.
func supportLegs(s *FitStats, levels map[int64]int) *FitStats {
	c := *s
	f := (1 + navigationPerLevel*skillLevel(levels, skillNavigation)) / allVVelocity
	c.MaxVelocity *= f
	c.PropVelocity *= f
	return &c
}

// supportReach rebases a fit's reach onto the pilot's own range skills. A launcher fit moves
// from the all-V missile basis to Missile Projection and Bombardment. A turret fit carries no
// skill in the stat engine, so Sharpshooter lifts the optimal half and Trajectory Analysis the
// falloff half of the shortest gun, as db.GetTurretSkillMults does.
func supportReach(s *FitStats, attrs map[int64]map[int64]float64, levels map[int64]int) *FitStats {
	if !s.ReachIsMissile {
		if s.ReachOptimalM <= 0 && s.ReachFalloffM <= 0 {
			return s
		}
		c := *s
		opt := s.ReachOptimalM * (1 + attrs[skillSharpshooter][attrSkillRangeBonus]/100*skillLevel(levels, skillSharpshooter))
		fall := s.ReachFalloffM * (1 + attrs[skillTrajectoryAnalysis][attrSkillFalloffBonus]/100*skillLevel(levels, skillTrajectoryAnalysis))
		c.WeaponReachM = opt + 2*fall
		return &c
	}
	c := *s
	v := 1 + attrs[skillMissileProjection][attrSkillMissileVelBonus]/100*skillLevel(levels, skillMissileProjection)
	f := 1 + attrs[skillMissileBombardment][attrSkillMissileFlightBonus]/100*skillLevel(levels, skillMissileBombardment)
	c.WeaponReachM = s.WeaponReachM * v * f / allVMissileRange
	return &c
}

// supportCap is the fit's capacitor and recharge time rebased from the all-V
// basis onto the pilot's own Energy Management and Energy Systems Operation.
func supportCap(s *FitStats, attrs map[int64]map[int64]float64, levels map[int64]int) (capGJ, rechSec float64) {
	capGJ = s.CapGJ / allVCapCapacity *
		(1 + attrs[skillEnergyManagement][attrSkillCapCapacityBonus]/100*skillLevel(levels, skillEnergyManagement))
	rechSec = s.CapRechargeSec / allVCapRecharge *
		(1 + attrs[skillEnergySystemsOp][attrSkillCapRechargeBonus]/100*skillLevel(levels, skillEnergySystemsOp))
	return capGJ, rechSec
}

// launchedDrones is how many of a fit's drones can be in space at once at the all-V basis,
// the number the stat engine counted.
func launchedDrones(f *Fit) int {
	n := 0
	for _, d := range f.Drones {
		n += int(d.Qty)
	}
	if n > droneMaxLaunched {
		n = droneMaxLaunched
	}
	return n
}

// ── the order book ──────────────────────────────────────────────────────────
//
// The trade skills touch no ship, so the fit pool cannot see them. The numbers below mirror
// db.GetMarketSkillLimits, so the planner agrees with the market service about how many
// orders a sheet allows.

const (
	orderSlotsBase        = 5
	orderSlotsPerTrade    = 4
	orderSlotsPerRetail   = 8
	orderSlotsPerWholesle = 16
	orderSlotsPerTycoon   = 32
	// The base broker fee and sales tax, and what a level takes off each.
	brokerFeeBase     = 0.01
	brokerFeePerLevel = 0.05
	salesTaxBase      = 0.01
	salesTaxPerLevel  = 0.10
)

// marketIndustryShare is how much a manufacturer cares about the order book against a
// trader. Selling is the tail of a factory's business, so keep it below a half. Every pilot
// has some manufacture disposition (tuning.go), so the term is small but never zero.
const marketIndustryShare = 0.35

// marketLeverSkills are the skills that set how much of the market a pilot can have open and
// how much of each sale it keeps. Margin Trading is left out: the module's traders size buys
// against the wallet, not escrow.
func marketLeverSkills() []int64 {
	return []int64{
		evedb.SkillTrade, evedb.SkillRetail, evedb.SkillWholesale, evedb.SkillTycoon,
		evedb.SkillBrokerRelations, evedb.SkillAccounting,
	}
}

// marketPower is listings times the share of each sale that survives fees: how much business
// the pilot can run at once and keep. Only the ratio of two readings is used. The reserve is
// the trader's own (tradebook.go), so the count is the listings it would actually place.
func marketPower(levels map[int64]int) float64 {
	slots := orderSlotsBase +
		orderSlotsPerTrade*skillLevel(levels, evedb.SkillTrade) +
		orderSlotsPerRetail*skillLevel(levels, evedb.SkillRetail) +
		orderSlotsPerWholesle*skillLevel(levels, evedb.SkillWholesale) +
		orderSlotsPerTycoon*skillLevel(levels, evedb.SkillTycoon)
	free := slots - orderSlotReserve
	if free < 1 {
		free = 1 // it can always work one listing at a time
	}
	fee := brokerFeeBase * (1 - brokerFeePerLevel*skillLevel(levels, evedb.SkillBrokerRelations))
	tax := salesTaxBase * (1 - salesTaxPerLevel*skillLevel(levels, evedb.SkillAccounting))
	keep := 1 - fee - tax
	if keep < 0.01 {
		keep = 0.01
	}
	return free * keep
}
