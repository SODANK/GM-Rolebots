package rolebots

// Alliance relationships: the executor rolls its members' ledgers up into the four declarations the client
// knows (db/alliancedb.go: NAP, Friend, Competitor, Enemy, shown as overview colours).
//
// The rule is a pilot-weighted mean of the member corporations' ledgers about a target, read against the
// executor's charter lines: AllyAt and above is Friend, FriendAt a NAP, RivalAt and below Competitor,
// EnemyAt and below Enemy. An allied target collapses onto its alliance. Hysteresis stops flapping. The
// executor's MaxAllies+MaxFriends cap the friendly names and 2*MaxRivals the hostile ones; current
// declarations fill each cap first, then friends by highest mean and rivals by lowest, so a full list does
// not swap names from one pass to the next. allianceRelRollup is pure; the result is applied
// through evedb.SetAllianceRelationship with the push.

import (
	"context"
	"fmt"
	"sort"

	evedb "gurimoon/db"
)

// allianceRelView is one member's view of one target: the ledger value and
// the member's weight.
type allianceRelView struct {
	Member, Target int64
	Value          float64
	Pilots         int64
}

// allianceRelChange is one declaration to write (Rel 0 = delete).
type allianceRelChange struct {
	ToID int64
	Rel  int64
	Why  string
}

