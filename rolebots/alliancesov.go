package rolebots

// Shared sovereignty: the executor names the systems the alliance means to hold, and its members' towers go there.
//
// Sov is the majority rule (db/sovmajority.go): an online tower counts 1 and an outpost 2 for the owner's
// current alliance, and a corporation in no alliance holds nothing. The executor's pass (alliancePlanSov)
// scores every 0.0 system in reach (sovTargetScore) and keeps every held system plus the top MaxSovTargets
// not yet held as rolebot_alliance_targets rows (pickSovTargets): distance runs from held sov once there is
// any, and a system on its border scores more, so the alliance grows outward. The tower planner reads them: siteCandidates scores a target system x1.5, the cluster map counts
// alliance-mates' towers as the corporation's own, and towerDanger ignores them. A human executor's sov
// requests come first. A member's tower also opens to the alliance (allianceTowerPolicy). Moon intel is
// shared (posintel.go intelFriendsOf) and a sov flip reaches the mailbox and book (evedb.SovChangeHook).

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/server/posmgr"
)

const (
	// sovTargetReachJumps is how far from a member's home or tower the
	// executor looks for a system to hold.
	sovTargetReachJumps = 8
	// sovTargetSiteBonus is what a candidate moon in a target system scores
	// over the same moon elsewhere.
	sovTargetSiteBonus = 1.5
)

// sovContestable is the core's test for a system the majority rule counts (RecomputeSystemSov in
// db/sovmajority.go): 0.0 with no NPC faction on the system row, as Syndicate and Curse carry. It reads
// the system column alone: universe.sovFaction falls back to constellation and region for ore, and that
// fallback calls 31 contestable null systems NPC space. Pure.
func sovContestable(security float64, npcFaction bool) bool {
	return security < 0 && !npcFaction
}

