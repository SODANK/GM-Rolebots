package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Offices: what a bot-run corporation holds, and paying for it. ceoTryRentOffice (corpceo_estate.go) pays
// the first month up front. After that the core's bill sweep (billwrites.go) issues a rental bill every 30
// days and evicts the office when it is unpaid past its due date, so the CEO pays from the corporation's wallet.
// Strict priority, in the order the CEO would keep last: the headquarters, the factory, then everything else
// by how many members live there (the posted Base once a member has one, else where it keeps its clone).
// Once an office in that order cannot be paid nothing below it is paid, so the money is held for the one above.
// A short bill is not given up on at once. Only when its due date is inside rentDropLeadSec does the CEO let
// the office go: it is marked 'dropping', the corporation's bids there come off the book, its stock is carried
// to the factory (corpfactorymove.go), and once that move has closed the office is released and its bill
// written off. An eviction that beats the move costs only the impound. The headquarters is never dropped.

// Office purposes, as a bitmask on rolebot_corp_offices.purposes. One office
// can serve several (a factory with research lines is FACTORY|LAB).
const (
	officeHQ = 1 << iota
	officeFactory
	officeLab
	officeMining
	officeStaging
	officeSales
	officeBranch
	// officeYard: a capital program's yard, a station whose manufacturing lines may build the hull when the
	// factory's may not (corpprogram.go).
	officeYard
	// officeForward: a jumping-off point on the frontier, a station in or near unclaimed 0.0 that towers
	// there are staged and fuelled from (frontier.go).
	officeForward
	// officeFront: the station beside 0.0 the alliance holds, where the field arms stage out to
	// (corpfront.go).
	officeFront
	// officeCapital: a 0.0 station the corporation owns, where every arm lives (corpfront.go).
	officeCapital
)

const (
	// rentDropLeadSec is how close to its due date an unpaid rental bill may get before the CEO gives the
	// office up. It leaves time for one or two hauls of stock to the factory.
	rentDropLeadSec = 2 * 86400

	// rentShortNoteSec throttles the "cannot pay yet" ledger line.
	rentShortNoteSec = 12 * 3600
)

// rentOffice is one office as the rent pass sees it.
type rentOffice struct {
	StationID int64
	OfficeID  int64
	HQ        bool
	Factory   bool
	Members   int
	Dropping  bool

	// The oldest unpaid rental bill against it, 0 for none.
	BillID int64
	Amount float64
	DueAt  int64 // unix
}

// rankRentOffices orders offices by what the corporation keeps last: HQ,
// factory, then members living there, then station ID so the order is
// stable. Pure, for the test.
func rankRentOffices(list []rentOffice) []rentOffice {
	out := append([]rentOffice(nil), list...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.HQ != b.HQ {
			return a.HQ
		}
		if a.Factory != b.Factory {
			return a.Factory
		}
		if a.Members != b.Members {
			return a.Members > b.Members
		}
		return a.StationID < b.StationID
	})
	return out
}

// rentPlan is what one rent pass does: bills to pay, offices to give up,
// offices short but not yet due.
type rentPlan struct {
	Pay   []rentOffice
	Drop  []rentOffice
	Short []rentOffice
}

// planRent walks the ranked offices with the money there is. Strict
// priority: the first office it cannot pay holds the rest of the money, so
// nothing ranked below it is paid. A short office is dropped only when its
// bill is due within rentDropLeadSec, and never the HQ. Pure, for the test.
func planRent(ranked []rentOffice, balance float64, now int64) rentPlan {
	var p rentPlan
	blocked := false
	for _, o := range ranked {
		if o.BillID == 0 {
			continue
		}
		if !blocked && o.Amount <= balance {
			p.Pay = append(p.Pay, o)
			balance -= o.Amount
			continue
		}
		blocked = true
		if !o.HQ && o.DueAt-now <= rentDropLeadSec {
			p.Drop = append(p.Drop, o)
			continue
		}
		p.Short = append(p.Short, o)
	}
	return p
}

