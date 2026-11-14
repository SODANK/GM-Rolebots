package rolebots

// The human applicant and the new human member: what a player who walks up to a bot-run corporation is told.
// The desk (corpceo_hr.go) judges a player fairly; this file is the voice and the clock.
// mailApplicant writes the answer from whoever read the desk, in the CEO's own words for a refusal (which can
// only be the CEO's Tolerance). humanOfferSweep lapses an unanswered human offer after humanOfferSec, freeing
// the opening corpFreeOpenings counted, since chrApplications has no offered-at column. welcomeSweep notices a
// player who has joined (there is no join hook) and writes the welcome: what the corporation does, what the CEO
// wants, how rank is earned, the two in-game browser pages and the departments they may choose
// (corparm_human.go, armInviteSweep). All of it sits behind settingHumanPipeline except the offer clock's
// bookkeeping, so switching the pipeline on does not lapse every offer at once. Only humans are mailed.

import (
	"context"
	"fmt"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/modapi"
)

// What the desk did with a human's application, for the letter.
const (
	applicantOffered = iota
	applicantHeld
	applicantRefused
	applicantStale
	applicantOverflow
	applicantLapsed
)

// welcomeQuietSec: a member who joined longer ago than this when the
// pipeline first sees them is stamped welcomed without a letter. Switching
// the pipeline on must not welcome a veteran of six months.
const welcomeQuietSec = 7 * 86400

// mailPilot writes one personal mail and rings the inbox. Best effort; a
// suppressed (bot) mailbox comes back 0 and is not rung.
func (m *Mod) mailPilot(ctx context.Context, to, from int64, subject, body string) int64 {
	subject = mailSubject(subject)
	created := win32From(clock.Now())
	msgID, err := evedb.SendEveMail(ctx, m.db, to, from, subject, body, created)
	if err != nil {
		rlogf("rolebots: mail to %d (%q): %v", to, subject, err)
		return 0
	}
	if msgID == 0 {
		return 0
	}
	if h := modapi.Gangs(); h.PushMail != nil {
		h.PushMail(to, msgID, from, subject, created)
	}
	return msgID
}

// mailSubjectASCII maps the punctuation our letters use to what the inbox's
// subject column can show.
var mailSubjectASCII = strings.NewReplacer(
	"—", "-", "–", "-", "‘", "'", "’", "'", "“", `"`, "”", `"`, "…", "...", " ", " ", "·", "-")

// mailSubject makes a subject safe for the inbox list. The client decodes the subject column as cp1252, so a
// UTF-8 em dash shows as mojibake there. Every subject goes out as plain ASCII: usual punctuation mapped,
// anything else dropped.
func mailSubject(s string) string {
	s = mailSubjectASCII.Replace(s)
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return strings.Map(func(r rune) rune {
				if r >= 0x80 {
					return -1
				}
				return r
			}, s)
		}
	}
	return s
}

// igbLink is an in-game browser link embedded in mail text. Always words, never a bare URL, which reads as
// machine-written.
func igbLink(path, text string) string {
	return `<a href="` + esp.IGBHost() + path + `">` + text + `</a>`
}

// signature is the closing line of a corporation letter: the writer's name
// and the hat they wear.
func (m *Mod) signature(ctx context.Context, from int64, hat, corpName string) string {
	return fmt.Sprintf("<br>-- %s<br>%s, %s", m.charNameOr(ctx, from), hat, corpName)
}

// readerHat is how the desk's reader signs: an officer by their post, the
// CEO as Chief Executive.
func readerHat(c *corpCEO, rd hrReader) string {
	if rd.CharID == c.CharID {
		return "Chief Executive Officer"
	}
	return "Recruitment Officer"
}

