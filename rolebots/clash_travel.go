//go:build mindclash

package rolebots

// Mind Clash: the fan errand (docs/MIND_CLASH_BOTS.md §4).
// A bot with real money on a fight may fly to the venue and dock there for the night, as a
// won patent sends a pilot to collect (research.go): travel (TaskClashTravel), then stay
// docked (TaskClashAttend) until the fight it came for is over. Docked with a bet down, the
// bot is the crowd in the house: the core counts it toward the Crowd at the bell through
// evedb.MCBotDockedAt.
// Fans are never un-benched: nothing rides on attendance but atmosphere, so the population
// cap treats them like any idle bot.

import (
	"context"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
)

const (
	TaskClashTravel TaskKind = "clash.travel"
	TaskClashAttend TaskKind = "clash.attend"
	ActClash                 = "clash"

	clashErrandLeadSec = 6 * 3600 // start this long before (bell − travel)
	clashSecPerJump    = 75
)

var (
	clashErrandMu sync.RWMutex
	clashErrands  = map[int64]clashRow{} // charID → errand
)

func init() {
	registerTask(TaskDef{
		Kind: TaskClashTravel, Label: "Flying to a Mind Clash venue",
		Posture: PostureDocked, Detail: detailClash,
		Step: (*Mod).clashStep,
	})
	registerTask(TaskDef{
		Kind: TaskClashAttend, Label: "Docked at the Mind Clash venue for Clash Night",
		Posture: PostureDocked, Detail: detailClash,
		Step: (*Mod).clashStep,
	})
}

// clashErrandActive: the population cap's bench exemption. Only fighters
// ever had one, and the roster's fighters are the circuit's own now, so no
// bot is ever exempt.
func clashErrandActive(charID int64) bool { return false }

// clashRefreshErrands reloads the errand cache and clears errands whose
// card has gone (a cancelled night, a finished one).
func (m *Mod) clashRefreshErrands(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT characterID FROM rolebot_clash WHERE errandSlot > 0 AND errandState <> 'done'`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	fresh := map[int64]clashRow{}
	for _, id := range ids {
		r := m.clashRowOf(ctx, id)
		if c, err := evedb.MCGetCard(ctx, m.db, r.ErrandCard); err != nil || c.State == evedb.MCCardCancelled || c.State == evedb.MCCardDone {
			m.clashErrandDone(ctx, id, "the night is over")
			continue
		} else if c.FirstBell != r.FirstBell {
			// The night moved.
			r.FirstBell = c.FirstBell
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_clash SET firstBell = ? WHERE characterID = ?`, c.FirstBell, id)
		}
		fresh[id] = r
	}
	clashErrandMu.Lock()
	clashErrands = fresh
	clashErrandMu.Unlock()
}

// clashSetErrand files a venue errand.
func (m *Mod) clashSetErrand(ctx context.Context, id int64, c *evedb.MCCard, slotID int64, kind string) {
	sysID, _ := evedb.GetSolarSystemForStation(ctx, m.db, c.VenueStationID)
	m.clashTouch(ctx, id)
	_, err := m.db.ExecContext(ctx, `UPDATE rolebot_clash SET errandSlot = ?, errandCard = ?, errandKind = ?, venueStation = ?,
		venueSystem = ?, firstBell = ?, errandState = 'due' WHERE characterID = ?`, slotID, c.ID, kind, c.VenueStationID, sysID, c.FirstBell, id)
	if err != nil {
		return
	}
	r := m.clashRowOf(ctx, id)
	clashErrandMu.Lock()
	clashErrands[id] = r
	clashErrandMu.Unlock()
}

