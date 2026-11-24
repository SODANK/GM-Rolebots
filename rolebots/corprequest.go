package rolebots

import (
	"context"
	"fmt"
	"math"
	"sync"

	evedb "gurimoon/db"
)

// Departments as customers. Every piece of corporation work is somebody's request: a
// request names who asked (requesterArm) and where the result goes, a hangar division the
// asking arm owns (corpdeptdivs.go), at a station. A work order (corpwork.go) is something
// to make: destFlag and destStationID say where the output is delivered; a bot installs
// into that division (installCorpJob), a human picks it in the S&I window's Output dropdown
// (armIndustryRoles). A requisition (posreq.go) is something to have: when the library
// holds the print and building costs less than buying, it becomes a work order
// (routeReqToBuild) and sits 'building'. Output delivered elsewhere is still paid and then
// filed where it was asked for (fileWorkOutput).

// reqStatusBuilding is a requisition being made by its work order rather than
// bought.
const reqStatusBuilding = "building"

// reqRouteCheckSec is how often one open line is re-asked "build or buy".
// The answer reads a bill of materials and a price set; a line that came out
// "buy" stays bought unless something changes, which is not by the minute.
const reqRouteCheckSec = 3600

// reqBuildMargin is how much cheaper than buying a build has to come out
// before a line is built: the bill of materials is priced at today's market
// and the run takes time, so a near-tie is a buy.
const reqBuildMargin = 0.9

// workPriorityRequest is the board priority of a requested build: ahead of
// market work, the same as a replacement owed (SRP).
const workPriorityRequest = 5

var reqRouteChecked sync.Map // reqID to unix time last routed

// workRequesterArm is the arm a work order is for: its own column, or read
// off the purpose of an order older than the column. Pure.
func workRequesterArm(w corpWork) corpArm {
	if w.RequesterArm != "" {
		return corpArm(w.RequesterArm)
	}
	switch w.Purpose {
	case "srp", "armory":
		return armSecurity
	case "market", "store", "for sale", "spoils":
		return armSales
	case "yard":
		return armMining
	case corpDemandPurpose:
		return armIndustry
	}
	switch w.Kind {
	case workKindCopy, workKindME, workKindPE:
		// Prints made better or multiplied for the builders who run them.
		return armIndustry
	}
	return armIndustry
}

// workFulfillerArm is the arm whose members carry the work out. Pure.
func workFulfillerArm(w corpWork) corpArm {
	switch w.Kind {
	case workKindCopy, workKindME, workKindPE:
		return armResearch
	}
	return armIndustry
}

// workDestFlag is the hangar division a work order's output goes into: its own when it
// names one, else Production, where the library reconcile picks up copies for the shelf. Pure.
func workDestFlag(w corpWork) int64 {
	if w.DestFlag != 0 && flagDiv(w.DestFlag) != 0 {
		return w.DestFlag
	}
	return corpDivProduct
}

// workDestStation is the station a work order's output belongs at: its own
// when it names one, else where it is built (0 = wherever that was). Pure.
func workDestStation(w corpWork) int64 {
	if w.DestStationID != 0 {
		return w.DestStationID
	}
	return w.StationID
}

// workInstallFlag is the output division a bot installs a job into at
// stationID: the order's destination when the order is delivered here, else
// Production (the output is hauled on from there). Pure.
func workInstallFlag(w corpWork, stationID int64) int64 {
	if d := workDestStation(w); d == 0 || d == stationID {
		return workDestFlag(w)
	}
	return corpDivProduct
}

// reqRequesterArm is the arm a requisition is for: its own column, or read
// off the requester string of a line older than the column. Pure.
func reqRequesterArm(r requisition) corpArm {
	if r.RequesterArm != "" {
		return corpArm(r.RequesterArm)
	}
	switch r.Requester {
	case "security":
		return armSecurity
	case "industry":
		return armIndustry
	case reqTrade:
		return armSales
	case "fueler", "ceo":
		return armInfrastructure
	}
	return armIndustry
}

