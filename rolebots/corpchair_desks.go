package rolebots

import (
	"context"
	"math"
	"sort"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// The human chair's desks: the pass over the corporations a person runs (corpchair.go).
//
// Every corporation's boards are kept: lines handed to buyers, claims that went stale taken back,
// finished rows closed. Then each division the CEO has put a bot in charge of (corpheads.go) runs its
// desk, with the same code a bot CEO's departments run, in the head's name. A division headed by a
// person, or by nobody, orders nothing by itself.
//
// What never runs here whoever is in charge: the think, tax, payroll, dividends and shares, offices
// and headquarters, identity, diplomacy, war, sieges, planting or taking down a starbase, locking
// prints down. What touches people or goods beyond the division's own orders (listing hangar stock,
// answering applications, stipends) runs only where the appointment says so (headFlag*).

const (
	// chairDeskTimeout bounds one corporation's pass, chairProdTimeout its production stage.
	chairDeskTimeout = 90 * time.Second
	chairProdTimeout = 60 * time.Second
	chairPassTimeout = 5 * time.Minute

	// chairProdEverySec is the production review's cadence, a bot CEO's think's.
	chairProdEverySec = 2 * 3600
	// chairBasesEverySec is how often the offices' purposes are derived again, and chairSeatsEverySec
	// how often a bot head deals its fleet seats.
	chairBasesEverySec = 3600
	chairSeatsEverySec = 24 * 3600
	chairSalesEverySec = 3600
	chairFundsEverySec = 6 * 3600
)

var (
	humanChairBusy atomic.Bool
	humanChairNext atomic.Int64 // where the last pass that ran out of time stopped
)

// startHumanChairSweep registers the pass, on the officers' cadence and gate.
func (m *Mod) startHumanChairSweep() {
	m.registerPeriodic("rolebot-human-chair", staffDeskEveryTicks, func(uint64) bool {
		if !Master() || !humanChairBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("human chair sweep", func() {
			defer humanChairBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), chairPassTimeout)
			defer cancel()
			m.humanChairPass(ctx)
		})
		return true
	})
}

// humanChairPass walks every human-chaired corporation, from where the last pass stopped.
func (m *Mod) humanChairPass(ctx context.Context) {
	ids := humanChairCorpIDs()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, corpID := range rotateFrom(ids, humanChairNext.Load()) {
		if ctx.Err() != nil {
			humanChairNext.Store(corpID)
			return
		}
		lk := corpOpLock(corpID)
		if !lk.TryLock() {
			continue // the order desk has it; next pass
		}
		func() {
			defer lk.Unlock()
			cctx, cancel := context.WithTimeout(withOpLockHeld(ctx), chairDeskTimeout)
			defer cancel()
			m.humanChairDesks(cctx, corpID)
		}()
	}
	humanChairNext.Store(0)
}

// rotateFrom is a sorted list started at the first entry not under from. Pure.
func rotateFrom(ids []int64, from int64) []int64 {
	i := sort.Search(len(ids), func(i int) bool { return ids[i] >= from })
	out := make([]int64, 0, len(ids))
	out = append(out, ids[i:]...)
	return append(out, ids[:i]...)
}

// humanChairDesks is one corporation's turn.
func (m *Mod) humanChairDesks(ctx context.Context, corpID int64) {
	m.pruneHeads(ctx, corpID)
	c := m.deskCEO(ctx, corpID)
	if c == nil {
		return
	}
	ctx = withThinkCache(ctx, corpID)
	ctx = m.withChairStaff(ctx, c)
	ch := ceoCharterOf(ctx, m, c)
	if deskDue(corpID, "chair.bases", chairBasesEverySec) {
		if _, err := m.syncOfficePurposes(ctx, corpID); err != nil {
			rlogf("rolebots: human chair bases of corp %d: %v", corpID, err)
		}
	}
	heads, err := m.corpHeadsErr(ctx, corpID)
	if err != nil {
		return
	}
	m.chairHangars(ctx, c)
	m.chairPayroll(ctx, c, heads)
	if deskDue(corpID, "chair.leftovers", chairBasesEverySec) {
		m.chairLeftovers(ctx, c, heads)
	}
	m.chairBoards(ctx, c, ch)
	for _, a := range armOrder {
		h, ok := heads[a]
		if !ok || ctx.Err() != nil || !m.botFlown(h.CharID) || popIsOffline(h.CharID) {
			continue
		}
		hc := humanChairCEO(corpID, h.CharID)
		actor := staffActor{CharID: h.CharID, Name: m.charNameOr(ctx, h.CharID), Office: "head of " + armLabel(a)}
		if err := m.chairDesk(withStaffActor(ctx, actor), hc, ch, h); err != nil {
			m.logBot(h.CharID, logError, "ceo", "%s desk: %v", armLabel(a), err)
		}
	}
}

