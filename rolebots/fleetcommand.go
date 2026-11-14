package rolebots

import (
	"context"
	"math"
	"sort"
)

// Fleet command: how a fleet is run while it is out. FleetCommand is the shared command
// state, written at formation from the doctrine kind's defaults and carried on GangState
// (persisted JSON, reset with the blackboard on a gang reboot). It holds the FC and
// succession (promote), the call (fcChoosePrimary), rep priority by seat (repRank) and
// the retreat rule (fcShouldRetreat). Every decision is pure and is read by the abstract
// fight (resolveTeamFightCmd) and by the grid. A nil command means no commander:
// operator gangs, mining fleets. Design: docs/FLEET_DOCTRINE.md section 2.

type callPolicy string

const (
	callFocus callPolicy = "focus" // one primary, the FC's
	callSplit callPolicy = "split" // two primaries when a swarm outnumbers the line (fcSplitActive)
	callFree  callPolicy = "free"  // every member its own scorer
)

// fcSplitRatio is how far the hostiles in reach must outnumber the line before a split
// command halves it onto two primaries.
const fcSplitRatio = 1.5

// fcSplitActive reports whether a command splits its fire now: the policy is split, the
// line has two hulls to halve, and the hostiles in reach outnumber it by fcSplitRatio. Pure.
func fcSplitActive(call callPolicy, line, hostiles int) bool {
	return call == callSplit && line >= 2 && hostiles >= 2 && float64(hostiles) >= fcSplitRatio*float64(line)
}

