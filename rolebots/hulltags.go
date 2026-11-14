package rolebots

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// What a hull is, read off the ship and never typed by anybody.
//
// A ship states what it is for in two places. The Special Ability line in
// invTypes.description names the weapon family and size (module names lie about
// size in this era; the bonus text does not). The hull's hard attributes
// (hardpoints, slot split, drone bay, cargo, CPU, powergrid) exist on every hull.
//
// The result is a preference, not a gate. Whether the assembled ship can do the
// job is decided by jobPower over the simulated stats (fitscore.go), where zero
// means wrong ship.

// HullClass is the size step, from the hull's group. It orders hulls by cost and
// by what they ask of a pilot.
type HullClass int

const (
	ClassUnknown HullClass = iota
	ClassRookie
	ClassShuttle
	ClassFrigate
	ClassDestroyer
	ClassCruiser
	ClassBattlecruiser
	ClassBattleship
	ClassIndustrial
	ClassBarge
	ClassFreighter
	ClassCapital
)

var hullClassByGroup = map[int64]HullClass{
	237: ClassRookie,
	31:  ClassShuttle,
	25:  ClassFrigate,
	324: ClassFrigate, // Elite Frigate: T2, same hull step
	420: ClassDestroyer,
	541: ClassDestroyer,
	26:  ClassCruiser,
	358: ClassCruiser,
	419: ClassBattlecruiser,
	540: ClassBattlecruiser,
	27:  ClassBattleship,
	381: ClassBattleship,
	28:  ClassIndustrial,
	380: ClassIndustrial,
	463: ClassBarge,
	543: ClassBarge,
	513: ClassFreighter,
	30:  ClassCapital,
	485: ClassCapital,
	547: ClassCapital,
	659: ClassCapital,
}

func (c HullClass) String() string {
	switch c {
	case ClassRookie:
		return "rookie ship"
	case ClassShuttle:
		return "shuttle"
	case ClassFrigate:
		return "frigate"
	case ClassDestroyer:
		return "destroyer"
	case ClassCruiser:
		return "cruiser"
	case ClassBattlecruiser:
		return "battlecruiser"
	case ClassBattleship:
		return "battleship"
	case ClassIndustrial:
		return "industrial"
	case ClassBarge:
		return "mining barge"
	case ClassFreighter:
		return "freighter"
	case ClassCapital:
		return "capital"
	}
	return "unknown"
}

// WeaponPref is the weapon family a hull is built around: what its bonus
// multiplies.
type WeaponPref string

const (
	WeaponNone       WeaponPref = ""
	WeaponEnergy     WeaponPref = "energy"
	WeaponHybrid     WeaponPref = "hybrid"
	WeaponProjectile WeaponPref = "projectile"
	WeaponMissile    WeaponPref = "missile"
	WeaponDrone      WeaponPref = "drone"
)

// TankPref is which layer a hull is built to hold: its bonus, or failing that its
// own slot layout.
type TankPref string

const (
	TankNone   TankPref = ""
	TankShield TankPref = "shield"
	TankArmor  TankPref = "armor"
)

// raceWeapon is each empire's own turret family, used when a hull has turret
// hardpoints but its bonus does not say what goes in them.
var raceWeapon = map[int64]WeaponPref{
	1: WeaponHybrid,     // Caldari: hybrids on the gunboats, missiles elsewhere
	2: WeaponProjectile, // Minmatar
	4: WeaponEnergy,     // Amarr
	8: WeaponHybrid,     // Gallente
}

// raceTank is the same fallback for the tanking layer: the two armour
// empires and the two shield ones.
var raceTank = map[int64]TankPref{
	1: TankShield,
	2: TankShield,
	4: TankArmor,
	8: TankArmor,
}

