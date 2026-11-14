package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The parts of the Industry page (corpigb.go industryBody): how a member takes part, the
// build board, the jobs on the factory floor, the materials against the board, the factory
// bases and the originals at the factory. The R&D page (corprnd_igb.go) reads the same jobs.
// A corporation job needs Factory Manager plus view access to the Materials, Blueprint
// Library and Production divisions, which the Industry and R&D departments give every member
// (armIndustryRoles). The page says whether the reader holds them and links the department.

// runningJob is one corporation job on a line: what print is in it (by item,
// so a page can say which of two originals of a type is busy), who put it
// there, and at which station.
type runningJob struct {
	Activity    int
	InstallerID int64
	BPItemID    int64
	BPTypeID    int64
	OutTypeID   int64
	StationID   int64
	Runs        int
	EndWin32    int64
	Ready       bool
}

// corpRunningJobs is the corporation's undelivered jobs, soonest done first.
func (m *Mod) corpRunningJobs(ctx context.Context, corpID int64) []runningJob {
	rows, err := m.db.QueryContext(ctx, `
		SELECT j.activityID, j.installerID, j.installedItemID, COALESCE(e.typeID, 0), j.outputTypeID,
		       COALESCE(al.containerID, 0), j.runs, j.endProductionTime, j.completedStatusID
		  FROM ramJobs j
		  LEFT JOIN entity e ON e.itemID = j.installedItemID
		  LEFT JOIN ramassemblylines al ON al.assemblyLineID = j.assemblyLineID
		 WHERE j.ownerID = ? AND j.completedStatusID IN (0, 1)
		 ORDER BY j.endProductionTime
		 LIMIT 400`, corpID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	now := win32Now()
	var out []runningJob
	for rows.Next() {
		var j runningJob
		var status int
		if rows.Scan(&j.Activity, &j.InstallerID, &j.BPItemID, &j.BPTypeID, &j.OutTypeID, &j.StationID, &j.Runs,
			&j.EndWin32, &status) == nil {
			j.Ready = status == 1 || (j.EndWin32 > 0 && j.EndWin32 <= now)
			out = append(out, j)
		}
	}
	return out
}

// jobActivityWord names a ramJobs activity for a row.
func jobActivityWord(act int) string {
	switch act {
	case 1:
		return "building"
	case 3:
		return "PE research"
	case 4:
		return "ME research"
	case 5:
		return "copying"
	case 7:
		return "reverse engineering"
	case 8:
		return "invention"
	}
	return "job"
}

// splitBoard parts the board into building (manufacture, refine) and
// research (ME, PE, copies). Pure.
func splitBoard(work []corpWork) (build, research []corpWork) {
	for _, w := range work {
		switch w.Kind {
		case workKindME, workKindPE, workKindCopy:
			research = append(research, w)
		default:
			build = append(build, w)
		}
	}
	return build, research
}

// boardGridAlign / boardGridWidth is the grid both board panels share.
var (
	boardGridAlign = []string{"", "", "right"}
	boardGridWidth = []string{"36", "", "120"}
)

// boardPanel is one half of the board: icon, what and how far along, runs
// left and the wage.
func (m *Mod) boardPanel(ctx context.Context, pg igbPage, title, note string, work []corpWork, empty string) esp.Panel {
	p := esp.Panel{Title: title, Note: note, Align: boardGridAlign, Width: boardGridWidth, Empty: empty}
	for _, w := range work {
		what := esp.IGBText(m.typeNameOr(ctx, w.ProductTypeID))
		icon, unit := w.ProductTypeID, "run"
		switch w.Kind {
		case workKindME:
			what, icon, unit = "ME on "+esp.IGBText(m.typeNameOr(ctx, w.BPTypeID)), w.BPTypeID, "level"
		case workKindPE:
			what, icon, unit = "PE on "+esp.IGBText(m.typeNameOr(ctx, w.BPTypeID)), w.BPTypeID, "level"
		case workKindCopy:
			what, icon, unit = "copy "+esp.IGBText(m.typeNameOr(ctx, w.BPTypeID)), w.BPTypeID, "copy"
		case workKindRefine:
			what = "refine " + what
		}
		frac := 0.0
		if w.RunsWanted > 0 {
			frac = float64(w.RunsDone) / float64(w.RunsWanted)
		}
		line2 := esp.Bar(frac, 12, pg.pal.Up) + " " + pg.soft(fmt.Sprintf("%d of %d done", w.RunsDone, w.RunsWanted))
		if w.Purpose == "srp" {
			line2 += " " + pg.tint(pg.pal.Amber, "replaces a loss")
		}
		// Who asked, and the hangar to set as Output (corprequest.go).
		line2 += "<br>" + m.deliverLine(ctx, pg, w.CorpID, workRequesterArm(w), workDestFlag(w), workDestStation(w))
		p.Rows = append(p.Rows, []string{
			esp.ShipIcon(icon, 32),
			igbF3(what) + "<br>" + line2,
			igbF3(fmt.Sprintf("<b>%s</b>", esp.ISKShort(w.WagePerRun))) + pg.dim(" per "+unit) + "<br>" +
				pg.soft(fmt.Sprintf("%d %s(s) left", w.RunsLeft(), unit)),
		})
	}
	return p
}

// corpJobGate reports whether a member can install corporation jobs from the library
// against its materials: the CEO, a Director, or Factory Manager with view access to the
// three divisions (db.ResolveStationCorpJob's rule).
func corpJobGate(roles uint64, isCEO bool) bool {
	if isCEO || roles&corp.RoleDirector != 0 {
		return true
	}
	return roles&armCorpJobGate == armCorpJobGate
}

// writeTakePart is how a member takes part: four steps beside where the
// reader stands.
func (m *Mod) writeTakePart(ctx context.Context, b *strings.Builder, pg igbPage, corpID, viewer, factoryID int64, jobs []runningJob) {
	host := esp.IGBHost()
	factory := "the corporation's factory office"
	if factoryID != 0 {
		factory = "<b>" + esp.IGBText(m.stationLabel(ctx, factoryID)) + "</b>"
	}
	if n := len(m.industryBaseStations(ctx, corpID)); n > 1 {
		factory = fmt.Sprintf("whichever of its <b>%d</b> factory offices is yours", n)
	}
	steps := esp.Panel{Title: "How to take part", Align: []string{"right", ""}, Width: []string{"24", ""}}
	step := func(n int, text string) {
		steps.Rows = append(steps.Rows, []string{pg.accent(fmt.Sprintf("%d", n)), igbF3(text)})
	}
	step(1, `Join <font color=`+armColor(armIndustry)+`><b>Industry</b></font> to build, or <font color=`+
		armColor(armResearch)+`><b>Research &amp; Development</b></font> for research and copies, on the `+
		`<a href="`+host+`/corp/">Overview</a>. The corporation mails you a one-click confirmation.`)
	step(2, "Your department gives you the roles the work needs: <b>Factory Manager</b> and view access to "+
		"every hangar division, so the Output list shows them all. Viewing is not taking: nothing lets a print leave the office.")
	step(3, "Dock at "+factory+" &mdash; work is taken at the office you are posted to, your <b>Base</b>. In Science "+
		"&amp; Industry, pick the print from the Blueprint Library and install it <b>for the corporation</b>; the "+
		"materials come out of Materials.")
	step(4, "Set <b>Output</b> to the division the order names under it &mdash; Sales' shelf for market goods, the "+
		"Armory for Security's kit, <b>Production</b> when it names none (every request is on "+
		`<a href="`+host+requestsPath+`">Requests</a>). The wage is paid on delivery wherever it lands, and the `+
		"corporation files it where it was asked for. The work earns merit toward your next rank.")

	you := esp.Panel{Title: "You", Align: []string{"", ""}, Width: []string{"90", ""}}
	if viewer == 0 {
		you.Rows = append(you.Rows, []string{"", pg.soft("Open this page from your own terminal.")})
	} else {
		var roles uint64
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(corprole, 0) | COALESCE(rolesAtBase, 0) FROM chrCorporationRoles WHERE characterID = ?`, viewer).Scan(&roles)
		isCEO := m.corpCEOID(ctx, corpID) == viewer
		dept := pg.tint(pg.pal.Amber, "none yet")
		seat, inArm := armOf(viewer, corpID)
		if inArm {
			dept = armName(seat.Arm)
		}
		you.Rows = append(you.Rows, []string{pg.dim3("Department"), dept})
		if corpJobGate(roles, isCEO) {
			you.Rows = append(you.Rows, []string{pg.dim3("Roles"), pg.tint(pg.pal.Green, "ready") + " " +
				pg.soft("&mdash; you can install corporation jobs")})
		} else {
			fix := `<a href="` + host + `/corp/join?arm=industry">join Industry</a> or <a href="` + host +
				`/corp/join?arm=research">R&amp;D</a>`
			you.Rows = append(you.Rows, []string{pg.dim3("Roles"), pg.tint(pg.pal.Red, "not yet") + " " + igbF3("&mdash; "+fix)})
		}
		mine := 0
		for _, j := range jobs {
			if j.InstallerID == viewer {
				mine++
			}
		}
		you.Rows = append(you.Rows, []string{pg.dim3("Your jobs"), igbF3(fmt.Sprintf("%d in the factory", mine))})
		you.Rows = append(you.Rows, []string{pg.dim3("Your desk"),
			igbF3(`<a href="`+host+`/corp/join?desk=1">mail me the link</a>`) + " " + pg.soft("rank and pay")})
	}
	esp.PanelBand(b, pg.pal, []esp.Panel{steps, you})
}

