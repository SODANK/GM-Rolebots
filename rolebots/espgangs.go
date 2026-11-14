package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The Gangs tool: /rolebots/gangs and /rolebots/gang/<id>.
//
// This page is an observatory, not a console. Corporations form, crew, steer and wind down
// fleets (gangform.go, corprank.go, gangplan.go), and nothing here starts, stops, re-crews or
// reboots one: a per-fleet override has to be un-pressed later. The formation switch, fleet
// cap, clocks and PvP dials are not edited here. A fleet's size is its corporation's roster,
// not a cap. The bottom card is the player-fleet half (conscription).

// ── list page ───────────────────────────────────────────────────────────────

const tmplGangs = gangTabs + `{{define "content"}}` + castTabsStyle + `
<style>
.gf-bar { display:flex; flex-wrap:wrap; gap:8px; align-items:center; margin-bottom:8px; }
.gf-bar input[type=text] { width:260px; }
.gf-tbl th.sortable { cursor:pointer; user-select:none; }
.gf-tbl th.sortable:after { content:" \2195"; color:#6b7280; font-size:10px; }
.gf-tbl td { padding-top:3px; padding-bottom:3px; }
.gf-pill { display:inline-block;padding:1px 8px;border-radius:10px;font-size:11px;background:#382020;color:#9ca3af }
.gf-pill.warn { background:#2a0f0f;color:#fca5a5 }
.gf-note { font-size:12px;color:#6b7280 }
.gf-rules label.row { display:inline-block;margin:5px 14px 5px 0 }
</style>

<h2>Gangs</h2>
{{template "rb_gangtabs" .}}

{{if .Msg}}<div class="alert alert-success">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="card">
  <h3>Fleets <span class="gf-note">({{.GangCount}} flying)</span></h3>
  {{if .Gangs}}
  <div class="gf-bar">
    <input type="text" id="gfq" placeholder="Filter by name, leader, corp or where..." autocomplete="off" oninput="gfFilter()">
    <select id="gfact" onchange="gfFilter()">
      <option value="">Every activity</option>
      {{range .Activities}}<option value="{{.}}">{{.}}</option>{{end}}
    </select>
    <select id="gfphase" onchange="gfFilter()">
      <option value="">Every stage</option>
      {{range .Phases}}<option value="{{.}}">{{.}}</option>{{end}}
    </select>
    <span class="gf-note" id="gfcount"></span>
  </div>
  <table class="gf-tbl" id="gftbl">
    <thead>
      <tr>
        <th class="sortable" onclick="gfSort(0,0)">Fleet</th>
        <th class="sortable" onclick="gfSort(1,0)">Activity</th>
        <th class="sortable" onclick="gfSort(2,0)">Stage</th>
        <th class="sortable" onclick="gfSort(3,0)">Commander</th>
        <th class="sortable" onclick="gfSort(4,0)">Corporation</th>
        <th class="sortable" onclick="gfSort(5,1)">Crew</th>
        <th class="sortable" onclick="gfSort(6,0)">Where it is working</th>
        <th class="sortable" onclick="gfSort(7,1)">Winds down</th>
        <th class="sortable" onclick="gfSort(8,0)">Window</th>
      </tr>
    </thead>
    <tbody>
    {{range .Gangs}}
    <tr data-act="{{.Activity}}" data-phase="{{.Phase}}" data-q="{{.Search}}">
      <td><a href="/rolebots/gang/{{.GangID}}">{{.Name}}</a>{{if not .AutoFormed}} <span class="gf-pill">hand-built</span>{{end}}</td>
      <td>{{.Activity}}</td>
      <td>{{.Phase}}</td>
      <td>{{if .LeaderName}}{{.LeaderName}}{{else}}<span class="gf-note">&mdash;</span>{{end}}</td>
      <td>{{if .CorpName}}{{.CorpName}}{{else}}<span class="gf-note">&mdash;</span>{{end}}</td>
      <td data-v="{{.MemberCount}}">{{.MemberCount}}</td>
      <td style="font-size:12px">{{.OpNote}}</td>
      <td data-v="{{.ExpiresIn}}" style="font-size:12px">{{if .ExpiresNote}}{{.ExpiresNote}}{{else}}<span class="gf-note">&mdash;</span>{{end}}</td>
      <td style="font-size:12px">{{if .Window}}{{.Window}}{{else}}<span class="gf-note">&mdash;</span>{{end}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  {{else}}
  <p>No fleets are flying. {{if .GangFormOn}}Commanders seat themselves on the corporation promotion pass; the next formation sweep raises one.{{else}}Corporations are not fielding fleets.{{end}}</p>
  {{end}}
</div>

<div class="card gf-rules">
  <h3>Fleet Seats for Members</h3>
  <p class="gf-note">
    With this on, a corporation's fleets appear on its members' Corporate Portal
    (Galnet &rarr; Corporate Portal &rarr; Operations) with a <em>Request a seat</em> link.
    The commander opens a real fleet, invites the capsuleer, and from then on narrates
    the op in fleet chat: formed up, site picked, engaging, winding down. A CEO or
    Director aboard may type <em>mine here</em>, <em>mine &lt;ore&gt;</em>, <em>hunt here</em>
    or <em>stand down</em> and the whole op moves; anyone aboard may ask for <em>status</em>
    or a <em>roll call</em>. The Window column above says which fleets have one open.
  </p>
  <form method="POST" action="/rolebots/gangs">
    <input type="hidden" name="action" value="fleetmirror">
    <p><label><input type="checkbox" name="mirror" {{if .FleetMirrorOn}}checked{{end}}> Members may request a seat in a corporation fleet</label></p>
    <p><label><input type="checkbox" name="narration" {{if .FleetNarrationOn}}checked{{end}}> Commanders narrate the op in fleet chat</label>
      <span class="gf-note">&mdash; only while a capsuleer is aboard; shares the voice budget below.</span></p>
    <p><label><input type="checkbox" name="pointCalls" {{if .PointCallsOn}}checked{{end}}> Pilots call their points in fleet chat ("Point on X")</label>
      <span class="gf-note">&mdash; bot fleets and conscripts, only while a capsuleer is aboard.</span></p>
    <label class="row">Invitation lapses after (s) <input type="number" name="seatTTL" value="{{.SeatTTL}}" min="60" style="width:80px"></label>
    <br>
    <p><label><input type="checkbox" name="orders" {{if .CorpOrdersOn}}checked{{end}}> The chair gives orders on the portal</label>
      <span class="gf-note">&mdash; a human CEO or Director posts fleet orders, a directive and work on the Orders page (with a mailed portal key); the order desk carries them out and the Operations page reports.</span></p>
    <label class="row">Order lapses after (h) <input type="number" name="orderTTL" value="{{.OrderTTLHours}}" min="1" style="width:70px"></label>
    <label class="row">Tries before refusing <input type="number" name="orderRetries" value="{{.OrderRetries}}" min="1" style="width:70px"></label>
    <label class="row">Fleet orders in hand per corp <input type="number" name="fleetMaxOpen" value="{{.FleetMaxOpen}}" min="1" style="width:70px"></label>
    <label class="row">Directive held at most (days) <input type="number" name="holdMax" value="{{.HoldMax}}" min="1" style="width:70px"></label>
    <button type="submit">Save</button>
  </form>
</div>

<div class="card gf-rules">
  <h3>Player Fleets</h3>
  <p class="gf-note">
    With this on, a player can right-click a rolebot and invite it into their own fleet.
    It joins, comes to them, and takes plain-English orders typed in fleet chat —
    <em>mine</em>, <em>follow me</em>, <em>attack</em>, <em>stockpile here</em>, <em>sell</em>, <em>dismiss</em>.
    A name in front of the order addresses one pilot; no name addresses the crew.
    Only the fleet's LEADER is obeyed. A pilot refuses if it is mid-something that
    dropping would strand, or if it dislikes the caller — neutral standing is enough
    to be hired. Ore and loot go to the commander's hangar, and a sale's proceeds to
    their wallet.
  </p>
  <form method="POST" action="/rolebots/gangs">
    <input type="hidden" name="action" value="conscript">
    <p><label><input type="checkbox" name="enabled" {{if .ConscriptOn}}checked{{end}}> Players may conscript rolebots</label></p>
    <p>
      <label><input type="checkbox" name="defend" {{if .ConscriptDefend}}checked{{end}}> Defend the fleet unasked</label>
      <span class="gf-note">&mdash; a servant engages whatever opens up on its commander or its crewmates without being told. It still will not shoot a clean pilot in empire space.</span>
    </p>
    <p>
      <label><input type="checkbox" name="voice" {{if .ConscriptVoice}}checked{{end}}> Speak in fleet chat</label>
      <span class="gf-note">&mdash; acknowledgements, refusals and the occasional line about what it is doing. Budgeted below.</span>
    </p>
    <label class="row">Max bots per fleet <input type="number" name="maxFleet" value="{{.MaxFleet}}" min="1" style="width:70px"></label>
    <label class="row">Max serving server-wide <input type="number" name="maxTotal" value="{{.MaxTotal}}" min="1" style="width:70px"></label>
    <label class="row">Release after silence (s) <input type="number" name="idleTTL" value="{{.IdleTTL}}" min="60" style="width:80px"></label>
    <br>
    <label class="row">Quiet gap per pilot (s) <input type="number" name="voiceGap" value="{{.VoiceGap}}" min="0" style="width:70px"></label>
    <label class="row">Lines per minute per fleet <input type="number" name="voicePerMin" value="{{.VoicePerMin}}" min="1" style="width:70px"></label>
    <button type="submit">Save</button>
  </form>

  <h4 style="margin-top:14px">Serving now</h4>
  {{if .Conscripts}}
  <table>
    <tr>
      <th>Pilot</th><th>Commander</th><th>Fleet</th><th>Seat</th>
      <th>Order</th><th>Doing</th><th>Drop-off</th><th>Serving</th><th>Last order</th><th></th>
    </tr>
    {{range .Conscripts}}
    <tr>
      <td><a href="/rolebot/{{.CharID}}">{{if .Name}}{{.Name}}{{else}}{{.CharID}}{{end}}</a></td>
      <td>{{if .BossName}}{{.BossName}}{{else}}<span class="gf-note">{{.BossID}}</span>{{end}}</td>
      <td>{{.GangID}}{{if not .FleetLive}} <span class="gf-pill warn">nobody flying it</span>{{end}}</td>
      <td>{{if .Role}}{{.Role}}{{else}}<span class="gf-note">&mdash;</span>{{end}}</td>
      <td>{{if .Order}}{{.Order}}{{else}}<span class="gf-note">standing by</span>{{end}}</td>
      <td style="font-size:12px">{{.Doing}}</td>
      <td style="font-size:12px">{{if .Duty}}{{.Duty}}{{else}}<span class="gf-note">&mdash;</span>{{end}}</td>
      <td>{{.ServedFor}}</td>
      <td>{{.LastOrder}}</td>
      <td>
        <form method="POST" action="/rolebots/gangs" style="display:inline">
          <input type="hidden" name="action" value="conscriptRelease">
          <input type="hidden" name="charID" value="{{.CharID}}">
          <button type="submit">Release</button>
        </form>
      </td>
    </tr>
    {{end}}
  </table>
  <form method="POST" action="/rolebots/gangs" style="margin-top:8px"
        onsubmit="return confirm('Release every conscript? Each goes back to its own rotation; nothing else changes.');">
    <input type="hidden" name="action" value="conscriptReleaseAll">
    <button type="submit">Release all</button>
  </form>
  {{else}}
  <p>Nobody is serving in a player's fleet.</p>
  {{end}}
  <p class="gf-note">
    Rows clear themselves: a pilot is released when it is kicked, when the fleet
    disbands, when the last human in it logs off, and after the silence window
    above. Every row is dropped at boot as well — the core's fleet registry lives
    in memory and renumbers from 1, so a surviving row would name whichever fleet
    took that number next.
  </p>
</div>

<script>
function gfRows() { var t = document.getElementById('gftbl'); return t ? Array.prototype.slice.call(t.tBodies[0].rows) : []; }
function gfFilter() {
  var qEl = document.getElementById('gfq');
  if (!qEl) return;
  var q = (qEl.value || '').toLowerCase().trim();
  var act = document.getElementById('gfact').value;
  var ph = document.getElementById('gfphase').value;
  var shown = 0;
  gfRows().forEach(function (tr) {
    var ok = (!act || tr.getAttribute('data-act') === act) &&
             (!ph || tr.getAttribute('data-phase') === ph) &&
             (!q || (tr.getAttribute('data-q') || '').toLowerCase().indexOf(q) >= 0);
    tr.style.display = ok ? '' : 'none';
    if (ok) shown++;
  });
  document.getElementById('gfcount').textContent = shown + ' shown';
}
var gfDir = {};
function gfSort(col, numeric) {
  var tb = document.getElementById('gftbl').tBodies[0];
  var dir = gfDir[col] = -(gfDir[col] || 1);
  var rows = gfRows();
  rows.sort(function (a, b) {
    var x = a.cells[col], y = b.cells[col];
    if (numeric) {
      return (parseFloat(x.getAttribute('data-v') || '0') - parseFloat(y.getAttribute('data-v') || '0')) * dir;
    }
    return x.textContent.trim().localeCompare(y.textContent.trim()) * dir;
  });
  rows.forEach(function (r) { tb.appendChild(r); });
}
gfFilter();
</script>
{{end}}`

