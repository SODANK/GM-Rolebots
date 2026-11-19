package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gurimoon/esp"
)

// The CEOs tab: /rolebots/ceos, one card per bot CEO, laid out like the Main Cast. The corporation
// itself is /rolebots/corp/<id> (espcorp_page.go). The Corps pages share one sidebar entry behind a
// sub-tab strip (rb_corptabs).

const corpTabs = `{{define "rb_corptabs"}}
<div class="rb-subtabs">
  <a href="/rolebots/corps"{{if eq .Tab "corps"}} class="on"{{end}}>Corps</a>
  <a href="/rolebots/ceos"{{if eq .Tab "ceos"}} class="on"{{end}}>CEOs</a>
  <a href="/rolebots/corps/npc"{{if eq .Tab "npc"}} class="on"{{end}}>NPC corps</a>
  <a href="/rolebots/corps/alliances"{{if eq .Tab "alliances"}} class="on"{{end}}>Alliances</a>
  <a href="/rolebots/corps/diplomacy"{{if eq .Tab "diplomacy"}} class="on"{{end}}>Diplomacy</a>
  <a href="/rolebots/corps/departments"{{if eq .Tab "departments"}} class="on"{{end}}>Departments</a>
  <a href="/rolebots/corps/morale"{{if eq .Tab "morale"}} class="on"{{end}}>Morale</a>
</div>
{{end}}`

// charName is a best-effort character name.
func (m *Mod) charName(ctx context.Context, charID int64) string {
	var name string
	if err := m.db.QueryRowContext(ctx,
		`SELECT characterName FROM character_ WHERE characterID = ?`, charID).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("character %d", charID)
	}
	return name
}

// ceoOfCorp is the corporation charID runs as a bot CEO, 0 for none.
func ceoOfCorp(charID int64) int64 {
	corpID, _ := managedCEOOf(charID)
	return corpID
}

// corpPageLinks fills the core's /corporation/<id> page with a link to the
// rolebot page for the corporation (esp.CorpPageLinks).
func (m *Mod) corpPageLinks(ctx context.Context, corpID int64) []esp.CorpPageLink {
	link := esp.CorpPageLink{Label: "Rolebot corporation →", Href: fmt.Sprintf("/rolebots/corp/%d", corpID)}
	if ceoID, ok := managedCEO(corpID); ok {
		link.Note = "run by " + m.charName(ctx, ceoID)
	}
	return []esp.CorpPageLink{link}
}

// ── the list ────────────────────────────────────────────────────────────────

type ceoListRow struct {
	CorpID     int64
	CorpName   string
	Ticker     string
	CharID     int64
	CEOName    string
	Lead       int
	Traits     []string
	Members    int
	Limit      int
	Bots       int
	Balance    string
	Net        string
	NetNeg     bool
	TaxPct     float64
	Tags       string
	Where      string
	Directive  string
	LastAction string
	LastAge    string
	Paused     bool
	NextThink  string
	Origin     string
	// Managed is false for a rostered bot that sits in a player corporation's chair without running it:
	// CEO in the game, deciding nothing. That is a deactivated pilot (an active one takes charge at the
	// next CEO sweep, reconcileCEOSeats), and Active tells the two apart.
	Managed bool
	Active  bool
}

