package rolebots

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// A pilot leaving its corporation on morale. corpmorale.go's sampler decides and flags
// rolebot_corp_reviews.exitDueAt; the pilot's next review (corpjoin.go) runs considerLeaving, which
// asks the guards, picks a destination and moves it through leaveCorpTo. docs/CORPS.md §5b.

const (
	moraleWarnD   = 0.5        // a human CEO hears once when a member's debt reaches this
	leaveLapseSec = 10 * 86400 // a flag the guards kept deferring this long is dropped
	leaveWalkoutD = 0.85       // with applications still pending, walk out anyway from here
	leaveDeferSec = 86400
)

// moraleExitChance is the sampler's roll: the hazard over the sample window, ramped in over the
// fortnight after the switch went on. Pure.
func moraleExitChance(h, dtDays float64, on bool, onAt, now int64) float64 {
	p := moraleLeaveProb(h, dtDays)
	if on {
		p *= moraleRamp(onAt, now)
	}
	return p
}

// flagExitDue marks the pilot for its next review, now.
func (m *Mod) flagExitDue(ctx context.Context, charID, now int64) error {
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_reviews (characterID, nextReviewAt, exitDueAt) VALUES (?, 0, ?)
		ON DUPLICATE KEY UPDATE exitDueAt = VALUES(exitDueAt),
		  nextReviewAt = IF(nextReviewAt = 0, 0, LEAST(nextReviewAt, ?))`, charID, now, now)
	if err != nil {
		return fmt.Errorf("flagExitDue: %w", err)
	}
	corpReviewNextAt.Delete(charID)
	return nil
}

// warnHumanCEO mails a human CEO once per member whose debt reached moraleWarnD.
func (m *Mod) warnHumanCEO(ctx context.Context, corpID int64, mb moraleMember, d float64, now int64) {
	wrote, err := m.recordMoraleEvent(ctx, corpID, moraleEvent{CharID: mb.CharID, Kind: mkWarned,
		SrcKey: fmt.Sprintf("corp:%d", corpID), Detail: fmt.Sprintf("D %.2f", d)}, now)
	if err != nil || !wrote {
		return
	}
	ceo := m.corpCEOID(ctx, corpID)
	if ceo == 0 || ceo == mb.CharID {
		return
	}
	m.mailPilot(ctx, ceo, mb.CharID, "A word about my place here",
		fmt.Sprintf("%s here. I'll be straight: I'm not happy in the corporation, and if nothing changes I'll be looking elsewhere.", mb.Name))
}

// leaveGuard is the first reason the pilot cannot go yet, "" when it can.
func (m *Mod) leaveGuard(ctx context.Context, charID int64) string {
	if po, err := m.openPOFor(ctx, charID); err == nil && po != nil {
		return "an open purchase order"
	}
	if holdsLoanedKit(charID) {
		return "kit on loan from the armory"
	}
	if conscriptOf(charID) != nil {
		return "serving in a player's fleet"
	}
	if g, _, err := gangForChar(ctx, m.db, charID); err == nil && g != nil && g.LeaderCharID == charID {
		return "leading a fleet"
	}
	for _, c := range []struct{ why, q string }{
		{"a haul it claimed", `SELECT 1 FROM rolebot_corp_haul WHERE carrierID = ? AND status = 'claimed' LIMIT 1`},
		{"a factory move it is carrying", `SELECT 1 FROM rolebot_corp_moves WHERE carrierID = ? AND status = 'open' LIMIT 1`},
		{"a print it is carrying", `SELECT 1 FROM rolebot_corp_print_moves WHERE carrierID = ? AND status IN ('claimed', 'carrying') LIMIT 1`},
		{"a work order it claimed", `SELECT 1 FROM rolebot_corp_work_claims WHERE characterID = ? LIMIT 1`},
	} {
		var one int
		if err := m.db.QueryRowContext(ctx, c.q, charID).Scan(&one); err == nil {
			return c.why
		}
	}
	return ""
}

// leaveDestination picks where a flagged pilot goes. offer is the best open door other than its
// own, nil for none. Pure.
func leaveDestination(offer *corpCandidate, npcEmployer bool, d float64, pendingApps int, prevNPC, school int64) (dest int64, how string) {
	switch {
	case offer != nil && offer.Corp.Player:
		return offer.Corp.CorpID, "apply"
	case offer != nil:
		return offer.Corp.CorpID, "offer"
	case npcEmployer:
		return 0, "stay"
	case pendingApps > 0 && d < leaveWalkoutD:
		return 0, "wait"
	}
	if prevNPC == 0 {
		prevNPC = school
	}
	if prevNPC == 0 {
		return 0, "stay"
	}
	return prevNPC, "walkout"
}

// bestLeaveOffer is the top open door other than the current corporation, over the absolute floor only.
func bestLeaveOffer(ranked []corpCandidate, currentCorpID int64) *corpCandidate {
	var best *corpCandidate
	for i := range ranked {
		c := &ranked[i]
		if c.Corp.CorpID == currentCorpID || !c.Score.Verdict.OK || c.Score.Score < corpMinAcceptScore {
			continue
		}
		if best == nil || c.Score.Score > best.Score.Score {
			best = c
		}
	}
	return best
}

func (m *Mod) moraleDebtOf(ctx context.Context, corpID, charID, now int64) float64 {
	cells, err := m.loadMoraleCells(ctx, corpID)
	if err != nil {
		return 0
	}
	return moraleTotal(cells[charID], now)
}

// considerLeaving acts on the sampler's flag at the pilot's review. It reports whether the pilot moved.
func (m *Mod) considerLeaving(ctx context.Context, b *botRun, rev *corpReviewRow, traits map[string]int) (bool, error) {
	now := clock.Unix()
	settle := func(kind, why string) {
		rev.ExitDueAt = 0
		_, _ = m.recordMoraleEvent(ctx, b.CorpID, moraleEvent{CharID: b.CharID, Kind: kind,
			SrcKey: fmt.Sprintf("day:%d", now/86400), Detail: why}, now)
		m.logBot(b.CharID, logInfo, "corp", "was set to leave %s, and stays: %s", m.corpNameOf(ctx, b.CorpID), why)
	}
	if corpIsSchool(ctx, m.db, b.CorpID) || corpIsConcord(ctx, m.db, b.CorpID) || corpIsSansha(ctx, m.db, b.CorpID) {
		settle(mkExitStayed, "nobody leaves this corporation on morale")
		return false, nil
	}
	if _, ok := managedCEOOf(b.CharID); ok || m.corpCEOID(ctx, b.CorpID) == b.CharID {
		settle(mkExitStayed, "it is the CEO")
		return false, nil
	}
	if why := m.leaveGuard(ctx, b.CharID); why != "" {
		if now-rev.ExitDueAt > leaveLapseSec {
			settle(mkExitLapsed, why)
			return false, nil
		}
		rev.NextReviewAt = now + leaveDeferSec + b.CharID%leaveDeferSec
		m.logBot(b.CharID, logInfo, "corp", "wants out of %s, staying for now: %s", m.corpNameOf(ctx, b.CorpID), why)
		return false, nil
	}
	npc, err := isNPCCorp(ctx, m.db, b.CorpID)
	if err != nil {
		return false, err
	}
	d := m.moraleDebtOf(ctx, b.CorpID, b.CharID, now)
	blocked, pendingApps, err := m.corpApplyBlocked(ctx, b.CharID)
	if err != nil {
		return false, err
	}
	var ranked []corpCandidate
	var pilot pilotIdentity
	if pilots, err := m.loadPilotIdentities(ctx, b.CharID, false); err == nil && len(pilots) > 0 {
		pilot = pilots[0]
		if ranked, err = m.rankCorpsForPilot(ctx, pilot, 0); err != nil {
			return false, err
		}
	}
	for i := range ranked {
		if blocked[ranked[i].Corp.CorpID] && ranked[i].Corp.CorpID != b.CorpID {
			ranked[i].Score.Verdict = corpVerdict{Rule: "applied", Why: "this pilot applied here recently and is not asking again yet"}
			ranked[i].Score.Score = 0
		}
	}
	offer := bestLeaveOffer(ranked, b.CorpID)
	if offer != nil && offer.Corp.Player && pendingApps >= corpApplyMax {
		offer = nil
	}
	dest, how := leaveDestination(offer, npc, d, pendingApps, evedb.PreviousNPCCorp(ctx, m.db, b.CharID), rev.SchoolCorpID)
	switch how {
	case "apply":
		if err := m.applyToPlayerCorp(ctx, b, offer, pilot); err != nil {
			return false, err
		}
		rev.NextReviewAt = now + 2*leaveDeferSec
		m.logBot(b.CharID, logInfo, "corp", "wants out of %s and has applied to %s", m.corpNameOf(ctx, b.CorpID), offer.Corp.Name)
		return false, nil
	case "wait":
		rev.NextReviewAt = now + leaveDeferSec
		return false, nil
	case "stay":
		settle(mkExitStayed, "nowhere better to go")
		return false, nil
	case "walkout":
		// The previous employer may refuse the pilot on identity; the school takes it then.
		for i := range ranked {
			c := &ranked[i]
			if c.Corp.CorpID == dest && !c.Score.Verdict.OK && (c.Score.Verdict.Rule == "race" || c.Score.Verdict.Rule == "tribe") {
				dest = rev.SchoolCorpID
			}
		}
		if dest == 0 || dest == b.CorpID || corpIsConcord(ctx, m.db, dest) || corpIsSansha(ctx, m.db, dest) {
			settle(mkExitStayed, "nowhere to walk out to")
			return false, nil
		}
	}
	if err := m.leaveCorpTo(ctx, b, rev, dest, how, d); err != nil {
		return false, err
	}
	return true, nil
}

// leaveCorpTo is the move: the core's JoinCorporation, then the module's own rows, the review row and the voice.
func (m *Mod) leaveCorpTo(ctx context.Context, b *botRun, rev *corpReviewRow, dest int64, how string, d float64) error {
	oldCorp := b.CorpID
	lock := corpOpLock(oldCorp)
	lock.Lock()
	defer lock.Unlock()
	ctx = withOpLockHeld(ctx)
	oldName, destName := m.corpNameOf(ctx, oldCorp), m.corpNameOf(ctx, dest)
	// Said in corp chat before presence drops the channel.
	m.chatterNoteBot(ctx, b.CharID, "corp.leave", map[string]string{"corp": oldName}, 0)
	if _, err := evedb.JoinCorporation(ctx, m.db, b.CharID, dest, oldCorp); err != nil {
		return fmt.Errorf("leaving %s for %s: %w", oldName, destName, err)
	}
	m.leaveCorpSteps(ctx, b.CharID, oldCorp)
	m.markLeft(ctx, b.CharID, oldCorp, leftOnMorale)
	now := clock.Unix()
	rev.LastMoveAt, rev.FromCorpID, rev.HeldCorpID, rev.ExitDueAt = now, oldCorp, dest, 0
	rev.Moves++
	b.CorpID = dest
	_, _ = m.recordMoraleEvent(ctx, oldCorp, moraleEvent{CharID: b.CharID, Kind: mkLeft,
		SrcKey: fmt.Sprintf("day:%d", now/86400), Detail: fmt.Sprintf("%s to %s, D %.2f", how, destName, d)}, now)
	m.logBot(b.CharID, logInfo, "corp", "left %s for %s (%s; morale %.2f)", oldName, destName, how, d)
	if _, ok := managedCEO(oldCorp); ok {
		m.queueCorpNews(ctx, oldCorp, newsMembers, fmt.Sprintf("%s left the corporation.", b.Name), b.CharID)
	} else if npc, _ := isNPCCorp(ctx, m.db, oldCorp); !npc {
		if ceo := m.corpCEOID(ctx, oldCorp); ceo != 0 {
			m.mailPilot(ctx, ceo, b.CharID, "Resignation",
				fmt.Sprintf("%s here. I've left %s today. It wasn't working for me any more. Fly safe.", b.Name, oldName))
		}
	}
	m.syncPresence()
	return nil
}

// leaveCorpSteps drops what the old corporation kept on the pilot: the corporation-keyed rows of
// moduleCharacterSteps (botpurge.go), its tower residency, its gang, an NPC security-head seat and its title.
func (m *Mod) leaveCorpSteps(ctx context.Context, charID, corpID int64) {
	for _, q := range []string{
		`DELETE FROM rolebot_corp_posts WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_corp_ranks WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_corp_bench WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_corp_grants WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_corp_pos_grants WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_corp_invites WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_corp_merit WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_corp_merit_log WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_corp_arm_members WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_corp_assignments WHERE characterID = ? AND corporationID = ?`,
		`DELETE FROM rolebot_human_offers WHERE characterID = ? AND corporationID = ?`,
	} {
		if _, err := m.db.ExecContext(ctx, q, charID, corpID); err != nil {
			rlogf("rolebots: leave %d from %d: %v", charID, corpID, err)
		}
	}
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_tower_residents WHERE characterID = ?`, charID)
	_, _ = m.db.ExecContext(ctx, `UPDATE character_ SET title = '' WHERE characterID = ?`, charID)
	if r, ok, err := m.getNPCChair(ctx, corpID); err == nil && ok && r.SecurityHeadID == charID {
		r.SecurityHeadID = 0
		_ = m.saveNPCChair(ctx, r)
	}
	if g, _, err := gangForChar(ctx, m.db, charID); err == nil && g != nil && g.CorpID == corpID && g.LeaderCharID != charID {
		_ = removeGangMember(ctx, m.db, g.GangID, charID)
	}
	m.refreshPostIndex(ctx)
	forgetCorpHome(charID)
}

// Reasons markLeft records on the application row.
const (
	leftOnMorale = "left on morale"
	leftByApply  = "moved on by application"
)

// markLeft closes the door on the corporation left for corpApplyLeftCooldownSec (corpapply.go).
func (m *Mod) markLeft(ctx context.Context, charID, corpID int64, reason string) {
	now := clock.Unix()
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_applications (characterID, corporationID, appliedAt, expiresAt, decidedAt, state, score, reason)
		VALUES (?, ?, ?, 0, ?, ?, 0, ?)
		ON DUPLICATE KEY UPDATE state = VALUES(state), decidedAt = VALUES(decidedAt), reason = VALUES(reason)`,
		charID, corpID, now, now, corpAppLeft, reason)
	if err != nil {
		rlogf("rolebots: mark left %d/%d: %v", charID, corpID, err)
	}
}
