package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
)

// /corp/research: the Research & Development department's page, one row per original in
// the library with everything asked of it and whose hands it is in. Sections: your desk
// (your base, your roles there, your jobs, errands you may take, your pay this week);
// management's orders; the figures; the research bases (lines in use, who is posted, what
// of the library each holds, and the pressure that makes the corporation expand,
// corpexpand.go); the library (levels against target, directives, who holds each); prints
// on the move between bases (corpprintmove.go); lately. Every member may read it; work is
// taken only at the base a member is posted to, so each row says where.

const (
	rndLibraryPageRows = 30
	rndLatelyRows      = 10
)

// baseGridAlign / baseGridWidth is the bases panels' grid (R&D and Industry).
var (
	baseGridAlign = []string{"", "", "", "", ""}
	baseGridWidth = []string{"", "130", "110", "140", "150"}
)

// baseShade shades the viewer's own base.
func baseShade(bs corpBase, viewer int64) string {
	if containsInt64(bs.Posted, viewer) {
		return igbShadeMine
	}
	return ""
}

// basePressureLine is the base's pressure in words, for its bar. Pure.
func basePressureLine(p basePressure, noun string) string {
	switch {
	case p.ByRefusal:
		return fmt.Sprintf("full &mdash; %d member(s) turned away %d time(s)", p.Pilots, p.Refusals)
	case p.ByWait:
		return fmt.Sprintf("full &mdash; %s jobs wait %.1f h for a line against %.1f h of work",
			activityLineNoun(p.Wait.ActivityID), p.Wait.AvgWaitSec/3600, p.Wait.AvgRunSec/3600)
	case p.Refusals > 0:
		return fmt.Sprintf("%d refusal(s) for want of a %s line", p.Refusals, noun)
	case p.Wait.Jobs > 0 && p.Wait.AvgWaitSec >= basePressureWaitFloorSec:
		return fmt.Sprintf("%s jobs wait %.1f h for a line", activityLineNoun(p.Wait.ActivityID), p.Wait.AvgWaitSec/3600)
	}
	return "room to spare"
}

// baseRow is one base: where, lines, people, library, pressure.
func (m *Mod) baseRow(ctx context.Context, pg igbPage, bs corpBase, u lineUse, viewer int64, noun string) []string {
	what := "<b>" + esp.IGBText(bs.Name) + "</b>"
	label := officePurposeLabel(bs.Purposes)
	if bs.Factory {
		label += " &middot; the store and the refinery"
	}
	if bs.Dropping {
		label += " &middot; " + pg.tint(pg.pal.Red, "being given up")
	}
	lines := igbF3(fmt.Sprintf("<b>%d</b> of %d busy", u.Lines-u.Free, u.Lines))
	switch {
	case u.Lines == 0:
		lines = igbF3("no " + noun + " lines")
	case u.Free > 0:
		lines += "<br>" + pg.soft(fmt.Sprintf("%d free &middot; %d ours", u.Free, u.Ours))
	default:
		lines += "<br>" + pg.soft(fmt.Sprintf("next free %s &middot; %d ours", igbIn(clock.Unix()+u.Next), u.Ours))
	}
	posted := igbF3(fmt.Sprintf("%d posted", len(bs.Posted)))
	if containsInt64(bs.Posted, viewer) {
		posted += "<br>" + pg.tint(pg.pal.Green, "your base")
	}
	lib := igbF3(fmt.Sprintf("%d original(s)", bs.Originals)) + "<br>" +
		pg.soft(fmt.Sprintf("%d cop(ies) &middot; %d run(s) queued", bs.Copies, bs.Backlog))
	color := pg.pal.Green
	switch {
	case bs.Pressure.Full:
		color = pg.pal.Red
	case bs.Pressure.Refusals > 0:
		color = pg.pal.Amber
	}
	press := esp.Bar(bs.Pressure.Score, 10, color) + "<br>" + pg.soft(basePressureLine(bs.Pressure, noun))
	return []string{igbF3(what) + "<br>" + pg.soft(label), lines, posted, lib, press}
}

// handleIGBCorpResearch serves /corp/research.
func (m *Mod) handleIGBCorpResearch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 || !m.corpHasWorkDesk(ctx, corpID) {
		esp.WriteCorpPortalPage(w, r, "research", "<font size=3>Your corporation keeps no blueprint library, so it "+
			"posts no research. Corporations with an industry desk research and copy their prints here.</font>")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	esp.WriteCorpPortalPage(w, r, "research", m.researchBody(ctx, r, corpID, charID, qInt(r, "base"), max(page, 0)))
}

