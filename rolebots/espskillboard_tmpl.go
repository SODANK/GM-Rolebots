package rolebots

// The Skills tab's markup, split from its handler as espore.go and espore_tmpl.go are. It follows the
// module page rules stated at listKit (esptrade_tmpl.go); espskillboard_render_test.go guards that
// castTabs is concatenated in.
const tmplSkillBoard = castTabs + `{{define "content"}}
` + castTabsStyle + listKit + `
<style>
.rb-sk-sum { display:grid; grid-template-columns:repeat(auto-fit,minmax(165px,1fr));
             gap:10px; margin-bottom:12px; }
.rb-sk-sum div { background:#170a0a; border:1px solid #2a1a1a; border-radius:6px;
                 padding:9px 11px; }
.rb-sk-sum .k { font-size:10.5px; color:#6b7280; }
.rb-sk-sum .v { font-size:19px; color:#f0e6dc; font-variant-numeric:tabular-nums; }
.rb-sk-sum .v.good { color:#4ade80; }
.rb-sk-sum .v.warn { color:#b45309; }
.rb-sk-sum .v.bad  { color:#dc2626; }
.rb-sk td, .rb-sk th { font-size:12px; padding:4px 6px; }
.rb-sk td.n, .rb-sk th.n { text-align:right; font-variant-numeric:tabular-nums; }
.rb-sk .sub { color:#6b7280; font-size:11px; }
.rb-none { color:#4b5563; }
.rb-bar { height:5px; background:#2a1a1a; border-radius:3px; overflow:hidden; margin-top:2px; }
.rb-bar i { display:block; height:100%; background:#4b7f52; }
.rb-bar.amber i { background:#b45309; }
.rb-bar.blue i { background:#2f6f8f; }
.rb-hist { display:grid; grid-template-columns:130px 1fr 90px 80px; gap:4px 10px;
           align-items:center; font-size:12px; }
.rb-hist .lab { color:#9ca3af; }
.rb-hist .num { text-align:right; font-variant-numeric:tabular-nums; color:#f0e6dc; }
.rb-hist .pc { text-align:right; font-variant-numeric:tabular-nums; color:#6b7280; }
.rb-two { display:grid; grid-template-columns:repeat(auto-fit,minmax(360px,1fr));
          gap:12px; align-items:start; }
.rb-lever { display:flex; gap:8px; align-items:center; flex-wrap:wrap; margin:6px 0 0; }
.rb-lever input[type=number] { width:90px; }
.rb-filter { width:100%; max-width:340px; margin-bottom:6px; }
</style>

<h2>Roster Skills</h2>
{{template "rb_casttabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
{{range .B.Notes}}<div class="alert alert-error">Panel unavailable — {{.}}</div>{{end}}

<div class="card">
  <p class="muted" style="margin:0">
    Skill training across the whole roster. Assembled {{.B.Age}}.
    <a href="/rolebots/skills?fresh=1">Rebuild now</a>.
  </p>
</div>

<div class="rb-sk-sum">
  <div><div class="k">pilots on the roster</div>
       <div class="v">{{.B.Roster}} <span style="font-size:12px;color:#6b7280">{{.B.Enabled}} enabled</span></div></div>
  <div><div class="k">median skill points</div>
       <div class="v">{{.B.MedianSPStr}}</div></div>
  <div><div class="k">mean, for comparison</div>
       <div class="v">{{.B.MeanSPStr}}</div></div>
  <div><div class="k">training something now</div>
       <div class="v good">{{.B.Training}}</div></div>
  <div><div class="k">training nothing</div>
       <div class="v{{if .B.Idle}} bad{{end}}">{{.B.Idle}}</div></div>
  <div><div class="k">distinct skills held, of all there are</div>
       <div class="v">{{.B.DistinctSkills}} <span style="font-size:12px;color:#6b7280">of {{.B.CatalogueSkills}}</span></div></div>
</div>

<div class="card">
  <h3 style="margin-top:0">Reconsider, and how far ahead they plan</h3>
  <p class="muted" style="margin:0 0 8px">
    <b>Reconsider</b> marks every pilot's plan stale. Pilots replan gradually and keep training meanwhile.
  </p>
  <p class="muted" style="margin:0 0 8px">
    <b>Plan horizon</b> is how much training one plan lays down. At {{.B.HorizonStr}}, a patient pilot
    plans {{.B.PatientStr}} ahead and a hurried one {{.B.HurriedStr}}. Plans are capped at
    <b>{{.MaxSteps}} steps</b>. Default: {{.HorizonShip}}.
  </p>
  <div class="rb-lever">
    <form method="POST" style="display:inline">
      <input type="hidden" name="action" value="replan-all">
      <button type="submit">Reconsider every pilot's training</button>
    </form>
    {{if .B.SweepAsked}}
    <form method="POST" style="display:inline">
      <input type="hidden" name="action" value="replan-stop">
      <button type="submit">Clear the sweep</button>
    </form>
    {{end}}
    <form method="POST" style="display:inline">
      <input type="hidden" name="action" value="horizon">
      <label class="muted">plan
        <input type="number" name="hours" min="{{.HorizonMinH}}" max="{{.HorizonMaxH}}"
               step="1" value="{{.B.HorizonHours}}"> hours at a time</label>
      <button type="submit">Save</button>
    </form>
    <form method="POST" style="display:inline">
      <input type="hidden" name="action" value="refine-floor">
      <label class="muted">force Refining to
        <input type="number" name="level" min="0" max="{{.RefineMaxLv}}"
               step="1" value="{{.RefineFloor}}"> (0 = off)</label>
      <button type="submit">Save</button>
    </form>
  </div>
  <p class="muted" style="margin:10px 0 0">
    <b>The refining floor</b> forces every pilot to train Refining to that level. Pilots only refine
    ore once their yield clears {{.RefineBar}}. At 0 it is off.
  </p>
  {{if .B.SweepAsked}}
  <p class="muted" style="margin:10px 0 2px">
    Sweep asked for <b>{{.B.SweepStamp}}</b>. <b>{{.B.Swept}}</b> done, <b>{{.B.Sweeping}}</b> to go,
    about <b>{{.B.SweepETA}}</b> left.
  </p>
  <div class="rb-bar blue"><i style="width:{{.B.SweepPc}}%"></i></div>
  {{else}}
  <p class="muted" style="margin:10px 0 0">
    No sweep running. Plans rebuild about every {{.ReplanMin}} minutes per pilot, and when a queue empties.
  </p>
  {{end}}
</div>

<div class="rb-two">
  <div class="card">
    <h3 style="margin-top:0">Where the skill-point curve is</h3>
    <p class="muted" style="margin:0 0 8px">
      Total SP per pilot. Each band is double the last.
    </p>
    <div class="rb-hist">
      {{range .B.Bands}}
      <div class="lab">{{.Label}}</div>
      <div><div class="rb-bar"><i style="width:{{.SharePc}}%"></i></div></div>
      <div class="num">{{.Pilots}}</div>
      <div class="pc">{{.ShareStr}}</div>
      {{end}}
    </div>
    <p class="muted" style="margin:10px 0 0">
      10th percentile <b>{{.B.P10Str}}</b>, quartiles <b>{{.B.P25Str}}</b> /
      <b>{{.B.MedianSPStr}}</b> / <b>{{.B.P75Str}}</b>, 90th <b>{{.B.P90Str}}</b>, top
      <b>{{.B.MaxSPStr}}</b>. The richest tenth holds <b>{{.B.TopDecileStr}}</b> of all
      {{.B.TotalSPStr}} SP. Median pilot knows <b>{{.B.MedianSkills}}</b> skills, mean {{.B.MeanSkillsStr}}.{{if .B.NoSheet}}
      <b>{{.B.NoSheet}}</b> pilot(s) have no skill sheet.{{end}}
    </p>
  </div>

  <div class="card">
    <h3 style="margin-top:0">Where the points went</h3>
    <p class="muted" style="margin:0 0 8px">
      Skill points by group, whole roster.
    </p>
    <table class="rb-sk rb-sortable">
      <thead><tr>
        <th>Group</th><th class="n">Skills</th><th class="n">Sheets</th>
        <th class="n">SP</th><th class="n">Share</th>
      </tr></thead>
      <tbody>
        {{range .B.Groups}}
        <tr>
          <td>{{.Name}}</td>
          <td class="n" data-v="{{.Skills}}">{{.Skills}}</td>
          <td class="n" data-v="{{.Holders}}">{{.Holders}}</td>
          <td class="n" data-v="{{.SP}}">{{.SPStr}}</td>
          <td class="n" data-v="{{.Share}}">{{.ShareStr}}
            <div class="rb-bar"><i style="width:{{.SharePc}}%"></i></div></td>
        </tr>
        {{else}}
        <tr><td colspan="5" class="rb-none">No skill sheets on the roster.</td></tr>
        {{end}}
      </tbody>
    </table>
  </div>
</div>

<div class="card">
  <h3 style="margin-top:0">What is training right now</h3>
  <p class="muted" style="margin:0 0 8px">
    Skills being trained this moment. <b>Levels</b> is the mix of target levels, biggest first
    (whole front: {{.B.FrontLevelStr}}). <b>Planned</b> is the share of pilots whose current plan
    contains the skill. The rest got it as a stopgap for an empty queue.
    Overall <b>{{.B.PlannedFrontStr}}</b> is planned.
  </p>
  <input class="rb-filter" data-target="fronttab" placeholder="filter by skill or group&hellip;">
  <table class="rb-sk rb-sortable" id="fronttab">
    <thead><tr>
      <th>Skill</th><th>Group</th><th class="n">Pilots</th><th class="n">Share</th>
      <th>Levels</th><th class="n">Planned</th>
    </tr></thead>
    <tbody>
      {{range .B.Front}}
      <tr>
        <td>{{.Name}}</td>
        <td class="sub">{{.Group}}</td>
        <td class="n" data-v="{{.Pilots}}">{{.Pilots}}</td>
        <td class="n" data-v="{{.Share}}">{{.ShareStr}}</td>
        <td class="sub">{{.LevelStr}}</td>
        <td class="n" data-v="{{.PlannedShare}}">{{.PlannedStr}}
          <div class="rb-bar{{if lt .PlannedPc 50}} amber{{end}}"><i style="width:{{.PlannedPc}}%"></i></div></td>
      </tr>
      {{else}}
      <tr><td colspan="6" class="rb-none">Nobody on the roster is training anything.</td></tr>
      {{end}}
    </tbody>
  </table>
</div>

<div class="rb-two">
  <div class="card">
    <h3 style="margin-top:0">Overlap</h3>
    <p class="muted" style="margin:0 0 8px">
      The roster holds {{.B.DistinctSkills}} of {{.B.CatalogueSkills}} skills. <b>{{.B.CoreSkills}}</b> are
      held by {{.CoreShare}} or more of pilots. The average pilot has {{.B.CorePerPilotStr}} of them
      out of {{.B.MeanSkillsStr}} skills.
    </p>
    <div class="rb-hist">
      {{range .B.CoverBands}}
      <div class="lab">{{.Label}}</div>
      <div><div class="rb-bar amber"><i style="width:{{.SharePc}}%"></i></div></div>
      <div class="num">{{.Skills}}</div>
      <div class="pc">{{.SharePc}}%</div>
      {{end}}
    </div>
    <p class="muted" style="margin:10px 0 0">
      The ten biggest skills in training hold <b>{{.B.FrontTop10Str}}</b> of it. The training mix is as
      varied as <b>{{.B.FrontEffectiveStr}}</b> evenly trained skills.
    </p>
  </div>

  <div class="card">
    <h3 style="margin-top:0">What is queued behind it</h3>
    <p class="muted" style="margin:0 0 8px">
      <b>{{.B.QueueSteps}}</b> step(s) waiting across <b>{{.B.Queued}}</b> pilot(s), about
      <b>{{.B.QueueDepthStr}}</b> each. <b>{{.B.QueueEmpty}}</b> pilot(s) have nothing queued.
    </p>
    <table class="rb-sk rb-sortable">
      <thead><tr>
        <th>Skill</th><th>Group</th><th class="n">Steps</th><th class="n">Share</th>
        <th class="n">Avg level</th>
      </tr></thead>
      <tbody>
        {{range .B.QueueRows}}
        <tr>
          <td>{{.Name}}</td>
          <td class="sub">{{.Group}}</td>
          <td class="n" data-v="{{.Holders}}">{{.Holders}}</td>
          <td class="n" data-v="{{.Share}}">{{.ShareStr}}</td>
          <td class="n" data-v="{{.AvgLevel}}">{{.AvgLevelStr}}</td>
        </tr>
        {{else}}
        <tr><td colspan="5" class="rb-none">Nothing is queued.</td></tr>
        {{end}}
      </tbody>
    </table>
  </div>
</div>

<div class="card">
  <h3 style="margin-top:0">Who knows what</h3>
  <p class="muted" style="margin:0 0 8px">
    Every skill held on the roster, most common first, with average level.
  </p>
  <input class="rb-filter" data-target="covertab" placeholder="filter by skill or group&hellip;">
  <table class="rb-sk rb-sortable" id="covertab">
    <thead><tr>
      <th>Skill</th><th>Group</th><th class="n">Pilots</th><th class="n">Share of roster</th>
      <th class="n">Avg level</th><th class="n">SP held</th>
    </tr></thead>
    <tbody>
      {{range .B.Cover}}
      <tr>
        <td>{{.Name}}</td>
        <td class="sub">{{.Group}}</td>
        <td class="n" data-v="{{.Holders}}">{{.Holders}}</td>
        <td class="n" data-v="{{.Share}}">{{.ShareStr}}
          <div class="rb-bar"><i style="width:{{.SharePc}}%"></i></div></td>
        <td class="n" data-v="{{.AvgLevel}}">{{.AvgLevelStr}}</td>
        <td class="n" data-v="{{.SP}}">{{.SPStr}}</td>
      </tr>
      {{else}}
      <tr><td colspan="6" class="rb-none">Nobody on the roster holds a skill.</td></tr>
      {{end}}
    </tbody>
  </table>
</div>

<div class="card">
  <h3 style="margin-top:0">How a pilot picks</h3>
  <p class="muted" style="margin:0 0 8px">
    Pilots rank goals by value per minute of training, including prerequisites. Learning skills are valued
    by the time they give back, capped at {{.LearnHorizon}}.
  </p>
  <p class="muted" style="margin:0 0 8px">
    {{.B.KindSteps}} planned step(s) by the kind of goal behind each. <b>Waiting on a book</b> means planned
    but not queued yet: <b>{{.B.KindPending}}</b> step(s).
  </p>
  <table class="rb-sk rb-sortable">
    <thead><tr>
      <th>Because&hellip;</th><th class="n">Steps</th><th class="n">Share</th>
      <th class="n">Pilots</th><th class="n">Days of training</th><th class="n">Waiting on a book</th>
    </tr></thead>
    <tbody>
      {{range .B.Kinds}}
      <tr>
        <td>{{.Label}}<div class="sub">{{.Blurb}}</div></td>
        <td class="n" data-v="{{.Steps}}">{{.Steps}}</td>
        <td class="n" data-v="{{.Share}}">{{.ShareStr}}
          <div class="rb-bar"><i style="width:{{.SharePc}}%"></i></div></td>
        <td class="n" data-v="{{.Pilots}}">{{.Pilots}}</td>
        <td class="n" data-v="{{.Hours}}">{{.DaysStr}}</td>
        <td class="n" data-v="{{.Pending}}">{{.Pending}}</td>
      </tr>
      {{else}}
      <tr><td colspan="6" class="rb-none">No plans have been recorded yet.</td></tr>
      {{end}}
    </tbody>
  </table>
</div>

<div class="rb-two">
  <div class="card">
    <h3 style="margin-top:0">How far the planner reaches</h3>
    <p class="muted" style="margin:0 0 8px">
      <b>{{.B.PlansPerPass}}</b> pilot(s) replan per pass, so a full circuit takes about
      <b>{{.B.PlanReachStr}}</b> against a target of {{.ReplanMin}} minutes. Age of current plans:
    </p>
    <div class="rb-hist">
      {{range .B.PlanAges}}
      <div class="lab">{{.Label}}</div>
      <div><div class="rb-bar blue"><i style="width:{{.SharePc}}%"></i></div></div>
      <div class="num">{{.Pilots}}</div>
      <div class="pc">{{.ShareStr}}</div>
      {{end}}
    </div>
    <p class="muted" style="margin:10px 0 0">
      Unqueued pilots get up to {{.FloorSteps}} stopgap steps ({{.FloorPerPass}} pilots a pass) until
      their real plan.{{if .B.AutoOff}} <b>{{.B.AutoOff}}</b> pilot(s) have training automation off.{{end}}
    </p>
  </div>

  <div class="card">
    <h3 style="margin-top:0">The dials, as the roster carries them</h3>
    <p class="muted" style="margin:0 0 8px">
      <b>Satisfied level</b> is where a wanted skill stops being urgent (set by patience).
      <b>Group cap</b> is the most steps of one skill group in a plan (set by wanderlust).
      Defaults: satisfied at <b>{{.Satisfied}}</b>, <b>{{.GroupCap}}</b> steps per group.
    </p>
    <div class="rb-hist">
      {{range .B.Satisfied}}
      <div class="lab">{{.Label}}</div>
      <div><div class="rb-bar"><i style="width:{{.SharePc}}%"></i></div></div>
      <div class="num">{{.Pilots}}</div>
      <div class="pc">{{.ShareStr}}</div>
      {{end}}
      {{range .B.GroupCaps}}
      <div class="lab">{{.Label}}</div>
      <div><div class="rb-bar amber"><i style="width:{{.SharePc}}%"></i></div></div>
      <div class="num">{{.Pilots}}</div>
      <div class="pc">{{.ShareStr}}</div>
      {{end}}
    </div>
    <p class="muted" style="margin:10px 0 0">
      Same for everyone: steps up to <b>{{.StepCap}}</b>, at most <b>{{.MaxSteps}}</b> steps per plan,
      <b>{{.EvalBudget}}</b> fits re-judged, and a <b>{{.WalletFloor}}</b> wallet floor that book buying never touches.
    </p>
  </div>
</div>

{{if .B.Prefs}}
<div class="card">
  <h3 style="margin-top:0">Operator overrides</h3>
  <p class="muted" style="margin:0 0 8px">
    Pins and bans set on individual pilots. A pin trains a skill to V first. A ban removes it, and
    anything needing it, from the pilot's reach.
  </p>
  <table class="rb-sk rb-sortable">
    <thead><tr><th>Skill</th><th>Override</th><th class="n">Pilots</th></tr></thead>
    <tbody>
      {{range .B.Prefs}}
      <tr><td>{{.Name}}</td><td>{{.Mode}}</td>
          <td class="n" data-v="{{.Pilots}}">{{.Pilots}}</td></tr>
      {{end}}
    </tbody>
  </table>
</div>
{{end}}

<script>
// The filter box over the training front. listKit carries the sorter and
// nothing else, so this is the page's own — one pass over the rows hiding
// anything that does not contain the typed text. Cheap enough at forty rows
// that there is nothing to debounce.
(function () {
  // Every box on the page, not just the first: two tables carry one, and a
  // handler bound to querySelector leaves the second one inert — which is a
  // filter box that silently does nothing, the worst of the three states.
  Array.prototype.forEach.call(document.querySelectorAll('.rb-filter'), function (box) {
    var table = document.getElementById(box.getAttribute('data-target'));
    if (!table || !table.tBodies[0]) return;
    box.addEventListener('input', function () {
      var q = box.value.toLowerCase();
      Array.prototype.forEach.call(table.tBodies[0].rows, function (r) {
        r.style.display = (r.textContent || '').toLowerCase().indexOf(q) >= 0 ? '' : 'none';
      });
    });
  });
})();
</script>
{{end}}`
