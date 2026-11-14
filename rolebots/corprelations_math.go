package rolebots

// The arithmetic of a corporation's opinion of another, with no clock, database or
// randomness; every rule is a cell of corprelations_test.go.
//
// One directed pair is a relState: a live value in ±10, the trait baseline it drifts
// toward, a sticky tier read off the value, and the written number (crpStandings), the
// only part the game reads. Incidents push the value away from the baseline, decay pulls
// it back, and a pact floors it. Grudges and goodwill decay at different half-lives
// (GrudgeHalfLifeD, GoodwillHalfLifeD); the aggressor's ledger takes only AggressorShare.
// A tier holds while the value is within Hysteresis of its line (standings.go), and the
// written number snaps onto the line so stanceOf never disagrees with the journaled tier.

import (
	"math"
	"sort"
)

// Incident kinds. The string is the stored column; keep them short (the
// column is VARCHAR(12)).
const (
	incKill       = "kill"       // one of ours lost a ship to them
	incGank       = "gank"       // one of ours was suicide-ganked by them in policed space (gankrel.go)
	incPod        = "pod"        // one of ours lost a capsule to them
	incAggress    = "aggress"    // they opened fire and nobody died
	incRobbery    = "robbery"    // ransom paid / hold-up
	incBounty     = "bounty"     // their corporation put a price on one of ours
	incSiege      = "siege"      // they put a tower of ours into reinforcement
	incTowerKill  = "towerkill"  // they destroyed a tower of ours
	incWarDec     = "wardec"     // they declared war on us
	incWarRetract = "warretract" // they retracted it
	incWarEnd     = "warend"     // the war ended (lapsed, finished)
	incCrowd      = "crowd"      // they worked our belt (mutual)
	incContest    = "contest"    // they raced us for a site we were running (mutual, siteetiquette.go)
	incEnvoyKill  = "envoykill"  // they killed our envoy on the road
	incPact       = "pact"       // a pact between us was agreed (mutual)
	incReparation = "reparation" // they paid us reparations
	incPeace      = "peace"      // peace between us (mutual)
	incPactBreak  = "pactbreak"  // they broke a pact with us
	incFeud       = "feud"       // one of ours holds a personal feud with one of theirs (grace_ledger.go)
)

// Which side of an incident a ledger row records.
const (
	relSideVictim    = "victim"
	relSideAggressor = "aggressor"
	relSideMutual    = "mutual"
)

// relISKRef is the ISK at which value scaling starts, relISKFull where it
// saturates: a 100k ISK frigate is a kill, a 100M ISK battleship is three of
// them.
const (
	relISKRef  = 1e5
	relISKFull = 1e8
	// relPactBreakWeight is the base magnitude at or past which a hostile incident from a
	// pact party breaks the pact: a kill, a siege or a war declaration does; a shot that
	// missed, a hold-up or a crowded belt does not.
	relPactBreakWeight = 1.0
	// relWrittenEps is the smallest change in the written number worth a
	// SetCorpStanding call.
	relWrittenEps = 0.05
	// relDropBelow: a pair whose value sits this close to zero, holds no tier
	// and no pact, is not written at all (the row is deleted). This is what
	// keeps a corporation far under the core's 300-row cap.
	relDropBelow = 0.5
)

// relParams is the operator's dials as the fold reads them (relledgersettings.go),
// handed in as a value so the arithmetic stays pure.
type relParams struct {
	KillImpulse    float64 // global multiplier on every violence impulse
	AggressorShare float64 // what the doer's own ledger takes of a victim's loss
	CrowdFloor     float64 // crowding alone never pushes a value below this
}

// relDefaultParams is the shipped tuning, for callers with no dials to hand.
func relDefaultParams() relParams {
	return relParams{KillImpulse: 1, AggressorShare: relAggressorShareDefault, CrowdFloor: relCrowdFloorDefault}
}

// relState is one directed pair's live state: a rolebot_corp_relations row.
type relState struct {
	Value      float64 // live, continuous, ±10
	Baseline   float64 // the trait baseline at the last diplomacy pass
	PrevValue  float64 // the value at the previous pass (the panel's trend)
	Written    float64 // what crpStandings holds (0 = no row)
	Tier       float64 // ceoRelAlly/Friend/0/Rival/Enemy, sticky
	TierAt     int64   // unix the tier last changed
	DecayedAt  int64   // unix the value was last decayed to
	FloorValue float64 // a standing pact's floor on this direction (0 = none)
	FloorUntil int64
	Yielded    bool
	Reason     string
}

