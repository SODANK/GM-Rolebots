package rolebots

// Assignments: orders a human member can act on, sent by mail and paid on delivery. One
// concrete job (deliver, build or patrol) is offered with signed accept and decline links, and
// the token in the link is the authority (corpdesk.go). A reward is a commitment against the
// corporation's wallet (corpcommit.go) from the moment it is offered, and payout takes
// corpOpLock like every other spend. See docs/comment-notes-b03.md#assignment-kinds.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"gurimoon/corp"
	evedb "gurimoon/db"
)

const (
	assignDeliver = "deliver"
	assignBuild   = "build"
	assignPatrol  = "patrol"

	asOffered  = "offered"
	asPosted   = "posted" // on the board, nobody's yet
	asAccepted = "accepted"
	asDone     = "done"
	asExpired  = "expired"
	asDeclined = "declined"

	// assignEverySec: how often the desk restocks the board per corporation.
	assignEverySec = 2 * 3600
	// assignOfferSec: an offer to one member nobody accepts lapses quietly after this.
	assignOfferSec = 24 * 3600
	// assignBoardSec: a job nobody takes comes off the board after this.
	assignBoardSec = 48 * 3600
	// assignNoticeIgnoredMax: "new work" letters in a row with nothing taken, after which the
	// board stops writing to that member until they take one.
	assignNoticeIgnoredMax = 3

	assignDeliverMinISK  = 1e6
	assignDeliverMaxISK  = 25e6
	assignDeliverPremium = 1.10
	assignBuildBonusFrac = 0.25 // of the board wage for the reserved runs
	assignPatrolKills    = 10
	assignPatrolPerKill  = 150000.0
	assignMeritPerMISK   = 15.0 // merit per million ISK of reward, on top of the ledger's
	assignMeritMin       = 10.0

	// The office's division 1: where the buy orders deliver and a deliver assignment counts.
	assignDeliverFlag = corpDivMaterials

	journalBountyRef = 17
)

// assignment is one row of rolebot_corp_assignments.
type assignment struct {
	ID         int64
	CorpID     int64
	CharID     int64
	Kind       string
	TypeID     int64
	Qty        int64
	Progress   int64
	StationID  int64
	Division   int
	SystemID   int64
	WorkID     int64
	Reward     float64
	PaidISK    float64
	MeritPts   float64
	Status     string
	OfferedBy  int64
	OfferedAt  int64
	AcceptedAt int64
	ExpiresAt  int64
	ClosedAt   int64
}

const assignCols = `assignmentID, corporationID, characterID, kind, typeID, qty, progress, stationID,
	division, systemID, workID, rewardISK, paidISK, meritPts, status, offeredBy, offeredAt,
	acceptedAt, expiresAt, closedAt`

