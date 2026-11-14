package rolebots

// Templates for the trade pages, apart from their handlers in esptrade.go. The map is two
// pages, New Eden by region and one region drawn alone; they share styling and the pan/zoom
// rig through the two consts below.

// tradeTabs is the sub-tab strip the Economy pages share. It is concatenated into each
// page's source because module pages are parsed independently, so a {{define}} in one is
// invisible to the next. It reads .Tab, not .Page: every page renders .Page "rolebot-economy".
const tradeTabs = `{{define "rb_tradetabs"}}
<div class="rb-subtabs">
  <a href="/rolebots/economy"{{if eq .Tab "overview"}} class="on"{{end}}>Overview</a>
  <a href="/rolebots/trademap"{{if eq .Tab "hubs"}} class="on"{{end}}>Hubs</a>
  <a href="/rolebots/trades"{{if eq .Tab "feed"}} class="on"{{end}}>Feed</a>
  <a href="/rolebots/peers"{{if eq .Tab "peers"}} class="on"{{end}}>Bot&#8202;&rarr;&#8202;Bot</a>
  <a href="/rolebots/wants"{{if eq .Tab "wants"}} class="on"{{end}}>Wants List</a>
</div>
{{end}}`

// tradeTabsStyle is its chrome, likewise concatenated per page.
const tradeTabsStyle = `<style>
.rb-subtabs { display:flex; gap:4px; margin:0 0 12px; border-bottom:1px solid #2a1a1a;
              flex-wrap:wrap; }
.rb-subtabs a { padding:6px 13px; font-size:12px; color:#9ca3af; text-decoration:none;
                border:1px solid transparent; border-bottom:none;
                border-radius:5px 5px 0 0; margin-bottom:-1px; }
.rb-subtabs a:hover { color:#e6ded6; background:#180f0f; }
.rb-subtabs a.on { color:#f0e6dc; background:#140d0d; border-color:#2a1a1a;
                   border-bottom:1px solid #140d0d; }
</style>`

// mapStyle is the chrome both map pages wear.
const mapStyle = `<style>
.rb-mapwrap { position:relative; border:1px solid #2a1a1a; border-radius:6px; background:#0b0708;
              overflow:hidden; }
.rb-mapwrap svg { display:block; width:100%; height:min(66vh, 820px); cursor:grab; touch-action:none; }
.rb-mapwrap svg.drag { cursor:grabbing; }
.rb-mapwrap svg .hit { cursor:pointer; }
.rb-tip { position:absolute; pointer-events:none; display:none; z-index:5; max-width:280px;
          background:#1a1010; border:1px solid #3a2020; border-radius:5px; padding:7px 9px;
          font-size:11px; line-height:1.5; color:#d8d2cc; box-shadow:0 4px 16px #000a; }
.rb-tip b { color:#f0e6dc; }
.rb-tip .term { color:#9ca3af; }
.rb-mapbar { display:flex; gap:14px; align-items:center; flex-wrap:wrap; margin-bottom:10px;
             font-size:12px; color:#9ca3af; }
.rb-mapbar label { display:flex; gap:5px; align-items:center; white-space:nowrap; }
.rb-mapbar input[type=range] { width:120px; }
.rb-mapbar button { background:#180f0f; color:#cbbfae; border:1px solid #33272a; border-radius:4px;
                    padding:2px 9px; font:inherit; font-size:11px; cursor:pointer; }
.rb-mapbar button:hover { background:#241717; color:#f0e6dc; }
.rb-dot { width:8px; height:8px; border-radius:50%; background:#22c55e; display:inline-block; }
.rb-dot.off { background:#6b7280; }
.rb-legend { display:flex; gap:12px; align-items:center; font-size:11px; color:#6b7280;
             margin-top:8px; flex-wrap:wrap; }
.rb-ramp { width:150px; height:8px; border-radius:4px;
           background:linear-gradient(90deg,#1e3a5f,#2f6f8f,#c8922e,#f06a2a,#fff1d6); }
.rb-hubs { width:100%; }
.rb-hubs td.n, .rb-hubs th.n { text-align:right; font-variant-numeric:tabular-nums; }
.rb-hubs td.term { color:#9ca3af; }
.rb-hubs td.neg { color:#e08a8a; }
.rb-hubs tr.anchored td.anchor { color:#6ee7b7; }
.rb-hubs tr.click { cursor:pointer; }
.rb-hubs tr.click:hover { background:#1a1010; }
.rb-hubs a { color:#e0a94a; text-decoration:none; }
.rb-hubs a:hover { text-decoration:underline; }
.rb-crumb { font-size:12px; color:#6b7280; margin:0 0 8px; }
.rb-crumb a { color:#9ca3af; }
</style>`

// listKit is the chrome and behaviour the two ledgers under the map share: collapsed panels side by
// side and click-to-sort on every column. They start closed so the map stays on screen.
// See docs/comment-notes-b08.md#module-page-templates.
const listKit = `<style>
.rb-cols { display:grid; grid-template-columns:repeat(auto-fit,minmax(430px,1fr));
           gap:12px; align-items:start; }
.rb-panel { padding:0; overflow:hidden; }
.rb-panel > summary, .rb-panel > .rb-hdr { padding:9px 12px; font-size:13px; color:#f0e6dc;
                      list-style:none; display:flex; gap:8px; align-items:baseline; }
.rb-panel > summary::-webkit-details-marker { display:none; }
.rb-panel > summary { cursor:pointer; }
.rb-panel > summary::before { content:'\25B8'; color:#9ca3af; font-size:11px; }
.rb-panel[open] > summary::before { content:'\25BE'; }
.rb-panel > summary .term, .rb-panel > .rb-hdr .term { color:#6b7280; font-size:11px; margin-left:auto;
                            text-align:right; font-weight:normal; }
.rb-panel > summary:hover { background:#180f0f; }
.rb-scroll { max-height:70vh; overflow:auto; border-top:1px solid #2a1a1a; }
.rb-scroll table { font-size:11.5px; width:100%; }
.rb-scroll td, .rb-scroll th { padding:3px 7px; white-space:nowrap; }
.rb-scroll th { position:sticky; top:0; background:#140d0d; z-index:1; }
.rb-scroll tbody tr:nth-child(even) { background:#120c0c; }
th.sortable { cursor:pointer; user-select:none; }
th.sortable:hover { color:#f0e6dc; }
th.sortable::after { content:'\2195'; opacity:.22; margin-left:4px; font-size:9px; }
th.sortable.asc::after  { content:'\25B2'; opacity:.85; }
th.sortable.desc::after { content:'\25BC'; opacity:.85; }
</style>
<script>
// A cell sorts on its data-v when it carries one, and only then on its own
// text. Every number on these pages is displayed rounded ("0.5") or short
// ("1.24b"), and both sort wrong as text — 1.24b lands under 900.0m.
function rbCellVal(td) {
  if (!td) return '';
  var v = td.getAttribute('data-v');
  if (v !== null) { var n = parseFloat(v); return isNaN(n) ? 0 : n; }
  var t = (td.textContent || '').trim();
  if (/^-?[\d.]+%?$/.test(t)) return parseFloat(t);
  return t.toLowerCase();
}
function rbApplySort(table) {
  var st = table.__sort;
  if (!st) return;
  var tb = table.tBodies[0];
  if (!tb) return;
  var rows = Array.prototype.slice.call(tb.rows);
  rows.sort(function (a, b) {
    var x = rbCellVal(a.cells[st.i]), y = rbCellVal(b.cells[st.i]);
    if (typeof x === 'number' && typeof y === 'number') return (x - y) * st.dir;
    return String(x).localeCompare(String(y)) * st.dir;
  });
  rows.forEach(function (r) { tb.appendChild(r); });
}
function rbSortable(table) {
  if (!table || !table.tHead) return;
  var ths = table.tHead.rows[0].cells;
  Array.prototype.forEach.call(ths, function (th, i) {
    if (th.classList.contains('nosort')) return;
    th.classList.add('sortable');
    th.addEventListener('click', function () {
      var st = table.__sort;
      // First click on a numeric column opens on the big end: nobody asks
      // for the quietest border or the emptiest region first.
      var dir = (st && st.i === i) ? -st.dir : (th.classList.contains('n') ? -1 : 1);
      Array.prototype.forEach.call(ths, function (o) { o.classList.remove('asc', 'desc'); });
      th.classList.add(dir === 1 ? 'asc' : 'desc');
      table.__sort = {i: i, dir: dir};
      rbApplySort(table);
    });
  });
}
</script>`

