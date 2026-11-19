package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// The Corps front page: /rolebots/corps. A dashboard of the player corporations bots work for
// or run. The NPC catalogue, the recruitment stats and the employer-move log are on their own
// tab (espcorps_npc.go).
//
// By default only corporations a bot has to do with are listed (a rostered pilot, a bot in the
// chair, or an application waiting); state=all lists every one. Rows come from one joined
// catalogue read (listPlayerCorpFacts) plus four whole-table reads (wallets, ledger, wars,
// applications), never a query per row.

// rbTableStyle is the dense-table chrome the corporation tables share.
const rbTableStyle = `<style>
  details.rbfold > summary {
    cursor:pointer; list-style:none; font-size:15px; font-weight:600;
    padding:2px 0; user-select:none;
  }
  details.rbfold > summary::-webkit-details-marker { display:none }
  details.rbfold > summary::before { content:"\25BC "; font-size:11px; color:#6b7280 }
  details.rbfold:not([open]) > summary::before { content:"\25B6 "; }
  details.rbfold > summary .rbcount { font-weight:normal; color:#6b7280 }
  table.rbtight { font-size:12px; border-collapse:collapse; width:100% }
  table.rbtight th, table.rbtight td { padding:3px 6px; white-space:nowrap }
  table.rbtight tbody tr:nth-child(even) { background:rgba(127,127,127,.06) }
  table.rbtight td.rbwrap { white-space:normal; max-width:220px; overflow:hidden; text-overflow:ellipsis }
  table.rbtight td.rbnum { text-align:right }
  .rbdim { color:#6b7280 }
  .rb-pill { display:inline-block; padding:1px 8px; border-radius:9px; font-size:11px;
             background:#382020; color:#9ca3af; }
  .rb-pill.on { background:#0f2a1f; color:#6ee7b7; }
  .rb-pill.warn { background:#2a2010; color:#eab308; }
  .rb-pill.bad { background:#2a1010; color:#fca5a5; }
</style>`

// corpDashStats is the tile row.
type corpDashStats struct {
	Corps   int // every player corporation
	Listed  int // after the default scope
	Hidden  int // player corporations no bot touches, not listed
	Bots    int // rostered pilots wearing a player badge
	BotRun  int // corporations a bot CEO is running
	Pending int // applications waiting on a director
	AtWar   int // corporations in at least one open war
	Full    int // at their member limit
}

// corpWallets is every corporation's balance in one read.
func (m *Mod) corpWallets(ctx context.Context) map[int64]float64 {
	out := map[int64]float64{}
	rows, err := m.db.QueryContext(ctx, `SELECT corporationID, balance FROM corporation`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var bal float64
		if rows.Scan(&id, &bal) == nil {
			out[id] = bal
		}
	}
	return out
}

