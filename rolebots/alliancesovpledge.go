package rolebots

// The sov-tower pledge: the alliance pays for one member's tower in a system it means to hold.
//
// allianceSovPledge (the executor) picks a free target with a usable moon and a builder
// (pickSovPledgeBuilder), prices a Small tower (towerBuildCost) and raises the builder's shortfall as
// 'sovtower' tithe rows payable to it (sovPledgeShares). The builder holds the money under a
// sovPledgeCommit that towerFreeCapital adds back; considerNewTower plants it in the pledged system,
// one over the cap; planTower links the plan (linkSovPledge); settleSovPledges closes it.
// Switch: rolebots.alliance.frontier. Design note: docs/ALLIANCES.md, "The sov-tower pledge".

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

const (
	titheKindSovTower = "sovtower"  // rolebot_alliance_ledger.kind
	sovPledgeCommit   = "sovpledge" // rolebot_corp_commit.refKind on the builder, refID = pledgeID

	sovPledgeOpen    = "open"    // raising money, no plan yet
	sovPledgePlanned = "planned" // the builder planned the tower; waiting for it to stand
	sovPledgeDone    = "done"    // the tower is rigging or running
	sovPledgeFailed  = "failed"  // the plan died before the tower stood
	sovPledgeExpired = "expired" // no plan within sovPledgeTTLSec; paid shares repaid
	sovPledgeVoid    = "void"    // the builder left the alliance

	// sovPledgeTTLSec is how long a pledge waits for its plan.
	sovPledgeTTLSec = 14 * 86400
	// sovPledgeRetrySec is how long the executor rests after a pass that found nothing to pledge.
	sovPledgeRetrySec = 6 * 3600
)

// sovPledgeTried is when each alliance last looked for a pledge and found none (allianceID to unix).
var sovPledgeTried sync.Map

// sovPledge is one rolebot_alliance_sovpledge row.
type sovPledge struct {
	ID, AllianceID, BuilderCorpID int64
	SystemID, MoonID              int64
	Total, Shortfall              float64
	Status                        string
	PlanID, CreatedAt, ExpiresAt  int64
}

const sovPledgeCols = `pledgeID, allianceID, builderCorpID, systemID, moonID, total, shortfall, status, planID, createdAt, expiresAt`

func scanSovPledge(sc interface{ Scan(...any) error }) (*sovPledge, error) {
	p := &sovPledge{}
	if err := sc.Scan(&p.ID, &p.AllianceID, &p.BuilderCorpID, &p.SystemID, &p.MoonID, &p.Total, &p.Shortfall,
		&p.Status, &p.PlanID, &p.CreatedAt, &p.ExpiresAt); err != nil {
		return nil, err
	}
	return p, nil
}

// openSovPledgeFor is the open pledge naming the corporation as builder, nil with none or when the
// corporation has left the pledging alliance.
func (m *Mod) openSovPledgeFor(ctx context.Context, corpID int64) *sovPledge {
	p, err := scanSovPledge(m.db.QueryRowContext(ctx, `
		SELECT `+sovPledgeCols+` FROM rolebot_alliance_sovpledge
		 WHERE builderCorpID = ? AND status = ? ORDER BY pledgeID LIMIT 1`, corpID, sovPledgeOpen))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			rlogf("rolebots: sov pledge of corp %d: %v", corpID, err)
		}
		return nil
	}
	if m.corpAllianceID(ctx, corpID) != p.AllianceID {
		return nil
	}
	return p
}

// sovPledgeHeld is the pledge money the corporation holds for its tower: its open sovPledgeCommit rows.
func (m *Mod) sovPledgeHeld(ctx context.Context, corpID int64) float64 {
	var held float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(GREATEST(amount - spent, 0)), 0) FROM rolebot_corp_commit
		 WHERE corporationID = ? AND refKind = ? AND closedAt = 0`, corpID, sovPledgeCommit).Scan(&held)
	return held
}

// towerFreeCapital is what a corporation may put into a tower: its free capital and the pledge money it
// holds for one.
func (m *Mod) towerFreeCapital(ctx context.Context, corpID int64, ch Charter) (float64, error) {
	free, _, err := m.corpFreeCapital(ctx, corpID, ch)
	if err != nil {
		return 0, err
	}
	return free + m.sovPledgeHeld(ctx, corpID), nil
}

// sovPledgePaidIn is what the members have paid toward a pledge.
func (m *Mod) sovPledgePaidIn(ctx context.Context, pledgeID int64) float64 {
	var paid float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM rolebot_alliance_ledger
		 WHERE kind = ? AND refID = ? AND status = ?`, titheKindSovTower, pledgeID, titheStatusPaid).Scan(&paid)
	return paid
}

