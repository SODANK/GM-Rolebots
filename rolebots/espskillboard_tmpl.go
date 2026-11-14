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
  <p class="muted" style="margin:0 0 6px">
    Every pilot's own queue is on its own page; this is the population. Where it sits on the
    skill-point curve, what it is training <em>right now</em>, how much of that is the same
    handful of skills over and over, and by what arithmetic any of it was chosen. Nothing here
    is stored &mdash; it is recomputed from the sheets, the training rows and the plans
    themselves, so it cannot drift from what the bots are actually doing.
  </p>
  <p class="muted" style="margin:0">
    Assembled {{.B.Age}} and held for a couple of minutes, because it is two grouped scans of
    every rostered pilot's skill entities. <a href="/rolebots/skills?fresh=1">Rebuild it now</a>.
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
    Two levers over the whole roster. Neither plans anything itself: a plan is the most
    expensive arithmetic in the module and there are {{.B.Roster}} pilots, so ten thousand of
    them cannot happen inside a button press. <b>Reconsider</b> marks every pilot's plan as
    stale, and the skill lane &mdash; which already runs in batches, idle pilots first, then
    whoever has waited longest &mdash; drains them at its own pace. Nothing is interrupted:
    each pilot keeps training what it has until its own turn comes.
  </p>
  <p class="muted" style="margin:0 0 8px">
    <b>Plan horizon</b> is how much training one plan lays down. It is the centre of a
    personality spread, not a flat setting: at {{.B.HorizonStr}}, an unhurried pilot plans
    {{.B.PatientStr}} ahead and a hurried one {{.B.HurriedStr}}. Longer plans mean the lane
    has to come back to a pilot less often, which on a roster this size is the difference
    between a queue that runs dry and one that does not. A plan is still capped at
    <b>{{.MaxSteps}} steps</b> whatever the horizon says, so past a point that cap is what
    binds rather than the time. Shipped default: {{.HorizonShip}}.
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
    <b>The refining floor</b> is the blunt instrument for the ore economy. A pilot only refines
    instead of feeding its rock to an NPC bid once its after-tax yield clears {{.RefineBar}},
    which Refining&nbsp;V alone does and Refining&nbsp;IV alone does not &mdash; and Refining is
    a rank&nbsp;1 skill, so buying the whole roster that level costs hours. Left at 0 the pair is
    still offered on merit like any other lever; set, it rides the same pinned path an operator
    override does and the arithmetic is not consulted. The Ore page counts who is currently short.
  </p>
  {{if .B.SweepAsked}}
  <p class="muted" style="margin:10px 0 2px">
    Sweep asked for <b>{{.B.SweepStamp}}</b> &mdash; <b>{{.B.Swept}}</b> pilot(s) have been
    round since, <b>{{.B.Sweeping}}</b> still to go, roughly <b>{{.B.SweepETA}}</b> left at the
    pace the lane is currently managing.
  </p>
  <div class="rb-bar blue"><i style="width:{{.B.SweepPc}}%"></i></div>
  {{else}}
  <p class="muted" style="margin:10px 0 0">
    No sweep running. Plans are rebuilt on the ordinary cadence: roughly every
    {{.ReplanMin}} minutes per pilot when the lane can keep up, and immediately for a pilot
    whose queue has emptied (with a {{.IdleReplan}}-minute cool-down so an unplannable one
    cannot monopolise the pass).
  </p>
  {{end}}
</div>

