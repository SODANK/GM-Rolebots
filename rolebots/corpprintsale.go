package rolebots

import (
	"context"
	"errors"

	evedb "gurimoon/db"
)

// Selling the library's copies: what makes research pay the corporation and not only its scientists.
// Originals are never sold: they are locked down (corpvault.go) and a second one of a type is copy and
// research capacity (corpbottleneck.go). Three steps.
// MARK (the CEO's production review): copies of a type with no open build order go up for sale, less one
// copy kept back of every type built lately; at most corpCopySalesMax are out at once.
// LIST (a builder at its base, a Sales member at any office): a member at the counter brokers it on escrow,
// the corporation paying the fee, at the copy tape's price and no more of a type than sells (corpcopytape.go).
// An original with an old sale row is withdrawn, never listed.
// SETTLE (the wage sweep): a claimed listing pays the broker and the sweep hands the money on. An expired
// one returns to the broker's hangar and the shelf. A broker who left has defaulted.

const (
	// corpCopySalesMax bounds the copies one corporation has marked or out on escrow at once. Every house
	// copies for sale what the roster buys from NPCs (corpcopydemand.go), keeping that ISK in play. Raise it
	// only after measuring.
	corpCopySalesMax   = 60
	corpPrintIdleSec   = 30 * 86400
	corpPrintRelistSec = 7 * 86400
	corpPrintChoreMax  = 10

	lineResearchSales = "researchsales"

	printSaleWanted   = "wanted"
	printSaleListed   = "listed"
	printSaleSold     = "sold"
	printSaleReturned = "returned"
	printSaleDefault  = "default"
	// printSaleWithdrawn closes the row of an original marked for sale
	// before originals were locked down; it is never re-marked.
	printSaleWithdrawn = "withdrawn"
)

// surplusCopies picks the library copies the corporation may sell: every free copy of a type with no open
// build order, less one free copy (the one with most runs; copies come sorted so) of every type in keepOne,
// the types built lately. Pure.
func surplusCopies(copies []libraryCopy, onBoard, keepOne map[int64]bool) []libraryCopy {
	return surplusCopiesKeeping(copies, onBoard, keepOne, nil)
}

// surplusCopiesKeeping is surplusCopies with a per-type reserve on top of
// the one-copy rule: keepN[t] copies of type t are held back (the ones with
// the most runs), for the prints a fund bought so the board never finds the
// last copy of its battleship sold (corpfund.go). Pure.
func surplusCopiesKeeping(copies []libraryCopy, onBoard, keepOne map[int64]bool, keepN map[int64]int) []libraryCopy {
	var out []libraryCopy
	kept := map[int64]int{}
	for _, c := range copies {
		if c.Flag == 111 || onBoard[c.BPTypeID] {
			continue
		}
		reserve := keepN[c.BPTypeID]
		if keepOne[c.BPTypeID] {
			reserve = max(reserve, 1)
		}
		if kept[c.BPTypeID] < reserve {
			kept[c.BPTypeID]++
			continue
		}
		out = append(out, c)
	}
	return out
}

// printSaleRemarkable reports whether a print with a sale row in this state
// may be marked again: one that sold (and has come back to the library), one
// whose listing defaulted, one the chair took back before it was listed, or one back from an expired listing a week ago.
// Never one still out. Pure.
func printSaleRemarkable(status string, returnedAt, now int64) bool {
	switch status {
	case printSaleSold, printSaleDefault, printSaleWithdrawn:
		return true
	case printSaleReturned:
		return now-returnedAt >= corpPrintRelistSec
	}
	return false
}

