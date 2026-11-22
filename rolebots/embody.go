package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	"gurimoon/modapi"
)

// The embodied tier. The abstract sim (sim.go) is the truth about where a rolebot is and what
// it does. This file projects a nil-Conn destiny.Entity for any enabled rolebot in a solar
// system a real player has loaded, so players see the ship on grid. The projection wanders and
// warps between celestials (belts while mining) with real warp broadcasts, and is torn down when
// the bot docks, leaves the system, or the last real player leaves. The sim decides where the bot
// goes and what it does; while a projection is live it owns the ship's coordinate, mines the real
// rocks and can lose the ship.

const (
	embodyLandingBuffer = 15_000.0

	// embodyQueueHold mirrors destiny.maxPendingHold, which is unexported. It is the worst-case
	// latency between queueing an action and it reaching the wire, and two actions queued closer
	// together than this can share one packet and stamp. Every phase gap in embodyWarpTo uses it.
	embodyQueueHold = 3.0

	// A departing projection stays in the ballparks of the pilots it left until
	// destiny.ObserverDepartSec, when the warp curve has carried the ball past BubbleRadius on their
	// clients. Phase 2 force-flushes the departure grid's warp actions so the removal is timed
	// against that geometry.

	// embodyAlignDirScale is how far out the align heading is projected before it goes on the wire,
	// matching the core's GotoDirection handler. A direction rides in a position field by making the
	// offset swamp the position: at 1e15 against ~1e12 coordinates the normalised result is the heading.
	embodyAlignDirScale = 1e15

	embodyDwellMinSec = 25
	embodyDwellMaxSec = 70
	// The dwell for a projection's first move after an undock or gate jump: a couple of seconds,
	// not the wander clock.
	embodyBirthDwellMinSec = 2
	embodyBirthDwellMaxSec = 6
	// undockGraceSec is how long after an undock a fresh projection still launches out of the dock
	// instead of appearing beside the station. It outlasts the roster pass (about a minute at this
	// size), since a player may load the system after the undock.
	undockGraceSec = 180
	// arriveFreshSec is how recently a bot must have jumped in for its birth to count as the arrival
	// (gate flash and short doorway dwell). navGateDwellMaxSec is how long the abstract tier holds a
	// watched jump-in; the slack covers a pass boundary. It is far shorter than undockGraceSec.
	arriveFreshSec = navGateDwellMaxSec + 5
	// The undock burn's distance clear of the station door is destiny.UndockRunM, shared with the
	// server's own undock. A mining bot parks on its belt much longer between hops.
	embodyMiningDwellMinSec = 240
	embodyMiningDwellMaxSec = 600

	// embodyMaxBirthsPerPass is the most projections one reconcile builds. A hub holds hundreds of
	// rolebots and each birth costs several queries; the rest are wanted next pass (see popMaxChurnPerPass).
	embodyMaxBirthsPerPass = 25

	// embodyLoadFillPasses is how many extra reconciles a freshly loaded system gets, and
	// embodyLoadFillGap how far apart, before it returns to the roster pass (see fillLoadedSystem).
	// 8 x embodyMaxBirthsPerPass covers every hub; the gap is the grid lane's cadence.
	embodyLoadFillPasses = 8
	embodyLoadFillGap    = time.Second
)

// embodyWant is what the abstract tier says one projection should be: which
// character, in which ship and system, doing what.
type embodyWant struct {
	charID, shipID, systemID, corpID int64
	charName                         string
	mining                           bool
	holding                          bool
	patrol                           bool
	prey                             bool
	gankScout, gankStrike            bool
	checkpoint                       bool
	oreTypeID                        int64
	oreName                          string
	// iceFieldID is the Ice Field an ice trip harvests (0 = not an ice trip).
	iceFieldID int64
	// Enabled gang this pilot flies for (0 = none): the lowest gangID, as loadRuns and gangForChar
	// pick. It lets a warp started by one member pull the idle rest of the pack along (gangFollowWarp).
	gangID int64
	// gangLeader is that gang's commander (0 when this pilot flies alone), resolved on the same
	// query because the grid tick cannot go to the database to find the ball to circle.
	gangLeader int64
	// secStatus is the pilot's own security record, carried onto the ball. The client overview,
	// killmails and pod.go victimOutlaw read it from the ball, so an outlaw bot must not read 0.0:
	// the pilot who kills an outlaw is not charged for it.
	secStatus float64
	// factionID is the pilot's employment faction: the faction behind corpID when that is an NPC
	// corporation, 0 for a player corp. It stops a pilot working for the Guristas from shooting
	// Guristas rats (faction.go). It is resolved here because the combat loop must not touch the database.
	factionID int64
	// engage is the lawful twin of prey: this projection shoots the pilots
	// its rules of engagement allow (roe.go), robbing nobody and committing
	// no crime. Set from Posture.engages().
	engage bool
	// allianceID is corpID's alliance (0 = none), read on the roster query so a live ball's slim
	// item can follow a corporation into or out of an alliance (slimrefresh.go).
	allianceID int64
	// posture is the whole task-registry answer. The fight-or-flight rule (combatdoctrine.go) needs
	// what the pilot came out to do, which the four booleans above cannot say: a miner and a belt
	// hunter both park on belts.
	posture Posture
	// doctrine and wingNerve are the employer's combat policy and the fleet's nerve (roe.go).
	// They are stamped here, as factionID is, to keep the database out of the grid tick.
	doctrine  Doctrine
	wingNerve float64
	// Station this bot just undocked from (0 when it did not or the undock is older than
	// undockGraceSec), the slot at its door reserved for this ship, and until when the launch burn
	// owns the helm (undockwave.go).
	undockStationID   int64
	undockSlot        int
	undockRunUntil    int64
	undockLoiterUntil int64
	// When that undock happened. The launch arm interpolates along the run from it, so a projection
	// born mid-burn appears where the burn has reached.
	undockAt int64
	// Gate this bot just jumped in through, with the same grace window.
	arriveGateID int64

	// undockLaunch and arriveFresh say whether the undock and gate markers are still events.
	// See docs/comment-notes-b07.md#fresh-flags.
	undockLaunch bool
	arriveFresh  bool
	// undockJustLeft is the narrow half of undockLaunch: the undock is recent enough to fire the
	// engine FX (BoosterShort is a 30 s ramp) and the doorway dwell. Position takes the whole run;
	// events take this.
	undockJustLeft bool
	// This pilot's resolved personality, carried by value because the combat and mining loops read
	// it from their own goroutines. The two tiers' flee thresholds mirror each other.
	tune Tuning
	// Service in a player's fleet (conscript.go): who is commanding, what they last said, and
	// whether the guns are cold. Carried here so the combat loop does not touch the database.
	bossID          int64
	orderVerb       string
	orderTargetChar int64
	orderHoldFire   bool
	siteID          int64
	// siteRoomIdx is the room the paper run reached (StateData.SiteRoomIdx); a projection born on
	// the site starts there (sitePlacement).
	siteRoomIdx int64
}

