package rolebots

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// generatedFitID is the fit ID of a generated fit: the negative of its hull type, so it cannot clash
// with an authored positive ID and what a pilot owns reads as the ship it built on hull X
// (rolebot_fit_ships). Fits are built per pilot and job from the hull, skills and market.
// See docs/comment-notes-b09.md#fit-generation.
func generatedFitID(hullTypeID int64) int64 { return -hullTypeID }

// isGeneratedFit reports whether a fitID names a generated fit. The harness asserts the sign
// convention, which keeps hullOfGeneratedFit and generatedFitID inverses of each other.
func isGeneratedFit(fitID int64) bool { return fitID < 0 }

// hullOfGeneratedFit recovers the hull a generated fitID was built on.
func hullOfGeneratedFit(fitID int64) int64 {
	if fitID < 0 {
		return -fitID
	}
	return 0
}

const (
	// fitGenMarketTTL is how long the buyable-module catalogue is reused.
	// Prices and availability move when somebody trades; the catalogue is
	// read for every pilot on every plan pass, so it cannot be per-call.
	fitGenMarketTTL = 5 * time.Minute
	// fitGenChargeQty is how many rounds a generated fit carries. Enough
	// for a belt, small enough that the shopping trip is not the trip.
	fitGenChargeQty = 200
	// fitGenUnsoldPenalty is what a hull nobody sells is worth against one on the market. Not zero: it
	// stays a candidate so it can be wanted (wants.go), which makes a shortage visible to industrialists.
	fitGenUnsoldPenalty = 0.5
	// fitGenOwnedBonus keeps a pilot in the hull it owns unless something is clearly better: hysteresis
	// applied where hulls are ranked.
	fitGenOwnedBonus = 1.4
)

// ── the buyable-module catalogue ────────────────────────────────────────────

// marketModule is one module type a pilot could actually buy right now.
type marketModule struct {
	TypeID   int64
	Name     string
	GroupID  int64
	Slot     int64 // flagHiFirst | flagMedFirst | flagLoFirst; 0 = not a slot module
	Turret   bool
	Launcher bool
	CPU, PG  float64
	// ChargeSize is the authority on what size a weapon is. Module names mislead: a Medium Beam Laser I
	// is chargeSize 1, a small turret, so nothing reads a size from a name.
	ChargeSize int
	// MetaLevel is attribute 633 as shipped and is incomplete; moduleGradeLevel is what the scorer reads.
	MetaLevel int
	// MetaGroup is invmetatypes.metaGroupID: 1 tech one, 2 tech two,
	// 3 storyline, 4 faction, 5 officer, 6 deadspace, 0 for a type with no
	// row. It is the band; MetaLevel only places a type inside one.
	MetaGroup int
	// ChargeDmg is the damage of the best round this weapon can load, so the two weapon families compare
	// in one unit: a turret carries its damage multiplier as an attribute and a launcher does not (the
	// warhead does). Same arithmetic as ComputeFitStats' volley. 0 when no charge could be read.
	ChargeDmg float64
	// ChargeReachM is how far the best round this weapon can load flies. A launcher has no range of its
	// own (the missile does), so scoring it by nothing would read every missile boat as point blank. Zero
	// for a turret, whose reach comes from its attributes (fitreach.go weaponReachM).
	ChargeReachM float64
	Ask          float64           // cheapest live ask anywhere
	Attrs        map[int64]float64 // the handful of attributes scoring reads
	// Style and Size are the module's hard tags (modtags.go): the engagement
	// range it serves and the hull size it was built for. Derived at load,
	// never stored.
	Style modStyle
	Size  modSize
	// MaxCopies caps how many of this type one fit may plan, 0 for no cap. It is set only on an entry no
	// seller in reach lists that the catalogue carries because the pilot owns it (fitted, via
	// catalogueWithIncumbents, or in a hangar, via withOwnStock), and equals what the pilot owns.
	MaxCopies int64
}

// dropExhausted is the catalogue minus a capped type once a fit has planned
// as many as the pilot owns. A copy: the catalogue may be shared.
func dropExhausted(cat []*marketModule, typeID int64) []*marketModule {
	out := make([]*marketModule, 0, len(cat))
	for _, mm := range cat {
		if mm.TypeID != typeID {
			out = append(out, mm)
		}
	}
	return out
}

var (
	modCatMu  sync.Mutex
	modCatAt  time.Time
	modCatAll []*marketModule
)

// fitGenScoreAttrs are the attributes a module is scored on, a short list because the query runs over
// every published module and the stat engine does the real arithmetic later.
var fitGenScoreAttrs = append([]int64{
	attrCPUUse, attrPGUse, attrChargeSize,
	attrMiningAmount,     // 77: mining laser yield
	attrRateOfFire,       // 51: cycle time, for anything that cycles
	attrDamageMultTurret, // 64: turret damage, and damage mods
	attrMissileDmgMult,   // 213: ballistic control
	attrShieldBonus,      // 68: shield booster
	attrCapacityBonus,    // 72: shield extender
	attrCargoMult,        // 149: expanded cargohold
	attrCPUMultMod, attrPGMult, attrPGAddFlat, // 202, 145, 549: fitting-capacity modules (fitgrid.go)
	attrShieldBoostPct, // 548: Shield Boost Amplifier (fitlocalrep.go)
	attrLockRangeMult, attrScanResMult, // 237, 565: signal amplifier, sensor booster
	attrMiningAmountBonus, // 434: mining upgrade yield (fitminingupgrade.go)
	attrCPUPenaltyPct,     // 1082: what an upgrade costs the miners in CPU
	attrWarpScrambleStr,  // 105: warp scrambler / disruptor points
	attrCargoScanRange,   // 126: cargo scanner reach (roleScanner)
	// 20 is both ends of the speed argument: a stasis web carries it negative and a propulsion module
	// positive. Attribute 80 is the passive velocity attribute (overdrives, nanofibers) and no prop mod
	// in the database has it.
	attrSpeedFactor,         // 20: web bite (negative) / prop boost (positive)
	attrTackleRange,         // 54: tackle reach
	attrMetaLevel,           // 633
	attrPropMass,            // 796: propulsion size, since the name misleads
	attrSigRadiusBonus,      // 554: signature bloom, what makes a prop mod an MWD
	attrCapNeed,             // 6: cap per cycle
	attrDuration,            // 73: cycle time, for the cap sum
	attrCapRechMult,         // 144: cap recharger
	attrCapacitorBonusFlat,  // 67: cap battery
	attrMaxGroupActive,      // 763: the server's own only-one-of-these limit
	attrArmorRepAmount,      // 84: armor repair amount, local and remote
	attrShieldTransferRange, // 87: remote shield rep reach
	attrFalloff,             // 158: turret falloff, the other half of reach
	// The three required-skill slots and the first charge group: what
	// minerKindOf (minercap.go) reads to tell a deep core miner from a laser,
	// the same way the core's mining gate does.
	182, 183, 184, // required skills 1-3
	604,           // chargeGroup1 (dogma.AttrChargeGroup1)
	// The tag reads (modtags.go) and the honest speed (fitstats.go): which
	// group-78 family a hull mod is, a prop module's thrust, and the EWAR
	// and energy-transfer numbers the seat lenses score on.
	attrSpeedBonusPct, attrAgilityPct, attrStructHPMult, attrSpeedBoostFactor,
	attrTrackingSpeedMult, attrPowerTransferAmt, attrPowerTransferRng,
	237, 238, // sensor damper multipliers (EWAR seat lens)
	attrTrackingRangeMult, // 243: tracking computer / enhancer optimal
	attrArmorPlateHPAdd,     // 335: armour plate buffer
	// Without 335 the TankArmor arm reads only attribute 65, which a plate does not carry, so every plate
	// scores 0 and is never fitted.
}, fitGenResistAttrs()...)

// fitGenResistAttrs is every resistance attribute the generator scores on: the four stacked bonuses
// hardeners, coatings and platings carry, and the twelve resonances only a damage control does. A
// function, not an append chain, because appending onto resistBonusAttrs[:] could write through into
// the array that fitstats.go and resistScore share.
func fitGenResistAttrs() []int64 {
	out := make([]int64, 0, len(resistBonusAttrs)+len(dcResonanceAttrs))
	out = append(out, resistBonusAttrs[:]...) // 984-987
	out = append(out, dcResonanceAttrs[:]...) // 267-274, 974-977
	return out
}

// Attribute IDs this file needs beyond the ones already named elsewhere.
const (
	attrShieldBonus   = 68
	attrCapacityBonus = 72
	attrMetaLevel     = 633
	// attrSigRadiusBonus is the signature bloom a microwarpdrive carries and an afterburner does not (56
	// of the 115 published propulsion modules). It is the class test for isMicrowarpdrive; nothing reads
	// it as a stat.
	attrSigRadiusBonus = 554
	// The same attribute as attrMaxRange in fitstats.go, named for what it
	// means here: how far this pilot can hold somebody.
	attrTackleRange = attrMaxRange
)

// buyableModules is every published module with at least one live sell order anywhere, with the
// attributes scoring needs and the slot band it occupies. Anywhere, not within reach: this decides
// what a fit consists of, while procurement decides whether the trip is worth making. Filtering by
// distance here would make a fit depend on where the pilot stands when it plans.
func (m *Mod) buyableModules(ctx context.Context) ([]*marketModule, error) {
	modCatMu.Lock()
	if modCatAll != nil && clock.Now().Sub(modCatAt) < fitGenMarketTTL {
		out := modCatAll
		modCatMu.Unlock()
		return out, nil
	}
	modCatMu.Unlock()

	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, MIN(o.price)
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 7
		JOIN market_orders o ON o.typeID = t.typeID
		     AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		WHERE t.published = 1
		GROUP BY t.typeID, t.typeName, t.groupID`)
	if err != nil {
		return nil, fmt.Errorf("buyableModules: %w", err)
	}
	byID := map[int64]*marketModule{}
	var list []*marketModule
	for rows.Next() {
		mm := &marketModule{Attrs: map[int64]float64{}}
		if err := rows.Scan(&mm.TypeID, &mm.Name, &mm.GroupID, &mm.Ask); err != nil {
			rows.Close()
			return nil, fmt.Errorf("buyableModules scan: %w", err)
		}
		byID[mm.TypeID] = mm
		list = append(list, mm)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}

	if err := m.fillModuleAttrs(ctx, byID); err != nil {
		return nil, err
	}
	if err := m.fillModuleSlots(ctx, byID); err != nil {
		return nil, err
	}
	for _, mm := range list {
		mm.CPU = mm.Attrs[attrCPUUse]
		mm.PG = mm.Attrs[attrPGUse]
		mm.ChargeSize = int(mm.Attrs[attrChargeSize])
		mm.MetaLevel = int(mm.Attrs[attrMetaLevel])
	}
	if err := m.fillModuleMetaGroups(ctx, byID); err != nil {
		return nil, err
	}
	// Must run after fillModuleSlots, which is what sets Turret/Launcher.
	m.fillWeaponChargeDamage(ctx, list)
	// Tags come from the static catalogue (allModulesByID), measured over every published module, so a
	// market-thinned list does not move the turret split. Measured here only if that read fails.
	if every, aerr := m.allModulesByID(ctx); aerr == nil {
		for _, mm := range list {
			if e := every[mm.TypeID]; e != nil {
				mm.Style, mm.Size = e.Style, e.Size
			}
		}
	} else {
		tagModules(list)
	}

	modCatMu.Lock()
	modCatAll, modCatAt = list, clock.Now()
	modCatMu.Unlock()
	return list, nil
}

// allModulesByID is every published module with its fitting attributes, sold or not: static data
// loaded once per process. It exists for incumbents (catalogueWithIncumbents), since what a ship wears
// must be scorable whether or not anybody lists it. Ask stays 0.
var (
	allModsMu   sync.Mutex
	allModsByID map[int64]*marketModule
)

func (m *Mod) allModulesByID(ctx context.Context) (map[int64]*marketModule, error) {
	allModsMu.Lock()
	defer allModsMu.Unlock()
	if allModsByID != nil {
		return allModsByID, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 7
		WHERE t.published = 1`)
	if err != nil {
		return nil, fmt.Errorf("allModules: %w", err)
	}
	byID := map[int64]*marketModule{}
	var list []*marketModule
	for rows.Next() {
		mm := &marketModule{Attrs: map[int64]float64{}}
		if err := rows.Scan(&mm.TypeID, &mm.Name, &mm.GroupID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("allModules scan: %w", err)
		}
		byID[mm.TypeID] = mm
		list = append(list, mm)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := m.fillModuleAttrs(ctx, byID); err != nil {
		return nil, err
	}
	if err := m.fillModuleSlots(ctx, byID); err != nil {
		return nil, err
	}
	for _, mm := range list {
		mm.CPU = mm.Attrs[attrCPUUse]
		mm.PG = mm.Attrs[attrPGUse]
		mm.ChargeSize = int(mm.Attrs[attrChargeSize])
		mm.MetaLevel = int(mm.Attrs[attrMetaLevel])
	}
	if err := m.fillModuleMetaGroups(ctx, byID); err != nil {
		return nil, err
	}
	m.fillWeaponChargeDamage(ctx, list)
	// The tags are measured over the whole catalogue, so a turret's line does not depend on which
	// siblings are for sale today.
	tagModules(list)
	allModsByID = byID
	return byID, nil
}

