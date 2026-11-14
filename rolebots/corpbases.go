package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

// Bases: the offices a department works out of.
//
// An office carries purposes (corpofficehunt.go). Two are workplaces: the factory bit is an
// Industry base and the lab bit an R&D base. A department that fills its base's lines grows into
// another office (corpexpand.go), so a corporation may hold several of either, and a member
// works at the one it is posted to (corpposting.go). An R&D base is an office claimed for
// research whose station has research lines; an Industry base is one claimed for manufacturing
// whose station has manufacturing lines. The pinned factory (corpFactoryStation) keeps the
// store, replacements and ore refinery. Build orders are posted at every Industry base, each
// base's Materials division feeding its own orders (corpbuildbases.go).

// renderOfficeBits, set only by the live portal render (corpportal_live_test.go), adds purpose
// bits to an office's stored ones, so a page can be drawn on a read-only connection as it would
// look with a base not yet taken. Nil in the game.
var renderOfficeBits func(corpID, stationID int64) int

// officeBits is an office's stored purposes plus any the render adds.
func officeBits(corpID, stationID int64, stored int) int {
	if renderOfficeBits != nil {
		return stored | renderOfficeBits(corpID, stationID)
	}
	return stored
}

// lineUse is one activity's lines at a station.
type lineUse struct {
	Lines int
	Free  int   // lines with no running job
	Next  int64 // seconds until the soonest busy line frees, when none is free
	Ours  int   // the corporation's own running jobs of the activity
}

// corpBase is one office as a department sees it.
type corpBase struct {
	StationID, OfficeID, SystemID int64
	Name                          string
	Purposes                      int
	Dropping                      bool
	Factory                       bool // the pinned factory: the store, replacements, the refinery
	Lines                         map[int64]lineUse
	Pressure                      basePressure
	Posted                        []int64 // members whose Base it is
	Originals, Copies             int
	Backlog                       int // research and copy runs posted here and not yet done
}

// labLines is the base's research lines: material, time and copying.
func (b corpBase) labLines() lineUse {
	var u lineUse
	for _, act := range []int64{ramActivityMEResearch, ramActivityPEResearch, ramActivityCopying} {
		l := b.Lines[act]
		u.Lines += l.Lines
		u.Free += l.Free
		u.Ours += l.Ours
		if l.Free == 0 && l.Lines > 0 && (u.Next == 0 || l.Next < u.Next) {
			u.Next = l.Next
		}
	}
	if u.Free > 0 {
		u.Next = 0
	}
	return u
}

// mfgLines is the base's manufacturing lines.
func (b corpBase) mfgLines() lineUse { return b.Lines[ramActivityManufacturing] }

// armBaseBit is the office purpose that makes an office a department's base. Pure.
func armBaseBit(a corpArm) int {
	switch a {
	case armResearch:
		return officeLab
	case armIndustry:
		return officeFactory
	}
	return 0
}

// isArmBase reports whether an office with these purposes and lines is a base of the
// department. Pure.
func isArmBase(a corpArm, purposes int, hasLab, hasMfg bool) bool {
	switch a {
	case armResearch:
		return purposes&officeLab != 0 && hasLab
	case armIndustry:
		return purposes&officeFactory != 0 && hasMfg
	}
	return false
}