// npcNullSystems is every 0.0 system the majority rule never contests.
func (m *Mod) npcNullSystems(ctx context.Context) (map[int64]bool, error) {
	ids, err := int64Column(ctx, m, `SELECT solarSystemID FROM mapSolarSystems WHERE security < 0 AND factionID IS NOT NULL AND factionID <> 0`)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// sovTargetIn is what the executor reads about one candidate system.
type sovTargetIn struct {
	OurTally     int // the alliance's sov score there (towers 1, outposts 2)
	TheirTally   int // the strongest other alliance's
	Held         bool
	MemberTowers int // members' towers in the system, online or not
	Danger       float64
	Jumps        int // from held sov when the alliance holds any, else from the nearest member home or tower
	// Border is gate jumps from the nearest system the alliance holds, -1 when it holds none or
	// none is in reach; SameCon is a system sharing a constellation with one it holds.
	Border  int
	SameCon bool
}

const (
	// sovBorderBonus is what a system one gate from held sov adds to sovTargetScore, and
	// sovConstellationBonus what one further out in a held constellation adds: the alliance fills
	// in around what it holds before it reaches anywhere else.
	sovBorderBonus        = 1.5
	sovConstellationBonus = 0.75
)

// sovTargetScore is how much the alliance wants a system. Holding it or being one tower short is worth
// most, then a member's tower already there, then lying against held sov (sovBorderBonus); distance
// and danger pull it down, and a system where another alliance is far ahead scores 0. Pure.
func sovTargetScore(in sovTargetIn) float64 {
	gap := in.TheirTally - in.OurTally
	if gap >= 3 {
		return 0 // three towers behind: not this alliance's fight
	}
	s := 1.0
	if in.Held {
		s += 2
	} else if gap <= 0 {
		s += 1.5 // level or ahead and not yet holding: one tower away
	} else {
		s += 0.5 / float64(gap)
	}
	if !in.Held {
		switch {
		case in.Border == 1:
			s += sovBorderBonus
		case in.SameCon:
			s += sovConstellationBonus
		}
	}
	s += 0.5 * math.Min(4, float64(in.MemberTowers))
	s /= 1 + 0.25*float64(max(in.Jumps, 0)) + in.Danger*2
	return s
}

// pickSovTargets keeps every held system plus the best maxNew systems not yet held, best first:
// what the alliance holds never takes a slot from where it means to grow. A held system that
// scores 0 is still kept. Pure.
func pickSovTargets(cands []sovTargetCand, maxNew int) []sovTargetCand {
	out := make([]sovTargetCand, 0, len(cands))
	n := 0
	for _, c := range cands {
		if c.held {
			out = append(out, c)
			continue
		}
		if c.score > 0 && n < maxNew {
			out = append(out, c)
			n++
		}
	}
	return out
}

// sovTargetCand is one system the executor scored.
type sovTargetCand struct {
	sys   int64
	score float64
	why   string
	held  bool
}

// allianceTargetSystems is the set of systems an alliance has named.
func (m *Mod) allianceTargetSystems(ctx context.Context, allianceID int64) map[int64]bool {
	out := map[int64]bool{}
	if allianceID == 0 {
		return out
	}
	ids, _ := int64Column(ctx, m, fmt.Sprintf(`SELECT systemID FROM rolebot_alliance_targets WHERE allianceID = %d AND kind = 'sov'`, allianceID))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// allianceMateTowerSystems is every system where a corporation of the
// alliance (other than corpID) has an active tower plan.
func (m *Mod) allianceMateTowerSystems(ctx context.Context, allianceID, corpID int64) map[int64]bool {
	out := map[int64]bool{}
	if allianceID == 0 {
		return out
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT t.systemID FROM rolebot_corp_towers t
		  JOIN corporation c ON c.corporationID = t.corporationID
		 WHERE c.allianceID = ? AND t.corporationID <> ? AND t.status <> 'gone'`, allianceID, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s int64
		if rows.Scan(&s) == nil {
			out[s] = true
		}
	}
	return out
}

// alliancePlanSov is the executor's pass over the map.
func (m *Mod) alliancePlanSov(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) error {
	if m.humanHoldOn(ctx, c.CorpID, "allsov") {
		return nil
	}
	if now := clock.Unix(); now-m.lastCEOActionAt(ctx, c.CorpID, "sovplan") < ch.DwellSec {
		return nil
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return err
	}
	// NPC null is never contested: a target there sends members' towers
	// where they count for nobody.
	npcNull, err := m.npcNullSystems(ctx)
	if err != nil {
		return err
	}
	// Where the members' 0.0 starts (localnull.go): their headquarters, their forward and front
	// offices, the systems the alliance holds, and its towers in contestable 0.0. Other offices and
	// towers in NPC null are not sources, or the alliance's push starts wherever a branch stands.
	memberSet := map[int64]bool{}
	var sources []int64
	for _, f := range members {
		memberSet[f.CorpID] = true
		if facts, err := getAnyCorpFacts(ctx, m.db, f.CorpID); err == nil && facts.SystemID != 0 {
			sources = append(sources, facts.SystemID)
		}
	}
	if ids := memberCorpIDs(members); len(ids) > 0 {
		offices, _ := int64Column(ctx, m, `
			SELECT DISTINCT s.solarSystemID FROM rolebot_corp_offices o JOIN staStations s ON s.stationID = o.stationID
			 WHERE o.corporationID IN (`+int64CSV(ids)+`) AND (o.claimed & ?) <> 0 AND o.state <> 'dropping'`,
			officeForward|officeFront)
		sources = append(sources, offices...)
	}
	held, _ := int64Column(ctx, m, `SELECT solarSystemID FROM solarSystemSov WHERE allianceID = ?`, seat.AllianceID)
	sources = append(sources, held...)
	towerSys := map[int64]int{}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT s.solarSystemID, COUNT(*) FROM starbase s JOIN corporation c ON c.corporationID = s.ownerCorpID
		 WHERE c.allianceID = ? GROUP BY s.solarSystemID`, seat.AllianceID); err == nil {
		for rows.Next() {
			var sys int64
			var n int
			if rows.Scan(&sys, &n) == nil {
				towerSys[sys] = n
				if sovContestable(u.meta[sys].Security, npcNull[sys]) {
					sources = append(sources, sys)
				}
			}
		}
		rows.Close()
	}
	// Human orders first: a sov request names a system outright.
	forced := map[int64]string{}
	if reqs, err := m.allianceRequestsPending(ctx, seat.AllianceID, allianceReqSov); err == nil {
		for _, r := range reqs {
			if r.SystemID != 0 && !sovContestable(u.meta[r.SystemID].Security, npcNull[r.SystemID]) {
				m.allianceRequestClose(ctx, r.ID, allianceReqRefused,
					fmt.Sprintf("%s cannot be held: only 0.0 outside NPC faction space counts towers", systemNameOf(ctx, m, r.SystemID)))
				continue
			}
			if r.SystemID != 0 {
				forced[r.SystemID] = fmt.Sprintf("ordered by %s", m.charName(ctx, r.PosterID))
				m.allianceRequestClose(ctx, r.ID, allianceReqTaken, "named a target")
			}
		}
	}
	// Candidates: 0.0 within reach of any source.
	jumps := map[int64]int{}
	for _, src := range sources {
		for sys, j := range bfsJumps(u, src, sovTargetReachJumps) {
			if cur, ok := jumps[sys]; !ok || j < cur {
				jumps[sys] = j
			}
		}
	}
	// The border: jumps from held sov, and the constellations it lies in. Once the alliance holds
	// anything, distance is measured from there, so it grows outward from what it holds.
	border := map[int64]int{}
	heldCons := map[int64]bool{}
	for _, h := range held {
		heldCons[u.meta[h].ConstellationID] = true
		for sys, j := range bfsJumps(u, h, sovTargetReachJumps) {
			if cur, ok := border[sys]; !ok || j < cur {
				border[sys] = j
			}
		}
	}
	var cands []sovTargetCand
	for sys, j := range jumps {
		if !sovContestable(u.meta[sys].Security, npcNull[sys]) {
			continue
		}
		tallies, err := evedb.SovTallies(ctx, m.db, sys)
		if err != nil {
			continue
		}
		in := sovTargetIn{MemberTowers: towerSys[sys], Jumps: j, Border: -1,
			SameCon: heldCons[u.meta[sys].ConstellationID],
			Danger:  m.towerDanger(ctx, c.CorpID, sys, u.meta[sys].Security, ch.PosDangerWeight)}
		if b, ok := border[sys]; ok {
			in.Border, in.Jumps = b, b
		}
		var top int64
		topScore := 0
		for _, t := range tallies {
			if t.AllianceID == seat.AllianceID {
				in.OurTally = t.Score
			} else if t.Score > in.TheirTally {
				in.TheirTally = t.Score
			}
			if t.Score > topScore {
				top, topScore = t.AllianceID, t.Score
			}
		}
		in.Held = top == seat.AllianceID && topScore > 0
		s := sovTargetScore(in)
		why := fmt.Sprintf("ours %d vs %d, %d member towers, %d jumps, danger %.1f", in.OurTally, in.TheirTally, in.MemberTowers, in.Jumps, in.Danger)
		if !in.Held && in.Border == 1 {
			why += ", on the border"
		}
		if f, ok := forced[sys]; ok {
			s, why = math.Max(s, 10), f
		}
		if s <= 0 && !in.Held {
			continue
		}
		cands = append(cands, sovTargetCand{sys: sys, score: s, why: why, held: in.Held})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].sys < cands[j].sys
	})
	maxTargets := ch.MaxSovTargets
	sovMotive := m.allianceSovMotive(ctx, seat.AllianceID)
	if sovMotive {
		maxTargets++ // founded to hold 0.0 (frontier.go)
	}
	cands = pickSovTargets(cands, maxTargets)
	growth := 0
	for _, x := range cands {
		if !x.held {
			growth++
		}
	}
	// Nowhere to grow within reach (or an alliance founded to hold 0.0): stake the nearest unclaimed
	// constellation, so members survey, open forward offices and plant out there (allianceStake).
	if growth == 0 || sovMotive {
		if err := m.allianceStake(ctx, c, ch, seat, u, sources); err != nil {
			return err
		}
	} else {
		m.dropAllianceStake(ctx, c, seat, "the alliance has systems to hold within reach")
	}
	// Write the set: replace, keeping what is still wanted.
	old := m.allianceTargetSystems(ctx, seat.AllianceID)
	now := clock.Unix()
	var added, kept, dropped []string
	wanted := map[int64]bool{}
	for _, x := range cands {
		wanted[x.sys] = true
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_alliance_targets (allianceID, systemID, kind, score, setAt, reason)
			VALUES (?, ?, 'sov', ?, ?, ?)
			ON DUPLICATE KEY UPDATE kind = 'sov', score = VALUES(score), reason = VALUES(reason)`,
			seat.AllianceID, x.sys, x.score, now, truncate(x.why, 255)); err != nil {
			return fmt.Errorf("alliance targets: %w", err)
		}
		if old[x.sys] {
			kept = append(kept, systemNameOf(ctx, m, x.sys))
		} else {
			added = append(added, systemNameOf(ctx, m, x.sys))
		}
	}
	for sys := range old {
		if !wanted[sys] {
			_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_alliance_targets WHERE allianceID = ? AND systemID = ? AND kind = 'sov'`, seat.AllianceID, sys)
			dropped = append(dropped, systemNameOf(ctx, m, sys))
		}
	}
	if len(added) == 0 && len(dropped) == 0 {
		// Nothing moved: note the pass so the dwell counts from it.
		m.ceoLog(ctx, c, powerAlliance, "sovplan", 0, 0, "keeps %s's %d sovereignty targets", seat.Name, len(kept))
		return nil
	}
	line := fmt.Sprintf("names %d sovereignty targets for %s", len(cands), seat.Name)
	if len(added) > 0 {
		line += "; new: " + strings.Join(added, ", ")
	}
	if len(dropped) > 0 {
		line += "; dropped: " + strings.Join(dropped, ", ")
	}
	m.ceoLog(ctx, c, powerAlliance, "sovplan", 0, float64(len(cands)), "%s", line)
	m.allianceBook(ctx, seat.AllianceID, c.CorpID, "targets", 0, float64(len(cands)), "%s", line)
	if len(added) > 0 {
		m.allianceBulletin(ctx, seat.AllianceID, c.CharID, fmt.Sprintf("%s: sovereignty targets", seat.Name),
			fmt.Sprintf("The executor asks the alliance's Infrastructure departments to plant towards %s. Towers there count for the alliance's claim.", strings.Join(added, ", ")))
	}
	return nil
}

