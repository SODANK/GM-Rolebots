package rolebots

// Bot escrow selling: a researcher's blueprint copies and spare originals, and surplus fitted ships.
//
// Entry points:
//   - listLibraryProducts: a research career lists its copies, and any fully researched spare
//     original, wherever it is docked (bpcopy.go prices them).
//   - maybeEscrowSurplusShip: called from procure when the old ship is replaced; if the old
//     hull and fit are worth enough, it is listed.
//
// Both run docked where the goods sit, through the player DB path: CreateCourierEscrowContainer,
// EscrowCourierItem, InsertEscrow.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/server"
)

// fittedItem is one module/charge in a fitted ship for escrow listing.
type fittedItem struct {
	ItemID   int64
	TypeID   int64
	Quantity int64
}

// ── config defaults ─────────────────────────────────────────────────────────

const (
	escrowBPOMinME          = 5
	escrowBPOMinPE          = 3
	escrowBPOAmortProjects  = 10
	escrowBPODefaultExpiry  = 14 // days
	escrowBPOMarkup         = 1.5
	escrowShipMinValue      = 500000.0
	escrowShipMarkup        = 1.15
	escrowShipMaxBaseMul    = 3.0
	escrowShipDefaultExpiry = 7 // days
)

// ── BPO valuation ───────────────────────────────────────────────────────────

// escrowBPOResearchValue computes the ISK value of the research invested in a blueprint: the
// material savings of this ME over ME 0 at wasteFactor 10, multiplied over amort builds.
func escrowBPOResearchValue(ctx context.Context, db *sql.DB, bpTypeID int64, me, pe int64, peSkill int) float64 {
	return printSavingPerRun(ctx, db, bpTypeID, me, peSkill) * escrowBPOAmortProjects
}

// ── library sales ───────────────────────────────────────────────────────────

// libraryListBatch bounds how many prints one pass lists: a copy job delivers
// up to copyBatchMax copies, and the whole batch should reach the board in one
// visit rather than dribble out over a day of half-hour passes.
const libraryListBatch = 40

// researchProductFeeRuns is how many listing fees a researcher must be able to
// pay before it starts listing at all.
const researchProductFeeRuns = 2