// splitSecondHalf is the half of the line that shoots the secondary under a split: the
// members other than the commander, ordered by charID so the halves are stable from tick
// to tick, take the secondary from the back, half the line (commander counted) rounded
// down. The commander and the rest keep the primary. Pure.
func splitSecondHalf(members []int64, fc int64) map[int64]bool {
	rest := make([]int64, 0, len(members))
	for _, id := range members {
		if id != fc {
			rest = append(rest, id)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	out := map[int64]bool{}
	for _, id := range rest[len(rest)-(len(rest)+1)/2:] {
		out[id] = true
	}
	return out
}

// retreatRule is when the FC calls the fleet off. Zero fields are "never on
// that ground"; a wholly zero rule never retreats on its own.
type retreatRule struct {
	// FleetEHPFrac: call it when the line's pooled EHP falls under this share
	// of what it started the fight with.
	FleetEHPFrac float64 `json:"ehp,omitempty"`
	// LogiLost: call it when the last logi dies (a fleet that started with one).
	LogiLost bool `json:"logiLost,omitempty"`
	// PrimaryHeld: call it when the enemy's rep on the primary outruns what the fleet
	// lands on it, so the target cannot be broken.
	PrimaryHeld bool `json:"primaryHeld,omitempty"`
	// OddsFlip: call it when this side's share of the field's fighting power (DPS times
	// EHP) falls under this.
	OddsFlip float64 `json:"odds,omitempty"`
}

// FleetCommand is one fleet's command state.
type FleetCommand struct {
	FC         int64   `json:"fc"`
	Succession []int64 `json:"succession,omitempty"`
	// Anchor is the hull the line holds position on (grid command); the FC by default.
	Anchor int64 `json:"anchor,omitempty"`

	Call callPolicy `json:"call"`
	// Primary and Secondary are the called targets: an entity ID on the grid,
	// a charID in the abstract. Live state, not a setting.
	Primary   int64 `json:"primary,omitempty"`
	Secondary int64 `json:"secondary,omitempty"`

	// The envelope: the line's hold distance and a place it holds on a quiet grid (fleetHoldPoint).
	// HoldAt is the celestial the line warps to (a siege: the tower's moon; a camp: the gate).
	// HoldBall is the ball it then orbits at HoldRangeM (a siege: the tower, outside its force
	// field); 0 orbits HoldAt itself. HoldBash: the line's guns show fire on HoldBall while the
	// abstract bash runs (siege_embodied.go). The commander's projection fills them from the
	// fleet's orders at embodiment (loadFleetSeat); the stored command leaves them zero.
	RangeM     float64 `json:"rangeM,omitempty"`
	HoldAt     int64   `json:"holdAt,omitempty"`
	HoldBall   int64   `json:"holdBall,omitempty"`
	HoldRangeM float64 `json:"holdRangeM,omitempty"`
	HoldBash   bool    `json:"holdBash,omitempty"`

	Retreat   retreatRule `json:"retreat"`
	RetreatTo int64       `json:"retreatTo,omitempty"`
	AlignOut  bool        `json:"alignOut,omitempty"`
	CalledAt  int64       `json:"calledAt,omitempty"`
}

// kindRetreat is each kind's retreat rule at a commander of ordinary nerve. A siege
// line is stubborn; a skirmish pack is not.
var kindRetreat = map[doctrineKind]retreatRule{
	kindBrawl:    {FleetEHPFrac: 0.35, LogiLost: true, PrimaryHeld: true, OddsFlip: 0.30},
	kindSniper:   {FleetEHPFrac: 0.40, LogiLost: true, PrimaryHeld: true, OddsFlip: 0.35},
	kindKite:     {FleetEHPFrac: 0.50, LogiLost: false, PrimaryHeld: true, OddsFlip: 0.40},
	kindSkirmish: {FleetEHPFrac: 0.50, LogiLost: false, PrimaryHeld: true, OddsFlip: 0.35},
	kindSiege:    {FleetEHPFrac: 0.20, LogiLost: true, PrimaryHeld: false, OddsFlip: 0.20},
	kindGuard:    {FleetEHPFrac: 0.40, LogiLost: true, PrimaryHeld: true, OddsFlip: 0.30},
	// A gank fleet's strikers are spent when they fire, so the rule matters only before the
	// strike. It must stay less stubborn than kindSiege (pinned in fleetcommand_test).
	kindGank: {FleetEHPFrac: 0.25, LogiLost: false, PrimaryHeld: false, OddsFlip: 0.20},
}

// defaultRetreat is the rule for a fleet with no doctrine kind on record.
var defaultRetreat = retreatRule{FleetEHPFrac: 0.35, LogiLost: true, PrimaryHeld: true, OddsFlip: 0.30}

// defaultFleetCommand is a new fleet's command: the kind's call and retreat rule, the
// commander in the seat, the succession as given. nerve scales the retreat thresholds:
// 1 is ordinary, above it a careful commander leaves sooner, below it a reckless one stays.
// A guard fights free; the heavy lines (sniper, siege) split against a swarm.
func defaultFleetCommand(kind doctrineKind, fc int64, succession []int64, nerve float64) *FleetCommand {
	rule, ok := kindRetreat[kind]
	if !ok {
		rule = defaultRetreat
	}
	cmd := &FleetCommand{
		FC: fc, Succession: succession, Anchor: fc,
		Call:    callFocus,
		Retreat: scaleRetreat(rule, nerve),
	}
	switch kind {
	case kindGuard:
		cmd.Call = callFree
	case kindSniper, kindSiege:
		cmd.Call = callSplit
	}
	return cmd
}

// scaleRetreat applies a commander's nerve to a rule, bounded to 0.5..1.5 so a reckless
// FC still leaves and a careful one still fights.
func scaleRetreat(r retreatRule, nerve float64) retreatRule {
	if nerve <= 0 {
		return r
	}
	k := math.Max(0.5, math.Min(1.5, nerve))
	r.FleetEHPFrac = math.Min(0.9, r.FleetEHPFrac*k)
	r.OddsFlip = math.Min(0.9, r.OddsFlip*k)
	return r
}

// fcNerve is a commander's nerve as a multiplier on the rule: its own
// break-off fraction against the module default.
func fcNerve(tn Tuning) float64 {
	if abstractBreakOffFrac <= 0 || tn.BreakOffFrac <= 0 {
		return 1
	}
	return tn.BreakOffFrac / abstractBreakOffFrac
}

// promote hands the seat to the first living name in the succession when the
// FC is dead. Reports whether a commander is in the seat afterwards: false
// is a headless fleet, which fights as it did before it had a command.
func (c *FleetCommand) promote(alive func(charID int64) bool) bool {
	if c == nil {
		return false
	}
	if c.FC != 0 && alive(c.FC) {
		return true
	}
	for _, id := range c.Succession {
		if id != c.FC && alive(id) {
			c.FC = id
			if c.Anchor == 0 || !alive(c.Anchor) {
				c.Anchor = id
			}
			return true
		}
	}
	return false
}

// repRank orders a side's wounded for the logi under a command: the logi
// first, then the tackle, then the anchor, then the line. Without a command
// it is repPriority's role order, which is the same order minus the anchor.
func repRank(c *pvpCombatant, cmd *FleetCommand) int {
	if c == nil {
		return 9
	}
	switch c.Role {
	case RoleLogi:
		return 0
	case RoleTackler:
		return 1
	}
	if cmd != nil && cmd.Anchor != 0 && c.CharID == cmd.Anchor {
		return 2
	}
	return 3
}

// fcChoosePrimary is the FC's call: which living enemy hull the fleet shoots. The rule
// is in fcPickPrimary. Ties go to the lower EHP, then the lower charID, so the call is
// stable from tick to tick. Nil when nothing on the other side is alive.
func fcChoosePrimary(ours, theirs []*pvpCombatant, leaving bool) *pvpCombatant {
	var fleetDPS, theirRep float64
	for _, c := range ours {
		if c != nil && c.EHP > 0 {
			fleetDPS += c.DPS
		}
	}
	byID := map[int64]*pvpCombatant{}
	var cands []fcCand
	for i, c := range theirs {
		if c == nil || c.EHP <= 0 {
			continue
		}
		theirRep += c.RepPerSec
		// The index stands in for a combatant with no charID (a test hull); a real charID wins.
		id := c.CharID
		if id == 0 {
			id = -int64(i + 1)
		}
		byID[id] = c
		cands = append(cands, fcCand{ID: id, EHP: c.EHP, Rep: c.RepPerSec, Points: c.ScramPoints})
	}
	id, ok := fcPickPrimary(fleetDPS, theirRep, cands, leaving)
	if !ok {
		return nil
	}
	return byID[id]
}

// fcCand is one enemy hull as the FC's scorer sees it on either tier: remaining
// hitpoints, remote repair and tackle. The grid builds them off balls
// (fleetcommand_grid.go), the abstract fight off its combatants.
type fcCand struct {
	ID          int64
	EHP         float64
	Rep, Points float64
}

// fcPickPrimary is the FC's call over candidates, tier-free. fleetDPS is what this side
// lands a second, theirRep what the other side can put back. When leaving it takes the
// tackle first, else the logi, else the fastest kill. Returns the ID, false for none.
func fcPickPrimary(fleetDPS, theirRep float64, cands []fcCand, leaving bool) (int64, bool) {
	type scored struct {
		c   fcCand
		ttk float64
	}
	var tackle, logi, line []scored
	for _, c := range cands {
		if c.EHP <= 0 {
			continue
		}
		net := fleetDPS - theirRep
		ttk := math.Inf(1)
		if net > 0 {
			ttk = c.EHP / net
		}
		k := scored{c, ttk}
		switch {
		case c.Points > 0:
			tackle = append(tackle, k)
		case c.Rep > 0:
			logi = append(logi, k)
		}
		line = append(line, k)
	}
	pick := func(cs []scored) (int64, bool) {
		if len(cs) == 0 {
			return 0, false
		}
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].ttk != cs[j].ttk {
				return cs[i].ttk < cs[j].ttk
			}
			if cs[i].c.EHP != cs[j].c.EHP {
				return cs[i].c.EHP < cs[j].c.EHP
			}
			return cs[i].c.ID < cs[j].c.ID
		})
		return cs[0].c.ID, true
	}
	if leaving {
		if id, ok := pick(tackle); ok {
			return id, true
		}
	}
	if id, ok := pick(logi); ok {
		return id, true
	}
	return pick(line)
}

