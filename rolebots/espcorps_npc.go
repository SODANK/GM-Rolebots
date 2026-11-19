package rolebots

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The NPC corps tab: /rolebots/corps/npc. The whole NPC corporation list on one page, since the question is
// comparative: which of these should be open at all. It leads with where the bots are (the overview), then the
// recruitment stats, then the catalogue. Filters narrow it (faction, bloc, activity, tag, state, free text),
// and each row carries whether it is open and how many rostered pilots would clear the hard rules if it were.

// npcFactionRow is one line of the "who employs the bots" rollup.
type npcFactionRow struct {
	Faction string
	Bloc    string
	Corps   int
	Open    int
	Bots    int
	Pct     int // of every bot in an NPC corporation
	Top     string
	TopBots int
}

// npcOverview is the population readout over the whole NPC catalogue, before
// any filter: the tiles and the two tables are about the game's employers, not
// about the rows the operator happened to narrow to.
type npcOverview struct {
	Corps, Open, Shut int
	WithBots          int
	Bots              int
	SchoolBots        int
	EmployedBots      int
	Factions          []npcFactionRow
	TopEmployers      []npcEmployerRow
}

type npcEmployerRow struct {
	CorpID  int64
	Name    string
	Ticker  string
	Faction string
	Bots    int
	Open    bool
	School  bool
}

func buildNPCOverview(corps []CorpFacts) npcOverview {
	var ov npcOverview
	byFaction := map[string]*npcFactionRow{}
	var employers []npcEmployerRow
	for _, c := range corps {
		ov.Corps++
		if c.Policy.Joinable {
			ov.Open++
		} else {
			ov.Shut++
		}
		if c.Bots > 0 {
			ov.WithBots++
			ov.Bots += c.Bots
			if c.School {
				ov.SchoolBots += c.Bots
			} else {
				ov.EmployedBots += c.Bots
			}
			employers = append(employers, npcEmployerRow{
				CorpID: c.CorpID, Name: c.Name, Ticker: c.Ticker, Faction: c.FactionName,
				Bots: c.Bots, Open: c.Policy.Joinable, School: c.School,
			})
		}
		name := c.FactionName
		if name == "" {
			name = "no faction"
		}
		fr := byFaction[name]
		if fr == nil {
			fr = &npcFactionRow{Faction: name, Bloc: c.BlocLabel()}
			byFaction[name] = fr
		}
		fr.Corps++
		if c.Policy.Joinable {
			fr.Open++
		}
		fr.Bots += c.Bots
		if c.Bots > fr.TopBots {
			fr.Top, fr.TopBots = c.Name, c.Bots
		}
	}
	for _, fr := range byFaction {
		fr.Pct = pctOf(float64(fr.Bots), float64(ov.Bots))
		ov.Factions = append(ov.Factions, *fr)
	}
	sort.Slice(ov.Factions, func(i, j int) bool {
		if ov.Factions[i].Bots != ov.Factions[j].Bots {
			return ov.Factions[i].Bots > ov.Factions[j].Bots
		}
		return ov.Factions[i].Faction < ov.Factions[j].Faction
	})
	sort.Slice(employers, func(i, j int) bool {
		if employers[i].Bots != employers[j].Bots {
			return employers[i].Bots > employers[j].Bots
		}
		return employers[i].Name < employers[j].Name
	})
	if len(employers) > 12 {
		employers = employers[:12]
	}
	ov.TopEmployers = employers
	return ov
}

