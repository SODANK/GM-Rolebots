package rolebots

// The fold and the flush: where incidents become a value and the value becomes a standing.
//
// relFoldCorp decays every pair of one corporation to now, applies the unfolded incidents,
// holds the pact floors, re-tiers and saves; it writes nothing to the game. relFlushCorp is
// the only writer of crpStandings for a managed corporation: it applies the charter's caps
// (relApplyWriteCaps), yields pairs a human edited, and calls SetCorpStanding with the actor
// as setter only on a tier crossing. The periodic (startRelationsFold) flushes tier crossings
// only; the diplomacy pass (corpdiplomacy_pass.go) writes sub-tier drift. Both hold corpOpLock.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// relFoldSaveEps: a decay that moved a value less than this is not saved, so an idle
// pair is not rewritten on every fold.
const relFoldSaveEps = 0.01

// relFoldCorp folds one corporation's pairs. Caller holds corpOpLock.
func (m *Mod) relFoldCorp(ctx context.Context, c *corpCEO, ch Charter, b diploBend) (bool, error) {
	rows, err := m.relRowsOf(ctx, c.CorpID)
	if err != nil {
		return false, err
	}
	pend, err := m.relPendingIncidents(ctx, c.CorpID)
	if err != nil {
		return false, err
	}
	now := clock.Unix()
	p := relParamsNow()
	tierChanged := false
	var applied []int64
	seen := map[int64]bool{}
	fold := func(toID int64, s relState, has bool) error {
		incs := pend[toID]
		if !has {
			if len(incs) == 0 {
				return nil
			}
			s = relState{DecayedAt: now, TierAt: now}
		}
		before := s
		next, res := relFoldState(s, incs, now, ch, b, p)
		if res.PactBroken {
			m.relBreakPact(ctx, c, toID, now)
		}
		if res.TierChanged {
			tierChanged = true
		}
		for _, inc := range incs {
			applied = append(applied, inc.ID)
		}
		moved := len(incs) > 0 || res.TierChanged || res.PactBroken ||
			math.Abs(next.Value-before.Value) >= relFoldSaveEps ||
			(before.FloorUntil != 0 && next.FloorUntil == 0)
		if !moved {
			return nil
		}
		return m.relSaveRow(ctx, c.CorpID, toID, next)
	}
	for toID, s := range rows {
		seen[toID] = true
		if err := fold(toID, s, true); err != nil {
			return tierChanged, err
		}
	}
	for toID := range pend {
		if seen[toID] {
			continue
		}
		if err := fold(toID, relState{}, false); err != nil {
			return tierChanged, err
		}
	}
	if err := m.relMarkApplied(ctx, applied, now); err != nil {
		return tierChanged, err
	}
	return tierChanged, nil
}

// relBreakPact ends the standing pact between two corporations after one
// side's hostile act: the pact row is marked broken, the OTHER direction's
// floor is cleared (the fold cleared ours), the breaker's own ledger takes
// its share, and the ledger line says so.
func (m *Mod) relBreakPact(ctx context.Context, c *corpCEO, breaker int64, now int64) {
	a, bb := c.CorpID, breaker
	if a > bb {
		a, bb = bb, a
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_pacts SET status = 'broken', brokenBy = ?, endAt = ?, reason = 'broken by a hostile act'
		 WHERE corpA = ? AND corpB = ? AND status = 'standing'`, breaker, now, a, bb); err != nil {
		rlogf("rolebots: relations: break pact %d/%d: %v", a, bb, err)
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_relations SET floorValue = 0, floorUntil = 0
		 WHERE corporationID = ? AND toID = ?`, breaker, c.CorpID); err != nil {
		rlogf("rolebots: relations: clear pact floor %d → %d: %v", breaker, c.CorpID, err)
	}
	p := relParamsNow()
	m.noteCorpIncident(ctx, breaker, c.CorpID, incPactBreak, relSideAggressor,
		relBaseWeight(incPactBreak, relSideAggressor, 0, p), 0, 0, 0,
		fmt.Sprintf("pactbreak:%d:%d", c.CorpID, now), "broke the pact with "+corpNameOf(ctx, m.db, c.CorpID))
	m.ceoLog(ctx, c, powerDiplomacy, "pactbreak", breaker, 0,
		"%s broke its pact with us", corpNameOf(ctx, m.db, breaker))
}

