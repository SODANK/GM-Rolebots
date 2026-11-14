package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	serverram "gurimoon/server/ram"
)

// Skill investment: a rolebot buying and training its own skills.
//
// A pilot prices the options against its own appetites and picks the best ratio.
// What a step is worth is skillscore.go; this file is the shopping and the queue,
// which is the core's: the plan is written into chrSkillQueue and the server's own
// sweep advances it, logged in or not.
//
// The planning unit is a chain, not a step: priority = value(goal) / minutes of the
// whole prerequisite closure. A wanted skill is pushed to SkillSatisfiedLevel (3 by
// default), where it stops being a gate and starts being an ability.

const (
	// skillSatisfiedLevelDefault is where a wanted skill stops being urgent.
	skillSatisfiedLevelDefault = 3
	// skillPlanHorizonMinDefault is how much training a plan lays down: two days, long
	// enough that the pilot is never idle between replans, short enough to stay current.
	skillPlanHorizonMinDefault = 48 * 60.0
	// skillStepMaxMinutesDefault caps a single step at three days, so one rank-6 level V
	// cannot swallow a plan. A step over the cap is affordable again once the pilot's
	// learning attributes have grown.
	skillStepMaxMinutesDefault = 3 * 24 * 60.0
	// skillGroupCapDefault is how many steps of one skill group (Gunnery, Navigation,
	// ...) one plan may contain: the diversification brake.
	skillGroupCapDefault = 3
	// skillLearningHorizonMinDefault bounds what a learning skill may claim to save:
	// thirty days of foreseen training. Uncapped, learning always wins.
	skillLearningHorizonMinDefault = 30 * 24 * 60.0
	// skillBookCushionDefault multiplies the reserve a pilot keeps back from book
	// buying: its clone cover plus a replacement for the ship it flies.
	skillBookCushionDefault = 1.0
	// skillWalletFloorDefault is ISK the pilot never spends on books. The clone-and-hull
	// reserve covers only what it owns; this standing float keeps a pilot with nothing
	// able to buy the hull the skills were for.
	skillWalletFloorDefault = 1_000_000.0

	// skillHoldbackMaxShare caps everything kept back from book money (wallet floor plus
	// clone-and-hull reserve) as a share of what the pilot has: three quarters.
	skillHoldbackMaxShare = 0.75

	// skillReplanSec is how often a pilot rethinks: hourly notices a new fit, a fresh
	// wallet or a finished book, and is cheap enough to run one pilot per sim pass.
	skillReplanSec = 3600
	// skillMaxPlanSteps bounds one plan; the next replan extends it with better
	// information.
	skillMaxPlanSteps = 24
	// skillChainMaxDepth guards the prerequisite walk so a data error cannot hang a pass.
	skillChainMaxDepth = 8
	// industrySkillCandidates is how far down the demand ledger the planner looks for
	// blueprints the pilot is locked out of: the handful its industry planner picks from.
	industrySkillCandidates = 12
	// skillEvalBudget bounds the expensive half, re-judging fits against skill sheets
	// the pilot does not have. Each evaluation resolves a fit from scratch, outside the
	// derived-fit cache (skillscore.go), so the count is bounded.
	skillEvalBudget = 150
)

// skillStep is one queued training step the planner produced.
type skillStep struct {
	TypeID   int64
	Name     string
	Level    int
	GroupID  int64
	Minutes  float64
	Learning bool
	NeedBook bool    // the pilot does not own this book yet
	Price    float64 // quoted book price at plan time, 0 when nothing to buy
	Why      string
	// Kind is the goal kind that pulled this step in (one of the goal* constants below).
	// Why names the ship or blueprint for a person; Kind lets the Skills page group
	// without parsing prose.
	Kind  string
	Score float64 // the chain score that pulled it in
}

// skillGoal is one thing the pilot might want, and the chain that gets it
// there. Goals are built once per plan and re-priced each round, because
// committing one chain changes what the others cost.
type skillGoal struct {
	TypeID int64
	Level  int
	Kind   string // "fit" | "deepen" | "learning" | "forced"
	Why    string

	// leadsTo is the processing skill a goalProcessing stepping stone
	// (Science, Metallurgy) is on the road to; 0 on every other goal.
	leadsTo int64

	chain   []skillStep
	minutes float64
	books   float64
	touches map[int]bool // fit pairs this chain could move
	value   float64
	valued  bool
	dead    bool
}

const (
	goalFit      = "fit"
	goalDeepen   = "deepen"
	goalLearning = "learning"
	goalForced   = "forced"
	// The factory's two, mirroring the ship's two: a gate is a blueprint the
	// pilot cannot install at all, a lever is a skill that changes the
	// numbers on the ones it can.
	goalIndGate  = "industry-gate"
	goalIndLever = "industry"
	// The two levers that touch no requirement list at all (skillsupport.go):
	// what makes the ship it already flies work properly, and how much of the
	// market it may have open at once.
	goalSupport = "support"
	goalMarket  = "market"
	// The R&D field skill (research.go): what opens a collaboration with a
	// research agent, and then what raises the pilot's ticket rate in that
	// agent's Tech II patent allocation.
	goalResearch = "research"
	// The lab levers (labwork.go): what makes a blueprint research job finish sooner. A
	// third factory kind: industryPower does not model research time, so the lever arm
	// would price every one at zero.
	goalLab = "lab"
	// The lab slot skills (labwork.go): what buys a second research job beside the first.
	// Separate from goalLab: a lever shortens one job, a slot adds another, and the
	// time-lever curve would score a doubling of throughput as 5%.
	goalLabSlot = "labslot"
	// Cybernetics (implantupkeep.go): every implant requires it, at I for cheap ones and
	// up to V for a set piece. Priced as the share of the ship the implants the pilot
	// holds or wants would add.
	goalImplant = "implant"
	// The refinery pair, priced apart from the factory levers: industryPower reads Refining
	// as a throughput slope, but refineWorthEdge makes one level a step that flips
	// resolveMiningDisposal from sell the rock to refine it. See evaluateRefine.
	goalRefine = "refine"
	// A Jove pilot's own hull line (joveLadder). Selected like an operator pin, ahead of
	// the arithmetic, and exempt from the step and group caps: the hull skills are rank
	// 6-12 and no seeded fit asks for them, so the ranking would never train them.
	goalJove = "jove"
	// Command (corpceo_skills.go): Leadership for a pilot that wants a fleet
	// seat, and the member-limit ladder for a bot CEO holding applicants at
	// a full door. Neither touches a fit, so neither could win on ship score.
	goalCommand = "command"
	// The reprocessing ladder (skills_processing.go): Science IV, Metallurgy, then the
	// processing skill for a rock. Its own kind: only the whole chain buys yield, so it
	// is priced as a chain, not as levers.
	goalProcessing = "processing"
	// The deep core road (skills_deepcore.go): Mining V, Astrogeology V, Deep Core Mining,
	// the gate to Mercoxit. Priced as a chain like the processing ladder and exempt from
	// the step and group caps.
	goalDeepCore = "deepcore"
	// The barge and ice road (skills_ice.go): barge, ice, then harvest levers for a pilot
	// that owns a barge. Priced as a road like the deep core one, since no stored fit
	// carries an ice harvester, and exempt from the step and group caps.
	goalIce = "ice"
	// The mining cruiser road (skills_minecruiser.go): a mining-career pilot's racial
	// Cruiser skill, then its levels once it flies one. Priced as a road like the ice
	// road, and exempt from the step cap.
	goalMineCruiser = "minecruiser"
	// The lifter's skills (skills_lift.go): racial Industrial, General Freight and Hull
	// Upgrades for an Infrastructure member, priced by the lift they add; no fit's score
	// sees a hold that is only big enough.
	goalLift = "lift"
)

// raceJove is chrRaces.raceID for the Jove.
const raceJove = 16

// joveLadder is the Jove hull line in climbing order, with the level each rung is
// worth training to (from the hulls themselves). Spaceship Command V is granted at
// creation for race 16 (db/chardb.go CreateCharacter); Caldari Frigate IV is a chain step.
var joveLadder = []struct {
	typeID int64
	cap    int
}{
	{3755, 4},  // Jove Frigate
	{3758, 4},  // Jove Cruiser
	{11075, 5}, // Jove Industrial
	{11078, 1}, // Jove Battleship
}

// joveGoalWhy is the Why text of a Jove rung. The skills board's fallback classifier
// matches it exactly (skillKindCase), so it is one fixed string.
const joveGoalWhy = "the Jove hulls it was born to fly"

// cheapGoal reports whether a goal can be priced without resolving a fit.
// The industry, support and market goals are all arithmetic over numbers
// already in hand, so they are never rationed out of the evaluation budget
// the fit half has to share.
func cheapGoal(kind string) bool {
	switch kind {
	case goalIndGate, goalIndLever, goalSupport, goalMarket, goalResearch, goalLab, goalLabSlot,
		goalRefine, goalCommand, goalProcessing, goalDeepCore, goalIce, goalMineCruiser, goalLift,
		goalImplant, goalProgram:
		return true
	}
	return false
}

// researchLevelWorth is what a full research collaboration is worth to a manufacturer,
// in the units the industry gates use: about a fifth of unlocking the whole candidate
// list. Modest, so a patent never outbids a hull or blueprint gate.
const researchLevelWorth = 0.2

// researchOpenWorth is what opening a collaboration is worth (the field skill's first
// level), priced on industryGateWorth's anchor as a step, not a lever.
// See docs/comment-notes-b17.md#research-open-worth.
const researchOpenWorth = 1.0

// labLeverWorth is what the lab lane's throughput is worth beside the factory's, on
// the same pool anchor. Below 1.0: a lab lever raises a blueprint's ME and only then
// makes builds cheaper, which is slower than the output it feeds.
const labLeverWorth = 0.30

// labSlotWorth is what a parallel research slot is worth on the same anchor. The raw
// ratio already rewards a slot (0 to 1 slots is a gain of 1.0), so this carries only
// what the ratio overstates. Level with labLeverWorth: a copy job is income (bpcopy.go).
const labSlotWorth = 0.30

// industryGateWorth is what opening the whole candidate list is worth, measured in
// the pilot's best ship; a skill that unlocks half the list is worth half. Tying the
// anchor to the pilot's ship makes the exchange rate scale with the pilot.
const industryGateWorth = 1.0

// industryRefFallback is the anchor when the pilot has no flyable ship at all
// to measure against. Only reachable on a pilot that cannot fly anything,
// where training toward a factory is a perfectly reasonable answer.
const industryRefFallback = 100.0

