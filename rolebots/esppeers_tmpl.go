package rolebots

// Template for the bot-to-bot ledger. See esppeers.go for what it reads.

// peerRowStyle is the ledger row's chrome, shared with the system page.
const peerRowStyle = `<style>
.rb-p { display:grid;
        grid-template-columns:64px minmax(0,1fr) 14px minmax(0,1fr) minmax(0,1.2fr)
                              52px 68px 72px minmax(0,1.3fr) 84px;
        gap:8px; align-items:baseline; padding:5px 8px; border-bottom:1px solid #1e1414;
        font-size:12px; }
.rb-p:hover { background:#1a1010; }
.rb-p .t { color:#6b7280; font-family:ui-monospace,Consolas,monospace; font-size:11px; }
.rb-p .who { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.rb-p .who a { color:#e0a94a; text-decoration:none; }
.rb-p .arrow { color:#6b7280; text-align:center; }
.rb-p .what { color:#e6ded6; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.rb-p .n { text-align:right; font-variant-numeric:tabular-nums; color:#c9c1ba; }
.rb-p .where { color:#9ca3af; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.rb-p .where .sys { color:#6b7280; }
.rb-p.rb-new { animation:rbflash 1.6s ease-out; }
@keyframes rbflash { from { background:#123a25; } to { background:transparent; } }
.rb-why { display:inline-block; width:100%; text-align:center; padding:1px 0; border-radius:4px;
          font-size:10px; letter-spacing:.04em; text-transform:uppercase;
          background:#241a1a; color:#9ca3af; }
.rb-why[data-p="blueprint"]    { background:#231a2c; color:#c4a5f5; }
.rb-why[data-p="manufactured"] { background:#10261c; color:#6ee7b7; }
.rb-why[data-p="fitting"]      { background:#132433; color:#7fb8e6; }
.rb-why[data-p="hull"]         { background:#132433; color:#9ecbf0; }
.rb-why[data-p="materials"]    { background:#26210f; color:#e0c068; }
.rb-why[data-p="minerals"]     { background:#26210f; color:#e0c068; }
.rb-why[data-p="ore"]          { background:#26210f; color:#d9b26a; }
.rb-why[data-p="haul"]         { background:#1c3320; color:#8fe0a0; }
.rb-why[data-p="loot"]         { background:#331a1a; color:#e08a8a; }
.rb-head { color:#6b7280; font-size:10px; letter-spacing:.05em; text-transform:uppercase;
           border-bottom:1px solid #2a1a1a; }
.rb-pager { display:flex; gap:14px; align-items:center; margin-top:10px; font-size:12px; color:#6b7280; }
.rb-pager a { color:#e0a94a; text-decoration:none; }
.rb-pager a:hover { text-decoration:underline; }
</style>`