// mailApplicant tells a human applicant what the desk did. The offer's clock
// starts here whether or not the letter goes out.
func (m *Mod) mailApplicant(ctx context.Context, c *corpCEO, rd hrReader, a hrApplicant, kind int, why string) {
	if kind == applicantOffered {
		m.stampHumanOffer(ctx, c.CorpID, a.CharID)
	}
	if !humanPipelineOn() {
		return
	}
	corpName := m.corpNameOf(ctx, c.CorpID)
	var b strings.Builder
	line(&b, "%s,", a.Name)
	b.WriteString("<br>")
	var subject string
	switch kind {
	case applicantOffered:
		subject = "Your application to " + corpName + " — accepted"
		line(&b, "Your application to %s has been accepted. A place is held in your name for %s.",
			corpName, fmtDuration(humanOfferSec()))
		line(&b, "To take it, open your corporation window, go to Applications, and accept our offer.")
		line(&b, "Before you do, you can read what we are and what we pay: %s.",
			igbLink(fmt.Sprintf("/corp/profile?id=%d", c.CorpID), "our corporation profile"))
	case applicantHeld:
		subject = "Your application to " + corpName + " — waiting list"
		line(&b, "We would take you, but %s is full today. You are on our waiting list.", corpName)
		line(&b, "When an opening comes up it is yours, and you will hear from us. Nothing is needed from you meanwhile.")
	case applicantRefused:
		subject = "Your application to " + corpName
		line(&b, "We will not be taking you on. The Chief Executive's reason: %s.", why)
	case applicantStale:
		subject = "Your application to " + corpName + " — closed"
		line(&b, "You waited on our list and no opening came free. We have closed your application rather than keep you hanging.")
		line(&b, "You are welcome to apply again.")
	case applicantOverflow:
		subject = "Your application to " + corpName + " — closed"
		line(&b, "%s is full and our waiting list is taken. We have closed your application.", corpName)
		line(&b, "You are welcome to apply again later.")
	case applicantLapsed:
		subject = "Your offer from " + corpName + " has lapsed"
		line(&b, "We held a place for you for %s and heard nothing, so we have given it up.",
			fmtDuration(humanOfferSec()))
		line(&b, "If you still want to join, apply again.")
	default:
		return
	}
	b.WriteString(m.signature(ctx, rd.CharID, readerHat(c, rd), corpName))
	m.mailPilot(ctx, a.CharID, rd.CharID, subject, b.String())
}

// stampHumanOffer starts an offer's clock. INSERT IGNORE: a second accept of
// the same row must not restart it.
func (m *Mod) stampHumanOffer(ctx context.Context, corpID, charID int64) {
	if _, err := m.db.ExecContext(ctx, `
		INSERT IGNORE INTO rolebot_human_offers (corporationID, characterID, offeredAt)
		VALUES (?, ?, ?)`, corpID, charID, clock.Unix()); err != nil {
		rlogf("rolebots: stamp human offer %d→%d: %v", charID, corpID, err)
	}
}

// humanApplicantsPending reports whether a human has an unanswered
// application at this corporation. The staff desk uses it to answer players
// on its fifteen-minute cadence even when no bot holds the Recruitment post.
func (m *Mod) humanApplicantsPending(ctx context.Context, corpID int64) bool {
	ids, err := int64Column(ctx, m, `
		SELECT a.characterID
		  FROM chrApplications a
		  LEFT JOIN rolebot_characters r ON r.characterID = a.characterID
		 WHERE a.corporationID = ? AND a.deleted = 0 AND a.status = ? AND r.characterID IS NULL`,
		corpID, evedb.AppAppliedByCharacter)
	if err != nil {
		return false
	}
	for _, id := range ids {
		if !m.isSynthetic(id) {
			return true
		}
	}
	return false
}

// humanOffer is one outstanding offer to a human, as the sweep reads it.
type humanOffer struct {
	CharID    int64
	Name      string
	CurCorpID int64
	OfferedAt int64
	Text      string
}

// offerLapsed is the pure rule: an offer with no stamp is not lapsed (its
// clock starts now), anything older than ttl is.
func offerLapsed(offeredAt, now, ttl int64) bool {
	return offeredAt > 0 && now-offeredAt >= ttl
}

