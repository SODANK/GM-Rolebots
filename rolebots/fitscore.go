package rolebots

import (
	"context"
	"math"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// What a fit is worth to one pilot: three numbers multiply.
//
//	power     what the fit can do at the job, from the stat engine on an all-V basis (a property
//	          of the fit, cached with it)
//	affinity  how far past the minimum the pilot's skills go on what the fit is made of
//	race      a small pull toward the pilot's own faction's hulls: a bias, not a rule
//
// Affordability is not here. Money is a gate the wallet applies to the ranked list (fitselect.go).

const (
	// combatEHPBasis is the tank a combat fit is measured against: score
	// is DPS scaled by sqrt(EHP / basis), so tank counts but never lets a
	// gunless brick out-rank a ship that can actually kill something.
	combatEHPBasis = 1000.0
	// miningHoldWorthM3 sets how much a big hold is worth against yield. A hold twice this size
	// doubles the score of the same lasers, since the trip back to the station is time not mining.
	miningHoldWorthM3 = 2000.0
	// tradeAlignWorth is the align time at which a hauler is judged half as nimble as an instant one;
	// the factor runs from 1x to 2x.
	tradeAlignWorth = 6.0

	// skillAffinityWeight is how much perfect skills are worth against
	// raw capability: a fit the pilot has maxed out scores 60% above the
	// same fit flown at the bare minimum.
	skillAffinityWeight = 0.60
	// raceAffinityBonus is the pull toward one's own faction's hulls.
	raceAffinityBonus = 0.15
	// xenoRefusedHullMul is what a hull of a refused race keeps of its score
	// (fitJudgement.Foreign): low enough that any allowed fit for the job
	// beats it, above zero so it still wins when it is the only one.
	xenoRefusedHullMul = 0.02
	// hullSkillWeight weights the hull's own requirements against the modules' when measuring
	// affinity. Ship-class skill is what a pilot trains toward; a tech-2 gun is incidental.
	hullSkillWeight = 3.0
)

// ── fit power (cached per fit, per pilot) ───────────────────────────────────

// fitCacheKey is (fit, whose version of it, for which job). A fit's contents differ by pilot and
// job (effectiveFit drops what a pilot cannot online and sheds what its grid cannot take), so
// derived values are keyed all three ways. CharID 0 and job empty are the fit as authored.
type fitCacheKey struct {
	fitID    int64
	charID   int64
	job      string
	hullOnly bool
}

func keyFor(t *fitTemplate) fitCacheKey {
	return fitCacheKey{fitID: t.FitID, charID: t.EffectiveFor, job: t.Job, hullOnly: t.HullOnly}
}

// derivedFitTTL bounds how stale a pilot's version of a fit may get. Its contents move when skills
// move and no hook says a training queue finished, so the numbers expire on their own.
const derivedFitTTL = 10 * time.Minute

type fitPowerEntry struct {
	power float64
	at    time.Time
}

var (
	fitPowerMu    sync.Mutex
	fitPowerCache = map[fitCacheKey]fitPowerEntry{}

	// Skill requirements are static data, cached for the life of the process because effectiveFit
	// reads them for every module of every fit in the pool.
	typeReqMu    sync.Mutex
	typeReqCache = map[int64][]evedb.SkillReq{}
)

// requiredSkills is evedb.GetRequiredSkills with the static-data cache in
// front of it.
func (m *Mod) requiredSkills(ctx context.Context, typeID int64) ([]evedb.SkillReq, error) {
	typeReqMu.Lock()
	r, ok := typeReqCache[typeID]
	typeReqMu.Unlock()
	if ok {
		return r, nil
	}
	reqs, err := evedb.GetRequiredSkills(ctx, m.db, typeID)
	if err != nil {
		return nil, err
	}
	typeReqMu.Lock()
	typeReqCache[typeID] = reqs
	typeReqMu.Unlock()
	return reqs, nil
}

// forgetFitDerived drops everything derived from a pilot's fits. Called when
// a character is purged (forget.go): the entries are keyed by character ID
// and nothing would ever look them up again.
func forgetFitDerived(charID int64) {
	fitCostMu.Lock()
	for k := range fitCostCache {
		if k.charID == charID {
			delete(fitCostCache, k)
		}
	}
	fitCostMu.Unlock()
	fitPowerMu.Lock()
	for k := range fitPowerCache {
		if k.charID == charID {
			delete(fitPowerCache, k)
		}
	}
	fitPowerMu.Unlock()
}

// sweepFitDerived drops entries past their TTL. Both caches expire entries at the point of use,
// but an entry nobody asks for again is never deleted, and the key space (fit, pilot, job,
// hull-only) runs to millions across the roster, so without the sweep the caches only grow.
func sweepFitDerived() {
	now := clock.Now()
	fitCostMu.Lock()
	for k, e := range fitCostCache {
		if now.Sub(e.at) >= fitCostTTL {
			delete(fitCostCache, k)
		}
	}
	fitCostMu.Unlock()
	fitPowerMu.Lock()
	for k, e := range fitPowerCache {
		if now.Sub(e.at) >= derivedFitTTL {
			delete(fitPowerCache, k)
		}
	}
	fitPowerMu.Unlock()
}

// Tackle, priced as the multiplier it is on a piracy fit.
const (
	// What a fit with no tackle at all keeps. Small enough that any pointed
	// hull beats any unpointed one of the same class, large enough that a
	// pilot who cannot yet fit a point still has something to fly.
	tackleFactorNone = 0.15
	// A single scramble point is the whole difference between a robbery and
	// a story about one, so it is worth more than doubling the fit.
	tackleFactorPoint = 1.0
	// Each further point past the first is insurance against a warp core
	// stabiliser, not a second robbery.
	tackleFactorExtraPoint = 0.15
	// A full web roughly halves the time to close and stops the prey
	// crawling out of scramble range. Scaled by how hard the web bites.
	tackleFactorFullWeb = 0.45
	// A cargo scanner prices the robbery (camp.go ransomScan): the hold's worth, and the cargo demanded
	// outright when it dwarfs any ransom. Worth a little more than a full web, so point-prop-scanner
	// outranks point-prop-web, the order wishList builds it in. A multiplier on the piracy lens only.
	piracyScanWorth = 0.5
)

// ransomScanFactor is piracy's lift for carrying a cargo scanner.
func ransomScanFactor(s *FitStats) float64 {
	if s.CargoScanRangeM > 0 {
		return 1 + piracyScanWorth
	}
	return 1
}

// The other end of the tackle mechanic, priced for the ship being tackled. jobPower(JobTrade) reads
// CargoM3 and AlignSec only, so without this a stabiliser in a low slot is an expander the hauler
// did not fit, strictly negative to the only lens that judges it (the roleEwar law in fitgen.go).
const (
	// escapeFactorPerPoint is what one point of warp core strength is worth to a hauling hull: ten
	// percent, an order of magnitude under tackleFactor's pay for the other end of the mechanic. A
	// stabiliser pays only on a rare interdicted trip, and priced higher it would outrank the cargo hold.
	escapeFactorPerPoint = 0.10
	// escapeFactorMaxPoints caps the credit at two points: one stabiliser beats a lone frigate's point,
	// two beat a scrambler, and more is insurance against a gang that would hold the ship anyway.
	escapeFactorMaxPoints = 2.0
)

// escapeFactor is how much more a hauling fit is worth for being harder to hold. Multiplied into
// the trade and loot lenses as tackleFactor is into piracy: a multiplier, not an added term,
// because it makes the whole trip more likely to finish rather than the hold bigger.
func escapeFactor(s *FitStats) float64 {
	return 1 + escapeFactorPerPoint*math.Min(s.WarpCoreStrength, escapeFactorMaxPoints)
}

// tackleFactor is how much of its damage a piracy fit can expect to land on
// a pilot who is trying to leave.
func tackleFactor(s *FitStats) float64 {
	f := tackleFactorNone
	if s.ScramStrength >= 1 {
		f = tackleFactorPoint + tackleFactorExtraPoint*(s.ScramStrength-1)
	}
	if s.WebPct > 0 {
		f += tackleFactorFullWeb * math.Min(1, s.WebPct/75)
	}
	return f
}

// jobPower folds a stat readout into the one number a job cares about. Zero means the ship cannot
// do that work at all: it is how this module says wrong ship, with no tag or category. A hull with
// no mining laser and no mining drone scores nothing as a miner.
func jobPower(job string, s *FitStats) float64 {
	switch job {
	case JobMining:
		yield := s.MiningM3PerMin + s.DroneMiningM3PerMin
		if yield <= 0 {
			return 0
		}
		return yield * (1 + s.CargoM3/miningHoldWorthM3)
	case JobTrade:
		if s.CargoM3 <= 0 {
			return 0
		}
		align := math.Max(s.AlignSec, 0)
		return s.CargoM3 * (1 + tradeAlignWorth/(tradeAlignWorth+align)) * escapeFactor(s)
	case JobFreight:
		// Cargo alone. A lifter is judged on whether the kit's biggest unit goes in the hold; align time
		// and stabilisers do not make an 8,000 m3 array fit a 6,620 m3 hold.
		return math.Max(s.CargoM3, 0)
	case JobPiracy:
		// Combat's number multiplied by whether the victim can leave. Tackle is a multiplier, not an
		// added term, and its floor is small but non-zero. See docs/comment-notes-b09.md#piracy-tackle.
		dps := s.TurretDPS + s.LauncherDPS + s.DroneDPS
		if dps <= 0 {
			return 0
		}
		base := dps * math.Sqrt(math.Max(effectiveEHP(s), 1)/combatEHPBasis) * standoffFactor(s)
		return base * tackleFactor(s) * ransomScanFactor(s)
	case JobTackle:
		// No point, no tackler: unlike piracy there is no un-pointed floor, since the tackle seat exists
		// only to hold.
		if s.ScramStrength <= 0 {
			return 0
		}
		// Holding power x the speed to get it onto somebody x living long enough to matter. Velocity is
		// the tackler's real weapon: the point works at 20-odd km.
		surv := math.Sqrt(math.Max(effectiveEHP(s), 1) / combatEHPBasis)
		speed := math.Max(fitSpeed(s), 1) / 100
		dps := s.TurretDPS + s.LauncherDPS + s.DroneDPS
		return tackleFactor(s) * speed * surv * 100 * (1 + dps/200)
	case JobLogi:
		// HP a second onto the wing, carried by the logi's own survival and sustained by its capacitor.
		// No remote rep means no logi, a hard zero.
		if s.RemoteRepPerSec <= 0 {
			return 0
		}
		sustain := 1.0
		if s.CapRechargeSec > 0 && s.CapGJ > 0 {
			// Passive cap a second against the ~10 GJ/s a full rack of large transporters drinks, bounded so
			// cap at most doubles the score.
			sustain += math.Min(s.CapGJ/s.CapRechargeSec/10, 1)
		}
		return s.RemoteRepPerSec * math.Sqrt(math.Max(effectiveEHP(s), 1)/combatEHPBasis) * sustain
	case JobScout:
		// No scanner, no scout. Then the hold for the loot, lifted by stabilisers that get it off a grid
		// the victim's friends may hold. Scan reach is a mild lift: every scanner reads a hold from 30 km.
		if s.CargoScanRangeM <= 0 {
			return 0
		}
		return math.Max(s.CargoM3, 1) * (1 + s.WarpCoreStrength) * (0.5 + math.Min(s.CargoScanRangeM/120_000, 0.5))
	case JobEwar:
		// No electronics, no ewar seat: a hard zero. Then what the rack takes off the fleet, carried by
		// the seat's own survival, with a bounded nod to its guns.
		if s.EwarStrength <= 0 {
			return 0
		}
		dps := s.TurretDPS + s.LauncherDPS + s.DroneDPS
		return s.EwarStrength * 100 * math.Sqrt(math.Max(effectiveEHP(s), 1)/combatEHPBasis) *
			(1 + math.Min(dps/400, 0.5))
	default:
		dps := s.TurretDPS + s.LauncherDPS + s.DroneDPS
		if dps <= 0 {
			return 0
		}
		// Standoff, in the effective-hitpoint form the duel's pre-fight look uses. standoffFactor calls
		// the same rangeAdvantage as the duel (fitreach.go), so a longer-ranged fit is not scored lower.
		return dps * math.Sqrt(math.Max(effectiveEHP(s), 1)/combatEHPBasis) * standoffFactor(s)
	}
}

// fitPower is jobPower over a template, for the job it was resolved for, cached. The stat engine
// walks every module's attributes and effects, and this runs for every fit in the pool on every
// plan pass for every bot, so it must be cached.
func (m *Mod) fitPower(ctx context.Context, t *fitTemplate) (float64, error) {
	key := keyFor(t)
	if t.FitID != 0 {
		fitPowerMu.Lock()
		e, ok := fitPowerCache[key]
		fitPowerMu.Unlock()
		if ok && clock.Now().Sub(e.at) < derivedFitTTL {
			return e.power, nil
		}
	}
	s, err := m.ComputeFitStats(ctx, t.Fit)
	if err != nil {
		return 0, err
	}
	p := jobPower(t.Job, s)
	if t.FitID != 0 {
		fitPowerMu.Lock()
		fitPowerCache[key] = fitPowerEntry{power: p, at: clock.Now()}
		fitPowerMu.Unlock()
	}
	return p, nil
}

// jobPowerRow is one line of the "good at" readout.
type jobPowerRow struct {
	Job   string
	Label string
	Power float64
	Able  bool
}

// jobPowers scores a template at every job at once: one stat fold, one reading per job. The panel
// uses it instead of an activity tag, so the zeros show what the ship cannot do.
func (m *Mod) jobPowers(ctx context.Context, t *fitTemplate) []jobPowerRow {
	s, err := m.ComputeFitStats(ctx, t.Fit)
	if err != nil {
		return nil
	}
	out := make([]jobPowerRow, 0, len(jobCatalog))
	for _, d := range jobCatalog {
		p := jobPower(d.Key, s)
		out = append(out, jobPowerRow{Job: d.Key, Label: d.Label, Power: p, Able: p > 0})
	}
	return out
}

// ── skill affinity ──────────────────────────────────────────────────────────

// weightedReq is one skill a fit needs, and how much that requirement
// says about the pilot's commitment to this kind of ship.
type weightedReq struct {
	SkillID int64
	Level   int
	Weight  float64
}

// fitSkillRequirements collects every skill the fit's hull, modules, charges and drones ask for,
// with the hull's weighted heavier. Run on the effective fit it measures what the pilot flies.
// Cheap: lookups come from the static requirement cache.
func (m *Mod) fitSkillRequirements(ctx context.Context, t *fitTemplate) ([]weightedReq, error) {
	byID := map[int64]weightedReq{}
	collect := func(typeID int64, weight float64) error {
		if typeID == 0 {
			return nil
		}
		reqs, err := m.requiredSkills(ctx, typeID)
		if err != nil {
			return err
		}
		for _, r := range reqs {
			cur, ok := byID[r.SkillID]
			if !ok {
				byID[r.SkillID] = weightedReq{SkillID: r.SkillID, Level: r.Level, Weight: weight}
				continue
			}
			// The hardest requirement on a skill gates the fit, and the heaviest weight any line gave it says
			// how much the fit is about that skill.
			if r.Level > cur.Level {
				cur.Level = r.Level
			}
			if weight > cur.Weight {
				cur.Weight = weight
			}
			byID[r.SkillID] = cur
		}
		return nil
	}
	if err := collect(t.Fit.HullTypeID, hullSkillWeight); err != nil {
		return nil, err
	}
	for _, mod := range t.Fit.Modules {
		if err := collect(mod.TypeID, 1); err != nil {
			return nil, err
		}
		if err := collect(mod.ChargeTypeID, 1); err != nil {
			return nil, err
		}
	}
	for _, d := range t.Fit.Drones {
		if err := collect(d.TypeID, 1); err != nil {
			return nil, err
		}
	}
	out := make([]weightedReq, 0, len(byID))
	for _, r := range byID {
		out = append(out, r)
	}
	return out, nil
}

// skillAffinity scores a pilot against a fit's requirements, 0..1, in two even halves. Headroom:
// how far past the minimum the pilot is. Depth: how trained they are in absolute terms, which stops
// a fit of level-1 requirements scoring full marks for everyone. A fit that requires nothing scores 0.5.
func skillAffinity(reqs []weightedReq, skills map[int64]int) float64 {
	if len(reqs) == 0 {
		return 0.5
	}
	var wsum, headroom, depth float64
	for _, r := range reqs {
		have := float64(skills[r.SkillID])
		need := float64(r.Level)
		h := 1.0
		if need < 5 {
			h = (have - need) / (5 - need)
		} else if have < 5 {
			h = 0
		}
		headroom += r.Weight * clamp01(h)
		depth += r.Weight * clamp01(have/5)
		wsum += r.Weight
	}
	if wsum <= 0 {
		return 0.5
	}
	return 0.5*(headroom/wsum) + 0.5*(depth/wsum)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ── racial bias ─────────────────────────────────────────────────────────────

var (
	hullRaceMu    sync.Mutex
	hullRaceCache = map[int64]int64{}
)

// hullRace is invTypes.raceID for a hull (0 = raceless, e.g. ORE and
// pirate hulls, which nobody gets a bias toward).
func (m *Mod) hullRace(ctx context.Context, typeID int64) int64 {
	hullRaceMu.Lock()
	r, ok := hullRaceCache[typeID]
	hullRaceMu.Unlock()
	if ok {
		return r
	}
	var race int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(raceID, 0) FROM invTypes WHERE typeID = ?`, typeID).Scan(&race)
	hullRaceMu.Lock()
	hullRaceCache[typeID] = race
	hullRaceMu.Unlock()
	return race
}

// charRace is the pilot's own race (character_.raceID).
func (m *Mod) charRace(ctx context.Context, charID int64) int64 {
	var race int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT raceID FROM character_ WHERE characterID = ?`, charID).Scan(&race)
	return race
}

// pilotRace is what a fit judgement needs to know about who will fly it: its
// race and how much that race matters to it (Tuning.Xenophobia).
type pilotRace struct {
	Race   int64
	Phobia float64
}

// charPilotRace reads both, once per judging pass rather than once per fit.
// A pilot without a tolerance trait (a human, an unseeded bot) has phobia 0.
func (m *Mod) charPilotRace(ctx context.Context, charID int64) pilotRace {
	pr := pilotRace{Race: m.charRace(ctx, charID)}
	var v int
	if m.db.QueryRowContext(ctx,
		`SELECT value FROM rolebot_traits WHERE characterID = ? AND trait = ?`,
		charID, TraitTolerance).Scan(&v) == nil {
		pr.Phobia = posPole(traitScale(v))
	}
	return pr
}

// ── the score ───────────────────────────────────────────────────────────────

// fitJudgement is why a fit scored what it scored, kept whole so the log and the panel can explain
// a choice instead of printing a bare number.
type fitJudgement struct {
	Power    float64
	Affinity float64
	SameRace bool
	// Foreign is a hull the pilot's xenophobia refuses (xenoItemMul). It is not dropped: it scores
	// xenoRefusedHullMul of itself, so it is chosen only when nothing the pilot will fly can do the job.
	Foreign  bool
	Score    float64
	// Unusable is power 0: this ship does not do this job. It is the only wrong-ship signal the module
	// has, derived and never declared.
	Unusable bool
}

// judgeFit scores one fit for one pilot. skills and charRace are passed in because a plan pass
// judges the whole pool for one character, and re-reading them per fit would repeat the queries.
func (m *Mod) judgeFit(ctx context.Context, t *fitTemplate, skills map[int64]int, who pilotRace) (fitJudgement, error) {
	var j fitJudgement
	power, err := m.fitPower(ctx, t)
	if err != nil {
		return j, err
	}
	j.Power = power
	if power <= 0 {
		j.Unusable = true
		return j, nil
	}
	reqs, err := m.fitSkillRequirements(ctx, t)
	if err != nil {
		return j, err
	}
	j.Affinity = skillAffinity(reqs, skills)
	race := m.hullRace(ctx, t.Fit.HullTypeID)
	j.SameRace = race != 0 && race == who.Race
	j.Score = power * (1 + skillAffinityWeight*j.Affinity)
	// The race term: the long-standing own-race affinity at tolerance zero,
	// and the xenophobe's stages above it (tolerance.go xenoItemMul).
	mul, ok := xenoItemMul(who.Race, who.Phobia, race)
	if !ok {
		j.Foreign = true
		mul = xenoRefusedHullMul
	}
	j.Score *= mul
	// The fit came out the way its pilot meant it to (fitstyle.go). Small, so
	// it only settles near-ties between finished fits.
	if styleMatches(t.Style, t.Lean) {
		j.Score *= styleMatchBonus
	}
	return j, nil
}

func init() {
	registerCharForget("fitCostCache/fitPowerCache", forgetFitDerived)
	registerSweep("fitDerived", sweepFitDerived)
}