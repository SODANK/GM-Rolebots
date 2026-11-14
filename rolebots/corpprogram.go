package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// Capital programs: a corporation building one big hull from components.
//
// A capital print lists Capital Construction Components (group 334), each with its own
// print and mineral bill, and nothing in the build book or work board reaches them. A
// program names the hull, the parts and the yard and walks them through stages: saving
// the hull print (a print fund of kind "program"), printing the components, components as
// work-board rows (programDemand), building the hull, delivered, crewed (handOverHull).
// The corporation pays; only a shipyard opens one, behind rolebots.program.freighter.enabled
// or rolebots.program.capital.enabled, at a factory whose lines may build the hull. One open
// program per corporation. The alliance consortium splits the kit across corporations.

const (
	programKindFreighter = "freighter"
	programKindCapital   = "capital"

	programStageSaving     = "saving"
	programStagePrint      = "print"
	programStageComponents = "components"
	programStageBuilding   = "building"
	programStageDelivered  = "delivered"
	programStageCrewed     = "crewed"
	programStageClosed     = "closed"

	programFreighterKey = "rolebots.program.freighter.enabled"
	programCapitalKey   = "rolebots.program.capital.enabled"

	programPurpose  = "program"
	programPriority = 45 // under the store (50), over the roster's demand ledger (40)

	fundKindProgram = "program"

	skillCapitalShipConstruction = 22242
	skillAdvancedSpaceshipCmd    = 20342
	skillCapitalShips            = 20533

	groupFreighter   = 513
	groupDreadnought = 485
	groupCarrier     = 547
	attrRequiredSkill1 = 182
	attrRequiredSkill2 = 183

	programLogEverySec = 6 * 3600
)

// corpProgram is one program row.
type corpProgram struct {
	ProgramID, CorpID, AllianceID int64
	Kind                          string
	HullTypeID, HullBPTypeID      int64
	YardStationID                 int64
	Stage                         string
	BuilderID, PilotID            int64
	HullItemID                    int64
	OpenedAt, UpdatedAt           int64
	Note                          string
}

// programPart is one component line of a program.
type programPart struct {
	ProgramID, ComponentTypeID, BPTypeID, SupplierCorpID int64
	Needed, Delivered                                   int64
	UnitPrice                                           float64
	Status                                              string
}

func programFreighterOn(ctx context.Context, db *sql.DB) bool {
	return escrowSettingInt(ctx, db, programFreighterKey, 0) != 0
}

func programCapitalOn(ctx context.Context, db *sql.DB) bool {
	return escrowSettingInt(ctx, db, programCapitalKey, 0) != 0
}

// programHullGroups is the hull groups a program may be opened for under the
// switches that are on. Pure.
func programHullGroups(freighter, capital bool) []int64 {
	var out []int64
	if freighter {
		out = append(out, groupFreighter)
	}
	if capital {
		out = append(out, groupDreadnought, groupCarrier)
	}
	return out
}

// programStageFrom is the stage machine's pure half: given what the library
// holds and what the yard has on hand, the stage a program should be in.
// Stages only move forward here; the driver writes the row. Pure.
func programStageFrom(cur string, hullPrint bool, partPrints, partsDone, hullOnHand bool) string {
	switch cur {
	case programStageSaving:
		if hullPrint {
			return programStagePrint
		}
	case programStagePrint:
		if partPrints {
			return programStageComponents
		}
	case programStageComponents:
		if partsDone {
			return programStageBuilding
		}
	case programStageBuilding:
		if hullOnHand {
			return programStageDelivered
		}
	}
	return cur
}

// partsShort is the component rows still to build: needed less what is on
// hand, per type, in units. Pure.
func partsShort(parts []programPart, onHand map[int64]int64) map[int64]int64 {
	out := map[int64]int64{}
	for _, p := range parts {
		if s := p.Needed - onHand[p.ComponentTypeID]; s > 0 {
			out[p.ComponentTypeID] = s
		}
	}
	return out
}

