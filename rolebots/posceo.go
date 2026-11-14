package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The CEO's starbase power: deciding a tower goes up, what it is for, where, how big and
// whose, then seeing it through: kit requisitioned and staged, the tower anchored, onlined,
// rigged, run, reviewed and torn down when it stops paying.
// It thinks with the rest of the CEO (ceoThink), so it reads free capital after the treasury
// and war powers have booked theirs. The day-to-day (fuel runs, purchases, onlining a tower
// whose anchoring finished between thinks) belongs to the seats (posseats.go: the Fueler and
// Procurer desks run on the staff desk sweep every 15 minutes).

const (
	powerStarbase = "starbase"

	towerMinMembers     = 3
	towerPerMembers     = 15
	towerMaxPerCorp     = 6
	towerReviewEverySec = 7 * 86400
	towerBadReviewsMax  = 2
	towerPlanStaleSec   = 21 * 86400
	towerKitBatches     = 2 // fuel batches staged with the tower for its first run
	towerSaveCommit     = "towersave"
	// towerStartISK is the free capital a corporation needs before it looks for a tower at all
	// (towerCapitalReady): about the cheapest plan, a Small tower, a harvester set and four
	// weeks of fuel.
	towerStartISK = 150_000_000.0
	// towerNeedMargin is how far free capital must clear a chosen plan's own
	// cost (kit plus fuelReserveWeeks of fuel) before the plan is written.
	towerNeedMargin = 1.25
	// settingTowerSavingsReleased marks the one-shot release of towersave commitments
	// (releaseTowerSavings).
	settingTowerSavingsReleased = "rolebots.towerSavingsReleased"
)

// towerPlan is one rolebot_corp_towers row.
type towerPlan struct {
	PlanID, CorpID, TowerID, MoonID, SystemID int64
	Purpose, Size                             string
	TowerTypeID                               int64
	Status                                    string
	Layout                                    towerLayout
	StagingStationID                          int64
	PlannedAt, AnchoredAt, OnlineAt           int64
	FuelerID, LastRunAt, UrgentAt             int64
	ReinforcedAt                              int64
	Losses, BadReviews                        int
	ReviewAt, ClosedAt                        int64
	Note                                      string
	// lostX is set when the stored layout lost its X offsets (layoutLostX); repairLostX
	// rebuilds it on load.
	lostX bool
}

func (p *towerPlan) active() bool { return p.Status != towerGone }

const towerCols = `planID, corporationID, towerID, moonID, systemID, purpose, size, towerTypeID, status,
	COALESCE(layout, ''), stagingStationID, plannedAt, anchoredAt, onlineAt, fuelerID, lastRunAt, urgentAt,
	reinforcedAt, losses, badReviews, reviewAt, closedAt, note`

func scanTowerPlan(sc interface{ Scan(...any) error }) (*towerPlan, error) {
	var p towerPlan
	var layout string
	if err := sc.Scan(&p.PlanID, &p.CorpID, &p.TowerID, &p.MoonID, &p.SystemID, &p.Purpose, &p.Size,
		&p.TowerTypeID, &p.Status, &layout, &p.StagingStationID, &p.PlannedAt, &p.AnchoredAt, &p.OnlineAt,
		&p.FuelerID, &p.LastRunAt, &p.UrgentAt, &p.ReinforcedAt, &p.Losses, &p.BadReviews, &p.ReviewAt,
		&p.ClosedAt, &p.Note); err != nil {
		return nil, err
	}
	if layout != "" {
		_ = json.Unmarshal([]byte(layout), &p.Layout)
		p.lostX = p.Status != towerGone && layoutLostX(layout)
	}
	return &p, nil
}

// repairLostX rebuilds a layout that lost its X offsets (layoutLostX) and saves it. Standing
// pieces take their real offsets from space; a battery read back as (0,0,0), the tower's
// centre, is refused by the core as inside the force field.
func (m *Mod) repairLostX(ctx context.Context, p *towerPlan) {
	if p == nil || !p.lostX {
		return
	}
	p.lostX = false
	standing := map[int64][3]float64{}
	var tx, ty, tz float64
	if p.TowerID != 0 && m.db.QueryRowContext(ctx,
		`SELECT x, y, z FROM entity WHERE itemID = ?`, p.TowerID).Scan(&tx, &ty, &tz) == nil {
		for _, pc := range p.Layout.Pieces {
			if pc.ItemID == 0 {
				continue
			}
			var x, y, z float64
			if m.db.QueryRowContext(ctx,
				`SELECT x, y, z FROM entity WHERE itemID = ?`, pc.ItemID).Scan(&x, &y, &z) == nil {
				standing[pc.ItemID] = [3]float64{x - tx, y - ty, z - tz}
			}
		}
	}
	race, _ := towerRaceOf(p.TowerTypeID)
	kept, dropped := rebuildLostX(p.Layout.Pieces, p.Size, race, p.MoonID, standing)
	p.Layout.Pieces = kept
	if err := m.saveTowerPlan(ctx, p); err != nil {
		rlogf("rolebots: repairing tower plan %d's layout: %v", p.PlanID, err)
		return
	}
	rlogf("rolebots: repaired tower plan %d's layout (%d standing piece(s) re-read from space, %d dropped)",
		p.PlanID, len(standing), dropped)
}

