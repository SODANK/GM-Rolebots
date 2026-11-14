package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	evedb "gurimoon/db"
)

// A human member chooses an arm by mail. The deal never places a human (dealArms skips them), so
// the corporation offers its arms: one letter listing every joinable arm (joinableArms) with a
// signed desk link (armPath), sent with the welcome letter or once by armInviteSweep.
//
// The click (memberChoosesArm) rechecks membership and the pick, writes the row SetBy human
// (which ceoArms leaves alone) and applies the bundle through applyArmRoles: query and Factory
// Manager at most, never Take. The member's posting is then handed back to the chair so the next
// posting pass posts them to the arm's office; a Base a director set by Edit Member stands.
// All of it sits behind settingHumanPipeline. A bot never takes this path.

// ensureArmInviteTable is called from ensureArmTables. One row per member: when the
// corporation last wrote them its arms. A rejoin (joinedAt after invitedAt) is written again.
const armInviteTableSQL = `
	CREATE TABLE IF NOT EXISTS rolebot_corp_arm_invites (
		characterID   BIGINT NOT NULL,
		corporationID BIGINT NOT NULL,
		invitedAt     BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (characterID)
	)`

// joinableArms is the arms a member may choose: the justified line arms, in lineArms order.
// Pure. HR and Finance are officer-staffed and never offered.
func joinableArms(justified []corpArm) []corpArm {
	ok := map[corpArm]bool{}
	for _, a := range justified {
		ok[a] = true
	}
	var out []corpArm
	for _, a := range lineArms {
		if ok[a] {
			out = append(out, a)
		}
	}
	return out
}

// armInviteDue is the pure rule for writing a member the arms letter: there must be a choice,
// the member must hold no arm, and must not have been written during this membership.
func armInviteDue(joinedAt, invitedAt int64, placed bool, joinable int) bool {
	if placed || joinable == 0 {
		return false
	}
	return invitedAt == 0 || joinedAt > invitedAt
}

// armChoiceRefusal is why a human's pick cannot stand, "" when it can. Pure; the reader
// gathers the facts.
func armChoiceRefusal(pipelineOn, member, human bool, arm corpArm, joinable []corpArm, pinned corpArm) string {
	switch {
	case !pipelineOn:
		return "the corporation is not taking department choices at the moment"
	case !member:
		return "you are no longer a member"
	case !human:
		return "that member is not yours to place"
	}
	found := false
	for _, a := range joinable {
		if a == arm {
			found = true
		}
	}
	if !found {
		return "the corporation keeps no " + armLabel(arm) + " department you can join today"
	}
	if pinned != "" && pinned != arm {
		return "your post already places you in " + armLabel(pinned)
	}
	return ""
}

// postingReopen is what a member's arm choice does to their posting row. Pure. A row from
// another corporation, or none, is written fresh as the chair's (insert); the member's own pick
// is handed back to the chair (reset); the chair's posting already sees the arm and a
// director's Edit Member stands.
func postingReopen(rowCorpID, corpID int64, setBy string) (insert, reset bool) {
	if rowCorpID != corpID {
		return true, false
	}
	return false, setBy == postSetMember
}

// armPath is the signed desk link that puts a member in an arm.
func armPath(corpID, charID int64, arm corpArm) string {
	t := signDesk(deskKeyAny(), "arm", corpID, charID, string(arm))
	return fmt.Sprintf("/ig/corpdesk?corp=%d&c=%d&arm=%s&t=%s", corpID, charID, arm, t)
}

// armPitch is one line about an arm for a letter or the desk: what its people do, where they
// are posted, and what it opens.
func armPitch(a corpArm) string {
	work := map[corpArm]string{
		armIndustry:       "builds on the corporation's work board",
		armResearch:       "researches the corporation's blueprints",
		armMining:         "mines and refines for the corporation",
		armSecurity:       "flies the corporation's patrols and fleets",
		armSales:          "trades and hauls for the corporation",
		armInfrastructure: "keeps the corporation's starbases fuelled, armed and standing",
	}[a]
	s := work + ", from our " + officePurposeLabel(armPurpose(a))
	if words := armRoleWords(armRoles(a, armTierMember, false)); len(words) > 0 {
		s += "; members may " + strings.Join(words, ", ")
	}
	return s
}

