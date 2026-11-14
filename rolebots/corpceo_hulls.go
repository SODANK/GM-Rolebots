package rolebots

import (
	"context"
	"fmt"
)

// What a corporation can actually put on the field, read from the hulls its
// members are sitting in rather than from the trade tags on its charter.
//
// A corporation tagged "trade" with no armed tag was called soft, and a war
// declared on it for that reason (ceoWarMotive's predation term) was priced as a
// war on 25% of its members. Hexcraft is tagged that way and its fifty pilots
// all fly armed Amarr frigates; Rancid Warband declared on it with a combat wing
// of eight. The tags say what a corporation does for a living; the hulls say what
// it can shoot with.

// warSoftHullFrac is the share of a corporation's members flying armed hulls
// under which it still reads as soft prey.
const warSoftHullFrac = 0.25

// hullCount is how many of a corporation's pilots sit in an armed combat hull,
// out of the pilots with any hull at all.
type hullCount struct {
	Armed, Boarded int
}

// armedHullClass is whether a hull class is a gunboat: the five combat steps.
// Rookie ships, shuttles, industrials, barges, freighters and capitals are not.
func armedHullClass(c HullClass) bool {
	switch c {
	case ClassFrigate, ClassDestroyer, ClassCruiser, ClassBattlecruiser, ClassBattleship:
		return true
	}
	return false
}

// hullsArmed is whether enough of a corporation's members fly gunboats that it
// is not soft prey whatever its tags say (warSoftHullFrac). Pure.
func hullsArmed(hc hullCount, members int) bool {
	if members <= 0 {
		return false
	}
	return float64(hc.Armed) >= warSoftHullFrac*float64(members)
}

// hullCountFromGroups folds a count of pilots per ship group into a hullCount.
// A group the hull table does not know (a capsule, say) is a boarded pilot that
// is not armed. Pure.
func hullCountFromGroups(byGroup map[int64]int) hullCount {
	var hc hullCount
	for groupID, n := range byGroup {
		hc.Boarded += n
		if armedHullClass(hullClassByGroup[groupID]) {
			hc.Armed += n
		}
	}
	return hc
}

// corpArmedHulls counts armed hulls per corporation in one query. With wingOnly
// it counts only the pilots a corporation could send to fight: those on a
// combat footing (rolebot activity "combat") and any human member, whose
// activity is their own business.
func (m *Mod) corpArmedHulls(ctx context.Context, corpIDs []int64, wingOnly bool) (map[int64]hullCount, error) {
	out := map[int64]hullCount{}
	if len(corpIDs) == 0 {
		return out, nil
	}
	wing := ""
	if wingOnly {
		wing = ` AND (s.activity = 'combat' OR s.characterID IS NULL)`
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.corporationID, t.groupID, COUNT(*)
		  FROM character_ c
		  JOIN entity ce ON ce.itemID = c.characterID
		  JOIN entity sh ON sh.itemID = ce.locationID
		  JOIN invTypes t ON t.typeID = sh.typeID
		  LEFT JOIN rolebot_state s ON s.characterID = c.characterID
		 WHERE c.accountID > 0 AND c.corporationID IN (`+placeholders(len(corpIDs))+`)`+wing+`
		 GROUP BY c.corporationID, t.groupID`, int64Args(corpIDs)...)
	if err != nil {
		return nil, fmt.Errorf("corpArmedHulls: %w", err)
	}
	defer rows.Close()
	byCorp := map[int64]map[int64]int{}
	for rows.Next() {
		var corpID, groupID int64
		var n int
		if err := rows.Scan(&corpID, &groupID, &n); err != nil {
			return nil, fmt.Errorf("corpArmedHulls scan: %w", err)
		}
		if byCorp[corpID] == nil {
			byCorp[corpID] = map[int64]int{}
		}
		byCorp[corpID][groupID] += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for corpID, groups := range byCorp {
		out[corpID] = hullCountFromGroups(groups)
	}
	return out, nil
}

// warStrengthHulls is warStrength from hulls: the fighters count in full and the
// rest of the members at the soft fraction, scaled by killboard efficiency
// (eff below 0 reads as middling). Pure.
func warStrengthHulls(fighters, members int, eff float64) float64 {
	if fighters < 0 {
		fighters = 0
	}
	if members < fighters {
		members = fighters
	}
	if members == 0 {
		return 0
	}
	if eff < 0 {
		eff = 0.5
	}
	return (float64(fighters) + warSoftFrac*float64(members-fighters)) * (0.5 + eff)
}

// corpFightStrength is one corporation's fighting weight from its hulls. wing
// selects the pilots it could send (its own strength) over every armed hull in
// it (what an enemy has to face). A failed read falls back to the tag-based
// warStrength rather than reading as no strength at all.
func (m *Mod) corpFightStrength(ctx context.Context, corpID int64, members int, wing, tagArmed bool, eff float64) float64 {
	hulls, err := m.corpArmedHulls(ctx, []int64{corpID}, wing)
	if err != nil {
		return warStrength(members, tagArmed, eff)
	}
	return warStrengthHulls(hulls[corpID].Armed, members, eff)
}

// warOutmatched reports whether a corporation is so far short of an enemy that a
// war it has not yet hurt should be called off: their strength past
// maxOdds x warOutmatchedMul of its own. A corporation that can field nothing is
// outmatched by anyone who can field anything. Pure.
func warOutmatched(ours, theirs, maxOdds float64) bool {
	if theirs <= 0 {
		return false
	}
	return theirs > ours*maxOdds*warOutmatchedMul
}

// warOutmatchedMul is the slack past WarMaxOdds before a running war is judged
// lost. Declaring takes the charter's odds as they are; an existing war is only
// called off when it is clearly worse than that.
const warOutmatchedMul = 1.5