// catalogueWithIncumbents is the scoped catalogue plus every module the ship
// already carries that the scope left out. Pure, so the harness pins it.
func catalogueWithIncumbents(scoped []*marketModule, every map[int64]*marketModule, own *ownedLoadout) []*marketModule {
	if own == nil || len(own.byBand) == 0 {
		return scoped
	}
	fitted := map[int64]int64{}
	for _, band := range own.byBand {
		for typeID, n := range band {
			if n > 0 {
				fitted[typeID] += int64(n)
			}
		}
	}
	at := make(map[int64]int, len(scoped))
	for i, mm := range scoped {
		at[mm.TypeID] = i
	}
	var out []*marketModule // copied on first change
	var extra []*marketModule
	for typeID, n := range fitted {
		if i, ok := at[typeID]; ok {
			// Sold in reach: no cap. Hangar stock only (withOwnStock): what is
			// fitted adds to what the pilot owns.
			if scoped[i].MaxCopies == 0 {
				continue
			}
			if out == nil {
				out = append([]*marketModule(nil), scoped...)
			}
			c := *scoped[i]
			c.MaxCopies += n
			out[i] = &c
			continue
		}
		if mm := every[typeID]; mm != nil {
			c := *mm
			c.MaxCopies = n // nobody in reach sells it: only the copies it owns
			extra = append(extra, &c)
		}
	}
	if out == nil {
		out = scoped
	}
	if len(extra) == 0 {
		return out
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].TypeID < extra[j].TypeID })
	merged := make([]*marketModule, 0, len(out)+len(extra))
	return append(append(merged, out...), extra...)
}

func (m *Mod) fillModuleAttrs(ctx context.Context, byID map[int64]*marketModule) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ta.typeID, ta.attributeID,
		       COALESCE(NULLIF(ta.valueFloat, 0), CAST(ta.valueInt AS DOUBLE), ta.valueFloat)
		FROM dgmTypeAttributes ta
		JOIN invTypes t ON t.typeID = ta.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 7
		WHERE t.published = 1 AND ta.attributeID IN (`+placeholders(len(fitGenScoreAttrs))+`)`,
		int64Args(fitGenScoreAttrs)...)
	if err != nil {
		return fmt.Errorf("fillModuleAttrs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, attrID int64
		var v float64
		if err := rows.Scan(&typeID, &attrID, &v); err != nil {
			return fmt.Errorf("fillModuleAttrs scan: %w", err)
		}
		if mm := byID[typeID]; mm != nil {
			mm.Attrs[attrID] = v
		}
	}
	return rows.Err()
}

// fillModuleMetaGroups reads invmetatypes.metaGroupID for the catalogue. It is a separate pass
// because the catalogue query is grouped (MIN(price) per type) and every joined column would have to
// survive the GROUP BY. A missing row is common and not an error: 0 reads as no band, which
// moduleGradeLevel treats as plain tech one.
func (m *Mod) fillModuleMetaGroups(ctx context.Context, byID map[int64]*marketModule) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT mt.typeID, mt.metaGroupID
		  FROM invmetatypes mt
		  JOIN invTypes t ON t.typeID = mt.typeID AND t.published = 1
		  JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 7`)
	if err != nil {
		// The catalogue is usable without it: every module falls back to its attribute-633 reading. Not
		// worth failing a fit over.
		rlogf("rolebots: module meta groups: %v (grading on attribute 633 alone)", err)
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, grp int64
		if err := rows.Scan(&typeID, &grp); err != nil {
			return fmt.Errorf("fillModuleMetaGroups scan: %w", err)
		}
		if mm := byID[typeID]; mm != nil {
			mm.MetaGroup = int(grp)
		}
	}
	return rows.Err()
}

// The meta bands, as invmetatypes ships them. Same constants as lootgrade.go, restated because the
// two answer different questions: that file decides what a drop is worth, this one what it is good for.
const (
	fitMetaTech2     = 2
	fitMetaStoryline = 3
	fitMetaFaction   = 4
	fitMetaOfficer   = 5
	fitMetaDeadspace = 6
)

// moduleGradeLevel is the meta level the scorer uses. Attribute 633 is not the ladder it looks like,
// so the band comes from invmetatypes and 633 only refines inside it. It stays a shallow quality
// ladder, since a steeper one would let a name beat a module that fits the hull.
// See docs/comment-notes-b09.md#module-grade.
func moduleGradeLevel(mm *marketModule) int {
	if mm == nil {
		return 0
	}
	floor := 0
	switch mm.MetaGroup {
	case fitMetaTech2:
		floor = 5
	case fitMetaStoryline:
		floor = 6
	case fitMetaFaction:
		floor = 7
	case fitMetaDeadspace:
		floor = 8
	case fitMetaOfficer:
		floor = 9
	}
	if mm.MetaLevel > floor {
		return mm.MetaLevel
	}
	return floor
}

