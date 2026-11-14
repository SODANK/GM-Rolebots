package rolebots

import (
	"context"
	"time"

	"gurimoon/destiny"
)

// Fleet command on the grid: the embodied half of fleetcommand.go. The commander calls and
// members read the call off its projection (embodyLive, under its mu, the lock order
// gangprimary.go uses), using the pure rules of the abstract fight (fcPickPrimary,
// fcShouldRetreat).
//
//   - Primary and secondary: the FC names both so members pre-lock the secondary.
//   - Retreat: every member breaks off to the same celestial (fleetRetreatCalled).
//   - Anchor: gun and logi seats orbit it; the anchor flies the stance (holdCombatRange).
//
// A Director on a mirrored fleet (fleetmirror_orders.go) can override these calls.

const (
	// fleetAnchorOrbitM is how far a line member circles its anchor. It is inside the muster
	// formation radius (formUpOrbitM, 5 km): a fighting line is tighter than a gathering one.
	fleetAnchorOrbitM = 2_500.0
	// fleetHumanCallSec is how long a Director's named primary or anchor
	// stands before the FC's own judgement resumes.
	fleetHumanCallSec = 10 * 60
)

// fleetCall is the commander's live calls, read by every member under the
// commander's mu.
type fleetCall struct {
	primary     uint32
	secondary   uint32
	secondaryAt time.Time
	anchor      int64 // charID; 0 = the commander
	retreat     bool
	retreatWhy  string
	retreatDest int64 // celestial itemID every member runs to; 0 = each its own
	hold        bool  // a Director's "hold position": the line anchors and fires on call only
	// A Director's named primary and anchor, standing until humanUntil.
	humanPrimary uint32
	humanAnchor  int64
	humanUntil   time.Time
	// The fight's start, for the retreat rule; engaged marks a live fight.
	start   fightStart
	engaged bool
	// split is the half of the line on the secondary under a split call (setFCSplit), nil when
	// the line is on one primary; splitSaid keeps the call said once.
	split     map[int64]bool
	splitSaid bool
	// What has been narrated, so a call is said once.
	saidPrimary uint32
	saidAnchor  bool
}

// primaryFor is the called target for one member: the secondary for the half of the line a
// split put on it (setFCSplit), else the primary. Read under the commander's mu.
func (c *fleetCall) primaryFor(charID int64) uint32 {
	if c.split[charID] && c.secondary != 0 {
		return c.secondary
	}
	return c.primary
}

// loadFleetSeat reads the projection's seat and, for the commander, the fleet's command.
// It runs at embodiment only; the grid tick never queries.
func (m *Mod) loadFleetSeat(ctx context.Context, eb *embodiedBot) {
	if eb == nil || eb.gangID == 0 {
		return
	}
	g, mem, err := gangForChar(ctx, m.db, eb.charID)
	if err != nil || g == nil {
		return
	}
	if mem != nil {
		eb.gangRole = mem.Role
	}
	if g.LeaderCharID == eb.charID {
		eb.fleetCmd = fleetCommandOf(g)
		if eb.fleetCmd != nil && eb.fleetCmd.Anchor != 0 && eb.fleetCmd.Anchor != eb.charID {
			eb.call.anchor = eb.fleetCmd.Anchor
		}
		if eb.fleetCmd != nil {
			m.fillFleetHold(ctx, g, eb.systemID, eb.fleetCmd)
		}
	}
}

// fillFleetHold sets the command's hold point for this system from the fleet's orders: a siege
// the fleet's side has open here (siegeHoldFor), else the gate a camp or checkpoint holds
// (campHoldGate). A projection is rebuilt on every jump, so the point follows the fleet from
// system to system.
func (m *Mod) fillFleetHold(ctx context.Context, g *Gang, sysID int64, cmd *FleetCommand) {
	if at, ball, rangeM, bash := m.siegeHoldFor(ctx, g.CorpID, sysID); at != 0 {
		cmd.HoldAt, cmd.HoldBall, cmd.HoldRangeM, cmd.HoldBash = at, ball, rangeM, bash
		return
	}
	if gate := campHoldGate(&g.State, sysID); gate != 0 {
		cmd.HoldAt, cmd.HoldBall, cmd.HoldRangeM, cmd.HoldBash = gate, 0, fleetCampHoldM, false
	}
}

// fleetCampHoldM is how far off the gate's surface a camp's line circles it, close enough that a
// jump-in lands among the line.
const fleetCampHoldM = 5_000.0