// relFlushCorp writes one corporation's standings. drift false = only pairs
// whose tier crossed since the last write; true = every pair whose written
// number moved. Caller holds corpOpLock. Returns whether any tier crossed.
func (m *Mod) relFlushCorp(ctx context.Context, c *corpCEO, ch Charter, actorID int64, drift bool) (bool, error) {
	rows, err := m.relRowsOf(ctx, c.CorpID)
	if err != nil {
		return false, err
	}
	live, err := m.ceoRelLive(ctx, c.CorpID)
	if err != nil {
		return false, err
	}
	now := clock.Unix()
	var writes []relWriteRow
	for toID, s := range rows {
		lv, hasLive := live[toID]
		switch relOwnership(lv, hasLive, s, true) {
		case relOwnYielded:
			continue
		case relOwnYield:
			name := corpNameOf(ctx, m.db, toID)
			if err := markRelationYielded(ctx, m.db, c.CorpID, toID, lv, "edited by a director"); err != nil {
				rlogf("rolebots: relations yield corp %d → %d: %v", c.CorpID, toID, err)
				continue
			}
			m.ceoLog(ctx, c, powerDiplomacy, "yield", toID, lv,
				"left its standing toward %s to the directors — somebody else changed it", name)
			continue
		}
		writes = append(writes, relWriteRow{ToID: toID, Value: s.Value, Tier: s.Tier,
			Floor: s.FloorValue, HasFloor: s.FloorUntil > now, Seated: relTierEnter(s.Written)})
	}
	relApplyWriteCaps(writes, ch)

	crossedAny := false
	n, max := 0, relFlushMax()
	for _, w := range writes {
		s := rows[w.ToID]
		cur, want := s.Written, w.Written
		crossed := relTierEnter(cur) != relTierEnter(want)
		if !crossed && (!drift || math.Abs(want-cur) < relWrittenEps) {
			continue
		}
		if n >= max {
			break
		}
		n++
		setBy := int64(0)
		if crossed {
			setBy = actorID
		}
		if err := evedb.SetCorpStanding(ctx, m.db, c.CorpID, w.ToID, want, setBy, truncate(s.Reason, 200)); err != nil {
			if errors.Is(err, evedb.ErrMaxCorpStandings) {
				m.logBot(c.CharID, logInfo, "ceo", "diplomacy: the standings list is full — %s left unwritten",
					corpNameOf(ctx, m.db, w.ToID))
				continue
			}
			return crossedAny, err
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_corp_relations SET written = ? WHERE corporationID = ? AND toID = ?`,
			want, c.CorpID, w.ToID); err != nil {
			return crossedAny, fmt.Errorf("relFlushCorp written: %w", err)
		}
		if crossed {
			crossedAny = true
			name := corpNameOf(ctx, m.db, w.ToID)
			tier := relTierEnter(want)
			m.ceoLog(ctx, c, powerDiplomacy, ceoRelVerb(tier), w.ToID, want,
				"%s (%+.1f) — %s", ceoRelPhrase(relTierEnter(cur), tier, name), want, s.Reason)
		}
		if want == 0 && s.Tier == 0 && s.FloorUntil <= now && math.Abs(s.Value) < relDropBelow {
			if err := m.relDeleteRow(ctx, c.CorpID, w.ToID); err != nil {
				return crossedAny, err
			}
		}
	}
	return crossedAny, nil
}

// ceoCharterFor resolves a managed corporation's charter outside its think:
// the CEO's traits through resolveCharter, then the chair's standing orders.
func (m *Mod) ceoCharterFor(ctx context.Context, c *corpCEO) (Charter, error) {
	traits, err := getTraits(ctx, m.db, c.CharID)
	if err != nil {
		return Charter{}, err
	}
	p := pilotIdentity{CharID: c.CharID, Traits: traits, Tune: resolveTuning(traits)}
	return m.chairCharter(ctx, c.CorpID, resolveCharter(p)), nil
}

// ── the periodic ────────────────────────────────────────────────────────────

var (
	relFoldBusy   atomic.Bool
	relFoldLastAt atomic.Int64
)

// startRelationsFold registers the fold: every foldSec, every managed
// corporation with an incident waiting or a pact expiring is folded and its
// tier crossings flushed. Gated on Master like the staff desks, and on the
// ledger switch; never on the think, which folds for itself.
func (m *Mod) startRelationsFold() {
	m.registerPeriodic("rolebot-relations-fold", 30, func(uint64) bool {
		if !Master() || !relationsEnabled() || !relLedgerOn() {
			return true
		}
		if now := clock.Unix(); now-relFoldLastAt.Load() < int64(relFoldSec()) {
			return true
		}
		if !relFoldBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("relations fold", func() {
			defer relFoldBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.relFoldPass(ctx)
			relFoldLastAt.Store(clock.Unix())
		})
		return true
	})
}

// relFoldPass is one sweep: the corporations with work, each under its own
// lock, each folded and flushed for tier crossings. One snapshot refresh at
// the end if anything crossed.
func (m *Mod) relFoldPass(ctx context.Context) {
	corps, err := m.relCorpsWithPending(ctx)
	if err != nil {
		rlogf("rolebots: relations fold: %v", err)
		return
	}
	now := clock.Unix()
	if rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT corporationID FROM rolebot_corp_relations
		 WHERE floorUntil > 0 AND floorUntil < ?`, now); err == nil {
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				corps = append(corps, id)
			}
		}
		rows.Close()
	}
	seen := map[int64]bool{}
	crossed := false
	for _, corpID := range corps {
		if seen[corpID] || ctx.Err() != nil {
			continue
		}
		seen[corpID] = true
		c, err := getCEO(ctx, m.db, corpID)
		if err != nil || c == nil || c.Paused || !c.powerOn(powerDiplomacy) {
			continue
		}
		lk := corpOpLock(corpID)
		if !lk.TryLock() {
			continue // the CEO is thinking or a desk is open; next pass
		}
		func() {
			defer lk.Unlock()
			ctx := withOpLockHeld(ctx)
			ch, err := m.ceoCharterFor(ctx, c)
			if err != nil {
				rlogf("rolebots: relations fold corp %d charter: %v", corpID, err)
				return
			}
			b := m.diplomatBendFor(ctx, c)
			if t, err := m.relFoldCorp(ctx, c, ch, b); err != nil {
				m.logBot(c.CharID, logError, "ceo", "relations fold: %v", err)
				return
			} else if t {
				crossed = true
			}
			if t, err := m.relFlushCorp(ctx, c, ch, c.CharID, false); err != nil {
				m.logBot(c.CharID, logError, "ceo", "relations flush: %v", err)
			} else if t {
				crossed = true
			}
		}()
	}
	if crossed {
		m.refreshRelations(ctx)
	}
	m.sweepGankLog(ctx)
}

// diplomatBendFor is how the sitting Director of Diplomacy bends the rates:
// identity with no director, or a human one (the CEO covers the post in
// their name, with its own temperament).
func (m *Mod) diplomatBendFor(ctx context.Context, c *corpCEO) diploBend {
	if h := m.staffFor(ctx, c).head(portDiplomacy); h != nil && !h.Human && h.Enabled {
		return diploBendOf(h.Ident)
	}
	return diploBendNone()
}
