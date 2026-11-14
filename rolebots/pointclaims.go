package rolebots

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/server/gangsta"
	"gurimoon/server/lsc"
)

// Spreading points across a grid (docs/FLEET_DOCTRINE.md, "Spreading points"). A fleet's
// pointers split across the hostiles instead of all scrambling the primary.
// Each fleet keeps one claim per hostile ball. A ball another pointer has claimed, or that
// already carries a scramble from anyone else, is "held". choosePointTarget is the rule;
// pointTargetFor builds its input from the grid and makes the claim.
// The tackler seat flies to its pick (tackleBurn follows the gun target). Any other ship
// with a point only spreads it to a hostile already inside scram range (spreadPoint).

// pointClaimTTL is how long a claim stands without a refresh. A pointer
// refreshes its claim on every pick, so only a pointer that stopped picking
// (dead, docked, out of the fight) loses it this way.
const pointClaimTTL = 15 * time.Second

// The point modes a combat tick sets on the loadout (combatLoadout.pointMode).
const (
	pointFollow = iota // the scram goes on the gun target
	pointSpread        // the scram may move to an unheld hostile in range
)

type pointClaim struct {
	holder int64
	at     time.Time
}

// pointClaims holds, per fleetKey, the claim on each hostile ball.
var pointClaims = struct {
	mu sync.Mutex
	by map[int64]map[uint32]pointClaim
}{by: map[int64]map[uint32]pointClaim{}}

func init() {
	registerCharForget("pointClaims", releasePointsOf)
	registerSweep("pointClaims", sweepPointClaims)
}

// fleetKey is the claim space a pilot shares with its fleetmates: the module
// fleet's ID, else minus the real fleet's ID for a pilot serving in a
// player's fleet (conscript.go), else 0 (a lone pilot, which spreads nothing).
func fleetKey(eb *embodiedBot) int64 {
	if eb == nil {
		return 0
	}
	eb.mu.Lock()
	gangID := eb.gangID
	eb.mu.Unlock()
	if gangID != 0 {
		return gangID
	}
	if k := conscriptOf(eb.charID); k != nil && k.GangID != 0 {
		return -k.GangID
	}
	return 0
}

// claimPoint takes ball for holder in fleet key, dropping holder's other
// claims there. False when a fresh claim by someone else stands.
func claimPoint(key int64, ball uint32, holder int64, now time.Time) bool {
	if key == 0 || ball == 0 || holder == 0 {
		return false
	}
	pointClaims.mu.Lock()
	defer pointClaims.mu.Unlock()
	fleet := pointClaims.by[key]
	if fleet == nil {
		fleet = map[uint32]pointClaim{}
		pointClaims.by[key] = fleet
	}
	if c, ok := fleet[ball]; ok && c.holder != holder && now.Sub(c.at) < pointClaimTTL {
		return false
	}
	for id, c := range fleet {
		if c.holder == holder && id != ball {
			delete(fleet, id)
		}
	}
	fleet[ball] = pointClaim{holder: holder, at: now}
	return true
}

// releasePoint drops holder's claim on ball, if it is holder's.
func releasePoint(key int64, ball uint32, holder int64) {
	if key == 0 {
		return
	}
	pointClaims.mu.Lock()
	defer pointClaims.mu.Unlock()
	fleet := pointClaims.by[key]
	if c, ok := fleet[ball]; ok && c.holder == holder {
		delete(fleet, ball)
	}
	if len(fleet) == 0 {
		delete(pointClaims.by, key)
	}
}

// releasePointsOf drops every claim holder has, in every fleet.
func releasePointsOf(holder int64) {
	pointClaims.mu.Lock()
	defer pointClaims.mu.Unlock()
	for key, fleet := range pointClaims.by {
		for id, c := range fleet {
			if c.holder == holder {
				delete(fleet, id)
			}
		}
		if len(fleet) == 0 {
			delete(pointClaims.by, key)
		}
	}
}

