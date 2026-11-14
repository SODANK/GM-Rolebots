package rolebots

// The diplomacy pass: what the CEO, or its Director of Diplomacy, does about the
// corporation's relations every few hours. It is the only place the baseline is
// recomputed and sub-tier drift reaches the game; the fold periodic
// (corprelations_fold.go) handles tier crossings between passes.
//  1. Every live non-kin player corporation gets a baseline from the static terms
//     (ceoRelBaseline); a gone counterparty's pair decays to 0 and drops.
//  2. The killboard catch-up files kills the hooks never saw; the fold applies
//     everything pending; the flush writes every pair whose number moved.
//  3. The letters (corpletters.go): unanswered ones lapse, those on this desk are
//     answered, accepted ones are honoured, and at most one new one is written.

import (
	"context"
)

// ceoDiplomacy is the CEO doing its own diplomacy: the pass with no
// director's bend on it.
func (m *Mod) ceoDiplomacy(ctx context.Context, c *corpCEO, ch Charter) error {
	return m.diplomacyPass(ctx, c, ch, diploBendNone())
}

// diplomacyPass runs under corpOpLock (the think holds it; the desk holds it).
func (m *Mod) diplomacyPass(ctx context.Context, c *corpCEO, ch Charter, b diploBend) error {
	if !ceoDiplomacyOn() || !relationsEnabled() || !relLedgerOn() {
		return nil
	}
	selfFacts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	corps, err := m.playerCorpFactsShared(ctx)
	if err != nil {
		return err
	}
	var ids []int64
	var targets []CorpFacts
	// kin are the other members of the alliance (kinship.go). Their rows decide
	// nothing while it stands but are kept, floored at FriendAt: relAlliesOf reads them.
	kin := map[int64]bool{}
	for _, f := range corps {
		if f.CorpID == c.CorpID || f.Members+f.Bots == 0 {
			continue
		}
		if selfFacts.AllianceID != 0 && f.AllianceID == selfFacts.AllianceID {
			kin[f.CorpID] = true
		}
		targets = append(targets, f)
		ids = append(ids, f.CorpID)
	}
	peoples, err := m.ceoRelPeoples(ctx, append(ids, c.CorpID))
	if err != nil {
		return err
	}
	self := ceoRelPartyOf(*selfFacts, peoples[c.CorpID])
	if self.Race == 0 {
		self.Race = m.charRace(ctx, c.CharID)
	}
	empires, _ := m.empireFactionIDs(ctx)
	mx, mxOK := m.standingsMatrix(ctx)
	theirView, err := m.ceoRelTheirView(ctx, c.CorpID)
	if err != nil {
		return err
	}
	rows, err := m.relRowsOf(ctx, c.CorpID)
	if err != nil {
		return err
	}

	now := clock.Unix()
	seen := map[int64]bool{}
	for _, f := range targets {
		seen[f.CorpID] = true
		them := ceoRelPartyOf(f, peoples[f.CorpID])
		in := ceoRelInputs{Self: self, Target: them, TheirView: theirView[f.CorpID]}
		switch e1, ok1 := empires[self.Race]; {
		case self.Race != 0 && self.Race == them.Race:
			in.RaceStanding, in.RaceKnown = 10, true
		case ok1 && mxOK:
			if e2, ok2 := empires[them.Race]; ok2 {
				in.RaceStanding, in.RaceKnown = blocStanding(mx, e1.ID, e2.ID), true
			}
		}
		baseline, terms := ceoRelBaseline(in, ch)
		reason := ceoRelReason(terms, baseline)
		if kin[f.CorpID] && baseline < ch.FriendAt {
			// The kin baseline: an alliance-mate is at least a friend for
			// as long as the alliance stands.
			baseline, reason = ch.FriendAt, "our alliance; "+reason
		}
		s, has := rows[f.CorpID]
		if !has {
			if baseline == 0 {
				continue // nothing to say and nothing has happened: no row
			}
			s = relState{Value: baseline, DecayedAt: now, TierAt: now}
		}
		s.PrevValue = s.Value
		s.Baseline = baseline
		s.Reason = reason
		if err := m.relSaveRow(ctx, c.CorpID, f.CorpID, s); err != nil {
			return err
		}
	}
	// A pair whose counterparty dissolved, emptied, or joined our alliance:
	// its baseline goes to 0 so it decays out and the flush drops it.
	for toID, s := range rows {
		if seen[toID] || s.Yielded || s.Baseline == 0 {
			continue
		}
		s.PrevValue, s.Baseline, s.Reason = s.Value, 0, "no longer a corporation we deal with"
		if err := m.relSaveRow(ctx, c.CorpID, toID, s); err != nil {
			return err
		}
	}

	m.relKillboardCatchUp(ctx, c.CorpID, ceoRelWindowDays)
	tierA, err := m.relFoldCorp(ctx, c, ch, b)
	if err != nil {
		return err
	}
	actor := c.CharID
	if a, ok := staffActorOf(ctx); ok && a.CharID != 0 {
		actor = a.CharID
	}

	// The letters (corpletters.go): lapse the unanswered, answer what is on
	// our desk, honour what the other side accepted, write at most one.
	m.expireLetters(ctx, c.CorpID)
	if rows, err = m.relRowsOf(ctx, c.CorpID); err != nil {
		return err
	}
	if err := m.answerPendingLetters(ctx, c, ch, b, rows); err != nil {
		return err
	}
	if err := m.honourLetters(ctx, c, ch); err != nil {
		return err
	}
	// No letters to kin: a pact with an alliance-mate is the alliance.
	if len(kin) > 0 {
		strangers := make(map[int64]relState, len(rows))
		for toID, s := range rows {
			if !kin[toID] {
				strangers[toID] = s
			}
		}
		rows = strangers
	}
	if err := m.proposeLetter(ctx, c, ch, b, self, rows, theirView); err != nil {
		return err
	}

	tierB, err := m.relFlushCorp(ctx, c, ch, actor, true)
	if err != nil {
		return err
	}
	if tierA || tierB {
		// Refresh now: HR, the fleets and the combat ROE read the snapshot, and the
		// ledger line just reported a changed position.
		m.refreshRelations(ctx)
	}
	return m.ceoConsiderAlliance(ctx, c, ch, relAlliesOf(c.CorpID))
}
