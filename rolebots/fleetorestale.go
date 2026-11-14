package rolebots

// Orphaned fleet-ore holds: rows whose owner can never come back for them.
// rolebot_ore_hold is keyed by character so a live pilot's haul is disposed of wherever
// the commander docks (orestrays.go sellHangarOre). A hold whose owner is gone (a purged
// bot, a deleted character) is never disposed of, since nothing keys the ledger to the
// roster. This pass removes only those rows; a row for a pilot still on the roster is left
// alone however old it is.

import (
	"context"
	"time"
)

const (
	// fleetOreStaleSweepEveryTicks is the cadence (one tick is a second). The sweep is a
	// single anti-join that is almost always empty, so an hour is ample.
	fleetOreStaleSweepEveryTicks = 3600

	// fleetOreStaleSec is how long an orphan waits before it is dropped: long enough that a
	// roster row being rewritten (a purge and reseed) cannot have its ore taken mid-flight.
	fleetOreStaleSec = 24 * 3600
)

// startFleetOreStaleSweep registers the pass.
func (m *Mod) startFleetOreStaleSweep() {
	m.registerPeriodic("rolebot-fleet-ore-stale", fleetOreStaleSweepEveryTicks, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m.sweepOrphanedFleetOre(ctx)
		return true
	})
}

// sweepOrphanedFleetOre drops holds belonging to pilots the roster no longer
// has. Reports how many rows went, for the log.
func (m *Mod) sweepOrphanedFleetOre(ctx context.Context) int64 {
	res, err := m.db.ExecContext(ctx, `
		DELETE h FROM rolebot_ore_hold h
		LEFT JOIN rolebot_characters c ON c.characterID = h.characterID
		WHERE h.takenAt < ? AND c.characterID IS NULL`,
		clock.Unix()-fleetOreStaleSec)
	if err != nil {
		rlogf("rolebots: orphaned fleet ore sweep: %v", err)
		return 0
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		rlogf("rolebots: dropped %d orphaned fleet-ore hold row(s) — no pilot on the roster owns them", n)
	}
	return n
}
