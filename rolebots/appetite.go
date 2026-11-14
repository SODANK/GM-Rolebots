package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/seclaw"
)

// Appetite: how an idle bot decides what to do.
//
// Picking work is a pricing question over the bot's information base (standings, traits, regions,
// ships), as picking a belt is (scoring.go). Per activity three terms multiply:
//
//	Disposition: what the pilot wants, from its traits alone.
//	Opportunity: what the world offers now. Zero means cannot, the only hard gate.
//	Satiation:   how worn out this work is for the pilot lately, so work rotates without a timer.
//
// The pick is a softmax draw at the pilot's temperature. A new activity is one ActivityDef below.

// ActivityDef is everything the chooser needs to know about one activity.
type ActivityDef struct {
	Slug  string
	Label string
	Job   string
	// Plan starts the activity from idle. Configs are derived from the
	// pilot's own flags (activitycfg.go), not stored per activity.
	Plan func(m *Mod, ctx context.Context, b *botRun) error
	// Disposition is the trait-only pull, ~1.0 = indifferent.
	Disposition func(tn Tuning) float64
	// Opportunity is the world's multiplier and the prose explaining it.
	// 0 means the bot cannot do this at all right now.
	Opportunity func(m *Mod, ctx context.Context, b *botRun) (float64, string)
	// HalfLife overrides how fast this work wears thin. Nil means the pilot's own SatiationHalfLifeSec.
	// A calling (industry) overrides it so the anti-monotony term does not pull a devoted pilot off its line.
	HalfLife func(tn Tuning) float64
	// Tactic marks an entry that is a trip of another activity, not a chooser option. It stays in the
	// catalogue for the skill planner, the arms and the CEO directive, which read its disposition, job
	// and half-life. scoreActivities skips it.
	Tactic bool
	// Members makes this entry an umbrella over several member activities (industry: manufacturing,
	// mining, ice). The members stay as Tactic entries because stored phases and planners key on them.
	// Satiation is the sum of their worked tallies.
	Members []string
	// Composite, set with Members, prices the umbrella: one score per sub-trip (industrytrip.go), the
	// best being the umbrella's. The planner draws its trip from it too, so chooser and trip agree.
	Composite func(m *Mod, ctx context.Context, b *botRun, need float64) []tripScore
}

// memberWorked is the decayed working time that counts against this entry's
// appetite: its own slug, or the sum over its members for an umbrella.
func (d ActivityDef) memberWorked(w map[string]float64) float64 {
	if len(d.Members) == 0 {
		return w[d.Slug]
	}
	t := 0.0
	for _, s := range d.Members {
		t += w[s]
	}
	return t
}

// covers reports whether a stored activity slug is this entry or one of its
// members (the stickiness bonus and LastActivity may name either).
func (d ActivityDef) covers(slug string) bool {
	if slug == "" {
		return false
	}
	if slug == d.Slug {
		return true
	}
	for _, s := range d.Members {
		if s == slug {
			return true
		}
	}
	return false
}