// corpJoinableArms is the arms a member may choose now: the last pass's justification, or a
// fresh reading when no pass has run since the server started.
func (m *Mod) corpJoinableArms(ctx context.Context, c *corpCEO) []corpArm {
	if memo, ok := armMemoOf(c.CorpID); ok {
		return joinableArms(memo.Justified)
	}
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil || facts == nil {
		return nil
	}
	return joinableArms(justifiedArms(m.gatherArmSignals(ctx, c, facts)))
}

// writeArmChoices writes the arms section of a letter or the desk: the member's current arm
// and a link for each other arm they may join. It answers false when there is nothing to choose.
func writeArmChoices(b *strings.Builder, corpID, charID int64, cur corpArm, joinable []corpArm) bool {
	var offer []corpArm
	for _, a := range joinable {
		if a != cur {
			offer = append(offer, a)
		}
	}
	if len(offer) == 0 {
		return false
	}
	for _, a := range offer {
		line(b, "%s — %s.", igbLink(armPath(corpID, charID, a), armLabel(a)), armPitch(a))
	}
	return true
}

// armLetterFrom is who writes about arms: the Director of Personnel when the corporation has
// one, else the CEO.
func (m *Mod) armLetterFrom(ctx context.Context, c *corpCEO) (int64, string) {
	if s := m.staffFor(ctx, c); s != nil {
		if h := s.head(portHR); h != nil {
			return h.CharID, armHeadTitle(armHR)
		}
	}
	return c.CharID, "Chief Executive Officer"
}

// stampArmInvite records that a member has been written the arms.
func (m *Mod) stampArmInvite(ctx context.Context, corpID, charID int64) {
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_arm_invites (characterID, corporationID, invitedAt) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), invitedAt = VALUES(invitedAt)`,
		charID, corpID, clock.Unix()); err != nil {
		rlogf("rolebots: stamp arm invite %d: %v", charID, err)
	}
}

// mailArmInvite writes a human member the corporation's arms.
func (m *Mod) mailArmInvite(ctx context.Context, c *corpCEO, cm corpMember, corpName string, joinable []corpArm) {
	from, hat := m.armLetterFrom(ctx, c)
	var b strings.Builder
	line(&b, "%s,", cm.Name)
	b.WriteString("<br>")
	line(&b, "Every member of %s works in one of its departments. You are in none yet, and the choice is yours.", corpName)
	line(&b, "Pick the work you want; you will be posted to that department's office, and its roles are yours while you stay in it.")
	b.WriteString("<br>")
	writeArmChoices(&b, c.CorpID, cm.CharID, "", joinable)
	b.WriteString("<br>")
	line(&b, "You can change your mind at any time from %s.", igbLink(deskViewPath(c.CorpID, cm.CharID), "your desk"))
	b.WriteString(m.signature(ctx, from, hat, corpName))
	m.mailPilot(ctx, cm.CharID, from, "Choose your department — "+corpName, b.String())
	m.stampArmInvite(ctx, c.CorpID, cm.CharID)
}

// armInviteSweep writes the arms to every human member who holds none and has not been
// written them during this membership.
func (m *Mod) armInviteSweep(ctx context.Context, c *corpCEO) error {
	members, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return err
	}
	var humans []corpMember
	for _, cm := range members {
		if cm.Human() {
			humans = append(humans, cm)
		}
	}
	if len(humans) == 0 {
		return nil
	}
	joinable := m.corpJoinableArms(ctx, c)
	if len(joinable) == 0 {
		return nil
	}
	invited := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, invitedAt FROM rolebot_corp_arm_invites WHERE corporationID = ?`, c.CorpID)
	if err != nil {
		return fmt.Errorf("armInviteSweep read: %w", err)
	}
	for rows.Next() {
		var id, at int64
		if rows.Scan(&id, &at) == nil {
			invited[id] = at
		}
	}
	rows.Close()
	var corpName string
	for _, cm := range humans {
		joined := int64(0)
		if cm.Since > 0 {
			joined = evedb.Win32ToUnix(cm.Since)
		}
		_, placed := armOf(cm.CharID, c.CorpID)
		if !armInviteDue(joined, invited[cm.CharID], placed, len(joinable)) {
			continue
		}
		if corpName == "" {
			corpName = m.corpNameOf(ctx, c.CorpID)
		}
		m.mailArmInvite(ctx, c, cm, corpName, joinable)
		m.ceoLog(ctx, c, powerHR, "arminvite", cm.CharID, 0, "%s",
			fmt.Sprintf("wrote %s the corporation's departments", cm.Name))
	}
	return nil
}