// fightStart is what a side began the fight with, for the retreat rule.
type fightStart struct {
	MaxEHP  float64
	HadLogi bool
}

func startOf(side []*pvpCombatant) fightStart {
	var s fightStart
	for _, c := range side {
		if c == nil {
			continue
		}
		s.MaxEHP += c.MaxEHP
		if c.Role == RoleLogi && c.EHP > 0 {
			s.HadLogi = true
		}
	}
	return s
}

// fcShouldRetreat is the FC's other call. primary is the hull the fleet is shooting
// (nil when nothing is); landed is what the fleet puts on it a second, before the
// enemy's rep. The returned text is for the log.
func fcShouldRetreat(cmd *FleetCommand, ours, theirs []*pvpCombatant, start fightStart,
	primary *pvpCombatant, landed float64) (bool, string) {
	if cmd == nil {
		return false, ""
	}
	r := cmd.Retreat
	var cur, oursPow, theirsPow, theirRep float64
	logiAlive := false
	for _, c := range ours {
		if c == nil || c.EHP <= 0 {
			continue
		}
		cur += c.EHP
		oursPow += c.DPS * c.EHP
		if c.Role == RoleLogi {
			logiAlive = true
		}
	}
	for _, c := range theirs {
		if c == nil || c.EHP <= 0 {
			continue
		}
		theirsPow += c.DPS * c.EHP
		theirRep += c.RepPerSec
	}
	if r.FleetEHPFrac > 0 && start.MaxEHP > 0 && cur/start.MaxEHP < r.FleetEHPFrac {
		return true, "the line is under its hitpoint floor"
	}
	if r.LogiLost && start.HadLogi && !logiAlive {
		return true, "the last logi is gone"
	}
	if r.PrimaryHeld && primary != nil && landed > 0 && theirRep >= landed {
		return true, "the primary cannot be broken"
	}
	if r.OddsFlip > 0 && oursPow+theirsPow > 0 && oursPow/(oursPow+theirsPow) < r.OddsFlip {
		return true, "the odds have turned"
	}
	return false, ""
}

