package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	evedb "gurimoon/db"
)

// The blueprint library: the corporation's own prints, and how it gets them. It is kept in the Blueprint
// Library division of the factory office, where builders run prints as corporation jobs (db/ramcorp.go)
// and cannot carry them away. rolebot_corp_library is a reconciled view of the entity table: every think,
// each original print in any of the corporation's offices is upserted and anything gone is dropped. The
// table adds only what entity cannot say: where a print came from, what it cost, and who gave it.
// Prints arrive by donation (a member on its visit home gives a print it holds twice or keeps idle, credited
// as equity at the print's value in rolebot_corp_equity) or by purchase (the production power advances ISK
// to the deputy under a purchase order; the deputy buys through the ordinary blueprint hunt and deposits
// the print; the cost is booked and the rest handed back; a pilot that leaves holding an advance has
// defaulted and it is written off). Prints a human director drops in are found by the reconcile, no equity.

const (
	// corpLibraryDivisions is where a library print may sit: Materials, the
	// Library, Production, or a job.
	corpLibraryDivisions = "4, 116, 117, 111"
	poAdvanceMarkup      = 1.25 // advance over the print's value
	poMaxOpen            = 1
	poStaleSec           = 5 * 86400
	donatePrintIdleSec   = 30 * 86400
	donatePrintMinValue  = 50000
)

