package rolebots

import "math"

// What a hull wants, as a number rather than a veto.
//
// The family rules are weights, not gates, so an off-family gun beats an empty
// rack. Size stays absolute upward: a gun too big for the hull is refused (serves).
//
// A damage mod is binary: a heat sink on a missile boat multiplies attribute 64
// while missile damage comes from 213, so it is CPU, PG and ISK for zero DPS.
//
// This is a function, not a field on HullProfile: profiles are shared pointers and
// tests flip h.Weapon after build, so a cached map would go stale.

const (
	doctrineHome     = 1.00 // the family the hull was built around
	doctrineCousin   = 0.55 // another turret family: real guns, no ship bonus
	doctrineStranger = 0.40 // across the turret/launcher divide
	doctrineOffTank  = 0.50 // the layer this hull does not hold
	doctrineOffSize  = 0.25 // per size step below the hull's own
	doctrineOverSize = 0.40 // per size step above it (modtags.go moduleSizeFactor)
	// doctrineGapFloor is how close to home a module must be before an empty slot it
	// could have filled stops counting as a market shortage; under it the want ledger
	// still reports (bestForWish).
	doctrineGapFloor = 0.90
)

// turretGroups is the three turret families. Launchers are sized by group
// in this database and so are handled by launcherBands instead.
var turretGroups = []int64{grpEnergyTurret, grpHybridTurret, grpProjTurret}

// launcherBands is the launcher group per weapon size, the table weaponGroups
// reads. 506 is Missile Launcher Cruise.
var launcherBands = map[int][]int64{
	1: {grpRocketLauncher, grpStandardLauncher},
	2: {grpAssaultLauncher, grpHeavyLauncher},
	3: {grpSiegeLauncher, 506},
}

// damageModFamily is the weapon family each damage mod multiplies.
var damageModFamily = map[int64]WeaponPref{
	grpHeatSink: WeaponEnergy,
	grpMagStab:  WeaponHybrid,
	grpGyrostab: WeaponProjectile,
	grpBCS:      WeaponMissile,
}

// turretGroupFor is the turret group a weapon family shoots, 0 for the
// families that are not turrets at all.
func turretGroupFor(w WeaponPref) int64 {
	switch w {
	case WeaponEnergy:
		return grpEnergyTurret
	case WeaponHybrid:
		return grpHybridTurret
	case WeaponProjectile:
		return grpProjTurret
	}
	return 0
}

// launcherBandOf is the weapon size a launcher group belongs to, 0 when the
// group is not a launcher.
func launcherBandOf(groupID int64) int {
	for size, groups := range launcherBands {
		for _, g := range groups {
			if g == groupID {
				return size
			}
		}
	}
	return 0
}

func isTurretGroup(groupID int64) bool {
	for _, g := range turretGroups {
		if g == groupID {
			return true
		}
	}
	return false
}

// hullAffinity is how much this hull wants a module of this group in this role.
// Zero is a hard gate and serves() treats it as one; anything above is a
// preference the market may argue with.
func hullAffinity(h *HullProfile, role modRole, groupID int64) float64 {
	switch role {
	case roleWeapon:
		return weaponAffinity(h, groupID)
	case roleDamage:
		return damageAffinity(h.Weapon, groupID)
	case roleTank:
		return layerAffinity(h.Tank, groupID, tankGroups)
	case roleRemoteRep:
		return layerAffinity(remoteRepLayer(h), groupID, remoteRepGroups)
	case roleHarden:
		return layerAffinity(h.Tank, groupID, hardenerGroups)
	}
	// Every other role has exactly one group list and no doctrine in it: a
	// cargo expander is a cargo expander on any hull ever built.
	for _, g := range roleGroups[role] {
		if g == groupID {
			return doctrineHome
		}
	}
	return 0
}

// remoteRepLayer is the layer a hull's remote repairers mend. The bonus line wins
// over the tank (an Osprey is bonused for shield transporters, an Exequror for
// armour projectors); without one it is the hull's own tank, since the wing's
// hulls tank alike.
func remoteRepLayer(h *HullProfile) TankPref {
	switch {
	case h.HasSpecialty(SpecRemoteShield) && !h.HasSpecialty(SpecRemoteArmor):
		return TankShield
	case h.HasSpecialty(SpecRemoteArmor) && !h.HasSpecialty(SpecRemoteShield):
		return TankArmor
	}
	return h.Tank
}