const tmplPeers = tradeTabs + `{{define "content"}}
` + tradeTabsStyle + peerRowStyle + `
<style>
.rb-live { display:flex; align-items:center; gap:10px; font-size:12px; color:#9ca3af; flex-wrap:wrap; }
.rb-dot { width:8px; height:8px; border-radius:50%; background:#22c55e; display:inline-block; }
.rb-dot.off { background:#6b7280; }

.rb-tot { display:grid; grid-template-columns:repeat(auto-fit,minmax(130px,1fr)); gap:10px; }
.rb-tot div { border:1px solid #2a1a1a; border-radius:6px; padding:8px 10px; background:#180f0f; }
.rb-tot .k { font-size:10px; letter-spacing:.05em; text-transform:uppercase; color:#6b7280; }
.rb-tot .v { font-size:17px; color:#e6ded6; font-variant-numeric:tabular-nums; }

.rb-cols { display:grid; grid-template-columns:repeat(auto-fit,minmax(280px,1fr)); gap:14px; }
.rb-mini { width:100%; font-size:12px; }
.rb-mini th { font-size:10px; letter-spacing:.05em; text-transform:uppercase; color:#6b7280;
              font-weight:normal; text-align:left; padding:3px 6px; }
.rb-mini td { padding:3px 6px; border-bottom:1px solid #1e1414; }
.rb-mini td.n, .rb-mini th.n { text-align:right; font-variant-numeric:tabular-nums; }
.rb-mini td.dim { color:#9ca3af; }
.rb-mini a { color:#e0a94a; text-decoration:none; }
.rb-mini a:hover { text-decoration:underline; }
.rb-feed { max-height:calc(100vh - 420px); overflow-y:auto; border-top:1px solid #2a1a1a; }
</style>

{{template "rb_tradetabs" .}}
<h2>Bot&#8202;&rarr;&#8202;Bot Trades</h2>

<div class="card">
  <p style="margin:0 0 10px;color:#9ca3af;font-size:12px">
    Trades where both sides were rolebots. Trades with a <b>player</b> are on the
    <a href="/rolebots/trades" style="color:#e0a94a">Feed</a>. Kept {{.Retention}} days. Figures above the ledger cover the last week.
  </p>
  <div class="rb-tot">
    <div><div class="k">trades (24h)</div><div class="v">{{.Summary.Totals.Day}}</div></div>
    <div><div class="k">isk (24h)</div><div class="v">{{.Summary.Totals.DayISK}}</div></div>
    <div><div class="k">trades (7d)</div><div class="v">{{.Summary.Totals.Week}}</div></div>
    <div><div class="k">isk (7d)</div><div class="v">{{.Summary.Totals.WeekISK}}</div></div>
    <div><div class="k">trading pairs (7d)</div><div class="v">{{.Summary.Totals.Pairs}}</div></div>
    <div><div class="k">stations used (7d)</div><div class="v">{{.Summary.Totals.Stations}}</div></div>
  </div>
</div>

{{if .Summary.Pairs}}
<div class="card">
  <div class="rb-cols">
    <div>
      <h3 style="margin-top:0">Who trades with whom <span style="font-size:11px;color:#6b7280;font-weight:normal">7 days</span></h3>
      <table class="rb-mini">
        <thead><tr><th>seller</th><th>buyer</th><th class="n">trades</th><th class="n">isk</th></tr></thead>
        <tbody>
          {{range .Summary.Pairs}}
          <tr>
            <td><a href="/rolebot/{{.SellerID}}">{{.Seller}}</a></td>
            <td><a href="/rolebot/{{.BuyerID}}">{{.Buyer}}</a></td>
            <td class="n">{{.Trades}}</td><td class="n">{{.Value}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </div>
    <div>
      <h3 style="margin-top:0">Where they meet</h3>
      <table class="rb-mini">
        <thead><tr><th>station</th><th>system</th><th class="n">trades</th><th class="n">isk</th></tr></thead>
        <tbody>
          {{range .Summary.Places}}
          <tr>
            <td>{{.Station}}</td>
            <td class="dim">{{.System}}{{if .Region}} <span style="color:#4b5563">&middot; {{.Region}}</span>{{end}}</td>
            <td class="n">{{.Trades}}</td><td class="n">{{.Value}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </div>
    <div>
      <h3 style="margin-top:0">What changes hands</h3>
      <table class="rb-mini">
        <thead><tr><th>item</th><th class="n">trades</th><th class="n">units</th><th class="n">isk</th></tr></thead>
        <tbody>
          {{range .Summary.Goods}}
          <tr>
            <td><a href="/rolebots/want/{{.TypeID}}">{{.TypeName}}</a></td>
            <td class="n">{{.Trades}}</td><td class="n">{{.Units}}</td><td class="n">{{.Value}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </div>
  </div>
</div>
{{end}}

<div class="card">
  <form method="GET" action="/rolebots/peers"
        style="display:flex;gap:10px;align-items:end;flex-wrap:wrap;margin-bottom:10px">
    <label>Pilot (either side) <input type="text" name="who" value="{{.Who}}" placeholder="pilot name" autocomplete="off"></label>
    <label>Why
      <select name="purpose">
        <option value="">any reason</option>
        {{$p := .PurposeSel}}
        {{range .Purposes}}<option value="{{.}}"{{if eq . $p}} selected{{end}}>{{.}}</option>{{end}}
      </select>
    </label>
    <label>Text <input type="text" name="q" value="{{.Text}}" placeholder="item or station"></label>
    <button type="submit">Filter</button>
    <a href="/rolebots/peers" style="font-size:12px;color:#9ca3af">clear</a>
    <div class="rb-live" style="margin-left:auto">
      <label><input type="checkbox" id="rbLive"> <span class="rb-dot off" id="rbDot"></span> live</label>
      <span id="rbNote">paused &middot; tick to poll every 15s while this tab is visible</span>
    </div>
  </form>

  {{if not .Rows}}
    <p style="color:#9ca3af;font-size:12px">
      {{if .Paged}}Nothing further this way.{{else}}No bot has traded with another bot yet. See the <a href="/rolebots/wants" style="color:#e0a94a">Wants List</a>.{{end}}
    </p>
  {{else}}
  <div class="rb-p rb-head">
    <span>time</span><span>buyer</span><span></span><span>seller</span><span>item</span>
    <span style="text-align:right">qty</span><span style="text-align:right">unit</span>
    <span style="text-align:right">value</span><span>station</span><span>why</span>
  </div>
  <div class="rb-feed" id="rbFeed">
    {{range .Rows}}
    <div class="rb-p" data-id="{{.ID}}">
      <span class="t" title="{{.Full}}">{{.When}}</span>
      <span class="who"><a href="/rolebot/{{.BuyerID}}">{{.Buyer}}</a></span>
      <span class="arrow" title="{{.TakerSide}}">&larr;</span>
      <span class="who"><a href="/rolebot/{{.SellerID}}">{{.Seller}}</a></span>
      <span class="what"><a href="/rolebots/want/{{.TypeID}}" style="color:#e6ded6;text-decoration:none">{{.TypeName}}</a></span>
      <span class="n">{{.Qty}}</span>
      <span class="n">{{.Price}}</span>
      <span class="n">{{.Value}}</span>
      <span class="where" title="{{.Station}}{{if .Region}} — {{.Region}}{{end}}">
        {{.Station}}{{if .System}} <span class="sys">&middot; {{.System}}</span>{{end}}</span>
      <span><span class="rb-why" data-p="{{.Purpose}}">{{.Purpose}}</span></span>
    </div>
    {{end}}
  </div>
  {{end}}
  <div class="rb-pager">
    {{if .Newer}}<a href="{{.Newer}}">&larr; newer</a>{{end}}
    {{if .Older}}<a href="{{.Older}}">older &rarr;</a>{{end}}
    {{if .Paged}}<a href="/rolebots/peers{{if .Query}}?{{.Query}}{{end}}">latest</a>{{end}}
  </div>
</div>

<script>
(function () {
  var feed = document.getElementById('rbFeed'), live = document.getElementById('rbLive');
  var dot = document.getElementById('rbDot'), note = document.getElementById('rbNote');
  if (!live) return;
  var query = {{.Query}};
  var paged = {{.Paged}};
  var lastID = 0;
  var first = feed ? feed.querySelector('.rb-p[data-id]') : null;
  if (first) lastID = +first.getAttribute('data-id');
  var KEY = 'rb.peers.live';
  try { if (!paged && localStorage.getItem(KEY) === '1') live.checked = true; } catch (e) {}

  function esc(s) { return (s || '').replace(/[&<>"]/g, function (c) {
    return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }

  function addRow(r) {
    if (!feed || feed.querySelector('.rb-p[data-id="' + r.id + '"]')) return;
    var el = document.createElement('div');
    el.className = 'rb-p rb-new';
    el.setAttribute('data-id', r.id);
    el.innerHTML =
      '<span class="t" title="' + esc(r.full) + '">' + esc(r.when) + '</span>' +
      '<span class="who"><a href="/rolebot/' + r.buyerID + '">' + esc(r.buyer) + '</a></span>' +
      '<span class="arrow">&larr;</span>' +
      '<span class="who"><a href="/rolebot/' + r.sellerID + '">' + esc(r.seller) + '</a></span>' +
      '<span class="what">' + esc(r.typeName) + '</span>' +
      '<span class="n">' + r.qty + '</span>' +
      '<span class="n">' + esc(r.price) + '</span>' +
      '<span class="n">' + esc(r.value) + '</span>' +
      '<span class="where">' + esc(r.station) +
        (r.system ? ' <span class="sys">&middot; ' + esc(r.system) + '</span>' : '') + '</span>' +
      '<span><span class="rb-why" data-p="' + esc(r.purpose) + '">' + esc(r.purpose) + '</span></span>';
    feed.insertBefore(el, feed.firstChild);
    while (feed.children.length > 300) feed.removeChild(feed.lastChild);
  }

  function poll() {
    // Only while asked to, only while looked at, and never on an older page:
    // a page scrolled back in time has nothing newer to prepend.
    if (!live.checked || document.hidden || paged || !feed) return;
    fetch('/rolebots/peers/api?after=' + lastID + (query ? '&' + query : ''),
          {credentials: 'same-origin'})
      .then(function (r) { return r.json(); })
      .then(function (d) {
        (d.rows || []).slice().reverse().forEach(function (r) {
          if (r.id > lastID) lastID = r.id;
          addRow(r);
        });
        note.textContent = 'updated ' + (d.now || '');
        dot.className = 'rb-dot';
      })
      .catch(function () { dot.className = 'rb-dot off'; });
  }
  function paint() {
    dot.className = live.checked ? 'rb-dot' : 'rb-dot off';
    note.textContent = live.checked ? 'polling every 15s while visible' : 'paused · tick to poll every 15s while this tab is visible';
  }
  live.addEventListener('change', function () {
    try { localStorage.setItem(KEY, live.checked ? '1' : '0'); } catch (e) {}
    paint();
    if (live.checked) poll();
  });
  paint();
  setInterval(poll, 15000);
})();
</script>
{{end}}`