// rndPrint is one original as the R&D page draws it.
type rndPrint struct {
	ItemID, BPTypeID, StationID int64
	Flag, ME, PE                int64
	Orders                      []corpWork
	Job                         *runningJob
	Move                        *printMove
	State                       int // rndStateMine.. for the order
	Status                      string
}

// The order rows are drawn in: the viewer's base, then the stuck, the
// working, the moving, the queued, the idle.
const (
	rndBlocked = iota
	rndWorking
	rndMoving
	rndQueued
	rndIdle
)

// rndOriginals is every original the corporation holds in an office, with
// its levels.
func (m *Mod) rndOriginals(ctx context.Context, corpID int64) ([]rndPrint, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, o.stationID, e.flag,
		       COALESCE(b.materialLevel, 0), COALESCE(b.productivityLevel, 0)
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE o.corporationID = ? AND e.flag IN (`+corpLibraryDivisions+`) AND COALESCE(b.copy, 0) = 0`, corpID)
	if err != nil {
		return nil, fmt.Errorf("rndOriginals: %w", err)
	}
	defer rows.Close()
	var out []rndPrint
	for rows.Next() {
		var p rndPrint
		if rows.Scan(&p.ItemID, &p.BPTypeID, &p.StationID, &p.Flag, &p.ME, &p.PE) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// rndDirectives is a print's orders in a few words: "ME 3/16 · PE 0/10 ·
// copies 1/3". Pure.
func rndDirectives(orders []corpWork) string {
	var parts []string
	for _, k := range []string{workKindME, workKindPE, workKindCopy} {
		done, want := 0, 0
		for _, w := range orders {
			if w.Kind == k {
				done += w.RunsDone
				want += w.RunsWanted
			}
		}
		if want == 0 {
			continue
		}
		label := map[string]string{workKindME: "ME", workKindPE: "PE", workKindCopy: "copies"}[k]
		parts = append(parts, fmt.Sprintf("%s %d/%d", label, done, want))
	}
	return strings.Join(parts, " &middot; ")
}

// rndOrdersLeft is the runs a print's orders still want. Pure.
func rndOrdersLeft(orders []corpWork) int {
	n := 0
	for _, w := range orders {
		n += w.RunsLeft()
	}
	return n
}

// researchBody is the page.
func (m *Mod) researchBody(ctx context.Context, r *http.Request, corpID, viewer, baseFilter int64, page int) string {
	pg := m.igbPageFor(r)
	host := esp.IGBHost()
	c, _ := getCEO(ctx, m.db, corpID)
	meTarget := int64(bpoFullMEDefault)
	if c != nil {
		if t := ceoCharterOf(ctx, m, c).METarget; t > 0 {
			meTarget = int64(t)
		}
	}
	bases, _ := m.corpBases(ctx, corpID, armResearch)
	baseByStation := map[int64]corpBase{}
	for _, bs := range bases {
		baseByStation[bs.StationID] = bs
	}
	work, _ := m.openWorkFor(ctx, corpID)
	var rwork []corpWork
	for _, w := range work {
		if w.Kind == workKindME || w.Kind == workKindPE || w.Kind == workKindCopy {
			rwork = append(rwork, w)
		}
	}
	jobs := m.corpRunningJobs(ctx, corpID)
	jobByItem := map[int64]*runningJob{}
	labJobs := 0
	for i := range jobs {
		if jobs[i].Activity != 1 {
			labJobs++
			if jobs[i].BPItemID != 0 {
				jobByItem[jobs[i].BPItemID] = &jobs[i]
			}
		}
	}
	live, _ := m.printMovesWhere(ctx, `WHERE corporationID = ? AND status IN `+liveMoveStatuses+` ORDER BY postedAt`, corpID)
	moveByItem := map[int64]*printMove{}
	for i := range live {
		moveByItem[live[i].ItemID] = &live[i]
	}
	prints, _ := m.rndOriginals(ctx, corpID)
	// A print out of every office on a move is still the corporation's.
	have := map[int64]bool{}
	for _, p := range prints {
		have[p.ItemID] = true
	}
	for _, mv := range live {
		if mv.Kind == moveOriginal && !have[mv.ItemID] {
			p := rndPrint{ItemID: mv.ItemID, BPTypeID: mv.BPTypeID}
			_ = m.db.QueryRowContext(ctx, `
				SELECT COALESCE(materialLevel, 0), COALESCE(productivityLevel, 0) FROM invBlueprints
				 WHERE blueprintID = ?`, mv.ItemID).Scan(&p.ME, &p.PE)
			prints = append(prints, p)
		}
	}
	// The directives, pinned by item first; an unpinned order goes to the
	// first print of its type at its station.
	byItem := map[int64]int{}
	for i, p := range prints {
		byItem[p.ItemID] = i
	}
	for _, w := range rwork {
		if i, ok := byItem[w.BPItemID]; ok && w.BPItemID != 0 {
			prints[i].Orders = append(prints[i].Orders, w)
			continue
		}
		for i := range prints {
			if prints[i].BPTypeID == w.BPTypeID && (w.StationID == 0 || prints[i].StationID == w.StationID) {
				prints[i].Orders = append(prints[i].Orders, w)
				break
			}
		}
	}

	var viewerBase int64
	var roles uint64
	if viewer != 0 {
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(baseID, 0), COALESCE(corprole, 0) | COALESCE(rolesAtBase, 0) FROM chrCorporationRoles WHERE characterID = ?`,
			viewer).Scan(&viewerBase, &roles)
	}

	blocked := 0
	for i := range prints {
		p := &prints[i]
		p.Job, p.Move = jobByItem[p.ItemID], moveByItem[p.ItemID]
		left := rndOrdersLeft(p.Orders)
		bs, isBase := baseByStation[p.StationID]
		at := esp.IGBText(stationNameOf(ctx, m, p.StationID))
		switch {
		case p.Job != nil:
			p.State = rndWorking
			word := jobActivityWord(p.Job.Activity)
			if p.Job.Ready {
				p.Status = pg.tint(pg.pal.Green, word+" done") + " " + pg.soft("by "+esp.IGBText(m.charNameOr(ctx, p.Job.InstallerID))+", waiting to be delivered")
			} else {
				p.Status = pg.tint(armColor(armResearch), word) + " " + pg.soft("by "+
					esp.IGBText(m.charNameOr(ctx, p.Job.InstallerID))+", ends "+igbIn(evedb.Win32ToUnix(p.Job.EndWin32)))
			}
		case p.Move != nil:
			p.State = rndMoving
			to := esp.IGBText(stationNameOf(ctx, m, p.Move.To))
			switch p.Move.Status {
			case moveCarrying:
				p.Status = pg.tint(pg.pal.Amber, "in transit") + " " + pg.soft("with "+esp.IGBText(m.charNameOr(ctx, p.Move.CarrierID))+" to "+to)
			case moveClaimed:
				p.Status = pg.tint(pg.pal.Amber, "being fetched") + " " + pg.soft("by "+esp.IGBText(m.charNameOr(ctx, p.Move.CarrierID))+" for "+to)
			default:
				p.Status = pg.tint(pg.pal.Amber, "waiting to move") + " " + pg.soft("to "+to+" &mdash; a Director carries it")
			}
		case left > 0 && !isBase:
			p.State = rndBlocked
			p.Status = pg.tint(pg.pal.Red, "no labs here") + " " + pg.soft(at+" is not a research base")
		case left > 0 && bs.Pressure.Full:
			p.State = rndBlocked
			p.Status = pg.tint(pg.pal.Red, "no free line") + " " + pg.soft("the lab lines at "+at+" are queued out")
		case left > 0 && len(bs.Posted) == 0:
			p.State = rndBlocked
			p.Status = pg.tint(pg.pal.Red, "nobody posted") + " " + pg.soft("no member is posted to "+at)
		case left > 0:
			p.State = rndQueued
			p.Status = pg.soft("on the shelf, waiting for a researcher at " + at)
		case p.Flag == 111:
			p.State = rndWorking
			p.Status = pg.tint(pg.pal.Amber, "on a production line")
		default:
			p.State = rndIdle
			if p.ME >= meTarget {
				p.Status = pg.soft("on the shelf &mdash; researched")
			} else {
				p.Status = pg.soft("on the shelf")
			}
		}
		if p.State == rndBlocked {
			blocked++
		}
	}
	sort.SliceStable(prints, func(i, j int) bool {
		a, b := prints[i], prints[j]
		ma, mb := a.StationID == viewerBase && viewerBase != 0, b.StationID == viewerBase && viewerBase != 0
		if ma != mb {
			return ma
		}
		if a.State != b.State {
			return a.State < b.State
		}
		if la, lb := rndOrdersLeft(a.Orders), rndOrdersLeft(b.Orders); la != lb {
			return la > lb
		}
		return a.ItemID < b.ItemID
	})

	var b strings.Builder
	b.WriteString(pg.soft(`The corporation's blueprint library and the research asked of it. Every member may read this page; `+
		`work is taken at the office you are posted to. Building is on the <a href="`+host+`/corp/industry">Industry</a> page.`) +
		"<br><br>\n")

	// 1. Your desk.
	esp.Section(&b, pg.pal, "Your desk")
	m.writeRndDesk(ctx, &b, pg, corpID, viewer, viewerBase, roles, baseByStation, jobs, live)
	b.WriteString("<br>\n")

	// 1b. What management asks.
	esp.Section(&b, pg.pal, "Orders from management")
	m.writeRndOrders(ctx, &b, pg, corpID, rwork, prints, viewer, viewerBase)
	b.WriteString("<br>\n")

	// 2. The figures.
	copies := 0
	for _, bs := range bases {
		copies += bs.Copies
	}
	pg.statStrip(&b, []igbStat{
		{Label: "Research bases", Value: strconv.Itoa(len(bases))},
		{Label: "Originals", Value: strconv.Itoa(len(prints))},
		{Label: "Copies", Value: strconv.Itoa(copies), Note: "at the bases"},
		{Label: "In the labs", Value: strconv.Itoa(labJobs), Note: "jobs"},
		{Label: "Directives", Value: strconv.Itoa(len(rwork)), Note: fmt.Sprintf("ME to %d, PE to %d", meTarget, meTarget/2)},
		{Label: "Stuck", Value: strconv.Itoa(blocked), Note: "prints"},
	})
	b.WriteString("<br>\n")

	// 3. The bases.
	esp.Section(&b, pg.pal, "Research bases")
	bp := esp.Panel{Title: "Where R&amp;D works", Note: "a base is full when researchers are turned away for want of a line",
		Align: baseGridAlign, Width: baseGridWidth,
		Empty: "The corporation has no office with research lines yet; the CEO looks for one."}
	for _, bs := range bases {
		row := m.baseRow(ctx, pg, bs, bs.labLines(), viewer, "research")
		row[0] += " " + pg.soft(`<a href="`+host+`/corp/research?base=`+strconv.FormatInt(bs.StationID, 10)+`">its prints</a>`)
		bp.Rows = append(bp.Rows, row)
		bp.Shades = append(bp.Shades, baseShade(bs, viewer))
	}
	esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{bp}})
	b.WriteString("<br>\n")

	// 4. The library.
	esp.Section(&b, pg.pal, "The library")
	m.writeRndLibrary(ctx, &b, pg, prints, meTarget, viewerBase, baseFilter, page)
	b.WriteString("<br>\n")

	// 5. On the move.
	esp.Section(&b, pg.pal, "Prints on the move")
	m.writeRndMoves(ctx, &b, pg, corpID, live, viewer)
	b.WriteString("<br>\n")

	// 6. Lately.
	m.writeRndLately(ctx, &b, pg, corpID)
	m.writeArmRequests(ctx, &b, pg, corpID, armResearch)
	return b.String()
}

