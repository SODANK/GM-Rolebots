package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gurimoon/esp"
)

// The Management page's settings (corpcommands_org.go), the CEO's alone: where the corporation's
// effort goes, what it is, its budget, and what its fleets do with their ore. Also the field helpers
// the page's forms share.

func unixTime(unix int64) time.Time { return time.Unix(unix, 0) }

// orgMember is a member the chair may place or name.
type orgMember struct {
	CharID int64
	Name   string
}

// orgMembers is the corporation's active bots, by name.
func (m *Mod) orgMembers(ctx context.Context, corpID int64) []orgMember {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, ch.characterName
		  FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID AND b.enabled = 1
		 WHERE ch.corporationID = ? AND ch.accountID > 0
		 ORDER BY ch.characterName`, corpID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []orgMember
	for rows.Next() {
		var o orgMember
		if rows.Scan(&o.CharID, &o.Name) == nil {
			out = append(out, o)
		}
	}
	return out
}

func memberSelect(name string, members []orgMember, cur string, none string) string {
	var b strings.Builder
	b.WriteString(`<select name="` + name + `">`)
	if none != "" {
		b.WriteString(selOpt("", none, cur == ""))
	}
	for _, o := range members {
		v := strconv.FormatInt(o.CharID, 10)
		b.WriteString(selOpt(v, o.Name, cur == v))
	}
	b.WriteString(`</select>`)
	return b.String()
}

func (m *Mod) officeSelect(ctx context.Context, name string, corpID int64, cur string, none string) (string, int) {
	offices, _ := m.loadPostOffices(ctx, corpID)
	var b strings.Builder
	b.WriteString(`<select name="` + name + `">`)
	if none != "" {
		b.WriteString(selOpt("", none, cur == ""))
	}
	sort.Slice(offices, func(i, j int) bool { return offices[i].StationID < offices[j].StationID })
	for _, o := range offices {
		v := strconv.FormatInt(o.StationID, 10)
		b.WriteString(selOpt(v, m.stationLabel(ctx, o.StationID), cur == v))
	}
	b.WriteString(`</select>`)
	return b.String(), len(offices)
}

func holdField(cur string, def string) string {
	if cur == "" {
		cur = def
	}
	return ` <font size=3>held for </font><input type="text" name="days" size="3" value="` + attrEsc(cur) + `"><font size=3> day(s)</font>`
}

// parseHold reads a hold's days.
func (p *mgPage) parseHold(hidden map[string]string, def int) (int, string) {
	v := strings.TrimSpace(p.q.Get("days"))
	if v == "" {
		return def, ""
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || int64(n) > directiveHoldMax.Load() {
		return 0, fmt.Sprintf("A setting is held for 1 to %d day(s).", directiveHoldMax.Load())
	}
	hidden["days"] = v
	return n, ""
}

// heldLine says what of a setting is in force.
func (p *mgPage) heldLine(kind string) string {
	if c := p.m.humanHold(p.ctx, p.corpID, kind); c != nil {
		return p.pg.dim3("In force: "+esp.IGBText(orgHoldLine(c, p.m, p.ctx))) + "<br>"
	}
	return ""
}

func (p *mgPage) settingPreview(line, note string) string {
	return igbF3(line) + "<br>" + p.pg.dim3(note) + "<br><br>"
}

// settingForms is the corporation's own settings, on the CEO's Corporation page.
func settingForms() []mgForm {
	return []mgForm{
		{Form: "directive", Icon: uiTile("07_07"), Title: "Set the directive", Short: "where effort goes",
			Note: "Where the corporation's effort goes. Shares are relative; the push is how hard members lean toward it. " +
				"Held for the days you say.",
			Write: writeDirectiveForm, Parse: parseDirectiveForm},
		{Form: "tags", Icon: logoTile, Title: "Identity", Short: "what the corporation is",
			Note:  "What the corporation is, in up to three words the members act on: what its fleets do, what it recruits for, what it builds.",
			Write: writeTagsForm, Parse: parseTagsForm},
		{Form: "budget", Icon: uiTile("07_12"), Title: "Budget", Short: "reserve, stipends, levy",
			Note: "The reserve is months of office rent no division spends below; stipends are the share of the week's surplus handed to members; " +
				"the store markup and the buyback fraction price the company store and the ore bought from members; the levy takes a cut of members' harvest sales.",
			Write: writeBudgetForm, Parse: parseBudgetForm},
		{Form: "hangars", Icon: uiTile("17_01"), Title: "Hangars", Short: "where the war chest stands",
			Note: "Which hangar the Armory, the Fleet Yard and Spoils stand in. Left automatic, the corporation places them where its fleets " +
				"need them; choose one to put it somewhere yourself, or none to go without. Each division's members get the hangar roles " +
				"for the hangars it owns.",
			Write: writeHangarsForm, Parse: parseHangarsForm},
		{Form: "oredrop", Icon: typeTile(17480), Title: "Fleet ore", Short: "where miners deliver",
			Note:  "Where a mining fleet drops its ore: a corporation office, where each miner and hauler is paid for what was delivered, else each miner keeps its own.",
			Write: writeOreDropForm, Parse: parseOreDropForm},
		{Form: "oredesk", Icon: uiTile("17_01"), Title: "Ore desk", Short: "keep it or sell it",
			Note:  "How much of the corporation's ore is kept for production rather than sold.",
			Write: writeOreDeskForm, Parse: parseOreDeskForm},
	}
}

// ── identity ────────────────────────────────────────────────────────────────

func writeTagsForm(p *mgPage, b *strings.Builder, _ corpArm) {
	b.WriteString(p.heldLine(cmdKindTags) + p.open("tags"))
	for i := 1; i <= ceoTagMax; i++ {
		k := "tag" + strconv.Itoa(i)
		b.WriteString(`<select name="` + k + `">` + selOpt("", "—", p.cur("tags", k, "") == ""))
		for _, t := range ceoTagPool {
			b.WriteString(selOpt(t, corpTagLabel(t), p.cur("tags", k, "") == t))
		}
		b.WriteString(`</select> `)
	}
	b.WriteString(holdField(p.cur("tags", "days", ""), "30") + `<br>` + p.submit("Preview identity"))
}

func parseTagsForm(p *mgPage, _ corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	var tags []string
	seen := map[string]bool{}
	for i := 1; i <= ceoTagMax; i++ {
		t := p.q.Get("tag" + strconv.Itoa(i))
		if t == "" || seen[t] {
			continue
		}
		if !containsStr(ceoTagPool, t) {
			return nil, "", "That is not a word the corporation can be."
		}
		seen[t] = true
		tags = append(tags, t)
		hidden["tag"+strconv.Itoa(i)] = t
	}
	if len(tags) == 0 {
		return nil, "", "Pick at least one word."
	}
	d, bad := p.parseHold(hidden, 30)
	if bad != "" {
		return nil, "", bad
	}
	return []corpCommand{{Kind: cmdKindTags, Payload: encodeCommandPayload(cmdTagsPayload{Tags: tags, HoldDays: d})}},
		p.settingPreview(fmt.Sprintf("Declare the corporation a <b>%s</b> outfit, held %d day(s).", esp.IGBText(strings.Join(tags, " / ")), d),
			"Fleets, recruiting and the desks read these."), ""
}

// ── the budget ──────────────────────────────────────────────────────────────

func writeBudgetForm(p *mgPage, b *strings.Builder, _ corpArm) {
	b.WriteString(p.heldLine(cmdKindBudget) + p.open("budget"))
	b.WriteString(mgText("Reserve:", "reserve", p.cur("budget", "reserve", ""), 4, "months"))
	b.WriteString(mgText("Stipends:", "stipend", p.cur("budget", "stipend", ""), 3, "%"))
	b.WriteString(mgText("Store: cost +", "store", p.cur("budget", "store", ""), 3, "%"))
	b.WriteString(mgText("Buyback:", "buyback", p.cur("budget", "buyback", ""), 3, "% of value"))
	b.WriteString(`<font size=3>Levy: </font><select name="levy">` + selOpt("", "off", p.cur("budget", "levy", "") == "") +
		selOpt("1", "on", p.cur("budget", "levy", "") == "1") + `</select>`)
	b.WriteString(holdField(p.cur("budget", "days", ""), "30") + `<br>` + p.submit("Preview budget"))
}

func parseBudgetForm(p *mgPage, _ corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	bad := ""
	num := func(field string, def, most float64) float64 {
		v := strings.TrimSpace(p.q.Get(field))
		if v == "" {
			return def
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f >= 0 && f <= most) {
			bad = "Stipends and buyback are percentages, store markup up to 200%, reserve up to 36 months."
			return 0
		}
		hidden[field] = v
		return f
	}
	reserve, stipend := num("reserve", 3, 36), num("stipend", 10, 100)
	store, buyback := num("store", 5, 200), num("buyback", 85, 100)
	if bad != "" {
		return nil, "", bad
	}
	d, bad := p.parseHold(hidden, 30)
	if bad != "" {
		return nil, "", bad
	}
	hidden["levy"] = p.q.Get("levy")
	pay := cmdBudgetPayload{ReserveMonths: reserve, StipendShare: stipend / 100, StoreMarkup: store / 100,
		BuybackFrac: buyback / 100, LevyOn: p.q.Get("levy") == "1", HoldDays: d}
	cmd := corpCommand{Kind: cmdKindBudget, Payload: encodeCommandPayload(pay)}
	return []corpCommand{cmd}, p.settingPreview("Budget: <b>"+esp.IGBText(cmd.orgSummary(p.m, p.ctx))+"</b>.",
		"The divisions, the store, the buyback and the levy read these for the hold."), ""
}

// ── ore ─────────────────────────────────────────────────────────────────────

func writeOreDropForm(p *mgPage, b *strings.Builder, _ corpArm) {
	sel, _ := p.m.officeSelect(p.ctx, "station", p.corpID, p.cur("oredrop", "station", ""), "fleets keep their own ore")
	b.WriteString(p.heldLine(cmdKindOreDrop) + p.open("oredrop") + `<font size=3>Fleet ore to: </font>` + sel +
		holdField(p.cur("oredrop", "days", ""), "30") + `<br>` + p.submit("Preview drop-off"))
}

func parseOreDropForm(p *mgPage, _ corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	st, _ := strconv.ParseInt(p.q.Get("station"), 10, 64)
	if st != 0 && !p.m.corpHasOfficeAt(p.ctx, p.corpID, st) {
		return nil, "", "Pick one of the corporation's offices."
	}
	hidden["station"] = p.q.Get("station")
	d, bad := p.parseHold(hidden, 30)
	if bad != "" {
		return nil, "", bad
	}
	cmd := corpCommand{Kind: cmdKindOreDrop, Payload: encodeCommandPayload(cmdOreDropPayload{StationID: st, HoldDays: d})}
	return []corpCommand{cmd}, p.settingPreview("Fleet ore: <b>"+esp.IGBText(cmd.orgSummary(p.m, p.ctx))+"</b>.",
		"A mining fleet's haulers deliver into the corporation hangar there; nothing, and each miner keeps its own rock."), ""
}

func writeOreDeskForm(p *mgPage, b *strings.Builder, _ corpArm) {
	b.WriteString(p.heldLine(cmdKindOreDesk) + p.open("oredesk") + `<font size=3>Keep </font><select name="share">`)
	for _, s := range []struct{ v, l string }{{"0", "nothing — sell it all"}, {"25", "a quarter"}, {"50", "half"}, {"75", "three quarters"}, {"100", "all of it for production"}} {
		b.WriteString(selOpt(s.v, s.l, p.cur("oredesk", "share", "50") == s.v))
	}
	b.WriteString(`</select>` + holdField(p.cur("oredesk", "days", ""), "30") + `<br>` + p.submit("Preview ore desk"))
}

func parseOreDeskForm(p *mgPage, _ corpArm, hidden map[string]string) ([]corpCommand, string, string) {
	share, err := strconv.ParseFloat(p.q.Get("share"), 64)
	if err != nil || !(share >= 0 && share <= 100) {
		return nil, "", "Pick how much the desk keeps."
	}
	hidden["share"] = p.q.Get("share")
	d, bad := p.parseHold(hidden, 30)
	if bad != "" {
		return nil, "", bad
	}
	cmd := corpCommand{Kind: cmdKindOreDesk, Payload: encodeCommandPayload(cmdOreDeskPayload{Share: share / 100, HoldDays: d})}
	return []corpCommand{cmd}, p.settingPreview("Ore desk: <b>"+esp.IGBText(cmd.orgSummary(p.m, p.ctx))+"</b>.",
		"The business pass stops moving the share for the hold."), ""
}
