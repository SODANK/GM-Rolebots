package rolebots

import (
	"math"
	"time"

	"gurimoon/destiny"
)

// The slingshot breaks a kite. A pilot that is slower than its target and held outside
// everything it can reach it with (guns, web, scram) burns straight away. The kiter follows in a
// line to keep its range, and once it is committed at speed the pilot turns and comes back: the
// kiter has to kill its momentum before it can open the range again, and the gap shuts while it
// does. Nothing here is scripted against the other side: every call is read off the two balls.
// See docs/FLEET_DOCTRINE.md#slingshot.

// slingPhase is where a pilot is in the manoeuvre.
type slingPhase uint8

const (
	// slingIdle flies the stance (holdCombatRange) and watches whether the gap shuts.
	slingIdle slingPhase = iota
	// slingBurn flies straight away from the target along a heading fixed at the start.
	slingBurn
	// slingTurn comes back at the target at full speed.
	slingTurn
	// slingCooldown flies the stance again before another try.
	slingCooldown
)

func (p slingPhase) String() string {
	switch p {
	case slingBurn:
		return "burn"
	case slingTurn:
		return "turn"
	case slingCooldown:
		return "cooldown"
	}
	return "idle"
}

const (
	// slingReachMarginM is how far outside its control reach the target must sit before the pilot
	// calls it a kite. Inside it the orbit's own wander brings the target in.
	slingReachMarginM = 500.0
	// slingOutpacedFrac: the target's top speed must be at least this share of the pilot's own
	// combat speed. A slower target is caught by flying at it.
	slingOutpacedFrac = 0.95
	// slingWatchSec is the window the gap is measured over before the pilot decides it is not
	// shutting; slingTurnWatchSec is the shorter one used on the way back in.
	slingWatchSec     = 6.0
	slingTurnWatchSec = 3.0
	// The gap counts as not shutting below this closure: a share of the pilot's top speed, floored.
	slingStuckClosureFrac  = 0.1
	slingStuckClosureMinMS = 20.0
	// A burn lasts at least slingBurnMinSec and slingBurnTauMult time constants of the pilot's own
	// hull (95% of top speed). It gives up after slingBurnMaxSec, or slingBurnMaxTau of the kiter's
	// time constants past the minimum when that is longer: a sluggish kiter that was backing off
	// when the burn began needs that long to reverse and catch up.
	slingBurnMinSec  = 6.0
	slingBurnTauMult = 3.0
	slingBurnMaxSec  = 40.0
	slingBurnMaxTau  = 8.0
	// slingAtSpeedFrac is the share of its top speed the pilot must be flying before it turns.
	slingAtSpeedFrac = 0.85
	// slingAlignCos is how straight the target must be following: the cosine between its velocity
	// and the burn heading.
	slingAlignCos = 0.9
	// The share of the shortfall (gap minus reach) one turn must be predicted to win (slingGain)
	// before the pilot commits: slingGainNeedLo for the weakest pilot, who gambles on a slow
	// kiter, up to 1 for the sharpest, who goes only when the numbers alone say it works.
	slingGainNeedLo = 0.5
	// slingGainHorizonSec is how far after the turn slingGain looks for the closest point.
	slingGainHorizonSec = 30.0
	// The way back in: the gap may keep opening for slingTurnGraceTau time constants (the pilot
	// has to kill its own burn first), then two opening samples or slingTurnMaxSec end it.
	slingTurnGraceTau   = 2.0
	slingTurnMaxSec     = 20.0
	slingOpeningSamples = 2
	// slingMaxFails is how many failed tries a pilot gives one target before it stops trying.
	slingMaxFails = 3
	// Cooldown between tries: slingCooldownMinSec plus up to slingCooldownSpanSec, drawn per pilot
	// and try.
	slingCooldownMinSec  = 30.0
	slingCooldownSpanSec = 30.0
	// slingSampleKeepSec is how much gap history is kept. It must cover slingWatchSec.
	slingSampleKeepSec = 10.0
	// The chance a pilot knows the trick: slingKnowBase plus slingKnowBrawl times its BrawlLean.
	slingKnowBase  = 0.2
	slingKnowBrawl = 0.6

	slingKnowSalt     = 0x51A6_5807_0000_0001
	slingSharpSalt    = 0x51A6_5807_0000_0002
	slingCooldownSalt = 0x51A6_5807_0000_0003
)