// listLibraryProducts is the research career's sale, run wherever the pilot is docked. Copies
// go on escrow at bpcPrice (printListingDesc labels them), up to copyListingCap open listings.
// A spare full original is sold at fullBPOPrice only with sellOriginalsEnabled; a partly
// researched one is never sold. Returns how many it listed.
//
// A pilot outside a research career lists only hull copies it made itself from an original it holds here (ownercopy.go), never the
// copies it builds from, and never an original.
func (m *Mod) listLibraryProducts(ctx context.Context, b *botRun) int {
	career := researchCareer(b.Tune)
	if b.StationID == 0 || !escrowSellingEnabled(ctx, m.db) || (!career && !ownerCopyOn(ctx, m)) {
		return 0
	}
	// For a non-career pilot: the hull originals it holds, which make a copy of that type its own product rather than a print it
	// bought to build from. None, and there is nothing it may list.
	var ownMake map[int64]bool
	if !career {
		ownMake = m.ownerCopyOriginals(ctx, b.CharID)
		if len(ownMake) == 0 {
			return 0
		}
	}
	// The broker's fee comes first: a researcher that cannot pay it is poor,
	// not broken.
	bal, berr := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if berr != nil || spendable(bal) < evedb.CourierBasicFee*researchProductFeeRuns {
		return 0
	}
	skills, _ := m.charSkillsFor(ctx, b.CharID)
	peSkill := skills[skillProductionEff]
	science := skills[skillScience]
	frac := m.bpcPriceFrac(ctx)
	fullME, fullPE := m.fullResearchLevels(ctx)
	mulT1, mulT2 := m.fullBPOMarkups(ctx)
	expiry := escrowSettingInt(ctx, m.db, "escrow.bpoDaysToExpire", escrowBPODefaultExpiry)

	inUse := map[int64]bool{}
	if b.State != nil {
		d := b.State.Data
		inUse[d.IndBPItemID], inUse[d.IndFetchItemID], inUse[d.LabBPItemID] = true, true, true
	}
	listed := 0
	post := func(itemID, typeID int64, price float64, desc string, isCopy bool, runs, me, pe int64) bool {
		if _, err := m.postPrintListing(ctx, b, itemID, typeID, price, desc, expiry, isCopy, runs, me, pe, 0); err != nil {
			if !strings.Contains(err.Error(), "cannot afford") {
				m.logBot(b.CharID, logError, "escrow", "could not list %s: %v", desc, err)
			}
			return false
		}
		listed++
		m.logBot(b.CharID, logInfo, "escrow", "listed %s on escrow for %s", desc, iskString(price))
		return true
	}

	// Copies.
	held, err := m.hangarCopies(ctx, b.CharID, b.StationID)
	if err != nil {
		return 0
	}
	_, open, _ := m.copyStockFor(ctx, b, b.StationID)
	copyCost := map[int64]float64{} // per-copy share of a copy job here, by type
	for _, c := range held {
		if listed >= libraryListBatch || open >= copyListingCap {
			break
		}
		if inUse[c.ItemID] || c.Runs <= 0 {
			continue
		}
		if !career && (!ownMake[c.TypeID] || ownerCopyKeepsType(b, c.TypeID)) {
			continue
		}
		f, ferr := m.printFactsFor(ctx, c.TypeID)
		if ferr != nil {
			continue
		}
		cost, ok := copyCost[c.TypeID]
		if !ok {
			cost = m.copyFeePerCopy(ctx, b, c.TypeID, c.Runs, science)
			copyCost[c.TypeID] = cost
		}
		ref := m.printRefPrice(ctx, b, c.TypeID)
		saving := printSavingPerRun(ctx, m.db, c.TypeID, c.ME, peSkill)
		price := bpcPrice(ref, c.Runs, f.MaxRuns, saving, cost, frac)
		desc := printListingDesc(f.Name, true, c.Runs, c.ME, c.PE)
		if !post(c.ItemID, c.TypeID, price, desc, true, c.Runs, c.ME, c.PE) {
			break
		}
		open++
	}

	// Spare originals at the research target, only while sellOriginalsEnabled is on, and only a research career's.
	if !career || !sellOriginalsEnabled(ctx, m.db) {
		return listed
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, COALESCE(bp.materialLevel, 0), COALESCE(bp.productivityLevel, 0)
		  FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND COALESCE(bp.copy, 0) = 0`,
		b.CharID, b.StationID)
	if err != nil {
		return listed
	}
	var originals []heldPrint
	for rows.Next() {
		var p heldPrint
		if rows.Scan(&p.ItemID, &p.TypeID, &p.ME, &p.PE) == nil {
			originals = append(originals, p)
		}
	}
	rows.Close()
	for _, p := range spareFullOriginals(originals, inUse, fullME, fullPE) {
		if listed >= libraryListBatch {
			break
		}
		f, ferr := m.printFactsFor(ctx, p.TypeID)
		if ferr != nil {
			continue
		}
		ref := m.printRefPrice(ctx, b, p.TypeID)
		if ref <= 0 {
			continue
		}
		research := escrowBPOResearchValue(ctx, m.db, p.TypeID, p.ME, p.PE, peSkill)
		price := fullBPOPrice(ref, f.TechLevel >= 2, research, mulT1, mulT2)
		desc := printListingDesc(f.Name, false, 0, p.ME, p.PE)
		if !post(p.ItemID, p.TypeID, price, desc, false, 0, p.ME, p.PE) {
			break
		}
	}
	return listed
}

// spareFullOriginals picks the originals a library may sell: per type, every
// print but the best (highest ME, then PE), and of those only the ones at full
// research. inUse prints are never spare. Pure.
func spareFullOriginals(prints []heldPrint, inUse map[int64]bool, fullME, fullPE int64) []heldPrint {
	best := map[int64]heldPrint{}
	for _, p := range prints {
		if cur, ok := best[p.TypeID]; !ok || p.ME > cur.ME || (p.ME == cur.ME && p.PE > cur.PE) {
			best[p.TypeID] = p
		}
	}
	var out []heldPrint
	for _, p := range prints {
		if inUse[p.ItemID] || best[p.TypeID].ItemID == p.ItemID {
			continue
		}
		if printFullyResearched(p.ME, p.PE, fullME, fullPE) {
			out = append(out, p)
		}
	}
	return out
}

// copyFeePerCopy is one copy's share of the cost of a copy job of this print on the copy line
// at the pilot's station: the floor under a copy's price. Zero when the station has no copy
// line it can quote.
func (m *Mod) copyFeePerCopy(ctx context.Context, b *botRun, bpTypeID, runs int64, science int) float64 {
	line, err := m.assemblyLineAt(ctx, b, b.StationID, ramActivityCopying, bpTypeID)
	if err != nil {
		return 0
	}
	bpType, err := evedb.GetBlueprintTypeInfo(ctx, m.db, bpTypeID)
	if err != nil {
		return 0
	}
	dur := copyDurationSec(bpType, 1, runs, science, line)
	return line.Info.CostInstall/float64(copyBatchMin) + line.Info.CostPerHour*dur/3600
}

// escrowBPOPrice is a researched original's asking price: its catalogue price
// plus the research worth to a builder times the premium. It returns the price
// and the research value separately for the log.
func escrowBPOPrice(ctx context.Context, db *sql.DB, bpTypeID, me, pe int64, peSkill int, markup float64) (price, value float64) {
	value = escrowBPOResearchValue(ctx, db, bpTypeID, me, pe, peSkill)
	var base float64
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(basePrice, 0) FROM invTypes WHERE typeID = ?`, bpTypeID).Scan(&base)
	return base + value*markup, value
}

