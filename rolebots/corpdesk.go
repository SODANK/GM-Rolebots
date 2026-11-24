package rolebots

// The member's desk and the corporation's notice board in the in-game browser.
//   /ig/corpdesk?corp=N: a corporation's public profile.
//   /ig/corpdesk?corp=N&c=M&t=TOKEN: member M's own desk (rank, merit,
//     assignments, pay). Personal, so it needs the token.
//   ...&a=ID&do=accept|decline&t=TOKEN: answers an assignment.
//   /corp/bulletins (old address /ig/corpnews): the corporation's standing state.
// The eve.charID header is forgeable (esp/igbmodule.go), so it only chooses
// which public page to show. Anything personal or acting is keyed on an HMAC
// token over (action, corporation, member, assignment), mailed only to that
// pilot. IGB rules (docs/galnet-igb.md): flat tables, no forms, no CSS.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

const settingDeskSecret = "rolebots.corp.deskSecret"

var (
	deskSecretMu sync.Mutex
	deskSecret   []byte
)

// deskKey is the link-signing secret, generated and stored on first use.
// Without a database answer it signs with a process-local key: links still
// work until a restart, which beats links that never work.
func (m *Mod) deskKey() []byte {
	deskSecretMu.Lock()
	defer deskSecretMu.Unlock()
	if deskSecret != nil {
		return deskSecret
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if m.db != nil {
		if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingDeskSecret); err == nil && ok {
			if b, err := hex.DecodeString(strings.TrimSpace(v)); err == nil && len(b) >= 16 {
				deskSecret = b
				return deskSecret
			}
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("rolebots: desk secret: %v", err))
	}
	if m.db != nil {
		if err := evedb.SetServerSetting(ctx, m.db, settingDeskSecret, hex.EncodeToString(b)); err != nil {
			rlogf("rolebots: store desk secret: %v (links will not survive a restart)", err)
		}
	}
	deskSecret = b
	return deskSecret
}

// signDesk is the pure token: HMAC-SHA256 over the parts, 16 bytes in hex.
func signDesk(key []byte, parts ...any) string {
	mac := hmac.New(sha256.New, key)
	for i, p := range parts {
		if i > 0 {
			mac.Write([]byte{'|'})
		}
		fmt.Fprint(mac, p)
	}
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// checkDesk compares in constant time.
func checkDesk(key []byte, token string, parts ...any) bool {
	if len(key) < 16 {
		return false
	}
	want := signDesk(key, parts...)
	return token != "" && hmac.Equal([]byte(want), []byte(token))
}

// The module is a singleton; the link builders are called from places that
// do not carry *Mod (and should not need to).
var deskMod *Mod

// deskKeyAny is the signing key, never empty: with no module wired (a
// harness, say) it falls back to a process-random key rather than signing
// with nothing, which anybody could forge.
func deskKeyAny() []byte {
	if deskMod != nil {
		return deskMod.deskKey()
	}
	deskSecretMu.Lock()
	defer deskSecretMu.Unlock()
	if deskSecret == nil {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic(fmt.Sprintf("rolebots: desk secret: %v", err))
		}
		deskSecret = b
	}
	return deskSecret
}

// deskViewPath is a member's own desk.
func deskViewPath(corpID, charID int64) string {
	t := signDesk(deskKeyAny(), "view", corpID, charID)
	return fmt.Sprintf("/ig/corpdesk?corp=%d&c=%d&t=%s", corpID, charID, t)
}

// deskActionPath answers one assignment.
func deskActionPath(a assignment, action string) string {
	t := signDesk(deskKeyAny(), action, a.CorpID, a.CharID, a.ID)
	return fmt.Sprintf("/ig/corpdesk?corp=%d&c=%d&a=%d&do=%s&t=%s", a.CorpID, a.CharID, a.ID, action, t)
}

func qInt(r *http.Request, k string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(k), 10, 64)
	return n
}

// ── /ig/corpdesk ────────────────────────────────────────────────────────────

