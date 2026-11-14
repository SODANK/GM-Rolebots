package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/rates"
)

// Mining activity, abstract tier. The bot boards its mining ship, picks the
// best-priority ore reachable under its security floor and region rules (reading the
// OreDistribution the belt generator uses), travels there, fills its hold on a timer,
// then sells to NPC buy orders or stockpiles. A gang stores miningConfig as JSON; a
// solo pilot's is derived every pass by miningConfigDerived (activitycfg.go). The
// "refine" disposal reprocesses the load and lists the minerals, the only path that
// makes a mineral supply other bots can buy; resolveMiningDisposal chooses it per trip
// from the wallet and the pilot's refining yield.

type miningConfig struct {
	OrePriority       []string `json:"orePriority"`
	SecFloor          *float64 `json:"secFloor"`
	AvoidHostileBelts bool     `json:"avoidHostileBelts"`
	// Disposal is omitempty so "let the fleet decide" is an absent key: gangConfigHas
	// reads the raw blob to tell an instruction from a zero value, and a written ""
	// would read as an instruction.
	Disposal           string `json:"disposal,omitempty"`
	StockpileStationID int64  `json:"stockpileStationID"`
	// RefineStationID is where a "refine" disposal takes the load: the pilot's own
	// factory. Separate from StockpileStationID: one is where ore is put, the other is
	// where it is turned into something and sold.
	RefineStationID int64 `json:"refineStationID"`
	// ByValue ranks the ores by what they are worth (oreDigValues) instead of by
	// OrePriority. A miner's own trips run by value; a project's materials run and a gang
	// list keep their order. Derived, never read from a config blob.
	ByValue bool `json:"-"`
	// Strict limits the search to OrePriority: a system holding none of it is not a
	// candidate. A project's materials run wants the minerals on its bill, and any other
	// rock is a hold the factory does not need. Derived, never read from a config blob.
	Strict bool `json:"-"`
	// ListOre rests the hold on the book instead of feeding a bid (oreask.go). It rides
	// the "sell" disposal (same travel and station search); miningSell falls through to a
	// sale. Not an operator field: resolveMiningDisposal decides it, so no json tag.
	ListOre bool `json:"-"`
}

// defaultOrePriority is the sensible classic hierarchy, top tier first.
// Mercoxit is excluded by default (needs a deep core miner fitted); a pilot
// carrying one has it added to its value table (oreDigValues).
var defaultOrePriority = []string{
	"Arkonor", "Bistot", "Crokite", "Dark Ochre", "Gneiss", "Spodumain",
	"Hedbergite", "Hemorphite", "Jaspet", "Kernite", "Omber",
	"Plagioclase", "Pyroxeres", "Scordite", "Veldspar",
}

// Mining skills.
const (
	skillMining       = 3386
	skillAstrogeology = 3410
	miningCycleSec    = 60.0
)

// What a belt system is worth, in jumps a bot will fly for it. There is no search
// radius: the sweep runs until nothing unexplored could beat what it has
// (findMiningTarget).
const (
	// beltOreJumpWorth is how far a bot will fly for one tier better ore: eight jumps
	// per step up the priority list.
	beltOreJumpWorth = 8
	// beltHostileCost is what a ratted belt costs a pilot that asked to avoid them:
	// steep, but not a wall, since an off-list ore in a hostile system beats sitting docked.
	beltHostileCost = 25
	// beltOreShareCost is what a system where the wanted ore is a rumour costs, in jumps,
	// applied through a knee (Arkonor is 1 part in 100 in shallow null, 11 in 100 deeper).
	// It stays below one ore tier (beltOreJumpWorth): a tie-break, not grounds to switch ore.
	beltOreShareCost = 6
	// beltOreShareKnee is where that curve bends: below about 5% the ore is a rumour and
	// the cost is near its maximum. Share/(share+knee), not a threshold, so there is no cliff.
	beltOreShareKnee = 0.05
	// beltRecentCost nudges a bot out of the system it just emptied.
	beltRecentCost = 4
	// beltPreferredBonus is the head start a preferred region gets.
	beltPreferredBonus = 5
)

// How often the abstract tier re-checks a hold that real mining is filling.
const realMiningPollSec = 20

// What a money-mining trip's full hold becomes (resolveMiningDisposal).
const (
	// miningCashFloorDefault: below this the wallet is hurting and ore goes to the best
	// bid for cash today. Sized like gangSubsidyDonorFloor (broke.go): a replacement
	// frigate and fit. Patience bends it per pilot (Tuning.MiningCashFloor).
	miningCashFloorDefault = 2_000_000.0
	// oreIntrinsicSpread is the ore-to-mineral market spread: ore basePrice is 0.75 of the
	// intrinsic value of what it refines into (sql/ore_prices.sql), so a station-yield
	// refiner loses against selling the rock and a skilled one wins.
	oreIntrinsicSpread = 0.75
	// refineWorthEdge is the premium refining must clear over selling the rock: the wait,
	// the broker fee and the batch hold cost something. 0.75 x 1.15 is about 0.86 after-tax
	// yield; untrained fails, a few levels of Refining and Refinery Efficiency clear it.
	refineWorthEdge = 1.15
)

// miningRules are the rules a belt search runs under, scoped by residency
// (residency.go): a miner works its own region's rocks, the coherent behaviour and the
// biggest saving in the module. findMiningTarget climbs out of the scope when the
// region has no belt it will touch.
func (m *Mod) miningRules(ctx context.Context, b *botRun, cfg miningConfig) travelRules {
	// startBand, not defaultBand: a region this pilot has just worked out has
	// no belt for it is skipped rather than walked again (residency.go).
	band := startBand(b.CharID, defaultBand(b))
	r := m.economicRules(ctx, b, band)
	if cfg.SecFloor != nil {
		r.SecFloor = *cfg.SecFloor
	}
	// No comfort line on a mining trip (beltCost, router.go hopCost): what makes a belt
	// dangerous is measured and priced by the belt walk, so security alone is not a danger.
	r.SecAversion = 0
	return r
}

// errNoFitAvailable marks a plan that failed because no fit in the module suits this
// pilot for this job: none built for the role, or every one beyond its skills. Both
// are facts about the roster, not bot failures: the rotation skips the slot (planSolo).
var errNoFitAvailable = errors.New("no usable fit")

// errNoFitGenerated is the sharper half of errNoFitAvailable: the generator produced
// nothing this pilot could fly or buy. It still matches errNoFitAvailable; the
// industrialist's project reads the difference (industryMineFor) to avoid a desert.
var errNoFitGenerated = errors.New("no fit generated")

// errNoBeltForOre is a strict search (miningConfig.Strict) that found none of
// its named rock anywhere it may fly. The project reads it as "the belts are
// not the answer" and falls back to what it holds (industryMineFor).
var errNoBeltForOre = errors.New("no belt yields the ore")

// strictMiningMaxBand is how far out a strict search widens.
const strictMiningMaxBand = bandNear2

// miningPlan is entered from idle: make sure the bot has (and is in) a mining ship,
// procuring one if needed, then head for ore.
func (m *Mod) miningPlan(ctx context.Context, b *botRun) error {
	m.resolveStandbyMining(ctx, b)
	if m.resolveMiningDisposal(ctx, b) {
		return nil // gone to sell what it already holds for cash first
	}
	cfg := miningConfigDerived(b)
	st := b.State
	st.Activity = ActMining

	ready, err := m.ensureActivityShip(ctx, b, ActMining)
	if err != nil || !ready {
		return err
	}
	return m.miningHeadForOre(ctx, b, cfg)
}

// ensureRefineryBase settles a refining miner on a factory to refine at, so its ore
// comes back as minerals on the market. The refine disposal needs
// Profile.IndustryStationID, which only rememberIndustryBase wrote. Best effort and
// quiet: a miner that cannot settle refines wherever it lands (miningDispose).
func (m *Mod) ensureRefineryBase(ctx context.Context, b *botRun) {
	if b.Profile == nil || b.Profile.IndustryStationID != 0 {
		return
	}
	// A materials run inside a project already has its factory and stockpiles
	// there; leave it alone.
	if b.State.Data.IndFactoryID != 0 {
		return
	}
	// Scoped to home (industryFactoryStationIn): the pick is kept in
	// Profile.IndustryStationID, so an unscoped one would move a miner out of region for good.
	stationID, _, err := m.industryFactoryStationIn(ctx, b,
		m.economicRules(ctx, b, bandHome))
	if err != nil {
		return
	}
	m.rememberIndustryBase(ctx, b, stationID)
}

// resolveStandbyMining decides once per plan whether this trip is standby work for a
// factory pilot with nothing left to install: every line busy, the pilot likes the
// belt (industryMinesOwnOre) and has a settled factory. It stockpiles there for the
// next project's industryShortfall. Database-backed, so not in miningConfigDerived.
func (m *Mod) resolveStandbyMining(ctx context.Context, b *botRun) {
	d := &b.State.Data
	d.StandbyFactoryID = 0
	// A live project owns its own materials runs (miningConfigDerived case 1); standby
	// is only for the pilot between projects.
	if d.IndFactoryID != 0 || b.Profile == nil {
		return
	}
	if b.Profile.IndustryStationID == 0 || !industryMinesOwnOre(b.Tune) {
		return
	}
	// Standby mining only runs when every line is full: a pilot with every oven baking
	// fills the larder for the next batch.
	linesFull := false
	if free, err := m.industryFreeSlots(ctx, b); err == nil && free == 0 && len(d.IndCooking) > 0 {
		linesFull = true
	}
	// A cold build book or a free line is not a reason to mine: that pilot falls through
	// to its ordinary appetites.
	if !linesFull {
		return
	}
	d.StandbyFactoryID = b.Profile.IndustryStationID
	// What to dig is the ordinary value choice (findMiningTargetIn): the load comes
	// home to the factory either way.
	m.logBot(b.CharID, logInfo, "mining",
		"standby mining — every manufacturing line is busy, so this trip stockpiles minerals at %s for the next project",
		m.stationLabel(ctx, d.StandbyFactoryID))
}

// resolveMiningDisposal decides once per plan what this trip's full hold becomes: ore
// sold for cash by a hurting wallet, refined and listed when the yield pays, else
// listed as rock. It lands on State.Data (carried by idleData). Reports true when the
// pilot was sent to raise cash from what it holds (cashInjectionPlan); the plan stops.
func (m *Mod) resolveMiningDisposal(ctx context.Context, b *botRun) bool {
	d := &b.State.Data
	d.MineDisposal, d.MineRefineStationID = "", 0
	// A standby trip's disposal is already decided: stockpile at the pilot's factory
	// (miningConfigDerived case 1.5). A materials run in a project owns it too (case 1).
	if d.StandbyFactoryID != 0 || d.IndFactoryID != 0 || b.Profile == nil {
		return false
	}
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		d.MineDisposal = "sell" // the safe read of an unreadable wallet
		return false
	}
	// Under the line, holding nothing, owning rock somewhere: sell that first. A
	// non-empty hold is already headed to a bid by the "sell" disposal below.
	if bal < b.Tune.MiningCashFloor {
		if held, herr := m.cashHoldGoods(ctx, b); herr == nil && len(held) == 0 {
			if took, cerr := m.cashInjectionPlan(ctx, b, false); cerr != nil {
				rlogf("rolebots: mining cash injection char=%d: %v", b.CharID, cerr)
			} else if took {
				return true
			}
		}
	}
	// A hurting wallet takes the cash today. The sale is bounded (oreSellMaxJumps,
	// miningDispose): a pilot that must fly further lists the rock instead.
	if b.Broke || bal < b.Tune.MiningCashFloor {
		d.MineDisposal = "sell"
		m.logBot(b.CharID, logInfo, "mining",
			"wallet at %s, under its %s line — selling ore to the best bid in reach until it recovers",
			iskString(bal), iskString(b.Tune.MiningCashFloor))
		return false
	}
	// A member of a corporation building from ore (corpbuyback.go) sells it the load; the
	// sale still weighs the corporation's bid against the market (loyalSell).
	if m.corpWantsMemberOre(ctx, b.CorpID) {
		d.MineDisposal = "sell"
		m.logBot(b.CharID, logInfo, "mining",
			"selling this trip's ore toward its corporation's buyback — the corporation is building from it")
		return false
	}
	// Otherwise the load goes where it is worth most to this pilot: its own refinery when
	// its yield beats what the rock fetches, else the ore market.
	worth, yield, why := m.miningRefineWorth(ctx, b, refineWorthEdge)
	if !worth {
		d.MineDisposal = "list"
		m.logBot(b.CharID, logInfo, "mining",
			"listing the rock on the market — %s", why)
		return false
	}
	d.MineDisposal = "refine"
	m.ensureRefineryBase(ctx, b) // best-effort; 0 stays "wherever it lands"
	d.MineRefineStationID = b.Profile.IndustryStationID
	m.logBot(b.CharID, logInfo, "mining",
		"refining this trip — %.0f%% yield makes it pay (%s)", yield*100, why)
	return false
}

// miningRefineWorth reports whether refining pays at this pilot's yield: the after-tax
// yield must clear oreIntrinsicSpread (what ore already sells for, as a share of its
// refined value) plus edge (refineEdgeUnder, mineralstock.go). The why string names
// the bar actually used.
func (m *Mod) miningRefineWorth(ctx context.Context, b *botRun, edge float64) (bool, float64, string) {
	// The ore-specific skill stays out of it (refineHandAt gets a zero group) because the
	// rock is not known yet; leaving it out only understates the yield.
	src := "an ordinary refinery"
	stationID := b.Profile.IndustryStationID
	if stationID != 0 {
		if _, err := evedb.GetStationReprocessingInfo(ctx, m.db, stationID); err == nil {
			src = "its own refinery"
		} else {
			stationID = 0 // a station that does not reprocess is not its refinery
		}
	}
	net := m.refineHandAt(ctx, b, stationID, 0).Net()
	bar := oreIntrinsicSpread * edge
	why := fmt.Sprintf("%.0f%% after tax at %s, against the %.0f%% the ore market already pays",
		net*100, src, bar*100)
	return net >= bar, net, why
}