// leftoverCutoff is when the desks now running began: what a desk posted before that was posted for a
// chair that is gone. Now, when no bot heads anything. Pure but for the bot test.
func leftoverCutoff(heads map[corpArm]corpHead, bot func(int64) bool, now int64) int64 {
	cutoff := now
	for _, h := range heads {
		if bot(h.CharID) && h.Since < cutoff {
			cutoff = h.Since
		}
	}
	return cutoff
}

// chairLeftovers takes off the boards what a bot CEO's desks posted before a person took the chair:
// this wallet pays cash for what its own chair and heads ask for. What a pilot already has in hand
// runs its course.
func (m *Mod) chairLeftovers(ctx context.Context, c *corpCEO, heads map[corpArm]corpHead) {
	cutoff := leftoverCutoff(heads, m.botFlown, clock.Unix())
	if ws, err := m.queryWork(ctx, `
		WHERE corporationID = ? AND status = 'open' AND source <> 'human' AND reqID = 0 AND postedAt < ?`, c.CorpID, cutoff); err == nil {
		for _, w := range ws {
			if !m.workHasClaims(ctx, w.WorkID) {
				m.closeWork(ctx, w.WorkID, workVoid)
			}
		}
	}
	if reqs, err := m.queryReqs(ctx, `
		WHERE corporationID = ? AND status IN ('open', 'bidding', 'building') AND refKind <> ? AND openedAt < ?
		  AND NOT EXISTS (SELECT 1 FROM rolebot_corp_commands k
		                   WHERE k.corporationID = rolebot_corp_requisition.corporationID AND k.refKind = 'req'
		                     AND k.refID = rolebot_corp_requisition.reqID)`, c.CorpID, refKindCommand, cutoff); err == nil {
		for i := range reqs {
			m.closeReq(ctx, &reqs[i], "void", "the chair changed hands")
		}
	}
	if legs, err := m.queryHauls(ctx, `
		WHERE corporationID = ? AND status = 'open' AND fromKind = ? AND postedBy <> ? AND postedAt < ?`,
		c.CorpID, haulFromOffice, haulByChair, cutoff); err == nil {
		for _, l := range legs {
			m.closeHaul(ctx, l, "void", "the chair changed hands")
		}
	}
	if moves, err := m.printMovesWhere(ctx, `
		WHERE corporationID = ? AND status = 'open' AND postedAt < ?
		  AND NOT EXISTS (SELECT 1 FROM rolebot_corp_commands k
		                   WHERE k.corporationID = rolebot_corp_print_moves.corporationID AND k.refKind = 'printmove'
		                     AND k.refID = rolebot_corp_print_moves.moveID)`, c.CorpID, cutoff); err == nil {
		for i := range moves {
			m.closePrintMove(ctx, &moves[i], moveVoid, "the chair changed hands")
		}
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_survey SET status = 'void', closedAt = ?
		 WHERE corporationID = ? AND status = 'open' AND postedAt < ? AND surveyID NOT IN (`+m.chairSurveys(ctx, c.CorpID)+`)`,
		clock.Unix(), c.CorpID, cutoff)
}

// chairBoards keeps a corporation's boards whoever heads what: they carry the chair's orders.
func (m *Mod) chairBoards(ctx context.Context, c *corpCEO, ch Charter) {
	m.sweepHauls(ctx, c.CorpID)
	m.sweepSurveys(ctx, c.CorpID)
	m.sweepPOs(ctx, c.CorpID)
	m.sweepPrintMoves(ctx, c)
	m.sweepCorpBids(ctx, c)
	if assignmentsOn() {
		if err := m.assignProgressPatrols(ctx, c); err != nil {
			rlogf("rolebots: human chair patrols of corp %d: %v", c.CorpID, err)
		}
		if err := m.assignExpire(ctx, c); err != nil {
			rlogf("rolebots: human chair assignments of corp %d: %v", c.CorpID, err)
		}
	}
	buyer := c.CharID
	if procurers := m.seatHolders(ctx, c.CorpID, seatProcurer); len(procurers) > 0 {
		buyer = m.seatExecutor(ctx, c, procurers[0])
	}
	if err := m.procurerDeskLines(ctx, c, ch, buyer, !StarbasePower()); err != nil {
		rlogf("rolebots: human chair requisitions of corp %d: %v", c.CorpID, err)
	}
	if _, err := m.tradeDeskSweep(ctx, c); err != nil {
		rlogf("rolebots: human chair trade runs of corp %d: %v", c.CorpID, err)
	}
	// The marks the pilots' plans read are memory, so a restart empties them.
	if m.corpHasOpenPosWork(ctx, c.CorpID) {
		markPosWork(c.CorpID)
	} else {
		clearPosWork(c.CorpID)
	}
}

// chairDeskNames is what a bot head's desk runs, for the page and the test: the division's own
// stages, and its option only when the appointment ticks it. Pure.
func chairDeskNames(h corpHead) []string {
	switch h.Arm {
	case armMining:
		return []string{"operations", "seats"}
	case armSecurity:
		return []string{"security", "seats"}
	case armIndustry:
		return []string{"build"}
	case armResearch:
		return []string{"research"}
	case armSales:
		out := []string{"orders", "trade"}
		if h.Flags&headFlagListStock != 0 {
			out = append(out, "list")
		}
		return out
	case armInfrastructure:
		return []string{"starbases"}
	case armHR:
		out := []string{"headhunt"}
		if h.Flags&headFlagApplicant != 0 {
			out = append(out, "applications")
		}
		return out
	case armFinance:
		if h.Flags&headFlagStipend != 0 {
			return []string{"stipends"}
		}
	}
	return nil
}

// chairDesk runs one bot head's desk.
func (m *Mod) chairDesk(ctx context.Context, c *corpCEO, ch Charter, h corpHead) error {
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	var sm *staffMember
	for _, d := range m.staffFor(ctx, c).Directors {
		if d.CharID == h.CharID {
			sm = d
		}
	}
	for _, desk := range chairDeskNames(h) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var err error
		switch desk {
		case "operations":
			if sm != nil {
				err = m.operationsDesk(ctx, c, ch, sm, facts)
			}
		case "security":
			if sm != nil {
				err = m.securityDesk(ctx, c, ch, sm, facts)
			}
		case "seats":
			if deskDue(c.CorpID, "chair.seats", chairSeatsEverySec) {
				_, err = m.promoteCorp(ctx, c.CorpID)
			}
		case "build", "research":
			err = m.chairProduction(ctx, c, ch, desk)
		case "orders":
			err = m.ceoSweepCorpOrders(ctx, c)
		case "trade":
			err = m.tradeDesk(ctx, c, ch)
		case "list":
			if deskDue(c.CorpID, "chair.list", chairSalesEverySec) {
				if err = m.ceoListCorpStock(ctx, c); err == nil {
					err = m.ceoListCorpSpoils(ctx, c)
				}
			}
		case "starbases":
			if sm != nil {
				err = m.infraDesk(ctx, c, ch, sm)
			}
		case "headhunt":
			if sm != nil && deskDue(c.CorpID, "headhunt", headhuntEverySec) {
				err = m.headhunt(ctx, c, ch, sm, facts)
			}
		case "applications":
			err = m.workHRDesk(ctx, c, ch, hrReader{CharID: h.CharID, Accepts: hrOfficerPerPass, DelaySec: hrOfficerDelaySec})
		case "stipends":
			err = m.chairStipends(ctx, c, ch)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// chairProduction is one head's half of the production review, on a bot CEO's cadence: R&D researches
// and copies, Industry builds and buys what its orders are short of.
func (m *Mod) chairProduction(ctx context.Context, c *corpCEO, ch Charter, stage string) error {
	if !deskDue(c.CorpID, "chair.prod."+stage, chairProdEverySec) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, chairProdTimeout)
	defer cancel()
	w, err := m.prodOpen(ctx, c, ch)
	if err != nil || w == nil {
		return err
	}
	if stage == "research" {
		m.prodResearch(ctx, c, ch, w, nil)
		return nil
	}
	m.prodBuild(ctx, c, ch, w)
	m.prodProcure(ctx, c, ch, w)
	return nil
}

// chairStipends pays the stipends the Budget order sets, when one is in force: nothing is handed out
// on the neutral charter's own leaning.
func (m *Mod) chairStipends(ctx context.Context, c *corpCEO, ch Charter) error {
	if m.humanHold(ctx, c.CorpID, cmdKindBudget) == nil || !deskDue(c.CorpID, "chair.stipends", chairFundsEverySec) {
		return nil
	}
	balance, err := evedb.GetCorpBalance(ctx, m.db, c.CorpID)
	if err != nil || balance < ceoStipendFloorISK {
		return err
	}
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return err
	}
	budget := math.Max(0, ch.StipendShare*free-m.ceoSpentSince(ctx, c.CorpID, "stipend", clock.Unix()-ceoStipendWeek))
	if budget < 1 {
		return nil
	}
	// ceoStipends leaves out its CharID, the CEO: here that is the person, and nothing else of it
	// acts as them.
	cc := *c
	cc.CharID = m.corpCEOID(ctx, c.CorpID)
	return m.ceoStipends(ctx, &cc, budget, false)
}

// chairSeatActivities is the fleets a human chair's bot heads deal seats for, nil for any other
// corporation.
func (m *Mod) chairSeatActivities(ctx context.Context, corpID int64) []string {
	if _, ok := humanChaired(corpID); !ok {
		return nil
	}
	heads := m.corpHeads(ctx, corpID)
	var out []string
	if h, ok := heads[armMining]; ok && m.botFlown(h.CharID) {
		out = append(out, ActMining)
	}
	if h, ok := heads[armSecurity]; ok && m.botFlown(h.CharID) {
		out = append(out, ActCombat, ActSecurity)
	}
	return out
}
