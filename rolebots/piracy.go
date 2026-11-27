package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/rand"

	evedb "gurimoon/db"
	"gurimoon/seclaw"
)

// Piracy: robbing other pilots for a living.
//
// CONCORD is a stopwatch, not a wall: concordDelayForSecurity gives an aggressor
// 5 seconds at 0.9, 12 at 0.6, 16 at 0.5 and none below 0.45, and the 15-minute
// criminal flag travels with the pilot. So empire piracy is arithmetic (time to
// kill plus time to align out, against the clock) and lowsec piracy is free.
// The disposition is aggression times lawlessness (Tuning.PiracyAppetite): a
// violent lawful pilot or a pacifist outlaw never draws it. Phases: prowl, camp,
// loot (piracy_loot.go), fence and lielow. The fighting is the projection's
// combat loop (piracy_prey.go), not here.

const (
	// How long a pirate works one system before moving on. Longer than a
	// belt hunter's sweep: prey arrives on its own schedule, and a pirate
	// that keeps moving is a pirate that is never where somebody jumps in.
	piracyStintSec = 25 * 60
	// A gate camp is a longer commitment still: sitting is the tactic.
	piracyCampSec = 45 * 60
	// The temperate answers, before traits bend them (tuning.go). The security
	// ceiling default is lowsec, where there is no CONCORD, only station guns;
	// anything above is bought with carelessness and lawlessness.
	piracySecCeilDefault = 0.4
	// Seconds of margin left on the CONCORD clock by a pilot of even nerve.
	concordNerveSecDefault = 8
	// Odds a robbery is a robbery rather than a murder.
	ransomChanceDefault = 0.5
	// Odds the capsule dies too.
	podKillChanceDefault = 0.35
	// Odds a trip is spent on a gate rather than touring belts.
	gateCampChanceDefault = 0.35

	// The base demand, before envy multiplies it and the victim's hull scales it.
	// Small: a ransom that costs more than the ship is a ransom nobody pays.
	ransomBaseISK = 250_000
	// How long a victim has to pay, before patience bends it.
	ransomWindowSec = 60

	// What the criminal flag lasts, mirroring the core's aggressionDuration. A
	// pirate reads its own flag through the PvP bridge when it can; this is the
	// fallback and the margin added on top.
	criminalFlagSec   = 15 * 60
	criminalMarginSec = 45
)

// ── the derived config ──────────────────────────────────────────────────────

// piracyConfig is where this pilot will hunt and how it behaves when it finds
// somebody, derived from personality each time it plans (no stored form). Only
// abstract-tier decisions are here; the projection reads the rest off Tuning.
type piracyConfig struct {
	// The security band it will strike in. Floor is where it is willing to
	// BE (its own travel line); ceiling is where it is willing to commit a
	// crime, which is a different and lower number.
	SecMin, SecMax float64
	// Camp a gate rather than prowl belts on this trip.
	Camp bool
	// Terms, or straight to the guns: here only so the log line can say which kind
	// of pirate just flew out.
	RansomChance float64
}

func (m *Mod) piracyConfigDerived(b *botRun) piracyConfig {
	r := rulesForBot(b)
	// The floor is where the pilot works comfortably, the ceiling its conscience. If
	// the floor ends above the ceiling, widen down, never up: the ceiling is the safety limit.
	lo := math.Min(r.SecFloor, b.Tune.PersonalSecFloor)
	if r.SecFloor <= -1 {
		lo = b.Tune.PersonalSecFloor
	}
	hi := b.Tune.PiracySecCeil
	// A pilot past the outlaw line has no reputation left to protect: widen the band
	// up to the police line. Additive, so it composes with the lawlessness trait.
	if b.SecStatus <= seclaw.OutlawStatus {
		hi = math.Max(hi, policedSecLine)
	}
	if lo > hi {
		lo = -1
	}
	return piracyConfig{
		SecMin:       lo,
		SecMax:       hi,
		Camp:         rand.Float64() < b.Tune.GateCampChance,
		RansomChance: b.Tune.RansomChance,
	}
}

