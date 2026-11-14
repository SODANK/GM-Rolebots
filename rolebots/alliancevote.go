package rolebots

// The pure half of the alliance power (alliance.go): who a member CEO votes for, when it walks out,
// which applicants an executor lets in, who it shows the door, how a fronted bill is shared and how
// the panel sums a bloc up. No clock, database or randomness; every rule has a table in alliance_test.go.

import (
	"fmt"
	"math"
	"sort"
)

// allianceMemberFacts is what the alliance power reads about one member
// corporation, its own included.
type allianceMemberFacts struct {
	CorpID   int64
	Name     string
	Pilots   int64 // players + rostered bots
	Wallet   float64
	CEOID    int64
	HumanCEO bool    // no managed CEO in the chair
	Lead     float64 // its CEO's leadership pole, 0..1 (0.5 for a human)

	OurStanding   float64 // what the viewing corporation's ledger says of it
	TheirStanding float64 // what it has declared about the viewer
	TenureSec     int64   // how long it has been in the alliance
	IsExecutor    bool
	ChosenExec    int64 // its declared executor support, 0 = secret
	RefusedTithes int   // tithes it refused the executor, recent
}

// allianceSupportPick is the member CEO's vote for executor support. Leadership weighs most, then size
// and wallet, then how the voter feels about it. The wallet term is 0.3 per tenfold over 1M ISK
// (allianceWalletWeight). The sitting executor keeps ExecutorIncumbency, so the seat moves on a real
// difference. A CEO may pick itself at a small discount. Ties go to the lower corpID.
func allianceSupportPick(selfCorp int64, members []allianceMemberFacts, incumbent int64, ch Charter) (pick int64, why string) {
	type cand struct {
		id    int64
		score float64
		why   string
	}
	var cands []cand
	for _, f := range members {
		s := 2*f.Lead + 0.3*math.Log1p(float64(f.Pilots)) + allianceWalletWeight(f.Wallet) + f.OurStanding/10
		parts := fmt.Sprintf("leadership %.2f, %d pilots, %s wallet, standing %+.1f", f.Lead, f.Pilots, iskString(f.Wallet), f.OurStanding)
		if f.CorpID == incumbent && incumbent != 0 {
			s += ch.ExecutorIncumbency
			parts += fmt.Sprintf(", sitting executor +%.1f", ch.ExecutorIncumbency)
		}
		if f.CorpID == selfCorp {
			s -= 0.5
			parts += ", ourselves −0.5"
		}
		cands = append(cands, cand{f.CorpID, s, parts})
	}
	if len(cands) == 0 {
		return 0, "no members"
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].id < cands[j].id
	})
	return cands[0].id, cands[0].why
}

// allianceWalletWeight is the support vote's money term: 0.3 per tenfold of the wallet over 1M ISK,
// 0 at or under 1M.
func allianceWalletWeight(wallet float64) float64 {
	return 0.3 * math.Log10(math.Max(wallet, 1e6)/1e6)
}

// allianceMemberView is what a member CEO weighs when it asks itself whether
// to stay.
type allianceMemberView struct {
	IsExecutor        bool
	ExecStandingToUs  float64 // the executor's view of us
	ExecSouredSince   int64   // when that view fell under RivalAt; 0 = not known
	Now               int64
	RefusedTithes     int // tithes we have refused: we cannot afford this bloc
	AllianceWars      int
	TithesOwed        float64 // what we still owe the executor
	TitheOwedSinceSec int64   // how long the oldest unpaid tithe has stood
}

// allianceLeaveVerdict says whether a member walks, and why. The executor never leaves. A member leaves
// when the executor has held it a rival for DwellSec x AllianceLoyalty, when it has refused more tithes
// than its TitheTolerance, or when a pacifist charter finds the alliance in more wars than it would fight.
func allianceLeaveVerdict(v allianceMemberView, ch Charter) (leave bool, why string) {
	if v.IsExecutor {
		return false, ""
	}
	if v.ExecStandingToUs < ch.RivalAt {
		wait := int64(float64(ch.DwellSec) * (1 + ch.AllianceLoyalty))
		if v.ExecSouredSince == 0 || v.Now-v.ExecSouredSince >= wait {
			return true, fmt.Sprintf("the executor holds us at %+.1f, under %+.1f", v.ExecStandingToUs, ch.RivalAt)
		}
	}
	if v.RefusedTithes > ch.TitheTolerance {
		return true, fmt.Sprintf("refused %d tithes; cannot carry this alliance's bills", v.RefusedTithes)
	}
	if ch.WarAt >= 8 && v.AllianceWars > ch.MaxWars {
		return true, fmt.Sprintf("the alliance is in %d wars; we would fight at most %d", v.AllianceWars, ch.MaxWars)
	}
	return false, ""
}

// allianceApplicantFacts is what an executor reads about a corporation at
// its door.
type allianceApplicantFacts struct {
	CorpID        int64
	Name          string
	Pilots        int64
	Human         bool // a human CEO (no managed bot in the chair)
	MutualAlly    bool // a mutual ally (relAlliesOf) of any member corporation, the executor included
	OurStanding   float64
	TheirStanding float64
	AtWar         bool // with the alliance or any member
}

