package rolebots

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gurimoon/esp"
)

// The Orders page's organisation forms (corpcommands_org.go): who commands,
// who runs the industry desk, which arm and which office each member is
// in, the corporation's identity, its budget, and what its fleets do with
// their ore. Same shape as the fleet, directive and work forms
// (corporders_igb.go): one GET form each outside any table, a preview, the
// key, a confirm.

func unixTime(unix int64) time.Time { return time.Unix(unix, 0) }

// orgMember is a member the chair may place: a bot of the corporation.
type orgMember struct {
	CharID int64
	Name   string
}

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

// writeOrgForms is the organisation half of the page.
func (m *Mod) writeOrgForms(ctx context.Context, b *strings.Builder, pg igbPage, corpID int64, q url.Values) {
	cur := func(form, k string) string {
		if q.Get("form") == form {
			return q.Get(k)
		}
		return ""
	}
	host := esp.IGBHost()
	open := func(form string) string {
		return `<form action="` + host + ordersPath + `" method="get"><input type="hidden" name="form" value="` + form + `">`
	}
	members := m.orgMembers(ctx, corpID)
	heldLine := func(kind string) string {
		if c := m.humanHold(ctx, corpID, kind); c != nil {
			return pg.dim3("In force: "+esp.IGBText(orgHoldLine(c, m, ctx))) + "<br>"
		}
		return ""
	}

	// Who commands.
	esp.Section(b, pg.pal, "Fleet commanders")
	b.WriteString(pg.dim3("A seated commander raises fleets of that kind on the formation sweep. A seat you give is kept until you release it.") + "<br>")
	b.WriteString(open("seat"))
	b.WriteString(`<font size=3>Member: </font>` + memberSelect("member", members, cur("seat", "member"), "") + ` `)
	b.WriteString(`<font size=3>commands </font><select name="activity">`)
	for _, a := range []struct{ slug, label string }{{ActMining, "Mining"}, {ActCombat, "Hunting"}, {ActSecurity, "Security"}, {ActPiracy, "Raiding"}} {
		b.WriteString(selOpt(a.slug, a.label, cur("seat", "activity") == a.slug))
	}
	b.WriteString(`</select> <font size=3>fleets</font> <select name="release">` +
		selOpt("", "seat", cur("seat", "release") == "") + selOpt("1", "release", cur("seat", "release") == "1") + `</select><br>`)
	b.WriteString(keyField(q) + `<input type="submit" value="Preview seat"></form><br>`)

	// Arms and postings.
	esp.Section(b, pg.pal, "Arms and postings")
	b.WriteString(pg.dim3("An arm is what a member's work leans toward; a posting is the office they live and work at. "+
		"A placement you make is kept by the CEO's own passes.") + "<br>")
	b.WriteString(open("arm"))
	b.WriteString(`<font size=3>Member: </font>` + memberSelect("member", members, cur("arm", "member"), "") + ` `)
	b.WriteString(`<font size=3>arm: </font><select name="arm">` + selOpt("", "the pool (no arm)", cur("arm", "arm") == ""))
	for _, a := range lineArms {
		b.WriteString(selOpt(string(a), armLabel(a), cur("arm", "arm") == string(a)))
	}
	b.WriteString(`</select><br>` + keyField(q) + `<input type="submit" value="Preview arm"></form>`)
	offSel, offices := m.officeSelect(ctx, "station", corpID, cur("posting", "station"), "")
	if offices > 0 {
		b.WriteString(open("posting"))
		b.WriteString(`<font size=3>Member: </font>` + memberSelect("member", members, cur("posting", "member"), "") + ` `)
		b.WriteString(`<font size=3>posted to </font>` + offSel + `<br>` + keyField(q) + `<input type="submit" value="Preview posting"></form>`)
	}
	b.WriteString("<br>")

	// The industry desk.
	esp.Section(b, pg.pal, "Industry desk")
	b.WriteString(heldLine(cmdKindIndustry))
	b.WriteString(pg.dim3(corpIndustryDirector+" runs the Production Board and the library; the deputy stands in.") + "<br>")
	b.WriteString(open("industry"))
	b.WriteString(`<font size=3>Director: </font>` + memberSelect("director", members, cur("industry", "director"), "") + ` `)
	b.WriteString(`<font size=3>deputy: </font>` + memberSelect("deputy", members, cur("industry", "deputy"), "none"))
	b.WriteString(holdField(cur("industry", "days"), "30") + `<br>` + keyField(q) + `<input type="submit" value="Preview desk"></form><br>`)

	// Identity.
	esp.Section(b, pg.pal, "Identity")
	b.WriteString(heldLine(cmdKindTags))
	b.WriteString(pg.dim3("What the corporation is, in up to three words the members act on: what its fleets do, what it recruits for, what it builds.") + "<br>")
	b.WriteString(open("tags"))
	for i := 1; i <= ceoTagMax; i++ {
		k := "tag" + strconv.Itoa(i)
		b.WriteString(`<select name="` + k + `">` + selOpt("", "—", cur("tags", k) == ""))
		for _, t := range ceoTagPool {
			b.WriteString(selOpt(t, corpTagLabel(t), cur("tags", k) == t))
		}
		b.WriteString(`</select> `)
	}
	b.WriteString(holdField(cur("tags", "days"), "30") + `<br>` + keyField(q) + `<input type="submit" value="Preview identity"></form><br>`)

	// The budget.
	esp.Section(b, pg.pal, "Budget")
	b.WriteString(heldLine(cmdKindBudget))
	b.WriteString(pg.dim3("The reserve is months of office rent the corporation never spends below; stipends are the share of the week's surplus handed to members; "+
		"the store markup and the buyback fraction price the company store and the ore it buys from members; the levy takes a cut of members' harvest sales.") + "<br>")
	b.WriteString(open("budget"))
	b.WriteString(`<font size=3>Reserve: </font><input type="text" name="reserve" size="4" value="` + attrEsc(cur("budget", "reserve")) + `"><font size=3> months</font> `)
	b.WriteString(`<font size=3>Stipends: </font><input type="text" name="stipend" size="3" value="` + attrEsc(cur("budget", "stipend")) + `"><font size=3> %</font> `)
	b.WriteString(`<font size=3>Store: cost + </font><input type="text" name="store" size="3" value="` + attrEsc(cur("budget", "store")) + `"><font size=3> %</font> `)
	b.WriteString(`<font size=3>Buyback: </font><input type="text" name="buyback" size="3" value="` + attrEsc(cur("budget", "buyback")) + `"><font size=3> % of value</font> `)
	b.WriteString(`<font size=3>Levy: </font><select name="levy">` + selOpt("", "off", cur("budget", "levy") == "") + selOpt("1", "on", cur("budget", "levy") == "1") + `</select>`)
	b.WriteString(holdField(cur("budget", "days"), "30") + `<br>` + keyField(q) + `<input type="submit" value="Preview budget"></form><br>`)

	// Ore.
	esp.Section(b, pg.pal, "Ore")
	b.WriteString(heldLine(cmdKindOreDrop) + heldLine(cmdKindOreDesk))
	b.WriteString(pg.dim3("Where a mining fleet drops its ore (a corporation office, else each miner keeps its own), and how much of the corporation's ore the desk keeps for production rather than selling.") + "<br>")
	dropSel, _ := m.officeSelect(ctx, "station", corpID, cur("oredrop", "station"), "fleets sell their own ore")
	b.WriteString(open("oredrop"))
	b.WriteString(`<font size=3>Fleet ore to: </font>` + dropSel + holdField(cur("oredrop", "days"), "30") + `<br>` + keyField(q) + `<input type="submit" value="Preview drop-off"></form>`)
	b.WriteString(open("oredesk"))
	b.WriteString(`<font size=3>Ore desk keeps </font><select name="share">`)
	for _, s := range []struct{ v, l string }{{"0", "nothing — sell it all"}, {"25", "a quarter"}, {"50", "half"}, {"75", "three quarters"}, {"100", "all of it for production"}} {
		b.WriteString(selOpt(s.v, s.l, cur("oredesk", "share") == s.v))
	}
	b.WriteString(`</select>` + holdField(cur("oredesk", "days"), "30") + `<br>` + keyField(q) + `<input type="submit" value="Preview ore desk"></form>`)
}

