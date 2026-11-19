package rolebots

// Template for the Economy overview; see especonomy.go for what it reads. There is no funcMap: module pages
// parse with none, so every number is a preformatted string and every bar width an int the handler computed.
// The look is the base panel's Science & Industry page under the module's palette.

// econStyle is the chrome the Economy pages share: the overview, the hub
// front page and the system page all draw KPI tiles, bar tables and column
// charts out of it.
const econStyle = `<style>
.ec { --fa:#6ee7b7; --si:#fca5a5; --tr:#7fb8e6; --dim:#6b7280; --line:#2a1a1a; --card:#170a0a; }
.ec-top { display:flex; align-items:baseline; gap:14px; flex-wrap:wrap; margin:0 0 6px; }
.ec-top h2 { margin:0; }
.ec-top .asof { margin-left:auto; font-size:12px; color:var(--dim); }
.ec-top .asof a { color:#9ca3af; }
.ec-win { display:inline-flex; gap:4px; }
.ec-win a { padding:2px 9px; font-size:11px; border:1px solid var(--line); border-radius:4px;
            color:#9ca3af; text-decoration:none; }
.ec-win a.on { background:#8e2d2d; border-color:#8e2d2d; color:#f0e6dc; }
.ec-lede { font-size:12px; color:#9ca3af; margin:0 0 12px; }
.ec-lede b { color:#e6ded6; }
.ec-kpis { display:grid; grid-template-columns:repeat(8,minmax(0,1fr)); gap:0; background:var(--card);
           border:1px solid var(--line); border-radius:6px; margin-bottom:14px; overflow:hidden; }
.ec-kpi { padding:10px 14px; border-left:1px solid var(--line); border-top:1px solid var(--line); margin:-1px 0 0 -1px; min-width:0; }
.ec-kpi .label { font-size:10px; color:var(--dim); text-transform:uppercase; letter-spacing:1px; }
.ec-kpi .big { font-size:22px; font-weight:600; color:#e6ded6; line-height:1.15; margin-top:2px; font-variant-numeric:tabular-nums; }
.ec-kpi .big.up { color:var(--fa); } .ec-kpi .big.down { color:var(--si); }
.ec-kpi .big small { font-size:11px; color:var(--dim); font-weight:400; }
.ec-kpi .split { font-size:11px; color:#9ca3af; margin-top:3px; }
.ec-grid { display:grid; grid-template-columns:repeat(12,minmax(0,1fr)); gap:12px; margin-bottom:12px; }
.ec-p { background:var(--card); border:1px solid var(--line); border-radius:6px; padding:10px 12px; min-width:0; }
.ec-p h3 { margin:0 0 2px; font-size:13px; color:#f0e6dc; display:flex; align-items:baseline; gap:8px; }
.ec-p h3 .hint { margin-left:auto; font-size:11px; font-weight:400; color:var(--dim); }
.ec-p .note { font-size:11px; color:var(--dim); margin:0 0 8px; }
.c3{grid-column:span 3}.c4{grid-column:span 4}.c5{grid-column:span 5}.c6{grid-column:span 6}.c7{grid-column:span 7}.c8{grid-column:span 8}.c12{grid-column:span 12}
@media (max-width:1500px){ .ec-kpis{grid-template-columns:repeat(4,minmax(0,1fr))} }
@media (max-width:1350px){ .c3,.c4,.c5,.c7,.c8{grid-column:span 6} }
@media (max-width:900px){ .c3,.c4,.c5,.c6,.c7,.c8{grid-column:span 12} .ec-kpis{grid-template-columns:repeat(2,minmax(0,1fr))} }
.ec-p table { width:100%; border-collapse:collapse; }
.ec-p th { padding:3px 7px; font-size:10px; color:var(--dim); text-transform:uppercase; letter-spacing:.05em; text-align:left; font-weight:normal; }
.ec-p td { padding:3px 7px; font-size:12px; white-space:nowrap; border-top:1px solid #1e1414; }
.ec-p td.r, .ec-p th.r { text-align:right; font-variant-numeric:tabular-nums; }
.ec-p td.name { max-width:260px; overflow:hidden; text-overflow:ellipsis; }
.ec-p td.sub, .ec-p .sub { color:var(--dim); font-size:11px; }
.ec-p td.up { color:var(--fa); } .ec-p td.down { color:var(--si); }
.ec-p tr.dim td { color:#4b5563; }
.ec-empty { color:var(--dim); font-size:12px; padding:8px 0; }
.ec-bar { display:block; height:8px; background:#0e0808; border:1px solid var(--line); border-radius:3px; overflow:hidden; min-width:70px; }
.ec-bar i { display:block; height:100%; }
.ec-bar i.fa { background:var(--fa); } .ec-bar i.si { background:var(--si); } .ec-bar i.tr { background:var(--tr); }
.ec-bar i.up { background:var(--fa); } .ec-bar i.down { background:var(--si); }
.ec-cols { display:flex; align-items:flex-end; gap:3px; height:120px; border-bottom:1px solid var(--line); }
.ec-col { flex:1; height:100%; display:flex; flex-direction:row; align-items:flex-end; gap:1px; min-width:0; }
.ec-col > span { flex:1; height:100%; display:flex; flex-direction:column-reverse; }
.ec-col i { display:block; min-height:1px; }
.ec-col .fa { background:var(--fa); } .ec-col .si { background:var(--si); opacity:.8; }
.ec-axis { display:flex; gap:3px; font-size:9px; color:var(--dim); margin-top:3px; }
.ec-axis span { flex:1; text-align:center; min-width:0; overflow:hidden; white-space:nowrap; }
.ec-legend { display:flex; gap:12px; flex-wrap:wrap; font-size:11px; color:#9ca3af; margin-top:6px; }
.ec-legend .sw { display:inline-block; width:9px; height:9px; border-radius:2px; margin-right:4px; vertical-align:-1px; }
.ec-dir { display:inline-block; padding:1px 6px; border-radius:8px; font-size:10px; letter-spacing:.04em; text-transform:uppercase; }
.ec-dir.faucet { background:#10261c; color:var(--fa); } .ec-dir.sink { background:#331a1a; color:var(--si); }
.ec-dir.transfer { background:#132433; color:var(--tr); } .ec-dir.excluded, .ec-dir.unmeasured { background:#241a1a; color:#9ca3af; }
.ec-code { font-family:ui-monospace,Consolas,monospace; font-size:10.5px; color:#9ca3af; white-space:normal; }
.ec-wrap td { white-space:normal; }
</style>`

