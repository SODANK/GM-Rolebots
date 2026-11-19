package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// The skills page: /rolebot/<charID>/skills.
//
// The core's queue records what is training and nothing about why, so the page shows the plan with its
// score and reason. It is read-only about the queue, since editing a queue the pilot is automating
// loses to the next replan. The levers change the pilot's mind: the automation flag, a pinned skill,
// a banned skill, and a button that replans now instead of within the hour.

// skillPlanRow is one line of the plan readout.
type skillPlanRow struct {
	TypeID  int64
	Name    string
	Level   string
	Time    string
	Why     string
	Pending bool // planned but not queued: waiting on a book it cannot buy yet
}

// skillQueueRow is one line of the live queue.
type skillQueueRow struct {
	Position int
	Name     string
	Level    string
	ETA      string
}

// skillPrefRow is one operator override.
type skillPrefRow struct {
	TypeID int64
	Name   string
	Mode   string
}

func (m *Mod) handleSkillsPage(w http.ResponseWriter, r *http.Request, charID int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleSkillsPost(w, r, ctx, charID)
		return
	}

	rb, err := getRolebot(ctx, m.db, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	if rb == nil {
		espError(w, "rolebots", fmt.Sprintf("character %d is not on the rolebot roster", charID))
		return
	}
	prof, err := m.getProfile(ctx, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	status, err := charStatus(ctx, m.db, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}

	// The plan, as recorded by the last pass.
	plan, plannedAt, err := loadSkillPlan(ctx, m.db, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	planRows := make([]skillPlanRow, 0, len(plan))
	planMinutes := 0.0
	for _, s := range plan {
		planMinutes += s.Minutes
		planRows = append(planRows, skillPlanRow{
			TypeID: s.TypeID, Name: s.Name, Level: romanLevel(s.Level),
			Time: durationText(s.Minutes), Why: s.Why, Pending: s.NeedBook,
		})
	}
	planned := "never"
	if plannedAt > 0 {
		planned = time.Unix(plannedAt, 0).UTC().Format("2006-01-02 15:04:05 UTC")
	}

	// What is actually training, and what is behind it.
	training := ""
	if st, err := evedb.GetSkillTrainingInfo(ctx, m.db, charID); err == nil && st != nil {
		training = fmt.Sprintf("%s %s — done %s", st.TypeName, romanLevel(st.TargetLevel),
			evedb.FiletimeToTime(st.EndOfTraining).UTC().Format("2006-01-02 15:04 UTC"))
	}
	var queueRows []skillQueueRow
	if entries, err := evedb.GetSkillQueue(ctx, m.db, charID); err == nil {
		_ = evedb.EstimateSkillQueueETAs(ctx, m.db, charID, entries)
		for _, e := range entries {
			eta := "—"
			if e.EstEnd > 0 {
				eta = evedb.FiletimeToTime(e.EstEnd).UTC().Format("2006-01-02 15:04 UTC")
			}
			queueRows = append(queueRows, skillQueueRow{
				Position: e.Position, Name: e.TypeName,
				Level: romanLevel(e.TargetLevel), ETA: eta,
			})
		}
	}

	// The overrides, and the personality that shapes the planner.
	force, ban, err := skillPrefs(ctx, m.db, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	var prefRows []skillPrefRow
	addPrefs := func(set map[int64]bool, mode string) {
		for typeID := range set {
			name := fmt.Sprintf("type %d", typeID)
			_ = m.db.QueryRowContext(ctx,
				`SELECT typeName FROM invTypes WHERE typeID = ?`, typeID).Scan(&name)
			prefRows = append(prefRows, skillPrefRow{TypeID: typeID, Name: name, Mode: mode})
		}
	}
	addPrefs(force, "force")
	addPrefs(ban, "ban")

	traits, err := getTraits(ctx, m.db, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	tn := resolveTuning(traits)

	espRender(w, "rolebots_skills.html", map[string]any{
		"Page":     "rolebots",
		"Bot":      rb,
		"Profile":  prof,
		"Status":   status,
		"Wallet":   iskString(status.Balance),
		"Plan":     planRows,
		"PlanTime": durationText(planMinutes),
		"Planned":  planned,
		"Training": training,
		"Queue":    queueRows,
		"Prefs":    prefRows,

		"Satisfied":   romanLevel(tn.SkillSatisfiedLevel),
		"Horizon":     durationText(tn.SkillPlanHorizonMin),
		"StepCap":     durationText(tn.SkillStepMaxMinutes),
		"GroupCap":    tn.SkillGroupCap,
		"BookCushion": fmt.Sprintf("%.1f×", tn.SkillBookCushion),
		"WalletFloor": iskString(tn.SkillWalletFloor),

		"Msg": r.URL.Query().Get("msg"),
		"Err": r.URL.Query().Get("err"),
	})
}

func (m *Mod) handleSkillsPost(w http.ResponseWriter, r *http.Request, ctx context.Context, charID int64) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	self := fmt.Sprintf("/rolebot/%d/skills", charID)
	switch r.FormValue("action") {
	case "auto":
		on := r.FormValue("state") == "on"
		if err := setAutoSkills(ctx, m.db, charID, on); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		if on {
			redirect(w, r, self, "this pilot now plans its own training within the hour", "")
		} else {
			redirect(w, r, self, "automation off. The current queue runs out and is not replaced", "")
		}
	case "replan":
		if err := m.planSkillsFor(ctx, charID); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, "re-planned", "")
	case "pref":
		typeID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("typeID")), 10, 64)
		if typeID == 0 {
			redirect(w, r, self, "", "pick a skill first")
			return
		}
		// Only a real skill: a typeID from the wrong category would sit in the table, unseen by the planner.
		var one int64
		if err := m.db.QueryRowContext(ctx, `
			SELECT 1 FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID
			WHERE t.typeID = ? AND g.categoryID = 16`, typeID).Scan(&one); err != nil {
			redirect(w, r, self, "", fmt.Sprintf("type %d is not a skill", typeID))
			return
		}
		mode := r.FormValue("mode")
		if err := setSkillPref(ctx, m.db, charID, typeID, mode); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		switch mode {
		case "":
			redirect(w, r, self, "override removed — the pilot decides for itself again", "")
		case "ban":
			redirect(w, r, self, "skill banned, along with anything needing it", "")
		default:
			redirect(w, r, self, "skill pinned, trained to V first from the next plan", "")
		}
	default:
		http.Redirect(w, r, self, http.StatusSeeOther)
	}
}