// campHoldGate is the gate a fleet's orders hold in sysID: a checkpoint's gate, or a pirate
// pack's camp gate. A gank fleet's gate is its scout's, and the strikers stage elsewhere, so it
// holds nothing. Pure.
func campHoldGate(gs *GangState, sysID int64) int64 {
	if gs == nil || sysID == 0 {
		return 0
	}
	if gs.CheckpointGateID != 0 && gs.CheckpointSystemID == sysID {
		return gs.CheckpointGateID
	}
	if gs.Focus != gangFocusGank && gs.PirGateID != 0 && gs.PirSystemID == sysID {
		return gs.PirGateID
	}
	return 0
}

// isFleetCommander reports a projection commanding a fleet under a command.
func (eb *embodiedBot) isFleetCommander() bool {
	return eb != nil && eb.fleetCmd != nil && eb.gangID != 0 && eb.gangLeader == eb.charID
}

// fleetLeaderLive is a member's commander on this grid: projected, in the
// same system, alive, carrying a command. Nil for the commander itself and
// for every fleet without one. Lock order is embodyMu then lead.mu, as
// gangPrimary takes them.
func fleetLeaderLive(eb *embodiedBot) *embodiedBot {
	if eb == nil || eb.gangID == 0 || eb.gangLeader == 0 || eb.gangLeader == eb.charID {
		return nil
	}
	embodyMu.Lock()
	lead := embodyLive[eb.gangLeader]
	embodyMu.Unlock()
	if lead == nil || lead.systemID != eb.systemID || lead.ent == nil || lead.fleetCmd == nil {
		return nil
	}
	lead.mu.Lock()
	gone := lead.dead
	lead.mu.Unlock()
	if gone {
		return nil
	}
	return lead
}

// anchorSeat reports a seat that fights from the anchor: the guns and the
// logi. Tackle goes to the target; electronic warfare holds its own range;
// the scout sits on the gate it is scanning, never on the anchor.
func anchorSeat(role string) bool {
	switch role {
	case RoleTackler, RoleEwar, RoleScout:
		return false
	}
	return true
}

// preLockReady reports whether a target called at calledAt has been "on the
// lock" long enough to shoot at now.
func preLockReady(calledAt, now time.Time, lockSec float64) bool {
	return !calledAt.IsZero() && now.Sub(calledAt).Seconds() >= lockSec
}

// fleetSay narrates a call to the fleet's chat, when the fleet is mirrored.
func (m *Mod) fleetSay(eb *embodiedBot, format string, args ...any) {
	if eb == nil || eb.gangID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.narrateFleet(ctx, eb.gangID, true, format, args...)
}

// ── the commander's calls ───────────────────────────────────────────────────

// fcGridCands is the hostiles as the FC's scorer sees them: those alive and inside the FC's
// own lock reach, with remaining hitpoints, remote repair and tackle (gridShipFactsOf), and
// theirRep, what every living hostile on grid can put back (fcCandsOf).
func (m *Mod) fcGridCands(eb *embodiedBot, l *combatLoadout, hostiles []uint32) ([]fcCand, map[int64]uint32, float64) {
	if eb.ent == nil || eb.ss == nil || l == nil {
		return nil, nil, 0
	}
	selfPos := eb.ent.Pos()
	reach := l.targetRnge * eb.ent.EwarTotals().MaxTargetRangeMult
	balls := make([]fcBall, 0, len(hostiles))
	for _, id := range hostiles {
		e := eb.ss.GetEntity(id)
		if e == nil || e.IsDestroyed() || e.IsInWarp() || e.IsCloaked() {
			continue
		}
		hp := e.HPSnapshot()
		balls = append(balls, fcBall{
			ID:      id,
			EHP:     hp.CurShield + hp.CurArmor + hp.CurStructure,
			InReach: e.Pos().Sub(selfPos).Len() <= reach,
			Facts:   m.gridShipFactsOf(e),
		})
	}
	return fcCandsOf(balls)
}

// fleetGridDPS is what the fleet's projections on this grid land a second.
func (m *Mod) fleetGridDPS(eb *embodiedBot) float64 {
	total := 0.0
	for _, mate := range m.fleetMatesOnGrid(eb) {
		mate.mu.Lock()
		l := mate.load
		mate.mu.Unlock()
		if l != nil {
			total += l.dps()
		}
	}
	return total
}

