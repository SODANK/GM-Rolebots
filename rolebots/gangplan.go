package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// Gang engine: the op each role runs while its gang has an activity set.
//
// Mining ("cans"): miners fly to the op belt and stay, jettison full loads into cargo
// containers and put the lasers back on. The hauler loops belt and station emptying
// everyone's cans (a real player member's jettisoned cans included). Combat pilots patrol
// the op system's belts and keep the rats off everyone.
//
// Bounty (PvE sweep): hunters pick a system in the configured security band, comb its belts
// together and rotate to the next system when it has been worked. There is no trade fleet:
// trading is a solo career (trade.go) and a corporation trades through its Commerce desk.

const (
	gangWaitSec       = 60 // hauler with no cans, member with no site yet
	gangDealTTLSec    = 20 * 60
	gangHuntSweepSec  = 15 * 60
	gangHuntRecentMax = 6

	// Hauler pacing: how long loading at the belt takes, and how often an
	// empty-handed hauler re-checks the cans.
	gangLoadSec     = 45
	gangHaulPollSec = 60

	// Patrol pacing: dwell on one belt before bouncing to the next.
	gangPatrolDwellMinSec = 60
	gangPatrolDwellMaxSec = 150
)

// gangUnfitRetrySec is how long an excused member stays off the gang's books
// (gangSeatReady): long enough for a home trip and a shopping pass, short enough that a
// pilot that gears up rejoins the same evening's op.
const gangUnfitRetrySec = 1800

// gangSeatReady is the check-only half of ensureActivityShip for gang seats: is this pilot
// in, or one boarding away from, a working ship for the seat's job? It buys nothing.
// ready=false with a nil error means the pass is handled: the pilot is flying to its own
// ship, or was excused with a GangUnfitUntil memo that makes the gang gate skip it.
func (m *Mod) gangSeatReady(ctx context.Context, b *botRun, activity string) (bool, error) {
	excuse := func(why string) (bool, error) {
		st := b.State
		m.logBot(b.CharID, logInfo, "gang",
			"not geared for the %s seat (%s) — sitting this op out to sort itself out at home", activity, why)
		m.narrateMember(ctx, st.Data.GangID, b.CharID, "No ship for this seat — sitting the op out.")
		// An excused commander is the whole fleet gone, since only the leader picks the site: hand
		// the seat over before the memo goes down, while the gang row still names this pilot.
		m.handOverGangLead(ctx, b.CharID, why)
		m.leaveGangMuster(ctx, gangIDOf(b), b.CharID)
		st.Data.GangUnfitUntil = clock.Unix() + gangUnfitRetrySec
		st.Data.GangID, st.Data.GangBeltID = 0, 0
		// Still unfit when the memo lapses? It is excused again: the check costs a few reads and
		// keeps a pilot whose fit is on procure cooldown from wedging the seat.
		return false, m.planSolo(ctx, b)
	}

	// A kit the corporation lent for this fleet (corparmory_issue.go) is the seat's ship,
	// whatever the pilot's own hangar holds.
	if holdsLoanedKit(b.CharID) {
		if is := m.openIssueFor(ctx, b.CharID); is != nil && is.GangID == gangIDOf(b) {
			return m.boardIssuedKit(ctx, b, is, activity)
		}
	}
	v, err := m.seatVerdict(ctx, b, activity)
	if err != nil {
		return false, err
	}
	if v.deadShip {
		// Clear the dead record as ensureActivityShip does, so the solo pass it is excused into
		// starts from the truth.
		if err := setFitShip(ctx, m.db, b.CharID, v.a.FitID, 0); err != nil {
			return false, err
		}
	}
	if v.why != "" {
		return excuse(v.why)
	}
	a, t := v.a, v.t
	// Only the free half of ammo management: rounds in the hold go into the guns. What the
	// hold does not carry, the op flies without.
	if _, err := m.reloadFromCargo(ctx, b, a.ShipItemID, t.Fit); err != nil {
		return false, err
	}

	if b.ShipID == a.ShipItemID {
		return true, nil
	}
	var shipLoc int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`, a.ShipItemID).Scan(&shipLoc); err != nil {
		return false, fmt.Errorf("ship %d location: %w", a.ShipItemID, err)
	}
	if shipLoc == b.StationID && b.StationID != 0 {
		return true, m.boardShip(ctx, b, a.ShipItemID)
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, shipLoc)
	if err != nil {
		return false, fmt.Errorf("%s ship %d is not in a reachable station: %w", activity, a.ShipItemID, err)
	}
	m.logBot(b.CharID, logInfo, "gang", "collecting its %s ship from station %d for the op", activity, shipLoc)
	b.State.Activity = activity
	return false, m.beginTravel(ctx, b, loc.SolarSystemID, shipLoc, string(TaskBoard))
}

// seatVerdictResult is gangSeatReady's judgement on a pilot, with nothing
// acted on: why is empty when the pilot is fit for the seat.
type seatVerdictResult struct {
	a        *fitShip
	t        *fitTemplate
	why      string
	deadShip bool // the fit's ship record points at a hull that no longer exists
}

// seatVerdict is the strict seat test with no side effects: the questions gangSeatReady
// asks before it excuses anyone, answered without excusing, boarding, reloading or
// clearing a dead record. Recruiting asks it of its picks (gangRecruitFrom), since the cheap
// probe (hasSeatShip) passes a pilot whose hull is the wrong one or half-fitted.
func (m *Mod) seatVerdict(ctx context.Context, b *botRun, activity string) (seatVerdictResult, error) {
	var v seatVerdictResult
	// selectFlyableFit, not selectFitForActivity: this check buys nothing, and the buying
	// selector's upgrade arm answers with a fit the pilot does not own yet (fitselect.go).
	a, t, err := m.selectFlyableFit(ctx, b, activity)
	if err != nil {
		return v, err
	}
	if a == nil {
		v.why = "no fit it can fly"
		return v, nil
	}
	v.a, v.t = a, t
	if a.ShipItemID != 0 && !a.ShipExists {
		// A dead record: the caller that acts clears it (gangSeatReady).
		v.deadShip = true
		v.why = "no ship for the seat"
		return v, nil
	}
	if a.ShipItemID == 0 {
		v.why = "no ship for the seat"
		return v, nil
	}
	if sameHull, err := m.shipHullMatches(ctx, a.ShipItemID, t.Fit); err != nil {
		return v, err
	} else if !sameHull {
		v.why = "its ship is not the seat's hull"
		return v, nil
	}
	// A hauler's seat is industrial-only (haulerSeatSpec). The fit pipeline is steered there,
	// but a pilot whose only cargo hull is a frigate still gets it, and ore rots in the cans.
	if activity == ActTrade {
		if s, ok := m.seatSpecFor(ctx, b.CharID); ok && isHaulerRole(s.Role) {
			if byID, _, herr := m.loadHullProfiles(ctx); herr == nil {
				if h := byID[t.Fit.HullTypeID]; h == nil || !hullInBand(h.Class, s) {
					v.why = "its ship is not an industrial"
					return v, nil
				}
			}
		}
	}
	if !m.seatRackOK(ctx, activity, a.ShipItemID) {
		v.why = "its ship carries no logistics rack"
		return v, nil
	}
	t = m.alignedToShip(ctx, t, a.ShipItemID) // see ensureActivityShip
	v.t = t
	missing, err := m.shipMissing(ctx, a.ShipItemID, t.Fit)
	if err != nil {
		return v, err
	}
	// A fitted module of the planned module's group sits the seat in its place
	// (seatSubstitutesCover); the plan's upgrade is procurement's business, not the seat's.
	if len(missing) > 0 {
		if slots, serr := m.shipSlotModules(ctx, a.ShipItemID); serr == nil {
			if mods, merr := m.allModulesByID(ctx); merr == nil {
				missing = seatSubstitutesCover(missing, slots, t.Fit, func(id int64) int64 {
					if mm := mods[id]; mm != nil {
						return mm.GroupID
					}
					return 0
				})
			}
		}
	}
	// The rule a solo pilot flies by (fitshort.go): a combat, tackle or loot seat is not taken
	// in a hull with half its rack missing. Trade and mining seats keep "does it do the job".
	if len(missing) > 0 && !shopIsConsumablesOnly(missing) {
		if ok, why := m.shortFitVerdict(ctx, t, missing); !ok {
			v.why = "its ship is missing " + shopSummary(shopHardLines(missing)) + " — " + why
			return v, nil
		}
	}
	// A short-fitted ship that still does the job is seat-worthy, except a toothless one on a
	// combat or belt seat, which every belt refuses.
	if beltWork(activity) && m.shipHasNoTeeth(ctx, a.ShipItemID) {
		v.why = "its ship has no gun, launcher or drone bay"
		return v, nil
	}
	return v, nil
}

// hasSeatShip is the cheap half of gangSeatReady's ship test: does this pilot own a live
// hull against one of the fits its activity draws from? It is silent because it runs over
// every idle commander seat on the formation sweep. The strict test (right hull, fitted,
// boarded) runs at gangSeatReady when the op starts.
func (m *Mod) hasSeatShip(ctx context.Context, charID int64, activity string) (bool, error) {
	pool, err := m.candidatePool(ctx, charID, m.jobForPilot(ctx, charID, activity))
	if err != nil || len(pool) == 0 {
		return false, err
	}
	ships, err := shipsForChar(ctx, m.db, charID)
	if err != nil {
		return false, err
	}
	for _, t := range pool {
		if shipID, ok := ships[t.FitID]; ok && shipID != 0 && entityExists(ctx, m.db, shipID, charID) &&
			m.seatRackOK(ctx, activity, shipID) {
			return true, nil
		}
	}
	return false, nil
}

// handOverGangLead promotes somebody else when the commander leaves the op. The successor
// must do the commander's job: a driven working member, never an escort, that owns a hull
// for the activity. With nobody fit the fleet is expired, not deleted, because expiry goes
// through the wind-down settlement and deletion would drop the ledgers unpaid.
func (m *Mod) handOverGangLead(ctx context.Context, charID int64, why string) {
	g, _, err := gangForChar(ctx, m.db, charID)
	if err != nil || g == nil || g.LeaderCharID != charID {
		return
	}
	mems, err := gangMembers(ctx, m.db, g.GangID)
	if err != nil {
		rlogf("rolebots: hand over gang %d: %v", g.GangID, err)
		return
	}
	fit := func(mem GangMember) bool {
		if mem.CharacterID == charID || !mem.Driven || mem.Role == RoleEscort {
			return false
		}
		// Never promote an excused pilot (GangUnfitUntil): the gang gate skips it, so the fleet's
		// commander would never reach gangPlanHunt.
		mb, err := m.loadRun(ctx, mem.CharacterID)
		if err != nil || mb == nil || mb.State == nil || clock.Unix() < mb.State.Data.GangUnfitUntil {
			return false
		}
		// The strict seat test, acting on nothing: a successor gangSeatReady would excuse on its
		// own first step hands the seat on again, one pilot per pass, until the fleet is gone.
		v, err := m.seatVerdict(ctx, mb, seatActivityFor(g.Activity, mem.Role))
		return err == nil && v.why == ""
	}
	promote := func(mem GangMember) bool {
		if err := setGangLeader(ctx, m.db, g.GangID, mem.CharacterID); err != nil {
			rlogf("rolebots: hand over gang %d: %v", g.GangID, err)
			return true // handled, badly; nothing further to try
		}
		// The command's seat moves with the leader's (fleetcommand.go).
		if g.State.Command != nil {
			g.State.Command.FC = mem.CharacterID
			if g.State.Command.Anchor == charID {
				g.State.Command.Anchor = mem.CharacterID
			}
			if err := saveGangState(ctx, m.db, g.GangID, &g.State); err != nil {
				rlogf("rolebots: hand over gang %d: save command: %v", g.GangID, err)
			}
		}
		m.logBot(mem.CharacterID, logInfo, "gang",
			"taking over %q — its commander stood down (%s)", g.Name, why)
		return true
	}
	// The succession first, where the fleet has one (fleetcommand.go): the order the
	// commander's formation wrote down, by Leadership.
	if g.State.Command != nil {
		byID := map[int64]GangMember{}
		for _, mem := range mems {
			byID[mem.CharacterID] = mem
		}
		for _, id := range g.State.Command.Succession {
			if mem, ok := byID[id]; ok && fit(mem) {
				if promote(mem) {
					return
				}
			}
		}
	}
	// The commander's own role first: the seat's duties live in that role's planner (the hunt
	// system is picked in gangPlanHunt), so a fleet led from another seat has a blind leader.
	lead := gangLeaderRole(g.Activity)
	for _, preferred := range []bool{true, false} {
		for _, mem := range mems {
			if (mem.Role == lead) != preferred || !fit(mem) {
				continue
			}
			if promote(mem) {
				return
			}
		}
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_gangs SET expiresAt = ?, updatedAt = ? WHERE gangID = ?`,
		clock.Unix(), clock.Unix(), g.GangID); err != nil {
		rlogf("rolebots: expire headless gang %d: %v", g.GangID, err)
	}
	m.logBot(charID, logInfo, "gang",
		"stood down from command of %q (%s) and nobody aboard can take the seat — winding the fleet down",
		g.Name, why)
}

