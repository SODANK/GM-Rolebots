package rolebots

import (
	"context"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// On-grid threat assessment for the embodied tier.
//
// The embodied loop runs on a grid a real player watches. This file works out what is on the
// grid, what it is worth against this hull, and whether to stay. It runs on the projection
// goroutine inside the one-second guard loop and is throttled, because the numbers change
// slowly and the cold-cache warm behind it is a database call.

const (
	// How often the sighted-grid look recomputes. The guard loop ticks every second.
	gridAssessEvery = 5 * time.Second
)

// settingThreatGuard switches the on-grid threat arithmetic on and off, so an operator can
// turn this live combat behaviour off without a rebuild.
const settingThreatGuard = "rolebots.threatGuard"

// threatGuard is that switch. Default on; off leaves only the damage-bar disengage rule.
var threatGuard atomic.Bool

func init() { threatGuard.Store(true) }

func threatGuardEnabled() bool { return threatGuard.Load() }

// setThreatGuard is the ESP page's setter.
func setThreatGuard(on bool) { threatGuard.Store(on) }

// loadThreatGuard reads the persisted setting at Init.
func (m *Mod) loadThreatGuard(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingThreatGuard); err == nil && ok {
		threatGuard.Store(v == "1")
	}
}

// settingDefendVsCriminal switches the bystander response to a flagged pilot on and off
// (piracy_prey.go flaggedPilotsOnGrid). Like threatGuard, it changes live-grid behaviour, so
// an operator can turn it off without a rebuild.
const settingDefendVsCriminal = "rolebots.defendVsCriminal"

// defendVsCriminal is that switch. Default on.
var defendVsCriminal atomic.Bool

func init() { defendVsCriminal.Store(true) }

func defendVsCriminalEnabled() bool { return defendVsCriminal.Load() }

// loadDefendVsCriminal reads the persisted setting at Init.
func (m *Mod) loadDefendVsCriminal(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingDefendVsCriminal); err == nil && ok {
		defendVsCriminal.Store(v == "1")
	}
}

// dps is what this loadout puts out per second, cached after the first ask. It is reduced as
// botCombatant reduces it for the abstract tier, so both tiers agree on how hard a ship hits.
func (l *combatLoadout) dps() float64 {
	if l == nil {
		return 0
	}
	if l.dpsCache > 0 {
		return l.dpsCache
	}
	total := 0.0
	for _, w := range l.weapons {
		if w.CycleSec > 0 && w.Dmg != nil {
			total += w.cycleDamage() / w.CycleSec
		}
	}
	total += l.droneDPS()
	l.dpsCache = total
	return total
}

// assessGrid folds everything sighted on the grid into one engage margin against this hull's
// current state. It does no database work (it runs per tick in a fight). known is false when
// the rat-threat cache knows none of the balls present; unknown reads as no worry, as in
// threat.go, so a cold cache does not ground the roster.
func (m *Mod) assessGrid(eb *embodiedBot, l *combatLoadout, sighted []gridHostile) (float64, bool) {
	th, botEHP, known := m.gridThreat(eb, sighted)
	if !known {
		return 0, false
	}
	return threatMargin(l.dps(), botEHP, th), true
}

// gridThreat is the arithmetic under assessGrid: everything sighted folded into one spawn
// threat at its current health, and this hull's current hit points. known is false when
// nothing present is in the rat-threat cache or the hull has nothing left.
func (m *Mod) gridThreat(eb *embodiedBot, sighted []gridHostile) (spawnThreat, float64, bool) {
	if !threatGuardEnabled() || eb == nil || eb.ent == nil || eb.ss == nil || len(sighted) == 0 {
		return spawnThreat{}, 0, false
	}

	var foes []*ratThreat
	for _, h := range sighted {
		e := eb.ss.GetEntity(h.ID)
		if e == nil || e.IsDestroyed() {
			continue
		}
		rt, ok := ratThreatCached(e.Slim.TypeID)
		if !ok {
			continue
		}
		hp := e.HPSnapshot()
		total := hp.MaxShield + hp.MaxArmor + hp.MaxStructure
		frac := 1.0
		if total > 0 {
			frac = (hp.CurShield + hp.CurArmor + hp.CurStructure) / total
		}
		c := *rt
		c.EHP = rt.EHP * frac
		foes = append(foes, &c)
	}
	if len(foes) == 0 {
		return spawnThreat{}, 0, false
	}

	hp := eb.ent.HPSnapshot()
	botEHP := hp.CurShield + hp.CurArmor + hp.CurStructure
	if botEHP <= 0 {
		return spawnThreat{}, 0, false
	}
	return spawnThreatOf(foes), botEHP, true
}

