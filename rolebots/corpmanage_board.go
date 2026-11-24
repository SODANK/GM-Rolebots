package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"gurimoon/esp"
)

// The Management page's board forms: every order a bot CEO gives its members, on the page of the
// division that carries it out. Each form is checked against what the corporation has (the print, the
// office, the money above the reserve) before it is previewed, and the desk checks again when it
// takes it up (corpcommanddesk_board.go).

// workPriorities is what the chair may ask of the board: ahead of everything, with replacements, or
// with the rest.
var workPriorities = []struct {
	v     int
	label string
}{{100, "first"}, {60, "soon"}, {30, "when there is time"}}

// divisionForms is the forms of the divisions, in the order a page draws them.
func divisionForms() []mgForm {
	return []mgForm{
		{Form: "fleet", Icon: fleetTileIcon, Title: "Raise a fleet", Short: "pilots to a system, now", Arms: []corpArm{armMining, armSecurity},
			Note:  "A commander of the corporation musters the fleet at their home station and takes it where you say.",
			Write: writeFleetForm, Parse: parseFleetForm},
		{Form: "standing", Icon: uiTile("07_07"), Title: "Standing order", Short: "what the fleets go for", Arms: []corpArm{armMining, armSecurity},
			Note:  "What the division's fleets go for as they form, without raising one now.",
			Write: writeStandingForm, Parse: parseStandingForm},
		{Form: "build", Icon: uiTile("18_04"), Title: "Build", Short: "paid runs on the board", Arms: []corpArm{armIndustry},
			Note: "Paid runs of a print on the shelf at a factory base, fed from the Materials division there. Members with Factory Manager take them, " +
				"bots and capsuleers alike, and are paid on delivery.",
			Write: writeBuildForm, Parse: parseBuildForm},
		{Form: "research", Icon: uiTile("33_02"), Title: "Research and copying", Short: "ME, PE and copies", Arms: []corpArm{armResearch},
			Note:  "ME, PE or copies of one original, at the office it is in. Members based there take it and are paid on delivery.",
			Write: writeResearchForm, Parse: parseResearchForm},
		{Form: "bid", Icon: uiTile("18_01"), Title: "Buy on the market", Short: "ore, minerals, anything", Arms: []corpArm{armIndustry, armMining, armSales},
			Note:  "A corporation buy order at one of the offices: ore, minerals, anything. Members who sell into it deliver to the corporation.",
			Write: writeBidForm, Parse: parseBidForm},
		{Form: "req", Icon: uiTile("07_13"), Title: "Requisition", Short: "get something into a hangar", Arms: armOrder,
			Note: "Something the division needs in its hangar. With days to spare a buy order is put up; in a hurry a hauler is funded and sent; " +
				"and if the library can make it for less, Industry builds it.",
			Write: writeReqForm, Parse: parseReqForm},
		{Form: "haul", Icon: typeTile(648), Title: "Haul", Short: "goods between offices", Arms: []corpArm{armInfrastructure, armIndustry, armSales},
			Note:  "Goods from a division of one office to a division of another, carried by members for a wage.",
			Write: writeHaulForm, Parse: parseHaulForm},
		{Form: "trade", Icon: typeTile(649), Title: "Trade run", Short: "buy there, sell here", Arms: []corpArm{armSales},
			Note:  "A hauler is funded to buy at a system's best price and bring the goods to the Trade division of an office.",
			Write: writeTradeForm, Parse: parseTradeForm},
		{Form: "printbuy", Icon: uiTile("07_12"), Title: "Buy a blueprint", Short: "send a bot for a print", Arms: []corpArm{armIndustry, armResearch},
			Note:  "A bot is funded to buy it and bring it to the library. One at a time.",
			Write: writePrintBuyForm, Parse: parsePrintBuyForm},
		{Form: "printmove", Icon: typeTile(672), Title: "Move a blueprint", Short: "to another base", Arms: []corpArm{armResearch},
			Note:  "Carried to another office's library by a Director, or a copy by any member based where it lies.",
			Write: writePrintMoveForm, Parse: parsePrintMoveForm},
		{Form: "printsale", Icon: uiTile("18_01"), Title: "Sell a blueprint copy", Short: "copies for the market", Arms: []corpArm{armResearch, armSales},
			Note:  "A member at the office lists it on contract. Originals are never sold.",
			Write: writePrintSaleForm, Parse: parsePrintSaleForm},
		{Form: "survey", Icon: typeTile(typeSurveyProbe), Title: "Survey moons", Short: "what the moons hold", Arms: []corpArm{armInfrastructure},
			Note:  "A member fetches probes from a Starbase division and surveys the system, paid by the moon.",
			Write: writeSurveyForm, Parse: parseSurveyForm},
		{Form: "assign", Icon: uiTile("12_01"), Title: "Assignment for capsuleers", Short: "a paid job on the board", Arms: []corpArm{armIndustry, armSecurity, armSales, armMining},
			Note:  "A job on the Assignments board, for the first capsuleer of the corporation to take it, paid when it is done.",
			Write: writeAssignmentForm, Parse: parseAssignmentForm},
		{Form: "invite", Icon: uiTile("12_01"), Title: "Invite a pilot", Short: "bring a pilot in", Arms: []corpArm{armHR},
			Note:  "A bot pilot looks at the invitation at once and joins if it would rather fly here.",
			Write: writeInviteForm, Parse: parseInviteForm},
		{Form: "factory", Icon: uiTile("17_01"), Title: "Factory base", Short: "where members build", Arms: []corpArm{armIndustry}, CEOOnly: true,
			Note:  "Which office the corporation builds at. Moving it sends the old office's stock after it and withdraws the bids there.",
			Write: writeFactoryForm, Parse: parseFactoryForm},
	}
}

// ── shared checks ───────────────────────────────────────────────────────────

// num reads a number off the form: commas and an ISK suffix are forgiven.
func (p *mgPage) num(field string) (float64, bool) {
	v := strings.TrimSpace(strings.ReplaceAll(strings.TrimSuffix(strings.TrimSpace(strings.ToUpper(p.q.Get(field))), "ISK"), ",", ""))
	mul := 1.0
	switch {
	case strings.HasSuffix(v, "K"):
		mul, v = 1e3, strings.TrimSuffix(v, "K")
	case strings.HasSuffix(v, "M"):
		mul, v = 1e6, strings.TrimSuffix(v, "M")
	case strings.HasSuffix(v, "B"):
		mul, v = 1e9, strings.TrimSuffix(v, "B")
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return f * mul, true
}

// count reads a whole number in a range.
func (p *mgPage) count(field string, most int64) (int64, bool) {
	f, ok := p.num(field)
	if !ok || f != math.Trunc(f) || f < 1 || f > float64(most) {
		return 0, false
	}
	return int64(f), true
}

// keep copies fields the confirmation must carry.
func (p *mgPage) keep(hidden map[string]string, fields ...string) {
	for _, k := range fields {
		if v := p.q.Get(k); v != "" {
			hidden[k] = v
		}
	}
}

// chairFree is what the corporation may commit above its reserve.
func (m *Mod) chairFree(ctx context.Context, corpID int64) float64 {
	ch := m.humanChairCharter(ctx, corpID)
	if c, err := getCEO(ctx, m.db, corpID); err == nil && c != nil {
		ch = ceoCharterOf(ctx, m, c)
	}
	free, _, err := m.corpFreeCapital(ctx, corpID, ch)
	if err != nil {
		return 0
	}
	return free
}

// afford is why the corporation cannot commit an amount, "" when it can.
func (m *Mod) afford(ctx context.Context, corpID int64, amount float64) string {
	if free := m.chairFree(ctx, corpID); amount > free {
		return fmt.Sprintf("That commits %s and the corporation has %s free above its reserve.", iskString(amount), iskString(free))
	}
	return ""
}

// marketTypeNamed finds something the market trades by its exact name.
func (m *Mod) marketTypeNamed(ctx context.Context, name string) (int64, string) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return 0, ""
	}
	var id int64
	var found string
	_ = m.db.QueryRowContext(ctx, `
		SELECT typeID, typeName FROM invTypes
		 WHERE typeName = ? AND published = 1 AND marketGroupID IS NOT NULL LIMIT 1`, name).Scan(&id, &found)
	return id, found
}

