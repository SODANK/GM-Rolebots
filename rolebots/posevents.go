package rolebots

import (
	"context"
	"time"

	evedb "gurimoon/db"
)

// The core's starbase lifecycle events as a bot corporation hears them
// (db.SubscribeStarbaseEvents). The desks poll every 15 minutes; these let a tower that is
// running dry, dark or reinforced jump the queue.
//   - fuel-low, strontium-low: the tower is marked urgent; its requisitions are bought on a
//     trip and its fuel leg goes first.
//   - out-of-fuel: urgent, and the next desk pass re-onlines it once a fuel leg has landed
//     (OnlineStarbaseForChecked).
//   - reinforced: a siege row with the exit time both sides read (possiege.go).
//     reinforce-ended: the defence held. destroyed: the plan is closed and a loss counted.
// Any tower planted, onlined, running low or taken down resets the ice ledger (icedemand.go).

// startStarbaseEvents subscribes once, at Init.
func (m *Mod) startStarbaseEvents() {
	evedb.SubscribeStarbaseEvents(func(ev evedb.StarbaseEvent) {
		if !Master() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		m.onStarbaseEvent(ctx, ev)
	})
}

func (m *Mod) onStarbaseEvent(ctx context.Context, ev evedb.StarbaseEvent) {
	switch ev.Kind {
	case evedb.StarbaseEventPlanted, evedb.StarbaseEventOnline, evedb.StarbaseEventUnanchored,
		evedb.StarbaseEventRemoved, evedb.StarbaseEventDestroyed, evedb.StarbaseEventOutOfFuel, evedb.StarbaseEventFuelLow:
		// A tower appeared, went or ran low: its fuel is wanted (or not) now,
		// not at the next hourly rebuild (icedemand.go).
		invalidateIceDemand()
	}
	p, _ := m.towerPlanByTower(ctx, ev.TowerID)
	now := clock.Unix()
	switch ev.Kind {
	case evedb.StarbaseEventFuelLow, evedb.StarbaseEventStrontiumLow, evedb.StarbaseEventOutOfFuel:
		if p == nil {
			return
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_towers SET urgentAt = ? WHERE planID = ?`, now, p.PlanID)
		markPosWork(p.CorpID)
		if c, _ := getCEO(ctx, m.db, p.CorpID); c != nil {
			m.ceoLog(ctx, c, powerStarbase, ev.Kind, ev.TowerID, ev.HoursLeft,
				"heard its tower at %s is %s (%.0f h left)", moonNameOf(ctx, m, p.MoonID), ev.Kind, ev.HoursLeft)
		}
	case evedb.StarbaseEventReinforced:
		m.onTowerReinforced(ctx, ev, p)
	case evedb.StarbaseEventReinforceEnded:
		m.onReinforceEnded(ctx, ev, p)
	case evedb.StarbaseEventDestroyed:
		m.onTowerDestroyed(ctx, ev, p)
		if p != nil {
			p.Losses++
			p.Status, p.ClosedAt, p.Note = towerGone, now, "destroyed"
			if c, _ := getCEO(ctx, m.db, p.CorpID); c != nil {
				m.ceoLog(ctx, c, powerStarbase, "lost", ev.TowerID, 0,
					"lost its %s tower at %s — destroyed", purposeLabel[p.Purpose], moonNameOf(ctx, m, p.MoonID))
				_ = m.closeTowerPlan(ctx, c, p, "destroyed")
			} else {
				_ = m.saveTowerPlan(ctx, p)
			}
		}
	}
}
