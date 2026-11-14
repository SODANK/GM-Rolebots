package rolebots

// The corporation page's Relations card (/rolebots/corp/<id>): one corporation's standings with the ledger behind
// them. What it holds toward others (live value, written standing, tier, baseline, drift direction, pair owner),
// what others hold toward it, its wars with the CEO's book and mercenary paper, the incident ledger, letters and
// pacts, and the two operator writes: declare a standing (which yields the pair) and hand a yielded pair back to
// the ledger. Every counterparty links to its own page. Module pages parse with no funcMap, so every number is a
// preformatted string.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const relIncidentPageSize = 50

// ceoRelationsViewData is the card.
type ceoRelationsViewData struct {
	CorpID   int64
	Managed  bool
	Declared []relDeclRow
	Received []relRecvRow
	Wars     []relCorpWarRow
	WarBook  []relWarBookRow
	MercJobs []relMercJobRow
	MercCtrs []relMercContractRow
	Letters  []relLetterRow
	Pacts    []relPactRow
	Incs     []relIncRow
	IncKinds []string
	IncKind  string
	IncTo    string
	IncPage  int
	IncNext  int
	IncPrev  int
	IncMore  bool
	Self     string
	Ledger   bool
}

type relDeclRow struct {
	ToID            int64
	Name, Link      string
	Value, Written  string
	Tier, Baseline  string
	Trend           string
	Owner           string
	Floor           string
	Why             string
	Yielded         bool
	TierClass       string
	HasLedger       bool
	Human           bool // a standing nobody on the ledger holds: a human's or the operator's
	HandBackAllowed bool
}

type relRecvRow struct {
	FromID     int64
	Name, Link string
	Value      string
	Tier       string
	TierClass  string
	Bot        bool
}

type relCorpWarRow struct {
	Text    string
	State   string
	Hostile bool
}

type relLetterRow struct {
	ID        int64
	Dir       string // "to" / "from"
	Other     string
	OtherLink string
	Kind      string
	Status    string
	Envoy     string
	EnvoyLink string
	Offer     string
	When      string
	Reason    string
}

type relPactRow struct {
	Other, OtherLink string
	Kind, Floor      string
	Until            string
	Status           string
}

type relIncRow struct {
	When, Kind, Side string
	Other, OtherLink string
	Weight, ISK      string
	Where, Detail    string
	Hostile          bool
}

