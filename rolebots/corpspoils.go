package rolebots

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// Spoils: the loot members hand in (lootdonate.go), turned into money. A donation lands in
// the corporation's Spoils division (corpdivisions.go); the quartermaster takes what its
// doctrine kits can use first (armoryReserved) and the Sales seat lists the rest here, one
// stack per listing, priced as a hunter prices its own haul (lootAskPrice, lifted to the NPC
// band) and sold on the corporation's account, booked as lineSales for the Sales Agent.

const (
	corpOrderSpoils     = "spoils" // rolebot_corp_orders.kind for Spoils listings
	spoilsListPerThink  = 6
	spoilsListDays      = 30
	spoilsListMinValue  = 20000.0 // under this a listing is not worth a slot
	spoilsDumpUnderSec  = corpStockAgedSec
	spoilsDumpMinFrac   = 0.6 // a bid this far under the ask is still a sale for aged loot
)

// armoryReserved is what the quartermaster holds back out of a corporation's
// Spoils for its doctrine kits, by typeID (corparmory.go). Nil until the
// Armory is stocking.
var armoryReserved = func(ctx context.Context, m *Mod, corpID int64) map[int64]int64 { return nil }

// ceoListCorpSpoils lists the Spoils division's stacks. Called from the
// business power after the ore.
func (m *Mod) ceoListCorpSpoils(ctx context.Context, c *corpCEO) error {
	if !lootSpoilsOn.Load() {
		return nil
	}
	flag, ok := m.corpDivFlag(ctx, c.CorpID, purSpoils)
	if !ok {
		return nil
	}
	// One stack per listing, never a sum: the core's sale walks every division the placer may
	// take from (see ceoListCorpStock), so merging first keeps a stack's quantity whole.
	if offices, err := int64Column(ctx, m,
		`SELECT itemID FROM crpoffices WHERE corporationID = ?`, c.CorpID); err == nil {
		for _, office := range offices {
			if _, err := evedb.StackAllItems(ctx, m.db, c.CorpID, office, flag); err != nil {
				rlogf("rolebots: stacking spoils corp %d office %d: %v", c.CorpID, office, err)
			}
		}
	}
	// Ships are the Armory's (an assembled hull in a folded division is a
	// kit); singleton items are never listed from here.
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, e.typeID, e.quantity, e.itemID
		  FROM crpoffices o
		  JOIN entity e     ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		                   AND e.flag = ? AND e.singleton = 0
		  JOIN invTypes t   ON t.typeID = e.typeID
		  JOIN invGroups g  ON g.groupID = t.groupID
		 WHERE o.corporationID = ? AND g.categoryID NOT IN (6, 9, 25) AND e.quantity > 0
		 ORDER BY o.stationID, e.typeID, e.quantity DESC, e.itemID`, flag, c.CorpID)
	if err != nil {
		return fmt.Errorf("corp spoils: %w", err)
	}
	type lot struct{ stationID, typeID, qty, itemID int64 }
	var lots []lot
	seen := map[[2]int64]bool{}
	for rows.Next() {
		var l lot
		if err := rows.Scan(&l.stationID, &l.typeID, &l.qty, &l.itemID); err != nil {
			rows.Close()
			return err
		}
		key := [2]int64{l.stationID, l.typeID}
		if seen[key] {
			continue
		}
		seen[key] = true
		lots = append(lots, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lots) == 0 {
		return nil
	}
	reserved := armoryReserved(ctx, m, c.CorpID)

	ceo, err := m.loadRun(ctx, c.CharID)
	if err != nil {
		return nil // a CEO that cannot be hydrated cannot price; try next think
	}
	listed := 0
	for _, l := range lots {
		if listed >= spoilsListPerThink || ctx.Err() != nil {
			return nil
		}
		if keep := reserved[l.typeID]; keep > 0 {
			if l.qty <= keep {
				continue
			}
			// The kit's share stays; only the surplus may be listed, and only
			// as its own stack (split off so the sale consumes exactly it).
			surplus, err := m.splitCorpStack(ctx, l.itemID, l.qty-keep)
			if err != nil || surplus == 0 {
				continue
			}
			l.itemID, l.qty = surplus, l.qty-keep
		}
		var open int
		if err := m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_corp_orders
			 WHERE corporationID = ? AND stationID = ? AND typeID = ? AND bid = 0`,
			c.CorpID, l.stationID, l.typeID).Scan(&open); err != nil || open > 0 {
			continue
		}
		placer := m.corpOrderPlacerFor(ctx, c.CorpID, c.CharID, corpOrderSpoils)
		if placer == 0 {
			addCorpOrderPressure(c.CorpID, len(lots)-listed)
			return nil
		}
		ask := m.lootAskPrice(ctx, ceo, l.typeID)
		price, _ := m.anchoredAsk(ctx, ceo.Tune, l.stationID, l.typeID, ask)
		if price <= 0 {
			continue
		}
		intoBid := false
		if price*float64(l.qty) < spoilsListMinValue {
			// Not worth a slot: a small lot goes into somebody else's bid
			// when one pays a fair share of the ask, and waits otherwise.
			bp, n := m.outsideBidTake(ctx, c.CorpID, l.stationID, l.typeID, l.qty)
			if bp <= 0 || n <= 0 || bp < price*spoilsDumpMinFrac {
				continue
			}
			price, l.qty, intoBid = bp, n, true
		}
		out, err := evedb.PlaceSellOrder(ctx, m.db, placer, l.stationID,
			l.typeID, l.itemID, price, l.qty, -1 /*station*/, 1, spoilsListDays, true)
		if err != nil {
			return fmt.Errorf("list corp spoils %s: %w", m.typeNameOr(ctx, l.typeID), err)
		}
		if out.ErrorMsg != "" {
			m.logBot(c.CharID, logInfo, "ceo", "the market refused the corporation's spoils listing of %s: %s",
				m.typeNameOr(ctx, l.typeID), out.ErrorMsg)
			continue
		}
		listed++
		filled := 0.0
		for _, f := range out.Fills {
			filled += f.Price * float64(f.Quantity)
		}
		if filled > 0 {
			m.corpBook(ctx, c.CorpID, lineSales, filled, l.typeID,
				fmt.Sprintf("%d× %s (spoils)", out.FilledQty, m.typeNameOr(ctx, l.typeID)))
			m.creditSalesSeat(ctx, c.CorpID, placer, filled)
		}
		if out.RestingOrderID != 0 {
			if _, err := m.db.ExecContext(ctx, `
				INSERT INTO rolebot_corp_orders
					(orderID, corporationID, stationID, typeID, bid, price, volSeen, placedAt, kind, placedBy)
				VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, ?)`,
				out.RestingOrderID, c.CorpID, l.stationID, l.typeID, price, l.qty-out.FilledQty,
				clock.Unix(), corpOrderSpoils, placer); err != nil {
				return err
			}
		}
		if intoBid {
			m.ceoLog(ctx, c, powerBusiness, "spoils", l.typeID, filled,
				"sold %d× %s out of the spoils into a bid at %s", out.FilledQty,
				m.typeNameOr(ctx, l.typeID), m.stationLabel(ctx, l.stationID))
			continue
		}
		m.ceoLog(ctx, c, powerBusiness, "spoils", l.typeID, price*float64(l.qty),
			"listed %d× %s out of the spoils at %s for %s each", l.qty,
			m.typeNameOr(ctx, l.typeID), m.stationLabel(ctx, l.stationID), iskString(price))
	}
	return nil
}

// splitCorpStack splits qty off a stack into a new stack in the same place
// (same owner, location, flag) and returns the new itemID. 0 when the stack
// is too small or gone.
func (m *Mod) splitCorpStack(ctx context.Context, itemID, qty int64) (int64, error) {
	if qty <= 0 {
		return 0, nil
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE entity SET quantity = quantity - ? WHERE itemID = ? AND singleton = 0 AND quantity > ?`,
		qty, itemID, qty)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil
	}
	res, err = tx.ExecContext(ctx, `
		INSERT INTO entity (itemName, typeID, ownerID, locationID, flag, contraband, singleton,
		                    quantity, x, y, z, customInfo)
		SELECT itemName, typeID, ownerID, locationID, flag, 0, 0, ?, 0, 0, 0, NULL
		  FROM entity WHERE itemID = ?`, qty, itemID)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}