// ensureActivityShip gets the bot into the ship the fit it picked for this job calls
// for. It reports ready=false when it took the bot somewhere else first (procuring a
// hull, finishing an outfit, flying to the ship); the activity resumes after that trip.
// The checks run in a fixed order: fit, hull exists, right hull, fit complete, ammo, boarded.
func (m *Mod) ensureActivityShip(ctx context.Context, b *botRun, activity string) (bool, error) {
	// Which fit is it flying? Every fit built for this job is a candidate
	// and the answer moves with its skills and its wallet (fitselect.go).
	a, t, err := m.selectFitForActivity(ctx, b, activity)
	if err != nil {
		return false, err
	}
	if a == nil {
		return false, fmt.Errorf("%w: %w: no %s fit this pilot can fly — the generator found nothing for its skills (fitgen.go)",
			errNoFitAvailable, errNoFitGenerated, jobLabel(jobFor(activity)))
	}
	// Ship lost (destroyed?): clear the record and procure.
	if a.ShipItemID != 0 && !a.ShipExists {
		m.logBot(b.CharID, logWarn, activity,
			"its %s ship %d no longer exists (destroyed?) — reprocuring", activity, a.ShipItemID)
		if err := setFitShip(ctx, m.db, b.CharID, a.FitID, 0); err != nil {
			return false, err
		}
		a.ShipItemID = 0
	}
	if a.ShipItemID == 0 {
		// A starter hull is issued rather than bought (rookie.go): nothing here sells one, so
		// procurement could only end at an empty till. After this the hull is like any other.
		granted, err := m.grantRookieHull(ctx, b, a, t)
		if err != nil {
			return false, err
		}
		if !granted {
			return false, m.startProcurement(ctx, b, a, t, activity)
		}
	}
	// The ship exists, but is it the fit? Procurement records the hull when bought, so a
	// trip that ran out of sellers leaves a bare hull; finish the shopping list first.
	if sameHull, err := m.shipHullMatches(ctx, a.ShipItemID, t.Fit); err != nil {
		return false, err
	} else if !sameHull {
		m.logBot(b.CharID, logError, activity,
			"ship %d is not a %s — reprocuring", a.ShipItemID, t.Fit.HullName)
		if err := setFitShip(ctx, m.db, b.CharID, a.FitID, 0); err != nil {
			return false, err
		}
		return false, m.startProcurement(ctx, b, a, t, activity)
	}
	// Plan each module the ship wears in the slot it sits in, so a module one slot over
	// is not read as missing and as a stray (alignFitToShip).
	t = m.alignedToShip(ctx, t, a.ShipItemID)
	missing, err := m.shipMissing(ctx, a.ShipItemID, t.Fit)
	if err != nil {
		return false, err
	}
	// A destitute pilot is issued the free lines of the fit before anything decides it is
	// short of them (broke.go): a shopping trip cannot buy a Basic Miner on an empty wallet.
	if b.Broke && len(missing) > 0 {
		// Substitute first, then issue: the starter kit is matched by typeID, and a fit's
		// missing line is rarely one of those five (brokestarter.go).
		missing = m.substituteStarterLines(ctx, b, missing)
		missing = m.grantStarterLines(ctx, b, a, missing)
	}
	if len(missing) > 0 {
		// An outstanding line that is only ammo goes through the same cooldown as a restock
		// run, or a bot whose charge nobody stocks re-flies the same errand every pass.
		switch {
		case shopIsConsumablesOnly(missing) && b.State.Data.RestockAfter > clock.Unix():
			m.logBot(b.CharID, logInfo, activity,
				"still short of %s, but the last run found no seller — working with what it has",
				shopSummary(missing))
		case fitProcureCoolingDown(&b.State.Data, a.FitID):
			// Hard lines are missing and the last trip found no seller: fly short-fitted while that
			// does the job (fitshort.go), else the activity is off the table until the market changes.
			if ok, why := m.shortFitVerdict(ctx, t, missing); !ok {
				return false, fmt.Errorf(
					"%w: its %s ship is missing %s, nothing in reach sells them, and it will not fly half-fitted — %s",
					errNoFitAvailable, activity, shopSummary(missing), why)
			}
			m.logBot(b.CharID, logInfo, activity,
				"still short of %s and nothing in reach sells them — working short-fitted",
				shopSummary(missing))
		case b.Broke:
			// Same answer as no seller: a shopping trip it cannot pay for is not a trip. Fly what
			// is on the ship while that still does the job.
			if ok, why := m.shortFitVerdict(ctx, t, missing); !ok {
				return false, fmt.Errorf(
					"%w: its %s ship is missing %s, it cannot pay for them, and it will not fly half-fitted — %s",
					errCannotAfford, activity, shopSummary(missing), why)
			}
			m.logBot(b.CharID, logInfo, activity, "%s", brokeShortFitNote(missing))
		default:
			return false, m.resumeOutfit(ctx, b, a, t, missing, activity)
		}
	}
	// Teeth: an unarmed ship is refused every ratted belt (findMiningTargetIn's toothless
	// rule), so restart the shopping list for a gun; a destitute pilot is issued one (broke.go).
	if beltWork(activity) && m.shipHasNoTeeth(ctx, a.ShipItemID) {
		if handled, err := m.rearmToothless(ctx, b, a, t, activity); err != nil || handled {
			return false, err
		}
	}

	// Ammo. The fit says what a full ship carries; the hold says what is left. Only a
	// hold that is genuinely running dry is worth a trip to a market.
	if restock, err := m.ammoCheck(ctx, b, a, t); err != nil {
		return false, err
	} else if len(restock) > 0 {
		return false, m.startRestock(ctx, b, a, t, restock, activity)
	}

	// A mining job needs a miner on the hull, and nothing above asks: the fit-ship record
	// can name a gunboat. Ask before any undock and send it shopping.
	if jobFor(activity) == JobMining && !m.shipCanMine(ctx, a.ShipItemID) {
		if len(missing) > 0 {
			m.logBot(b.CharID, logInfo, activity,
				"its %s has no miner fitted — outfitting before it flies", t.Fit.HullName)
			return false, m.resumeOutfit(ctx, b, a, t, missing, activity)
		}
		// Nothing outstanding yet nothing mines: the record disagrees with the hull. Drop the
		// record and buy the fit afresh; resumeOutfit with an empty list is a silent no-op.
		m.logBot(b.CharID, logInfo, activity,
			"ship %d is recorded as its %s fit but carries no miner — reprocuring (the hull stays in the hangar)",
			a.ShipItemID, t.Fit.HullName)
		if err := setFitShip(ctx, m.db, b.CharID, a.FitID, 0); err != nil {
			return false, err
		}
		a.ShipItemID = 0
		return false, m.startProcurement(ctx, b, a, t, activity)
	}

	if b.ShipID == a.ShipItemID {
		return true, nil
	}
	// Where is the ship? Docked ships live in a station; the bot must be
	// there to board.
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
	m.logBot(b.CharID, logInfo, activity, "traveling to pick up its %s ship at station %d", activity, shipLoc)
	// One board task for every activity: boardStep reads the activity off the state,
	// since a phase built by concatenation (activity + ".board") dispatches to nothing.
	b.State.Activity = activity
	err = m.beginTravel(ctx, b, loc.SolarSystemID, shipLoc, string(TaskBoard))
	if errors.Is(err, errNoRoute) {
		// The ship is stranded where this pilot cannot reach (an outlaw's hull left in
		// empire): not a fault, so no halt; the record is let go and a hull is kitted out here.
		b.State.Activity = ""
		return m.abandonStrandedHull(ctx, b, a, t, activity, shipLoc, err)
	}
	return false, err
}

// abandonStrandedHull releases a hull the pilot cannot reach and starts a replacement
// from where it stands: a fresh starter hull when the fit is a rookie one (rookie.go),
// otherwise the ordinary procurement trip. It reports like ensureActivityShip.
func (m *Mod) abandonStrandedHull(ctx context.Context, b *botRun, a *fitShip, t *fitTemplate,
	activity string, shipLoc int64, cause error) (bool, error) {
	m.logBot(b.CharID, logInfo, activity,
		"its %s ship is stranded at station %d (%v) — leaving it there and kitting out a hull from here",
		activity, shipLoc, cause)
	if err := setFitShip(ctx, m.db, b.CharID, a.FitID, 0); err != nil {
		return false, err
	}
	a.ShipItemID, a.ShipExists = 0, false
	granted, err := m.grantRookieHullAt(ctx, b, a, t, true)
	if err != nil {
		return false, err
	}
	if granted {
		// The hull is in the hangar the pilot is standing in. Run the ladder again so the
		// rack is completed before anything boards; it cannot loop, the ship is here now.
		return m.ensureActivityShip(ctx, b, activity)
	}
	return false, m.startProcurement(ctx, b, a, t, activity)
}

// ── hostile NPCs on belts ───────────────────────────────────────────────────
//
// Hostile means rats that would shoot this pilot are standing there, not that rats
// could appear. The flag reads the persisted belt population (db/beltwavedb.go):
// npcBeltRats is a belt's real contents loaded or not, npcLiveEntities the balls in
// a watched system. Those tables are lazy, so no rows means unvisited, not empty.
// That case is not priced: almost every system is unvisited, so charging it would
// make every known-clear system beat every unknown one by 25 jumps and send the
// roster to wherever patrols swept. Margin (threat.go) prices what might be
// waiting, and beltTooHot is the backstop on arrival.

// beltThreatChecker returns a memoised predicate for "hostile NPCs on this system's
// belts", resolved once per system per search. Hostile means to this pilot, by
// factionHostile (faction.go), the rule the arrival check uses too: a pirate-corp
// miner is not kept out of space it can park in all day. A player corp is faction 0.
func (m *Mod) beltThreatChecker(ctx context.Context, b *botRun) func(sysID, regionID int64, security float64) bool {
	mine := int64(0)
	if b != nil {
		mine = b.FactionID
	}
	var mx map[[2]int64]float64
	if mine != 0 {
		var ok bool
		// An unreadable matrix means every rat is hostile, as in every other caller.
		if mx, ok = m.standingsMatrix(ctx); !ok {
			mine = 0
		}
	}

	// Both censuses are read whole, once, as (system, faction) pairs: a search walks
	// thousands of systems and only hundreds have rats on record. Live data, read per search.
	occupied := map[int64]bool{}
	for _, q := range []string{
		// Balls in space, in systems a client currently has loaded.
		`SELECT DISTINCT solarSystemID, factionID FROM npcLiveEntities`,
		// And the persisted waves, every belt anything has visited, loaded or not. alive = 1
		// excludes belts cleared and waiting out their respawn: a swept belt is quiet.
		`SELECT DISTINCT solarSystemID, factionID FROM npcBeltRats WHERE alive = 1`,
	} {
		pairs, err := m.censusPairs(ctx, q)
		if err != nil {
			// A census that will not read contributes nothing; the other still answers. Failing
			// to hostile would stop every miner on a missing table, while quiet costs one trip.
			rlogf("rolebots: belt threat census: %v", err)
			continue
		}
		for _, pr := range pairs {
			id, faction := pr[0], pr[1]
			// mine == 0 is every pilot in a player corp: factionHostile answers true for it, so
			// the branch is skipped rather than asked.
			if mine == 0 || factionHostile(mx, mine, faction) {
				occupied[id] = true
			}
		}
	}

	return func(sysID, _ int64, _ float64) bool { return occupied[sysID] }
}

