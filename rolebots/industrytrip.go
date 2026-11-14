package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"

	evedb "gurimoon/db"
)

// An industry stint is one activity with a trip table under it, as combat has (combattactic.go):
// the chooser offers one "industry", this file prices the sub-trips and the planner draws one.
//
//	mining     rock or ice, whichever pays        miningTripPlan
//	build      a manufacturing job at the factory industryPlan
//	research   a lab job on a print worth improving labworkTrip
//
// Trips are priced with their member activity's disposition, opportunity, need, directive and arm
// terms; the umbrella score is the best trip's score times one shared satiation. At draw time the
// scores are bent by horizon (PlanLean in tuning.go tilts long against short trips) and by recency.

// ActResearch is the key the R&D arm (corparm.go) and the research trip share. It is not a
// catalogue activity: nothing is drawn, stored or planned under it. It lets an arm name research
// in the slug-keyed vocabulary of armActivities, armMultiplier and armFit.
const ActResearch = "research"

const (
	indTripRock     = "rock"
	indTripBuild    = "build"
	indTripResearch = "research"

	// indTripRepeatPenalty is what having just flown a trip costs it: a penalty rather than an
	// exclusion, as in combat (tacticRepeatPenalty), so a pilot with one open trip can fly it twice.
	indTripRepeatPenalty = 0.35
	// indHorizonSpan is how far PlanLean tilts the draw: a pilot at either
	// extreme weights the trips that suit its horizon at 1.4x and the others at
	// 0.6x; the middle weights everything alike.
	indHorizonSpan = 0.4
)

// industryTrip is one entry of the table.
type industryTrip struct {
	Slug  string
	Label string
	// Act is the member activity whose directive, arm, need and war-hold terms
	// price this trip. Research has no activity of its own; it rides industry's.
	Act string
	// ArmKey, when set, is the slug the corporation arm pulls this trip by,
	// where it differs from Act: research rides industry's directive and wallet
	// pressure but belongs to the R&D arm, not the Industry arm.
	ArmKey string
	// Long marks the long-horizon trips (see the header).
	Long bool
	// Plan starts the trip from idle.
	Plan func(m *Mod, ctx context.Context, b *botRun) error
}

var industryTrips = []industryTrip{
	// One mining trip: rock or ice is decided inside it (miningTripPlan), never
	// by the draw. A barge mines dirt or ice; it is the same work.
	{Slug: indTripRock, Label: "mining", Act: ActMining, Plan: (*Mod).miningTripPlan},
	{Slug: indTripBuild, Label: "manufacturing", Act: ActIndustry, Long: true, Plan: (*Mod).industryPlan},
	{Slug: indTripResearch, Label: "research", Act: ActIndustry, ArmKey: ActResearch, Long: true,
		Plan: (*Mod).researchTripPlan},
}

// armSlug is the key the arm layer prices this trip by.
func (t industryTrip) armSlug() string {
	if t.ArmKey != "" {
		return t.ArmKey
	}
	return t.Act
}

// researchOpen reports whether this pilot may start research: it is a research
// career, or it sits in its corporation's R&D arm, which exists to do the
// corporation's research whatever the individual's temperament.
func researchOpen(b *botRun) bool {
	if researchCareer(b.Tune) {
		return true
	}
	if b.CorpID == 0 {
		return false
	}
	s, ok := armOf(b.CharID, b.CorpID)
	return ok && s.Arm == armResearch
}

// tripScore is one trip priced for one pilot right now.
type tripScore struct {
	Trip      industryTrip
	Drive     float64
	Opp       float64
	Need      float64
	Directive float64
	Score     float64
	Why       string
}

func init() {
	// Bound here and not in the catalogue literal: both reach the directive
	// code, which reads activityCatalog (see the note on the entry).
	for i := range activityCatalog {
		if activityCatalog[i].Slug == ActIndustry {
			activityCatalog[i].Plan = (*Mod).industryUmbrellaPlan
			activityCatalog[i].Composite = (*Mod).industryTripScores
		}
	}
}

// industryHalfLife is how fast industry wears thin for this pilot: the slowest
// of the three callings' own half-lives, since the umbrella tallies them
// together. Each only ever stretches the pilot's base.
func industryHalfLife(tn Tuning) float64 {
	h := tn.SatiationHalfLifeSec * (1 + tn.SupplyDrive)
	if v := careerHalfLife(tn, tn.CareerMining, careerStretch); v > h {
		h = v
	}
	if v := careerHalfLife(tn, tn.CareerResearch, careerStretch); v > h {
		h = v
	}
	return h
}

