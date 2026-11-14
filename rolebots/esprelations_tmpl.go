package rolebots

// The Relations page's markup, split from its handler as the ore and industry pages are. It follows the
// module page rules stated at listKit (esptrade_tmpl.go): listKit and the page's <style> go inside
// {{define "content"}}, and with no funcMap every number is a preformatted string or a row field.
const tmplRelations = corpTabs + `{{define "content"}}` + castTabsStyle + `
` + listKit + `
<style>
.rb-rel-sum { display:grid; grid-template-columns:repeat(auto-fit,minmax(165px,1fr));
              gap:10px; margin-bottom:12px; }
.rb-rel-sum div { background:#0a0f17; border:1px solid #1a2230; border-radius:6px;
                  padding:9px 11px; }
.rb-rel-sum .k { font-size:10.5px; color:#6b7280; }
.rb-rel-sum .v { font-size:19px; color:#dce6f0; font-variant-numeric:tabular-nums; }
.rb-rel-sum .v.warn { color:#eab308; }
.rb-rel-sum .v.bad { color:#dc2626; }
.rb-rel td, .rb-rel th { font-size:12px; padding:4px 6px; }
.rb-rel td.n, .rb-rel th.n { text-align:right; font-variant-numeric:tabular-nums; }
.rb-t-despised { color:#dc2626; }
.rb-t-disliked { color:#eab308; }
.rb-t-neutral  { color:#9ca3af; }
.rb-t-liked    { color:#4ade80; }
.rb-t-devoted  { color:#22d3ee; }
.rb-w-hostile  { color:#dc2626; font-weight:600; }
.rb-w-arming   { color:#eab308; }
.rb-w-finished { color:#4b5563; }
.rb-w-future   { color:#c084fc; }
.rb-none { color:#4b5563; }
.rb-dial { display:flex; gap:8px; align-items:center; flex-wrap:wrap; margin:8px 0 0; }
.rb-dial input[type=number] { width:90px; }
.rb-dial label { font-size:12px; color:#9ca3af; min-width:210px; }
.rb-onesided { color:#eab308; font-size:11px; }
</style>

<h2>Diplomacy</h2>
{{template "rb_corptabs" .}}
{{if .Msg}}<div class="alert">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

{{if not .Enabled}}
<div class="alert alert-error">
  The relations layer is <b>off</b>. Every bot reads exactly as it did before this
  existed: no corporation has an enemy, no war opens highsec, and nothing below
  reaches a pilot.
</div>
{{end}}
{{if .Stale}}
<div class="alert alert-error">
  The snapshot has not rebuilt in over {{.StaleMins}} minutes, so the <b>war half of
  it is no longer believed</b> &mdash; declared standings still apply, wars read as
  cold. That is deliberate: a stale war flag would have a bot open fire in highsec
  expecting an exemption the server may no longer grant.
</div>
{{end}}

<div class="rb-rel-sum">
  <div><div class="k">SNAPSHOT BUILT</div><div class="v">{{.Age}}</div></div>
  <div><div class="k">DECLARED PAIRS</div><div class="v">{{.Stats.Pairs}}</div></div>
  <div><div class="k">WARS</div><div class="v">{{.Stats.Wars}}</div></div>
  <div><div class="k">HOSTILE NOW</div><div class="v{{if .Stats.HostileNow}} bad{{end}}">{{.Stats.HostileNow}}</div></div>
  <div><div class="k">CORPS WITH ENEMIES</div><div class="v">{{.Stats.Viewers}}</div></div>
  <div><div class="k">ENEMY-HELD SYSTEMS</div><div class="v">{{.Stats.EnemySys}}</div></div>
</div>

<div class="card">
  <h3 style="margin-top:0">What this page does</h3>
  <p class="muted" style="margin:0 0 8px">
    The switches and dials of corporate politics. Everything about ONE corporation
    &mdash; what it holds toward others and they toward it, its wars and mercenary
    paper, its incident ledger, its letters and pacts, and the declare and hand-back
    forms &mdash; is on that corporation's own page (<a href="/rolebots/corps">corporations</a>
    &rarr; a corporation &rarr; Relations).
  </p>
  <p class="muted" style="margin:0">
    A standing of <b>&minus;2.5 or worse</b> makes a corporation an enemy: that side's
    combat pilots hunt it, and its miners, haulers and industrialists price its systems
    as somewhere to be avoided. Highsec stays safe between them until the two are at
    <b>war</b>. Bot-run corporations hold a live value per pair that incidents move
    and decay pulls back to the CEO's own leaning; what they write to the game is that
    value, so &minus;3.2 is a grudge cooling and +7.5 an ally.
  </p>
</div>

<div class="card">
  <h3 style="margin-top:0">CEO wars &amp; mercenaries</h3>
  <p class="muted" style="margin:0 0 8px">
    Bot CEOs may take a quarrel to CONCORD &mdash; declare, pay the weekly bill, give
    up &mdash; and corporations may hire a mercenary outfit to declare a war for them.
    A merc declares a <b>real</b> war of its own (the client has no ally slot), so both
    sides see it in their Wars tab. Every obligation is a commitment against free
    capital: nothing here takes a wallet below its reserve.
  </p>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.war.ceoWars">
    <label>Bot CEOs make war</label>
    <button type="submit" name="on" value="{{if .CEOWars}}0{{else}}1{{end}}">
      {{if .CEOWars}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">Off: nothing new is declared and every bill a CEO owes lapses, ending its wars on CONCORD's timer.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.war.mercMarket">
    <label>Mercenary market</label>
    <button type="submit" name="on" value="{{if .MercMarket}}0{{else}}1{{end}}">
      {{if .MercMarket}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">Off: open tenders are withdrawn and running contracts end at their next payday.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.war.gankHires">
    <label>Gank victims may hire</label>
    <button type="submit" name="on" value="{{if .GankHires}}0{{else}}1{{end}}">
      {{if .GankHires}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">With the market shut, a corporation suicide-ganked in highsec may still put out a tender for war on the gankers, and a mercenary may take it. A gank also weighs four kills on the ledger and scores its own war term past the grievance cap.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.war.mutualWars">
    <label>Mutual wars</label>
    <button type="submit" name="on" value="{{if .MutualWars}}0{{else}}1{{end}}">
      {{if .MutualWars}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">A war both sides are fighting and want to keep fighting turns mutual &mdash; only when both consent &mdash; and stops costing a bill. Either side withdrawing (cooled, quiet, or losing) hands it back to the bill, and it dies when that goes unpaid. Off: every mutual war a bot consented to is handed back.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="rolebots.war.chestMaxFrac">
    <label>War chest ceiling (share of free capital)</label>
    <input type="number" name="value" step="0.05" min="0.05" max="0.9" value="{{.WarChestMax}}">
    <button type="submit">Save</button>
    <span class="muted">The most any one war or hire may commit, whatever the CEO's temperament.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="rolebots.war.mercWagePerPilot">
    <label>Merc wage (ISK per pilot per week)</label>
    <input type="number" name="value" step="100000" min="10000" value="{{.MercWage}}">
    <button type="submit">Save</button>
    <span class="muted">What a mercenary prices its labour from, before risk, politics and greed.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="rolebots.war.avoidMul">
    <label>War enemy route cost (&times; enemy)</label>
    <input type="number" name="value" step="0.5" min="1" value="{{.WarAvoidMul}}">
    <button type="submit">Save</button>
    <span class="muted">A war enemy may shoot in highsec; its systems cost this many times a rival's to fly through.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="rolebots.war.markMul">
    <label>War enemy hunting pull (&times; enemy)</label>
    <input type="number" name="value" step="0.5" min="1" value="{{.WarMarkMul}}">
    <button type="submit">Save</button>
    <span class="muted">How much harder hunters go after a war enemy's ground than a rival's.</span>
  </form>

  <h4>CEO war books</h4>
  {{if .WarBook}}
  <table class="rb-rel rb-sortable">
    <thead><tr><th class="n">War</th><th>Corporation</th><th>Against</th><th>Motive</th><th class="n">Bills paid</th><th>Note</th></tr></thead>
    <tbody>
    {{range .WarBook}}
      <tr><td class="n" data-v="{{.WarID}}">{{.WarID}}</td><td>{{.Corp}}</td><td>{{.Target}}</td>
          <td>{{.Motive}}</td><td class="n">{{.Bills}}</td><td class="muted">{{.Note}}</td></tr>
    {{end}}
    </tbody>
  </table>
  {{else}}<p class="rb-none" style="margin:0">No CEO is running a war.</p>{{end}}

  <h4>Mercenary tenders</h4>
  {{if .MercJobs}}
  <table class="rb-rel rb-sortable">
    <thead><tr><th class="n">Job</th><th>Client</th><th>Kind</th><th>Target</th><th class="n">Budget</th><th class="n">Round</th><th>Status</th><th>Quotes</th></tr></thead>
    <tbody>
    {{range .MercJobs}}
      <tr><td class="n" data-v="{{.JobID}}">{{.JobID}}</td><td>{{.Client}}</td><td>{{.Kind}}</td><td>{{.Target}}</td>
          <td class="n">{{.Budget}}</td><td class="n">{{.Round}}</td><td>{{.Status}}</td><td class="muted">{{.Quotes}}</td></tr>
    {{end}}
    </tbody>
  </table>
  {{else}}<p class="rb-none" style="margin:0">No tenders.</p>{{end}}

  <h4>Mercenary contracts</h4>
  {{if .MercContracts}}
  <table class="rb-rel rb-sortable">
    <thead><tr><th class="n">Contract</th><th>Client</th><th>Merc</th><th>Target</th><th class="n">Weekly</th><th class="n">Paid</th><th class="n">Destroyed</th><th>Status</th><th>Note</th></tr></thead>
    <tbody>
    {{range .MercContracts}}
      <tr><td class="n" data-v="{{.ID}}">{{.ID}}</td><td>{{.Client}}</td><td>{{.Merc}}</td><td>{{.Target}}</td>
          <td class="n">{{.Weekly}}</td><td class="n">{{.Paid}}</td><td class="n">{{.Destroyed}}</td>
          <td>{{.Status}}</td><td class="muted">{{.Note}}</td></tr>
    {{end}}
    </tbody>
  </table>
  {{else}}<p class="rb-none" style="margin:0">No contracts.</p>{{end}}
</div>

<div class="card">
  <h3 style="margin-top:0">The snapshot</h3>
  <p class="muted" style="margin:0 0 8px">
    The resolved view every pilot reads: after precedence, alliance expansion, the
    mutual floor and the wars. Per corporation, on its own page.
  </p>
  <form method="POST" action="/rolebots/corps/diplomacy" style="margin-top:10px">
    <input type="hidden" name="action" value="rebuild">
    <button type="submit">Rebuild now</button>
    <span class="muted" style="margin-left:8px">Otherwise every {{.RefreshSec}}s.</span>
  </form>
</div>

<div class="card">
  <h3 style="margin-top:0">Switches</h3>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.relations.enabled">
    <label>Corporate politics</label>
    <button type="submit" name="on" value="{{if .Enabled}}0{{else}}1{{end}}">
      {{if .Enabled}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">Off restores the behaviour that shipped before this feature.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.relations.mutual">
    <label>Hostility is mutual</label>
    <button type="submit" name="on" value="{{if .Mutual}}0{{else}}1{{end}}">
      {{if .Mutual}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">A one-sided declaration makes both sides fight.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.war.everywhere">
    <label>A war opens highsec</label>
    <button type="submit" name="on" value="{{if .WarEverywhere}}0{{else}}1{{end}}">
      {{if .WarEverywhere}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">Off, policed space stays passive even between corporations at war.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.relations.ceoDiplomacy">
    <label>Bot CEOs choose sides</label>
    <button type="submit" name="on" value="{{if .CEODiplomacy}}0{{else}}1{{end}}">
      {{if .CEODiplomacy}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">A bot CEO declares its corporation's friends, allies and rivals. Off leaves what it declared in place; declaring here for a CEO's corporation takes that pair out of its hands.</span>
  </form>
</div>

<div class="card">
  <h3 style="margin-top:0">Dials</h3>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="rolebots.relations.enemyCost">
    <label>Enemy belt cost (jumps)</label>
    <input type="number" name="value" step="0.5" min="0" value="{{.EnemyCost}}">
    <button type="submit">Save</button>
    <span class="muted">What a system an enemy works is worth flying past. 0 turns belt avoidance off.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="rolebots.relations.enemyHopJumps">
    <label>Enemy route cost (jumps)</label>
    <input type="number" name="value" step="1" min="0" value="{{.EnemyHop}}">
    <button type="submit">Save</button>
    <span class="muted">What passing through one adds to a leg. Small: this is every trip, not one destination.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="rolebots.war.opportunityMul">
    <label>War combat pull</label>
    <input type="number" name="value" step="0.1" min="1" value="{{.WarOppMul}}">
    <button type="submit">Save</button>
    <span class="muted">How much harder a corporation at war fields its combat wing.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="rolebots.relations.refreshSec">
    <label>Snapshot period (seconds)</label>
    <input type="number" name="value" step="10" min="30" value="{{.RefreshSec}}">
    <button type="submit">Save</button>
    <span class="muted">The arming boundary stays exact whatever this is &mdash; it is re-tested on every read.</span>
  </form>
</div>

<div class="card">
  <h3 style="margin-top:0">The ledger</h3>
  <p class="muted" style="margin:0 0 8px">
    The dynamic layer: incidents between corporations (kills, hold-ups, bounties,
    sieges, wars, crowded belts, letters) move a live value per pair, decay pulls it
    back toward the CEO's own leaning, and the value is what gets written. Folded every
    few minutes; written on a tier crossing, or at the Director of Diplomacy's pass.
  </p>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.relations.ledger">
    <label>Ledger</label>
    <button type="submit" name="on" value="{{if .Ledger}}0{{else}}1{{end}}">
      {{if .Ledger}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">Off, values freeze, nothing is written and no letter goes out; standings already written stay.</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.relations.letters">
    <label>Letters</label>
    <button type="submit" name="on" value="{{if .Letters}}0{{else}}1{{end}}">
      {{if .Letters}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">Pacts, alliances of friendship, reparations and peace, proposed by the Director of Diplomacy (or the CEO).</span>
  </form>
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="switch">
    <input type="hidden" name="key" value="rolebots.relations.envoys">
    <label>Envoys</label>
    <button type="submit" name="on" value="{{if .Envoys}}0{{else}}1{{end}}">
      {{if .Envoys}}Turn off{{else}}Turn on{{end}}</button>
    <span class="muted">An idle member carries each letter to the other corporation's headquarters. Off, letters go by mail.</span>
  </form>
  {{range .LedgerDials}}
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="{{.Key}}">
    <label>{{.Label}}</label>
    <input type="number" name="value" step="{{.Step}}" value="{{.Value}}">
    <button type="submit">Save</button>
    <span class="muted">{{.Key}}</span>
  </form>
  {{end}}
</div>

<div class="card">
  <h3 style="margin-top:0">Grace</h3>
  <p class="muted" style="margin:0 0 8px">
    What a bot holds against, or owes, a named pilot: kills, hold-ups, ransoms, fair fights,
    favours from its own leadership. Each bot keeps its own ledger, decayed on the grace trait's
    clock; a real player sees it on their character sheet only when a band is crossed.
  </p>
  <p class="muted" style="margin:0 0 8px">The ledger is always on.</p>
  {{range .GraceDials}}
  <form method="POST" action="/rolebots/corps/diplomacy" class="rb-dial">
    <input type="hidden" name="action" value="dial">
    <input type="hidden" name="key" value="{{.Key}}">
    <label>{{.Label}}</label>
    <input type="number" name="value" step="{{.Step}}" value="{{.Value}}">
    <button type="submit">Save</button>
    <span class="muted">{{.Key}}</span>
  </form>
  {{end}}
</div>
{{end}}`
