package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gurimoon/esp"
)

// /corp/orders (also /ig/corporders): Orders, where a human CEO or Director gives the corporation's bots
// something to do: a fleet to raise, a directive to steer by, work for the Production Board. Every order is a
// row (corpcommands.go) the order desk carries out (corpcommanddesk.go) and the Operations page reports on.
// Identity is the portal key (portalkey.go), pasted into every form; rank is read from the database on every
// request. Pages are GET forms, each outside any table, with a preview step before anything is filed, in the
// SCC order-ticket shape (esp/igbexchange.go), which the in-game browser renderer survives.

const ordersPath = "/corp/orders"

var fleetOrderSizes = []int{3, 4, 5, 6, 8, 10, 12}
var fleetOrderDurations = []int{30, 60, 90, 120, 180, 240, 360, 480}

// handleIGBCorpOrders serves /corp/orders and /ig/corporders.
func (m *Mod) handleIGBCorpOrders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "orders", igbF3("Open this page from your own terminal."))
		return
	}
	pg := m.igbPageFor(r)
	q := r.URL.Query()
	officer := charID != 0 && !m.isSynthetic(charID) && m.isCorpOfficer(ctx, corpID, charID)

	var top strings.Builder
	switch {
	case !CorpOrders():
		top.WriteString(pg.ordersBanner("Orders are not being taken at the moment.", true))
	case q.Get("key") == "request":
		switch {
		case charID == 0 || m.isSynthetic(charID):
			top.WriteString(pg.ordersBanner("Keys are issued to capsuleers at their own terminal.", true))
		case !officer:
			top.WriteString(pg.ordersBanner("Keys are issued to the chair — the CEO and the Directors.", true))
		default:
			top.WriteString(pg.ordersBanner(m.mailPortalKey(ctx, charID, corpID), false))
		}
	case q.Get("form") != "":
		top.WriteString(m.ordersSubmit(ctx, pg, charID, corpID, officer, q))
	}
	esp.WriteCorpPortalPage(w, r, "orders", top.String()+m.ordersBody(ctx, pg, charID, corpID, officer, q))
}

func (p igbPage) ordersBanner(text string, bad bool) string {
	color := p.pal.Accent
	if bad {
		color = p.pal.Red
	}
	return "<table width=100% bgcolor=" + p.pal.Panel + "><tr><td>" + p.tint(color, esp.IGBText(text)) + "</td></tr></table><br>\n"
}

// ── the page ────────────────────────────────────────────────────────────────

func (m *Mod) ordersBody(ctx context.Context, pg igbPage, charID, corpID int64, officer bool, q url.Values) string {
	var b strings.Builder
	host := esp.IGBHost()

	// Who may order, and the key.
	if !officer {
		b.WriteString(igbF3("Orders are given by the chair — the CEO and the Directors of the corporation. " +
			"What they have ordered, and what became of it, is on the " +
			`<a href="` + host + `/corp/ops">Operations</a> page.`))
		b.WriteString("<br><br>\n")
		m.writeOpenOrdersPanel(ctx, &b, pg, corpID, charID, officer)
		return b.String()
	}
	hat := m.officerHat(ctx, corpID, charID)
	if m.hasPortalKey(ctx, charID, corpID) {
		b.WriteString(igbF3(fmt.Sprintf("You hold the chair as <b>%s</b>. Every order below takes your portal key; "+
			`<a href="%s%s?key=request">request a new key</a> if you have lost it (the old one stops working).`, hat, host, ordersPath)))
	} else {
		b.WriteString(igbF3(fmt.Sprintf("You hold the chair as <b>%s</b>. Orders take a portal key, mailed to you by the corporation: "+
			`<a href="%s%s?key=request">request your key</a>.`, hat, host, ordersPath)))
	}
	b.WriteString("<br><br>\n")

	m.writeOpenOrdersPanel(ctx, &b, pg, corpID, charID, officer)
	b.WriteString("<br>\n")

	// The forms. Each is closed before the next band opens; none sits in a
	// cell.
	esp.Section(&b, pg.pal, "Raise a fleet")
	b.WriteString(pg.dim3("A commander of the corporation musters the fleet at their home station and takes it where you say. "+
		"One standing order per kind of fleet: a new one replaces the old.") + "<br>")
	b.WriteString(m.fleetOrderForm(ctx, pg, corpID, q))
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "Set the directive")
	b.WriteString(pg.dim3("Where the corporation's effort goes. Shares are relative; the push is how hard members lean toward it. "+
		"Held for the days you say, then the chair's own leaning returns.") + "<br>")
	b.WriteString(m.directiveOrderForm(pg, q))
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "Post work")
	b.WriteString(pg.dim3("Paid runs of a print the library holds at the factory office, delivered to Production. "+
		"Wages are paid on delivery out of the corporation's wallet.") + "<br>")
	b.WriteString(m.workOrderForm(ctx, pg, corpID, q))
	b.WriteString("<br>\n")
	m.writeOrgForms(ctx, &b, pg, corpID, q)
	return b.String()
}

