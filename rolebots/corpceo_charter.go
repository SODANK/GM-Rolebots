package rolebots

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// The charter: what a CEO's personality means for its corporation.
// Same construction as Tuning (tuning.go): the traits resolve, once per think,
// into a flat set of numbers that every power reads. The arithmetic stays in
// one pure function a test can sweep, and Charter.Why prints the answer to why
// the CEO did something. Nothing here reads the database. Tags are scored with
// the Appeal functions of the recruitment scorer (corps.go) and the directive
// with the dispositions of the work chooser (appetite.go).

// ceoTagPool is the vocabulary a CEO describes its corporation from: tags that
// something downstream acts on (fleets, doctrine, a recruit's fit). Civic tags
// describe NPC institutions and are left out.
var ceoTagPool = []string{
	"mining", "industry", "shipbuilding", "research",
	"trade", "hauling",
	"combat", "security", "outlaw", "cartelindustry",
}

const (
	// ceoTagKeepFrac: a tag is kept when its appeal is within this fraction of the
	// best one (the same half-mean/half-best rule corpTagFit uses).
	ceoTagKeepFrac = 0.7
	ceoTagMax      = 3

	// ceoDirectiveMax is how many activities a directive names; every other
	// activity gets zero weight.
	ceoDirectiveMax = 3
)

