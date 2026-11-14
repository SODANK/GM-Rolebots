package rolebots

import (
	"context"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// Every tower's P&L, and what the CEO does about a tower that stops paying.
// Out: fuel, ammunition, structures, the tower itself. In: product at market value, booked as
// it is hauled out of silos and arrays; internal transfers count at the market price, so a
// feeder tower whose goo goes to the corporation's own hub still shows its worth.
// The weekly review reads the last seven days; two bad reviews running re-purpose or tear the
// tower down. Depots and fortresses produce nothing and are judged on use: residents, and
// the sovereignty a fortress holds down.

const (
	towerLineFuel      = "fuel"
	towerLineStructure = "structure"
	towerLineAmmo      = "ammo"
	towerLineProduct   = "product"
	towerLineSupply    = "supply"
)

// towerBook writes one line on a tower's ledger.
func (m *Mod) towerBook(ctx context.Context, p *towerPlan, line string, isk float64, typeID, qty int64, detail string) {
	if p == nil || p.PlanID == 0 || isk == 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_tower_ledger (planID, corporationID, at, line, isk, typeID, qty, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PlanID, p.CorpID, clock.Unix(), line, isk, typeID, qty, truncate(detail, 255)); err != nil {
		rlogf("rolebots: tower ledger %d: %v", p.PlanID, err)
	}
}

// towerBookReq books a requisition purchase raised for a tower.
func (m *Mod) towerBookReq(ctx context.Context, r *requisition, cost float64, qty int64) {
	if r.RefKind != "tower" || r.RefID == 0 {
		return
	}
	p, _ := m.loadTowerPlan(ctx, r.RefID)
	if p == nil {
		return
	}
	line := towerLineSupply
	switch r.Requester {
	case "fueler":
		line = towerLineFuel
	case "security":
		line = towerLineAmmo
	case "ceo":
		// The kit is booked as it is anchored (towerBook "structure"), at
		// value; its purchase is the corporation's books only.
		return
	}
	m.towerBook(ctx, p, line, -cost, r.TypeID, qty, fmt.Sprintf("%d× %s", qty, m.typeNameOr(ctx, r.TypeID)))
}

// towerBookProduct books output hauled out of a tower at market value.
func (m *Mod) towerBookProduct(ctx context.Context, planID, typeID, qty int64) {
	if planID == 0 || qty <= 0 {
		return
	}
	p, _ := m.loadTowerPlan(ctx, planID)
	if p == nil {
		return
	}
	v := evedb.TypeValueOf(ctx, m.db, typeID).Value * float64(qty)
	m.towerBook(ctx, p, towerLineProduct, v, typeID, qty, fmt.Sprintf("%d× %s", qty, m.typeNameOr(ctx, typeID)))
}

// towerLedgerLine is one ledger line summed over a window.
type towerLedgerLine struct {
	Line string
	ISK  float64
}

// towerPnL sums a tower's books since a time.
func (m *Mod) towerPnL(ctx context.Context, planID, since int64) (net float64, lines []towerLedgerLine) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT line, COALESCE(SUM(isk), 0) FROM rolebot_tower_ledger WHERE planID = ? AND at >= ? GROUP BY line`,
		planID, since)
	if err != nil {
		return 0, nil
	}
	defer rows.Close()
	for rows.Next() {
		var l towerLedgerLine
		if rows.Scan(&l.Line, &l.ISK) == nil {
			lines = append(lines, l)
			net += l.ISK
		}
	}
	return net, lines
}

// reviewTowers is the weekly look at every running tower.
func (m *Mod) reviewTowers(ctx context.Context, c *corpCEO, ch Charter, plans []*towerPlan) {
	now := clock.Unix()
	for _, p := range plans {
		if p.Status != towerRunning || now-p.ReviewAt < towerReviewEverySec {
			continue
		}
		if p.ReviewAt == 0 {
			p.ReviewAt = now // the first week is a baseline
			_ = m.saveTowerPlan(ctx, p)
			continue
		}
		if p.OnlineAt != 0 && now-p.OnlineAt < towerReviewGraceSec {
			continue // still in its grace: it is judged once it has been used
		}
		net, _ := m.towerPnL(ctx, p.PlanID, now-towerReviewEverySec)
		bad := net < 0
		switch p.Purpose {
		case purposeDepot:
			var residents int
			_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_tower_residents WHERE planID = ?`, p.PlanID).Scan(&residents)
			bad = residents == 0 && net < -m.weeklyFuelCost(ctx, p)*0.5
		case purposeFortress:
			// A fortress is judged by the sovereignty it holds down, not by ISK: bad once its system is no
			// longer a sov hold (the alliance left, the other towers went, or it was never 0.0).
			bad = !m.sovHoldSystems(ctx, c.CorpID, plans, p.PlanID)[p.SystemID]
		}
		if p.Losses >= 2 {
			bad = true
		}
		if bad {
			p.BadReviews++
		} else {
			p.BadReviews = 0
		}
		p.ReviewAt = now
		m.ceoLog(ctx, c, powerStarbase, "review", p.TowerID, net,
			"reviewed its %s tower at %s: %s this week%s", purposeLabel[p.Purpose], moonNameOf(ctx, m, p.MoonID),
			iskString(net), map[bool]string{true: fmt.Sprintf(" — bad review %d of %d", p.BadReviews, towerBadReviewsMax), false: ""}[bad])
		if p.BadReviews >= towerBadReviewsMax {
			if err := m.startTeardown(ctx, c, p, "it stopped paying for itself"); err != nil {
				m.logBot(c.CharID, logError, "ceo", "teardown: %v", err)
			}
			continue
		}
		_ = m.saveTowerPlan(ctx, p)
	}
	// A corporation that cannot fuel its towers tears the weakest down.
	m.fuelAffordabilityCheck(ctx, c, ch, plans)
}

