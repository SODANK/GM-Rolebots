package rolebots

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// The Morale tab under Corps (/rolebots/corps/morale): the attrition switch,
// how long pilots have stayed where they are, and the morale ledger
// (corpmorale.go) per corporation and per member. Read-only apart from the
// switch.

// moraleCorpRow is one corporation on the page.
type moraleCorpRow struct {
	CorpID  int64
	Name    string
	Kind    string
	Members int
	MeanD   float64
	Median  string
	Exits30 int
	Top     string
	sumH    float64
	topSum  map[string]float64
}

// moralePilotRow is one member in the drill-down.
type moralePilotRow struct {
	CharID int64
	Name   string
	D      float64
	Top    string
	P30    int
	Median string
}

type moraleEventRow struct {
	At     string
	Name   string
	Kind   string
	Weight string
	Detail string
}

type moraleBucket struct {
	Label  string
	NPC    int
	Player int
}

func moraleDaysText(d float64) string {
	switch {
	case math.IsInf(d, 1) || d > 3650:
		return "—"
	case d >= 60:
		return fmt.Sprintf("%.1f mo", d/30)
	default:
		return fmt.Sprintf("%.0f d", d)
	}
}

func (m *Mod) handleCorpsMorale(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	self := "/rolebots/corps/morale"

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			espError(w, "rolebot-morale", err.Error())
			return
		}
		if r.FormValue("action") == "attrition" {
			on := r.FormValue("state") == "on"
			if err := m.setCorpAttrition(ctx, on); err != nil {
				redirect(w, r, self, "", err.Error())
				return
			}
			if on {
				redirect(w, r, self, "attrition ON — pilots that roll an exit leave at their next review, ramping in over 14 days", "")
			} else {
				redirect(w, r, self, "attrition OFF — the ledger keeps running in shadow", "")
			}
			return
		}
		redirect(w, r, self, "", "unknown action")
		return
	}

	now := clock.Unix()
	rows, pilots, err := m.moraleBoard(ctx, now)
	if err != nil {
		espError(w, "rolebot-morale", err.Error())
		return
	}
	tenure, err := m.moraleTenure(ctx, now)
	if err != nil {
		espError(w, "rolebot-morale", err.Error())
		return
	}
	var exits7, exits30, left7, left30 int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(at >= ?), 0), COUNT(*) FROM rolebot_corp_morale_events
		 WHERE kind IN (?, ?) AND at >= ?`, now-7*86400, mkShadowExit, mkExit, now-30*86400).Scan(&exits7, &exits30)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(at >= ?), 0), COUNT(*) FROM rolebot_corp_morale_events
		 WHERE kind = ? AND at >= ?`, now-7*86400, mkLeft, now-30*86400).Scan(&left7, &left30)
	ramp := fmt.Sprintf("%.0f%%", 100*moraleRamp(corpAttritionAt.Load(), now))

	tracked, sumD, sumH := 0, 0.0, 0.0
	buckets := []struct {
		Label string
		N     int
	}{{"goodwill (< 0)", 0}, {"0 – 0.25", 0}, {"0.25 – 0.5", 0}, {"0.5 – 0.75", 0}, {"0.75 +", 0}}
	for _, p := range pilots {
		tracked++
		sumD += p.D
		sumH += p.h
		switch {
		case p.D < 0:
			buckets[0].N++
		case p.D < 0.25:
			buckets[1].N++
		case p.D < 0.5:
			buckets[2].N++
		case p.D < 0.75:
			buckets[3].N++
		default:
			buckets[4].N++
		}
	}
	meanD, median := 0.0, "—"
	if tracked > 0 {
		meanD = sumD / float64(tracked)
		median = moraleDaysText(moraleMedianDays(sumH / float64(tracked)))
	}

	var drill []moralePilotRow
	var events []moraleEventRow
	drillName := ""
	if id, err := strconv.ParseInt(r.URL.Query().Get("corp"), 10, 64); err == nil && id > 0 {
		drillName = m.corpNameOf(ctx, id)
		for _, p := range pilots {
			if p.corpID != id {
				continue
			}
			drill = append(drill, moralePilotRow{CharID: p.CharID, Name: p.Name, D: p.D, Top: moraleLabel(p.top),
				P30: int(math.Round(100 * moraleLeaveProb(p.h, 30))), Median: moraleDaysText(moraleMedianDays(p.h))})
		}
		sort.Slice(drill, func(i, j int) bool { return drill[i].D > drill[j].D })
		events = m.moraleRecentEvents(ctx, id)
	}

	espRender(w, "rolebots_morale.html", map[string]any{
		"Page":        "rolebot-corps",
		"Tab":         "morale",
		"Msg":         r.URL.Query().Get("msg"),
		"Err":         r.URL.Query().Get("err"),
		"AttritionOn": corpAttritionOn.Load(),
		"Tracked":     tracked,
		"MeanD":       fmt.Sprintf("%.2f", meanD),
		"Median":      median,
		"Exits7":      exits7,
		"Exits30":     exits30,
		"Left7":       left7,
		"Left30":      left30,
		"Ramp":        ramp,
		"Buckets":     buckets,
		"Tenure":      tenure,
		"Rows":        rows,
		"DrillID":     r.URL.Query().Get("corp"),
		"DrillName":   drillName,
		"Drill":       drill,
		"Events":      events,
	})
}

