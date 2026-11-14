package rolebots

// The industry mining fleet (ActIndMining): a corp mining op that feeds the commander's
// factory. It is retired for new fleets (corpFleetActivities no longer deals the seat,
// migrateIndMiningSeats re-slugs old seats, formFleetAround refuses the slug) and the
// code stays to drain fleets in flight: payouts, dropoff and disband write-off. The
// ordinary mining fleet (ActMining) is a co-op where nobody buys rock (fleetore.go
// handBackFleetOre). See docs/comment-notes-b10.md#industry-mining-fleet.

import (
	"context"

	evedb "gurimoon/db"
)

const (
	// fleetOrePayoutBase is the friendly corp rate: the fraction of the ore's ideal refined
	// value a delivered unit pays. It is under 1.0 because the miner is spared the haul,
	// refine loss, listing wait and broker fee, and the leader takes the price risk.
	fleetOrePayoutBase = 0.85
	// fleetOrePayoutBend scales how far the leader's generosity (GangShareFrac, 0.03
	// grasping to 0.35 generous) moves the rate off the base. At 0.5: grasping 0.77,
	// generous 0.93.
	fleetOrePayoutBend = 0.5
	// The clamp keeps every personality inside a fair deal for both sides: never under 0.70
	// (miners would do better solo), never over 0.95 (the leader would do better shopping).
	fleetOrePayoutMin = 0.70
	fleetOrePayoutMax = 0.95

	// fleetOreLeaderWalletFloor is the least a commander keeps after a payout round. The
	// conditional debit in handISK checks each payment; the floor stops a round starting when
	// the wallet is already thin.
	fleetOreLeaderWalletFloor = 500_000.0
)

// fleetPaysForOre reports whether this fleet's commander buys its miners' rock. Only the
// retired industry fleet does; a co-op mining fleet hands each miner's rock back
// (fleetSharesOre). One definition serves the accrual, the payout and the dropoff. Pure.
func fleetPaysForOre(activity string) bool {
	return activity == ActIndMining
}

// fleetSharesOre reports whether this is a co-op mining fleet: miners fill their own
// cans, the hauler runs them in, and each miner's units are handed back at the dropoff to
// sell or refine on its own account.
func fleetSharesOre(activity string) bool {
	return activity == ActMining
}

// fleetAttributesOre reports whether the hauler must write down whose can every unit came
// out of. It is true for both shapes, since both settle per miner.
func fleetAttributesOre(activity string) bool {
	return fleetPaysForOre(activity) || fleetSharesOre(activity)
}

// fleetOreDropoff resolves where a mining fleet's haul lands: the commander's settled
// factory for the industry fleet (industryShortfall reads the leader's own flag-4
// hangar), or the staging station for a co-op (handBackFleetOre). All zeros means no
// target, and the haul falls through to the ordinary sell path.
func (m *Mod) fleetOreDropoff(ctx context.Context, g *Gang) (leaderID, stationID, sysID int64) {
	if !fleetAttributesOre(g.Activity) || g.LeaderCharID == 0 {
		return 0, 0, 0
	}
	switch g.Activity {
	case ActIndMining:
		prof, err := m.getProfile(ctx, g.LeaderCharID)
		if err != nil || prof == nil || prof.IndustryStationID == 0 {
			return 0, 0, 0
		}
		stationID = prof.IndustryStationID
	default:
		stationID = g.State.StagingStationID
		if stationID == 0 {
			stationID = m.homeStationFor(ctx, g.LeaderCharID)
		}
	}
	if stationID == 0 {
		return 0, 0, 0
	}
	if sysID = m.stationSystemOf(ctx, stationID); sysID == 0 {
		return 0, 0, 0
	}
	return g.LeaderCharID, stationID, sysID
}

// fleetOrePayoutFrac is the leader's rate: base plus generosity, clamped. Pure.
func fleetOrePayoutFrac(tn Tuning) float64 {
	f := fleetOrePayoutBase + (tn.GangShareFrac-gangShareFrac)*fleetOrePayoutBend
	if f < fleetOrePayoutMin {
		f = fleetOrePayoutMin
	}
	if f > fleetOrePayoutMax {
		f = fleetOrePayoutMax
	}
	return f
}

// fleetOreUnitValue is what one unit of each ore is worth as minerals at market at ideal
// yield, for every ore in the catalogue at once. Ideal because fleetOrePayoutBase already
// prices the refine loss, and a per-leader yield would pay two miners differently for the
// same rock. The caller resolves the fleet's region set once from its leader.
func (m *Mod) fleetOreUnitValue(ctx context.Context, regions []int64) map[int64]float64 {
	vals, err := m.oreValues(ctx, regions, oreValueAsk)
	if err != nil {
		return nil
	}
	out := make(map[int64]float64, len(vals))
	for id, v := range vals {
		out[id] = oreRefinedValue(v, 1.0)
	}
	return out
}

