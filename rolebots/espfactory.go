package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	serverram "gurimoon/server/ram"
)

// The factory page: /rolebot/<charID>/factory.
//
// The left is the build book: the four evidence tiers priced for this pilot at this factory now
// (buildbook.go), with the rival count that discounted each. It runs on demand behind a button
// because reading it is a real market sweep. The right is the shelf: the standing stock lines this
// factory holds. Anything built here is on it; anything an operator types is pinned and exempt from
// the pruner, as rolebot_skill_prefs exempts a forced skill from the planner. A target of 0 removes
// the line.

// bookRow is one line of the build-book preview.
type bookRow struct {
	TypeID   int64
	TypeName string
	Wanted   int64
	Evidence string
	Why      string
	Rivals   int
	Building int
	Own      int
	Score    string
}

// cookingView is one job on a line, dressed for the page.
type cookingView struct {
	TypeName string
	Runs     int
	Station  string
	Ready    string
	// Spent is the cash the project sank before install: blueprint, bought materials and the install
	// fee (IndSpentISK at install time).
	Spent string
}

// shelfView is one shelf line dressed for the page.
type shelfView struct {
	shelfRow
	Station   string
	LastBuilt string
	Pinned    bool
}

func (m *Mod) handleFactoryPage(w http.ResponseWriter, r *http.Request, charID int64) {
	// A book preview sweeps the market and walks the fit library; the
	// profile page's 15 seconds is not enough for it.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleFactoryPost(w, r, ctx, charID)
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
	traits, err := getTraits(ctx, m.db, charID)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	tn := resolveTuning(traits)

	base := prof.IndustryStationID
	baseLabel := "not settled yet — it picks one the first time it manufactures"
	if base != 0 {
		baseLabel = m.stationLabel(ctx, base)
	}

	// Lines: the ceiling its Mass Production skills buy, and what is on them.
	lines, free, cooking := 0, 0, []cookingView(nil)
	if b, lerr := m.loadRun(ctx, charID); lerr == nil {
		if n, ferr := m.industryFreeSlots(ctx, b); ferr == nil {
			free = n
		}
		mp, _ := m.charSkillLevelFor(ctx, charID, serverram.SkillMassProduction)
		amp, _ := m.charSkillLevelFor(ctx, charID, serverram.SkillAdvancedMassProduction)
		lines = serverram.MaxConcurrentJobs(mp, amp)
		for _, j := range b.State.Data.IndCooking {
			v := cookingView{TypeName: j.TypeName, Runs: j.Runs,
				Station: m.stationLabel(ctx, j.FactoryID), Ready: "—",
				Spent: iskString(j.CashSpent)}
			if j.Delivered {
				// Off the line already (industryOvenSweep): the goods are in the hangar waiting to be listed.
				v.Ready = "in its hangar, waiting to be listed"
				cooking = append(cooking, v)
				continue
			}
			if _, ready, endTime, jerr := m.industryJobStatus(ctx, j.JobID); jerr == nil {
				if ready {
					v.Ready = "ready to collect"
				} else if left := evedb.Win32ToUnix(endTime) - clock.Unix(); left > 0 {
					v.Ready = "in " + agoString(left)
				}
			} else {
				v.Ready = "no longer on any line"
			}
			cooking = append(cooking, v)
		}
	}
	// The pilot's clone anchor: for an industrialist the factory or the nearest bay to it.
	anchorLabel := "wherever it happens to be when it needs one"
	if b, lerr := m.loadRun(ctx, charID); lerr == nil {
		if st, _, ok := m.industryAnchor(ctx, b); ok {
			anchorLabel = m.stationLabel(ctx, st)
			if st == base {
				anchorLabel += " (its own factory)"
			}
		}
	}

	shelf, err := m.shelfFor(ctx, charID, 0)
	if err != nil {
		espError(w, "rolebots", err.Error())
		return
	}
	views := make([]shelfView, 0, len(shelf))
	for _, s := range shelf {
		v := shelfView{shelfRow: s, Station: m.stationLabel(ctx, s.StationID),
			LastBuilt: "never", Pinned: s.Source == "operator"}
		if s.LastBuiltAt > 0 {
			v.LastBuilt = time.Unix(s.LastBuiltAt, 0).UTC().Format("2006-01-02 15:04 UTC")
		}
		views = append(views, v)
	}

	// The book, only when asked for.
	var book []bookRow
	bookErr := ""
	if r.URL.Query().Get("book") == "1" {
		rows, err := m.previewBuildBook(ctx, charID, 25)
		if err != nil {
			bookErr = err.Error()
		} else {
			book = rows
		}
	}

	espRender(w, "rolebots_factory.html", map[string]any{
		"Page":        "rolebots",
		"Bot":         rb,
		"Profile":     prof,
		"Status":      status,
		"Wallet":      iskString(status.Balance),
		"Base":        base,
		"BaseLabel":   baseLabel,
		"AnchorLabel": anchorLabel,
		"Lines":       lines,
		"FreeLines":   free,
		"Cooking":     cooking,
		"Shelf":       views,
		"Book":        book,
		"BookErr":     bookErr,
		"Asked":       r.URL.Query().Get("book") == "1",
		// Everybody builds (SupplyDrive floors at 0.15), so supplier compares against the base drive.
		"Supplier": tn.SupplyDrive > supplyDriveBase,
		"Margin":   fmt.Sprintf("%.2f×", tn.IndustryMargin),
		"Compete":  industryCompeteJumps,
		"MaxRows":  industryShelfMaxRows,
		"Msg":      r.URL.Query().Get("msg"),
		"Err":      r.URL.Query().Get("err"),
	})
}