// ── detail page ─────────────────────────────────────────────────────────────

const tmplGangView = `{{define "content"}}
<style>
.gv-badge { display:inline-block; padding:1px 8px; border-radius:10px; font-size:11px; background:#382020; color:#9ca3af; }
.gv-badge.bot { background:#0f2a1f; color:#6ee7b7; }
.gv-badge.player { background:#1e2a4a; color:#93c5fd; }
.gv-note { font-size:12px;color:#6b7280 }
.gv-board { display:grid; grid-template-columns:repeat(auto-fit,minmax(230px,1fr)); gap:10px 18px; }
.gv-board .k { font-size:11px;color:#6b7280;text-transform:uppercase;letter-spacing:.04em }
.gv-board .v { font-size:14px }
.gv-bar { display:inline-block;width:70px;height:7px;background:#241313;border-radius:4px;vertical-align:middle }
.gv-bar span { display:block;height:7px;background:#22c55e;border-radius:4px }
</style>

<h2>{{.Gang.Name}}
  {{if .Gang.Activity}}<span class="gv-badge">{{.Gang.Activity}}</span>{{else}}<span class="gv-badge">standby</span>{{end}}
  {{if .Gang.AutoFormed}}<span class="gv-badge bot">corporation fleet</span>{{else}}<span class="gv-badge">hand-built</span>{{end}}
  {{if not .Gang.Enabled}}<span class="gv-badge">stood down</span>{{end}}
</h2>
<p class="gv-note"><a href="/rolebots/gangs">&larr; every fleet</a></p>

{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="card">
  <h3>Op Board</h3>
  <div class="gv-board">
    <div><div class="k">Stage</div><div class="v">{{.PhaseLabel}}</div></div>
    <div><div class="k">Doing</div><div class="v">{{if .OpNote}}{{.OpNote}}{{else}}<span class="gv-note">nothing posted yet</span>{{end}}</div></div>
    <div><div class="k">Where it should be</div><div class="v">{{if .OpWhere}}{{.OpWhere}}{{else}}<span class="gv-note">not chosen yet</span>{{end}}</div></div>
    <div><div class="k">Staging</div><div class="v">{{if .Staging}}{{.Staging}}{{else}}<span class="gv-note">&mdash;</span>{{end}}</div></div>
    <div><div class="k">Commander</div><div class="v">{{if .LeaderName}}{{.LeaderName}}{{else}}<span class="gv-note">unseated</span>{{end}}</div></div>
    <div><div class="k">Corporation</div><div class="v">{{if .CorpName}}{{.CorpName}}{{else}}<span class="gv-note">&mdash;</span>{{end}}</div></div>
    <div><div class="k">Crew</div><div class="v">{{.MemberCount}} <span class="gv-note">({{.OnStation}} in position)</span></div></div>
    <div><div class="k">Clock</div><div class="v">{{if .ClockNote}}{{.ClockNote}}{{else}}<span class="gv-note">&mdash;</span>{{end}}</div></div>
  </div>
  {{if .Cans}}
  <h4 style="margin-top:12px">Cans on the belt</h4>
  <table style="max-width:680px">
    <tr><th>Can</th><th>Owner</th><th>Belt</th><th>Filled</th><th>Age</th></tr>
    {{range .Cans}}<tr><td>{{.CanID}}</td><td>{{.OwnerName}}</td><td>{{.BeltItemID}}</td><td>{{.Fill}}</td><td>{{.Age}}</td></tr>{{end}}
  </table>
  {{end}}
  <p class="gv-note" style="margin-top:10px">This fleet flies itself: its commander picked the op, its corporation crewed it, and it winds down on its own clock. Nothing on this page steers it.</p>
</div>

<div class="card">
  <h3>Crew</h3>
  {{if .Members}}
  <table>
    <tr><th></th><th>Pilot</th><th>Seat</th><th>Flying</th><th>Where</th><th>Doing</th><th>Heading for</th><th>Hold</th></tr>
    {{range .Members}}
    <tr>
      <td style="width:40px"><img src="/portrait/{{.CharacterID}}?size=64" width="36" height="36" style="vertical-align:middle;border-radius:4px;background:#170a0a" onerror="this.style.display='none'"></td>
      <td>
        {{if .Driven}}<a href="/rolebot/{{.CharacterID}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}
        {{if eq .CharacterID $.Gang.LeaderCharID}}<span class="gv-badge bot">commander</span>{{end}}
        {{if not .Driven}}<span class="gv-badge player">rider</span>{{end}}
        <div class="gv-note">{{.CorpName}}</div>
      </td>
      <td>{{.Role}}</td>
      <td>{{.ShipName}}</td>
      <td style="font-size:12px">{{.Where}}{{if .OnGrid}} <span class="gv-badge bot">on grid</span>{{end}}</td>
      <td style="font-size:12px">
        {{if .Halted}}<strong style="color:#ef4444">HALTED</strong> <span style="color:#fca5a5">{{.LastError}}</span>
        {{else}}{{.Doing}}{{if .Detail}}<div class="gv-note">{{.Detail}}</div>{{end}}{{end}}
      </td>
      <td style="font-size:12px">{{if .Dest}}{{.Dest}}{{else}}<span class="gv-note">&mdash;</span>{{end}}</td>
      <td style="font-size:12px">{{if .HoldText}}<span class="gv-bar"><span style="width:{{.HoldPct}}%"></span></span> {{.HoldText}}{{else}}<span class="gv-note">&mdash;</span>{{end}}</td>
    </tr>
    {{end}}
  </table>
  {{else}}<p>Nobody is crewed to this fleet.</p>{{end}}
</div>
{{end}}`