// reconcileLibrary brings rolebot_corp_library in line with the offices, and
// files any loose print into the library division.
func (m *Mod) reconcileLibrary(ctx context.Context, corpID int64) (int, error) {
	now := clock.Unix()
	// Prints left in Materials or Production go on the shelf where builders
	// look for them. Never one in a job (flag 111): its flag is the job's.
	if _, err := m.db.ExecContext(ctx, `
		UPDATE entity e
		  JOIN crpoffices o ON o.itemID = e.locationID AND o.corporationID = e.ownerID
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		 SET e.flag = ?
		 WHERE o.corporationID = ? AND e.flag IN (?, ?)`,
		corpDivLibrary, corpID, corpDivMaterials, corpDivProduct); err != nil {
		return 0, fmt.Errorf("library shelve: %w", err)
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_library (itemID, corporationID, bpTypeID, stationID, source, acquiredAt, lastSeenAt)
		SELECT e.itemID, o.corporationID, e.typeID, o.stationID, 'found', ?, ?
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE o.corporationID = ? AND e.flag IN (`+corpLibraryDivisions+`) AND COALESCE(b.copy, 0) = 0
		ON DUPLICATE KEY UPDATE lastSeenAt = VALUES(lastSeenAt), stationID = VALUES(stationID),
		                        corporationID = VALUES(corporationID)`,
		now, now, corpID)
	if err != nil {
		return 0, fmt.Errorf("library reconcile: %w", err)
	}
	_ = res
	if _, err := m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_library WHERE corporationID = ? AND lastSeenAt < ?`, corpID, now); err != nil {
		return 0, fmt.Errorf("library prune: %w", err)
	}
	var n int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_library WHERE corporationID = ?`, corpID).Scan(&n)
	return n, nil
}

// libraryPrint is one print on the shelf.
type libraryPrint struct {
	ItemID, BPTypeID, StationID int64
	Flag                        int64
	ME, PE                      int64
}

// libraryPrints reads the corporation's prints at one office station, with
// their research levels; inJob prints included (Flag 111).
func (m *Mod) libraryPrints(ctx context.Context, corpID, stationID int64) ([]libraryPrint, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, o.stationID, e.flag,
		       COALESCE(b.materialLevel, 0), COALESCE(b.productivityLevel, 0)
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE o.corporationID = ? AND o.stationID = ? AND e.flag IN (`+corpLibraryDivisions+`)
		   AND COALESCE(b.copy, 0) = 0
		 ORDER BY e.typeID, COALESCE(b.materialLevel, 0) DESC, e.itemID`, corpID, stationID)
	if err != nil {
		return nil, fmt.Errorf("libraryPrints: %w", err)
	}
	defer rows.Close()
	var out []libraryPrint
	for rows.Next() {
		var p libraryPrint
		if err := rows.Scan(&p.ItemID, &p.BPTypeID, &p.StationID, &p.Flag, &p.ME, &p.PE); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// libraryPrintsAll is libraryPrints over every office the corporation holds.
func (m *Mod) libraryPrintsAll(ctx context.Context, corpID int64) ([]libraryPrint, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, o.stationID, e.flag,
		       COALESCE(b.materialLevel, 0), COALESCE(b.productivityLevel, 0)
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE o.corporationID = ? AND e.flag IN (`+corpLibraryDivisions+`)
		   AND COALESCE(b.copy, 0) = 0
		 ORDER BY e.typeID, COALESCE(b.materialLevel, 0) DESC, e.itemID`, corpID)
	if err != nil {
		return nil, fmt.Errorf("libraryPrintsAll: %w", err)
	}
	defer rows.Close()
	var out []libraryPrint
	for rows.Next() {
		var p libraryPrint
		if err := rows.Scan(&p.ItemID, &p.BPTypeID, &p.StationID, &p.Flag, &p.ME, &p.PE); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// libraryCopy is one blueprint COPY in the corporation's office.
type libraryCopy struct {
	ItemID, BPTypeID, StationID int64
	Flag                        int64
	ME, PE, Runs                int64
}

// libraryCopies reads the corporation's blueprint copies at one office station, made by its R&D arm for
// builders to run beside the original or for sale. libraryPrints stays originals-only: a copy can be
// neither researched nor copied.
func (m *Mod) libraryCopies(ctx context.Context, corpID, stationID int64) ([]libraryCopy, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, o.stationID, e.flag,
		       b.materialLevel, b.productivityLevel, b.licensedProductionRunsRemaining
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		  JOIN invBlueprints b ON b.blueprintID = e.itemID AND b.copy = 1
		 WHERE o.corporationID = ? AND o.stationID = ? AND e.flag IN (`+corpLibraryDivisions+`)
		   AND b.licensedProductionRunsRemaining > 0
		 ORDER BY e.typeID, b.licensedProductionRunsRemaining DESC, e.itemID`, corpID, stationID)
	if err != nil {
		return nil, fmt.Errorf("libraryCopies: %w", err)
	}
	defer rows.Close()
	var out []libraryCopy
	for rows.Next() {
		var c libraryCopy
		if err := rows.Scan(&c.ItemID, &c.BPTypeID, &c.StationID, &c.Flag, &c.ME, &c.PE, &c.Runs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// libraryCopiesAll is libraryCopies over every office the corporation holds.
func (m *Mod) libraryCopiesAll(ctx context.Context, corpID int64) ([]libraryCopy, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, o.stationID, e.flag,
		       b.materialLevel, b.productivityLevel, b.licensedProductionRunsRemaining
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		  JOIN invBlueprints b ON b.blueprintID = e.itemID AND b.copy = 1
		 WHERE o.corporationID = ? AND e.flag IN (`+corpLibraryDivisions+`)
		   AND b.licensedProductionRunsRemaining > 0
		 ORDER BY e.typeID, b.licensedProductionRunsRemaining DESC, e.itemID`, corpID)
	if err != nil {
		return nil, fmt.Errorf("libraryCopiesAll: %w", err)
	}
	defer rows.Close()
	var out []libraryCopy
	for rows.Next() {
		var c libraryCopy
		if err := rows.Scan(&c.ItemID, &c.BPTypeID, &c.StationID, &c.Flag, &c.ME, &c.PE, &c.Runs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// libraryCanBuild reports whether the corporation can build the type from its own shelf: an original in
// the library, or a copy with runs left in any office's library divisions. libraryHas is the test for
// originals alone, which research and copying need.
func (m *Mod) libraryCanBuild(ctx context.Context, corpID, bpTypeID int64) bool {
	if m.libraryHas(ctx, corpID, bpTypeID) {
		return true
	}
	return m.libraryCopyRuns(ctx, corpID, 0, bpTypeID) > 0
}

// libraryCopyRuns is the build runs left on the corporation's copies of a
// type, at one office station (or every office, stationID 0).
func (m *Mod) libraryCopyRuns(ctx context.Context, corpID, stationID, bpTypeID int64) int64 {
	var runs sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT SUM(b.licensedProductionRunsRemaining)
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		  JOIN invBlueprints b ON b.blueprintID = e.itemID AND b.copy = 1
		 WHERE o.corporationID = ? AND (? = 0 OR o.stationID = ?) AND e.typeID = ?
		   AND e.flag IN (`+corpLibraryDivisions+`) AND b.licensedProductionRunsRemaining > 0`,
		corpID, stationID, stationID, bpTypeID).Scan(&runs)
	return runs.Int64
}

// libraryHas reports whether the corporation holds a print of the type.
func (m *Mod) libraryHas(ctx context.Context, corpID, bpTypeID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_library WHERE corporationID = ? AND bpTypeID = ?`,
		corpID, bpTypeID).Scan(&n)
	return n > 0
}

// fileIntoLibrary moves a pilot's print into the corporation's library at an
// office and records where it came from. The ownership change is the whole
// transfer: a hangar item is its owner, location and flag.
func (m *Mod) fileIntoLibrary(ctx context.Context, corpID, officeID, stationID, fromCharID, itemID int64,
	source string, cost float64) error {
	res, err := m.db.ExecContext(ctx, `
		UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
		 WHERE itemID = ? AND ownerID = ?`, corpID, officeID, corpDivLibrary, itemID, fromCharID)
	if err != nil {
		return fmt.Errorf("file print %d: %w", itemID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("file print %d: no longer %d's to give", itemID, fromCharID)
	}
	var bpType int64
	_ = m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, itemID).Scan(&bpType)
	now := clock.Unix()
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_library
			(itemID, corporationID, bpTypeID, stationID, source, cost, donorID, acquiredAt, lastSeenAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), source = VALUES(source),
			cost = VALUES(cost), donorID = VALUES(donorID), acquiredAt = VALUES(acquiredAt),
			lastSeenAt = VALUES(lastSeenAt), stationID = VALUES(stationID)`,
		itemID, corpID, bpType, stationID, source, cost, fromCharID, now, now)
	if err != nil {
		return err
	}
	m.lockArrivedPrint(ctx, corpID, itemID)
	return nil
}

// ── donation ─────────────────────────────────────────────────────────────────

// donatePrintToCorp is a member's visit-home gift of a print the library lacks, or a second original of a
// bottleneck type (corpbottleneck.go). Only at the corporation's factory, one print a visit.
func (m *Mod) donatePrintToCorp(ctx context.Context, b *botRun) {
	if b == nil || b.CorpID == 0 || b.State == nil {
		return
	}
	if _, ok := managedCEO(b.CorpID); !ok {
		return
	}
	// Only a corporation with an industry desk builds from a library; a
	// print given to any other would sit on a shelf nothing reads.
	if facts, err := getAnyCorpFacts(ctx, m.db, b.CorpID); err != nil || !hasIndustryDesk(facts.Policy.Tags) {
		return
	}
	stationID, officeID := m.corpFactoryOffice(ctx, b.CorpID)
	if stationID == 0 || b.StationID != stationID {
		return
	}
	d := b.State.Data
	busy := map[int64]bool{d.IndBPItemID: true, d.IndFetchItemID: true}
	for _, j := range d.IndCooking {
		busy[j.BPItemID] = true
	}
	// Originals in its hangar here whose type the library lacks (or wants a second of), with how many it owns
	// anywhere and when it last put one on a line.
	wants, wantArgs := libraryWantsTypeSQL(b.CorpID)
	args := append([]any{b.CharID, b.StationID}, wantArgs...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID,
		       (SELECT COUNT(*) FROM entity e2 LEFT JOIN invBlueprints b2 ON b2.blueprintID = e2.itemID
		         WHERE e2.ownerID = e.ownerID AND e2.typeID = e.typeID AND COALESCE(b2.copy, 0) = 0),
		       COALESCE((SELECT MAX(j.installTime) FROM ramJobs j
		                  WHERE j.installerID = e.ownerID AND j.installedItemID = e.itemID), 0)
		  FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND COALESCE(b.copy, 0) = 0
		   AND `+wants+`
		 LIMIT 40`, args...)
	if err != nil {
		return
	}
	type cand struct {
		itemID, typeID int64
		owned          int
		lastUsed       int64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.itemID, &c.typeID, &c.owned, &c.lastUsed) == nil && !busy[c.itemID] {
			cands = append(cands, c)
		}
	}
	rows.Close()
	if len(cands) == 0 {
		return
	}
	idleCut := evedb.UnixToWin32(clock.Unix() - donatePrintIdleSec)
	best, bestVal := cand{}, 0.0
	for _, c := range cands {
		duplicate := c.owned >= 2
		idle := c.lastUsed < idleCut
		if !duplicate && !idle {
			continue
		}
		// A print kept idle is given only as often as the pilot is loyal;
		// a spare copy is given whenever the library lacks one.
		if !duplicate && roll01(b.CharID, c.itemID, clock.Unix()/86400) > 0.25*b.Tune.Obedience {
			continue
		}
		v := evedb.TypeValueOf(ctx, m.db, c.typeID).Value
		if v >= donatePrintMinValue && v > bestVal {
			best, bestVal = c, v
		}
	}
	if best.itemID == 0 {
		return
	}
	if err := m.fileIntoLibrary(ctx, b.CorpID, officeID, stationID, b.CharID, best.itemID, "donation", bestVal); err != nil {
		m.logBot(b.CharID, logError, "corp", "giving a print to its corporation: %v", err)
		return
	}
	// Paid for in stock at the ask (corpshares.go), as far as the treasury
	// has any to sell; a print given past that is a gift.
	paid := m.creditSharesInKind(ctx, b.CorpID, b.CharID, bestVal, "a "+m.typeNameOr(ctx, best.typeID))
	m.logBot(b.CharID, logInfo, "corp",
		"gave its corporation's library a %s (worth %s) — paid %d shares for it",
		m.typeNameOr(ctx, best.typeID), iskString(bestVal), paid)
	if c, _ := getCEO(ctx, m.db, b.CorpID); c != nil {
		m.ceoLog(ctx, c, powerProduction, "donateprint", b.CharID, bestVal,
			"%s gave the library a %s", m.charNameOr(ctx, b.CharID), m.typeNameOr(ctx, best.typeID))
	}
}

// roll01 is a deterministic [0,1) draw keyed on its arguments, so a
// decision is stable across passes on the same day.
func roll01(a, b, c int64) float64 {
	x := splitmix64(uint64(a)*0x9E3779B97F4A7C15 ^ uint64(b)*0xBF58476D1CE4E5B9 ^ uint64(c))
	return float64(x>>11) / float64(uint64(1)<<53)
}

// ── purchase orders ──────────────────────────────────────────────────────────

// corpPO is one purchase order.
type corpPO struct {
	POID, CorpID, CharID, BPTypeID, StationID int64
	Advance, Spent                            float64
	Status                                    string
	OpenedAt                                  int64
	// AllowCopy lets the buyer fill the order with a blueprint copy from
	// escrow (corpPOArm). Set on a shipyard's hull orders.
	AllowCopy bool
}

// openPOFor is a pilot's open purchase order, if any.
func (m *Mod) openPOFor(ctx context.Context, charID int64) (*corpPO, error) {
	// Who holds an open order at all, shared for a minute: every managed
	// member asked for its own row on every plan, and nearly none has one.
	holders, herr := snapshotFor(snapKey("po-holders"), time.Minute, func() (map[int64]bool, error) {
		rows, err := m.db.QueryContext(ctx, `SELECT DISTINCT characterID FROM rolebot_corp_po WHERE status = 'open'`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[int64]bool{}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				out[id] = true
			}
		}
		return out, rows.Err()
	})
	if herr == nil && !holders[charID] {
		return nil, nil
	}
	var po corpPO
	err := m.db.QueryRowContext(ctx, `
		SELECT poID, corporationID, characterID, bpTypeID, stationID, advance, spent, status, openedAt,
		       allowCopy
		  FROM rolebot_corp_po WHERE characterID = ? AND status = 'open'
		 ORDER BY poID LIMIT 1`, charID).
		Scan(&po.POID, &po.CorpID, &po.CharID, &po.BPTypeID, &po.StationID, &po.Advance, &po.Spent,
			&po.Status, &po.OpenedAt, &po.AllowCopy)
	if err != nil {
		return nil, nil
	}
	return &po, nil
}

// postPO orders one print for the library. No money moves yet: the corporation reserves the budget and
// the buyer is funded only when it commits to the trip (fundPO), so advanced ISK does not sit in a bot's
// wallet. allowCopy lets the buyer bring back a copy (a shipyard's hull orders); value is then the cheaper
// of the two asks.
func (m *Mod) postPO(ctx context.Context, c *corpCEO, buyer, bpTypeID, stationID int64, value float64, allowCopy bool) error {
	_, err := m.postPOWith(ctx, c, buyer, bpTypeID, stationID, value, allowCopy, false)
	return err
}

// postPOWith is postPO answering the order's id. fundBacked is an order a print fund saved for over weeks
// (corpfund.go): its reservation is the fund's own commit, so the one-think gate is not run.
func (m *Mod) postPOWith(ctx context.Context, c *corpCEO, buyer, bpTypeID, stationID int64, value float64,
	allowCopy, fundBacked bool) (int64, error) {
	budget := math.Ceil(value * poAdvanceMarkup)
	// The one gate every caller passes through: a print the corporation's production budget could not commit
	// to is not ordered, whoever asks.
	ch := poCharterOf(ctx, m, c)
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return 0, err
	}
	if fundBacked {
		// Saved money is still money that has to be there.
		if budget > free {
			return 0, fmt.Errorf("%w: the fund's %s against %s of free capital", errPOOverCap,
				iskString(budget), iskString(free))
		}
	} else if !poWithinCap(budget, ch.CapitalRisk, free) {
		return 0, fmt.Errorf("%w: %s for a %s against %s of free capital", errPOOverCap,
			iskString(budget), m.typeNameOr(ctx, bpTypeID), iskString(free))
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_po (corporationID, characterID, bpTypeID, stationID, advance, openedAt, allowCopy)
		VALUES (?, ?, ?, ?, 0, ?, ?)`, c.CorpID, buyer, bpTypeID, stationID, clock.Unix(), allowCopy)
	if err != nil {
		return 0, fmt.Errorf("postPO: %w", err)
	}
	poID, _ := res.LastInsertId()
	if err := m.openCommit(ctx, c.CorpID, "po", poID, budget); err != nil {
		return 0, err
	}
	m.ceoLog(ctx, c, powerProduction, "po", buyer, budget,
		"asked %s to buy a %s for the library (up to %s)", m.charNameOr(ctx, buyer),
		m.typeNameOr(ctx, bpTypeID), iskString(budget))
	return poID, nil
}

// poCharterOf resolves a CEO's charter for the purchase-order gate. It is a variable set in init because
// the charter resolver reaches the purchase-order arms, and a direct call is an initialization cycle.
var poCharterOf func(ctx context.Context, m *Mod, c *corpCEO) Charter

func init() { poCharterOf = ceoCharterOf }

// errPOOverCap is a purchase order the corporation's budget cannot commit
// to: a skip, not a failure.
var errPOOverCap = errors.New("over the production budget")

// poWithinCap reports whether an order's reservation fits the share of free
// capital the charter lets production commit (CapitalRisk). Pure.
func poWithinCap(reserve, capitalRisk, free float64) bool {
	return reserve > 0 && capitalRisk > 0 && reserve <= capitalRisk*free
}

// poFundFor is what a buyer is advanced for a print asking price: the price
// and a small margin for a move between the scan and the counter, never past
// the budget the order reserved. 0 means the print costs more than that.
// Pure, for the test.
func poFundFor(ask, budget float64) float64 {
	if ask <= 0 || ask > budget {
		return 0
	}
	return math.Min(budget, math.Ceil(ask*1.05))
}

// fundPO advances the buyer the money for the print it is about to fly for.
func (m *Mod) fundPO(ctx context.Context, po *corpPO, ask float64) error {
	var budget float64
	_ = m.db.QueryRowContext(ctx,
		`SELECT amount FROM rolebot_corp_commit WHERE refKind = 'po' AND refID = ?`, po.POID).Scan(&budget)
	fund := poFundFor(ask, budget)
	if fund <= 0 {
		return fmt.Errorf("the print asks %s, over the %s the order allows", iskString(ask), iskString(budget))
	}
	// Checked again at the counter, hours after posting, because the treasury may have been spent. This
	// order's own reservation is added back. A fund-backed order (corpfund.go) is measured against the wallet alone.
	if c, _ := getCEO(ctx, m.db, po.CorpID); c != nil && !m.poFundBacked(ctx, po.POID) {
		ch := poCharterOf(ctx, m, c)
		free, _, ferr := m.corpFreeCapital(ctx, po.CorpID, ch)
		if ferr != nil {
			return ferr
		}
		var spent float64
		_ = m.db.QueryRowContext(ctx,
			`SELECT spent FROM rolebot_corp_commit WHERE refKind = 'po' AND refID = ?`, po.POID).Scan(&spent)
		if !poWithinCap(fund, ch.CapitalRisk, free+math.Max(budget-spent, 0)) {
			return fmt.Errorf("%w: a %s advance against %s of free capital", errPOOverCap,
				iskString(fund), iskString(free))
		}
	}
	if _, _, err := m.corpWithdraw(ctx, po.CorpID, po.CharID, fund,
		"Advance to buy a blueprint for the corporation"); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE rolebot_corp_po SET advance = ? WHERE poID = ?`, fund, po.POID); err != nil {
		return err
	}
	m.spendCommit(ctx, "po", po.POID, fund)
	po.Advance = fund
	return nil
}

// settlePO closes a purchase order: the print is on the shelf, what it cost
// is the corporation's, and the rest of the advance comes back.
func (m *Mod) settlePO(ctx context.Context, po *corpPO, itemID int64, spent float64) {
	refund := math.Floor(po.Advance - spent)
	if refund >= 1 {
		if _, _, err := evedb.TransferISK(ctx, m.db, po.CharID, po.CorpID, refund, refPlayerDonation,
			"Unspent blueprint advance returned"); err != nil && !errors.Is(err, evedb.ErrInsufficientFunds) {
			rlogf("rolebots: PO %d refund: %v", po.POID, err)
		}
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_po SET status = 'done', spent = ?, itemID = ?, closedAt = ? WHERE poID = ?`,
		spent, itemID, clock.Unix(), po.POID)
	m.closeCommit(ctx, "po", po.POID)
	m.corpBook(ctx, po.CorpID, lineBPO, -spent, po.BPTypeID, "blueprint for the library: "+m.typeNameOr(ctx, po.BPTypeID))
	if c, _ := getCEO(ctx, m.db, po.CorpID); c != nil {
		m.ceoLog(ctx, c, powerProduction, "bpo", po.CharID, spent,
			"%s delivered a %s to the library for %s", m.charNameOr(ctx, po.CharID),
			m.typeNameOr(ctx, po.BPTypeID), iskString(spent))
	}
	m.fundPODelivered(ctx, po)
}

