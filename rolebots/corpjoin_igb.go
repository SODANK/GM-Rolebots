package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"gurimoon/esp"
)

// /corp/join: the Overview's "Join" and "send me my desk" links. The in-game browser's identity header is a hint
// anybody can forge, so this page never acts on it. It writes a letter: the corporation mails the pilot the
// header names a signed one-click link (armPath, deskViewPath), and only the click from that pilot's own inbox
// moves anyone. A forged header costs the victim one letter they can ignore.
//
//	/corp/join?arm=industry   mail a confirmation for joining a department
//	/corp/join?desk=1         mail the member their own desk link

// joinMailCooldownSec is how often one pilot may be written the same letter:
// a reader clicking twice gets one letter, a forger looping gets one every
// few minutes.
const joinMailCooldownSec = 10 * 60

var joinMailLast = struct {
	sync.Mutex
	at map[string]int64
}{at: map[string]int64{}}

// joinMailDue reports whether a letter last sent at last may go again now.
// Pure.
func joinMailDue(last, now int64) bool {
	return last == 0 || now-last >= joinMailCooldownSec
}

// claimJoinMail takes the cooldown for one (pilot, letter). False when a
// letter went inside the window.
func claimJoinMail(charID int64, what string) bool {
	key := fmt.Sprintf("%d|%s", charID, what)
	now := clock.Unix()
	joinMailLast.Lock()
	defer joinMailLast.Unlock()
	if !joinMailDue(joinMailLast.at[key], now) {
		return false
	}
	joinMailLast.at[key] = now
	// Keep the map from growing without bound on a long uptime.
	if len(joinMailLast.at) > 4096 {
		for k, at := range joinMailLast.at {
			if now-at >= joinMailCooldownSec {
				delete(joinMailLast.at, k)
			}
		}
	}
	return true
}

// handleIGBCorpJoin serves /corp/join.
func (m *Mod) handleIGBCorpJoin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	pg := m.igbPageFor(r)
	charID, corpID := m.portalViewer(ctx, r)
	q := r.URL.Query()
	var msg string
	bad := true
	switch {
	case charID == 0 || corpID == 0:
		msg = "Open this page from your own terminal."
	case m.isSynthetic(charID):
		msg = "That terminal is not a capsuleer's."
	case q.Get("desk") != "":
		msg, bad = m.mailDeskLink(ctx, corpID, charID)
	case q.Get("arm") != "":
		msg, bad = m.mailArmConfirm(ctx, corpID, charID, corpArm(q.Get("arm")))
	case q.Get("carry") != "":
		msg, bad = m.mailCarryConfirm(ctx, corpID, charID, qInt(r, "carry"))
	default:
		msg = "Nothing was asked for."
	}
	body := pg.ordersBanner(msg, bad) +
		igbF3(`<a href="`+esp.IGBHost()+`/corp/">Back to the Overview</a>`)
	esp.WriteCorpPortalPage(w, r, "overview", body)
}

// mailArmConfirm writes a member the one-click link that puts them in a
// department, once the pick would stand. Answers the page's sentence.
func (m *Mod) mailArmConfirm(ctx context.Context, corpID, charID int64, arm corpArm) (string, bool) {
	known := false
	for _, a := range armOrder {
		if a == arm {
			known = true
		}
	}
	if !known {
		return "There is no such department.", true
	}
	c, cm, err := m.armChoiceCheck(ctx, corpID, charID, arm)
	if err != nil {
		return "You cannot join " + armLabel(arm) + ": " + err.Error() + ".", true
	}
	if seat, ok := armOf(charID, corpID); ok && seat.Arm == arm {
		return "You are already in " + armLabel(arm) + ".", false
	}
	if !claimJoinMail(charID, "arm:"+string(arm)) {
		return "A letter about " + armLabel(arm) + " is already in your inbox — open it and click the link.", false
	}
	from, hat := m.armLetterFrom(ctx, c)
	corpName := m.corpNameOf(ctx, corpID)
	var b strings.Builder
	line(&b, "%s,", cm.Name)
	b.WriteString("<br>")
	line(&b, "You asked to join %s. Its people %s.", armLabel(arm), armPitch(arm))
	if roles := armRoleShort(armRoles(arm, armTierMember, false)); roles != "" {
		line(&b, "Joining gives you these roles while you stay in it: %s.", roles)
	}
	b.WriteString("<br>")
	line(&b, "%s", igbLink(armPath(corpID, charID, arm), "Yes &mdash; put me in "+armLabel(arm)))
	b.WriteString("<br>")
	line(&b, "Nothing changes until you click. If you did not ask, ignore this letter.")
	b.WriteString(m.signature(ctx, from, hat, corpName))
	if m.mailPilot(ctx, charID, from, "Joining "+armLabel(arm)+" — "+corpName, b.String()) == 0 {
		return "The letter could not be delivered. Try again shortly.", true
	}
	return "A letter from " + m.charNameOr(ctx, from) + " is in your inbox. Click its link to join " +
		armLabel(arm) + ".", false
}