// skillPlanner is one pilot's planning pass. Everything it needs is loaded once and
// mutated in memory as chains commit: the levels promised, the learning attributes
// bought, the money spent.
type skillPlanner struct {
	m      *Mod
	ctx    context.Context
	charID int64
	tn     Tuning
	raceID int64

	rows     map[int64]evedb.ESPSkillCatalogueRow
	levels   map[int64]int // projected: trained + training + already queued, then + this plan
	injected map[int64]bool
	stats    map[int64]float64 // effective learning attributes, moved by learning steps
	learn    map[int64]learningBonus
	prereqs  map[int64][]evedb.SkillReq
	prices   map[int64]evedb.SkillPriceQuote

	fits    []*fitBase
	weights map[string]float64

	// The factory half. wIndustry is the manufacture disposition (SupplyDrive x
	// IndustryAppetite; the drive floors at 0.15, so never 0). wRefine is the mining
	// side's claim on the refinery levers: a skilled miner refines and lists (mining.go).
	wIndustry float64
	wRefine   float64
	// wMine is the mining disposition itself: the deep core road's weight
	// (skills_deepcore.go).
	wMine   float64
	poolRef float64 // the pilot's best ship, the anchor industry is priced in
	levers  industryLevers
	// cmdSeat memoises commandSeatValue for the life of one plan.
	cmdSeat *float64
	// hasBarge memoises ownsBarge for the life of one plan (skills_ice.go).
	hasBarge *bool
	// anchorCorpID memoises anchorCorp for the life of one plan (posanchor.go).
	anchorCorpID *int64
	// liftCorpID memoises liftCorp for the life of one plan (skills_lift.go).
	liftCorpID *int64
	indGates   map[int64]int // skill to candidate blueprints it would unlock
	indGateLv  map[int64]int // skill to the hardest level any of them asks for
	indCands   int           // candidates considered, the denominator for a gate
	// The R&D science fields this pilot researches, or means to (research.go).
	// It sits in the factory half because a Tech II patent is a blueprint, and
	// the whole reason to hold one is to build from it.
	rndFields []int64
	// implantNeeds are the Cybernetics levels the implants this pilot holds
	// in a hangar, or wants, would need (implantupkeep.go).
	implantNeeds []implantNeed

	// The support half (skillsupport.go): the pilot's best ship at each job it wants, kept
	// as an all-V stat readout so levers absent from requirement lists can be priced by
	// rebasing it onto a real sheet. skillAttrs holds their per-level bonuses from static data.
	refs       []*jobRef
	skillAttrs map[int64]map[int64]float64

	// The order book. wMarket is the trade disposition plus a share of the factory's (a
	// manufacturer has to sell what it builds), so a pilot with no interest in commerce
	// and no factory never trains a trade skill.
	wMarket   float64
	marketRef float64
	// wTrade is the commerce half of wMarket alone: the trader's own claim without the
	// factory's share. The processing ladder weighs the two careers separately
	// (procWeight), so it needs them apart.
	wTrade float64
	// procOres are the processing skills of the rocks this pilot actually handles, most
	// specific first (oreSkillCandidates), at most processingHandledMax. OreSkillID on
	// the levers is the first of them.
	procOres []int64

	force map[int64]bool
	ban   map[int64]bool

	budget   float64 // ISK the pilot may still spend on books this plan
	groupUse map[int64]int
	evals    int

	// The region set this pilot prices against (materialize.go), resolved once here
	// because the planner runs from a background sweep with no botRun and the
	// ship-replacement reserve must be worth what a hull costs where this pilot shops.
	regions []int64
}

// newSkillPlanner loads everything one planning pass reads.
func (m *Mod) newSkillPlanner(ctx context.Context, charID int64, tn Tuning) (*skillPlanner, error) {
	catalogue, err := m.skillCatalogueFor(ctx, charID)
	if err != nil {
		return nil, err
	}
	p := &skillPlanner{
		m: m, ctx: ctx, charID: charID, tn: tn,
		rows:     make(map[int64]evedb.ESPSkillCatalogueRow, len(catalogue)),
		injected: map[int64]bool{},
		groupUse: map[int64]int{},
		weights:  jobWeights(tn),
		regions:  m.valuationRegionsForChar(ctx, charID),
	}
	for _, row := range catalogue {
		p.rows[row.TypeID] = row
		if row.Known {
			p.injected[row.TypeID] = true
		}
	}
	// Projected, not trained: a skill already queued is a skill the plan may
	// build on, and one it must not queue twice.
	p.levels, err = evedb.ProjectedSkillLevels(ctx, m.db, charID)
	if err != nil {
		return nil, err
	}
	if p.prereqs, err = evedb.GetSkillPrereqMap(ctx, m.db); err != nil {
		return nil, err
	}
	if p.learn, err = m.learningSkills(ctx); err != nil {
		return nil, err
	}
	if p.prices, err = evedb.SkillMarketPrices(ctx, m.db); err != nil {
		return nil, err
	}
	p.stats = evedb.ESPEffectiveLearningStats(ctx, m.db, charID)
	p.raceID = m.charRace(ctx, charID)
	if p.force, p.ban, err = skillPrefs(ctx, m.db, charID); err != nil {
		return nil, err
	}
	if err := p.loadFits(); err != nil {
		return nil, err
	}
	if err := p.loadIndustry(); err != nil {
		return nil, err
	}
	p.loadSupport()
	p.loadMarket()
	if err := p.loadBudget(); err != nil {
		return nil, err
	}
	return p, nil
}

// loadSupport picks the ship this pilot actually flies for each job it wants and
// reads its stats once. It uses the best flyable fit, not the best aspirational one:
// support skills make the ship in the hangar work, and a pilot who can fly nothing
// for a job trains no support skills toward it (the gate comes first).
func (p *skillPlanner) loadSupport() {
	best := map[string]*fitBase{}
	for _, fb := range p.fits {
		if fb.score <= 0 || fb.weight <= 0 {
			continue
		}
		if b, ok := best[fb.job]; !ok || fb.score > b.score {
			best[fb.job] = fb
		}
	}
	for _, jd := range jobCatalog {
		fb := best[jd.Key]
		if fb == nil {
			continue
		}
		eff, err := p.m.effectiveFitWithSkills(p.ctx, p.charID, fb.job, fb.stored, p.levels)
		if err != nil || eff == nil {
			continue
		}
		stats, err := p.m.ComputeFitStats(p.ctx, eff.Fit)
		if err != nil {
			continue
		}
		p.refs = append(p.refs, &jobRef{
			Job: fb.job, Stats: stats, Drones: launchedDrones(eff.Fit),
			Score: fb.score, Weight: fb.weight, FitName: fb.stored.Name,
		})
	}
	// The per-level bonuses, straight off the skills: the three attributes
	// db.GetWeaponCombatBonuses reads and the scan-resolution one AddTarget reads.
	p.skillAttrs = map[int64]map[int64]float64{}
	ids := append(append([]int64{}, turretSupportSkills...), launcherSupportSkills...)
	ids = append(ids, skillSignatureAnalysis, skillMissileProjection, skillMissileBombardment,
		skillSharpshooter, skillTrajectoryAnalysis,
		skillEnergyManagement, skillEnergySystemsOp)
	for _, id := range ids {
		if attrs, err := evedb.GetTypeAttributes(p.ctx, p.m.db, id); err == nil {
			p.skillAttrs[id] = attrs
		}
	}
}

// loadMarket works out how much this pilot cares about the order book. Anchored on
// its hauler where it has one, since a trader's business is the ship and the book
// together; the pool's best ship stands in otherwise.
func (p *skillPlanner) loadMarket() {
	var wCommerce float64
	for _, d := range activityCatalog {
		if d.Slug == ActTrade {
			wCommerce = d.Disposition(p.tn)
		}
	}
	// tradeInterest, not the raw appetite: every pilot has a commerce appetite near 1.0,
	// so weighing the book on it would have the whole roster training Retail (tradeorders.go).
	p.wTrade = wCommerce * tradeInterest(p.tn)
	p.wMarket = p.wTrade + marketIndustryShare*p.wIndustry
	p.marketRef = p.poolRef
	for _, ref := range p.refs {
		if ref.Job == JobTrade && ref.Score > 0 {
			p.marketRef = ref.Score
		}
	}
}