func (m *Mod) queryAssignments(ctx context.Context, where string, args ...any) ([]assignment, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+assignCols+` FROM rolebot_corp_assignments `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("queryAssignments: %w", err)
	}
	defer rows.Close()
	var out []assignment
	for rows.Next() {
		var a assignment
		if err := rows.Scan(&a.ID, &a.CorpID, &a.CharID, &a.Kind, &a.TypeID, &a.Qty, &a.Progress,
			&a.StationID, &a.Division, &a.SystemID, &a.WorkID, &a.Reward, &a.PaidISK, &a.MeritPts,
			&a.Status, &a.OfferedBy, &a.OfferedAt, &a.AcceptedAt, &a.ExpiresAt, &a.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (m *Mod) getAssignment(ctx context.Context, id int64) (*assignment, error) {
	as, err := m.queryAssignments(ctx, `WHERE assignmentID = ?`, id)
	if err != nil || len(as) == 0 {
		return nil, err
	}
	return &as[0], nil
}

// ── words ───────────────────────────────────────────────────────────────────

// assignmentText says what an assignment asks, in one line.
func (m *Mod) assignmentText(ctx context.Context, a assignment) string {
	switch a.Kind {
	case assignDeliver:
		return fmt.Sprintf("Deliver %d %s to division 1 of our office at %s",
			a.Qty, m.typeNameOr(ctx, a.TypeID), m.stationLabel(ctx, a.StationID))
	case assignBuild:
		return fmt.Sprintf("Build %d run(s) of %s at %s, as a corporation job from our library",
			a.Qty, m.typeNameOr(ctx, a.TypeID), m.stationLabel(ctx, a.StationID))
	case assignPatrol:
		where := "wherever you hunt"
		if a.SystemID != 0 {
			where = "— Security wants " + m.nameOfSystem(ctx, a.SystemID) + " swept"
		}
		return fmt.Sprintf("Kill %d pirates and collect their bounties %s", a.Qty, where)
	case assignFuel:
		return m.fuelAssignmentText(ctx, a)
	}
	return a.Kind
}

// ── offering ────────────────────────────────────────────────────────────────

// assignDesk is the assignments' turn on the staff desk sweep: patrol progress, expiries, and
// every assignEverySec new offers.
func (m *Mod) assignDesk(ctx context.Context, c *corpCEO, ch Charter) {
	if !assignmentsOn() || !m.corpHasHumans(ctx, c.CorpID) {
		return
	}
	// The staff desk sweep already holds this corporation's corpOpLock.
	ctx = withOpLockHeld(ctx)
	if err := m.assignProgressPatrols(ctx, c); err != nil {
		m.logBot(c.CharID, logError, "ceo", "assignments (patrol): %v", err)
	}
	if err := m.assignExpire(ctx, c); err != nil {
		m.logBot(c.CharID, logError, "ceo", "assignments (expiry): %v", err)
	}
	if !deskDue(c.CorpID, "assign", assignEverySec) {
		return
	}
	if err := m.assignPost(ctx, c, ch); err != nil {
		m.logBot(c.CharID, logError, "ceo", "assignments (board): %v", err)
	}
}

// assignPost keeps the corporation's Assignments board stocked: up to assignBoardMax open jobs
// drawn from what a department needs now. Nothing is offered by name; a member takes a job off
// the board (corpassign_igb.go) and the members it suits get one short letter (assignNotice).
func (m *Mod) assignPost(ctx context.Context, c *corpCEO, ch Charter) error {
	open, err := m.queryAssignments(ctx,
		`WHERE corporationID = ? AND status IN ('posted', 'offered', 'accepted')`, c.CorpID)
	if err != nil {
		return err
	}
	busyWork := map[int64]bool{}
	busyType := map[int64]bool{}
	busySys := map[int64]bool{}
	posted := 0
	for _, a := range open {
		if a.Status == asPosted {
			posted++
		}
		if a.WorkID != 0 && a.Kind != assignFuel {
			busyWork[a.WorkID] = true
		}
		switch a.Kind {
		case assignDeliver:
			busyType[a.TypeID] = true
		case assignPatrol:
			busySys[a.SystemID] = true
		}
	}
	if posted >= assignBoardMax() {
		return nil
	}
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return err
	}
	// The board may promise at most a quarter of free capital per pass.
	budget := free / 4
	var fresh []assignment
	for _, kind := range []string{assignPatrol, assignDeliver, assignBuild} {
		if posted >= assignBoardMax() || ctx.Err() != nil {
			break
		}
		a, ok := m.assignCandidate(ctx, c, corpMember{}, kind, busyWork, busyType)
		if !ok || a.Reward > budget || (kind == assignPatrol && busySys[a.SystemID]) {
			continue
		}
		a.Status = asPosted
		a.ExpiresAt = a.OfferedAt + assignBoardSec
		a.OfferedBy, _ = m.letterFrom(ctx, c, assignTopic(kind))
		id, err := m.insertAssignment(ctx, a)
		if err != nil {
			return err
		}
		a.ID = id
		budget -= a.Reward
		posted++
		if a.WorkID != 0 {
			busyWork[a.WorkID] = true
		}
		if kind == assignDeliver {
			busyType[a.TypeID] = true
		}
		fresh = append(fresh, a)
		m.ceoLog(ctx, c, powerProduction, "assign", 0, a.Reward,
			"posted an assignment: %s, for %s", m.assignmentText(ctx, a), iskString(a.Reward))
	}
	if len(fresh) > 0 {
		m.assignNotice(ctx, c, fresh)
	}
	return nil
}

// assignTopic is which officer writes about a kind of job (corpmailfrom.go).
func assignTopic(kind string) string {
	switch kind {
	case assignPatrol:
		return mailFromSecurity
	case assignDeliver:
		return mailFromCommerce
	case assignBuild:
		return mailFromIndustry
	case assignFuel:
		return mailFromInfrastructure
	}
	return mailFromChair
}

// assignFrom is the writer of an assignment's letters and the hat they sign with: the officer
// who posted it.
func (m *Mod) assignFrom(ctx context.Context, a assignment) (int64, string) {
	if c, _ := getCEO(ctx, m.db, a.CorpID); c != nil {
		if a.OfferedBy == 0 || a.OfferedBy == c.CharID {
			return c.CharID, "Chief Executive Officer"
		}
	}
	return a.OfferedBy, m.writerHat(ctx, a.OfferedBy, mailFromDefaultHat[assignTopic(a.Kind)])
}

// assignKindOrder is which kinds suit a member, best suited first. The member's revealed
// record (memberprofile.go) leads; building is for a Factory Manager only.
func (m *Mod) assignKindOrder(ctx context.Context, cm corpMember) []string {
	tn := m.revealedTuning(ctx, cm.CharID)
	type kw struct {
		k string
		w float64
	}
	cands := []kw{
		{assignDeliver, math.Max(tn.MiningAppetite, tn.HaulAppetite)},
		{assignPatrol, tn.ViolenceAppetite},
	}
	if cm.Roles&corp.RoleFactoryManager != 0 {
		cands = append(cands, kw{assignBuild, tn.IndustryAppetite + 0.5})
	}
	// Stable, best first.
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && cands[j].w > cands[j-1].w; j-- {
			cands[j], cands[j-1] = cands[j-1], cands[j]
		}
	}
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.k
	}
	return out
}

