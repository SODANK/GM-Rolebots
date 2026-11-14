// Package rolebots is the RoleBots module: persistent pilots that animate
// existing player-made characters. Characters are added to a roster from the
// ESP admin panel ("Rolebot" section). Each has an identity (standings toward
// factions and pilots, a personality of six traits) and its activity follows
// from that (appetite.go): a timid pilot drifts to mining and hauling, an
// aggressive one lives in the belts.
//
// The module is compiled in with `go build -overlay` from
// GM-RoleBot-mod\mod-rebuild.ps1 and removed by a plain rebuild.
package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/esp"
	"gurimoon/modapi"
	"gurimoon/npc"
	"gurimoon/server/lsc"
	"gurimoon/server/station"
	"gurimoon/tick"
)

func init() { modapi.Register(&Mod{}) }

// Hook interfaces are matched by method set at Fire* time. These assertions
// break the build if core changes a signature.
var (
	_ modapi.Module              = (*Mod)(nil)
	_ modapi.SystemLoadedHook    = (*Mod)(nil)
	_ modapi.CharacterSelectGate = (*Mod)(nil)
	_ modapi.EntityRemovedHook   = (*Mod)(nil)
	_ modapi.SystemUnloadingHook = (*Mod)(nil)
)

// Mod carries the module's process-wide state across hooks.
type Mod struct {
	db       *sql.DB
	nodeID   uint64
	cfg      Config
	tick     *tick.Manager
	lsc      *lsc.Registry
	stations *station.StationRegistry
	systems  *destiny.SystemManager
	npc      *npc.NPCManager // embodied combat entry points (may be nil on old cores)
	// spawner owns belt population. The abstract tier asks it what is at a belt
	// in a system nobody has loaded (npc/beltwave.go). Nil on an older core,
	// where the abstract tier has nothing to fight.
	spawner  *npc.SpawnerManager
	sessions *esp.SessionRegistry // live player sessions (may be nil on old cores)
	simBusy  atomic.Bool          // one sim pass in flight at a time
	// One station-hull sweep in flight at a time (parklane.go).
	hullSweepBusy atomic.Bool
	gridBusy      atomic.Bool // one grid-lane pass in flight at a time
	// One ring pass in flight at a time (preload.go), with its own guard so a pass
	// waiting on a system build cannot hold up the next.
	preloadBusy atomic.Bool

	// The skill lane runs on its own clock (skilllane.go), so it has its own
	// overlap guard and sizes its batch against how long its passes take.
	skillBusy atomic.Bool
	skillPace atomic.Int64 // seconds the last skill pass took

	// The R&D enrolment lane (research.go) has its own clock: signing a pilot up
	// with a research agent needs no travel, so it does not ride the sim cadence.
	researchBusy atomic.Bool
}

func (m *Mod) Name() string { return "rolebots" }

