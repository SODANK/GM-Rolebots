package rolebots

import (
	"context"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

// What a drone bay is worth in a fight and on a rock.
// fitdrones.go decides what goes into a bay; this file reads what is in one and turns it into
// the numbers both tiers fight and mine on.
// It matches the core, not the stat engine: a player's drone hits with its raw type
// attributes times rates.PlayerDamage (loadDroneAttributes; drone.Manager.fireCycle) and
// mines its raw attribute 77 times rates.MiningYield (droneMineCycle), with no drone skill
// bonuses. The Drones skill caps how many can be in space (droneMaxActive), applied here.

// droneControlRangeM is the reach credited to a ship that fights with drones alone: the
// classic 20 km control range. A ship with any gun keeps its gun's reach.
const droneControlRangeM = 20_000.0

// botDrone is one drone type sitting in a bay.
type botDrone struct {
	TypeID  int64
	GroupID int64
	Units   int64
	// DPS is one drone's damage per second, before rates.PlayerDamage, the same basis
	// combatLoadout.dps() keeps its guns on.
	DPS float64
	// MiningM3PerSec is one mining drone's yield, before rates.MiningYield.
	MiningM3PerSec float64
}

// droneLaunchPick is how many of one type go into space.
type droneLaunchPick struct {
	TypeID int64
	N      int64
}

// planDroneLaunch picks what goes into space from a bay: the best drones of one group
// first, up to max in total. Pure, so both tiers take the same flight off the same bay.
func planDroneLaunch(bay []botDrone, groupID int64, max int64) []droneLaunchPick {
	if max <= 0 {
		return nil
	}
	var cands []botDrone
	for _, d := range bay {
		if d.GroupID == groupID && d.Units > 0 && droneOutput(d) > 0 {
			cands = append(cands, d)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if a, b := droneOutput(cands[i]), droneOutput(cands[j]); a != b {
			return a > b
		}
		return cands[i].TypeID < cands[j].TypeID
	})
	var out []droneLaunchPick
	left := max
	for _, d := range cands {
		if left <= 0 {
			break
		}
		n := d.Units
		if n > left {
			n = left
		}
		out = append(out, droneLaunchPick{TypeID: d.TypeID, N: n})
		left -= n
	}
	return out
}

// droneOutput is the one number a drone is ranked on within its group.
func droneOutput(d botDrone) float64 {
	if d.GroupID == grpMiningDrone {
		return d.MiningM3PerSec
	}
	return d.DPS
}

// launchedOutput sums a group's output over the flight planDroneLaunch picks.
func launchedOutput(bay []botDrone, groupID int64, max int64) float64 {
	by := make(map[int64]botDrone, len(bay))
	for _, d := range bay {
		by[d.TypeID] = d
	}
	total := 0.0
	for _, p := range planDroneLaunch(bay, groupID, max) {
		total += float64(p.N) * droneOutput(by[p.TypeID])
	}
	return total
}

// shipBayDrones reads the combat and mining drones in a ship's drone bay
// (flag 87) with each type's per-drone output.
func (m *Mod) shipBayDrones(ctx context.Context, shipID int64) ([]botDrone, error) {
	if shipID == 0 {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, t.groupID, SUM(e.quantity)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.locationID = ? AND e.flag = 87 AND t.groupID IN (?, ?)
		GROUP BY e.typeID, t.groupID`,
		shipID, grpCombatDrone, grpMiningDrone)
	if err != nil {
		return nil, fmt.Errorf("shipBayDrones: %w", err)
	}
	var bay []botDrone
	var ids []int64
	for rows.Next() {
		var d botDrone
		if err := rows.Scan(&d.TypeID, &d.GroupID, &d.Units); err != nil {
			rows.Close()
			return nil, fmt.Errorf("shipBayDrones scan: %w", err)
		}
		bay = append(bay, d)
		ids = append(ids, d.TypeID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(bay) == 0 {
		return nil, nil
	}
	attrs, err := evedb.TypeAttrsMany(ctx, m.db, ids)
	if err != nil {
		return nil, fmt.Errorf("shipBayDrones attrs: %w", err)
	}
	for i := range bay {
		bay[i].DPS, bay[i].MiningM3PerSec = droneOutputFromAttrs(bay[i].GroupID, attrs[bay[i].TypeID])
	}
	return bay, nil
}

// droneOutputFromAttrs reads one drone's output off its type attributes, the fields the core
// reads for a drone in space: a combat drone cycles on attribute 51 and hits for the four
// damage channels; a mining drone takes attribute 77 m3 per attribute-73 cycle (fitstats.go).
func droneOutputFromAttrs(groupID int64, a map[int64]float64) (dps, minePerSec float64) {
	if a == nil {
		return 0, 0
	}
	if groupID == grpMiningDrone {
		if cycle := a[attrDuration]; cycle > 0 {
			return 0, a[attrMiningAmount] / (cycle / 1000)
		}
		return 0, 0
	}
	cycle := a[attrRateOfFire]
	if cycle <= 0 {
		return 0, 0
	}
	dmg := a[attrEmDamage] + a[attrExplosiveDamage] + a[attrKineticDamage] + a[attrThermalDamage]
	if mult := a[attrDamageMult]; mult > 0 {
		dmg *= mult
	}
	return dmg / (cycle / 1000), 0
}

// droneLaunchCap is how many drones this pilot may have in space: one per
// level of Drones, exactly the core's droneMaxActive.
func (m *Mod) droneLaunchCap(ctx context.Context, charID int64) int64 {
	skills, err := m.charSkillsFor(ctx, charID)
	if err != nil || skills == nil {
		return 0
	}
	return int64(skills[skillDrones])
}
