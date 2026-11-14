package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"runtime/debug"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// The abstract-tier sim: a tick-driven planner that advances every enabled
// rolebot's persistent state machine. Travel, mining and market actions happen on
// realistic timers whether or not a player can see the bot; presence (Local,
// station guest lists) follows along. The embodied tier (a nil-Conn destiny
// entity in player-loaded systems) attaches on top, and the abstract state is
// the truth.

const (
	simEveryTicks = 5 // tick.Manager runs at 1s; sim advances every 5s

	// One gate-to-gate transit, before the hull's own align is added; with a
	// frigate's align it lands at roughly 41-76 s.
	jumpLegMinSec = 35
	jumpLegMaxSec = 70
	// alignLegFactor is how many aligns one gate transit is worth: align off the
	// last object, the sublight burn to the gate, and align again on the far side.
	alignLegFactor = 3.0
	dockSec        = 15
	// Time spent sitting off the station before docking. Long enough that
	// a player watching an undock actually sees the bot on grid.
	dockApproachSec = 45
	idleRetrySec    = 120 // idle bots re-plan this often

	// What one bot's turn may spend on the database before the pass moves
	// on without it, and the shorter clock the failure record itself gets
	// (a timed-out bot's own context is already dead).
	simBotBudget  = 20 * time.Second
	simSaveBudget = 5 * time.Second

	// How long a docked bot stays in a hangar doing its business before it undocks.
	// A trader that docks, buys and undocks inside one sim pass reads as a script,
	// and a player who chased it in would find an empty guest list.
	dwellMinSec = 20
	dwellMaxSec = 40
	// The dwell when somebody can see it (a real player on the station's guest list,
	// or the system on grid): long enough to open the bot's info and talk to it.
	dwellWatchedMinSec = 60
	dwellWatchedMaxSec = 120
)

// stationHasRealGuest reports whether anyone who is not one of our own bots
// is docked at the station. The guest list is the only place a docked bot
// is visible, so it is what "being watched" means inside a hangar.
func (m *Mod) stationHasRealGuest(stationID int64) bool {
	if m.stations == nil || stationID == 0 {
		return false
	}
	presenceMu.Lock()
	defer presenceMu.Unlock()
	for _, id := range m.stations.Guests(stationID) {
		if _, bot := presenceApplied[id]; !bot {
			return true
		}
	}
	return false
}

// stationDwell picks how long the bot lingers docked before its next move,
// stretching when a real player could notice it leave and scaling by the
// pilot's patience.
func (m *Mod) stationDwell(b *botRun) int64 {
	if m.stationHasRealGuest(b.StationID) || m.systemWatched(b.SystemID) {
		return b.Tune.dwell(int64(dwellWatchedMinSec + rand.Intn(dwellWatchedMaxSec-dwellWatchedMinSec+1)))
	}
	return b.Tune.dwell(int64(dwellMinSec + rand.Intn(dwellMaxSec-dwellMinSec+1)))
}

func (m *Mod) startSim() {
	m.registerPeriodic("rolebots-sim", simEveryTicks, func(tickNum uint64) bool {
		if !Master() {
			return true
		}
		// The tick manager runs everything on one goroutine: do the DB work off it and
		// let ticks overlap-guard themselves.
		if !m.simBusy.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer m.simBusy.Store(false)
			m.simPass(context.Background())
		}()
		return true
	})
}

// simPass advances every enabled rolebot once. The deadline is per bot, not per
// pass: a shared budget lets one slow planner starve every bot behind it, and a
// bot that overruns loses only its own turn. Bots run in a sharded worker pool
// (simpool.go); the once-per-pass upkeep sweeps follow, serially.
func (m *Mod) simPass(ctx context.Context) {
	loadCtx, loadCancel := context.WithTimeout(ctx, simBotBudget)
	bots, err := m.loadRuns(loadCtx)
	loadCancel()
	if err != nil {
		rlogf("rolebots: sim: %v", err)
		return
	}
	// The online cap decides who is on the server this pass (popcap.go). A benched
	// pilot takes no turn, holds no presence and projects no ball.
	m.runBotPass(ctx, m.applyPopCap(bots))
	// Pilots off for their hours took no turn; one whose job has sat finished too long
	// logs in briefly to take it off the line (tzclaim.go).
	m.tzClaimPass(ctx, bots)
	// Presence follows the location in the database, not what the step claimed;
	// syncPresence is a diff, so a pass with nothing to do costs one query.
	m.syncPresence()
	m.upkeepPass(ctx)
}

// upkeepPass runs the once-per-pass sweeps that belong to the roster as a whole.
// Reconciling projections and expiring jetcans mutate the same SolarSystems and push
// balls at the same connections, so they stay in this order on one goroutine, and the
// reconcile runs before embodyTick lets projections warp. Each phase has its own budget.
func (m *Mod) upkeepPass(ctx context.Context) {
	withBudget := func(f func(context.Context)) {
		c, cancel := context.WithTimeout(ctx, simBotBudget)
		defer cancel()
		f(c)
	}

	// Grid lane, on the calling goroutine.
	func() {
		defer func() {
			if r := recover(); r != nil {
				rlogf("rolebots: upkeep (grid lane) panicked: %v\n%s", r, debug.Stack())
			}
		}()
		// Embodied tier: align projections with the new truth. embodyTick runs on the
		// grid lane's own clock (gridlane.go); only this roster-wide reconcile stays here.
		withBudget(func(c context.Context) { m.reconcileEmbodiment(c) })
		// Jetcan upkeep: expire module cans and drop registry rows whose
		// entity the core has already removed.
		withBudget(m.sweepCans)
	}()
}

// botRun is the in-memory working set for one enabled rolebot.
type botRun struct {
	CharID int64
	Name   string
	CorpID int64
	// Traits is the pilot's personality as the roster pass bulk-loaded it (hydrateRuns);
	// Tune is resolved from it. Read through botTraits so a run built without the bulk
	// load still answers.
	Traits    map[string]int
	SystemID  int64 // current system (from character_)
	StationID int64 // 0 = in space
	ShipID    int64 // active ship entity (0 = none boarded)
	// GangID is the enabled gang this pilot belongs to, or 0. It is read on the
	// roster query because the sim pass needs it before any bot steps: it is the
	// shard key that keeps a gang's members on one worker (simpool.go).
	GangID int64
	// BossID is the player this pilot is serving under, or 0. It rides the roster row
	// so conscriptCallUp needs no query. Not a shard key: a conscript shares no
	// blackboard with its fleetmates, so it shards by charID and keeps the grid lane.
	BossID int64
	// SecStatus is the pilot's own security record, carried on the roster query
	// rather than looked up per pilot per pass. It is what makes an outlaw behave
	// like one in routing, engagement and fleeing.
	SecStatus float64
	// FactionID is the pilot's employment faction: the faction behind CorpID when
	// that is an NPC corporation, 0 for a player corp. It stops a pilot employed by
	// the Guristas opening fire on Guristas rats (corpjoin.go, faction.go).
	FactionID int64
	Profile   *Profile
	State     *SimState
	// Tune is this bot's personality resolved into the numbers the sim reads (flee
	// thresholds, belt worth in jumps, loiter time), once per pass in loadRuns. It is
	// copied by value into the embodied tier, which reads it from its own goroutines.
	Tune Tuning
	// fastLane marks a run hydrated by the grid lane (gridlane.go) rather
	// than by the roster pass. It is what tells stepBot which of the two
	// owns this pilot's turn.
	fastLane bool
	// Broke is set for the one pass in which a pilot that could not pay for any work
	// goes and earns instead (broke.go). It relaxes what the fit selector and the ship
	// check settle for, and is cleared when that attempt ends.
	Broke bool
	// loadedSeq is the reset sequence this run was read under (stateepoch.go):
	// a pilot reset since then is not stepped from this copy. 0 = built by
	// hand, never stale.
	loadedSeq uint64
}