// HullProfile is one hull, read.
type HullProfile struct {
	TypeID    int64
	Name      string
	GroupID   int64
	GroupName string
	RaceID    int64
	Class     HullClass

	// Roles is the hull's pull toward each job, keyed by the job constants
	// (fitjob.go). Around 1.0 means the ship is for it, a small value that it can be
	// pressed into the work, absent that it brings nothing. Never a gate.
	Roles map[string]float64

	Weapon     WeaponPref
	WeaponSize int // 1 small, 2 medium, 3 large: the charge size, not the name
	Tank       TankPref

	HiSlots, MedSlots, LoSlots int
	Turrets, Launchers         int

	// StripCapable is whether this hull may mount a strip miner or ice harvester, and
	// is the one hard gate in this file. Powergrid would not stop a Strip Miner I (less
	// CPU than a Miner II) from fitting a Rifter.
	StripCapable bool

	DroneCapacity     float64
	CargoM3           float64
	CPU, PG           float64
	ShieldHP, ArmorHP float64

	// The capacitor, which decides whether an active module is a module or an
	// ornament. A prop mod and a repairer are bought against this budget
	// (fitsynergy.go).
	CapGJ          float64
	CapRechargeSec float64

	// Specialties is what the bonus line says the hull is built to do beyond its guns
	// (jammer, scanner, remote repair), keyed by Spec*: Roles answers which job, this
	// which kit. Read by the EWAR scorer, fitgen.go and deriveRoles.
	Specialties map[string]float64

	// Bonus is the Special Ability line verbatim ("" when none). Why is how each
	// conclusion was reached, so the panel can show what the module read.
	Bonus string
	Why   []string
}

// Hull specialties (HullProfile.Specialties).
const (
	SpecECM          = "ecm"
	SpecDamp         = "damp"
	SpecTrackDisrupt = "trackdisrupt"
	SpecPainter      = "painter"
	SpecNos          = "nos"
	SpecWeb          = "web"
	SpecScan         = "scan"
	SpecRemoteRep    = "remoterep" // derived: SpecRemoteShield or SpecRemoteArmor
	SpecRemoteShield = "remoteshield"
	SpecRemoteArmor  = "remotearmor"
	// SpecRemoteCap and SpecTrackingLink are read so the panel can say what the hull
	// was built for, but no bot effect exists for either, so the Augoror and Scythe
	// take the fallback fit.
	SpecRemoteCap    = "remotecap"
	SpecTrackingLink = "trackinglink"
	SpecDrone        = "drone"
	SpecMining       = "mining"
	SpecCargo        = "cargo"
)

// specialtyPhrases maps bonus-text phrases to the specialty they state. Matched
// against the lower-cased Special Ability line; every phrase is tried.
var specialtyPhrases = []struct {
	phrase string
	spec   string
}{
	{"ecm", SpecECM},
	{"target jammer", SpecECM},
	{"electronic warfare", SpecECM},
	{"sensor dampener", SpecDamp},
	{"tracking disrupt", SpecTrackDisrupt},
	{"target painter", SpecPainter},
	{"energy vampire", SpecNos},
	{"stasis webifier", SpecWeb},
	{"astrometrics", SpecScan},
	{"remote armor repair", SpecRemoteArmor},
	{"shield transporter", SpecRemoteShield},
	{"energy transfer", SpecRemoteCap},
	{"tracking link", SpecTrackingLink},
	{"drone", SpecDrone},
	{"mining laser", SpecMining},
	{"strip miner", SpecMining},
	{"cargo", SpecCargo},
}

// ewarSpecialtyByGroup is which EWAR module group a specialty is about.
var ewarSpecialtyByGroup = map[int64]string{
	grpECM:               SpecECM,
	grpSensorDamper:      SpecDamp,
	grpTrackingDisruptor: SpecTrackDisrupt,
	grpTargetPainter:     SpecPainter,
}

// HasSpecialty reports whether the bonus line built the hull for this.
func (h *HullProfile) HasSpecialty(spec string) bool { return spec != "" && h.Specialties[spec] > 0 }

// EwarSpecialist reports whether the hull is built around an EWAR module.
func (h *HullProfile) EwarSpecialist() bool {
	for _, s := range ewarSpecialtyByGroup {
		if h.HasSpecialty(s) {
			return true
		}
	}
	return false
}