// relIncident is one ledger row as the fold sees it.
type relIncident struct {
	ID        int64
	Kind      string
	Side      string
	Weight    float64 // the BASE impulse (relBaseWeight): signed, before the charter's weights
	ISK       float64
	At        int64
	SubjectID int64
}

// diploBend is how a Director of Diplomacy bends the rates: how the corporation
// forgives and warms, never what it thinks (baseline and lines are the CEO's). The zero
// value is "no director": every multiplier 1.
type diploBend struct {
	HLGrudge float64 // × GrudgeHalfLifeD
	HLGood   float64 // × GoodwillHalfLifeD
	Hyst     float64 // × Hysteresis
	Appetite float64 // × letter eagerness
}

// diploBendNone is the CEO doing its own diplomacy.
func diploBendNone() diploBend { return diploBend{1, 1, 1, 1} }

// diploBendOf reads the bends off a director's own traits. Polarity is
// traitCatalog's: LOW loyalty is devoted, LOW patience is unhurried.
func diploBendOf(p pilotIdentity) diploBend {
	return diploBend{
		HLGrudge: bend(p.trait(TraitLoyalty), 0.7, 1.0, 1.4), // a devoted diplomat forgives sooner
		HLGood:   bend(p.trait(TraitLoyalty), 1.4, 1.0, 0.7), // and keeps friends longer
		Hyst:     bend(p.trait(TraitPatience), 1.3, 1.0, 0.7),
		Appetite: 1 + 0.5*p.trait(TraitLeadership),
	}
}

// norm fills a zero value with "no director".
func (b diploBend) norm() diploBend {
	if b.HLGrudge <= 0 {
		b.HLGrudge = 1
	}
	if b.HLGood <= 0 {
		b.HLGood = 1
	}
	if b.Hyst <= 0 {
		b.Hyst = 1
	}
	if b.Appetite <= 0 {
		b.Appetite = 1
	}
	return b
}

// iskScale is how much bigger than a frigate a loss was, 0..1 on a log scale:
// 100k ISK is 0, 10M is about two thirds, 100M and up is 1.
func iskScale(isk float64) float64 {
	if isk <= relISKRef {
		return 0
	}
	v := math.Log10(isk/relISKRef) / math.Log10(relISKFull/relISKRef)
	return math.Max(0, math.Min(1, v))
}

// relBaseWeight is the impulse an incident carries before the viewer's charter weighs
// it: what the hooks store. Negative is hostile. The charter's part is applied at fold
// time by relImpulse, so a change of CEO re-weights incidents still to be folded.
func relBaseWeight(kind, side string, isk float64, p relParams) float64 {
	var w float64
	switch kind {
	case incKill:
		w = -(1 + 2*iskScale(isk)) * p.KillImpulse
	case incGank:
		// Four kills' worth: they paid CONCORD to open one of ours where the
		// law was supposed to stand between us.
		w = -4 * (1 + 2*iskScale(isk)) * p.KillImpulse
	case incPod:
		w = -0.5 * p.KillImpulse
	case incAggress:
		w = -0.3
	case incRobbery:
		w = -0.8 * (1 + iskScale(isk))
	case incBounty:
		if side == relSideAggressor {
			return 0
		}
		return -0.3
	case incSiege:
		w = -2.0
	case incTowerKill:
		w = -4.0 * (1 + iskScale(isk))
	case incWarDec:
		if side == relSideAggressor {
			return -0.5
		}
		return ceoRelWarTerm
	case incWarRetract:
		if side == relSideAggressor {
			return 0
		}
		return 1.5
	case incWarEnd:
		return 0.5
	case incCrowd, incContest:
		return -0.4
	case incEnvoyKill:
		w = -1.5 * (1 + 2*iskScale(isk))
	case incPact:
		return 1.0
	case incReparation:
		if side == relSideAggressor {
			return 0.3 // we paid: relief, not warmth
		}
		return 1 + 2*iskScale(isk)
	case incPeace:
		return 2.0
	case incPactBreak:
		w = -5.0
	case incFeud:
		// The hooks pass the feud's depth as the weight (−1 feud, −2 blood); only the victim's side files.
		if side == relSideAggressor {
			return 0
		}
		return -1.0
	default:
		return 0
	}
	if side == relSideAggressor {
		w *= p.AggressorShare
	}
	return w
}