// refreshSovPledgeCommit sets the builder's commit to what has been paid in, while the pledge is open
// (payTithes, after a share is paid).
func (m *Mod) refreshSovPledgeCommit(ctx context.Context, builder, pledgeID int64) {
	var status string
	_ = m.db.QueryRowContext(ctx, `SELECT status FROM rolebot_alliance_sovpledge WHERE pledgeID = ?`, pledgeID).Scan(&status)
	if status != sovPledgeOpen {
		return
	}
	_ = m.openCommit(ctx, builder, sovPledgeCommit, pledgeID, m.sovPledgePaidIn(ctx, pledgeID))
}

// sovPledgeWaitLine is the builder's outcome while the money comes in.
func (m *Mod) sovPledgeWaitLine(ctx context.Context, p *sovPledge) string {
	return fmt.Sprintf("waits on its alliance's pledge for a tower at %s: %s of %s paid in",
		systemNameOf(ctx, m, p.SystemID), iskString(m.sovPledgePaidIn(ctx, p.ID)), iskString(p.Shortfall))
}

// linkSovPledge ties a new plan to the builder's open pledge when it stands in the pledged system: the
// money goes into the plan (its commit closes) and the pledge waits on the tower.
func (m *Mod) linkSovPledge(ctx context.Context, c *corpCEO, plan *towerPlan) {
	p := m.openSovPledgeFor(ctx, c.CorpID)
	if p == nil || plan.SystemID != p.SystemID {
		return
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_alliance_sovpledge SET status = ?, planID = ? WHERE pledgeID = ? AND status = ?`,
		sovPledgePlanned, plan.PlanID, p.ID, sovPledgeOpen)
	m.closeCommit(ctx, sovPledgeCommit, p.ID)
	m.allianceBook(ctx, p.AllianceID, c.CorpID, "sovpledge", p.SystemID, p.Total,
		"%s planned the tower the alliance pledged for at %s", ownerNameOf(ctx, m.db, c.CorpID), systemNameOf(ctx, m, p.SystemID))
}

// ── the executor's pass ─────────────────────────────────────────────────────

// sovPledgeCand is a member that could build the pledged tower.
type sovPledgeCand struct {
	CorpID int64
	Free   float64
	Jumps  int // from its nearest office to the system
}

// pickSovPledgeBuilder is the member with the most free capital, then the fewest jumps, then the lowest
// corpID; 0 with none. Pure.
func pickSovPledgeBuilder(cands []sovPledgeCand) int64 {
	if len(cands) == 0 {
		return 0
	}
	cs := append([]sovPledgeCand(nil), cands...)
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Free != cs[j].Free {
			return cs[i].Free > cs[j].Free
		}
		if cs[i].Jumps != cs[j].Jumps {
			return cs[i].Jumps < cs[j].Jumps
		}
		return cs[i].CorpID < cs[j].CorpID
	})
	return cs[0].CorpID
}

// sovPledgeShortfall is what the builder lacks of the total, in whole ISK, never below 0. Pure.
func sovPledgeShortfall(total, free float64) float64 {
	return math.Max(0, math.Ceil(total-math.Max(free, 0)))
}

// sovPledgeShares splits the shortfall among the paying members (every member but the builder and the
// human-run ones, who have no think to pay from) by pilots, whole ISK; the rounding remainder goes to the
// largest share. Empty when nobody can pay or nothing is owed. Pure.
func sovPledgeShares(members []allianceMemberFacts, builder int64, shortfall float64) map[int64]float64 {
	out := map[int64]float64{}
	if shortfall <= 0 {
		return out
	}
	var payers []allianceMemberFacts
	var heads float64
	for _, f := range members {
		if f.CorpID == builder || f.HumanCEO {
			continue
		}
		payers = append(payers, f)
		heads += math.Max(1, float64(f.Pilots))
	}
	if len(payers) == 0 {
		return out
	}
	var dealt float64
	var top int64
	for _, f := range payers {
		s := math.Floor(shortfall * math.Max(1, float64(f.Pilots)) / heads)
		out[f.CorpID] = s
		dealt += s
		if top == 0 || s > out[top] || (s == out[top] && f.CorpID < top) {
			top = f.CorpID
		}
	}
	out[top] += shortfall - dealt
	return out
}

// allianceSovPledge is the executor's pledge pass: it settles the alliance's pledges, then raises a new
// one when none is open and a target can take a tower.
func (m *Mod) allianceSovPledge(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) error {
	m.settleSovPledges(ctx, seat)
	var open int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_alliance_sovpledge WHERE allianceID = ? AND status IN (?, ?)`,
		seat.AllianceID, sovPledgeOpen, sovPledgePlanned).Scan(&open)
	if open > 0 {
		return nil
	}
	now := clock.Unix()
	if v, ok := sovPledgeTried.Load(seat.AllianceID); ok && now-v.(int64) < sovPledgeRetrySec {
		return nil
	}
	sovPledgeTried.Store(seat.AllianceID, now)
	targets := m.allianceFrontierSystems(ctx, seat.AllianceID)
	if len(targets) == 0 {
		return nil
	}
	// A target where a member already plans or holds a tower needs no pledge.
	if ids := memberCorpIDs(members); len(ids) > 0 {
		taken, _ := int64Column(ctx, m, `
			SELECT DISTINCT systemID FROM rolebot_corp_towers
			 WHERE corporationID IN (`+int64CSV(ids)+`) AND status <> 'gone'`)
		owned, _ := int64Column(ctx, m, `
			SELECT DISTINCT solarSystemID FROM starbase WHERE ownerCorpID IN (`+int64CSV(ids)+`)`)
		for _, s := range append(taken, owned...) {
			delete(targets, s)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	// The best usable moon the alliance has surveyed in a free target (intel is shared alliance-wide).
	hq := int64(0)
	if facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID); err == nil {
		hq = facts.StationID
	}
	floor := 0.0
	if hq != 0 {
		floor = m.miningValueFloor(ctx, c, ch, hq)
	}
	intel, err := m.corpMoonIntel(ctx, c.CorpID, moonR4)
	if err != nil {
		return err
	}
	occupied := m.occupiedMoons(ctx)
	var moon *moonIntel
	for i := range intel {
		mi := &intel[i]
		if targets[mi.SystemID] && !occupied[mi.MoonID] && mi.Value > 0 && mi.Value >= floor &&
			(moon == nil || mi.Value > moon.Value) {
			moon = mi
		}
	}
	if moon == nil {
		return nil // the surveys have not found one yet (considerNewTower keeps them going)
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return err
	}
	type builderCtx struct {
		ceo   *corpCEO
		ch    Charter
		facts *CorpFacts
	}
	ctxOf := map[int64]builderCtx{}
	var cands []sovPledgeCand
	for _, f := range members {
		if f.HumanCEO {
			continue
		}
		bc, err := getCEO(ctx, m.db, f.CorpID)
		if err != nil || bc == nil {
			continue
		}
		bch := ceoCharterOf(ctx, m, bc)
		if want, _ := m.frontierAmbition(ctx, bc, bch); !want {
			continue
		}
		facts, err := getAnyCorpFacts(ctx, m.db, f.CorpID)
		if err != nil {
			continue
		}
		bhq := facts.StationID
		if bhq == 0 {
			bhq, _ = m.corpFactoryOffice(ctx, f.CorpID)
		}
		if bhq == 0 {
			continue
		}
		mems, _ := m.corpMembers(ctx, f.CorpID, 0)
		eligible := 0
		for _, cm := range mems {
			if cm.Eligible() {
				eligible++
			}
		}
		if maxTowersFor(eligible) == 0 {
			continue
		}
		reach, _ := m.corpPlatformReach(ctx, f.CorpID, bhq, u, unlimitedJumps)
		j, ok := reach[moon.SystemID]
		if !ok {
			continue
		}
		free, _, err := m.corpFreeCapital(ctx, f.CorpID, bch)
		if err != nil {
			continue
		}
		cands = append(cands, sovPledgeCand{CorpID: f.CorpID, Free: free, Jumps: j})
		ctxOf[f.CorpID] = builderCtx{bc, bch, facts}
	}
	builder := pickSovPledgeBuilder(cands)
	if builder == 0 {
		return nil
	}
	b := ctxOf[builder]
	var free float64
	for _, cd := range cands {
		if cd.CorpID == builder {
			free = cd.Free
		}
	}
	// A Small mining tower on the moon, as planTower would build it, with its margin.
	ids := make([]int64, 0, len(moon.Materials))
	for _, mm := range moon.Materials {
		ids = append(ids, mm.TypeID)
	}
	unit := map[int64]float64{}
	if vals, err := evedb.TypeValues(ctx, m.db, ids); err == nil {
		for _, id := range ids {
			unit[id] = vals[id].Value
		}
	}
	race := pickTowerRace(m.charRace(ctx, b.ceo.CharID), b.ch.Xenophobia, m.isotopeCostNear(ctx, moon.SystemID))
	tags := b.facts.Policy.Tags
	_, _, kit, keep, err := m.towerBuildCost(ctx, layoutInput{Purpose: purposeMining, Size: towerSmall, Race: race,
		Materials: materialsByValue(moon.Materials, unit), Tags: tags,
		Arrays: m.purposeArrays(ctx, builder, purposeMining, tags), Seed: moon.MoonID}, moon.SystemID, builder)
	if err != nil {
		return err
	}
	total := math.Ceil((kit + keep) * towerNeedMargin)
	shortfall := sovPledgeShortfall(total, free)
	shares := sovPledgeShares(members, builder, shortfall)
	if shortfall > 0 && len(shares) == 0 {
		return nil // nobody else to pay: the builder saves on its own
	}
	sys := systemNameOf(ctx, m, moon.SystemID)
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_alliance_sovpledge
		  (allianceID, builderCorpID, systemID, moonID, total, shortfall, status, createdAt, expiresAt, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seat.AllianceID, builder, moon.SystemID, moon.MoonID, total, shortfall, sovPledgeOpen, now, now+sovPledgeTTLSec,
		truncate(fmt.Sprintf("a Small tower at %s for %s", moonNameOf(ctx, m, moon.MoonID), ownerNameOf(ctx, m.db, builder)), 255))
	if err != nil {
		return fmt.Errorf("sov pledge: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := m.titheIssue(ctx, seat.AllianceID, builder, shares, titheKindSovTower, id,
		"pledge toward a tower holding "+sys); err != nil {
		return err
	}
	sovPledgeTried.Delete(seat.AllianceID)
	line := fmt.Sprintf("asks the alliance to fund %s's tower at %s: %s to raise among %d member(s), %s in all",
		ownerNameOf(ctx, m.db, builder), sys, iskString(shortfall), len(shares), iskString(total))
	if shortfall == 0 {
		line = fmt.Sprintf("names %s to build the alliance's tower at %s (%s, which it can pay itself)",
			ownerNameOf(ctx, m.db, builder), sys, iskString(total))
	}
	m.ceoLog(ctx, c, powerAlliance, "sovpledge", moon.SystemID, shortfall, "%s", line)
	m.allianceBook(ctx, seat.AllianceID, c.CorpID, "sovpledge", moon.SystemID, shortfall, "%s", line)
	m.allianceBulletin(ctx, seat.AllianceID, c.CharID, fmt.Sprintf("%s: a tower for %s", seat.Name, sys),
		fmt.Sprintf("The executor asks the members to fund a tower at %s, built by %s. Each member's share is in the alliance ledger and falls due within a week. A tower online there counts for the alliance's claim.",
			moonNameOf(ctx, m, moon.MoonID), ownerNameOf(ctx, m.db, builder)))
	markPosWork(builder)
	wakeCEO(builder)
	return nil
}

// settleSovPledges closes what is finished: a planned pledge whose tower stands (done) or whose plan died
// (failed), an open one past expiry (expired, repaid) and one whose builder left the alliance (void,
// repaid as far as it can).
func (m *Mod) settleSovPledges(ctx context.Context, seat *allianceSeat) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT `+sovPledgeCols+` FROM rolebot_alliance_sovpledge WHERE allianceID = ? AND status IN (?, ?)`,
		seat.AllianceID, sovPledgeOpen, sovPledgePlanned)
	if err != nil {
		return
	}
	var list []*sovPledge
	for rows.Next() {
		if p, err := scanSovPledge(rows); err == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	now := clock.Unix()
	for _, p := range list {
		sys := systemNameOf(ctx, m, p.SystemID)
		switch {
		case m.corpAllianceID(ctx, p.BuilderCorpID) != p.AllianceID:
			m.closeSovPledge(ctx, p, sovPledgeVoid, "the builder left the alliance", true)
			m.allianceBook(ctx, p.AllianceID, p.BuilderCorpID, "sovpledge", p.SystemID, 0,
				"the pledge for a tower at %s is void: %s left the alliance", sys, ownerNameOf(ctx, m.db, p.BuilderCorpID))
		case p.Status == sovPledgeOpen && now >= p.ExpiresAt:
			m.closeSovPledge(ctx, p, sovPledgeExpired, "no tower was planned in time", true)
			m.allianceBook(ctx, p.AllianceID, p.BuilderCorpID, "sovpledge", p.SystemID, 0,
				"the pledge for a tower at %s expired unplanned; %s repays what was paid in", sys, ownerNameOf(ctx, m.db, p.BuilderCorpID))
		case p.Status == sovPledgePlanned:
			var status string
			_ = m.db.QueryRowContext(ctx, `SELECT status FROM rolebot_corp_towers WHERE planID = ?`, p.PlanID).Scan(&status)
			switch status {
			case towerRigging, towerRunning:
				m.closeSovPledge(ctx, p, sovPledgeDone, "the tower stands", false)
				m.allianceBook(ctx, p.AllianceID, p.BuilderCorpID, "sovpledge", p.SystemID, p.Total,
					"the tower the alliance funded at %s is online", sys)
			case towerGone, "":
				m.closeSovPledge(ctx, p, sovPledgeFailed, "the plan died before the tower stood", false)
				m.allianceBook(ctx, p.AllianceID, p.BuilderCorpID, "sovpledge", p.SystemID, 0,
					"the tower the alliance funded at %s never stood", sys)
			}
		}
	}
}

// closeSovPledge ends a pledge: unpaid shares are voided, the builder's commit closes and, with repay,
// paid shares go back to their payers as far as the builder can carry them.
func (m *Mod) closeSovPledge(ctx context.Context, p *sovPledge, status, why string, repay bool) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_alliance_ledger SET status = ?, reason = LEFT(CONCAT(reason, ' — ', ?), 255)
		 WHERE kind = ? AND refID = ? AND status = ?`,
		titheStatusVoid, why, titheKindSovTower, p.ID, titheStatusPending)
	m.closeCommit(ctx, sovPledgeCommit, p.ID)
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_alliance_sovpledge SET status = ?, closedAt = ?, note = LEFT(CONCAT(note, ' — ', ?), 255)
		 WHERE pledgeID = ?`, status, clock.Unix(), why, p.ID)
	if repay {
		m.repaySovPledge(ctx, p)
	}
}

// repaySovPledge returns each paid share from the builder to its payer, marked repaid; a share the
// builder cannot cover stays paid.
func (m *Mod) repaySovPledge(ctx context.Context, p *sovPledge) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT titheID, debtorCorpID, amount FROM rolebot_alliance_ledger
		 WHERE kind = ? AND refID = ? AND executorCorpID = ? AND status = ?`,
		titheKindSovTower, p.ID, p.BuilderCorpID, titheStatusPaid)
	if err != nil {
		return
	}
	type share struct {
		id, debtor int64
		amount     float64
	}
	var list []share
	for rows.Next() {
		var s share
		if rows.Scan(&s.id, &s.debtor, &s.amount) == nil {
			list = append(list, s)
		}
	}
	rows.Close()
	sys := systemNameOf(ctx, m, p.SystemID)
	for _, s := range list {
		if s.amount < 1 {
			continue
		}
		reason := evedb.JournalReason("Alliance pledge repaid — " + sys)
		if _, _, err := m.corpWithdraw(ctx, p.BuilderCorpID, s.debtor, s.amount, reason); err != nil {
			continue
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_alliance_ledger SET status = ? WHERE titheID = ?`, titheStatusRepaid, s.id)
		m.corpBook(ctx, p.BuilderCorpID, lineTithe, -s.amount, s.debtor, reason)
		m.corpBook(ctx, s.debtor, lineTithe, s.amount, p.BuilderCorpID, reason)
	}
}
