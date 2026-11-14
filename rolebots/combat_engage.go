package rolebots

import (
	"context"
	"fmt"
	"math/rand"

	"gurimoon/seclaw"
)

// Bot-versus-bot violence that is not a robbery.
//
// This pass is the lawful sibling of piratePredationPass (combat_botvbot.go). It has the same
// cadence, caps and single-flight rule, but it does no robbery, ransom or criminal tally, so it
// may run in lowsec and null. Targets come from the rules of engagement (roe.go), not from what
// a ship carries, and both sides bring their fleets.

// combatEngagePhases are the trips that go looking for a fight. The solo belt sweep and the
// gang patrol are left out so shared belts do not become brawls; the gang hunt is included.
// They still answer a hostile that engages them.
func combatEngagePhases() []string {
	return []string{
		string(TaskCombatRoam),
		string(TaskCombatGate),
		string(TaskCombatIntercept),
		string(TaskGangHunt),
	}
}

// combatEngagementPass is the third pass beside predation and the hunt.
func (m *Mod) combatEngagementPass(ctx context.Context) {
	phases := combatEngagePhases()
	args := make([]interface{}, len(phases))
	for i, p := range phases {
		args[i] = p
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID, c.solarSystemID
		FROM rolebot_state s
		JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = s.characterID
		WHERE s.phase IN (`+placeholders(len(phases))+`) AND COALESCE(c.stationID, 0) = 0
		ORDER BY RAND()`, args...)
	if err != nil {
		rlogf("rolebots: botpvp engage scan: %v", err)
		return
	}
	type patrol struct {
		charID, sysID int64
	}
	var patrols []patrol
	for rows.Next() {
		var p patrol
		if rows.Scan(&p.charID, &p.sysID) == nil {
			patrols = append(patrols, p)
		}
	}
	rows.Close()

	done := 0
	seenGroup := map[int64]bool{} // gangID, or -charID for loners
	for _, p := range patrols {
		if done >= int(dialMaxEngagements.Get()) || ctx.Err() != nil {
			return
		}
		if m.systemProjected(p.sysID) {
			continue // a projected grid already has the embodied machinery
		}
		g, _, err := gangForChar(ctx, m.db, p.charID)
		if err != nil {
			continue
		}
		// One attempt per fleet per pass, as the predation pass does, so members of one gang do not
		// each start an engagement against the same neighbours.
		groupKey := -p.charID
		if g != nil {
			groupKey = g.GangID
		}
		if seenGroup[groupKey] {
			continue
		}
		seenGroup[groupKey] = true
		// Same-corporation pilots on a hunt in this system fly with a loner (packMates). They are
		// marked seen so the pass does not start a second engagement from the same ship.
		var pack []int64
		if g == nil && lethalityOn() {
			pack = m.packMates(ctx, p.charID, p.sysID)
			for _, id := range pack {
				seenGroup[-id] = true
			}
		}
		if m.engageOnce(ctx, p.charID, p.sysID, g, pack) {
			done++
		}
	}
}

// engageOnce runs one patrol's engagement attempt in one system.
func (m *Mod) engageOnce(ctx context.Context, leaderCharID, sysID int64, g *Gang, packIDs []int64) bool {
	lb, doc, ours, ok := m.engageSide(ctx, leaderCharID, sysID, g, packIDs)
	if !ok {
		return false
	}
	cands := m.pickEngageTargets(ctx, lb, doc, sysID, ours, g)
	if len(cands) == 0 {
		// No target the rules of engagement allow. A Mercenary checkpoint may still hold a neutral
		// for a toll (camp.go).
		return m.checkpointTollAbstract(ctx, lb, sysID, g, ours, doc)
	}
	// The catch roll is the leader's nerve, scaled. Every phase this pass selects on is a hunt,
	// so it starts from the hunt base, not the sweep base.
	base := botPvPAmbushBase
	if lethalityOn() {
		base = dialPvPAmbush.Get()
	}
	chance := base * (lb.Tune.PatrolEncounterChance / abstractPatrolEncounterChance)
	if rand.Float64() >= chance {
		return false
	}
	return m.engageResolve(ctx, lb, sysID, g, ours, cands)
}

// engageSide is a patrol's side of a fight: the leader plus the fleet, loose wing and kin
// fleets standing in the system. ok is false when there is no leader or combat wing.
func (m *Mod) engageSide(ctx context.Context, leaderCharID, sysID int64, g *Gang, packIDs []int64) (
	lb *botRun, doc Doctrine, ours []*pvpCombatant, ok bool) {
	lb, err := m.loadRun(ctx, leaderCharID)
	if err != nil {
		return nil, DoctrineNone, nil, false
	}
	doc = m.corpDoctrineFor(ctx, lb.CorpID)
	if doc == DoctrineNone {
		return nil, DoctrineNone, nil, false
	}

	// The leader plus whoever of its fleet is standing here.
	lead := m.pvpCombatantFor(ctx, leaderCharID, RoleHunter)
	if lead == nil {
		return nil, DoctrineNone, nil, false
	}
	ours = append(ours, lead)
	if g != nil {
		members, err := gangMembers(ctx, m.db, g.GangID)
		if err == nil {
			for _, mem := range members {
				if !mem.Driven || mem.CharacterID == leaderCharID {
					continue
				}
				pc := m.pvpCombatantFor(ctx, mem.CharacterID, mem.Role)
				if pc == nil || pc.SystemID != sysID {
					continue
				}
				ours = append(ours, pc)
			}
		}
	}
	// The loose wing: same-corporation pilots on a hunt in this system with no fleet (packMates).
	// Their points and guns are pooled like a fleet's.
	for _, id := range packIDs {
		if pc := m.pvpCombatantFor(ctx, id, RoleCombat); pc != nil && pc.SystemID == sysID {
			ours = append(ours, pc)
		}
	}
	// Kin fleets on this grid (gangcalls.go) fight alongside this one: separate gangs and
	// commands, one pooled roster.
	var ourGang int64
	if g != nil {
		ourGang = g.GangID
	}
	ours = append(ours, m.siblingCombatants(ctx, lb.CorpID, ourGang, sysID, combatantIDs(ours))...)
	return lb, doc, ours, true
}

// engageResolve weighs the candidates on whether they can be caught, picks the best, fights
// it and bills it. Shared by engageOnce (after its encounter roll) and siteRivalFight (no
// roll, both sides are in one pocket).
func (m *Mod) engageResolve(ctx context.Context, lb *botRun, sysID int64, g *Gang,
	ours []*pvpCombatant, cands []engageCand) bool {
	leaderCharID := lb.CharID
	var oDPS, oEHP float64
	for _, c := range ours {
		oDPS += c.DPS
		oEHP += c.EHP
	}

	// A candidate is judged on whether it can be caught, not on whether the patrol outweighs it
	// (pvpescape.go engageVerdict). With the dial off the old margin test applies.
	type pick struct {
		fb        *botRun
		foe       *pvpCombatant
		theirs    []*pvpCombatant
		foeFights bool
		odds      float64
		score     float64
		why       motive
	}
	var best *pick
	// For the "passed up" line: the best odds reached, the line needed, and how many
	// candidates would have stood and fought.
	var seenOdds, seenFloor float64
	seenStand := 0
	for _, c := range cands {
		fb, err := m.loadRun(ctx, c.charID)
		if err != nil {
			continue
		}
		foe := m.pvpCombatantFor(ctx, c.charID, RoleCombat)
		if foe == nil {
			continue
		}
		// Their fleet stands with them as ours does with us. This is all of gang-versus-gang:
		// two rosters arrive with their friends.
		theirs := []*pvpCombatant{foe}
		if fb.State != nil && fb.State.Data.GangID != 0 {
			mates, err := m.gangCombatRoster(ctx, fb.State.Data.GangID, c.charID)
			if err == nil {
				for _, mate := range mates {
					if mate.SystemID != sysID {
						continue
					}
					if pc := m.pvpCombatantFor(ctx, mate.CharID, RoleCombat); pc != nil {
						theirs = append(theirs, pc)
					}
				}
			}
		}
		// Their kin fleets on the grid stand with them the same way.
		var foeGang int64
		if fb.State != nil {
			foeGang = fb.State.Data.GangID
		}
		have := combatantIDs(theirs)
		for id := range combatantIDs(ours) {
			have[id] = true
		}
		theirs = append(theirs, m.siblingCombatants(ctx, fb.CorpID, foeGang, sysID, have)...)
		var tDPS, tEHP float64
		for _, t := range theirs {
			tDPS += t.DPS
			tEHP += t.EHP
		}
		foeFights := wouldEngage(fb.Tune, engageMargin(tDPS, tEHP, oDPS, oEHP))
		odds := 1.0
		if foeFights {
			seenStand++
		}
		if lethalityOn() {
			odds = engageVerdict(ours, theirs, foeFights)
			floor := engageFloor(c.atWar, dialMinKillOdds.Get(), dialWarMinKillOdds.Get())
			if odds > seenOdds || seenFloor == 0 {
				seenOdds, seenFloor = odds, floor
			}
			if odds < floor {
				continue
			}
		} else if !wouldEngage(lb.Tune, engageMargin(oDPS, oEHP, tDPS, tEHP)) {
			continue
		}
		score := odds
		for _, bonus := range []bool{c.outlaw, c.atWar, c.bounty > 0} {
			if bonus {
				score += odds * 0.5
			}
		}
		if best == nil || score > best.score {
			best = &pick{fb: fb, foe: foe, theirs: theirs, foeFights: foeFights, odds: odds, score: score, why: c.why}
		}
	}
	if best == nil {
		m.logBot(leaderCharID, logInfo, "combat",
			"passed up %d possible target(s) in %s — nothing it could catch (best odds %.2f, needed %.2f, %d would stand and fight)",
			len(cands), m.nameOfSystem(ctx, sysID), seenOdds, seenFloor, seenStand)
		// Somebody here would stand and fight and this fleet cannot take them: ask the kin fleets
		// nearby to come (gangcalls.go).
		if g != nil && seenStand > 0 {
			m.postHelpCall(ctx, g.GangID, lb.CorpID, sysID, helpOutnumbered, len(ours)+seenStand,
				fmt.Sprintf("%s found a fight it cannot take alone in %s", g.Name, m.nameOfSystem(ctx, sysID)))
		}
		return false
	}
	fb, foe, theirs, foeFights := best.fb, best.foe, best.theirs, best.foeFights
	foeID := foe.CharID
	noteHostileEncounter(gangScope(g), sysID)
	m.logBot(leaderCharID, logInfo, "combat", "engaged %s in %s — %s",
		fb.Name, m.nameOfSystem(ctx, sysID),
		map[bool]string{true: "they turned and fought", false: "they ran for the gate"}[foeFights])
	if g != nil {
		m.narrateFleet(ctx, g.GangID, true, "Engaging %s in %s.", fb.Name, m.nameOfSystem(ctx, sysID))
	}
	// Nobody is on this grid, but somebody may be in this Local.
	m.chatterEngageAbstract(ctx, lb, fb, sysID, best.why)

	// Billed after the fight, only for shots that landed, then answered by CONCORD where it
	// would be (pvplaw.go). Each side acts under its fleet's command (fleetcommand.go) if it has one.
	ourCmd, ourBefore := fleetCommandOf(g), fleetCommandOf(g)
	theirCmd, theirGang := m.fleetCommandForChar(ctx, foeID)
	var theirBefore *FleetCommand
	if theirCmd != nil {
		c := *theirCmd
		theirBefore = &c
	}
	out := resolveTeamFightCmd(ours, theirs,
		lb.Tune.BreakOffFrac, fb.Tune.BreakOffFrac, !foeFights, ourCmd, theirCmd)
	if g != nil {
		m.commitFleetCommand(ctx, g.GangID, ourBefore, ourCmd)
	}
	m.commitFleetCommand(ctx, theirGang, theirBefore, theirCmd)
	offenders := m.chargeAbstractAggression(ctx, ours, theirs, sysID)
	defer m.abstractConcord(ctx, offenders, sysID, float64(out.Ticks), out.AEscaped)

	// crime=false on both sides: not piracy, so no criminal timer, no security status loss and
	// no bounty board change.
	for _, dead := range out.BDead {
		m.killBotShip(ctx, dead, ours, false)
	}
	for _, dead := range out.ADead {
		m.killBotShip(ctx, dead, theirs, false)
	}
	// The dead pilots' employers put a price on the killer when it was a crime or a war
	// (pvplaw.go).
	m.fileCrimeBounties(ctx, out.BDead, ours, offenders)
	// A side that lost ships calls its kin in (gangcalls.go).
	if g != nil && len(out.ADead) > 0 {
		m.postHelpCall(ctx, g.GangID, lb.CorpID, sysID, helpRetreat, len(theirs),
			fmt.Sprintf("%s lost %d ship(s) to %s in %s", g.Name, len(out.ADead), fb.Name, m.nameOfSystem(ctx, sysID)))
	}
	if theirGang != 0 && len(out.BDead) > 0 {
		m.postHelpCall(ctx, theirGang, fb.CorpID, sysID, helpRetreat, len(ours),
			fmt.Sprintf("lost %d ship(s) to %s in %s", len(out.BDead), lb.Name, m.nameOfSystem(ctx, sysID)))
	}
	m.logBot(leaderCharID, logInfo, "combat",
		"engagement result: %s; odds=%.2f window=%.0fs held=%t foealign=%.0fs",
		pvpOutcomeLabel(out), best.odds, sideEscapeWindow(theirs),
		sideTacklePoints(ours) > sideWarpCore(theirs), foe.AlignSec)
	if out.BEscaped {
		m.logBot(foeID, logInfo, "combat", "broke off and warped clear in %s",
			m.nameOfSystem(ctx, sysID))
	}
	return true
}

// engageCand is a rolebot this patrol's rules of engagement allow it to shoot, with the facts
// the ranking in engageOnce prefers: an outlaw, war enemy or priced pilot outranks a neutral
// picked through the nerve roll.
type engageCand struct {
	charID int64
	outlaw bool
	atWar  bool
	bounty float64
	// why is the motive chatter gives for the shot (chatter_motive.go).
	why motive
}

// engageCandCap bounds how many candidates one patrol weighs: each costs a ship read and a
// fleet roster.
const engageCandCap = 6

// pickEngageTargets finds the rolebots in the system this patrol's rules of engagement allow
// it to shoot, up to engageCandCap, in random order. The nerve is rolled once per fleet, keyed
// by the negated gang ID like the embodied path.
func (m *Mod) pickEngageTargets(ctx context.Context, lb *botRun, doc Doctrine,
	sysID int64, ours []*pvpCombatant, g *Gang) []engageCand {

	oursChars := map[int64]bool{}
	oursCorps := map[int64]bool{}
	for _, c := range ours {
		if c != nil {
			oursChars[c.CharID] = true
			oursCorps[c.CorpID] = true
		}
	}

	phases := []string{}
	for kind, def := range taskRegistry {
		if def.Exposed {
			phases = append(phases, string(kind))
		}
	}
	if len(phases) == 0 {
		return nil
	}
	args := make([]interface{}, 0, len(phases)+1)
	for _, p := range phases {
		args = append(args, p)
	}
	args = append(args, sysID)
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID, c.corporationID, COALESCE(c.securityRating, 0), COALESCE(c.bounty, 0)
		FROM rolebot_state s
		JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = s.characterID
		WHERE s.phase IN (`+placeholders(len(phases))+`)
		  AND c.solarSystemID = ? AND COALESCE(c.stationID, 0) = 0
		ORDER BY RAND()`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	band := secBand(m.systemSecurity(ctx, sysID))
	decider := lb.CharID
	if g != nil {
		decider = -g.GangID
	}
	// The fleet's nerve is its commander's (roe.go).
	nerve := lb.Tune.NeutralAggression
	if g != nil {
		// The commander's, not the picked member's: the decider key is the gang's and the embodied
		// tier reads the commander's number the same way.
		nerve = m.gangNerve(ctx, g.GangID, nerve)
	}
	var out []engageCand

	for rows.Next() {
		var charID, corpID int64
		var secStatus, bounty float64
		if rows.Scan(&charID, &corpID, &secStatus, &bounty) != nil || popIsOffline(charID) {
			continue
		}
		if oursChars[charID] || oursCorps[corpID] {
			continue
		}
		// Any member of the fleet vetoes kin, unlike the nerve roll. Same split as pickAbstractPrey
		// and the grid.
		kin := false
		for _, c := range ours {
			if c != nil && m.kinToPilot(ctx, c.Self, charID) {
				kin = true
				break
			}
		}
		if kin {
			continue
		}
		// Friendship is vetoed the same way.
		friend := false
		for _, c := range ours {
			if c != nil && m.standingToPilot(ctx, c.Self, charID) >= StandingLiked {
				friend = true
				break
			}
		}
		if friend {
			continue
		}

		read := m.standingMotiveToPilotFor(ctx, lb.standingSelf(), charID, true, doc)
		st := stanceOf(read.Standing, false)
		// The core's table for this system, or a flag the pilot carried out of a loaded one
		// (criminalcarry.go).
		criminal := criminalSince(sysID, charID) > 0
		// War is asked about the leader's owners, the side the stance and nerve are read from: one
		// fleet, one decision (roe.go). Kin and friendship are the only per-member vetoes.
		atWar := false
		if warEverywhere() {
			atWar = relAtWar(lb.CorpID, m.corpAllianceID(ctx, lb.CorpID),
				corpID, m.corpAllianceID(ctx, corpID))
		}
		// An outlaw is a legal target for everybody in any band (roe.go). The record comes from the
		// row, which does not depend on a ball having stood in a loaded system.
		outlaw := secStatus <= seclaw.OutlawStatus
		if ok, _ := roeEngage(st, doc, band, criminal, outlaw, atWar, nerve, decider, charID); ok {
			out = append(out, engageCand{charID: charID, outlaw: outlaw || criminal, atWar: atWar, bounty: bounty,
				why: abstractMotive(read, criminal, outlaw, atWar, bounty)})
			if len(out) >= engageCandCap {
				break
			}
		}
	}
	return out
}

// systemSecurity is a system's truesec from the universe graph, loading it if needed. It runs
// on the sim side, unlike loadedSystemSecurity (conscript_embodied.go), and fails to 1.0, the
// strictest reading, so a failure means nobody is shot.
func (m *Mod) systemSecurity(ctx context.Context, sysID int64) float64 {
	u, err := m.universe(ctx)
	if err != nil {
		return 1.0
	}
	ms, ok := u.meta[sysID]
	if !ok {
		return 1.0
	}
	return dispSec(ms.Security)
}

// packMateCap is how many pilots join a loner: a patrol of four, the size the formation code
// builds a PvP fleet to.
const packMateCap = 3

// packMates finds the pilots who fly with a loner for this pass: same corporation, same
// system, on a hunt, in no fleet, and past the same rules of engagement as the leader. It is a
// loose wing, not a formal fleet (gangform.go).
func (m *Mod) packMates(ctx context.Context, leaderCharID, sysID int64) []int64 {
	var corpID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, leaderCharID).Scan(&corpID); err != nil || corpID == 0 {
		return nil
	}
	phases := combatEngagePhases()
	args := make([]interface{}, 0, len(phases)+3)
	for _, p := range phases {
		args = append(args, p)
	}
	args = append(args, corpID, sysID, leaderCharID)
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID
		FROM rolebot_state s
		JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = s.characterID
		WHERE s.phase IN (`+placeholders(len(phases))+`)
		  AND c.corporationID = ? AND c.solarSystemID = ? AND c.characterID <> ?
		  AND COALESCE(c.stationID, 0) = 0
		ORDER BY RAND() LIMIT 8`, args...)
	if err != nil {
		return nil
	}
	var cands []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			cands = append(cands, id)
		}
	}
	rows.Close()
	var out []int64
	for _, id := range cands {
		if len(out) >= packMateCap {
			break
		}
		if g, _, err := gangForChar(ctx, m.db, id); err != nil || g != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}
