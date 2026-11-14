package rolebots

import (
	"context"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// The openings sweep keeps every managed corporation's member limit and "full"
// flag true whether or not its CEO is logged on. The CEO's think refreshes both
// too (ceoIdentity, workHRDesk), but a logged-off CEO does not think while its
// skills keep training, so a corporation could sit full at a limit its CEO had
// already passed. The flag lives in memory (corpceo_hr.go openingsFull); the
// sweep's first run at boot seeds it, so the skill planner's member-limit goal
// survives a restart.

// openingSweepTicks: every ten minutes.
const openingSweepTicks = 600

var openingSweepBusy atomic.Bool

// startOpeningSweep registers the sweep and runs it once now.
func (m *Mod) startOpeningSweep() {
	run := func() {
		if !Master() || !openingSweepBusy.CompareAndSwap(false, true) {
			return
		}
		goSafe("corp openings sweep", func() {
			defer openingSweepBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			m.openingSweep(ctx)
		})
	}
	run()
	m.registerPeriodic("rolebot-corp-openings", openingSweepTicks, func(uint64) bool {
		run()
		return true
	})
}

// openingSweep refreshes each managed corporation's member limit from its CEO's
// skills, then records whether it has an opening left.
func (m *Mod) openingSweep(ctx context.Context) {
	ceos, err := listCEOs(ctx, m.db)
	if err != nil {
		rlogf("rolebots: openings sweep: %v", err)
		return
	}
	for _, c := range ceos {
		if ctx.Err() != nil {
			return
		}
		if c.Paused {
			continue
		}
		if c.powerOn(powerIdentity) {
			oldLimit, newLimit, oldRaces, newRaces, err := evedb.RecomputeCorpAbilities(ctx, m.db, c.CorpID)
			if err != nil {
				rlogf("rolebots: openings sweep corp %d: %v", c.CorpID, err)
				continue
			}
			if oldLimit != newLimit || oldRaces != newRaces {
				m.noteAbilityChange(ctx, c, corpNameOf(ctx, m.db, c.CorpID), oldLimit, newLimit, oldRaces, newRaces)
			}
		}
		free, unlimited, err := m.corpFreeOpenings(ctx, c.CorpID)
		if err != nil {
			continue
		}
		setOpeningsFull(c.CorpID, !unlimited && free <= 0)
	}
}
