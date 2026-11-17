package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/seclaw"
)

// Robbing somebody on a live grid.
//
// Everything that happens when a victim shows up happens here, at combat tick
// rate in the projection's goroutine, hung off the combat loop every rolebot
// fight uses. Piracy adds three things that loop never had: prey (a target that
// has not shot anybody), the clock (a crime in empire space is a stopwatch,
// piracy.go, so a pirate that cannot finish in time does not start) and terms
// (the point goes on, the guns stay cold and Local gets a price). The ransom is
// real: the scrambler uses the destiny call a player's module uses, and if it
// drops the victim leaves.

const (
	// A victim gets this long to answer a demand before the guns start, times
	// the pilot's patience.
	ransomWaitSec = ransomWindowSec
	// How often the pirate checks its wallet while it waits.
	ransomPollSec = 3
	// A pilot who has just been ransomed is left alone for this long, so a paid mark
	// is not re-pointed by the same bot, which would make paying pointless.
	ransomAmnestySec = 300
)

// ransomState is one robbery in progress, on the pirate's projection.
type ransomState struct {
	victimCharID int64
	victimName   string
	demanded     float64
	demandedAt   time.Time
	baseline     float64 // the pirate's ISK when the demand went out
	// settled means the negotiation is over and the ordinary combat loop is back in
	// charge; released means it ended in payment. They are two flags because a
	// released mark must never be shot at again by this projection.
	settled  bool
	released bool

	// A cargo demand (camp.go): the hold is asked for because the scan reads it above
	// any price a pilot would type. cansBefore is every container near the mark when
	// the demand went out, so a new one is compliance. botAnswered: a bot jettisons once.
	cargoDemand bool
	cargoISK    float64
	hullISK     float64
	cansBefore  map[int64]bool
	botAnswered bool
	victimShip  int64
	// toll marks a checkpoint's demand (camp.go): a mark that does not pay is
	// released, never shot for refusing.
	toll bool
	// heldFleet marks that opening these terms put the fleet on hold
	// (fleetHoldCalled): the line fires on the commander's call alone while a
	// price stands in Local, and the hold is lifted when the terms end.
	heldFleet bool
}

// amnesty is every pilot recently released for payment, so no bot re-robs a mark
// that just paid. Process-wide and in memory; five minutes is all a ransom needs
// and a checkpoint's toll buys an hour (grantAmnestyFor).
var (
	amnestyMu sync.Mutex
	amnesty   = map[int64]time.Time{} // charID to until
)

func grantAmnesty(charID int64) {
	grantAmnestyFor(charID, ransomAmnestySec*time.Second)
}

// grantAmnestyFor is a grant with its own term. A longer grant never shortens
// a standing one.
func grantAmnestyFor(charID int64, d time.Duration) {
	until := time.Now().Add(d)
	amnestyMu.Lock()
	if cur, ok := amnesty[charID]; !ok || until.After(cur) {
		amnesty[charID] = until
	}
	amnestyMu.Unlock()
}

// sweepAmnesty forgets expired grants. underAmnesty drops one when the mark is
// considered again; this covers a pilot who paid once and never crossed a
// pirate's grid a second time, whose entry would otherwise linger.
func sweepAmnesty() {
	now := time.Now()
	amnestyMu.Lock()
	for charID, until := range amnesty {
		if now.After(until) {
			delete(amnesty, charID)
		}
	}
	amnestyMu.Unlock()
}

func underAmnesty(charID int64) bool {
	amnestyMu.Lock()
	defer amnestyMu.Unlock()
	until, ok := amnesty[charID]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(amnesty, charID)
		return false
	}
	return true
}

// ── choosing a victim ───────────────────────────────────────────────────────

// preyOnGrid is every pilot this projection would rob, ignoring whether it can;
// the legality clock and the range checks come later. Called from hostilesOnGrid
// when the posture says prey.
func (m *Mod) preyOnGrid(eb *embodiedBot) []uint32 {
	if eb.ent == nil || eb.ss == nil {
		return nil
	}
	var out []uint32
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if !m.wouldRob(eb, e) {
			continue
		}
		out = append(out, e.ID)
	}
	return out
}

