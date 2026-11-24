package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gurimoon/esp"
)

// The Management page's people: who heads each division (the CEO's Appointments) and where every
// character works (Characters: division, base, fleet command). A division head places its own
// division's members and the pool's, and nobody the CEO has placed.

const mgPeoplePerPage = 40

// ── appointments ────────────────────────────────────────────────────────────

// directorsOf is the members a division may be given to: Directors who are not the CEO.
func (m *Mod) directorsOf(ctx context.Context, corpID int64) []corpMember {
	members, err := m.corpMembers(ctx, corpID, m.corpCEOID(ctx, corpID))
	if err != nil {
		return nil
	}
	var out []corpMember
	for _, cm := range members {
		if cm = m.asSeated(cm); cm.Eligible() && holdsDirector(cm.Roles) {
			out = append(out, cm)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// armDuty is what heading a division means for a bot, in a line. Pure.
func armDuty(a corpArm) string {
	switch a {
	case armIndustry:
		return "posts build orders, buys materials and prints"
	case armResearch:
		return "posts ME, PE and copy orders, moves prints to the labs"
	case armMining:
		return "reads the factory's shortfall and points the mining fleets at it"
	case armSecurity:
		return "reads the belts and the wars and points the combat wing"
	case armSales:
		return "keeps the market orders and posts trade runs"
	case armInfrastructure:
		return "keeps the starbases fuelled, defended and supplied"
	case armHR:
		return "invites pilots the divisions are short of"
	case armFinance:
		return "keeps watch on the books"
	}
	return ""
}

// pendingHeads is the appointments the desk has not carried out yet, newest first per division: the
// division's new head (0 for a stand-down) and, by pilot, the division they were appointed to. Without
// it a page drawn right after an order shows the old state until the desk's next pass.
func (m *Mod) pendingHeads(ctx context.Context, corpID int64) (byArm map[corpArm]int64, byChar map[int64]corpArm) {
	byArm, byChar = map[corpArm]int64{}, map[int64]corpArm{}
	for _, c := range m.listCorpCommands(ctx, corpID, 100) {
		h, ok := c.head()
		if !c.Open() || !ok {
			continue
		}
		a := corpArm(h.Arm)
		if _, seen := byArm[a]; seen {
			continue
		}
		byArm[a] = h.CharID
		if h.CharID != 0 {
			if _, seen := byChar[h.CharID]; !seen {
				byChar[h.CharID] = a
			}
		}
	}
	return byArm, byChar
}

// directorPlace is where a Director stands among the divisions, for the appointment list: the
// division they head, the one they are appointed to and the desk has yet to confirm, or the one they
// work in as a member (placed on the Characters page or dealt by the corporation). kind is "heads",
// "pending", "in" or "" for none. Pure.
func directorPlace(id int64, heads map[corpArm]corpHead, pending map[int64]corpArm, rows map[int64]armRow) (kind string, a corpArm) {
	for x, h := range heads {
		if h.CharID == id {
			return "heads", x
		}
	}
	if x, ok := pending[id]; ok {
		return "pending", x
	}
	if r, ok := rows[id]; ok && validArm(r.Arm) {
		return "in", r.Arm
	}
	return "", ""
}

// writeHeads is the appointments: a card per division with its head's face, and once a division is
// picked the Directors who could have it.
func (p *mgPage) writeHeads(b *strings.Builder) {
	if p.botCEO {
		b.WriteString(p.pg.ordersBanner(errHeadBotCEO.Error()+".", true))
		return
	}
	heads := p.m.corpHeads(p.ctx, p.corpID)
	pr := p.m.portalRosterFor(p.ctx, p.corpID)
	directors := p.m.directorsOf(p.ctx, p.corpID)
	pendingArm, pendingChar := p.m.pendingHeads(p.ctx, p.corpID)
	p.masthead(b, esp.UIIcon("12_01", 48, ""), p.pg.pal.Title, "Division heads",
		"One head per division, bot or capsuleer. The Director role qualifies; you grant it in the corporation window.", 0, "")
	b.WriteString("<br>\n")

	if p.q.Get("o") == "head" || p.q.Get("form") == "head" {
		if !p.previewing {
			p.writeAppoint(b, corpArm(p.cur("head", "arm", string(armOrder[0]))), heads, directors, pendingChar)
		}
	}
	var bands [][]esp.Panel
	var band []esp.Panel
	for _, a := range armOrder {
		face, who, verb := armIcon(a, p.corpID, 48), p.pg.soft("nobody heads it"), "appoint"
		if h, ok := heads[a]; ok {
			verb = "change"
			face = esp.Portrait(h.CharID, 64)
			who = igbF3("<b>"+esp.IGBText(p.m.charNameOr(p.ctx, h.CharID))+"</b>") + " " + p.pg.dim(p.kindOf(h.CharID)) +
				"<br>" + p.pg.soft("since "+igbWhen(h.Since))
			if h.DeputyID != 0 {
				who += "<br>" + p.pg.soft("deputy "+esp.IGBText(p.m.charNameOr(p.ctx, h.DeputyID)))
			}
			if flag, label := headFlagOf(a); flag != 0 && h.Flags&flag != 0 && p.m.botFlown(h.CharID) {
				who += "<br>" + p.pg.soft(label)
			}
		} else if id := p.headOf(a, pr, heads); id != 0 {
			// Nobody appointed, but the corporation's posts name a head, the one the desk's cards
			// show: list it, or this page says nobody heads a division somebody does.
			verb = "replace"
			face = esp.Portrait(id, 64)
			who = igbF3("<b>"+esp.IGBText(p.m.charNameOr(p.ctx, id))+"</b>") + " " + p.pg.dim(p.kindOf(id)) +
				"<br>" + p.pg.soft("posted by the corporation, not appointed")
		}
		if id, ok := pendingArm[a]; ok {
			// Ordered, not yet carried out: say so, or the order looks lost.
			if id == 0 {
				who += "<br>" + p.pg.soft("stand-down ordered; the desk takes it up within a minute")
			} else {
				who += "<br>" + p.pg.soft("ordered: "+esp.IGBText(p.m.charNameOr(p.ctx, id))+"; the desk takes it up within a minute")
			}
		}
		who += "<br>" + p.pg.dim3("a bot head "+armDuty(a)) + "<br>" +
			igbF3(p.link(mgHeads, "<b>"+verb+" &raquo;</b>", "o", "head", "pre", "head", "pre_arm", string(a)))
		band = append(band, esp.Panel{Title: armIcon(a, p.corpID, 24) + " " + armName(a), Align: []string{"", ""},
			Width: []string{"72", ""}, Rows: [][]string{{face, who}}})
		if len(band) == 2 {
			bands = append(bands, band)
			band = nil
		}
	}
	if len(band) > 0 {
		bands = append(bands, band)
	}
	esp.PanelGrid(b, p.pg.pal, bands)
	b.WriteString("<br>" + p.pg.soft("A bot head runs its division's desk inside your budget. A capsuleer head sees this page for the one division. "+
		"What you order yourself stands over either.") + "<br>\n")
}

// writeAppoint is one division's appointment: the candidates by face, then the form.
func (p *mgPage) writeAppoint(b *strings.Builder, a corpArm, heads map[corpArm]corpHead, directors []corpMember, pending map[int64]corpArm) {
	if !validArm(a) {
		a = armOrder[0]
	}
	p.openStrip(b, armColor(a), "Who heads "+armLabel(a)+"?", "Pick a face, or use the list.")
	if len(directors) == 0 {
		b.WriteString(p.pg.soft("Nobody but you holds the Director role. Grant it in the corporation window first.") + "<br><br>\n")
		return
	}
	placed, _ := p.m.loadArmRows(p.ctx, p.corpID)
	t := esp.Panel{Title: "Directors", Align: []string{"", "", "right"}, Width: []string{"40", "", "110"}}
	for i, d := range directors {
		if i >= 16 {
			break
		}
		kind := "bot"
		if d.Human() {
			kind = "capsuleer"
		}
		now := p.pg.soft("no division")
		held, at := directorPlace(d.CharID, heads, pending, placed)
		switch held {
		case "heads":
			now = p.pg.soft("heads ") + armName(at)
		case "pending":
			now = p.pg.soft("ordered to head ") + armName(at)
		case "in":
			now = p.pg.soft("works in ") + armName(at)
		}
		act := igbF3(p.link(mgHeads, "appoint", "form", "head", "arm", string(a), "member", strconv.FormatInt(d.CharID, 10)))
		if held == "heads" && at == a {
			act = igbF3(p.link(mgHeads, "stand down", "form", "head", "arm", string(a), "member", ""))
		}
		t.Rows = append(t.Rows, []string{esp.Portrait(d.CharID, 32), igbF3(esp.IGBText(d.Name)) + " " + p.pg.dim(kind) + "<br>" + now, act})
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
	b.WriteString("<br>" + p.open("head") + mgHidden("arm", string(a)))
	b.WriteString(`<font size=3>Head: </font><select name="member">` + selOpt("", "nobody (stand down)", p.cur("head", "member", "") == ""))
	for _, d := range directors {
		v := strconv.FormatInt(d.CharID, 10)
		kind := "bot"
		if d.Human() {
			kind = "capsuleer"
		}
		b.WriteString(selOpt(v, d.Name+" ("+kind+")", p.cur("head", "member", "") == v))
	}
	b.WriteString(`</select><br>`)
	if a == armIndustry {
		b.WriteString(`<font size=3>Deputy, who flies its print purchases: </font>` +
			memberSelect("deputy", p.m.orgMembers(p.ctx, p.corpID), p.cur("head", "deputy", ""), "the head, if a bot") + `<br>`)
	}
	if flag, label := headFlagOf(a); flag != 0 {
		b.WriteString(`<font size=3>A bot head ` + esp.IGBText(label) + `: </font><select name="extra">` +
			selOpt("", "no", p.cur("head", "extra", "") == "") + selOpt("1", "yes", p.cur("head", "extra", "") == "1") + `</select><br>`)
	}
	b.WriteString(p.submit("Preview appointment"))
}

func parseHeadForm(p *mgPage, _ corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	if p.botCEO {
		return nil, "", errHeadBotCEO.Error() + "."
	}
	a := corpArm(p.q.Get("arm"))
	if !validArm(a) {
		return nil, "", "Pick a division."
	}
	hidden["arm"] = string(a)
	id, _ := strconv.ParseInt(p.q.Get("member"), 10, 64)
	hidden["member"] = p.q.Get("member")
	hidden["deputy"], hidden["extra"] = p.q.Get("deputy"), p.q.Get("extra")
	heads := p.m.corpHeads(p.ctx, p.corpID)
	if id == 0 {
		cur, ok := heads[a]
		if !ok {
			return nil, "", "Nobody heads " + armLabel(a) + "."
		}
		cmd := corpCommand{Kind: cmdKindHead, Payload: encodeCommandPayload(cmdHeadPayload{Arm: string(a)})}
		return []corpCommand{cmd}, igbF3(fmt.Sprintf("Stand <b>%s</b> down from <b>%s</b>.",
			esp.IGBText(p.m.charNameOr(p.ctx, cur.CharID)), esp.IGBText(armLabel(a)))) + "<br>" +
			p.pg.dim3("They stay in the division as a member; its desk waits for a new head.") + "<br><br>", ""
	}
	if err := headRefusal(p.m.memberRankIn(p.ctx, p.corpID, id), a, p.botCEO); err != nil {
		msg := err.Error()
		return nil, "", strings.ToUpper(msg[:1]) + msg[1:] + "."
	}
	pay := cmdHeadPayload{Arm: string(a), CharID: id}
	if a == armIndustry {
		pay.DeputyID, _ = strconv.ParseInt(p.q.Get("deputy"), 10, 64)
		if pay.DeputyID == id {
			pay.DeputyID = 0
		}
		if pay.DeputyID != 0 && p.m.charCorpID(p.ctx, pay.DeputyID) != p.corpID {
			return nil, "", "The deputy is not a member."
		}
	}
	bot := p.m.botFlown(id)
	flag, label := headFlagOf(a)
	if flag != 0 && bot && p.q.Get("extra") == "1" {
		pay.Flags = flag
	}
	var b strings.Builder
	b.WriteString(igbF3(fmt.Sprintf("Put <b>%s</b> in charge of <b>%s</b>.", esp.IGBText(p.m.charNameOr(p.ctx, id)), esp.IGBText(armLabel(a)))) + "<br>")
	for _, old := range headsDisplaced(heads, corpHead{CorpID: p.corpID, Arm: a, CharID: id}) {
		b.WriteString(igbF3(fmt.Sprintf("%s stands down from %s.", esp.IGBText(p.m.charNameOr(p.ctx, old.CharID)), esp.IGBText(armLabel(old.Arm)))) + "<br>")
	}
	switch {
	case !bot:
		b.WriteString(p.pg.dim3("They see this page for the division alone, and give its orders themselves.") + "<br>")
	default:
		b.WriteString(p.pg.dim3("A bot: it "+armDuty(a)+", spending above the reserve only.") + "<br>")
		if pay.Flags != 0 {
			b.WriteString(p.pg.dim3("It "+label+".") + "<br>")
		}
	}
	b.WriteString("<br>")
	return []corpCommand{{Kind: cmdKindHead, Payload: encodeCommandPayload(pay)}}, b.String(), ""
}

// ── characters ──────────────────────────────────────────────────────────────

// peopleRow is one character as the roster draws it.
type peopleRow struct {
	portalMember
	BaseID     int64
	Seat       string // the fleet activity commanded, "" for none
	AssignedBy int64
}

// peopleFor is the characters a page may place: everybody for the CEO, the division's own and the
// pool's for a head. The CEO of the corporation is never listed.
func (p *mgPage) peopleFor(a corpArm) []peopleRow {
	pr := p.m.portalRosterFor(p.ctx, p.corpID)
	bases := map[int64]int64{}
	if rows, err := p.m.db.QueryContext(p.ctx, `
		SELECT r.characterID, COALESCE(r.baseID, 0)
		  FROM chrCorporationRoles r JOIN character_ c ON c.characterID = r.characterID
		 WHERE c.corporationID = ?`, p.corpID); err == nil {
		for rows.Next() {
			var id, st int64
			if rows.Scan(&id, &st) == nil {
				bases[id] = st
			}
		}
		rows.Close()
	}
	seats := map[int64]string{}
	if ranks, err := p.m.listCorpRanks(p.ctx, p.corpID); err == nil {
		for _, r := range ranks {
			seats[r.CharID] = r.Activity
		}
	}
	arms, _ := p.m.loadArmRows(p.ctx, p.corpID)
	var out []peopleRow
	for _, pm := range pr.Members {
		if pm.CharID == pr.CEOID || !pm.Eligible() {
			continue
		}
		if a != "" && pm.Arm != a && pm.Arm != "" {
			continue
		}
		out = append(out, peopleRow{portalMember: pm, BaseID: bases[pm.CharID], Seat: seats[pm.CharID],
			AssignedBy: arms[pm.CharID].AssignedBy})
	}
	return out
}

// seatActivities is the fleets a page may give command of.
func seatActivities(a corpArm) []struct{ slug, label string } {
	var out []struct{ slug, label string }
	for _, x := range []struct{ slug, label string }{{ActMining, "mining"}, {ActCombat, "hunting"}, {ActSecurity, "security"}, {ActPiracy, "raiding"}} {
		if a == "" || armOfSeatActivity(x.slug) == a {
			out = append(out, x)
		}
	}
	return out
}

func (p *mgPage) writePeople(b *strings.Builder, a corpArm) {
	people := p.peopleFor(a)
	switch {
	case a == "":
		bots, loose := 0, 0
		for _, r := range people {
			if !r.Human() {
				bots++
			}
			if r.Arm == "" {
				loose++
			}
		}
		p.masthead(b, esp.UIIcon("07_13", 48, ""), p.pg.pal.Title, "Characters",
			"Every member works in one division, lives and works at one base, and may command a fleet. A placement you make is kept.", 0, "")
		p.pg.statStrip(b, []igbStat{
			{Label: "Characters", Value: strconv.Itoa(len(people)), Note: "besides you"},
			{Label: "Bots", Value: strconv.Itoa(bots)},
			{Label: "Capsuleers", Value: strconv.Itoa(len(people) - bots)},
			{Label: "Unassigned", Value: strconv.Itoa(loose)},
		})
		b.WriteString("<br>\n")
	case p.scope.CEO:
		esp.Section(b, p.pg.pal, "Characters")
		b.WriteString(p.pg.dim3("The division's members, and those in no division.") + "<br>\n")
	default:
		esp.Section(b, p.pg.pal, "Characters")
		b.WriteString(p.pg.dim3("Your division's members, and those in no division. Take in the unassigned or release your own; "+
			"whoever the CEO placed stays.") + "<br>\n")
	}

	filter := ""
	if a == "" {
		filter = p.q.Get("f")
		var links []string
		links = append(links, p.link(p.div, "all"))
		for _, x := range portalArmOrder {
			label, slug := armLabel(x), string(x)
			if x == "" {
				label, slug = "Unassigned", "none"
			}
			links = append(links, p.link(p.div, esp.IGBText(label), "f", slug))
		}
		b.WriteString(igbF3(strings.Join(links, " &middot; ")) + "<br><br>\n")
	}
	var shown []peopleRow
	for _, r := range people {
		if filter == "" || string(r.Arm) == filter || (filter == "none" && r.Arm == "") {
			shown = append(shown, r)
		}
	}
	page, _ := strconv.Atoi(p.q.Get("page"))
	pages := (len(shown) + mgPeoplePerPage - 1) / mgPeoplePerPage
	page = min(max(page, 0), max(pages-1, 0))
	from := page * mgPeoplePerPage
	to := min(from+mgPeoplePerPage, len(shown))

	if who := p.picked(people); who != nil && !p.previewing {
		p.writeAssignForm(b, a, *who)
	}

	t := esp.Panel{Title: fmt.Sprintf("%d character(s)", len(shown)), Align: []string{"", "", "", "", ""},
		Width: []string{"36", "", "170", "", "70"}, Empty: "Nobody."}
	for _, r := range shown[from:to] {
		kind := "bot"
		if r.Human() {
			kind = "capsuleer"
		}
		who := igbF3(esp.PilotLink(r.CharID, esp.IGBText(r.Name))) + "<br>" + p.pg.dim(kind)
		if r.Title != "" {
			who += " " + p.pg.dim(esp.IGBText(r.Title))
		}
		arm := armName(r.Arm)
		switch r.Tier {
		case armTierHead:
			if r.Arm != "" {
				arm += " " + p.pg.dim("head")
			}
		case armTierJunior:
			arm += " " + p.pg.dim("officer")
		}
		work := p.pg.dim("no base")
		if r.BaseID != 0 {
			work = p.pg.soft(esp.IGBText(p.m.stationLabel(p.ctx, r.BaseID)))
		}
		if r.Seat != "" {
			work += "<br>" + p.pg.dim("commands "+strings.ToLower(activityLabel(r.Seat))+" fleets")
		}
		act := ""
		if why := p.assignRefusal(r, r.Arm); why == "" {
			act = igbF3(p.link(p.div, "assign", "o", "people", "pre", "people", "pre_member", strconv.FormatInt(r.CharID, 10)))
		}
		t.Rows = append(t.Rows, []string{deptFace(r.portalMember, r.Title), who, arm, work, act})
		shade := ""
		if r.CharID == p.charID {
			shade = igbShadeMine
		}
		t.Shades = append(t.Shades, shade)
	}
	esp.PanelGrid(b, p.pg.pal, [][]esp.Panel{{t}})
	if pages > 1 {
		var links []string
		for i := 0; i < pages; i++ {
			if i == page {
				links = append(links, "<b>"+strconv.Itoa(i+1)+"</b>")
			} else {
				links = append(links, p.link(p.div, strconv.Itoa(i+1), "page", strconv.Itoa(i), "f", filter))
			}
		}
		b.WriteString(igbF3("Page "+strings.Join(links, " ")) + "<br>\n")
	}
}

// picked is the character the page has open for assignment, nil for none.
func (p *mgPage) picked(people []peopleRow) *peopleRow {
	if p.q.Get("o") != "people" && p.q.Get("form") != "people" {
		return nil
	}
	id, _ := strconv.ParseInt(p.cur("people", "member", ""), 10, 64)
	for i := range people {
		if people[i].CharID == id && p.assignRefusal(people[i], people[i].Arm) == "" {
			return &people[i]
		}
	}
	return nil
}

// writeAssignForm is one character's assignment: who they are, and where they go.
func (p *mgPage) writeAssignForm(b *strings.Builder, a corpArm, r peopleRow) {
	now := "in no division"
	if r.Arm != "" {
		now = "in " + armLabel(r.Arm)
	}
	if r.BaseID != 0 {
		now += ", based at " + p.m.stationLabel(p.ctx, r.BaseID)
	}
	b.WriteString("<table width=100% bgcolor=" + igbShadeMine + "><tr><td width=72>" + esp.Portrait(r.CharID, 64) + "</td><td valign=middle><font size=5 color=" +
		p.pg.pal.Title + "><b>" + esp.IGBText(r.Name) + "</b></font><br>" + p.pg.soft(esp.IGBText(now)) + "</td><td width=70 align=right valign=top>" +
		igbF3(p.link(p.div, "close")) + "</td></tr></table><br>\n")
	b.WriteString(p.open("people") + mgHidden("member", strconv.FormatInt(r.CharID, 10)))
	cur := p.cur("people", "arm", "keep")
	b.WriteString(`<font size=3>Division: </font><select name="arm">` + selOpt("keep", "leave as it is", cur == "keep") +
		selOpt("pool", "none (the pool)", cur == "pool"))
	for _, x := range armOrder {
		if a == "" || x == a {
			b.WriteString(selOpt(string(x), armLabel(x), cur == string(x)))
		}
	}
	b.WriteString(`</select><br>`)
	if sel, n := p.m.officeSelect(p.ctx, "station", p.corpID, p.cur("people", "station", ""), "leave as it is"); n > 0 {
		b.WriteString(`<font size=3>Base: </font>` + sel + `<br>`)
	}
	if acts := seatActivities(a); len(acts) > 0 {
		cs := p.cur("people", "seat", "keep")
		b.WriteString(`<font size=3>Commands: </font><select name="seat">` + selOpt("keep", "leave as it is", cs == "keep") +
			selOpt("none", "no fleet", cs == "none"))
		for _, x := range acts {
			b.WriteString(selOpt(x.slug, x.label+" fleets", cs == x.slug))
		}
		b.WriteString(`</select><br>`)
	}
	b.WriteString(p.submit("Preview assignment"))
}

// assignRefusal is why a page's holder may not place a character into an arm, "" when they may.
func (p *mgPage) assignRefusal(r peopleRow, to corpArm) string {
	return placementRefusal(p.scope, r.Arm, to, r.Tier == armTierHead && r.Arm != "", r.AssignedBy != 0 && r.AssignedBy == p.ceoID)
}

// placementRefusal is the rule: a division head is moved by standing it down, and a division head
// places only between the pool and its own division, never over the CEO. Pure.
func placementRefusal(s commandScope, from, to corpArm, isHead, byCEO bool) string {
	switch {
	case isHead:
		return "They head a division. Stand them down under Appointments first."
	case s.CEO:
		return ""
	case from != "" && !s.Commands(from):
		return "They work in another division."
	case to != "" && !s.Commands(to):
		return "That is not your division."
	case byCEO && from != to:
		return "The CEO placed them."
	}
	return ""
}

// seatRefusal is why a page's holder may not change a character's fleet command, "" when they may.
func (p *mgPage) seatRefusal(r peopleRow, a corpArm) string {
	if p.scope.CEO {
		return ""
	}
	if r.Seat != "" && armOfSeatActivity(r.Seat) != a {
		return "They command another division's fleets."
	}
	return p.m.ceoHasSaid(p.ctx, p.scope, p.corpID, cmdKindSeat, r.CharID)
}

// ceoHasSaid is why a head may not change a character's base or seat: the last order on it was the
// CEO's. "" when it may.
func (m *Mod) ceoHasSaid(ctx context.Context, s commandScope, corpID int64, kind string, charID int64) string {
	if s.CEO {
		return ""
	}
	var ceo bool
	_ = m.db.QueryRowContext(ctx, `
		SELECT posterCEO FROM rolebot_corp_commands
		 WHERE corporationID = ? AND kind = ? AND refID = ? AND status = 'done'
		 ORDER BY commandID DESC LIMIT 1`, corpID, kind, charID).Scan(&ceo)
	if !ceo {
		return ""
	}
	if kind == cmdKindSeat {
		return "The CEO gave them their fleet command."
	}
	return "The CEO posted them where they are."
}

func parsePeopleForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	id, _ := strconv.ParseInt(p.q.Get("member"), 10, 64)
	var who *peopleRow
	people := p.peopleFor(a)
	for i := range people {
		if people[i].CharID == id {
			who = &people[i]
			break
		}
	}
	if who == nil {
		return nil, "", "Pick a character."
	}
	for _, k := range []string{"member", "arm", "station", "seat"} {
		hidden[k] = p.q.Get(k)
	}
	name := esp.IGBText(who.Name)
	var cmds []corpCommand
	var lines []string

	to := who.Arm
	switch v := p.q.Get("arm"); v {
	case "", "keep":
	case "pool":
		to = ""
	default:
		to = corpArm(v)
		if !validArm(to) {
			return nil, "", "Pick a division."
		}
	}
	if why := p.assignRefusal(*who, to); why != "" {
		return nil, "", why
	}
	if to != who.Arm {
		cmds = append(cmds, corpCommand{Kind: cmdKindArm, Payload: encodeCommandPayload(cmdArmPayload{CharID: id, Arm: string(to)})})
		if to == "" {
			lines = append(lines, fmt.Sprintf("Release <b>%s</b> from %s to the pool.", name, esp.IGBText(armLabel(who.Arm))))
		} else {
			line := fmt.Sprintf("Place <b>%s</b> in <b>%s</b>.", name, esp.IGBText(armLabel(to)))
			if roles := armGrantShort(p.m.corpDivs(p.ctx, p.corpID), to, armTierMember, p.m.baseRolesActive(p.ctx)); roles != "" {
				line += " It gives them: " + esp.IGBText(roles) + "."
			}
			lines = append(lines, line)
		}
	}
	if st, _ := strconv.ParseInt(p.q.Get("station"), 10, 64); st != 0 && st != who.BaseID {
		if why := p.m.ceoHasSaid(p.ctx, p.scope, p.corpID, cmdKindPosting, id); why != "" {
			return nil, "", why
		}
		if !p.m.corpHasOfficeAt(p.ctx, p.corpID, st) {
			return nil, "", "Pick one of the corporation's offices."
		}
		cmds = append(cmds, corpCommand{Kind: cmdKindPosting, Payload: encodeCommandPayload(cmdPostingPayload{CharID: id, StationID: st})})
		lines = append(lines, fmt.Sprintf("Post <b>%s</b> to <b>%s</b>.", name, esp.IGBText(p.m.stationLabel(p.ctx, st))))
	}
	switch v := p.q.Get("seat"); v {
	case "", "keep":
	case "none":
		if why := p.seatRefusal(*who, a); why != "" {
			return nil, "", why
		}
		if who.Seat != "" {
			cmds = append(cmds, corpCommand{Kind: cmdKindSeat, Payload: encodeCommandPayload(cmdSeatPayload{CharID: id, Activity: who.Seat, Release: true})})
			lines = append(lines, fmt.Sprintf("Release <b>%s</b> from fleet command.", name))
		}
	default:
		ok := false
		for _, x := range seatActivities(a) {
			ok = ok || x.slug == v
		}
		if !ok {
			return nil, "", "Pick a fleet you may give command of."
		}
		if why := p.seatRefusal(*who, a); why != "" {
			return nil, "", why
		}
		if v != who.Seat {
			cmds = append(cmds, corpCommand{Kind: cmdKindSeat, Payload: encodeCommandPayload(cmdSeatPayload{CharID: id, Activity: v})})
			lines = append(lines, fmt.Sprintf("Seat <b>%s</b> as <b>%s</b> fleet commander.", name, strings.ToLower(activityLabel(v))))
		}
	}
	if len(cmds) == 0 {
		return nil, "", "That changes nothing."
	}
	note := "They work that way from their next plan, and the placement is kept until you change it."
	if who.Human() {
		note = "They are told by letter, and the placement is kept until you change it."
	}
	return cmds, igbF3(strings.Join(lines, "<br>")) + "<br>" + p.pg.dim3(note) + "<br><br>", ""
}
