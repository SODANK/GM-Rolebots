package rolebots

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The arms board (/rolebots/corps/departments): the switch and every bot-run corporation's org chart in one
// line. /rolebots/corps/departments/<corporationID> is one corporation's chart: each arm's head, juniors and
// members, its headcount against its target, and what its roles open.

type armBoardRow struct {
	CorpID  int64
	Corp    string
	Members int
	Arms    []armBoardCell
	Pool    int
	Checked string
}

type armBoardCell struct {
	Label     string
	Count     int
	Target    int
	Justified bool
}

type armChartMember struct {
	CharID int64
	Name   string
	Title  string
	Human  bool
}

type armChartArm struct {
	Arm       string
	Label     string
	Justified bool
	Target    int
	Heads     []armChartMember
	Juniors   []armChartMember
	Members   []armChartMember
	HeadRoles []string
	JrRoles   []string
	RankRoles []string
}

// armsBoardPath is the board's own path; a corporationID after it is one chart.
const armsBoardPath = "rolebots/corps/departments"

// armsPathCorp reads a departments URL: the board (corpID 0) or one corporation's chart. ok is false
// for anything else under the board's path.
func armsPathCorp(urlPath string) (corpID int64, ok bool) {
	rest, found := strings.CutPrefix(strings.Trim(urlPath, "/"), armsBoardPath)
	if !found {
		return 0, false
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (m *Mod) handleArmsBoard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	corpID, ok := armsPathCorp(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if corpID != 0 {
		m.handleArmChart(ctx, w, corpID)
		return
	}
	counts := map[int64]map[corpArm]int{}
	if rows, err := m.db.QueryContext(ctx,
		`SELECT corporationID, arm, COUNT(*) FROM rolebot_corp_arm_members GROUP BY corporationID, arm`); err == nil {
		for rows.Next() {
			var corpID int64
			var arm string
			var n int
			if rows.Scan(&corpID, &arm, &n) == nil {
				if counts[corpID] == nil {
					counts[corpID] = map[corpArm]int{}
				}
				counts[corpID][corpArm(arm)] = n
			}
		}
		rows.Close()
	}
	var out []armBoardRow
	for _, corpID := range managedCorpIDs() {
		facts, err := getAnyCorpFacts(ctx, m.db, corpID)
		if err != nil {
			continue
		}
		row := armBoardRow{CorpID: corpID, Corp: facts.Name, Members: facts.Members}
		memo, ok := armMemoOf(corpID)
		just := map[corpArm]bool{}
		if ok {
			for _, a := range memo.Justified {
				just[a] = true
			}
			row.Checked = time.Unix(memo.At, 0).UTC().Format("01-02 15:04")
		}
		placed := 0
		for _, a := range armOrder {
			n := counts[corpID][a]
			placed += n
			row.Arms = append(row.Arms, armBoardCell{Label: armTag(a), Count: n, Target: memo.Targets[a], Justified: just[a]})
		}
		row.Pool = max(0, facts.Members-1-placed) // the CEO sits above every arm
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Members > out[j].Members })
	var heads []string
	for _, a := range armOrder {
		heads = append(heads, armLabel(a))
	}
	espRender(w, "rolebots_arms.html", map[string]any{
		"Page":  "rolebot-corps",
		"Tab":   "departments",
		"Msg":   r.URL.Query().Get("msg"),
		"Err":   r.URL.Query().Get("err"),
		"Heads": heads,
		"Corps": out,
	})
}

func (m *Mod) handleArmChart(ctx context.Context, w http.ResponseWriter, corpID int64) {
	facts, err := getAnyCorpFacts(ctx, m.db, corpID)
	if err != nil {
		espError(w, "rolebot-arms", err.Error())
		return
	}
	ceoID, _ := managedCEO(corpID)
	members, err := m.corpMembers(ctx, corpID, ceoID)
	if err != nil {
		espError(w, "rolebot-arms", err.Error())
		return
	}
	rows, err := m.loadArmRows(ctx, corpID)
	if err != nil {
		espError(w, "rolebot-arms", err.Error())
		return
	}
	memo, _ := armMemoOf(corpID)
	just := map[corpArm]bool{}
	for _, a := range memo.Justified {
		just[a] = true
	}
	byArm := map[corpArm]*armChartArm{}
	var arms []*armChartArm
	for _, a := range armOrder {
		ca := &armChartArm{Arm: string(a), Label: armLabel(a), Justified: just[a], Target: memo.Targets[a],
			HeadRoles: armRoleWords(armRoles(a, armTierHead, false)),
			JrRoles:   armRoleWords(armRoles(a, armTierJunior, false)),
			RankRoles: armRoleWords(armRoles(a, armTierMember, false))}
		byArm[a] = ca
		arms = append(arms, ca)
	}
	var pool []armChartMember
	for _, cm := range members {
		mem := armChartMember{CharID: cm.CharID, Name: cm.Name, Title: cm.Title, Human: cm.Human()}
		row, ok := rows[cm.CharID]
		ca := byArm[row.Arm]
		if !ok || ca == nil {
			if cm.Eligible() {
				pool = append(pool, mem)
			}
			continue
		}
		switch row.Tier {
		case armTierHead:
			ca.Heads = append(ca.Heads, mem)
		case armTierJunior:
			ca.Juniors = append(ca.Juniors, mem)
		default:
			ca.Members = append(ca.Members, mem)
		}
	}
	checked := "not yet — the CEO's next think organises it"
	if memo.At != 0 {
		checked = time.Unix(memo.At, 0).UTC().Format("2006-01-02 15:04") + " UTC"
	}
	espRender(w, "rolebots_arm_chart.html", map[string]any{
		"Page":    "rolebot-corps",
		"Tab":     "departments",
		"CorpID":  corpID,
		"Corp":    facts.Name,
		"CEO":     m.charNameOr(ctx, ceoID),
		"CEOID":   ceoID,
		"Members": facts.Members,
		"Checked": checked,
		"Arms":    arms,
		"Pool":    pool,
	})
}

const tmplArms = corpTabs + `{{define "content"}}` + castTabsStyle + `
<h2>Departments</h2>
{{template "rb_corptabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
<div class="card">
  <h3>How it works</h3>
  <p>Bot CEOs split their corporations into departments (Security, Industry, Mining, Sales, Human Resources, Finance)
  and place every active member in one. A department is kept only when the corporation has the people and the work for it.</p>
</div>
<div class="card">
  <h3>Bot-run corporations</h3>
  {{if .Corps}}
  <table class="tbl" style="font-size:12px">
    <tr><th>Corporation</th><th>Members</th>{{range .Heads}}<th>{{.}}</th>{{end}}<th>Unassigned</th><th>Organised</th></tr>
    {{range .Corps}}<tr><td><a href="/rolebots/corps/departments/{{.CorpID}}">{{.Corp}}</a></td><td>{{.Members}}</td>
      {{range .Arms}}<td>{{if .Justified}}<b>{{.Count}}</b>{{if .Target}} / {{.Target}}{{end}}{{else}}{{if .Count}}{{.Count}}{{else}}<span style="color:#6b7280">—</span>{{end}}{{end}}</td>{{end}}
      <td>{{.Pool}}</td><td>{{if .Checked}}{{.Checked}}{{else}}<span style="color:#6b7280">not yet</span>{{end}}</td></tr>{{end}}
  </table>
  <p style="font-size:12px;color:#6b7280">Bold: a department the corporation keeps, members / target. A plain number is a department the corporation
  has not yet earned, with the position holders already recorded in it.</p>
  {{else}}<p>No corporation has a bot CEO.</p>{{end}}
</div>
{{end}}`

const tmplArmChart = corpTabs + `{{define "content"}}` + castTabsStyle + `
<h2>Departments</h2>
{{template "rb_corptabs" .}}
<h3>{{.Corp}} — org chart</h3>
<p><a href="/rolebots/corps/departments">&larr; all corporations</a> · <a href="/rolebots/corp/{{.CorpID}}">corporation page</a></p>
<div class="card">
  <p>Chief Executive Officer: <b>{{.CEO}}</b> · {{.Members}} members · organised {{.Checked}}
  </p>
</div>
{{range .Arms}}
<div class="card">
  <h3>{{.Label}} {{if .Justified}}<span style="font-size:12px">(kept, target {{.Target}})</span>{{else}}<span style="font-size:12px;color:#6b7280">(not kept yet)</span>{{end}}</h3>
  <table class="tbl" style="font-size:12px">
    <tr><th style="width:110px">Head</th><td>{{range .Heads}}<a href="/rolebot/{{.CharID}}">{{.Name}}</a>{{if .Human}} (player){{end}} <i>{{.Title}}</i><br>{{else}}<span style="color:#6b7280">the CEO fills this role</span>{{end}}
      {{if .HeadRoles}}<div style="color:#6b7280">{{range .HeadRoles}}{{.}}; {{end}}</div>{{end}}</td></tr>
    <tr><th>Juniors</th><td>{{range .Juniors}}<a href="/rolebot/{{.CharID}}">{{.Name}}</a>{{if .Human}} (player){{end}} <i>{{.Title}}</i><br>{{else}}<span style="color:#6b7280">none</span>{{end}}
      {{if .JrRoles}}<div style="color:#6b7280">{{range .JrRoles}}{{.}}; {{end}}</div>{{end}}</td></tr>
    <tr><th>Members ({{len .Members}})</th><td>{{range .Members}}<a href="/rolebot/{{.CharID}}">{{.Name}}</a> <i>{{.Title}}</i>, {{else}}<span style="color:#6b7280">none</span>{{end}}
      {{if .RankRoles}}<div style="color:#6b7280">{{range .RankRoles}}{{.}}; {{end}}</div>{{end}}</td></tr>
  </table>
</div>
{{end}}
<div class="card">
  <h3>Unassigned ({{len .Pool}})</h3>
  <p>{{range .Pool}}<a href="/rolebot/{{.CharID}}">{{.Name}}</a>{{if .Human}} (player){{end}}, {{else}}Nobody. Every member is in a department.{{end}}</p>
</div>
{{end}}`