// armChoiceCheck gathers the facts armChoiceRefusal rules on and rules. Shared by the click
// (memberChoosesArm) and the portal's Join, which mails the click only once the pick would
// stand.
func (m *Mod) armChoiceCheck(ctx context.Context, corpID, charID int64, arm corpArm) (*corpCEO, corpMember, error) {
	c, _ := getCEO(ctx, m.db, corpID)
	if c == nil {
		return nil, corpMember{}, errors.New("this corporation keeps no departments")
	}
	members, err := m.corpMembers(ctx, corpID, 0)
	if err != nil {
		return nil, corpMember{}, err
	}
	var cm corpMember
	member := false
	for _, x := range members {
		if x.CharID == charID {
			cm, member = x, true
		}
	}
	human := member && cm.Human() && !m.isSynthetic(charID)
	pinned := corpArm("")
	if member && human {
		if p, ok := m.armPositions(ctx, corpID)[charID]; ok && p.Tier > armTierMember {
			pinned = p.Arm
		}
	}
	if why := armChoiceRefusal(humanPipelineOn(), member, human, arm, m.corpJoinableArms(ctx, c), pinned); why != "" {
		return nil, corpMember{}, errors.New(why)
	}
	return c, cm, nil
}

// memberChoosesArm applies a human member's pick from a letter or the desk. It answers the
// arm's label.
func (m *Mod) memberChoosesArm(ctx context.Context, corpID, charID int64, arm corpArm) (string, error) {
	c, cm, err := m.armChoiceCheck(ctx, corpID, charID, arm)
	if err != nil {
		return "", err
	}

	rows, err := m.loadArmRows(ctx, corpID)
	if err != nil {
		return "", err
	}
	prev, had := rows[charID]
	if had && prev.Arm == arm && prev.SetBy == postSetHuman {
		return armLabel(arm), nil
	}
	next := armPlacement{Arm: arm, Tier: armTierMember}
	grant, err := m.applyArmRoles(ctx, cm, prev.GrantMask, next, m.benchedBuilders(ctx, corpID)[charID])
	if err != nil {
		return "", err
	}
	row := armRow{CharID: charID, CorpID: corpID, Arm: arm, Tier: armTierMember, Since: prev.Since,
		AssignedBy: charID, GrantMask: grant, SetBy: postSetHuman}
	if !had || prev.Arm != arm {
		row.Since = clock.Unix()
	}
	if err := m.saveArmRow(ctx, row); err != nil {
		return "", err
	}
	m.refreshArmIndex(ctx)
	m.stampArmInvite(ctx, corpID, charID)
	if err := m.reopenPostingForArm(ctx, corpID, charID); err != nil {
		rlogf("rolebots: reopen posting %d: %v", charID, err)
	}
	requestSeatPass(corpID)
	if had && prev.Arm != "" && prev.Arm != arm {
		m.ceoLog(ctx, c, powerRoles, "arm", charID, 0, "%s moved from %s to %s at their own request",
			cm.Name, armLabel(prev.Arm), armLabel(arm))
	} else {
		m.ceoLog(ctx, c, powerRoles, "arm", charID, 0, "%s joined %s at their own request", cm.Name, armLabel(arm))
	}
	return armLabel(arm), nil
}

// reopenPostingForArm hands a member's posting back to the chair after an arm choice
// (postingReopen), so the next posting pass posts them for it.
func (m *Mod) reopenPostingForArm(ctx context.Context, corpID, charID int64) error {
	var rowCorp int64
	var setBy string
	err := m.db.QueryRowContext(ctx,
		`SELECT corporationID, setBy FROM rolebot_corp_postings WHERE characterID = ?`, charID).Scan(&rowCorp, &setBy)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	insert, reset := postingReopen(rowCorp, corpID, setBy)
	switch {
	case insert:
		var base int64
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(baseID, 0) FROM chrCorporationRoles WHERE characterID = ?`, charID).Scan(&base)
		_, err = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_postings (characterID, corporationID, stationID, setBy, arm, postedAt)
			VALUES (?, ?, ?, ?, '', 0)
			ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), stationID = VALUES(stationID),
				setBy = VALUES(setBy), arm = '', postedAt = 0`,
			charID, corpID, base, postSetCEO)
	case reset:
		_, err = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_postings SET setBy = ?, arm = ''
			 WHERE characterID = ? AND corporationID = ?`, postSetCEO, charID, corpID)
	}
	return err
}
