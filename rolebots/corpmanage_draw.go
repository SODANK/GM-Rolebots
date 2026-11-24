package rolebots

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gurimoon/esp"
)

// How the Management page looks. The desk opens on cards, one per division, with the head's face, how
// many are on duty and what it is doing; a rail of division icons runs across the top of every view.
// A division's view is its masthead, its orders in hand, a board of order tiles of which one opens at
// a time, and the things an order is usually about as tables whose rows open the right form already
// filled in: what the factory is short of, the prints on the shelves, the belts in trouble.
// The in-game browser draws flat tables only, with no form inside a cell, so an open form sits under
// a coloured strip of its own rather than in its tile.

// ── icons ───────────────────────────────────────────────────────────────────

// uiTile is a client interface icon as a form's tile.
func uiTile(id string) func(int64) string {
	return func(int64) string { return esp.UIIcon(id, 32, "") }
}

// typeTile is an item's icon as a form's tile.
func typeTile(typeID int64) func(int64) string {
	return func(int64) string { return esp.ShipIcon(typeID, 32) }
}

func logoTile(corpID int64) string { return esp.CorpLogo(corpID, 32) }

func fleetTileIcon(int64) string { return esp.ShipIcon(622, 32) } // Stabber

// ── the frame ───────────────────────────────────────────────────────────────

// body is the whole page. top is what the request came to, under the rail: a banner, or a preview
// waiting to be confirmed.
func (p *mgPage) body(top string) string {
	var b strings.Builder
	p.writeRail(&b)
	b.WriteString("<br>\n" + top)
	if !p.botCEO {
		_, ok := humanChaired(p.corpID)
		if !ok && deskDue(p.corpID, "chair.index", 60) {
			// The index is rebuilt every few minutes; a corporation that has just taken its first bot
			// need not wait for it.
			p.m.refreshHumanChairIndex(p.ctx)
			_, ok = humanChaired(p.corpID)
		}
		if !ok {
			b.WriteString(p.pg.ordersBanner(p.idleDeskReason(), true))
		}
	}
	switch {
	case p.div == mgCorp:
		p.writeCorp(&b)
	case p.div == mgHeads:
		p.writeHeads(&b)
	case p.div == mgPeople:
		p.writePeople(&b, "")
	case p.arm() != "":
		p.writeDivision(&b, p.arm())
	default:
		p.writeHome(&b)
	}
	return b.String()
}

// idleDeskReason is why nobody works this corporation's orders: no active bot on the books, or a CEO
// that is a bot nobody plays.
func (p *mgPage) idleDeskReason() string {
	var bots int64
	err := p.m.db.QueryRowContext(p.ctx, `
		SELECT COUNT(*) FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID AND b.enabled = 1
		 WHERE ch.corporationID = ?`, p.corpID).Scan(&bots)
	switch {
	case err != nil:
		return "Nobody will carry these orders out yet."
	case bots == 0:
		return "No active bot flies for this corporation, so nobody will carry these orders out yet."
	case p.ceoID != 0 && !p.m.seatedByPerson(p.ctx, p.ceoID):
		return esp.IGBText(p.m.charNameOr(p.ctx, p.ceoID)) +
			", the CEO, is a bot that does not run this corporation, so nobody will carry these orders out yet."
	}
	return "The corporation's bots have not taken up these orders yet. Try again in a minute."
}

// railCell is one stop of the rail: an icon over a label, lit when it is the page shown.
func (p *mgPage) railCell(div, icon, label, color string) string {
	bg := p.pg.pal.Header
	text := "<font size=3 color=" + color + ">" + label + "</font>"
	if p.div == div {
		bg = igbShadeMine
		text = "<font size=3 color=" + p.pg.pal.Title + "><b>" + label + "</b></font>"
	}
	return "<td align=center valign=middle bgcolor=" + bg + ">" + p.link(div, icon) + "<br>" + p.link(div, text) + "</td>"
}