// mgSuggestMax is how many near misses a page offers.
const mgSuggestMax = 12

// marketTypesLike is what the market trades whose name holds the words, shortest name first.
func (m *Mod) marketTypesLike(ctx context.Context, words string) []orgMember {
	words = strings.Join(strings.Fields(words), " ")
	if len(words) < 3 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, typeName FROM invTypes
		 WHERE typeName LIKE ? AND published = 1 AND marketGroupID IS NOT NULL
		 ORDER BY (typeName LIKE ?) DESC, CHAR_LENGTH(typeName), typeName LIMIT ?`,
		"%"+likeEsc(words)+"%", likeEsc(words)+"%", mgSuggestMax)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []orgMember
	for rows.Next() {
		var t orgMember
		if rows.Scan(&t.CharID, &t.Name) == nil {
			out = append(out, t)
		}
	}
	return out
}

// likeEsc escapes a LIKE pattern's own marks.
func likeEsc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// typeField reads the item field: its exact name, else the one thing the letters plainly mean (words in
// any order, a part of the name, a slip of the keys), else a refusal with the things they could mean to
// click.
func (p *mgPage) typeField(form string) (int64, string, string) {
	words := strings.TrimSpace(p.q.Get("item"))
	if words == "" {
		return 0, "", "Name the item."
	}
	if id, name := p.m.marketTypeNamed(p.ctx, words); id != 0 {
		return id, name, ""
	}
	hits := p.m.findMarketTypes(p.ctx, words, mgSuggestMax)
	if findClear(hits) {
		return hits[0].ID, hits[0].Name, ""
	}
	p.offerHits("item", hits, func(id int64) string { return esp.ShipIcon(id, 16) })
	if len(hits) == 0 {
		return 0, "", "Nothing on the market looks like \"" + words + "\"."
	}
	return 0, "", "\"" + words + "\" could be more than one thing."
}

// systemField reads the system field: its exact name, else the one the letters plainly mean, else a
// refusal with the near ones to click.
func (p *mgPage) systemField() (int64, string) {
	words := strings.TrimSpace(p.q.Get("system"))
	if words == "" {
		return 0, "Name the solar system."
	}
	if id := p.m.systemByName(p.ctx, words); id != 0 {
		return id, ""
	}
	hits := p.m.findSystems(p.ctx, words, mgSuggestMax)
	if findClear(hits) {
		return hits[0].ID, ""
	}
	p.offerHits("system", hits, nil)
	if len(hits) == 0 {
		return 0, "No solar system looks like \"" + words + "\"."
	}
	return 0, "\"" + words + "\" could be more than one system."
}

// office reads an office select.
func (p *mgPage) office(field string) (int64, string) {
	st, _ := strconv.ParseInt(p.q.Get(field), 10, 64)
	if st == 0 || p.m.officeAt(p.ctx, p.corpID, st) == 0 {
		return 0, "Pick one of the corporation's offices."
	}
	return st, ""
}

// divSelect is a hangar division select. A division head is offered its own divisions first.
func (p *mgPage) divSelect(name string, a corpArm, cur string, only bool) string {
	divs := armDivisions(p.m.corpDivs(p.ctx, p.corpID), a)
	if len(divs) == 0 || !only || p.scope.CEO {
		own := map[int]bool{}
		for _, d := range divs {
			own[d] = true
		}
		for d := 1; d <= 7; d++ {
			if !own[d] {
				divs = append(divs, d)
			}
		}
	}
	var b strings.Builder
	b.WriteString(`<select name="` + name + `">`)
	for i, d := range divs {
		v := strconv.Itoa(d)
		b.WriteString(selOpt(v, p.m.divLabelAt(p.ctx, p.corpID, divFlag(d)), cur == v || (cur == "" && i == 0)))
	}
	b.WriteString(`</select>`)
	return b.String()
}

// division reads a division select.
func (p *mgPage) division(field string) (int64, string) {
	d, err := strconv.Atoi(p.q.Get(field))
	if err != nil || d < 1 || d > 7 {
		return 0, "Pick a hangar division."
	}
	return divFlag(d), ""
}

func (p *mgPage) officeField(form, label, name string) (string, bool) {
	sel, n := p.m.officeSelect(p.ctx, name, p.corpID, p.cur(form, name, ""), "")
	return `<font size=3>` + label + ` </font>` + sel + ` `, n > 0
}

func (p *mgPage) noOffice(b *strings.Builder) {
	b.WriteString(p.pg.dim3("The corporation rents no office yet.") + "<br>")
}

func one(c corpCommand, preview string) ([]corpCommand, string, string) {
	return []corpCommand{c}, preview + "<br><br>", ""
}

func no(why string) ([]corpCommand, string, string) { return nil, "", why }

// ── build ───────────────────────────────────────────────────────────────────

// buildPrint is a print type a base can build from.
type buildPrint struct {
	StationID, BPTypeID int64
	Name                string
	ME                  int64
	Original            bool
	CopyRuns            int64
}

// buildPrints is what every factory base may build: originals, and copies with runs left.
func (m *Mod) buildPrints(ctx context.Context, corpID int64) []buildPrint {
	var out []buildPrint
	for _, st := range m.industryBaseStations(ctx, corpID) {
		byType := map[int64]*buildPrint{}
		at := func(t int64) *buildPrint {
			if x, ok := byType[t]; ok {
				return x
			}
			x := &buildPrint{StationID: st, BPTypeID: t, Name: m.typeNameOr(ctx, t)}
			byType[t] = x
			return x
		}
		if prints, err := m.libraryPrints(ctx, corpID, st); err == nil {
			for _, pr := range prints {
				x := at(pr.BPTypeID)
				x.Original = true
				x.ME = max(x.ME, pr.ME)
			}
		}
		if copies, err := m.libraryCopies(ctx, corpID, st); err == nil {
			for _, c := range copies {
				at(c.BPTypeID).CopyRuns += c.Runs
			}
		}
		for _, x := range byType {
			out = append(out, *x)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].StationID < out[j].StationID
	})
	return out
}

func writeBuildForm(p *mgPage, b *strings.Builder, a corpArm) {
	prints := p.m.buildPrints(p.ctx, p.corpID)
	if len(prints) == 0 {
		b.WriteString(p.pg.dim3("There is nothing to build from: rent an office at a station with assembly lines and put prints in its Blueprint Library division.") + "<br>")
		return
	}
	bases := map[int64]bool{}
	for _, x := range prints {
		bases[x.StationID] = true
	}
	b.WriteString(p.open("build") + `<font size=3>Print: </font><select name="print">`)
	cur := p.cur("build", "print", "")
	for _, x := range prints {
		label := x.Name
		switch {
		case x.Original && x.ME != 0:
			label += fmt.Sprintf(" (ME %d)", x.ME)
		case !x.Original:
			label += fmt.Sprintf(" (copies, %d runs)", x.CopyRuns)
		}
		if len(bases) > 1 {
			label += " - " + p.m.stationLabel(p.ctx, x.StationID)
		}
		v := fmt.Sprintf("%d.%d", x.BPTypeID, x.StationID)
		b.WriteString(selOpt(v, label, cur == v))
	}
	b.WriteString(`</select><br>`)
	b.WriteString(mgText("Runs:", "runs", p.cur("build", "runs", ""), 5, ""))
	b.WriteString(mgText("wage per run:", "wage", p.cur("build", "wage", ""), 10, "ISK"))
	b.WriteString(p.prioritySelect("build") + `<br>`)
	b.WriteString(`<font size=3>Deliver into </font><select name="dest">` + selOpt("", "Production", p.cur("build", "dest", "") == ""))
	for d := 1; d <= 7; d++ {
		v := strconv.Itoa(d)
		b.WriteString(selOpt(v, p.m.divLabelAt(p.ctx, p.corpID, divFlag(d)), p.cur("build", "dest", "") == v))
	}
	b.WriteString(`</select> ` + mgText("note:", "note", p.cur("build", "note", ""), 30, "") + `<br>` + p.submit("Preview build order"))
}

func (p *mgPage) prioritySelect(form string) string {
	var b strings.Builder
	b.WriteString(`<font size=3>wanted </font><select name="prio">`)
	cur := p.cur(form, "prio", "100")
	for _, x := range workPriorities {
		b.WriteString(selOpt(strconv.Itoa(x.v), x.label, cur == strconv.Itoa(x.v)))
	}
	b.WriteString(`</select>`)
	return b.String()
}

func (p *mgPage) priority() int {
	n, _ := strconv.Atoi(p.q.Get("prio"))
	for _, x := range workPriorities {
		if x.v == n {
			return n
		}
	}
	return workPriorities[0].v
}

func parseBuildForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	bs, ss, _ := strings.Cut(p.q.Get("print"), ".")
	bp, _ := strconv.ParseInt(bs, 10, 64)
	st, _ := strconv.ParseInt(ss, 10, 64)
	ok := false
	for _, x := range p.m.buildPrints(p.ctx, p.corpID) {
		ok = ok || (x.BPTypeID == bp && x.StationID == st)
	}
	if !ok {
		return no("Pick a print.")
	}
	runs, okRuns := p.count("runs", 1000)
	if !okRuns {
		return no("Runs are 1 to 1000.")
	}
	wage, okWage := p.num("wage")
	if !okWage || wage < 1 {
		return no("Give a wage per run, in ISK.")
	}
	var product int64
	_ = p.m.db.QueryRowContext(p.ctx, `SELECT productTypeID FROM invBlueprintTypes WHERE blueprintTypeID = ?`, bp).Scan(&product)
	if product == 0 {
		return no("That print builds nothing this board knows.")
	}
	cost := wage * float64(runs)
	if why := p.m.afford(p.ctx, p.corpID, cost); why != "" {
		return no(why)
	}
	pay := cmdWorkPayload{Kind: workKindBuild, BPTypeID: bp, ProductTypeID: product, StationID: st, Runs: int(runs),
		WagePerRun: wage, Priority: p.priority()}
	where := "Production"
	if p.q.Get("dest") != "" {
		flag, bad := p.division("dest")
		if bad != "" {
			return no(bad)
		}
		pay.DestFlag = flag
		pay.RequesterArm = string(divisionArm(p.m.corpDivs(p.ctx, p.corpID), flagDiv(flag)))
		where = p.m.divLabelAt(p.ctx, p.corpID, flag)
	}
	p.keep(hidden, "print", "runs", "wage", "prio", "dest", "note")
	c := corpCommand{Kind: cmdKindWork, Payload: encodeCommandPayload(pay), Note: clampText(strings.TrimSpace(p.q.Get("note")), 160)}
	return one(c, esp.ShipIcon(product, 32)+" "+igbF3(fmt.Sprintf("Build <b>%d</b> run(s) of <b>%s</b> at %s into %s, <b>%s</b> per run: <b>%s</b> in wages.",
		runs, esp.IGBText(p.m.typeNameOr(p.ctx, product)), esp.IGBText(p.m.stationLabel(p.ctx, st)), esp.IGBText(where), iskString(wage), iskString(cost)))+
		"<br>"+p.pg.dim3("The materials come out of the Materials division at that office."))
}

// ── research and copying ────────────────────────────────────────────────────

func (p *mgPage) printLabel(pr libraryPrint) string {
	return fmt.Sprintf("%s (ME %d, PE %d) - %s", p.m.typeNameOr(p.ctx, pr.BPTypeID), pr.ME, pr.PE, p.m.stationLabel(p.ctx, pr.StationID))
}

func (p *mgPage) printSelect(form, name string, prints []libraryPrint) string {
	type row struct{ v, l string }
	var rows []row
	for _, pr := range prints {
		rows = append(rows, row{strconv.FormatInt(pr.ItemID, 10), p.printLabel(pr)})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].l < rows[j].l })
	var b strings.Builder
	b.WriteString(`<select name="` + name + `">`)
	cur := p.cur(form, name, "")
	for _, r := range rows {
		b.WriteString(selOpt(r.v, r.l, cur == r.v))
	}
	b.WriteString(`</select>`)
	return b.String()
}

// labActivityOf is the lab line a kind of research runs on.
func labActivityOf(kind string) int64 { return int64(workActivity(kind)) }

func writeResearchForm(p *mgPage, b *strings.Builder, a corpArm) {
	prints, _ := p.m.libraryPrintsAll(p.ctx, p.corpID)
	var labs []libraryPrint
	for _, pr := range prints {
		if p.m.labStationHasLine(p.ctx, pr.StationID, ramActivityMECorp) || p.m.labStationHasLine(p.ctx, pr.StationID, ramActivityPEResearch) ||
			p.m.labStationHasLine(p.ctx, pr.StationID, ramActivityCopying) {
			labs = append(labs, pr)
		}
	}
	if len(labs) == 0 {
		b.WriteString(p.pg.dim3("No original of the corporation's is at an office with laboratory lines. Put one in the Blueprint Library division there, or move one there below.") + "<br>")
		return
	}
	b.WriteString(p.open("research") + `<select name="what">`)
	for _, k := range []struct{ v, l string }{{workKindME, "Material research (ME)"}, {workKindPE, "Time research (PE)"}, {workKindCopy, "Copies"}} {
		b.WriteString(selOpt(k.v, k.l, p.cur("research", "what", "") == k.v))
	}
	b.WriteString(`</select> <font size=3>of </font>` + p.printSelect("research", "print", labs) + `<br>`)
	b.WriteString(mgText("Levels, or copies:", "runs", p.cur("research", "runs", ""), 5, ""))
	b.WriteString(mgText("wage for each:", "wage", p.cur("research", "wage", ""), 10, "ISK, empty for the going rate"))
	b.WriteString(p.prioritySelect("research") + `<br>` + p.submit("Preview research order"))
}

func parseResearchForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	kind := p.q.Get("what")
	if kind != workKindME && kind != workKindPE && kind != workKindCopy {
		return no("Pick the research.")
	}
	item, _ := strconv.ParseInt(p.q.Get("print"), 10, 64)
	prints, _ := p.m.libraryPrintsAll(p.ctx, p.corpID)
	var pr *libraryPrint
	for i := range prints {
		if prints[i].ItemID == item {
			pr = &prints[i]
		}
	}
	if pr == nil {
		return no("Pick an original.")
	}
	n, ok := p.count("runs", 100)
	if !ok {
		return no("Levels and copies are 1 to 100.")
	}
	if !p.m.labStationHasLine(p.ctx, pr.StationID, labActivityOf(kind)) {
		return no(p.m.stationLabel(p.ctx, pr.StationID) + " has no line for that.")
	}
	if why := p.m.printBusy(p.ctx, p.corpID, item); why != "" {
		return no(why)
	}
	wage := p.m.researchWage(p.ctx, pr.BPTypeID)
	if kind == workKindCopy {
		wage = p.m.copyWage(p.ctx, pr.BPTypeID)
	}
	if strings.TrimSpace(p.q.Get("wage")) != "" {
		if wage, ok = p.num("wage"); !ok || wage < 1 {
			return no("Give the wage in ISK.")
		}
	}
	cost := wage * float64(n)
	if why := p.m.afford(p.ctx, p.corpID, cost); why != "" {
		return no(why)
	}
	p.keep(hidden, "what", "print", "runs", "wage", "prio")
	pay := cmdWorkPayload{Kind: kind, BPTypeID: pr.BPTypeID, ProductTypeID: pr.BPTypeID, StationID: pr.StationID, Runs: int(n),
		WagePerRun: wage, BPItemID: item, Priority: p.priority()}
	c := corpCommand{Kind: cmdKindWork, Payload: encodeCommandPayload(pay)}
	return one(c, esp.ShipIcon(pr.BPTypeID, 32)+" "+igbF3(fmt.Sprintf("%s <b>%s</b>: <b>%d</b> %s at %s, <b>%s</b> each, <b>%s</b> in wages.",
		workKindWords(kind), esp.IGBText(p.m.typeNameOr(p.ctx, pr.BPTypeID)), n, workUnit(kind),
		esp.IGBText(p.m.stationLabel(p.ctx, pr.StationID)), iskString(wage), iskString(cost)))+
		"<br>"+p.pg.dim3("Members with Factory Manager whose base is that office take it."))
}

// copyWage is the board's wage for one copy of a print (postCopyWork's rate).
func (m *Mod) copyWage(ctx context.Context, bpTypeID int64) float64 {
	f, err := m.printFactsFor(ctx, bpTypeID)
	if err != nil {
		return corpCopyWageMin
	}
	share := 1.0
	if f.MaxRuns > 0 {
		share = float64(f.CopyRuns) / float64(f.MaxRuns)
	}
	return math.Max(corpCopyWageMin, math.Floor(corpCopyWageFrac*m.printRefPrice(ctx, nil, bpTypeID)*share))
}

// printBusy is why a print cannot be ordered on, "" when it can: it is being moved, or an order is
// already pinned to it.
func (m *Mod) printBusy(ctx context.Context, corpID, itemID int64) string {
	if m.movingItems(ctx, corpID)[itemID] {
		return "That print is being carried to another base."
	}
	if ws, err := m.queryWork(ctx, `WHERE corporationID = ? AND status = 'open' AND bpItemID = ?`, corpID, itemID); err == nil && len(ws) > 0 {
		return "There is already an order on that print: " + workKindWords(ws[0].Kind) + " it."
	}
	return ""
}

// ── buy orders ──────────────────────────────────────────────────────────────

func writeBidForm(p *mgPage, b *strings.Builder, a corpArm) {
	off, n := p.m.officeSelect(p.ctx, "station", p.corpID, p.cur("bid", "station", ""), "")
	if n == 0 {
		p.noOffice(b)
		return
	}
	b.WriteString(p.open("bid") + "<br>")
	b.WriteString(p.line("What to buy", "item", p.cur("bid", "item", ""), 30, hintItem, "item"))
	b.WriteString(p.line("How many", "qty", p.cur("bid", "qty", ""), 10, "", ""))
	b.WriteString(p.line("Highest price each (ISK)", "price", p.cur("bid", "price", ""), 12, "The buy order is placed at this price.", ""))
	b.WriteString(p.lineHTML("Delivered at", off, "The office the goods are brought to."))
	b.WriteString(p.submit("Preview buy order"))
}

func parseBidForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	id, name, bad := p.typeField("bid")
	if bad != "" {
		return no(bad)
	}
	if a == armMining && p.m.typeKinds(p.ctx, []int64{id})[id].Category != 25 {
		return no("Mining buys ore and ice. Other goods are Industry's or Sales'.")
	}
	qty, ok := p.count("qty", 2_000_000_000)
	if !ok {
		return no("Give how many.")
	}
	price, ok := p.num("price")
	price = roundBidPrice(price)
	if !ok || price <= 0 {
		return no("Give a price each, in ISK.")
	}
	st, bad := p.office("station")
	if bad != "" {
		return no(bad)
	}
	if p.m.corpBiddingOn(p.ctx, p.corpID)[[2]int64{st, id}] {
		return no("The corporation already has a buy order for that there. Withdraw it first.")
	}
	if why := p.m.afford(p.ctx, p.corpID, price*float64(qty)); why != "" {
		return no(why)
	}
	p.keep(hidden, "item", "qty", "price", "station")
	c := corpCommand{Kind: cmdKindBid, Payload: encodeCommandPayload(cmdBoardPayload{TypeID: id, Qty: qty, Price: price, StationID: st})}
	return one(c, esp.ShipIcon(id, 32)+" "+igbF3(fmt.Sprintf("Bid for <b>%s</b> <b>%s</b> at <b>%s</b> each at %s: <b>%s</b> in escrow.",
		fmtUnits(qty), esp.IGBText(name), iskString(price), esp.IGBText(p.m.stationLabel(p.ctx, st)), iskString(price*float64(qty))))+
		"<br>"+p.pg.dim3("Placed by a bot of the corporation that holds Trader or Director and has an order slot free. It delivers into division 1."))
}

// ── requisitions ────────────────────────────────────────────────────────────

func writeReqForm(p *mgPage, b *strings.Builder, a corpArm) {
	off, n := p.m.officeSelect(p.ctx, "station", p.corpID, p.cur("req", "station", ""), "")
	if n == 0 {
		p.noOffice(b)
		return
	}
	b.WriteString(p.open("req") + "<br>")
	b.WriteString(p.line("What to get", "item", p.cur("req", "item", ""), 30, hintItem, "item"))
	b.WriteString(p.line("How many", "qty", p.cur("req", "qty", ""), 10, "", ""))
	b.WriteString(p.lineHTML("Into this hangar", p.divSelect("flag", a, p.cur("req", "flag", ""), true), "Where the goods end up."))
	b.WriteString(p.lineHTML("At this office", off, ""))
	b.WriteString(p.line("Within how many days", "days", p.cur("req", "days", "4"), 3, "1 to 9.", ""))
	b.WriteString(p.lineHTML("Built or bought", `<select name="buy">`+
		selOpt("", "built if that is cheaper", p.cur("req", "buy", "") == "")+selOpt("1", "bought, never built", p.cur("req", "buy", "") == "1")+`</select>`, ""))
	b.WriteString(p.submit("Preview requisition"))
}

func parseReqForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	id, name, bad := p.typeField("req")
	if bad != "" {
		return no(bad)
	}
	qty, ok := p.count("qty", 2_000_000_000)
	if !ok {
		return no("Give how many.")
	}
	st, bad := p.office("station")
	if bad != "" {
		return no(bad)
	}
	flag, bad := p.division("flag")
	if bad != "" {
		return no(bad)
	}
	if own := armDivisions(p.m.corpDivs(p.ctx, p.corpID), a); !p.scope.CEO && len(own) > 0 {
		mine := false
		for _, d := range own {
			mine = mine || d == flagDiv(flag)
		}
		if !mine {
			return no("That hangar is another division's.")
		}
	}
	days, ok := p.count("days", 9)
	if !ok {
		return no("A requisition is given 1 to 9 days.")
	}
	budget := p.m.reqBudget(p.ctx, id, qty)
	if why := p.m.afford(p.ctx, p.corpID, budget); why != "" {
		return no(why)
	}
	p.keep(hidden, "item", "qty", "station", "flag", "days", "buy")
	pay := cmdBoardPayload{TypeID: id, Qty: qty, StationID: st, Flag: flag, Days: int(days), BuyOnly: p.q.Get("buy") == "1"}
	return one(corpCommand{Kind: cmdKindReq, Payload: encodeCommandPayload(pay)},
		esp.ShipIcon(id, 32)+" "+igbF3(fmt.Sprintf("Get <b>%s</b> <b>%s</b> into %s at %s within <b>%d</b> day(s), spending up to <b>%s</b>.",
			fmtUnits(qty), esp.IGBText(name), esp.IGBText(p.m.divLabelAt(p.ctx, p.corpID, flag)), esp.IGBText(p.m.stationLabel(p.ctx, st)), days, iskString(budget))))
}

// ── hauling ─────────────────────────────────────────────────────────────────

func writeHaulForm(p *mgPage, b *strings.Builder, a corpArm) {
	from, n := p.m.officeSelect(p.ctx, "from", p.corpID, p.cur("haul", "from", ""), "")
	if n == 0 {
		p.noOffice(b)
		return
	}
	to, _ := p.m.officeSelect(p.ctx, "station", p.corpID, p.cur("haul", "station", ""), "")
	b.WriteString(p.open("haul") + "<br>")
	b.WriteString(p.line("What to carry", "item", p.cur("haul", "item", ""), 30, hintItem, "item"))
	b.WriteString(p.line("How many", "qty", p.cur("haul", "qty", ""), 10, "", ""))
	b.WriteString(p.lineHTML("Take it from", p.divSelect("fromflag", a, p.cur("haul", "fromflag", ""), false)+" "+from, "A hangar, at an office."))
	b.WriteString(p.lineHTML("Bring it to", p.divSelect("flag", a, p.cur("haul", "flag", ""), false)+" "+to, "A hangar, at another office."))
	b.WriteString(p.line("Hauler's wage (ISK)", "wage", p.cur("haul", "wage", ""), 12,
		"Leave empty for the going rate for the distance and the load. The Treasury pays it.", ""))
	b.WriteString(p.line("Within how many days", "days", p.cur("haul", "days", "3"), 3, "1 to 7.", ""))
	b.WriteString(p.submit("Preview haul"))
}

func parseHaulForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	id, name, bad := p.typeField("haul")
	if bad != "" {
		return no(bad)
	}
	qty, ok := p.count("qty", 2_000_000_000)
	if !ok {
		return no("Give how many.")
	}
	from, bad := p.office("from")
	if bad != "" {
		return no(bad)
	}
	to, bad := p.office("station")
	if bad != "" {
		return no(bad)
	}
	if from == to {
		return no("Pick two different offices.")
	}
	ff, bad := p.division("fromflag")
	if bad != "" {
		return no(bad)
	}
	tf, bad := p.division("flag")
	if bad != "" {
		return no(bad)
	}
	if own := armDivisions(p.m.corpDivs(p.ctx, p.corpID), a); !p.scope.CEO && len(own) > 0 {
		mine := false
		for _, d := range own {
			mine = mine || d == flagDiv(ff)
		}
		if !mine {
			return no("That hangar is another division's. Haul out of your own.")
		}
	}
	if have := p.m.officeUnits(p.ctx, p.corpID, from, ff, id); have < qty {
		return no("There are " + fmtUnits(have) + " there.")
	}
	days, ok := p.count("days", 7)
	if !ok {
		return no("A haul is given 1 to 7 days.")
	}
	wage := p.m.haulWageBetween(p.ctx, id, qty, from, to)
	if strings.TrimSpace(p.q.Get("wage")) != "" {
		if wage, ok = p.num("wage"); !ok || wage < 1 {
			return no("Give the wage in ISK.")
		}
	}
	if why := p.m.afford(p.ctx, p.corpID, wage); why != "" {
		return no(why)
	}
	p.keep(hidden, "item", "qty", "from", "fromflag", "station", "flag", "wage", "days")
	pay := cmdBoardPayload{TypeID: id, Qty: qty, FromStationID: from, FromFlag: ff, StationID: to, Flag: tf, Wage: wage, Days: int(days)}
	return one(corpCommand{Kind: cmdKindHaul, Payload: encodeCommandPayload(pay)},
		esp.ShipIcon(id, 32)+" "+igbF3(fmt.Sprintf("Carry <b>%s</b> <b>%s</b> from %s at %s to %s at %s for <b>%s</b>.", fmtUnits(qty), esp.IGBText(name),
			esp.IGBText(p.m.divLabelAt(p.ctx, p.corpID, ff)), esp.IGBText(p.m.stationLabel(p.ctx, from)),
			esp.IGBText(p.m.divLabelAt(p.ctx, p.corpID, tf)), esp.IGBText(p.m.stationLabel(p.ctx, to)), iskString(wage)+wageNote(strings.TrimSpace(p.q.Get("wage")) == ""))+
			"<br>"+p.pg.dim3(treasuryPaysLine)))
}

// ── trade runs ──────────────────────────────────────────────────────────────

func writeTradeForm(p *mgPage, b *strings.Builder, a corpArm) {
	off, n := p.m.officeSelect(p.ctx, "station", p.corpID, p.cur("trade", "station", ""), "")
	if n == 0 {
		p.noOffice(b)
		return
	}
	b.WriteString(p.open("trade") + "<br>")
	b.WriteString(p.line("What to buy", "item", p.cur("trade", "item", ""), 30, hintItem, "item"))
	b.WriteString(p.line("How many", "qty", p.cur("trade", "qty", ""), 10, "", ""))
	b.WriteString(p.line("Buy in this system", "system", p.cur("trade", "system", ""), 18, hintSystem+" The cheapest seller there is used.", "system"))
	b.WriteString(p.line("Highest price each (ISK)", "price", p.cur("trade", "price", ""), 12,
		"The hauler pays no more than this per unit. The preview shows what sellers ask now.", ""))
	b.WriteString(p.lineHTML("Bring them to", off, "The Trade division of this office."))
	b.WriteString(p.line("Hauler's wage (ISK)", "wage", p.cur("trade", "wage", ""), 12,
		"Leave empty for the going rate for the distance and the load. The Treasury pays it.", ""))
	b.WriteString(p.submit("Preview trade run"))
}

// bestSeller is the station of a system with the cheapest ask for a type, and that ask.
func (m *Mod) bestSeller(ctx context.Context, systemID, typeID int64) (stationID int64, ask float64) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT o.stationID, o.price FROM market_orders o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE s.solarSystemID = ? AND o.typeID = ? AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		 ORDER BY o.price LIMIT 1`, systemID, typeID).Scan(&stationID, &ask)
	return stationID, ask
}

func parseTradeForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	id, name, bad := p.typeField("trade")
	if bad != "" {
		return no(bad)
	}
	qty, ok := p.count("qty", 2_000_000_000)
	if !ok {
		return no("Give how many.")
	}
	sys, bad := p.systemField()
	if bad != "" {
		return no(bad)
	}
	seller, ask := p.m.bestSeller(p.ctx, sys, id)
	if seller == 0 {
		return no("Nobody sells that in " + p.m.nameOfSystem(p.ctx, sys) + ".")
	}
	price, ok := p.num("price")
	if !ok || price < ask {
		return no(fmt.Sprintf("The cheapest there asks %s.", iskString(ask)))
	}
	st, bad := p.office("station")
	if bad != "" {
		return no(bad)
	}
	if st == seller {
		return no("It is sold where you would bring it: buy it on the market.")
	}
	// An empty wage is the going rate for the load and the distance, as a haul's is; the treasury pays it.
	wage, going := p.m.haulWageBetween(p.ctx, id, qty, seller, st), true
	if strings.TrimSpace(p.q.Get("wage")) != "" {
		if wage, ok = p.num("wage"); !ok || wage < 1 {
			return no("Give the hauler's wage in ISK, or leave it empty for the going rate.")
		}
		going = false
	}
	outlay := price*float64(qty) + wage
	if why := p.m.afford(p.ctx, p.corpID, outlay); why != "" {
		return no(why)
	}
	p.keep(hidden, "item", "qty", "system", "price", "station", "wage")
	pay := cmdBoardPayload{TypeID: id, Qty: qty, Price: price, FromStationID: seller, StationID: st, Wage: wage}
	return one(corpCommand{Kind: cmdKindTrade, Payload: encodeCommandPayload(pay)},
		esp.ShipIcon(id, 32)+" "+igbF3(fmt.Sprintf("Buy <b>%s</b> <b>%s</b> at %s for at most <b>%s</b> each (asked now: %s) and bring them to %s, wage <b>%s</b>%s: up to <b>%s</b>.",
			fmtUnits(qty), esp.IGBText(name), esp.IGBText(p.m.stationLabel(p.ctx, seller)), iskString(price), iskString(ask),
			esp.IGBText(p.m.stationLabel(p.ctx, st)), iskString(wage), wageNote(going), iskString(outlay)))+
			"<br>"+p.pg.dim3(treasuryPaysLine))
}