func (m *Mod) loadTowerPlan(ctx context.Context, planID int64) (*towerPlan, error) {
	p, err := scanTowerPlan(m.db.QueryRowContext(ctx, `SELECT `+towerCols+` FROM rolebot_corp_towers WHERE planID = ?`, planID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil {
		m.repairLostX(ctx, p)
	}
	return p, err
}

func (m *Mod) towerPlanByTower(ctx context.Context, towerID int64) (*towerPlan, error) {
	p, err := scanTowerPlan(m.db.QueryRowContext(ctx,
		`SELECT `+towerCols+` FROM rolebot_corp_towers WHERE towerID = ? AND status <> 'gone' ORDER BY planID DESC LIMIT 1`, towerID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil {
		m.repairLostX(ctx, p)
	}
	return p, err
}

func (m *Mod) corpTowerPlans(ctx context.Context, corpID int64, activeOnly bool) ([]*towerPlan, error) {
	where := `WHERE corporationID = ?`
	if activeOnly {
		where += ` AND status <> 'gone'`
	}
	rows, err := m.db.QueryContext(ctx, `SELECT `+towerCols+` FROM rolebot_corp_towers `+where+` ORDER BY planID`, corpID)
	if err != nil {
		return nil, fmt.Errorf("corpTowerPlans: %w", err)
	}
	defer rows.Close()
	var out []*towerPlan
	for rows.Next() {
		p, err := scanTowerPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // the repair writes; free the connection first
	for _, p := range out {
		m.repairLostX(ctx, p)
	}
	return out, nil
}

func (m *Mod) saveTowerPlan(ctx context.Context, p *towerPlan) error {
	layout, _ := json.Marshal(p.Layout)
	if p.PlanID == 0 {
		res, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_towers
				(corporationID, towerID, moonID, systemID, purpose, size, towerTypeID, status, layout,
				 stagingStationID, plannedAt, note)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.CorpID, p.TowerID, p.MoonID, p.SystemID, p.Purpose, p.Size, p.TowerTypeID, p.Status, string(layout),
			p.StagingStationID, p.PlannedAt, truncate(p.Note, 255))
		if err != nil {
			return fmt.Errorf("saveTowerPlan insert: %w", err)
		}
		p.PlanID, _ = res.LastInsertId()
		return nil
	}
	_, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_towers SET towerID = ?, moonID = ?, systemID = ?, purpose = ?, size = ?, towerTypeID = ?,
			status = ?, layout = ?, stagingStationID = ?, anchoredAt = ?, onlineAt = ?, fuelerID = ?,
			lastRunAt = ?, urgentAt = ?, reinforcedAt = ?, losses = ?, badReviews = ?, reviewAt = ?,
			closedAt = ?, note = ?
		 WHERE planID = ?`,
		p.TowerID, p.MoonID, p.SystemID, p.Purpose, p.Size, p.TowerTypeID, p.Status, string(layout),
		p.StagingStationID, p.AnchoredAt, p.OnlineAt, p.FuelerID, p.LastRunAt, p.UrgentAt, p.ReinforcedAt,
		p.Losses, p.BadReviews, p.ReviewAt, p.ClosedAt, truncate(p.Note, 255), p.PlanID)
	return err
}

// corpAllianceOf is the corporation's alliance (0 for none).
func (m *Mod) corpAllianceOf(ctx context.Context, corpID int64) int64 {
	var a sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID).Scan(&a)
	return a.Int64
}

// ── the power ───────────────────────────────────────────────────────────────

// ceoStarbase is the think's starbase pass.
func (m *Mod) ceoStarbase(ctx context.Context, c *corpCEO, ch Charter) error {
	if !StarbasePower() {
		return nil
	}
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	m.sweepSurveys(ctx, c.CorpID)
	m.sweepHauls(ctx, c.CorpID)
	plans, err := m.corpTowerPlans(ctx, c.CorpID, true)
	if err != nil {
		return err
	}
	// Once a Director of Infrastructure is seated, a tower in space is the department's
	// (posinfra.go); the CEO keeps planning, staging and the deploy, and runs the upkeep while nobody heads it.
	infra := m.infraRunsTowers(ctx, c)
	for _, p := range plans {
		if ctx.Err() != nil {
			return nil
		}
		m.restageTower(ctx, c, p)
		if infra && infraOwnsStatus(p.Status) {
			continue
		}
		if err := m.advanceTowerPlan(ctx, c, ch, facts, p); err != nil {
			m.logBot(c.CharID, logError, "ceo", "tower %d: %v", p.PlanID, err)
			continue
		}
		if p.Status == towerRunning {
			m.maintainTower(ctx, c, ch, p)
		}
	}
	m.reviewTowers(ctx, c, ch, plans)
	m.reconsiderTowers(ctx, c, ch, facts, plans)
	m.ceoTowerNetwork(ctx, c, ch, plans)
	m.dealResidents(ctx, c, plans)
	m.ceoTowerIndustry(ctx, c, ch, plans)
	m.ceoRefineryPass(ctx, c)
	outcome, err := m.considerNewTower(ctx, c, ch, facts, plans)
	if err != nil {
		return err
	}
	m.logTowerOutcome(ctx, c, outcome)
	return nil
}

// towerNarrated are the verbs that already say, in the CEO log, what the
// tower decision came to: the money wait, the price wait and the plan.
var towerNarrated = []string{"towerwait", "towersave", "plan", "towerwhy"}

// logTowerOutcome writes the tower decision's outcome to the CEO log once a day when nothing
// else has said it, so a wealthy corporation that never planned a tower still shows why.
func (m *Mod) logTowerOutcome(ctx context.Context, c *corpCEO, outcome string) {
	if outcome == "" {
		return
	}
	now := clock.Unix()
	for _, verb := range towerNarrated {
		if now-m.lastCEOActionAt(ctx, c.CorpID, verb) < 86400 {
			return
		}
	}
	m.ceoLog(ctx, c, powerStarbase, "towerwhy", 0, 0, "is not planning a tower: %s", outcome)
}

// considerTowerNow runs one corporation's tower decision on the spot, outside its think (the
// Starbases board's "consider now"). The gates are the same; only the moment is the
// operator's. It holds the corporation's op lock, so it cannot interleave with the CEO or a
// staff desk, and returns the outcome line.
func (m *Mod) considerTowerNow(ctx context.Context, corpID int64) (string, error) {
	if !StarbasePower() {
		return "", errors.New("the starbase power is switched off")
	}
	c, err := getCEO(ctx, m.db, corpID)
	if err != nil {
		return "", err
	}
	if c == nil {
		return "", errors.New("no bot CEO runs that corporation")
	}
	if !c.powerOn(powerStarbase) {
		return "", errors.New("its CEO is paused or has the starbase power off")
	}
	ch, err := m.ceoCharterFor(ctx, c)
	if err != nil {
		return "", err
	}
	facts, err := getAnyCorpFacts(ctx, m.db, corpID)
	if err != nil {
		return "", err
	}
	lk := corpOpLock(corpID)
	lk.Lock()
	defer lk.Unlock()
	ctx = withOpLockHeld(ctx)
	if s, err := m.loadCorpStaff(ctx, corpID, c.CharID); err == nil {
		ctx = withCorpStaff(ctx, s)
	}
	plans, err := m.corpTowerPlans(ctx, corpID, true)
	if err != nil {
		return "", err
	}
	return m.considerNewTower(ctx, c, ch, facts, plans)
}

// ── advancing a plan ────────────────────────────────────────────────────────

// advanceTowerPlan moves one tower along its lifecycle.
func (m *Mod) advanceTowerPlan(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, p *towerPlan) error {
	// The core's word on the tower outranks the plan's.
	if p.TowerID != 0 {
		if done, err := m.reconcileTowerPlan(ctx, c, p); err != nil || done {
			return err
		}
	}
	switch p.Status {
	case towerPlanned:
		return m.advancePlanned(ctx, c, ch, facts, p)
	case towerStaging:
		if leg := m.openHaulFor(ctx, p.PlanID, haulKindDeploy); leg == nil && p.TowerID == 0 {
			// The deploy leg ended without anchoring anything: back to staging.
			p.Status = towerPlanned
			return m.saveTowerPlan(ctx, p)
		}
	case towerAnchored:
		m.tryOnlineTower(ctx, c, p)
	case towerRigging:
		return m.advanceRigging(ctx, c, ch, p)
	case towerRunning:
		m.ensureTowerWiring(ctx, c, p)
	case towerUnanchor:
		return m.advanceTeardown(ctx, c, p)
	}
	return nil
}

// reconcileTowerPlan reads the tower's real state. done = the plan is closed.
func (m *Mod) reconcileTowerPlan(ctx context.Context, c *corpCEO, p *towerPlan) (bool, error) {
	rec, ok, err := evedb.GetStarbase(ctx, m.db, p.TowerID)
	if err != nil {
		return false, err
	}
	if !ok {
		// Not anchored any more: unanchored (a free entity in space) or gone.
		var loc int64
		lerr := m.db.QueryRowContext(ctx, `SELECT locationID FROM entity WHERE itemID = ?`, p.TowerID).Scan(&loc)
		if lerr == nil && p.Status == towerUnanchor {
			return false, nil // teardown carries on (scoop)
		}
		if lerr == nil && loc != p.SystemID {
			p.Status, p.ClosedAt = towerGone, clock.Unix()
			p.Note = "the tower was scooped"
			return true, m.closeTowerPlan(ctx, c, p, "scooped")
		}
		p.Losses++
		p.Status, p.ClosedAt = towerGone, clock.Unix()
		p.Note = "the tower no longer stands"
		m.ceoLog(ctx, c, powerStarbase, "lost", p.MoonID, 0,
			"lost its %s tower at %s", purposeLabel[p.Purpose], moonNameOf(ctx, m, p.MoonID))
		return true, m.closeTowerPlan(ctx, c, p, "lost")
	}
	switch rec.State {
	case evedb.StarbaseStateAnchored, evedb.StarbaseStateOnlining:
		if p.Status == towerRunning || p.Status == towerRigging {
			// Went dark (out of fuel, or offlined): the Fueler's urgent run
			// and the next online attempt bring it back.
			p.Status = towerAnchored
			p.UrgentAt = clock.Unix()
		} else if p.Status == towerStaging || p.Status == towerPlanned {
			p.Status = towerAnchored
		}
	case evedb.StarbaseStateOnline, evedb.StarbaseStateReinforced:
		if p.Status == towerAnchored || p.Status == towerStaging || p.Status == towerPlanned {
			p.Status = towerRigging
			if p.OnlineAt == 0 {
				p.OnlineAt = clock.Unix()
			}
		}
		if rec.State == evedb.StarbaseStateReinforced && p.ReinforcedAt == 0 {
			p.ReinforcedAt = clock.Unix()
		}
		if rec.State == evedb.StarbaseStateOnline {
			p.ReinforcedAt = 0
		}
	case evedb.StarbaseStateUnanchoring:
		p.Status = towerUnanchor
	}
	return false, m.saveTowerPlan(ctx, p)
}

// closeTowerPlan releases everything a dead plan still holds.
func (m *Mod) closeTowerPlan(ctx context.Context, c *corpCEO, p *towerPlan, why string) error {
	m.voidReqsFor(ctx, c.CorpID, "tower", p.PlanID, "the tower is "+why)
	// The standing fuel reserve the plan held (planTower) is released.
	m.closeCommit(ctx, towerSaveCommit, p.PlanID)
	legs, _ := m.queryHauls(ctx, `WHERE planID = ? AND status = 'open'`, p.PlanID)
	for _, l := range legs {
		if l.ToKind == haulToTower {
			m.closeHaul(ctx, l, "void", "the tower is "+why)
		}
	}
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_tower_residents WHERE planID = ?`, p.PlanID)
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_chain WHERE fromPlanID = ? OR toPlanID = ?`, p.PlanID, p.PlanID)
	return m.saveTowerPlan(ctx, p)
}

// advancePlanned stages a planned tower's kit and posts its deploy leg once
// the tower and its first fuel are on the shelf.
func (m *Mod) advancePlanned(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, p *towerPlan) error {
	if clock.Unix()-p.PlannedAt > towerPlanStaleSec {
		p.Status, p.ClosedAt, p.Note = towerGone, clock.Unix(), "the kit never came together"
		m.ceoLog(ctx, c, powerStarbase, "abandon", p.MoonID, 0,
			"abandoned its tower plan at %s — the kit never came together", moonNameOf(ctx, m, p.MoonID))
		return m.closeTowerPlan(ctx, c, p, "abandoned")
	}
	// A plan whose staging office is gone is re-staged from the office now nearest the moon, or
	// abandoned below.
	if p.StagingStationID == 0 || m.officeAt(ctx, c.CorpID, p.StagingStationID) == 0 {
		st, err := m.pickStagingStation(ctx, c, p.SystemID)
		if err != nil {
			return err
		}
		if st == 0 {
			// No office reaches the moon any more: release the kit on order and replan from the offices
			// the corporation has.
			p.Status, p.ClosedAt, p.Note = towerGone, clock.Unix(), "no office to stage it from"
			m.ceoLog(ctx, c, powerStarbase, "abandon", p.MoonID, 0,
				"abandoned its tower plan at %s — no office of its reaches it to stage it from",
				moonNameOf(ctx, m, p.MoonID))
			return m.closeTowerPlan(ctx, c, p, "abandoned")
		}
		p.StagingStationID = st
		if err := m.saveTowerPlan(ctx, p); err != nil {
			return err
		}
	}
	m.nameStarbaseDivisions(ctx, c.CorpID, facts.Name)
	office := m.officeAt(ctx, c.CorpID, p.StagingStationID)
	kit := m.divisionHeld(ctx, c.CorpID, office, corpDivStarbase)
	fuel := m.divisionHeld(ctx, c.CorpID, office, corpDivFuel)
	batch, err := m.towerFuelBatch(ctx, 0, p.TowerTypeID, p.SystemID, c.CorpID, 0)
	if err != nil {
		return err
	}
	// Requisition whatever the shelf lacks: the tower, each structure, and
	// the first fuel batches.
	want := map[int64]int64{p.TowerTypeID: 1}
	for _, pc := range p.Layout.Pieces {
		if isDefenseType(pc.TypeID) {
			continue // guns are ordered once the working loop stands (rigMissing)
		}
		want[pc.TypeID]++
		if isReactorType(pc.TypeID) && pc.Provision != 0 {
			want[pc.Provision]++ // the reaction the reactor runs
		}
	}
	// The kit is bought on trips, not bid for: a tower nobody sells at the
	// staging station would otherwise wait out a whole bid's staleness.
	deadline := clock.Unix() + 2*86400
	for t, q := range want {
		short := q - kit[t] - m.reqPending(ctx, c.CorpID, t, p.StagingStationID, corpDivStarbase)
		short -= m.pullStash(ctx, c, p, t, short) // spare kit in another office first (posstash.go)
		if short > 0 {
			if _, err := m.postRequisition(ctx, c, requisition{TypeID: t, Qty: short, DestStationID: p.StagingStationID,
				DestFlag: corpDivStarbase, Requester: "ceo", RefKind: "tower", RefID: p.PlanID, Deadline: deadline,
				Note: "starbase kit"}); err != nil {
				return err
			}
		}
	}
	for t, q := range topUpUnits(batch, fuel, towerKitBatches) {
		short := q - m.reqPending(ctx, c.CorpID, t, p.StagingStationID, corpDivFuel)
		if short > 0 {
			if _, err := m.postRequisition(ctx, c, requisition{TypeID: t, Qty: short, DestStationID: p.StagingStationID,
				DestFlag: corpDivFuel, Requester: "fueler", RefKind: "tower", RefID: p.PlanID, Deadline: clock.Unix() + 2*86400,
				Note: "first fuel"}); err != nil {
				return err
			}
		}
	}
	// Ready to deploy: the tower and at least one fuel batch.
	if kit[p.TowerTypeID] < 1 || batchesHeld(batch, fuel) < 1 {
		return nil
	}
	if m.openHaulFor(ctx, p.PlanID, haulKindDeploy) != nil {
		return nil
	}
	items := []haulItem{{TypeID: p.TowerTypeID, Qty: 1, Flag: corpDivStarbase}}
	for t, q := range batchUnits(batch, min(towerKitBatches, batchesHeld(batch, fuel))) {
		items = append(items, haulItem{TypeID: t, Qty: q, Flag: corpDivFuel})
	}
	if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindDeploy, PlanID: p.PlanID, FromKind: haulFromOffice,
		FromID: office, FromStationID: p.StagingStationID, FromFlag: -1, ToKind: haulToTower,
		ToSystemID: p.SystemID, Items: items, PostedBy: "ceo", Deadline: clock.Unix() + 3*86400,
		Note: "anchor the tower at " + moonNameOf(ctx, m, p.MoonID)}); err != nil {
		return err
	}
	p.Status = towerStaging
	m.ceoLog(ctx, c, powerStarbase, "deploy", p.MoonID, 0,
		"has its %s tower's kit in hand and sent it out to %s", purposeLabel[p.Purpose], moonNameOf(ctx, m, p.MoonID))
	return m.saveTowerPlan(ctx, p)
}

// towerNoFuelLogAt throttles tryOnlineTower's "the bay lacks" line to once a day per tower plan.
var towerNoFuelLogAt sync.Map // planID to unix time

// fuelShortForCycle is the bill's types the bay holds less than one cycle of, in bill order. Pure.
func fuelShortForCycle(needs []evedb.FuelNeed, held map[int64]int64) []int64 {
	var out []int64
	for _, n := range needs {
		if n.Quantity > 0 && held[n.TypeID] < n.Quantity {
			out = append(out, n.TypeID)
		}
	}
	return out
}

// tryOnlineTower onlines an anchored tower once its anchoring has run and
// its bay holds a cycle of fuel (db.OnlineStarbaseForChecked). A bay short of a
// cycle is logged once a day per plan, naming the missing types.
func (m *Mod) tryOnlineTower(ctx context.Context, c *corpCEO, p *towerPlan) {
	if p.TowerID == 0 {
		return
	}
	rec, ok, err := evedb.GetStarbase(ctx, m.db, p.TowerID)
	if err != nil || !ok || rec.State != evedb.StarbaseStateAnchored {
		return
	}
	if _, err := evedb.OnlineStarbaseForChecked(ctx, m.db, p.TowerID, nil); err != nil {
		if !errors.Is(err, evedb.ErrAnchoringNotDone) && !errors.Is(err, evedb.ErrNoFuelForOnline) {
			m.logBot(c.CharID, logInfo, "ceo", "could not online its tower at %s: %v", moonNameOf(ctx, m, p.MoonID), err)
		}
		if errors.Is(err, evedb.ErrNoFuelForOnline) {
			now := clock.Unix()
			p.UrgentAt = now
			_ = m.saveTowerPlan(ctx, p)
			if v, ok := towerNoFuelLogAt.Load(p.PlanID); !ok || now-v.(int64) >= 86400 {
				towerNoFuelLogAt.Store(p.PlanID, now)
				m.logBot(c.CharID, logInfo, "ceo", "cannot online its tower at %s: the bay lacks %s",
					moonNameOf(ctx, m, p.MoonID), m.towerFuelShortLabel(ctx, rec))
			}
		}
		return
	}
	m.ceoLog(ctx, c, powerStarbase, "online", p.TowerID, 0,
		"brought its tower at %s online", moonNameOf(ctx, m, p.MoonID))
}

// towerFuelShortLabel names the types an anchored tower's bay holds less than one cycle of
// (fuelShortForCycle), or "a cycle of fuel" when the bill cannot be read.
func (m *Mod) towerFuelShortLabel(ctx context.Context, rec evedb.StarbaseRecord) string {
	needs, err := evedb.StarbaseFuelNeeds(ctx, m.db, evedb.StarbaseFuelRow{EntityID: rec.EntityID, TypeID: rec.TypeID,
		SolarSystemID: rec.SolarSystemID, OwnerCorpID: rec.OwnerCorpID, OwnerAllianceID: rec.OwnerAllianceID})
	if err != nil {
		return "a cycle of fuel"
	}
	short := fuelShortForCycle(needs, m.bayHeld(ctx, rec.EntityID))
	if len(short) == 0 {
		return "a cycle of fuel"
	}
	names := make([]string, 0, len(short))
	for _, t := range short {
		names = append(names, m.typeNameOr(ctx, t))
	}
	return strings.Join(names, ", ")
}

// advanceRigging hauls the structures out, onlines them, and sets them to
// work. The tower runs once every piece stands and is online.
func (m *Mod) advanceRigging(ctx context.Context, c *corpCEO, ch Charter, p *towerPlan) error {
	if p.TowerID == 0 {
		return nil
	}
	// Pieces anchored but not yet online, brought up in onlineOrder: grid boosters, then the
	// working pieces, then hardeners, then batteries. Once a piece is refused for want of power
	// or CPU, nothing of a later rank is tried this pass, so a battery never takes the grid a
	// harvester is waiting for.
	allUp := true
	states := make([]string, len(p.Layout.Pieces))
	for i := range p.Layout.Pieces {
		pc := &p.Layout.Pieces[i]
		if pc.Strip {
			continue // on its way out (posreconsider.go)
		}
		if pc.ItemID == 0 {
			allUp = false
			continue
		}
		rec, ok, err := evedb.GetStarbaseStructure(ctx, m.db, pc.ItemID)
		if err != nil {
			return err
		}
		if !ok {
			pc.ItemID = 0 // destroyed or removed: back on the list
			allUp = false
			continue
		}
		states[i] = rec.State
		if rec.State != evedb.StarbaseStateOnline && !(rec.State == evedb.StarbaseStateAnchored && pc.Off) {
			allUp = false // an offline piece Infrastructure took down (posinfra.go) does not count
		}
	}
	blockedRank := -1
	for _, i := range onlineOrder(p.Layout.Pieces) {
		pc := &p.Layout.Pieces[i]
		if pc.Strip || pc.Off || pc.ItemID == 0 || states[i] != evedb.StarbaseStateAnchored {
			continue
		}
		rank := onlineRank(pc.TypeID)
		if blockedRank >= 0 && rank > blockedRank {
			continue
		}
		_, err := evedb.OnlineStructureFor(ctx, m.db, pc.ItemID, nil)
		if err == nil {
			continue
		}
		if isGridRefusal(err) {
			if blockedRank < 0 {
				blockedRank = rank
			}
			// A booster or working piece short of grid makes room by taking down batteries and
			// hardeners that came up first (an older tower, or one that came back from dark in
			// layout order), then tries once more.
			if rank <= rankWork && m.makeRoomFor(ctx, c, p, states, i) {
				if _, err = evedb.OnlineStructureFor(ctx, m.db, pc.ItemID, nil); err == nil {
					continue
				}
			}
		}
		m.logBot(c.CharID, logInfo, "ceo", "a %s at %s would not online: %v",
			m.typeNameOr(ctx, pc.TypeID), moonNameOf(ctx, m, p.MoonID), err)
	}
	if allUp {
		p.Status = towerRunning
		m.ensureTowerWiring(ctx, c, p)
		m.applyTowerDefense(ctx, c, p)
		m.allianceTowerPolicy(ctx, c, p)
		m.ceoLog(ctx, c, powerStarbase, "running", p.TowerID, 0,
			"its %s tower at %s is up and running (%d structures)", purposeLabel[p.Purpose],
			moonNameOf(ctx, m, p.MoonID), len(p.Layout.Pieces))
		return m.saveTowerPlan(ctx, p)
	}
	// Wire what already stands. Wiring is per piece (a harvester and its silo, a reactor and its
	// feed), so a pair that is up does not wait for a battery or hangar that is not.
	m.ensureTowerWiring(ctx, c, p)
	if err := m.rigMissing(ctx, c, p); err != nil {
		return err
	}
	return m.saveTowerPlan(ctx, p)
}

// makeRoomFor takes down the batteries and hardeners roomFor names so piece i (a booster or a
// working piece) fits the tower's grid. They are not marked Off: advanceRigging brings them
// back in order once the piece is up and the grid allows. false when nothing was taken down.
func (m *Mod) makeRoomFor(ctx context.Context, c *corpCEO, p *towerPlan, states []string, i int) bool {
	grid, err := evedb.GetStarbaseGrid(ctx, m.db, p.TowerID, p.TowerTypeID)
	if err != nil {
		return false
	}
	online := make([]bool, len(p.Layout.Pieces))
	for j, st := range states {
		online[j] = st == evedb.StarbaseStateOnline || st == evedb.StarbaseStateOnlining
	}
	pc := p.Layout.Pieces[i]
	free := structFit{Power: grid.PowerOutput - grid.PowerUsed, CPU: grid.CPUOutput - grid.CPUUsed}
	off := roomFor(p.Layout.Pieces, online, onlineRank(pc.TypeID), free, pieceFit(pc.TypeID))
	took := 0
	for _, j := range off {
		if err := evedb.OfflineStructureFor(ctx, m.db, p.Layout.Pieces[j].ItemID, nil); err != nil {
			continue
		}
		states[j] = evedb.StarbaseStateAnchored
		took++
	}
	if took > 0 {
		m.ceoLog(ctx, c, powerStarbase, "offline", p.TowerID, 0,
			"took %d defensive structure(s) at %s offline to bring its %s up first",
			took, moonNameOf(ctx, m, p.MoonID), m.typeNameOr(ctx, pc.TypeID))
	}
	return took > 0
}

// coreMissing reports whether any working piece of the layout (everything but
// the batteries) is still to stand. Pieces on their way out do not count.
func coreMissing(p *towerPlan) bool {
	for _, pc := range p.Layout.Pieces {
		if pc.Strip || isDefenseType(pc.TypeID) {
			continue
		}
		if pc.ItemID == 0 {
			return true
		}
	}
	return false
}

// rigMissing hauls what is on the Kit shelf for the plan's pieces that do not stand, and
// orders the rest from Sales: a new tower's rigging, a loss and an upgrade (posinfra.go)
// alike. One rig leg per tower at a time.
func (m *Mod) rigMissing(ctx context.Context, c *corpCEO, p *towerPlan) error {
	if m.openHaulFor(ctx, p.PlanID, haulKindRig) == nil {
		office := m.officeAt(ctx, c.CorpID, p.StagingStationID)
		kit := m.divisionHeld(ctx, c.CorpID, office, corpDivStarbase)
		missing := map[int64]int64{}
		// Guns come after the working loop: while a harvester, silo or hangar is still to stand,
		// batteries are neither hauled nor ordered, so money and trips go to the pieces that earn.
		holdGuns := coreMissing(p)
		for _, pc := range p.Layout.Pieces {
			if holdGuns && isDefenseType(pc.TypeID) {
				continue
			}
			if pc.ItemID == 0 {
				missing[pc.TypeID]++
				if isReactorType(pc.TypeID) && pc.Provision != 0 {
					missing[pc.Provision]++
				}
			}
		}
		var items []haulItem
		for t, q := range missing {
			if n := min(q, kit[t]); n > 0 {
				items = append(items, haulItem{TypeID: t, Qty: n, Flag: corpDivStarbase})
			} else if covered := m.pullStash(ctx, c, p, t, q); covered >= q {
				continue // spare kit is on its way from another office (posstash.go)
			} else if m.reqPending(ctx, c.CorpID, t, p.StagingStationID, corpDivStarbase) < q-covered {
				_, _ = m.postRequisition(ctx, c, requisition{TypeID: t, Qty: q - kit[t] - covered, DestStationID: p.StagingStationID,
					DestFlag: corpDivStarbase, Requester: posRequester(ctx, "ceo"), RefKind: "tower", RefID: p.PlanID,
					Deadline: clock.Unix() + 4*86400, Note: "structure for " + moonNameOf(ctx, m, p.MoonID)})
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].TypeID < items[j].TypeID })
		if len(items) > 0 && office != 0 {
			if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindRig, PlanID: p.PlanID, FromKind: haulFromOffice,
				FromID: office, FromStationID: p.StagingStationID, FromFlag: corpDivStarbase, ToKind: haulToTower,
				ToID: p.TowerID, ToSystemID: p.SystemID, Items: items, PostedBy: posRequester(ctx, "ceo"),
				Deadline: clock.Unix() + 3*86400, Note: "anchor structures at " + moonNameOf(ctx, m, p.MoonID)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureTowerWiring sets every harvester and silo to its material and wires each harvester
// to its silo, the Production tab's job. Idempotent.
func (m *Mod) ensureTowerWiring(ctx context.Context, c *corpCEO, p *towerPlan) {
	if p.TowerID == 0 {
		return
	}
	var links []evedb.StructureLink
	siloFor := map[int64]int64{}
	for _, pc := range p.Layout.Pieces {
		if pc.ItemID == 0 || pc.Provision == 0 {
			continue
		}
		rec, ok, _ := evedb.GetStarbaseStructure(ctx, m.db, pc.ItemID)
		if !ok {
			continue
		}
		if rec.ProvisionTypeID != pc.Provision {
			if err := evedb.SetStructureProvisionType(ctx, m.db, pc.ItemID, pc.Provision); err != nil {
				continue
			}
		}
		if pc.TypeID == typeSilo {
			siloFor[pc.Provision] = pc.ItemID
		}
	}
	for _, pc := range p.Layout.Pieces {
		if pc.ItemID != 0 && pc.TypeID == typeHarvester && siloFor[pc.Provision] != 0 {
			links = append(links, evedb.StructureLink{TypeID: pc.Provision, SourceID: pc.ItemID, DestID: siloFor[pc.Provision]})
		}
	}
	links = append(links, m.reactionLinks(ctx, p)...)
	if len(links) == 0 {
		return
	}
	cur, _ := evedb.ListStructureLinks(ctx, m.db, p.TowerID)
	if linksEqual(cur, links) {
		return
	}
	if err := evedb.SetStructureLinks(ctx, m.db, p.TowerID, links); err != nil {
		m.logBot(c.CharID, logError, "ceo", "wiring the tower at %s: %v", moonNameOf(ctx, m, p.MoonID), err)
	}
}

func isReactorType(t int64) bool { return t == typeReactor || t == typeMediumReactor }

func linksEqual(a, b []evedb.StructureLink) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(l evedb.StructureLink) [3]int64 { return [3]int64{l.TypeID, l.SourceID, l.DestID} }
	set := map[[3]int64]bool{}
	for _, l := range a {
		set[key(l)] = true
	}
	for _, l := range b {
		if !set[key(l)] {
			return false
		}
	}
	return true
}

// ── deploy and rig at the tower (the carrier's half) ────────────────────────

// deployTower anchors the tower item the carrier brought at its moon, and
// loads the fuel it brought into the new tower's bay.
func (m *Mod) deployTower(ctx context.Context, b *botRun, l *haulLeg) (string, error) {
	p, err := m.loadTowerPlan(ctx, l.PlanID)
	if err != nil || p == nil {
		return "the tower plan is gone", err
	}
	if p.TowerID == 0 {
		var itemID int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT itemID FROM entity WHERE locationID = ? AND flag = 5 AND ownerID = ? AND typeID = ? ORDER BY itemID LIMIT 1`,
			b.ShipID, b.CorpID, p.TowerTypeID).Scan(&itemID)
		if itemID == 0 {
			return "the tower was not in the hold", nil
		}
		rec, err := evedb.AnchorTowerFor(ctx, m.db, evedb.AnchorTowerRequest{
			ItemID: itemID, MoonID: p.MoonID, CorpID: b.CorpID, AllianceID: m.corpAllianceOf(ctx, b.CorpID),
		}, nil)
		if err != nil {
			// A placement refusal (somebody got the moon first, the rules
			// changed): the tower rides home and the plan is reconsidered.
			p.Note = "anchor refused: " + err.Error()
			_ = m.saveTowerPlan(ctx, p)
			return "the anchor was refused: " + err.Error(), nil
		}
		p.TowerID, p.Status, p.AnchoredAt = rec.EntityID, towerAnchored, clock.Unix()
		if err := m.saveTowerPlan(ctx, p); err != nil {
			return "", err
		}
		for i := range l.Carrying {
			if l.Carrying[i].TypeID == p.TowerTypeID {
				l.Carrying[i].Qty--
			}
		}
		l.ToID = p.TowerID
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_haul SET toID = ? WHERE legID = ?`, p.TowerID, l.LegID)
		if c, _ := getCEO(ctx, m.db, b.CorpID); c != nil {
			m.ceoLog(ctx, c, powerStarbase, "anchor", p.TowerID, 0,
				"%s anchored a %s at %s", m.charNameOr(ctx, b.CharID), m.typeNameOr(ctx, p.TowerTypeID), moonNameOf(ctx, m, p.MoonID))
		}
		// A tower in space is Infrastructure's from here (infraOwnsStatus):
		// the CEO thinks next sweep so the department is staffed for it.
		wakeCEO(b.CorpID)
		m.towerBook(ctx, p, "structure", -evedb.TypeValueOf(ctx, m.db, p.TowerTypeID).Value, p.TowerTypeID, 1, "the tower")
	}
	// The fuel goes into the bay like any fuel leg's.
	l.Kind = haulKindFuel
	note, err := m.deliverToTower(ctx, b, l)
	l.Kind = haulKindDeploy
	return "anchored the tower and " + note, err
}

// rigTower anchors the structures the carrier brought around an online tower
// at the offsets the plan laid out.
func (m *Mod) rigTower(ctx context.Context, b *botRun, l *haulLeg) (string, error) {
	p, err := m.loadTowerPlan(ctx, l.PlanID)
	if err != nil || p == nil {
		return "the tower plan is gone", err
	}
	if p.TowerID == 0 {
		return "the tower is not anchored", nil
	}
	placeDefenses(p.Layout.Pieces, p.Size)
	anchored := 0
	var last error
	for i := range p.Layout.Pieces {
		pc := &p.Layout.Pieces[i]
		if pc.ItemID != 0 {
			continue
		}
		var itemID int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT itemID FROM entity WHERE locationID = ? AND flag = 5 AND ownerID = ? AND typeID = ? ORDER BY itemID LIMIT 1`,
			b.ShipID, b.CorpID, pc.TypeID).Scan(&itemID)
		if itemID == 0 {
			continue
		}
		rec, err := evedb.AnchorSlaveFor(ctx, m.db, p.TowerID, itemID, pc.DX, pc.DY, pc.DZ, nil)
		if err != nil {
			last = err
			continue
		}
		pc.ItemID = rec.EntityID
		anchored++
		for j := range l.Carrying {
			if l.Carrying[j].TypeID == pc.TypeID && l.Carrying[j].Qty > 0 {
				l.Carrying[j].Qty--
				break
			}
		}
		m.towerBook(ctx, p, "structure", -evedb.TypeValueOf(ctx, m.db, pc.TypeID).Value, pc.TypeID, 1, m.typeNameOr(ctx, pc.TypeID))
	}
	if err := m.saveTowerPlan(ctx, p); err != nil {
		return "", err
	}
	if n := m.insertReaction(ctx, b, p, l); n > 0 {
		return fmt.Sprintf("anchored %d structure(s) and loaded the reactor", anchored), nil
	}
	if anchored == 0 && last != nil {
		return "no structure would anchor: " + last.Error(), nil
	}
	return fmt.Sprintf("anchored %d structure(s)", anchored), nil
}