// embodiedBot is one live projection.
type embodiedBot struct {
	charID   int64
	shipID   int64
	charName string
	corpID   int64
	// factionID is corpID's faction when the pilot works for an NPC corporation, 0 otherwise
	// (see embodyWant.factionID).
	factionID int64
	systemID  int64
	mining    bool // parks at asteroid belts
	holding   bool // sim has it on a fixed spot (station approach): no wandering
	patrol    bool // hunts: every NPC on grid is a target, not just aggressors
	prey      bool // pirates: PILOTS on grid are targets too (piracy_prey.go)
	engage    bool // combat patrol: PILOTS the rules of engagement allow (roe.go)
	// The gank fleet's grid roles (gank_embodied.go): the scout scans arrivals and calls the strike;
	// a striker shoots the called mark only and never breaks off.
	gankScout  bool
	gankStrike bool
	gank       gankGridState
	// checkpoint marks a member of a checkpoint fleet (camp.go): it waves through the pilots its
	// rules of engagement spare, and a Mercenary one tolls them.
	checkpoint bool
	camp       campGridState
	// posture is what the pilot came out to do; the fight-or-flight rule reads it
	// (combatdoctrine.go).
	posture    Posture
	gangID     int64 // enabled gang this pilot flies for (0 = none)
	gangLeader int64 // that gang's commander (0 = none), the ball a member forms up on
	// The fleet command on the grid (fleetcommand_grid.go). gangRole is this pilot's seat; fleetCmd
	// is carried by the commander only; call is the commander's live calls, read by every member
	// off the commander's projection under its mu.
	gangRole string
	fleetCmd *FleetCommand
	call     fleetCall
	// fleetHoldSince is when this member began holding its planned leg for the commander's warp
	// (fleetmove.go); zero when not holding.
	fleetHoldSince time.Time
	// doctrine is what this pilot's employer allows its guns to do (corpdoctrine.go); wingNerve is
	// the appetite for a fight with a stranger (roe.go), the gang leader's number in a gang.
	// Both are resolved on the hydration query so the combat loop does not touch the database.
	doctrine  Doctrine
	wingNerve float64
	tune      Tuning

	// Service in a player's fleet (conscript_embodied.go). Guarded by mu and refreshed every
	// projection pass, so a new order reaches the helm without rebuilding the ball.
	bossID          int64
	orderVerb       string
	orderTargetChar int64
	orderHoldFire   bool
	// The open site this projection is running (siteembodied.go), and where in it the projection
	// stands. 0 = none.
	siteID      int64
	siteRoomIdx int
	siteRoomID  int64
	siteArrived bool           // the beacon warp has been commanded
	siteBuilt   bool           // this room's rats are built (or adopted)
	siteLoot    map[int64]bool // dunObjects with authored loot, this room
	siteDone    bool
	// The field sweep of a quiet room (siteSweepField): the can being flown to, since when, whether
	// a take is running, the cans given up on this room, and a hold too full to take more.
	siteLootCan   int64
	siteLootSince time.Time
	siteLootBusy  bool
	siteLootSkip  map[int64]bool
	siteHoldFull  bool
	// raidNextAt spaces a prowler's look for site runners (raid.go).
	raidNextAt time.Time
	// Where the commander was when this projection last looked, and when. The escort tick uses it
	// to choose between an approach and a warp.
	escortSeenAt time.Time
	// siegeHold is a fleet member's own copy of its side's siege hold in this system, read at
	// embodiment (loadFleetSeat). fleetHoldPoint falls back on it when the commander is not on
	// this grid, so the line holds and shoots without one.
	siegeHold fleetHold

	// Earliest this projection may ask the warp budget again after a refusal (see deferWarp). It
	// stops refused bots re-asking in lockstep.
	warpAskAfter time.Time

	ent        *destiny.Entity
	ss         *destiny.SolarSystem
	celestials []evedb.CelestialEntity

	// The ore the abstract tier planned for. The embodied miner shoots only rocks of this type, so
	// the hold stays sellable in one stack.
	oreTypeID int64
	oreName   string
	// iceFieldID is the Ice Field an ice trip harvests. The miner stays on it instead of
	// hopping belts (mine_embodied.go). 0 = an ore trip.
	iceFieldID int64

	mu sync.Mutex
	// dead is set by deEmbody and never cleared; a new projection is a new struct. Sleeping
	// goroutines (warp phases, gang follow, wander tick) check it on waking, because the warp's
	// arrival phase would otherwise put the ball back into a bubble the teardown removed it from.
	dead       bool
	warping    bool
	warpCancel context.CancelFunc
	// warpGen identifies the warp in flight. Starting a warp cancels the previous one, and the
	// cancelled goroutine clears `warping` only if it still owns this generation.
	warpGen     int64
	nextActAt   time.Time
	currentDest int64
	// lastGridStepAt is when the grid lane last gave this pilot a full abstract turn
	// (gridlane.go gridStepDue). Zero until the first.
	lastGridStepAt time.Time

	mineCancel context.CancelFunc // live mining loop (nil = not mining)
	mineTarget uint32             // asteroid currently being shot

	// Piracy (piracy_prey.go). ransom is the robbery in progress; load is the resolved combat
	// loadout, published for the ransom paths; profile is the pilot's standings, which stop a pirate
	// robbing someone it likes.
	ransom   *ransomState
	load     *combatLoadout
	profile  *Profile
	passedOn map[int64]bool // pilots this projection has already declined to rob
	// The one capsule this pirate has condemned, and until when. It is rolled once per ship kill
	// against the pilot's cruelty and never re-rolled.
	podMark      int64
	podMarkUntil time.Time

	// Belts this session found empty of the planned ore, and the flag raised once that is all of
	// them. The abstract tier reads it and re-plans the trip (see miningWatchHold).
	oreDryBelts map[int64]bool
	oreDry      bool

	// When the sighted-grid threat look last ran (combat_threat.go). The guard loop ticks every
	// second; this look is slower.
	lastAssessAt time.Time
	// When the whole-system war look last ran (combat_threat.go assessWarInSystem), on its own
	// slower clock.
	lastWarScanAt time.Time

	// NPC kills this engagement, counted where landDamage confirms the death and read at the
	// grid-clear stand-down (combat_embodied.go). Atomic: missile impacts land from other goroutines.
	npcKills atomic.Int64

	// When this projection last reported that its grid is full of its own employer's rats
	// (combat_embodied.go). The condition holds while the patrol sits there, so it logs once an hour.
	lastFriendlyGridAt time.Time

	// Combat: a bot being shot at runs its own loop, which owns the ship's navigation. Mining does
	// not resume while it is up.
	combatCancel context.CancelFunc
	fighting     bool
	fleeing      bool
	combatTarget uint32
	// fightSince is when the current engagement began (zero when not fighting); travel holds for
	// the fight up to a ceiling measured from it (travelHoldsForFight, sim.go).
	fightSince time.Time
	// departing is set by departCommit (crimewatch.go) once the sim has decided this ship docks or
	// jumps; no act against a pilot lands after it. aggrMu orders each act against that decision.
	departing bool
	aggrMu    sync.Mutex
	// Where this projection is escaping to, latched for the whole break-off (0 = not escaping).
	// Re-picking each tick would cancel the warp under way. fleeBlocked marks a break-off that has
	// reported it cannot get away, so the report is made once.
	fleeDest    int64
	fleeBlocked bool
	// preFleeAt is when a flee began with nothing shooting (beltbail.go): no engagement stands
	// down to release that latch, so settlePreemptiveFlee does. Zero for an engaged flee.
	preFleeAt time.Time
	// bailedBelts is each belt this projection left for what was on it, and when. It outlives a
	// mining restart, unlike oreDryBelts, and keeps the next hop off that belt for a while.
	bailedBelts map[int64]time.Time

	// Drones in space (drones_embodied.go), all under mu. droneIDs maps each launched drone to its
	// group; droneTarget and droneRock are what the flight was last commanded onto, with the N beside
	// each its drone count; droneRecallSince is when a discretionary warp called them home.
	droneIDs         map[int64]int64
	droneTarget      uint32
	droneEngagedN    int
	droneRock        uint32
	droneMiningN     int
	droneRecallSince time.Time
	droneLaunchAfter time.Time
	// droneStaged is set once armour is gone in a fight against a pilot: the combat flight is called
	// home and not re-sent until the engagement stands down (stageDrones).
	droneStaged bool

	// Last movement command put on the wire. Repeats are dropped so the client keeps the orbit
	// plane it derived (see embodyOrbit).
	navMode   int
	navTarget uint32
	navDist   float64
	// navHeading is the unit heading of the last straight burn (navModeGoto).
	navHeading destiny.Vec3

	// sling is the slingshot against a kiter (slingshot.go). Owned by the combat loop's goroutine,
	// which is the only reader and writer, so it takes no lock.
	sling slingState

	// The celestial being burned toward after a warp landing, and when that burn started (see
	// burnToActivation).
	approachID    int64
	approachSince time.Time
	// approach is the burn's budget clock (approachStep). Under mu.
	approach approachClock

	// breakReason is the rule that called the break-off under way (breakOffReason), read and
	// cleared by fleeGrid's log line. stayOutLoggedAt spaces the declined-fight line. Under mu.
	breakReason     breakArm
	stayOutLoggedAt time.Time

	// The undock burn and the loiter after it (undockwave.go). undockRunUntil is when the run out
	// of the door ends; nothing steers the ship before then. loiterUntil is when a loitering pilot
	// stops hanging around undockStationID.
	undockRunUntil  time.Time
	loiterUntil     time.Time
	undockStationID int64

	// How this projection came to exist, for the one line installProjection logs. Only
	// prepareProjection knows the birth mode.
	birthMode string
	birthNote string

	// slimSent is the pilot identity (corporation, alliance, security status) the witnesses were
	// last told; slimDirty marks a change still owed to one of them (slimrefresh.go). Under mu.
	slimSent  slimIdentity
	slimDirty bool
}

var (
	// embodyMu guards the embodyLive map and nothing else. It is the most contended lock in the
	// module: every pilot's turn takes it through isEmbodied. Do no database work under it.
	embodyMu   sync.Mutex
	embodyLive = map[int64]*embodiedBot{}

	// embodyWorkMu serialises the reconcile itself (deciding what is on grid, building and tearing
	// down). Nothing on a bot's turn takes it. Lock order: embodyWorkMu, then embodyMu, then eb.mu,
	// and never embodyMu across a database call.
	embodyWorkMu sync.Mutex
)

// reconcileEmbodiment aligns live projections with the sim's truth. It is cheap when nothing
// changed and safe from any goroutine. It reports whether the pass ran out of birth budget
// (embodyMaxBirthsPerPass), which fillLoadedSystem acts on.
func (m *Mod) reconcileEmbodiment(ctx context.Context) bool {
	if m.systems == nil {
		return false
	}

	// Taken before the roster is read: a pilot whose projection a scoped change touches after this
	// is left to that change (touchedSince).
	snapAt := time.Now()
	desired := map[int64]embodyWant{}
	if Master() {
		var err error
		if desired, err = m.desiredProjections(ctx, 0); err != nil {
			// Abandon the whole pass rather than reconcile against a half-read roster: an empty `desired`
			// would tear down every live projection.
			rlogf("rolebots: embody: %v", err)
			return false
		}
	}

	changed := m.applyProjections(ctx, desired, nil, snapAt)

	// A remembered crime becomes answerable when a player arrives or the bots gain projections;
	// this pass is that notification (the deferred half of crimescene.go).
	m.dispatchCrimeScenes()

	return changed
}

// fillLoadedSystem builds out a freshly loaded system's projections instead of waiting for the
// roster pass, which comes round about once a minute (see gridlane.go). It repeats the same
// reconcile a second apart and stops once the birth budget is no longer the limit.
func (m *Mod) fillLoadedSystem(ctx context.Context) {
	for i := 0; i < embodyLoadFillPasses; i++ {
		if !m.reconcileEmbodiment(ctx) {
			return
		}
		if !embodySleep(ctx, embodyLoadFillGap) {
			return
		}
	}
}

// embodyNow puts one bot on grid immediately, without waiting for the sim's 5 s grid lane, so an
// undock reads as a launch and a jump-in as an arrival. It is scoped to this pilot: it may replace
// this bot's stale projection (a jump lands with the old system's projection still live) but never
// withdraws anybody else's.
func (m *Mod) embodyNow(ctx context.Context, charID int64) {
	if !Master() || m.systems == nil || charID == 0 {
		return
	}
	desired, err := m.desiredProjections(ctx, charID)
	if err != nil {
		rlogf("rolebots: embody now charID=%d: %v", charID, err)
		return
	}
	m.applyProjections(ctx, desired, map[int64]bool{charID: true}, time.Time{})
}

// projTouched is when a scoped change (embodyNow, or a dock or jump withdrawing the ball) last
// built or removed one pilot's projection. See docs/comment-notes-b07.md#departure-teardown.
var projTouched sync.Map // charID (int64) -> time.Time

func noteProjectionTouched(charID int64) { projTouched.Store(charID, time.Now()) }

// touchedSince reports whether a scoped change has handled this pilot's projection at or after
// snapAt. The roster-wide reconcile reads its roster before it takes embodyWorkMu, so for such a
// pilot its roster row is older than what is on grid and it must neither build nor tear down.
// A zero snapAt (a scoped call) never matches.
func touchedSince(charID int64, snapAt time.Time) bool {
	if snapAt.IsZero() {
		return false
	}
	v, ok := projTouched.Load(charID)
	if !ok {
		return false
	}
	at, _ := v.(time.Time)
	return !at.Before(snapAt)
}

func init() {
	registerCharForget("projTouched", func(id int64) { projTouched.Delete(id) })
}

