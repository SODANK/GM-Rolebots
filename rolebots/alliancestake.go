package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// The alliance's stake (rolebot_alliance_targets, kind 'stake'): when no 0.0 system worth holding lies
// within sovTargetReachJumps of a member's headquarters, forward or front office, held system or
// contestable tower (alliancePlanSov's sources), the executor names the unclaimed
// constellation nearest the members, however far, as where the alliance means to go. Every unclaimed
// system of it becomes a stake row. Members read the rows like targets (allianceFrontierSystems):
// they reach for the frontier (frontierAmbition), survey there first (surveyOpts.Pull), open a
// forward office near it (forwardStationScore) and plant there first (considerNewTower). An alliance
// founded to hold 0.0 keeps a stake beside its targets. Switch: rolebots.alliance.frontier.

const (
	// stakeSystemsCap is the most unclaimed systems of a constellation a stake counts and writes.
	stakeSystemsCap = 12
	// stakeJumpCost discounts a constellation per jump from the nearest member.
	stakeJumpCost = 0.1
	// stakeDangerTop is how many of the best constellations have their danger measured.
	stakeDangerTop = 6
	// stakeKeepShare is how close the current stake must stay to the best one to be kept: a stake
	// moves only for a clearly better one, so members are not sent back and forth.
	stakeKeepShare = 0.8
)

// stakeIn is what the executor reads about one unclaimed constellation.
type stakeIn struct {
	Systems int     // unclaimed systems in it
	Worth   float64 // frontierSystemWorth summed over the surveyed ones
	Jumps   int     // from the nearest source (alliancePlanSov)
	Danger  float64 // towerDanger at its nearest system; 0 before it is measured
}

// stakeScore is how good a constellation is to stake: its unclaimed systems (up to eight) plus what
// surveys found there, discounted by distance and danger. Pure.
func stakeScore(in stakeIn) float64 {
	if in.Systems <= 0 {
		return 0
	}
	s := math.Min(float64(in.Systems), 8) + max(in.Worth, 0)
	return s / (1 + stakeJumpCost*float64(max(in.Jumps, 0))) / (1 + max(in.Danger, 0))
}

// stakeCand is one unclaimed constellation the executor could stake.
type stakeCand struct {
	Constellation int64
	Nearest       int64   // its unclaimed system nearest the members
	Systems       []int64 // its unclaimed systems, nearest first
	in            stakeIn
	Score         float64
}

// stakeCandidates groups the unclaimed systems the members reach by constellation, nearest system
// first in each, scored without danger, best first. dist is jumps from the nearest member source,
// conOf a system's constellation, worth frontierSystemWorth by system. Pure.
func stakeCandidates(frontier map[int64]bool, dist map[int64]int, conOf func(int64) int64, worth map[int64]float64) []stakeCand {
	byCon := map[int64]*stakeCand{}
	for sys := range frontier {
		d, ok := dist[sys]
		if !ok {
			continue
		}
		con := conOf(sys)
		if con == 0 {
			continue
		}
		sc := byCon[con]
		if sc == nil {
			sc = &stakeCand{Constellation: con, in: stakeIn{Jumps: d}}
			byCon[con] = sc
		}
		sc.Systems = append(sc.Systems, sys)
		sc.in.Worth += worth[sys]
		if d < sc.in.Jumps {
			sc.in.Jumps = d
		}
	}
	out := make([]stakeCand, 0, len(byCon))
	for _, sc := range byCon {
		sort.Slice(sc.Systems, func(i, j int) bool {
			di, dj := dist[sc.Systems[i]], dist[sc.Systems[j]]
			if di != dj {
				return di < dj
			}
			return sc.Systems[i] < sc.Systems[j]
		})
		if len(sc.Systems) > stakeSystemsCap {
			sc.Systems = sc.Systems[:stakeSystemsCap]
		}
		sc.Nearest = sc.Systems[0]
		sc.in.Systems = len(sc.Systems)
		sc.Score = stakeScore(sc.in)
		out = append(out, *sc)
	}
	sortStakes(out)
	return out
}

func sortStakes(c []stakeCand) {
	sort.SliceStable(c, func(i, j int) bool {
		if c[i].Score != c[j].Score {
			return c[i].Score > c[j].Score
		}
		return c[i].Constellation < c[j].Constellation
	})
}

// memberCorpIDs is the corporations of an alliance's member list.
func memberCorpIDs(members []allianceMemberFacts) []int64 {
	out := make([]int64, 0, len(members))
	for _, f := range members {
		out = append(out, f.CorpID)
	}
	return out
}