// loadIndustry sets up the factory half: how much this pilot cares, what its refining
// looks like, and which blueprints it is locked out of. The appetite comes from the
// activity catalog, so one place decides whether a pilot manufactures and trains for it.
func (p *skillPlanner) loadIndustry() error {
	for _, d := range activityCatalog {
		switch d.Slug {
		case ActIndustry:
			p.wIndustry = d.Disposition(p.tn)
		case ActMining:
			// Refining is half a miner's trade (mining.go), so the yield levers are priced off
			// the mining disposition too: a young miner trains toward the career.
			p.wRefine = miningRefineShare * d.Disposition(p.tn)
			p.wMine = d.Disposition(p.tn)
		}
	}
	// The anchor: the best ship this pilot could put in space right now.
	for _, fb := range p.fits {
		if fb.score > p.poolRef {
			p.poolRef = fb.score
		}
	}
	if p.poolRef <= 0 {
		p.poolRef = industryRefFallback
	}
	p.indGates, p.indGateLv = map[int64]int{}, map[int64]int{}
	if p.wIndustry <= 0 {
		return nil // a consumer: nothing below would ever be worth reading
	}

	// Refining is measured against the station this pilot uses and the ore it mines:
	// a level is worth what it recovers, which depends on both.
	p.levers.StationEff = refineStationEffDefault
	stationID := int64(0)
	if prof, err := p.m.getProfile(p.ctx, p.charID); err == nil && prof != nil {
		stationID = prof.IndustryStationID
	}
	if stationID == 0 {
		_ = p.m.db.QueryRowContext(p.ctx,
			`SELECT COALESCE(stationID, 0) FROM character_ WHERE characterID = ?`,
			p.charID).Scan(&stationID)
	}
	if stationID != 0 {
		if sri, err := evedb.GetStationReprocessingInfo(p.ctx, p.m.db, stationID); err == nil && sri != nil {
			p.levers.StationEff = sri.Efficiency
		}
	}
	p.implantNeeds = p.m.implantSkillNeeds(p.ctx, p.charID, p.tn)
	if st, err := loadState(p.ctx, p.m.db, p.charID); err == nil && st != nil {
		// The R&D fields, including the one the enrolment lane earmarked but cannot open yet;
		// that earmark breaks the chicken-and-egg, since StartResearch refuses an untrained field.
		p.rndFields = researchFieldsFor(p.ctx, p.m.db, p.charID, st.Data.RndFieldID)

		// Which rock's processing skill is worth training (oreSkillCandidates). The ore skill
		// is 0.05 a level, the largest yield term, so the wrong family wastes most of the gain.
		for _, c := range oreSkillCandidates(st.Data) {
			var groupID int64
			var err error
			if c.TypeID != 0 {
				if groupID = p.m.typeGroupOf(p.ctx, c.TypeID); groupID == 0 {
					err = sql.ErrNoRows
				}
			} else {
				err = p.m.db.QueryRowContext(p.ctx,
					`SELECT groupID FROM invTypes WHERE typeName = ?`, c.Name).Scan(&groupID)
			}
			if err != nil {
				continue
			}
			id, err := evedb.GetOreProcessingSkill(p.ctx, p.m.db, groupID)
			// 12196 is the lookup's own fallback (Scrapmetal Processing), not a rock: it needs
			// Refinery Efficiency V and Metallurgy V and refines nothing an ore bill wants.
			if err != nil || id == 0 || id == skillScrapmetalProcessing {
				continue
			}
			if p.levers.OreSkillID == 0 {
				p.levers.OreSkillID = id
			}
			// Every rock it handles, not just the first, for the
			// reprocessing ladder (skills_processing.go).
			p.procOres = appendUniqueCapped(p.procOres, id, processingHandledMax)
		}
	}

	// The gates: what its industry planner would try to build and which skills stand in
	// the way, from the same candidate query industryStartProject runs.
	cands, err := p.m.industryCandidates(p.ctx, p.charID, industrySkillCandidates)
	if err != nil {
		return nil // the demand ledger being empty is not a planning failure
	}
	p.indCands = len(cands)
	for _, c := range cands {
		reqs, err := evedb.GetActivitySkillReqs(p.ctx, p.m.db, c.BPTypeID, 1)
		if err != nil {
			continue
		}
		for _, r := range reqs {
			if p.levels[r.SkillID] < r.Level {
				p.indGates[r.SkillID]++
				if r.Level > p.indGateLv[r.SkillID] {
					p.indGateLv[r.SkillID] = r.Level
				}
			}
		}
	}
	return nil
}

// oreSkillPick is one candidate rock, by whichever handle the state carries: IndOreWanted
// is a hierarchy of names (read by a miner choosing a belt) and OreTypeID is an id.
// Carrying both keeps the ordering below pure.
type oreSkillPick struct {
	Name   string
	TypeID int64
}

// oreSkillCandidates is the rock this pilot's processing skill should be trained for,
// most specific first: what a materials run was dispatched after (IndOreWanted), then
// what it last mined (OreTypeID). The first that resolves to a real processing skill
// wins, and that order is the content of this function.
func oreSkillCandidates(d StateData) []oreSkillPick {
	var out []oreSkillPick
	seenName := map[string]bool{}
	for _, name := range d.IndOreWanted {
		if name == "" || seenName[name] {
			continue
		}
		seenName[name] = true
		out = append(out, oreSkillPick{Name: name})
	}
	if d.OreTypeID != 0 {
		out = append(out, oreSkillPick{TypeID: d.OreTypeID})
	}
	return out
}

func (p *skillPlanner) industryLeverSkills() []int64 {
	out := []int64{skillIndustry, skillProductionEff, skillIndRefining, skillIndRefineryEf,
		serverram.SkillMassProduction, serverram.SkillAdvancedMassProduction}
	if p.levers.OreSkillID != 0 {
		out = append(out, p.levers.OreSkillID)
	}
	return out
}

// loadFits builds the (fit, job) pairs the plan is judged against and their
// baseline scores. Every fit against every job whose appetite is non-zero:
// what a pilot should train for is what it would fly, and what it would fly
// is decided by scoring, not by a label on the fit.
func (p *skillPlanner) loadFits() error {
	// The aspirational library, not the flying one (fitgen.go): the ships this pilot might
	// grow into. The list it can already fly has every requirement met.
	pool, err := p.m.aspirationalFits(p.ctx, p.charID, p.weights, p.tn)
	if err != nil {
		return err
	}
	for _, stored := range pool {
		reqs, err := p.m.fitRequirementLevels(p.ctx, stored)
		if err != nil {
			return err
		}
		// A style variant of a combat fit (fitstyle.go) carries only the share of appetite the
		// pilot's temperament puts on that style; the auto variant keeps the whole weight.
		brawlShare, kiteShare := styleShares(p.tn)
		styleMul := 1.0
		switch stored.Style {
		case engageBrawl:
			styleMul = brawlShare
		case engageKite:
			styleMul = kiteShare
		}
		for _, jd := range jobCatalog {
			w := p.weights[jd.Key] * styleMul
			if w <= 0 {
				continue // this pilot has no appetite for this kind of work
			}
			fb := &fitBase{stored: stored, job: jd.Key, weight: w, reqs: reqs}
			fb.score, _ = p.m.judgeHypothetical(p.ctx, p.charID, jd.Key, stored, p.levels, p.raceID)
			p.fits = append(p.fits, fb)
		}
	}
	return nil
}

// loadBudget works out what the pilot may spend on skillbooks: its wallet, less the
// standing floor it never spends, less the reserve to stay insured and replace the
// ship it flies. The clone reserve matters because raising SP crosses clone grade
// boundaries (clone.go); the floor covers a pilot with nothing to reserve against.
func (p *skillPlanner) loadBudget() error {
	bal, err := evedb.GetCharBalance(p.ctx, p.m.db, p.charID)
	if err != nil {
		return fmt.Errorf("wallet: %w", err)
	}
	if bal <= 0 {
		p.budget = 0
		return nil
	}
	reserve := p.m.cloneReserve(p.ctx, p.charID)
	// The ship it is actually flying, at what it would cost to buy again.
	if _, t, err := p.m.shipForJob(p.ctx, p.charID, JobCombat); err == nil && t != nil {
		if cost, err := p.m.fitCostIn(p.ctx, p.regions, t); err == nil {
			reserve += cost
		}
	}
	p.budget = bal - holdback(bal, p.tn.SkillWalletFloor+reserve*p.tn.SkillBookCushion)
	if p.budget < 0 {
		p.budget = 0
	}
	return nil
}

// holdback is what the pilot keeps back from book money, never more than a share of the
// wallet. A reserve that consumes the whole wallet stops a poor pilot learning: the
// floor is a flat 1M and a new character starts with 1M, so the cap leaves a quarter
// of any wallet as book money.
func holdback(balance, want float64) float64 {
	if max := balance * skillHoldbackMaxShare; want > max {
		return max
	}
	return want
}

// ── chains ──────────────────────────────────────────────────────────────────

func (p *skillPlanner) levelOf(typeID int64, over map[int64]int) int {
	if v, ok := over[typeID]; ok {
		return v
	}
	return p.levels[typeID]
}

// rate is this pilot's SP per minute on one skill, at the learning
// attributes the plan has reached so far.
func (p *skillPlanner) rate(row evedb.ESPSkillCatalogueRow) float64 {
	return evedb.ESPSkillSPPerMinute(p.stats, row.PrimaryAttrID, row.SecondaryAttrID)
}

// chainFor returns the ordered steps that take the pilot from where the plan
// currently stands to typeID at goal, prerequisites first. ok=false when the chain
// cannot be built: a banned skill in the way, a prerequisite the static data does
// not describe, or a depth that says the data is wrong.
func (p *skillPlanner) chainFor(typeID int64, goal int) ([]skillStep, bool) {
	var out []skillStep
	over := map[int64]int{}
	if !p.walk(typeID, goal, over, &out, 0) {
		return nil, false
	}
	return out, true
}

func (p *skillPlanner) walk(typeID int64, goal int, over map[int64]int, out *[]skillStep, depth int) bool {
	if depth > skillChainMaxDepth {
		return false
	}
	if goal > 5 {
		goal = 5
	}
	row, ok := p.rows[typeID]
	if !ok || p.ban[typeID] {
		return false
	}
	if p.levelOf(typeID, over) >= goal {
		return true
	}
	for _, r := range p.prereqs[typeID] {
		if !p.walk(r.SkillID, r.Level, over, out, depth+1) {
			return false
		}
	}
	// The book is bought once, on the step that first needs it.
	needBook := !p.injected[typeID]
	rate := p.rate(row)
	for lvl := p.levelOf(typeID, over) + 1; lvl <= goal; lvl++ {
		mins := stepMinutes(row.Rank, float64(row.SkillPoints), lvl, rate)
		price := 0.0
		if needBook {
			price = p.bookPrice(typeID)
		}
		*out = append(*out, skillStep{
			TypeID:   typeID,
			Name:     row.TypeName,
			Level:    lvl,
			GroupID:  row.GroupID,
			Minutes:  mins,
			Learning: p.isLearning(typeID),
			NeedBook: needBook,
			Price:    price,
		})
		needBook = false
		over[typeID] = lvl
	}
	return true
}

func (p *skillPlanner) isLearning(typeID int64) bool {
	_, ok := p.learn[typeID]
	return ok
}

// skillByName resolves a skill name to its typeID from the catalogue.
// Returns 0 when the name is not in this database.
func (p *skillPlanner) skillByName(name string) int64 {
	for _, row := range p.rows {
		if row.TypeName == name {
			return row.TypeID
		}
	}
	return 0
}

// bookPrice is what the pilot would pay Genolution for a book it does not own. Zero
// means unpriced, which the shop refuses to sell: treated as cannot have it, not free.
func (p *skillPlanner) bookPrice(typeID int64) float64 {
	q, ok := p.prices[typeID]
	if !ok {
		return 0
	}
	return q.Price
}

// chainCost sums a chain's minutes and the books it has to buy. buyable is
// false when any book it needs has no price at all.
func (p *skillPlanner) chainCost(chain []skillStep) (minutes, books float64, buyable bool) {
	buyable = true
	for _, s := range chain {
		minutes += s.Minutes
		if s.NeedBook {
			price := p.bookPrice(s.TypeID)
			if price <= 0 {
				buyable = false
			}
			books += price
		}
	}
	return minutes, books, buyable
}