// activityCatalog is the single source of truth for what work exists. Agent missions are not an
// entry: they are a trip of the combat patrol (combattactic.go tacticMission, missions.go), flown
// in the combat ship.
var activityCatalog = []ActivityDef{
	{
		// Mining is owned by the Toil trait (tuning.go). It is a Tactic: a trip of the industry umbrella,
		// weighed there by this entry's terms. The slug names a mining stint in the phases, fleet seats,
		// the skill planner and the broke ladder. Satiation stretches with the trait (careerHalfLife).
		Slug: ActMining, Label: "mining", Job: JobMining, Tactic: true,
		Plan:        (*Mod).miningPlan,
		Disposition: func(tn Tuning) float64 { return tn.MiningAppetite },
		Opportunity: opportunityFromFits(JobMining),
		HalfLife:    func(tn Tuning) float64 { return careerHalfLife(tn, tn.CareerMining, careerStretch) },
	},
	{
		// The combat patrol (combatplan.go). The planner draws a trip from the tactic table; one of its five
		// is a bounty.
		Slug: ActCombat, Label: "combat patrol", Job: JobCombat,
		Plan:        (*Mod).combatPlan,
		Disposition: func(tn Tuning) float64 { return tn.ViolenceAppetite },
		Opportunity: combatOpportunity,
		HalfLife:    func(tn Tuning) float64 { return careerHalfLife(tn, tn.CareerCombat, careerStretchCombat) },
	},
	{
		// Piracy. Disposition is a product of violence and lawlessness (tuning.go), so it is 0 for every
		// lawful or pacifist pilot. It is a Tactic: robbery is a trip in the combat tactic table
		// (tacticRobbery, robberyWeight). The slug names a robbery in progress for the piracy tasks, the
		// loot and fence resume, the fleet kind and the directive weights. Satiation is stretched.
		Slug: ActPiracy, Label: "piracy", Job: JobPiracy, Tactic: true,
		Plan:        (*Mod).piracyPlan,
		Disposition: func(tn Tuning) float64 { return tn.PiracyAppetite },
		Opportunity: piracyOpportunity,
		HalfLife:    piracyHalfLife,
	},
	{
		// Trading on the pilot's own capital: the greedy pole of Envy, and the
		// desk end of Toil.
		Slug: ActTrade, Label: "trading", Job: JobTrade,
		Plan:        (*Mod).tradePlan,
		Disposition: func(tn Tuning) float64 { return tn.TradeAppetite },
		Opportunity: opportunityFromFits(JobTrade),
		HalfLife:    func(tn Tuning) float64 { return careerHalfLife(tn, tn.CareerTrade, careerStretch) },
	},
	{
		// Courier contract acceptance: pick up a hauling job, fly to the pickup, accept, haul to the drop,
		// deliver. Collateral is at risk, so the task is non-interruptible and uses a trade ship. It is the
		// generous pole of Envy, with its own appetite; contracts come mostly from outsourceHaul (courier_issue.go).
		Slug: ActCourier, Label: "courier hauling", Job: JobTrade,
		Plan:        (*Mod).courierAcceptPlan,
		Disposition: func(tn Tuning) float64 { return tn.HaulAppetite },
		Opportunity: courierOpportunity,
		HalfLife:    func(tn Tuning) float64 { return careerHalfLife(tn, tn.CareerHaul, careerStretch) },
	},
	{
		// Industry: the umbrella over mining (rock or ice), manufacturing and research. The chooser offers
		// one industry entry and industrytrip.go prices the sub-trips with the member entries' own terms.
		// Disposition and Opportunity are manufacturing's, because the skill planner, the arms and the CEO
		// directive read this entry as how much of a builder the pilot is. Every pilot builds: SupplyDrive
		// floors at 0.15 instead of 0 (tuning.go), so a consumer's sliver of it loses every draw.
		Slug: ActIndustry, Label: "industry", Job: JobTrade,
		// Plan is manufacturing's here. The umbrella's Composite is attached in industrytrip.go init(),
		// because an initialiser that reads activityCatalog would be an initialisation cycle (as
		// combattactic.go does for robberyWeightFn).
		Plan:        (*Mod).industryPlan,
		Disposition: func(tn Tuning) float64 { return tn.SupplyDrive * tn.IndustryAppetite },
		Opportunity: industryOpportunity,
		Members:     []string{ActIndustry, ActMining, ActIce},
		// Industry is a calling: a strong factory shift, mining or research calling stretches the half-life.
		// It only ever stretches.
		HalfLife: industryHalfLife,
	},
}

// activityDefFor looks an activity up by slug.
func activityDefFor(slug string) (ActivityDef, bool) {
	for _, d := range activityCatalog {
		if d.Slug == slug {
			return d, true
		}
	}
	return ActivityDef{}, false
}

// implementedActivity reports whether an activity slug has a planner behind it, by catalog lookup.
func implementedActivity(slug string) bool {
	_, ok := activityDefFor(slug)
	return ok
}

// ── appetite constants (defaults; Tuning bends them per pilot) ──────────────

