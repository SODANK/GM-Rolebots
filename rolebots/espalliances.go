package rolebots

// Corps, Alliances: the ESP insight board for alliances: what each one is, who runs it, who is in it, what it
// holds, who it fights, and what its corporations have done about it (alliance.go). It is built from the core's
// alliance tables plus the module's book. The only control is the founding switch and its cap; each alliance is
// one collapsed summary row that opens to its full card.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const (
	allianceFeedLimit    = 30
	allianceRelNames     = "NAP|Friend|Competitor|Enemy"
	allianceBoardSelfURL = "/rolebots/corps/alliances"
)

// allianceCardMember is one member row on a card.
type allianceCardMember struct {
	CorpID       int64
	Name, Ticker string
	Pilots       int64
	BotCEO       bool
	CEOID        int64
	CEOName      string
	IsExecutor   bool
	Chosen       string
	Standing     string // the executor's standing toward it
	Owed, Paid   string
	Refused      int
	DuesUnpaid   int
	Towers       int
	TowersOnline int
}

type allianceWarRow struct {
	OtherID       int64
	Other         string
	Side          string // "declared" or "defending"
	Hostile       bool
	Declared      string
}

type allianceRelRow struct {
	ToID   int64
	ToName string
	Rel    string
	Mine   bool // the alliance's own declaration (false: another alliance's about it)
}

type allianceAppRow struct {
	CorpID  int64
	Corp    string
	State   string
	Applied string
	Human   bool
}

type allianceFeedRow struct {
	At    int64
	When  string
	Actor string
	Verb  string
	Line  string
}

// allianceCard is one alliance as the board draws it.
type allianceCard struct {
	ID            int64
	Name, Ticker  string
	Age           string
	ExecID        int64
	Executor      string
	ExecBot       bool
	ExecCEOName   string
	ExecCEOID     int64
	Dictatorial   bool
	BotFounded    bool
	Deleted       bool
	Members       []allianceCardMember
	Pilots        int64
	BotCorps      int
	Sov           []string
	Towers        int
	TowersOnline  int
	Wars          []allianceWarRow
	Rels          []allianceRelRow
	Apps          []allianceAppRow
	Targets       []string
	Forward       []string // member corporations' forward offices (frontier.go)
	SovMotive     bool     // founded by a bot to hold 0.0
	OpenRequests  int
	Feed          []allianceFeedRow
	Health        string
	HealthWhy     string
	Duties        string
	OwedTotal     string
	PaidTotal     string
}

// handleCorpsAlliances is /rolebots/corps/alliances.
func (m *Mod) handleCorpsAlliances(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	self := allianceBoardSelfURL

	if r.Method == http.MethodPost {
		switch r.FormValue("action") {
		case "found":
			if err := m.setAllianceFound(ctx, r.FormValue("on") == "1"); err != nil {
				redirect(w, r, self, "", err.Error())
				return
			}
			redirect(w, r, self, "saved", "")
		case "foundmax":
			n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("value")))
			if err != nil {
				redirect(w, r, self, "", "a whole number")
				return
			}
			if err := m.setAllianceFoundMax(ctx, n); err != nil {
				redirect(w, r, self, "", err.Error())
				return
			}
			redirect(w, r, self, "saved", "")
		default:
			redirect(w, r, self, "", "unknown action")
		}
		return
	}

	cards, err := m.allianceCards(ctx)
	if err != nil {
		espError(w, "rolebot-alliances", err.Error())
		return
	}
	var live, bots int
	var standing, dissolved []allianceCard
	for _, c := range cards {
		if c.Deleted {
			dissolved = append(dissolved, c)
			continue
		}
		standing = append(standing, c)
		live++
		if c.ExecBot {
			bots++
		}
	}
	espRender(w, "rolebots_alliances.html", map[string]any{
		"Page":      "rolebot-corps",
		"Tab":       "alliances",
		"Msg":       r.URL.Query().Get("msg"),
		"Err":       r.URL.Query().Get("err"),
		"FoundOn":   allianceFoundOn(),
		"FoundMax":  allianceFoundMax(),
		"FoundLive": m.liveFoundedAlliances(ctx),
		"FoundFee":  fmt.Sprintf("%.0fM", allianceFoundFee/1e6),
		"Cards":     standing,
		"Dissolved": dissolved,
		"Live":      live,
		"BotRun":    bots,
	})
}