// ceoRelationsView builds the card for any player corporation; the ledger
// halves are present only for a managed one.
func (m *Mod) ceoRelationsView(ctx context.Context, corpID int64, facts *CorpFacts, managed bool, q url.Values) *ceoRelationsViewData {
	v := &ceoRelationsViewData{CorpID: corpID, Managed: managed, Self: fmt.Sprintf("/rolebots/corp/%d", corpID),
		Ledger: relLedgerOn(), IncKinds: relIncidentKinds()}
	now := clock.Unix()
	link := func(id int64) string { return fmt.Sprintf("/rolebots/corp/%d", id) }

	// Declared: the ledger's rows joined to the live standing; live rows the
	// ledger does not hold are a human's or the operator's.
	live, _ := m.ceoRelLive(ctx, corpID)
	rows := map[int64]relState{}
	if managed {
		rows, _ = m.relRowsOf(ctx, corpID)
	}
	seen := map[int64]bool{}
	for toID, s := range rows {
		seen[toID] = true
		lv, hasLive := live[toID]
		r := relDeclRow{ToID: toID, Name: corpNameOf(ctx, m.db, toID), Link: link(toID), HasLedger: true,
			Value: fmt.Sprintf("%+.1f", s.Value), Written: fmt.Sprintf("%+.1f", s.Written),
			Baseline: fmt.Sprintf("%+.1f", s.Baseline), Tier: standingTier(s.Written), Why: s.Reason, Yielded: s.Yielded}
		r.TierClass = "rb-t-" + r.Tier
		switch d := s.Value - s.PrevValue; {
		case d >= 0.2:
			r.Trend = "↑"
		case d <= -0.2:
			r.Trend = "↓"
		default:
			r.Trend = "→"
		}
		switch relOwnership(lv, hasLive, s, true) {
		case relOwnYielded:
			r.Owner, r.HandBackAllowed = "yielded to the directors", true
			if hasLive {
				r.Written = fmt.Sprintf("%+.1f", lv)
			}
		case relOwnYield:
			r.Owner = "edited since — yields at the next flush"
		default:
			r.Owner = "the ledger"
		}
		if s.FloorUntil > now {
			r.Floor = fmt.Sprintf("floor %+.1f until %s", s.FloorValue, ycDate(time.Unix(s.FloorUntil, 0)))
		}
		v.Declared = append(v.Declared, r)
	}
	for toID, lv := range live {
		if seen[toID] || lv == 0 {
			continue
		}
		v.Declared = append(v.Declared, relDeclRow{ToID: toID, Name: corpNameOf(ctx, m.db, toID), Link: link(toID),
			Written: fmt.Sprintf("%+.1f", lv), Value: "—", Baseline: "—", Trend: "", Tier: standingTier(lv),
			TierClass: "rb-t-" + standingTier(lv), Owner: "a director or the operator", Human: true})
	}
	sortRows := func(a, b relDeclRow) bool {
		wa, _ := strconv.ParseFloat(a.Written, 64)
		wb, _ := strconv.ParseFloat(b.Written, 64)
		if wa != wb {
			return wa < wb
		}
		return a.Name < b.Name
	}
	for i := 1; i < len(v.Declared); i++ {
		for j := i; j > 0 && sortRows(v.Declared[j], v.Declared[j-1]); j-- {
			v.Declared[j], v.Declared[j-1] = v.Declared[j-1], v.Declared[j]
		}
	}

	// Received.
	if theirs, err := m.ceoRelTheirView(ctx, corpID); err == nil {
		for fromID, val := range theirs {
			if val == 0 {
				continue
			}
			_, bot := managedCEO(fromID)
			v.Received = append(v.Received, relRecvRow{FromID: fromID, Name: corpNameOf(ctx, m.db, fromID), Link: link(fromID),
				Value: fmt.Sprintf("%+.1f", val), Tier: standingTier(val), TierClass: "rb-t-" + standingTier(val), Bot: bot})
		}
	}

	// Wars: this corporation's, through the module's own hostility test.
	if all, err := evedb.ListWars(ctx, m.db); err == nil {
		isMe := func(id int64) bool {
			return id == corpID || (facts != nil && facts.AllianceID != 0 && id == facts.AllianceID)
		}
		nowTicks := uint64(win32Now())
		for _, wv := range all {
			if !wv.Active {
				continue
			}
			var text string
			switch {
			case isMe(wv.DeclaredByID):
				text = "Declared on " + wv.AgainstName
			case isMe(wv.AgainstID):
				text = wv.DeclaredByName + " declared on it"
			default:
				continue
			}
			hostile := relWarHostile(wv.War, nowTicks)
			state := "arming"
			switch {
			case wv.Retracted.Valid && hostile:
				state = "cooling"
			case wv.Retracted.Valid:
				state = "retracted"
			case hostile:
				state = "hostile"
			case wv.TimeDeclared > int64(nowTicks):
				state = "future-dated"
			}
			v.Wars = append(v.Wars, relCorpWarRow{Text: text, State: state, Hostile: hostile})
		}
	}
	if managed {
		// The war-market rows carry owner NAMES (esprelations.go): this
		// corporation's, or its alliance's, is what to match.
		mine := map[string]bool{}
		if facts != nil {
			mine[facts.Name] = true
			if facts.AllianceName != "" {
				mine[facts.AllianceName] = true
			}
		}
		for _, b := range m.relWarBookRows(ctx) {
			if mine[b.Corp] || mine[b.Target] {
				v.WarBook = append(v.WarBook, b)
			}
		}
		for _, j := range m.relMercJobRows(ctx) {
			if mine[j.Client] || mine[j.Target] {
				v.MercJobs = append(v.MercJobs, j)
			}
		}
		for _, c := range m.relMercContractRows(ctx) {
			if mine[c.Client] || mine[c.Merc] || mine[c.Target] {
				v.MercCtrs = append(v.MercCtrs, c)
			}
		}
	}

	// Letters and pacts.
	if ls, err := m.lettersWhere(ctx, `(fromCorpID = ? OR toCorpID = ?) AND draftedAt >= ? ORDER BY letterID DESC`,
		corpID, corpID, now-relClosedRetentionSec); err == nil {
		for _, l := range ls {
			r := relLetterRow{ID: l.ID, Kind: letterKindWords(l.Kind), Status: l.Status, Reason: l.Reason,
				When: fmtDuration(now-l.DraftedAt) + " ago", Offer: letterOfferWords(l)}
			if l.FromCorpID == corpID {
				r.Dir, r.Other, r.OtherLink = "to", corpNameOf(ctx, m.db, l.ToCorpID), link(l.ToCorpID)
			} else {
				r.Dir, r.Other, r.OtherLink = "from", corpNameOf(ctx, m.db, l.FromCorpID), link(l.FromCorpID)
			}
			if l.EnvoyCharID != 0 {
				r.Envoy, r.EnvoyLink = m.charNameOr(ctx, l.EnvoyCharID), fmt.Sprintf("/rolebot/%d", l.EnvoyCharID)
			}
			v.Letters = append(v.Letters, r)
		}
	}
	if prow, err := m.db.QueryContext(ctx, `
		SELECT corpA, corpB, kind, floorValue, endAt, status FROM rolebot_corp_pacts
		 WHERE (corpA = ? OR corpB = ?) AND endAt >= ? ORDER BY endAt DESC`, corpID, corpID, now-relClosedRetentionSec); err == nil {
		for prow.Next() {
			var a, b, until int64
			var kind, status string
			var floor float64
			if prow.Scan(&a, &b, &kind, &floor, &until, &status) != nil {
				continue
			}
			other := a
			if other == corpID {
				other = b
			}
			if status == "standing" && until <= now {
				status = "expired"
			}
			v.Pacts = append(v.Pacts, relPactRow{Other: corpNameOf(ctx, m.db, other), OtherLink: link(other),
				Kind: letterKindWords(kind), Floor: fmt.Sprintf("%+.1f", floor), Until: ycDate(time.Unix(until, 0)), Status: status})
		}
		prow.Close()
	}

	// Incidents, paged and filtered.
	v.IncKind, v.IncTo = q.Get("rkind"), q.Get("rto")
	v.IncPage, _ = strconv.Atoi(q.Get("rp"))
	if v.IncPage < 0 {
		v.IncPage = 0
	}
	v.IncNext, v.IncPrev = v.IncPage+1, v.IncPage-1
	if managed {
		where := `corporationID = ?`
		args := []any{corpID}
		if v.IncKind != "" {
			where += ` AND kind = ?`
			args = append(args, v.IncKind)
		}
		if to, _ := strconv.ParseInt(v.IncTo, 10, 64); to != 0 {
			where += ` AND toID = ?`
			args = append(args, to)
		}
		args = append(args, relIncidentPageSize+1, v.IncPage*relIncidentPageSize)
		if irow, err := m.db.QueryContext(ctx, `
			SELECT toID, kind, side, weight, isk, systemID, at, detail FROM rolebot_corp_incidents
			 WHERE `+where+` ORDER BY at DESC, incidentID DESC LIMIT ? OFFSET ?`, args...); err == nil {
			for irow.Next() {
				var to, sys, at int64
				var kind, side, detail string
				var weight, isk float64
				if irow.Scan(&to, &kind, &side, &weight, &isk, &sys, &at, &detail) != nil {
					continue
				}
				if len(v.Incs) == relIncidentPageSize {
					v.IncMore = true
					break
				}
				r := relIncRow{When: fmtDuration(now-at) + " ago", Kind: kind, Side: side, Other: corpNameOf(ctx, m.db, to),
					OtherLink: link(to), Weight: fmt.Sprintf("%+.2f", weight), Detail: detail, Hostile: weight < 0}
				if isk > 0 {
					r.ISK = iskString(isk)
				}
				if sys != 0 {
					r.Where = m.nameOfSystem(ctx, sys)
				}
				v.Incs = append(v.Incs, r)
			}
			irow.Close()
		}
	}
	return v
}