// humanOfferSweep lapses a human's unanswered offers and tidies the clock
// rows of offers that ended some other way.
func (m *Mod) humanOfferSweep(ctx context.Context, c *corpCEO) error {
	if _, err := m.db.ExecContext(ctx, `
		DELETE o FROM rolebot_human_offers o
		  LEFT JOIN chrApplications a
		    ON a.corporationID = o.corporationID AND a.characterID = o.characterID
		 WHERE o.corporationID = ?
		   AND (a.characterID IS NULL OR a.deleted = 1 OR a.status <> ?)`,
		c.CorpID, evedb.AppAcceptedByCorporation); err != nil && !isMissingTable(err) {
		return fmt.Errorf("humanOfferSweep tidy: %w", err)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT a.characterID, COALESCE(ch.characterName, ''), COALESCE(ch.corporationID, 0),
		       COALESCE(o.offeredAt, 0), COALESCE(a.applicationText, '')
		  FROM chrApplications a
		  JOIN character_ ch ON ch.characterID = a.characterID
		  LEFT JOIN rolebot_human_offers o
		    ON o.corporationID = a.corporationID AND o.characterID = a.characterID
		 WHERE a.corporationID = ? AND a.deleted = 0 AND a.status = ?`,
		c.CorpID, evedb.AppAcceptedByCorporation)
	if err != nil {
		if isMissingTable(err) {
			return nil
		}
		return fmt.Errorf("humanOfferSweep read: %w", err)
	}
	var offers []humanOffer
	for rows.Next() {
		var o humanOffer
		if err := rows.Scan(&o.CharID, &o.Name, &o.CurCorpID, &o.OfferedAt, &o.Text); err != nil {
			rows.Close()
			return fmt.Errorf("humanOfferSweep scan: %w", err)
		}
		offers = append(offers, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	now := clock.Unix()
	ttl := humanOfferSec()
	for _, o := range offers {
		if m.isSynthetic(o.CharID) || o.CurCorpID == c.CorpID {
			continue // a bot's offer is corpapply.go's; a member has joined
		}
		if o.OfferedAt == 0 {
			// An offer made before this clock existed, or by a human in the
			// corp window: it starts now.
			m.stampHumanOffer(ctx, c.CorpID, o.CharID)
			continue
		}
		if !offerLapsed(o.OfferedAt, now, ttl) {
			continue
		}
		if err := evedb.UpdateApplication(ctx, m.db, o.CharID, c.CorpID, c.CharID,
			evedb.AppRejectedByCorporation, o.Text); err != nil {
			return fmt.Errorf("humanOfferSweep lapse: %w", err)
		}
		_ = evedb.SoftDeleteApplication(ctx, m.db, o.CharID, c.CorpID)
		if h := modapi.Corp().ApplicationChanged; h != nil {
			h(c.CorpID, o.CharID, evedb.AppAcceptedByCorporation, evedb.AppRejectedByCorporation, c.CharID)
		}
		_, _ = m.db.ExecContext(ctx,
			`DELETE FROM rolebot_human_offers WHERE corporationID = ? AND characterID = ?`, c.CorpID, o.CharID)
		m.ceoLog(ctx, c, powerHR, "lapse", o.CharID, 0, "%s",
			fmt.Sprintf("withdrew the offer to %s — unanswered for %s", o.Name, fmtDuration(now-o.OfferedAt)))
		m.mailApplicant(ctx, c, hrReader{CharID: c.CharID}, hrApplicant{CharID: o.CharID, Name: o.Name},
			applicantLapsed, "")
	}
	return nil
}

// welcomeDue is the pure rule for one human member: is a welcome letter
// owed (mail), or should the member only be stamped as seen (stamp)?
// joinedAt and welcomedAt are unix seconds; welcomedAt 0 means never seen.
func welcomeDue(joinedAt, welcomedAt, now int64) (mail, stamp bool) {
	if welcomedAt > 0 && joinedAt <= welcomedAt {
		return false, false // seen since this membership began
	}
	if joinedAt > 0 && now-joinedAt > welcomeQuietSec {
		return false, true // a veteran the pipeline is meeting late
	}
	return true, true
}

// welcomeSweep finds human members the pipeline has not yet greeted.
func (m *Mod) welcomeSweep(ctx context.Context, c *corpCEO) error {
	members, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return err
	}
	seen := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, welcomedAt FROM rolebot_corp_merit WHERE corporationID = ?`, c.CorpID)
	if err != nil {
		return fmt.Errorf("welcomeSweep read: %w", err)
	}
	for rows.Next() {
		var id, at int64
		if err := rows.Scan(&id, &at); err == nil {
			seen[id] = at
		}
	}
	rows.Close()

	now := clock.Unix()
	var corpName string
	for _, cm := range members {
		if !cm.Human() {
			continue
		}
		joined := int64(0)
		if cm.Since > 0 {
			joined = evedb.Win32ToUnix(cm.Since)
		}
		mail, stamp := welcomeDue(joined, seen[cm.CharID], now)
		if !stamp {
			continue
		}
		// A rejoin keeps its points (the record is the corporation's memory
		// of the member) but not its rank roles: the join reset them.
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_merit
			  (corporationID, characterID, rankSince, lastActiveAt, welcomedAt)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE welcomedAt = VALUES(welcomedAt), rankRoleMask = 0,
			  rankSince = VALUES(rankSince), lastActiveAt = VALUES(lastActiveAt)`,
			c.CorpID, cm.CharID, now, now, now); err != nil {
			return fmt.Errorf("welcomeSweep stamp: %w", err)
		}
		if !mail {
			continue
		}
		if corpName == "" {
			corpName = m.corpNameOf(ctx, c.CorpID)
		}
		m.mailWelcome(ctx, c, cm, corpName)
		m.queueCorpNews(ctx, c.CorpID, newsMembers, fmt.Sprintf("%s joined the corporation.", cm.Name), cm.CharID)
		requestSeatPass(c.CorpID)
		m.ceoLog(ctx, c, powerHR, "welcome", cm.CharID, 0, "%s",
			fmt.Sprintf("welcomed %s aboard", cm.Name))
	}
	return nil
}

// mailWelcome is the new member's first letter from the chair.
func (m *Mod) mailWelcome(ctx context.Context, c *corpCEO, cm corpMember, corpName string) {
	var b strings.Builder
	line(&b, "%s,", cm.Name)
	b.WriteString("<br>")
	line(&b, "Welcome to %s.", corpName)
	if f, err := getAnyCorpFacts(ctx, m.db, c.CorpID); err == nil && f != nil && len(f.Policy.Tags) > 0 {
		line(&b, "We are a %s outfit.", strings.Join(f.Policy.Tags, ", "))
	}
	if d, _, err := m.loadCorpDirective(ctx, c.CorpID); err == nil && len(d) > 0 {
		line(&b, "What I want from the corporation right now: %s.", directiveText(d))
	}

	heading(&b, "How you rise here")
	line(&b, "Every member starts as %s. Rank is earned, not given: work for the corporation —",
		meritRankName(0))
	line(&b, "building on the work board, selling ore into our buy orders, killing on our patrols,")
	line(&b, "taking the assignments we send you — and time served, both count.")
	line(&b, "Each rank opens more of the corporation's hangars and roles, and pays better.")
	if !meritOn() {
		line(&b, "(Ranks are not being awarded yet. Your work is still on the books.)")
	}

	joinable := m.corpJoinableArms(ctx, c)
	if len(joinable) > 0 {
		heading(&b, "Choose your department")
		line(&b, "Every member works in one of our departments, and the choice is yours. Pick the work you want;")
		line(&b, "you will be posted to that department's office, and its roles are yours while you stay in it.")
		writeArmChoices(&b, c.CorpID, cm.CharID, "", joinable)
	}

	heading(&b, "Where to look")
	line(&b, "Corporation news comes to your Corp mail tab. Orders and pay come to you personally.")
	line(&b, "%s — the directive, the officers and the fleet orders.",
		igbLink("/corp/bulletins", "Corporation bulletins"))
	line(&b, "%s — your rank, your assignments and what you have earned.",
		igbLink(deskViewPath(c.CorpID, cm.CharID), "Your desk"))
	line(&b, "%s — what the factory needs built, and what it pays.",
		igbLink("/corp/industry", "Industry"))
	from, hat := m.letterFrom(ctx, c, mailFromPersonnel)
	b.WriteString(m.signature(ctx, from, hat, corpName))
	m.mailPilot(ctx, cm.CharID, from, "Welcome to "+corpName, b.String())
	if len(joinable) > 0 {
		m.stampArmInvite(ctx, c.CorpID, cm.CharID)
	}
}

// humanDesk is the pipeline's turn on the staff desk sweep.
func (m *Mod) humanDesk(ctx context.Context, c *corpCEO) {
	if !humanPipelineOn() {
		return
	}
	if err := m.humanOfferSweep(ctx, c); err != nil {
		m.logBot(c.CharID, logError, "ceo", "human offers: %v", err)
	}
	if err := m.welcomeSweep(ctx, c); err != nil {
		m.logBot(c.CharID, logError, "ceo", "welcome: %v", err)
	}
	if err := m.armInviteSweep(ctx, c); err != nil {
		m.logBot(c.CharID, logError, "ceo", "arm invites: %v", err)
	}
}
