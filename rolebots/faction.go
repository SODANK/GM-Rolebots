package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// Who a bot may shoot. The core refuses to cold-aggro a pilot whose corporation shares a rat's
// corporation or faction (npc/manager.go isPlayerHostile), and one hit on a rat turns the whole
// belt hostile (shareFactionAggro). So the module asks the core's question, StandingsMap.IsHostile:
//
//	same faction             friendly
//	either side factionless  hostile
//	standing < 0             hostile
//
// The predicate takes a matrix so a test can check it without a database or NPCManager. A bot's
// side is its employer's faction or a player corporation's allegiance (allegiance.go), one field.

// factionHostile mirrors npc/standings.go StandingsMap.IsHostile. It is not built on blocStanding
// (corps.go): that answers 0 for a zero faction, 0 is not < 0, and the second rule would invert,
// so every player-corp bot would be friends with every rat.
func factionHostile(mx map[[2]int64]float64, from, to int64) bool {
	if from == to {
		return false
	}
	if from == 0 || to == 0 {
		return true
	}
	return mx[[2]int64{from, to}] < 0
}

// factionAtOdds is factionHostile read from both sides: at odds when either
// faction's row on the other is below 0. The employer filters use it because
// the matrix is one-sided for pirates (Caldari on Guristas -9, no reverse row)
// while a core rat of a different faction aggroes regardless, so the one-sided
// read would leave a pirate's employees declining sites whose rats shoot them.
func factionAtOdds(mx map[[2]int64]float64, from, to int64) bool {
	return factionHostile(mx, from, to) || mx[[2]int64{to, from}] < 0
}

// filterHostileRats keeps only the rats this pilot's employer is actually at
// odds with. `mine` is the bot's employment faction; 0 (a player corp with no
// allegiance, or an NPC corp with no faction) returns the spawn untouched and
// allocates nothing, which is the common case.
func filterHostileRats(mx map[[2]int64]float64, mine int64, spawn []*ratThreat) []*ratThreat {
	if mine == 0 || len(spawn) == 0 {
		return spawn
	}
	var out []*ratThreat
	for _, r := range spawn {
		if r == nil {
			continue
		}
		if factionAtOdds(mx, mine, r.FactionID) {
			out = append(out, r)
		}
	}
	return out
}

// huntableFactions is the bounty target list: the pirate spawn factions, minus the pilot's own
// employer and anyone that employer is on terms with, minus anyone the pilot personally likes.
// The personality half alone cannot discriminate: an outlaw personality is warm toward every
// pirate faction at once (outlook.go) until it adopts its employer's outlook.
func huntableFactions(mx map[[2]int64]float64, mine int64, pirates []int64, p *Profile) []int64 {
	var out []int64
	for _, id := range pirates {
		if mine != 0 && !factionAtOdds(mx, mine, id) {
			continue
		}
		if p != nil && p.StandingTo(id) >= StandingLiked {
			continue
		}
		out = append(out, id)
	}
	return out
}

// siteFactionHuntable is whether a pilot employed by `mine` may take an open site whose rats fly
// for `fid`. Factionless: hostile to everybody. Pirate faction: huntableFactions decides. A faction
// the matrix lacks: fair game for everybody (it has no side to turn, unlike factionHostile, which
// reads a missing pair as 0 and spares). Any other: the employer's standing, then the pilot's.
func siteFactionHuntable(mx map[[2]int64]float64, mine, fid int64, pirates, hunt []int64, p *Profile) bool {
	if fid == 0 {
		return true
	}
	for _, id := range pirates {
		if id != fid {
			continue
		}
		if len(hunt) == 0 {
			return true
		}
		for _, h := range hunt {
			if h == fid {
				return true
			}
		}
		return false
	}
	if !matrixKnowsFaction(mx, fid) {
		return true
	}
	if mine != 0 && !factionAtOdds(mx, mine, fid) {
		return false
	}
	return p == nil || p.StandingTo(fid) < StandingLiked
}

// matrixKnowsFaction is whether the static standings name `fid` at all.
func matrixKnowsFaction(mx map[[2]int64]float64, fid int64) bool {
	for k := range mx {
		if k[0] == fid || k[1] == fid {
			return true
		}
	}
	return false
}

// ── the Mod's side: sourcing the matrix and the corp→faction map ────────────

var (
	standingsMxMu     sync.Mutex
	standingsMx       map[[2]int64]float64
	standingsMxLoaded bool

	corpFactionMu    sync.Mutex
	corpFactionCache = map[int64]corpFactionEntry{}
)