// ── ship listing ────────────────────────────────────────────────────────────

// maybeEscrowSurplusShip lists a surplus fitted ship on escrow, called when a bot procures a hull and still
// holds the ship it replaces.
func (m *Mod) maybeEscrowSurplusShip(ctx context.Context, b *botRun, shipItemID int64) {
	if !escrowSellingEnabled(ctx, m.db) {
		return
	}
	if shipItemID == 0 || b.StationID == 0 {
		return
	}

	// Get the ship's type and verify it is at this station.
	var typeID int64
	var locationID int64
	err := m.db.QueryRowContext(ctx, `
		SELECT typeID, locationID FROM entity
		WHERE itemID = ? AND ownerID = ?`,
		shipItemID, b.CharID).Scan(&typeID, &locationID)
	if err != nil || locationID != b.StationID {
		return
	}

	// Value the hull + all fitted modules.
	var totalValue float64
	var typeIDs []int64

	typeIDs = append(typeIDs, typeID)

	// Get fitted modules.
	var fitted []fittedItem
	modRows, err := m.db.QueryContext(ctx, `
		SELECT itemID, typeID, quantity FROM entity
		WHERE locationID = ? AND ownerID = ? AND flag BETWEEN 11 AND 35`,
		shipItemID, b.CharID)
	if err == nil {
		for modRows.Next() {
			var f fittedItem
			if err := modRows.Scan(&f.ItemID, &f.TypeID, &f.Quantity); err != nil {
				break
			}
			fitted = append(fitted, f)
			typeIDs = append(typeIDs, f.TypeID)
		}
		modRows.Close()
	}

	// Unfitted hull: not worth listing as Fitted.
	if len(fitted) == 0 {
		return
	}

	prices, _ := evedb.TypeValues(ctx, m.db, typeIDs)
	if tv, ok := prices[typeID]; ok {
		totalValue += tv.Value
	}
	for _, f := range fitted {
		if tv, ok := prices[f.TypeID]; ok {
			totalValue += tv.Value * float64(f.Quantity)
		}
	}

	minValue := escrowSettingFloat(ctx, m.db, "escrow.shipBotMinValue", escrowShipMinValue)
	if totalValue < minValue {
		return
	}

	// Cap the price against the sum of base prices to guard against inflated
	// VWAP from thin markets or NPC seeder orders.
	var basePriceSum float64
	bpArgs := make([]any, len(typeIDs))
	for i, id := range typeIDs {
		bpArgs[i] = id
	}
	bpRows, err := m.db.QueryContext(ctx,
		`SELECT typeID, COALESCE(basePrice, 0) FROM invTypes WHERE typeID IN (`+
			placeholders(len(typeIDs))+`)`, bpArgs...)
	if err == nil {
		basePrices := map[int64]float64{}
		for bpRows.Next() {
			var tid int64
			var bp float64
			bpRows.Scan(&tid, &bp)
			basePrices[tid] = bp
		}
		bpRows.Close()
		basePriceSum += basePrices[typeID]
		for _, f := range fitted {
			basePriceSum += basePrices[f.TypeID] * float64(f.Quantity)
		}
	}

	shipMarkup := escrowSettingFloat(ctx, m.db, "escrow.shipBotMarkup", escrowShipMarkup)
	price := totalValue * shipMarkup

	// If base prices are available, cap at maxBasePriceMul × basePriceSum.
	if basePriceSum > 0 {
		maxMul := escrowSettingFloat(ctx, m.db, "escrow.shipMaxBasePriceMul", escrowShipMaxBaseMul)
		cap := basePriceSum * maxMul
		if price > cap {
			price = cap
		}
	}

	// Get ship name for description.
	var typeName string
	m.db.QueryRowContext(ctx,
		`SELECT typeName FROM invTypes WHERE typeID = ?`, typeID).Scan(&typeName)

	desc := fmt.Sprintf("Fitted %s (%d modules)", typeName, len(fitted))
	expiry := escrowSettingInt(ctx, m.db, "escrow.shipDaysToExpire", escrowShipDefaultExpiry)

	// Escrow the ship and its modules.
	if err := m.postEscrowShipListing(ctx, b, shipItemID, typeID, fitted, price, desc, expiry); err != nil {
		m.logBot(b.CharID, logError, "escrow",
			"failed to list surplus %s on escrow: %v", typeName, err)
		return
	}
	// The crated ship cannot be boarded, so the fit assignment naming it is cleared; left in place,
	// every plan for that activity fails with "not in a reachable station" and retries.
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_fit_ships SET shipItemID = 0 WHERE characterID = ? AND shipItemID = ?`,
		b.CharID, shipItemID)
	m.logBot(b.CharID, logInfo, "escrow",
		"listed surplus fitted %s on escrow for %.0f ISK (component value %.0f)",
		typeName, price, totalValue)
}

// ── core posting ────────────────────────────────────────────────────────────

// postEscrowListing posts a single non-ship item on escrow.
func (m *Mod) postEscrowListing(ctx context.Context, b *botRun,
	itemID, typeID, qty int64, price float64, desc string, expiryDays int) error {
	_, err := m.postEscrowListingID(ctx, b, itemID, typeID, qty, price, desc, expiryDays)
	return err
}

// postEscrowListingID is postEscrowListing returning the new escrowID, which
// the print ledger (bpclistings.go) keys on.
func (m *Mod) postEscrowListingID(ctx context.Context, b *botRun,
	itemID, typeID, qty int64, price float64, desc string, expiryDays int) (int64, error) {

	corpID, _ := evedb.GetCharCorpID(ctx, m.db, b.CharID)

	// Wallet check for listing fee: an early out only. The debit below is the real test.
	var balance float64
	m.db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`,
		b.CharID).Scan(&balance)
	if spendable(balance) < evedb.CourierBasicFee {
		return 0, fmt.Errorf("cannot afford listing fee")
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}

	// Debit listing fee, relative and conditional on the floor (chargeSpendableTx).
	ok, newBalance, err := chargeSpendableTx(ctx, tx, b.CharID, evedb.CourierBasicFee)
	if err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("debit fee: %w", err)
	}
	if !ok {
		tx.Rollback()
		return 0, fmt.Errorf("cannot afford listing fee")
	}

	// Journal for the fee.
	tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		b.CharID, server.Win32TimeNow(), evedb.CourierRefType, b.CharID, b.CharID,
		-evedb.CourierBasicFee, newBalance,
		evedb.JournalReason("Escrow listing fee"))

	// Create container and escrow the item.
	containerID, err := evedb.CreateCourierEscrowContainer(ctx, tx, b.StationID)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	if err := evedb.EscrowCourierItem(ctx, tx, b.CharID, b.StationID,
		containerID, itemID, qty); err != nil {
		tx.Rollback()
		return 0, fmt.Errorf("escrow item: %w", err)
	}

	// Resolve geography.
	hereRec, _ := m.stationStaticRec(ctx, b.StationID)
	sysID, regionID := hereRec.SystemID, hereRec.RegionID

	// Insert escrow listing.
	escrowID, err := evedb.InsertEscrow(ctx, tx, evedb.EscrowCreateParams{
		IssuerID:            b.CharID,
		IssuerCorpID:        corpID,
		PickupStationID:     b.StationID,
		Collateral:          price,
		ListingFee:          evedb.CourierBasicFee,
		Volume:              0,
		Description:         desc,
		DaysToExpire:        expiryDays,
		ContainerID:         containerID,
		IssuerIsBot:         true,
		PickupSolarSystemID: sysID,
		PickupRegionID:      regionID,
	})
	if err != nil {
		tx.Rollback()
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return escrowID, nil
}