// relKindCharterMul is the charter weight a kind is multiplied by at fold.
func relKindCharterMul(kind string, ch Charter) float64 {
	switch kind {
	case incKill, incGank, incPod, incAggress, incRobbery, incEnvoyKill:
		return ch.GrievanceWeight
	case incCrowd, incContest:
		return ch.CompeteWeight / 1.5 // 1.5 is the neutral CompeteWeight
	}
	return 1
}

// relImpulse is what one incident moves the value by, for this viewer.
func relImpulse(inc relIncident, ch Charter) float64 {
	w := inc.Weight * relKindCharterMul(inc.Kind, ch)
	if w > 0 && ch.GoodwillGain > 0 {
		w *= ch.GoodwillGain
	}
	return w
}

// relPactBreaks reports whether an incident from the other party is grave
// enough to break a standing pact: a hostile act of base weight at least
// relPactBreakWeight on the VICTIM's ledger, or a war declaration.
func relPactBreaks(inc relIncident) bool {
	if inc.Side != relSideVictim {
		return false
	}
	if inc.Kind == incWarDec {
		return true
	}
	return inc.Weight <= -relPactBreakWeight
}

// relDecay moves the value toward the baseline for the time since it was last
// decayed. Below the baseline the half-life is the grudge's, above it the
// goodwill's. Never overshoots: it is a pure exponential toward the target.
func relDecay(s relState, now int64, ch Charter, b diploBend) relState {
	b = b.norm()
	dt := float64(now - s.DecayedAt)
	if s.DecayedAt == 0 || dt <= 0 {
		s.DecayedAt = now
		return s
	}
	d := s.Value - s.Baseline
	var halfDays float64
	if d < 0 {
		halfDays = ch.GrudgeHalfLifeD * b.HLGrudge
	} else {
		halfDays = ch.GoodwillHalfLifeD * b.HLGood
	}
	if halfDays <= 0 {
		halfDays = 7
	}
	s.Value = s.Baseline + d*math.Pow(2, -dt/(halfDays*86400))
	s.DecayedAt = now
	return s
}

// relApplyImpulse adds one impulse to a value. Crowding is capped: it may
// take a value down to CrowdFloor and no further, and once violence has
// put the value below the floor it neither lifts nor deepens it.
func relApplyImpulse(value, w float64, kind string, p relParams) float64 {
	if (kind == incCrowd || kind == incContest) && w < 0 {
		floor := math.Min(value, p.CrowdFloor)
		return math.Max(value+w, floor)
	}
	return value + w
}

// relTierEnter is the tier a value earns from nothing.
func relTierEnter(v float64) float64 {
	switch {
	case v >= StandingDevoted:
		return ceoRelAlly
	case v >= StandingLiked:
		return ceoRelFriend
	case v <= StandingDespised:
		return ceoRelEnemy
	case v <= StandingDisliked:
		return ceoRelRival
	}
	return 0
}

// relTierLine is the line a tier sits on.
func relTierLine(tier float64) float64 {
	switch tier {
	case ceoRelAlly:
		return StandingDevoted
	case ceoRelFriend:
		return StandingLiked
	case ceoRelRival:
		return StandingDisliked
	case ceoRelEnemy:
		return StandingDespised
	}
	return 0
}

// relTierHolds reports whether a standing tier survives this value: its line,
// moved back by the hysteresis.
func relTierHolds(tier, v, hyst float64) bool {
	switch tier {
	case ceoRelAlly, ceoRelFriend:
		return v >= relTierLine(tier)-hyst
	case ceoRelRival, ceoRelEnemy:
		return v <= relTierLine(tier)+hyst
	}
	return false
}

