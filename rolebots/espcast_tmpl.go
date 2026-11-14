package rolebots

// The Main Cast page, kept apart from its handler. The tab strip is concatenated in (castTabs +
// castTabsStyle, esptabs.go) because module pages parse independently; espcast_render_test.go guards a
// missing define.
//
// ── Why cards and not a table ──────────────────────────────────────────────
//
// A cast is a few pilots, asked what each is doing, where it lives and what it is like: long strings that
// would cost a column most rows leave blank. The every-character browser below stays a table, since there
// the question is find one by name.

// rbCardStyle is the card grid the Main Cast uses, shared with the CEOs tab and
// the corporation page so the three read as one family.
const rbCardStyle = `<style>
.rb-cast { display:grid; grid-template-columns:repeat(auto-fill,minmax(330px,1fr)); gap:12px; }
.rb-card { border:1px solid #2a1a1a; border-radius:7px; background:#140d0d; padding:12px 13px;
           display:flex; flex-direction:column; gap:8px; }
.rb-card.halt { border-color:#7f1d1d; }
.rb-card .hd { display:flex; gap:11px; align-items:flex-start; }
.rb-card .hd img { width:64px; height:64px; border-radius:6px; background:#170a0a; flex:0 0 auto;
                   object-fit:cover; box-shadow:0 0 0 1px #382020; }
.rb-card .nm { font-size:15px; color:#f0e6dc; font-weight:600; line-height:1.25; }
.rb-card .nm a { color:#f0e6dc; text-decoration:none; }
.rb-card .nm a:hover { text-decoration:underline; }
.rb-card .id { font-size:11px; color:#6b7280; font-weight:400; }
.rb-card .co { font-size:12px; color:#9ca3af; margin-top:2px; }
.rb-card dl { display:grid; grid-template-columns:auto 1fr; gap:2px 10px; margin:0; font-size:12px; }
.rb-card dt { color:#6b7280; white-space:nowrap; }
.rb-card dd { margin:0; color:#d8d2cc; overflow-wrap:anywhere; }
.rb-pill { display:inline-block; padding:1px 8px; border-radius:9px; font-size:11px;
           background:#382020; color:#9ca3af; }
.rb-pill.on { background:#0f2a1f; color:#6ee7b7; }
.rb-pill.warn { background:#2a2010; color:#eab308; }
.rb-pill.bad { background:#2a1010; color:#fca5a5; }
.rb-chip { display:inline-block; padding:1px 7px; border-radius:3px; font-size:11px;
           background:#1d1414; border:1px solid #382020; color:#c3b8ae; margin-right:4px; }
.rb-acts { margin-top:auto; padding-top:8px; border-top:1px solid #241313; white-space:nowrap; }
.rb-acts form { display:inline }
.rb-empty { color:#9ca3af; }
</style>`