// touching is the set of (fit, job) pairs a chain could move: those whose authored
// requirements mention any skill in it. Everything else scores the same before and
// after, so re-judging it is wasted work; this keeps the value pass affordable.
func (p *skillPlanner) touching(chain []skillStep) map[int]bool {
	out := map[int]bool{}
	for i, fb := range p.fits {
		for _, s := range chain {
			if _, ok := fb.reqs[s.TypeID]; ok {
				out[i] = true
				break
			}
		}
	}
	return out
}

// ── value ───────────────────────────────────────────────────────────────────

// evaluate prices a fit-facing goal: what the whole chain would add to the
// pool, weighted by how much this pilot wants each kind of work.
func (p *skillPlanner) evaluate(g *skillGoal) {
	switch g.Kind {
	case goalIndGate, goalIndLever:
		p.evaluateIndustry(g)
		return
	case goalSupport:
		p.evaluateSupport(g)
		return
	case goalMarket:
		p.evaluateMarket(g)
		return
	case goalResearch:
		p.evaluateResearch(g)
		return
	case goalImplant:
		p.evaluateImplant(g)
		return
	case goalLab:
		p.evaluateLab(g)
		return
	case goalLabSlot:
		p.evaluateLabSlot(g)
		return
	case goalRefine:
		p.evaluateRefine(g)
		return
	case goalCommand:
		p.evaluateCommand(g)
		return
	case goalProgram:
		p.evaluateProgram(g)
		return
	case goalProcessing:
		p.evaluateProcessing(g)
		return
	case goalDeepCore:
		p.evaluateDeepCore(g)
		return
	case goalIce:
		p.evaluateIce(g)
		return
	case goalMineCruiser:
		p.evaluateMineCruiser(g)
		return
	case goalLift:
		p.evaluateLift(g)
		return
	}
	sheet := p.sheetWith(g.chain)
	total := 0.0
	for i := range p.fits {
		if !g.touches[i] {
			continue
		}
		fb := p.fits[i]
		after, err := p.m.judgeHypothetical(p.ctx, p.charID, fb.job, fb.stored, sheet, p.raceID)
		if err != nil {
			continue
		}
		p.evals++
		if gain := after - fb.score; gain > 0 {
			total += fb.weight * gain
		}
	}
	g.value, g.valued = total, true
}

// refineGateWorth is what crossing the disposal bar is worth, in the units the industry
// gates use: selling rock into an NPC sink versus supplying metal. It sits under
// researchLevelWorth and industryGateWorth so a refinery skill never outbids a hull gate.
const refineGateWorth = 0.25

// refineTradeShare is how much of a trader's market weight backs the refinery levers:
// a second lane, not a second career (cf. miningRefineShare, marketIndustryShare).
// Without it a pure commerce pilot priced Refining at a sliver and never trained it.
const refineTradeShare = 0.5

// refineGoalValue is the whole rule, pulled out pure so the harness can pin both arms
// without a database. The step arm: a level that lifts a pilot from under the bar to
// over it is worth a fixed jump, not the 0.8% of yield it adds. Otherwise the
// ordinary lever arm applies.
func refineGoalValue(before, after, bar, weight, ref float64) float64 {
	if before < bar && after >= bar {
		return weight * ref * refineGateWorth
	}
	if after <= before || before <= 0 {
		return 0
	}
	return weight * ref * (after/before - 1)
}

// evaluateRefine prices the refinery pair (see goalRefine and refineGoalValue). The
// weight is every career that benefits: the factory refines its own feedstock, the
// miner refines instead of feeding the bid, the trader buys rock to flip as metal.
func (p *skillPlanner) evaluateRefine(g *skillGoal) {
	g.valued = true
	w := p.wIndustry + p.wRefine + refineTradeShare*p.wMarket
	if w <= 0 || p.poolRef <= 0 {
		g.value = 0
		return
	}
	bar := oreIntrinsicSpread * refineWorthEdge
	g.value = refineGoalValue(p.refineNet(p.levels), p.refineNet(p.sheetWith(g.chain)),
		bar, w, p.poolRef)
}

// refineNet is this pilot's after-tax reprocessing yield on a given sheet, measured
// as resolveMiningDisposal measures it: a station-standard plant, the standard tax, no
// ore-specific skill. oreRefiners shows the same reading on the panel, so training
// and the panel share one number.
func (p *skillPlanner) refineNet(levels map[int64]int) float64 {
	return refineEffYield(refineStationEffDefault,
		levels[skillIndRefining], levels[skillIndRefineryEf], 0) * refineNetFactor()
}

// evaluateIndustry prices the factory half. A lever is priced on the throughput it
// adds, as a fraction of the pilot's best ship times how much of a manufacturer it is.
// A gate is priced on the share of the blueprint list it unlocks, on the same anchor.
// No fit is resolved, so these goals never touch the evaluation budget.
func (p *skillPlanner) evaluateIndustry(g *skillGoal) {
	g.valued = true
	if p.wIndustry <= 0 {
		g.value = 0
		return
	}
	if g.Kind == goalIndGate {
		share := 1.0
		if p.indCands > 0 {
			share = float64(p.indGates[g.TypeID]) / float64(p.indCands)
		}
		g.value = p.wIndustry * p.poolRef * industryGateWorth * share
		return
	}
	after := p.sheetWith(g.chain)
	before := industryPower(p.levels, p.levers)
	if before <= 0 {
		g.value = 0
		return
	}
	gain := industryPower(after, p.levers)/before - 1
	if gain <= 0 {
		g.value = 0
		return
	}
	// The refinery levers serve two careers, the factory and the mining trade
	// (refine-and-list), so both weights apply; the pure factory levers stay the manufacturer's.
	w := p.wIndustry
	if p.isRefineLever(g.TypeID) {
		w += p.wRefine
	}
	g.value = w * p.poolRef * gain
}

// labLeverSkills are the skills that shorten a lab job: Metallurgy (material research),
// Research (time research) and Science (copying, -5% copy time per level, read by the
// server's CalcCopyTime). Each is a flat -5% on its job, which evaluateLab prices.
// The slot skills are labSlotSkills' and are priced on a different curve.
func labLeverSkills() []int64 {
	return []int64{skillMetallurgy, skillResearch, skillScience}
}

// labSlotSkills are the skills that buy a research job running beside the one already
// on a line: Laboratory Operation and its advanced pair, counted by
// serverram.MaxConcurrentJobs over activityID <> 1. Offering them needs StateData's
// LabCooking list (state.go), which lets an installed job leave the trip fields.
func labSlotSkills() []int64 {
	return []int64{serverram.SkillLaboratoryOperation, serverram.SkillAdvancedLabOperation}
}

// evaluateLab prices a lab lever on the research throughput it adds. Both skills are a
// flat -5% job time per level, so level 0 to IV means a job runs at 0.8 of its time:
// 25% more research per day, priced on the factory's pool anchor times labLeverWorth.
// No fit is resolved, so it never touches the evaluation budget.
func (p *skillPlanner) evaluateLab(g *skillGoal) {
	g.valued = true
	if p.wIndustry <= 0 {
		g.value = 0
		return
	}
	have, want := p.levels[g.TypeID], g.Level
	if want <= have {
		g.value = 0
		return
	}
	before, after := researchSkillMod(have), researchSkillMod(want)
	if after <= 0 {
		g.value = 0
		return
	}
	gain := before/after - 1
	if gain <= 0 {
		g.value = 0
		return
	}
	g.value = p.wIndustry * p.poolRef * labLeverWorth * gain
}

// evaluateLabSlot prices a parallel research slot on the throughput it adds:
// MaxConcurrentJobs is 1 + basic + advanced, so 0/0 to 1/0 doubles research and 4/0 to
// 5/0 adds a fifth. The other skill of the pair is read at its projected level, or the
// two goals would double-count the gain. No fit is resolved.
func (p *skillPlanner) evaluateLabSlot(g *skillGoal) {
	g.valued = true
	if p.wIndustry <= 0 {
		g.value = 0
		return
	}
	have, want := p.levels[g.TypeID], g.Level
	if want <= have {
		g.value = 0
		return
	}
	other := p.levels[serverram.SkillAdvancedLabOperation]
	if g.TypeID == serverram.SkillAdvancedLabOperation {
		other = p.levels[serverram.SkillLaboratoryOperation]
	}
	before := float64(serverram.MaxConcurrentJobs(have, other))
	after := float64(serverram.MaxConcurrentJobs(want, other))
	if before <= 0 || after <= before {
		g.value = 0
		return
	}
	g.value = p.wIndustry * p.poolRef * labSlotWorth * (after/before - 1)
}

// researchSkillMod is the server's own -5%-per-level research time multiplier
// (CalcMEResearchTime / CalcPEResearchTime), floored so the ratio above can
// never divide by zero however the formula is retuned.
func researchSkillMod(level int) float64 {
	mod := 1 - 0.05*float64(level)
	if mod < 0.05 {
		mod = 0.05
	}
	return mod
}

// evaluateResearch prices a research field skill against the same pool anchor the
// factory goals use. The first level carries the full worth (it is the difference
// between having a collaboration and not); later levels are priced on the RP/day the
// chain adds, a ratio of two squares that tapers, so the ranking stops at the right level.
func (p *skillPlanner) evaluateResearch(g *skillGoal) {
	g.valued = true
	if p.wIndustry <= 0 {
		g.value = 0
		return
	}
	have := p.levels[g.TypeID]
	if have == 0 {
		// Opening the collaboration: a step change, priced on the gate anchor.
		g.value = p.wIndustry * p.poolRef * researchOpenWorth
		return
	}
	full := p.wIndustry * p.poolRef * researchLevelWorth
	// The agent's own level is unknown here and irrelevant to the shape: a mid-level
	// agent is the same stand-in on both sides of the ratio.
	const refAgentLevel = 3.0
	before := refAgentLevel + float64(have)
	after := refAgentLevel + float64(g.Level)
	gain := (after*after)/(before*before) - 1
	if gain <= 0 {
		g.value = 0
		return
	}
	g.value = full * gain
}

// implantNeedWorth is what one point of implant gain (implantwants.go: roughly
// one percent on a stat the pilot lives by) is worth in ship-score units.
const implantNeedWorth = 0.01

// evaluateImplant prices a Cybernetics level as the gain of every held or
// wanted implant that level unlocks (needs between the trained level and
// this goal's), as a share of the pilot's best ship.
func (p *skillPlanner) evaluateImplant(g *skillGoal) {
	g.valued = true
	have := p.levels[g.TypeID]
	gain := 0.0
	for _, n := range p.implantNeeds {
		if n.Level > have && n.Level <= g.Level {
			gain += n.Gain
		}
	}
	if gain <= 0 {
		g.value = 0
		return
	}
	g.value = p.poolRef * implantNeedWorth * gain
}