// reqBuildable is whether a requisition may be considered for building at
// all: not the Trade desk's merchandise (it is bought to be resold), not the
// work board's own raw material, and not a line already moving. Pure.
func reqBuildable(r requisition) bool {
	if r.Requester == reqTrade || r.RefKind == reqWorkboard || r.Requester == reqChairBuy {
		return false
	}
	return r.Status == "open" && r.OrderID == 0 && r.BuyerID == 0 && r.left() > 0
}

// reqBuildWins is the build-or-buy call: build when one unit costs at most
// reqBuildMargin of what the market charges. Pure.
func reqBuildWins(unitBuild, unitMarket float64) bool {
	return unitBuild > 0 && unitMarket > 0 && unitBuild <= unitMarket*reqBuildMargin
}

// routeReqToBuild turns an open requisition into a work order when the
// corporation can make the goods for less than it would pay for them.
// Reports whether it did; a line it leaves goes on to be bought as before.
func (m *Mod) routeReqToBuild(ctx context.Context, c *corpCEO, ch Charter, r *requisition) bool {
	if !reqBuildable(*r) {
		return false
	}
	now := clock.Unix()
	if v, ok := reqRouteChecked.Load(r.ReqID); ok && now-v.(int64) < reqRouteCheckSec {
		return false
	}
	reqRouteChecked.Store(r.ReqID, now)

	var bpTypeID int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT blueprintTypeID FROM invBlueprintTypes WHERE productTypeID = ? LIMIT 1`, r.TypeID).Scan(&bpTypeID); err != nil ||
		bpTypeID == 0 {
		return false
	}
	if !m.libraryHas(ctx, c.CorpID, bpTypeID) {
		return false
	}
	factory, _ := m.corpFactoryOffice(ctx, c.CorpID)
	if factory == 0 {
		return false
	}
	row, err := m.planRowAt(ctx, ch, bpTypeID, m.stationRegionOf(ctx, factory))
	if err != nil || row == nil || row.Portion < 1 {
		return false
	}
	unitBuild := (row.MatsPerRun + row.WagePerRun) / float64(row.Portion)
	unitMarket := evedb.TypeValueOf(ctx, m.db, r.TypeID).Value
	// A big hull nobody has traded has no market to lose to: its market value is far under
	// the bill at current minerals, so it is built at cost-plus when the scope allows (corpcostplus.go).
	bigNoTape := false
	if costPlusScope(ctx, m.db) != costPlusScopeOff {
		if k := m.typeKinds(ctx, []int64{r.TypeID})[r.TypeID]; costPlusHullGroups[k.Group] && !m.hullHasTape(ctx, r.TypeID) {
			bigNoTape = true
		}
	}
	if !bigNoTape && !reqBuildWins(unitBuild, unitMarket) {
		return false
	}
	runs := int(math.Ceil(float64(r.left()) / float64(row.Portion)))
	if row.MaxRuns > 0 && int64(runs) > row.MaxRuns {
		runs = int(row.MaxRuns)
	}
	if runs < 1 {
		return false
	}
	arm := reqRequesterArm(*r)
	workID, err := m.postWork(ctx, corpWork{
		CorpID: c.CorpID, Kind: workKindBuild, BPTypeID: bpTypeID, ProductTypeID: r.TypeID,
		StationID: factory, RunsWanted: runs, WagePerRun: row.WagePerRun,
		Purpose: "request", Priority: workPriorityRequest,
		Note: fmt.Sprintf("for %s: %d wanted in %s", armLabel(arm), r.left(),
			m.divLabelAt(ctx, c.CorpID, r.DestFlag)),
		RequesterArm: string(arm), DestFlag: r.DestFlag, DestStationID: r.DestStationID, ReqID: r.ReqID,
	})
	if err != nil || workID == 0 {
		return false
	}
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_requisition SET status = ?, workID = ?, note = ?
		 WHERE reqID = ? AND status = 'open'`,
		reqStatusBuilding, workID, truncate(fmt.Sprintf("built by Industry (work order %d)", workID), 255), r.ReqID)
	if n, _ := res.RowsAffected(); err != nil || n == 0 {
		m.closeWork(ctx, workID, workVoid)
		return false
	}
	r.Status, r.WorkID = reqStatusBuilding, workID
	m.ceoLog(ctx, c, powerProduction, "reqbuild", r.TypeID, unitBuild*float64(r.left()),
		"%s asked for %d× %s — building %d run(s) at %s a unit instead of buying at %s",
		armLabel(arm), r.left(), m.typeNameOr(ctx, r.TypeID), runs, iskString(unitBuild), iskString(unitMarket))
	return true
}