// jobsGridAlign / jobsGridWidth is the grid both job panels share.
var (
	jobsGridAlign = []string{"", "", "", "right"}
	jobsGridWidth = []string{"36", "", "150", "90"}
)

// writeBuildJobs is the factory floor: every build job, who runs it and when
// it is done.
func (m *Mod) writeBuildJobs(ctx context.Context, b *strings.Builder, pg igbPage, jobs []runningJob, viewer int64) {
	const perPanel = 20
	build := esp.Panel{Title: "Building", Align: jobsGridAlign, Width: jobsGridWidth, Empty: "No line is building for us."}
	nb := 0
	for _, j := range jobs {
		if j.Activity != 1 {
			continue
		}
		p := &build
		nb++
		if nb > perPanel {
			continue
		}
		typeID, what := j.BPTypeID, esp.IGBText(m.typeNameOr(ctx, j.BPTypeID))
		if j.Activity == 1 && j.OutTypeID != 0 {
			typeID, what = j.OutTypeID, esp.IGBText(m.typeNameOr(ctx, j.OutTypeID))
		}
		when := pg.soft("ends " + igbIn(evedb.Win32ToUnix(j.EndWin32)))
		if j.Ready {
			when = pg.tint(pg.pal.Green, "ready")
		}
		icon, name := m.pilotCells(ctx, j.InstallerID, 24)
		shade := ""
		if j.InstallerID == viewer {
			shade = igbShadeMine
		}
		p.Rows = append(p.Rows, []string{esp.ShipIcon(typeID, 32),
			igbF3(what) + "<br>" + pg.soft(fmt.Sprintf("%s &middot; %d run(s)", jobActivityWord(j.Activity), j.Runs)),
			icon + " " + igbF3(name), when})
		p.Shades = append(p.Shades, shade)
	}
	if nb > perPanel {
		build.Note = fmt.Sprintf("%d jobs, soonest first", nb)
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{build}})
}