// writeRail is the way round: the desk, the divisions commanded, and for the CEO the corporation's
// own pages. A head with one division has nowhere else to go and gets no rail.
func (p *mgPage) writeRail(b *strings.Builder) {
	divs := p.scope.Divisions()
	if !p.scope.CEO && len(divs) < 2 {
		return
	}
	var cells []string
	if p.scope.CEO {
		cells = append(cells, p.railCell("", esp.CorpLogo(p.corpID, 32), "Desk", p.pg.pal.Title))
	}
	for _, a := range divs {
		label := armLabel(a)
		switch a {
		case armResearch:
			label = "R&amp;D"
		case armInfrastructure:
			label = "Starbases"
		case armHR:
			label = "Personnel"
		}
		cells = append(cells, p.railCell(string(a), armIcon(a, p.corpID, 32), label, armColor(a)))
	}
	if p.scope.CEO {
		cells = append(cells, p.railCell(mgCorp, esp.UIIcon("07_12", 32, "The corporation"), "Corporation", p.pg.pal.Title))
		if !p.botCEO {
			cells = append(cells, p.railCell(mgHeads, esp.UIIcon("12_01", 32, "Appointments"), "Heads", p.pg.pal.Title))
		}
		cells = append(cells, p.railCell(mgPeople, esp.Portrait(p.charID, 32), "Characters", p.pg.pal.Title))
	}
	b.WriteString("<table width=100%><tr>" + strings.Join(cells, "") + "</tr></table>\n")
}

// masthead is a coloured band naming what the page commands, with a face on its right.
func (p *mgPage) masthead(b *strings.Builder, icon, color, title, line string, faceID int64, faceLine string) {
	b.WriteString("<table width=100% bgcolor=" + p.pg.pal.Panel + "><tr><td width=60 valign=middle>" + icon + "</td>" +
		"<td valign=middle><font size=6 color=" + color + "><b>" + title + "</b></font><br>" + p.pg.soft(line) + "</td>")
	if faceLine != "" {
		b.WriteString("<td valign=middle align=right>" + faceLine + "</td>")
	}
	if faceID != 0 {
		b.WriteString("<td width=72 valign=middle>" + esp.Portrait(faceID, 64) + "</td>")
	}
	b.WriteString("</tr></table>\n")
}

// ── the CEO's desk ──────────────────────────────────────────────────────────

func (p *mgPage) writeHome(b *strings.Builder) {
	ctx, m := p.ctx, p.m
	pr := m.portalRosterFor(ctx, p.corpID)
	heads := m.corpHeads(ctx, p.corpID)
	cmds := m.listCorpCommands(ctx, p.corpID, 200)
	open := map[corpArm]int{}
	total := 0
	for _, c := range cmds {
		if c.Open() && c.Kind != cmdKindChat {
			open[corpArm(c.Arm)]++
			total++
		}
	}
	work, _ := m.openWorkFor(ctx, p.corpID)
	mfg, rnd := m.corpJobCounts(ctx, p.corpID)
	present, inSpace := onDuty(pr.Members)
	p.masthead(b, esp.CorpLogo(p.corpID, 48), p.pg.pal.Title, "Your desk", "Every division answers to you. Your orders stand over a head's.",
		p.charID, p.pg.accent(esp.IGBText(m.charNameOr(ctx, p.charID)))+"<br>"+p.pg.soft("Chief Executive Officer"))
	p.pg.statStrip(b, []igbStat{
		{Label: "On duty", Value: strconv.Itoa(present), Note: fmt.Sprintf("of %d &middot; %d in space", len(pr.Members), inSpace)},
		{Label: "Orders open", Value: strconv.Itoa(total)},
		{Label: "On the boards", Value: strconv.Itoa(len(work)), Note: "work orders"},
		{Label: "Jobs running", Value: strconv.Itoa(mfg + rnd)},
		{Label: "Free to spend", Value: esp.ISKShort(m.chairFree(ctx, p.corpID)), Note: "ISK above the reserve"},
	})
	b.WriteString("<br>\n")

	facts := m.deptFacts(ctx, p.corpID, pr, work, mfg, rnd, m.corpSnapshotFor(ctx, p.corpID, pr.CEOID))
	esp.Section(b, p.pg.pal, "Divisions")
	var bands [][]esp.Panel
	var band []esp.Panel
	for _, a := range armOrder {
		band = append(band, p.divisionCard(a, pr, heads, facts[a], open[a]))
		if len(band) == 2 {
			bands = append(bands, band)
			band = nil
		}
	}
	if len(band) > 0 {
		bands = append(bands, band)
	}
	esp.PanelGrid(b, p.pg.pal, bands)
	b.WriteString("<br>\n")
	p.writeOrders(b, func(corpCommand) bool { return true })
}