// drive is the trip's trait-only pull.
func (t industryTrip) drive(tn Tuning) float64 {
	if t.Slug == indTripResearch {
		return tn.ResearchAppetite
	}
	if def, ok := activityDefFor(t.Act); ok {
		return def.Disposition(tn)
	}
	return 0
}

// opportunity is the world's multiplier for the trip.
func (t industryTrip) opportunity(m *Mod, ctx context.Context, b *botRun) (float64, string) {
	if t.Slug == indTripResearch {
		return researchTripOpportunity(m, ctx, b)
	}
	if t.Slug == indTripRock {
		return miningTripOpportunity(m, ctx, b)
	}
	if def, ok := activityDefFor(t.Act); ok {
		return def.Opportunity(m, ctx, b)
	}
	return 0, "no such activity"
}

// researchTripOpportunity is the opportunity for a researcher with a free lab slot. The career
// is the gate: the umbrella must not hand a lab to a pilot just because it owns a blueprint.
func researchTripOpportunity(m *Mod, ctx context.Context, b *botRun) (float64, string) {
	if !researchOpen(b) {
		return 0, "research is not its calling"
	}
	d := b.State.Data
	if d.LabJobID != 0 || d.LabBPItemID != 0 {
		return 0, "a lab errand is already under way"
	}
	if until := d.LabFailAt; until > clock.Unix() {
		return 0, "nothing worth researching last time it looked"
	}
	if free, err := m.labFreeSlots(ctx, b); err == nil && free == 0 {
		return 0, "every lab slot it can run is already busy"
	}
	return 1, "its lab has a free slot"
}

// researchTripPlan starts a research stint. Reports the same refusal as every
// planner that has nothing to do, so the umbrella moves on to its next trip.
func (m *Mod) researchTripPlan(ctx context.Context, b *botRun) error {
	// The corporation's board first: research for its industrialists uses the corporation's own
	// prints at its own factory, for a wage. corpResearchArm answers false for a solo researcher.
	if handled, err := m.corpResearchArm(ctx, b); err != nil {
		return err
	} else if handled {
		return nil
	}
	handled, err := m.labworkTrip(ctx, b)
	if err != nil {
		return err
	}
	if !handled {
		return fmt.Errorf("%w: nothing it could put in a lab", errNoFitAvailable)
	}
	return nil
}

// tripNeed is the wallet pressure on one trip. Mining earns, so a thin wallet
// lifts it; building and researching spend before they earn, so it damps them.
func tripNeed(t industryTrip, need float64) float64 {
	return needPull(t.Act, need)
}

// industryTripScores prices every trip. It is the umbrella's Composite and the
// planner's draw input, so the chooser and the trip pick cannot disagree.
func (m *Mod) industryTripScores(ctx context.Context, b *botRun, need float64) []tripScore {
	out := make([]tripScore, 0, len(industryTrips))
	for _, t := range industryTrips {
		ts := tripScore{Trip: t, Drive: t.drive(b.Tune)}
		if ts.Drive <= 0 {
			ts.Why = "no appetite for it"
			out = append(out, ts)
			continue
		}
		ts.Opp, ts.Why = t.opportunity(m, ctx, b)
		ts.Need = tripNeed(t, need)
		ts.Directive = m.directiveTerm(ctx, b, t.Act) * m.armTerm(b, t.armSlug())
		ts.Score = ts.Drive * ts.Opp * ts.Need * ts.Directive
		// At war with the enemy in this very system: work done out in space
		// loses to work done from a station, per member activity.
		if hold := warHoldTerm(b.CorpID, b.SystemID, t.Act); hold < 1 {
			ts.Score *= hold
			ts.Why += "; a war enemy is in the system"
		}
		out = append(out, ts)
	}
	return out
}

// scoreUmbrella turns the trips into one activity score: the best trip, times
// the umbrella's shared satiation.
func (m *Mod) scoreUmbrella(ctx context.Context, b *botRun, def ActivityDef, need float64) activityScore {
	trips := def.Composite(m, ctx, b, need)
	// The best score wins; with nothing open, the strongest appetite is the one
	// worth explaining in the log.
	best := -1
	for i := range trips {
		switch {
		case best < 0:
			best = i
		case trips[i].Score > trips[best].Score:
			best = i
		case trips[i].Score <= 0 && trips[best].Score <= 0 && trips[i].Drive > trips[best].Drive:
			best = i
		}
	}
	s := activityScore{Def: def}
	if best < 0 {
		s.Why = "no industry trips"
		return s
	}
	t := trips[best]
	s.Drive, s.Opp, s.Need, s.Directive = t.Drive, t.Opp, t.Need, t.Directive
	s.Why = t.Trip.Label + ": " + t.Why
	s.Sat = satiationFactor(def.memberWorked(b.State.Data.Worked), activityHalfLife(def, b.Tune))
	s.Score = t.Score * s.Sat
	return s
}