// stationLineUse reads every activity's lines at a station.
func (m *Mod) stationLineUse(ctx context.Context, corpID, stationID int64) (map[int64]lineUse, error) {
	now := win32Now()
	rows, err := m.db.QueryContext(ctx, `
		SELECT x.activityID, COUNT(*), SUM(x.nxt <= ?), MIN(x.nxt)
		  FROM (SELECT al.assemblyLineID, al.activityID, COALESCE(MAX(j.endProductionTime), 0) AS nxt
		          FROM ramassemblylines al
		          LEFT JOIN ramJobs j ON j.assemblyLineID = al.assemblyLineID AND j.completedStatusID = 0
		         WHERE al.containerID = ?
		         GROUP BY al.assemblyLineID, al.activityID) x
		 GROUP BY x.activityID`, now, stationID)
	if err != nil {
		return nil, fmt.Errorf("stationLineUse: %w", err)
	}
	out := map[int64]lineUse{}
	for rows.Next() {
		var act int64
		var u lineUse
		var free sql.NullInt64
		var next int64
		if rows.Scan(&act, &u.Lines, &free, &next) != nil {
			continue
		}
		u.Free = int(free.Int64)
		if u.Free == 0 && next > now {
			u.Next = (next - now) / 10_000_000
		}
		out[act] = u
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	ours, err := m.db.QueryContext(ctx, `
		SELECT j.activityID, COUNT(*)
		  FROM ramJobs j JOIN ramassemblylines al ON al.assemblyLineID = j.assemblyLineID
		 WHERE j.ownerID = ? AND al.containerID = ? AND j.completedStatusID = 0
		 GROUP BY j.activityID`, corpID, stationID)
	if err != nil {
		return out, nil
	}
	defer ours.Close()
	for ours.Next() {
		var act int64
		var n int
		if ours.Scan(&act, &n) == nil {
			u := out[act]
			u.Ours = n
			out[act] = u
		}
	}
	return out, nil
}

// corpBases is every base of one department, the pinned factory first, then by station, with
// its lines, pressure, who is posted there and what of the library it holds. Read-only: it
// reads the purposes the CEO's estate think last stored.
func (m *Mod) corpBases(ctx context.Context, corpID int64, arm corpArm) ([]corpBase, error) {
	bit := armBaseBit(arm)
	if bit == 0 {
		return nil, nil
	}
	purposes := map[int64]int{}
	prow, err := m.db.QueryContext(ctx, `
		SELECT r.stationID, r.purposes FROM rolebot_corp_offices r
		  JOIN crpoffices o ON o.corporationID = r.corporationID AND o.stationID = r.stationID
		 WHERE r.corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("corpBases: %w", err)
	}
	for prow.Next() {
		var st int64
		var bits int
		if prow.Scan(&st, &bits) == nil {
			purposes[st] = officeBits(corpID, st, bits)
		}
	}
	prow.Close()
	factory := m.corpFactoryStation(ctx, corpID)
	kind := pressureLab
	if arm == armIndustry {
		kind = pressureMfg
	}
	press, _ := m.basePressures(ctx, corpID, kind)
	var out []corpBase
	for st, bits := range purposes {
		if bits&bit == 0 {
			continue
		}
		lines, err := m.stationLineUse(ctx, corpID, st)
		if err != nil {
			return nil, err
		}
		hasLab := lines[ramActivityMEResearch].Lines > 0
		hasMfg := lines[ramActivityManufacturing].Lines > 0
		if !isArmBase(arm, bits, hasLab, hasMfg) {
			continue
		}
		b := corpBase{StationID: st, Purposes: bits, Factory: st == factory, Lines: lines, Pressure: press[st]}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Factory != out[j].Factory {
			return out[i].Factory
		}
		return out[i].StationID < out[j].StationID
	})
	for i := range out {
		b := &out[i]
		b.Name = stationNameOf(ctx, m, b.StationID)
		b.SystemID = m.stationSystemID(ctx, b.StationID)
		b.OfficeID, _, _ = evedb.GetCorpOfficeAt(ctx, m.db, corpID, b.StationID)
		var state string
		_ = m.db.QueryRowContext(ctx, `
			SELECT state FROM rolebot_corp_offices WHERE corporationID = ? AND stationID = ?`,
			corpID, b.StationID).Scan(&state)
		b.Dropping = state == "dropping"
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(COALESCE(bp.copy, 0) = 0), 0), COALESCE(SUM(COALESCE(bp.copy, 0) = 1), 0)
			  FROM entity e
			  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
			  LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
			 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag IN (`+corpLibraryDivisions+`)`,
			corpID, b.OfficeID).Scan(&b.Originals, &b.Copies)
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(GREATEST(runsWanted - runsDone, 0)), 0) FROM rolebot_corp_work
			 WHERE corporationID = ? AND stationID = ? AND status = 'open' AND kind IN ('me', 'pe', 'copy')`,
			corpID, b.StationID).Scan(&b.Backlog)
		if ids, err := int64Column(ctx, m, `
			SELECT r.characterID FROM chrCorporationRoles r
			  JOIN character_ c ON c.characterID = r.characterID
			 WHERE c.corporationID = ? AND r.baseID = ?`, corpID, b.StationID); err == nil {
			b.Posted = ids
		}
	}
	return out, nil
}

// armBaseStations is the stations of one department's bases, cheaply: the office purposes and
// a line check, no counting. The pinned factory is always an Industry base; for R&D it counts
// only if its station has labs.
func (m *Mod) armBaseStations(ctx context.Context, corpID int64, arm corpArm) []int64 {
	bit := armBaseBit(arm)
	if bit == 0 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, purposes FROM rolebot_corp_offices
		 WHERE corporationID = ? AND state <> 'dropping'`, corpID)
	if err != nil {
		return nil
	}
	type off struct {
		st   int64
		bits int
	}
	var list []off
	for rows.Next() {
		var o off
		if rows.Scan(&o.st, &o.bits) == nil {
			if o.bits = officeBits(corpID, o.st, o.bits); o.bits&bit != 0 {
				list = append(list, o)
			}
		}
	}
	rows.Close()
	var out []int64
	for _, o := range list {
		hasLab := m.stationHasLab(ctx, o.st)
		hasMfg := m.stationHasLine(ctx, o.st)
		if isArmBase(arm, o.bits, hasLab, hasMfg) {
			out = append(out, o.st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// corpWorkStation is where a member takes corporation work: its own Base when that is an R&D
// or Industry base, else the factory. corpWorkFor only offers what was posted where the member
// stands, so a member works the base it is posted to and nothing else.
func (m *Mod) corpWorkStation(ctx context.Context, b *botRun) (stationID, officeID int64) {
	factory, factoryOffice := m.corpFactoryOffice(ctx, b.CorpID)
	if !postingsOn() {
		return factory, factoryOffice
	}
	var base int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(baseID, 0) FROM chrCorporationRoles WHERE characterID = ?`, b.CharID).Scan(&base)
	if base == 0 || base == factory {
		return factory, factoryOffice
	}
	for _, arm := range []corpArm{armResearch, armIndustry} {
		for _, st := range m.armBaseStations(ctx, b.CorpID, arm) {
			if st == base {
				off, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, b.CorpID, st)
				if off != 0 && off != st {
					return st, off
				}
			}
		}
	}
	return factory, factoryOffice
}