// gangHoldsThisPilot reports whether this pilot is a member of a fleet with a live
// activity, the question every solo disposal asks before it sells cargo that is not its
// own. It reads b.GangID, not st.Data.GangID: gangSeatReady's excusal zeroes the state
// field but the membership row stands. A deleted gang answers false.
func (m *Mod) gangHoldsThisPilot(ctx context.Context, b *botRun) bool {
	if b == nil || b.GangID == 0 {
		return false
	}
	var activity string
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(activity, '') FROM rolebot_gangs WHERE gangID = ? AND enabled = 1`,
		b.GangID).Scan(&activity); err != nil {
		return false
	}
	return activity != ""
}

// gangCallUp is the muster's claim on a pilot busy with something of its own; it reports
// whether it took the pilot. plan() is the only place gang duty is checked, and it runs
// from idle. TaskDef.Interruptible decides what may be dropped: a pilot holding unfitted
// modules, unsold cargo, a line job or a criminal timer is left alone.
func (m *Mod) gangCallUp(ctx context.Context, b *botRun) bool {
	if b == nil || b.State == nil || b.GangID == 0 {
		return false
	}
	st := b.State
	if st.Phase == string(TaskGangMuster) || st.Phase == string(TaskGangFormUp) ||
		clock.Unix() < st.Data.GangUnfitUntil {
		// Already answering (in the hangar or holding formation outside) or excused: re-claiming
		// an excused pilot every pass is the loop the memo breaks (gangSeatReady).
		return false
	}
	// Claiming an idle pilot is a livelock: advance() reports it handled, but the idle step is
	// plan(), the only route into gangPlanMuster. Idle is Interruptible for gangRecruit's sake.
	if st.Phase == "" || st.Phase == string(TaskIdle) {
		return false
	}
	if !interruptibleState(st) {
		return false
	}
	// Claimed a moment ago and busy again: plan()'s ladder sent it on an errand that outranks
	// gang duty (gangcallup.go), and a second claim would only break that leg off.
	if gangCalledRecently(b.CharID) {
		return false
	}
	g, _, err := gangForChar(ctx, m.db, b.CharID)
	if err != nil || g == nil || g.Activity == "" {
		return false
	}
	// The muster calls its members in; so does a fleet born live for its first quarter-hour
	// (gangtier.go), so a member recruited mid-belt-run comes and meets the fleet at its site.
	mustering := g.State.OpPhase == gangOpMuster && g.State.StagingStationID != 0
	joining := g.State.OpPhase == gangOpLive && clock.Unix() < g.State.JoinBy
	if !mustering && !joining {
		return false
	}
	where := "muster at " + m.stationLabel(ctx, g.State.StagingStationID)
	if joining {
		where = "join it"
	}
	was := "what it was doing"
	if d, ok := taskDef(st.Phase); ok {
		was = d.Label
	}
	m.logBot(b.CharID, logInfo, "gang",
		"called up by gang %q — breaking off (%s) to %s",
		g.Name, was, where)
	noteGangCalled(b.CharID)
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	if err := saveState(ctx, m.db, st); err != nil {
		rlogf("rolebots: gang call-up %d: %v", b.CharID, err)
		return false
	}
	return true
}

// gangDutyData is idleData for a pilot starting a leg of gang g's op: per-trip fields
// cleared, gang duty kept. GangSiteSeq must survive with GangID, because gangPlan stamps
// it before routing here and a leg that dropped it would re-plan and loop after travel.
func gangDutyData(prev StateData, g *Gang) StateData {
	d := idleData(prev)
	d.GangID = g.GangID
	d.GangSiteSeq = g.State.SiteSeq
	return d
}

// gangPlan routes an idle gang member into its role's op. Called from plan() before the
// solo rotation: gang duty wins.
func (m *Mod) gangPlan(ctx context.Context, b *botRun, g *Gang, mem *GangMember) error {
	st := b.State
	st.Data.GangID = g.GangID
	st.Data.GangSiteSeq = g.State.SiteSeq

	// A guest (a seconded escort or an ally's pilot in a working seat) is here only while the
	// politics it was recruited under hold; that is re-asked every plan.
	if mem != nil && mem.RecruitCorpID != 0 {
		if released, err := m.gangEscortStillWelcome(ctx, b, g, mem); err != nil || released {
			return err
		}
	}

	// The lifecycle gate comes before the role dispatch: a fleet in muster is gathering and
	// one in wind-down is going home, whatever the activity.
	switch g.State.OpPhase {
	case gangOpMuster:
		return m.gangPlanMuster(ctx, b, g, mem)
	case gangOpWindDown:
		return m.gangPlanWindDown(ctx, b, g, mem)
	}

	// Each member is held to its own seat's fit (logi, tackle, EWAR, scout or the gun seat),
	// the same test formation and the muster put it through (seatActivityFor).
	seat := seatActivityFor(g.Activity, mem.Role)
	switch g.Activity {
	case ActMining, ActIndMining:
		// The industry fleet flies the mining op verbatim; the difference is in the haul steps
		// (gangindustry.go).
		switch mem.Role {
		case RoleMiner:
			return m.gangPlanMiner(ctx, b, g)
		case RoleHauler, RoleTrader:
			return m.gangPlanHauler(ctx, b, g)
		case RoleCombat, RoleHunter, RoleTackler, RoleEscort:
			// A seconded gun on a mining op is a patrol and needs nothing of
			// its own: the belts it sweeps are the belts the fleet mines.
			return m.gangPlanPatrol(ctx, b, g, seat)
		}
	case ActCombat:
		switch mem.Role {
		case RoleHunter, RoleCombat, RoleTackler, RoleLogi, RoleEwar:
			return m.gangPlanHunt(ctx, b, g, seat)
		}
	case ActSecurity:
		// The employer's own patrol: a hunting pack's seats and ships, with gangSecuritySite
		// confining it to the home region. A checkpoint fleet holds one gate instead (camp.go).
		switch mem.Role {
		case RoleHunter, RoleCombat, RoleTackler, RoleLogi, RoleEwar:
			if g.State.Focus == gangFocusCheckpoint {
				return m.gangPlanCheckpoint(ctx, b, g, seat)
			}
			return m.gangPlanSecurity(ctx, b, g, seat)
		}
	case ActPiracy:
		if g.State.Focus == gangFocusGank {
			// The gank fleet (gank_op.go): every seat has a part, the scout on the gate and the guns
			// staged one jump out.
			switch mem.Role {
			case RoleCombat, RoleHunter, RoleScout, RoleTackler, RoleLogi, RoleEwar:
				return m.gangPlanGank(ctx, b, g, mem)
			}
			break
		}
		switch mem.Role {
		case RoleCombat, RoleHunter, RoleTackler, RoleLogi, RoleEwar, RoleScout:
			// A scout in an ordinary raid rides the pirate op's plan: unarmed,
			// it camps with the pack and piracyTarget hands it nothing to shoot.
			return m.gangPlanPirate(ctx, b, g, seat)
		}
	}

	// This role has no job under this activity: sit the op out on the pilot's own schedule.
	m.logBot(b.CharID, logInfo, "gang",
		"gang %q runs %s and a %s has no part in it — falling back to this pilot's own rotation",
		g.Name, g.Activity, mem.Role)
	st.Data.GangID, st.Data.GangBeltID = 0, 0
	return m.planSolo(ctx, b)
}

// gangStep advances every gang.* phase. The gang is re-read each step: the
// operator disabling it, clearing its activity, or rebooting it pulls every
// member back to its own planning within one pass.
func (m *Mod) gangStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	// Membership is re-checked, not just the gang row: a member removed on the panel mid-op
	// walks off the op. The one phase that outlives its gang is the hauler's ore drop.
	if st.Phase == string(TaskGangOreDrop) {
		return false, m.gangOreDropStep(ctx, b)
	}
	g, _, err := gangForChar(ctx, m.db, b.CharID)
	if err != nil {
		return false, err
	}
	// The commander listens for kin fleets' calls while it works (gangcalls.go): a call
	// moves the whole fleet, and its SiteSeq bump makes every member re-plan below.
	if g != nil && g.GangID == st.Data.GangID && b.CharID == g.LeaderCharID && gangSiteBound(st.Phase) &&
		(g.Activity == ActCombat || g.Activity == ActSecurity) {
		if _, err := m.adoptHelpCall(ctx, b, g); err != nil {
			return false, err
		}
	}
	if g != nil && g.GangID == st.Data.GangID && g.State.SiteSeq != st.Data.GangSiteSeq && gangSiteBound(st.Phase) {
		// A Director moved the op (fleetmirror_orders.go): this leg points at ground the fleet
		// has left. Back to the planner; a hauler mid-run finishes its run.
		m.logBot(b.CharID, logInfo, "gang", "the op moved — re-planning for the new site")
		st.Data.GangSiteSeq = g.State.SiteSeq
		st.Phase = "idle"
		st.Data.LegEndsAt = 0
		return false, saveState(ctx, m.db, st)
	}
	if g == nil || g.GangID != st.Data.GangID || g.Activity == "" {
		m.logBot(b.CharID, logInfo, "gang", "gang op is over — back to its own schedule")
		// A hauler disbanded mid-trip still carries the fleet's rock. The claims died with the
		// gang, so it is this pilot's now and is brought in (strandedOreHome).
		if handled, err := m.strandedOreHome(ctx, b); err != nil || handled {
			return false, err
		}
		st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}

	// A wreck this member made is waiting (noteBeltLoot): it loots it for
	// itself before going back to its seat (gangloot.go).
	if st.Data.HuntLootID != 0 && lootsPhase(st.Phase) {
		st.Phase = string(TaskGangSalvage)
		return false, saveState(ctx, m.db, st)
	}
	switch st.Phase {
	case string(TaskGangSalvage):
		return false, m.gangSalvageStep(ctx, b, g)
	case "gang.mine":
		return false, m.gangMineStep(ctx, b, g)
	case "gang.haul.load":
		return m.gangHaulLoadStep(ctx, b, g)
	case "gang.haul.drop":
		return true, m.gangHaulDropStep(ctx, b, g)
	case "gang.patrol":
		return false, m.gangPatrolStep(ctx, b, g)
	case "gang.hunt":
		return false, m.gangHuntStep(ctx, b, g, "gang.hunt")
	case "gang.security":
		return false, m.gangHuntStep(ctx, b, g, "gang.security")
	case "gang.pirate.prowl", "gang.pirate.camp":
		return false, m.gangPirateStep(ctx, b, g)
	case string(TaskGangGankStage), string(TaskGangGankScout), string(TaskGangGankStrike), string(TaskGangGankLoot):
		return false, m.gangGankStep(ctx, b, g)
	case string(TaskGangCheckpoint):
		return false, m.gangCheckpointStep(ctx, b, g)
	case "gang.muster":
		return false, m.gangMusterStep(ctx, b, g)
	case "gang.formup":
		return false, m.gangFormUpStep(ctx, b, g)
	case "gang.settle":
		return false, m.gangSettleStep(ctx, b, g)
	case "gang.wait":
		// Waited out the pause: re-plan (plan() routes back into the gang, which re-checks
		// whether there is work now).
		st.Activity, st.Phase = "", "idle"
		return false, saveState(ctx, m.db, st)
	}
	return false, fmt.Errorf("unknown gang phase %q", st.Phase)
}

// gangLastRunGraceSec is how much longer than the wind-down deadline the
// haulers' last collection run may take (gangPlanWindDown). Granted once per
// fleet, when the run is first called.
const gangLastRunGraceSec = 20 * 60

// gangWait parks the bot in a hold pattern without counting as a failure: a hauler whose
// miners have not filled a can yet is not broken.
func (m *Mod) gangWait(ctx context.Context, b *botRun, why string) error {
	st := b.State
	m.logBot(b.CharID, logInfo, "gang", "holding — %s", why)
	st.Phase = "gang.wait"
	st.Data.LegEndsAt = clock.Unix() + gangWaitSec
	return saveState(ctx, m.db, st)
}

// ── mining op: shared site ──────────────────────────────────────────────────

// gangMiningConfigOf reads the gang's mining config (the solo activity's JSON shape,
// minus disposal defaults).
func gangMiningConfigOf(g *Gang) miningConfig {
	cfg := miningConfig{AvoidHostileBelts: true, Disposal: "sell"}
	_ = gangConfig(g.Config, &cfg)
	if len(cfg.OrePriority) == 0 {
		cfg.OrePriority = defaultOrePriority
	}
	if cfg.Disposal != "stockpile" && cfg.Disposal != "refine" {
		cfg.Disposal = "sell"
	}
	return cfg
}

// gangMiningPlan is the op's config as the fleet will fly it. An operator's ore list and
// disposal are kept as given; an uninstructed fleet works the dearest rock in reach
// (oreDigValues), as a solo miner does, and sells it.
func (m *Mod) gangMiningPlan(ctx context.Context, b *botRun, g *Gang) miningConfig {
	cfg := gangMiningConfigOf(g)
	if !gangConfigHas(g.Config, "orePriority") {
		cfg.ByValue = true
		// The Director of Operations' standing order (corpstaff_dept.go): the ores the factory is
		// short of. Not strict, and never over an operator's own ore list.
		if o := m.corpFleetOrderFor(ctx, g.CorpID, portOperations); o != nil && len(o.OreNames) > 0 {
			cfg.OrePriority = append([]string(nil), o.OreNames...)
			cfg.ByValue = false
			g.State.OrderOres = strings.Join(o.OreNames, ",")
		}
	}
	return cfg
}

// gangHomeRegion is the region an op belongs to: the one its site sits in, else the
// asking member's home region. gangMiningPlan runs before gangMiningSite on the first
// pass, so without the fallback the choice of the op's ore would have no region to use.
func (m *Mod) gangHomeRegion(ctx context.Context, b *botRun, g *Gang) int64 {
	if g.State.SystemID != 0 {
		if u, err := m.universe(ctx); err == nil {
			if r := u.meta[g.State.SystemID].RegionID; r != 0 {
				return r
			}
		}
	}
	return m.homeRegionFor(ctx, b)
}

// gangMiningSite returns the op's system, belt and ore, picking them on the first call:
// the standard belt search run for the first miner to plan, then one belt of that system
// for the cans to cluster on.
func (m *Mod) gangMiningSite(ctx context.Context, b *botRun, g *Gang, cfg miningConfig) (*GangState, error) {
	if g.State.SystemID != 0 && g.State.BeltItemID != 0 && g.State.OreTypeID != 0 {
		return &g.State, nil
	}
	if g.State.PinSystemID != 0 {
		return m.gangPinnedMiningSite(ctx, b, g)
	}
	target, note, err := m.findMiningTarget(ctx, b, cfg)
	if err != nil {
		return nil, fmt.Errorf("no belt for the gang op: %w", err)
	}
	if note != "" {
		m.logBot(b.CharID, logInfo, "gang", "%s", note)
	}
	// The op's ore is settled against the real rocks before any pilot is told where to fly: a
	// dozen miners sit on one belt for hours, and an ore the system never rolled wastes it.
	oreID, oreName := target.OreID, target.Ore
	if f, ferr := m.beltFieldFor(ctx, target.SysID); ferr == nil && f != nil {
		if avail := f.familyUnits(0); len(avail) > 0 {
			if avail[destiny.BaseOreType(oreID)] == 0 {
				best, bestUnits := int64(0), int64(0)
				for familyID, units := range avail {
					if oreFamilyBases[familyID] == "Mercoxit" {
						continue
					}
					if units > bestUnits {
						best, bestUnits = familyID, units
					}
				}
				if best != 0 {
					oreID, oreName = best, oreFamilyBases[best]
				}
			}
		}
	}
	beltID := m.beltWithMostOre(ctx, target.SysID, destiny.BaseOreType(oreID))
	if beltID == 0 {
		return nil, fmt.Errorf("op system %d has no belts after all", target.SysID)
	}
	g.State.SystemID = target.SysID
	g.State.SystemName = target.SysName
	g.State.BeltItemID = beltID
	g.State.OreTypeID = oreID
	g.State.OreName = oreName
	g.State.PickedAt = clock.Unix()
	if err := saveGangState(ctx, m.db, g.GangID, &g.State); err != nil {
		return nil, err
	}
	m.logBot(b.CharID, logInfo, "gang",
		"op site picked for gang %q — %s in %s (%d jumps out), cans on belt %d",
		g.Name, oreName, target.SysName, target.Jumps, beltID)
	return &g.State, nil
}

// gangPinnedMiningSite is the site when a Director pinned the op to a system
// (fleetmirror_orders.go): the ore they named if the belts hold it, else the richest
// family there, on the belt with most of it. No search.
func (m *Mod) gangPinnedMiningSite(ctx context.Context, b *botRun, g *Gang) (*GangState, error) {
	pin := g.State.PinSystemID
	oreID := g.State.OreTypeID
	f, ferr := m.beltFieldFor(ctx, pin)
	if ferr != nil || f == nil {
		return nil, fmt.Errorf("pinned system %d has no belts to read", pin)
	}
	avail := f.familyUnits(0)
	if oreID == 0 || avail[destiny.BaseOreType(oreID)] == 0 {
		best, bestUnits := int64(0), int64(0)
		for familyID, units := range avail {
			if oreFamilyBases[familyID] == "Mercoxit" {
				continue
			}
			if units > bestUnits {
				best, bestUnits = familyID, units
			}
		}
		if best == 0 {
			return nil, fmt.Errorf("pinned system %d is mined out", pin)
		}
		oreID = best
	}
	beltID := m.beltWithMostOre(ctx, pin, destiny.BaseOreType(oreID))
	if beltID == 0 {
		return nil, fmt.Errorf("pinned system %d has no belt with %s", pin, m.typeNameOr(ctx, oreID))
	}
	g.State.SystemID = pin
	g.State.SystemName = m.nameOfSystem(ctx, pin)
	g.State.BeltItemID = beltID
	g.State.OreTypeID = oreID
	g.State.OreName = m.typeNameOr(ctx, oreID)
	g.State.PickedAt = clock.Unix()
	if err := saveGangState(ctx, m.db, g.GangID, &g.State); err != nil {
		return nil, err
	}
	m.logBot(b.CharID, logInfo, "gang",
		"op site pinned by the chair for gang %q — %s in %s, cans on belt %d",
		g.Name, g.State.OreName, g.State.SystemName, beltID)
	return &g.State, nil
}

// gangSiteBound reports whether a gang phase is pointed at the op's site, which a moved
// site invalidates. Muster, form-up, wind-down and the haul legs are about the staging
// station or the hold.
func gangSiteBound(phase string) bool {
	switch TaskKind(phase) {
	case TaskGangMine, TaskGangPatrol, TaskGangHunt, TaskGangSecurity, TaskGangWait,
		TaskGangPirateProwl, TaskGangPirateCamp:
		return true
	}
	return false
}

// ── mining op: the miner ────────────────────────────────────────────────────

func (m *Mod) gangPlanMiner(ctx context.Context, b *botRun, g *Gang) error {
	// The duty-aware config, because the op's site is chosen here (gangMiningSite runs off
	// cfg.OrePriority); a belt picked off the generic hierarchy would be the wrong rock.
	cfg := m.gangMiningPlan(ctx, b, g)
	st := b.State
	st.Activity = ActMining

	ready, err := m.gangSeatReady(ctx, b, ActMining)
	if err != nil || !ready {
		return err
	}
	// Data.GangID must be set before the site search: effectivePower (threat.go) sums the
	// gang's guns only then, and a solo-power search refuses belts the escorted fleet can work.
	st.Data.GangID = g.GangID
	site, err := m.gangMiningSite(ctx, b, g, cfg)
	if err != nil {
		return err
	}
	st.Data = gangDutyData(st.Data, g)
	st.Data.GangBeltID = site.BeltItemID
	st.Data.TargetSystemID = site.SystemID
	st.Data.OreTypeID = site.OreTypeID
	st.Data.OreName = site.OreName
	m.logBot(b.CharID, logInfo, "gang", "joining the %q mining op — %s in %s",
		g.Name, site.OreName, site.SystemName)
	return m.beginTravel(ctx, b, site.SystemID, 0, "gang.mine")
}

// gangMineStep is the miner's whole life on the op: park on the belt, fill, jettison,
// repeat. It never sells and never leaves; the hauler does that.
func (m *Mod) gangMineStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	site := &g.State
	if site.SystemID == 0 || st.Data.OreTypeID == 0 {
		// Op was rebooted under this miner: re-plan.
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	if b.SystemID != site.SystemID {
		// The op moved (reboot picked a new site): follow it.
		return m.beginTravel(ctx, b, site.SystemID, 0, "gang.mine")
	}
	// A miner that followed a moved op still carries the old site in its leg. Adopt the
	// new belt and ore before anything reads them (gangMineSiteSync).
	gangMineSiteSync(&st.Data, site)
	if b.ShipID == 0 {
		return fmt.Errorf("gang miner has no ship boarded")
	}
	now := clock.Unix()
	// The op belt, for this gang's projections: a miner's next hop prefers it (beltbail.go).
	noteGangOpBelt(g.GangID, st.Data.GangBeltID)

	// Lasers started in the middle of an abstract fill: that fill already debited its rocks,
	// so its lots go into the can now, before the embodied branch below takes the leg.
	if m.embodiedMining(b.CharID) && st.Data.OreUnits > 0 {
		if err := m.gangSettleOwedOre(ctx, b, g, site); err != nil {
			return err
		}
	}

	// The op is winding down: can what is aboard and stand down so gangPlanWindDown sends
	// the miner to staging. A running abstract fill finishes first; its belt debit is taken.
	if g.State.OpPhase == gangOpWindDown && (st.Data.OreUnits == 0 || m.embodiedMining(b.CharID)) {
		return m.gangMinerStandDown(ctx, b, g)
	}

	// Sit on the op belt (also where the projection materialises). A live projection owns
	// its coordinates; do not move them.
	if !m.isEmbodied(b.CharID) {
		m.parkAtCelestial(ctx, b.ShipID, st.Data.GangBeltID)
	}

	// A projection with live lasers mines real rocks into the real hold; this tier tops the
	// can up when the hold gets heavy, so the beams never stall on a full hold for long.
	if m.embodiedMining(b.CharID) {
		free, err := m.holdFreeM3(ctx, b.CharID, b.ShipID)
		if err != nil {
			return err
		}
		oreVol, verr := evedb.GetTypeVolume(ctx, m.db, st.Data.OreTypeID)
		if verr != nil || oreVol <= 0 {
			oreVol = 0.1
		}
		if free <= oreVol*4 {
			// Everything the lasers cut, not just the family's base type: real cycles credit each
			// rock's own variant, so emptying by typeID alone leaves grades aboard and stalls the beams.
			lots, lerr := m.cargoOreLots(ctx, b, 0)
			if lerr != nil {
				return lerr
			}
			var moved int64
			for _, lot := range lots {
				n, err := m.jettisonToCan(ctx, g.GangID, b.CharID, b.ShipID,
					site.SystemID, st.Data.GangBeltID, lot.TypeID)
				if err != nil {
					return err
				}
				moved += n
			}
			if moved > 0 {
				m.logBot(b.CharID, logInfo, "gang",
					"hold full — jettisoned %d units of %s for the hauler", moved, st.Data.OreName)
				m.narrateMember(ctx, st.Data.GangID, b.CharID, "Can's full on the belt — hauler.")
			}
		}
		st.Data.OreUnits = 0
		st.Data.GangOffBeltSince = 0
		st.Data.LegEndsAt = now + realMiningPollSec
		return saveState(ctx, m.db, st)
	}

	// A projection off the op belt cannot fill a can there, since both doors jettison beside
	// the ship. Hold the leg: no new fill starts and a running one waits for the ship to return.
	// The hold is said once. A miner kept off for OpBeltAbandonSec means the belt is not
	// workable for this fleet, and the op gives the site up as it does a stripped one, so the
	// whole fleet moves instead of waiting at a station.
	if m.gangMinerOffBelt(b.CharID, st.Data.GangBeltID) {
		switch since := st.Data.GangOffBeltSince; {
		case since == 0:
			st.Data.GangOffBeltSince = now
			m.logBot(b.CharID, logInfo, "gang",
				"off the op belt — holding the can until the ship is back on it")
		case now-since >= OpBeltAbandonSec():
			m.logBot(b.CharID, logInfo, "gang",
				"kept off the op belt for %dm — the site needs moving", (now-since)/60)
			noteHostileEncounter(gangScope(g), site.SystemID)
			noteGangOpBelt(g.GangID, 0)
			if err := m.gangDropOpSite(ctx, g, st.Data.GangBeltID); err != nil {
				return err
			}
			st.Data.GangOffBeltSince = 0
			st.Activity, st.Phase = "", "idle"
			return saveState(ctx, m.db, st)
		}
		if st.Data.LegEndsAt < now+realMiningPollSec {
			st.Data.LegEndsAt = now + realMiningPollSec
		}
		return saveState(ctx, m.db, st)
	}
	st.Data.GangOffBeltSince = 0

	// Abstract tier: one hold-fill per timer, straight over the side.
	if st.Data.OreUnits == 0 {
		// Flush whatever real mining left aboard before starting a fresh fictional load. Every
		// grade, not just the base type: the fill is sized to an empty hold, and ore left aboard
		// would push part of the debited load past the free space.
		aboard, lerr := m.cargoOreLots(ctx, b, 0)
		if lerr != nil {
			return lerr
		}
		var flushed int64
		for _, lot := range aboard {
			n, err := m.jettisonToCan(ctx, g.GangID, b.CharID, b.ShipID,
				site.SystemID, st.Data.GangBeltID, lot.TypeID)
			if err != nil {
				return err
			}
			flushed += n
		}
		if flushed > 0 {
			m.logBot(b.CharID, logInfo, "gang", "jettisoned %d units of ore left in the hold", flushed)
		}
		units, fillSec, rate, err := m.miningYield(ctx, b, st.Data.OreTypeID)
		if err != nil {
			return err
		}
		if units <= 0 {
			// The backstop behind ensureActivityShip's shipCanMine gate: not a halt but a trip back
			// to plan, where the gate sends it shopping.
			m.logBot(b.CharID, logInfo, "gang",
				"mining ship yields nothing — no miners fitted; leaving the op to outfit")
			st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
			return saveState(ctx, m.db, st)
		}
		// The op belt is a real belt and a gang is what strips one: this leg comes out of its
		// rocks like any other (beltfield.go).
		lots, got, terr := m.beltTakeReal(ctx, site.SystemID, st.Data.GangBeltID,
			destiny.BaseOreType(st.Data.OreTypeID), units)
		if terr != nil {
			return terr
		}
		if got <= 0 {
			// The op has mined its belt out. Clearing the site sends the next planning pass through
			// gangMiningSite for a fresh one, and the whole fleet moves together.
			m.logBot(b.CharID, logInfo, "gang",
				"the op belt is stripped — the site needs moving")
			if err := m.gangDropOpSite(ctx, g, st.Data.GangBeltID); err != nil {
				return err
			}
			st.Activity, st.Phase = "", "idle"
			return saveState(ctx, m.db, st)
		}
		if got < units {
			fillSec = fillSec * got / units
			if fillSec < 1 {
				fillSec = 1
			}
			units = got
		}
		st.Data.OreLots = lots
		st.Data.OreUnits = units
		st.Data.YieldM3PerS = rate
		st.Data.LegEndsAt = now + fillSec
		m.logBot(b.CharID, logDebug, "gang", "mining %s for the can — hold full in %dm%ds",
			st.Data.OreName, fillSec/60, fillSec%60)
		return saveState(ctx, m.db, st)
	}

	// Timer elapsed: the load becomes real inside the can, as the variants beltTakeReal
	// returned, less what real cycles delivered. An empty list means there is nothing to can.
	lots := st.Data.OreLots
	if len(lots) == 0 {
		m.logBot(b.CharID, logInfo, "gang",
			"came away empty — nothing was taken from the op belt")
		st.Data.OreUnits, st.Data.OreLots = 0, nil
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	for _, lot := range lots {
		units := lot.Units
		if aboard, err := m.cargoUnitsOf(ctx, b, lot.TypeID); err == nil && aboard > 0 {
			if aboard >= units {
				units = 0
			} else {
				units -= aboard
			}
			if _, err := m.jettisonToCan(ctx, g.GangID, b.CharID, b.ShipID,
				site.SystemID, st.Data.GangBeltID, lot.TypeID); err != nil {
				return err
			}
		}
		if units > 0 {
			if err := m.conjureToCan(ctx, g.GangID, b.CharID, b.ShipID,
				site.SystemID, st.Data.GangBeltID, lot.TypeID, units); err != nil {
				return err
			}
		}
	}
	// A fleet miner's rock is mined rock (oreflow.go), booked here once: the two can-filling
	// paths together carry exactly this lot list, so booking in both would double it.
	m.noteOreFlowLots(ctx, oreStageMined, oreLotUnits(st.Data), 0)
	m.logBot(b.CharID, logInfo, "gang", "canned a load — %d units of %s on belt %d%s",
		st.Data.OreUnits, st.Data.OreName, st.Data.GangBeltID, m.oreLotNote(ctx, lots))
	st.Data.OreUnits, st.Data.OreLots = 0, nil
	return saveState(ctx, m.db, st)
}

// gangSettleOwedOre cans the lots of an abstract fill whose miner was embodied before
// the timer ran out. The rocks were debited when the fill started, so the whole lot
// list is conjured into the op can, booked once as mined, and the leg is cleared.
func (m *Mod) gangSettleOwedOre(ctx context.Context, b *botRun, g *Gang, site *GangState) error {
	st := b.State
	for _, lot := range st.Data.OreLots {
		if lot.Units <= 0 {
			continue
		}
		if err := m.conjureToCan(ctx, g.GangID, b.CharID, b.ShipID,
			site.SystemID, st.Data.GangBeltID, lot.TypeID, lot.Units); err != nil {
			return err
		}
	}
	if len(st.Data.OreLots) > 0 {
		m.noteOreFlowLots(ctx, oreStageMined, oreLotUnits(st.Data), 0)
		m.logBot(b.CharID, logInfo, "gang", "in space now — canned this fill's %d units of %s, lasers take over",
			st.Data.OreUnits, st.Data.OreName)
	}
	st.Data.OreUnits, st.Data.OreLots = 0, nil
	return saveState(ctx, m.db, st)
}

// gangMineSiteSync points a miner's leg at the op's current site. A miner that was mid-fill
// when the op moved follows it with the old belt and ore in its leg. Left alone, the
// load it brings is canned on a belt record from another system, where no hauler finds
// it. The next fill then draws on a belt this system lacks, comes back empty and wipes
// the new site as stripped. A load in flight keeps its lots and is canned at the new
// belt. Reports whether anything changed. Pure.
func gangMineSiteSync(d *StateData, site *GangState) bool {
	if site.BeltItemID == 0 || d.GangBeltID == site.BeltItemID {
		return false
	}
	d.GangBeltID = site.BeltItemID
	d.TargetSystemID = site.SystemID
	d.OreTypeID = site.OreTypeID
	d.OreName = site.OreName
	return true
}

// gangDropOpSite clears the op's site so the next planning pass picks a fresh one, but only
// while the row still names beltID. A member that lags the fleet by one move must not wipe
// the site the others have just picked. Reads the row fresh (writeHuntState).
func (m *Mod) gangDropOpSite(ctx context.Context, g *Gang, beltID int64) error {
	return m.writeHuntState(ctx, g.GangID, &g.State, func(s *GangState) {
		if s.BeltItemID == beltID {
			s.BeltItemID, s.SystemID, s.OreTypeID = 0, 0, 0
		}
	})
}

// gangCanBeltReach is how far from the op belt's centre a projection still counts as on
// it: rocks spawn inside mineScanRange of the centre and a miner orbits them, short of
// any other celestial's grid.
const gangCanBeltReach = 300_000.0

// gangMinerOffBelt reports whether this miner has a live projection somewhere other than
// the op belt: breaking off (a latched flee), in warp, or too far from the belt. A pilot
// with no projection is never off it, and neither is one with lasers hot: its mining
// loop hops belts itself and its hold came out of the rocks under it.
func (m *Mod) gangMinerOffBelt(charID, beltID int64) bool {
	eb := liveProjection(charID)
	if eb == nil || beltID == 0 {
		return false
	}
	eb.mu.Lock()
	fleeing, warping, lasers := eb.fleeing, eb.warping, eb.mineCancel != nil
	eb.mu.Unlock()
	if lasers {
		return false
	}
	if fleeing || warping {
		return true
	}
	belt := celestialByID(eb.celestials, beltID)
	if belt == nil || eb.ent == nil {
		// Not this system's belt (the op moved under it) or no ball yet: nowhere a can could go.
		return true
	}
	center := destiny.Vec3{X: belt.X, Y: belt.Y, Z: belt.Z}
	return eb.ent.Pos().Sub(center).Len() > gangCanBeltReach
}

// conjureToCan mints an unwatched fill directly into the miner's can: the gang twin of
// the solo timer's addCargoStack.
func (m *Mod) conjureToCan(ctx context.Context, gangID, charID, shipID, systemID, beltItemID, oreTypeID, units int64) error {
	oreVol, err := evedb.GetTypeVolume(ctx, m.db, oreTypeID)
	if err != nil || oreVol <= 0 {
		oreVol = 0.1
	}
	// The can stage covers what reaches a can by either door (this one and jettisonToCan,
	// which books its own). A load is split between them, so each books its own share.
	m.noteOreFlow(ctx, oreTypeID, oreStageJetcan, units, 0)
	for units > 0 {
		canID, room, err := m.openCanFor(ctx, gangID, charID, shipID, systemID, beltItemID)
		if err != nil {
			return err
		}
		take := int64(room / oreVol)
		if take <= 0 {
			return fmt.Errorf("can %d reports no room straight after opening", canID)
		}
		if take > units {
			take = units
		}
		if err := evedb.CreateNPCLootItem(ctx, m.db, canID, charID, oreTypeID, take); err != nil {
			return fmt.Errorf("conjure into can: %w", err)
		}
		if err := m.mergeCanStacks(ctx, canID, oreTypeID); err != nil {
			return err
		}
		units -= take
	}
	return nil
}

// mergeCanStacks folds every stack of one type inside a can into a single row, since
// repeated jettisons would otherwise litter the can with stacks.
func (m *Mod) mergeCanStacks(ctx context.Context, canID, typeID int64) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE locationID = ? AND typeID = ? AND singleton = 0
		ORDER BY itemID`, canID, typeID)
	if err != nil {
		return fmt.Errorf("mergeCanStacks: %w", err)
	}
	var ids []int64
	var total int64
	for rows.Next() {
		var id, qty int64
		if err := rows.Scan(&id, &qty); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		total += qty
	}
	rows.Close()
	if len(ids) < 2 {
		return nil
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET quantity = ? WHERE itemID = ?`, total, ids[0]); err != nil {
		return err
	}
	for _, id := range ids[1:] {
		if _, err := m.db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ── mining op: the hauler ───────────────────────────────────────────────────

func (m *Mod) gangPlanHauler(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	st.Activity = ActTrade // haulers fly their trade (cargo) fit

	ready, err := m.gangSeatReady(ctx, b, ActTrade)
	if err != nil || !ready {
		if err != nil {
			return fmt.Errorf("hauler needs a trade fit for its cargo ship: %w", err)
		}
		return nil
	}
	site := &g.State
	if site.SystemID == 0 {
		return m.gangWait(ctx, b, "no op site yet — waiting for a miner to pick the belt")
	}
	// The hauler needs cover: a patrol on station, a recently cleared system, an empty belt
	// or the op's miners on the belt; else it waits. See docs/comment-notes-b10.md#hauler-cover.
	if !m.gangPatrolCovers(ctx, g.GangID, site.SystemID) && !patrolCovered(gangScope(g), site.SystemID) &&
		!m.gangPhaseInSystem(ctx, g.GangID, site.SystemID, TaskGangMine) &&
		m.systemHasLiveRats(ctx, site.SystemID) {
		return m.gangWait(ctx, b,
			fmt.Sprintf("holding at the station — %s has no patrol on it yet", site.SystemName))
	}

	st.Data = gangDutyData(st.Data, g)
	st.Data.GangBeltID = site.BeltItemID
	m.logBot(b.CharID, logInfo, "gang", "hauling for the %q op — collecting cans in %s",
		g.Name, site.SystemName)
	return m.beginTravel(ctx, b, site.SystemID, 0, "gang.haul.load")
}

// gangHaulLoadStep empties the op's cans into the hold, fullest first.
func (m *Mod) gangHaulLoadStep(ctx context.Context, b *botRun, g *Gang) (bool, error) {
	st := b.State
	site := &g.State
	if site.SystemID == 0 {
		st.Activity, st.Phase = "", "idle"
		return false, saveState(ctx, m.db, st)
	}
	// The cans are worked where they are: the op's site or, when the hauler was sent after
	// them, a system the op left rock behind in (gangStrayCanSystem). Anywhere else, back.
	loadSys, beltID := site.SystemID, site.BeltItemID
	if b.SystemID != site.SystemID {
		if m.gangCanM3At(ctx, g.GangID, b.SystemID) < 1 {
			return false, m.beginTravel(ctx, b, site.SystemID, 0, "gang.haul.load")
		}
		loadSys, beltID = b.SystemID, 0
	}
	if b.ShipID == 0 {
		return false, fmt.Errorf("gang hauler has no ship boarded")
	}

	free, err := m.holdFreeM3(ctx, b.CharID, b.ShipID)
	if err != nil {
		return false, err
	}
	cans, err := m.gangCansAt(ctx, g.GangID, loadSys)
	if err != nil {
		return false, err
	}
	if beltID == 0 {
		for _, c := range cans {
			if c.BeltItemID != 0 {
				beltID = c.BeltItemID
				break
			}
		}
	}
	m.parkAtCelestial(ctx, b.ShipID, beltID)

	loaded := 0.0
	for _, c := range cans {
		if c.UsedM3 <= 0 || free-loaded < 1 {
			continue
		}
		took, perType, err := m.takeCanContents(ctx, b.CharID, b.ShipID, c.CanID, loadSys, free-loaded)
		if err != nil {
			return false, err
		}
		loaded += took
		// The can is one miner's work (openCanFor keys by owner), so its contents are that miner's
		// share (fleetore.go, gangindustry.go). A player member's jetsam carries its owner too.
		if fleetAttributesOre(g.Activity) && c.OwnerID != 0 {
			m.recordFleetOre(ctx, g.GangID, b.CharID, c.OwnerID, perType)
		}
	}

	held, err := m.holdUsedM3(ctx, b.CharID, b.ShipID)
	if err != nil {
		return false, err
	}
	if held > 0 {
		// Anything aboard, from this pass or an earlier partial, runs in. The cans left behind
		// refill while the hauler makes the round trip.
		if loaded > 0 {
			m.logBot(b.CharID, logInfo, "gang", "loaded %.0f m³ from the cans — running it in", loaded)
			m.narrateMember(ctx, g.GangID, b.CharID, "Loaded %.0f m³ — running it in.", loaded)
		}
		return false, m.gangHaulDispatch(ctx, b, g)
	}
	// Cans and hold are dry here. Rock the op left in another system is the next run, live or
	// winding down; it rolls only while the gang lives and expires the moment it does not.
	if stray, m3 := m.gangStrayCanSystem(ctx, g.GangID, site.SystemID); stray != 0 && stray != b.SystemID {
		m.logBot(b.CharID, logInfo, "gang",
			"%.0f m³ of the op's ore is still in cans the fleet left behind — going back for it", m3)
		st.Phase = "gang.haul.load"
		return false, m.beginTravel(ctx, b, stray, 0, "gang.haul.load")
	}
	// Cans and hold are dry. On a wind-down that is the last run done: go idle, and
	// gangPlanWindDown sends the hauler to staging (or back here if a miner canned more).
	if g.State.OpPhase == gangOpWindDown {
		m.logBot(b.CharID, logInfo, "gang", "the cans are dry and the op is winding down — heading in")
		st.Activity, st.Phase = "", "idle"
		return false, saveState(ctx, m.db, st)
	}
	// Otherwise sit on the belt and check again.
	st.Phase = "gang.haul.load"
	st.Data.LegEndsAt = clock.Unix() + gangHaulPollSec
	return false, saveState(ctx, m.db, st)
}

// gangHaulDispatch routes a loaded hauler to the drop-off the mining config asks for: the
// corp hangar a player corporation pointed its fleets at, a named stockpile hangar, or the
// best reachable buyer for the op's ore.
func (m *Mod) gangHaulDispatch(ctx context.Context, b *botRun, g *Gang) error {
	cfg := m.gangMiningPlan(ctx, b, g)
	r := rulesForBot(b)
	prefer := m.homeRegionSet(ctx, b)

	// A mining fleet delivers to its dropoff (fleetOreDropoff); a bot-run corporation's op
	// goes first (corpfleetore.go). See docs/comment-notes-b10.md#haul-dispatch-order.
	if handled, err := m.corpOreRunDispatch(ctx, b, g); handled || err != nil {
		return err
	}
	// A co-op fleet with a corporation refinery near the op drops its load there: each miner's
	// share is refined under its own name at 75% and waits at the tower (posrefinery.go).
	if a, ok := m.gangRefineryTarget(ctx, b, g); ok {
		b.State.Data.PosRefineArray = a.ArrayID
		return m.beginTravel(ctx, b, a.SystemID, 0, "gang.haul.drop")
	}
	if _, stationID, sysID := m.fleetOreDropoff(ctx, g); stationID != 0 {
		return m.beginTravel(ctx, b, sysID, stationID, "gang.haul.drop")
	}

	// A corp fleet whose corporation set a delivery station runs the haul there: the corp
	// hangar is the drop, not the market. Reached by an industry fleet with no factory.
	if _, stationID, officeID := m.fleetOreTarget(ctx, g); officeID != 0 {
		loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
		if err != nil {
			return err
		}
		return m.beginTravel(ctx, b, loc.SolarSystemID, stationID, "gang.haul.drop")
	}

	// Refine: the op's ore comes back as minerals on its own region's book, after the delivery
	// targets and before the sell branch it replaces. The hauler is the member that docks.
	if cfg.Disposal == "refine" {
		stationID, sysID, err := m.gangRefineStation(ctx, b, g)
		if err != nil {
			return err
		}
		if stationID != 0 {
			m.logBot(b.CharID, logInfo, "gang",
				"running the op's %s in to %s to refine — the op is set to refine its haul",
				g.State.OreName, m.stationLabel(ctx, stationID))
			return m.beginTravel(ctx, b, sysID, stationID, "gang.haul.drop")
		}
		// Nowhere in reach will reprocess for this pilot. Fall through and sell the rock.
		m.logBot(b.CharID, logInfo, "gang",
			"no refinery in reach it will use — selling the op's %s as rock instead", g.State.OreName)
		cfg.Disposal = "sell"
	}

	if cfg.Disposal == "stockpile" {
		stationID, sysID := cfg.StockpileStationID, int64(0)
		if stationID == 0 {
			var err error
			stationID, sysID, err = m.nearestAllowedStation(ctx, b, m.gangHomeRules(ctx, b, g), prefer)
			if err != nil {
				return err
			}
		}
		if sysID == 0 {
			loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
			if err != nil {
				return err
			}
			sysID = loc.SolarSystemID
		}
		return m.beginTravel(ctx, b, sysID, stationID, "gang.haul.drop")
	}

	// Sell: best bid for the op ore in the hauler's own economic scope,
	// haul distance priced in, widening only if home has no buyer.
	var sellTo *marketStop
	if _, err := m.shopLadder(ctx, b, []int64{g.State.OreTypeID}, false, prefer,
		defaultBand(b), "", func(scan marketScan) bool {
			sellTo = bestSell(scan.Stops, b.Tune)
			return sellTo != nil
		}); err != nil {
		return err
	}
	if sellTo == nil {
		m.logBot(b.CharID, logInfo, "gang",
			"nobody reachable bids on %s — stockpiling the load instead", g.State.OreName)
		stationID, sysID, err := m.nearestAllowedStation(ctx, b, r, prefer)
		if err != nil {
			return err
		}
		return m.beginTravel(ctx, b, sysID, stationID, "gang.haul.drop")
	}
	m.logBot(b.CharID, logInfo, "gang", "selling the gang's %s at %.2f ISK/unit at %s",
		g.State.OreName, sellTo.Price, m.stationLabel(ctx, sellTo.StationID))
	return m.beginTravel(ctx, b, sellTo.SystemID, sellTo.StationID, "gang.haul.drop")
}

// gangHomeRules scopes a haul to the op's own region. rulesForBot carries no residency
// band and the sell branch's shopLadder passes the hauler's band (bandAdjacent for a
// commerce-appetite pilot), so ore leaves the region. That suits selling rock to the best
// bid but not a load refined to stock the region the fleet stands in.
func (m *Mod) gangHomeRules(ctx context.Context, b *botRun, g *Gang) travelRules {
	r := rulesForBot(b)
	region := m.gangHomeRegion(ctx, b, g)
	if region == 0 {
		return r
	}
	if scope := m.scopeRegions(ctx, region, bandHome); len(scope) > 0 {
		r.Regions = scope
	}
	return r
}

// gangRefineStation is where a stocking op's hauler takes its load: a station in the op's
// own region that will reprocess for this pilot. stationID 0 with a nil error means none
// is in reach, and the caller sells the rock instead of stranding a full hauler.
func (m *Mod) gangRefineStation(ctx context.Context, b *botRun, g *Gang) (int64, int64, error) {
	r := m.gangHomeRules(ctx, b, g)
	stationID, sysID, err := m.nearestAllowedStation(ctx, b, r, m.homeRegionSet(ctx, b))
	if err != nil || stationID == 0 {
		return 0, 0, nil // not an error: no dock here it will use
	}
	if sysID == 0 {
		loc, lerr := evedb.GetStationLocation(ctx, m.db, stationID)
		if lerr != nil {
			return 0, 0, lerr
		}
		sysID = loc.SolarSystemID
	}
	return stationID, sysID, nil
}

// gangHaulDropStep unloads at the station: ore with a live local bid is
// sold through the real matching engine, everything else goes to the
// hangar. Then straight back to the belt.
func (m *Mod) gangHaulDropStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	if st.Data.PosRefineArray > 0 {
		return m.gangRefineryDrop(ctx, b, g) // the load goes to a corporation refinery (posrefinery.go)
	}
	if b.StationID == 0 {
		return fmt.Errorf("gang.haul.drop while not docked")
	}
	cfg := m.gangMiningPlan(ctx, b, g)

	// Everything in the hold came out of the cans (any ore family). Read before any move
	// below: the leader's delivery branch must write down what it took, and only the hold shows it.
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, COALESCE(t.typeName, ''), SUM(e.quantity) FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups gr ON gr.groupID = t.groupID
		WHERE e.locationID = ? AND e.ownerID = ? AND e.flag = 5 AND gr.categoryID = 25
		GROUP BY e.typeID, t.typeName`,
		b.ShipID, b.CharID)
	if err != nil {
		return err
	}
	type oreLoad struct {
		typeID   int64
		typeName string
		units    int64
	}
	var load []oreLoad
	var types []int64
	for rows.Next() {
		var o oreLoad
		if rows.Scan(&o.typeID, &o.typeName, &o.units) == nil {
			load = append(load, o)
			types = append(types, o.typeID)
		}
	}
	rows.Close()

	// Docked at a bot-run corporation's office on a corp run: the members' shares become one
	// corp stack in the office hangar, credited as labour; a non-member's is handed back.
	if corpID, stationID, officeID, ok := m.corpOreRun(ctx, g); ok && stationID == b.StationID {
		lots := make([]oreHoldRow, 0, len(load))
		for _, o := range load {
			lots = append(lots, oreHoldRow{TypeID: o.typeID, TypeName: o.typeName, Units: o.units})
		}
		toCorp, handed, err := m.corpOreDrop(ctx, b, g, corpID, officeID, lots, m.corpOreUnitValues(ctx, stationID))
		if err != nil {
			return err
		}
		m.logBot(b.CharID, logInfo, "gang",
			"ran the load in to the corporation's office at %s — %d units into the corp hangar, %d non-member share(s) handed back — heading back to the belt",
			m.stationLabel(ctx, b.StationID), toCorp, handed)
		st.Activity, st.Phase = "", "idle"
		st.Data = gangDutyData(st.Data, g)
		return saveState(ctx, m.db, st)
	}

	// Docked at a co-op's dropoff: each miner's share lands in its own hangar with a
	// rolebot_ore_hold note (handBackFleetOre). Nothing is bought or sold here.
	if _, stationID, _ := m.fleetOreDropoff(ctx, g); fleetSharesOre(g.Activity) && stationID == b.StationID {
		lots := make([]oreHoldRow, 0, len(load))
		for _, o := range load {
			lots = append(lots, oreHoldRow{TypeID: o.typeID, TypeName: o.typeName, Units: o.units})
		}
		handed, err := m.handBackFleetOre(ctx, b, g, lots)
		if err != nil {
			return err
		}
		m.logBot(b.CharID, logInfo, "gang",
			"ran the load in to %s and handed it back to %d miner(s) — heading back to the belt",
			m.stationLabel(ctx, b.StationID), handed)
		st.Activity, st.Phase = "", "idle"
		st.Data = gangDutyData(st.Data, g)
		return saveState(ctx, m.db, st)
	}

	// Docked at an industry fleet's factory: the load becomes the leader's property and the
	// miners are paid from its wallet (payFleetOre). rolebot_ore_hold marks the fleet's ore.
	if leaderID, stationID, _ := m.fleetOreDropoff(ctx, g); fleetPaysForOre(g.Activity) &&
		leaderID != 0 && stationID == b.StationID {
		for _, o := range load {
			if err := m.moveCargoToLeaderHangar(ctx, b, leaderID, o.typeID); err != nil {
				return err
			}
			m.noteFleetOreDelivered(ctx, leaderID, b.StationID, o.typeID, o.typeName, o.units)
		}
		m.stackHangar(ctx, leaderID, b.StationID)
		m.payFleetOre(ctx, g)
		m.logBot(b.CharID, logInfo, "gang",
			"delivered the load into the commander's factory hangar at %s — heading back to the belt",
			m.stationLabel(ctx, b.StationID))
		st.Activity, st.Phase = "", "idle"
		st.Data = gangDutyData(st.Data, g)
		return saveState(ctx, m.db, st)
	}

	// Docked at the corporation's delivery station: everything goes into the corp hangar and
	// nothing is sold. Anywhere else the load lands in the pilot's own hangar.
	if corpID, stationID, officeID := m.fleetOreTarget(ctx, g); officeID != 0 && stationID == b.StationID {
		for _, typeID := range types {
			if err := m.moveCargoToCorpHangar(ctx, b, corpID, officeID, typeID); err != nil {
				return err
			}
		}
		m.logBot(b.CharID, logInfo, "gang",
			"delivered the load into the corp hangar at %s — heading back to the belt",
			m.stationLabel(ctx, b.StationID))
		st.Activity, st.Phase = "", "idle"
		st.Data = gangDutyData(st.Data, g)
		return saveState(ctx, m.db, st)
	}

	// Refine: every ore family the cans held goes to the hangar and comes back as minerals
	// on this station's book. reserved is nil: a hauler has no project to hold anything for.
	if cfg.Disposal == "refine" {
		for _, typeID := range types {
			if err := m.moveCargoToHangar(ctx, b, typeID); err != nil {
				return err
			}
		}
		unitsIn, got, rerr := m.refineHangarOre(ctx, b)
		if rerr != nil {
			return rerr
		}
		if unitsIn == 0 {
			m.logBot(b.CharID, logInfo, "gang",
				"nothing in the load makes a whole portion yet — it waits in the hangar for the next run")
		} else {
			listed, _, held, lerr := m.listRefinedMinerals(ctx, b, got, nil, mineralHoldMaxSec)
			if lerr != nil {
				return lerr
			}
			m.logBot(b.CharID, logInfo, "gang",
				"refined %d unit(s) of the op's ore at %s and listed %d mineral line(s)%s — heading back to the belt",
				unitsIn, m.stationLabel(ctx, b.StationID), listed,
				map[bool]string{true: fmt.Sprintf(", %d stack(s) still short of a lot", held)}[held > 0])
		}
		st.Activity, st.Phase = "", "idle"
		st.Data = gangDutyData(st.Data, g)
		return saveState(ctx, m.db, st)
	}

	for _, typeID := range types {
		if err := m.moveCargoToHangar(ctx, b, typeID); err != nil {
			return err
		}
		if cfg.Disposal == "stockpile" {
			continue
		}
		if err := m.gangSellFromHangar(ctx, b, typeID); err != nil {
			m.logBot(b.CharID, logError, "gang", "selling type %d: %v — left in the hangar", typeID, err)
		}
	}
	m.logBot(b.CharID, logInfo, "gang", "dropped the load at %s — heading back to the belt", m.stationLabel(ctx, b.StationID))
	st.Activity, st.Phase = "", "idle"
	st.Data = gangDutyData(st.Data, g)
	return saveState(ctx, m.db, st)
}

