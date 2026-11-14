package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gurimoon/esp"
	"gurimoon/server/gangsta"
)

// /corp/ops (old address /ig/corpops) â€” Operations, the "what's up" page of
// the Corporate Portal: the fleets in the field and a seat in each, where
// every member is right now, the standing orders in force, the orders
// members have posted, and what happened lately. Any corporation: the fleets
// are the module's, the members are whoever the roster says.
//
// The one thing on it that ACTS is "Request a seat", and it needs no key:
// the identity header only decides whose client the fleet invitation is sent
// to, and a forged header costs the forger an invitation dialog on somebody
// else's screen that they can decline.

// handleIGBCorpOps serves /corp/ops and /ig/corpops.
func (m *Mod) handleIGBCorpOps(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "ops", igbF3("Open this page from your own terminal to see your corporation's operations."))
		return
	}
	q := r.URL.Query()
	banner := ""
	if join, _ := strconv.ParseInt(q.Get("join"), 10, 64); join != 0 {
		g, _ := getGang(ctx, m.db, join)
		switch {
		case g == nil || !m.fleetBelongsTo(ctx, g, corpID):
			banner = "That fleet is not one of your corporation's."
		default:
			if refusal := m.requestSeat(ctx, g, charID, q.Get("role")); refusal != "" {
				banner = refusal
			} else {
				banner = "Your invitation is on its way â€” accept it in your fleet window."
			}
		}
	}
	if kit, _ := strconv.ParseInt(q.Get("kit"), 10, 64); kit != 0 && banner == "" {
		g, _ := getGang(ctx, m.db, kit)
		switch {
		case g == nil || g.CorpID != corpID:
			banner = "That fleet is not one of your corporation's."
		default:
			if refusal := m.drawHumanKit(ctx, g, charID); refusal != "" {
				banner = refusal
			} else {
				banner = "The kit is in your hangar at the Armory's station — the quartermaster has mailed you the details."
			}
		}
	}
	expand, _ := strconv.ParseInt(q.Get("fleet"), 10, 64)
	esp.WriteCorpPortalPage(w, r, "ops", m.opsBody(ctx, r, corpID, charID, banner, expand))
}

