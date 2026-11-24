package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// The order desk: what carries an order from the chair (corpcommands.go) out to
// the bots and reports back. A human CEO or division head posts on the Management page,
// which writes only a row. Every minute this lane takes each pending row, finds
// the member that can carry it out (a commander for a fleet order, the board for
// a work order, the directive table for a directive), follows it until done and
// words the outcome on the row the Operations page shows. A refusal is a
// sentence, never silence. A human order outranks the bot CEO: a fleet order and
// a work order carry source human and the department desks leave them alone
// (postFleetOrder / clearFleetOrder); a directive is written with a hold that
// ceoDirective respects.

const (
	settingCorpOrders        = "rolebots.corp.orders"
	settingOrderTTLHours     = "rolebots.corp.orderTTLHours"
	settingOrderRetries      = "rolebots.corp.orderRetries"
	settingHumanFleetMaxOpen = "rolebots.corp.humanFleetMaxOpen"
	settingDirectiveHoldMax  = "rolebots.corp.directiveHoldDaysMax"

	defaultOrderTTLHours     = 24
	defaultOrderRetries      = 6
	defaultHumanFleetMaxOpen = 2
	defaultDirectiveHoldMax  = 30

	commandDeskEveryTicks  = 60
	commandDeskLaneTimeout = 90 * time.Second
	commandOneTimeout      = 60 * time.Second
	// workDeskMemoSec is how long "does this corporation run a work board"
	// is remembered per corporation: the planner asks it per member per pass.
	workDeskMemoSec = 60
)

var (
	corpOrdersOn      atomic.Bool
	orderTTLHours     atomic.Int64
	orderRetries      atomic.Int64
	humanFleetMaxOpen atomic.Int64
	directiveHoldMax  atomic.Int64
	commandDeskBusy   atomic.Bool
	commandDeskOnce   sync.Once
)

func CorpOrders() bool { return corpOrdersOn.Load() }

func (m *Mod) loadCommandSettings(ctx context.Context) {
	corpOrdersOn.Store(true)
	orderTTLHours.Store(defaultOrderTTLHours)
	orderRetries.Store(defaultOrderRetries)
	humanFleetMaxOpen.Store(defaultHumanFleetMaxOpen)
	directiveHoldMax.Store(defaultDirectiveHoldMax)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingCorpOrders); err == nil && ok {
		corpOrdersOn.Store(truthySetting(v))
	}
	for _, d := range []struct {
		key   string
		cache *atomic.Int64
	}{
		{settingOrderTTLHours, &orderTTLHours},
		{settingOrderRetries, &orderRetries},
		{settingHumanFleetMaxOpen, &humanFleetMaxOpen},
		{settingDirectiveHoldMax, &directiveHoldMax},
	} {
		if v, ok, err := getServerSettingInt(ctx, m, d.key); err == nil && ok && v > 0 {
			d.cache.Store(v)
		}
	}
	m.loadPortalKeySettings(ctx)
}

// ── the lane ────────────────────────────────────────────────────────────────

func (m *Mod) startCommandDesk() {
	if m.tick == nil {
		return
	}
	commandDeskOnce.Do(func() {
		m.registerPeriodic("rolebots-order-desk", commandDeskEveryTicks, func(uint64) bool {
			if !Master() || !CorpOrders() {
				return true
			}
			if !commandDeskBusy.CompareAndSwap(false, true) {
				return true
			}
			go func() {
				defer commandDeskBusy.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), commandDeskLaneTimeout)
				defer cancel()
				m.commandDeskPass(ctx)
			}()
			return true
		})
	})
}