// censusPairs is one (system, faction) census, shared across the roster for
// a minute: the two reads above have no pilot in them and a belt search runs
// per pilot per plan. The faction filter stays per pilot, in Go.
func (m *Mod) censusPairs(ctx context.Context, q string) ([][2]int64, error) {
	sctx, cancel := snapContext(ctx)
	defer cancel()
	return snapshotFor(snapKey("belt-census", q), time.Minute, func() ([][2]int64, error) {
		rows, err := m.db.QueryContext(sctx, q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out [][2]int64
		for rows.Next() {
			var id, faction int64
			if rows.Scan(&id, &faction) != nil {
				continue
			}
			out = append(out, [2]int64{id, faction})
		}
		return out, rows.Err()
	})
}

// quietBeltAdvice explains a hostile pick in terms an operator can act on. The flag
// is the belt census (beltThreatChecker): rats are standing on the belts now, so kill
// them or give the pilot something that can. The spawner settings decide what turns
// up next, so the rule is quoted.
func (m *Mod) quietBeltAdvice(ctx context.Context, security float64) string {
	rule, err := evedb.GetSpawnerRuleForSecurity(ctx, m.db, security)
	if err != nil || rule == nil {
		return "no spawner rule covers this security, so nothing new will spawn here — " +
			"the rats on these belts are what is already standing there, and clearing them clears it"
	}
	return fmt.Sprintf(
		"every belt in range has rats on it. They respawn under the rule %q, which allows up to %d ship(s) "+
			"at security %.1f — a patrol clears the ground a miner then works (the distress board), and the "+
			"spawner settings (ESP → Region Spawns) decide what comes back",
		rule.RuleName, rule.MaxShips, security)
}

// beltTarget is one system the belt search considered good enough to fly to.
type beltTarget struct {
	SysID   int64
	SysName string
	Sec     float64
	Jumps   int
	Ore     string
	OreID   int64
	Rank    int // index in the bot's ore priority list; len(list) = off-list
	// ValueGap is how many doublings of value this system's best rock sits
	// under the best rock the pilot could reach (oreValueGap); 0 on a trip
	// ranked by name, where Rank carries the preference instead.
	ValueGap float64
	Hostile  bool // hostile NPCs are on its belts, or can spawn there
	Recent   bool // this bot has mined here in the last few trips
	Bitten   bool // a rolebot was actually attacked here recently (hostilemem.go)
	// Enemy marks a system a corporation this pilot's employer counts as an enemy is
	// working or keeps an address in (corprel.go). Unlike Hostile and Bitten it is about
	// people, so a patrol cannot clear it.
	Enemy bool
	// Margin is this pilot's (or its pack's) engage margin against what the region's
	// belts spawn (threat.go). 0 = unread: no hull to measure, flag-based pricing only.
	Margin float64
	// How much ore the system has left, 0..1; 1 is untouched, pristine or never worked
	// by a bot. See beltbudget.go: it makes a worked-out system expensive.
	Fullness float64
	// OreShare is the chosen ore's share of this system's distribution table, 0..1.
	// Arkonor is 1 rock in 100 in shallow null and 11 in 100 deep in, so membership alone
	// would send an expedition where the ore is a rumour. 0 means unread.
	OreShare float64
	// HoldFillSec is how long this pilot's hold takes to fill (miningYield),
	// read once per search. It sets what a better rock is worth in jumps
	// (oreJumpWorthFor). 0 = unread.
	HoldFillSec float64
	// NoBidNear marks a system with no ore buyer within oreSellMaxJumps
	// (orebidreach.go): a hold mined there is stockpiled, not sold.
	NoBidNear bool
}

// findMiningTarget picks the best belt system a bot can reach, reading the map outward
// and counting only systems with belts. Ranking: belts clear of hostile NPCs, ore
// priority, preferred regions, somewhere not just worked, fewest jumps; every
// preference degrades and the fallback is logged. Residency scopes the walk (residency.go).
func (m *Mod) findMiningTarget(ctx context.Context, b *botRun, cfg miningConfig) (*beltTarget, string, error) {
	return m.findMiningTargetPass(ctx, b, cfg)
}

// findMiningTargetPass is the whole search: the pilot's own scope first, a
// retreat to safer space when every belt would kill the hull, then wider.
func (m *Mod) findMiningTargetPass(ctx context.Context, b *botRun, cfg miningConfig) (*beltTarget, string, error) {
	r := m.miningRules(ctx, b, cfg)
	retreated := false
	for {
		t, note, err := m.findMiningTargetIn(ctx, b, cfg, r)
		if err == nil {
			return t, note, nil
		}
		// Every belt in reach would kill this hull: raise the security floor and look again
		// in safer space, once. An activity's own floor is an operator decision and is kept.
		if !retreated && errors.Is(err, errAllBeltsHopeless) && cfg.SecFloor == nil &&
			r.SecFloor < beltRetreatFloor {
			retreated = true
			m.logBot(b.CharID, logInfo, "threat",
				"nothing in reach is survivable in this hull — pulling back to %.1f security and looking again",
				beltRetreatFloor)
			r.SecFloor = beltRetreatFloor
			continue
		}
		// A strict search (a project's named rock) stops at the two-hop neighbourhood: past
		// it the walk is the whole map per pilot.
		if cfg.Strict && r.Band >= strictMiningMaxBand {
			return nil, "", err
		}
		wider, ok := m.widen(ctx, b, r)
		if !ok {
			return nil, "", err
		}
		if noteScopeFail(b.CharID, r.Band) {
			m.logBot(b.CharID, logInfo, "residency",
				"no belt worth working in %s — looking further out (%s)", m.scopeNote(ctx, r), wider.Band)
		}
		// The activity's own security override survives the widening: it is an explicit
		// operator setting.
		if cfg.SecFloor != nil {
			wider.SecFloor = *cfg.SecFloor
		}
		r = wider
	}
}

// findMiningTargetIn is one rung: the branch-and-bound belt walk under
// exactly these rules, with no widening of its own.
func (m *Mod) findMiningTargetIn(ctx context.Context, b *botRun, cfg miningConfig,
	r travelRules) (*beltTarget, string, error) {

	u, err := m.universe(ctx)
	if err != nil {
		return nil, "", err
	}
	prefer := m.homeRegionSet(ctx, b)

	sovFor := func(sysID int64) int64 { return u.sovFaction(ctx, m.db, sysID) }
	threat := m.beltThreatChecker(ctx, b)
	hasBelts := func(sysID int64) bool { return u.hasBelts(ctx, m.db, sysID) }

	rank := map[string]int{}
	for i, name := range cfg.OrePriority {
		if _, dup := rank[name]; !dup {
			rank[name] = i
		}
	}
	offList := len(cfg.OrePriority)
	// A trip the pilot takes for itself ranks rock by what it is worth
	// (oreDigValues); the name list only orders a project's materials run and
	// an operator's gang list. Read once for the whole sweep.
	var oreVal map[string]float64
	topVal := 0.0
	// Whether this ship can work Mercoxit at all (minercap.go). Without a deep core miner
	// it is never worth picking a system for.
	canMerc := m.canMineMercoxit(ctx, b)
	if cfg.ByValue {
		if oreVal = m.oreDigValues(ctx, b, canMerc); oreVal != nil {
			topVal = topOreValue(oreVal, r.SecFloor)
		}
	}

	// Systems this bot has worked lately: everything else equal it goes elsewhere,
	// since the belt it just emptied is the worst one to fly back to.
	recent := map[int64]bool{}
	for _, id := range b.State.Data.RecentSystems {
		recent[id] = true
	}

	// A ship with nothing that can shoot back refuses hostile belts instead of pricing
	// them: HostileCost is a nerve setting, for pilots with a say in the fight.
	toothless := cfg.AvoidHostileBelts && m.shipHasNoTeeth(ctx, b.ShipID)
	// Whether this pilot's employer has anybody to avoid at all (corprel.go), hoisted
	// so the common case costs one boolean.
	watchEnemies := cfg.AvoidHostileBelts && relHasEnemies(b.CorpID)

	// The pilot's (on a gang op, the pack's) weight class, read once; each candidate's
	// spawn threat prices in as jumps through beltCost (threat.go).
	botDPS, botEHP, powerKnown := m.effectivePower(ctx, b)

	// How worked-over each system is, read once: the search touches thousands of
	// systems and cannot query each.
	fullnessOf, err := m.systemFullnessMap(ctx)
	if err != nil {
		return nil, "", err
	}
	// How long this hold takes to fill, and where an ore buyer can be reached from.
	// Both are priced by beltCost, read once, and degrade to unread, not to a failure.
	fillSec := m.searchHoldFillSec(ctx, b)
	bidReach, err := m.oreBidReach(ctx, u)
	if err != nil {
		rlogf("rolebots: ore bid reach: %v", err)
		bidReach = nil
	}

	// Cost is in jumps, with this pilot's own weights (scoring.go). A signed mine job's
	// pull (gmmjobs.go, zero without GMMordu) is the one bonus, so the floor subtracts it.
	jobPullMax := gmmMinePullMax(b.CharID)
	cost := func(t *beltTarget, preferred bool) float64 {
		return beltCost(b.Tune, t, preferred, cfg.AvoidHostileBelts) - gmmMinePull(b.CharID, t.SysID)
	}

	var best *beltTarget
	bestCost := 0.0
	scanned, noBelts, blockedSec, blockedScope := 0, 0, 0, 0
	hopelessSkipped := 0
	// The nearest few systems the margin turned away (BFS is nearest-first): the
	// distress signal if the whole search comes up empty (beltdistress.go).
	var hopelessSys []distressCand

	seen := map[int64]bool{b.SystemID: true}
	frontier := []int64{b.SystemID}
	for depth := 0; len(frontier) > 0; depth++ {
		// Branch and bound instead of a radius: beltSearchFloor (kept in step with
		// beltCost) is the cheapest any system at this depth could be; once worse, stop.
		if best != nil && beltSearchFloor(b.Tune, depth)-jobPullMax > bestCost {
			break
		}
		for _, sysID := range frontier {
			ms := u.meta[sysID]
			scanned++
			if !hasBelts(sysID) {
				noBelts++
				continue
			}
			ores := destiny.OreDistribution(ms.Security, sovFor(sysID))
			// Drop what this system's field was lately found not to hold: the
			// table says what can spawn, the seed decided what did (orememory.go).
			if absent := oreKnownAbsent(sysID); len(absent) > 0 {
				kept := ores[:0:0]
				for _, w := range ores {
					if !absent[w.Name] {
						kept = append(kept, w)
					}
				}
				ores = kept
			}
			if len(ores) == 0 {
				continue
			}
			bestRank, bestOre, bestWeight := offList, "", 0
			totalWeight := 0
			valueGap := 0.0
			if oreVal != nil {
				// The dearest rock that spawns here; the larger seam breaks a tie.
				bestVal := -1.0
				for _, w := range ores {
					totalWeight += w.Weight
					if w.Name == oreMercoxit && !canMerc {
						continue
					}
					if v := oreVal[w.Name]; v > bestVal || (v == bestVal && w.Weight > bestWeight) {
						bestVal, bestOre, bestWeight = v, w.Name, w.Weight
					}
				}
				bestRank = 0
				valueGap = oreValueGap(topVal, bestVal)
			} else {
				for _, w := range ores {
					totalWeight += w.Weight
					if w.Name == oreMercoxit && !canMerc {
						continue
					}
					if rk, ok := rank[w.Name]; ok && rk < bestRank {
						bestRank, bestOre, bestWeight = rk, w.Name, w.Weight
					}
				}
			}
			if bestOre == "" && cfg.Strict {
				continue // none of the rock this trip is for
			}
			if bestOre == "" {
				// Nothing the bot asked for spawns here, but ore does: keep it as a last resort
				// behind every listed ore.
				bestOre, bestWeight = ores[0].Name, ores[0].Weight
			}
			// How much of this system's rock is the rock the pilot came for.
			share := 0.0
			if totalWeight > 0 {
				share = float64(bestWeight) / float64(totalWeight)
			}
			fullness := 1.0
			if f, ok := fullnessOf[sysID]; ok {
				fullness = f
			}
			hostile := threat(sysID, ms.RegionID, ms.Security)
			bitten := recentHostileEncounter(botScope(b), sysID)
			// A hunter cleared this system's spawn lately: the belt is open, which lets a
			// miner flood back into space the table calls hostile.
			covered := patrolCovered(botScope(b), sysID)
			if covered {
				hostile, bitten = false, false
			}
			// A rival corporation working here or keeping an address here. Patrol cover does
			// not clear it: dead rats do not make a rival stop being one.
			enemy := watchEnemies && relEnemySystem(b.CorpID, sysID)
			if toothless && (hostile || bitten || enemy) {
				continue // no belt is worth a ship that cannot fight for it
			}
			cand := &beltTarget{
				SysID: sysID, Sec: ms.Security, Jumps: depth,
				Ore: bestOre, Rank: bestRank, ValueGap: valueGap,
				Hostile:  hostile,
				Recent:   recent[sysID],
				Bitten:   bitten,
				Enemy:    enemy,
				Fullness: fullness,
				OreShare: share,

				HoldFillSec: fillSec,
				NoBidNear:   bidReach != nil && !bidReach[sysID],
			}
			// Under patrol cover the margin is skipped (margin 0): a hunter just proved nothing
			// is standing here. beltTooHot still reads the real rats on arrival.
			if powerKnown && !covered {
				// minerMargin, not threatMargin: this pilot is choosing a belt, not a fight, and
				// the fight test refuses every ratted belt to a mining hull (threat.go).
				cand.Margin = minerMargin(botDPS, botEHP,
					m.spawnThreatFor(ctx, ms.RegionID, ms.Security))
				// Below the refusal floor the margin says the ship dies, so skip the system as the
				// hunt searches do (bounty.go, gangplan.go). The floor is minerHopeless, not marginRefuse.
				if minerHopeless(cand.Margin) {
					hopelessSkipped++
					if len(hopelessSys) < beltDistressMaxPerPass {
						hopelessSys = append(hopelessSys, distressCand{
							SysID: sysID, RegionID: ms.RegionID, Sec: ms.Security})
					}
					continue
				}
			}
			c := cost(cand, prefer[ms.RegionID])
			// Ties split randomly so a whole roster does not land on
			// whichever system the search happened to reach first.
			if best == nil || c < bestCost || (c == bestCost && rand.Intn(2) == 0) {
				best, bestCost = cand, c
			}
		}
		// Expand, counting what the rules turned away for the diagnostic.
		var next []int64
		for _, sysID := range frontier {
			for _, nb := range u.adj[sysID] {
				if seen[nb] {
					continue
				}
				seen[nb] = true
				nm, ok := u.meta[nb]
				if !ok {
					continue
				}
				if dispSec(nm.Security) < r.SecFloor {
					blockedSec++
					continue
				}
				// A security ceiling is a hard line too, as route() enforces.
				if r.SecCeil > 0 && dispSec(nm.Security) >= r.SecCeil {
					blockedSec++
					continue
				}
				// The residency scope prunes the walk here, not in u.allowed, because this search
				// expands on its own: the frontier stops at the region line.
				if len(r.Regions) > 0 && !r.Regions[nm.RegionID] {
					blockedScope++
					continue
				}
				next = append(next, nb)
			}
		}
		frontier = next
	}

	if best == nil && hopelessSkipped > 0 {
		// Everything reachable would kill this hull, so fly somewhere safer (findMiningTarget
		// retries with a higher floor). Only this total failure raises distress (beltdistress.go).
		if wrote := m.noteBeltDistress(ctx, b.CharID, b.CorpID, hopelessSys, distressSourceSearch); wrote > 0 {
			m.logBot(b.CharID, logInfo, "threat",
				"distress signal raised — %d nearby belt system(s) are unwinnable in this hull and need a combat sweep", wrote)
		}
		return nil, "", fmt.Errorf("%w: %d reachable belt system(s) would kill this hull",
			errAllBeltsHopeless, hopelessSkipped)
	}
	if best == nil && cfg.Strict {
		return nil, "", fmt.Errorf("%w: none of %s spawns anywhere it will fly to (scanned %d system(s))",
			errNoBeltForOre, strings.Join(cfg.OrePriority, ", "), scanned)
	}
	if best == nil {
		here := m.nameOfSystem(ctx, b.SystemID)
		scopeNote := ""
		if blockedScope > 0 {
			scopeNote = fmt.Sprintf(", %d sit outside the space this pilot's economy is scoped to", blockedScope)
		}
		return nil, "", fmt.Errorf(
			"no belt system anywhere this pilot will fly to: scanned %d system(s) (%d without asteroid belts); "+
				"%d neighbour(s) sit below the hard space limit%s%s. "+
				"Sitting in %s (security %.1f)",
			scanned, noBelts, blockedSec, r.limitNote(), scopeNote,
			here, dispSec(u.meta[b.SystemID].Security))
	}

	oreTypeID, _, err := m.oreType(ctx, best.Ore)
	if err != nil {
		return nil, "", fmt.Errorf("ore %q is not in this database: %w", best.Ore, err)
	}
	best.OreID = oreTypeID
	best.SysName = m.nameOfSystem(ctx, best.SysID)

	// Say which preference had to bend, and whether the answer was worth a real
	// journey, so a bot that committed to fourteen jumps shows it on the board.
	var note string
	switch {
	case best.Rank >= offList:
		note = fmt.Sprintf(
			"nothing on the ore priority list spawns anywhere it can reach — mining %s in %s (%d jumps) instead",
			best.Ore, best.SysName, best.Jumps)
	case cfg.AvoidHostileBelts && best.Bitten:
		note = fmt.Sprintf(
			"a rolebot was attacked in %s inside the last hour and nowhere quieter was worth the trip — mining there anyway, eyes open",
			best.SysName)
	case cfg.AvoidHostileBelts && best.Hostile:
		note = fmt.Sprintf("no belt clear of hostile NPCs was worth the trip — mining %s (%d jumps) anyway. %s",
			best.SysName, best.Jumps, m.quietBeltAdvice(ctx, best.Sec))
	case best.Jumps >= beltOreJumpWorth:
		note = fmt.Sprintf("nothing close by was worth working — flying %d jumps out to %s for %s",
			best.Jumps, best.SysName, best.Ore)
	}
	return best, note, nil
}

// beltTooHot reads the rats actually standing at a belt and says, with a reason for
// the log, whether this pilot should work it. The bar is minerHopeless (can it get
// back off the belt: minerMargin, threat.go), not a fraction of a fight. Rats that
// will not fight this pilot are not counted (faction.go).
func (m *Mod) beltTooHot(ctx context.Context, b *botRun, beltID int64) (string, bool) {
	if m.spawner == nil {
		return "", false
	}
	dps, ehp, known := m.effectivePower(ctx, b)
	if !known {
		// No hull to measure. Unknown is not danger (threat.go).
		return "", false
	}
	sec := 1.0
	if u, err := m.universe(ctx); err == nil {
		sec = u.meta[b.SystemID].Security
	}
	spawn, err := m.hostileRatsAtBelt(ctx, b, beltID, b.SystemID, sec)
	if err != nil || len(spawn) == 0 {
		return "", false
	}
	th := spawnThreatOf(spawn)
	margin := minerMargin(dps, ehp, th)
	if !minerHopeless(margin) {
		return "", false
	}
	return fmt.Sprintf("%s is sitting on it and this hull cannot get back off the belt (%s)",
		describeSpawn(spawn), minerVerdict(margin)), true
}

// errAllBeltsHopeless is findMiningTargetIn saying it found belts and refused
// every one of them on the arithmetic. Distinct from "no belts" because the
// remedy is different: safer space, not more space.
var errAllBeltsHopeless = errors.New("every reachable belt is unwinnable in this hull")

// beltRetreatFloor is the security a miner pulls back to when nothing it can
// reach is survivable. High-sec proper, not the 0.45 boundary: the point is to
// get the hull somewhere its own guns are not the deciding factor.
const beltRetreatFloor = 0.5

// recentSystemMemory is how many belt systems back a bot remembers having
// worked. Long enough to break a two-system ping-pong, short enough that a
// small pocket of space still gets revisited.
const recentSystemMemory = 4

// idleData empties the state blob as every "this trip is over" path does, but
// carries the facts about the bot rather than the trip. It copies whole state
// families, never half of one (TestIdleDataIsFamilyWise); each family file says
// which it is. Worked belts are carried so the next pick moves on.
func idleData(prev StateData) StateData {
	return StateData{
		AppetiteState: prev.AppetiteState,
		StickyState:   prev.StickyState,
		OrderState:    prev.OrderState,
		TradeMemory:   prev.TradeMemory,
		CombatMemory:  prev.CombatMemory,
		PiracyMemory:  prev.PiracyMemory,
		MineMemory:    prev.MineMemory,
		IndState:      prev.IndState,
		RndState:      prev.RndState,
		LabState:      prev.LabState,
		CourierState:  prev.CourierState,
		MissionState:  prev.MissionState,
	}
}

// pushRecent adds sysID to the front of the recent list, without duplicates,
// keeping it at most depth long. depth is the pilot's own memory
// (Tuning.RecentSystemMemory): a nomad remembers further back and so
// ranges wider, a settled one forgets quickly and works a small pocket.
func pushRecent(list []int64, sysID int64, depth int) []int64 {
	if depth < 1 {
		depth = 1
	}
	out := make([]int64, 0, depth)
	out = append(out, sysID)
	for _, id := range list {
		if id == sysID {
			continue
		}
		if len(out) == depth {
			break
		}
		out = append(out, id)
	}
	return out
}

// miningHeadForOre picks the target ore + system and starts travel.
func (m *Mod) miningHeadForOre(ctx context.Context, b *botRun, cfg miningConfig) error {
	st := b.State
	target, note, err := m.findMiningTarget(ctx, b, cfg)
	if err != nil {
		return err
	}
	if note != "" {
		// A note is a preference that bent, not a trip that failed: it comes with a target
		// the pilot is heading for, so it is logged at info and not buried among errors.
		m.logBot(b.CharID, logInfo, "mining", "%s", note)
	}
	recent := pushRecent(st.Data.RecentSystems, target.SysID, b.Tune.RecentSystemMemory)
	st.Data = idleData(st.Data)
	st.Data.TargetSystemID = target.SysID
	st.Data.OreTypeID = target.OreID
	st.Data.OreName = target.Ore
	st.Data.RecentSystems = recent
	m.logBot(b.CharID, logInfo, "mining", "plan — %s in %s (%d jump(s), security %.1f)",
		target.Ore, target.SysName, target.Jumps, target.Sec)
	return m.beginTravel(ctx, b, target.SysID, 0, "mining.mine")
}

// miningStep advances the mining FSM phases.
func (m *Mod) miningStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	cfg := miningConfigDerived(b)

	switch st.Phase {
	case "mining.mine":
		// A projection with its lasers running is mining for real. The abstract tier
		// stops conjuring and just watches the hold fill.
		if m.embodiedMining(b.CharID) {
			if err := m.settleOwedOre(ctx, b); err != nil {
				return false, err
			}
			return false, m.miningWatchHold(ctx, b, cfg)
		}
		if st.Data.OreUnits == 0 {
			// What is actually floating here: the search used destiny.OreDistribution (what
			// can spawn), but rocks were rolled once from a stored seed (beltfield.go).
			family, famName, ok, err := m.miningRealOre(ctx, b, cfg)
			if err != nil {
				return false, err
			}
			if !ok {
				m.logBot(b.CharID, logInfo, "mining",
					"the belts here are worked out — nothing left worth a laser, moving on")
				st.Data.RecentSystems = pushRecent(st.Data.RecentSystems, b.SystemID, b.Tune.RecentSystemMemory)
				st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
				return false, saveState(ctx, m.db, st)
			}
			if family != st.Data.OreTypeID {
				m.logBot(b.CharID, logInfo, "mining",
					"no %s in these rocks after all — working %s instead", st.Data.OreName, famName)
				st.Data.OreTypeID, st.Data.OreName = family, famName
			}

			// Park on the belt holding the most of it, so the embodied projection appears
			// where a miner belongs and this leg debits the rocks under the ship.
			beltID := int64(0)
			if b.ShipID != 0 {
				beltID = m.parkAtBelt(ctx, b.ShipID, b.SystemID, family)
			}
			// No belt, no rocks, no leg: every real-rock call below is fenced behind
			// beltID != 0, so a zero would skip the debit and let delivery invent the load.
			if beltID == 0 {
				m.logBot(b.CharID, logInfo, "mining",
					"no belt to work here after all — moving on")
				st.Data.RecentSystems = pushRecent(st.Data.RecentSystems, b.SystemID, b.Tune.RecentSystemMemory)
				st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
				return false, saveState(ctx, m.db, st)
			}
			// Look before working: the search chose this system from the region's expected
			// spawn (threat.go). This is the only read that sees the real rats, before a cycle.
			if why, bail := m.beltTooHot(ctx, b, beltID); bail {
				m.logBot(b.CharID, logInfo, "threat",
					"not working this belt — %s", why)
				// The strong distress: real rats confirmed standing, not
				// an estimate. One row for the system in front of it.
				if u, err := m.universe(ctx); err == nil {
					ms := u.meta[b.SystemID]
					m.noteBeltDistress(ctx, b.CharID, b.CorpID, []distressCand{{
						SysID: b.SystemID, RegionID: ms.RegionID, Sec: ms.Security,
					}}, distressSourceBelt)
				}
				st.Data.RecentSystems = pushRecent(st.Data.RecentSystems, b.SystemID, b.Tune.RecentSystemMemory)
				st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
				return false, saveState(ctx, m.db, st)
			}
			units, fillSec, rate, err := m.miningYield(ctx, b, st.Data.OreTypeID)
			if err != nil {
				return false, err
			}
			// Who else is working these belts: the relations ledger's one
			// economic signal (corprelations_hooks.go).
			m.noteBeltPresence(ctx, b.CorpID, b.SystemID)
			if units <= 0 {
				// The backstop behind ensureActivityShip's shipCanMine gate. Not a failure toward
				// a halt: the next plan pass meets the gate and goes shopping for a miner.
				m.logBot(b.CharID, logInfo, "mining",
					"mining ship yields nothing — no miners fitted; back to plan for an outfit")
				st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
				return false, saveState(ctx, m.db, st)
			}
			// miningYield sizes the leg to an empty hold. Ore the lasers left aboard when the
			// projection went away takes room, and a load debited past the free space would be
			// lost at delivery, so the leg is cut to what fits.
			free, ferr := m.holdFreeM3(ctx, b.CharID, b.ShipID)
			if ferr != nil {
				return false, fmt.Errorf("mining leg: %w", ferr)
			}
			famVol, verr := evedb.GetTypeVolume(ctx, m.db, family)
			if verr != nil || famVol <= 0 {
				famVol = 0.1
			}
			if fits := holdCapUnits(units, free, famVol); fits < units {
				if fits <= 0 {
					return false, m.miningHoldFullAtStart(ctx, b, cfg, family)
				}
				fillSec = fillSec * fits / units
				if fillSec < 1 {
					fillSec = 1
				}
				units = fits
			}

			// Take it out of the real rocks now, debiting asteroidDepletion as a player's lasers
			// do; debiting up front stops forty bots each being promised the last rock.
			lots, got, terr := m.beltTakeReal(ctx, b.SystemID, beltID, family, units)
			if terr != nil {
				return false, terr
			}
			if got <= 0 {
				// Worked out. Remember the system and re-plan; the depletion is already priced
				// into beltCost.
				m.logBot(b.CharID, logInfo, "mining",
					"this belt is stripped — moving on")
				st.Data.RecentSystems = pushRecent(st.Data.RecentSystems, b.SystemID, b.Tune.RecentSystemMemory)
				st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
				return false, saveState(ctx, m.db, st)
			}
			if got < units {
				// A partial load: the belt gave what it had. Shorten the
				// leg to match so the timer still reflects the work.
				fillSec = fillSec * got / units
				if fillSec < 1 {
					fillSec = 1
				}
				units = got
			}
			// The lot list is the leg's receipt: these exact rocks were debited, so these
			// variants are what the hold may receive. It is never empty past this point.
			st.Data.OreLots = lots

			st.Data.MineBeltID = beltID
			st.Data.OreUnits = units
			st.Data.YieldM3PerS = rate
			st.Data.LegEndsAt = clock.Unix() + fillSec
			m.logBot(b.CharID, logInfo, "mining", "mining %s — hold full in %dm%ds", st.Data.OreName, fillSec/60, fillSec%60)
			return false, saveState(ctx, m.db, st)
		}
		// Hold is full: the ore becomes real cargo variant by variant (pickVariant,
		// MinedOreType). Nothing is delivered that was not debited: no lots, no ore.
		lots := st.Data.OreLots
		if len(lots) == 0 {
			m.logBot(b.CharID, logInfo, "mining",
				"came away empty — nothing was taken from these rocks")
			st.Data.OreUnits, st.Data.OreLots = 0, nil
			st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
			return false, saveState(ctx, m.db, st)
		}
		if _, err := m.deliverOreLots(ctx, b, lots); err != nil {
			return false, err
		}
		if have, err := m.cargoOreUnits(ctx, b, lots); err == nil && have > 0 {
			st.Data.OreUnits = have
		}
		m.logBot(b.CharID, logInfo, "mining", "hold full — %d units of %s%s",
			st.Data.OreUnits, st.Data.OreName, m.oreLotNote(ctx, lots))
		return false, m.miningDispose(ctx, b, cfg)

	case "mining.sell":
		return true, m.miningSell(ctx, b)

	case "mining.stockpile":
		if err := m.moveOreToHangar(ctx, b, st.Data); err != nil {
			return false, err
		}
		// Recorded before idleData clears the lot list. A stockpile is neither a sale nor
		// a loss, and must be visible so shares do not read a larder as vanished ore.
		m.noteOreFlowLots(ctx, oreStageStockpiled, oreLotUnits(st.Data), 0)
		m.logBot(b.CharID, logInfo, "mining", "stockpiled %s at %s", st.Data.OreName, m.stationLabel(ctx, b.StationID))
		m.minerReboardMiningShip(ctx, b)
		st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
		return false, saveState(ctx, m.db, st)

	case "mining.refine":
		return true, m.miningRefineAndList(ctx, b)
	}
	return false, fmt.Errorf("unknown mining phase %q", st.Phase)
}

