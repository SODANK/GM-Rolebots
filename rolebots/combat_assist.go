package rolebots

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/npc"
	"gurimoon/seclaw"
)

// Coming to a friend's aid: the embodied half of assistAllows (roe.go). A corpmate, an
// alliance mate or a blue shot on the same grid is helped two ways from one evidence source.
//   - Fighting. An attacker of a friend joins hostilesOnGrid as aggro, behind the bot's own
//     attackers, when assistAllows says the law and the employer allow it: a duty for corp
//     and alliance mates, a sticky roll for blues.
//   - Healing. A friend under fire joins the remote repairers' candidate list (runRepCycles),
//     with no roll and no doctrine gate. A flagged friend is never mended (seclaw.AidIsCrime).
// The evidence is the kill-report ledger every damage path writes on the victim (also read
// by fleetAggressors), scanned once per grid per second and shared by every projection.

// ── settings ────────────────────────────────────────────────────────────────

// settingAssist switches both halves on and off, so an operator can turn it off without a
// rebuild (as settingDefendVsCriminal does, combat_threat.go).
const settingAssist = "rolebots.combat.assist"

var assistOn atomic.Bool

func init() { assistOn.Store(true) }

func assistEnabled() bool { return assistOn.Load() }

func (m *Mod) loadAssist(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingAssist); err == nil && ok {
		assistOn.Store(v == "1")
	}
}

// settingAssistBlue is the odds a pilot (or a fleet, rolled once by its
// commander's key) joins a BLUE's fight. Corp and alliance mates are a duty
// and never rolled.
const settingAssistBlue = "rolebots.combat.assistBlueChance"

const assistBlueChanceDefault = 0.5

// assistBlueV caches it in hundredths, the settingPodKill convention: 0 is
// unset, an explicit 0 stores -1.
var assistBlueV atomic.Int64

func assistBlueChance() float64 {
	switch v := assistBlueV.Load(); {
	case v < 0:
		return 0
	case v > 0:
		return float64(v) / 100
	}
	return assistBlueChanceDefault
}

func (m *Mod) loadAssistBlueChance(ctx context.Context) {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingAssistBlue)
	if err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingAssistBlue, err)
		return
	}
	if !ok {
		return
	}
	f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if perr != nil || f < 0 {
		return
	}
	if f = clampRange(f, 0, 1); f == 0 {
		assistBlueV.Store(-1)
		return
	}
	assistBlueV.Store(int64(f * 100))
}

// settingTransitWar is the base odds a pilot in transit takes on a war target it meets
// (warTargetsOnGrid), before its nerve scales it (transitWarOdds).
const settingTransitWar = "rolebots.combat.transitWarChance"

const transitWarChanceDefault = 0.5

// transitWarV caches it in hundredths, the assistBlueV convention: 0 is unset, an explicit 0 stores -1.
var transitWarV atomic.Int64

func transitWarChance() float64 {
	switch v := transitWarV.Load(); {
	case v < 0:
		return 0
	case v > 0:
		return float64(v) / 100
	}
	return transitWarChanceDefault
}

func (m *Mod) loadTransitWarChance(ctx context.Context) {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingTransitWar)
	if err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingTransitWar, err)
		return
	}
	if !ok {
		return
	}
	f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if perr != nil || f < 0 {
		return
	}
	if f = clampRange(f, 0, 1); f == 0 {
		transitWarV.Store(-1)
		return
	}
	transitWarV.Store(int64(f * 100))
}

// ── who is being shot ───────────────────────────────────────────────────────

// gridVictimTTL is how long one scan of a system's hit ledgers is shared.
// Every projection on a grid asks every combat tick; without the cache a hub
// copied every pilot's ledger once per projection per tick.
const gridVictimTTL = time.Second

type victimHits struct {
	victim *destiny.Entity
	hits   []destiny.KillHit
}

type gridVictimEntry struct {
	at      time.Time
	victims []victimHits
}

// gridVictimCache is solar-system ID → *gridVictimEntry.
var gridVictimCache sync.Map

// gridVictimHits is every pilot ball in the system that has been hit inside
// combatAggroMemorySec, with those hits. Pilots only: a friend is a pilot.
func gridVictimHits(ss *destiny.SolarSystem) []victimHits {
	if ss == nil {
		return nil
	}
	now := time.Now()
	if v, ok := gridVictimCache.Load(ss.ID); ok {
		if e := v.(*gridVictimEntry); now.Sub(e.at) < gridVictimTTL {
			return e.victims
		}
	}
	cutoff := now.Unix() - combatAggroMemorySec
	var out []victimHits
	for _, e := range ss.GetAllPlayerEntities() {
		if e == nil || e.IsDestroyed() {
			continue
		}
		var recent []destiny.KillHit
		for _, h := range e.KillHitsSnapshot() {
			if h.Time >= cutoff {
				recent = append(recent, h)
			}
		}
		if len(recent) > 0 {
			out = append(out, victimHits{victim: e, hits: recent})
		}
	}
	gridVictimCache.Store(ss.ID, &gridVictimEntry{at: now, victims: out})
	return out
}