func (m *Mod) loadRuns(ctx context.Context) ([]*botRun, error) {
	return m.loadRunsWhere(ctx, "", nil)
}

// loadRun hydrates one pilot exactly as a sim pass would. The panel uses it to
// price a decision the way the bot would (espfactory.go), not against default
// traits. It returns an error for a character that is not an enabled rolebot.
func (m *Mod) loadRun(ctx context.Context, charID int64) (*botRun, error) {
	runs, err := m.loadRunsWhere(ctx, " AND c.characterID = ?", []any{charID})
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("character %d is not an enabled rolebot", charID)
	}
	return runs[0], nil
}

func (m *Mod) loadRunsWhere(ctx context.Context, where string, args []any) ([]*botRun, error) {
	// The gang column is a correlated subquery, not a join: a character may sit in
	// several gang rows and a join would give two botRuns for one pilot. MIN() matches gangForChar (gangs.go).
	seq := rosterLoadSeq() // before the read: stateepoch.go
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName, c.corporationID,
		       c.solarSystemID, c.stationID, COALESCE(ce.locationID, 0),
		       COALESCE((SELECT MIN(gm.gangID)
		                 FROM rolebot_gang_members gm
		                 JOIN rolebot_gangs g ON g.gangID = gm.gangID AND g.enabled = 1
		                 WHERE gm.characterID = c.characterID), 0),
		       COALESCE((SELECT k.bossCharID
		                 FROM rolebot_conscripts k
		                 WHERE k.characterID = c.characterID), 0),
		       COALESCE(c.securityRating, 0),
		       COALESCE(n.factionID, rc.allegiance, 0)
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		LEFT JOIN entity ce ON ce.itemID = c.characterID
		LEFT JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
		LEFT JOIN rolebot_corps rc ON rc.corporationID = c.corporationID
		WHERE b.enabled = 1`+where+`
		ORDER BY c.characterID`, args...)
	if err != nil {
		return nil, fmt.Errorf("loadRuns: %w", err)
	}
	defer rows.Close()
	var out []*botRun
	// The character entity sits inside its boarded ship; a station location means the
	// char is unboarded in a hangar. Ship ownership is confirmed in one query afterwards (bulkload.go).
	shipCand := map[int64]int64{}
	for rows.Next() {
		b := &botRun{loadedSeq: seq}
		var charLoc int64
		if err := rows.Scan(&b.CharID, &b.Name, &b.CorpID, &b.SystemID, &b.StationID,
			&charLoc, &b.GangID, &b.BossID, &b.SecStatus, &b.FactionID); err != nil {
			return nil, err
		}
		if charLoc != 0 && charLoc != b.StationID {
			shipCand[b.CharID] = charLoc
		}
		rosterCorpOf.Store(b.CharID, b.CorpID)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := m.hydrateRuns(ctx, out, shipCand); err != nil {
		return nil, err
	}
	return out, nil
}

// rosterCorpOf is every rostered pilot's corporation as the roster query
// last read it: the answer charCorpID gives for a bot without a query.
var rosterCorpOf sync.Map // charID to corpID

func init() { registerCharForget("rosterCorpOf", func(id int64) { rosterCorpOf.Delete(id) }) }

// advance runs one FSM step. Returns whether the bot changed location (so the
// caller can resync presence). Dispatch is a single registry lookup (task.go),
// and a phase nothing claims is a loud failure instead of a quiet reset.
func (m *Mod) advance(ctx context.Context, b *botRun) (bool, error) {
	st := b.State

	// A finished rung oven comes off the line here, not in plan(), which runs from idle
	// only and would strand the job for an hour (industry.go). Free when no job is cooking.
	m.industryOvenSweep(ctx, b)

	// The same for a rung lab line (labwork_oven.go): a research job needs no pilot
	// standing by, so there is usually nothing left to fly for.
	m.labOvenSweep(ctx, b)

	// A launch that is over stops being a launch: setInSpace parks the ship in the bay
	// for the burn out of the door, and a projection built from that row would be born there.
	m.settleUndock(ctx, b)

	// A Sales member at an office lists the library's copies marked for sale (corpsalesprints.go).
	m.salesPrintChore(ctx, b)

	// A fleet this pilot belongs to is gathering. Like the oven sweep it runs here, not
	// in plan(), which an errand-bound pilot does not reach before the muster deadline.
	if m.gangCallUp(ctx, b) {
		return false, nil
	}

	// A player's fleet has spoken (conscript.go). Here for the same reason, and ahead
	// of the leg wait, since a human watching chat has less patience. Free: b.BossID is on the roster row.
	if m.conscriptCallUp(ctx, b) {
		return false, nil
	}

	// Shot on an undock, the projection asked to dock back (gank_undock.go).
	// Ahead of the leg wait: the pilot is mid-errand by definition.
	if m.redockCallUp(ctx, b) {
		return false, nil
	}

	// Waiting on a timed leg? Not while lasers run against a leg's debited load: that
	// load is settled on the next step, not when the abstract timer would have run out.
	if st.Data.LegEndsAt > clock.Unix() && !m.owesDebitedLoad(b) {
		return false, nil
	}

	def, ok := taskDef(st.Phase)
	if !ok {
		// A phase no task claims: say so and re-plan, so a removed task is not mistaken
		// for a bot that finished its work.
		m.logBot(b.CharID, logError, "sim",
			"no task registered for phase %q — re-planning (an old or removed task, or a bug)", st.Phase)
		st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	return def.Step(m, ctx, b)
}

// owesDebitedLoad reports a mining or ice leg whose rocks were debited by the abstract
// tier while the pilot's projection is now running real lasers on grid.
func (m *Mod) owesDebitedLoad(b *botRun) bool {
	d := &b.State.Data
	switch TaskKind(b.State.Phase) {
	case "mining.mine", TaskGangMine:
		if d.OreUnits == 0 {
			return false
		}
	case TaskIceWork:
		if len(d.IceLots) == 0 {
			return false
		}
	default:
		return false
	}
	return m.embodiedMining(b.CharID)
}

// travelDockStep is the last leg of a journey to a station: sit off the
// door long enough to be seen, then dock.
func (m *Mod) travelDockStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	// A player may have arrived since the approach began: put a hull on the grid
	// before the hangar takes it, as the ship is already parked at the door.
	m.embodyIfWatched(ctx, b)
	// Docking clearance: a projection at the door files its request now, so the
	// station's 4-6 s runs alongside the burn-in (before embodyDockPrep).
	m.embodyDockClearanceWait(b.CharID, b.ShipID, st.Data.TargetStationID)
	// A projection on grid flies the last leg itself: it warps to the station and docks
	// from there, instead of the ship vanishing from where it was working.
	if m.embodyDockPrep(b.CharID, st.Data.TargetStationID) {
		st.Data.LegEndsAt = clock.Unix() + navLegHoldSec
		return false, saveState(ctx, m.db, st)
	}
	// At the door; hold until traffic control clears it, like a player.
	if wait := m.embodyDockClearanceWait(b.CharID, b.ShipID, st.Data.TargetStationID); wait > 0 {
		st.Data.LegEndsAt = clock.Unix() + wait
		return false, saveState(ctx, m.db, st)
	}
	// Crimewatch: a recent PvP act keeps the doors shut, and once the doors
	// open the ball stops fighting (crimewatch.go departCommit).
	if !m.departCommit(b.CharID) {
		st.Data.LegEndsAt = clock.Unix() + crimewatchRetrySec
		return false, saveState(ctx, m.db, st)
	}
	// The approach leg is over: dock for real.
	if err := m.dockAt(ctx, b, st.Data.TargetStationID); err != nil {
		m.departAborted(b.CharID)
		return false, err
	}
	st.Data.LegEndsAt = clock.Unix() + dockSec
	st.Phase = st.Data.NextPhase
	st.Data.Path = nil
	return true, saveState(ctx, m.db, st)
}

// boardStep is the arrival at wherever an activity's ship is parked: get into it,
// then carry on with that activity. One step serves every activity, so a new
// activity needs no phase of its own.
func (m *Mod) boardStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	activity := st.Activity
	if activity == "" {
		st.Phase = string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}

	a, _, err := m.shipForActivity(ctx, b.CharID, activity)
	if err != nil {
		return false, err
	}
	if a == nil || a.ShipItemID == 0 || !a.ShipExists {
		return false, fmt.Errorf("%s ship vanished before boarding", activity)
	}
	if err := m.boardShip(ctx, b, a.ShipItemID); err != nil {
		return false, err
	}

	// A gang member picked its ship up on gang business: hand the freshly boarded
	// pilot back to planning, where the gang reclaims it, instead of a solo trip.
	if g, _, err := gangForChar(ctx, m.db, b.CharID); err == nil && g != nil && g.Activity != "" {
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}

	if !implementedActivity(activity) {
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}
	// A planner that refuses after boarding is world news, not a broken bot: drop to
	// idle and let the appetite draw find other work, instead of retrying into a halt.
	if err := m.planActivity(ctx, b, activity); err != nil {
		m.logBot(b.CharID, logInfo, "sim",
			"%s did not start after boarding (%v) — looking for something else to do", activity, err)
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	return false, nil
}

// planActivity dispatches to the planner behind one activity slug.
func (m *Mod) planActivity(ctx context.Context, b *botRun, activity string) error {
	def, ok := activityDefFor(activity)
	if !ok {
		return fmt.Errorf("activity %q has no planner", activity)
	}
	return def.Plan(m, ctx, b)
}

// plan picks what an idle bot does next: the pre-emption ladder
// (plan_ladder.go), then gang duty, then whatever its own appetites choose
// (appetite.go).
func (m *Mod) plan(ctx context.Context, b *botRun) error {
	st := b.State
	now := clock.Unix()

	// The last pass's reason for idling is stale from here on: whichever branch leaves
	// the pilot idle again writes its own.
	why := st.Data.IdleWhy
	st.Data.IdleWhy = ""

	// First, a docked pilot with no ship at all gets one (rookie.go): everything
	// below assumes the pilot has something to undock in.
	m.ensureStarterHull(ctx, b)

	// Minerals still aboard between jobs belong to no trip: a job that ended away from
	// its unload (an abandoned industry project, a broken-off carry) left them there.
	// On the hangar floor the mineral sweep sells them; left in the hold they fill every
	// later mining leg. Every trip that carries minerals runs in a phase of its own, so
	// none of them passes through here.
	if n := m.unloadMinerals(ctx, b); n > 0 {
		m.logBot(b.CharID, logInfo, "home",
			"unloaded %d unit(s) of minerals left in the hold by an earlier job into its hangar", n)
	}

	// The pre-emptions, in order (plan_ladder.go): ride home, moving house, clone, repair,
	// stranded-capital collections, corporation errands, a player's fleet; one ends the pass.
	if m.runLadder(ctx, b) {
		return nil
	}

	// Near the end of its hours a docked solo pilot starts nothing new: it
	// stays in the hangar and logs off there (timezone.go). It naps between looks,
	// so the ladder above runs once per idleRetrySec instead of on every pass, and
	// says so once per wind-down.
	if b.StationID != 0 && b.GangID == 0 && b.BossID == 0 && tzWindingDown(b.CharID) {
		if why != idleWhyWindDown {
			m.logBot(b.CharID, logInfo, "presence", "%s", idleWhyWindDown)
		}
		// The session that just ended is tallied here, as planSolo does: the cleared
		// activity is saved, so the next plan could not.
		settleWork(&st.Data, st.Activity, now, b.Tune.SatiationHalfLifeSec)
		st.Activity, st.Phase = "", "idle"
		st.Data.IdleWhy = idleWhyWindDown
		st.Data.LegEndsAt = now + idleRetrySec
		return saveState(ctx, m.db, st)
	}

	// Gang duty comes before the pilot's own rotation. A pilot excused for lacking
	// gear (gangSeatReady) is skipped until its memo lapses, which breaks the excuse loop.
	if g, mem, err := gangForChar(ctx, m.db, b.CharID); err != nil {
		m.logBot(b.CharID, logError, "gang", "membership lookup: %v", err)
	} else if g != nil && g.Activity != "" && now >= st.Data.GangUnfitUntil {
		err := m.gangPlan(ctx, b, g, mem)
		// A gangmate that cannot pay for its seat asks the pack first (broke.go): keeping
		// the member in the op is worth more to the gang than the ISK.
		if errors.Is(err, errCannotAfford) && m.gangWhipRound(ctx, b, g, err) {
			err = m.gangPlan(ctx, b, g, mem)
		}
		// Still short and nobody could cover it: the member earns on its own and rejoins
		// on a later pass.
		if errors.Is(err, errCannotAfford) && m.brokeEarn(ctx, b, []string{g.Activity}) {
			// A commander sent off to earn strands the fleet, so the lead is handed over.
			// See docs/comment-notes-b17.md#commander-earns.
			m.handOverGangLead(ctx, b.CharID, "it cannot fund the op")
			return nil
		}
		if err != nil {
			st.Activity, st.Phase = "", "idle"
			st.Data = idleData(st.Data)
			st.Data.LegEndsAt = now + idleRetrySec
			st.Data.IdleWhy = idleWhyRefused([]string{g.Activity + " with its gang: " + err.Error()})
			// The same fault-vs-world split planSolo's tail makes: a world refusal sleeps,
			// only a real fault burns halt credit.
			if refusalIsPassOver(err) {
				m.logBot(b.CharID, logInfo, "gang", "%s plan: %v (idle-retry in %ds)", g.Activity, err, idleRetrySec)
				return saveState(ctx, m.db, st)
			}
			m.logBot(b.CharID, logError, "gang", "%s plan: %v (retrying in %ds)", g.Activity, err, idleRetrySec)
			if serr := saveState(ctx, m.db, st); serr != nil {
				return serr
			}
			m.noteFailure(ctx, st, "gang", g.Activity+" plan: "+err.Error())
			return nil
		}
		m.clearFailure(ctx, st)
		return nil
	}

	return m.planSolo(ctx, b)
}

// idleWhyWindDown is the idle reason of a pilot in the last tzWindDownMin of its hours,
// and the line it logs once when the wind-down starts.
const idleWhyWindDown = "winding down — its hours are nearly over, so it starts nothing new"

// idleWhyMax bounds the idle reason: it rides in every save of the state blob.
const idleWhyMax = 240

// idleWhyRefused is the idle reason of a pilot whose every pick was refused, or which
// had nothing worth picking. The refusals read as the log lines do: "label: cause".
func idleWhyRefused(refused []string) string {
	if len(refused) == 0 {
		return "nothing it wants to do is on offer right now"
	}
	return trunc("nothing would start — "+joinAnd(refused), idleWhyMax)
}

// refusalIsPassOver separates refusals that describe the world from ones that
// describe a broken bot. Each of these already has its own remedy running, and
// freezing the pilot adds nothing; anything not in the set (a dead database, a
// failed save) still burns halt credit. errNoRoute is a rules or geography refusal.
func refusalIsPassOver(err error) bool {
	return errors.Is(err, errNoRoute) ||
		errors.Is(err, errNoFitAvailable) ||
		errors.Is(err, errCannotAfford) ||
		errors.Is(err, errAllBeltsHopeless) ||
		errors.Is(err, errNoHuntingGround)
}

// planSolo is the pilot's own mind, used when no gang claims it. The pilot prices
// every activity (appetite.go) from its traits, the world and how worn thin that
// work is, and draws from the scores. An out-of-character pick is possible by design.
func (m *Mod) planSolo(ctx context.Context, b *botRun) error {
	st := b.State
	now := clock.Unix()

	// Fold the session that just ended into the satiation tally.
	settleWork(&st.Data, st.Activity, now, b.Tune.SatiationHalfLifeSec)
	st.Activity = ""

	// A corporation kit lent for an op that is over goes back first
	// (corparmory_issue.go), before the pilot may change employer.
	if handled, err := m.armoryReturnResume(ctx, b); err != nil || handled {
		return err
	}

	// Between jobs is when a pilot thinks about who it works for (corpjoin.go): a
	// switch-gated, docked-only, rate-limited check. A change of employer takes no time.
	if _, err := m.corpReview(ctx, b); err != nil {
		m.logBot(b.CharID, logError, "corp", "reviewing its employment: %v", err)
	}

	// A criminal timer outranks every appetite: the pilot cannot safely be near a
	// station in empire space until the flag lapses (piracy.go).
	if until := m.piracyHeatUntil(b); until > now {
		return m.piracyGoToGround(ctx, b, until)
	}

	// An in-flight industry project is sunk capital and resumes deterministically
	// (industry.go). A cooking job reports unhandled: the pilot is free until it rings.
	if handled, err := m.industryResume(ctx, b); err != nil {
		m.logBot(b.CharID, logError, "industry", "resume: %v", err)
	} else if handled {
		return nil
	}
	// A hauler grant from the corporation is spent before the draw (gangsupply.go):
	// the ISK was handed over for one purpose.
	if handled, err := m.haulerGrantResume(ctx, b); err != nil {
		m.logBot(b.CharID, logError, "gang", "hauler grant: %v", err)
	} else if handled {
		return nil
	}
	// An agent mission in hand comes back before the draw (missions.go): its clock is running,
	// and a draw that picked other work would leave it to expire at a standing loss.
	if handled, err := m.missionHeldResume(ctx, b); err != nil || handled {
		return err
	}

	// The home loop's seam (homeshop.go). Everything above outranks it; the draw below
	// never runs away from home: the pilot flies back, shops and picks from its own hangar.
	if HomeLoop() {
		if handled, err := m.goHomeAndShop(ctx, b); err != nil || handled {
			return err
		}
	}

	// Between jobs, docked, a pilot also puts unfitted hangar modules back into the
	// economy (strays.go); throttled inside.
	m.sellHangarStrays(ctx, b)
	m.resellSurplusPrints(ctx, b)
	// The rock (orestrays.go) is sold before the refined pile (mineralstrays.go): the rock
	// sweep's first arm refines, so the minerals are on the floor for the mineral sweep.
	m.sellHangarOre(ctx, b)
	m.sellHangarMinerals(ctx, b)
	// The sweep of last resort (liquidate.go) runs after both: it sells whatever the
	// hangar holds, only for a pilot left below the wallet floor.
	m.liquidateForCash(ctx, b)

	// Lab housekeeping (labwork.go): bring a stray blueprint home. Starting a research
	// job is the research trip of the industry umbrella (industrytrip.go), not done here.
	if handled, err := m.labworkStart(ctx, b); err != nil {
		m.logBot(b.CharID, logError, "lab", "starting a research job: %v", err)
	} else if handled {
		return nil
	}

	scores := m.scoreActivities(ctx, b)

	// The draw, retried past anything that will not start: a refusal zeroes the option
	// and the pilot takes the next thing it feels like, giving up only when its whole
	// appetite has been tried.
	var refused []string
	// The subset of refused that is a fault rather than a fact about the world: only
	// these are worth a failure report (see refusalIsPassOver).
	var hardRefused []string
	var moneyRefused []string
	// The cheapest money refusal of the pass, kept for the gang whip-round.
	var cheapestGap float64
	var gapErr error
	var gapSlug string
	for tries := 0; tries < len(scores); tries++ {
		pick, ok := pickActivity(scores, b.Tune)
		if !ok {
			break
		}
		m.logBot(b.CharID, logDebug, "sim", "%s", appetiteSummary(pick, scores))
		err := m.planActivity(ctx, b, pick.Def.Slug)
		if err == nil {
			// Planned: stamp the session for the satiation tally.
			st.Data.LastActivity = pick.Def.Slug
			st.Data.ActivityStartedAt = now
			m.clearFailure(ctx, st)
			return nil
		}
		// Both arms are the same event, a refused activity: the loop goes on to the next
		// one, so they are logged the same way.
		if errors.Is(err, errNoFitAvailable) {
			m.logBot(b.CharID, logInfo, "sim",
				"%s passed over (%v) — looking for something else to do", pick.Def.Label, err)
		} else {
			m.logBot(b.CharID, logInfo, "sim",
				"%s did not start (%v) — looking for something else to do", pick.Def.Label, err)
		}
		refused = append(refused, pick.Def.Label+": "+err.Error())
		if !refusalIsPassOver(err) {
			hardRefused = append(hardRefused, pick.Def.Label+": "+err.Error())
		}
		if errors.Is(err, errCannotAfford) {
			moneyRefused = append(moneyRefused, pick.Def.Label)
			// Remember the smallest gap of the pass: the gang should be asked for the least
			// that gets this pilot working again.
			if gap := shortfallOf(err); gap > 0 && (cheapestGap == 0 || gap < cheapestGap) {
				cheapestGap, gapErr, gapSlug = gap, err, pick.Def.Slug
			}
		}
		for i := range scores {
			if scores[i].Def.Slug == pick.Def.Slug {
				scores[i].Score = 0
			}
		}
	}

	// Everything it wanted was refused over money: a pilot with packmates asks them
	// first (broke.go), funding only the cheapest refusal.
	if len(moneyRefused) > 0 && b.GangID != 0 && gapErr != nil {
		if g, _, err := gangForChar(ctx, m.db, b.CharID); err == nil && g != nil &&
			m.gangWhipRound(ctx, b, g, gapErr) {
			if perr := m.planActivity(ctx, b, gapSlug); perr == nil {
				st.Data.LastActivity = gapSlug
				st.Data.ActivityStartedAt = now
				m.clearFailure(ctx, st)
				return nil
			} else {
				m.logBot(b.CharID, logInfo, "sim",
					"the gang covered it and %s still would not start (%v)", gapSlug, perr)
			}
		}
	}

	// Nobody to ask, or nobody with anything spare: the pilot is sent to earn instead
	// of idling into a halt (broke.go).
	if len(moneyRefused) > 0 && m.brokeEarn(ctx, b, moneyRefused) {
		return nil
	}

	// Nothing runnable at all: say why, and sleep.
	for _, s := range scores {
		if s.Opp == 0 {
			// The labels are gerunds (trading, manufacturing), so the message says no X for it
			// rather than cannot X.
			m.logBot(b.CharID, logInfo, "sim", "no %s for it right now: %s", s.Def.Label, s.Why)
		}
	}
	st.Activity, st.Phase = "", "idle"
	st.Data = idleData(st.Data)
	st.Data.LegEndsAt = now + idleRetrySec
	st.Data.IdleWhy = idleWhyRefused(refused)
	if err := saveState(ctx, m.db, st); err != nil {
		return err
	}
	// Every kind of work refused this pilot. A pass of only world refusals is an idle
	// afternoon; any real fault counts toward the halt threshold and is all that is reported.
	if len(hardRefused) > 0 {
		m.logBot(b.CharID, logError, "sim",
			"nothing it felt like doing would start (retrying in %ds) — %s", idleRetrySec, joinAnd(hardRefused))
		m.noteFailure(ctx, st, "sim", "nothing would start — "+joinAnd(hardRefused))
	} else if len(refused) > 0 {
		m.logBot(b.CharID, logInfo, "sim",
			"nothing it felt like doing would start (idle-retry in %ds) — %s", idleRetrySec, joinAnd(refused))
	}
	return nil
}

// ── travel ──────────────────────────────────────────────────────────────────

// beginTravel routes the bot to a system and optionally a station there.
// nextPhase is entered on arrival (after docking when stationID != 0).
func (m *Mod) beginTravel(ctx context.Context, b *botRun, toSystem, toStation int64, nextPhase string) error {
	st := b.State

	// Already docked exactly where this is going? Skip the undock/redock.
	if toStation != 0 && b.StationID == toStation {
		st.Phase = nextPhase
		st.Data.Path = nil
		st.Data.TargetSystemID = toSystem
		st.Data.TargetStationID = toStation
		st.Data.NextPhase = ""
		return saveState(ctx, m.db, st)
	}

	// A hauler's rules price the week's gate violence into the road
	// (gatekills.go); every other hull flies the plain rules.
	path, err := m.route(ctx, b.SystemID, toSystem, m.travelRulesFor(ctx, b))
	if err != nil {
		return err
	}
	st.Phase = "travel"
	st.Data.Path = path
	// Stamped once for the whole journey: a ship entity's typeID is immutable, so an
	// unwatched leg reads a float from state it already holds.
	st.Data.AlignSec = m.shipAlignSec(ctx, b.ShipID)
	st.Data.TargetSystemID = toSystem
	st.Data.TargetStationID = toStation
	st.Data.NextPhase = nextPhase
	// A small per-bot jitter, deterministic from charID, so bots sharing a route segment
	// do not all hit their first travelStep in the same pass.
	st.Data.LegEndsAt = clock.Unix() + int64((uint64(b.CharID)*0x9E3779B97F4A7C15>>48)%7)

	// Undock first when docked, if the station will release it. The trip counts as
	// begun whatever happens here; the wave gate only decides when travelStep lets the ship out.
	undocked := false
	if b.StationID != 0 {
		if m.admitUndockFor(ctx, b) {
			if err := m.setInSpace(ctx, b); err != nil {
				return err
			}
			st.Data.LegEndsAt = undockLegEnd(&st.Data)
			undocked = true
		} else {
			st.Data.LegEndsAt = clock.Unix() + undockRetryDelay()
		}
	}
	if err := saveState(ctx, m.db, st); err != nil {
		return err
	}
	// The ball goes out now, not on the next reconcile: embodyNow reads the undock marker
	// saveState just wrote, and waiting would show the ship appearing already coasting away.
	if undocked && m.systemProjected(b.SystemID) {
		m.embodyNow(ctx, b.CharID)
	}
	return nil
}

// travelStep advances one gate jump (or completes travel).
func (m *Mod) travelStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State

	// Still in the hangar with a trip planned: the wave gate has not let this one out
	// yet (beginTravel). Ask again; every branch below assumes a ship in space.
	if b.StationID != 0 {
		if !m.admitUndockFor(ctx, b) {
			st.Data.LegEndsAt = clock.Unix() + undockRetryDelay()
			return true, saveState(ctx, m.db, st)
		}
		if err := m.setInSpace(ctx, b); err != nil {
			return false, err
		}
		st.Data.LegEndsAt = undockLegEnd(&st.Data)
		if err := saveState(ctx, m.db, st); err != nil {
			return false, err
		}
		if m.systemProjected(b.SystemID) {
			m.embodyNow(ctx, b.CharID)
		}
		return true, nil
	}

	if len(st.Data.Path) == 0 {
		// Arrived in the target system. A station destination gets an approach leg, so the
		// bot sits off the station long enough to be seen (and embodied) before it docks.
		if st.Data.TargetStationID != 0 && b.StationID == 0 {
			st.Phase = "travel.dock"
			st.Data.Path = nil
			st.Data.LegEndsAt = clock.Unix() + dockApproachSec
			// Saved before the projection is asked for: desiredProjections reads the pilot's
			// posture back from the state row, and the old phase would build it mining.
			if err := saveState(ctx, m.db, st); err != nil {
				return false, err
			}
			// Asked for before parkAtStationDoor, which teleports the entity row to the door:
			// embodying after it would materialise the ship at the door instead of warping in.
			embodied := m.embodyIfWatched(ctx, b)
			if b.ShipID != 0 && !embodied {
				// Nobody to see it: the abstract tier owns the coordinate, and only here, since
				// a live projection owns its own.
				m.parkAtStationDoor(ctx, b, st.Data.TargetStationID)
				return true, nil
			}
			// An embodied bot's approach is the warp-in and the burn to the door, which
			// travel.dock drives, so it needs no fixed timer first.
			st.Data.LegEndsAt = clock.Unix() + navLegHoldSec
			return true, saveState(ctx, m.db, st)
		}
		st.Phase = st.Data.NextPhase
		st.Data.Path = nil
		return false, saveState(ctx, m.db, st)
	}
	next := st.Data.Path[0]
	// Fly the gate for real when someone is watching, then let the abstract tier jump
	// the bot. The gate wave is asked at the jump, after the flight. See docs/comment-notes-b17.md#gate-wave-order.
	destGateID := m.resolveDestGate(ctx, b.SystemID, next)
	convoy := false
	switch jumpDecision(m.embodyFlyToGate(b.CharID, next), func() bool {
		// The convoy first (fleetmove.go): a fleet member waits at the gate for packmates still
		// behind it on the same trip, so the fleet crosses together.
		if m.convoyHolds(ctx, b) {
			convoy = true
			return false
		}
		// Crimewatch first among the rest, so a locked pilot spends no wave slot.
		if crimewatchLocked(b.CharID) {
			return false
		}
		if destGateID != 0 && !m.admitGateJumpFor(ctx, b, next, destGateID) {
			return false
		}
		// Admitted: the ball stops fighting before it jumps (departCommit).
		return m.departCommit(b.CharID)
	}) {
	case jumpFlying:
		st.Data.LegEndsAt = clock.Unix() + navLegHoldSec
		return false, saveState(ctx, m.db, st)
	case jumpHeld:
		if convoy {
			st.Data.LegEndsAt = clock.Unix() + convoyRetrySec
		} else {
			st.Data.LegEndsAt = clock.Unix() + gateRetryDelay()
		}
		return true, saveState(ctx, m.db, st)
	}
	m.embodyJumpFX(b.CharID, next)
	st.Data.ConvoyHoldSince = 0
	st.Data.Path = st.Data.Path[1:]
	if err := m.setSystem(ctx, b, next); err != nil {
		m.departAborted(b.CharID)
		return false, err
	}
	// The abstract jump timer stands in for a flight nobody can see. In a projected
	// system (a ring, not only a watched one) the flight is real, so the bot only pauses on arrival.
	watched := m.systemProjected(next)
	if watched {
		st.Data.LegEndsAt = clock.Unix() +
			int64(navGateDwellMinSec+rand.Intn(navGateDwellMaxSec-navGateDwellMinSec+1))
	} else {
		st.Data.LegEndsAt = clock.Unix() + unwatchedLegSec(st.Data.AlignSec)
	}
	if err := saveState(ctx, m.db, st); err != nil {
		return false, err
	}
	// Land the ball on the far side now, as the undock does: setSystem wrote the
	// arrival gate that embodyNow reads, and waiting for the grid lane would leave an empty grid.
	// With nobody on the far side the ball it left behind still goes now, right after its jump flash.
	if watched {
		m.embodyNow(ctx, b.CharID)
	} else {
		m.withdrawProjection(b.CharID)
	}
	return true, nil
}

// ── location writers ────────────────────────────────────────────────────────

// setSystem moves the bot (character + boarded ship) into a system, in
// space, landing at the stargate it came through so the embodied tier
// materialises it where a jump would actually put it.
func (m *Mod) setSystem(ctx context.Context, b *botRun, sysID int64) error {
	fromSys := b.SystemID
	constID, regID, err := evedb.GetSolarSystemLocation(ctx, m.db, sysID)
	if err != nil {
		return err
	}
	if err := evedb.UpdateCharacterStation(ctx, m.db, b.CharID, 0, sysID, constID, regID); err != nil {
		return err
	}
	if b.ShipID != 0 {
		if err := evedb.UpdateEntityLocation(ctx, m.db, b.ShipID, sysID); err != nil {
			return err
		}
		if fromSys != sysID {
			// Remember which gate it came out of: a projection built here
			// flashes it, the way a real jump-in does.
			b.State.Data.ArriveGateID = m.parkAtGate(ctx, b, sysID, fromSys)
			b.State.Data.ArriveAt = clock.Unix()
		}
	}
	if fromSys != sysID && fromSys != 0 {
		// The star map's jump overlay (map.GetHistory type 3) counts arrivals at the
		// destination system, as the player StargateJump path does.
		if err := evedb.RecordMapEvent(ctx, m.db, sysID, evedb.StatJump, 1); err != nil {
			rlogf("rolebots: record map jump %d->%d: %v", fromSys, sysID, err)
		}
		// The module's own ledger counts the same jump for the trade planner: which lanes
		// the roster actually flies (traffic.go).
		m.recordJump(ctx, fromSys, sysID)
		// And against the pilot (statcount.go): recordJump takes no character and its
		// tables decay, so neither can answer who has flown furthest.
		bumpStats(b.CharID, func(d *statDelta) { d.Jumps++ })
	}
	b.SystemID, b.StationID = sysID, 0
	return nil
}

// setInSpace undocks the bot in its current system, leaving its ship in the
// station's dock entry where a real undock puts it, and notes the undock so a
// projection built in the next seconds launches out of the door under thrust.
func (m *Mod) setInSpace(ctx context.Context, b *botRun) error {
	station := b.StationID
	if err := m.setSystem(ctx, b, b.SystemID); err != nil {
		return err
	}
	if b.ShipID == 0 || station == 0 {
		return nil
	}
	// The slot is claimed once, here, and carried on the state row: the persisted ship
	// position and the projection's start must be the same coordinate.
	slot := claimUndockSlot(station, b.ShipID, undockHoldSec()+dockApproachSec)
	runSec := UndockRunSec()
	if pos, _, _, sec, ok := m.dockExit(ctx, b.ShipID, b.CharID, station, slot); ok {
		runSec = sec
		// Jitter is the last resort when two ships share a slot (the registry is empty after
		// a restart). It is reproducible from the shipID so prepareProjection recomputes this point.
		pos = pos.Add(parkJitter(b.ShipID))
		if err := evedb.UpdateEntityPosition(ctx, m.db, b.ShipID, pos.X, pos.Y, pos.Z); err != nil {
			rlogf("rolebots: park at dock exit ship %d: %v", b.ShipID, err)
		}
	} else {
		// No authored door: hand the slot back instead of holding one at a station this ship
		// will not leave through (parkAtCelestial claims its own).
		releaseUndockSlot(station, b.ShipID)
		m.parkAtCelestial(ctx, b.ShipID, station)
	}
	now := clock.Unix()
	b.State.Data.UndockStationID = station
	b.State.Data.UndockAt = now
	b.State.Data.UndockSlot = slot
	b.State.Data.UndockRunUntil = now + runSec
	b.State.Data.UndockLoiterUntil = 0
	b.State.Data.UndockLoiterStationID = 0
	// A share of departures circle the station before going anywhere (undockwave.go),
	// decided here so both tiers agree; the station is kept as the loiter's anchor.
	if loiter := undockLoiterFor(b.CharID, now); loiter > 0 {
		b.State.Data.UndockLoiterUntil = b.State.Data.UndockRunUntil + loiter
		b.State.Data.UndockLoiterStationID = station
	}
	return nil
}

// dockExit resolves where a ship undocking from stationID at lane slot slot is placed,
// its exit velocity and the point it burns toward, all from destiny.UndockPlacement (the
// call initDestiny makes), so a bot and a player leave one door alike. ok=false means no
// authored dock entry. runSec comes from the geometry (UndockRunSecFor).
func (m *Mod) dockExit(ctx context.Context, shipID, charID, stationID int64, slot int) (pos, vel, goTo destiny.Vec3, runSec int64, ok bool) {
	stats, err := evedb.GetShipStats(ctx, m.db, shipID, charID, stationID)
	if err != nil {
		rlogf("rolebots: dock exit station %d: %v", stationID, err)
		return destiny.Vec3{}, destiny.Vec3{}, destiny.Vec3{}, UndockRunSec(), false
	}
	pos, vel, goTo, ok = destiny.UndockPlacement(
		destiny.Vec3{X: stats.StationX, Y: stats.StationY, Z: stats.StationZ},
		destiny.Vec3{X: stats.DockEntryX, Y: stats.DockEntryY, Z: stats.DockEntryZ},
		destiny.Vec3{X: stats.DockOrientationX, Y: stats.DockOrientationY, Z: stats.DockOrientationZ},
		stats.Radius, stats.MaxVelocity, slot,
	)
	if !ok {
		return pos, vel, goTo, UndockRunSec(), false
	}
	return pos, vel, goTo, UndockRunSecFor(goTo.Sub(pos).Len(), stats.MaxVelocity), true
}

// ── where the ship physically sits ──────────────────────────────────────────
//
// The embodied tier materialises a projection at the ship's persisted coordinates, so
// the ship is parked at the right celestial on every transition.

const parkStandoff = 15_000.0 // metres off the object's surface

// parkAtGate puts the ship where a jump into sysID from fromSys lands it: 15 km beyond
// the arrival gate along the line the two gates make, as for a player's StargateJump.
// Falls back to any gate in the system. Returns the arrival gate's itemID (0 if none).
func (m *Mod) parkAtGate(ctx context.Context, b *botRun, sysID, fromSys int64) int64 {
	gateID := m.gateBetween(ctx, sysID, fromSys)
	if gateID == 0 {
		var x, y, z, radius float64
		if qerr := m.db.QueryRowContext(ctx, `
			SELECT itemID, COALESCE(x,0), COALESCE(y,0), COALESCE(z,0), COALESCE(radius,1)
			FROM mapDenormalize WHERE solarSystemID = ? AND groupID = 10 LIMIT 1`,
			sysID).Scan(&gateID, &x, &y, &z, &radius); qerr != nil {
			return 0
		}
		m.persistPosition(ctx, b.ShipID, gateID, x, y, z, radius)
		return gateID
	}
	// The jump line through the arrival gate (lane.go gateArrivalGeom, the geometry a
	// camp covering this gate parks on).
	gate, dir, radius, ok := m.gateArrivalGeom(ctx, gateID)
	if !ok {
		m.parkAtCelestial(ctx, b.ShipID, gateID)
		return gateID
	}
	landing := gate.Add(dir.Scale(radius + parkStandoff))
	// The arrival lane: the landing is a pure function of the gate pair, so without it
	// every bot jumping this link parks on one coordinate (parklane.go).
	landing = landing.Add(parkLaneOffset(gateID, b.ShipID, dir))
	m.writePosition(ctx, b.ShipID, landing)
	return gateID
}

// writePosition persists a ship's position, logging rather than failing: a
// bot's flight is not failed over a coordinate.
func (m *Mod) writePosition(ctx context.Context, shipID int64, p destiny.Vec3) {
	if err := evedb.UpdateEntityPosition(ctx, m.db, shipID, p.X, p.Y, p.Z); err != nil {
		rlogf("rolebots: park ship %d: %v", shipID, err)
	}
}

// stationStandoff resolves where a ship that is not undocking sits at a station: on
// the undock axis, clear of the hull, at the warp landing's distance
// (destiny.StationStandoffPoint). dockExit answers the bay, inside the model; everything
// that is not a live launch belongs here.
func (m *Mod) stationStandoff(ctx context.Context, shipID, charID, stationID int64, slot int) (destiny.Vec3, bool) {
	pos, orient, radius, ok := m.stationDoorGeom(ctx, shipID, charID, stationID)
	if !ok {
		return destiny.Vec3{}, false
	}
	return destiny.StationStandoffPoint(pos, orient, radius, parkStandoff, slot)
}

// stationDoorGeom reads the three things destiny's door arithmetic needs: where the
// station is, which way its bay faces, and how big its model is. The radius costs one
// mapDenormalize row, as GetShipStats.Radius is the ship's hull.
func (m *Mod) stationDoorGeom(ctx context.Context, shipID, charID, stationID int64) (pos, orient destiny.Vec3, radius float64, ok bool) {
	stats, err := evedb.GetShipStats(ctx, m.db, shipID, charID, stationID)
	if err != nil {
		rlogf("rolebots: station door geometry %d: %v", stationID, err)
		return destiny.Vec3{}, destiny.Vec3{}, 0, false
	}
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(radius,0) FROM mapDenormalize WHERE itemID = ?`,
		stationID).Scan(&radius); err != nil {
		rlogf("rolebots: station door radius %d: %v", stationID, err)
		return destiny.Vec3{}, destiny.Vec3{}, 0, false
	}
	return destiny.Vec3{X: stats.StationX, Y: stats.StationY, Z: stats.StationZ},
		destiny.Vec3{X: stats.DockOrientationX, Y: stats.DockOrientationY, Z: stats.DockOrientationZ},
		radius, true
}

