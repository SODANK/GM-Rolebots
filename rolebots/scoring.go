package rolebots

// Preference scoring: the one place a bot's opinions turn into numbers.
//
// The module prices preferences in jumps: a ratted belt is worth 25 jumps of
// flying to avoid, the pilot's own region gets a five-jump head start. One
// currency keeps every preference comparable and lets the belt search
// branch-and-bound.
//
// Everything that reads a bot's personality goes through here. A new trait or
// signal is an edit to this file, not to miningPlan, tradePlan and gangPlan.

// beltCost is what a candidate belt system is worth to this bot,
// denominated in jumps. Lower wins. The weights come from the pilot's own
// Tuning, so a careful miner flies past a ratted belt that a careless one works.
func beltCost(tn Tuning, t *beltTarget, preferred, avoidHostile bool) float64 {
	// The rock. ValueGap (value-ranked trip) or Rank (named list) prices each step
	// down at OreJumpWorth jumps, scaled by how fast the hold fills (oreJumpWorthFor).
	c := float64(t.Jumps) + (float64(t.Rank)+t.ValueGap)*oreJumpWorthFor(tn, t.HoldFillSec)
	// How picked over the system is: the belt worked last trip is the dearest in
	// reach, so the search walks outward. Fullness is 1 for untouched systems.
	if t.Fullness < 1 {
		c += (1 - t.Fullness) * beltDepletionMaxCost
	}
	// How much of the system's rock is the rock wanted. A penalty, never a bonus,
	// so beltSearchFloor stays a lower bound. OreShare 0 means unread: no penalty.
	if t.OreShare > 0 {
		c += beltOreShareCost * (1 - t.OreShare/(t.OreShare+beltOreShareKnee))
	}
	// No security term: a system's security is not its danger. Hostile, Margin,
	// Bitten and Enemy are, and each is priced here.
	if avoidHostile && t.Hostile {
		c += tn.HostileCost
	}
	// A belt where a bot was mauled (Bitten) costs extra on top of Hostile.
	if avoidHostile && t.Bitten {
		c += tn.BittenCost
	}
	// A rival corporation working or keeping an address here (corprel.go). Never a
	// bonus: beltSearchFloor needs every term but RecentCost >= 0 (EnemyCost clamps at 0).
	if avoidHostile && t.Enemy {
		c += tn.EnemyCost
	}
	// The miner's escape margin (minerMargin, threat.go), zero once it clears the
	// pilot's line. threatMargin must not be used here: it prices every ratted belt as lethal.
	if t.Margin > 0 {
		c += threatJumpCost(tn, t.Margin)
	}
	// No ore buyer within a hauling trip (orebidreach.go): the hold ends in a
	// stockpile. A penalty, never a bonus: beltSearchFloor depends on it.
	if t.NoBidNear {
		c += beltNoBidCost
	}
	if t.Recent {
		c += tn.RecentCost
	}
	if preferred {
		c -= tn.PreferredBonus
	}
	return c
}

const (
	// oreJumpRefFillSec is the hold-fill time at which OreJumpWorth applies in
	// full: a doubling of value is worth fillSec/120 jumps of flying (about a minute
	// a jump each way), so 20 minutes, a barge's hold, reaches the default eight.
	oreJumpRefFillSec = 1200.0
	// oreJumpWorthMinFrac keeps a small hold's worth above nothing: the
	// value term must still break ties between neighbours.
	oreJumpWorthMinFrac = 0.25
	// beltNoBidCost is what a belt with no ore buyer within oreSellMaxJumps
	// is charged, in jumps.
	beltNoBidCost = 6.0
)

// oreJumpWorthFor is the pilot's OreJumpWorth scaled to how long its hold takes
// to fill. It only shrinks the worth, so beltSearchFloor stays a lower bound.
// fillSec 0 means unread and gives the unscaled worth. Pure.
func oreJumpWorthFor(tn Tuning, fillSec float64) float64 {
	if fillSec <= 0 {
		return tn.OreJumpWorth
	}
	f := fillSec / oreJumpRefFillSec
	if f < oreJumpWorthMinFrac {
		f = oreJumpWorthMinFrac
	}
	if f > 1 {
		f = 1
	}
	return tn.OreJumpWorth * f
}

// beltSearchFloor is the cheapest a system at the given BFS depth could be:
// top-listed ore, no rats, never worked, preferred region. The search stops
// when it exceeds the best candidate, so it must track beltCost exactly.
func beltSearchFloor(tn Tuning, depth int) float64 {
	f := float64(depth) - tn.PreferredBonus
	// A settled pilot's RecentCost is negative, so it is part of the cheapest a
	// candidate could be; ignoring it would prune the pilot's home systems.
	if tn.RecentCost < 0 {
		f += tn.RecentCost
	}
	return f
}
