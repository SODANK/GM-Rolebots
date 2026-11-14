package rolebots

// The Industry board's markup, split from its handler as espstats.go and espstats_tmpl.go are. It
// follows the module page rules stated at listKit (esptrade_tmpl.go), with industryTabs concatenated in.
const tmplIndustry = industryTabs + `{{define "content"}}
` + industryTabsStyle + listKit + `
<style>
.rb-ind-sum { display:grid; grid-template-columns:repeat(auto-fit,minmax(165px,1fr));
              gap:10px; margin-bottom:12px; }
.rb-ind-sum div { background:#170a0a; border:1px solid #2a1a1a; border-radius:6px;
                  padding:9px 11px; }
.rb-ind-sum .k { font-size:10.5px; color:#6b7280; }
.rb-ind-sum .v { font-size:19px; color:#f0e6dc; font-variant-numeric:tabular-nums; }
.rb-ind-sum .v.warn { color:#eab308; }
.rb-funnel td.bar { width:34%; }
.rb-funnel .bar span { display:block; height:9px; background:#3b82f6; border-radius:2px;
                       min-width:1px; }
.rb-ind-filters { display:flex; gap:8px; flex-wrap:wrap; align-items:flex-end;
                  margin:0 0 10px; font-size:12px; }
.rb-ind-filters label { display:flex; flex-direction:column; gap:2px; color:#9ca3af;
                        font-size:10.5px; }
.rb-chip { display:inline-block; font-size:10px; padding:1px 6px; border-radius:9px;
           border:1px solid #2a1a1a; color:#9ca3af; }
.rb-chip.block { color:#ef4444; border-color:#5b1d1d; }
.rb-chip.churn { color:#eab308; border-color:#5b4a1d; }
.rb-chip.work  { color:#22c55e; border-color:#1d5b2e; }
.rb-chip.other { color:#9ca3af; }
.rb-flag { font-size:10px; padding:1px 5px; border-radius:3px; }
.rb-flag.stalling { background:#3b1010; color:#f87171; }
.rb-flag.halted   { background:#3b1010; color:#f87171; }
.rb-flag.trip     { background:#3a3110; color:#eab308; }
.rb-none { color:#4b5563; }
#rbProjects td.n, #rbProjects th.n,
#rbFunnel td.n, #rbFunnel th.n,
#rbChokes td.n, #rbChokes th.n,
#rbBids td.n, #rbBids th.n,
#rbBlocked td.n, #rbBlocked th.n { text-align:right; }
.rb-num { font-variant-numeric:tabular-nums; }
.rb-wins a { font-size:11px; margin-right:8px; }
.rb-wins a.on { color:#f0e6dc; font-weight:bold; }
</style>

<h2>Rolebot Industry</h2>
{{template "rb_indtabs" .}}
{{if .Msg}}<div class="alert">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="card">
  <p class="muted" style="margin:0">
    Everything else that can see manufacturing sees it <b>after</b> a job
    exists. This page is the half before that: what each supplier has committed
    to, how far it has got toward putting the job on a line, and what is in the
    way. The jobs themselves, the assembly lines and the blueprint catalogue
    live on <a href="/industry/factories">Industry &rarr; Factories</a> and
    <a href="/industry/blueprints">Industry &rarr; Blueprints</a> and are
    deliberately not repeated here.
    Figures are {{.AgeSecs}}s old &mdash;
    <a href="/rolebots/industry?fresh=1">rebuild now</a>.
  </p>
</div>

{{if .Board.Projects}}
<div class="rb-ind-sum">
  <div><div class="k">projects in flight</div>
       <div class="v rb-num">{{.Total}}</div></div>
  <div><div class="k">ISK sunk into them</div>
       <div class="v rb-num">{{.Board.SpentISK}}</div></div>
  <div><div class="k">past the stall clock</div>
       <div class="v rb-num{{if .Board.Stalled}} warn{{end}}">{{.Board.Stalled}}</div></div>
  <div><div class="k">jobs baking on a line</div>
       <div class="v rb-num">{{.Board.Oven.Baking}}</div></div>
  <div><div class="k">baked, not yet listed</div>
       <div class="v rb-num">{{.Board.Oven.Delivered}}</div></div>
  <div><div class="k">halted, holding a project</div>
       <div class="v rb-num{{if .Board.Oven.HaltedWithProject}} warn{{end}}">{{.Board.Oven.HaltedWithProject}}</div></div>
</div>
{{end}}

<div class="card">
  <h3>The pipeline</h3>
  <p class="muted">
    A project is chosen at the factory, then has to reach a line: buy the
    blueprint, gather the bill of materials by digging or shopping, and install.
    The rung a pilot is on is its <b>effective</b> phase &mdash; where it is
    flying to, not the fact that it is flying &mdash; so a ship in transit
    counts against the thing it is going to do. Age is a <b>median</b>: one
    project stuck for a fortnight would drag a mean off the rung everybody else
    is standing on. A project that
    makes no progress for 24 hours is abandoned by the sim and its sunk ISK
    written off, which is what the "over a day" column counts.
  </p>
  {{if .Board.Projects}}
  <table class="grid rb-funnel" id="rbFunnel">
    <thead><tr>
      <th>Stage</th><th class="n">Projects</th><th class="bar nosort"></th>
      <th class="n">Median age</th><th class="n">Over a day</th>
      <th class="n">ISK sunk</th><th class="n">Mining trips</th>
    </tr></thead>
    <tbody>
    {{range .Board.Funnel}}
    <tr>
      <td><a href="/rolebots/industry?stage={{.Key}}">{{.Label}}</a>
          <div class="muted" style="font-size:10.5px">{{.Note}}</div></td>
      <td class="n rb-num" data-v="{{.Count}}">{{.Count}}</td>
      <td class="bar"><span style="width:{{.Count}}px;max-width:100%"></span></td>
      <td class="n" data-v="{{.MedianAgeSec}}">{{.MedianAge}}</td>
      <td class="n rb-num" data-v="{{.Over24}}">{{.Over24}}</td>
      <td class="n rb-num" data-v="{{.Spent}}">{{.SpentISK}}</td>
      <td class="n rb-num" data-v="{{.AvgTrips}}">{{.TripsStr}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <p class="rb-none">No industry projects in flight. Nobody on the roster has
     committed to building anything &mdash; check that pilots carry a supply
     trait above the floor, and that the build book is not cold.</p>
  {{end}}

  <h4>Out the other end</h4>
  {{if .Board.ThroughputErr}}
    <div class="alert alert-error">{{.Board.ThroughputErr}}</div>
  {{else if .Board.Throughput}}
  <p class="muted">
    Jobs installed per day, and how many of those have been delivered. This is
    the rate, not the jobs &mdash; the jobs are on
    <a href="/industry/factories">Factories</a>.
  </p>
  <table class="grid">
    <tr><th>Day</th><th class="n">Installed</th><th class="n">Delivered</th></tr>
    {{range .Board.Throughput}}
    <tr><td>{{.Day}}</td>
        <td class="n rb-num">{{.Installed}}</td>
        <td class="n rb-num">{{.Delivered}}</td></tr>
    {{end}}
  </table>
  {{else}}
  <p class="rb-none">No manufacturing job has been installed in the last week.</p>
  {{end}}
</div>

<div class="card">
  <h3>Chokepoints</h3>
  <p class="muted">
    Nearly every refusal in this lane is recorded as prose and nowhere else, so
    this is the log folded: runs of digits collapsed, then the fragments named
    where a rule recognises them. <b>Blocked</b> means the project cannot
    proceed, <b>churn</b> means it is repeating itself, <b>work</b> means it is
    getting on with it. Anything with no rule keeps its own row rather than
    being hidden &mdash; an unnamed row near the top is a failure mode nobody
    has written down yet. The <b>per pilot</b> column is the reading: a big line
    count over a small pilot count is a handful of bots looping, the same count
    spread over thousands is the roster doing something normal.
  </p>
  <p class="rb-wins">
    Window:
    {{range .Windows}}<a href="/rolebots/industry?win={{.Key}}"{{if eq $.Win .Key}} class="on"{{end}}>{{.Label}}</a>{{end}}
    <span class="muted">&mdash; the fold is a full scan of the window; it runs
    in the background and is refreshed every five minutes.</span>
  </p>
  {{if .ChokeErr}}
    <div class="alert alert-error">{{.ChokeErr}}</div>
  {{else if .Chokes}}
  <p class="muted">{{.ChokeLines}} log line(s) in this window{{if .ChokeAge}}, folded {{.ChokeAge}}{{end}}{{if .ChokeBuilding}} &mdash; a fresh fold is running{{end}}.</p>
  <table class="grid" id="rbChokes">
    <thead><tr>
      <th>Reason</th><th>Kind</th><th>Where</th>
      <th class="n">Lines</th><th class="n">Pilots</th><th class="n">Per pilot</th>
    </tr></thead>
    <tbody>
    {{range .Chokes}}
    <tr>
      <td>{{if .Named}}<b>{{.Name}}</b>{{else}}<span class="muted">{{.Name}}</span>{{end}}
          {{if .Note}}<div class="muted" style="font-size:10.5px">{{.Note}}</div>{{end}}</td>
      <td><span class="rb-chip {{.Kind}}">{{.Kind}}</span></td>
      <td class="muted">{{.Category}}</td>
      <td class="n rb-num" data-v="{{.Lines}}">{{.LinesStr}}</td>
      <td class="n rb-num" data-v="{{.Pilots}}">{{.PilotsStr}}</td>
      <td class="n rb-num">{{.PerPilot}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else if .ChokeBuilding}}
  <p class="rb-none">The fold over this window is being computed in the
     background &mdash; reload in a few seconds.</p>
  {{else}}
  <p class="rb-none">Nothing logged in this window &mdash; either the roster is
     idle or the window is shorter than the gap between passes.</p>
  {{end}}
</div>

<div class="card">
  <h3>What the projects are waiting on</h3>
  <p class="muted">
    Scoped to <b>materials</b>, which is what makes it a different question from
    the <a href="/rolebots/wants">wants list</a> (which ranks products) and the
    <a href="/rolebots/minerals">mineral supply page</a> (which maps regional
    stock). A ship hull here is not a mistake &mdash; a Coercer is the material
    a Heretic blueprint eats. The verdict column is the reading: "nothing on
    sale anywhere" and "on sale somewhere but not here" are opposite problems
    with opposite fixes. <b>Minerals lead and the two halves are not
    comparable</b>: a mineral figure is what is short right now, a non-mineral
    figure is a running total of unmet asks since the ledger was last cleared,
    so the table is banded rather than ranked across both.
  </p>
  {{if .Board.BlockedErr}}
    <div class="alert alert-error">{{.Board.BlockedErr}}</div>
  {{else if .Board.Blocked}}
  <table class="grid" id="rbBlocked">
    <thead><tr>
      <th>Material</th><th class="n">Units wanted</th><th>Basis</th>
      <th class="n">Stations asking</th>
      <th class="n">On sale, game-wide</th><th>Verdict</th>
    </tr></thead>
    <tbody>
    {{range .Board.Blocked}}
    <tr>
      <td>{{.TypeName}} <span class="muted">#{{.TypeID}}</span>
          {{if .Mineral}}<span class="rb-chip">mineral</span>{{end}}</td>
      <td class="n rb-num" data-v="{{.Units}}">{{.UnitsStr}}</td>
      <td class="muted">{{.Basis}}</td>
      <td class="n rb-num" data-v="{{.Stations}}">{{.Stations}}</td>
      <td class="n rb-num" data-v="{{.OnMarket}}">{{.MarketStr}}</td>
      <td class="muted">{{.Verdict}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <p class="rb-none">No material line is recorded as short. Either every bill is
     being filled, or nothing has committed to a project recently enough for the
     shortfall board to hold a row.</p>
  {{end}}

  <h4>Money on the counter</h4>
  <p class="muted">
    When a project commits to a bill line it can neither dig at its comfort line
    nor buy in reach, it leaves a standing buy order at its own factory and gets
    on with the rest. A row here is a factory holding ISK in escrow waiting for
    somebody to fly the goods in; a <b>fill</b> column of zeroes is the supply
    chain not answering.
  </p>
  {{if .Board.BidsErr}}
    <div class="alert alert-error">{{.Board.BidsErr}}</div>
  {{else if .Board.Bids}}
  <table class="grid" id="rbBids">
    <thead><tr>
      <th>Type</th><th class="n">Bids</th><th class="n">Stations</th>
      <th class="n">Escrow</th><th class="n">Units still wanted</th>
      <th class="n">Filled</th><th class="n">Oldest</th>
    </tr></thead>
    <tbody>
    {{range .Board.Bids}}
    <tr>
      <td>{{.TypeName}} <span class="muted">#{{.TypeID}}</span></td>
      <td class="n rb-num" data-v="{{.Bids}}">{{.Bids}}</td>
      <td class="n rb-num" data-v="{{.Stations}}">{{.Stations}}</td>
      <td class="n rb-num" data-v="{{.Escrow}}">{{.EscrowISK}}</td>
      <td class="n rb-num" data-v="{{.Units}}">{{.UnitsStr}}</td>
      <td class="n rb-num" data-v="{{.FillPct}}">{{.FillPct}}%</td>
      <td class="n" data-v="{{.OldestAt}}">{{.Oldest}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <p class="rb-none">No factory has a standing supply bid out. Either every bill
     is being covered locally, or no project has reached the point of asking.</p>
  {{end}}
</div>

<div class="card">
  <h3>Projects</h3>
  <p class="muted">
    One row per committed project, oldest first &mdash; the top of this table is
    the ISK with a deadline on it. <b>Evidence</b> is the build-book tier that
    chose the product; <b>doing</b> is the sim's own description of the phase,
    so it cannot drift from what the pilot thinks it is doing. For the bill of
    materials, the priced build book and the shelf, open a pilot's own factory
    page &mdash; that is a market sweep per pilot and deliberately not run here.
  </p>
  <form class="rb-ind-filters" method="GET" action="/rolebots/industry">
    <label>Stage
      <select name="stage">
        <option value="">any</option>
        {{range .Stages}}<option value="{{.Key}}"{{if eq $.Stage .Key}} selected{{end}}>{{.Label}}</option>{{end}}
      </select>
    </label>
    <label>Evidence
      <select name="evidence">
        <option value="">any</option>
        {{range $k := .Evidences}}<option value="{{$k}}"{{if eq $.Evidence $k}} selected{{end}}>{{$k}}</option>{{end}}
      </select>
    </label>
    <label>Product or pilot
      <input type="text" name="q" value="{{.Query}}" style="width:12em">
    </label>
    <label>&nbsp;
      <span><input type="checkbox" name="stalled" value="1"{{if .AtRisk}} checked{{end}}> at risk only</span>
    </label>
    <input type="hidden" name="win" value="{{.Win}}">
    <button type="submit">Filter</button>
  </form>
  {{if .Rows}}
  <p class="muted">{{.Matched}} of {{.Total}} project(s).</p>
  <div class="rb-scroll">
  <table class="grid" id="rbProjects">
    <thead><tr>
      <th>Pilot</th><th>Product</th><th>Evidence</th><th class="n">Runs</th>
      <th>Stage</th><th>Doing</th><th class="n">Age</th><th class="n">Trips</th>
      <th class="n">ISK sunk</th><th>Factory</th><th>Region</th><th></th>
    </tr></thead>
    <tbody>
    {{range .Rows}}
    <tr>
      <td><a href="/rolebot/{{.CharID}}/factory">{{.Name}}</a></td>
      <td>{{.Product}} <span class="muted">#{{.TypeID}}</span></td>
      <td><b>{{.Evidence}}</b><div class="muted" style="font-size:10.5px">{{.Why}}</div></td>
      <td class="n rb-num" data-v="{{.Runs}}">{{.Runs}}</td>
      <td>{{.Stage}}</td>
      <td class="muted">{{if .Doing}}{{.Doing}}{{else}}&mdash;{{end}}</td>
      <td class="n" data-v="{{.AgeSec}}">{{.Age}}</td>
      <td class="n rb-num" data-v="{{.Trips}}">{{.TripsStr}}</td>
      <td class="n rb-num" data-v="{{.Spent}}">{{.SpentISK}}</td>
      <td>{{if .Factory}}{{.Factory}}{{else}}<span class="muted">not settled</span>{{end}}</td>
      <td class="muted">{{.Region}}</td>
      <td>{{if .Halted}}<span class="rb-flag halted">halted</span>
          {{else if .Stalled}}<span class="rb-flag stalling">stalling</span>
          {{else if .TripCap}}<span class="rb-flag trip">trip cap</span>{{end}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  <p>
    {{if .HasPrev}}<a href="/rolebots/industry{{.PrevQ}}">&larr; previous</a>{{end}}
    {{if .HasNext}}<a href="/rolebots/industry{{.NextQ}}" style="margin-left:12px">next &rarr;</a>{{end}}
  </p>
  {{else if .Board.Projects}}
  <p class="rb-none">No project matches those filters. {{.Total}} project(s) in
     flight overall.</p>
  {{else}}
  <p class="rb-none">No industry projects in flight.</p>
  {{end}}
</div>

<script>
rbSortable(document.getElementById('rbFunnel'));
rbSortable(document.getElementById('rbChokes'));
rbSortable(document.getElementById('rbBlocked'));
rbSortable(document.getElementById('rbBids'));
rbSortable(document.getElementById('rbProjects'));
</script>
{{end}}`