// commandDeskPass walks every open order.
func (m *Mod) commandDeskPass(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT `+cmdCols+` FROM rolebot_corp_commands
		 WHERE status IN ('pending','claimed','live')
		 ORDER BY corporationID, postedAt`)
	if err != nil {
		rlogf("rolebots: order desk: %v", err)
		return
	}
	var open []corpCommand
	for rows.Next() {
		if c, err := scanCommand(rows); err == nil {
			open = append(open, c)
		}
	}
	rows.Close()
	for _, c := range open {
		if ctx.Err() != nil {
			return
		}
		lk := corpOpLock(c.CorpID)
		lk.Lock()
		func() {
			defer lk.Unlock()
			// An order may have been withdrawn while the lock was waited for.
			cur, err := m.getCorpCommand(ctx, c.CommandID)
			if err != nil || !cur.Open() {
				return
			}
			// An order once begun is finished: the lane running out of time must not leave a row on
			// a board with no order following it.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(withOpLockHeld(ctx)), commandOneTimeout)
			defer cancel()
			switch cur.Status {
			case cmdPending:
				m.commandTake(cctx, *cur)
			default:
				m.commandFollow(cctx, *cur)
			}
		}()
	}
}

// commandTake carries a pending order out, or says why not yet.
func (m *Mod) commandTake(ctx context.Context, c corpCommand) {
	now := clock.Unix()
	if c.ExpiresAt > 0 && now > c.ExpiresAt {
		why := "nobody could take it before it lapsed"
		if c.Result != "" {
			why = "lapsed — last tried: " + c.Result
		}
		_ = m.setCommandStatus(ctx, c.CommandID, cmdExpired, why)
		m.mailCommandOutcome(ctx, c, "Order lapsed: "+c.Title(m, ctx), why)
		return
	}
	why, err := m.chairAllowed(ctx, c)
	if err != nil {
		return // the database could not say; next pass
	}
	if why != "" {
		m.refuseCommand(ctx, c, why)
		return
	}
	switch c.Kind {
	case cmdKindFleet:
		m.takeFleetCommand(ctx, c)
	case cmdKindDirective:
		m.takeDirectiveCommand(ctx, c)
	case cmdKindWork:
		m.takeWorkCommand(ctx, c)
	default:
		if !m.takeOrgCommand(ctx, c) && !m.takeBoardCommand(ctx, c) {
			_ = m.setCommandStatus(ctx, c.CommandID, cmdRefused, "an order of a kind the desk does not know")
		}
	}
}

// commandFollow moves a claimed or live order along with what it became.
func (m *Mod) commandFollow(ctx context.Context, c corpCommand) {
	now := clock.Unix()
	if m.followOrgCommand(ctx, c) || m.followBoardCommand(ctx, c) {
		return
	}
	switch c.Kind {
	case cmdKindFleet:
		g, err := getGang(ctx, m.db, c.RefID)
		if err != nil {
			return
		}
		if g == nil {
			_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, "the fleet stood down and settled up")
			return
		}
		if c.Status == cmdClaimed && (g.State.OpPhase == gangOpLive || g.State.OpPhase == "") {
			_ = m.setCommandStatus(ctx, c.CommandID, cmdLive, "under way — "+m.gangOpNote(ctx, g))
		}
	case cmdKindDirective:
		p, _ := c.directive()
		hold := c.ClaimedAt + int64(p.HoldDays)*86400
		if now >= hold {
			_, _ = m.db.ExecContext(ctx,
				`DELETE FROM rolebot_corp_directive WHERE corporationID = ? AND setBy = 'human'`, c.CorpID)
			forgetCorpDirective(c.CorpID)
			_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, "the hold ran out; the chair may set a new one")
		}
	case cmdKindWork:
		var status string
		var done, wanted int
		err := m.db.QueryRowContext(ctx,
			`SELECT status, runsDone, runsWanted FROM rolebot_corp_work WHERE workID = ?`, c.RefID).
			Scan(&status, &done, &wanted)
		if err != nil {
			_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, "the order left the board")
			return
		}
		switch {
		case status == workDone || done >= wanted:
			unit := workUnit(workKindBuild)
			if p, ok := c.work(); ok {
				unit = workUnit(p.Kind)
			}
			_ = m.setCommandStatus(ctx, c.CommandID, cmdDone, fmt.Sprintf("%d %s delivered", done, unit))
			m.mailCommandOutcome(ctx, c, "Work order delivered: "+c.Title(m, ctx), fmt.Sprintf("%d of %d %s delivered.", done, wanted, unit))
		case status != "open":
			_ = m.setCommandStatus(ctx, c.CommandID, cmdRefused, "the board withdrew it ("+status+")")
		case done > 0 && c.Status == cmdClaimed:
			_ = m.setCommandStatus(ctx, c.CommandID, cmdLive, fmt.Sprintf("%d of %d delivered", done, wanted))
		}
	}
}

// ── fleet orders ────────────────────────────────────────────────────────────

// fleetOrderSpec is what a fleet order hands the formation code: the fleet
// the chair asked for instead of the one the seat would have raised.
type fleetOrderSpec struct {
	CommandID  int64
	SystemID   int64
	SystemName string
	Size       int
	LifeSec    int64
	OreTypeID  int64
	OreNames   string
	// CallToArms marks the uncapped fleet raised for a siege or an alliance
	// defend (gangtier.go, gangcalls.go): every free pilot, the siege
	// doctrine, a quorum muster.
	CallToArms bool
	// HelpCallID is the call to arms this fleet is raised for.
	HelpCallID int64
	// LineDoctrine makes a call-to-arms fleet fly the corporation's war line (sniper, kite,
	// brawl) instead of the siege doctrine. formCallsToArms sets it when no commander can seat
	// the siege doctrine.
	LineDoctrine bool
	// SiegeID marks the one alliance fleet raised for a siege (siegefleet.go): it musters at
	// StagingStationID (forward of the target, not the commander's home), recruits from
	// SiegeCorps, flies the siege muster doctrine, and is judged at its muster on QuotaDPS.
	SiegeID          int64
	StagingStationID int64
	SiegeCorps       []int64
	QuotaDPS         float64
}

func (m *Mod) takeFleetCommand(ctx context.Context, c corpCommand) {
	p, ok := c.fleet()
	if !ok {
		_ = m.setCommandStatus(ctx, c.CommandID, cmdRefused, "the order could not be read")
		return
	}
	seat, why := m.pickOrderCommander(ctx, c.CorpID, p.Activity)
	if why != "" {
		m.commandRetry(ctx, c, why)
		return
	}
	spec := &fleetOrderSpec{
		CommandID: c.CommandID, SystemID: p.SystemID, SystemName: p.SystemName,
		Size: p.Size, LifeSec: int64(p.DurationMin) * 60, OreNames: p.OreNames,
	}
	if len(p.Ores) > 0 {
		spec.OreTypeID = p.Ores[0]
	}
	formed, refusal, err := m.formFleetAround(ctx, seat, spec)
	if err != nil {
		m.commandRetry(ctx, c, "could not form: "+err.Error())
		return
	}
	if !formed {
		m.commandRetry(ctx, c, refusal)
		return
	}
	g, _, _ := gangForChar(ctx, m.db, seat.charID)
	if g == nil {
		m.commandRetry(ctx, c, "the fleet formed and dissolved at once")
		return
	}
	poster := m.charNameOr(ctx, c.PosterID)
	_ = m.claimCommand(ctx, c.CommandID, seat.charID, "gang", g.GangID,
		"mustering at "+m.stationLabel(ctx, g.State.StagingStationID))
	// The standing order beside it, so sibling fleets of the same kind steer
	// the same way (gangplan.go reads it), and the desks leave it alone.
	_ = m.postFleetOrder(ctx, corpFleetOrder{
		CorpID: c.CorpID, Dept: deptForActivity(p.Activity), SystemID: p.SystemID,
		OreNames: splitOres(p.OreNames), Reason: "ordered by " + poster, PostedBy: c.PosterID,
		Source: fleetOrderHuman, ExpiresAt: clock.Unix() + spec.LifeSec,
	})
	m.logBot(seat.charID, logInfo, "fleet", "took the chair's order — %s fleet to %s, %d pilots, ~%s",
		p.Activity, p.SystemName, p.Size, durationWords(p.DurationMin))
	m.humanLog(ctx, c.CorpID, c.PosterID, "fleet", seat.charID, 0,
		"%s's fleet order taken by %s: %s to %s", poster, m.charNameOr(ctx, seat.charID), activityLabel(p.Activity), p.SystemName)
	m.mailPilot(ctx, c.PosterID, seat.charID, "Fleet order taken: "+g.Name,
		fmt.Sprintf("I have your order. <b>%s</b> is mustering at %s — %d pilots, bound for %s, winding down in about %s. "+
			"It is on your Corporate Portal's Operations page; request a seat there if you want to fly along.",
			g.Name, m.stationLabel(ctx, g.State.StagingStationID), p.Size, p.SystemName, durationWords(p.DurationMin)))
}

// commandRetry counts a failed try and gives up after the retry allowance.
func (m *Mod) commandRetry(ctx context.Context, c corpCommand, why string) {
	_ = m.noteCommandAttempt(ctx, c.CommandID, why)
	if int64(c.Attempts)+1 >= orderRetries.Load() {
		_ = m.setCommandStatus(ctx, c.CommandID, cmdRefused, why)
		m.mailCommandOutcome(ctx, c, "Order refused: "+c.Title(m, ctx), "It could not be carried out: "+why+".")
	}
}

// pickOrderCommander finds a bot of the corporation to lead the ordered
// fleet: an idle seated commander of that activity first, else the best
// candidate by career who owns a hull for the seat and is free.
func (m *Mod) pickOrderCommander(ctx context.Context, corpID int64, activity string) (fcSeat, string) {
	seats, why := m.pickOrderCommanders(ctx, corpID, activity, 1)
	if len(seats) == 0 {
		return fcSeat{}, why
	}
	return seats[0], ""
}

// pickOrderCommanders is up to limit commanders for an ordered fleet, best first, in the order
// pickOrderCommander would try them. The caller tries each with formFleetAround, which holds the
// doctrine's own hull and fit tests. why is set when the list is empty.
func (m *Mod) pickOrderCommanders(ctx context.Context, corpID int64, activity string, limit int) ([]fcSeat, string) {
	var out []fcSeat
	have := map[int64]bool{}
	if seats, err := m.idleFleetCommanders(ctx); err == nil {
		for _, s := range seats {
			if len(out) < limit && s.corpID == corpID && s.activity == activity && !s.pack && !have[s.charID] {
				out = append(out, s)
				have[s.charID] = true
			}
		}
	}
	if len(out) >= limit {
		return out, ""
	}
	cands, err := m.rankCorpFCs(ctx, corpID)
	if err != nil {
		if len(out) > 0 {
			return out, ""
		}
		return nil, "the roster could not be read"
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Career[activity] != cands[j].Career[activity] {
			return cands[i].Career[activity] > cands[j].Career[activity]
		}
		return cands[i].Leadership > cands[j].Leadership
	})
	seatAct := seatActivityFor(activity, gangLeaderRole(activity))
	tried := 0
	for _, cand := range cands {
		if len(out) >= limit {
			break
		}
		if have[cand.CharID] || !m.isSynthetic(cand.CharID) || popIsOffline(cand.CharID) || tzShortHours(cand.CharID) {
			continue
		}
		if g, _, _ := gangForChar(ctx, m.db, cand.CharID); g != nil {
			continue
		}
		if st, err := loadState(ctx, m.db, cand.CharID); err != nil || (st != nil && st.Halted) {
			continue
		}
		tried++
		if tried > 8+limit {
			break
		}
		if ok, err := m.hasSeatShip(ctx, cand.CharID, seatAct); err == nil && ok {
			out = append(out, fcSeat{corpID: corpID, charID: cand.CharID, activity: activity})
			have[cand.CharID] = true
		}
	}
	if len(out) == 0 {
		return nil, "nobody aboard can command it yet — no free member with a hull for the seat"
	}
	return out, ""
}

func deptForActivity(activity string) string {
	switch activity {
	case ActMining, ActIndMining:
		return portOperations
	}
	return portSecurity
}

func splitOres(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// ── directives ──────────────────────────────────────────────────────────────

func (m *Mod) takeDirectiveCommand(ctx context.Context, c corpCommand) {
	p, ok := c.directive()
	if !ok || len(p.Weights) == 0 {
		_ = m.setCommandStatus(ctx, c.CommandID, cmdRefused, "the directive could not be read")
		return
	}
	now := clock.Unix()
	hold := now + int64(p.HoldDays)*86400
	reason := truncate("ordered by "+m.charNameOr(ctx, c.PosterID), 255)
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM rolebot_corp_directive WHERE corporationID = ?`, c.CorpID); err != nil {
		return
	}
	for act, w := range p.Weights {
		if w <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rolebot_corp_directive (corporationID, activity, weight, reason, setAt, setBy, holdUntil, strength)
			VALUES (?, ?, ?, ?, ?, 'human', ?, ?)`, c.CorpID, act, w, reason, now, hold, p.Strength); err != nil {
			return
		}
	}
	if err := tx.Commit(); err != nil {
		return
	}
	forgetCorpDirective(c.CorpID)
	_ = m.claimCommand(ctx, c.CommandID, c.PosterID, "directive", c.CorpID, "")
	_ = m.setCommandStatus(ctx, c.CommandID, cmdLive, "in force until "+esp_YC(hold))
	m.humanLog(ctx, c.CorpID, c.PosterID, "directive", c.CorpID, p.Strength,
		"%s set the directive: %s, held %d day(s)", m.charNameOr(ctx, c.PosterID), directiveText(p.Weights), p.HoldDays)
	m.queueCorpNews(ctx, c.CorpID, newsDirective,
		fmt.Sprintf("%s set the corporation's directive: %s.", m.charNameOr(ctx, c.PosterID), directiveText(p.Weights)))
}

// humanDirectiveHold is the human-set directive's push and its hold, zeros
// when the chair has none in force.
func (m *Mod) humanDirectiveHold(ctx context.Context, corpID int64) (strength float64, holdUntil int64) {
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(strength), 0), COALESCE(MAX(holdUntil), 0)
		  FROM rolebot_corp_directive WHERE corporationID = ? AND setBy = 'human'`, corpID).Scan(&strength, &holdUntil)
	if holdUntil <= clock.Unix() {
		return 0, 0
	}
	return strength, holdUntil
}

