package rolebots

import (
	"fmt"
	"hash/fnv"
	"sort"
)

// Where a doctrine comes from: the Director of Security derives it on its desk pass from
// five inputs (fleetdoctrine_db.go gathers them; this file is pure, pinned by
// fleetdoctrine_test.go).
//
//   1. Race: hull families and tank layer (raceTank), unless the market can arm a logi
//      only on the other layer.
//   2. Roster: the hull band, the largest T1 class a third of the wing can fly and the one below.
//   3. Market: which specialist seats exist (logi per layer, EWAR).
//   4. Treasury: the cost ceiling. A band whose hulls cost more steps down a class.
//   5. The ROE doctrine and the war: which kinds are offered and which an order raises.

// doctrineInputs is everything derivation reads. Hashable, so a desk pass
// whose inputs have not moved writes nothing (doctrineInputs.hash).
type doctrineInputs struct {
	CorpID int64
	ROE    Doctrine
	RaceID int64
	// Wing is how many pilots the fleets are drawn from (the Security arm,
	// or the whole roster of a corporation too small to have one).
	Wing int
	// Able is how many of them can fly a T1 hull of each class of the
	// corporation's race.
	Able map[HullClass]int
	// The market: a remote repairer for sale on each layer, any EWAR module.
	LogiShield, LogiArmor, Ewar bool
	// ClassCost is the typical hull price per class; CostCeiling what the
	// corporation will replace (0 = unbounded).
	ClassCost   map[HullClass]float64
	CostCeiling float64
	AtWar       bool
}

// doctrineTableVersion is folded into the inputs hash, so a change to the kind catalogue
// (a kind added, a seat rule moved) re-derives every corporation's rows on the next
// desk pass.
const doctrineTableVersion = 3 // bump when the kind catalogue changes

// hash folds the inputs into one number. Deterministic over map order.
func (in doctrineInputs) hash() uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "v%d|%d|%d|%d|%d|%t|%t|%t|%.0f|%t|", doctrineTableVersion, in.CorpID, in.ROE, in.RaceID, in.Wing,
		in.LogiShield, in.LogiArmor, in.Ewar, in.CostCeiling, in.AtWar)
	for _, c := range hullClassOrder {
		fmt.Fprintf(h, "%d:%d:%.0f|", c, in.Able[c], in.ClassCost[c])
	}
	return h.Sum64()
}

// hullClassOrder is the line's ladder, smallest first. Rookie ships, shuttles and
// industrials are not fighting classes. Capitals are the top rung (dreadnoughts, group
// 485), reached only when capital programs (corpprogram.go) put hulls and skills in hand.
var hullClassOrder = []HullClass{ClassFrigate, ClassDestroyer, ClassCruiser, ClassBattlecruiser, ClassBattleship, ClassCapital}

// t1LineGroups is the T1 hull group of each class, the hulls a doctrine's band is read
// against. T2 groups (324, 541, 358, 540, 381) are left out because a line is built on
// what a corporation can buy and rebuild. A carrier is not a line hull.
var t1LineGroups = map[HullClass]int64{
	ClassFrigate:       25,
	ClassDestroyer:     420,
	ClassCruiser:       26,
	ClassBattlecruiser: 419,
	ClassBattleship:    27,
	ClassCapital:       485,
}

const (
	// doctrineBandShare is the share of the wing that must be able to fly a
	// class before the line is built on it.
	doctrineBandShare = 1.0 / 3
	// doctrineCapitalFrac is the share of free capital one replaced hull may
	// cost, when the charter sets no SRP cap.
	doctrineCapitalFrac = 0.10
)

// hullBandFor is the line's band: the largest class at least a third of the wing can
// fly (never below the frigate), and the class below it. A wing that can fly nothing
// (no skill sheets read) gets frigates.
func hullBandFor(able map[HullClass]int, wing int) (lo, hi HullClass) {
	need := int(float64(wing)*doctrineBandShare + 0.999)
	if need < 1 {
		need = 1
	}
	hi = ClassFrigate
	for _, c := range hullClassOrder {
		if able[c] >= need {
			hi = c
		}
	}
	lo = hi
	for i, c := range hullClassOrder {
		if c == hi && i > 0 {
			lo = hullClassOrder[i-1]
		}
	}
	return lo, hi
}

// capBandByCost steps the band down while its top class costs more than the ceiling. A
// ceiling of 0 is no ceiling; the frigate is never stepped below. A stepped band is the
// new top and the class below it, not the uncapped band's bottom, so a frigate commander
// still has a seat in its own doctrine.
func capBandByCost(lo, hi HullClass, cost map[HullClass]float64, ceiling float64) (HullClass, HullClass) {
	if ceiling <= 0 {
		return lo, hi
	}
	stepped := false
	for hi > ClassFrigate && cost[hi] > ceiling {
		idx := 0
		for i, c := range hullClassOrder {
			if c == hi {
				idx = i
			}
		}
		if idx == 0 {
			break
		}
		hi = hullClassOrder[idx-1]
		stepped = true
	}
	if stepped {
		lo = ClassFrigate
		for i, c := range hullClassOrder {
			if c == hi && i > 0 {
				lo = hullClassOrder[i-1]
			}
		}
	}
	if lo > hi {
		lo = hi
	}
	return lo, hi
}