// stationHullEscape answers the standoff only for a ship still inside the station's
// model, and refuses for one that has moved on. An undock marker lives undockGraceSec,
// long enough to have warped to a belt, and re-deriving from the marker alone would
// drag that ship back. A ship at the standoff is outside, so both callers are idempotent.
func (m *Mod) stationHullEscape(ctx context.Context, shipID, charID, stationID int64, slot int, cur destiny.Vec3) (destiny.Vec3, bool) {
	pos, orient, radius, ok := m.stationDoorGeom(ctx, shipID, charID, stationID)
	if !ok || radius <= 0 {
		return destiny.Vec3{}, false
	}
	if cur.Sub(pos).Len() >= radius {
		return destiny.Vec3{}, false // already clear of the hull, or long gone
	}
	return destiny.StationStandoffPoint(pos, orient, radius, parkStandoff, slot)
}

// shipPosition reads a ship's persisted coordinate. Zero is the module's
// "not set" sentinel throughout (see embarkPosition), and is reported as such.
func (m *Mod) shipPosition(ctx context.Context, shipID int64) (destiny.Vec3, bool) {
	var p destiny.Vec3
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
		shipID).Scan(&p.X, &p.Y, &p.Z); err != nil {
		return destiny.Vec3{}, false
	}
	return p, !p.IsZero()
}

