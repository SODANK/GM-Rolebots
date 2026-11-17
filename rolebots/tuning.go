package rolebots

import "math"

// Personality.
//
// Traits are the operator's handle on the numbers that decide how a bot behaves. Each trait is a
// -100..+100 scalar wired to specific fields below, and resolves to a Tuning once per bot per sim pass.
// With no traits set, resolveTuning must return exactly defaultTuning().
//
// A trait must never require editing miningPlan and tradePlan and gangPlan: the dial belongs in this
// file and the shared scoring functions in scoring.go read it. The tolerance axis also reaches the
// corporation, home, market and stance seams (tolerance.go lists them).

// Trait keys. Stored per character in rolebot_traits, one row each.
const (
	TraitCaution    = "caution"
	TraitGreed      = "greed"
	TraitWanderlust = "wanderlust"
	TraitPatience   = "patience"
	TraitAggression = "aggression"
	TraitLoyalty    = "loyalty"
	TraitLawfulness = "lawfulness"
	TraitSupply     = "supply"
	TraitLeadership = "leadership"
	TraitTolerance  = "tolerance"
	TraitToil       = "toil"
	TraitGrace      = "grace"
)

// industryMarginDefault is the cost-plus multiplier an even-tempered
// supplier puts on manufactured goods; envy bends it (1.10 generous,
// 3.0 greedy).
const industryMarginDefault = 1.5

// lootMarkupDefault is what an even-tempered hunter asks for salvage against the going rate for the
// type; envy bends it (1.0 generous, the going rate, to 2.5 greedy).
const lootMarkupDefault = 1.25

// supplyDriveBase is the manufacture disposition of a pilot with no opinion. It sits below the other
// appetites' 1.0 so an indifferent pilot mines or hauls more often than it builds, and still builds.
// supplyDriveFloor is the consumer pole's value and stays above zero so every pilot can run a line.
const (
	supplyDriveBase  = 0.6
	supplyDriveFloor = 0.15
)

// The leadership axis, as three points each (resolveTuning). The follower end
// of CommandDrive is not zero: a follower handed a seat still takes it, it
// simply does not compete for one.
const (
	commandDriveFollower = 0.4
	commandDriveLeader   = 2.0
	obedienceFollower    = 1.5
	obedienceLeader      = 0.6
	// foundPropensityFrom is where on the trait (as -1..+1) a pilot starts to
	// think about a corporation of its own.
	foundPropensityFrom = 0.5
)

// The tolerance tiers, as a pole's strength 0..1 (Tuning.Xenophobia / Xenophilia), read in tolerance.go.
// Xenophobe stages: (0, 0.25) prefers own race and its empire's allies; [0.25, 0.5) only own race and
// allies; [0.5, 0.8) only own race; [0.8, 1] only own race and, among people, only own bloodline.
const (
	xenoWary    = 0.25 // phile: curious
	xenoStrong  = 0.5  // phile: seeks foreigners out
	xenoExtreme = 0.75 // phile: devoted

	xenoPhobeAllied    = 0.25 // own race and allies only
	xenoPhobeOwnRace   = 0.5  // own race only
	xenoPhobeBloodline = 0.8  // own bloodline among people
)

// The grace tiers, as a pole's strength 0..1 (Tuning.Vindictiveness / Forgiveness), read in grace.go.
const (
	graceGrudging   = 0.25 // vindictive: sore below, grudging from here
	graceVengeful   = 0.5  // bounties, hunts, a word to the CEO
	graceImplacable = 0.8  // never decays below the feud band

	graceClement     = 0.33 // forgiving: lenient below
	graceMagnanimous = 0.66 // pays a ransom, reads any fight as fair
)

// pacifistAggressionFloor is where on the aggression axis (as -1..+1) a pilot
// stops being willing to hold a gun in a fleet: the -40 the "will not shoot"
// archetypes are generated at (genplan.go).
const pacifistAggressionFloor = -0.4

// mineralMarkupDefault is what an even-tempered prospector asks for refined minerals over the going
// local rate; envy bends it (1.0 generous to 1.9 grasping). Narrower than the loot markup because
// minerals are fungible and a buyer compares stations.
const mineralMarkupDefault = 1.15

// TraitDef describes one trait for the DB, the ESP form and the docs.
// Polarity: the peaceful end of every axis is Low (-100, left on the panel) and the hostile end is High
// (+100, right). resolveTuning follows the same convention.
type TraitDef struct {
	Key   string
	Label string
	// Low and High name the two ends; What is the one-line description of the dial. LowDesc and
	// HighDesc say how each extreme flies and are shown under their own end of the slider.
	Low      string
	High     string
	What     string
	LowDesc  string
	HighDesc string
	// Flip changes only how the panel draws this axis: the High pole goes on the left. The stored value
	// and the polarity convention are unchanged. Market Role and Leadership use it so the supplier and the leader read on the left.
	Flip bool
}