const (
	// satiationHalfLifeSec is how much decayed working time halves an activity's pull: about one hour of
	// one job lets a same-appetite alternative start winning draws.
	satiationHalfLifeSec = 14400
	// pickTemperature is the softmax noise. 0.35 lets a strongly
	// out-of-character option through a few percent of the time.
	pickTemperature = 0.35
	// grudgeJumpWorth prices a point of negative standing toward whoever
	// spawns in a hunting ground, in jumps of extra travel the grudge is
	// worth.
	grudgeJumpWorth = 1.5
	// stickinessBonus is the current activity's multiplier: finishing the
	// belt beats flip-flopping. Satiation erodes it on its own.
	stickinessBonus = 1.25
	// ownedShipOpportunity is the boost for work the bot is already equipped for, so a combat pilot
	// mostly means a pilot who owns a combat ship.
	ownedShipOpportunity = 1.2
	// careerStretch is how much longer a career takes to wear thin at the pole of its owning trait:
	// x(1 + 2), three times. Combat stretches a little less.
	careerStretch       = 2.0
	careerStretchCombat = 1.5
)

// careerHalfLife is the pilot's satiation half-life stretched by how far out
// it sits on the trait that owns this career (0 = not at all, 1 = the pole).
func careerHalfLife(tn Tuning, strength, stretch float64) float64 {
	return tn.SatiationHalfLifeSec * (1 + stretch*clamp01(strength))
}

// ── satiation ───────────────────────────────────────────────────────────────

// settleWork folds the session that just ended into the pilot's worked
// tally and decays the whole tally toward zero. Called once per plan, so
// the arithmetic is lazy: nothing ticks while the bot is out working.
func settleWork(d *StateData, current string, now int64, halfLifeSec float64) {
	// Ice is mining done in a barge (miningtrip.go): one tally, one tiredness.
	if current == ActIce {
		current = ActMining
	}
	if current != "" && d.ActivityStartedAt > 0 && now > d.ActivityStartedAt {
		if d.Worked == nil {
			d.Worked = map[string]float64{}
		}
		d.Worked[current] += float64(now - d.ActivityStartedAt)
	}
	d.ActivityStartedAt = 0
	if d.WorkedAt > 0 && now > d.WorkedAt && len(d.Worked) > 0 {
		decay := math.Exp2(-float64(now-d.WorkedAt) / halfLifeSec)
		for k, v := range d.Worked {
			v *= decay
			if v < 60 { // under a minute of memory is no memory
				delete(d.Worked, k)
				continue
			}
			d.Worked[k] = v
		}
	}
	d.WorkedAt = now
}

// satiationFactor is the multiplier tiredness of one kind of work puts on
// its score. 1.0 fresh; 0.5 after a half-life's worth of recent work.
func satiationFactor(workedSec, halfLifeSec float64) float64 {
	return 1 / (1 + workedSec/halfLifeSec)
}

// activityHalfLife is how fast this work wears thin for this pilot: the activity's own answer when it
// has one, else the pilot's. It guards against zero, which would divide by zero in satiationFactor.
func activityHalfLife(def ActivityDef, tn Tuning) float64 {
	h := tn.SatiationHalfLifeSec
	if def.HalfLife != nil {
		if v := def.HalfLife(tn); v > 0 {
			h = v
		}
	}
	if h <= 0 {
		h = satiationHalfLifeSec
	}
	return h
}

// ── opportunity probes ──────────────────────────────────────────────────────

// opportunityFromFits is the shared capability probe: does the pilot own a live hull for the role
// (best), or is there a fit its skills allow (workable). It spends no money; affordability is the
// planner's.
func opportunityFromFits(job string) func(m *Mod, ctx context.Context, b *botRun) (float64, string) {
	return func(m *Mod, ctx context.Context, b *botRun) (float64, string) {
		owned, err := m.ownsShipForJob(ctx, b.CharID, job)
		if err == nil && owned != "" {
			return ownedShipOpportunity, "already owns " + owned
		}
		flyable, err := m.canWorkJob(ctx, b.CharID, job)
		if err != nil {
			return 0, "fit check failed: " + err.Error()
		}
		if !flyable {
			return 0, "nothing in the fits it could take " + jobLabel(job)
		}
		return 1, "would have to buy a ship first"
	}
}