// mayTargetPilot is the structural half of the prey filter: the clauses about
// what a ball is and who it belongs to. A pirate asks whether a target is worth
// robbing (wouldRob) and a combat pilot whether it may be shot (wouldEngagePilot,
// roe.go); both share these vetoes (capsules, kin, corpmates, gangmates).
func (m *Mod) mayTargetPilot(eb *embodiedBot, e *destiny.Entity) bool {
	if e == nil || !e.IsPlayer || e.ID == eb.ent.ID {
		return false
	}
	if e.IsDestroyed() || e.IsInWarp() || e.IsCloaked() {
		return false
	}
	victimCharID := e.Slim.CharID
	if victimCharID == 0 || victimCharID == eb.charID {
		return false
	}
	// A capsule is not prey. Whether the pod dies is decided once, when its ship
	// dies (markPod), and only that pod, for a few seconds.
	if e.Slim.TypeID == capsuleTypeID {
		return eb.podMarked(victimCharID)
	}
	// Corp mates.
	if eb.corpID != 0 && e.Slim.CorpID == eb.corpID {
		return false
	}
	// Kin: the same alliance or employer faction (kinship.go). Standing cannot carry
	// this (strangers of one faction stand at 0); it forbids only starting a fight.
	if m.gridKin(eb, e) {
		return false
	}
	// Gangmates. A pack that read its own members as prey would spend the
	// camp robbing itself; player riders in the gang count too.
	if eb.gangID != 0 && m.gangOfCharCached(victimCharID) == eb.gangID {
		return false
	}
	return true
}

// wouldRob is the robbery half: what a pirate wants out of a grid. Everything
// structural is mayTargetPilot's; what is left is the value judgement.
func (m *Mod) wouldRob(eb *embodiedBot, e *destiny.Entity) bool {
	if !m.mayTargetPilot(eb, e) {
		return false
	}
	victimCharID := e.Slim.CharID
	// Somebody it thinks well of (faction.go): its own opinion of that pilot and what
	// their employers think of each other. The grid variant, since this may not query.
	if m.gridStandingToPilot(eb, e) >= StandingLiked {
		return false
	}
	// Somebody who just paid: re-pointing a settled mark would teach that paying
	// does not work.
	if underAmnesty(victimCharID) {
		return false
	}
	// Another pirate mid-crime: two bot pirates on one gate that read each other as
	// prey would kill each other instead of the traffic.
	if since := modapi.PvP().CriminalSince; since != nil && since(eb.systemID, victimCharID) > 0 {
		return false
	}
	// Anything that already shot at this bot is handled by the ordinary aggressor
	// path and is already in the hostile list.
	return true
}

// wouldEngagePilot is the combat half: may this pilot open fire on that one,
// here, now (roe.go). Unlike wouldRob it does not spare a criminal or a mark
// under amnesty, and ignores cargo. The decider ID is the gang, negated so it
// cannot collide with a character ID, giving one decision per fleet (roe.go).
func (m *Mod) wouldEngagePilot(eb *embodiedBot, e *destiny.Entity) bool {
	if !m.mayTargetPilot(eb, e) {
		return false
	}
	victimCharID := e.Slim.CharID
	criminal := false
	if since := modapi.PvP().CriminalSince; since != nil {
		criminal = since(eb.systemID, victimCharID) > 0
	}
	// The ENGAGE opinion, not the full one: a navy's race hate shapes who it
	// flies with, never who it shoots (engageStanding, roe.go).
	st := stanceOf(m.gridEngageStandingToPilot(eb, e), false)
	band := secBand(loadedSystemSecurity(eb.systemID))

	// A declared war between the two owners makes policed space shootable (roe.go).
	// A snapshot read (combat goroutine); warEverywhere() is the operator's switch.
	atWar := false
	if warEverywhere() {
		selfAlly := int64(0)
		if eb.ent != nil {
			selfAlly = eb.ent.Slim.AllianceID
		}
		atWar = relAtWar(eb.corpID, selfAlly, e.Slim.CorpID, e.Slim.AllianceID)
	}

	decider := eb.charID
	if eb.gangID != 0 {
		decider = -eb.gangID
	}
	// The ball carries the pilot's own record (embody.go stamps it for a
	// rolebot; the core stamps it for a player), so this costs nothing.
	outlaw := e.Slim.SecurityStatus <= seclaw.OutlawStatus
	ok, _ := roeEngage(st, eb.doctrine, band, criminal, outlaw, atWar,
		eb.wingNerve, decider, victimCharID)
	return ok
}

// pilotIsFlagged reports whether a pilot is a legal target for anybody: a
// crimewatch criminal flag or an outlaw security record, the two facts roe.go
// calls fair game in every band. A war or a stance is not a flag.
func (m *Mod) pilotIsFlagged(eb *embodiedBot, e *destiny.Entity) bool {
	if e == nil || e.Slim.CharID == 0 {
		return false
	}
	if e.Slim.SecurityStatus <= seclaw.OutlawStatus {
		return true
	}
	if since := modapi.PvP().CriminalSince; since != nil {
		return since(eb.systemID, e.Slim.CharID) > 0
	}
	return false
}