// fleetBelongsTo reports whether a module fleet is this corporation's own or
// one it has seconded escorts to.
func (m *Mod) fleetBelongsTo(ctx context.Context, g *Gang, corpID int64) bool {
	if g.CorpID == corpID {
		return true
	}
	// An alliance-mate's fleet is the alliance's (gangalliance.go).
	for _, mate := range m.allianceMemberCorps(ctx, corpID) {
		if g.CorpID == mate {
			return true
		}
	}
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_gang_members WHERE gangID = ? AND recruitCorpID = ?`,
		g.GangID, corpID).Scan(&n)
	return n > 0
}

// corpFleets lists the live module fleets of a corporation â€” its own, then
// any it has seconded escorts to â€” newest first.
func (m *Mod) corpFleets(ctx context.Context, corpID int64) []*Gang {
	ids := map[int64]bool{}
	var order []int64
	collect := func(query string) {
		rows, err := m.db.QueryContext(ctx, query, corpID)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil && !ids[id] {
				ids[id] = true
				order = append(order, id)
			}
		}
	}
	collect(`SELECT gangID FROM rolebot_gangs WHERE corpID = ? AND enabled = 1 ORDER BY createdDate DESC`)
	collect(`SELECT DISTINCT g.gangID FROM rolebot_gangs g
	           JOIN rolebot_gang_members gm ON gm.gangID = g.gangID
	          WHERE gm.recruitCorpID = ? AND g.enabled = 1 ORDER BY g.gangID DESC`)
	// Then the alliance's fleets (gangalliance.go), when the switch is on.
	for _, mate := range m.allianceMemberCorps(ctx, corpID) {
		rows, err := m.db.QueryContext(ctx, `SELECT gangID FROM rolebot_gangs WHERE corpID = ? AND enabled = 1 ORDER BY createdDate DESC`, mate)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil && !ids[id] {
				ids[id] = true
				order = append(order, id)
			}
		}
		rows.Close()
	}
	var out []*Gang
	for _, id := range order {
		if g, err := getGang(ctx, m.db, id); err == nil && g != nil {
			out = append(out, g)
		}
	}
	return out
}

// opsBody is the page: what this page is, the figures by department, the
// fleets with a seat in each, who is doing what by department, where the
// corporation is, the orders in force, and what happened lately.
func (m *Mod) opsBody(ctx context.Context, r *http.Request, corpID, viewer int64, banner string, expand int64) string {
	pg := m.igbPageFor(r)
	pr := m.portalRosterFor(ctx, corpID)
	fleets := pr.Fleets
	commands := m.listCorpCommands(ctx, corpID, 20)
	work, _ := m.openWorkFor(ctx, corpID)
	viewerGang := gangsta.GangOf(viewer)
	host := esp.IGBHost()

	var b strings.Builder
	if banner != "" {
		b.WriteString("<table width=100% bgcolor=" + pg.pal.Panel + "><tr><td>" + pg.accent(esp.IGBText(banner)) + "</td></tr></table><br>\n")
	}
	b.WriteString(pg.soft("What the corporation is doing right now. Fleets are raised by the heads of Security and Mining, "+
		`or by the chair on the <a href="`+host+`/corp/orders">Orders</a> page; <b>Request a seat</b> sends a fleet invitation `+
		`to your client. Which department you work in is chosen on the <a href="`+host+`/corp/">Overview</a>.`) + "<br><br>\n")

	// The figures: who is out, by department.
	stats := []igbStat{{Label: "Fleets out", Value: strconv.Itoa(len(fleets))}}
	for _, a := range []corpArm{armSecurity, armMining, armIndustry, armResearch, armSales, armInfrastructure} {
		on, space := onDuty(pr.ByArm[a])
		stats = append(stats, igbStat{Label: "<font color=" + armColor(a) + ">" + esp.IGBText(armLabel(a)) + "</font>",
			Value: strconv.Itoa(on), Note: fmt.Sprintf("on duty, %d in space", space)})
	}
	pg.statStrip(&b, stats)
	b.WriteString("<br>\n")

	// The fleets, and a seat in each.
	m.writeFleetsPanel(ctx, &b, pg, fleets, pr.Present, viewer, viewerGang, expand)
	b.WriteString("<br>\n")

	// Who is doing what, department by department.
	m.writeDeptActivity(ctx, &b, pg, pr, viewer)
	b.WriteString("<br>\n")

	// Where the corporation is, beside the orders in force; the directive
	// beside the work in hand; the chair's orders.
	m.writeWhereAndOrders(ctx, &b, pg, corpID, pr)
	b.WriteString("<br>\n")
	m.writeDirectiveBand(ctx, &b, pg, corpID, work, commands)
	b.WriteString("<br>\n")
	m.writeCommandsPanel(ctx, &b, pg, commands, viewer)
	b.WriteString("<br>\n")

	// Lately.
	s := m.corpSnapshotFor(ctx, corpID, pr.CEOID)
	recent := esp.Panel{Title: "Lately", Align: []string{"", ""}, Width: []string{"110", ""}, Empty: "Nothing to report."}
	for _, l := range s.Recent {
		recent.Rows = append(recent.Rows, []string{pg.dim3(igbAgo(l.At)), igbF3(esp.IGBText(l.Text))})
	}
	esp.PanelBand(&b, pg.pal, []esp.Panel{recent})
	return b.String()
}

// opsGridCols is the one column grid every band of the fleets section
// shares: icon, icon, name, place, who, figure.
var (
	opsGridAlign = []string{"", "", "", "", "", "right"}
	opsGridWidth = []string{"36", "36", "", "170", "150", "120"}
)

func (m *Mod) writeFleetsPanel(ctx context.Context, b *strings.Builder, pg igbPage, fleets []*Gang,
	present map[int64]esp.Session, viewer, viewerGang, expand int64) {

	list := esp.Panel{Title: "Fleets in the field", Note: "request a seat to fly along",
		Align: opsGridAlign, Width: opsGridWidth,
		Empty: "No fleet is out right now. When Security or Mining musters one it appears here, with a seat to request."}
	var bands [][]esp.Panel
	for _, g := range fleets {
		mems, _ := gangMembers(ctx, m.db, g.GangID)
		byRole := map[string]int{}
		aboard := 0
		for _, mem := range mems {
			byRole[mem.Role]++
			aboard++
		}
		hull, _ := m.shipTypeOf(ctx, g.LeaderCharID)
		fcIcon, fcName := m.pilotCells(ctx, g.LeaderCharID, 32)
		name := igbF3("<b>"+esp.IGBText(g.Name)+"</b>") + "<br>" +
			pg.dim(esp.IGBText(activityLabel(g.Activity))+" &middot; "+rosterSummary(byRole))
		where := pg.tint(fleetPhaseColor(pg, g), gangStageLabel(g))
		if sysID := fleetSiteSystem(g); sysID != 0 {
			where += "<br>" + m.systemCell(ctx, sysID)
		} else if g.State.StagingStationID != 0 {
			where += "<br>" + pg.dim(esp.IGBText(m.stationLabel(ctx, g.State.StagingStationID)))
		}
		who := igbF3(fcName) + "<br>" + pg.dim("commander")
		when := ""
		switch g.State.OpPhase {
		case gangOpMuster:
			when = pg.dim("musters " + igbIn(g.State.MusterDeadline))
		case gangOpWindDown:
			when = pg.dim("settles " + igbIn(g.State.WindDownDeadline))
		default:
			if g.ExpiresAt > 0 {
				when = pg.dim("winds down " + igbIn(g.ExpiresAt))
			}
		}
		seat := ""
		fm := mirrorOf(g.GangID)
		switch {
		case fm != nil && viewerGang != 0 && viewerGang == fm.RealGangID:
			seat = pg.tint(pg.pal.Green, "aboard")
			// The seat's kit, when the Armory has one for this capsuleer
			// (corparmory_human.go).
			if k, why := m.humanKitOffer(ctx, g, viewer); k != nil {
				seat += "<br>" + igbF3(`<a href="`+esp.IGBHost()+`/corp/ops?kit=`+strconv.FormatInt(g.GangID, 10)+
					`">Draw a `+esp.IGBText(m.typeNameOr(ctx, k.HullTypeID))+`</a>`)
			} else if why != "" {
				seat += "<br>" + pg.dim(esp.IGBText(why))
			}
		case g.State.OpPhase == gangOpWindDown || g.State.OpPhase == gangOpSettled:
			seat = pg.dim("standing down")
		default:
			seat = igbF3(`<a href="` + esp.IGBHost() + `/corp/ops?join=` + strconv.FormatInt(g.GangID, 10) + `">Request a seat</a>`)
		}
		roster := igbF3(`<a href="` + esp.IGBHost() + `/corp/ops?fleet=` + strconv.FormatInt(g.GangID, 10) + `">roster</a>`)
		if expand == g.GangID {
			roster = pg.dim("roster")
		}
		list.Rows = append(list.Rows, []string{esp.ShipIcon(hull, 32), fcIcon, name, where, who, seat + "<br>" + roster + "<br>" + when})
		list.Shades = append(list.Shades, fleetShade(g))
		if expand == g.GangID {
			bands = append(bands, []esp.Panel{list})
			bands = append(bands, []esp.Panel{m.fleetRosterPanel(ctx, pg, g, mems, present, viewer)})
			list = esp.Panel{Title: "More fleets", Align: opsGridAlign, Width: opsGridWidth}
		}
	}
	if len(list.Rows) > 0 || len(bands) == 0 {
		bands = append(bands, []esp.Panel{list})
	}
	esp.PanelGrid(b, pg.pal, bands)
}