// parkAtStationDoor puts the ship where a pilot waiting to dock sits: off the door on
// the undock axis, clear of the station's hull, not in the bay (dockExit is inside the
// model). parkAtCelestial is the fallback when the geometry cannot be resolved. It takes
// a lane slot so bots waiting at one hub do not stack on one coordinate.
func (m *Mod) parkAtStationDoor(ctx context.Context, b *botRun, stationID int64) {
	// No hull, nothing to park: a zero shipID is the lane registry's holder key, so
	// claiming with one would let every shipless pilot share one seat.
	if b.ShipID == 0 {
		return
	}
	// The claim is on the standoff lane (destiny.StandoffLaneObject), not the station's
	// own, and held for parkSlotHoldSec. See docs/comment-notes-b17.md#standoff-lane.
	lane := destiny.StandoffLaneObject(stationID)
	slot := claimUndockSlot(lane, b.ShipID, parkSlotHoldSec)
	if pos, ok := m.stationStandoff(ctx, b.ShipID, b.CharID, stationID, slot); ok {
		// Jitter for the same reason setInSpace carries one.
		pos = pos.Add(parkJitter(b.ShipID))
		if err := evedb.UpdateEntityPosition(ctx, m.db, b.ShipID, pos.X, pos.Y, pos.Z); err != nil {
			rlogf("rolebots: park at station door ship %d: %v", b.ShipID, err)
		}
		return
	}
	// Same as setInSpace's fallback: give the slot back, since parkAtCelestial
	// claims its own.
	releaseUndockSlot(lane, b.ShipID)
	m.parkAtCelestial(ctx, b.ShipID, stationID)
}