// carryPath is the signed link that gives a capsuleer a print move.
func carryPath(corpID, charID, moveID int64) string {
	t := signDesk(deskKeyAny(), "carry", corpID, charID, moveID)
	return fmt.Sprintf("/ig/corpdesk?corp=%d&c=%d&mv=%d&do=carry&t=%s", corpID, charID, moveID, t)
}

// mailCarryConfirm writes a member the one-click link that makes a print
// move theirs, once they may carry it. Answers the page's sentence.
func (m *Mod) mailCarryConfirm(ctx context.Context, corpID, charID, moveID int64) (string, bool) {
	mv, err := m.loadPrintMove(ctx, moveID)
	if err != nil || mv == nil || mv.CorpID != corpID {
		return "There is no such move.", true
	}
	if mv.Status != moveOpen {
		return "That print is already on its way with somebody else.", false
	}
	if !m.mayCarry(ctx, mv, charID) {
		if mv.Kind == moveOriginal {
			return "Only a Director or the Chief Executive carries an original.", true
		}
		return "A copy goes with a member posted at the office it leaves.", true
	}
	if !claimJoinMail(charID, fmt.Sprintf("carry:%d", moveID)) {
		return "A letter about this print is already in your inbox &mdash; open it and click the link.", false
	}
	c, _ := getCEO(ctx, m.db, corpID)
	if c == nil {
		return "This corporation keeps no member desks.", true
	}
	from, hat := m.rndLetterFrom(ctx, c)
	corpName := m.corpNameOf(ctx, corpID)
	name := m.typeNameOr(ctx, mv.BPTypeID)
	var b strings.Builder
	line(&b, "%s,", esp.IGBText(m.charNameOr(ctx, charID)))
	b.WriteString("<br>")
	line(&b, "You offered to carry the %s %s from our office at %s to our office at %s (%d jump(s)).",
		mv.Kind, esp.IGBText(name), esp.IGBText(stationNameOf(ctx, m, mv.From)),
		esp.IGBText(stationNameOf(ctx, m, mv.To)), mv.Jumps)
	if mv.Kind == moveOriginal {
		line(&b, "It is locked down in the Blueprint Library. Clicking the link below lifts the lock for you to take it.")
	}
	line(&b, "Put it into the corporation hangar at %s, any division. You are paid %s the moment it lands.",
		esp.IGBText(stationNameOf(ctx, m, mv.To)), iskString(mv.Wage))
	line(&b, "If you have not taken it within %s the move goes back to the board.", fmtDuration(printMoveHoldSec()))
	b.WriteString("<br>")
	line(&b, "%s", igbLink(carryPath(corpID, charID, moveID), "Yes &mdash; I will carry it"))
	b.WriteString(m.signature(ctx, from, hat, corpName))
	if m.mailPilot(ctx, charID, from, "Carrying "+name+" — "+corpName, b.String()) == 0 {
		return "The letter could not be delivered. Try again shortly.", true
	}
	return "A letter from " + m.charNameOr(ctx, from) + " is in your inbox. Click its link to take the print.", false
}

// rndLetterFrom is who writes about the library: the head of R&D when the
// department has one, else the CEO.
func (m *Mod) rndLetterFrom(ctx context.Context, c *corpCEO) (int64, string) {
	var id int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT characterID FROM rolebot_corp_arm_members
		 WHERE corporationID = ? AND arm = ? AND tier = ? ORDER BY since LIMIT 1`,
		c.CorpID, string(armResearch), armTierHead).Scan(&id)
	if id != 0 {
		return id, armHeadTitle(armResearch)
	}
	return c.CharID, "Chief Executive Officer"
}

// mailDeskLink writes a member the link to their own desk.
func (m *Mod) mailDeskLink(ctx context.Context, corpID, charID int64) (string, bool) {
	c, _ := getCEO(ctx, m.db, corpID)
	if c == nil {
		return "This corporation keeps no member desks.", true
	}
	if !claimJoinMail(charID, "desk") {
		return "Your desk link went to your inbox a few minutes ago.", false
	}
	from, hat := m.armLetterFrom(ctx, c)
	corpName := m.corpNameOf(ctx, corpID)
	var b strings.Builder
	line(&b, "%s,", m.charNameOr(ctx, charID))
	b.WriteString("<br>")
	line(&b, "Here is %s: your rank, your department, your work and what we have paid you.",
		igbLink(deskViewPath(corpID, charID), "your desk"))
	if assignmentsOn() {
		line(&b, "And %s, where the departments post work for you to take.",
			igbLink(assignBoardPath(corpID, charID), "the Assignments board"))
	}
	line(&b, "The link is yours alone; keep this letter.")
	b.WriteString(m.signature(ctx, from, hat, corpName))
	if m.mailPilot(ctx, charID, from, "Your desk — "+corpName, b.String()) == 0 {
		return "The letter could not be delivered. Try again shortly.", true
	}
	return "Your desk link is in your inbox.", false
}