// mapRig is the shared drawing kit: colour ramp, money formatter, lattice projection, road
// router and pan/zoom/tooltip handlers. Nodes sit one per cell on the lattice the server
// returns (maplayout.go) and roads are drawn octilinearly. Each page defines its own draw().
const mapRig = `
  var SVGNS = 'http://www.w3.org/2000/svg';
  var svg = document.getElementById('rbMap'), tip = document.getElementById('rbTip');
  var wrap = document.getElementById('rbWrap'), note = document.getElementById('rbNote');
  var live = document.getElementById('rbLive'), dot = document.getElementById('rbDot');
  var fitBtn = document.getElementById('rbFit');
  var view = {x:0, y:0, w:1000, h:1000};

  // The board. PITCH is deliberately wider than it is tall: a label is a
  // wide thing, so a square cell either wastes the vertical or clips the
  // horizontal. Each page sets its own before the first draw.
  var GRID = {cols:1, rows:1};
  var PITCH = {x:168, y:74};
  var MARGIN = 80;
  var BG = [11,7,8]; // the map's own background, for mixing opaque tints

  // Score to colour. Dim blue for a place that merely exists, through
  // amber, to near-white for the busiest there is. Non-linear on purpose:
  // most of the map sits near the bottom and the eye needs the top of the
  // ramp spread over the few that matter.
  function rampRGB(s) {
    var t = Math.pow(Math.max(0, Math.min(100, s)) / 100, 0.6);
    var stops = [[30,58,95],[47,111,143],[200,146,46],[240,106,42],[255,241,214]];
    var i = Math.min(stops.length - 2, Math.floor(t * (stops.length - 1)));
    var f = t * (stops.length - 1) - i, a = stops[i], b = stops[i+1];
    return [Math.round(a[0]+(b[0]-a[0])*f),
            Math.round(a[1]+(b[1]-a[1])*f),
            Math.round(a[2]+(b[2]-a[2])*f)];
  }
  function rgb(c) { return 'rgb(' + c[0] + ',' + c[1] + ',' + c[2] + ')'; }
  function ramp(s) { return rgb(rampRGB(s)); }
  // A chip's fill is mixed against the background rather than made
  // translucent, because a translucent chip lets the roads under it show
  // through and the point of a chip is that it stops them.
  function mix(c, a) {
    return rgb([Math.round(BG[0] + (c[0]-BG[0])*a),
                Math.round(BG[1] + (c[1]-BG[1])*a),
                Math.round(BG[2] + (c[2]-BG[2])*a)]);
  }
  // The client's own security colours, so a sec figure reads the way it
  // does in game.
  function secColor(s) {
    var v = Math.round(Math.max(0, Math.min(1, s)) * 10);
    var c = ['#f00000','#d73000','#f04800','#f06000','#d77700','#efef00',
             '#8fef2f','#00f000','#00ef47','#48f0c0','#2fefef'];
    return c[v];
  }

  function boardW() { return GRID.cols * PITCH.x + MARGIN*2; }
  function boardH() { return GRID.rows * PITCH.y + MARGIN*2; }
  function px(n) { return MARGIN + n.col * PITCH.x + PITCH.x/2; }
  function py(n) { return MARGIN + n.row * PITCH.y + PITCH.y/2; }

  // The default frame is the box the NODES occupy, not the whole lattice.
  // A grid is sized to its widest row and tallest column, so a cluster that
  // leaves one far corner empty gets framed against dead board and drifts
  // off centre; measuring the nodes puts the map in the middle of the
  // window where it belongs. FIT is that frame, and it is also the zoom-out
  // stop — nothing on this page is served by pulling further back than the
  // view the page opens on.
  var EXTENT = null, FIT = null;
  function setExtent(list) {
    if (!list || !list.length) { EXTENT = null; return; }
    var c0 = 1e9, c1 = -1e9, r0 = 1e9, r1 = -1e9;
    list.forEach(function (n) {
      if (n.col < c0) c0 = n.col;
      if (n.col > c1) c1 = n.col;
      if (n.row < r0) r0 = n.row;
      if (n.row > r1) r1 = n.row;
    });
    EXTENT = {c0:c0, c1:c1, r0:r0, r1:r1};
  }
  function fit() {
    if (!EXTENT) {
      view = {x:0, y:0, w:boardW(), h:boardH()};
    } else {
      var padX = PITCH.x * 0.25, padY = PITCH.y * 0.35;
      var x0 = MARGIN + EXTENT.c0 * PITCH.x - padX;
      var y0 = MARGIN + EXTENT.r0 * PITCH.y - padY;
      view = {x: x0, y: y0,
              w: (EXTENT.c1 - EXTENT.c0 + 1) * PITCH.x + padX*2,
              h: (EXTENT.r1 - EXTENT.r0 + 1) * PITCH.y + padY*2};
    }
    FIT = {x:view.x, y:view.y, w:view.w, h:view.h};
    setView();
  }
  // Panning is kept inside the default frame for the same reason zooming
  // out is stopped at it: off the edge there is nothing to look at, and a
  // map dragged into empty space reads as a map that failed to load.
  function clampView() {
    if (!FIT) return;
    view.x = view.w >= FIT.w ? FIT.x + (FIT.w - view.w)/2
                             : Math.min(Math.max(view.x, FIT.x), FIT.x + FIT.w - view.w);
    view.y = view.h >= FIT.h ? FIT.y + (FIT.h - view.h)/2
                             : Math.min(Math.max(view.y, FIT.y), FIT.y + FIT.h - view.h);
  }

  function isk(v) {
    if (v >= 1e9) return (v/1e9).toFixed(2) + 'b';
    if (v >= 1e6) return (v/1e6).toFixed(2) + 'm';
    if (v >= 1e3) return (v/1e3).toFixed(1) + 'k';
    return Math.round(v);
  }
  function esc(s) { return (s || '').replace(/[&<>"]/g, function (c) {
    return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }
  function setView() {
    svg.setAttribute('viewBox', view.x + ' ' + view.y + ' ' + view.w + ' ' + view.h);
  }
  function clear() {
    while (svg.firstChild) svg.removeChild(svg.firstChild);
  }
  function layer() {
    var g = document.createElementNS(SVGNS, 'g');
    svg.appendChild(g);
    return g;
  }
  // Everything is deaf to the pointer by default and made to listen one
  // call at a time (hit()). A chip is half a dozen shapes stacked up — the
  // score bar, the depth bar, the labels — and any one of them left
  // listening swallows the hover meant for the chip under it, so the
  // tooltip blinks out wherever the operator's mouse crosses a decoration.
  function rect(g, x, y, w, h, fill, stroke, sw, rx) {
    var r = document.createElementNS(SVGNS, 'rect');
    r.setAttribute('x', x); r.setAttribute('y', y);
    r.setAttribute('width', Math.max(0, w)); r.setAttribute('height', Math.max(0, h));
    if (rx) r.setAttribute('rx', rx);
    r.setAttribute('fill', fill || 'none');
    if (stroke) { r.setAttribute('stroke', stroke); r.setAttribute('stroke-width', sw || 1); }
    r.style.pointerEvents = 'none';
    g.appendChild(r);
    return r;
  }
  // hit makes one shape the thing the pointer finds, and hangs whatever the
  // page's tipFor() needs off it.
  function hit(node, key, val, href) {
    node.style.pointerEvents = 'all';
    node[key] = val;
    if (href) { node.setAttribute('class', 'hit'); node.__href = href; }
    return node;
  }
  // Monospace on purpose: a rigid map wants a rigid face, and a fixed
  // advance width means a label's width can be estimated from its length
  // instead of measured — a getComputedTextLength() per node is a layout
  // flush per node, several hundred of them per redraw.
  function text(g, x, y, str, size, fill, anchor, maxW) {
    var t = document.createElementNS(SVGNS, 'text');
    t.setAttribute('x', x); t.setAttribute('y', y);
    t.setAttribute('fill', fill); t.setAttribute('font-size', size);
    t.setAttribute('font-family', 'Consolas,Menlo,DejaVu Sans Mono,monospace');
    if (anchor) t.setAttribute('text-anchor', anchor);
    t.setAttribute('pointer-events', 'none');
    t.textContent = str;
    if (maxW && str.length * size * 0.6 > maxW) {
      t.setAttribute('textLength', maxW);
      t.setAttribute('lengthAdjust', 'spacingAndGlyphs');
    }
    g.appendChild(t);
    return t;
  }

  // Octilinear routing. A pair that is already aligned — same column, same
  // row, or exactly on the diagonal — is one straight line. Anything else
  // gets one diagonal leg out of A and one axis-aligned leg into B, so
  // every segment on the map is at 0, 45 or 90 degrees and a road can be
  // followed by eye. Which leg comes first is fixed rather than chosen,
  // because a router that picks per pair makes the same road change shape
  // when its neighbour moves.
  function route(a, b) {
    var dc = b.col - a.col, dr = b.row - a.row;
    var pts = [[px(a), py(a)]];
    if (dc !== 0 && dr !== 0 && Math.abs(dc) !== Math.abs(dr)) {
      var k = Math.min(Math.abs(dc), Math.abs(dr));
      pts.push([px({col: a.col + (dc>0?k:-k)}), py({row: a.row + (dr>0?k:-k)})]);
    }
    pts.push([px(b), py(b)]);
    return pts;
  }
  function road(g, a, b, stroke, width, opacity, dash) {
    var pts = route(a, b);
    var p = document.createElementNS(SVGNS, 'polyline');
    p.setAttribute('points', pts.map(function (q) { return q[0] + ',' + q[1]; }).join(' '));
    p.setAttribute('fill', 'none');
    p.setAttribute('stroke', stroke);
    p.setAttribute('stroke-width', width);
    p.setAttribute('stroke-opacity', opacity);
    p.setAttribute('stroke-linecap', 'round');
    p.setAttribute('stroke-linejoin', 'round');
    if (dash) p.setAttribute('stroke-dasharray', dash);
    p.style.pointerEvents = 'none';
    g.appendChild(p);
    return p;
  }
  // A one-pixel line is a hard thing to hover, and a road drawn later would
  // sit over an earlier road's catcher. So every hoverable road gets a fat
  // invisible twin, all of them in a layer of their own above the paint.
  function roadHit(g, a, b, lane) {
    var h = road(g, a, b, 'transparent', 11, 1, '');
    h.style.pointerEvents = 'stroke';
    h.style.cursor = 'default';
    h.__lane = lane;
    return h;
  }

  svg.addEventListener('mousemove', function (e) {
    var html = tipFor(e.target);
    if (!html) { tip.style.display = 'none'; return; }
    tip.innerHTML = html;
    tip.style.display = 'block';
    var box = wrap.getBoundingClientRect();
    var x = e.clientX - box.left + 14, y = e.clientY - box.top + 14;
    if (x + 290 > box.width) x = box.width - 290;
    tip.style.left = x + 'px'; tip.style.top = y + 'px';
  });
  svg.addEventListener('mouseleave', function () { tip.style.display = 'none'; });

  // Zoom keeps the board's aspect: the lattice is not square, and a square
  // viewBox over a wide board letterboxes half the map away.
  svg.addEventListener('wheel', function (e) {
    e.preventDefault();
    var box = svg.getBoundingClientRect();
    var fx = (e.clientX - box.left) / box.width, fy = (e.clientY - box.top) / box.height;
    var k = e.deltaY < 0 ? 0.82 : 1.22;
    var aspect = view.h / view.w;
    var cap = FIT ? FIT.w : boardW();
    if (k > 1 && view.w >= cap - 0.5) return; // already at the stop
    var nw = Math.max(240, Math.min(cap, view.w * k)), nh = nw * aspect;
    view.x += (view.w - nw) * fx; view.y += (view.h - nh) * fy;
    view.w = nw; view.h = nh;
    clampView();
    setView();
  }, {passive: false});

  // A drag must not fire the click that opens a region — a pan of two
  // hundred pixels ending on a dot is not a request to navigate.
  var dragging = null, dragged = false;
  svg.addEventListener('mousedown', function (e) {
    dragging = {x: e.clientX, y: e.clientY, vx: view.x, vy: view.y};
    dragged = false;
    svg.classList.add('drag');
  });
  window.addEventListener('mouseup', function () { dragging = null; svg.classList.remove('drag'); });
  window.addEventListener('mousemove', function (e) {
    if (!dragging) return;
    if (Math.abs(e.clientX - dragging.x) + Math.abs(e.clientY - dragging.y) > 3) dragged = true;
    var box = svg.getBoundingClientRect();
    view.x = dragging.vx - (e.clientX - dragging.x) * view.w / box.width;
    view.y = dragging.vy - (e.clientY - dragging.y) * view.h / box.height;
    clampView();
    setView();
  });
  svg.addEventListener('click', function (e) {
    if (dragged) return;
    var href = e.target.__href;
    if (href) window.location = href;
  });
  if (fitBtn) fitBtn.addEventListener('click', fit);
`

