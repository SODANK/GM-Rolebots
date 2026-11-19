package rolebots

// Templates for the Bot Population section.
//
// Both pages carry a run banner that meta-refreshes while a seed or wipe is in flight. Generation runs on its
// own goroutine and context so it outlives the request, so the page is the only way to tell a long run from a
// hung one. The refresh stops when the run does.
//
// ── Why the mixes are percentages ──────────────────────────────────────────
//
// The generator consumes relative weights, but every mix is shown as a percentage of 100 with the head count
// it works out to. Each mix is a set of linked sliders that always sum to exactly 100%: moving one gives to or
// takes from the others, and a locked row is left alone (the script below, rbSet). The named number box in each
// row is the submitted field; the slider and lock button carry no name. The server still scales a mix that does
// not sum to 100, so a client without script degrades to the old behaviour.

const tmplSeed = castTabs + `{{define "content"}}` + castTabsStyle + `
<h2>RoleBot Generation</h2>

{{if .Msg}}<div class="alert alert-success">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

{{template "rb_casttabs" .}}

<div class="card" id="rb-migrate" style="border-left:3px solid #6d28d9">
  <h3>Hand a Capsuleer to a player account</h3>
  <p style="font-size:12px;color:#9ca3af;margin-top:-4px">
    Moves one generated Capsuleer onto a player account with a free slot. It stops being a rolebot and
    becomes an ordinary character, docked at its medical clone station. A replacement is rolled into the vacated slot.
  </p>
  <form method="POST" action="/rolebots/seed"
        onsubmit="return confirm('Hand this Capsuleer over? It stops being a rolebot, and a replacement is rolled onto its old account.');">
    <input type="hidden" name="action" value="migrate">
    <label style="font-size:12px;color:#9ca3af">Capsuleer
      <input type="text" name="subject" value="{{.MigrateSubject}}" autocomplete="off"
             placeholder="name or character ID" style="width:260px">
    </label>
    <label style="font-size:12px;color:#9ca3af;margin-left:12px">to account
      <select name="accountID" style="min-width:220px">
        <option value="">pick an account&hellip;</option>
        {{range .Targets}}
        <option value="{{.AccountID}}"{{if not .Free}} disabled{{end}}>{{.AccountName}} — {{if .Free}}{{.Free}} free slot(s){{else}}no free slot{{end}}</option>
        {{end}}
      </select>
    </label>
    <button type="submit" style="margin-left:8px">Hand over</button>
  </form>
  {{if not .Targets}}
  <p style="margin-top:8px;font-size:12px;color:#eab308">
    No player accounts exist yet — only {{.Prefix}} accounts, which cannot be handed a Capsuleer.
  </p>
  {{end}}
  <p style="margin-top:8px;font-size:12px;color:#6b7280">
    The Capsuleer must be deactivated first, and no run may be in flight. The replacement is drawn from
    <strong>{{.MixSource}}</strong>. Generated Characters has a Migrate link per row that fills this box in.
  </p>
</div>

{{if .Run.Active}}<meta http-equiv="refresh" content="3">{{end}}
{{if .Run.Total}}
<div class="card" style="border-left:3px solid {{if .Run.Active}}#eab308{{else}}#22c55e{{end}}">
  <h3>{{if .Run.Active}}Running{{else}}Last run{{end}} — {{.Run.Label}}</h3>
  <p>
    <strong>{{.Run.Done}}</strong> / {{.Run.Total}} processed
    &middot; <strong style="color:#22c55e">{{.Run.Created}}</strong> succeeded
    {{if .Run.Errs}}&middot; <strong style="color:#ef4444">{{len .Run.Errs}}</strong> problem(s){{end}}
    &middot; {{.Run.Elapsed}}
  </p>
  <div style="background:#1f1512;height:10px;border-radius:5px;overflow:hidden;max-width:520px">
    <div style="background:#22c55e;height:10px;width:{{.Run.Pct}}%"></div>
  </div>
  {{if .Run.Last}}<p style="font-size:12px;color:#9ca3af;margin-top:6px">latest: {{.Run.Last}}</p>{{end}}
  {{if .Run.Errs}}
  <details style="margin-top:8px">
    <summary style="cursor:pointer;color:#fca5a5">problems ({{len .Run.Errs}})</summary>
    <ul style="font-size:12px;color:#fca5a5">{{range .Run.Errs}}<li>{{.}}</li>{{end}}</ul>
  </details>
  {{end}}
</div>
{{end}}

<div class="card">
  <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:18px">

    <div>
      <h3 style="margin-top:0">Population</h3>
      <p style="margin:0">
        <strong style="font-size:20px">{{.Pop.Count}}</strong> Capsuleer(s)
        on <strong>{{.Pop.Accounts}}</strong> account(s){{if .Pop.Count}},
        <strong style="color:#22c55e">{{.Pop.Enabled}}</strong> active{{end}}.
      </p>
      <p style="margin:4px 0;font-size:12px;color:#9ca3af">
        {{if .Pop.Count}}Occupying {{.Prefix}}{{.Pop.MinSeq}}–{{.Prefix}}{{.Pop.MaxSeq}}. {{end}}
        Next account: <strong>{{.Prefix}}{{.NextSeq}}</strong> &middot; {{.PerAccount}} per account.
      </p>
      <p style="margin:6px 0 0"><a href="/rolebots/generated">Browse the population &rarr;</a></p>
      {{if ne .Pop.SeqAccounts .Pop.Accounts}}
      <div class="alert alert-error" style="margin-top:8px;font-size:12px">
        {{.Pop.SeqAccounts}} account number(s) are claimed by Capsuleers but {{.Pop.Accounts}} account
        row(s) exist — a run was interrupted, or an account was made by hand. A total purge clears it.
      </div>
      {{end}}
    </div>

    <div>
      <h3 style="margin-top:0">Online cap</h3>
      <p style="margin:0">
        {{if .Cap.Cap}}
          <strong style="font-size:20px">{{.Cap.Cap}}</strong> Capsuleer(s) on the server &middot;
          <strong>{{.Cap.Players}}</strong> player(s) &middot;
          <strong style="color:#22c55e">{{.Cap.BotsOnline}}</strong> bot(s) online
        {{else}}
          <strong style="font-size:20px">No cap</strong> — every active rolebot is online.
        {{end}}
      </p>
      <p style="margin:4px 0;font-size:12px;color:#9ca3af">
        {{if .Cap.Cap}}{{if .Cap.BotsBenched}}<strong style="color:#eab308">{{.Cap.BotsBenched}}</strong>{{else}}0{{end}}
        logged off by the cap &middot; {{.Cap.Seats}} seat(s) free &middot; {{end}}
        {{.Cap.Enabled}} active on the roster{{if not .Cap.Cap}}, {{.Cap.Players}} player(s) connected{{end}}.
      </p>
      <form method="POST" action="/rolebots/seed" style="margin-top:6px">
        <input type="hidden" name="action" value="popcap">
        <input type="number" name="maxOnline" value="{{.Cap.Cap}}" min="0" style="width:100px">
        <button type="submit">Set cap</button>
        <span style="margin-left:6px;font-size:12px;color:#6b7280">0 = unlimited</span>
      </form>
    </div>

    <div>
      <h3 style="margin-top:0">Projection cap</h3>
      <p style="margin:0">
        {{if .Proj.Cap}}
          <strong style="font-size:20px">{{.Proj.Cap}}</strong> ball(s) in space &middot;
          <strong style="color:#22c55e">{{.Proj.Live}}</strong> live
        {{else}}
          <strong style="font-size:20px">No cap</strong> &mdash; every watched grid fills as far as it wants.
        {{end}}
      </p>
      <p style="margin:4px 0;font-size:12px;color:#9ca3af">
        {{if .Proj.Cap}}{{if .Proj.Shed}}<strong style="color:#eab308">{{.Proj.Shed}}</strong>{{else}}0{{end}}
        withdrawn by the cap since boot &middot; {{end}}
        a grid a player is standing on is never shed.
      </p>
      <form method="POST" action="/rolebots/seed" style="margin-top:6px">
        <input type="hidden" name="action" value="projcap">
        <input type="number" name="maxProjections" value="{{.Proj.Cap}}" min="0" style="width:100px">
        <button type="submit">Set cap</button>
        <span style="margin-left:6px;font-size:12px;color:#6b7280">0 = unlimited</span>
      </form>
    </div>

    <div>
      <h3 style="margin-top:0">Ring preload</h3>
      <p style="margin:0">
        {{if not .Preload.Available}}
          <strong style="font-size:20px;color:#eab308">Unavailable</strong> &mdash; this server has no system-loading seam.
        {{else if .Preload.On}}
          <strong style="font-size:20px;color:#22c55e">On</strong> &middot;
          <strong>{{.Preload.Pinned}}</strong> system(s) held open
        {{else}}
          <strong style="font-size:20px">Off</strong> &mdash; bots are only projected where a player is standing.
        {{end}}
      </p>
      <p style="margin:4px 0;font-size:12px;color:#9ca3af">
        Loads systems one gate jump from a player so grids are already alive on arrival.
        {{if .Preload.Available}}{{if or .Preload.Loads .Preload.Unloads}}
        <br>{{.Preload.Loads}} loaded, {{.Preload.Unloads}} released since boot.{{end}}{{end}}
      </p>
      {{if .Preload.Available}}
      <form method="POST" action="/rolebots/seed" style="margin-top:6px">
        <input type="hidden" name="action" value="preloadring">
        <input type="hidden" name="preloadRing" value="{{if .Preload.On}}0{{else}}1{{end}}">
        <button type="submit">{{if .Preload.On}}Switch off{{else}}Switch on{{end}}</button>
        <span style="margin-left:6px;font-size:12px;color:#6b7280">costs are held down by the projection cap</span>
      </form>
      {{end}}
    </div>

    <div>
      <h3 style="margin-top:0">Docked pin</h3>
      <p style="margin:0">
        {{if not .Preload.Available}}
          <strong style="font-size:20px;color:#eab308">Unavailable</strong> &mdash; this server has no system-loading seam.
        {{else if .Preload.DockedOn}}
          <strong style="font-size:20px;color:#22c55e">On</strong>
        {{else}}
          <strong style="font-size:20px">Off</strong> &mdash; a system is torn down when its last pilot docks.
        {{end}}
      </p>
      <p style="margin:4px 0;font-size:12px;color:#9ca3af">
        Keeps a player's docked system loaded so undocking shows real traffic.
      </p>
      {{if .Preload.Available}}
      <form method="POST" action="/rolebots/seed" style="margin-top:6px">
        <input type="hidden" name="action" value="preloaddocked">
        <input type="hidden" name="preloadDocked" value="{{if .Preload.DockedOn}}0{{else}}1{{end}}">
        <button type="submit">{{if .Preload.DockedOn}}Switch off{{else}}Switch on{{end}}</button>
      </form>
      {{end}}
    </div>

    <div>
      <h3 style="margin-top:0">Run limit</h3>
      <p style="margin:0">
        <strong style="font-size:20px">{{.MaxBatch}}</strong> Capsuleers per run.
      </p>
      <p style="margin:4px 0;font-size:12px;color:#9ca3af">
        Guards against a mistyped number. Ceiling {{.MaxLimit}}.
      </p>
      <form method="POST" action="/rolebots/seed" style="margin-top:6px">
        <input type="hidden" name="action" value="runlimit">
        <input type="number" name="maxBatch" value="{{.MaxBatch}}" min="1" max="{{.MaxLimit}}" style="width:100px">
        <button type="submit">Set</button>
      </form>
    </div>

  </div>

  {{if .Pop.Count}}
  <div style="margin-top:14px;padding-top:12px;border-top:1px solid #2a1a16">
    <form method="POST" action="/rolebots/seed" style="display:inline">
      <input type="hidden" name="action" value="enableall">
      <button type="submit">Activate all</button>
    </form>
    <form method="POST" action="/rolebots/seed" style="display:inline">
      <input type="hidden" name="action" value="disableall">
      <button type="submit">Deactivate all</button>
    </form>
    <form method="POST" action="/rolebots/seed" style="display:inline">
      <input type="hidden" name="action" value="fixportraits">
      <button type="submit">Repair portraits</button>
    </form>
    {{if .Pop.Enabled}}
    <span style="margin-left:14px;color:#eab308">
      Purge is locked — <strong>{{.Pop.Enabled}}</strong> Capsuleer(s) still active. Deactivate all first.
    </span>
    {{else}}
    <form method="POST" action="/rolebots/seed" style="display:inline;margin-left:14px"
          onsubmit="return confirm('Purge ALL {{.Pop.Count}} generated Capsuleers on {{.Pop.Accounts}} accounts? This cannot be undone. Your Main Cast is not touched.');">
      <input type="hidden" name="action" value="wipeall">
      <input type="text" name="confirm" placeholder="{{.WipeConfirm}}" autocomplete="off"
             style="width:190px;border-color:#7f1d1d">
      <button type="submit" style="background:#7f1d1d;border-color:#991b1b">Purge everything</button>
    </form>
    {{end}}
    <div style="margin-top:8px;font-size:12px;color:#9ca3af">
      Purge reaches <strong>{{.Prefix}}&lt;number&gt; accounts only</strong>. Your
      <a href="/rolebots">Main Cast</a> is never deleted from here.
    </div>
  </div>
  <div style="margin-top:8px;font-size:12px;color:#6b7280">
    <details>
      <summary style="cursor:pointer">What each of these does</summary>
      <p style="margin:6px 0">
        <strong>Repair portraits</strong> — re-rolls appearance slots that ship broken in the client, on Capsuleers
        generated before the fix. Drop the stored render on the character page afterwards.
      </p>
      <p style="margin:6px 0">
        <strong>Purge everything</strong> — deletes every {{.Prefix}} account and everything on it. Irreversible.
        Needs <code>{{.WipeConfirm}}</code> typed first, and the whole population deactivated.
      </p>
      <p style="margin:6px 0">
        <strong>The online cap</strong> counts every Capsuleer, players included. A player logging in
        logs a bot off. A capped bot is logged off, not disabled, and resumes when a seat frees. A Capsuleer
        someone can currently see is never taken.
      </p>
    </details>
  </div>
  {{end}}
</div>

<form method="POST" action="/rolebots/seed" onsubmit="return confirm('Create ' + document.getElementById('gen-count').value + ' Capsuleers? Accounts and characters are created immediately.');">
<input type="hidden" name="action" value="generate">

<div class="card">
  <h3>1. How many Capsuleers</h3>
  <p>
    <input type="number" id="gen-count" name="count" value="{{.Count}}" min="1" max="{{.MaxBatch}}" style="width:120px">
    Capsuleers
    <span id="gen-acct-note" style="margin-left:10px;color:#9ca3af"></span>
  </p>
  <p style="font-size:12px;color:#6b7280">
    {{.Combos}} distinct names and {{.PoseCount}} portrait poses are available. Every mix below is a
    <strong>percentage</strong>, with the head count it works out to beside each row.
  </p>
  <p style="font-size:12px;color:#6b7280">
    Each mix adds up to 100%. Moving a slider adjusts the others. <strong>Lock</strong> a row to pin it. You can also type an exact figure.
  </p>
</div>

<style>
.rb-mix { margin-top:6px; }
.rb-row { display:grid; grid-template-columns:minmax(150px,1.3fr) minmax(170px,2fr) 78px 62px 70px; gap:6px 14px;
  align-items:center; padding:9px 4px; border-top:1px solid #2a1a1a; }
.rb-row .rb-who strong { display:block; }
.rb-row .rb-blurb { display:block; font-size:12px; color:#9ca3af; margin-top:2px; }
.rb-row .rb-est { font-size:12px; color:#9ca3af; text-align:right; white-space:nowrap; }
.rb-val { display:inline-flex; align-items:center; gap:3px; white-space:nowrap; color:#9ca3af; }
.rb-val input[type=number] { width:56px; text-align:right; -moz-appearance:textfield; appearance:textfield; font-variant-numeric:tabular-nums; }
.rb-val input[type=number]::-webkit-inner-spin-button,
.rb-val input[type=number]::-webkit-outer-spin-button { -webkit-appearance:none; margin:0; }
.rb-slide { width:100%; margin:0; height:8px; border-radius:4px; cursor:pointer; -webkit-appearance:none; appearance:none;
  --rb-fill:#b91c1c;
  background:linear-gradient(to right, var(--rb-fill) var(--p,0%), #2a1a1a var(--p,0%)); }
.rb-slide::-webkit-slider-runnable-track { height:8px; border-radius:4px; background:transparent; }
.rb-slide::-moz-range-track { height:8px; border-radius:4px; background:transparent; }
.rb-slide::-webkit-slider-thumb { -webkit-appearance:none; width:16px; height:16px; margin-top:-4px; border-radius:50%; background:#e5e7eb; border:2px solid #170a0a; }
.rb-slide::-moz-range-thumb { width:12px; height:12px; border-radius:50%; background:#e5e7eb; border:2px solid #170a0a; }
.rb-slide:disabled { cursor:not-allowed; --rb-fill:#ca8a04; opacity:.55; }
.rb-lock { font-size:11px; padding:2px 6px; cursor:pointer; min-width:56px; }
.rb-lock[aria-pressed=true] { border-color:#ca8a04; color:#fbbf24; background:#2a1d08; }
.rb-row.is-locked .rb-val input { color:#fbbf24; }
.rb-bl-row { display:block; padding:5px 0; border-top:0; }
.rb-bl-top { display:flex; align-items:baseline; gap:6px; }
.rb-bl-top .rb-est { margin-left:auto; }
.rb-bl-ctl { display:grid; grid-template-columns:minmax(80px,1fr) auto auto; gap:8px; align-items:center; margin-top:3px; }
.rb-tot .rb-lockn { color:#fbbf24; }
@media (max-width:760px) {
  .rb-row { grid-template-columns:1fr auto auto; }
  .rb-row .rb-who { grid-column:1 / -1; }
  .rb-row .rb-slide { grid-column:1; }
  .rb-row .rb-est { grid-column:1 / -1; text-align:left; }
}
</style>

<div class="card">
  <h3>2. What they do <span class="rb-tot" id="tot-arch"></span>
    <button type="button" class="rb-even" data-g="arch" style="font-size:11px;padding:1px 6px">Even split</button>
  </h3>
  <p style="font-size:12px;color:#6b7280">
    Sets the starting career and the personality traits. Set a share to 0 to exclude it.
  </p>
  <div class="rb-mix">
    {{range .Archetypes}}
    <div class="rb-row">
      <div class="rb-who"><strong>{{.Label}}</strong><span class="rb-blurb">{{.Blurb}}</span></div>
      <input type="range" class="rb-slide" min="0" max="100" step="0.1" value="{{.FormPct}}" aria-label="{{.Label}} share">
      <span class="rb-val"><input type="number" class="rb-pct" data-g="arch" data-est="est-arch-{{.Key}}"
                 name="arch_{{.Key}}" value="{{.FormPct}}" min="0" max="100" step="0.1">%</span>
      <button type="button" class="rb-lock" aria-pressed="false" title="Lock this share so other sliders leave it alone">Lock</button>
      <span class="rb-est" id="est-arch-{{.Key}}"></span>
    </div>
    {{end}}
  </div>
</div>

<div class="card">
  <h3>3. Inside the law, or outside it <span class="rb-tot" id="tot-law"></span></h3>
  <p style="font-size:12px;color:#6b7280">
    Criminality applies on top of the archetype above. Keep it mostly lawful.
  </p>
  <div class="rb-mix">
    {{range .Dispositions}}
    <div class="rb-row">
      <div class="rb-who"><strong>{{.Label}}</strong><span class="rb-blurb">{{.Blurb}}</span></div>
      <input type="range" class="rb-slide" min="0" max="100" step="0.1" value="{{if eq .Key "criminal"}}10{{else}}90{{end}}" aria-label="{{.Label}} share">
      <span class="rb-val"><input type="number" class="rb-pct" data-g="law" data-est="est-law-{{.Key}}"
                 name="law_{{.Key}}" value="{{if eq .Key "criminal"}}10{{else}}90{{end}}" min="0" max="100" step="0.1">%</span>
      <button type="button" class="rb-lock" aria-pressed="false" title="Lock this share so other sliders leave it alone">Lock</button>
      <span class="rb-est" id="est-law-{{.Key}}"></span>
    </div>
    {{end}}
  </div>
  <p style="font-size:12px;color:#6b7280">
    Where a Capsuleer flies is not set here. It follows disposition, archetype and employer.
  </p>
</div>

<div class="card">
  <h3>4. Race and bloodline <span class="rb-tot" id="tot-bl"></span>
    <button type="button" class="rb-even" data-g="bl" style="font-size:11px;padding:1px 6px">Even split</button>
  </h3>
  <p style="font-size:12px;color:#6b7280">
    Sets the name pool, appearance, starting school and standings. Hover a bloodline for its name count.
    Jove Modifier is off by default and excluded from the even split. It is men only, has a single name, and
    two of its schools sit in gateless Jove space.
  </p>
  <div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(270px,1fr));gap:14px">
    {{range $race := .RaceOrder}}
    <div>
      <h4 style="margin:0 0 6px">{{$race}}
        <button type="button" class="rb-race" data-race="{{$race}}" data-v="{{$.EvenBloodline}}" style="font-size:11px;padding:1px 6px">all</button>
        <button type="button" class="rb-race" data-race="{{$race}}" data-v="0" style="font-size:11px;padding:1px 6px">none</button>
      </h4>
      {{range index $.ByRace $race}}
      <div class="rb-row rb-bl-row" title="{{if .Mononym}}{{.Male}} given names, no surname = {{.Combos}} possible names{{else}}{{.Male}} male + {{.Female}} female given names &times; {{.Last}} surnames = {{.Combos}} possible names{{end}}{{if .Borrowed}} (borrowed from every other bloodline){{end}}{{if .MaleOnly}} — men only: the client has no female mesh for this bloodline{{end}}">
        <div class="rb-bl-top">
          <strong>{{.BloodlineName}}</strong>
          {{if .MaleOnly}}<span style="font-size:11px;color:#6b7280">men only{{if .Mononym}}, one name{{end}}</span>{{end}}
          <span class="rb-est" id="est-bl-{{.BloodlineID}}"></span>
        </div>
        <div class="rb-bl-ctl">
          <input type="range" class="rb-slide" min="0" max="100" step="0.1" value="{{if .OffByDefault}}0{{else}}{{$.EvenBloodline}}{{end}}" aria-label="{{.BloodlineName}} share">
          <span class="rb-val"><input type="number" class="rb-pct rb-bl" data-g="bl" data-race="{{$race}}"
                 data-est="est-bl-{{.BloodlineID}}" name="bl_{{.BloodlineID}}"
                 {{if .OffByDefault}}data-off="1" {{end}}value="{{if .OffByDefault}}0{{else}}{{$.EvenBloodline}}{{end}}"
                 min="0" max="100" step="0.1">%</span>
          <button type="button" class="rb-lock" aria-pressed="false" title="Lock this share so other sliders leave it alone">Lock</button>
        </div>
      </div>
      {{end}}
    </div>
    {{end}}
  </div>
  <p style="margin-top:10px">
    Female share:
    <input type="number" name="femalePct" value="50" min="0" max="100" style="width:70px">%
  </p>
</div>

<div class="card">
  <h3>5. Starting skills and ISK <span class="rb-tot" id="tot-mat"></span></h3>
  <p style="font-size:12px;color:#6b7280">
    Each tier is a skill sheet per archetype and race. Keep it mostly green.
  </p>
  <div class="rb-mix">
    {{range .Maturities}}
    <div class="rb-row">
      <div class="rb-who"><strong>{{.Label}}</strong><span class="rb-blurb">{{.Blurb}}</span></div>
      <input type="range" class="rb-slide" min="0" max="100" step="0.1" value="{{if eq .Key "green"}}70{{else if eq .Key "seasoned"}}25{{else}}5{{end}}" aria-label="{{.Label}} share">
      <span class="rb-val"><input type="number" class="rb-pct" data-g="mat" data-est="est-mat-{{.Key}}"
                 name="mat_{{.Key}}" value="{{if eq .Key "green"}}70{{else if eq .Key "seasoned"}}25{{else}}5{{end}}" min="0" max="100" step="0.1">%</span>
      <button type="button" class="rb-lock" aria-pressed="false" title="Lock this share so other sliders leave it alone">Lock</button>
      <span class="rb-est" id="est-mat-{{.Key}}"></span>
    </div>
    {{end}}
  </div>
  <p style="font-size:12px;color:#6b7280">
    Seasoned and veteran Capsuleers start with a <strong>past</strong>: an employment history, docked at their employer's headquarters.
  </p>
  <p style="margin-top:10px">
    Starting wallet, ISK:
    <input type="number" name="walletMin" value="1000000" min="0" step="100000" style="width:150px"> to
    <input type="number" name="walletMax" value="30000000" min="0" step="100000" style="width:150px">
  </p>
  <p style="font-size:12px;color:#6b7280">
    Each draw leans by tier: green near the floor, veterans near the ceiling.
  </p>
</div>

<div class="card">
  <h3>6. When they play <span class="rb-tot" id="tot-tz"></span>
    <button type="button" class="rb-even" data-g="tz" style="font-size:11px;padding:1px 6px">Even split</button>
  </h3>
  <p style="font-size:12px;color:#6b7280">
    Times are the <strong>server's clock</strong> (now <strong>{{.TZClock}}</strong>). Each Capsuleer plays 3&ndash;6 hours inside its window.
    Login drifts up to {{.TZ.BleedMin}} minutes daily and about {{.TZ.SkipDayPct}}% of days are skipped.
    Hours and the online cap decide who is in space.
  </p>
  <div class="rb-mix">
    {{range .TZWindows}}
    <div class="rb-row">
      <div class="rb-who"><strong>{{.Label}}</strong><span class="rb-blurb">{{.Name}}</span></div>
      <input type="range" class="rb-slide" min="0" max="100" step="0.1" value="{{.Default}}" aria-label="{{.Label}} share">
      <span class="rb-val"><input type="number" class="rb-pct" data-g="tz" data-est="est-tz-{{.Key}}"
                 name="tz_{{.Key}}" value="{{.Default}}" min="0" max="100" step="0.1">%</span>
      <button type="button" class="rb-lock" aria-pressed="false" title="Lock this share so other sliders leave it alone">Lock</button>
      <span class="rb-est" id="est-tz-{{.Key}}"></span>
    </div>
    {{end}}
  </div>
  <h4 style="margin:14px 0 4px">This batch online, by server hour <span id="tz-peak" style="font-size:12px;font-weight:normal;color:#9ca3af"></span></h4>
  <div id="tz-chart" style="display:flex;align-items:flex-end;gap:2px;height:110px;border-bottom:1px solid #2a1a1a"></div>
  <div style="display:flex;gap:2px;font-size:10px;color:#6b7280">
    {{range .TZHourLabels}}<div style="flex:1;text-align:center">{{.}}</div>{{end}}
  </div>
  <p style="font-size:12px;color:#6b7280;margin-top:6px">
    Expected Capsuleers of this batch online each hour. Hover a bar for the number. The Timezones page shows the whole roster.
  </p>
</div>
<script>
(function(){
  var cover = {{.TZCover}};
  var keys = [{{range $i, $w := .TZWindows}}{{if $i}},{{end}}{{$w.Key}}{{end}}];
  var chart = document.getElementById('tz-chart'), peak = document.getElementById('tz-peak');
  var cnt = document.getElementById('gen-count');
  var bars = [];
  for (var h = 0; h < 24; h++) {
    var b = document.createElement('div');
    b.style.cssText = 'flex:1;background:#7f1d1d;border-radius:2px 2px 0 0;min-height:1px';
    chart.appendChild(b); bars.push(b);
  }
  function draw(){
    var n = parseInt(cnt.value, 10); if (!(n > 0)) n = 0;
    var w = keys.map(function(k){
      var v = parseFloat((document.querySelector('[name="tz_' + k + '"]') || {}).value); return v > 0 ? v : 0;
    });
    var total = w.reduce(function(a, b){ return a + b; }, 0);
    if (!total) { w = w.map(function(){ return 1; }); total = w.length; }
    var on = [], top = 0, topH = 0;
    for (var h = 0; h < 24; h++) {
      var s = 0;
      for (var i = 0; i < w.length; i++) { s += w[i] / total * cover[i][h]; }
      on.push(s * n);
      if (on[h] > top) { top = on[h]; topH = h; }
    }
    on.forEach(function(v, h){
      bars[h].style.height = (top ? Math.max(1, Math.round(v / top * 100)) : 1) + '%';
      bars[h].title = (h < 10 ? '0' : '') + h + ':00 — about ' + Math.round(v) + ' online';
    });
    peak.textContent = n ? '— busiest around ' + (topH < 10 ? '0' : '') + topH + ':00 with about ' + Math.round(top) + ' online' : '';
  }
  // The linked sliders write the number boxes without firing input, so the mix script announces each change.
  document.addEventListener('rb-mix', draw);
  cnt.addEventListener('input', draw);
  draw();
})();
</script>

<div class="card">
  <button type="submit" style="font-size:15px;padding:8px 20px">Accept &amp; create</button>
</div>
</form>

<script>
(function(){
  var per = {{.PerAccount}}, next = {{.NextSeq}};
  var cnt = document.getElementById('gen-count');
  var note = document.getElementById('gen-acct-note');
  var pcts = Array.prototype.slice.call(document.querySelectorAll('.rb-pct'));

  function count(){ var n = parseInt(cnt.value, 10); return (n > 0) ? n : 0; }

  // Every mix is drawn by largest remainder on the server (allocate, in
  // genplan.go), so the numbers printed here are the numbers that will exist —
  // not an expected value a roll might miss by twenty.
  function estimate(group){
    var ins = pcts.filter(function(i){ return i.getAttribute('data-g') === group; });
    var total = 0;
    ins.forEach(function(i){ var v = parseFloat(i.value); if (v > 0) total += v; });
    // Largest remainder, the same way the server does it.
    var n = count(), floors = [], assigned = 0;
    ins.forEach(function(i){
      var w = parseFloat(i.value); if (!(w > 0)) w = 0;
      var exact = total ? n * w / total : 0;
      var f = Math.floor(exact);
      floors.push({ el: i, n: f, rem: exact - f, live: w > 0 });
      assigned += f;
    });
    floors.filter(function(f){ return f.live; })
          .sort(function(a, b){ return b.rem - a.rem; })
          .forEach(function(f){ if (assigned < n) { f.n++; assigned++; } });
    floors.forEach(function(f){
      var out = document.getElementById(f.el.getAttribute('data-est'));
      if (out) { out.textContent = (n && f.live) ? ('≈ ' + f.n) : ''; }
    });
  }

  function groups(){
    var seen = {};
    pcts.forEach(function(i){ seen[i.getAttribute('data-g')] = 1; });
    return Object.keys(seen);
  }
  function refresh(){ groups().forEach(estimate); }

  // rb:rebalance begin
  // Mix arithmetic on integer tenths of a percent: a whole mix is 1000, so the
  // shares sum to exactly 100.0 and no rounding is left over. Pure functions
  // (no DOM) so the rules can be driven from a script; snap is the row values
  // at the start of a gesture, locked and off are boolean arrays by row index.
  // off marks a row that stays out of an even split (the Jove Modifier).

  // Split total across weights w by largest remainder; all-zero w gives all zero.
  function rbLargest(w, total){
    var sum = 0, i, out = [], rem = [], used = 0;
    for (i = 0; i < w.length; i++) { sum += w[i]; }
    for (i = 0; i < w.length; i++) {
      var ex = sum ? w[i] * total / sum : 0, f = Math.floor(ex);
      out.push(f); used += f;
      rem.push({ i: i, r: ex - f, live: w[i] > 0 });
    }
    rem.filter(function(x){ return x.live; })
       .sort(function(a, b){ return (b.r - a.r) || (a.i - b.i); })
       .forEach(function(x){ if (used < total) { out[x.i]++; used++; } });
    return out;
  }

  // Share total out over rows idxs: in proportion to what they hold, so a row
  // at 0 stays at 0; evenly (off rows last) only when none of them holds anything.
  function rbSplit(snap, off, idxs, total){
    var w = idxs.map(function(j){ return snap[j]; });
    if (!w.some(function(x){ return x > 0; })) {
      var pool = idxs.filter(function(j){ return !off[j]; });
      if (!pool.length) { pool = idxs; }
      w = idxs.map(function(j){ return pool.indexOf(j) >= 0 ? 1 : 0; });
    }
    return rbLargest(w, total);
  }

  // Move rows idxs to targets. Locked rows are never touched; what the move
  // frees or needs comes from the unlocked rows outside idxs. With nothing left
  // to trade with, nothing moves.
  function rbSet(snap, locked, off, idxs, targets){
    var n = snap.length, j, lockedSum = 0, fixed = {}, fi = [], ft = [], rest = [];
    for (j = 0; j < n; j++) { if (locked[j]) { lockedSum += snap[j]; } }
    var pool = 1000 - lockedSum;
    idxs.forEach(function(row, k){
      if (!locked[row]) { fixed[row] = 1; fi.push(row); ft.push(Math.max(0, Math.round(targets[k]))); }
    });
    for (j = 0; j < n; j++) { if (!locked[j] && !fixed[j]) { rest.push(j); } }
    if (!fi.length || !rest.length) { return snap.slice(); }
    // An off row at 0 is not revived to soak up a move: if it is all that is
    // left to trade with, nothing moves until the operator raises it themselves.
    if (!rest.some(function(row){ return snap[row] > 0 || !off[row]; })) { return snap.slice(); }
    var sumT = ft.reduce(function(a, b){ return a + b; }, 0);
    if (sumT > pool) { ft = rbLargest(ft, pool); sumT = pool; }
    var out = snap.slice();
    fi.forEach(function(row, k){ out[row] = ft[k]; });
    var give = rbSplit(snap, off, rest, pool - sumT);
    rest.forEach(function(row, k){ out[row] = give[k]; });
    return out;
  }

  // Even split of whatever the locked rows leave. Off rows go to 0.
  function rbEven(snap, locked, off){
    var n = snap.length, j, lockedSum = 0, on = [], out = snap.slice();
    for (j = 0; j < n; j++) {
      if (locked[j]) { lockedSum += snap[j]; } else if (!off[j]) { on.push(j); }
    }
    if (!on.length) { return out; }
    var share = rbLargest(on.map(function(){ return 1; }), 1000 - lockedSum);
    for (j = 0; j < n; j++) { if (!locked[j] && off[j]) { out[j] = 0; } }
    on.forEach(function(row, k){ out[row] = share[k]; });
    return out;
  }

  // Bring a mix that loads off 100 (twelve bloodlines at 8.3 is 99.6) to exactly 1000.
  function rbNormalize(snap, off){
    var sum = snap.reduce(function(a, b){ return a + b; }, 0);
    if (sum === 1000) { return snap.slice(); }
    if (!sum) { return rbEven(snap, snap.map(function(){ return false; }), off); }
    return rbLargest(snap, 1000);
  }
  // rb:rebalance end

  // Rows by group. The named number box is the submitted field; the slider and
  // the lock button only drive it.
  var rows = {}, snap = {}, committed = {};
  pcts.forEach(function(num){
    var g = num.getAttribute('data-g'), row = num.closest('.rb-row');
    if (!row) { return; }
    (rows[g] = rows[g] || []).push({
      num: num, row: row, slide: row.querySelector('.rb-slide'), lock: row.querySelector('.rb-lock'),
      off: num.getAttribute('data-off') === '1'
    });
  });
  function tenths(el){ var v = parseFloat(el.value); return v > 0 ? Math.round(v * 10) : 0; }
  function read(g){ return rows[g].map(function(r){ return tenths(r.num); }); }
  function lockedOf(g){ return rows[g].map(function(r){ return r.lock.getAttribute('aria-pressed') === 'true'; }); }
  function offOf(g){ return rows[g].map(function(r){ return r.off; }); }

  // The header badge is always 100%; the lock count says why a slider may be stuck.
  function paintTot(g){
    var tot = document.getElementById('tot-' + g);
    if (!tot) { return; }
    var nl = lockedOf(g).filter(Boolean).length;
    tot.textContent = '100%';
    if (nl) {
      var s = document.createElement('span');
      s.className = 'rb-lockn';
      s.textContent = ' · ' + nl + ' locked';
      tot.appendChild(s);
    }
  }

  function write(g, vals){
    rows[g].forEach(function(r, k){
      var v = vals[k] / 10;
      r.num.value = v.toFixed(1);
      r.slide.value = v;
      r.slide.style.setProperty('--p', v + '%');
    });
    committed[g] = vals.slice();
    estimate(g);
    paintTot(g);
    // The boxes are written without an input event; the timezone chart listens for this.
    document.dispatchEvent(new CustomEvent('rb-mix', { detail: { g: g } }));
  }

  Object.keys(rows).forEach(function(g){
    rows[g].forEach(function(r, k){
      // A drag recomputes from the values at its first step, so dragging back
      // restores the rows it took from, and rounding never accumulates.
      r.slide.addEventListener('input', function(){
        if (!snap[g]) { snap[g] = read(g); }
        write(g, rbSet(snap[g], lockedOf(g), offOf(g), [k], [Math.round(parseFloat(r.slide.value) * 10)]));
      });
      r.slide.addEventListener('change', function(){ snap[g] = null; });

      // A typed figure commits on Tab or Enter (Enter would otherwise submit the form).
      r.num.addEventListener('change', function(){
        var v = parseFloat(r.num.value), base = committed[g].slice();
        if (!(v >= 0)) { write(g, base); return; }
        write(g, rbSet(base, lockedOf(g), offOf(g), [k], [Math.round(Math.min(v, 100) * 10)]));
      });
      r.num.addEventListener('keydown', function(e){
        if (e.key === 'Enter') { e.preventDefault(); r.num.dispatchEvent(new Event('change')); }
      });

      r.lock.addEventListener('click', function(){
        var on = r.lock.getAttribute('aria-pressed') !== 'true';
        r.lock.setAttribute('aria-pressed', on ? 'true' : 'false');
        r.lock.textContent = on ? 'Locked' : 'Lock';
        r.slide.disabled = on;
        r.num.readOnly = on;
        r.row.classList.toggle('is-locked', on);
        snap[g] = null;
        paintTot(g);
      });
    });
  });

  // Per-race buttons: "Caldari only" should not mean clearing nine boxes by
  // hand. all sets the race's rows to an even share, none to 0; the rest of
  // the mix, minus anything locked, takes up the difference.
  Array.prototype.forEach.call(document.querySelectorAll('.rb-race'), function(btn){
    btn.addEventListener('click', function(){
      if (!rows.bl) { return; }
      var race = btn.getAttribute('data-race'), v = Math.round(parseFloat(btn.getAttribute('data-v')) * 10) || 0;
      var idxs = [], targets = [];
      rows.bl.forEach(function(r, k){
        if (r.num.getAttribute('data-race') === race) { idxs.push(k); targets.push(v); }
      });
      write('bl', rbSet(committed.bl.slice(), lockedOf('bl'), offOf('bl'), idxs, targets));
    });
  });

  // Even split: what the locked rows leave, shared evenly. An off-by-default row
  // (the Jove) is zeroed rather than given a share: "even" means even across the
  // population an unattended run produces.
  Array.prototype.forEach.call(document.querySelectorAll('.rb-even'), function(btn){
    btn.addEventListener('click', function(){
      var g = btn.getAttribute('data-g');
      if (!rows[g]) { return; }
      write(g, rbEven(committed[g].slice(), lockedOf(g), offOf(g)));
    });
  });

  Object.keys(rows).forEach(function(g){ write(g, rbNormalize(read(g), offOf(g))); });

  // Capsuleers in, accounts out. The operator asked for Capsuleers; the account
  // count is the part they cannot work out at a glance, so it is spelled out
  // beside the box as they type instead of left as arithmetic.
  function acct(){
    var n = count();
    if (!n) { note.textContent = ''; refresh(); return; }
    var a = Math.ceil(n / per), last = next + a - 1;
    var range = (a === 1) ? '{{.Prefix}}' + next : '{{.Prefix}}' + next + '-{{.Prefix}}' + last;
    var spare = a * per - n;
    note.textContent = '= ' + a + ' account' + (a === 1 ? '' : 's') + ' (' + range + ')'
      + (spare ? ', ' + spare + ' unused slot' + (spare === 1 ? '' : 's') : '');
    refresh();
  }
  cnt.addEventListener('input', acct);
  acct();
})();
</script>
{{end}}`

