package rolebots

import "context"

// The settlement round that ends a mining fleet's op.
//
// The op ends with everyone at the leader's staging station and the books settled
// before the gang row is deleted. The only books left are ore (fleetore.go). A
// combat fleet has no books and disbands where it stands (sweepAutoGangs).

// settleGangOp runs the settlement round for a gang at its staging station. The
// leader's gang.settle step calls it once per gang; the caller deletes the gang
// on a nil return.
func (m *Mod) settleGangOp(ctx context.Context, g *Gang) error {
	// A draining industry fleet pays any ore balance left unpaid at delivery. No-op
	// for a co-op.
	m.payFleetOre(ctx, g)

	// A co-op commander disposes of its own share of the ore at the staging
	// station. A leader away from the counter is covered by its next home loop.
	if g.Activity == ActMining && g.LeaderCharID != 0 && g.State.StagingStationID != 0 {
		if lb, lerr := m.loadRun(ctx, g.LeaderCharID); lerr == nil &&
			lb.StationID == g.State.StagingStationID {
			m.disposeFleetOre(ctx, lb)
		}
	}
	return nil
}