// ── New Eden, by region ─────────────────────────────────────────────────────

const tmplTradeMap = tradeTabs + `{{define "content"}}
` + tradeTabsStyle + mapStyle + listKit + `

{{template "rb_tradetabs" .}}
<h2>Trade Hubs</h2>

<p style="margin:0 0 10px;color:#9ca3af;font-size:12px">
  Where commerce actually happens, measured rather than assumed — New Eden by <b>region</b>, drawn on
  a lattice rather than at real coordinates. Click a region to open it. Swept <b>{{.Swept}}</b>.
  <a href="#" id="rbWhy" style="color:#9ca3af">how it is scored &rsaquo;</a>
  {{if not .ShowPurge}}<a href="/rolebots/trademap?purge=1" style="color:#9ca3af;margin-left:8px">wipe economic data &rsaquo;</a>{{end}}
</p>

{{if .ShowPurge}}
<div class="card" style="border-color:#7f1d1d">
  <h3 style="margin:0 0 6px;color:#fca5a5">Wipe economic data</h3>
  <p style="margin:0 0 10px;color:#9ca3af;font-size:12px">
    This is the economy's own wipe, separate from the bot wipe on purpose — it takes what the roster
    LEARNED and what it DID, and leaves the pilots alone. Retuning prices or bands without it means
    the roster keeps routing by a map of hubs scored under the old economy.
  </p>
  <form method="post" action="/rolebots/trademap">
    <input type="hidden" name="action" value="purgeecon">
    <p style="margin:0 0 6px">
      <label><input type="checkbox" name="learned" checked>
        <b>Learned data</b> — hub and station scores, traffic (systems, lanes, stations), the trade
        book behind the Feed and the bot-to-bot half behind Bot&rarr;Bot Trades, the roster's order
        ledger, the market and mineral demand ledgers, the mineral hold, the industry shelf and trade
        stock, and the ISK-flow ledgers behind the Economy overview. <b>{{.LearnedRows}}</b> row(s). All of it rebuilds: the sweeps re-score within minutes
        and the ledgers refill as pilots trade.</label>
    </p>
    <p style="margin:0 0 6px">
      <label><input type="checkbox" name="orders" checked>
        <b>Resting orders</b> — every order the roster currently has on the market.
        <b>{{.RestingOrders}}</b> live. Cancelled properly rather than deleted: bids refund their
        escrow and asks return their unsold goods to the hangar, so no ISK and no cargo is destroyed.
        Taken automatically with the learned data whether or not this is ticked — emptying the order
        ledger while the orders are still resting would leave them on the market with nothing left
        that knows to cancel them.</label>
    </p>
    <p style="margin:0 0 6px">
      <label><input type="checkbox" name="history">
        <b>Wallet history</b> — the rostered pilots' own market transactions and journal entries.
        <b>{{.HistoryRows}}</b> row(s). This does <b>not</b> come back. Real players' rows are keyed
        out of it; only characters on the roster are touched.</label>
    </p>
    <p style="margin:0 0 8px;color:#6b7280;font-size:12px">
      Never touched: the NPC seeder's orders and any real player's — only orders belonging to
      characters on the roster come down, and both wallet queries are keyed the same way. Killmails
      are not economic data and keep their own retention. The pilots themselves, their skills,
      corporations and gangs are the bot wipe's business, not this one's.
    </p>
    <p style="margin:0">
      <input type="text" name="confirm" placeholder="type {{.PurgeConfirm}}" autocomplete="off"
             style="width:230px" required>
      <button type="submit" class="danger"
              onclick="return confirm('Wipe the roster\'s economic data? Wallet history does not come back.')">
        Wipe economic data</button>
      <a href="/rolebots/trademap" style="margin-left:8px;color:#9ca3af">cancel</a>
    </p>
  </form>
</div>
{{end}}
<div class="card" id="rbWhyBox" style="display:none;color:#6b7280;font-size:12px">
  <p style="margin:0 0 6px">
    One region per cell, borders at right angles or clean diagonals, seeded from real positions so the
    cluster still sits where you expect it — nothing overlaps, which is the only way sixty-odd names
    and their numbers fit in one frame. A region is drawn on its <b>best</b> system, because a region
    is somewhere you go to trade at its hub and forty quiet stations do not add up to one busy one;
    <b>depth</b> is every system in it added up.
  </p>
  <p style="margin:0 0 6px">
    A system scores only if it has a station: arrivals, lane weight either side, the size of the local
    book, ISK the roster has moved there, gate count (a dead end is a place you go <i>to</i>, a
    crossroads one you go <i>through</i>) and security. <b>This map describes; it does not steer.</b>
    No bot reads this score to decide anything. Where a bot sells reads measured <b>liquidity</b>
    &mdash; what actually changed hands at a station between two parties neither of whom is an NPC
    seeder &mdash; and where a bot lives is its employer's call, priced against how crowded each
    address already is. Homes and factories are shown, never scored.
  </p>
  <p style="margin:0">
    <b>Every order count and book value here is players and rolebots only</b> — the NPC seeder is at
    every station, never runs out, and is much the same everywhere, so counting it tells one station
    from another not at all (it still earns a fifth of a real order's weight in the score).
  </p>
</div>

{{if not .HasAny}}
<div class="alert alert-error">No scores yet. The sweep runs every five minutes. Nothing waits on it:
the map is a description, and no bot reads it.</div>
{{end}}

<div class="card">
  <div class="rb-mapbar">
    <label><input type="checkbox" id="rbLive" checked> <span class="rb-dot" id="rbDot"></span> live</label>
    <label><input type="checkbox" id="rbPilots"> count pilots</label>
    <label><input type="checkbox" id="rbLanes" checked> roads</label>
    <label><input type="checkbox" id="rbRoutes" checked> off-road trade routes</label>
    <label><input type="checkbox" id="rbNames" checked> labels</label>
    <label>min score <input type="range" id="rbMin" min="0" max="60" value="0"> <span id="rbMinV">0</span></label>
    <button type="button" id="rbFit">fit</button>
    <span id="rbNote" style="margin-left:auto">loading&hellip;</span>
  </div>
  <div class="rb-mapwrap" id="rbWrap">
    <svg id="rbMap" viewBox="0 0 1000 1000" preserveAspectRatio="xMidYMid meet"></svg>
    <div class="rb-tip" id="rbTip"></div>
  </div>
  <div class="rb-legend">
    <span>best system's score</span><span class="rb-ramp"></span><span>0 &rarr; 100</span>
    <span style="margin-left:12px">bar under the name = depth behind it</span>
    <span style="color:#6ee7b7">green border = a declared home, clone bay or factory</span>
    <span style="color:#e0a94a">&#9473; a road the roster hauls over</span>
    <span style="color:#7a6a62">&#9548; a trade route with no stargate under it</span>
    <span style="margin-left:auto">click a region to open it &middot; scroll to zoom, drag to pan</span>
  </div>
</div>

<div class="rb-cols">
  <div class="card rb-panel">
    <div class="rb-hdr">Traffic between regions <span class="term" id="rbTrafficNote">&hellip;</span></div>
    <div class="rb-scroll">
      <table class="rb-hubs" id="rbTraffic">
        <thead><tr>
          <th>Route</th><th class="n">Jumps</th><th class="n">Hauls</th><th class="n">Moved</th>
          <th class="n">Per haul</th><th class="n">Share</th>
        </tr></thead>
        <tbody></tbody>
      </table>
    </div>
  </div>

  <div class="card rb-panel">
    <div class="rb-hdr">Regions <span class="term">{{len .Top}} scored</span></div>
    <div class="rb-scroll">
      <table class="rb-hubs" id="rbRegions">
        <thead><tr>
          <th>Region</th><th>Best system</th><th class="n">Sec</th><th class="n">Score</th>
          <th class="n">Depth</th><th class="n">Sys</th>
          <th class="n">Orders</th><th class="n">Book</th>
          <th>Bases</th>
        </tr></thead>
        <tbody>
        {{range .Top}}
          <tr class="click{{if or .Homes .Factories}} anchored{{end}}"
              onclick="window.location='/rolebots/trademap/region/{{.ID}}'">
            <td><a href="/rolebots/trademap/region/{{.ID}}">{{.Name}}</a></td>
            <td>{{.Where}}</td>
            <td class="n" data-v="{{.SecV}}">{{.Sec}}</td>
            <td class="n" data-v="{{.ScoreV}}"><b>{{.Score}}</b></td>
            <td class="n term" data-v="{{.DepthV}}">{{.Base}}</td>
            <td class="n term">{{.Systems}}</td>
            <td class="n">{{.PlayerOrders}}</td>
            <td class="n" data-v="{{.BookV}}">{{.Stock}}</td>
            <td class="anchor" data-v="{{.AnchorV}}">{{if .Homes}}{{.Homes}} home{{end}}{{if .Factories}} {{.Factories}} factory{{end}}</td>
          </tr>
        {{end}}
        </tbody>
      </table>
    </div>
  </div>
</div>

<script>
(function () {
  var pilotsOn = document.getElementById('rbPilots'), lanesOn = document.getElementById('rbLanes');
  var routesOn = document.getElementById('rbRoutes');
  var namesOn = document.getElementById('rbNames'), minR = document.getElementById('rbMin');
  var minV = document.getElementById('rbMinV');
  var tbody = document.querySelector('#rbTraffic tbody');
  var tnote = document.getElementById('rbTrafficNote');
  var data = null, fitted = false;
` + mapRig + `

  PITCH = {x:206, y:86};
  var CW = 164, CH = 48; // the chip inside that cell, leaving a clear gap for the roads

  function tipFor(t) {
    var l = t.__lane;
    if (l) {
      var html = '<b>' + esc(l.aName) + '</b> &harr; <b>' + esc(l.bName) + '</b>' +
                 '<br><span class="term">' + (l.road ? 'gate border' : 'no stargate — a run, not a road') +
                 '</span>';
      if (!l.jumps && !l.hauls) {
        return html + '<br><span class="term">the roster has never used it</span>';
      }
      return html +
        '<br>' + Math.round(l.jumps) + ' jumps &middot; ' + Math.round(l.hauls) + ' hauls' +
        '<br>' + isk(l.isk) + ' moved' +
        (l.hauls > 0 ? ' <span class="term">(' + isk(l.isk / l.hauls) + ' a haul)</span>' : '');
    }
    var r = t.__region;
    if (!r) return '';
    var html = '<b>' + esc(r.name) + '</b>';
    if (!r.systems) return html + '<br><span class="term">no station in the region scores</span>';
    html += '<br>best <b>' + esc(r.best) + '</b> at score ' + r.score.toFixed(0) +
            ' <span class="term">' + r.sec.toFixed(1) + '</span>' +
            '<br><span class="term">' + r.systems + ' scoring systems, depth ' + r.depth.toFixed(0) + '</span>' +
            '<br><span class="term">' + r.orders + ' player/bot orders, ' +
            isk(r.isk) + ' on the book</span>';
    if (r.homes || r.factories) {
      html += '<br><span style="color:#6ee7b7">' +
              (r.homes ? r.homes + ' home ' : '') +
              (r.factories ? r.factories + ' factory' : '') + '</span>';
    }
    if (r.pilots) {
      html += '<br><span class="term">' + r.pilots + ' pilot(s) here, ' + r.bots + ' of them bots</span>';
    }
    return html + '<br><span class="term">click to open</span>';
  }

  function draw() {
    if (!data) return;
    var minScore = +minR.value;
    var byID = {}, deepest = 1;
    data.regions.forEach(function (r) {
      byID[r.id] = r;
      if (r.depth > deepest) deepest = r.depth;
    });

    clear();
    var gRoads = layer(), gHits = layer(), gChips = layer();

    (data.links || []).forEach(function (l) {
      var a = byID[l.a], b = byID[l.b];
      if (!a || !b) return;
      if (!(l.road ? lanesOn.checked : routesOn.checked)) return;
      // A road the roster hauls over is drawn as a trade route; one it has
      // only ever looked at stays a road; and a pair with no stargate at
      // all is dashed, because it is a run the roster invented.
      var used = l.w > 0;
      road(gRoads, a, b,
           l.hauls > 0 ? '#e0a94a' : (used ? '#5b86ad' : '#463732'),
           (l.road ? 1.4 + l.w * 5 : 1 + l.w * 3.5).toFixed(2),
           (used ? 0.45 + l.w * 0.5 : 0.9).toFixed(2),
           l.road ? '' : '7 6');
      roadHit(gHits, a, b, {aName:a.name, bName:b.name, road:l.road,
                            jumps:l.jumps, hauls:l.hauls, isk:l.isk});
    });

    data.regions.forEach(function (r) {
      var x = px(r), y = py(r);
      // A region under the filter is still drawn — as a stud, not a chip.
      // Dropping it entirely would tear holes in the road network, which
      // is the one thing on this page that is not about score at all.
      var href = '/rolebots/trademap/region/' + r.id;
      if (r.score < minScore) {
        hit(rect(gChips, x-8, y-8, 16, 16, '#1a1214', '#3a2c26', 1, 2), '__region', r, href);
        return;
      }
      var anchored = (r.homes + r.factories) > 0;
      var c = r.systems ? rampRGB(r.score) : [70,62,58];
      if (!namesOn.checked) {
        hit(rect(gChips, x-11, y-11, 22, 22, mix(c, 0.75),
                 anchored ? '#6ee7b7' : mix(c, 1), anchored ? 1.6 : 1, 3),
            '__region', r, href);
        return;
      }
      hit(rect(gChips, x-CW/2, y-CH/2, CW, CH, mix(c, 0.17),
               anchored ? '#6ee7b7' : mix(c, 0.6), anchored ? 1.6 : 1, 3),
          '__region', r, href);
      // The score bar down the left edge, so a column of chips can be read
      // for standing without reading a single number.
      rect(gChips, x-CW/2+3, y-CH/2+3, 5, CH-6, r.systems ? rgb(c) : '#3a332e', null, 0, 2);
      text(gChips, x-CW/2+15, y-6, r.name, 13.5, r.systems ? '#f2e9df' : '#8a807a',
           null, CW-64);
      text(gChips, x+CW/2-9, y-6, r.systems ? r.score.toFixed(0) : '–', 13.5,
           r.systems ? rgb(c) : '#5c534c', 'end');
      text(gChips, x-CW/2+15, y+9, r.systems ? r.best : 'no station scores', 9.5,
           '#93887f', null, CW-58);
      if (r.pilots) {
        text(gChips, x+CW/2-9, y+9, r.pilots + (r.bots ? '/' + r.bots : ''), 9.5,
             '#6ee7b7', 'end');
      }
      // Depth along the bottom: colour is the peak, this is the hinterland.
      rect(gChips, x-CW/2+15, y+CH/2-8, CW-30, 3, '#241c1c', null, 0, 1.5);
      if (r.depth > 0) {
        rect(gChips, x-CW/2+15, y+CH/2-8, (CW-30) * Math.sqrt(r.depth/deepest), 3,
             rgb(c), null, 0, 1.5);
      }
    });
  }

  // The traffic ledger under the map, off the same links the roads are
  // drawn from — so the table and the picture can never disagree.
  function drawTraffic() {
    if (!data) return;
    var byID = {};
    data.regions.forEach(function (r) { byID[r.id] = r; });
    var rows = (data.links || []).filter(function (l) {
      return (l.jumps > 0 || l.hauls > 0) && byID[l.a] && byID[l.b];
    });
    var moved = 0;
    rows.forEach(function (l) { moved += l.isk; });
    rows.sort(function (a, b) {
      return (b.hauls*3 + b.jumps) - (a.hauls*3 + a.jumps);
    });
    tbody.innerHTML = rows.slice(0, 80).map(function (l) {
      var share = moved > 0 ? (l.isk / moved * 100) : 0;
      // A pair with no stargate under it is a run the roster invented
      // rather than a border of New Eden. That used to be a column of its
      // own; it is a dash on the arrow now, which is the same fact in the
      // width the panel actually has.
      return '<tr>' +
        '<td><a href="/rolebots/trademap/region/' + l.a + '">' + esc(byID[l.a].name) + '</a>' +
        ' <span class="term" title="' + (l.road ? 'gate border' : 'no stargate — a run, not a road') +
        '">' + (l.road ? '&harr;' : '&#8703;') + '</span> ' +
        '<a href="/rolebots/trademap/region/' + l.b + '">' + esc(byID[l.b].name) + '</a></td>' +
        '<td class="n">' + Math.round(l.jumps) + '</td>' +
        '<td class="n">' + Math.round(l.hauls) + '</td>' +
        '<td class="n" data-v="' + l.isk + '">' + isk(l.isk) + '</td>' +
        '<td class="n term" data-v="' + (l.hauls > 0 ? l.isk / l.hauls : 0) + '">' +
          (l.hauls > 0 ? isk(l.isk / l.hauls) : '&mdash;') + '</td>' +
        '<td class="n term">' + share.toFixed(1) + '%</td>' +
        '</tr>';
    }).join('');
    // The table is rebuilt from scratch every poll, so whatever column the
    // operator sorted by has to be re-applied or it lasts eight seconds.
    rbApplySort(document.getElementById('rbTraffic'));
    if (!rows.length) {
      tnote.textContent = 'nothing crossed yet';
      return;
    }
    tnote.textContent = rows.length + ' pairs, ' + isk(moved) + ' moved' +
      (rows.length > 80 ? ' — busiest 80 shown' : '');
  }

  function load() {
    if (!live.checked || document.hidden) return;
    fetch('/rolebots/trademap/api' + (pilotsOn.checked ? '?pilots=1' : ''),
          {credentials: 'same-origin'})
      .then(function (r) { return r.json(); })
      .then(function (d) {
        data = d;
        GRID = {cols: d.cols || 1, rows: d.rows || 1};
        setExtent(d.regions);
        // Fit once. Refitting on every poll would yank the view back from
        // wherever the operator had panned it, every eight seconds.
        if (!fitted) { fit(); fitted = true; }
        draw();
        drawTraffic();
        var roads = (d.links || []).filter(function (l) { return l.road; }).length;
        note.textContent = d.regions.length + ' regions, ' + roads + ' borders, ' +
          ((d.links || []).length - roads) + ' off-road routes' +
          (d.pilots !== undefined ? ', ' + d.pilots + ' pilots' : '') +
          ' — ' + (d.now || '');
        dot.className = 'rb-dot';
      })
      .catch(function () { dot.className = 'rb-dot off'; note.textContent = 'update failed'; });
  }
  rbSortable(document.getElementById('rbTraffic'));
  rbSortable(document.getElementById('rbRegions'));
  var why = document.getElementById('rbWhy'), whyBox = document.getElementById('rbWhyBox');
  why.addEventListener('click', function (e) {
    e.preventDefault();
    var open = whyBox.style.display === 'none';
    whyBox.style.display = open ? '' : 'none';
    why.textContent = open ? 'hide ‹' : 'how it is scored ›';
  });
  [lanesOn, routesOn, namesOn].forEach(function (el) { el.addEventListener('change', draw); });
  pilotsOn.addEventListener('change', load);
  minR.addEventListener('input', function () { minV.textContent = minR.value; draw(); });
  live.addEventListener('change', function () {
    dot.className = live.checked ? 'rb-dot' : 'rb-dot off';
    if (live.checked) load();
  });
  setView();
  load();
  setInterval(load, 8000);
})();
</script>
{{end}}`