// allianceApplicantScore is the executor's answer. A mutual ally is let in; anyone else must score at
// least FriendAt (the line for proposing a pact) from standing, size and its own view of us. Human
// corporations are weighed the same way; one at war with the alliance is refused.
func allianceApplicantScore(a allianceApplicantFacts, ch Charter) (accept bool, score float64, why string) {
	if a.AtWar {
		return false, 0, "at war with the alliance"
	}
	score = a.OurStanding + 0.5*math.Log1p(float64(a.Pilots)) + 0.25*a.TheirStanding
	if a.MutualAlly {
		score += 3
		return true, score, fmt.Sprintf("a mutual ally (%+.1f, %d pilots)", a.OurStanding, a.Pilots)
	}
	if score >= ch.FriendAt {
		return true, score, fmt.Sprintf("scores %.1f against our %.1f bar (%d pilots, standing %+.1f)", score, ch.FriendAt, a.Pilots, a.OurStanding)
	}
	return false, score, fmt.Sprintf("scores %.1f, under our %.1f bar", score, ch.FriendAt)
}

// allianceKickVerdict is the executor's view of a member it may show the
// door: one that has refused more tithes than the executor's own
// TitheTolerance, or one the executor's ledger has come to despise.
func allianceKickVerdict(f allianceMemberFacts, ch Charter) (kick bool, why string) {
	if f.IsExecutor {
		return false, ""
	}
	if f.RefusedTithes > ch.TitheTolerance {
		return true, fmt.Sprintf("refused %d tithes", f.RefusedTithes)
	}
	if f.OurStanding <= ch.EnemyAt {
		return true, fmt.Sprintf("held at %+.1f, an enemy", f.OurStanding)
	}
	return false, ""
}

// allianceFoundAppetite is the AllianceAppetite of a leader's bloc: the founder's, and every ally that
// puts money into the founding.
const allianceFoundAppetite = 0.5

// allianceFunder is one corporation of a founding cluster and the free capital it could put in.
type allianceFunder struct {
	CorpID int64
	Free   float64
}

// allianceCoFund splits CONCORD's fee over a founding cluster: self and its allies of appetite
// allianceFoundAppetite or more. The richest member founds (ties to the lower corpID) and must hold a
// third of the fee itself; the cluster together must hold the whole fee. Each member pays in proportion
// to its free capital, whole ISK, the remainder to the founder. shares is what each ally pays the
// founder before the fee is charged; the founder's own part is fee minus their sum. total is the
// cluster's free capital. Pure.
func allianceCoFund(self int64, cluster []allianceFunder, fee float64) (shares map[int64]float64, total float64, ok bool, why string) {
	var selfFree float64
	var richest allianceFunder
	inCluster := false
	for _, f := range cluster {
		if f.Free <= 0 {
			continue
		}
		total += f.Free
		if f.CorpID == self {
			selfFree, inCluster = f.Free, true
		}
		if richest.CorpID == 0 || f.Free > richest.Free || (f.Free == richest.Free && f.CorpID < richest.CorpID) {
			richest = f
		}
	}
	switch {
	case !inCluster:
		return nil, total, false, "no free capital of its own"
	case richest.CorpID != self:
		return nil, total, false, fmt.Sprintf("an ally holds more (%s against our %s); the richest founds", iskString(richest.Free), iskString(selfFree))
	case selfFree*3 < fee:
		return nil, total, false, fmt.Sprintf("%s free, under a third of the %s fee", iskString(selfFree), iskString(fee))
	case total < fee:
		return nil, total, false, fmt.Sprintf("%s free across the cluster against a %s fee", iskString(total), iskString(fee))
	}
	shares = map[int64]float64{}
	for _, f := range cluster {
		if f.CorpID == self || f.Free <= 0 {
			continue
		}
		if s := math.Floor(fee * f.Free / total); s > 0 {
			shares[f.CorpID] = s
		}
	}
	return shares, total, true, fmt.Sprintf("%s free across %d corporations", iskString(total), len(shares)+1)
}

// allianceFoundVerdict says whether a CEO founds an alliance: a leader (appetite allianceFoundAppetite or
// more) with at least two mutual allies to fill it, no war of its own in progress (the core refuses a
// founder with declared wars), and the fee co-funded by its cluster (allianceCoFund). shares is what each
// ally pays in. Pure; the pace and the cap are the caller's.
func allianceFoundVerdict(appetite float64, mutualAllies int, self int64, cluster []allianceFunder, fee float64, declaredWars int) (ok bool, why string, shares map[int64]float64) {
	switch {
	case appetite < allianceFoundAppetite:
		return false, fmt.Sprintf("appetite %.2f, under %.2f", appetite, allianceFoundAppetite), nil
	case mutualAllies < 2:
		return false, fmt.Sprintf("%d mutual allies, fewer than 2", mutualAllies), nil
	case declaredWars > 0:
		return false, fmt.Sprintf("%d declared wars in progress", declaredWars), nil
	}
	shares, _, ok, fundWhy := allianceCoFund(self, cluster, fee)
	if !ok {
		return false, fundWhy, nil
	}
	return true, fmt.Sprintf("appetite %.2f, %d mutual allies, %s", appetite, mutualAllies, fundWhy), shares
}