// parseOrgOrder validates one organisation form. Reports whether the form
// was one of these.
func (m *Mod) parseOrgOrder(ctx context.Context, pg igbPage, corpID int64, q url.Values, hidden map[string]string) (corpCommand, string, string, bool) {
	days := func(def int) (int, string) {
		v := strings.TrimSpace(q.Get("days"))
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
	member := func(field string) (int64, string) {
		id, _ := strconv.ParseInt(q.Get(field), 10, 64)
		if id == 0 {
			return 0, "Pick a member."
		}
		if !m.isSynthetic(id) || m.charCorpID(ctx, id) != corpID {
			return 0, "That pilot is not a member the module flies."
		}
		hidden[field] = q.Get(field)
		return id, ""
	}
	preview := func(line, note string) string {
		return igbF3(line) + "<br>" + pg.dim3(note) + "<br><br>"
	}
	ttl := clock.Unix() + 3600

	switch q.Get("form") {
	case "seat":
		id, bad := member("member")
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		act := q.Get("activity")
		switch act {
		case ActMining, ActCombat, ActSecurity, ActPiracy:
		default:
			return corpCommand{}, "", "Pick what the commander raises.", true
		}
		release := q.Get("release") == "1"
		hidden["activity"], hidden["release"] = act, q.Get("release")
		p := cmdSeatPayload{CharID: id, Activity: act, Release: release}
		cmd := corpCommand{Kind: cmdKindSeat, Payload: encodeCommandPayload(p), ExpiresAt: ttl}
		if release {
			return cmd, preview(fmt.Sprintf("Release <b>%s</b> from command.", esp.IGBText(m.charNameOr(ctx, id))),
				"They keep flying; they raise no more fleets."), "", true
		}
		return cmd, preview(fmt.Sprintf("Seat <b>%s</b> as <b>%s</b> fleet commander.", esp.IGBText(m.charNameOr(ctx, id)), strings.ToLower(activityLabel(act))),
			"They raise fleets of that kind on the formation sweep, when they have a hull for the seat and crew to recruit."), "", true

	case "arm":
		id, bad := member("member")
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		arm := q.Get("arm")
		if arm != "" {
			ok := false
			for _, a := range lineArms {
				if string(a) == arm {
					ok = true
				}
			}
			if !ok {
				return corpCommand{}, "", "Pick an arm.", true
			}
		}
		hidden["arm"] = arm
		p := cmdArmPayload{CharID: id, Arm: arm}
		cmd := corpCommand{Kind: cmdKindArm, Payload: encodeCommandPayload(p), ExpiresAt: ttl}
		if arm == "" {
			return cmd, preview(fmt.Sprintf("Release <b>%s</b> to the pool.", esp.IGBText(m.charNameOr(ctx, id))), "The CEO's own pass may place them again."), "", true
		}
		return cmd, preview(fmt.Sprintf("Place <b>%s</b> in <b>%s</b>.", esp.IGBText(m.charNameOr(ctx, id)), armLabel(corpArm(arm))),
			"Their work leans that way from their next plan; the placement is kept until you release it."), "", true

	case "posting":
		id, bad := member("member")
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		st, _ := strconv.ParseInt(q.Get("station"), 10, 64)
		if st == 0 || !m.corpHasOfficeAt(ctx, corpID, st) {
			return corpCommand{}, "", "Pick one of the corporation's offices.", true
		}
		hidden["station"] = q.Get("station")
		p := cmdPostingPayload{CharID: id, StationID: st}
		cmd := corpCommand{Kind: cmdKindPosting, Payload: encodeCommandPayload(p), ExpiresAt: ttl}
		return cmd, preview(fmt.Sprintf("Post <b>%s</b> to <b>%s</b>.", esp.IGBText(m.charNameOr(ctx, id)), esp.IGBText(m.stationLabel(ctx, st))),
			"They live and work from that office; the CEO's posting pass leaves a posting you made alone."), "", true

	case "industry":
		dir, bad := member("director")
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		var dep int64
		if q.Get("deputy") != "" {
			if dep, bad = member("deputy"); bad != "" {
				return corpCommand{}, "", bad, true
			}
			if dep == dir {
				return corpCommand{}, "", "The deputy is somebody else.", true
			}
		}
		d, bad := days(30)
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		p := cmdIndustryPayload{DirectorID: dir, DeputyID: dep, HoldDays: d}
		cmd := corpCommand{Kind: cmdKindIndustry, Payload: encodeCommandPayload(p), ExpiresAt: ttl}
		return cmd, preview(fmt.Sprintf("Name <b>%s</b> %s%s, held %d day(s).", esp.IGBText(m.charNameOr(ctx, dir)), corpIndustryDirector,
			map[bool]string{true: " with " + esp.IGBText(m.charNameOr(ctx, dep)) + " as deputy", false: ""}[dep != 0], d),
			"The CEO's staffing pass keeps them in the chair for the hold."), "", true

	case "tags":
		var tags []string
		seen := map[string]bool{}
		for i := 1; i <= ceoTagMax; i++ {
			t := q.Get("tag" + strconv.Itoa(i))
			if t == "" || seen[t] {
				continue
			}
			ok := false
			for _, p := range ceoTagPool {
				if p == t {
					ok = true
				}
			}
			if !ok {
				return corpCommand{}, "", "That is not a word the corporation can be.", true
			}
			seen[t] = true
			tags = append(tags, t)
			hidden["tag"+strconv.Itoa(i)] = t
		}
		if len(tags) == 0 {
			return corpCommand{}, "", "Pick at least one word.", true
		}
		d, bad := days(30)
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		p := cmdTagsPayload{Tags: tags, HoldDays: d}
		cmd := corpCommand{Kind: cmdKindTags, Payload: encodeCommandPayload(p), ExpiresAt: ttl}
		return cmd, preview(fmt.Sprintf("Declare the corporation a <b>%s</b> outfit, held %d day(s).", esp.IGBText(strings.Join(tags, " / ")), d),
			"Fleets, recruiting and the ore desk read these; the CEO's identity pass leaves them alone for the hold."), "", true

	case "budget":
		num := func(field string, def float64) (float64, string) {
			v := strings.TrimSpace(q.Get(field))
			if v == "" {
				return def, ""
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 {
				return 0, "Budget figures are plain numbers."
			}
			hidden[field] = v
			return f, ""
		}
		reserve, bad := num("reserve", 3)
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		stipend, bad := num("stipend", 10)
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		store, bad := num("store", 5)
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		buyback, bad := num("buyback", 85)
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		if stipend > 100 || store > 200 || buyback > 100 || reserve > 36 {
			return corpCommand{}, "", "Stipends and buyback are percentages, store markup up to 200%, reserve up to 36 months.", true
		}
		d, bad := days(30)
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		hidden["levy"] = q.Get("levy")
		p := cmdBudgetPayload{ReserveMonths: reserve, StipendShare: stipend / 100, StoreMarkup: store / 100,
			BuybackFrac: buyback / 100, LevyOn: q.Get("levy") == "1", HoldDays: d}
		cmd := corpCommand{Kind: cmdKindBudget, Payload: encodeCommandPayload(p), ExpiresAt: ttl}
		return cmd, preview("Budget: <b>"+esp.IGBText(cmd.orgSummary(m, ctx))+"</b>.",
			"The treasury, the store, the buyback and the levy read these for the hold."), "", true

	case "oredrop":
		st, _ := strconv.ParseInt(q.Get("station"), 10, 64)
		if st != 0 && !m.corpHasOfficeAt(ctx, corpID, st) {
			return corpCommand{}, "", "Pick one of the corporation's offices.", true
		}
		hidden["station"] = q.Get("station")
		d, bad := days(30)
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		p := cmdOreDropPayload{StationID: st, HoldDays: d}
		cmd := corpCommand{Kind: cmdKindOreDrop, Payload: encodeCommandPayload(p), ExpiresAt: ttl}
		return cmd, preview("Fleet ore: <b>"+esp.IGBText(cmd.orgSummary(m, ctx))+"</b>.",
			"A mining fleet's haulers deliver into the corporation hangar there; nothing, and each miner keeps its own rock."), "", true

	case "oredesk":
		share, err := strconv.ParseFloat(q.Get("share"), 64)
		if err != nil || share < 0 || share > 100 {
			return corpCommand{}, "", "Pick how much the desk keeps.", true
		}
		hidden["share"] = q.Get("share")
		d, bad := days(30)
		if bad != "" {
			return corpCommand{}, "", bad, true
		}
		p := cmdOreDeskPayload{Share: share / 100, HoldDays: d}
		cmd := corpCommand{Kind: cmdKindOreDesk, Payload: encodeCommandPayload(p), ExpiresAt: ttl}
		return cmd, preview("Ore desk: <b>"+esp.IGBText(cmd.orgSummary(m, ctx))+"</b>.",
			"The business pass stops moving the share for the hold."), "", true
	}
	return corpCommand{}, "", "", false
}
