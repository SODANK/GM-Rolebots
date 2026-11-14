package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Escorts: the one place a fleet recruits outside its own corporation. gangRecruit takes
// the commander's own corporation only, which is right for working seats, but a mining
// corp with no combat pilot would field an op with no guns. This second pass is narrow:
//   - Combat seats only. Nothing else is recruited outside the corp.
//   - A corp of the same bloc (employer faction or alliance, kinship.go) or a mutual
//     ally, never across the lawful/outlaw line in either direction.
//   - Both pilots willing: standings are read from each side and either can refuse.
//
// An answering pilot joins as an ordinary member (RoleEscort, with recruitCorpID recording
// who lent it). The welcome is re-checked every plan pass (gangEscortStillWelcome).

const (
	// escortMaxPerCorp is how many pilots one lending corporation gives to one fleet. A
	// mining op asks for one gun at most (escortQuota), and stripping a security outfit to
	// escort mining ops helps nobody.
	escortMaxPerCorp = 1
)

// escortQuota is how many guns this fleet may go looking for, given what its own
// corporation put in the seats. A mining op asks only when its own corp could not fill
// its combat seat. Combat fleets ask for none: a hunting pack is already guns, and a
// pirate raid does not borrow lawful ones.
func escortQuota(activity string, recruits []gangRecruitPick, capacity int) int {
	var want int
	switch activity {
	case ActMining, ActIndMining:
		for _, r := range recruits {
			if r.role == RoleCombat {
				return 0 // its own corp covered it
			}
		}
		want = 1
	default:
		return 0
	}
	// Escorts are real members and spend the same Leadership capacity the
	// working seats do.
	if room := capacity - 1 - len(recruits); want > room {
		want = room
	}
	if want < 0 {
		return 0
	}
	return want
}

// escortCorpEligible is the whole diplomatic test on a lending corporation. Pure over the
// standings matrix. allies is the hirer's mutual allies (relAlliesOf), the one door into
// the pool that is not a bloc: corporations that each declared the other an ally lend
// guns the way an alliance does.
func escortCorpEligible(c CorpFacts, hiringCorpID, hiringFaction, hiringAlliance int64,
	hiringOutlaw bool, allies map[int64]bool) bool {

	if c.CorpID == hiringCorpID || c.School {
		return false
	}
	if !c.Policy.HasTag("combat") && !c.Policy.HasTag("security") {
		return false
	}
	// The lawful/outlaw line, symmetric in both directions: a mining corp does
	// not hire pirates to guard it, and a cartel does not hire the navy.
	if c.Policy.HasTag("outlaw") != hiringOutlaw {
		return false
	}
	// One bloc and nothing wider (kinship.go): same faction or same alliance. A player corp
	// has no faction, so it relies on the alliance clause or declared allies.
	return sameBloc(hiringCorpID, hiringAlliance, hiringFaction,
		c.CorpID, c.AllianceID, c.FactionID) || allies[c.CorpID]
}

// relAllySet is relAlliesOf as a set, nil when there are none.
func relAllySet(corpID int64) map[int64]bool {
	list := relAlliesOf(corpID)
	if len(list) == 0 {
		return nil
	}
	out := make(map[int64]bool, len(list))
	for _, id := range list {
		out[id] = true
	}
	return out
}