// writeRndDesk is the viewer's own panel pair: who they are to R&D, and what
// waits on them.
func (m *Mod) writeRndDesk(ctx context.Context, b *strings.Builder, pg igbPage, corpID, viewer, viewerBase int64,
	roles uint64, baseByStation map[int64]corpBase, jobs []runningJob, live []printMove) {
	host := esp.IGBHost()
	you := esp.Panel{Title: "You", Align: []string{"", ""}, Width: []string{"100", ""}}
	desk := esp.Panel{Title: "On your desk", Align: []string{"", "", "right"}, Width: []string{"36", "", "120"},
		Empty: "Nothing waits on you right now."}
	if viewer == 0 {
		you.Rows = append(you.Rows, []string{"", pg.soft("Open this page from your own terminal.")})
		esp.PanelBand(b, pg.pal, []esp.Panel{you, desk})
		return
	}
	dept := pg.tint(pg.pal.Amber, "none yet") + " " +
		igbF3(`&mdash; <a href="`+host+`/corp/join?arm=research">join R&amp;D</a>`)
	if seat, ok := armOf(viewer, corpID); ok {
		dept = armName(seat.Arm)
	}
	you.Rows = append(you.Rows, []string{pg.dim3("Department"), dept})
	base := pg.tint(pg.pal.Amber, "none") + " " + pg.soft("&mdash; the CEO posts you by letter")
	if viewerBase != 0 {
		base = igbF3(esp.IGBText(stationNameOf(ctx, m, viewerBase)))
		if _, ok := baseByStation[viewerBase]; ok {
			base += " " + pg.tint(pg.pal.Green, "R&amp;D base")
		} else {
			base += " " + pg.soft("&mdash; not a research base")
		}
	}
	you.Rows = append(you.Rows, []string{pg.dim3("Your base"), base + " " +
		pg.soft(`<a href="`+host+`/corp/join?desk=1">change it</a>`)})
	isCEO := m.corpCEOID(ctx, corpID) == viewer
	if corpJobGate(roles, isCEO) {
		you.Rows = append(you.Rows, []string{pg.dim3("Roles"), pg.tint(pg.pal.Green, "ready") + " " +
			pg.soft("&mdash; you can install corporation research")})
	} else {
		you.Rows = append(you.Rows, []string{pg.dim3("Roles"), pg.tint(pg.pal.Red, "not yet") + " " +
			pg.soft("&mdash; R&amp;D gives Factory Manager and the library view")})
	}
	officer := isCEO || roles&corp.RoleDirector != 0
	if officer {
		you.Rows = append(you.Rows, []string{pg.dim3("Carries"), igbF3("originals and copies") + " " +
			pg.soft("&mdash; a Director moves the library")})
	}
	week := clock.Unix() - 7*86400
	var paid float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(g.wage), 0) FROM rolebot_corp_wages g
		  JOIN rolebot_corp_work w ON w.workID = g.workID
		 WHERE g.characterID = ? AND g.corporationID = ? AND g.paidAt > ? AND w.kind IN ('me', 'pe', 'copy')`,
		viewer, corpID, week).Scan(&paid)
	var carried float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(wage), 0) FROM rolebot_corp_print_moves
		 WHERE carrierID = ? AND corporationID = ? AND status = 'done' AND doneAt > ?`,
		viewer, corpID, week).Scan(&carried)
	// Credited to the labour record and paid in the weekly payroll (corppayroll.go), not cash in hand.
	you.Rows = append(you.Rows, []string{pg.dim3("Credited this week"),
		igbF3(esp.ISKShort(paid+carried)+" ISK") + " " + pg.soft(fmt.Sprintf("research %s, carrying %s",
			esp.ISKShort(paid), esp.ISKShort(carried)))})

	for _, j := range jobs {
		if j.InstallerID != viewer || j.Activity == 1 {
			continue
		}
		when := pg.soft("ends " + igbIn(evedb.Win32ToUnix(j.EndWin32)))
		if j.Ready {
			when = pg.tint(pg.pal.Green, "ready")
		}
		desk.Rows = append(desk.Rows, []string{esp.ShipIcon(j.BPTypeID, 32),
			igbF3(jobActivityWord(j.Activity)+" on "+esp.IGBText(m.typeNameOr(ctx, j.BPTypeID))), when})
	}
	for i := range live {
		mv := &live[i]
		switch {
		case mv.CarrierID == viewer && mv.Status != moveOpen:
			desk.Rows = append(desk.Rows, []string{esp.ShipIcon(mv.BPTypeID, 32),
				igbF3("carry "+esp.IGBText(m.typeNameOr(ctx, mv.BPTypeID))+" to "+esp.IGBText(stationNameOf(ctx, m, mv.To))),
				pg.tint(pg.pal.Amber, esp.ISKShort(mv.Wage)+" on arrival")})
		case mv.Status == moveOpen && m.mayCarry(ctx, mv, viewer):
			desk.Rows = append(desk.Rows, []string{esp.ShipIcon(mv.BPTypeID, 32),
				igbF3(esp.IGBText(m.typeNameOr(ctx, mv.BPTypeID))+" from "+esp.IGBText(stationNameOf(ctx, m, mv.From))+
					" to "+esp.IGBText(stationNameOf(ctx, m, mv.To))) + " " +
					pg.soft(`<a href="`+host+`/corp/join?carry=`+strconv.FormatInt(mv.MoveID, 10)+`">carry this</a>`),
				igbF3(esp.ISKShort(mv.Wage)) + " " + pg.dim(fmt.Sprintf("%dj", mv.Jumps))})
		}
		if len(desk.Rows) >= 10 {
			break
		}
	}
	esp.PanelBand(b, pg.pal, []esp.Panel{you, desk})
}

