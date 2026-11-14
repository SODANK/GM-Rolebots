package rolebots

import (
	"context"
	"fmt"
	"sort"
)

// TaskKind is what a bot is doing. Every fact about a task lives in one TaskDef registry entry, and
// Init refuses to boot if a field is missing. Values are the historical phase strings, so
// rolebot_state rows keep working; combat_abstract.go also matches one in raw SQL (phase = 'gang.patrol').
type TaskKind string

const (
	TaskIdle TaskKind = "idle"

	TaskTravel     TaskKind = "travel"
	TaskTravelDock TaskKind = "travel.dock"

	// One board task serves every activity, so no phase is built by string concatenation.
	TaskBoard TaskKind = "board"

	// The home loop's shopping pass (homeshop.go): docked at home between
	// activities, topping up stock before the next draw.
	TaskShopping TaskKind = "home.shop"

	// The home loop's disposal trip (metasell.go): a hangar of named and faction gear sells well, but
	// almost never where the pilot lives.
	TaskHomeSell TaskKind = "home.sell"
	// TaskWarehouse is the trip to a station holding goods this pilot left
	// behind. Not a shopping visit and not a sale run: it goes to where the
	// hangar sweeps can see the goods at all (warehouse.go).
	TaskWarehouse TaskKind = "home.warehouse"

	TaskProcureDropShip TaskKind = "procure.dropship"
	TaskProcureFindHull TaskKind = "procure.findhull"
	TaskProcureBuyHull  TaskKind = "procure.buyhull"
	// Arrived at a station holding a listed ship it chose for the hull
	// (escrow_ship.go): claim it, or fall back to the market.
	TaskProcureEscrowShip TaskKind = "procure.escrowship"
	TaskProcureBoard      TaskKind = "procure.board"
	TaskProcureOutfit     TaskKind = "procure.outfit"
	TaskProcureFindNext   TaskKind = "procure.findnext"

	TaskMineWork      TaskKind = "mining.mine"
	TaskMineSell      TaskKind = "mining.sell"
	TaskMineStockpile TaskKind = "mining.stockpile"
	// Refine the load at the pilot's own factory and list the minerals there (mining.go,
	// miningRefineAndList): the third disposal, besides selling the rock and stockpiling it.
	TaskMineRefine TaskKind = "mining.refine"

	TaskTradeBuy  TaskKind = "trade.buy"
	TaskTradeLoad TaskKind = "trade.load"
	TaskTradeSell TaskKind = "trade.sell"
	// Collecting goods the pilot already owns out of a hangar it is not standing in: a standing bid that
	// filled while it was elsewhere, or a listing it gave up on (tradeorders.go).
	TaskTradePickup TaskKind = "trade.pickup"

	TaskCombatSweep TaskKind = "combat.sweep"
	// What the sweep is FOR (beltloot.go): emptying the cans the rats
	// leave, and taking the haul home to sell.
	TaskCombatLoot TaskKind = "combat.loot"
	TaskCombatSell TaskKind = "combat.sell"
	// Carrying the haul to the corporation's office instead (lootdonate.go).
	TaskCombatDonate TaskKind = "combat.donate"

	// Piracy (piracy.go). Prowl and camp are the two ways to find somebody; loot and fence are what
	// happens after; lie-low waits out the criminal timer.
	TaskPiracyProwl  TaskKind = "piracy.prowl"
	TaskPiracyCamp   TaskKind = "piracy.camp"
	TaskPiracyLoot   TaskKind = "piracy.loot"
	TaskPiracyFence  TaskKind = "piracy.fence"
	TaskPiracyLieLow TaskKind = "piracy.lielow"

	// The combat lane's own tactics (combattactic.go). The belt sweep and the relief run stay
	// TaskCombatSweep (same posture, same step); a task of its own is for a different place to be or a
	// different thing to do on arrival.
	TaskCombatRoam      TaskKind = "combat.roam"
	TaskCombatGate      TaskKind = "combat.gate"
	TaskCombatIntercept TaskKind = "combat.intercept"

	// Industry (industry.go). Every phase runs docked; the mine-for-
	// materials loop inside a project runs the ordinary mining tasks.
	TaskIndBuyBPO  TaskKind = "industry.buybpo"
	TaskIndRefine  TaskKind = "industry.refine"
	TaskIndBuyMats TaskKind = "industry.buymats"
	TaskIndInstall TaskKind = "industry.install"
	TaskIndCollect TaskKind = "industry.collect"
	TaskIndSell    TaskKind = "industry.sell"
	// TaskIndCorpJob installs a corporation job on the corporation's print
	// and materials at its factory office (corpjobs.go).
	TaskIndCorpJob TaskKind = "industry.corpjob"
	// TaskIndCorpMove carries the desk's stock from an old factory office to
	// the new one (corpfactorymove.go).
	TaskIndCorpMove TaskKind = "industry.corpmove"
	// TaskIndPrintMove carries one blueprint from one of the corporation's
	// bases to another (corpprintmove.go).
	TaskIndPrintMove TaskKind = "industry.printmove"

	// R&D patent lottery (research.go). Collecting a won Tech II blueprint
	// from the agent that granted it, and hauling it back to the factory.
	// Both run docked; the flying between them is the ordinary travel task.
	TaskRndCollect TaskKind = "rnd.collect"
	TaskRndHaul    TaskKind = "rnd.haul"

	// An envoy's errand (corpenvoy.go): a letter carried to another
	// corporation's headquarters, then the road home. Both docked; the
	// flying between is the ordinary travel task.
	TaskEnvoyDeliver TaskKind = "envoy.deliver"
	TaskEnvoyHome    TaskKind = "envoy.home"

	// The deep-metal carry (corpdeepcarry.go): a member flying its Zydrine
	// or Megacyte to its corporation's resting bid. Docked step on arrival;
	// the flying is the ordinary travel task.
	TaskDeepCarry TaskKind = "deep.carry"

	// Courier contract acceptance and delivery (courier_accept.go).
	// All three are non-interruptible: collateral is at risk.
	TaskCourierPickup  TaskKind = "courier.pickup"  // docked at pickup, accept contract
	TaskCourierHaul    TaskKind = "courier.haul"    // dwell at pickup before departure
	TaskCourierDeliver TaskKind = "courier.deliver" // docked at drop, deliver crate

	// Blueprint research (labwork.go): a print goes to a station with a lab line, cooks for hours and
	// comes home. Distinct from the industry phases (the pilot is free in between; a lab job is a second
	// slot track) and from the Rnd pair (which fetches a print the pilot does not own).
	TaskLabFetch   TaskKind = "lab.fetch"
	TaskLabInstall TaskKind = "lab.install"
	TaskLabCollect TaskKind = "lab.collect"
	TaskLabHaul    TaskKind = "lab.haul"
	// A researcher flying to a blueprint counter to buy a print to research
	// and sell (labbuy.go researcherShopTrip).
	TaskLabBuy TaskKind = "lab.buy"

	// Escrow listing (escrow_sell.go). Docked, posting items on escrow.
	TaskEscrowList TaskKind = "escrow.list"
	// Escrow buying (escrow_buy.go). Docked, claiming an escrow listing.
	TaskEscrowBuy TaskKind = "escrow.buy"

	// Buying a ride home (shuttle.go). A pilot in a capsule flies to the
	// nearest station that stocks a shuttle and buys one; the home loop
	// takes it from there.
	TaskRideBuy TaskKind = "ride.buy"

	TaskGangMine     TaskKind = "gang.mine"
	TaskGangHaulLoad TaskKind = "gang.haul.load"
	TaskGangHaulDrop TaskKind = "gang.haul.drop"
	TaskGangPatrol   TaskKind = "gang.patrol"
	TaskGangHunt     TaskKind = "gang.hunt"
	// ActSecurity's own guns walking their own beat. Its loop is the hunt's (gangHuntStep serves both)
	// but the panel must say which order it is. TaskGangPatrol is different: the mining op's escort,
	// pinned to the belt its miners picked.
	TaskGangSecurity TaskKind = "gang.security"
	TaskGangWait     TaskKind = "gang.wait"
	// A fleet member emptying a wreck it made into its OWN hold (gangloot.go).
	// Not "gang.loot": migrateLooterSeat resets that retired phase at boot.
	TaskGangSalvage TaskKind = "gang.salvage"
	// A member whose fleet ended while ore was still in its hold brings it
	// in to the nearest dock it will use, as its own rock (gangStep's
	// op-over branch). The gang is already gone by the time this runs.
	TaskGangOreDrop TaskKind = "gang.oredrop"
	// The pirate pack (gangplan.go): the whole gang prowls one system's
	// belts or sits one gate together. Postures mirror the solo pair.
	TaskGangPirateProwl TaskKind = "gang.pirate.prowl"
	TaskGangPirateCamp  TaskKind = "gang.pirate.camp"
	// The gank fleet (gank_op.go): strikers hold on the lawless far gate (stage), the scout sits the
	// policed gate scanning (scout), a called strike jumps in and fires (strike), and the scout strips
	// the wrecks afterwards (loot).
	TaskGangGankStage  TaskKind = "gang.gank.stage"
	TaskGangGankScout  TaskKind = "gang.gank.scout"
	TaskGangGankStrike TaskKind = "gang.gank.strike"
	TaskGangGankLoot   TaskKind = "gang.gank.loot"
	// The checkpoint (camp.go): a Security or Mercenary fleet sitting a gate
	// in its employer's own region under its rules of engagement, waving
	// through what it will not shoot; a Mercenary one tolls the neutrals.
	TaskGangCheckpoint TaskKind = "gang.checkpoint"
	// The fleet lifecycle (gangform.go): mustering at the staging station
	// before the op, and settling the books there after it.
	TaskGangMuster TaskKind = "gang.muster"
	// TaskGangFormUp is the second half of the muster and the visible one: a member that has arrived
	// and shipped up leaves the hangar and holds formation off the staging station until the commander
	// releases the fleet.
	TaskGangFormUp TaskKind = "gang.formup"
	TaskGangSettle TaskKind = "gang.settle"

	// Serving in a player's fleet (conscript.go): form is getting to them, escort is being with them,
	// fight is doing what they pointed at, hold is staying put, deliver is putting fleet property where
	// they asked. None is Interruptible: a pilot pulled off any of them leaves a human waiting.
	TaskConscriptForm    TaskKind = "conscript.form"
	TaskConscriptEscort  TaskKind = "conscript.escort"
	TaskConscriptFight   TaskKind = "conscript.fight"
	TaskConscriptHold    TaskKind = "conscript.hold"
	TaskConscriptDeliver TaskKind = "conscript.deliver"
)