// writeOpenOrdersPanel is the chair's own orders, with a cancel link each.
func (m *Mod) writeOpenOrdersPanel(ctx context.Context, b *strings.Builder, pg igbPage, corpID, viewer int64, officer bool) {
	p := esp.Panel{Title: "Orders in hand", Align: []string{"", "", "", "right"}, Width: []string{"30", "", "150", "90"},
		Empty: "Nothing is on order."}
	for _, c := range m.listCorpCommands(ctx, corpID, 12) {
		if !c.Open() {
			continue
		}
		icon, name := m.pilotCells(ctx, c.PosterID, 24)
		what := igbF3("<b>"+esp.IGBText(c.Title(m, ctx))+"</b>") + "<br>" + pg.dim(esp.IGBText(c.Summary(m, ctx))) +
			"<br>" + pg.dim(esp.IGBText(name)+", "+c.PosterHat)
		status := pg.tint(commandColor(pg, c.Status), c.Status)
		if c.Result != "" {
			status += "<br>" + pg.dim(esp.IGBText(c.Result))
		}
		cancel := ""
		if officer && c.Kind != cmdKindChat {
			cancel = igbF3(`<a href="` + esp.IGBHost() + ordersPath + `?form=cancel&id=` + strconv.FormatInt(c.CommandID, 10) + `">cancel</a>`)
		}
		p.Rows = append(p.Rows, []string{icon, what, status, cancel})
		p.Shades = append(p.Shades, commandShade(c.Status))
	}
	esp.PanelGrid(b, pg.pal, [][]esp.Panel{{p}})
}

// ── forms ───────────────────────────────────────────────────────────────────

func selOpt(value, label string, selected bool) string {
	s := ""
	if selected {
		s = " selected"
	}
	return `<option value="` + esp.IGBText(value) + `"` + s + `>` + esp.IGBText(label) + `</option>`
}

func keyField(q url.Values) string {
	return `<font size=3>Portal key: </font><input type="text" name="key" size="22" value="` + attrEsc(q.Get("key")) + `"><br>`
}

func attrEsc(s string) string {
	return strings.NewReplacer(`&`, "&amp;", `"`, "&quot;", `<`, "&lt;", `>`, "&gt;").Replace(s)
}

