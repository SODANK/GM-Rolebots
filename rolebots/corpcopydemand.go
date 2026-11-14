package rolebots

// The NPC print demand: what the roster pays the NPC shelves for blueprints, and
// so what a corporation's copy service could sell instead. A builder takes an
// acceptable escrow copy before an original (bestEscrowBPO), so the demand side
// needs nothing; what was missing was supply aimed at it. The ledger is one
// 14-day aggregate over market_transactions, (region, blueprint type) to
// purchases and ISK, for every blueprint a rolebot bought from an NPC (a seller
// is an NPC when clientID is under evedb.MinPlayerEntityID, as in npcleak.go;
// transactionType 1 is the buyer's row). Readers: the library plan
// (corplibplan.go), the print sale (corpprintsale.go) and the purchase order for
// originals to copy (corpproduction.go). See docs/comment-notes-b04.md#print-demand.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	evedb "gurimoon/db"
)

const (
	// printDemandWindowDays is how far back the ledger reads.
	printDemandWindowDays = 14
	// printDemandTTL is how long one read stands.
	printDemandTTL = 30 * time.Minute
	// printDemandMinISK is the least NPC spend on a type over the window that
	// counts as demand. Under it one pilot bought one print.
	printDemandMinISK = 5e6
	// printDemandDeepISK is the spend from which a type is copied for sale in
	// the selling batch (copyStockTargetSelling) rather than the stock one,
	// and from which a corporation will buy an original just to copy it.
	printDemandDeepISK = 50e6
	// catBlueprint is the blueprint category.
	catBlueprint = 9
)

// printDemandRow is one region's NPC spend on one blueprint type.
type printDemandRow struct {
	RegionID int64
	Buys     int64
	ISK      float64
}

// printDemandBook is the ledger, indexed by blueprint type. Shared by every
// reader and never mutated after load.
type printDemandBook struct {
	byType map[int64][]printDemandRow
}

// total is one type's cluster-wide demand. Nil-safe.
func (pb *printDemandBook) total(bpTypeID int64) (buys int64, isk float64) {
	if pb == nil {
		return 0, 0
	}
	for _, r := range pb.byType[bpTypeID] {
		buys += r.Buys
		isk += r.ISK
	}
	return buys, isk
}

// byRegion is one type's demand per region. Nil-safe; the map is fresh.
func (pb *printDemandBook) byRegion(bpTypeID int64) map[int64]float64 {
	out := map[int64]float64{}
	if pb == nil {
		return out
	}
	for _, r := range pb.byType[bpTypeID] {
		out[r.RegionID] += r.ISK
	}
	return out
}

// deepTypes is every type whose demand reaches minISK, biggest spend first.
// Nil-safe.
func (pb *printDemandBook) deepTypes(minISK float64) []int64 {
	if pb == nil {
		return nil
	}
	type kv struct {
		t   int64
		isk float64
	}
	var list []kv
	for t := range pb.byType {
		if _, isk := pb.total(t); isk >= minISK {
			list = append(list, kv{t, isk})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].isk != list[j].isk {
			return list[i].isk > list[j].isk
		}
		return list[i].t < list[j].t
	})
	out := make([]int64, len(list))
	for i, e := range list {
		out[i] = e.t
	}
	return out
}

// printDemand is the cached ledger. A failed read answers nil, which every
// reader treats as no demand anywhere.
func (m *Mod) printDemand(ctx context.Context) *printDemandBook {
	pb, err := snapshotFor(snapKey("npcprintdemand"), printDemandTTL, func() (*printDemandBook, error) {
		sctx, cancel := snapContext(ctx)
		defer cancel()
		return m.loadPrintDemand(sctx, clock.Unix())
	})
	if err != nil {
		return nil
	}
	return pb
}

// loadPrintDemand is the read itself, uncached, so the harness can run it on
// a fixture. now is unix seconds; transactionDateTime is Win32 FILETIME.
func (m *Mod) loadPrintDemand(ctx context.Context, now int64) (*printDemandBook, error) {
	rows, err := m.db.QueryContext(ctx, capStatement(`
		SELECT t.regionID, t.typeID, COUNT(*), SUM(t.price * t.quantity)
		FROM market_transactions t
		JOIN rolebot_characters rb ON rb.characterID = t.characterID
		JOIN invTypes it ON it.typeID = t.typeID
		JOIN invGroups g ON g.groupID = it.groupID
		WHERE t.transactionType = 1 AND t.transactionDateTime >= ?
		  AND t.clientID < ? AND g.categoryID = ?
		GROUP BY t.regionID, t.typeID`),
		evedb.UnixToWin32(now-printDemandWindowDays*86400), evedb.MinPlayerEntityID, catBlueprint)
	if err != nil {
		return nil, fmt.Errorf("npc print demand: %w", err)
	}
	defer rows.Close()
	book := &printDemandBook{byType: map[int64][]printDemandRow{}}
	for rows.Next() {
		var r printDemandRow
		var t int64
		if err := rows.Scan(&r.RegionID, &t, &r.Buys, &r.ISK); err != nil {
			return nil, fmt.Errorf("npc print demand row: %w", err)
		}
		if r.Buys <= 0 || r.ISK <= 0 {
			continue
		}
		book.byType[t] = append(book.byType[t], r)
	}
	return book, rows.Err()
}