// titheShares splits a sum the executor fronted: the executor takes a third as the price of the seat
// and the rest is shared by headcount across every member, executor included. Whole ISK, remainder to
// the executor. A member with no pilots counts one.
func titheShares(members []allianceMemberFacts, executor int64, total float64) map[int64]float64 {
	out := map[int64]float64{}
	if total <= 0 || len(members) == 0 {
		return out
	}
	var heads float64
	for _, f := range members {
		heads += math.Max(1, float64(f.Pilots))
	}
	shared := total * 2 / 3
	var dealt float64
	for _, f := range members {
		if f.CorpID == executor {
			continue
		}
		s := math.Floor(shared * math.Max(1, float64(f.Pilots)) / heads)
		out[f.CorpID] = s
		dealt += s
	}
	out[executor] = math.Round(total - dealt)
	return out
}

// allianceHealthIn is what the panel's one-line verdict reads.
type allianceHealthIn struct {
	Members        int
	HasExecutor    bool
	BotExecutor    bool
	Disputed       int // members whose declared support is for somebody else
	BelowRival     int // members the executor holds under its RivalAt
	RefusedTithes  int
	ActiveWars     int
	SovSystems     int
	PendingApps    int
	TowersOnline   int
}

// allianceHealth sums a bloc up in a word and a reason: dormant (nobody
// runs it), fracturing (the seat is disputed or the executor has turned on
// its members), straining (tithes refused), at war, growing (applications
// waiting), or cohesive.
func allianceHealth(in allianceHealthIn) (label, why string) {
	switch {
	case in.Members == 0:
		return "empty", "no member corporations"
	case !in.HasExecutor:
		return "headless", "no executor corporation"
	case !in.BotExecutor && in.Members == 1:
		return "dormant", "one capsuleer-run corporation, nothing for the bots to do"
	case in.Disputed*2 > in.Members || in.BelowRival >= 2:
		return "fracturing", fmt.Sprintf("%d of %d dispute the executor; %d held as rivals", in.Disputed, in.Members, in.BelowRival)
	case in.RefusedTithes > in.Members:
		return "straining", fmt.Sprintf("%d tithes refused across %d corporations", in.RefusedTithes, in.Members)
	case in.ActiveWars > 0:
		return "at war", fmt.Sprintf("%d wars, %d systems held, %d towers online", in.ActiveWars, in.SovSystems, in.TowersOnline)
	case in.PendingApps > 0:
		return "growing", fmt.Sprintf("%d corporations, %d applications waiting", in.Members, in.PendingApps)
	}
	return "cohesive", fmt.Sprintf("%d corporations, %d refused tithes, %d systems held", in.Members, in.RefusedTithes, in.SovSystems)
}

// sovSeekAppetite is the AllianceAppetite at which a corporation reaching for unclaimed 0.0 seeks an
// alliance (allianceSeek), under the ordinary allianceSeekAppetite; sovSeekPull is how many allies more
// an alliance founded to hold 0.0 counts for it.
const (
	sovSeekAppetite = 0.25
	sovSeekPull     = 2
	// sovFoundAppetite is the AllianceAppetite an alliance founded to hold 0.0 needs, under the 0.50 of a
	// leader's bloc; sovFoundTowerAllies is how many of the founder's allies must already have a foot
	// on the frontier (a mining tower or a forward office).
	sovFoundAppetite    = 0.35
	sovFoundTowerAllies = 1
)

// sovFoundVerdict says whether a CEO founds an alliance to hold 0.0: some appetite for an alliance
// (sovFoundAppetite), a tower appetite of frontierAppetite, two mutual allies of whom
// sovFoundTowerAllies already have a foot on the frontier, the founder too (ready), the fee and no war
// of its own. Pure; the pace and the cap are the caller's.
func sovFoundVerdict(appetite, posAppetite float64, mutualAllies, towerAllies int, ready bool,
	chest, fee float64, declaredWars int) (ok bool, why string) {
	switch {
	case appetite < sovFoundAppetite:
		return false, fmt.Sprintf("appetite %.2f, under %.2f", appetite, sovFoundAppetite)
	case posAppetite < frontierAppetite:
		return false, fmt.Sprintf("tower appetite %.2f, under %.2f", posAppetite, frontierAppetite)
	case !ready:
		return false, "no mining tower or forward office of its own"
	case mutualAllies < 2:
		return false, fmt.Sprintf("%d mutual allies, fewer than 2", mutualAllies)
	case towerAllies < sovFoundTowerAllies:
		return false, "no ally with a foot on the frontier"
	case declaredWars > 0:
		return false, fmt.Sprintf("%d declared wars in progress", declaredWars)
	case chest < fee:
		return false, fmt.Sprintf("%s free against a %s fee", iskString(chest), iskString(fee))
	}
	return true, fmt.Sprintf("to hold 0.0: tower appetite %.2f, %d mutual allies (%d on the frontier), %s free",
		posAppetite, mutualAllies, towerAllies, iskString(chest))
}