func (m *Mod) fleetOrderForm(ctx context.Context, pg igbPage, corpID int64, q url.Values) string {
	cur := func(k string) string {
		if q.Get("form") == "fleet" {
			return q.Get(k)
		}
		return ""
	}
	var b strings.Builder
	b.WriteString(`<form action="` + esp.IGBHost() + ordersPath + `" method="get"><input type="hidden" name="form" value="fleet">`)
	b.WriteString(`<font size=3>Fleet: </font><select name="activity">`)
	for _, a := range []struct{ slug, label string }{{ActMining, "Mining"}, {ActCombat, "Hunting pack"}, {ActSecurity, "Security patrol"}} {
		b.WriteString(selOpt(a.slug, a.label, cur("activity") == a.slug))
	}
	b.WriteString(`</select><br>`)
	b.WriteString(`<font size=3>System: </font><input type="text" name="system" size="24" value="` + attrEsc(cur("system")) + `"><br>`)
	b.WriteString(`<font size=3>Pilots: </font><select name="size">`)
	for _, n := range fleetOrderSizes {
		b.WriteString(selOpt(strconv.Itoa(n), strconv.Itoa(n), cur("size") == strconv.Itoa(n) || (cur("size") == "" && n == 5)))
	}
	b.WriteString(`</select> <font size=3>for </font><select name="duration">`)
	for _, d := range fleetOrderDurations {
		b.WriteString(selOpt(strconv.Itoa(d), durationWords(d), cur("duration") == strconv.Itoa(d) || (cur("duration") == "" && d == 120)))
	}
	b.WriteString(`</select><br>`)
	b.WriteString(`<font size=3>Ore (mining, optional): </font><input type="text" name="ore" size="20" value="` + attrEsc(cur("ore")) + `"><br>`)
	b.WriteString(keyField(q))
	b.WriteString(`<input type="submit" value="Preview fleet order"></form>`)
	return b.String()
}

func (m *Mod) directiveOrderForm(pg igbPage, q url.Values) string {
	cur := func(k, def string) string {
		if q.Get("form") == "directive" && q.Get(k) != "" {
			return q.Get(k)
		}
		return def
	}
	var b strings.Builder
	b.WriteString(`<form action="` + esp.IGBHost() + ordersPath + `" method="get"><input type="hidden" name="form" value="directive">`)
	for _, a := range directiveActivities() {
		b.WriteString(`<font size=3>` + esp.IGBText(a.Label) + `: </font><input type="text" name="w_` + a.Slug + `" size="4" value="` +
			attrEsc(cur("w_"+a.Slug, "")) + `"><font size=2> %</font>&nbsp;&nbsp; `)
	}
	b.WriteString(`<br><font size=3>Push: </font><select name="push">`)
	for _, p := range []struct{ v, l string }{{"gentle", "gentle"}, {"firm", "firm"}, {"hard", "hard"}} {
		b.WriteString(selOpt(p.v, p.l, cur("push", "firm") == p.v))
	}
	b.WriteString(`</select> <font size=3>held for </font><input type="text" name="days" size="3" value="` +
		attrEsc(cur("days", "7")) + `"><font size=3> day(s)</font><br>`)
	b.WriteString(keyField(q))
	b.WriteString(`<input type="submit" value="Preview directive"></form>`)
	return b.String()
}

// directiveActivities is what the chair may weigh: the catalogue's own
// careers, not its tactics.
func directiveActivities() []ActivityDef {
	var out []ActivityDef
	for _, a := range activityCatalog {
		if a.Tactic {
			continue
		}
		out = append(out, a)
	}
	return out
}

