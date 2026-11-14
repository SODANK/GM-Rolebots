package rolebots

import (
	"context"
	"sort"

	"gurimoon/esp"
)

// What the Corporate Portal's Overview and Operations pages read about a
// corporation's people, gathered once per request: every member with their
// department, whether they are present, and the fleet they sit in. Both
// pages answer "who does what", and two readers of the same question must
// not disagree about who is in Mining.

// portalMember is one member as the portal draws them.
type portalMember struct {
	corpMember
	Arm     corpArm // "" = in no department
	Tier    int
	Present bool
	Session esp.Session
	Fleet   string
}

// InSpace reports a present member undocked.
func (pm portalMember) InSpace() bool {
	return pm.Present && pm.Session.StationID == 0 && pm.Session.SolarSystemID != 0
}

// portalRoster is a corporation's people for one page.
type portalRoster struct {
	CorpID  int64
	CEOID   int64
	Members []portalMember
	ByArm   map[corpArm][]portalMember
	Present map[int64]esp.Session
	Fleets  []*Gang
}

// portalArmOrder is the order departments are drawn in: the line arms, the
// desk arms, then the members in none.
var portalArmOrder = []corpArm{armIndustry, armResearch, armMining, armSecurity, armSales, armInfrastructure, armHR, armFinance, ""}

// portalRosterFor reads a corporation's people. Best effort: a part that
// fails to read is left empty.
func (m *Mod) portalRosterFor(ctx context.Context, corpID int64) *portalRoster {
	pr := &portalRoster{CorpID: corpID, CEOID: m.corpCEOID(ctx, corpID), ByArm: map[corpArm][]portalMember{}}
	members, _ := m.corpMembers(ctx, corpID, 0)
	arms, _ := m.loadArmRows(ctx, corpID)
	positions := m.armPositions(ctx, corpID)
	pr.Present = m.corpPresence(corpID)
	pr.Fleets = m.corpFleets(ctx, corpID)
	fleetOf := map[int64]string{}
	for _, g := range pr.Fleets {
		mems, _ := gangMembers(ctx, m.db, g.GangID)
		for _, mem := range mems {
			fleetOf[mem.CharacterID] = g.Name
		}
	}
	for _, cm := range members {
		pm := portalMember{corpMember: cm, Fleet: fleetOf[cm.CharID]}
		if r, ok := arms[cm.CharID]; ok {
			pm.Arm, pm.Tier = r.Arm, r.Tier
		}
		// A position places its holder whatever the row says: the head of a
		// department is in it.
		if p, ok := positions[cm.CharID]; ok && p.Arm != "" && (pm.Arm == "" || p.Tier > pm.Tier) {
			pm.Arm, pm.Tier = p.Arm, p.Tier
		}
		if s, ok := pr.Present[cm.CharID]; ok {
			pm.Present, pm.Session = true, s
		}
		pr.Members = append(pr.Members, pm)
	}
	sortPortalMembers(pr.Members)
	for _, pm := range pr.Members {
		pr.ByArm[pm.Arm] = append(pr.ByArm[pm.Arm], pm)
	}
	return pr
}

// sortPortalMembers orders a list the way every panel reads it: heads first,
// then present before absent, in space before docked, then by name. Pure.
func sortPortalMembers(ms []portalMember) {
	sort.SliceStable(ms, func(i, j int) bool {
		a, b := ms[i], ms[j]
		if a.Tier != b.Tier {
			return a.Tier > b.Tier
		}
		if a.Present != b.Present {
			return a.Present
		}
		if a.InSpace() != b.InSpace() {
			return a.InSpace()
		}
		return a.Name < b.Name
	})
}

// member finds one member.
func (pr *portalRoster) member(charID int64) (portalMember, bool) {
	for _, pm := range pr.Members {
		if pm.CharID == charID {
			return pm, true
		}
	}
	return portalMember{}, false
}

// onDuty counts a list's present members, and those in space.
func onDuty(ms []portalMember) (present, inSpace int) {
	for _, pm := range ms {
		if pm.Present {
			present++
			if pm.InSpace() {
				inSpace++
			}
		}
	}
	return present, inSpace
}

// head is a department's head: its member with the highest tier, if that
// tier is a head's.
func armHeadOf(ms []portalMember) (portalMember, bool) {
	for _, pm := range ms {
		if pm.Tier == armTierHead {
			return pm, true
		}
	}
	return portalMember{}, false
}

// corpJobCounts is a corporation's jobs in the factory right now:
// manufacturing, and research of any kind (ME, PE, copying, reverse
// engineering, invention).
func (m *Mod) corpJobCounts(ctx context.Context, corpID int64) (mfg, research int) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(activityID = 1), 0), COALESCE(SUM(activityID IN (3, 4, 5, 7, 8)), 0)
		  FROM ramJobs WHERE ownerID = ? AND completedStatusID IN (0, 1)`, corpID).Scan(&mfg, &research)
	return mfg, research
}

// officesByPurpose is the corporation's office stations per purpose bit, as
// the estate pass last stamped them (rolebot_corp_offices).
func (m *Mod) officesByPurpose(ctx context.Context, corpID int64) map[int64]int {
	out := map[int64]int{}
	rows, err := m.db.QueryContext(ctx,
		`SELECT stationID, purposes FROM rolebot_corp_offices WHERE corporationID = ?`, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st int64
		var bits int
		if rows.Scan(&st, &bits) == nil {
			out[st] = bits
		}
	}
	return out
}

// memberDoing is a member's work in a few words: a driven pilot's task, a
// capsuleer's hull.
func (m *Mod) memberDoing(ctx context.Context, pm portalMember) string {
	if !pm.Present {
		return "off duty"
	}
	if pm.OnRoster {
		if st, err := loadState(ctx, m.db, pm.CharID); err == nil && st != nil {
			return phaseLabel(st)
		}
		return "about"
	}
	if pm.Session.StationID != 0 {
		return "docked"
	}
	_, hull := m.shipTypeOf(ctx, pm.CharID)
	return "flying a " + hull
}
