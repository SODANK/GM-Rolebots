package rolebots

// Bot courier contract acceptance and delivery.
// A bot scans the available courier contracts, filters by volume, collateral and route,
// picks the best reward per jump, flies to the pickup, accepts, hauls and delivers.
// The collateral is real ISK at risk, so courier tasks are non-interruptible.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	evedb "gurimoon/db"
)

// ── activity & config ─────────────────────────────────────────────────────

const ActCourier = "courier"

// openCouriers returns every posted contract in the game, shared across the roster for a
// minute. The question is table-wide, so asking it per pilot would repeat the same query.
func (m *Mod) openCouriers(ctx context.Context) ([]evedb.CourierListing, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	shared, err := snapshotFor(snapKey("couriers-open"), time.Minute, func() ([]evedb.CourierListing, error) {
		return evedb.ListAllAvailableCouriers(sctx, m.db)
	})
	if err != nil {
		return nil, err
	}
	out := make([]evedb.CourierListing, len(shared))
	copy(out, shared)
	return out, nil
}

// courierAcceptMinISKPerJump is the default minimum reward per jump to accept a contract.
// The server_settings key courier.acceptMinISKPerJump overrides it.
const courierAcceptMinISKPerJump = 8000.0

// courierMaxJumps is the maximum total jumps (pickup to drop) a bot will accept.
const courierMaxJumps = 30

// ── opportunity probe ─────────────────────────────────────────────────────

// courierOpportunity checks whether any available courier contract passes this bot's
// filters. It returns a value above 0 if something is worth hauling.
func courierOpportunity(m *Mod, ctx context.Context, b *botRun) (float64, string) {
	if b.StationID == 0 {
		return 0, "in space"
	}

	// A trade ship is needed. canWorkJob checks skills and grid with no wallet and no side
	// effects; selectFitForActivity would log an upgrade and return a purchase proposal.
	if ok, err := m.canWorkJob(ctx, b.CharID, JobTrade); err != nil || !ok {
		return 0, "no hauler fit"
	}

	contracts, err := m.openCouriers(ctx)
	if err != nil {
		return 0, "db error"
	}
	if len(contracts) == 0 {
		return 0, "no contracts available"
	}

	// Read the bot's balance and cargo capacity.
	var balance float64
	m.db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`,
		b.CharID).Scan(&balance)
	spend := spendable(balance)

	cargoM3 := m.shipCargoCapacity(ctx, b)
	if cargoM3 <= 0 {
		// Estimate when the ship is not boarded.
		cargoM3 = 5000
	}

	minISK := courierAcceptMinISKPerJump
	if vs, ok, _ := evedb.GetServerSetting(ctx, m.db, "courier.acceptMinISKPerJump"); ok {
		if v, err := strconv.ParseFloat(vs, 64); err == nil && v > 0 {
			minISK = v
		}
	}

	best := 0.0
	var nSelf, nVol, nCollat, nRoute, nPay int
	for _, c := range contracts {
		if c.IssuerID == b.CharID {
			nSelf++
			continue
		}
		if c.Volume > cargoM3 {
			nVol++
			continue
		}
		if c.Collateral > spend {
			nCollat++
			continue
		}
		jumps := m.courierJumpsBetween(ctx, b, c.PickupStationID, c.DropStationID)
		if jumps <= 0 || jumps > courierMaxJumps {
			nRoute++
			continue
		}
		iskPerJump := c.TotalReward / float64(jumps)
		if iskPerJump < minISK {
			nPay++
			continue
		}
		if iskPerJump > best {
			best = iskPerJump
		}
	}
	if best <= 0 {
		return 0, fmt.Sprintf("no contracts worth taking (%d collat/%d vol/%d route/%d pay)", nCollat, nVol, nRoute, nPay)
	}
	return 1.0 + best/100000, fmt.Sprintf("%.0f ISK/jump best contract", best)
}

// ── planner ───────────────────────────────────────────────────────────────

// courierAcceptPlan picks the best available contract and starts travel to the pickup
// station.
func (m *Mod) courierAcceptPlan(ctx context.Context, b *botRun) error {
	st := b.State
	st.Activity = ActCourier

	// A trade ship is required.
	ready, err := m.ensureActivityShip(ctx, b, ActTrade)
	if err != nil {
		return err
	}
	if !ready {
		return nil // procurement kicked off
	}

	contracts, err := m.openCouriers(ctx)
	if err != nil {
		return fmt.Errorf("listing courier contracts: %w", err)
	}

	var balance float64
	m.db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`,
		b.CharID).Scan(&balance)
	spend := spendable(balance)

	cargoM3 := m.shipCargoCapacity(ctx, b)
	if cargoM3 <= 0 {
		cargoM3 = 5000
	}

	minISK := courierAcceptMinISKPerJump
	if vs, ok, _ := evedb.GetServerSetting(ctx, m.db, "courier.acceptMinISKPerJump"); ok {
		if v, err := strconv.ParseFloat(vs, 64); err == nil && v > 0 {
			minISK = v
		}
	}

	type candidate struct {
		listing    evedb.CourierListing
		jumps      int
		iskPerJump float64
	}
	var best *candidate

	// Skip contracts other pilots are already flying for: the accept happens at the pickup
	// station, so all but the first pilot would arrive to find it taken.
	claimed := m.claimedCouriers(ctx, b.CharID)
	// The pilot's own horizon (reach) is the reachability test and gives the length of the
	// leg to the pickup, which the score includes.
	reach, err := m.reachFrom(ctx, b.SystemID, rulesForBot(b))
	if err != nil {
		return err
	}
	stationSys := m.courierStationSystems(ctx, contracts)

	for _, c := range contracts {
		if c.IssuerID == b.CharID {
			continue
		}
		if claimed[c.MissionID] != 0 {
			continue
		}
		if c.Volume > cargoM3 {
			continue
		}
		if c.Collateral > spend {
			continue
		}
		pickupJumps, ok := reach[stationSys[c.PickupStationID]]
		if !ok {
			continue
		}
		// The route (a Dijkstra per call) runs only for what survived the cheap gates above.
		jumps := m.courierJumpsBetween(ctx, b, c.PickupStationID, c.DropStationID)
		if jumps <= 0 || jumps > courierMaxJumps {
			continue
		}
		iskPerJump := c.TotalReward / float64(jumps+pickupJumps)
		if iskPerJump < minISK {
			continue
		}
		if best == nil || iskPerJump > best.iskPerJump {
			best = &candidate{listing: c, jumps: jumps, iskPerJump: iskPerJump}
		}
	}

	if best == nil {
		return fmt.Errorf("%w: no courier contracts pass the filters", errNoFitAvailable)
	}

	// Record the contract being pursued.
	st.Data.CourierMissionID = best.listing.MissionID
	st.Data.CourierPickupStation = best.listing.PickupStationID
	st.Data.CourierDropStation = best.listing.DropStationID
	st.Data.CourierReward = best.listing.TotalReward
	st.Data.CourierCollateral = best.listing.Collateral
	st.Data.CourierVolumeM3 = best.listing.Volume

	m.logBot(b.CharID, logInfo, "courier",
		"accepting contract %d: %.0f ISK reward, %.0f ISK collateral, %.0f m³, %d jumps (%.0f ISK/jump)",
		best.listing.MissionID, best.listing.TotalReward, best.listing.Collateral,
		best.listing.Volume, best.jumps, best.iskPerJump)

	// Resolve the pickup station's system.
	pickupSysID := m.stationSystemOf(ctx, best.listing.PickupStationID)

	// Travel to the pickup station.
	return m.beginTravel(ctx, b, pickupSysID, best.listing.PickupStationID,
		string(TaskCourierPickup))
}

