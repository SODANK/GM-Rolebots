package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// One NPC corporation: /rolebots/corp/<id> for an ID the game shipped. Top: what the game says. Then what
// the module may do about it. Last the dry run: the pilots who would walk in, ranked, with the arithmetic and
// the cause of every refusal. A player corporation's page is espcorp_page.go.

// corpCandidateRow is one line of the dry run.
type corpCandidateRow struct {
	CharID    int64
	Name      string
	CorpName  string
	Archetype string
	Eligible  bool
	Why       string
	Rule      string
	Score     float64
	Bar       int
	Fit       float64
	Standing  float64
	Home      float64
	Crowd     float64
	Jumps     string
}

func (m *Mod) npcCorpPage(w http.ResponseWriter, r *http.Request, ctx context.Context, c *CorpFacts) {
	corpID := c.CorpID
	cands, err := m.rankPilotsForCorp(ctx, *c, 40)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	best := 0.0
	for _, cd := range cands {
		if cd.Score.Score > best {
			best = cd.Score.Score
		}
	}
	arch, err := m.generatedArchetypes(ctx)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	rows := make([]corpCandidateRow, 0, len(cands))
	eligible := 0
	for _, cd := range cands {
		jumps := "—"
		switch {
		case cd.Score.SameHome:
			jumps = "lives here"
		case cd.Score.Jumps == 0:
			jumps = "same system"
		case cd.Score.Jumps > 0:
			jumps = fmt.Sprintf("%d", cd.Score.Jumps)
		}
		if cd.Score.Verdict.OK {
			eligible++
		}
		rows = append(rows, corpCandidateRow{
			CharID: cd.Pilot.CharID, Name: cd.Pilot.Name, CorpName: cd.Pilot.CorpName,
			Archetype: arch[cd.Pilot.CharID],
			Eligible:  cd.Score.Verdict.OK,
			Why:       cd.Score.Why, Rule: cd.Score.Verdict.Rule,
			Score: cd.Score.Score, Bar: pctOf(cd.Score.Score, best),
			Fit: cd.Score.Fit, Standing: cd.Score.Standing, Home: cd.Score.Home,
			Crowd: cd.Score.Crowd,
			Jumps: jumps,
		})
	}

	// The bloc rule for this corporation: which empires it would take, straight off the static matrix.
	mx, err := factionStandings(ctx, m.db)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	empires, err := m.empireFactionIDs(ctx)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	type blocRow struct {
		Empire string
		Theirs float64
		Ours   float64
		Takes  bool
	}
	var blocRows []blocRow
	for _, e := range empires {
		theirs := blocStanding(mx, c.FactionID, e.ID)
		ours := blocStanding(mx, e.ID, c.FactionID)
		blocRows = append(blocRows, blocRow{
			Empire: e.Name, Theirs: theirs, Ours: ours,
			Takes: c.Bloc() != blocEmpire || c.FactionID == 0 ||
				e.ID == c.FactionID || (theirs >= 0 && ours >= 0),
		})
	}
	sort.Slice(blocRows, func(i, j int) bool { return blocRows[i].Empire < blocRows[j].Empire })

	// The chain of command (corprank.go): who holds a fleet-commander seat.
	rawRanks, err := m.listCorpRanks(ctx, corpID)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	type rankRow struct {
		corpRankRow
		AppointedText string
	}
	ranks := make([]rankRow, 0, len(rawRanks))
	for _, rk := range rawRanks {
		txt := "—"
		if rk.AppointedAt > 0 {
			txt = time.Unix(rk.AppointedAt, 0).UTC().Format("2006-01-02")
		}
		ranks = append(ranks, rankRow{rk, txt})
	}
	enabledBots, err := m.countEnabledCorpBots(ctx, corpID)
	if err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}

	espRender(w, "rolebots_corp_npc.html", map[string]any{
		"Page":   "rolebot-corps",
		"Tab":    "npc",
		"Msg":    r.URL.Query().Get("msg"),
		"Err":    r.URL.Query().Get("err"),
		"Corp":   c,
		"Policy": c.Policy,
		"Tags":   corpTagCatalog,
		// The combat doctrine is derived from the tags (corpdoctrine.go), not stored, so it is shown and not
		// edited: changing the tags moves it.
		"Doctrine":   doctrineFromTags(c.Policy.Tags),
		"WingPct":    int(wingFracFromTags(c.Policy.Tags)*100 + 0.5),
		"WingPilots": int(float64(enabledBots)*wingFracFromTags(c.Policy.Tags) + 0.5),
		"Ranks":      ranks,
		"FleetSeats": enabledBots / FCSeatDivisor(),
		"FleetActs":  corpFleetActivities(c.Policy),
		"Chair":      m.npcChairSummary(ctx, corpID),
		"Candidates": rows,
		"Eligible":   eligible,
		"BlocRows":   blocRows,
		// The corporation's own people, named rather than described, so the
		// page says the same thing corpEligibility enforces (corps.go).
		"CorpRace": raceName(c.RaceID),
		"RaceRules": []struct{ Key, Label string }{
			{corpRaceAuto, "Automatic — a state's corporation takes its own race, everyone else takes anybody"},
			{corpRaceBloc, "Bloc rule — empire standing must not be negative"},
			{corpRaceStrict, "Own race only"},
			{corpRaceOpen, "Open — anyone, whatever their race"},
		},
	})
}

