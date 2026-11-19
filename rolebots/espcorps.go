package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// The Corps section's shared machinery, used by four pages:
//
//	/rolebots/corps        the player-corporation dashboard (espcorps_dash.go)
//	/rolebots/ceos         one card per bot CEO (espceo.go)
//	/rolebots/corps/npc    the NPC catalogue and recruitment (espcorps_npc.go)
//	/rolebots/corp/<id>    one corporation: espcorp_page.go (player), espcorps_npccorp.go (NPC)
//
// Shared here: the row type both tables use, the sorting, the POST handler behind the door
// switches and recruitment controls, and the two operator tools (kick-all, recruitment stats).

// corpListRow is one line of either catalogue: a CorpFacts and the module's policy beside it.
// Only the columns drawn differ (corpSortCols against playerCorpSortCols).
type corpListRow struct {
	CorpFacts
	Eligible int
	TagText  string
	// Pending is how many rolebot applications this corporation's directors
	// are sitting on. Always 0 for an NPC corporation: nobody applies to one,
	// the module simply joins it.
	Pending int

	// The dashboard's own columns (espcorps_dash.go). All zero on the NPC
	// catalogue, which draws none of them.
	WalletISK, NetISK   float64
	WalletText, NetText string
	NetNeg              bool
	Wars                int
	// BotCEO is set when the chair is a rostered pilot; Managed when that pilot
	// is actually running the corporation (a rolebot_ceos row), Paused when
	// the row says it decides nothing.
	BotCEO, Managed, Paused bool
}

// LimitText is "members / limit" for a player corporation, and just the count
// when the corporation set no limit. The pair is the whole answer to "why is
// this one refusing everybody", so it is one cell rather than two.
func (r corpListRow) LimitText() string {
	if r.MemberLimit <= 0 {
		return fmt.Sprintf("%d", r.Members)
	}
	return fmt.Sprintf("%d / %d", r.Members, r.MemberLimit)
}

// Full reports a player corporation with no openings left; its row is drawn dimmed.
func (r corpListRow) Full() bool {
	return r.Player && r.MemberLimit > 0 && r.Members >= r.MemberLimit
}

// ── sorting the catalogue ───────────────────────────────────────────────────
//
// Server-side and deep-linkable. Each row is one record, not parallel arrays (the trap the
// station browser documents). Every drawn column is sortable except the button; count columns
// lead with the biggest and name columns lead A to Z.

type corpSortCol struct {
	Key       string
	Label     string
	DescFirst bool
	Less      func(a, b corpListRow) bool
}

// corpSortCols is the header row; its order is the display order, so a new column is one entry.
// The bloc rides in the faction cell as a suffix and stays a filter of its own, and the door
// is drawn as the button that changes it.
var corpSortCols = []corpSortCol{
	{Key: "name", Label: "Corporation",
		Less: func(a, b corpListRow) bool { return lessFold(a.Name, b.Name) }},
	{Key: "faction", Label: "Faction",
		Less: func(a, b corpListRow) bool { return lessFold(a.FactionName, b.FactionName) }},
	{Key: "activity", Label: "Activity",
		Less: func(a, b corpListRow) bool { return lessFold(a.ActivityName(), b.ActivityName()) }},
	{Key: "tags", Label: "Tags",
		Less: func(a, b corpListRow) bool { return lessFold(a.TagText, b.TagText) }},
	{Key: "where", Label: "Where",
		Less: func(a, b corpListRow) bool {
			if a.RegionName != b.RegionName {
				return lessFold(a.RegionName, b.RegionName)
			}
			return lessFold(a.SystemName, b.SystemName)
		}},
	{Key: "stations", Label: "St", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.Stations < b.Stations }},
	{Key: "bots", Label: "Bots", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.Bots < b.Bots }},
	{Key: "eligible", Label: "Pass", DescFirst: true,
		Less: func(a, b corpListRow) bool { return a.Eligible < b.Eligible }},
	{Key: "open", Label: "Door", DescFirst: true,
		Less: func(a, b corpListRow) bool { return !a.Policy.Joinable && b.Policy.Joinable }},
}

