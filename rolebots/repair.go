package rolebots

import (
	"context"
	"fmt"
	"sync"

	evedb "gurimoon/db"
)

// Ship repair. Both combat tiers persist hull damage through the core's store
// (combat.go, combat_abstract.go) and read it back when the ship is next built.
// This file pays the repair bill with evedb.RepairItems, the function the repair
// shop RPC calls (server/repairsvc), so a bot pays the player rate. Three gates:
// docked, at a repair facility (RepairItems does not check, as with
// stationHasCloning in clone.go) and affordable with a reserve. It is a
// plan-step errand between jobs: a damaged bot at a station with no shop stays damaged.

// serviceRepair is the staServices row for Repair Facilities.
const serviceRepair = 4096

const (
	// repairMinDamage is the damage floor worth a transaction: fights leave
	// fractional scratches.
	repairMinDamage = 1.0
	// repairWalletFloor is ISK the pilot will not spend on repairs whatever the
	// quote says, the same float the skill planner keeps under book buying (skills.go).
	repairWalletFloor = 1_000_000.0
	// repairWalletShare caps one repair at this fraction of what the pilot holds
	// above the floor. A partly funded repair is valid (armour is mended before
	// structure), so a poor pilot comes again.
	repairWalletShare = 0.5
)

// repairBrokeTTL is how long a pilot who could not afford the yard is not quoted again, in
// seconds. repairUpkeep runs on every plan pass, and an idle pilot plans every few seconds.
const repairBrokeTTL = 900

var (
	repairBrokeMu    sync.Mutex
	repairBrokeUntil = map[int64]int64{} // charID → unix
)

// repairBroke reports whether this pilot was turned away for money inside repairBrokeTTL.
func repairBroke(charID int64) bool {
	now := clock.Unix()
	repairBrokeMu.Lock()
	defer repairBrokeMu.Unlock()
	return now < repairBrokeUntil[charID]
}

// noteRepairBroke remembers that this pilot could not afford the yard just now.
func noteRepairBroke(charID int64) {
	until := clock.Unix() + repairBrokeTTL
	repairBrokeMu.Lock()
	defer repairBrokeMu.Unlock()
	repairBrokeUntil[charID] = until
}

// sweepRepairBroke drops expired entries (registered on the cache sweep): a pilot that
// logged off broke never asks again, so the read path would keep its entry for ever.
func sweepRepairBroke() {
	now := clock.Unix()
	repairBrokeMu.Lock()
	defer repairBrokeMu.Unlock()
	for id, until := range repairBrokeUntil {
		if now >= until {
			delete(repairBrokeUntil, id)
		}
	}
}

func init() {
	registerCharForget("repairBrokeUntil", func(id int64) {
		repairBrokeMu.Lock()
		defer repairBrokeMu.Unlock()
		delete(repairBrokeUntil, id)
	})
	registerSweep("repairBroke", sweepRepairBroke)
}

// repairUpkeep mends the boarded ship when the pilot is docked at a repair
// facility and can afford it, and reports whether anything was spent. The cheap
// damage read comes first and skips a pilot with no ship or station.
func (m *Mod) repairUpkeep(ctx context.Context, b *botRun) (repaired bool, err error) {
	if b.StationID == 0 || b.ShipID == 0 {
		return false, nil
	}
	// Turned away for money a moment ago: the wallet has not changed enough to ask again.
	if repairBroke(b.CharID) {
		return false, nil
	}
	structDmg, armorDmg, err := evedb.GetShipCombatDamage(ctx, m.db, b.ShipID)
	if err != nil {
		return false, fmt.Errorf("stored damage: %w", err)
	}
	if structDmg+armorDmg < repairMinDamage {
		return false, nil
	}
	// Only where the service exists: RepairItems itself does not check.
	if !m.stationHasRepair(ctx, b.StationID) {
		return false, nil
	}

	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return false, fmt.Errorf("wallet: %w", err)
	}
	budget := (bal - repairWalletFloor) * repairWalletShare
	if budget <= 0 {
		noteRepairBroke(b.CharID)
		m.logBot(b.CharID, logInfo, "repair",
			"carrying %.0f points of damage and cannot afford the yard — flying it as it is",
			structDmg+armorDmg)
		return false, nil
	}

	spent, err := evedb.RepairItems(ctx, m.db, b.CharID, []int64{b.ShipID}, budget)
	if err != nil {
		return false, fmt.Errorf("repair: %w", err)
	}
	if spent <= 0 {
		return false, nil
	}
	// Read the damage back: the core funds only as far as the budget reaches, so
	// a half-paid repair must not be reported as whole.
	after := 0.0
	if s, a, rerr := evedb.GetShipCombatDamage(ctx, m.db, b.ShipID); rerr == nil {
		after = s + a
	}
	if after >= repairMinDamage {
		m.logBot(b.CharID, logInfo, "repair",
			"paid %.2f ISK at the yard — %.0f points still outstanding, that is what the wallet covered",
			spent, after)
	} else {
		m.logBot(b.CharID, logInfo, "repair",
			"repaired for %.2f ISK — hull whole again after %.0f points of damage",
			spent, structDmg+armorDmg)
	}
	return true, nil
}

// stationHasRepair reports whether a station offers Repair Facilities.
func (m *Mod) stationHasRepair(ctx context.Context, stationID int64) bool {
	var one int64
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM staStations s
		JOIN staOperationServices os ON os.operationID = s.operationID
		WHERE s.stationID = ? AND os.serviceID = ?
		LIMIT 1`, stationID, serviceRepair).Scan(&one)
	return err == nil
}