// recordFleetOre credits loaded can units to their miner's unpaid balance. The hauler's
// load step passes the can owner's ID (the can registry is the attribution) and the
// hauler's own ID, because the claim rides in that hauler's hold and is settled only by
// its drop (handBackFleetOre).
func (m *Mod) recordFleetOre(ctx context.Context, gangID, haulerID, minerID int64, units map[int64]int64) {
	for typeID, n := range units {
		if n <= 0 {
			continue
		}
		// The op's work log, separate from the balance below and never subtracted from, so the
		// after-action report (gangreport.go) can say what the miner cut.
		vol := 0.0
		if v, verr := evedb.GetTypeVolume(ctx, m.db, typeID); verr == nil && v > 0 {
			vol = v * float64(n)
		}
		m.noteGangHaul(ctx, gangID, minerID, haulOre, typeID, "", n, vol, 0)
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_gang_ore (gangID, haulerCharID, characterID, typeID, units, updatedAt)
			VALUES (?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE units = units + VALUES(units), updatedAt = VALUES(updatedAt)`,
			gangID, haulerID, minerID, typeID, n, clock.Unix()); err != nil {
			rlogf("rolebots: record fleet ore gang=%d miner=%d: %v", gangID, minerID, err)
		}
	}
}

// payFleetOre settles every unpaid ore balance the leader's wallet can cover: one payment
// per (miner, ore type) row, at the leader's rate. Rows it cannot pay stay for the next
// delivery or the settlement round. The leader's own rows are cleared: the ore in its
// hangar is its payment.
func (m *Mod) payFleetOre(ctx context.Context, g *Gang) {
	if !fleetPaysForOre(g.Activity) || g.LeaderCharID == 0 {
		return
	}
	// One valuation scope for the whole payout, taken from the leader, so every miner is paid
	// the same rate for the same rock.
	payRegions := m.valuationRegionsForChar(ctx, g.LeaderCharID)
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, typeID, units FROM rolebot_gang_ore
		WHERE gangID = ? AND units > 0`, g.GangID)
	if err != nil {
		rlogf("rolebots: pay fleet ore gang=%d: %v", g.GangID, err)
		return
	}
	type due struct {
		minerID, typeID, units int64
	}
	var owed []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.minerID, &d.typeID, &d.units) == nil {
			owed = append(owed, d)
		}
	}
	rows.Close()
	if len(owed) == 0 {
		return
	}

	traits, err := getTraits(ctx, m.db, g.LeaderCharID)
	if err != nil {
		return
	}
	frac := fleetOrePayoutFrac(resolveTuning(traits))

	clear := func(minerID, typeID int64) {
		if _, err := m.db.ExecContext(ctx, `
			DELETE FROM rolebot_gang_ore
			WHERE gangID = ? AND characterID = ? AND typeID = ?`,
			g.GangID, minerID, typeID); err != nil {
			rlogf("rolebots: clear fleet ore gang=%d miner=%d: %v", g.GangID, minerID, err)
		}
	}
	// One catalogue read for the whole payout, not one per owed row.
	oreUnitValue := m.fleetOreUnitValue(ctx, payRegions)
	for _, d := range owed {
		if d.minerID == g.LeaderCharID {
			clear(d.minerID, d.typeID) // its own rock is its payment
			continue
		}
		amount := oreUnitValue[d.typeID] * float64(d.units) * frac
		if amount < 1 {
			clear(d.minerID, d.typeID) // dust: not worth keeping the row
			continue
		}
		// The floor guards the leader's own solvency; the conditional debit inside handISK
		// guards the race.
		if bal, berr := evedb.GetCharBalance(ctx, m.db, g.LeaderCharID); berr != nil ||
			bal-amount < fleetOreLeaderWalletFloor {
			continue // left standing; the next delivery or the settle retries
		}
		if err := m.handISKReason(ctx, g.LeaderCharID, d.minerID, amount, "Mining fleet payout"); err != nil {
			continue
		}
		clear(d.minerID, d.typeID)
		m.noteGangOrePaid(ctx, g.GangID, d.minerID, d.typeID, amount)
		m.logBot(g.LeaderCharID, logInfo, "gang",
			"paid %.0f ISK for %d units of %s delivered to its factory (%.0f%% of refined value)",
			amount, d.units, m.typeNameOr(ctx, d.typeID), frac*100)
		m.logBot(d.minerID, logInfo, "gang",
			"was paid %.0f ISK by its commander for the ore it cut — corp rate, no hauling, no broker",
			amount)
	}
}

// settleFleetOreBeforeDisband is the last call on a fleet's unpaid ore before deleteGang
// purges the ledger. A fleet killed where it stands never got its settlement round, so
// the commander pays what it still can (payFleetOre) and writes off the rest in both
// pilots' logs: an unpayable debt is fair for a gone commander, an unannounced one is not.
func (m *Mod) settleFleetOreBeforeDisband(ctx context.Context, gangID int64, name string) {
	g, err := getGang(ctx, m.db, gangID)
	if err != nil || g == nil {
		return
	}
	m.payFleetOre(ctx, g)

	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, typeID, units FROM rolebot_gang_ore
		WHERE gangID = ? AND units > 0`, gangID)
	if err != nil {
		return
	}
	type due struct{ minerID, typeID, units int64 }
	var left []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.minerID, &d.typeID, &d.units) == nil {
			left = append(left, d)
		}
	}
	rows.Close()
	if fleetSharesOre(g.Activity) {
		// A co-op owes nothing: these are units a hauler loaded and never ran in, lost with its
		// ship or still aboard when the fleet died.
		for _, d := range left {
			m.logBot(d.minerID, logInfo, "gang",
				"%d units of %s it cut for %q were never delivered — the fleet was disbanded first",
				d.units, m.typeNameOr(ctx, d.typeID), name)
		}
		return
	}
	for _, d := range left {
		if d.minerID == g.LeaderCharID {
			continue // its own rock was always its own payment
		}
		m.logBot(d.minerID, logInfo, "gang",
			"wrote off %d units of %s owed by %q — the fleet was disbanded before it could settle",
			d.units, m.typeNameOr(ctx, d.typeID), name)
	}
	if len(left) > 0 && g.LeaderCharID != 0 {
		m.logBot(g.LeaderCharID, logInfo, "gang",
			"disbanded owing %d ore balance(s) it could not pay — %q ends in arrears",
			len(left), name)
	}
}