// headOf is who heads a division: the appointment, else whoever the corporation's posts say.
func (p *mgPage) headOf(a corpArm, pr *portalRoster, heads map[corpArm]corpHead) int64 {
	if h, ok := heads[a]; ok && !p.botCEO {
		return h.CharID
	}
	if h, ok := armHeadOf(pr.ByArm[a]); ok {
		return h.CharID
	}
	return 0
}

// divisionCard is one division on the desk: the head's face beside what the division is, has and
// does. Every card has the same rows, so two side by side line up.
func (p *mgPage) divisionCard(a corpArm, pr *portalRoster, heads map[corpArm]corpHead, fact deptFact, open int) esp.Panel {
	ms := pr.ByArm[a]
	present, _ := onDuty(ms)
	frac := 0.0
	if len(ms) > 0 {
		frac = float64(present) / float64(len(ms))
	}
	face, head := armIcon(a, p.corpID, 48), p.pg.soft("nobody heads it")
	if id := p.headOf(a, pr, heads); id != 0 {
		face = esp.Portrait(id, 64)
		head = igbF3("<b>"+esp.IGBText(p.m.charNameOr(p.ctx, id))+"</b>") + " " + p.pg.dim(p.kindOf(id)) + p.headActions(a, heads)
	} else if p.scope.CEO && !p.botCEO {
		head += " " + igbF3(p.link(mgHeads, "appoint", "o", "head", "pre", "head", "pre_arm", string(a)))
	}
	note := ""
	if open > 0 {
		note = fmt.Sprintf("%d order(s) open", open)
	}
	detail := head + "<br>" + esp.Bar(frac, 14, armColor(a)) + " " + p.pg.soft(fmt.Sprintf("%d of %d on duty", present, len(ms))) +
		"<br>" + p.pg.soft(esp.IGBText(fact.Doing)) + "<br>" + igbF3(p.link(string(a), "<b>Give orders &raquo;</b>"))
	return esp.Panel{Title: armIcon(a, p.corpID, 24) + " " + armName(a), Note: note,
		Align: []string{"", ""}, Width: []string{"72", ""}, Rows: [][]string{{face, detail}}}
}

// headActions is the CEO's way to swap or stand down a division's head, drawn beside the head. "change"
// opens the appointment for the division (naming another Director replaces the head); "stand down" files
// the same order the Heads page does and previews it first. Only a human CEO appoints heads (a bot CEO
// deals its own), and only an appointment can be stood down: a head the corporation's posts name is
// not one. Empty for anybody else. Pure apart from the page's own state.
func (p *mgPage) headActions(a corpArm, heads map[corpArm]corpHead) string {
	if !p.scope.CEO || p.botCEO {
		return ""
	}
	s := " " + igbF3(p.link(mgHeads, "change", "o", "head", "pre", "head", "pre_arm", string(a)))
	if _, ok := heads[a]; ok {
		s += " " + igbF3(p.link(mgHeads, "stand down", "form", "head", "arm", string(a), "member", ""))
	}
	return s
}

// kindOf says whether a member is a bot or a person.
func (p *mgPage) kindOf(charID int64) string {
	if p.m.botFlown(charID) {
		return "bot"
	}
	return "capsuleer"
}

// ── a division ──────────────────────────────────────────────────────────────