// claimedPoints is the fleet's fresh claims as a map from ball to holder. A snapshot.
func claimedPoints(key int64, now time.Time) map[uint32]int64 {
	pointClaims.mu.Lock()
	defer pointClaims.mu.Unlock()
	out := map[uint32]int64{}
	for id, c := range pointClaims.by[key] {
		if now.Sub(c.at) < pointClaimTTL {
			out[id] = c.holder
		}
	}
	return out
}

// myClaim is the ball holder has a fresh claim on in fleet key, or 0.
func myClaim(key int64, holder int64, now time.Time) uint32 {
	pointClaims.mu.Lock()
	defer pointClaims.mu.Unlock()
	for id, c := range pointClaims.by[key] {
		if c.holder == holder && now.Sub(c.at) < pointClaimTTL {
			return id
		}
	}
	return 0
}

func sweepPointClaims() {
	now := clock.Now()
	pointClaims.mu.Lock()
	for key, fleet := range pointClaims.by {
		for id, c := range fleet {
			if now.Sub(c.at) >= pointClaimTTL {
				delete(fleet, id)
			}
		}
		if len(fleet) == 0 {
			delete(pointClaims.by, key)
		}
	}
	pointClaims.mu.Unlock()
	sweepPointCalls(now)
}

// ── the rule ────────────────────────────────────────────────────────────────

// pointCand is one hostile as the rule sees it.
type pointCand struct {
	id    uint32
	dist  float64
	pilot bool // a player or a bot: what a point is most worth holding
	held  bool // another pointer already holds it
}

// pointPick is the rule's input. reach 0 means anywhere on the grid.
type pointPick struct {
	primary, secondary, current uint32
	reach                       float64
	cands                       []pointCand
}

// choosePointTarget answers which hostile one pointer should hold, in order: the primary
// if nobody holds it; its current claim, so points do not flip-flop; the secondary if nobody
// holds it; the nearest unheld pilot, then rat; else the primary anyway (a second point
// beats a warp core stabilizer). It returns 0 when the primary is the answer but not a candidate.
func choosePointTarget(p pointPick) uint32 {
	in := map[uint32]pointCand{}
	for _, c := range p.cands {
		if p.reach > 0 && c.dist > p.reach {
			continue
		}
		in[c.id] = c
	}
	free := func(id uint32) bool {
		c, ok := in[id]
		return id != 0 && ok && !c.held
	}
	if free(p.primary) {
		return p.primary
	}
	// A ball held by someone else as well is still this pointer's claim, so keep it: the other
	// is an unclaimed point (a human, a gun ship) and that one moves.
	if _, ok := in[p.current]; ok && p.current != 0 {
		return p.current
	}
	if free(p.secondary) {
		return p.secondary
	}
	var best uint32
	bestPilot, bestDist := false, math.MaxFloat64
	for _, c := range p.cands {
		if _, ok := in[c.id]; !ok || c.held {
			continue
		}
		if best == 0 || (c.pilot && !bestPilot) || (c.pilot == bestPilot && c.dist < bestDist) {
			best, bestPilot, bestDist = c.id, c.pilot, c.dist
		}
	}
	if best != 0 {
		return best
	}
	if _, ok := in[p.primary]; ok {
		return p.primary
	}
	return 0
}

// ── reading the grid ────────────────────────────────────────────────────────

// pointModeFor is the tick's point mode: follow the gun target for a held
// mark, a pilot the commander named, or a lone pilot; spread otherwise.
func pointModeFor(eb *embodiedBot, markFixed bool) int {
	if markFixed || fleetKey(eb) == 0 {
		return pointFollow
	}
	eb.mu.Lock()
	named := eb.bossID != 0 && eb.orderTargetChar != 0
	eb.mu.Unlock()
	if named {
		return pointFollow
	}
	return pointSpread
}

// tackleSeat reports a pilot whose job is the point: the fleet's tackler, a
// conscript given the tackler role, or a conscript told to tackle without a
// name. It must carry a point.
func tackleSeat(eb *embodiedBot, l *combatLoadout) bool {
	if eb == nil || !loadoutHasPoint(l) {
		return false
	}
	eb.mu.Lock()
	role, boss, verb, named := eb.gangRole, eb.bossID, eb.orderVerb, eb.orderTargetChar
	eb.mu.Unlock()
	if role == RoleTackler {
		return true
	}
	if boss == 0 {
		return false
	}
	if verb == OrderTackle && named == 0 {
		return true
	}
	k := conscriptOf(eb.charID)
	return k != nil && k.Role == RoleTackler
}

