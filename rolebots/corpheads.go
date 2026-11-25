package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// Division heads: who a human CEO put in charge of what (Management, Appointments).
//
// One head per division and one division per head, and both bots and people serve. The Director
// role is the qualification: the CEO grants it in the corporation window, and it is never granted
// here. rolebot_corp_heads is the appointment; what the rest of the module reads is kept in step
// with it, one row at a time: a portfolio in rolebot_corp_posts for a Director-headed department,
// rolebot_corp_policy for Industry, the arm row at head tier with its role bundle for all. A bot CEO
// deals its own heads (ceoRoles), so the rows are cleared when one takes the chair.
//
// The other way round nothing is cleared: a bot that a bot CEO put in charge of a division keeps it
// when a person takes the chair (inheritHeads), until the CEO names somebody else or stands it down.
// Such a row has setBy 0.

// headFlag* are what a bot head may do beyond its division's orders. Off in an appointment until the
// CEO ticks them, because each touches people or goods in a way an order does not. A head kept from a
// bot CEO's day was already doing it and goes on (inheritHeads).
const (
	headFlagListStock = 1 << iota // Sales: list what is in the Trade and Production hangars
	headFlagApplicant             // HR: answer applications
	headFlagStipend               // Finance: pay the Budget order's stipends
)

// headFlagOf is the option a division's head has, 0 for none. Pure.
func headFlagOf(a corpArm) (flag int, label string) {
	switch a {
	case armSales:
		return headFlagListStock, "may list goods from the corporation's hangars on the market"
	case armHR:
		return headFlagApplicant, "may accept and refuse applications"
	case armFinance:
		return headFlagStipend, "may pay the stipends the budget sets"
	}
	return 0, ""
}

// corpHead is one appointment.
type corpHead struct {
	CorpID   int64
	Arm      corpArm
	CharID   int64
	DeputyID int64
	SetBy    int64
	Since    int64
	Flags    int
}

// Inherited reports whether the head was kept from a bot CEO's day, not appointed by the chair. Pure.
func (h corpHead) Inherited() bool { return h.SetBy == 0 }

func ensureHeadTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rolebot_corp_heads (
			corporationID BIGINT NOT NULL,
			arm           VARCHAR(16) NOT NULL,
			characterID   BIGINT NOT NULL,
			deputyID      BIGINT NOT NULL DEFAULT 0,
			setBy         BIGINT NOT NULL DEFAULT 0,
			since         BIGINT NOT NULL DEFAULT 0,
			flags         INT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, arm),
			UNIQUE KEY uq_rbhead_char (corporationID, characterID)
		)`)
	if err != nil {
		return fmt.Errorf("ensureHeadTables: %w", err)
	}
	return nil
}

// corpHeads is a corporation's appointments by division. Empty when none.
func (m *Mod) corpHeads(ctx context.Context, corpID int64) map[corpArm]corpHead {
	out, _ := m.corpHeadsErr(ctx, corpID)
	return out
}

func (m *Mod) corpHeadsErr(ctx context.Context, corpID int64) (map[corpArm]corpHead, error) {
	out := map[corpArm]corpHead{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, arm, characterID, deputyID, setBy, since, flags
		  FROM rolebot_corp_heads WHERE corporationID = ?`, corpID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var h corpHead
		var arm string
		if rows.Scan(&h.CorpID, &arm, &h.CharID, &h.DeputyID, &h.SetBy, &h.Since, &h.Flags) == nil {
			h.Arm = corpArm(arm)
			out[h.Arm] = h
		}
	}
	return out, rows.Err()
}

// validArm reports whether a slug names a division. Pure.
func validArm(a corpArm) bool {
	for _, x := range armOrder {
		if x == a {
			return true
		}
	}
	return false
}

var (
	errHeadNotMember   = errors.New("that pilot is not a member of the corporation")
	errHeadNotDirector = errors.New("that pilot does not hold the Director role")
	errHeadIsCEO       = errors.New("the CEO already commands every division")
	errHeadBotCEO      = errors.New("a bot CEO appoints its own division heads")
	errHeadNoArm       = errors.New("that is not a division")
)