// voidPO cancels a purchase order and takes the advance back from the buyer.
func (m *Mod) voidPO(ctx context.Context, po *corpPO, why string) {
	take := po.Advance
	if bal, err := evedb.GetCharBalance(ctx, m.db, po.CharID); err == nil {
		take = math.Min(take, bal)
	}
	status := "void"
	if take >= 1 {
		if _, _, err := evedb.TransferISK(ctx, m.db, po.CharID, po.CorpID, math.Floor(take), refPlayerDonation,
			"Blueprint advance returned"); err != nil {
			take = 0
		}
	}
	if take < po.Advance-1 {
		status = "default"
		m.corpBook(ctx, po.CorpID, lineBPO, -(po.Advance - take), po.CharID, "blueprint advance written off")
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_po SET status = ?, closedAt = ? WHERE poID = ?`, status, clock.Unix(), po.POID)
	m.closeCommit(ctx, "po", po.POID)
	m.fundPOVoided(ctx, po)
	if c, _ := getCEO(ctx, m.db, po.CorpID); c != nil {
		m.ceoLog(ctx, c, powerProduction, "povoid", po.CharID, po.Advance,
			"cancelled %s's order for a %s — %s", m.charNameOr(ctx, po.CharID), m.typeNameOr(ctx, po.BPTypeID), why)
	}
}

// sweepPOs voids orders that went stale or whose buyer left the corporation.
func (m *Mod) sweepPOs(ctx context.Context, corpID int64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT p.poID, p.corporationID, p.characterID, p.bpTypeID, p.stationID, p.advance, p.spent, p.status,
		       p.openedAt, COALESCE(ch.corporationID, 0)
		  FROM rolebot_corp_po p
		  LEFT JOIN character_ ch ON ch.characterID = p.characterID
		 WHERE p.corporationID = ? AND p.status = 'open'`, corpID)
	if err != nil {
		return
	}
	type row struct {
		po  corpPO
		now int64
	}
	var list []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.po.POID, &r.po.CorpID, &r.po.CharID, &r.po.BPTypeID, &r.po.StationID, &r.po.Advance,
			&r.po.Spent, &r.po.Status, &r.po.OpenedAt, &r.now) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	for _, r := range list {
		switch {
		case r.now != corpID:
			// Left holding the corporation's money: a default. HR reads
			// rolebot_corp_po for these and never takes the pilot back.
			m.voidPO(ctx, &r.po, "the buyer left the corporation")
		case clock.Unix()-r.po.OpenedAt > poStaleSec:
			m.voidPO(ctx, &r.po, "it was never delivered")
		}
	}
}

// defaultedOnCorp reports whether a pilot ever walked off with a corporation's advance; HR will not take
// it back.
func (m *Mod) defaultedOnCorp(ctx context.Context, corpID, charID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_po WHERE corporationID = ? AND characterID = ? AND status = 'default'`,
		corpID, charID).Scan(&n)
	return n > 0
}