// fleetOreTarget resolves a corp fleet's delivery station: the gang must belong to a
// corporation that has a drop-off and still holds an office there. officeID 0 means no
// target, and the haul takes the ordinary sell path. See
// docs/comment-notes-b10.md#fleet-ore-target.
func (m *Mod) fleetOreTarget(ctx context.Context, g *Gang) (corpID, stationID, officeID int64) {
	return m.corpDepositTarget(ctx, g.CorpID)
}

// corpDepositTarget is where a member puts something into its corporation's hands: the
// drop-off station (fleetOreStationID) or the headquarters, and the office it rents there.
// Zeros when there is no real office, as for every NPC corporation (GetCorpOfficeAt
// answers officeID == stationID).
func (m *Mod) corpDepositTarget(ctx context.Context, corp int64) (corpID, stationID, officeID int64) {
	if corp == 0 {
		return 0, 0, 0
	}
	// Per corporation, shared for five minutes: three reads, asked on every
	// loot decision and every ore drop of every member.
	type target struct{ corp, station, office int64 }
	t, _ := snapshotFor(snapKey("deposit-target", corp), 5*time.Minute, func() (target, error) {
		c, s, o := m.readCorpDepositTarget(ctx, corp)
		return target{c, s, o}, nil
	})
	return t.corp, t.station, t.office
}