// settleUndock moves a ship off the docking bay once its launch is over. setInSpace
// writes the bay coordinate, which would otherwise stand for the whole leg and birth a
// projection inside the station. Called from advance() ahead of the leg wait and
// guarded on the marker, so it is free for everyone else (destiny.UndockRunSecFor).
func (m *Mod) settleUndock(ctx context.Context, b *botRun) {
	sd := &b.State.Data
	station := sd.UndockStationID
	if station == 0 || b.ShipID == 0 {
		return
	}
	// Back inside: dockAt clears the markers and releases the slot, so this is reachable
	// only if another path put the pilot in a hangar, where a new coordinate means nothing.
	if b.StationID != 0 {
		return
	}
	now := clock.Unix()
	if sd.UndockRunUntil > now {
		return // still burning out of the door; the bay is correct
	}
	// The loiter outlives the run, and undockLoiterCircle needs its station, carried on
	// its own field so clearing the launch marker cannot take the anchor.
	if sd.UndockLoiterUntil > now {
		sd.UndockLoiterStationID = station
	}
	// Only a ship still standing in the bay is moved: one that warped on and had
	// parkAtBelt write its coordinate just loses the marker (see stationHullEscape).
	if cur, ok := m.shipPosition(ctx, b.ShipID); ok {
		// Moved onto the standoff lane: the launch is over, and destiny keeps one seat per
		// holder, so claiming here gives the undock seat back with no release call to forget.
		lane := destiny.StandoffLaneObject(station)
		slot := claimUndockSlot(lane, b.ShipID, parkSlotHoldSec)
		if pos, ok := m.stationHullEscape(ctx, b.ShipID, b.CharID, station, slot, cur); ok {
			pos = pos.Add(parkJitter(b.ShipID))
			if err := evedb.UpdateEntityPosition(ctx, m.db, b.ShipID, pos.X, pos.Y, pos.Z); err != nil {
				rlogf("rolebots: settle undock ship %d: %v", b.ShipID, err)
				releaseUndockSlot(lane, b.ShipID)
				return // leave the marker so the next pass tries again
			}
		} else {
			releaseUndockSlot(lane, b.ShipID)
		}
	}
	sd.UndockStationID, sd.UndockAt, sd.UndockSlot = 0, 0, 0
	sd.UndockRunUntil = 0
	if err := saveState(ctx, m.db, b.State); err != nil {
		rlogf("rolebots: settle undock state %d: %v", b.CharID, err)
	}
}