// ── one region ──────────────────────────────────────────────────────────────

const tmplTradeMapRegion = tradeTabs + `{{define "content"}}
` + tradeTabsStyle + mapStyle + `

{{template "rb_tradetabs" .}}
<p class="rb-crumb"><a href="/rolebots/trademap">&larr; New Eden</a></p>
<h2>{{.Region}}</h2>

<div class="card">
  <p style="margin:0;color:#9ca3af;font-size:12px">
    Every system in the region on its real gate graph, drawn on a <b>lattice rather than at its real
    coordinates</b>: one system per cell, every road at a right angle or a clean diagonal. The shape
    is no longer to scale and is not trying to be — what it buys is that no two labels can collide
    and a route can be followed from one end to the other. The pale lines are stargates, the bright
    ones are lanes the roster has actually flown; hover either for what has moved over it. The table
    below is the whole score, term by term. The score is a description &mdash; no bot reads it.
    <b>Traded</b> is what bots do read when choosing where to sell: ISK that changed hands here lately
    between two parties neither of whom is an NPC seeder. <b>Orders</b> and <b>on the book</b> are what
    players and rolebots have written here; the NPC seeder is at every station in the game and is
    never counted into either. Last swept: <b>{{.Swept}}</b>.
  </p>
</div>

{{if not .HasAny}}
<div class="alert alert-error">Nothing in this region has a score. Either no system here has a
station, or the sweep has not run yet.</div>
{{end}}

<div class="card">
  <div class="rb-mapbar">
    <label><input type="checkbox" id="rbLive" checked> <span class="rb-dot" id="rbDot"></span> live</label>
    <label><input type="checkbox" id="rbPilots"> show everybody</label>
    <label><input type="checkbox" id="rbGates" checked> stargates</label>
    <label><input type="checkbox" id="rbLanes" checked> roster lanes</label>
    <label><input type="checkbox" id="rbNames" checked> labels</label>
    <label>min score <input type="range" id="rbMin" min="0" max="60" value="0"> <span id="rbMinV">0</span></label>
    <button type="button" id="rbFit">fit</button>
    <span id="rbNote" style="margin-left:auto">loading&hellip;</span>
  </div>
  <div class="rb-mapwrap" id="rbWrap">
    <svg id="rbMap" viewBox="0 0 1000 1000" preserveAspectRatio="xMidYMid meet"></svg>
    <div class="rb-tip" id="rbTip"></div>
  </div>
  <div class="rb-legend">
    <span>score</span><span class="rb-ramp"></span><span>0 &rarr; 100</span>
    <span style="margin-left:12px;color:#6ee7b7">green border = a bot home, clone bay or factory (not scored)</span>
    <span style="color:#e0a94a">&#9473; a lane the roster hauls over</span>
    <span>&#9679; pilot</span><span style="color:#6ee7b7">&#9679; trading</span>
    <span style="color:#5c534c">no station &mdash; cannot be a hub</span>
    <span style="margin-left:auto">hover a lane for its numbers &middot; scroll to zoom, drag to pan</span>
  </div>
</div>

<div class="card">
  <h3>Systems</h3>
  <table class="rb-hubs">
    <thead><tr>
      <th>System</th><th>Constellation</th><th class="n">Sec</th><th class="n">Gates</th>
      <th class="n">Score</th><th class="n">Traded</th><th>School</th>
      <th class="n">Traffic</th><th class="n">Lanes</th><th class="n">Market</th>
      <th class="n">Flow</th><th class="n">Docks</th><th class="n">Topo</th><th class="n">Sec</th>
      <th class="n">Orders</th><th class="n">On the book</th><th>Bases</th>
    </tr></thead>
    <tbody>
    {{range .Top}}
      <tr{{if or .Homes .Factories}} class="anchored"{{end}}>
        <td>{{.Name}}</td>
        <td class="term">{{.Where}}</td>
        <td class="n">{{.Sec}}</td>
        <td class="n term">{{.Gates}}</td>
        <td class="n"><b>{{.Score}}</b></td>
        <td class="n">{{.Traded}}</td>
        <td class="term">{{.School}}</td>
        <td class="n term">{{.Traffic}}</td>
        <td class="n term">{{.Lane}}</td>
        <td class="n term">{{.Market}}</td>
        <td class="n term">{{.Flow}}</td>
        <td class="n term">{{.StationT}}</td>
        <td class="n term">{{.Topo}}</td>
        <td class="n term">{{.SecT}}</td>
        <td class="n">{{.PlayerOrders}}</td>
        <td class="n">{{.Stock}}</td>
        <td>{{if .Homes}}{{.Homes}} home{{end}}{{if .Factories}} {{.Factories}} factory{{end}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
</div>

<div class="card">
  <h3>Docks</h3>
  <p class="term" style="margin-top:0">
    The contest INSIDE each system: once a bot has picked a system to trade in,
    this says which of its docks it stocks, sells loot at or heads for. It is
    built from what a station offers that its neighbours do not &mdash; a
    medical bay, a factory floor, a laboratory &mdash; plus what actually trades
    there and how many pilots have been landing there. Bot homes and factories
    are shown, not scored. Market, fitting, repair and docking are worth nothing
    here: every station in New Eden has them, so they separate nothing.
    <b>Winner</b> marks the dock its system would send a bot to today.
    <b>Shop gap</b> counts the goods shoppers came here for this week and found
    short; sellers carry those goods toward it. It is not part of the score.
  </p>
  <table class="rb-hubs">
    <thead><tr>
      <th>Station</th><th>System</th><th class="n">Score</th>
      <th class="n">Amenities</th><th class="n">Traded</th><th class="n">Guests</th>
      <th class="n">Book</th><th class="n">Shop gap</th><th>Exclusive services</th><th>Bases</th><th></th>
    </tr></thead>
    <tbody>
    {{range .Docks}}
      <tr{{if or .Homes .Factories}} class="anchored"{{end}}>
        <td>{{.Name}}</td>
        <td class="term">{{.System}}</td>
        <td class="n"><b>{{.Score}}</b></td>
        <td class="n term">{{.Amenity}}</td>
        <td class="n term">{{.Traded}}</td>
        <td class="n term">{{.Guest}}</td>
        <td class="n term">{{.Book}}</td>
        <td class="n term">{{.ShopGap}}</td>
        <td class="term">{{.Services}}</td>
        <td>{{if .Homes}}{{.Homes}} home{{end}}{{if .Factories}} {{.Factories}} factory{{end}}</td>
        <td class="term">{{if .Winner}}winner{{end}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  {{if not .Docks}}<p class="term">Nothing swept yet.</p>{{end}}
</div>

<script>
(function () {
  var pilotsOn = document.getElementById('rbPilots'), lanesOn = document.getElementById('rbLanes');
  var gatesOn = document.getElementById('rbGates'), namesOn = document.getElementById('rbNames');
  var minR = document.getElementById('rbMin'), minV = document.getElementById('rbMinV');
  var regionID = {{.RegionID}};
  var data = null, fitted = false;
` + mapRig + `

  PITCH = {x:176, y:72};
  var CW = 138, CH = 38; // the chip inside that cell, leaving a clear gap for the roads

  function tipFor(t) {
    var s = t.__sys, p = t.__pilot, l = t.__lane;
    if (p) {
      return '<b>' + esc(p.name) + '</b><br>' + esc(p.doing || '') +
             '<br><span class="term">' + (p.docked ? 'docked in ' : 'in space, ') + esc(p.where) + '</span>';
    }
    if (l) {
      var html = '<b>' + esc(l.aName) + '</b> &harr; <b>' + esc(l.bName) + '</b>' +
                 '<br><span class="term">' + (l.gate ? 'stargate' : 'no stargate — a run, not a road') +
                 '</span>';
      if (!l.jumps && !l.hauls) {
        return html + '<br><span class="term">the roster has never used it</span>';
      }
      return html + '<br>' + Math.round(l.jumps) + ' jumps &middot; ' + Math.round(l.hauls) +
             ' hauls<br>' + isk(l.isk) + ' moved' +
             (l.hauls > 0 ? ' <span class="term">(' + isk(l.isk / l.hauls) + ' a haul)</span>' : '');
    }
    if (!s) return '';
    var html = '<b>' + esc(s.name) + '</b> <span class="term">' + esc(s.const || '') +
               ' &middot; ' + s.sec.toFixed(1) + ' &middot; ' + s.gates + ' gates</span>';
    if (!s.stations) return html + '<br><span class="term">no station — cannot be a hub</span>';
    html += '<br>score <b>' + s.score.toFixed(0) + '</b>' +
            (s.school ? ' <span class="term">(a starter system)</span>' : '') +
            '<br>' + isk(s.traded) + ' traded here lately' +
            '<br><span class="term">traffic ' + s.traffic.toFixed(0) +
            ' &middot; lanes ' + s.lane.toFixed(0) +
            ' &middot; market ' + s.market.toFixed(0) +
            ' &middot; flow ' + s.flow.toFixed(0) +
            ' &middot; docks ' + (s.stationT >= 0 ? '+' : '') + s.stationT.toFixed(0) +
            ' &middot; topo ' + (s.topo >= 0 ? '+' : '') + s.topo.toFixed(0) +
            ' &middot; sec ' + s.secT.toFixed(0) + '</span>' +
            '<br><span class="term">' + s.orders + ' player/bot orders, ' +
            isk(s.isk) + ' on the book</span>';
    if (s.homes || s.factories) {
      html += '<br><span style="color:#6ee7b7">' +
              (s.homes ? s.homes + ' home ' : '') +
              (s.factories ? s.factories + ' factory' : '') + '</span>';
    }
    return html;
  }

  function draw() {
    if (!data) return;
    var minScore = +minR.value;
    var byID = {}, laneAt = {};
    data.systems.forEach(function (s) { byID[s.id] = s; });
    // The roster's own numbers, keyed by pair, so a stargate can carry the
    // traffic over it in its own tooltip rather than in a second line the
    // operator has to find.
    (data.lanes || []).forEach(function (l) {
      var k = l.a < l.b ? l.a + ':' + l.b : l.b + ':' + l.a;
      var cur = laneAt[k] || {jumps:0, hauls:0, isk:0, w:0};
      cur.jumps += l.jumps; cur.hauls += l.hauls; cur.isk += l.isk;
      cur.w = Math.max(cur.w, l.w);
      laneAt[k] = cur;
    });

    clear();
    var gGates = layer(), gLanes = layer(), gHits = layer(), gSys = layer(), gPilots = layer();

    function hoverable(g, a, b, gate) {
      var k = a.id < b.id ? a.id + ':' + b.id : b.id + ':' + a.id;
      var t = laneAt[k] || {jumps:0, hauls:0, isk:0};
      roadHit(g, a, b, {aName:a.name, bName:b.name, gate:gate,
                        jumps:t.jumps, hauls:t.hauls, isk:t.isk});
    }

    var drawn = {};
    if (gatesOn.checked) {
      (data.gates || []).forEach(function (g) {
        var a = byID[g.a], b = byID[g.b];
        if (!a || !b) return;
        road(gGates, a, b, '#5a463d', 1.6, 0.95, '');
        drawn[a.id < b.id ? a.id + ':' + b.id : b.id + ':' + a.id] = true;
        hoverable(gHits, a, b, true);
      });
    }
    if (lanesOn.checked) {
      (data.lanes || []).forEach(function (l) {
        var a = byID[l.a], b = byID[l.b];
        if (!a || !b) return;
        var k = a.id < b.id ? a.id + ':' + b.id : b.id + ':' + a.id;
        road(gLanes, a, b, l.hauls > 0 ? '#e0a94a' : '#5b86ad',
             (1 + l.w * 4).toFixed(2), (0.35 + l.w * 0.6).toFixed(2),
             drawn[k] ? '' : '7 6');
        if (!drawn[k]) { drawn[k] = true; hoverable(gHits, a, b, false); }
      });
    }

    data.systems.forEach(function (s) {
      var x = px(s), y = py(s);
      // A stationless system is drawn whatever the filter says: it is the
      // road between two that are not, and hiding it breaks the graph into
      // islands that are not there. A filtered-out one becomes a stud for
      // the same reason.
      if (s.stations && s.score < minScore) {
        hit(rect(gSys, x-7, y-7, 14, 14, '#1a1214', '#3a2c26', 1, 2), '__sys', s);
        return;
      }
      var anchored = (s.homes + s.factories) > 0;
      var c = s.stations ? rampRGB(s.score) : [58,51,46];
      if (!namesOn.checked) {
        hit(rect(gSys, x-10, y-10, 20, 20, mix(c, s.stations ? 0.8 : 0.4),
                 anchored ? '#6ee7b7' : mix(c, 1), anchored ? 1.5 : 1, 3), '__sys', s);
        return;
      }
      var box = hit(rect(gSys, x-CW/2, y-CH/2, CW, CH, mix(c, s.stations ? 0.17 : 0.1),
                         anchored ? '#6ee7b7' : mix(c, 0.55), anchored ? 1.5 : 1, 3),
                    '__sys', s);
      if (!s.stations) box.setAttribute('stroke-dasharray', '4 3');
      rect(gSys, x-CW/2+3, y-CH/2+3, 4, CH-6, s.stations ? rgb(c) : '#2f2a26', null, 0, 2);
      text(gSys, x-CW/2+13, y-4, s.name, 12, s.stations ? '#f2e9df' : '#8a807a', null, CW-46);
      text(gSys, x+CW/2-8, y-4, s.sec.toFixed(1), 10, secColor(s.sec), 'end');
      if (s.stations) {
        text(gSys, x-CW/2+13, y+11, s.score.toFixed(0), 11, rgb(c));
        text(gSys, x+CW/2-8, y+11,
             s.orders ? s.orders + ' ord' : '', 9, '#7e746c', 'end');
      } else {
        text(gSys, x-CW/2+13, y+11, 'no station', 9, '#6b625c');
      }
    });

    if (pilotsOn.checked && data.pilots) {
      // Pilots ride under their chip in a row rather than fanned round it:
      // on a lattice there is always a gap below a cell, and a ring of dots
      // reaches into the road running past.
      var perSys = {};
      data.pilots.forEach(function (p) { (perSys[p.sys] = perSys[p.sys] || []).push(p); });
      Object.keys(perSys).forEach(function (sysID) {
        var s = byID[sysID];
        if (!s) return;
        var group = perSys[sysID], shown = Math.min(group.length, 9);
        var y = py(s) + CH/2 + 6, x0 = px(s) - (shown - 1) * 4;
        group.slice(0, shown).forEach(function (p, i) {
          var c = document.createElementNS(SVGNS, 'circle');
          c.setAttribute('cx', x0 + i * 8); c.setAttribute('cy', y);
          c.setAttribute('r', p.bot ? 2.6 : 3.2);
          c.setAttribute('fill', !p.bot ? '#7fb8e6' : (p.trading ? '#6ee7b7' : '#e06a6a'));
          c.setAttribute('stroke', '#0b0708'); c.setAttribute('stroke-width', '0.6');
          c.__pilot = p;
          gPilots.appendChild(c);
        });
        if (group.length > shown) {
          text(gPilots, x0 + shown * 8, y + 3, '+' + (group.length - shown), 9, '#93887f');
        }
      });
    }
  }

  function load() {
    if (!live.checked || document.hidden) return;
    fetch('/rolebots/trademap/api?region=' + regionID + (pilotsOn.checked ? '&pilots=1' : ''),
          {credentials: 'same-origin'})
      .then(function (r) { return r.json(); })
      .then(function (d) {
        data = d;
        GRID = {cols: d.cols || 1, rows: d.rows || 1};
        setExtent(d.systems);
        if (!fitted) { fit(); fitted = true; }
        draw();
        note.textContent = d.systems.length + ' systems, ' + (d.gates || []).length +
          ' gates, ' + (d.lanes || []).length + ' lanes' +
          (d.pilots ? ', ' + d.pilots.length + ' pilots' : '') + ' — ' + (d.now || '');
        dot.className = 'rb-dot';
      })
      .catch(function () { dot.className = 'rb-dot off'; note.textContent = 'update failed'; });
  }
  [gatesOn, lanesOn, namesOn].forEach(function (el) { el.addEventListener('change', draw); });
  pilotsOn.addEventListener('change', load);
  minR.addEventListener('input', function () { minV.textContent = minR.value; draw(); });
  live.addEventListener('change', function () {
    dot.className = live.checked ? 'rb-dot' : 'rb-dot off';
    if (live.checked) load();
  });
  setView();
  load();
  setInterval(load, 5000);
})();
</script>
{{end}}`