// Charter is a CEO's resolved policy. Exported fields because the last one is
// stored as JSON for the panel.
type Charter struct {
	// Identity.
	Tags      []string
	TagAppeal map[string]float64

	// Mercenary: the corporation hires its guns out (the mercenary tag, appended
	// beyond ceoTagMax). MercenaryChance is how likely this temperament is to run
	// one; Mercenary is this CEO's own stable roll (ceoMercenaryRoll).
	Mercenary       bool
	MercenaryChance float64

	// Directive: activity slug to share of the corporation's effort (sums to 1
	// over the named activities). Strength is the CEO's own CommandDrive.
	Directive         map[string]float64
	DirectiveStrength float64

	// Cadence: how often it thinks at all.
	ThinkEverySec int64

	// Treasury.
	TaxRate       float64 // a fraction, as corporation.taxRate stores it
	ReserveMonths float64 // months of office rent it will not spend below
	StipendShare  float64 // share of the surplus it hands out per week
	BackingFrac   float64 // share of its own spare wealth it puts into a short corporation per week

	// Applications.
	HRBar           float64 // minimum fit an applicant must bring
	HRDelaySec      int64   // how long an application sits before it is read
	HRHoldSec       int64   // how long a good applicant is kept waiting for an opening
	ApplicantMinSec float64 // lowest security status it will take on

	// Estate.
	HQSecFloor float64 // lowest security it will keep an address in
	Nomad      bool    // lives on the move: keeps a second office in another region

	// Hierarchy: one director per DirectorPer members (from 5 members), one
	// personnel manager per PersonnelPer.
	DirectorPer  int
	PersonnelPer int

	// Industry (oredesk.go, corpproduction.go, corpwork.go, corpsrp.go,
	// corpstore.go). The ore desk's bounds come from the corporation's tags; these
	// are how the CEO moves inside them and what it pays.
	OreHalfLifeSec int64   // how slowly the ore split follows the economics
	OreCashWeight  float64 // λ: how hard a thin treasury pulls ore toward the market
	WageFrac       float64 // a run's labour credit, as a share of its product value (corppayroll.go)
	PayrollShare   float64 // share of a week's operating profit paid out as the merit payroll
	DirectorSalary float64 // the Director of Industry's weekly salary
	BuybackFrac    float64 // what the corporation bids for ore/minerals, as a share of their value
	CapitalRisk    float64 // share of free capital the production power may commit
	METarget       int     // the most material levels the library is researched to, and only while they pay
	SRPCap         float64 // largest hull value the corporation replaces
	StoreMarkup    float64 // company store margin over cost

	// What it bids for Zydrine and Megacyte over their value (corpdeep.go).
	DeepPremium float64

	// Shares (corpshares.go). The price is the corporation's value per share;
	// these are how the CEO leans on it.
	FounderFrac   float64 // share of the issue the founder keeps, at no cost
	ControlFloor  float64 // share of the issue the treasury never sells below
	PremiumMax    float64 // most it will ask over fair value when stock is in demand
	Haircut       float64 // discount on book value when it buys shares back
	GoodwillWeeks float64 // weeks of earnings the price counts on top of book value
	BuybackWeekly float64 // share of free capital it spends buying shares back, per week

	// Dividends (corpdividend.go): the share of what the corporation earned
	// since the last payout that goes to its shareholders, and how often.
	PayoutRatio      float64
	DividendEverySec int64

	// Diplomacy (corpceo_diplomacy.go): the relations ledger holds a +-10 value per
	// player corporation. FriendAt and AllyAt propose a pact or alliance, RivalAt
	// and EnemyAt mark rivals and enemies. Hysteresis and DwellSec damp tier changes.
	FriendAt, AllyAt, RivalAt, EnemyAt float64
	Hysteresis                         float64
	DwellSec                           int64
	MaxAllies, MaxFriends, MaxRivals   int
	// The rates of the ledger: GoodwillGain scales positive impulses,
	// GoodwillHalfLifeD fades warmth above baseline (GrudgeHalfLifeD the grudge),
	// ProposalEverySec spaces letters and LetterAppetite sets how eager it is.
	GoodwillGain      float64
	GoodwillHalfLifeD float64
	ProposalEverySec  int64
	LetterAppetite    float64
	// The weights of the signals (ceoRelScore).
	Xenophobia      float64 // the CEO's tolerance, as tuning resolves it (tolerance.go)
	Xenophilia      float64
	PrejudiceWeight float64 // how hard the CEO's tolerance leans on a corporation's people
	PoliticsWeight  float64 // the empires' own opinion of each other's peoples
	PreyWeight      float64 // an outlaw outfit marking a soft industrial corporation
	LawWeight       float64 // a lawful outfit marking a pirate one
	GrievanceWeight float64 // per member of ours they killed, decayed
	GrudgeHalfLifeD float64 // how fast a killing is forgiven, in days
	CompeteWeight   float64 // the same trade in the same region
	ReciprocityWarm float64 // share of their warmth toward us returned
	ReciprocityCold float64 // share of their hostility toward us returned
	// The alliance (alliance.go): AllianceAppetite is the keenness to found or join
	// one, AllianceLoyalty how long it stays once the executor sours, TitheTolerance
	// how many tithes it may refuse, ExecutorIncumbency the executor's vote edge.
	AllianceAppetite   float64
	AllianceLoyalty    float64
	TitheTolerance     int
	ExecutorIncumbency float64
	MaxSovTargets      int
	// StationPolicy is what a station the corporation owns lets in (stationpolicy.go): a freeport,
	// or blues only.
	StationPolicy string

	// War (corpceo_war.go, merc.go): WarAt is the motive a quarrel must reach before
	// the CEO goes to CONCORD. WarMaxOdds is the worst enemy ratio it fights itself
	// (beyond it, it hires); WarPatienceWeeks and WarPainRatio say when it gives up.
	WarAt            float64
	MaxWars          int
	WarMaxOdds       float64
	WarPatienceWeeks float64
	WarPainRatio     float64
	WarChestFrac     float64
	WarRunwayWeeks   float64
	WarFootingMax    float64
	// Negotiation, both sides of a merc contract: ConcedeExp above 1 holds the price
	// until the deadline, below 1 gives ground early. OpenFrac is a client's opening
	// against its walk-away; the Merc fields shape a mercenary's price and cut.
	ConcedeExp    float64
	OpenFrac      float64
	MercMargin    float64
	MercGreedPull float64
	MercMaxOdds   float64
	MercBonusRate float64

	// Starbases (posceo.go): PosAppetite is the keenness to put infrastructure in
	// space, PosDangerWeight how hard measured danger bites a site's score,
	// FuelWeekReserve the fuel batches per tower, ProcureShare the requisition cap.
	PosAppetite     float64
	PosDangerWeight float64
	FuelWeekReserve int
	ProcureShare    float64
	StrontiumHours  int

	// Why is the table the panel prints: each decision with the trait that
	// drove it, in the operator's words.
	Why []charterWhy
}

type charterWhy struct {
	Field, Value, Because string
}

