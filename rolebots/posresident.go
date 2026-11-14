package rolebots

import (
	"context"
	"fmt"
	"sort"
)

// Life around the tower: what a player sees. There is no new mechanism for it. A bot whose
// abstract state is "in space at tower T" while a player has the system loaded is embodied there
// like at a belt or a gate. Towers give bots new reasons to be in space: fuel and backhaul runs,
// rig and deploy legs, survey passes, siege musters, and residents. A depot or fortress tower is
// home base to a few of its corporation's pilots (towerResidentCap, an operator setting) who
// stage there between jobs, idling inside the shield.
// An embodied visitor flies to the tower's moon for real before its cargo moves
// (embodyTowerPrep). Only the owning corporation's pilots are routed inside the force field: the
// server sim does not push a hostile ball out of it (only the client draws the bounce), so a
// hostile's approach point is always outside (parkAtTower parks own-corp carriers only).

const (
	residentStageEverySec = 4 * 3600
	residentStageMinSec   = 10 * 60
	residentStageMaxSec   = 25 * 60
	// TaskTowerStage is a resident idling at its tower between jobs.
	TaskTowerStage TaskKind = "starbase.stage"
)

func init() {
	registerTask(TaskDef{
		Kind: TaskTowerStage, Label: "Staging at its corporation's tower", Interruptible: true,
		Posture: PostureHold, Step: (*Mod).towerStageStep, Detail: detailTowerStage,
	})
}

func detailTowerStage(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.PosStagePlanID == 0 {
		return ""
	}
	p, _ := m.loadTowerPlan(ctx, st.Data.PosStagePlanID)
	if p == nil {
		return ""
	}
	return "at the tower on " + moonNameOf(ctx, m, p.MoonID)
}

// embodyTowerPrep flies an embodied visitor to a tower's moon before its
// business there happens. True while it is still on its way.
func (m *Mod) embodyTowerPrep(charID, moonID int64) bool {
	eb := liveProjection(charID)
	if eb == nil || moonID == 0 {
		return false
	}
	return m.flyToCelestial(eb, moonID)
}

// dealResidents keeps each depot, fortress and refinery tower's residents at the cap:
// combat and hauling pilots of the corporation, the keenest first, none of
// the seated officers, nobody resident at two towers.
func (m *Mod) dealResidents(ctx context.Context, c *corpCEO, plans []*towerPlan) {
	capN := towerResidentCap()
	homes := map[int64]bool{}
	for _, p := range plans {
		if (p.Purpose == purposeDepot || p.Purpose == purposeFortress || p.Purpose == purposeRefinery) && p.Status == towerRunning {
			homes[p.PlanID] = true
		}
	}
	// Drop residents whose tower is no longer a home, or who left.
	_, _ = m.db.ExecContext(ctx, `
		DELETE r FROM rolebot_tower_residents r
		  LEFT JOIN character_ ch ON ch.characterID = r.characterID
		  LEFT JOIN rolebot_corp_towers t ON t.planID = r.planID
		 WHERE (t.corporationID = ? AND (t.status <> 'running' OR t.purpose NOT IN ('depot','fortress','refinery')))
		    OR (t.corporationID = ? AND COALESCE(ch.corporationID, 0) <> t.corporationID)`, c.CorpID, c.CorpID)
	if len(homes) == 0 || capN <= 0 {
		return
	}
	members, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return
	}
	seated := m.posSeatsOf(ctx, c.CorpID)
	taken := map[int64]bool{}
	ids, _ := int64Column(ctx, m, `SELECT characterID FROM rolebot_tower_residents`)
	for _, id := range ids {
		taken[id] = true
	}
	type cand struct {
		id    int64
		score float64
	}
	var cands []cand
	for _, cm := range members {
		if cm.Human() || !cm.Enabled || seated[cm.CharID] != "" || taken[cm.CharID] {
			continue
		}
		id := m.memberIdentity(ctx, cm)
		cands = append(cands, cand{cm.CharID, id.Tune.ViolenceAppetite + id.Tune.HaulAppetite})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	planIDs := make([]int64, 0, len(homes))
	for id := range homes {
		planIDs = append(planIDs, id)
	}
	sort.Slice(planIDs, func(i, j int) bool { return planIDs[i] < planIDs[j] })
	next := 0
	for _, pid := range planIDs {
		var have int
		_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_tower_residents WHERE planID = ?`, pid).Scan(&have)
		for ; have < capN && next < len(cands); have++ {
			_, _ = m.db.ExecContext(ctx, `
				INSERT IGNORE INTO rolebot_tower_residents (characterID, planID, since) VALUES (?, ?, ?)`,
				cands[next].id, pid, clock.Unix())
			next++
		}
	}
}

// towerStageDue sends a resident, now and then, to idle at its tower.
func (m *Mod) towerStageDue(ctx context.Context, b *botRun) (bool, error) {
	if b.StationID == 0 || clock.Unix()-b.State.Data.PosStagedAt < residentStageEverySec {
		return false, nil
	}
	var planID int64
	if err := m.db.QueryRowContext(ctx, `SELECT planID FROM rolebot_tower_residents WHERE characterID = ?`, b.CharID).Scan(&planID); err != nil {
		return false, nil
	}
	p, _ := m.loadTowerPlan(ctx, planID)
	if p == nil || p.Status != towerRunning || p.CorpID != b.CorpID {
		return false, nil
	}
	if j := m.jumpsTo(ctx, b, p.SystemID); j < 0 || j > corpWorkMaxJumps {
		return false, nil
	}
	st := b.State
	st.Data = idleData(st.Data)
	st.Data.PosStagePlanID = planID
	st.Data.PosStagedAt = clock.Unix()
	m.logBot(b.CharID, logInfo, "corp", "heading out to stage at its corporation's tower on %s", moonNameOf(ctx, m, p.MoonID))
	return true, m.beginTravel(ctx, b, p.SystemID, 0, string(TaskTowerStage))
}

// towerStageStep holds the resident at its tower for a while, then hands it
// back to its own appetites.
func (m *Mod) towerStageStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	p, _ := m.loadTowerPlan(ctx, d.PosStagePlanID)
	done := func(note string) (bool, error) {
		if note != "" {
			m.logBot(b.CharID, logInfo, "corp", "%s", note)
		}
		st.Activity, st.Phase = "", string(TaskIdle)
		stagedAt := d.PosStagedAt
		st.Data = idleData(st.Data)
		st.Data.PosStagedAt = stagedAt
		return false, saveState(ctx, m.db, st)
	}
	if p == nil || p.Status != towerRunning {
		return done("its tower is no longer standing")
	}
	if b.SystemID != p.SystemID || b.StationID != 0 {
		return false, m.beginTravel(ctx, b, p.SystemID, 0, string(TaskTowerStage))
	}
	if m.embodyTowerPrep(b.CharID, p.MoonID) {
		d.LegEndsAt = clock.Unix() + navLegHoldSec
		return false, saveState(ctx, m.db, st)
	}
	if d.PosAtTowerUntil == 0 {
		m.parkAtTower(ctx, b, &haulLeg{PlanID: p.PlanID, ToID: p.TowerID})
		dwell := residentStageMinSec + int64(roll01(b.CharID, p.PlanID, clock.Unix()/3600)*float64(residentStageMaxSec-residentStageMinSec))
		d.PosAtTowerUntil = clock.Unix() + dwell
		d.LegEndsAt = d.PosAtTowerUntil
		return false, saveState(ctx, m.db, st)
	}
	return done(fmt.Sprintf("finished staging at the tower on %s", moonNameOf(ctx, m, p.MoonID)))
}