// fillModuleSlots reads which band a module occupies from its effects, not its group: the effect is
// what the server fits by, and a group is not a slot (Shield Amplifiers and Armor Coatings look alike
// and sit in different bands).
func (m *Mod) fillModuleSlots(ctx context.Context, byID map[int64]*marketModule) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT te.typeID, te.effectID
		FROM dgmTypeEffects te
		JOIN invTypes t ON t.typeID = te.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 7
		WHERE t.published = 1 AND te.effectID IN (?, ?, ?, ?, ?)`,
		effLoPower, effMedPower, effHiPower, effTurretFitted, effLauncherFitted)
	if err != nil {
		return fmt.Errorf("fillModuleSlots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, eff int64
		if err := rows.Scan(&typeID, &eff); err != nil {
			return fmt.Errorf("fillModuleSlots scan: %w", err)
		}
		mm := byID[typeID]
		if mm == nil {
			continue
		}
		switch eff {
		case effLoPower:
			mm.Slot = flagLoFirst
		case effMedPower:
			mm.Slot = flagMedFirst
		case effHiPower:
			mm.Slot = flagHiFirst
		case effTurretFitted:
			mm.Turret = true
		case effLauncherFitted:
			mm.Launcher = true
		}
	}
	return rows.Err()
}

// ── what a ship wants on it ─────────────────────────────────────────────────

// modRole is what a slot is being filled for: the generator's whole vocabulary. A wish list is roles
// in priority order, and each role knows which module groups serve it and how to score one.
type modRole string

const (
	roleWeapon modRole = "weapon" // the hull's own family, at the hull's own size
	roleMiner  modRole = "miner"
	// roleDeepCore is a mining slot that wants a deep core miner, the only module Mercoxit answers to
	// (minercap.go). It is tried first in one laser slot of every mining fit and falls through to
	// roleMiner in the same wish.
	roleDeepCore modRole = "deepcore"
	roleCargo  modRole = "cargo"
	// roleMiningUpgrade is the miner's yield module (fitminingupgrade.go): its own role because its unit
	// is a percentage of the lasers' yield, so it can never be score-compared with cargo (miningUpgradeCount
	// decides the mix).
	roleMiningUpgrade modRole = "miningupgrade"
	// roleSensor is the signal amplifier and the sensor booster (fitlocalrep.go). Backfill-only for the
	// combat jobs and scored under a first damage mod (0.25 against ~1.1), so it takes a slot only when
	// the roles above have stood down, as a fourth damage mod (~0.1) has.
	roleSensor modRole = "sensor"
	roleTank   modRole = "tank"   // resolved to shield or armor by the hull
	roleDamage modRole = "damage" // gyro / heat sink / mag stab / ballistic control
	roleSpeed  modRole = "speed"
	roleCap    modRole = "cap"
	// Tackle, the piracy rack, as two roles bought in this order: the scrambler decides whether there is
	// a robbery at all, the web whether it is a quick one. A hull with one free mid gets the point.
	roleTackle modRole = "tackle" // warp scrambler / disruptor
	roleWeb    modRole = "web"    // stasis web
	// The gang-seat racks. Remote rep resolves to the hull's own tank flavour as roleTank does (a Rifter
	// reps armour, an Osprey shield); EWAR is the tackler's leftover mids.
	roleRemoteRep modRole = "remoterep"
	roleEwar      modRole = "ewar"
	// roleDamageControl is its own role because a damage control covers all three layers, so layerAffinity
	// would be wrong for whichever layer it was filed under. roleHarden is its own role for units: roleTank
	// scores hundreds for an extender and tens for a hardener, which would lose every slot.
	roleDamageControl modRole = "damagecontrol"
	roleHarden        modRole = "harden"
	// The hauler's answer to the pirate. A low slot spent on not being held
	// still, which is the only defence a ship whose job is to leave has.
	roleWarpCore modRole = "warpcore"
	// The scout's eye (JobScout): a cargo scanner, scored by reach. Its own role because nothing else
	// shares its unit (metres of scan range) and the scout wish must place exactly one, which oneWish
	// guarantees.
	roleScanner modRole = "scanner"
)

// roleGroups maps a role to the module groups that serve it. Weapons and
// tanks are resolved per hull (weaponGroups / tankGroups) because the
// answer depends on what the hull is.
var roleGroups = map[modRole][]int64{
	roleMiner:    {grpMiningLaser, grpStripMiner, grpDeepCoreMiner},
	roleDeepCore: {grpMiningLaser, grpDeepCoreMiner},
	roleCargo:    {grpCargoExpander},
	roleMiningUpgrade: {grpMiningUpgrade},
	roleSensor:        {grpSignalAmp, grpSensorBooster},
	roleDamage: {grpGyrostab, grpHeatSink, grpMagStab, grpBCS},
	roleSpeed:  {grpAfterburner},
	roleCap:    {grpCapRecharger, groupCapBatteryID},
	roleTackle: {grpWarpScrambler},
	roleWeb:    {grpStasisWeb},
	roleEwar:   {grpTargetPainter, grpTrackingDisruptor, grpECM, grpSensorDamper},

	roleDamageControl: {grpDamageControl},
	roleWarpCore:      {grpWarpCoreStab},
	roleScanner:       {grpCargoScanner},
	// The kiter's legs (fitstyle.go): group 78 read for speed, filtered to
	// the Kite-tagged families by styleAdmits.
	roleSpeedMod: {grpCargoExpander},
	roleTracking: {grpTrackingEnhancer, grpTrackingComputer},
	// Hardeners resolve per hull like tankGroups does: see hardenerGroups.
}

// hardenerGroups is the active-resist family that hardens the layer this hull
// actually holds, resolved per hull for the same reason remoteRepGroups is:
// an armour hull hardening its shields is spending a slot on the layer it does
// not fight out of.
func hardenerGroups(t TankPref) []int64 {
	if t == TankArmor {
		return []int64{grpArmorHardener}
	}
	return []int64{grpShieldHardener}
}

// remoteRepGroups is the remote-repair family that mends the layer this hull's friends tank, resolved
// per hull like tankGroups: a logi reps what its wing flies, and its wing is its own corp's hulls,
// which tank what this hull tanks.
func remoteRepGroups(t TankPref) []int64 {
	if t == TankArmor {
		return []int64{grpArmorRepProjector}
	}
	return []int64{grpShieldTransporter}
}

// Which guns and damage mods a hull reaches for is hulldoctrine.go's weaponAffinity and damageAffinity:
// preferences, not vetoes. hulldoctrine.go owns the launcher size table (launcherBands) so the sizes
// cannot drift between two copies.

// tankGroups is what holds the layer this hull is built to hold. A list because hullAffinity reads it
// from both sides: this hull's layer at full weight and the other one at a discount.
func tankGroups(t TankPref) []int64 {
	if t == TankArmor {
		// Repairer, coating, energized plating and the plate: an armour hull's lows must hold hitpoints. The
		// duel has no repair term (combat_abstract.go), so a repairer is buffer the sim cannot see.
		return []int64{grpArmorRepairer, 98, 326, grpArmorReinforcer}
	}
	return []int64{grpShieldBooster, grpShieldExtender, 295, grpShieldBoostAmp} // booster, extender, resist amp, boost amp
}

// propWishes is the engine, as its own Count: 1 mid wish, or nothing on a hull with fewer than minMed
// mids. Its own wish because bestForWish restarts the role order per slot, so a role that must appear
// exactly once is guaranteed structurally. Callers subtract len() from the leftover count.
func propWishes(med, minMed int) []slotWish {
	if med < minMed {
		return nil
	}
	return []slotWish{{Band: flagMedFirst, Roles: []modRole{roleSpeed}, Count: 1}}
}

// dcWishes is the damage control, as its own Count as propWishes is: a role that must appear exactly
// once is guaranteed structurally. Folded into a shared low wish it would be crowded out by damage
// mods or fill the rack with copies the server refuses to run. minLo is 2: a one-low hull's low goes
// to the gun multiplier.
func dcWishes(lo, minLo int) []slotWish {
	if lo < minLo {
		return nil
	}
	return oneWish(flagLoFirst, roleDamageControl, lo)
}

// armorDamageWishes is the one low an armour gunboat keeps for a damage mod, out of avail lows left
// after the damage control. An armour hull tanks out of the rack its damage mods live in, and a
// {roleTank, roleDamage} wish never reaches damage because tank roles stay above roleYieldFloor. One,
// and only when another low is left for tank; roleTank rides behind it.
func armorDamageWishes(h *HullProfile, avail int) []slotWish {
	if h == nil || h.Tank != TankArmor || avail < 2 {
		return nil
	}
	return []slotWish{{Band: flagLoFirst, Roles: []modRole{roleDamage, roleTank}, Count: 1}}
}

// ewarSpecialtyBonus is the lead a hull's own EWAR flavour gets in roleEwar
// scoring (moduleRoleScoreBase).
const ewarSpecialtyBonus = 1.5

// ewarSpecialtyWishes is the mid rack a hull built around an EWAR module spends on it: every mid in
// avail but keep, each asking for the hull's own family first (roleEwarSpec) with tank and cap behind
// it. That fallback is the bonus-first rule: a Celestis offered no damp is a gunboat with its shields
// up. keep is the tank held back behind the rack (one mid on a shield hull).
func ewarSpecialtyWishes(h *HullProfile, avail, keep int) []slotWish {
	if h == nil || !h.EwarSpecialist() {
		return nil
	}
	n := avail - keep
	if n < 1 {
		return nil
	}
	return []slotWish{{Band: flagMedFirst, Roles: []modRole{roleEwarSpec, roleTank, roleCap}, Count: n}}
}

// wishSlots is how many slots a run of wishes spends.
func wishSlots(ws []slotWish) int {
	n := 0
	for _, w := range ws {
		n += w.Count
	}
	return n
}

// shieldTankKeep is the mids a shield hull holds back for its tank behind an
// EWAR rack: one, so the specialty never takes the last slot the tank had. An
// armour hull tanks out of its lows and keeps none.
func shieldTankKeep(h *HullProfile, avail int) int {
	if h.Tank == TankShield && avail >= 1 {
		return 1
	}
	return 0
}

// leanedJob reports whether this job has a kite/brawl archetype: the three combat doctrines only.
// Every other job runs the single-phase path.
func leanedJob(job string) bool {
	switch job {
	case JobCombat, JobPiracy, JobTackle:
		return true
	}
	return false
}

// bandWishes splits a wish list on one band: `want` true keeps the wishes for
// that band, false keeps everything else. Order within each half is preserved,
// because the order inside a wish list is doctrine.
func bandWishes(wishes []slotWish, band int64, want bool) []slotWish {
	out := make([]slotWish, 0, len(wishes))
	for _, w := range wishes {
		if (w.Band == band) == want {
			out = append(out, w)
		}
	}
	return out
}

// warpCoreWishes is how many lows a hauler spends on not being held still. Never the whole rack, and
// none on a one-low hull: a courier with no hold is not a courier. Two is the ceiling, since a lone
// pirate points with one and the second stabiliser breaks the common robbery. It returns a number
// because the cargo wish behind it is reduced by it.
func warpCoreWishes(lo int) int {
	if lo < 2 {
		return 0
	}
	return min(lo-1, 2)
}

// oneWish is a single slot for a single role, or nothing when the band has no room left. The avail < 1
// guard matters: a wish for a slot the hull lacks is truncated by generateFitFor's band limit and
// misleads everything that reads a wish list, including the band arithmetic of later wishes.
func oneWish(band int64, role modRole, avail int) []slotWish {
	if avail < 1 {
		return nil
	}
	return []slotWish{{Band: band, Roles: []modRole{role}, Count: 1}}
}

// hardenWishes is one active hardener on whichever band this hull tanks out of (mids for shield, lows
// for armour). Its own Count for the same structural reason, and one because a second is the stacking
// curve arguing with itself; the redundancy factor reaches the same answer and this states it.
func hardenWishes(h *HullProfile, band int64, avail int) []slotWish {
	if avail < 1 {
		return nil
	}
	if (h.Tank == TankArmor) != (band == flagLoFirst) {
		return nil // not the band this hull hardens on
	}
	w := oneWish(band, roleHarden, avail)
	if len(w) == 0 {
		return nil
	}
	// roleTank rides behind the hardener: a wish naming one role leaves the slot empty when the market has
	// none of it. See docs/comment-notes-b09.md#hardener-fallback.
	w[0].Roles = append(w[0].Roles, roleTank)
	return w
}

// miningLowsAndMids is a miner's rack below the highs, shared by the barge and the other branch of
// wishList so the two cannot drift. Lows are hold; mids keep the harvesters running. Tank goes where
// this hull tanks and nowhere else, since a wrong-layer module is a slot that never fills and reads as
// a shortage. A one-mid miner spends it on tank; from two mids up it takes an engine.
func miningLowsAndMids(h *HullProfile, med, lo int) []slotWish {
	prop := propWishes(med, 2)
	var wishes []slotWish
	// Yield first (fitminingupgrade.go): a few lows are mining upgrades where the lens says an upgrade
	// beats an expander, each falling back to what the low would have been.
	var tail []modRole
	if h.Tank == TankArmor {
		tail = []modRole{roleCargo, roleTank}
	} else {
		tail = []modRole{roleCargo}
	}
	ups := miningUpgradeWishes(h, lo, tail...)
	for _, w := range ups {
		wishes = append(wishes, w)
		lo -= w.Count
	}
	if h.Tank == TankArmor {
		wishes = append(wishes,
			slotWish{Band: flagLoFirst, Roles: []modRole{roleCargo, roleTank}, Count: lo})
		wishes = append(wishes, prop...)
		wishes = append(wishes,
			slotWish{Band: flagMedFirst, Roles: []modRole{roleCap}, Count: med - len(prop)})
	} else {
		wishes = append(wishes,
			slotWish{Band: flagLoFirst, Roles: []modRole{roleCargo}, Count: lo})
		wishes = append(wishes, prop...)
		wishes = append(wishes,
			slotWish{Band: flagMedFirst, Roles: []modRole{roleTank, roleCap}, Count: med - len(prop)})
	}
	return wishes
}

// slotWish is one band of slots and what should go in it, best role first.
type slotWish struct {
	Band  int64 // flagHiFirst | flagMedFirst | flagLoFirst
	Roles []modRole
	Count int // how many slots this wish may claim
}

// wishList is the whole design brief for one hull doing one job: the hull's tags turn into a short,
// readable shopping list with no module names. lean is how the hull intends to fight (fitreach.go);
// it is leanUnknown while the highs are placed, and only the shield combat arm reads it.
func wishList(h *HullProfile, job string, lean combatLean) []slotWish {
	hi, med, lo := h.HiSlots, h.MedSlots, h.LoSlots
	switch job {
	case JobMining:
		// A barge mines with its whole high rack and takes no gun: strip miners take no turret hardpoint.
		// See docs/comment-notes-b09.md#barge-wish.
		if h.StripCapable {
			return append([]slotWish{
				{Band: flagHiFirst, Roles: []modRole{roleMiner}, Count: hi},
			}, miningLowsAndMids(h, med, lo)...)
		}
		// Everywhere else the lasers are turrets and take turret hardpoints, so the highs are capped by
		// hardpoints however many high slots the hull has.
		lasers := min(hi, max(h.Turrets, 0))
		// One hardpoint goes to a gun whenever the hull can spare it: a toothless hull scores an engagement
		// margin of zero and hopeless() refuses every belt (threat.go). A hull with one hardpoint keeps its laser.
		guns := 0
		if lasers >= 2 {
			guns = 1
			lasers--
		}
		// One laser slot asks for a deep core miner first. A DCML I mines ordinary ore like a Miner I and is
		// the only thing that takes Mercoxit, so it costs a skilled pilot nothing. The role falls through to
		// roleMiner inside the same wish (bestForWish tries roles in order).
		var wishes []slotWish
		if lasers > 0 {
			wishes = append(wishes, slotWish{Band: flagHiFirst, Roles: []modRole{roleDeepCore, roleMiner}, Count: 1})
			lasers--
		}
		if lasers > 0 {
			wishes = append(wishes, slotWish{Band: flagHiFirst, Roles: []modRole{roleMiner}, Count: lasers})
		}
		if guns > 0 {
			wishes = append(wishes,
				slotWish{Band: flagHiFirst, Roles: []modRole{roleWeapon}, Count: guns})
		}
		return append(wishes, miningLowsAndMids(h, med, lo)...)
	case JobTrade:
		// A hauler is a hold with an engine and no guns, which would cost grid that could be cargo. The
		// engine turns an align into an escape; the stabilisers are the only defence a ship built to leave has.
		wc := warpCoreWishes(lo)
		wishes := []slotWish{{Band: flagLoFirst, Roles: []modRole{roleWarpCore}, Count: wc}}
		wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleCargo}, Count: lo - wc})
		prop := propWishes(med, 1)
		wishes = append(wishes, prop...)
		if n := min(med-len(prop), 1); n > 0 {
			wishes = append(wishes, slotWish{Band: flagMedFirst, Roles: []modRole{roleTank}, Count: n})
		}
		return wishes
	case JobFreight:
		// The trade hauler minus its stabilisers: a lifter's trip is decided by whether the kit's biggest unit
		// fits the hold. Two stabilisers leave a Hoarder at 6,620 m3 against an 8,000 m3 array; all expanders lift 9,217 m3.
		wishes := []slotWish{{Band: flagLoFirst, Roles: []modRole{roleCargo}, Count: lo}}
		prop := propWishes(med, 1)
		wishes = append(wishes, prop...)
		if n := min(med-len(prop), 1); n > 0 {
			wishes = append(wishes, slotWish{Band: flagMedFirst, Roles: []modRole{roleTank}, Count: n})
		}
		return wishes
	case JobPiracy:
		// A gunboat whose mids belong to the victim: point, then web, then the leftovers, so a two-mid frigate
		// has a scrambler and a web and no tank. A pirate wins by the hauler still being there.
		guns := h.Turrets
		if h.Weapon == WeaponMissile {
			guns = h.Launchers
		}
		guns = min(hi, max(guns, 0))
		// The mid rack is built as separate one-slot wishes, counted down as it is spent. See
		// docs/comment-notes-b09.md#piracy-mid-rack.
		prop := propWishes(med, 2)
		medLeft := med
		tackle := oneWish(flagMedFirst, roleTackle, medLeft)
		medLeft -= len(tackle) + len(prop)
		// The cargo scanner comes third, ahead of the web: the robbery is priced on it (camp.go ransomScan).
		// Not on an EWAR-specialist hull, whose jammer rack is the ship.
		specialist := h.EwarSpecialist()
		var scanner []slotWish
		if !specialist {
			scanner = oneWish(flagMedFirst, roleScanner, medLeft)
			medLeft -= len(scanner)
			if len(scanner) > 0 && medLeft < 1 {
				scanner[0].Roles = append(scanner[0].Roles, roleWeb)
			}
		}
		web := oneWish(flagMedFirst, roleWeb, medLeft)
		medLeft -= len(web)
		if specialist && medLeft-shieldTankKeep(h, medLeft) >= 3 {
			scanner = oneWish(flagMedFirst, roleScanner, medLeft)
			medLeft -= len(scanner)
		}
		// What is left after the victim's share goes to the hull's own EWAR when it was built around one:
		// a jammed victim cannot shoot back.
		ew := ewarSpecialtyWishes(h, medLeft, shieldTankKeep(h, medLeft))
		medLeft -= wishSlots(ew)

		wishes := []slotWish{{Band: flagHiFirst, Roles: []modRole{roleWeapon}, Count: guns}}
		wishes = append(wishes, tackle...)
		wishes = append(wishes, prop...)
		wishes = append(wishes, scanner...)
		wishes = append(wishes, web...)
		wishes = append(wishes, ew...)
		wishes = append(wishes,
			slotWish{Band: flagMedFirst, Roles: []modRole{roleTank, roleCap}, Count: max(medLeft, 0)})
		// Damage in the lows for a shield hull; an armour hull keeps its plating there, since its mids are
		// spoken for. The damage control leads either way: the mids go to the victim, so the lows are its only tank.
		dc := dcWishes(lo, 2)
		wishes = append(wishes, dc...)
		if h.Tank == TankShield {
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleDamage}, Count: lo - len(dc)})
		} else {
			dmg := armorDamageWishes(h, lo-len(dc))
			wishes = append(wishes, dmg...)
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleTank, roleDamage}, Count: lo - len(dc) - len(dmg)})
		}
		return wishes
	case JobTackle:
		// The piracy doctrine with the leftovers spent on making the other side worse: point, engine, web,
		// then EWAR. A tackler keeps the target on grid until the gang arrives, so its mids belong to the victim.
		guns := h.Turrets
		if h.Weapon == WeaponMissile {
			guns = h.Launchers
		}
		guns = min(hi, max(guns, 0))
		// Counted down as it is spent, exactly as the piracy arm above.
		prop := propWishes(med, 2)
		medLeft := med
		tackle := oneWish(flagMedFirst, roleTackle, medLeft)
		medLeft -= len(tackle) + len(prop)
		web := oneWish(flagMedFirst, roleWeb, medLeft)
		medLeft -= len(web)
		ew := ewarSpecialtyWishes(h, medLeft, shieldTankKeep(h, medLeft))
		medLeft -= wishSlots(ew)

		wishes := []slotWish{{Band: flagHiFirst, Roles: []modRole{roleWeapon}, Count: guns}}
		wishes = append(wishes, tackle...)
		wishes = append(wishes, prop...)
		wishes = append(wishes, web...)
		wishes = append(wishes, ew...)
		wishes = append(wishes,
			slotWish{Band: flagMedFirst, Roles: []modRole{roleEwar, roleTank, roleCap}, Count: max(medLeft, 0)})
		// Same reasoning as the pirate's, stronger: a tackler must still be on the field when the gang
		// arrives, so always-on resistance on all three layers is what the seat is for.
		dc := dcWishes(lo, 2)
		wishes = append(wishes, dc...)
		if h.Tank == TankShield {
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleDamage}, Count: lo - len(dc)})
		} else {
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleTank, roleDamage}, Count: lo - len(dc)})
		}
		return wishes
	case JobScout:
		// The gank fleet's eye and looter, and the one combat-fleet seat with no gun: a gun makes it an
		// aggressor and CONCORD would answer. Mids: scanner, engine, sensor booster. Lows: stabilisers, then cargo.
		medLeft := med
		scanner := oneWish(flagMedFirst, roleScanner, medLeft)
		medLeft -= len(scanner)
		prop := propWishes(medLeft, 1)
		medLeft -= len(prop)
		wishes := append([]slotWish{}, scanner...)
		wishes = append(wishes, prop...)
		if medLeft > 0 {
			wishes = append(wishes, slotWish{Band: flagMedFirst, Roles: []modRole{roleSensor, roleTank}, Count: medLeft})
		}
		wc := warpCoreWishes(lo)
		if wc > 0 {
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleWarpCore}, Count: wc})
		}
		if lo-wc > 0 {
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleCargo}, Count: lo - wc})
		}
		return wishes
	case JobLogi:
		// Repairers in every high, then capacitor across the mids after the engine, ahead of tank: remote reps
		// drink cap and a logi that runs dry is a spectator. The engine keeps the wing in rep range.
		prop := propWishes(med, 2)
		wishes := []slotWish{{Band: flagHiFirst, Roles: []modRole{roleRemoteRep}, Count: hi}}
		wishes = append(wishes, prop...)
		wishes = append(wishes,
			slotWish{Band: flagMedFirst, Roles: []modRole{roleCap, roleTank}, Count: med - len(prop)})
		// The lows tank whatever the hull tanks; a shield logi spends them
		// on cap where it can (batteries live in the lows in this era).
		if h.Tank == TankArmor {
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleTank, roleCap}, Count: lo})
		} else {
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleCap, roleTank}, Count: lo})
		}
		return wishes
	case JobEwar:
		// The disruption seat: the hull's guns, an engine to hold the range its electronics work at, then the
		// mids are the seat, a shield hull keeping one mid of tank. The lows are the gunboat's.
		guns := h.Turrets
		if h.Weapon == WeaponMissile {
			guns = h.Launchers
		}
		guns = min(hi, max(guns, 0))
		prop := propWishes(med, 2)
		medLeft := med - len(prop)
		tank := 0
		if h.Tank == TankShield && medLeft >= 3 {
			tank = 1
		}
		wishes := []slotWish{{Band: flagHiFirst, Roles: []modRole{roleWeapon}, Count: guns}}
		wishes = append(wishes, prop...)
		ewRoles := []modRole{roleEwar, roleCap}
		if h.EwarSpecialist() {
			// The hull's own family first, then any EWAR: this is the EWAR seat and a damp on a Blackbird still works.
			ewRoles = []modRole{roleEwarSpec, roleEwar, roleCap}
		}
		wishes = append(wishes,
			slotWish{Band: flagMedFirst, Roles: ewRoles, Count: max(medLeft-tank, 0)})
		if tank > 0 {
			wishes = append(wishes, slotWish{Band: flagMedFirst, Roles: []modRole{roleTank, roleCap}, Count: tank})
		}
		dc := dcWishes(lo, 2)
		wishes = append(wishes, dc...)
		if h.Tank == TankShield {
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleDamage}, Count: lo - len(dc)})
		} else {
			wishes = append(wishes, slotWish{Band: flagLoFirst, Roles: []modRole{roleTank, roleDamage}, Count: lo - len(dc)})
		}
		return wishes
	default:
		// The gunboat, the job nearly every combat pilot flies. EWAR queues behind tank on a shield hull and
		// ahead of cap on an armour hull, whose mids are spare. roleEwar is here only because the duel prices it.
		guns := h.Turrets
		if h.Weapon == WeaponMissile {
			guns = h.Launchers
		}
		guns = min(hi, max(guns, 0))
		wishes := []slotWish{{Band: flagHiFirst, Roles: []modRole{roleWeapon}, Count: guns}}
		// The damage control goes first in the lows on both tank flavours: it is the cheapest effective tank
		// and the only module that covers all three layers.
		dc := dcWishes(lo, 2)
		wishes = append(wishes, dc...)
		if h.Tank == TankShield {
			// A shield hull tanks out of the rack the engine wants, so a one-mid hull gives that mid to tank, unless
			// it is kiting: then the engine is the first mid and tank keeps the head of the rest (fitreach_test.go).
			minMed := 2
			if lean == leanKiter {
				minMed = 1
			}
			prop := propWishes(med, minMed)
			wishes = append(wishes, prop...)
			// A hull built around an EWAR module spends its mids on it, ahead of the hardener, keeping one for
			// tank (ewarSpecialtyWishes); tank sits behind it in the wish so a market with no jammers still fills.
			avail := med - len(prop)
			ew := ewarSpecialtyWishes(h, avail, shieldTankKeep(h, avail))
			wishes = append(wishes, ew...)
			hard := hardenWishes(h, flagMedFirst, avail-wishSlots(ew))
			wishes = append(wishes, hard...)
			wishes = append(wishes,
				slotWish{Band: flagMedFirst, Roles: []modRole{roleTank, roleEwar, roleCap}, Count: avail - wishSlots(ew) - len(hard)},
				slotWish{Band: flagLoFirst, Roles: []modRole{roleDamage}, Count: lo - len(dc)})
		} else {
			// An armour hull's mids are spare because its tank is in the lows, so even a single mid is the engine's.
			prop := propWishes(med, 1)
			// The damage mod is counted before the hardener so a three-low
			// hull keeps both: damage control, hardener, damage mod.
			dmg := armorDamageWishes(h, lo-len(dc))
			hard := hardenWishes(h, flagLoFirst, lo-len(dc)-len(dmg))
			wishes = append(wishes, hard...)
			wishes = append(wishes, dmg...)
			wishes = append(wishes,
				slotWish{Band: flagLoFirst, Roles: []modRole{roleTank, roleDamage}, Count: lo - len(dc) - len(hard) - len(dmg)})
			wishes = append(wishes, prop...)
			ew := ewarSpecialtyWishes(h, med-len(prop), 0)
			wishes = append(wishes, ew...)
			wishes = append(wishes,
				slotWish{Band: flagMedFirst, Roles: []modRole{roleEwar, roleCap}, Count: med - len(prop) - wishSlots(ew)})
		}
		return wishes
	}
}

// ── scoring one module for one role ─────────────────────────────────────────

// moduleRoleScore is how good this module is at this role on this ship as it stands, in the role's
// units; zero means it does not serve the role. These are proxies that order candidates within a
// role; the stat engine scores the assembled ship (fitscore.go). fs is what is already bolted on, so
// redundancy discounts more of the same and synergy pays for the complement (fitsynergy.go).
func moduleRoleScore(mm *marketModule, role modRole, h *HullProfile, fs *fitState) float64 {
	raw := moduleRoleScoreRaw(mm, role, h, fs)
	if raw <= 0 {
		return 0
	}
	return raw * fitAdjust(fs, mm, h, role)
}

// moduleRoleScoreRaw is what the module is worth to this hull before the half-built ship gets an
// opinion: its own numbers (moduleRoleScoreBase) bent by hull affinity and size. They are kept apart
// because bestForWish needs the unbent number: the shortage signal is about what is for sale, and a
// gun that scored low only as the wrong empire's is still on a shelf.
func moduleRoleScoreRaw(mm *marketModule, role modRole, h *HullProfile, fs *fitState) float64 {
	base := moduleRoleScoreBase(mm, role, h, fs)
	if base <= 0 {
		return 0
	}
	br := baseRole(role)
	return base * hullAffinity(h, br, mm.GroupID) * doctrineSizeFactor(mm, br, h)
}

func moduleRoleScoreBase(mm *marketModule, role modRole, h *HullProfile, fs *fitState) float64 {
	base := 1 + float64(moduleGradeLevel(mm))/10
	// A styled role scores exactly as its plain role (fitstyle.go), except the disruptor's, where range leads.
	if role != roleTackleLong {
		role = baseRole(role)
	}
	switch role {
	case roleSpeedMod:
		return speedModScore(mm)
	case roleTracking:
		// Optimal range bought, in percent, and only for a ship with turrets to put it on.
		if fs == nil || fs.turrets == 0 {
			return 0
		}
		if v := mm.Attrs[attrTrackingRangeMult]; v > 1 {
			return (v - 1) * 100
		}
		return 0
	case roleTackleLong:
		// Reach first, then points: a kiter's point has to land from where the kiter stands, and a two-point
		// scrambler at 7.5 km is a module it never gets to switch on.
		str := mm.Attrs[attrWarpScrambleStr]
		if str <= 0 {
			return 0
		}
		return mm.Attrs[attrTackleRange]/1000 + str
	case roleMiner, roleDeepCore:
		// Yield per cycle over cycle time, m3 a minute. Cycle time is attribute 73 for a miner, not 51 (nothing
		// that mines carries 51), so yields of modules whose cycles differ 10x are never compared per cycle.
		amount, cycle := mm.Attrs[attrMiningAmount], minerCycleMS(mm.Attrs)
		if amount <= 0 {
			return 0
		}
		if cycle <= 0 {
			return amount * base
		}
		return amount / (cycle / 60000)
	case roleWeapon:
		mult, cycle := mm.Attrs[attrDamageMultTurret], mm.Attrs[attrRateOfFire]
		// There is no capacitor term here: scaling a gun by capacitor re-armed Amarr hulls with projectiles.
		// See docs/comment-notes-b09.md#weapon-cap-term.
		if cycle <= 0 {
			return base
		}
		// Real DPS with the round it will carry, so a turret (multiplier) and a launcher (the warhead carries
		// it) share one unit: damage x max(mult, 1) over the cycle, times a reach term (fitreach.go).
		reach := reachWorth(weaponReachM(mm))
		if mm.ChargeDmg > 0 {
			if mult <= 0 {
				mult = 1
			}
			return mm.ChargeDmg * mult / (cycle / 1000) * reach
		}
		// No charge could be read for this weapon: keep the multiplier-only shape rather than answer zero,
		// which would refuse the slot outright.
		if mult > 0 {
			return mult / (cycle / 1000) * reach
		}
		return (1000 / cycle) * base * reach
	case roleCargo:
		// Strictly greater than one: group 78 is four module families sharing one group, each with attribute
		// 149, and an Overdrive Injector's 0.85 shrinks the hold. roleCargo never yields, so it would take every low.
		if v := mm.Attrs[attrCargoMult]; v > 1 {
			return v * base
		}
		return 0
	case roleDamageControl:
		return damageControlScore(mm)
	case roleSensor:
		// The shares of scan resolution and lock range it adds, as a small fraction: under a first damage
		// mod's ~1.1 and far under a tank's hundreds, so it only breaks ties for a slot other roles left.
		sr, lr := mm.Attrs[attrScanResMult], mm.Attrs[attrLockRangeMult]
		gain := 0.0
		if sr > 1 {
			gain += sr - 1
		}
		if lr > 1 {
			gain += lr - 1
		}
		return gain
	case roleMiningUpgrade:
		// Attribute 434 only: the one the server folds into yield (db.GetMiningYieldBonus). The meta variants'
		// 207 mines nothing extra, so it scores zero. With no lasers placed an upgrade multiplies nothing.
		if fs != nil && fs.miners == 0 {
			return 0
		}
		if v := mm.Attrs[attrMiningAmountBonus]; v > 0 {
			return v
		}
		return 0
	case roleScanner:
		// Reach, in kilometres: the further a scout reads a hold from, the sooner a strike is called. A
		// scanner with no range attribute is refused.
		if r := mm.Attrs[attrCargoScanRange]; r > 0 {
			return r / 1000 * base
		}
		return 0
	case roleWarpCore:
		// The negation of attribute 105, and a hard refusal at zero: group 315 has no metaLevel, so the raw
		// strength is the score, and the four zero-strength base stabilisers must not tie and be bought.
		str := -mm.Attrs[attrWarpScrambleStr]
		if str <= 0 {
			return 0
		}
		return str * 100
	case roleHarden:
		// Hardeners carry 984-987 and nothing else, so the resist reading is
		// already the right shape and needs no arm of its own.
		return resistScore(mm)
	case roleTank:
		// A Shield Boost Amplifier is worth the hitpoints its percentage adds to the fitted booster over a
		// fight, the unit an extender is scored in; nothing without a booster or on an armour hull (fitlocalrep.go).
		if mm.GroupID == grpShieldBoostAmp {
			if h.Tank == TankArmor {
				return 0
			}
			return sbaTankScore(mm.Attrs[attrShieldBoostPct], fs)
		}
		if h.Tank == TankArmor {
			// Buffer and active repair in one unit: a plate's headline is flat hitpoints and a repairer's is per
			// cycle, so the plate is divided down to the repair it is worth over a fight (armorRepWorthCycles).
			if v := mm.Attrs[attrArmorPlateHPAdd]; v > 0 {
				return v / armorRepWorthCycles * base
			}
			// A repairer's pulse is attribute 84, the one the stat engine reads. Attribute 65 is on no repairer,
			// so scoring on it would give every repairer the resist score and lose every low slot.
			if v := mm.Attrs[attrArmorRepAmount]; v > 0 {
				return v * base
			}
		} else {
			if v := mm.Attrs[attrShieldBonus]; v > 0 {
				return v * base
			}
			if v := mm.Attrs[attrCapacityBonus]; v > 0 {
				return v * base
			}
		}
		// Resist plating and amplifiers score on the resistance they bring. What the ship already resists is
		// a redundancy factor (resistCovered), so the role stands down instead of buying one coating all rack.
		return resistScore(mm)
	case roleDamage:
		// A damage mod multiplies guns. With no guns on the ship it multiplies nothing and the slot is worth
		// more as tank.
		if fs.weapons == 0 {
			return 0
		}
		if v := mm.Attrs[attrDamageMultTurret]; v > 1 {
			return v * base
		}
		if v := mm.Attrs[attrMissileDmgMult]; v > 1 {
			return v * base
		}
		return 0
	case roleSpeed:
		// Propulsion: attribute 20 positive, not attribute 80 (see fitGenScoreAttrs).
		v := mm.Attrs[attrSpeedFactor]
		if v <= 0 {
			return 0
		}
		// An afterburner is scored on its speed alone: it is pulsed, not a permanent drain, and discounting
		// it by capFeasibility squared cut it to about 10 on every frigate while the Civilian one kept its 30.
		if !isMicrowarpdrive(mm) {
			return v
		}
		// A microwarpdrive keeps the squared capacitor discount, the choice that discount is for: a 1MN MWD
		// draws 4.5 GJ/s against an afterburner's 2.0. A cap-rich cruiser still comes out with the MWD.
		f := capFeasibility(mm, h, fs)
		return v * f * f
	case roleCap:
		// What the module does to the capacitor, not a flat meta constant, so a recharger and a battery score
		// on their own numbers.
		if v := mm.Attrs[attrCapRechMult]; v > 0 && v < 1 {
			return (1/v - 1) * 100 // 0.835 gives +19.8% recharge rate
		}
		if v := mm.Attrs[attrCapacitorBonusFlat]; v > 0 && h.CapGJ > 0 {
			return v / h.CapGJ * 100 // flat GJ as a % of the hull's own cap
		}
		return base
	case roleTackle:
		// Points first, then reach: a pilot's warp core is what is argued with, so strength is the headline;
		// between equals the longer reach wins, since the engagement happens inside it.
		str := mm.Attrs[attrWarpScrambleStr]
		if str <= 0 {
			return 0
		}
		return str*100 + mm.Attrs[attrTackleRange]/1000
	case roleWeb:
		// How hard it bites, then how far it reaches.
		pct := -mm.Attrs[attrSpeedFactor]
		if pct <= 0 {
			return 0
		}
		return pct + mm.Attrs[attrTackleRange]/1000
	case roleRemoteRep:
		// HP a second onto another ship, then reach. The module's group decides which attribute pair is read,
		// not the hull's tank, so an off-flavour repairer still scores; hullAffinity discounts it.
		amount, reach := mm.Attrs[attrShieldBonus], mm.Attrs[attrShieldTransferRange]
		if mm.GroupID == grpArmorRepProjector {
			amount, reach = mm.Attrs[attrArmorRepAmount], mm.Attrs[attrTackleRange]
		}
		if amount <= 0 {
			return 0
		}
		cycle := mm.Attrs[attrDuration]
		if cycle <= 0 {
			return amount * base
		}
		return amount/(cycle/1000) + reach/1000
	case roleEwar:
		// No single attribute compares a painter to a damp to a jammer, so the meta ladder (moduleGradeLevel,
		// not attribute 633) orders within a group. A hull built around one flavour gets ewarSpecialtyBonus.
		if h != nil && h.HasSpecialty(ewarSpecialtyByGroup[mm.GroupID]) {
			return base * ewarSpecialtyBonus
		}
		return base
	}
	return 0
}

// fillWeaponChargeDamage reads, for every weapon in the catalogue, the damage of the hardest-hitting
// round it can load: the round pickCharge (fitammo.go) would pick. Skills are not applied; chargeFor
// takes the per-pilot answer later. Behind process-wide caches. A failure leaves ChargeDmg at 0,
// which moduleRoleScoreBase reads as fall back, not as no damage.
func (m *Mod) fillWeaponChargeDamage(ctx context.Context, list []*marketModule) {
	for _, mm := range list {
		if !mm.Turret && !mm.Launcher {
			continue
		}
		opts, err := m.chargeOptions(ctx, mm.TypeID)
		if err != nil || len(opts) == 0 {
			continue
		}
		ids := make([]int64, 0, len(opts))
		for _, o := range opts {
			ids = append(ids, o.ID)
		}
		attrs, err := evedb.TypeAttrsMany(ctx, m.db, ids)
		if err != nil {
			continue
		}
		for _, id := range ids {
			a := attrs[id]
			d := a[attrEmDamage] + a[attrExplosiveDamage] +
				a[attrKineticDamage] + a[attrThermalDamage]
			if d > mm.ChargeDmg {
				mm.ChargeDmg = d
				// The reach of that round rather than the longest-flying one available, since it is the round the
				// fit will carry (fitammo.go ranks by damage).
				mm.ChargeReachM = a[attrMaxVelocity] * a[attrExplosionDelay] / 1000
			}
		}
	}
}

// isMiningModule reports whether a group is one of the three that mine. A mining laser wears
// effTurretFitted as a gun does, so anything reasoning about turrets must subtract miners: it is a
// turret that takes no ammunition and does no damage.
func isMiningModule(groupID int64) bool {
	switch groupID {
	case grpMiningLaser, grpStripMiner, grpDeepCoreMiner:
		return true
	}
	return false
}

// isStripClassMiner reports whether a mining module is a strip miner or ice harvester (barge
// equipment) rather than a laser any hull can mount. effTurretFitted decides, not the group or name:
// group 483 holds both a Strip Miner II and a Deep Core Miner II, and names a word apart land on
// opposite sides. Lasers are turret-mounted and strip miners are not, so barges have no turret hardpoints.
func isStripClassMiner(mm *marketModule) bool {
	return isMiningModule(mm.GroupID) && !mm.Turret
}

// serves reports whether a module can be considered for a role on a hull. It is a ceiling, not a
// preference: hullAffinity (hulldoctrine.go) decides only when it reaches zero. The one absolute is
// size, upward only: a gun bigger than the hull is refused, a smaller one is left to score badly, so a
// cruiser whose station sells no medium guns fits smalls.
func serves(mm *marketModule, role modRole, h *HullProfile, band int64) bool {
	if mm.Slot != band {
		return false
	}
	// A styled role is its plain role behind a tag filter (fitstyle.go).
	if !styleAdmits(mm, role, h) {
		return false
	}
	role = baseRole(role)
	// Zero affinity is a hard gate: not this hull's family at all, or a launcher band above its size.
	if hullAffinity(h, role, mm.GroupID) <= 0 {
		return false
	}
	// A strip-class harvester off a hull that cannot carry one is an absolute refusal, like the
	// oversized turret below: its grid use is low, so the grid check would wave it onto a frigate.
	if (role == roleMiner || role == roleDeepCore) && isStripClassMiner(mm) && !h.StripCapable {
		return false
	}
	if role == roleDeepCore && minerKindOf(mm.Attrs) != minerDeepCore {
		return false
	}
	// An oversized turret. A Medium Beam Laser I is chargeSize 1 and belongs on a frigate, so the name
	// is never asked. Launchers carry their size in the group and are refused by launcherSizeFactor.
	if mm.Turret && mm.ChargeSize > 0 && h.WeaponSize > 0 && mm.ChargeSize > h.WeaponSize {
		return false
	}
	// A propulsion module bigger than the hull: a 5MN Afterburner I carries the 1MN mass addition and is
	// a frigate module. Attribute 796 is the authority, not the name; an unclassifiable engine stays refused.
	if role == roleSpeed {
		if want := hullPropClass(h.Class); want != 0 {
			if got := propMassClass(mm); got == 0 || got > want {
				return false
			}
		}
	}
	return true
}

// ── generation ──────────────────────────────────────────────────────────────

// wishHasRole reports whether a wish asks for this role, styled or plain.
func wishHasRole(w slotWish, role modRole) bool {
	for _, r := range w.Roles {
		if baseRole(r) == role {
			return true
		}
	}
	return false
}

// engineGridHold is the CPU and powergrid to hold back for a hull-sized engine while the wishes
// before it are placed: the cheapest engine of the hull's size this pilot can use, on a cruiser-sized
// or bigger hull whose wish list asks for one. Zero when there is nothing to hold for.
func (m *Mod) engineGridHold(ctx context.Context, h *HullProfile, job string, skills map[int64]int,
	cat []*marketModule, intent engageStyle, cpuCap, pgCap float64) (float64, float64) {
	want := hullPropClass(h.Class)
	if want < 2 {
		return 0, 0
	}
	asks := false
	for _, w := range wishListFor(h, job, leanUnknown, intent) {
		if wishHasRole(w, roleSpeed) {
			asks = true
			break
		}
	}
	if !asks {
		return 0, 0
	}
	var bestCPU, bestPG float64
	found := false
	for _, mm := range cat {
		if propMassClass(mm) != want || !serves(mm, roleSpeed, h, flagMedFirst) {
			continue
		}
		if mm.CPU > cpuCap || mm.PG > pgCap {
			continue
		}
		if found && mm.PG >= bestPG {
			continue
		}
		if ok, err := m.pilotCanUse(ctx, mm.TypeID, skills); err != nil || !ok {
			continue
		}
		bestCPU, bestPG, found = mm.CPU, mm.PG, true
	}
	return bestCPU, bestPG
}

// generateFitForOnce is one pass of generateFitFor (fitgrid.go): the best fit this pilot could
// assemble on one hull for one job, or nil when the hull is out of reach or nothing usable fits.
// own is what the pilot already wears (it only breaks near-ties, fitsticky.go); taste is its
// preference (the zero value is the identity); reserve is the second pass's reserved grid module.
func (m *Mod) generateFitForOnce(ctx context.Context, charID int64, job string, h *HullProfile,
	skills map[int64]int, cat []*marketModule, dcat []*marketDrone, own *ownedLoadout,
	reach map[int64]bool, taste fitTaste, reserve *marketModule) (*fitTemplate, []wantGap, gridRefusal, error) {

	// The hull is the one hard gate: a ship you cannot board is somebody else's ship, not a degraded fit.
	if ok, err := m.pilotCanUse(ctx, h.TypeID, skills); err != nil || !ok {
		return nil, nil, gridRefusal{}, err
	}
	var gaps []wantGap

	cpuCap := h.CPU * (1 + fittingSkillBonusPerLevel*float64(skills[skillElectronics]))
	pgCap := h.PG * (1 + fittingSkillBonusPerLevel*float64(skills[skillEngineering]))

	fit := &Fit{
		HullTypeID: h.TypeID, HullName: h.Name,
		RepairTrigger: "structure",
	}
	if h.Tank == TankArmor {
		fit.RepairTrigger = "armor"
	}

	usedCPU, usedPG := 0.0, 0.0
	fs := newFitState()
	// A capped type (marketModule.MaxCopies) leaves the catalogue once the fit has planned every copy
	// the pilot owns.
	planned := map[int64]int64{}
	spend := func(mm *marketModule) {
		if mm == nil || mm.MaxCopies <= 0 {
			return
		}
		planned[mm.TypeID]++
		if planned[mm.TypeID] >= mm.MaxCopies {
			cat = dropExhausted(cat, mm.TypeID)
		}
	}
	nextFlag := map[int64]int64{flagHiFirst: flagHiFirst, flagMedFirst: flagMedFirst, flagLoFirst: flagLoFirst}
	bandLimit := map[int64]int64{
		flagHiFirst:  flagHiFirst + int64(h.HiSlots),
		flagMedFirst: flagMedFirst + int64(h.MedSlots),
		flagLoFirst:  flagLoFirst + int64(h.LoSlots),
	}

	// The second pass's reserved low (fitgrid.go): the fitting-capacity module goes on first and its
	// grid is credited before anything is placed. Skipped on a hull with no low to give.
	gridCreditFit := identityGrid()
	if reserve != nil && nextFlag[flagLoFirst] < bandLimit[flagLoFirst] &&
		reserve.CPU <= cpuCap && reserve.PG <= pgCap {
		if c, ok := moduleGridCredit(reserve.Attrs); ok {
			gridCreditFit = c
			cpuCap, pgCap = c.apply(cpuCap, pgCap)
			fit.Modules = append(fit.Modules, FitModule{
				TypeID: reserve.TypeID, TypeName: reserve.Name,
				Flag: nextFlag[flagLoFirst], CPU: reserve.CPU, PG: reserve.PG,
			})
			own.claim(flagLoFirst, reserve.TypeID)
			spend(reserve)
			usedCPU += reserve.CPU
			usedPG += reserve.PG
			nextFlag[flagLoFirst]++
			fs.add(reserve, roleGridMod)
		}
	}

	// The engine's grid is held back until the engine wish is placed, or heavy launchers take the grid a
	// cruiser's 10MN engine needs. Frigate-sized hulls are left alone: a 1MN engine is never the squeeze.
	holdCPU, holdPG := m.engineGridHold(ctx, h, job, skills, cat, taste.style, cpuCap, pgCap)

	// Two phases for the combat jobs, because the archetype needs the guns on first (fitreach.go): the
	// highs go on under leanUnknown, then the hull is classified from the mounted guns and the rest placed.
	place := func(wishes []slotWish) {
		for _, wish := range wishes {
			for placed := 0; placed < wish.Count; placed++ {
				if nextFlag[wish.Band] >= bandLimit[wish.Band] {
					break // the hull ran out of that band
				}
				hc, hp := holdCPU, holdPG
				if wishHasRole(wish, roleSpeed) {
					hc, hp = 0, 0 // the engine spends its own hold
				}
				pick, role, marketGap := m.bestForWish(ctx, wish, h, skills, cat, fs,
					cpuCap-usedCPU-hc, pgCap-usedPG-hp, own, taste)
				if pick == nil {
					// Only a market gap is recorded as a want (wants.go): a missing skill is the pilot's to train and a
					// grid shortfall is the hull's, and asking factories for either would build modules that already exist.
					if marketGap {
						gaps = append(gaps, wantGap{Band: wish.Band, Roles: wish.Roles, Hull: h})
					}
					break
				}
				mod := FitModule{
					TypeID: pick.TypeID, TypeName: pick.Name,
					Flag: nextFlag[wish.Band], CPU: pick.CPU, PG: pick.PG,
				}
				// Ammo, only for things that take it. Nothing else in a
				// generated fit ever triggers a restock trip.
				if pick.Turret || pick.Launcher {
					m.chargeFor(ctx, &mod, skills, own.charge(pick.TypeID), reach)
				}
				own.claim(wish.Band, pick.TypeID)
				spend(pick)
				fit.Modules = append(fit.Modules, mod)
				usedCPU += pick.CPU + miningUpgradePenaltyCPU(fs, pick)
				usedPG += pick.PG
				nextFlag[wish.Band]++
				// Record the plain role: a styled weapon is a weapon to everything that counts them.
				fs.add(pick, baseRole(role))
			}
			if wishHasRole(wish, roleSpeed) {
				holdCPU, holdPG = 0, 0 // placed or not, the hold has had its turn
			}
		}
	}

	// The pilot's intent (fitstyle.go). engageAuto is wishList's own answer with the lean read off the
	// guns; a pilot with a style decides the rest of the rack by its intent instead.
	intent := taste.style
	if styledJob(job) {
		all := wishListFor(h, job, leanUnknown, intent)
		place(bandWishes(all, flagHiFirst, true))
		lean := fitLean(h, fs)
		if intent != engageAuto {
			lean = intent.lean()
		}
		place(bandWishes(wishListFor(h, job, lean, intent), flagHiFirst, false))
	} else {
		place(wishList(h, job, leanUnknown))
	}

	holdCPU, holdPG = 0, 0

	// Backfill: fill any remaining empty slots with whatever the pilot can use. Doctrine had its say in
	// the wish pass, and an empty slot is worse than a suboptimal module.
	for _, band := range []int64{flagHiFirst, flagMedFirst, flagLoFirst} {
		for nextFlag[band] < bandLimit[band] {
			pick, role := m.bestBackfill(ctx, band, h, job, skills, cat, fs,
				cpuCap-usedCPU, pgCap-usedPG, fit, taste)
			if pick == nil {
				break // genuinely nothing left that fits
			}
			mod := FitModule{
				TypeID: pick.TypeID, TypeName: pick.Name,
				Flag: nextFlag[band], CPU: pick.CPU, PG: pick.PG,
			}
			if pick.Turret || pick.Launcher {
				m.chargeFor(ctx, &mod, skills, own.charge(pick.TypeID), reach)
			}
			own.claim(band, pick.TypeID)
			spend(pick)
			fit.Modules = append(fit.Modules, mod)
			usedCPU += pick.CPU
			usedPG += pick.PG
			nextFlag[band]++
			fs.add(pick, role)
		}
	}

	for i := range gaps {
		gaps[i].Empty = nextFlag[gaps[i].Band] < bandLimit[gaps[i].Band]
	}

	if len(fit.Modules) == 0 && job != JobTrade && job != JobFreight {
		// A hull with nothing on it is still a hold, so it is a real answer for hauling only. The gaps still
		// travel: a pilot that could not be armed at all is the shortage the want ledger should show.
		return nil, gaps, fs.gridRefused, nil
	}

	for _, mod := range fit.Modules {
		fit.CPUUsed += mod.CPU
		fit.PGUsed += mod.PG
	}
	fit.CPUCap, fit.PGCap = gridCreditFit.apply(
		h.CPU*(1+fittingSkillBonusPerLevel*5), h.PG*(1+fittingSkillBonusPerLevel*5))
	// The bay, after the slots (a drone competes with no module for grid or space) and before the ammo,
	// because regenAmmoCargo sizes the cargo hold against what the fit already carries.
	if err := m.fillDroneBay(ctx, fit, h, job, skills, dcat, own, taste); err != nil {
		return nil, gaps, fs.gridRefused, err
	}
	fit.AmmoBudget = taste.ammo
	m.regenAmmo(ctx, fit, job)

	return &fitTemplate{
		FitID: generatedFitID(h.TypeID), Name: h.Name, HullTypeID: h.TypeID,
		RepairTrigger: fit.RepairTrigger, Fit: fit,
		EffectiveFor: charID, Job: job,
		Style: intent, Lean: fitLean(h, fs),
	}, gaps, fs.gridRefused, nil
}

// bestForWish picks the highest-scoring module this pilot can use that serves one of the wish's
// roles and fits the grid left. Roles are tried in order and never score-compared, since the order
// is doctrine; a role stands down when its best fitAdjust is under roleYieldFloor but stays the
// fallback. Returns the pick, its role, and whether nothing was picked because the market had none.
func (m *Mod) bestForWish(ctx context.Context, wish slotWish, h *HullProfile,
	skills map[int64]int, cat []*marketModule, fs *fitState, cpuLeft, pgLeft float64,
	own *ownedLoadout, taste fitTaste) (*marketModule, modRole, bool) {

	anyOffered := false // something served this wish, before grid and skills
	var yielded *marketModule
	var yieldedRole modRole
	for _, role := range wish.Roles {
		var best *marketModule
		var bestScore, bestAdj, bestRaw float64
		// What the grid cost this role: the best raw score among candidates refused only for CPU or
		// powergrid, and which budget refused it. Read after the scan (fitgrid.go) to tell a slot left short
		// by the grid from one that was well served.
		var refusedRaw float64
		var refusedCPU, refusedPG bool
		// The best candidate the ship is ALREADY wearing, tracked beside the
		// best overall so the incumbent can hold its slot without ever being
		// allowed to win a comparison it lost (fitsticky.go).
		var incumbent *marketModule
		var incumbentScore, incumbentAdj float64
		for _, mm := range cat {
			if !serves(mm, role, h, wish.Band) {
				continue
			}
			// anyOffered is the market's answer, read on the base score without doctrine: a module discounted to
			// nothing because the ship already has one is no shortage; an off-family fill still leaves the want standing.
			base := moduleRoleScoreBase(mm, role, h, fs)
			if base <= 0 {
				continue
			}
			// Everything past the score works on the plain role, so a styled pick is counted, discounted and
			// priced as a plain one (fitstyle.go).
			br := baseRole(role)
			aff := hullAffinity(h, br, mm.GroupID)
			if aff >= doctrineGapFloor {
				anyOffered = true
			}
			raw := base * aff * doctrineSizeFactor(mm, br, h)
			if raw <= 0 {
				continue
			}
			// The grid is the first real gate: nothing below runs for a module this hull could not afford anyway.
			if need := mm.CPU + miningUpgradePenaltyCPU(fs, mm); need > cpuLeft || mm.PG > pgLeft {
				if raw > refusedRaw {
					refusedRaw, refusedCPU, refusedPG = raw, need > cpuLeft, mm.PG > pgLeft
				}
				continue
			}
			// Hardpoints are the other hard budget, counted per family here where what is placed is known. The
			// wish list cannot enforce them: a slot may be filled from the other family when the market has none.
			if mm.Turret && fs.turrets >= h.Turrets {
				continue
			}
			if mm.Launcher && fs.launchers >= h.Launchers {
				continue
			}
			// And never a close-range gun into a kiting battery (fitsynergy.go).
			if dragsKiteReach(fs, mm) {
				continue
			}
			// The pilot's own preference is applied only here, on score (fittaste.go). See
			// docs/comment-notes-b09.md#taste-placement.
			adj := fitAdjust(fs, mm, h, br)
			fitted := own.has(wish.Band, mm.TypeID)
			// What it costs this pilot, not for a module already bolted on: that price was paid, and a wallet
			// dipping a band must not rip out a plate the pilot owns.
			price := 1.0
			if !fitted {
				price = taste.priceFactor(mm.Ask, br)
			}
			score := raw * adj * taste.forType(mm.TypeID) * price
			// An incumbent still has to clear the same grid, skill and role
			// gates; it is only spared the comparison.
			if score <= bestScore && !(fitted && score > incumbentScore) {
				continue
			}
			ok, err := m.pilotCanUse(ctx, mm.TypeID, skills)
			if err != nil || !ok {
				continue
			}
			if fitted && score > incumbentScore {
				incumbent, incumbentScore, incumbentAdj = mm, score, adj
			}
			if score > bestScore {
				best, bestScore, bestAdj, bestRaw = mm, score, adj, raw
			}
		}
		// A module the grid refused that would have beaten what the slot got.
		fs.noteGridRefusal(refusedRaw, bestRaw, best != nil, refusedCPU, refusedPG)
		// Close enough is close enough: pulling a working module out of a slot costs a station trip, worth
		// more than the margin below.
		if incumbent != nil && incumbentScore >= fitGenStickiness*bestScore {
			best, bestAdj = incumbent, incumbentAdj
		}
		if best == nil {
			continue
		}
		if bestAdj >= roleYieldFloor {
			return best, role, false
		}
		if yielded == nil && bestAdj >= roleAbandonFloor {
			yielded, yieldedRole = best, role
		}
	}
	if yielded != nil {
		return yielded, yieldedRole, false
	}
	return nil, "", !anyOffered
}

// backfillRoles are all roles a backfill pass considers, per band, with no role filter and no yield
// floor: the wish pass placed what doctrine asked for, and an empty slot is worse than a suboptimal module.
// The remote repairer is not here: backfillRolesFor adds it to a logi fit's highs only.
var backfillRoles = map[int64][]modRole{
	flagHiFirst: {roleWeapon, roleMiner},
	flagMedFirst: {roleSpeed, roleTank, roleHarden, roleTackle, roleWeb,
		roleEwar, roleCap},
	flagLoFirst: {roleDamage, roleTank, roleHarden, roleDamageControl,
		roleCargo, roleWarpCore},
}

// backfillRolesFor is the role list one backfill pass walks for one band, job and intent. Pure. A
// kiter's empty low is a speed module first (fitstyle.go). A combat, ewar or logi fit never gets a
// cargo expander in a backfilled low: no lens for those jobs reads cargo, so it would only cost a
// slot, grid and mass. Mining, trade and loot fits keep it. Only a logi fit backfills a high with a
// remote repairer; any other fit leaves a high its guns or miners cannot take empty, because a filler
// repairer costs grid the mids and lows want, cycles on fleetmates off the guns' capacitor, and is
// lost with the hull.
func backfillRolesFor(band int64, job string, style engageStyle) []modRole {
	base := backfillRoles[band]
	if len(base) == 0 {
		return nil
	}
	roles := base
	if band == flagHiFirst && job == JobLogi {
		roles = append(append([]modRole{}, base...), roleRemoteRep)
	}
	if band == flagLoFirst && (styledJob(job) || job == JobLogi) {
		roles = make([]modRole, 0, len(base))
		for _, r := range base {
			if r != roleCargo {
				roles = append(roles, r)
			}
		}
	}
	if style == engageKite && band == flagLoFirst {
		roles = append([]modRole{roleSpeedMod}, roles...)
	}
	// Targeting modules take a combat fit's otherwise-empty slot and nothing
	// else's (roleSensor).
	if styledJob(job) && (band == flagMedFirst || band == flagLoFirst) {
		roles = append(append([]modRole{}, roles...), roleSensor)
	}
	return roles
}

// bestBackfill finds the best module the pilot can use in a band, across all
// roles. It is the backfill counterpart of bestForWish: same scoring, no role
// filter, no yield floor and no market-gap reporting. An empty slot at the end
// of generation means nothing usable exists, not that the market is short.
func (m *Mod) bestBackfill(ctx context.Context, band int64, h *HullProfile, job string,
	skills map[int64]int, cat []*marketModule, fs *fitState,
	cpuLeft, pgLeft float64, fit *Fit, taste fitTaste) (*marketModule, modRole) {

	roles := backfillRolesFor(band, job, taste.style)
	if len(roles) == 0 {
		return nil, ""
	}

	var best *marketModule
	var bestScore float64
	var bestRole modRole

	for _, mm := range cat {
		if mm.Slot != band {
			continue
		}
		if mm.CPU > cpuLeft || mm.PG > pgLeft {
			continue
		}
		if mm.Turret && fs.turrets >= h.Turrets {
			continue
		}
		if mm.Launcher && fs.launchers >= h.Launchers {
			continue
		}
		if dragsKiteReach(fs, mm) {
			continue
		}
		// One capacitor cannot run two active repairers, and the wish pass refuses the second
		// (dupActiveTankFactor). Backfill has no yield floor, so it must refuse it too.
		if activeTank(mm) && fs.byGroup[grpArmorRepairer]+fs.byGroup[grpShieldBooster] > 0 {
			continue
		}
		for _, role := range roles {
			if !serves(mm, role, h, band) {
				continue
			}
			// The kiter's legs stop at speedModMax here too: past two the stacking penalty has eaten the module.
			if role == roleSpeedMod && fs.byRole[roleSpeedMod] >= speedModMax {
				continue
			}
			base := moduleRoleScoreBase(mm, role, h, fs)
			if base <= 0 {
				continue
			}
			aff := hullAffinity(h, role, mm.GroupID)
			if aff <= 0 {
				continue
			}
			adj := fitAdjust(fs, mm, h, role)
			if adj <= 0 {
				continue
			}
			// Price only, not the taste noise: backfill is the plain argmax and fills a slot, not expresses one.
			score := base * aff * adj * taste.priceFactor(mm.Ask, role)
			if score <= bestScore {
				continue
			}
			ok, err := m.pilotCanUse(ctx, mm.TypeID, skills)
			if err != nil || !ok {
				continue
			}
			best, bestScore, bestRole = mm, score, role
		}
	}
	return best, bestRole
}

// pilotCanUse is the skill gate, with the module's exemption honoured: the Basic Miner is fittable
// by anybody (fitting.go), which keeps a new pilot's mining fit from coming out empty. A nil sheet
// means do not gate at all, unlike an empty one (a pilot who trained nothing): nil is the
// aspirational library (aspirationalFits), which must show ships the pilot cannot fly yet.
func (m *Mod) pilotCanUse(ctx context.Context, typeID int64, skills map[int64]int) (bool, error) {
	if typeID == basicMinerTypeID || skills == nil {
		return true, nil
	}
	reqs, err := m.requiredSkills(ctx, typeID)
	if err != nil {
		return false, nil
	}
	return len(evedb.UnmetSkillReqs(reqs, skills, nil)) == 0, nil
}

// Choosing what a gun is loaded with lives in fitammo.go.

// chargeAskCache holds the cheapest live ask per charge type, refreshed on the module catalogue's
// TTL. The generator fits guns on every plan pass for every pilot, and a charge's ask moves on the
// market's timescale, not the planner's.
var (
	chargeAskMu   sync.Mutex
	chargeAskAt   time.Time
	chargeAskByID map[int64]float64
)

// chargeAsks answers the cheapest live ask anywhere per charge type; a type nobody sells is absent.
// One query loads the whole charge category (8) per TTL, because per-ID misses cannot be told from
// nobody sells it.
func (m *Mod) chargeAsks(ctx context.Context) map[int64]float64 {
	chargeAskMu.Lock()
	if chargeAskByID != nil && clock.Now().Sub(chargeAskAt) < fitGenMarketTTL {
		out := chargeAskByID
		chargeAskMu.Unlock()
		return out
	}
	chargeAskMu.Unlock()

	byID := map[int64]float64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, MIN(o.price)
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 8
		JOIN market_orders o ON o.typeID = t.typeID
		     AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		WHERE t.published = 1
		GROUP BY t.typeID`)
	if err != nil {
		return byID // an unreadable market reads as "nobody sells anything"
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p float64
		if rows.Scan(&id, &p) == nil {
			byID[id] = p
		}
	}
	chargeAskMu.Lock()
	chargeAskByID, chargeAskAt = byID, clock.Now()
	chargeAskMu.Unlock()
	return byID
}