// ── steps ─────────────────────────────────────────────────────────────────

// courierStep dispatches the current courier phase.
func (m *Mod) courierStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	now := clock.Unix()

	switch TaskKind(st.Phase) {
	case TaskCourierPickup:
		// Docked at pickup: accept the contract.
		if b.StationID != d.CourierPickupStation {
			// Not at the right station: travel there.
			sysID := m.stationSystemOf(ctx, d.CourierPickupStation)
			return true, m.beginTravel(ctx, b, sysID, d.CourierPickupStation,
				string(TaskCourierPickup))
		}

		corpID, _ := evedb.GetCharCorpID(ctx, m.db, b.CharID)
		crateID, err := evedb.AcceptCourierMissionDB(ctx, m.db,
			d.CourierMissionID, b.CharID, corpID, true)
		if err != nil {
			m.logBot(b.CharID, logError, "courier",
				"accept mission %d failed: %v — aborting", d.CourierMissionID, err)
			// Contract gone (someone else took it): go idle.
			m.clearCourierState(st)
			return true, saveState(ctx, m.db, st)
		}

		d.CourierCrateID = crateID
		// The package travels with the bot: delivery checks that it is at the drop station.
		// Selection already capped the contract's volume at this hull's hold.
		if err := m.moveItemToCargo(ctx, b, crateID); err != nil {
			m.logBot(b.CharID, logError, "courier",
				"accepted contract %d but could not load package %d: %v", d.CourierMissionID, crateID, err)
		}
		m.logBot(b.CharID, logInfo, "courier",
			"accepted contract %d, crate %d — hauling to drop", d.CourierMissionID, crateID)

		// Dwell briefly, then travel to the drop station.
		st.Phase = string(TaskCourierHaul)
		d.LegEndsAt = now + 15 // 15s dwell at pickup
		return true, saveState(ctx, m.db, st)

	case TaskCourierHaul:
		// Waiting at pickup before departure.
		if now < d.LegEndsAt {
			return false, nil
		}
		// Travel to the drop station.
		dropSysID := m.stationSystemOf(ctx, d.CourierDropStation)
		return true, m.beginTravel(ctx, b, dropSysID, d.CourierDropStation,
			string(TaskCourierDeliver))

	case TaskCourierDeliver:
		// Docked at drop: deliver the package.
		if b.StationID != d.CourierDropStation {
			sysID := m.stationSystemOf(ctx, d.CourierDropStation)
			return true, m.beginTravel(ctx, b, sysID, d.CourierDropStation,
				string(TaskCourierDeliver))
		}

		err := evedb.DeliverCourierMissionDB(ctx, m.db,
			d.CourierMissionID, b.CharID, b.StationID)
		if err != nil {
			m.logBot(b.CharID, logError, "courier",
				"deliver mission %d failed: %v", d.CourierMissionID, err)
			// Contract gone: go idle.
			m.clearCourierState(st)
			return true, saveState(ctx, m.db, st)
		}

		m.logBot(b.CharID, logInfo, "courier",
			"delivered contract %d — earned %.0f ISK reward + %.0f ISK collateral returned",
			d.CourierMissionID, d.CourierReward, d.CourierCollateral)

		m.clearCourierState(st)
		return true, saveState(ctx, m.db, st)

	default:
		rlogf("rolebots: courier: unknown phase %q for charID=%d", st.Phase, b.CharID)
		m.clearCourierState(st)
		return true, saveState(ctx, m.db, st)
	}
}

