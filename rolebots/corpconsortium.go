package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// The consortium: a capital program split across an alliance. No bot corporation
// affords a Charon's component prints and hull print alone (corpprogram.go), so
// the yard (the member running the program) files a request and the executor
// deals one component print to each managed member by free capital. That member
// buys it through its own program fund, builds the parts as board rows and hauls
// them to the yard on a program leg, the yard paying cost-plus per unit. The hull
// print is pledged: one tithe-shaped row per member (titheShares, kind pledge)
// paid to the yard, which repays pro rata on delivery. The yard owns the program,
// print and hull. Off unless rolebots.consortium.enabled (under the alliance
// layer); then a program is one corporation's alone.

const (
	consortiumEnabledKey = "rolebots.consortium.enabled"

	allianceReqProgram = "program" // rolebot_alliance_requests.kind
	titheKindPledge    = "pledge"  // rolebot_alliance_ledger.kind
	titheStatusRepaid  = "repaid"
	haulKindProgram    = "program" // rolebot_corp_haul.kind
	haulToYard         = "yard"    // rolebot_corp_haul.toKind: another corporation's office
	lineConsortium     = "consort"

	programRequestTTLSec = 30 * 86400
)

func consortiumOn(ctx context.Context, db *sql.DB) bool {
	return escrowSettingInt(ctx, db, consortiumEnabledKey, 0) != 0
}

// programRequestPayload is what a program request carries.
type programRequestPayload struct {
	ProgramID  int64 `json:"programID"`
	HullTypeID int64 `json:"hullTypeID"`
}

// assignSuppliers deals the unsupplied parts among the member corporations,
// richest first, one part each and round again when there are more parts
// than members. The yard keeps nothing while anyone else can take a part;
// with no members it keeps them all. Pure, for the test.
func assignSuppliers(parts []programPart, yard int64, members []int64) map[int64]int64 {
	out := map[int64]int64{}
	var pool []int64
	for _, mb := range members {
		if mb != yard && mb != 0 {
			pool = append(pool, mb)
		}
	}
	i := 0
	for _, p := range parts {
		if p.SupplierCorpID != 0 && p.SupplierCorpID != yard {
			continue // already dealt
		}
		if len(pool) == 0 {
			out[p.ComponentTypeID] = yard
			continue
		}
		out[p.ComponentTypeID] = pool[i%len(pool)]
		i++
	}
	return out
}

// fileProgramRequest is the yard asking its alliance to share a program's
// component prints. One pending request per program.
func (m *Mod) fileProgramRequest(ctx context.Context, c *corpCEO, seat *allianceSeat, p *corpProgram) {
	var open int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_alliance_requests
		 WHERE allianceID = ? AND fromCorpID = ? AND kind = ? AND targetID = ? AND status IN ('pending', 'taken')`,
		seat.AllianceID, c.CorpID, allianceReqProgram, p.ProgramID).Scan(&open)
	if open > 0 {
		return
	}
	payload, _ := json.Marshal(programRequestPayload{ProgramID: p.ProgramID, HullTypeID: p.HullTypeID})
	now := clock.Unix()
	hull := m.typeNameOr(ctx, p.HullTypeID)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_alliance_requests
		  (allianceID, fromCorpID, posterID, kind, targetID, systemID, payload, note, postedAt, expiresAt, status)
		VALUES (?, ?, 0, ?, ?, 0, ?, ?, ?, ?, 'pending')`,
		seat.AllianceID, c.CorpID, allianceReqProgram, p.ProgramID, string(payload),
		truncate("component prints for a "+hull, 255), now, now+programRequestTTLSec); err != nil {
		m.logBot(c.CharID, logError, "ceo", "program request: %v", err)
		return
	}
	m.ceoLog(ctx, c, powerProduction, "program", p.HullTypeID, 0,
		"asked the alliance to share the component prints of its %s program", hull)
	m.allianceBook(ctx, seat.AllianceID, c.CorpID, "program", p.HullTypeID, 0,
		"%s asks the alliance to build a %s together", ownerNameOf(ctx, m.db, c.CorpID), hull)
}

