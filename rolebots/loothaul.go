package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// A haul nobody sells. The loot ledger (rolebot_loot_hold, beltloot.go) is read
// back by the combat planner, so a pilot that changes career keeps its haul.
// lootHaulDue is the career-agnostic half, a row on the planner ladder
// (plan_ladder.go). A hold mostly full of loot goes where combatStartSale would
// send it, unless the pilot is already at that market. A haul older than
// lootHaulStaleSec is sold at the dock the pilot is in. Fleet members are left
// alone until the fleet stands down. Whether a pilot carries a ledger is
// remembered (lootCarriers), so most passes cost nothing.

// lootHaulStaleSec is how long a haul may ride before it is sold wherever the
// pilot docks: longer than a belt sweep's stint, so a hunter's own sweep-over
// sale wins, short enough that a career change does not carry it into next week.
const lootHaulStaleSec = 6 * 3600

// lootCarriers remembers which pilots hold a loot ledger: charID to bool. Absent
// means not looked up yet. A stale true costs one query and corrects itself.
var lootCarriers sync.Map

func init() {
	registerCharForget("lootCarriers", func(id int64) { lootCarriers.Delete(id) })
}

func noteLootCarrier(charID int64, has bool) { lootCarriers.Store(charID, has) }

// carriesLoot reports whether a pilot may hold a loot ledger. A failed read
// is not remembered, so the next pass asks again.
func (m *Mod) carriesLoot(ctx context.Context, charID int64) bool {
	if v, ok := lootCarriers.Load(charID); ok {
		return v.(bool)
	}
	var one int
	err := m.db.QueryRowContext(ctx,
		`SELECT 1 FROM rolebot_loot_hold WHERE characterID = ? AND units > 0 LIMIT 1`,
		charID).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		noteLootCarrier(charID, false)
		return false
	case err != nil:
		return false
	}
	noteLootCarrier(charID, true)
	return true
}

// lootHaulAct is what lootHaulDue does with a haul.
type lootHaulAct int

const (
	lootHaulKeep lootHaulAct = iota // leave it aboard
	lootHaulTrip                    // take it to market (combatStartSale)
	lootHaulHere                    // sell it at this dock
)

// lootHaulMove is the rule, apart from the reads. A full hold goes to market when
// a better one exists and sells here otherwise; a stale haul sells here; anything
// else rides along. A pilot in space waits for a dock unless the hold is full and
// a market is worth the trip.
func lootHaulMove(full, stale, docked, hubElsewhere bool) lootHaulAct {
	switch {
	case full && hubElsewhere:
		return lootHaulTrip
	case (full || stale) && docked:
		return lootHaulHere
	}
	return lootHaulKeep
}

// lootHaulDue is the ladder row (plan_ladder.go). Handled only when it sends
// the pilot on a trip; a sale at this dock takes no time and the pilot goes on
// to plan its own work.
func (m *Mod) lootHaulDue(ctx context.Context, b *botRun) (bool, error) {
	if b.ShipID == 0 || !m.carriesLoot(ctx, b.CharID) {
		return false, nil
	}
	if m.gangHoldsThisPilot(ctx, b) {
		return false, nil
	}

	var stacks int
	var oldest int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MIN(takenAt), 0) FROM rolebot_loot_hold
		WHERE characterID = ? AND units > 0`, b.CharID).Scan(&stacks, &oldest); err != nil {
		return false, fmt.Errorf("loot ledger age: %w", err)
	}
	if stacks == 0 {
		noteLootCarrier(b.CharID, false)
		return false, nil
	}

	full, err := m.holdMostlyFull(ctx, b)
	if err != nil {
		return false, err
	}
	stale := clock.Unix()-oldest >= lootHaulStaleSec
	hubElsewhere := false
	if full {
		station, _, _ := m.lootMarketFor(ctx, b)
		hubElsewhere = station != 0 && station != b.StationID
	}

	switch lootHaulMove(full, stale, b.StationID != 0, hubElsewhere) {
	case lootHaulTrip:
		return true, m.combatStartSale(ctx, b, "carrying a haul nobody has sold")
	case lootHaulHere:
		if _, _, _, _, err := m.sellLootLedgerHere(ctx, b, "market"); err != nil {
			return false, err
		}
		m.clearLootLedger(ctx, b.CharID)
		st := b.State
		st.Data.LootDecidedAt, st.Data.LootDonateOffice, st.Data.LootDonateStation = 0, 0, 0
		return false, saveState(ctx, m.db, st)
	}
	return false, nil
}