// previewBuildBook runs the real book for one pilot from where it stands, on the botRun the sim
// would build, since the book is priced by the pilot's own travel rules and traits. It resolves the
// factory as a project does, so an unsettled pilot settles here (industryFactoryStation records it,
// rememberIndustryBase): a write behind a page view, so the preview never picks another factory.
func (m *Mod) previewBuildBook(ctx context.Context, charID int64, limit int) ([]bookRow, error) {
	b, err := m.loadRun(ctx, charID)
	if err != nil {
		return nil, err
	}
	factoryID, factorySys, err := m.industryFactoryStation(ctx, b)
	if err != nil {
		return nil, fmt.Errorf("no usable factory: %w", err)
	}
	cands, err := m.buildBook(ctx, b, factoryID, factorySys, limit)
	if err != nil {
		return nil, err
	}
	out := make([]bookRow, 0, len(cands))
	for _, c := range cands {
		out = append(out, bookRow{
			TypeID: c.TypeID, TypeName: c.TypeName, Wanted: c.Wanted,
			Evidence: c.Evidence, Why: evidenceLabel(c.Evidence),
			Rivals: c.Rivals, Building: c.Building, Own: c.Own,
			Score: fmt.Sprintf("%.2f", c.Score),
		})
	}
	return out, nil
}

func (m *Mod) handleFactoryPost(w http.ResponseWriter, r *http.Request, ctx context.Context, charID int64) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	self := fmt.Sprintf("/rolebot/%d/factory", charID)
	switch r.FormValue("action") {
	case "target":
		stationID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("stationID")), 10, 64)
		typeID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("typeID")), 10, 64)
		target, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("target")), 10, 64)
		if stationID == 0 || typeID == 0 {
			redirect(w, r, self, "", "pick a station and a type first")
			return
		}
		// Only something with a blueprint the pilot could buy (bpBuyableJoin). A stock line for a type
		// nothing can make is a promise the pilot cannot keep and would sit at the top of its shelf.
		var name string
		if err := m.db.QueryRowContext(ctx, `
			SELECT t.typeName FROM invTypes t
			JOIN invBlueprintTypes bt ON bt.productTypeID = t.typeID
			`+bpBuyableJoin+`
			WHERE t.typeID = ?`, typeID).Scan(&name); err != nil {
			redirect(w, r, self, "", fmt.Sprintf(
				"type %d has no blueprint anyone can buy — nothing can build it", typeID))
			return
		}
		if err := m.setShelfTarget(ctx, charID, stationID, typeID, name, target); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		if target <= 0 {
			redirect(w, r, self, "stock line removed", "")
			return
		}
		redirect(w, r, self, fmt.Sprintf("%s pinned at %d units", name, target), "")
	default:
		http.Redirect(w, r, self, http.StatusSeeOther)
	}
}