// assignSuits is the jobs out of these a member may take, best suited first.
func (m *Mod) assignSuits(ctx context.Context, cm corpMember, jobs []assignment) []assignment {
	var out []assignment
	for _, k := range m.assignKindOrder(ctx, cm) {
		for _, a := range jobs {
			if a.Kind == k {
				out = append(out, a)
			}
		}
	}
	return out
}

// assignCandidate builds one job of a kind, if the corporation has anything
// of that kind to give. cm is the member it is for; zero for the board.
func (m *Mod) assignCandidate(ctx context.Context, c *corpCEO, cm corpMember, kind string,
	busyWork, busyType map[int64]bool) (assignment, bool) {
	now := clock.Unix()
	a := assignment{CorpID: c.CorpID, CharID: cm.CharID, Kind: kind, Status: asOffered,
		OfferedBy: c.CharID, OfferedAt: now}
	switch kind {
	case assignDeliver:
		rows, err := m.db.QueryContext(ctx, `
			SELECT r.typeID, o.price, o.volRemaining, r.stationID
			  FROM rolebot_corp_orders r
			  JOIN market_orders o ON o.orderID = r.orderID
			  JOIN crpoffices f ON f.corporationID = r.corporationID AND f.stationID = r.stationID
			 WHERE r.corporationID = ? AND r.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
			 ORDER BY o.price * o.volRemaining DESC
			 LIMIT 8`, c.CorpID)
		if err != nil {
			return a, false
		}
		defer rows.Close()
		for rows.Next() {
			var typeID, rem, station int64
			var price float64
			if rows.Scan(&typeID, &price, &rem, &station) != nil || busyType[typeID] || price <= 0 {
				continue
			}
			qty := deliverQty(price, rem)
			if qty <= 0 {
				continue
			}
			a.TypeID, a.Qty, a.StationID, a.Division = typeID, qty, station, 1
			a.Reward = math.Floor(float64(qty) * price * assignDeliverPremium)
			a.MeritPts = assignMerit(a.Reward)
			return a, true
		}
		return a, false
	case assignBuild:
		if cm.CharID != 0 && m.meritBenched(ctx, c.CorpID, cm.CharID) {
			return a, false
		}
		work, err := m.openWorkFor(ctx, c.CorpID)
		if err != nil {
			return a, false
		}
		for _, w := range work {
			if w.Kind != workKindBuild || busyWork[w.WorkID] || w.RunsLeft() <= 0 {
				continue
			}
			// No bot-only cap on the runs reserved: the order's runs left, then what is free of other
			// claims. The install holds it to the print's own run limit, as for a player.
			runs := w.RunsLeft()
			if free := m.workRunsFree(ctx, w, cm.CharID); free < runs {
				runs = free
			}
			if runs <= 0 {
				continue
			}
			a.TypeID, a.Qty, a.StationID, a.WorkID = w.ProductTypeID, int64(runs), w.StationID, w.WorkID
			a.Reward = math.Floor(float64(runs) * w.WagePerRun * assignBuildBonusFrac)
			a.MeritPts = assignMerit(float64(runs) * w.WagePerRun)
			return a, a.Reward >= 1
		}
		return a, false
	case assignPatrol:
		// Only where Security has sent the combat wing for the belts, so the letter varies.
		o := m.corpFleetOrderFor(ctx, c.CorpID, portSecurity)
		if o == nil || o.SystemID == 0 || !m.distressedBeltSystems(ctx, c.CorpID)[o.SystemID] {
			return a, false
		}
		a.SystemID, a.Qty = o.SystemID, patrolKills(o.SystemID)
		a.Reward = float64(a.Qty) * assignPatrolPerKill
		a.MeritPts = assignMerit(a.Reward)
		return a, true
	}
	return a, false
}

// patrolKills is how many bounties a patrol asks: 6 to 14, steady for one system so a job
// reads the same while it stands.
func patrolKills(systemID int64) int64 {
	if systemID < 0 {
		systemID = -systemID
	}
	return assignPatrolKills - 4 + 2*(systemID%5)
}

// deliverQty sizes a delivery so its value lands between the min and max reward, and never
// more than the corporation's bid still wants.
func deliverQty(price float64, remaining int64) int64 {
	if price <= 0 || remaining <= 0 {
		return 0
	}
	want := int64(math.Ceil(assignDeliverMinISK * 5 / price))
	most := int64(math.Floor(assignDeliverMaxISK / (price * assignDeliverPremium)))
	q := min(max(want, 1), most, remaining)
	if float64(q)*price*assignDeliverPremium < assignDeliverMinISK {
		return 0
	}
	return q
}

