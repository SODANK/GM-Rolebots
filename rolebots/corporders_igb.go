package rolebots

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gurimoon/esp"
)

// The Management page's fleet forms, and what withdrawing an order undoes. Every order is a row
// (corpcommands.go) the order desk carries out (corpcommanddesk.go) and the Operations page reports
// on; the page they sit on is corpmanage_igb.go.

var fleetOrderSizes = []int{3, 4, 5, 6, 8, 10, 12}
var fleetOrderDurations = []int{30, 60, 90, 120, 180, 240, 360, 480}

// standingOrderHours is how long a standing order may be held.
var standingOrderHours = []int{6, 12, 24, 48, 72, 168}

func (p igbPage) ordersBanner(text string, bad bool) string {
	color := p.pal.Accent
	if bad {
		color = p.pal.Red
	}
	return "<table width=100% bgcolor=" + p.pal.Panel + "><tr><td>" + p.tint(color, esp.IGBText(text)) + "</td></tr></table><br>\n"
}

// keyAskBanner is the refusal of a page that takes the key itself (Relations, Alliance), with the
// way to a new one.
func (p igbPage) keyAskBanner() string {
	return "<table width=100% bgcolor=" + p.pal.Panel + "><tr><td>" + p.tint(p.pal.Red, "That portal key is not yours, or has lapsed. "+
		`<a href="`+esp.IGBHost()+managePath+`?key=request">Request a key</a> on the Management page.`) + "</td></tr></table><br>\n"
}

func selOpt(value, label string, selected bool) string {
	s := ""
	if selected {
		s = " selected"
	}
	return `<option value="` + esp.IGBText(value) + `"` + s + `>` + esp.IGBText(label) + `</option>`
}

func attrEsc(s string) string {
	return strings.NewReplacer(`&`, "&amp;", `"`, "&quot;", `<`, "&lt;", `>`, "&gt;").Replace(s)
}

// ── fleets ──────────────────────────────────────────────────────────────────

// fleetKinds is the fleets a division raises. Pure.
func fleetKinds(a corpArm) []struct{ slug, label string } {
	switch a {
	case armMining:
		return []struct{ slug, label string }{{ActMining, "Mining"}}
	case armSecurity:
		return []struct{ slug, label string }{{ActCombat, "Hunting pack"}, {ActSecurity, "Security patrol"}}
	}
	return nil
}

func writeFleetForm(p *mgPage, b *strings.Builder, a corpArm) {
	b.WriteString(p.open("fleet"))
	kinds := fleetKinds(a)
	b.WriteString(`<font size=3>Fleet: </font><select name="activity">`)
	for _, k := range kinds {
		b.WriteString(selOpt(k.slug, k.label, p.cur("fleet", "activity", "") == k.slug))
	}
	b.WriteString("</select><br><br>\n")
	if a == armSecurity {
		b.WriteString(p.placeLine("Where to", p.cur("fleet", "place", "")) + p.leaderLine(a, "fleet"))
	} else {
		b.WriteString(p.line("To system", "system", p.cur("fleet", "system", ""), 20, hintSystem, "system"))
	}
	b.WriteString(`<font size=3>Pilots: </font><select name="size">`)
	size := p.cur("fleet", "size", "5")
	for _, n := range fleetOrderSizes {
		b.WriteString(selOpt(strconv.Itoa(n), strconv.Itoa(n), size == strconv.Itoa(n)))
	}
	b.WriteString(`</select> <font size=3>for </font><select name="duration">`)
	dur := p.cur("fleet", "duration", "120")
	for _, d := range fleetOrderDurations {
		b.WriteString(selOpt(strconv.Itoa(d), durationWords(d), dur == strconv.Itoa(d)))
	}
	b.WriteString(`</select><br>`)
	if a == armMining {
		b.WriteString(mgText("Ores, best first (optional):", "ore", p.cur("fleet", "ore", ""), 36, "separate with commas") + `<br>`)
	}
	b.WriteString(p.submit("Preview fleet order"))
}

// parseOres reads a comma-separated ore list.
func (p *mgPage) parseOres(raw string) (ids []int64, names []string, bad string) {
	for _, w := range splitOres(raw) {
		id, name := p.m.oreNamed(p.ctx, w)
		if id == 0 {
			return nil, nil, "No ore called \"" + w + "\"."
		}
		ids, names = append(ids, id), append(names, name)
	}
	return ids, names, ""
}

func parseFleetForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	act, ok := p.q.Get("activity"), false
	for _, k := range fleetKinds(a) {
		ok = ok || k.slug == act
	}
	if !ok {
		return nil, "", "Pick the kind of fleet."
	}
	var place int64
	var where, bad string
	if a == armSecurity {
		place, where, bad = p.placeField()
	} else if place, bad = p.systemField(); bad == "" {
		where = p.m.nameOfSystem(p.ctx, place)
	}
	if bad != "" {
		return nil, "", bad
	}
	size, _ := strconv.Atoi(p.q.Get("size"))
	if size < gangMinMembers || size > 12 {
		return nil, "", "A fleet is 3 to 12 pilots."
	}
	dur, _ := strconv.Atoi(p.q.Get("duration"))
	if dur < 30 || dur > 480 {
		return nil, "", "A fleet is out for 30 minutes to 8 hours."
	}
	// The CEO's fleets are counted apart: a division's two do not stop the CEO raising one.
	open := 0
	for _, c := range p.m.listCorpCommands(p.ctx, p.corpID, 50) {
		if c.Kind == cmdKindFleet && c.Open() && c.PosterCEO == p.scope.CEO && (p.scope.CEO || c.Arm == string(a)) {
			open++
		}
	}
	if int64(open) >= humanFleetMaxOpen.Load() {
		return nil, "", fmt.Sprintf("There are already %d fleet order(s) in hand — wait for one to finish or withdraw it.", open)
	}
	pay := cmdFleetPayload{Activity: act, SystemName: where, Size: size, DurationMin: dur}
	pay.SystemID, pay.AreaID = splitPlace(place)
	led := ""
	if a == armSecurity {
		l, bad := p.leaderField(a)
		if bad != "" {
			return nil, "", bad
		}
		if pay.LeaderID = l.CharID; l.CharID != 0 {
			led = ", led by <b>" + esp.IGBText(l.Name) + "</b>" + fmt.Sprintf(" (Leadership %d)", l.Leadership)
		}
	}
	if raw := p.q.Get("ore"); a == armMining && strings.TrimSpace(raw) != "" {
		ids, names, bad := p.parseOres(raw)
		if bad != "" {
			return nil, "", bad
		}
		pay.Ores, pay.OreNames = ids, strings.Join(names, ",")
	}
	for _, k := range []string{"activity", "system", "place", "leader", "size", "duration", "ore"} {
		hidden[k] = p.q.Get(k)
	}
	cmd := corpCommand{Kind: cmdKindFleet, Payload: encodeCommandPayload(pay), ExpiresAt: clock.Unix() + orderTTLHours.Load()*3600}
	takes := "A commander with a hull for the seat"
	if pay.LeaderID != 0 {
		takes = esp.IGBText(p.m.charNameOr(p.ctx, pay.LeaderID))
	}
	dig := ""
	if pay.OreNames != "" {
		dig = ", digging " + esp.IGBText(strings.ReplaceAll(pay.OreNames, ",", ", "))
	}
	return []corpCommand{cmd}, igbF3(fmt.Sprintf("Raise a <b>%s</b> fleet of <b>%d</b> pilots to <b>%s</b> %s for <b>%s</b>%s%s.",
		strings.ToLower(activityLabel(act)), size, esp.IGBText(where), p.placeNote(place), durationWords(dur), dig, led)) + "<br>" +
		p.pg.dim3(takes+" takes it up within a minute and musters at their home station. "+
			"If that cannot be done, the order is retried and then refused with the reason.") + "<br><br>", ""
}

// ── standing orders ─────────────────────────────────────────────────────────

func writeStandingForm(p *mgPage, b *strings.Builder, a corpArm) {
	b.WriteString(p.open("standing"))
	if a == armMining {
		b.WriteString(mgText("Ores, best first:", "ore", p.cur("standing", "ore", ""), 36, "separate with commas") + `<br>`)
	} else {
		b.WriteString(p.placeLine("Patrol", p.cur("standing", "place", "")))
	}
	b.WriteString(`<font size=3>for </font><select name="hours">`)
	for _, h := range standingOrderHours {
		b.WriteString(selOpt(strconv.Itoa(h), durationWords(h*60), p.cur("standing", "hours", "24") == strconv.Itoa(h)))
	}
	b.WriteString(`</select> ` + p.submit("Preview standing order"))
}

