package rolebots

// The print ledger: every blueprint the roster puts on escrow, copy or original, with the facts the
// escrow row lacks.
//
// An escrow listing does not know it holds a blueprint, its type, whether it is a copy or its runs: the
// crate does, until a buyer claims it. So the lister writes one row here per listing (postPrintListing),
// and two questions read it joined to escrows for the state:
//
//	stock   how many copies of a type this researcher has on the board, so the next copy job tops up
//	demand  which copy types have sold anywhere lately, which raises the stock target

import (
	"context"
	"fmt"
	"sync/atomic"

	evedb "gurimoon/db"
)

// postPrintListing lists one blueprint on escrow and records it in the ledger.
// corpID is the corporation the sale is for (0 for the pilot's own).
func (m *Mod) postPrintListing(ctx context.Context, b *botRun, itemID, bpTypeID int64, price float64,
	desc string, expiryDays int, isCopy bool, runs, me, pe, corpID int64) (int64, error) {
	escrowID, err := m.postEscrowListingID(ctx, b, itemID, bpTypeID, 1, price, desc, expiryDays)
	if err != nil {
		return 0, err
	}
	cp := 0
	if isCopy {
		cp = 1
	}
	if _, lerr := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_bpc_listings
		  (escrowID, characterID, corporationID, bpTypeID, isCopy, runs, me, pe, price, listedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE characterID = VALUES(characterID), corporationID = VALUES(corporationID),
		  bpTypeID = VALUES(bpTypeID), isCopy = VALUES(isCopy), runs = VALUES(runs), me = VALUES(me),
		  pe = VALUES(pe), price = VALUES(price), listedAt = VALUES(listedAt)`,
		escrowID, b.CharID, corpID, bpTypeID, cp, runs, me, pe, price, clock.Unix()); lerr != nil {
		// The listing is real either way; a missing ledger row only makes the
		// researcher's next stock count low by one.
		rlogf("rolebots: print ledger %d: %v", escrowID, lerr)
	}
	return escrowID, nil
}

// heldPrintCopy is one blueprint copy in a hangar.
type heldPrintCopy struct {
	ItemID, TypeID int64
	Runs, ME, PE   int64
}

// copyStockFor counts this pilot's copies by blueprint type: open copy
// listings on the board, copies in its hangar at stationID, and copies still
// cooking (LabCooking). Also returns the open listing total for the cap.
func (m *Mod) copyStockFor(ctx context.Context, b *botRun, stationID int64) (map[int64]int, int, error) {
	stock := map[int64]int{}
	open := 0
	rows, err := m.db.QueryContext(ctx, `
		SELECT l.bpTypeID, COUNT(*)
		  FROM rolebot_bpc_listings l
		  JOIN escrows e ON e.escrowID = l.escrowID
		 WHERE l.characterID = ? AND l.isCopy = 1 AND e.state = 0
		 GROUP BY l.bpTypeID`, b.CharID)
	if err != nil {
		return nil, 0, fmt.Errorf("copy listings: %w", err)
	}
	for rows.Next() {
		var t int64
		var n int
		if rows.Scan(&t, &n) == nil {
			stock[t] += n
			open += n
		}
	}
	rows.Close()
	held, err := m.hangarCopies(ctx, b.CharID, stationID)
	if err != nil {
		return nil, 0, err
	}
	for _, c := range held {
		stock[c.TypeID]++
	}
	if b.State != nil {
		for _, j := range b.State.Data.LabCooking {
			if j.ActivityID == ramActivityCopying {
				stock[j.BPTypeID] += j.Copies
			}
		}
	}
	return stock, open, nil
}

// hangarCopies lists the blueprint copies a pilot holds at a station.
func (m *Mod) hangarCopies(ctx context.Context, charID, stationID int64) ([]heldPrintCopy, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, bp.licensedProductionRunsRemaining,
		       bp.materialLevel, bp.productivityLevel
		  FROM entity e
		  JOIN invBlueprints bp ON bp.blueprintID = e.itemID AND bp.copy = 1
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4`, charID, stationID)
	if err != nil {
		return nil, fmt.Errorf("hangar copies: %w", err)
	}
	defer rows.Close()
	var out []heldPrintCopy
	for rows.Next() {
		var c heldPrintCopy
		if err := rows.Scan(&c.ItemID, &c.TypeID, &c.Runs, &c.ME, &c.PE); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// copyDemand is how many copies of each blueprint type sold anywhere in the
// last copyDemandWindowSec, shared by the roster.
func (m *Mod) copyDemand(ctx context.Context) (map[int64]int, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshot(snapKey("bpc-demand"), func() (map[int64]int, error) {
		rows, err := m.db.QueryContext(sctx, `
			SELECT l.bpTypeID, COUNT(*)
			  FROM rolebot_bpc_listings l
			  JOIN escrows e ON e.escrowID = l.escrowID
			 WHERE l.isCopy = 1 AND e.state = ? AND e.dateCompleted > ?
			 GROUP BY l.bpTypeID`, evedb.CourierStateDelivered, clock.Unix()-copyDemandWindowSec)
		if err != nil {
			return nil, fmt.Errorf("copy demand: %w", err)
		}
		defer rows.Close()
		out := map[int64]int{}
		for rows.Next() {
			var t int64
			var n int
			if err := rows.Scan(&t, &n); err != nil {
				return nil, err
			}
			out[t] = n
		}
		return out, rows.Err()
	})
}

// copyStockTargetFor is how many copies of a type a researcher keeps on hand.
func copyStockTargetFor(sold int) int {
	if sold > 0 {
		return copyStockTargetSelling
	}
	return copyStockTarget
}

// bpcListingKeepDays is how long a ledger row outlives its listing. The longest reader looks back 14 days
// (corpCopyDemandWindowSec); pruneBPCListings removes older rows.
const bpcListingKeepDays = 30

// bpcPrunedAt spaces pruneBPCListings to hourly: the order sweep that calls it
// runs every minute, and listedAt has no index of its own.
var bpcPrunedAt atomic.Int64

// pruneBPCListings drops ledger rows whose listing closed (delivered, expired,
// or cancelled) or whose escrow row is gone, once they are past
// bpcListingKeepDays. An open listing keeps its row however old it is: the
// stock count reads it.
func (m *Mod) pruneBPCListings(ctx context.Context) {
	now := clock.Unix()
	if now-bpcPrunedAt.Load() < 3600 {
		return
	}
	bpcPrunedAt.Store(now)
	cut := now - bpcListingKeepDays*86400
	for i := 0; i < tradePruneMaxChunks && ctx.Err() == nil; i++ {
		if i > 0 && !prunePause(ctx) {
			return
		}
		rows, err := m.db.QueryContext(ctx, capStatement(`
			SELECT l.escrowID FROM rolebot_bpc_listings l
			  LEFT JOIN escrows e ON e.escrowID = l.escrowID
			 WHERE l.listedAt < ?
			   AND (e.escrowID IS NULL OR (e.state <> 0 AND e.dateCompleted < ?))
			 LIMIT ?`), cut, cut, tradePruneChunk)
		if err != nil {
			rlogf("rolebots: print ledger prune: %v", err)
			return
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if len(ids) == 0 {
			return
		}
		if _, err := m.db.ExecContext(ctx,
			`DELETE FROM rolebot_bpc_listings WHERE escrowID IN (`+placeholders(len(ids))+`)`,
			int64Args(ids)...); err != nil {
			rlogf("rolebots: print ledger prune: %v", err)
			return
		}
		if len(ids) < tradePruneChunk {
			return
		}
	}
}