// assignMerit is the merit an assignment of this value is worth.
func assignMerit(reward float64) float64 {
	return math.Max(assignMeritMin, math.Round(reward/1e6*assignMeritPerMISK))
}

// workRunsFree is how many runs of an order nobody else has claimed.
func (m *Mod) workRunsFree(ctx context.Context, w corpWork, charID int64) int {
	return w.RunsLeft() - m.workClaimedExceptOpen(ctx, w.WorkID, charID)
}

func (m *Mod) insertAssignment(ctx context.Context, a assignment) (int64, error) {
	if a.Status == "" {
		a.Status = asOffered
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_assignments
			(corporationID, characterID, kind, typeID, qty, stationID, division, systemID, workID,
			 rewardISK, meritPts, status, offeredBy, offeredAt, expiresAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.CorpID, a.CharID, a.Kind, a.TypeID, a.Qty, a.StationID, a.Division, a.SystemID, a.WorkID,
		a.Reward, a.MeritPts, a.Status, a.OfferedBy, a.OfferedAt, a.ExpiresAt)
	if err != nil {
		return 0, fmt.Errorf("insertAssignment: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := m.openCommit(ctx, a.CorpID, "assign", id, a.Reward); err != nil {
		return id, err
	}
	return id, nil
}

// assignNoticeDue is the pure half of the notice rule: at most one letter per assignNoticeSec,
// and none once assignNoticeIgnoredMax have gone unanswered in a row (a take resets it).
func assignNoticeDue(lastAt int64, unanswered int, now int64) bool {
	return unanswered < assignNoticeIgnoredMax && now-lastAt >= assignNoticeSec()
}

// assignNotice tells each human member the new jobs that suit them: one short letter, rarely,
// with the board and a take link for each. A member holding as much work as they may take
// hears nothing.
func (m *Mod) assignNotice(ctx context.Context, c *corpCEO, fresh []assignment) {
	members, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return
	}
	now := clock.Unix()
	corpName := ""
	for _, cm := range members {
		if !cm.Human() || ctx.Err() != nil {
			continue
		}
		suits := m.assignSuits(ctx, cm, fresh)
		if len(suits) == 0 || m.assignHeldBy(ctx, c.CorpID, cm.CharID) >= assignOpenMax() {
			continue
		}
		var lastAt int64
		var unanswered int
		_ = m.db.QueryRowContext(ctx, `
			SELECT lastNoticeAt, unanswered FROM rolebot_corp_assign_notice
			 WHERE corporationID = ? AND characterID = ?`, c.CorpID, cm.CharID).Scan(&lastAt, &unanswered)
		if !assignNoticeDue(lastAt, unanswered, now) {
			continue
		}
		if corpName == "" {
			corpName = m.corpNameOf(ctx, c.CorpID)
		}
		if m.mailAssignmentNotice(ctx, c, cm, suits, corpName) == 0 {
			continue
		}
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_assign_notice (corporationID, characterID, lastNoticeAt, unanswered)
			VALUES (?, ?, ?, 1)
			ON DUPLICATE KEY UPDATE lastNoticeAt = VALUES(lastNoticeAt), unanswered = unanswered + 1`,
			c.CorpID, cm.CharID, now)
	}
}

// assignHeldBy is how many jobs a member holds now.
func (m *Mod) assignHeldBy(ctx context.Context, corpID, charID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_assignments
		 WHERE corporationID = ? AND characterID = ? AND status = ?`, corpID, charID, asAccepted).Scan(&n)
	return n
}

// mailAssignmentNotice is the "new work" letter: the jobs, a take link for each, and the
// board. Signed by the officer who posted the first.
func (m *Mod) mailAssignmentNotice(ctx context.Context, c *corpCEO, cm corpMember, jobs []assignment, corpName string) int64 {
	if len(jobs) > 3 {
		jobs = jobs[:3]
	}
	pay := m.meritPayFor(ctx, c.CorpID, cm.CharID)
	from, hat := m.assignFrom(ctx, jobs[0])
	var b strings.Builder
	line(&b, "%s,", cm.Name)
	b.WriteString("<br>")
	line(&b, "There is work on the board that suits you:")
	for _, a := range jobs {
		line(&b, "<b>%s</b> &mdash; %s, %.0f merit. %s",
			m.assignmentText(ctx, a), iskString(math.Floor(a.Reward*pay)), a.MeritPts,
			igbLink(assignTakePath(a.CorpID, cm.CharID, a.ID), "Take it"))
	}
	b.WriteString("<br>")
	line(&b, "First to take a job has it. Everything open is on %s.",
		igbLink(assignBoardPath(c.CorpID, cm.CharID), "the Assignments board"))
	b.WriteString(m.signature(ctx, from, hat, corpName))
	subject := "New work: " + m.assignmentText(ctx, jobs[0])
	if len(jobs) > 1 {
		subject = fmt.Sprintf("New work on the board (%d jobs)", len(jobs))
	}
	return m.mailPilot(ctx, cm.CharID, from, subject, b.String())
}

// mailAssignmentOffer is a job offered to one member by name (a human Fuel Technician's tower
// run, posassign.go) with the two links in its text. Everything else goes on the board.
func (m *Mod) mailAssignmentOffer(ctx context.Context, c *corpCEO, cm corpMember, a assignment, corpName string) {
	pay := m.meritPayFor(ctx, c.CorpID, cm.CharID)
	from, hat := m.assignFrom(ctx, a)
	var b strings.Builder
	line(&b, "%s,", cm.Name)
	b.WriteString("<br>")
	line(&b, "I have work for you.")
	line(&b, "<b>%s.</b>", m.assignmentText(ctx, a))
	switch a.Kind {
	case assignBuild:
		line(&b, "The work board pays its wage per run as always; finishing all %d adds %s.",
			a.Qty, iskString(math.Floor(a.Reward*pay)))
		line(&b, "Accept and those runs are held for you — nobody else may build them.")
	case assignDeliver:
		line(&b, "Pays %s on delivery — more than our own buy order.", iskString(math.Floor(a.Reward*pay)))
		line(&b, "Put the goods into the corporation hangar there, division 1. Every unit you deposit counts.")
	case assignPatrol:
		line(&b, "Pays %s when the last bounty lands; every bounty counts toward it.",
			iskString(math.Floor(a.Reward*pay)))
	case assignFuel:
		line(&b, "Pays %s for the %d day(s), and its share for every day you bring.", iskString(math.Floor(a.Reward*pay)), a.Qty)
		line(&b, "The fuel is waiting in the Fuel division; your seat's roles let you take it. Put it into the tower's own bay.")
	}
	line(&b, "Worth %.0f merit toward your next rank. You have %s once you accept.",
		a.MeritPts, fmtDuration(assignExpireSec()))
	b.WriteString("<br>")
	line(&b, "%s &nbsp; or &nbsp; %s",
		igbLink(deskActionPath(a, "accept"), "Accept this assignment"),
		igbLink(deskActionPath(a, "decline"), "Decline it"))
	line(&b, "This offer stands for %s.", fmtDuration(assignOfferSec))
	b.WriteString(m.signature(ctx, from, hat, corpName))
	m.mailPilot(ctx, cm.CharID, from, "Assignment: "+m.assignmentText(ctx, a), b.String())
}

// ── the board: take and drop ────────────────────────────────────────────────

var (
	errAssignTaken   = errors.New("somebody took that job first")
	errAssignFull    = errors.New("you already hold as much work as you may")
	errAssignRole    = errors.New("building needs the Factory Manager role")
	errAssignBenched = errors.New("you are benched from building for now")
	errAssignNotMine = errors.New("that job is not yours")
)

// takeAssignment gives a posted job to a member. First to take it has it: the conditional
// UPDATE is the referee.
func (m *Mod) takeAssignment(ctx context.Context, id, charID int64) (*assignment, error) {
	a, err := m.getAssignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil || a.Status != asPosted || clock.Unix() >= a.ExpiresAt {
		return a, errAssignGone
	}
	var cur int64
	var roles uint64
	_ = m.db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&cur)
	if cur != a.CorpID || !m.isCapsuleer(ctx, charID) {
		return a, errAssignGone
	}
	if m.assignHeldBy(ctx, a.CorpID, charID) >= assignOpenMax() {
		return a, errAssignFull
	}
	if a.Kind == assignBuild {
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(corprole, 0) FROM chrCorporationRoles WHERE characterID = ?`, charID).Scan(&roles)
		if roles&corp.RoleFactoryManager == 0 {
			return a, errAssignRole
		}
		if m.meritBenched(ctx, a.CorpID, charID) {
			return a, errAssignBenched
		}
	}
	now := clock.Unix()
	expires := now + assignExpireSec()
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_assignments SET characterID = ?, status = ?, acceptedAt = ?, expiresAt = ?
		 WHERE assignmentID = ? AND status = ?`, charID, asAccepted, now, expires, id, asPosted)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return a, errAssignTaken
	}
	a.CharID, a.Status, a.AcceptedAt, a.ExpiresAt = charID, asAccepted, now, expires
	if err := m.reserveBuildRuns(ctx, *a); err != nil {
		return a, err
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_assign_notice (corporationID, characterID, lastNoticeAt, unanswered)
		VALUES (?, ?, 0, 0) ON DUPLICATE KEY UPDATE unanswered = 0`, a.CorpID, charID)
	return a, nil
}

// dropAssignment hands a job back. With nothing done it returns to the board for the rest of
// its posting; with some done it closes, paying that share, as an expiry would.
func (m *Mod) dropAssignment(ctx context.Context, id, charID int64) (*assignment, error) {
	a, err := m.getAssignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil || a.Status != asAccepted {
		return a, errAssignGone
	}
	if a.CharID != charID {
		return a, errAssignNotMine
	}
	if a.Progress > 0 || a.Kind == assignFuel {
		m.settleAssignment(ctx, *a, asExpired)
		return a, nil
	}
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_assignments SET characterID = 0, status = ?, acceptedAt = 0, expiresAt = ?
		 WHERE assignmentID = ? AND status = ? AND characterID = ? AND progress = 0`,
		asPosted, a.OfferedAt+assignBoardSec, id, asAccepted, charID)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return a, errAssignGone
	}
	if a.Kind == assignBuild && a.WorkID != 0 {
		_, _ = m.db.ExecContext(ctx, `
			DELETE FROM rolebot_corp_work_claims WHERE workID = ? AND characterID = ? AND jobID = 0`,
			a.WorkID, charID)
	}
	a.Status, a.CharID = asPosted, 0
	return a, nil
}