// fleetMatesOnGrid is every projection of this fleet in this system, the
// caller included. Snapshotted under embodyMu and released.
func (m *Mod) fleetMatesOnGrid(eb *embodiedBot) []*embodiedBot {
	if eb == nil || eb.gangID == 0 {
		return nil
	}
	embodyMu.Lock()
	var out []*embodiedBot
	for _, mate := range embodyLive {
		if mate.gangID == eb.gangID && mate.systemID == eb.systemID && mate.ent != nil {
			out = append(out, mate)
		}
	}
	embodyMu.Unlock()
	return out
}

// fcGridPrimary is the commander's call on this grid: the Director's named
// primary while it stands, else fcPickPrimary over the hostiles in reach,
// with the next pick named as the secondary. 0 when this projection is not a
// commander under a focus call, or nothing is in reach.
func (m *Mod) fcGridPrimary(eb *embodiedBot, l *combatLoadout, hostiles []uint32) uint32 {
	if !eb.isFleetCommander() {
		return 0
	}
	now := clock.Now()
	eb.mu.Lock()
	human, until, leaving := eb.call.humanPrimary, eb.call.humanUntil, eb.call.retreat
	eb.mu.Unlock()
	// A human in command (fleetmirror_command.go) calls every primary: a named one, else their
	// first tag on this grid. The commander's own judgement is off.
	if fc, real := mirrorFCOf(eb.gangID); fc != 0 {
		usable := func(id uint32) bool {
			e := eb.ss.GetEntity(id)
			return e != nil && !e.IsDestroyed() && !e.IsCloaked()
		}
		if human != 0 && usable(human) {
			m.setFCPrimary(eb, human, 0, now)
			m.setFCSplit(eb, 0, false)
			return human
		}
		var picked []uint32
		for _, id := range fcTagged(real) {
			if usable(id) {
				picked = append(picked, id)
			}
		}
		if len(picked) == 0 {
			return 0
		}
		var second uint32
		if len(picked) > 1 {
			second = picked[1]
		}
		m.setFCPrimary(eb, picked[0], second, now)
		m.setFCSplit(eb, 0, false)
		return picked[0]
	}
	if eb.fleetCmd.Call == callFree {
		return 0
	}
	if human != 0 && now.Before(until) {
		if e := eb.ss.GetEntity(human); e != nil && !e.IsDestroyed() && !e.IsCloaked() {
			m.setFCPrimary(eb, human, 0, now)
			m.setFCSplit(eb, 0, false)
			return human
		}
	}
	cands, byID, theirRep := m.fcGridCands(eb, l, hostiles)
	if len(cands) == 0 {
		return 0
	}
	fleetDPS := m.fleetGridDPS(eb)
	primary, ok := fcPickPrimary(fleetDPS, theirRep, cands, leaving)
	if !ok {
		return 0
	}
	secondary, _ := pickSecondary(cands, primary, fleetDPS, theirRep)
	m.setFCPrimary(eb, byID[primary], byID[secondary], now)
	m.setFCSplit(eb, len(cands), secondary != 0)
	return byID[primary]
}

// setFCSplit halves the line onto the primary and the secondary when the command splits against
// a swarm (fcSplitActive), and puts it back on one primary when it does not. Members read their
// half through fleetCall.primaryFor.
func (m *Mod) setFCSplit(eb *embodiedBot, hostilesInReach int, haveSecondary bool) {
	var ids []int64
	for _, mate := range m.fleetMatesOnGrid(eb) {
		ids = append(ids, mate.charID)
	}
	var half map[int64]bool
	if haveSecondary && fcSplitActive(eb.fleetCmd.Call, len(ids), hostilesInReach) {
		half = splitSecondHalf(ids, eb.charID)
	}
	eb.mu.Lock()
	said := eb.call.splitSaid
	eb.call.split = half
	eb.call.splitSaid = half != nil
	eb.mu.Unlock()
	if half != nil && !said {
		m.fleetSay(eb, "Split fire — second half on the secondary.")
	}
}

// pickSecondary is the FC's next call: fcPickPrimary with the primary taken
// out. False when the primary is the only hull left.
func pickSecondary(cands []fcCand, primary int64, fleetDPS, theirRep float64) (int64, bool) {
	rest := make([]fcCand, 0, len(cands))
	for _, c := range cands {
		if c.ID != primary {
			rest = append(rest, c)
		}
	}
	return fcPickPrimary(fleetDPS, theirRep, rest, false)
}