const tmplCast = castTabs + `{{define "content"}}` + castTabsStyle + `
` + rbCardStyle + `

<h2>Main Cast</h2>

{{if .Msg}}<div class="alert alert-success">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

{{template "rb_casttabs" .}}

<div class="card">
  <h3>Master Switch</h3>
  <p>
    Status: {{if .MasterOn}}<strong style="color:#22c55e">ON</strong> — enabled rolebots are live (presence, sim, market).{{else}}<strong style="color:#ef4444">OFF</strong> — no rolebot acts, regardless of per-character settings.{{end}}
  </p>
  <form method="POST" action="/rolebots" style="display:inline">
    <input type="hidden" name="action" value="master">
    <input type="hidden" name="state" value="{{if .MasterOn}}off{{else}}on{{end}}">
    <button type="submit">{{if .MasterOn}}Switch RoleBot OFF{{else}}Switch RoleBot ON{{end}}</button>
  </form>
  <p style="margin-top:8px;font-size:12px;color:#6b7280">
    The switch is the whole module, cast and generated population together — it is not scoped to
    this tab. Persisted in server_settings, so it survives restarts; config.json
    <code>modules.rolebots.enabled</code> is only the pre-first-toggle default.
  </p>
  <form method="POST" action="/rolebots" style="display:inline">
    <input type="hidden" name="action" value="rehome">
    <button type="submit">Reconsider every pilot's address</button>
  </form>
  <p style="margin-top:8px;font-size:12px;color:#6b7280">
    Not a setting — a refresh. Every rostered pilot looks again at where it lives and where it builds:
    its employer's stations first, then the nearest medical bay and manufacturing line to them. Use it
    after changing a corporation's stations. Nobody moves who is already living with their employer, and
    the looks land one per pilot per planning pass.
  </p>
</div>

<div class="card">
  <h3>The Cast <span style="font-size:12px;color:#6b7280">({{len .Bots}})</span></h3>
  <p style="font-size:12px;color:#6b7280;margin-top:-4px">
    Characters you made yourself and handed to the sim. Nothing the generator produced appears here or
    in the browser below — those live on
    <a href="/rolebots/seed">RoleBot Generation</a> and
    <a href="/rolebots/generated">Generated Characters</a>, and there is no setting that mixes them in.
  </p>
  {{if .Bots}}
  <div class="rb-cast">
    {{range .Bots}}
    <div class="rb-card{{if .Halted}} halt{{end}}">
      <div class="hd">
        <img src="/portrait/{{.CharacterID}}?size=128" width="64" height="64"
             onerror="this.style.visibility='hidden'" loading="lazy" alt="">
        <div>
          <div class="nm"><a href="/rolebot/{{.CharacterID}}">{{.CharacterName}}</a>
            <span class="id">{{.CharacterID}}</span></div>
          <div class="co">{{if .CorpID}}<a href="/rolebots/corp/{{.CorpID}}">{{.CorpName}}</a>{{else}}{{.CorpName}}{{end}}</div>
          <div style="margin-top:5px">
            {{if .Enabled}}<span class="rb-pill on">possessed</span>{{else}}<span class="rb-pill">idle</span>{{end}}
            {{if .Offline}}<span class="rb-pill warn" title="over the online cap — logged off, resumes when a seat frees">logged off</span>{{end}}
            {{if .Halted}}<span class="rb-pill bad">HALTED</span>{{end}}
          </div>
        </div>
      </div>

      {{if .Halted}}<div style="font-size:11px;color:#fca5a5">{{.LastError}}</div>{{end}}

      {{if .Traits}}
      <div>{{range .Traits}}<span class="rb-chip">{{.}}</span>{{end}}</div>
      {{end}}

      <dl>
        <dt>Doing</dt>
        <dd>{{if .SimActivity}}{{.SimActivity}} <span style="color:#6b7280">{{.SimPhase}}</span>{{else}}<span style="color:#6b7280">nothing yet</span>{{end}}</dd>
        <dt>Now at</dt>
        <dd>{{if .StationName}}Docked — {{.StationName}}{{else if .SystemName}}In space — {{.SystemName}}{{else}}<span style="color:#6b7280">unknown</span>{{end}}</dd>
        <dt>Lives at</dt>
        <dd>{{if .HomeName}}{{.HomeName}}{{else}}<span style="color:#6b7280">no clone set</span>{{end}}{{if .HomePinned}} <span class="rb-pill" title="pinned — nothing may move this pilot's home">pinned</span>{{end}}</dd>
        <dt>Builds at</dt>
        <dd>{{if .FactoryName}}{{.FactoryName}}{{else}}<span style="color:#6b7280">—</span>{{end}}</dd>
        <dt>Wallet</dt>
        <dd>{{.Balance}}</dd>
        <dt>Cast since</dt>
        <dd>{{.Added}}</dd>
      </dl>

      <div class="rb-acts">
        <a href="/rolebot/{{.CharacterID}}" style="font-size:12px;margin-right:8px">Profile</a>
        <form method="POST" action="/rolebots">
          <input type="hidden" name="action" value="reboot">
          <input type="hidden" name="charID" value="{{.CharacterID}}">
          <button type="submit">Reboot</button>
        </form>
        {{if .Enabled}}
        <form method="POST" action="/rolebots">
          <input type="hidden" name="action" value="disable">
          <input type="hidden" name="charID" value="{{.CharacterID}}">
          <button type="submit">Deactivate</button>
        </form>
        {{else}}
        <form method="POST" action="/rolebots">
          <input type="hidden" name="action" value="enable">
          <input type="hidden" name="charID" value="{{.CharacterID}}">
          <button type="submit">Activate</button>
        </form>
        {{end}}
        <form method="POST" action="/rolebots"
              onsubmit="return confirm('Take {{.CharacterName}} out of the Main Cast? The character itself is untouched.');">
          <input type="hidden" name="action" value="remove">
          <input type="hidden" name="charID" value="{{.CharacterID}}">
          <button type="submit">Remove</button>
        </form>
      </div>
    </div>
    {{end}}
  </div>
  {{else}}
  <p class="rb-empty">No cast yet — pick one of your own characters below and it becomes a rolebot.</p>
  {{end}}
</div>

<div class="card">
  <h3>Your Characters <span style="font-size:12px;color:#6b7280">({{len .AllChars}} — <span id="rb-shown">{{len .AllChars}}</span> shown)</span></h3>
  <p style="font-size:12px;color:#6b7280;margin-top:-4px">
    Every character on a player account. Casting one hands it to the sim; removing it later leaves the
    character exactly as it was.
  </p>
  <input type="text" id="rb-filter" placeholder="Filter by name, character ID, account, or corporation..." style="width:100%;max-width:420px;margin-bottom:8px" autocomplete="off">
  {{if .AllChars}}
  <table id="rb-allchars">
    <tr><th></th><th>Name</th><th>Account</th><th>Corporation</th><th>Cast</th><th></th></tr>
    {{range .AllChars}}
    <tr data-search="{{.Search}}">
      <td style="width:40px"><img src="/portrait/{{.CharacterID}}?size=64" width="36" height="36" style="vertical-align:middle;border-radius:4px;background:#170a0a" onerror="this.style.display='none'" loading="lazy"></td>
      <td>{{if .OnRoster}}<a href="/rolebot/{{.CharacterID}}">{{.CharacterName}}</a>{{else}}{{.CharacterName}}{{end}} <span style="font-size:11px;color:#6b7280">{{.CharacterID}}</span></td>
      <td>{{.AccountName}} <span style="font-size:11px;color:#6b7280">{{.AccountID}}</span></td>
      <td>{{.CorpName}}</td>
      <td>
        {{if not .OnRoster}}<span style="color:#6b7280">—</span>
        {{else if .Enabled}}<strong style="color:#22c55e">active</strong>
        {{else}}<span style="color:#eab308">idle</span>{{end}}
      </td>
      <td style="white-space:nowrap">
        {{if not .OnRoster}}
        <form method="POST" action="/rolebots" style="display:inline">
          <input type="hidden" name="action" value="add">
          <input type="hidden" name="charID" value="{{.CharacterID}}">
          <button type="submit">Add to cast</button>
        </form>
        {{else if .Enabled}}
        <form method="POST" action="/rolebots" style="display:inline">
          <input type="hidden" name="action" value="disable">
          <input type="hidden" name="charID" value="{{.CharacterID}}">
          <button type="submit">Deactivate</button>
        </form>
        {{else}}
        <form method="POST" action="/rolebots" style="display:inline">
          <input type="hidden" name="action" value="enable">
          <input type="hidden" name="charID" value="{{.CharacterID}}">
          <button type="submit">Activate</button>
        </form>
        {{end}}
      </td>
    </tr>
    {{end}}
  </table>
  <script>
  (function(){
    var box = document.getElementById('rb-filter');
    var rows = Array.prototype.slice.call(
      document.querySelectorAll('#rb-allchars tr[data-search]'));
    var shown = document.getElementById('rb-shown');
    box.addEventListener('input', function(){
      var q = box.value.trim().toLowerCase();
      var terms = q ? q.split(/\s+/) : [];
      var n = 0;
      rows.forEach(function(tr){
        var hay = tr.getAttribute('data-search');
        var hit = terms.every(function(t){ return hay.indexOf(t) !== -1; });
        tr.style.display = hit ? '' : 'none';
        if (hit) n++;
      });
      shown.textContent = n;
    });
  })();
  </script>
  {{else}}
  <p class="rb-empty">No player characters exist yet. Create them normally in game.</p>
  {{end}}
</div>
{{end}}`