// ── work orders ─────────────────────────────────────────────────────────────

func (m *Mod) takeWorkCommand(ctx context.Context, c corpCommand) {
	p, ok := c.work()
	if !ok || p.Runs <= 0 || p.BPTypeID == 0 {
		_ = m.setCommandStatus(ctx, c.CommandID, cmdRefused, "the work order could not be read")
		return
	}
	stationID := p.StationID
	if stationID == 0 {
		stationID, _ = m.corpFactoryOffice(ctx, c.CorpID)
	}
	if stationID == 0 {
		m.commandRetry(ctx, c, "the corporation has no factory office to build at")
		return
	}
	kind := p.Kind
	if kind == "" {
		kind = workKindBuild
	}
	prints, err := m.libraryPrints(ctx, c.CorpID, stationID)
	if err != nil {
		m.commandRetry(ctx, c, "the library could not be read")
		return
	}
	have := false
	for _, pr := range prints {
		if pr.BPTypeID == p.BPTypeID && (p.BPItemID == 0 || pr.ItemID == p.BPItemID) {
			have = true
			break
		}
	}
	if !have && kind == workKindBuild {
		// A build runs on a copy as well.
		if copies, cerr := m.libraryCopies(ctx, c.CorpID, stationID); cerr == nil {
			for _, pr := range copies {
				have = have || pr.BPTypeID == p.BPTypeID
			}
		}
	}
	if !have {
		_ = m.setCommandStatus(ctx, c.CommandID, cmdRefused,
			"the library at "+m.stationLabel(ctx, stationID)+" holds no "+m.typeNameOr(ctx, p.BPTypeID))
		m.mailCommandOutcome(ctx, c, "Work order refused: "+c.Title(m, ctx),
			"The library at "+m.stationLabel(ctx, stationID)+" does not hold that print any more.")
		return
	}
	if p.BPItemID != 0 {
		if why := m.printBusy(ctx, c.CorpID, p.BPItemID); why != "" {
			m.refuseCommand(ctx, c, why)
			return
		}
	}
	if why := m.afford(ctx, c.CorpID, p.WagePerRun*float64(p.Runs)); why != "" {
		m.commandRetry(ctx, c, why)
		return
	}
	if kind != workKindBuild {
		m.ensureLabBase(ctx, c.CorpID, stationID)
	}
	priority := p.Priority
	if priority == 0 {
		priority = 100
	}
	note := m.postedLine(ctx, c)
	if kind == workKindCopy {
		if f, ferr := m.printFactsFor(ctx, p.BPTypeID); ferr == nil {
			note = clampText(fmt.Sprintf("%d-run copies, %s", f.CopyRuns, note), 255)
		}
	}
	workID, err := m.postWork(ctx, corpWork{
		CorpID: c.CorpID, Kind: kind, BPTypeID: p.BPTypeID, ProductTypeID: p.ProductTypeID, StationID: stationID,
		RunsWanted: p.Runs, WagePerRun: p.WagePerRun, Purpose: "human", Priority: priority,
		Note: note, BPItemID: p.BPItemID, RequesterArm: p.RequesterArm, DestFlag: p.DestFlag,
		PostedBy: c.PosterID, Source: fleetOrderHuman,
	})
	if err != nil && workID == 0 {
		m.commandRetry(ctx, c, "the board refused it: "+err.Error())
		return
	}
	forgetWorkDesk(c.CorpID)
	_ = m.claimCommand(ctx, c.CommandID, c.PosterID, "work", workID, "on the board at "+m.stationLabel(ctx, stationID))
	m.humanLog(ctx, c.CorpID, c.PosterID, "work", p.ProductTypeID, p.WagePerRun*float64(p.Runs),
		"%s posted work: %s %s, %d %s at %s each", m.charNameOr(ctx, c.PosterID), workKindWords(kind),
		m.typeNameOr(ctx, p.ProductTypeID), p.Runs, workUnit(kind), iskString(p.WagePerRun))
}