func (m *Mod) workOrderForm(ctx context.Context, pg igbPage, corpID int64, q url.Values) string {
	cur := func(k string) string {
		if q.Get("form") == "work" {
			return q.Get(k)
		}
		return ""
	}
	stationID, _ := m.corpFactoryOffice(ctx, corpID)
	if stationID == 0 {
		return pg.dim3("The corporation has no factory office yet — rent an office at a station with assembly lines and put prints in its Blueprint Library division.")
	}
	prints, _ := m.libraryPrints(ctx, corpID, stationID)
	seen := map[int64]bool{}
	var opts []libraryPrint
	for _, p := range prints {
		if seen[p.BPTypeID] {
			continue
		}
		seen[p.BPTypeID] = true
		opts = append(opts, p)
	}
	if len(opts) == 0 {
		return pg.dim3("The library at " + esp.IGBText(m.stationLabel(ctx, stationID)) + " holds no originals to build from.")
	}
	names := map[int64]string{}
	for _, p := range opts {
		names[p.BPTypeID] = m.typeNameOr(ctx, p.BPTypeID)
	}
	sort.Slice(opts, func(i, j int) bool { return names[opts[i].BPTypeID] < names[opts[j].BPTypeID] })
	var b strings.Builder
	b.WriteString(`<form action="` + esp.IGBHost() + ordersPath + `" method="get"><input type="hidden" name="form" value="work">`)
	b.WriteString(`<font size=3>Print at ` + esp.IGBText(m.stationLabel(ctx, stationID)) + `: </font><select name="bp">`)
	for _, p := range opts {
		label := names[p.BPTypeID]
		if p.ME != 0 || p.PE != 0 {
			label += fmt.Sprintf(" (ME %d, PE %d)", p.ME, p.PE)
		}
		b.WriteString(selOpt(strconv.FormatInt(p.BPTypeID, 10), label, cur("bp") == strconv.FormatInt(p.BPTypeID, 10)))
	}
	b.WriteString(`</select><br>`)
	b.WriteString(`<font size=3>Runs: </font><input type="text" name="runs" size="4" value="` + attrEsc(cur("runs")) + `">`)
	b.WriteString(` <font size=3>Wage per run (ISK): </font><input type="text" name="wage" size="10" value="` + attrEsc(cur("wage")) + `"><br>`)
	b.WriteString(`<font size=3>Note: </font><input type="text" name="note" size="40" value="` + attrEsc(cur("note")) + `"><br>`)
	b.WriteString(keyField(q))
	b.WriteString(`<input type="submit" value="Preview work order"></form>`)
	return b.String()
}

// ── submission ──────────────────────────────────────────────────────────────

// ordersSubmit validates a form, shows its preview, and on confirm files it.
// Returns the fragment for the top of the page.
func (m *Mod) ordersSubmit(ctx context.Context, pg igbPage, charID, corpID int64, officer bool, q url.Values) string {
	if !officer {
		return pg.ordersBanner("Orders are given by the chair — the CEO and the Directors.", true)
	}
	key := strings.TrimSpace(q.Get("key"))
	if key == "" {
		return pg.ordersBanner("Paste your portal key into the form.", true)
	}
	if err := m.verifyPortalKey(ctx, charID, corpID, key); err != nil {
		return pg.ordersBanner(err.Error()+".", true)
	}
	var (
		cmd     corpCommand
		preview string
		hidden  = map[string]string{"form": q.Get("form"), "key": key, "confirm": "1"}
		bad     string
	)
	switch q.Get("form") {
	case "fleet":
		cmd, preview, bad = m.parseFleetOrder(ctx, pg, corpID, q, hidden)
	case "directive":
		cmd, preview, bad = m.parseDirectiveOrder(ctx, pg, q, hidden)
	case "work":
		cmd, preview, bad = m.parseWorkOrder(ctx, pg, corpID, q, hidden)
	case "cancel":
		return m.cancelOrder(ctx, pg, charID, corpID, q)
	default:
		var known bool
		if cmd, preview, bad, known = m.parseOrgOrder(ctx, pg, corpID, q, hidden); !known {
			return pg.ordersBanner("That is not a form this page knows.", true)
		}
	}
	if bad != "" {
		return pg.ordersBanner(bad, true)
	}
	if q.Get("confirm") != "1" {
		var b strings.Builder
		esp.Section(&b, pg.pal, "Preview")
		b.WriteString(preview)
		b.WriteString(`<form action="` + esp.IGBHost() + ordersPath + `" method="get">`)
		keys := make([]string, 0, len(hidden))
		for k := range hidden {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(`<input type="hidden" name="` + k + `" value="` + attrEsc(hidden[k]) + `">`)
		}
		b.WriteString(`<input type="submit" value="Give the order"></form><br>`)
		return b.String()
	}
	cmd.CorpID, cmd.PosterID = corpID, charID
	cmd.PosterHat = m.officerHat(ctx, corpID, charID)
	if cmd.ExpiresAt == 0 {
		cmd.ExpiresAt = clock.Unix() + orderTTLHours.Load()*3600
	}
	id, err := m.postCorpCommand(ctx, cmd)
	if err != nil {
		rlogf("rolebots: post order: %v", err)
		return pg.ordersBanner("The order could not be filed. Try again shortly.", true)
	}
	m.logBot(0, logInfo, "order", "%s (%s) posted %s #%d for corporation %d: %s",
		m.charNameOr(ctx, charID), cmd.PosterHat, cmd.Kind, id, corpID, cmd.Summary(m, ctx))
	return pg.ordersBanner("Order given. The desk takes it up within a minute; watch the Operations page.", false)
}

