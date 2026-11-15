package rolebots

import (
	"context"

	evedb "gurimoon/db"
)

// A station's doors: when a bot corporation owns a conquerable, its CEO's personality decides who docks
// (Charter.StationPolicy). A freeport is open to everyone, as CONCORD left it. Blues only lets in the
// owner's alliance and anyone the owner or its alliance stands positively toward (the core's
// OwnerStandingToward reads alliances); neutrals and reds stay out. The rule covers docking, office
// rental, cloning and the jump clone bay. The capture itself clears every rule (the core's
// ResetStationManagement), so a new owner starts as a freeport until its CEO's first pass.

const (
	stationFreeport = "freeport"
	stationBlues    = "blues"
	// stationClosedLine is the closedness score above which a CEO keeps blues only.
	stationClosedLine = 0.25
	// stationBlueMinStanding is the least standing a blues-only door admits: anything positive.
	stationBlueMinStanding = 0.01
	// stationPolicyEverySec is how often a standing policy is written again.
	stationPolicyEverySec = 7 * 86400
)

// stationClosedScore is how closed a CEO keeps a station: xenophobia, a violent pole and a leader pole
// close it; a greedy pole opens it for trade. Each input is 0..1. Pure.
func stationClosedScore(xenophobia, violent, leader, greedy float64) float64 {
	return 0.4*max(xenophobia, 0) + 0.3*violent + 0.3*leader - 0.5*greedy
}

// stationPolicyFor is the policy a CEO keeps: blues only above stationClosedLine, else a freeport.
// Pure.
func stationPolicyFor(xenophobia, violent, leader, greedy float64) string {
	if stationClosedScore(xenophobia, violent, leader, greedy) > stationClosedLine {
		return stationBlues
	}
	return stationFreeport
}

// stationPolicyLabel words a policy for the panel.
func stationPolicyLabel(policy string) string {
	if policy == stationBlues {
		return "blues only: the alliance and its friends dock, neutrals and reds stay out"
	}
	return "a freeport: anyone may dock"
}

// stationPolicyRules is the access rules a policy writes; nil for a freeport, which clears them. Pure.
func stationPolicyRules(policy string) []evedb.ServiceAccessRule {
	if policy != stationBlues {
		return nil
	}
	var out []evedb.ServiceAccessRule
	for _, svc := range []int64{evedb.StationServiceDocking, evedb.StationServiceOfficeRental,
		evedb.StationServiceCloning, evedb.StationServiceJumpCloneFacility} {
		r := evedb.DefaultServiceAccessRule(svc)
		r.MinimumStanding = stationBlueMinStanding
		out = append(out, r)
	}
	return out
}

// keepStationPolicy writes the CEO's policy onto a station it owns when it changed or once a week,
// and logs a change.
func (m *Mod) keepStationPolicy(ctx context.Context, c *corpCEO, ch Charter, station int64) {
	if station == 0 {
		return
	}
	policy := ch.StationPolicy
	if policy == "" {
		policy = stationFreeport
	}
	var cur string
	var at int64
	_ = m.db.QueryRowContext(ctx, `SELECT policy, policyAt FROM rolebot_corp_fronts WHERE corporationID = ?`, c.CorpID).Scan(&cur, &at)
	now := clock.Unix()
	if cur == policy && now-at < stationPolicyEverySec {
		return
	}
	var err error
	if rules := stationPolicyRules(policy); rules != nil {
		err = evedb.SetStationAccessRules(ctx, m.db, station, c.CorpID, rules)
	} else {
		err = evedb.ClearStationAccessRules(ctx, m.db, station, c.CorpID)
	}
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "station policy at %s: %v", stationNameOf(ctx, m, station), err)
		return
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_fronts SET policy = ?, policyAt = ? WHERE corporationID = ?`,
		policy, now, c.CorpID)
	forgetStationAccess()
	if cur == policy {
		return
	}
	name := stationNameOf(ctx, m, station)
	m.ceoLog(ctx, c, powerEstate, "access", station, 0, "keeps %s as %s", name, stationPolicyLabel(policy))
	if aid := m.corpAllianceID(ctx, c.CorpID); aid != 0 {
		m.allianceBook(ctx, aid, c.CorpID, "access", station, 0, "%s keeps %s as %s",
			corpNameOf(ctx, m.db, c.CorpID), name, stationPolicyLabel(policy))
	}
}