func (m *Mod) openProgramFor(ctx context.Context, corpID int64) *corpProgram {
	p := &corpProgram{}
	err := m.db.QueryRowContext(ctx, `
		SELECT programID, corporationID, allianceID, kind, hullTypeID, hullBPTypeID, yardStationID, stage,
		       builderID, pilotID, hullItemID, openedAt, updatedAt, note
		  FROM rolebot_corp_program
		 WHERE corporationID = ? AND stage NOT IN (?, ?)
		 ORDER BY openedAt LIMIT 1`, corpID, programStageCrewed, programStageClosed).
		Scan(&p.ProgramID, &p.CorpID, &p.AllianceID, &p.Kind, &p.HullTypeID, &p.HullBPTypeID, &p.YardStationID,
			&p.Stage, &p.BuilderID, &p.PilotID, &p.HullItemID, &p.OpenedAt, &p.UpdatedAt, &p.Note)
	if err != nil {
		return nil
	}
	return p
}

func (m *Mod) programParts(ctx context.Context, programID int64) []programPart {
	rows, err := m.db.QueryContext(ctx, `
		SELECT programID, componentTypeID, bpTypeID, supplierCorpID, needed, delivered, unitPrice, status
		  FROM rolebot_program_parts WHERE programID = ? ORDER BY componentTypeID`, programID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []programPart
	for rows.Next() {
		var p programPart
		if rows.Scan(&p.ProgramID, &p.ComponentTypeID, &p.BPTypeID, &p.SupplierCorpID, &p.Needed, &p.Delivered,
			&p.UnitPrice, &p.Status) == nil {
			out = append(out, p)
		}
	}
	return out
}

// programYardStation is the open program's yard, 0 with none open.
func (m *Mod) programYardStation(ctx context.Context, corpID int64) int64 {
	var st int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT yardStationID FROM rolebot_corp_program
		 WHERE corporationID = ? AND stage NOT IN (?, ?) ORDER BY openedAt LIMIT 1`,
		corpID, programStageCrewed, programStageClosed).Scan(&st)
	return st
}

// programOffice is the yard's station and the corporation's office there: where parts
// land, are moved to Materials, and the hull is built.
func (m *Mod) programOffice(ctx context.Context, p *corpProgram) (stationID, officeID int64) {
	if p == nil || p.YardStationID == 0 {
		return 0, 0
	}
	return p.YardStationID, m.officeAt(ctx, p.CorpID, p.YardStationID)
}

// capitalProgramWanted is the demand gate for a dreadnought or carrier: a
// corporation at war, besieging or defending a tower, or whose alliance has
// named sovereignty targets. A freighter needs no reason; a dreadnought with
// nothing to shoot is a hangar ornament.
func (m *Mod) capitalProgramWanted(ctx context.Context, c *corpCEO) (bool, string) {
	if relCorpAtWar(c.CorpID) {
		return true, "the corporation is at war"
	}
	if sys, why := m.siegeOrderFor(ctx, c.CorpID); sys != 0 {
		return true, why
	}
	if seat, _ := m.allianceOf(ctx, c.CorpID); seat != nil {
		if targets := m.allianceTargetSystems(ctx, seat.AllianceID); len(targets) > 0 {
			return true, fmt.Sprintf("the alliance has %d sovereignty target(s)", len(targets))
		}
	}
	return false, ""
}

func (m *Mod) setProgramStage(ctx context.Context, c *corpCEO, p *corpProgram, stage, note string) {
	if stage == p.Stage {
		return
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_program SET stage = ?, updatedAt = ?, note = ? WHERE programID = ?`,
		stage, clock.Unix(), note, p.ProgramID)
	p.Stage = stage
	m.ceoLog(ctx, c, powerProduction, "program", p.HullTypeID, 0, "%s program: %s — %s",
		m.typeNameOr(ctx, p.HullTypeID), stage, note)
}