// clearCourierState resets the courier fields and returns to idle.
func (m *Mod) clearCourierState(st *SimState) {
	st.Activity = ""
	st.Phase = string(TaskIdle)
	st.Data.CourierMissionID = 0
	st.Data.CourierPickupStation = 0
	st.Data.CourierDropStation = 0
	st.Data.CourierReward = 0
	st.Data.CourierCollateral = 0
	st.Data.CourierVolumeM3 = 0
	st.Data.CourierCrateID = 0
}

// ── helpers ───────────────────────────────────────────────────────────────

// shipCargoCapacity returns the cargo capacity of the bot's current ship in m3, the
// fitted number the core checks a player's cargo move against. dgmTypeAttributes 38 alone
// is not enough: most hulls carry the value only as invTypes.capacity.
func (m *Mod) shipCargoCapacity(ctx context.Context, b *botRun) float64 {
	if b.ShipID == 0 {
		return 0
	}
	var shipTypeID int64
	if m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, b.ShipID).Scan(&shipTypeID) != nil {
		return 0
	}
	cap, _ := evedb.GetShipCargoCapacityFitted(ctx, m.db, shipTypeID, b.ShipID)
	return cap
}

// courierStationSystems resolves every pickup station in the listing to its solar system
// in one query, so the plan's loop reads a map instead of querying per contract.
func (m *Mod) courierStationSystems(ctx context.Context, contracts []evedb.CourierListing) map[int64]int64 {
	out := map[int64]int64{}
	if len(contracts) == 0 {
		return out
	}
	ids := make([]int64, 0, len(contracts))
	seen := map[int64]bool{}
	for _, c := range contracts {
		if !seen[c.PickupStationID] {
			seen[c.PickupStationID] = true
			ids = append(ids, c.PickupStationID)
		}
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT stationID, COALESCE(solarSystemID, 0) FROM staStations WHERE stationID IN (`+
			placeholders(len(ids))+`)`, int64Args(ids)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st, sys int64
		if rows.Scan(&st, &sys) == nil && sys != 0 {
			out[st] = sys
		}
	}
	return out
}

// courierJumpsBetween returns the jump count between two stations using the
// bot's own travel rules. A Dijkstra per call: run it only for contracts
// that have passed every cheap gate.
func (m *Mod) courierJumpsBetween(ctx context.Context, b *botRun, fromStation, toStation int64) int {
	fromSys, toSys := m.stationSystemOf(ctx, fromStation), m.stationSystemOf(ctx, toStation)
	if fromSys == 0 || toSys == 0 {
		return -1
	}
	if fromSys == toSys {
		return 0
	}
	path, err := m.route(ctx, fromSys, toSys, rulesForBot(b))
	if err != nil || len(path) == 0 {
		return -1
	}
	return len(path)
}