const tmplEconomy = tradeTabs + `{{define "content"}}
` + tradeTabsStyle + econStyle + `

{{template "rb_tradetabs" .}}
<div class="ec">
<div class="ec-top">
  <h2>Economy</h2>
  <span class="ec-win">{{range .Windows}}<a href="{{.Href}}"{{if .On}} class="on"{{end}}>{{.Label}}</a>{{end}}</span>
  <span class="asof">built {{.AsOf}} &middot; <a href="{{.Refresh}}">refresh</a></span>
</div>
<p class="ec-lede">
  Roster ISK flows over the last <b>{{.Window.Label}}</b>. A <b>faucet</b> creates ISK, a <b>sink</b> destroys it,
  a <b>transfer</b> moves it between wallets.
</p>

{{range .O.Warnings}}<div class="alert alert-error" style="margin-bottom:12px">Panel unavailable &mdash; {{.}}</div>{{end}}
{{if .O.Stats.Partial}}<div class="alert alert-error" style="margin-bottom:12px">
  The ledger started on <b>{{.O.Stats.LedgerSince}}</b>, inside this window, so the window is incomplete.</div>{{end}}

<div class="ec-kpis">
  <div class="ec-kpi"><div class="label">Faucets</div><div class="big up">{{.O.Stats.Faucet}}</div><div class="split">ISK created</div></div>
  <div class="ec-kpi"><div class="label">Sinks</div><div class="big down">{{.O.Stats.Sink}}</div><div class="split">ISK destroyed</div></div>
  <div class="ec-kpi"><div class="label">Net</div><div class="big {{if .O.Stats.NetUp}}up{{else}}down{{end}}">{{.O.Stats.Net}}</div><div class="split">{{if .O.Stats.NetUp}}inflation{{else}}deflation{{end}} over the window</div></div>
  <div class="ec-kpi"><div class="label">Bot &harr; bot</div><div class="big">{{.O.Stats.TransferBot}}</div><div class="split">moved between pilots</div></div>
  <div class="ec-kpi"><div class="label">Bot &harr; corporation</div><div class="big">{{.O.Stats.TransferCorp}}</div><div class="split">levies, wages, shares, dividends</div></div>
  <div class="ec-kpi"><div class="label">Players</div><div class="big">{{.O.Stats.FromPlayers}} <small>in</small></div><div class="split">{{.O.Stats.ToPlayers}} out to players</div></div>
  <div class="ec-kpi"><div class="label">Bot wallets</div><div class="big">{{.O.Stats.BotWallets}}</div><div class="split">{{.O.Stats.BotCount}} pilots, live balance</div></div>
  <div class="ec-kpi"><div class="label">Corporation wallets</div><div class="big">{{.O.Stats.CorpWallets}}</div><div class="split">{{.O.Stats.CorpCount}} bot-run corporations</div></div>
</div>

<div class="ec-grid">
  <div class="ec-p c6">
    <h3>Faucets by line <span class="hint">share of ISK created</span></h3>
    {{if .O.Faucets}}
    <table><thead><tr><th>line</th><th class="r">isk</th><th class="r">rows</th><th class="r">share</th><th style="width:28%"></th></tr></thead><tbody>
    {{range .O.Faucets}}<tr>
      <td class="name" title="{{.Desc}}">{{.Label}}<div class="sub">pilots {{.BotISK}} &middot; corporations {{.CorpISK}}</div></td>
      <td class="r up">{{.In}}</td><td class="r sub">{{.N}}</td><td class="r">{{.Pct}}</td>
      <td><span class="ec-bar"><i class="fa" style="width:{{.Bar}}%"></i></span></td>
    </tr>{{end}}
    </tbody></table>
    {{else}}<div class="ec-empty">Nothing created in this window.</div>{{end}}
  </div>
  <div class="ec-p c6">
    <h3>Sinks by line <span class="hint">share of ISK destroyed</span></h3>
    {{if .O.Sinks}}
    <table><thead><tr><th>line</th><th class="r">isk</th><th class="r">rows</th><th class="r">share</th><th style="width:28%"></th></tr></thead><tbody>
    {{range .O.Sinks}}<tr>
      <td class="name" title="{{.Desc}}">{{.Label}}<div class="sub">pilots {{.BotISK}} &middot; corporations {{.CorpISK}}</div></td>
      <td class="r down">{{.Out}}</td><td class="r sub">{{.N}}</td><td class="r">{{.Pct}}</td>
      <td><span class="ec-bar"><i class="si" style="width:{{.Bar}}%"></i></span></td>
    </tr>{{end}}
    </tbody></table>
    {{else}}<div class="ec-empty">Nothing destroyed in this window.</div>{{end}}
  </div>

  <div class="ec-p c6">
    <h3>Sold to an NPC bid, by goods <span class="hint">{{.O.Goods.SoldTotal}} on the tape</span></h3>
    <p class="note">Pilot market transactions only. Can run ahead of the faucet line.</p>
    {{if .O.Goods.Sold}}
    <table><thead><tr><th>goods</th><th class="r">isk</th><th class="r">units</th><th class="r">share</th><th style="width:24%"></th></tr></thead><tbody>
    {{range .O.Goods.Sold}}<tr>
      <td class="name" title="{{.Groups}}">{{.Label}}<div class="sub">{{.Groups}}</div></td>
      <td class="r up">{{.ISK}}</td><td class="r sub">{{.Units}}</td><td class="r">{{.Pct}}</td>
      <td><span class="ec-bar"><i class="fa" style="width:{{.Bar}}%"></i></span></td>
    </tr>{{end}}
    </tbody></table>
    {{else if .O.Goods.Has}}<div class="ec-empty">Nothing sold to an NPC bid in this window.</div>
    {{else}}<div class="ec-empty">The goods ledger has not loaded yet.</div>{{end}}
  </div>
  <div class="ec-p c6">
    <h3>Bought from an NPC ask, by goods <span class="hint">{{.O.Goods.BoughtTotal}} on the tape</span></h3>
    <p class="note">Includes skillbooks bought from Genolution.</p>
    {{if .O.Goods.Bought}}
    <table><thead><tr><th>goods</th><th class="r">isk</th><th class="r">units</th><th class="r">share</th><th style="width:24%"></th></tr></thead><tbody>
    {{range .O.Goods.Bought}}<tr>
      <td class="name" title="{{.Groups}}">{{.Label}}<div class="sub">{{.Groups}}</div></td>
      <td class="r down">{{.ISK}}</td><td class="r sub">{{.Units}}</td><td class="r">{{.Pct}}</td>
      <td><span class="ec-bar"><i class="si" style="width:{{.Bar}}%"></i></span></td>
    </tr>{{end}}
    </tbody></table>
    {{else if .O.Goods.Has}}<div class="ec-empty">Nothing bought from an NPC ask in this window.</div>
    {{else}}<div class="ec-empty">The goods ledger has not loaded yet.</div>{{end}}
  </div>

  <div class="ec-p c12">
    <h3>Faucets against sinks <span class="hint">per bucket, scaled to the tallest</span></h3>
    {{if .O.Hist}}
    <div class="ec-cols">{{range .O.Hist}}<div class="ec-col" title="{{.Label}}: created {{.Faucet}}, destroyed {{.Sink}}, net {{.Net}}"><span><i class="fa" style="height:{{.FaucetH}}%"></i></span><span><i class="si" style="height:{{.SinkH}}%"></i></span></div>{{end}}</div>
    <div class="ec-axis">{{range .O.Hist}}<span>{{.Label}}</span>{{end}}</div>
    <div class="ec-legend"><span><i class="sw" style="background:var(--fa)"></i>created</span><span><i class="sw" style="background:var(--si)"></i>destroyed</span><span class="sub">hover a bucket for its numbers</span></div>
    {{else}}<div class="ec-empty">No ledger rows yet.</div>{{end}}
  </div>

  <div class="ec-p c7">
    <h3>NPC market by category <span class="hint">what the roster buys from and sells to NPCs</span></h3>
    <p class="note">Sold is a faucet, bought a sink.</p>
    {{if .O.Categories}}
    <table><thead><tr><th>category</th><th class="r">bought</th><th class="r">sold</th><th class="r">net</th><th class="r">units</th><th style="width:22%"></th></tr></thead><tbody>
    {{range .O.Categories}}<tr>
      <td class="name">{{.Name}}</td>
      <td class="r down">{{.Buy}}</td><td class="r up">{{.Sell}}</td>
      <td class="r {{if .NetUp}}up{{else}}down{{end}}">{{.Net}}</td><td class="r sub">{{.Units}}</td>
      <td><span class="ec-bar"><i class="{{if .NetUp}}up{{else}}down{{end}}" style="width:{{.Bar}}%"></i></span></td>
    </tr>{{end}}
    </tbody></table>
    {{else}}<div class="ec-empty">No NPC trades in this window.</div>{{end}}
  </div>
  <div class="ec-p c5">
    <h3>Transfers <span class="hint">gross volume moved</span></h3>
    {{if .O.Transfers}}
    <table><thead><tr><th>line</th><th class="r">in</th><th class="r">out</th><th class="r">rows</th></tr></thead><tbody>
    {{range .O.Transfers}}<tr>
      <td class="name" title="{{.Desc}}">{{.Label}}</td>
      <td class="r">{{.In}}</td><td class="r">{{.Out}}</td><td class="r sub">{{.N}}</td>
    </tr>{{end}}
    </tbody></table>
    {{else}}<div class="ec-empty">Nothing moved in this window.</div>{{end}}
  </div>

  <div class="ec-p c6">
    <h3>Top corporations by net capture <span class="hint">faucets minus sinks, members and treasury</span></h3>
    <p class="note">Pilot flows count toward the corporation they were in at the time.</p>
    {{if .O.Corps}}
    <table><thead><tr><th>corporation</th><th class="r">created</th><th class="r">destroyed</th><th class="r">net</th><th>biggest line</th><th style="width:20%"></th></tr></thead><tbody>
    {{range .O.Corps}}<tr>
      <td class="name"><a href="/rolebots/corp/{{.CorpID}}">{{.Name}}</a></td>
      <td class="r up">{{.In}}</td><td class="r down">{{.Out}}</td>
      <td class="r {{if .Up}}up{{else}}down{{end}}">{{.Net}}</td><td class="sub">{{.TopLine}}</td>
      <td><span class="ec-bar"><i class="{{if .Up}}up{{else}}down{{end}}" style="width:{{.Bar}}%"></i></span></td>
    </tr>{{end}}
    </tbody></table>
    {{else}}<div class="ec-empty">No corporation flows in this window.</div>{{end}}
  </div>
  <div class="ec-p c6">
    <h3>Bot wallets <span class="hint">live balances, not tied to the window</span></h3>
    {{if .O.Wallets.Bands}}
    <p class="note">Bots under {{.O.Wallets.FloorISK}} can't replace ships or pay market fees.</p>
    <div style="display:flex;gap:18px;flex-wrap:wrap;margin-bottom:8px">
      <div><div class="sub">Under the minimum</div><div style="font-size:20px;color:{{if gt .O.Wallets.BelowPct 10.0}}var(--si){{else}}var(--fa){{end}}">{{.O.Wallets.BelowFloor}} <small class="sub">({{.O.Wallets.BelowStr}}%)</small></div></div>
      <div><div class="sub">Median wallet</div><div style="font-size:20px;color:#e6ded6">{{.O.Wallets.MedianISK}}</div></div>
      <div><div class="sub">All bots combined</div><div style="font-size:20px;color:#e6ded6">{{.O.Wallets.TotalISK}}</div></div>
    </div>
    <table><thead><tr><th>balance</th><th class="r">pilots</th><th class="r">share</th><th style="width:34%"></th></tr></thead><tbody>
    {{range .O.Wallets.Bands}}<tr>
      <td class="name"{{if .Poor}} style="color:var(--si)"{{end}}>{{.Label}}</td>
      <td class="r">{{.Count}}</td><td class="r">{{printf "%.1f" .Pct}}%</td>
      <td><span class="ec-bar"><i class="{{if .Poor}}si{{else}}tr{{end}}" style="width:{{printf "%.1f" .Pct}}%"></i></span></td>
    </tr>{{end}}
    </tbody></table>
    {{else}}<div class="ec-empty">No bots on the roster yet.</div>{{end}}
  </div>

  <div class="ec-p c12 ec-wrap">
    <h3>Every touch point <span class="hint">every way a bot wallet moves</span></h3>
    <p class="note">Dimmed lines had no rows this window. Unclassified above zero needs a new line.</p>
    <table><thead><tr><th>line</th><th>direction</th><th>other side</th><th>refType</th><th>written by</th><th class="r">in</th><th class="r">out</th><th class="r">rows</th></tr></thead><tbody>
    {{range .O.Catalogue}}<tr{{if not .Has}} class="dim"{{end}}>
      <td class="name" title="{{.Desc}}"><b>{{.Label}}</b>{{if .Desc}}<div class="sub">{{.Desc}}</div>{{end}}</td>
      <td><span class="ec-dir {{.Dir}}">{{.Dir}}</span></td>
      <td class="sub">{{.Counter}}</td>
      <td class="sub">{{.RefTypes}}</td>
      <td class="ec-code">{{.Code}}</td>
      <td class="r">{{.In}}</td><td class="r">{{.Out}}</td><td class="r sub">{{.N}}</td>
    </tr>{{end}}
    </tbody></table>
  </div>
</div>
</div>
{{end}}`