// allianceRelRollup folds the members' views into declarations. targetAlly
// maps a target corporation onto its alliance (0 for none); current is what
// the alliance declares today.
func allianceRelRollup(views []allianceRelView, targetAlly map[int64]int64, current map[int64]int64, ch Charter) []allianceRelChange {
	type agg struct {
		sum, weight float64
		members     int
	}
	by := map[int64]*agg{}
	for _, v := range views {
		to := v.Target
		if a := targetAlly[v.Target]; a != 0 {
			to = a
		}
		w := float64(v.Pilots)
		if w < 1 {
			w = 1
		}
		a := by[to]
		if a == nil {
			a = &agg{}
			by[to] = a
		}
		a.sum += v.Value * w
		a.weight += w
		a.members++
	}
	// Each target's declaration by the lines, hysteresis holding a current one; then friends and rivals
	// apart, each side with what is declared today first (it held within hysteresis), then the strongest
	// view: friends by highest mean, rivals by lowest (most hostile first). The caps cut the tail.
	type scored struct {
		to   int64
		mean float64
		rel  int64
		held bool // already declared on this side
	}
	var friendList, rivalList []scored
	for to, a := range by {
		s := scored{to: to, mean: a.sum / a.weight}
		cur := current[to]
		switch {
		case s.mean >= ch.AllyAt || (cur == evedb.AllianceRelationshipFriend && s.mean >= ch.AllyAt-ch.Hysteresis):
			s.rel = evedb.AllianceRelationshipFriend
		case s.mean >= ch.FriendAt || (cur == evedb.AllianceRelationshipNAP && s.mean >= ch.FriendAt-ch.Hysteresis):
			s.rel = evedb.AllianceRelationshipNAP
		case s.mean <= ch.EnemyAt || (cur == evedb.AllianceRelationshipEnemy && s.mean <= ch.EnemyAt+ch.Hysteresis):
			s.rel = evedb.AllianceRelationshipEnemy
		case s.mean <= ch.RivalAt || (cur == evedb.AllianceRelationshipCompetitor && s.mean <= ch.RivalAt+ch.Hysteresis):
			s.rel = evedb.AllianceRelationshipCompetitor
		default:
			continue
		}
		if allianceRelFriendly(s.rel) {
			s.held = allianceRelFriendly(cur)
			friendList = append(friendList, s)
		} else {
			s.held = allianceRelHostile(cur)
			rivalList = append(rivalList, s)
		}
	}
	rank := func(list []scored, hostile bool) {
		sort.Slice(list, func(i, j int) bool {
			if list[i].held != list[j].held {
				return list[i].held
			}
			if list[i].mean != list[j].mean {
				return (list[i].mean < list[j].mean) == hostile
			}
			return list[i].to < list[j].to
		})
	}
	rank(friendList, false)
	rank(rivalList, true)
	if n := ch.MaxAllies + ch.MaxFriends; len(friendList) > n {
		friendList = friendList[:max(n, 0)]
	}
	if n := 2 * ch.MaxRivals; len(rivalList) > n {
		rivalList = rivalList[:max(n, 0)]
	}
	want := map[int64]int64{}
	why := map[int64]string{}
	for _, s := range append(friendList, rivalList...) {
		want[s.to] = s.rel
		why[s.to] = fmt.Sprintf("the members hold them at %+.1f on average", s.mean)
	}
	var out []allianceRelChange
	for to, rel := range want {
		if current[to] != rel {
			out = append(out, allianceRelChange{ToID: to, Rel: rel, Why: why[to]})
		}
	}
	for to := range current {
		if _, keep := want[to]; !keep {
			out = append(out, allianceRelChange{ToID: to, Rel: 0, Why: "the members no longer hold a view worth declaring"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ToID < out[j].ToID })
	return out
}

// allianceRelFriendly reports a Friend or NAP declaration.
func allianceRelFriendly(rel int64) bool {
	return rel == evedb.AllianceRelationshipFriend || rel == evedb.AllianceRelationshipNAP
}

// allianceRelHostile reports a Competitor or Enemy declaration.
func allianceRelHostile(rel int64) bool {
	return rel == evedb.AllianceRelationshipEnemy || rel == evedb.AllianceRelationshipCompetitor
}

// allianceRollupRelations is the executor's pass: read every bot member's
// ledger, fold, write what changed.
func (m *Mod) allianceRollupRelations(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) error {
	if !relationsEnabled() || !relLedgerOn() {
		return nil
	}
	if m.humanHoldOn(ctx, c.CorpID, "allrel") {
		return nil
	}
	// Once per DwellSec: a relationship is a declaration every pilot's
	// overview repaints on, not a number to chase.
	if now := clock.Unix(); now-m.lastCEOActionAt(ctx, c.CorpID, "allrel") < ch.DwellSec {
		return nil
	}
	memberSet := map[int64]bool{}
	for _, f := range members {
		memberSet[f.CorpID] = true
	}
	var views []allianceRelView
	targetAlly := map[int64]int64{}
	for _, f := range members {
		if f.HumanCEO {
			continue // a human corporation's view is its own declared standing, not a ledger
		}
		rows, err := m.relRowsOf(ctx, f.CorpID)
		if err != nil {
			continue
		}
		for to, s := range rows {
			if memberSet[to] || s.Yielded {
				continue
			}
			views = append(views, allianceRelView{Member: f.CorpID, Target: to, Value: s.Value, Pilots: f.Pilots})
			if _, seen := targetAlly[to]; !seen {
				targetAlly[to] = m.corpAllianceID(ctx, to)
			}
		}
	}
	delete(targetAlly, seat.AllianceID)
	for to, a := range targetAlly {
		if a == seat.AllianceID {
			delete(targetAlly, to) // an alliance-mate seen through a stale row
		}
	}
	current := map[int64]int64{}
	if rels, err := evedb.ListAllianceRelationships(ctx, m.db, seat.AllianceID); err == nil {
		for _, r := range rels {
			current[r.ToID] = r.Relationship
		}
	}
	changes := allianceRelRollup(views, targetAlly, current, ch)
	if len(changes) == 0 {
		return nil
	}
	for _, chg := range changes {
		if chg.ToID == seat.AllianceID {
			continue
		}
		name := ownerNameOf(ctx, m.db, chg.ToID)
		if chg.Rel == 0 {
			if err := evedb.DeleteAllianceRelationship(ctx, m.db, seat.AllianceID, chg.ToID); err != nil {
				continue
			}
			m.allianceChanged("rel-del", seat.AllianceID, chg.ToID)
			m.ceoLog(ctx, c, powerAlliance, "allrel", chg.ToID, 0, "withdrew %s's declaration on %s — %s", seat.Name, name, chg.Why)
			m.allianceBook(ctx, seat.AllianceID, c.CorpID, "rel-del", chg.ToID, 0, "%s withdrew its declaration on %s", seat.Name, name)
			continue
		}
		if err := evedb.SetAllianceRelationship(ctx, m.db, seat.AllianceID, chg.ToID, chg.Rel); err != nil {
			m.logBot(c.CharID, logInfo, "ceo", "alliance relationship %d: %v", chg.ToID, err)
			continue
		}
		label := evedb.AllianceRelationshipName(chg.Rel)
		m.allianceChanged("rel-set", seat.AllianceID, chg.ToID)
		m.ceoLog(ctx, c, powerAlliance, "allrel", chg.ToID, float64(chg.Rel), "declared %s a %s of %s — %s", name, label, seat.Name, chg.Why)
		m.allianceBook(ctx, seat.AllianceID, c.CorpID, "rel-set", chg.ToID, float64(chg.Rel), "%s names %s: %s — %s", seat.Name, name, label, chg.Why)
	}
	m.refreshRelations(ctx)
	return nil
}