func parseStandingForm(p *mgPage, a corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	hours, _ := strconv.Atoi(p.q.Get("hours"))
	if hours < 1 || hours > 168 {
		return nil, "", "A standing order is held for 1 hour to 7 days."
	}
	pay := cmdStandingPayload{Hours: hours}
	var line string
	if a == armMining {
		_, names, bad := p.parseOres(p.q.Get("ore"))
		if bad != "" {
			return nil, "", bad
		}
		if len(names) == 0 {
			return nil, "", "Name at least one ore."
		}
		pay.Dept, pay.Ores = portOperations, strings.Join(names, ",")
		line = "Mining fleets dig <b>" + esp.IGBText(strings.Join(names, ", ")) + "</b>"
	} else {
		place, where, bad := p.placeField()
		if bad != "" {
			return nil, "", bad
		}
		pay.Dept, pay.SystemName = portSecurity, where
		pay.SystemID, pay.AreaID = splitPlace(place)
		line = "The combat wing works <b>" + esp.IGBText(where) + "</b> " + p.placeNote(place)
	}
	for _, k := range []string{"ore", "place", "hours"} {
		hidden[k] = p.q.Get(k)
	}
	return []corpCommand{{Kind: cmdKindStanding, Payload: encodeCommandPayload(pay)}},
		igbF3(line+" for <b>"+durationWords(hours*60)+"</b>.") + "<br>" +
			p.pg.dim3("Every fleet of the division steers by it as it forms and plans; no fleet is raised for it. It replaces the standing order before it.") + "<br><br>", ""
}

// ── the directive ───────────────────────────────────────────────────────────