// ── staging ─────────────────────────────────────────────────────────────────

// pickStagingStation is the office a tower's kit and fuel are staged at: the corporation's
// office nearest the tower, however far. An office is never rented just to stage a tower, so
// 0 means the corporation holds no office the gate map joins to the moon.
func (m *Mod) pickStagingStation(ctx context.Context, c *corpCEO, sysID int64) (int64, error) {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return 0, err
	}
	reach := bfsJumps(u, sysID, unlimitedJumps)
	best, bestJ := int64(0), math.MaxInt32
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, s.solarSystemID FROM crpoffices o JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ?`, c.CorpID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var st, sys int64
		if rows.Scan(&st, &sys) == nil {
			if j, ok := reach[sys]; ok && (j < bestJ || (j == bestJ && st < best)) {
				best, bestJ = st, j
			}
		}
	}
	rows.Close()
	return best, nil
}

// restageTower moves a standing tower's platform to the next-nearest office when the office
// it was supplied from is gone (evicted, unrented): the requisitions, fuel legs and rig legs
// all run from p.StagingStationID. A plan still being staged is advancePlanned's to re-stage.
func (m *Mod) restageTower(ctx context.Context, c *corpCEO, p *towerPlan) {
	if p.TowerID == 0 || p.Status == towerPlanned || p.Status == towerGone || p.StagingStationID == 0 {
		return
	}
	if m.officeAt(ctx, c.CorpID, p.StagingStationID) != 0 {
		return
	}
	st, err := m.pickStagingStation(ctx, c, p.SystemID)
	if err != nil {
		return
	}
	if st == 0 {
		if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "nostage") >= 86400 {
			m.ceoLog(ctx, c, powerStarbase, "nostage", p.TowerID, 0,
				"has no office left to supply its tower at %s from", moonNameOf(ctx, m, p.MoonID))
		}
		return
	}
	old := p.StagingStationID
	p.StagingStationID = st
	if err := m.saveTowerPlan(ctx, p); err != nil {
		return
	}
	m.ceoLog(ctx, c, powerStarbase, "restage", p.TowerID, 0,
		"now supplies its tower at %s from %s (lost its office at %s)", moonNameOf(ctx, m, p.MoonID),
		m.stationLabel(ctx, st), m.stationLabel(ctx, old))
}

// nameStarbaseDivisions names divisions 6 and 7 for the starbase desk when
// nobody has named them.
func (m *Mod) nameStarbaseDivisions(ctx context.Context, corpID int64, corpName string) {
	for div, name := range starbaseDivisionNames {
		want := taggedDivName(deskDivArm[div], name)
		col := fmt.Sprintf("division%d", div)
		var cur sql.NullString
		if err := m.db.QueryRowContext(ctx, `SELECT `+col+` FROM corporation WHERE corporationID = ?`, corpID).Scan(&cur); err != nil {
			continue
		}
		// Empty, or the desk's own name untagged: the module's to (re)name. Anything else is a
		// human's.
		if cur.Valid && cur.String != "" && (cur.String == want || untagDivName(cur.String) != name) {
			continue
		}
		var old any
		if cur.Valid && cur.String != "" {
			old = cur.String
		}
		if _, err := m.db.ExecContext(ctx, `UPDATE corporation SET `+col+` = ? WHERE corporationID = ?`, want, corpID); err == nil {
			pushCorpChanged(corpID, corpName, esp.CorpFieldChange{Column: col, Old: old, New: want})
		}
	}
}

// releaseTowerSavings closes, once, every unplanned towersave commitment (those keyed by the
// corporation itself), so savings held against towers that can never be planned stop starving
// the powers that read free capital. A planned tower's fuel reserve is not touched.
func (m *Mod) releaseTowerSavings(ctx context.Context) error {
	have, _, err := evedb.GetServerSetting(ctx, m.db, settingTowerSavingsReleased)
	if err != nil {
		return fmt.Errorf("releaseTowerSavings: %w", err)
	}
	if have == "1" {
		return nil
	}
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_commit SET closedAt = ?
		 WHERE refKind = ? AND refID = corporationID AND closedAt = 0`, clock.Unix(), towerSaveCommit)
	if err != nil {
		return fmt.Errorf("releaseTowerSavings: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := evedb.SetServerSetting(ctx, m.db, settingTowerSavingsReleased, "1"); err != nil {
		return fmt.Errorf("releaseTowerSavings marker: %w", err)
	}
	if n > 0 {
		rlogf("rolebots: released %d corporation tower-savings commitment(s) — tower planning now starts at %s of free capital", n, iskString(towerStartISK))
	}
	return nil
}