// isRefineLever names the industry levers whose gain is reprocessing yield rather
// than factory throughput: the ones a refining miner trains too.
func (p *skillPlanner) isRefineLever(typeID int64) bool {
	return typeID == skillIndRefining || typeID == skillIndRefineryEf ||
		(typeID != 0 && typeID == p.levers.OreSkillID)
}

// sheetWith is the pilot's projected levels with one chain's promises written over
// them: the sheet every what-would-this-be-worth question is asked against.
func (p *skillPlanner) sheetWith(chain []skillStep) map[int64]int {
	sheet := make(map[int64]int, len(p.levels)+len(chain))
	for k, v := range p.levels {
		sheet[k] = v
	}
	for _, s := range chain {
		sheet[s.TypeID] = s.Level
	}
	return sheet
}

// evaluateSupport prices the skills that make the ship this pilot already flies work
// properly (skillsupport.go): one reading of the job's power function before the chain
// and one after, per job the pilot wants, as a share of what that ship is worth to it.
// The weight and the ship both come from the pilot. No evaluation budget is used.
func (p *skillPlanner) evaluateSupport(g *skillGoal) {
	g.valued = true
	after := p.sheetWith(g.chain)
	total := 0.0
	for _, ref := range p.refs {
		if ref.Weight <= 0 || ref.Score <= 0 {
			continue
		}
		before := supportPower(ref, p.skillAttrs, p.levels)
		if before <= 0 {
			continue
		}
		gain := supportPower(ref, p.skillAttrs, after)/before - 1
		if gain <= 0 {
			continue
		}
		total += ref.Weight * ref.Score * gain
	}
	g.value = total
}

// evaluateMarket prices the trade skills: how much more of the market this pilot could
// have open at once, and how much more of each sale it keeps, against the business it
// is already doing. Nothing to Trade I is the biggest single move for a trader.
func (p *skillPlanner) evaluateMarket(g *skillGoal) {
	g.valued = true
	if p.wMarket <= 0 {
		g.value = 0
		return
	}
	before := marketPower(p.levels)
	if before <= 0 {
		g.value = 0
		return
	}
	gain := marketPower(p.sheetWith(g.chain))/before - 1
	if gain <= 0 {
		g.value = 0
		return
	}
	g.value = p.wMarket * p.marketRef * gain
}

// foreseenMinutes is how much of the work still on the table would cost at a given set
// of learning attributes: the quantity a learning skill reduces. It over-counts because
// chains share prerequisites, but learning is valued on the difference between two
// readings of this number, so the over-count cancels.
func (p *skillPlanner) foreseenMinutes(goals []*skillGoal, stats map[int64]float64) float64 {
	limit := p.tn.SkillLearningHorizonMin
	total := 0.0
	for _, g := range goals {
		if g.dead || g.Kind == goalLearning {
			continue
		}
		for _, s := range g.chain {
			row, ok := p.rows[s.TypeID]
			if !ok {
				continue
			}
			rate := evedb.ESPSkillSPPerMinute(stats, row.PrimaryAttrID, row.SecondaryAttrID)
			total += stepMinutes(row.Rank, float64(row.SkillPoints), s.Level, rate)
			if total >= limit {
				return limit
			}
		}
	}
	return total
}

// learningSaved is the minutes a learning chain would give back across the
// work still foreseen.
func (p *skillPlanner) learningSaved(g *skillGoal, goals []*skillGoal) float64 {
	stats := p.stats
	learnLvl := p.levels[learningSkillTypeID]
	for _, s := range g.chain {
		lb, ok := p.learn[s.TypeID]
		if !ok {
			continue
		}
		stats = predictStats(stats, lb, learnLvl, s.Level-1, s.Level)
		if s.TypeID == learningSkillTypeID {
			learnLvl = s.Level
		}
	}
	saved := p.foreseenMinutes(goals, p.stats) - p.foreseenMinutes(goals, stats)
	if saved < 0 {
		saved = 0
	}
	return saved
}

// ── goals ───────────────────────────────────────────────────────────────────

// relevantLearningAttrs is the set of learning attributes this pilot's foreseen work
// trains on: the top learningRelevantAttrs by the minutes of work each governs (primary
// credited in full, secondary at half, as ESPSkillSPPerMinute pays). The rest are never
// goals, because fallbackChain would otherwise train every learning skill.
func (p *skillPlanner) relevantLearningAttrs(goals []*skillGoal) map[int64]bool {
	demand := map[int64]float64{}
	for _, g := range goals {
		if g.Kind == goalLearning {
			continue
		}
		chain, ok := p.chainFor(g.TypeID, g.Level)
		if !ok {
			continue
		}
		for _, s := range chain {
			row, ok := p.rows[s.TypeID]
			if !ok {
				continue
			}
			demand[row.PrimaryAttrID] += s.Minutes
			if row.SecondaryAttrID > 0 {
				demand[row.SecondaryAttrID] += s.Minutes / 2
			}
		}
	}
	type attrDemand struct {
		id   int64
		mins float64
	}
	ranked := make([]attrDemand, 0, len(demand))
	for id, mins := range demand {
		if id < attrLearnFirst || id > attrLearnLast || mins <= 0 {
			continue
		}
		ranked = append(ranked, attrDemand{id, mins})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].mins != ranked[j].mins {
			return ranked[i].mins > ranked[j].mins
		}
		return ranked[i].id < ranked[j].id
	})
	out := map[int64]bool{}
	for i := 0; i < len(ranked) && i < learningRelevantAttrs; i++ {
		out[ranked[i].id] = true
	}
	return out
}

// goals enumerates everything this pilot might train toward.
func (p *skillPlanner) goals() []*skillGoal {
	seen := map[[2]int64]bool{}
	var out []*skillGoal
	add := func(typeID int64, level int, kind, why string) *skillGoal {
		if level > 5 {
			level = 5
		}
		if level < 1 || p.ban[typeID] {
			return nil
		}
		if _, ok := p.rows[typeID]; !ok {
			return nil
		}
		if p.levels[typeID] >= level {
			return nil
		}
		key := [2]int64{typeID, int64(level)}
		if seen[key] {
			return nil
		}
		seen[key] = true
		g := &skillGoal{TypeID: typeID, Level: level, Kind: kind, Why: why}
		out = append(out, g)
		return g
	}

	// The operator's own instructions come first and are not negotiable.
	for typeID := range p.force {
		name := "this skill"
		if row, ok := p.rows[typeID]; ok {
			name = row.TypeName
		}
		add(typeID, 5, goalForced, "pinned by an operator ("+name+")")
	}
	// A Jove pilot climbs its own hull line, one level per plan, which is the size the plan
	// horizon is built for; the rung after it is the next plan's.
	if p.raceID == raceJove {
		for _, r := range joveLadder {
			if lv := p.levels[r.typeID]; lv < r.cap {
				add(r.typeID, lv+1, goalJove, joveGoalWhy)
			}
		}
	}
	// What the ships want. The gate level and the satisfied level are separate goals: the
	// gate is the cheap step that puts a module back on the ship and should win alone.
	for _, fb := range p.fits {
		for skillID, level := range fb.reqs {
			add(skillID, level, goalFit, "opens "+fb.stored.Name)
			add(skillID, p.tn.SkillSatisfiedLevel, goalDeepen, "deeper into "+fb.stored.Name)
		}
	}
	// The barge and ice road, then its levers (skills_ice.go), ahead of the support levers
	// so the Astrogeology and Mining levels on the road go to the goal that knows where they lead.
	for _, ig := range p.iceGoals() {
		if g := add(ig.typeID, ig.level, goalIce, ig.why); g != nil {
			g.leadsTo = ig.leadsTo
		}
	}
	// The mining cruiser road (skills_minecruiser.go), beside the barge
	// road: the hull a frigate miner can actually reach.
	for _, cg := range p.miningCruiserGoals() {
		if g := add(cg.typeID, cg.level, goalMineCruiser, cg.why); g != nil {
			g.leadsTo = cg.leadsTo
		}
	}
	for _, cg := range p.miningCruiserGoals() {
		if g := add(cg.typeID, cg.level, goalMineCruiser, cg.why); g != nil {
			g.leadsTo = cg.leadsTo
		}
	}
	// The lifter's skills, ahead of the hauler goals below so a racial
	// Industrial level both want is priced by the lift it adds.
	for _, lg := range p.liftGoals() {
		add(lg.typeID, lg.level, goalLift, lg.why)
	}
	if indSkillName := racialShip(p.raceID, "Industrial"); indSkillName != "" {
		if indTypeID := p.skillByName(indSkillName); indTypeID != 0 {
			nonCombat := p.tn.MiningAppetite > 0 || p.tn.TradeAppetite > 0 || p.tn.HaulAppetite > 0 ||
				(p.tn.SupplyDrive > 0 && p.tn.IndustryAppetite > 0)
			if nonCombat {
				add(indTypeID, 1, goalFit, "opens its empire's hauler")
				add(indTypeID, p.tn.SkillSatisfiedLevel, goalDeepen, "deeper into hauling")
			}
		}
	}
	// The refinery pair, outside the factory block: every pilot is offered it and the
	// ranking decides. One level at a time, since a goal for V outright is a chain overStepCap discards.
	add(skillIndRefining, p.levels[skillIndRefining]+1, goalRefine,
		"turns a hold of rock into metal somebody can buy")
	add(skillIndRefineryEf, p.levels[skillIndRefineryEf]+1, goalRefine,
		"turns a hold of rock into metal somebody can buy")
	// The operator's blunt instrument (skillsweep.go), off by default: when set it rides
	// goalForced, which is non-negotiable and already labelled (skillKindCase).
	if lv := refineSkillFloor(); lv > 0 {
		add(skillIndRefining, lv, goalForced, "the operator set a refining floor")
	}

	// Command: Leadership for a pilot that wants a fleet, the member-limit ladder for a CEO
	// holding applicants at the door (corpceo_skills.go). One level at a time.
	for _, cg := range p.commandGoals() {
		add(cg.typeID, cg.level, goalCommand, cg.why)
	}
	// A capital program's builder and pilot (corpprogram_skills.go): the next level of every
	// skill the print or the hull rests on, exempt from the step caps and committed first.
	for _, pg := range p.programGoals() {
		add(pg.typeID, pg.level, goalProgram, pg.why)
	}

	// The reprocessing ladder (skills_processing.go), ahead of the factory block so a
	// Metallurgy level both want goes to the goal that knows what it leads to.
	for _, pg := range p.processingGoals() {
		if g := add(pg.typeID, pg.level, goalProcessing, pg.why); g != nil {
			g.leadsTo = pg.leadsTo
		}
	}

	// The deep core road (skills_deepcore.go), ahead of the support levers, which also
	// want Mining V and Astrogeology V; a support goal claiming them first would be dropped.
	if id, lv, leads, ok := p.deepCoreGoal(); ok {
		if g := add(id, lv, goalDeepCore, deepCoreWhy); g != nil {
			g.leadsTo = leads
		}
	}

	// The factory. Both halves are skipped for a pilot that does not manufacture, since
	// wIndustry is 0 for the whole consumer side of the market-role trait.
	if p.wIndustry > 0 {
		// Gates: a blueprint it cannot install is the factory's version of a
		// module it cannot online, and it is worth the same kind of jump.
		for skillID, count := range p.indGates {
			if level := p.indGateLv[skillID]; level > 0 {
				add(skillID, level, goalIndGate,
					fmt.Sprintf("opens %d blueprint(s) it is locked out of", count))
			}
		}
		// Levers: the skills that change the numbers on work it can already do, at the
		// satisfied level and V, since they pay linearly; the ranking decides how far.
		for _, skillID := range p.industryLeverSkills() {
			add(skillID, p.tn.SkillSatisfiedLevel, goalIndLever, "its factory and refinery run on this")
			add(skillID, 5, goalIndLever, "its factory and refinery run on this")
		}
		// The R&D fields this pilot collaborates in, plus the one the enrolment lane earmarked
		// (research.go). One level at a time: a V goal is longer than SkillStepMaxMinutes.
		for _, skillID := range p.rndFields {
			add(skillID, 1, goalResearch, "opens an R&D collaboration")
			add(skillID, p.levels[skillID]+1, goalResearch, "raises its patent allocation rate")
		}
		// Cybernetics, to the level the implants in its hangar (or the ones it wants) demand,
		// one goal per level, each carrying the implant that asks (implantupkeep.go).
		for _, n := range p.implantNeeds {
			add(skillCybernetics, n.Level, goalImplant, n.Why)
		}
		// The lab levers (labwork.go), at the satisfied level and V like the factory's. Only for
		// a research career (tuning.go researchCareer): the lab lane starts jobs for no one else.
		if researchCareer(p.tn) {
			for _, skillID := range labLeverSkills() {
				add(skillID, p.tn.SkillSatisfiedLevel, goalLab, "its blueprint research and copying run on this")
				add(skillID, 5, goalLab, "its blueprint research and copying run on this")
			}
		}
		// The lab slots (labwork_oven.go), one level at a time: each level is worth less than
		// the last, so a V goal prices the chain at the taper's bottom rung and loses every plan.
		if researchCareer(p.tn) {
			for _, skillID := range labSlotSkills() {
				add(skillID, p.levels[skillID]+1, goalLabSlot, "runs another research or copy job beside the first")
			}
		}
	}
	// The support levers (skillsupport.go): skills that make the ship this pilot flies work
	// properly and that no fit asks for, only for jobs it has a ship and an appetite for.
	for _, ref := range p.refs {
		for _, skillID := range supportLeverSkills(ref.Job) {
			add(skillID, p.tn.SkillSatisfiedLevel, goalSupport, "its "+ref.FitName+" runs on this")
			add(skillID, 5, goalSupport, "its "+ref.FitName+" runs on this")
		}
	}
	// The order book. Zero for a pilot with no taste for commerce and no
	// factory to sell out of, so it never sees these at all.
	if p.wMarket > 0 {
		for _, skillID := range marketLeverSkills() {
			add(skillID, p.tn.SkillSatisfiedLevel, goalMarket, "more of the market open to it at once")
			add(skillID, 5, goalMarket, "more of the market open to it at once")
		}
	}
	// Learning, satisfied at the same level as everything else and only where it pays: the
	// Learning skill itself plus the attribute skills feeding relevantLearningAttrs.
	rel := p.relevantLearningAttrs(out)
	for typeID, lb := range p.learn {
		if lb.TypeID != learningSkillTypeID && !rel[lb.AttrID] {
			continue // feeds an attribute this pilot's work barely reads
		}
		add(typeID, p.tn.SkillSatisfiedLevel, goalLearning, "trains everything else faster")
	}
	return out
}