func (m *Mod) handleIGBCorpDesk(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	corpID := qInt(r, "corp")
	charID := qInt(r, "c")
	token := r.URL.Query().Get("t")
	key := m.deskKey()

	if corpID == 0 {
		// No link: show the viewer's own corporation's profile by the header (public
		// information only).
		if id := esp.IGBCharID(r); id != 0 {
			corpID, _ = evedb.GetCharCorpID(ctx, m.db, id)
		}
	}
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "", "<font size=3>Open your desk from a link in a letter "+
			"from your corporation.</font>")
		return
	}

	var b strings.Builder
	notice := ""
	// A Base picked from the CEO's posting letter (corpposting.go).
	if base := qInt(r, "base"); base != 0 && charID != 0 {
		if checkDesk(key, token, "base", corpID, charID, base) {
			if name, err := m.memberChoosesBase(ctx, corpID, charID, base); err != nil {
				notice = "That office cannot be your Base: " + err.Error() + "."
			} else {
				notice = "Your Base is now " + name + ". Nobody will move you from it."
			}
			token = signDesk(key, "view", corpID, charID)
		} else {
			notice = "That link is not valid."
		}
	}
	// An arm picked from a letter or the desk (corparm_human.go).
	if arm := corpArm(r.URL.Query().Get("arm")); arm != "" && charID != 0 {
		if checkDesk(key, token, "arm", corpID, charID, string(arm)) {
			if label, err := m.memberChoosesArm(ctx, corpID, charID, arm); err != nil {
				notice = "You cannot join that department: " + err.Error() + "."
			} else {
				notice = "You are in " + label + " now."
				if postingsOn() && humanPostingsOn() {
					notice += " Your posting to its office follows by mail."
				}
			}
			token = signDesk(key, "view", corpID, charID)
		} else {
			notice = "That link is not valid."
		}
	}
	// A print move taken from the R&D page's letter (corpprintmove.go).
	if mvID := qInt(r, "mv"); mvID != 0 && charID != 0 {
		if r.URL.Query().Get("do") == "carry" && checkDesk(key, token, "carry", corpID, charID, mvID) {
			mv, err := m.humanClaimPrintMove(ctx, mvID, charID)
			switch {
			case errors.Is(err, errMoveGone):
				notice = "That print is already on its way with somebody else."
			case err != nil:
				notice = "You cannot carry it: " + err.Error() + "."
			default:
				notice = fmt.Sprintf("It is yours. Take the %s out of the office at %s and put it into the "+
					"corporation hangar at %s. %s is paid on arrival.", m.typeNameOr(ctx, mv.BPTypeID),
					stationNameOf(ctx, m, mv.From), stationNameOf(ctx, m, mv.To), iskString(mv.Wage))
			}
			token = signDesk(key, "view", corpID, charID)
		} else {
			notice = "That link is not valid."
		}
	}
	if aid := qInt(r, "a"); aid != 0 && charID != 0 {
		action := r.URL.Query().Get("do")
		if (action == "accept" || action == "decline") && checkDesk(key, token, action, corpID, charID, aid) {
			a, err := m.answerAssignment(ctx, aid, action == "accept")
			switch {
			case errors.Is(err, errAssignGone):
				notice = "That assignment is no longer open."
			case err != nil:
				notice = "Something went wrong; try the link again in a minute."
			case a != nil && a.Status == asAccepted:
				notice = "Accepted. You have " + fmtDuration(a.ExpiresAt-clock.Unix()) + " — it is yours."
			case a != nil:
				notice = "Declined. There will be other work."
			}
			// Re-sign as a view token so the rest of the page renders the desk.
			token = signDesk(key, "view", corpID, charID)
		} else {
			notice = "That link is not valid."
		}
	}

	c, _ := getCEO(ctx, m.db, corpID)
	if c == nil {
		if notice == "" {
			notice = "This corporation keeps no member desks."
		}
		esp.WriteCorpPortalPage(w, r, "", igbF3(esp.IGBText(notice)))
		return
	}
	if notice != "" {
		b.WriteString("<font size=4 color=#ffcc66>" + esp.IGBText(notice) + "</font><br><br>")
	}
	if charID != 0 && checkDesk(key, token, "view", corpID, charID) {
		m.writeMemberDesk(ctx, &b, c, charID)
	} else {
		m.writeCorpProfile(ctx, &b, c)
	}
	esp.WriteCorpPortalPage(w, r, "", b.String())
}