// traitCatalog is the single source of truth for what traits exist.
var traitCatalog = []TraitDef{
	{
		Key: TraitCaution, Label: "Caution",
		Low: "careful", High: "careless",
		What:     "Nerve: where it is willing to be, and how much of a fight it will take. A combat trait — it never picks between peaceful jobs.",
		LowDesc:  "Will not leave the police umbrella, and may not go under 0.5 at all; breaks off at the first serious hit; rarely picks a fight.",
		HighDesc: "Deep null is fine even if it otherwise respects the law; fights into structure; earns in the belts with guns out.",
	},
	{
		Key: TraitGreed, Label: "Envy",
		Low: "generous", High: "greedy",
		What:     "What money is worth in distance and risk, the markup it puts on everything it sells — and whether it hauls for a fee or trades on its own account.",
		LowDesc:  "Hauls other people's cargo for the fee; sells what it builds near cost; keeps a deep cash cushion.",
		HighDesc: "Trades on its own capital and crosses regions for a margin; prices steeply; refuses work that is not worth its time.",
	},
	{
		Key: TraitWanderlust, Label: "Wanderlust",
		Low: "settled", High: "nomad",
		What:     "Whether it keeps a home pocket of space or lives on the move.",
		LowDesc:  "Works the same systems around its home station over and over; runs its factory lines and its trade book there.",
		HighDesc: "Refuses to repeat itself; little patience for a factory; a different belt every trip, freight on the road, merc work, or a new base somewhere else.",
	},
	{
		Key: TraitPatience, Label: "Patience",
		Low: "unhurried", High: "hurried",
		What:     "How long it lingers, and how far ahead it plans. It does not decide what work it does.",
		LowDesc:  "Loiters in hangars and on gates; sees plans through; accumulates big lots before it lists.",
		HighDesc: "Moves on quickly; abandons a plan that drags; lists as soon as it has a lot and takes the bid in front of it.",
	},
	{
		Key: TraitAggression, Label: "Aggression",
		Low: "pacifist", High: "violent",
		What:     "Combat work versus everything else — the main axis of its days.",
		LowDesc:  "Will work peacefully rather than shoot (Toil, Market Role and Envy decide which work); warps off rather than engage.",
		HighDesc: "Murder, player or rat variety: lives in the belts with guns out and finds trouble in unwatched space. Piracy needs this AND lawlessness — a violent pilot that respects the law hunts rats, never pilots.",
	},
	{
		Key: TraitLoyalty, Label: "Loyalty",
		Low: "devoted", High: "mercenary",
		What:     "Whether its own space and its grudges outrank the best offer going.",
		LowDesc:  "Works its home region above all else; honours dislikes at real inconvenience; hunts despised factions across regions.",
		HighDesc: "Ends up wherever the work is, doing it for whoever is paying; holds no grudges.",
	},
	{
		Key: TraitLawfulness, Label: "Lawfulness",
		Low: "lawful", High: "outlaw",
		What:     "Respect for the law — the second check on combat: shady operator versus outright menace.",
		LowDesc:  "Keeps to policed space even when a plan would pay elsewhere; earns quietly. Will not commit piracy at any level of aggression.",
		HighDesc: "Sinks below the security floor; hunts where CONCORD doesn't look; attracts trouble. Combined with aggression, robs and murders other pilots for a living — belt ambushes, gate camps and ransom demands.",
	},
	{
		Key: TraitSupply, Label: "Market Role",
		Low: "consumer", High: "supplier",
		What:     "Whether this pilot makes things or buys them — how much of a manufacturer it is.",
		LowDesc:  "Never runs a factory; buys what it needs and earns it with guns.",
		HighDesc: "Reads unmet demand, buys blueprints, refines, manufactures and lists the product; mines its own inputs when it has to.",
		Flip:     true,
	},
	{
		Key: TraitToil, Label: "Toil",
		Low: "blue collar", High: "white collar",
		What:     "Hands-on work or desk work — which peaceful career it is cut out for.",
		LowDesc:  "Works with its hands: a miner first, a freight hauler second. Lives on the rocks for days.",
		HighDesc: "Works at a desk: research a little past the middle, then the trade book. Manufacturing sits in the middle.",
	},
	{
		Key: TraitLeadership, Label: "Leadership",
		Low: "follower", High: "leader",
		What:     "Whether it takes charge of other pilots or takes its orders from them — fleets, corporations, and the skills that command both.",
		LowDesc:  "Happy in the ranks: follows its corporation's directive closely and rarely trains to command anybody.",
		HighDesc: "Wants the chair: pushes for fleet command, trains Leadership and corporation management, and is the material a CEO is made of.",
		Flip:     true,
	},
	{
		Key: TraitTolerance, Label: "Tolerance",
		Low: "xenophile", High: "xenophobe",
		What:     "What race means to it — where it lives and trades, which corporations it joins, who it hires, and who it counts as an enemy.",
		LowDesc:  "Seeks out other peoples: foreign corporations, foreign stations, and Ethnic Relations early so it can hire them.",
		HighDesc: "Keeps to its own, in four stages: prefers its race and its allies (0-25), only its race and allies (25-50), only its race (50-80), only its bloodline among people (80-100). Flies and builds its own race's ships.",
	},
	{
		Key: TraitGrace, Label: "Grace",
		Low: "forgiving", High: "vindictive",
		What:     "How long it holds a grievance against a named pilot and what it pays to settle it; whether a fair fight earns respect.",
		LowDesc:  "Lets it go: pays a ransom, says gf, forgets a kill in days. Reads even a lost fight as a good one.",
		HighDesc: "Keeps score, in four stages: sore (0-25), grudging (25-50), vengeful (50-80: bounties, hunts, a word to the CEO), implacable (80-100: never forgets). Reads even a fair fight as an insult.",
	},
}

