package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// The Staff panel on a CEO page (espceo.go): who holds each office, who
// actually does its work, how the CEO last rated them, what they did this
// week, and the standing orders the department heads have out.

type ceoStaffRow struct {
	CharID   int64
	Name     string
	Bot      bool
	Office   string
	Executor string // "" when the holder does the work itself
	Perf     string
	Reviewed string
	Week     int // ledger lines it acted on in the last 7 days
}

type ceoOrderRow struct {
	Dept    string
	Target  string
	Reason  string
	By      string
	Expires string
}

type ceoDemandRow struct {
	Dept  string
	Share string
}

type ceoStaffViewData struct {
	Rows    []ceoStaffRow
	Orders  []ceoOrderRow
	Demand  []ceoDemandRow
	War     bool
	Missing []string // departments the corporation needs and nobody runs
}

func (m *Mod) ceoStaffView(ctx context.Context, c *corpCEO) *ceoStaffViewData {
	s, err := m.loadCorpStaff(ctx, c.CorpID, c.CharID)
	if err != nil {
		return nil
	}
	v := &ceoStaffViewData{}
	since := clock.Unix() - staffReviewEverySec
	for _, sm := range s.all() {
		office := staffOffice(sm)
		row := ceoStaffRow{CharID: sm.CharID, Name: sm.Name, Bot: !sm.Human, Office: office,
			Perf: fmt.Sprintf("%.2f", sm.Perf), Reviewed: "not yet"}
		if sm.PerfAt > 0 {
			row.Reviewed = fmtDuration(clock.Unix()-sm.PerfAt) + " ago"
		}
		if ex := s.executor(sm, office); ex.CharID != sm.CharID {
			row.Executor = ex.Name + " (CEO)"
		}
		row.Week = m.actorActions(ctx, c.CorpID, sm.CharID, since)
		v.Rows = append(v.Rows, row)
	}
	sort.SliceStable(v.Rows, func(i, j int) bool { return v.Rows[i].Office < v.Rows[j].Office })

	for _, o := range m.listFleetOrders(ctx, c.CorpID) {
		target := strings.Join(o.OreNames, ", ")
		if o.SystemID != 0 {
			target = m.nameOfSystem(ctx, o.SystemID)
		}
		v.Orders = append(v.Orders, ceoOrderRow{
			Dept: portfolioLabel(o.Dept), Target: target, Reason: o.Reason,
			By: m.charNameOr(ctx, o.PostedBy), Expires: "in " + fmtDuration(o.ExpiresAt-clock.Unix()),
		})
	}
	demand, war := corpSeatDemand(c.CorpID)
	v.War = war
	for _, port := range []string{portOperations, portSecurity, portCommerce} {
		if d, ok := demand[port]; ok {
			v.Demand = append(v.Demand, ceoDemandRow{Dept: portfolioLabel(port), Share: fmt.Sprintf("%.0f%%", d*100)})
		}
	}
	if facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID); err == nil {
		for _, port := range append(neededPortfolios(facts.Policy), m.diplomacyPortfolio(ctx, c.CorpID, facts.Members)...) {
			if s.head(port) == nil {
				v.Missing = append(v.Missing, portfolioLabel(port))
			}
		}
	}
	return v
}

const tmplCEOStaff = `{{with .Staff}}
<div class="card">
  <h3>Staff — the officers who do the work</h3>
  {{if .Rows}}
  <table class="tbl" style="font-size:12px">
    <tr><th>Office</th><th>Holder</th><th>Does the work</th><th>Rating</th><th>Reviewed</th><th>Acted (7d)</th></tr>
    {{range .Rows}}<tr><td>{{.Office}}</td>
      <td>{{if .Bot}}<a href="/rolebot/{{.CharID}}">{{.Name}}</a>{{else}}{{.Name}} <span style="color:#6b7280">(player)</span>{{end}}</td>
      <td>{{if .Executor}}{{.Executor}}{{else}}themselves{{end}}</td>
      <td>{{.Perf}}</td><td>{{.Reviewed}}</td><td>{{.Week}}</td></tr>{{end}}
  </table>
  {{else}}<p>No officers yet — the CEO runs everything itself until the corporation has five members.</p>{{end}}
  {{if .Missing}}<p style="font-size:12px;color:#b45309">No director runs: {{range $i, $d := .Missing}}{{if $i}}, {{end}}{{$d}}{{end}} — the CEO covers it.</p>{{end}}
  {{if .Orders}}
  <h4 style="margin:10px 0 4px">Standing fleet orders</h4>
  <table class="tbl" style="font-size:12px">
    <tr><th>Department</th><th>Target</th><th>Why</th><th>Posted by</th><th>Lapses</th></tr>
    {{range .Orders}}<tr><td>{{.Dept}}</td><td>{{.Target}}</td><td>{{.Reason}}</td><td>{{.By}}</td><td>{{.Expires}}</td></tr>{{end}}
  </table>
  {{end}}
  {{if or .Demand .War}}<p style="font-size:12px;color:#6b7280">Seat demand:
    {{range .Demand}}{{.Dept}} {{.Share}} · {{end}}{{if .War}}<b>war footing</b> — extra combat seats{{end}}</p>{{end}}
  <p style="font-size:12px;color:#6b7280">The CEO sets policy; its officers carry it out every 15 minutes. The Treasurer pays
  stipends, skillbooks, salary and dividends. Recruitment Officers answer applications and headhunt commanders. Directors run
  a department each: Operations orders what the mining fleets dig, Security where the combat wing sweeps, Commerce the
  corporation's market business, Diplomacy (part-time, every several hours) the corporation's relations and letters.
  A post held by a player is covered by the CEO in their name.</p>
</div>
{{end}}`