// gangRecruitEscorts is the second pass: guns from somebody else's payroll.
func (m *Mod) gangRecruitEscorts(ctx context.Context, lb *botRun, want int) ([]gangRecruitPick, error) {
	if want <= 0 {
		return nil, nil
	}
	hiringFaction := m.corpFactionID(ctx, lb.CorpID)
	hiringAlliance := m.corpAllianceID(ctx, lb.CorpID)
	allies := relAllySet(lb.CorpID)
	if hiringFaction == 0 && hiringAlliance == 0 && len(allies) == 0 {
		// Nobody's kin: no faction, alliance or declared allies means no bloc to hire inside.
		return nil, nil
	}
	facts, err := listCorpFacts(ctx, m.db)
	if err != nil {
		return nil, fmt.Errorf("gangRecruitEscorts: %w", err)
	}
	var hiringOutlaw bool
	for _, c := range facts {
		if c.CorpID == lb.CorpID {
			hiringOutlaw = c.Policy.HasTag("outlaw")
			break
		}
	}
	lenders := map[int64]bool{}
	var ids []any
	for _, c := range facts {
		if escortCorpEligible(c, lb.CorpID, hiringFaction, hiringAlliance, hiringOutlaw, allies) {
			lenders[c.CorpID] = true
			ids = append(ids, c.CorpID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// Same shape as gangRecruit's own query, with the corp gate widened to the
	// lenders instead of removed.
	placeholders := ""
	for i := range ids {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
	}
	args := append([]any{lb.CharID}, ids...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT b.characterID, c.characterName, c.solarSystemID, c.corporationID,
		       COALESCE(s.phase, ''), COALESCE(s.data, '')
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		LEFT JOIN rolebot_gang_members gm ON gm.characterID = b.characterID
		LEFT JOIN rolebot_state s ON s.characterID = b.characterID
		WHERE b.enabled = 1 AND gm.characterID IS NULL
		  AND COALESCE(s.halted, 0) = 0 AND b.characterID <> ?
		  AND c.corporationID IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("gangRecruitEscorts: %w", err)
	}
	type cand struct {
		charID, systemID, corpID int64
		name                     string
		jumps                    int
		free                     bool
		standing                 float64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		var phase, blob string
		if rows.Scan(&c.charID, &c.name, &c.systemID, &c.corpID, &phase, &blob) != nil {
			continue
		}
		st := &SimState{Phase: phase}
		if blob != "" {
			_ = json.Unmarshal([]byte(blob), &st.Data)
		}
		c.free = interruptibleState(st)
		// An excused pilot skips gang duty until its memo lapses (gangSeatReady), so asking it
		// seats nobody.
		if st.Data.GangUnfitUntil > clock.Unix() {
			continue
		}
		cands = append(cands, c)
	}
	rows.Close()

	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	reach := gangReachMap(u, lb.SystemID, gangRecruitMaxJumps)

	kept := cands[:0]
	for _, c := range cands {
		j, inReach := reach[c.systemID]
		if !inReach || popIsOffline(c.charID) {
			continue
		}
		// Both directions: hiring is an agreement, so the commander's opinion of the candidate
		// and the candidate's opinion of the commander must both clear.
		if m.standingToPilot(ctx, lb.standingSelf(), c.charID) <= StandingDisliked {
			continue
		}
		mine := m.standingToPilot(ctx, m.escortSelf(ctx, c.charID, c.corpID), lb.CharID)
		if mine <= StandingDisliked {
			continue
		}
		c.jumps, c.standing = j, mine
		kept = append(kept, c)
	}
	// Who can break off and come first (the key the own-corp pass sorts on), then the
	// warmest opinion of the caller, then distance.
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].free != kept[j].free {
			return kept[i].free
		}
		if kept[i].standing != kept[j].standing {
			return kept[i].standing > kept[j].standing
		}
		return kept[i].jumps < kept[j].jumps
	})

	perCorp := map[int64]int{}
	var picks []gangRecruitPick
	for _, c := range kept {
		if len(picks) >= want {
			break
		}
		if perCorp[c.corpID] >= escortMaxPerCorp {
			continue
		}
		if ok, err := m.canWorkJob(ctx, c.charID, JobCombat); err != nil || !ok {
			continue
		}
		// The strict seat test every other pick gets (recruitSeatOK): an escort in a half-fitted
		// hull is excused on its first step and the op flies unguarded anyway.
		if !m.recruitSeatOK(ctx, c.charID, seatActivityFor(ActMining, RoleEscort), seatSpec{}) {
			continue
		}
		perCorp[c.corpID]++
		picks = append(picks, gangRecruitPick{
			charID: c.charID, name: c.name, role: RoleEscort,
			jumps: c.jumps, fromCorpID: c.corpID,
		})
	}
	return picks, nil
}

// escortSelf builds the judging side for a pilot the caller has not loaded a full botRun
// for: how that candidate's employer regards the commander's. The corp and its faction
// (corpFactionID is cached) are the whole question.
func (m *Mod) escortSelf(ctx context.Context, charID, corpID int64) standingSelf {
	return standingSelf{
		CharID:    charID,
		CorpID:    corpID,
		FactionID: m.corpFactionID(ctx, corpID),
	}
}

// ── flying the seat ─────────────────────────────────────────────────────────
// An escort on a mining op is routed to gangPlanPatrol (gangplan.go): the belts it
// sweeps are the belts the fleet is mining, so the seat needs no planner of its own.

// gangEscortStillWelcome re-checks the politics an escort was hired under and releases
// it when they changed: either corporation can leave the bloc, or the pilot can change
// employer mid-op (recruitCorpID is stored for that, and the live corp is re-asked). It
// pays what is owed, says so on both sides and lets it go. Reports whether it released.
func (m *Mod) gangEscortStillWelcome(ctx context.Context, b *botRun, g *Gang, mem *GangMember) (bool, error) {
	if mem == nil || mem.RecruitCorpID == 0 {
		return false, nil // not a hire; nothing to re-check
	}
	liveCorp := m.charCorpID(ctx, b.CharID)

	// The kinship gate the hire was made under (kinship.go), re-asked against the pilot's
	// current employer. A mutual alliance (relAllied) is the other door and is re-asked too.
	if sameBloc(g.CorpID, m.corpAllianceID(ctx, g.CorpID), m.corpFactionID(ctx, g.CorpID),
		liveCorp, m.corpAllianceID(ctx, liveCorp), m.corpFactionID(ctx, liveCorp)) ||
		relAllied(g.CorpID, liveCorp) {
		return false, nil
	}

	why := "its employer no longer flies with the fleet's bloc or its allies"
	if liveCorp != mem.RecruitCorpID {
		why = "it has changed employer since it was hired"
	}
	m.logBot(b.CharID, logInfo, "gang",
		"released from %q — %s", g.Name, why)
	m.logBot(g.LeaderCharID, logInfo, "gang",
		"stood a seconded pilot down — %s", why)
	if err := removeGangMember(ctx, m.db, g.GangID, b.CharID); err != nil {
		return false, err
	}
	b.State.Data.GangID, b.State.Data.GangBeltID = 0, 0
	return true, m.planSolo(ctx, b)
}