// allianceStakeSystems is an alliance's current stake rows.
func (m *Mod) allianceStakeSystems(ctx context.Context, allianceID int64) map[int64]bool {
	out := map[int64]bool{}
	ids, _ := int64Column(ctx, m, `SELECT systemID FROM rolebot_alliance_targets WHERE allianceID = ? AND kind = 'stake'`, allianceID)
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// allianceStake names (or keeps) the alliance's stake: the best unclaimed constellation the members'
// gates reach, by stakeScore, its danger measured for the best few. The current stake is kept while
// it stays within stakeKeepShare of the best.
func (m *Mod) allianceStake(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, u *universe, sources []int64) error {
	frontier := m.unclaimedNullSystems(ctx)
	if len(frontier) == 0 || len(sources) == 0 {
		return nil
	}
	dist, err := m.corpJumpMap(ctx, sources...)
	if err != nil {
		return err
	}
	worth := m.frontierWorths(ctx, seat.ExecutorCorpID, frontier)
	cands := stakeCandidates(frontier, dist, func(sys int64) int64 { return u.meta[sys].ConstellationID }, worth)
	if len(cands) == 0 {
		return nil
	}
	for i := 0; i < len(cands) && i < stakeDangerTop; i++ {
		cands[i].in.Danger = m.towerDanger(ctx, c.CorpID, cands[i].Nearest, u.meta[cands[i].Nearest].Security, ch.PosDangerWeight)
		cands[i].Score = stakeScore(cands[i].in)
	}
	if len(cands) > stakeDangerTop {
		cands = cands[:stakeDangerTop]
	}
	sortStakes(cands)
	pick := cands[0]
	old := m.allianceStakeSystems(ctx, seat.AllianceID)
	for _, sc := range cands {
		if old[sc.Nearest] && sc.Score >= stakeKeepShare*pick.Score {
			pick = sc // the standing stake is still good enough: members are already on their way
			break
		}
	}
	keep := map[int64]bool{}
	for _, sys := range pick.Systems {
		keep[sys] = true
	}
	now := clock.Unix()
	why := truncate(fmt.Sprintf("stake: %d unclaimed system(s), %d jump(s) from the nearest member, danger %.1f",
		pick.in.Systems, pick.in.Jumps, pick.in.Danger), 255)
	for _, sys := range pick.Systems {
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_alliance_targets (allianceID, systemID, kind, score, setAt, reason)
			VALUES (?, ?, 'stake', ?, ?, ?)
			ON DUPLICATE KEY UPDATE score = IF(kind = 'stake', VALUES(score), score),
			                        reason = IF(kind = 'stake', VALUES(reason), reason)`,
			seat.AllianceID, sys, pick.Score, now, why); err != nil {
			return fmt.Errorf("alliance stake: %w", err)
		}
	}
	moved := false
	for sys := range old {
		if !keep[sys] {
			moved = true
			_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_alliance_targets WHERE allianceID = ? AND systemID = ? AND kind = 'stake'`,
				seat.AllianceID, sys)
		}
	}
	if old[pick.Nearest] && !moved {
		return nil // the same stake as before
	}
	name := m.constellationName(ctx, pick.Constellation, pick.Nearest)
	line := fmt.Sprintf("stakes %s for %s: %d unclaimed system(s) from %s, %d jump(s) from the nearest member",
		name, seat.Name, pick.in.Systems, systemNameOf(ctx, m, pick.Nearest), pick.in.Jumps)
	m.ceoLog(ctx, c, powerAlliance, "stake", pick.Nearest, float64(pick.in.Systems), "%s", line)
	m.allianceBook(ctx, seat.AllianceID, c.CorpID, "stake", pick.Nearest, float64(pick.in.Systems), "%s", line)
	m.allianceBulletin(ctx, seat.AllianceID, c.CharID, fmt.Sprintf("%s: a stake in %s", seat.Name, name),
		fmt.Sprintf("Nothing within reach is ours to hold, so the alliance looks further: %s, %d unclaimed system(s) starting at %s, %d jump(s) from our nearest member. "+
			"Survey its moons, open a forward office near it and plant there: one online tower of ours in an unclaimed system makes it the alliance's.",
			name, pick.in.Systems, systemNameOf(ctx, m, pick.Nearest), pick.in.Jumps))
	return nil
}

// dropAllianceStake clears the alliance's stake once it has systems to hold within reach.
func (m *Mod) dropAllianceStake(ctx context.Context, c *corpCEO, seat *allianceSeat, why string) {
	res, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_alliance_targets WHERE allianceID = ? AND kind = 'stake'`, seat.AllianceID)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		m.ceoLog(ctx, c, powerAlliance, "stake", 0, 0, "lifts %s's stake: %s", seat.Name, why)
	}
}

// constellationName names a constellation, falling back to "the constellation of <system>".
func (m *Mod) constellationName(ctx context.Context, constellationID, anySystem int64) string {
	var name string
	_ = m.db.QueryRowContext(ctx, `SELECT constellationName FROM mapConstellations WHERE constellationID = ?`, constellationID).Scan(&name)
	if name == "" {
		return "the constellation of " + systemNameOf(ctx, m, anySystem)
	}
	return name
}