// applyProjections brings embodyLive in line with `desired`. `scope` limits which characters may
// be torn down: nil means desired describes the whole roster, a set means only those characters.
// snapAt is when a roster-wide caller began reading `desired` (zero for a scoped call). It decides
// under embodyMu, then tears down and builds with the lock released, because both are database
// work. embodyWorkMu keeps two reconciles from interleaving.
func (m *Mod) applyProjections(ctx context.Context, desired map[int64]embodyWant, scope map[int64]bool, snapAt time.Time) bool {
	embodyWorkMu.Lock()
	defer embodyWorkMu.Unlock()

	// Stamped under embodyWorkMu, so a roster-wide pass waiting on the lock sees it before it decides.
	for charID := range scope {
		noteProjectionTouched(charID)
	}

	var doomed []*embodiedBot
	var births []embodyWant
	// Live projections whose pilot identity no longer matches what their witnesses hold.
	var restamp []*embodiedBot

	// Which loaded systems a real player is standing in, resolved before the registry lock. It is
	// skipped while the projection ceiling is off (the default), since this runs on every undock.
	var playerSys map[int64]bool
	if MaxProjections() > 0 {
		playerSys = m.playerSystems()
	}

	embodyMu.Lock()
	// Stale projections (disabled, docked, moved, ship swapped, system unloaded or emptied of real
	// players) leave the registry here and are torn down below.
	for charID, eb := range embodyLive {
		// A scoped change newer than this roster owns the pilot: its row here is stale.
		if scope == nil && touchedSince(charID, snapAt) {
			continue
		}
		w, ok := desired[charID]
		if ok && w.systemID == eb.systemID && w.shipID == eb.shipID {
			eb.mining, eb.holding, eb.patrol = w.mining, w.holding, w.patrol
			eb.prey = w.prey
			eb.engage = w.engage
			eb.gankScout, eb.gankStrike = w.gankScout, w.gankStrike
			if !w.gankStrike {
				eb.gank.verdict, eb.gank.escape, eb.gank.saidEscaped = gankUndecided, nil, false
			}
			eb.checkpoint = w.checkpoint
			// Refreshed so a bot that changes task changes its stand-and-fight rule without a rebuild.
			eb.posture = w.posture
			eb.gangID, eb.gangLeader = w.gangID, w.gangLeader
			eb.oreTypeID, eb.oreName, eb.iceFieldID = w.oreTypeID, w.oreName, w.iceFieldID
			// Refreshed every pass so a panel slider changes a live projection's nerve.
			eb.tune = w.tune
			// The employer's policy and the fleet's nerve are refreshed too (roe.go), and the
			// side it flies for, so a new allegiance (allegiance.go) reaches a live projection.
			eb.doctrine, eb.wingNerve = w.doctrine, w.wingNerve
			eb.factionID = w.factionID
			// Who the pilot is follows the roster too; the witnesses are told once the lock is released.
			if eb.refreshIdentity(w) {
				restamp = append(restamp, eb)
			}
			// A new order reaches the helm without rebuilding the ball.
			eb.mu.Lock()
			eb.bossID = w.bossID
			eb.orderVerb = w.orderVerb
			eb.orderTargetChar = w.orderTargetChar
			eb.orderHoldFire = w.orderHoldFire
			// The site follows the sim: a run picked while on grid starts with the beacon warp, and
			// a run the sim ended frees the projection (siteembodied.go).
			if eb.siteID != w.siteID {
				eb.resetSiteLocked(w.siteID)
			}
			eb.mu.Unlock()
			continue
		}
		if scope != nil && !scope[charID] {
			continue
		}
		doomed = append(doomed, eb)
		delete(embodyLive, charID)
	}
	for _, w := range desired {
		if _, ok := embodyLive[w.charID]; ok {
			continue
		}
		// Docked or jumped since this roster was read: do not put the ball back.
		if scope == nil && touchedSince(w.charID, snapAt) {
			continue
		}
		births = append(births, w)
	}
	// The projection ceiling (projcap.go) may refuse births and withdraw unwatched projections. What
	// it sheds joins `doomed`, so one teardown path serves both.
	var shed []*embodiedBot
	births, shed = m.projCapSeats(births, len(embodyLive)-len(doomed), playerSys)
	for _, eb := range shed {
		delete(embodyLive, eb.charID)
	}
	doomed = append(doomed, shed...)
	embodyMu.Unlock()

	// Teardown first: a pilot that jumped a gate is both doomed (old system) and a birth (new
	// system), and the old ball must leave space before the new one is built.
	for _, eb := range doomed {
		m.deEmbody(eb)
	}

	// One send per witness, so it runs with the registry lock released.
	for _, eb := range restamp {
		m.pushSlimChange(eb)
	}

	// Cap births per pass: a hub carries hundreds of rolebots and each birth is several queries. The
	// rest stay desired next pass, and the birth dwell staggers their first move. A bashing siege's
	// pilots are built first.
	sortSiegeBirthsFirst(births)
	capped := len(births) > embodyMaxBirthsPerPass
	if capped {
		rlogf("rolebots: %d projections wanted here, building %d this pass",
			len(births), embodyMaxBirthsPerPass)
		births = births[:embodyMaxBirthsPerPass]
	}
	for _, w := range births {
		eb, err := m.prepareProjection(ctx, w)
		if err != nil {
			rlogf("rolebots: embody %s: %v", w.charName, err)
			continue
		}
		if eb == nil {
			continue
		}
		m.installProjection(eb, w)
	}

	// Mining lasers follow the sim (mine_embodied.go). A bot in a fight is exempt: the combat loop
	// owns the helm. This runs over a snapshot so the registry lock is not held across eb.mu.
	embodyMu.Lock()
	live := make([]*embodiedBot, 0, len(embodyLive))
	for _, eb := range embodyLive {
		live = append(live, eb)
	}
	embodyMu.Unlock()
	for _, eb := range live {
		eb.mu.Lock()
		busy := eb.fighting || eb.fleeing
		eb.mu.Unlock()
		switch {
		case busy && eb.mineCancel != nil:
			m.stopEmbodyMining(eb)
		case busy:
			// Guns out: leave it alone.
		case eb.mining && eb.oreTypeID != 0 && eb.mineCancel == nil:
			m.startEmbodyMining(eb)
		case (!eb.mining || eb.oreTypeID == 0) && eb.mineCancel != nil:
			m.stopEmbodyMining(eb)
		}
	}
	return capped
}