// relTier is the tier this value calls for, given the one standing.
// Escalation in the same direction is immediate; anything else has to cross
// back over the hysteresis first, and a fallen ally lands on friend (a fallen
// enemy on rival) when that line still holds.
func relTier(prev, v, hyst float64) float64 {
	enter := relTierEnter(v)
	if prev == 0 {
		return enter
	}
	if enter != 0 && (enter > 0) == (prev > 0) && math.Abs(enter) > math.Abs(prev) {
		return enter
	}
	if relTierHolds(prev, v, hyst) {
		return prev
	}
	switch {
	case prev == ceoRelAlly && relTierHolds(ceoRelFriend, v, hyst):
		return ceoRelFriend
	case prev == ceoRelEnemy && relTierHolds(ceoRelRival, v, hyst):
		return ceoRelRival
	}
	return enter
}

// relWritten is the number crpStandings gets: the value at 0.1 steps, snapped
// onto the tier's line while the tier holds inside its hysteresis band, so a
// pair the ledger calls a rival never reads as neutral to stanceOf.
func relWritten(value, tier float64) float64 {
	w := clampStanding(value)
	switch tier {
	case ceoRelAlly, ceoRelFriend:
		if line := relTierLine(tier); w < line {
			w = line
		}
	case ceoRelRival, ceoRelEnemy:
		if line := relTierLine(tier); w > line {
			w = line
		}
	}
	return w
}

// relFoldResult is what one fold of a pair reports beside the new state.
type relFoldResult struct {
	TierChanged bool
	PactBroken  bool // a standing pact was broken by one of these incidents
}

// relFoldState is one pair's fold: decay to now, apply every unapplied incident oldest
// first, hold the pact floor, clamp, re-tier. A pact broken mid-fold clears the floor at
// once; the caller files the mirror incident and ends the pact row.
func relFoldState(s relState, incs []relIncident, now int64, ch Charter, b diploBend, p relParams) (relState, relFoldResult) {
	b = b.norm()
	var res relFoldResult
	s = relDecay(s, now, ch, b)
	floorOn := s.FloorUntil > now
	for _, inc := range incs {
		if floorOn && relPactBreaks(inc) {
			floorOn = false
			s.FloorValue, s.FloorUntil = 0, 0
			res.PactBroken = true
			s.Value = relApplyImpulse(s.Value, relBaseWeight(incPactBreak, relSideVictim, 0, p), incPactBreak, p)
		}
		s.Value = relApplyImpulse(s.Value, relImpulse(inc, ch), inc.Kind, p)
	}
	if !floorOn {
		s.FloorValue, s.FloorUntil = 0, 0
	} else if s.Value < s.FloorValue {
		s.Value = s.FloorValue
	}
	s.Value = math.Max(-10, math.Min(10, s.Value))
	tier := relTier(s.Tier, s.Value, ch.Hysteresis*b.Hyst)
	if tier != s.Tier {
		s.Tier, s.TierAt = tier, now
		res.TierChanged = true
	}
	return s, res
}

// relWriteRow is one pair as the flush sees it.
type relWriteRow struct {
	ToID     int64
	Value    float64
	Tier     float64
	Floor    float64 // a standing pact's floor, 0 = none
	HasFloor bool
	// Seated is the tier the number last WRITTEN for this pair sits in
	// (relTierEnter of rolebot_corp_relations.written): which pairs hold a
	// capped seat in the game right now.
	Seated  float64
	Written float64 // out: what to write (0 = delete the row)
}

// relSeatMargin is how far a challenger's value must clear a seated pair's before it
// takes that pair's place under a cap, so near-equal pairs do not swap seats every fold.
const relSeatMargin = 1.0