// generatedArchetypes is what the population generator made each pilot as, shown beside a candidate
// because it explains a score. Empty for a hand-adopted character.
func (m *Mod) generatedArchetypes(ctx context.Context) (map[int64]string, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, archetype FROM rolebot_generated`)
	if err != nil {
		return nil, fmt.Errorf("generatedArchetypes: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		if a, ok := archetypeByKey(key); ok {
			out[id] = a.Label
		} else {
			out[id] = key
		}
	}
	return out, rows.Err()
}

// npcCorpPost is the NPC page's own actions: the policy form, the reset, the
// promotion pass and the kick-all. A player corporation's page posts to
// handleCEOPost instead.
func (m *Mod) npcCorpPost(w http.ResponseWriter, r *http.Request, ctx context.Context, c *CorpFacts) {
	if err := r.ParseForm(); err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	corpID := c.CorpID
	self := fmt.Sprintf("/rolebots/corp/%d", corpID)

	switch r.FormValue("action") {
	case "save":
		p := c.Policy
		p.Joinable = r.FormValue("joinable") == "1"
		p.Restricted = r.FormValue("restricted") == "1"
		// Tags come from the checkbox grid and a free-text field: the catalogue is a vocabulary, not a fence,
		// so an operator can filter on a new kind of outfit before the scorer knows it.
		tags := append([]string{}, r.Form["tag"]...)
		tags = append(tags, parseTagList(r.FormValue("extraTags"))...)
		p.Tags = parseTagList(strings.Join(tags, ","))
		p.RaceRule = r.FormValue("raceRule")
		switch p.RaceRule {
		case corpRaceAuto, corpRaceBloc, corpRaceStrict, corpRaceOpen:
		default:
			redirect(w, r, self, "", "unknown race rule")
			return
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("minStanding")), 64); err == nil {
			p.MinStanding = clampStanding(v)
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("weight")), 64); err == nil && v > 0 {
			p.Weight = v
		}
		p.Notes = strings.TrimSpace(r.FormValue("notes"))
		if len(p.Notes) > 255 {
			p.Notes = p.Notes[:255]
		}
		if err := saveCorpPolicy(ctx, m.db, &p); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, "saved", "")

	case "defaults":
		def := defaultCorpPolicy(*c)
		// Same rule as the bulk retag: the derived answer owns the
		// description of the outfit, never the door.
		p := c.Policy
		p.Tags, p.Restricted = def.Tags, def.Restricted
		if err := saveCorpPolicy(ctx, m.db, &p); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, "tags and the default door reset to the derived answer", "")

	case "promote":
		// The CEO's weekly review, on demand: an NPC corporation reshuffles
		// its seats.
		summary, err := m.promoteCorp(ctx, corpID)
		if err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, summary, "")

	case "demote":
		charID := parseCorpID(r.FormValue("charID"))
		if charID == 0 {
			redirect(w, r, self, "", "no pilot named")
			return
		}
		if err := m.demoteCorpRank(ctx, corpID, charID); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, "seat removed", "")

	case "kickall":
		msg, errMsg := m.kickAllMessage(ctx, corpID)
		redirect(w, r, self, msg, errMsg)

	default:
		http.Redirect(w, r, self, http.StatusSeeOther)
	}
}

const tmplCorpNPC = corpTabs + `{{define "content"}}` + castTabsStyle + `
<h2>{{.Corp.Name}}{{if .Corp.Ticker}} <span style="font-weight:normal;color:#6b7280">[{{.Corp.Ticker}}]</span>{{end}}</h2>
{{template "rb_corptabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
<p><a href="/rolebots/corps/npc">&larr; Back to the NPC corporations</a></p>

<div class="card">
  <h3>What the game says</h3>
  <table class="tbl">
    <tr><th>Faction</th><td>{{if .Corp.FactionName}}{{.Corp.FactionName}}{{else}}—{{end}} <span style="color:#6b7280">({{.Corp.BlocLabel}} bloc)</span></td></tr>
    <tr><th>Activity</th><td>{{.Corp.ActivityName}}{{if .Corp.Activity2ID}} + {{.Corp.Activity2Name}}{{end}}</td></tr>
    <tr><th>Headquarters</th><td>{{if .Corp.StationName}}{{.Corp.StationName}}{{else}}—{{end}}{{if .Corp.SystemName}} — {{.Corp.SystemName}}{{if .Corp.RegionName}} ({{.Corp.RegionName}}){{end}}{{end}}</td></tr>
    <tr><th>Stations owned</th><td>{{.Corp.Stations}}</td></tr>
    <tr><th>Player pilots</th><td>{{.Corp.Members}} <span style="color:#6b7280">(NPC agents excluded)</span></td></tr>
    <tr><th>Rostered bots here</th><td>{{.Corp.Bots}}</td></tr>
  </table>
</div>

<div class="card">
  <h3>Policy</h3>
  <form method="POST">
    <input type="hidden" name="action" value="save">
    <p>
      <label><input type="checkbox" name="joinable" value="1"{{if .Policy.Joinable}} checked{{end}}> <b>Bots may join this corporation</b></label><br>
      <label><input type="checkbox" name="restricted" value="1"{{if .Policy.Restricted}} checked{{end}}> Closed by default — seeds shut, and a bulk open skips it</label>
    </p>
    <p style="margin-top:10px"><b>Combat wing</b> —
      <b>{{.Doctrine.Label}}</b>. {{.Doctrine.Blurb}}
      {{if .WingPct}}
      Up to <b>{{.WingPilots}}</b> of this corporation's pilots may be under arms at once
      ({{.WingPct}}% of the roster).
      {{else}}
      Nothing this corporation does puts a hull where something could shoot it, so it fields nobody.
      {{end}}
      <span style="font-size:12px;color:#6b7280">Derived from the tags below.</span></p>
    <p><b>Tags</b> — what kind of outfit this is.</p>
    <div style="display:flex;flex-wrap:wrap;gap:6px 18px">
      {{range .Tags}}
      <label style="width:210px" title="{{.Blurb}}">
        <input type="checkbox" name="tag" value="{{.Key}}"{{if $.Policy.HasTag .Key}} checked{{end}}> {{.Label}}
      </label>
      {{end}}
    </div>
    <p style="margin-top:8px">
      Extra tags <input type="text" name="extraTags" value="" placeholder="comma separated" style="width:260px">
      <span style="font-size:12px;color:#6b7280">Custom tags display and filter only. They score nothing.</span>
    </p>
    <p>
      Race rule
      <select name="raceRule">
        {{range .RaceRules}}<option value="{{.Key}}"{{if eq .Key $.Policy.RaceRule}} selected{{end}}>{{.Label}}</option>{{end}}
      </select>
      &nbsp; Minimum standing toward the faction
      <input type="number" step="0.1" min="-10" max="10" name="minStanding" value="{{printf "%.1f" .Policy.MinStanding}}" style="width:90px">
      &nbsp;
      Appeal weight
      <input type="number" step="0.05" min="0.05" name="weight" value="{{printf "%.2f" .Policy.Weight}}" style="width:90px">
    </p>
    <p>Notes <input type="text" name="notes" value="{{.Policy.Notes}}" style="width:60%"></p>
    <button type="submit">Save</button>
  </form>
  <form method="POST" style="margin-top:8px">
    <input type="hidden" name="action" value="defaults">
    <button type="submit">Reset tags and the default door to the derived answer</button>
  </form>
  {{if .Corp.Bots}}
  <form method="POST" style="margin-top:8px" onsubmit="return confirm('Send all {{.Corp.Bots}} rolebot(s) in {{.Corp.Name}} back to their school corporations?')">
    <input type="hidden" name="action" value="kickall">
    <button type="submit">Kick all {{.Corp.Bots}} bot(s) back to school</button>
    <span style="font-size:12px;color:#6b7280">&mdash; each returns to the corporation it was created in. Shutting the door only stops new arrivals.</span>
  </form>
  {{end}}
  <p style="margin-top:8px;font-size:12px;color:#6b7280">
    <b>Weight</b> scales this corporation's appeal to every pilot. <b>Minimum standing</b> refuses any pilot below it.
    Standing of −7.5 or worse toward the faction is always refused.
  </p>
</div>

<div class="card">
  <h3>Chain of command <span style="font-weight:normal;color:#6b7280">(fleet commanders)</span></h3>
  <p style="font-size:13px">
    {{if .Chair}}<b>{{.Chair}}.</b> The chair re-deals the seats daily.<br>{{end}}
    Only a seated commander may take a fleet out. Activities{{if .FleetActs}}: <b>{{range $i, $a := .FleetActs}}{{if $i}}, {{end}}{{$a}}{{end}}</b>{{else}}: <b style="color:#ef4444">none</b> (tag it mining, combat, security or outlaw to start organizing){{end}}.
    Seats by headcount: <b>{{.FleetSeats}}</b>.
  </p>
  {{if .Ranks}}
  <table class="tbl">
    <tr><th>Seat</th><th>Pilot</th><th>Fleet</th><th>Score</th><th>Appointed</th><th></th></tr>
    {{range .Ranks}}
    <tr>
      <td>{{.Seat}}</td>
      <td><a href="/rolebot/{{.CharID}}">{{if .Name}}{{.Name}}{{else}}{{.CharID}}{{end}}</a>{{if .Manual}} <span style="color:#eab308">(appointed)</span>{{end}}</td>
      <td>{{.Activity}}</td>
      <td>{{printf "%.0f" .Score}}</td>
      <td>{{.AppointedText}}</td>
      <td>
        <form method="POST" style="display:inline">
          <input type="hidden" name="action" value="demote">
          <input type="hidden" name="charID" value="{{.CharID}}">
          <button type="submit">remove</button>
        </form>
      </td>
    </tr>
    {{end}}
  </table>
  {{else}}
  <p>No seated commanders.</p>
  {{end}}
  <form method="POST" style="margin-top:8px">
    <input type="hidden" name="action" value="promote">
    <button type="submit">Run the promotion pass now</button>
    <span style="font-size:12px;color:#6b7280">&mdash; ranks the roster and fills the seats. In game: <code>/rolebots promote {{.Corp.CorpID}}</code>.</span>
  </form>
</div>

<div class="card">
  <h3>The race rule for this corporation</h3>
  {{if .Corp.School}}
  <p style="margin-bottom:10px;padding:8px;border-left:3px solid #eab308">
    <b>This is a school.</b> Pilots start here and are never sent to it. Opening the door changes nothing.
  </p>
  {{end}}
  {{if eq .Corp.BlocLabel "empire"}}
  <p style="margin-bottom:10px;padding:8px;border-left:3px solid #22c55e">
    <b>This is a {{.CorpRace}} corporation and it employs {{.CorpRace}} pilots.</b> Change the race rule above to override it.
  </p>
  {{end}}
  {{if .Corp.Tribe}}
  <p style="margin-bottom:10px;padding:8px;border-left:3px solid #eab308">
    <b>{{.Corp.Tribe}} is a tribe.</b>
    {{if .Corp.TribeBloodlineID}}
      It recruits <b>{{.Corp.Tribe}} pilots only</b>.
    {{else}}
      It takes <b>any Minmatar</b>.
    {{end}}
    Set the race rule above to <i>open</i> to switch this off.
  </p>
  {{end}}
  {{if eq .Corp.BlocLabel "empire"}}
  <p style="font-size:12px;color:#6b7280;margin-bottom:6px">
    What the <i>Bloc rule</i> setting above would do.
  </p>
  <table class="tbl">
    <tr><th>Empire</th><th>{{.Corp.FactionName}} → them</th><th>Them → {{.Corp.FactionName}}</th><th>Takes their pilots</th></tr>
    {{range .BlocRows}}
    <tr>
      <td>{{.Empire}}</td>
      <td>{{printf "%+.1f" .Theirs}}</td>
      <td>{{printf "%+.1f" .Ours}}</td>
      <td>{{if .Takes}}<b style="color:#22c55e">yes</b>{{else}}<span style="color:#ef4444">no</span>{{end}}</td>
    </tr>
    {{end}}
  </table>
  <p style="margin-top:8px;font-size:12px;color:#6b7280">
    Under the bloc rule, a negative standing on either side shuts the door.
  </p>
  {{else if eq .Corp.BlocLabel "outlaw"}}
  <p>An outlaw corporation takes <b>any race</b> but refuses <b>lawful</b> pilots.</p>
  {{else}}
  <p>An unaligned corporation has <b>no race rule</b>. Only the pilot's standing toward
     {{if .Corp.FactionName}}{{.Corp.FactionName}}{{else}}its faction{{end}} and this page's settings decide.</p>
  {{end}}
</div>

<div class="card">
  <h3>Who would walk in <span style="font-weight:normal;color:#6b7280">(dry run — nothing is written)</span></h3>
  <p style="font-size:12px;color:#6b7280">
    Every <b>enabled</b> rostered pilot. <b>{{.Eligible}}</b> would join. Score is fit &times; standing &times; home &times; crowd &times; weight.
    <b>Crowd</b> is how full this corporation is against the average.
  </p>
  {{if .Candidates}}
  <table class="tbl">
    <tr><th>Pilot</th><th>Archetype</th><th>Now in</th><th>Score</th><th>Fit</th><th>Standing</th><th>Home</th><th>Crowd</th><th>Jumps</th><th>Why</th></tr>
    {{range .Candidates}}
    <tr{{if not .Eligible}} style="opacity:.55"{{end}}>
      <td><a href="/rolebot/{{.CharID}}">{{.Name}}</a></td>
      <td>{{if .Archetype}}{{.Archetype}}{{else}}—{{end}}</td>
      <td>{{.CorpName}}</td>
      <td>{{if .Eligible}}{{printf "%.2f" .Score}} <span style="color:#6b7280">({{.Bar}}%)</span>{{else}}<span style="color:#ef4444">refused</span>{{end}}</td>
      <td>{{printf "%.2f" .Fit}}</td>
      <td>&times;{{printf "%.2f" .Standing}}</td>
      <td>&times;{{printf "%.2f" .Home}}</td>
      <td{{if lt .Crowd 0.8}} style="color:#eab308"{{end}}>&times;{{printf "%.2f" .Crowd}}</td>
      <td>{{.Jumps}}</td>
      <td>{{.Why}}{{if not .Eligible}} <span style="color:#6b7280">[{{.Rule}}]</span>{{end}}</td>
    </tr>
    {{end}}
  </table>
  {{else}}
  <p>No enabled rolebots on the roster to price.</p>
  {{end}}
</div>
{{end}}`