// ── the appetite probe ──────────────────────────────────────────────────────

// piracyOpportunity is the capability probe plus the two things that stop a
// pirate flying out today: a criminal flag it has not finished paying off,
// and a wreck it left behind that is worth more than a new robbery.
func piracyOpportunity(m *Mod, ctx context.Context, b *botRun) (float64, string) {
	f, why := opportunityFromFits(JobPiracy)(m, ctx, b)
	if f == 0 {
		return 0, why
	}
	// A lawful employer (a Security or Defence wing) does not keep pirates on its roster: the
	// doctrine's whole promise is that its guns commit no crime (roeAllows). Only a classified
	// corporation answers; an NPC corporation has no doctrine row and leaves it to the pilot.
	if b.CorpID != 0 {
		if doc, _, known := m.corpDoctrineOf(ctx, b.CorpID); known && doc.Lawful() {
			return 0, "employer's " + doc.String() + " doctrine forbids piracy"
		}
	}
	// A cartel's raiding strength is capped like any wing (corpwing.go): predation
	// sets the doctrine and industrial tags cut the wing to a quarter.
	if ok, reason := m.wingAllows(ctx, b); !ok {
		return 0, reason
	}
	// Heat is not a reason to refuse the work (lying low is part of it, planned
	// below) but to be less keen: a flagged pirate hunting again dies to a station gun.
	if until := m.piracyHeatUntil(b); until > clock.Unix() {
		return f * 0.5, why + "; still carrying a criminal timer"
	}
	// A ship with something in the hold wants fencing, not another fight.
	if used, err := m.holdUsedM3(ctx, b.CharID, b.ShipID); err == nil && used > 0 {
		return f * 1.2, why + "; hold still has somebody else's cargo in it"
	}
	// An outlaw is keener, not warier: the record is already at the floor, so the
	// marginal cost of the next crime has collapsed.
	if b.SecStatus <= seclaw.OutlawStatus {
		return f * 1.3, why + "; already an outlaw, nothing left to lose"
	}
	return f, why
}

// piracyHalfLife is how long a robbery takes to wear thin: double the ordinary
// satiation, since piracy is something a pilot is and a camp should outlast the
// wait. A function because both the catalogue entry and the robbery tactic
// (robberyWeight) need it, and the tactic cannot read the catalogue.
func piracyHalfLife(tn Tuning) float64 { return tn.SatiationHalfLifeSec * 2 }

// piracyHeatUntil is when this pilot judges it safe to be seen in policed
// space again: the criminal flag the core is actually tracking when the PvP
// bridge is available (it travels with the pilot, so the pilot's CURRENT
// system is the one to ask about), else whatever the bot last wrote down.
func (m *Mod) piracyHeatUntil(b *botRun) int64 {
	stored := b.State.Data.PiracyHeatUntil
	if ts := criminalSince(b.SystemID, b.CharID); ts > 0 {
		if live := ts + criminalFlagSec + criminalMarginSec; live > stored {
			return live
		}
	}
	return stored
}

// ── planning ────────────────────────────────────────────────────────────────

// piracyPlan is entered from idle: settle the bill first, then go hunting.
func (m *Mod) piracyPlan(ctx context.Context, b *botRun) error {
	st := b.State
	st.Activity = ActPiracy

	// Unfinished work from a previous robbery comes before a new one: a flagged
	// pirate must not fly to a market, nor one holding stolen goods start a fight.
	if until := m.piracyHeatUntil(b); until > clock.Unix() {
		return m.piracyGoToGround(ctx, b, until)
	}
	if handled, err := m.piracyResumeLoot(ctx, b); err != nil {
		return err
	} else if handled {
		return nil
	}
	if handled, err := m.piracyResumeFence(ctx, b); err != nil {
		return err
	} else if handled {
		return nil
	}

	cfg := m.piracyConfigDerived(b)

	ready, err := m.ensureActivityShip(ctx, b, ActPiracy)
	if err != nil || !ready {
		return err
	}

	sysID, gateID, name, err := m.piracySite(ctx, b, cfg)
	if err != nil {
		return err
	}

	keep := st.Data
	st.Data = idleData(keep)
	st.Data.PiracySystemID = sysID
	st.Data.PiracyPickedAt = clock.Unix()
	st.Data.PiracyGateID = gateID

	next := TaskPiracyProwl
	if gateID != 0 {
		next = TaskPiracyCamp
		m.logBot(b.CharID, logInfo, "piracy",
			"setting up a gate camp in %s — %s", name, piracyDoctrine(cfg))
	} else {
		m.logBot(b.CharID, logInfo, "piracy",
			"prowling the belts of %s — %s", name, piracyDoctrine(cfg))
	}
	return m.beginTravel(ctx, b, sysID, 0, string(next))
}