// rndOrderRows caps the orders panel; the library below lists the rest by
// print.
const rndOrderRows = 25

// writeRndOrders is every research and copy order on the board, one row each:
// the viewer's base first, then the board's own priority. An order's status is
// its print's (rndPrint.Status, worked out for the library), unless members
// already hold runs of it.
func (m *Mod) writeRndOrders(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64, orders []corpWork,
	prints []rndPrint, viewer, viewerBase int64) {
	host := esp.IGBHost()
	factory := m.corpFactoryStation(ctx, corpID)
	printOf := map[int64]*rndPrint{}
	for i := range prints {
		for _, w := range prints[i].Orders {
			printOf[w.WorkID] = &prints[i]
		}
	}
	// Who holds runs of each order (claims taken, jobs installed).
	held := map[int64][]string{}
	mine := map[int64]bool{}
	if len(orders) > 0 {
		ids := make([]int64, 0, len(orders))
		for _, w := range orders {
			ids = append(ids, w.WorkID)
		}
		if rows, err := m.db.QueryContext(ctx, `
			SELECT workID, characterID, SUM(runs) FROM rolebot_corp_work_claims
			 WHERE workID IN (`+placeholders(len(ids))+`) GROUP BY workID, characterID`, int64Args(ids)...); err == nil {
			type claim struct{ work, char, runs int64 }
			var list []claim
			for rows.Next() {
				var c claim
				if rows.Scan(&c.work, &c.char, &c.runs) == nil {
					list = append(list, c)
				}
			}
			rows.Close()
			for _, c := range list {
				held[c.work] = append(held[c.work], fmt.Sprintf("%s (%d)", esp.IGBText(m.charNameOr(ctx, c.char)), c.runs))
				if c.char == viewer {
					mine[c.work] = true
				}
			}
		}
	}
	list := append([]corpWork(nil), orders...)
	sort.SliceStable(list, func(i, j int) bool { // openWorkFor is already in priority order
		a, b := orderStation(list[i], factory), orderStation(list[j], factory)
		ma, mb := viewerBase != 0 && a == viewerBase, viewerBase != 0 && b == viewerBase
		return ma && !mb
	})
	here := 0
	for _, w := range list {
		if viewerBase != 0 && orderStation(w, factory) == viewerBase {
			here++
		}
	}
	note := fmt.Sprintf("%d order(s) &middot; the wage is paid on delivery", len(list))
	if viewerBase != 0 {
		note = fmt.Sprintf("%d order(s), %d at your base &middot; the wage is paid on delivery", len(list), here)
	}
	p := esp.Panel{Title: "Research and copies asked for", Note: note,
		Align: []string{"", "", "right", ""}, Width: []string{"36", "", "120", "220"},
		Empty: "Management has asked for no research or copies right now."}
	for i, w := range list {
		if i == rndOrderRows {
			p.Note = fmt.Sprintf("%d order(s); the first %d shown &middot; every one is in the library below", len(list), rndOrderRows)
			break
		}
		name := esp.IGBText(m.typeNameOr(ctx, w.BPTypeID))
		what, unit := "", "run"
		switch w.Kind {
		case workKindCopy:
			what, unit = fmt.Sprintf("Copy <b>%s</b> &times;%d", name, w.RunsWanted), "copy"
		case workKindME:
			what, unit = "Material research on <b>"+name+"</b>", "level"
		case workKindPE:
			what, unit = "Time research on <b>"+name+"</b>", "level"
		}
		st := orderStation(w, factory)
		where := esp.IGBText(stationNameOf(ctx, m, st))
		detail := where
		if w.Note != "" {
			detail = esp.IGBText(w.Note) + " &middot; " + where
		}
		frac := 0.0
		if w.RunsWanted > 0 {
			frac = float64(w.RunsDone) / float64(w.RunsWanted)
		}
		line := igbF3(what) + "<br>" + esp.Bar(frac, 10, pg.pal.Up) + " " +
			pg.soft(fmt.Sprintf("%d of %d done &middot; %s", w.RunsDone, w.RunsWanted, detail))
		pay := igbF3("<b>"+esp.ISKShort(w.WagePerRun)+"</b>") + pg.dim(" per "+unit) + "<br>" +
			pg.soft(fmt.Sprintf("%d %s(s) left", w.RunsLeft(), unit))
		status := pg.soft("open")
		if pr := printOf[w.WorkID]; pr != nil && pr.Status != "" {
			status = pr.Status
		}
		switch {
		case mine[w.WorkID]:
			status = pg.tint(pg.pal.Green, "yours") + " " + pg.soft("&mdash; you hold runs of it")
		case len(held[w.WorkID]) > 0:
			status = pg.tint(armColor(armResearch), "in hand") + " " + pg.soft(strings.Join(held[w.WorkID], ", "))
		case viewerBase != 0 && st == viewerBase && printOf[w.WorkID] != nil && printOf[w.WorkID].State == rndQueued:
			status = pg.tint(pg.pal.Green, "take it here") + " " +
				pg.soft("&mdash; install it for the corporation from the Blueprint Library")
		}
		shade := ""
		if viewerBase != 0 && st == viewerBase {
			shade = igbShadeMine
		} else if pr := printOf[w.WorkID]; pr != nil && pr.State == rndBlocked {
			shade = igbShadeLoss
		}
		p.Rows = append(p.Rows, []string{esp.ShipIcon(w.BPTypeID, 32), line, pay, status})
		p.Shades = append(p.Shades, shade)
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
	if viewerBase == 0 && viewer != 0 && len(list) > 0 {
		b.WriteString(pg.soft(`You have no Base yet, so none of these is yours to take. `+
			`<a href="`+host+`/corp/join?arm=research">Join R&amp;D</a> and the CEO posts you to a research base.`) + "<br>\n")
	}
}

// rndLibGrid is the library panel's grid: icon, print and base, levels,
// directives, holder.
var (
	rndLibAlign = []string{"", "", "", "", ""}
	rndLibWidth = []string{"36", "", "100", "160", ""}
)

// writeRndLibrary is the library, one row per original, paged.
func (m *Mod) writeRndLibrary(ctx context.Context, b *strings.Builder, pg igbPage, prints []rndPrint, meTarget,
	viewerBase, baseFilter int64, page int) {
	host := esp.IGBHost()
	var list []rndPrint
	for _, p := range prints {
		if baseFilter == 0 || p.StationID == baseFilter {
			list = append(list, p)
		}
	}
	note := fmt.Sprintf("%d original(s): yours first, then the stuck, the working, the moving, the waiting", len(list))
	if baseFilter != 0 {
		note = fmt.Sprintf("%d original(s) at %s &middot; <a href=\"%s/corp/research\">all bases</a>",
			len(list), esp.IGBText(stationNameOf(ctx, m, baseFilter)), host)
	}
	p := esp.Panel{Title: "Every original and whose hands it is in", Note: note, Align: rndLibAlign, Width: rndLibWidth,
		Empty: "The library holds no originals."}
	from := page * rndLibraryPageRows
	if from >= len(list) {
		from, page = 0, 0
	}
	to := min(len(list), from+rndLibraryPageRows)
	for _, pr := range list[from:to] {
		where := "in transit"
		if pr.StationID != 0 {
			where = esp.IGBText(stationNameOf(ctx, m, pr.StationID))
		}
		meColor := pg.pal.Up
		if pr.ME >= meTarget {
			meColor = pg.pal.Green
		}
		levels := igbF3(fmt.Sprintf("ME <font color=%s><b>%d</b></font>/%d", meColor, pr.ME, meTarget)) + "<br>" +
			pg.soft(fmt.Sprintf("PE %d/%d", pr.PE, meTarget/2))
		dirs := pg.soft("nothing asked")
		if d := rndDirectives(pr.Orders); d != "" {
			dirs = igbF3(d)
		}
		shade := ""
		switch {
		case pr.StationID == viewerBase && viewerBase != 0:
			shade = igbShadeMine
		case pr.State == rndBlocked:
			shade = igbShadeLoss
		}
		p.Rows = append(p.Rows, []string{esp.ShipIcon(pr.BPTypeID, 32),
			igbF3(esp.IGBText(m.typeNameOr(ctx, pr.BPTypeID))) + "<br>" + pg.soft(where),
			levels, dirs, pr.Status})
		p.Shades = append(p.Shades, shade)
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
	if len(list) > rndLibraryPageRows {
		var nav []string
		pages := (len(list) + rndLibraryPageRows - 1) / rndLibraryPageRows
		for i := 0; i < pages; i++ {
			label := strconv.Itoa(i + 1)
			if i == page {
				nav = append(nav, "<b>"+label+"</b>")
				continue
			}
			q := "page=" + strconv.Itoa(i)
			if baseFilter != 0 {
				q += "&base=" + strconv.FormatInt(baseFilter, 10)
			}
			nav = append(nav, `<a href="`+host+`/corp/research?`+q+`">`+label+`</a>`)
		}
		b.WriteString(igbF3("Page " + strings.Join(nav, " ")))
	}
}

// writeRndMoves is the prints on the move, and the last few delivered.
func (m *Mod) writeRndMoves(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64, live []printMove, viewer int64) {
	p := esp.Panel{Title: "Between the bases", Note: "originals go with a Director or the CEO; copies with a member posted where they sit",
		Align: []string{"", "", "", "right"}, Width: []string{"36", "", "", "110"},
		Empty: "No print is moving between the corporation's bases."}
	done, _ := m.printMovesWhere(ctx, `WHERE corporationID = ? AND status = 'done' ORDER BY doneAt DESC LIMIT 10`, corpID)
	rows := append(append([]printMove(nil), live...), done...)
	for _, mv := range rows {
		route := igbF3(esp.IGBText(m.typeNameOr(ctx, mv.BPTypeID))+" "+pg.soft("("+mv.Kind+")")) + "<br>" +
			pg.soft(esp.IGBText(stationNameOf(ctx, m, mv.From))+" &rarr; "+esp.IGBText(stationNameOf(ctx, m, mv.To))+
				fmt.Sprintf(" &middot; %d jump(s)", mv.Jumps))
		var who string
		switch mv.Status {
		case moveOpen:
			who = pg.tint(pg.pal.Amber, "waiting for a carrier") + "<br>" + pg.soft(esp.IGBText(mv.Reason))
		case moveClaimed:
			who = igbF3(esp.IGBText(m.charNameOr(ctx, mv.CarrierID))) + "<br>" + pg.soft("on the way to fetch it")
		case moveCarrying:
			who = igbF3(esp.IGBText(m.charNameOr(ctx, mv.CarrierID))) + "<br>" + pg.soft("carrying it")
		case moveDone:
			who = igbF3(esp.IGBText(m.charNameOr(ctx, mv.CarrierID))) + "<br>" + pg.soft("delivered "+igbAgo(mv.DoneAt))
		}
		shade := ""
		if mv.CarrierID == viewer && viewer != 0 {
			shade = igbShadeMine
		}
		p.Rows = append(p.Rows, []string{esp.ShipIcon(mv.BPTypeID, 32), route, who,
			igbF3(esp.ISKShort(mv.Wage)) + " " + pg.dim("ISK")})
		p.Shades = append(p.Shades, shade)
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
}

// writeRndLately is the department's recent ledger: moves, expansions, the
// library plan.
func (m *Mod) writeRndLately(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64) {
	p := esp.Panel{Title: "Lately in R&amp;D", Align: []string{"", ""}, Width: []string{"110", ""}, Empty: "Nothing to report."}
	rows, err := m.db.QueryContext(ctx, `
		SELECT at, detail FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND verb IN ('printmove', 'printmove.done', 'printmove.lost', 'printmove.late',
		                                      'expand.research', 'noexpand', 'library', 'lockdown')
		 ORDER BY at DESC LIMIT ?`, corpID, rndLatelyRows)
	if err == nil {
		for rows.Next() {
			var at int64
			var detail string
			if rows.Scan(&at, &detail) == nil {
				p.Rows = append(p.Rows, []string{pg.dim3(igbAgo(at)), igbF3(esp.IGBText(detail))})
			}
		}
		rows.Close()
	}
	esp.PanelBand(b, pg.pal, []esp.Panel{p})
}