// saleOfficeFor picks where a copy of a type should be listed: the office
// (station to region) in the region with the most demand for it. 0 when no
// office region carries demand; the caller lists at the factory. Ties go to the
// lower stationID. Pure.
func saleOfficeFor(demand map[int64]float64, offices map[int64]int64) int64 {
	var best int64
	bestISK := 0.0
	for station, region := range offices {
		isk := demand[region]
		if isk <= 0 {
			continue
		}
		if isk > bestISK || (isk == bestISK && station < best) {
			best, bestISK = station, isk
		}
	}
	return best
}

// corpOfficeRegions is every station the corporation holds an office at,
// with its region.
func (m *Mod) corpOfficeRegions(ctx context.Context, corpID int64) map[int64]int64 {
	out := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, s.regionID FROM crpoffices o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ?`, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st, rg int64
		if rows.Scan(&st, &rg) == nil {
			out[st] = rg
		}
	}
	return out
}

// copyPOScan bounds how many of the deepest-demand types one purchase pass
// weighs.
const copyPOScan = 40

// postCopyDemandPurchase is the library purchase's second pass: an original the
// corporation does not hold, of a type the roster keeps buying from NPCs, bought
// to be copied for sale. It reuses the product pass's purchase order (postPO)
// and one-open-order limit on its own cadence (pocopy).
func (m *Mod) postCopyDemandPurchase(ctx context.Context, c *corpCEO, ch Charter, stationID int64, budget float64) {
	if budget <= 0 || clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "pocopy") < corpPOEverySec {
		return
	}
	var openPOs int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_po WHERE corporationID = ? AND status = 'open'`,
		c.CorpID).Scan(&openPOs)
	if openPOs >= poMaxOpen {
		return
	}
	p, err := m.loadCorpPolicy(ctx, c.CorpID)
	if err != nil || p.DeputyID == 0 {
		return
	}
	canCopy := false
	for st := range m.corpWorkStations(ctx, c.CorpID, stationID) {
		if m.labStationHasLine(ctx, st, ramActivityCopying) {
			canCopy = true
			break
		}
	}
	if !canCopy {
		return
	}
	pd := m.printDemand(ctx)
	ceo := pilotRace{Race: raceOf(c.CharID), Phobia: ch.Xenophobia}
	if ceo.Race == 0 {
		ceo.Race = m.charRace(ctx, c.CharID)
	}
	frac := m.bpcPriceFrac(ctx)
	taken := m.printsTakenElsewhere(ctx, c.CorpID)
	var bestType int64
	var bestPrice, bestScore float64
	for i, bp := range pd.deepTypes(printDemandDeepISK) {
		if i >= copyPOScan {
			break
		}
		if m.libraryCanBuild(ctx, c.CorpID, bp) || taken[bp] {
			continue // ours already, or another house is on it
		}
		f, ferr := m.printFactsFor(ctx, bp)
		if ferr != nil || f.ProductTypeID == 0 || f.TechLevel >= 2 || m.metaGroupOf(ctx, f.ProductTypeID) != 0 {
			continue
		}
		raceMul, ok := xenoBuildMul(ceo, m.hullRace(ctx, f.ProductTypeID))
		if !ok {
			continue
		}
		buys, isk := pd.total(bp)
		price := isk / float64(buys) // what the roster paid the NPC shelf
		if price <= 0 || price*poAdvanceMarkup > budget {
			continue
		}
		copyPrice := bpcPrice(price, f.CopyRuns, f.MaxRuns, 0, 0, frac)
		if !copySalesPayBack(buys, copyPrice, price*poAdvanceMarkup) {
			continue
		}
		if score := isk * corpCopyDemandShare / price * raceMul; score > bestScore {
			bestType, bestPrice, bestScore = bp, price, score
		}
	}
	if bestType == 0 {
		return
	}
	if err := m.postPO(ctx, c, p.DeputyID, bestType, stationID, bestPrice, false); err != nil {
		if !errors.Is(err, errPOOverCap) {
			m.logBot(c.CharID, logError, "ceo", "copy-service purchase order: %v", err)
		}
		return
	}
	buys, isk := pd.total(bestType)
	m.ceoLog(ctx, c, powerProduction, "pocopy", bestType, bestPrice,
		"buying a %s to copy for sale: the roster bought %d from NPC shelves for %s in two weeks",
		m.typeNameOr(ctx, bestType), buys, iskString(isk))
}

// printsTakenElsewhere is every print type another corporation has an open
// purchase order for or added to its library in the last day. Every house
// ranks the same roster-wide demand, so without this they all bought its top
// type in the same hour.
func (m *Mod) printsTakenElsewhere(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	ids, err := int64Column(ctx, m, `
		SELECT bpTypeID FROM rolebot_corp_po WHERE corporationID <> ? AND status = 'open'
		UNION
		SELECT bpTypeID FROM rolebot_corp_library WHERE corporationID <> ? AND acquiredAt > ?`,
		corpID, corpID, clock.Unix()-86400)
	if err == nil {
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}