// parkAtCelestial puts the ship just off a station, belt or gate by itemID.
func (m *Mod) parkAtCelestial(ctx context.Context, shipID, itemID int64) {
	var x, y, z, radius float64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0), COALESCE(radius,1)
		FROM mapDenormalize WHERE itemID = ?`, itemID).Scan(&x, &y, &z, &radius); err != nil {
		return
	}
	m.persistPosition(ctx, shipID, itemID, x, y, z, radius)
}

// parkAtBelt puts the ship on the belt of a system holding the most of the ore
// family the pilot came for, and reports which belt so the caller can debit its
// rocks (beltfield.go). familyID 0 asks for the fullest belt of any ore. Returns 0
// when the system has no belts.
func (m *Mod) parkAtBelt(ctx context.Context, shipID, sysID, familyID int64) int64 {
	beltID := m.beltWithMostOre(ctx, sysID, familyID)
	if beltID == 0 {
		return 0
	}
	var x, y, z, radius float64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0), COALESCE(radius,1)
		FROM mapDenormalize WHERE itemID = ?`, beltID).Scan(&x, &y, &z, &radius); err != nil {
		return 0
	}
	m.persistPosition(ctx, shipID, beltID, x, y, z, radius)
	return beltID
}

// persistPosition writes a standoff point near (x,y,z), offset toward the system's
// star. objectID keys the park lane (parklane.go): the standoff is a pure function of
// the object, so without a lane every bot parking here gets one coordinate. Pass 0
// only when there is no object to key on.
func (m *Mod) persistPosition(ctx context.Context, shipID, objectID int64, x, y, z, radius float64) {
	d := math.Sqrt(x*x + y*y + z*z)
	off := radius + parkStandoff
	// The standoff runs from the object back toward the star, so the lane extends along
	// that axis and each further slot is further clear of the object.
	dir := destiny.Vec3{X: -x, Y: -y, Z: -z}
	if d > off {
		s := (d - off) / d
		x, y, z = x*s, y*s, z*s
	}
	lane := parkLaneOffset(objectID, shipID, dir)
	x, y, z = x+lane.X, y+lane.Y, z+lane.Z
	if err := evedb.UpdateEntityPosition(ctx, m.db, shipID, x, y, z); err != nil {
		rlogf("rolebots: park ship %d: %v", shipID, err)
	}
}