// programRequestOpen reports whether the yard's request is still with the
// executor (pending); the yard waits rather than funding every print itself.
func (m *Mod) programRequestOpen(ctx context.Context, programID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_alliance_requests WHERE kind = ? AND targetID = ? AND status = 'pending'`,
		allianceReqProgram, programID).Scan(&n)
	return n > 0
}

// allianceAssignPrograms is the executor's duty: every pending program
// request has its unsupplied parts dealt to managed members and the hull
// print pledged among them.
func (m *Mod) allianceAssignPrograms(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) error {
	if !consortiumOn(ctx, m.db) {
		return nil
	}
	reqs, err := m.allianceRequestsPending(ctx, seat.AllianceID, allianceReqProgram)
	if err != nil || len(reqs) == 0 {
		return err
	}
	// Managed members only, richest first: a human corporation is not handed
	// a print to buy.
	var pool []allianceMemberFacts
	for _, f := range members {
		if _, ok := managedCEO(f.CorpID); ok && !f.HumanCEO {
			pool = append(pool, f)
		}
	}
	for i := 0; i < len(pool); i++ {
		for j := i + 1; j < len(pool); j++ {
			if pool[j].Wallet > pool[i].Wallet {
				pool[i], pool[j] = pool[j], pool[i]
			}
		}
	}
	ids := make([]int64, 0, len(pool))
	for _, f := range pool {
		ids = append(ids, f.CorpID)
	}
	for _, r := range reqs {
		var pl programRequestPayload
		_ = json.Unmarshal([]byte(r.Payload), &pl)
		parts := m.programParts(ctx, pl.ProgramID)
		if len(parts) == 0 {
			m.allianceRequestClose(ctx, r.ID, allianceReqRefused, "no such program")
			continue
		}
		yard := r.FromCorp
		dealt := assignSuppliers(parts, yard, ids)
		shared := 0
		for _, p := range parts {
			sup, ok := dealt[p.ComponentTypeID]
			if !ok || sup == p.SupplierCorpID {
				continue
			}
			// What the yard pays per unit: the part at cost plus the executor's store
			// markup, from the component's own mineral bill (components have no catalogue price).
			unit := 0.0
			if row, rerr := m.planRowFor(ctx, ch, p.BPTypeID); rerr == nil && row != nil {
				if v, _ := costPlusRow(row.MatsPerRun, ch.WageFrac, ch.StoreMarkup, oreDeskFeeFrac); v > 0 {
					unit = v / math.Max(float64(row.Portion), 1)
				}
			}
			_, _ = m.db.ExecContext(ctx, `
				UPDATE rolebot_program_parts SET supplierCorpID = ?, unitPrice = ? WHERE programID = ? AND componentTypeID = ?`,
				sup, unit, p.ProgramID, p.ComponentTypeID)
			if sup != yard {
				shared++
			}
		}
		// The hull print is pledged among the members, paid to the yard.
		hullValue := 0.0
		var hullBP int64
		_ = m.db.QueryRowContext(ctx, `SELECT hullBPTypeID FROM rolebot_corp_program WHERE programID = ?`, pl.ProgramID).Scan(&hullBP)
		if hullBP != 0 {
			hullValue = evedb.TypeValueOf(ctx, m.db, hullBP).Value * poAdvanceMarkup
		}
		pledged := 0
		if hullValue > 0 && len(pool) > 0 {
			shares := titheShares(members, yard, hullValue)
			if err := m.titheIssue(ctx, seat.AllianceID, yard, shares, titheKindPledge, pl.ProgramID,
				"pledge toward the "+m.typeNameOr(ctx, pl.HullTypeID)+" hull print"); err == nil {
				pledged = len(shares)
			}
		}
		result := fmt.Sprintf("%d component print(s) dealt to members, %d pledge(s) issued", shared, pledged)
		m.allianceRequestClose(ctx, r.ID, allianceReqTaken, result)
		m.ceoLog(ctx, c, powerAlliance, "program", pl.HullTypeID, hullValue, "shared %s's %s program — %s",
			ownerNameOf(ctx, m.db, yard), m.typeNameOr(ctx, pl.HullTypeID), result)
		m.allianceBook(ctx, seat.AllianceID, c.CorpID, "program", pl.HullTypeID, hullValue,
			"the alliance builds %s's %s — %s", ownerNameOf(ctx, m.db, yard), m.typeNameOr(ctx, pl.HullTypeID), result)
	}
	return nil
}

// suppliedParts is the parts of OTHER corporations' open programs this one
// was dealt, with the yard's station.
type suppliedPart struct {
	programPart
	YardCorpID, YardStationID, HullTypeID int64
}

func (m *Mod) suppliedParts(ctx context.Context, corpID int64) []suppliedPart {
	rows, err := m.db.QueryContext(ctx, `
		SELECT pp.programID, pp.componentTypeID, pp.bpTypeID, pp.supplierCorpID, pp.needed, pp.delivered, pp.unitPrice, pp.status,
		       p.corporationID, p.yardStationID, p.hullTypeID
		  FROM rolebot_program_parts pp
		  JOIN rolebot_corp_program p ON p.programID = pp.programID
		 WHERE pp.supplierCorpID = ? AND p.corporationID <> ? AND p.stage NOT IN (?, ?)
		 ORDER BY pp.programID, pp.componentTypeID`, corpID, corpID, programStageCrewed, programStageClosed)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []suppliedPart
	for rows.Next() {
		var s suppliedPart
		if rows.Scan(&s.ProgramID, &s.ComponentTypeID, &s.BPTypeID, &s.SupplierCorpID, &s.Needed, &s.Delivered,
			&s.UnitPrice, &s.Status, &s.YardCorpID, &s.YardStationID, &s.HullTypeID) == nil {
			out = append(out, s)
		}
	}
	return out
}

// supplyProgramParts is the member's side: fund the print it was dealt,
// build the parts (programDemand posts them), and ship what has landed in
// Production to the yard on a program leg.
func (m *Mod) supplyProgramParts(ctx context.Context, c *corpCEO, ch Charter) {
	parts := m.suppliedParts(ctx, c.CorpID)
	if len(parts) == 0 {
		return
	}
	stationID, officeID := m.corpFactoryOffice(ctx, c.CorpID)
	if officeID == 0 {
		return
	}
	byProgram := map[int64][]suppliedPart{}
	for _, p := range parts {
		if !m.libraryCanBuild(ctx, c.CorpID, p.BPTypeID) {
			if p.BPTypeID != 0 && m.openFundFor(ctx, c.CorpID, fundKindProgram) == nil {
				value := evedb.TypeValueOf(ctx, m.db, p.BPTypeID).Value
				m.openFund(ctx, c, fundKindProgram, p.BPTypeID, math.Ceil(value*poAdvanceMarkup),
					fmt.Sprintf("the %s print it was dealt for %s's %s program", m.typeNameOr(ctx, p.ComponentTypeID),
						ownerNameOf(ctx, m.db, p.YardCorpID), m.typeNameOr(ctx, p.HullTypeID)))
			}
			continue
		}
		byProgram[p.ProgramID] = append(byProgram[p.ProgramID], p)
	}
	for programID, ps := range byProgram {
		if m.openHaulFor(ctx, programID, haulKindProgram) != nil {
			continue
		}
		var items []haulItem
		yard := ps[0]
		for _, p := range ps {
			onHand := m.corpStockOf(ctx, c.CorpID, officeID, p.ComponentTypeID, corpOrderProduct, corpDivProduct)
			if onHand <= 0 {
				continue
			}
			left := p.Needed - p.Delivered
			if left <= 0 {
				continue
			}
			items = append(items, haulItem{TypeID: p.ComponentTypeID, Qty: min(onHand, left), Flag: corpDivProduct})
		}
		if len(items) == 0 {
			continue
		}
		if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindProgram, PlanID: programID,
			FromKind: haulFromOffice, FromID: officeID, FromStationID: stationID, FromFlag: corpDivProduct,
			ToKind: haulToYard, ToID: yard.YardCorpID, ToStationID: yard.YardStationID, ToFlag: corpDivMaterials,
			Items: items, PostedBy: "program",
			Note: fmt.Sprintf("components for %s's %s", ownerNameOf(ctx, m.db, yard.YardCorpID), m.typeNameOr(ctx, yard.HullTypeID))}); err != nil {
			m.logBot(c.CharID, logError, "ceo", "program leg: %v", err)
		}
	}
}

// consortiumDeliver is a program leg's unload at the yard: the cargo changes
// hands at the yard's office and the yard pays cost-plus for what landed.
// Answers units delivered.
func (m *Mod) consortiumDeliver(ctx context.Context, b *botRun, l *haulLeg) (int64, error) {
	office := m.officeAt(ctx, l.ToID, l.ToStationID)
	if office == 0 {
		return 0, fmt.Errorf("the yard has no office at %d", l.ToStationID)
	}
	var delivered int64
	for i := range l.Carrying {
		cg := &l.Carrying[i]
		if cg.Qty <= 0 {
			continue
		}
		got, err := m.moveUnits(ctx, b.CorpID, b.ShipID, 5, cg.TypeID, cg.Qty, office, l.ToFlag, l.ToID)
		if err != nil {
			return delivered, err
		}
		cg.Qty -= got
		delivered += got
		if got > 0 {
			m.consortiumPay(ctx, l.ToID, b.CorpID, l.PlanID, cg.TypeID, got)
		}
	}
	return delivered, nil
}

// consortiumPay is the yard paying the supplier for units of a part at the
// price the executor set; a yard that cannot pay today books nothing and the
// part row's delivered count still rises, since the metal is in the yard.
func (m *Mod) consortiumPay(ctx context.Context, yard, supplier, programID, typeID, units int64) {
	var unit float64
	_ = m.db.QueryRowContext(ctx, `SELECT unitPrice FROM rolebot_program_parts WHERE programID = ? AND componentTypeID = ?`,
		programID, typeID).Scan(&unit)
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_program_parts SET delivered = delivered + ? WHERE programID = ? AND componentTypeID = ?`,
		units, programID, typeID)
	amount := math.Floor(unit * float64(units))
	if amount < 1 || yard == supplier {
		return
	}
	yc, _ := getCEO(ctx, m.db, yard)
	if yc == nil {
		return
	}
	ych := poCharterOf(ctx, m, yc)
	if !m.canPay(ctx, yard, ych, amount, 0) {
		m.ceoLog(ctx, yc, powerProduction, "program", typeID, amount,
			"took %d× %s from %s for its program and could not pay the %s yet", units,
			m.typeNameOr(ctx, typeID), ownerNameOf(ctx, m.db, supplier), iskString(amount))
		return
	}
	reason := evedb.JournalReason(fmt.Sprintf("Consortium: %d× %s", units, m.typeNameOr(ctx, typeID)))
	if _, _, err := m.corpWithdraw(ctx, yard, supplier, amount, reason); err != nil {
		m.logBot(yc.CharID, logError, "ceo", "consortium payment: %v", err)
		return
	}
	m.corpBook(ctx, yard, lineConsortium, -amount, supplier, reason)
	m.corpBook(ctx, supplier, lineConsortium, amount, yard, reason)
	m.ceoLog(ctx, yc, powerProduction, "program", typeID, amount, "paid %s %s for %d× %s delivered to its program",
		ownerNameOf(ctx, m.db, supplier), iskString(amount), units, m.typeNameOr(ctx, typeID))
}

