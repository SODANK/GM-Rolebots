package rolebots

import (
	"context"
	"fmt"
	"math"

	evedb "gurimoon/db"
	"gurimoon/server"
)

// Outsourcing the haul board. A station-to-station leg (a "move": tower output staged at one
// office that the factory at another wants) that nobody has claimed by half its deadline goes
// out as a public courier contract, so players and other corporations' bots can earn from it.
// The courier system is personal (an issuer, items in the issuer's hangar, delivery into the
// issuer's hangar), so the CEO stands in as issuer: the cargo leaves the office for the CEO's
// hangar at the pickup, the corporation funds the reward and fee, and on delivery the cargo is
// filed back into the destination office's division. An expired contract returns the cargo to
// the pickup office and the leg reopens; a failed one is the collateral, which the CEO passes
// to the corporation wallet. Legs that touch a tower stay with the members (no tower end).

const (
	haulKindMove       = "move"
	haulStatusContract = "contract"
	courierCollateralX = 1.1
	courierExpireDays  = 5
	courierMinLegValue = 500000
)

// postStagingMoves files staged tower output the factory wants at another
// station onto the board as move legs.
func (m *Mod) postStagingMoves(ctx context.Context, c *corpCEO, plans []*towerPlan) {
	factoryStation, factoryOffice := m.corpFactoryOffice(ctx, c.CorpID)
	if factoryOffice == 0 {
		return
	}
	seen := map[int64]bool{}
	for _, p := range plans {
		st := p.StagingStationID
		if st == 0 || st == factoryStation || seen[st] {
			continue
		}
		seen[st] = true
		office := m.officeAt(ctx, c.CorpID, st)
		if office == 0 {
			continue
		}
		var items []haulItem
		for t, q := range m.divisionHeld(ctx, c.CorpID, office, corpDivProduct) {
			if q > 0 && m.factoryWants(ctx, c.CorpID, t) {
				items = append(items, haulItem{TypeID: t, Qty: q, Flag: corpDivProduct})
			}
		}
		if len(items) == 0 {
			continue
		}
		var open int
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_corp_haul WHERE corporationID = ? AND kind = ? AND fromStationID = ?
			   AND status IN ('open','claimed','contract')`, c.CorpID, haulKindMove, st).Scan(&open)
		if open > 0 {
			continue
		}
		_, _ = m.postHaul(ctx, c, haulLeg{Kind: haulKindMove, FromKind: haulFromOffice, FromID: office,
			FromStationID: st, FromFlag: corpDivProduct, ToKind: haulToOffice, ToStationID: factoryStation,
			ToFlag: corpDivMaterials, Items: items, PostedBy: "trader", Deadline: clock.Unix() + 4*86400,
			Note: "tower output for the factory"})
	}
}

// outsourceStaleLegs puts unclaimed station-to-station legs out as courier
// contracts, and reconciles the ones already out.
func (m *Mod) outsourceStaleLegs(ctx context.Context, c *corpCEO) {
	now := clock.Unix()
	legs, err := m.queryHauls(ctx, `WHERE corporationID = ? AND kind = ? AND status IN ('open', 'contract')`, c.CorpID, haulKindMove)
	if err != nil {
		return
	}
	for _, l := range legs {
		if l.Status == haulStatusContract {
			m.reconcileLegContract(ctx, c, l)
			continue
		}
		half := l.PostedAt + (l.Deadline-l.PostedAt)/2
		// The chair's own haul stays on its board: a contract would leave in a bot's name.
		if now < half || l.FromKind != haulFromOffice || l.ToKind != haulToOffice || l.PostedBy == haulByChair {
			continue
		}
		if err := m.issueLegContract(ctx, c, l); err != nil {
			m.logBot(c.CharID, logInfo, "ceo", "could not put a haul leg out to contract: %v", err)
		}
	}
}

// issueLegContract posts one leg as a courier contract in the CEO's name.
func (m *Mod) issueLegContract(ctx context.Context, c *corpCEO, l *haulLeg) error {
	office := m.officeAt(ctx, c.CorpID, l.FromStationID)
	if office == 0 {
		return fmt.Errorf("no office at the pickup")
	}
	// Into the CEO's hangar at the pickup.
	type lot struct{ itemID, qty int64 }
	var lots []lot
	value, volume := 0.0, 0.0
	for _, it := range l.Items {
		if it.Qty <= 0 {
			continue
		}
		got, err := m.moveUnits(ctx, c.CorpID, office, it.Flag, it.TypeID, it.Qty, l.FromStationID, 4, c.CharID)
		if err != nil || got <= 0 {
			continue
		}
		var itemID int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT itemID FROM entity WHERE ownerID = ? AND locationID = ? AND flag = 4 AND typeID = ? ORDER BY quantity DESC LIMIT 1`,
			c.CharID, l.FromStationID, it.TypeID).Scan(&itemID)
		if itemID == 0 {
			continue
		}
		lots = append(lots, lot{itemID, got})
		value += evedb.TypeValueOf(ctx, m.db, it.TypeID).Value * float64(got)
		volume += m.typeVolume(ctx, it.TypeID) * float64(got)
	}
	if len(lots) == 0 || value < courierMinLegValue || volume > evedb.CourierMaxVolumeM3 {
		// Not worth a contract (or too big for one): back on the shelf.
		for _, it := range l.Items {
			_, _ = m.moveUnits(ctx, c.CharID, l.FromStationID, 4, it.TypeID, it.Qty, office, it.Flag, c.CorpID)
		}
		return fmt.Errorf("worth %s / %.0f m³ — not a courier contract", iskString(value), volume)
	}
	fromSys, toSys := m.stationSystemID(ctx, l.FromStationID), m.stationSystemID(ctx, l.ToStationID)
	jumps := 1
	if u, err := m.universe(ctx); err == nil && u != nil {
		if j, ok := bfsJumps(u, fromSys, 60)[toSys]; ok {
			jumps = max(j, 1)
		}
	}
	reward := math.Max(rewardForCourier(jumps, volume, value), l.Wage)
	cost := reward + evedb.CourierBasicFee
	// The corporation funds the contract; the CEO holds it.
	if _, _, err := m.corpWithdraw(ctx, c.CorpID, c.CharID, cost,
		"Courier contract for corporation cargo"); err != nil {
		for _, it := range l.Items {
			_, _ = m.moveUnits(ctx, c.CharID, l.FromStationID, 4, it.TypeID, it.Qty, office, it.Flag, c.CorpID)
		}
		return err
	}
	m.corpBook(ctx, c.CorpID, lineWages, -cost, 0, "courier contract for a haul leg")
	fromRegion, toRegion := m.stationRegionOf(ctx, l.FromStationID), m.stationRegionOf(ctx, l.ToStationID)

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var bal float64
	if err := tx.QueryRowContext(ctx, `SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`, c.CharID).Scan(&bal); err != nil {
		return err
	}
	if bal < cost {
		return fmt.Errorf("the CEO's wallet cannot hold the contract's cost")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE character_ SET balance = balance - ? WHERE characterID = ?`, cost, c.CharID); err != nil {
		return err
	}
	_, _ = tx.ExecContext(ctx, `
		INSERT INTO market_journal (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		  accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		c.CharID, server.Win32TimeNow(), evedb.CourierRefType, c.CharID, c.CharID, -cost, bal-cost,
		evedb.JournalReason("Courier contract for corporation cargo"))
	containerID, err := evedb.CreateCourierEscrowContainer(ctx, tx, l.FromStationID)
	if err != nil {
		return err
	}
	for _, lt := range lots {
		if err := evedb.EscrowCourierItem(ctx, tx, c.CharID, l.FromStationID, containerID, lt.itemID, lt.qty); err != nil {
			return err
		}
	}
	missionID, err := evedb.InsertCourierMission(ctx, tx, evedb.CourierCreateParams{
		IssuerID: c.CharID, IssuerCorpID: c.CorpID, PickupStationID: l.FromStationID, DropStationID: l.ToStationID,
		Reward: reward, Collateral: value * courierCollateralX, ListingFee: evedb.CourierBasicFee, Volume: volume,
		Description: "Corporation cargo", DaysToExpire: courierExpireDays, ContainerID: containerID, IssuerIsBot: true,
		PickupSolarSystemID: fromSys, PickupRegionID: fromRegion, DropSolarSystemID: toSys, DropRegionID: toRegion,
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_haul SET status = ?, contractID = ? WHERE legID = ?`,
		haulStatusContract, missionID, l.LegID)
	m.ceoLog(ctx, c, powerStarbase, "courier", missionID, reward,
		"nobody in the corporation took a haul leg, so it went out as a courier contract: %.0f m³ from %s to %s, %s reward",
		volume, m.stationLabel(ctx, l.FromStationID), m.stationLabel(ctx, l.ToStationID), iskString(reward))
	return nil
}

// reconcileLegContract settles a contracted leg once its contract is over.
func (m *Mod) reconcileLegContract(ctx context.Context, c *corpCEO, l *haulLeg) {
	var contractID int64
	_ = m.db.QueryRowContext(ctx, `SELECT contractID FROM rolebot_corp_haul WHERE legID = ?`, l.LegID).Scan(&contractID)
	if contractID == 0 {
		return
	}
	var state int
	var collateral float64
	if err := m.db.QueryRowContext(ctx, `SELECT state, collateral FROM couriermissions WHERE missionID = ?`, contractID).
		Scan(&state, &collateral); err != nil {
		return
	}
	switch state {
	case evedb.CourierStateDelivered:
		office := m.officeAt(ctx, c.CorpID, l.ToStationID)
		for _, it := range l.Items {
			if office != 0 {
				_, _ = m.moveUnits(ctx, c.CharID, l.ToStationID, 4, it.TypeID, it.Qty, office, l.ToFlag, c.CorpID)
			}
		}
		m.closeHaul(ctx, l, "done", fmt.Sprintf("delivered by courier contract %d", contractID))
	case evedb.CourierStateExpired, evedb.CourierStateCancelled:
		office := m.officeAt(ctx, c.CorpID, l.FromStationID)
		for _, it := range l.Items {
			if office != 0 {
				_, _ = m.moveUnits(ctx, c.CharID, l.FromStationID, 4, it.TypeID, it.Qty, office, it.Flag, c.CorpID)
			}
		}
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_haul SET status = 'open', contractID = 0, postedAt = ?, deadline = ? WHERE legID = ?`,
			clock.Unix(), clock.Unix()+4*86400, l.LegID)
	case evedb.CourierStateFailed:
		if collateral >= 1 {
			if _, _, err := evedb.TransferISK(ctx, m.db, c.CharID, c.CorpID, math.Floor(collateral), refPlayerDonation,
				"Courier collateral for lost corporation cargo"); err == nil {
				m.corpBook(ctx, c.CorpID, lineWages, collateral, 0, "courier collateral for lost cargo")
			}
		}
		m.closeHaul(ctx, l, "lost", fmt.Sprintf("courier contract %d failed; collateral collected", contractID))
	}
}