// ── the plan ────────────────────────────────────────────────────────────────

// plan is the greedy loop: price every live goal, take the best ratio, commit it, and
// let the next round see the world it left behind (cheaper prerequisites, better
// attributes, a thinner wallet).
func (p *skillPlanner) plan() []skillStep {
	goals := p.goals()
	var out []skillStep
	planned := 0.0

	for len(out) < skillMaxPlanSteps && planned < p.tn.SkillPlanHorizonMin {
		var live []*skillGoal
		for _, g := range goals {
			if g.dead {
				continue
			}
			chain, ok := p.chainFor(g.TypeID, g.Level)
			if !ok {
				g.dead = true
				continue
			}
			if len(chain) == 0 {
				g.dead = true // the plan already reached it
				continue
			}
			minutes, books, buyable := p.chainCost(chain)
			if !buyable || books > p.budget {
				continue // no seller, or the money is spoken for: try later
			}
			if g.Kind != goalJove && g.Kind != goalDeepCore && g.Kind != goalIce && g.Kind != goalMineCruiser &&
				g.Kind != goalProgram && !openingLadderGoal(g) &&
				(p.overStepCap(chain) || p.overGroupCap(g, chain)) {
				continue
			}
			if !sameChain(g.chain, chain) {
				g.valued = false
			}
			g.chain, g.minutes, g.books = chain, minutes, books
			g.touches = p.touching(chain)
			live = append(live, g)
		}
		if len(live) == 0 {
			break
		}

		// Forced goals bypass the arithmetic entirely: an operator pinning a
		// skill is not making a suggestion.
		if g := firstOfKind(live, goalForced); g != nil {
			p.commit(g, &out, goals)
			planned += g.minutes
			continue
		}
		// ...and so does a Jove pilot's own hull line, right behind them.
		if g := firstOfKind(live, goalJove); g != nil {
			p.commit(g, &out, goals)
			planned += g.minutes
			continue
		}
		// ...and a capital program's builder or pilot: the corporation has
		// billions in that hull, and no fit will ever ask for the skills.
		if g := firstOfKind(live, goalProgram); g != nil {
			p.commit(g, &out, goals)
			planned += g.minutes
			continue
		}
		// ...and a full corporation's CEO's next member-limit rung: the corporation
		// cannot grow until it lands (corpceo_skills.go).
		if p.ladderFirst() {
			if g := firstLadderGoal(live); g != nil {
				p.commit(g, &out, goals)
				planned += g.minutes
				continue
			}
		}

		// Cheapest chains are priced first, so a tight evaluation budget is
		// spent on the steps the pilot could actually have soon.
		sort.SliceStable(live, func(i, j int) bool { return live[i].minutes < live[j].minutes })
		for _, g := range live {
			if g.valued || g.Kind == goalLearning {
				continue
			}
			// The factory, support and market goals are arithmetic on numbers already in hand, so
			// they are not rationed out of the budget the fit half shares.
			if cheapGoal(g.Kind) {
				p.evaluate(g)
				continue
			}
			if p.evals >= skillEvalBudget {
				continue
			}
			p.evaluate(g)
		}
		// Learning is worth what the time it gives back would otherwise buy,
		// so it is priced against the best rate anything else pays.
		bestRate := 0.0
		for _, g := range live {
			if g.Kind == goalLearning || !g.valued || g.minutes <= 0 {
				continue
			}
			if r := g.value / g.minutes; r > bestRate {
				bestRate = r
			}
		}
		for _, g := range live {
			if g.Kind != goalLearning {
				continue
			}
			g.value, g.valued = p.learningSaved(g, goals)*bestRate, true
		}

		var best *skillGoal
		bestScore := 0.0
		for _, g := range live {
			if !g.valued || g.value <= 0 || g.minutes <= 0 {
				continue
			}
			if s := g.value / g.minutes; best == nil || s > bestScore {
				best, bestScore = g, s
			}
		}
		if best == nil {
			// Nothing is worth anything, but the pilot has a queue to fill and idleness costs it
			// the one resource it cannot earn back. See fallbackChain.
			if g := fallbackChain(live); g != nil {
				p.commit(g, &out, goals)
				planned += g.minutes
				continue
			}
			break
		}
		p.commit(best, &out, goals)
		planned += best.minutes
	}
	return out
}

// fallbackChain is what a pilot trains when the arithmetic says nothing is worth
// training. Learning is priced as an opportunity cost, so when nothing else has value it
// is worth zero too and the queue would stay empty. The floor: learning first, else the
// cheapest chain; it runs only when every other goal has scored zero.
func fallbackChain(live []*skillGoal) *skillGoal {
	var best *skillGoal
	for _, g := range live {
		if g.minutes <= 0 {
			continue
		}
		switch {
		case best == nil:
			best = g
		case best.Kind != goalLearning && g.Kind == goalLearning:
			best = g // learning outranks anything, however cheap
		case best.Kind == goalLearning && g.Kind != goalLearning:
		case g.minutes < best.minutes:
			best = g
		}
	}
	return best
}

// overStepCap reports whether any single step in the chain takes longer than
// this pilot's patience allows.
func (p *skillPlanner) overStepCap(chain []skillStep) bool {
	if p.tn.SkillStepMaxMinutes <= 0 {
		return false
	}
	for _, s := range chain {
		if s.Minutes > p.tn.SkillStepMaxMinutes {
			return true
		}
	}
	return false
}

// overGroupCap is the diversification brake: one plan may put only so many steps into
// one skill group. Learning is exempt as the clock everything runs on. Research is exempt
// too: its chain is all-or-nothing and sits in one or two groups, so a cap of three would
// refuse it outright; the step cap still bounds it.
func (p *skillPlanner) overGroupCap(g *skillGoal, chain []skillStep) bool {
	// The reprocessing ladder is exempt for the research chain's reason: Science I-IV and
	// Metallurgy are one group, five steps or more against a cap of three.
	if g.Kind == goalLearning || g.Kind == goalResearch || g.Kind == goalProcessing ||
		p.tn.SkillGroupCap <= 0 {
		return false
	}
	used := map[int64]int{}
	for _, s := range chain {
		if s.Learning {
			continue
		}
		used[s.GroupID]++
		if p.groupUse[s.GroupID]+used[s.GroupID] > p.tn.SkillGroupCap {
			return true
		}
	}
	return false
}

