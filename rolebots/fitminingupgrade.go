package rolebots

// Mining Laser Upgrade I (group 546) carries miningAmountBonus (434, +5%), which the server
// multiplies into every laser's yield per module with no stacking penalty (db.GetMiningYieldBonus).
// The role scores on 434 only: the meta variants (22609-22623) carry 207 and no 434, so they mine
// nothing extra on the server and nobody sells them. The Ice Harvester Upgrade (780) is not here:
// roleMiner serves ore lasers and strip miners, and ice.go prices that module.
//
// Every upgrade carries cpuPenaltyPercent (1082, +10%) on the ship's miners. The server does not
// enforce it, but the stat engine charges it and the generator reserves it, as the client does.

const (
	grpMiningUpgrade      = 546
	attrMiningAmountBonus = 434  // % yield per upgrade, multiplied flat
	attrCPUPenaltyPct     = 1082 // % extra CPU on the ship's miners, per upgrade

	// miningUpgradeTypicalPct and cargoExpanderTypicalMult are the tech-one figures (Mining Laser
	// Upgrade I +5%, Expanded Cargohold I x1.18) the count assumes before it knows what the market has.
	miningUpgradeTypicalPct  = 5.0
	cargoExpanderTypicalMult = 1.18

	// miningUpgradeMax bounds the count. The yield stacks linearly but each one
	// adds a tenth of the ship's miner CPU again, and a rack of nothing but
	// upgrades is a hull with no hold at all.
	miningUpgradeMax = 3
)

// miningUpgradeCount is how many of a miner's lows should be upgrades rather than cargo
// expanders. Roles are tried in order and never score-compared (bestForWish), so this is decided
// before the wish is written: a low takes an upgrade (yield x 1.05) unless an expander raises the
// hold worth more, (1 + hold*1.18/W) / (1 + hold/W) with W = miningHoldWorthM3. Pure.
func miningUpgradeCount(holdM3 float64, lo int) int {
	if lo < 1 || holdM3 < 0 {
		return 0
	}
	upgrade := 1 + miningUpgradeTypicalPct/100
	hold := holdM3
	n := 0
	for slot := 0; slot < lo && n < miningUpgradeMax; slot++ {
		expander := (1 + hold*cargoExpanderTypicalMult/miningHoldWorthM3) / (1 + hold/miningHoldWorthM3)
		if upgrade < expander {
			// An expander wins this low, and the ratio only grows, so it wins every later low too.
			break
		}
		n++
	}
	return n
}

// canMineWithLasers reports whether this hull has anything for an upgrade to
// multiply: a strip-capable barge, or a hull with a turret hardpoint a laser
// can take (the wishList's own rule). A mining drone is not multiplied by an
// upgrade, so a hull with only drones gets none.
func canMineWithLasers(h *HullProfile) bool {
	if h == nil {
		return false
	}
	if h.StripCapable {
		return h.HiSlots > 0
	}
	return min(h.HiSlots, max(h.Turrets, 0)) > 0
}

// miningUpgradeWishes is the leading low wish of a miner's rack: u slots that ask for an upgrade
// first, then fall back to the roles the low would have had. The fallback matters because Mining
// Upgrades I is a skill only some sheets hold, and a single-role wish leaves the slot empty when
// the pilot cannot fit its module.
func miningUpgradeWishes(h *HullProfile, lo int, fallback ...modRole) []slotWish {
	if !canMineWithLasers(h) {
		return nil
	}
	u := miningUpgradeCount(h.CargoM3, lo)
	if u < 1 {
		return nil
	}
	roles := append([]modRole{roleMiningUpgrade}, fallback...)
	return []slotWish{{Band: flagLoFirst, Roles: roles, Count: u}}
}

// miningUpgradePenaltyCPU is the CPU a candidate upgrade costs the ship's
// miners on top of its own. Zero for anything that is not an upgrade, and zero
// before a miner is placed. Pure.
func miningUpgradePenaltyCPU(fs *fitState, mm *marketModule) float64 {
	if fs == nil || mm == nil || mm.GroupID != grpMiningUpgrade {
		return 0
	}
	return mm.Attrs[attrCPUPenaltyPct] / 100 * fs.minerCPU
}