func (m *Mod) handleCorpsNPC(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleCorpsPost(w, r, ctx)
		return
	}

	corps, err := listCorpFacts(ctx, m.db)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	counts, err := m.eligibleCounts(ctx, corps)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	fFaction := r.URL.Query().Get("faction")
	fBloc := r.URL.Query().Get("bloc")
	fTag := r.URL.Query().Get("tag")
	fState := r.URL.Query().Get("state")
	fActivity, _ := strconv.ParseInt(r.URL.Query().Get("activity"), 10, 64)

	factions := map[string]bool{}
	activities := map[int64]bool{}
	var rows []corpListRow
	var open, restricted int
	for _, c := range corps {
		if c.FactionName != "" {
			factions[c.FactionName] = true
		}
		if c.ActivityID != 0 {
			activities[c.ActivityID] = true
		}
		if c.Policy.Joinable {
			open++
		}
		if c.Policy.Restricted {
			restricted++
		}

		if q != "" && !strings.Contains(strings.ToLower(c.Name), q) &&
			!strings.Contains(strings.ToLower(c.Ticker), q) &&
			!strings.Contains(strings.ToLower(c.FactionName), q) &&
			!strings.Contains(strings.ToLower(c.Policy.TagCSV()), q) {
			continue
		}
		if fFaction != "" && c.FactionName != fFaction {
			continue
		}
		if fBloc != "" && c.Bloc() != fBloc {
			continue
		}
		if fTag != "" && !c.Policy.HasTag(fTag) {
			continue
		}
		if fActivity != 0 && c.ActivityID != fActivity {
			continue
		}
		switch fState {
		case "open":
			if !c.Policy.Joinable {
				continue
			}
		case "shut":
			if c.Policy.Joinable {
				continue
			}
		case "restricted":
			if !c.Policy.Restricted {
				continue
			}
		case "untagged":
			if len(c.Policy.Tags) > 0 {
				continue
			}
		case "bots":
			if c.Bots == 0 {
				continue
			}
		}

		var labels []string
		for _, t := range c.Policy.Tags {
			labels = append(labels, corpTagLabel(t))
		}
		rows = append(rows, corpListRow{
			CorpFacts: c,
			Eligible:  counts[c.CorpID],
			TagText:   strings.Join(labels, ", "),
		})
	}

	sortKey, sortDir, sortHeads := sortCorpRows(npcSortSpec, rows,
		r.URL.Query().Get("sort"), r.URL.Query().Get("dir"), r.URL.Query())

	stats, err := m.corpRecruitmentStats(ctx)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	pending, err := m.pendingApplicationCounts(ctx)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	waiting := 0
	for _, n := range pending {
		waiting += n
	}

	espRender(w, "rolebots_npccorps.html", map[string]any{
		"Page":       "rolebot-corps",
		"Tab":        "npc",
		"Msg":        r.URL.Query().Get("msg"),
		"Err":        r.URL.Query().Get("err"),
		"Stats":      stats,
		"Overview":   buildNPCOverview(corps),
		"Waiting":    waiting,
		"Rows":       rows,
		"Total":      len(corps),
		"Shown":      len(rows),
		"Open":       open,
		"Restricted": restricted,
		"Factions":   sortedKeys(factions),
		"Activities": activityOptions(activities),
		"Tags":       corpTagCatalog,
		"Q":          r.URL.Query().Get("q"),
		"FFaction":   fFaction,
		"FBloc":      fBloc,
		"FTag":       fTag,
		"FState":     fState,
		"FActivity":  fActivity,
		"Sort":       sortKey,
		"Dir":        sortDir,
		"SortHeads":  sortHeads,
		"Query":      r.URL.RawQuery,
	})
}