// markPrintsForSale is step 1, run from the CEO's production review.
func (m *Mod) markPrintsForSale(ctx context.Context, c *corpCEO, ch Charter, stationID int64, sellIdle bool) {
	var openCopies int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM rolebot_corp_print_sales s
		  JOIN invBlueprints bp ON bp.blueprintID = s.itemID AND bp.copy = 1
		 WHERE s.corporationID = ? AND s.status IN (?, ?)`, c.CorpID, printSaleWanted, printSaleListed).
		Scan(&openCopies)
	if openCopies >= corpCopySalesMax {
		return
	}
	work, err := m.openWorkFor(ctx, c.CorpID)
	if err != nil {
		return
	}
	onBoard := map[int64]bool{}
	for _, w := range work {
		if w.Kind == workKindBuild {
			onBoard[w.BPTypeID] = true
		}
	}
	// Built from lately: a manufacturing job on one of its prints in the
	// last month keeps a copy of the type back.
	builtLately := map[int64]bool{}
	if rows, qerr := m.db.QueryContext(ctx, `
		SELECT DISTINCT e.typeID FROM ramJobs j JOIN entity e ON e.itemID = j.installedItemID
		 WHERE j.ownerID = ? AND j.activityID = 1 AND j.beginProductionTime > ?`,
		c.CorpID, evedb.UnixToWin32(clock.Unix()-corpPrintIdleSec)); qerr == nil {
		for rows.Next() {
			var t int64
			if rows.Scan(&t) == nil {
				builtLately[t] = true
			}
		}
		rows.Close()
	}
	// Every office's copies: a copy made at an R&D base is sold from there,
	// or carried to where its type sells (routePrintSales).
	copies, err := m.libraryCopiesAll(ctx, c.CorpID)
	if err != nil {
		return
	}
	// A copy is sold from a station somebody works at (corpWorkStation), because the chore that lists it runs
	// where builders stand. Among those, the region that buys its type from NPCs the most.
	worked := m.corpSaleStations(ctx, c.CorpID, stationID)
	demand := m.printDemand(ctx)
	offices := map[int64]int64{}
	for st, rg := range m.corpOfficeRegions(ctx, c.CorpID) {
		if worked[st] {
			offices[st] = rg
		}
	}
	// A fund-bought print keeps a few copies back for the board, built from lately or not (corpfund.go): the
	// copies are why the corporation saved for it.
	keepN := map[int64]int{}
	if keep := printFundKeepCopies(ctx, m.db); keep > 0 {
		for bp := range m.fundBoughtPrints(ctx, c.CorpID) {
			keepN[bp] = keep
		}
	}
	// A type the market already holds copyTapeCapWeeks of sales of is not marked: its copy would only take a
	// place under corpCopySalesMax from a type that sells (corpcopytape.go).
	tapes, _ := m.copyTapes(ctx)
	markedType := map[int64]int{}
	marked := 0
	for _, cp := range surplusCopiesKeeping(copies, onBoard, builtLately, keepN) {
		if openCopies >= corpCopySalesMax {
			break
		}
		if tapes != nil && tapeRoom(tapes[cp.BPTypeID])-markedType[cp.BPTypeID] <= 0 {
			continue
		}
		at := saleOfficeFor(demand.byRegion(cp.BPTypeID), offices)
		if at == 0 {
			// No base region buys it: sell it where it sits, if anyone works
			// there, else at the factory.
			at = stationID
			if worked[cp.StationID] {
				at = cp.StationID
			}
		}
		if m.markPrintSale(ctx, c, cp.ItemID, cp.BPTypeID, at) {
			openCopies++
			marked++
			markedType[cp.BPTypeID]++
		}
	}
	if marked > 0 {
		m.ceoLog(ctx, c, powerProduction, "printsale", 0, float64(marked),
			"put %d of the library's blueprint copies up for sale - the board has no build order for them", marked)
	}
	m.routePrintSales(ctx, c, ch)
}

// corpWorkStations is every station a member works at: the factory and the
// R&D and Industry bases (corpWorkStation's set).
func (m *Mod) corpWorkStations(ctx context.Context, corpID, factory int64) map[int64]bool {
	out := map[int64]bool{}
	if factory != 0 {
		out[factory] = true
	}
	for _, arm := range []corpArm{armResearch, armIndustry} {
		for _, st := range m.armBaseStations(ctx, corpID, arm) {
			out[st] = true
		}
	}
	return out
}

// corpSaleStations is where a marked copy may be listed: every work station (corpWorkStations) and, when
// the corporation seats a Sales arm, the trade desk's offices, where its traders dock.
func (m *Mod) corpSaleStations(ctx context.Context, corpID, factory int64) map[int64]bool {
	out := m.corpWorkStations(ctx, corpID, factory)
	if armCount(corpID, armSales) == 0 {
		return out
	}
	offices, _, _ := m.tradeDeskOffices(ctx, corpID)
	for st := range offices {
		out[st] = true
	}
	return out
}

// printSaleMoveReason is the text every move carrying a copy to its sale office starts with, which tells
// those moves apart from the builders'.
const printSaleMoveReason = "for sale"

// printSaleMoveOpenMax bounds the sale moves a corporation has open at once,
// apart from printCopyMoveOpenMax: a copy on its way to a counter must never
// hold up a copy a builder is waiting for.
const printSaleMoveOpenMax = 4

// routePrintSales posts a move for every marked copy that sits at another
// office than the one it is to be sold from (markPrintSale's saleStationID).
// The carrier lists it on arrival (printMoveStep).
func (m *Mod) routePrintSales(ctx context.Context, c *corpCEO, ch Charter) {
	var open int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_print_moves
		 WHERE corporationID = ? AND reason LIKE ? AND status IN `+liveMoveStatuses,
		c.CorpID, printSaleMoveReason+"%").Scan(&open)
	if open >= printSaleMoveOpenMax {
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.itemID, s.bpTypeID, o.stationID, s.saleStationID
		  FROM rolebot_corp_print_sales s
		  JOIN entity e ON e.itemID = s.itemID AND e.ownerID = s.corporationID AND e.flag <> 111
		  JOIN crpoffices o ON o.itemID = e.locationID AND o.corporationID = s.corporationID
		 WHERE s.corporationID = ? AND s.status = ? AND s.saleStationID <> 0 AND o.stationID <> s.saleStationID
		 ORDER BY s.markedAt LIMIT ?`, c.CorpID, printSaleWanted, printSaleMoveOpenMax)
	if err != nil {
		return
	}
	type route struct{ itemID, typeID, from, to int64 }
	var rs []route
	for rows.Next() {
		var r route
		if rows.Scan(&r.itemID, &r.typeID, &r.from, &r.to) == nil {
			rs = append(rs, r)
		}
	}
	rows.Close()
	for _, r := range rs {
		if open >= printSaleMoveOpenMax {
			return
		}
		if m.postPrintMove(ctx, c, ch, r.itemID, r.typeID, moveCopy, r.from, r.to,
			printSaleMoveReason+": its type sells best there") {
			open++
		}
	}
}

// markPrintSale marks one library COPY for sale, unless a sale row for it is
// still out. An original is refused: originals are locked down, never sold.
// Reports whether it marked.
func (m *Mod) markPrintSale(ctx context.Context, c *corpCEO, itemID, bpTypeID, saleStationID int64) bool {
	var isCopy int
	if err := m.db.QueryRowContext(ctx,
		`SELECT copy FROM invBlueprints WHERE blueprintID = ?`, itemID).Scan(&isCopy); err != nil || isCopy != 1 {
		return false
	}
	var status string
	var returned int64
	err := m.db.QueryRowContext(ctx, `
		SELECT status, returnedAt FROM rolebot_corp_print_sales WHERE itemID = ?`, itemID).Scan(&status, &returned)
	now := clock.Unix()
	if err == nil && !printSaleRemarkable(status, returned, now) {
		return false
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_print_sales (itemID, corporationID, bpTypeID, status, markedAt, saleStationID)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), bpTypeID = VALUES(bpTypeID),
		  status = VALUES(status), markedAt = VALUES(markedAt), brokerID = 0, escrowID = 0, price = 0,
		  listedAt = 0, returnedAt = 0, saleStationID = VALUES(saleStationID)`,
		itemID, c.CorpID, bpTypeID, printSaleWanted, now, saleStationID); err != nil {
		m.logBot(c.CharID, logError, "ceo", "marking a print for sale: %v", err)
		return false
	}
	return true
}

