package rolebots

// The Stats board's markup, split from its handler as espminerals.go and espminerals_tmpl.go are. It
// follows the module page rules stated at listKit (esptrade_tmpl.go): listKit goes inside
// {{define "content"}}.
const tmplStats = `{{define "content"}}
` + listKit + `
<style>
.rb-pods { display:grid; grid-template-columns:repeat(auto-fit,minmax(300px,1fr));
           gap:12px; align-items:start; margin-bottom:12px; }
.rb-pod { background:#170a0a; border:1px solid #2a1a1a; border-radius:6px; padding:10px 12px; }
.rb-pod h3 { margin:0; font-size:13px; color:#f0e6dc; }
.rb-pod .note { font-size:10.5px; color:#6b7280; margin:1px 0 8px; }
.rb-pod ol { list-style:none; margin:0; padding:0; }
.rb-pod li { display:flex; gap:8px; align-items:center; padding:3px 0; }
.rb-pod li + li { border-top:1px solid #1e1212; }
.rb-pod img { width:32px; height:32px; border-radius:3px; background:#100a0a; }
.rb-pod .who { flex:1; min-width:0; }
.rb-pod .who a { font-size:12px; }
.rb-pod .who div { font-size:10px; color:#6b7280; overflow:hidden; text-overflow:ellipsis;
                   white-space:nowrap; }
.rb-pod .val { font-size:13px; color:#f0e6dc; font-variant-numeric:tabular-nums; }
.rb-pip { width:16px; text-align:center; font-size:11px; font-weight:bold; }
.rb-pip.p1 { color:#eab308; } .rb-pip.p2 { color:#cbd5e1; } .rb-pip.p3 { color:#b45309; }
.rb-group { margin:16px 0 8px; font-size:14px; color:#f0e6dc; }
.rb-none { color:#4b5563; }
.rb-num { font-variant-numeric:tabular-nums; }
#rbStats td.n, #rbStats th.n { text-align:right; }
#rbStats td.pos { color:#22c55e; } #rbStats td.neg { color:#ef4444; }
</style>
<h2>Stats</h2>
{{if .Msg}}<div class="alert">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

{{if .Rows}}
{{range .Groups}}
<h3 class="rb-group">{{.Title}}</h3>
<div class="rb-pods">
  {{range .Podiums}}
  <div class="rb-pod">
    <h3>{{.Title}}</h3>
    <p class="note">{{.Note}}</p>
    {{if .Empty}}
      <p class="rb-none" style="font-size:11.5px;margin:0">nobody yet</p>
    {{else}}
    <ol>
      {{range .Rows}}
      <li>
        <span class="rb-pip p{{.Rank}}">{{.Rank}}</span>
        <img src="/portrait/{{.CharID}}?size=64" alt="" onerror="this.style.visibility='hidden'">
        <span class="who">
          <a href="/rolebot/{{.CharID}}">{{.Name}}</a>
          <div>{{.Corp}}</div>
        </span>
        <span class="val">{{.Value}}</span>
      </li>
      {{end}}
    </ol>
    {{end}}
  </div>
  {{end}}
</div>
{{end}}
{{end}}

<div class="card">
  <p style="font-size:12px;color:#9ca3af;margin:0 0 6px">
    Skill points, wallet, bounties, ships built, blueprints and kills come straight from the
    game's own records and cover the whole life of the server.
    <strong>Ore mined, jumps, docks and the trade columns only count from the update that added
    them</strong>, because nothing earlier was recorded. The trade columns were filled once from the
    last 30 days of the <a href="/rolebots/trades">Trade Feed</a>, as far back as it keeps.
  </p>
  <p style="font-size:12px;color:#6b7280;margin:0">
    {{.Count}} pilot{{if ne .Count 1}}s{{end}} &middot;
    {{if .HideGen}}
      generated pilots hidden &middot; <a href="/rolebots/stats">show them</a>
    {{else}}
      generated pilots included &middot; <a href="/rolebots/stats?gen=0">hide them</a>
    {{end}}
    &middot; updated {{.AgeSecs}}s ago &middot; <a href="/rolebots/stats?fresh=1">refresh now</a>
    <br>Skill points, bounties and blueprints are recounted every 10 minutes (last {{.SlowAge}}s ago); a refresh recounts them straight away and takes longer.
  </p>
</div>

<div class="card rb-panel">
  {{if .Rows}}
  <div class="rb-scroll">
  <table id="rbStats">
    <thead>
    <tr>
      <th>Pilot</th><th>Corp</th>
      <th class="n">SP</th><th class="n">Wallet</th>
      <th class="n">Bounty</th><th class="n">Payouts</th>
      <th class="n">Ore m&sup3;</th><th class="n">Ore units</th>
      <th class="n">Trades</th><th class="n">Turnover</th><th class="n">Profit</th>
      <th class="n">Best trade</th><th class="n">Orders</th><th class="n">Fees</th>
      <th class="n" title="last 60 days">Jobs</th><th class="n" title="last 60 days">Built</th><th class="n">BPOs</th>
      <th class="n">Jumps</th><th class="n">Docks</th>
      <th class="n">Kills</th><th class="n">Losses</th><th class="n">ISK destroyed</th>
      <th class="n">Solo</th><th class="n">Blows</th><th class="n">Eff</th>
      <th class="n">Sec</th><th class="n">Corp moves</th>
    </tr>
    </thead>
    <tbody>
    {{range .Rows}}
    <tr>
      <td><a href="/rolebot/{{.CharID}}">{{.Name}}</a></td>
      <td>{{.CorpName}}</td>
      <td class="n rb-num" data-v="{{.SP}}">{{.SPShort}}</td>
      <td class="n rb-num" data-v="{{.Balance}}">{{.WalletISK}}</td>
      <td class="n rb-num" data-v="{{.Bounty}}">{{.BountyISK}}</td>
      <td class="n rb-num" data-v="{{.BountyPayouts}}" title="bounty payments received — one per fight, not one per rat">{{.BountyPayouts}}</td>
      <td class="n rb-num" data-v="{{.OreM3}}">{{.OreM3Short}}</td>
      <td class="n rb-num" data-v="{{.OreUnits}}">{{.OreUnits}}</td>
      <td class="n rb-num" data-v="{{.Trades}}" title="{{.TradeSplit}}">{{.Trades}}</td>
      <td class="n rb-num" data-v="{{.Turnover}}">{{.TurnoverISK}}</td>
      <td class="n rb-num{{if gt .Profit 0.0}} pos{{else if lt .Profit 0.0}} neg{{end}}" data-v="{{.Profit}}">{{.ProfitISK}}</td>
      <td class="n rb-num" data-v="{{.BestTrade}}">{{.BestTradeISK}}</td>
      <td class="n rb-num" data-v="{{.Orders}}">{{.Orders}}</td>
      <td class="n rb-num" data-v="{{.Fees}}">{{.FeesISK}}</td>
      <td class="n rb-num" data-v="{{.Built}}">{{.Built}}</td>
      <td class="n rb-num" data-v="{{.BuiltRuns}}">{{.BuiltRuns}}</td>
      <td class="n rb-num" data-v="{{.BPOs}}">{{.BPOs}}</td>
      <td class="n rb-num" data-v="{{.Jumps}}">{{.Jumps}}</td>
      <td class="n rb-num" data-v="{{.Docks}}">{{.Docks}}</td>
      <td class="n rb-num" data-v="{{.Kills}}">{{.Kills}}</td>
      <td class="n rb-num" data-v="{{.Losses}}">{{.Losses}}</td>
      <td class="n rb-num" data-v="{{.ISKDestroyed}}">{{.DestroyedISK}}</td>
      <td class="n rb-num" data-v="{{.Solo}}">{{.Solo}}</td>
      <td class="n rb-num" data-v="{{.FinalBlows}}">{{.FinalBlows}}</td>
      <td class="n rb-num" data-v="{{.Eff}}">{{.EffStr}}%</td>
      <td class="n rb-num" data-v="{{.Sec}}">{{.SecStr}}</td>
      <td class="n rb-num" data-v="{{.CorpMoves}}">{{.CorpMoves}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{else}}
  <p style="color:#6b7280">
    No characters on the roster yet &mdash; add one on the <a href="/rolebots">Roster</a> page,
    or generate a population on <a href="/rolebots/seed">Bot Population</a>.
  </p>
  {{end}}
</div>
<script>
rbSortable(document.getElementById('rbStats'));
</script>
{{end}}`
