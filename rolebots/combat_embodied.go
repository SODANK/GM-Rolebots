package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"runtime/debug"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	"gurimoon/modapi"
	"gurimoon/npc"
	"gurimoon/rates"
)

// Embodied combat: anything that shoots a rolebot gets shot back at once, with everything the
// ship has. What separates ships is capability, not willingness: a hull with real guns
// commits and fights, while one with only mining lasers, a hauler, or anything that has lost
// the argument runs.
// The server's weapon and EWAR loops need a destinyConn (a player session), which a projection
// lacks, so hit rolls, ammo draw, capacitor, debuffs and effect broadcasts are reimplemented
// against the same evedb helpers and destiny.Entity API. The formulas match the player path
// (the client's GetAccuracy contract, DOGMA_MATH.md section 6), so a bot's Rifter hits exactly
// as hard as a player's.

const (
	// How often a fighting projection re-reads the world: retarget, re-check
	// range, decide whether to keep fighting or leave.
	combatTick = 1500 * time.Millisecond
	// How often an idle projection checks whether anything has started shooting at it: one bubble
	// scan, so a bot answers fire in about a second, not on the next 5-second sim pass.
	combatGuardTick = 1 * time.Second

	// An attacker stays "current" for this long after its last hit. Rats that
	// stop shooting (out of range, dead, jammed) age out and the bot stands
	// down instead of orbiting a ghost forever.
	combatAggroMemorySec = 25

	// Structure fractions at which a bot breaks off. An armed bot fights deep into structure;
	// anything that cannot shoot back leaves as soon as it is clearly attacked.
	combatFleeHullFracArmed   = 0.35
	combatFleeTankFracUnarmed = 0.80

	// Ceiling on lock time, so a bot with terrible scan resolution against a
	// tiny target still eventually shoots rather than hanging forever.
	combatMaxLockSec = 30.0
	// Floor, so nothing locks instantly.
	combatMinLockSec = 1.5

	// Grace on the EWAR range check, matching the player loop: a target
	// drifting on the edge must not flicker the effect on and off.
	combatEwarRangeGrace = 1.2

	// Default targeting range when the hull has no attribute for it.
	combatDefaultTargetRange = 30_000.0
	// Default turret signature resolution (matches the player loop).
	combatDefaultWeaponSig = 400.0
	// Default target signature (subcapital).
	combatDefaultTargetSig = 125.0
)

// groupStation is the mapDenormalize group a station celestial carries: where a bot in
// trouble runs.
const groupStation = 15

// Signature radii are per ship type and never change, so one query each is
// enough for the life of the process.
var (
	sigCacheMu sync.Mutex
	sigCache   = map[int64]float64{}
)

// EWAR classes the bot knows how to run. Anything fitted that is not one of these is not used.
const (
	ewNone = iota
	ewWeb
	ewScram
	ewJam
	ewTrackDisrupt
	ewDamp
	ewPaint
)

// botWeapon is one fitted gun or launcher, resolved to the numbers a shot needs. Each weapon
// runs its own cycle clock, so a blaster and a rocket launcher do not fire in lockstep.
type botWeapon struct {
	ItemID, TypeID, SlotFlag int64
	Name                     string
	EffectName               string
	FXGUID                   string
	IsMissile                bool

	CycleSec  float64
	Optimal   float64
	Falloff   float64
	Tracking  float64
	WeaponSig float64
	CapNeed   float64
	MaxRange  float64

	Dmg          *evedb.WeaponDamageProfile
	ChargeTypeID int64
	FlightSec    float64 // missiles: time from launch to impact
	// ExpRadius and ExpVel are the charge's explosion radius and velocity (attributes 654 and
	// 653), read for the abstract tier's missile application (application.go). The embodied
	// missileImpact does not read them.
	ExpRadius, ExpVel float64

	// What the pilot and the fit add to the gun (buildWeapon): DmgMult is skills x damage mods x
	// the hull's turret bonus, PerType the hull's per-type missile bonus; both default to 1. The
	// cycle and range folds are already in CycleSec and Optimal/Falloff/Tracking.
	DmgMult float64
	PerType [4]float64

	// Tracking needs the previous positions of both ships to derive angular
	// velocity, exactly as the player and NPC loops do.
	lastTargetPos destiny.Vec3
	lastSelfPos   destiny.Vec3
	lastSampleAt  time.Time

	nextFireAt time.Time
	firingAt   uint32 // target the client currently has FX running against
}

// botEwarModule is one fitted electronic warfare module.
type botEwarModule struct {
	ItemID, TypeID, SlotFlag int64
	Name                     string
	EffectName               string
	FXGUID                   string
	Kind                     int

	CycleSec float64
	MaxRange float64
	CapNeed  float64
	Attrs    map[int64]float64

	appliedTo   uint32
	nextCycleAt time.Time
}

// combatLoadout is everything a hull brings to a fight, plus the capacitor
// that pays for it.
type combatLoadout struct {
	weapons []*botWeapon
	ewar    []*botEwarModule
	// pointMode is where this tick's scram may go (pointclaims.go), set by
	// the combat tick before runEwarCycle.
	pointMode int

	capMax     float64
	capCur     float64
	capTauSec  float64 // capacitor recharge time constant
	lastCapAt  time.Time
	targetRnge float64
	scanRes    float64

	// dpsCache memoises the loadout's damage per second (combat_threat.go).
	// The threat assessment asks for it every tick of every fight and the
	// answer cannot change without the loadout being rebuilt.
	dpsCache float64

	// drones is the drone bay as it stood when the loadout was read, and
	// droneCap how many this pilot may put in space (drones.go). Combat
	// drones fight; mining drones ride along for the mining loop.
	drones   []botDrone
	droneCap int64

	// The rest of the rack (support_embodied.go): the engine, the local and remote repairers.
	// topSpeed is the speed with the engine lit, which the abstract duel's stance reads.
	prop     *botPropModule
	reps     []*botRepModule
	hullMass float64
	massAdd  float64
	velPcts  []float64
	topSpeed float64
	// capGift is capacitor another projection's energy transfer array put on
	// this ship, folded in at the next regen. Behind its own lock because it
	// arrives from the OTHER ship's combat goroutine.
	capGiftMu sync.Mutex
	capGift   float64
}

// armed reports whether this ship can actually make an attacker stop. Mining
// lasers do not count, which is the entire distinction between a bot that
// turns and fights and one that runs. Combat drones it can launch DO count:
// a Tristan with a bay of Hobgoblins is armed whatever its high slots hold.
func (l *combatLoadout) armed() bool {
	return l != nil && (len(l.weapons) > 0 || l.droneDPS() > 0)
}

// droneDPS is what this loadout's combat drones put out per second with as many in space as
// the pilot may launch, on the same basis as the guns' dps() before rates.PlayerDamage.
func (l *combatLoadout) droneDPS() float64 {
	if l == nil {
		return 0
	}
	return launchedOutput(l.drones, grpCombatDrone, l.droneCap)
}

// combatDronesLaunchable is how many combat drones this loadout puts in
// space: the bay's combat drones, capped at the pilot's Drones level.
func (l *combatLoadout) combatDronesLaunchable() int64 {
	if l == nil {
		return 0
	}
	var n int64
	for _, p := range planDroneLaunch(l.drones, grpCombatDrone, l.droneCap) {
		n += p.N
	}
	return n
}

// ── loading ─────────────────────────────────────────────────────────────────

// effectRow is one dgmEffects row a module carries.
type effectRow struct {
	Name string
	GUID string
	// DurationAttr is dgmEffects.durationAttributeID: which attribute times
	// this effect's cycle (51 `speed` for the weapon effects, 73 `duration`
	// for nearly everything else). 0 when the row carries none.
	DurationAttr int64
}