// weeklyFuelCost is a tower's week of fuel at value.
func (m *Mod) weeklyFuelCost(ctx context.Context, p *towerPlan) float64 {
	batch, err := m.towerFuelBatch(ctx, p.TowerID, p.TowerTypeID, p.SystemID, p.CorpID, 0)
	if err != nil {
		return 0
	}
	return m.batchCost(ctx, batch, 7)
}

// fuelAffordabilityCheck tears down the worst-earning tower when the wallet
// no longer covers a week of fuel for the whole network.
func (m *Mod) fuelAffordabilityCheck(ctx context.Context, c *corpCEO, ch Charter, plans []*towerPlan) {
	var running []*towerPlan
	week := 0.0
	for _, p := range plans {
		if p.Status == towerRunning || p.Status == towerRigging {
			running = append(running, p)
			week += m.weeklyFuelCost(ctx, p)
		}
	}
	if len(running) == 0 || week <= 0 {
		return
	}
	bal, err := evedb.GetCorpBalance(ctx, m.db, c.CorpID)
	if err != nil || bal >= week {
		return
	}
	if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "fuelbroke") < 3*86400 {
		return
	}
	worst, worstNet := running[0], math.Inf(1)
	for _, p := range running {
		if net, _ := m.towerPnL(ctx, p.PlanID, clock.Unix()-14*86400); net < worstNet {
			worst, worstNet = p, net
		}
	}
	m.ceoLog(ctx, c, powerStarbase, "fuelbroke", worst.TowerID, week,
		"cannot fund a week of fuel for its towers (%s needed, %s held)", iskString(week), iskString(bal))
	if err := m.startTeardown(ctx, c, worst, "the corporation cannot fund its fuel"); err != nil {
		m.logBot(c.CharID, logError, "ceo", "teardown: %v", err)
	}
}

// ── teardown ────────────────────────────────────────────────────────────────

// startTeardown takes a tower down: offline (slaves with it), every bay and
// silo emptied back to the staging office, the structures and the tower
// unanchored, then scooped and carried home.
func (m *Mod) startTeardown(ctx context.Context, c *corpCEO, p *towerPlan, why string) error {
	if p.TowerID == 0 {
		p.Status, p.ClosedAt, p.Note = towerGone, clock.Unix(), why
		return m.closeTowerPlan(ctx, c, p, "abandoned")
	}
	rec, ok, err := evedb.GetStarbase(ctx, m.db, p.TowerID)
	if err != nil {
		return err
	}
	if ok && (rec.State == evedb.StarbaseStateOnline || rec.State == evedb.StarbaseStateOnlining) {
		if err := evedb.OfflineStarbaseFor(ctx, m.db, p.TowerID, nil); err != nil {
			return err
		}
	}
	p.Status, p.Note = towerUnanchor, why
	m.ceoLog(ctx, c, powerStarbase, "teardown", p.TowerID, 0,
		"is taking down its %s tower at %s — %s", purposeLabel[p.Purpose], moonNameOf(ctx, m, p.MoonID), why)
	m.voidReqsFor(ctx, c.CorpID, "tower", p.PlanID, "the tower is coming down")
	return m.saveTowerPlan(ctx, p)
}

