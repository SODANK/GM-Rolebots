package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
)

// The combat patrol: a pilot goes out looking for action, and the kind of action is drawn from
// a weighted table by the employer's doctrine (combattactic.go): belt sweep, belt relief, gate
// hold, roam, run down a mark, or robbery. Robbery delegates to piracyPlan, which keeps its own
// site pick. The slug is "combat"; migrateCombatRename moves rows stored under old names.
//
// Fighting itself is the posture: unwatched, the abstract tier rolls encounters against
// Tuning.PatrolEncounterChance and resolves the duel; in a loaded system the projection's
// combat loop takes over (task.go Posture and Exposed flags).

// errNoHuntingGround marks a hunt plan, solo or pack, that found no belt system it could both
// reach and win in. It is a fact about the map and sweep clocks, so the planner passes over it
// (refusalIsPassOver). The literal starts both messages built on it.
var errNoHuntingGround = errors.New("no belt system")

const (
	// How long a hunter works one system before moving on, matching the gang sweep.
	huntSweepSec = 15 * 60
	// How many systems back a hunter remembers sweeping, so it does not ping-pong between two
	// neighbours. Per bot; a smaller ring let a roster converge on the shared bitten map.
	huntRecentMax = 12
)

// combatPlan is entered from idle: get into the hunting ship, pick a system, and go.
func (m *Mod) combatPlan(ctx context.Context, b *botRun) error {
	cfg := m.combatConfigDerived(ctx, b)
	st := b.State
	st.Activity = ActCombat

	ready, err := m.ensureActivityShip(ctx, b, ActCombat)
	if err != nil || !ready {
		return err
	}

	// Unfinished business from the last sweep comes first, as in piracyPlan: a floating can is on
	// a despawn clock and a hold carrying a haul has no room for another.
	if cfg.LootField {
		if handled, err := m.combatResumeLoot(ctx, b); err != nil {
			return err
		} else if handled {
			return nil
		}
		if handled, err := m.combatResumeSale(ctx, b); err != nil {
			return err
		} else if handled {
			return nil
		}
	}
	// An agent mission in hand comes back before a new trip is drawn (missions.go): the agent's
	// clock is running and an abandoned mission costs standing.
	if handled, err := m.missionResume(ctx, b); err != nil || handled {
		return err
	}

	// Which kind of combat trip this is (combattactic.go). A tactic whose site comes back empty
	// falls through to the next one, since the board can empty between the weighing and the pick.
	doc := m.corpDoctrineFor(ctx, b.CorpID)
	tactic := m.pickCombatTactic(ctx, b, cfg, doc)
	// A robbery is flown whole by the piracy planner: prelude, ship and site pick. With nowhere to
	// rob in reach the pilot sweeps belts instead.
	if tactic.Slug == tacticRobbery {
		perr := m.piracyPlan(ctx, b)
		if perr == nil {
			return nil
		}
		if !strings.HasPrefix(perr.Error(), "no system between") {
			return perr
		}
		st.Activity = ActCombat
		tactic, _ = combatTacticBySlug(tacticBeltSweep)
	}
	sysID, extra, name, err := tactic.Site(m, ctx, b, cfg)
	if err != nil {
		return err
	}
	if sysID == 0 && tactic.Slug != tacticBeltSweep {
		tactic, _ = combatTacticBySlug(tacticBeltSweep)
		sysID, extra, name, err = tactic.Site(m, ctx, b, cfg)
		if err != nil {
			return err
		}
	}
	if sysID == 0 {
		return errNoHuntingGround
	}

	keep := st.Data
	st.Data = idleData(keep)
	st.Data.HuntSystemID = sysID
	st.Data.HuntPickedAt = clock.Unix()
	st.Data.CombatTactic = tactic.Slug
	st.Data.TacticRecent = pushTacticRecent(keep.TacticRecent, tactic.Slug)
	switch tactic.Slug {
	case tacticGateHold:
		st.Data.CombatGateID = extra
	case tacticRunDown:
		st.Data.CombatMarkID = extra
	case tacticSite:
		st.Data.SiteID = extra
		st.Data.SiteStartedAt = clock.Unix()
	case tacticMission:
		// The agent's desk, not the system, is the destination.
		return m.missionBegin(ctx, b, extra, name)
	}
	target := huntTargetSummary(cfg)
	if tactic.Slug == tacticSite {
		target = "whatever the site holds" // its own faction, not the hunt list (siteFactionHuntable)
	}
	m.logBot(b.CharID, logInfo, "combat", "%s in %s (%s) — %s",
		tactic.Label, name, doc.Label(), target)
	return m.beginTravel(ctx, b, sysID, 0, string(tactic.Task))
}