// setFCPrimary records the call and narrates a change of primary once.
func (m *Mod) setFCPrimary(eb *embodiedBot, primary, secondary uint32, now time.Time) {
	eb.mu.Lock()
	changed := eb.call.primary != primary
	eb.call.primary = primary
	if secondary != eb.call.secondary {
		eb.call.secondary, eb.call.secondaryAt = secondary, now
	}
	say := changed && eb.call.saidPrimary != primary
	if say {
		eb.call.saidPrimary = primary
	}
	if !eb.call.saidAnchor {
		eb.call.saidAnchor = true
		defer m.fleetSay(eb, "Anchor on me.")
	}
	eb.mu.Unlock()
	if say {
		if e := eb.ss.GetEntity(primary); e != nil {
			m.fleetSay(eb, "Primary is %s.", ballLabel(e))
		}
	}
}

// ballLabel is a ball's name for the fleet's chat: a projected pilot's name on a bot's
// ship, the type's on a rat, and "the target" for a player. The grid tick has no name
// table it may ask.
func ballLabel(e *destiny.Entity) string {
	if e == nil {
		return "the target"
	}
	if e.Slim.CharID != 0 {
		embodyMu.Lock()
		p := embodyLive[e.Slim.CharID]
		embodyMu.Unlock()
		if p != nil && p.charName != "" {
			return p.charName
		}
	}
	if rt, ok := ratThreatCached(e.Slim.TypeID); ok && rt.Name != "" {
		return rt.Name
	}
	return "the target"
}

// gridHostileIDs is the aggro half of what is on grid (hostilesOnGrid, aggroIDs), the
// part the engagement machinery acts on.
func (m *Mod) gridHostileIDs(eb *embodiedBot) []uint32 {
	return aggroIDs(m.hostilesOnGrid(eb))
}

// fcGridViews builds the two sides the retreat rule reads: the fleet's
// projections on this grid and the hostiles, as combatant stubs. Hostile DPS
// is the threat cache's for a rat; a pilot's DPS, rep and tackle are its
// ship's numbers (gridShipFactsOf).
func (m *Mod) fcGridViews(eb *embodiedBot, hostiles []uint32) (ours, theirs []*pvpCombatant) {
	for _, mate := range m.fleetMatesOnGrid(eb) {
		if mate.ent == nil {
			continue
		}
		hp := mate.ent.HPSnapshot()
		mate.mu.Lock()
		l, dead := mate.load, mate.dead
		mate.mu.Unlock()
		if dead {
			continue
		}
		dps := 0.0
		if l != nil {
			dps = l.dps()
		}
		cur := hp.CurShield + hp.CurArmor + hp.CurStructure
		ours = append(ours, &pvpCombatant{
			abstractCombatant: &abstractCombatant{DPS: dps, EHP: cur, MaxEHP: hp.MaxShield + hp.MaxArmor + hp.MaxStructure, Armed: dps > 0},
			CharID:            mate.charID, Role: mate.gangRole,
		})
	}
	if eb.ss == nil {
		return ours, nil
	}
	for _, id := range hostiles {
		e := eb.ss.GetEntity(id)
		if e == nil || e.IsDestroyed() {
			continue
		}
		hp := e.HPSnapshot()
		var f gridShipFacts
		if e.Slim.CharID != 0 {
			f = m.gridShipFactsOf(e)
		} else if rt, ok := ratThreatCached(e.Slim.TypeID); ok {
			f.DPS = rt.DPS
		}
		cur := hp.CurShield + hp.CurArmor + hp.CurStructure
		theirs = append(theirs, &pvpCombatant{
			abstractCombatant: &abstractCombatant{DPS: f.DPS, EHP: cur, MaxEHP: hp.MaxShield + hp.MaxArmor + hp.MaxStructure,
				Armed: f.DPS > 0, RepPerSec: f.Rep},
			CharID:      int64(id),
			ScramPoints: f.Points,
		})
	}
	return ours, theirs
}