// dockAt docks the bot at a station (it must be in that station's system; the
// router got it there).
func (m *Mod) dockAt(ctx context.Context, b *botRun, stationID int64) error {
	loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
	if err != nil {
		return err
	}
	if err := evedb.UpdateCharacterStation(ctx, m.db, b.CharID, stationID,
		loc.SolarSystemID, loc.ConstellationID, loc.RegionID); err != nil {
		return err
	}
	if b.ShipID != 0 {
		if err := evedb.UpdateEntityLocation(ctx, m.db, b.ShipID, stationID); err != nil {
			return err
		}
	}
	b.SystemID, b.StationID = loc.SolarSystemID, stationID
	// A docked pilot has no ball: take it off the grid in this step, as a player's dock does, and
	// not at the next roster-wide reconcile. See docs/comment-notes-b07.md#departure-teardown.
	m.withdrawProjection(b.CharID)
	// Inside the station, free the door seat for the next ship out (undockwave.go).
	// Release by holder, not by key: a docking ship may hold the standoff or the station's own seat.
	if b.ShipID != 0 {
		destiny.ReleaseHeldSlot(b.ShipID)
		// Docked: out of the station's clearance line (docking clearance,
		// embodyDockClearanceWait).
		destiny.ReleaseDockClearance(b.ShipID)
	}
	b.State.Data.UndockStationID = 0
	b.State.Data.UndockSlot = 0
	b.State.Data.UndockRunUntil = 0
	b.State.Data.UndockLoiterUntil = 0
	b.State.Data.UndockLoiterStationID = 0
	// Arriving is when a pilot tidies up: goods that reached the hangar by a path this
	// module does not own are folded into one row per type (stacking.go).
	m.tidyOnDock(ctx, b)
	// Footfall: one arrival at this dock, as a gate transit is one arrival in a system;
	// it is the whole guest term of the station score (stationscore.go).
	m.recordStationDock(ctx, stationID)
	bumpStats(b.CharID, func(d *statDelta) { d.Docks++ })
	return nil
}

