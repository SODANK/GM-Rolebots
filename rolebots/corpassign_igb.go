package rolebots

// /corp/assignments: the corporation's job board, a section of the Corporate Portal.
//
// Departments post work here (corpassign.go assignPost) and a capsuleer takes it; the first to
// take a job has it. There are two ways to the page:
//
//   - from a letter. A link in an EVE-mail opens inside the mail pane, not the browser, and no
//     link scheme reaches the real one without a client patch. The links carry a signed token
//     (corpdesk.go) and the page stays narrow enough to read there.
//   - from the browser, through the portal menu. The identity headers are forgeable, so a Take
//     mails the pilot a one-line confirmation whose link carries the token (as corpjoin_igb.go).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"gurimoon/corp"
	"gurimoon/esp"
)

const assignBoardPortalPath = "/corp/assignments"

// assignBoardPath is a member's own signed view of the board.
func assignBoardPath(corpID, charID int64) string {
	t := signDesk(deskKeyAny(), "view", corpID, charID)
	return fmt.Sprintf("%s?corp=%d&c=%d&t=%s", assignBoardPortalPath, corpID, charID, t)
}

// assignTakePath takes one job for a member.
func assignTakePath(corpID, charID, id int64) string {
	t := signDesk(deskKeyAny(), "take", corpID, charID, id)
	return fmt.Sprintf("%s?corp=%d&c=%d&a=%d&do=take&t=%s", assignBoardPortalPath, corpID, charID, id, t)
}

// assignDropPath hands one job back.
func assignDropPath(corpID, charID, id int64) string {
	t := signDesk(deskKeyAny(), "drop", corpID, charID, id)
	return fmt.Sprintf("%s?corp=%d&c=%d&a=%d&do=drop&t=%s", assignBoardPortalPath, corpID, charID, id, t)
}

func (m *Mod) handleIGBCorpAssignments(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	q := r.URL.Query()
	key := m.deskKey()
	corpID, charID := qInt(r, "corp"), qInt(r, "c")
	token := q.Get("t")
	aid := qInt(r, "a")
	action := q.Get("do")

	// Signed: the link came out of the pilot's own mailbox.
	signed := false
	notice := ""
	if corpID != 0 && charID != 0 {
		switch {
		case (action == "take" || action == "drop") && aid != 0 && checkDesk(key, token, action, corpID, charID, aid):
			signed = true
			notice = m.answerBoardAction(ctx, action, aid, charID)
		case checkDesk(key, token, "view", corpID, charID):
			signed = true
		default:
			notice = "That link is not valid."
		}
	}
	// Unsigned: the browser's own headers say who is looking.
	if !signed {
		charID, corpID = m.portalViewer(ctx, r)
		if action == "ask" && aid != 0 && charID != 0 {
			notice = m.mailTakeConfirm(ctx, corpID, charID, aid)
		}
	}
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "assignments",
			igbF3("Open this page from your own terminal to see your corporation's work."))
		return
	}
	c := m.deskCEO(ctx, corpID)
	if c == nil || !assignmentsOn() {
		esp.WriteCorpPortalPage(w, r, "assignments",
			igbF3("Your corporation does not post assignments."))
		return
	}
	esp.WriteCorpPortalPage(w, r, "assignments", m.assignBoardBody(ctx, r, c, charID, signed, notice))
}

// answerBoardAction applies a signed take or drop. The sentence is for the page.
func (m *Mod) answerBoardAction(ctx context.Context, action string, id, charID int64) string {
	if action == "drop" {
		a, err := m.dropAssignment(ctx, id, charID)
		switch {
		case err != nil:
			return boardRefusal(err)
		case a != nil && a.Status == asPosted:
			return "Handed back. It is on the board again for somebody else."
		default:
			return "Closed. You are credited for the share you did."
		}
	}
	a, err := m.takeAssignment(ctx, id, charID)
	if err != nil {
		return boardRefusal(err)
	}
	return "It is yours: " + m.assignmentText(ctx, *a) + ". You have " +
		fmtDuration(max(0, a.ExpiresAt-clock.Unix())) + "."
}