// ── who is a friend ─────────────────────────────────────────────────────────

// assistRelation is what victim is to this projection. Grid-only, no queries. A capsule is
// not defended, and a flagged friend is not a friend here: shooting a criminal is lawful
// punishment, and joining against that pilot or mending the criminal is itself a crime.
func (m *Mod) assistRelation(eb *embodiedBot, v *destiny.Entity) assistRel {
	if eb == nil || eb.ent == nil || v == nil || v.ID == eb.ent.ID || !v.IsPlayer {
		return assistNone
	}
	if v.Slim.CharID == 0 || v.Slim.CharID == eb.charID || v.Slim.TypeID == capsuleTypeID {
		return assistNone
	}
	if v.IsDestroyed() || m.pilotIsFlagged(eb, v) {
		return assistNone
	}
	if eb.corpID != 0 && v.Slim.CorpID == eb.corpID {
		return assistDuty
	}
	ally := eb.ent.Slim.AllianceID
	if ally != 0 && v.Slim.AllianceID == ally {
		return assistDuty
	}
	if relDeclaredBlue(eb.corpID, ally, v.Slim.CorpID, v.Slim.AllianceID) {
		return assistBlue
	}
	return assistNone
}

// gridIDs is the set of balls on this projection's own grid.
func gridIDs(eb *embodiedBot) map[uint32]bool {
	out := map[uint32]bool{}
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		out[e.ID] = true
	}
	return out
}

// ── fighting ────────────────────────────────────────────────────────────────

// assistPair is one attacker this projection will take on, and the friend it
// is taking it on for (named for the log).
type assistPair struct {
	attacker uint32
	victim   string
}

// assistPairs is every attacker of a friend on this grid that this projection
// may open fire on, each once.
func (m *Mod) assistPairs(eb *embodiedBot) []assistPair {
	if !assistEnabled() || eb == nil || eb.ent == nil || eb.ss == nil || eb.doctrine == DoctrineNone {
		return nil
	}
	// A pacifist does not join the fight; its repairers still answer it (assistHealTargets).
	if !takesElectiveFights(eb.tune) {
		return nil
	}
	victims := gridVictimHits(eb.ss)
	if len(victims) == 0 {
		return nil
	}
	grid := gridIDs(eb)
	seen := map[uint32]bool{}
	var out []assistPair
	for _, vh := range victims {
		if !grid[vh.victim.ID] {
			continue
		}
		rel := m.assistRelation(eb, vh.victim)
		if rel == assistNone {
			continue
		}
		for _, h := range vh.hits {
			id, ok := eb.attackerEntityID(h)
			if !ok || id == 0 || id == eb.ent.ID || seen[id] {
				continue
			}
			if !m.assistMayShoot(eb, eb.ss.GetEntity(id), rel) {
				continue
			}
			seen[id] = true
			name := vh.victim.CharName
			if name == "" {
				name = "a friend"
			}
			out = append(out, assistPair{attacker: id, victim: name})
		}
	}
	return out
}

// assistAggressors is assistPairs as the ID list hostilesOnGrid wants.
func (m *Mod) assistAggressors(eb *embodiedBot) []uint32 {
	pairs := m.assistPairs(eb)
	if len(pairs) == 0 {
		return nil
	}
	out := make([]uint32, len(pairs))
	for i, p := range pairs {
		out[i] = p.attacker
	}
	return out
}