func (p *mgPage) writeDivision(b *strings.Builder, a corpArm) {
	ctx, m := p.ctx, p.m
	pr := p.people()
	heads := m.corpHeads(ctx, p.corpID)
	present, inSpace := onDuty(pr.ByArm[a])
	open := 0
	for _, c := range m.listCorpCommands(ctx, p.corpID, 200) {
		if c.Open() && c.Kind != cmdKindChat && corpArm(c.Arm) == a {
			open++
		}
	}
	headID := p.headOf(a, pr, heads)
	faceLine := p.pg.soft("nobody heads it")
	switch {
	case headID == p.charID:
		faceLine = p.pg.accent("You head it") + "<br>" + p.pg.soft("the CEO may overrule you")
	case headID != 0:
		faceLine = p.pg.accent(esp.IGBText(m.charNameOr(ctx, headID))) + "<br>" + p.pg.soft("heads it &middot; "+p.kindOf(headID)) +
			p.headActions(a, heads)
	}
	line := armPurposeLine(a)
	if path := armPagePath(a); path != "" {
		line += ` <a href="` + esp.IGBHost() + path + `">Division page</a>`
	}
	p.masthead(b, armIcon(a, p.corpID, 48), armColor(a), esp.IGBText(armLabel(a)), line, headID, faceLine)
	stats := []igbStat{
		{Label: "Members", Value: strconv.Itoa(len(pr.ByArm[a])), Note: fmt.Sprintf("%d on duty &middot; %d in space", present, inSpace)},
		{Label: "Orders open", Value: strconv.Itoa(open)},
		{Label: "Free to spend", Value: esp.ISKShort(m.chairFree(ctx, p.corpID)), Note: "ISK above the reserve"},
	}
	if a == armSecurity {
		stats = append(stats, igbStat{Label: "Fleets out", Value: strconv.Itoa(len(armFleets(pr, a)))})
	}
	p.pg.statStrip(b, stats)
	b.WriteString("<br>\n")

	if a == armSecurity {
		p.writeFleets(b, a)
	}
	p.writeTiles(b, a)
	p.writeOrders(b, func(c corpCommand) bool { return corpArm(c.Arm) == a })
	b.WriteString("<br>\n")
	switch a {
	case armIndustry:
		p.writeShortage(b, a)
		p.writeBuildShelf(b)
	case armMining:
		p.writeShortage(b, a)
	case armResearch:
		p.writeResearchShelf(b)
	case armSecurity:
		p.writeBelts(b)
	}
	m.writeArmRequests(ctx, b, p.pg, p.corpID, a)
	p.writePeople(b, a)
}

// armPurposeLine is a division in a sentence, for its masthead. Pure.
func armPurposeLine(a corpArm) string {
	switch a {
	case armIndustry:
		return "Builds what the corporation uses and sells."
	case armResearch:
		return "Researches the originals and copies them for the builders."
	case armMining:
		return "Digs what the factory is short of."
	case armSecurity:
		return "Hunts, patrols and answers for the members."
	case armSales:
		return "Buys, hauls and sells on the corporation's money."
	case armInfrastructure:
		return "Keeps the starbases standing and the goods moving."
	case armHR:
		return "Brings people in and looks after them."
	case armFinance:
		return "Minds the wallet."
	}
	return ""
}

// ── the order tiles ─────────────────────────────────────────────────────────

const mgTilesAcross = 3

// openForm is the form the page shows open: the one clicked, or the one being answered.
func (p *mgPage) openForm(a corpArm) *mgForm {
	for _, want := range []string{p.q.Get("o"), p.q.Get("form")} {
		if want == "" {
			continue
		}
		if f := manageFormFor(p.scope, want, a); f != nil {
			return f
		}
	}
	return nil
}