// combatOpportunity is the combat probe plus the pilot's opinions: a
// grudge against someone who spawns in belts makes hunting genuinely more
// attractive, and a roster-mate having just been mauled somewhere nearby
// does not (fear is priced per system by the hunt search, not here).
func combatOpportunity(m *Mod, ctx context.Context, b *botRun) (float64, string) {
	f, why := opportunityFromFits(m.jobForPilot(ctx, b.CharID, ActCombat))(m, ctx, b)
	if f == 0 {
		return 0, why
	}
	// The employer's combat wing has a size (corpwing.go). Refusing here, where the chooser can say
	// cannot, sends the pilot to other work instead of stopping it later.
	if ok, reason := m.wingAllows(ctx, b); !ok {
		return 0, reason
	}
	// The deepest grudge against a pirate spawn faction sweetens the work.
	worst := 0.0
	for _, id := range m.pirateFactionIDs(ctx) {
		if v := b.Profile.StandingTo(id); v < worst {
			worst = v
		}
	}
	if worst <= StandingDisliked {
		f *= 1 + (-worst/10)*0.4 // a −10 grudge is a 40% kicker
		why += "; carries a grudge worth hunting"
	}
	// A named pilot on the grace ledger (grace_ledger.go) does the same, more so for a vindictive pilot.
	if w := graceWorstFor(b.CharID); w <= StandingDisliked {
		f *= 1 + (-w/10)*0.4*(0.5+b.Tune.Vindictiveness)
		why += "; has a name to settle"
	}
	// A corporation at war fields its wing (corprel.go). A multiplier, not a bypass of wingAllows: a war
	// makes the seats worth taking.
	if relCorpAtWar(b.CorpID) {
		f *= warOpportunityMul()
		why += "; the corporation is at war"
	}
	return f, why
}

// ownsShipForJob names the hull the pilot holds for a fit of the job ("" when it holds none). Cheap:
// one query plus an existence check.
func (m *Mod) ownsShipForJob(ctx context.Context, charID int64, job string) (string, error) {
	// A hull counts only if what this pilot can run on it still does the work: the same Vexor answers
	// yes to combat and no to mining.
	s, t, err := m.shipForJob(ctx, charID, job)
	if err != nil || s == nil {
		return "", err
	}
	name := t.Fit.HullName
	if name == "" {
		name = t.Name
	}
	return "a " + name, nil
}

// canFlyJob lives in fitselect.go as canWorkJob: whether any fit in the module, cut down to what this
// pilot can run, still does this work.

var (
	pirateIDsOnce sync.Once
	pirateIDs     []int64
)

// pirateFactionIDs resolves the pirate spawn factions by name, cached for
// the life of the process (static data).
func (m *Mod) pirateFactionIDs(ctx context.Context) []int64 {
	pirateIDsOnce.Do(func() {
		rows, err := m.db.QueryContext(ctx,
			`SELECT factionID, factionName FROM chrFactions`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var name string
			if rows.Scan(&id, &name) == nil && pirateFactionNames[normFactionName(name)] {
				pirateIDs = append(pirateIDs, id)
			}
		}
	})
	return pirateIDs
}

// ── scoring & the draw ──────────────────────────────────────────────────────

// activityScore is one activity priced for one pilot right now.
type activityScore struct {
	Def   ActivityDef
	Drive float64 // disposition (traits only)
	Opp   float64 // world multiplier (0 = cannot)
	Sat   float64 // 1 fresh … →0 worn out
	Need  float64 // wallet pressure: 1 comfortable, >1 skint and earning
	// Directive is the pull of the pilot's corporation's standing directive
	// (corpceo_directive.go): 1 for everybody whose corporation has no bot
	// CEO, above 1 for work the CEO asked for and below it for work it did not.
	Directive float64
	Score     float64
	Why       string // opportunity prose, for the log and the panel
}

// ── need: what a thin wallet does to an appetite ────────────────────────────
// Poverty is a multiplier in the softmax, not a gate, so a poor bot leans toward earning and the draw keeps its noise.
const (
	// needComfortISK is where money stops mattering: above it the term is 1.0. It is four times the
	// wallet floor (broke.go), enough to replace a ship three times.
	needComfortISK = 4 * walletFloorISK
	// needMaxPull is what an empty wallet is worth to work that pays. It is modest: dispositions span
	// about an order of magnitude, so it bends a marginal choice without overruling a temperament.
	needMaxPull = 3.0
	// needSpenderDamp is applied to work that costs money before it earns (manufacturing buys a
	// blueprint and materials up front). It damps rather than blocks, so a part-paid job still finishes.
	needSpenderDamp = 0.5
)

