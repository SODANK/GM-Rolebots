package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

// One player corporation: /rolebots/corp/<id> (the old /rolebots/ceo/<id> answers the same page). It reads
// like the Main Cast: the CEO first, then members, books, wars, desk and decisions. It is one page for
// every player corporation:
//
//   - a bot CEO running the corporation (a rolebot_ceos row) adds the sections only it has: charter,
//     directive, books, shares, industry, starbases, staff and the decision log;
//   - a bot in the chair that runs nothing, or a human CEO, gets the rest; absent sections are not drawn.
//
// A running CEO decides the corporation's tags, directive and powers itself, so there is no policy form.
// The operator's levers (the door, kick-all, the HQ ban) sit in the Advanced block at the bottom.

type corpWarRow struct {
	Text    string
	Hostile bool
}

type ceoDeskRow struct {
	CharID   int64
	Name     string
	Waiting  string
	OnRoster bool
}

type ceoMemberRow struct {
	CharID  int64
	Name    string
	Title   string
	Officer string
	Seat    string
	Lead    int
	Bot     bool
}

type ceoDirectiveRow struct {
	Label string
	Pct   int
}

type ceoPowerRow struct {
	Key, Label string
	On         bool
}

const ceoLedgerPage = 50

// ceoBooksWindowSec is the P&L window the corporation page shows.
const ceoBooksWindowSec = 7 * 86400

// ceoBookRow is one line of the corporation's P&L, formatted.
type ceoBookRow struct {
	Label, In, Out, Net string
	Neg                 bool
}

