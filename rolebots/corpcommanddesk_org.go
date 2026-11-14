package rolebots

import (
	"context"
	"fmt"
	"strings"

	"gurimoon/esp"
)

// The order desk's organisation half (corpcommands_org.go): placements are
// written and done; settings are applied, held, and let go when the hold
// runs out.

// takeOrgCommand carries out one organisation order. Reports whether the
// kind was one of these.
func (m *Mod) takeOrgCommand(ctx context.Context, c corpCommand) bool {
	who := m.charNameOr(ctx, c.PosterID)
	hold := func(days int) int64 { return clock.Unix() + int64(days)*86400 }
	live := func(holdUntil int64, refKind string, refID int64, result string) {
		_ = m.claimCommand(ctx, c.CommandID, c.PosterID, refKind, refID, result)
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_commands SET status = 'live', expiresAt = ? WHERE commandID = ?`,
			holdUntil, c.CommandID)
	}
	done := func(refKind string, refID int64, result string) {
		_ = m.claimCommand(ctx, c.CommandID, c.PosterID, refKind, refID, result)
		_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, result)
	}
	refuse := func(why string) {
		_ = m.setCommandStatus(ctx, c.CommandID, cmdRefused, why)
		m.mailCommandOutcome(ctx, c, "Order refused: "+c.Title(m, ctx), "It could not be carried out: "+why+".")
	}

	switch c.Kind {
	case cmdKindTags:
		p, ok := c.tags()
		if !ok || len(p.Tags) == 0 {
			refuse("no tags named")
			return true
		}
		facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
		if err != nil || facts == nil {
			refuse("the corporation's policy could not be read")
			return true
		}
		pol := facts.Policy
		pol.CorpID = c.CorpID
		pol.Tags = append([]string(nil), p.Tags...)
		if err := saveCorpPolicy(ctx, m.db, &pol); err != nil {
			refuse("the policy could not be written")
			return true
		}
		forgetCorp(c.CorpID)
		live(hold(p.HoldDays), "policy", c.CorpID, "declared a "+strings.Join(p.Tags, "/")+" outfit")
		m.humanLog(ctx, c.CorpID, c.PosterID, "tags", c.CorpID, 0, "%s declared the corporation a %s outfit", who, strings.Join(p.Tags, "/"))
		return true

	case cmdKindSeat:
		p, ok := c.seat()
		if !ok || p.CharID == 0 {
			refuse("no member named")
			return true
		}
		if !m.isSynthetic(p.CharID) || m.charCorpID(ctx, p.CharID) != c.CorpID {
			refuse("that pilot is not a member the module flies")
			return true
		}
		now := clock.Unix()
		if p.Release {
			_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_ranks WHERE corporationID = ? AND characterID = ?`, c.CorpID, p.CharID)
			done("seat", p.CharID, m.charNameOr(ctx, p.CharID)+" stood down")
			m.humanLog(ctx, c.CorpID, c.PosterID, "seat", p.CharID, 0, "%s released %s from command", who, m.charNameOr(ctx, p.CharID))
			return true
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_ranks (corporationID, characterID, activity, seat, score, manual, appointedAt, updatedAt)
			VALUES (?, ?, ?, 0, 0, 1, ?, ?)
			ON DUPLICATE KEY UPDATE activity = VALUES(activity), manual = 1, appointedAt = VALUES(appointedAt), updatedAt = VALUES(updatedAt)`,
			c.CorpID, p.CharID, p.Activity, now, now); err != nil {
			refuse("the seat could not be written")
			return true
		}
		done("seat", p.CharID, m.charNameOr(ctx, p.CharID)+" commands "+strings.ToLower(activityLabel(p.Activity))+" fleets")
		m.humanLog(ctx, c.CorpID, c.PosterID, "seat", p.CharID, 0, "%s seated %s as %s fleet commander", who,
			m.charNameOr(ctx, p.CharID), p.Activity)
		m.logBot(p.CharID, logInfo, "corp", "seated as %s fleet commander by the chair (%s)", p.Activity, who)
		return true

	case cmdKindIndustry:
		p, ok := c.industry()
		if !ok || p.DirectorID == 0 {
			refuse("no director named")
			return true
		}
		for _, id := range []int64{p.DirectorID, p.DeputyID} {
			if id != 0 && m.charCorpID(ctx, id) != c.CorpID {
				refuse(m.charNameOr(ctx, id) + " is not a member")
				return true
			}
		}
		pol, err := m.loadCorpPolicy(ctx, c.CorpID)
		if err != nil {
			refuse("the policy could not be read")
			return true
		}
		if pol.DirectorID != p.DirectorID {
			pol.DirectorID, pol.DirectorSince = p.DirectorID, clock.Unix()
		}
		pol.DeputyID = p.DeputyID
		if err := m.saveCorpPolicyRow(ctx, pol); err != nil {
			refuse("the policy could not be written")
			return true
		}
		live(hold(p.HoldDays), "policy", c.CorpID, corpIndustryDirector+" "+m.charNameOr(ctx, p.DirectorID))
		m.humanLog(ctx, c.CorpID, c.PosterID, "industry", p.DirectorID, 0, "%s named %s %s", who,
			m.charNameOr(ctx, p.DirectorID), corpIndustryDirector)
		return true

	case cmdKindArm:
		p, ok := c.arm()
		if !ok || p.CharID == 0 {
			refuse("no member named")
			return true
		}
		if !m.isSynthetic(p.CharID) || m.charCorpID(ctx, p.CharID) != c.CorpID {
			refuse("that pilot is not a member the module flies")
			return true
		}
		if p.Arm == "" {
			m.deleteArmRow(ctx, p.CharID)
			m.refreshArmIndex(ctx)
			done("arm", p.CharID, m.charNameOr(ctx, p.CharID)+" released to the pool")
			m.humanLog(ctx, c.CorpID, c.PosterID, "arm", p.CharID, 0, "%s released %s to the pool", who, m.charNameOr(ctx, p.CharID))
			return true
		}
		valid := false
		for _, a := range armOrder {
			if string(a) == p.Arm {
				valid = true
			}
		}
		if !valid {
			refuse("no arm called " + p.Arm)
			return true
		}
		rows, _ := m.loadArmRows(ctx, c.CorpID)
		row := rows[p.CharID]
		row.CharID, row.CorpID = p.CharID, c.CorpID
		if row.Arm != corpArm(p.Arm) {
			row.Since = clock.Unix()
		}
		row.Arm, row.AssignedBy, row.SetBy = corpArm(p.Arm), c.PosterID, postSetHuman
		if err := m.saveArmRow(ctx, row); err != nil {
			refuse("the arm could not be written")
			return true
		}
		m.refreshArmIndex(ctx)
		done("arm", p.CharID, m.charNameOr(ctx, p.CharID)+" to "+armLabel(corpArm(p.Arm)))
		m.humanLog(ctx, c.CorpID, c.PosterID, "arm", p.CharID, 0, "%s placed %s in %s", who, m.charNameOr(ctx, p.CharID), armLabel(corpArm(p.Arm)))
		m.logBot(p.CharID, logInfo, "corp", "placed in %s by the chair (%s)", armLabel(corpArm(p.Arm)), who)
		return true

	case cmdKindPosting:
		p, ok := c.posting()
		if !ok || p.CharID == 0 || p.StationID == 0 {
			refuse("no member or office named")
			return true
		}
		if !m.isSynthetic(p.CharID) || m.charCorpID(ctx, p.CharID) != c.CorpID {
			refuse("that pilot is not a member the module flies")
			return true
		}
		if !m.corpHasOfficeAt(ctx, c.CorpID, p.StationID) {
			refuse("the corporation has no office at " + m.stationLabel(ctx, p.StationID))
			return true
		}
		arm := corpArm("")
		if s, ok := armOf(p.CharID, c.CorpID); ok {
			arm = s.Arm
		}
		if err := m.writePosting(ctx, c.CorpID, p.CharID, p.StationID, arm, postSetHuman); err != nil {
			refuse("the posting could not be written")
			return true
		}
		done("posting", p.CharID, m.charNameOr(ctx, p.CharID)+" posted to "+m.stationLabel(ctx, p.StationID))
		m.humanLog(ctx, c.CorpID, c.PosterID, "basepost", p.CharID, 0, "%s posted %s to %s", who,
			m.charNameOr(ctx, p.CharID), m.stationLabel(ctx, p.StationID))
		m.logBot(p.CharID, logInfo, "corp", "posted to %s by the chair (%s)", m.stationLabel(ctx, p.StationID), who)
		return true

	case cmdKindBudget:
		p, ok := c.budget()
		if !ok {
			refuse("the budget could not be read")
			return true
		}
		live(hold(p.HoldDays), "policy", c.CorpID, c.orgSummary(m, ctx))
		m.humanLog(ctx, c.CorpID, c.PosterID, "budget", c.CorpID, p.ReserveMonths, "%s set the budget: %s", who, c.orgSummary(m, ctx))
		return true

	case cmdKindOreDrop:
		p, ok := c.oreDrop()
		if !ok {
			refuse("the drop-off could not be read")
			return true
		}
		if p.StationID != 0 && !m.corpHasOfficeAt(ctx, c.CorpID, p.StationID) {
			refuse("the corporation has no office at " + m.stationLabel(ctx, p.StationID))
			return true
		}
		if err := m.setCorpFleetOreStation(ctx, c.CorpID, p.StationID); err != nil {
			refuse("the drop-off could not be written")
			return true
		}
		live(hold(p.HoldDays), "estate", p.StationID, c.orgSummary(m, ctx))
		m.humanLog(ctx, c.CorpID, c.PosterID, "oredrop", p.StationID, 0, "%s set the fleet ore drop-off: %s", who, c.orgSummary(m, ctx))
		return true

	case cmdKindOreDesk:
		p, ok := c.oreDesk()
		if !ok {
			refuse("the ore desk order could not be read")
			return true
		}
		pol, err := m.loadCorpPolicy(ctx, c.CorpID)
		if err != nil {
			refuse("the policy could not be read")
			return true
		}
		pol.OreShare, pol.OreTarget, pol.OreAt = p.Share, p.Share, clock.Unix()
		pol.OreWhy = "set by the chair (" + who + ")"
		if err := m.saveCorpPolicyRow(ctx, pol); err != nil {
			refuse("the policy could not be written")
			return true
		}
		live(hold(p.HoldDays), "policy", c.CorpID, c.orgSummary(m, ctx))
		m.humanLog(ctx, c.CorpID, c.PosterID, "oredesk", c.CorpID, p.Share, "%s turned the ore desk to %.0f%% kept", who, p.Share*100)
		return true
	}
	return false
}

// followOrgCommand lets a held setting go when its hold runs out. Reports
// whether the kind was one of these.
func (m *Mod) followOrgCommand(ctx context.Context, c corpCommand) bool {
	if !cmdHoldKinds[c.Kind] {
		return false
	}
	if c.ExpiresAt > 0 && clock.Unix() >= c.ExpiresAt {
		_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, "the hold ran out")
		if c.Kind == cmdKindArm {
			m.refreshArmIndex(ctx)
		}
	}
	return true
}

// corpHasOfficeAt reports whether the corporation rents an office (or owns
// the station) at a station.
func (m *Mod) corpHasOfficeAt(ctx context.Context, corpID, stationID int64) bool {
	offices, err := m.loadPostOffices(ctx, corpID)
	if err != nil {
		return false
	}
	for _, o := range offices {
		if o.StationID == stationID {
			return true
		}
	}
	return false
}

// chairCharter is a corporation's charter with the chair's budget on top:
// the numbers a live budget order names replace the CEO's own leaning for
// as long as the order is held. Every reader of a CEO's charter goes
// through here.
func (m *Mod) chairCharter(ctx context.Context, corpID int64, ch Charter) Charter {
	c := m.humanHold(ctx, corpID, cmdKindBudget)
	if c == nil {
		return ch
	}
	p, ok := c.budget()
	if !ok {
		return ch
	}
	if p.ReserveMonths > 0 {
		ch.ReserveMonths = p.ReserveMonths
	}
	if p.StipendShare >= 0 {
		ch.StipendShare = p.StipendShare
	}
	if p.StoreMarkup >= 0 {
		ch.StoreMarkup = p.StoreMarkup
	}
	if p.BuybackFrac > 0 {
		ch.BuybackFrac = p.BuybackFrac
	}
	return ch
}

// humanLevyCorps is every corporation whose chair has the levy on by a live
// budget order: the levy sweep's scope beside the bot-run corporations.
func (m *Mod) humanLevyCorps(ctx context.Context) map[int64]bool {
	rows, err := m.db.QueryContext(ctx, `
		SELECT `+cmdCols+` FROM rolebot_corp_commands
		 WHERE kind = ? AND status = 'live' AND expiresAt > ?`, cmdKindBudget, clock.Unix())
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			continue
		}
		if p, ok := c.budget(); ok && p.LevyOn {
			out[c.CorpID] = true
		}
	}
	return out
}

// orgHoldLine words a held setting for a page.
func orgHoldLine(c *corpCommand, m *Mod, ctx context.Context) string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s — set by %s, until %s", c.orgSummary(m, ctx), m.charNameOr(ctx, c.PosterID), esp.YC(unixTime(c.ExpiresAt)))
}