// desiredProjections resolves what the sim says should be on grid now. charID 0 asks about the
// whole roster; a specific ID asks about one pilot, which embodyNow needs.
func (m *Mod) desiredProjections(ctx context.Context, charID int64) (map[int64]embodyWant, error) {
	desired := map[int64]embodyWant{}
	where, args := "", []any(nil)
	if charID != 0 {
		where, args = " AND b.characterID = ?", []any{charID}
	} else {
		if m.systems == nil {
			return desired, nil // nothing is loaded, so nothing is projected
		}
		// The roster-wide reconcile reads only the systems that carry projections (the same
		// systemProjected test each row passes below). On a large roster nearly every bot is
		// elsewhere, and reading its state blob every pass only to drop it is the cost.
		var sys []string
		for _, ss := range m.systems.All() {
			if ss != nil && m.systemProjected(ss.ID) {
				sys = append(sys, "?")
				args = append(args, ss.ID)
			}
		}
		if len(sys) == 0 {
			return desired, nil
		}
		where = " AND c.solarSystemID IN (" + strings.Join(sys, ",") + ")"
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName, c.corporationID, c.solarSystemID,
		       COALESCE(ce.locationID, 0), COALESCE(s.activity, ''), COALESCE(s.phase, ''),
		       COALESCE(s.data, ''),
		       COALESCE((SELECT MIN(gm.gangID)
		                 FROM rolebot_gang_members gm
		                 JOIN rolebot_gangs g ON g.gangID = gm.gangID AND g.enabled = 1
		                 WHERE gm.characterID = c.characterID), 0),
		       COALESCE((SELECT g2.leaderCharID
		                 FROM rolebot_gang_members gm2
		                 JOIN rolebot_gangs g2 ON g2.gangID = gm2.gangID AND g2.enabled = 1
		                 WHERE gm2.characterID = c.characterID
		                 ORDER BY gm2.gangID LIMIT 1), 0),
		       COALESCE(n.factionID, rc.allegiance, 0), COALESCE(c.securityRating, 0),
		       COALESCE(co.allianceID, 0)
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		LEFT JOIN entity ce ON ce.itemID = c.characterID
		LEFT JOIN rolebot_state s ON s.characterID = b.characterID
		LEFT JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
		LEFT JOIN rolebot_corps rc ON rc.corporationID = c.corporationID
		LEFT JOIN corporation co ON co.corporationID = c.corporationID
		WHERE b.enabled = 1 AND c.stationID = 0`+where, args...)
	if err != nil {
		return nil, fmt.Errorf("embody query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var w embodyWant
		var shipCandidate int64
		var activity, phase, data string
		if err := rows.Scan(&w.charID, &w.charName, &w.corpID, &w.systemID,
			&shipCandidate, &activity, &phase, &data, &w.gangID, &w.gangLeader,
			&w.factionID, &w.secStatus, &w.allianceID); err != nil {
			return nil, fmt.Errorf("embody scan: %w", err)
		}
		// Logged off by the online cap (popcap.go): nobody is flying that ship. This fires when a player
		// warps in on a bot the cap benched a moment earlier.
		if popIsOffline(w.charID) {
			continue
		}
		// Systems that should carry projections: one a real player is in, or one the ring loaded a jump
		// ahead (preload.go). Never a system this module conjured: a bare GetOrCreate registers an empty shell.
		// Checked first: it needs only the system, and it rules out nearly every bot on a large roster
		// before the state blob is decoded and the hull is confirmed with a query of its own.
		if !m.systemProjected(w.systemID) {
			continue
		}
		// What this projection does comes from the task registry (task.go), not phase strings. The
		// mining flag drives belt-to-belt wander; an empty oreTypeID keeps the lasers off.
		posture := postureFor(phase)
		w.posture = posture
		w.mining = posture.mining()
		w.patrol = posture.patrol()
		w.prey = posture.prey()
		w.engage = posture.engages()
		w.gankScout = phase == string(TaskGangGankScout)
		w.gankStrike = phase == string(TaskGangGankStrike)
		w.checkpoint = phase == string(TaskGangCheckpoint)
		if data != "" {
			var sd StateData
			if json.Unmarshal([]byte(data), &sd) == nil {
				if w.mining && !w.patrol {
					w.oreTypeID, w.oreName = sd.OreTypeID, sd.OreName
				}
				// An ice trip carries no ore family: the harvesters shoot any ice rock on the
				// trip's own field (mine_embodied.go), so the marker only has to be an ice type.
				if phase == string(TaskIceWork) && sd.IceBeltID != 0 {
					w.oreTypeID, w.oreName, w.iceFieldID = iceMarkerTypeID, "ice", sd.IceBeltID
				}
				now := time.Now().Unix()
				// Within the grace window of an undock the projection knows which station it left. That is a
				// position fact and keeps the wide window.
				if sd.UndockStationID != 0 && now-sd.UndockAt < undockGraceSec {
					w.undockStationID = sd.UndockStationID
					w.undockSlot = sd.UndockSlot
				}
				// Carried even when the launch no longer applies; the zero it leaves says the run is over.
				w.undockRunUntil = sd.UndockRunUntil
				w.undockLoiterUntil = sd.UndockLoiterUntil
				w.undockAt = sd.UndockAt
				// Whether it is still coming out of that door decides the kinematics (see undockLaunch).
				w.undockLaunch = w.undockStationID != 0 && undockIsLaunching(sd, now)
				// Whether it left in the last few seconds drives the booster flare and doorway dwell.
				w.undockJustLeft = w.undockLaunch && undockJustLeft(sd, now)
				// The loiter outlives the launch marker (settleUndock clears it when the run ends), so it
				// carries its own station.
				if w.undockStationID == 0 && sd.UndockLoiterUntil > now {
					w.undockStationID = sd.UndockLoiterStationID
					if w.undockStationID == 0 {
						w.undockStationID = sd.UndockStationID
					}
				}
				if sd.ArriveGateID != 0 && now-sd.ArriveAt < undockGraceSec {
					w.arriveGateID = sd.ArriveGateID
					// The gate is where the ship is for the full grace, but the flash applies only while the sim's
					// jump-in dwell lasts.
					w.arriveFresh = arrivalIsFresh(sd, now)
				}
				// The standing order (conscript.go), read from the state blob already decoded here. The
				// commander is the live crew's: a leadership change (conscript_command.go) moves it
				// without rewriting the blob.
				w.bossID = sd.OrderBossID
				if k := conscriptOf(w.charID); k != nil && k.BossCharID != 0 {
					w.bossID = k.BossCharID
				}
				w.orderVerb = sd.OrderVerb
				w.orderTargetChar = sd.OrderTargetChar
				w.orderHoldFire = sd.OrderHoldFire
				w.siteID, w.siteRoomIdx = sd.SiteID, sd.SiteRoomIdx
			}
		}
		// A travelling bot flies the legs the sim gives it and does not sightsee between them. A hauler
		// working the cans and a member holding for the op sit still too.
		w.holding = posture.holding()
		// The character entity sits inside its boarded ship; confirm
		// ownership so a hangar/station locationID never embodies.
		if shipCandidate == 0 {
			continue
		}
		// A corporation kit lent for the op counts as its own (corparmory.go).
		lent := 0
		if loanedTo(shipCandidate) == w.charID {
			lent = 1
		}
		var one int64
		if m.db.QueryRowContext(ctx, `
			SELECT 1 FROM entity WHERE itemID = ? AND (ownerID = ? OR ? = 1)
			  AND typeID IN (SELECT typeID FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID WHERE g.categoryID = 6)`,
			shipCandidate, w.charID, lent).Scan(&one) != nil {
			continue
		}
		w.shipID = shipCandidate

		// Resolved last, so the query is paid only for bots about to be projected.
		if traits, err := getTraits(ctx, m.db, w.charID); err != nil {
			rlogf("rolebots: embody traits charID=%d: %v (using defaults)", w.charID, err)
			w.tune = defaultTuning()
		} else {
			w.tune = resolveTuning(traits)
		}
		w.doctrine = m.corpDoctrineFor(ctx, w.corpID)
		// A pilot flying alone rolls its own nerve; one in a fleet rolls its
		// commander's, and every member looks up the same answer (roe.go).
		w.wingNerve = w.tune.NeutralAggression
		if w.gangID != 0 {
			w.wingNerve = m.gangNerve(ctx, w.gangID, w.tune.NeutralAggression)
		}

		desired[w.charID] = w
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embody rows: %w", err)
	}
	return desired, nil
}

// lookupProjectionByShip finds the live projection whose ship ball is
// entityID (nil when the entity is not one of ours).
func lookupProjectionByShip(entityID uint32) *embodiedBot {
	embodyMu.Lock()
	defer embodyMu.Unlock()
	for _, eb := range embodyLive {
		if uint32(eb.shipID) == entityID {
			return eb
		}
	}
	return nil
}

// dropProjectionsInSystem forgets every projection in a system that is
// being torn down. No RemoveBalls: the system (and every ball in it) is
// already gone, and its entity map must not be touched.
func (m *Mod) dropProjectionsInSystem(solarSystemID int64) {
	embodyMu.Lock()
	var dropped []*embodiedBot
	for charID, eb := range embodyLive {
		if eb.systemID != solarSystemID {
			continue
		}
		dropped = append(dropped, eb)
		delete(embodyLive, charID)
	}
	embodyMu.Unlock()

	for _, eb := range dropped {
		m.persistProjectionDamage(eb)
		// Also persist where it stood, or the system's traffic is rebuilt on stale rows
		// (persistProjectionPosition).
		m.persistProjectionPosition(eb)
		m.stopEmbodyCombat(eb)
		m.stopEmbodyMining(eb)
		eb.mu.Lock()
		// The same mark deEmbody sets: a warp goroutine waking on this projection must not touch it.
		eb.dead = true
		if eb.warpCancel != nil {
			eb.warpCancel()
			eb.warpCancel = nil
		}
		eb.mu.Unlock()
		m.logBot(eb.charID, logInfo, "embody",
			"projection dropped — system %d unloaded", solarSystemID)
	}
}

// loseShip handles a rolebot ball that was destroyed in space. The projection
// goes, and then the death itself is handed to the pod tier: a hull loss puts
// the pilot in a capsule at the wreck, a capsule loss spends its clone and
// wakes it at its medical station (pod.go).
func (m *Mod) loseShip(ctx context.Context, eb *embodiedBot, killerID uint32) {
	embodyMu.Lock()
	if cur, ok := embodyLive[eb.charID]; ok && cur == eb {
		delete(embodyLive, eb.charID)
	}
	embodyMu.Unlock()

	m.stopEmbodyCombat(eb)
	m.stopEmbodyMining(eb)
	eb.mu.Lock()
	// The ship is gone from space; nothing sleeping on this projection may
	// steer it again, least of all across the pod grace below.
	eb.dead = true
	if eb.warpCancel != nil {
		eb.warpCancel()
		eb.warpCancel = nil
	}
	eb.mu.Unlock()

	// Let the explosion finish playing on every client that watched it before
	// a replacement ball turns up in its place.
	time.Sleep(podDeathGrace)

	if m.isCapsule(ctx, eb.shipID) {
		m.handlePodLoss(ctx, eb, killerID)
		return
	}
	m.handleShipLoss(ctx, eb, killerID)
}

// embodiedMining reports whether this bot has a projection with its mining lasers running.
func (m *Mod) embodiedMining(charID int64) bool {
	embodyMu.Lock()
	eb, ok := embodyLive[charID]
	embodyMu.Unlock()
	if !ok {
		return false
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	return eb.mineCancel != nil
}

// embodiedOreDry reports whether this bot's live projection has toured every
// belt in its system and found none of the planned ore anywhere.
func (m *Mod) embodiedOreDry(charID int64) bool {
	embodyMu.Lock()
	eb, ok := embodyLive[charID]
	embodyMu.Unlock()
	if !ok {
		return false
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	return eb.oreDry
}

// noteBeltDry records that the belt the miner sits on has none of its planned ore left, and
// raises oreDry once every belt in the system has come up empty. A system stripped that clean is
// being worked by someone else, so the planner moves the bot.
func (m *Mod) noteBeltDry(eb *embodiedBot) {
	// Only a belt is charged: a miner whose lasers restart at a station or a moon (after a
	// flee) has learned nothing about any belt's rocks.
	beltID := beltUnderfoot(eb)
	if beltID == 0 {
		return
	}
	total := 0
	for _, c := range eb.celestials {
		if c.GroupID == 9 {
			total++
		}
	}
	eb.mu.Lock()
	if eb.oreDryBelts == nil {
		eb.oreDryBelts = map[int64]bool{}
	}
	eb.oreDryBelts[beltID] = true
	raise := total > 0 && len(eb.oreDryBelts) >= total && !eb.oreDry
	if raise {
		eb.oreDry = true
	}
	eb.mu.Unlock()
	if raise {
		m.logBot(eb.charID, logInfo, "mining",
			"every belt in the system is out of %s — asking the planner for somewhere else", eb.oreName)
	}
}

// systemHasRealPlayer reports whether any Conn-backed player is in ss.
// GetAllPlayersInSystem already excludes Conn-less entities (our own
// projections among them); the explicit check keeps that guarantee local.
func systemHasRealPlayer(ss *destiny.SolarSystem) bool {
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn != nil {
			return true
		}
	}
	return false
}

// prepareProjection builds everything a projection needs and puts nothing into space. This is the
// expensive half (six database round trips per bot) and runs with no lock held. Nothing it touches
// is shared until installProjection publishes it.
func (m *Mod) prepareProjection(ctx context.Context, w embodyWant) (*embodiedBot, error) {
	charID, shipID, systemID := w.charID, w.shipID, w.systemID
	ss := m.systems.Get(systemID)
	if ss == nil {
		return nil, nil
	}
	stats, err := evedb.GetShipStats(ctx, m.db, shipID, charID, w.undockStationID)
	if err != nil {
		return nil, err
	}
	celestials, err := evedb.CachedSystemCelestials(ctx, m.db, systemID)
	if err != nil {
		return nil, err
	}

	dm := destiny.New(uint32(shipID), stats.Mass, stats.Agility, stats.MaxVelocity, stats.Radius)
	dm.SetPosition(m.embarkPosition(ctx, shipID, celestials))
	// How this ball came to be where it is, for the birth log. "materialise" is what is left when no
	// arm below fires.
	birthMode, birthNote := "materialise", ""
	if w.arriveFresh {
		birthMode = "gate-arrival"
	}
	// A bot under its undock burn comes out of the door under thrust, as the server launches a
	// player: on the undock line at max velocity along the station's undock orientation.
	launched := false
	var launchGoTo destiny.Vec3
	if w.undockLaunch {
		// destiny.UndockPlacement is shared with setInSpace and the server's initDestiny, so the three
		// agree. The slot is the one already claimed and stored on the state row, not a fresh one.
		if exit, vel, goTo, ok := destiny.UndockPlacement(
			destiny.Vec3{X: stats.StationX, Y: stats.StationY, Z: stats.StationZ},
			destiny.Vec3{X: stats.DockEntryX, Y: stats.DockEntryY, Z: stats.DockEntryZ},
			destiny.Vec3{X: stats.DockOrientationX, Y: stats.DockOrientationY, Z: stats.DockOrientationZ},
			stats.Radius, stats.MaxVelocity, w.undockSlot,
		); ok {
			// The same jitter setInSpace wrote into the entity row (same shipID), so this point and that
			// row are one coordinate.
			exit = exit.Add(parkJitter(shipID))
			goTo = goTo.Add(parkJitter(shipID))
			// Advance along the run: build at the point the sim says the ship has reached, at max velocity.
			// See docs/comment-notes-b07.md#undock-launch.
			lane := goTo.Sub(exit)
			runLen := lane.Len()
			travelled := 0.0
			if w.undockAt > 0 {
				if elapsed := time.Now().Unix() - w.undockAt; elapsed > 0 {
					travelled = float64(elapsed) * stats.MaxVelocity
				}
			}
			// Past the end of its run (a slow hull): fall through to the standoff, which lies further out
			// along the same line, so the ship keeps going forward.
			if runLen > 0 && travelled < runLen {
				dm.SetPosition(exit.Add(lane.Normalize().Scale(travelled)))
				dm.Vel = vel
				// The run is bounded (destiny.UndockRunM) and the waypoint travels in the ball description, so
				// client and server coast to the same point. The bot then aligns out for its next task.
				dm.GotoDirection(goTo)
				dm.SetSpeedFraction(1)
				launched = true
				launchGoTo = goTo
				birthMode = "launch"
				if travelled > 0 {
					birthMode = "launch-inflight"
				}
				birthNote = fmt.Sprintf("station %d slot %d, %.0f m of %.0f m run",
					w.undockStationID, w.undockSlot, travelled, runLen)
			}
		}
	}
	// Not launching (or a door UndockPlacement refuses for lack of an authored dock entry) but still
	// at that station.
	if !launched && w.undockStationID != 0 {
		// At that door but not coming out of it: place the ball at the standoff point, not in the bay
		// inside the station model. See docs/comment-notes-b07.md#standoff-repair.
		if stand, ok := m.stationHullEscape(ctx, shipID, charID,
			w.undockStationID, w.undockSlot, dm.Pos); ok {
			dm.SetPosition(stand.Add(parkJitter(shipID)))
			birthMode = "standoff-repair"
			birthNote = fmt.Sprintf("station %d slot %d", w.undockStationID, w.undockSlot)
		}
	}
	// A pilot the paper run has on a site is born in the room it reached, not where its ship row
	// last stood: it was already fighting there, so a warp in from a belt reads as a late arrival.
	var placed *sitePlace
	if !launched && w.undockStationID == 0 && !w.arriveFresh {
		if sp, ok := m.sitePlacement(ctx, w); ok {
			dm.SetPosition(sp.Pos)
			placed = &sp
			birthMode = "site"
			birthNote = fmt.Sprintf("room %d of site %d", sp.RoomIdx+1, w.siteID)
		}
	}
	// The backstop for every writer above: never put this hull down on another one (occupancy.go).
	// Births run one at a time under embodyWorkMu and each is installed before the next is
	// prepared, so this sees the ones born just before it. A launch moves sideways only, exit and
	// waypoint together, and keeps its run velocity; anything else may move any way and the row
	// follows, so the abstract tier and the next embodiment agree.
	if occ := destiny.OccupancySnapshot(ss, systemID, dm.Pos, birthScanRange, uint32(shipID)); len(occ) > 0 {
		axis := dm.Vel
		if launched {
			axis = launchGoTo.Sub(dm.Pos)
		}
		if p, off, ok := birthClear(dm.Pos, axis, stats.Radius, occ, launched); ok && !off.IsZero() {
			vel := dm.Vel
			dm.SetPosition(p)
			if launched {
				dm.Vel = vel
				dm.GotoDirection(launchGoTo.Add(off))
			} else {
				m.writePosition(ctx, shipID, p)
			}
			birthNote = strings.TrimSpace(birthNote + fmt.Sprintf(" (nudged %.0f m off another hull)", off.Len()))
		}
	}
	dm.TargetLookup = func(id uint32) (destiny.Vec3, bool) {
		if t := ss.GetEntity(id); t != nil {
			return t.Pos(), true
		}
		for i := range ss.CelBalls {
			if ss.CelBalls[i].EntityID == id {
				return ss.CelBalls[i].Pos, true
			}
		}
		// Asteroids are balls, not entities: a miner must approach and orbit the rock it shoots.
		if pos, ok := ss.GetAsteroidPosition(id); ok {
			return pos, true
		}
		// Loot cans are balls too: a site runner sweeping its field flies to each one.
		if pos, ok := ss.GetLootContainerPosition(id); ok {
			return pos, true
		}
		return destiny.Vec3{}, false
	}

	// Fitted modules drive the client's FitHardpoints(): without them the
	// hull renders bare, with no turrets in its hardpoints.
	var modRefs []destiny.ModuleRef
	if fitted, err := evedb.GetFittedModules(ctx, m.db, shipID); err == nil {
		modRefs = make([]destiny.ModuleRef, 0, len(fitted))
		for _, fm := range fitted {
			modRefs = append(modRefs, destiny.ModuleRef{ItemID: fm.ItemID, TypeID: fm.TypeID})
		}
	} else {
		rlogf("rolebots: embody fitted modules ship=%d: %v", shipID, err)
	}

	e := &destiny.Entity{
		ID:       uint32(shipID),
		Manager:  dm,
		IsPlayer: true,
		Conn:     nil,
		BubbleID: -1,
		CharName: w.charName,
		Slim: destiny.SlimItem{
			BallID:     shipID,
			ItemID:     shipID,
			TypeID:     stats.TypeID,
			GroupID:    stats.GroupID,
			CategoryID: stats.CategoryID,
			OwnerID:    charID,
			CorpID:     w.corpID,
			// The ball carries the alliance as a player's does: the grid tier reads kinship off Slim and may
			// not query mid-tick (kinship.go), and killmails read it from here. It comes from the roster
			// row, which is what the refresh compares a live ball against (slimrefresh.go).
			AllianceID: w.allianceID,
			CharID:     charID,
			// The pilot's own record (see embodyWant.secStatus).
			SecurityStatus: w.secStatus,
			Modules:        modRefs,
		},
		Damage: destiny.DamageState{Shield: 1.0, Tau: 1.0e9, Armor: 1.0, Structure: 1.0},
	}
	// Give it a tank before it goes on grid: without HP pools ApplyDamage no-ops and the bot is
	// invulnerable.
	m.initProjectionTank(ctx, e, charID, shipID, stats.TypeID)
	e.Damage = e.DamageStateFrac()

	eb := &embodiedBot{
		charID: charID, shipID: shipID, charName: w.charName, corpID: w.corpID,
		factionID: w.factionID,
		doctrine:  w.doctrine, wingNerve: w.wingNerve,
		systemID: systemID, mining: w.mining, holding: w.holding, patrol: w.patrol,
		prey: w.prey, engage: w.engage, posture: w.posture, tune: w.tune,
		gankScout: w.gankScout, gankStrike: w.gankStrike, checkpoint: w.checkpoint,
		siteID: w.siteID,
		ent:    e, ss: ss, celestials: celestials,
		undockStationID: w.undockStationID,
		birthMode:       birthMode,
		birthNote:       birthNote,
		// The AddBalls that publishes this ball carries the slim built above.
		slimSent:        identityOf(e.Slim),
		bossID:          w.bossID,
		orderVerb:       w.orderVerb,
		orderTargetChar: w.orderTargetChar,
		orderHoldFire:   w.orderHoldFire,
	}
	if w.undockRunUntil > time.Now().Unix() {
		eb.undockRunUntil = time.Unix(w.undockRunUntil, 0)
	}
	if w.undockLoiterUntil > time.Now().Unix() {
		eb.loiterUntil = time.Unix(w.undockLoiterUntil, 0)
	}
	// A pirate's standings decide who it will not rob (piracy_prey.go). They are resolved once here;
	// a failure only means its grudges do not apply this session.
	if prof, err := m.getProfile(ctx, charID); err != nil {
		rlogf("rolebots: embody profile charID=%d: %v", charID, err)
	} else {
		eb.profile = prof
	}
	eb.oreTypeID, eb.oreName, eb.iceFieldID = w.oreTypeID, w.oreName, w.iceFieldID
	eb.gangID, eb.gangLeader = w.gangID, w.gangLeader
	// Born in its room: the site tick builds (or adopts) that room next, with no beacon warp.
	if placed != nil {
		eb.siteArrived, eb.siteRoomIdx, eb.siteRoomID = true, placed.RoomIdx, placed.RoomID
	}
	// The seat and, for the commander, the fleet's command (fleetcommand_grid.go).
	m.loadFleetSeat(ctx, eb)
	// Set the dwell before the projection is published. A bot that just undocked or jumped gets the
	// short doorway dwell; a materialised one gets the long timer, and one born mid-site the short
	// one so its room fills at once. See docs/comment-notes-b07.md#birth-dwell.
	lo, hi := embodyDwellMinSec, embodyDwellMaxSec
	if w.undockJustLeft || w.arriveFresh || placed != nil {
		lo, hi = embodyBirthDwellMinSec, embodyBirthDwellMaxSec
	}
	dwellUntil := time.Now().Add(time.Duration(lo+rand.Intn(hi-lo+1)) * time.Second)
	eb.nextActAt = dwellUntil

	// The sim's legs respect the dwell too: warpAskAfter holds a materialisation, not an undock
	// launch or gate arrival. See docs/comment-notes-b07.md#birth-dwell.
	if !w.undockJustLeft && !w.arriveFresh {
		eb.warpAskAfter = dwellUntil
	}
	return eb, nil
}

// installProjection puts a prepared projection into space and publishes it: the ball into the
// solar system, an AddBalls to everyone who can see it, the guard loop and the registry entry.
// No database call may enter this function; that keeps embodyMu short.
func (m *Mod) installProjection(eb *embodiedBot, w embodyWant) {
	e, dm, ss := eb.ent, eb.ent.Manager, eb.ss
	charID, shipID, systemID := eb.charID, eb.shipID, eb.systemID

	// Claim the registry slot before the ball goes into space, so lookupProjectionByShip resolves it
	// if the bot dies in its first instant.
	embodyMu.Lock()
	if _, taken := embodyLive[charID]; taken {
		// Another build won while this one prepared; nothing is in space yet, so drop this one.
		embodyMu.Unlock()
		return
	}
	embodyLive[charID] = eb
	embodyMu.Unlock()

	bubbleMates, merged, ok := ss.AddEntity(e)
	if !ok {
		embodyMu.Lock()
		if cur, ok := embodyLive[charID]; ok && cur == eb {
			delete(embodyLive, charID)
		}
		embodyMu.Unlock()
		return
	}

	// One law for players and bots: the core's space-entry rules (spaceentry.go), the call a
	// player's undock and gate jump make. An outlaw raises no flag; a carried criminal flag meets CONCORD.
	if enter := modapi.PvP().OutlawEntered; enter != nil {
		enter(e, ss)
	}
	// And a criminal flag raised in the system it came from follows it in
	// (criminalcarry.go), as it does a player through a gate.
	raiseCarriedCriminal(e, ss, charID)

	// Show the ship to players already holding the bubble.
	bd := destiny.BallDescForObserver(dm.BallDesc(), nil)
	bd.CorpID = uint32(w.corpID)
	bd.Name = w.charName
	// Queued under the shared key embodyAddKey, which destiny.QueueAddBalls merges, so any number of
	// births is one AddBalls action. See queueForBubble's header for why lifecycle events use the queue.
	for _, mate := range publishTargets(ss, e.ID) {
		if len(mate.ClaimUnknownBalls(e.ID)) == 0 {
			continue
		}
		mate.QueueAddBalls(embodyAddKey, bd, e.Slim, e.Damage)
	}
	// A projection appearing between two bubbles merges them, and the pilots on either side must be
	// told about each other, as the core's registerUndock does (ensureBallsKnown).
	if merged {
		m.crossNotifyMergedBubble(ss, bubbleMates)
	}
	// Arriving through a gate flashes it for everyone on it (the GateActivity a player's jump-in plays).
	// arriveFresh, not arriveGateID: the marker outlasts the jump, and a system load builds many at once.
	if w.arriveFresh {
		m.gateActivityFX(eb, uint32(w.arriveGateID))
	}
	// Engines lit on the way out of the door, as the core does for a player (pendingUndockBurst,
	// sendBoosterFX). undockJustLeft, not undockLaunch: BoosterShort is a 30 s ramp.
	if w.undockJustLeft {
		m.undockBoosterFX(eb)
	}
	// Warn about a ball born on top of another: two free balls within a hull's length are thrown apart
	// by destiny.dll. The fix belongs in whichever writer produced the point, not in a correction here.
	m.warnIfStacked(eb)
	// On watch from the moment it exists (see combat_embodied.go).
	m.startEmbodyGuard(eb)
	if eb.birthNote != "" {
		m.logBot(charID, logInfo, "embody", "born %s in space (system %d, ship %d) — %s",
			eb.birthMode, systemID, shipID, eb.birthNote)
	} else {
		m.logBot(charID, logInfo, "embody", "born %s in space (system %d, ship %d)",
			eb.birthMode, systemID, shipID)
	}
}

// embarkPosition picks where the projection appears: the ship's persisted coordinates when set,
// else near a celestial it makes sense to stand at (wanderable excludes stars, gates and stations,
// so a materialised bot is not born beside a station and warped off).
func (m *Mod) embarkPosition(ctx context.Context, shipID int64, cels []evedb.CelestialEntity) destiny.Vec3 {
	var x, y, z float64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
		shipID).Scan(&x, &y, &z); err == nil && (x != 0 || y != 0 || z != 0) {
		// The persisted point, unmodified: every writer of that row already carries its own separation
		// (lane slot or parkLaneOffset), so an offset on read would move the ball off its database row.
		return destiny.Vec3{X: x, Y: y, Z: z}
	}
	for _, c := range shuffledCelestials(cels) {
		if !wanderable(c.GroupID) {
			continue
		}
		// Jittered like every park point (parklane.go): two ships falling through to the same celestial
		// would otherwise share one coordinate.
		p := destiny.Vec3{X: c.X + c.Radius + embodyLandingBuffer, Y: c.Y, Z: c.Z}
		return p.Add(parkJitter(shipID))
	}
	return destiny.Vec3{X: 1e6, Y: 0, Z: 0}
}

// undockIsLaunching reports whether a projection built now is still coming out of its door, so it
// is placed on the undock line under thrust rather than at the standoff. It is the control window
// (UndockRunUntil). See docs/comment-notes-b07.md#undock-windows.
func undockIsLaunching(sd StateData, now int64) bool {
	return sd.UndockStationID != 0 && sd.UndockRunUntil > now
}

// undockJustLeft reports whether the undock is still an event: recent enough that a booster flare
// and the doorway dwell describe what a watcher saw. It keys on UndockAt, as arrivalIsFresh keys on
// ArriveAt, and is not widened to the run (effects.BoosterShort ramps over 30 s).
func undockJustLeft(sd StateData, now int64) bool {
	return sd.UndockStationID != 0 && now-sd.UndockAt < undockFreshSec()
}

// undockFreshSec is how recently a bot must have undocked for its birth to count as the launch
// event: the configured baseline run (UndockRunSec), distinct from how long the burn owns the helm.
func undockFreshSec() int64 { return UndockRunSec() }

// arrivalIsFresh reports whether this pilot has just come through its gate, so a projection built
// now should flash it and take the doorway dwell. ArriveGateID holds for undockGraceSec as a
// position; the jump itself ends with the sim's jump-in dwell.
func arrivalIsFresh(sd StateData, now int64) bool {
	return sd.ArriveGateID != 0 && now-sd.ArriveAt <= arriveFreshSec
}

func shuffledCelestials(cels []evedb.CelestialEntity) []evedb.CelestialEntity {
	out := make([]evedb.CelestialEntity, len(cels))
	copy(out, cels)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// deEmbody tears one projection down, with RemoveBalls to anyone who saw it. The caller has
// already removed it from embodyLive and must not hold embodyMu: this persists hull damage.
func (m *Mod) deEmbody(eb *embodiedBot) {
	// Whatever the last fight left on the hull follows the ship out of space.
	m.persistProjectionDamage(eb)
	// Also persist where it stood, or the next embodiment materialises the ship at the last warp
	// landing (persistProjectionPosition).
	m.persistProjectionPosition(eb)
	m.stopEmbodyCombat(eb)
	m.stopEmbodyMining(eb)
	// Drones still out go into the bay if the ship leaves whole, or are lost with it
	// (drones_embodied.go). The abstract tier has no grid to leave them on.
	if eb.ent != nil && eb.ent.IsDestroyed() {
		m.loseDrones(eb, "their ship was destroyed")
	} else {
		m.recallDronesInstant(eb)
	}
	eb.mu.Lock()
	// Marked before the cancel: cancellation only reaches a goroutine looking at its context, and
	// sleeping ones check dead on waking.
	eb.dead = true
	if eb.warpCancel != nil {
		eb.warpCancel()
		eb.warpCancel = nil
	}
	eb.mu.Unlock()

	// The criminal flag leaves with the ball; read it while this system's
	// table still holds it (criminalcarry.go).
	noteCriminalLeaving(eb.systemID, eb.charID)

	mates := eb.ss.RemoveEntity(eb.ent.ID)
	rmKey := "rm:" + strconv.FormatUint(uint64(eb.ent.ID), 10)
	for _, mate := range mates {
		if mate.Conn == nil || !mate.KnowsBall(eb.ent.ID) {
			continue
		}
		// A projection that materialised and withdrew inside the queue's hold window costs the witness
		// nothing: drop both, or the client answers an add-then-remove with a non-existing-ball error.
		if mate.DropPendingAdd(eb.ent.ID) {
			mate.ForgetBalls(eb.ent.ID)
			continue
		}
		// QueueRemoveBall, not ForgetBalls then queue: the ball stays known until the packet leaves, so
		// the window cannot be read as the client never having it.
		mate.QueueRemoveBall(rmKey, eb.ent.ID)
	}
	// The last projection out empties the system: release via modapi.ReleaseSystem, off this goroutine
	// (its unloading hook re-enters teardown). See docs/comment-notes-b07.md#system-release.
	sysID := eb.systemID
	if modapi.CanLoadSystems() {
		goSafe("system release", func() {
			if modapi.ReleaseSystem(sysID) {
				rlogf("rolebots: system %d released with its last projection", sysID)
			}
		})
	} else if m.systems.RemoveIfEmpty(eb.ss) {
		// An older core with no system-loading seam: the belt flush is out of reach, so keep the NPC
		// teardown copy.
		goSafe("system-unloading hook", func() { modapi.FireSystemUnloading(sysID) })
		if m.npc != nil {
			if n := m.npc.RemoveSystem(sysID); n > 0 {
				rlogf("rolebots: system %d unloaded with the last projection — despawned %d NPCs",
					sysID, n)
			}
		}
	}
	m.logBot(eb.charID, logInfo, "embody", "projection withdrawn (system %d)", eb.systemID)
}

// embodyTick drives idle wander/warp for every projection. Called from the
// grid lane's own second (gridlane.go), not from the roster pass.
func (m *Mod) embodyTick() {
	embodyMu.Lock()
	bots := make([]*embodiedBot, 0, len(embodyLive))
	for _, eb := range embodyLive {
		bots = append(bots, eb)
	}
	embodyMu.Unlock()

	now := time.Now()
	for _, eb := range bots {
		// Two windows own a fresh departure before anything else, checked ahead of the readiness test: the
		// undock run (nothing steers) and the loiter circle (issued here because embodyOrbit needs a clear ship).
		eb.mu.Lock()
		dead, fighting, fleeing := eb.dead, eb.fighting, eb.fleeing
		runUntil, loiterUntil, stationID := eb.undockRunUntil, eb.loiterUntil, eb.undockStationID
		slimOwed := eb.slimDirty
		eb.mu.Unlock()
		if dead {
			continue
		}
		// An identity change a witness could not take yet (its AddBalls was still queued) is sent again.
		if slimOwed {
			m.pushSlimChange(eb)
		}
		if !fighting && !fleeing {
			if now.Before(runUntil) {
				continue
			}
			if now.Before(loiterUntil) {
				m.undockLoiterCircle(eb, stationID)
				continue
			}
		}

		// A servant keeps station on its commander (conscript_embodied.go), ahead of the wander gate
		// because PostureEscort holds. A pilot running an open site is flown by the site (siteembodied.go).
		if m.siteEmbodyTick(eb, now) {
			continue
		}
		// A prowling pirate with a site runner in its system goes to the runner's pocket (raid.go); on
		// landing the prey picker takes over.
		if m.raidEmbodyTick(eb, now) {
			continue
		}
		if m.escortTick(eb, now) {
			continue
		}

		// A holding projection circles its station (stationHoldCircle) or, in a gang, its commander
		// (gangFormUpCircle). It runs after escortTick and before the ready test, which excludes eb.holding.
		eb.mu.Lock()
		holding, leader := eb.holding, eb.gangLeader
		hDead, hFighting, hFleeing := eb.dead, eb.fighting, eb.fleeing
		hWarping, hMine, hApproach := eb.warping, eb.mineTarget, eb.approachID
		hMining := eb.mineCancel != nil
		eb.mu.Unlock()
		// The same busy set the ready test uses, mineCancel included: a session loading lasers holds no
		// target yet and would look idle.
		if holding && !hDead && !hFighting && !hFleeing && !hWarping && !hMining &&
			hMine == 0 && hApproach == 0 {
			// A gank scout on an undock sits where it parked (gank_undock.go).
			if m.gangFormUpCircle(eb, leader) || (!eb.gankScout && m.stationHoldCircle(eb)) {
				continue
			}
		}

		eb.mu.Lock()
		// A ship that is mining, burning toward a gate or station, loading its lasers or fighting is flown
		// elsewhere, so only an idle one wanders. dead is rechecked: a withdrawal may land after the snapshot.
		ready := !eb.dead && !eb.warping && !eb.holding && eb.mineTarget == 0 &&
			eb.approachID == 0 && eb.mineCancel == nil &&
			!eb.fighting && !eb.fleeing && now.After(eb.nextActAt)
		eb.mu.Unlock()
		if ready {
			// A refusal is the stamp budget, not a failure: push this pilot's next turn out a few seconds so
			// the tick does not re-ask every second.
			if !m.startEmbodyWarp(eb) {
				m.deferWarp(eb)
			}
		}
	}
}

// wanderable reports whether an idle projection may pick this kind of celestial to sit at. Stars
// (6), stargates (10) and stations (15) are excluded: a bot on a gate or station looks about to leave,
// and docking belongs to the abstract tier.
func wanderable(groupID int64) bool {
	switch groupID {
	case groupStar, groupStargate, groupStation:
		return false
	}
	return true
}

// startEmbodyWarp picks the next celestial (belts for miners) and warps. Both callers are idle
// behaviour (the wander tick and a miner whose belt ran dry), so it is always warpDiscretionary.
// It reports whether a warp started; false means the stamp budget refused and the caller defers.
func (m *Mod) startEmbodyWarp(eb *embodiedBot) bool {
	eb.mu.Lock()
	dry := make(map[int64]bool, len(eb.oreDryBelts))
	for id := range eb.oreDryBelts {
		dry[id] = true
	}
	current := eb.currentDest
	eb.mu.Unlock()

	// A gang's guns go to the belt its miners left and stay on it while the alarm is live
	// (beltbail.go). Holding there reads as a refused hop, which the caller defers. The warp
	// does not lead: the miners come back on their own recall (opBeltWorkable), not in tow.
	if eb.patrol {
		if belt := gangBeltAlarmFor(eb.gangID); belt != 0 {
			if c := celestialByID(eb.celestials, belt); c != nil {
				if current == belt {
					return false
				}
				return m.startEmbodyWarpAs(eb, *c, false, warpDiscretionary)
			}
		}
	}
	// A gang miner goes back to its op belt when that is open to it; everyone keeps off a belt
	// it has just bailed from (wanderCandidates).
	prefer := int64(0)
	if eb.mining {
		prefer = gangOpBeltFor(eb.gangID)
	}
	candidates := wanderCandidates(eb.celestials, current, eb.mining, dry, bailedNow(eb), prefer)
	if len(candidates) == 0 {
		return false
	}
	// A gang member's wander is the whole gang's: the pick is broadcast so idle packmates warp with it
	// (gangFollowWarp), and a hunting pack sweeps a system as a formation.
	return m.startEmbodyWarpAs(eb, candidates[rand.Intn(len(candidates))], true, warpDiscretionary)
}

// startEmbodyWarpTo warps a projection to one named celestial. The warp runs on its own goroutine
// and eb.warping stays true until it lands. It reports whether the warp started; false means the
// stamp budget refused and nothing was touched.
func (m *Mod) startEmbodyWarpTo(eb *embodiedBot, target evedb.CelestialEntity, class warpClass) bool {
	return m.startEmbodyWarpAs(eb, target, false, class)
}

// startEmbodyWarpAs is startEmbodyWarpTo with the gang-warp seam: lead=true (the idle wander only,
// never the sim's travel legs) pulls the pack along. Followers enter through startEmbodyWarpTo, so a
// follow cannot cascade into a second broadcast. class says whether the stamp budget may refuse
// this warp (see warpClass). It reports whether the warp started.
func (m *Mod) startEmbodyWarpAs(eb *embodiedBot, target evedb.CelestialEntity, lead bool, class warpClass) bool {
	// The one gate every caller passes: a withdrawn projection must not be commanded from any snapshot
	// or sleep. The undock run is refused too, since a warp inside it cancels the launch; getting shot ends the run.
	eb.mu.Lock()
	if eb.dead {
		eb.mu.Unlock()
		return false
	}
	if !eb.undockRunUntil.IsZero() && time.Now().Before(eb.undockRunUntil) {
		if !eb.fighting && !eb.fleeing {
			eb.mu.Unlock()
			return false
		}
		eb.undockRunUntil = time.Time{}
	}
	eb.mu.Unlock()

	// Drones out? A discretionary warp waits for them and a mandatory one leaves them
	// (drones_embodied.go). A refusal here must leave nothing touched and spend no budget slot.
	if !m.dronesClearForWarp(eb, class) {
		return false
	}

	// The stamp budget (warpbudget.go) is consulted here, before clearNav, so a refusal leaves
	// nothing touched. See docs/comment-notes-b07.md#warp-budget-gate.
	if !m.admitBotWarp(eb, class) {
		return false
	}

	eb.clearNav()
	eb.mu.Lock()
	eb.currentDest = target.ItemID
	eb.approachID = 0
	eb.warping = true
	eb.warpGen++
	gen := eb.warpGen
	ctx, cancel := context.WithCancel(context.Background())
	if eb.warpCancel != nil {
		eb.warpCancel()
	}
	eb.warpCancel = cancel
	gangID := eb.gangID
	eb.mu.Unlock()

	if lead && gangID != 0 {
		m.gangFollowWarp(eb.charID, gangID, eb.systemID, target)
	}
	m.goSafeBot(eb.charID, "warp", func() { m.embodyWarpTo(ctx, eb, target, gen) })
	return true
}

// gangFormUpCircle puts a holding gang member into an orbit of its commander's ball, so a gathering
// reads as a fleet. It returns false for a leaderless pilot, an unprojected or other-system
// commander, and the commander itself; the caller then falls through to stationHoldCircle. Lock
// order is embodyMu, then eb.mu.
func (m *Mod) gangFormUpCircle(eb *embodiedBot, leaderCharID int64) bool {
	// eb.ent is checked here because embodyOrbit dereferences it unguarded (nav_embodied.go).
	if leaderCharID == 0 || leaderCharID == eb.charID || eb.ent == nil {
		return false
	}
	embodyMu.Lock()
	lead := embodyLive[leaderCharID]
	embodyMu.Unlock()
	if lead == nil || lead.systemID != eb.systemID || lead.ent == nil {
		return false
	}
	lead.mu.Lock()
	gone := lead.dead
	lead.mu.Unlock()
	if gone {
		return false
	}
	m.embodyOrbit(eb, lead.ent.ID, formUpOrbitM, 0)
	return true
}

// gangFollowWarp pulls a warping member's idle packmates into the same warp, staggered so the pack
// streams onto the grid. Only idle projections follow; one warping, fighting, fleeing, approaching,
// mining or held by its sim stays put, so the follow never fights another owner for the helm.
func (m *Mod) gangFollowWarp(leadCharID, gangID, systemID int64, target evedb.CelestialEntity) {
	embodyMu.Lock()
	var mates []*embodiedBot
	lead := embodyLive[leadCharID]
	for _, eb := range embodyLive {
		if eb.charID == leadCharID || eb.gangID != gangID || eb.systemID != systemID {
			continue
		}
		mates = append(mates, eb)
	}
	embodyMu.Unlock()
	// Under a fleet command the formed-up line follows too (fleetmove.go): a
	// member holding in orbit of the commander is waiting for exactly this.
	commanded := lead != nil && lead.fleetCmd != nil
	var leadBall uint32
	if lead != nil && lead.ent != nil {
		leadBall = lead.ent.ID
	}
	for _, mate := range mates {
		mate := mate
		delay := time.Duration(300+rand.Intn(1500)) * time.Millisecond
		m.goSafeBot(mate.charID, "gang follow-warp", func() {
			time.Sleep(delay)
			mate.mu.Lock()
			// dead first: a mate withdrawn during the sleep must not get a warp, which would put a ball back
			// into an emptied bubble.
			orbitingLead := leadBall != 0 && mate.navMode == navModeOrbit && mate.navTarget == leadBall
			ok := followEligible(mate.dead, mate.warping, mate.fighting, mate.fleeing, mate.holding,
				mate.mineCancel != nil || mate.mineTarget != 0, mate.approachID != 0,
				mate.currentDest == target.ItemID, orbitingLead, commanded)
			mate.mu.Unlock()
			if !ok {
				return
			}
			m.fleetHoldRelease(mate)
			m.startEmbodyWarpTo(mate, target, warpDiscretionary)
		})
	}
}

// embodyWarpTo runs the four-phase server-driven warp, broadcasting each phase to Conn-backed
// bubble mates. It mirrors the player warp goroutine; the bot is the source of every packet.
func (m *Mod) embodyWarpTo(ctx context.Context, eb *embodiedBot, target evedb.CelestialEntity, gen int64) {
	defer func() {
		eb.mu.Lock()
		// Only the warp still in flight may declare the ship out of warp; a cancelled warp was replaced by a
		// newer one that already raised the flag.
		if eb.warpGen == gen {
			eb.warping = false
			// Clear the manager's nav mode too: nothing steps a warping manager, so an abandoned warp would
			// leave IsInWarp() true and the ball exempt from the core's ballpark prune.
			if dm := eb.ent.Manager; dm != nil && dm.CurrentBallMode() == destiny.ModeWarp {
				dm.Stop()
			}
		}
		lo, hi := embodyDwellMinSec, embodyDwellMaxSec
		if eb.mining {
			lo, hi = embodyMiningDwellMinSec, embodyMiningDwellMaxSec
		}
		eb.nextActAt = time.Now().Add(time.Duration(lo+rand.Intn(hi-lo)) * time.Second)
		eb.mu.Unlock()
	}()

	dm := eb.ent.Manager
	ss := eb.ss
	ent := eb.ent
	if dm == nil || ss == nil {
		return
	}

	celPos := destiny.Vec3{X: target.X, Y: target.Y, Z: target.Z}
	exitDist := target.Radius + embodyLandingBuffer
	landing := embodyWarpLanding(dm.Pos, celPos, exitDist)
	// Arrival lane shared with the core's startPlayerWarp, so a bot and a player cannot claim one slot.
	// Do not land station arrivals on the undock axis: see docs/comment-notes-b07.md#warp-landing-lane.
	laneDir := landing.Sub(celPos)
	if laneDir.IsZero() {
		laneDir = destiny.Vec3{X: 1}
	}
	// The per-ship nudge rides on the base, before the claim, never on the point the claim returns.
	// See docs/comment-notes-b07.md#warp-landing-lane.
	// laneBase and departPos are kept for the landing's second look before the mirror (phase 2b).
	laneBase := landing.Add(parkJitter(eb.shipID))
	departPos := dm.Pos
	landing = destiny.ClaimArrivalPoint(ss, ss.ID, laneBase, laneDir, dm.Radius(), ent.ID)
	// No exit radius is carried past this point: the lane's offset is lateral, so both the wire action
	// and the ball's warp sector name the landing itself and pass 0.
	_, tunnelSec, _ := dm.WarpPhases(celPos)

	// Phase 1: align, queued like the later phases. GotoDirection takes a direction, so the target is
	// scaled by embodyAlignDirScale as in the core. See docs/comment-notes-b07.md#warp-align.
	alignDir := celPos.Sub(dm.Pos)
	if alignDir.IsZero() {
		alignDir = destiny.Vec3{X: 1}
	}
	alignFar := dm.Pos.Add(alignDir.Normalize().Scale(embodyAlignDirScale))
	dm.GotoDirection(alignFar)
	dm.NextStamp()
	m.queueForBubble(ss, ent.ID, ent.ID, keyedAction{"wgoto", destiny.BuildSimpleAction(
		"GotoDirection",
		marshal.PyInt{Value: uint64(dm.EntityID)},
		marshal.PyReal{Value: alignFar.X},
		marshal.PyReal{Value: alignFar.Y},
		marshal.PyReal{Value: alignFar.Z},
	)})

	var mirrorMates []*destiny.Entity

	// The witnesses' dll arms the warp on the tick the ball passes its align gate, so the WarpTo goes
	// out when the server's own ball passes it: a bot already flying another way needs longer than
	// the from-rest align. Floored at embodyQueueHold, or the align and the WarpTo could share one
	// packet and stamp and the ship would never align.
	if _, ok := ent.AwaitWarpGate(ctx.Done(), celPos, time.Duration(embodyQueueHold*float64(time.Second))); !ok {
		m.dropWarpMirror(mirrorMates, ent.ID)
		return
	}

	// Phase 2: tunnel. SendWarpTo's three actions each take their own key; a shared key would coalesce
	// them to the last and warp a ball still at speed 0.
	effectStamp := dm.Stamp
	if effectStamp == 0 {
		// A warp tail with a zero effect stamp is the hollow shape destiny.WarpHandoffBallDesc refuses,
		// which would drop the arrival back to a parked ball.
		effectStamp = dm.NextStamp()
	}
	// Warp to the landing with exit range 0, the form the wire action and the core use. See
	// docs/comment-notes-b07.md#warp-tail-form.
	dm.WarpTo(landing, effectStamp, dm.EntityID, 0)
	dm.NextStamp()
	warpActions := destiny.BuildWarpToActions(dm.EntityID,
		landing.X, landing.Y, landing.Z, 0, destiny.WarpSpeedAUperSec)
	warpKeyed := []keyedAction{
		{"wssf", warpActions[0]},
		{"warp", warpActions[1]},
		{"wfx", warpActions[2]},
	}
	m.queueForBubble(ss, ent.ID, ent.ID, warpKeyed...)
	// Flushed so the removal window can be short: with the warp drained here (as startPlayerWarp does)
	// the removal cannot share a tick with it, and the window is destiny.ObserverDepartSec.
	// A forced flush mints, though, and on a hub grid a stamp per departure per witness runs the
	// witness's sim clock ahead until the client fast-forwards. So it is forced only when the mint is
	// free (StampLead() < 0, as the warp mirror); otherwise the warp rides the next headroom flush, at
	// most embodyQueueHold later, and that witness's removal waits the same extra time.
	var lateWarp map[uint32]bool
	for _, mate := range ss.GetBubbleConnectedPlayersExcept(ent.ID) {
		if mate.Conn == nil || !mate.KnowsBall(ent.ID) {
			continue
		}
		if mate.StampLead() < 0 {
			mate.FlushQueuedActions(destiny.FlushNodeID, true)
			continue
		}
		if lateWarp == nil {
			lateWarp = make(map[uint32]bool, 4)
		}
		lateWarp[mate.ID] = true
	}
	departMates := ss.RemoveFromBubble(ent.ID)

	// Whoever watched the ship leave must be told the ball is gone, even if the tunnel sleep is
	// cancelled, or a ghost ball strands in their ballpark. Queued. See docs/comment-notes-b07.md#warp-removal.
	rmKey := "rm:" + strconv.FormatUint(uint64(ent.ID), 10)
	removeDeparted := func(late bool) {
		for _, mate := range departMates {
			if mate.Conn == nil || !mate.KnowsBall(ent.ID) || lateWarp[mate.ID] != late {
				continue
			}
			// HasPendingAdd, not DropPendingAdd: DropPendingAdd purges every queued action for the ball,
			// including the warp itself. See docs/comment-notes-b07.md#warp-removal.
			if mate.HasPendingAdd(ent.ID) {
				mate.DropPendingAdd(ent.ID)
				mate.ForgetBalls(ent.ID)
				continue
			}
			// See deEmbody: the ball is retired by the flush, not here. A bot re-entering this grid inside the
			// hold window would otherwise find it forgotten and its AddBalls ignored as a duplicate.
			mate.QueueRemoveBall(rmKey, ent.ID)
		}
	}
	// The tunnel sleep is split around the bystander removal, as startPlayerWarp does, so a witness
	// does not hold a motionless hull for the whole hop. Cancellation in the first half removes first.
	observerHold := tunnelSec
	if observerHold > destiny.ObserverDepartSec {
		observerHold = destiny.ObserverDepartSec
	}
	if !embodySleep(ctx, time.Duration(observerHold*float64(time.Second))) {
		removeDeparted(false)
		removeDeparted(true)
		return
	}
	removeDeparted(false)
	if len(lateWarp) > 0 {
		// Not on ctx: the removal must go out even if the warp is cancelled, or a ghost ball strands.
		time.AfterFunc(time.Duration(embodyQueueHold*float64(time.Second)), func() {
			defer func() {
				if r := recover(); r != nil {
					rlogf("rolebots: PANIC in late warp removal for ball %d: %v", ent.ID, r)
				}
			}()
			removeDeparted(true)
		})
	}

	// Phase 2b: the warp mirror, destiny.WarpMirrorLeadSec before the landing. It is sent late so the
	// witness is not buried by mirrored balls published at warp start (see destiny/warpin.go).
	mirrorAt := tunnelSec - destiny.WarpMirrorLeadSec
	if mirrorAt < observerHold {
		mirrorAt = observerHold
	}
	if wait := mirrorAt - observerHold; wait > 0 {
		if !embodySleep(ctx, time.Duration(wait*float64(time.Second))) {
			return
		}
	}
	// The landing was claimed before the align; another hull may have moved onto it since (a ship
	// orbiting a fight at the gate, one burning in from its own landing). Look again while no
	// witness holds this landing yet: on a hop past the bubble the departure grid's witnesses are
	// gone and the destination's get the ball from the mirror below or at phase 3. A move re-aims
	// the server's warp and the actions the mirror sends, so nobody sees a snap.
	relanded := func(stage string) {
		nl, _, moved := destiny.RecheckArrivalPoint(ss, ss.ID, landing, laneBase, laneDir, dm.Radius(), ent.ID)
		if !moved {
			return
		}
		m.logBot(eb.charID, logInfo, "embody", "landing moved %.0f m off a hull standing on it (%s)",
			nl.Sub(landing).Len(), stage)
		landing = nl
		dm.WarpTo(landing, effectStamp, dm.EntityID, 0)
		warpActions = destiny.BuildWarpToActions(dm.EntityID,
			landing.X, landing.Y, landing.Z, 0, destiny.WarpSpeedAUperSec)
		warpKeyed = []keyedAction{
			{"wssf", warpActions[0]},
			{"warp", warpActions[1]},
			{"wfx", warpActions[2]},
		}
	}
	longHop := landing.Sub(departPos).Len() > destiny.BubbleRadius
	if longHop {
		relanded("before the mirror")
	}
	mirrorMates = m.armWarpMirror(ss, eb, ent, dm, landing, warpKeyed)
	if rest := tunnelSec - mirrorAt; rest > 0 {
		if !embodySleep(ctx, time.Duration(rest*float64(time.Second))) {
			m.dropWarpMirror(mirrorMates, ent.ID)
			return
		}
	}

	// Phase 3: arrive in the destination bubble. The last check before the call that puts a ball back
	// into space: a teardown landing since the sleep must not have its removal undone.
	eb.mu.Lock()
	gone := eb.dead
	eb.mu.Unlock()
	if gone {
		return
	}
	// The last look at the landing, only while no mirror witness is flying the old WarpTo: moving
	// it then would be a snap on their screens.
	if longHop && len(mirrorMates) == 0 {
		relanded("at arrival")
	}
	// Phase 3: the late joiner's ball only. Pilots at this landing at warp start hold it already; a
	// late joiner gets the ship parked at its landing. See docs/comment-notes-b07.md#warp-mirror.
	bd := destiny.BallDescForObserver(dm.BallDesc(), nil)
	bd.CorpID = uint32(eb.corpID)
	bd.Name = ent.CharName
	entryPos := landing

	// The server's copy lands here, not in phase 4, so dm.Pos is not left at the departure point (a
	// lock on the bot would be refused as too far). Nav stays navWarp until phase 4's Stop.
	dm.SetPosition(landing)

	destMates, _ := ss.AddToBubble(ent.ID, entryPos)
	if len(destMates) > 0 {
		// Counted so a missing arrival can be traced: published went on the wire, knew is a witness that
		// already held the ball (stale known-ball set), notReady is a witness with no ballpark yet.
		var published, knew, notReady int
		for _, mate := range destMates {
			if mate.Conn == nil {
				continue
			}
			if !mate.HasBallparkState() {
				notReady++
				continue
			}
			if len(mate.ClaimUnknownBalls(ent.ID)) == 0 {
				knew++
				continue
			}
			// Queued and merged: several bots landing in one tick share one AddBalls action.
			mate.QueueAddBalls(embodyAddKey, bd, ent.Slim, ent.Damage)
			published++
		}
		if published+knew+notReady > 0 {
			m.logBot(eb.charID, logInfo, "embody",
				"warp arrival published to %d witness(es) (%d already held the ball, %d had no ballpark)",
				published, knew, notReady)
		}
	}
	// Wait out the core's exit buffer (startPlayerWarp's expression), or land at once if cancelled:
	// returning early would leave an announced arrival stuck mid-warp-in.
	landWait := math.Max(5, 3+dm.Mass()*dm.Agility()/1e6*0.3)
	embodySleep(ctx, time.Duration(landWait*float64(time.Second)))

	// Phase 4: land, queued. SetBallPosition stops the ball and GOTOs it to the point, so sending it
	// late is safe. The recipients are the destination bubble's mates.
	dm.SetPosition(landing)
	dm.Stop()
	dm.NextStamp()
	m.queueForBubble(ss, ent.ID, ent.ID, keyedAction{"wland", destiny.BuildSimpleAction(
		"SetBallPosition",
		marshal.PyInt{Value: uint64(ent.ID)},
		marshal.PyReal{Value: landing.X},
		marshal.PyReal{Value: landing.Y},
		marshal.PyReal{Value: landing.Z},
	)})

	// Persist so re-embodiment resumes here, unless the ship has left this system since.
	pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.writeProjectionPosition(pctx, eb.shipID, eb.systemID, landing)
}

func embodyWarpLanding(shipPos, celestial destiny.Vec3, exitDist float64) destiny.Vec3 {
	dir := shipPos.Sub(celestial).Normalize()
	return celestial.Add(dir.Scale(exitDist))
}

func embodySleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// keyedAction is one destiny action plus the coalescing kind it belongs to.
type keyedAction struct {
	kind   string
	action marshal.PyRep
}

// queueForBubble queues actions describing ballID on every bubble-mate holding it, instead of
// sending each as its own stamped packet. A key names a kind of command so a newer one replaces a
// stale one; actions that must all arrive need one key each. See docs/comment-notes-b07.md#queue-for-bubble.
func (m *Mod) queueForBubble(ss *destiny.SolarSystem, sourceID, ballID uint32, actions ...keyedAction) {
	if ss == nil {
		return
	}
	suffix := fmt.Sprintf(":%d", ballID)
	for _, mate := range ss.GetBubbleConnectedPlayersExcept(sourceID) {
		if mate.Conn == nil || !mate.KnowsBall(ballID) {
			continue
		}
		for _, ka := range actions {
			mate.QueueAction(ka.kind+suffix, ballID, ka.action)
		}
		// No flush here: the 5 Hz solar-system tick owns delivery, and a per-event flush in a per-recipient
		// loop multiplies packets. The cost is at most 200 ms of latency.
	}
}

// embodyAddKey is the single coalescing key every projection delivery uses. destiny.QueueAddBalls
// merges on a key collision, so a pass publishing many bots to one witness is one AddBalls action.
const embodyAddKey = "rb:add"

// publishTargets is the set of connected pilots on ss's grid able to receive a delivery for ballID
// now. The AddBalls and bubble-merge halves must agree on it, or a pilot is left holding half a grid.
func publishTargets(ss *destiny.SolarSystem, ballID uint32) []*destiny.Entity {
	all := ss.GetBubbleConnectedPlayersExcept(ballID)
	out := all[:0]
	for _, mate := range all {
		if mate.Conn == nil || !mate.HasBallparkState() {
			continue
		}
		out = append(out, mate)
	}
	return out
}

// crossNotifyMergedBubble tells every connected pilot in a freshly merged bubble about every ball in
// it they do not hold. SolarSystem.AddEntity returns merged=true with the union of both sides, and
// the core's registerUndock acts on the same flag. It sends one merged AddBalls per recipient.
func (m *Mod) crossNotifyMergedBubble(ss *destiny.SolarSystem, mates []*destiny.Entity) {
	for _, rcpt := range mates {
		if rcpt.Conn == nil || !rcpt.HasBallparkState() {
			continue
		}
		for _, subj := range mates {
			if subj.ID == rcpt.ID || subj.IsCloaked() {
				continue
			}
			if len(rcpt.ClaimUnknownBalls(subj.ID)) == 0 {
				continue
			}
			// Sanitized like every first-sight ball: a mate caught mid-warp is published where it is going, at
			// rest.
			bd := destiny.BallDescForObserver(subj.BallDesc(), rcpt.KnowsBall)
			bd.CorpID = uint32(subj.Slim.CorpID)
			bd.Name = subj.BallName()
			dmg := subj.Damage
			if subj.HasHP() {
				dmg = subj.DamageStateFrac()
			}
			rcpt.QueueAddBalls(embodyAddKey, bd, subj.Slim, dmg)
		}
	}
}

// stackWarnRadius is how close two free balls must be before a projection birth is logged. It is
// sized off destiny.ArrivalClearMargin: inside the two radii plus that margin the dll's overlap
// resolution throws ships apart.
const stackWarnRadius = 5_000.0

// warnIfStacked logs when a freshly published projection shares its coordinate with a free ball. It
// only reports; the fix belongs in the writer of the point.
func (m *Mod) warnIfStacked(eb *embodiedBot) {
	if eb.ss == nil || eb.ent == nil || eb.ent.Manager == nil {
		return
	}
	pos := eb.ent.Pos()
	// FreeBallFootprints reports only free balls; one mid-warp counts at its landing.
	for _, fp := range eb.ss.FreeBallFootprints(pos, stackWarnRadius, eb.ent.ID) {
		d := fp.Pos.Sub(pos).Len()
		sep := eb.ent.Manager.Radius() + fp.Radius + destiny.ArrivalClearMargin
		level, stacked := stackLevel(d, sep)
		if !stacked {
			continue
		}
		m.logBot(eb.charID, level, "embody",
			"materialised %.2fm from another hull (needs %.0fm, born %s) — whichever writer produced this point is stacking ships",
			d, sep, eb.birthMode)
		return
	}
}

// ── the warp mirror ─────────────────────────────────────────────────────────
// Warp state on the client is built by the WarpTo command, not by a ball description, so the
// destination grid gets the same align and WarpTo stream as the departure grid and flies the
// warp in its own destiny.dll.

// armWarpMirror publishes the warping ball on its final approach to every connected pilot at the
// landing, with the WarpTo that flies it in, and returns the witnesses that took it. dm.Pos stays the
// departure point for the whole tunnel; a false ok from WarpMirrorPoint means the hop is too short to
// warp and phase 3 publishes at rest. See docs/comment-notes-b07.md#warp-mirror.
func (m *Mod) armWarpMirror(ss *destiny.SolarSystem, eb *embodiedBot, ent *destiny.Entity,
	dm *destiny.Manager, landing destiny.Vec3, warp []keyedAction) []*destiny.Entity {

	if ss == nil || ent == nil || dm == nil {
		return nil
	}
	pos, vel, ok := destiny.WarpMirrorPoint(landing, dm.Pos,
		destiny.WarpMirrorLeadSec, dm.EffectiveMaxVelocity())
	if !ok {
		return nil
	}
	bd := destiny.WarpMirrorBallDesc(dm.BallDesc(), pos, vel)
	bd.CorpID = uint32(eb.corpID)
	bd.Name = ent.CharName

	suffix := fmt.Sprintf(":%d", ent.ID)
	var out []*destiny.Entity
	for _, mate := range ss.PlayersAtPoint(landing, ent.ID) {
		if mate.Conn == nil || !mate.HasBallparkState() {
			continue
		}
		// The mirror is optional polish that costs a witness one forced stamp, so it is sent only when
		// StampLead() < 0, before ClaimUnknownBalls so a skipped witness is untouched. See docs/comment-notes-b07.md#warp-mirror.
		if mate.StampLead() >= 0 {
			continue
		}
		// Skips the departure grid's witnesses, which already hold the ball.
		if len(mate.ClaimUnknownBalls(ent.ID)) == 0 {
			continue
		}
		mate.QueueAddBalls(embodyAddKey, bd, ent.Slim, ent.Damage)
		// Each under its own key: a shared key would collapse the three actions to the last
		// (destiny/notify.go BuildWarpToActions).
		for _, ka := range warp {
			mate.QueueAction(ka.kind+suffix, ent.ID, ka.action)
		}
		mate.FlushQueuedActions(destiny.FlushNodeID, true)
		out = append(out, mate)
	}
	if len(out) > 0 {
		m.logBot(eb.charID, logInfo, "embody",
			"warp mirror armed for %d destination witness(es), %.0f m out",
			len(out), landing.Sub(pos).Len())
	}
	return out
}

// dropWarpMirror withdraws the mirrored ball from witnesses when the warp is abandoned before it
// lands; otherwise they hold a ball that never arrives and the core's add-only reconciliation never
// heals it. The HasPendingAdd arm is removeDeparted's: a removal must not outlive a pending add.
func (m *Mod) dropWarpMirror(mates []*destiny.Entity, ballID uint32) {
	rmKey := "rm:" + strconv.FormatUint(uint64(ballID), 10)
	for _, mate := range mates {
		if mate.Conn == nil {
			continue
		}
		if mate.HasPendingAdd(ballID) {
			mate.DropPendingAdd(ballID)
			mate.ForgetBalls(ballID)
			continue
		}
		// Retired by the flush, not here (see deEmbody and removeDeparted).
		mate.QueueRemoveBall(rmKey, ballID)
	}
}
