package rolebots

// The Ore page's markup, split from its handler as espindustry.go and espindustry_tmpl.go are. It
// follows the module page rules stated at listKit (esptrade_tmpl.go), with industryTabs concatenated in.
const tmplOre = industryTabs + `{{define "content"}}
` + industryTabsStyle + listKit + `
<style>
.rb-ore-sum { display:grid; grid-template-columns:repeat(auto-fit,minmax(175px,1fr));
              gap:10px; margin-bottom:12px; }
.rb-ore-sum div { background:#170a0a; border:1px solid #2a1a1a; border-radius:6px;
                  padding:9px 11px; }
.rb-ore-sum .k { font-size:10.5px; color:#6b7280; }
.rb-ore-sum .v { font-size:19px; color:#f0e6dc; font-variant-numeric:tabular-nums; }
.rb-ore-sum .v.bad { color:#dc2626; }
.rb-ore-sum .v.good { color:#4ade80; }
.rb-ore-sum .v.warn { color:#eab308; }
.rb-ore td, .rb-ore th { font-size:12px; padding:4px 6px; }
.rb-ore td.n, .rb-ore th.n { text-align:right; font-variant-numeric:tabular-nums; }
.rb-none { color:#4b5563; }
.rb-yes { color:#4ade80; }
.rb-no { color:#9ca3af; }
.rb-warn { color:#eab308; }
.rb-edge { display:flex; gap:8px; align-items:center; flex-wrap:wrap; margin:10px 0 0; }
.rb-edge input[type=number] { width:80px; }
.rb-bar { height:5px; background:#2a1a1a; border-radius:3px; overflow:hidden; margin-top:2px; }
.rb-bar i { display:block; height:100%; background:#4b7f52; }
.rb-bar.thin i { background:#b45309; }
.rb-bar.bare i { background:#991b1b; }
</style>

<h2>Ore Supply</h2>
{{template "rb_indtabs" .}}
{{if .Msg}}<div class="alert">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="card">
  <h3 style="margin-top:0">How the ore market is priced</h3>
  <p class="muted" style="margin:0 0 8px">
    Miners list their holds, factories buy and bid for rock, traders flip it and stranded
    stacks get swept. That is standard logic and there is no switch for it &mdash; the five
    that used to sit here were a staged rollout, and the rollout is over. What is left are the
    two numbers, because they are tuning rather than on-and-off.
  </p>
  <div class="rb-edge">
    <form method="POST">
      <input type="hidden" name="action" value="askEdge">
      <label class="muted">a listing leaves the buyer
        <input type="number" name="value" min="1" max="1.5" step="0.05" value="{{.AskEdge}}">
        &times; margin</label>
      <button type="submit">Save</button>
    </form>
    <form method="POST">
      <input type="hidden" name="action" value="subEdge">
      <label class="muted">rock must be
        <input type="number" name="value" min="0.8" max="1.5" step="0.05" value="{{.SubEdge}}">
        &times; the cost of metal</label>
      <button type="submit">Save</button>
    </form>
  </div>
  <p class="muted" style="margin:10px 0 0">
    The first is what a miner deliberately leaves on the table: price at a refiner&rsquo;s exact
    break-even and nobody has a reason to act, which is not listing at all with a broker fee
    attached. The second is how much cheaper rock has to be than metal before a factory buys it
    &mdash; 1.00 is &ldquo;strictly cheaper&rdquo;. Neither applies to a factory that cannot
    buy the metal at all: <b>if it needs the material it takes the rock</b>, whatever the
    arithmetic says, because the comparison only means something when there is something to
    compare against.
  </p>
</div>

<div class="card">
  <p class="muted" style="margin:0 0 6px">
    The Minerals tab asks whether a region's shelves are stocked. This one asks the question
    underneath it: a shelf is stocked by <b>refining</b>, refining is fed by <b>rock</b>, and
    until this counter existed nothing recorded what became of the rock. The number to read
    first is the <b>sink share</b> &mdash; how much of everything mined went into an
    <em>NPC bid</em>, which pays forever and takes the units out of the game. Whatever is sold
    into an NPC bid is not on anyone's market as a mineral, was not built into anything, and
    never will be.
  </p>
  <p class="muted" style="margin:0">
    Counted per day and kept for {{.KeepDays}} days. A trip's rock is counted once when it comes
    out of the belt and again at whatever it was disposed of, so the disposal rows are shares of
    the mined row rather than a second population.
  </p>
</div>

<div class="rb-ore-sum">
  <div><div class="k">of today's rock, into an NPC bid</div>
       <div class="v bad">{{.SinkShareDay}}</div></div>
  <div><div class="k">same, over seven days</div>
       <div class="v">{{.SinkShareWeek}}</div></div>
  <div><div class="k">of the week's rock, refined</div>
       <div class="v good">{{.RefinedShare}}</div></div>
  <div><div class="k">minerals out per unit of ore in</div>
       <div class="v">{{.RealisedYield}}</div></div>
  <div><div class="k">pilots who can refine at a profit</div>
       <div class="v">{{.RefinerShare}} <span style="font-size:12px;color:#6b7280">{{.RefinerPays}} of {{.RefinerTotal}}</span></div></div>
  <div><div class="k">one level of Refining short of the bar</div>
       <div class="v warn">{{.RefinerShort}}</div></div>
  <div><div class="k">of today's rock, rested on the market</div>
       <div class="v good">{{.ListedShareDay}}</div></div>
  <div><div class="k">units bought to refine today</div>
       <div class="v">{{.BoughtDay}}</div></div>
</div>

<div class="card">
  <h3 style="margin-top:0">Where the rock went</h3>
  <p class="muted" style="margin:0 0 8px">
    Today beside the last seven days. <b>Mined</b> is the denominator; everything below it is a
    disposal. <b>Stockpiled</b> is neither a loss nor a supply &mdash; it is the pile that has
    not decided yet &mdash; and <b>jettisoned</b> is a fleet miner filling a can its hauler
    will collect, which is counted again under the fleet row when the commander disposes of it.
  </p>
  <p class="muted" style="margin:0 0 8px">
    <b>Listed</b> is not a sale: the rock is resting on a book and still belongs to the miner,
    and its fill turns up again later under one of the two sold rows. <b>Bought to refine</b> is
    the other side of the same market &mdash; a builder taking ore instead of metal, or a trader
    flipping a stack. The two will not balance, because a purchase off an NPC ask has no bot sale
    to pair with. The number to watch is <b>sold to an NPC bid</b>, which should FALL: those
    units leave the game, and nothing they would have refined into reaches anybody's market.
  </p>
  <table class="rb-ore">
    <thead><tr>
      <th>Stage</th><th class="n">Units today</th><th class="n">m&sup3; today</th>
      <th class="n">Units, 7d</th><th class="n">m&sup3;, 7d</th><th class="n">ISK, 7d</th>
    </tr></thead>
    <tbody>
      {{range $i, $d := .Day}}
      <tr>
        <td>{{$d.Label}}</td>
        <td class="n">{{$d.UnitsStr}}</td>
        <td class="n">{{$d.M3Str}}</td>
        {{with index $.Week $i}}
        <td class="n">{{.UnitsStr}}</td>
        <td class="n">{{.M3Str}}</td>
        <td class="n">{{.ISKStr}}</td>
        {{end}}
      </tr>
      {{end}}
    </tbody>
  </table>
</div>

<div class="card">
  <h3 style="margin-top:0">What a rock is worth</h3>
  <p class="muted" style="margin:0 0 8px">
    <b>Break-even</b> is the net refining yield at which this ore's own refined value and its own
    best bid are the same number &mdash; the real version of the flat
    {{.RefineBar}} bar the disposal rule applies. A rock whose break-even is at or under
    {{.ReferenceYield}} (a station-standard plant worked by Refining&nbsp;IV / Refinery
    Efficiency&nbsp;IV, after tax) is marked <span class="rb-yes">worth refining</span>; the rest
    are worth more as rock. Refined value is priced off the best live <em>bid</em> on each mineral,
    because that is what a refiner can actually realise today.
  </p>
  <p class="muted" style="margin:0 0 8px">
    The volume pair is why the ore market is local and the mineral market is not. A batch of
    Arkonor is 3,200&nbsp;m&sup3; of rock and 8&nbsp;m&sup3; of metal: nobody can haul the rock to a
    hub, so it has to be bought where it is dug. An ore with <span class="rb-none">no ask</span>
    is not for sale anywhere in the game.
  </p>
  <input class="rb-filter" data-target="oretab" placeholder="filter by name or group&hellip;">
  <table class="rb-ore rb-sortable" id="oretab">
    <thead><tr>
      <th>Ore</th><th>Group</th>
      <th class="n">Mined, 7d</th>
      <th class="n">NPC bid</th><th class="n">Bot bid</th><th class="n">Ask</th>
      <th class="n">Refined value/unit</th><th class="n">Break-even</th>
      <th class="n">Raw m&sup3;/batch</th><th class="n">Refined m&sup3;</th><th class="n">Compression</th>
      <th class="n">ISK/m&sup3; raw</th><th class="n">ISK/m&sup3; refined</th>
    </tr></thead>
    <tbody>
      {{range .Econ}}
      <tr>
        <td>{{.Name}}{{if .PaysToRefine}} <span class="rb-yes" title="refining beats the bid">&#10003;</span>{{end}}</td>
        <td class="rb-no">{{.Group}}</td>
        <td class="n" data-v="{{.Mined}}">{{.MinedStr}}</td>
        <td class="n" data-v="{{.NPCBid}}">{{.NPCBidStr}}</td>
        <td class="n" data-v="{{.BotBid}}">{{if .BotBid}}{{.BotBidStr}}{{else}}<span class="rb-none">none</span>{{end}}</td>
        <td class="n" data-v="{{.Ask}}">{{if .Ask}}{{.AskStr}}{{else}}<span class="rb-none">none</span>{{end}}</td>
        <td class="n" data-v="{{.Intrinsic}}">{{.IntrinsicStr}}</td>
        <td class="n" data-v="{{.BreakEven}}">{{.BreakEvenStr}}</td>
        <td class="n" data-v="{{.RawM3}}">{{.RawM3Str}}</td>
        <td class="n" data-v="{{.RefM3}}">{{.RefM3Str}}</td>
        <td class="n" data-v="{{.Compression}}">{{.CompressionStr}}</td>
        <td class="n" data-v="{{.ISKPerM3Raw}}">{{.RawIPMStr}}</td>
        <td class="n" data-v="{{.ISKPerM3Refined}}">{{.RefIPMStr}}</td>
      </tr>
      {{else}}
      <tr><td colspan="13" class="rb-none">No reprocessable ore in the type catalogue.</td></tr>
      {{end}}
    </tbody>
  </table>
</div>

<div class="card">
  <h3 style="margin-top:0">Who can refine</h3>
  <p class="muted" style="margin:0 0 8px">
    Read off <b>skills</b>, not off the counter: the question is not who reprocessed something
    yesterday but who <em>could</em>, because a pilot one level short is the one worth doing
    something about. Yield is at a station-standard plant after the usual 5% tax; the bar a
    disposal is judged against is {{.RefineBar}}. Every enabled pilot is counted here
    &mdash; {{.RefinerTotal}} of them, {{.RefinerTrained}} with any refining skill at all.
  </p>
  <p class="muted" style="margin:0 0 8px">
    <b>One level short</b> is the row to act on. Refinery&nbsp;Efficiency has the bigger term
    (0.04 a level against Refining's 0.02) but it is rank&nbsp;3 to Refining's rank&nbsp;1
    <em>and</em> needs Refining&nbsp;V first, so a pilot under the bar always reaches it through
    Refining &mdash; which is why the census counts that level and not the other one.
  </p>
  <table class="rb-ore rb-sortable">
    <thead><tr>
      <th class="n">Refining</th><th class="n">Refinery Eff.</th><th class="n">Pilots</th>
      <th class="n">Share</th><th class="n">Yield</th><th>Standing</th>
    </tr></thead>
    <tbody>
      {{range .RefinerBands}}
      <tr>
        <td class="n" data-v="{{.Refining}}">{{.Refining}}</td>
        <td class="n" data-v="{{.Efficient}}">{{.Efficient}}</td>
        <td class="n" data-v="{{.Pilots}}">{{.PilotsStr}}</td>
        <td class="n" data-v="{{.Share}}">{{.ShareStr}}</td>
        <td class="n" data-v="{{.Yield}}">{{.YieldStr}}</td>
        <td>{{if .Pays}}<span class="rb-yes">{{.Verdict}}</span>{{else if .OneShort}}<span class="rb-warn">{{.Verdict}}</span>{{else}}<span class="rb-no">{{.Verdict}}</span>{{end}}</td>
      </tr>
      {{else}}
      <tr><td colspan="6" class="rb-none">No enabled pilots on the roster.</td></tr>
      {{end}}
    </tbody>
  </table>
</div>

<div class="card">
  <h3 style="margin-top:0">A few of the best</h3>
  <p class="muted" style="margin:0 0 8px">
    {{.RefinerNamed}} named pilots off the top of the distribution above, so a row can be
    followed to a character. Not a ranking of the roster and not the whole of it &mdash; the
    table above is the population.
  </p>
  <table class="rb-ore rb-sortable">
    <thead><tr>
      <th>Pilot</th><th class="n">Refining</th><th class="n">Refinery Eff.</th>
      <th class="n">Yield</th><th class="n">Next level</th><th>Beats the bid</th>
    </tr></thead>
    <tbody>
      {{range .Refiners}}
      <tr>
        <td><a href="/rolebot/{{.CharID}}">{{if .Name}}{{.Name}}{{else}}char {{.CharID}}{{end}}</a></td>
        <td class="n" data-v="{{.Refining}}">{{.Refining}}</td>
        <td class="n" data-v="{{.Efficient}}">{{.Efficient}}</td>
        <td class="n" data-v="{{.Yield}}">{{.YieldStr}}</td>
        <td class="n" data-v="{{.NextGain}}">{{.NextStr}}</td>
        <td>{{if .Pays}}<span class="rb-yes">yes</span>{{else}}<span class="rb-no">no</span>{{end}}</td>
      </tr>
      {{else}}
      <tr><td colspan="6" class="rb-none">Nobody on the roster has trained a refining skill.</td></tr>
      {{end}}
    </tbody>
  </table>
</div>

<div class="card">
  <h3 style="margin-top:0">What is left in the belts</h3>
  <p class="muted" style="margin:0 0 8px">
    Written down whenever a pilot actually works a field, joined to the asteroid seed &mdash; a
    regenerated field rolls a new seed, the row drops out, and the system correctly reads as
    untouched again rather than reporting last cycle's depletion against this cycle's rocks. Only
    systems somebody has visited appear. Thinnest first.
  </p>
  {{if .BeltNote}}<p class="rb-none" style="margin:0">Belt fullness unavailable: {{.BeltNote}}</p>{{end}}
  <table class="rb-ore rb-sortable">
    <thead><tr>
      <th>Region</th><th class="n">Systems worked</th><th class="n">Rock left</th>
    </tr></thead>
    <tbody>
      {{range .Belts}}
      <tr>
        <td>{{.Name}}</td>
        <td class="n" data-v="{{.Systems}}">{{.Systems}}</td>
        <td class="n" data-v="{{.Fullness}}">{{.FullnessPc}}%
          <div class="rb-bar{{if lt .FullnessPc 25}} bare{{else if lt .FullnessPc 60}} thin{{end}}"><i style="width:{{.FullnessPc}}%"></i></div>
        </td>
      </tr>
      {{else}}
      <tr><td colspan="3" class="rb-none">No belt field has been measured yet.</td></tr>
      {{end}}
    </tbody>
  </table>
</div>
{{end}}`