func loadoutHasPoint(l *combatLoadout) bool {
	if l == nil {
		return false
	}
	for _, ew := range l.ewar {
		if ew.Kind == ewScram {
			return true
		}
	}
	return false
}

// scramStrength is what one scram module puts on a target.
func scramStrength(ew *botEwarModule) float64 {
	if s := ew.Attrs[dogma.AttrWarpScrambleStrength]; s > 0 {
		return s
	}
	return 1
}

// fleetSecondary is the fleet commander's secondary call, or 0.
func fleetSecondary(eb *embodiedBot) uint32 {
	eb.mu.Lock()
	leaderID, own := eb.gangLeader, eb.call.secondary
	commander := eb.fleetCmd != nil
	eb.mu.Unlock()
	if commander {
		return own
	}
	if leaderID == 0 || leaderID == eb.charID {
		return 0
	}
	embodyMu.Lock()
	lead := embodyLive[leaderID]
	embodyMu.Unlock()
	if lead == nil || lead.systemID != eb.systemID {
		return 0
	}
	lead.mu.Lock()
	defer lead.mu.Unlock()
	return lead.call.secondary
}

// pointTargetFor picks and claims the hostile this pilot should point, with
// primary as the fleet's primary and reach as in pointPick. It answers
// primary unchanged for a lone pilot. Map reads only: the combat goroutine
// calls it every tick.
func (m *Mod) pointTargetFor(eb *embodiedBot, l *combatLoadout, primary uint32, reach float64, hostiles []uint32) uint32 {
	key := fleetKey(eb)
	if key == 0 || eb.ent == nil || eb.ss == nil {
		return primary
	}
	now := clock.Now()
	// What each of this pilot's points puts on each ball, so its own scramble
	// does not read as someone else holding the ball.
	mine := map[uint32]float64{}
	for _, ew := range l.ewar {
		if ew.Kind == ewScram && ew.appliedTo != 0 {
			mine[ew.appliedTo] += scramStrength(ew)
		}
	}
	claims := claimedPoints(key, now)
	eb.mu.Lock()
	conscript := eb.bossID != 0
	eb.mu.Unlock()
	selfPos := eb.ent.Pos()
	var cands []pointCand
	for _, id := range hostiles {
		e := eb.ss.GetEntity(id)
		if e == nil || e.IsDestroyed() || e.IsInWarp() || e.IsCloaked() {
			continue
		}
		if conscript && !m.conscriptMayEngage(eb, e) {
			continue
		}
		holder, claimed := claims[id]
		claimedByOther := claimed && holder != eb.charID
		// A ship already in scram range of the primary points it now rather
		// than wait for a tackler whose claim has not landed yet.
		if reach > 0 && id == primary {
			claimedByOther = false
		}
		held := claimedByOther || e.WarpScrambleTotal()-mine[id] > 0
		cands = append(cands, pointCand{
			id: id, dist: e.Pos().Sub(selfPos).Len(),
			pilot: e.IsPlayer || e.Slim.CharID != 0, held: held,
		})
	}
	pick := pointPick{
		primary: primary, secondary: fleetSecondary(eb),
		current: myClaim(key, eb.charID, now), reach: reach, cands: cands,
	}
	// A claim another pointer made since the snapshot loses the race: mark that ball held and
	// choose again. When everything is held the rule answers the primary.
	for range 3 {
		t := choosePointTarget(pick)
		if t == 0 {
			return primary
		}
		if t == primary && reach > 0 {
			// A gun ship's point on its own gun target needs no claim: once
			// it lands, the scramble on the ball is what others read.
			if pick.current != 0 && pick.current != t {
				releasePoint(key, pick.current, eb.charID)
			}
			return t
		}
		if claimPoint(key, t, eb.charID, now) {
			return t
		}
		for i := range pick.cands {
			if pick.cands[i].id == t {
				pick.cands[i].held = true
			}
		}
	}
	return primary
}