// isConscriptPhase reports whether a stored phase is one of the five above. It matches the prefix
// rather than a set, because the boot sweep selects on `phase LIKE 'conscript.%'` and two
// definitions of the family would drift.
func isConscriptPhase(phase string) bool {
	return len(phase) > 10 && phase[:10] == "conscript."
}

// Posture is what the embodied tier needs to know about a task: whether to run lasers, hunt, or hold
// station. One type replaces per-file sets of phase literals, where a mismatch silently left a
// projection that never mined or shot.
type Posture uint8

const (
	// PostureIdle: nothing in particular; the projection wanders.
	PostureIdle Posture = iota
	// PostureTransit: the sim is flying this bot somewhere and owns its position. No wandering.
	PostureTransit
	// PostureHold: parked (waiting for a gang, sitting on the op belt while the hauler works). No
	// wandering.
	PostureHold
	// PostureWorkBelt: parks at belts and runs mining lasers.
	PostureWorkBelt
	// PostureCombatBelt: parks at belts with guns out; every NPC on grid is a target, not just whatever
	// shot first.
	PostureCombatBelt
	// PostureDocked: inside a station, doing business.
	PostureDocked
	// PostureAmbushBelt: belt to belt like a hunter, but hunting pilots. Every NPC on grid is still a
	// target (a pirate does not ignore rats while it waits); this adds prey.
	PostureAmbushBelt
	// PostureAmbushGate: sat on a stargate waiting for somebody to jump in. The sim owns the position,
	// so the projection must not wander off the gate.
	PostureAmbushGate
	// PostureCombatPatrol: a combat patrol with its rules of engagement live (roe.go). PostureCombatBelt
	// plus pilots as candidates: rivals outside policed space, strangers if nerve and doctrine allow. Not
	// PostureAmbushBelt, which means piracy (robbery, ransom, criminal timer).
	PostureCombatPatrol
	// PostureCombatGate: sat on a stargate with the same rules of engagement, a checkpoint when a
	// security corporation runs it and a blockade when a mercenary does. The sim owns the position, as
	// for PostureAmbushGate.
	PostureCombatGate
	// PostureLieLow: flagged and hiding. Guns cold, no prey, no wandering: the pilot waits out a criminal
	// timer somewhere the police are not.
	PostureLieLow
	// PostureEscort: flying at a player's side under their orders. The escort tick owns the position
	// (conscript_embodied.go), so this reads as holding. Not patrol and not prey: a servant shoots only
	// what it is told to and what is shooting its fleet.
	PostureEscort
)

