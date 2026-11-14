package rolebots

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// The founding demand pass. A leader founds on its own roll at its employer
// review (corpfound.go considerFounding), which knows nothing of the job market.
// This pass does: when the bots waiting in NPC corporations outnumber the free
// openings in the player corporations that take bots by the dial
// rolebots.corpFound.demandRatio, it founds one corporation for the readiest
// leader (foundAnyNow). The switch, the cap, the pace and the sector balance
// all hold.

const (
	// foundDemandTicks: once an hour.
	foundDemandTicks = 3600
	// foundDemandMin is the least waiting bots worth a founding, so a small roster
	// with every corporation full does not found one an hour.
	foundDemandMin = 50
	// foundUnlimitedOpenings stands for a player corporation with no member limit.
	foundUnlimitedOpenings = 1000
)

var foundDemandBusy atomic.Bool

// startFoundingDemand registers the pass.
func (m *Mod) startFoundingDemand() {
	m.registerPeriodic("rolebot-corp-found-demand", foundDemandTicks, func(uint64) bool {
		if !Master() || !CorpFound() || !foundDemandBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("corp founding demand", func() {
			defer foundDemandBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.foundingDemandPass(ctx)
		})
		return true
	})
}

// foundDemandVerdict reports whether waiting bots call for a new corporation.
// Pure.
func foundDemandVerdict(waiting, openings, ratio int64) bool {
	return waiting >= foundDemandMin && waiting > ratio*max(openings, 0)
}

// foundingDemandPass reads the market and founds at most one corporation.
func (m *Mod) foundingDemandPass(ctx context.Context) {
	if clock.Unix()-corpFoundLastAt.Load() < corpFoundPaceSec {
		return
	}
	waiting, openings, err := m.foundingDemand(ctx)
	if err != nil {
		rlogf("rolebots: founding demand: %v", err)
		return
	}
	if !foundDemandVerdict(waiting, openings, foundDemandRatio()) {
		return
	}
	corpID, who, err := m.foundAnyNow(ctx, "demand")
	switch {
	case err == nil:
		rlogf("rolebots: founding demand: %d bots waiting for %d openings — %s founded corporation %d",
			waiting, openings, who, corpID)
	case errors.Is(err, errFoundSkip):
		// Nobody ready, the cap, or the pace: the next pass looks again.
	default:
		rlogf("rolebots: founding demand: %v", err)
	}
}

// foundingDemand counts the enabled bots in NPC corporations a founding could
// employ (out of school, not held by CONCORD or Sansha's Nation) and the free
// openings in the player corporations open to bots.
func (m *Mod) foundingDemand(ctx context.Context) (waiting, openings int64, err error) {
	if err = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM rolebot_characters r
		  JOIN character_ c ON c.characterID = r.characterID
		  JOIN crpNPCCorporations n ON n.corporationID = c.corporationID
		  LEFT JOIN chrFactions f ON f.factionID = n.factionID
		 WHERE r.enabled = 1
		   AND COALESCE(n.mainActivityID, 0) <> ?
		   AND COALESCE(f.factionName, '') NOT IN ('CONCORD Assembly', 'Sansha''s Nation')`,
		corpActivitySchool).Scan(&waiting); err != nil {
		return 0, 0, err
	}
	err = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN co.memberLimit <= 0 THEN ?
		                         ELSE GREATEST(co.memberLimit - COALESCE(mm.n, 0), 0) END), 0)
		  FROM rolebot_corps rc
		  JOIN corporation co ON co.corporationID = rc.corporationID AND COALESCE(co.deleted, 0) = 0
		  LEFT JOIN crpNPCCorporations n ON n.corporationID = co.corporationID
		  LEFT JOIN (SELECT corporationID, COUNT(*) AS n FROM character_
		              WHERE accountID > 0 GROUP BY corporationID) mm ON mm.corporationID = co.corporationID
		 WHERE rc.joinable = 1 AND n.corporationID IS NULL`,
		foundUnlimitedOpenings).Scan(&openings)
	return waiting, openings, err
}