// ── the members' towers ─────────────────────────────────────────────────────

// allianceTowerPolicy opens a running tower to the alliance (force field and structure access) and
// closes it again when the corporation has left one. Stored policy only; the core applies it under
// starbase.enforceAccess.
func (m *Mod) allianceTowerPolicy(ctx context.Context, c *corpCEO, p *towerPlan) {
	if p.TowerID == 0 {
		return
	}
	allied := m.corpAllianceID(ctx, c.CorpID) != 0
	rec, ok, err := evedb.GetStarbase(ctx, m.db, p.TowerID)
	if err != nil || !ok {
		return
	}
	pol, err := evedb.GetStarbasePolicy(ctx, m.db, rec)
	if err != nil {
		return
	}
	changed := false
	if pol.FFAllowAlliance != allied {
		pol.FFAllowAlliance = allied
		if err := evedb.SaveStarbasePolicy(ctx, m.db, pol); err != nil {
			m.logBot(c.CharID, logError, "ceo", "alliance force field at %s: %v", moonNameOf(ctx, m, p.MoonID), err)
			return
		}
		changed = true
		if posmgr.ForcefieldChanged != nil {
			posmgr.ForcefieldChanged(p.TowerID)
		}
	}
	level := evedb.StarbaseAccessCorporation
	if allied {
		level = evedb.StarbaseAccessAlliance
	}
	if rows, err := evedb.ListStructureAccess(ctx, m.db, p.TowerID); err == nil && len(rows) > 0 {
		dirty := false
		for i := range rows {
			if rows[i].ViewPut != level || rows[i].Use != level {
				rows[i].ViewPut, rows[i].Use = level, level
				dirty = true
			}
		}
		if dirty {
			if _, err := evedb.SaveStructureAccess(ctx, m.db, p.TowerID, rows); err == nil {
				changed = true
			}
		}
	}
	if changed {
		verb, what := "closed", "to the corporation"
		if allied {
			verb, what = "opened", "to the alliance"
		}
		m.ceoLog(ctx, c, powerStarbase, "access", p.TowerID, 0, "%s its tower at %s %s (force field and structures)", verb, moonNameOf(ctx, m, p.MoonID), what)
	}
}