// relApplyWriteCaps decides the written number for every pair of one corporation under
// its charter's limits. The strongest values win within each kind; excess allies and
// friends are written just under their line, excess rivals and enemies just above the
// rival line, then the hard ceiling applies. A pact pair is never written below its floor.
func relApplyWriteCaps(rows []relWriteRow, ch Charter) {
	for i := range rows {
		r := &rows[i]
		r.Written = relWritten(r.Value, r.Tier)
		if r.Tier == 0 && !r.HasFloor && math.Abs(r.Value) < relDropBelow {
			r.Written = 0
		}
	}
	idxBy := func(match func(relWriteRow) bool, coldFirst bool, seated func(relWriteRow) bool) []int {
		var idx []int
		for i := range rows {
			if match(rows[i]) {
				idx = append(idx, i)
			}
		}
		// Rank strength, with the incumbent's edge: a pair already holding a
		// seat of this kind counts relSeatMargin stronger.
		key := func(r relWriteRow) float64 {
			v := r.Value
			if coldFirst {
				v = -v
			}
			if seated(r) {
				v += relSeatMargin
			}
			return v
		}
		sort.SliceStable(idx, func(a, b int) bool {
			va, vb := key(rows[idx[a]]), key(rows[idx[b]])
			if va != vb {
				return va > vb
			}
			return rows[idx[a]].ToID < rows[idx[b]].ToID
		})
		return idx
	}
	clipTo := func(idx []int, max int, to float64, upper bool) {
		for n, i := range idx {
			if n < max {
				continue
			}
			if upper && rows[i].Written > to {
				rows[i].Written = to
			}
			if !upper && rows[i].Written < to {
				rows[i].Written = to
			}
		}
	}
	clipTo(idxBy(func(r relWriteRow) bool { return r.Written >= StandingDevoted }, false,
		func(r relWriteRow) bool { return r.Seated == ceoRelAlly }),
		ch.MaxAllies, StandingDevoted-0.1, true)
	// An ally seat counts as a friend seat too: an ally cooled to friend
	// keeps its place among the friends.
	clipTo(idxBy(func(r relWriteRow) bool { return r.Written >= StandingLiked && r.Written < StandingDevoted }, false,
		func(r relWriteRow) bool { return r.Seated >= ceoRelFriend }),
		ch.MaxFriends, StandingLiked-0.1, true)
	clipTo(idxBy(func(r relWriteRow) bool { return r.Written <= StandingDisliked }, true,
		func(r relWriteRow) bool { return r.Seated <= ceoRelRival }),
		ch.MaxRivals, StandingDisliked+0.1, false)
	// The hard ceiling: everything past the strongest ceoRelHardCap opinions
	// is not written.
	var idx []int
	for i := range rows {
		if rows[i].Written != 0 {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		wa, wb := math.Abs(rows[idx[a]].Written), math.Abs(rows[idx[b]].Written)
		if wa != wb {
			return wa > wb
		}
		return rows[idx[a]].ToID < rows[idx[b]].ToID
	})
	for n, i := range idx {
		if n >= ceoRelHardCap {
			rows[i].Written = 0
		}
	}
	// A pact's floor beats every cap: the corporation gave its word.
	for i := range rows {
		if rows[i].HasFloor && rows[i].Written < rows[i].Floor {
			rows[i].Written = rows[i].Floor
		}
	}
}

// Ownership of one (our corporation, target) pair.
const (
	relOwnFree    = iota // nobody has declared anything: the ledger may
	relOwnOurs           // the ledger's own row, untouched since: the ledger may
	relOwnHuman          // somebody else's declaration: never touched
	relOwnYield          // the ledger's row, since edited by a human: hand it over now
	relOwnYielded        // handed over before: never touched again
)

// relOwnership decides whose a pair is: a row stays ours only while the live standing is
// still what we last wrote. A human's edit or deletion is theirs and the pair is yielded.
// Written 0 and no live row agree (both mean "no standing").
func relOwnership(live float64, hasLive bool, s relState, hasRow bool) int {
	if hasRow && s.Yielded {
		return relOwnYielded
	}
	cur := 0.0
	if hasLive {
		cur = live
	}
	if hasRow {
		if math.Abs(cur-s.Written) < 1e-6 {
			return relOwnOurs
		}
		return relOwnYield
	}
	if hasLive && live != 0 {
		return relOwnHuman
	}
	return relOwnFree
}

// ceoRelBaseline is the static half of ceoRelScore: what the CEO thinks of a
// corporation before anything has happened between them. Prejudice,
// politics, predation, law, trade and reciprocity; the event terms
// (grievance, comradeship, war) are the ledger's now.
func ceoRelBaseline(in ceoRelInputs, ch Charter) (float64, []ceoRelTerm) {
	in.Grievance, in.Comrades, in.AtWar = 0, 0, false
	return ceoRelScore(in, ch)
}