const tmplCorpsNPC = corpTabs + `{{define "content"}}` + castTabsStyle + rbTableStyle + `
<h2>Corporations</h2>
{{template "rb_corptabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="stats">
  <div class="stat"><div class="label">NPC corporations</div><div class="value">{{.Overview.Corps}}</div></div>
  <div class="stat"><div class="label">Open to bots</div><div class="value">{{.Overview.Open}} <span class="rbdim" style="font-size:12px">/ {{.Overview.Corps}}</span></div></div>
  <div class="stat"><div class="label">Bots working for an NPC corp</div><div class="value">{{.Overview.Bots}}</div></div>
  <div class="stat"><div class="label">Still at school</div><div class="value">{{.Overview.SchoolBots}}</div></div>
  <div class="stat"><div class="label">With an employer</div><div class="value">{{.Overview.EmployedBots}}</div></div>
  <div class="stat"><div class="label">Corporations with bots</div><div class="value">{{.Overview.WithBots}}</div></div>
</div>

<div class="card">
  <h3>Where the bots work</h3>
  {{if .Overview.Bots}}
  <div style="display:flex;flex-wrap:wrap;gap:24px">
    <div style="flex:1 1 420px">
      <h4 style="margin-top:0">By faction</h4>
      <table class="rbtight">
        <thead><tr><th>Faction</th><th>Bloc</th><th class="rbnum">Corps</th><th class="rbnum">Open</th><th class="rbnum">Bots</th><th class="rbnum">Share</th><th>Biggest employer</th></tr></thead>
        <tbody>
        {{range .Overview.Factions}}
        <tr>
          <td><a href="/rolebots/corps/npc?faction={{.Faction}}">{{.Faction}}</a></td>
          <td class="rbdim">{{.Bloc}}</td>
          <td class="rbnum">{{.Corps}}</td>
          <td class="rbnum">{{.Open}}</td>
          <td class="rbnum">{{if .Bots}}{{.Bots}}{{else}}<span class="rbdim">&mdash;</span>{{end}}</td>
          <td class="rbnum">{{if .Bots}}{{.Pct}}%{{else}}<span class="rbdim">&mdash;</span>{{end}}</td>
          <td>{{if .Top}}{{.Top}} <span class="rbdim">({{.TopBots}})</span>{{else}}<span class="rbdim">&mdash;</span>{{end}}</td>
        </tr>
        {{end}}
        </tbody>
      </table>
    </div>
    <div style="flex:1 1 320px">
      <h4 style="margin-top:0">Biggest employers</h4>
      <table class="rbtight">
        <thead><tr><th>Corporation</th><th>Faction</th><th class="rbnum">Bots</th><th>Door</th></tr></thead>
        <tbody>
        {{range .Overview.TopEmployers}}
        <tr>
          <td><a href="/rolebots/corp/{{.CorpID}}">{{.Name}}</a>{{if .Ticker}} <span class="rbdim">[{{.Ticker}}]</span>{{end}}{{if .School}} <span class="rb-pill" title="a school — where pilots start">school</span>{{end}}</td>
          <td class="rbdim">{{if .Faction}}{{.Faction}}{{else}}&mdash;{{end}}</td>
          <td class="rbnum">{{.Bots}}</td>
          <td>{{if .Open}}<span style="color:#22c55e">open</span>{{else}}<span class="rbdim">shut</span>{{end}}</td>
        </tr>
        {{end}}
        </tbody>
      </table>
    </div>
  </div>
  {{else}}
  <p>No rostered bot works for an NPC corporation yet.</p>
  {{end}}
</div>

<div class="card">
<details class="rbfold" id="rbfold-recruit" open>
<summary>Recruitment</summary>
  <p style="margin-top:8px">
    Pilots leave school, sign with an employer and apply to player corporations.
  </p>
  <p style="font-size:12px" class="rbdim">
    {{.Restricted}} corporations are shut by default (schools, ministries, armed services, CONCORD and similar).
    {{.Stats.Reviewed}} pilot(s) have looked, {{.Stats.DueSoon}} due within a day,
    {{.Stats.Moved}} have changed employer ({{.Stats.Moves}} move(s)); {{.Waiting}} application(s) are waiting.
  </p>
  <details style="margin-top:6px">
    <summary style="cursor:pointer;font-size:12px" class="rbdim">How a pilot picks an employer</summary>
    <div style="font-size:12px;margin-top:6px" class="rbdim">
      <p>
        Pilots look for work when docked and between jobs. A school is never a destination.
      </p>
      <p>
        A pilot signs with one NPC corporation and stays there. Only a <b>player</b> corporation can take it away, and
        only when a director accepts its application.
      </p>
      <p>
        A bot thrown out of a corporation returns to its school.
      </p>
    </div>
  </details>
  <p style="margin-top:8px;padding:8px;border-left:3px solid #64748b">
    <b>A pilot's faction standings are its employer's.</b> Change the corporation's standings and its pilots follow.
  </p>
  {{if .Stats.Recent}}
  <h4>Recent employer changes</h4>
  <table class="tbl">
    <tr><th>Pilot</th><th>Left</th><th>Now in</th><th>When</th></tr>
    {{range .Stats.Recent}}
    <tr>
      <td><a href="/rolebot/{{.CharID}}">{{.Name}}</a></td>
      <td>{{if .FromName}}{{.FromName}}{{else}}—{{end}}</td>
      <td>{{.ToName}}</td>
      <td>{{.When}}</td>
    </tr>
    {{end}}
  </table>
  {{end}}
</details>
</div>

<div class="card">
<details class="rbfold" id="rbfold-npc" open>
<summary>The catalogue <span class="rbcount">({{.Open}} of {{.Total}} open)</span></summary>

<div style="margin-top:10px">
  <form method="GET" style="display:flex;flex-wrap:wrap;gap:8px;align-items:center">
    <input type="hidden" name="sort" value="{{.Sort}}">
    <input type="hidden" name="dir" value="{{.Dir}}">
    <input type="text" name="q" value="{{.Q}}" placeholder="name, ticker, faction, tag" style="width:220px">
    <select name="faction">
      <option value="">every faction</option>
      {{range .Factions}}<option value="{{.}}"{{if eq . $.FFaction}} selected{{end}}>{{.}}</option>{{end}}
    </select>
    <select name="bloc">
      <option value="">every bloc</option>
      <option value="empire"{{if eq .FBloc "empire"}} selected{{end}}>empire</option>
      <option value="fringe"{{if eq .FBloc "fringe"}} selected{{end}}>unaligned</option>
      <option value="outlaw"{{if eq .FBloc "outlaw"}} selected{{end}}>outlaw</option>
    </select>
    <select name="activity">
      <option value="0">every activity</option>
      {{range .Activities}}<option value="{{.ID}}"{{if eq .ID $.FActivity}} selected{{end}}>{{.Label}}</option>{{end}}
    </select>
    <select name="tag">
      <option value="">every tag</option>
      {{range .Tags}}<option value="{{.Key}}"{{if eq .Key $.FTag}} selected{{end}}>{{.Label}}</option>{{end}}
    </select>
    <select name="state">
      <option value="">open and shut</option>
      <option value="open"{{if eq .FState "open"}} selected{{end}}>open only</option>
      <option value="shut"{{if eq .FState "shut"}} selected{{end}}>shut only</option>
      <option value="bots"{{if eq .FState "bots"}} selected{{end}}>with bots</option>
      <option value="restricted"{{if eq .FState "restricted"}} selected{{end}}>closed by default</option>
      <option value="untagged"{{if eq .FState "untagged"}} selected{{end}}>untagged only</option>
    </select>
    <button type="submit">Filter</button>
    <a href="/rolebots/corps/npc">clear</a>
    <span style="margin-left:12px" class="rbdim">Bulk</span>
  </form>
  <form method="POST" style="display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin-top:6px">
    <input type="hidden" name="action" value="bulk">
    <input type="hidden" name="back" value="{{.Query}}">
    <select name="mode">
      <option value="open">open to bots</option>
      <option value="shut">shut to bots</option>
      <option value="retag">reset tags to the derived defaults</option>
    </select>
    <select name="scopeFaction">
      <option value="">every faction</option>
      {{range .Factions}}<option value="{{.}}">{{.}}</option>{{end}}
    </select>
    <select name="scopeTag">
      <option value="">every tag</option>
      {{range .Tags}}<option value="{{.Key}}">{{.Label}}</option>{{end}}
    </select>
    <button type="submit">Apply</button>
    <span style="font-size:12px" class="rbdim">Opening in bulk skips the closed-by-default rows.</span>
  </form>
</div>

<div style="margin-top:14px">
  <h3>{{.Shown}} of {{.Total}}</h3>
  <table class="rbtight">
    <thead>
    <tr>
      {{range .SortHeads}}<th{{if .On}} style="color:#22c55e"{{end}}><a href="{{.Href}}" style="color:inherit;text-decoration:none">{{.Label}}{{.Arrow}}</a></th>{{end}}
    </tr>
    </thead>
    <tbody>
    {{range .Rows}}
    <tr>
      <td><a href="/rolebots/corp/{{.CorpID}}">{{.Name}}</a>{{if .Ticker}} <span class="rbdim">[{{.Ticker}}]</span>{{end}}</td>
      <td>{{if .FactionName}}{{.FactionName}}{{else}}—{{end}} <span class="rbdim">{{.BlocLabel}}</span></td>
      <td>{{.ActivityName}}</td>
      <td class="rbwrap" title="{{.TagText}}">{{if .TagText}}{{.TagText}}{{else}}<span style="color:#ef4444">untagged</span>{{end}}</td>
      <td>{{if .SystemName}}{{.SystemName}}{{if .RegionName}} <span class="rbdim">({{.RegionName}})</span>{{end}}{{else}}—{{end}}</td>
      <td class="rbnum">{{.Stations}}</td>
      <td class="rbnum">{{if .Bots}}{{.Bots}}{{else}}<span class="rbdim">—</span>{{end}}</td>
      <td class="rbnum">{{.Eligible}}</td>
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
    <b>Bots</b> is rostered pilots working there now. <b>Pass</b> is enabled pilots who could join if the door were open.
    The <b>Door</b> button shows the state and toggles it. Every heading sorts.
  </p>
</div>
</details>
</div>

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
