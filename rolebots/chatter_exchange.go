package rolebots

import (
	"context"
	"math"
	"math/rand"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// Exchanges in Local: who says what when a fight starts, lands and ends. Each trigger
// builds a short list of beats and hands it to runExchange, which says them on its own
// goroutine with seconds between. Triggers run where blocking is forbidden (the combat
// goroutine, a sim pass), so they never query the database or sleep; the beats do.

const (
	beatOpen     = 600 * time.Millisecond // the aggressor, as the point lands
	beatReply    = 4 * time.Second        // the victim answers
	beatComeback = 4 * time.Second        // the aggressor has the last word
	beatAside    = 7 * time.Second        // a bystander, after the dust settles
)

// racialRewrite turns an opinion decided by another layer into a race line now and then,
// when the speaker's prejudice is strong enough to say so: a Caldari xenophobe at war with
// a Gallente corporation still says what it thinks of the Federation.
func racialRewrite(read motiveRead, phobia float64) motive {
	switch read.Why {
	case motiveRace, motiveBloodline, motiveDefense, motivePiracy:
		return read.Why
	}
	if read.Racial && xenoPhobeStage(phobia) >= xenoStageOwnRace && rand.Intn(100) < 40 {
		return motiveRace
	}
	return read.Why
}

// nearestCelestialName is the closest named celestial to a point, for the {where} slot
// ("Rens VI - Asteroid Belt 1"). Empty off grid.
func nearestCelestialName(cels []evedb.CelestialEntity, pos destiny.Vec3) string {
	best, bestD := "", math.MaxFloat64
	for _, c := range cels {
		if c.Name == "" {
			continue
		}
		dx, dy, dz := c.X-pos.X, c.Y-pos.Y, c.Z-pos.Z
		if d := dx*dx + dy*dy + dz*dz; d < bestD {
			best, bestD = c.Name, d
		}
	}
	return best
}

// localBystander is a bot in a system's Local that is neither party, or nil.
func (m *Mod) localBystander(sysID int64, exclude ...int64) *chatParty {
	if m.lsc == nil {
		return nil
	}
	skip := map[int64]bool{}
	for _, id := range exclude {
		skip[id] = true
	}
	var pool []*chatParty
	for _, mem := range m.lsc.Members(sysID) {
		if mem.Conn != nil || mem.CharID == 0 || skip[mem.CharID] {
			continue
		}
		p := partyFromID(mem.CharID, mem.CharName, mem.CorpID, 0, true)
		pool = append(pool, p)
		if len(pool) >= 32 {
			break
		}
	}
	if len(pool) == 0 {
		return nil
	}
	return pool[rand.Intn(len(pool))]
}

// ── the embodied tier ───────────────────────────────────────────────────────

// chatterEngageEmbodied is the first lock of a projection on a pilot. Called
// on the combat goroutine: map reads and a goroutine, nothing else.
func (m *Mod) chatterEngageEmbodied(eb *embodiedBot, tgt *destiny.Entity, aggressors []uint32) {
	if !chatterLocalOn.Load() || eb == nil || tgt == nil || !tgt.IsPlayer || tgt.Slim.CharID == 0 {
		return
	}
	sysID := eb.systemID
	if !m.hearable(sysID, false) {
		return
	}
	victimID := tgt.Slim.CharID
	if !claimExchange(eb.charID, victimID) {
		return
	}
	// A fleet speaks once per target: eight pilots locking one ship is one line. The gang key
	// is negated so it cannot collide with a character ID (as in roe.go's decider).
	if eb.gangID != 0 && !claimExchange(-eb.gangID, victimID) {
		return
	}
	read := m.engageMotive(eb, tgt, aggressors)
	// A robbery speaks for itself: the demand, or the execution line, goes
	// out from piracy_prey.go once the point holds (pirateSay).
	if read.Why == motivePiracy {
		return
	}
	aggr := partyFromEmbodied(eb)
	why := racialRewrite(read, aggr.Phobia)

	var victim *chatParty
	var victimRead motiveRead
	if vb := liveProjection(victimID); vb != nil {
		victim = partyFromEmbodied(vb)
		if eb.ent != nil {
			victimRead = m.gridMotiveToPilot(vb, eb.ent)
		}
	} else {
		victim = partyFromID(victimID, tgt.CharName, tgt.Slim.CorpID, tgt.Slim.TypeID, false)
	}
	victim.ShipTypeID = tgt.Slim.TypeID
	lastMotive.note(eb.charID, victimID, why)
	where := ""
	if eb.ent != nil {
		where = nearestCelestialName(eb.celestials, eb.ent.Pos())
	}
	m.openEngageExchange(sysID, aggr, victim, why, victimRead, where)
}

// openEngageExchange is the shared shape of an engagement in Local, whichever
// tier found it: the aggressor's line, the victim's answer if it is a bot, a
// comeback, and sometimes a bystander. A human victim instead opens a bark
// window, so their own reply is heard (chatter_retort.go).
func (m *Mod) openEngageExchange(sysID int64, aggr, victim *chatParty, why motive, victimRead motiveRead, where string) {
	extra := chatSlots{}
	if where != "" {
		extra["where"] = where
	}
	// What the victim answers is keyed by what it was accused of, unless it
	// has a prejudice of its own toward the aggressor to answer with.
	replyWhy := why
	if victim.Bot && victimRead.Racial && xenoPhobeStage(victim.Phobia) >= xenoStageOwnRace {
		replyWhy = motiveRace
	}
	beats := []beat{
		{after: beatOpen, speaker: aggr, about: victim, ev: evEngage, why: why, extra: extra, urgent: true},
	}
	if victim.Bot {
		beats = append(beats,
			beat{after: beatReply, speaker: victim, about: aggr, ev: evVictim, why: replyWhy, extra: extra,
				urgent: true, odds: int(chatterReplyPct.Load())},
			beat{after: beatComeback, speaker: aggr, about: victim, ev: evComeback, why: why, extra: extra,
				odds: 45},
		)
	} else {
		noteBark(sysID, aggr, victim, why, extra)
	}
	if chance(&chatterBystander) {
		if by := m.localBystander(sysID, aggr.CharID, victim.CharID); by != nil {
			aside := chatSlots{"attacker": aggr.link(), "victim": victim.link()}
			for k, v := range extra {
				aside[k] = v
			}
			beats = append(beats, beat{after: beatAside, speaker: by, about: victim, ev: evBystander,
				why: why, extra: aside, optional: true})
		}
	}
	m.runExchange("engage", sysID, false, beats)
	// A corporation hears its own member call for help.
	if victim.Bot {
		m.chatterCorpHelp(victim, aggr, sysID, where)
	}
}

// chatterUnderFire is a bot hit by somebody it has not exchanged words with: the aggressor
// said nothing (a human, a silent pirate, a missile from off grid). Called from the combat
// goroutine when a new pilot appears in the hit log.
func (m *Mod) chatterUnderFire(eb *embodiedBot, attacker *destiny.Entity) {
	if !chatterLocalOn.Load() || eb == nil || attacker == nil || attacker.Slim.CharID == 0 {
		return
	}
	sysID := eb.systemID
	if !m.hearable(sysID, false) {
		return
	}
	aID := attacker.Slim.CharID
	// An exchange the attacker opened (or this pair already had) covers it.
	if !claimExchange(aID, eb.charID) {
		return
	}
	self := partyFromEmbodied(eb)
	var them *chatParty
	if lp := liveProjection(aID); lp != nil {
		them = partyFromEmbodied(lp)
	} else {
		them = partyFromID(aID, attacker.CharName, attacker.Slim.CorpID, attacker.Slim.TypeID, false)
	}
	them.ShipTypeID = attacker.Slim.TypeID
	why := motiveNone
	if r := m.gridMotiveToPilot(eb, attacker); r.Racial && xenoPhobeStage(self.Phobia) >= xenoStageOwnRace {
		why = motiveRace
	}
	where := ""
	if eb.ent != nil {
		where = nearestCelestialName(eb.celestials, eb.ent.Pos())
	}
	extra := chatSlots{}
	if where != "" {
		extra["where"] = where
	}
	m.runExchange("underfire", sysID, false, []beat{
		{after: 2 * time.Second, speaker: self, about: them, ev: evUnderFire, why: why, extra: extra, urgent: true,
			odds: int(chatterReplyPct.Load())},
	})
	m.chatterCorpHelp(self, them, sysID, where)
}

// chatterKill is a ship (not a pod) lost to a known killer. killer and victim
// are whatever parties the death path could build; a human on either side is
// simply not spoken for.
func (m *Mod) chatterKill(sysID int64, killer, victim *chatParty, why motive) {
	// The killer's alliance hears of it whether or not anybody is in Local.
	m.chatterAllyKill(killer, victim)
	if !chatterLocalOn.Load() || killer == nil || victim == nil || !m.hearable(sysID, false) {
		return
	}
	// A racist's gloat is answered in kind, or with a dignified gf. A fight the loser's ledger read
	// as fair (grace_hooks.go) is a gf with nothing behind it.
	lament := evLament
	if graceGoodFightBetween(killer.CharID, victim.CharID) {
		lament = evGoodFight
	} else if why == motiveRace || why == motiveBloodline {
		if rand.Intn(2) == 0 {
			lament = evSpite
		}
	}
	m.runExchange("kill", sysID, false, []beat{
		{after: 1500 * time.Millisecond, speaker: killer, about: victim, ev: evGloat, why: why, urgent: true,
			odds: 75, optional: true},
		{after: 5 * time.Second, speaker: victim, about: killer, ev: lament, why: why, urgent: true,
			odds: int(chatterReplyPct.Load()), optional: true},
	})
	// The victim's corporation hears of it from the killmail (killboard.go
	// pers.loss), and a corpmate answers there (chatterCorpConsole).
}

// lastMotive remembers the motive a pair's exchange was opened with, so the kill line that
// follows a race bark is a race gloat.
var lastMotive = newMotiveMemo()

type motiveMemo struct {
	at  map[[2]int64]time.Time
	why map[[2]int64]motive
}

func newMotiveMemo() *motiveMemo {
	return &motiveMemo{at: map[[2]int64]time.Time{}, why: map[[2]int64]motive{}}
}

func (mm *motiveMemo) note(a, b int64, why motive) {
	exchangeMu.Lock()
	defer exchangeMu.Unlock()
	k := [2]int64{a, b}
	mm.at[k], mm.why[k] = clock.Now(), why
	if len(mm.at) > 4096 {
		for k, t := range mm.at {
			if clock.Now().Sub(t) > exchangeCooldown {
				delete(mm.at, k)
				delete(mm.why, k)
			}
		}
	}
}

func (mm *motiveMemo) sweep(now time.Time) {
	exchangeMu.Lock()
	defer exchangeMu.Unlock()
	for k, t := range mm.at {
		if now.Sub(t) > exchangeCooldown {
			delete(mm.at, k)
			delete(mm.why, k)
		}
	}
}

func (mm *motiveMemo) get(a, b int64) motive {
	exchangeMu.Lock()
	defer exchangeMu.Unlock()
	k := [2]int64{a, b}
	if t, ok := mm.at[k]; ok && clock.Now().Sub(t) < exchangeCooldown {
		return mm.why[k]
	}
	return motiveNone
}

// ── the abstract tier ───────────────────────────────────────────────────────

// chatterEngageAbstract is an engagement nobody watches on grid; a human in another belt
// of the same system is in Local and would otherwise never know of it.
func (m *Mod) chatterEngageAbstract(ctx context.Context, lb, fb *botRun, sysID int64, why motive) {
	if !chatterLocalOn.Load() || lb == nil || fb == nil || !m.hearable(sysID, false) {
		return
	}
	if !claimExchange(lb.CharID, fb.CharID) {
		return
	}
	aggr := partyFromRun(lb, why == motivePiracy)
	victim := partyFromRun(fb, false)
	aggr.ShipTypeID = m.activeShipType(ctx, lb.ShipID)
	victim.ShipTypeID = m.activeShipType(ctx, fb.ShipID)
	read := motiveRead{Why: why}
	if why == motiveRace || why == motiveFaction || why == motiveCorpRival || why == motiveWar {
		read.Racial = xenoStanding(aggr.RaceID, aggr.Phobia, victim.RaceID) <= StandingDisliked
	}
	why = racialRewrite(read, aggr.Phobia)
	victimRead := m.standingMotiveToPilot(ctx, fb.standingSelf(), lb.CharID)
	lastMotive.note(lb.CharID, fb.CharID, why)
	m.openEngageExchange(sysID, aggr, victim, why, victimRead, "")
}

// activeShipType is a ship entity's typeID, 0 when unknown.
func (m *Mod) activeShipType(ctx context.Context, shipID int64) int64 {
	if shipID == 0 {
		return 0
	}
	var t int64
	_ = m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, shipID).Scan(&t)
	return t
}