// moralePilot is one tracked member as the board reads it.
type moralePilot struct {
	CharID int64
	Name   string
	corpID int64
	D      float64
	h      float64
	top    string
}

// moraleBoard reads every enabled pilot's morale against its current
// corporation and rolls it up per corporation.
func (m *Mod) moraleBoard(ctx context.Context, now int64) ([]moraleCorpRow, []moralePilot, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName, c.corporationID, COALESCE(co.corporationName, ''),
		       n.corporationID IS NOT NULL, COALESCE(t.value, 0), mo.kind, mo.value, mo.at
		  FROM rolebot_corp_morale mo
		  JOIN character_ c ON c.characterID = mo.characterID AND c.corporationID = mo.corporationID
		  JOIN rolebot_characters b ON b.characterID = c.characterID AND b.enabled = 1
		  LEFT JOIN corporation co ON co.corporationID = c.corporationID
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
		  LEFT JOIN rolebot_traits t ON t.characterID = c.characterID AND t.trait = ?
		 ORDER BY c.characterID`, TraitLoyalty)
	if err != nil {
		return nil, nil, fmt.Errorf("morale board: %w", err)
	}
	defer rows.Close()
	type acc struct {
		p     moralePilot
		npc   bool
		loy   int
		cells map[string]moraleCell
		cname string
	}
	var order []int64
	byChar := map[int64]*acc{}
	for rows.Next() {
		var id, corp int64
		var name, cname, kind string
		var npc bool
		var loy int
		var c moraleCell
		if err := rows.Scan(&id, &name, &corp, &cname, &npc, &loy, &kind, &c.Value, &c.At); err != nil {
			return nil, nil, err
		}
		a := byChar[id]
		if a == nil {
			a = &acc{p: moralePilot{CharID: id, Name: name, corpID: corp}, npc: npc, loy: loy,
				cells: map[string]moraleCell{}, cname: cname}
			byChar[id] = a
			order = append(order, id)
		}
		a.cells[kind] = c
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	byCorp := map[int64]*moraleCorpRow{}
	pilots := make([]moralePilot, 0, len(order))
	for _, id := range order {
		a := byChar[id]
		a.p.D = moraleTotal(a.cells, now)
		a.p.top = moraleTop(a.cells, now)
		a.p.h = moraleHazard(a.p.D, moraleH0(map[string]int{TraitLoyalty: a.loy}, a.npc, false))
		pilots = append(pilots, a.p)
		cr := byCorp[a.p.corpID]
		if cr == nil {
			cr = &moraleCorpRow{CorpID: a.p.corpID, Name: a.cname, Kind: m.moraleCorpKind(a.p.corpID, a.npc),
				topSum: map[string]float64{}}
			byCorp[a.p.corpID] = cr
		}
		cr.Members++
		cr.MeanD += a.p.D
		cr.sumH += a.p.h
		for k, c := range a.cells {
			if v := c.value(now); !strings.HasPrefix(k, "_") && v > 0 {
				cr.topSum[k] += v
			}
		}
	}
	exits := map[int64]int{}
	if er, err := m.db.QueryContext(ctx, `
		SELECT corporationID, COUNT(*) FROM rolebot_corp_morale_events
		 WHERE kind = ? AND at >= ? GROUP BY corporationID`, mkShadowExit, now-30*86400); err == nil {
		for er.Next() {
			var id int64
			var n int
			if er.Scan(&id, &n) == nil {
				exits[id] = n
			}
		}
		er.Close()
	}
	out := make([]moraleCorpRow, 0, len(byCorp))
	for _, cr := range byCorp {
		cr.MeanD /= float64(cr.Members)
		cr.Median = moraleDaysText(moraleMedianDays(cr.sumH / float64(cr.Members)))
		cr.Exits30 = exits[cr.CorpID]
		best, bestV := "", 0.0
		for k, v := range cr.topSum {
			if v > bestV {
				best, bestV = k, v
			}
		}
		if best != "" {
			cr.Top = moraleLabel(best)
		}
		if cr.Name == "" {
			cr.Name = fmt.Sprintf("corporation %d", cr.CorpID)
		}
		out = append(out, *cr)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MeanD != out[j].MeanD {
			return out[i].MeanD > out[j].MeanD
		}
		return out[i].CorpID < out[j].CorpID
	})
	return out, pilots, nil
}

// moraleCorpKind names who runs a corporation, for the page.
func (m *Mod) moraleCorpKind(corpID int64, npc bool) string {
	switch {
	case npc && npcChaired(corpID):
		return "NPC, chaired"
	case npc:
		return "NPC"
	}
	if _, ok := managedCEO(corpID); ok {
		return "bot CEO"
	}
	return "human CEO"
}

// moraleTenure buckets every enabled pilot by how long it has been in its
// current corporation, NPC and player apart: the before-and-after baseline.
func (m *Mod) moraleTenure(ctx context.Context, now int64) ([]moraleBucket, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT COALESCE(c.corporationDateTime, 0), n.corporationID IS NOT NULL
		  FROM rolebot_characters b
		  JOIN character_ c ON c.characterID = b.characterID
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
		 WHERE b.enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("morale tenure: %w", err)
	}
	defer rows.Close()
	out := []moraleBucket{{Label: "under 7 d"}, {Label: "7 – 30 d"}, {Label: "1 – 3 mo"}, {Label: "3 – 6 mo"}, {Label: "6 mo +"}, {Label: "unknown"}}
	nowWin := evedb.UnixToWin32(now)
	for rows.Next() {
		var since int64
		var npc bool
		if err := rows.Scan(&since, &npc); err != nil {
			return nil, err
		}
		i := 5
		if since > 0 {
			switch d := tenureDays(since, nowWin); {
			case d < 7:
				i = 0
			case d < 30:
				i = 1
			case d < 90:
				i = 2
			case d < 180:
				i = 3
			default:
				i = 4
			}
		}
		if npc {
			out[i].NPC++
		} else {
			out[i].Player++
		}
	}
	return out, rows.Err()
}

// moraleRecentEvents is a corporation's latest felt events, newest first.
func (m *Mod) moraleRecentEvents(ctx context.Context, corpID int64) []moraleEventRow {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.at, COALESCE(c.characterName, ''), e.kind, e.weight, e.detail
		  FROM rolebot_corp_morale_events e
		  LEFT JOIN character_ c ON c.characterID = e.characterID
		 WHERE e.corporationID = ?
		 ORDER BY e.eventID DESC LIMIT 60`, corpID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []moraleEventRow
	for rows.Next() {
		var at int64
		var name, kind, detail string
		var w float64
		if rows.Scan(&at, &name, &kind, &w, &detail) != nil {
			continue
		}
		ws := fmt.Sprintf("%+.2f", w)
		if kind == mkShadowExit {
			ws = "exit"
		}
		out = append(out, moraleEventRow{At: time.Unix(at, 0).UTC().Format("01-02 15:04"), Name: name,
			Kind: moraleLabel(kind), Weight: ws, Detail: detail})
	}
	return out
}