// boardShip puts the character entity inside a ship entity. If the pilot is stepping
// out of a capsule the pod is deleted, since it is not an asset and would pile up in
// station inventories.
func (m *Mod) boardShip(ctx context.Context, b *botRun, shipID int64) error {
	// Snapshot the old ship before moving, so we can clean up a pod.
	oldShip := b.ShipID

	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET locationID = ? WHERE itemID = ?`, shipID, b.CharID); err != nil {
		return fmt.Errorf("board ship %d: %w", shipID, err)
	}
	b.ShipID = shipID

	// Delete the vacated capsule, only when docked (in space the entity is a projected
	// ball the embodiment layer tears down) and only when the old ship was a pod.
	if oldShip != 0 && oldShip != shipID && b.StationID != 0 && m.isCapsule(ctx, oldShip) {
		if err := evedb.DeleteEntity(ctx, m.db, oldShip); err != nil {
			rlogf("rolebots: drop orphan capsule %d for char %d: %v", oldShip, b.CharID, err)
		}
	}
	// Cover for the hull, bought at the dock it was boarded in (botinsure.go).
	m.maybeInsureHull(ctx, b, shipID)
	return nil
}

// boardHullInHangar boards a hull the pilot already owns at the station it is docked
// in, biggest cargo hold first, and reports whether it found one. b.ShipID is what is
// boarded, not what is owned. Nothing is fitted: for the right ship for a job use
// shipForActivity. Assembled hulls only (singleton = 1); capacity > 0 keeps a capsule out.
func (m *Mod) boardHullInHangar(ctx context.Context, b *botRun) (bool, error) {
	if b.StationID == 0 || b.CharID == 0 {
		return false, nil
	}
	var shipID int64
	err := m.db.QueryRowContext(ctx, `
		SELECT e.itemID
		  FROM entity e
		  JOIN invTypes  t ON t.typeID  = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4
		   AND g.categoryID = 6 AND e.singleton = 1 AND t.capacity > 0
		 ORDER BY t.capacity DESC, e.itemID ASC
		 LIMIT 1`, b.CharID, b.StationID).Scan(&shipID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("hull in hangar: %w", err)
	}
	if err := m.boardShip(ctx, b, shipID); err != nil {
		return false, err
	}
	return true, nil
}

// gangConfig unmarshals a gang's JSON config blob into dst (dst keeps its defaults
// when the blob is empty). Gangs are the only place a stored activity config
// survives; solo behaviour is derived (activitycfg.go).
func gangConfig(blob string, dst any) error {
	if blob == "" {
		return nil
	}
	return json.Unmarshal([]byte(blob), dst)
}

// gangConfigHas reports whether the operator actually wrote a key, as against the
// field holding its zero value after the unmarshal. A derived default
// (gangMiningPlan) may fill an unset field but never overrule a set one, even one
// set to the value the default would pick, since the default may change.
func gangConfigHas(blob, key string) bool {
	if blob == "" {
		return false
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(blob), &raw) != nil {
		return false
	}
	v, ok := raw[key]
	return ok && string(v) != "null"
}

// ── travel timing ───────────────────────────────────────────────────────────

// unwatchedLegSec is how long one gate-to-gate transit takes when nobody can see it:
// the base transit plus the hull's own align, three times over (alignLegFactor). A
// watched leg does not come through here; the projection flies it and the sim waits.
func unwatchedLegSec(alignSec float64) int64 {
	if alignSec <= 0 {
		alignSec = alignUnknownSec
	}
	base := jumpLegMinSec + rand.Intn(jumpLegMaxSec-jumpLegMinSec+1)
	return int64(base) + int64(alignSec*alignLegFactor)
}

// shipAlignSec is a hull's nominal align time, by class (hulltags.go). One indexed
// read per journey (beginTravel). A capsule, or a ship row that has gone, gets the
// unknown-class figure rather than zero, which would make it the fastest hull.
func (m *Mod) shipAlignSec(ctx context.Context, shipID int64) float64 {
	if shipID == 0 {
		return alignUnknownSec
	}
	var groupID int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT t.groupID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.itemID = ?`, shipID).Scan(&groupID); err != nil {
		return alignUnknownSec
	}
	return alignSecForClass(hullClassByGroup[groupID])
}

// registerPeriodic is how every lane in the module registers its clock.
func (m *Mod) registerPeriodic(label string, everyTicks uint64, fn func(uint64) bool) uint64 {
	return m.tick.RegisterPeriodic(label, everyTicks, fn)
}