// ── the Mod half: building and committing a command ─────────────────────────

// buildSuccession orders a fleet's members for the seat: the commander's own
// role first (its planner is the one that picks the site), inside that by
// Leadership skill, then by characterID so the list is stable. Escorts and
// riders never lead.
func (m *Mod) buildSuccession(ctx context.Context, activity string, recruits []gangRecruitPick) []int64 {
	type cand struct {
		id    int64
		lead  bool
		level int
	}
	leadRole := gangLeaderRole(activity)
	var cs []cand
	for _, r := range recruits {
		if r.role == RoleEscort || r.fromCorpID != 0 {
			continue
		}
		lv, _ := m.charSkillLevelFor(ctx, r.charID, skillLeadership)
		cs = append(cs, cand{r.charID, r.role == leadRole, lv})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].lead != cs[j].lead {
			return cs[i].lead
		}
		if cs[i].level != cs[j].level {
			return cs[i].level > cs[j].level
		}
		return cs[i].id < cs[j].id
	})
	out := make([]int64, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.id)
	}
	return out
}

// fleetCommandOf is a fleet's command for one fight: a copy of the stored one, so the
// fight's promotions and calls never write the blackboard. Nil for a fleet without one.
func fleetCommandOf(g *Gang) *FleetCommand {
	if g == nil {
		return nil
	}
	return fleetCommandOfState(&g.State)
}

// fleetCommandOfState is fleetCommandOf for a caller holding the state alone.
func fleetCommandOfState(st *GangState) *FleetCommand {
	if st == nil || st.Command == nil {
		return nil
	}
	c := *st.Command
	c.Succession = append([]int64(nil), st.Command.Succession...)
	return &c
}

// fleetCommandForChar is the command of the fleet a pilot is in, as a copy,
// with the gangID to commit a promotion against. Nil, 0 for a pilot in no
// fleet or a fleet with no command.
func (m *Mod) fleetCommandForChar(ctx context.Context, charID int64) (*FleetCommand, int64) {
	g, _, err := gangForChar(ctx, m.db, charID)
	if err != nil || g == nil {
		return nil, 0
	}
	return fleetCommandOf(g), g.GangID
}

// commitFleetCommand writes a fight's outcome back: a new FC takes the leader's seat
// (setGangLeader, the seat handOverGangLead fills) and the command's FC and anchor move
// with it. The primary and retreat are the fight's own and are not kept.
func (m *Mod) commitFleetCommand(ctx context.Context, gangID int64, before, after *FleetCommand) {
	if before == nil || after == nil || before.FC == after.FC {
		return
	}
	fresh, err := getGang(ctx, m.db, gangID)
	if err != nil || fresh == nil || fresh.State.Command == nil {
		return
	}
	if err := setGangLeader(ctx, m.db, gangID, after.FC); err != nil {
		rlogf("rolebots: fleet %d: promote FC %d: %v", gangID, after.FC, err)
		return
	}
	fresh.State.Command.FC = after.FC
	fresh.State.Command.Anchor = after.Anchor
	if err := saveGangState(ctx, m.db, gangID, &fresh.State); err != nil {
		rlogf("rolebots: fleet %d: save command: %v", gangID, err)
		return
	}
	m.logBot(after.FC, logInfo, "gang", "took command of %q — the commander went down in the fight", fresh.Name)
}