// clashErrandDone ends an errand and hands the pilot back to the planner on
// its next idle pass.
func (m *Mod) clashErrandDone(ctx context.Context, id int64, why string) {
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_clash SET errandSlot = 0, errandCard = 0, errandKind = '', errandState = 'done' WHERE characterID = ?`, id)
	clashErrandMu.Lock()
	delete(clashErrands, id)
	clashErrandMu.Unlock()
	if why != "" {
		m.logBot(id, logInfo, "clash", "venue errand over: %s", why)
	}
}

func (m *Mod) clashCardSettled(ctx context.Context, cardID int64) {
	rows, err := m.db.QueryContext(ctx, `SELECT characterID FROM rolebot_clash WHERE errandCard = ?`, cardID)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		m.clashErrandDone(ctx, id, "Clash Night is over")
	}
}

// ── plan() ─────────────────────────────────────────────────────────────────

// clashVenueDue is plan()'s hook: a fan with an errand sets off in time for
// the bell. Free on the common pass: a map read.
func (m *Mod) clashVenueDue(ctx context.Context, b *botRun) (bool, error) {
	clashErrandMu.RLock()
	e, ok := clashErrands[b.CharID]
	clashErrandMu.RUnlock()
	if !ok || e.ErrandState == "done" || e.VenueStation == 0 {
		return false, nil
	}
	now := clock.Unix()
	if now > e.FirstBell+4*3600 {
		m.clashErrandDone(ctx, b.CharID, "the night is long over")
		return false, nil
	}
	jumps := 0
	if b.SystemID != e.VenueSystem {
		if j, err := m.jumpsBetween(ctx, b, b.SystemID, e.VenueSystem); err == nil {
			jumps = j
		} else {
			jumps = 20
		}
	}
	travel := int64(jumps*clashSecPerJump + 600)
	if now < e.FirstBell-travel-clashErrandLeadSec {
		return false, nil
	}
	st := b.State
	st.Activity = ActClash
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_clash SET errandState = 'travel' WHERE characterID = ?`, b.CharID)
	if b.StationID == e.VenueStation {
		st.Phase = string(TaskClashAttend)
		return true, saveState(ctx, m.db, st)
	}
	m.logBot(b.CharID, logInfo, "clash", "sets off to cheer from the house at Clash Night (%d jumps; first bell %s)", jumps,
		time.Unix(e.FirstBell, 0).UTC().Format("Mon 15:04"))
	return true, m.beginTravel(ctx, b, e.VenueSystem, e.VenueStation, string(TaskClashTravel))
}

// clashStep advances the errand.
func (m *Mod) clashStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	clashErrandMu.RLock()
	e, ok := clashErrands[b.CharID]
	clashErrandMu.RUnlock()
	finish := func() (bool, error) {
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	if !ok || e.ErrandState == "done" {
		return finish()
	}
	switch TaskKind(st.Phase) {
	case TaskClashTravel:
		if b.StationID != e.VenueStation {
			return false, m.beginTravel(ctx, b, e.VenueSystem, e.VenueStation, string(TaskClashTravel))
		}
		st.Phase = string(TaskClashAttend)
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_clash SET errandState = 'docked' WHERE characterID = ?`, b.CharID)
		m.logBot(b.CharID, logInfo, "clash", "docked at the venue")
		return false, saveState(ctx, m.db, st)
	case TaskClashAttend:
		if b.StationID != e.VenueStation {
			// Moved off the venue somehow (a restart, a GM): go back.
			st.Phase = string(TaskClashTravel)
			return false, m.beginTravel(ctx, b, e.VenueSystem, e.VenueStation, string(TaskClashTravel))
		}
		c, err := evedb.MCGetCard(ctx, m.db, e.ErrandCard)
		if err != nil || c.State == evedb.MCCardDone || c.State == evedb.MCCardCancelled {
			m.clashErrandDone(ctx, b.CharID, "the card is over")
			return finish()
		}
		s, err := evedb.MCGetSlot(ctx, m.db, e.ErrandSlot)
		if err != nil || s.State == "done" || s.State == "noshow" || s.State == "cancelled" {
			m.clashErrandDone(ctx, b.CharID, "the fight they came for is over")
			return finish()
		}
		return false, nil // stay docked
	}
	return finish()
}

func (m *Mod) clashHasBetsOn(ctx context.Context, id, cardID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mcToteBets b JOIN mcToteMarkets m ON m.marketID = b.marketID
		WHERE b.bettorID = ? AND m.cardID = ? AND b.state = 'open'`, id, cardID).Scan(&n)
	return n > 0
}

// detailClash is the panel's one-line description of the errand.
func detailClash(m *Mod, ctx context.Context, st *SimState) string {
	clashErrandMu.RLock()
	e, ok := clashErrands[st.CharID]
	clashErrandMu.RUnlock()
	if !ok {
		return ""
	}
	return fmt.Sprintf("in the house at Clash Night, first bell %s", time.Unix(e.FirstBell, 0).UTC().Format("Mon 15:04"))
}

func init() {
	registerCharForget("clashErrands", func(id int64) {
		clashErrandMu.Lock()
		delete(clashErrands, id)
		clashErrandMu.Unlock()
	})
}