// mining reports whether a projection in this posture parks on belts. Hunters do too: the
// belt-to-belt wander is shared, and only the lasers or the guns differ.
func (p Posture) mining() bool {
	return p == PostureWorkBelt || p == PostureCombatBelt ||
		p == PostureAmbushBelt || p == PostureCombatPatrol
}

// patrol reports whether every NPC on grid is a target rather than only
// whatever shot first. Pirates included: a pirate sitting on a belt with a
// spawn on it shoots the spawn, the same as anybody else with guns out.
func (p Posture) patrol() bool {
	return p == PostureCombatBelt || p == PostureAmbushBelt ||
		p == PostureAmbushGate || p == PostureCombatPatrol || p == PostureCombatGate
}

// prey reports whether this posture robs pilots: the piracy machinery in full (pick the fattest hold
// in reach, point it, put a price in Local, take the cargo, carry the criminal timer; piracy_prey.go).
// It is piracy-only. engages() is the other half: starting fights with people and committing robbery
// are separate flags.
func (p Posture) prey() bool {
	return p == PostureAmbushBelt || p == PostureAmbushGate
}

// engages reports whether this posture shoots pilots under rules of engagement (roe.go): rivals
// outside policed space, strangers if nerve and doctrine agree, without robbery or crime. It is the
// lawful twin of prey(). A pirate posture does not set it: two target pickers over one grid would
// let engagement compete with the robbery, which has to win.
func (p Posture) engages() bool {
	return p == PostureCombatPatrol || p == PostureCombatGate
}