// lessFold compares case-insensitively and sorts an empty value last whichever way the column
// points, since a corporation with no faction is missing data.
func lessFold(a, b string) bool {
	switch {
	case a == "" && b == "":
		return false
	case a == "":
		return false
	case b == "":
		return true
	}
	return strings.ToLower(a) < strings.ToLower(b)
}

// corpSortHead is one rendered header cell: what it says, where clicking it
// goes, and which way it is currently pointing.
type corpSortHead struct {
	Label string
	Href  string
	Arrow string
	On    bool
}

// corpSortSpec is one table's sorting: which columns it has and which query
// parameters carry its order, and which page its header links point back at
// (the dashboard and the NPC tab are separate pages with separate columns).
type corpSortSpec struct {
	Cols     []corpSortCol
	KeyParam string
	DirParam string
	Path     string
}

var (
	npcSortSpec  = corpSortSpec{Cols: corpSortCols, KeyParam: "sort", DirParam: "dir", Path: "/rolebots/corps/npc"}
	dashSortSpec = corpSortSpec{Cols: dashCorpSortCols, KeyParam: "sort", DirParam: "dir", Path: "/rolebots/corps"}
)

// sortCorpRows orders one catalogue and builds its header row. An unknown or absent key keeps
// the query's order (faction, then name).
func sortCorpRows(spec corpSortSpec, rows []corpListRow, key, dir string, q url.Values) (string, string, []corpSortHead) {
	desc := dir == "desc"
	var active *corpSortCol
	for i := range spec.Cols {
		if spec.Cols[i].Key == key {
			active = &spec.Cols[i]
			break
		}
	}
	if active == nil {
		key, desc = "", false
	} else {
		// Name breaks every tie, so a sort over few distinct values still reads in a fixed order.
		sort.SliceStable(rows, func(i, j int) bool { return lessFold(rows[i].Name, rows[j].Name) })
		sort.SliceStable(rows, func(i, j int) bool {
			if desc {
				return active.Less(rows[j], rows[i])
			}
			return active.Less(rows[i], rows[j])
		})
	}

	heads := make([]corpSortHead, 0, len(spec.Cols))
	for _, c := range spec.Cols {
		next := url.Values{}
		for k, v := range q {
			// A sort click is not a re-run of the last action, so msg and err are dropped. Every other
			// parameter, the filters included, is carried so a narrowed list stays narrowed.
			if k == "msg" || k == "err" || k == spec.KeyParam || k == spec.DirParam {
				continue
			}
			next[k] = v
		}
		h := corpSortHead{Label: c.Label, On: c.Key == key}
		wantDesc := c.DescFirst
		if h.On {
			wantDesc = !desc
			if desc {
				h.Arrow = " ▾" // ▾ pointing down: biggest first
			} else {
				h.Arrow = " ▴"
			}
		}
		next.Set(spec.KeyParam, c.Key)
		if wantDesc {
			next.Set(spec.DirParam, "desc")
		}
		h.Href = spec.Path + "?" + next.Encode()
		heads = append(heads, h)
	}
	d := ""
	if desc {
		d = "desc"
	}
	return key, d, heads
}

// activityOption is one entry of the activity filter.
type activityOption struct {
	ID    int64
	Label string
}