// allianceCards builds every card. Each alliance's reads are best effort:
// a failed panel leaves its slice empty rather than blanking the board.
func (m *Mod) allianceCards(ctx context.Context) ([]allianceCard, error) {
	rows, err := evedb.ESPListAlliancesFull(ctx, m.db)
	if err != nil {
		return nil, fmt.Errorf("alliances: %w", err)
	}
	founded := map[int64]bool{}
	if fr, err := m.db.QueryContext(ctx, `SELECT allianceID FROM rolebot_founded_alliances`); err == nil {
		for fr.Next() {
			var id int64
			if fr.Scan(&id) == nil {
				founded[id] = true
			}
		}
		fr.Close()
	}
	wars, _ := evedb.ListWars(ctx, m.db)
	now := clock.Unix()
	var cards []allianceCard
	for _, a := range rows {
		c := allianceCard{
			ID: a.AllianceID, Name: a.AllianceName, Ticker: a.ShortName,
			ExecID: a.ExecutorCorpID, Executor: a.ExecutorName, Dictatorial: a.Dictatorial,
			Deleted: a.Deleted, BotFounded: founded[a.AllianceID], Pilots: a.Pilots,
		}
		if a.StartDate > 0 {
			c.Age = fmtDuration(now - win32ToUnix(a.StartDate))
		}
		if a.Deleted {
			cards = append(cards, c)
			continue
		}
		if ceoID, ok := managedCEO(a.ExecutorCorpID); ok {
			c.ExecBot, c.ExecCEOID, c.ExecCEOName = true, ceoID, m.charName(ctx, ceoID)
		}
		m.fillAllianceMembers(ctx, &c)
		m.fillAllianceSov(ctx, &c)
		m.fillAllianceWars(ctx, &c, wars)
		m.fillAllianceRels(ctx, &c)
		m.fillAllianceApps(ctx, &c)
		m.fillAllianceTargets(ctx, &c)
		m.fillAllianceFeed(ctx, &c)
		c.Health, c.HealthWhy = allianceHealth(m.allianceHealthInput(ctx, &c))
		c.Duties = allianceDutiesLine(&c)
		cards = append(cards, c)
	}
	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].Deleted != cards[j].Deleted {
			return !cards[i].Deleted
		}
		return cards[i].Pilots > cards[j].Pilots
	})
	return cards, nil
}

func (m *Mod) fillAllianceMembers(ctx context.Context, c *allianceCard) {
	members, err := evedb.ListAllianceMembers(ctx, m.db, c.ID)
	if err != nil {
		return
	}
	bal := m.titheBalances(ctx, c.ID)
	var owed, paid float64
	for _, mb := range members {
		row := allianceCardMember{
			CorpID: mb.CorporationID, Name: mb.CorporationName, Ticker: mb.TickerName,
			Pilots: mb.Pilots, IsExecutor: mb.IsExecutor, Chosen: "secret",
		}
		if mb.ChosenExecutorID != 0 {
			row.Chosen = mb.ChosenName
			if row.Chosen == "" {
				row.Chosen = fmt.Sprintf("#%d", mb.ChosenExecutorID)
			}
		}
		if ceoID, ok := managedCEO(mb.CorporationID); ok {
			row.BotCEO, row.CEOID, row.CEOName = true, ceoID, m.charName(ctx, ceoID)
			c.BotCorps++
		} else {
			_ = m.db.QueryRowContext(ctx, `SELECT ceoID FROM corporation WHERE corporationID = ?`, mb.CorporationID).Scan(&row.CEOID)
			if row.CEOID != 0 {
				row.CEOName = m.charName(ctx, row.CEOID)
			}
		}
		if c.ExecID != 0 && c.ExecID != mb.CorporationID {
			row.Standing = fmt.Sprintf("%+.1f", relCorpStanding(c.ExecID, mb.CorporationID))
		}
		if b := bal[mb.CorporationID]; b != nil {
			row.Owed, row.Paid, row.Refused = iskString(b.Owed), iskString(b.Paid), b.Refused
			owed += b.Owed
			paid += b.Paid
		}
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM bills WHERE debtorID = ? AND billTypeID = ? AND paid = 0`,
			mb.CorporationID, evedb.BillTypeAllianceMaintainance).Scan(&row.DuesUnpaid)
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(state = 'online'), 0) FROM starbase WHERE ownerCorpID = ?`,
			mb.CorporationID).Scan(&row.Towers, &row.TowersOnline)
		c.Towers += row.Towers
		c.TowersOnline += row.TowersOnline
		c.Members = append(c.Members, row)
	}
	sort.SliceStable(c.Members, func(i, j int) bool {
		if c.Members[i].IsExecutor != c.Members[j].IsExecutor {
			return c.Members[i].IsExecutor
		}
		return c.Members[i].Pilots > c.Members[j].Pilots
	})
	c.OwedTotal, c.PaidTotal = iskString(owed), iskString(paid)
}