// fcGridRetreat is the commander's retreat call on this grid: the fleet rule
// over what is standing here. Records the fight's start on the first ask of
// an engagement, and on a call picks the celestial the whole fleet runs to.
func (m *Mod) fcGridRetreat(eb *embodiedBot, l *combatLoadout, hostiles []uint32) bool {
	if !eb.isFleetCommander() {
		return false
	}
	eb.mu.Lock()
	if eb.call.retreat {
		eb.mu.Unlock()
		return true
	}
	engaged := eb.call.engaged
	eb.mu.Unlock()
	// A human in command calls the retreat.
	if fc, _ := mirrorFCOf(eb.gangID); fc != 0 {
		return false
	}
	ours, theirs := m.fcGridViews(eb, hostiles)
	if !engaged {
		eb.mu.Lock()
		eb.call.engaged, eb.call.start = true, startOf(ours)
		eb.mu.Unlock()
	}
	eb.mu.Lock()
	start, primaryID := eb.call.start, eb.call.primary
	eb.mu.Unlock()
	var primary *pvpCombatant
	for _, t := range theirs {
		if t.CharID == int64(primaryID) {
			primary = t
		}
	}
	call, why := fcShouldRetreat(eb.fleetCmd, ours, theirs, start, primary, m.fleetGridDPS(eb))
	if !call {
		return false
	}
	m.callFleetRetreat(eb, why)
	return true
}

// callFleetRetreat is the call itself: one destination for the whole fleet,
// said once.
func (m *Mod) callFleetRetreat(eb *embodiedBot, why string) {
	var dest int64
	if d := m.fleeDestination(eb, 0); d != nil {
		dest = d.ItemID
	}
	eb.mu.Lock()
	already := eb.call.retreat
	eb.call.retreat, eb.call.retreatWhy, eb.call.retreatDest = true, why, dest
	eb.mu.Unlock()
	if !already {
		m.logBot(eb.charID, logInfo, "combat", "calling the fleet off — %s", why)
		m.fleetSay(eb, "Align out — %s.", why)
	}
}

// fcClearCall ends the commander's calls with the engagement. A Director's
// named primary and anchor stand until their own clock runs out.
func (m *Mod) fcClearCall(eb *embodiedBot) {
	if eb == nil {
		return
	}
	eb.mu.Lock()
	eb.call.primary, eb.call.secondary, eb.call.secondaryAt = 0, 0, time.Time{}
	eb.call.retreat, eb.call.retreatWhy, eb.call.retreatDest = false, "", 0
	eb.call.engaged, eb.call.start = false, fightStart{}
	eb.call.saidPrimary, eb.call.saidAnchor = 0, false
	eb.call.split, eb.call.splitSaid = nil, false
	eb.mu.Unlock()
}

// ── what a member reads ─────────────────────────────────────────────────────

// fleetCallPrimary is the commander's called primary for this member: the
// call when the member's own rules count it a hostile and its guns reach it,
// else 0 (the member's own scorer, or gangPrimary's reading of the leader's
// lock). usable answers the per-ball questions the way choosePrimary takes it.
func (m *Mod) fleetCallPrimary(eb *embodiedBot, hostiles []uint32, usable func(uint32) bool) uint32 {
	lead := fleetLeaderLive(eb)
	if lead == nil {
		return 0
	}
	lead.mu.Lock()
	primary := lead.call.primaryFor(eb.charID)
	lead.mu.Unlock()
	return choosePrimary(primary, hostiles, usable)
}

// fleetRetreatCalled reports the commander's retreat for a member, and the
// celestial it was called to (0 = pick your own).
func fleetRetreatCalled(eb *embodiedBot) (bool, int64) {
	lead := fleetLeaderLive(eb)
	if lead == nil {
		return false, 0
	}
	lead.mu.Lock()
	defer lead.mu.Unlock()
	return lead.call.retreat, lead.call.retreatDest
}

// fleetHoldCalled reports a Director's hold: the line anchors and fires only
// on the called primary.
func fleetHoldCalled(eb *embodiedBot) bool {
	lead := fleetLeaderLive(eb)
	if lead == nil {
		return false
	}
	lead.mu.Lock()
	defer lead.mu.Unlock()
	return lead.call.hold
}

// fleetAnchorBall is the ball a member in an anchor seat orbits: the anchor's projection
// on this grid, which is the commander or the pilot the doctrine or a Director named.
// False for the anchor itself, for tackle and EWAR seats, and for a fleet with no command
// on this grid.
func (m *Mod) fleetAnchorBall(eb *embodiedBot) (uint32, bool) {
	if !anchorSeat(eb.gangRole) {
		return 0, false
	}
	lead := fleetLeaderLive(eb)
	if lead == nil {
		return 0, false
	}
	lead.mu.Lock()
	anchor, human, until := lead.call.anchor, lead.call.humanAnchor, lead.call.humanUntil
	lead.mu.Unlock()
	fc, _ := mirrorFCOf(lead.gangID)
	if human != 0 && (fc != 0 || clock.Now().Before(until)) {
		anchor = human
	}
	if anchor == 0 || anchor == lead.charID {
		return lead.ent.ID, true
	}
	if anchor == eb.charID {
		return 0, false
	}
	embodyMu.Lock()
	a := embodyLive[anchor]
	embodyMu.Unlock()
	if a == nil || a.systemID != eb.systemID || a.ent == nil || a.ent.IsDestroyed() {
		// A human anchor is a player ship on this grid; anything else not here, the commander
		// stands in.
		if p := m.pilotBall(eb, anchor); p != nil {
			return p.ID, true
		}
		return lead.ent.ID, true
	}
	return a.ent.ID, true
}