// feedRowStyle is the ledger row's chrome, shared with the system page.
const feedRowStyle = `<style>
.rb-t { display:grid; grid-template-columns:64px 70px 116px minmax(0,1.3fr) 58px 74px 74px minmax(0,1.1fr);
        gap:8px; align-items:baseline; padding:5px 8px; border-bottom:1px solid #1e1414;
        border-left:3px solid transparent; font-size:12px; }
.rb-t:hover { background:#1a1010; }
.rb-t .t { color:#6b7280; font-family:ui-monospace,Consolas,monospace; font-size:11px; }
.rb-t .who { color:#9ca3af; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.rb-t .what { color:#e6ded6; overflow-wrap:anywhere; }
.rb-t .what .note { color:#6b7280; font-size:11px; }
.rb-t .n { text-align:right; font-variant-numeric:tabular-nums; color:#c9c1ba; }
.rb-t .n.up { color:#6ee7b7; } .rb-t .n.down { color:#fca5a5; }
.rb-t .where { color:#9ca3af; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.rb-t .where .sys { color:#6b7280; }
.rb-t.rb-new { animation:rbflash 1.6s ease-out; }
@keyframes rbflash { from { background:#123a25; } to { background:transparent; } }
.rb-k { display:inline-block; width:100%; text-align:center; padding:1px 0; border-radius:4px;
        font-size:10px; letter-spacing:.04em; text-transform:uppercase; background:#241a1a; color:#9ca3af; }
.rb-k[data-k="buy"]    { background:#132433; color:#7fb8e6; }
.rb-k[data-k="sell"]   { background:#10261c; color:#6ee7b7; }
.rb-k[data-k="haul"]   { background:#10261c; color:#9ee7c7; }
.rb-k[data-k="list"]   { background:#26210f; color:#e0c068; }
.rb-k[data-k="fill"]   { background:#1c3320; color:#8fe0a0; }
.rb-k[data-k="price"]  { background:#231a2c; color:#c4a5f5; }
.rb-k[data-k="cancel"] { background:#331a1a; color:#e08a8a; }
.rb-k[data-k="fee"]    { background:#2b1a26; color:#e0a5c8; }
.rb-head { color:#6b7280; font-size:10px; letter-spacing:.05em; text-transform:uppercase;
           border-bottom:1px solid #2a1a1a; }
.rb-pager { display:flex; gap:14px; align-items:center; margin-top:10px; font-size:12px; color:#6b7280; }
.rb-pager a { color:#e0a94a; text-decoration:none; }
.rb-pager a:hover { text-decoration:underline; }
</style>`