func (m *Mod) fillAllianceSov(ctx context.Context, c *allianceCard) {
	ids, err := evedb.ListSovForAlliance(ctx, m.db, c.ID)
	if err != nil {
		return
	}
	for _, id := range ids {
		c.Sov = append(c.Sov, systemNameOf(ctx, m, id))
	}
	sort.Strings(c.Sov)
}

func (m *Mod) fillAllianceWars(ctx context.Context, c *allianceCard, wars []evedb.WarView) {
	for _, w := range wars {
		if !w.Active {
			continue
		}
		row := allianceWarRow{Hostile: w.HostileNow, Declared: fmtWin32Date(w.TimeDeclared)}
		switch {
		case w.DeclaredByID == c.ID:
			row.OtherID, row.Other, row.Side = w.AgainstID, w.AgainstName, "declared on"
		case w.AgainstID == c.ID:
			row.OtherID, row.Other, row.Side = w.DeclaredByID, w.DeclaredByName, "defending against"
		default:
			continue
		}
		c.Wars = append(c.Wars, row)
	}
}

func (m *Mod) fillAllianceRels(ctx context.Context, c *allianceCard) {
	if rels, err := evedb.ListAllianceRelationships(ctx, m.db, c.ID); err == nil {
		for _, r := range rels {
			c.Rels = append(c.Rels, allianceRelRow{ToID: r.ToID, ToName: r.ToName, Rel: evedb.AllianceRelationshipName(r.Relationship), Mine: true})
		}
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.allianceID, COALESCE(a.allianceName, ''), r.relationship
		  FROM allianceRelationships r LEFT JOIN alliances a ON a.allianceID = r.allianceID
		 WHERE r.toID = ?`, c.ID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var row allianceRelRow
		var rel int64
		if rows.Scan(&row.ToID, &row.ToName, &rel) == nil {
			row.Rel = evedb.AllianceRelationshipName(rel)
			c.Rels = append(c.Rels, row)
		}
	}
}

func (m *Mod) fillAllianceApps(ctx context.Context, c *allianceCard) {
	apps, err := evedb.ListAllianceApplications(ctx, m.db, c.ID)
	if err != nil {
		return
	}
	for _, a := range apps {
		if a.State == evedb.AllianceApplicationEffective || a.State == evedb.AllianceApplicationRejected {
			continue
		}
		row := allianceAppRow{CorpID: a.CorporationID, Corp: a.CorporationName,
			State: evedb.AllianceApplicationStateName(a.State), Applied: fmtWin32Date(a.AppliedAt)}
		_, bot := managedCEO(a.CorporationID)
		row.Human = !bot
		c.Apps = append(c.Apps, row)
	}
}

func (m *Mod) fillAllianceTargets(ctx context.Context, c *allianceCard) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT systemID, kind FROM rolebot_alliance_targets WHERE allianceID = ? ORDER BY score DESC`, c.ID)
	if err == nil {
		for rows.Next() {
			var sys int64
			var kind string
			if rows.Scan(&sys, &kind) == nil {
				c.Targets = append(c.Targets, systemNameOf(ctx, m, sys)+" ("+kind+")")
			}
		}
		rows.Close()
	}
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_alliance_requests WHERE allianceID = ? AND status = 'pending'`, c.ID).Scan(&c.OpenRequests)
	c.SovMotive = m.allianceSovMotive(ctx, c.ID)
	if rows, err := m.db.QueryContext(ctx, `
		SELECT co.corporationName, r.stationID FROM rolebot_corp_offices r
		  JOIN corporation co ON co.corporationID = r.corporationID
		 WHERE co.allianceID = ? AND (r.claimed & ?) <> 0 AND r.state <> 'dropping'
		 ORDER BY co.corporationName`, c.ID, officeForward); err == nil {
		for rows.Next() {
			var corp string
			var st int64
			if rows.Scan(&corp, &st) == nil {
				c.Forward = append(c.Forward, corp+" at "+stationNameOf(ctx, m, st))
			}
		}
		rows.Close()
	}
}

// fillAllianceFeed merges the alliance's book with its member CEOs'
// alliance-power lines, newest first.
func (m *Mod) fillAllianceFeed(ctx context.Context, c *allianceCard) {
	if book, err := m.allianceBookRecent(ctx, c.ID, allianceFeedLimit); err == nil {
		for _, e := range book {
			c.Feed = append(c.Feed, allianceFeedRow{At: e.At, When: fmtUnixWhen(e.At), Actor: e.ActorName, Verb: e.Verb, Line: allianceBookLine(e.Verb, e.Detail)})
		}
	}
	if len(c.Members) > 0 {
		ids := make([]string, 0, len(c.Members))
		for _, mb := range c.Members {
			ids = append(ids, strconv.FormatInt(mb.CorpID, 10))
		}
		rows, err := m.db.QueryContext(ctx, `
			SELECT a.at, a.verb, a.detail, COALESCE(co.corporationName, '')
			  FROM rolebot_ceo_actions a LEFT JOIN corporation co ON co.corporationID = a.corporationID
			 WHERE a.power = ? AND a.corporationID IN (`+strings.Join(ids, ",")+`)
			 ORDER BY a.at DESC LIMIT ?`, powerAlliance, allianceFeedLimit)
		if err == nil {
			for rows.Next() {
				var at int64
				var verb, detail, actor string
				if rows.Scan(&at, &verb, &detail, &actor) == nil {
					c.Feed = append(c.Feed, allianceFeedRow{At: at, When: fmtUnixWhen(at), Actor: actor, Verb: verb, Line: allianceBookLine(verb, detail)})
				}
			}
			rows.Close()
		}
	}
	sort.SliceStable(c.Feed, func(i, j int) bool { return c.Feed[i].At > c.Feed[j].At })
	if len(c.Feed) > allianceFeedLimit {
		c.Feed = c.Feed[:allianceFeedLimit]
	}
}

// allianceHealthInput gathers the health verdict's numbers from a card.
func (m *Mod) allianceHealthInput(ctx context.Context, c *allianceCard) allianceHealthIn {
	in := allianceHealthIn{
		Members: len(c.Members), HasExecutor: c.ExecID != 0, BotExecutor: c.ExecBot,
		ActiveWars: len(c.Wars), SovSystems: len(c.Sov), PendingApps: len(c.Apps), TowersOnline: c.TowersOnline,
	}
	for _, mb := range c.Members {
		in.RefusedTithes += mb.Refused
		if mb.Chosen != "secret" && c.Executor != "" && mb.Chosen != c.Executor {
			in.Disputed++
		}
	}
	if c.ExecBot {
		if ch, ok := m.chairCharterOf(ctx, c.ExecID); ok {
			for _, mb := range c.Members {
				if mb.IsExecutor {
					continue
				}
				if relCorpStanding(c.ExecID, mb.CorpID) < ch.RivalAt {
					in.BelowRival++
				}
			}
		}
	}
	return in
}

// chairCharterOf resolves a managed CEO's charter for a read-only board.
func (m *Mod) chairCharterOf(ctx context.Context, corpID int64) (Charter, bool) {
	ceoID, ok := managedCEO(corpID)
	if !ok {
		return Charter{}, false
	}
	traits, err := getTraits(ctx, m.db, ceoID)
	if err != nil {
		return Charter{}, false
	}
	return resolveCharter(pilotIdentity{CharID: ceoID, Traits: traits, Tune: resolveTuning(traits)}), true
}

// allianceDutiesLine says what the bots do in this alliance.
func allianceDutiesLine(c *allianceCard) string {
	switch {
	case c.BotCorps == 0:
		return "no bot corporations"
	case c.ExecBot:
		return fmt.Sprintf("bot executor; %d of %d corporations bot-run", c.BotCorps, len(c.Members))
	default:
		return fmt.Sprintf("capsuleer-run executor; %d of %d corporations bot-run", c.BotCorps, len(c.Members))
	}
}

// allianceBookLine words one feed row: the detail when the writer left one,
// the verb otherwise. Pure.
func allianceBookLine(verb, detail string) string {
	if strings.TrimSpace(detail) != "" {
		return detail
	}
	switch verb {
	case "dues":
		return "paid its alliance dues"
	case "support":
		return "declared executor support"
	case "tithe":
		return "paid a tithe"
	case "refused":
		return "could not pay a tithe"
	case "joined":
		return "joined"
	case "left", "leave":
		return "left"
	case "kicked", "kick":
		return "expelled a member"
	case "founded", "found":
		return "founded the alliance"
	case "accept":
		return "accepted an application"
	case "reject":
		return "rejected an application"
	}
	return verb
}

// win32ToUnix turns a Win32 FILETIME into unix seconds.
func win32ToUnix(ft int64) int64 { return ft/10000000 - 11644473600 }

func fmtWin32Date(ft int64) string {
	if ft <= 0 {
		return "—"
	}
	return time.Unix(win32ToUnix(ft), 0).UTC().Format("2006-01-02")
}

func fmtUnixWhen(at int64) string {
	if at <= 0 {
		return "—"
	}
	return time.Unix(at, 0).UTC().Format("2006-01-02 15:04")
}

// tmplAlliances is the Alliances tab of the Corps page.
const tmplAlliances = corpTabs + `{{define "content"}}` + castTabsStyle + rbCardStyle + rbTableStyle + `
<h2>Alliances</h2>
{{template "rb_corptabs" .}}
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}