// repayPledges returns the members' hull-print pledges once the hull is
// delivered, pro rata, as far as the yard can pay; each paid row is marked
// repaid so a think never pays one twice.
func (m *Mod) repayPledges(ctx context.Context, c *corpCEO, ch Charter, p *corpProgram) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT titheID, debtorCorpID, amount, allianceID FROM rolebot_alliance_ledger
		 WHERE kind = ? AND refID = ? AND executorCorpID = ? AND status = ?`,
		titheKindPledge, p.ProgramID, c.CorpID, titheStatusPaid)
	if err != nil {
		return
	}
	type pledge struct {
		id, debtor, alliance int64
		amount               float64
	}
	var list []pledge
	for rows.Next() {
		var pl pledge
		if rows.Scan(&pl.id, &pl.debtor, &pl.amount, &pl.alliance) == nil {
			list = append(list, pl)
		}
	}
	rows.Close()
	hull := m.typeNameOr(ctx, p.HullTypeID)
	for _, pl := range list {
		if pl.amount < 1 || !m.canPay(ctx, c.CorpID, ch, pl.amount, 0) {
			continue
		}
		reason := evedb.JournalReason("Consortium: pledge repaid — " + hull)
		if _, _, err := m.corpWithdraw(ctx, c.CorpID, pl.debtor, pl.amount, reason); err != nil {
			continue
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_alliance_ledger SET status = ? WHERE titheID = ?`, titheStatusRepaid, pl.id)
		m.corpBook(ctx, c.CorpID, lineConsortium, -pl.amount, pl.debtor, reason)
		m.corpBook(ctx, pl.debtor, lineConsortium, pl.amount, c.CorpID, reason)
		m.allianceBook(ctx, pl.alliance, c.CorpID, "repaid", pl.debtor, pl.amount, "%s repaid %s's %s pledge — the %s is built",
			ownerNameOf(ctx, m.db, c.CorpID), ownerNameOf(ctx, m.db, pl.debtor), iskString(pl.amount), hull)
	}
}