// writeTiles is the orders a page gives as tiles, and the open one under them. A preview waiting to be
// confirmed is above already, so its form is not drawn twice.
func (p *mgPage) writeTiles(b *strings.Builder, a corpArm) {
	var forms []*mgForm
	for i := range manageForms {
		f := &manageForms[i]
		if !f.on(a) || (f.CEOOnly && !p.scope.CEO) {
			continue
		}
		forms = append(forms, f)
	}
	if len(forms) == 0 {
		return
	}
	open := p.openForm(a)
	color := p.pg.pal.Accent
	if a != "" {
		color = armColor(a)
	}
	title := "Give an order"
	if a == "" {
		title = "Settings"
	}
	esp.Section(b, p.pg.pal, title)
	b.WriteString("<table width=100%>\n")
	for i := 0; i < len(forms); i += mgTilesAcross {
		b.WriteString("<tr>")
		for j := i; j < i+mgTilesAcross; j++ {
			if j > i {
				b.WriteString("<td width=6></td>")
			}
			if j >= len(forms) {
				b.WriteString("<td width=40></td><td></td>")
				continue
			}
			f := forms[j]
			bg, name := p.pg.pal.RowB, "<font size=3 color="+color+"><b>"+esp.IGBText(f.Title)+"</b></font>"
			if open == f {
				bg, name = igbShadeMine, "<font size=3 color="+p.pg.pal.Title+"><b>"+esp.IGBText(f.Title)+"</b></font>"
			}
			icon := ""
			if f.Icon != nil {
				icon = p.link(p.div, f.Icon(p.corpID), "o", f.Form)
			}
			b.WriteString("<td width=40 valign=middle align=center bgcolor=" + bg + ">" + icon + "</td><td valign=middle bgcolor=" + bg + ">" +
				p.link(p.div, name, "o", f.Form) + "<br>" + p.pg.dim3(esp.IGBText(f.Short)) + "</td>")
		}
		b.WriteString("</tr>\n<tr><td><font size=1>&nbsp;</font></td></tr>\n")
	}
	b.WriteString("</table>\n")
	if open != nil && !p.previewing {
		p.openStrip(b, color, open.Title, open.Note)
		open.Write(p, b, a)
	}
}

// openStrip heads an open form.
func (p *mgPage) openStrip(b *strings.Builder, color, title, note string) {
	b.WriteString("<table width=100% bgcolor=" + p.pg.pal.Panel + "><tr><td><font size=5 color=" + color + "><b>" + esp.IGBText(title) + "</b></font><br>" +
		p.pg.soft(note) + "</td><td width=70 align=right valign=top>" + igbF3(p.link(p.div, "close")) + "</td></tr></table><br>\n")
}

// ── orders in hand ──────────────────────────────────────────────────────────

// commandIcon is what an order is about, as a picture.
func (p *mgPage) commandIcon(c corpCommand) string {
	if w, ok := c.work(); ok {
		return esp.ShipIcon(w.ProductTypeID, 32)
	}
	if x, ok := c.board(); ok {
		switch {
		case x.TypeID != 0:
			return esp.ShipIcon(x.TypeID, 32)
		case x.CharID != 0:
			return esp.Portrait(x.CharID, 32)
		}
	}
	if h, ok := c.head(); ok && h.CharID != 0 {
		return esp.Portrait(h.CharID, 32)
	}
	for _, id := range []func() int64{
		func() int64 { x, _ := c.arm(); return x.CharID },
		func() int64 { x, _ := c.posting(); return x.CharID },
		func() int64 { x, _ := c.seat(); return x.CharID },
	} {
		if v := id(); v != 0 {
			return esp.Portrait(v, 32)
		}
	}
	for i := range manageForms {
		if f := &manageForms[i]; f.Form == c.Kind && f.Icon != nil {
			return f.Icon(p.corpID)
		}
	}
	return esp.CorpLogo(p.corpID, 32)
}