// deriveSpecialties reads the kit the bonus line is about.
func (h *HullProfile) deriveSpecialties(bonusLower string) {
	if h.Specialties == nil {
		h.Specialties = map[string]float64{}
	}
	for _, sp := range specialtyPhrases {
		if h.Specialties[sp.spec] > 0 || !strings.Contains(bonusLower, sp.phrase) {
			continue
		}
		h.Specialties[sp.spec] = 1
		h.why("specialises in %s — its bonus line says %q", sp.spec, sp.phrase)
	}
	if h.StripCapable && h.Specialties[SpecMining] == 0 {
		h.Specialties[SpecMining] = 1
		h.why("specialises in mining — it can equip strip miners")
	}
	if h.Specialties[SpecRemoteShield] > 0 || h.Specialties[SpecRemoteArmor] > 0 {
		h.Specialties[SpecRemoteRep] = 1
	}
}

// RemoteRepSpecialist reports whether the bonus line is about a remote repairer a
// bot can run: shield transporter or armour projector. Energy transfer is not
// one, since it repairs nothing.
func (h *HullProfile) RemoteRepSpecialist() bool { return h.HasSpecialty(SpecRemoteRep) }

// SameRaceAs reports whether this hull is the pilot's own empire's.
func (h *HullProfile) SameRaceAs(raceID int64) bool { return h.RaceID != 0 && h.RaceID == raceID }

// RoleWeight is the hull's pull toward one job.
func (h *HullProfile) RoleWeight(job string) float64 { return h.Roles[job] }

// ── the bonus line ──────────────────────────────────────────────────────────

// specialAbilityRe pulls the Special Ability sentence(s) out of a hull
// description. Everything after the marker is bonus text, including the faction
// hulls' per-race bonus lines.
var specialAbilityRe = regexp.MustCompile(`(?is)special ability\s*:?\s*(.*)$`)

// weaponPhrases maps a bonus-text phrase to the family it names, tried in this
// order so "Siege Launcher" is not read as a turret by a shorter rule.
var weaponPhrases = []struct {
	phrase string
	pref   WeaponPref
}{
	{"energy turret", WeaponEnergy},
	{"hybrid turret", WeaponHybrid},
	{"projectile turret", WeaponProjectile},
	{"missile launcher", WeaponMissile},
	{"siege launcher", WeaponMissile},
	{"cruise launcher", WeaponMissile},
	{"assault launcher", WeaponMissile},
	{"standard launcher", WeaponMissile},
	{"rocket launcher", WeaponMissile},
	{"heavy launcher", WeaponMissile},
	{"missile", WeaponMissile},
	{"torpedo", WeaponMissile},
	{"drone", WeaponDrone},
}

// sizePhrases reads the weapon size straight out of the bonus text, the only
// place this database states it plainly.
var sizePhrases = []struct {
	phrase string
	size   int
}{
	{"small", 1},
	{"medium", 2},
	{"large", 3},
	{"rocket", 1},
	{"standard missile", 1},
	{"assault", 2},
	{"heavy", 2},
	{"cruise", 3},
	{"siege", 3},
	{"torpedo", 3},
}

// classWeaponSize is the size a hull of each class carries when nothing says
// otherwise: the ordinary progression.
var classWeaponSize = map[HullClass]int{
	ClassRookie:        1,
	ClassFrigate:       1,
	ClassDestroyer:     1,
	ClassCruiser:       2,
	ClassBattlecruiser: 2,
	ClassBattleship:    3,
	ClassIndustrial:    1,
	ClassBarge:         1,
	ClassFreighter:     1,
	ClassCapital:       3,
}

// ── align ───────────────────────────────────────────────────────────────────

