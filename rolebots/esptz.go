package rolebots

// The Timezones tab: when the roster plays (timezone.go). The settings form,
// the expected online count per server hour today, the split by home
// timezone, and how much of each bot-CEO corporation plays its CEO's hours.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tzHourRow is one bar of the day's curve.
type tzHourRow struct {
	Hour  int
	Count int
	Pct   float64
	Width int // bar width in percent of the tallest hour
	Now   bool
}

// tzBlocRow is one home timezone's share of the roster.
type tzBlocRow struct {
	Name  string
	Count int
	Pct   float64
}

// tzCorpRow is one bot-CEO corporation.
type tzCorpRow struct {
	CorpID   int64
	Name     string
	CEOID    int64
	CEOName  string
	CEOHours string
	CEOOn    bool
	Members  int
	InBloc   int
	Pct      float64
}

// tzHoursText words a base slot in server time.
func tzHoursText(s tzSlot) string {
	end := tzMod(s.Start+s.Len, tzDayMin)
	return fmt.Sprintf("%02d:%02d–%02d:%02d (home %+dh)", s.Start/60, s.Start%60, end/60, end%60, s.Offset)
}

// tzTodayText words today's window for one pilot and where it stands.
func tzTodayText(charID int64) string {
	c := tzSettings()
	if !c.On {
		return "timezones are off — online all day"
	}
	st := tzNow(charID)
	switch {
	case st.In:
		return "in its hours, " + tzDur(st.LeftMin) + " left"
	case st.NextMin < tzNever:
		return "outside its hours, back in " + tzDur(st.NextMin)
	default:
		return "outside its hours"
	}
}

func (m *Mod) handleTimezones(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	const self = "/rolebots/timezones"

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		c := tzSettings()
		c.On = r.FormValue("enabled") == "1"
		c.ArmsPing = r.FormValue("armsPing") == "1"
		num := func(key string, dst *int64) {
			if n, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(key)), 10, 64); err == nil {
				*dst = n
			}
		}
		num("graceMin", &c.GraceMin)
		num("bleedMin", &c.BleedMin)
		num("skipDayPct", &c.SkipDayPct)
		num("claimAfterHours", &c.ClaimAfterH)
		num("armsMinNeed", &c.ArmsMinNeed)
		num("armsMaxNeed", &c.ArmsMaxNeed)
		num("armsReachJumps", &c.ArmsReachJumps)
		if f, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("corpSharePct")), 64); err == nil {
			c.CorpShare = f / 100
		}
		if err := m.setTZSettings(ctx, c); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		msg := "timezones on"
		if !c.On {
			msg = "timezones off, pilots return over the next passes"
		}
		redirect(w, r, self, msg, "")
		return
	}

	data, err := m.tzPageData(ctx)
	if err != nil {
		espError(w, "rolebot-timezones", err.Error())
		return
	}
	data["Msg"] = r.URL.Query().Get("msg")
	data["Err"] = r.URL.Query().Get("err")
	espRender(w, "rolebots_timezones.html", data)
}

