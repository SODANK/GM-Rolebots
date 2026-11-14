package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"gurimoon/esp"
)

// The module's block on the Corporate Portal's Overview (esp.CorpOverviewSections): the corporation at a
// glance, where the viewer stands in it, and its departments as cards (who runs each, who is in it, what it is
// doing, where it works from, what joining gives, and the Join link). It is the front door for a capsuleer in
// a bot-run corporation. Joining goes by letter (corpjoin_igb.go) because the browser's identity is a hint.

// renderCorpOverview is the esp.CorpOverviewSection.
func (m *Mod) renderCorpOverview(ctx context.Context, r *http.Request, corpID, viewer int64) string {
	if corpID == 0 {
		return ""
	}
	pg := m.igbPageFor(r)
	pr := m.portalRosterFor(ctx, corpID)
	if len(pr.Members) == 0 {
		return ""
	}
	work, _ := m.openWorkFor(ctx, corpID)
	mfgJobs, rndJobs := m.corpJobCounts(ctx, corpID)
	snap := m.corpSnapshotFor(ctx, corpID, pr.CEOID)

	var b strings.Builder

	// At a glance.
	present, inSpace := onDuty(pr.Members)
	wages := 0.0
	for _, w := range work {
		wages += float64(w.RunsLeft()) * w.WagePerRun
	}
	pg.statStrip(&b, []igbStat{
		{Label: "On duty", Value: strconv.Itoa(present), Note: fmt.Sprintf("of %d &middot; %d in space", len(pr.Members), inSpace)},
		{Label: "Fleets out", Value: strconv.Itoa(len(pr.Fleets))},
		{Label: "Factory jobs", Value: strconv.Itoa(mfgJobs), Note: fmt.Sprintf("+ %d research", rndJobs)},
		{Label: "Paid work", Value: strconv.Itoa(len(work)), Note: esp.ISKShort(wages) + " ISK on offer"},
		{Label: "Came in", Value: esp.ISKShort(snap.Income), Note: "ISK this week"},
	})
	b.WriteString("<br>\n")

	// Where the viewer stands.
	c, _ := getCEO(ctx, m.db, corpID)
	var joinable []corpArm
	if c != nil {
		joinable = m.corpJoinableArms(ctx, c)
	}
	me, isMember := pr.member(viewer)
	m.writeYourPlace(ctx, &b, pg, pr, me, isMember, c != nil)
	b.WriteString("<br>\n")

	// The departments.
	esp.Section(&b, pg.pal, "Departments")
	b.WriteString(pg.soft("Every member works in one department. Its head sets the work; joining gives you the roles that work needs, "+
		"and you can move whenever you like.") + "<br>\n")
	facts := m.deptFacts(ctx, corpID, pr, work, mfgJobs, rndJobs, snap)
	offices := m.officesByPurpose(ctx, corpID)
	// Why nobody can join from here, "" when the reader can.
	closed := ""
	switch {
	case !isMember || me.OnRoster || c == nil:
		closed = "&mdash;"
	case !humanPipelineOn():
		closed = "the chair is not taking choices today"
	}
	var bands [][]esp.Panel
	var band []esp.Panel
	for _, a := range portalArmOrder {
		card := m.deptCard(ctx, pg, pr, a, facts[a], offices, me, closed, joinable)
		band = append(band, card)
		if len(band) == 3 {
			bands = append(bands, band)
			band = nil
		}
	}
	if len(band) > 0 {
		bands = append(bands, band)
	}
	esp.PanelGrid(&b, pg.pal, bands)
	return b.String()
}