// headRefusal is why a pilot may not head a division, nil when it may. Pure.
func headRefusal(r memberRank, arm corpArm, botCEO bool) error {
	switch {
	case botCEO:
		return errHeadBotCEO
	case !validArm(arm):
		return errHeadNoArm
	case !r.Member:
		return errHeadNotMember
	case r.CEO:
		return errHeadIsCEO
	case !r.Director:
		return errHeadNotDirector
	}
	return nil
}

// inheritRefusal is why a bot CEO's head may not stay one under a person, nil when it may. Pure. The
// post it was dealt is its qualification: Industry is run by the Industry Director and Finance by the
// Treasurer, and neither needs the Director role (armOfficerQuota).
func inheritRefusal(r memberRank, arm corpArm, industryDirector bool) error {
	err := headRefusal(r, arm, false)
	if err != errHeadNotDirector {
		return err
	}
	if (arm == armIndustry && industryDirector) || (arm == armFinance && r.Accountant) {
		return nil
	}
	return err
}

// standsRefusal is why an appointment no longer holds, nil when it does.
func (m *Mod) standsRefusal(ctx context.Context, h corpHead, r memberRank) error {
	if !h.Inherited() {
		return headRefusal(r, h.Arm, false)
	}
	ind := false
	if h.Arm == armIndustry && !r.Director {
		p, err := m.loadCorpPolicy(ctx, h.CorpID)
		ind = err != nil || p.DirectorID == h.CharID // the database could not say: it stands
	}
	return inheritRefusal(r, h.Arm, ind)
}

// headsToInherit is the bots a bot CEO left at the head of a division nobody has been appointed to,
// by division. Pure. A row the chair has written is the chair's word and not a head to keep, unless
// the post says the bot CEO dealt it one afterwards: dismissHead clears the post, so a head the CEO
// stood down never comes back. Of several, the longest in the division.
func headsToInherit(cur map[corpArm]corpHead, rows map[int64]armRow, portfolio func(int64) string, industryDirector int64,
	bot func(int64) bool) map[corpArm]int64 {

	heads := map[int64]bool{}
	for _, h := range cur {
		heads[h.CharID] = true
	}
	out := map[corpArm]int64{}
	for id, r := range rows {
		if _, taken := cur[r.Arm]; taken || heads[id] || r.Tier != armTierHead || !validArm(r.Arm) || !bot(id) {
			continue
		}
		if r.SetBy == postSetHuman {
			port := portfolioOfArm(r.Arm)
			dealt := (port != "" && portfolio(id) == port) || (r.Arm == armIndustry && id == industryDirector)
			if !dealt {
				continue
			}
		}
		if best, ok := out[r.Arm]; ok {
			b := rows[best]
			if b.Since < r.Since || (b.Since == r.Since && best < id) {
				continue
			}
		}
		out[r.Arm] = id
	}
	return out
}

// inheritHeads keeps a bot CEO's bot heads at their desks under a person: each becomes an
// appointment, options and all, which the CEO changes or ends like any other. Since is now, so what
// the old chair's desks had posted is cleared off the boards (chairLeftovers) and the heads post
// their own. Run by the human chair's pass.
func (m *Mod) inheritHeads(ctx context.Context, corpID int64) {
	cur, err := m.corpHeadsErr(ctx, corpID)
	if err != nil || len(cur) == len(armOrder) {
		return
	}
	rows, err := m.loadArmRows(ctx, corpID)
	if err != nil {
		return
	}
	ceoID := m.corpCEOID(ctx, corpID)
	members, err := m.corpMembers(ctx, corpID, ceoID)
	if err != nil {
		return
	}
	flies := map[int64]bool{}
	for _, cm := range members {
		flies[cm.CharID] = !cm.Human() && cm.Enabled && m.botFlown(cm.CharID)
	}
	policy, err := m.loadCorpPolicy(ctx, corpID)
	if err != nil {
		return
	}
	posts := m.loadStaffRows(ctx, corpID)
	found := headsToInherit(cur, rows, func(id int64) string { return posts[id].Portfolio }, policy.DirectorID,
		func(id int64) bool { return flies[id] })
	var kept []string
	for _, a := range armOrder {
		id, ok := found[a]
		if !ok || ctx.Err() != nil {
			continue
		}
		rank, err := m.memberRankErr(ctx, corpID, id)
		if err != nil || inheritRefusal(rank, a, id == policy.DirectorID) != nil {
			continue
		}
		h := corpHead{CorpID: corpID, Arm: a, CharID: id}
		h.Flags, _ = headFlagOf(a)
		if a == armIndustry && policy.DeputyID != id && flies[policy.DeputyID] {
			h.DeputyID = policy.DeputyID
		}
		if err := m.installHead(ctx, h); err != nil {
			rlogf("rolebots: keep head of %s in corp %d: %v", a, corpID, err)
			continue
		}
		name := m.charNameOr(ctx, id)
		m.humanLog(ctx, corpID, 0, "head", id, 0, "%s goes on heading %s under the new CEO", name, armLabel(a))
		m.logBot(id, logInfo, "corp", "goes on heading %s under the new CEO", armLabel(a))
		line := "<b>" + esp.IGBText(name) + "</b> heads " + esp.IGBText(armLabel(a)) + " and " + armDuty(a)
		if _, label := headFlagOf(a); label != "" {
			line += "; it " + label
		}
		kept = append(kept, line+".")
	}
	if len(kept) == 0 || !m.seatedByPerson(ctx, ceoID) {
		return
	}
	m.mailPilot(ctx, ceoID, corpID, "Your division heads carry on",
		"These pilots headed their divisions before you took the chair and go on doing so:<br><br>"+strings.Join(kept, "<br>")+
			"<br><br>Your own orders stand over theirs. Change a head, or stand one down, on the "+
			igbLink(managePath, "Management page")+" of the Corporate Portal.")
}