// fleetRosterPanel is one fleet opened up: everyone aboard with their face,
// hull, seat and where they are.
func (m *Mod) fleetRosterPanel(ctx context.Context, pg igbPage, g *Gang, mems []GangMember,
	present map[int64]esp.Session, viewer int64) esp.Panel {

	p := esp.Panel{Title: esp.IGBText(g.Name), Note: "aboard", Align: opsGridAlign, Width: opsGridWidth,
		Empty: "Nobody aboard."}
	sort.SliceStable(mems, func(i, j int) bool {
		if (mems[i].CharacterID == g.LeaderCharID) != (mems[j].CharacterID == g.LeaderCharID) {
			return mems[i].CharacterID == g.LeaderCharID
		}
		return mems[i].Role < mems[j].Role
	})
	for _, mem := range mems {
		hull, hullName := m.shipTypeOf(ctx, mem.CharacterID)
		icon, name := m.pilotCells(ctx, mem.CharacterID, 32)
		seat := roleLabel(mem.Role)
		if mem.CharacterID == g.LeaderCharID {
			seat = "commander &middot; " + seat
		}
		if !mem.Driven {
			seat += " &middot; capsuleer"
		}
		where, _, _ := m.memberWhere(ctx, mem.CharacterID, present)
		doing := ""
		if mem.Driven {
			if st, err := loadState(ctx, m.db, mem.CharacterID); err == nil && st != nil {
				doing = pg.dim(esp.IGBText(phaseLabel(st)))
			}
		}
		shade := ""
		if mem.CharacterID == viewer {
			shade = igbShadeMine
		}
		p.Rows = append(p.Rows, []string{esp.ShipIcon(hull, 32), icon,
			igbF3(name) + "<br>" + pg.dim(esp.IGBText(hullName)),
			igbF3(where), roleIcon(mem.Role) + " " + igbF3(seat), doing})
		p.Shades = append(p.Shades, shade)
	}
	return p
}