// deliverOreLots puts a leg's debited lots into the hold, each capped by the free
// space, and books them as mined. It returns the units delivered. Units that do not
// fit are lost: the rock was already debited.
func (m *Mod) deliverOreLots(ctx context.Context, b *botRun, lots []oreLot) (int64, error) {
	// An unreadable hold is not an unlimited one. The caller keeps the lots, so the
	// next tick retries.
	free, err := m.holdFreeM3(ctx, b.CharID, b.ShipID)
	if err != nil {
		return 0, fmt.Errorf("mining delivery: %w", err)
	}
	var delivered int64
	for _, lot := range lots {
		if lot.Units <= 0 {
			continue
		}
		oreVol, verr := evedb.GetTypeVolume(ctx, m.db, lot.TypeID)
		if verr != nil || oreVol <= 0 {
			oreVol = 0.1
		}
		units := holdCapUnits(lot.Units, free, oreVol)
		if units <= 0 {
			continue
		}
		if err := m.addCargoStack(ctx, b, lot.TypeID, units); err != nil {
			return delivered, err
		}
		free -= float64(units) * oreVol
		delivered += units
		// Booked here and not in addCargoStack, which also carries loot, jetcan pickups
		// and gang settlements: this is ore the pilot cut out of a rock (statcount.go).
		mined := units
		bumpStats(b.CharID, func(d *statDelta) {
			d.OreUnits += mined
			d.OreM3 += float64(mined) * oreVol
		})
		// The same event on the roster's ore-flow ledger (oreflow.go), for the Ore page's
		// shares. Valued at nothing: a rock in the hold is not priced yet.
		m.noteOreFlow(ctx, lot.TypeID, oreStageMined, mined, 0)
		m.emitOreMined(ctx, b.CharID, lot.TypeID, mined, float64(mined)*oreVol, b.SystemID, true)
	}
	return delivered, nil
}