// hullRequiredSkills reads a type's first two required skills (dogma 182/183): for a
// hull, the racial capital skill and the racial industrial behind it.
func (m *Mod) hullRequiredSkills(ctx context.Context, typeID int64) (first, second int64) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(IF(attributeID = ?, COALESCE(valueInt, valueFloat), NULL)), 0),
		       COALESCE(MAX(IF(attributeID = ?, COALESCE(valueInt, valueFloat), NULL)), 0)
		  FROM dgmTypeAttributes WHERE typeID = ? AND attributeID IN (?, ?)`,
		attrRequiredSkill1, attrRequiredSkill2, typeID, attrRequiredSkill1, attrRequiredSkill2).Scan(&first, &second)
	return
}

// programHullFor picks the hull a new program builds: a published hull of an
// allowed group whose race is the CEO's, else the cheapest print among them;
// nothing with a zero catalogue price (test hulls).
func (m *Mod) programHullFor(ctx context.Context, c *corpCEO, groups []int64) (hullTypeID, bpTypeID int64) {
	if len(groups) == 0 {
		return 0, 0
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, bt.blueprintTypeID, t.raceID, bp.basePrice
		  FROM invTypes t
		  JOIN invBlueprintTypes bt ON bt.productTypeID = t.typeID
		  JOIN invTypes bp ON bp.typeID = bt.blueprintTypeID
		 WHERE t.groupID IN (`+placeholders(len(groups))+`) AND t.published = 1 AND t.basePrice > 0 AND bp.basePrice > 0
		 ORDER BY bp.basePrice`, int64Args(groups)...)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	race := m.charRace(ctx, c.CharID)
	var cheapest, cheapestBP int64
	for rows.Next() {
		var t, bp, r int64
		var price float64
		if rows.Scan(&t, &bp, &r, &price) != nil {
			continue
		}
		if cheapest == 0 {
			cheapest, cheapestBP = t, bp
		}
		if race != 0 && r == race {
			return t, bp
		}
	}
	return cheapest, cheapestBP
}

// programBuilderFor picks the member who will install the hull and its
// parts: one already holding the construction skill, else one with the
// skills its book needs (Mechanic, Industry and Production Efficiency V),
// else the Director of Industry.
func (m *Mod) programBuilderFor(ctx context.Context, corpID, directorID int64) int64 {
	if id := m.memberWithSkills(ctx, corpID, map[int64]int{skillCapitalShipConstruction: 1}); id != 0 {
		return id
	}
	if id := m.memberWithSkills(ctx, corpID, map[int64]int{3392: 5, 3380: 5, 3388: 5}); id != 0 {
		return id
	}
	return directorID
}