// tmplSkills is the skills page.
const tmplSkills = `{{define "content"}}
<h2>Skill Training — {{.Status.CharacterName}}</h2>
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
<p><a href="/rolebot/{{.Bot.CharacterID}}">&larr; Back to this pilot's profile</a></p>

<div class="card">
  <h3>Automation</h3>
  <form method="POST" style="display:inline">
    <input type="hidden" name="action" value="auto">
    <input type="hidden" name="state" value="{{if .Profile.AutoSkills}}off{{else}}on{{end}}">
    <button type="submit">{{if .Profile.AutoSkills}}Stop automating training{{else}}Let this pilot train itself{{end}}</button>
  </form>
  <form method="POST" style="display:inline;margin-left:8px">
    <input type="hidden" name="action" value="replan">
    <button type="submit">Re-plan now</button>
  </form>
  <p style="margin-top:8px;font-size:12px;color:#6b7280">
    {{if .Profile.AutoSkills}}This pilot buys its own skillbooks and writes its own training queue.{{else}}
    This pilot is not training itself. Whatever is queued now will finish and nothing will replace it.{{end}}
    Last planned: {{.Planned}}. Wallet: {{.Wallet}}.
  </p>
  <p style="font-size:12px;color:#6b7280">Plans rebuild hourly and when the queue empties. Limits: satisfied at level <b>{{.Satisfied}}</b>, about <b>{{.Horizon}}</b> planned at a time, steps up to <b>{{.StepCap}}</b>, <b>{{.GroupCap}}</b> step(s) per skill group, <b>{{.BookCushion}}</b> held back for clone cover and a ship, wallet floor <b>{{.WalletFloor}}</b>.</p>
</div>

<div class="card">
  <h3>What it decided, and why</h3>
  {{if .Training}}<p><b>Training now:</b> {{.Training}}</p>{{end}}
  {{if .Plan}}
  <table class="tbl">
    <tr><th>#</th><th>Skill</th><th>To</th><th>Time</th><th>Why</th><th></th></tr>
    {{range $i, $s := .Plan}}
    <tr{{if $s.Pending}} style="opacity:.6"{{end}}>
      <td>{{$i}}</td>
      <td>{{$s.Name}}</td>
      <td>{{$s.Level}}</td>
      <td>{{$s.Time}}</td>
      <td>{{$s.Why}}</td>
      <td>{{if $s.Pending}}saving up for the book{{end}}</td>
    </tr>
    {{end}}
  </table>
  <p style="margin-top:8px;font-size:12px;color:#6b7280">{{.PlanTime}} of training in this plan. A greyed row is planned but not queued yet.</p>
  {{else}}
  <p>No plan recorded yet. It will be built on the next sim pass.</p>
  {{end}}
</div>

<div class="card">
  <h3>The queue as the server holds it</h3>
  {{if .Queue}}
  <table class="tbl">
    <tr><th>#</th><th>Skill</th><th>To</th><th>Estimated finish</th></tr>
    {{range .Queue}}
    <tr><td>{{.Position}}</td><td>{{.Name}}</td><td>{{.Level}}</td><td>{{.ETA}}</td></tr>
    {{end}}
  </table>
  <p style="margin-top:8px;font-size:12px;color:#6b7280">Estimates use current attributes, so queued learning skills are not counted.</p>
  {{else}}
  <p>Nothing queued.</p>
  {{end}}
</div>

<div class="card">
  <h3>Overrides</h3>
  <p style="font-size:12px;color:#6b7280">Pin trains a skill to V first. Ban removes it, and anything needing it, from the pilot's reach.</p>
  {{if .Prefs}}
  <table class="tbl">
    <tr><th>Skill</th><th>Override</th><th></th></tr>
    {{range .Prefs}}
    <tr>
      <td>{{.Name}}</td><td>{{.Mode}}</td>
      <td>
        <form method="POST" style="display:inline">
          <input type="hidden" name="action" value="pref">
          <input type="hidden" name="typeID" value="{{.TypeID}}">
          <input type="hidden" name="mode" value="">
          <button type="submit">remove</button>
        </form>
      </td>
    </tr>
    {{end}}
  </table>
  {{end}}
  <form method="POST" style="margin-top:8px">
    <input type="hidden" name="action" value="pref">
    <input type="number" name="typeID" placeholder="skill typeID" style="width:140px">
    <select name="mode">
      <option value="force">pin — train to V first</option>
      <option value="ban">ban — never train</option>
    </select>
    <button type="submit">Add</button>
  </form>
</div>
{{end}}`