func (m *Mod) parseFleetOrder(ctx context.Context, pg igbPage, corpID int64, q url.Values, hidden map[string]string) (corpCommand, string, string) {
	act := q.Get("activity")
	switch act {
	case ActMining, ActCombat, ActSecurity:
	default:
		return corpCommand{}, "", "Pick the kind of fleet."
	}
	sysName := strings.TrimSpace(q.Get("system"))
	sysID := m.systemByName(ctx, sysName)
	if sysID == 0 {
		return corpCommand{}, "", "No solar system called \"" + sysName + "\"."
	}
	sysName = m.nameOfSystem(ctx, sysID)
	size, _ := strconv.Atoi(q.Get("size"))
	if size < gangMinMembers || size > 12 {
		return corpCommand{}, "", "A fleet is 3 to 12 pilots."
	}
	dur, _ := strconv.Atoi(q.Get("duration"))
	if dur < 30 || dur > 480 {
		return corpCommand{}, "", "A fleet is out for 30 minutes to 8 hours."
	}
	open := 0
	for _, c := range m.listCorpCommands(ctx, corpID, 50) {
		if c.Kind == cmdKindFleet && c.Open() {
			open++
		}
	}
	if int64(open) >= humanFleetMaxOpen.Load() {
		return corpCommand{}, "", fmt.Sprintf("The corporation already has %d fleet order(s) in hand — wait for one to finish or cancel it.", open)
	}
	p := cmdFleetPayload{Activity: act, SystemID: sysID, SystemName: sysName, Size: size, DurationMin: dur}
	if ore := strings.TrimSpace(q.Get("ore")); ore != "" {
		if act != ActMining {
			return corpCommand{}, "", "Only a mining fleet takes an ore."
		}
		id, name := m.oreNamed(ctx, ore)
		if id == 0 {
			return corpCommand{}, "", "No ore called \"" + ore + "\"."
		}
		p.Ores, p.OreNames = []int64{id}, name
	}
	for _, k := range []string{"activity", "system", "size", "duration", "ore"} {
		hidden[k] = q.Get(k)
	}
	cmd := corpCommand{Kind: cmdKindFleet, Payload: encodeCommandPayload(p), ExpiresAt: clock.Unix() + orderTTLHours.Load()*3600}
	var b strings.Builder
	b.WriteString(igbF3(fmt.Sprintf("Raise a <b>%s</b> fleet of <b>%d</b> pilots to <b>%s</b> %s for <b>%s</b>%s.",
		strings.ToLower(activityLabel(act)), size, sysName, m.systemCell(ctx, sysID), durationWords(dur),
		map[bool]string{true: ", digging " + p.OreNames, false: ""}[p.OreNames != ""])) + "<br>")
	b.WriteString(pg.dim3("A commander with a hull for the seat takes it up within a minute and musters at their home station. "+
		"If nobody can, the order is retried and then refused with the reason.") + "<br><br>")
	return cmd, b.String(), ""
}