// assessSightedGrid is the look a working bot takes when nothing is shooting at it: rats stand
// on this belt, is it a belt to stand on? The bar is hopeless(), not wouldEngage(): a miner is
// choosing whether to stand somewhere it would lose a fight, not whether to start one.
func (m *Mod) assessSightedGrid(ctx context.Context, eb *embodiedBot, l *combatLoadout, sighted []gridHostile) {
	if !threatGuardEnabled() || eb == nil || eb.patrol || eb.prey || len(sighted) == 0 {
		return
	}
	if eb.ent == nil || eb.ss == nil || eb.ent.IsDestroyed() {
		return
	}

	eb.mu.Lock()
	last := eb.lastAssessAt
	eb.mu.Unlock()
	now := time.Now()
	if now.Sub(last) < gridAssessEvery {
		return
	}
	eb.mu.Lock()
	eb.lastAssessAt = now
	eb.mu.Unlock()

	// A hostile pilot on a belt gets a flat rule, not arithmetic: leave. The rule is gated by
	// workingABelt, because hubs hold rival pilots by design. See docs/comment-notes-b03.md#hub-flee-storm.
	if workingABelt(eb) {
		if id := m.sightedHostilePilot(eb, sighted); id != 0 {
			m.logBot(eb.charID, logInfo, "threat",
				"a hostile pilot is on this grid — leaving the belt to them")
			m.stopEmbodyMining(eb)
			eb.setMineTarget(0)
			m.markBeltBailed(eb)
			// warpDiscretionary: nothing is shooting this ship, so the hop can be reordered freely (see
			// fleeGrid).
			m.fleeGrid(ctx, eb, l, warpDiscretionary)
			notePreFlee(eb)
			return
		}
	}

	// NPCs only from here on: ratThreatFor prices NPC types, and the cache also warms player hulls,
	// which would be summed as rats. See docs/comment-notes-b03.md#hub-flee-storm.
	npcSighted := npcSightings(sighted, func(id uint32) (int64, bool) {
		e := eb.ss.GetEntity(id)
		if e == nil {
			return 0, false
		}
		return e.Slim.CharID, true
	})
	if len(npcSighted) == 0 {
		return
	}

	// Warm the cache for anything new in sight. Without it the first look at a belt is always
	// unknown.
	var types []int64
	for _, h := range npcSighted {
		if e := eb.ss.GetEntity(h.ID); e != nil {
			if _, ok := ratThreatCached(e.Slim.TypeID); !ok {
				types = append(types, e.Slim.TypeID)
			}
		}
	}
	if len(types) > 0 {
		dbCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		m.warmRatThreats(dbCtx, types)
		cancel()
	}

	// The loadout may not be resolved yet, since first contact normally loads it. The look must
	// happen before first contact.
	if l == nil {
		dbCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		shipTypeID := int64(0)
		if eb.ent != nil {
			shipTypeID = eb.ent.Slim.TypeID
		}
		loaded, err := m.loadCombatLoadout(dbCtx, eb.charID, eb.shipID, shipTypeID)
		cancel()
		if err != nil || loaded == nil {
			return
		}
		l = loaded
	}

	th, botEHP, known := m.gridThreat(eb, npcSighted)
	if !known {
		return
	}
	verdict, leave := sightedVerdict(eb.mining, l.dps(), botEHP, th, eb.tune)
	if !leave {
		return
	}
	// A ship that is not here to fight leaves the rats to its gang's guns when they are on
	// the grid or on their way (beltbail.go); if the rats turn on it, shouldBreakOff decides.
	if (eb.mining || l.dps() <= 0) && gangGunOnGrid(eb, &th) {
		return
	}

	m.logBot(eb.charID, logInfo, "threat",
		"%d hostile(s) on this grid and this hull loses that fight (%s) — leaving before they notice",
		len(npcSighted), verdict)
	m.stopEmbodyMining(eb)
	eb.setMineTarget(0)
	m.markBeltBailed(eb)
	// warpDiscretionary: nothing has opened fire. Once something does, shouldBreakOff owns the
	// decision and it is mandatory.
	m.fleeGrid(ctx, eb, l, warpDiscretionary)
	notePreFlee(eb)
}

// sightedVerdict is the look's decision for one hull against what stands on its grid:
// whether to leave, and the words for the log. A miner, or any hull with no guns, is not
// choosing a fight, so it is held to the escape test the unwatched tier uses (minerMargin:
// can it get back off the grid alive); threatMargin would read every armed rat as a lost
// fight and empty every belt the moment a player looked at it. A gunned hull that is not
// mining keeps the fight margin. Pure.
func sightedVerdict(mining bool, botDPS, botEHP float64, th spawnThreat, tune Tuning) (string, bool) {
	if mining || botDPS <= 0 {
		margin := minerMargin(botDPS, botEHP, th)
		return minerVerdict(margin), minerHopeless(margin)
	}
	margin := threatMargin(botDPS, botEHP, th)
	return marginVerdict(tune, margin), hopeless(margin)
}

// warScanEvery is the whole-system war look's cadence.
const warScanEvery = 15 * time.Second