// writeOrders is the orders in hand, with a way to withdraw each the viewer may, and what became of
// the last day's.
func (p *mgPage) writeOrders(b *strings.Builder, mine func(corpCommand) bool) {
	t := esp.Panel{Title: "Orders in hand", Note: "and what became of the last day's", Align: []string{"", "", "", "right"}, Width: []string{"40", "", "240", "80"},
		Empty: "Nothing is on order."}
	now := clock.Unix()
	for _, c := range p.m.listCorpCommands(p.ctx, p.corpID, 120) {
		if !mine(c) || len(t.Rows) >= 25 || (!c.Open() && now-c.ClosedAt > 86400) || c.Kind == cmdKindChat {
			continue
		}
		by := esp.IGBText(p.m.charNameOr(p.ctx, c.PosterID)) + ", " + esp.IGBText(c.PosterHat)
		if c.Arm != "" && p.arm() == "" {
			by = "<font color=" + armColor(corpArm(c.Arm)) + ">" + esp.IGBText(armLabel(corpArm(c.Arm))) + "</font> &middot; " + by
		}
		what := igbF3("<b>"+esp.IGBText(c.Title(p.m, p.ctx))+"</b>") + " " + p.pg.soft(esp.IGBText(c.Summary(p.m, p.ctx))) + "<br>" + p.pg.dim(by)
		status := p.pg.tint(commandColor(p.pg, c.Status), commandWords(c.Status))
		if done, wanted := p.commandProgress(c); wanted > 0 {
			status += " " + esp.Bar(float64(done)/float64(wanted), 10, p.pg.pal.Green) + " " + p.pg.soft(fmtUnits(done)+" of "+fmtUnits(wanted))
		}
		// A work order's own line only counts, and the bar has.
		if c.Result != "" && !(c.Kind == cmdKindWork && c.Status == cmdLive) {
			status += "<br>" + p.pg.soft(esp.IGBText(c.Result))
		}
		act := ""
		switch {
		case !c.Open():
		case mayCancelCommand(p.scope, c):
			act = igbF3(p.link(p.div, "withdraw", "form", "cancel", "id", strconv.FormatInt(c.CommandID, 10)))
		case c.PosterCEO:
			act = p.pg.dim("the CEO's")
		}
		t.Rows = append(t.Rows, []string{p.commandIcon(c), what, status, act})
		t.Shades = append(t.Shades, commandShade(c.Status))
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
}

// commandWords is a status as the page says it. Pure.
func commandWords(status string) string {
	switch status {
	case cmdPending:
		return "waiting for the desk"
	case cmdClaimed:
		return "in hand"
	case cmdLive:
		return "under way"
	}
	return status
}

// commandProgress is how far a board order has got, zeros for one that is not counted.
func (p *mgPage) commandProgress(c corpCommand) (done, wanted int64) {
	if c.RefID == 0 || !c.Open() {
		return 0, 0
	}
	switch c.Kind {
	case cmdKindWork:
		_ = p.m.db.QueryRowContext(p.ctx, `SELECT runsDone, runsWanted FROM rolebot_corp_work WHERE workID = ?`, c.RefID).Scan(&done, &wanted)
	case cmdKindReq, cmdKindTrade:
		_ = p.m.db.QueryRowContext(p.ctx, `SELECT qtyDone, qty FROM rolebot_corp_requisition WHERE reqID = ?`, c.RefID).Scan(&done, &wanted)
	case cmdKindAssign:
		_ = p.m.db.QueryRowContext(p.ctx, `SELECT progress, qty FROM rolebot_corp_assignments WHERE assignmentID = ?`, c.RefID).Scan(&done, &wanted)
	}
	return done, wanted
}

// ── what orders are about ───────────────────────────────────────────────────

const mgShelfRows = 24

// writeShortage is what the factory is short of for the work on its board, with the orders that mend
// it a click away: minerals to buy or requisition for Industry, ores to dig or buy for Mining.
func (p *mgPage) writeShortage(b *strings.Builder, a corpArm) {
	ctx, m := p.ctx, p.m
	st, office := m.corpFactoryOffice(ctx, p.corpID)
	if office == 0 {
		return
	}
	short, err := m.corpMaterialShortfall(ctx, p.corpID, st, office)
	if err != nil {
		return
	}
	t := esp.Panel{Title: "Short at " + esp.IGBText(m.stationLabel(ctx, st)), Note: "for the work on the board",
		Align: []string{"", "", "right", "right"}, Width: []string{"36", "", "140", "220"}, Empty: "The factory has what it needs."}
	type row struct {
		id, units int64
	}
	var rows []row
	if a == armMining {
		pol, _ := m.loadCorpPolicy(ctx, p.corpID)
		yield := m.corpRefineYield(ctx, p.corpID, pol)
		if yield <= 0 {
			yield = 0.5
		}
		if len(short) > 0 {
			for _, o := range m.oreBidPicks(ctx, st, short, yield) {
				rows = append(rows, row{o.typeID, o.units})
			}
		}
		t.Title = "Ores the factory wants"
	} else {
		for id, n := range short {
			rows = append(rows, row{id, n})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].units != rows[j].units {
				return rows[i].units > rows[j].units
			}
			return rows[i].id < rows[j].id
		})
	}
	sts := strconv.FormatInt(st, 10)
	for i, r := range rows {
		if i >= 12 {
			break
		}
		name, qty := m.typeNameOr(ctx, r.id), strconv.FormatInt(r.units, 10)
		acts := []string{p.link(p.div, "buy", "o", "bid", "pre", "bid", "pre_item", name, "pre_qty", qty, "pre_station", sts)}
		if a == armMining {
			acts = append([]string{p.link(p.div, "dig", "o", "standing", "pre", "standing", "pre_ore", name),
				p.link(p.div, "raise a fleet", "o", "fleet", "pre", "fleet", "pre_ore", name)}, acts...)
		} else {
			acts = append(acts, p.link(p.div, "requisition", "o", "req", "pre", "req", "pre_item", name, "pre_qty", qty, "pre_station", sts, "pre_flag", "1"))
		}
		t.Rows = append(t.Rows, []string{esp.ShipIcon(r.id, 32), igbF3(esp.IGBText(name)), p.pg.soft(fmtUnits(r.units)),
			igbF3(strings.Join(acts, " &nbsp; "))})
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
	b.WriteString("<br>\n")
}