// reserveBuildRuns holds an accepted build job's runs in the work board's claim table until it
// expires. A claim counts while claimedAt is within corpWorkClaimSec, so it is stamped that
// far before the expiry.
func (m *Mod) reserveBuildRuns(ctx context.Context, a assignment) error {
	if a.Kind != assignBuild || a.WorkID == 0 {
		return nil
	}
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_work_claims (workID, characterID, runs, claimedAt)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE runs = VALUES(runs), jobID = 0, claimedAt = VALUES(claimedAt)`,
		a.WorkID, a.CharID, a.Qty, a.ExpiresAt-corpWorkClaimSec)
	return err
}

// ── the member's answer ─────────────────────────────────────────────────────

var errAssignGone = errors.New("that assignment is no longer open")

// opLockHeldKey marks a context whose caller already holds the corporation's corpOpLock (a
// sync.Mutex, not reentrant).
type opLockHeldKey struct{}

// answerAssignment applies an accept or decline from a signed link. The caller checked the
// token; this checks the state.
func (m *Mod) answerAssignment(ctx context.Context, id int64, accept bool) (*assignment, error) {
	a, err := m.getAssignment(ctx, id)
	if err != nil {
		return nil, err
	}
	if a == nil || a.Status != asOffered {
		return a, errAssignGone
	}
	now := clock.Unix()
	if now-a.OfferedAt > assignOfferSec {
		return a, errAssignGone
	}
	var cur int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, a.CharID).Scan(&cur)
	if cur != a.CorpID {
		return a, errAssignGone
	}
	if !accept {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_assignments SET status = ?, closedAt = ?
			 WHERE assignmentID = ? AND status = ?`, asDeclined, now, id, asOffered); err != nil {
			return a, err
		}
		m.closeCommit(ctx, "assign", id)
		a.Status = asDeclined
		return a, nil
	}
	expires := now + assignExpireSec()
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_assignments SET status = ?, acceptedAt = ?, expiresAt = ?
		 WHERE assignmentID = ? AND status = ?`, asAccepted, now, expires, id, asOffered)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return a, errAssignGone
	}
	a.Status, a.AcceptedAt, a.ExpiresAt = asAccepted, now, expires
	if err := m.reserveBuildRuns(ctx, *a); err != nil {
		return a, err
	}
	return a, nil
}

// ── progress ────────────────────────────────────────────────────────────────

// noteAssignmentDeposit is the office-deposit hook's half: a member put an item into a
// corporation office. It counts toward an accepted deliver assignment of that type, station
// and division.
func (m *Mod) noteAssignmentDeposit(ctx context.Context, corpID, charID, itemID, typeID, flag int64) {
	if !assignmentsOn() || flag != assignDeliverFlag {
		return
	}
	var qty, officeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT quantity, locationID FROM entity WHERE itemID = ?`, itemID).Scan(&qty, &officeID); err != nil || qty <= 0 {
		return
	}
	var stationID int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT stationID FROM crpoffices WHERE itemID = ? AND corporationID = ?`, officeID, corpID).Scan(&stationID)
	if stationID == 0 {
		return
	}
	as, err := m.queryAssignments(ctx, `
		WHERE corporationID = ? AND characterID = ? AND kind = ? AND status = ?
		  AND typeID = ? AND stationID = ? AND expiresAt > ?
		ORDER BY acceptedAt LIMIT 1`,
		corpID, charID, assignDeliver, asAccepted, typeID, stationID, clock.Unix())
	if err != nil || len(as) == 0 {
		return
	}
	m.advanceAssignment(ctx, as[0], qty)
}

// noteAssignmentBuild is the wage sweep's half: runs of a work order the member delivered.
func (m *Mod) noteAssignmentBuild(ctx context.Context, corpID, charID, workID int64, runs int) {
	if runs <= 0 {
		return
	}
	as, err := m.queryAssignments(ctx, `
		WHERE corporationID = ? AND characterID = ? AND kind = ? AND status = ? AND workID = ?
		ORDER BY acceptedAt LIMIT 1`, corpID, charID, assignBuild, asAccepted, workID)
	if err != nil || len(as) == 0 {
		return
	}
	// The delivered runs leave the reservation, or they would count twice against the bots.
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_work_claims SET runs = GREATEST(runs - ?, 0)
		 WHERE workID = ? AND characterID = ? AND jobID = 0`, runs, workID, charID)
	_, _ = m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_work_claims WHERE workID = ? AND characterID = ? AND runs = 0`, workID, charID)
	m.advanceAssignment(ctx, as[0], int64(runs))
}

// assignmentCovers reports whether a delivered corporation job that matched no open order was
// the member's own accepted build assignment (the order closed under them), which must not
// count as an off-plan strike.
func (m *Mod) assignmentCovers(ctx context.Context, corpID, charID, productTypeID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_assignments
		 WHERE corporationID = ? AND characterID = ? AND kind = ? AND typeID = ?
		   AND status IN ('accepted', 'done', 'expired') AND acceptedAt > ?`,
		corpID, charID, assignBuild, productTypeID, clock.Unix()-corpOffPlanWindow).Scan(&n)
	return n > 0
}