// resolveCharter turns a CEO's identity into its corporation's policy.
func resolveCharter(p pilotIdentity) Charter {
	tn := p.Tune
	ch := Charter{TagAppeal: map[string]float64{}, Directive: map[string]float64{}}
	why := func(field, value, format string, args ...any) {
		ch.Why = append(ch.Why, charterWhy{field, value, fmt.Sprintf(format, args...)})
	}

	// ── identity ────────────────────────────────────────────────────────
	type scored struct {
		key string
		v   float64
	}
	var tags []scored
	for _, k := range ceoTagPool {
		t, ok := corpTagByKey(k)
		if !ok {
			continue
		}
		v := t.Appeal(p)
		ch.TagAppeal[k] = v
		if v > 0 {
			tags = append(tags, scored{k, v})
		}
	}
	sort.SliceStable(tags, func(i, j int) bool { return tags[i].v > tags[j].v })
	if len(tags) > 0 {
		best := tags[0].v
		for _, s := range tags {
			if len(ch.Tags) >= ceoTagMax || s.v < best*ceoTagKeepFrac {
				break
			}
			// A police outfit and a pirate outfit are not the same outfit;
			// whichever the CEO leans to first wins and the other is dropped.
			if (s.key == "security" && containsStr(ch.Tags, "outlaw")) ||
				(s.key == "outlaw" && containsStr(ch.Tags, "security")) {
				continue
			}
			ch.Tags = append(ch.Tags, s.key)
		}
	}
	why("Tags", strings.Join(ch.Tags, ", "),
		"the work it would most want to join an outfit for — the recruitment scorer's own appeal, from the CEO's side")

	// ── directive ───────────────────────────────────────────────────────
	type act struct {
		slug string
		v    float64
	}
	var acts []act
	for _, def := range activityCatalog {
		// Piracy is a directive only for an outlaw-tagged corporation: other members
		// are not asked to rob people.
		if def.Slug == ActPiracy && !containsStr(ch.Tags, "outlaw") {
			continue
		}
		if v := def.Disposition(tn); v > 0 {
			acts = append(acts, act{def.Slug, v})
		}
	}
	sort.SliceStable(acts, func(i, j int) bool { return acts[i].v > acts[j].v })
	if len(acts) > ceoDirectiveMax {
		acts = acts[:ceoDirectiveMax]
	}
	total := 0.0
	for _, a := range acts {
		total += a.v
	}
	var named []string
	for _, a := range acts {
		if total > 0 {
			ch.Directive[a.slug] = a.v / total
			named = append(named, fmt.Sprintf("%s %.0f%%", a.slug, 100*a.v/total))
		}
	}
	ch.DirectiveStrength = tn.CommandDrive
	why("Directive", strings.Join(named, ", "), "what the CEO itself most wants to be doing")
	why("Directive strength", fmt.Sprintf("%.2f", ch.DirectiveStrength),
		"its leadership — a born leader pushes its directive twice as hard as a neutral one")

	// ── mercenary ───────────────────────────────────────────────────────
	// Read after the directive: a fighting outfit is armed-tagged or directs members to fight.
	ch.MercenaryChance = ceoMercenaryChance(p, ch)
	if ch.MercenaryChance > 0 {
		ch.Mercenary = ceoMercenaryRoll(p.CharID) < ch.MercenaryChance
		verdict := "no"
		if ch.Mercenary {
			verdict = "yes"
			ch.Tags = append(ch.Tags, tagMercenary)
		}
		why("Guns for hire", fmt.Sprintf("%s (%.0f%% likely)", verdict, ch.MercenaryChance*100),
			"loyalty — a mercenary CEO running a fighting outfit hires its guns out to other corporations' wars")
	}

	// ── cadence ─────────────────────────────────────────────────────────
	ch.ThinkEverySec = int64(bend(p.trait(TraitPatience), 6*3600, 2*3600, 30*60))
	why("Reviews the corporation", "every "+fmtDuration(ch.ThinkEverySec), "patience — unhurried looks in every six hours, hurried every half hour")

	// ── treasury ────────────────────────────────────────────────────────
	ch.TaxRate = bend(p.trait(TraitGreed), 0.02, 0.10, 0.20)
	why("Tax rate", fmt.Sprintf("%.0f%%", ch.TaxRate*100), "envy — generous CEOs tax lightly, greedy ones take a fifth")
	ch.ReserveMonths = bend(p.trait(TraitCaution), 6, 3, 1.5)
	why("Reserve", fmt.Sprintf("%.1f months of rent", ch.ReserveMonths), "caution — never spends the corporation below this")
	ch.StipendShare = bend(p.trait(TraitGreed), 0.25, 0.10, 0.02)
	why("Stipends", fmt.Sprintf("%.0f%% of the surplus a week", ch.StipendShare*100), "envy — what it hands struggling members doing the corporation's work")
	// Polarity: LOW loyalty is devoted, LOW greed generous, HIGH leadership
	// the born leader who thinks of the outfit as its own.
	ch.BackingFrac = bend(p.trait(TraitLoyalty), 0.50, 0.25, 0.05) *
		bend(p.trait(TraitGreed), 1.3, 1.0, 0.6) * bend(p.trait(TraitLeadership), 0.8, 1.0, 1.2)
	ch.BackingFrac = math.Max(0, math.Min(ch.BackingFrac, 0.6))
	why("Backs a short corporation with", fmt.Sprintf("up to %.0f%% of its own spare ISK a week", ch.BackingFrac*100),
		"loyalty and envy — a devoted, generous CEO bails its outfit out of its own pocket, a mercenary or greedy one lets it starve")

	// ── applications ────────────────────────────────────────────────────
	// The bar is on corpTagFit's scale: neutral pilots score about 0.9, applicants about 1.4.
	ch.HRBar = bend(p.trait(TraitLoyalty), 1.4, 0.85, 0.5)
	why("Applicant bar", fmt.Sprintf("fit ≥ %.2f", ch.HRBar), "loyalty — a devoted CEO is picky about who wears the badge, a mercenary takes anyone")
	ch.HRDelaySec = int64(bend(p.trait(TraitPatience), 24*3600, 6*3600, 3600))
	why("Answers applications after", fmtDuration(ch.HRDelaySec), "patience — Personnel Managers read the desk within the hour whatever the CEO's habit")
	// Held + delay stays under corpApplyExpirySec, so a full corporation answers
	// a waiting applicant before it gives up.
	ch.HRHoldSec = int64(bend(p.trait(TraitPatience), 4*86400, 2*86400, 12*3600))
	why("Keeps a waiting list for", fmtDuration(ch.HRHoldSec), "patience — how long a good applicant is kept waiting for an opening before being let go")
	ch.ApplicantMinSec = bend(p.trait(TraitLawfulness), 0, -2, -10)
	why("Lowest applicant security status", fmt.Sprintf("%.1f", ch.ApplicantMinSec), "lawfulness — a lawful CEO hires nobody with a criminal record")

	// ── estate ──────────────────────────────────────────────────────────
	// The pilot's own travel line (Tuning.PersonalSecFloor): the CEO keeps the address where it would live.
	ch.HQSecFloor = tn.PersonalSecFloor
	why("Addresses", fmt.Sprintf("security ≥ %.2f", ch.HQSecFloor), "where the CEO itself is willing to fly")
	ch.Nomad = p.nomadPole() > 0.5
	if ch.Nomad {
		why("Second office", "yes, in another region", "wanderlust — an outfit that lives on the move wants somewhere to land at both ends")
	}

	// ── starbases ───────────────────────────────────────────────────────
	// Polarity: HIGH caution is careless, HIGH wanderlust travelled.
	ch.PosAppetite = math.Max(0, tn.SupplyDrive/supplyDriveBase+
		0.5*p.trait(TraitWanderlust)+0.5*p.trait(TraitCaution))
	why("Starbase appetite", fmt.Sprintf("%.2f", ch.PosAppetite),
		"supply drive, wanderlust and nerve — a builder that roams and takes risks puts towers in space")
	ch.PosDangerWeight = bend(p.trait(TraitCaution), 1.6, 1.0, 0.6)
	why("Weighs a site's danger", fmt.Sprintf("×%.1f", ch.PosDangerWeight),
		"caution — how hard the measured danger at a moon counts against it (no security line of its own)")
	ch.FuelWeekReserve = fuelTargetBatches
	ch.ProcureShare = bend(p.trait(TraitCaution), 0.25, 0.4, 0.6)
	why("Procurement budget", fmt.Sprintf("%.0f%% of free capital in open requisitions", ch.ProcureShare*100),
		"caution")
	ch.StrontiumHours = bendInt(p.trait(TraitCaution), 36, strontiumHoursDefault, 12)
	why("Keeps strontium for", fmt.Sprintf("%d hours of reinforcement", ch.StrontiumHours),
		"caution — a careful CEO stocks for a longer timer")

	// ── hierarchy ───────────────────────────────────────────────────────
	ch.DirectorPer = 15
	ch.PersonnelPer = 20

	// ── industry ────────────────────────────────────────────────────────
	ch.OreHalfLifeSec = int64(bend(p.trait(TraitPatience), 72*3600, 36*3600, 12*3600))
	why("Ore desk moves", "half way in "+fmtDuration(ch.OreHalfLifeSec),
		"patience — an unhurried CEO lets the sell/build split drift over days, a hurried one swings it in hours")
	ch.OreCashWeight = bend(p.trait(TraitGreed), 1.5, 3, 6)
	why("Cash pull on ore", fmt.Sprintf("λ %.1f", ch.OreCashWeight),
		"envy — a greedy CEO sells ore for cash the moment the treasury runs thin")
	ch.WageFrac = bend(p.trait(TraitGreed), 0.12, 0.08, 0.04)
	why("Builder credit", fmt.Sprintf("%.0f%% of each run's value", ch.WageFrac*100),
		"envy — what a run on the corporation's blueprints counts for on a member's labour record")
	ch.PayrollShare = bend(p.trait(TraitGreed), 0.40, 0.30, 0.20)
	why("Weekly payroll", fmt.Sprintf("%.0f%% of the week's operating profit, split by merit", ch.PayrollShare*100),
		"envy — a generous CEO shares the week's profit with the members who earned it, a greedy one keeps it")
	ch.DirectorSalary = bend(p.trait(TraitGreed), 3e6, 1.5e6, 0.5e6)
	why("Director of Industry salary", iskString(ch.DirectorSalary)+" a week", "envy")
	ch.BuybackFrac = bend(p.trait(TraitGreed), 0.92, 0.87, 0.80)
	why("Buyback", fmt.Sprintf("%.0f%% of value", ch.BuybackFrac*100),
		"envy — what the corporation pays members for ore and minerals it wants")
	ch.CapitalRisk = bend(p.trait(TraitCaution), 0.25, 0.4, 0.6)
	why("Production budget", fmt.Sprintf("%.0f%% of free capital", ch.CapitalRisk*100),
		"caution — a careful CEO commits a quarter of what it can spare, a careless one more than half")
	ch.METarget = bendInt(p.trait(TraitPatience), 20, 10, 5)
	why("Blueprint research", fmt.Sprintf("up to ME %d, while it pays", ch.METarget),
		"patience — an unhurried CEO allows its library more research; each level must still save more than its wage")
	ch.SRPCap = bend(p.trait(TraitLoyalty), 50e6, 20e6, 5e6)
	why("Ship replacement", "hulls up to "+iskString(ch.SRPCap),
		"loyalty — a devoted CEO replaces what its members lose flying for it")
	ch.StoreMarkup = bend(p.trait(TraitGreed), 0.02, 0.05, 0.10)
	why("Company store", fmt.Sprintf("cost + %.0f%%", ch.StoreMarkup*100),
		"envy — what the corporation charges its own members for what it builds")
	// Deep metals (corpdeep.go). Polarity: LOW greed is the generous.
	ch.DeepPremium = bend(p.trait(TraitGreed), 1.5, 1.3, 1.15)
	why("Bids for Zydrine and Megacyte", fmt.Sprintf("×%.2f their price", ch.DeepPremium),
		"envy — a generous CEO pays miners to go deep for the metal its hulls need")

	// Polarity: LOW leadership follows, LOW greed is generous, LOW patience is
	// unhurried, LOW caution is careful.
	ch.FounderFrac = bend(p.trait(TraitLeadership), 0.10, 0.20, 0.30)
	why("Founder's stake", fmt.Sprintf("%.0f%% of the shares", ch.FounderFrac*100),
		"leadership — what the founder keeps for having built the outfit")
	ch.ControlFloor = bend(p.trait(TraitLeadership), 0, 0.35, 0.51)
	if ch.ControlFloor >= 0.5 {
		why("Control", fmt.Sprintf("keeps %.0f%% of the shares in the treasury", ch.ControlFloor*100),
			"leadership — a born leader never floats enough to be voted out; it issues more instead")
	} else {
		why("Control", fmt.Sprintf("keeps %.0f%% of the shares in the treasury", ch.ControlFloor*100),
			"leadership — a follower floats the corporation to its members, and they may one day outvote it")
	}
	ch.PremiumMax = bend(p.trait(TraitGreed), 0.10, 0.25, 0.50)
	why("Share price", fmt.Sprintf("up to %.0f%% over fair value", ch.PremiumMax*100),
		"envy — how far it raises the price while its shares are in demand")
	ch.Haircut = bend(p.trait(TraitGreed), 0.05, 0.10, 0.20)
	why("Buys shares back at", fmt.Sprintf("book value less %.0f%%", ch.Haircut*100), "envy")
	ch.GoodwillWeeks = bend(p.trait(TraitPatience), 26, 16, 8)
	why("Values the corporation at", fmt.Sprintf("book value + %.0f weeks of earnings", ch.GoodwillWeeks),
		"patience — an unhurried CEO prices in the long run")
	ch.BuybackWeekly = bend(p.trait(TraitCaution), 0.15, 0.25, 0.35)
	why("Buyback budget", fmt.Sprintf("%.0f%% of free capital a week", ch.BuybackWeekly*100),
		"caution — a careful CEO keeps its cash when holders want out")
	// Generosity sets the payout; a careful or an unhurried CEO keeps more
	// of it to grow on, a careless or hurried one hands it out.
	ch.PayoutRatio = bend(p.trait(TraitGreed), 0.60, 0.35, 0.15) *
		bend(p.trait(TraitCaution), 0.8, 1.0, 1.15) * bend(p.trait(TraitPatience), 0.8, 1.0, 1.15)
	ch.PayoutRatio = math.Max(0.05, math.Min(ch.PayoutRatio, 0.8))
	why("Dividend", fmt.Sprintf("%.0f%% of earnings", ch.PayoutRatio*100),
		"envy sets it — a generous CEO pays out most of what the corporation earns; caution and patience keep some back to grow on")
	ch.DividendEverySec = int64(bend(p.trait(TraitPatience), 28*86400, 14*86400, 7*86400))
	why("Pays dividends", fmt.Sprintf("every %d days", ch.DividendEverySec/86400), "patience")

	// Polarity is traitCatalog's: LOW loyalty is devoted, LOW lawfulness is lawful,
	// LOW patience is unhurried. Reading it the other way inverts each line.
	ch.FriendAt = bend(p.trait(TraitLoyalty), 2.5, 3.0, 4.0)
	ch.AllyAt = bend(p.trait(TraitLoyalty), 5.5, 6.5, 8.0)
	why("Proposes a pact at", fmt.Sprintf("%+.1f, an alliance at %+.1f", ch.FriendAt, ch.AllyAt),
		"loyalty — a devoted CEO gives its word easily and means it, a mercenary one keeps its options open")
	ch.RivalAt = bend(p.trait(TraitAggression), -4.5, -3.0, -2.0)
	ch.EnemyAt = bend(p.trait(TraitAggression), -8.0, -6.5, -5.0)
	why("Answers no letter below", fmt.Sprintf("%+.1f; counts an enemy for war at %+.1f", ch.RivalAt, ch.EnemyAt),
		"aggression — a violent CEO names enemies quickly, a pacifist one almost never")
	ch.Hysteresis = bend(p.trait(TraitPatience), 1.5, 1.0, 0.5)
	ch.DwellSec = int64(bend(p.trait(TraitPatience), 48*3600, 24*3600, 6*3600))
	why("Gives up a tier", fmt.Sprintf("only %.1f past its line; counts it held after %s", ch.Hysteresis, fmtDuration(ch.DwellSec)),
		"patience — an unhurried CEO lets a relationship settle before it re-judges it")
	ch.GoodwillGain = bend(p.trait(TraitLoyalty), 1.0, 0.6, 0.3)
	ch.GoodwillHalfLifeD = bend(p.trait(TraitLoyalty), 30, 14, 5)
	why("Warms", fmt.Sprintf("×%.1f per good turn, fading by half every %.0f days", ch.GoodwillGain, ch.GoodwillHalfLifeD),
		"loyalty — a devoted CEO is won over and keeps its friends, a mercenary one forgets a favour in a week")
	ch.ProposalEverySec = int64(bend(p.trait(TraitPatience), 7*86400, 3*86400, 86400))
	ch.LetterAppetite = 0.5 + posPole(p.trait(TraitLeadership))
	why("Sends a letter", fmt.Sprintf("at most every %s, appetite ×%.1f", fmtDuration(ch.ProposalEverySec), ch.LetterAppetite),
		"patience paces the envoys, leadership makes the approach")
	ch.MaxAllies = bendInt(p.trait(TraitLeadership), 1, 2, 4)
	ch.MaxFriends = bendInt(p.trait(TraitLeadership), 4, 6, 10)
	ch.MaxRivals = bendInt(p.trait(TraitAggression), 1, 3, 6)
	why("Keeps at most", fmt.Sprintf("%d allies, %d friends, %d rivals", ch.MaxAllies, ch.MaxFriends, ch.MaxRivals),
		"leadership sizes the friendships it can hold together, aggression the fights it will pick")

	// Prejudice is tolerance (a xenophobe favours its own people, a xenophile seeks
	// foreigners); politics is loyalty (a devoted CEO carries its empire's views).
	ch.Xenophobia, ch.Xenophilia = p.Tune.Xenophobia, p.Tune.Xenophilia
	ch.PrejudiceWeight = ceoRelPrejudiceWeight
	switch {
	case xenoTierLabel(p.Tune) == xenoNeutralLabel:
		// Below the wary line the pole still weighs in, faintly (ceoRelScore).
		why("Race policy", xenoNeutralLabel, "tolerance — a corporation's people barely sway it either way")
	case ch.Xenophobia > 0:
		why("Race policy", xenoTierLabel(p.Tune),
			"tolerance — favours its own people and marks those its empire stands against (every other people, at the extreme)")
	case ch.Xenophilia > 0:
		why("Race policy", xenoTierLabel(p.Tune),
			"tolerance — seeks out corporations of other peoples")
	default:
		why("Race policy", xenoTierLabel(p.Tune), "tolerance — a corporation's people count for nothing either way")
	}
	ch.PoliticsWeight = bend(p.trait(TraitLoyalty), 2.0, 1.0, 0.3)
	why("Empire politics", fmt.Sprintf("×%.1f", ch.PoliticsWeight),
		"loyalty — how much the empires' own opinion of each other's peoples counts")
	// Predation is only an outlaw outfit's (ceoRelScore checks the tag); soft
	// targets are marked for cargo, hence aggression and envy.
	ch.PreyWeight = bend(p.trait(TraitAggression), 2.0, 4.0, 7.0) * (1 + 0.3*p.trait(TraitGreed))
	// A lawful CEO marks pirates, an outlaw never does (police outfits are floored
	// in ceoRelScore). A neutral 2.4 sits just short of the rival line (-2.5).
	ch.LawWeight = bend(p.trait(TraitLawfulness), 7.0, 2.4, 0)
	why("Stance on pirates", fmt.Sprintf("×%.1f", ch.LawWeight),
		"lawfulness — a lawful CEO marks outlaw outfits as rivals, an outlaw one never does")
	ch.GrievanceWeight = bend(p.trait(TraitAggression), 0.8, 1.2, 2.0) *
		bend(p.trait(TraitLoyalty), 1.3, 1.0, 0.5)
	ch.GrudgeHalfLifeD = bend(p.trait(TraitPatience), 14, 7, 3)
	why("Grudges", fmt.Sprintf("%.1f per member killed, halving every %.0f days", ch.GrievanceWeight, ch.GrudgeHalfLifeD),
		"aggression sets the anger, loyalty whether it is held — a mercenary holds no grudges")
	ch.CompeteWeight = bend(p.trait(TraitGreed), 0.5, 1.5, 3.0)
	ch.ReciprocityWarm = bend(p.trait(TraitLoyalty), 0.6, 0.4, 0.2)
	ch.ReciprocityCold = bend(p.trait(TraitAggression), 0.3, 0.5, 0.8)
	ch.AllianceAppetite = posPole(p.trait(TraitLeadership))
	why("Alliance appetite", fmt.Sprintf("%.2f", ch.AllianceAppetite),
		"leadership — a leader founds a bloc or follows its allies into one; a follower keeps its own counsel")
	ch.AllianceLoyalty = bend(p.trait(TraitLoyalty), 1.0, 0.6, 0.3)
	ch.TitheTolerance = bendInt(p.trait(TraitPatience), 4, 2, 1)
	ch.ExecutorIncumbency = bend(p.trait(TraitLoyalty), 1.5, 1.0, 0.4)
	why("Stands by its alliance", fmt.Sprintf("×%.1f past a soured executor; swallows %d refused tithes; gives the sitting executor +%.1f in its vote",
		ch.AllianceLoyalty, ch.TitheTolerance, ch.ExecutorIncumbency),
		"loyalty — a devoted CEO is the last to leave and the last to unseat; patience sets how many missed tithes it bears")
	ch.MaxSovTargets = bendInt(p.trait(TraitLeadership), 1, 2, 4)
	why("Names for the alliance", fmt.Sprintf("up to %d sovereignty targets at once", ch.MaxSovTargets),
		"leadership — how many systems an executor asks its members to hold together")
	ch.StationPolicy = stationPolicyFor(ch.Xenophobia, p.violentPole(), posPole(p.trait(TraitLeadership)), p.greedyPole())
	why("A station it owns", stationPolicyLabel(ch.StationPolicy),
		"tolerance, aggression and leadership close the doors to all but blues; greed keeps them open for trade")

	// ── war ─────────────────────────────────────────────────────────────
	// Polarity: LOW caution careful, LOW patience unhurried, HIGH greed and aggression.
	ch.WarAt = bend(p.trait(TraitAggression), 9.0, 6.0, 3.5)
	ch.MaxWars = bendInt(p.trait(TraitAggression), 1, 1, 3)
	why("Goes to war", fmt.Sprintf("over a quarrel worth %.1f, at most %d at once", ch.WarAt, ch.MaxWars),
		"aggression — a violent CEO takes a grievance to CONCORD, a pacifist one almost never")
	ch.WarMaxOdds = bend(p.trait(TraitCaution), 0.8, 1.2, 2.0)
	why("Fights its own wars", fmt.Sprintf("against up to %.1f× its own strength", ch.WarMaxOdds),
		"caution — beyond that it hires mercenaries rather than fight")
	ch.WarPatienceWeeks = bend(p.trait(TraitPatience), 6, 4, 2)
	ch.WarPainRatio = bend(p.trait(TraitAggression), 1.2, 2.0, 3.5)
	why("Gives up a war", fmt.Sprintf("after %.0f weeks that do not pay, or losing %.1f ISK per ISK killed", ch.WarPatienceWeeks, ch.WarPainRatio),
		"patience sets how long it waits, aggression how much pain it takes")
	ch.WarChestFrac = bend(p.trait(TraitCaution), 0.15, 0.25, 0.40)
	ch.WarRunwayWeeks = bend(p.trait(TraitCaution), 4, 3, 2)
	why("War chest", fmt.Sprintf("%.0f%% of free capital, %.0f weeks of bills set aside first", ch.WarChestFrac*100, ch.WarRunwayWeeks),
		"caution — a careful CEO never bets the corporation on a war")
	ch.WarFootingMax = bend(p.trait(TraitLeadership), 0.20, 0.35, 0.50)
	why("War footing", fmt.Sprintf("up to %.0f%% of the directive to fighting", ch.WarFootingMax*100),
		"leadership — how hard it can turn its members out when the corporation is at war")
	ch.ConcedeExp = bend(p.trait(TraitPatience), 3.0, 1.5, 0.6)
	ch.OpenFrac = bend(p.trait(TraitGreed), 0.8, 0.65, 0.45)
	why("Haggles", fmt.Sprintf("opens at %.0f%% of its limit, concedes with exponent %.1f", ch.OpenFrac*100, ch.ConcedeExp),
		"envy opens low, patience holds out — an unhurried CEO gives ground only at the deadline")
	ch.MercMargin = bend(p.trait(TraitGreed), 0.05, 0.15, 0.35)
	ch.MercGreedPull = bend(p.trait(TraitGreed), 0, 0.1, 0.35)
	ch.MercMaxOdds = bend(p.trait(TraitCaution), 0.8, 1.3, 2.2)
	ch.MercBonusRate = bend(p.trait(TraitGreed), 0.05, 0.10, 0.20)
	if ch.Mercenary {
		why("Sells its guns", fmt.Sprintf("margin %.0f%%, bonus %.0f%% of kills, odds up to %.1f×", ch.MercMargin*100, ch.MercBonusRate*100, ch.MercMaxOdds),
			"envy prices the work and the client's wallet, caution the fights it will take")
	}
	return ch
}