// miningHoldFullAtStart handles a leg that would start with no room in the hold, which
// happens after real mining filled it and the projection went away. Ore of the planned
// family aboard is taken to market as a load; with none of it aboard the pilot re-plans.
func (m *Mod) miningHoldFullAtStart(ctx context.Context, b *botRun, cfg miningConfig, family int64) error {
	st := b.State
	lots, err := m.cargoOreLots(ctx, b, family)
	if err != nil {
		return err
	}
	var units int64
	for _, lot := range lots {
		units += lot.Units
	}
	if units == 0 {
		return m.miningHoldClogged(ctx, b)
	}
	st.Data.OreLots, st.Data.OreUnits = lots, units
	m.logBot(b.CharID, logInfo, "mining", "hold already full — %d units of %s aboard", units, st.Data.OreName)
	return m.miningDispose(ctx, b, cfg)
}

// settleOwedOre runs when a projection starts mining in the middle of an abstract
// leg. The leg already debited its rocks, so its lots go into the hold now and
// OreUnits returns to 0, the "lasers running" value miningWatchHold expects. A
// stale OreUnits would make the abstract delivery repeat the hold after de-embody.
func (m *Mod) settleOwedOre(ctx context.Context, b *botRun) error {
	st := b.State
	if st.Data.OreUnits == 0 {
		return nil
	}
	if len(st.Data.OreLots) > 0 {
		got, err := m.deliverOreLots(ctx, b, st.Data.OreLots)
		if err != nil {
			return err
		}
		m.logBot(b.CharID, logInfo, "mining",
			"in space now — %d unit(s) of %s from this leg are aboard, lasers take over", got, st.Data.OreName)
	}
	st.Data.OreUnits, st.Data.OreLots = 0, nil
	return saveState(ctx, m.db, st)
}

// miningWatchHold is the mining.mine step while the bot is mining for real
// in space. The ore is already arriving in the hold cycle by cycle, so the
// abstract tier only decides when the trip is over.
func (m *Mod) miningWatchHold(ctx context.Context, b *botRun, cfg miningConfig) error {
	st := b.State
	free, err := m.holdFreeM3(ctx, b.CharID, b.ShipID)
	if err != nil {
		return err
	}
	// Read the hold by family, not one typeID: the lasers credit each rock's own variant
	// (mine_embodied.go, MinedOreType).
	lots, err := m.cargoOreLots(ctx, b, destiny.BaseOreType(st.Data.OreTypeID))
	if err != nil {
		return err
	}
	var units int64
	for _, lot := range lots {
		units += lot.Units
	}
	st.Data.OreLots = lots
	// The projection has toured every belt and found none of the planned ore: take
	// what is aboard to market, or re-plan; the stripped system joins the recent list.
	if m.embodiedOreDry(b.CharID) {
		st.Data.RecentSystems = pushRecent(st.Data.RecentSystems, b.SystemID, b.Tune.RecentSystemMemory)
		if units > 0 {
			st.Data.OreUnits = units
			m.logBot(b.CharID, logInfo, "mining",
				"belts here are out of %s — selling the %d unit(s) already aboard", st.Data.OreName, units)
			return m.miningDispose(ctx, b, cfg)
		}
		m.logBot(b.CharID, logInfo, "mining",
			"belts here are out of %s and the hold is empty — re-planning", st.Data.OreName)
		idle := idleData(st.Data)
		st.Activity, st.Phase, st.Data = "", "idle", idle
		return saveState(ctx, m.db, st)
	}

	// One more cycle would not fit: call it a load and go sell it.
	oreVol, err := evedb.GetTypeVolume(ctx, m.db, st.Data.OreTypeID)
	if err != nil || oreVol <= 0 {
		oreVol = 0.1
	}
	if free > oreVol {
		// OreUnits stays 0 while the lasers run: it marks ore owed to the hold by the
		// abstract tier, and setting it would conjure the same load if the player logs off.
		st.Data.LegEndsAt = clock.Unix() + realMiningPollSec
		return saveState(ctx, m.db, st)
	}
	if units == 0 {
		// Hold full of something else entirely: nothing to sell of the planned ore.
		return m.miningHoldClogged(ctx, b)
	}
	st.Data.OreUnits = units
	m.logBot(b.CharID, logInfo, "mining", "hold full — %d units of %s mined from real rocks", units, st.Data.OreName)
	return m.miningDispose(ctx, b, cfg)
}

// miningHoldClogged handles a mining leg whose hold is full of none of the planned
// ore. Minerals from an earlier job are the usual cargo: docked, the pilot unloads
// them and re-plans; in space, it docks at the nearest station it will use, where
// plan unloads them. With no minerals aboard the pilot idles and the rotation moves on.
func (m *Mod) miningHoldClogged(ctx context.Context, b *botRun) error {
	st := b.State
	oreName := st.Data.OreName
	idle := func() error {
		d := idleData(st.Data)
		d.LegEndsAt = clock.Unix() + idleRetrySec
		st.Activity, st.Phase, st.Data = "", "idle", d
		return saveState(ctx, m.db, st)
	}
	var minerals int64
	for _, q := range m.cargoMinerals(ctx, b) {
		minerals += q
	}
	if minerals == 0 {
		m.logBot(b.CharID, logError, "mining", "hold is full but holds no %s — idling", oreName)
		return idle()
	}
	if b.StationID != 0 {
		n := m.unloadMinerals(ctx, b)
		m.logBot(b.CharID, logInfo, "mining",
			"hold was full of %d unit(s) of minerals from an earlier job, not %s — unloaded them into its hangar",
			n, oreName)
		return idle()
	}
	stationID, sysID, err := m.nearestAllowedStation(ctx, b, rulesForBot(b), m.homeRegionSet(ctx, b))
	if err == nil && stationID != 0 && sysID == 0 {
		if loc, lerr := evedb.GetStationLocation(ctx, m.db, stationID); lerr == nil {
			sysID = loc.SolarSystemID
		}
	}
	if err != nil || stationID == 0 || sysID == 0 {
		m.logBot(b.CharID, logError, "mining",
			"hold is full of %d unit(s) of minerals, not %s, and no station is in reach to unload them — idling",
			minerals, oreName)
		return idle()
	}
	m.logBot(b.CharID, logInfo, "mining",
		"hold is full of %d unit(s) of minerals from an earlier job, not %s — docking at %s to unload",
		minerals, oreName, m.stationLabel(ctx, stationID))
	st.Activity, st.Data = "", idleData(st.Data)
	return m.beginTravel(ctx, b, sysID, stationID, string(TaskIdle))
}