const tmplTrades = tradeTabs + `{{define "content"}}
` + tradeTabsStyle + feedRowStyle + `
<style>
.rb-live { display:flex; align-items:center; gap:10px; font-size:12px; color:#9ca3af; flex-wrap:wrap; }
.rb-dot { width:8px; height:8px; border-radius:50%; background:#22c55e; display:inline-block; }
.rb-dot.off { background:#6b7280; }

.rb-tot { display:grid; grid-template-columns:repeat(auto-fit,minmax(130px,1fr)); gap:10px; }
.rb-tot div { border:1px solid #2a1a1a; border-radius:6px; padding:8px 10px; background:#180f0f; }
.rb-tot .k { font-size:10px; letter-spacing:.05em; text-transform:uppercase; color:#6b7280; }
.rb-tot .v { font-size:17px; color:#e6ded6; font-variant-numeric:tabular-nums; }
.rb-tot .v small { font-size:11px; color:#6b7280; font-weight:normal; }
.rb-tot .v.up { color:#6ee7b7; } .rb-tot .v.down { color:#fca5a5; }

.rb-feed { max-height:calc(100vh - 380px); overflow-y:auto; border-top:1px solid #2a1a1a; }
</style>

{{template "rb_tradetabs" .}}
<h2>Trade Feed</h2>

<div class="card">
  <p style="margin:0 0 10px;color:#9ca3af;font-size:12px">
    Every movement of goods the roster makes, and nothing else — this is the trade book, not the
    activity log. A <b>listing</b> commits an order slot and pays nothing until somebody takes it;
    the <b>fill</b> that follows, whenever it comes, is where that money shows up. Profit is what
    reached the wallet: every sale is booked net of its sales tax, and the <b>broker fee</b> a
    listing cost is its own line, charged where the ISK actually left — at placement, whether or
    not the order ever fills. The figures are refreshed every minute; the ledger is paged.
  </p>
  <div class="rb-tot">
    <div><div class="k">bought</div><div class="v">{{.Summary.Day.Bought}} <small>24h</small></div><div class="k">{{.Summary.Week.Bought}} 7d</div></div>
    <div><div class="k">sold</div><div class="v">{{.Summary.Day.Sold}} <small>24h</small></div><div class="k">{{.Summary.Week.Sold}} 7d</div></div>
    <div><div class="k">profit</div><div class="v up">{{.Summary.Day.Profit}} <small>24h</small></div><div class="k">{{.Summary.Week.Profit}} 7d</div></div>
    <div><div class="k">runs</div><div class="v">{{.Summary.Day.Runs}} <small>24h</small></div><div class="k">{{.Summary.Week.Runs}} 7d</div></div>
    <div><div class="k">order fills</div><div class="v">{{.Summary.Day.Fills}} <small>24h</small></div><div class="k">{{.Summary.Week.Fills}} 7d</div></div>
    <div><div class="k">resting orders</div><div class="v">{{.Summary.Day.Orders}}</div><div class="k">live now</div></div>
    <div><div class="k">value resting</div><div class="v">{{.Summary.Day.Resting}}</div><div class="k">live now</div></div>
  </div>
</div>

<div class="card">
  <form method="GET" action="/rolebots/trades" style="display:flex;gap:10px;align-items:end;flex-wrap:wrap;margin-bottom:10px">
    <label>Kind
      <select name="kind">
        <option value="">everything</option>
        {{$k := .Kind}}
        {{range .Kinds}}<option value="{{.Key}}"{{if eq .Key $k}} selected{{end}}>{{.Label}}</option>{{end}}
      </select>
    </label>
    <label>Pilot <input type="text" name="who" value="{{.Who}}" placeholder="pilot name" autocomplete="off"></label>
    <label>Text <input type="text" name="q" value="{{.Text}}" placeholder="item or note"></label>
    <button type="submit">Filter</button>
    <a href="/rolebots/trades" style="font-size:12px;color:#9ca3af">clear</a>
    <div class="rb-live" style="margin-left:auto">
      <label><input type="checkbox" id="rbLive"> <span class="rb-dot off" id="rbDot"></span> live</label>
      <span id="rbNote">paused &middot; tick to poll every 15s while this tab is visible</span>
    </div>
  </form>

  <div class="rb-t rb-head">
    <span>time</span><span>kind</span><span>pilot</span><span>what</span>
    <span style="text-align:right">qty</span><span style="text-align:right">unit</span>
    <span style="text-align:right">profit</span><span>station</span>
  </div>
  <div class="rb-feed" id="rbFeed">
    {{range .Rows}}
    <div class="rb-t" data-id="{{.ID}}">
      <span class="t" title="{{.Full}}">{{.When}}</span>
      <span><span class="rb-k" data-k="{{.Kind}}">{{.Kind}}</span></span>
      <span class="who"><a href="/rolebot/{{.CharID}}">{{.Who}}</a></span>
      <span class="what">{{.TypeName}}<span class="note"> — {{.Note}}</span></span>
      <span class="n">{{.Qty}}</span>
      <span class="n">{{.Price}}</span>
      <span class="n {{if gt .Profit 0.0}}up{{else if lt .Profit 0.0}}down{{end}}">{{.ProfitS}}</span>
      <span class="where" title="{{if .From}}from {{.From}} — {{end}}{{.Station}}{{if .Where}} ({{.Where}}){{end}}">
        {{if .From}}<span class="sys">{{.From}} &rarr;</span> {{end}}{{if .Station}}{{.Station}}{{else}}{{.Where}}{{end}}{{if and .Station .Where}} <span class="sys">&middot; {{.Where}}</span>{{end}}</span>
    </div>
    {{end}}
    {{if not .Rows}}<p style="color:#9ca3af;font-size:12px;padding:8px">{{if .Paged}}Nothing further this way.{{else}}Nothing in the book matches.{{end}}</p>{{end}}
  </div>
  <div class="rb-pager">
    {{if .Newer}}<a href="{{.Newer}}">&larr; newer</a>{{end}}
    {{if .Older}}<a href="{{.Older}}">older &rarr;</a>{{end}}
    {{if .Paged}}<a href="/rolebots/trades{{if .Query}}?{{.Query}}{{end}}">latest</a>{{end}}
  </div>
</div>

<script>
(function () {
  var feed = document.getElementById('rbFeed'), live = document.getElementById('rbLive');
  var dot = document.getElementById('rbDot'), note = document.getElementById('rbNote');
  var query = {{.Query}};
  var paged = {{.Paged}};
  var lastID = 0;
  var first = feed.querySelector('.rb-t[data-id]');
  if (first) lastID = +first.getAttribute('data-id');
  var KEY = 'rb.feed.live';
  try { if (!paged && localStorage.getItem(KEY) === '1') live.checked = true; } catch (e) {}

  function esc(s) { return (s || '').replace(/[&<>"]/g, function (c) {
    return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]; }); }

  function addRow(r) {
    if (feed.querySelector('.rb-t[data-id="' + r.id + '"]')) return;
    var el = document.createElement('div');
    el.className = 'rb-t rb-new';
    el.setAttribute('data-id', r.id);
    var cls = r.profitRaw > 0 ? ' up' : (r.profitRaw < 0 ? ' down' : '');
    el.innerHTML =
      '<span class="t" title="' + esc(r.full) + '">' + esc(r.when) + '</span>' +
      '<span><span class="rb-k" data-k="' + esc(r.kind) + '">' + esc(r.kind) + '</span></span>' +
      '<span class="who"><a href="/rolebot/' + r.charID + '">' + esc(r.who) + '</a></span>' +
      '<span class="what">' + esc(r.typeName) + '<span class="note"> — ' + esc(r.note) + '</span></span>' +
      '<span class="n">' + r.qty + '</span>' +
      '<span class="n">' + esc(r.price) + '</span>' +
      '<span class="n' + cls + '">' + esc(r.profit) + '</span>' +
      '<span class="where">' + (r.from ? '<span class="sys">' + esc(r.from) + ' &rarr;</span> ' : '') +
        esc(r.station || r.where) +
        (r.station && r.where ? ' <span class="sys">&middot; ' + esc(r.where) + '</span>' : '') + '</span>';
    feed.insertBefore(el, feed.firstChild);
    while (feed.children.length > 300) feed.removeChild(feed.lastChild);
  }

  function poll() {
    // Only while asked to, only while looked at, and never on an older page:
    // a page scrolled back in time has nothing newer to prepend.
    if (!live.checked || document.hidden || paged) return;
    fetch('/rolebots/trades/api?after=' + lastID + (query ? '&' + query : ''),
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