// Tuning is the resolved per-bot behaviour, produced from traits once per sim pass.
// It is passed by value to the embodied tier, which reads it from its own goroutines; never share a
// pointer to one.
type Tuning struct {
	// Combat. The abstract and embodied tiers mirror each other so an unwatched bot breaks off at the
	// same point the embodied one would.
	FleeHullFracArmed   float64 // combat_embodied: armed, structure fraction
	FleeTankFracUnarmed float64 // combat_embodied: unarmed, total tank fraction
	BreakOffFrac        float64 // combat_abstract mirror of FleeHullFracArmed
	UnarmedBreakOffFrac float64 // combat_abstract mirror of FleeTankFracUnarmed

	// EngageMargin decides whether to start a fight: the line the pilot's nerve draws through threat.go's
	// margin. Read by the bounty site search, the miner's belt costing and the pre-fight look.
	EngageMargin float64

	// How much trouble unwatched space produces.
	EncounterChance       float64
	PatrolEncounterChance float64

	// NeutralAggression is the odds this pilot picks a fight with a stranger (neither liked nor disliked)
	// out past the police (roe.go). It is multiplied by the employer's doctrine (corpdoctrine.go), which is
	// zero for a navy and an industrial defence wing, so those pilots never open on neutral traffic.
	NeutralAggression float64

	// Belt scoring, denominated in jumps: what a preference is worth flying to avoid.
	OreJumpWorth float64 // one ore tier better is worth this many jumps
	HostileCost  float64 // a belt that could spawn rats
	BittenCost   float64 // a system where a rolebot was actually mauled
	// EnemyCost is a system a corporation this one counts as an enemy is working or keeps an address in
	// (corprel.go). It must not go negative: beltSearchFloor models only RecentCost, so every other term must
	// stay a penalty. To draw a combatant toward enemy space use rivalSystems (combattactic.go).
	EnemyCost  float64
	RecentCost float64 // a system this bot just worked
	// PreferredBonus is the head start the pilot's own region gets, in jumps. The region is derived from
	// the home station (residency.go, homeRegionSet).
	PreferredBonus float64

	// PreferredEdge is the same preference as a fraction of a price: what a preferred region shaves off a
	// buy and adds to a sale (reach.go). Loyalty owns it.
	PreferredEdge float64

	// Distance pricing for market decisions: what one jump costs as a
	// fraction of the price being weighed.
	IskJumpSlope float64

	// Memory depth and anti-monotony.
	RecentSystemMemory int
	TradeMaxStreak     int

	// TradeMinMarginFrac is the spread the pilot asks for; TradeFloorMarginFrac is the thinner one it settles
	// for when the market pays nobody that much. A grasping pilot's floor equals its ask, so it finds other
	// work instead.
	TradeMinMarginFrac   float64
	TradeFloorMarginFrac float64

	// LootMarkup is what a pilot asks for gear it did not pay for: the going rate for the type times this.
	// Loot has no cost basis, so the cost-plus spread above cannot price it.
	LootMarkup float64

	// Money nerve: how many times over a fit must be affordable before
	// the bot buys it, and the higher bar for abandoning a working ship.
	FitAffordMultiple     float64
	UpgradeAffordMultiple float64

	// The share of its own wallet this pilot will hand a gangmate that
	// cannot afford the gang's job (broke.go). Envy owns it.
	GangShareFrac float64

	// Timing. Multiplies every dwell the sim invents.
	DwellMul float64

	// HonourSoftStationPass attempts the soft station pass (prefer not to deal with disliked factions)
	// before the hard rule that only excludes the despised. A mercenary skips it.
	HonourSoftStationPass bool

	// Appetites: what kind of work pulls this pilot (appetite.go). ~1.0 is indifferent; the biggest product
	// of appetite, opportunity and satiation usually wins the draw. Each peaceful career has one owner trait.
	// See docs/comment-notes-b19.md#career-owners.
	ViolenceAppetite float64
	MiningAppetite   float64
	IndustryAppetite float64
	TradeAppetite    float64
	HaulAppetite     float64
	// ResearchFactor multiplies the manufacturing disposition into the
	// research one (research.go, the research tag). Toil alone moves it.
	ResearchFactor float64
	// ResearchPatience is the research career's reading of Patience: an unhurried pilot sits through a lab
	// job that runs for days. It multiplies only the research score, never ResearchFactor, so Patience cannot
	// move a pick between careers (TestCombatTraitsNeverPickPeacefulWork).
	ResearchPatience float64
	// ResearchAppetite is the research career: manufacturing's disposition times Toil's laboratory curve times
	// the patience to wait on a job. One expression serves the lab lane, the R&D-agent lane, the research
	// corporation tag and the R&D arm. Resolved last in resolveTuning.
	ResearchAppetite float64

	// Career strength, 0..1: how far out the pilot sits on the trait that owns each career. It stretches that
	// career's satiation (appetite.go careerHalfLife). Manufacturing's equivalent is SupplyDrive.
	CareerMining float64
	CareerHaul   float64
	CareerTrade  float64
	CareerCombat float64
	// CareerResearch is ResearchAppetite as a 0..1 strength (full at
	// researchCareerFull), for the same satiation stretch the others get.
	CareerResearch float64
	// PlanLean is how long a pilot's horizon is, 0 (hurried nomad) to 1 (unhurried and settled), 0.5 at zero
	// traits. It picks the sub-trip inside the industry umbrella (industrytrip.go): long trips (build, research)
	// against short ones (rock, ice). It never moves a pick between careers.
	PlanLean float64

	// Combat style and the specialist seats (fitstyle.go, skillscore.go, gangform.go); all are 0 at zero traits.
	// KiteLean and BrawlLean lean toward range or close fighting (caution 60%, aggression 40%); styleFor reads them.
	// LogiDrive pulls toward the logistics seat, only for a pilot not violent. EwarDrive: careful pilots and leaders.
	KiteLean  float64
	BrawlLean float64
	LogiDrive float64
	EwarDrive float64
	// ScoutDrive is the pull toward the gank fleet's scout seat (JobScout), the greedy pole: the scout scans
	// the hold, calls the price and flies home with what the wrecks left.
	ScoutDrive float64
	// TackleDrive is the reckless pole alone: the pilot who wants to be the
	// first ship on the target.
	TackleDrive float64
	// Pacifist marks a pilot at or past pacifistAggressionFloor on the
	// aggression axis: it may join a combat fleet in the logistics seat and
	// in no other (gangform.go pacifistMaySit, gangplan.go's muster gate).
	Pacifist bool
	// FightsPolice marks a reckless pilot (Caution at or past fightsPoliceFloor toward careless): it
	// shoots back at CONCORD, customs or a sentry gun that engages it. A cautious one breaks off and leaves.
	FightsPolice bool

	// Piracy (piracy.go). PiracyAppetite is the product of the aggression and outlaw factors, and either at
	// zero zeroes it. The outlaw factor is zero on the lawful half of the axis and at neutral, so a lawful
	// pilot hunts rats however violent it is.
	PiracyAppetite float64
	// PiracySecCeil is the highest security this pilot will commit a crime in. CONCORD arrives in 12 s at 0.6,
	// 16 s at 0.5 and not at all below 0.45.
	PiracySecCeil float64
	// ConcordNerveSec is the margin in seconds the pilot insists on between the prey's death and the police
	// arriving. A careless outlaw shaves it; a careful one shoots in high-sec only well inside the clock.
	ConcordNerveSec float64
	// RansomChance is the odds it offers terms rather than simply killing; RansomMul is what it asks against
	// the module's base demand. Envy owns both. PodKillChance is whether the capsule dies too.
	RansomChance  float64
	RansomMul     float64
	PodKillChance float64
	// GateCampChance is the odds a hunting trip sits on a gate instead of
	// GateCampChance is the odds a hunting trip sits on a gate instead of touring belts. Patience owns it.
	GateCampChance float64

	// Market role (the supply trait). SupplyDrive is the manufacture disposition and the only route the trait
	// has to the factory; it never reaches 0. IndustryMargin is the cost-plus multiplier on goods it builds (envy).
	// TradeSupplyBias scales the pilot's interest in commerce (tradeorders.go, tradeInterest).
	SupplyDrive     float64
	IndustryMargin  float64
	TradeSupplyBias float64
	// MineralMarkup is what a prospector asks for the minerals it refined from its own ore (minerals.go).
	// Envy owns it.
	MineralMarkup float64
	// MineralBatchMul scales how big a lot of refined minerals must be before it earns an order slot
	// (mineralbatch.go). Patience owns it: it is how long the pilot accumulates before listing.
	MineralBatchMul float64
	// MiningCashFloor is where a money-miner's wallet counts as hurting: below it a full hold goes to the best
	// bid for cash; above it a pilot whose refining skills pay waits and lists (resolveMiningDisposal, mining.go).
	// Patience owns it, like the batch gate.
	MiningCashFloor float64

	// GrudgeJumpWorth prices standings into target choices: jumps of
	// extra flying one point of negative standing toward whoever spawns
	// there is worth.
	GrudgeJumpWorth float64

	// SatiationHalfLifeSec is how fast one kind of work wears thin (the anti-monotony clock).
	// PickTemperature is the draw's noise: how often the out-of-character option wins.
	SatiationHalfLifeSec float64
	PickTemperature      float64

	// The security band a hunter works (bounty hunting).
	HuntSecFloor float64
	HuntSecCeil  float64

	// Skill investment (skills.go). SkillSatisfiedLevel is where a wanted skill stops being urgent; SkillPlanHorizonMin
	// is the training per plan, SkillStepMaxMinutes caps one step and SkillGroupCap the steps per skill group. SkillBookCushion
	// multiplies the book-buying reserve; only wallet above SkillWalletFloor is book money, capped at skillHoldbackMaxShare.
	SkillSatisfiedLevel     int
	SkillPlanHorizonMin     float64
	SkillStepMaxMinutes     float64
	SkillGroupCap           int
	SkillLearningHorizonMin float64
	SkillBookCushion        float64
	SkillWalletFloor        float64

	// PersonalSecFloor is the security line the pilot draws for itself when the profile's opt-in space limit is off;
	// rulesForBot uses it instead of the profile secFloor. Lawfulness is the main pull and caution is added. Clamped
	// to [-1, 0.8]. Any crime system must read the lawfulness trait rather than add its own switch.
	PersonalSecFloor float64

	// Command (the leadership trait). CommandDrive is how much a pilot wants to be in charge: it multiplies the
	// career tie-break for fleet seats (corprank.go fcScore), Leadership book picks and command skill value.
	// Obedience is how closely it follows its corporation's directive (corpceo_directive.go); FoundPropensity is the pull to found one.
	CommandDrive    float64
	Obedience       float64
	FoundPropensity float64

	// Race (the tolerance trait). One of the two is always zero: Xenophobia is the xenophobe pole's strength and
	// Xenophilia the xenophile's, each 0..1, read against the xeno* tiers (tolerance.go).
	Xenophobia float64
	Xenophilia float64

	// Grace (the grace trait). One of the two is always zero: Vindictiveness is the vindictive pole's
	// strength, Forgiveness the forgiving pole's, each 0..1, read against the grace* tiers (grace.go).
	Vindictiveness float64
	Forgiveness    float64
}

