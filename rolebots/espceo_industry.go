package rolebots

import (
	"context"
	"fmt"
)

// The CEO page's industry desk card (docs/CORPS.md section 10): the ore desk's stance and why, who runs the
// factory, the library, the work board, and what the corporation is buying and has replaced.

// ceoWorkRow is one work order as the panel shows it.
type ceoWorkRow struct {
	What, Kind, Purpose, Wage string
	Done, Wanted              int
}

// ceoIndustryView is the card's data; nil for a corporation with no desk.
type ceoIndustryView struct {
	Share, Target, Mode, Why string
	Director, Deputy         string
	DirectorHuman            bool
	Factory                  string
	Library                  int
	// Fund is the open print fund (corpfund.go): what it saves toward, how
	// far along, and about how long to go. Empty when none.
	Fund                     string
	Work                     []ceoWorkRow
	Buying                   []string
	POs                      int
	SRPOwed, SRPGranted      int
	WagesWeek                string
	Department               []ceoDeptRow
}

// ceoDeptRow is one Industry department seat holder (corpindseats.go).
type ceoDeptRow struct {
	Name, Title string
	Human       bool
	Days, Perf  string
}

// ceoIndustryView reads the card, or nil when the corporation's tags give it
// no industry desk.
func (m *Mod) ceoIndustryView(ctx context.Context, corpID int64, facts *CorpFacts) *ceoIndustryView {
	if facts == nil || !hasIndustryDesk(facts.Policy.Tags) {
		return nil
	}
	p, err := m.loadCorpPolicy(ctx, corpID)
	if err != nil {
		return nil
	}
	v := &ceoIndustryView{
		Share:  fmt.Sprintf("%.0f%%", p.OreShare*100),
		Target: fmt.Sprintf("%.0f%%", p.OreTarget*100),
		Mode:   oreDeskLabel(p.OreShare),
		Why:    p.OreWhy,
	}
	if p.DirectorID != 0 {
		v.Director = m.charNameOr(ctx, p.DirectorID)
		v.DirectorHuman = !m.isSynthetic(p.DirectorID)
	}
	if p.DeputyID != 0 && p.DeputyID != p.DirectorID {
		v.Deputy = m.charNameOr(ctx, p.DeputyID)
	}
	if st := m.corpFactoryStation(ctx, corpID); st != 0 {
		v.Factory = m.stationLabel(ctx, st)
	}
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_library WHERE corporationID = ?`, corpID).Scan(&v.Library)
	v.Fund = m.fundSummary(ctx, corpID)
	if prog := m.programSummary(ctx, corpID); prog != "" {
		if v.Fund != "" {
			v.Fund += " · "
		}
		v.Fund += "program: " + prog
	}
	if ws, err := m.openWorkFor(ctx, corpID); err == nil {
		for _, w := range ws {
			what := m.typeNameOr(ctx, w.ProductTypeID)
			if w.Kind != workKindBuild {
				what = m.typeNameOr(ctx, w.BPTypeID)
			}
			v.Work = append(v.Work, ceoWorkRow{What: what, Kind: w.Kind, Purpose: w.Purpose,
				Wage: iskString(w.WagePerRun), Done: w.RunsDone, Wanted: w.RunsWanted})
		}
	}
	v.Buying = m.buybackSummary(ctx, corpID)
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_po WHERE corporationID = ? AND status = 'open'`, corpID).Scan(&v.POs)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(status = 'owed'), 0), COALESCE(SUM(status = 'granted'), 0)
		  FROM rolebot_corp_srp WHERE corporationID = ?`, corpID).Scan(&v.SRPOwed, &v.SRPGranted)
	var wages float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(wage), 0) FROM rolebot_corp_wages WHERE corporationID = ? AND paidAt > ?`,
		corpID, clock.Unix()-7*86400).Scan(&wages)
	v.WagesWeek = iskString(wages)
	for _, r := range m.industryDepartment(ctx, corpID) {
		v.Department = append(v.Department, ceoDeptRow{Name: r.Name, Title: r.Title, Human: r.Human,
			Days: fmt.Sprintf("%.0f", r.Days), Perf: fmt.Sprintf("%.2f", r.Perf)})
	}
	return v
}

// tmplCEOIndustry is the card, included in the CEO page.
const tmplCEOIndustry = `{{with .Industry}}
<div class="card">
  <h3>Industry desk</h3>
  <p><b>Ore desk:</b> {{.Mode}} — keeps {{.Share}} of the ore it takes in for production (heading for {{.Target}}).
  {{if .Why}}<span style="color:#6b7280">{{.Why}}</span>{{end}}</p>
  <p><b>Director of Industry:</b> {{if .Director}}{{.Director}}{{if .DirectorHuman}} (player){{end}}{{else}}nobody yet{{end}}
  {{if .Deputy}} · <b>deputy</b> {{.Deputy}} flies the chores{{end}}
  · <b>Factory:</b> {{if .Factory}}{{.Factory}}{{else}}none — no office at a station with lines{{end}}
  · <b>Library:</b> {{.Library}} print(s){{if .POs}} · {{.POs}} on order{{end}}</p>
  {{if .Fund}}<p><b>Print fund:</b> {{.Fund}}</p>{{end}}
  <p><b>Work credited this week:</b> {{.WagesWeek}} · <b>Ships replaced:</b> {{.SRPGranted}}{{if .SRPOwed}} ({{.SRPOwed}} owed){{end}}</p>
  {{if .Work}}
  <table class="tbl" style="font-size:12px">
    <tr><th>Work order</th><th>Kind</th><th>For</th><th>Done</th><th>Wage</th></tr>
    {{range .Work}}<tr><td>{{.What}}</td><td>{{.Kind}}</td><td>{{.Purpose}}</td><td>{{.Done}} / {{.Wanted}}</td><td>{{.Wage}}</td></tr>{{end}}
  </table>
  {{else}}<p>The work board is empty.</p>{{end}}
  {{if .Buying}}<h4 style="margin-top:10px">Buying</h4>
  <p style="font-size:12px">{{range .Buying}}{{.}}<br>{{end}}</p>{{end}}
  <h4 style="margin-top:10px">Industry department</h4>
  {{if .Department}}<table class="tbl" style="font-size:12px">
    <tr><th>Member</th><th>Seat</th><th>Days</th><th>Rating</th></tr>
    {{range .Department}}<tr><td>{{.Name}}{{if .Human}} (player){{end}}</td><td>{{.Title}}</td><td>{{.Days}}</td><td>{{.Perf}}</td></tr>{{end}}
  </table>{{else}}<p>No seats dealt yet.</p>{{end}}
</div>
{{end}}`
