package rolebots

import (
	"context"
	"sort"
)

// The Research card on a player corporation's page: the library's copies, what is up for sale and how it
// went over the week, the research lines of the books, and the copy tape for the types on the shelf
// (corpcopytape.go). Shown only for a house with an R&D arm or a copy shelf.

const researchCardTypes = 12

type ceoResearchType struct {
	Name               string
	Shelf, Sold7, Open int
	Room               int
	PerRun             string
	Saturated          bool
}

type ceoResearchView struct {
	Arm                int
	Shelf              int
	Wanted, Listed     int
	Sold7, Returned7   int
	SalesIn, FeesOut   string
	BPOOut, JobFeesOut string
	Net                string
	NetNeg             bool
	Types              []ceoResearchType
}

func (m *Mod) ceoResearchView(ctx context.Context, corpID int64) *ceoResearchView {
	v := &ceoResearchView{Arm: armCount(corpID, armResearch)}
	copies, _ := m.libraryCopiesAll(ctx, corpID)
	v.Shelf = len(copies)
	if v.Arm == 0 && v.Shelf == 0 {
		return nil
	}
	now := clock.Unix()
	week := now - 7*86400
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(status = ?), 0), COALESCE(SUM(status = ?), 0), COALESCE(SUM(returnedAt > ?), 0)
		  FROM rolebot_corp_print_sales WHERE corporationID = ?`,
		printSaleWanted, printSaleListed, week, corpID).Scan(&v.Wanted, &v.Listed, &v.Returned7)

	var salesIn, feesOut, bpoOut, jobFees float64
	if rows, err := m.db.QueryContext(ctx, `
		SELECT line, COALESCE(SUM(IF(amount > 0, amount, 0)), 0), COALESCE(SUM(IF(amount < 0, -amount, 0)), 0),
		       COALESCE(SUM(amount > 0), 0)
		  FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND at > ? AND line IN (?, ?, ?)
		 GROUP BY line`, corpID, week, lineResearchSales, lineBPO, lineJobFees); err == nil {
		for rows.Next() {
			var line string
			var in, out float64
			var n int
			if rows.Scan(&line, &in, &out, &n) != nil {
				continue
			}
			switch line {
			case lineResearchSales:
				salesIn, feesOut, v.Sold7 = in, out, n
			case lineBPO:
				bpoOut = out
			case lineJobFees:
				jobFees = out
			}
		}
		rows.Close()
	}
	v.SalesIn, v.FeesOut, v.BPOOut, v.JobFeesOut = iskString(salesIn), iskString(feesOut), iskString(bpoOut), iskString(jobFees)
	net := salesIn - feesOut - bpoOut - jobFees
	v.Net, v.NetNeg = iskString(net), net < 0

	shelf := map[int64]int{}
	for _, c := range copies {
		shelf[c.BPTypeID]++
	}
	tapes, _ := m.copyTapes(ctx)
	for t, n := range shelf {
		tape := tapes[t]
		row := ceoResearchType{Name: m.typeNameOr(ctx, t), Shelf: n, Sold7: tape.Sold7, Open: tape.Open,
			Room: tapeRoom(tape), Saturated: tapeRoom(tape) == 0}
		if tape.PerRun > 0 {
			row.PerRun = iskString(tape.PerRun)
		}
		v.Types = append(v.Types, row)
	}
	sort.Slice(v.Types, func(i, j int) bool {
		if v.Types[i].Shelf != v.Types[j].Shelf {
			return v.Types[i].Shelf > v.Types[j].Shelf
		}
		return v.Types[i].Name < v.Types[j].Name
	})
	if len(v.Types) > researchCardTypes {
		v.Types = v.Types[:researchCardTypes]
	}
	return v
}

const tmplCEOResearch = `
{{with .Research}}
<div class="card">
  <h3>Research</h3>
  <p style="font-size:12px">R&amp;D arm: {{.Arm}} &middot; copies on the shelf: {{.Shelf}} &middot; marked for sale: {{.Wanted}} &middot; listed: {{.Listed}}
   &middot; this week: {{.Sold7}} sold, {{.Returned7}} came back</p>
  <table class="tbl" style="font-size:12px"><tr><th>7 days</th><th>Amount</th></tr>
    <tr><td>Copy sales</td><td>{{.SalesIn}}</td></tr>
    <tr><td>Listing fees</td><td>{{.FeesOut}}</td></tr>
    <tr><td>Blueprints bought</td><td>{{.BPOOut}}</td></tr>
    <tr><td>Job fees (all jobs)</td><td>{{.JobFeesOut}}</td></tr>
    <tr><td><b>Net</b></td><td>{{if .NetNeg}}<b style="color:#ef4444">{{.Net}}</b>{{else}}<b>{{.Net}}</b>{{end}}</td></tr>
  </table>
  {{if .Types}}<h4>Copy tape for the shelf</h4>
  <table class="tbl" style="font-size:12px"><tr><th>Print</th><th>On shelf</th><th>Sold 7d (all bots)</th><th>Price per run</th><th>Listed now</th><th>Room</th></tr>
    {{range .Types}}<tr><td>{{.Name}}</td><td>{{.Shelf}}</td><td>{{.Sold7}}</td><td>{{if .PerRun}}{{.PerRun}}{{else}}&ndash;{{end}}</td><td>{{.Open}}</td>
      <td>{{if .Saturated}}<b style="color:#ef4444">full</b>{{else}}{{.Room}}{{end}}</td></tr>{{end}}
  </table>{{end}}
</div>
{{end}}
`