// defaultTuning is the Tuning of a pilot with no traits set. Its values are the module constants, named
// here so the two cannot drift apart.
func defaultTuning() Tuning {
	return Tuning{
		FleeHullFracArmed:   combatFleeHullFracArmed,
		FleeTankFracUnarmed: combatFleeTankFracUnarmed,
		BreakOffFrac:        abstractBreakOffFrac,
		UnarmedBreakOffFrac: abstractUnarmedBreakOffFrac,

		EngageMargin: defaultEngageMargin,

		EncounterChance:       abstractEncounterChance,
		PatrolEncounterChance: abstractPatrolEncounterChance,
		NeutralAggression:     neutralAggressionDefault,

		OreJumpWorth:   beltOreJumpWorth,
		HostileCost:    beltHostileCost,
		BittenCost:     beltEncounterCost,
		EnemyCost:      relEnemyCostDefault,
		RecentCost:     beltRecentCost,
		PreferredBonus: beltPreferredBonus,
		PreferredEdge:  preferredRegionEdge,

		IskJumpSlope: jumpPriceCost,

		RecentSystemMemory: recentSystemMemory,
		TradeMaxStreak:     tradeMaxStreak,

		TradeMinMarginFrac:   tradeMinMarginFrac,
		TradeFloorMarginFrac: tradeFloorMarginFrac,
		LootMarkup:           lootMarkupDefault,

		FitAffordMultiple:     fitAffordMultiple,
		UpgradeAffordMultiple: upgradeAffordMultiple,
		GangShareFrac:         gangShareFrac,

		DwellMul: 1.0,

		HonourSoftStationPass: true,

		ViolenceAppetite: 1.0,
		MiningAppetite:   toilMining(0),
		IndustryAppetite: toilIndustry(0),
		TradeAppetite:    toilTrade(0),
		HaulAppetite:     toilHaul(0),
		ResearchFactor:   toilResearch(0),
		ResearchPatience: 1.0,
		// Same product resolveTuning ends on at zero traits, so a caller holding defaultTuning() reads the
		// number a resolved untraited pilot does.
		ResearchAppetite: defaultResearchAppetite(),
		CareerResearch:   clamp01(defaultResearchAppetite() / researchCareerFull),
		PlanLean:         0.5,

		// Zero piracy appetite at zero traits (resolveTuning). The rest are read only by a pilot the appetite
		// let through, and hold the temperate answer.
		PiracyAppetite:  0,
		PiracySecCeil:   piracySecCeilDefault,
		ConcordNerveSec: concordNerveSecDefault,
		RansomChance:    ransomChanceDefault,
		RansomMul:       1.0,
		PodKillChance:   podKillBase(),
		GateCampChance:  gateCampChanceDefault,

		// An unconfigured pilot has no opinion about manufacturing: see supplyDriveBase.
		SupplyDrive:     supplyDriveBase,
		IndustryMargin:  industryMarginDefault,
		TradeSupplyBias: 1.0,
		MineralMarkup:   mineralMarkupDefault,
		MineralBatchMul: mineralBatchMulDefault,
		MiningCashFloor: miningCashFloorDefault,

		GrudgeJumpWorth: grudgeJumpWorth,

		SatiationHalfLifeSec: satiationHalfLifeSec,
		PickTemperature:      pickTemperature,

		// Default bounty security band.
		HuntSecFloor: 0.1,
		HuntSecCeil:  0.8,

		SkillSatisfiedLevel: skillSatisfiedLevelDefault,
		// The one skill dial an operator can move for the whole roster (skillsweep.go); the default is
		// skillPlanHorizonMinDefault.
		SkillPlanHorizonMin:     skillPlanHorizonMin(),
		SkillStepMaxMinutes:     skillStepMaxMinutesDefault,
		SkillGroupCap:           skillGroupCapDefault,
		SkillLearningHorizonMin: skillLearningHorizonMinDefault,
		SkillBookCushion:        skillBookCushionDefault,
		SkillWalletFloor:        skillWalletFloorDefault,

		// An even temperament keeps to highsec: 0.45, a hair under the profile default of 0.5.
		PersonalSecFloor: 0.45,

		// The even temperament holds exactly 1 on both command axes.
		CommandDrive:    1.0,
		Obedience:       1.0,
		FoundPropensity: 0,
	}
}