// postEscrowShipListing posts a fitted ship on escrow, rack still on the hull.
func (m *Mod) postEscrowShipListing(ctx context.Context, b *botRun,
	shipItemID, shipTypeID int64, fitted []fittedItem,
	price float64, desc string, expiryDays int) error {

	corpID, _ := evedb.GetCharCorpID(ctx, m.db, b.CharID)

	// An early out only: the debit below is the real test.
	var balance float64
	m.db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`,
		b.CharID).Scan(&balance)
	if spendable(balance) < evedb.CourierBasicFee {
		return fmt.Errorf("cannot afford listing fee")
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Debit listing fee, relative and conditional on the floor (chargeSpendableTx).
	ok, newBalance, err := chargeSpendableTx(ctx, tx, b.CharID, evedb.CourierBasicFee)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("debit fee: %w", err)
	}
	if !ok {
		tx.Rollback()
		return fmt.Errorf("cannot afford listing fee")
	}

	tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		b.CharID, server.Win32TimeNow(), evedb.CourierRefType, b.CharID, b.CharID,
		-evedb.CourierBasicFee, newBalance,
		evedb.JournalReason("Escrow listing fee"))

	containerID, err := evedb.CreateCourierEscrowContainer(ctx, tx, b.StationID)
	if err != nil {
		tx.Rollback()
		return err
	}

	// The ship is sold fitted: the hull goes into the crate with its rack and charges, and the claim
	// re-owns the whole tree (evedb.ClaimEscrowMission). Cargo and drone bay go to the hangar first.
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 4
		WHERE locationID = ? AND ownerID = ? AND flag NOT BETWEEN 11 AND 35`,
		b.StationID, shipItemID, b.CharID); err != nil {
		tx.Rollback()
		return fmt.Errorf("unload ship: %w", err)
	}

	if err := evedb.EscrowCourierItem(ctx, tx, b.CharID, b.StationID,
		containerID, shipItemID, 1); err != nil {
		tx.Rollback()
		return fmt.Errorf("escrow ship: %w", err)
	}

	hereRec, _ := m.stationStaticRec(ctx, b.StationID)
	sysID, regionID := hereRec.SystemID, hereRec.RegionID

	_, err = evedb.InsertEscrow(ctx, tx, evedb.EscrowCreateParams{
		IssuerID:            b.CharID,
		IssuerCorpID:        corpID,
		PickupStationID:     b.StationID,
		Collateral:          price,
		ListingFee:          evedb.CourierBasicFee,
		Volume:              0,
		Description:         desc,
		DaysToExpire:        expiryDays,
		ContainerID:         containerID,
		IssuerIsBot:         true,
		PickupSolarSystemID: sysID,
		PickupRegionID:      regionID,
	})
	if err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit()
}