// relIncidentKinds is the filter list.
func relIncidentKinds() []string {
	return []string{incKill, incGank, incPod, incAggress, incRobbery, incBounty, incSiege, incTowerKill, incWarDec,
		incWarRetract, incWarEnd, incCrowd, incContest, incEnvoyKill, incPact, incReparation, incPeace, incPactBreak}
}

// handleCorpRelationsPost is the card's two writes, dispatched from
// handleCEOPost: declare (operator's standing; yields a managed pair) and
// handback (a yielded pair returns to the ledger).
func (m *Mod) handleCorpRelationsPost(w http.ResponseWriter, r *http.Request, ctx context.Context, back string, corpID int64) {
	to, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("to")), 10, 64)
	if corpID == 0 || to == 0 || to == corpID {
		redirect(w, r, back, "", "a counterparty is required")
		return
	}
	name := corpNameOf(ctx, m.db, to)
	switch r.FormValue("action") {
	case "declare":
		v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("standing")), 64)
		if err != nil || math.IsNaN(v) {
			redirect(w, r, back, "", "that is not a standing")
			return
		}
		v = clampStanding(v)
		// setBy 0: no director made this. SetCorpStanding deletes on zero.
		if err := evedb.SetCorpStanding(ctx, m.db, corpID, to, v, 0, ""); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		if c, _ := managedCEO(corpID); c != 0 {
			if err := markRelationYielded(ctx, m.db, corpID, to, v, "set by the operator"); err != nil {
				redirect(w, r, back, "", err.Error())
				return
			}
			if ceo, _ := getCEO(ctx, m.db, corpID); ceo != nil {
				m.ceoLog(ctx, ceo, powerDiplomacy, "declare", to, v, "the operator set its standing toward %s to %+.1f", name, v)
			}
		}
		m.refreshRelations(ctx)
		if v == 0 {
			redirect(w, r, back, "neutral toward "+name+" again", "")
			return
		}
		redirect(w, r, back, fmt.Sprintf("now stands %+.1f (%s) toward %s", v, standingTier(v), name), "")
	case "handback":
		if _, managed := managedCEO(corpID); !managed {
			redirect(w, r, back, "", "no ledger runs this corporation")
			return
		}
		if err := relHandBack(ctx, m.db, corpID, to); err != nil {
			redirect(w, r, back, "", err.Error())
			return
		}
		if ceo, _ := getCEO(ctx, m.db, corpID); ceo != nil {
			m.ceoLog(ctx, ceo, powerDiplomacy, "handback", to, 0, "the operator handed its standing toward %s back to the ledger", name)
		}
		redirect(w, r, back, "the pair with "+name+" is the ledger's again — the next flush writes it", "")
	default:
		redirect(w, r, back, "", "unknown action")
	}
}