// writeFactoryBases is Industry's bases: manufacturing lines in use, who is
// posted there and how close the department is to outgrowing it.
func (m *Mod) writeFactoryBases(ctx context.Context, b *strings.Builder, pg igbPage, corpID, viewer int64) {
	bases, _ := m.corpBases(ctx, corpID, armIndustry)
	p := esp.Panel{Title: "Where Industry works", Note: "a base is full when members are turned away for want of a line",
		Align: baseGridAlign, Width: baseGridWidth, Empty: "The corporation has no factory office yet."}
	for _, bs := range bases {
		u := bs.mfgLines()
		p.Rows = append(p.Rows, m.baseRow(ctx, pg, bs, u, viewer, "manufacturing"))
		p.Shades = append(p.Shades, baseShade(bs, viewer))
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
}

// materialRow is one material: held in the Materials division against what
// the open build orders need.
type materialRow struct {
	TypeID     int64
	Held, Need int64
}

// materialRows sums the board's bills against the division's stock, the
// shortest first. Pure.
func materialRows(bills []map[int64]int64, held map[int64]int64) []materialRow {
	need := map[int64]int64{}
	for _, bill := range bills {
		for t, n := range bill {
			need[t] += n
		}
	}
	out := make([]materialRow, 0, len(need))
	for t, n := range need {
		out = append(out, materialRow{TypeID: t, Held: held[t], Need: n})
	}
	sort.Slice(out, func(i, j int) bool {
		ci, cj := coverage(out[i]), coverage(out[j])
		if ci != cj {
			return ci < cj
		}
		return out[i].TypeID < out[j].TypeID
	})
	return out
}

func coverage(r materialRow) float64 {
	if r.Need <= 0 {
		return 1
	}
	return min(1, float64(r.Held)/float64(r.Need))
}

// writeMaterialsBand is the materials against the board beside the buyback:
// one panel per Industry base, each its own Materials division against the
// orders posted there.
func (m *Mod) writeMaterialsBand(ctx context.Context, b *strings.Builder, pg igbPage, corpID, factoryID, officeID int64, bases []int64) {
	var panels []esp.Panel
	if len(bases) > 1 {
		for _, st := range bases {
			off, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, corpID, st)
			if off == st {
				off = 0
			}
			panels = append(panels, m.materialsPanel(ctx, pg, corpID, st, off,
				"At "+esp.IGBText(stationNameOf(ctx, m, st)), "Materials division against the orders posted here"))
		}
	} else {
		panels = append(panels, m.materialsPanel(ctx, pg, corpID, factoryID, officeID,
			"On hand vs needed", "Materials division against every open build order"))
	}
	buys := esp.Panel{Title: "The corporation buys", Note: "sell into its buy orders",
		Align: []string{"", "", "", "right"}, Width: []string{"36", "", "110", "120"}, Empty: "No standing buy orders."}
	for _, o := range m.buybackRows(ctx, corpID) {
		buys.Rows = append(buys.Rows, []string{
			esp.ShipIcon(o.TypeID, 32),
			igbF3(esp.IGBText(m.typeNameOr(ctx, o.TypeID))) + "<br>" + pg.soft(esp.IGBText(m.stationLabel(ctx, o.StationID))),
			igbF3(esp.ISKShort(float64(o.Remaining))) + " " + pg.dim("wanted"),
			igbF3(esp.ISKShort(o.Price)) + " " + pg.dim("each"),
		})
	}
	panels = append(panels, buys)
	var grid [][]esp.Panel
	for i := 0; i < len(panels); i += 2 {
		grid = append(grid, panels[i:min(i+2, len(panels))])
	}
	esp.PanelGrid(b, pg.pal, grid)
}