// ── prints ──────────────────────────────────────────────────────────────────

func writePrintBuyForm(p *mgPage, b *strings.Builder, a corpArm) {
	b.WriteString(p.open("printbuy") + "<br>")
	b.WriteString(p.line("Which blueprint", "item", p.cur("printbuy", "item", ""), 34,
		"Type part of the name. Spelling need not be exact; Find lists what it could be.", "item"))
	b.WriteString(p.line("Highest price (ISK)", "price", p.cur("printbuy", "price", ""), 14, "The most the bot may pay for it.", ""))
	b.WriteString(p.lineHTML("Original or copy", `<select name="copy">`+selOpt("", "an original", p.cur("printbuy", "copy", "") == "")+
		selOpt("1", "an original or a copy", p.cur("printbuy", "copy", "") == "1")+`</select>`, ""))
	b.WriteString(p.lineHTML("Bought by", memberSelect("buyer", p.m.orgMembers(p.ctx, p.corpID), p.cur("printbuy", "buyer", ""), ""), "The bot that goes and buys it."))
	b.WriteString(p.submit("Preview purchase"))
}

func parsePrintBuyForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	var id int64
	name := strings.Join(strings.Fields(p.q.Get("item")), " ")
	_ = p.m.db.QueryRowContext(p.ctx, `
		SELECT bt.blueprintTypeID FROM invBlueprintTypes bt JOIN invTypes t ON t.typeID = bt.blueprintTypeID
		 WHERE t.typeName IN (?, ?) AND t.published = 1 LIMIT 1`, name, name+" Blueprint").Scan(&id)
	if id == 0 || name == "" {
		return no("No blueprint is called \"" + name + "\".")
	}
	price, ok := p.num("price")
	if !ok || price < 1 {
		return no("Give the most it may cost, in ISK.")
	}
	buyer, _ := strconv.ParseInt(p.q.Get("buyer"), 10, 64)
	if buyer == 0 || !p.m.isSynthetic(buyer) || p.m.charCorpID(p.ctx, buyer) != p.corpID {
		return no("Pick the bot that buys it.")
	}
	if n := p.m.openPOs(p.ctx, p.corpID); n >= poMaxOpen {
		return no("A blueprint is already being bought. One at a time.")
	}
	if why := p.m.afford(p.ctx, p.corpID, price); why != "" {
		return no(why)
	}
	// The purchase desk commits only a share of free capital to one print (poWithinCap).
	if c := p.m.deskCEOFor(p.ctx, p.corpID, a); c != nil {
		ch := ceoCharterOf(p.ctx, p.m, c)
		free := p.m.chairFree(p.ctx, p.corpID)
		if budget := math.Ceil(math.Ceil(price/poAdvanceMarkup) * poAdvanceMarkup); !poWithinCap(budget, ch.CapitalRisk, free) {
			return no(fmt.Sprintf("One print may take %s now, %.0f%% of what is free.", iskString(ch.CapitalRisk*free), ch.CapitalRisk*100))
		}
	}
	st, _ := p.m.corpFactoryOffice(p.ctx, p.corpID)
	if st == 0 {
		return no("The corporation has no factory office to bring it to.")
	}
	p.keep(hidden, "item", "price", "copy", "buyer")
	pay := cmdBoardPayload{TypeID: id, Price: price, CharID: buyer, StationID: st, CopyOK: p.q.Get("copy") == "1"}
	return one(corpCommand{Kind: cmdKindPrintBuy, Payload: encodeCommandPayload(pay)},
		esp.ShipIcon(id, 32)+" "+igbF3(fmt.Sprintf("Send <b>%s</b> for a <b>%s</b>, up to <b>%s</b>, to the library at %s.",
			esp.IGBText(p.m.charNameOr(p.ctx, buyer)), esp.IGBText(p.m.typeNameOr(p.ctx, id)), iskString(price), esp.IGBText(p.m.stationLabel(p.ctx, st)))))
}