func (m *Mod) readCorpDepositTarget(ctx context.Context, corp int64) (corpID, stationID, officeID int64) {
	// An NPC corporation's owner offices (npcchair_offices.go) have no desk
	// that sells or uses what lands in them: ore or loot given there is lost.
	if npc, err := isNPCCorp(ctx, m.db, corp); err != nil || npc {
		return 0, 0, 0
	}
	stationID = m.corpFleetOreStation(ctx, corp)
	if stationID == 0 {
		stationID = m.corpHQStation(ctx, corp)
	}
	if stationID == 0 {
		return 0, 0, 0
	}
	officeID, _, err := evedb.GetCorpOfficeAt(ctx, m.db, corp, stationID)
	if err != nil || officeID == 0 || officeID == stationID {
		return 0, 0, 0
	}
	return corp, stationID, officeID
}

// corpHQStation is corporation.stationID, the headquarters, at most one per corporation.
func (m *Mod) corpHQStation(ctx context.Context, corpID int64) int64 {
	var id int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(stationID, 0) FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&id)
	return id
}

// moveCargoToLeaderHangar is the industry fleet's delivery: every cargo stack of typeID
// lands in the leader's personal hangar at this station and becomes its property. Personal,
// never corp: industryShortfall reads the pilot's own flag-4 hangar.
func (m *Mod) moveCargoToLeaderHangar(ctx context.Context, b *botRun, leaderID, typeID int64) error {
	if b.StationID == 0 {
		return fmt.Errorf("not docked")
	}
	if b.ShipID == 0 {
		return nil
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 4, ownerID = ?
		WHERE locationID = ? AND ownerID = ? AND typeID = ? AND flag = ?`,
		b.StationID, leaderID,
		b.ShipID, b.CharID, typeID, flagCargo); err != nil {
		return err
	}
	return nil
}

// moveCargoToCorpHangar is moveCargoToHangar's corp-delivery twin: every cargo stack of
// typeID lands in the corporation's office hangar (division 1) and becomes corp property,
// which is what makes it a delivery rather than storage (as the client's own deposit path
// does, officewrites.go). Stacks fold into what the corp already holds there.
func (m *Mod) moveCargoToCorpHangar(ctx context.Context, b *botRun, corpID, officeID, typeID int64) error {
	if b.StationID == 0 {
		return fmt.Errorf("not docked")
	}
	if b.ShipID == 0 {
		return nil
	}
	// What is about to move, read before the move and out of the cargo hold: reading the
	// hangar afterwards would count the pile already there and re-book the corp's whole stack.
	moved := m.cargoOreUnitsOf(ctx, b, typeID)
	if _, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = ?, ownerID = ?
		WHERE locationID = ? AND ownerID = ? AND typeID = ? AND flag = ?`,
		officeID, flagHangar, corpID,
		b.ShipID, b.CharID, typeID, flagCargo); err != nil {
		return err
	}
	m.stackContainer(ctx, corpID, officeID, flagHangar)
	// A corp hangar deposit is booked as stockpiled, not as a fleet disposal: the ore has not
	// been sold, refined or listed, it has been put somewhere (oreflow.go).
	if moved > 0 {
		m.noteOreFlow(ctx, typeID, oreStageStockpiled, moved, 0)
	}
	return nil
}