// ensureLabBase makes an office an R&D base, so the members based there are offered its research
// (corpWorkStation reads the purposes).
func (m *Mod) ensureLabBase(ctx context.Context, corpID, stationID int64) {
	for _, st := range m.armBaseStations(ctx, corpID, armResearch) {
		if st == stationID {
			return
		}
	}
	if stationID == m.corpFactoryStation(ctx, corpID) {
		return
	}
	m.claimOfficePurpose(ctx, corpID, stationID, officeLab)
	if _, err := m.syncOfficePurposes(ctx, corpID); err != nil {
		rlogf("rolebots: lab base %d of corp %d: %v", stationID, corpID, err)
	}
}

// ── the work desk test ──────────────────────────────────────────────────────

var (
	workDeskMu   sync.Mutex
	workDeskMemo = map[int64]struct {
		has bool
		at  int64
	}{}
)

func forgetWorkDesk(corpID int64) {
	workDeskMu.Lock()
	delete(workDeskMemo, corpID)
	workDeskMu.Unlock()
}

// corpHasWorkDesk reports whether a corporation runs a Production Board: a
// bot CEO's and a human chair's always, any other's while somebody has work posted.
func (m *Mod) corpHasWorkDesk(ctx context.Context, corpID int64) bool {
	if corpID == 0 {
		return false
	}
	if corpDeskRun(corpID) {
		return true
	}
	now := clock.Unix()
	workDeskMu.Lock()
	memo, hit := workDeskMemo[corpID]
	workDeskMu.Unlock()
	if hit && now-memo.at < workDeskMemoSec {
		return memo.has
	}
	var n int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_work WHERE corporationID = ? AND status = 'open' AND source = 'human'`,
		corpID).Scan(&n)
	workDeskMu.Lock()
	workDeskMemo[corpID] = struct {
		has bool
		at  int64
	}{n > 0, now}
	workDeskMu.Unlock()
	return n > 0
}

// workDeskCorpIDs is every corporation with a person's work open, or under a human chair with any:
// the wage sweep's scope beside the bot-run corporations.
func (m *Mod) workDeskCorpIDs(ctx context.Context) []int64 {
	rows, err := m.db.QueryContext(ctx,
		`SELECT corporationID, MAX(source = 'human') FROM rolebot_corp_work WHERE status = 'open' GROUP BY corporationID`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id, person int64
		if rows.Scan(&id, &person) != nil {
			continue
		}
		if _, human := humanChaired(id); human || person != 0 {
			out = append(out, id)
		}
	}
	return out
}

// ── audit and letters ───────────────────────────────────────────────────────

// humanLog is ceoLog's sibling for a human's order: the same ledger the CEO
// pages read, power "human", the poster as actor.
func (m *Mod) humanLog(ctx context.Context, corpID, posterID int64, verb string, subjectID int64, amount float64, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_ceo_actions (corporationID, ceoID, at, power, verb, subjectID, amount, detail, actorID)
		VALUES (?, ?, ?, 'human', ?, ?, ?, ?, ?)`,
		corpID, m.corpCEOID(ctx, corpID), clock.Unix(), verb, subjectID, amount, truncate(detail, 255), posterID); err != nil {
		rlogf("rolebots: human log: %v", err)
	}
	m.logBot(0, logInfo, "order", "%s", detail)
}

// mailCommandOutcome tells the poster what became of their order, from the
// corporation.
func (m *Mod) mailCommandOutcome(ctx context.Context, c corpCommand, subject, body string) {
	m.mailPilot(ctx, c.PosterID, c.CorpID, subject, body+
		"<br><br>Your orders are on the "+igbLink(managePath, "Management page")+" of the Corporate Portal.")
}

// esp_YC words a unix time as a YC date for a result sentence.
func esp_YC(unix int64) string { return igbWhen(unix) }

// encodeCommandPayload marshals a payload for a row.
func encodeCommandPayload(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