// assignProgressPatrols counts the bounties each accepted patrol has collected since it was
// accepted.
func (m *Mod) assignProgressPatrols(ctx context.Context, c *corpCEO) error {
	as, err := m.queryAssignments(ctx, `
		WHERE corporationID = ? AND kind = ? AND status = ?`, c.CorpID, assignPatrol, asAccepted)
	if err != nil {
		return err
	}
	for _, a := range as {
		var n int64
		if err := m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM market_journal
			 WHERE characterID = ? AND refTypeID = ? AND ownerID2 = ? AND transDate >= ?`,
			a.CharID, journalBountyRef, a.CharID, evedb.UnixToWin32(a.AcceptedAt)).Scan(&n); err != nil {
			continue
		}
		if gain := min(n, a.Qty) - a.Progress; gain > 0 {
			m.advanceAssignment(ctx, a, gain)
		}
	}
	return nil
}

// advanceAssignment adds progress and completes the assignment when it is full. The
// conditional UPDATEs make two hooks racing on one assignment pay it once.
func (m *Mod) advanceAssignment(ctx context.Context, a assignment, by int64) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_assignments SET progress = LEAST(qty, progress + ?)
		 WHERE assignmentID = ? AND status = ?`, by, a.ID, asAccepted); err != nil {
		return
	}
	cur, err := m.getAssignment(ctx, a.ID)
	if err != nil || cur == nil || cur.Status != asAccepted || cur.Progress < cur.Qty {
		return
	}
	m.settleAssignment(ctx, *cur, asDone)
}

