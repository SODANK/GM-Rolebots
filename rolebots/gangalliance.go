package rolebots

// Joint fleets: the alliance as one bloc in space. Kinship treats an alliance-mate as kin
// where a bot decides who to shoot or help (kinship.go sameBloc, roe.go), and escorts
// cross the alliance (gangescort.go escortCorpEligible). The working seats and the siege
// arithmetic cross it too:
//   - gangRecruitAlliance fills a combat fleet's empty seats from every corporation of the
//     alliance, between the own-corp pass and the mutual-ally pass;
//   - siegeSideOwner counts every alliance pilot in the system on a side;
//   - a member's reinforced tower raises a defend request (rolebot_alliance_requests), and
//     every bot member's alliance power answers it with a Security fleet order;
//   - a corporation with no fleet doctrine of its own flies the executor's.

import (
	"context"
	"fmt"
)

const (
	// allianceDefendTTLSec bounds a defend call when the reinforcement timer
	// is unknown: a day.
	allianceDefendTTLSec = 86400
)

// allianceMemberCorps lists the corporations of corpID's alliance other than
// itself; nil when it is in none.
func (m *Mod) allianceMemberCorps(ctx context.Context, corpID int64) []int64 {
	aid := m.corpAllianceID(ctx, corpID)
	if aid == 0 {
		return nil
	}
	ids, _ := int64Column(ctx, m, `SELECT corporationID FROM corporation WHERE allianceID = ? AND corporationID <> ?`, aid, corpID)
	return ids
}

// gangRecruitAlliance fills the seats the commander's own corporation left empty from the
// rest of the alliance, for combat fleets only, like the ally pass. Guests carry their
// corporation as fromCorpID.
func (m *Mod) gangRecruitAlliance(ctx context.Context, lb *botRun, activity string, seats int, own []gangRecruitPick, pack bool, doc *FleetDoctrine) ([]gangRecruitPick, error) {
	if !gangAllySeatActivities[activity] || len(own) >= seats {
		return nil, nil
	}
	mates := m.allianceMemberCorps(ctx, lb.CorpID)
	if len(mates) == 0 {
		return nil, nil
	}
	return m.gangRecruitFrom(ctx, lb, activity, seats, mates, own, pack, doc)
}

// siegeSideOwner is a corporation's combatants in a system plus every
// alliance-mate's. The siege arithmetic reads it.
func (m *Mod) siegeSideOwner(ctx context.Context, corpID, sysID int64, role string) []*pvpCombatant {
	out := m.siegeSide(ctx, corpID, sysID, role)
	for _, mate := range m.allianceMemberCorps(ctx, corpID) {
		out = append(out, m.siegeSide(ctx, mate, sysID, role)...)
	}
	return out
}

// allianceRaiseDefend files a defend call for a member's tower under attack.
// One open call per tower.
func (m *Mod) allianceRaiseDefend(ctx context.Context, ownerCorp, towerID, sysID, exitAt int64) {
	aid := m.corpAllianceID(ctx, ownerCorp)
	if aid == 0 {
		return
	}
	var open int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_alliance_requests
		 WHERE allianceID = ? AND kind = ? AND targetID = ? AND status = 'pending'`,
		aid, allianceReqDefend, towerID).Scan(&open)
	if open > 0 {
		return
	}
	now := clock.Unix()
	expires := now + allianceDefendTTLSec
	if exitAt > now {
		expires = exitAt + 3600
	}
	note := fmt.Sprintf("%s's tower in %s is reinforced", corpNameOf(ctx, m.db, ownerCorp), systemNameOf(ctx, m, sysID))
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_alliance_requests
		  (allianceID, fromCorpID, posterID, kind, targetID, systemID, payload, note, postedAt, expiresAt, status)
		VALUES (?, ?, 0, ?, ?, ?, '', ?, ?, ?, 'pending')`,
		aid, ownerCorp, allianceReqDefend, towerID, sysID, truncate(note, 255), now, expires); err != nil {
		rlogf("rolebots: alliance defend call: %v", err)
		return
	}
	m.allianceBook(ctx, aid, ownerCorp, "defend", towerID, 0, "%s — the alliance is called to %s", note, systemNameOf(ctx, m, sysID))
	m.chatterAllyDefend(ctx, aid, ownerCorp, sysID, exitAt)
	if exec := m.allianceExecutor(ctx, aid); exec != 0 {
		if c, _ := getCEO(ctx, m.db, exec); c != nil {
			var name string
			_ = m.db.QueryRowContext(ctx, `SELECT allianceName FROM alliances WHERE allianceID = ?`, aid).Scan(&name)
			m.allianceBulletin(ctx, aid, c.CharID, fmt.Sprintf("%s: defend %s", name, systemNameOf(ctx, m, sysID)),
				note+". Every member corporation's security wing is asked to form up there before the timer ends.")
		}
	}
}

// allianceAnswerDefend is every bot member's duty: a Security fleet order
// toward each open defend call it has not yet answered. Whole alliance,
// security and combat seats (the Security department's pickers).
func (m *Mod) allianceAnswerDefend(ctx context.Context, c *corpCEO, seat *allianceSeat) error {
	reqs, err := m.allianceRequestsPending(ctx, seat.AllianceID, allianceReqDefend)
	if err != nil || len(reqs) == 0 {
		return err
	}
	for _, r := range reqs {
		var answered int
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_ceo_actions
			 WHERE corporationID = ? AND verb = 'defend' AND subjectID = ?`, c.CorpID, r.ID).Scan(&answered)
		if answered > 0 {
			continue
		}
		// The order itself is the Security desk's (allianceDefendOrderFor, gangcalls.go). This
		// logs the corporation's word that it is sending its wing, once.
		m.ceoLog(ctx, c, powerAlliance, "defend", r.ID, 0, "sends its security wing to %s — %s",
			systemNameOf(ctx, m, r.SystemID), r.Note)
		if r.FromCorp != c.CorpID {
			m.chatterAllyDefendYes(ctx, seat.AllianceID, c.CorpID, r)
		}
	}
	return nil
}
