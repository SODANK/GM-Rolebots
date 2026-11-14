package rolebots

import (
	"context"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
)

// What a mining module can actually chew, by the core's own rule.
//
// The server refuses three combinations at activation (the mining gate in
// bound.Activate, cmd/gurimoon/main.go): anything but an ice harvester on ice,
// an ice harvester on anything but ice, and anything but a deep core miner on
// Mercoxit. Neither rolebots tier goes through that gate, so this file restates
// it once for both. Classification reads static attributes as the core does:
// the required-skill slots (182/183/184) and chargeGroup1 (604). A deep core
// module requires Deep Core Mining or takes Mercoxit crystals (the Modulated
// Deep Core Miner II has both).

// minerKind is which family of rock a mining module is for.
type minerKind int

const (
	minerOre      minerKind = iota // an ordinary laser or strip miner
	minerDeepCore                  // Deep Core Mining Laser I/II, Modulated Deep Core Miner II
	minerIce                       // an ice harvester
)

// grpMercoxitCrystal is the charge group only deep core miners take.
const grpMercoxitCrystal = 663

// oreMercoxit is the belt table's name for the one family only a deep core
// miner can work.
const oreMercoxit = "Mercoxit"

// requiredSkillAttrs are the three required-skill slots on a type.
var requiredSkillAttrs = [...]int64{182, 183, 184}

// minerKindOf classifies a mining module from its static attributes.
func minerKindOf(attrs map[int64]float64) minerKind {
	requires := func(skill int64) bool {
		for _, a := range requiredSkillAttrs {
			if int64(attrs[a]) == skill {
				return true
			}
		}
		return false
	}
	switch {
	case requires(dogma.SkillIceHarvesting):
		return minerIce
	case requires(dogma.SkillDeepCoreMining) || int64(attrs[dogma.AttrChargeGroup1]) == grpMercoxitCrystal:
		return minerDeepCore
	}
	return minerOre
}

// minerWorksRock reports whether a module of this kind yields anything from a
// rock of this inventory group. A deep core miner works ordinary ore too: the
// core only fences Mercoxit.
func minerWorksRock(k minerKind, rockGroupID int64) bool {
	switch rockGroupID {
	case destiny.GroupMercoxit:
		return k == minerDeepCore
	case destiny.GroupIce:
		return k == minerIce
	}
	return k != minerIce
}

// minerCycle is one fitted mining module reduced to what its yield rate needs.
type minerCycle struct {
	Kind     minerKind
	AmountM3 float64 // per cycle, raw (before skills and bonuses)
	CycleSec float64
}

// minerRatePerSec is the raw m³ per second a rack of mining modules pulls from a
// rock of this group, counting only modules the rock answers to. Each module's
// amount is divided by its own cycle, not one fixed 60 s, which would credit a
// strip miner (540 m³ per 180 s) three times too much.
func minerRatePerSec(mods []minerCycle, rockGroupID int64) float64 {
	rate := 0.0
	for _, mc := range mods {
		if mc.AmountM3 <= 0 || !minerWorksRock(mc.Kind, rockGroupID) {
			continue
		}
		cycle := mc.CycleSec
		if cycle <= 0 {
			cycle = miningCycleSec
		}
		rate += mc.AmountM3 / cycle
	}
	return rate
}

// oreGroupOf is the inventory group of an ore type, the key the core's gate and
// minerWorksRock both read. 0 when unreadable, which minerWorksRock treats as
// ordinary ore.
func (m *Mod) oreGroupOf(ctx context.Context, typeID int64) int64 {
	if typeID == 0 {
		return 0
	}
	return m.typeGroupOf(ctx, typeID)
}

// fittedMinerCycles reads a ship's fitted mining modules (high slots 27-34)
// into minerCycles. One attribute read per module type.
func (m *Mod) fittedMinerCycles(ctx context.Context, shipID int64) ([]minerCycle, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.locationID = ? AND e.flag BETWEEN 27 AND 34
		  AND t.groupID IN (?, ?, ?)`,
		shipID, grpMiningLaser, grpStripMiner, grpDeepCoreMiner)
	if err != nil {
		return nil, err
	}
	var types []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		types = append(types, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]minerCycle, 0, len(types))
	for _, t := range types {
		attrs, err := evedb.GetTypeAttributes(ctx, m.db, t)
		if err != nil {
			return nil, err
		}
		out = append(out, minerCycle{
			Kind:     minerKindOf(attrs),
			AmountM3: attrs[attrMiningAmount],
			CycleSec: attrs[attrDuration] / 1000,
		})
	}
	return out, nil
}

// shipCanMine reports whether a hull, as it sits, can take anything from a rock
// at all: a mining module in a high slot or a mining drone in the bay (the two
// sources miningYield reads). It is asked before the trip, so a pilot whose fit
// was recorded against a gunboat does not undock, warp to a belt and halt.
func (m *Mod) shipCanMine(ctx context.Context, shipID int64) bool {
	if shipID == 0 {
		return false
	}
	if lasers, err := m.fittedMinerCycles(ctx, shipID); err == nil {
		for _, l := range lasers {
			if l.AmountM3 > 0 {
				return true
			}
		}
	}
	if bay, err := m.shipBayDrones(ctx, shipID); err == nil {
		for _, d := range bay {
			if d.GroupID == grpMiningDrone && d.Units > 0 {
				return true
			}
		}
	}
	return false
}

// canMineMercoxit reports whether this pilot, in this ship, can take anything
// from a Mercoxit rock: Deep Core Mining trained and a deep core module fitted.
// The core checks only the module; the skill is asked too, so a ship the pilot
// cannot run is not read as capability.
func (m *Mod) canMineMercoxit(ctx context.Context, b *botRun) bool {
	if b == nil || b.ShipID == 0 {
		return false
	}
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil || skills[dogma.SkillDeepCoreMining] < 1 {
		return false
	}
	mods, err := m.fittedMinerCycles(ctx, b.ShipID)
	if err != nil {
		return false
	}
	for _, mc := range mods {
		if mc.Kind == minerDeepCore && mc.AmountM3 > 0 {
			return true
		}
	}
	return false
}