// abstractMotive names why the abstract tier's rules let a patrol shoot a candidate, in the
// order the target would want it explained.
func abstractMotive(read motiveRead, criminal, outlaw, atWar bool, bounty float64) motive {
	switch {
	case criminal:
		return motiveCriminal
	case outlaw:
		return motiveOutlaw
	case atWar:
		return motiveWar
	case read.Standing <= StandingDisliked && read.Why != motiveNone:
		return read.Why
	case bounty > 0:
		return motiveBounty
	}
	return motiveNerve
}

// chatterOwnLoss is a projection's own hull going down (pod.go handleShipLoss). A bot
// killer has already spoken from its landDamage; this covers a human killer. Death to rats
// is only its corporation's business, told by the killmail (pers.loss).
func (m *Mod) chatterOwnLoss(eb *embodiedBot, hits []destiny.KillHit) {
	if eb == nil {
		return
	}
	victim := partyFromEmbodied(eb)
	victim.Alive = nil
	killer, ok := topPlayerHit(hits, eb.charID)
	if !ok {
		return
	}
	if m.isSynthetic(killer.CharID) {
		return
	}
	kp := partyFromID(killer.CharID, killer.Name, killer.CorpID, killer.ShipTypeID, false)
	m.chatterKill(eb.systemID, kp, victim, lastMotive.get(killer.CharID, eb.charID))
}
