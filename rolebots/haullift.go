package rolebots

import (
	"context"
	"math"
	"sort"
	"sync"

	"gurimoon/dogma"
	evedb "gurimoon/db"
)

// Heavy lifts: how much one pilot can carry in a single unit.
//
// A tower's kit holds single units no ordinary hauler takes (a Ship Maintenance
// Array or Large tower is 8,000 m3). Expanders compound with no stacking penalty,
// so the answer is arithmetic over the pilot's skills, not the hull it flies today
// (liftPotential). The Infrastructure arm is staffed by it (corparm_assign.go).
// Potential, not the fitted hold, ranks pilots: a pilot new to Infrastructure
// flies the trade fit, and ranking by that would keep the arm full of whoever
// already holds the seats.

const (
	skillGeneralFreight = int64(12834)
	attrCargoCapBonus   = int64(614) // cargoCapacityBonus, on General Freight
	attrReqSkill1       = int64(182)
	attrReqSkill1Lvl    = int64(277)
	typeExpandedCargoI  = int64(1317) // needs Hull Upgrades I
	typeBasicExpCargo   = int64(1315) // needs Mechanic I
	groupIndustrial     = int64(28)
)

// industrialHull is one hauler as liftPotential reads it.
type industrialHull struct {
	TypeID   int64
	Capacity float64
	Lows     int
	ReqSkill int64
	ReqLevel int
	// CargoSkill is the skill its cargo bonus scales with and CargoPct the
	// per-level percentage (0 for a hull with no cargo bonus).
	CargoSkill int64
	CargoPct   float64
}

// liftBook is the static half of the measure: the hauler catalogue and the
// multipliers the data gives the skills that move it.
type liftBook struct {
	Hulls      []industrialHull
	ExpT1      float64 // Expanded Cargohold I
	ExpBasic   float64 // Basic Expanded Cargohold
	FreightPct float64 // General Freight, per level
}

// liftPotential is the biggest single-unit hold a pilot with these skills can
// put under itself: over every industrial it may fly, capacity × its cargo
// bonus × the best expander it may fit in every low × General Freight. Pure.
func liftPotential(skills map[int64]int, b liftBook) float64 {
	exp := 1.0
	switch {
	case skills[skillHullUpgrades] >= 1 && b.ExpT1 > 0:
		exp = b.ExpT1
	case skills[skillMechanic] >= 1 && b.ExpBasic > 0:
		exp = b.ExpBasic
	}
	freight := 1 + b.FreightPct*float64(skills[skillGeneralFreight])/100
	best := 0.0
	for _, h := range b.Hulls {
		if h.ReqSkill != 0 && skills[h.ReqSkill] < h.ReqLevel {
			continue
		}
		bonus := 1.0
		if h.CargoSkill != 0 {
			bonus += h.CargoPct * float64(skills[h.CargoSkill]) / 100
		}
		v := h.Capacity * bonus * math.Pow(exp, float64(h.Lows)) * freight
		best = math.Max(best, v)
	}
	return best
}

var (
	liftBookMu   sync.Mutex
	liftBookMemo *liftBook
)

// liftCatalogue loads the liftBook once per process: static data.
func (m *Mod) liftCatalogue(ctx context.Context) liftBook {
	liftBookMu.Lock()
	defer liftBookMu.Unlock()
	if liftBookMemo != nil {
		return *liftBookMemo
	}
	b := liftBook{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, COALESCE(capacity, 0) FROM invTypes WHERE groupID = ? AND published = 1`, groupIndustrial)
	if err != nil {
		return b
	}
	for rows.Next() {
		var h industrialHull
		if rows.Scan(&h.TypeID, &h.Capacity) == nil && h.Capacity > 0 {
			b.Hulls = append(b.Hulls, h)
		}
	}
	rows.Close()
	for i := range b.Hulls {
		h := &b.Hulls[i]
		attrs, err := evedb.TypeAttrs(ctx, m.db, h.TypeID)
		if err != nil {
			continue
		}
		h.Lows = int(attrs[int64(attrLowSlots)])
		h.ReqSkill = int64(attrs[attrReqSkill1])
		h.ReqLevel = int(attrs[attrReqSkill1Lvl])
		// The hull's cargo bonus, by the core's own spec table, so the
		// measure and the hold the core reports cannot disagree.
		effects, err := evedb.GetTypeEffects(ctx, m.db, h.TypeID)
		if err != nil {
			continue
		}
		for _, te := range effects {
			for _, sp := range dogma.HullBonusSpecs[te.EffectName] {
				if sp.Target != dogma.HBCargo {
					continue
				}
				for _, a := range sp.Attrs {
					if v := attrs[a]; v != 0 {
						h.CargoSkill, h.CargoPct = sp.SkillID, v
						break
					}
				}
			}
		}
	}
	sort.Slice(b.Hulls, func(i, j int) bool { return b.Hulls[i].TypeID < b.Hulls[j].TypeID })
	if a, err := evedb.TypeAttrs(ctx, m.db, typeExpandedCargoI); err == nil {
		b.ExpT1 = a[attrCargoMult]
	}
	if a, err := evedb.TypeAttrs(ctx, m.db, typeBasicExpCargo); err == nil {
		b.ExpBasic = a[attrCargoMult]
	}
	if a, err := evedb.TypeAttrs(ctx, m.db, skillGeneralFreight); err == nil {
		b.FreightPct = a[attrCargoCapBonus]
	}
	if len(b.Hulls) > 0 {
		liftBookMemo = &b
	}
	return b
}

// liftOf is one pilot's lift potential, 0 when its skills cannot be read.
func (m *Mod) liftOf(ctx context.Context, charID int64) float64 {
	skills, err := m.charSkillsFor(ctx, charID)
	if err != nil || skills == nil {
		return 0
	}
	return liftPotential(skills, m.liftCatalogue(ctx))
}