// writeCorpProfile is the public page: who, what, and how rank works.
func (m *Mod) writeCorpProfile(ctx context.Context, b *strings.Builder, c *corpCEO) {
	s := m.corpSnapshotFor(ctx, c.CorpID, c.CharID)
	facts, _ := getAnyCorpFacts(ctx, m.db, c.CorpID)
	b.WriteString("<h2>" + esp.IGBText(s.Name) + "</h2><font size=3>")
	b.WriteString("Chief Executive: " + esp.IGBText(s.CEOName) + "<br>")
	if facts != nil {
		fmt.Fprintf(b, "Members: %d", facts.Members)
		if facts.MemberLimit > 0 {
			fmt.Fprintf(b, " of %d", facts.MemberLimit)
		}
		b.WriteString("<br>")
		if facts.StationName != "" {
			b.WriteString("Headquarters: " + esp.IGBText(facts.StationName) + "<br>")
		}
	}
	if len(s.Tags) > 0 {
		b.WriteString("What we do: " + esp.IGBText(strings.Join(s.Tags, ", ")) + "<br>")
	}
	if s.Directive != "" {
		b.WriteString("Where the effort goes: " + esp.IGBText(s.Directive) + "<br>")
	}
	b.WriteString("<br><b>How rank is earned</b><br>")
	b.WriteString("Rank is work and time served: building on the work board, the assignments we send, " +
		"and the weeks you stay. Each rank opens more of our hangars and pays better.<br><br>")
	m.writeLadderTable(b, -1)
	b.WriteString("</font>")
}

// writeLadderTable draws the ladder; mark is the reader's rank (-1 none).
func (m *Mod) writeLadderTable(b *strings.Builder, mark int) {
	b.WriteString("<table width=100% border=0 cellpadding=2>")
	b.WriteString("<tr><td><b>Rank</b></td><td><b>Merit</b></td><td><b>Days served</b></td><td><b>Pay</b></td><td><b>Opens</b></td></tr>")
	for i, r := range meritLadder {
		bg := ""
		if i == mark {
			bg = " bgcolor=#333322"
		}
		opens := strings.Join(meritRoleWords(rankRoles(i, false)&^rankRoles(i-1, false)), "; ")
		if i == 0 {
			opens = "—"
		}
		fmt.Fprintf(b, "<tr%s><td>%s</td><td>%.0f</td><td>%.0f</td><td>%.0f%%</td><td>%s</td></tr>",
			bg, esp.IGBText(r.Name), r.Points, r.TenureDays, r.Pay*100, esp.IGBText(opens))
	}
	b.WriteString("</table>")
}