// corpPrintSaleChore is the list step: a member standing at one of the corporation's offices lists what
// the CEO marked to be sold from there, at its base (corpWorkArmFor) or on delivering a copy to its sale
// office (printMoveStep). A row with no sale office (saleStationID 0) is listed wherever its copy sits.
func (m *Mod) corpPrintSaleChore(ctx context.Context, b *botRun, stationID, officeID int64) {
	m.corpPrintSaleChoreN(ctx, b, stationID, officeID, corpPrintChoreMax)
}

// corpPrintSaleChoreN is corpPrintSaleChore listing at most max prints this visit. The Sales arm's
// visit (corpsalesprints.go) lists more than a builder's.
func (m *Mod) corpPrintSaleChoreN(ctx context.Context, b *botRun, stationID, officeID int64, max int) {
	if b.StationID != stationID || officeID == 0 || officeID == stationID || !escrowSellingEnabled(ctx, m.db) {
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.itemID, s.bpTypeID, COALESCE(bp.materialLevel, 0), COALESCE(bp.productivityLevel, 0),
		       COALESCE(bp.copy, 0), COALESCE(bp.licensedProductionRunsRemaining, -1),
		       COALESCE(e.ownerID = s.corporationID AND e.locationID = ? AND e.flag <> 111, 0),
		       COALESCE(e.ownerID = s.corporationID AND EXISTS (
		         SELECT 1 FROM crpoffices o WHERE o.itemID = e.locationID AND o.corporationID = s.corporationID), 0),
		       EXISTS (SELECT 1 FROM rolebot_corp_print_moves mv
		                WHERE mv.itemID = s.itemID AND mv.status IN `+liveMoveStatuses+`),
		       s.returnedAt > 0
		  FROM rolebot_corp_print_sales s
		  LEFT JOIN entity e ON e.itemID = s.itemID
		  LEFT JOIN invBlueprints bp ON bp.blueprintID = s.itemID
		 WHERE s.corporationID = ? AND s.status = ? AND s.saleStationID IN (0, ?)
		 ORDER BY s.markedAt LIMIT ?`, officeID, b.CorpID, printSaleWanted, stationID, max)
	if err != nil {
		return
	}
	type sale struct {
		itemID, typeID, me, pe int64
		isCopy                 bool
		runs                   int64
		here                   bool
		// shelved: on a shelf of another of the corporation's offices, or on a move: not here, but not gone.
		shelved bool
		// returned: it went up before and came back unsold, so it asks one step lower (tapeAsk).
		returned bool
	}
	// The tape decides the ask and how many of a type may be out at once (corpcopytape.go). Without it
	// the catalogue price stands and nothing is held back.
	tapes, _ := m.copyTapes(ctx)
	listedNow := map[int64]int{} // this visit's listings, on top of the tape's (shared, read-only) counts
	var sales []sale
	for rows.Next() {
		var s sale
		var here, cp, inOffice, moving, returned int
		if rows.Scan(&s.itemID, &s.typeID, &s.me, &s.pe, &cp, &s.runs, &here, &inOffice, &moving, &returned) == nil {
			s.returned = returned == 1
			s.here = here == 1
			s.isCopy = cp == 1
			s.shelved = inOffice == 1 || moving == 1
			sales = append(sales, s)
		}
	}
	rows.Close()
	if len(sales) == 0 {
		return
	}
	expiry := escrowSettingInt(ctx, m.db, "escrow.bpoDaysToExpire", escrowBPODefaultExpiry)
	c, _ := getCEO(ctx, m.db, b.CorpID)
	for _, s := range sales {
		if !s.isCopy {
			// Marked before the vault: an original stays in the library.
			m.setPrintSale(ctx, s.itemID, printSaleWithdrawn, 0, 0, 0)
			continue
		}
		if !s.here {
			if s.shelved {
				continue // at another office or on its way: listed where it lands
			}
			// Gone from every office (a job, a human's hand): not ours to list.
			m.setPrintSale(ctx, s.itemID, printSaleDefault, 0, 0, 0)
			continue
		}
		tape := tapes[s.typeID]
		if tapes != nil && tapeRoom(tape)-listedNow[s.typeID] <= 0 {
			continue // as many of the type are listed as it sells in copyTapeCapWeeks: it waits on the shelf
		}
		returns := 0
		if s.returned {
			returns = 1
		}
		price := tapeAsk(m.corpCopyPrice(ctx, b, s.typeID, s.runs, s.me), tape, s.runs, returns)
		if price < 1 {
			m.setPrintSale(ctx, s.itemID, printSaleDefault, 0, 0, 0)
			continue
		}
		name := m.typeNameOr(ctx, s.typeID)
		// The corporation pays the broker's fee before anything moves.
		_, bal, err := m.corpWithdraw(ctx, b.CorpID, b.CharID, evedb.CourierBasicFee,
			evedb.JournalReason("Listing fee: "+name))
		if err != nil {
			return // the corporation is short; the next visit tries again
		}
		m.pushBalance(b.CharID, bal)
		refund := func() {
			if cb, _, rerr := evedb.TransferISK(ctx, m.db, b.CharID, b.CorpID, evedb.CourierBasicFee, refPlayerDonation,
				evedb.JournalReason("Listing fee refund: "+name)); rerr == nil {
				m.pushBalance(b.CharID, cb)
			}
		}
		// Never a locked item: a lock row outlives the hand-over, and the
		// broker could then never sell what it was handed.
		res, err := m.db.ExecContext(ctx, `
			UPDATE entity SET ownerID = ?, locationID = ?, flag = 4
			 WHERE itemID = ? AND ownerID = ? AND locationID = ?
			   AND NOT EXISTS (SELECT 1 FROM crpLockedItems l WHERE l.itemID = ?)`,
			b.CharID, stationID, s.itemID, b.CorpID, officeID, s.itemID)
		if err != nil {
			refund()
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			refund()
			m.setPrintSale(ctx, s.itemID, printSaleDefault, 0, 0, 0)
			continue
		}
		runs := s.runs
		desc := printListingDesc(name, s.isCopy, runs, s.me, s.pe)
		escrowID, err := m.postPrintListing(ctx, b, s.itemID, s.typeID, price, desc, expiry, s.isCopy, runs, s.me, s.pe, b.CorpID)
		if err != nil {
			_, _ = m.db.ExecContext(ctx, `
				UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
				 WHERE itemID = ? AND ownerID = ? AND locationID = ?`,
				b.CorpID, officeID, corpDivLibrary, s.itemID, b.CharID, stationID)
			refund()
			m.logBot(b.CharID, logError, "escrow", "could not list its corporation's %s: %v", name, err)
			return
		}
		m.setPrintSale(ctx, s.itemID, printSaleListed, b.CharID, escrowID, price)
		listedNow[s.typeID]++
		m.corpBook(ctx, b.CorpID, lineResearchSales, -evedb.CourierBasicFee, b.CharID, "listing fee: "+name)
		m.logBot(b.CharID, logInfo, "escrow", "listed its corporation's %s on escrow for %.0f ISK", desc, price)
		if c != nil {
			m.ceoLog(ctx, c, powerProduction, "printsale", s.typeID, price,
				"%s listed the library's %s on escrow for %s", m.charNameOr(ctx, b.CharID), name, iskString(price))
		}
	}
}

func (m *Mod) setPrintSale(ctx context.Context, itemID int64, status string, brokerID, escrowID int64, price float64) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_print_sales
		   SET status = ?, brokerID = IF(? = 0, brokerID, ?), escrowID = IF(? = 0, escrowID, ?),
		       price = IF(? = 0, price, ?), listedAt = IF(? = ?, ?, listedAt)
		 WHERE itemID = ?`,
		status, brokerID, brokerID, escrowID, escrowID, price, price, status, printSaleListed, clock.Unix(), itemID)
}