// writeBuildShelf is what the factory bases can build.
func (p *mgPage) writeBuildShelf(b *strings.Builder) {
	prints := p.m.buildPrints(p.ctx, p.corpID)
	t := esp.Panel{Title: "On the shelves", Note: "what the factory bases can build", Align: []string{"", "", "", "right"},
		Width: []string{"36", "", "150", "80"}, Empty: "No prints in the Blueprint Library division at a factory base."}
	for i, x := range prints {
		if i >= mgShelfRows {
			break
		}
		kind := "original, ME " + strconv.FormatInt(x.ME, 10)
		if !x.Original {
			kind = fmt.Sprintf("copies, %d runs", x.CopyRuns)
		} else if x.CopyRuns > 0 {
			kind += fmt.Sprintf(", and copies (%d runs)", x.CopyRuns)
		}
		t.Rows = append(t.Rows, []string{esp.ShipIcon(x.BPTypeID, 32),
			igbF3(esp.IGBText(x.Name)) + "<br>" + p.pg.dim(esp.IGBText(p.m.stationLabel(p.ctx, x.StationID))), p.pg.soft(kind),
			igbF3(p.link(p.div, "build", "o", "build", "pre", "build", "pre_print", fmt.Sprintf("%d.%d", x.BPTypeID, x.StationID)))})
	}
	if len(prints) > mgShelfRows {
		t.Note += fmt.Sprintf("; %d more in the form's list", len(prints)-mgShelfRows)
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
	b.WriteString("<br>\n")
}

// writeResearchShelf is the originals, with what a laboratory could do for each.
func (p *mgPage) writeResearchShelf(b *strings.Builder) {
	ctx, m := p.ctx, p.m
	prints, _ := m.libraryPrintsAll(ctx, p.corpID)
	moving := m.movingItems(ctx, p.corpID)
	pinned := map[int64]string{}
	if work, err := m.openWorkFor(ctx, p.corpID); err == nil {
		for _, w := range work {
			if w.BPItemID != 0 {
				pinned[w.BPItemID] = fmt.Sprintf("%s: %d of %d %s", map[string]string{workKindME: "ME", workKindPE: "PE", workKindCopy: "copying"}[w.Kind],
					w.RunsDone, w.RunsWanted, workUnit(w.Kind))
			}
		}
	}
	lines := map[int64]map[string]bool{}
	can := func(st int64, kind string) bool {
		if lines[st] == nil {
			lines[st] = map[string]bool{}
			for _, k := range []string{workKindME, workKindPE, workKindCopy} {
				lines[st][k] = m.labStationHasLine(ctx, st, labActivityOf(k))
			}
		}
		return lines[st][kind]
	}
	t := esp.Panel{Title: "Originals", Align: []string{"", "", "", "right"}, Width: []string{"36", "", "110", "190"},
		Empty: "No originals in the Blueprint Library division of any office."}
	for i, x := range prints {
		if i >= mgShelfRows {
			break
		}
		id := strconv.FormatInt(x.ItemID, 10)
		var acts []string
		state := ""
		switch {
		case moving[x.ItemID]:
			state = "being moved"
		case pinned[x.ItemID] != "":
			state = pinned[x.ItemID]
		case x.Flag == 111:
			state = "in a job"
		default:
			for _, k := range []struct{ v, l string }{{workKindME, "ME"}, {workKindPE, "PE"}, {workKindCopy, "copy"}} {
				if can(x.StationID, k.v) {
					acts = append(acts, p.link(p.div, k.l, "o", "research", "pre", "research", "pre_what", k.v, "pre_print", id))
				}
			}
			acts = append(acts, p.link(p.div, "move", "o", "printmove", "pre", "printmove", "pre_print", id))
		}
		right := igbF3(strings.Join(acts, " &nbsp; "))
		if state != "" {
			right = p.pg.soft(state)
		}
		t.Rows = append(t.Rows, []string{esp.ShipIcon(x.BPTypeID, 32),
			igbF3(esp.IGBText(m.typeNameOr(ctx, x.BPTypeID))) + "<br>" + p.pg.dim(esp.IGBText(m.stationLabel(ctx, x.StationID))),
			p.pg.soft(fmt.Sprintf("ME %d &middot; PE %d", x.ME, x.PE)), right})
	}
	if len(prints) > mgShelfRows {
		t.Note = fmt.Sprintf("%d of %d; the rest are in the form's list", mgShelfRows, len(prints))
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
	b.WriteString("<br>\n")
}

// writeBelts is the belts members called for help from.
func (p *mgPage) writeBelts(b *strings.Builder) {
	var ids []int64
	for id := range p.m.distressedBeltSystems(p.ctx, p.corpID) {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	t := esp.Panel{Title: "Belts in trouble", Note: "where members were driven off", Align: []string{"", "right"}, Width: []string{"", "260"},
		Empty: "Nobody has called for help."}
	for i, id := range ids {
		if i >= 12 {
			break
		}
		name := p.m.nameOfSystem(p.ctx, id)
		t.Rows = append(t.Rows, []string{igbF3("<b>"+esp.IGBText(name)+"</b> ") + p.m.systemCell(p.ctx, id), igbF3(strings.Join([]string{
			p.link(p.div, "send a fleet", "o", "fleet", "pre", "fleet", "pre_place", name),
			p.link(p.div, "patrol", "o", "standing", "pre", "standing", "pre_place", name),
			p.link(p.div, "post a job", "o", "assign", "pre", "assign", "pre_place", name)}, " &nbsp; "))})
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
	b.WriteString("<br>\n")
}

// ── the corporation ─────────────────────────────────────────────────────────

func (p *mgPage) writeCorp(b *strings.Builder) {
	m := p.m
	p.masthead(b, esp.CorpLogo(p.corpID, 48), p.pg.pal.Title, "The corporation", "What it is, where its effort goes and what it may spend.", 0, "")
	t := esp.Panel{Title: "In force", Align: []string{"", ""}, Width: []string{"130", ""}, Empty: "Nothing is set: every member follows its own leaning."}
	for _, k := range []struct{ kind, label string }{{cmdKindTags, "Identity"}, {cmdKindBudget, "Budget"}, {cmdKindOreDrop, "Fleet ore"}, {cmdKindOreDesk, "Ore desk"}, {cmdKindDirective, "Directive"}} {
		if k.kind == cmdKindDirective {
			if _, until := m.humanDirectiveHold(p.ctx, p.corpID); until > 0 {
				if d, _, _ := m.loadCorpDirective(p.ctx, p.corpID); len(d) > 0 {
					t.Rows = append(t.Rows, []string{p.pg.dim3(k.label), igbF3(esp.IGBText(directiveText(d))) + " " + p.pg.soft("until "+igbWhen(until))})
				}
			}
			continue
		}
		if c := m.humanHold(p.ctx, p.corpID, k.kind); c != nil {
			t.Rows = append(t.Rows, []string{p.pg.dim3(k.label), igbF3(esp.IGBText(orgHoldLine(c, m, p.ctx)))})
		}
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
	b.WriteString("<br>\n")
	p.writeTiles(b, "")
	p.writeOrders(b, func(c corpCommand) bool { return c.Arm == "" })
}