// writeMemberDesk is one member's own page.
func (m *Mod) writeMemberDesk(ctx context.Context, b *strings.Builder, c *corpCEO, charID int64) {
	name := m.charNameOr(ctx, charID)
	corpName := m.corpNameOf(ctx, c.CorpID)
	b.WriteString("<h2>" + esp.IGBText(name) + " — " + esp.IGBText(corpName) + "</h2><font size=3>")

	var cur int64
	_ = m.db.QueryRowContext(ctx, `SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&cur)
	if cur != c.CorpID {
		b.WriteString("You are no longer a member of this corporation.</font>")
		return
	}

	var pts float64
	var rank int
	_ = m.db.QueryRowContext(ctx, `
		SELECT points, rankNo FROM rolebot_corp_merit WHERE corporationID = ? AND characterID = ?`,
		c.CorpID, charID).Scan(&pts, &rank)
	rank = clampRank(rank)
	fmt.Fprintf(b, "<b>Rank:</b> %s &nbsp; <b>Merit:</b> %.0f &nbsp; <b>Pay:</b> %.0f%%<br>",
		esp.IGBText(meritRankName(rank)), pts, meritPayMult(rank)*100)
	if !meritOn() {
		b.WriteString("Ranks are not being awarded yet; your work is on the books.<br>")
	} else if rank+1 < len(meritLadder) {
		nx := meritLadder[rank+1]
		fmt.Fprintf(b, "Next: %s at %.0f merit (%.0f to go) and %.0f days' service.<br>",
			esp.IGBText(nx.Name), nx.Points, max(0, nx.Points-pts), nx.TenureDays)
	}

	// The member's department, and the others they may move to.
	if humanPipelineOn() && !m.isSynthetic(charID) {
		cur := corpArm("")
		if seat, ok := armOf(charID, c.CorpID); ok {
			cur = seat.Arm
		}
		var arms strings.Builder
		offered := writeArmChoices(&arms, c.CorpID, charID, cur, m.corpJoinableArms(ctx, c))
		if cur != "" {
			b.WriteString("<b>Department:</b> " + esp.IGBText(armLabel(cur)) + "<br>")
		}
		if offered {
			if cur == "" {
				b.WriteString("You are in no department yet. Choose one:<br>")
			} else {
				b.WriteString("Move to another department:<br>")
			}
			b.WriteString(arms.String())
		}
	}

	var earned float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(-amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND counterID = ? AND line IN (?, ?) AND at >= ?`,
		c.CorpID, charID, lineWages, linePayroll, clock.Unix()-30*86400).Scan(&earned)
	fmt.Fprintf(b, "Paid by the corporation, last 30 days: %s<br>", esp.IGBText(iskString(earned)))
	if payrollManaged(c.CorpID) {
		// The weekly payroll (corppayroll.go): what is on the record and
		// when the cheques are signed.
		var credit float64
		var next int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(credit), 0) FROM rolebot_corp_labour
			 WHERE corporationID = ? AND characterID = ? AND paidAt = 0`, c.CorpID, charID).Scan(&credit)
		_ = m.db.QueryRowContext(ctx,
			`SELECT nextPayAt FROM rolebot_corp_payroll WHERE corporationID = ?`, c.CorpID).Scan(&next)
		fmt.Fprintf(b, "Work credited since the last payroll: %s", esp.IGBText(iskString(credit)))
		if next > 0 {
			fmt.Fprintf(b, " &mdash; next payroll %s", esp.IGBText(igbIn(next)))
		}
		b.WriteString("<br>Members are paid weekly, out of the week's profit, by the merit they earned.<br>")
	}

	// Assignments.
	as, _ := m.queryAssignments(ctx, `
		WHERE corporationID = ? AND characterID = ?
		ORDER BY FIELD(status, 'accepted', 'offered') DESC, offeredAt DESC LIMIT 12`, c.CorpID, charID)
	b.WriteString("<br><b>Assignments</b> &nbsp; " + `<a href="` + esp.IGBHost() + assignBoardPath(c.CorpID, charID) +
		`">the board</a><br>`)
	if len(as) == 0 {
		if assignmentsOn() {
			b.WriteString("None yet &mdash; take one off the board.<br>")
		} else {
			b.WriteString("This corporation is not handing out assignments yet.<br>")
		}
	} else {
		b.WriteString("<table width=100% border=0 cellpadding=2>")
		b.WriteString("<tr><td><b>Work</b></td><td><b>Done</b></td><td><b>Pays</b></td><td><b>State</b></td></tr>")
		now := clock.Unix()
		for _, a := range as {
			state := a.Status
			switch a.Status {
			case asOffered:
				if now-a.OfferedAt > assignOfferSec {
					state = "lapsed"
				} else {
					state = `<a href="` + esp.IGBHost() + deskActionPath(a, "accept") + `">accept</a> / ` +
						`<a href="` + esp.IGBHost() + deskActionPath(a, "decline") + `">decline</a>`
				}
			case asAccepted:
				state = "due in " + fmtDuration(max(0, a.ExpiresAt-now))
			case asDone, asExpired:
				state = a.Status + ", paid " + iskString(a.PaidISK)
			}
			fmt.Fprintf(b, "<tr><td>%s</td><td>%d/%d</td><td>%s</td><td>%s</td></tr>",
				esp.IGBText(m.assignmentText(ctx, a)), a.Progress, a.Qty, esp.IGBText(iskString(a.Reward)), state)
		}
		b.WriteString("</table>")
	}

	// The merit trail.
	rows, err := m.db.QueryContext(ctx, `
		SELECT at, points, reason FROM rolebot_corp_merit_log
		 WHERE corporationID = ? AND characterID = ? ORDER BY at DESC LIMIT 10`, c.CorpID, charID)
	if err == nil {
		var lines []string
		for rows.Next() {
			var at int64
			var p float64
			var why string
			if rows.Scan(&at, &p, &why) == nil {
				lines = append(lines, fmt.Sprintf("<tr><td>%s ago</td><td>+%.0f</td><td>%s</td></tr>",
					agoString(clock.Unix()-at), p, esp.IGBText(why)))
			}
		}
		rows.Close()
		if len(lines) > 0 {
			b.WriteString("<br><b>Recent merit</b><br><table width=100% border=0 cellpadding=2>")
			b.WriteString(strings.Join(lines, ""))
			b.WriteString("</table>")
		}
	}
	b.WriteString("<br><b>The ladder</b><br>")
	m.writeLadderTable(b, rank)
	b.WriteString("<br>" + `<a href="` + esp.IGBHost() + `/corp/bulletins">Bulletins</a> · ` +
		`<a href="` + esp.IGBHost() + `/corp/industry">Industry</a>`)
	b.WriteString("</font>")
}