// settlePrintSales is step 3, run from the wage sweep.
func (m *Mod) settlePrintSales(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.itemID, s.corporationID, s.brokerID, s.bpTypeID, e.state, e.collateral, e.pickupStationID,
		       COALESCE(ch.corporationID, 0)
		  FROM rolebot_corp_print_sales s
		  JOIN escrows e ON e.escrowID = s.escrowID
		  LEFT JOIN character_ ch ON ch.characterID = s.brokerID
		 WHERE s.status = ? AND e.state IN (?, ?, ?)
		 LIMIT 100`, printSaleListed,
		evedb.CourierStateDelivered, evedb.CourierStateExpired, evedb.CourierStateCancelled)
	if err != nil {
		return
	}
	type done struct {
		itemID, corpID, brokerID, typeID int64
		state                            int
		paid                             float64
		station, brokerCorp              int64
	}
	var ds []done
	for rows.Next() {
		var d done
		if rows.Scan(&d.itemID, &d.corpID, &d.brokerID, &d.typeID, &d.state, &d.paid, &d.station, &d.brokerCorp) == nil {
			ds = append(ds, d)
		}
	}
	rows.Close()
	for _, d := range ds {
		c, _ := getCEO(ctx, m.db, d.corpID)
		name := m.typeNameOr(ctx, d.typeID)
		broker := m.charNameOr(ctx, d.brokerID)
		if d.brokerCorp != d.corpID {
			m.setPrintSale(ctx, d.itemID, printSaleDefault, 0, 0, 0)
			if c != nil {
				m.ceoLog(ctx, c, powerProduction, "printsale", d.typeID, d.paid,
					"wrote off the library's %s: %s left the corporation holding it", name, broker)
			}
			continue
		}
		if d.state == evedb.CourierStateDelivered {
			if d.paid >= 1 {
				bal, _, err := evedb.TransferISK(ctx, m.db, d.brokerID, d.corpID, d.paid, refPlayerDonation,
					evedb.JournalReason("Proceeds of the corporation's "+name))
				if errors.Is(err, evedb.ErrInsufficientFunds) {
					continue // the broker spent it; try again next pass
				}
				if err != nil {
					rlogf("rolebots: settling print sale %d: %v", d.itemID, err)
					continue
				}
				m.pushBalance(d.brokerID, bal)
				m.corpBook(ctx, d.corpID, lineResearchSales, d.paid, d.brokerID, name)
			}
			m.setPrintSale(ctx, d.itemID, printSaleSold, 0, 0, 0)
			if c != nil {
				m.ceoLog(ctx, c, powerProduction, "printsale", d.typeID, d.paid,
					"sold the library's %s for %s (brokered by %s)", name, iskString(d.paid), broker)
			}
			continue
		}
		// Expired or withdrawn: the print is back in the broker's hangar at
		// the pickup station; it goes back on the shelf if the corporation
		// keeps an office there.
		var officeID int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT itemID FROM crpoffices WHERE corporationID = ? AND stationID = ? LIMIT 1`,
			d.corpID, d.station).Scan(&officeID)
		if officeID != 0 {
			_, _ = m.db.ExecContext(ctx, `
				UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
				 WHERE itemID = ? AND ownerID = ? AND locationID = ?`,
				d.corpID, officeID, corpDivLibrary, d.itemID, d.brokerID, d.station)
			// An original listed before the vault comes home locked.
			m.lockArrivedPrint(ctx, d.corpID, d.itemID)
		}
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_print_sales SET status = ?, returnedAt = ? WHERE itemID = ?`,
			printSaleReturned, clock.Unix(), d.itemID)
		if c != nil {
			m.ceoLog(ctx, c, powerProduction, "printsale", d.typeID, 0,
				"the library's %s did not sell and is back on the shelf", name)
		}
	}
}

// withdrawOriginalSales closes, once at boot, every sale row of an ORIGINAL
// still waiting for a broker: originals are locked down now and the chore
// would only refuse them one visit at a time. One already listed comes home
// when its listing ends (settlePrintSales) and is locked on arrival.
func (m *Mod) withdrawOriginalSales(ctx context.Context) {
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_print_sales s
		  LEFT JOIN invBlueprints b ON b.blueprintID = s.itemID
		   SET s.status = ?
		 WHERE s.status = ? AND COALESCE(b.copy, 0) = 0`, printSaleWithdrawn, printSaleWanted)
	if err != nil {
		rlogf("rolebots: withdrawing original print sales: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		rlogf("rolebots: withdrew %d original print(s) from sale - the library keeps its originals", n)
	}
}