<style>
  .rbal-head, details.rbal > summary {
    display:grid; grid-template-columns:minmax(220px,2.4fr) 110px minmax(160px,1.6fr) 70px 70px 70px 60px 60px;
    gap:8px; align-items:center; padding:6px 8px; font-size:13px;
  }
  .rbal-head { font-size:11px; color:#6b7280; text-transform:uppercase; letter-spacing:.03em; border-bottom:1px solid #374151 }
  .rbal-head span.n, details.rbal > summary span.n { text-align:right }
  details.rbal { border-bottom:1px solid #2d333b; border-left:4px solid #6b7280 }
  details.rbal.h-cohesive { border-left-color:#16a34a }
  details.rbal.h-atwar { border-left-color:#dc2626 }
  details.rbal.h-fracturing { border-left-color:#f59e0b }
  details.rbal > summary { cursor:pointer; list-style:none }
  details.rbal > summary::-webkit-details-marker { display:none }
  details.rbal > summary:hover { background:rgba(148,163,184,.08) }
  details.rbal > summary .nm::before { content:"\25B6 "; font-size:10px; color:#6b7280 }
  details.rbal[open] > summary .nm::before { content:"\25BC "; }
  details.rbal[open] > summary { background:rgba(148,163,184,.06) }
  details.rbal > .rbal-body { padding:8px 12px 14px 12px }
  .rbal-tools { display:flex; gap:8px; align-items:center; margin:6px 0 10px 0; flex-wrap:wrap }
</style>

<div class="card">
  <h3>Founding</h3>
  <p style="font-size:12px;color:#6b7280;margin-top:-4px">
    Bot CEOs always take part in alliances. The one choice here is whether a bot leader may found one.
  </p>
  <table class="rb-table" style="max-width:760px">
    <tr><td>Bot founding — a leader may found an alliance ({{.FoundFee}} ISK from the founder's wallet)</td>
      <td style="width:90px;text-align:right">
        <form method="post" style="display:inline"><input type="hidden" name="action" value="found">
          {{if .FoundOn}}<input type="hidden" name="on" value="0"><button class="btn btn-sm">on → off</button>
          {{else}}<input type="hidden" name="on" value="1"><button class="btn btn-sm btn-primary">off → on</button>{{end}}
        </form></td></tr>
    <tr><td>Founding cap — bot-founded alliances standing: <b>{{.FoundLive}}</b> of <b>{{.FoundMax}}</b></td>
      <td style="text-align:right"><form method="post" style="display:inline"><input type="hidden" name="action" value="foundmax">
        <input name="value" value="{{.FoundMax}}" size="4" style="width:48px"> <button class="btn btn-sm">set</button></form></td></tr>
  </table>
</div>

<div class="card">
  <h3>Alliances <span style="font-size:12px;color:#6b7280">({{.Live}} standing, {{.BotRun}} bot-run)</span></h3>
  {{if not .Cards}}<p style="color:#6b7280">No alliances. Create one on <a href="/alliances">Alliances</a> and add a bot corporation.</p>{{else}}
  <div class="rbal-tools">
    <input type="text" id="rbal-q" placeholder="filter: name, ticker, executor" style="width:240px">
    <button type="button" class="btn btn-sm" id="rbal-open">expand all</button>
    <button type="button" class="btn btn-sm" id="rbal-shut">collapse all</button>
    <span id="rbal-shown" style="font-size:12px;color:#6b7280"></span>
  </div>
  <div class="rbal-head"><span>Alliance</span><span>Verdict</span><span>Executor</span><span class="n">Corps</span><span class="n">Pilots</span><span class="n">Systems</span><span class="n">Wars</span><span class="n">Apps</span></div>
  {{end}}
  {{range .Cards}}
  <details class="rbal {{if eq .Health "cohesive"}}h-cohesive{{else if eq .Health "at war"}}h-atwar{{else if eq .Health "fracturing"}}h-fracturing{{end}}" data-key="{{.Name}} {{.Ticker}} {{.Executor}} {{.ExecCEOName}}">
    <summary>
      <span class="nm"><b>{{.Name}}</b> <span style="color:#6b7280">[{{.Ticker}}]</span>
        {{if .Dictatorial}}<span class="rb-pill warn" title="the seat is frozen">dictatorial</span>{{end}}
        {{if .BotFounded}}<span class="rb-pill" title="founded by a bot CEO">bot</span>{{end}}</span>
      <span><span class="rb-pill {{if eq .Health "cohesive"}}on{{else if eq .Health "fracturing"}}warn{{end}}">{{.Health}}</span></span>
      <span>{{if .ExecID}}{{.Executor}}{{if not .ExecBot}} <span style="color:#9ca3af">(capsuleer)</span>{{end}}{{else}}<span style="color:#9ca3af">none</span>{{end}}</span>
      <span class="n">{{len .Members}}</span>
      <span class="n">{{.Pilots}}</span>
      <span class="n">{{len .Sov}}</span>
      <span class="n">{{if .Wars}}<span style="color:#dc2626">{{len .Wars}}</span>{{else}}0{{end}}</span>
      <span class="n">{{len .Apps}}</span>
    </summary>
    <div class="rbal-body">
    <p style="font-size:12px;color:#6b7280;margin:0 0 6px 0"><a href="/alliance/{{.ID}}">Alliance page</a> &middot; {{.HealthWhy}}. Executor:
      {{if .ExecID}}<a href="/rolebots/corp/{{.ExecID}}">{{.Executor}}</a>{{if .ExecBot}} (bot CEO <a href="/rolebot/{{.ExecCEOID}}">{{.ExecCEOName}}</a>){{else}} (capsuleer-run){{end}}{{else}}none{{end}}.
      {{.Pilots}} pilots{{if .Age}}, {{.Age}} old{{end}}. {{.Duties}}.</p>

    <table class="rb-table">
      <tr><th>Member</th><th>CEO</th><th style="text-align:right">Pilots</th><th>Backs</th><th style="text-align:right">Exec. standing</th><th style="text-align:right">Tithes owed</th><th style="text-align:right">Paid</th><th style="text-align:right">Refused</th><th style="text-align:right">Dues unpaid</th><th style="text-align:right">Towers</th></tr>
      {{range .Members}}
      <tr>
        <td><a href="/rolebots/corp/{{.CorpID}}">{{.Name}}</a>{{if .Ticker}} <span style="color:#9ca3af">[{{.Ticker}}]</span>{{end}}{{if .IsExecutor}} <span class="rb-pill on">executor</span>{{end}}</td>
        <td>{{if .CEOID}}<a href="/rolebot/{{.CEOID}}">{{.CEOName}}</a>{{if .BotCEO}} <span style="color:#9ca3af">bot</span>{{else}} <span style="color:#9ca3af">capsuleer</span>{{end}}{{else}}—{{end}}</td>
        <td style="text-align:right">{{.Pilots}}</td>
        <td>{{.Chosen}}</td>
        <td style="text-align:right">{{if .Standing}}{{.Standing}}{{else}}—{{end}}</td>
        <td style="text-align:right">{{if .Owed}}{{.Owed}}{{else}}—{{end}}</td>
        <td style="text-align:right">{{if .Paid}}{{.Paid}}{{else}}—{{end}}</td>
        <td style="text-align:right">{{if .Refused}}<span style="color:#dc2626">{{.Refused}}</span>{{else}}0{{end}}</td>
        <td style="text-align:right">{{if .DuesUnpaid}}<span style="color:#f59e0b">{{.DuesUnpaid}}</span>{{else}}0{{end}}</td>
        <td style="text-align:right">{{.TowersOnline}}/{{.Towers}}</td>
      </tr>
      {{end}}
    </table>

    <div style="display:flex;gap:24px;flex-wrap:wrap;font-size:12px;margin-top:8px">
      <div style="min-width:220px"><b>Sovereignty</b> ({{len .Sov}} systems, {{.TowersOnline}} of {{.Towers}} towers online)<br>
        {{if .Sov}}{{range .Sov}}{{.}}<br>{{end}}{{else}}<span style="color:#9ca3af">holds no system</span>{{end}}
        {{if .SovMotive}}<br><i>Founded to hold 0.0</i>{{end}}
        {{if .Targets}}<br><b>Targets</b><br>{{range .Targets}}{{.}}<br>{{end}}{{end}}
        {{if .Forward}}<br><b>Forward offices</b><br>{{range .Forward}}{{.}}<br>{{end}}{{end}}</div>
      <div style="min-width:260px"><b>Wars</b><br>
        {{if .Wars}}{{range .Wars}}{{.Side}} <a href="/rolebots/corp/{{.OtherID}}">{{.Other}}</a> since {{.Declared}}{{if .Hostile}} <span style="color:#dc2626">hot</span>{{else}} <span style="color:#9ca3af">arming</span>{{end}}<br>{{end}}{{else}}<span style="color:#9ca3af">none</span>{{end}}</div>
      <div style="min-width:220px"><b>Relationships</b><br>
        {{if .Rels}}{{range .Rels}}{{if .Mine}}→{{else}}←{{end}} {{.ToName}}: {{.Rel}}<br>{{end}}{{else}}<span style="color:#9ca3af">none declared</span>{{end}}</div>
      <div style="min-width:220px"><b>Applications</b>{{if .OpenRequests}} · {{.OpenRequests}} open requests{{end}}<br>
        {{if .Apps}}{{range .Apps}}<a href="/rolebots/corp/{{.CorpID}}">{{.Corp}}</a>{{if .Human}} <span style="color:#9ca3af">capsuleer</span>{{end}} — {{.State}}, {{.Applied}}<br>{{end}}{{else}}<span style="color:#9ca3af">none waiting</span>{{end}}</div>
    </div>

    <h4 style="margin:12px 0 4px 0;font-size:13px">What they are up to</h4>
    {{if .Feed}}
    <table class="rb-table">
      {{range .Feed}}<tr><td style="white-space:nowrap;color:#6b7280;width:120px">{{.When}}</td><td style="width:180px">{{.Actor}}</td><td>{{.Line}}</td></tr>{{end}}
    </table>
    {{else}}<p style="color:#9ca3af;font-size:12px">nothing yet</p>{{end}}
    </div>
  </details>
  {{end}}
  {{if .Dissolved}}
  <details style="margin-top:12px;font-size:12px">
    <summary style="cursor:pointer;color:#6b7280">Dissolved ({{len .Dissolved}})</summary>
    <p style="margin:6px 0 0 0">{{range $i, $d := .Dissolved}}{{if $i}} &middot; {{end}}<a href="/alliance/{{$d.ID}}">{{$d.Name}}</a> <span style="color:#9ca3af">[{{$d.Ticker}}]</span>{{end}}</p>
  </details>
  {{end}}
</div>
<script>
(function () {
  var q = document.getElementById('rbal-q');
  if (!q) return;
  var rows = Array.prototype.slice.call(document.querySelectorAll('details.rbal'));
  var shown = document.getElementById('rbal-shown');
  function apply() {
    var v = q.value.trim().toLowerCase(), n = 0;
    rows.forEach(function (d) {
      var hit = !v || (d.getAttribute('data-key') || '').toLowerCase().indexOf(v) >= 0;
      d.style.display = hit ? '' : 'none';
      if (hit) n++;
    });
    shown.textContent = v ? n + ' of ' + rows.length + ' shown' : '';
  }
  q.addEventListener('input', apply);
  document.getElementById('rbal-open').addEventListener('click', function () {
    rows.forEach(function (d) { if (d.style.display !== 'none') d.open = true; });
  });
  document.getElementById('rbal-shut').addEventListener('click', function () {
    rows.forEach(function (d) { d.open = false; });
  });
})();
</script>
{{end}}`