// flaggedPilotsOnGrid is the bystander sweep: the pilots on this grid that anyone
// may shoot, for a bot not on a combat posture, so a crime in a crowd does not go
// unanswered. It runs the same wouldEngagePilot as every other path, so the
// doctrine gate and the kin, corpmate, gangmate and capsule vetoes still apply.
func (m *Mod) flaggedPilotsOnGrid(eb *embodiedBot) []uint32 {
	if eb.ent == nil || eb.ss == nil || !defendVsCriminalEnabled() {
		return nil
	}
	// A pacifist leaves the criminal to someone else (takesElectiveFights).
	if !takesElectiveFights(eb.tune) {
		return nil
	}
	var out []uint32
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if !m.pilotIsFlagged(eb, e) {
			continue
		}
		if !m.wouldEngagePilot(eb, e) {
			continue
		}
		out = append(out, e.ID)
	}
	return out
}

// warTargetsOnGrid is what a pilot in transit takes on of its own accord: the pilots on its grid
// whose corporation or alliance is at war with its own, that the rules of engagement let it shoot
// (wouldEngagePilot: a combat wing, a hostile stance, none of its own), and that it has the nerve to
// stay for (transitWarRoll, one sticky choice per fleet and enemy). The pilot that passes flies on.
func (m *Mod) warTargetsOnGrid(eb *embodiedBot) []uint32 {
	if eb == nil || eb.ent == nil || eb.ss == nil || eb.posture != PostureTransit {
		return nil
	}
	// A pacifist flies on (takesElectiveFights).
	if !takesElectiveFights(eb.tune) {
		return nil
	}
	if eb.doctrine == DoctrineNone || !warEverywhere() || !relCorpAtWar(eb.corpID) {
		return nil
	}
	ally := eb.ent.Slim.AllianceID
	decider := eb.charID
	if eb.gangID != 0 {
		decider = -eb.gangID
	}
	odds := transitWarOdds(transitWarChance(), eb.wingNerve)
	var out []uint32
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if e == nil || !e.IsPlayer || e.Slim.CharID == 0 {
			continue
		}
		if !relAtWar(eb.corpID, ally, e.Slim.CorpID, e.Slim.AllianceID) {
			continue
		}
		if !m.wouldEngagePilot(eb, e) {
			continue
		}
		if !transitWarRoll(decider, e.Slim.CharID, odds) {
			continue
		}
		out = append(out, e.ID)
	}
	return out
}

// engageablePilots is the grid sweep behind wouldEngagePilot, mirroring
// preyOnGrid above.
func (m *Mod) engageablePilots(eb *embodiedBot) []uint32 {
	if eb.ent == nil || eb.ss == nil {
		return nil
	}
	var out []uint32
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if !m.wouldEngagePilot(eb, e) {
			continue
		}
		out = append(out, e.ID)
	}
	return out
}

// pickPrey chooses which pilot to rob: the fattest, slowest thing in reach. It is
// not the ordinary combat scorer (closest to dying): cargo capacity makes a target
// worth robbing, low speed makes it catchable, and a big tank makes it a fight.
func (m *Mod) pickPrey(eb *embodiedBot, l *combatLoadout, prey []uint32) uint32 {
	if eb.ent == nil || eb.ss == nil || len(prey) == 0 {
		return 0
	}
	selfPos := eb.ent.Pos()
	reach := l.targetRnge
	if eb.ent != nil {
		reach *= eb.ent.EwarTotals().MaxTargetRangeMult
	}

	var best uint32
	bestScore := -math.MaxFloat64
	for _, id := range prey {
		e := eb.ss.GetEntity(id)
		if e == nil {
			continue
		}
		dist := e.Pos().Sub(selfPos).Len()
		if dist > reach {
			continue
		}
		hp := e.HPSnapshot()
		tank := hp.MaxShield + hp.MaxArmor + hp.MaxStructure
		// Worth robbing, easy to catch, cheap to kill, and near.
		score := float64(m.preyCargoM3(e))*2 -
			tank/50 -
			dist/1000
		// A pilot of a corporation this outfit has marked (corpceo_diplomacy.go) is the one
		// it came for: the bonus lets a mark beat a fatter stranger, still within reach.
		switch v := relCorpStanding(eb.corpID, e.Slim.CorpID); {
		case v <= StandingDespised:
			score += preyMarkBonus * 2
		case v <= StandingDisliked:
			score += preyMarkBonus
		}
		if score > bestScore {
			best, bestScore = id, score
		}
	}
	return best
}