// tmplFactory is the factory page.
const tmplFactory = `{{define "content"}}
<h2>Factory &amp; Shelf — {{.Status.CharacterName}}</h2>
{{if .Msg}}<div class="alert alert-ok">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
<p><a href="/rolebot/{{.Bot.CharacterID}}">&larr; Back to this pilot's profile</a></p>

{{if not .Supplier}}
<div class="alert alert-error">
  This pilot's <b>supply</b> trait is zero or below, so it never builds. Raise
  supply on its profile to make it a producer.
</div>
{{end}}

<div class="card">
  <h3>Base</h3>
  <p><b>{{.BaseLabel}}</b></p>
  <p class="muted">
    Set by the pilot the first time it manufactures. A pin on the profile wins.
    Sale margin {{.Margin}}. Rival listings within {{.Compete}} jumps count as competition.
  </p>
  <p>Wakes up at: <b>{{.AnchorLabel}}</b></p>
</div>

<div class="card">
  <h3>Lines</h3>
  <p><b>{{.Lines}}</b> concurrent manufacturing line(s) — its Mass Production
     skills — <b>{{.FreeLines}}</b> free right now.</p>
  {{if .Cooking}}
  <table class="grid">
    <tr><th>Product</th><th>Runs</th><th>Station</th><th>Cash sunk</th><th>Ready</th></tr>
    {{range .Cooking}}
    <tr><td>{{.TypeName}}</td><td>{{.Runs}}</td><td>{{.Station}}</td><td>{{.Spent}}</td><td>{{.Ready}}</td></tr>
    {{end}}
  </table>
  {{else}}
  <p class="muted">Nothing on a line.</p>
  {{end}}
</div>

<div class="card">
  <h3>Build book</h3>
  <p class="muted">
    What the pilot may build next, strongest evidence first: <b>ledger</b> (unmet buy),
    <b>shelf</b> (stock ran down), <b>bid</b> (open bid in reach), <b>gap</b> (nothing nearby sells it).
    The pilot draws from this list, so order is not the decision. Reading it is slow, so it runs on request.
  </p>
  {{if not .Asked}}
  <p><a class="btn" href="/rolebot/{{.Bot.CharacterID}}/factory?book=1">Read the book now</a></p>
  {{end}}
  {{if .BookErr}}<div class="alert alert-error">{{.BookErr}}</div>{{end}}
  {{if .Book}}
  <table class="grid">
    <tr><th>Product</th><th>Evidence</th><th>Why</th><th>Wanted</th><th>Rivals near</th><th>Others building</th><th>On its own lines</th><th>Score</th></tr>
    {{range .Book}}
    <tr>
      <td>{{.TypeName}} <span class="muted">#{{.TypeID}}</span></td>
      <td><b>{{.Evidence}}</b></td>
      <td class="muted">{{.Why}}</td>
      <td>{{.Wanted}}</td>
      <td>{{.Rivals}}</td>
      <td>{{.Building}}</td>
      <td>{{.Own}}</td>
      <td>{{.Score}}</td>
    </tr>
    {{end}}
  </table>
  {{else if .Asked}}{{if not .BookErr}}
  <p class="muted">Nothing in the book.</p>
  {{end}}{{end}}
</div>

<div class="card">
  <h3>Shelf</h3>
  <p class="muted">
    Standing stock lines. Auto lines are dropped after a month without a sale or rebuild, up to {{.MaxRows}} per station.
    Lines you set are <b>pinned</b> and kept.
  </p>
  {{if .Shelf}}
  <table class="grid">
    <tr><th>Product</th><th>Station</th><th>Target</th><th>On hand</th><th>Listed</th>
        <th>Short</th><th>Built</th><th>Sold</th><th>Last built</th><th></th></tr>
    {{range .Shelf}}
    <tr>
      <td>{{.TypeName}} <span class="muted">#{{.TypeID}}</span>{{if .Pinned}} <b>(pinned)</b>{{end}}</td>
      <td>{{.Station}}</td>
      <td>
        <form method="POST" style="display:inline">
          <input type="hidden" name="action" value="target">
          <input type="hidden" name="stationID" value="{{.StationID}}">
          <input type="hidden" name="typeID" value="{{.TypeID}}">
          <input type="number" name="target" value="{{.TargetQty}}" min="0" style="width:6em">
          <button type="submit">Set</button>
        </form>
      </td>
      <td>{{.OnHand}}</td>
      <td>{{.Listed}}</td>
      <td>{{.Short}}</td>
      <td>{{.BuiltQty}}</td>
      <td>{{.SoldQty}}</td>
      <td class="muted">{{.LastBuilt}}</td>
      <td>
        <form method="POST" style="display:inline">
          <input type="hidden" name="action" value="target">
          <input type="hidden" name="stationID" value="{{.StationID}}">
          <input type="hidden" name="typeID" value="{{.TypeID}}">
          <input type="hidden" name="target" value="0">
          <button type="submit">Remove</button>
        </form>
      </td>
    </tr>
    {{end}}
  </table>
  {{else}}
  <p class="muted">Nothing on the shelf yet.</p>
  {{end}}

  <h4>Pin a stock line</h4>
  <form method="POST">
    <input type="hidden" name="action" value="target">
    <label>Station ID <input type="number" name="stationID" value="{{.Base}}" style="width:10em"></label>
    <label>Type ID <input type="number" name="typeID" style="width:8em"></label>
    <label>Target units <input type="number" name="target" min="0" value="10" style="width:6em"></label>
    <button type="submit">Pin</button>
  </form>
  <p class="muted">The type must have a blueprint.</p>
</div>
{{end}}`