// materialsPanel is one base's Materials division against the build orders
// posted there, the shortest first.
func (m *Mod) materialsPanel(ctx context.Context, pg igbPage, corpID, stationID, officeID int64, title, note string) esp.Panel {
	const maxRows = 12
	mats := esp.Panel{Title: title, Note: note,
		Align: []string{"", "", "", "right"}, Width: []string{"36", "", "110", "120"},
		Empty: "The board needs nothing it does not have."}
	if stationID != 0 && officeID != 0 {
		held := map[int64]int64{}
		if rows, err := m.db.QueryContext(ctx, `
			SELECT typeID, SUM(quantity) FROM entity
			 WHERE ownerID = ? AND locationID = ? AND flag = ? GROUP BY typeID`,
			corpID, officeID, corpDivMaterials); err == nil {
			for rows.Next() {
				var t, n int64
				if rows.Scan(&t, &n) == nil {
					held[t] = n
				}
			}
			rows.Close()
		}
		bills, _ := m.corpOrderBills(ctx, corpID, stationID)
		for i, r := range materialRows(bills, held) {
			if i == maxRows {
				mats.Note = "the shortest first"
				break
			}
			cov := coverage(r)
			color := pg.pal.Green
			state := pg.tint(pg.pal.Green, "covered")
			if cov < 1 {
				color = pg.pal.Red
				state = pg.tint(pg.pal.Red, esp.ISKShort(float64(r.Need-r.Held))+" short")
			}
			mats.Rows = append(mats.Rows, []string{esp.ShipIcon(r.TypeID, 32),
				igbF3(esp.IGBText(m.typeNameOr(ctx, r.TypeID))) + "<br>" +
					pg.soft(fmt.Sprintf("%s held of %s", esp.ISKShort(float64(r.Held)), esp.ISKShort(float64(r.Need)))),
				esp.Bar(cov, 10, color), state})
		}
	}
	return mats
}