// ── step functions ──────────────────────────────────────────────────────────

// escrowListStep advances an escrow listing task. The listing itself was
// already posted in the entry point (listLibraryProducts / maybeEscrowSurplusShip),
// so this task only exists as a dwell before returning to idle.
func (m *Mod) escrowListStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	st.Data.EscrowListTypeID = 0
	st.Data.EscrowListTypeName = ""
	st.Data.EscrowListPrice = 0
	st.Data.EscrowListStation = 0
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return false, saveState(ctx, m.db, st)
}

// ── detail functions ────────────────────────────────────────────────────────

func detailEscrowList(_ *Mod, _ context.Context, st *SimState) string {
	if st.Data.EscrowListTypeName != "" {
		return fmt.Sprintf("listing %s for %.0f ISK on escrow",
			st.Data.EscrowListTypeName, st.Data.EscrowListPrice)
	}
	return "posting an escrow listing"
}

func detailEscrowBuy(_ *Mod, _ context.Context, st *SimState) string {
	return "claiming an escrow listing"
}

// ── settings helpers ────────────────────────────────────────────────────────

func escrowSellingEnabled(ctx context.Context, db *sql.DB) bool {
	if vs, ok, _ := evedb.GetServerSetting(ctx, db, "escrow.botSellingEnabled"); ok {
		return vs == "1" || vs == "true"
	}
	return true // default on
}

func escrowBuyingEnabled(ctx context.Context, db *sql.DB) bool {
	if vs, ok, _ := evedb.GetServerSetting(ctx, db, "escrow.botBuyingEnabled"); ok {
		return vs == "1" || vs == "true"
	}
	return true // default on
}

func escrowSettingInt(ctx context.Context, db *sql.DB, key string, def int) int {
	if vs, ok, _ := evedb.GetServerSetting(ctx, db, key); ok {
		if v, err := strconv.Atoi(vs); err == nil {
			return v
		}
	}
	return def
}

func escrowSettingFloat(ctx context.Context, db *sql.DB, key string, def float64) float64 {
	if vs, ok, _ := evedb.GetServerSetting(ctx, db, key); ok {
		if v, err := strconv.ParseFloat(vs, 64); err == nil {
			return v
		}
	}
	return def
}

// Keeps the time import in use for postEscrowListing.
var _ = time.Now