func (m *Mod) playerCorpPage(w http.ResponseWriter, r *http.Request, ctx context.Context, facts *CorpFacts) {
	corpID := facts.CorpID
	c, err := getCEO(ctx, m.db, corpID) // nil: a human CEO, or a bot not running it
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	managed := c != nil
	ceoChar := facts.CEOID
	if managed {
		ceoChar = c.CharID
	}

	// Is the chair a rostered pilot, and is it switched on?
	var botCEO, botActive bool
	if ceoChar != 0 {
		var en int
		if err := m.db.QueryRowContext(ctx,
			`SELECT enabled FROM rolebot_characters WHERE characterID = ?`, ceoChar).Scan(&en); err == nil {
			botCEO, botActive = true, en != 0
		}
	}
	ceoName := facts.CEOName
	if ceoName == "" && ceoChar != 0 {
		ceoName = m.charName(ctx, ceoChar)
	}

	state, stateClass := "human CEO", ""
	switch {
	case managed && c.Paused:
		state, stateClass = "paused", "warn"
	case managed:
		state, stateClass = "running the corporation", "on"
	case botCEO && botActive:
		state, stateClass = "taking charge at the next sweep", ""
	case botCEO:
		state, stateClass = "seated — its pilot is inactive", ""
	}

	data := map[string]any{
		"Page":       "rolebot-corps",
		"Tab":        "corps",
		"Msg":        r.URL.Query().Get("msg"),
		"Err":        r.URL.Query().Get("err"),
		"Corp":       facts,
		"CEOName":    ceoName,
		"CEOChar":    ceoChar,
		"BotCEO":     botCEO,
		"Managed":    managed,
		"State":      state,
		"StateClass": stateClass,
		"Self":       fmt.Sprintf("/rolebots/corp/%d", corpID),
		"RaceFilter": playerCorpRaceNames(facts.AllowedRaceMask),
		"Doctrine":   doctrineFromTags(facts.Policy.Tags),
		"WingPct":    int(wingFracFromTags(facts.Policy.Tags)*100 + 0.5),
		"ApplyDays":  corpApplyExpirySec / 86400,
		"RejectDays": corpApplyRejectCooldownSec / 86400,
	}
	var tagLabels []string
	for _, t := range facts.Policy.Tags {
		tagLabels = append(tagLabels, corpTagLabel(t))
	}
	data["TagText"] = strings.Join(tagLabels, ", ")
	// The side it flies for (allegiance.go), and the choices the operator may set.
	data["Allegiance"] = facts.Policy.Allegiance
	data["AllegianceName"] = m.factionLabel(ctx, facts.Policy.Allegiance)
	if choices, err := m.allegianceChoices(ctx); err == nil {
		data["AllegianceOptions"] = choices
	}

	if botCEO {
		if traits, err := getTraits(ctx, m.db, ceoChar); err == nil {
			data["Chips"] = traitHighlights(traits, 5)
		}
	}

	bal, _ := evedb.GetCorpBalance(ctx, m.db, corpID)
	data["Balance"] = iskString(bal)

	// Hostility goes through relWarHostile, which guards the future-dated war the core's War.Hostile reads as
	// hostile at once (corprel.go), so the at-war pill and the Relations card agree.
	rel := m.ceoRelationsView(ctx, corpID, facts, managed, r.URL.Query())
	var wars []corpWarRow
	for _, w := range rel.Wars {
		wars = append(wars, corpWarRow{w.Text, w.Hostile})
	}
	data["Wars"] = wars
	data["Relations"] = rel

	// ── members, with their fleet seat ──────────────────────────────────
	seats := map[int64]string{}
	if ranks, err := m.listCorpRanks(ctx, corpID); err == nil {
		for _, rk := range ranks {
			seats[rk.CharID] = fmt.Sprintf("%s FC", rk.Activity)
		}
	}
	var members []ceoMemberRow
	if mrs, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, ch.characterName, COALESCE(ch.title, ''), COALESCE(r.corprole, 0),
		       CASE WHEN b.characterID IS NULL THEN 0 ELSE 1 END
		  FROM character_ ch
		  LEFT JOIN chrCorporationRoles r ON r.characterID = ch.characterID
		  LEFT JOIN rolebot_characters b ON b.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0
		 ORDER BY ch.characterName`, corpID); err == nil {
		for mrs.Next() {
			var mr ceoMemberRow
			var roles uint64
			var bot int
			if mrs.Scan(&mr.CharID, &mr.Name, &mr.Title, &roles, &bot) == nil {
				mr.Bot = bot != 0
				mr.Seat = seats[mr.CharID]
				if mr.CharID == ceoChar {
					mr.Officer = "CEO"
				} else if o := roles & ceoRoleMask; o != 0 {
					mr.Officer = strings.TrimPrefix(officerName(o), "a ")
				}
				members = append(members, mr)
			}
		}
		mrs.Close()
	}
	for i := range members {
		if members[i].Bot {
			if tr, err := getTraits(ctx, m.db, members[i].CharID); err == nil {
				members[i].Lead = tr[TraitLeadership]
			}
		}
	}
	data["Members"] = members

	// ── applications: the real desk (humans too) and the module's ledger ──
	var desk []ceoDeskRow
	if drs, err := m.db.QueryContext(ctx, `
		SELECT a.characterID, COALESCE(ch.characterName, ''), a.applicationDateTime,
		       CASE WHEN r.characterID IS NULL THEN 0 ELSE 1 END
		  FROM chrApplications a
		  LEFT JOIN character_ ch ON ch.characterID = a.characterID
		  LEFT JOIN rolebot_characters r ON r.characterID = a.characterID
		 WHERE a.corporationID = ? AND a.deleted = 0 AND a.status = ?
		 ORDER BY a.applicationDateTime`, corpID, evedb.AppAppliedByCharacter); err == nil {
		now := win32Now()
		for drs.Next() {
			var d ceoDeskRow
			var at int64
			var bot int
			if drs.Scan(&d.CharID, &d.Name, &at, &bot) == nil {
				d.OnRoster = bot != 0
				if at > 0 && now > at {
					d.Waiting = fmtDuration((now - at) / 10_000_000)
				}
				desk = append(desk, d)
			}
		}
		drs.Close()
	}
	data["Desk"] = desk
	if apps, err := m.listCorpApplications(ctx, corpID, false, 30); err == nil {
		data["Apps"] = apps
	}

	if !managed {
		espRender(w, "rolebots_corp.html", data)
		return
	}

	// ── everything below is a running CEO's own ─────────────────────────
	traits, _ := getTraits(ctx, m.db, c.CharID)
	ch := resolveCharter(pilotIdentity{CharID: c.CharID, Traits: traits, Tune: resolveTuning(traits)})

	directive, _, _ := m.loadCorpDirective(ctx, corpID)
	var drows []ceoDirectiveRow
	for act, wt := range directive {
		label := act
		if def, ok := activityDefFor(act); ok {
			label = def.Label
		}
		drows = append(drows, ceoDirectiveRow{label, int(wt*100 + 0.5)})
	}
	sort.Slice(drows, func(i, j int) bool { return drows[i].Pct > drows[j].Pct })

	fPower := r.URL.Query().Get("power")
	fVerb := r.URL.Query().Get("verb")
	page, _ := strconv.Atoi(r.URL.Query().Get("p"))
	if page < 0 {
		page = 0
	}
	acts, err := m.listCEOActions(ctx, corpID, fPower, fVerb, ceoLedgerPage+1, page*ceoLedgerPage)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	more := len(acts) > ceoLedgerPage
	if more {
		acts = acts[:ceoLedgerPage]
	}

	var books []ceoBookRow
	var net float64
	if lines, err := m.corpPnL(ctx, corpID, clock.Unix()-ceoBooksWindowSec); err == nil {
		for _, l := range lines {
			books = append(books, ceoBookRow{l.Label, iskString(l.In), iskString(l.Out), iskString(l.Net), l.Net < 0})
		}
	}
	net = m.corpNets(ctx, clock.Unix()-ceoBooksWindowSec)[corpID]

	var verbs []string
	if vr, err := m.db.QueryContext(ctx,
		`SELECT DISTINCT verb FROM rolebot_ceo_actions WHERE corporationID = ? ORDER BY verb`, corpID); err == nil {
		for vr.Next() {
			var v string
			if vr.Scan(&v) == nil {
				verbs = append(verbs, v)
			}
		}
		vr.Close()
	}
	var powerOpts []ceoPowerRow
	for _, p := range ceoPowers {
		powerOpts = append(powerOpts, ceoPowerRow{Key: p, Label: ceoPowerLabel[p], On: p == fPower})
	}

	lastThink := "never"
	if c.LastThinkAt > 0 {
		lastThink = fmtDuration(clock.Unix()-c.LastThinkAt) + " ago"
	}
	nextThink := "due"
	if left := c.NextThinkAt - clock.Unix(); left > 0 {
		nextThink = "in " + fmtDuration(left)
	}

	data["CEO"] = c
	data["Charter"] = ch
	data["Traits"] = traitRows(traits)
	data["Directive"] = drows
	data["Actions"] = acts
	data["More"] = more
	data["PageNo"] = page
	data["NextPage"] = page + 1
	data["PrevPage"] = page - 1
	data["FPower"] = fPower
	data["FVerb"] = fVerb
	data["PowerOpts"] = powerOpts
	data["Verbs"] = verbs
	data["Staff"] = m.ceoStaffView(ctx, c)
	data["Held"] = corpOpeningPressure(corpID)
	data["HRDelay"] = fmtDuration(ch.HRDelaySec)
	data["Reserve"] = iskString(m.corpReserve(ctx, corpID, ch))
	data["Books"] = books
	data["Net"] = iskString(net)
	data["NetNeg"] = net < 0
	data["Shares"] = m.ceoShareView(ctx, c, ch)
	data["Industry"] = m.ceoIndustryView(ctx, corpID, facts)
	data["Research"] = m.ceoResearchView(ctx, corpID)
	data["Trade"] = m.ceoTradeView(ctx, c, facts)
	data["Starbase"] = m.ceoStarbaseView(ctx, corpID)
	data["Armory"] = m.ceoArmoryView(ctx, corpID)
	data["LastThink"] = lastThink
	data["NextThink"] = nextThink
	espRender(w, "rolebots_corp.html", data)
}

// tmplCorpPage is the player-corporation page.
const tmplCorpPage = corpTabs + `{{define "content"}}` + castTabsStyle + rbCardStyle + rbTableStyle + `
<h2>{{.Corp.Name}}{{if .Corp.Ticker}} <span style="font-weight:normal;color:#6b7280">[{{.Corp.Ticker}}]</span>{{end}}</h2>
{{template "rb_corptabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
<p><a href="/rolebots/corps">&larr; All player corporations</a> &middot; <a href="/rolebots/ceos">bot CEOs</a>
  &middot; <a href="/corporation/{{.Corp.CorpID}}">corporation record</a></p>

<div class="rb-card" style="margin-bottom:12px">
  <div class="hd">
    {{if .CEOChar}}<img src="/portrait/{{.CEOChar}}?size=128" width="64" height="64"
         onerror="this.style.visibility='hidden'" loading="lazy" alt="">{{end}}
    <div>
      <div class="nm">{{if .BotCEO}}<a href="/rolebot/{{.CEOChar}}">{{.CEOName}}</a>{{else if .CEOName}}{{.CEOName}}{{else}}no CEO{{end}}
        <span class="id">CEO</span></div>
      <div class="co">{{if .Corp.AllianceName}}{{.Corp.AllianceName}} &middot; {{end}}{{if .Corp.StationName}}{{.Corp.StationName}}{{else if .Corp.SystemName}}{{.Corp.SystemName}}{{else}}no headquarters{{end}}{{if .Corp.RegionName}} <span class="rbdim">({{.Corp.RegionName}})</span>{{end}}</div>
      <div style="margin-top:5px">
        <span class="rb-pill {{.StateClass}}">{{.State}}</span>
        {{if .Corp.Policy.Joinable}}<span class="rb-pill on">door open</span>{{else}}<span class="rb-pill">door shut</span>{{end}}
        {{if .Wars}}<span class="rb-pill bad">at war</span>{{end}}
      </div>
    </div>
  </div>
  {{if .Chips}}<div>{{range .Chips}}<span class="rb-chip">{{.}}</span>{{end}}</div>{{end}}
  <dl>
    <dt>Does</dt>
    <dd>{{if .TagText}}{{.TagText}}{{else}}<span style="color:#ef4444">untagged</span>{{end}}
      <span class="rbdim">&mdash; {{.Doctrine.Label}}{{if .WingPct}}, {{.WingPct}}% of the roster may be under arms{{end}}</span></dd>
    <dt>Offices</dt>
    <dd>{{.Corp.Offices}} rented</dd>
    <dt>Race filter</dt>
    <dd>{{if .RaceFilter}}<b>{{.RaceFilter}}</b> only{{else}}any race{{end}}</dd>
    <dt>Allegiance</dt>
    <dd>{{if .Allegiance}}<b>{{.AllegianceName}}</b>
      <span class="rbdim">&mdash; members spare its rats and sites</span>{{else}}unaligned
      <span class="rbdim">&mdash; every faction's rats and sites are fair game</span>{{end}}</dd>
    {{if .Managed}}
    <dt>Reviewed</dt>
    <dd>{{.LastThink}}; next {{.NextThink}}</dd>
    {{end}}
    <dt>Door</dt>
    <dd>
      <form method="POST" style="display:inline">
        <input type="hidden" name="action" value="toggle">
        <input type="hidden" name="corpID" value="{{.Corp.CorpID}}">
        <input type="hidden" name="state" value="{{if .Corp.Policy.Joinable}}off{{else}}on{{end}}">
        <button type="submit">{{if .Corp.Policy.Joinable}}shut it to bots{{else}}open it to bots{{end}}</button>
      </form>
      {{if .Managed}}<span class="rbdim">&mdash; the CEO may change it</span>{{end}}
    </dd>
  </dl>
</div>

<div class="stats">
  <div class="stat"><div class="label">Members</div><div class="value">{{.Corp.Members}}{{if .Corp.MemberLimit}} / {{.Corp.MemberLimit}}{{end}}</div></div>
  <div class="stat"><div class="label">Bots</div><div class="value">{{.Corp.Bots}}</div></div>
  <div class="stat"><div class="label">Wallet</div><div class="value">{{.Balance}}</div></div>
  {{if .Managed}}
  <div class="stat"><div class="label">Reserve</div><div class="value">{{.Reserve}}</div></div>
  <div class="stat"><div class="label">7-day net</div><div class="value">{{if .NetNeg}}<span style="color:#ef4444">{{.Net}}</span>{{else}}{{.Net}}{{end}}</div></div>
  <div class="stat"><div class="label">Waiting for an opening</div><div class="value">{{.Held}}</div></div>
  {{end}}
  <div class="stat"><div class="label">Tax</div><div class="value">{{printf "%.0f" .Corp.TaxPercent}}%</div></div>
</div>

{{if .Wars}}
<div class="card">
  <h3>Wars</h3>
  <table class="tbl" style="font-size:12px">
    {{range .Wars}}<tr><td>{{.Text}}</td><td>{{if .Hostile}}<span class="rb-pill bad">shootable</span>{{else}}<span class="rb-pill warn">arming</span>{{end}}</td></tr>{{end}}
  </table>
</div>
{{end}}
` + tmplCEORelations + `

{{if .Managed}}
<div class="card">
  <h3>The charter — what this CEO's personality means for the corporation</h3>
  <table class="tbl" style="font-size:12px">
    <tr><th>Decision</th><th>Value</th><th>Because</th></tr>
    {{range .Charter.Why}}<tr><td>{{.Field}}</td><td><b>{{.Value}}</b></td><td style="color:#9ca3af">{{.Because}}</td></tr>{{end}}
  </table>
  <h4 style="margin-top:10px">The CEO's traits</h4>
  <p style="font-size:12px">{{range .Traits}}{{.Label}} <b>{{.Value}}</b> &nbsp; {{end}}</p>
</div>

<div class="card">
  <h3>Directive to members</h3>
  {{if .Directive}}
  <table class="tbl" style="font-size:12px">
    {{range .Directive}}<tr><td>{{.Label}}</td><td><div style="background:#7c2d12;height:10px;width:{{.Pct}}%"></div></td><td>{{.Pct}}%</td></tr>{{end}}
  </table>
  <p style="font-size:12px;color:#6b7280">Members lean toward the named work, in proportion to their obedience.</p>
  {{else}}<p>No directive yet — set at the next review.</p>{{end}}
</div>

<div class="card">
  <h3>The books — last 7 days</h3>
  {{if .Books}}
  <table class="tbl" style="font-size:12px">
    <tr><th>Line</th><th>In</th><th>Out</th><th>Net</th></tr>
    {{range .Books}}<tr><td>{{.Label}}</td><td>{{.In}}</td><td>{{.Out}}</td>
      <td>{{if .Neg}}<span style="color:#ef4444">{{.Net}}</span>{{else}}{{.Net}}{{end}}</td></tr>{{end}}
  </table>
  {{else}}<p>Nothing booked yet.</p>{{end}}
  <p style="font-size:12px;color:#6b7280">Module-driven money only. Bounty tax and office rent are not shown. The wallet is the truth.</p>
</div>
` + tmplCEOShares + `

` + tmplCEOIndustry + tmplCEOResearch + tmplCEOTrade + tmplCEOStarbase + tmplCEOArmory + `
{{end}}

<div class="card">
  <h3>Applications</h3>
  {{if .Desk}}
  <table class="tbl" style="font-size:12px">
    <tr><th>On the desk</th><th>Waiting</th></tr>
    {{range .Desk}}<tr><td>{{if .OnRoster}}<a href="/rolebot/{{.CharID}}">{{.Name}}</a>{{else}}{{.Name}} <span style="color:#6b7280">(player)</span>{{end}}</td>
      <td>{{.Waiting}}</td></tr>{{end}}
  </table>
  {{if .Managed}}<p style="font-size:12px;color:#6b7280">Applications are read after {{.HRDelay}} on the desk, best fit first.</p>{{end}}
  {{else}}<p>Nobody is waiting on a director.{{if not .Corp.Policy.Joinable}} The door is shut &mdash; nothing will.{{else if not .Corp.Policy.Tags}} The door is open but the corporation is <b>untagged</b>, so nothing is advertised.{{end}}</p>{{end}}
  {{if .Apps}}
  <h4 style="margin-top:12px">Bot applications</h4>
  <table class="tbl" style="font-size:12px">
    <tr><th>Pilot</th><th>Applied</th><th>State</th><th>Expires / decided</th><th>Score</th><th>Why it applied</th><th></th></tr>
    {{range .Apps}}
    <tr>
      <td><a href="/rolebot/{{.CharID}}">{{if .CharName}}{{.CharName}}{{else}}{{.CharID}}{{end}}</a></td>
      <td>{{.AppliedText}}</td>
      <td>{{.StateLabel}}</td>
      <td>{{.WaitText}}</td>
      <td>{{printf "%.2f" .Score}}</td>
      <td style="color:#6b7280">{{.Reason}}</td>
      <td>
        {{if eq .State "pending"}}
        <form method="POST" style="display:inline">
          <input type="hidden" name="action" value="withdraw">
          <input type="hidden" name="charID" value="{{.CharID}}">
          <input type="hidden" name="corpID" value="{{$.Corp.CorpID}}">
          <button type="submit">withdraw</button>
        </form>
        {{end}}
      </td>
    </tr>
    {{end}}
  </table>
  <p style="font-size:12px;color:#6b7280">Unanswered applications are withdrawn after {{.ApplyDays}} days. A rejected pilot waits {{.RejectDays}} to reapply.</p>
  {{end}}
</div>

<div class="card">
  <h3>Members</h3>
  <table class="tbl" style="font-size:12px">
    <tr><th>Pilot</th><th>Title</th><th>Office</th><th>Fleet seat</th><th>Leader trait</th></tr>
    {{range .Members}}
    <tr><td>{{if .Bot}}<a href="/rolebot/{{.CharID}}">{{.Name}}</a>{{else}}{{.Name}} <span style="color:#6b7280">(player)</span>{{end}}</td>
      <td>{{if .Title}}{{.Title}}{{else}}—{{end}}</td>
      <td>{{if .Officer}}{{.Officer}}{{else}}—{{end}}</td>
      <td>{{if .Seat}}{{.Seat}}{{else}}—{{end}}</td>
      <td>{{if .Bot}}{{.Lead}}{{else}}—{{end}}</td></tr>
    {{end}}
  </table>
</div>

{{if .Managed}}
` + tmplCEOStaff + `

<div class="card">
  <h3>What it has done</h3>
  <form method="GET" style="margin-bottom:8px">
    <select name="power"><option value="">every power</option>
      {{range .PowerOpts}}<option value="{{.Key}}"{{if .On}} selected{{end}}>{{.Label}}</option>{{end}}</select>
    <select name="verb"><option value="">every kind</option>
      {{range .Verbs}}<option value="{{.}}"{{if eq . $.FVerb}} selected{{end}}>{{.}}</option>{{end}}</select>
    <button type="submit">filter</button>
  </form>
  {{if .Actions}}
  <table class="tbl" style="font-size:12px">
    <tr><th>When</th><th>Power</th><th>Kind</th><th>By</th><th>Decision</th></tr>
    {{range .Actions}}<tr><td style="white-space:nowrap">{{.AgeText}}</td><td>{{.PowerLabel}}</td><td>{{.Verb}}</td>
      <td>{{if .ActorID}}{{.ActorName}}{{else}}CEO{{end}}</td><td>{{.Detail}}</td></tr>{{end}}
  </table>
  <p>{{if ge .PrevPage 0}}<a href="{{.Self}}?power={{.FPower}}&amp;verb={{.FVerb}}&amp;p={{.PrevPage}}">&larr; newer</a>{{end}}
     {{if .More}} <a href="{{.Self}}?power={{.FPower}}&amp;verb={{.FVerb}}&amp;p={{.NextPage}}">older &rarr;</a>{{end}}</p>
  {{else}}<p>Nothing recorded{{if or .FPower .FVerb}} for this filter{{end}}.</p>{{end}}
</div>
{{end}}

<div class="card">
<details>
<summary style="cursor:pointer" class="rbdim">Advanced</summary>
  <div style="margin-top:10px">
    {{if .Managed}}
    <p>
      <b>HQ move ban</b> <span class="rbdim">(offices may still be rented)</span>:
      {{if .CEO.HQMovable}}<span style="color:#eab308">lifted</span>{{else}}<b>banned</b>{{end}}
      <form method="POST" style="display:inline">
        <input type="hidden" name="corpID" value="{{.Corp.CorpID}}">
        <input type="hidden" name="action" value="hqmove">
        <input type="hidden" name="on" value="{{if .CEO.HQMovable}}0{{else}}1{{end}}">
        <button type="submit">{{if .CEO.HQMovable}}ban HQ moves{{else}}allow HQ moves{{end}}</button>
      </form>
    </p>
    {{if .CEO.Paused}}
    <p>
      This CEO is <b style="color:#eab308">paused</b> and decides nothing.
      <form method="POST" style="display:inline">
        <input type="hidden" name="corpID" value="{{.Corp.CorpID}}">
        <input type="hidden" name="action" value="resume">
        <button type="submit">Resume</button>
      </form>
    </p>
    {{end}}
    {{end}}
    <p>
      <b>Allegiance</b>
      <form method="POST" style="display:inline">
        <input type="hidden" name="corpID" value="{{.Corp.CorpID}}">
        <input type="hidden" name="action" value="allegiance">
        <select name="factionID">
          <option value="0"{{if not .Allegiance}} selected{{end}}>Unaligned</option>
          {{range .AllegianceOptions}}<option value="{{.ID}}"{{if eq .ID $.Allegiance}} selected{{end}}>{{.Name}}</option>{{end}}
        </select>
        <button type="submit">Save</button>
      </form>
      <span style="font-size:12px" class="rbdim">&mdash; the faction its members fly for. Set at founding. Only you can change it.</span>
    </p>
    {{if .Corp.Bots}}
    <form method="POST" onsubmit="return confirm('Send all {{.Corp.Bots}} rolebot(s) in {{.Corp.Name}} back to their school corporations?')">
      <input type="hidden" name="action" value="kickall">
      <button type="submit">Kick all {{.Corp.Bots}} bot(s) back to school</button>
      <span style="font-size:12px" class="rbdim">&mdash; each returns to the corporation it was created in.{{if .Managed}} The CEO stays.{{end}}</span>
    </form>
    {{end}}
  </div>
</details>
</div>
{{end}}`