func boardRefusal(err error) string {
	switch {
	case errors.Is(err, errAssignGone):
		return "That job is no longer on the board."
	case errors.Is(err, errAssignTaken), errors.Is(err, errAssignFull), errors.Is(err, errAssignRole),
		errors.Is(err, errAssignBenched), errors.Is(err, errAssignNotMine):
		s := err.Error()
		return strings.ToUpper(s[:1]) + s[1:] + "."
	}
	return "Something went wrong; try the link again in a minute."
}

// mailTakeConfirm is a Take pressed in the browser: the pilot is mailed the signed link that
// takes it. Rate-limited per job.
func (m *Mod) mailTakeConfirm(ctx context.Context, corpID, charID, id int64) string {
	a, err := m.getAssignment(ctx, id)
	if err != nil || a == nil || a.CorpID != corpID || a.Status != asPosted {
		return "That job is no longer on the board."
	}
	if !claimJoinMail(charID, fmt.Sprintf("take:%d", id)) {
		return "The letter for that job is already in your inbox - open it and click the link."
	}
	from, hat := m.assignFrom(ctx, *a)
	var b strings.Builder
	line(&b, "%s,", m.charNameOr(ctx, charID))
	b.WriteString("<br>")
	line(&b, "You asked for: <b>%s</b>.", m.assignmentText(ctx, *a))
	line(&b, "%s", igbLink(assignTakePath(corpID, charID, id), "Yes &mdash; it is mine"))
	b.WriteString("<br>")
	line(&b, "Nothing changes until you click, and somebody else may take it first. If you did not ask, ignore this letter.")
	b.WriteString(m.signature(ctx, from, hat, m.corpNameOf(ctx, corpID)))
	if m.mailPilot(ctx, charID, from, "Take: "+m.assignmentText(ctx, *a), b.String()) == 0 {
		return "The letter could not be delivered. Try again shortly."
	}
	return "A letter from " + m.charNameOr(ctx, from) + " is in your inbox. Click its link to take the job."
}