// traitScale converts a stored -100..+100 trait to -1..+1.
func traitScale(v int) float64 {
	f := float64(v) / 100
	if f < -1 {
		f = -1
	}
	if f > 1 {
		f = 1
	}
	return f
}

// fightsPoliceFloor is how far toward reckless a pilot's Caution must sit before it shoots back at
// the law instead of leaving. Cautious and middling pilots leave.
const fightsPoliceFloor = 0.5

// fightsPolice is Tuning.FightsPolice from the Caution scale (-1 cautious .. +1 reckless).
func fightsPolice(caution float64) bool {
	return caution >= fightsPoliceFloor
}

// bend interpolates a tuning value from a trait: atZero at t = 0 (a no-op by construction), atLow at
// t = -1 and atHigh at t = +1.
func bend(t float64, atLow, atZero, atHigh float64) float64 {
	if t < 0 {
		return atZero + (atZero-atLow)*t // t is negative: walks toward atLow
	}
	return atZero + (atHigh-atZero)*t
}

func bendInt(t float64, atLow, atZero, atHigh int) int {
	v := bend(t, float64(atLow), float64(atZero), float64(atHigh))
	if v < 1 {
		return 1
	}
	return int(v + 0.5)
}

// ── the Toil curves ─────────────────────────────────────────────────────────
//
// t is the trait as -1..+1: -1 the blue-collar pole, +1 the white-collar pole.
//
//	-1      mining 2.3, freight 1.5      the rocks, then the road
//	 0      manufacturing peaks (1.35)   the factory floor
//	+0.35   research peaks (x1.4)        the laboratory
//	+1      trading 1.8                  the trade book
//
// Envy then splits freight from trade; Market Role sets how much of a builder the middle is.

func toilMining(t float64) float64 {
	return 1 + 1.3*posPole(-t) - 0.6*posPole(t)
}

func toilHaul(t float64) float64 {
	return 1 + 0.5*posPole(-t) - 0.3*posPole(t)
}

func toilTrade(t float64) float64 {
	return 1 - 0.4*posPole(-t) + 0.8*posPole(t)
}

func toilIndustry(t float64) float64 {
	return 1.35 - 0.6*math.Abs(t)
}

// toilResearch is the research multiplier over manufacturing: researchFloor away from the laboratory,
// researchFloor+researchGain at t = researchToilPeak. The low floor means only a pilot near the
// laboratory end of Toil clears the research bar.
func toilResearch(t float64) float64 {
	return researchFloor + researchGain*math.Max(0, 1-math.Abs(t-researchToilPeak)/researchToilWidth)
}

const (
	researchToilPeak  = 0.35
	researchToilWidth = 0.45
	researchFloor     = 0.2
	researchGain      = 1.4

	// researchPatienceHurried / researchPatienceUnhurried are the research
	// career's multiplier at each pole of Patience (bend: low end is unhurried).
	researchPatienceUnhurried = 1.4
	researchPatienceHurried   = 0.5

	// researchCareerMin is the ResearchAppetite at which research is a living rather than a hobby.
	// researchCareerFull is the appetite at which CareerResearch saturates at 1.
	researchCareerMin  = 1.0
	researchCareerFull = 2.0
)

// defaultResearchAppetite is the research appetite of a pilot with no traits.
func defaultResearchAppetite() float64 {
	return supplyDriveBase * toilIndustry(0) * toilResearch(0)
}

// researchCareer reports whether research is this pilot's calling. It gates every new speculative lab job
// and every R&D-agent enrolment; work already in flight is never gated on it.
func researchCareer(tn Tuning) bool {
	return tn.ResearchAppetite >= researchCareerMin
}

