package rolebots

import (
	"context"
	"fmt"
)

// The ambition ladder: how a bot alliance climbs. Empire, then towers in 0.0, then sovereignty, then a
// conquerable station. Each rung reached for the first time is a milestone in the alliance's book and
// mailbox (rolebot_alliance_milestones); the station siege desk opens once the alliance holds sov
// (allianceStationSiege). The executor reads the ladder every pass (allianceLadder).

// allianceRung is a step of the ladder.
type allianceRung int

const (
	stageEmpire  allianceRung = iota // nothing in 0.0
	stageTowers                      // a member tower online in 0.0
	stageSov                         // the alliance holds a system
	stageStation                     // the alliance holds a conquerable station
)

// Milestone keys, one per rung above empire.
const (
	milestoneTowers  = "towers"
	milestoneSov     = "sov"
	milestoneStation = "station"
)

// rungLabel names a rung for the panel and the book.
func rungLabel(r allianceRung) string {
	switch r {
	case stageTowers:
		return "towers in 0.0"
	case stageSov:
		return "sovereignty"
	case stageStation:
		return "a conquerable station"
	}
	return "empire"
}

// rungMilestone is the milestone key a rung books, "" for empire.
func rungMilestone(r allianceRung) string {
	switch r {
	case stageTowers:
		return milestoneTowers
	case stageSov:
		return milestoneSov
	case stageStation:
		return milestoneStation
	}
	return ""
}

// allianceStageOf is the highest rung an alliance stands on. Pure.
func allianceStageOf(nullTowers, heldSystems, conquerables int) allianceRung {
	switch {
	case conquerables > 0:
		return stageStation
	case heldSystems > 0:
		return stageSov
	case nullTowers > 0:
		return stageTowers
	}
	return stageEmpire
}

// allianceLadderCounts reads what the ladder is measured on: member towers online in 0.0, held
// systems, conquerable stations its corporations own.
func (m *Mod) allianceLadderCounts(ctx context.Context, allianceID int64) (towers, held, stations int) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM starbase s
		  JOIN corporation c ON c.corporationID = s.ownerCorpID
		  JOIN mapSolarSystems m ON m.solarSystemID = s.solarSystemID
		 WHERE c.allianceID = ? AND s.state = 'online' AND m.security < 0`, allianceID).Scan(&towers)
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM solarSystemSov WHERE allianceID = ?`, allianceID).Scan(&held)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM staStations st
		  JOIN staStationTypes t ON t.stationTypeID = st.stationTypeID AND t.conquerable = 1
		  JOIN corporation c ON c.corporationID = st.corporationID
		 WHERE c.allianceID = ?`, allianceID).Scan(&stations)
	return towers, held, stations
}

// allianceStage is the rung an alliance stands on now.
func (m *Mod) allianceStage(ctx context.Context, allianceID int64) allianceRung {
	if allianceID == 0 {
		return stageEmpire
	}
	return allianceStageOf(m.allianceLadderCounts(ctx, allianceID))
}

// allianceLadder is the executor's pass: every rung up to the current one gets its milestone booked
// the first time it is reached.
func (m *Mod) allianceLadder(ctx context.Context, c *corpCEO, seat *allianceSeat) {
	stage := m.allianceStage(ctx, seat.AllianceID)
	for r := stageTowers; r <= stage; r++ {
		m.noteMilestone(ctx, seat.AllianceID, rungMilestone(r), 0, 0, fmt.Sprintf("%s reached %s", seat.Name, rungLabel(r)))
	}
}

// noteMilestone books a milestone the first time an alliance reaches it: a row, the alliance's book
// and a bulletin. A milestone already booked is left alone.
func (m *Mod) noteMilestone(ctx context.Context, allianceID int64, milestone string, systemID, stationID int64, line string) {
	if allianceID == 0 || milestone == "" {
		return
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT IGNORE INTO rolebot_alliance_milestones (allianceID, milestone, reachedAt, systemID, stationID)
		VALUES (?, ?, ?, ?, ?)`, allianceID, milestone, clock.Unix(), systemID, stationID)
	if err != nil {
		rlogf("rolebots: alliance milestone: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	exec := m.allianceExecutor(ctx, allianceID)
	m.allianceBook(ctx, allianceID, exec, "milestone", stationID, 0, "milestone: %s", line)
	if c, _ := getCEO(ctx, m.db, exec); c != nil {
		m.allianceBulletin(ctx, allianceID, c.CharID, "Milestone: "+line, line+". The alliance climbs.")
	}
}

// allianceMilestone is one booked milestone, as the panel reads it.
type allianceMilestone struct {
	Milestone string
	ReachedAt int64
	SystemID  int64
	StationID int64
}

// allianceMilestones lists an alliance's milestones, oldest first.
func (m *Mod) allianceMilestones(ctx context.Context, allianceID int64) []allianceMilestone {
	rows, err := m.db.QueryContext(ctx, `
		SELECT milestone, reachedAt, systemID, stationID FROM rolebot_alliance_milestones
		 WHERE allianceID = ? ORDER BY reachedAt`, allianceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []allianceMilestone
	for rows.Next() {
		var ms allianceMilestone
		if rows.Scan(&ms.Milestone, &ms.ReachedAt, &ms.SystemID, &ms.StationID) == nil {
			out = append(out, ms)
		}
	}
	return out
}