// chargeRegionAskCache is chargeAsks broken out per region, for the tier that asks whether anybody
// the pilot can shop from sells the round. Same TTL: the market moves on its own timescale.
var (
	chargeRegionAskMu   sync.Mutex
	chargeRegionAskAt   time.Time
	chargeRegionAskByID map[int64]map[int64]float64 // typeID to regionID to cheapest ask
)

// chargeRegionAsks answers the cheapest live ask per charge type per region; a (type, region) pair
// nobody sells in is absent. One query per TTL, like chargeAsks. A read failure answers empty, which
// drops every round to the sold-somewhere tier and promotes nothing.
func (m *Mod) chargeRegionAsks(ctx context.Context) map[int64]map[int64]float64 {
	chargeRegionAskMu.Lock()
	if chargeRegionAskByID != nil && clock.Now().Sub(chargeRegionAskAt) < fitGenMarketTTL {
		out := chargeRegionAskByID
		chargeRegionAskMu.Unlock()
		return out
	}
	chargeRegionAskMu.Unlock()

	byID := map[int64]map[int64]float64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, s.regionID, MIN(o.price)
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 8
		JOIN market_orders o ON o.typeID = t.typeID
		     AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		JOIN mapSolarSystems s ON s.solarSystemID = o.solarSystemID
		WHERE t.published = 1
		GROUP BY t.typeID, s.regionID`)
	if err != nil {
		return byID
	}
	defer rows.Close()
	for rows.Next() {
		var id, region int64
		var p float64
		if rows.Scan(&id, &region, &p) == nil {
			byRegion := byID[id]
			if byRegion == nil {
				byRegion = map[int64]float64{}
				byID[id] = byRegion
			}
			byRegion[region] = p
		}
	}
	chargeRegionAskMu.Lock()
	chargeRegionAskByID, chargeRegionAskAt = byID, clock.Now()
	chargeRegionAskMu.Unlock()
	return byID
}

// chargeShopScope is the set of regions this pilot's ammo shopping can reach, or nil for unbounded.
// It mirrors the buy cap: under the home loop a trip is capped at the adjacent band (shopClimb), so
// a charge sold only beyond it is, for this pilot, one nobody sells. Anchored to home, not to where
// the pilot stands: a roaming scope would change the fit's mind every few jumps.
func (m *Mod) chargeShopScope(ctx context.Context, charID int64) map[int64]bool {
	if !HomeLoop() || !Residency() {
		return nil
	}
	home := m.homeRegionByChar(ctx, charID)
	if home == 0 {
		return nil // unknown home is not a reason to fence a pilot in
	}
	return m.scopeRegions(ctx, home, frontBuyCap(charID, bandAdjacent))
}

// ── the candidate list ──────────────────────────────────────────────────────

// generatedCandidate is one generated fit and why its hull was considered.
type generatedCandidate struct {
	Template *fitTemplate
	Hull     *HullProfile
	// HullRank is the pre-scoring pull toward this hull: what it is for, whose empire built it, whether
	// the pilot owns one and whether anybody sells one. It orders the shortlist that gets simulated;
	// the simulation decides the winner.
	HullRank float64
	Buyable  bool
	// Owned is whether the pilot already has this hull. A candidate that is
	// neither owned nor Buyable cannot be flown by buying it, and
	// candidatePool leaves it out of selection.
	Owned bool
	// Gaps are the slots that stayed empty because nothing serving them is
	// for sale anywhere. They are the raw material of a want.
	Gaps []wantGap
}

// generateFits builds a shortlist of fits this pilot could fly for this job, best hulls first. It
// does not generate a fit for every hull: each costs a market scan and a stat simulation, and this
// runs every plan pass for every bot. Hulls are ranked cheaply (role weight, race, ownership,
// availability) and only the top few are assembled.
func (m *Mod) generateFits(ctx context.Context, charID int64, job string, limit int) ([]*generatedCandidate, error) {
	// An outlaw shops only where it may go, and rehabilitates in a plain rack
	// (outlawhaven.go). Every other pilot's ctx comes back untouched.
	ctx = m.outlawCatalogueCtx(ctx, charID, job)
	_, hulls, err := m.loadHullProfiles(ctx)
	if err != nil {
		return nil, err
	}
	skills, err := m.charSkillsFor(ctx, charID)
	if err != nil {
		return nil, fmt.Errorf("skills: %w", err)
	}
	who := m.charPilotRace(ctx, charID)
	owned, err := m.ownedHullTypes(ctx, charID)
	if err != nil {
		return nil, err
	}
	// One reach scope per pass, not per hull: every hull shops from the same home, and the hull ranking
	// and the module catalogue both read through it.
	reach := m.chargeShopScope(ctx, charID)
	sold, err := m.hullsSoldWithin(ctx, reach)
	if err != nil {
		return nil, err
	}
	// ...and only at stations it will buy from. A pilot's despised factions are its employer's
	// (outlook.go) and procurement refuses their stations, so a hull only they sell is not for sale to it.
	var despised []int64
	if len(reach) > 0 {
		if outlook, oerr := m.outlookFor(ctx, charID); oerr == nil {
			despised = despisedFactionIDs(outlook)
		}
		if len(despised) > 0 {
			if s2, serr := m.typesSoldInScopeFor(ctx, reach, catShip, despised); serr == nil {
				sold = s2
			}
		}
	}
	// One taste per pass beside the reach: it is a fact about the pilot, not the hull, so drawing it
	// per hull would only repeat the arithmetic.
	taste := m.fitTasteFor(ctx, charID)
	// And how it means to fight (fitstyle.go), drawn off the same seed so it
	// moves only when the epoch does.
	taste.style = m.pilotStyle(ctx, charID, taste.seed)
	// A fleet seat outranks the pilot's own intent and cuts the ladder to the seat's hull band
	// (fleetdoctrine_seat.go), so the fit a seated pilot buys in the muster is the one the line asked
	// for. Only for the job the seat governs.
	var seat seatSpec
	if s, ok := m.seatSpecFor(ctx, charID); ok && seatMatchesJob(s, job) {
		seat = s
		// A hauler's seat is a hull band and nothing else: it has no range
		// band to impose on a cargo fit.
		if !isHaulerRole(s.Role) {
			taste.style = seatIntent(s.Style)
		}
	}
	// And its buying power, bucketed (fitBudgetFor) and read off spendable, since procurement pays out
	// of spendable (broke.go). A failed read stays price-blind: ignorance is not poverty.
	if bal, berr := evedb.GetCharBalance(ctx, m.db, charID); berr == nil {
		sp := spendable(bal)
		taste.budget = fitBudgetFor(sp)
		taste.ammo = fitAmmoBudgetFor(sp)
	}

	var shortlist []rankedHull
	for _, h := range hulls {
		w := h.RoleWeight(job)
		if w <= 0 {
			continue
		}
		// Fighting starts at a real frigate: a rookie hull with its free gun scores above zero at combat
		// but only feeds commander spawns. A pilot that cannot afford a fighting hull earns its way to one.
		if h.Class == ClassRookie && (job == JobCombat || job == JobPiracy ||
			job == JobTackle || job == JobLogi || job == JobEwar || job == JobScout) {
			// The gang seats gate with the combat lenses: each parks in the same exposed postures.
			continue
		}
		if ok, _ := m.pilotCanUse(ctx, h.TypeID, skills); !ok {
			continue // never want a cruiser it cannot fly
		}
		rank := w * fitGenRaceMul(who, h.RaceID)
		if owned[h.TypeID] {
			rank *= fitGenOwnedBonus
		}
		if !sold[h.TypeID] && !owned[h.TypeID] {
			rank *= fitGenUnsoldPenalty
		}
		shortlist = append(shortlist, rankedHull{h, rank})
	}
	// The seat's band, when it holds anything: a line of cruisers is not served by a pilot in a frigate.
	// An empty band falls back to the whole ladder, and gangSeatReady judges that at the muster.
	if seat.HullMax != 0 {
		inBand := shortlist[:0:0]
		for _, r := range shortlist {
			if hullInBand(r.h.Class, seat) {
				inBand = append(inBand, r)
			}
		}
		if len(inBand) > 0 {
			shortlist = inBand
		}
	}
	sort.SliceStable(shortlist, func(i, j int) bool { return shortlist[i].rank > shortlist[j].rank })
	// The ladder, not the top of one rung: rank carries no hull size, so without spreadByClass every
	// place would go to a frigate. See docs/comment-notes-b09.md#hull-ladder.
	shortlist = spreadByClass(shortlist, limit)

	cat, err := m.buyableModulesWithinFor(ctx, reach, despised)
	if err != nil {
		return nil, err
	}
	cat = rehabModules(ctx, cat)
	// And whatever the pilot keeps in a hangar it shops in, priced as sunk (ownstock.go), so a module
	// in its own hangar is not bought again.
	if ownedStock := m.ownModulesInReach(ctx, charID, reach); len(ownedStock) > 0 {
		if every, eerr := m.allModulesByID(ctx); eerr == nil {
			cat = withOwnStock(cat, every, ownedStock)
		}
	}
	// The drone catalogue rides the same scope as the modules. A failure here is not fatal: a hull
	// with an empty bay is a worse fit, but grounding the pilot over it would be worse.
	dcat, err := m.buyableDronesWithin(ctx, reach)
	if err != nil {
		rlogf("rolebots: drone catalogue: %v", err)
		dcat = nil
	}
	var out []*generatedCandidate
	for _, r := range shortlist {
		// Only a hull the pilot owns can have an incumbent loadout, and reading one costs a query, so ask
		// only for hulls ownedHullTypes reported.
		var own *ownedLoadout
		if owned[r.h.TypeID] {
			if own, err = m.ownedFitLoadout(ctx, charID, r.h.TypeID); err != nil {
				return nil, err
			}
		}
		// What is already bolted on is always a candidate for its own slot: the catalogue lists only modules
		// with a live ask in reach, so an unlisted incumbent would lose its slot (fitsticky.go decides).
		hullCat := cat
		if own != nil {
			if every, eerr := m.allModulesByID(ctx); eerr == nil {
				hullCat = catalogueWithIncumbents(cat, every, own)
			}
		}
		// Assembled on the seat's tank layer when the pilot holds a seat on one (hullForSeat returns a
		// copy; the shared profile is never written).
		t, gaps, err := m.generateFitFor(ctx, charID, job, hullForSeat(r.h, seat), skills, hullCat, dcat, own, reach, taste)
		if err != nil {
			return nil, err
		}
		if t == nil && len(gaps) == 0 {
			continue
		}
		out = append(out, &generatedCandidate{
			Template: t, Hull: r.h, HullRank: r.rank, Buyable: sold[r.h.TypeID],
			Owned: owned[r.h.TypeID], Gaps: gaps})
	}
	// Only the pilot's first choice reports its holes, once per plan: every shortlisted hull has the
	// same holes in the same market. What the roster wants is counted by the census (wants.go).
	if len(out) > 0 {
		m.logFitGaps(ctx, charID, out[0])
	}
	out2 := out[:0]
	for _, c := range out {
		if c.Template != nil {
			out2 = append(out2, c)
		}
	}
	return out2, nil
}

// fitGenShortlist is how many hulls are fully assembled per plan. Ranking is cheap and assembly is
// not: each candidate walks the module catalogue once per slot and is simulated by the stat engine.
const fitGenShortlist = 6

// fitGenAspirationalShortlist is how far up the ladder the skill planner is shown per job: wider
// than the flying shortlist, to see past what the pilot can fly today, but not the whole catalogue.
const fitGenAspirationalShortlist = 8

// candidatePool is every fit this pilot could be flying for this job, built for the asker each
// time out of what the hull is, what the pilot can fly and what the market has. There is no
// authored pool.
func (m *Mod) candidatePool(ctx context.Context, charID int64, job string) ([]*fitTemplate, error) {
	gen, err := m.generateFits(ctx, charID, job, fitGenShortlist)
	if err != nil {
		return nil, err
	}
	pool := make([]*fitTemplate, 0, len(gen))
	for _, c := range gen {
		// A hull it neither owns nor can buy inside its fence is not a fit it can fly: choosing one loops
		// through pick, shop, abandon. It still ranks in generateFits and reports its gaps.
		if !c.Buyable && !c.Owned {
			// A hull built for this job that nobody sells is demand the build book cannot otherwise see. It is
			// written at most once a day per pilot and hull to the evidence ledger (rolebot_market_demand).
			if c.Hull != nil && c.Hull.Roles[job] >= 1.0 {
				m.noteHullDemand(ctx, charID, c.Hull.TypeID, c.Hull.Name)
			}
			continue
		}
		pool = append(pool, c.Template)
	}
	return pool, nil
}

// hullDemandNoted is the per-(pilot, hull) daily throttle on noteHullDemand.
var (
	hullDemandMu    sync.Mutex
	hullDemandNoted = map[[2]int64]int64{}
)

const hullDemandNoteEverySec = 24 * 3600

func init() {
	registerSweep("hullDemandNotedSweep", func() {
		now := clock.Unix()
		hullDemandMu.Lock()
		for k, at := range hullDemandNoted {
			if now-at >= 2*hullDemandNoteEverySec {
				delete(hullDemandNoted, k)
			}
		}
		hullDemandMu.Unlock()
	})
	registerCharForget("hullDemandNoted", func(id int64) {
		hullDemandMu.Lock()
		for k := range hullDemandNoted {
			if k[0] == id {
				delete(hullDemandNoted, k)
			}
		}
		hullDemandMu.Unlock()
	})
}

// noteHullDemand writes one unit of a built-for-the-job hull nobody sells to
// the demand ledger, at most once a day per pilot per hull.
func (m *Mod) noteHullDemand(ctx context.Context, charID, typeID int64, name string) {
	if charID == 0 || typeID == 0 {
		return
	}
	now := clock.Unix()
	k := [2]int64{charID, typeID}
	hullDemandMu.Lock()
	if now-hullDemandNoted[k] < hullDemandNoteEverySec {
		hullDemandMu.Unlock()
		return
	}
	hullDemandNoted[k] = now
	hullDemandMu.Unlock()
	if err := m.noteDemandRow(ctx, charID, 0, typeID, name, 1); err != nil {
		rlogf("rolebots: hull demand %s: %v", name, err)
	}
}

// rankedHull is one hull in the running for a job, with the pull it has
// toward it.
type rankedHull struct {
	h    *HullProfile
	rank float64
}

// spreadByClass trims a rank-sorted shortlist to limit entries while spanning the hull ladder: the
// best of each size step first, then the best of what is left. The input must be sorted best-first;
// each pass keeps that order, so the pilot's own empire's hull wins its rung.
func spreadByClass(short []rankedHull, limit int) []rankedHull {
	if limit <= 0 || len(short) <= limit {
		return short
	}
	out := make([]rankedHull, 0, limit)
	taken := make(map[int64]bool, limit)
	seenClass := map[HullClass]bool{}
	for _, r := range short {
		if len(out) >= limit {
			return out
		}
		if seenClass[r.h.Class] {
			continue
		}
		seenClass[r.h.Class] = true
		taken[r.h.TypeID] = true
		out = append(out, r)
	}
	for _, r := range short {
		if len(out) >= limit {
			break
		}
		if taken[r.h.TypeID] {
			continue
		}
		out = append(out, r)
	}
	return out
}

// aspirationalFits is the library the skill planner reads: what this pilot might one day fly, with
// the skill gate lifted and the market gate kept. The planner decides what to train by scoring fits,
// so a skill-gated list would show only ships it can already fly. Every job is included.
func (m *Mod) aspirationalFits(ctx context.Context, charID int64, weights map[string]float64, tn Tuning) ([]*fitTemplate, error) {
	_, hulls, err := m.loadHullProfiles(ctx)
	if err != nil {
		return nil, err
	}
	cat, err := m.buyableModules(ctx)
	if err != nil {
		return nil, err
	}
	// Unscoped, like the module catalogue above: the library asks what a hull would carry, not what one
	// station shelves. A bay the planner cannot see shows no reason to train Drones.
	dcat, err := m.buyableDrones(ctx)
	if err != nil {
		rlogf("rolebots: aspirational drone catalogue: %v", err)
		dcat = nil
	}
	who := m.charPilotRace(ctx, charID)
	sold, err := m.hullsOnTheMarket(ctx)
	if err != nil {
		return nil, err
	}
	var out []*fitTemplate
	// The specialist seats join the library only for a pilot with some pull toward them (jobWeights),
	// and combat gains Kite and Brawl variants only for a pilot with a style (fitstyle.go).
	jobs := []string{JobMining, JobTrade, JobCombat, JobPiracy}
	// Freight joins the same way: no appetite weighs it (no activity maps to it), so it is in the
	// library only once something gives a lifter a weight.
	for _, seat := range []string{JobLogi, JobTackle, JobEwar, JobScout, JobFreight} {
		if weights[seat] > 0 {
			jobs = append(jobs, seat)
		}
	}
	brawlShare, kiteShare := styleShares(tn)
	for _, job := range jobs {
		var short []rankedHull
		for _, h := range hulls {
			w := h.RoleWeight(job)
			if w <= 0 {
				continue
			}
			rank := w * fitGenRaceMul(who, h.RaceID)
			if !sold[h.TypeID] {
				rank *= fitGenUnsoldPenalty
			}
			short = append(short, rankedHull{h, rank})
		}
		sort.SliceStable(short, func(i, j int) bool { return short[i].rank > short[j].rank })
		// The ladder, not the top of one rung: the best hull of each size step first, the rest by rank, so
		// the planner sees what a pilot might grow into. See docs/comment-notes-b09.md#hull-ladder.
		short = spreadByClass(short, fitGenAspirationalShortlist)
		for _, r := range short {
			// nil loadout, nil reach and zero taste: the library gates nothing and jitters nothing, since a
			// taste that swapped a module would change the skill a pilot is told to train.
			t, _, err := m.generateFitFor(ctx, charID, job, r.h, nil, cat, dcat, nil, nil, fitTaste{})
			if err != nil || t == nil {
				continue
			}
			out = append(out, t)
			// The style variants: an explicit intent and nothing else, no seed or budget, so they differ from
			// the auto fit only in what the style changes.
			if job != JobCombat && job != JobPiracy {
				continue
			}
			for _, v := range []struct {
				st    engageStyle
				share float64
			}{{engageBrawl, brawlShare}, {engageKite, kiteShare}} {
				if v.share <= 0 {
					continue
				}
				vt, _, err := m.generateFitFor(ctx, charID, job, r.h, nil, cat, dcat, nil, nil, fitTaste{style: v.st})
				if err != nil || vt == nil {
					continue
				}
				vt.Name = r.h.Name + " (" + v.st.String() + ")"
				out = append(out, vt)
			}
		}
	}
	return out, nil
}

// ownedHullTypes is which hull TYPES this pilot already has a live ship of.
// Ownership pulls a pilot toward the hull it is already sitting in, which
// is both cheaper and how a pilot behaves.
func (m *Mod) ownedHullTypes(ctx context.Context, charID int64) (map[int64]bool, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.typeID
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND g.categoryID = 6 AND t.groupID <> 29`, charID)
	if err != nil {
		return nil, fmt.Errorf("ownedHullTypes: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// hullsOnTheMarket is which hulls anybody is currently selling. A hull nobody sells is not excluded
// but penalised, and a want is raised for it.
func (m *Mod) hullsOnTheMarket(ctx context.Context) (map[int64]bool, error) {
	// Through snapshotFor for its single-flight: otherwise every pilot that finds the snapshot expired
	// runs the read at once.
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshotFor(snapKey("hulls-on-market"), fitGenMarketTTL, func() (map[int64]bool, error) {
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT DISTINCT o.typeID
			FROM market_orders o
			JOIN invTypes t ON t.typeID = o.typeID
			JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 6
			WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0`))
		if err != nil {
			return nil, fmt.Errorf("hullsOnTheMarket: %w", err)
		}
		defer rows.Close()
		out := map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out[id] = true
		}
		return out, rows.Err()
	})
}

// ── the buy cap, applied to what a fit is made of ───────────────────────────
//
// buyableModules and hullsOnTheMarket ask whether anybody anywhere sells a type. The home loop's buy
// cap (shopClimb, reach.go) ends the outward search at the adjacent band, so a fit naming something
// sold only past it has a shopping trip that cannot succeed. Fits are therefore assembled from what
// this pilot can buy, as chargeShopScope does for ammo. The scope is the pilot's, so it is a filter
// over the global cached catalogue. See docs/comment-notes-b09.md#buy-cap-scope.

var (
	scopeSoldMu sync.Mutex
	scopeSoldAt = map[string]time.Time{}
	scopeSold   = map[string]map[int64]bool{}
)

// typesSoldInScope is every published type of one inventory category with a live sell order inside
// regions. Keyed by the region set, not the pilot: residents of one region share an answer, which
// keeps a plan pass over the whole roster affordable. Same TTL as the catalogue it filters.
func (m *Mod) typesSoldInScope(ctx context.Context, regions map[int64]bool, categoryID int64) (map[int64]bool, error) {
	return m.typesSoldInScopeFor(ctx, regions, categoryID, nil)
}

// typesSoldInScopeFor is typesSoldInScope minus the stations of factions the pilot despises, which
// procurement refuses to buy at (stationFilterPasses' hard pass). Without it a seller procurement
// refuses counts as sold in scope and the hull is picked in vain. Keyed by the exclusion too, so
// pilots sharing a region and an employer's politics share the read.
func (m *Mod) typesSoldInScopeFor(ctx context.Context, regions map[int64]bool, categoryID int64,
	despised []int64) (map[int64]bool, error) {
	ids := make([]int64, 0, len(regions))
	for id := range regions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	ex := append([]int64(nil), despised...)
	sort.Slice(ex, func(i, j int) bool { return ex[i] < ex[j] })
	key := fmt.Sprintf("%d:%v:x%v", categoryID, ids, ex)

	scopeSoldMu.Lock()
	if out, ok := scopeSold[key]; ok && clock.Now().Sub(scopeSoldAt[key]) < fitGenMarketTTL {
		scopeSoldMu.Unlock()
		return out, nil
	}
	scopeSoldMu.Unlock()

	sold, err := m.soldByRegionFaction(ctx, categoryID)
	if err != nil {
		return nil, fmt.Errorf("typesSoldInScope: %w", err)
	}
	out := soldInScope(sold, regions, ex)

	scopeSoldMu.Lock()
	scopeSold[key], scopeSoldAt[key] = out, clock.Now()
	scopeSoldMu.Unlock()
	return out, nil
}

// regionFaction is one place a type is on sale: a region, and the faction of
// the NPC corporation that owns the station (0 for a player-owned or
// unaligned station).
type regionFaction struct {
	Region, Faction int64
	// Policed is whether the station is in space at or above the outlaw travel
	// ceiling (outlawhaven.go): a shelf a pilot with a closed record cannot reach.
	Policed bool
}

// soldByRegionFaction is every published type of one category with a live sell order, and where:
// one roster-wide read per category per fitGenMarketTTL, single-flight through snapshotFor. The
// pilot's scope and its employer's politics are applied in Go (soldInScope) over this result.
func (m *Mod) soldByRegionFaction(ctx context.Context, categoryID int64) (map[int64][]regionFaction, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshotFor(snapKey("sold-scope", categoryID), fitGenMarketTTL,
		func() (map[int64][]regionFaction, error) {
			rows, err := m.db.QueryContext(sctx, capStatement(`
				SELECT DISTINCT o.typeID, s.regionID, COALESCE(n.factionID, 0), (COALESCE(s.security, 0) >= 0.45)
				FROM market_orders o
				JOIN invTypes t ON t.typeID = o.typeID AND t.published = 1
				JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = ?
				JOIN staStations st ON st.stationID = o.stationID
				JOIN mapSolarSystems s ON s.solarSystemID = st.solarSystemID
				LEFT JOIN crpNPCCorporations n ON n.corporationID = st.corporationID
				WHERE o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0`), categoryID)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := map[int64][]regionFaction{}
			for rows.Next() {
				var typeID int64
				var rf regionFaction
				if err := rows.Scan(&typeID, &rf.Region, &rf.Faction, &rf.Policed); err != nil {
					return nil, err
				}
				out[typeID] = append(out[typeID], rf)
			}
			return out, rows.Err()
		})
}

// soldInScope is the per-pilot half: a type counts when any place it is sold is inside the region
// set and not at a station of a despised faction. A station with no NPC owner (faction 0) passes.
func soldInScope(sold map[int64][]regionFaction, regions map[int64]bool, despised []int64) map[int64]bool {
	bad := make(map[int64]bool, len(despised))
	for _, f := range despised {
		bad[f] = true
	}
	out := map[int64]bool{}
	for typeID, places := range sold {
		for _, p := range places {
			if regions[p.Region] && (p.Faction == 0 || !bad[p.Faction]) {
				out[typeID] = true
				break
			}
		}
	}
	return out
}

// sweepScopeSold drops scope answers past their TTL. The key carries a pilot's region set and
// politics, so without the sweep every combination ever asked about is held for the process life.
func sweepScopeSold() {
	cut := clock.Now().Add(-fitGenMarketTTL)
	scopeSoldMu.Lock()
	for k, at := range scopeSoldAt {
		if at.Before(cut) {
			delete(scopeSoldAt, k)
			delete(scopeSold, k)
		}
	}
	scopeSoldMu.Unlock()
}

// hullsSoldWithin is hullsOnTheMarket seen through the pilot's buy cap.
func (m *Mod) hullsSoldWithin(ctx context.Context, reach map[int64]bool) (map[int64]bool, error) {
	if len(reach) == 0 {
		return m.hullsOnTheMarket(ctx)
	}
	return m.typesSoldInScope(ctx, reach, catShip)
}

// buyableModulesWithin is buyableModules seen through the pilot's buy cap. A scope that sells
// nothing is returned empty, not replaced by the global catalogue: a band with no module market
// cannot complete a fit, and the global list only restores the doomed trip. A failed scope read
// degrades to the global answer: an unreadable filter is ignorance, not scarcity.
func (m *Mod) buyableModulesWithin(ctx context.Context, reach map[int64]bool) ([]*marketModule, error) {
	return m.buyableModulesWithinFor(ctx, reach, nil)
}

// buyableModulesWithinFor is buyableModulesWithin minus what only despised
// factions' stations sell (typesSoldInScopeFor).
func (m *Mod) buyableModulesWithinFor(ctx context.Context, reach map[int64]bool, despised []int64) ([]*marketModule, error) {
	cat, err := m.buyableModules(ctx)
	if err != nil || len(reach) == 0 || len(cat) == 0 {
		return cat, err
	}
	in, err := m.typesSoldInScopeFor(ctx, reach, catModule, despised)
	if err != nil {
		rlogf("rolebots: scoped module catalogue: %v", err)
		return cat, nil
	}
	out := make([]*marketModule, 0, len(cat))
	for _, mm := range cat {
		if in[mm.TypeID] {
			out = append(out, mm)
		}
	}
	return out, nil
}

// isMicrowarpdrive reports whether a propulsion module is an MWD rather than
// an afterburner, by the signature bloom only an MWD carries. Never by name.
func isMicrowarpdrive(mm *marketModule) bool {
	return mm != nil && mm.Attrs[attrSigRadiusBonus] > 0
}

// fitGenRaceMul is a hull's race term on the generator's shortlist. At tolerance zero it is the
// own-race thumb on the scale (x1.15); above zero it is the xenophobe's item rule (tolerance.go
// xenoItemMul). A refused hull is kept at xenoRefusedHullMul, not dropped: it falls off the shortlist
// whenever anything the pilot will fly can do the job.
func fitGenRaceMul(who pilotRace, hullRace int64) float64 {
	mul, ok := xenoItemMul(who.Race, who.Phobia, hullRace)
	if !ok {
		return xenoRefusedHullMul
	}
	return mul
}

func init() { registerSweep("scopeSold", sweepScopeSold) }