// deptActivityRows is how many present members a department lists before
// "and N more": enough to read the department, short enough to scroll past.
const deptActivityRows = 10

// opsDeptAlign / opsDeptWidth is the one grid every department band shares:
// hull, face, who, doing, where, fleet.
var (
	opsDeptAlign = []string{"", "", "", "", "", ""}
	opsDeptWidth = []string{"36", "36", "", "", "170", "130"}
)

// writeDeptActivity is who is doing what: one band per department, present
// members only, each with their hull, face, rank, task, place and fleet. The
// viewer's own row, and only that row, is shaded.
func (m *Mod) writeDeptActivity(ctx context.Context, b *strings.Builder, pg igbPage, pr *portalRoster, viewer int64) {
	esp.Section(b, pg.pal, "Who is doing what")
	var bands [][]esp.Panel
	for _, a := range portalArmOrder {
		ms := pr.ByArm[a]
		if len(ms) == 0 {
			continue
		}
		on, _ := onDuty(ms)
		p := esp.Panel{Title: armIcon(a, pr.CorpID, 24) + " " + armName(a),
			Note:  fmt.Sprintf("%d on duty &middot; %d off duty", on, len(ms)-on),
			Align: opsDeptAlign, Width: opsDeptWidth,
			Empty: "Nobody from " + esp.IGBText(armLabel(a)) + " is on duty."}
		if a == "" {
			p.Empty = "Nobody outside a department is on duty."
		}
		shown := 0
		for _, pm := range ms {
			if !pm.Present {
				continue
			}
			if shown == deptActivityRows {
				p.Rows = append(p.Rows, []string{"", "", pg.soft(fmt.Sprintf("&hellip; and %d more on duty", on-shown)), "", "", ""})
				p.Shades = append(p.Shades, "")
				break
			}
			shown++
			p.Rows = append(p.Rows, m.deptActivityRow(ctx, pg, a, pm))
			shade := ""
			if pm.CharID == viewer {
				shade = igbShadeMine
			}
			p.Shades = append(p.Shades, shade)
		}
		bands = append(bands, []esp.Panel{p})
	}
	if len(bands) == 0 {
		bands = [][]esp.Panel{{{Title: "Members", Align: opsDeptAlign, Width: opsDeptWidth, Empty: "Nobody is about right now."}}}
	}
	esp.PanelGrid(b, pg.pal, bands)
}

