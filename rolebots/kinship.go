package rolebots

import (
	"context"
	"sync"
	"time"

	"gurimoon/destiny"
)

// sameBloc reports whether two pilots are on the same side: same corporation, same alliance, or
// same faction (both employed by the same power). A zero on either side is never kinship.
// It is structural, with no standing in it, and it only forbids starting a fight.
// See docs/comment-notes-b12.md#kinship.
func sameBloc(selfCorp, selfAlliance, selfFaction, otherCorp, otherAlliance, otherFaction int64) bool {
	if selfCorp != 0 && selfCorp == otherCorp {
		return true
	}
	if selfAlliance != 0 && selfAlliance == otherAlliance {
		return true
	}
	return selfFaction != 0 && selfFaction == otherFaction
}

// ── the alliance half ───────────────────────────────────────────────────────

// Corp to faction is static seed data, memoised forever (faction.go). Corp to alliance changes
// as corporations join and leave and as the module moves bots, so this cache expires: a stale
// answer costs one sweep of over-politeness.
const corpAllianceTTL = 5 * time.Minute

type corpAllianceEntry struct {
	id int64
	at time.Time
}

var (
	corpAllianceMu    sync.Mutex
	corpAllianceCache = map[int64]corpAllianceEntry{}
)

// corpAllianceID answers which alliance a corporation flies for, 0 for none.
func (m *Mod) corpAllianceID(ctx context.Context, corpID int64) int64 {
	if corpID == 0 || m.db == nil {
		return 0
	}
	now := clock.Now()
	corpAllianceMu.Lock()
	if e, ok := corpAllianceCache[corpID]; ok && now.Sub(e.at) < corpAllianceTTL {
		corpAllianceMu.Unlock()
		return e.id
	}
	corpAllianceMu.Unlock()

	var id int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(allianceID, 0) FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&id); err != nil {
		// Unknown, not "no alliance": a database error must not be cached as a fact that lets a
		// bot shoot its own alliance.
		return 0
	}
	corpAllianceMu.Lock()
	corpAllianceCache[corpID] = corpAllianceEntry{id: id, at: now}
	corpAllianceMu.Unlock()
	return id
}

// ── the two tiers ───────────────────────────────────────────────────────────

// kinToPilot is the abstract sim's question, and it costs two cached lookups
// plus one row of character_. Used on the planning paths, never in a combat
// loop.
func (m *Mod) kinToPilot(ctx context.Context, self standingSelf, otherCharID int64) bool {
	if otherCharID == 0 || self.CharID == otherCharID {
		return true // itself: never a target, whatever the paperwork says
	}
	otherCorp := m.charCorpID(ctx, otherCharID)
	if otherCorp == 0 {
		return false
	}
	if self.CorpID != 0 && self.CorpID == otherCorp {
		return true
	}
	return sameBloc(
		self.CorpID, m.corpAllianceID(ctx, self.CorpID), self.FactionID,
		otherCorp, m.corpAllianceID(ctx, otherCorp), m.corpFactionID(ctx, otherCorp),
	)
}

// gridKin is the embodied tier's twin and, like gridStandingToPilot (faction.go), must not
// touch the database: it runs on the combat loop's goroutine once per ball per tick. Corp and
// alliance ride the ball (Slim, stamped at spawn); the employer faction comes from the
// NPCManager's StandingsMap. Without a manager only the faction clause is lost.
func (m *Mod) gridKin(eb *embodiedBot, e *destiny.Entity) bool {
	if eb == nil || e == nil {
		return false
	}
	if e.Slim.CharID != 0 && e.Slim.CharID == eb.charID {
		return true
	}
	var selfAlliance, otherAlliance int64
	if eb.ent != nil {
		selfAlliance = eb.ent.Slim.AllianceID
	}
	otherAlliance = e.Slim.AllianceID
	otherFaction := int64(0)
	if m.npc != nil && m.npc.Standings != nil {
		otherFaction = m.npc.Standings.CorpFaction(e.Slim.CorpID)
	}
	return sameBloc(eb.corpID, selfAlliance, eb.factionID,
		e.Slim.CorpID, otherAlliance, otherFaction)
}

func init() {
	registerCorpForget("corpAllianceCache", func(id int64) {
		corpAllianceMu.Lock()
		delete(corpAllianceCache, id)
		corpAllianceMu.Unlock()
	})
}