// parksInSpace reports whether this posture leaves a ship sitting still in space, which is what
// makes a bot ambushable in unwatched space: a gate camp is as reachable by a roaming spawn as a belt
// (the spawn draw is per region, see drawSpawn). PostureLieLow is excluded: its pilot waits where
// nothing happens. Only the registry invariant in harness_test.go calls it.
func (p Posture) parksInSpace() bool {
	return p.mining() || p == PostureAmbushGate || p == PostureCombatGate
}

// holding reports whether the sim owns this bot's position, so the
// embodied tier must not wander it off.
func (p Posture) holding() bool {
	return p == PostureTransit || p == PostureHold ||
		p == PostureAmbushGate || p == PostureCombatGate ||
		p == PostureLieLow || p == PostureEscort
}

// TaskDef is everything the rest of the module needs to know about one
// kind of task. Every field is required; validateTaskRegistry enforces it
// at boot so a half-registered task cannot reach production.
type TaskDef struct {
	Kind TaskKind
	// Label is the panel's prose for this task.
	Label string
	// Step advances the task by one sim pass. Reports whether the bot
	// changed location.
	Step func(m *Mod, ctx context.Context, b *botRun) (bool, error)
	// Detail is the panel's one-line description of what this task is
	// Detail is the panel's one-line description of what this task is doing right now (the ore in the
	// hold, the shopping list outstanding, the destination). Optional; nil means no detail line.
	Detail func(m *Mod, ctx context.Context, st *SimState) string
	// Posture drives the embodied projection.
	Posture Posture
	// Exposed says whether a bot doing this in unwatched space can be jumped by NPCs.
	Exposed bool
	// Loots says whether a bot doing this stops to open what it kills (beltloot.go). It differs from
	// Exposed: a miner that beats off a spawn does not loot, since its hold is full of rock. Only tasks
	// that look for kills and can dispose of the proceeds set it.
	Loots bool
	// Interruptible says whether a muster call (gangCallUp, gangplan.go) may pull a pilot off this task.
	// It is true only when dropping the task strands nothing (idle, a belt, travel toward either): unfitted
	// modules, unsold cargo, a blueprint on a line, a half-emptied can or a criminal timer keep the pilot.
	Interruptible bool
}

var taskRegistry = map[TaskKind]TaskDef{}

// industryTasks is the set of phases the industry activity owns, filled by the loop that registers
// them. A bot with a job in the oven may be in any other phase, so the panel tests membership
// instead of a phase prefix.
var industryTasks = map[TaskKind]bool{}

// isIndustryPhase reports whether a stored phase string is an industry one.
func isIndustryPhase(phase string) bool { return industryTasks[TaskKind(phase)] }

func registerTask(d TaskDef) {
	if _, dup := taskRegistry[d.Kind]; dup {
		// Registration happens in init(); panicking here is a programming
		// error caught the first time the binary runs, not a runtime risk.
		panic("rolebots: duplicate task registration: " + string(d.Kind))
	}
	taskRegistry[d.Kind] = d
}

// taskDef looks up a task by its stored phase string.
func taskDef(phase string) (TaskDef, bool) {
	if phase == "" {
		phase = string(TaskIdle)
	}
	d, ok := taskRegistry[TaskKind(phase)]
	return d, ok
}

// postureFor is the embodied tier's question: what should a projection of a bot in this phase be
// doing? An unknown phase reads as idle, the safe answer: it wanders instead of mining or shooting.
func postureFor(phase string) Posture {
	if d, ok := taskDef(phase); ok {
		return d.Posture
	}
	return PostureIdle
}

// exposedPhase reports whether a bot in this phase is exposed to NPC
// spawns in unwatched space.
func exposedPhase(phase string) bool {
	d, ok := taskDef(phase)
	return ok && d.Exposed
}

// lootsPhase reports whether a bot in this phase empties the wrecks it
// makes (beltloot.go).
func lootsPhase(phase string) bool {
	d, ok := taskDef(phase)
	return ok && d.Loots
}

// interruptibleState reports whether this pilot may be pulled off what it is doing for a fleet
// muster (TaskDef.Interruptible). Travel inherits from NextPhase: a journey is as droppable as the
// errand at its far end, so flying to buy modules and flying to mine do not read the same.
func interruptibleState(st *SimState) bool {
	if st == nil {
		return false
	}
	d, ok := taskDef(st.Phase)
	if !ok {
		// A phase nothing claims re-plans on the next pass anyway (advance).
		return true
	}
	if !d.Interruptible {
		return false
	}
	switch TaskKind(st.Phase) {
	case TaskTravel, TaskTravelDock:
		nd, ok := taskDef(st.Data.NextPhase)
		return ok && nd.Interruptible
	}
	return true
}