// alignSecByClass is nominal align time by hull step. Warp speed is uniform, so
// align is the whole difference between ships. See docs/comment-notes-b11.md#align-table.
var alignSecByClass = map[HullClass]float64{
	ClassShuttle:       1.5,
	ClassRookie:        2,
	ClassFrigate:       2,
	ClassDestroyer:     3,
	ClassCruiser:       4,
	ClassIndustrial:    4,
	ClassBarge:         5,
	ClassBattlecruiser: 6,
	ClassBattleship:    8,
	ClassFreighter:     25,
	ClassCapital:       40,
}

// alignUnknownSec is what a hull whose group is in no class gets: a middling
// ship. Never 0, which would make an unrecognised hull the fastest in the game.
const alignUnknownSec = 4.0

// alignSecForClass is alignSecByClass with the unknown fallback applied.
func alignSecForClass(c HullClass) float64 {
	if s, ok := alignSecByClass[c]; ok && s > 0 {
		return s
	}
	return alignUnknownSec
}

// ── derivation ──────────────────────────────────────────────────────────────

// deriveHullProfile reads one hull from data the caller already loaded. It is
// pure, so the harness can run it without a database.
func deriveHullProfile(typeID int64, name, groupName, description string, groupID, raceID int64,
	attrs map[int64]float64, cargoM3 float64) *HullProfile {

	h := &HullProfile{
		TypeID: typeID, Name: name, GroupID: groupID, GroupName: groupName,
		RaceID: raceID, Class: hullClassByGroup[groupID],
		Roles:         map[string]float64{},
		Specialties:   map[string]float64{},
		HiSlots:       int(attrs[attrHiSlots]),
		MedSlots:      int(attrs[attrMedSlots]),
		LoSlots:       int(attrs[attrLowSlots]),
		Turrets:       int(attrs[attrTurretHard]),
		Launchers:     int(attrs[attrLauncherHard]),
		DroneCapacity: attrs[attrDroneCapacity],
		CargoM3:       cargoM3,
		CPU:           attrs[attrCPUOutput],
		PG:            attrs[attrPGOutput],
		ShieldHP:      attrs[attrShieldCapacity],
		ArmorHP:       attrs[attrArmorHP],
		CapGJ:         attrs[attrCapacitorCap],
		// The attribute is milliseconds; everything downstream wants seconds.
		CapRechargeSec: attrs[attrCapRechargeMS] / 1000,
	}
	bonus := ""
	if mm := specialAbilityRe.FindStringSubmatch(description); len(mm) == 2 {
		bonus = strings.Join(strings.Fields(mm[1]), " ")
	}
	h.Bonus = bonus
	low := strings.ToLower(bonus)

	// Strip-miner capability is read off the whole description: barges say it in the
	// bonus line, Exhumers in the prose above it. deriveRoles also sets it by class.
	if strings.Contains(strings.ToLower(description), "strip miner") {
		h.StripCapable = true
	}

	h.deriveWeapon(low)
	h.deriveTank(low)
	h.deriveSpecialties(low)
	h.deriveRoles(low)
	return h
}

// deriveWeapon settles what the hull shoots with, and its size. The bonus line
// wins when it names a family. Without one, hardpoints decide: more launchers than
// turrets is a missile boat, turrets with no bonus take the empire's family, and a
// drone bay alone makes a drone boat.
func (h *HullProfile) deriveWeapon(bonusLower string) {
	for _, wp := range weaponPhrases {
		if strings.Contains(bonusLower, wp.phrase) {
			h.Weapon = wp.pref
			h.why("shoots %s — its bonus line says %q", wp.pref, wp.phrase)
			break
		}
	}
	if h.Weapon == WeaponNone {
		switch {
		case h.Launchers > h.Turrets && h.Launchers > 0:
			h.Weapon = WeaponMissile
			h.why("shoots missiles — %d launcher hardpoint(s) against %d turret", h.Launchers, h.Turrets)
		case h.Turrets > 0:
			h.Weapon = raceWeapon[h.RaceID]
			if h.Weapon == WeaponNone {
				h.Weapon = WeaponHybrid
			}
			h.why("shoots %s — %d turret hardpoint(s) and no bonus line, so its empire's family",
				h.Weapon, h.Turrets)
		case h.DroneCapacity > 0:
			h.Weapon = WeaponDrone
			h.why("fights with drones — no hardpoints, %.0f m³ of drone bay", h.DroneCapacity)
		}
	}
	// Size. The bonus line states it; failing that the hull's class carries
	// the ordinary progression.
	for _, sp := range sizePhrases {
		if strings.Contains(bonusLower, sp.phrase) {
			h.WeaponSize = sp.size
			break
		}
	}
	if h.WeaponSize == 0 {
		h.WeaponSize = classWeaponSize[h.Class]
	}
	if h.WeaponSize == 0 {
		h.WeaponSize = 1
	}
}

