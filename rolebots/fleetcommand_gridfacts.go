package rolebots

import (
	"context"
	"sync"
	"time"

	"gurimoon/destiny"
)

// What the grid FC knows of a hostile pilot's ship (fleetcommand_grid.go): its remote repair, its
// tackle and its dps, the numbers the abstract fight reads off a pvpCombatant. The grid tick may
// not query, so they come from a per-ship cache: a projection's live loadout answers at once, and
// anything else (a player, a projection not yet fighting) is loaded off the tick on the first
// sighting and read 0 until it lands. Rats keep the threat cache (ratThreatCached).

// gridFactsTTL is how long one ship's loaded numbers stand. A pilot refits only docked, so a few
// minutes covers a fight and catches the next.
const gridFactsTTL = 5 * time.Minute

// gridFactsMax bounds the cache; past it, entries older than gridFactsTTL are dropped.
const gridFactsMax = 2048

// gridShipFacts is one hostile ship's fighting numbers.
type gridShipFacts struct {
	Rep    float64 // remote repair, HP a second, sustained (loadoutRepRates)
	Points float64 // warp scramble strength fitted (tacklePointsOn)
	DPS    float64
}

type gridFactsEntry struct {
	facts   gridShipFacts
	at      time.Time
	loading bool
}

var (
	gridFactsMu sync.Mutex
	gridFacts   = map[int64]*gridFactsEntry{} // by ship itemID
)

// gridShipFactsOf is a pilot ship's numbers for the FC's scorer: 0 for a rat (no pilot) or a
// ship whose numbers are still loading. A projection's live loadout (its combat loop's) overrides
// the cached rep and dps.
func (m *Mod) gridShipFactsOf(e *destiny.Entity) gridShipFacts {
	if e == nil || e.Slim.CharID == 0 {
		return gridShipFacts{}
	}
	charID, shipID, typeID := e.Slim.CharID, int64(e.ID), e.Slim.TypeID
	now := clock.Now()
	gridFactsMu.Lock()
	ent := gridFacts[shipID]
	if ent == nil {
		if len(gridFacts) >= gridFactsMax {
			for id, old := range gridFacts {
				if !old.loading && now.Sub(old.at) > gridFactsTTL {
					delete(gridFacts, id)
				}
			}
		}
		ent = &gridFactsEntry{}
		gridFacts[shipID] = ent
	}
	f := ent.facts
	load := !ent.loading && now.Sub(ent.at) > gridFactsTTL && m != nil && m.db != nil
	if load {
		ent.loading = true
	}
	gridFactsMu.Unlock()
	if load {
		goSafe("grid ship facts", func() { m.loadGridShipFacts(charID, shipID, typeID) })
	}
	if p := liveProjection(charID); p != nil {
		p.mu.Lock()
		l := p.load
		p.mu.Unlock()
		if l != nil {
			f.Rep, _ = loadoutRepRates(l)
			f.DPS = l.dps()
		}
	}
	return f
}

// loadGridShipFacts reads one ship's fitted rack into the cache, off the grid tick.
func (m *Mod) loadGridShipFacts(charID, shipID, typeID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var f gridShipFacts
	l, err := m.loadCombatLoadout(ctx, charID, shipID, typeID)
	if err == nil && l != nil {
		f.Rep, _ = loadoutRepRates(l)
		f.DPS = l.dps()
	}
	f.Points = m.tacklePointsOn(ctx, shipID, grpWarpScrambler, 19, 26, 1)
	gridFactsMu.Lock()
	defer gridFactsMu.Unlock()
	if ent := gridFacts[shipID]; ent != nil {
		ent.loading = false
		if err == nil {
			ent.facts, ent.at = f, clock.Now()
		} else {
			// A failed read waits out the TTL before it is tried again.
			ent.at = clock.Now()
		}
	}
}

// fcBall is one hostile as the grid FC sees it before scoring: its ball, the hitpoints it has
// left, whether it is inside the commander's lock reach, and its ship's numbers.
type fcBall struct {
	ID      uint32
	EHP     float64
	InReach bool
	Facts   gridShipFacts
}

// fcCandsOf is the FC's scorer input from the balls: a candidate for each living hostile in
// reach, with its rep and tackle, and theirRep, every living hostile's remote repair, in reach or
// not (a logi off the edge of the lock still mends the primary). Pure.
func fcCandsOf(balls []fcBall) (cands []fcCand, byID map[int64]uint32, theirRep float64) {
	byID = map[int64]uint32{}
	for _, b := range balls {
		if b.EHP <= 0 {
			continue
		}
		theirRep += b.Facts.Rep
		if !b.InReach {
			continue
		}
		cands = append(cands, fcCand{ID: int64(b.ID), EHP: b.EHP, Rep: b.Facts.Rep, Points: b.Facts.Points})
		byID[int64(b.ID)] = b.ID
	}
	return cands, byID, theirRep
}