func activityOptions(ids map[int64]bool) []activityOption {
	var out []activityOption
	for id := range ids {
		out = append(out, activityOption{ID: id, Label: corpActivityName(id)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// handleCorpsPost carries the catalogue's own actions: the per-row switch and the bulk ones.
// Bulk open refuses to touch a closed-by-default corporation and says how many it left alone.
func (m *Mod) handleCorpsPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		espError(w, "rolebot-corps", err.Error())
		return
	}
	self := "/rolebots/corps"
	if strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/npc") {
		self = "/rolebots/corps/npc"
	}
	// back is the page's own query string. The banner a previous redirect left in it is dropped,
	// or a second action would announce the first one again.
	if q, err := url.ParseQuery(r.FormValue("back")); err == nil {
		q.Del("msg")
		q.Del("err")
		if enc := q.Encode(); enc != "" {
			self += "?" + enc
		}
	}

	switch r.FormValue("action") {
	case "found":
		on := r.FormValue("state") == "on"
		val := "0"
		if on {
			val = "1"
		}
		if err := evedb.SetServerSetting(ctx, m.db, settingCorpFound, val); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		corpFoundEnabled.Store(on)
		if on {
			redirect(w, r, self, fmt.Sprintf("self-founding ON — leaders may found up to %d corporation(s)", corpFoundMax()), "")
		} else {
			redirect(w, r, self, "self-founding OFF — no bot founds a corporation", "")
		}

	case "foundmax":
		n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max")))
		if err != nil || n < 0 || n > maxCorpFoundMax {
			redirect(w, r, self, "", fmt.Sprintf("the cap must be a number from 0 to %d", maxCorpFoundMax))
			return
		}
		if err := evedb.SetServerSetting(ctx, m.db, settingCorpFoundMax, strconv.Itoa(n)); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		corpFoundMaxV.Store(int64(n))
		redirect(w, r, self, fmt.Sprintf("bots may run at most %d self-founded corporation(s) at a time", n), "")

	case "foundnow":
		// The probe's test button: found for one pilot (or the readiest when none is named), past the
		// switch, the roll and the pace. The cap and every refusal still hold; the sector balance holds only
		// for the readiest (a named pilot is the operator's own pick).
		charID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("charID")), 10, 64)
		var corpID int64
		var who string
		var err error
		if charID > 0 {
			who = fmt.Sprintf("pilot %d", charID)
			corpID, err = m.foundNow(ctx, charID, "panel", true, false)
		} else {
			corpID, who, err = m.foundAnyNow(ctx, "panel")
		}
		if err != nil {
			redirect(w, r, self, "", fmt.Sprintf("%s did not found a corporation: %v", who, err))
			return
		}
		redirect(w, r, self, fmt.Sprintf("%s founded %s (corporation %d)", who, corpNameOf(ctx, m.db, corpID), corpID), "")

	case "deletefounded":
		if strings.TrimSpace(r.FormValue("confirm")) != "DELETE" {
			redirect(w, r, self, "", "type DELETE to close every rolebot-founded corporation")
			return
		}
		n, failed, err := m.deleteBotFoundedCorps(ctx)
		if err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		msg := fmt.Sprintf("closed %d rolebot-founded corporation(s)", n)
		if len(failed) > 0 {
			// redirect carries one banner, so the refusals ride the message.
			msg += fmt.Sprintf(" — %d refused: %s", len(failed), strings.Join(failed, "; "))
		}
		redirect(w, r, self, msg, "")

	case "withdraw":
		// The operator taking one bot's application back off a director's
		// list rather than waiting a week for it to expire.
		charID, _ := strconv.ParseInt(r.FormValue("charID"), 10, 64)
		corpID, _ := strconv.ParseInt(r.FormValue("corpID"), 10, 64)
		if charID == 0 || corpID == 0 {
			redirect(w, r, self, "", "no application named")
			return
		}
		if err := m.withdrawApplicationByID(ctx, charID, corpID); err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, "application withdrawn", "")

	case "toggle":
		// Either catalogue: the switch is one column of one policy row, and
		// the row does not care who founded the corporation.
		msg, err := m.setCorpDoor(ctx, parseCorpID(r.FormValue("corpID")), r.FormValue("state") == "on")
		if err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		redirect(w, r, self, msg, "")

	case "bulk":
		corps, err := listCorpFacts(ctx, m.db)
		if err != nil {
			redirect(w, r, self, "", err.Error())
			return
		}
		mode := r.FormValue("mode")
		scopeFaction := r.FormValue("scopeFaction")
		scopeTag := r.FormValue("scopeTag")
		changed, skipped := 0, 0
		for _, c := range corps {
			if scopeFaction != "" && c.FactionName != scopeFaction {
				continue
			}
			if scopeTag != "" && !c.Policy.HasTag(scopeTag) {
				continue
			}
			switch mode {
			case "open":
				if c.Policy.Restricted {
					skipped++
					continue
				}
				if c.Policy.Joinable {
					continue
				}
				c.Policy.Joinable = true
			case "shut":
				if !c.Policy.Joinable {
					continue
				}
				c.Policy.Joinable = false
			case "retag":
				// Back to the derived defaults: tags and the restricted flag only. The switch and the
				// operator's own numbers stay, since re-deriving tags must not reopen a shut door.
				def := defaultCorpPolicy(c)
				c.Policy.Tags = def.Tags
				c.Policy.Restricted = def.Restricted
			default:
				redirect(w, r, self, "", "unknown bulk action")
				return
			}
			if err := saveCorpPolicy(ctx, m.db, &c.Policy); err != nil {
				redirect(w, r, self, "", err.Error())
				return
			}
			changed++
		}
		msg := fmt.Sprintf("%d corporation(s) updated", changed)
		if skipped > 0 {
			msg += fmt.Sprintf(" — %d closed-by-default one(s) left shut; open those one at a time", skipped)
		}
		redirect(w, r, self, msg, "")

	default:
		http.Redirect(w, r, self, http.StatusSeeOther)
	}
}