func (m *Mod) parseDirectiveOrder(ctx context.Context, pg igbPage, q url.Values, hidden map[string]string) (corpCommand, string, string) {
	weights := map[string]float64{}
	total := 0.0
	for _, a := range directiveActivities() {
		v := strings.TrimSpace(q.Get("w_" + a.Slug))
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return corpCommand{}, "", "Shares are whole percentages."
		}
		if f > 0 {
			weights[a.Slug] = f
			total += f
		}
		hidden["w_"+a.Slug] = v
	}
	if total <= 0 {
		return corpCommand{}, "", "Give at least one activity a share."
	}
	for k := range weights {
		weights[k] /= total
	}
	strength := 1.0
	switch q.Get("push") {
	case "gentle":
		strength = 0.5
	case "hard":
		strength = 1.5
	}
	days, _ := strconv.Atoi(q.Get("days"))
	if days < 1 || int64(days) > directiveHoldMax.Load() {
		return corpCommand{}, "", fmt.Sprintf("A directive is held for 1 to %d day(s).", directiveHoldMax.Load())
	}
	hidden["push"], hidden["days"] = q.Get("push"), q.Get("days")
	p := cmdDirectivePayload{Weights: weights, Strength: strength, HoldDays: days}
	cmd := corpCommand{Kind: cmdKindDirective, Payload: encodeCommandPayload(p), ExpiresAt: clock.Unix() + 3600}
	var b strings.Builder
	b.WriteString(igbF3(fmt.Sprintf("Directive: <b>%s</b>, pushed <b>%s</b>, held <b>%d</b> day(s).",
		esp.IGBText(directiveText(weights)), q.Get("push"), days)) + "<br>")
	b.WriteString(pg.dim3("It takes effect within a minute and replaces whatever the chair had set; the bot CEO, if there is one, leaves it alone until the hold runs out.") + "<br><br>")
	return cmd, b.String(), ""
}

func (m *Mod) parseWorkOrder(ctx context.Context, pg igbPage, corpID int64, q url.Values, hidden map[string]string) (corpCommand, string, string) {
	bp, _ := strconv.ParseInt(q.Get("bp"), 10, 64)
	if bp == 0 {
		return corpCommand{}, "", "Pick a print."
	}
	runs, _ := strconv.Atoi(strings.TrimSpace(q.Get("runs")))
	if runs < 1 || runs > 100 {
		return corpCommand{}, "", "Runs are 1 to 100."
	}
	wage, err := strconv.ParseFloat(strings.TrimSpace(strings.ReplaceAll(q.Get("wage"), ",", "")), 64)
	if err != nil || wage < 1 {
		return corpCommand{}, "", "Give a wage per run, in ISK."
	}
	stationID, _ := m.corpFactoryOffice(ctx, corpID)
	if stationID == 0 {
		return corpCommand{}, "", "The corporation has no factory office."
	}
	var product int64
	_ = m.db.QueryRowContext(ctx, `SELECT productTypeID FROM invBlueprintTypes WHERE blueprintTypeID = ?`, bp).Scan(&product)
	if product == 0 {
		return corpCommand{}, "", "That print builds nothing this board knows."
	}
	var bal float64
	_ = m.db.QueryRowContext(ctx, `SELECT balance FROM corporation WHERE corporationID = ?`, corpID).Scan(&bal)
	cost := wage * float64(runs)
	if cost > bal-m.corpCommitted(ctx, corpID) {
		return corpCommand{}, "", fmt.Sprintf("The wages (%s) are more than the corporation has free (%s).",
			iskString(cost), iskString(bal-m.corpCommitted(ctx, corpID)))
	}
	for _, k := range []string{"bp", "runs", "wage", "note"} {
		hidden[k] = q.Get(k)
	}
	p := cmdWorkPayload{BPTypeID: bp, ProductTypeID: product, StationID: stationID, Runs: runs, WagePerRun: wage, Kind: workKindBuild}
	cmd := corpCommand{Kind: cmdKindWork, Payload: encodeCommandPayload(p), Note: truncate(strings.TrimSpace(q.Get("note")), 255),
		ExpiresAt: clock.Unix() + 3600}
	var b strings.Builder
	b.WriteString(esp.ShipIcon(product, 32) + " " + igbF3(fmt.Sprintf("Build <b>%d</b> run(s) of <b>%s</b> at %s, <b>%s</b> per run — <b>%s</b> in wages, committed now.",
		runs, esp.IGBText(m.typeNameOr(ctx, product)), esp.IGBText(m.stationLabel(ctx, stationID)), iskString(wage), iskString(cost))) + "<br>")
	b.WriteString(pg.dim3("Members holding Factory Manager take it from the Production Board, build from the library's print and the Materials division, and are paid on delivery.") + "<br><br>")
	return cmd, b.String(), ""
}