// clampBand applies a kind's own bounds to the corporation's band. ok is
// false when the kind cannot be flown in it at all (a siege line on a
// frigate roster).
func clampBand(k kindDef, lo, hi HullClass) (HullClass, HullClass, bool) {
	if k.MaxBand != 0 && hi > k.MaxBand {
		hi = k.MaxBand
		if lo > hi {
			lo = hi
		}
	}
	if k.MinBand != 0 {
		if hi < k.MinBand {
			return lo, hi, false
		}
		if lo < k.MinBand {
			lo = k.MinBand
		}
	}
	return lo, hi, true
}

// tankFor is the fleet's layer: the race's, unless the market can arm a logi only on the
// other one. With neither for sale the race's stands.
func tankFor(raceID int64, logiShield, logiArmor bool) TankPref {
	t := raceTank[raceID]
	if t == TankNone {
		t = TankShield
	}
	switch {
	case t == TankShield && !logiShield && logiArmor:
		return TankArmor
	case t == TankArmor && !logiArmor && logiShield:
		return TankShield
	}
	return t
}

// deriveFleetDoctrines is the whole derivation: every kind the ROE doctrine
// allows that the band can carry, each with its seats. Sorted in kindTable
// order. A corporation with no combat wing (DoctrineNone) gets none.
func deriveFleetDoctrines(in doctrineInputs, now int64) []FleetDoctrine {
	if in.ROE == DoctrineNone {
		return nil
	}
	tank := tankFor(in.RaceID, in.LogiShield, in.LogiArmor)
	lo, hi := hullBandFor(in.Able, in.Wing)
	lo, hi = capBandByCost(lo, hi, in.ClassCost, in.CostCeiling)
	var out []FleetDoctrine
	for _, k := range kindTable {
		if !k.allows(in.ROE) {
			continue
		}
		kLo, kHi, ok := clampBand(k, lo, hi)
		if !ok {
			continue
		}
		out = append(out, FleetDoctrine{
			CorpID:      in.CorpID,
			Kind:        k.Kind,
			Tank:        tank,
			Style:       k.Style,
			HullMin:     kLo,
			HullMax:     kHi,
			CostCeiling: in.CostCeiling,
			Seats:       seatsForKind(k, kLo, kHi, tank),
			DerivedAt:   now,
		})
	}
	return out
}

// kindForOrder picks the doctrine a fleet order raises: gank for a gank order (nothing
// else stands in for it), else siege, skirmish, guard, sniper or brawl by order type,
// with the fallbacks in the switch below. It returns "" when the corporation has no
// doctrines, which tells the caller to use gangComposition's single curve.
func kindForOrder(have []FleetDoctrine, roe Doctrine, pack, siege, war, gank bool) doctrineKind {
	if len(have) == 0 {
		return ""
	}
	has := map[doctrineKind]bool{}
	for _, d := range have {
		has[d.Kind] = true
	}
	first := func(ks ...doctrineKind) doctrineKind {
		for _, k := range ks {
			if has[k] {
				return k
			}
		}
		return ""
	}
	if gank {
		return first(kindGank)
	}
	var pick doctrineKind
	switch {
	case siege:
		pick = first(kindSiege, kindSniper, kindBrawl)
	case pack:
		pick = first(kindSkirmish, kindKite, kindBrawl)
	case roe == DoctrineDefence:
		pick = first(kindGuard, kindBrawl)
	case war:
		pick = first(kindSniper, kindKite, kindBrawl)
	default:
		pick = first(kindBrawl, kindGuard)
	}
	if pick != "" {
		return pick
	}
	// Fall back to the first non-gank kind in catalogue order. A gank is no stand-in for a
	// line: an ordinary order raised as a gank would send destroyers to die to CONCORD.
	kinds := make([]doctrineKind, 0, len(has))
	for k := range has {
		if k != kindGank {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		return ""
	}
	sort.Slice(kinds, func(i, j int) bool { return kindIndex(kinds[i]) < kindIndex(kinds[j]) })
	return kinds[0]
}

func kindIndex(k doctrineKind) int {
	for i, d := range kindTable {
		if d.Kind == k {
			return i
		}
	}
	return len(kindTable)
}

// doctrineSummary is one line of prose for a doctrine, such as "brawl line (cruiser,
// armour, brawl)", for the CEO log and the panel.
func doctrineSummary(d FleetDoctrine) string {
	band := d.HullMax.String()
	if d.HullMin != d.HullMax {
		band = d.HullMin.String() + "–" + d.HullMax.String()
	}
	tank := "shield"
	if d.Tank == TankArmor {
		tank = "armour"
	}
	return fmt.Sprintf("%s (%s, %s, %s)", d.Kind.Label(), band, tank, d.Style)
}