const tmplMorale = corpTabs + `{{define "content"}}` + castTabsStyle + rbTableStyle + `
<h2>Corporations</h2>
{{template "rb_corptabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="stats">
  <div class="stat"><div class="label">Pilots tracked</div><div class="value">{{.Tracked}}</div></div>
  <div class="stat"><div class="label">Mean morale debt</div><div class="value">{{.MeanD}}</div></div>
  <div class="stat"><div class="label">Projected median stay</div><div class="value">{{.Median}}</div></div>
  <div class="stat"><div class="label">Exits rolled, 7 d / 30 d</div><div class="value">{{.Exits7}} <span class="rbdim" style="font-size:12px">/ {{.Exits30}}</span></div></div>
  <div class="stat"><div class="label">Left, 7 d / 30 d</div><div class="value">{{.Left7}} <span class="rbdim" style="font-size:12px">/ {{.Left30}}</span></div></div>
</div>

<div class="card">
<details class="rbfold" open>
<summary>Attrition <span class="rbcount">({{if .AttritionOn}}ON{{else}}OFF{{end}})</span></summary>
  <p style="margin-top:8px">
    {{if .AttritionOn}}<strong style="color:#22c55e">ON</strong>{{else}}<strong style="color:#ef4444">OFF</strong>{{end}}
    — the ledger runs either way. OFF, would-be leavers are only counted. ON, pilots that roll an exit leave at
    their next review. The hazard ramps in over 14 days (now {{.Ramp}}).
    <form method="POST" style="display:inline;margin-left:8px">
      <input type="hidden" name="action" value="attrition">
      <input type="hidden" name="state" value="{{if .AttritionOn}}off{{else}}on{{end}}">
      <button type="submit">{{if .AttritionOn}}Switch OFF{{else}}Switch ON{{end}}</button>
    </form>
  </p>
  <p style="font-size:12px" class="rbdim">
    Grievances raise a pilot's morale debt, goodwill pays it down. Higher debt means a higher chance of leaving.
  </p>
</details>
</div>

<div class="card" style="display:flex;gap:24px;flex-wrap:wrap">
  <div style="flex:1 1 280px">
    <h4 style="margin-top:0">Time in current corporation</h4>
    <table class="rbtight">
      <thead><tr><th>Tenure</th><th class="rbnum">NPC</th><th class="rbnum">Player</th></tr></thead>
      <tbody>{{range .Tenure}}<tr><td>{{.Label}}</td><td class="rbnum">{{.NPC}}</td><td class="rbnum">{{.Player}}</td></tr>{{end}}</tbody>
    </table>
  </div>
  <div style="flex:1 1 280px">
    <h4 style="margin-top:0">Morale debt</h4>
    <table class="rbtight">
      <thead><tr><th>D</th><th class="rbnum">Pilots</th></tr></thead>
      <tbody>{{range .Buckets}}<tr><td>{{.Label}}</td><td class="rbnum">{{.N}}</td></tr>{{end}}</tbody>
    </table>
  </div>
</div>

{{if .DrillID}}
<div class="card">
  <h3 style="margin-top:0">{{.DrillName}} <a href="/rolebots/corps/morale" style="font-size:12px">&larr; all corporations</a></h3>
  <table class="rbtight">
    <thead><tr><th>Pilot</th><th class="rbnum">D</th><th>Worst</th><th class="rbnum">Leave in 30 d</th><th class="rbnum">Median stay</th></tr></thead>
    <tbody>
    {{range .Drill}}
    <tr><td><a href="/rolebot/{{.CharID}}">{{.Name}}</a></td><td class="rbnum">{{printf "%.2f" .D}}</td>
        <td class="rbdim">{{if .Top}}{{.Top}}{{else}}&mdash;{{end}}</td><td class="rbnum">{{.P30}}%</td><td class="rbnum">{{.Median}}</td></tr>
    {{else}}<tr><td colspan="5" class="rbdim">No member sampled yet.</td></tr>{{end}}
    </tbody>
  </table>
  <h4>Recent events</h4>
  <table class="rbtight">
    <thead><tr><th>When (UTC)</th><th>Pilot</th><th>Kind</th><th class="rbnum">Weight</th><th>Detail</th></tr></thead>
    <tbody>
    {{range .Events}}<tr><td class="rbdim">{{.At}}</td><td>{{.Name}}</td><td>{{.Kind}}</td><td class="rbnum">{{.Weight}}</td><td class="rbdim">{{.Detail}}</td></tr>
    {{else}}<tr><td colspan="5" class="rbdim">Nothing recorded yet.</td></tr>{{end}}
    </tbody>
  </table>
</div>
{{end}}

<div class="card">
  <h3 style="margin-top:0">By corporation</h3>
  <table class="rbtight">
    <thead><tr><th>Corporation</th><th>Run by</th><th class="rbnum">Members</th><th class="rbnum">Mean D</th><th class="rbnum">Median stay</th><th class="rbnum">Would have left, 30 d</th><th>Biggest grievance</th></tr></thead>
    <tbody>
    {{range .Rows}}
    <tr><td><a href="/rolebots/corps/morale?corp={{.CorpID}}">{{.Name}}</a></td><td class="rbdim">{{.Kind}}</td>
        <td class="rbnum">{{.Members}}</td><td class="rbnum">{{printf "%.2f" .MeanD}}</td><td class="rbnum">{{.Median}}</td>
        <td class="rbnum">{{.Exits30}}</td><td class="rbdim">{{if .Top}}{{.Top}}{{else}}&mdash;{{end}}</td></tr>
    {{else}}<tr><td colspan="7" class="rbdim">No corporation sampled yet — the first pass primes the ledger, the second starts counting.</td></tr>{{end}}
    </tbody>
  </table>
</div>
{{end}}`
