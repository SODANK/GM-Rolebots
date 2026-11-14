package rolebots

import (
	"context"
	"fmt"
	"math"
)

// A human Fueler. A Fuel Technician seat held by a player keeps its title and roles, and the
// corporation's fuel still has to arrive, so the seat's runs are offered to the human as an
// assignment (corpassign.go): bring the tower at X up to N days of fuel, the fuel waiting in
// the staging office's Fuel division, paid per fuel-day batch delivered.
// Progress is read off the tower itself (the bay's whole batches plus the days burned since
// acceptance, against what it held at the offer), so hauling with the client's own cargo
// window is credited as a bot leg would be. While an accepted assignment is open the tower's
// bot fuel leg is held back.

const (
	assignFuel = "fuel"
	// fuelBatchWageMin is the least a batch pays a human carrier.
	fuelBatchWageMin = 150000
)

// fuelAssignmentText says what a fuel assignment asks.
func (m *Mod) fuelAssignmentText(ctx context.Context, a assignment) string {
	return fmt.Sprintf("Fuel our tower in %s: %d day(s) of fuel from the Fuel division of our office at %s into its bay",
		systemNameOf(ctx, m, a.SystemID), a.Qty, m.stationLabel(ctx, a.StationID))
}

// openFuelAssignment is a plan's open fuel assignment, nil for none.
func (m *Mod) openFuelAssignment(ctx context.Context, planID int64) *assignment {
	as, err := m.queryAssignments(ctx, `WHERE kind = ? AND workID = ? AND status IN ('offered','accepted') LIMIT 1`, assignFuel, planID)
	if err != nil || len(as) == 0 {
		return nil
	}
	return &as[0]
}

// offerFuelAssignment hands a human Fueler the tower's run.
func (m *Mod) offerFuelAssignment(ctx context.Context, c *corpCEO, p *towerPlan, batches, held int64, batchM3 float64) {
	if !assignmentsOn() || batches <= 0 || m.openFuelAssignment(ctx, p.PlanID) != nil {
		return
	}
	jumps := 1
	if u, err := m.universe(ctx); err == nil && u != nil {
		if j, ok := bfsJumps(u, m.stationSystemID(ctx, p.StagingStationID), 40)[p.SystemID]; ok {
			jumps = max(j, 1)
		}
	}
	perBatch := math.Max(fuelBatchWageMin, batchM3*float64(jumps)*haulISKPerM3Jump)
	a := assignment{CorpID: c.CorpID, CharID: p.FuelerID, Kind: assignFuel, TypeID: p.TowerTypeID, Qty: batches,
		StationID: p.StagingStationID, SystemID: p.SystemID, WorkID: p.PlanID, Division: int(held),
		Reward: math.Ceil(perBatch * float64(batches)), Status: asOffered, OfferedAt: clock.Unix()}
	a.OfferedBy, _ = m.letterFrom(ctx, c, mailFromInfrastructure)
	a.MeritPts = assignMerit(a.Reward)
	id, err := m.insertAssignment(ctx, a)
	if err != nil {
		return
	}
	a.ID = id
	members, _ := m.corpMembers(ctx, c.CorpID, c.CharID)
	for _, cm := range members {
		if cm.CharID == p.FuelerID {
			m.mailAssignmentOffer(ctx, c, cm, a, m.corpNameOf(ctx, c.CorpID))
			break
		}
	}
	m.ceoLog(ctx, c, powerStarbase, "assign", p.FuelerID, a.Reward,
		"offered %s the fuel run: %s, for %s", m.charNameOr(ctx, p.FuelerID), m.fuelAssignmentText(ctx, a), iskString(a.Reward))
}

// progressFuelAssignment credits an accepted fuel assignment from the bay.
func (m *Mod) progressFuelAssignment(ctx context.Context, a *assignment, heldNow int64) {
	if a == nil || a.Status != asAccepted {
		return
	}
	burned := int64(0)
	if a.AcceptedAt > 0 {
		burned = (clock.Unix() - a.AcceptedAt) / 86400
	}
	delivered := heldNow + burned - int64(a.Division)
	if delivered > a.Progress {
		m.advanceAssignment(ctx, *a, delivered-a.Progress)
	}
}