// cancelOrder withdraws an open order of the chair's.
func (m *Mod) cancelOrder(ctx context.Context, pg igbPage, charID, corpID int64, q url.Values) string {
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	c, err := m.getCorpCommand(ctx, id)
	if err != nil || c == nil || c.CorpID != corpID {
		return pg.ordersBanner("No such order.", true)
	}
	if !c.Open() {
		return pg.ordersBanner("That order is already closed.", true)
	}
	if q.Get("confirm") != "1" {
		var b strings.Builder
		esp.Section(&b, pg.pal, "Cancel")
		b.WriteString(igbF3("Withdraw <b>"+esp.IGBText(c.Title(m, ctx))+"</b> — "+esp.IGBText(c.Summary(m, ctx))+"?") + "<br>")
		if c.Kind == cmdKindFleet && c.RefID != 0 {
			b.WriteString(pg.dim3("The fleet it raised winds down and settles up.") + "<br>")
		}
		b.WriteString(`<form action="` + esp.IGBHost() + ordersPath + `" method="get"><input type="hidden" name="form" value="cancel">` +
			`<input type="hidden" name="id" value="` + strconv.FormatInt(id, 10) + `"><input type="hidden" name="confirm" value="1">` +
			`<input type="hidden" name="key" value="` + attrEsc(q.Get("key")) + `"><input type="submit" value="Withdraw the order"></form><br>`)
		return b.String()
	}
	who := m.charNameOr(ctx, charID)
	switch c.Kind {
	case cmdKindFleet:
		if g, _ := getGang(ctx, m.db, c.RefID); g != nil && c.RefID != 0 {
			if g.State.OpPhase != gangOpWindDown && g.State.OpPhase != gangOpSettled {
				g.State.OpPhase = gangOpWindDown
				g.State.WindDownDeadline = clock.Unix() + WindDownSec()
				g.State.SiteSeq++
				_ = saveGangState(ctx, m.db, g.GangID, &g.State)
				m.narrateFleet(ctx, g.GangID, true, "The chair withdrew the order — winding down.")
			}
		}
		if p, ok := c.fleet(); ok {
			m.clearFleetOrderAny(ctx, corpID, deptForActivity(p.Activity))
		}
	case cmdKindDirective:
		_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_directive WHERE corporationID = ? AND setBy = 'human'`, corpID)
		forgetCorpDirective(corpID)
	case cmdKindWork:
		if c.RefID != 0 {
			m.closeWork(ctx, c.RefID, workVoid)
			forgetWorkDesk(corpID)
		}
	}
	_ = m.setCommandStatus(ctx, id, cmdCancelled, "withdrawn by "+who)
	m.humanLog(ctx, corpID, charID, "cancel", id, 0, "%s withdrew the order: %s", who, c.Title(m, ctx))
	return pg.ordersBanner("Order withdrawn.", false)
}

// systemByName resolves a solar system by name, case-insensitively.
func (m *Mod) systemByName(ctx context.Context, name string) int64 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	var id int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT solarSystemID FROM mapSolarSystems WHERE LOWER(solarSystemName) = LOWER(?) LIMIT 1`, name).Scan(&id)
	return id
}