// deptActivityRow is one present member: hull, face, who, doing, where, fleet.
func (m *Mod) deptActivityRow(ctx context.Context, pg igbPage, a corpArm, pm portalMember) []string {
	hull, hullName := m.shipTypeOf(ctx, pm.CharID)
	who := igbF3(esp.PilotLink(pm.CharID, esp.IGBText(pm.Name)))
	if pm.Tier == armTierHead {
		who += " " + pg.tint(armColor(a), "head")
	}
	switch {
	case pm.Title != "":
		who += "<br>" + pg.soft(esp.IGBText(pm.Title))
	case !pm.OnRoster:
		who += "<br>" + pg.soft("capsuleer")
	}
	doing := colored(armColor(a), esp.IGBText(m.memberDoing(ctx, pm)))
	where := m.systemCell(ctx, pm.Session.SolarSystemID)
	if pm.Session.StationID != 0 {
		where += "<br>" + pg.soft("docked")
	} else {
		doing += "<br>" + pg.soft(esp.IGBText(hullName))
		where += "<br>" + pg.soft("in space")
	}
	fleet := pg.soft("&mdash;")
	if pm.Fleet != "" {
		fleet = igbF3(esp.IGBText(pm.Fleet))
	}
	return []string{esp.ShipIcon(hull, 32), esp.Portrait(pm.CharID, 32), who, doing, where, fleet}
}

// whereRows is the systems the corporation is in, busiest first. Pure.
func whereRows(present map[int64]esp.Session) (systems []int64, count map[int64]int) {
	count = map[int64]int{}
	for _, s := range present {
		if s.SolarSystemID != 0 {
			count[s.SolarSystemID]++
		}
	}
	for id := range count {
		systems = append(systems, id)
	}
	sort.Slice(systems, func(i, j int) bool {
		if count[systems[i]] != count[systems[j]] {
			return count[systems[i]] > count[systems[j]]
		}
		return systems[i] < systems[j]
	})
	return systems, count
}

// writeWhereAndOrders is where the corporation is (a tally by system) beside
// the department heads' standing fleet orders.
func (m *Mod) writeWhereAndOrders(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64, pr *portalRoster) {
	const maxSystems = 10
	systems, count := whereRows(pr.Present)
	where := esp.Panel{Title: "Where we are", Note: fmt.Sprintf("%d present in %d system(s)", len(pr.Present), len(systems)),
		Align: []string{"", "", "right"}, Width: []string{"", "110", "40"}, Empty: "Nobody is about right now."}
	top := 1
	if len(systems) > 0 {
		top = count[systems[0]]
	}
	for i, sysID := range systems {
		if i == maxSystems {
			where.Rows = append(where.Rows, []string{pg.soft(fmt.Sprintf("&hellip; and %d more", len(systems)-i)), "", ""})
			break
		}
		where.Rows = append(where.Rows, []string{m.systemCell(ctx, sysID),
			esp.Bar(float64(count[sysID])/float64(top), 12, pg.pal.Accent), igbF3(strconv.Itoa(count[sysID]))})
	}
	orders := esp.Panel{Title: "Standing orders", Note: "from the department heads",
		Align: []string{"", ""}, Width: []string{"36", ""},
		Empty: "No standing orders &mdash; fleets pick their own ground."}
	for _, o := range m.listFleetOrders(ctx, corpID) {
		text := fleetOrderText(o.Dept, m.nameOfSystem(ctx, o.SystemID), strings.Join(o.OreNames, ", "), o.Reason)
		orders.Rows = append(orders.Rows, []string{roleIcon(fleetOrderRole(o.Dept)),
			igbF3(esp.IGBText(text)) + "<br>" + pg.soft("until "+igbWhen(o.ExpiresAt))})
	}
	esp.PanelBand(b, pg.pal, []esp.Panel{where, orders})
}