// combatSite picks where the hunt happens. Home ground first: the belts around the pilot's
// home station, then every system its corporation keeps an address in, nearest first. Then
// weight class at every step: each candidate is read against the pilot's, or on a gang op the
// pack's pooled, guns (threat.go). A hunter that can win nowhere in its band walks up.
func (m *Mod) combatSite(ctx context.Context, b *botRun, cfg combatConfig) (int64, string, error) {
	u, err := m.universe(ctx)
	if err != nil {
		return 0, "", err
	}
	// The hunt is scoped as the belt walk is (residency.go): own region and neighbours, widening
	// only when the scope is empty. The home-ground pass is exempt.
	r := m.economicRules(ctx, b, startBand(b.CharID, defaultBand(b)))

	recent := map[int64]bool{}
	for _, id := range b.State.Data.HuntRecent {
		recent[id] = true
	}
	if cur := b.State.Data.HuntSystemID; cur != 0 {
		recent[cur] = true
	}

	botDPS, botEHP, powerKnown := m.effectivePower(ctx, b)

	// The distress board (beltdistress.go): systems whose belts miners are refused. It outranks
	// the bitten bucket everywhere below. Read once per pick.
	distress := m.distressedBeltSystems(ctx, b.CorpID)

	// weigh reads one candidate: out = worth working at all, engage = clears the pilot's nerve
	// line, bitten = a miner's recent mauling, which is a hunter's work order.
	weigh := func(sysID int64, loBand, hiBand float64) (ok, engage, bitten bool) {
		ms := u.meta[sysID]
		if ms.Security < loBand || ms.Security > hiBand || recent[sysID] {
			return false, false, false
		}
		if dispSec(ms.Security) < r.SecFloor {
			return false, false, false
		}
		// Home ground and corp addresses reach weigh without the walk, so the security ceiling is
		// asked here too. The region scope does not fence a pilot's own addresses.
		if sysID != b.SystemID && r.SecCeil > 0 && dispSec(ms.Security) >= r.SecCeil {
			return false, false, false
		}
		if !u.hasBelts(ctx, m.db, sysID) {
			return false, false, false
		}
		// Belts crewed by this pilot's own employer are belts it will not fire on, so a system full of
		// them is not a hunting ground (threat.go). It only narrows NPC-faction pilots.
		if !m.regionHasHuntableRats(ctx, b, ms.RegionID, ms.Security) {
			return false, false, false
		}
		engage = true
		if powerKnown {
			margin := threatMargin(botDPS, botEHP, m.spawnThreatFor(ctx, ms.RegionID, ms.Security))
			if hopeless(margin) {
				return false, false, false
			}
			engage = wouldEngage(b.Tune, margin)
		}
		return true, engage, recentHostileEncounter(botScope(b), sysID)
	}

	// The home ground: the pilot's own doorstep, then every corp address, nearest first. Bitten
	// systems outrank quiet ones within the list.
	var homeGround []int64
	if hs := m.homeStationFor(ctx, b.CharID); hs != 0 {
		if homeSys := m.stationSystemOf(ctx, hs); homeSys != 0 {
			homeGround = append(homeGround, homeSys)
		}
	}
	if b.CorpID != 0 {
		if anchors, err := m.corpAnchorStations(ctx, b.CorpID); err == nil {
			seen := map[int64]bool{}
			for _, sysID := range homeGround {
				seen[sysID] = true
			}
			reach := gangReachMap(u, b.SystemID, gangRecruitMaxJumps)
			type anchorSys struct {
				sysID int64
				jumps int
			}
			var list []anchorSys
			for _, a := range anchors {
				if seen[a.SystemID] {
					continue
				}
				seen[a.SystemID] = true
				j, ok := reach[a.SystemID]
				if !ok {
					continue // a corp address across the map is not "around home"
				}
				list = append(list, anchorSys{a.SystemID, j})
			}
			sort.Slice(list, func(i, j int) bool { return list[i].jumps < list[j].jumps })
			for _, a := range list {
				homeGround = append(homeGround, a.sysID)
			}
		}
		// A corporation living out of a front or capital patrols the sov its alliance holds around it.
		if front := m.frontOfficeOf(ctx, b.CorpID); front != 0 {
			held := m.allianceHeldSystems(ctx, m.corpAllianceID(ctx, b.CorpID))
			homeGround = appendSovHomeGround(homeGround, held, bfsJumps(u, m.stationSystemOf(ctx, front), sovHomeJumps))
		}
	}
	var homePlain int64
	for _, sysID := range homeGround {
		ok, engage, bitten := weigh(sysID, cfg.SecMin, cfg.SecMax)
		if !ok || !engage {
			continue // "too big around home" falls through to the next address
		}
		if bitten || distress[sysID] {
			if distress[sysID] {
				m.logBot(b.CharID, logInfo, "combat",
					"answering a distress signal — miners are refused %s until its spawn dies",
					m.nameOfSystem(ctx, sysID))
			}
			return sysID, m.nameOfSystem(ctx, sysID), nil
		}
		if homePlain == 0 && !patrolCovered(botScope(b), sysID) {
			homePlain = sysID
		}
	}
	// A freelance kill contract's ground (gmmjobs.go; needs the GMMordu module), with the top of
	// the band opened as in the wider walk.
	if sysID := m.gmmCombatSite(ctx, b, u, func(sysID int64) (bool, bool) {
		ok, engage, _ := weigh(sysID, cfg.SecMin, 1.0)
		return ok, engage
	}); sysID != 0 {
		return sysID, m.nameOfSystem(ctx, sysID), nil
	}
	if homePlain != 0 {
		return homePlain, m.nameOfSystem(ctx, homePlain), nil
	}

	// The wider walk: in scope, safest first then nearest, in band; if nothing is winnable it runs
	// again with the top of the band opened. An empty scope climbs a band at a time (widen).
	for {
		for _, band := range [][2]float64{{cfg.SecMin, cfg.SecMax}, {cfg.SecMin, 1.0}} {
			var cands []huntCand
			seen := map[int64]bool{b.SystemID: true}
			frontier := []int64{b.SystemID}
			for depth := 0; len(frontier) > 0; depth++ {
				for _, sysID := range frontier {
					ok, engage, bitten := weigh(sysID, band[0], band[1])
					if !ok {
						continue
					}
					c := huntCand{
						SysID: sysID,
						Rung:  huntSecRung(u.meta[sysID].Security),
						Depth: depth,
					}
					// What the ground pays, for ground the pilot would take on (huntBountyBand). Cached per
					// region and band.
					if engage {
						ms := u.meta[sysID]
						c.Bounty = m.spawnThreatFor(ctx, ms.RegionID, ms.Security).Bounty
					}
					// Bucket order matters: a distressed system is the loudest work order, but only for a pilot
					// that can win there. It is checked before the taste shunt (fleet-only ones wait, gangform.go).
					switch {
					case distress[sysID]:
						c.Bucket = huntBucketDistress
					case !engage:
						c.Bucket = huntBucketAny // survivable, just not appealing
					case bitten:
						c.Bucket = huntBucketBitten
					case !patrolCovered(botScope(b), sysID):
						c.Bucket = huntBucketPlain
					default:
						continue // quiet AND already under patrol cover
					}
					cands = append(cands, c)
				}
				var next []int64
				for _, sysID := range frontier {
					for _, nb := range u.adj[sysID] {
						if seen[nb] {
							continue
						}
						seen[nb] = true
						// allowed() is the floor, the scope's region line and an outlaw's security ceiling. The scope
						// prunes the walk as the belt search does (findMiningTargetIn).
						if !u.allowed(nb, r) {
							continue
						}
						next = append(next, nb)
					}
				}
				frontier = next
			}
			best, found := pickHuntCand(cands)
			pick := best.SysID
			if found && best.Bucket == huntBucketDistress {
				m.logBot(b.CharID, logInfo, "combat",
					"answering a distress signal — miners are refused %s until its spawn dies",
					m.nameOfSystem(ctx, pick))
			}
			if pick != 0 {
				if powerKnown {
					ms := u.meta[pick]
					th := m.spawnThreatFor(ctx, ms.RegionID, ms.Security)
					m.logBot(b.CharID, logInfo, "combat", "%s reads as %s — %s",
						m.nameOfSystem(ctx, pick), th.describe(),
						marginVerdict(b.Tune, threatMargin(botDPS, botEHP, th)))
				}
				return pick, m.nameOfSystem(ctx, pick), nil
			}
		}
		wider, ok := m.widen(ctx, b, r)
		if !ok {
			break
		}
		if noteScopeFail(b.CharID, r.Band) {
			m.logBot(b.CharID, logInfo, "residency",
				"no hunting ground in %s — looking further out (%s)", m.scopeNote(ctx, r), wider.Band)
		}
		r = wider
	}
	// Say which of the pilot's own rules boxed it in: the security band and the travel floor are
	// different settings on different pages.
	return 0, "", fmt.Errorf(
		"%w in the %.1f–%.1f hunting band (or above it) is reachable that this pilot"+
			" has not just swept and could survive (hard space limit%s)",
		errNoHuntingGround, cfg.SecMin, cfg.SecMax, r.limitNote())
}