// validateTaskRegistry refuses to boot on a half-registered task, turning a renamed or missing phase
// into a startup failure naming the task. The harness runs the same check, so it fails at -Test
// rather than at restart.
func validateTaskRegistry() error {
	if len(taskRegistry) == 0 {
		return fmt.Errorf("task registry is empty — no init() registered anything")
	}
	var problems []string
	for kind, def := range taskRegistry {
		if def.Kind != kind {
			problems = append(problems, fmt.Sprintf("%q is registered under key %q", def.Kind, kind))
		}
		if def.Label == "" {
			problems = append(problems, fmt.Sprintf("%q has no Label — the panel would show the raw slug", kind))
		}
		if def.Step == nil {
			problems = append(problems, fmt.Sprintf("%q has no Step — a bot entering it would stall", kind))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("task registry is incomplete: %v", problems)
	}
	return nil
}

// ── registrations ───────────────────────────────────────────────────────────
//
// Grouped by the file that owns the behaviour. This is the dispatch table advance() reads.

func init() {
	registerTask(TaskDef{
		Kind: TaskIdle, Label: "Idle — waiting to plan",
		Posture: PostureIdle, Detail: detailIdle, Interruptible: true,
		Step: func(m *Mod, ctx context.Context, b *botRun) (bool, error) {
			return false, m.plan(ctx, b)
		},
	})

	// Travel inherits its interruptibility from where it is going (interruptibleState); the flag here
	// only says a journey is not in itself something a pilot must see through.
	registerTask(TaskDef{
		Kind: TaskTravel, Label: "Traveling",
		Posture: PostureTransit, Detail: detailTravel, Interruptible: true,
		Step: (*Mod).travelStep,
	})
	registerTask(TaskDef{
		Kind: TaskTravelDock, Label: "Approaching the station",
		Posture: PostureTransit, Detail: detailTravel, Interruptible: true,
		Step: (*Mod).travelDockStep,
	})

	registerTask(TaskDef{
		Kind: TaskBoard, Label: "Boarding its ship",
		Posture: PostureDocked,
		Step:    (*Mod).boardStep,
	})

	registerTask(TaskDef{
		Kind: TaskShopping, Label: "Shopping at its home station",
		Posture: PostureDocked,
		Step:    (*Mod).shoppingStep,
	})
	// Not Interruptible, like TaskCombatSell: the gear is out of the hangar and in the hold, bought and
	// unsold, so a muster pulling the pilot off would strand it.
	registerTask(TaskDef{
		Kind: TaskHomeSell, Label: "Taking meta gear to a trade hub",
		Posture: PostureDocked, Detail: detailHomeSell,
		Step: (*Mod).homeSellStep,
	})
	// Interruptible, unlike the meta run: this trip carries nothing, and the goods sit in a hangar until
	// after the fleet.
	registerTask(TaskDef{
		Kind: TaskWarehouse, Label: "Clearing a hangar it left behind",
		Posture: PostureDocked, Detail: detailWarehouse, Interruptible: true,
		Step: (*Mod).warehouseStep,
	})

	// Procurement: a shopping trip that has taken the bot over.
	for _, p := range []struct {
		kind  TaskKind
		label string
		post  Posture
	}{
		{TaskProcureDropShip, "Dropping its old ship off at home", PostureTransit},
		{TaskProcureFindHull, "Looking for a station selling the hull", PostureDocked},
		{TaskProcureBuyHull, "Buying and assembling the hull", PostureDocked},
		{TaskProcureEscrowShip, "Claiming a listed ship off escrow", PostureDocked},
		{TaskProcureBoard, "Boarding its half-built ship", PostureDocked},
		{TaskProcureOutfit, "Buying and fitting modules", PostureDocked},
		{TaskProcureFindNext, "Hunting for the modules still missing", PostureDocked},
	} {
		registerTask(TaskDef{
			Kind: p.kind, Label: p.label, Posture: p.post,
			Detail: detailProcure,
			Step:   (*Mod).procureStep,
		})
	}

	// Mining: only the working phase is exposed to spawns and interruptible. A pilot pulled out of a
	// refine or a sale leaves the load behind in a station it is leaving.
	registerTask(TaskDef{
		Kind: TaskMineWork, Label: "Mining",
		Posture: PostureWorkBelt, Exposed: true, Detail: detailMining,
		Interruptible: true,
		Step:          (*Mod).miningStep,
	})
	registerTask(TaskDef{
		Kind: TaskMineSell, Label: "Selling ore",
		Posture: PostureDocked, Detail: detailOreHold, Step: (*Mod).miningStep,
	})
	registerTask(TaskDef{
		Kind: TaskMineStockpile, Label: "Stockpiling ore",
		Posture: PostureDocked, Detail: detailOreHold, Step: (*Mod).miningStep,
	})
	registerTask(TaskDef{
		Kind: TaskMineRefine, Label: "Refining ore and listing the minerals",
		Posture: PostureDocked, Detail: detailOreHold, Step: (*Mod).miningStep,
	})

	// Trade.
	registerTask(TaskDef{
		Kind: TaskTradeBuy, Label: "Buying cargo to haul",
		Posture: PostureDocked, Detail: detailTrade, Step: (*Mod).tradeStep,
	})
	registerTask(TaskDef{
		Kind: TaskTradeLoad, Label: "Loading the hold",
		Posture: PostureDocked, Detail: detailTrade, Step: (*Mod).tradeStep,
	})
	registerTask(TaskDef{
		Kind: TaskTradeSell, Label: "Selling its cargo",
		Posture: PostureDocked, Detail: detailTrade, Step: (*Mod).tradeStep,
	})
	registerTask(TaskDef{
		Kind: TaskTradePickup, Label: "Collecting goods it left in a hangar",
		Posture: PostureDocked, Detail: detailTrade, Step: (*Mod).tradeStep,
	})

	// Belt hunting, solo. Looting is declared here but not on the gang's hunt and patrol tasks: a gang
	// hunter cannot dispose of a haul. Only the sweep is interruptible; a half-emptied can strands capital.
	registerTask(TaskDef{
		Kind: TaskCombatSweep, Label: "Combing the belts for rats",
		Posture: PostureCombatBelt, Exposed: true, Loots: true,
		Detail:        detailBounty,
		Interruptible: true,
		Step:          (*Mod).combatStep,
	})
	// The three tactics that are not a belt sweep all loot: a combat pilot that kills something stands
	// over a wreck with a free hold and a reason to go home with it.
	registerTask(TaskDef{
		Kind: TaskCombatRoam, Label: "Roaming for a fight",
		Posture: PostureCombatPatrol, Exposed: true, Loots: true,
		Detail:        detailBounty,
		Interruptible: true,
		Step:          (*Mod).combatStep,
	})
	registerTask(TaskDef{
		Kind: TaskCombatGate, Label: "Holding a gate",
		Posture: PostureCombatGate, Exposed: true, Loots: true,
		Detail:        detailBounty,
		Interruptible: true,
		Step:          (*Mod).combatStep,
	})
	// Not interruptible: a pilot running somebody down holds a decision, and a muster call would leave
	// the mark's bounty unclaimed.
	registerTask(TaskDef{
		Kind: TaskCombatIntercept, Label: "Running down a mark",
		Posture: PostureCombatPatrol, Exposed: true, Loots: true,
		Detail: detailBounty,
		Step:   (*Mod).combatStep,
	})
	registerTask(TaskDef{
		Kind: TaskCombatLoot, Label: "Emptying what the rats left",
		Posture: PostureCombatBelt, Exposed: true, Loots: true,
		Detail: detailBounty,
		Step:   (*Mod).combatStep,
	})
	registerTask(TaskDef{
		Kind: TaskCombatSell, Label: "Selling the haul at its home station",
		Posture: PostureDocked, Detail: detailBounty,
		Step: (*Mod).combatStep,
	})
	// Holds looted goods bound for the corporation's hangar: not droppable.
	registerTask(TaskDef{
		Kind: TaskCombatDonate, Label: "Giving the haul to its corporation",
		Posture: PostureDocked, Detail: detailBounty,
		Step: (*Mod).combatDonateStep,
	})

	// Piracy: prowling, camping and looting are exposed, since a pirate on a lowsec belt or gate is where
	// the rats are too. Lying low is not: it waits where nothing happens.
	registerTask(TaskDef{
		Kind: TaskPiracyProwl, Label: "Prowling the belts for a victim",
		Posture: PostureAmbushBelt, Exposed: true, Detail: detailPiracy,
		Step: (*Mod).piracyStep,
	})
	registerTask(TaskDef{
		Kind: TaskPiracyCamp, Label: "Camping a gate",
		Posture: PostureAmbushGate, Exposed: true, Detail: detailPiracy,
		Step: (*Mod).piracyStep,
	})
	registerTask(TaskDef{
		Kind: TaskPiracyLoot, Label: "Stripping the wreck",
		Posture: PostureAmbushBelt, Exposed: true, Detail: detailPiracy,
		Step: (*Mod).piracyStep,
	})
	registerTask(TaskDef{
		Kind: TaskPiracyFence, Label: "Fencing the haul at home",
		Posture: PostureDocked, Detail: detailPiracy,
		Step: (*Mod).piracyStep,
	})
	registerTask(TaskDef{
		Kind: TaskPiracyLieLow, Label: "Lying low until the criminal timer lapses",
		Posture: PostureLieLow, Detail: detailPiracy,
		Step: (*Mod).piracyStep,
	})

	// Industry: all hangar business, so a bot buying a blueprint or waiting on an assembly line is not
	// out where spawns can jump it.
	for _, p := range []struct {
		kind  TaskKind
		label string
	}{
		{TaskIndBuyBPO, "Buying the blueprint"},
		{TaskIndRefine, "Refining ore at its factory"},
		{TaskIndBuyMats, "Buying the minerals still missing"},
		{TaskIndInstall, "Installing the manufacturing job"},
		{TaskIndCollect, "Collecting the finished goods"},
		{TaskIndSell, "Listing the product for sale"},
		{TaskIndCorpJob, "Running a job for its corporation"},
		{TaskIndCorpMove, "Moving its corporation's factory"},
		{TaskIndPrintMove, "Carrying a blueprint between its corporation's bases"},
	} {
		registerTask(TaskDef{
			Kind: p.kind, Label: p.label, Posture: PostureDocked,
			Detail: detailIndustry,
			Step:   (*Mod).industryStep,
		})
		industryTasks[p.kind] = true
	}

	// The patent errand is not interruptible, nor is its travel leg (it inherits from NextPhase): a won
	// patent has a claim deadline, and a muster would let the capital lapse.
	registerTask(TaskDef{
		Kind: TaskRndCollect, Label: "Collecting a patent from its research agent",
		Posture: PostureDocked, Detail: detailResearch,
		Step: (*Mod).researchStep,
	})
	registerTask(TaskDef{
		Kind: TaskRndHaul, Label: "Hauling a won blueprint to its factory",
		Posture: PostureDocked, Detail: detailResearch,
		Step: (*Mod).researchStep,
	})

	// The envoy's errand is not interruptible either: a letter in hand expires in three days, and a
	// muster would lose the corporation its proposal.
	registerTask(TaskDef{
		Kind: TaskEnvoyDeliver, Label: "Carrying a letter to another corporation",
		Posture: PostureDocked, Detail: detailEnvoy,
		Step: (*Mod).envoyStep,
	})
	registerTask(TaskDef{
		Kind: TaskEnvoyHome, Label: "Coming home from an embassy",
		Posture: PostureDocked, Detail: detailEnvoy,
		Step: (*Mod).envoyStep,
	})

	// The deep-metal carry is not interruptible: the stack is in the hold and the bid is escrowed ISK
	// waiting for it.
	registerTask(TaskDef{
		Kind: TaskDeepCarry, Label: "Carrying deep minerals to its corporation's bid",
		Posture: PostureDocked, Detail: detailDeepCarry,
		Step: (*Mod).deepCarryStep,
	})

	// Courier contract hauling. Not interruptible: collateral is real ISK at
	// risk, and abandoning mid-haul forfeits it.
	for _, p := range []struct {
		kind  TaskKind
		label string
	}{
		{TaskCourierPickup, "Accepting a courier contract at the pickup station"},
		{TaskCourierHaul, "Loading the courier crate"},
		{TaskCourierDeliver, "Delivering the courier crate at the drop station"},
	} {
		registerTask(TaskDef{
			Kind: p.kind, Label: p.label, Posture: PostureDocked,
			Step: (*Mod).courierStep,
		})
	}

	// The lab errand is not interruptible, like the patent pair: from the print leaving the factory
	// hangar until it is back, a muster would strand a blueprint nothing else will look for.
	for _, p := range []struct {
		kind  TaskKind
		label string
	}{
		{TaskLabFetch, "Fetching a blueprint from its factory"},
		{TaskLabInstall, "Installing a research job on its blueprint"},
		{TaskLabCollect, "Collecting a researched blueprint"},
		{TaskLabHaul, "Hauling a researched blueprint to its factory"},
	} {
		registerTask(TaskDef{
			Kind: p.kind, Label: p.label, Posture: PostureDocked,
			Detail: detailLabwork,
			Step:   (*Mod).labworkStep,
		})
	}

	registerTask(TaskDef{
		Kind: TaskLabBuy, Label: "Buying a blueprint to research",
		Posture: PostureDocked, Detail: detailLabShop,
		Step: (*Mod).labShopStep,
	})

	// Escrow. Both run docked and are not interruptible: a listing in
	// flight has items escrowed, and a buy is a one-step transaction.
	registerTask(TaskDef{
		Kind: TaskEscrowList, Label: "Listing items on escrow",
		Posture: PostureDocked, Detail: detailEscrowList,
		Step: (*Mod).escrowListStep,
	})
	registerTask(TaskDef{
		Kind: TaskEscrowBuy, Label: "Claiming an escrow listing",
		Posture: PostureDocked, Detail: detailEscrowBuy,
		Step: (*Mod).escrowBuyStep,
	})

	// The ride home (shuttle.go) is interruptible: a capsule pilot with nothing in the hold and no
	// deadline is cheap for a muster to take, and the next plan buys another shuttle.
	registerTask(TaskDef{
		Kind: TaskRideBuy, Label: "Buying a shuttle to get home",
		Posture: PostureDocked, Interruptible: true,
		Step: (*Mod).rideBuyStep,
	})

	// Gang ops.
	registerTask(TaskDef{
		Kind: TaskGangMine, Label: "Gang op — mining into cans",
		Posture: PostureWorkBelt, Exposed: true,
		Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangHaulLoad, Label: "Gang op — emptying the cans",
		Posture: PostureHold, Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangHaulDrop, Label: "Gang op — dropping the haul off",
		Posture: PostureDocked, Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangOreDrop, Label: "Gang op over — bringing the hold's ore in",
		Posture: PostureDocked, Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangPatrol, Label: "Gang op — patrolling the belts",
		Posture: PostureCombatBelt, Exposed: true, Loots: true,
		Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangHunt, Label: "Gang op — hunting belt rats",
		Posture: PostureCombatBelt, Exposed: true, Loots: true,
		Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangSecurity, Label: "Gang op — patrolling home ground",
		Posture: PostureCombatBelt, Exposed: true, Loots: true,
		Step: (*Mod).gangStep,
	})
	// Emptying a wreck the member made, into its own hold. Exposed: it is on
	// the same belt the rats were.
	registerTask(TaskDef{
		Kind: TaskGangSalvage, Label: "Gang op — looting its wrecks",
		Posture: PostureCombatBelt, Exposed: true,
		Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangWait, Label: "Gang op — holding",
		Posture: PostureHold, Step: (*Mod).gangStep,
	})
	// The pirate pack. Exposed for the same reason the solo pair is: a
	// pirate on a lowsec belt or gate is exactly where the rats are too.
	registerTask(TaskDef{
		Kind: TaskGangPirateProwl, Label: "Gang op — prowling for victims",
		Posture: PostureAmbushBelt, Exposed: true,
		Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangPirateCamp, Label: "Gang op — camping a gate together",
		Posture: PostureAmbushGate, Exposed: true,
		Step: (*Mod).gangStep,
	})
	// The gank fleet (gank_op.go). The stage is a hold, not an ambush posture, so it is not Exposed. Scout
	// and strike are gate ambushes; loot does its own taking (gankLootStep).
	registerTask(TaskDef{
		Kind: TaskGangGankStage, Label: "Gank fleet — staged one jump out",
		Posture: PostureHold,
		Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangGankScout, Label: "Gank fleet — scanning the gate",
		Posture: PostureAmbushGate, Exposed: true,
		Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangGankStrike, Label: "Gank fleet — striking the mark",
		Posture: PostureAmbushGate, Exposed: true,
		Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangGankLoot, Label: "Gank fleet — stripping the wrecks",
		Posture: PostureAmbushBelt, Exposed: true,
		Step: (*Mod).gangStep,
	})
	// The checkpoint: the solo gate hold's posture (rules of engagement live) as a fleet, with the wave
	// and the toll on top (camp.go).
	registerTask(TaskDef{
		Kind: TaskGangCheckpoint, Label: "Gang op — holding a checkpoint",
		Posture: PostureCombatGate, Exposed: true,
		Step: (*Mod).gangStep,
	})
	// The fleet lifecycle. The settlement round is a docked waiting room, so it is not exposed; the
	// muster is docked only until a member has its ship.
	registerTask(TaskDef{
		Kind: TaskGangMuster, Label: "Gang op — mustering at the staging station",
		Posture: PostureDocked, Step: (*Mod).gangStep,
	})
	// Formed up in space: PostureHold so the sim owns the position (gangFormUpCircle, embody.go). Not
	// Exposed: the abstract encounter roll is for a pilot out working, and a staging station's guns make it safe.
	registerTask(TaskDef{
		Kind: TaskGangFormUp, Label: "Gang op — formed up off the staging station",
		Posture: PostureHold, Step: (*Mod).gangStep,
	})
	registerTask(TaskDef{
		Kind: TaskGangSettle, Label: "Gang op — settling the books at staging",
		Posture: PostureDocked, Step: (*Mod).gangStep,
	})
	// Gang combat tasks loot for the member itself; conscript tasks are neither Exposed nor Loots.
	// See docs/comment-notes-b18.md#task-conscript-flags.
	for _, c := range []struct {
		kind  TaskKind
		label string
		post  Posture
	}{
		{TaskConscriptForm, "Joining a player's fleet", PostureTransit},
		{TaskConscriptEscort, "Flying with a player's fleet", PostureEscort},
		{TaskConscriptFight, "Fighting for a player's fleet", PostureEscort},
		{TaskConscriptHold, "Holding position for a player's fleet", PostureHold},
		{TaskConscriptDeliver, "Delivering for a player's fleet", PostureDocked},
	} {
		registerTask(TaskDef{
			Kind: c.kind, Label: c.label, Posture: c.post,
			Detail: detailConscript,
			Step:   (*Mod).conscriptStep,
		})
	}
}