// ── sov flips ───────────────────────────────────────────────────────────────

// startAllianceSovHook takes the core's sov seam, if nobody has.
func (m *Mod) startAllianceSovHook() {
	if evedb.SovChangeHook != nil {
		return
	}
	evedb.SovChangeHook = func(ch evedb.SovChange) {
		if !ch.Changed || !Master() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m.onSovChange(ctx, ch)
	}
}

// onSovChange tells the alliance that gained and the one that lost.
func (m *Mod) onSovChange(ctx context.Context, ch evedb.SovChange) {
	sys := systemNameOf(ctx, m, ch.SystemID)
	for _, side := range []struct {
		alliance int64
		verb     string
	}{{ch.NewAlliance, "sov-gain"}, {ch.OldAlliance, "sov-loss"}} {
		if side.alliance == 0 {
			continue
		}
		exec := m.allianceExecutor(ctx, side.alliance)
		if exec == 0 {
			continue
		}
		var name string
		_ = m.db.QueryRowContext(ctx, `SELECT allianceName FROM alliances WHERE allianceID = ?`, side.alliance).Scan(&name)
		gained := side.verb == "sov-gain"
		line := fmt.Sprintf("%s now holds %s", name, sys)
		if !gained {
			line = fmt.Sprintf("%s lost %s", name, sys)
			if ch.NewAlliance != 0 {
				line += " to " + ownerNameOf(ctx, m.db, ch.NewAlliance)
			}
		}
		m.allianceBook(ctx, side.alliance, exec, side.verb, ch.SystemID, 0, "%s", line)
		if c, _ := getCEO(ctx, m.db, exec); c != nil {
			m.ceoLog(ctx, c, powerAlliance, side.verb, ch.SystemID, 0, "%s", line)
			subject := fmt.Sprintf("%s: sovereignty of %s", name, sys)
			body := line + ". Sovereignty follows the alliance whose online towers outnumber the rest in a system."
			m.allianceBulletin(ctx, side.alliance, c.CharID, subject, body)
		}
	}
}
