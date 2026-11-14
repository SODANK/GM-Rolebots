package rolebots

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// Drones are filled by a separate step, not a modRole, for three reasons: serves() opens with a
// Slot != band test and every drone has Slot 0; the budget is cubic metres, not slots, which
// bestForWish has no concept of; and a drone never competes with a module for anything.
// fillDroneBay and the trimmer are the only writers of fit.Drones.
//
// Mechanics: attribute 352 maxActiveDrones is on no hull and bandwidth does not exist, so a rack
// is bounded by bay volume (attribute 283) and the pilot's Drones skill. The rack is what the bay
// can hold, up to five, because a frigate's bay is 5 to 15 m3 and cannot take five.

// Drone categories and the groups worth carrying.
const (
	catDroneID = 18

	grpCombatDrone = 100
	grpMiningDrone = 101

	// The launch cap is skillDrones (skillsupport.go): one drone in space per level. ComputeFitStats
	// works on an all-V basis, so the template carries as much of a rack of five as the bay holds and
	// the per-pilot trim (fiteffective.go) lowers it to what this pilot can fly.

	// droneSpareSets is how many replacement racks ride along in the bay. One covers a bad fight;
	// more would lengthen the shopping list for a pilot who re-shops at home anyway.
	droneSpareSets = 1

	// droneAskCapMult refuses a drone whose cheapest ask anywhere is more than this many times its
	// basePrice. Bot-built drones list at their own cost-plus, and a flight priced that high would
	// make the selector read the fit as unaffordable and drop the hull, not just the drones.
	droneAskCapMult = 3.0
)

// marketDrone is one buyable drone: its cost in bay space and what it brings. It is not a
// marketModule: a drone has no slot, CPU or powergrid, and those fields would invite it into
// bestForWish.
type marketDrone struct {
	TypeID  int64
	Name    string
	GroupID int64
	Ask     float64
	// BasePrice is invTypes.basePrice, what droneAskCapMult is read against.
	// Zero means unknown and turns the guard off for that type.
	BasePrice float64
	VolumeM3  float64
	// Output in whatever unit the group's job cares about: damage per second
	// for a combat drone, m3 per minute for a mining one. Only ever compared
	// within one group, so the units never meet.
	Output float64
}

var (
	droneCatMu  sync.Mutex
	droneCatAt  time.Time
	droneCatAll []*marketDrone
)