// slingKnows reports whether this pilot knows the slingshot at all. Stable per pilot; reckless
// and violent pilots, who fly brawlers, are likelier to have learnt it.
func slingKnows(tn Tuning, charID int64) bool {
	p := slingKnowBase + slingKnowBrawl*clamp01(tn.BrawlLean)
	return slingUnit(charID, slingKnowSalt) < p
}

// slingSharpness is how well this pilot times the turn, 0 (turns early) to 1.
func slingSharpness(charID int64) float64 {
	return slingUnit(charID, slingSharpSalt)
}

// slingCooldownFor is how long this pilot waits after its fails-th failed try.
func slingCooldownFor(charID int64, fails int) float64 {
	u := slingUnit(charID^int64(uint64(fails)*noiseGolden), slingCooldownSalt)
	return slingCooldownMinSec + slingCooldownSpanSec*u
}

// slingUnit is a stateless draw in [0, 1).
func slingUnit(seed int64, salt uint64) float64 {
	return (splitmixUnit(uint64(seed)^salt) + 1) / 2
}

// slingInput is everything slingStep reads. Distances are surface metres, speeds m/s.
type slingInput struct {
	phase       slingPhase
	inPhase     float64 // seconds since the phase began
	knows       bool
	sharp       float64
	fails       int
	worse       int // consecutive opening samples on the way back in
	cooldownSec float64

	gap   float64
	reach float64 // controlReachM

	selfSpeed, selfTop, selfTau float64
	tgtTop, tgtTau              float64
	tgtSpeed                    float64
	// tgtChase is the target's velocity along the burn heading: positive when it is following.
	tgtChase float64

	// closure is how fast the gap shuts over the watch window; closureOK is false until the
	// samples span enough of it.
	closure   float64
	closureOK bool
}

// slingDecision is the phase to be in after this tick, the counters to carry, and a line for
// the log when the phase changes.
type slingDecision struct {
	next         slingPhase
	fails, worse int
	note         string
}

// slingStuckClosure is the closure below which the gap is not shutting.
func slingStuckClosure(selfTop float64) float64 {
	return math.Max(slingStuckClosureMinMS, slingStuckClosureFrac*selfTop)
}

// slingGain is how many metres one turn wins, predicted on destiny's inertia model
// (Manager.Step: velocity eases toward the commanded one with time constant tau). At the turn
// the pilot flies away at selfV and the kiter follows at chaseV; the pilot reverses toward it at
// selfV and the kiter backs off at tgtV. The gap then changes by
//
//	(tgtV-selfV)·t + 2·selfV·selfTau·(1-e^(-t/selfTau)) - (chaseV+tgtV)·tgtTau·(1-e^(-t/tgtTau))
//
// and the gain is the deepest dip of that. It is zero when both hulls are equally agile and the
// kiter merely matches the burn: the kiter then recovers as fast as the pilot turns. It grows
// with the kiter's inertia (an MWD's mass) and with how fast it is surging in at the turn.
func slingGain(selfV, selfTau, tgtV, tgtTau, chaseV float64) float64 {
	if selfV <= 0 || selfTau <= 0 || tgtTau <= 0 {
		return 0
	}
	chaseV = math.Max(0, chaseV)
	best := 0.0
	for t := 0.25; t <= slingGainHorizonSec; t += 0.25 {
		dg := (tgtV-selfV)*t + 2*selfV*selfTau*(1-math.Exp(-t/selfTau)) -
			(chaseV+tgtV)*tgtTau*(1-math.Exp(-t/tgtTau))
		if -dg > best {
			best = -dg
		}
	}
	return best
}

// slingGainNeed is the share of the shortfall a pilot of this sharpness wants one turn to win.
func slingGainNeed(sharp float64) float64 {
	return slingGainNeedLo + (1-slingGainNeedLo)*clamp01(sharp)
}