// moduleEffects reads the effects a module type carries, with the client GUID each draws.
// The effect name decides how the mod treats the module, as in the server's activation handler.
func (m *Mod) moduleEffects(ctx context.Context, typeID int64) ([]effectRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.effectName, COALESCE(e.guid, ''), COALESCE(e.durationAttributeID, 0)
		FROM dgmTypeEffects te
		JOIN dgmEffects e ON e.effectID = te.effectID
		WHERE te.typeID = ?`, typeID)
	if err != nil {
		return nil, fmt.Errorf("module effects %d: %w", typeID, err)
	}
	defer rows.Close()
	var out []effectRow
	for rows.Next() {
		var er effectRow
		if err := rows.Scan(&er.Name, &er.GUID, &er.DurationAttr); err != nil {
			return nil, err
		}
		out = append(out, er)
	}
	return out, rows.Err()
}

// weaponCycleSec is a weapon's cycle in seconds. Weapons cycle on speed (51), not duration
// (73): 51 is the durationAttributeID of targetAttack, projectileFired, useMissiles and
// barrage, and turrets and launchers carry no 73. Order: the effect's named attribute when
// present and positive, else 73, else 51, else the fallback.
func weaponCycleSec(attrs map[int64]float64, durationAttr int64, fallback float64) float64 {
	if durationAttr > 0 {
		if v := attrs[durationAttr]; v > 0 {
			return v / 1000
		}
	}
	if v := attrs[dogma.AttrDuration]; v > 0 {
		return v / 1000
	}
	if v := attrs[dogma.AttrSpeed]; v > 0 {
		return v / 1000
	}
	return fallback
}

// ewarKindFor maps an effect name to the EWAR class the bot runs for it.
func ewarKindFor(effectName string) int {
	switch effectName {
	case dogma.EffNameDecreaseTargetSpeed, dogma.EffNameModifyTargetSpeed:
		return ewWeb
	case dogma.EffNameWarpScramble, dogma.EffNameWarpScrambler:
		return ewScram
	case dogma.EffNameECMJam:
		return ewJam
	case dogma.EffNameTrackingDisrupt:
		return ewTrackDisrupt
	case dogma.EffNameSensorDamp:
		return ewDamp
	case dogma.EffNameTargetPaint:
		return ewPaint
	}
	return ewNone
}

// weaponEffect reports whether an effect name is a gun, and whether it throws
// missiles rather than shooting.
func weaponEffect(effectName string) (isWeapon, isMissile bool) {
	switch effectName {
	case dogma.EffNameLaserFired, dogma.EffNameProjectileFired, dogma.EffNameHybridFired:
		return true, false
	case dogma.EffNameMissileLaunching, dogma.EffNameUseMissiles:
		return true, true
	}
	return false, false
}

// fxGUIDFor normalises a module's client effect GUID, falling back to the
// hand-written table when the database row carries none.
func fxGUIDFor(effectName, dbGUID string) string {
	if dbGUID == "" {
		dbGUID, _ = dogma.FallbackGUID(effectName)
	}
	return dogma.NormalizeClientGUID(dbGUID)
}

// targetingSkillMult is one targeting skill's multiplier for this pilot: the
// per-level percentage read off the skill itself, never asserted here, so the
// module and the server cannot drift about what a level is worth. 1.0 when
// the skill is untrained or carries no bonus.
func (m *Mod) targetingSkillMult(ctx context.Context, charID, skillID, bonusAttr int64) float64 {
	lvl, err := m.charSkillLevelFor(ctx, charID, skillID)
	if err != nil || lvl <= 0 {
		return 1
	}
	attrs, err := evedb.GetTypeAttributes(ctx, m.db, skillID)
	if err != nil {
		return 1
	}
	bonus := attrs[bonusAttr]
	if bonus <= 0 {
		return 1
	}
	return 1 + bonus/100*float64(lvl)
}

// loadCombatLoadout resolves everything on the hull the bot can fight with.
// A module whose numbers cannot be read is skipped and logged rather than
// guessed at: firing a gun with invented damage would be worse than not
// having it.
func (m *Mod) loadCombatLoadout(ctx context.Context, charID, shipID, shipTypeID int64) (*combatLoadout, error) {
	fitted, err := evedb.GetFittedModules(ctx, m.db, shipID)
	if err != nil {
		return nil, fmt.Errorf("fitted modules: %w", err)
	}
	// A gun that ran dry on the last fight is reloaded from the hold before it is read: buildWeapon
	// drops an empty gun, and a ship with its hold full of rounds would read as unarmed (gunreload.go).
	m.reloadEmptyGuns(ctx, charID, shipID, fitted)

	l := &combatLoadout{
		targetRnge: combatDefaultTargetRange,
		scanRes:    100,
		lastCapAt:  time.Now(),
	}

	// Capacitor: the pool every activation is paid out of. A bot that runs
	// dry stops shooting, the same as a player who overheats his cap.
	if hullAttrs, err := evedb.GetTypeAttributes(ctx, m.db, shipTypeID); err == nil {
		l.capMax = hullAttrs[dogma.AttrCapacity]
		l.capTauSec = hullAttrs[dogma.AttrRechargeRate] / 1000.0
		if r := hullAttrs[dogma.AttrMaxTargetRange]; r > 0 {
			l.targetRnge = r
		}
		if s := hullAttrs[dogma.AttrScanResolution]; s > 0 {
			l.scanRes = s
		}
		// The pilot's targeting skills, as the server folds them for a player: Long Range Targeting
		// on max target range, Signature Analysis on scan resolution (main.go AddTarget, GetShipStats).
		l.targetRnge *= m.targetingSkillMult(ctx, charID,
			dogma.SkillLongRangeTargeting, dogma.AttrMaxTargetRangeBonus)
		l.scanRes *= m.targetingSkillMult(ctx, charID,
			dogma.SkillSignatureAnalysis, dogma.AttrScanResolutionBonus)
	} else {
		rlogf("rolebots: combat: hull attrs type=%d: %v", shipTypeID, err)
	}
	if l.capMax <= 0 {
		l.capMax = 250
	}
	if l.capTauSec <= 0 {
		l.capTauSec = 150
	}
	// The pilot's capacitor skills, as the server folds them (db.GetCapacitorSkillMultipliers):
	// Energy Management on the pool, Energy Systems Operation on the recharge.
	if capMult, rechMult, err := evedb.GetCapacitorSkillMultipliers(ctx, m.db, charID); err == nil {
		l.capMax *= capMult
		l.capTauSec *= rechMult
	}
	l.capCur = l.capMax
	// What the engine has to push: the hull's own mass (the server's prop arm
	// reads the ball's mass; invTypes.mass is that number for a fresh hull).
	l.hullMass = m.typeMassOf(ctx, shipTypeID)

	// The hull's weapon bonuses, read once per ship and lazily: the per-weapon fold is memoized
	// (weaponBonusesFor), so a warm loadout pays for no lookup.
	var (
		hullB      evedb.HullBonuses
		hullLoaded bool
	)
	hullFor := func() evedb.HullBonuses {
		if !hullLoaded {
			hullLoaded = true
			var err error
			if hullB, err = evedb.GetShipHullBonuses(ctx, m.db, charID, shipTypeID); err != nil {
				rlogf("rolebots: combat: hull bonuses type=%d: %v", shipTypeID, err)
			}
		}
		return hullB
	}

	for _, fm := range fitted {
		effects, err := m.moduleEffects(ctx, fm.TypeID)
		if err != nil {
			rlogf("rolebots: combat: %v", err)
			continue
		}
		attrs, err := evedb.GetTypeAttributes(ctx, m.db, fm.TypeID)
		if err != nil {
			rlogf("rolebots: combat: module attrs type=%d: %v", fm.TypeID, err)
			continue
		}
		name, _ := evedb.GetTypeName(ctx, m.db, fm.TypeID)
		notePassiveSpeed(l, fm.GroupID, attrs)

		for _, eff := range effects {
			if isWeapon, isMissile := weaponEffect(eff.Name); isWeapon {
				if w := m.buildWeapon(ctx, charID, shipID, fm, attrs, eff, isMissile, name, hullFor); w != nil {
					l.weapons = append(l.weapons, w)
				}
				break
			}
			if kind := ewarKindFor(eff.Name); kind != ewNone {
				l.ewar = append(l.ewar, &botEwarModule{
					ItemID: fm.ItemID, TypeID: fm.TypeID, SlotFlag: fm.Flag,
					Name: name, EffectName: eff.Name, FXGUID: fxGUIDFor(eff.Name, eff.GUID),
					Kind:     kind,
					CycleSec: durationSecOr(attrs[dogma.AttrDuration], 5),
					MaxRange: rangeOr(attrs[dogma.AttrMaxRange], 10_000),
					CapNeed:  attrs[dogma.AttrCapacitorNeed],
					Attrs:    attrs,
				})
				break
			}
			if supportFromEffect(l, fm, attrs, eff, name) {
				break
			}
		}
	}
	// Tracking computers and enhancers on the turrets, sensor boosters on the lock: the server's folds
	// (db.GetTurretTrackingMults, db.GetActiveSensorBoosterMult), every fitted module counted as running.
	active := m.fittedActiveSet(ctx, shipID)
	if rm, tm, err := evedb.GetTurretTrackingMults(ctx, m.db, shipID, active); err == nil && (rm != 1 || tm != 1) {
		for _, w := range l.weapons {
			if w.IsMissile {
				continue
			}
			w.Optimal *= rm
			w.Tracking *= tm
			w.MaxRange = w.Optimal + 2*w.Falloff
		}
	}
	if sb, err := evedb.GetActiveSensorBoosterMult(ctx, m.db, shipID, active); err == nil && sb > 0 {
		l.scanRes *= sb
	}
	// Signal amplifiers (passive) raise scan resolution and lock range, and a running sensor
	// booster raises range as well as resolution (db.GetShipPassiveBonuses, AddTarget).
	if pb, err := evedb.GetShipPassiveBonuses(ctx, m.db, shipID); err == nil {
		if pb.ScanResMult > 0 {
			l.scanRes *= pb.ScanResMult
		}
		if pb.TargetRangeMult > 0 {
			l.targetRnge *= pb.TargetRangeMult
		}
	}
	if sr, err := evedb.GetActiveSensorBoosterRangeMult(ctx, m.db, shipID, active); err == nil && sr > 0 {
		l.targetRnge *= sr
	}
	// Local repairers get the same skill, amplifier and hull terms the server gives a player's
	// module, so the embodied cycles and the abstract loadoutRepRates read the real rate.
	for _, r := range l.reps {
		if r.Remote {
			// Remote reps get Repair Systems and the emission skills' cycle
			// and capacitor terms (db.GetRemoteRepairBonuses), never an amount.
			cm, km := evedb.GetRemoteRepairBonuses(ctx, m.db, charID, r.TypeID)
			foldRemoteRepBonuses(r, cm, km)
			continue
		}
		am, cm, km, err := evedb.GetRepairModuleBonuses(ctx, m.db, charID, shipID, r.TypeID, r.Kind == repShield)
		if err != nil {
			rlogf("rolebots: combat: repair bonuses type=%d: %v", r.TypeID, err)
			continue
		}
		foldLocalRepBonuses(r, am, cm, km, hullFor())
	}
	if hullAttrs, err := evedb.GetTypeAttributes(ctx, m.db, shipTypeID); err == nil {
		nav := 0
		if id, ok := m.skillIDByNameCached(ctx, "Navigation"); ok {
			nav, _ = m.charSkillLevelFor(ctx, charID, id)
		}
		l.topSpeed = loadoutTopSpeed(l, hullAttrs[attrMaxVelocity], nav)
	}

	// The drone bay. A read failure leaves the loadout guns-only rather than failing the fight.
	if bay, err := m.shipBayDrones(ctx, shipID); err != nil {
		rlogf("rolebots: combat: drone bay ship=%d: %v", shipID, err)
	} else if len(bay) > 0 {
		l.drones = bay
		l.droneCap = m.droneLaunchCap(ctx, charID)
	}
	return l, nil
}

// buildWeapon resolves one gun into the numbers a shot needs, nil when the module has no
// damage behind it (an empty launcher, a mining laser sharing an effect name). The pilot is
// part of the gun: skills, damage mods and hull bonus come from db.GetWeaponCombatBonuses and
// HullBonuses.WeaponMults, the calls the player path makes (main.go bound.Activate).
func (m *Mod) buildWeapon(ctx context.Context, charID, shipID int64, fm evedb.FittedModule,
	attrs map[int64]float64, eff effectRow, isMissile bool, name string,
	hullFor func() evedb.HullBonuses) *botWeapon {

	chargeTypeID, _ := evedb.GetChargeTypeID(ctx, m.db, shipID, fm.Flag)
	if chargeTypeID != 0 && !m.chargeAccepts(ctx, chargeTypeID, fm.TypeID) {
		// A round this gun cannot fire is not in it as far as the fight is concerned, and a civilian
		// gun's own damage must not be lost (fitslotcharge.go).
		chargeTypeID = 0
	}
	dmg, err := evedb.GetWeaponDamageProfile(ctx, m.db, fm.TypeID, chargeTypeID)
	if err != nil {
		rlogf("rolebots: combat: damage profile type=%d charge=%d: %v", fm.TypeID, chargeTypeID, err)
		return nil
	}
	if dmg == nil || dmg.Total() <= 0 {
		// A launcher with no missiles in it, or a turret with no crystal. The
		// bot will fight with whatever else it has.
		return nil
	}

	w := &botWeapon{
		ItemID: fm.ItemID, TypeID: fm.TypeID, SlotFlag: fm.Flag,
		Name: name, EffectName: eff.Name, FXGUID: fxGUIDFor(eff.Name, eff.GUID),
		IsMissile:    isMissile,
		CycleSec:     weaponCycleSec(attrs, eff.DurationAttr, 5),
		Optimal:      attrs[dogma.AttrMaxRange],
		Falloff:      attrs[dogma.AttrFalloff],
		Tracking:     attrs[dogma.AttrTrackingSpeed],
		CapNeed:      attrs[dogma.AttrCapacitorNeed],
		Dmg:          dmg,
		ChargeTypeID: chargeTypeID,
		DmgMult:      1,
		PerType:      [4]float64{1, 1, 1, 1},
	}

	b := m.weaponBonusesFor(ctx, charID, shipID, fm.TypeID, chargeTypeID, isMissile, hullFor)
	w.DmgMult = b.dmg
	w.PerType = b.hull.PerType
	w.CycleSec *= b.cycle
	w.Optimal *= b.hull.Optimal * b.skill.Optimal
	w.Falloff *= b.hull.Falloff * b.skill.Falloff
	w.Tracking *= b.hull.Tracking * b.skill.Tracking
	w.CapNeed *= b.hull.CapNeed * b.skill.CapNeed

	w.WeaponSig = combatDefaultWeaponSig
	if s := attrs[dogma.AttrOptimalSigRadius]; s > 0 {
		w.WeaponSig = s
	} else if s := attrs[dogma.AttrSignatureRadius]; s > 0 {
		w.WeaponSig = s
	}

	if isMissile {
		// A missile's reach is how far it flies before it expires, not an optimal, and it must cross
		// that distance first.
		if mfp, err := evedb.GetMissileFlightParams(ctx, m.db, chargeTypeID); err == nil && mfp != nil {
			// Missile Projection and Missile Bombardment and the hull's missile range bonus: the
			// server's MissileRangeMults, which a player's launcher reads.
			vMul, fMul := 1.0, 1.0
			if shipType := m.entityTypeID(ctx, shipID); shipType != 0 {
				vMul, fMul = evedb.MissileRangeMults(ctx, m.db, charID, shipType, fm.GroupID)
			}
			w.MaxRange = mfp.Velocity * vMul * mfp.MaxFlightTime * fMul
			w.FlightSec = mfp.MaxFlightTime * fMul
			w.ExpRadius, w.ExpVel = mfp.AoeCloudSize, mfp.AoeVelocity
		}
		if w.MaxRange <= 0 {
			w.MaxRange = 20_000
		}
	} else {
		w.MaxRange = w.Optimal + 2*w.Falloff
		if w.MaxRange <= 0 {
			w.MaxRange = 10_000
		}
	}
	return w
}

// ── the weapon-bonus memo ───────────────────────────────────────────────────
// A gun's worth in this pilot's hands is a dozen queries asked on every site search (threat.go), so the fold
// is memoized per (pilot, ship, weapon, charge); staleness is bounded by weaponBonusTTLSec.
type weaponBonusKey struct{ charID, shipID, typeID, chargeID int64 }

type weaponBonus struct {
	dmg   float64 // skills × damage mods × the hull's turret bonus
	cycle float64 // skills × damage mods × the hull's RoF bonus
	hull  evedb.HullWeaponMults
	// skill is the pilot's turret range skills (Sharpshooter, Trajectory Analysis, Motion
	// Prediction, Controlled Bursts). It is its own field because HullWeaponMults is built by
	// keyed literal here, so a new field there would read 0 in every copy. Identity for a missile.
	skill evedb.TurretSkillMults
	at    int64
}

const (
	weaponBonusTTLSec = 300
	// Bounded so a long-running server cannot accumulate a row per (pilot, hull, gun, ammo). At
	// the cap the whole map is dropped, not swept: each entry is one cheap re-derivation away.
	weaponBonusCacheMax = 50_000
)

var (
	weaponBonusMu    sync.Mutex
	weaponBonusCache = map[weaponBonusKey]weaponBonus{}
)

// identityWeaponBonus is what an unbonused gun looks like: everything ×1.
func identityWeaponBonus() weaponBonus {
	return weaponBonus{
		dmg: 1, cycle: 1,
		hull: evedb.HullWeaponMults{
			Dmg: 1, PerType: [4]float64{1, 1, 1, 1},
			Cycle: 1, Optimal: 1, Falloff: 1, Tracking: 1, CapNeed: 1,
		},
		skill: evedb.IdentityTurretSkillMults(),
	}
}

// weaponBonusesFor resolves, and caches, everything the pilot and the fit add to one gun.
// hullFor is called only on a miss. A failed lookup returns the identity rather than nothing
// and is not cached, so a transient database error does not pin a pilot at unskilled.
func (m *Mod) weaponBonusesFor(ctx context.Context, charID, shipID, typeID, chargeTypeID int64,
	isMissile bool, hullFor func() evedb.HullBonuses) weaponBonus {

	key := weaponBonusKey{charID, shipID, typeID, chargeTypeID}
	now := clock.Unix()

	weaponBonusMu.Lock()
	if v, ok := weaponBonusCache[key]; ok && now-v.at < weaponBonusTTLSec {
		weaponBonusMu.Unlock()
		return v
	}
	weaponBonusMu.Unlock()

	out := identityWeaponBonus()
	groupID, _, err := evedb.GetTypeGroupCategory(ctx, m.db, typeID)
	if err != nil {
		rlogf("rolebots: combat: weapon group type=%d: %v", typeID, err)
		return out
	}
	dmgMult, cycleMult, err := evedb.GetWeaponCombatBonuses(
		ctx, m.db, charID, shipID, typeID, groupID, chargeTypeID, !isMissile)
	if err != nil {
		rlogf("rolebots: combat: weapon bonuses type=%d: %v", typeID, err)
		return out
	}
	out.dmg, out.cycle = dmgMult, cycleMult
	if !isMissile {
		out.skill = evedb.GetTurretSkillMults(ctx, m.db, charID)
	}

	// Hull bonuses matching this weapon's class (a Rifter's small projectile bonus, a Kestrel's missile bonuses).
	// Turret specs match on the weapon's required skills, missile ones on the launcher's group.
	if hullB := hullFor(); len(hullB) > 0 {
		reqSkills := map[int64]bool{}
		if reqs, err := evedb.GetRequiredSkills(ctx, m.db, typeID); err == nil {
			for _, r := range reqs {
				reqSkills[r.SkillID] = true
			}
		}
		out.hull = hullB.WeaponMults(reqSkills, groupID, isMissile)
		out.dmg *= out.hull.Dmg
		out.cycle *= out.hull.Cycle
	}

	out.at = now
	weaponBonusMu.Lock()
	if len(weaponBonusCache) >= weaponBonusCacheMax {
		weaponBonusCache = map[weaponBonusKey]weaponBonus{}
	}
	weaponBonusCache[key] = out
	weaponBonusMu.Unlock()
	return out
}

// shotDamage is one cycle's damage by type: the gun's multiplier, the pilot's skills, the
// fitted damage mods and the hull's bonuses. It is the one place that knows what multiplies
// what, shared by the turret shot, the missile impact and botCombatant's DPS estimate, so
// they cannot disagree.
func (w *botWeapon) shotDamage() (em, exp, kin, therm float64) {
	if w.Dmg == nil {
		return 0, 0, 0, 0
	}
	k := w.Dmg.Multiplier * w.DmgMult
	return w.Dmg.Em * k * w.PerType[0],
		w.Dmg.Explosive * k * w.PerType[1],
		w.Dmg.Kinetic * k * w.PerType[2],
		w.Dmg.Thermal * k * w.PerType[3]
}

// cycleDamage is shotDamage summed: what one full cycle puts on a target
// before the accuracy roll.
func (w *botWeapon) cycleDamage() float64 {
	em, exp, kin, therm := w.shotDamage()
	return em + exp + kin + therm
}

func durationSecOr(ms, fallback float64) float64 {
	if ms <= 0 {
		return fallback
	}
	return ms / 1000
}

func rangeOr(v, fallback float64) float64 {
	if v <= 0 {
		return fallback
	}
	return v
}

// ── capacitor ───────────────────────────────────────────────────────────────

// regenCap advances the capacitor to now and reports the level. Uses the same
// exponential the shield does, with the pool's own time constant.
func (l *combatLoadout) regenCap() float64 {
	l.capGiftMu.Lock()
	if l.capGift > 0 {
		l.capCur = math.Min(l.capMax, l.capCur+l.capGift)
		l.capGift = 0
	}
	l.capGiftMu.Unlock()
	now := time.Now()
	dt := now.Sub(l.lastCapAt).Seconds()
	l.lastCapAt = now
	if dt <= 0 || l.capMax <= 0 {
		return l.capCur
	}
	tau := l.capTauSec / 5.0
	if tau <= 0 {
		return l.capCur
	}
	// dC/dt toward full, integrated over dt.
	l.capCur += (l.capMax - l.capCur) * (1 - math.Exp(-dt/tau))
	if l.capCur > l.capMax {
		l.capCur = l.capMax
	}
	return l.capCur
}

// drawCap pays for one activation, or reports that the pool cannot cover it.
func (l *combatLoadout) drawCap(need float64) bool {
	if need <= 0 {
		return true
	}
	l.regenCap()
	if l.capCur < need {
		return false
	}
	l.capCur -= need
	return true
}

// ── engagement ──────────────────────────────────────────────────────────────

// startEmbodyGuard puts a projection on watch. The guard runs for the projection's life and
// costs a grid scan a second, so a bot answers fire at once and not on the 5-second sim pass.
func (m *Mod) startEmbodyGuard(eb *embodiedBot) {
	eb.mu.Lock()
	if eb.combatCancel != nil {
		eb.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	eb.combatCancel = cancel
	eb.mu.Unlock()
	m.goSafeBot(eb.charID, "combat loop", func() { m.embodyCombatLoop(ctx, eb) })
}

// stopEmbodyCombat stands the bot down and drops every effect it was running.
func (m *Mod) stopEmbodyCombat(eb *embodiedBot) {
	eb.mu.Lock()
	cancel := eb.combatCancel
	eb.combatCancel = nil
	eb.fighting = false
	eb.fightSince = time.Time{}
	eb.combatTarget = 0
	eb.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// underAttack reports the entity IDs that have hit this bot recently, newest
// first. The kill-report ledger every damage path already writes is the
// aggressor list: NPCs and players both land in it, so "whoever is shooting
// me" needs no bookkeeping of its own.
func (eb *embodiedBot) underAttack() []uint32 {
	if eb.ent == nil {
		return nil
	}
	cutoff := time.Now().Unix() - combatAggroMemorySec
	var out []uint32
	for _, h := range eb.ent.KillHitsSnapshot() {
		if h.Time < cutoff {
			continue
		}
		if id, ok := eb.attackerEntityID(h); ok {
			out = append(out, id)
		}
	}
	return out
}

// attackerEntityID resolves one kill-report hit back to the ball on this grid that made it.
func (eb *embodiedBot) attackerEntityID(h destiny.KillHit) (uint32, bool) {
	if eb.ss == nil {
		return 0, false
	}
	// A hit from a ball names it (destiny.KillHit.EntityID): exact, even with three rats of one type on
	// the grid. Gone or off this grid, it resolves to nothing; a lookalike must not stand in for it.
	if h.EntityID != 0 {
		for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
			if e.ID == h.EntityID && !e.IsDestroyed() {
				return e.ID, true
			}
		}
		return 0, false
	}
	// Drone and mine hits name only an owner: match the ship back by type and pilot.
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if e.Slim.TypeID == h.ShipTypeID && (h.IsNPC != e.IsPlayer) {
			if h.CharID != 0 && e.Slim.CharID != h.CharID {
				continue
			}
			return e.ID, true
		}
	}
	return 0, false
}

// gridHostile is one ball on the grid the bot has an opinion about. Aggro separates a ball
// that is shooting the bot, or is a target by the posture's own job, from one that is merely
// visible, so a miner can look at rats on its belt and not only be hit by them.
type gridHostile struct {
	ID    uint32
	Aggro bool
	// Assist marks an aggro ball that is here only because it is shooting a friend
	// (combat_assist.go).
	Assist bool
	// Elective marks an aggro ball the pilot chose to take on rather than one fighting it: a friend's
	// attacker (Assist) or a war target met in transit (warTargetsOnGrid). An unarmed pilot sits
	// those out instead of fleeing (electiveOnly).
	Elective bool
	// Police marks law enforcement that has hit or locked this pilot. Only a reckless, violent outlaw
	// (Tuning.FightsPolice) shoots back; anyone else leaves.
	Police bool
	// Provoked marks a ball that has hit or locked this pilot: a fight brought to it, as opposed to
	// one it would be choosing (chosenFight).
	Provoked bool
}

// stayOutLogEvery spaces the "staying out of it" line for a pilot that keeps declining fights.
const stayOutLogEvery = 5 * time.Minute

// chosenFight reports whether every ball fighting the pilot is one it would be picking a fight
// with (a war target, a friend's attacker, prey, a rule-of-engagement target, rats on a patrol's
// belt) and none has yet hit or locked it. Only a chosen fight can be declined.
func chosenFight(hs []gridHostile) bool {
	found := false
	for _, h := range hs {
		if !h.Aggro {
			continue
		}
		if h.Provoked {
			return false
		}
		found = true
	}
	return found
}

// policeEngaged reports whether law enforcement is among the balls fighting the bot.
func policeEngaged(hs []gridHostile) bool {
	for _, h := range hs {
		if h.Aggro && h.Police {
			return true
		}
	}
	return false
}

// aggroIDs is the subset that is actually fighting the bot: what the engage machinery may act on.
func aggroIDs(hs []gridHostile) []uint32 {
	var out []uint32
	for _, h := range hs {
		if h.Aggro {
			out = append(out, h.ID)
		}
	}
	return out
}

// lawEnforcementGroup reports whether an NPC group is police: 446 Customs Official (gate
// customs, CONCORD's included), 301 Concord Drone, 182 Police Drone and 99 Sentry Gun (the six
// empire station gun types). gridNPCHostileTo cannot answer this: it reads hostile for a pilot
// with no NPC faction, right for a rat and wrong for a cop. See docs/comment-notes-b02.md#law-enforcement-group.
func lawEnforcementGroup(groupID int64) bool {
	switch groupID {
	case 446, 301, 182, 99:
		return true
	}
	return false
}

// hostilesOnGrid is everything the bot has grounds to shoot, plus, for a non-patrol posture,
// everything it can merely see. Whatever has hit it and every NPC that has it locked is aggro.
// The rest of the bubble's NPCs come back non-aggro for a working bot, for assessGrid to
// weigh. NPCs this pilot's employer is not at odds with do not come back at all (faction.go).
func (m *Mod) hostilesOnGrid(eb *embodiedBot) []gridHostile {
	seen := map[uint32]bool{}
	var out []gridHostile
	add := func(id uint32, aggro bool) {
		if id == 0 {
			return
		}
		if seen[id] {
			if aggro {
				// Promote: seen first as scenery, now shooting.
				for i := range out {
					if out[i].ID == id {
						out[i].Aggro = true
						out[i].Assist = false
						out[i].Elective = false
						break
					}
				}
			}
			return
		}
		seen[id] = true
		out = append(out, gridHostile{ID: id, Aggro: aggro})
	}
	// markPolice flags a ball already listed as law enforcement (lawEnforcementGroup).
	markPolice := func(id uint32) {
		e := eb.ss.GetEntity(id)
		if e == nil || e.IsPlayer || !lawEnforcementGroup(e.Slim.GroupID) {
			return
		}
		for i := range out {
			if out[i].ID == id {
				out[i].Police = true
				return
			}
		}
	}
	// markProvoked flags a listed ball as one that brought the fight (chosenFight).
	markProvoked := func(id uint32) {
		for i := range out {
			if out[i].ID == id {
				out[i].Provoked = true
				return
			}
		}
	}
	for _, id := range eb.underAttack() {
		add(id, true)
		markProvoked(id)
		if eb.ss != nil {
			markPolice(id)
		}
	}
	// A pirate does not wait to be shot at: the pilots on its grid are the job (piracy_prey.go).
	// They go in after the genuine aggressors, so a losing pirate still prioritises whoever hurts it.
	if eb.prey {
		for _, id := range m.preyOnGrid(eb) {
			add(id, true)
		}
	}
	// A combat patrol does not wait either: after the genuine aggressors it adds the pilots its
	// rules of engagement allow (roe.go), then a shared site's rivals, decided once at arrival (siteetiquette.go).
	for _, id := range m.siteRivalsOnGrid(eb) {
		add(id, true)
	}
	if eb.engage {
		for _, id := range m.engageablePilots(eb) {
			add(id, true)
		}
	} else {
		// Everyone else answers a crime: a pilot flagged criminal or with an outlaw record, fair game in every band
		// (roe.go). The doctrine gate in wouldEngagePilot applies: a corporation with no combat wing shoots nobody.
		for _, id := range m.flaggedPilotsOnGrid(eb) {
			add(id, true)
		}
	}
	// A pilot in transit whose corporation is at war may choose to take on the enemy it meets
	// (warTargetsOnGrid): a choice, so an unarmed ship sits it out rather than flees (electiveOnly).
	for _, id := range m.warTargetsOnGrid(eb) {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, gridHostile{ID: id, Aggro: true, Elective: true})
	}
	if m.npc != nil && eb.ent != nil && eb.ss != nil {
		for _, id := range m.npc.GetTargetersOf(eb.ent.ID) {
			add(id, true)
			markProvoked(id)
			markPolice(id)
		}
		// A patrol does not wait to be shot at: every managed NPC on its grid is a target; for everyone else they are
		// sighted, not engaged. The loops above already marked anything that hit or locked this bot as aggro.
		for _, e := range eb.ss.GetBubbleNPCs(eb.ent.ID) {
			if !m.npc.IsNPC(e.ID) {
				continue
			}
			// Scenery: a wall or a container carries no faction, which reads as hostile under the shared
			// rule. The core exempts the same groups (npc/manager.go).
			if npc.IsInertStructure(e.Slim.GroupID) {
				continue
			}
			// The law: Customs officers and CONCORD shoot only a smuggler or a criminal, and one that has
			// locked this pilot is already listed, marked Police, by the loops above. See lawEnforcementGroup.
			if lawEnforcementGroup(e.Slim.GroupID) {
				continue
			}
			// Its employer's friends are not its enemies (faction.go).
			if !m.gridNPCHostileTo(eb, e) {
				continue
			}
			add(e.ID, eb.patrol)
		}
	}
	// The site this projection is running (siteembodied.go): its rats in its room, and every structure holding a
	// prize, are targets. Then the fleet this pilot serves (conscript_embodied.go), after its own attackers.
	for _, id := range m.siteTargetsOnGrid(eb) {
		add(id, true)
	}
	for _, id := range m.fleetAggressors(eb) {
		add(id, true)
	}
	// What a human commander called (fctags.go): a tag, a lock under "attack", a named rat.
	for _, id := range m.commandedHostiles(eb) {
		add(id, true)
	}
	// A corpmate, alliance mate or blue being shot on this grid (combat_assist.go). After the
	// fleet, so whatever hurts this bot comes first. Only a ball not listed above is marked Assist.
	for _, id := range m.assistAggressors(eb) {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, gridHostile{ID: id, Aggro: true, Assist: true, Elective: true})
	}
	// For a working bot only, the hostile pilots it can merely see (corprel.go). They are non-aggro, so
	// assessSightedGrid (combat_threat.go) leaves on a sighting, and add() cannot demote a shooting pilot.
	if !eb.patrol && !eb.prey && !eb.engage && eb.ent != nil && eb.ss != nil &&
		(relHasEnemies(eb.corpID) || outlookHasRivals(eb.profile) || personalHasRivals(eb.charID)) {
		// The guard is the whole cost of this on a quiet shard: it runs once per combat tick per
		// working bot. The arms are what gridStandingToPilot folds: registry, faction matrix, grace ledger.
		for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
			if e == nil || e.Slim.CharID == 0 || e.Slim.CharID == eb.charID {
				continue
			}
			if e.IsDestroyed() || m.gridKin(eb, e) {
				continue
			}
			if m.gridStandingToPilot(eb, e) > StandingDisliked {
				continue
			}
			add(e.ID, false)
		}
	}
	return out
}

// friendlyGridLogEvery throttles the patrol-with-nothing-to-shoot line.
const friendlyGridLogEvery = 30 * time.Minute

// noteFriendlyGrid says once, occasionally, that a patrol is sharing its grid
// with rats it will not fire on because they work for the same people it
// does. Only for a patrol posture: a miner standing quietly among friendly
// rats is working, not idle, and needs no explanation.
func (m *Mod) noteFriendlyGrid(eb *embodiedBot) {
	if eb == nil || !eb.patrol || eb.factionID == 0 || eb.ent == nil || eb.ss == nil {
		return
	}
	if m.npc == nil {
		return
	}
	held := 0
	for _, e := range eb.ss.GetBubbleNPCs(eb.ent.ID) {
		if m.npc.IsNPC(e.ID) && !npc.IsInertStructure(e.Slim.GroupID) {
			held++
		}
	}
	if held == 0 {
		return
	}
	now := time.Now()
	eb.mu.Lock()
	last := eb.lastFriendlyGridAt
	if !last.IsZero() && now.Sub(last) < friendlyGridLogEvery {
		eb.mu.Unlock()
		return
	}
	eb.lastFriendlyGridAt = now
	eb.mu.Unlock()
	m.logBot(eb.charID, logInfo, "combat",
		"%d rat(s) on this grid, all of them flying for faction %d — the same people paying this pilot; holding fire",
		held, eb.factionID)
}

// embodyCombatLoop is the projection's whole combat life: watch the grid, and when something
// is hostile keep a target, keep it in range, keep the guns cycling, and know when to leave.
// The loadout is resolved on the first shot fired at the bot, not at materialisation.
func (m *Mod) embodyCombatLoop(ctx context.Context, eb *embodiedBot) {
	// goSafeBot guards the goroutine; this inner recover names the pilot and carries the stack.
	defer func() {
		if r := recover(); r != nil {
			rlogf("rolebots: PANIC in combat loop for %s: %v\n%s", eb.charName, r, debug.Stack())
		}
	}()

	var loadout *combatLoadout
	var lockedAt time.Time
	var locked uint32
	// lockedEnt is the locked target's entity, held so a kill nobody's guns reported (the
	// drones') is still recognised once the ball is gone.
	var lockedEnt *destiny.Entity
	engaged := false
	// policeNoted keeps the "leaving the police" line to one per engagement.
	policeNoted := false
	// notes tells each locked pilot's client it is locked, so the bot shows as
	// attacking and not only its drones (locknotes.go).
	notes := lockNotes{}

	standDown := func(reason string) {
		if !engaged {
			return
		}
		engaged, locked, lockedEnt = false, 0, nil
		policeNoted = false
		m.disengageAll(eb, loadout)
		notes.clear(eb)
		m.dronesReturn(eb, grpCombatDrone)
		eb.mu.Lock()
		eb.fighting, eb.combatTarget = false, 0
		eb.fightSince = time.Time{}
		eb.mu.Unlock()
		eb.clearNav()
		eb.sling = slingState{}
		m.clearFlee(eb)
		// The commander's calls end with its engagement (fleetcommand_grid.go).
		m.fcClearCall(eb)
		m.logBot(eb.charID, logInfo, "combat", "standing down — %s", reason)
	}
	defer func() {
		// Death, de-embodiment, system teardown: whatever ends the projection
		// must not leave a rat webbed by a ship that no longer exists.
		m.disengageAll(eb, loadout)
		notes.clear(eb)
	}()

	for {
		if ctx.Err() != nil {
			return
		}
		if eb.ent == nil || eb.ss == nil || eb.ent.IsDestroyed() {
			return
		}

		sighted := m.hostilesOnGrid(eb)
		// Only the aggro half drives the engagement: counting sightings would leave every miner in
		// a ratted belt in combat forever.
		hostiles := aggroIDs(sighted)
		if len(hostiles) == 0 {
			wasEngaged := engaged
			standDown("grid is clear")
			// The embodied half of the belt-distress settlement: the abstract path never runs in a
			// watched system. Kills plus an empty grid guard against settling a fight the rats warped out of.
			if gridClearSettles(wasEngaged, eb.npcKills.Load(), len(sighted)) {
				eb.npcKills.Store(0)
				// The gang's miners may come back to a belt its guns have just cleared (beltbail.go).
				if eb.patrol {
					noteGangBeltCleared(eb.gangID, beltUnderfoot(eb))
				}
				sysID := eb.systemID
				m.goSafeBot(eb.charID, "belt distress settlement", func() {
					notePatrolCover(ebScope(eb), sysID)
					dbCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if m.settleBeltDistress(dbCtx, eb.corpID, sysID) {
						m.logBot(eb.charID, logInfo, "combat",
							"distress answered — the belts here are open to miners again")
					}
				})
			}
			// The gank scout's work is done on the quiet ticks: nothing shoots a scout that shoots
			// nobody (gank_embodied.go).
			if eb.gankScout {
				m.gankScoutTick(ctx, eb)
			}
			// A checkpoint waves the pilots it will not shoot through, in
			// Local, once each (camp.go).
			if eb.checkpoint {
				m.checkpointTick(ctx, eb)
			}
			// A patrol idle on a belt that has rats on it is the visible face of the faction filter;
			// say so, or it reads as a broken bot.
			m.noteFriendlyGrid(eb)
			// A logi whose fleetmate is under fire is not itself under fire, so the repairers also cycle
			// on the guard tick (support_embodied.go).
			loadout = m.supportLoadoutIdle(eb, loadout)
			m.runRepCycles(eb, loadout)
			// And an idle logi flies in to a friend under fire just out of
			// its repairers' reach (combat_assist.go).
			m.assistHealApproach(eb, loadout)
			// A ship that left a grid unprovoked and has landed is released here (beltbail.go).
			m.settlePreemptiveFlee(eb)
			// Nothing is shooting: look at what is merely standing there, and leave if this belt is not
			// survivable. mine_embodied.go has no NPC awareness of its own.
			m.assessSightedGrid(ctx, eb, loadout, sighted)
			// And past the grid: a corporation at war is in danger wherever
			// its enemies are in the system, highsec included.
			m.assessWarInSystem(ctx, eb, loadout)
			if !embodySleep(ctx, combatGuardTick) {
				return
			}
			continue
		}

		// No first contact while the ship is still in a warp: its movement commands would take the
		// manager out of navWarp early. See docs/comment-notes-b02.md#warp-first-contact.
		if !engaged {
			eb.mu.Lock()
			inWarp := eb.warping
			eb.mu.Unlock()
			if inWarp {
				if !embodySleep(ctx, combatGuardTick) {
					return
				}
				continue
			}
		}

		// First contact. Resolve what this hull can do about it, take the
		// lasers off the rock, and commit.
		if !engaged {
			if loadout == nil {
				dbCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				shipTypeID := int64(0)
				if eb.ent != nil {
					shipTypeID = eb.ent.Slim.TypeID
				}
				l, err := m.loadCombatLoadout(dbCtx, eb.charID, eb.shipID, shipTypeID)
				cancel()
				if err != nil {
					rlogf("rolebots: combat loadout %s: %v", eb.charName, err)
					if !embodySleep(ctx, combatGuardTick) {
						return
					}
					continue
				}
				loadout = l
			}
			// A friend's fight and no guns to bring: stay out rather than run, or the unarmed flight
			// below would empty a belt whenever a rat shot the miner next door. A repairer still works.
			if !loadout.armed() && electiveOnly(sighted) {
				m.runRepCycles(eb, loadout)
				m.assistHealApproach(eb, loadout)
				if !embodySleep(ctx, combatGuardTick) {
					return
				}
				continue
			}
			// A fight it would be choosing, and its own nerve would pull it out on the first tick (a hull
			// still carrying the last fight's damage): it does not open fire, lock or call out at all.
			// Aggressing and warping off in the same second is what a watching player would notice.
			// A gank strike and a fleet keep their own calls (fcGridRetreat has side effects).
			if !eb.gankStrike && !eb.isFleetCommander() && fleetLeaderLive(eb) == nil && chosenFight(sighted) {
				margin, known := m.assessGrid(eb, loadout, sighted)
				if arm := m.breakOffReason(eb, loadout, margin, known); arm != "" {
					eb.mu.Lock()
					note := time.Since(eb.stayOutLoggedAt) >= stayOutLogEvery
					if note {
						eb.stayOutLoggedAt = time.Now()
					}
					eb.mu.Unlock()
					if note {
						m.logBot(eb.charID, logInfo, "combat",
							"staying out of it at %.0f%% hull, %.0f%% tank (%s)",
							eb.hullFraction()*100, eb.tankFraction()*100, arm)
					}
					if !embodySleep(ctx, combatGuardTick) {
						return
					}
					continue
				}
			}
			engaged = true
			// A fresh engagement starts its rat tally at zero, so kills left from an earlier fight do
			// not settle a later clear.
			eb.npcKills.Store(0)
			// Somebody opened fire without a word (chatter_exchange.go): the pilot says something and
			// its corporation hears. Only the first pilot in the hit log, one shout per engagement.
			for _, id := range eb.underAttack() {
				if e := eb.ss.GetEntity(id); e != nil && e.IsPlayer && e.Slim.CharID != 0 {
					m.chatterUnderFire(eb, e)
					break
				}
			}
			// Resolve the grid's rat types into the threat cache while the loop already pays for
			// database work; the per-tick target choice reads it as a map lookup (threat.go).
			{
				var gridTypes []int64
				for _, id := range hostiles {
					if e := eb.ss.GetEntity(id); e != nil {
						gridTypes = append(gridTypes, e.Slim.TypeID)
					}
				}
				dbCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				m.warmRatThreats(dbCtx, gridTypes)
				cancel()
			}
			eb.mu.Lock()
			eb.fighting = true
			eb.fightSince = time.Now()
			// Published for the piracy paths, which have to be able to drop
			// a point on a paying victim from outside this loop.
			eb.load = loadout
			eb.mu.Unlock()
			m.stopEmbodyMining(eb)
			eb.setMineTarget(0)
			eb.clearNav()
			// A working bot that got aggroed remembers the system; a patrol chose the fight and holds no grudge.
			if !eb.patrol {
				noteHostileEncounter(ebScope(eb), eb.systemID)
			}
			if loadout.armed() {
				m.logBot(eb.charID, logInfo, "combat",
					"engaged — %d weapon(s), %d EWAR module(s), %d hostile(s) on grid",
					len(loadout.weapons), len(loadout.ewar), len(hostiles))
				if assistOnly(sighted) {
					if pairs := m.assistPairs(eb); len(pairs) > 0 {
						name := "a hostile"
						if a := eb.ss.GetEntity(pairs[0].attacker); a != nil && a.CharName != "" {
							name = a.CharName
						}
						m.logBot(eb.charID, logInfo, "combat",
							"came to the aid of %s against %s", pairs[0].victim, name)
					}
				}
				for _, h := range sighted {
					if h.Aggro && h.Elective && !h.Assist {
						name := "a war target"
						if a := eb.ss.GetEntity(h.ID); a != nil && a.CharName != "" {
							name = a.CharName
						}
						m.logBot(eb.charID, logInfo, "combat", "engaged a war target in transit: %s", name)
						break
					}
				}
			} else {
				m.logBot(eb.charID, logWarn, "combat",
					"taking fire with nothing to shoot back with — looking for a way out")
			}
		}

		// Whether to leave is decided every tick; where to, and the warp itself, once (fleeGrid). The engagement
		// stays up for the whole break-off. The margin weighs the whole grid; unknown falls back to the damage bars.
		margin, marginKnown := m.assessGrid(eb, loadout, sighted)
		// A gank striker never breaks off mid-volley: warping out half-way throws the hull away. Once
		// the mark is down or CONCORD has it, it runs or brawls (gankStrikerRuns).
		gankRun := eb.gankStrike && m.gankStrikerRuns(ctx, eb)
		// The law on this ship: only a reckless, violent outlaw stands and shoots it out (Tuning.FightsPolice).
		policeRun := !eb.gankStrike && !eb.tune.FightsPolice && policeEngaged(sighted)
		if policeRun && !policeNoted {
			policeNoted = true
			m.logBot(eb.charID, logWarn, "combat", "the police are on me — leaving")
		}
		var arm breakArm
		switch {
		case gankRun:
			arm = breakGankRun
		case policeRun:
			arm = breakPolice
		case !eb.gankStrike:
			arm = m.breakOffReason(eb, loadout, margin, marginKnown)
		}
		if arm != "" {
			// Read by fleeGrid's log line, so a break-off says which rule called it.
			eb.mu.Lock()
			eb.breakReason = arm
			eb.mu.Unlock()
			// warpMandatory: this ship is being shot at, and one refused
			// break-off warp is a bot that stands there and dies.
			//
			// fleeGrid reports whether the ship is actually leaving. One that cannot (warp-scrambled,
			// nowhere to run) is cornered, and a cornered ship fights: falling through to the gun
			// loop below, not sitting out the break-off with its guns cold while it is shot to pieces.
			if m.fleeGrid(ctx, eb, loadout, warpMandatory) {
				// The escape rack, re-cycled every tick of the break-off, after fleeGrid (which decides the
				// ship is leaving). A jam lapses on its own cycle and must be renewed.
				m.runFlightEwar(eb, loadout, hostiles)
				if !embodySleep(ctx, combatGuardTick) {
					return
				}
				continue
			}
		}

		// The locked rat died without the guns reporting it: the drones got
		// it (drones_embodied.go), and the tally has to hear about it.
		if lockedEnt != nil && lockedEnt.ID == locked && lockedEnt.IsDestroyed() {
			m.droneKilledTarget(eb, lockedEnt)
			locked, lockedEnt = 0, nil
		}

		target := m.pickCombatTarget(eb, loadout, hostiles)
		// markFixed: the target below is a held mark (a robbery, a gank call,
		// a toll), and every point stays on it.
		markFixed := false
		// A pirate chooses the fattest, slowest thing in reach, and only if the CONCORD clock says the robbery can be
		// finished (piracy_prey.go). Anything that shot the bot stays the ordinary scorer's business.
		if eb.prey {
			if t := m.piracyTarget(eb, loadout, hostiles); t != 0 {
				target, markFixed = t, true
			}
		}
		// A gank striker shoots the called mark and nothing else, with no strike-window refusal: the scout ran the
		// arithmetic (gank_embodied.go). Anything shooting the striker still wins the ordinary scorer above.
		if eb.gankStrike && len(eb.underAttack()) == 0 {
			if t := m.gankStrikeTarget(eb); t != 0 {
				target, markFixed = t, true
			}
		}
		// A Mercenary checkpoint with nothing to shoot holds a neutral for a toll (camp.go): pointed,
		// asked, released paid or not, guns never open (tollEngage holds fire).
		if eb.checkpoint && target == 0 && len(eb.underAttack()) == 0 {
			target = m.tollTarget(eb)
			markFixed = target != 0
		}
		// Points spread across the grid (pointclaims.go). A tackler flies to its own pick, which
		// becomes its gun target; any other point spreads inside scram range in runEwarCycle.
		loadout.pointMode = pointModeFor(eb, markFixed)
		if loadout.pointMode == pointSpread && target != 0 && tackleSeat(eb, loadout) {
			target = m.pointTargetFor(eb, loadout, target, 0, hostiles)
			loadout.pointMode = pointFollow
		}
		if target == 0 {
			notes.sync(eb, lockedIDs(loadout, 0)...)
			if !embodySleep(ctx, combatTick) {
				return
			}
			continue
		}
		if target != locked {
			locked, lockedAt = target, time.Now()
			// A ball the commander called as the secondary has been on the lock since the call
			// (fleetcommand_grid.go): the switch pays only what is left of the lock delay.
			if at, ok := fleetPreLockAt(eb, target); ok && at.Before(lockedAt) {
				lockedAt = at
			}
			eb.mu.Lock()
			eb.combatTarget = target
			eb.mu.Unlock()
			m.stopAllFire(eb, loadout)
			// A new pilot on the lock is the moment to say why, in Local (chatter_exchange.go): map
			// reads and a goroutine, the line is said off this loop.
			if te := eb.ss.GetEntity(target); te != nil && te.IsPlayer && te.Slim.CharID != 0 {
				m.chatterEngageEmbodied(eb, te, eb.underAttack())
			}
		}

		tgt := eb.ss.GetEntity(target)
		if tgt == nil {
			locked, lockedEnt = 0, nil
			continue
		}
		lockedEnt = tgt

		// Hold the range the guns want as a player would: orbit, keep at range or hold at optimal,
		// off the fit (combatdoctrine.go). Not once a trip's burn-in has outlasted the fight hold:
		// then travel flies the ship and the guns fire from wherever it is (combatHoldsHelm).
		eb.mu.Lock()
		helm := combatHoldsHelm(eb.approachID != 0, eb.fighting, eb.fightSince, time.Now())
		eb.mu.Unlock()
		if helm {
			m.holdCombatRange(eb, loadout, tgt)
		}

		// Lock delay: a ship cannot shoot what it has not finished locking.
		// A switched-away pilot hears otherlost here; the new one waits for the lock.
		if wait := m.lockSeconds(loadout, tgt); time.Since(lockedAt).Seconds() < wait {
			notes.sync(eb, lockedIDs(loadout, 0)...)
			if !embodySleep(ctx, combatTick) {
				return
			}
			continue
		}
		// Locked: the pilot hears otheradd before the first shot's FX.
		notes.sync(eb, lockedIDs(loadout, tgt.ID)...)

		// EWAR first, always: that order makes the point land before the guns, and for a pirate it is
		// the whole engagement. Each module picks its own target off the grid, so the hostile list travels.
		m.runEwarCycle(eb, loadout, tgt, hostiles)
		// The EWAR cycle may have put a module on a second pilot.
		notes.sync(eb, lockedIDs(loadout, tgt.ID)...)
		// The repairers, own and remote, every tick: a logi under fire is still a logi.
		m.runRepCycles(eb, loadout)

		// Ordered to hold fire (conscript_embodied.go). After the ewar cycle: hold fire means hold the
		// target down, not kill it, so a tackler must keep its point. The tackle order shares this branch.
		if eb.holdingFire() || eb.orderedTackleOnly() {
			// Held fire holds the drones too.
			m.dronesReturn(eb, grpCombatDrone)
			if !embodySleep(ctx, combatTick) {
				return
			}
			continue
		}

		// Terms: while a demand is live the guns stay cold (point on, a price in Local, the victim
		// has a minute). A pirate that shot while it waited would be asking for money it can no longer take.
		if eb.prey && m.piracyEngage(eb, loadout, tgt) {
			if !embodySleep(ctx, combatTick) {
				return
			}
			continue
		}
		// A checkpoint's toll is the same shape: point on, price in Local, guns cold whether or not
		// it is paid (camp.go).
		if eb.checkpoint && m.tollEngage(eb, loadout, tgt) {
			if !embodySleep(ctx, combatTick) {
				return
			}
			continue
		}

		// Drones onto the gun target, a pilot's included (drones_embodied.go). Against a pilot the
		// flight is staged home once the armour is gone, ahead of the hull-line break-off.
		if tgt.IsPlayer && eb.armorFraction() < droneStageArmorFrac {
			m.stageDrones(eb)
		}
		m.droneEngage(eb, loadout, tgt)

		if dead := m.runWeaponCycles(eb, loadout, tgt); dead {
			locked, lockedEnt = 0, nil
			eb.mu.Lock()
			eb.combatTarget = 0
			eb.mu.Unlock()
		}

		if !embodySleep(ctx, combatTick) {
			return
		}
	}
}

// shouldBreakOff decides whether this fight is still worth having. The damage-bar thresholds
// are the pilot's own nerve (Tuning.Flee*), mirrored by the abstract tier (abstractBreakOff).
// The margin arm exists because a bar check does not know what it is fighting: below the
// refusal floor the arithmetic says the ship dies, so the first tick is the cheapest moment to act.
func (m *Mod) shouldBreakOff(eb *embodiedBot, l *combatLoadout, margin float64, marginKnown bool) bool {
	return m.breakOffReason(eb, l, margin, marginKnown) != ""
}

// breakArm names the rule that called a break-off, for the log; "" is no break-off.
type breakArm string

const (
	breakFleetCalled   breakArm = "fleet called retreat"
	breakCommander     breakArm = "commander's read"
	breakFleetHullLine breakArm = "fleet hull floor"
	breakHopeless      breakArm = "hopeless margin"
	breakWorkerTank    breakArm = "worker tank line"
	breakUnarmedTank   breakArm = "unarmed tank line"
	breakArmedHull     breakArm = "armed hull line"
	breakGankRun       breakArm = "gank run"
	breakPolice        breakArm = "police"
)

// breakInputs is everything breakOffArm weighs, read off the pilot once.
type breakInputs struct {
	fleetCalled   bool // the commander called the fleet off
	underLeader   bool // a live commander that has not called it
	hopeless      bool // the threat guard reads the margin as hopeless
	fightsPosture bool // the pilot came out to fight (postureFights)
	armed         bool
	hull, tank    float64
	fleeHullArmed float64 // Tuning.FleeHullFracArmed
	fleeTankUnarm float64 // Tuning.FleeTankFracUnarmed
}

// breakOffArm is the pure rule behind shouldBreakOff, short of the commander's own read
// (fcGridRetreat), which breakOffReason asks first.
func breakOffArm(in breakInputs) breakArm {
	// The fleet's call outranks the pilot's own nerve (fleetcommand_grid.go): a member leaves when its commander
	// calls the fleet off and stays while it holds, and the commander judges the fleet's state. No command, no call.
	if in.fleetCalled {
		return breakFleetCalled
	}
	if in.underLeader {
		// Under a live commander that has not called it: stay, unless the hull itself is about to go,
		// the one call no commander may make for a member.
		if in.hull < in.fleeHullArmed*0.5 {
			return breakFleetHullLine
		}
		return ""
	}
	if in.hopeless {
		return breakHopeless
	}
	// What the pilot came out to do comes ahead of what it carries (combatdoctrine.go): a mining
	// frigate with a popgun reads as armed. A worker keeps working until something lands.
	if !in.fightsPosture {
		if in.tank < in.fleeTankUnarm {
			return breakWorkerTank
		}
		return ""
	}
	if !in.armed {
		// Nothing to shoot with. The only question is whether it is taking damage yet: a bot that
		// has merely been locked keeps working.
		if in.tank < in.fleeTankUnarm {
			return breakUnarmedTank
		}
		return ""
	}
	if in.hull < in.fleeHullArmed {
		return breakArmedHull
	}
	return ""
}

// breakOffReason decides whether this fight is still worth having and names the rule that
// says not; "" means stay.
func (m *Mod) breakOffReason(eb *embodiedBot, l *combatLoadout, margin float64, marginKnown bool) breakArm {
	called, _ := fleetRetreatCalled(eb)
	if !called && eb.isFleetCommander() {
		if m.fcGridRetreat(eb, l, m.gridHostileIDs(eb)) {
			return breakCommander
		}
		return ""
	}
	return breakOffArm(breakInputs{
		fleetCalled:   called,
		underLeader:   fleetLeaderLive(eb) != nil,
		hopeless:      marginKnown && threatGuardEnabled() && hopeless(margin),
		fightsPosture: postureFights(eb.posture),
		armed:         l.armed(),
		hull:          eb.hullFraction(),
		tank:          eb.tankFraction(),
		fleeHullArmed: eb.tune.FleeHullFracArmed,
		fleeTankUnarm: eb.tune.FleeTankFracUnarmed,
	})
}

// pickCombatTarget takes the hostile the bot should be shooting: the one
// closest to dying that is inside the reach of its guns, else simply the
// nearest. Finishing a hurt rat removes its damage from the field faster than
// spreading fire does.
func (m *Mod) pickCombatTarget(eb *embodiedBot, l *combatLoadout, hostiles []uint32) uint32 {
	if eb.ent == nil || eb.ss == nil {
		return 0
	}
	// A commander's primary overrides the scorer (conscript_embodied.go): the human picked the target. Zero when
	// there is no command or the ball is gone, and then the scoring below takes over.
	if id := m.commandedTarget(eb, hostiles); id != 0 {
		return id
	}
	// "On call" (fctags.go): nothing called, nothing shot.
	if eb.onCallOnly() {
		return 0
	}
	// A fleet commander under a command calls the primary for the fleet (fleetcommand_grid.go): the fastest kill
	// in its reach, or the one a Director aboard named. Its own scorer is for a commander with nothing in reach.
	if id := m.fcGridPrimary(eb, l, hostiles); id != 0 {
		return id
	}
	// A bot fleet's commander calls a primary the same way (gangprimary.go): the leader's target if this member
	// counts it hostile and its guns reach it. Otherwise the scorer below picks the secondary.
	if id := m.gangPrimary(eb, l, hostiles); id != 0 {
		return id
	}
	// A member under a Director's hold fires on the called primary and nothing else: a camp that
	// opened on a neutral would be revealed.
	if fleetHoldCalled(eb) {
		return 0
	}
	selfPos := eb.ent.Pos()
	reach := l.targetRnge
	if eb.ent != nil {
		reach *= eb.ent.EwarTotals().MaxTargetRangeMult
	}

	var best uint32
	bestScore := math.MaxFloat64
	var fallback uint32
	fallbackDist := math.MaxFloat64
	// What each pilot is doing to this ship, built on the first pilot candidate (pilotDPSOn).
	var pilotDPS map[int64]float64

	for _, id := range hostiles {
		e := eb.ss.GetEntity(id)
		if e == nil || e.IsDestroyed() || e.IsInWarp() || e.IsCloaked() {
			continue
		}
		// The law is something to leave, not to shoot, unless this pilot is a reckless, violent outlaw
		// (Tuning.FightsPolice); the police break-off is already flying anyone else out.
		if !e.IsPlayer && lawEnforcementGroup(e.Slim.GroupID) && !eb.tune.FightsPolice {
			continue
		}
		dist := e.Pos().Sub(selfPos).Len()
		if dist < fallbackDist {
			fallback, fallbackDist = id, dist
		}
		if dist > reach {
			continue
		}
		hp := e.HPSnapshot()
		total := hp.MaxShield + hp.MaxArmor + hp.MaxStructure
		frac := 1.0
		if total > 0 {
			frac = (hp.CurShield + hp.CurArmor + hp.CurStructure) / total
		}
		remainingEHP := 0.0
		targetDPS := 0.0
		if rt, ok := ratThreatCached(e.Slim.TypeID); ok {
			remainingEHP, targetDPS = rt.EHP*frac, rt.DPS
		} else if e.IsPlayer && e.Slim.CharID != 0 {
			// A pilot has no threat-cache row: its health is what is left on the bars, and its
			// output is what it has actually done to this ship lately. One that is not shooting
			// scores 0 and keeps the health-then-distance order.
			if pilotDPS == nil {
				pilotDPS = pilotDPSOn(eb.ent.KillHitsSnapshot(), time.Now().Unix())
			}
			remainingEHP = hp.CurShield + hp.CurArmor + hp.CurStructure
			targetDPS = pilotDPS[e.Slim.CharID]
		}
		score := combatTargetScore(l.dps(), frac, dist, remainingEHP, targetDPS)
		if score < bestScore {
			best, bestScore = id, score
		}
	}
	if best != 0 {
		return best
	}
	return fallback
}

// pilotDPSOn is each pilot's recent damage to this ship per second, from its hit ledger: damage
// over the attacker-memory window, for pilots whose last hit is inside it. NPC hits are skipped,
// as the threat cache prices those. The ledger's damage is the attacker's running total, so a long
// fight overstates the rate; it is a preference between targets, not a measurement.
func pilotDPSOn(hits []destiny.KillHit, now int64) map[int64]float64 {
	out := map[int64]float64{}
	cutoff := now - combatAggroMemorySec
	for _, h := range hits {
		if h.IsNPC || h.CharID == 0 || h.Time < cutoff {
			continue
		}
		out[h.CharID] += h.Damage / combatAggroMemorySec
	}
	return out
}

// holdCombatRange puts the ship where its guns work, and how it holds that place is the
// stance (combatdoctrine.go). Keep-at-range is FollowBall, the client's own Keep at Range,
// sent by embodyApproach (nav_embodied.go).
func (m *Mod) holdCombatRange(eb *embodiedBot, l *combatLoadout, target *destiny.Entity) {
	if len(l.weapons) == 0 || target == nil {
		return
	}
	// A commander's align, orbit or range order owns the ship's motion (fcmove_embodied.go).
	if eb.steersShip() {
		return
	}
	// A tackler burns to the target at best speed and holds inside its web and point, whatever
	// its guns prefer (camp.go tackleBurn): the target must still be there when the line arrives.
	if eb.gangRole == RoleTackler && m.tackleBurn(eb, l, target) {
		return
	}
	// A line member fights from the anchor (fleetcommand_grid.go): it orbits the anchor's ball and shoots the primary,
	// lighting its engine only when the primary is out of reach. The anchor and tackle and EWAR seats fly the stance.
	if anchor, ok := m.fleetAnchorBall(eb); ok && eb.ent != nil {
		gap := target.Pos().Sub(eb.ent.Pos()).Len()
		m.runPropCycle(eb, l, gap > loadoutReachM(l)+propChaseMarginM)
		m.embodyOrbit(eb, anchor, fleetAnchorOrbitM, 0)
		return
	}
	shortest := math.MaxFloat64
	allMissile := true
	for _, w := range l.weapons {
		if !w.IsMissile {
			allMissile = false
			if w.Optimal > 0 && w.Optimal < shortest {
				shortest = w.Optimal
			}
		}
		if w.MaxRange < shortest && w.IsMissile {
			shortest = w.MaxRange
		}
	}
	if shortest == math.MaxFloat64 {
		shortest = 10_000
	}
	// A missile boat has no optimal, the flight envelope is the whole of it, so the two numbers
	// stanceFor and holdRangeFor want are the same.
	optimal := shortest
	if allMissile {
		optimal = 0
	}

	reach := loadoutReachM(l)
	// The speed the ship could fly with its engine lit (combatSpeed), so a hull whose engine is
	// off can still decide to kite, and the decision lights it.
	st := doctrineStance(reach, combatSpeed(eb, l), targetTopSpeed(target))
	dist := holdRangeFor(st, reach, optimal)
	gap := target.Pos().Sub(eb.ent.Pos()).Len()
	m.runPropCycle(eb, l, st == stanceKite || gap > dist+propChaseMarginM)
	if allMissile && st == stanceBrawl {
		// Two thirds of flight range: inside the envelope with room to keep it when the target burns
		// away. holdRangeFor's brawler arm would give orbitRangeFor(0) instead.
		dist = shortest * 0.66
	}

	// A pilot held outside its reach by a faster ship breaks the kite (slingshot.go). The kiter
	// itself never needs it.
	if st != stanceKite && m.slingshot(eb, l, target, time.Now()) {
		return
	}

	// A kiter holds a range and does not circle inside one. FollowBall is the client's Keep at
	// Range and the server's Manager.Follow is the same intent.
	if st == stanceKite {
		radius := 0.0
		if target.Manager != nil {
			radius = target.Manager.Radius()
		}
		m.embodyApproach(eb, target.ID, dist, radius)
		return
	}
	// Everything else circles. The stance only changes the radius for a holder: orbitRangeFor
	// would pull a 27 km gun to a 15 km circle.
	m.embodyOrbit(eb, target.ID, dist, 0)
}

// hullSpeed is what this projection's ball can actually do, for the stance. It reads the
// entity, not the database (the combat loop must not query), and the number is already
// reduced by a web, so a webbed hull stops believing it can kite.
func (eb *embodiedBot) hullSpeed() float64 {
	if eb == nil || eb.ent == nil || eb.ent.Manager == nil {
		return 0
	}
	return eb.ent.Manager.EffectiveMaxVelocity()
}

// targetTopSpeed is what the hull has to beat to hold a range against this target. Zero when
// unreadable, which stanceFor resolves to the slow answer.
func targetTopSpeed(target *destiny.Entity) float64 {
	if target == nil || target.Manager == nil {
		return 0
	}
	// EffectiveMaxVelocity, not the base attribute, so a rat this bot has webbed reads as the
	// slow thing it now is.
	return target.Manager.EffectiveMaxVelocity()
}

// lockSeconds is EVE's lock time for this ship against this target:
// 40000 / (scanRes · asinh(sig)²) seconds.
func (m *Mod) lockSeconds(l *combatLoadout, target *destiny.Entity) float64 {
	sig := combatDefaultTargetSig
	if s := m.targetSigRadius(target); s > 0 {
		sig = s
	}
	return lockSecondsAt(l.scanRes, sig)
}

// lockSecondsAt is that formula over bare numbers, so the skill planner can ask what a level
// of Signature Analysis is worth without a ship in space (skillsupport.go). The clamps are the
// fight loop's own: nothing locks instantly, and a terrible sheet still eventually shoots.
func lockSecondsAt(scanRes, sig float64) float64 {
	if scanRes <= 0 {
		scanRes = 100
	}
	as := math.Asinh(sig)
	if as <= 0 {
		return combatMaxLockSec
	}
	t := 40000.0 / (scanRes * as * as)
	if t < combatMinLockSec {
		return combatMinLockSec
	}
	if t > combatMaxLockSec {
		return combatMaxLockSec
	}
	return t
}

// targetSigRadius reads a target's signature radius, cached per type for the life of the process.
func (m *Mod) targetSigRadius(target *destiny.Entity) float64 {
	if target == nil {
		return combatDefaultTargetSig
	}
	typeID := target.Slim.TypeID
	sigCacheMu.Lock()
	if v, ok := sigCache[typeID]; ok {
		sigCacheMu.Unlock()
		return v * target.EwarTotals().SigRadiusMult
	}
	sigCacheMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sig := combatDefaultTargetSig
	if attrs, err := evedb.GetTypeAttributes(ctx, m.db, typeID); err == nil {
		if s := attrs[dogma.AttrSignatureRadius]; s > 0 {
			sig = s
		}
	}
	sigCacheMu.Lock()
	sigCache[typeID] = sig
	sigCacheMu.Unlock()
	return sig * target.EwarTotals().SigRadiusMult
}

// ── firing ──────────────────────────────────────────────────────────────────

// runWeaponCycles fires every weapon whose cycle has come round. Reports
// whether the target died on one of them.
func (m *Mod) runWeaponCycles(eb *embodiedBot, l *combatLoadout, target *destiny.Entity) bool {
	now := time.Now()
	selfPos := eb.ent.Pos()
	targetPos := target.Pos()
	dist := targetPos.Sub(selfPos).Len()

	// A target in warp, cloaked or mid-landing is untouchable, the same as it
	// is for an NPC or a player.
	if target.IsInWarp() || target.IsWarpPending() || target.IsUntargetable() {
		m.stopAllFire(eb, l)
		return false
	}

	for _, w := range l.weapons {
		if now.Before(w.nextFireAt) {
			continue
		}
		if dist > w.MaxRange {
			m.stopWeaponFire(eb, w)
			continue
		}
		if !l.drawCap(w.CapNeed) {
			m.stopWeaponFire(eb, w)
			continue
		}
		if !m.consumeCharge(eb, w) {
			m.stopWeaponFire(eb, w)
			// The load ran out: reload from the hold and rest the gun a reload's length.
			if m.reloadGunFromHold(eb, w) {
				w.nextFireAt = now.Add(gunReloadDelay)
				continue
			}
			// Out of ammo, hold included. The gun goes quiet for the rest of the fight; the
			// procurement pass restocks it when the bot next docks.
			w.nextFireAt = now.Add(time.Hour)
			m.logBot(eb.charID, logWarn, "combat", "%s is out of ammunition", w.Name)
			continue
		}

		w.nextFireAt = now.Add(time.Duration(w.CycleSec * float64(time.Second)))
		m.startWeaponFire(eb, w, target.ID)

		if w.IsMissile {
			// The missile has to fly there, and damage lands when it arrives, so a target that dies or
			// warps meanwhile is missed. The core puts the visible ball in flight and says how long it takes.
			targetID := target.ID
			ballFlight := 0.0
			if launch := modapi.PvP().LaunchMissile; launch != nil && eb.ent != nil && eb.ss != nil {
				flight, fizzle := launch(eb.ss, eb.ent, target, w.ChargeTypeID)
				if fizzle {
					continue
				}
				ballFlight = flight
			}
			m.goSafeBot(eb.charID, "missile impact", func() {
				m.missileImpact(eb, l, w, targetID, dist, ballFlight)
			})
			continue
		}
		if destroyed := m.turretShot(eb, l, w, target, selfPos, targetPos, dist, now); destroyed {
			return true
		}
	}
	return false
}

// turretShot resolves one turret cycle: tracking, falloff, the roll, damage. It is the
// client's GetAccuracy contract, the same one the player and NPC loops implement.
func (m *Mod) turretShot(eb *embodiedBot, l *combatLoadout, w *botWeapon, target *destiny.Entity,
	selfPos, targetPos destiny.Vec3, dist float64, now time.Time) bool {

	selfEw := eb.ent.EwarTotals()
	effOptimal := w.Optimal * selfEw.TurretRangeMult
	effTracking := w.Tracking * selfEw.TurretTrackingMult
	targetSig := m.targetSigRadius(target)

	exponent := 0.0
	if effOptimal > 0 && dist > effOptimal && w.Falloff > 0 {
		f := (dist - effOptimal) / w.Falloff
		exponent += f * f
	}
	if effTracking > 0 {
		if !w.lastSampleAt.IsZero() {
			dt := now.Sub(w.lastSampleAt).Seconds()
			if dist > 0 && dt > 0 {
				// Relative velocity, so an orbit that matches the target's own
				// motion is rewarded exactly as it is for a player.
				vx := (targetPos.X - w.lastTargetPos.X - (selfPos.X - w.lastSelfPos.X)) / dt
				vy := (targetPos.Y - w.lastTargetPos.Y - (selfPos.Y - w.lastSelfPos.Y)) / dt
				vz := (targetPos.Z - w.lastTargetPos.Z - (selfPos.Z - w.lastSelfPos.Z)) / dt
				rd := targetPos.Sub(selfPos)
				ux, uy, uz := rd.X/dist, rd.Y/dist, rd.Z/dist
				cx := uy*vz - uz*vy
				cy := uz*vx - ux*vz
				cz := ux*vy - uy*vx
				transverse := math.Sqrt(cx*cx + cy*cy + cz*cz)
				angular := transverse / dist
				sigTerm := effTracking * targetSig / w.WeaponSig
				if sigTerm > 0 {
					t := angular / sigTerm
					exponent += t * t
				}
			}
		}
		w.lastTargetPos, w.lastSelfPos, w.lastSampleAt = targetPos, selfPos, now
	}
	hitChance := math.Pow(0.5, exponent)

	// One roll decides miss, hit and wrecking, exactly as elsewhere: a shot
	// with a poor chance can still land, but only weakly.
	roll := rand.Float64()
	dmgMod := 0.0
	switch {
	case roll < 0.01*hitChance:
		dmgMod = 3.0
	case roll <= hitChance:
		dmgMod = roll + 0.49
	}
	if dmgMod == 0 {
		return false
	}

	em, exp, kin, therm := w.shotDamage()
	hit := func() bool {
		applied, destroyed := target.ApplyDamage(
			em*dmgMod*rates.PlayerDamage,
			exp*dmgMod*rates.PlayerDamage,
			kin*dmgMod*rates.PlayerDamage,
			therm*dmgMod*rates.PlayerDamage,
		)
		return m.landDamage(eb, target, applied, destroyed, w.TypeID)
	}
	if !target.IsPlayer {
		return hit()
	}
	// A pilot: not once the ship is docking or jumping (crimewatch.go).
	killed := false
	eb.actOnPilot(func() { killed = hit() })
	return killed
}

// missileImpact applies a missile's damage after its flight time. Missiles do
// not track; range is the only thing that reduces them, and a target that is
// gone when it arrives simply never gets hit.
func (m *Mod) missileImpact(eb *embodiedBot, l *combatLoadout, w *botWeapon, targetID uint32,
	launchDist, ballFlight float64) {

	// The core's own flight clock when a ball was drawn (runWeaponCycles);
	// the module's estimate only for a shot nothing drew.
	flight := ballFlight
	if flight <= 0 {
		flight = w.FlightSec
		if flight <= 0 || w.MaxRange <= 0 {
			flight = 1
		} else {
			// Time in flight scales with how far it actually had to go.
			flight *= math.Min(1, launchDist/w.MaxRange)
		}
	}
	time.Sleep(time.Duration(flight * float64(time.Second)))

	if eb.ss == nil || eb.ent == nil || eb.ent.IsDestroyed() {
		return
	}
	target := eb.ss.GetEntity(targetID)
	if target == nil || target.IsDestroyed() || target.IsInWarp() || target.IsCloaked() {
		return
	}
	dist := target.Pos().Sub(eb.ent.Pos()).Len()
	if dist > w.MaxRange {
		return
	}
	em, exp, kin, therm := w.shotDamage()
	hit := func() {
		applied, destroyed := target.ApplyDamage(
			em*rates.PlayerDamage,
			exp*rates.PlayerDamage,
			kin*rates.PlayerDamage,
			therm*rates.PlayerDamage,
		)
		m.landDamage(eb, target, applied, destroyed, w.TypeID)
	}
	if !target.IsPlayer {
		hit()
		return
	}
	// A missile still in flight when its shooter docks or jumps lands on
	// nobody (crimewatch.go).
	eb.actOnPilot(hit)
}

// gridClearSettles is the embodied settlement decision, pure for testing: an engagement that
// killed rats and left nothing hostile in sight (not merely nothing aggressing) cleared the
// spawn. Sightings still on grid mean unfinished rats; zero kills mean the hostiles left.
func gridClearSettles(engaged bool, npcKills int64, sightedHostiles int) bool {
	return engaged && npcKills > 0 && sightedHostiles == 0
}

// landDamage is everything that follows a hit: the threat table, the HP sync,
// the damage bar on every client watching, and the kill. Reports whether the
// target died.
func (m *Mod) landDamage(eb *embodiedBot, target *destiny.Entity, applied float64, destroyed bool, weaponTypeID int64) bool {
	if applied <= 0 && !destroyed {
		return false
	}
	// A player-owned structure, such as a sieged tower, goes through the core's structure rules:
	// saved damage, the reinforcement floor, the tower guns' memory and the structure's death.
	if eb.ss != nil && eb.ss.IsStructureEntity(target.ID) {
		if hit := modapi.PvP().StructureHit; hit != nil {
			hit(eb.ss, target, eb.ent, weaponTypeID, applied, destroyed)
		}
		m.broadcastDamageState(eb, target)
		return destroyed
	}
	// A conquerable station goes through the core's station rule: the damage state, and the capture
	// for the shooter's corporation once the shield is gone (stationsiege.go). No threat, no crime.
	if eb.ss != nil && eb.ss.IsStationEntity(target.ID) {
		if hit := modapi.PvP().StationHit; hit != nil {
			return hit(eb.ss, target, eb.corpID, eb.charID)
		}
		return false
	}

	if !target.IsPlayer && m.npc != nil {
		// Shooting a rat puts the bot at the top of its threat table, which is
		// what makes the rat fight the bot instead of wandering off.
		npcMgr, targetID, selfID := m.npc, target.ID, eb.ent.ID
		goSafe("npc threat notify", func() { npcMgr.NotifyDamage(targetID, selfID, applied) })
		m.npc.SyncHPToDB(target.ID)
	} else if target.IsPlayer {
		target.RecordKillHit(eb.ent, weaponTypeID, applied, destroyed)
		// Shooting a pilot is a crime recorded by the core's own crimewatch: the flag and the police run through the
		// functions a player's guns use (modapi.PvPHooks), so a rolebot lives under the law its victims do.
		if eb.ss != nil {
			if note := modapi.PvP().NotePlayerAggression; note != nil {
				note(eb.ent, target, eb.ss)
			}
			if police := modapi.PvP().PoliceCrime; police != nil {
				police(eb.ent, target, eb.ss)
			}
		}
	}

	m.broadcastDamageState(eb, target)

	if !destroyed {
		return false
	}
	m.logBot(eb.charID, logInfo, "combat", "destroyed target entity %d", target.ID)
	if !target.IsPlayer {
		// The engagement's rat tally, read at the grid-clear stand-down: a clear with kills behind it
		// is the embodied half of the belt-distress settlement.
		eb.npcKills.Add(1)
	}
	if !target.IsPlayer && m.npc != nil {
		// Route the kill through the manager so bounty, security status, standings and the spawner's
		// bookkeeping run with this bot credited as the killer.
		m.npc.Kill(target.ID, eb.ent.ID)
		m.siteOnBotKill(eb, target)
		// The core paid the one killer; a fleet's kill is everyone's, so the killer evens its gangmates out afterwards
		// (bountypay.go) on its own goroutine and context, so the money never holds up the combat loop.
		charID, sysID, typeID := eb.charID, eb.systemID, target.Slim.TypeID
		m.goSafeBot(charID, "bounty share", func() {
			bctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			m.shareEmbodiedBounty(bctx, charID, sysID, typeID)
		})
		// The core spawns the loot can off that death path a few seconds later, at the wreck. A
		// hunter goes back for it (beltloot.go); anything else ignores it.
		m.noteBeltLoot(eb, target)
		return true
	}
	if target.IsPlayer {
		// A destroyed player ship is not finished by zeroing its structure: the explosion, insurance, capsule, loot
		// container and killmail live on the victim's connection goroutine, reached only through the core (piracy_prey.go).
		m.piracyKilledPrey(eb, target)
		// And the killer has something to say about it (chatter_exchange.go).
		// A pod is not a kill worth a line.
		if target.Slim.CharID != 0 && target.Slim.GroupID != capsuleGroupID {
			killer := partyFromEmbodied(eb)
			var victim *chatParty
			if vb := liveProjection(target.Slim.CharID); vb != nil {
				victim = partyFromEmbodied(vb)
				victim.Alive = nil // the dead speak from the pod
			} else {
				victim = partyFromID(target.Slim.CharID, target.CharName, target.Slim.CorpID, target.Slim.TypeID, false)
			}
			victim.ShipTypeID = target.Slim.TypeID
			m.chatterKill(eb.systemID, killer, victim, lastMotive.get(eb.charID, target.Slim.CharID))
		}
	}
	return true
}

// broadcastDamageState pushes a target's new health to everyone on grid.
func (m *Mod) broadcastDamageState(eb *embodiedBot, target *destiny.Entity) {
	if eb.ss == nil {
		return
	}
	ds := target.DamageStateFrac()
	action := destiny.BuildSimpleAction("OnDamageStateChange",
		marshal.PyInt{Value: uint64(target.ID)},
		destiny.EncodeDamageState(ds),
	)
	for _, mate := range eb.ss.GetBubbleConnectedPlayersExcept(eb.ent.ID) {
		if mate.Conn == nil || !mate.KnowsBall(target.ID) {
			continue
		}
		_ = mate.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionListEvents(mate.Conn, m.nodeID, mate.AccountID, stamp, nil, action)
		})
	}
}

// consumeCharge draws one unit of ammunition. Energy turrets burn no ammo
// (their crystals do not deplete here), so a weapon with no loaded charge row
// is treated as always fed.
func (m *Mod) consumeCharge(eb *embodiedBot, w *botWeapon) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	lc, err := evedb.GetLoadedCharge(ctx, m.db, eb.shipID, w.SlotFlag)
	if err != nil {
		rlogf("rolebots: combat: charge lookup ship=%d slot=%d: %v", eb.shipID, w.SlotFlag, err)
		return true
	}
	if lc == nil || !m.chargeAccepts(ctx, lc.TypeID, w.TypeID) {
		// No round, or one this gun cannot fire (buildWeapon fought without
		// it): nothing is spent, and the gun is not silenced over it.
		return true
	}
	// A crystal is not spent by firing through it (durableChargeGroups): a laser holds exactly
	// one, so decrementing it like a round would delete it on the first shot.
	if groupID, _, err := evedb.GetGroupAndCategory(ctx, m.db, lc.TypeID); err == nil && durableChargeGroups[groupID] {
		return true
	}
	if lc.Quantity <= 1 {
		if err := evedb.DeleteEntity(ctx, m.db, lc.ItemID); err != nil {
			rlogf("rolebots: combat: delete spent charge %d: %v", lc.ItemID, err)
		}
		return false
	}
	if err := evedb.UpdateCargoItemQty(ctx, m.db, lc.ItemID, lc.Quantity-1); err != nil {
		rlogf("rolebots: combat: decrement charge %d: %v", lc.ItemID, err)
	}
	return true
}

// ── electronic warfare ──────────────────────────────────────────────────────

// runEwarCycle keeps the bot's electronic modules working. Each is applied once and refreshed
// on its own cycle, and disengageAll lifts every one when the fight ends, or a rat would stay
// webbed forever. Each module picks its own target (pickEwarTarget).
func (m *Mod) runEwarCycle(eb *embodiedBot, l *combatLoadout, gunTarget *destiny.Entity, hostiles []uint32) {
	now := time.Now()
	for _, ew := range l.ewar {
		if now.Before(ew.nextCycleAt) {
			continue
		}
		target := m.pickEwarTarget(eb, ew, gunTarget, hostiles)
		if ew.Kind == ewScram && l.pointMode == pointSpread {
			target = m.spreadPoint(eb, l, ew, target, hostiles)
		}
		if target == nil {
			m.dropEwar(eb, ew)
			continue
		}
		dist := target.Pos().Sub(eb.ent.Pos()).Len()
		if dist > ew.MaxRange*combatEwarRangeGrace {
			if ew.Kind == ewScram && ew.appliedTo == target.ID {
				m.notePointLost(eb, target)
			}
			m.dropEwar(eb, ew)
			continue
		}
		if !l.drawCap(ew.CapNeed) {
			if ew.Kind == ewScram && ew.appliedTo == target.ID {
				m.notePointLost(eb, target)
			}
			m.dropEwar(eb, ew)
			continue
		}
		if ew.appliedTo != 0 && ew.appliedTo != target.ID {
			m.dropEwar(eb, ew)
		}
		ew.nextCycleAt = now.Add(time.Duration(ew.CycleSec * float64(time.Second)))
		if m.ecmRollMissed(ew, target) {
			m.dropEwar(eb, ew)
			continue
		}
		m.applyEwar(eb, ew, target)
	}
}

// ecmJamPairs is the client's own ladder (godma.py:944, the core's
// electronicJamPairs): jammer strength attribute → victim sensor attribute,
// gravimetric / ladar / magnetometric / radar.
var ecmJamPairs = [4][2]int64{{238, 211}, {239, 209}, {240, 210}, {241, 208}}

// ecmJamOdds is the chance one ECM cycle lands: the jammer's bonus against the victim's
// strongest sensor among the pairs the jammer carries a real bonus for (>= 1.0; a tie on the
// defender side takes the higher attack). 1 when the pair cannot be formed. Pure, for the test.
func ecmJamOdds(attack, defend map[int64]float64) float64 {
	var maxA, maxD float64
	havePair := false
	for _, pair := range ecmJamPairs {
		a := attack[pair[0]]
		if a < 1.0 {
			continue
		}
		d := defend[pair[1]]
		switch {
		case !havePair || d > maxD:
			maxA, maxD, havePair = a, d, true
		case d == maxD && a > maxA:
			maxA = a
		}
	}
	if !havePair || maxD <= 0 {
		return 1
	}
	return math.Min(1, maxA/maxD)
}

// sensorStrengthCache is the victim type's four sensor strengths, read once
// per typeID: the roll runs every ECM cycle on the combat goroutine and the
// type table does not change.
var sensorStrengthCache sync.Map // typeID → map[int64]float64

// ecmRollMissed is the jammer's roll, as a player's jammer rolls it
// (chanceEffectLoop): strength against the victim's strongest sensor, once
// per cycle. The cycle and the capacitor are spent either way. Only a jammer
// rolls; everything else in the rack lands when it is in range.
func (m *Mod) ecmRollMissed(ew *botEwarModule, target *destiny.Entity) bool {
	if ew.Kind != ewJam || target == nil || m.db == nil {
		return false
	}
	typeID := int64(target.Slim.TypeID)
	var defend map[int64]float64
	if v, ok := sensorStrengthCache.Load(typeID); ok {
		defend = v.(map[int64]float64)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		attrs, err := evedb.GetTypeAttributes(ctx, m.db, typeID)
		cancel()
		if err != nil {
			return false
		}
		defend = map[int64]float64{}
		for _, pair := range ecmJamPairs {
			defend[pair[1]] = attrs[pair[1]]
		}
		sensorStrengthCache.Store(typeID, defend)
	}
	return rand.Float64() > ecmJamOdds(ew.Attrs, defend)
}

// ewarKindName is the bridge's name for a module kind (modapi.PvPHooks.EwarLanded).
func ewarKindName(kind int) string {
	switch kind {
	case ewScram:
		return "scram"
	case ewWeb:
		return "web"
	case ewJam:
		return "jam"
	case ewDamp:
		return "damp"
	case ewTrackDisrupt:
		return "trackDisrupt"
	case ewPaint:
		return "paint"
	}
	return ""
}

// ewarLanded hands the VICTIM'S side of an effect to the core: lock break,
// stat push, jam bar (see the hook's doc). The Entity write is already done.
func (m *Mod) ewarLanded(eb *embodiedBot, ew *botEwarModule, target *destiny.Entity, start bool) {
	landed := modapi.PvP().EwarLanded
	if landed == nil || eb.ent == nil || eb.ss == nil || target == nil {
		return
	}
	landed(eb.ent, target, eb.ss, ew.ItemID, ew.FXGUID, ewarKindName(ew.Kind),
		int64(ew.CycleSec*1000), start)
}

// pickEwarTarget answers what one electronic module should hold down. A scrambler and a painter go on the gun
// target; everything else goes on the biggest incoming problem (a web on what is closing, a jammer, damp or
// tracking disruptor on what hits hardest). It falls back to the gun target when the grid cannot answer; in a
// fleet runEwarCycle may move a scrambler to an unheld hostile (pointclaims.go spreadPoint).
func (m *Mod) pickEwarTarget(eb *embodiedBot, ew *botEwarModule,
	gunTarget *destiny.Entity, hostiles []uint32) *destiny.Entity {

	switch ew.Kind {
	case ewScram, ewPaint:
		return gunTarget
	case ewWeb:
		// A tackler's web goes on the gun target, the commander's primary it is holding, never on
		// whichever rat hits hardest: the seat exists to keep one thing from leaving (camp.go).
		if eb.gangRole == RoleTackler {
			return gunTarget
		}
	}
	if eb.ss == nil || eb.ent == nil {
		return gunTarget
	}
	selfPos := eb.ent.Pos()
	reach := ew.MaxRange * combatEwarRangeGrace

	var best *destiny.Entity
	bestScore := -1.0
	for _, id := range hostiles {
		e := eb.ss.GetEntity(id)
		if e == nil || e.IsDestroyed() || e.IsInWarp() || e.IsCloaked() {
			continue
		}
		dist := e.Pos().Sub(selfPos).Len()
		if reach > 0 && dist > reach {
			continue
		}
		// Rat threat is already in the cache from the engagement (threat.go); a pilot is not in it
		// and scores zero, which leaves the gun target's claim standing below.
		score := 0.0
		if rt, ok := ratThreatCached(e.Slim.TypeID); ok {
			score = rt.DPS
			if ew.Kind == ewWeb {
				// A web is about what arrives, not what hurts most once it has: weight the fast and the near,
				// and keep the DPS term so a harmless hauler is not slowed while a battleship walks in.
				score = rt.DPS * (1 + rt.SpeedMS/1000)
			}
		}
		if score > bestScore {
			best, bestScore = e, score
		}
	}
	if best == nil || bestScore <= 0 {
		return gunTarget
	}
	return best
}

// applyEwar lands one electronic module on a target, using the same entity
// API the player and NPC paths write through. On a pilot it goes through
// actOnPilot, so a docking or jumping ship points nobody (crimewatch.go).
func (m *Mod) applyEwar(eb *embodiedBot, ew *botEwarModule, target *destiny.Entity) {
	if target.IsPlayer {
		eb.actOnPilot(func() { m.landEwar(eb, ew, target) })
		return
	}
	m.landEwar(eb, ew, target)
}

// landEwar is applyEwar's body.
func (m *Mod) landEwar(eb *embodiedBot, ew *botEwarModule, target *destiny.Entity) {
	first := ew.appliedTo != target.ID
	ew.appliedTo = target.ID

	switch ew.Kind {
	case ewWeb:
		// speedFactor is a percentage change: -75 → ×0.25.
		mult := 1.0 + ew.Attrs[dogma.AttrSpeedFactor]/100.0
		newSpeed := target.SetSpeedModifier(ew.ItemID, mult)
		m.broadcastMaxSpeed(eb, target, newSpeed)

	case ewScram:
		strength := ew.Attrs[dogma.AttrWarpScrambleStrength]
		if strength <= 0 {
			strength = 1
		}
		target.AddWarpScramble(ew.ItemID, strength)

	case ewJam:
		// Against a rat, jamming means the manager drops its locks. Against a player the server
		// owns that state, and ewarLanded below hands the core the lock break (jamPlayerTargets).
		if !target.IsPlayer && m.npc != nil {
			m.npc.JamTargets(target.ID, time.Now().Add(time.Duration(ew.CycleSec*float64(time.Second))))
		}

	case ewTrackDisrupt:
		target.SetEwarDebuff(ew.ItemID, destiny.EwarDebuff{
			TurretRangeMult:    ew.Attrs[dogma.AttrMaxRangeMultiplier],
			TurretTrackingMult: ew.Attrs[dogma.AttrTrackingSpeedMultiplier],
		})

	case ewDamp:
		target.SetEwarDebuff(ew.ItemID, destiny.EwarDebuff{
			MaxTargetRangeMult: ew.Attrs[dogma.AttrMaxTargetRangeMultiplier],
			ScanResolutionMult: ew.Attrs[dogma.AttrScanResolutionMultiplier],
		})

	case ewPaint:
		target.SetEwarDebuff(ew.ItemID, destiny.EwarDebuff{
			SigRadiusMult: 1.0 + ew.Attrs[dogma.AttrSignatureRadiusBonus]/100.0,
		})
	}

	// Holding a pilot down is the aggression: the same hooks a gun hit runs (landDamage), every cycle, since the core
	// throttles rebroadcasts and claims the security charge once per pair (flaggedPilotsOnGrid reads a pointer as a criminal).
	if target.IsPlayer && eb.ss != nil && eb.ent != nil {
		if note := modapi.PvP().NotePlayerAggression; note != nil {
			note(eb.ent, target, eb.ss)
		}
		if police := modapi.PvP().PoliceCrime; police != nil {
			police(eb.ent, target, eb.ss)
		}
	}
	// The victim's side (a jam's lock break, a damp's stat push, the jam bar) every cycle, as the
	// player loops do; the bar is keyed on the triple, so a re-send refreshes it.
	m.ewarLanded(eb, ew, target, true)

	if first {
		m.registerEffect(eb, ew.ItemID, ew.TypeID, 0, ew.FXGUID, target.ID, ew.CycleSec)
	}
	m.moduleFX(eb, ew.ItemID, ew.TypeID, ew.FXGUID, target.ID, true, int64(ew.CycleSec*1000), true)
	if first {
		m.logBot(eb.charID, logInfo, "combat", "%s on entity %d", ew.Name, target.ID)
		if ew.Kind == ewScram {
			m.notePointLanded(eb, target)
		}
	}
}

// dropEwar lifts one module's effect off whatever it was holding.
func (m *Mod) dropEwar(eb *embodiedBot, ew *botEwarModule) {
	if ew.appliedTo == 0 {
		return
	}
	if eb.ss != nil {
		if target := eb.ss.GetEntity(ew.appliedTo); target != nil {
			switch ew.Kind {
			case ewWeb:
				if newSpeed, removed := target.RemoveSpeedModifier(ew.ItemID); removed {
					m.broadcastMaxSpeed(eb, target, newSpeed)
				}
			case ewScram:
				target.RemoveWarpScramble(ew.ItemID)
				releasePoint(fleetKey(eb), ew.appliedTo, eb.charID)
			case ewTrackDisrupt, ewDamp, ewPaint:
				target.RemoveEwarDebuff(ew.ItemID)
			}
			m.ewarLanded(eb, ew, target, false)
		}
	}
	m.moduleFX(eb, ew.ItemID, ew.TypeID, ew.FXGUID, ew.appliedTo, false, 0, true)
	m.clearEffect(eb, ew.ItemID)
	ew.appliedTo = 0
}

// broadcastMaxSpeed tells every client on grid that a webbed ball's ceiling
// changed, so the victim visibly slows instead of drifting out of sync.
func (m *Mod) broadcastMaxSpeed(eb *embodiedBot, target *destiny.Entity, speed float64) {
	if eb.ss == nil || speed <= 0 {
		return
	}
	action := destiny.BuildSimpleAction("SetMaxSpeed",
		marshal.PyInt{Value: uint64(target.ID)},
		marshal.PyReal{Value: speed},
	)
	// Keyed on the webbed ball, not the webber: several projections webbing one victim describe
	// one ceiling. Queued so a web landing and expiring does not cost every witness two sim-seconds.
	m.queueForBubble(eb.ss, eb.ent.ID, target.ID, keyedAction{"maxspeed", action})
}

// ── effects on the wire ─────────────────────────────────────────────────────

// startWeaponFire begins (or retargets) a weapon's visual on every client.
func (m *Mod) startWeaponFire(eb *embodiedBot, w *botWeapon, targetID uint32) {
	if w.firingAt == targetID {
		return
	}
	if w.firingAt != 0 {
		m.moduleFX(eb, w.ItemID, w.TypeID, w.FXGUID, w.firingAt, false, 0, true)
	}
	w.firingAt = targetID
	m.registerEffect(eb, w.ItemID, w.TypeID, w.ChargeTypeID, w.FXGUID, targetID, w.CycleSec)
	m.moduleFX(eb, w.ItemID, w.TypeID, w.FXGUID, targetID, true, int64(w.CycleSec*1000), true)
}

// stopWeaponFire kills one weapon's visual.
func (m *Mod) stopWeaponFire(eb *embodiedBot, w *botWeapon) {
	if w.firingAt == 0 {
		return
	}
	m.moduleFX(eb, w.ItemID, w.TypeID, w.FXGUID, w.firingAt, false, 0, true)
	m.clearEffect(eb, w.ItemID)
	w.firingAt = 0
}

// registerEffect records a running module on the bot's own entity, where the core's catch-up
// reads it (ensureBallsKnown replays Entity.GetActiveEffects to a client that learns the ball
// mid-fight). Without it a pilot arriving a second late sees a ship shooting nothing, since
// the OnSpecialFX start goes out once. It is the registry a player's and an NPC's modules use.
func (m *Mod) registerEffect(eb *embodiedBot, itemID, typeID, chargeTypeID int64, guid string,
	targetID uint32, cycleSec float64) {

	if eb.ent == nil || guid == "" {
		return
	}
	eb.ent.SetActiveEffect(itemID, destiny.ActiveEffect{
		EntityID:    eb.ent.ID,
		ModuleID:    itemID,
		ModuleType:  typeID,
		TargetID:    targetID,
		OtherType:   chargeTypeID,
		GUID:        guid,
		IsOffensive: true,
		DurationMs:  int64(cycleSec * 1000),
		StartTime:   uint64(evedb.UnixToWin32(time.Now().Unix())),
	})
}

// clearEffect is registerEffect's other half.
func (m *Mod) clearEffect(eb *embodiedBot, itemID int64) {
	if eb.ent != nil {
		eb.ent.ClearActiveEffect(itemID)
	}
}

// stopAllFire silences every gun (target swap, target untouchable).
func (m *Mod) stopAllFire(eb *embodiedBot, l *combatLoadout) {
	for _, w := range l.weapons {
		m.stopWeaponFire(eb, w)
	}
}

// disengageAll ends the engagement cleanly: guns quiet, every debuff lifted. It runs on every
// exit from the combat loop, the bot's death included, since nothing else would clear a rat
// left webbed by a ship that is gone.
func (m *Mod) disengageAll(eb *embodiedBot, l *combatLoadout) {
	if l == nil {
		return
	}
	m.stopAllFire(eb, l)
	for _, ew := range l.ewar {
		m.dropEwar(eb, ew)
	}
	releasePointsOf(eb.charID)
	m.propOff(eb, l)
}

// ewarCoversFlight reports whether a module is worth keeping running while the ship aligns
// out: a web (it persists without re-cycling) and a jammer (re-cycled by runFlightEwar). A
// scrambler is not: holding a point while running asks the target to stay and shoot, and
// against a pilot it is a crime for no gain.
func ewarCoversFlight(kind int) bool { return kind == ewWeb || kind == ewJam }

// disengageForFlight is disengageAll for a ship that is leaving: guns quiet, and everything
// lifted except what buys the align. The full lift still happens at standDown and in the
// loop's defer on death and teardown.
func (m *Mod) disengageForFlight(eb *embodiedBot, l *combatLoadout) {
	if l == nil {
		return
	}
	m.stopAllFire(eb, l)
	for _, ew := range l.ewar {
		if ewarCoversFlight(ew.Kind) {
			continue
		}
		m.dropEwar(eb, ew)
	}
}

// runFlightEwar keeps the escape rack cycling while the ship aligns out: only the modules
// that cover a flight, and only against something actually shooting (pickEwarTarget's grid
// arm). A jam must be re-applied on its own cycle or it lapses; a web does not.
func (m *Mod) runFlightEwar(eb *embodiedBot, l *combatLoadout, hostiles []uint32) {
	if l == nil || len(l.ewar) == 0 || len(hostiles) == 0 {
		return
	}
	now := time.Now()
	for _, ew := range l.ewar {
		if !ewarCoversFlight(ew.Kind) || now.Before(ew.nextCycleAt) {
			continue
		}
		target := m.pickEwarTarget(eb, ew, nil, hostiles)
		if target == nil {
			m.dropEwar(eb, ew)
			continue
		}
		if target.Pos().Sub(eb.ent.Pos()).Len() > ew.MaxRange*combatEwarRangeGrace {
			m.dropEwar(eb, ew)
			continue
		}
		if !l.drawCap(ew.CapNeed) {
			m.dropEwar(eb, ew)
			continue
		}
		if ew.appliedTo != 0 && ew.appliedTo != target.ID {
			m.dropEwar(eb, ew)
		}
		ew.nextCycleAt = now.Add(time.Duration(ew.CycleSec * float64(time.Second)))
		if m.ecmRollMissed(ew, target) {
			m.dropEwar(eb, ew)
			continue
		}
		m.applyEwar(eb, ew, target)
	}
}

// moduleFX is the OnSpecialFX a module's activation puts on the wire: the payload a player's
// client sends, sourced from the bot since it has no connection.
func (m *Mod) moduleFX(eb *embodiedBot, itemID, typeID int64, guid string,
	targetID uint32, start bool, durationMs int64, offensive bool) {

	if eb.ss == nil || eb.ent == nil || guid == "" {
		return
	}
	startVal := int64(0)
	if start {
		startVal = 1
	}
	offVal := int64(0)
	if offensive {
		offVal = 1
	}
	if durationMs <= 0 {
		durationMs = 5000
	}
	var targetRep marshal.PyRep = marshal.PyNone{}
	if targetID != 0 {
		targetRep = marshal.PyInt{Value: uint64(targetID)}
	}

	args := []marshal.PyRep{
		marshal.PyInt{Value: uint64(eb.ent.ID)},
		marshal.PyInt{Value: uint64(itemID)},
		marshal.PyInt{Value: uint64(typeID)},
		targetRep,
		marshal.PyNone{},
		marshal.PyList{},
		marshal.PyString{Value: guid},
		marshal.PyInt{Value: uint64(offVal)},
		marshal.PyInt{Value: uint64(startVal)},
		marshal.PyInt{Value: uint64(startVal)},
	}
	if start {
		args = append(args,
			marshal.PyReal{Value: float64(durationMs)},
			marshal.PyInt{Value: destiny.FXRepeatCount},
		)
	}
	action := destiny.BuildSimpleAction("OnSpecialFX", args...)

	for _, mate := range eb.ss.GetBubbleConnectedPlayersExcept(eb.ent.ID) {
		// Both balls, as the core and the POS guns require: an effect naming a target the client does
		// not hold is dropped, and a later witness is caught up by the registered effect (ensureBallsKnown).
		if mate.Conn == nil || !mate.KnowsBall(eb.ent.ID) ||
			(targetID != 0 && mate.ID != targetID && !mate.KnowsBall(targetID)) {
			continue
		}
		if err := mate.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionListEvents(mate.Conn, m.nodeID, mate.AccountID, stamp, nil, action)
		}); err != nil {
			rlogf("rolebots: module FX to %s: %v", mate.Addr, err)
		}
	}
}

// ── leaving ─────────────────────────────────────────────────────────────────

// fleeGrid gets the bot out. Warp is the only real escape, so a scrambled ship burns instead,
// and says so. An escape is one decision: the destination is latched for the whole break-off
// and a break-off in flight is left alone, so it is safe to call every tick. The class is the
// caller's. See docs/comment-notes-b02.md#flee-grid.
//
// It reports whether the ship is leaving: a warp or dock is under way, latched or just started.
// False means it is stuck here (warp-scrambled, no destination, a hop refused), and the combat
// loop must keep fighting rather than wait out a break-off that cannot happen.
func (m *Mod) fleeGrid(ctx context.Context, eb *embodiedBot, l *combatLoadout, class warpClass) bool {
	// Not disengageAll: a webbed or jammed pursuer is what buys the align. Everything lifts at
	// stand-down (see disengageForFlight).
	m.disengageForFlight(eb, l)

	eb.mu.Lock()
	// wasFleeing lets a refused hop restore it: a refusal must leave the projection identical to
	// an idle one (deferWarp's contract), and the idle wander, gang follow and escort treat a fleeing bot as busy.
	wasFleeing := eb.fleeing
	eb.fleeing = true
	warping, fleeDest, blocked, from := eb.warping, eb.fleeDest, eb.fleeBlocked, eb.currentDest
	eb.mu.Unlock()

	// Already leaving. Warp is not interruptible by this loop, and re-issuing it cancels it.
	if fleeDest != 0 && warping {
		return true
	}

	// The two exits that leave the ship where it is are reported once per break-off, not once
	// per tick.
	report := func(level, format string, args ...any) {
		if blocked {
			return
		}
		eb.mu.Lock()
		eb.fleeBlocked = true
		eb.mu.Unlock()
		m.logBot(eb.charID, level, "combat", format, args...)
	}

	if eb.ent != nil && eb.ent.WarpScrambleTotal() > 0 {
		report(logWarn, "warp scrambled — cannot disengage, fighting it out at %.0f%% hull",
			eb.hullFraction()*100)
		return false
	}

	// On an undock, dock back rather than warp (gank_undock.go).
	if fleeDest == 0 && m.tryRedock(eb) {
		return true
	}

	// Anywhere but here: a station if the system has one (it can dock and the abstract tier
	// re-plans from safety), else the nearest celestial that is not the one it is being shot on.
	if fleeDest != 0 {
		from = fleeDest
	}
	dest := m.fleeDestination(eb, from)
	// A running gank striker heads for its escape safe, never the station
	// beside the crime (gank_undock.go).
	if eb.gankStrike && eb.gank.escape != nil && eb.gank.escape.ItemID != from {
		esc := *eb.gank.escape
		dest = &esc
	}
	// A fleet under a commander's retreat call runs to the celestial the commander named
	// (fleetcommand_grid.go), so the withdrawal lands as a fleet, unless that is where it flees from.
	if called, to := fleetRetreatCalled(eb); called && to != 0 && to != from {
		for i := range eb.celestials {
			if eb.celestials[i].ItemID == to {
				dest = &eb.celestials[i]
				break
			}
		}
	}
	if dest == nil {
		report(logWarn, "nowhere to run to in this system")
		return false
	}
	// A miner run off a belt keeps off it for a while and calls its gang's guns there
	// (beltbail.go); the destination is not yet the bolt-hole, so the belt is still underfoot.
	if eb.mining {
		noteBeltBail(eb)
	}
	m.stopEmbodyMining(eb)
	eb.setMineTarget(0)
	eb.mu.Lock()
	eb.fleeDest = dest.ItemID
	eb.fleeBlocked = false
	eb.mu.Unlock()
	if !m.startEmbodyWarpTo(eb, *dest, class) {
		// Refused by the budget, which only a discretionary caller can be. Put the latch back, or the retry would pick
		// another destination. The mining stop stands; the busy flag does not (see wasFleeing).
		eb.mu.Lock()
		eb.fleeDest = fleeDest
		eb.fleeing = wasFleeing
		eb.mu.Unlock()
		return false
	}
	// Logged after admission: a refused hop is a bot that had a quiet second (deferWarp's
	// contract), and announcing a break-off that did not happen is noise. The reason is the rule
	// the combat loop said called it (breakOffReason); a flee from elsewhere has none.
	eb.mu.Lock()
	why := eb.breakReason
	eb.breakReason = ""
	eb.mu.Unlock()
	if why == "" {
		why = "pre-emptive"
	}
	m.logBot(eb.charID, logInfo, "combat",
		"breaking off at %.0f%% hull, %.0f%% tank (%s) — warping to %s",
		eb.hullFraction()*100, eb.tankFraction()*100, why, celestialLabel(*dest))

	// Hold the loop open until the warp actually starts, so the caller does
	// not immediately re-enter combat and cancel it.
	_ = embodySleep(ctx, 5*time.Second)
	return true
}

// clearFlee ends the break-off. An engaged flee ends when the engagement stands down: a
// quiet grid is the evidence the ship got away. A flee that began with nothing shooting has
// no engagement, and settlePreemptiveFlee ends it on landing (beltbail.go).
func (m *Mod) clearFlee(eb *embodiedBot) {
	eb.mu.Lock()
	eb.fleeing, eb.fleeDest, eb.fleeBlocked = false, 0, false
	eb.preFleeAt = time.Time{}
	// A staged drone flight is for the engagement that is now over.
	eb.droneStaged = false
	eb.mu.Unlock()
}

// fleeDestination picks where a bot runs to: the nearest station a warp away, else in 0.0 a tower
// whose force field admits it (towerHaven), else the nearest other celestial, never the grid it is
// running from (nearestFleeTarget).
func (m *Mod) fleeDestination(eb *embodiedBot, from int64) *evedb.CelestialEntity {
	var pos destiny.Vec3
	minRange := navWarpMinRange
	if eb.ent != nil {
		pos = eb.ent.Pos()
	} else {
		// No ball to measure from: every distance is equal, so the pick falls back to list order.
		minRange = math.Inf(1)
	}
	dest := nearestFleeTarget(eb.celestials, pos, from, minRange)
	if dest != nil && dest.GroupID == groupStation {
		return dest
	}
	if haven := m.towerHaven(eb.corpID, eb.systemID, pos); haven != nil && haven.ItemID != from {
		return haven
	}
	return dest
}