// cargoOreUnitsOf is how much ore of one type is in this pilot's hold. It returns 0 for
// anything that is not ore, because a corp delivery carries every category and booking
// ammunition onto the ore flow would be wrong; that category test is the difference from
// cargoUnitsOf (mining.go).
func (m *Mod) cargoOreUnitsOf(ctx context.Context, b *botRun, typeID int64) int64 {
	if b.ShipID == 0 {
		return 0
	}
	var units int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(e.quantity), 0) FROM entity e
		JOIN invTypes t  ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.typeID = ? AND e.flag = ?
		  AND g.categoryID = 25`,
		b.CharID, b.ShipID, typeID, flagCargo).Scan(&units); err != nil {
		return 0
	}
	return units
}

// gangSellFromHangar sells one hangar stack into local bids, leaving it in the hangar when
// nobody bids (a later run may clear it). A mineral sells only to a player bid here
// (bidDump.PlayerOnly): an unfiltered sweep handed the refine hub's minerals to the NPC
// seeder's standing bid. Ore is unfiltered.
func (m *Mod) gangSellFromHangar(ctx context.Context, b *botRun, typeID int64) error {
	var itemID, qty int64
	err := m.db.QueryRowContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
		ORDER BY quantity DESC LIMIT 1`,
		b.CharID, b.StationID, typeID).Scan(&itemID, &qty)
	if err != nil {
		return nil // nothing to sell
	}
	q := `
		SELECT MIN(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`
	args := []any{b.StationID, typeID}
	if _, isMineral := mineralListFloors[typeID]; isMineral {
		q += ` AND charID NOT IN (?, ?)`
		args = append(args, evedb.SeederCharID, evedb.AdminSeederCharID)
	}
	var floor float64
	if err := m.db.QueryRowContext(ctx, q, args...).Scan(&floor); err != nil || floor <= 0 {
		return nil // no bid here: stays stockpiled
	}
	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
		typeID, itemID, floor, qty, -1, 1, 0, false)
	if err != nil {
		return err
	}
	if out.ErrorMsg == "" && out.FilledQty > 0 {
		var gross float64
		for _, f := range out.Fills {
			gross += f.Price * float64(f.Quantity)
		}
		m.notePeerFills(ctx, tradeEntry{
			CharID: b.CharID, TypeID: typeID, TypeName: m.typeNameOr(ctx, typeID),
			Station: b.StationID, System: b.SystemID,
		}, out.Fills, false, "gang haul")
		m.logBot(b.CharID, logInfo, "market", "gang haul: sold %d units for %.2f ISK at %s",
			out.FilledQty, gross, m.stationLabel(ctx, b.StationID))
		// A fleet's rock reaching a market on the road is a disposal of the pooled haul and is
		// booked as the fleet stage. Minerals are excluded: a refined mineral is no longer ore.
		if _, isMineral := mineralListFloors[typeID]; !isMineral {
			m.noteOreFlow(ctx, typeID, oreStageFleet, out.FilledQty, gross)
		}
	}
	return nil
}

// holdUsedM3 is the cargo volume currently aboard.
func (m *Mod) holdUsedM3(ctx context.Context, charID, shipID int64) (float64, error) {
	return evedb.GetCargoUsedVolume(ctx, m.db, charID, shipID)
}

// ── mining op: the patrol ───────────────────────────────────────────────────

func (m *Mod) gangPlanPatrol(ctx context.Context, b *botRun, g *Gang, seat string) error {
	st := b.State
	st.Activity = ActCombat // patrols fly their bounty-hunting fit

	ready, err := m.gangSeatReady(ctx, b, seat)
	if err != nil || !ready {
		if err != nil {
			return fmt.Errorf("patrol needs a bounty fit for its combat ship: %w", err)
		}
		return nil
	}
	site := &g.State
	if site.SystemID == 0 {
		return m.gangWait(ctx, b, "no op site yet — waiting for a miner to pick the belt")
	}
	st.Data = gangDutyData(st.Data, g)
	st.Data.GangBeltID = site.BeltItemID
	m.logBot(b.CharID, logInfo, "gang", "flying patrol for the %q op in %s",
		g.Name, site.SystemName)
	return m.beginTravel(ctx, b, site.SystemID, 0, "gang.patrol")
}

// gangPatrolStep bounces the patrol between the op system's belts. In a
// watched system the projection wanders belts on its own and its guard
// engages every rat it meets; out here the accelerated encounter roll in
// combat_abstract.go does the fighting.
func (m *Mod) gangPatrolStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	site := &g.State
	if site.SystemID == 0 {
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	if g.State.OpPhase == gangOpWindDown {
		st.Activity, st.Phase = "", "idle" // gangPlanWindDown takes it from here
		return saveState(ctx, m.db, st)
	}
	if b.SystemID != site.SystemID {
		return m.beginTravel(ctx, b, site.SystemID, 0, "gang.patrol")
	}
	if b.ShipID == 0 {
		return fmt.Errorf("gang patrol has no ship boarded")
	}
	m.hopPatrolBelt(ctx, b)
	st.Data.LegEndsAt = clock.Unix() +
		int64(gangPatrolDwellMinSec+rand.Intn(gangPatrolDwellMaxSec-gangPatrolDwellMinSec+1))
	return saveState(ctx, m.db, st)
}

// hopPatrolBelt moves the abstract ship to a belt: the one with live rats on it when the
// census knows of any, else a random one. Embodied patrols ignore this; the persisted
// position still follows so a fresh projection appears on a belt.
func (m *Mod) hopPatrolBelt(ctx context.Context, b *botRun) {
	if m.isEmbodied(b.CharID) {
		return
	}
	// A rat hunter goes where it would fight: the belt the encounter roll draws at
	// (beltForBot). No belt means the system is dry for this hull and the sweep moves on.
	if b.State != nil && postureFor(b.State.Phase) == PostureCombatBelt {
		if beltID, _, err := m.beltForBot(ctx, b); err == nil && beltID != 0 {
			m.parkAtCelestial(ctx, b.ShipID, beltID)
		}
		return
	}
	// Where the rats are: npcBeltRats is the belt's contents whether or not a client has the
	// system loaded; npcLiveEntities holds only instantiated balls, none for an unwatched system.
	var beltID int64
	err := m.db.QueryRowContext(ctx, `
		SELECT celestialID FROM npcBeltRats
		WHERE solarSystemID = ? AND alive = 1
		GROUP BY celestialID ORDER BY COUNT(*) DESC LIMIT 1`, b.SystemID).Scan(&beltID)
	if err != nil || beltID == 0 {
		if err := m.db.QueryRowContext(ctx, `
			SELECT itemID FROM mapDenormalize WHERE solarSystemID = ? AND groupID = 9
			ORDER BY RAND() LIMIT 1`, b.SystemID).Scan(&beltID); err != nil {
			return
		}
	}
	m.parkAtCelestial(ctx, b.ShipID, beltID)
}

// ── the gang combat patrol ────────────────────────────────────────────────────

func (m *Mod) gangPlanHunt(ctx context.Context, b *botRun, g *Gang, seat string) error {
	cfg := combatConfigFor(g.Config)
	st := b.State
	st.Activity = ActCombat

	ready, err := m.gangSeatReady(ctx, b, seat)
	if err != nil || !ready {
		return err
	}
	// A siege its orders hold comes before any hunting ground (possiege.go).
	if post := m.gangSiegePost(ctx, g); post.TowerID != 0 {
		return m.gangPlanSiege(ctx, b, g, post, "gang.hunt")
	}
	site, err := m.gangHuntSite(ctx, b, g, cfg)
	if err != nil {
		// Nothing winnable in reach: the pack waits out a window, then stands down (fleetQuiet).
		// Until then the error stands, and plan() retries it as a world refusal.
		if errors.Is(err, errNoHuntingGround) {
			if stood, qerr := m.fleetQuiet(ctx, b, g); qerr != nil || stood {
				return qerr
			}
		}
		return err
	}
	m.fleetHasWork(ctx, g)
	st.Data = gangDutyData(st.Data, g)
	m.logBot(b.CharID, logInfo, "gang", "hunting with gang %q — sweeping %s",
		g.Name, site.HuntSystemName)
	return m.beginTravel(ctx, b, site.HuntSystemID, 0, "gang.hunt")
}

// fleetQuiet is called when a combat or security fleet finds nothing to do on its ground.
// The first call starts a clock on the gang row; once FleetQuietSec has passed with no site
// picked, the fleet disbands and the calling pilot goes back to its own schedule, instead
// of the whole roster idling until the op expires hours later. A fleet raised by a chair's
// order or answering a help call is left alone, as is an operator-built gang. It reports
// whether the fleet was stood down. See docs/comment-notes-b10.md#quiet-stand-down.
func (m *Mod) fleetQuiet(ctx context.Context, b *botRun, g *Gang) (bool, error) {
	window := FleetQuietSec()
	if window <= 0 || !g.AutoFormed || g.State.OrderID != 0 || g.State.HelpCallID != 0 {
		return false, nil
	}
	now := clock.Unix()
	if g.State.QuietSince == 0 {
		return false, m.writeHuntState(ctx, g.GangID, &g.State, func(s *GangState) {
			if s.QuietSince == 0 {
				s.QuietSince = now
			}
		})
	}
	if now-g.State.QuietSince < window {
		return false, nil
	}
	why := fmt.Sprintf("nothing to do on its ground for %dm", (now-g.State.QuietSince)/60)
	if err := m.standFleetDown(ctx, g, why); err != nil {
		return false, err
	}
	st := b.State
	st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
	return true, saveState(ctx, m.db, st)
}

// fleetHasWork clears the quiet clock once the fleet has a site again.
func (m *Mod) fleetHasWork(ctx context.Context, g *Gang) {
	if g.State.QuietSince == 0 {
		return
	}
	if err := m.writeHuntState(ctx, g.GangID, &g.State, func(s *GangState) { s.QuietSince = 0 }); err != nil {
		rlogf("rolebots: clear quiet clock on gang %d: %v", g.GangID, err)
	}
}

// siteScope is the whole difference between a hunting pack's search and a security
// patrol's. The walk, threat arithmetic, distress board and sweep rotation are one function;
// a corporation decides how far its guns may go and whether they may be fussy about ground.
type siteScope struct {
	// band is the residency scope the walk may not leave (residency.go): bandAdjacent for a
	// hunting pack, bandHome for a patrol, which does not leave home.
	band scopeBand
	// picky reports whether the fleet's own taste may veto a system it could survive: a pack
	// hunts for a living and may want a system worth the trip, a patrol works its own ground.
	picky bool
	// verb is the log line's word for the trip, and the phase the members fly.
	verb  string
	phase string
}

var (
	huntScope     = siteScope{band: bandAdjacent, picky: true, verb: "hunt", phase: "gang.hunt"}
	securityScope = siteScope{band: bandHome, picky: false, verb: "patrol", phase: "gang.security"}
)

// gangPlanSecurity routes one member of the employer's patrol onto its beat. The seat
// flies the ordinary combat gunboat (seatActivityFor); gangSecuritySite makes it a patrol.
func (m *Mod) gangPlanSecurity(ctx context.Context, b *botRun, g *Gang, seat string) error {
	cfg := combatConfigFor(g.Config)
	st := b.State
	st.Activity = ActCombat // a patrol flies its bounty-hunting fit

	ready, err := m.gangSeatReady(ctx, b, seat)
	if err != nil || !ready {
		return err
	}
	// A siege its orders hold comes before the beat (possiege.go).
	if post := m.gangSiegePost(ctx, g); post.TowerID != 0 {
		return m.gangPlanSiege(ctx, b, g, post, securityScope.phase)
	}
	site, err := m.gangSecuritySite(ctx, b, g, cfg)
	if err != nil {
		// Nothing on home ground worth turning out for is a guard's resting state, not a failure.
		// It holds rather than widening, until the quiet window stands the patrol down.
		if errors.Is(err, errNoHuntingGround) {
			if stood, qerr := m.fleetQuiet(ctx, b, g); qerr != nil || stood {
				return qerr
			}
			return m.gangWait(ctx, b, "home ground is quiet — standing by")
		}
		return err
	}
	m.fleetHasWork(ctx, g)
	st.Data = gangDutyData(st.Data, g)
	m.logBot(b.CharID, logInfo, "gang", "on patrol with %q — working %s",
		g.Name, site.HuntSystemName)
	return m.beginTravel(ctx, b, site.HuntSystemID, 0, securityScope.phase)
}

// gangHuntSite picks (or returns) the system the pack is sweeping: the
// nearest system with belts inside the configured security band that the
// gang has not just cleaned out.
func (m *Mod) gangHuntSite(ctx context.Context, b *botRun, g *Gang, cfg combatConfig) (*GangState, error) {
	// A pack formed to hunt pilots goes where the legal-target census says the fights are
	// (legalcensus.go); with no lawful trouble in reach it hunts the ordinary way.
	if g.State.Focus == gangFocusPvP && lethalityOn() {
		if gs, err := m.gangPackSite(ctx, b, g, cfg); err != nil {
			return nil, err
		} else if gs != nil {
			return gs, nil
		}
	}
	return m.gangSite(ctx, b, g, cfg, huntScope)
}

// gangPackSite is the PvP pack's site pick: the census's best system for the
// commander, held for a sweep's length so the whole pack arrives and stays
// together. Nil (no error) when there is nothing lawful in reach.
func (m *Mod) gangPackSite(ctx context.Context, b *botRun, g *Gang, cfg combatConfig) (*GangState, error) {
	now := clock.Unix()
	gs := &g.State
	// A pack answers its kin's calls like any combat fleet (gangcalls.go): a fight a kin
	// fleet is losing is what a pack is for.
	if adopted, err := m.adoptHelpCall(ctx, b, g); err != nil || adopted {
		return gs, err
	}
	if huntSweepHeld(gs, now) {
		return gs, nil
	}
	sysID, score := m.legalSiteFor(ctx, b, cfg.SecMin)
	if sysID == 0 || score <= 0 {
		return nil, nil
	}
	name := m.nameOfSystem(ctx, sysID)
	v, err := m.commitObjective(ctx, g.GangID, gs, GangObjective{
		Kind: objSweep, Ref: sysID, SystemID: sysID,
		Prio: objectivePrio(objSweep), Until: now + gangHuntSweepSec,
	}, func(st *GangState) {
		fileHuntRecent(st, sysID)
		dropSiteOffHunt(st, g.GangID, sysID)
		st.HuntSystemID, st.HuntSystemName, st.HuntPickedAt = sysID, name, now
	})
	if err != nil {
		return nil, err
	}
	if v == objRefuse || v == objSame {
		if gs.HuntSystemID == 0 {
			return nil, nil
		}
		return gs, nil
	}
	m.logBot(b.CharID, logInfo, "gang",
		"hunting party %q is going where the trouble is - %s", g.Name, gs.HuntSystemName)
	return gs, nil
}