// writeDirectiveBand is the chair's directive beside the work in hand.
func (m *Mod) writeDirectiveBand(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64, work []corpWork, commands []corpCommand) {
	focus := esp.Panel{Title: "The directive", Note: "where the chair wants the effort",
		Align: []string{"", "", "right"}, Width: []string{"110", "", "50"}, Empty: "No standing directive."}
	if d, _, err := m.loadCorpDirective(ctx, corpID); err == nil && len(d) > 0 {
		for _, k := range directiveOrder(d) {
			focus.Rows = append(focus.Rows, []string{igbF3(esp.IGBText(k)), esp.Bar(d[k], 16, pg.pal.Accent),
				igbF3(fmt.Sprintf("%.0f%%", 100*d[k]))})
		}
	}
	var postings int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_corp_postings WHERE corporationID = ?`, corpID).Scan(&postings)
	openCmds := 0
	for _, c := range commands {
		if c.Open() {
			openCmds++
		}
	}
	hand := esp.Panel{Title: "In hand", Align: []string{"", ""}, Width: []string{"110", ""}}
	hand.Rows = append(hand.Rows,
		[]string{pg.dim3("Work"), igbF3(fmt.Sprintf(`%d paid order(s) on the <a href="%s/corp/industry">Industry</a> page`, len(work), esp.IGBHost()))},
		[]string{pg.dim3("Orders"), igbF3(fmt.Sprintf("%d order(s) from the chair open", openCmds))},
		[]string{pg.dim3("Postings"), igbF3(fmt.Sprintf("%d member(s) posted to offices", postings))})
	esp.PanelBand(b, pg.pal, []esp.Panel{focus, hand})
}

// writeCommandsPanel is what members with a chair have ordered, and what
// became of it.
func (m *Mod) writeCommandsPanel(ctx context.Context, b *strings.Builder, pg igbPage, commands []corpCommand, viewer int64) {
	p := esp.Panel{Title: "Orders from the chair", Note: "posted by directors on the Orders page",
		Align: []string{"", "", "", ""}, Width: []string{"30", "", "150", "170"},
		Empty: "No orders have been posted."}
	for _, c := range commands {
		icon, name := m.pilotCells(ctx, c.PosterID, 24)
		what := igbF3("<b>"+esp.IGBText(c.Title(m, ctx))+"</b>") + "<br>" + pg.dim(esp.IGBText(c.Summary(m, ctx)))
		status := pg.tint(commandColor(pg, c.Status), c.Status)
		if c.ClaimedBy != 0 {
			status += "<br>" + pg.dim("by "+esp.IGBText(m.charNameOr(ctx, c.ClaimedBy)))
		}
		result := pg.dim(esp.IGBText(c.Result))
		if c.Result == "" {
			result = pg.dim("posted " + igbAgo(c.PostedAt))
		}
		shade := commandShade(c.Status)
		if c.PosterID == viewer && shade == "" {
			shade = igbShadeMine
		}
		p.Rows = append(p.Rows, []string{icon, what + "<br>" + pg.dim(esp.IGBText(name)+", "+c.PosterHat), status, result})
		p.Shades = append(p.Shades, shade)
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
}

// â”€â”€ small helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func rosterSummary(byRole map[string]int) string {
	var parts []string
	for _, role := range gangRoles {
		if n := byRole[role]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, roleLabel(role)))
		}
	}
	if len(parts) == 0 {
		return "nobody aboard"
	}
	return strings.Join(parts, ", ")
}

func fleetSiteSystem(g *Gang) int64 {
	switch g.Activity {
	case ActMining, ActIndMining:
		return g.State.SystemID
	case ActCombat, ActSecurity:
		return g.State.HuntSystemID
	case ActPiracy:
		return g.State.PirSystemID
	}
	return 0
}

func fleetPhaseColor(pg igbPage, g *Gang) string {
	switch g.State.OpPhase {
	case gangOpMuster:
		return pg.pal.Amber
	case gangOpWindDown, gangOpSettled:
		return pg.pal.Dim
	}
	return pg.pal.Green
}

func commandColor(pg igbPage, status string) string {
	switch status {
	case cmdLive, cmdDone:
		return pg.pal.Green
	case cmdClaimed, cmdPending:
		return pg.pal.Amber
	case cmdRefused, cmdExpired:
		return pg.pal.Red
	}
	return pg.pal.Dim
}

func commandShade(status string) string {
	switch status {
	case cmdLive:
		return igbShadeLive
	case cmdClaimed, cmdPending:
		return igbShadeMuster
	case cmdRefused, cmdExpired:
		return igbShadeLoss
	}
	return ""
}