// writeLibraryPanels is the library's originals with their research levels,
// across two panels; a print out on a job says so.
func (m *Mod) writeLibraryPanels(ctx context.Context, b *strings.Builder, pg igbPage, corpID, factoryID int64, jobs []runningJob) {
	const maxPrints = 24
	align := []string{"", "", "right", "right"}
	width := []string{"36", "", "60", "60"}
	left := esp.Panel{Title: "Originals", Note: "material (ME) and time (PE) research levels",
		Align: align, Width: width, Empty: "The library holds no originals at the factory."}
	right := esp.Panel{Title: "&nbsp;", Align: align, Width: width}
	if factoryID == 0 {
		esp.PanelGrid(b, pg.pal, [][]esp.Panel{{left, right}})
		return
	}
	prints, _ := m.libraryPrints(ctx, corpID, factoryID)
	working := map[int64]string{}
	for _, j := range jobs {
		if j.Activity != 1 && j.BPItemID != 0 {
			working[j.BPItemID] = jobActivityWord(j.Activity) + " by " + esp.IGBText(m.charNameOr(ctx, j.InstallerID))
		}
	}
	// Research under way first, then the most researched.
	sort.SliceStable(prints, func(i, j int) bool {
		wi, wj := working[prints[i].ItemID] != "", working[prints[j].ItemID] != ""
		if wi != wj {
			return wi
		}
		return prints[i].ME+prints[i].PE > prints[j].ME+prints[j].PE
	})
	if len(prints) > maxPrints {
		left.Note = fmt.Sprintf("%d originals; the %d most worked", len(prints), maxPrints)
		prints = prints[:maxPrints]
	}
	half := (len(prints) + 1) / 2
	for i, p := range prints {
		status := pg.soft("on the shelf")
		switch {
		case working[p.ItemID] != "":
			status = pg.tint(armColor(armResearch), "in the lab: "+working[p.ItemID])
		case p.Flag == 111:
			status = pg.tint(pg.pal.Amber, "on a production line")
		}
		row := []string{esp.ShipIcon(p.BPTypeID, 32),
			igbF3(esp.IGBText(m.typeNameOr(ctx, p.BPTypeID))) + "<br>" + status,
			igbF3(fmt.Sprintf("ME <b>%d</b>", p.ME)), igbF3(fmt.Sprintf("PE <b>%d</b>", p.PE))}
		if i < half {
			left.Rows = append(left.Rows, row)
		} else {
			right.Rows = append(right.Rows, row)
		}
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{left, right}})
}