// commit writes a chain into the plan and moves the world on: the levels it
// promised, the attributes a learning step bought, the money a book cost,
// and the baselines every affected fit is now measured from.
func (p *skillPlanner) commit(g *skillGoal, out *[]skillStep, goals []*skillGoal) {
	for _, s := range g.chain {
		s.Why, s.Kind, s.Score = g.Why, g.Kind, g.value
		if s.Learning {
			// Read the Learning level BEFORE the step lands: a flat bonus is
			// applied inside the multiplier that level sets.
			if lb, ok := p.learn[s.TypeID]; ok {
				p.stats = predictStats(p.stats, lb, p.levels[learningSkillTypeID], s.Level-1, s.Level)
			}
		}
		if s.NeedBook {
			p.budget -= p.bookPrice(s.TypeID)
			p.injected[s.TypeID] = true
		}
		p.levels[s.TypeID] = s.Level
		p.groupUse[s.GroupID]++
		*out = append(*out, s)
	}
	g.dead = true

	// Every fit this chain touched is now worth something different, and so
	// is every goal measured against those fits.
	for i := range g.touches {
		fb := p.fits[i]
		fb.score, _ = p.m.judgeHypothetical(p.ctx, p.charID, fb.job, fb.stored, p.levels, p.raceID)
	}
	for _, other := range goals {
		if other.dead || !other.valued {
			continue
		}
		// The factory, support and market goals read one throughput number off the whole sheet,
		// so any committed step can move them; re-pricing one is free.
		if cheapGoal(other.Kind) {
			other.valued = false
			continue
		}
		for i := range other.touches {
			if g.touches[i] {
				other.valued = false
				break
			}
		}
	}
}

func firstOfKind(goals []*skillGoal, kind string) *skillGoal {
	for _, g := range goals {
		if g.Kind == kind {
			return g
		}
	}
	return nil
}

func sameChain(a, b []skillStep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].TypeID != b[i].TypeID || a[i].Level != b[i].Level {
			return false
		}
	}
	return true
}

// ── applying a plan ─────────────────────────────────────────────────────────

// applySkillPlan buys what the plan needs and writes it into the core's queue. The plan
// is walked in order and truncated at the first book the pilot cannot have: the core
// injects a skill only when its prerequisites are trained, not queued, and the next
// replan resumes there. The wallet floor is enforced here too, against the live balance.
func (m *Mod) applySkillPlan(ctx context.Context, charID int64, plan []skillStep, floor float64) ([]skillStep, error) {
	applied := make([]skillStep, 0, len(plan))
	bal, err := evedb.GetCharBalance(ctx, m.db, charID)
	if err != nil {
		return nil, fmt.Errorf("wallet: %w", err)
	}
	// The same yielding floor the budget was priced against (holdback), fixed once before
	// the first purchase so it cannot chase the balance down.
	floor = holdback(bal, floor)
	// skipped holds books refused for an untrained prerequisite: that step and the
	// same skill's later levels are left out, the rest of the plan still applies.
	skipped := map[int64]bool{}
	var skippedNames []string
	defer func() {
		if len(skippedNames) > 0 {
			m.logBot(charID, logInfo, "skills",
				"left %s out of this plan — a prerequisite is still training; the next pass buys it",
				strings.Join(skippedNames, ", "))
		}
	}()
buying:
	for _, s := range plan {
		if skipped[s.TypeID] {
			continue
		}
		if s.NeedBook {
			// Strictly above the floor, and still above it once the book is paid for: the floor
			// is money the pilot keeps.
			if bal <= floor || bal-s.Price <= floor {
				m.logBot(charID, logInfo, "skills",
					"stopping the plan at %s — %s book against %s in the wallet and a %s floor; the rest waits for a richer pass",
					s.Name, iskString(s.Price), iskString(bal), iskString(floor))
				break buying
			}
			// Solvency is not the only question: one book must not be most of everything the pilot
			// has (skillbudget.go). The same truncation applies; the next replan resumes from a bigger wallet.
			if !bookAffordable(bal, s.Price) {
				m.logBot(charID, logInfo, "skills",
					"stopping the plan at %s — %s; the rest waits for a richer pass",
					s.Name, bookDeferralNote(s.Name, s.Price, bal))
				break buying
			}
			purchase, err := evedb.PurchaseAndInjectSkill(ctx, m.db, charID, s.TypeID)
			switch {
			case err == nil:
				bal -= purchase.Price
				// The sheet just changed and the very next thing this pass
				// does is plan against it (skillcache.go).
				forgetCharSkills(charID)
				m.logBot(charID, logInfo, "skills",
					"bought %s for %s from Genolution Biotech — %s",
					s.Name, iskString(purchase.Price), s.Why)
				m.chatterNoteBot(ctx, charID, "pers.skillbook", map[string]string{"skill": s.Name}, s.TypeID)
			case errors.Is(err, evedb.ErrSkillOwned):
				// Already had it: something else injected the book between
				// the plan and here. Nothing to buy, nothing wrong.
			case bookWaitsOnPrereq(err):
				skipped[s.TypeID] = true
				skippedNames = append(skippedNames, s.Name)
				continue buying
			default:
				m.logBot(charID, logInfo, "skills",
					"stopping the plan at %s — cannot buy the book yet (%v); the rest waits for the next pass",
					s.Name, err)
				// Break, do not return: the plan up to here is real and still has to reach the queue;
				// returning here would leave the pilot with an empty queue.
				break buying
			}
		}
		applied = append(applied, s)
	}
	if len(applied) == 0 {
		return nil, nil
	}
	// The plan is a rebuild, not an addition: this pilot's queue is the planner's. Active
	// training is untouched, as it is already counted in the projected levels.
	if err := evedb.ClearSkillQueue(ctx, m.db, charID); err != nil {
		return nil, fmt.Errorf("clear queue: %w", err)
	}
	steps := make([]evedb.MinMaxPlanStep, 0, len(applied))
	for _, s := range applied {
		steps = append(steps, evedb.MinMaxPlanStep{
			TypeID: s.TypeID, TypeName: s.Name, TargetLevel: s.Level, Minutes: s.Minutes,
		})
	}
	if _, err := evedb.AppendSkillQueuePlan(ctx, m.db, charID, steps); err != nil {
		return nil, fmt.Errorf("queue plan: %w", err)
	}
	return applied, nil
}

// bookWaitsOnPrereq reports whether a refused book only waits on a prerequisite still
// training: applySkillPlan skips that step instead of truncating the plan. Pure.
func bookWaitsOnPrereq(err error) bool {
	return errors.Is(err, evedb.ErrSkillPrereq)
}

// ── the sweep ───────────────────────────────────────────────────────────────

// skillPass replans the pilots due for it, idle ones first, several at once. The batch
// follows the roster (skillPlansPerPass) and runs on a small worker pool, as the bot pass
// does (simpool.go), since the pilots are independent character rows. Throughput is
// workers / plan cost, and a pilot training nothing should not wait behind full queues.
func (m *Mod) skillPass(ctx context.Context) {
	due, err := m.duePlanners(ctx, m.skillPlansPerPass(ctx))
	if err != nil {
		rlogf("rolebots: skill pass: %v", err)
		return
	}
	if len(due) == 0 {
		return
	}
	workers := skillPlanWorkers
	if workers > len(due) {
		workers = len(due)
	}
	ids := make(chan int64)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					rlogf("rolebots: skill planner panicked: %v\n%s", r, debug.Stack())
				}
			}()
			for charID := range ids {
				// A plan is the most expensive arithmetic in the module: stop when the budget cannot fit
				// another (skillPlanHasTime), not only when it is spent, so no plan is cancelled half-made.
				if !skillPlanHasTime(ctx) {
					return
				}
				if err := m.planSkillsFor(ctx, charID); err != nil {
					// The budget running out mid-plan is this lane's business, not this pilot's, and the
					// worker behind this one has no time either.
					if ctx.Err() != nil {
						return
					}
					m.logBot(charID, logError, "skills", "skill planning failed: %v", err)
				}
			}
		}()
	}
	// Selected rather than sent blind: a worker that returned on a dead context is not
	// draining this channel, and a plain send would block forever.
feed:
	for _, charID := range due {
		select {
		case ids <- charID:
		case <-ctx.Done():
			break feed
		}
	}
	close(ids)
	wg.Wait()
}

const (
	// skillIdleReplanSec is the shortest a pilot with an empty queue waits before it may
	// rethink. Idle pilots jump the queue, so without a cool-down a pilot with nothing worth
	// training would be re-planned every pass and starve the roster.
	skillIdleReplanSec = 300
	// skillPlansPerPassMax bounds one pass however big the roster is. Sized so the batch is
	// not what binds: throughput is skillPlanWorkers / plan cost, and leftovers are simply
	// not sent and picked up by duePlanners next pass.
	skillPlansPerPassMax = 64
	// skillPlanWorkers is how many pilots may rethink at once: four against the core's
	// SetMaxOpenConns(64), beside the sim pass's eight. A plan is a run of short queries,
	// and this is background work the game must never wait on.
	skillPlanWorkers = 4

	// skillPlanHeadroom is how much of the pass budget must be in hand before a worker takes
	// on another pilot: a plan takes eight to ten seconds (skilllane.go), so one started
	// inside this window finishes nothing.
	skillPlanHeadroom = 12 * time.Second
)

// skillPlanHasTime reports whether enough of the pass budget is left to start
// another plan. A lane with no deadline at all (the harness) is always in time.
func skillPlanHasTime(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	dl, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(dl) >= skillPlanHeadroom
}