// cargoOreLots reads the ship's cargo back as a lot list: every variant of one ore
// family aboard, biggest first. It is the embodied tier's half of beltTakeReal:
// real rocks hit grades the plan did not choose, so the load is discovered from the
// hold. familyID 0 asks for every ore aboard.
func (m *Mod) cargoOreLots(ctx context.Context, b *botRun, familyID int64) ([]oreLot, error) {
	if b.ShipID == 0 {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity) FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.ownerID = ? AND e.flag = 5 AND g.categoryID = 25
		GROUP BY e.typeID`, b.ShipID, b.CharID)
	if err != nil {
		return nil, fmt.Errorf("cargo ore: %w", err)
	}
	defer rows.Close()
	var out []oreLot
	for rows.Next() {
		var lot oreLot
		if err := rows.Scan(&lot.TypeID, &lot.Units); err != nil {
			return nil, err
		}
		if lot.Units <= 0 {
			continue
		}
		if familyID != 0 && destiny.BaseOreType(lot.TypeID) != familyID {
			continue
		}
		out = append(out, lot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Units != out[j].Units {
			return out[i].Units > out[j].Units
		}
		return out[i].TypeID < out[j].TypeID
	})
	return out, nil
}

// cargoUnitsOf counts units of one type sitting in the ship's cargo.
func (m *Mod) cargoUnitsOf(ctx context.Context, b *botRun, typeID int64) (int64, error) {
	var units sql.NullInt64
	if err := m.db.QueryRowContext(ctx, `
		SELECT SUM(quantity) FROM entity
		WHERE locationID = ? AND ownerID = ? AND typeID = ? AND flag = 5`,
		b.ShipID, b.CharID, typeID).Scan(&units); err != nil {
		return 0, fmt.Errorf("cargo units: %w", err)
	}
	return units.Int64, nil
}

// miningDispose routes the full hold to a buyer or a stockpile station. The mining
// security floor gates where the bot mines, not where it hauls: a 0.9 floor in a
// 0.8 pocket would otherwise strand a full hold. Hauling uses the profile's floor.
func (m *Mod) miningDispose(ctx context.Context, b *botRun, cfg miningConfig) error {
	st := b.State
	r := rulesForBot(b)
	prefer := m.homeRegionSet(ctx, b)

	// Ship swap: a solo miner with a trade ship at this station boards it for the haul
	// leg; the sell phase re-boards the mining ship afterwards.
	if st.Data.MiningReturnFitID == 0 && st.Data.GangID == 0 && b.StationID != 0 {
		m.minerMaySwapToHauler(ctx, b)
	}

	// Refine at the pilot's factory. With no pinned station (activitycfg.go) it returns
	// to the station already holding its part-filled lots, so stacks reach their floor.
	if cfg.Disposal == "refine" {
		stationID, sysID := cfg.RefineStationID, int64(0)
		if stationID == 0 {
			stationID, sysID = m.mineralAnchorStation(ctx, b, r)
			if stationID != 0 {
				m.logBot(b.CharID, logInfo, "mining",
					"hauling the load back to %s — its part-filled mineral lots are accumulating there",
					m.stationLabel(ctx, stationID))
			}
		}
		if stationID == 0 {
			var err error
			stationID, sysID, err = m.nearestAllowedStation(ctx, b, r, prefer)
			if err != nil {
				return err
			}
		}
		if stationID == 0 {
			return fmt.Errorf("no station it will use is in reach to refine at")
		}
		if sysID == 0 {
			loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
			if err != nil {
				return err
			}
			sysID = loc.SolarSystemID
		}
		return m.beginTravel(ctx, b, sysID, stationID, string(TaskMineRefine))
	}

	if cfg.Disposal == "stockpile" {
		stationID, sysID := cfg.StockpileStationID, int64(0)
		if stationID == 0 {
			var err error
			stationID, sysID, err = m.nearestAllowedStation(ctx, b, r, prefer)
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
		return m.beginTravel(ctx, b, sysID, stationID, "mining.stockpile")
	}

	// Sell: the best bid in the pilot's own economic scope with the haul priced in
	// (reach.go, residency.go); every grade in the load is offered to the scan.
	var sellTo *marketStop
	// Its own corporation's bids are valued up by its loyalty (loyalSell).
	corpBids := m.corpBidStations(ctx, b.CorpID, oreLotTypes(st.Data))
	look, err := m.shopLadder(ctx, b, oreLotTypes(st.Data), false, prefer, defaultBand(b),
		fmt.Sprintf("every buyer for %s it can reach belongs to a faction it dislikes — "+
			"selling anyway, but never to the despised", st.Data.OreName),
		func(s marketScan) bool {
			sellTo = loyalSell(stopsWithin(s.Stops, oreSellMaxJumps), b.Tune, corpBids)
			return sellTo != nil
		})
	if err != nil {
		return err
	}
	scan := look.Scan
	if sellTo == nil {
		// The ladder prices a lowsec hop at about eight jumps (hopCost), the belt search
		// that sent this miner at one (miningRules). Count the same gates before giving up.
		sellTo = m.sellWithinGates(ctx, b, look, scan, corpBids)
	}
	if sellTo == nil {
		// Nobody close enough is bidding: stockpile rather than haul across the map, and
		// say why on the board. The stray sweep lists the rock (orestrays.go).
		m.logBot(b.CharID, logInfo, "mining", "stockpiling %s instead: %s",
			st.Data.OreName, m.noSellerReasonWithin(ctx, scan, "bids on "+st.Data.OreName, oreSellMaxJumps))
		stationID, sysID, err := m.nearestAllowedStation(ctx, b, r, prefer)
		if err != nil {
			return err
		}
		// A dock whose region bids on this rock buys it on arrival (NPC ore bids are
		// region-wide), so the trip is a sale: the stockpile phase only stores.
		if m.dockBidsOnLots(ctx, b, stationID, oreLotTypes(st.Data)) {
			m.logBot(b.CharID, logInfo, "mining",
				"the dock it is heading for bids on %s — selling there rather than storing it", st.Data.OreName)
			return m.beginTravel(ctx, b, sysID, stationID, "mining.sell")
		}
		return m.beginTravel(ctx, b, sysID, stationID, "mining.stockpile")
	}
	// The factory feed (factorydemand.go): a listing trip carries the hold to a busy bot
	// factory whose builders would pay more than the best bid; miningSell lists it there.
	if cfg.ListOre && st.Data.OreTypeID != 0 {
		if fid, val := m.oreFactoryOutlet(ctx, b, st.Data.OreTypeID, sellTo.Price); fid != 0 {
			if loc, lerr := evedb.GetStationLocation(ctx, m.db, fid); lerr == nil && loc.SolarSystemID != 0 {
				m.logBot(b.CharID, logInfo, "mining",
					"carrying %s to the factory at %s — worth %.2f ISK/unit to its builders against a best bid of %.2f",
					st.Data.OreName, m.stationLabel(ctx, fid), val, sellTo.Price)
				return m.beginTravel(ctx, b, loc.SolarSystemID, fid, "mining.sell")
			}
		}
	}
	m.logBot(b.CharID, logInfo, "mining", "selling %s at %.2f ISK/unit, station %d in %s",
		st.Data.OreName, sellTo.Price, sellTo.StationID,
		m.travelNote(ctx, b, sellTo.SystemID, sellTo.Jumps))
	return m.beginTravel(ctx, b, sellTo.SystemID, sellTo.StationID, "mining.sell")
}

// oreSellMaxJumps bounds how far a miner hauls a hold to a bid. shopClimb runs to
// bandUniverse and its only distance term is a per-jump haircut that Greed can bend
// below 1%, so the sale needs a cap. Ten matches shopSoftMaxJumps.
const oreSellMaxJumps = 10

// stopsWithin keeps the stops no more than maxJumps away.
func stopsWithin(stops []marketStop, maxJumps int) []marketStop {
	out := stops[:0:0]
	for _, st := range stops {
		if st.Jumps <= maxJumps {
			out = append(out, st)
		}
	}
	return out
}

// minerMaySwapToHauler boards a trade ship with more cargo capacity at the current
// station for the haul leg, if the miner owns one. The mining fit's ID goes in
// MiningReturnFitID so the endpoint can re-board the mining ship. Best effort and
// quiet: a miner without a hauler hauls in its mining ship.
func (m *Mod) minerMaySwapToHauler(ctx context.Context, b *botRun) {
	tradeShip, _, err := m.shipForJob(ctx, b.CharID, JobTrade)
	if err != nil || tradeShip == nil || tradeShip.ShipItemID == 0 || !tradeShip.ShipExists {
		return
	}
	// Already in this ship: nothing to swap.
	if tradeShip.ShipItemID == b.ShipID {
		return
	}
	// The trade ship must be at this station.
	var shipLoc int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`,
		tradeShip.ShipItemID).Scan(&shipLoc); err != nil || shipLoc != b.StationID {
		return
	}
	// Remember which mining fit to return to, then board the hauler.
	// The mining fit is the one that owns the ship the bot is currently in.
	miningFitID := m.fitIDForShip(ctx, b.CharID, b.ShipID)
	if miningFitID == 0 {
		return
	}
	if err := m.boardShip(ctx, b, tradeShip.ShipItemID); err != nil {
		return
	}
	b.State.Data.MiningReturnFitID = miningFitID
	m.logBot(b.CharID, logInfo, "mining",
		"boarding its hauler for the ore run — will re-board its mining ship afterwards")
}

// minerReboardMiningShip re-boards the mining ship after a haul leg, if a
// swap happened. Called from every mining completion endpoint (sell, stockpile,
// refine). Best-effort: if the mining ship is gone, the next miningPlan will
// sort it out.
func (m *Mod) minerReboardMiningShip(ctx context.Context, b *botRun) {
	fitID := b.State.Data.MiningReturnFitID
	if fitID == 0 {
		return
	}
	b.State.Data.MiningReturnFitID = 0
	ships, err := shipsForChar(ctx, m.db, b.CharID)
	if err != nil {
		return
	}
	shipID, ok := ships[fitID]
	if !ok || shipID == 0 {
		return
	}
	if !entityExists(ctx, m.db, shipID, b.CharID) {
		return
	}
	// Only re-board if the mining ship is here.
	var shipLoc int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`,
		shipID).Scan(&shipLoc); err != nil || shipLoc != b.StationID {
		return
	}
	_ = m.boardShip(ctx, b, shipID)
}

// fitIDForShip returns the fitID that owns a ship entity, or 0.
func (m *Mod) fitIDForShip(ctx context.Context, charID, shipItemID int64) int64 {
	var fitID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT fitID FROM rolebot_fit_ships WHERE characterID = ? AND shipItemID = ?`,
		charID, shipItemID).Scan(&fitID); err != nil {
		return 0
	}
	return fitID
}

// beltWork names the activities that take a pilot out to a belt, where the
// spawn table decides whether it may be there at all. Trading and hauling are
// not on this list: nobody refuses a freighter for being unarmed.
func beltWork(activity string) bool {
	switch activity {
	case ActMining, ActIndMining, ActCombat, ActPiracy:
		return true
	}
	return false
}

// rearmToothless puts a gun back on the shopping list for a ship that has none and
// reports whether it took the pilot somewhere to get one. The gun is the pilot's own
// empire's civilian starter weapon: free, and enough to clear the belt refusal.
// handled=false when nothing can be done (no slot, no race); the caller carries on.
func (m *Mod) rearmToothless(ctx context.Context, b *botRun, a *fitShip, t *fitTemplate,
	activity string) (bool, error) {

	gunID, ok := m.civilianGunFor(ctx, b.CharID)
	if !ok {
		return false, nil
	}
	flag, ok, err := m.freeHighSlot(ctx, a.ShipItemID, t)
	if err != nil {
		return false, err
	}
	if !ok {
		// Nothing spare. Fall back to the slot the fit holds open for a gun: on a
		// two-hardpoint hull it is the only high left.
		if flag, ok, err = m.plannedGunSlot(ctx, a.ShipItemID, t); err != nil || !ok {
			return false, err
		}
	}
	want := []ShopItem{{
		TypeID: gunID,
		Name:   m.typeNameOr(ctx, gunID),
		Qty:    1,
		Dest:   "module",
		Flag:   flag,
	}}
	// The gun is issued here, to anybody, rich or destitute (grantStarterLines fits it).
	// See docs/comment-notes-b13.md#starter-gun-is-issued-not-bought.
	if left := m.grantStarterLines(ctx, b, a, want); len(left) == 0 {
		m.logBot(b.CharID, logInfo, activity,
			"was flying unarmed, which no belt in New Eden would have accepted — issued a %s",
			want[0].Name)
		return false, nil
	}
	// The grant did not take (in space, ship in another hangar, or slot filled). The outfit
	// trip docks the pilot where its ship is; no station sells the gun.
	m.logBot(b.CharID, logInfo, activity,
		"its %s ship has nothing to shoot with and the %s could not be issued where it stands — heading to its ship to fit one",
		activity, want[0].Name)
	return true, m.resumeOutfit(ctx, b, a, t, want, activity)
}

// civilianGunFor is the starter weapon of this pilot's empire.
func (m *Mod) civilianGunFor(ctx context.Context, charID int64) (int64, bool) {
	var raceID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(bl.raceID, 0) FROM character_ c
		 LEFT JOIN chrBloodlines bl ON bl.bloodlineID = c.bloodlineID
		 WHERE c.characterID = ?`, charID).Scan(&raceID); err != nil {
		return 0, false
	}
	switch raceID {
	case 1:
		return civGatlingRailgunTypeID, true // Caldari
	case 2:
		return civGatlingAutocannonTypeID, true // Minmatar
	case 4:
		return civGatlingPulseLaserTypeID, true // Amarr
	case 8:
		return civLightElectronBlasterID, true // Gallente
	}
	return 0, false
}

// freeHighSlot finds a high slot on the ship that neither the hull's fitted
// modules nor the fit template already claims. The template matters: a gun placed
// where the fit's next purchase goes would be unfitted by procurement again
// (stripOffFitModules).
func (m *Mod) freeHighSlot(ctx context.Context, shipID int64, t *fitTemplate) (int64, bool, error) {
	if shipID == 0 || t == nil || t.Fit == nil {
		return 0, false, nil
	}
	claimed := map[int64]bool{}
	for _, mod := range t.Fit.Modules {
		claimed[mod.Flag] = true
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT flag FROM entity WHERE locationID = ? AND flag BETWEEN ? AND ?`,
		shipID, flagHiFirst, flagHiFirst+7)
	if err != nil {
		return 0, false, fmt.Errorf("free high slot for ship %d: %w", shipID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var f int64
		if rows.Scan(&f) == nil {
			claimed[f] = true
		}
	}
	// The hull's own high-slot count bounds the search: a flag past it is a slot the
	// ship lacks. Read off the hull type, not the fit, which records only slots in use.
	var his int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT CAST(COALESCE(valueInt, COALESCE(valueFloat, 0)) AS SIGNED)
		   FROM dgmTypeAttributes WHERE typeID = ? AND attributeID = ?`,
		t.Fit.HullTypeID, attrHiSlots).Scan(&his); err != nil || his <= 0 {
		return 0, false, nil
	}
	for i := int64(0); i < his; i++ {
		if flag := flagHiFirst + i; !claimed[flag] {
			return flag, true, nil
		}
	}
	return 0, false, nil
}

// plannedGunSlot is the high slot this fit means to put a gun in and has not
// bought one for yet. freeHighSlot refuses it, so on a rookie hull (every high
// claimed by the template) an unarmed pilot would find no slot; it is filled
// early with the free civilian gun. Only genuine weapons count as a plan.
func (m *Mod) plannedGunSlot(ctx context.Context, shipID int64, t *fitTemplate) (int64, bool, error) {
	if shipID == 0 || t == nil || t.Fit == nil {
		return 0, false, nil
	}
	occupied := map[int64]bool{}
	rows, err := m.db.QueryContext(ctx,
		`SELECT flag FROM entity WHERE locationID = ? AND flag BETWEEN ? AND ?`,
		shipID, flagHiFirst, flagHiFirst+7)
	if err != nil {
		return 0, false, fmt.Errorf("planned gun slot for ship %d: %w", shipID, err)
	}
	for rows.Next() {
		var f int64
		if rows.Scan(&f) == nil {
			occupied[f] = true
		}
	}
	rows.Close()

	// Lowest empty planned-weapon high wins, so two passes pick the same slot.
	best, found := int64(0), false
	for _, mod := range t.Fit.Modules {
		if mod.Flag < flagHiFirst || mod.Flag > flagHiFirst+7 || occupied[mod.Flag] {
			continue
		}
		if found && mod.Flag >= best {
			continue
		}
		isGun, err := m.typeIsWeapon(ctx, mod.TypeID)
		if err != nil {
			return 0, false, err
		}
		if isGun {
			best, found = mod.Flag, true
		}
	}
	return best, found, nil
}

// typeIsWeapon reports whether a module type can shoot: the turret-or-launcher
// test shipHasNoTeeth applies, minus mining lasers, which wear effTurretFitted
// without being guns.
func (m *Mod) typeIsWeapon(ctx context.Context, typeID int64) (bool, error) {
	if typeID == 0 {
		return false, nil
	}
	var n int64
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM dgmTypeEffects te
		WHERE te.typeID = ? AND te.effectID IN (?, ?)
		  AND NOT EXISTS (
			SELECT 1 FROM dgmTypeEffects me
			WHERE me.typeID = te.typeID AND me.effectID = ?)`,
		typeID, effTurretFitted, effLauncherFitted, effMiningLaser).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("weapon test for type %d: %w", typeID, err)
	}
	return n > 0, nil
}

// A mining laser or mining drone is not teeth: both look armed to the database but
// deal no combat damage (groupMiningDrone is invGroups 101, "Mines."). See
// docs/comment-notes-b13.md#mining-laser-is-not-a-gun.
const groupMiningDrone = 101