// assignBoardBody is the page: the viewer's own work, then the board.
func (m *Mod) assignBoardBody(ctx context.Context, r *http.Request, c *corpCEO, viewer int64, signed bool, notice string) string {
	pg := m.igbPageFor(r)
	var b strings.Builder
	if notice != "" {
		b.WriteString("<font size=4 color=" + pg.pal.Accent + ">" + esp.IGBText(notice) + "</font><br><br>\n")
	}
	b.WriteString(pg.soft("Work the departments need done, paid on delivery and worth merit toward your next rank. "+
		"First to take a job has it.") + "<br><br>\n")

	var me *corpMember
	if viewer != 0 {
		if members, err := m.corpMembers(ctx, c.CorpID, c.CharID); err == nil {
			for i := range members {
				if members[i].CharID == viewer && members[i].Human() {
					me = &members[i]
					break
				}
			}
		}
	}
	pay := 1.0
	if me != nil {
		pay = m.meritPayFor(ctx, c.CorpID, viewer)
		held, _ := m.queryAssignments(ctx, `
			WHERE corporationID = ? AND characterID = ? AND status IN ('offered', 'accepted')
			ORDER BY acceptedAt DESC`, c.CorpID, viewer)
		mine := esp.Panel{Title: "Your work", Align: []string{"", "", "right"}, Width: []string{"36", "", ""},
			Empty: "You hold no work. Take a job below."}
		now := clock.Unix()
		for _, a := range held {
			state := ""
			switch a.Status {
			case asAccepted:
				state = pg.tint(pg.pal.Green, fmt.Sprintf("%d of %d", a.Progress, a.Qty)) + " " +
					pg.soft("due in "+fmtDuration(max(0, a.ExpiresAt-now)))
				if signed {
					state += " " + pg.soft(`<a href="`+esp.IGBHost()+assignDropPath(c.CorpID, viewer, a.ID)+`">hand back</a>`)
				}
			case asOffered:
				if now-a.OfferedAt > assignOfferSec {
					continue
				}
				state = pg.tint(pg.pal.Amber, "offered to you") + " " + pg.soft(
					`<a href="`+esp.IGBHost()+deskActionPath(a, "accept")+`">accept</a> / `+
						`<a href="`+esp.IGBHost()+deskActionPath(a, "decline")+`">decline</a>`)
			}
			mine.Rows = append(mine.Rows, []string{assignIcon(a),
				igbF3(esp.IGBText(m.assignmentText(ctx, a))) + "<br>" + state,
				igbF3(esp.IGBText(iskString(math.Floor(a.Reward*pay))))})
		}
		esp.PanelBand(&b, pg.pal, []esp.Panel{mine})
		b.WriteString("<br>\n")
	}

	posted, _ := m.queryAssignments(ctx, `
		WHERE corporationID = ? AND status = ? AND expiresAt > ? ORDER BY offeredAt`,
		c.CorpID, asPosted, clock.Unix())
	if me != nil {
		// Suited first, the rest after.
		suited := m.assignSuits(ctx, *me, posted)
		seen := map[int64]bool{}
		for _, a := range suited {
			seen[a.ID] = true
		}
		for _, a := range posted {
			if !seen[a.ID] {
				suited = append(suited, a)
			}
		}
		posted = suited
	}
	board := esp.Panel{Title: "On the board", Align: []string{"", "", "right"}, Width: []string{"36", "", ""},
		Empty: "Nothing posted right now. The departments post work when they need it."}
	for _, a := range posted {
		from, hat := m.assignFrom(ctx, a)
		who := pg.soft("Posted by " + esp.IGBText(m.charNameOr(ctx, from)) + ", " + esp.IGBText(hat) +
			" &middot; " + fmtDuration(max(0, a.ExpiresAt-clock.Unix())) + " left")
		act := ""
		switch {
		case me == nil:
		case a.Kind == assignBuild && me.Roles&corp.RoleFactoryManager == 0:
			act = pg.soft("needs Factory Manager")
		case signed:
			act = `<a href="` + esp.IGBHost() + assignTakePath(c.CorpID, viewer, a.ID) + `">Take it</a>`
		default:
			act = `<a href="` + esp.IGBHost() + fmt.Sprintf("%s?do=ask&a=%d", assignBoardPortalPath, a.ID) +
				`">Take it</a> ` + pg.soft("(confirmed by mail)")
		}
		board.Rows = append(board.Rows, []string{assignIcon(a),
			igbF3("<b>"+esp.IGBText(m.assignmentText(ctx, a))+"</b>") + "<br>" + who,
			igbF3(esp.IGBText(iskString(math.Floor(a.Reward*pay)))) + "<br>" +
				pg.soft(fmt.Sprintf("%.0f merit", a.MeritPts)) + "<br>" + igbF3(act)})
	}
	esp.PanelBand(&b, pg.pal, []esp.Panel{board})
	b.WriteString("<br>\n")
	if me != nil {
		fmt.Fprintf(&b, "%s<br>\n", pg.soft(fmt.Sprintf(
			"You may hold %d job(s) at once. A job you take runs %s; hand it back untouched and it returns to the board.",
			assignOpenMax(), fmtDuration(assignExpireSec()))))
	}
	return b.String()
}

// assignIcon is a job's picture: the goods, the product, the tower, or a combat hull for a
// patrol.
func assignIcon(a assignment) string {
	if a.Kind == assignPatrol || a.TypeID == 0 {
		return roleIcon(RoleCombat)
	}
	return esp.ShipIcon(a.TypeID, 32)
}
