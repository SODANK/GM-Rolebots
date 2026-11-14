package rolebots

import (
	"context"
	"fmt"
)

// The corporation page's War chest card (corpdivisions.go, corparmory.go):
// which hangar divisions the Armory, Fleet Yard and Spoils borrowed, the kit
// shelf, who holds a kit right now, and who is barred from it.

type ceoKitRow struct {
	Kind, Seat, Hull, Where, Status, Value string
}

type ceoIssueRow struct {
	Pilot, Hull, Kind, Since, Due string
	Human, Overdue                bool
}

type ceoBarredRow struct {
	Pilot, Reason, Value, Until string
}

type ceoArmoryView struct {
	Divisions []string
	Kits      []ceoKitRow
	Issues    []ceoIssueRow
	Barred    []ceoBarredRow
	Lost7d    int
}

// ceoArmoryView reads the card; nil when the corporation has no war chest
// and has never lent anything.
func (m *Mod) ceoArmoryView(ctx context.Context, corpID int64) *ceoArmoryView {
	p := m.corpDivs(ctx, corpID)
	v := &ceoArmoryView{}
	for _, d := range p.guardedDivs() {
		v.Divisions = append(v.Divisions, fmt.Sprintf("division %d: %s", d, p.Label(d)))
	}
	if kits, err := m.corpKits(ctx, corpID, ""); err == nil {
		for _, k := range kits {
			v.Kits = append(v.Kits, ceoKitRow{Kind: kitLabel(k.Kind), Seat: seatRoleLabel(k.SeatRole),
				Hull: m.typeNameOr(ctx, k.HullTypeID), Where: m.stationLabel(ctx, k.StationID),
				Status: k.Status, Value: iskString(k.Value)})
		}
	}
	now := clock.Unix()
	if rows, err := m.db.QueryContext(ctx, `SELECT `+issueCols+` FROM rolebot_corp_issue
		WHERE corporationID = ? AND status = ? ORDER BY issueID`, corpID, issueIssued); err == nil {
		var list []corpIssue
		for rows.Next() {
			if is, err := scanIssue(rows); err == nil {
				list = append(list, is)
			}
		}
		rows.Close()
		for _, is := range list {
			v.Issues = append(v.Issues, ceoIssueRow{Pilot: m.charNameOr(ctx, is.CharID),
				Hull: m.typeNameOr(ctx, is.HullTypeID), Kind: kitLabel(is.Kind),
				Since: fmtDuration(max(0, now-is.IssuedAt)), Due: fmtDuration(max(0, is.DueAt-now)),
				Human: is.Human, Overdue: now > is.DueAt})
		}
	}
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_corp_issue
		WHERE corporationID = ? AND status = ? AND closedAt >= ?`, corpID, issueLost, now-7*86400).Scan(&v.Lost7d)
	if rows, err := m.db.QueryContext(ctx, `SELECT characterID, reason, value, untilAt FROM rolebot_corp_barred
		WHERE corporationID = ? ORDER BY barredAt DESC LIMIT 20`, corpID); err == nil {
		type bar struct {
			char      int64
			reason    string
			value     float64
			untilAt   int64
		}
		var list []bar
		for rows.Next() {
			var b bar
			if rows.Scan(&b.char, &b.reason, &b.value, &b.untilAt) == nil {
				list = append(list, b)
			}
		}
		rows.Close()
		for _, b := range list {
			until := "for good"
			if b.untilAt != 0 {
				if b.untilAt < now {
					continue
				}
				until = fmtDuration(b.untilAt - now)
			}
			v.Barred = append(v.Barred, ceoBarredRow{Pilot: m.charNameOr(ctx, b.char), Reason: b.reason,
				Value: iskString(b.value), Until: until})
		}
	}
	if len(v.Divisions) == 0 && len(v.Kits) == 0 && len(v.Issues) == 0 && len(v.Barred) == 0 {
		return nil
	}
	return v
}

const tmplCEOArmory = `
{{with .Armory}}
<div class="card">
  <h3>War chest</h3>
  <p style="font-size:12px">{{if .Divisions}}{{range $i, $d := .Divisions}}{{if $i}} &middot; {{end}}{{$d}}{{end}}{{else}}no division free for an Armory, Yard or Spoils{{end}}
   &middot; kits lost in 7 days: {{.Lost7d}}</p>
  {{if .Kits}}<h4>Shelf</h4>
  <table class="tbl" style="font-size:12px"><tr><th>Shelf</th><th>Seat</th><th>Hull</th><th>At</th><th>Status</th><th>Value</th></tr>
    {{range .Kits}}<tr><td>{{.Kind}}</td><td>{{.Seat}}</td><td>{{.Hull}}</td><td>{{.Where}}</td><td>{{.Status}}</td><td>{{.Value}}</td></tr>{{end}}
  </table>{{end}}
  {{if .Issues}}<h4>Out on loan</h4>
  <table class="tbl" style="font-size:12px"><tr><th>Pilot</th><th>Hull</th><th>Shelf</th><th>Out for</th><th>Due in</th></tr>
    {{range .Issues}}<tr><td>{{.Pilot}}{{if .Human}} <b>(capsuleer)</b>{{end}}</td><td>{{.Hull}}</td><td>{{.Kind}}</td><td>{{.Since}}</td>
      <td>{{if .Overdue}}<b style="color:#ef4444">overdue</b>{{else}}{{.Due}}{{end}}</td></tr>{{end}}
  </table>{{end}}
  {{if .Barred}}<h4>Barred from the shelf</h4>
  <table class="tbl" style="font-size:12px"><tr><th>Pilot</th><th>Why</th><th>Value</th><th>Until</th></tr>
    {{range .Barred}}<tr><td>{{.Pilot}}</td><td>{{.Reason}}</td><td>{{.Value}}</td><td>{{.Until}}</td></tr>{{end}}
  </table>{{end}}
</div>
{{end}}
`