// preyMarkBonus is what a marked corporation's pilot adds to pickPrey's
// score, in its own units (m³ of hold × 2).
const preyMarkBonus = 2000.0

// preyCargoM3 is the victim's hold (invTypes.capacity), the best proxy for "is
// this worth robbing" available from a ball on a grid.
func (m *Mod) preyCargoM3(e *destiny.Entity) float64 {
	if e == nil || e.Slim.TypeID == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var cap float64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(capacity, 0) FROM invTypes WHERE typeID = ?`, e.Slim.TypeID).Scan(&cap)
	return cap
}

// ── the strike decision ─────────────────────────────────────────────────────

// mayStrike is the last gate before a pirate becomes a criminal: can this robbery
// be finished and left before the police arrive? Below 0.45 there are no police.
// It is arithmetic off both ships (the victim's effective hitpoints over this fit's
// damage, plus this hull's align time); a pirate that errs dies to CONCORD.
func (m *Mod) mayStrike(eb *embodiedBot, l *combatLoadout, victim *destiny.Entity) (bool, string) {
	sec := 0.0
	if eb.ss != nil {
		sec = eb.ss.Security
	}
	if _, policed := concordDelayFor(sec); !policed {
		return true, ""
	}

	dps := 0.0
	for _, w := range l.weapons {
		if w.CycleSec > 0 && w.Dmg != nil {
			dps += w.cycleDamage() / w.CycleSec
		}
	}
	// The drone flight shoots the victim too (drones_embodied.go), and the
	// CONCORD window is measured against everything that will be hitting it.
	dps += l.droneDPS()
	if dps <= 0 {
		return false, "nothing to shoot with"
	}
	hp := victim.HPSnapshot()
	ehp := hp.CurShield + hp.CurArmor + hp.CurStructure
	ttk := ehp / dps

	align := eb.alignSec()
	ok, budget := piracyStrikeWindow(sec, ttk, align, eb.tune.ConcordNerveSec)
	if ok {
		return true, ""
	}
	return false, fmt.Sprintf(
		"%.0fs to kill it plus %.0fs to align, and CONCORD is %.0fs away in %.1f space",
		ttk, align, budget, sec)
}

// alignSec is how long this hull takes to enter warp, the second half of every
// escape, derived from the ball's mass and agility as the client's align time is.
func (eb *embodiedBot) alignSec() float64 {
	if eb.ent == nil || eb.ent.Manager == nil {
		return 10
	}
	mass, agility := eb.ent.Manager.Mass(), eb.ent.Manager.Agility()
	if mass <= 0 || agility <= 0 {
		return 10
	}
	// The classic formula: -ln(0.25) x inertia x mass / 1e6.
	return math.Ln2 * 2 * agility * mass / 1_000_000
}

// piracyTarget is the pirate's own target choice, or 0 to leave the ordinary
// combat scorer in charge. It ranks by value (pickPrey) and it refuses: a victim
// the CONCORD clock says cannot be killed and left in time is not attacked,
// which is what separates a pirate from a suicide gank.
func (m *Mod) piracyTarget(eb *embodiedBot, l *combatLoadout, hostiles []uint32) uint32 {
	if !l.armed() {
		// Nothing to rob with (guns or a drone flight, which engages a pilot like
		// anything else, drones_embodied.go).
		return 0
	}
	// An aggressor outranks a victim: if something on this grid is actually
	// shooting the pirate, that is the fight it is in.
	for _, id := range eb.underAttack() {
		for _, h := range hostiles {
			if h == id {
				return 0
			}
		}
	}
	// Already holding somebody: stay on them.
	eb.mu.Lock()
	rs := eb.ransom
	eb.mu.Unlock()
	if rs != nil && rs.victimCharID != 0 {
		for _, id := range hostiles {
			if e := eb.ss.GetEntity(id); e != nil && e.Slim.CharID == rs.victimCharID {
				return id
			}
		}
		m.clearRansom(eb) // the mark is gone
	}

	var prey []uint32
	for _, id := range hostiles {
		if e := eb.ss.GetEntity(id); e != nil && m.wouldRob(eb, e) {
			prey = append(prey, id)
		}
	}
	target := m.pickPrey(eb, l, prey)
	if target == 0 {
		return 0
	}
	victim := eb.ss.GetEntity(target)
	if victim == nil {
		return 0
	}
	if ok, why := m.mayStrike(eb, l, victim); !ok {
		// Logged at info once per victim, not every tick: an operator needs to tell "sat
		// next to a hauler and did nothing" apart from a broken bot.
		if eb.noteStrikeRefusal(victim.Slim.CharID) {
			m.logBot(eb.charID, logInfo, "piracy",
				"let %s go — %s", victim.CharName, why)
		}
		return 0
	}
	return target
}

// ── the pod ─────────────────────────────────────────────────────────────────

// markPod decides, once, whether this pilot's capsule dies too. Rolled when its
// ship dies and held for a short window: long enough to lock and kill a pod, not
// to become a hobby.
func (m *Mod) markPod(eb *embodiedBot, victimCharID int64) {
	if victimCharID == 0 || rand.Float64() >= eb.tune.PodKillChance {
		return
	}
	eb.mu.Lock()
	eb.podMark, eb.podMarkUntil = victimCharID, time.Now().Add(podHuntWindow)
	eb.mu.Unlock()
	m.logBot(eb.charID, logInfo, "piracy", "going for the pod as well")
}

// podMarked reports whether this capsule is the one just condemned.
func (eb *embodiedBot) podMarked(charID int64) bool {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	return eb.podMark != 0 && eb.podMark == charID && time.Now().Before(eb.podMarkUntil)
}

// podHuntWindow is how long a condemned capsule stays condemned. A pod that
// gets clear in that time has got clear.
const podHuntWindow = 45 * time.Second

// noteStrikeRefusal reports whether this refusal is worth a log line: the first
// time this projection has passed on this pilot.
func (eb *embodiedBot) noteStrikeRefusal(charID int64) bool {
	if charID == 0 {
		return false
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	if eb.passedOn == nil {
		eb.passedOn = map[int64]bool{}
	}
	if eb.passedOn[charID] {
		return false
	}
	eb.passedOn[charID] = true
	return true
}

// ── the robbery ─────────────────────────────────────────────────────────────

// piracyEngage runs one tick of a robbery against a locked, in-range victim.
// Reports whether the caller (the ordinary combat loop) should hold its fire
// this tick: while terms are on the table the guns stay cold, which is the
// entire difference between a ransom and a murder with a countdown.
func (m *Mod) piracyEngage(eb *embodiedBot, l *combatLoadout, victim *destiny.Entity) bool {
	if victim == nil || !eb.prey {
		return false
	}
	victimCharID := victim.Slim.CharID

	eb.mu.Lock()
	rs := eb.ransom
	eb.mu.Unlock()

	// A robbery already in progress against somebody else on the grid: leave
	// it be, the loop will come back to this target.
	if rs != nil && rs.victimCharID != victimCharID {
		return false
	}

	// Nothing started yet. The point must be on before terms mean anything, so a
	// pirate with no scramble applied simply shoots: a demand to a free ship is empty.
	if rs == nil {
		if !m.tackleHolds(eb, l, victim) {
			return false
		}
		if rand.Float64() >= eb.tune.RansomChance {
			// No terms. Say something anyway: a player deserves to know a person did this,
			// and it costs one line of chat.
			m.pirateSay(eb, evExecute, victimCharID, m.preyLink(victim), 0)
			eb.mu.Lock()
			eb.ransom = &ransomState{victimCharID: victimCharID, settled: true}
			eb.mu.Unlock()
			return false
		}
		m.openRansom(eb, victim)
		return true
	}

	// Paid. Hold fire for good: the amnesty keeps the mark out of the prey list and
	// this is the other half of the same promise while the projection remembers.
	if rs.released {
		return true
	}
	if rs.settled {
		return false // terms declined: the ordinary loop owns it now
	}

	// Terms are out. Poll the wallet (or, for a cargo demand, the grid for a new
	// container, camp.go), then the clock.
	if rs.cargoDemand {
		if m.cargoDropped(eb, rs, victim) {
			m.closeRansomPaid(eb, rs)
			return true
		}
	} else if time.Since(rs.demandedAt) >= time.Duration(ransomPollSec)*time.Second {
		if m.ransomPaid(eb, rs) {
			m.closeRansomPaid(eb, rs)
			return true
		}
	}
	window := time.Duration(eb.tune.dwell(ransomWaitSec)) * time.Second
	if time.Since(rs.demandedAt) < window {
		return true // still waiting: hold the point, hold the fire
	}
	m.pirateSay(eb, evRansomLate, rs.victimCharID, rs.victimName, 0)
	m.logBot(eb.charID, logInfo, "piracy",
		"%s did not pay %s — opening fire", rs.victimName, iskWords(rs.demanded))
	m.releaseFleetHold(eb, rs)
	eb.mu.Lock()
	rs.settled = true
	eb.mu.Unlock()
	return false
}

// tackleHolds reports whether this pirate currently has the victim held with a
// live warp scramble it applied itself, read off the target's ewar state because
// that decides whether the victim can leave.
func (m *Mod) tackleHolds(eb *embodiedBot, l *combatLoadout, victim *destiny.Entity) bool {
	if victim == nil {
		return false
	}
	if victim.WarpScrambleTotal() > 0 {
		return true
	}
	// No scrambler fitted at all: a web is not a hold, but a pirate that carries
	// neither must still be allowed to try (tackleFactorNone).
	for _, ew := range l.ewar {
		if ew.Kind == ewScram {
			return false // it has one; it just has not landed it yet
		}
	}
	return true
}

// openRansom puts the demand in Local and starts the clock. The price scales with
// what the victim is flying so the demand looks proportionate: envy sets the
// multiplier (Tuning.RansomMul), the hull sets the scale.
func (m *Mod) openRansom(eb *embodiedBot, victim *destiny.Entity) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	name := victim.CharName
	if name == "" {
		name = evedb.GetCharacterName(ctx, m.db, victim.Slim.CharID)
	}
	price := m.ransomPrice(eb, victim)
	// A scan prices the demand when the pack can make one (camp.go ransomScan). A hold
	// worth far more than any typed price is asked for outright: jettison it, and go.
	cargoISK, hullISK, scanned := m.ransomScan(ctx, eb, victim)
	cargoDemand := false
	if scanned {
		price = ransomPriceFromScan(cargoISK, hullISK, eb.tune.RansomMul)
		cargoDemand = cargoISK >= price*ransomCargoDemandRatio
	}
	bal, _ := evedb.GetCharBalance(ctx, m.db, eb.charID)

	rs := &ransomState{
		victimCharID: victim.Slim.CharID,
		victimName:   m.preyLink(victim),
		demanded:     price,
		demandedAt:   time.Now(),
		baseline:     bal,
		cargoISK:     cargoISK,
		hullISK:      hullISK,
		victimShip:   victim.Slim.ItemID,
	}
	if cargoDemand {
		rs.cargoDemand = true
		rs.demanded = cargoISK
		rs.cansBefore = m.containerSet(ctx, eb.systemID, victim.Pos(), ransomCargoDropRadiusM)
	}
	eb.mu.Lock()
	eb.ransom = rs
	eb.mu.Unlock()
	// The whole pack holds while a price stands: a line that kept shooting
	// would be asking for money it had stopped being able to take.
	m.holdFleetForTerms(eb, rs)

	if cargoDemand {
		m.pirateSay(eb, evCargoDemand, rs.victimCharID, rs.victimName, cargoISK)
		m.logBot(eb.charID, logInfo, "piracy",
			"holding %s and demanding the hold — the scan reads %s against a %s hull", name, iskWords(cargoISK), iskWords(hullISK))
	} else {
		m.pirateSay(eb, evRansom, rs.victimCharID, rs.victimName, price)
		if scanned {
			m.logBot(eb.charID, logInfo, "piracy",
				"holding %s and asking %s — priced off the scan (%s in the hold, %s hull)", name, iskWords(price), iskWords(cargoISK), iskWords(hullISK))
		} else {
			m.logBot(eb.charID, logInfo, "piracy",
				"holding %s and asking %s", name, iskWords(price))
		}
	}
	m.chatterNoteBot(ctx, eb.charID, "pers.ransom", map[string]string{
		"isk": iskWords(rs.demanded), "victim": rs.victimName}, rs.victimCharID)
}

// ransomPrice is the demand: the pilot's base multiplier against the size of
// what it caught, rounded to something a person would actually type.
func (m *Mod) ransomPrice(eb *embodiedBot, victim *destiny.Entity) float64 {
	price := ransomBaseISK * eb.tune.RansomMul
	if cargo := m.preyCargoM3(victim); cargo > 0 {
		// A hauler is worth more than a frigate, sub-linearly: the demand
		// grows with the hull but never into a number nobody pays.
		price *= 1 + math.Sqrt(cargo/100)
	}
	// Round to a clean 10k so the line reads like a person said it.
	return math.Round(price/10_000) * 10_000
}

// preyLink is the victim's name as a clickable chat link.
func (m *Mod) preyLink(victim *destiny.Entity) string {
	if victim == nil {
		return "you"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	name := victim.CharName
	if name == "" {
		name = evedb.GetCharacterName(ctx, m.db, victim.Slim.CharID)
	}
	return charLink(victim.Slim.CharID, m.charTypeID(ctx, victim.Slim.CharID), name)
}

// ransomPaid asks whether the money has arrived. It reads the wallet journal, not
// the balance (which moves for other reasons): refType 10 is what account.GiveCash
// writes, ownerID1 is the sender, and the window starts at the demand so an old
// donation cannot buy a second robbery.
func (m *Mod) ransomPaid(eb *embodiedBot, rs *ransomState) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var paid float64
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM market_journal
		WHERE characterID = ? AND refTypeID = 10 AND ownerID1 = ?
		  AND amount > 0 AND transDate >= ?`,
		eb.charID, rs.victimCharID, win32From(rs.demandedAt)).Scan(&paid)
	if err != nil {
		rlogf("rolebots: ransom poll char=%d: %v", eb.charID, err)
		return false
	}
	// Short payment is not payment, but an overpayer gets the same answer as an exact
	// one: nobody is haggled upwards.
	return paid >= rs.demanded*0.95
}