// gangSecuritySite is the same search confined to the employer's home region. Nothing
// outside it is a candidate at any depth: the BFS stops at the region line as the hunt's
// does, and only one region lies inside it.
func (m *Mod) gangSecuritySite(ctx context.Context, b *botRun, g *Gang, cfg combatConfig) (*GangState, error) {
	return m.gangSite(ctx, b, g, cfg, securityScope)
}

func (m *Mod) gangSite(ctx context.Context, b *botRun, g *Gang, cfg combatConfig, sc siteScope) (*GangState, error) {
	now := clock.Unix()
	gs := &g.State
	// A kin fleet's call for help or the corporation's call to arms (gangcalls.go) comes
	// before the sweep clock; answering bumps SiteSeq so members flying elsewhere re-plan.
	if adopted, err := m.adoptHelpCall(ctx, b, g); err != nil || adopted {
		return gs, err
	}
	if huntSweepHeld(gs, now) {
		return gs, nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	// Scoped by the planning pilot's residency (residency.go): the pack works its leader's
	// neighbourhood, the patrol its own region, with no widen ladder.
	r := m.economicRules(ctx, b, sc.band)
	recent := map[int64]bool{}
	for _, id := range gs.HuntRecent {
		recent[id] = true
	}
	if gs.HuntSystemID != 0 {
		recent[gs.HuntSystemID] = true
	}

	// The pack's pooled guns decide what it can take on: the margin the solo search reads,
	// against the whole fleet (threat.go). Bigger gangs unlock harder space by arithmetic.
	packDPS, packEHP, powerKnown := m.effectivePower(ctx, b)

	// The distress board, read once for the relief stamp and the preferred bucket below
	// (beltdistress.go).
	distress := m.distressedBeltSystems(ctx, b.CorpID)

	// A fleet formed to answer a cry spends its stamp here, once, whatever comes of it. See
	// docs/comment-notes-b10.md#relief-stamp.
	spentRelief := gs.ReliefSystemID != 0
	pick := func(kind string, sysID int64, extra func(*GangState)) (objVerdict, error) {
		name := m.nameOfSystem(ctx, sysID)
		return m.commitObjective(ctx, g.GangID, gs, GangObjective{
			Kind: kind, Ref: sysID, SystemID: sysID,
			Prio: objectivePrio(kind), Until: now + gangHuntSweepSec,
		}, func(st *GangState) {
			if spentRelief {
				st.ReliefSystemID, st.ReliefSystemName = 0, ""
			}
			fileHuntRecent(st, sysID)
			dropSiteOffHunt(st, g.GangID, sysID)
			st.HuntSystemID, st.HuntSystemName, st.HuntPickedAt = sysID, name, now
			if extra != nil {
				extra(st)
			}
		})
	}
	// kept is what a refused or redundant commit hands back: the fleet's standing site, which
	// outranked the pick.
	kept := func() (*GangState, error) {
		if gs.HuntSystemID == 0 {
			return nil, fmt.Errorf("%w: the fleet is committed elsewhere", errNoHuntingGround)
		}
		return gs, nil
	}
	if relief := gs.ReliefSystemID; relief != 0 {
		gs.ReliefSystemID, gs.ReliefSystemName = 0, ""
		ms, known := u.meta[relief], distress[relief] && !recent[relief]
		if known && ms.Security >= cfg.SecMin && ms.Security <= 1.0 &&
			(!powerKnown || !hopeless(threatMargin(packDPS, packEHP,
				m.spawnThreatFor(ctx, ms.RegionID, ms.Security)))) {
			v, err := pick(objRelief, relief, nil)
			if err != nil {
				return nil, err
			}
			if v == objRefuse || v == objSame {
				return kept()
			}
			m.logBot(b.CharID, logInfo, "gang",
				"gang %q answers a distress signal — sweeping %s to open its belts",
				g.Name, gs.HuntSystemName)
			return gs, nil
		}
	}

	// The Director of Security's standing order (corpstaff_dept.go): the war enemy's system or
	// a belt its miners are run off. Taken on the relief stamp's terms, else ignored.
	if o := m.corpFleetOrderFor(ctx, g.CorpID, portSecurity); o != nil && o.SystemID != 0 &&
		o.SystemID != gs.HuntSystemID && !recent[o.SystemID] {
		ms := u.meta[o.SystemID]
		// A war order is the one exception to the no-march rule: the corporation paid CONCORD to
		// fight that enemy, wherever it works.
		inBand := r.Regions == nil || r.Regions[ms.RegionID] ||
			(warEverywhere() && relWarEnemySystem(g.CorpID, o.SystemID))
		if inBand && ms.Security >= cfg.SecMin && ms.Security <= 1.0 &&
			(!powerKnown || !hopeless(threatMargin(packDPS, packEHP,
				m.spawnThreatFor(ctx, ms.RegionID, ms.Security)))) {
			v, err := pick(objOrder, o.SystemID, func(st *GangState) { st.OrderSystemID = o.SystemID })
			if err != nil {
				return nil, err
			}
			if v == objRefuse || v == objSame {
				return kept()
			}
			m.logBot(b.CharID, logInfo, "gang",
				"gang %q takes its corporation's order — sweeping %s (%s)",
				g.Name, gs.HuntSystemName, o.Reason)
			return gs, nil
		}
	}

	// An open site in reach the pooled pack can clear beats a belt sweep (sites.go). Hunt
	// scope only: a patrol is there for the belts. Nothing winnable leaves the walk below.
	if sc.verb == huntScope.verb {
		if c := m.gangSiteCandidate(ctx, b, g, cfg, packDPS, packEHP, powerKnown); c != nil {
			sysID, name := c.Site.SolarSystemID, m.nameOfSystem(ctx, c.Site.SolarSystemID)
			v, err := m.commitObjective(ctx, g.GangID, gs, GangObjective{
				Kind: objSite, Ref: c.Site.SiteID, SystemID: sysID,
				Prio: objectivePrio(objSite), Until: now + siteRunMaxSec + siteMusterMaxSec,
			}, func(st *GangState) {
				if spentRelief {
					st.ReliefSystemID, st.ReliefSystemName = 0, ""
				}
				fileHuntRecent(st, sysID)
				st.HuntSystemID, st.HuntSystemName, st.HuntPickedAt = sysID, name, now
				st.SiteID, st.SiteName = c.Site.SiteID, c.Card.Name
			})
			if err != nil {
				return nil, err
			}
			if v == objRefuse || v == objSame {
				return kept()
			}
			m.logBot(b.CharID, logInfo, "gang",
				"gang %q is going to run %s in %s (%d rat(s), %.0f%% margin)",
				g.Name, c.Card.Name, gs.HuntSystemName, c.Card.Rats, c.Margin*100)
			return gs, nil
		}
	}

	// walk is the breadth-first search for the nearest system worth a sweep, given the
	// systems to leave out. It returns the pick and the survivable-but-unappealing spare.
	walk := func(recent map[int64]bool) (choice, fallback int64) {
		seen := map[int64]bool{b.SystemID: true}
		frontier := []int64{b.SystemID}
		for depth := 0; len(frontier) > 0 && choice == 0; depth++ {
			// Collected then drawn, not first-found: two gangs walking the same graph from nearby
			// systems would otherwise agree on the same answer every time.
			var candidates, distressCands []int64
			for _, sysID := range frontier {
				ms := u.meta[sysID]
				// A distressed system ignores the band's ceiling, like the relief stamp: the cry is a work
				// order, and most of the backlog sits at 0.8 and up where no hunting band reaches.
				secMax := cfg.SecMax
				if distress[sysID] {
					secMax = 1.0
				}
				if ms.Security < cfg.SecMin || ms.Security > secMax || recent[sysID] {
					continue
				}
				if !u.hasBelts(ctx, m.db, sysID) {
					continue
				}
				if powerKnown {
					margin := threatMargin(packDPS, packEHP,
						m.spawnThreatFor(ctx, ms.RegionID, ms.Security))
					if hopeless(margin) {
						continue
					}
					// Taste never demotes a work order: hopeless() is the hard gate for a distressed system,
					// wouldEngage only for plain ground.
					if sc.picky && !wouldEngage(b.Tune, margin) && !distress[sysID] {
						if fallback == 0 {
							// Survivable but unappealing: remembered while the walk looks for a system worth having,
							// taken only when the band offers nothing better.
							fallback = sysID
						}
						continue
					}
				}
				// Miners locked out here: the pack's loudest work order at this depth (beltdistress.go).
				if distress[sysID] {
					distressCands = append(distressCands, sysID)
					continue
				}
				candidates = append(candidates, sysID)
			}
			switch {
			case len(distressCands) > 0:
				choice = distressCands[rand.Intn(len(distressCands))]
				m.logBot(b.CharID, logInfo, "gang",
					"gang %q answers a distress signal — sweeping %s to open its belts",
					g.Name, m.nameOfSystem(ctx, choice))
			case len(candidates) > 0:
				choice = candidates[rand.Intn(len(candidates))]
			}
			var next []int64
			for _, sysID := range frontier {
				for _, nb := range u.adj[sysID] {
					if seen[nb] {
						continue
					}
					seen[nb] = true
					nm, ok := u.meta[nb]
					if !ok || dispSec(nm.Security) < r.SecFloor {
						continue
					}
					if len(r.Regions) > 0 && !r.Regions[nm.RegionID] {
						continue // the walk stops at the scope's region line
					}
					next = append(next, nb)
				}
			}
			frontier = next
		}
		return choice, fallback
	}
	choice, fallback := walk(recent)
	// A patrol works one region and never takes the spare, so a beat of a few systems is all
	// "just swept" after one rotation. It starts the beat over rather than standing idle.
	rewalked := false
	if choice == 0 && fallback == 0 && !sc.picky && len(gs.HuntRecent) > 0 {
		again := map[int64]bool{}
		if gs.HuntSystemID != 0 {
			again[gs.HuntSystemID] = true
		}
		choice, fallback = walk(again)
		rewalked = choice != 0
	}
	if choice == 0 {
		choice = fallback
	}
	if choice == 0 {
		return nil, fmt.Errorf(
			"%w in the %.1f–%.1f band is reachable that the %s has not just swept and could win in",
			errNoHuntingGround, cfg.SecMin, cfg.SecMax, sc.verb)
	}
	var restart func(*GangState)
	if rewalked {
		restart = func(st *GangState) { st.HuntRecent = nil }
	}
	if v, err := pick(objSweep, choice, restart); err != nil {
		return nil, err
	} else if v == objRefuse || v == objSame {
		return kept()
	}
	if powerKnown {
		ms := u.meta[choice]
		th := m.spawnThreatFor(ctx, ms.RegionID, ms.Security)
		m.logBot(b.CharID, logInfo, "gang", "gang %q moves the %s to %s — %s, %s for the pack",
			g.Name, sc.verb, gs.HuntSystemName, th.describe(),
			marginVerdict(b.Tune, threatMargin(packDPS, packEHP, th)))
	} else {
		m.logBot(b.CharID, logInfo, "gang", "gang %q moves the %s to %s",
			g.Name, sc.verb, gs.HuntSystemName)
	}
	return gs, nil
}

// ── gang piracy ─────────────────────────────────────────────────────────────

// gangPlanPirate routes one pack member into the shared hunting ground. Whatever a
// member's own past left unfinished (a criminal timer, a half-stripped wreck, a hold of
// stolen goods) comes first, in the solo planner's order; the pack does not wait for it.
func (m *Mod) gangPlanPirate(ctx context.Context, b *botRun, g *Gang, seat string) error {
	st := b.State
	st.Activity = ActPiracy

	// A pilot with no piracy in it does not commit crimes because a board said so. The
	// formation sweep never drafts one, but a hand-built gang can: sit the op out.
	if b.Tune.PiracyAppetite <= 0 {
		m.logBot(b.CharID, logInfo, "gang",
			"gang %q turned pirate and this pilot is no outlaw — sitting the op out on its own schedule",
			g.Name)
		st.Data.GangID, st.Data.GangBeltID = 0, 0
		return m.planSolo(ctx, b)
	}

	if until := m.piracyHeatUntil(b); until > clock.Unix() {
		return m.piracyGoToGround(ctx, b, until)
	}
	if handled, err := m.piracyResumeLoot(ctx, b); err != nil {
		return err
	} else if handled {
		return nil
	}
	if handled, err := m.piracyResumeFence(ctx, b); err != nil {
		return err
	} else if handled {
		return nil
	}

	ready, err := m.gangSeatReady(ctx, b, seat)
	if err != nil || !ready {
		return err
	}
	site, err := m.gangPirateSite(ctx, b, g)
	if err != nil {
		return err
	}
	st.Data = gangDutyData(st.Data, g)
	st.Data.PiracySystemID = site.PirSystemID
	st.Data.PiracyGateID = site.PirGateID
	st.Data.PiracyPickedAt = clock.Unix()
	next := "gang.pirate.prowl"
	if site.PirGateID != 0 {
		next = "gang.pirate.camp"
		m.logBot(b.CharID, logInfo, "gang",
			"joining gang %q on the gate camp in %s", g.Name, site.PirSystemName)
	} else {
		m.logBot(b.CharID, logInfo, "gang",
			"prowling %s with gang %q", site.PirSystemName, g.Name)
	}
	return m.beginTravel(ctx, b, site.PirSystemID, 0, next)
}

// gangPirateSite is the pack's shared hunting ground, picked once by the first member to
// plan: the solo site search run with that pilot's nerve (its crime ceiling and camp or
// prowl taste become the pack's for this stint), then posted on the blackboard. It rotates
// on the solo piracy stint clocks and remembers where it has just worked.
func (m *Mod) gangPirateSite(ctx context.Context, b *botRun, g *Gang) (*GangState, error) {
	gs := &g.State
	now := clock.Unix()
	if gs.PirSystemID != 0 && now-gs.PirPickedAt < gangPirateStint(gs) {
		return gs, nil
	}
	cfg := m.piracyConfigDerived(b)
	// The pack shares one memory of where it has just worked; hand it to
	// the solo search, which reads the bot's own HuntRecent.
	b.State.Data.HuntRecent = gs.HuntRecent
	sysID, gateID, name, err := m.piracySite(ctx, b, cfg)
	if err != nil {
		return nil, fmt.Errorf("no hunting ground for the gang: %w", err)
	}
	if gs.PirSystemID != 0 {
		gs.HuntRecent = append([]int64{gs.PirSystemID}, gs.HuntRecent...)
		if len(gs.HuntRecent) > gangHuntRecentMax {
			gs.HuntRecent = gs.HuntRecent[:gangHuntRecentMax]
		}
	}
	gs.PirSystemID, gs.PirSystemName = sysID, name
	gs.PirGateID = gateID
	gs.PirPickedAt = now
	if err := saveGangState(ctx, m.db, g.GangID, gs); err != nil {
		return nil, err
	}
	if gateID != 0 {
		m.logBot(b.CharID, logInfo, "gang",
			"gang %q sets up a gate camp in %s — %s", g.Name, name, piracyDoctrine(cfg))
	} else {
		m.logBot(b.CharID, logInfo, "gang",
			"gang %q moves the hunt to the belts of %s — %s", g.Name, name, piracyDoctrine(cfg))
	}
	return gs, nil
}

// gangPirateStint is how long the pack works one ground: a camp is a longer
// commitment than a prowl, same as solo.
func gangPirateStint(gs *GangState) int64 {
	if gs.PirGateID != 0 {
		return piracyCampSec
	}
	return piracyStintSec
}

// gangPirateStep keeps one pack member on the shared ground. The violence is not here: the
// projection's combat loop picks victims at combat tick rate (piracy_prey.go) and unwatched
// space runs the abstract equivalent. This keeps the ship where somebody might come along
// and pulls it to the next ground when the blackboard rotates.
func (m *Mod) gangPirateStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	// A criminal timer outranks the op, as in the solo dispatcher.
	if until := m.piracyHeatUntil(b); until > clock.Unix() {
		return m.piracyGoToGround(ctx, b, until)
	}
	gs := &g.State
	if gs.PirSystemID == 0 {
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	// Stint over: re-plan; the first planner rotates the ground for all.
	if clock.Unix()-gs.PirPickedAt >= gangPirateStint(gs) {
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	// The ground moved under this pilot (rotation mid-stint): follow it.
	if st.Data.PiracySystemID != gs.PirSystemID {
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	if b.SystemID != gs.PirSystemID {
		return m.beginTravel(ctx, b, gs.PirSystemID, 0, st.Phase)
	}
	if b.ShipID == 0 {
		return fmt.Errorf("gang pirate has no ship boarded")
	}
	if gs.PirGateID != 0 {
		// The whole pack sits the same gate, which is what makes it a camp, on the arrival side
		// where a jump-in lands (lane.go).
		if !st.Data.PiracyCampSet {
			m.parkAtGateArrival(ctx, b.ShipID, gs.PirGateID)
			m.pirateSayRun(b, evCamp)
			st.Data.PiracyCampSet = true
		}
		st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(60)
		return saveState(ctx, m.db, st)
	}
	m.hopPatrolBelt(ctx, b)
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(
		int64(gangPatrolDwellMinSec+rand.Intn(gangPatrolDwellMaxSec-gangPatrolDwellMinSec+1)))
	return saveState(ctx, m.db, st)
}

// gangHuntStep combs the hunt system's belts; the sweep clock rotates the pack to a fresh
// system through gangHuntSite on the next plan. The phase is carried in because the
// security patrol runs this loop under its own name (TaskGangSecurity); a patrol
// re-entering travel must not come back as a hunt.
func (m *Mod) gangHuntStep(ctx context.Context, b *botRun, g *Gang, phase string) error {
	st := b.State
	gs := &g.State
	// A siege its orders hold: the fleet sits on the tower, not the belts, and no dry verdict or
	// sweep clock moves it on (possiege.go).
	if post := m.gangSiegePost(ctx, g); post.TowerID != 0 {
		return m.gangSiegeStep(ctx, b, post, phase)
	}
	if gs.HuntSystemID == 0 {
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	// Sweep over: re-plan (which picks the next system for the pack). A site the pack is still
	// running holds it past the clock (siteHeld).
	if !huntSweepHeld(gs, clock.Unix()) {
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	// A member that found the system dry (markHuntDry) expires the pick for the whole fleet,
	// except for a war order (warhunt.go). See docs/comment-notes-b10.md#hunt-dry-verdict.
	if st.Data.HuntDryAt != 0 && b.SystemID == gs.HuntSystemID && gs.OrderSystemID == gs.HuntSystemID {
		if enemies := relWarEnemiesOf(g.CorpID); len(enemies) > 0 {
			set := make(map[int64]bool, len(enemies))
			for _, id := range enemies {
				set[id] = true
			}
			if warOrderLive(m.targetCensusNow(ctx).bySys, gs.HuntSystemID, set) {
				st.Data.HuntDryAt = 0
			}
		}
	}
	// A committed bounty intercept is the other exception (bountyhunt.go): the pack flew here
	// for a pilot, and an empty belt does not say the pilot is gone.
	if st.Data.HuntDryAt != 0 && huntPinned(gs, clock.Unix()) {
		st.Data.HuntDryAt = 0
	}
	if st.Data.HuntDryAt != 0 && b.SystemID == gs.HuntSystemID {
		if err := m.expireGangHunt(ctx, b, g); err != nil {
			return err
		}
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	if b.SystemID != gs.HuntSystemID {
		return m.beginTravel(ctx, b, gs.HuntSystemID, 0, phase)
	}
	if b.ShipID == 0 {
		return fmt.Errorf("gang hunter has no ship boarded")
	}
	if gs.SiteID != 0 {
		return m.gangSiteStep(ctx, b, g)
	}
	m.hopPatrolBelt(ctx, b)
	st.Data.LegEndsAt = clock.Unix() +
		int64(gangPatrolDwellMinSec+rand.Intn(gangPatrolDwellMaxSec-gangPatrolDwellMinSec+1))
	return saveState(ctx, m.db, st)
}

// expireGangHunt ends the pack's sweep of the system this member just found dry, so the
// next plan (gangSite) picks somewhere else, with this system in its recent list. It
// re-reads rather than writing from the member's copy: every member can reach this within
// a pass of each other, and only a pick still pointing at this system is expired.
func (m *Mod) expireGangHunt(ctx context.Context, b *botRun, g *Gang) error {
	fresh, err := getGang(ctx, m.db, g.GangID)
	if err != nil || fresh == nil {
		return err
	}
	if fresh.State.HuntSystemID != b.SystemID || fresh.State.HuntPickedAt == 0 {
		return nil
	}
	// The pick is a bounty intercept's, not a belt sweep's: it holds until the intercept
	// resolves or its deadline passes (bountyhunt.go).
	if huntPinned(&fresh.State, clock.Unix()) {
		return nil
	}
	// Nor is a site the pack is running: a dry belt says nothing about a dungeon.
	if siteHeld(&fresh.State, clock.Unix()) {
		return nil
	}
	fresh.State.HuntPickedAt = 0
	if fresh.State.SiteID != 0 {
		releaseSite(fresh.State.SiteID, b.CharID, g.GangID)
		fresh.State.SiteID, fresh.State.SiteName = 0, ""
	}
	fresh.State.Objective = nil
	if err := saveGangState(ctx, m.db, g.GangID, &fresh.State); err != nil {
		return err
	}
	g.State = fresh.State
	m.logBot(b.CharID, logInfo, "gang",
		"gang %q has nothing it can win left in %s — moving the hunt on",
		g.Name, m.nameOfSystem(ctx, b.SystemID))
	return nil
}

// ── the fleet lifecycle: muster → live → wind-down → settle ─────────────────

// seatActivityFor is the activity slug whose ship a member of this seat flies. Specialist
// seats resolve to their pseudo-activities (fitjob.go) so procurement buys the right hull;
// everyone else flies the ship the op itself calls for.
func seatActivityFor(activity, role string) string {
	switch role {
	case RoleTackler:
		return ActSeatTackle
	case RoleLogi:
		return ActSeatLogi
	case RoleEwar:
		return ActSeatEwar
	case RoleScout:
		return ActSeatScout
	case RoleEscort:
		// An escort wants a gunboat and nothing more particular, so it buys through the ordinary
		// combat pipeline.
		return ActCombat
	}
	switch activity {
	case ActMining, ActIndMining:
		switch role {
		case RoleHauler, RoleTrader:
			return ActTrade
		case RoleCombat, RoleHunter:
			return ActCombat
		}
		return ActMining
	case ActPiracy:
		return ActPiracy
	}
	// ActSecurity falls through here: a patrol flies the ordinary combat gunboat, so
	// procurement knows nothing about it. The activity is a fleet's orders, not a kind of ship.
	return ActCombat
}

// gangPlanMuster routes one member into the gathering: acquire the seat's ship (any
// distance, procurement already travels), then converge on the staging station and wait
// for the leader's word.
func (m *Mod) gangPlanMuster(ctx context.Context, b *botRun, g *Gang, mem *GangMember) error {
	st := b.State
	st.Activity = g.Activity

	// The appetite gate holds at the door too: a lawful pilot does not muster for a raid an
	// operator seated it on.
	if g.Activity == ActPiracy && b.Tune.PiracyAppetite <= 0 {
		m.logBot(b.CharID, logInfo, "gang",
			"gang %q is mustering for a raid and this pilot is no outlaw — sitting the op out",
			g.Name)
		m.leaveGangMuster(ctx, g.GangID, b.CharID)
		st.Data.GangID, st.Data.GangBeltID = 0, 0
		return m.planSolo(ctx, b)
	}
	// The pacifist's gate, at the same door: a pilot that will not hold a gun mends a fleet
	// from the logistics seat or sits the op out (gangform.go pacifistMaySit).
	if !pacifistMaySit(b.Tune, jobFor(seatActivityFor(g.Activity, mem.Role))) {
		m.logBot(b.CharID, logInfo, "gang",
			"gang %q wants this pilot in a %s seat and it will not fight — sitting the op out",
			g.Name, mem.Role)
		m.leaveGangMuster(ctx, g.GangID, b.CharID)
		st.Data.GangID, st.Data.GangBeltID = 0, 0
		return m.planSolo(ctx, b)
	}
	if g.State.StagingStationID == 0 {
		// No staging on record (a legacy row mid-flight): nothing to muster at, so the op runs
		// live from here.
		g.State.OpPhase = gangOpLive
		m.stampFleetLive(ctx, g)
		return saveGangState(ctx, m.db, g.GangID, &g.State)
	}

	ready, err := m.gangSeatReady(ctx, b, seatActivityFor(g.Activity, mem.Role))
	if err != nil || !ready {
		return err
	}
	st.Data = gangDutyData(st.Data, g)
	m.logBot(b.CharID, logInfo, "gang", "mustering with gang %q at %s",
		g.Name, m.stationLabel(ctx, g.State.StagingStationID))
	return m.beginTravel(ctx, b, g.State.StagingSystemID, g.State.StagingStationID, "gang.muster")
}

// gangMusterStep is the waiting room. Members hold at the staging station; the leader
// counts heads each pass and releases the fleet when everyone is docked, or drops the
// stragglers at the deadline, so one pilot lost in a shopping trip does not wedge the rest.
func (m *Mod) gangMusterStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	if g.State.OpPhase != gangOpMuster {
		// The leader released the fleet (or the op moved on): re-plan into the content phase.
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	staging := g.State.StagingStationID
	if staging == 0 {
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	if b.StationID != staging {
		return m.beginTravel(ctx, b, g.State.StagingSystemID, staging, "gang.muster")
	}
	if b.CharID == g.LeaderCharID {
		if err := m.gangMusterCheck(ctx, b, g); err != nil {
			return err
		}
	}
	// Arrived and shipped up: take it outside, where the pack forms up on its commander
	// (gangFormUpCircle, embody.go).
	if g.State.OpPhase == gangOpMuster && b.ShipID != 0 {
		// The wave gate: a fleet claims the whole window as a unit (undockwave.go). A refusal costs
		// nothing: the pilot stays docked in gang.muster, still counted present, and asks again.
		if m.admitUndockFor(ctx, b) {
			if err := m.setInSpace(ctx, b); err != nil {
				return err
			}
			st.Phase = string(TaskGangFormUp)
			st.Data.LegEndsAt = undockLegEnd(&st.Data)
			if err := saveState(ctx, m.db, st); err != nil {
				return err
			}
			// The ball goes out now, as beginTravel's does: embodyNow reads the undock marker just
			// saved, and waiting for the reconcile would show the ship already off the door.
			if m.systemProjected(b.SystemID) {
				m.embodyNow(ctx, b.CharID)
			}
			return nil
		}
		st.Data.LegEndsAt = clock.Unix() + undockRetryDelay()
		return saveState(ctx, m.db, st)
	}
	st.Data.LegEndsAt = clock.Unix() + gangWaitSec
	return saveState(ctx, m.db, st)
}

// gangFormUpStep is the waiting room with the ships out, the in-space twin of
// gangMusterStep: the phase a member sits in from clearing the staging door until the
// commander releases the fleet. It steers nothing (the circle is the embodied tier's,
// gangFormUpCircle with stationHoldCircle as fallback); it owns the phase and the leg clock.
func (m *Mod) gangFormUpStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	if g.State.OpPhase != gangOpMuster {
		// Released, or the op moved on: re-plan into the content phase.
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	if g.State.StagingSystemID == 0 {
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	// Drifted out of the staging system (a jump the sim made, a pod that woke elsewhere). Go
	// back through the station, since the form-up is entered from the door.
	if b.SystemID != g.State.StagingSystemID {
		return m.beginTravel(ctx, b, g.State.StagingSystemID, g.State.StagingStationID, "gang.muster")
	}
	if b.CharID == g.LeaderCharID {
		if err := m.gangMusterCheck(ctx, b, g); err != nil {
			return err
		}
	}
	st.Data.LegEndsAt = clock.Unix() + gangWaitSec
	return saveState(ctx, m.db, st)
}

// gangMusterCheck is the leader's head-count: every driven member docked at staging in the
// muster phase means the fleet is formed and the op goes live. Past the deadline the
// missing are dropped from the roster (they fall back to their own rotations through
// gangStep's membership re-check) and the fleet flies with whoever answered.
func (m *Mod) gangMusterCheck(ctx context.Context, b *botRun, g *Gang) error {
	return m.musterResolve(ctx, g)
}

// musterResolve is the head-count with no pilot attached, so it runs wherever the leader
// is. It is called from the leader's step and from the formation sweep (gangform.go),
// because a fleet whose commander is excused, called away or planning into another task
// would otherwise have nobody counting heads and its deadline would pass unread.
func (m *Mod) musterResolve(ctx context.Context, g *Gang) error {
	if g.State.OpPhase != gangOpMuster || g.State.StagingStationID == 0 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT gm.characterID, gm.role, COALESCE(c.stationID, 0),
		       COALESCE(c.solarSystemID, 0), COALESCE(s.phase, ''), COALESCE(s.data, '')
		FROM rolebot_gang_members gm
		JOIN rolebot_characters rb ON rb.characterID = gm.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = gm.characterID
		LEFT JOIN rolebot_state s ON s.characterID = gm.characterID
		WHERE gm.gangID = ?`, g.GangID)
	if err != nil {
		return fmt.Errorf("muster head-count: %w", err)
	}
	// The three buckets of the release rule (gangmuster.go).
	var present, enRoute, stuck []int64
	roleOf := map[int64]string{}
	for rows.Next() {
		var charID, stationID, systemID int64
		var role, phase, blob string
		if rows.Scan(&charID, &role, &stationID, &systemID, &phase, &blob) != nil {
			continue
		}
		roleOf[charID] = role
		// A member answers in one of two places: the hangar (gang.muster) or outside holding
		// formation (gang.formup). Counting only the first would fail a fully undocked fleet.
		here := (stationID == g.State.StagingStationID && phase == string(TaskGangMuster)) ||
			(systemID == g.State.StagingSystemID && phase == string(TaskGangFormUp))
		// The commander counts as present wherever it stands: it is the one seat a fleet cannot
		// drop, and counting it absent would turn every commander-side stall into a disband.
		switch {
		case charID == g.LeaderCharID || here:
			present = append(present, charID)
		case planAnswersGang(blob, g.GangID):
			// gangPlan stamps the fleet into the plan state before routing a member, so a member
			// carrying it is answering (flying in, collecting its ship, boarding).
			enRoute = append(enRoute, charID)
		default:
			stuck = append(stuck, charID)
		}
	}
	rows.Close()

	now := clock.Unix()
	deadline := now >= g.State.MusterDeadline
	if !deadline && !musterRelease(len(present), len(enRoute), len(stuck),
		musterElapsed(g.State, now), MusterGraceSec(), MusterQuorumFrac()) {
		return nil // still gathering
	}
	// The members that never started the trip lose their seats; the ones flying in keep
	// theirs and catch up at the site.
	if len(stuck) > 0 {
		for _, charID := range stuck {
			if err := removeGangMember(ctx, m.db, g.GangID, charID); err != nil {
				rlogf("rolebots: muster drop %d from gang %d: %v", charID, g.GangID, err)
			}
		}
		if deadline {
			m.logBot(g.LeaderCharID, logInfo, "gang",
				"muster deadline passed for gang %q — flying without %d straggler(s)",
				g.Name, len(stuck))
		} else {
			m.logBot(g.LeaderCharID, logInfo, "gang",
				"gang %q has its core in — leaving %d pilot(s) that never set out off the roster",
				g.Name, len(stuck))
		}
		m.narrateFleet(ctx, g.GangID, false, "Leaving without %d straggler(s).", len(stuck))
	}
	if len(present)+len(enRoute) < gangMinMembers {
		// Not enough answered the call. Straight to the settlement: everyone left is at staging,
		// and gangPlanWindDown routes both the docked and the holding through beginTravel.
		m.narrateFleet(ctx, g.GangID, true, "Not enough of us answered — standing down.")
		return m.musterStandDown(ctx, g, "not enough of the fleet answered the muster")
	}
	// A call to arms against a tower needs the siege's attack floor (siegeAttackFloor): fewer
	// ships on grid never move the bash. Standing down records the young death, so the next
	// call waits out the formation back-off (formCallsToArms).
	if g.State.Tier == tierCallToArms {
		if floor := m.siegeAttackFloor(ctx, g); len(present)+len(enRoute) < floor {
			m.narrateFleet(ctx, g.GangID, true, "Too few of us to break a tower — standing down.")
			return m.musterStandDown(ctx, g, fmt.Sprintf("too few answered to break a tower (%d of %d)",
				len(present)+len(enRoute), floor))
		}
	}
	if ok, err := m.musterCoversTheOp(ctx, g, present, enRoute, roleOf); err != nil {
		return err
	} else if !ok {
		m.narrateFleet(ctx, g.GangID, true, "The seats that answered can't do the job — standing down.")
		return m.musterStandDown(ctx, g, "the seats that answered cannot do the job")
	}

	g.State.OpPhase = gangOpLive
	if err := saveGangState(ctx, m.db, g.GangID, &g.State); err != nil {
		return err
	}
	m.stampFleetLive(ctx, g)
	m.logBot(g.LeaderCharID, logInfo, "gang",
		"gang %q is formed up at %s — %d ship(s), releasing the fleet (%d more on the way)",
		g.Name, m.stationLabel(ctx, g.State.StagingStationID), len(present), len(enRoute))
	return nil
}

// musterStandDown ends a fleet that could not gather. It disbands where it stands: the
// after-action report, then the row. That holds for a mining op too, since a fleet still
// mustering has cut no ore and has no books to settle (gangsettle.go).
func (m *Mod) musterStandDown(ctx context.Context, g *Gang, why string) error {
	return m.standFleetDown(ctx, g, why)
}

// standFleetDown disbands a fleet that has no settlement to run: the after-action report,
// the row, and the corporation's formation back-off when the fleet died young. A mining op
// with ore in its cans or ledgers winds down through sweepAutoGangs, not through here.
func (m *Mod) standFleetDown(ctx context.Context, g *Gang, why string) error {
	release, ok, err := m.claimGangEnd(ctx, g.GangID)
	if err != nil || !ok {
		return err
	}
	defer release()
	m.mailFleetReport(ctx, g, why)
	if err := deleteGang(ctx, m.db, g.GangID); err != nil {
		return err
	}
	noteFleetDiedYoung(g.CorpID, g.Activity, g.Created)
	m.logBot(g.LeaderCharID, logInfo, "gang", "gang %q disbanded — %s; members return to their own rotations",
		g.Name, why)
	return nil
}

// haulerSeat reports whether a gang role carries the op's ore.
func haulerSeat(role string) bool { return role == RoleHauler || role == RoleTrader }

// haulerCovered reports whether a mining op's haul is covered by the pilots that answered:
// a hauling seat that is in, or one still flying to the muster. Pure.
func haulerCovered(present, enRoute []int64, role map[int64]string) (covered, onTheWay bool) {
	for _, charID := range present {
		if haulerSeat(role[charID]) {
			return true, false
		}
	}
	for _, charID := range enRoute {
		if haulerSeat(role[charID]) {
			return true, true
		}
	}
	return false, false
}

// musterCoversTheOp is the second half of the head-count: can the fleet that answered
// still do the job? A mining op released as miners alone would pile cans on the belt and
// deliver nothing. A hauler still flying in covers it: industrials are the slowest hulls to
// the muster, and no can is full before one arrives (liveOpCoversHaul is the net when it
// never does). Failing that, a present miner that owns an industrial is re-seated as hauler;
// with nobody able to carry, the op stands down.
func (m *Mod) musterCoversTheOp(ctx context.Context, g *Gang, present, enRoute []int64, role map[int64]string) (bool, error) {
	if g.Activity != ActMining && g.Activity != ActIndMining {
		return true, nil
	}
	if covered, onTheWay := haulerCovered(present, enRoute, role); covered {
		if onTheWay {
			m.logBot(g.LeaderCharID, logInfo, "gang",
				"gang %q releases with its hauler still flying in — the cans wait for it", g.Name)
		}
		return true, nil
	}
	if ok, err := m.reseatHauler(ctx, g, present); err != nil || ok {
		return ok, err
	}
	m.logBot(g.LeaderCharID, logInfo, "gang",
		"gang %q has no hull that can carry ore — standing the op down rather than mining into cans nobody will collect",
		g.Name)
	return false, nil
}

// reseatHauler gives the hauler's seat to the first candidate that owns an industrial
// (haulerSeatSpec is industrial-only); its band goes onto the row so gangSeatReady holds it
// there. The commander is skipped: it mines, and a mining op is led from the belt.
func (m *Mod) reseatHauler(ctx context.Context, g *Gang, candidates []int64) (bool, error) {
	for _, charID := range candidates {
		if charID == g.LeaderCharID {
			continue
		}
		ok, err := m.hasSeatShipIn(ctx, charID, ActTrade, haulerSeatSpec)
		if err != nil || !ok {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_gang_members SET role = ?, hullMin = ?, hullMax = ?
			WHERE gangID = ? AND characterID = ?`,
			RoleHauler, int(haulerSeatSpec.HullMin), int(haulerSeatSpec.HullMax),
			g.GangID, charID); err != nil {
			return false, fmt.Errorf("re-seat hauler: %w", err)
		}
		forgetSeatSpec(charID)
		m.logBot(charID, logInfo, "gang",
			"gang %q lost its haulage — taking the hauler's seat so the op's cans get collected",
			g.Name)
		return true, nil
	}
	return false, nil
}

// liveOpCoversHaul is the net under the muster's "hauler on the way" release: a live mining
// op with nobody answering in a hauling seat. It starts a clock (GangState.NoHaulerSince);
// after half of HaulerGraceSec it re-seats an answering miner that owns an industrial, and
// failing that it winds the op down after HaulerGraceSec, so the miners stop filling cans
// nobody collects. Run from
// sweepAutoGangs; it writes through writeHuntState because members' plans write the same row.
func (m *Mod) liveOpCoversHaul(ctx context.Context, g *Gang) error {
	if g.Activity != ActMining && g.Activity != ActIndMining {
		return nil
	}
	if g.State.OpPhase != gangOpLive && g.State.OpPhase != "" {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT gm.characterID, gm.role, COALESCE(s.data, '')
		FROM rolebot_gang_members gm
		JOIN rolebot_characters rb ON rb.characterID = gm.characterID AND rb.enabled = 1
		LEFT JOIN rolebot_state s ON s.characterID = gm.characterID
		WHERE gm.gangID = ?`, g.GangID)
	if err != nil {
		return fmt.Errorf("haul watch: %w", err)
	}
	var answering []int64
	role := map[int64]string{}
	for rows.Next() {
		var charID int64
		var r, blob string
		if rows.Scan(&charID, &r, &blob) != nil || !planAnswersGang(blob, g.GangID) {
			continue
		}
		answering = append(answering, charID)
		role[charID] = r
	}
	rows.Close()

	covered, _ := haulerCovered(answering, nil, role)
	now := clock.Unix()
	if !covered && haulReseatDue(g.State.NoHaulerSince, now, HaulerGraceSec()) {
		if covered, err = m.reseatHauler(ctx, g, answering); err != nil {
			return err
		}
	}
	switch {
	case covered:
		if g.State.NoHaulerSince == 0 {
			return nil
		}
		return m.writeHuntState(ctx, g.GangID, &g.State, func(s *GangState) { s.NoHaulerSince = 0 })
	case g.State.NoHaulerSince == 0:
		return m.writeHuntState(ctx, g.GangID, &g.State, func(s *GangState) { s.NoHaulerSince = now })
	case now-g.State.NoHaulerSince < HaulerGraceSec():
		return nil
	}
	if err := m.writeHuntState(ctx, g.GangID, &g.State, func(s *GangState) {
		s.OpPhase = gangOpWindDown
		s.WindDownDeadline = now + WindDownSec()
	}); err != nil {
		return err
	}
	m.logBot(g.LeaderCharID, logInfo, "gang",
		"gang %q winds down — nobody has hauled for it in %dm; the fleet returns to staging to settle up",
		g.Name, (now-g.State.NoHaulerSince)/60)
	return nil
}

// haulReseatDue reports whether a live op with no answering hauler may re-seat a miner: its
// no-hauler clock has run half the grace. A hauler between legs or in an idle-retry drops its
// gang stamp for a pass, so the first miss only starts the clock. Pure.
func haulReseatDue(noHaulerSince, now, grace int64) bool {
	return noHaulerSince != 0 && now-noHaulerSince >= grace/2
}

// gangPlanWindDown routes one member home: back to the staging station,
// docked, for the settlement round.
func (m *Mod) gangPlanWindDown(ctx context.Context, b *botRun, g *Gang, mem *GangMember) error {
	st := b.State
	st.Activity = g.Activity
	if g.State.StagingStationID == 0 {
		// Nowhere to settle (the sweep hard-kills these): hold until it does.
		return m.gangWait(ctx, b, "the op is over and there is no staging station to settle at")
	}
	// The last run: haulers go back for the rock still in the cans before anyone settles,
	// repeating until dry or the deadline (extended once, gangLastRunGraceSec).
	if mem != nil && (mem.Role == RoleHauler || mem.Role == RoleTrader) && g.State.SystemID != 0 &&
		(g.Activity == ActMining || g.Activity == ActIndMining) {
		now := clock.Unix()
		m3 := m.gangCanM3At(ctx, g.GangID, g.State.SystemID)
		if _, stray := m.gangStrayCanSystem(ctx, g.GangID, g.State.SystemID); stray > 0 {
			m3 += stray // gangHaulLoadStep goes on to it once the site is dry
		}
		if haulerLastRun(m3, now, g.State.WindDownDeadline) {
			if g.State.LastRunAt == 0 {
				g.State.LastRunAt = now
				if g.State.WindDownDeadline < now+gangLastRunGraceSec {
					g.State.WindDownDeadline = now + gangLastRunGraceSec
				}
				if err := saveGangState(ctx, m.db, g.GangID, &g.State); err != nil {
					return err
				}
			}
			st.Data = gangDutyData(st.Data, g)
			st.Data.GangBeltID = g.State.BeltItemID
			m.logBot(b.CharID, logInfo, "gang",
				"last run for the %q op — %.0f m³ still in the cans at %s", g.Name, m3, g.State.SystemName)
			return m.beginTravel(ctx, b, g.State.SystemID, 0, "gang.haul.load")
		}
	}
	st.Data = gangDutyData(st.Data, g)
	m.logBot(b.CharID, logInfo, "gang", "gang %q is winding down — returning to %s to settle up",
		g.Name, m.stationLabel(ctx, g.State.StagingStationID))
	return m.beginTravel(ctx, b, g.State.StagingSystemID, g.State.StagingStationID, "gang.settle")
}

// gangSettleStep is the other waiting room. Members hold docked at staging;
// the leader waits for the fleet (or the deadline), runs the settlement
// round, mails the after-action report, and dissolves the gang.
func (m *Mod) gangSettleStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	staging := g.State.StagingStationID
	if staging == 0 {
		st.Activity, st.Phase = "", "idle"
		return saveState(ctx, m.db, st)
	}
	if b.StationID != staging {
		return m.beginTravel(ctx, b, g.State.StagingSystemID, staging, "gang.settle")
	}
	if b.CharID != g.LeaderCharID {
		// Docked at staging with the evening's share in the hangar: every member of a co-op stands
		// where the hauler put its rock, so disposal runs here for everyone.
		if m.hasFleetOre(ctx, b.CharID, staging) {
			m.disposeFleetOre(ctx, b)
		}
		st.Data.LegEndsAt = clock.Unix() + gangWaitSec
		return saveState(ctx, m.db, st)
	}

	// The leader's count is the same one the sweep takes (settleResolve).
	settled, err := m.settleResolve(ctx, g)
	if err != nil {
		return err
	}
	if !settled {
		st.Data.LegEndsAt = clock.Unix() + gangWaitSec
		return saveState(ctx, m.db, st)
	}
	st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
	return saveState(ctx, m.db, st)
}

// endingGangs holds the gangIDs whose end (settleResolve, standFleetDown) is running.
var endingGangs sync.Map

// claimGangEnd lets one caller end a fleet. Several paths can reach the end of one fleet at
// once (the sweep, the leader's step, each member's plan), each with its own copy of the
// row; only the caller holding the claim, while the gang row still exists, mails the report,
// pays out and deletes. ok false means another caller has it or it is already gone. The
// caller runs release when done.
func (m *Mod) claimGangEnd(ctx context.Context, gangID int64) (release func(), ok bool, err error) {
	if _, busy := endingGangs.LoadOrStore(gangID, struct{}{}); busy {
		return nil, false, nil
	}
	release = func() { endingGangs.Delete(gangID) }
	var alive int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_gangs WHERE gangID = ?`, gangID).Scan(&alive); err != nil {
		release()
		return nil, false, fmt.Errorf("claim gang end: %w", err)
	}
	if alive == 0 {
		release()
		return nil, false, nil
	}
	return release, true, nil
}

// settleAwaits reports whether the settlement still waits for a member: one whose plan
// carries this fleet and who is not docked at staging. Pure.
func settleAwaits(stationID, staging int64, blob string, gangID int64) bool {
	return stationID != staging && planAnswersGang(blob, gangID)
}

// settleResolve is the settlement count with no pilot attached, the twin of musterResolve:
// it runs from the leader's gang.settle step and from the sweep (gangform.go), because a
// commander that is excused, logged off or on another errand never takes that step. The
// fleet settles when every member still answering it is docked at staging, or when the
// deadline says stop waiting. A member whose plan no longer carries this fleet (excused,
// out working solo) is not waited for. It reports whether the gang was settled and deleted.
func (m *Mod) settleResolve(ctx context.Context, g *Gang) (bool, error) {
	staging := g.State.StagingStationID
	if g.State.OpPhase != gangOpWindDown || staging == 0 {
		return false, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT COALESCE(c.stationID, 0), COALESCE(s.data, '')
		FROM rolebot_gang_members gm
		JOIN rolebot_characters rb ON rb.characterID = gm.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = gm.characterID
		LEFT JOIN rolebot_state s ON s.characterID = gm.characterID
		WHERE gm.gangID = ?`, g.GangID)
	if err != nil {
		return false, fmt.Errorf("settle head-count: %w", err)
	}
	out := 0
	for rows.Next() {
		var stationID int64
		var blob string
		if rows.Scan(&stationID, &blob) != nil {
			continue
		}
		if settleAwaits(stationID, staging, blob, g.GangID) {
			out++
		}
	}
	rows.Close()
	now := clock.Unix()
	if out > 0 && (g.State.WindDownDeadline == 0 || now < g.State.WindDownDeadline) {
		return false, nil
	}

	// The sweep and the leader's step can both reach this point for one fleet.
	release, ok, err := m.claimGangEnd(ctx, g.GangID)
	if err != nil || !ok {
		return false, err
	}
	defer release()

	if err := m.settleGangOp(ctx, g); err != nil {
		// The books did not balance this pass; the deadline-plus-grace
		// backstop in sweepAutoGangs bounds how long this can retry.
		return false, fmt.Errorf("settling gang %q: %w", g.Name, err)
	}
	m.mailFleetReport(ctx, g, "stood down and settled up")
	if err := deleteGang(ctx, m.db, g.GangID); err != nil {
		return false, err
	}
	m.logBot(g.LeaderCharID, logInfo, "gang",
		"gang %q settled its books at %s and stood down", g.Name, m.stationLabel(ctx, staging))
	return true, nil
}

// haulerLastRun decides whether a wind-down hauler goes back to the belt:
// there is ore in the cans and the deadline (0 = none) has not passed. Pure.
func haulerLastRun(canM3 float64, now, deadline int64) bool {
	if canM3 < 1 {
		return false
	}
	return deadline == 0 || now < deadline
}

// gangCanM3At is the ore sitting in the op's cans in one system.
func (m *Mod) gangCanM3At(ctx context.Context, gangID, systemID int64) float64 {
	cans, err := m.gangCansAt(ctx, gangID, systemID)
	if err != nil {
		return 0
	}
	total := 0.0
	for _, c := range cans {
		total += c.UsedM3
	}
	return total
}

// gangMinerStandDown sends a miner to idle with whatever it has aboard,
// where gangPlanWindDown picks it up.
func (m *Mod) gangMinerStandDown(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	// The ore stays aboard: canning it would drop at a station door off the belt, or swamp the
	// haulers' last run on it. The miner flies to staging anyway.
	var aboard int64
	if lots, err := m.cargoOreLots(ctx, b, 0); err == nil {
		for _, lot := range lots {
			aboard += lot.Units
		}
	}
	if aboard > 0 {
		m.logBot(b.CharID, logInfo, "gang",
			"the %q op is winding down — bringing the last %d units home aboard rather than canning them", g.Name, aboard)
	} else {
		m.logBot(b.CharID, logInfo, "gang", "the %q op is winding down — standing down", g.Name)
	}
	st.Data.OreUnits, st.Data.OreLots = 0, nil
	st.Activity, st.Phase = "", "idle"
	return saveState(ctx, m.db, st)
}

// strandedOreHome handles the hold of a member whose gang is gone: ore aboard is brought
// to the nearest dock it will use (gang.oredrop), or put away on the spot if already
// docked. Reports whether it took the pilot's next move.
func (m *Mod) strandedOreHome(ctx context.Context, b *botRun) (bool, error) {
	lots, err := m.cargoOreLots(ctx, b, 0)
	if err != nil || len(lots) == 0 || b.ShipID == 0 {
		return false, err
	}
	st := b.State
	if b.StationID != 0 {
		return true, m.gangOreDropStep(ctx, b)
	}
	stationID, sysID, err := m.nearestAllowedStation(ctx, b, rulesForBot(b), m.homeRegionSet(ctx, b))
	if err != nil || stationID == 0 {
		return false, err
	}
	if sysID == 0 {
		loc, lerr := evedb.GetStationLocation(ctx, m.db, stationID)
		if lerr != nil {
			return false, lerr
		}
		sysID = loc.SolarSystemID
	}
	var units int64
	for _, lot := range lots {
		units += lot.Units
	}
	st.Activity = ActMining
	st.Data = idleData(st.Data)
	m.logBot(b.CharID, logInfo, "gang",
		"the fleet is gone with %d units of ore still aboard — bringing it in to %s as its own",
		units, m.stationLabel(ctx, stationID))
	return true, m.beginTravel(ctx, b, sysID, stationID, string(TaskGangOreDrop))
}

// gangOreDropStep puts a stranded hold's ore in the hangar here with a
// disposal note (rolebot_ore_hold), so the pilot's own docked pass sells or
// refines it like any fleet share.
func (m *Mod) gangOreDropStep(ctx context.Context, b *botRun) error {
	st := b.State
	if b.StationID == 0 {
		return fmt.Errorf("gang.oredrop while not docked")
	}
	lots, err := m.cargoOreLots(ctx, b, 0)
	if err != nil {
		return err
	}
	var units int64
	for _, lot := range lots {
		if err := m.moveCargoToHangar(ctx, b, lot.TypeID); err != nil {
			return err
		}
		m.noteFleetOreDelivered(ctx, b.CharID, b.StationID, lot.TypeID, m.typeNameOr(ctx, lot.TypeID), lot.Units)
		units += lot.Units
	}
	m.stackHangar(ctx, b.CharID, b.StationID)
	m.logBot(b.CharID, logInfo, "gang",
		"put %d units of the dead fleet's ore in its hangar at %s — its own rock now", units, m.stationLabel(ctx, b.StationID))
	st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
	return saveState(ctx, m.db, st)
}