// corpFactionEntry is both halves of a corporation's faction: the NPC row and
// the declared allegiance (allegiance.go). At most one is nonzero.
type corpFactionEntry struct{ npc, allegiance int64 }

// standingsMatrix is the static faction-to-faction table, loaded once; the bool is whether it is
// trustworthy. A failed load is not cached as an empty matrix, which would make no pair hostile.
// Every caller treats !ok as hostile, so a failure never leaves a roster that refuses to shoot.
func (m *Mod) standingsMatrix(ctx context.Context) (map[[2]int64]float64, bool) {
	standingsMxMu.Lock()
	defer standingsMxMu.Unlock()
	if standingsMxLoaded {
		return standingsMx, true
	}
	mx, err := factionStandings(ctx, m.db)
	if err != nil {
		rlogf("rolebots: faction standings unavailable (%v) — treating every NPC as hostile this pass", err)
		return nil, false
	}
	standingsMx, standingsMxLoaded = mx, true
	return standingsMx, true
}

// corpFactionFacts reads both halves of a corporation's faction once: the NPC row through
// evedb.GetAgentCorpFactionID (the module's corp-to-faction road, industry_ram.go) and the
// declared allegiance from rolebot_corps. Memoised for the process like pirateIDs; a query
// error is not cached and reads as zeros. forgetCorpFaction drops one entry when
// setCorpAllegiance writes.
func (m *Mod) corpFactionFacts(ctx context.Context, corpID int64) corpFactionEntry {
	if corpID == 0 {
		return corpFactionEntry{}
	}
	corpFactionMu.Lock()
	if f, ok := corpFactionCache[corpID]; ok {
		corpFactionMu.Unlock()
		return f
	}
	corpFactionMu.Unlock()

	var e corpFactionEntry
	f, err := evedb.GetAgentCorpFactionID(ctx, m.db, corpID)
	if err != nil {
		return corpFactionEntry{}
	}
	e.npc = f
	if e.npc == 0 {
		if err := m.db.QueryRowContext(ctx,
			`SELECT COALESCE(allegiance, 0) FROM rolebot_corps WHERE corporationID = ?`,
			corpID).Scan(&e.allegiance); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return corpFactionEntry{}
		}
	}
	corpFactionMu.Lock()
	corpFactionCache[corpID] = e
	corpFactionMu.Unlock()
	return e
}

// corpFactionID answers which faction a corporation flies for: the NPC row, else the player
// corporation's declared allegiance, else 0. Every reader that asks "what side is this corp on"
// takes this one road.
func (m *Mod) corpFactionID(ctx context.Context, corpID int64) int64 {
	e := m.corpFactionFacts(ctx, corpID)
	if e.npc != 0 {
		return e.npc
	}
	return e.allegiance
}

// npcCorpFactionID is the NPC row alone, 0 for any player corporation. For the places that must
// tell an employer from a pledge: the outlook branch and the allegiance writer's guard.
func (m *Mod) npcCorpFactionID(ctx context.Context, corpID int64) int64 {
	return m.corpFactionFacts(ctx, corpID).npc
}

// corpAllegiance is the declared allegiance alone, 0 for an NPC or an unaligned corporation.
func (m *Mod) corpAllegiance(ctx context.Context, corpID int64) int64 {
	return m.corpFactionFacts(ctx, corpID).allegiance
}

// forgetCorpFaction drops one corporation's memoised faction, so the next read sees a new
// allegiance.
func forgetCorpFaction(corpID int64) {
	corpFactionMu.Lock()
	delete(corpFactionCache, corpID)
	corpFactionMu.Unlock()
}

// gridNPCHostileTo is the embodied tier's wrapper. It never touches the database: it runs on the
// combat loop's goroutine, once per NPC per tick. The faction comes from Slim.WarFactionID and the
// standing from the NPCManager's StandingsMap. A nil manager or map answers hostile.
func (m *Mod) gridNPCHostileTo(eb *embodiedBot, e *destiny.Entity) bool {
	if eb == nil || e == nil || eb.factionID == 0 {
		return true
	}
	if m.npc == nil || m.npc.Standings == nil {
		return true
	}
	rat := e.Slim.WarFactionID
	if rat == 0 {
		// Not every ball is stamped (a dungeon spawn can arrive bare); its owning corp is the other half
		// of the lookup.
		rat = m.npc.Standings.CorpFaction(e.Slim.CorpID)
	}
	// Both directions, as factionAtOdds reads the matrix: the rat shoots first either way.
	return m.npc.Standings.IsHostile(eb.factionID, rat) || m.npc.Standings.Get(rat, eb.factionID) < 0
}