// fleetPreLockAt is when the commander called this ball as the secondary, so a
// member switching onto it counts its lock from the call rather than from
// now. False for anything not called.
func fleetPreLockAt(eb *embodiedBot, target uint32) (time.Time, bool) {
	if target == 0 {
		return time.Time{}, false
	}
	var lead *embodiedBot
	if eb.isFleetCommander() {
		lead = eb
	} else {
		lead = fleetLeaderLive(eb)
	}
	if lead == nil {
		return time.Time{}, false
	}
	lead.mu.Lock()
	defer lead.mu.Unlock()
	if lead.call.secondary == target && !lead.call.secondaryAt.IsZero() {
		return lead.call.secondaryAt, true
	}
	return time.Time{}, false
}

// ── a Director's calls (fleetmirror_orders.go) ──────────────────────────────

// fleetCommanderLive is the mirrored fleet's commander on a grid, if it is
// projected anywhere and carries a command.
func fleetCommanderLive(leaderCharID int64) *embodiedBot {
	embodyMu.Lock()
	lead := embodyLive[leaderCharID]
	embodyMu.Unlock()
	if lead == nil || lead.fleetCmd == nil || lead.ent == nil {
		return nil
	}
	return lead
}

// directorPrimary names the primary for the fight: a pilot on the
// commander's grid.
func (m *Mod) directorPrimary(lead *embodiedBot, subject int64) (string, bool) {
	e := m.pilotBall(lead, subject)
	if e == nil {
		return "", false
	}
	lead.mu.Lock()
	lead.call.humanPrimary, lead.call.humanUntil = e.ID, clock.Now().Add(fleetHumanCallSec*time.Second)
	lead.mu.Unlock()
	return ballLabel(e), true
}

// directorPrimaryBall names any ball on the commander's grid as the primary (a rat, a tag, a
// lock), where directorPrimary takes a pilot.
func (m *Mod) directorPrimaryBall(lead *embodiedBot, id uint32) (string, bool) {
	if lead.ss == nil || id == 0 {
		return "", false
	}
	e := lead.ss.GetEntity(id)
	if e == nil || e.IsDestroyed() {
		return "", false
	}
	lead.mu.Lock()
	lead.call.humanPrimary, lead.call.humanUntil = e.ID, clock.Now().Add(fleetHumanCallSec*time.Second)
	lead.mu.Unlock()
	return ballLabel(e), true
}

// directorSecondary names the ball members pre-lock.
func (m *Mod) directorSecondary(lead *embodiedBot, id uint32) (string, bool) {
	if lead.ss == nil || id == 0 {
		return "", false
	}
	e := lead.ss.GetEntity(id)
	if e == nil || e.IsDestroyed() {
		return "", false
	}
	lead.mu.Lock()
	lead.call.secondary, lead.call.secondaryAt = e.ID, clock.Now()
	lead.mu.Unlock()
	return ballLabel(e), true
}

// directorClearPrimary drops a human's named primary.
func (m *Mod) directorClearPrimary(lead *embodiedBot) {
	lead.mu.Lock()
	lead.call.humanPrimary = 0
	lead.mu.Unlock()
}

// directorAnchor names the anchor: a member of the fleet.
func (m *Mod) directorAnchor(lead *embodiedBot, member int64) bool {
	if member == 0 {
		return false
	}
	lead.mu.Lock()
	lead.call.humanAnchor, lead.call.humanUntil = member, clock.Now().Add(fleetHumanCallSec*time.Second)
	lead.mu.Unlock()
	return true
}

// directorHold sets or clears the hold.
func (m *Mod) directorHold(lead *embodiedBot, on bool) {
	lead.mu.Lock()
	lead.call.hold = on
	lead.mu.Unlock()
}