// deriveTank settles which layer the hull holds, on three rungs: the bonus line,
// then the slot split, then base HP when the split ties. Empire habit is the last
// rung and the only guess, so it is rarely reached.
func (h *HullProfile) deriveTank(bonusLower string) {
	shieldWords := strings.Contains(bonusLower, "shield")
	armorWords := strings.Contains(bonusLower, "armor") || strings.Contains(bonusLower, "armour")
	switch {
	case shieldWords && !armorWords:
		h.Tank = TankShield
		h.why("tanks shields — its bonus line is about shields")
	case armorWords && !shieldWords:
		h.Tank = TankArmor
		h.why("tanks armour — its bonus line is about armour")
	case h.MedSlots > h.LoSlots:
		h.Tank = TankShield
		h.why("tanks shields — %d mid slots against %d low", h.MedSlots, h.LoSlots)
	case h.LoSlots > h.MedSlots:
		h.Tank = TankArmor
		h.why("tanks armour — %d low slots against %d mid", h.LoSlots, h.MedSlots)
	case h.ArmorHP > h.ShieldHP:
		h.Tank = TankArmor
		h.why("tanks armour — an even slot split, but %.0f armour against %.0f shield", h.ArmorHP, h.ShieldHP)
	case h.ShieldHP > h.ArmorHP:
		h.Tank = TankShield
		h.why("tanks shields — an even slot split, but %.0f shield against %.0f armour", h.ShieldHP, h.ArmorHP)
	default:
		h.Tank = raceTank[h.RaceID]
		if h.Tank == TankNone {
			h.Tank = TankShield
		}
		h.why("tanks %s — nothing about the hull says either way, so its empire's habit", h.Tank)
	}
}

// The opportunistic-miner residual: what a hull never built for rock is worth at
// it, by how many lasers it can hang. Capped far below the 1.0 a real miner reads,
// so a cruiser is a better stopgap than a frigate and neither out-ranks a barge.
const (
	miningResidualBase      = 0.15
	miningResidualPerHiSlot = 0.05
	miningResidualCap       = 0.6
	// A hull whose bonus line multiplies a weapon is a warship, a worse stopgap miner
	// than a hull that never made that claim. Without this a combat frigate outranked
	// the mining frigate at mining.
	miningResidualWarshipMul = 0.5
	// What a hull whose bonus multiplies drone mining yield is worth at the job: a
	// real miner (a Vexor's drones dig), but not level with a hull built around the
	// work.
	miningDroneBonusWeight = 0.7
	// nonWarshipCombatMul scales the structural combat weight of a hull built
	// for mining or scanning (deriveRoles).
	nonWarshipCombatMul = 0.5
)