// skillPlansPerPass is how many pilots may rethink in one pass: enough that
// the whole roster comes round inside skillReplanSec, capped.
func (m *Mod) skillPlansPerPass(ctx context.Context) int {
	// The roster count moves on the scale of births, not passes: ten
	// minutes between reads, not one COUNT per pass.
	skillPlanCountMu.Lock()
	n, at := skillPlanCount, skillPlanCountAt
	skillPlanCountMu.Unlock()
	if n == 0 || clock.Now().Sub(at) >= 10*time.Minute {
		if err := m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_characters c
			LEFT JOIN rolebot_profiles pr ON pr.characterID = c.characterID
			WHERE c.enabled = 1 AND COALESCE(pr.autoSkills, 1) = 1`).Scan(&n); err != nil {
			return 1
		}
		skillPlanCountMu.Lock()
		skillPlanCount, skillPlanCountAt = n, clock.Now()
		skillPlanCountMu.Unlock()
	}
	return plansPerPass(n, m.skillPaceSec())
}

var (
	skillPlanCountMu sync.Mutex
	skillPlanCount   int
	skillPlanCountAt time.Time
)

// plansPerPass is that arithmetic on its own: how many of n pilots have to rethink each
// pass for all of them to come round inside one replan interval. paceSec is how often a
// pass actually comes round, not the tick the lane is registered at; sizing against the
// registered tick under-sizes the batch when a pass overruns its tick.
func plansPerPass(n int, paceSec float64) int {
	if paceSec <= 0 {
		paceSec = skillEveryTicks
	}
	per := int(math.Ceil(float64(n) * paceSec / skillReplanSec))
	if per < 1 {
		return 1
	}
	if per > skillPlansPerPassMax {
		return skillPlansPerPassMax
	}
	return per
}

// duePlanners picks the pilots due for a replan: enabled, automating their own skills,
// and overdue, idle (an empty queue and nothing training), or holding a plan built before
// the operator's reconsider stamp (skillsweep.go). Idle first, then longest unplanned.
// See docs/comment-notes-b17.md#due-planners.
func (m *Mod) duePlanners(ctx context.Context, limit int) ([]int64, error) {
	if limit < 1 {
		limit = 1
	}
	now := clock.Unix()
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID,
		       (MIN(t.characterID) IS NULL
		        AND NOT EXISTS (SELECT 1 FROM chrSkillQueue q WHERE q.characterID = c.characterID)
		        AND (MAX(p.plannedAt) IS NULL OR MAX(p.applied) = 1)) AS idle,
		       COALESCE(MAX(p.plannedAt), 0) AS pa
		FROM rolebot_characters c
		LEFT JOIN rolebot_profiles pr ON pr.characterID = c.characterID
		LEFT JOIN rolebot_skill_plan p ON p.characterID = c.characterID
		LEFT JOIN chrSkillTraining t ON t.characterID = c.characterID
		WHERE c.enabled = 1 AND COALESCE(pr.autoSkills, 1) = 1
		GROUP BY c.characterID
		HAVING (idle = 1 AND ? - pa >= ?) OR ? - pa >= ? OR pa < ?
		ORDER BY idle DESC, pa ASC, c.characterID ASC
		LIMIT ?`,
		now, skillIdleReplanSec, now, skillReplanSec, replanEpoch(), limit)
	if err != nil {
		return nil, fmt.Errorf("duePlanners: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var charID, idle, plannedAt int64
		if err := rows.Scan(&charID, &idle, &plannedAt); err != nil {
			return nil, fmt.Errorf("duePlanners scan: %w", err)
		}
		out = append(out, charID)
	}
	return out, rows.Err()
}

// planSkillsFor builds, applies and records one pilot's plan.
func (m *Mod) planSkillsFor(ctx context.Context, charID int64) error {
	traits, err := getTraits(ctx, m.db, charID)
	if err != nil {
		return err
	}
	p, err := m.newSkillPlanner(ctx, charID, resolveTuning(traits))
	if err != nil {
		return err
	}
	plan := p.plan()
	applied, err := m.applySkillPlan(ctx, charID, plan, p.tn.SkillWalletFloor)
	if err != nil {
		return err
	}
	if err := storeSkillPlan(ctx, m.db, charID, plan, applied); err != nil {
		return err
	}
	m.preemptForLadder(ctx, charID, applied)
	if len(applied) == 0 {
		// Nothing worth training is a real answer: an all-V pilot with a fit pool it has
		// outgrown has finished. The plannedAt stamp above keeps it from being asked for an hour.
		return nil
	}
	m.logBot(charID, logInfo, "skills", "training plan: %s", planSummary(applied))
	return nil
}

// planSummary is the log line: the first few steps and what the plan adds up
// to, in the operator's units.
func planSummary(plan []skillStep) string {
	s := ""
	shown := len(plan)
	if shown > 4 {
		shown = 4
	}
	total := 0.0
	for _, st := range plan {
		total += st.Minutes
	}
	for i := 0; i < shown; i++ {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s %s (%s — %s)",
			plan[i].Name, romanLevel(plan[i].Level), durationText(plan[i].Minutes), plan[i].Why)
	}
	if len(plan) > shown {
		s += fmt.Sprintf(" and %d more", len(plan)-shown)
	}
	return s + fmt.Sprintf("; %s of training queued", durationText(total))
}

func romanLevel(level int) string {
	switch level {
	case 1:
		return "I"
	case 2:
		return "II"
	case 3:
		return "III"
	case 4:
		return "IV"
	case 5:
		return "V"
	}
	return fmt.Sprintf("%d", level)
}

// durationText renders training minutes the way a skill window would.
func durationText(minutes float64) string {
	if minutes <= 0 {
		return "—"
	}
	switch {
	case minutes < 60:
		return fmt.Sprintf("%.0fm", math.Ceil(minutes))
	case minutes < 24*60:
		return fmt.Sprintf("%.1fh", minutes/60)
	default:
		return fmt.Sprintf("%.1fd", minutes/(24*60))
	}
}

// ── persistence ─────────────────────────────────────────────────────────────

// storeSkillPlan replaces the recorded plan, keeping steps truncated for want of a book:
// those show what a pilot is saving up for. It retries on an InnoDB deadlock: the roster
// replans in a burst, each write is a DELETE then INSERT over adjacent index ranges, and a
// deadlock victim's plan only has to be written again.
func storeSkillPlan(ctx context.Context, db *sql.DB, charID int64, plan, applied []skillStep) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = storeSkillPlanOnce(ctx, db, charID, plan, applied); err == nil ||
			!strings.Contains(err.Error(), "Deadlock") {
			return err
		}
		time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
	}
	return err
}

func storeSkillPlanOnce(ctx context.Context, db *sql.DB, charID int64, plan, applied []skillStep) error {
	appliedCount := len(applied)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storeSkillPlan begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rolebot_skill_plan WHERE characterID = ?`, charID); err != nil {
		return fmt.Errorf("storeSkillPlan clear: %w", err)
	}
	now := clock.Unix()
	for i, s := range plan {
		wasApplied := 0
		if i < appliedCount {
			wasApplied = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rolebot_skill_plan
				(characterID, position, typeID, typeName, level, minutes, score, why, kind, applied, plannedAt)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			charID, i, s.TypeID, s.Name, s.Level, s.Minutes, s.Score, s.Why, s.Kind,
			wasApplied, now); err != nil {
			return fmt.Errorf("storeSkillPlan row: %w", err)
		}
	}
	if len(plan) == 0 {
		// A pilot with nothing to train still has to be stamped, or it is
		// replanned on every single sim pass forever.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rolebot_skill_plan
				(characterID, position, typeID, typeName, level, minutes, score, why, kind, applied, plannedAt)
			VALUES (?, 0, 0, '', 0, 0, 0, 'nothing worth training right now', '', 0, ?)`,
			charID, now); err != nil {
			return fmt.Errorf("storeSkillPlan stamp: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storeSkillPlan commit: %w", err)
	}
	return nil
}

// loadSkillPlan reads the recorded plan back for the panel.
func loadSkillPlan(ctx context.Context, db *sql.DB, charID int64) ([]skillStep, int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT typeID, typeName, level, minutes, score, why, kind, applied, plannedAt
		FROM rolebot_skill_plan WHERE characterID = ? ORDER BY position`, charID)
	if err != nil {
		return nil, 0, fmt.Errorf("loadSkillPlan: %w", err)
	}
	defer rows.Close()
	var out []skillStep
	var plannedAt int64
	for rows.Next() {
		var s skillStep
		var applied int
		var at int64
		if err := rows.Scan(&s.TypeID, &s.Name, &s.Level, &s.Minutes, &s.Score,
			&s.Why, &s.Kind, &applied, &at); err != nil {
			return nil, 0, fmt.Errorf("loadSkillPlan scan: %w", err)
		}
		plannedAt = at
		if s.TypeID == 0 {
			continue // the empty-plan stamp
		}
		s.NeedBook = applied == 0
		out = append(out, s)
	}
	return out, plannedAt, rows.Err()
}

// ── operator overrides ──────────────────────────────────────────────────────

// skillPrefs reads the force/ban lists for one pilot.
func skillPrefs(ctx context.Context, db *sql.DB, charID int64) (force, ban map[int64]bool, err error) {
	force, ban = map[int64]bool{}, map[int64]bool{}
	rows, err := db.QueryContext(ctx,
		`SELECT typeID, mode FROM rolebot_skill_prefs WHERE characterID = ?`, charID)
	if err != nil {
		return nil, nil, fmt.Errorf("skillPrefs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID int64
		var mode string
		if err := rows.Scan(&typeID, &mode); err != nil {
			return nil, nil, fmt.Errorf("skillPrefs scan: %w", err)
		}
		if mode == "ban" {
			ban[typeID] = true
		} else {
			force[typeID] = true
		}
	}
	return force, ban, rows.Err()
}

// setSkillPref writes one override; mode "" removes it.
func setSkillPref(ctx context.Context, db *sql.DB, charID, typeID int64, mode string) error {
	if mode == "" {
		_, err := db.ExecContext(ctx,
			`DELETE FROM rolebot_skill_prefs WHERE characterID = ? AND typeID = ?`, charID, typeID)
		if err != nil {
			return fmt.Errorf("setSkillPref clear: %w", err)
		}
		return nil
	}
	if mode != "force" && mode != "ban" {
		return fmt.Errorf("unknown skill preference %q", mode)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_skill_prefs (characterID, typeID, mode, addedAt) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE mode = VALUES(mode), addedAt = VALUES(addedAt)`,
		charID, typeID, mode, clock.Unix())
	if err != nil {
		return fmt.Errorf("setSkillPref: %w", err)
	}
	return nil
}

// setAutoSkills flips one pilot's automation flag. It is not part of saveProfile: the
// Global Flags form does not carry this field, and a form that does not carry a field
// must never be able to clear it.
func setAutoSkills(ctx context.Context, db *sql.DB, charID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_profiles (characterID, autoSkills) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE autoSkills = VALUES(autoSkills)`, charID, v)
	if err != nil {
		return fmt.Errorf("setAutoSkills: %w", err)
	}
	return nil
}
