package rolebots

// The Mineral Supply page's markup. Split from its handler the way
// esptrade.go / esptrade_tmpl.go are.

// industryTabs must be concatenated in with its style, since the page body calls {{template "rb_indtabs" .}}.
// The style goes inside {{define "content"}} like every sibling's, because markup outside a define is discarded.
// espminerals_render_test.go catches a mismatch between the concatenated constant and the call below.
const tmplMinerals = industryTabs + `{{define "content"}}
` + industryTabsStyle + `<style>
.rb-min td, .rb-min th { font-size:12px; padding:4px 6px; }
.rb-min th.rot { text-align:center; white-space:nowrap; }
.rb-min td.cell { text-align:center; min-width:74px; }
.rb-bid { color:#86efac; }
.rb-impname { color:#a78bfa; }
.rb-none { color:#4b5563; }
.rb-num { color:#9ca3af; font-variant-numeric:tabular-nums; }
</style>
<h2>Mineral Supply</h2>
{{template "rb_indtabs" .}}
{{if .Msg}}<div class="alert">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="card">
  <p style="font-size:12px;color:#9ca3af;margin:0 0 6px">
    Nothing tells a miner what to dig and nothing tells a trader what to carry. A miner ranks the rock it
    can reach by what the metal inside is worth <strong>per m³</strong> at the bids on its own markets, and
    flies further only for rock worth more; a trader lists goods no higher than what the destination region
    has actually paid. A factory that is short of a mineral says so by <strong>bidding</strong> for it
    &mdash; the bid raises the mineral's price, which raises the value of every ore that yields it, which is
    what brings miners to that rock.
  </p>
  <p style="font-size:12px;color:#6b7280;margin:0">
    Each mineral cell is <strong>units listed</strong> in the region over its <strong>best standing bid</strong>
    (&mdash; = nobody bidding; ore values then use the catalogue price). <strong>Best rock</strong> is the
    region's own ore ranked by ISK/m³ at those bids &mdash; a pilot's own ranking reads its wider shopping
    range, so it can differ. <strong>Mined</strong> is what its pilots planned trips for in the last hour.
    Busiest regions first.
  </p>
</div>

<div class="card">
  {{if .Rows}}
  <table class="rb-min">
    <tr>
      <th>Region</th><th>Belt sec</th><th>Pilots</th><th>Best rock (ISK/m³)</th><th>Mined, last hour</th>
      {{range .Cascade}}<th class="rot">{{.}}</th>{{end}}
    </tr>
    {{range .Rows}}
    <tr>
      <td><a href="/rolebots/trademap/region/{{.RegionID}}">{{.Name}}</a></td>
      <td class="rb-num">{{if .BandKnown}}{{printf "%.1f" .Band}}{{else}}<span class="rb-none">&mdash;</span>{{end}}</td>
      <td class="rb-num">{{.Pilots}}</td>
      <td>{{if .BestOres}}{{range $i, $o := .BestOres}}{{if $i}}, {{end}}{{$o.Name}} <span class="rb-num">{{printf "%.0f" $o.Value}}</span>{{end}}{{else}}<span class="rb-none">no belts</span>{{end}}</td>
      <td>{{if .Mined}}{{range $i, $o := .Mined}}{{if $i}}, {{end}}{{$o.Name}} <span class="rb-num">&times;{{$o.Trips}}</span>{{end}}{{else}}<span class="rb-none">&mdash;</span>{{end}}</td>
      {{range .Cells}}
      <td class="cell">
        {{if .Listed}}<span class="rb-num">{{.ListedShort}}</span>{{else}}<span class="rb-none">none</span>{{end}}
        <div style="font-size:10px">{{if .Bid}}<span class="rb-bid">bid {{.BidShort}}</span>{{else}}<span class="rb-none">&mdash;</span>{{end}}</div>
      </td>
      {{end}}
    </tr>
    {{end}}
  </table>
  {{else}}
  <p style="color:#6b7280">No region has belts or resident pilots yet.</p>
  {{end}}
</div>

<div class="card">
  <h3 style="margin:0 0 4px">What the factories are asking for</h3>
  <p style="font-size:12px;color:#9ca3af;margin:0 0 8px">
    The table above is <strong>supply</strong> and nothing stores it &mdash; it is the order book as it
    stands. This is <strong>demand</strong>, and it is the one thing on this page anybody wrote
    down: a factory naming an exact shortfall at an exact station, posted when a project <em>commits</em>
    as well as when one gets stuck. <strong>Asking</strong> and <strong>escrowed</strong> are different
    facts and come apart often: a mineral wanted by thirty stations with no ISK behind it is a shortage
    nobody has put money on, and only the money moves a miner &mdash; a bid is a price, and price is what
    it digs for.
    <strong>Famine</strong> is the point past which &ldquo;a pilot could go and dig this&rdquo; stops
    excusing a factory from bidding for it &mdash; measured in that mineral's own lots, so the deep rungs
    can reach it.
  </p>
  {{if .Demand}}
  <table class="rb-min">
    <tr>
      <th>Mineral</th><th>Stations asking</th><th>Units wanted</th><th>Escrowed</th>
      <th>Famine bar</th><th>Listed game-wide</th>
    </tr>
    {{range .Demand}}
    <tr>
      <td>{{.Name}}</td>
      <td class="rb-num">{{if .Stations}}{{.Stations}}{{else}}<span class="rb-none">&mdash;</span>{{end}}</td>
      <td class="rb-num">{{if .Wanted}}{{.Wanted}}{{else}}<span class="rb-none">&mdash;</span>{{end}}</td>
      <td class="rb-num">{{if .Escrow}}{{printf "%.0f" .Escrow}} ISK{{else}}<span class="rb-none">nothing on the counter</span>{{end}}</td>
      <td class="rb-num">{{.Famine}}{{if .Hungry}} <span class="rb-impname" title="over the bar: factories may bid for this even though it is diggable">famine</span>{{end}}</td>
      <td class="rb-num">{{if .Listed}}{{.Listed}}{{else}}<span class="rb-none">none</span>{{end}}</td>
    </tr>
    {{end}}
  </table>
  {{else}}
  <p style="color:#6b7280">No factory has posted a mineral shortfall in the ledger's window.</p>
  {{end}}
</div>

<div class="card">
  <h3 style="margin:0 0 4px">Materials no asteroid answers</h3>
  <p style="font-size:12px;color:#9ca3af;margin:0 0 8px">
    A bill of materials is not all minerals, and the rest of it used to go nowhere &mdash; the mineral
    ledger drops every line that is not one, so a project stuck on a component left no trace a miner or a
    trader could read. These are the non-mineral lines a factory needed and <em>nothing within its reach
    was selling</em>. Nobody can dig them: they are somebody's next production run, or a hauler's.
  </p>
  {{if .Materials}}
  <table class="rb-min">
    <tr><th>Material</th><th>Units wanted</th><th>Times asked</th></tr>
    {{range .Materials}}
    <tr><td>{{.Name}}</td><td class="rb-num">{{.Wanted}}</td><td class="rb-num">{{.Asks}}</td></tr>
    {{end}}
  </table>
  {{else}}
  <p style="color:#6b7280">No non-mineral material has gone unanswered lately.</p>
  {{end}}
</div>
{{end}}`