// needFactor is how hard this balance pushes toward earning: 1.0 at comfort,
// rising linearly to needMaxPull at an empty wallet.
func needFactor(balance float64) float64 {
	if balance >= needComfortISK {
		return 1
	}
	if balance <= 0 {
		return needMaxPull
	}
	return 1 + (needMaxPull-1)*(1-balance/needComfortISK)
}

// needPull applies the pressure to one activity: earning work is lifted and spending work is damped,
// in proportion to how thin the wallet is. Which work earns is the brokeEarners list (broke.go).
// Piracy is not in it; that choice is left to the lawlessness trait.
func needPull(slug string, need float64) float64 {
	if need <= 1 {
		return 1
	}
	for _, earner := range brokeEarners {
		if slug != earner {
			continue
		}
		// Trading earns only on a float. Under tradeSeedFloatISK a pilot cannot buy a first cargo, so it is
		// damped like other spending work (tradecapital.go).
		if slug == ActTrade && need > needFactor(tradeSeedFloatISK) {
			return 1 - (1-needSpenderDamp)*clamp01(
				(need-needFactor(tradeSeedFloatISK))/(needMaxPull-needFactor(tradeSeedFloatISK)))
		}
		return need
	}
	if slug == ActIndustry {
		// Damp toward indifference, never past it.
		return 1 - (1-needSpenderDamp)*clamp01((need-1)/(needMaxPull-1))
	}
	return 1
}

const (
	// uncoveredRiskDamp scales work where pods die (piracy) for a pilot whose clone
	// does not cover its SP: a pod death would cost a skill level as well as the clone.
	uncoveredRiskDamp = 0.3
	// uncoveredEarnLift lifts earning work for the same pilot, so it saves for cover.
	uncoveredEarnLift = 1.5
)

// uncoveredPull is the appetite multiplier for one activity of a pilot without clone cover:
// 1 when covered. Pure so the rule is testable without a roster.
func uncoveredPull(slug string, uncovered bool) float64 {
	if !uncovered {
		return 1
	}
	switch slug {
	case ActPiracy:
		return uncoveredRiskDamp
	}
	for _, earner := range brokeEarners {
		if slug == earner {
			return uncoveredEarnLift
		}
	}
	return 1
}

// outlawRehabPullMul is how much louder combat calls an outlaw. A pilot with other doors shut by the
// security ceiling would otherwise carry a record of -7 for weeks.
const outlawRehabPullMul = 2.5

// outlawRehabPull is the combat multiplier for a pilot with this security
// record: 1 above the outlaw line, outlawRehabPullMul at or below it. Pure so
// the rule is testable without a roster.
func outlawRehabPull(secStatus float64) float64 {
	if secStatus <= seclaw.OutlawStatus {
		return outlawRehabPullMul
	}
	return 1
}

// scoreActivities prices the whole catalog for this bot. Satiation reads the already-settled Worked
// tally, so call settleWork first.
func (m *Mod) scoreActivities(ctx context.Context, b *botRun) []activityScore {
	tn := b.Tune
	// One read per planning pass. An unreadable wallet counts as comfortable, so it cannot push the
	// roster into mining.
	need := 1.0
	if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err == nil {
		need = needFactor(bal)
	}
	out := make([]activityScore, 0, len(activityCatalog))
	for _, def := range activityCatalog {
		if def.Tactic {
			continue // a trip of another activity, weighed in its tactic table
		}
		var s activityScore
		if def.Composite != nil {
			// An umbrella: its sub-trips carry their own need, directive, arm and war-hold terms
			// (industrytrip.go); the score is the best of them times the shared satiation.
			s = m.scoreUmbrella(ctx, b, def, need)
		} else {
			s = activityScore{Def: def, Drive: def.Disposition(tn)}
			s.Opp, s.Why = def.Opportunity(m, ctx, b)
			s.Sat = satiationFactor(def.memberWorked(b.State.Data.Worked), activityHalfLife(def, tn))
			s.Need = needPull(def.Slug, need)
			// The corporation's pull: the CEO's directive over the membership, then the member's own arm
			// (corparm.go). Both are 1 outside a bot-run corporation.
			s.Directive = m.directiveTerm(ctx, b, def.Slug) * m.armTerm(b, def.Slug)
			s.Score = s.Drive * s.Opp * s.Sat * s.Need * s.Directive
			// An outlaw's record rises only by killing rats (passive forgiveness is off), so combat is pulled up
			// while the record is below the line.
			if def.Slug == ActCombat {
				if pull := outlawRehabPull(b.SecStatus); pull > 1 {
					s.Score *= pull
					s.Why += fmt.Sprintf("; its record is %.1f and killing rats is the only way back", b.SecStatus)
				}
			}
			// At war, with the enemy in this very system: the work done out in
			// space loses to the work done from a station (corpceo_war.go).
			if hold := warHoldTerm(b.CorpID, b.SystemID, def.Slug); hold < 1 {
				s.Score *= hold
				s.Why += "; a war enemy is in the system"
			}
			// A pilot whose clone does not cover its SP stays out of pod risk and earns the
			// price of cover (clone.go).
			if pull := uncoveredPull(def.Slug, cloneUncovered(b.CharID)); pull != 1 {
				s.Score *= pull
				if pull < 1 {
					s.Why += "; its clone does not cover its skills"
				} else {
					s.Why += "; it needs ISK for a clone that covers its skills"
				}
			}
		}
		if def.covers(b.State.Data.LastActivity) {
			s.Score *= stickinessBonus
		}
		out = append(out, s)
	}
	return out
}