// slingStep is the slingshot's decision for one combat tick. Pure, so the whole manoeuvre is
// testable without a ball.
func slingStep(in slingInput) slingDecision {
	d := slingDecision{next: in.phase, fails: in.fails, worse: in.worse}
	switch in.phase {
	case slingIdle:
		if !in.knows || in.fails >= slingMaxFails || in.reach <= 0 {
			return d
		}
		if in.gap <= in.reach+slingReachMarginM {
			return d
		}
		// A target the pilot can outfly is caught by flying at it.
		if in.tgtTop < in.selfTop*slingOutpacedFrac {
			return d
		}
		// Measured, not assumed: the stance has been flying at it and the gap is not shutting.
		if !in.closureOK || in.closure >= slingStuckClosure(in.selfTop) {
			return d
		}
		// Only a play the numbers support: a kiter that merely matches the burn, on its own hull's
		// inertia, must give up enough metres on the turn.
		need := (in.gap - in.reach) * slingGainNeed(in.sharp)
		if slingGain(in.selfTop, in.selfTau, in.tgtTop, in.tgtTau, in.selfTop) < need {
			return d
		}
		d.next, d.worse = slingBurn, 0
		d.note = "held outside reach by a faster ship: burning away to draw it into a line"

	case slingBurn:
		if in.gap <= in.reach {
			d.next = slingIdle
			d.note = "target came into reach on the burn"
			return d
		}
		minBurn := math.Max(slingBurnMinSec, slingBurnTauMult*in.selfTau)
		if in.inPhase >= math.Max(slingBurnMaxSec, minBurn+slingBurnMaxTau*in.tgtTau) {
			d.next = slingCooldown
			d.fails++
			d.note = "target never committed to the chase"
			return d
		}
		need := (in.gap - in.reach) * slingGainNeed(in.sharp)
		// The range opening early in the burn is the kiter's own inertia, and it surges back to
		// recover it. Only once it has had slingBurnTauMult of its time constants to answer, and
		// is still falling back, has the burn opened more than even a full surge would repay.
		settled := in.inPhase >= minBurn+slingBurnTauMult*in.tgtTau
		if settled && in.closureOK && in.closure < 0 &&
			slingGain(in.selfTop, in.selfTau, in.tgtTop, in.tgtTau, in.tgtTop) < need {
			d.next = slingCooldown
			d.fails++
			d.note = "range opened past what a turn wins back"
			return d
		}
		if in.inPhase < minBurn || in.selfSpeed < slingAtSpeedFrac*in.selfTop {
			return d
		}
		if in.tgtSpeed <= 0 || in.tgtChase < slingAlignCos*in.tgtSpeed {
			return d
		}
		// Turn when the kiter's momentum toward the pilot, as it is now, pays for the shortfall.
		if slingGain(in.selfTop, in.selfTau, in.tgtTop, in.tgtTau, in.tgtChase) < need {
			return d
		}
		d.next, d.worse = slingTurn, 0
		d.note = "target committed in a line: turning back into it"

	case slingTurn:
		if in.gap <= in.reach {
			d.next, d.fails, d.worse = slingIdle, 0, 0
			d.note = "slingshot brought the target into reach"
			return d
		}
		grace := slingTurnGraceTau*in.selfTau + slingTurnWatchSec
		if in.inPhase > grace && in.closureOK && in.closure < 0 {
			d.worse++
		} else {
			d.worse = 0
		}
		if d.worse >= slingOpeningSamples || in.inPhase >= slingTurnMaxSec {
			d.next, d.worse = slingCooldown, 0
			d.fails++
			d.note = "target recovered the range"
		}

	case slingCooldown:
		if in.inPhase >= in.cooldownSec {
			d.next = slingIdle
		}
	}
	return d
}

// slingSample is one reading of the gap.
type slingSample struct {
	at  time.Time
	gap float64
}

// slingState is one pilot's slingshot against one target.
type slingState struct {
	phase    slingPhase
	targetID uint32
	phaseAt  time.Time
	// heading is the burn's direction, fixed when the burn starts: re-aiming it each tick would
	// curve the line the kiter is meant to follow.
	heading destiny.Vec3
	fails   int
	worse   int
	samples []slingSample
}

// reset starts over against targetID.
func (s *slingState) reset(targetID uint32, now time.Time) {
	*s = slingState{targetID: targetID, phaseAt: now}
}

// sample records the gap and drops readings older than slingSampleKeepSec.
func (s *slingState) sample(now time.Time, gap float64) {
	s.samples = append(s.samples, slingSample{at: now, gap: gap})
	cut := 0
	for cut < len(s.samples) && now.Sub(s.samples[cut].at).Seconds() > slingSampleKeepSec {
		cut++
	}
	if cut > 0 {
		s.samples = append(s.samples[:0], s.samples[cut:]...)
	}
}

// closure is how fast the gap shut over the last window seconds, in m/s (negative when it
// opened). ok is false until the readings span most of the window.
func (s *slingState) closure(now time.Time, window float64) (float64, bool) {
	n := len(s.samples)
	if n < 2 {
		return 0, false
	}
	last := s.samples[n-1]
	first := -1
	for i := 0; i < n-1; i++ {
		if now.Sub(s.samples[i].at).Seconds() <= window+0.05 {
			first = i
			break
		}
	}
	if first < 0 {
		return 0, false
	}
	dt := last.at.Sub(s.samples[first].at).Seconds()
	if dt < window*0.6 {
		return 0, false
	}
	return (s.samples[first].gap - last.gap) / dt, true
}