// sweepBuildingReqs settles requisitions being built: done once the goods
// are filed, back to buying when the work order died or the deadline is close
// and nothing has been built yet.
func (m *Mod) sweepBuildingReqs(ctx context.Context, c *corpCEO) {
	reqs, err := m.queryReqs(ctx, `WHERE corporationID = ? AND status = ?`, c.CorpID, reqStatusBuilding)
	if err != nil {
		return
	}
	now := clock.Unix()
	for i := range reqs {
		r := &reqs[i]
		if r.left() <= 0 {
			m.closeReq(ctx, r, "done", "built and delivered")
			continue
		}
		ws, _ := m.queryWork(ctx, `WHERE workID = ?`, r.WorkID)
		var w *corpWork
		if len(ws) == 1 {
			w = &ws[0]
		}
		reopen := ""
		switch {
		case w == nil || w.Status == workVoid:
			reopen = "its work order was withdrawn"
		case w.Status == workDone:
			reopen = "the build came short"
		case r.Deadline-now < reqPatientSec/2 && w.RunsDone == 0 &&
			m.workClaimedExceptOpen(ctx, w.WorkID, 0) == 0:
			reopen = "nobody built it in time"
			m.closeWork(ctx, w.WorkID, workVoid)
		}
		if reopen == "" {
			continue
		}
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_requisition SET status = 'open', workID = 0, note = ?
			 WHERE reqID = ? AND status = ?`, truncate("buying the rest — "+reopen, 255), r.ReqID, reqStatusBuilding)
		reqRouteChecked.Store(r.ReqID, now) // a fresh look in an hour, not this pass
	}
}

// fileWorkOutput moves a delivered job's output to the hangar its order asked for: a flag
// move inside the office, or a haul leg to another station. Only an order that names a
// destination is filed. The requisition behind it is credited with what was filed.
func (m *Mod) fileWorkOutput(ctx context.Context, c *corpCEO, j deliveredCorpJob, w *corpWork, runs int) {
	if w == nil || (w.DestFlag == 0 && w.DestStationID == 0) || runs <= 0 {
		return
	}
	var typeID, units int64
	switch w.Kind {
	case workKindBuild:
		typeID = w.ProductTypeID
		portion, _ := evedb.GetTypePortionSize(ctx, m.db, typeID)
		units = int64(runs) * max(portion, 1)
	case workKindCopy:
		typeID, units = w.BPTypeID, int64(runs)
	default:
		return // research hands its print back to its own division
	}
	var outFlag, office int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(outputFlag, 0), COALESCE(outputLocationID, 0) FROM ramJobs WHERE jobID = ?`,
		j.JobID).Scan(&outFlag, &office)
	if outFlag == 0 {
		outFlag = corpDivMaterials
	}
	if office == 0 || office == j.StationID {
		return // not delivered into an office: nothing of the corporation's to file
	}
	destFlag, destStation := workDestFlag(*w), workDestStation(*w)
	if destStation == 0 {
		destStation = j.StationID
	}
	moved := units
	switch {
	case destStation == j.StationID && outFlag == destFlag:
		// Delivered where it was asked for.
	case destStation == j.StationID:
		moved = m.moveOfficeUnits(ctx, j.CorpID, office, typeID, outFlag, destFlag, units, w.Kind == workKindCopy)
		if moved > 0 && c != nil {
			m.ceoLog(ctx, c, powerProduction, "file", typeID, 0,
				"filed %d× %s from %s into %s, where %s asked for it", moved, m.typeNameOr(ctx, typeID),
				m.divLabelAt(ctx, j.CorpID, outFlag), m.divLabelAt(ctx, j.CorpID, destFlag),
				armLabel(workRequesterArm(*w)))
		}
	default:
		if c == nil {
			return
		}
		item := haulItem{TypeID: typeID, Qty: units, Flag: outFlag}
		if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindMove, FromKind: haulFromOffice, FromID: office,
			FromStationID: j.StationID, FromFlag: outFlag, ToKind: haulToOffice, ToStationID: destStation,
			ToFlag: destFlag, Items: []haulItem{item}, PostedBy: string(workRequesterArm(*w)),
			Note: fmt.Sprintf("%s for %s", m.typeNameOr(ctx, typeID), armLabel(workRequesterArm(*w)))}); err != nil {
			rlogf("rolebots: file work output %d: %v", j.JobID, err)
			return
		}
	}
	if w.ReqID != 0 && moved > 0 {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_requisition SET qtyDone = LEAST(qty, qtyDone + ?) WHERE reqID = ?`, moved, w.ReqID)
	}
}

// moveOfficeUnits moves up to units of a type from one division of an office
// to another, splitting a stack where it has to. Copies are singletons and
// move whole, newest first (the ones the job just made). Answers how many
// moved.
func (m *Mod) moveOfficeUnits(ctx context.Context, corpID, office, typeID, fromFlag, toFlag, units int64, copies bool) int64 {
	if units <= 0 || fromFlag == toFlag {
		return 0
	}
	if copies {
		return m.moveCopiesByID(ctx, corpID, office, typeID, fromFlag, toFlag, units)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		 WHERE ownerID = ? AND locationID = ? AND flag = ? AND typeID = ? AND singleton = 0
		 ORDER BY quantity DESC, itemID`, corpID, office, fromFlag, typeID)
	if err != nil {
		return 0
	}
	type stack struct{ id, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if rows.Scan(&s.id, &s.qty) == nil {
			stacks = append(stacks, s)
		}
	}
	rows.Close()
	var moved int64
	for _, s := range stacks {
		left := units - moved
		if left <= 0 {
			break
		}
		id := s.id
		take := s.qty
		if s.qty > left {
			nid, err := m.splitCorpStack(ctx, s.id, left)
			if err != nil || nid == 0 {
				continue
			}
			id, take = nid, left
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET flag = ? WHERE itemID = ? AND ownerID = ?`, toFlag, id, corpID); err != nil {
			continue
		}
		moved += take
	}
	return moved
}

func (m *Mod) moveCopiesByID(ctx context.Context, corpID, office, typeID, fromFlag, toFlag, n int64) int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID FROM entity e
		  JOIN invBlueprints b ON b.blueprintID = e.itemID AND b.copy = 1
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ? AND e.typeID = ?
		 ORDER BY e.itemID DESC LIMIT ?`, corpID, office, fromFlag, typeID, n)
	if err != nil {
		return 0
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	var moved int64
	for _, id := range ids {
		if res, err := m.db.ExecContext(ctx,
			`UPDATE entity SET flag = ? WHERE itemID = ? AND ownerID = ?`, toFlag, id, corpID); err == nil {
			if k, _ := res.RowsAffected(); k > 0 {
				moved++
			}
		}
	}
	return moved
}

// divLabelAt is how a corporation's division reads in a log line or on a
// page: its stored name, else its owner's tag and number.
func (m *Mod) divLabelAt(ctx context.Context, corpID, flag int64) string {
	div := flagDiv(flag)
	if div == 0 {
		return "the hangar"
	}
	names, _ := m.divisionNames(ctx, corpID)
	return divDisplayName(m.corpDivs(ctx, corpID), div, names[div])
}