// pickActivity draws from the scored options, softmax over score with the
// pilot's temperature. Zero-score options (no fit, appetite floored by
// satiation to nothing) are out of the draw entirely. ok=false when
// nothing at all is runnable.
func pickActivity(scores []activityScore, tn Tuning) (activityScore, bool) {
	live := scores[:0:0]
	for _, s := range scores {
		if s.Score > 0 {
			live = append(live, s)
		}
	}
	if len(live) == 0 {
		return activityScore{}, false
	}
	if len(live) == 1 {
		return live[0], true
	}
	// Softmax relative to the best, so temperature means the same thing
	// regardless of absolute score scale.
	best := live[0].Score
	for _, s := range live[1:] {
		if s.Score > best {
			best = s.Score
		}
	}
	t := tn.PickTemperature
	if t < 0.01 {
		t = 0.01
	}
	weights := make([]float64, len(live))
	total := 0.0
	for i, s := range live {
		weights[i] = math.Exp((s.Score - best) / t)
		total += weights[i]
	}
	roll := rand.Float64() * total
	for i, w := range weights {
		if roll < w {
			return live[i], true
		}
		roll -= w
	}
	return live[len(live)-1], true
}

// appetiteSummary is one line of why-prose for the log: what won and what
// it beat.
func appetiteSummary(pick activityScore, scores []activityScore) string {
	sorted := append([]activityScore(nil), scores...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	out := fmt.Sprintf("felt like %s (drive %.2f, opportunity %.2f, freshness %.2f)",
		pick.Def.Label, pick.Drive, pick.Opp, pick.Sat)
	// Only mentioned when it moved something. A line that prints "need 1.00"
	// on every comfortable pilot is noise on a board that already scrolls.
	if pick.Need != 1 {
		out += fmt.Sprintf(", and a thin wallet %s that %.2f×",
			map[bool]string{true: "pushed it toward", false: "pulled it away from"}[pick.Need > 1],
			pick.Need)
	}
	if pick.Directive != 1 && pick.Directive != 0 {
		out += fmt.Sprintf(", and the corporation's directive %s that %.2f×",
			map[bool]string{true: "pushed it toward", false: "pulled it away from"}[pick.Directive > 1],
			pick.Directive)
	}
	for _, s := range sorted {
		if s.Def.Slug == pick.Def.Slug {
			continue
		}
		if s.Score <= 0 {
			out += fmt.Sprintf("; %s not an option (%s)", s.Def.Label, s.blockedBy())
		}
	}
	return out
}

// blockedBy names the term that actually zeroed this activity. Why is the opportunity probe's answer,
// so a zero disposition would otherwise report a capability reason.
func (s activityScore) blockedBy() string {
	switch {
	case s.Opp <= 0:
		return s.Why
	case s.Drive <= 0:
		return "this pilot has no appetite for it at all"
	case s.Sat <= 0:
		return "worn out on it for now"
	}
	return s.Why
}