// ── /corp/bulletins (/ig/corpnews) ──────────────────────────────────────────────────────────────────────────────────────────────────────

func (m *Mod) handleIGBCorpNews(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var corpID int64
	if id := esp.IGBCharID(r); id != 0 {
		corpID, _ = evedb.GetCharCorpID(ctx, m.db, id)
	}
	ceoID := m.corpCEOID(ctx, corpID)
	if corpID == 0 || ceoID == 0 {
		esp.WriteCorpPortalPage(w, r, "bulletins", "<font size=3>Your corporation posts no bulletins. "+
			"Corporations that keep their members informed publish them here.</font>")
		return
	}
	esp.WriteCorpPortalPage(w, r, "bulletins", m.bulletinsBody(ctx, r, corpID, ceoID, esp.IGBCharID(r)))
}

// bulletinsBody is the notice board as a page. Panels are drawn in one-line
// rows: a band shares one table row per row index across its panels, so one tall
// cell (a 64 px portrait beside the directive) would stretch the whole row. It
// works for any corporation, whether or not the module sits in the chair.
func (m *Mod) bulletinsBody(ctx context.Context, r *http.Request, corpID, ceoID, viewer int64) string {
	pg := m.igbPageFor(r)
	s := m.corpSnapshotFor(ctx, corpID, ceoID)
	host := esp.IGBHost()
	var b strings.Builder

	// From the chair: the CEO, then the directive one activity a row.
	chair := esp.Panel{Title: "From the chair", Align: []string{"", "", "right"}, Width: []string{"80", "", "40"}}
	chair.Rows = append(chair.Rows, []string{esp.Portrait(ceoID, 32),
		igbF3("<b>"+esp.PilotLink(ceoID, esp.IGBText(s.CEOName))+"</b>") + " " + pg.soft("Chief Executive"), ""})
	if d, _, err := m.loadCorpDirective(ctx, corpID); err == nil && len(d) > 0 {
		for _, k := range directiveOrder(d) {
			chair.Rows = append(chair.Rows, []string{igbF3(esp.IGBText(k)), esp.Bar(d[k], 14, pg.pal.Accent),
				igbF3(fmt.Sprintf("%.0f%%", 100*d[k]))})
		}
		if s.DirectiveWhy != "" {
			chair.Rows = append(chair.Rows, []string{"", pg.soft(esp.IGBText(s.DirectiveWhy)), ""})
		}
	} else {
		chair.Rows = append(chair.Rows, []string{"", igbF3("No standing directive &mdash; work where you do best."), ""})
	}

	// The books.
	books := esp.Panel{Title: "The books", Note: "last seven days", Align: []string{"", "", "right"}, Width: []string{"80", "", "70"}}
	scale := max(s.Income, s.Cost)
	frac := func(v float64) float64 {
		if scale <= 0 {
			return 0
		}
		return v / scale
	}
	net := s.Income - s.Cost
	netColor := pg.pal.Green
	if net < 0 {
		netColor = pg.pal.Red
	}
	books.Rows = [][]string{
		{pg.dim3("Came in"), esp.Bar(frac(s.Income), 14, pg.pal.Up), igbF3(esp.ISKShort(s.Income))},
		{pg.dim3("Went out"), esp.Bar(frac(s.Cost), 14, pg.pal.Down), igbF3(esp.ISKShort(s.Cost))},
		{pg.dim3("Net"), "", pg.tint(netColor, esp.ISKShort(net))},
	}
	if s.OpenWork > 0 {
		books.Rows = append(books.Rows, []string{pg.dim3("Paid work"),
			igbF3(fmt.Sprintf(`<a href="%s/corp/industry">%d order(s) open</a>`, host, s.OpenWork)), ""})
	}

	esp.PanelBand(&b, pg.pal, []esp.Panel{chair, books})
	b.WriteString("<br>\n")

	// The standing fleet orders, full width: an order is a sentence, and a
	// sentence beside the chair stretched its first row.
	orders := esp.Panel{Title: "Fleet orders", Note: "standing, from the department heads",
		Align: []string{"", ""}, Width: []string{"30", ""}, Empty: "None &mdash; fleets pick their own ground."}
	for _, o := range m.listFleetOrders(ctx, corpID) {
		orders.Rows = append(orders.Rows, []string{roleIcon(fleetOrderRole(o.Dept)),
			igbF3(esp.IGBText(fleetOrderText(o.Dept, m.nameOfSystem(ctx, o.SystemID), strings.Join(o.OreNames, ", "), o.Reason)))})
	}
	esp.PanelBand(&b, pg.pal, []esp.Panel{orders})
	b.WriteString("<br>\n")

	// The officers, two panels across, one officer a row.
	type officer struct {
		post string
		sm   *staffMember
	}
	var list []officer
	if st, err := m.loadCorpStaff(ctx, corpID, ceoID); err == nil && st != nil {
		if st.Treasurer != nil {
			list = append(list, officer{"Treasurer", st.Treasurer})
		}
		for _, d := range st.Directors {
			post := "Director"
			if d.Portfolio != "" {
				post = portfolioTitle(d.Portfolio)
			}
			list = append(list, officer{post, d})
		}
		for _, p := range st.Personnel {
			list = append(list, officer{"Recruitment Officer", p})
		}
	}
	officerPanel := func(title string, part []officer) esp.Panel {
		p := esp.Panel{Title: title, Align: []string{"", "", ""}, Width: []string{"36", "", "170"},
			Empty: "No officers appointed."}
		for _, o := range part {
			icon, name := m.pilotCells(ctx, o.sm.CharID, 32)
			shade := ""
			if o.sm.CharID == viewer {
				shade = igbShadeMine
			}
			p.Rows = append(p.Rows, []string{icon, igbF3(name), pg.soft(esp.IGBText(o.post))})
			p.Shades = append(p.Shades, shade)
		}
		return p
	}
	half := (len(list) + 1) / 2
	left, right := officerPanel("Officers", list[:half]), officerPanel("&nbsp;", list[half:])
	right.Empty = ""
	esp.PanelGrid(&b, pg.pal, [][]esp.Panel{{left, right}})
	b.WriteString("<br>\n")

	// The week, and lately.
	carried := esp.Panel{Title: "Who carried us this week", Align: []string{"", "", "right"}, Width: []string{"20", "", "90"},
		Empty: "Nobody has earned merit yet this week."}
	for i, t := range s.TopWorkers {
		carried.Rows = append(carried.Rows, []string{pg.dim3(fmt.Sprintf("%d.", i+1)), igbF3(esp.IGBText(t[0])),
			igbF3(t[1]) + " " + pg.dim("merit")})
	}
	recent := esp.Panel{Title: "Lately", Align: []string{"", ""}, Width: []string{"110", ""}, Empty: "Nothing to report."}
	for _, l := range s.Recent {
		recent.Rows = append(recent.Rows, []string{pg.dim3(igbAgo(l.At)), igbF3(esp.IGBText(l.Text))})
	}
	esp.PanelBand(&b, pg.pal, []esp.Panel{carried, recent})

	b.WriteString("<br>" + pg.soft(`Your rank, assignments and pay are on your desk &mdash; `+
		`<a href="`+host+`/corp/join?desk=1">mail me my desk link</a>. `+
		`Departments, and joining one, are on the <a href="`+host+`/corp/">Overview</a>.`))
	return b.String()
}

// directiveOrder lists a directive's activities heaviest first.
func directiveOrder(d map[string]float64) []string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if d[keys[i]] != d[keys[j]] {
			return d[keys[i]] > d[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// fleetOrderRole is the seat a department's order is about, for its icon.
func fleetOrderRole(activity string) string {
	switch activity {
	case ActMining, ActIndMining:
		return RoleMiner
	case ActSecurity:
		return RoleCombat
	}
	return RoleHunter
}