const tmplGenerated = castTabs + `{{define "content"}}` + castTabsStyle + `
<h2>Generated Characters</h2>

{{if .Msg}}<div class="alert alert-success">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

{{template "rb_casttabs" .}}
{{if .Run.Active}}<meta http-equiv="refresh" content="5">
<div class="alert alert-success">{{.Run.Label}} — {{.Run.Done}}/{{.Run.Total}} done.</div>
{{end}}

<div class="card">
  <p>
    <strong>{{.Pop.Count}}</strong> generated Capsuleer(s) on {{.Pop.Accounts}} account(s), {{.Pop.Enabled}} active.
    <a href="/rolebots/seed">Create more, or purge them all &rarr;</a>
  </p>
  <form method="GET" action="/rolebots/generated" style="display:inline">
    <input type="text" name="q" value="{{.Q}}" placeholder="name, account, or sequence number" style="width:280px">
    <select name="arch">
      <option value="">every archetype</option>
      {{range .Archetypes}}<option value="{{.Key}}" {{if eq $.Arch .Key}}selected{{end}}>{{.Label}}</option>{{end}}
    </select>
    <button type="submit">Search</button>
    {{if or .Q .Arch}}<a href="/rolebots/generated" style="margin-left:8px">clear</a>{{end}}
  </form>
  <input type="text" id="gen-filter" placeholder="Filter the rows below..." style="width:100%;max-width:420px;margin-top:8px" autocomplete="off">
</div>

<div class="card">
  {{if .Rows}}
  <table id="gen-rows">
    <tr>
      <th></th><th>Account</th><th>Character</th><th>Archetype</th><th>Created</th>
      <th>Skills</th><th>Standing</th><th>Wallet</th><th>State</th><th>Location</th><th></th>
    </tr>
    {{range .Rows}}
    <tr data-search="{{.Search}}">
      <td style="width:40px"><img src="/portrait/{{.CharacterID}}?size=64" width="36" height="36" style="vertical-align:middle;border-radius:4px;background:#170a0a" onerror="this.style.display='none'" loading="lazy"></td>
      <td style="font-size:11px;color:#6b7280">{{.AccountName}}</td>
      <td>
        <a href="/rolebot/{{.CharacterID}}">{{.Name}}</a>
        <div style="font-size:11px;color:#6b7280">{{.Bloodline}} {{.Gender}}{{if .PoseSource}} &middot; {{.PoseSource}}{{end}}</div>
      </td>
      <td>{{.Archetype}}</td>
      <td style="font-size:12px;color:#9ca3af">{{.Created}}</td>
      <td>{{.Maturity}}</td>
      <td>{{.Standing}}</td>
      <td style="text-align:right">{{printf "%.0f" .Balance}}</td>
      <td>
        {{if .Halted}}<strong style="color:#ef4444">HALTED</strong>
        {{else if not .Enabled}}<span style="color:#6b7280">off</span>
        {{else if .Offline}}<span style="color:#eab308" title="over the online cap — logged off, resumes when a seat frees">logged off</span>
        {{else if .Activity}}{{.Activity}} <span style="font-size:11px;color:#6b7280">{{.Phase}}</span>
        {{else}}<span style="color:#eab308">idle</span>{{end}}
      </td>
      <td style="font-size:12px;color:#9ca3af">{{.Where}}</td>
      <td style="white-space:nowrap">
        <form method="POST" action="/rolebots/generated" style="display:inline">
          <input type="hidden" name="action" value="{{if .Enabled}}disable{{else}}enable{{end}}">
          <input type="hidden" name="charID" value="{{.CharacterID}}">
          <input type="hidden" name="back" value="q={{$.Q}}&arch={{$.Arch}}">
          <button type="submit">{{if .Enabled}}Deactivate{{else}}Activate{{end}}</button>
        </form>
        <form method="POST" action="/rolebots/generated" style="display:inline" onsubmit="return confirm('Delete {{.Name}} and everything it owns? This cannot be undone. Other characters on {{.AccountName}} are not affected.');">
          <input type="hidden" name="action" value="purge">
          <input type="hidden" name="charID" value="{{.CharacterID}}">
          <input type="hidden" name="back" value="q={{$.Q}}&arch={{$.Arch}}">
          <button type="submit">Purge</button>
        </form>
        <a href="/rolebots/seed?migrate={{.CharacterID}}#rb-migrate" style="font-size:12px;margin-left:6px"
           alt="hand this Capsuleer to a player account">Migrate</a>
      </td>
    </tr>
    {{end}}
  </table>
  <script>
  (function(){
    var box = document.getElementById('gen-filter');
    var rows = Array.prototype.slice.call(document.querySelectorAll('#gen-rows tr[data-search]'));
    box.addEventListener('input', function(){
      var terms = box.value.trim().toLowerCase().split(/\s+/).filter(Boolean);
      rows.forEach(function(tr){
        var hay = tr.getAttribute('data-search');
        var ok = terms.every(function(t){ return hay.indexOf(t) !== -1; });
        tr.style.display = ok ? '' : 'none';
      });
    });
  })();
  </script>
  {{else}}
  <p>No generated characters{{if or .Q .Arch}} match that search{{end}}. <a href="/rolebots/seed">Create some &rarr;</a></p>
  {{end}}
</div>
{{end}}`