// advanceTeardown runs a teardown along: empty the bays, unanchor, scoop.
func (m *Mod) advanceTeardown(ctx context.Context, c *corpCEO, p *towerPlan) error {
	office := m.officeAt(ctx, c.CorpID, p.StagingStationID)
	if p.StagingStationID == 0 || office == 0 {
		st, _ := m.corpFactoryOffice(ctx, c.CorpID)
		p.StagingStationID = st
		office = m.officeAt(ctx, c.CorpID, st)
	}
	// 1. Empty every bay (the tower's and each structure's) before anything is unanchored: a
	// structure that unanchors full carries its bay with it, and goo is worth more on a market.
	if m.openHaulFor(ctx, p.PlanID, haulKindBackhaul) == nil {
		posted := false
		for _, src := range m.towerBays(ctx, p) {
			held := m.bayHeld(ctx, src)
			var items []haulItem
			for t, q := range held {
				if q > 0 {
					items = append(items, haulItem{TypeID: t, Qty: q, Flag: -1})
				}
			}
			if len(items) == 0 || office == 0 {
				continue
			}
			if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindBackhaul, PlanID: p.PlanID, FromKind: haulFromStructure,
				FromID: src, FromSystemID: p.SystemID, ToKind: haulToOffice, ToStationID: p.StagingStationID,
				ToFlag: corpDivProduct, Items: items, PostedBy: "ceo", Note: "emptying a tower before teardown"}); err != nil {
				return err
			}
			posted = true
			break // one at a time
		}
		if posted {
			return m.saveTowerPlan(ctx, p)
		}
	} else {
		return nil
	}
	// 2. Unanchor: every structure, then the tower.
	slaves, _ := evedb.ListStarbaseStructuresForTower(ctx, m.db, p.TowerID)
	for _, s := range slaves {
		if s.State == evedb.StarbaseStateAnchored {
			_, _ = evedb.BeginStructureUnanchor(ctx, m.db, s.EntityID)
		}
	}
	if rec, ok, _ := evedb.GetStarbase(ctx, m.db, p.TowerID); ok {
		if rec.State == evedb.StarbaseStateAnchored && len(slaves) == 0 {
			if _, err := evedb.BeginStarbaseUnanchor(ctx, m.db, p.TowerID); err != nil {
				m.logBot(c.CharID, logInfo, "ceo", "unanchoring the tower at %s: %v", moonNameOf(ctx, m, p.MoonID), err)
			}
		}
		return m.saveTowerPlan(ctx, p)
	}
	// 3. The tower is free in space: scoop it and its structures home.
	if m.openHaulFor(ctx, p.PlanID, haulKindScoop) != nil {
		return nil
	}
	var items []haulItem
	ids := []int64{p.TowerID}
	for _, pc := range p.Layout.Pieces {
		if pc.ItemID != 0 {
			ids = append(ids, pc.ItemID)
		}
	}
	for _, id := range ids {
		var typeID, loc int64
		if m.db.QueryRowContext(ctx, `SELECT typeID, locationID FROM entity WHERE itemID = ?`, id).Scan(&typeID, &loc) != nil {
			continue
		}
		if loc != p.SystemID {
			continue // already home, or gone
		}
		var anchored int
		_ = m.db.QueryRowContext(ctx, `
			SELECT (SELECT COUNT(*) FROM starbase WHERE entityID = ?) + (SELECT COUNT(*) FROM starbaseStructures WHERE entityID = ?)`,
			id, id).Scan(&anchored)
		if anchored > 0 {
			return nil // still unanchoring
		}
		items = append(items, haulItem{TypeID: typeID, Qty: 1, Flag: -1, ItemID: id})
	}
	if len(items) == 0 {
		p.Status, p.ClosedAt = towerGone, clock.Unix()
		m.ceoLog(ctx, c, powerStarbase, "teardown", p.TowerID, 0,
			"finished taking down its tower at %s", moonNameOf(ctx, m, p.MoonID))
		return m.closeTowerPlan(ctx, c, p, "taken down")
	}
	if office == 0 {
		return nil
	}
	_, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindScoop, PlanID: p.PlanID, FromKind: haulFromSpace,
		FromSystemID: p.SystemID, ToKind: haulToOffice, ToStationID: p.StagingStationID, ToFlag: corpDivStarbase,
		Items: items, PostedBy: "ceo", Note: "scooping a taken-down tower"})
	return err
}

// towerBays is the tower and every structure with something in it.
func (m *Mod) towerBays(ctx context.Context, p *towerPlan) []int64 {
	ids, _ := int64Column(ctx, m, `
		SELECT DISTINCT e.locationID FROM entity e
		 WHERE e.locationID = ? OR e.locationID IN (SELECT entityID FROM starbaseStructures WHERE towerID = ?)`,
		p.TowerID, p.TowerID)
	return ids
}

// sweepHauls closes legs nobody will finish: past their deadline by
// haulStaleSec with nothing loaded, or whose carrier left the corporation. A claim
// with nothing loaded whose carrier is logged off (carrierAway) goes back on the board;
// the carrier's errand finds the claim gone when it is back (posHaulStep).
func (m *Mod) sweepHauls(ctx context.Context, corpID int64) {
	legs, err := m.queryHauls(ctx, `WHERE corporationID = ? AND status IN ('open','claimed')`, corpID)
	if err != nil {
		return
	}
	now := clock.Unix()
	for _, l := range legs {
		if l.CarrierID != 0 {
			var corpNow int64
			_ = m.db.QueryRowContext(ctx, `SELECT corporationID FROM character_ WHERE characterID = ?`, l.CarrierID).Scan(&corpNow)
			if corpNow != corpID {
				if l.carryingAny() {
					m.closeHaul(ctx, l, "lost", "the carrier left the corporation with the cargo")
				} else {
					m.releaseHaul(ctx, l)
				}
				continue
			}
			if !l.carryingAny() && carrierAway(l.CarrierID) != "" {
				m.releaseHaul(ctx, l)
				continue
			}
		}
		if !l.carryingAny() && now > l.Deadline+haulStaleSec {
			m.closeHaul(ctx, l, "void", "nobody carried it")
		}
	}
}