// Init wires the module: tables, master switch, presence, ESP pages, ticks.
func (m *Mod) Init(ctx *modapi.Context) error {
	m.db = ctx.DB
	m.nodeID = ctx.NodeID
	m.tick = ctx.Tick
	m.lsc = ctx.LSC
	m.stations = ctx.Stations
	m.systems = ctx.Systems
	m.npc = ctx.NPC
	m.spawner = ctx.Spawner
	m.sessions = ctx.Sessions
	m.cfg = parseConfig(ctx.Config)

	// The module's own console window, opened before anything logs (console.go).
	// Failure is not fatal: output stays on the server console.
	m.startConsole()

	// The activity log's batching writer (log.go). Lines Init writes before this
	// point are inserted synchronously.
	m.startLogWriter()

	// Every task must be fully declared before anything runs one. This is fatal:
	// a task missing its Step, Posture or Label would break a bot silently.
	if err := validateTaskRegistry(); err != nil {
		return fmt.Errorf("rolebots: %w", err)
	}

	bctx := context.Background()
	if err := ensureTables(bctx, m.db); err != nil {
		return fmt.Errorf("rolebots: %w", err)
	}
	// The demand ledger's decay anchor and region (demandledger.go).
	if err := ensureDemandLedger(bctx, m.db); err != nil {
		return fmt.Errorf("rolebots: %w", err)
	}

	// Relabel generated pilots whose poseSource still carries a character name
	// (poses.go). After the first pass no row is in that format.
	m.scrubLegacyPoseSources(bctx)

	// Stop writing mail to synthetic pilots (mailsuppress.go). Installed before
	// any lane runs: the first war declaration or corp bill would get through.
	m.installMailSuppression()

	// Master switch: server_settings key survives restarts; config.json
	// "enabled" is only the default before the first ESP toggle.
	if err := evedb.EnsureServerSettingsTable(bctx, m.db); err != nil {
		return fmt.Errorf("rolebots: %w", err)
	}

	// Clear mail that arrived before the mail gate was installed. One-shot,
	// marked in server_settings.
	m.clearMailBacklog(bctx)
	m.logOrphanBots(bctx)

	// Fold the 30-day trade feed into the lifetime counters once (statcount.go).
	// It must run before any lane books a leg, or that leg is counted twice.
	m.seedTradeCounters(bctx)
	masterOn := m.cfg.Enabled
	if v, ok, err := evedb.GetServerSetting(bctx, m.db, settingMaster); err != nil {
		rlogf("rolebots: read master setting: %v (using config default)", err)
	} else if ok {
		masterOn = v == "1"
	}
	setMaster(masterOn)

	// The online population cap (popcap.go). Unlimited until somebody sets it.
	m.loadMaxOnline(bctx)
	// The hours each pilot plays (timezone.go), which log pilots off ahead of the
	// cap. On unless switched off.
	m.loadTZSettings(bctx)

	// The projection ceiling (projcap.go): how many bots may hold a ball in space
	// at once. Unlimited until set.
	m.loadMaxProjections(bctx)

	// Whether space is loaded ahead of a player (preload.go). Off unless set, and
	// off on a core with no system-loading seam.
	m.loadPreloadSetting(bctx)

	// How departures are paced and spaced at a station door (undockwave.go).
	// Defaults until set.
	m.loadUndockSettings(bctx)

	// How gate arrivals are paced (gatewave.go).
	m.loadGateSettings(bctx)

	// How fast bots may start warps in a watched system (warpbudget.go). In memory
	// before the grid lane starts: embodyTick reads both dials with no database.
	m.loadWarpBudgetSettings(bctx)

	// How long the activity log keeps a row (log.go).
	m.loadLogRetention(bctx)

	// The two roster-wide skill dials (skillsweep.go): the reconsider stamp and
	// the plan horizon. In memory before the skill lane starts.
	m.loadSkillSweepSettings(bctx)
	m.loadOreSettings(bctx)
	// Corp mining ops deliver to the office and pay in labour (corpfleetore.go).
	m.loadCorpOreSettings(bctx)
	// The corp-relations dials (relsettings.go). Before the snapshot below and
	// before anything plans: two are read from the embodied combat loop.
	m.loadRelationsSettings(bctx)
	m.loadWarSettings(bctx)
	// The alliance founding switch and cap (alliancesettings.go); the rest of
	// the alliance layer is always on.
	m.loadAllianceSettings(bctx)
	m.loadHumanSettings(bctx)
	// How often a capsule is finished (pod.go). Read here because
	// resolveTuning consults it per pilot and has no database on that path.
	m.loadPodKillChance(bctx)
	// The abstract law (pvplaw.go): CONCORD's rate and the law bounty.
	m.loadPvPDials(bctx)
	m.loadWarehouseSettings(bctx)
	m.loadShareSettings(bctx)
	m.loadLockdownSettings(bctx)
	// The war chest (corpdivisions.go): division profiles, Armory, Yard, Spoils.
	m.loadDivisionSettings(bctx)
	// Implants (implantupkeep.go): the switch and the budget share.
	m.loadImplantSettings(bctx)
	m.loadArmorySettings(bctx)
	m.loadArmoryHumanSettings(bctx)
	// A human never holds take on the war chest, however it was granted
	// (core db.CorpRoleFilter).
	evedb.CorpRoleFilter = m.corpRoleFilter
	// The filter never strips a bot, so the core need not look up a bot's
	// corporation first (a character_ read per member-info read).
	evedb.CorpRoleFilterApplies = func(charID int64) bool { return !m.isSynthetic(charID) }
	// Kits in members' hands (corparmory_issue.go): the roster loader reads
	// this before the first pass.
	m.loadLoanedKits(bctx)
	m.withdrawOriginalSales(bctx)
	// Old donations become shares once, before anybody can buy any
	// (corpshares.go migrateEquityToShares).
	m.migrateEquityToShares(bctx)
	m.loadSkillBudgetSettings(bctx)
	// Bot corporations' starbases (postables.go): the power, ice, sieges.
	m.loadStarbaseSettings(bctx)
	m.loadCareerSettings(bctx)
	// Open sites (sites.go): bots run the dungeons the core seeds.
	m.loadSiteSettings(bctx)
	// Agent missions (missions.go): bots take, run and turn in missions.
	m.loadMissionSettings(bctx)
	// Bot corporations' arms (corparm.go): who is where.
	m.refreshArmIndex(bctx)
	// Who holds which seat (corpposts.go): the per-pilot paths read the index.
	m.refreshPostIndex(bctx)

	// Residency, the home loop and the mineral supply duty are pinned on at init
	// (residency.go, mineralstock.go): no boot read, config key or panel control.

	// Gang formation switch and caps (gangform.go). The switch stays off until
	// flipped on the Gangs page.
	m.loadGangSettings(bctx)

	// Conscription: players commanding bots in their own fleets (conscript.go).
	// Off until switched on.
	m.loadConscriptSettings(bctx)
	// Chatter in corp chat (chatter.go): off until the panel switches it on.
	m.loadChatterSettings(bctx)
	// The Summit desk (summit.go): off until the panel switches it on. Its
	// budget and template memory come back from its own tables.
	m.loadSummitSettings(bctx)
	m.summitLoadMarks(bctx)
	m.summitLoadTplMem(bctx)
	// Every conscript row is stale at boot, since the core's gang registry
	// renumbers from 1: a surviving row would alias the next fleet with that number.
	m.releaseAllConscripts(bctx)
	// Portal-seated riders (fleetmirror.go) do not survive a restart either.
	m.purgeFleetRiders(bctx)

	// Self-founding (corpfound.go): off until switched on.
	m.loadCorpFoundSettings(bctx)
	// Attrition (corpmorale.go): off until switched on.
	m.loadCorpAttritionSettings(bctx)

	m.registerESP()

	// The corporation catalogue (corps.go): a policy row beside every NPC
	// corporation, seeded open bar a short list. Idempotent; operator edits stay.
	if err := m.ensureCorpPolicies(bctx); err != nil {
		rlogf("rolebots: corporation catalogue: %v", err)
	}
	// A change to the defaults reaches rows nobody has edited, once, and leaves
	// edited rows alone.
	if err := m.applyCorpDefaultGeneration(bctx); err != nil {
		rlogf("rolebots: corporation defaults: %v", err)
	}
	// The side a bot-run corporation flies for (allegiance.go): pledged once for
	// every corporation that has none, then declared to the NPC manager so its
	// rats spare the members.
	if err := m.migrateCorpAllegiance(bctx); err != nil {
		rlogf("rolebots: corporation allegiance backfill: %v", err)
	}
	if err := m.pushAllAllegiances(bctx); err != nil {
		rlogf("rolebots: corporation allegiances: %v", err)
	}
	// The seats those defaults decide (corprank.go). After the generation pass,
	// which settles a corporation's tags; the seat is read off the doctrine.
	if err := m.migrateSecuritySeats(bctx); err != nil {
		rlogf("rolebots: security seat migration: %v", err)
	}
	if err := m.migrateIndMiningSeats(bctx); err != nil {
		rlogf("rolebots: industry-mining seat migration: %v", err)
	}
	if err := m.migratePirateSeats(bctx); err != nil {
		rlogf("rolebots: piracy seat migration: %v", err)
	}
	if err := m.migrateTradeConvoys(bctx); err != nil {
		rlogf("rolebots: trade convoy retirement: %v", err)
	}
	if err := m.releaseTowerSavings(bctx); err != nil {
		rlogf("rolebots: tower savings release: %v", err)
	}
	// The old researched-original listings go home to their issuers once
	// (bpcnuke.go).
	if _, err := m.migrateBPOEscrowNuke(bctx); err != nil {
		rlogf("rolebots: print listing clear-out: %v", err)
	}

	// Character sheets that hold a skill without its prerequisites (an impossible
	// state some adopted characters had) are mended first (seed.go).
	m.repairRosterSkills(bctx)

	// How long the server was down (downtime.go), before any pass reads a
	// project clock. Not master-gated.
	m.recordBootDowntime(bctx)
	m.startHeartbeat()

	// What the last shutdown left behind (crashsweep.go). Before syncPresence,
	// whose Local-membership prune reads where each pilot is. Not master-gated.
	m.runCrashSweep(bctx)

	// Coordinate pile-ups on disk (parklane.go). A parked ship has no clock, and
	// a lane would put a ball on a stacked coordinate.
	m.destackParkedShips(bctx)
	// Ships parked inside a station's model, cleaned for the same reason.
	m.resettleStationHullShips(bctx)
	// The sweep keeps running, since the bay refills as bots undock (settleUndock
	// is roster-paced).
	m.startStationHullSweep()

	// Presence for already-enabled rolebots at boot, after the pilots outside
	// their hours are logged off (timezone.go).
	m.tzBootGate(bctx)
	m.syncPresence()

	// The abstract-tier sim loop.
	m.startSim()

	// The fast clock for the bots a player can see (gridlane.go). Its own lane:
	// the roster pass is sized by thousands of pilots and is too slow for it.
	m.startGridLane()

	// The ring (preload.go): systems one gate from a player, loaded and projected
	// before they arrive. Its own clock, since a system build is database work.
	m.startPreloadLane()

	// Skill investment, on its own clock (skilllane.go).
	m.startSkills()

	// R&D enrolment (research.go): industry-minded pilots sign up with a research
	// agent so their points buy patent tickets. Its own slow clock.
	m.startResearch()

	// The capability census (wants.go): what the roster could use, which
	// industrialists plan against. A forecast, so a slow clock.
	m.startCapabilitySweep()

	// Trade upkeep: the trade map and measured liquidity (traffic.go, liquidity.go)
	// and resting-order reconciliation (tradebook.go). These are sweeps.
	m.startTradeSweeps()

	// The ISK-flow ledger (iskflow.go): what the roster's wallets create,
	// destroy and move, read forward out of market_journal once a minute.
	m.startISKFlowSweep()

	// The one-time address reconsider (rehome.go): spreads the first corp-home
	// looks.
	m.startRehomeSpread(bctx)

	// The lifetime-counter flush (statcount.go). Not gated on the master switch:
	// what accumulated before it went off really happened.
	m.startStatsWriter()

	// Fleet formation (gangform.go): seated fleet commanders take corp-mates out
	// on the op their seat names. Who is seated is set in corprank.go.
	m.startGangFormation()
	m.startBotPvP()

	// Player-commanded pilots (conscript.go). Registration makes an invite to a
	// bot land; the lane keeps the cache, fleet-chat membership and release sweep.
	m.registerConscription()
	m.startConscription()

	// Fleets opened to human members (fleetmirror.go): the portal's "Request a
	// seat" turns a module fleet into a real gang; this lane keeps the two in line.
	m.startFleetMirror()
	// Bots talking in a corporation channel a person is reading (chatter.go).
	m.startChatter()
	// Bot drama on the Intergalactic Summit forum (summit.go).
	m.startSummit()
	// Orders from the chair (corpcommanddesk.go): what a human CEO or
	// Director posts on the Corporate Portal is carried out here.
	m.startCommandDesk()

	// The chain of command's weekly review (corprank.go), and the GM commands
	// that drive it on demand (/rolebots promote, slashcmd.go).
	m.loadPromoteClock(bctx)
	m.startPromotionSweep()
	m.registerSlashCommands()

	// Applications to player corporations (corpapply.go). A bot applies and waits;
	// this pass applies a director's answer and drops stale applications.
	m.startCorpApplicationSweep()

	// Bot CEOs (corpceo.go): a rostered pilot an operator put in a player
	// corporation's chair runs tags, directive, officers, titles and treasury.
	m.startCEOSweep()
	// Member limits and the "full" flag (corpopenings.go), kept up while CEOs are
	// logged off; after the CEO sweep, which loads the CEO index.
	m.startOpeningSweep()
	// Founding on demand (corpfound_demand.go): a corporation when the bots waiting
	// in NPC corporations far outnumber the free openings.
	m.startFoundingDemand()
	// Every managed corporation's titles are reconciled (corptitles.go), after the
	// CEO sweep that loads the CEO index the title plan reads.
	m.reconcileCorpTitlesAtBoot(bctx)
	// Its officers (corpstaff.go): treasurer, recruitment, departments.
	m.startStaffDeskSweep()
	// NPC chairs (npcchair.go): an NPC corporation run by its own NPC CEO.
	// The index loads now so the directive term reads it from the first plan.
	m.refreshNPCChairIndex(bctx)
	m.startNPCChairSweep()
	// The morale ledger and its shadow rolls (corpmorale.go).
	m.startMoraleSweep()
	// The core's starbase lifecycle events, as bot corporations hear them
	// (posevents.go): fuel-low, out-of-fuel, reinforced, destroyed.
	m.startStarbaseEvents()
	// Sov flips reach the alliance's book and mailbox (alliancesov.go).
	m.startAllianceSovHook()
	// The share desk's journal poller (corpshares.go): a player's Give Money
	// payment whose text says "shares" is a purchase order.
	m.startShareDesk()
	// The SCC Exchange (sccbots.go): listing CEOs, investors, market makers.
	m.startSCCBots(bctx)
	// Mind Clash (clash.go): clashers, venue errands, bettors and fixers.
	m.startClash(bctx)
	// GMMordu's Network (gmmjobs.go): freelance sign-ons and collects.
	m.startGMMJobs(bctx)

	// The corporation work board's pay (corpwork.go): delivered corporation
	// jobs, human- or bot-installed, are paid out of the corporation wallet.
	m.startCorpWageSweep()
	// The harvest levy (corplevy.go): a managed corporation's tax rate off its
	// bot members' ore sales, read from the market's fills.
	m.startLevySweep()

	// Corporate politics (corprel.go): declarations and wars. Its own periodic,
	// built inline first so no pilot plans blind.
	m.startRelationsSweep()
	// The relations ledger's fold (corprelations_fold.go): incidents into
	// values, tier crossings into standings, every few minutes.
	m.startRelationsFold()
	// The grace ledger (grace_ledger.go): what each bot holds against named pilots, decayed on its
	// own clock. Loaded inline so the folds read it from the first pass.
	m.loadGraceOpinions(bctx)
	m.startGraceSweep()

	// Who is what race, and the politics between peoples (tolerance.go). Loaded
	// inline: a xenophobe with no race knowledge would be neutral at first.
	m.startRaceSweep()

	// Expiry for the in-memory caches (cachesweep.go). Read-through expiry
	// only frees what somebody asks for again; this frees the rest.
	m.startCacheSweep()

	// The traffic dials (undock wave, gate wave, warp budget) re-read once a
	// minute, so a jammed hub can be eased with a setting (trafficsettings.go).
	m.startTrafficSettingsReload()

	// Retention for the ore-flow counter (oreflow.go). Its own pass: the cache
	// sweep is a no-I/O map walk, and this is a DELETE that wants an hour between runs.
	m.startOreFlowSweep()

	// Fleet-ore holds whose owner is no longer on the roster (fleetorestale.go):
	// residue nothing else collects. A live commander's haul is sold by sellHangarOre.
	m.startFleetOreStaleSweep()

	// One row per type in every bot's hangar and hold (stacking.go). The landing
	// sites keep it true; this repairs scatter already in the database.
	m.startStackSweep()

	// Flag the trade book's older resale legs so the end-user tape
	// (tradeenduse.go) covers its whole window. Once; off the boot path.
	goSafe("trade resale backfill", m.backfillTradeResale)

	rlogf("rolebots: initialised (master=%v)", masterOn)
	return nil
}