// deriveRoles is the tagging: what work this hull is for. A role is a weight, not
// a flag, because most hulls do more than one thing. Nothing here is exclusive or
// a gate.
func (h *HullProfile) deriveRoles(bonusLower string) {
	// A barge is strip-capable by class whatever its description says; set here so
	// the flag is a property of the profile, not the parse. Only ever raised.
	if h.Class == ClassBarge {
		h.StripCapable = true
	}

	// Mining. A bonus that names it is decisive, as is being a barge. Any other hull
	// with a high slot can still bolt a laser on: a small weight, never preferred.
	switch {
	case h.StripCapable:
		h.Roles[JobMining] = 1.0
		h.why("mines — it can equip strip miners and ice harvesters")
	case strings.Contains(bonusLower, "mining laser"):
		h.Roles[JobMining] = 1.0
		h.why("mines — its bonus line is about mining lasers")
	case strings.Contains(bonusLower, "mining"):
		// A mining bonus that is not about lasers is about drones in every case here
		// ("drone hitpoints, damage and mining yield"): real weight, never 1.0.
		h.Roles[JobMining] = miningDroneBonusWeight
		h.why("mines with drones — its bonus line is about drone mining yield, not lasers")
	case h.HiSlots > 0:
		// A hull nobody built for mining mines with whatever its high slots hang, so the
		// weight scales with them: a five-slot cruiser beats a shuttle, far below a miner.
		w := miningResidualBase + miningResidualPerHiSlot*float64(h.HiSlots)
		if w > miningResidualCap {
			w = miningResidualCap
		}
		if weaponRoleWords(bonusLower) {
			w *= miningResidualWarshipMul
			h.why("could mine — %d high slot(s), but its bonus line says warship, so it is a poor stopgap", h.HiSlots)
		} else {
			h.why("could mine — %d high slot(s) to hang lasers off, but nothing about the hull says miner", h.HiSlots)
		}
		h.Roles[JobMining] = w
	}

	// Hauling. Named in the bonus, the hull's purpose by group, or a hold big enough
	// to load.
	switch {
	case strings.Contains(bonusLower, "cargo"):
		h.Roles[JobTrade] = 1.0
		h.why("hauls — its bonus line is about cargo")
	case h.Class == ClassIndustrial || h.Class == ClassFreighter:
		h.Roles[JobTrade] = 1.0
		h.why("hauls — it is a %s", h.Class)
	case h.CargoM3 > 0:
		// Scaled against a frigate-ish hold and capped below the real haulers, so a
		// combat cruiser with a big bay never out-ranks an industrial.
		w := h.CargoM3 / 400
		if w > 0.6 {
			w = 0.6
		}
		h.Roles[JobTrade] = w
		h.why("could haul — %.0f m³ of hold", h.CargoM3)
	}
	// Freight is the same hull judged for lift: whatever hauls, lifts, on the same
	// rungs.
	if w := h.Roles[JobTrade]; w > 0 {
		h.Roles[JobFreight] = w
	}

	// Combat: the bonus line is decisive, else hardpoints counted in absolute terms.
	// See docs/comment-notes-b11.md#hull-combat-role.
	guns := float64(h.Turrets + h.Launchers)
	if weaponRoleWords(bonusLower) {
		h.Roles[JobCombat] = 1.0
		h.why("fights — its bonus line multiplies a weapon")
	} else if guns > 0 || h.DroneCapacity > 0 {
		hp := guns / 4
		if hp > 1 {
			hp = 1
		}
		w := 0.3 + 0.5*hp
		if h.DroneCapacity > 0 {
			w += 0.05
		}
		if guns == 0 {
			w = 0.3 + 0.05 // drones and nothing else
		}
		h.Roles[JobCombat] = w
		h.why("could fight — %d hardpoint(s) and %.0f m³ of drone bay, but no bonus says warship",
			int(guns), h.DroneCapacity)
	}

	// A hull whose bonus line is about rock, cargo or scanning and never a weapon is
	// not a warship with hardpoints: halve it (flows into piracy and tackle).
	if !weaponRoleWords(bonusLower) && h.Roles[JobCombat] > 0 &&
		(h.HasSpecialty(SpecMining) || h.HasSpecialty(SpecScan)) {
		h.Roles[JobCombat] *= nonWarshipCombatMul
		h.why("a poor warship — built for mining or scanning, and no bonus says it fights")
	}

	// Piracy is combat read through the mid rack: a hull with no free mid cannot hold
	// a target, and the best pirates have a spare mid after the point and web.
	if c := h.Roles[JobCombat]; c > 0 {
		switch {
		case h.MedSlots >= 3:
			h.Roles[JobPiracy] = c
			h.why("could pirate — %d mid slot(s): point, web and something over", h.MedSlots)
		case h.MedSlots == 2:
			h.Roles[JobPiracy] = c * 0.85
			h.why("could pirate — 2 mid slots, which is a point and a web and no room to be clever")
		case h.MedSlots == 1:
			h.Roles[JobPiracy] = c * 0.4
			h.why("poor pirate — one mid slot, so it points OR webs, never both")
		default:
			h.why("cannot pirate — no mid slots, so nothing it shoots at has to stay")
		}
	}

	// The gang seats are read off the structure the seat uses, weighted by hull step
	// where the seat has a silhouette: a fast cheap tackler, a cruiser logi.
	if p := h.Roles[JobPiracy]; p > 0 {
		// Tackle rides the piracy weight, scaled by how catchable the hull is: a
		// battleship with three mids holds well and arrives after everyone has left.
		size := 0.2
		switch h.Class {
		case ClassFrigate, ClassRookie:
			size = 1.0
		case ClassDestroyer:
			size = 0.8
		case ClassCruiser:
			size = 0.5
		}
		h.Roles[JobTackle] = p * size
		h.why("could tackle — the piracy rack at a %s's speed", h.Class)
	}
	// The gank scout wants a mid for the scanner, a low for a stabiliser and legs: a
	// frigate or the strikers' destroyer. It is unarmed, so combat weight is no part.
	if h.MedSlots >= 1 && h.LoSlots >= 1 {
		switch h.Class {
		case ClassFrigate:
			h.Roles[JobScout] = 1.0
		case ClassDestroyer:
			h.Roles[JobScout] = 0.8
		case ClassCruiser:
			h.Roles[JobScout] = 0.3
		}
		if h.Roles[JobScout] > 0 {
			h.why("could scout for a gank — a mid for the scanner, a low for a stabiliser, at a %s's speed", h.Class)
		}
	}
	// Logi wants two highs or more for repairers; the weight climbs with the rack and
	// peaks at the cruiser step. Hauling and mining hulls never take the seat.
	if h.HiSlots >= 2 && h.Class != ClassIndustrial && h.Class != ClassBarge && h.Class != ClassFreighter {
		w := 0.15 + 0.12*float64(h.HiSlots)
		switch h.Class {
		case ClassCruiser, ClassBattlecruiser:
			w *= 1.25
		case ClassFrigate, ClassRookie, ClassDestroyer:
			w *= 0.6
		}
		if w > 1 {
			w = 1
		}
		// The same rule as the EWAR seat: a hull whose bonus line is about remote repair
		// (Osprey, Exequror) is the logi ship; a Maller only has somewhere to hang one.
		if !h.RemoteRepSpecialist() {
			w *= 0.4
		}
		h.Roles[JobLogi] = w
		h.why("could run logistics — %d high slot(s) to hang repairers off", h.HiSlots)
	}
	// The EWAR seat wants mids, and a hull built around an electronic module
	// (EwarSpecialist) is the natural one. Three mids is the floor.
	if h.MedSlots >= 3 {
		w := math.Min(1, 0.15*float64(h.MedSlots))
		if !h.EwarSpecialist() {
			w *= 0.4
		}
		switch h.Class {
		case ClassBattleship, ClassFreighter, ClassCapital, ClassIndustrial, ClassBarge:
			w *= 0.3
		}
		h.Roles[JobEwar] = w
		h.why("could fly electronic warfare — %d mid slot(s)", h.MedSlots)
	}
}