// combatStep combs the current system's belts, rotating to a fresh one when the sweep clock
// runs out.
func (m *Mod) combatStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State

	// The two tasks the sweep hands off to (beltloot.go) are part of combat, so they dispatch
	// through the same step.
	switch TaskKind(st.Phase) {
	case TaskCombatLoot:
		return m.combatLootStep(ctx, b)
	case TaskCombatSell:
		return m.combatSellStep(ctx, b)
	}

	if st.Data.HuntSystemID == 0 {
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}

	// A can this sweep produced takes priority over the next belt: it despawns.
	if st.Data.HuntLootID != 0 {
		if handled, err := m.combatResumeLoot(ctx, b); err != nil || handled {
			return false, err
		}
	}

	// Sweep over: sell what it carries, or remember this system and re-plan.
	if clock.Unix()-st.Data.HuntPickedAt >= tacticStint(st.Data.CombatTactic) {
		st.Data.HuntRecent = pushHuntRecent(st.Data.HuntRecent, st.Data.HuntSystemID)
		if rows, err := m.lootLedger(ctx, b.CharID); err == nil && len(rows) > 0 {
			return false, m.combatStartSale(ctx, b, "sweep over")
		}
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}
	// A hold the wrecks have filled ends the stint early, since unwatched kills stop silently once
	// nothing fits. A fleet member keeps the haul aboard (combatStartSale's gate).
	if m.carriesLoot(ctx, b.CharID) {
		if full, err := m.holdMostlyFull(ctx, b); err == nil && full && !m.gangHoldsThisPilot(ctx, b) {
			return false, m.combatStartSale(ctx, b, "the hold is getting full")
		}
	}
	if b.SystemID != st.Data.HuntSystemID {
		return false, m.beginTravel(ctx, b, st.Data.HuntSystemID, 0, st.Phase)
	}
	if b.ShipID == 0 {
		return false, fmt.Errorf("belt hunter has no ship boarded")
	}

	// A gate hold takes the spot once and holds it for the whole stint. The posture keeps the
	// projection there (task.go) and the sim must not move it. Same shape as piracyCampStep.
	if TaskKind(st.Phase) == TaskCombatGate {
		if st.Data.CombatGateID == 0 {
			// A hold with no gate is a pilot parked on nothing: re-plan and draw again.
			st.Activity, st.Phase = "", string(TaskIdle)
			return false, saveState(ctx, m.db, st)
		}
		if !st.Data.CombatCampSet {
			// The arrival side of the gate (lane.go): a hold sunward of the gate covers nothing that
			// jumps in through it.
			m.parkAtGateArrival(ctx, b.ShipID, st.Data.CombatGateID)
			st.Data.CombatCampSet = true
		}
		st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(60)
		return false, saveState(ctx, m.db, st)
	}

	// Nothing here this hull can win and nothing unvisited left (markHuntDry,
	// combat_abstract.go): end the stint now. The re-plan skips a system on HuntRecent.
	if st.Data.HuntDryAt != 0 {
		m.logBot(b.CharID, logInfo, "combat",
			"no belt in %s this hull can win — moving on", m.nameOfSystem(ctx, st.Data.HuntSystemID))
		st.Data.HuntRecent = pushHuntRecent(st.Data.HuntRecent, st.Data.HuntSystemID)
		if rows, err := m.lootLedger(ctx, b.CharID); err == nil && len(rows) > 0 {
			return false, m.combatStartSale(ctx, b, "sweep over")
		}
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}

	// Hop to another belt in the system. Watched, this is a real warp the projection flies;
	// unwatched, it is where the next encounter roll happens.
	m.hopPatrolBelt(ctx, b)
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(
		int64(gangPatrolDwellMinSec+rand.Intn(gangPatrolDwellMaxSec-gangPatrolDwellMinSec+1)))
	return false, saveState(ctx, m.db, st)
}

// pushHuntRecent records a swept system, newest first.
func pushHuntRecent(list []int64, sysID int64) []int64 {
	out := make([]int64, 0, huntRecentMax)
	out = append(out, sysID)
	for _, id := range list {
		if id == sysID {
			continue
		}
		if len(out) == huntRecentMax {
			break
		}
		out = append(out, id)
	}
	return out
}

// huntTargetSummary names what the pilot is out here to shoot, for the log.
func huntTargetSummary(cfg combatConfig) string {
	switch len(cfg.FactionIDs) {
	case 0:
		return "anything that shoots first"
	case 1:
		return "one pirate faction"
	}
	return fmt.Sprintf("%d pirate factions", len(cfg.FactionIDs))
}