// writeYourPlace is the viewer's own line: their department, rank, posting
// and roles, and the links that change them.
func (m *Mod) writeYourPlace(ctx context.Context, b *strings.Builder, pg igbPage, pr *portalRoster,
	me portalMember, isMember, managed bool) {

	if !isMember {
		return
	}
	host := esp.IGBHost()
	p := esp.Panel{Title: "Your place", Align: []string{"", "", ""}, Width: []string{"72", "", "220"}}
	var who strings.Builder
	who.WriteString(igbF3("<b>" + esp.IGBText(me.Name) + "</b>"))
	if me.Title != "" {
		who.WriteString(" " + pg.soft(esp.IGBText(me.Title)))
	}
	who.WriteString("<br>")
	switch {
	case me.CharID == pr.CEOID:
		who.WriteString(pg.accent("Chief Executive Officer") + "<br>")
	case me.Arm != "":
		who.WriteString(igbF3("You work in ") + armName(me.Arm))
		if me.Tier == armTierHead {
			who.WriteString(igbF3(" &mdash; you head it"))
		}
		who.WriteString("<br>")
		if roles := armRoleShort(armRoles(me.Arm, me.Tier, false)); roles != "" {
			who.WriteString(pg.soft("Your department gives you: "+esp.IGBText(roles)) + "<br>")
		}
	default:
		who.WriteString(pg.tint(pg.pal.Amber, "You are in no department yet") + " " +
			pg.soft("&mdash; pick one below and the corporation writes you to confirm.") + "<br>")
	}
	var posted int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT stationID FROM rolebot_corp_postings WHERE characterID = ? AND corporationID = ?`,
		me.CharID, pr.CorpID).Scan(&posted)
	if posted != 0 {
		who.WriteString(pg.soft("Posted to "+esp.IGBText(m.stationLabel(ctx, posted))) + "<br>")
	}
	links := ""
	if managed {
		links = igbF3(`<a href="`+host+`/corp/join?desk=1">Mail me my desk</a>`) + "<br>" +
			pg.soft("rank, assignments and pay") + "<br>"
	}
	links += igbF3(`<a href="` + host + `/corp/ops">Operations</a> &middot; <a href="` + host + `/corp/industry">Industry</a> &middot; <a href="` + host + `/corp/research">R&amp;D</a>`)
	p.Rows = [][]string{{esp.Portrait(me.CharID, 64), who.String(), links}}
	p.Shades = []string{igbShadeMine}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
}

// armPagePath is the department's own portal page, "" for one without.
func armPagePath(a corpArm) string {
	switch a {
	case armIndustry:
		return "/corp/industry"
	case armResearch:
		return "/corp/research"
	case armMining:
		return "/corp/mining"
	case armInfrastructure:
		return "/corp/infra"
	case armSecurity:
		return "/corp/security"
	case armSales:
		return "/corp/trade"
	}
	return ""
}

// deptFact is what one department is doing, in words, for its card.
type deptFact struct {
	Doing string
}

// deptFacts reads each department's live line.
func (m *Mod) deptFacts(ctx context.Context, corpID int64, pr *portalRoster, work []corpWork,
	mfgJobs, rndJobs int, snap corpSnapshot) map[corpArm]deptFact {

	out := map[corpArm]deptFact{}
	buildOrders, rndOrders := 0, 0
	for _, w := range work {
		switch w.Kind {
		case workKindME, workKindPE, workKindCopy:
			rndOrders++
		default:
			buildOrders++
		}
	}
	out[armIndustry] = deptFact{Doing: fmt.Sprintf("%d job(s) in the factory, %d build order(s) on the board", mfgJobs, buildOrders)}
	out[armResearch] = deptFact{Doing: fmt.Sprintf("%d research job(s) running, %d research order(s) on the board", rndJobs, rndOrders)}

	mining, security := 0, 0
	for _, g := range pr.Fleets {
		switch armOfSeatActivity(g.Activity) {
		case armMining:
			mining++
		case armSecurity:
			security++
		}
	}
	countSpace := func(a corpArm) int {
		_, s := onDuty(pr.ByArm[a])
		return s
	}
	out[armMining] = deptFact{Doing: fmt.Sprintf("%d mining fleet(s) out, %d member(s) in space", mining, countSpace(armMining))}
	out[armSecurity] = deptFact{Doing: fmt.Sprintf("%d combat fleet(s) out, %d member(s) in space", security, countSpace(armSecurity))}
	out[armSales] = deptFact{Doing: fmt.Sprintf("%d buy order(s) standing, %d member(s) trading or hauling",
		len(m.buybackRows(ctx, corpID)), countSpace(armSales))}

	var towers, legs int
	_ = m.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM rolebot_corp_towers WHERE corporationID = ? AND status = 'running'),
		       (SELECT COUNT(*) FROM rolebot_corp_haul WHERE corporationID = ? AND status IN ('open','claimed'))`,
		corpID, corpID).Scan(&towers, &legs)
	out[armInfrastructure] = deptFact{Doing: fmt.Sprintf("%d starbase(s) running, %d supply run(s) on the board", towers, legs)}

	var apps int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM chrApplications WHERE corporationID = ? AND deleted = 0 AND status = 0`, corpID).Scan(&apps)
	out[armHR] = deptFact{Doing: fmt.Sprintf("%d application(s) waiting", apps)}
	out[armFinance] = deptFact{Doing: fmt.Sprintf("%s in, %s out this week", esp.ISKShort(snap.Income), esp.ISKShort(snap.Cost))}
	out[""] = deptFact{Doing: "working wherever they do best"}
	return out
}

// deptCardRowCount is how many rows every card has, so cards side by side
// line up row for row (a band shares one table row per row index).
const deptCardRowCount = 6

// deptCardRows lays a card's parts out in the fixed row order. Pure.
func deptCardRows(labels, values [deptCardRowCount]string) [][]string {
	rows := make([][]string, deptCardRowCount)
	for i := range rows {
		rows[i] = []string{labels[i], values[i]}
	}
	return rows
}

// deptCardFaces is how many faces a card shows before "+N".
const deptCardFaces = 10

// deptFaceSize is the card portrait size: the client draws its silhouette
// for an uncached face only at 32 or 64 (components.py LoadPortrait), so a
// smaller face of a pilot nobody has rendered yet is "not available".
const deptFaceSize = 32

// deptFace is one member's portrait, named on hover with their title.
func deptFace(pm portalMember, title string) string {
	hint := pm.Name
	if title != "" {
		hint += " - " + title
	}
	return esp.PortraitHint(pm.CharID, deptFaceSize, hint)
}

// deptCard is one department as a card.
func (m *Mod) deptCard(ctx context.Context, pg igbPage, pr *portalRoster, a corpArm, fact deptFact,
	offices map[int64]int, me portalMember, closed string, joinable []corpArm) esp.Panel {

	ms := pr.ByArm[a]
	present, _ := onDuty(ms)
	title := armIcon(a, pr.CorpID, 24) + " " + armName(a)
	note := fmt.Sprintf("%d member(s)", len(ms))
	if path := armPagePath(a); path != "" {
		note += ` &middot; <a href="` + esp.IGBHost() + path + `">the department</a>`
	}
	card := esp.Panel{Title: title, Note: note,
		Align: []string{"", ""}, Width: []string{"70", ""}}

	// Head.
	head := pg.soft("nobody heads it")
	if a == "" {
		head = pg.soft("&mdash;")
	} else if h, ok := armHeadOf(ms); ok {
		hat := armHeadTitle(a)
		switch a {
		case armIndustry:
			hat = corpIndustryDirector
		case armFinance:
			hat = "Treasurer"
		}
		head = deptFace(h, hat) + " " + igbF3(esp.PilotLink(h.CharID, esp.IGBText(h.Name)))
		if hat != "" {
			head += "<br>" + pg.soft(esp.IGBText(hat))
		}
	}

	// Members.
	frac := 0.0
	if len(ms) > 0 {
		frac = float64(present) / float64(len(ms))
	}
	members := esp.Bar(frac, 12, armColor(a)) + " " + igbF3(fmt.Sprintf("%d on duty", present))

	// Office.
	office := pg.soft("&mdash;")
	bit := armPurpose(a)
	if a == armHR || a == armFinance || a == "" {
		bit = officeHQ
	}
	var at []string
	for st, bits := range offices {
		if bits&bit != 0 {
			at = append(at, esp.IGBText(m.stationLabel(ctx, st)))
		}
	}
	if len(at) > 0 {
		if len(at) > 2 {
			at = append(at[:2], fmt.Sprintf("+%d more", len(at)-2))
		}
		office = pg.soft(strings.Join(at, "<br>"))
	}

	// What it opens.
	opens := pg.soft("no corporation roles")
	if r := armRoleShort(armRoles(a, armTierMember, false)); r != "" {
		opens = pg.soft(esp.IGBText(r))
	} else if a == armHR || a == armFinance {
		opens = pg.soft("officer roles, for its officers")
	}

	// Faces under the duty bar, on duty first (the list is already sorted
	// that way). Each one names itself on hover and opens its dossier.
	var faces []string
	for i, pm := range ms {
		if i >= deptCardFaces {
			faces = append(faces, pg.dim(fmt.Sprintf("+%d", len(ms)-i)))
			break
		}
		faces = append(faces, `<a href="`+esp.IGBHost()+`/concord/capsuleer?id=`+strconv.FormatInt(pm.CharID, 10)+`">`+
			deptFace(pm, pm.Title)+`</a>`)
	}
	if len(faces) > 0 {
		members += "<br>" + strings.Join(faces, " ")
	} else {
		members += "<br>" + pg.soft("nobody yet")
	}

	// The action.
	action := ""
	inJoinable := false
	for _, j := range joinable {
		if j == a {
			inJoinable = true
		}
	}
	switch {
	case me.CharID != 0 && me.Arm == a && a != "":
		action = pg.tint(pg.pal.Green, "your department")
	case a == "":
		action = pg.soft("pick a department above")
	case a == armHR || a == armFinance:
		action = pg.soft("staffed by its officers")
	case closed != "":
		action = pg.soft(closed)
	case !inJoinable:
		action = pg.soft("not taking members today")
	default:
		action = igbF3(`<a href="` + esp.IGBHost() + `/corp/join?arm=` + string(a) + `"><b>Join ` +
			esp.IGBText(armLabel(a)) + ` &raquo;</b></a>`)
	}

	card.Rows = deptCardRows(
		[deptCardRowCount]string{pg.dim3("Head"), pg.dim3("Members"), pg.dim3("Doing now"), pg.dim3("Works from"),
			pg.dim3("Gives you"), ""},
		[deptCardRowCount]string{head, members, igbF3(esp.IGBText(fact.Doing)), office, opens, action})
	if me.CharID != 0 && me.Arm == a {
		for range card.Rows {
			card.Shades = append(card.Shades, igbShadeMine)
		}
	}
	return card
}