// OnSystemLoaded is called when a solar system becomes live (a player entered).
// Idempotent. Rolebots abstractly present in the system materialise.
func (m *Mod) OnSystemLoaded(ss *destiny.SolarSystem) {
	goSafe("system-loaded reconcile", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// fillLoadedSystem, not one reconcile: the birth budget caps a pass at
		// embodyMaxBirthsPerPass, so a player entering a busy hub needs the whole fill.
		m.fillLoadedSystem(ctx)
		// Module jetcans in this system go back on the grid, so a can made while
		// the system was dark is visible and openable on arrival.
		m.projectCansOnLoad(ss, ss.ID)
	})
}

// OnEntityRemoved is called when a player entity was destroyed and removed from
// space. If it is one of ours, the bot lost its ship: drop the projection and put
// the sim back to planning so it re-procures.
func (m *Mod) OnEntityRemoved(ss *destiny.SolarSystem, entityID uint32, killerID uint32) {
	eb := lookupProjectionByShip(entityID)
	if eb == nil {
		return
	}
	// This runs after the core removed the entity and sleeps out the pod grace, then
	// reaches back through eb for a ship and system that may no longer exist.
	m.goSafeBot(eb.charID, "ship-loss teardown", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		m.loseShip(ctx, eb, killerID)
	})
}

// OnSystemUnloading is called when a solar system is torn down (no real players
// left). Projections pinned to it go with it; the balls are gone, so nobody is
// notified.
func (m *Mod) OnSystemUnloading(solarSystemID int64) {
	m.dropProjectionsInSystem(solarSystemID)
}

// CharacterSelectGate refuses player login on a character whose rolebot
// assignment is enabled: the bot owns the character until disabled.
func (m *Mod) CharacterSelectGate(ctx context.Context, db *sql.DB, charID int64) error {
	rb, err := getRolebot(ctx, db, charID)
	if err != nil {
		rlogf("rolebots: select gate charID=%d: %v (allowing)", charID, err)
		return nil
	}
	if rb != nil && rb.Enabled {
		// Sentence-cased: the gate's error text is shown to the player as the refusal.
		//lint:ignore ST1005 user-facing message, not an error string
		return errors.New("This character is currently animated by RoleBot and cannot be played. Disable it in the Rolebot panel first.")
	}
	// A capsuleer is logging in: the Summit desk's idle gate (summit.go).
	m.summitNoteLogin(charID)
	return nil
}