// buyableDrones is every published drone with a live sell order anywhere, in the two groups the
// simulation can price. Scope and cache TTL match buyableModules. marketGroupID > 0 filters out
// test items with no market group, as hullCandidateWhere does for hulls.
func (m *Mod) buyableDrones(ctx context.Context) ([]*marketDrone, error) {
	droneCatMu.Lock()
	if droneCatAll != nil && clock.Now().Sub(droneCatAt) < fitGenMarketTTL {
		out := droneCatAll
		droneCatMu.Unlock()
		return out, nil
	}
	droneCatMu.Unlock()

	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, MIN(o.price), t.basePrice, t.volume
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = ?
		JOIN market_orders o ON o.typeID = t.typeID
		     AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		WHERE t.published = 1 AND t.marketGroupID > 0 AND t.groupID IN (?, ?)
		GROUP BY t.typeID, t.typeName, t.groupID, t.basePrice, t.volume`,
		catDroneID, grpCombatDrone, grpMiningDrone)
	if err != nil {
		return nil, fmt.Errorf("buyableDrones: %w", err)
	}
	var list []*marketDrone
	ids := make([]int64, 0, 64)
	for rows.Next() {
		d := &marketDrone{}
		if err := rows.Scan(&d.TypeID, &d.Name, &d.GroupID, &d.Ask, &d.BasePrice, &d.VolumeM3); err != nil {
			rows.Close()
			return nil, fmt.Errorf("buyableDrones scan: %w", err)
		}
		list = append(list, d)
		ids = append(ids, d.TypeID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	if err := m.fillDroneOutput(ctx, list, ids); err != nil {
		return nil, err
	}

	droneCatMu.Lock()
	droneCatAll, droneCatAt = list, clock.Now()
	droneCatMu.Unlock()
	return list, nil
}

// fillDroneOutput reads each drone's headline number in the unit ComputeFitStats uses, so the
// generator and the stat engine rank drones alike. A combat drone cycles on attribute 51 and a
// mining drone on 73; reading a mining drone on 51 leaves its yield at zero (see fitstats.go).
func (m *Mod) fillDroneOutput(ctx context.Context, list []*marketDrone, ids []int64) error {
	// Attributes read below: 51 combat cycle, 73 mining cycle, 77 m3 per
	// mining cycle, 64 damage multiplier, 114-118 the four damage channels.
	attrs, err := evedb.TypeAttrsMany(ctx, m.db, ids)
	if err != nil {
		return fmt.Errorf("fillDroneOutput: %w", err)
	}
	for _, d := range list {
		a := attrs[d.TypeID]
		if a == nil {
			continue
		}
		if d.GroupID == grpMiningDrone {
			cycle := a[attrDuration]
			if cycle > 0 {
				d.Output = a[attrMiningAmount] / (cycle / 1000) * 60
			}
			continue
		}
		cycle := a[attrRateOfFire]
		if cycle <= 0 {
			continue
		}
		dmg := a[attrEmDamage] + a[attrExplosiveDamage] + a[attrKineticDamage] + a[attrThermalDamage]
		if mult := a[attrDamageMult]; mult > 1 {
			dmg *= mult
		}
		d.Output = dmg / (cycle / 1000)
	}
	return nil
}

// buyableDronesWithin narrows the catalogue to what is sold inside this pilot's buy cap, as
// buyableModulesWithin does for modules. A read failure returns the whole catalogue: an
// unreadable scope is ignorance, not scarcity.
func (m *Mod) buyableDronesWithin(ctx context.Context, reach map[int64]bool) ([]*marketDrone, error) {
	all, err := m.buyableDrones(ctx)
	if err != nil || len(reach) == 0 || len(all) == 0 {
		return all, err
	}
	sold, err := m.typesSoldInScope(ctx, reach, catDroneID)
	if err != nil {
		return all, nil
	}
	out := make([]*marketDrone, 0, len(all))
	for _, d := range all {
		if sold[d.TypeID] {
			out = append(out, d)
		}
	}
	return out, nil
}

// fillDroneBay puts drones in the bay, sized to the hull's cubic metres and what the pilot can
// use. A miner gets mining drones and every other job combat drones. The test is the job, not
// the hull: jobPower(JobMining) counts only yield, so a combat drone on a barge would score zero.
func (m *Mod) fillDroneBay(ctx context.Context, fit *Fit, h *HullProfile, job string,
	skills map[int64]int, cat []*marketDrone, own *ownedLoadout, taste fitTaste) error {

	bay := h.DroneCapacity
	if bay <= 0 || len(cat) == 0 {
		return nil
	}
	wantGroup := int64(grpCombatDrone)
	if job == JobMining {
		wantGroup = grpMiningDrone
	}

	// Best rack the bay can hold: output times as many as can be in space (bay room, capped at
	// droneMaxLaunched), which is what DroneDPS counts.
	var best *marketDrone
	var bestScore float64
	var bestRack int64
	for _, d := range cat {
		if d.GroupID != wantGroup || d.Output <= 0 || d.VolumeM3 <= 0 {
			continue
		}
		rack := droneRackFor(bay, d.VolumeM3)
		if rack <= 0 {
			continue
		}
		if droneAskGouged(d) {
			continue
		}
		ok, err := m.pilotCanUse(ctx, d.TypeID, skills)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		// The pilot's own preference, which also breaks ties between modules, so two pilots in one hull
		// do not fly identical drones.
		score := d.Output * float64(rack) * taste.forType(d.TypeID)
		// An incumbent keeps the bay unless a challenger is clearly better (as fitsticky.go), so a
		// seller running dry does not trigger a trip to swap drones for drones.
		if own.hasDrone(d.TypeID) {
			score /= fitGenStickiness
		}
		// A tie goes to the smaller drone: the same output over more hulls survives a loss better.
		if score > bestScore || (score == bestScore && best != nil && d.VolumeM3 < best.VolumeM3) {
			best, bestScore, bestRack = d, score, rack
		}
	}
	if best == nil {
		return nil
	}

	// The rack plus its spares, never more than the bay holds.
	qty := bestRack * (1 + droneSpareSets)
	if room := droneBayRoom(bay, best.VolumeM3); qty > room {
		qty = room
	}
	if qty <= 0 {
		return nil
	}
	fit.Drones = append(fit.Drones, FitCargo{
		TypeID: best.TypeID, TypeName: best.Name, Qty: qty,
	})
	return nil
}

// droneBayRoom is how many drones of one volume a bay holds. The epsilon keeps
// a 15 m3 bay holding three 5 m3 drones when the division lands a hair under.
func droneBayRoom(bay, vol float64) int64 {
	if bay <= 0 || vol <= 0 {
		return 0
	}
	return int64(math.Floor(bay/vol + 1e-9))
}

// droneRackFor is how many of one drone can be in space at once off this bay:
// what the bay holds, capped at droneMaxLaunched (the all-V launch count the
// stat engine scores).
func droneRackFor(bay, vol float64) int64 {
	n := droneBayRoom(bay, vol)
	if n > droneMaxLaunched {
		n = droneMaxLaunched
	}
	return n
}

// droneAskGouged reports whether the cheapest ask anywhere for this drone is more than
// droneAskCapMult times its basePrice. An unknown basePrice gouges nothing.
func droneAskGouged(d *marketDrone) bool {
	return d.BasePrice > 0 && d.Ask > droneAskCapMult*d.BasePrice
}