// assessWarInSystem is the war half of assessSightedGrid, one ring wider: a war enemy may
// shoot anywhere, so a working bot leaves the belt when one is in the system. Fighters are
// exempt. It is cheap: relWarEnemiesOf is one snapshot lookup and no war means no system walk.
func (m *Mod) assessWarInSystem(ctx context.Context, eb *embodiedBot, l *combatLoadout) {
	if !threatGuardEnabled() || eb == nil || eb.patrol || eb.prey || eb.engage {
		return
	}
	if eb.ent == nil || eb.ss == nil || eb.ent.IsDestroyed() || !workingABelt(eb) {
		return
	}
	enemies := relWarEnemiesOf(eb.corpID)
	if len(enemies) == 0 {
		return
	}
	now := time.Now()
	eb.mu.Lock()
	if now.Sub(eb.lastWarScanAt) < warScanEvery {
		eb.mu.Unlock()
		return
	}
	eb.lastWarScanAt = now
	eb.mu.Unlock()
	set := make(map[int64]bool, len(enemies))
	for _, id := range enemies {
		set[id] = true
	}
	for _, e := range eb.ss.GetAllPlayerEntities() {
		if e == nil || e.Slim.CharID == 0 || e.Slim.CharID == eb.charID || e.IsDestroyed() {
			continue
		}
		if !set[e.Slim.CorpID] {
			continue
		}
		m.logBot(eb.charID, logInfo, "threat",
			"a pilot of a corporation we are at war with is in the system — leaving the belt")
		m.stopEmbodyMining(eb)
		eb.setMineTarget(0)
		m.markBeltBailed(eb)
		m.fleeGrid(ctx, eb, l, warpDiscretionary)
		notePreFlee(eb)
		return
	}
}

// npcSightings keeps the sightings that are not pilots, the only balls the rat-threat
// arithmetic can price. charOf answers a ball's character ID and whether the ball still exists;
// a ball that is gone is dropped.
func npcSightings(sighted []gridHostile, charOf func(id uint32) (int64, bool)) []gridHostile {
	var out []gridHostile
	for _, h := range sighted {
		charID, ok := charOf(h.ID)
		if !ok || charID != 0 {
			continue
		}
		out = append(out, h)
	}
	return out
}

// workingABelt reports whether this projection is out to mine a belt, the precondition of the
// hostile-pilot rule. Posture must be mining, and then either the lasers are running or the
// destination is a belt (groupID 9, as parkAtBelt and the ore-dry sweep test).
func workingABelt(eb *embodiedBot) bool {
	if eb == nil || !eb.mining {
		return false
	}
	eb.mu.Lock()
	live := eb.mineCancel != nil || eb.mineTarget != 0
	dest := eb.currentDest
	eb.mu.Unlock()
	if live {
		return true
	}
	if dest == 0 {
		return false
	}
	for _, c := range eb.celestials {
		if c.ItemID == dest {
			return c.GroupID == 9
		}
	}
	return false
}

// sightedHostilePilot is the first sighted player ship this bot's employer is at odds with, 0
// if none. It re-asks gridStandingToPilot because hostilesOnGrid also reports aggressors of
// any standing; anything actually shooting belongs to shouldBreakOff.
func (m *Mod) sightedHostilePilot(eb *embodiedBot, sighted []gridHostile) uint32 {
	if eb == nil || eb.ss == nil {
		return 0
	}
	for _, h := range sighted {
		e := eb.ss.GetEntity(h.ID)
		if e == nil || e.Slim.CharID == 0 || e.Slim.CharID == eb.charID {
			continue
		}
		if m.gridKin(eb, e) {
			continue
		}
		if m.gridStandingToPilot(eb, e) <= StandingDisliked {
			return h.ID
		}
	}
	return 0
}

// markBeltBailed puts the belt the bot is standing on out of bounds for a while: in the
// ore-dry set the current mining session reads, and in the bail memory that outlives it and
// calls the gang's guns (noteBeltBail). The system is noted as hostile either way.
func (m *Mod) markBeltBailed(eb *embodiedBot) {
	if eb == nil {
		return
	}
	if beltID := noteBeltBail(eb); beltID != 0 {
		eb.mu.Lock()
		if eb.oreDryBelts == nil {
			eb.oreDryBelts = map[int64]bool{}
		}
		eb.oreDryBelts[beltID] = true
		eb.mu.Unlock()
	}
	noteHostileEncounter(ebScope(eb), eb.systemID)
}

// combatTargetScore ranks one candidate target; lower wins. The score is the target's own
// output divided by how long it takes to kill, so a huge-health commander scores badly. With no
// threat-cache opinion it falls back to health-then-distance ordering.
func combatTargetScore(botDPS, healthFrac, dist, targetEHP, targetDPS float64) float64 {
	base := healthFrac*1e6 + dist
	if botDPS <= 0 || targetEHP <= 0 || targetDPS <= 0 {
		return base
	}
	timeToKill := targetEHP / botDPS
	if timeToKill <= 0 {
		return base
	}
	value := targetDPS / timeToKill // incoming DPS removed per second spent
	if value <= 0 {
		return base
	}
	// Divide rather than subtract so the term stays a preference over the health-and-distance
	// ordering.
	return base / (1 + value)
}