func writeDirectiveForm(p *mgPage, b *strings.Builder, _ corpArm) {
	b.WriteString(p.open("directive"))
	for _, a := range directiveActivities() {
		b.WriteString(`<font size=3>` + esp.IGBText(a.Label) + `: </font><input type="text" name="w_` + a.Slug + `" size="4" value="` +
			attrEsc(p.cur("directive", "w_"+a.Slug, "")) + `"><font size=2> %</font>&nbsp;&nbsp; `)
	}
	b.WriteString(`<br><font size=3>Push: </font><select name="push">`)
	for _, x := range []string{"gentle", "firm", "hard"} {
		b.WriteString(selOpt(x, x, p.cur("directive", "push", "firm") == x))
	}
	b.WriteString(`</select>` + holdField(p.cur("directive", "days", ""), "7") + `<br>` + p.submit("Preview directive"))
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

func parseDirectiveForm(p *mgPage, _ corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	weights := map[string]float64{}
	total := 0.0
	for _, a := range directiveActivities() {
		v := strings.TrimSpace(p.q.Get("w_" + a.Slug))
		if v == "" {
			continue
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 1000 {
			return nil, "", "Shares are whole percentages."
		}
		if f > 0 {
			weights[a.Slug] = f
			total += f
		}
		hidden["w_"+a.Slug] = v
	}
	if total <= 0 {
		return nil, "", "Give at least one activity a share."
	}
	for k := range weights {
		weights[k] /= total
	}
	strength := 1.0
	push := p.q.Get("push")
	switch push {
	case "gentle":
		strength = 0.5
	case "hard":
		strength = 1.5
	default:
		push = "firm"
	}
	days, _ := strconv.Atoi(p.q.Get("days"))
	if days < 1 || int64(days) > directiveHoldMax.Load() {
		return nil, "", fmt.Sprintf("A directive is held for 1 to %d day(s).", directiveHoldMax.Load())
	}
	hidden["push"], hidden["days"] = push, p.q.Get("days")
	pay := cmdDirectivePayload{Weights: weights, Strength: strength, HoldDays: days}
	return []corpCommand{{Kind: cmdKindDirective, Payload: encodeCommandPayload(pay)}},
		igbF3(fmt.Sprintf("Directive: <b>%s</b>, pushed <b>%s</b>, held <b>%d</b> day(s).",
			esp.IGBText(directiveText(weights)), push, days)) + "<br>" +
			p.pg.dim3("It takes effect within a minute and replaces whatever stood before.") + "<br><br>", ""
}

// ── withdrawing ─────────────────────────────────────────────────────────────

// withdrawCommand takes an open order back and undoes what it set going. Answers why not, "" when it
// is withdrawn.
func (m *Mod) withdrawCommand(ctx context.Context, c corpCommand, byID int64) string {
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
		if p, ok := c.fleet(); ok && c.Status != cmdPending {
			m.clearFleetOrderOf(ctx, c.CorpID, deptForActivity(p.Activity), c.PosterID, p.SystemID, p.AreaID, p.OreNames)
		}
	case cmdKindStanding:
		if p, ok := c.standing(); ok && c.Status != cmdPending {
			m.clearFleetOrderOf(ctx, c.CorpID, p.Dept, c.PosterID, p.SystemID, p.AreaID, p.Ores)
		}
	case cmdKindDirective:
		if c.Status != cmdPending {
			_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_directive WHERE corporationID = ? AND setBy = 'human'`, c.CorpID)
			forgetCorpDirective(c.CorpID)
		}
	case cmdKindWork:
		if c.RefID != 0 {
			m.withdrawWork(ctx, c.RefID)
			forgetWorkDesk(c.CorpID)
		}
	default:
		if c.Status != cmdPending {
			if why := m.undoOrgCommand(ctx, c); why != "" {
				return why
			}
		}
	}
	who := m.charNameOr(ctx, byID)
	_ = m.setCommandStatus(ctx, c.CommandID, cmdCancelled, "withdrawn by "+who)
	m.humanLog(ctx, c.CorpID, byID, "cancel", c.CommandID, 0, "%s withdrew the order: %s", who, c.Title(m, ctx))
	if byID != c.PosterID {
		m.mailPilot(ctx, c.PosterID, byID, "Order withdrawn: "+c.Title(m, ctx),
			who+" withdrew your order: "+esp.IGBText(c.Title(m, ctx))+" — "+esp.IGBText(c.Summary(m, ctx))+".")
	}
	return ""
}

// clearFleetOrderOf withdraws a department's standing order if it is still this one: one that
// replaced it stands.
func (m *Mod) clearFleetOrderOf(ctx context.Context, corpID int64, dept string, postedBy, systemID, areaID int64, ores string) {
	_, _ = m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_fleet_orders
		 WHERE corporationID = ? AND activity = ? AND postedBy = ? AND systemID = ? AND areaID = ? AND oreNames = ? AND source = 'human'`,
		corpID, dept, postedBy, systemID, areaID, truncate(strings.Join(splitOres(ores), ","), 255))
}

// withdrawWork takes a work order back. Jobs already in the lines are finished and paid: the order
// shrinks to them, and closes when they are delivered.
func (m *Mod) withdrawWork(ctx context.Context, workID int64) {
	ws, err := m.queryWork(ctx, `WHERE workID = ? AND status = 'open'`, workID)
	if err != nil || len(ws) == 0 {
		return
	}
	w := ws[0]
	running := m.workClaimedExceptOpen(ctx, workID, 0)
	// A capsuleer's job carries no claim: read what the corporation has running of it.
	var jobs int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(runs), 0) FROM ramJobs
		 WHERE ownerID = ? AND containerID = ? AND activityID = ? AND outputTypeID = ? AND completedStatusID IN (0, 1)
		   AND (? = 0 OR installedItemID = ?)`,
		w.CorpID, w.StationID, workActivity(w.Kind), w.ProductTypeID, w.BPItemID, w.BPItemID).Scan(&jobs)
	running = min(max(running, jobs), w.RunsLeft())
	if running <= 0 {
		m.closeWork(ctx, workID, workVoid)
		return
	}
	wanted := w.RunsDone + running
	if _, err := m.db.ExecContext(ctx, `UPDATE rolebot_corp_work SET runsWanted = ? WHERE workID = ? AND status = 'open'`, wanted, workID); err != nil {
		rlogf("rolebots: withdraw work %d: %v", workID, err)
		return
	}
	if !payrollManaged(w.CorpID) {
		_ = m.openCommit(ctx, w.CorpID, "work", workID, w.WagePerRun*float64(wanted))
	}
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