// assistMayShoot gathers the facts about one attacker and asks assistAllows,
// rolling for a blue.
func (m *Mod) assistMayShoot(eb *embodiedBot, a *destiny.Entity, rel assistRel) bool {
	if a == nil || a.ID == eb.ent.ID || a.IsDestroyed() || a.IsInWarp() || a.IsCloaked() {
		return false
	}
	isNPC := !a.IsPlayer
	var criminal, outlaw, atWar bool
	if isNPC {
		// A managed rat only: drones, structures and the rest of what is not
		// a pilot are not somebody to go to war with over a friend.
		if m.npc == nil || !m.npc.IsNPC(a.ID) {
			return false
		}
		if npc.IsInertStructure(a.Slim.GroupID) || lawEnforcementGroup(a.Slim.GroupID) {
			return false
		}
		// The employer's own rats are left to the friend who provoked them,
		// the same rule hostilesOnGrid applies to its own grid (faction.go).
		if !m.gridNPCHostileTo(eb, a) {
			return false
		}
	} else {
		// Corpmates, kin, gangmates and unmarked pods are vetoed here, so a
		// spat between two of this pilot's own is never "assisted".
		if !m.mayTargetPilot(eb, a) {
			return false
		}
		ally := eb.ent.Slim.AllianceID
		// Nor between two friends: a blue shooting a corpmate is somebody
		// else's quarrel to settle.
		if relDeclaredBlue(eb.corpID, ally, a.Slim.CorpID, a.Slim.AllianceID) {
			return false
		}
		criminal = criminalSince(eb.systemID, a.Slim.CharID) > 0
		outlaw = a.Slim.SecurityStatus <= seclaw.OutlawStatus
		if warEverywhere() {
			atWar = relAtWar(eb.corpID, ally, a.Slim.CorpID, a.Slim.AllianceID)
		}
	}
	band := secBand(loadedSystemSecurity(eb.systemID))
	allow, roll, _ := assistAllows(rel, eb.doctrine, band, isNPC, criminal, outlaw, atWar)
	if !roll {
		return allow
	}
	decider := eb.charID
	if eb.gangID != 0 {
		decider = -eb.gangID
	}
	key := a.Slim.CharID
	if isNPC || key == 0 {
		// Negated so an NPC entity ID cannot collide with a character ID.
		key = -int64(a.ID)
	}
	return assistNerveRoll(decider, key, assistBlueChance())
}

// electiveOnly reports whether every aggro hostile in hs is one this pilot chose to take on (a
// friend's attacker or a war target met in transit): nothing is fighting this projection itself.
func electiveOnly(hs []gridHostile) bool {
	found := false
	for _, h := range hs {
		if !h.Aggro {
			continue
		}
		if !h.Elective {
			return false
		}
		found = true
	}
	return found
}

// assistOnly reports whether every aggro hostile in hs came only from a friend's fight:
// nothing is shooting this projection itself.
func assistOnly(hs []gridHostile) bool {
	found := false
	for _, h := range hs {
		if !h.Aggro {
			continue
		}
		if !h.Assist {
			return false
		}
		found = true
	}
	return found
}

// ── healing ─────────────────────────────────────────────────────────────────

// assistHealTargets is every friend under fire on this grid, what a remote repairer may
// mend beyond its own fleet. No doctrine gate and no roll.
func (m *Mod) assistHealTargets(eb *embodiedBot) []*destiny.Entity {
	if !assistEnabled() || eb == nil || eb.ent == nil || eb.ss == nil {
		return nil
	}
	victims := gridVictimHits(eb.ss)
	if len(victims) == 0 {
		return nil
	}
	grid := gridIDs(eb)
	var out []*destiny.Entity
	for _, vh := range victims {
		if grid[vh.victim.ID] && m.assistRelation(eb, vh.victim) != assistNone {
			out = append(out, vh.victim)
		}
	}
	return out
}

// assistApproachReach is how far past its repairer's range an idle logi flies to reach a
// friend under fire; assistApproachHold is where it stops, as a fraction of that range.
const (
	assistApproachReach = 2.0
	assistApproachHold  = 0.7
)

// assistHealApproach flies an idle projection carrying a remote repairer into range of a
// wounded friend just out of reach. Only an uncommitted pilot moves (not warping, burning a
// leg, mining, holding a spot or flying with a fleet), so it never fights the nav loop.
// Everyone else mends what is in reach.
func (m *Mod) assistHealApproach(eb *embodiedBot, l *combatLoadout) {
	if l == nil || eb == nil || eb.ent == nil || eb.ss == nil {
		return
	}
	var rep *botRepModule
	for _, r := range l.reps {
		if r.Remote && r.Range > 0 && r.Kind != repCap {
			rep = r
			break
		}
	}
	if rep == nil {
		return
	}
	eb.mu.Lock()
	busy := eb.warping || eb.approachID != 0 || eb.mineTarget != 0 || eb.holding ||
		eb.gangID != 0 || eb.bossID != 0 || eb.fighting
	eb.mu.Unlock()
	if busy {
		return
	}
	friends := m.assistHealTargets(eb)
	if len(friends) == 0 {
		return
	}
	selfPos := eb.ent.Pos()
	var best *destiny.Entity
	bestDist := rep.Range * assistApproachReach
	for _, f := range friends {
		if f.IsInWarp() || f.IsCloaked() || layerFrac(f, rep.Kind) >= remoteRepWoundedFrac {
			continue
		}
		d := f.Pos().Sub(selfPos).Len()
		if d <= rep.Range {
			return // somebody wounded is already in reach; the cycle mends them
		}
		if d < bestDist {
			best, bestDist = f, d
		}
	}
	if best == nil {
		return
	}
	m.embodyApproach(eb, best.ID, rep.Range*assistApproachHold, 0)
}