// weaponRoleWords reports whether a bonus line multiplies a weapon: the
// difference between a hull built to fight and one that merely can.
func weaponRoleWords(bonusLower string) bool {
	for _, wp := range weaponPhrases {
		if strings.Contains(bonusLower, wp.phrase) {
			return true
		}
	}
	return false
}

func (h *HullProfile) why(format string, args ...any) {
	h.Why = append(h.Why, fmt.Sprintf(format, args...))
}

// ── the catalogue ───────────────────────────────────────────────────────────

var (
	hullProfileMu   sync.Mutex
	hullProfileByID map[int64]*HullProfile
	hullProfileList []*HullProfile
)

// hullCandidateWhere is what counts as a hull a pilot might one day fly: no
// capsules, no type with marketGroupID 0 (dev-test junk), and rookie ships (group
// 237, marketGroupID 0 and published = 0) exempt from both tests.
const hullCandidateWhere = `
	g.categoryID = 6
	AND t.groupID <> 29
	AND ((t.published = 1 AND t.marketGroupID > 0) OR t.groupID = 237)`

// loadHullProfiles reads and derives the whole ship catalogue once. Static data,
// cached for the life of the process.
func (m *Mod) loadHullProfiles(ctx context.Context) (map[int64]*HullProfile, []*HullProfile, error) {
	hullProfileMu.Lock()
	if hullProfileByID != nil {
		byID, list := hullProfileByID, hullProfileList
		hullProfileMu.Unlock()
		return byID, list, nil
	}
	hullProfileMu.Unlock()

	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, g.groupName,
		       COALESCE(t.raceID, 0), COALESCE(t.capacity, 0), COALESCE(t.description, '')
		FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID
		WHERE`+hullCandidateWhere+`
		ORDER BY t.typeID`)
	if err != nil {
		return nil, nil, fmt.Errorf("loadHullProfiles: %w", err)
	}
	type raw struct {
		typeID, groupID, raceID int64
		name, groupName, desc   string
		cargo                   float64
	}
	var all []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.typeID, &r.name, &r.groupID, &r.groupName,
			&r.raceID, &r.cargo, &r.desc); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("loadHullProfiles scan: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	// Attributes for the whole catalogue in one pass: per-hull queries would be about
	// 180 round trips behind a mutex on the first plan after a restart.
	attrs, err := m.hullAttributeSets(ctx)
	if err != nil {
		return nil, nil, err
	}

	byID := make(map[int64]*HullProfile, len(all))
	list := make([]*HullProfile, 0, len(all))
	for _, r := range all {
		h := deriveHullProfile(r.typeID, r.name, r.groupName, r.desc, r.groupID, r.raceID,
			attrs[r.typeID], r.cargo)
		byID[r.typeID] = h
		list = append(list, h)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Class != list[j].Class {
			return list[i].Class < list[j].Class
		}
		return list[i].Name < list[j].Name
	})

	hullProfileMu.Lock()
	hullProfileByID, hullProfileList = byID, list
	hullProfileMu.Unlock()
	return byID, list, nil
}

// hullAttributeSets pulls every fitting-relevant attribute for every hull
// candidate in one query.
func (m *Mod) hullAttributeSets(ctx context.Context) (map[int64]map[int64]float64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ta.typeID, ta.attributeID,
		       COALESCE(NULLIF(ta.valueFloat, 0), CAST(ta.valueInt AS DOUBLE), ta.valueFloat)
		FROM dgmTypeAttributes ta
		JOIN invTypes t ON t.typeID = ta.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE`+hullCandidateWhere+`
		  AND ta.attributeID IN (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attrHiSlots, attrMedSlots, attrLowSlots, attrTurretHard, attrLauncherHard,
		attrDroneCapacity, attrCPUOutput, attrPGOutput, attrShieldCapacity, attrArmorHP,
		attrCapacitorCap, attrCapRechargeMS)
	if err != nil {
		return nil, fmt.Errorf("hullAttributeSets: %w", err)
	}
	defer rows.Close()
	out := map[int64]map[int64]float64{}
	for rows.Next() {
		var typeID, attrID int64
		var v float64
		if err := rows.Scan(&typeID, &attrID, &v); err != nil {
			return nil, fmt.Errorf("hullAttributeSets scan: %w", err)
		}
		if out[typeID] == nil {
			out[typeID] = map[int64]float64{}
		}
		out[typeID][attrID] = v
	}
	return out, rows.Err()
}