func (m *Mod) shipHasNoTeeth(ctx context.Context, shipID int64) bool {
	if shipID == 0 {
		return true
	}
	n, err := m.shipTeeth(ctx, shipID, false)
	return err == nil && n == 0
}

// shipTeeth counts what the ship can shoot with: combat drones in the bay and fitted turrets
// and launchers that are not mining lasers. realOnly leaves the civilian starter weapons out,
// for the question of whether the ship stays armed once one is unfitted.
func (m *Mod) shipTeeth(ctx context.Context, shipID int64, realOnly bool) (int64, error) {
	notStarter, args := "", []any{shipID, groupMiningDrone, effTurretFitted, effLauncherFitted, effMiningLaser}
	if realOnly {
		ids := make([]int64, 0, len(civilianStarterWeapons))
		for id := range civilianStarterWeapons {
			ids = append(ids, id)
		}
		notStarter = ` AND e.typeID NOT IN (` + placeholders(len(ids)) + `)`
		args = append(args, int64Args(ids)...)
	}
	var n int64
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity e
		WHERE e.locationID = ?
		  AND ((e.flag = 87 AND NOT EXISTS (
			SELECT 1 FROM invTypes it
			WHERE it.typeID = e.typeID AND it.groupID = ?
		  )) OR (e.flag BETWEEN 11 AND 34 AND EXISTS (
			SELECT 1 FROM dgmTypeEffects te
			WHERE te.typeID = e.typeID AND te.effectID IN (?, ?)
		  ) AND NOT EXISTS (
			SELECT 1 FROM dgmTypeEffects te
			WHERE te.typeID = e.typeID AND te.effectID = ?)`+notStarter+`))`,
		args...).Scan(&n)
	return n, err
}

// mineralAnchorStation is where this pilot's part-filled mineral lots sit: the
// reachable station holding the most accumulated units, or (0, 0) when none. Reach
// uses the pilot's own travel rules, so an anchor behind a security line it no
// longer crosses does not drag it back; that stack's age-out flushes it.
func (m *Mod) mineralAnchorStation(ctx context.Context, b *botRun, r travelRules) (int64, int64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, SUM(units) FROM rolebot_mineral_hold
		WHERE characterID = ?
		GROUP BY stationID
		ORDER BY SUM(units) DESC
		LIMIT 8`, b.CharID)
	if err != nil {
		return 0, 0
	}
	type anchor struct{ stationID, units int64 }
	var anchors []anchor
	for rows.Next() {
		var a anchor
		if rows.Scan(&a.stationID, &a.units) == nil {
			anchors = append(anchors, a)
		}
	}
	rows.Close()
	if len(anchors) == 0 {
		return 0, 0
	}
	jumps, err := m.reachFrom(ctx, b.SystemID, r)
	if err != nil {
		return 0, 0
	}
	// Two passes: an anchor in the pilot's own region is where its supply is owed, so
	// it beats a fatter pile elsewhere; only if none is reachable does the fattest win.
	home := m.homeRegionFor(ctx, b)
	for pass := 0; pass < 2; pass++ {
		homeOnly := pass == 0 && home != 0
		for _, a := range anchors {
			loc, err := evedb.GetStationLocation(ctx, m.db, a.stationID)
			if err != nil {
				continue
			}
			if _, ok := jumps[loc.SolarSystemID]; !ok {
				continue
			}
			if homeOnly && loc.RegionID != home {
				continue
			}
			return a.stationID, loc.SolarSystemID
		}
	}
	return 0, 0
}

// miningSell moves the ore to the hangar and sells into the best local bids
// through the real matching engine, one sale per variant: a load of Veldspar is
// three invTypes rows (plain, Concentrated, Dense) with separate order books.
func (m *Mod) miningSell(ctx context.Context, b *botRun) (err error) {
	st := b.State
	if err := m.moveOreToHangar(ctx, b, st.Data); err != nil {
		return err
	}
	list := miningConfigDerived(b).ListOre
	var sold, gross float64
	var soldAny bool
	for _, typeID := range oreLotTypes(st.Data) {
		// List first when the trip decided to list; sell on anything that says no (no
		// order slot, too little value, or a bid paying more than a refiner, oreask.go).
		if list {
			if filled, took, lerr := m.listOreStack(ctx, b, typeID); lerr != nil {
				m.logBot(b.CharID, logError, "market", "listing ore: %v — selling it instead", lerr)
			} else if filled > 0 {
				soldAny = true
				sold += float64(filled)
				gross += took
				continue
			}
		}
		filled, took, err := m.sellOreStack(ctx, b, typeID)
		if err != nil {
			return err
		}
		if filled > 0 {
			soldAny = true
			sold += float64(filled)
			gross += took
		}
	}
	if soldAny {
		verb := "sold"
		if list {
			verb = "put"
		}
		m.logBot(b.CharID, logInfo, "market", "%s %.0f× %s for %.2f ISK at %s",
			verb, sold, st.Data.OreName, gross, m.stationLabel(ctx, b.StationID))
	}
	m.minerReboardMiningShip(ctx, b)
	st.Activity, st.Phase, st.Data = "", "idle", idleData(st.Data)
	// Hangar business before the next load (see stationDwell).
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return saveState(ctx, m.db, st)
}

// oreDumpFloor is the lowest bid a dump will fill into: half the best bid on the
// dock and never under a quarter of the catalogue price. A quarter sits well below
// any NPC ore bid at its deepest market-impact floor, so the ore sink still takes
// a broke miner's hold, and far above a 0.01 ISK junk bid. Pure.
func oreDumpFloor(bestBid, basePrice float64) float64 {
	floor := bestBid * 0.5
	if q := basePrice * 0.25; q > floor {
		floor = q
	}
	if floor < 0.01 {
		floor = 0.01
	}
	return math.Round(floor*100) / 100
}

// sellOreStack sells one ore type out of the hangar into the local bids. A type
// with nothing in the hangar is nothing to do: a mixed load's smallest grade can
// be a stack a partial hold never received.
func (m *Mod) sellOreStack(ctx context.Context, b *botRun, typeID int64) (int64, float64, error) {
	// Total units in hangar (may include earlier stockpiles here).
	var itemID, qty int64
	err := m.db.QueryRowContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
		ORDER BY quantity DESC LIMIT 1`,
		b.CharID, b.StationID, typeID).Scan(&itemID, &qty)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	// The bids an immediate sell here would match: every bid in the region whose
	// range reaches this dock (bidreach.go), since seeded NPC ore demand is region-wide.
	bids, _ := m.bidsReachingStation(ctx, b.StationID, typeID, b.CharID)
	// No bid reaching this dock means an immediate sell has nothing to match and the
	// market would refuse it every time; the ore stays here as stock.
	if len(bids) == 0 {
		return 0, 0, nil
	}
	bestBid := bids[0].Price
	// The price handed to PlaceSellOrder is the floor of the fill: it matches every
	// bid at or above it, best first (db/marketorders.go).
	price := oreDumpFloor(bestBid, m.stableFloorPrice(ctx, typeID)/askFloorFraction)
	if bestBid < price {
		return 0, 0, nil
	}
	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
		typeID, itemID, price, qty, -1 /*station*/, 1, 0 /*immediate*/, false)
	if err != nil {
		return 0, 0, fmt.Errorf("sell: %w", err)
	}
	if out.ErrorMsg != "" {
		m.logBot(b.CharID, logWarn, "market", "sell refused (%s) — stockpiling here", out.ErrorMsg)
		return 0, 0, nil
	}
	// Split the fill by who took the other side: an NPC bid takes units out of the
	// game, a player bid is a refiner (only MarketFill.IsNPCOrder shows which).
	var gross, npcISK, botISK float64
	var npcQty, botQty int64
	for _, f := range out.Fills {
		v := f.Price * float64(f.Quantity)
		gross += v
		if f.IsNPCOrder {
			npcQty += f.Quantity
			npcISK += v
		} else {
			botQty += f.Quantity
			botISK += v
		}
	}
	m.noteOreFlow(ctx, typeID, oreStageSoldNPC, npcQty, npcISK)
	m.noteOreFlow(ctx, typeID, oreStageSoldBot, botQty, botISK)
	name := m.typeNameOr(ctx, typeID)
	m.notePeerFills(ctx, tradeEntry{
		CharID: b.CharID, TypeID: typeID, TypeName: name,
		Station: b.StationID, System: b.SystemID,
	}, out.Fills, false, "ore")
	return out.FilledQty, gross, nil
}

// mineralSellDays is how long a refined-mineral listing rests: longer than a
// manufactured good's week, since every factory buys minerals and an unfilled
// listing is waiting for a customer.
const mineralSellDays = 30

// mineralAskMargin is what the pilot asks over the best local bid when there is
// one: listing at the bid would just sell to it, and a resting order should be
// the cheaper shelf a factory buys from instead of hauling.
const mineralAskMargin = 1.05

// The scarcity term in mineralAsk. Span is the most a shortage can add (half
// again); Ref is the shortfall at which the curve is halfway there, sized at a few
// projects' worth so one big ammunition run does not read as a famine.
const (
	mineralScarcitySpan = 0.5
	mineralScarcityRef  = 50000.0
)

// miningRefineAndList is the third disposal: reprocess the load at the pilot's
// own factory and list the minerals there, giving other bots' build books stock
// to shop for and traders stock to haul. Minerals the pilot's own running project
// still needs are kept back.
func (m *Mod) miningRefineAndList(ctx context.Context, b *botRun) (err error) {
	st := b.State
	defer func() {
		if err == nil {
			m.minerReboardMiningShip(ctx, b)
			st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
			st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
			err = saveState(ctx, m.db, st)
		}
	}()

	if err := m.moveOreToHangar(ctx, b, st.Data); err != nil {
		return err
	}

	unitsIn, got, err := m.refineHangarOre(ctx, b)
	if err != nil {
		return err
	}
	if unitsIn == 0 {
		m.logBot(b.CharID, logInfo, "mining",
			"nothing in the hangar refines into a whole portion yet — leaving it for the next load")
		return nil
	}

	holdSec := int64(mineralHoldMaxSec)
	listed, kept, held, err := m.listRefinedMinerals(ctx, b, got,
		m.projectReservations(ctx, b), holdSec)
	if err != nil {
		return err
	}
	m.logBot(b.CharID, logInfo, "mining",
		"refined %d unit(s) of ore at its factory and listed %d mineral line(s) for sale%s%s",
		unitsIn, listed,
		map[bool]string{true: " (holding back what its own job needs)"}[kept > 0],
		map[bool]string{true: fmt.Sprintf(", %d stack(s) accumulating toward a listable lot", held)}[held > 0])
	return nil
}

// mineralAskSteps maps MineralMarkup (1.0-1.9) to per-bot undercut and overcut
// steps by linear interpolation, so bots at one station reach different prices:
// generous 1.0 undercuts 5.0% and overcuts 3.0%; greedy 1.9 undercuts 1.5% and
// overcuts 8.0%.
func mineralAskSteps(mineralMarkup float64) askSteps {
	// Normalise markup into 0..1 where 0 = generous (1.0) and 1 = greedy (1.9).
	t := (mineralMarkup - 1.0) / 0.9
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return askSteps{
		Undercut: 0.05 - t*0.035, // 0.05 to 0.015
		Overcut:  0.03 + t*0.05,  // 0.03 to 0.08
	}
}

// mineralAsk prices a resting mineral order at a station: the pilot's valuation
// (going rate, roster scarcity, its own markup) goes to askAgainstBook
// (askbook.go), which prices it against the book on this dock. Only player bids
// count as the local bid. See docs/comment-notes-b13.md#mineral-ask-pricing.
func (m *Mod) mineralAsk(ctx context.Context, b *botRun, stationID, typeID, qty int64) (float64, string, error) {
	base := 0.0
	var bid sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MAX(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0
		  AND charID NOT IN (?, ?)`,
		stationID, typeID, evedb.SeederCharID, evedb.AdminSeederCharID).Scan(&bid)
	if bid.Valid && bid.Float64 > 0 {
		base = bid.Float64 * mineralAskMargin
	} else {
		est, err := m.estimatePriceFor(ctx, b, typeID)
		if err != nil {
			return 0, "", err
		}
		base = est
	}
	if base <= 0 {
		return 0, "", nil
	}
	// The valuation never follows the book below the stable floor: both sources above
	// are market-referential, so a collapsed book would pass straight through.
	if sf := m.stableFloorPrice(ctx, typeID); sf > 0 && base < sf {
		base = sf
	}
	// The valuation: what this pilot would ask with the counter to itself.
	markup := b.Tune.MineralMarkup
	fallback := base * mineralScarcityMul(m.mineralShortageUnits(ctx, typeID)) * markup
	if fallback < 0.01 {
		fallback = 0.01
	}

	// Per-bot step sizes from MineralMarkup: greedy bots undercut little and overcut
	// much, generous bots the reverse.
	steps := mineralAskSteps(b.Tune.MineralMarkup)

	// And the price, against the book that is actually on this dock.
	book, err := m.localAskBook(ctx, b.CharID, stationID, typeID)
	if err != nil {
		return 0, "", err
	}
	// The competitive floor is the pilot's own, not one shared number: a shared floor
	// ends a glutted book as a single tied price (personalAskFloorFrac).
	price, why := askAgainstBook(book, qty, fallback, m.personalFloorPrice(ctx, typeID, b.Tune), steps)
	if price < 0.01 {
		price = 0.01
	}
	// The ceiling, against the same authored anchor as the floor. The inputs above are
	// market-referential and compound without it; scarcity is already capped at +50%.
	if sf := m.stableFloorPrice(ctx, typeID); sf > 0 {
		if cap := (sf / askFloorFraction) * mineralAskMaxOverBase; price > cap {
			price = cap
			why = fmt.Sprintf("clamped at %.0f× base — the local book has drifted far past any real scarcity", mineralAskMaxOverBase)
		}
	}
	return math.Round(price*100) / 100, why, nil
}