const (
	// ceoMercenaryLean is how far toward the mercenary end of loyalty a CEO must
	// sit before it may sell its guns (the wary step, tolerance.go).
	ceoMercenaryLean = 0.25
	// ceoMercenaryFloor and ceoMercenaryCeil are the chance at that line and at
	// the pole.
	ceoMercenaryFloor = 0.70
	ceoMercenaryCeil  = 1.00
	// ceoMercenaryDirective is the share of the directive fighting must hold
	// for an outfit with no armed tag to count as a fighting one.
	ceoMercenaryDirective = 1.0 / 3
	// ceoMercenarySalt keeps this roll independent of every other stateless
	// draw keyed on a charID.
	ceoMercenarySalt = 0x6D657263 // "merc"
)

// ceoCombatOriented reports whether a charter describes a fighting outfit:
// it wears an armed tag, or its directive sends a real share of its members
// out to fight.
func ceoCombatOriented(ch Charter) bool {
	for _, t := range ceoRelArmedTags {
		if t != tagMercenary && containsStr(ch.Tags, t) {
			return true
		}
	}
	return ch.Directive[ActCombat]+ch.Directive[ActSecurity]+ch.Directive[ActPiracy] >= ceoMercenaryDirective
}

// ceoMercenaryChance is how likely this CEO is to run its corporation as a
// mercenary outfit: zero unless it leans mercenary AND runs a fighting outfit,
// then rising from ceoMercenaryFloor at the lean line to ceoMercenaryCeil at
// the pole. Pure.
func ceoMercenaryChance(p pilotIdentity, ch Charter) float64 {
	lean := p.mercenaryPole()
	if lean < ceoMercenaryLean || !ceoCombatOriented(ch) {
		return 0
	}
	f := (lean - ceoMercenaryLean) / (1 - ceoMercenaryLean)
	return ceoMercenaryFloor + (ceoMercenaryCeil-ceoMercenaryFloor)*f
}

// ceoMercenaryRoll is this CEO's own draw in [0, 1), stateless and keyed on
// the character: the charter resolves every think, and a fresh roll each time
// would retag the corporation. The chance only rises with the lean, so a CEO
// can cross into the tag, never out of it.
func ceoMercenaryRoll(charID int64) float64 {
	return (splitmixUnit(uint64(charID)*noiseGolden+ceoMercenarySalt) + 1) / 2
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