// weaponAffinity ranks a gun group against what the hull was built to shoot.
// Launchers carry their size in the group and turrets in an attribute, so
// launcher size is settled here.
func weaponAffinity(h *HullProfile, groupID int64) float64 {
	turret := isTurretGroup(groupID)
	band := launcherBandOf(groupID)
	if !turret && band == 0 {
		return 0 // not a weapon at all
	}

	switch h.Weapon {
	case WeaponEnergy, WeaponHybrid, WeaponProjectile:
		if turret {
			if groupID == turretGroupFor(h.Weapon) {
				return doctrineHome
			}
			return doctrineCousin
		}
		return doctrineStranger * launcherSizeFactor(band, h.WeaponSize)

	case WeaponMissile:
		if band != 0 {
			return doctrineHome * launcherSizeFactor(band, h.WeaponSize)
		}
		return doctrineStranger
	}

	// A drone boat or unclassified hull has no opinion, so every gun is a cousin: an
	// unread hull should end up armed.
	if turret {
		return doctrineCousin
	}
	return doctrineCousin * launcherSizeFactor(band, h.WeaponSize)
}

// launcherSizeFactor is 0 for a launcher too big for the hull (the refusal
// serves() applies to an oversized turret), 1 for the hull's own band, and a step
// of doctrineOffSize for each band below.
func launcherSizeFactor(band, hullSize int) float64 {
	if band == 0 || hullSize <= 0 {
		return 1 // no opinion either way
	}
	if band > hullSize {
		return 0
	}
	return math.Pow(doctrineOffSize, float64(hullSize-band))
}

// damageAffinity is binary: the mod either multiplies this hull's damage or does
// nothing (see the file comment).
func damageAffinity(w WeaponPref, groupID int64) float64 {
	fam, ok := damageModFamily[groupID]
	if !ok {
		return 0
	}
	switch w {
	case WeaponEnergy, WeaponHybrid, WeaponProjectile, WeaponMissile:
		if fam == w {
			return doctrineHome
		}
		return 0
	}
	// A drone boat or unread hull keeps all four as candidates: a damage mod that
	// might be wrong beats an empty low.
	return doctrineOffTank
}

// layerAffinity ranks a tank-flavoured module against the layer this hull
// holds, using the same group lists the hard gate used.
func layerAffinity(t TankPref, groupID int64, groupsFor func(TankPref) []int64) float64 {
	for _, g := range groupsFor(t) {
		if g == groupID {
			return doctrineHome
		}
	}
	other := TankShield
	if t == TankShield {
		other = TankArmor
	}
	for _, g := range groupsFor(other) {
		if g == groupID {
			return doctrineOffTank
		}
	}
	return 0
}

// doctrineSizeFactor is the penalty for a module smaller than the hull's own
// size. Oversized never reaches here (serves() refuses it). It belongs to the raw
// score, never to fitAdjust: roleYieldFloor and roleAbandonFloor read fitAdjust,
// and a size penalty there would stand a role down and leave the slot empty.
func doctrineSizeFactor(mm *marketModule, role modRole, h *HullProfile) float64 {
	switch role {
	case roleWeapon:
		// Launchers are already sized by launcherSizeFactor inside the
		// affinity, so only turrets are left to measure here.
		if !mm.Turret || mm.ChargeSize <= 0 || h.WeaponSize <= 0 {
			return 1
		}
		if steps := h.WeaponSize - mm.ChargeSize; steps > 0 {
			return math.Pow(doctrineOffSize, float64(steps))
		}
		return 1
	case roleSpeed:
		want, got := hullPropClass(h.Class), propMassClass(mm)
		if want == 0 || got == 0 || got >= want {
			return 1
		}
		return math.Pow(doctrineOffSize, float64(want-got))
	}
	// Every other sized module by the generic tag (modtags.go), so a battleship logi
	// does not carry small remote reps because they were cheap and fitted.
	return moduleSizeFactor(h, mm)
}
