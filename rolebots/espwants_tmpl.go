package rolebots

// Templates for the wants list and the single-want page. Split from
// espwants.go the way the rest of the module splits: the handler is a set
// of queries and the page is a page.

const tmplWants = tradeTabs + `{{define "content"}}
` + tradeTabsStyle + `
<style>
.rb-wants { width:100%; font-size:12px; }
.rb-wants th { font-size:10px; letter-spacing:.05em; text-transform:uppercase; color:#6b7280;
               font-weight:normal; white-space:nowrap; }
.rb-wants th.grp { color:#9ca3af; border-bottom:1px solid #2a1a1a; text-align:center;
                   padding-bottom:3px; }
.rb-wants td, .rb-wants th { padding:4px 7px; }
.rb-wants tbody tr { border-bottom:1px solid #1e1414; }
.rb-wants tbody tr:hover { background:#1a1010; }
.rb-wants td.n, .rb-wants th.n { text-align:right; font-variant-numeric:tabular-nums; }
.rb-wants td.dim { color:#6b7280; }
.rb-wants td.zero { color:#4b5563; }
.rb-wants a { color:#e0a94a; text-decoration:none; }
.rb-wants a:hover { text-decoration:underline; }
.rb-wants tr.open td.rk { border-left:3px solid #e0a94a; }
.rb-wants tr.served td.rk { border-left:3px solid #22c55e; }
.rb-wants td.rk { color:#6b7280; }
.rb-wants td.where { color:#9ca3af; max-width:230px; overflow:hidden; text-overflow:ellipsis;
                     white-space:nowrap; }
.rb-cat { display:inline-block; padding:1px 6px; border-radius:4px; font-size:10px;
          text-transform:uppercase; letter-spacing:.04em; background:#241a1a; color:#9ca3af; }
.rb-cat[data-c="ship"]   { background:#132433; color:#7fb8e6; }
.rb-cat[data-c="module"] { background:#231a2c; color:#c4a5f5; }
.rb-cat[data-c="charge"] { background:#26210f; color:#e0c068; }
.rb-cat[data-c="drone"]  { background:#10261c; color:#6ee7b7; }
.rb-tot { display:grid; grid-template-columns:repeat(auto-fit,minmax(130px,1fr)); gap:10px; }
.rb-tot div { border:1px solid #2a1a1a; border-radius:6px; padding:8px 10px; background:#180f0f; }
.rb-tot .k { font-size:10px; letter-spacing:.05em; text-transform:uppercase; color:#6b7280; }
.rb-tot .v { font-size:17px; color:#e6ded6; font-variant-numeric:tabular-nums; }
.rb-tot .v.open { color:#e0a94a; } .rb-tot .v.served { color:#6ee7b7; }
.rb-scroll { overflow-x:auto; }
</style>

{{template "rb_tradetabs" .}}
<h2>Wants List</h2>

<div class="card">
  <p style="margin:0 0 10px;color:#9ca3af;font-size:12px">
    Every <b>buildable</b> item, ranked by how many roster pilots have the skills to use it.
    Hulls weigh {{.HullWeight}} and ammunition {{.ChargeWeight}} against a module. <b>Units</b> is demand:
    one per hull or module, {{.Rounds}} per ammo type, 5 per drone.
  </p>
  <p style="margin:0 0 10px;color:#9ca3af;font-size:12px">
    The right half is the roster's answer. <b>BPOs</b> is industrialists holding the blueprint.
    <b>Suppliers</b> keep it as a standing shelf line. Order columns split the live book into
    <b>bot</b>, <b>player</b> and <b>NPC</b>.
  </p>
  <div class="rb-tot">
    <div><div class="k">types on the census</div><div class="v">{{.Sum.Types}}</div></div>
    <div><div class="k">answered</div><div class="v served">{{.Sum.Served}}</div></div>
    <div><div class="k">untouched</div><div class="v open">{{.Sum.Openings}}</div></div>
    <div><div class="k">industrialists</div><div class="v">{{.Sum.Suppliers}}</div></div>
    <div><div class="k">shelf lines</div><div class="v">{{.Sum.Shelves}}</div></div>
    <div><div class="k">blueprints held</div><div class="v">{{.Sum.Blueprints}}</div></div>
  </div>
  <p style="margin:10px 0 0;font-size:12px;color:#6b7280">Census retaken {{.Sum.Swept}}.</p>
</div>

<div class="card">
  <form method="GET" action="/rolebots/wants"
        style="display:flex;gap:10px;align-items:end;flex-wrap:wrap;margin-bottom:10px">
    <label>Category
      <select name="cat">
        <option value="0">everything</option>
        {{$c := .Cat}}
        {{range .Cats}}<option value="{{.ID}}"{{if eq .ID $c}} selected{{end}}>{{.Label}}</option>{{end}}
      </select>
    </label>
    <label>Item <input type="text" name="q" value="{{.Text}}" placeholder="name contains"></label>
    <label>Rows <input type="number" name="limit" value="{{.Limit}}" min="10" max="1000" style="width:80px"></label>
    <label style="display:flex;gap:5px;align-items:center">
      <input type="checkbox" name="open" value="1"{{if .Openings}} checked{{end}}> openings only
    </label>
    <button type="submit">Filter</button>
    <a href="/rolebots/wants" style="font-size:12px;color:#9ca3af">clear</a>
  </form>

  {{if not .Rows}}
    <p style="color:#9ca3af;font-size:12px">
      Nothing on the roll. The census runs every fifteen minutes.
    </p>
  {{else}}
  <div class="rb-scroll">
  <table class="rb-wants">
    <thead>
      <tr>
        <th colspan="3"></th>
        <th class="grp" colspan="4">the roster wants</th>
        <th class="grp" colspan="5">the roster's answer</th>
        <th class="grp" colspan="5">live orders</th>
      </tr>
      <tr>
        <th class="n">#</th><th>item</th><th></th>
        <th class="n" title="trained to use it">capable</th><th class="n" title="fits, loads or flies it; the ranking">users</th><th class="n">pull</th><th class="n">units</th>
        <th class="n">bpos</th><th class="n">suppliers</th><th class="n">stations</th>
        <th class="n">built</th><th>where</th>
        <th class="n">bot ask</th><th class="n">player ask</th><th class="n">npc ask</th>
        <th class="n">bot bid</th><th class="n">player bid</th>
      </tr>
    </thead>
    <tbody>
      {{range .Rows}}
      <tr class="{{if .Served}}served{{else}}open{{end}}">
        <td class="n rk">{{.Rank}}</td>
        <td><a href="/rolebots/want/{{.TypeID}}">{{.TypeName}}</a></td>
        <td><span class="rb-cat" data-c="{{.Category}}">{{.Category}}</span></td>
        <td class="n">{{.Capable}}</td>
        <td class="n">{{.Users}}</td>
        <td class="n dim">{{.Pull}}</td>
        <td class="n">{{.Qty}}</td>
        <td class="n {{if not .BPOwners}}zero{{end}}">{{.BPOwners}}</td>
        <td class="n {{if not .Suppliers}}zero{{end}}">{{.Suppliers}}</td>
        <td class="n {{if not .Stations}}zero{{end}}">{{.Stations}}</td>
        <td class="n {{if not .Built}}zero{{end}}">{{.Built}}</td>
        <td class="where" title="{{.Where}}">{{if .Where}}{{.Where}}{{else}}&mdash;{{end}}</td>
        <td class="n {{if not .BotAsks}}zero{{end}}">{{.BotAsks}}</td>
        <td class="n {{if not .PlayerAsks}}zero{{end}}">{{.PlayerAsks}}</td>
        <td class="n {{if not .NPCAsks}}zero{{end}}">{{.NPCAsks}}</td>
        <td class="n {{if not .BotBids}}zero{{end}}">{{.BotBids}}</td>
        <td class="n {{if not .PlayerBids}}zero{{end}}">{{.PlayerBids}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  </div>
  {{end}}
</div>
{{end}}`