// corpNets is what each managed corporation earned since a time, in one grouped read of the
// books. Capital flows (shares, dividends, the CEO's own ISK) are left out: they move the
// wallet without being profit.
func (m *Mod) corpNets(ctx context.Context, since int64) map[int64]float64 {
	out := map[int64]float64{}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(capitalLines)), ",")
	args := []any{since}
	for _, l := range capitalLines {
		args = append(args, l)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, COALESCE(SUM(amount), 0)
		  FROM rolebot_corp_ledger
		 WHERE at >= ? AND line NOT IN (`+ph+`)
		 GROUP BY corporationID`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var net float64
		if rows.Scan(&id, &net) == nil {
			out[id] = net
		}
	}
	return out
}

// warCountsByOwner is the open wars each owner (corporation or alliance) is a
// party to. A corporation is at war when it or its alliance is.
func warCountsByOwner(ctx context.Context, m *Mod) map[int64]int {
	out := map[int64]int{}
	wars, err := evedb.ListWars(ctx, m.db)
	if err != nil {
		return out
	}
	for _, w := range wars {
		if !w.Active {
			continue
		}
		out[w.DeclaredByID]++
		out[w.AgainstID]++
	}
	return out
}

func (r corpListRow) warsOf(byOwner map[int64]int) int {
	n := byOwner[r.CorpID]
	if r.AllianceID != 0 {
		n += byOwner[r.AllianceID]
	}
	return n
}

// dashCorpSortCols is the dashboard's header row.
var dashCorpSortCols = []corpSortCol{
	{Key: "name", Label: "Corporation",
		Less: func(a, b corpListRow) bool { return lessFold(a.Name, b.Name) }},
	{Key: "ceo", Label: "CEO",
		Less: func(a, b corpListRow) bool { return lessFold(a.CEOName, b.CEOName) }},
	{Key: "members", Label: "Members", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.Members < b.Members }},
	{Key: "bots", Label: "Bots", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.Bots < b.Bots }},
	{Key: "pool", Label: "Pool", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.Eligible < b.Eligible }},
	{Key: "pending", Label: "Applied", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.Pending < b.Pending }},
	{Key: "wallet", Label: "Wallet", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.WalletISK < b.WalletISK }},
	{Key: "net", Label: "7-day net", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.NetISK < b.NetISK }},
	{Key: "wars", Label: "Wars", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.Wars < b.Wars }},
	{Key: "tags", Label: "Does",
		Less: func(a, b corpListRow) bool { return lessFold(a.TagText, b.TagText) }},
	{Key: "open", Label: "Door", DescFirst: true,
		Less: func(a, b corpListRow) bool { return !a.Policy.Joinable && b.Policy.Joinable }},
}

func (m *Mod) handleCorps(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleCorpsPost(w, r, ctx)
		return
	}

	pcorps, err := listPlayerCorpFacts(ctx, m.db)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	counts, err := m.eligibleCounts(ctx, pcorps)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	pending, err := m.pendingApplicationCounts(ctx)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	seats, _, err := m.rosterCEOSeats(ctx)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	managed := map[int64]*corpCEO{}
	if list, err := listCEOs(ctx, m.db); err == nil {
		for _, c := range list {
			managed[c.CorpID] = c
		}
	}
	wallets := m.corpWallets(ctx)
	nets := m.corpNets(ctx, clock.Unix()-ceoBooksWindowSec)
	wars := warCountsByOwner(ctx, m)

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	fState := r.URL.Query().Get("state")

	var st corpDashStats
	var rows []corpListRow
	for _, c := range pcorps {
		row := corpListRow{
			CorpFacts: c, Eligible: counts[c.CorpID], Pending: pending[c.CorpID],
			WalletISK: wallets[c.CorpID], NetISK: nets[c.CorpID],
		}
		row.Wars = row.warsOf(wars)
		_, row.BotCEO = seats[c.CorpID]
		if ce := managed[c.CorpID]; ce != nil {
			row.BotCEO, row.Managed, row.Paused = true, true, ce.Paused
		}
		row.WalletText, row.NetText, row.NetNeg = iskString(row.WalletISK), iskString(row.NetISK), row.NetISK < 0
		var labels []string
		for _, t := range c.Policy.Tags {
			labels = append(labels, corpTagLabel(t))
		}
		row.TagText = strings.Join(labels, ", ")

		st.Corps++
		touched := c.Bots > 0 || row.BotCEO || row.Pending > 0
		if !touched {
			st.Hidden++
		} else {
			st.Listed++
			st.Bots += c.Bots
			st.Pending += row.Pending
			if row.Managed {
				st.BotRun++
			}
			if row.Wars > 0 {
				st.AtWar++
			}
			if row.Full() {
				st.Full++
			}
		}

		if q != "" && !strings.Contains(strings.ToLower(c.Name), q) &&
			!strings.Contains(strings.ToLower(c.Ticker), q) &&
			!strings.Contains(strings.ToLower(c.CEOName), q) &&
			!strings.Contains(strings.ToLower(c.AllianceName), q) &&
			!strings.Contains(strings.ToLower(c.SystemName), q) &&
			!strings.Contains(strings.ToLower(c.RegionName), q) &&
			!strings.Contains(strings.ToLower(row.TagText), q) {
			continue
		}
		keep := touched
		switch fState {
		case "all":
			keep = true
		case "botrun":
			keep = row.Managed
		case "humanrun":
			keep = !row.BotCEO
		case "open":
			keep = c.Policy.Joinable
		case "shut":
			keep = !c.Policy.Joinable
		case "applied":
			keep = row.Pending > 0
		case "full":
			keep = row.Full()
		case "war":
			keep = row.Wars > 0
		case "losing":
			keep = row.Managed && row.NetISK < 0
		}
		if !keep {
			continue
		}
		rows = append(rows, row)
	}
	sortKey, sortDir, sortHeads := sortCorpRows(dashSortSpec, rows,
		r.URL.Query().Get("sort"), r.URL.Query().Get("dir"), r.URL.Query())

	apps, err := m.listCorpApplications(ctx, 0, false, 40)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	// Applications to NPC corporations do not exist (the module simply joins
	// one), so every row is a player corporation's.
	stats, err := m.corpRecruitmentStats(ctx)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}

	// Self-founding (corpfound.go). The preview is opt-in: it rolls thirty brands and asks the
	// ticker suggester for each, which the front page must not do on every load.
	founded, err := m.liveFoundedCorps(ctx)
	if err != nil {
		rlogf("rolebots: corps dashboard: %v", err)
	}
	liveFounded := len(founded)
	lastFounded := ""
	if len(founded) > 0 {
		lastFounded = time.Unix(founded[0].FoundedAt, 0).UTC().Format("2006-01-02 15:04")
	}
	if len(founded) > 12 {
		founded = founded[:12]
	}
	var preview []brandPreviewRow
	previewOff := -1
	if v := r.URL.Query().Get("preview"); v != "" {
		previewOff, _ = strconv.Atoi(v)
		if previewOff < 0 {
			previewOff = 0
		}
		preview = m.brandPreview(ctx, 30, previewOff)
	}
	// The probe walks every rostered leader, so it is opt-in like the preview.
	var probe []foundProbeRow
	probeReady := 0
	if r.URL.Query().Get("probe") != "" {
		if probe, err = m.probeFounders(ctx); err != nil {
			rlogf("rolebots: corps dashboard probe: %v", err)
		}
		for _, p := range probe {
			if p.Ready() {
				probeReady++
			}
		}
	}
	botFounded, err := m.listBotFoundedCorps(ctx)
	if err != nil {
		rlogf("rolebots: corps dashboard: %v", err)
	}

	espRender(w, "rolebots_corps.html", map[string]any{
		"FoundOn":     CorpFound(),
		"FoundFee":    fmt.Sprintf("%.1fM", evedb.CorporationStartupCost/1e6),
		"FoundMax":    corpFoundMax(),
		"FoundLive":   liveFounded,
		"FoundLast":   lastFounded,
		"Founded":     founded,
		"Preview":     preview,
		"PreviewOff":  previewOff,
		"PreviewNext": previewOff + 1,
		"Probe":       probe,
		"ProbeOn":     r.URL.Query().Get("probe") != "",
		"ProbeReady":  probeReady,
		"BotFounded":  len(botFounded),
		"HQMax":       corpHQPerSystemMax,
		"SpreadJumps": corpFoundSpreadRadius,
		"Page":        "rolebot-corps",
		"Tab":         "corps",
		"Msg":         r.URL.Query().Get("msg"),
		"Err":         r.URL.Query().Get("err"),
		"Recruit":     stats,
		"Stats":       st,
		"Rows":        rows,
		"Shown":       len(rows),
		"Q":           r.URL.Query().Get("q"),
		"FState":      fState,
		"Sort":        sortKey,
		"Dir":         sortDir,
		"SortHeads":   sortHeads,
		"Query":       r.URL.RawQuery,
		"Apps":        apps,
		"ApplyDays":   corpApplyExpirySec / 86400,
		"RejectDays":  corpApplyRejectCooldownSec / 86400,
	})
}

// tmplCorps is the player-corporation dashboard.
const tmplCorps = corpTabs + `{{define "content"}}` + castTabsStyle + rbTableStyle + `
<h2>Corporations</h2>
{{template "rb_corptabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="stats">
  <div class="stat"><div class="label">Player corporations</div><div class="value">{{.Stats.Listed}}{{if .Stats.Hidden}} <span class="rbdim" style="font-size:12px">of {{.Stats.Corps}}</span>{{end}}</div></div>
  <div class="stat"><div class="label">Run by a bot CEO</div><div class="value">{{.Stats.BotRun}}</div></div>
  <div class="stat"><div class="label">Bots in them</div><div class="value">{{.Stats.Bots}}</div></div>
  <div class="stat"><div class="label">Applications waiting</div><div class="value">{{.Stats.Pending}}</div></div>
  <div class="stat"><div class="label">At war</div><div class="value">{{.Stats.AtWar}}</div></div>
  <div class="stat"><div class="label">At member limit</div><div class="value">{{.Stats.Full}}</div></div>
</div>

<p style="font-size:12px" class="rbdim">
  {{.Recruit.Moved}} pilot(s) have changed employer ({{.Recruit.Moves}} move(s)), {{.Recruit.DueSoon}} due to look within a day
  &middot; <a href="/rolebots/corps/npc">recruitment and the NPC catalogue &rarr;</a>
</p>

<style>
  .rblogo { position:relative; display:inline-block; width:40px; height:40px; background:#000; vertical-align:middle; }
  .rblogo i { position:absolute; inset:0; display:block;
    -webkit-mask-size:contain; mask-size:contain; -webkit-mask-repeat:no-repeat; mask-repeat:no-repeat; }
</style>
<details class="rbfold" id="rbfold-found"{{if or .FoundOn .Preview .ProbeOn}} open{{end}}>
<summary>Self-founding <span class="rbcount">({{if .FoundOn}}ON{{else}}OFF{{end}} &middot; {{.FoundLive}} of {{.FoundMax}} live)</span></summary>
<div class="card">
  <p style="font-size:12px" class="rbdim">
    Leaders (Leadership above 50) may found a corporation at their employment review. Needs twice the
    {{.FoundFee}} ISK fee in their wallet. At most one founding every ten minutes server-wide.
    {{if .FoundLast}}Last founded {{.FoundLast}} UTC.{{end}}
  </p>
  <div style="display:flex;gap:16px;flex-wrap:wrap;align-items:center">
    <form method="POST" style="display:inline">
      <input type="hidden" name="action" value="found">
      <input type="hidden" name="state" value="{{if .FoundOn}}off{{else}}on{{end}}">
      <input type="hidden" name="back" value="{{.Query}}">
      <span class="rb-pill{{if .FoundOn}} on{{end}}">{{if .FoundOn}}ON{{else}}OFF{{end}}</span>
      <button type="submit">{{if .FoundOn}}Switch off{{else}}Switch on{{end}}</button>
    </form>
    <form method="POST" style="display:inline">
      <input type="hidden" name="action" value="foundmax">
      <input type="hidden" name="back" value="{{.Query}}">
      at most <input type="number" name="max" value="{{.FoundMax}}" min="0" max="1000" style="width:70px"> live at a time
      <button type="submit">Set</button>
    </form>
    <a href="/rolebots/corps?preview={{.PreviewNext}}#rbfold-found">{{if .Preview}}another 30 &rarr;{{else}}preview 30 brands &rarr;{{end}}</a>
    <a href="/rolebots/corps?probe=1#rbfold-found">probe leaders who could found &rarr;</a>
  </div>

  {{if .ProbeOn}}
  <div style="margin-top:12px;display:flex;gap:12px;align-items:center;flex-wrap:wrap">
    <b>{{.ProbeReady}} of {{len .Probe}} rostered leader(s) could found now.</b>
    <form method="POST" style="display:inline">
      <input type="hidden" name="action" value="foundnow">
      <input type="hidden" name="back" value="{{.Query}}">
      <button type="submit"{{if not .ProbeReady}} disabled{{end}}>Found one now</button>
    </form>
    <span class="rbdim" style="font-size:12px">Test buttons skip the switch and the ten-minute pace. The cap still applies.</span>
  </div>
  {{if .Probe}}
  <table class="rbtight" style="margin-top:8px">
    <thead><tr><th>Pilot</th><th>Leadership</th><th>Kind</th><th>Corporation</th><th>Wallet</th><th>Status</th><th></th></tr></thead>
    <tbody>
    {{range .Probe}}
    <tr{{if not .Ready}} style="opacity:.6"{{end}}>
      <td>{{.Name}}</td>
      <td class="rbnum">{{.Leadership}}</td>
      <td>{{.Kind}}</td>
      <td>{{.CorpName}}</td>
      <td class="rbnum">{{.WalletText}}</td>
      <td class="rbwrap" style="max-width:420px">{{if .Ready}}<span class="rb-pill on">ready</span> HQ: {{.HQ}}{{else}}{{.Refusal}}{{end}}</td>
      <td>{{if .Ready}}
        <form method="POST" style="display:inline">
          <input type="hidden" name="action" value="foundnow">
          <input type="hidden" name="charID" value="{{.CharID}}">
          <input type="hidden" name="back" value="{{$.Query}}">
          <button type="submit">Found now</button>
        </form>{{end}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}<p class="rbdim" style="font-size:12px">No enabled rostered pilot has Leadership above 50.</p>{{end}}
  {{end}}

  <div style="margin-top:12px;padding-top:8px;border-top:1px solid rgba(127,127,127,.25)">
    <form method="POST" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap">
      <input type="hidden" name="action" value="deletefounded">
      <input type="hidden" name="back" value="{{.Query}}">
      <span><b>{{.BotFounded}}</b> open corporation(s) were founded by a rolebot (a character on a RoleBot&lt;N&gt; account).</span>
      <input type="text" name="confirm" placeholder="type DELETE" style="width:120px">
      <button type="submit"{{if not .BotFounded}} disabled{{end}}>Close them all</button>
      <span class="rbdim" style="font-size:12px">Members return to their NPC corporations. A corporation that owns a starbase is refused.</span>
    </form>
  </div>

  {{if .Founded}}
  <table class="rbtight" style="margin-top:12px">
    <thead><tr><th>Corporation</th><th>Founder</th><th>Region</th><th>Founded (UTC)</th></tr></thead>
    <tbody>
    {{range .Founded}}
    <tr>
      <td><a href="/rolebots/corp/{{.CorpID}}">{{.Name}}</a> <span class="rbdim">[{{.Ticker}}]</span></td>
      <td>{{.FounderName}}</td>
      <td>{{.RegionName}}</td>
      <td>{{.When}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{end}}

  {{if .Preview}}
  <p style="font-size:12px;margin-top:12px" class="rbdim">
    Sample brands. Nothing is written. Logos stack slot 3 (bottom) to slot 1.
  </p>
  <table class="rbtight">
    <thead><tr><th>Logo</th><th>Name</th><th>Ticker</th><th>Kind</th><th>Founder</th><th>Region</th><th>Description</th></tr></thead>
    <tbody>
    {{range .Preview}}
    <tr>
      <td><span class="rblogo">{{range .Layers}}<i style="{{.Style}}"></i>{{end}}</span></td>
      <td><b>{{.Name}}</b></td>
      <td>{{.Ticker}}</td>
      <td>{{.Race}} {{.KindLabel}}</td>
      <td>{{.Founder}}</td>
      <td>{{.Region}}</td>
      <td class="rbwrap" style="max-width:420px">{{.Description}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{end}}
</div>
</details>

<div class="card">
  <form method="GET" style="display:flex;flex-wrap:wrap;gap:8px;align-items:center">
    <input type="hidden" name="sort" value="{{.Sort}}">
    <input type="hidden" name="dir" value="{{.Dir}}">
    <input type="text" name="q" value="{{.Q}}" placeholder="name, ticker, CEO, alliance, system, region, tag" style="width:340px">
    <select name="state">
      <option value=""{{if eq .FState ""}} selected{{end}}>with bots</option>
      <option value="all"{{if eq .FState "all"}} selected{{end}}>every player corporation</option>
      <option value="botrun"{{if eq .FState "botrun"}} selected{{end}}>run by a bot CEO</option>
      <option value="humanrun"{{if eq .FState "humanrun"}} selected{{end}}>human CEO</option>
      <option value="applied"{{if eq .FState "applied"}} selected{{end}}>applications waiting</option>
      <option value="open"{{if eq .FState "open"}} selected{{end}}>door open</option>
      <option value="shut"{{if eq .FState "shut"}} selected{{end}}>door shut</option>
      <option value="full"{{if eq .FState "full"}} selected{{end}}>at member limit</option>
      <option value="war"{{if eq .FState "war"}} selected{{end}}>at war</option>
      <option value="losing"{{if eq .FState "losing"}} selected{{end}}>losing money (7 days)</option>
    </select>
    <button type="submit">Search</button>
    <a href="/rolebots/corps">clear</a>
    <span class="rbdim" style="margin-left:auto">{{.Shown}} shown</span>
  </form>

  {{if .Rows}}
  <table class="rbtight" style="margin-top:12px">
    <thead>
    <tr>{{range .SortHeads}}<th{{if .On}} style="color:#22c55e"{{end}}><a href="{{.Href}}" style="color:inherit;text-decoration:none">{{.Label}}{{.Arrow}}</a></th>{{end}}</tr>
    </thead>
    <tbody>
    {{range .Rows}}
    <tr{{if .Full}} style="opacity:.7"{{end}}>
      <td><a href="/rolebots/corp/{{.CorpID}}">{{.Name}}</a>{{if .Ticker}} <span class="rbdim">[{{.Ticker}}]</span>{{end}}
        {{if .Full}}<span title="at its member limit — nobody can join" style="color:#eab308">&#9888;</span>{{end}}</td>
      <td>{{if .CEOName}}{{.CEOName}}{{else}}&mdash;{{end}}
        {{if .Managed}}{{if .Paused}}<span class="rb-pill warn">paused</span>{{else}}<span class="rb-pill on">bot-run</span>{{end}}
        {{else if .BotCEO}}<span class="rb-pill" title="a bot holds the chair but is not running the corporation">seated</span>
        {{else}}<span class="rbdim">human</span>{{end}}</td>
      <td class="rbnum">{{.LimitText}}</td>
      <td class="rbnum">{{if .Bots}}{{.Bots}}{{else}}<span class="rbdim">&mdash;</span>{{end}}</td>
      <td class="rbnum">{{.Eligible}}</td>
      <td class="rbnum">{{if .Pending}}<b style="color:#eab308">{{.Pending}}</b>{{else}}<span class="rbdim">&mdash;</span>{{end}}</td>
      <td class="rbnum">{{.WalletText}}</td>
      <td class="rbnum">{{if .Managed}}{{if .NetNeg}}<span style="color:#ef4444">{{.NetText}}</span>{{else}}{{.NetText}}{{end}}{{else}}<span class="rbdim">&mdash;</span>{{end}}</td>
      <td class="rbnum">{{if .Wars}}<b style="color:#ef4444">{{.Wars}}</b>{{else}}<span class="rbdim">&mdash;</span>{{end}}</td>
      <td class="rbwrap" title="{{.TagText}}">{{if .TagText}}{{.TagText}}{{else}}<span style="color:#ef4444">untagged</span>{{end}}</td>
      <td>
        <form method="POST" style="display:inline">
          <input type="hidden" name="action" value="toggle">
          <input type="hidden" name="corpID" value="{{.CorpID}}">
          <input type="hidden" name="state" value="{{if .Policy.Joinable}}off{{else}}on{{end}}">
          <input type="hidden" name="back" value="{{$.Query}}">
          <button type="submit" title="{{if .Policy.Joinable}}open — click to shut{{else}}shut — click to open{{end}}"
            style="{{if .Policy.Joinable}}color:#22c55e;font-weight:600{{else}}color:#6b7280{{end}}">{{if .Policy.Joinable}}open{{else}}shut{{end}}</button>
        </form>
      </td>
    </tr>
    {{end}}
    </tbody>
  </table>
  <p style="margin-top:8px;font-size:12px" class="rbdim">
    <b>Members</b> counts bots against the corporation's limit. <b>Pool</b> is enabled pilots who could join if the door were open.
    <b>7-day net</b> is the CEO's earnings, excluding share sales, dividends and its own capital. Human-run corporations show none. Every heading sorts.
  </p>
  {{else}}
  <p style="margin-top:12px">{{if .Stats.Corps}}No player corporation matches.{{if .Stats.Hidden}} {{.Stats.Hidden}} have no bot in them &mdash; <a href="/rolebots/corps?state=all">list every player corporation</a>.{{end}}{{else}}No player corporations exist on this server yet.{{end}}</p>
  {{end}}
</div>

{{if .Apps}}
<div class="card">
<details class="rbfold" id="rbfold-apps">
<summary>Applications <span class="rbcount">({{len .Apps}} most recent)</span></summary>
  <table class="tbl" style="margin-top:8px">
    <tr><th>Pilot</th><th>Corporation</th><th>Applied</th><th>State</th><th>Expires / decided</th><th>Score</th><th>Why it applied</th><th></th></tr>
    {{range .Apps}}
    <tr>
      <td><a href="/rolebot/{{.CharID}}">{{if .CharName}}{{.CharName}}{{else}}{{.CharID}}{{end}}</a></td>
      <td><a href="/rolebots/corp/{{.CorpID}}">{{if .CorpName}}{{.CorpName}}{{else}}{{.CorpID}}{{end}}</a>{{if .CorpTick}} <span class="rbdim">[{{.CorpTick}}]</span>{{end}}</td>
      <td>{{.AppliedText}}</td>
      <td>{{.StateLabel}}</td>
      <td>{{.WaitText}}</td>
      <td>{{printf "%.2f" .Score}}</td>
      <td style="font-size:12px" class="rbdim">{{.Reason}}</td>
      <td>
        {{if eq .State "pending"}}
        <form method="POST" style="display:inline">
          <input type="hidden" name="action" value="withdraw">
          <input type="hidden" name="charID" value="{{.CharID}}">
          <input type="hidden" name="corpID" value="{{.CorpID}}">
          <input type="hidden" name="back" value="{{$.Query}}">
          <button type="submit">withdraw</button>
        </form>
        {{end}}
      </td>
    </tr>
    {{end}}
  </table>
  <p style="margin-top:8px;font-size:12px" class="rbdim">
    Unanswered applications are withdrawn after {{.ApplyDays}} days. A rejected pilot waits {{.RejectDays}} to reapply. <b>Withdraw</b> removes one early.
  </p>
</details>
</div>
{{end}}

<script>
  (function () {
    document.querySelectorAll('details.rbfold').forEach(function (d) {
      var k = 'rolebots.' + d.id;
      try { if (localStorage.getItem(k) === 'shut') { d.open = false; } } catch (e) {}
      d.addEventListener('toggle', function () {
        try { localStorage.setItem(k, d.open ? 'open' : 'shut'); } catch (e) {}
      });
    });
  })();
</script>
{{end}}`

// setCorpDoor is the one writer of a corporation's open/shut door, shared by
// the dashboard, the NPC catalogue and the corporation page.
func (m *Mod) setCorpDoor(ctx context.Context, corpID int64, on bool) (string, error) {
	c, err := getAnyCorpFacts(ctx, m.db, corpID)
	if err != nil {
		return "", err
	}
	c.Policy.Joinable = on
	if err := saveCorpPolicy(ctx, m.db, &c.Policy); err != nil {
		return "", err
	}
	if on {
		return fmt.Sprintf("%s is open to bots", c.Name), nil
	}
	return fmt.Sprintf("%s is shut to bots", c.Name), nil
}

// parseCorpID reads a corporation ID out of a form field.
func parseCorpID(s string) int64 {
	id, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return id
}