// horizonMul tilts a trip's draw weight by the pilot's horizon.
func horizonMul(long bool, planLean float64) float64 {
	lean := clamp01(planLean)
	if long {
		return (1 - indHorizonSpan) + 2*indHorizonSpan*lean
	}
	return (1 + indHorizonSpan) - 2*indHorizonSpan*lean
}

// orderTrips returns the open trips (positive score) in the order the planner should try them:
// a weighted draw without replacement, so a pilot whose first pick will not start still tries
// the others. roll supplies uniform [0,1) numbers so the draw is testable.
func orderTrips(scores []tripScore, planLean float64, recent []string, roll func() float64) []tripScore {
	seen := map[string]bool{}
	for _, s := range recent {
		seen[s] = true
	}
	type weighted struct {
		t tripScore
		w float64
	}
	var pool []weighted
	for _, s := range scores {
		if s.Score <= 0 {
			continue
		}
		w := s.Score * horizonMul(s.Trip.Long, planLean)
		if seen[s.Trip.Slug] {
			w *= indTripRepeatPenalty
		}
		if w > 0 {
			pool = append(pool, weighted{s, w})
		}
	}
	out := make([]tripScore, 0, len(pool))
	for len(pool) > 0 {
		total := 0.0
		for _, p := range pool {
			total += p.w
		}
		r := roll() * total
		pick := len(pool) - 1
		for i, p := range pool {
			r -= p.w
			if r <= 0 {
				pick = i
				break
			}
		}
		out = append(out, pool[pick].t)
		pool = append(pool[:pick], pool[pick+1:]...)
	}
	return out
}

// industryUmbrellaPlan is the planner behind the "industry" entry.
func (m *Mod) industryUmbrellaPlan(ctx context.Context, b *botRun) error {
	st := b.State

	// A continuation, not a fresh choice: the boarding step re-enters the planner of the activity
	// stamped before the pilot went to find its ship, and that must be the same trip.
	switch st.Activity {
	case ActMining:
		return m.miningPlan(ctx, b)
	case ActIce:
		return m.icePlan(ctx, b)
	case ActIndustry:
		return m.industryPlan(ctx, b)
	}
	// A project already in flight resumes; it is not a thing to choose again.
	if st.Data.IndTypeID != 0 {
		return m.industryPlan(ctx, b)
	}

	need := m.walletNeed(ctx, b)
	scores := m.industryTripScores(ctx, b, need)
	order := orderTrips(scores, b.Tune.PlanLean, st.Data.UmbRecent, rand.Float64)
	if len(order) == 0 {
		return fmt.Errorf("%w: no industry trip is open to it (%s)", errNoFitAvailable, tripSummary(scores))
	}

	var first, afford error
	for _, ts := range order {
		err := ts.Trip.Plan(m, ctx, b)
		if err == nil {
			st.Data.UmbTrip = ts.Trip.Slug
			st.Data.UmbRecent = pushTacticRecent(st.Data.UmbRecent, ts.Trip.Slug)
			return nil
		}
		// A trip that would not start leaves whatever stamp it made; clear it so
		// the next trip (and the continuation switch above) start clean.
		st.Activity = ""
		if errors.Is(err, errNoFitAvailable) {
			m.logBot(b.CharID, logInfo, "sim", "industry: %s passed over (%v) — trying its next trip", ts.Trip.Label, err)
		} else {
			m.logBot(b.CharID, logInfo, "sim", "industry: %s did not start (%v) — trying its next trip", ts.Trip.Label, err)
		}
		if first == nil {
			first = err
		}
		if afford == nil && errors.Is(err, errCannotAfford) {
			afford = err
		}
	}
	if afford != nil {
		return afford
	}
	return first
}

// walletNeed is the same wallet pressure scoreActivities reads, once.
func (m *Mod) walletNeed(ctx context.Context, b *botRun) float64 {
	need := 1.0
	if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err == nil {
		need = needFactor(bal)
	}
	return need
}

// tripSummary is a one-line account of why no trip was open, for the log.
func tripSummary(scores []tripScore) string {
	parts := make([]string, 0, len(scores))
	for _, s := range scores {
		parts = append(parts, s.Trip.Label+": "+s.Why)
	}
	return strings.Join(parts, "; ")
}