// tmplCEORelations is the card. Drawn for every player corporation; the
// ledger halves only when a bot CEO runs it.
const tmplCEORelations = `{{with .Relations}}
<div class="card">
  <h3>Relations</h3>
  {{if not .Ledger}}<p style="font-size:12px;color:#b45309">The relations ledger is switched off (Relations page): values are frozen and nothing is written.</p>{{end}}
  <h4 style="margin:6px 0 4px">What it holds toward others</h4>
  {{if .Declared}}
  <table class="tbl rb-sortable" style="font-size:12px">
    <tr><th>Corporation</th><th>Standing</th><th>Tier</th><th>Live</th><th>Baseline</th><th>Trend</th><th>Owner</th><th>Why</th><th></th></tr>
    {{range .Declared}}<tr>
      <td><a href="{{.Link}}">{{.Name}}</a></td>
      <td>{{.Written}}</td><td class="{{.TierClass}}">{{.Tier}}</td>
      <td>{{.Value}}</td><td>{{.Baseline}}</td><td>{{.Trend}}</td>
      <td style="color:#6b7280">{{.Owner}}{{if .Floor}} &middot; {{.Floor}}{{end}}</td>
      <td style="color:#6b7280">{{.Why}}</td>
      <td>{{if .HandBackAllowed}}<form method="POST" action="{{$.Relations.Self}}" style="display:inline">
        <input type="hidden" name="corpID" value="{{$.Relations.CorpID}}"><input type="hidden" name="action" value="handback">
        <input type="hidden" name="to" value="{{.ToID}}"><button type="submit">Hand back</button></form>{{end}}</td>
    </tr>{{end}}
  </table>
  {{else}}<p style="font-size:12px;color:#6b7280">Neutral toward every corporation.</p>{{end}}
  <form method="POST" action="{{.Self}}" style="margin:8px 0 0;font-size:12px">
    <input type="hidden" name="corpID" value="{{.CorpID}}"><input type="hidden" name="action" value="declare">
    Declare toward corporation <input type="number" name="to" placeholder="corp ID" required style="width:110px">
    standing <input type="number" name="standing" step="0.1" min="-10" max="10" value="-5" required style="width:70px">
    <button type="submit">Declare</button>
    <span style="color:#6b7280">0 clears it. {{if .Managed}}A pair you declare is yielded: the ledger never re-decides it until you hand it back.{{end}}</span>
  </form>

  <h4 style="margin:12px 0 4px">What others hold toward it</h4>
  {{if .Received}}
  <table class="tbl rb-sortable" style="font-size:12px">
    <tr><th>Corporation</th><th>Standing</th><th>Tier</th><th></th></tr>
    {{range .Received}}<tr><td><a href="{{.Link}}">{{.Name}}</a></td><td>{{.Value}}</td>
      <td class="{{.TierClass}}">{{.Tier}}</td><td>{{if .Bot}}<span class="rb-pill">bot-run</span>{{end}}</td></tr>{{end}}
  </table>
  {{else}}<p style="font-size:12px;color:#6b7280">Nobody has declared on it.</p>{{end}}

  {{if or .Wars .WarBook .MercJobs .MercCtrs}}
  <h4 style="margin:12px 0 4px">Wars</h4>
  <table class="tbl" style="font-size:12px">
    {{range .Wars}}<tr><td>{{.Text}}</td><td>{{if .Hostile}}<span class="rb-pill bad">{{.State}}</span>{{else}}<span class="rb-pill warn">{{.State}}</span>{{end}}</td></tr>{{end}}
  </table>
  {{if .WarBook}}<p style="font-size:12px;margin:6px 0 2px"><b>The CEO's war book</b></p>
  <table class="tbl" style="font-size:12px"><tr><th>War</th><th>Against</th><th>Motive</th><th>Bills paid</th><th>Reason</th></tr>
    {{range .WarBook}}<tr><td>{{.WarID}}</td><td>{{.Target}}</td><td>{{.Motive}}</td><td>{{.Bills}}</td><td style="color:#6b7280">{{.Note}}</td></tr>{{end}}
  </table>{{end}}
  {{if .MercJobs}}<p style="font-size:12px;margin:6px 0 2px"><b>Mercenary tenders</b></p>
  <table class="tbl" style="font-size:12px"><tr><th>Job</th><th>Client</th><th>Target</th><th>Kind</th><th>Status</th><th>Bids</th></tr>
    {{range .MercJobs}}<tr><td>{{.JobID}}</td><td>{{.Client}}</td><td>{{.Target}}</td><td>{{.Kind}}</td><td>{{.Status}}</td><td>{{.Quotes}}</td></tr>{{end}}
  </table>{{end}}
  {{if .MercCtrs}}<p style="font-size:12px;margin:6px 0 2px"><b>Mercenary contracts</b></p>
  <table class="tbl" style="font-size:12px"><tr><th>Contract</th><th>Client</th><th>Mercenary</th><th>Target</th><th>Status</th><th>ISK destroyed</th></tr>
    {{range .MercCtrs}}<tr><td>{{.ID}}</td><td>{{.Client}}</td><td>{{.Merc}}</td><td>{{.Target}}</td><td>{{.Status}}</td><td>{{.Destroyed}}</td></tr>{{end}}
  </table>{{end}}
  {{end}}

  {{if or .Letters .Pacts}}
  <h4 style="margin:12px 0 4px">Letters and pacts</h4>
  {{if .Letters}}<table class="tbl" style="font-size:12px">
    <tr><th>Letter</th><th>Proposing</th><th>Status</th><th>Envoy</th><th>When</th><th>Reason</th></tr>
    {{range .Letters}}<tr><td>{{.Dir}} <a href="{{.OtherLink}}">{{.Other}}</a></td><td>{{.Kind}}{{if .Offer}} <span style="color:#6b7280">{{.Offer}}</span>{{end}}</td>
      <td>{{.Status}}</td><td>{{if .Envoy}}<a href="{{.EnvoyLink}}">{{.Envoy}}</a>{{end}}</td><td>{{.When}}</td><td style="color:#6b7280">{{.Reason}}</td></tr>{{end}}
  </table>{{end}}
  {{if .Pacts}}<table class="tbl" style="font-size:12px;margin-top:6px">
    <tr><th>Pact</th><th>With</th><th>Floor</th><th>Until</th><th>Status</th></tr>
    {{range .Pacts}}<tr><td>{{.Kind}}</td><td><a href="{{.OtherLink}}">{{.Other}}</a></td><td>{{.Floor}}</td><td>{{.Until}}</td><td>{{.Status}}</td></tr>{{end}}
  </table>{{end}}
  {{end}}

  {{if .Managed}}
  <h4 style="margin:12px 0 4px">Incidents</h4>
  <form method="GET" action="{{.Self}}" style="font-size:12px;margin:0 0 6px">
    kind <select name="rkind"><option value="">any</option>{{range .IncKinds}}<option value="{{.}}"{{if eq . $.Relations.IncKind}} selected{{end}}>{{.}}</option>{{end}}</select>
    counterparty <input type="number" name="rto" value="{{.IncTo}}" placeholder="corp ID" style="width:110px">
    <button type="submit">Filter</button>
  </form>
  {{if .Incs}}
  <table class="tbl" style="font-size:12px">
    <tr><th>When</th><th>Kind</th><th>Side</th><th>Counterparty</th><th>Weight</th><th>ISK</th><th>Where</th><th>Detail</th></tr>
    {{range .Incs}}<tr><td>{{.When}}</td><td>{{.Kind}}</td><td>{{.Side}}</td><td><a href="{{.OtherLink}}">{{.Other}}</a></td>
      <td{{if .Hostile}} style="color:#b91c1c"{{else}} style="color:#15803d"{{end}}>{{.Weight}}</td><td>{{.ISK}}</td><td>{{.Where}}</td><td style="color:#6b7280">{{.Detail}}</td></tr>{{end}}
  </table>
  <p style="font-size:12px">{{if gt .IncPage 0}}<a href="{{.Self}}?rp={{.IncPrev}}&rkind={{.IncKind}}&rto={{.IncTo}}">&larr; newer</a> {{end}}{{if .IncMore}}<a href="{{.Self}}?rp={{.IncNext}}&rkind={{.IncKind}}&rto={{.IncTo}}">older &rarr;</a>{{end}}</p>
  {{else}}<p style="font-size:12px;color:#6b7280">Nothing on the ledger{{if or .IncKind .IncTo}} for that filter{{end}}.</p>{{end}}
  {{end}}
  <p style="font-size:12px;color:#6b7280">The <b>standing</b> is what the game holds (crpStandings) and what every pilot reads; the <b>live</b> value is the
  ledger's own, moved by incidents and decaying toward the <b>baseline</b> the CEO's temperament sets. Tiers: ally &ge; 7.5, friend &ge; 2.5,
  rival &le; &minus;2.5, enemy &le; &minus;7.5. Switches and dials are on the <a href="/rolebots/corps/diplomacy">Diplomacy page</a>.</p>
</div>
{{end}}`