// corpStats is the recruitment readout: how much of the roster has looked at
// the job market, how many have moved, and who moved most recently.
type corpStats struct {
	Reviewed  int
	Moved     int
	Moves     int
	DueSoon   int
	Recent    []corpMoveRow
	NextAt    int64
	HasNextAt bool
}

// corpMoveRow is one employment change, newest first.
type corpMoveRow struct {
	Name     string
	CharID   int64
	FromName string
	ToName   string
	When     string
}

func (m *Mod) corpRecruitmentStats(ctx context.Context) (corpStats, error) {
	var s corpStats
	now := clock.Unix()
	rows, err := m.db.QueryContext(ctx, `
		SELECT v.characterID, c.characterName, v.lastReviewAt, v.nextReviewAt,
		       v.lastMoveAt, v.fromCorpID, v.moves,
		       COALESCE(fc.corporationName, ''), COALESCE(tc.corporationName, '')
		FROM rolebot_corp_reviews v
		JOIN character_ c ON c.characterID = v.characterID
		LEFT JOIN corporation fc ON fc.corporationID = v.fromCorpID
		LEFT JOIN corporation tc ON tc.corporationID = c.corporationID
		ORDER BY v.lastMoveAt DESC`)
	if err != nil {
		return s, fmt.Errorf("corpRecruitmentStats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r corpMoveRow
		var lastReviewAt, nextReviewAt, lastMoveAt, fromCorpID int64
		var moves int
		if err := rows.Scan(&r.CharID, &r.Name, &lastReviewAt, &nextReviewAt,
			&lastMoveAt, &fromCorpID, &moves, &r.FromName, &r.ToName); err != nil {
			return s, err
		}
		if lastReviewAt > 0 {
			s.Reviewed++
		}
		if moves > 0 {
			s.Moved++
			s.Moves += moves
		}
		if nextReviewAt > 0 && nextReviewAt <= now+24*3600 {
			s.DueSoon++
		}
		if nextReviewAt > 0 && (!s.HasNextAt || nextReviewAt < s.NextAt) {
			s.NextAt, s.HasNextAt = nextReviewAt, true
		}
		if lastMoveAt > 0 && len(s.Recent) < 10 {
			r.When = time.Unix(lastMoveAt, 0).UTC().Format("2006-01-02 15:04 UTC")
			s.Recent = append(s.Recent, r)
		}
	}
	return s, rows.Err()
}

// handleCorpPage is /rolebots/corp/<id> (and its /rolebots/ceo/<id> alias). One address for every
// corporation: a player corporation gets the CEO-style page, an NPC one the policy editor.
func (m *Mod) handleCorpPage(w http.ResponseWriter, r *http.Request, corpID int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	c, err := getAnyCorpFacts(ctx, m.db, corpID)
	if err != nil {
		if r.Method == http.MethodPost {
			redirect(w, r, fmt.Sprintf("/rolebots/corp/%d", corpID), "", err.Error())
			return
		}
		espError(w, "rolebot-corps", err.Error())
		return
	}
	switch {
	case c.Player && r.Method == http.MethodPost:
		m.handleCEOPost(w, r, ctx, fmt.Sprintf("/rolebots/corp/%d", corpID))
	case c.Player:
		m.playerCorpPage(w, r, ctx, c)
	case r.Method == http.MethodPost:
		m.npcCorpPost(w, r, ctx, c)
	default:
		m.npcCorpPage(w, r, ctx, c)
	}
}

// kickBotsToSchool empties one corporation of rolebots, on either half of the catalogue and
// with recruitment off; the pilots sit at school until it is on. A pilot whose move fails is
// logged and skipped, so one bad row cannot leave the loop half done with no count.
func (m *Mod) kickBotsToSchool(ctx context.Context, corpID int64) (int, error) {
	// Sansha's Nation does not let go, and that includes the operator's kick
	// (corpIsSansha, corpjoin.go).
	if corpIsSansha(ctx, m.db, corpID) {
		return 0, fmt.Errorf("Sansha's Nation does not let go of its pilots — nobody is kicked out of it")
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.characterID FROM rolebot_characters r
		JOIN character_ c ON c.characterID = r.characterID
		WHERE c.corporationID = ?`, corpID)
	if err != nil {
		return 0, fmt.Errorf("kickBotsToSchool: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("kickBotsToSchool: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("kickBotsToSchool: %w", err)
	}

	moved := 0
	ceoID, managed := managedCEO(corpID)
	for _, id := range ids {
		// A bot CEO is not kicked out from under its own corporation, which would empty the chair.
		// Deactivate the pilot first.
		if managed && id == ceoID {
			continue
		}
		rev, err := getCorpReview(ctx, m.db, id)
		if err != nil {
			m.logBot(id, logError, "corp", "reading its review row to send it home: %v", err)
			continue
		}
		ok, err := m.sendPilotToSchool(ctx, id, corpID, rev)
		if err != nil {
			m.logBot(id, logError, "corp", "sending it back to its school corporation: %v", err)
			continue
		}
		if err := saveCorpReview(ctx, m.db, rev); err != nil {
			m.logBot(id, logError, "corp", "saving its review row: %v", err)
			continue
		}
		if ok {
			moved++
			m.logBot(id, logInfo, "corp",
				"was let go by the operator and returned to its school corporation")
		}
	}
	if len(ids) > 0 {
		// Presence is corp-keyed and reconciles the whole roster, so it is
		// one call at the end rather than one per pilot.
		m.syncPresence()
	}
	return moved, nil
}

// kickAllMessage runs kickBotsToSchool and words the result for a redirect, for both the NPC
// page and the player page. Every rostered bot wearing the badge goes back to the corporation
// it was created in, uncommitted, the same as one a director throws out.
func (m *Mod) kickAllMessage(ctx context.Context, corpID int64) (msg, errMsg string) {
	n, err := m.kickBotsToSchool(ctx, corpID)
	if err != nil {
		return "", err.Error()
	}
	if n == 0 {
		return "no rostered bot is in this corporation", ""
	}
	return fmt.Sprintf(
		"%d pilot(s) sent back to their school corporations", n), ""
}