// rosterCEOSeats is every player corporation whose CEO is a rostered bot, managed or not. The chair
// is the game's fact (corporation.ceoID); whether the module acts on it is rolebot_ceos.
func (m *Mod) rosterCEOSeats(ctx context.Context) (map[int64]int64, map[int64]bool, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT co.corporationID, co.ceoID, b.enabled
		  FROM corporation co
		  JOIN rolebot_characters b ON b.characterID = co.ceoID
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = co.corporationID
		 WHERE n.corporationID IS NULL AND COALESCE(co.deleted, 0) = 0`)
	if err != nil {
		return nil, nil, fmt.Errorf("rosterCEOSeats: %w", err)
	}
	defer rows.Close()
	out, active := map[int64]int64{}, map[int64]bool{}
	for rows.Next() {
		var corpID, charID, en int64
		if rows.Scan(&corpID, &charID, &en) == nil {
			out[corpID] = charID
			active[charID] = en != 0
		}
	}
	return out, active, rows.Err()
}

func (m *Mod) handleCEOs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if r.Method == http.MethodPost {
		m.handleCEOPost(w, r, ctx, "/rolebots/ceos")
		return
	}
	ceos, err := listCEOs(ctx, m.db)
	if err != nil {
		espError(w, "rolebot-ceos", err.Error())
		return
	}
	seats, active, err := m.rosterCEOSeats(ctx)
	if err != nil {
		espError(w, "rolebot-ceos", err.Error())
		return
	}
	byCorp := map[int64]*corpCEO{}
	for _, c := range ceos {
		byCorp[c.CorpID] = c
		seats[c.CorpID] = c.CharID
	}
	wallets := m.corpWallets(ctx)
	nets := m.corpNets(ctx, clock.Unix()-ceoBooksWindowSec)

	var rows []ceoListRow
	for corpID, charID := range seats {
		facts, err := getAnyCorpFacts(ctx, m.db, corpID)
		if err != nil {
			continue
		}
		row := ceoListRow{
			CorpID: corpID, CorpName: facts.Name, Ticker: facts.Ticker,
			CharID: charID, CEOName: m.charName(ctx, charID),
			Members: facts.Members, Limit: facts.MemberLimit, Bots: facts.Bots,
			TaxPct: facts.TaxRate * 100, Tags: facts.Policy.TagCSV(),
			Where:   facts.SystemName,
			Balance: iskString(wallets[corpID]),
			Active:  active[charID],
		}
		if tr, err := getTraits(ctx, m.db, charID); err == nil {
			row.Lead = tr[TraitLeadership]
			row.Traits = traitHighlights(tr, 4)
		}
		if c := byCorp[corpID]; c != nil {
			row.Managed, row.Paused, row.Origin = true, c.Paused, c.Origin
			row.Net, row.NetNeg = iskString(nets[corpID]), nets[corpID] < 0
			if d, _, err := m.loadCorpDirective(ctx, corpID); err == nil {
				row.Directive = directiveText(d)
			}
			if acts, err := m.listCEOActions(ctx, corpID, "", "", 1, 0); err == nil && len(acts) > 0 {
				row.LastAction, row.LastAge = acts[0].Detail, acts[0].AgeText()
			}
			if left := c.NextThinkAt - clock.Unix(); left > 0 {
				row.NextThink = "in " + fmtDuration(left)
			} else {
				row.NextThink = "due"
			}
		}
		rows = append(rows, row)
	}
	// Running CEOs first, then by name.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Managed != rows[j].Managed {
			return rows[i].Managed
		}
		return rows[i].CorpName < rows[j].CorpName
	})

	type humanSwitchRow struct {
		Key, Label string
		On         bool
	}
	type humanDialRow struct {
		Key, Label string
		Value      int64
	}
	var sws []humanSwitchRow
	for _, sw := range humanSwitches() {
		sws = append(sws, humanSwitchRow{sw.key, sw.label, sw.cache.Load()})
	}
	var dials []humanDialRow
	for _, d := range humanDials() {
		dials = append(dials, humanDialRow{d.key, d.label, d.get()})
	}

	espRender(w, "rolebots_ceos.html", map[string]any{
		"Page":        "rolebot-corps",
		"Tab":         "ceos",
		"Msg":         r.URL.Query().Get("msg"),
		"Err":         r.URL.Query().Get("err"),
		"Rows":        rows,
		"HumanSwitch": sws,
		"HumanDials":  dials,
	})
}

// handleCEOPost is every action behind the corporation pages and the CEOs tab. It has no pause,
// per-power switches or review-now: the pilot's roster switch decides whether a CEO runs
// (reconcileCEOSeats).
func (m *Mod) handleCEOPost(w http.ResponseWriter, r *http.Request, ctx context.Context, back string) {
	if err := r.ParseForm(); err != nil {
		redirect(w, r, back, "", err.Error())
		return
	}
	corpID := parseCorpID(r.FormValue("corpID"))
	switch r.FormValue("action") {
	case "humanswitch":
		key := r.FormValue("key")
		if err := m.setHumanSwitch(ctx, key, r.FormValue("on") == "1"); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		redirect(w, r, back, "saved "+key, "")
		return
	case "humandial":
		key := r.FormValue("key")
		n, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("value")), 10, 64)
		if err != nil {
			redirect(w, r, back, "", "not a whole number")
			return
		}
		if err := m.setHumanDial(ctx, key, n); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		redirect(w, r, back, "saved "+key, "")
		return

	case "declare", "handback":
		// The Relations card (espcorp_relations.go).
		m.handleCorpRelationsPost(w, r, ctx, back, corpID)
		return

	case "toggle":
		msg, err := m.setCorpDoor(ctx, corpID, r.FormValue("state") == "on")
		if err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		redirect(w, r, back, msg, "")

	case "withdraw":
		charID := parseCorpID(r.FormValue("charID"))
		if charID == 0 || corpID == 0 {
			redirect(w, r, back, "", "no application named")
			return
		}
		if err := m.withdrawApplicationByID(ctx, charID, corpID); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		redirect(w, r, back, "application withdrawn", "")

	case "kickall":
		msg, errMsg := m.kickAllMessage(ctx, corpID)
		redirect(w, r, back, msg, errMsg)

	case "resume":
		// Only for a CEO somebody paused before the Pause button went: nothing
		// pauses one any more, and a paused CEO would otherwise stay that way.
		c, err := getCEO(ctx, m.db, corpID)
		if err != nil || c == nil {
			redirect(w, r, back, "", "no such CEO")
			return
		}
		c.Paused = false
		if err := saveCEO(ctx, m.db, c); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		redirect(w, r, back, "resumed", "")

	case "hqmove":
		// The HQ ban: on by default, lifted only here. Offices are rented
		// either way; this decides only whether the headquarters may follow.
		c, err := getCEO(ctx, m.db, corpID)
		if err != nil || c == nil {
			redirect(w, r, back, "", "no such CEO")
			return
		}
		c.HQMovable = r.FormValue("on") == "1"
		if err := saveCEO(ctx, m.db, c); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		if c.HQMovable {
			redirect(w, r, back, "HQ ban lifted — the CEO may move the headquarters", "")
		} else {
			redirect(w, r, back, "HQ ban on — the CEO may rent offices but not move the headquarters", "")
		}

	case "dividend":
		// The operator's dividend override: blank hands a field back to the
		// charter.
		pct, days := -1.0, 0.0
		if v := strings.TrimSpace(r.FormValue("payoutPct")); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 || f > 100 {
				redirect(w, r, back, "", "payout must be a percentage from 0 to 100, or blank for the charter's")
				return
			}
			pct = f
		}
		if v := strings.TrimSpace(r.FormValue("dividendDays")); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 1 || f > 365 {
				redirect(w, r, back, "", "the interval must be 1 to 365 days, or blank for the charter's")
				return
			}
			days = f
		}
		if err := m.setDividendPolicy(ctx, corpID, pct, days); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		redirect(w, r, back, "dividend policy saved", "")

	case "paydividend":
		c, err := getCEO(ctx, m.db, corpID)
		if err != nil || c == nil {
			redirect(w, r, back, "", "no such CEO")
			return
		}
		paid, err := m.payDividend(ctx, c, ceoCharterOf(ctx, m, c), true)
		if err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		if paid <= 0 {
			redirect(w, r, back, "", "nothing to pay — no earnings since the last dividend, no free capital, or no shareholders outside the treasury")
			return
		}
		redirect(w, r, back, "paid a dividend of "+iskString(paid), "")

	case "allegiance":
		// The operator's pledge (allegiance.go): 0 or a faction allegianceChoices lists.
		fid, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("factionID")), 10, 64)
		if err != nil || fid < 0 {
			redirect(w, r, back, "", "not a faction")
			return
		}
		if fid != 0 {
			choices, err := m.allegianceChoices(ctx)
			if err != nil {
				redirect(w, r, back, "", err.Error())
				return
			}
			known := false
			for _, c := range choices {
				if c.ID == fid {
					known = true
					break
				}
			}
			if !known {
				redirect(w, r, back, "", "not a faction a corporation may pledge to")
				return
			}
		}
		facts, err := getAnyCorpFacts(ctx, m.db, corpID)
		if err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		if err := m.setCorpAllegiance(ctx, corpID, fid); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		var ceoChar int64 // the pilot line goes to a bot CEO only; a human chair has no rolebot_log
		if c, err := getCEO(ctx, m.db, corpID); err == nil && c != nil {
			ceoChar = c.CharID
		}
		m.notePledge(ctx, corpID, ceoChar, facts.Name, fid, "declared")
		if fid == 0 {
			redirect(w, r, back, "allegiance cleared", "")
			return
		}
		redirect(w, r, back, "allegiance set to "+m.factionLabel(ctx, fid), "")

	default:
		http.Redirect(w, r, back, http.StatusSeeOther)
	}
}

// ── templates ───────────────────────────────────────────────────────────────

const tmplCEOs = corpTabs + `{{define "content"}}` + castTabsStyle + rbCardStyle + `
<h2>Bot CEOs</h2>
{{template "rb_corptabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<div class="card">
  <h3>The CEOs <span style="font-size:12px;color:#6b7280">({{len .Rows}})</span></h3>
  <p style="font-size:12px;color:#6b7280;margin-top:-4px">
    Player corporations whose CEO is a rostered pilot. <b>Running</b> means an active bot is in charge. <b>Seated</b> means it is deactivated.
  </p>
  {{if .Rows}}
  <div class="rb-cast">
    {{range .Rows}}
    <div class="rb-card{{if .Paused}} halt{{end}}">
      <div class="hd">
        <img src="/portrait/{{.CharID}}?size=128" width="64" height="64"
             onerror="this.style.visibility='hidden'" loading="lazy" alt="">
        <div>
          <div class="nm"><a href="/rolebot/{{.CharID}}">{{.CEOName}}</a></div>
          <div class="co"><a href="/rolebots/corp/{{.CorpID}}">{{.CorpName}}</a>{{if .Ticker}} <span class="id">[{{.Ticker}}]</span>{{end}}</div>
          <div style="margin-top:5px">
            {{if .Managed}}{{if .Paused}}<span class="rb-pill warn">paused</span>{{else}}<span class="rb-pill on">running</span>{{end}}
            {{else if .Active}}<span class="rb-pill" title="takes charge at the next sweep">taking charge</span>
            {{else}}<span class="rb-pill" title="the pilot is inactive — activate it to hand it the corporation">seated</span>{{end}}
          </div>
        </div>
      </div>

      {{if .Traits}}<div>{{range .Traits}}<span class="rb-chip">{{.}}</span>{{end}}</div>{{end}}

      <dl>
        <dt>Members</dt>
        <dd>{{.Members}}{{if .Limit}} / {{.Limit}}{{end}}{{if .Bots}} <span style="color:#6b7280">({{.Bots}} bot{{if ne .Bots 1}}s{{end}})</span>{{end}}</dd>
        <dt>Wallet</dt>
        <dd>{{.Balance}}{{if .Managed}} <span style="color:#6b7280">&middot; 7-day net</span> {{if .NetNeg}}<span style="color:#ef4444">{{.Net}}</span>{{else}}{{.Net}}{{end}}{{end}}</dd>
        <dt>Leader trait</dt>
        <dd>{{.Lead}}</dd>
        {{if .Managed}}
        <dt>Directive</dt>
        <dd>{{if .Directive}}{{.Directive}}{{else}}<span style="color:#6b7280">none yet</span>{{end}}</dd>
        <dt>Latest</dt>
        <dd>{{if .LastAction}}{{.LastAction}} <span style="color:#6b7280">{{.LastAge}}</span>{{else}}<span style="color:#6b7280">nothing yet</span>{{end}}</dd>
        <dt>Next review</dt>
        <dd>{{.NextThink}}</dd>
        {{end}}
        <dt>Does</dt>
        <dd>{{if .Tags}}{{.Tags}}{{else}}<span style="color:#6b7280">&mdash;</span>{{end}}</dd>
        <dt>Based in</dt>
        <dd>{{if .Where}}{{.Where}}{{else}}<span style="color:#6b7280">&mdash;</span>{{end}} <span style="color:#6b7280">&middot; tax {{printf "%.0f" .TaxPct}}%</span></dd>
      </dl>

      <div class="rb-acts">
        <a href="/rolebots/corp/{{.CorpID}}" style="font-size:12px;margin-right:8px">Open corporation</a>
        <a href="/rolebot/{{.CharID}}" style="font-size:12px">Profile</a>
      </div>
    </div>
    {{end}}
  </div>
  {{else}}
  <p class="rb-empty">No player corporation has a rostered pilot as its CEO.</p>
  {{end}}
</div>

<div class="card">
<details>
<summary style="cursor:pointer;font-size:15px;font-weight:600">Human members</summary>
  <p style="font-size:12px;color:#6b7280;margin-top:8px">What a player in a bot-run corporation is told and offered.
  The merit ladder applies to <b>every</b> member, bots included, and grants and strips real corp roles.
  Refusing a human applicant is the CEO's Tolerance, not a switch.</p>
  <table class="tbl" style="font-size:12px">
    {{range .HumanSwitch}}
    <tr><td>{{.Label}}</td><td>{{if .On}}<b style="color:#16a34a">on</b>{{else}}<span style="color:#6b7280">off</span>{{end}}</td>
      <td><form method="post" style="margin:0">
        <input type="hidden" name="action" value="humanswitch"><input type="hidden" name="key" value="{{.Key}}">
        <input type="hidden" name="on" value="{{if .On}}0{{else}}1{{end}}">
        <button class="btn btn-sm">{{if .On}}Turn off{{else}}Turn on{{end}}</button></form></td></tr>
    {{end}}
    {{range .HumanDials}}
    <tr><td>{{.Label}}</td><td colspan="2"><form method="post" style="margin:0">
        <input type="hidden" name="action" value="humandial"><input type="hidden" name="key" value="{{.Key}}">
        <input name="value" value="{{.Value}}" size="5"> <button class="btn btn-sm">Save</button></form></td></tr>
    {{end}}
  </table>
</details>
</div>
{{end}}`