// piracyDoctrine says in one clause what this pilot intends to do to whoever
// it finds, for the log. An operator reading a roster's logs should be able
// to tell the ransomers from the murderers without opening a profile.
func piracyDoctrine(cfg piracyConfig) string {
	switch {
	case cfg.RansomChance >= 0.75:
		return "ransoming whoever it catches"
	case cfg.RansomChance >= 0.35:
		return fmt.Sprintf("ransom or murder, about %.0f%% terms", cfg.RansomChance*100)
	case cfg.RansomChance > 0:
		return "mostly just killing them"
	}
	return "killing them"
}

// piracySite picks where to work: the system, and the stargate to sit on when
// this trip is a camp. Each crime-band system within the search bound is scored
// on prey (the roster's exposed bots there), distance (scored up, capped) and
// roughness (lower security wins). Whether the prey can be beaten is decided later.
func (m *Mod) piracySite(ctx context.Context, b *botRun, cfg piracyConfig) (int64, int64, string, error) {
	u, err := m.universe(ctx)
	if err != nil {
		return 0, 0, "", err
	}
	// Scoped like every other venue pick (residency.go): the raid goes into the home
	// region or the next one over.
	r := m.economicRules(ctx, b, defaultBand(b))

	recent := map[int64]bool{}
	for _, id := range b.State.Data.HuntRecent {
		recent[id] = true
	}

	preyAt := m.exposedBotsBySystem(ctx)
	var sitesAt map[int64]int // claimed sites per system (raid.go)
	if raidOn() {
		sitesAt = claimedSitesBySystem()
	}

	type cand struct {
		sysID int64
		sec   float64
		score float64
	}
	var best *cand
	seen := map[int64]bool{b.SystemID: true}
	frontier := []int64{b.SystemID}
	for hops := 0; hops < piracySearchHops && len(frontier) > 0; hops++ {
		for _, sysID := range frontier {
			ms, ok := u.meta[sysID]
			if !ok || recent[sysID] {
				continue
			}
			sec := dispSec(ms.Security)
			if sec < cfg.SecMin || sec > cfg.SecMax {
				continue
			}
			prey := preyAt[sysID]
			if prey > piracyPreyCap {
				prey = piracyPreyCap
			}
			away := hops
			if away > piracyAwayCap {
				away = piracyAwayCap
			}
			score := float64(prey)*piracyPreyWorth +
				float64(away)*piracyAwayWorth +
				(1-sec)*piracyRoughWorth
			// Where the outfit's marks work (corpceo_diplomacy.go): a pirate corporation
			// that declared an industrial one its prey goes where that corporation mines.
			score += relMarkFor(b.CorpID, sysID)
			// Where a site is being run outside highsec: a runner pinned in a
			// pocket with a hold full of what the rooms dropped (raid.go).
			score += raidGroundScore(sitesAt[sysID], secBand(sec) == BandPoliced)
			if best == nil || score > best.score {
				best = &cand{sysID, sec, score}
			}
		}
		var next []int64
		for _, sysID := range frontier {
			for _, nb := range u.adj[sysID] {
				if seen[nb] {
					continue
				}
				seen[nb] = true
				// allowed() is the floor, the region scope and an outlaw's security ceiling, so
				// the walk never picks a venue the pilot cannot enter.
				if !u.allowed(nb, r) {
					continue // the walk stops at the scope's region line
				}
				next = append(next, nb)
			}
		}
		frontier = next
	}
	if best == nil {
		return 0, 0, "", fmt.Errorf(
			"no system between %.1f and %.1f security is reachable that this pilot has not just worked"+
				" — its crime ceiling is %.1f (lawfulness and nerve) and its travel floor%s",
			cfg.SecMin, cfg.SecMax, cfg.SecMax, r.limitNote())
	}

	gateID := int64(0)
	if cfg.Camp {
		// A camp goes where the traffic is (lane.go): the lawless end of the busiest lane
		// in reach, with the same crime band and travel rules as the prey walk above.
		if laneTrafficOn() {
			admit := func(sysID int64) bool {
				ms, ok := u.meta[sysID]
				if !ok || !u.allowed(sysID, r) {
					return false
				}
				sec := dispSec(ms.Security)
				return sec >= cfg.SecMin && sec <= cfg.SecMax
			}
			if sys, gate, far, ok := m.pickCampLane(ctx, b, lanePirate, piracySearchHops, nil, admit, recent); ok {
				m.logBot(b.CharID, logInfo, "piracy", "the %s–%s lane carries the traffic — camping its %s gate",
					m.nameOfSystem(ctx, sys), m.nameOfSystem(ctx, far), m.nameOfSystem(ctx, far))
				return sys, gate, m.nameOfSystem(ctx, sys), nil
			}
		}
		// No lane known in reach: any gate of the prey walk's pick, random so a roster
		// of campers spreads rather than piles onto one.
		if err := m.db.QueryRowContext(ctx, `
			SELECT itemID FROM mapDenormalize
			WHERE solarSystemID = ? AND groupID = 10
			ORDER BY RAND() LIMIT 1`, best.sysID).Scan(&gateID); err != nil {
			gateID = 0 // no gates resolved: prowl the belts instead
		}
	}
	return best.sysID, gateID, m.nameOfSystem(ctx, best.sysID), nil
}