func (m *Mod) tzPageData(ctx context.Context) (map[string]any, error) {
	c := tzSettings()
	rows, err := m.db.QueryContext(ctx, `SELECT characterID FROM rolebot_characters WHERE enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("timezones roster: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	now := clock.Now()
	day, minute := tzLocal(now)
	curve := tzCurve(ids, day, c)
	peak := 1
	for _, n := range curve {
		peak = max(peak, n)
	}
	hours := make([]tzHourRow, 24)
	for h, n := range curve {
		hours[h] = tzHourRow{Hour: h, Count: n, Width: n * 100 / peak, Now: int(minute/60) == h}
		if len(ids) > 0 {
			hours[h].Pct = 100 * float64(n) / float64(len(ids))
		}
	}

	var blocs [3]tzBlocRow
	for i := range blocs {
		blocs[i].Name = tzBlocNames[i]
	}
	inHours := 0
	for _, id := range ids {
		blocs[tzBloc(tzSlotOf(id).Offset)].Count++
		if tzStateAt(id, tzSlotOf(id), day, minute, c).In {
			inHours++
		}
	}
	for i := range blocs {
		if len(ids) > 0 {
			blocs[i].Pct = 100 * float64(blocs[i].Count) / float64(len(ids))
		}
	}

	corps, err := m.tzCorpRows(ctx)
	if err != nil {
		return nil, err
	}
	tzOffN := tzOffCount()
	return map[string]any{
		"Page":     "rolebots",
		"Tab":      "tz",
		"Cfg":      c,
		"SharePct": int(c.CorpShare*100 + 0.5),
		"Now":      now.In(time.Local).Format("Mon 15:04 MST"),
		"Enabled":  len(ids),
		"InHours":  inHours,
		"TZOff":    tzOffN,
		"CapOff":   max(popOfflineCount()-tzOffN, 0),
		"Hours":    hours,
		"Blocs":    blocs,
		"Corps":    corps,
		"Arms":     armsSnapshot(),
	}, nil
}

// tzCorpRows is every bot-CEO corporation with how many of its bot members
// play in the CEO's home timezone.
func (m *Mod) tzCorpRows(ctx context.Context) ([]tzCorpRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT ce.corporationID, COALESCE(co.corporationName, ''), ce.characterID,
		       COALESCE(ch.characterName, '')
		  FROM rolebot_ceos ce
		  LEFT JOIN corporation co ON co.corporationID = ce.corporationID
		  LEFT JOIN character_ ch ON ch.characterID = ce.characterID`)
	if err != nil {
		return nil, fmt.Errorf("timezones CEOs: %w", err)
	}
	byCorp := map[int64]*tzCorpRow{}
	var out []*tzCorpRow
	for rows.Next() {
		cr := &tzCorpRow{}
		if rows.Scan(&cr.CorpID, &cr.Name, &cr.CEOID, &cr.CEOName) == nil {
			s := tzSlotOf(cr.CEOID)
			cr.CEOHours = tzHoursText(s)
			cr.CEOOn = !popIsOffline(cr.CEOID)
			byCorp[cr.CorpID] = cr
			out = append(out, cr)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = m.db.QueryContext(ctx, `
		SELECT ch.corporationID, ch.characterID
		  FROM character_ ch
		  JOIN rolebot_characters b ON b.characterID = ch.characterID AND b.enabled = 1
		  JOIN rolebot_ceos ce ON ce.corporationID = ch.corporationID
		 WHERE ch.characterID <> ce.characterID`)
	if err != nil {
		return nil, fmt.Errorf("timezones members: %w", err)
	}
	for rows.Next() {
		var corpID, charID int64
		if rows.Scan(&corpID, &charID) != nil {
			continue
		}
		cr := byCorp[corpID]
		if cr == nil {
			continue
		}
		cr.Members++
		if tzBloc(tzSlotOf(charID).Offset) == tzBloc(tzSlotOf(cr.CEOID).Offset) {
			cr.InBloc++
		}
	}
	rows.Close()

	res := make([]tzCorpRow, 0, len(out))
	for _, cr := range out {
		if cr.Members > 0 {
			cr.Pct = 100 * float64(cr.InBloc) / float64(cr.Members)
		}
		res = append(res, *cr)
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Members != res[j].Members {
			return res[i].Members > res[j].Members
		}
		return res[i].CorpID < res[j].CorpID
	})
	return res, rows.Err()
}

const tmplTimezones = castTabs + `{{define "content"}}
` + castTabsStyle + `
<style>
.rb-tz-sum { display:grid; grid-template-columns:repeat(auto-fit,minmax(150px,1fr)); gap:10px; margin-bottom:12px; }
.rb-tz-sum div { background:#170a0a; border:1px solid #2a1a1a; border-radius:6px; padding:9px 11px; }
.rb-tz-sum .k { font-size:10.5px; color:#6b7280; }
.rb-tz-sum .v { font-size:19px; color:#f0e6dc; font-variant-numeric:tabular-nums; }
.rb-tz-bar { height:12px; background:#7f1d1d; border-radius:2px; }
.rb-tz-bar.now { background:#dc2626; }
.rb-tz td, .rb-tz th { font-size:12px; padding:3px 6px; }
</style>
{{template "rb_casttabs" .}}
{{if .Msg}}<div class="alert alert-success">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="card">
  <h3 style="margin-top:0">Play hours</h3>
  <p style="font-size:12px;color:#9ca3af;max-width:860px">
    Each rolebot plays a few hours a day in server time ({{.Now}}). Outside them it is logged off.
    Skill training continues.
  </p>
  <div class="rb-tz-sum">
    <div><div class="k">Active on the roster</div><div class="v">{{.Enabled}}</div></div>
    <div><div class="k">In their hours now</div><div class="v">{{.InHours}}</div></div>
    <div><div class="k">Logged off for their hours</div><div class="v">{{.TZOff}}</div></div>
    <div><div class="k">Logged off by the cap</div><div class="v">{{.CapOff}}</div></div>
  </div>
  <form method="POST" action="/rolebots/timezones">
    <label><input type="checkbox" name="enabled" value="1"{{if .Cfg.On}} checked{{end}}> Timezones on</label>
    &nbsp; Grace <input type="number" name="graceMin" value="{{.Cfg.GraceMin}}" min="0" max="600" style="width:70px"> min
    &nbsp; Daily drift &plusmn;<input type="number" name="bleedMin" value="{{.Cfg.BleedMin}}" min="0" max="360" style="width:70px"> min
    &nbsp; Days off <input type="number" name="skipDayPct" value="{{.Cfg.SkipDayPct}}" min="0" max="90" style="width:60px">%
    &nbsp; Corp share <input type="number" name="corpSharePct" value="{{.SharePct}}" min="0" max="100" style="width:60px">%
    &nbsp; Claim jobs after <input type="number" name="claimAfterHours" value="{{.Cfg.ClaimAfterH}}" min="0" max="720" style="width:60px"> h
    <input type="hidden" name="armsPing" value="{{if .Cfg.ArmsPing}}1{{else}}0{{end}}">
    <input type="hidden" name="armsMinNeed" value="{{.Cfg.ArmsMinNeed}}">
    <input type="hidden" name="armsMaxNeed" value="{{.Cfg.ArmsMaxNeed}}">
    <input type="hidden" name="armsReachJumps" value="{{.Cfg.ArmsReachJumps}}">
    <button type="submit">Save</button>
  </form>
  <form method="POST" action="/rolebots/timezones" style="margin-top:8px">
    <input type="hidden" name="enabled" value="{{if .Cfg.On}}1{{else}}0{{end}}">
    <input type="hidden" name="graceMin" value="{{.Cfg.GraceMin}}">
    <input type="hidden" name="bleedMin" value="{{.Cfg.BleedMin}}">
    <input type="hidden" name="skipDayPct" value="{{.Cfg.SkipDayPct}}">
    <input type="hidden" name="corpSharePct" value="{{.SharePct}}">
    <input type="hidden" name="claimAfterHours" value="{{.Cfg.ClaimAfterH}}">
    <label><input type="checkbox" name="armsPing" value="1"{{if .Cfg.ArmsPing}} checked{{end}}> Calls to arms page logged-off members</label>
    &nbsp; Need <input type="number" name="armsMinNeed" value="{{.Cfg.ArmsMinNeed}}" min="1" max="200" style="width:60px">
    to <input type="number" name="armsMaxNeed" value="{{.Cfg.ArmsMaxNeed}}" min="1" max="200" style="width:60px"> pilots
    &nbsp; Reach <input type="number" name="armsReachJumps" value="{{.Cfg.ArmsReachJumps}}" min="0" max="40" style="width:60px"> jumps
    <button type="submit">Save</button>
  </form>
  <p style="font-size:11px;color:#6b7280;margin-top:6px">
    Corp share: how much of a bot CEO's corporation shares the CEO's timezone.
    Claim jobs after: a logged-off pilot with a finished job for this long logs in briefly to free the line. 0 turns it off.
  </p>
</div>

<div class="card">
  <h3>Open calls to arms</h3>
  <p style="font-size:12px;color:#9ca3af;max-width:860px">
    A siege or alliance defend call pages the corporation's logged-off members until it has the numbers.
  </p>
  {{if .Arms}}
  <table class="rb-tz">
    <tr><th>Corporation</th><th>System</th><th>Why</th><th>Need</th><th>On hand</th><th>Answered</th><th>Slept through</th></tr>
    {{range .Arms}}
    <tr><td>{{.CorpName}}</td><td>{{.SystemName}}</td><td>{{.Why}}</td><td>{{.Need}}</td>
        <td>{{.Held}}</td><td>{{.Answered}}</td><td>{{.Slept}}</td></tr>
    {{end}}
  </table>
  {{else}}<p style="color:#6b7280">No siege or defend call is in its window.</p>{{end}}
</div>

<div class="card">
  <h3>Expected online today, by server hour</h3>
  <table class="rb-tz">
    <tr><th>Hour</th><th style="width:60%"></th><th>Pilots</th><th>Share</th></tr>
    {{range .Hours}}
    <tr><td>{{printf "%02d:00" .Hour}}{{if .Now}} &larr;{{end}}</td>
        <td><div class="rb-tz-bar{{if .Now}} now{{end}}" style="width:{{.Width}}%"></div></td>
        <td>{{.Count}}</td><td>{{printf "%.0f" .Pct}}%</td></tr>
    {{end}}
  </table>
</div>

<div class="card">
  <h3>Home timezones</h3>
  <table class="rb-tz">
    <tr><th>Timezone</th><th>Pilots</th><th>Share</th></tr>
    {{range .Blocs}}<tr><td>{{.Name}}</td><td>{{.Count}}</td><td>{{printf "%.0f" .Pct}}%</td></tr>{{end}}
  </table>
</div>

<div class="card">
  <h3>Corporations on their CEO's hours</h3>
  {{if .Corps}}
  <table class="rb-tz">
    <tr><th>Corporation</th><th>CEO</th><th>CEO's hours (server time)</th><th>Bot members</th><th>On the CEO's timezone</th></tr>
    {{range .Corps}}
    <tr><td>{{.Name}}</td>
        <td><a href="/rolebot/{{.CEOID}}">{{.CEOName}}</a>{{if not .CEOOn}} <span style="color:#6b7280">(logged off)</span>{{end}}</td>
        <td>{{.CEOHours}}</td><td>{{.Members}}</td>
        <td>{{.InBloc}} ({{printf "%.0f" .Pct}}%)</td></tr>
    {{end}}
  </table>
  {{else}}<p style="color:#6b7280">No corporation is run by a bot CEO.</p>{{end}}
</div>
{{end}}`