// openPOs is how many print purchases a corporation has open.
func (m *Mod) openPOs(ctx context.Context, corpID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_corp_po WHERE corporationID = ? AND status = 'open'`, corpID).Scan(&n)
	return n
}

// movablePrints is every print on the shelves, originals and copies.
func (p *mgPage) movablePrints() []libraryCopy {
	var out []libraryCopy
	originals, _ := p.m.libraryPrintsAll(p.ctx, p.corpID)
	for _, o := range originals {
		if o.Flag != 111 {
			out = append(out, libraryCopy{ItemID: o.ItemID, BPTypeID: o.BPTypeID, StationID: o.StationID, Flag: o.Flag, ME: o.ME, PE: o.PE})
		}
	}
	copies, _ := p.m.libraryCopiesAll(p.ctx, p.corpID)
	for _, c := range copies {
		if c.Flag != 111 {
			out = append(out, c)
		}
	}
	return out
}

func (p *mgPage) copySelect(form string, prints []libraryCopy) string {
	type row struct{ v, l string }
	var rows []row
	for _, c := range prints {
		l := p.m.typeNameOr(p.ctx, c.BPTypeID)
		if c.Runs > 0 {
			l += fmt.Sprintf(" (copy, %d runs)", c.Runs)
		} else {
			l += fmt.Sprintf(" (original, ME %d, PE %d)", c.ME, c.PE)
		}
		rows = append(rows, row{strconv.FormatInt(c.ItemID, 10), l + " - " + p.m.stationLabel(p.ctx, c.StationID)})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].l < rows[j].l })
	var b strings.Builder
	b.WriteString(`<select name="print">`)
	cur := p.cur(form, "print", "")
	for _, r := range rows {
		b.WriteString(selOpt(r.v, r.l, cur == r.v))
	}
	b.WriteString(`</select>`)
	return b.String()
}

func findCopy(prints []libraryCopy, raw string) *libraryCopy {
	id, _ := strconv.ParseInt(raw, 10, 64)
	for i := range prints {
		if id != 0 && prints[i].ItemID == id {
			return &prints[i]
		}
	}
	return nil
}

func writePrintMoveForm(p *mgPage, b *strings.Builder, a corpArm) {
	prints := p.movablePrints()
	off, ok := p.officeField("printmove", "to", "station")
	if !ok || len(prints) == 0 {
		b.WriteString(p.pg.dim3("There is no print on the shelves to move.") + "<br>")
		return
	}
	b.WriteString(p.open("printmove") + p.copySelect("printmove", prints) + `<br>` + off + p.submit("Preview move"))
}

func parsePrintMoveForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	if !printMovesOn() {
		return no("Prints are not being carried at the moment.")
	}
	pr := findCopy(p.movablePrints(), p.q.Get("print"))
	if pr == nil {
		return no("Pick a print.")
	}
	to, bad := p.office("station")
	if bad != "" {
		return no(bad)
	}
	if to == pr.StationID {
		return no("It is already there.")
	}
	if why := p.m.printMoveRefusal(p.ctx, p.corpID, pr.ItemID); why != "" {
		return no(why)
	}
	p.keep(hidden, "print", "station")
	pay := cmdBoardPayload{TypeID: pr.BPTypeID, ItemID: pr.ItemID, FromStationID: pr.StationID, StationID: to, IsCopy: pr.Runs > 0}
	return one(corpCommand{Kind: cmdKindMove, Payload: encodeCommandPayload(pay)},
		esp.ShipIcon(pr.BPTypeID, 32)+" "+igbF3(fmt.Sprintf("Carry <b>%s</b> from %s to <b>%s</b>.", esp.IGBText(p.m.typeNameOr(p.ctx, pr.BPTypeID)),
			esp.IGBText(p.m.stationLabel(p.ctx, pr.StationID)), esp.IGBText(p.m.stationLabel(p.ctx, to)))))
}

func writePrintSaleForm(p *mgPage, b *strings.Builder, a corpArm) {
	copies, _ := p.m.libraryCopiesAll(p.ctx, p.corpID)
	if len(copies) == 0 {
		b.WriteString(p.pg.dim3("There is no copy on the shelves to sell.") + "<br>")
		return
	}
	b.WriteString(p.open("printsale") + p.copySelect("printsale", copies) + ` ` + p.submit("Preview sale"))
}

func parsePrintSaleForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	copies, _ := p.m.libraryCopiesAll(p.ctx, p.corpID)
	pr := findCopy(copies, p.q.Get("print"))
	if pr == nil {
		return no("Pick a copy.")
	}
	if why := p.m.printBusy(p.ctx, p.corpID, pr.ItemID); why != "" {
		return no(why)
	}
	p.keep(hidden, "print")
	pay := cmdBoardPayload{TypeID: pr.BPTypeID, ItemID: pr.ItemID, StationID: pr.StationID, IsCopy: true}
	return one(corpCommand{Kind: cmdKindSale, Payload: encodeCommandPayload(pay)},
		esp.ShipIcon(pr.BPTypeID, 32)+" "+igbF3(fmt.Sprintf("Put a %d-run copy of <b>%s</b> up for sale at %s.", pr.Runs,
			esp.IGBText(p.m.typeNameOr(p.ctx, pr.BPTypeID)), esp.IGBText(p.m.stationLabel(p.ctx, pr.StationID)))))
}

// ── the rest ────────────────────────────────────────────────────────────────

func writeSurveyForm(p *mgPage, b *strings.Builder, a corpArm) {
	b.WriteString(p.open("survey") + "<br>" + p.line("Which system", "system", p.cur("survey", "system", ""), 20, hintSystem, "system") + p.submit("Preview survey"))
}

func parseSurveyForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	if !StarbasePower() {
		return no("Starbase work is switched off on this server.")
	}
	sys, bad := p.systemField()
	if bad != "" {
		return no(bad)
	}
	var open, moons int
	_ = p.m.db.QueryRowContext(p.ctx, `
		SELECT (SELECT COUNT(*) FROM rolebot_corp_survey WHERE corporationID = ? AND status IN ('open','claimed')),
		       (SELECT COUNT(*) FROM mapDenormalize WHERE groupID = 8 AND solarSystemID = ?)`, p.corpID, sys).Scan(&open, &moons)
	if open >= surveyOpenMax {
		return no("There are already as many surveys out as the corporation runs at once.")
	}
	if moons == 0 {
		return no("There are no moons there.")
	}
	if why := p.m.afford(p.ctx, p.corpID, surveyWageBase*float64(moons)); why != "" {
		return no(why)
	}
	p.keep(hidden, "system")
	return one(corpCommand{Kind: cmdKindSurvey, Payload: encodeCommandPayload(cmdBoardPayload{SystemID: sys})},
		igbF3(fmt.Sprintf("Survey the <b>%d</b> moon(s) of <b>%s</b>, %s a moon.", moons, esp.IGBText(p.m.nameOfSystem(p.ctx, sys)), iskString(surveyWageBase)))+
			"<br>"+p.pg.dim3("The surveyor takes probes from a hangar of the corporation that holds some; with none, requisition them first."))
}

// assignKindsOf is the jobs a division may put on the Assignments board. Pure.
func assignKindsOf(a corpArm) []struct{ v, l string } {
	switch a {
	case armSecurity:
		return []struct{ v, l string }{{assignPatrol, "Patrol: kill pirates"}}
	case armIndustry:
		return []struct{ v, l string }{{assignBuild, "Build runs of an order on the board"}, {assignDeliver, "Deliver goods"}}
	case armSales, armMining:
		return []struct{ v, l string }{{assignDeliver, "Deliver goods"}}
	}
	return nil
}

func writeAssignmentForm(p *mgPage, b *strings.Builder, a corpArm) {
	if !assignmentsOn() {
		b.WriteString(p.pg.dim3("The Assignments board is switched off on this server.") + "<br>")
		return
	}
	for _, k := range assignKindsOf(a) {
		form := p.open("assign") + mgHidden("what", k.v)
		what := p.cur("assign", "what", "")
		mine := what == "" || what == k.v
		cur := func(f string) string {
			if mine {
				return p.cur("assign", f, "")
			}
			return ""
		}
		switch k.v {
		case assignPatrol:
			b.WriteString(form + mgText("Kill", "qty", cur("qty"), 4, "pirates in") + mgText("", "system", cur("system"), 18, "") +
				mgText("for", "reward", cur("reward"), 10, "ISK"))
		case assignDeliver:
			off, ok := p.officeField("assign", "to division 1 at", "station")
			if !ok {
				p.noOffice(b)
				continue
			}
			b.WriteString(form + mgText("Deliver", "qty", cur("qty"), 8, "of") + mgText("", "item", cur("item"), 26, "") + off +
				mgText("for", "reward", cur("reward"), 10, "ISK"))
		case assignBuild:
			work, _ := p.m.openWorkFor(p.ctx, p.corpID)
			var opts strings.Builder
			for _, w := range work {
				if w.Kind == workKindBuild && p.m.workRunsFree(p.ctx, w, 0) > 0 {
					opts.WriteString(selOpt(strconv.FormatInt(w.WorkID, 10), fmt.Sprintf("%s, %d run(s) left", p.m.typeNameOr(p.ctx, w.ProductTypeID), w.RunsLeft()), false))
				}
			}
			if opts.Len() == 0 {
				continue
			}
			b.WriteString(form + mgText("Build", "qty", cur("qty"), 4, "run(s) of") + `<select name="work">` + opts.String() + `</select> ` +
				mgText("for a bonus of", "reward", cur("reward"), 10, "ISK over the wages"))
		}
		b.WriteString(p.submit("Preview assignment"))
	}
}

func parseAssignmentForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	if !assignmentsOn() {
		return no("The Assignments board is switched off on this server.")
	}
	what, ok := p.q.Get("what"), false
	for _, k := range assignKindsOf(a) {
		ok = ok || k.v == what
	}
	if !ok {
		return no("Pick the kind of job.")
	}
	qty, ok := p.count("qty", 2_000_000_000)
	if !ok {
		return no("Give how many.")
	}
	reward, ok := p.num("reward")
	if !ok || reward < 1 {
		return no("Give the reward in ISK.")
	}
	pay := cmdBoardPayload{Assign: what, Qty: qty, Reward: math.Floor(reward)}
	switch what {
	case assignPatrol:
		if qty > 100 {
			return no("A patrol is 1 to 100 kills.")
		}
		var bad string
		if pay.SystemID, bad = p.systemField(); bad != "" {
			return no(bad)
		}
	case assignDeliver:
		id, _, bad := p.typeField("assign")
		if bad != "" {
			return no(bad)
		}
		st, bad := p.office("station")
		if bad != "" {
			return no(bad)
		}
		pay.TypeID, pay.StationID = id, st
	case assignBuild:
		id, _ := strconv.ParseInt(p.q.Get("work"), 10, 64)
		ws, err := p.m.queryWork(p.ctx, `WHERE workID = ? AND corporationID = ? AND status = 'open' AND kind = 'build'`, id, p.corpID)
		if err != nil || len(ws) == 0 {
			return no("That order is no longer on the board.")
		}
		if int64(p.m.workRunsFree(p.ctx, ws[0], 0)) < qty {
			return no(fmt.Sprintf("%d run(s) of it are free.", p.m.workRunsFree(p.ctx, ws[0], 0)))
		}
		pay.WorkID, pay.TypeID, pay.StationID = id, ws[0].ProductTypeID, ws[0].StationID
	}
	if why := p.m.afford(p.ctx, p.corpID, pay.Reward); why != "" {
		return no(why)
	}
	p.keep(hidden, "what", "qty", "reward", "system", "item", "station", "work")
	c := corpCommand{Kind: cmdKindAssign, Payload: encodeCommandPayload(pay)}
	sum, _ := c.boardSummary(p.m, p.ctx)
	return one(c, igbF3("Post an assignment: <b>"+esp.IGBText(sum)+"</b>.")+"<br>"+
		p.pg.dim3("It stands on the board for two days, and whoever takes it has the board's usual time to do it."))
}

func writeInviteForm(p *mgPage, b *strings.Builder, a corpArm) {
	b.WriteString(p.open("invite") + mgText("Pilot:", "pilot", p.cur("invite", "pilot", ""), 24, "") + p.submit("Preview invitation"))
}

func parseInviteForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	var id, corpID int64
	name := strings.Join(strings.Fields(p.q.Get("pilot")), " ")
	if name != "" {
		_ = p.m.db.QueryRowContext(p.ctx, `
			SELECT characterID, corporationID FROM character_ WHERE characterName = ? AND accountID > 0 LIMIT 1`, name).Scan(&id, &corpID)
	}
	switch {
	case id == 0:
		return no("No pilot is called \"" + name + "\".")
	case corpID == p.corpID:
		return no("They are already a member.")
	case !p.m.isSynthetic(id):
		return no("That is a capsuleer. Write to them, or send the application window's link.")
	}
	if _, managed := managedCEOOf(id); managed {
		return no("That pilot runs a corporation of its own.")
	}
	p.keep(hidden, "pilot")
	return one(corpCommand{Kind: cmdKindInvite, Payload: encodeCommandPayload(cmdBoardPayload{CharID: id})},
		igbF3("Invite <b>"+esp.IGBText(p.m.charNameOr(p.ctx, id))+"</b> to join.")+"<br>"+
			p.pg.dim3("The invitation stands for a week. An applicant still needs your yes in the corporation window."))
}

func writeFactoryForm(p *mgPage, b *strings.Builder, a corpArm) {
	off, ok := p.officeField("factory", "Build at", "station")
	if !ok {
		p.noOffice(b)
		return
	}
	if st := p.m.corpFactoryStation(p.ctx, p.corpID); st != 0 {
		b.WriteString(p.pg.dim3("Now: "+esp.IGBText(p.m.stationLabel(p.ctx, st))) + "<br>")
	}
	b.WriteString(p.open("factory") + off + p.submit("Preview factory"))
}

func parseFactoryForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	st, bad := p.office("station")
	if bad != "" {
		return no(bad)
	}
	if !p.m.stationHasLine(p.ctx, st) {
		return no("That station has no assembly lines.")
	}
	old := p.m.corpFactoryStation(p.ctx, p.corpID)
	if old == st {
		return no("It is already there.")
	}
	p.keep(hidden, "station")
	note := "Members build there from now on."
	if old != 0 {
		note = "The stock at " + esp.IGBText(p.m.stationLabel(p.ctx, old)) + " is carried after it by a Director or the Industry deputy, and the corporation's bids there are withdrawn."
	}
	return one(corpCommand{Kind: cmdKindFactory, Payload: encodeCommandPayload(cmdBoardPayload{StationID: st, FromStationID: old})},
		igbF3("Make <b>"+esp.IGBText(p.m.stationLabel(p.ctx, st))+"</b> the factory.")+"<br>"+p.pg.dim3(note))
}