// dialLaneTraffic switches the lane picker: on, a camp sits on the busiest lane
// the ledgers know; off, it takes a random gate of the prey walk's pick.
var dialLaneTraffic = &pvpDial{key: "rolebots.piracy.laneTraffic", def: 1, lo: 0, hi: 1, step: 1,
	label: "Gate camps follow recorded traffic (1 = on, 0 = a random gate)"}

func laneTrafficOn() bool { return dialLaneTraffic.Get() >= 0.5 }

// piracySearchHops bounds the hunting-ground search. A pirate looks around its
// own neighbourhood for somewhere lawless; the route out of empire is where its
// own flag gets it shot.
const piracySearchHops = 8

// The hunting-ground score's weights, denominated against each other: one working
// pilot on grid is worth three jumps of distance and the whole security spread
// two. Prey dominates, so an empty lawless pocket loses to a hisec belt with miners.
const (
	piracyPreyWorth  = 3.0
	piracyPreyCap    = 5
	piracyAwayWorth  = 1.0
	piracyAwayCap    = 6
	piracyRoughWorth = 2.0
)

// exposedBotsBySystem counts the roster's working, space-exposed bots per
// system: the prey map the hunting-ground score reads. One grouped query per
// site pick; the phase list comes from the task registry, so a new exposed task
// is prey once registered.
func (m *Mod) exposedBotsBySystem(ctx context.Context) map[int64]int {
	var phases []string
	for kind, def := range taskRegistry {
		if def.Exposed {
			phases = append(phases, string(kind))
		}
	}
	if len(phases) == 0 {
		return nil
	}
	args := make([]interface{}, len(phases))
	for i, p := range phases {
		args[i] = p
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.solarSystemID, COUNT(*)
		FROM rolebot_state s
		JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = s.characterID
		WHERE s.phase IN (`+placeholders(len(phases))+`) AND COALESCE(c.stationID, 0) = 0
		GROUP BY c.solarSystemID`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var sysID int64
		var n int
		if rows.Scan(&sysID, &n) == nil {
			out[sysID] = n
		}
	}
	return out
}

// ── the steps ───────────────────────────────────────────────────────────────

// piracyStep dispatches the phases. One entry point for all five so the
// registry has a single Step per task and the ordering rules below (heat
// first, then loot, then everything else) cannot be bypassed by a phase.
func (m *Mod) piracyStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State

	// A criminal timer outranks whatever the pilot was doing, except in the phases
	// that serve it: hit, run, hide, come back.
	if st.Phase != string(TaskPiracyLieLow) && st.Phase != string(TaskPiracyFence) {
		if until := m.piracyHeatUntil(b); until > clock.Unix() {
			return false, m.piracyGoToGround(ctx, b, until)
		}
	}

	switch TaskKind(st.Phase) {
	case TaskPiracyProwl:
		return m.piracyProwlStep(ctx, b)
	case TaskPiracyCamp:
		return m.piracyCampStep(ctx, b)
	case TaskPiracyLoot:
		return m.piracyLootStep(ctx, b)
	case TaskPiracyFence:
		return m.piracyFenceStep(ctx, b)
	case TaskPiracyLieLow:
		return m.piracyLieLowStep(ctx, b)
	}
	st.Activity, st.Phase = "", string(TaskIdle)
	return false, saveState(ctx, m.db, st)
}

// piracyProwlStep tours the belts of the hunting ground. The actual violence
// is the projection's (piracy_prey.go) or the abstract tier's; this only
// keeps the ship moving between places somebody might be, and gives up on the
// system when the stint runs out.
func (m *Mod) piracyProwlStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if st.Data.PiracySystemID == 0 {
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}
	if clock.Unix()-st.Data.PiracyPickedAt >= piracyStintSec {
		st.Data.HuntRecent = pushHuntRecent(st.Data.HuntRecent, st.Data.PiracySystemID)
		st.Activity, st.Phase = "", string(TaskIdle)
		m.logBot(b.CharID, logInfo, "piracy", "nothing worth robbing here — moving on")
		return false, saveState(ctx, m.db, st)
	}
	if b.SystemID != st.Data.PiracySystemID {
		return false, m.beginTravel(ctx, b, st.Data.PiracySystemID, 0, string(TaskPiracyProwl))
	}
	if b.ShipID == 0 {
		return false, fmt.Errorf("pirate has no ship boarded")
	}
	m.hopPatrolBelt(ctx, b)
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(
		int64(gangPatrolDwellMinSec+rand.Intn(gangPatrolDwellMaxSec-gangPatrolDwellMinSec+1)))
	return false, saveState(ctx, m.db, st)
}

// piracyCampStep sits on the gate. Nothing happens here: the ship is parked, the
// posture holds it there, and the projection watches the grid.
func (m *Mod) piracyCampStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if st.Data.PiracySystemID == 0 || st.Data.PiracyGateID == 0 {
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}
	if clock.Unix()-st.Data.PiracyPickedAt >= piracyCampSec {
		st.Data.HuntRecent = pushHuntRecent(st.Data.HuntRecent, st.Data.PiracySystemID)
		st.Activity, st.Phase = "", string(TaskIdle)
		m.logBot(b.CharID, logInfo, "piracy", "camp is cold — breaking it up")
		return false, saveState(ctx, m.db, st)
	}
	if b.SystemID != st.Data.PiracySystemID {
		return false, m.beginTravel(ctx, b, st.Data.PiracySystemID, 0, string(TaskPiracyCamp))
	}
	if b.ShipID == 0 {
		return false, fmt.Errorf("pirate has no ship boarded")
	}
	// Arriving at the camp: take the spot on the arrival side of the gate, where a
	// jump-in lands (lane.go), and say so once.
	if !st.Data.PiracyCampSet {
		m.parkAtGateArrival(ctx, b.ShipID, st.Data.PiracyGateID)
		m.pirateSayRun(b, evCamp)
		st.Data.PiracyCampSet = true
	}
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(60)
	return false, saveState(ctx, m.db, st)
}

// piracyLieLowStep is the bill for a crime committed in policed space: sit
// somewhere the police are not until the flag lapses. It does not dock: empire
// stations refuse a flagged pilot, and staying in space means it is already out
// here and working when the timer lapses.
func (m *Mod) piracyLieLowStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	until := m.piracyHeatUntil(b)
	if until <= clock.Unix() {
		st.Data.PiracyHeatUntil = 0
		st.Activity, st.Phase = "", string(TaskIdle)
		m.logBot(b.CharID, logInfo, "piracy", "timer's up — nobody is looking for it any more")
		return false, saveState(ctx, m.db, st)
	}
	if st.Data.TargetSystemID != 0 && b.SystemID != st.Data.TargetSystemID {
		return false, m.beginTravel(ctx, b, st.Data.TargetSystemID, 0, string(TaskPiracyLieLow))
	}
	st.Data.LegEndsAt = clock.Unix() + 60
	return false, saveState(ctx, m.db, st)
}

// piracyGoToGround sends a flagged pirate somewhere CONCORD does not go.
// Already outside the police umbrella? Then it is already hiding, and it
// simply waits where it is rather than flying anywhere.
func (m *Mod) piracyGoToGround(ctx context.Context, b *botRun, until int64) error {
	st := b.State
	st.Activity = ActPiracy
	st.Phase = string(TaskPiracyLieLow)
	st.Data.PiracyHeatUntil = until

	mins := (until - clock.Unix() + 59) / 60
	if !m.inPolicedSpace(ctx, b.SystemID) {
		st.Data.TargetSystemID = 0
		st.Data.LegEndsAt = clock.Unix() + 60
		m.logBot(b.CharID, logInfo, "piracy",
			"criminal timer running — sitting it out here, about %d minute(s)", mins)
		return saveState(ctx, m.db, st)
	}

	sysID, name, err := m.nearestLawlessSystem(ctx, b)
	if err != nil {
		// Nowhere to run: wait it out where it stands and take what the station guns
		// give. Better than a halted bot.
		st.Data.TargetSystemID = 0
		st.Data.LegEndsAt = clock.Unix() + 60
		m.logBot(b.CharID, logWarn, "piracy",
			"flagged in policed space with no lawless system in reach (%v) — riding it out", err)
		return saveState(ctx, m.db, st)
	}
	st.Data.TargetSystemID = sysID
	m.logBot(b.CharID, logInfo, "piracy",
		"flagged — running for %s to sit out about %d minute(s)", name, mins)
	return m.beginTravel(ctx, b, sysID, 0, string(TaskPiracyLieLow))
}

// inPolicedSpace reports whether CONCORD answers calls here: the same 0.45 line
// the core's concordMinSec draws.
func (m *Mod) inPolicedSpace(ctx context.Context, sysID int64) bool {
	u, err := m.universe(ctx)
	if err != nil {
		return true // assume the worst: hide rather than loiter
	}
	ms, ok := u.meta[sysID]
	return !ok || dispSec(ms.Security) >= 0.45
}

// nearestLawlessSystem is the closest system CONCORD does not police, by raw
// gate distance. It applies no travel rule (not the crime ceiling, not security
// aversion): this is where the pirate wants to not be arrested, and every route
// out of empire runs through the empire it is in.
func (m *Mod) nearestLawlessSystem(ctx context.Context, b *botRun) (int64, string, error) {
	u, err := m.universe(ctx)
	if err != nil {
		return 0, "", err
	}
	seen := map[int64]bool{b.SystemID: true}
	frontier := []int64{b.SystemID}
	for hops := 0; hops < piracySearchHops && len(frontier) > 0; hops++ {
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
				if dispSec(nm.Security) < 0.45 {
					return nb, m.nameOfSystem(ctx, nb), nil
				}
				next = append(next, nb)
			}
		}
		frontier = next
	}
	return 0, "", fmt.Errorf("no lowsec or nullsec system within %d jumps", piracySearchHops)
}

// ── the CONCORD stopwatch ───────────────────────────────────────────────────

// concordDelayDefault is the core's response delay per rounded band,
// [1.0/0.9, 0.8, 0.7, 0.6, 0.5], in seconds (cmd/gurimoon/concord.go).
var concordDelayDefault = []float64{5, 8, 10, 12, 16}

// concordDelayFor is the police delay in seconds at this security, the same
// table the core's grid response reads: the server_settings row
// concord.responseDelaySec when set, else concordDelayDefault. The pirate uses
// it to decide before any crime exists, and the abstract tier's stopwatch
// (abstractConcord) uses it to settle one.
func concordDelayFor(sec float64) (float64, bool) {
	if sec < concordMinSecurity {
		return 0, false
	}
	d := evedb.CachedSettingFloats("concord.responseDelaySec", concordDelayDefault, 0, 600)
	switch r := math.Round(sec*10) / 10; {
	case r >= 0.9:
		return d[0], true
	case r >= 0.8:
		return d[1], true
	case r >= 0.7:
		return d[2], true
	case r >= 0.6:
		return d[3], true
	}
	return d[4], true
}

// piracyStrikeWindow answers whether a pirate can kill this and be gone before
// the police arrive in empire space. ttkSec is how long the guns need, alignSec
// how long the ship takes to leave, both off the fit. Below 0.45 there is no
// police response and the answer is always yes.
func piracyStrikeWindow(sec, ttkSec, alignSec, nerveSec float64) (ok bool, budget float64) {
	delay, policed := concordDelayFor(sec)
	if !policed {
		return true, math.Inf(1)
	}
	return ttkSec+alignSec+nerveSec <= delay, delay
}

// ── the panel line ──────────────────────────────────────────────────────────

// detailPiracy is what the roster shows about a pirate mid-robbery.
func detailPiracy(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	switch TaskKind(st.Phase) {
	case TaskPiracyCamp:
		left := (piracyCampSec - (clock.Unix() - d.PiracyPickedAt)) / 60
		return fmt.Sprintf("gate camp in %s, %d min left — %d kill(s), %d ransom(s) paid",
			m.nameOfSystem(ctx, d.PiracySystemID), left, d.PiracyKills, d.PiracyRansomed)
	case TaskPiracyProwl:
		left := (piracyStintSec - (clock.Unix() - d.PiracyPickedAt)) / 60
		return fmt.Sprintf("prowling %s, %d min left — %d kill(s), %d ransom(s) paid",
			m.nameOfSystem(ctx, d.PiracySystemID), left, d.PiracyKills, d.PiracyRansomed)
	case TaskPiracyLoot:
		return fmt.Sprintf("stripping a wreck in %s", m.nameOfSystem(ctx, d.PiracyLootSysID))
	case TaskPiracyFence:
		return "selling stolen goods"
	case TaskPiracyLieLow:
		mins := (d.PiracyHeatUntil - clock.Unix() + 59) / 60
		if mins < 0 {
			mins = 0
		}
		return fmt.Sprintf("lying low, about %d minute(s) of criminal timer left", mins)
	}
	return ""
}

// ── the victim's own record of it ───────────────────────────────────────────

// piracyRecordVictim writes the robbery into the victim's log, when the victim
// is one of ours. A real player's log is the game's own: the kill mail and the
// criminal flag say everything it needs to.
func (m *Mod) piracyRecordVictim(ctx context.Context, pirateCharID, victimCharID int64, killed bool) {
	if victimCharID == 0 || pirateCharID == victimCharID {
		return
	}
	rb, err := getRolebot(ctx, m.db, victimCharID)
	if err != nil || rb == nil {
		return // not one of ours
	}
	// The victim's own ledger (grace_ledger.go); a kill is filed by the death path.
	m.noteGraceEventAsync(victimCharID, pirateCharID, gkRobbery, fightFacts{}, 0, 0)
	if killed {
		m.logBot(victimCharID, logWarn, "piracy",
			"killed and robbed by character %d", pirateCharID)
		return
	}
	m.logBot(victimCharID, logWarn, "piracy", "robbed by character %d", pirateCharID)
}