<div class="rb-two">
  <div class="card">
    <h3 style="margin-top:0">Where the skill-point curve is</h3>
    <p class="muted" style="margin:0 0 8px">
      Total SP per pilot. The bands double because SP does &mdash; the distance from a fresh
      pilot to a useful one is the same number of doublings as from a useful one to a
      specialist, and linear bands would put nine tenths of any roster in the first bar.
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
      Quartiles: <b>{{.B.P10Str}}</b> at the tenth percentile, <b>{{.B.P25Str}}</b> /
      <b>{{.B.MedianSPStr}}</b> / <b>{{.B.P75Str}}</b>, <b>{{.B.P90Str}}</b> at the ninetieth,
      <b>{{.B.MaxSPStr}}</b> at the top. The richest tenth of the roster holds
      <b>{{.B.TopDecileStr}}</b> of all {{.B.TotalSPStr}} skill points in it &mdash; which is
      what says whether the mean above describes the population or only its oldest wave. The
      median pilot knows <b>{{.B.MedianSkills}}</b> skills; the mean is
      {{.B.MeanSkillsStr}}.{{if .B.NoSheet}} <b>{{.B.NoSheet}}</b> pilot(s) have no skill sheet
      at all.{{end}}
    </p>
  </div>

  <div class="card">
    <h3 style="margin-top:0">Where the points went</h3>
    <p class="muted" style="margin:0 0 8px">
      Skill points by group, over the whole roster. This is the population's shape as an
      economy: what it has spent its one unspendable resource on.
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
    One row per pilot in <code>chrSkillTraining</code> &mdash; not a queue, not a plan, what is
    actually running this second. <b>Levels</b> is the mix of target levels, biggest first,
    and it matters as much as the skill name: a front made of level Vs is a population
    deepening, a front made of level Is is one still unlocking. Right now the whole front
    reads {{.B.FrontLevelStr}}.
  </p>
  <p class="muted" style="margin:0 0 8px">
    <b>Planned</b> is the attribution, and it is the number to read hardest. It is the share of
    the pilots training this skill whose own current plan contains it. The rest were given it
    by the <em>floor</em> &mdash; the cheap stopgap that fills an empty queue with the next
    level of something the pilot already owns, because a pilot with an empty queue is losing
    the one resource it cannot earn back &mdash; or by a plan that has since been rewritten.
    Across the whole front, <b>{{.B.PlannedFrontStr}}</b> of what is training was asked for by
    a plan that still exists.
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
      Two things people mean by one word. <b>How narrow the education is</b>: the roster holds
      {{.B.DistinctSkills}} distinct skills of the {{.B.CatalogueSkills}} the game publishes,
      and <b>{{.B.CoreSkills}}</b> of those are held by {{.CoreShare}} or more of everybody
      &mdash; a common core the average sheet carries {{.B.CorePerPilotStr}} of, against
      {{.B.MeanSkillsStr}} skills in total.
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
      <b>How narrow the front is</b>: the ten biggest skills in training hold
      <b>{{.B.FrontTop10Str}}</b> of it, and the whole front is as varied as
      <b>{{.B.FrontEffectiveStr}}</b> skills trained evenly (1 &divide; &Sigma;&nbsp;share&sup2;).
      The effective count rather than a top-ten share because it comes out in the unit the
      question is asked in, and because it does not move when the roster grows.
    </p>
  </div>

  <div class="card">
    <h3 style="margin-top:0">What is queued behind it</h3>
    <p class="muted" style="margin:0 0 8px">
      <b>{{.B.QueueSteps}}</b> step(s) waiting across <b>{{.B.Queued}}</b> pilot(s), about
      <b>{{.B.QueueDepthStr}}</b> each; <b>{{.B.QueueEmpty}}</b> pilot(s) have nothing behind
      what they are training. Counted as steps, not pilots &mdash; a queue is a plan's residue,
      and what the roster has committed to next is the question.
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
    The overlap panel counted; this names. Every skill anybody on the roster holds, widest
    first, with the average level it is held to. The top of this table is the common core &mdash;
    the education a rolebot gets whatever it was generated to be &mdash; and the bottom of it is
    where a population's specialists actually are. <b>Training</b> beside it is how many of the
    holders are taking it further right now, which is the difference between a skill the roster
    has and one it is still buying.
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
    <b>The planning unit is a chain, not a step.</b> Marginal value alone is myopic &mdash;
    Gunnery&nbsp;I unlocks nothing on its own, so a step-at-a-time planner would never train it
    and never reach the gun behind it. A pilot prices a <em>goal</em> together with the whole
    prerequisite closure it still needs:
    <code>priority = value(goal) &divide; minutes of the whole chain</code>, and commits the
    best ratio. Committing one chain makes every chain sharing its prerequisites cheaper, so
    the cheap enabling skills &mdash; Mechanic, Navigation, Gunnery, Engineering, Electronics
    &mdash; get trained first without any list of them existing anywhere. They sit in the most
    chains, so they are the cheapest way to buy the most value, and the arithmetic finds them.
  </p>
  <p class="muted" style="margin:0 0 8px">
    Dividing by chain minutes <em>is</em> the "prefer what you can have sooner" rule &mdash; not
    a tie-break bolted on the side, the whole ranking. Learning is the one goal priced
    differently: it is worth the time it gives back, valued at whatever the best other chain
    pays per minute, capped at {{.LearnHorizon}} of foreseen training so its payback cannot run
    to infinity and win every plan forever. When every goal prices at zero the pilot still
    trains &mdash; the cheapest chain going, because a level of anything beats a level of
    nothing and training is the one thing a docked, broke, shipless pilot can still do.
  </p>
  <p class="muted" style="margin:0 0 8px">
    The table is that ranking's outcome across every current plan on the roster:
    {{.B.KindSteps}} step(s) of training, by the kind of goal that pulled each one in. It is
    the nearest thing there is to a statement of what this population is <em>for</em>.
    <b>Waiting on a book</b> is planned but not queued &mdash; the server injects a skillbook
    only when its prerequisites are <em>trained</em>, not queued, so a book behind queued levels
    is bought on a later pass rather than now &mdash; <b>{{.B.KindPending}}</b> of the
    {{.B.KindSteps}} planned steps are in that state.
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
      A plan takes eight to ten seconds of arithmetic and {{.Workers}} of them run at once, so
      the lane makes a bounded number an hour however big the roster is. It asks for
      <b>{{.B.PlansPerPass}}</b> pilot(s) each pass at the pace it is currently managing, which
      is a full circuit of the roster about every <b>{{.B.PlanReachStr}}</b> &mdash; against an
      aim of {{.ReplanMin}} minutes. Everything below that line is a pilot whose plan predates
      whatever it has bought, lost or joined since.
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
      Nobody is idle in the meantime: the floor lays down up to {{.FloorSteps}} cheap steps for
      {{.FloorPerPass}} unqueued pilots a pass, and a real plan replaces whatever it wrote when
      its turn comes.{{if .B.AutoOff}} <b>{{.B.AutoOff}}</b> pilot(s) have training automation
      switched off and are never in the batch.{{end}}
    </p>
  </div>

  <div class="card">
    <h3 style="margin-top:0">The dials, as the roster carries them</h3>
    <p class="muted" style="margin:0 0 8px">
      Two of the planner's numbers are personality rather than policy, so the roster holds a
      spread of them rather than one value. <b>Satisfied level</b> is where a wanted skill
      stops being urgent &mdash; past it, further levels are worth only the affinity they add.
      Patience sets it. <b>Group cap</b> is how many steps of one skill group a single plan may
      contain: the diversification brake, without which a pilot with one good gunboat spends
      every plan inside Gunnery. Wanderlust sets it, so a settled pilot builds a specialism and
      a nomad ends up a generalist. Shipped defaults, for a pilot with neither trait set:
      satisfied at <b>{{.Satisfied}}</b>, <b>{{.GroupCap}}</b> steps per group.
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
      The rest are the same for everyone: no single step longer than <b>{{.StepCap}}</b> (so one
      rank-6 level V cannot swallow a plan full of cheap unlocks), at most <b>{{.MaxSteps}}</b>
      steps in a plan, at most <b>{{.EvalBudget}}</b> fits re-judged against sheets the pilot
      does not have yet, and a standing <b>{{.WalletFloor}}</b> the wallet is never taken below
      to buy a book &mdash; only what it holds above that floor is book money, so no plan can
      leave a pilot unable to buy the hull the skills were for.
    </p>
  </div>
</div>

{{if .B.Prefs}}
<div class="card">
  <h3 style="margin-top:0">Operator overrides</h3>
  <p class="muted" style="margin:0 0 8px">
    Pins and bans somebody has set on individual pilots. A pin trains a skill to V ahead of
    everything else and bypasses the arithmetic entirely; a ban removes it, and everything
    behind it, from that pilot's reach. Deliberately sparse &mdash; every row here is an
    argument with the planner that somebody has to remember making.
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