const tmplWant = tradeTabs + `{{define "content"}}
` + tradeTabsStyle + `
<style>
.rb-w { width:100%; font-size:12px; }
.rb-w th { font-size:10px; letter-spacing:.05em; text-transform:uppercase; color:#6b7280;
           font-weight:normal; text-align:left; padding:4px 7px; }
.rb-w td { padding:4px 7px; border-bottom:1px solid #1e1414; }
.rb-w td.n, .rb-w th.n { text-align:right; font-variant-numeric:tabular-nums; }
.rb-w td.dim { color:#9ca3af; }
.rb-w a { color:#e0a94a; text-decoration:none; }
.rb-w a:hover { text-decoration:underline; }
.rb-side { display:inline-block; width:34px; text-align:center; padding:1px 0; border-radius:4px;
           font-size:10px; text-transform:uppercase; }
.rb-side[data-s="ask"] { background:#26210f; color:#e0c068; }
.rb-side[data-s="bid"] { background:#132433; color:#7fb8e6; }
.rb-who { display:inline-block; padding:1px 6px; border-radius:4px; font-size:10px;
          text-transform:uppercase; letter-spacing:.04em; }
.rb-who[data-w="bot"]    { background:#10261c; color:#6ee7b7; }
.rb-who[data-w="player"] { background:#231a2c; color:#c4a5f5; }
.rb-who[data-w="npc"]    { background:#241a1a; color:#9ca3af; }
.rb-cat { display:inline-block; padding:1px 6px; border-radius:4px; font-size:10px;
          text-transform:uppercase; letter-spacing:.04em; background:#241a1a; color:#9ca3af; }
.rb-tot { display:grid; grid-template-columns:repeat(auto-fit,minmax(130px,1fr)); gap:10px; }
.rb-tot div { border:1px solid #2a1a1a; border-radius:6px; padding:8px 10px; background:#180f0f; }
.rb-tot .k { font-size:10px; letter-spacing:.05em; text-transform:uppercase; color:#6b7280; }
.rb-tot .v { font-size:17px; color:#e6ded6; font-variant-numeric:tabular-nums; }
.rb-crumb { font-size:12px; color:#6b7280; margin:0 0 8px; }
.rb-crumb a { color:#9ca3af; }
.rb-scroll { overflow-x:auto; max-height:520px; }
</style>

{{template "rb_tradetabs" .}}
<p class="rb-crumb"><a href="/rolebots/wants">&larr; Wants List</a></p>
<h2>{{.W.TypeName}} <span class="rb-cat">{{.W.Category}}</span></h2>

<div class="card">
  <div class="rb-tot">
    <div><div class="k">pilots who could use it</div><div class="v">{{.W.Capable}}</div></div>
    <div><div class="k">pilots who fit, load or fly it</div><div class="v">{{.W.Users}}</div></div>
    <div><div class="k">units that adds up to</div><div class="v">{{.W.Qty}}</div></div>
    <div><div class="k">pull</div><div class="v">{{.W.Pull}}</div></div>
    <div><div class="k">meta level</div><div class="v">{{.W.MetaLevel}}</div></div>
    <div><div class="k">blueprint type</div><div class="v">{{.W.BPTypeID}}</div></div>
  </div>
</div>

<div class="card">
  <h3>Standing shelf lines</h3>
  <p style="margin:0 0 8px;color:#9ca3af;font-size:12px">
    Industrialists keeping this in stock at a station. Pinned lines were set by an operator and are not pruned.
  </p>
  {{if not .Suppliers}}
    <p style="color:#6b7280;font-size:12px">Nobody carries this. It is an opening.</p>
  {{else}}
  <table class="rb-w">
    <thead><tr><th>pilot</th><th>station</th><th>system</th>
      <th class="n">target</th><th class="n">built</th><th class="n">sold</th>
      <th>source</th><th>last built</th></tr></thead>
    <tbody>
      {{range .Suppliers}}
      <tr>
        <td><a href="/rolebot/{{.CharID}}">{{.Name}}</a></td>
        <td>{{.Station}}</td>
        <td class="dim">{{.System}}</td>
        <td class="n">{{.Target}}</td><td class="n">{{.Built}}</td><td class="n">{{.Sold}}</td>
        <td class="dim">{{.Source}}</td>
        <td class="dim">{{.LastBuilt}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  {{end}}
</div>

<div class="card">
  <h3>Blueprint holders</h3>
  <p style="margin:0 0 8px;color:#9ca3af;font-size:12px">
    Rolebots holding a blueprint for this product. One in transit shows as such.
  </p>
  {{if not .Holders}}
    <p style="color:#6b7280;font-size:12px">No rolebot owns the blueprint.</p>
  {{else}}
  <table class="rb-w">
    <thead><tr><th>pilot</th><th>where</th></tr></thead>
    <tbody>
      {{range .Holders}}
      <tr><td><a href="/rolebot/{{.CharID}}">{{.Name}}</a></td><td>{{.Where}}</td></tr>
      {{end}}
    </tbody>
  </table>
  {{end}}
</div>

<div class="card">
  <h3>Live orders</h3>
  {{if not .Orders}}
    <p style="color:#6b7280;font-size:12px">Nothing on the market for this anywhere.</p>
  {{else}}
  <div class="rb-scroll">
  <table class="rb-w">
    <thead><tr><th>side</th><th>who</th><th>station</th><th>system</th><th>region</th>
      <th class="n">price</th><th class="n">units</th></tr></thead>
    <tbody>
      {{range .Orders}}
      <tr>
        <td><span class="rb-side" data-s="{{.Side}}">{{.Side}}</span></td>
        <td>
          {{if .IsNPC}}<span class="rb-who" data-w="npc">npc</span> {{.Who}}
          {{else if .IsBot}}<span class="rb-who" data-w="bot">bot</span> <a href="/rolebot/{{.CharID}}">{{.Who}}</a>
          {{else}}<span class="rb-who" data-w="player">player</span> {{.Who}}{{end}}
        </td>
        <td>{{.Station}}</td>
        <td class="dim">{{.System}}</td>
        <td class="dim">{{.Region}}</td>
        <td class="n">{{.Price}}</td>
        <td class="n">{{.Units}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  </div>
  {{end}}
</div>

<div class="card">
  <h3>Bot-to-bot trades in this</h3>
  {{if not .Peers}}
    <p style="color:#6b7280;font-size:12px">
      No rolebot has bought this off another rolebot.
    </p>
  {{else}}
  <div class="rb-scroll">
  <table class="rb-w">
    <thead><tr><th>time</th><th>buyer</th><th>seller</th><th class="n">qty</th>
      <th class="n">unit</th><th class="n">value</th><th>station</th><th>why</th></tr></thead>
    <tbody>
      {{range .Peers}}
      <tr>
        <td class="dim" title="{{.Full}}">{{.When}}</td>
        <td><a href="/rolebot/{{.BuyerID}}">{{.Buyer}}</a></td>
        <td><a href="/rolebot/{{.SellerID}}">{{.Seller}}</a></td>
        <td class="n">{{.Qty}}</td><td class="n">{{.Price}}</td><td class="n">{{.Value}}</td>
        <td>{{.Station}}{{if .System}} <span class="dim">&middot; {{.System}}</span>{{end}}</td>
        <td class="dim">{{.Purpose}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  </div>
  {{end}}
</div>
{{end}}`