// ── list handler ────────────────────────────────────────────────────────────

type gangListRow struct {
	GangID      int64
	Name        string
	Activity    string
	Phase       string
	LeaderName  string
	CorpName    string
	AutoFormed  bool
	ExpiresNote string
	ExpiresIn   int64
	MemberCount int
	OpNote      string
	// Search is everything the quick filter matches on, flattened here because a filter that reads
	// the DOM cannot see a column the table does not draw.
	// Window is the real fleet open for members (fleetmirror.go), if any.
	Window string
	Search string
}

// gangStageLabel names the op lifecycle stage in the operator's words. An empty OpPhase is a
// legacy or hand-built gang that never musters or winds down, so it reads live.
func gangStageLabel(g *Gang) string {
	switch g.State.OpPhase {
	case "muster":
		return "mustering"
	case "winddown":
		return "winding down"
	case "settled":
		return "settled"
	}
	if !g.Enabled {
		return "stood down"
	}
	return "live"
}

func (m *Mod) handleGangs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleGangsPost(w, r, ctx)
		return
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT g.gangID, COALESCE(lc.characterName, ''), COALESCE(corp.corporationName, ''),
		       (SELECT COUNT(*) FROM rolebot_gang_members gm WHERE gm.gangID = g.gangID)
		FROM rolebot_gangs g
		LEFT JOIN character_ lc ON lc.characterID = g.leaderCharID
		LEFT JOIN corporation corp ON corp.corporationID = g.corpID
		ORDER BY g.name`)
	if err != nil {
		espError(w, "rolebot-gangs", err.Error())
		return
	}
	type seed struct {
		id           int64
		leader, corp string
		members      int
	}
	var seeds []seed
	for rows.Next() {
		var s seed
		if rows.Scan(&s.id, &s.leader, &s.corp, &s.members) == nil {
			seeds = append(seeds, s)
		}
	}
	rows.Close()

	now := time.Now().Unix()
	acts := map[string]bool{}
	phases := map[string]bool{}
	windows := mirroredFleets()
	gangs := make([]gangListRow, 0, len(seeds))
	for _, s := range seeds {
		g, err := getGang(ctx, m.db, s.id)
		if err != nil || g == nil {
			continue
		}
		row := gangListRow{
			GangID:      g.GangID,
			Name:        g.Name,
			Activity:    g.Activity,
			Phase:       gangStageLabel(g),
			LeaderName:  s.leader,
			CorpName:    s.corp,
			AutoFormed:  g.AutoFormed,
			MemberCount: s.members,
			OpNote:      m.gangOpNote(ctx, g),
		}
		if row.Activity == "" {
			row.Activity = "standby"
		}
		if wnd, ok := windows[g.GangID]; ok {
			row.Window = fmt.Sprintf("fleet %d · %d aboard", wnd[0], wnd[1])
		}
		if g.AutoFormed && g.ExpiresAt > now {
			row.ExpiresIn = g.ExpiresAt - now
			row.ExpiresNote = fmt.Sprintf("%dh%02dm", row.ExpiresIn/3600, (row.ExpiresIn%3600)/60)
		}
		row.Search = strings.Join([]string{row.Name, row.Activity, row.Phase,
			row.LeaderName, row.CorpName, row.OpNote}, " ")
		acts[row.Activity] = true
		phases[row.Phase] = true
		gangs = append(gangs, row)
	}

	conscripts, err := m.conscriptRows(ctx)
	if err != nil {
		espError(w, "rolebot-gangs", err.Error())
		return
	}

	espRender(w, "rolebots_gangs.html", map[string]any{
		"Page":               "rolebot-gangs",
		"Tab":                "fleets",
		"Gangs":              gangs,
		"GangCount":          len(gangs),
		"Activities":         sortedKeys(acts),
		"Phases":             sortedKeys(phases),
		"GangFormOn":         GangForm(),
		"FleetMirrorOn":      FleetMirror(),
		"FleetNarrationOn":   FleetNarration(),
		"PointCallsOn":       PointCalls(),
		"SeatTTL":            seatRequestTTL.Load(),
		"CorpOrdersOn":       CorpOrders(),
		"OrderTTLHours":      orderTTLHours.Load(),
		"OrderRetries":       orderRetries.Load(),
		"FleetMaxOpen":       humanFleetMaxOpen.Load(),
		"HoldMax":            directiveHoldMax.Load(),

		"Conscripts":      conscripts,
		"ConscriptOn":     Conscription(),
		"ConscriptDefend": ConscriptDefence(),
		"ConscriptVoice":  conscriptVoiceOn.Load(),
		"MaxFleet":        conscriptMaxFleet.Load(),
		"MaxTotal":        conscriptMaxTotal.Load(),
		"IdleTTL":         conscriptIdleTTL.Load(),
		"VoiceGap":        conscriptVoiceGap.Load(),
		"VoicePerMin":     conscriptVoiceRate.Load(),

		"Msg": r.URL.Query().Get("msg"),
		"Err": r.URL.Query().Get("err"),
	})
}

// gangOpNote is the one-line op summary shared by the list and the Op Board.
func (m *Mod) gangOpNote(ctx context.Context, g *Gang) string {
	switch g.Activity {
	case ActMining, ActIndMining:
		if g.State.SystemID == 0 {
			return "waiting for a miner to pick the belt"
		}
		cans, _ := m.gangCansAt(ctx, g.GangID, g.State.SystemID)
		total := 0.0
		for _, c := range cans {
			total += c.UsedM3
		}
		return fmt.Sprintf("mining %s in %s — %d can(s), %.0f m³ waiting",
			g.State.OreName, g.State.SystemName, len(cans), total)
	case ActCombat:
		if g.State.HuntSystemID == 0 {
			return "picking a system to sweep"
		}
		return "sweeping " + g.State.HuntSystemName
	case ActSecurity:
		if g.State.Focus == gangFocusCheckpoint {
			return checkpointOpNote(&g.State)
		}
		if g.State.HuntSystemID == 0 {
			return "picking a system on home ground"
		}
		return "patrolling " + g.State.HuntSystemName
	case ActPiracy:
		if g.State.Focus == gangFocusGank {
			return m.gankOpNote(ctx, g)
		}
		if g.State.PirSystemID == 0 {
			return "picking a hunting ground"
		}
		if g.State.PirGateID != 0 {
			return "camping a gate in " + g.State.PirSystemName
		}
		return "prowling the belts of " + g.State.PirSystemName
	}
	return ""
}

// gangOpWhere is the system the fleet is supposed to be in, the one place every crew line is
// read against. It is the blackboard's own answer, never a census of where the crew is: the
// gap between the two is what the page shows.
func (m *Mod) gangOpWhere(ctx context.Context, g *Gang) string {
	switch g.Activity {
	case ActMining, ActIndMining:
		if g.State.SystemName != "" {
			return g.State.SystemName
		}
	case ActCombat, ActSecurity:
		if g.State.Focus == gangFocusCheckpoint && g.State.CheckpointSystemName != "" {
			return g.State.CheckpointSystemName
		}
		if g.State.HuntSystemName != "" {
			return g.State.HuntSystemName
		}
	case ActPiracy:
		if g.State.PirSystemName != "" {
			return g.State.PirSystemName
		}
	}
	// Before the op proper, and again once it is over, the fleet's place is its staging system.
	if g.State.StagingSystemID != 0 &&
		(g.State.OpPhase == "muster" || g.State.OpPhase == "winddown") {
		return m.systemLabel(ctx, g.State.StagingSystemID)
	}
	return ""
}

// systemLabel names a solar system, falling back to its ID so a missing row
// reads as a number rather than as an empty cell.
func (m *Mod) systemLabel(ctx context.Context, systemID int64) string {
	if systemID == 0 {
		return "—"
	}
	var name string
	if err := m.db.QueryRowContext(ctx,
		`SELECT solarSystemName FROM mapSolarSystems WHERE solarSystemID = ?`,
		systemID).Scan(&name); err != nil || name == "" {
		return strconv.FormatInt(systemID, 10)
	}
	return name
}

func (m *Mod) handleGangsPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	switch r.FormValue("action") {
	case "conscript", "conscriptRelease", "conscriptReleaseAll":
		// The player-fleet card posts here so the page has one action namespace; the handler lives
		// with the conscription code.
		m.handleConscriptsPost(ctx, w, r)
	case "fleetmirror":
		m.handleFleetMirrorPost(ctx, w, r)
	default:
		redirect(w, r, "/rolebots/gangs", "", "unknown action")
	}
}

// ── detail handler ──────────────────────────────────────────────────────────

type gangMemberRow struct {
	CharacterID int64
	Name        string
	CorpName    string
	Role        string
	Driven      bool
	ShipName    string
	Where       string
	OnGrid      bool
	Doing       string
	Detail      string
	Dest        string
	HoldPct     int
	HoldText    string
	Halted      bool
	LastError   string
}

type gangCanRow struct {
	CanID      int64
	OwnerName  string
	BeltItemID int64
	Fill       string
	Age        string
}

func (m *Mod) handleGangView(w http.ResponseWriter, r *http.Request, gangID int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	g, err := getGang(ctx, m.db, gangID)
	if err != nil {
		espError(w, "rolebot-gangs", err.Error())
		return
	}
	if g == nil {
		espError(w, "rolebot-gangs", fmt.Sprintf("gang %d does not exist", gangID))
		return
	}

	members, err := m.gangMemberRows(ctx, gangID)
	if err != nil {
		espError(w, "rolebot-gangs", err.Error())
		return
	}

	// Cans are the mining op's own progress bar; every other activity keeps
	// its progress on the blackboard the Op Board already reads.
	var canRows []gangCanRow
	if (g.Activity == ActMining || g.Activity == ActIndMining) && g.State.SystemID != 0 {
		if cans, err := m.gangCansAt(ctx, g.GangID, g.State.SystemID); err == nil {
			for _, c := range cans {
				name := ""
				_ = m.db.QueryRowContext(ctx,
					`SELECT characterName FROM character_ WHERE characterID = ?`, c.OwnerID).Scan(&name)
				age := "—"
				if c.CreatedAt > 0 {
					age = fmt.Sprintf("%dm", (time.Now().Unix()-c.CreatedAt)/60)
				}
				canRows = append(canRows, gangCanRow{
					CanID: c.CanID, OwnerName: name, BeltItemID: c.BeltItemID,
					Fill: fmt.Sprintf("%.0f / %.0f m³", c.UsedM3, canCapacityM3),
					Age:  age,
				})
			}
		}
	}

	leaderName, corpName := "", ""
	if g.LeaderCharID != 0 {
		_ = m.db.QueryRowContext(ctx,
			`SELECT characterName FROM character_ WHERE characterID = ?`,
			g.LeaderCharID).Scan(&leaderName)
	}
	if g.CorpID != 0 {
		_ = m.db.QueryRowContext(ctx,
			`SELECT corporationName FROM corporation WHERE corporationID = ?`,
			g.CorpID).Scan(&corpName)
	}

	// In position is the crew who have reached where the fleet is supposed to be. It says whether
	// an op is happening or everybody is still in transit.
	where := m.gangOpWhere(ctx, g)
	onStation := 0
	if where != "" && where != "—" {
		for _, mem := range members {
			if strings.Contains(mem.Where, where) {
				onStation++
			}
		}
	}

	staging := ""
	if g.State.StagingStationID != 0 {
		staging = m.stationName(ctx, g.State.StagingStationID)
	}

	espRender(w, "rolebots_gang_view.html", map[string]any{
		"Page":        "rolebot-gangs",
		"Gang":        g,
		"LeaderName":  leaderName,
		"CorpName":    corpName,
		"PhaseLabel":  gangStageLabel(g),
		"OpNote":      m.gangOpNote(ctx, g),
		"OpWhere":     where,
		"Staging":     staging,
		"ClockNote":   gangClockNote(g),
		"MemberCount": len(members),
		"OnStation":   onStation,
		"Members":     members,
		"Cans":        canRows,
		"Err":         r.URL.Query().Get("err"),
	})
}

// gangClockNote is whichever deadline the fleet is flying against. At most one is live at a
// time, so they share a cell.
func gangClockNote(g *Gang) string {
	now := time.Now().Unix()
	left := func(at int64) string {
		d := at - now
		if d < 0 {
			return "overdue"
		}
		return fmt.Sprintf("%dm%02ds", d/60, d%60)
	}
	switch {
	case g.State.OpPhase == "muster" && g.State.MusterDeadline > 0:
		return "muster closes in " + left(g.State.MusterDeadline)
	case g.State.OpPhase == "winddown" && g.State.WindDownDeadline > 0:
		return "settles in " + left(g.State.WindDownDeadline)
	case g.AutoFormed && g.ExpiresAt > now:
		d := g.ExpiresAt - now
		return fmt.Sprintf("winds down in %dh%02dm", d/3600, (d%3600)/60)
	}
	return ""
}

func (m *Mod) gangMemberRows(ctx context.Context, gangID int64) ([]gangMemberRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT gm.characterID, gm.role,
		       c.characterName, COALESCE(corp.corporationName, ''),
		       COALESCE(b.enabled, 0),
		       COALESCE(sta.stationName, ''), COALESCE(ms.solarSystemName, ''),
		       COALESCE(t.typeName, ''), COALESCE(ship.itemID, 0)
		FROM rolebot_gang_members gm
		JOIN character_ c ON c.characterID = gm.characterID
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		LEFT JOIN rolebot_characters b ON b.characterID = gm.characterID
		LEFT JOIN staStations sta ON sta.stationID = c.stationID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = c.solarSystemID
		LEFT JOIN entity ce ON ce.itemID = c.characterID
		LEFT JOIN entity ship ON ship.itemID = ce.locationID AND ship.ownerID = c.characterID
		LEFT JOIN invTypes t ON t.typeID = ship.typeID
		WHERE gm.gangID = ?
		ORDER BY gm.role, c.characterName`, gangID)
	if err != nil {
		return nil, fmt.Errorf("gangMemberRows: %w", err)
	}
	defer rows.Close()

	var out []gangMemberRow
	shipOf := map[int64]int64{}
	for rows.Next() {
		var mr gangMemberRow
		var driven, shipID int64
		var station, system string
		if err := rows.Scan(&mr.CharacterID, &mr.Role, &mr.Name, &mr.CorpName,
			&driven, &station, &system, &mr.ShipName, &shipID); err != nil {
			return nil, fmt.Errorf("gangMemberRows scan: %w", err)
		}
		mr.Driven = driven != 0
		switch {
		case station != "":
			mr.Where = "Docked — " + station
		case system != "":
			mr.Where = "In space — " + system
		default:
			mr.Where = "unknown"
		}
		if mr.ShipName == "" {
			mr.ShipName = "capsule"
		}
		shipOf[mr.CharacterID] = shipID
		out = append(out, mr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The sim's own account of each pilot, which the character row cannot give: which leg it is
	// on and where that leg ends.
	for i := range out {
		mr := &out[i]
		if !mr.Driven {
			mr.Doing = "riding along (not driven)"
			continue
		}
		st, err := loadState(ctx, m.db, mr.CharacterID)
		if err != nil {
			return nil, err
		}
		mr.Doing = phaseLabel(st)
		mr.Detail = m.phaseDetail(ctx, st)
		mr.Dest = m.destLabel(ctx, st)
		mr.Halted = st.Halted
		mr.LastError = st.LastError
		mr.OnGrid = m.isEmbodied(mr.CharacterID)
		if shipOf[mr.CharacterID] != 0 {
			mr.HoldPct, mr.HoldText = m.holdGauge(ctx, mr.CharacterID, shipOf[mr.CharacterID])
		}
	}
	return out, nil
}