// slingObs is what the pilot sees this tick: both balls and its own numbers.
type slingObs struct {
	knows       bool
	sharp       float64
	cooldownSec float64

	selfPos, selfVel destiny.Vec3
	selfTop, selfTau float64
	tgtPos, tgtVel   destiny.Vec3
	tgtTop, tgtTau   float64

	gap   float64 // surface metres
	reach float64 // controlReachM
}

// advance records this tick's gap, runs slingStep and moves the phase. It reports whether the
// phase changed, with slingStep's line for the log.
func (s *slingState) advance(now time.Time, o slingObs) (bool, string) {
	s.sample(now, o.gap)
	window := slingWatchSec
	if s.phase == slingTurn {
		window = slingTurnWatchSec
	}
	c, ok := s.closure(now, window)
	in := slingInput{
		phase:       s.phase,
		inPhase:     now.Sub(s.phaseAt).Seconds(),
		knows:       o.knows,
		sharp:       o.sharp,
		fails:       s.fails,
		worse:       s.worse,
		cooldownSec: o.cooldownSec,
		gap:         o.gap,
		reach:       o.reach,
		selfSpeed:   o.selfVel.Len(),
		selfTop:     o.selfTop,
		selfTau:     o.selfTau,
		tgtTop:      o.tgtTop,
		tgtTau:      o.tgtTau,
		tgtSpeed:    o.tgtVel.Len(),
		closure:     c,
		closureOK:   ok,
	}
	if !s.heading.IsZero() {
		in.tgtChase = o.tgtVel.Dot(s.heading)
	}
	d := slingStep(in)
	s.fails, s.worse = d.fails, d.worse
	if d.next == s.phase {
		return false, ""
	}
	if d.next == slingBurn {
		away := o.selfPos.Sub(o.tgtPos)
		if away.IsZero() {
			away = destiny.Vec3{X: 1}
		}
		s.heading = away.Normalize()
	}
	s.phase, s.phaseAt = d.next, now
	return true, d.note
}

// slingTau is a ball's inertia time constant, as destiny's Manager.Step computes it.
func slingTau(mass, agility float64) float64 {
	return math.Max(0.1, mass*agility/1e6)
}

// slingshot runs the manoeuvre for one combat tick and reports whether it owns the ship's
// movement this tick. holdCombatRange calls it for every stance but the kite, after the
// engine decision; while it returns true the stance's orbit is not sent.
func (m *Mod) slingshot(eb *embodiedBot, l *combatLoadout, target *destiny.Entity, now time.Time) bool {
	if eb.ent == nil || eb.ent.Manager == nil || target == nil || target.Manager == nil {
		return false
	}
	s := &eb.sling
	if s.targetID != target.ID {
		s.reset(target.ID, now)
	}
	self, tgt := eb.ent.BallDesc(), target.BallDesc()
	gap := self.Pos.Sub(tgt.Pos).Len() - self.Radius - tgt.Radius
	o := slingObs{
		knows:       slingKnows(eb.tune, eb.charID),
		sharp:       slingSharpness(eb.charID),
		cooldownSec: slingCooldownFor(eb.charID, s.fails),
		selfPos:     self.Pos,
		selfVel:     self.Vel,
		selfTop:     combatSpeed(eb, l),
		selfTau:     slingTau(self.Mass, eb.ent.Manager.Agility()),
		tgtPos:      tgt.Pos,
		tgtVel:      tgt.Vel,
		tgtTop:      targetTopSpeed(target),
		tgtTau:      slingTau(tgt.Mass, target.Manager.Agility()),
		gap:         math.Max(0, gap),
		reach:       controlReachM(l),
	}
	if changed, note := s.advance(now, o); changed && note != "" {
		m.logBot(eb.charID, logInfo, "combat", "slingshot %s: %s (gap %.1f km, reach %.1f km)",
			s.phase, note, o.gap/1000, o.reach/1000)
	}
	switch s.phase {
	case slingBurn:
		m.runPropCycle(eb, l, true)
		m.embodyBurnHeading(eb, s.heading)
		return true
	case slingTurn:
		m.runPropCycle(eb, l, true)
		m.embodyApproach(eb, target.ID, 0, tgt.Radius)
		return true
	}
	return false
}