// memberWithSkills is a bot member of the corporation trained to every level
// asked (skill typeID to level), the first by character id; 0 when none.
func (m *Mod) memberWithSkills(ctx context.Context, corpID int64, reqs map[int64]int) int64 {
	if len(reqs) == 0 {
		return 0
	}
	ids := make([]int64, 0, len(reqs))
	for id := range reqs {
		ids = append(ids, id)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.ownerID, e.typeID, MAX(a.valueInt)
		  FROM character_ ch
		  JOIN rolebot_characters rc ON rc.characterID = ch.characterID AND rc.enabled = 1
		  JOIN entity e ON e.ownerID = ch.characterID AND e.flag IN (7, 61)
		  JOIN entity_attributes a ON a.itemID = e.itemID AND a.attributeID = 280
		 WHERE ch.corporationID = ? AND ch.accountID > 0 AND e.typeID IN (`+placeholders(len(ids))+`)
		 GROUP BY e.ownerID, e.typeID`, append([]any{corpID}, int64Args(ids)...)...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	have := map[int64]map[int64]int{}
	for rows.Next() {
		var char, skill int64
		var lv int
		if rows.Scan(&char, &skill, &lv) == nil {
			if have[char] == nil {
				have[char] = map[int64]int{}
			}
			have[char][skill] = lv
		}
	}
	best := int64(0)
	for char, lv := range have {
		ok := true
		for skill, need := range reqs {
			if lv[skill] < need {
				ok = false
				break
			}
		}
		if ok && (best == 0 || char < best) {
			best = char
		}
	}
	return best
}

// ceoProgram is the production think's program step: opens one when the
// switches allow and none is open, and walks the open one a stage.
func (m *Mod) ceoProgram(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, stationID int64) {
	// Parts this corporation was dealt for an ally's program (corpconsortium.go).
	if consortiumOn(ctx, m.db) {
		m.supplyProgramParts(ctx, c, ch)
	}
	groups := programHullGroups(programFreighterOn(ctx, m.db), programCapitalOn(ctx, m.db))
	p := m.openProgramFor(ctx, c.CorpID)
	if p == nil {
		if len(groups) == 0 || facts == nil || !containsStr(facts.Policy.Tags, "shipbuilding") || stationID == 0 {
			return
		}
		m.openProgram(ctx, c, ch, facts, groups, stationID)
		return
	}
	stationID, officeID := m.programOffice(ctx, p)
	if officeID == 0 {
		return
	}
	parts := m.programParts(ctx, p.ProgramID)
	switch p.Stage {
	case programStageSaving:
		if m.libraryCanBuild(ctx, c.CorpID, p.HullBPTypeID) {
			m.setProgramStage(ctx, c, p, programStagePrint, "the hull print is in the library")
			return
		}
		if m.openFundFor(ctx, c.CorpID, fundKindProgram) == nil {
			value := evedb.TypeValueOf(ctx, m.db, p.HullBPTypeID).Value
			m.openFund(ctx, c, fundKindProgram, p.HullBPTypeID, math.Ceil(value*poAdvanceMarkup),
				"the hull print of its "+m.typeNameOr(ctx, p.HullTypeID)+" program")
		}
	case programStagePrint:
		// With the alliance consortium on, component prints are the alliance's to share: ask
		// once and wait for the executor's deal before funding any alone.
		if consortiumOn(ctx, m.db) {
			if seat, _ := m.allianceOf(ctx, c.CorpID); seat != nil {
				m.fileProgramRequest(ctx, c, seat, p)
				if m.programRequestOpen(ctx, p.ProgramID) {
					return
				}
			}
		}
		var missing []programPart
		for _, pt := range parts {
			if pt.SupplierCorpID != 0 && pt.SupplierCorpID != c.CorpID {
				// A consortium member's part: its library, its fund. Held
				// here until the member holds the print.
				if !m.libraryCanBuild(ctx, pt.SupplierCorpID, pt.BPTypeID) {
					missing = append(missing, pt)
				}
				continue
			}
			if !m.libraryCanBuild(ctx, c.CorpID, pt.BPTypeID) {
				missing = append(missing, pt)
			}
		}
		if len(missing) == 0 {
			m.setProgramStage(ctx, c, p, programStageComponents, "every component print is in a library")
			return
		}
		own := missing[:0]
		for _, pt := range missing {
			if pt.SupplierCorpID == 0 || pt.SupplierCorpID == c.CorpID {
				own = append(own, pt)
			}
		}
		missing = own
		if len(missing) > 0 && m.openFundFor(ctx, c.CorpID, fundKindProgram) == nil {
			// Cheapest print first: the sooner one part line is building, the
			// sooner the yard's Materials division starts filling.
			best, bestValue := missing[0], math.Inf(1)
			for _, pt := range missing {
				if v := evedb.TypeValueOf(ctx, m.db, pt.BPTypeID).Value; v > 0 && v < bestValue {
					best, bestValue = pt, v
				}
			}
			if bestValue < math.Inf(1) {
				m.openFund(ctx, c, fundKindProgram, best.BPTypeID, math.Ceil(bestValue*poAdvanceMarkup),
					fmt.Sprintf("the %s print for its %s program", m.typeNameOr(ctx, best.ComponentTypeID),
						m.typeNameOr(ctx, p.HullTypeID)))
			}
		}
	case programStageComponents, programStageBuilding:
		// Parts that landed in Production go to Materials, where the hull job
		// consumes them; the lister never sells them (ceoOutput).
		onHand := map[int64]int64{}
		done := true
		for _, pt := range parts {
			if inProd := m.corpStockOf(ctx, c.CorpID, officeID, pt.ComponentTypeID, corpOrderProduct, corpDivProduct); inProd > 0 {
				m.moveCorpStock(ctx, c.CorpID, officeID, pt.ComponentTypeID, corpDivProduct, corpDivMaterials, inProd)
			}
			have := m.corpStockOf(ctx, c.CorpID, officeID, pt.ComponentTypeID, corpOrderProduct, corpDivMaterials)
			onHand[pt.ComponentTypeID] = have
			if have != pt.Delivered {
				_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_program_parts SET delivered = ? WHERE programID = ? AND componentTypeID = ?`,
					have, p.ProgramID, pt.ComponentTypeID)
			}
			if have < pt.Needed {
				done = false
			}
		}
		hullOnHand := m.corpStockOf(ctx, c.CorpID, officeID, p.HullTypeID, corpOrderProduct, corpDivProduct, corpDivStore, corpDivMaterials) > 0
		next := programStageFrom(p.Stage, true, true, done, hullOnHand)
		if next == programStageBuilding && p.Stage == programStageComponents {
			m.setProgramStage(ctx, c, p, next, "every component is in the yard's Materials division")
		} else if next == programStageDelivered {
			m.setProgramStage(ctx, c, p, next, "the hull is in the yard's Production division")
		} else if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "programwait") >= programLogEverySec {
			short := partsShort(parts, onHand)
			m.ceoLog(ctx, c, powerProduction, "programwait", p.HullTypeID, float64(len(short)),
				"%s program: %d component line(s) still short at the yard", m.typeNameOr(ctx, p.HullTypeID), len(short))
		}
	case programStageDelivered:
		if consortiumOn(ctx, m.db) {
			m.repayPledges(ctx, c, ch, p)
		}
		m.programHandOver(ctx, c, p, stationID, officeID)
	}
}