// resolveTuning turns a character's traits into the numbers the sim uses.
// Missing traits are 0, so a bot nobody has configured gets defaultTuning.
func resolveTuning(traits map[string]int) Tuning {
	tn := defaultTuning()
	if len(traits) == 0 {
		return tn
	}

	if t := traitScale(traits[TraitCaution]); t != 0 {
		// Careful breaks off at the first serious hit; reckless fights into the wreck. The abstract pair
		// tracks the embodied pair so an unwatched fight resolves with the same nerve.
		tn.FleeHullFracArmed = bend(t, 0.70, combatFleeHullFracArmed, 0.10)
		tn.BreakOffFrac = bend(t, 0.70, abstractBreakOffFrac, 0.10)
		tn.FleeTankFracUnarmed = bend(t, 0.95, combatFleeTankFracUnarmed, 0.50)
		tn.UnarmedBreakOffFrac = bend(t, 0.97, abstractUnarmedBreakOffFrac, 0.55)
		// A reckless pilot takes an even fight; a careful one wants to outlast the belt 2.5 times over.
		// Assigned: caution owns the base and aggression multiplies it, so the order does not matter.
		tn.EngageMargin = bend(t, 2.5, defaultEngageMargin, 0.8)
		// What a quiet belt is worth flying for.
		tn.HostileCost = bend(t, 60, beltHostileCost, 5)
		tn.BittenCost = bend(t, 35, beltEncounterCost, 2)
		// A rival corporation working the system bends the same way, relative to the operator's dial so that
		// setting enemyCost to 0 (off) means off at every trait value.
		if base := relEnemyCost(); base > 0 {
			tn.EnemyCost = bend(t, base*2.5, base, base*0.1)
		} else {
			tn.EnemyCost = 0
		}
		// EnemyCost must not go negative: beltSearchFloor (scoring.go) models only RecentCost's negative case,
		// and a negative EnemyCost would break the belt search's branch-and-bound.
		if tn.EnemyCost < 0 {
			tn.EnemyCost = 0
		}
		// Nerve also decides whether fighting appeals at all and where a hunter dares to hunt. Multiplied,
		// since aggression owns the base value. It is a combat trait and never lifts peaceful appetites.
		tn.ViolenceAppetite *= bend(t, 0.55, 1.0, 1.45)
		tn.HuntSecFloor = bend(t, 0.4, 0.1, -1.0)
		tn.HuntSecCeil = bend(t, 0.9, 0.8, 0.5)
		// Nerve is the dominant pull on the pilot's own travel line; lawfulness only nudges it, and both are added.
		// A careless pilot reaches deep null (0.45 - 1.45 = -1.0); a careful one holds 0.45 even as a full outlaw.
		tn.PersonalSecFloor += bend(t, 0.25, 0, -1.45)
	}

	if t := traitScale(traits[TraitGreed]); t != 0 {
		// A low slope means distance barely discounts a price, so the bot
		// will cross the map for a margin. A high slope keeps it local.
		tn.IskJumpSlope = bend(t, 0.05, jumpPriceCost, 0.008)
		tn.OreJumpWorth = bend(t, 3, beltOreJumpWorth, 16)
		// Nerve with money: grasping buys the next hull on a thin cushion.
		tn.FitAffordMultiple = bend(t, 3.5, fitAffordMultiple, 1.2)
		tn.UpgradeAffordMultiple = bend(t, 5.0, upgradeAffordMultiple, 1.8)
		// Envy splits the two commerce careers: a greedy pilot risks its own capital for a margin (trader), a
		// generous one moves other people's cargo for a fee (hauler).
		tn.TradeAppetite *= bend(t, 0.6, 1.0, 1.7)
		tn.HaulAppetite *= bend(t, 1.6, 1.0, 0.55)
		// What a haul has to pay: a content pilot takes the thin spread in front of it; a grasping one holds out
		// for a third over the ask and, with its floor raised to match, does other work instead of settling.
		tn.TradeMinMarginFrac = bend(t, 0.06, tradeMinMarginFrac, 0.35)
		tn.TradeFloorMarginFrac = bend(t, 0.02, tradeFloorMarginFrac, 0.35)
		// What a manufactured module is marked up: content sells near
		// material cost, grasping asks triple it (industry.go).
		tn.IndustryMargin = bend(t, 1.10, industryMarginDefault, 3.0)
		// What its refined minerals are marked up (minerals.go): the tightest margin at both ends, since
		// minerals are fungible and a buyer compares stations.
		tn.MineralMarkup = bend(t, 1.0, mineralMarkupDefault, 1.9)
		// What loot is marked up (beltloot.go): a generous hunter still asks the going rate rather than
		// undercutting the market, a grasping one asks 2.5 times the going rate and waits.
		tn.LootMarkup = bend(t, 1.0, lootMarkupDefault, 2.5)
		// Money nerve applied to skillbooks: the reserve is clone cover plus a replacement ship. Generous keeps
		// twice that back, grasping spends down to a fraction, in the same direction as the fit multiples above.
		tn.SkillBookCushion = bend(t, 2.0, skillBookCushionDefault, 0.4)
		// What it hands a packmate that cannot afford the gang's job (broke.go). Envy owns it: generous gives a
		// third of its wallet, grasping almost nothing.
		tn.GangShareFrac = bend(t, 0.35, gangShareFrac, 0.03)
	}

	if t := traitScale(traits[TraitWanderlust]); t != 0 {
		// A settled pilot's home pocket is expressed in jumps: the recent-system term goes negative below zero, so
		// a system just worked is cheaper than a stranger. A nomad pays 20 jumps to avoid repeating itself.
		tn.RecentCost = bend(t, -8, beltRecentCost, 20)
		tn.PreferredBonus = bend(t, 12, beltPreferredBonus, 1)
		tn.RecentSystemMemory = bendInt(t, 1, recentSystemMemory, 10)
		tn.TradeMaxStreak = bendInt(t, 8, tradeMaxStreak, 1)
		// Nomads tire of one kind of work quickly and take the
		// out-of-character option more often; settled ones grind.
		tn.SatiationHalfLifeSec = bend(t, 43200, satiationHalfLifeSec, 3600)
		tn.PickTemperature = bend(t, 0.15, pickTemperature, 0.7)
		// A factory and a trade book are places (settled), freight is work on the road (nomad). These are nudges
		// inside careers other traits own; mining is left alone because Toil owns it.
		tn.IndustryAppetite *= bend(t, 1.35, 1.0, 0.6)
		tn.TradeAppetite *= bend(t, 1.15, 1.0, 0.9)
		tn.HaulAppetite *= bend(t, 0.8, 1.0, 1.3)
		tn.ViolenceAppetite *= bend(t, 0.9, 1.0, 1.3)
		// The same axis in the skill sheet: a settled pilot spends a whole plan inside one skill group, a nomad
		// refuses to and becomes a generalist.
		tn.SkillGroupCap = bendInt(t, 6, skillGroupCapDefault, 2)
	}

	if t := traitScale(traits[TraitPatience]); t != 0 {
		tn.DwellMul = bend(t, 2.2, 1.0, 0.45)
		// What it will sit through to learn: an unhurried pilot takes a skill to IV and chews on a week-long step,
		// a hurried one wants the level it can have today and plans in half-day bites.
		tn.SkillSatisfiedLevel = bendInt(t, 4, skillSatisfiedLevelDefault, 2)
		// Bent around the operator's centre at the ratios of the shipped ends, so moving the roster's horizon
		// moves everyone and an unhurried pilot still plans further ahead than a hurried one.
		hz := skillPlanHorizonMin()
		tn.SkillPlanHorizonMin = bend(t, hz*skillHorizonPatientMul, hz, hz*skillHorizonHurriedMul)
		tn.SkillStepMaxMinutes = bend(t, 7*24*60, skillStepMaxMinutesDefault, 12*60)
		// Patience is pace: dwell, plans, lot sizes and the cash floor. It never picks between careers
		// (TestCombatTraitsNeverPickPeacefulWork); it only scales research and leans the industry sub-trip.
		tn.ResearchPatience = bend(t, researchPatienceUnhurried, 1.0, researchPatienceHurried)
		// How big a lot of refined minerals it accumulates before listing (mineralbatch.go). One-directional: the
		// table in mineralbatch.go is a minimum lot, so impatience lists at that minimum and never below it.
		tn.MineralBatchMul = bend(t, 2.5, mineralBatchMulDefault, mineralBatchMulDefault)
		// The wallet line one step earlier: the balance a miner insists on before it waits for its money (refine and
		// list) instead of taking the bid in front of it. A hurried pilot wants a comfortable float first.
		tn.MiningCashFloor = bend(t, 1_000_000, miningCashFloorDefault, 6_000_000)
	}

	if t := traitScale(traits[TraitAggression]); t != 0 {
		tn.EncounterChance = bend(t, 0.10, abstractEncounterChance, 0.30)
		tn.PatrolEncounterChance = bend(t, 0.35, abstractPatrolEncounterChance, 0.70)
		// Whether a stranger is a fight: a pacifist almost never starts one it was not owed, a violent pilot takes on
		// better than a third of the neutral traffic it meets in nullsec.
		tn.NeutralAggression = bend(t, 0.02, neutralAggressionDefault, 0.35)
		// Appetite decides how often it goes looking; this decides what edge it accepts when it finds something.
		// Multiplied against the caution value above, so the order does not matter.
		tn.EngageMargin *= bend(t, 1.35, 1.0, 0.75)
		// The main axis of what a pilot does with its days. A pacifist's small violence appetite drifts it to peaceful
		// work; which peaceful work is Toil's, Envy's and Market Role's choice.
		tn.ViolenceAppetite *= bend(t, 0.05, 1.0, 2.5)
	}

	if t := traitScale(traits[TraitLoyalty]); t != 0 {
		// A devoted pilot's grudges are worth real flying; a mercenary
		// hunts whatever is closest.
		tn.GrudgeJumpWorth = bend(t, 4.0, grudgeJumpWorth, 0.2)
		// Its own region outranks the offer. Multiplied onto wanderlust's base head start, so a settled devoted pilot
		// barely leaves its regions. The price-side edge (reach.go) is the same preference in ISK.
		tn.PreferredBonus *= bend(t, 2.0, 1.0, 0.2)
		tn.PreferredEdge = bend(t, 0.25, preferredRegionEdge, 0.02)
		// Only a properly mercenary pilot skips the polite option; anyone less prefers not to deal with factions
		// it dislikes.
		tn.HonourSoftStationPass = t < 0.5
		// How closely it follows its employer's standing directive (corpceo_directive.go). Multiplied onto the
		// leadership base: a devoted follower is the most obedient member.
		tn.Obedience *= bend(t, 1.3, 1.0, 0.7)
	}

	if t := traitScale(traits[TraitLawfulness]); t != 0 {
		// Where a hunter works: shifts the band caution chose (added, so the order does not matter). An outlaw
		// sinks it out of policed space; a lawful pilot pulls it up to the lanes.
		tn.HuntSecFloor += bend(t, 0.3, 0, -0.5)
		tn.HuntSecCeil += bend(t, 0.2, 0, -0.3)
		// An outlaw finds more trouble and attracts more of it. This is the second, weaker check on combat:
		// aggression decides whether a pilot shoots, lawfulness whether as a shady operator or a menace. Multiplied.
		tn.EncounterChance *= bend(t, 0.8, 1.0, 1.5)
		tn.PatrolEncounterChance *= bend(t, 0.9, 1.0, 1.4)
		// Whether a stranger's flag matters: a lawful pilot wants a reason before it shoots, an outlaw nearly has
		// one. Multiplied onto aggression's base; lawful is t<0, outlaw t>0.
		tn.NeutralAggression *= bend(t, 0.6, 1.0, 1.5)
		tn.ViolenceAppetite *= bend(t, 0.9, 1.0, 1.2)
		// The pilot's own travel line is a nudge here; caution owns this axis. An outlaw of even nerve slips into
		// lowsec (0.45 - 0.25 = 0.20); it takes carelessness on top to reach null. Added, not assigned.
		tn.PersonalSecFloor += bend(t, 0.15, 0, -0.25)
	}

	if t := traitScale(traits[TraitSupply]); t != 0 {
		// Where a pilot sits on the make-it/buy-it axis, as a disposition and not a gate: everyone builds and the trait
		// decides how much of one they are. TradeSupplyBias makes a consumer watch the shortage ledger more.
		tn.SupplyDrive = bend(t, supplyDriveFloor, supplyDriveBase, 2.0)
		tn.TradeSupplyBias = bend(t, 0.5, 1.0, 1.5)
		// The supply axis reaches manufacturing through SupplyDrive alone. It does not touch mining or commerce:
		// Toil owns mining, and a consumer still earns with guns.
		tn.ViolenceAppetite *= bend(t, 1.5, 1.0, 0.8)
	}

	if t := traitScale(traits[TraitToil]); t != 0 {
		// Hands-on versus desk work: the trait that owns mining and puts manufacturing in the middle. Each curve is
		// divided by its value at zero, so an untraited pilot is defaultTuning exactly.
		tn.MiningAppetite *= toilMining(t) / toilMining(0)
		tn.HaulAppetite *= toilHaul(t) / toilHaul(0)
		tn.TradeAppetite *= toilTrade(t) / toilTrade(0)
		tn.IndustryAppetite *= toilIndustry(t) / toilIndustry(0)
		tn.ResearchFactor = toilResearch(t)
	}

	tn.CareerMining = posPole(-traitScale(traits[TraitToil]))
	// A professional miner flies further for better rock: up to twice the jumps per doubling of hold value,
	// multiplied onto Greed's bend. Security is untouched; a belt's measured danger (beltCost) keeps a miner off it.
	tn.OreJumpWorth *= 1 + tn.CareerMining
	tn.CareerHaul = posPole(-traitScale(traits[TraitGreed]))
	tn.CareerTrade = posPole(traitScale(traits[TraitGreed]))
	tn.CareerCombat = posPole(traitScale(traits[TraitAggression]))

	if t := traitScale(traits[TraitLeadership]); t != 0 {
		// A follower barely wants a fleet seat; a leader wants it twice as badly as a neutral pilot. Obedience runs
		// the other way down the same axis.
		tn.CommandDrive = bend(t, commandDriveFollower, 1.0, commandDriveLeader)
		tn.Obedience *= bend(t, obedienceFollower, 1.0, obedienceLeader)
		// Founding a corporation is a leader's act: nothing until the trait is past foundPropensityFrom, then
		// rising to 1 at the pole.
		if t > foundPropensityFrom {
			tn.FoundPropensity = (t - foundPropensityFrom) / (1 - foundPropensityFrom)
		}
	}

	if t := traitScale(traits[TraitTolerance]); t != 0 {
		// Two poles, never both: the high end is the xenophobe (the hostile end by the polarity convention),
		// the low end the xenophile.
		tn.Xenophobia = posPole(t)
		tn.Xenophilia = posPole(-t)
	}

	if t := traitScale(traits[TraitGrace]); t != 0 {
		tn.Vindictiveness = posPole(t)
		tn.Forgiveness = posPole(-t)
	}

	// Piracy sits outside either trait's block: its appetite is the product of the violence and outlaw factors,
	// so a violent lawful pilot hunts rats, a pacifist outlaw smuggles, and neither pirates.
	violence := traitScale(traits[TraitAggression])
	lawless := traitScale(traits[TraitLawfulness])
	nerve := traitScale(traits[TraitCaution])
	if violence > 0 && lawless > 0 {
		// Both factors climb from nothing, so the appetite is quadratic in how far out the pilot is on both axes.
		// At both poles it reaches 3.6, which must clear the violence appetite the same traits produce (3.0).
		tn.PiracyAppetite = (2.4 * violence) * (1.5 * lawless)
	}

	// Nerve gates how high it strikes: lowsec by default, a careless pilot buys its way up toward 0.6 (CONCORD
	// in 12 s), lawlessness only sweetens that, and a careful pilot works deep lowsec however little it respects the law.
	switch {
	case nerve > 0:
		tn.PiracySecCeil = piracySecCeilDefault + 0.2*nerve*(0.5+0.5*max(0, lawless))
	case nerve < 0:
		tn.PiracySecCeil = piracySecCeilDefault + 0.2*nerve
	}
	if tn.PiracySecCeil > 0.6 {
		tn.PiracySecCeil = 0.6
	}
	// And how much of that clock it insists on leaving itself.
	tn.ConcordNerveSec = bend(nerve, 14, concordNerveSecDefault, 2)

	if t := traitScale(traits[TraitGreed]); t != 0 {
		// Terms or no terms: a grasping pirate would rather be paid than shoot, a generous one is here for the
		// violence and does not haggle. The multiplier is what it asks for.
		tn.RansomChance = bend(t, 0.15, ransomChanceDefault, 0.85)
		tn.RansomMul = bend(t, 0.5, 1.0, 2.5)
	}
	// Shooting the capsule pays nothing and costs a clone, so it reads off both violence and outlawry; a pilot
	// that ransoms for a living mostly lets pods go.
	tn.PodKillChance = podKillBase() + 0.35*max(0, violence) + 0.35*max(0, lawless)
	if tn.PodKillChance > 0.9 {
		tn.PodKillChance = 0.9
	}
	// Camp or prowl. An unhurried pirate will sit on a gate for an hour;
	// a hurried one cannot stand it and tours the belts instead.
	tn.GateCampChance = bend(traitScale(traits[TraitPatience]), 0.85, gateCampChanceDefault, 0.10)

	// Caution and lawfulness both move the hunting band; keep it a band.
	if tn.HuntSecFloor < -1 {
		tn.HuntSecFloor = -1
	}
	if tn.HuntSecCeil > 1 {
		tn.HuntSecCeil = 1
	}
	if tn.HuntSecCeil < tn.HuntSecFloor {
		tn.HuntSecCeil = tn.HuntSecFloor
	}

	// Caution and lawfulness both move the personal travel line; keep it
	// somewhere the pilot can actually fly.
	if tn.PersonalSecFloor < -1 {
		tn.PersonalSecFloor = -1
	}
	if tn.PersonalSecFloor > 0.8 {
		tn.PersonalSecFloor = 0.8
	}

	// Style and seats, off the raw poles (fitstyle.go).
	careful := posPole(-nerve)
	careless := posPole(nerve)
	pacifist := posPole(-violence)
	violent := posPole(violence)
	devoted := posPole(-traitScale(traits[TraitLoyalty]))
	lead := traitScale(traits[TraitLeadership])
	tn.KiteLean = 0.6*careful + 0.4*pacifist
	tn.BrawlLean = 0.6*careless + 0.4*violent
	tn.LogiDrive = pacifist + (0.25*devoted+0.15*posPole(-lead))*(1-violent)
	tn.EwarDrive = 0.6*careful + 0.4*posPole(lead)
	tn.TackleDrive = careless
	tn.ScoutDrive = posPole(traitScale(traits[TraitGreed]))
	tn.Pacifist = violence <= pacifistAggressionFloor
	tn.FightsPolice = fightsPolice(traitScale(traits[TraitCaution]))

	// The research career comes last: it reads the finished manufacturing disposition (SupplyDrive x
	// IndustryAppetite, bent by every trait above) so the lab, the tag and the arm agree with the factory draw.
	tn.ResearchAppetite = tn.SupplyDrive * tn.IndustryAppetite * tn.ResearchFactor * tn.ResearchPatience
	tn.CareerResearch = clamp01(tn.ResearchAppetite / researchCareerFull)
	// Unhurried (negative Patience) and settled (negative Wanderlust) pilots
	// plan long; Patience carries more of it than Wanderlust.
	tn.PlanLean = clamp01(0.5 - 0.5*(0.6*traitScale(traits[TraitPatience])+0.4*traitScale(traits[TraitWanderlust])))

	return tn
}

// traitsSummary describes a personality in the operator's own words, for
// the panel's confirmation line. A pilot with nothing set says so, rather
// than reporting a save that changed nothing.
func traitsSummary(traits map[string]int) string {
	var parts []string
	for _, def := range traitCatalog {
		v := traits[def.Key]
		switch {
		case v >= 60:
			parts = append(parts, "very "+def.High)
		case v >= 20:
			parts = append(parts, def.High)
		case v <= -60:
			parts = append(parts, "very "+def.Low)
		case v <= -20:
			parts = append(parts, def.Low)
		}
	}
	if len(parts) == 0 {
		return "personality saved — an even temperament, so this pilot behaves the way rolebots always have"
	}
	return "personality saved — this pilot is " + joinAnd(parts)
}

// joinAnd renders a list the way a sentence would.
func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	}
	out := ""
	for i, p := range parts[:len(parts)-1] {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out + " and " + parts[len(parts)-1]
}

// dwell scales one of the sim's invented durations by this bot's patience,
// never below a second.
func (t Tuning) dwell(sec int64) int64 {
	v := int64(float64(sec)*t.DwellMul + 0.5)
	if v < 1 {
		return 1
	}
	return v
}