// mineralAskMaxOverBase is the ceiling on a mineral ask in multiples of
// invTypes.basePrice (sql/ore_prices.sql). Scarcity tops out at 1.5x, envy under
// 1.4x and the sec-tier spread under 1.3x, so their product clears it.
const mineralAskMaxOverBase = 4.0

// mineralScarcityMul turns an outstanding roster-wide shortfall into a
// multiplier. Pure. It is gentle and capped: the units are a bill of materials, so
// one ammunition batch can write six figures of Tritanium onto the ledger, and a
// mineral must be short across many projects to price like it.
func mineralScarcityMul(shortUnits int64) float64 {
	if shortUnits <= 0 {
		return 1.0
	}
	mul := 1.0 + mineralScarcitySpan*(float64(shortUnits)/(float64(shortUnits)+mineralScarcityRef))
	if mul > 1.0+mineralScarcitySpan {
		return 1.0 + mineralScarcitySpan
	}
	return mul
}

// ── helpers ─────────────────────────────────────────────────────────────────

// miningYield computes the fitted hold-fill: units of ore, seconds to fill, and
// the m³/s rate. Each miner's miningAmount over its own cycle, times
// (1+0.05·Mining), (1+0.05·Astrogeology), mining upgrades and the server rate.
func (m *Mod) miningYield(ctx context.Context, b *botRun, oreTypeID int64) (units, fillSec int64, rate float64, err error) {
	if b.ShipID == 0 {
		return 0, 0, 0, fmt.Errorf("not in a ship")
	}
	mods, err := m.fittedMinerCycles(ctx, b.ShipID)
	if err != nil {
		return 0, 0, 0, err
	}
	// Each module over its own cycle, and only the ones this rock answers to
	// (minercap.go): the core's activation gate, which the abstract tier skips.
	laserRate := minerRatePerSec(mods, m.oreGroupOf(ctx, oreTypeID))
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return 0, 0, 0, err
	}
	if laserRate > 0 {
		laserRate *= 1 + 0.05*float64(skills[skillMining])
		laserRate *= 1 + 0.05*float64(skills[skillAstrogeology])
		// The mining hardwirings, the term the core's activation path folds.
		laserRate *= evedb.GetCharImplantMults(ctx, m.db, b.CharID).MiningYield
		if bonus, err := evedb.GetMiningYieldBonus(ctx, m.db, b.ShipID); err == nil && bonus > 0 {
			laserRate *= bonus
		}
		laserRate *= m.hullMiningMult(ctx, b.CharID, b.ShipID)
		laserRate *= rates.MiningYield
	}
	// Mining drones in the bay, as many as Drones lets it launch, on the core's basis
	// (drones.go: attribute 77 per attribute-73 cycle x rates.MiningYield, no skills).
	droneRate := m.miningDroneRate(ctx, b, skills, oreTypeID)
	if laserRate <= 0 && droneRate <= 0 {
		return 0, 0, 0, nil
	}

	var hullTypeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, b.ShipID).Scan(&hullTypeID); err != nil {
		return 0, 0, 0, err
	}
	capacity, err := evedb.GetShipCargoCapacityFitted(ctx, m.db, hullTypeID, b.ShipID)
	if err != nil {
		return 0, 0, 0, err
	}
	oreVol, err := evedb.GetTypeVolume(ctx, m.db, oreTypeID)
	if err != nil || oreVol <= 0 {
		oreVol = 0.1
	}
	rate = laserRate + droneRate
	units = int64(capacity / oreVol)
	fillSec = int64(capacity / rate)
	if fillSec < int64(miningCycleSec) {
		fillSec = int64(miningCycleSec)
	}
	return units, fillSec, rate, nil
}

// hullMiningMult is the hull's own mining-laser bonus at this pilot's skills (the
// Osprey's and Scythe's 20% per cruiser level, a barge's per Mining Barge level),
// read from the table the player's laser cycle reads (db.GetShipHullBonuses).
// 1 on any failure: a missing bonus must never stop a pilot mining.
func (m *Mod) hullMiningMult(ctx context.Context, charID, shipID int64) float64 {
	if shipID == 0 {
		return 1
	}
	var typeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, shipID).Scan(&typeID); err != nil {
		return 1
	}
	hb, err := evedb.GetShipHullBonuses(ctx, m.db, charID, typeID)
	if err != nil {
		return 1
	}
	if mult := hb.MiningMult(); mult > 0 {
		return mult
	}
	return 1
}

// miningDroneRate is the m³/s this pilot's mining drones add on this ore:
// zero on ice and Mercoxit, which the core's droneMineCycle refuses to chew.
func (m *Mod) miningDroneRate(ctx context.Context, b *botRun, skills map[int64]int, oreTypeID int64) float64 {
	bay, err := m.shipBayDrones(ctx, b.ShipID)
	if err != nil || len(bay) == 0 {
		return 0
	}
	perSec := launchedOutput(bay, grpMiningDrone, int64(skills[skillDrones]))
	if perSec <= 0 {
		return 0
	}
	if oreTypeID != 0 {
		if groupID := m.typeGroupOf(ctx, oreTypeID); groupID == destiny.GroupIce || groupID == destiny.GroupMercoxit {
			return 0
		}
	}
	return perSec * rates.MiningYield
}

// miningRealOre reconciles the trip's plan with the rocks actually in this system
// and reports the ore family to aim at. ok=false means nothing here is workable,
// or the field cannot be read; the caller re-plans. Never fall back to the planned
// ore on an unreadable field. See docs/comment-notes-b13.md#real-ore-selection.
func (m *Mod) miningRealOre(ctx context.Context, b *botRun, cfg miningConfig) (int64, string, bool, error) {
	planned := destiny.BaseOreType(b.State.Data.OreTypeID)
	f, err := m.beltFieldFor(ctx, b.SystemID)
	if err != nil {
		return 0, "", false, err
	}
	if f == nil {
		return 0, "", false, nil
	}
	// The next search through this system should not plan an ore its field
	// does not hold (orememory.go).
	learnOreAbsence(b.SystemID, f.familyUnits(0))
	priority := cfg.OrePriority
	canMerc := m.canMineMercoxit(ctx, b)
	if cfg.ByValue {
		// The planned rock was the dearest in the system; when it is gone, the
		// next dearest, not the next name on an old list.
		if byVal := oreNamesByValue(m.oreDigValues(ctx, b, canMerc)); len(byVal) > 0 {
			priority = byVal
		}
	}
	familyID, name, ok := chooseMiningOre(planned, f.familyUnits(0), priority, canMerc)
	if ok && familyID == planned && name == "" {
		name = b.State.Data.OreName
	}
	return familyID, name, ok, nil
}

// chooseMiningOre is miningRealOre's decision, apart from the database. The
// planned ore wins whenever it is there; priority is only the fallback order.
// ok=false means nothing here is workable. Without canMerc (canMineMercoxit)
// Mercoxit is never chosen, since a laser pointed at it produces nothing.
func chooseMiningOre(planned int64, avail map[int64]int64, priority []string, canMerc bool) (int64, string, bool) {
	if len(avail) == 0 {
		return 0, "", false
	}
	workable := func(name string) bool { return name != oreMercoxit || canMerc }
	if planned != 0 && avail[planned] > 0 && workable(oreFamilyBases[planned]) {
		return planned, oreFamilyBases[planned], true
	}

	rank := map[string]int{}
	for i, name := range priority {
		if _, dup := rank[name]; !dup {
			rank[name] = i
		}
	}
	offList := len(priority)

	bestID, bestName, bestRank, bestUnits := int64(0), "", offList+1, int64(0)
	for familyID, units := range avail {
		name := oreFamilyBases[familyID]
		if name == "" || !workable(name) {
			continue
		}
		rk, listed := rank[name]
		if !listed {
			// Off the priority list is a last resort, not a refusal: the pilot would rather
			// mine something it did not ask for than fly home empty.
			rk = offList
		}
		// Ties on rank go to the fatter deposit; ties on both go to the lower typeID, so
		// two pilots asking at once do not split over map iteration order.
		if rk < bestRank ||
			(rk == bestRank && units > bestUnits) ||
			(rk == bestRank && units == bestUnits && bestID != 0 && familyID < bestID) {
			bestID, bestName, bestRank, bestUnits = familyID, name, rk, units
		}
	}
	if bestID == 0 {
		return 0, "", false
	}
	return bestID, bestName, true
}

// oreLotTypes is the ore types a load is made of: the lots when the field was
// read, the planned family when it could not be.
func oreLotTypes(d StateData) []int64 {
	if len(d.OreLots) == 0 {
		if d.OreTypeID == 0 {
			return nil
		}
		return []int64{d.OreTypeID}
	}
	out := make([]int64, 0, len(d.OreLots))
	for _, lot := range d.OreLots {
		out = append(out, lot.TypeID)
	}
	return out
}

// oreLotUnits is the same load as a typeID to units map, which the flow counter
// takes. The fallback arm carries OreUnits against the planned family, so a load
// whose field could not be read still counts in the Ore page's denominator.
func oreLotUnits(d StateData) map[int64]int64 {
	if len(d.OreLots) == 0 {
		if d.OreTypeID == 0 || d.OreUnits <= 0 {
			return nil
		}
		return map[int64]int64{d.OreTypeID: d.OreUnits}
	}
	out := make(map[int64]int64, len(d.OreLots))
	for _, lot := range d.OreLots {
		if lot.Units > 0 {
			out[lot.TypeID] += lot.Units
		}
	}
	return out
}

// cargoOreUnits totals the load actually sitting in the hold, across every
// variant it is made of.
func (m *Mod) cargoOreUnits(ctx context.Context, b *botRun, lots []oreLot) (int64, error) {
	var total int64
	for _, lot := range lots {
		n, err := m.cargoUnitsOf(ctx, b, lot.TypeID)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// moveOreToHangar unloads every variant of the load into the station hangar; the
// single-type moveCargoToHangar would leave the other grades in the ship.
func (m *Mod) moveOreToHangar(ctx context.Context, b *botRun, d StateData) error {
	for _, typeID := range oreLotTypes(d) {
		if err := m.moveCargoToHangar(ctx, b, typeID); err != nil {
			return err
		}
	}
	return nil
}

// oreLotNote is the breakdown a mixed load gets in the log, and nothing for a
// single-grade one, whose family name already says it all.
func (m *Mod) oreLotNote(ctx context.Context, lots []oreLot) string {
	if len(lots) < 2 {
		return ""
	}
	parts := make([]string, 0, len(lots))
	for _, lot := range lots {
		parts = append(parts, fmt.Sprintf("%d× %s", lot.Units, m.typeNameOr(ctx, lot.TypeID)))
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

func (m *Mod) oreType(ctx context.Context, name string) (typeID int64, volume float64, err error) {
	err = m.db.QueryRowContext(ctx, `
		SELECT t.typeID, COALESCE(t.volume, 0.1)
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeName = ? AND g.categoryID = 25 AND t.published = 1
		LIMIT 1`, name).Scan(&typeID, &volume)
	if err != nil {
		return 0, 0, fmt.Errorf("ore %q: %w", name, err)
	}
	return
}

// addCargoStack merges qty of typeID into the ship's cargo.
func (m *Mod) addCargoStack(ctx context.Context, b *botRun, typeID, qty int64) error {
	if b.ShipID == 0 {
		return fmt.Errorf("not in a ship")
	}
	return m.addCargoStackFor(ctx, b.CharID, b.ShipID, typeID, qty)
}

// addCargoStackFor is addCargoStack for callers outside the sim's working
// set (the embodied miner, which only knows character and ship IDs).
func (m *Mod) addCargoStackFor(ctx context.Context, charID, shipID, typeID, qty int64) error {
	itemID, have, err := evedb.FindCargoItemByType(ctx, m.db, typeID, charID, shipID)
	if err == nil && itemID != 0 {
		return evedb.UpdateCargoItemQty(ctx, m.db, itemID, have+qty)
	}
	_, err = evedb.CreateCargoItem(ctx, m.db, typeID, charID, shipID, qty)
	return err
}

// moveCargoToHangar moves every cargo stack of typeID into the hangar at the
// bot's current station, folded into any of that type already there
// (stacking.go); without the merge each trip's ore lands as a separate row.
func (m *Mod) moveCargoToHangar(ctx context.Context, b *botRun, typeID int64) error {
	if b.StationID == 0 {
		return fmt.Errorf("not docked")
	}
	if b.ShipID == 0 {
		return nil
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 4
		WHERE locationID = ? AND ownerID = ? AND typeID = ? AND flag = 5`,
		b.StationID, b.ShipID, b.CharID, typeID); err != nil {
		return err
	}
	m.stackHangar(ctx, b.CharID, b.StationID)
	return nil
}

// moveCargoOreToHangar moves every raw ore stack in the hold into the hangar at the
// bot's current station, whatever its type. Rock bought to stand in for a mineral line
// (oresub.go) is no line of the bill, so the per-type unload never names it.
func (m *Mod) moveCargoOreToHangar(ctx context.Context, b *botRun) error {
	if b.StationID == 0 {
		return fmt.Errorf("not docked")
	}
	if b.ShipID == 0 {
		return nil
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		SET e.locationID = ?, e.flag = 4
		WHERE e.locationID = ? AND e.ownerID = ? AND e.flag = 5 AND g.categoryID = 25`,
		b.StationID, b.ShipID, b.CharID); err != nil {
		return err
	}
	m.stackHangar(ctx, b.CharID, b.StationID)
	return nil
}

// Station finding lives in reach.go: nearestAllowedStation weighs every station
// in the database.