// win32From converts a Go time to the FILETIME every client-facing timestamp
// column stores (the wallet journal, evemail.created and its OnMessage push).
// Unix seconds there reach util.FmtDate, which raises "argument 1 must be long"
// and the mail list fails to draw. RepairMailTimestamps only sweeps at boot.
func win32From(t time.Time) int64 {
	return t.UnixNano()/100 + 116444736000000000
}

// closeRansomPaid lets a paying victim go: point off, guns cold, amnesty, and a
// line in Local that others can read. The last part is the business model: a
// pirate known for releasing paid marks gets paid more often.
func (m *Mod) closeRansomPaid(eb *embodiedBot, rs *ransomState) {
	eb.mu.Lock()
	rs.settled, rs.released = true, true
	eb.mu.Unlock()
	m.releaseFleetHold(eb, rs)

	m.pirateSay(eb, evRansomPaid, rs.victimCharID, rs.victimName, 0)
	if rs.cargoDemand {
		m.logBot(eb.charID, logInfo, "piracy",
			"took the hold, %s worth — releasing the mark", iskWords(rs.demanded))
	} else {
		m.logBot(eb.charID, logInfo, "piracy",
			"paid %s — releasing the mark", iskWords(rs.demanded))
	}
	m.chatterNoteBot(context.Background(), eb.charID, "pers.ransom.paid", map[string]string{
		"isk": iskWords(rs.demanded), "victim": rs.victimName}, rs.victimCharID)
	grantAmnesty(rs.victimCharID)
	m.noteGraceEventAsync(rs.victimCharID, eb.charID, gkRansomOK, fightFacts{ISK: rs.demanded}, eb.systemID, 0)

	// Drop everything holding them and stand the fight down; releaseVictim uses the
	// disengage path so the client sees the effects end.
	m.releaseVictim(eb)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := bumpPiracyTally(ctx, m.db, eb.charID, false); err != nil {
		rlogf("rolebots: ransom tally char=%d: %v", eb.charID, err)
	}
	m.piracyRecordVictim(ctx, eb.charID, rs.victimCharID, false)
	m.chargeEmbodiedRobbery(eb, rs)
	victimCorpID, _ := m.affiliationOf(rs.victimCharID)
	m.noteRobberyIncident(ctx, rs.victimCharID, victimCorpID, rs.victimName, []int64{eb.corpID}, rs.demanded, eb.systemID)
}