// headsDisplaced is which appointments a new one ends: whoever heads the division now, and the
// pilot's own command of another. Pure.
func headsDisplaced(cur map[corpArm]corpHead, h corpHead) []corpHead {
	var out []corpHead
	for _, a := range armOrder {
		c, ok := cur[a]
		if !ok {
			continue
		}
		if (a == h.Arm) != (c.CharID == h.CharID) {
			out = append(out, c)
		}
	}
	return out
}

// appointHead puts a Director in charge of a division: the division's old head stands down, and the
// pilot leaves whatever division it headed before.
func (m *Mod) appointHead(ctx context.Context, h corpHead) error {
	_, bot := managedCEO(h.CorpID)
	if err := headRefusal(m.memberRankIn(ctx, h.CorpID, h.CharID), h.Arm, bot); err != nil {
		return err
	}
	if h.DeputyID != 0 && (h.DeputyID == h.CharID || m.charCorpID(ctx, h.DeputyID) != h.CorpID) {
		return errHeadNotMember
	}
	return m.installHead(ctx, h)
}

// installHead writes an appointment that has been found good.
func (m *Mod) installHead(ctx context.Context, h corpHead) error {
	for _, old := range headsDisplaced(m.corpHeads(ctx, h.CorpID), h) {
		if err := m.dismissHead(ctx, old); err != nil {
			return err
		}
	}
	if h.Since == 0 {
		h.Since = clock.Unix()
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_heads (corporationID, arm, characterID, deputyID, setBy, since, flags)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE characterID = VALUES(characterID), deputyID = VALUES(deputyID),
			setBy = VALUES(setBy), since = VALUES(since), flags = VALUES(flags)`,
		h.CorpID, string(h.Arm), h.CharID, h.DeputyID, h.SetBy, h.Since, h.Flags); err != nil {
		return fmt.Errorf("appointHead: %w", err)
	}
	if err := m.mirrorHead(ctx, h); err != nil {
		return err
	}
	m.headsChanged(ctx, h.CorpID)
	return nil
}

// mirrorHead writes an appointment where the module's readers look for it. Single rows only:
// savePosts replaces a whole family.
func (m *Mod) mirrorHead(ctx context.Context, h corpHead) error {
	if port := portfolioOfArm(h.Arm); port != "" {
		// Nobody else runs the department, a row a bot CEO left behind included.
		if _, err := m.db.ExecContext(ctx, `
			DELETE FROM rolebot_corp_posts
			 WHERE corporationID = ? AND post = ? AND (portfolio = ? OR characterID = ?)`,
			h.CorpID, postDirector, port, h.CharID); err != nil {
			return fmt.Errorf("mirrorHead clear: %w", err)
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_posts (`+corpPostCols+`) VALUES (?, ?, ?, ?, ?, 1, 0, 0, 0)
			ON DUPLICATE KEY UPDATE portfolio = VALUES(portfolio), since = VALUES(since)`,
			h.CorpID, h.CharID, postDirector, port, h.Since); err != nil {
			return fmt.Errorf("mirrorHead post: %w", err)
		}
	}
	if h.Arm == armIndustry {
		p, err := m.loadCorpPolicy(ctx, h.CorpID)
		if err != nil {
			return err
		}
		if p.DirectorID != h.CharID {
			p.DirectorID, p.DirectorSince = h.CharID, h.Since
		}
		p.DeputyID = h.DeputyID
		if p.DeputyID == 0 && m.botFlown(h.CharID) {
			// The chores a deputy flies (print purchases) want a bot; a bot
			// head is its own.
			p.DeputyID = h.CharID
		}
		if err := m.saveCorpPolicyRow(ctx, p); err != nil {
			return err
		}
	}
	return m.chairPlaceMember(ctx, h.CorpID, h.CharID, h.Arm, armTierHead, h.SetBy)
}