// programYardFor picks where a hull is built: the factory if its lines may
// build it, else an Industry base that may, else a yard office rented for it
// (huntYardOffice). 0 when none can be had this think.
func (m *Mod) programYardFor(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, factory, hull int64) int64 {
	canBuild := func(st int64) bool {
		line, err := evedb.FindAssemblyLineForProduct(ctx, m.db, st, 1, hull)
		return err == nil && line != 0
	}
	if canBuild(factory) {
		return factory
	}
	for _, st := range m.industryBaseStations(ctx, c.CorpID) {
		if st != factory && canBuild(st) {
			return st
		}
	}
	if yard, err := m.huntYardOffice(ctx, c, facts, ch, hull); err == nil && yard != 0 {
		return yard
	}
	return 0
}

// openProgram writes a new program and its parts for the hull the CEO picks,
// at a yard whose lines may build it.
func (m *Mod) openProgram(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, groups []int64, stationID int64) {
	if m.openProgramFor(ctx, c.CorpID) != nil {
		return // one program per corporation, whoever asks
	}
	hull, bp := m.programHullFor(ctx, c, groups)
	if hull == 0 || bp == 0 {
		return
	}
	kind := programKindFreighter
	if k := m.typeKinds(ctx, []int64{hull})[hull]; k.Group != groupFreighter {
		kind = programKindCapital
	}
	// A dreadnought or carrier needs a reason; a freighter hauls regardless.
	reason := "it hauls"
	if kind == programKindCapital {
		wanted, why := m.capitalProgramWanted(ctx, c)
		if !wanted {
			if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "programwant") >= programLogEverySec {
				m.ceoLog(ctx, c, powerProduction, "programwant", hull, 0,
					"no war, siege or sovereignty target wants a %s — no program opened", m.typeNameOr(ctx, hull))
			}
			return
		}
		reason = why
	}
	yard := m.programYardFor(ctx, c, facts, ch, stationID, hull)
	if yard == 0 {
		if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "programyard") >= programLogEverySec {
			m.ceoLog(ctx, c, powerProduction, "programyard", hull, 0,
				"no line at its factory or bases may build a %s, and no yard could be rented — no program opened", m.typeNameOr(ctx, hull))
		}
		return
	}
	mats, err := evedb.GetRequiredMaterials(ctx, m.db, bp, 1)
	if err != nil || len(mats) == 0 {
		return
	}
	p, _ := m.loadCorpPolicy(ctx, c.CorpID)
	builder := m.programBuilderFor(ctx, c.CorpID, p.DirectorID)
	pilot := m.programPilotFor(ctx, c.CorpID, p.DirectorID, hull)
	now := clock.Unix()
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_program (corporationID, kind, hullTypeID, hullBPTypeID, yardStationID, stage, builderID, pilotID, openedAt, updatedAt, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.CorpID, kind, hull, bp, yard, programStageSaving, builder, pilot, now, now, truncate(reason, 255))
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "opening a program: %v", err)
		return
	}
	programID, _ := res.LastInsertId()
	for _, mt := range mats {
		var partBP int64
		_ = m.db.QueryRowContext(ctx, `SELECT blueprintTypeID FROM invBlueprintTypes WHERE productTypeID = ? LIMIT 1`, mt.TypeID).Scan(&partBP)
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_program_parts (programID, componentTypeID, bpTypeID, supplierCorpID, needed)
			VALUES (?, ?, ?, ?, ?)`, programID, mt.TypeID, partBP, c.CorpID, int64(math.Ceil(float64(mt.Quantity)*mt.DamagePerJob)))
	}
	m.ceoLog(ctx, c, powerProduction, "program", hull, 0,
		"opened a %s program at %s — %d component line(s); the builder is %s, the pilot %s; %s",
		m.typeNameOr(ctx, hull), m.stationLabel(ctx, yard), len(mats), m.charNameOr(ctx, builder),
		m.charNameOr(ctx, pilot), reason)
}

// programDemand is the open program's component and hull rows for the work
// board: Purpose "program", priority between the store and the demand ledger,
// sized to what the yard still lacks. Nothing while the prints are not held.
func (m *Mod) programDemand(ctx context.Context, corpID int64) []buildPlanRow {
	_, officeID := m.corpFactoryOffice(ctx, corpID)
	if officeID == 0 {
		return nil
	}
	var out []buildPlanRow
	// Parts this corporation supplies to an ally's program (corpconsortium.go):
	// built here, hauled there, counted at the yard.
	for _, sp := range m.suppliedParts(ctx, corpID) {
		if sp.BPTypeID == 0 || !m.libraryCanBuild(ctx, corpID, sp.BPTypeID) {
			continue
		}
		onHand := m.corpStockOf(ctx, corpID, officeID, sp.ComponentTypeID, corpOrderProduct, corpDivMaterials, corpDivProduct)
		if short := sp.Needed - sp.Delivered - onHand; short > 0 {
			out = append(out, buildPlanRow{BPTypeID: sp.BPTypeID, ProductTypeID: sp.ComponentTypeID,
				Wanted: int(short), Priority: programPriority, Purpose: programPurpose})
		}
	}
	p := m.openProgramFor(ctx, corpID)
	if p == nil || (p.Stage != programStageComponents && p.Stage != programStageBuilding) {
		return out
	}
	// The yard's office, which may not be the factory's (programYardFor).
	if _, yardOffice := m.programOffice(ctx, p); yardOffice != 0 {
		officeID = yardOffice
	}
	parts := m.programParts(ctx, p.ProgramID)
	onHand := map[int64]int64{}
	for _, pt := range parts {
		onHand[pt.ComponentTypeID] = m.corpStockOf(ctx, corpID, officeID, pt.ComponentTypeID, corpOrderProduct,
			corpDivMaterials, corpDivProduct)
	}
	for _, pt := range parts {
		if pt.SupplierCorpID != 0 && pt.SupplierCorpID != corpID {
			continue // a consortium member builds it
		}
		if short := pt.Needed - onHand[pt.ComponentTypeID]; short > 0 && pt.BPTypeID != 0 {
			out = append(out, buildPlanRow{BPTypeID: pt.BPTypeID, ProductTypeID: pt.ComponentTypeID,
				Wanted: int(short), Priority: programPriority, Purpose: programPurpose})
		}
	}
	if p.Stage == programStageBuilding && len(partsShort(parts, onHand)) == 0 &&
		m.corpStockOf(ctx, corpID, officeID, p.HullTypeID, corpOrderProduct, corpDivProduct, corpDivMaterials, corpDivStore) == 0 {
		out = append(out, buildPlanRow{BPTypeID: p.HullBPTypeID, ProductTypeID: p.HullTypeID,
			Wanted: 1, Priority: programPriority, Purpose: programPurpose})
	}
	return out
}

// programPartTypes is the component types of the corporation's open program:
// the output lister leaves them alone, and the yard moves them to Materials.
func (m *Mod) programPartTypes(ctx context.Context, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT pp.componentTypeID FROM rolebot_program_parts pp
		  JOIN rolebot_corp_program p ON p.programID = pp.programID
		 WHERE (p.corporationID = ? OR pp.supplierCorpID = ?) AND p.stage NOT IN (?, ?)`,
		corpID, corpID, programStageCrewed, programStageClosed)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t int64
		if rows.Scan(&t) == nil {
			out[t] = true
		}
	}
	return out
}