// assignExpire lapses unanswered offers and settles expired work.
func (m *Mod) assignExpire(ctx context.Context, c *corpCEO) error {
	now := clock.Unix()
	stale, err := m.queryAssignments(ctx, `
		WHERE corporationID = ? AND status = ? AND offeredAt < ?`, c.CorpID, asOffered, now-assignOfferSec)
	if err != nil {
		return err
	}
	for _, a := range stale {
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_assignments SET status = ?, closedAt = ?
			 WHERE assignmentID = ? AND status = ?`, asExpired, now, a.ID, asOffered); err == nil {
			m.closeCommit(ctx, "assign", a.ID)
		}
	}
	posted, err := m.queryAssignments(ctx, `
		WHERE corporationID = ? AND status = ?`, c.CorpID, asPosted)
	if err != nil {
		return err
	}
	var patrolSys int64
	if len(posted) > 0 {
		if o := m.corpFleetOrderFor(ctx, c.CorpID, portSecurity); o != nil {
			patrolSys = o.SystemID
		}
	}
	for _, a := range posted {
		if now < a.ExpiresAt && (a.Kind != assignPatrol || a.SystemID == patrolSys) {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_assignments SET status = ?, closedAt = ?
			 WHERE assignmentID = ? AND status = ?`, asExpired, now, a.ID, asPosted); err == nil {
			m.closeCommit(ctx, "assign", a.ID)
		}
	}
	due, err := m.queryAssignments(ctx, `
		WHERE corporationID = ? AND status = ? AND expiresAt <= ?`, c.CorpID, asAccepted, now)
	if err != nil {
		return err
	}
	for _, a := range due {
		m.settleAssignment(ctx, a, asExpired)
	}
	// A member who left the corporation takes nothing with them.
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_assignments a
		  JOIN character_ ch ON ch.characterID = a.characterID
		   SET a.status = ?, a.closedAt = ?
		 WHERE a.corporationID = ? AND a.status IN ('offered', 'accepted') AND ch.corporationID <> a.corporationID`,
		asExpired, now, c.CorpID)
	return nil
}

// settlePay is the pure half: what an assignment pays when it closes. Done pays in full; an
// expiry pays the share delivered.
func settlePay(a assignment, final string, payMult float64) (isk, merit float64) {
	frac := 1.0
	if final != asDone {
		if a.Qty <= 0 {
			return 0, 0
		}
		frac = float64(a.Progress) / float64(a.Qty)
	}
	return math.Floor(a.Reward * frac * payMult), math.Round(a.MeritPts * frac)
}

// settleAssignment closes an accepted assignment and pays what it earned. It takes
// corpOpLock, since it spends corporation ISK.
func (m *Mod) settleAssignment(ctx context.Context, a assignment, final string) {
	if ctx.Value(opLockHeldKey{}) == nil {
		lk := corpOpLock(a.CorpID)
		lk.Lock()
		defer lk.Unlock()
	}
	now := clock.Unix()
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_assignments SET status = ?, closedAt = ?
		 WHERE assignmentID = ? AND status = ?`, final, now, a.ID, asAccepted)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // somebody else settled it
	}
	if a.Kind == assignBuild && a.WorkID != 0 {
		_, _ = m.db.ExecContext(ctx, `
			DELETE FROM rolebot_corp_work_claims WHERE workID = ? AND characterID = ? AND jobID = 0`,
			a.WorkID, a.CharID)
	}
	managed := payrollManaged(a.CorpID)
	payMult := 1.0
	if !managed {
		payMult = m.meritPayFor(ctx, a.CorpID, a.CharID)
	}
	isk, merit := settlePay(a, final, payMult)
	paid := 0.0
	if managed {
		// A bot-run corporation pays by the weekly payroll (corppayroll.go): the reward is credited to
		// the member's labour record and the rank multiplier applies to the cheque.
		if isk >= 1 {
			m.creditLabour(ctx, a.CorpID, a.CharID, "assignment", isk, "assignment: "+m.assignmentText(ctx, a))
			paid = isk
		}
	} else if isk >= 1 {
		_, bal, err := m.corpWithdraw(ctx, a.CorpID, a.CharID, isk,
			evedb.JournalReason("Corporation assignment"))
		if err == nil {
			paid = isk
			m.pushBalance(a.CharID, bal)
			m.corpBook(ctx, a.CorpID, lineWages, -isk, a.CharID, "assignment: "+m.assignmentText(ctx, a))
		} else {
			rlogf("rolebots: pay assignment %d: %v", a.ID, err)
		}
	}
	m.spendCommit(ctx, "assign", a.ID, paid)
	m.closeCommit(ctx, "assign", a.ID)
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_assignments SET paidISK = ? WHERE assignmentID = ?`, paid, a.ID)
	if merit > 0 {
		m.addMerit(ctx, a.CorpID, a.CharID, merit, "assignment: "+m.assignmentText(ctx, a))
	}

	c, _ := getCEO(ctx, m.db, a.CorpID)
	name := m.charNameOr(ctx, a.CharID)
	if c != nil {
		verb := "completed"
		if final != asDone {
			verb = fmt.Sprintf("ran out of time on (%d/%d)", a.Progress, a.Qty)
		}
		paidVerb := "paid"
		if managed {
			paidVerb = "credited"
		}
		m.ceoLog(ctx, c, powerProduction, "assigned", a.CharID, paid,
			"%s %s an assignment — %s %s", name, verb, paidVerb, iskString(paid))
		if final == asDone {
			m.queueCorpNews(ctx, a.CorpID, newsOps,
				fmt.Sprintf("%s: %s — done.", name, m.assignmentText(ctx, a)), a.CharID)
		}
	}
	m.mailAssignmentReceipt(ctx, a, final, paid, merit)
}

func (m *Mod) mailAssignmentReceipt(ctx context.Context, a assignment, final string, paid, merit float64) {
	corpName := m.corpNameOf(ctx, a.CorpID)
	var b strings.Builder
	line(&b, "%s,", m.charNameOr(ctx, a.CharID))
	b.WriteString("<br>")
	subject := "Assignment complete"
	if final == asDone {
		line(&b, "Done: %s.", m.assignmentText(ctx, a))
	} else {
		subject = "Assignment closed"
		line(&b, "Time is up on: %s.", m.assignmentText(ctx, a))
		line(&b, "You got %d of %d done, and are credited for that share.", a.Progress, a.Qty)
	}
	if payrollManaged(a.CorpID) {
		line(&b, "Credited to your labour record: %s, paid in the weekly payroll. Merit: %.0f.", iskString(paid), merit)
	} else {
		line(&b, "Paid: %s. Merit: %.0f.", iskString(paid), merit)
	}
	line(&b, "Your record: %s.", igbLink(deskViewPath(a.CorpID, a.CharID), "your desk"))
	from, hat := m.assignFrom(ctx, a)
	b.WriteString(m.signature(ctx, from, hat, corpName))
	m.mailPilot(ctx, a.CharID, from, subject, b.String())
}