// gridStandingToPilot is standingToPilot (standings.go) for the embodied tier: it must not touch
// the database, so it reads the other pilot's corporation from Slim.CorpID and the corp-to-faction
// map the NPCManager loaded. Same rule: the worse of the pilot's own opinion and its employer's of
// the other employer, same short-circuits, and a missing faction reads neutral, not hostile.
func (m *Mod) gridStandingToPilot(eb *embodiedBot, e *destiny.Entity) float64 {
	return m.gridStandingToPilotFor(eb, e, false)
}

// gridEngageStandingToPilot is the opinion that decides whether to open fire on a pilot: the same
// fold, but the prejudice term goes through engageStanding (roe.go), so a Security or Defence
// doctrine never reads a hated race as hostile. wouldEngagePilot is its one caller.
func (m *Mod) gridEngageStandingToPilot(eb *embodiedBot, e *destiny.Entity) float64 {
	return m.gridStandingToPilotFor(eb, e, true)
}

func (m *Mod) gridStandingToPilotFor(eb *embodiedBot, e *destiny.Entity, engage bool) float64 {
	return m.gridMotiveToPilotFor(eb, e, engage).Standing
}

// gridMotiveToPilot is gridStandingToPilot with the deciding layer named, for
// chatter (chatter_motive.go): one body, so the number a bot shoots on and the
// reason it gives for shooting are the same calculation.
func (m *Mod) gridMotiveToPilot(eb *embodiedBot, e *destiny.Entity) motiveRead {
	return m.gridMotiveToPilotFor(eb, e, false)
}

func (m *Mod) gridMotiveToPilotFor(eb *embodiedBot, e *destiny.Entity, engage bool) motiveRead {
	var r motiveRead
	if eb == nil || e == nil {
		return r
	}
	charID := e.Slim.CharID
	if charID == 0 || charID == eb.charID {
		return r
	}
	// The personal layer is the starting value, not a fold; a pilot who insulted this bot in Local
	// (chatter_retort.go) is folded in on top.
	if eb.profile != nil {
		if v := eb.profile.StandingTo(charID); v != 0 {
			r.Standing, r.Why = v, motivePersonal
		}
	}
	if g := insultGrudge(eb.charID, charID); g != 0 {
		r = foldWhy(r, g, motivePersonal)
	}
	// The grace ledger (grace_ledger.go), map reads only: a grudge folds, a regard is only a starting value.
	if g := gracePersonal(eb.charID, charID); g < 0 {
		r = foldWhy(r, g, motivePersonal)
	} else if g > 0 && r.Standing == 0 {
		r.Standing, r.Why = g, motivePersonal
	}

	// The corporate layer (corprel.go), the same fold as the abstract tier, before the faction
	// short-circuit. relFold reads an immutable snapshot, so no query runs; alliances come off the balls.
	selfAlly := int64(0)
	if eb.ent != nil {
		selfAlly = eb.ent.Slim.AllianceID
	}
	corpWhy := motiveCorpRival
	if relAtWar(eb.corpID, selfAlly, e.Slim.CorpID, e.Slim.AllianceID) {
		corpWhy = motiveWar
	}
	r = foldWhy(r, relFold(r.Standing, eb.corpID, selfAlly, e.Slim.CorpID, e.Slim.AllianceID), corpWhy)
	// "Don't kill blues": a declared blue is applied after everything else
	// below has had its say (relBlueFloor, corprel.go).
	blue := relDeclaredBlue(eb.corpID, selfAlly, e.Slim.CorpID, e.Slim.AllianceID)

	// The same prejudice term the abstract tier folds (tolerance.go). Both
	// races come off the index snapshot, so this is a map read, never a query.
	if e.Slim.CorpID != eb.corpID {
		if x := xenoStanding(raceOf(eb.charID), eb.tune.Xenophobia, raceOf(charID)); x != 0 {
			r = foldPrejudice(r, x, engage, eb.doctrine)
		}
	}

	if eb.factionID == 0 || e.Slim.CorpID == 0 || e.Slim.CorpID == eb.corpID {
		return blueFloorWhy(r, blue)
	}
	if m.npc == nil || m.npc.Standings == nil {
		return blueFloorWhy(r, blue)
	}
	other := m.npc.Standings.CorpFaction(e.Slim.CorpID)
	if other == 0 || other == eb.factionID {
		return blueFloorWhy(r, blue)
	}
	return blueFloorWhy(foldWhy(r, m.npc.Standings.Get(eb.factionID, other), motiveFaction), blue)
}

func init() {
	registerCorpForget("corpFactionCache", forgetCorpFaction)
}