// dismissHead stands a head down: the appointment, its mirrors and the roles they brought. The pilot
// stays in the division as a member.
func (m *Mod) dismissHead(ctx context.Context, h corpHead) error {
	if _, err := m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_heads WHERE corporationID = ? AND arm = ? AND characterID = ?`,
		h.CorpID, string(h.Arm), h.CharID); err != nil {
		return fmt.Errorf("dismissHead: %w", err)
	}
	if port := portfolioOfArm(h.Arm); port != "" {
		if _, err := m.db.ExecContext(ctx, `
			DELETE FROM rolebot_corp_posts
			 WHERE corporationID = ? AND characterID = ? AND post = ? AND portfolio = ?`,
			h.CorpID, h.CharID, postDirector, port); err != nil {
			return fmt.Errorf("dismissHead post: %w", err)
		}
	}
	if h.Arm == armIndustry {
		p, err := m.loadCorpPolicy(ctx, h.CorpID)
		if err != nil {
			return err
		}
		if p.DirectorID == h.CharID {
			p.DirectorID, p.DirectorSince = 0, 0
		}
		if p.DeputyID == h.CharID || (h.DeputyID != 0 && p.DeputyID == h.DeputyID) {
			p.DeputyID = 0
		}
		if err := m.saveCorpPolicyRow(ctx, p); err != nil {
			return err
		}
	}
	// A Sales head's own desk's orders go back with it, as a relieved commerce head's do
	// (ceoPortfolios). The chair's bids stay in its name, whoever else now heads Sales. isSynthetic, not
	// botFlown: a pilot its person has taken over may still hold the desk orders it placed as a bot.
	if h.Arm == armSales && m.isSynthetic(h.CharID) {
		m.cancelDeskOrdersOf(ctx, h.CorpID, h.CharID)
	}
	if m.charCorpID(ctx, h.CharID) == h.CorpID {
		if err := m.chairPlaceMember(ctx, h.CorpID, h.CharID, h.Arm, armTierMember, h.SetBy); err != nil {
			return err
		}
	} else if s, ok := armOf(h.CharID, h.CorpID); ok && s.CorpID == h.CorpID {
		m.deleteArmRow(ctx, h.CharID)
	}
	m.headsChanged(ctx, h.CorpID)
	return nil
}

// headsChanged rebuilds what reads the heads out of memory.
func (m *Mod) headsChanged(ctx context.Context, corpID int64) {
	forgetChairHand(corpID)
	m.refreshPostIndex(ctx)
	m.refreshArmIndex(ctx)
}

// pruneHeads stands down every head that left the corporation or lost what qualified it. Run by the
// human chair's pass.
func (m *Mod) pruneHeads(ctx context.Context, corpID int64) {
	for _, h := range m.corpHeads(ctx, corpID) {
		if ctx.Err() != nil {
			return
		}
		rank, err := m.memberRankErr(ctx, corpID, h.CharID)
		if err != nil {
			return // the database could not say; next pass
		}
		why := m.standsRefusal(ctx, h, rank)
		if why == nil {
			continue
		}
		if err := m.dismissHead(ctx, h); err != nil {
			rlogf("rolebots: head of %s in corp %d: %v", h.Arm, corpID, err)
			continue
		}
		m.humanLog(ctx, corpID, h.SetBy, "head", h.CharID, 0, "%s no longer heads %s: %s",
			m.charNameOr(ctx, h.CharID), armLabel(h.Arm), why.Error())
	}
}

// cancelDeskOrdersOf takes a pilot's orders for one corporation off the market, its fills booked
// first, and leaves what the chair ordered itself.
func (m *Mod) cancelDeskOrdersOf(ctx context.Context, corpID, charID int64) {
	if c := m.deskCEO(ctx, corpID); c != nil {
		m.sweepCorpBids(ctx, c)
	}
	ids, err := int64Column(ctx, m, `
		SELECT o.orderID FROM rolebot_corp_orders r JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND o.charID = ? AND o.isCorp = 1 AND o.orderState = 1 AND r.kind <> ?`,
		corpID, charID, corpOrderHuman)
	if err != nil {
		rlogf("rolebots: desk orders of %d: %v", charID, err)
		return
	}
	for _, id := range ids {
		if _, err := evedb.CancelMarketOrder(ctx, m.db, charID, id); err != nil {
			rlogf("rolebots: cancel desk order %d of %d: %v", id, charID, err)
			continue
		}
		m.forgetCorpOrder(ctx, id)
	}
}

// dropHeads forgets a corporation's appointments when a bot takes its chair: the bot CEO deals the
// posts itself, and deals the old heads' placements afresh.
func (m *Mod) dropHeads(ctx context.Context, corpID int64) {
	for _, h := range m.corpHeads(ctx, corpID) {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_arm_members SET tier = ?, setBy = 'ceo' WHERE corporationID = ? AND characterID = ? AND tier = ?`,
			armTierMember, corpID, h.CharID, armTierHead); err != nil {
			rlogf("rolebots: demote head %d of corp %d: %v", h.CharID, corpID, err)
		}
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_heads WHERE corporationID = ?`, corpID); err != nil {
		rlogf("rolebots: drop heads of corp %d: %v", corpID, err)
	}
	forgetChairHand(corpID)
}

// closeChairOrders ends what a person's chair left in force when a bot takes it: the settings let go
// and the orders are withdrawn, since nobody is left who could.
func (m *Mod) closeChairOrders(ctx context.Context, corpID, byID int64) {
	for _, c := range m.listCorpCommands(ctx, corpID, 500) {
		if !c.Open() {
			continue
		}
		if cmdHoldKinds[c.Kind] {
			_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, "the chair changed hands")
			continue
		}
		if why := m.withdrawCommand(ctx, c, byID); why != "" {
			rlogf("rolebots: order %d of corp %d stays as the chair changes hands: %s", c.CommandID, corpID, why)
		}
	}
}

// chairPlaceMember puts a member in a division at a tier ("" releases them to the pool), moving the
// role bits with them: what the old placement granted is taken back, what the new one brings is
// given. The row carries the chair's mark, so a bot CEO's deal leaves it alone.
func (m *Mod) chairPlaceMember(ctx context.Context, corpID, charID int64, arm corpArm, tier int, by int64) error {
	members, err := m.corpMembers(ctx, corpID, 0)
	if err != nil {
		return err
	}
	var cm corpMember
	found := false
	for _, x := range members {
		if x.CharID == charID {
			cm, found = m.asSeated(x), true
			break
		}
	}
	if !found {
		return errHeadNotMember
	}
	rows, err := m.loadArmRows(ctx, corpID)
	if err != nil {
		return err
	}
	prev, had := rows[charID]
	if arm == "" {
		if had {
			if _, err := m.applyArmRoles(ctx, cm, prev.GrantMask, armPlacement{}, false); err != nil {
				return err
			}
			m.deleteArmRow(ctx, charID)
		}
		m.refreshArmIndex(ctx)
		return nil
	}
	benched := m.benchedBuilders(ctx, corpID)[charID]
	grant, err := m.applyArmRoles(ctx, cm, prev.GrantMask, armPlacement{arm, tier}, benched)
	if err != nil {
		return err
	}
	row := armRow{CharID: charID, CorpID: corpID, Arm: arm, Tier: tier, Since: prev.Since,
		AssignedBy: by, GrantMask: grant, SetBy: postSetHuman}
	if !had || prev.Arm != arm {
		row.Since = clock.Unix()
	}
	if err := m.saveArmRow(ctx, row); err != nil {
		return err
	}
	m.refreshArmIndex(ctx)
	return nil
}