// chargeEmbodiedRobbery is chargeAbstractRobbery for a hold-up on a watched grid:
// one law in both tiers (as chargeKillerForBotDeath, pod.go), so a pirate pays
// the same whether or not a client had the system loaded. Only the record is
// charged here; the flag and police response came with the shots (PoliceCrime).
func (m *Mod) chargeEmbodiedRobbery(eb *embodiedBot, rs *ransomState) {
	hit := modapi.PvP().SecRobberyPenalty
	if hit == nil || eb == nil || rs == nil || rs.victimCharID == 0 {
		return
	}
	var killerAllianceID, shipTypeID int64
	if eb.ent != nil {
		killerAllianceID = eb.ent.Slim.AllianceID
		shipTypeID = eb.ent.Slim.TypeID
	}
	security := 0.0
	if eb.ss != nil {
		security = eb.ss.Security
	}
	victimCorpID, victimAllianceID := m.affiliationOf(rs.victimCharID)

	// The mark's own record. Robbing an outlaw is free: they are a legal target for
	// everybody, decided in the exemption ladder.
	var victimSec float64
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(securityRating, 0) FROM character_ WHERE characterID = ?`,
			rs.victimCharID).Scan(&victimSec)
	}()

	applied, why := hit(eb.ent, seclaw.KillFacts{
		KillerCharID:     eb.charID,
		KillerCorpID:     eb.corpID,
		KillerAllianceID: killerAllianceID,
		KillerShipTypeID: shipTypeID,
		VictimCharID:     rs.victimCharID,
		VictimCorpID:     victimCorpID,
		VictimAllianceID: victimAllianceID,
		VictimOutlaw:     victimSec <= seclaw.OutlawStatus,
		SystemID:         eb.systemID,
		Security:         security,
	})
	if applied != 0 {
		m.logBot(eb.charID, logInfo, "security",
			"record docked %+.2f for robbing %s", applied, rs.victimName)
	} else if why != "" {
		m.logBot(eb.charID, logInfo, "security",
			"robbing %s was lawful (%s)", rs.victimName, why)
	}
}

// releaseVictim ends the hold on a paid mark without ending the projection's
// combat readiness: the pirate is still on its gate, dangerous to the next pilot.
func (m *Mod) releaseVictim(eb *embodiedBot) {
	eb.mu.Lock()
	eb.combatTarget = 0
	eb.mu.Unlock()
	if l := eb.loadout(); l != nil {
		m.disengageAll(eb, l)
	}
}

// clearRansom forgets any robbery in progress. Called when the target dies or
// leaves, so the next victim starts from a clean sheet.
func (m *Mod) clearRansom(eb *embodiedBot) {
	eb.mu.Lock()
	eb.ransom = nil
	eb.mu.Unlock()
}

// loadout is the projection's resolved combat loadout, published by the combat
// loop on first contact. Nil until then: a pirate that has not engaged anything
// has not paid for the attribute queries.
func (eb *embodiedBot) loadout() *combatLoadout {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	return eb.load
}

// ── the kill ────────────────────────────────────────────────────────────────

// piracyKilledPrey is everything that follows a pirate destroying a player's
// ship: the death (which the core owns), the pod decision, the tally and the
// victim's log. Called from landDamage on the destroyed branch; it is the only
// path by which a rolebot's guns end a player's ship.
func (m *Mod) piracyKilledPrey(eb *embodiedBot, victim *destiny.Entity) {
	if victim == nil || !victim.IsPlayer {
		return
	}
	pod := victim.Slim.TypeID == capsuleTypeID
	victimCharID := victim.Slim.CharID

	if kill := modapi.PvP().KillPlayer; kill != nil && kill(victim, pod) {
		m.logBot(eb.charID, logInfo, "piracy",
			"destroyed %s's ship", victim.CharName)
	} else if bot := lookupProjectionByShip(victim.ID); bot != nil {
		// One of ours. Its own loss path already runs from OnEntityRemoved;
		// nothing to do here but take the credit.
		m.logBot(eb.charID, logInfo, "piracy",
			"destroyed rolebot %s's ship", bot.charName)
	}

	// No security charge to the killer here: it is billed from the victim's side
	// (core death handler, or chargeKillerForBotDeath), so a charge here would double it.
	m.noteGraceWin(eb, victimCharID, pod)
	m.clearRansom(eb)
	if !pod {
		// The capsule is now sitting where the ship was. Cruelty decides.
		m.markPod(eb, victimCharID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := bumpPiracyTally(ctx, m.db, eb.charID, true); err != nil {
		rlogf("rolebots: kill tally char=%d: %v", eb.charID, err)
	}
	m.piracyRecordVictim(ctx, eb.charID, victimCharID, true)

	// The wreck is the wages. Point the sim at it so the pilot goes and
	// empties it instead of wandering to the next belt (piracy_loot.go).
	m.notePiracyLoot(ctx, eb, victim)
}

// bumpPiracyTally records a kill or a paid ransom on the pilot's own state.
// Straight SQL on the JSON blob's owning row rather than through the sim,
// because this runs on a projection goroutine and the sim owns the struct.
func bumpPiracyTally(ctx context.Context, db *sql.DB, charID int64, killed bool) error {
	st, err := loadState(ctx, db, charID)
	if err != nil {
		return err
	}
	if killed {
		st.Data.PiracyKills++
	} else {
		st.Data.PiracyRansomed++
	}
	return saveState(ctx, db, st)
}

func init() {
	registerCharForget("amnesty", func(id int64) {
		amnestyMu.Lock()
		delete(amnesty, id)
		amnestyMu.Unlock()
	})
	registerSweep("amnesty", sweepAmnesty)
}