// spreadPoint moves one gun ship's point off the gun target when another
// pointer already holds it and an unheld hostile sits inside scram range.
func (m *Mod) spreadPoint(eb *embodiedBot, l *combatLoadout, ew *botEwarModule, gunTarget *destiny.Entity, hostiles []uint32) *destiny.Entity {
	if gunTarget == nil || eb.ss == nil {
		return gunTarget
	}
	id := m.pointTargetFor(eb, l, gunTarget.ID, ew.MaxRange*combatEwarRangeGrace, hostiles)
	if id == 0 || id == gunTarget.ID {
		return gunTarget
	}
	if e := eb.ss.GetEntity(id); e != nil {
		return e
	}
	return gunTarget
}

// ── calling it in fleet chat ────────────────────────────────────────────────

// settingFleetPointCalls switches the "Point on X" calls (default on).
const settingFleetPointCalls = "rolebots.fleet.pointCalls"

// pointCallGap is how long one fleet waits before calling the same ball again.
const pointCallGap = 90 * time.Second

var pointCallsOn atomic.Bool

func init() { pointCallsOn.Store(true) }

// PointCalls reports the switch.
func PointCalls() bool { return pointCallsOn.Load() }

// pointCallAt maps (real fleet, ball) to the time of the last call about it.
var pointCallAt = struct {
	mu sync.Mutex
	at map[[2]int64]time.Time
}{at: map[[2]int64]time.Time{}}

// pointCallDue stamps and reports whether the fleet may call ball now. The
// landed and lost calls share the stamp, so a point that flickers at the edge
// of range is called once.
func pointCallDue(fleet int64, ball uint32, now time.Time) bool {
	k := [2]int64{fleet, int64(ball)}
	pointCallAt.mu.Lock()
	defer pointCallAt.mu.Unlock()
	if at, ok := pointCallAt.at[k]; ok && now.Sub(at) < pointCallGap {
		return false
	}
	pointCallAt.at[k] = now
	return true
}

func sweepPointCalls(now time.Time) {
	pointCallAt.mu.Lock()
	defer pointCallAt.mu.Unlock()
	for k, at := range pointCallAt.at {
		if now.Sub(at) >= pointCallGap {
			delete(pointCallAt.at, k)
		}
	}
}

// notePointLanded calls a new point in fleet chat (applyEwar).
func (m *Mod) notePointLanded(eb *embodiedBot, target *destiny.Entity) {
	m.pointCall(eb, target, evPointLanded, true)
}

// notePointLost calls a point that dropped off a live target (runEwarCycle).
func (m *Mod) notePointLost(eb *embodiedBot, target *destiny.Entity) {
	if target == nil || target.IsDestroyed() {
		return
	}
	m.pointCall(eb, target, evPointLost, false)
}

// pointCall says one point line in the pilot's real fleet, only while a human
// is online in it. The line goes out on a goroutine: speakFleet queries.
func (m *Mod) pointCall(eb *embodiedBot, target *destiny.Entity, ev chatEvent, urgent bool) {
	if !PointCalls() || eb == nil || target == nil {
		return
	}
	fleet := gangsta.GangOf(eb.charID)
	if fleet == 0 || len(gangsta.HumanMembersOnline(fleet)) == 0 {
		return
	}
	if !pointCallDue(fleet, target.ID, clock.Now()) {
		return
	}
	slots := chatSlots{"self": eb.charName}
	if target.IsPlayer && target.Slim.CharID != 0 && target.CharName != "" {
		about := partyFromID(target.Slim.CharID, target.CharName, 0, 0, false)
		slots["target"], slots["name"] = about.link(), about.Name
	} else {
		name := ballLabel(target)
		slots["target"], slots["name"] = name, name
	}
	voice := partyFromEmbodied(eb).Voice
	charID := eb.charID
	m.goSafeBot(charID, "point call", func() {
		line, _ := pickChatLine(lsc.GangChannelID(fleet), ev, motiveFleetCall, voice, slots)
		if line == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if m.speakFleet(ctx, fleet, charID, urgent, line) {
			m.logBot(charID, logInfo, "fleet", "said: %s", line)
		}
	})
}