// syncCorpOffices keeps rolebot_corp_offices in step with crpoffices: a row
// for every office the corporation holds, none for one it lost (a human
// director's UNRENT, an eviction).
func (m *Mod) syncCorpOffices(ctx context.Context, corpID int64) {
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT IGNORE INTO rolebot_corp_offices (corporationID, stationID, rentedAt)
		SELECT corporationID, stationID, ? FROM crpoffices WHERE corporationID = ?`,
		now, corpID); err != nil {
		rlogf("rolebots: sync offices of %d: %v", corpID, err)
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		DELETE r FROM rolebot_corp_offices r
		 WHERE r.corporationID = ?
		   AND NOT EXISTS (SELECT 1 FROM crpoffices o
		                    WHERE o.corporationID = r.corporationID AND o.stationID = r.stationID)`,
		corpID); err != nil {
		rlogf("rolebots: sweep offices of %d: %v", corpID, err)
	}
}

// loadRentOffices reads the corporation's offices with their oldest unpaid
// rental bill and how many members live at each.
func (m *Mod) loadRentOffices(ctx context.Context, corpID int64) ([]rentOffice, error) {
	var hq int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(stationID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&hq)
	factory := m.corpFactoryStation(ctx, corpID)
	// Every department base ranks with the factory: an R&D base holds part of
	// the library, and losing it strands prints (corpbases.go).
	base := map[int64]bool{factory: true}
	for _, arm := range []corpArm{armResearch, armIndustry} {
		for _, st := range m.armBaseStations(ctx, corpID, arm) {
			base[st] = true
		}
	}

	// Members by where they live: the posted Base when a member has one,
	// otherwise its home (clone) station.
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.itemID, COALESCE(r.state, 'open'),
		       (SELECT COUNT(*) FROM character_ c
		          LEFT JOIN chrCorporationRoles cr ON cr.characterID = c.characterID
		         WHERE c.corporationID = o.corporationID
		           AND COALESCE(NULLIF(cr.baseID, 0), c.homeStationID) = o.stationID)
		  FROM crpoffices o
		  LEFT JOIN rolebot_corp_offices r
		         ON r.corporationID = o.corporationID AND r.stationID = o.stationID
		 WHERE o.corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("loadRentOffices: %w", err)
	}
	var out []rentOffice
	for rows.Next() {
		var o rentOffice
		var state string
		if err := rows.Scan(&o.StationID, &o.OfficeID, &state, &o.Members); err != nil {
			rows.Close()
			return nil, err
		}
		o.HQ = o.StationID == hq
		o.Factory = factory != 0 && base[o.StationID]
		o.Dropping = state == "dropping"
		out = append(out, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	bills, err := m.db.QueryContext(ctx, `
		SELECT billID, amount, dueDateTime, externalID2 FROM bills
		 WHERE debtorID = ? AND billTypeID = ? AND externalID = ? AND paid = 0
		 ORDER BY dueDateTime`, corpID, evedb.BillTypeRentalBill, evedb.TypeOfficeFolder)
	if err != nil {
		return out, nil // no bills table is no bills
	}
	defer bills.Close()
	byStation := make(map[int64]int, len(out))
	for i := range out {
		byStation[out[i].StationID] = i
	}
	for bills.Next() {
		var id, due, station int64
		var amount float64
		if bills.Scan(&id, &amount, &due, &station) != nil {
			continue
		}
		i, ok := byStation[station]
		if !ok || out[i].BillID != 0 {
			continue // an office already gone, or a later bill for one we hold
		}
		out[i].BillID, out[i].Amount, out[i].DueAt = id, amount, evedb.Win32ToUnix(due)
	}
	return out, nil
}

// ceoPayRent is one think's rent pass: pay what can be paid, give up what
// cannot, and finish giving up what was already being given up. Runs from
// the treasury power after the CEO has had its chance to back the
// corporation, so a bail-out is spendable here.
func (m *Mod) ceoPayRent(ctx context.Context, c *corpCEO) error {
	m.syncCorpOffices(ctx, c.CorpID)
	offices, err := m.loadRentOffices(ctx, c.CorpID)
	if err != nil {
		return err
	}
	balance, err := evedb.GetCorpBalance(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	now := clock.Unix()
	plan := planRent(rankRentOffices(offices), balance, now)

	hook := modapi.Corp().PayBill
	for _, o := range plan.Pay {
		if hook == nil {
			break
		}
		if err := hook(o.BillID, c.CharID); err != nil {
			m.logBot(c.CharID, logInfo, "ceo", "rent: pay bill %d: %v", o.BillID, err)
			continue
		}
		name := stationNameOf(ctx, m, o.StationID)
		m.corpBook(ctx, c.CorpID, lineRent, -o.Amount, o.StationID, "office rent at "+name)
		m.ceoLog(ctx, c, powerTreasury, "rent.pay", o.StationID, o.Amount,
			"paid %s rent for the office at %s", iskString(o.Amount), name)
		if o.Dropping {
			// The money turned up after all: keep the office.
			m.keepOffice(ctx, c, o)
		}
	}
	for _, o := range plan.Short {
		if now-m.lastCEOActionAt(ctx, c.CorpID, "rent.short") < rentShortNoteSec {
			break
		}
		m.ceoLog(ctx, c, powerTreasury, "rent.short", o.StationID, o.Amount,
			"cannot pay the %s rent on %s yet — due in %s",
			iskString(o.Amount), stationNameOf(ctx, m, o.StationID), fmtDuration(max(0, o.DueAt-now)))
	}
	for _, o := range plan.Drop {
		if !o.Dropping {
			m.dropOffice(ctx, c, o, "its rent cannot be paid")
		}
	}
	// A dropping office with nothing left owing was paid by somebody else, such as a director at the wallet's
	// Pay button. Keep it.
	for _, o := range offices {
		if o.Dropping && o.BillID == 0 {
			m.keepOffice(ctx, c, o)
		}
	}
	m.releaseDroppedOffices(ctx, c)
	return nil
}

// dropOffice starts giving an office up: bids off the book, the desk's stock
// on its way to the factory, the row marked so the release can finish it.
func (m *Mod) dropOffice(ctx context.Context, c *corpCEO, o rentOffice, why string) {
	if o.HQ {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_offices SET state = 'dropping', droppingAt = ?, note = ?
		 WHERE corporationID = ? AND stationID = ?`,
		clock.Unix(), clampText(why, 255), c.CorpID, o.StationID); err != nil {
		rlogf("rolebots: drop office %d/%d: %v", c.CorpID, o.StationID, err)
		return
	}
	m.cancelCorpBidsAt(ctx, c, o.StationID)
	// Every office but the factory itself sends its stock there; a given-up R&D base's prints go to the
	// factory's library.
	if o.StationID != m.corpFactoryStation(ctx, c.CorpID) && o.OfficeID != 0 && o.OfficeID != o.StationID {
		m.openFactoryMove(ctx, c.CorpID, o.OfficeID, o.StationID)
	}
	m.ceoLog(ctx, c, powerEstate, "rent.drop", o.StationID, o.Amount,
		"is giving up the office at %s — %s; its stock goes to the factory first",
		stationNameOf(ctx, m, o.StationID), why)
}

// keepOffice takes an office off the dropping list. Its move, if still open, is closed where it stands:
// what already reached the factory stays there.
func (m *Mod) keepOffice(ctx context.Context, c *corpCEO, o rentOffice) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_offices SET state = 'open', droppingAt = 0, note = ''
		 WHERE corporationID = ? AND stationID = ?`, c.CorpID, o.StationID); err != nil {
		rlogf("rolebots: keep office %d/%d: %v", c.CorpID, o.StationID, err)
		return
	}
	if mv, err := m.loadCorpMove(ctx, c.CorpID, o.OfficeID); err == nil && mv != nil &&
		mv.Status == "open" && len(mv.Carrying) == 0 {
		m.closeFactoryMove(ctx, mv, "kept", "the rent was paid after all")
	}
	m.ceoLog(ctx, c, powerEstate, "rent.keep", o.StationID, 0,
		"keeps the office at %s after all", stationNameOf(ctx, m, o.StationID))
}

// releaseDroppedOffices finishes giving up every dropping office whose stock
// has been carried off: the office released, its unpaid bill written off,
// the row gone.
func (m *Mod) releaseDroppedOffices(ctx context.Context, c *corpCEO) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.stationID, COALESCE(o.itemID, 0)
		  FROM rolebot_corp_offices r
		  LEFT JOIN crpoffices o ON o.corporationID = r.corporationID AND o.stationID = r.stationID
		 WHERE r.corporationID = ? AND r.state = 'dropping'`, c.CorpID)
	if err != nil {
		return
	}
	type drop struct{ station, office int64 }
	var list []drop
	for rows.Next() {
		var d drop
		if rows.Scan(&d.station, &d.office) == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	release := modapi.Corp().ReleaseOffice
	for _, d := range list {
		if d.office == 0 {
			// Evicted, or a director unrented it first.
			m.forgetOfficeRow(ctx, c.CorpID, d.station)
			continue
		}
		if !m.officeMoveSettled(ctx, c.CorpID, d.office) {
			continue
		}
		if release == nil {
			m.logBot(c.CharID, logInfo, "ceo", "rent: the server has no office-release hook")
			return
		}
		officeID, contents, err := release(c.CorpID, d.station)
		if err != nil {
			m.logBot(c.CharID, logError, "ceo", "rent: release office at %d: %v", d.station, err)
			continue
		}
		m.writeOffRentBills(ctx, c.CorpID, d.station)
		m.forgetOfficeRow(ctx, c.CorpID, d.station)
		if officeID == 0 {
			continue
		}
		note := ""
		if contents > 0 {
			note = fmt.Sprintf(" (%d item(s) left on the station floor)", contents)
		}
		m.ceoLog(ctx, c, powerEstate, "rent.release", d.station, 0,
			"gave up the office at %s%s", stationNameOf(ctx, m, d.station), note)
	}
}

// officeMoveSettled is whether a dropping office's stock is done moving: no
// move open off it, nothing in a carrier's hold, and nothing still coming
// back to it from a job.
func (m *Mod) officeMoveSettled(ctx context.Context, corpID, officeID int64) bool {
	mv, err := m.loadCorpMove(ctx, corpID, officeID)
	if err != nil {
		return false
	}
	if m.moveStillPending(ctx, corpID, officeID) {
		return false
	}
	if mv == nil || mv.Status != "open" {
		return true
	}
	if len(mv.Carrying) > 0 {
		return false
	}
	// An open move with nothing left to carry closes here, so a corporation with no director or deputy does
	// not hold the office until the core evicts it.
	stock, err := m.movableStock(ctx, corpID, officeID)
	if err != nil || len(stock) > 0 {
		return false
	}
	m.closeFactoryMove(ctx, mv, "done", "nothing left at the old office")
	return true
}

// writeOffRentBills closes the unpaid rental bills against an office the
// corporation no longer holds, so the next rent pass is not asked to pay
// for it.
func (m *Mod) writeOffRentBills(ctx context.Context, corpID, stationID int64) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE bills SET paid = 1
		 WHERE debtorID = ? AND billTypeID = ? AND externalID = ? AND externalID2 = ? AND paid = 0`,
		corpID, evedb.BillTypeRentalBill, evedb.TypeOfficeFolder, stationID); err != nil && err != sql.ErrNoRows {
		rlogf("rolebots: write off rent %d/%d: %v", corpID, stationID, err)
	}
}

func (m *Mod) forgetOfficeRow(ctx context.Context, corpID, stationID int64) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_corp_offices WHERE corporationID = ? AND stationID = ?`,
		corpID, stationID); err != nil {
		rlogf("rolebots: forget office %d/%d: %v", corpID, stationID, err)
	}
}