// programHandOver is the delivered stage: pick a pilot, wait for its skills,
// hand the hull over.
func (m *Mod) programHandOver(ctx context.Context, c *corpCEO, p *corpProgram, stationID, officeID int64) {
	if p.PilotID == 0 {
		pol, _ := m.loadCorpPolicy(ctx, c.CorpID)
		pilot := m.programPilotFor(ctx, c.CorpID, pol.DirectorID, p.HullTypeID)
		if pilot == 0 {
			return
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_program SET pilotID = ?, updatedAt = ? WHERE programID = ?`,
			pilot, clock.Unix(), p.ProgramID)
		p.PilotID = pilot
		m.ceoLog(ctx, c, powerProduction, "program", p.HullTypeID, 0, "%s program: %s will fly it once trained",
			m.typeNameOr(ctx, p.HullTypeID), m.charNameOr(ctx, pilot))
	}
	// Every skill the hull rests on, trained: the books are the treasury's (programBooks)
	// and the levels the planner's (programGoals).
	levels, err := evedb.TrainedSkillLevels(ctx, m.db, p.PilotID)
	if err != nil || len(treeShort(m.pilotSkillTree(ctx, p.HullTypeID), levels)) > 0 {
		return
	}
	itemID, err := m.handOverHull(ctx, c.CorpID, officeID, stationID, p.PilotID, p.HullTypeID)
	if err != nil || itemID == 0 {
		return
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_program SET stage = ?, hullItemID = ?, updatedAt = ? WHERE programID = ?`,
		programStageCrewed, itemID, clock.Unix(), p.ProgramID)
	m.ceoLog(ctx, c, powerProduction, "program", p.HullTypeID, 0, "%s program: handed the hull to %s — crewed",
		m.typeNameOr(ctx, p.HullTypeID), m.charNameOr(ctx, p.PilotID))
}

// programSummary is the CEO page's line on the open program.
func (m *Mod) programSummary(ctx context.Context, corpID int64) string {
	p := m.openProgramFor(ctx, corpID)
	if p == nil {
		return ""
	}
	parts := m.programParts(ctx, p.ProgramID)
	done := 0
	for _, pt := range parts {
		if pt.Delivered >= pt.Needed {
			done++
		}
	}
	s := fmt.Sprintf("%s — %s (%d of %d component lines on hand)", m.typeNameOr(ctx, p.HullTypeID), p.Stage, done, len(parts))
	if sk := m.programSkillSummary(ctx, p); sk != "" {
		s += "; " + sk
	}
	return s
}
