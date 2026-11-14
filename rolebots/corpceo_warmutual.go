package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Mutual wars: two sides that both want the fight to go on. The core's flag
// (db/wardb.go SetWarMutual) is one bit either party may flip and a mutual war
// bills nobody; the agreement lives here, as each side's consent row in
// rolebot_war_mutual, and the flag is on while both say yes. A side offers
// (ceoWarMutualConsent) only for a real two-way fight and holds at a lower bar
// than it offered at (mutualHoldFrac). A CEO speaks for its corporation, an
// alliance's executor CEO for an alliance; a bot's consent counts only while a
// think refreshed it within mutualConsentTTL. A human side consents through the
// flag itself.
// See docs/comment-notes-b04.md#mutual-wars.

const (
	// mutualMinAgeSec: a war must have run this long (arming day included) before
	// either side may offer, long enough for a real exchange.
	mutualMinAgeSec = 3 * 86400
	// mutualQuietSec: a mutual war with nothing killed either way in this
	// window has gone quiet, and a side lets it go.
	mutualQuietSec = 7 * 86400
	// mutualHoldFrac: the share of Charter.WarAt a consenting side's motive
	// may fall to before it withdraws. Above warCoolFrac, so mutuality is
	// given up before the declarer's exhaustion retracts outright.
	mutualHoldFrac = 0.65
	// mutualConsentTTL: a bot side's consent row stops counting this long
	// after the last think that weighed it.
	mutualConsentTTL = 3 * 86400
	// mutualLetterEverySec: at most one mutual letter per war in this window,
	// whatever became of the last one.
	mutualLetterEverySec = 7 * 86400
)

// errMutualLapsed is a mutual letter accepted after the offer behind it
// stopped standing.
var errMutualLapsed = errors.New("the offer of a mutual war has lapsed")

// warMutualIn is what one side knows about one of its wars.
type warMutualIn struct {
	Live       bool // armed, not retracted, not finished
	Contract   bool // a mercenary's contract war
	Combat     bool // the side runs a fighting outfit
	Motive     float64
	AgeSec     int64
	Killed     float64 // ISK of the enemy this side destroyed, whole war
	Lost       float64 // ISK this side lost to the enemy, whole war
	RecentKill float64 // ... in the last mutualQuietSec
	RecentLost float64
	Consenting bool  // this side's consent as it stands
	Withdrawn  int64 // seconds since this side last withdrew; <0 = never
}

// ceoWarMutualConsent is whether this side wants the war mutual, and why.
// Pure.
func ceoWarMutualConsent(in warMutualIn, ch Charter) (bool, string) {
	switch {
	case !in.Live:
		return false, "the war is not live"
	case in.Contract:
		return false, "a contract war"
	case !in.Combat:
		return false, "not a fighting outfit"
	case in.Lost > warPainFloorISK && in.Lost > math.Max(in.Killed, 1)*ch.WarPainRatio:
		return false, fmt.Sprintf("losing %s against %s killed", iskString(in.Lost), iskString(in.Killed))
	case in.AgeSec >= mutualQuietSec && in.RecentKill+in.RecentLost <= 0:
		return false, "the war has gone quiet"
	}
	if in.Consenting {
		if in.Motive < ch.WarAt*mutualHoldFrac {
			return false, "the quarrel has cooled"
		}
		return true, "still hot"
	}
	switch {
	case in.Withdrawn >= 0 && in.Withdrawn < ch.DwellSec:
		return false, "withdrew too lately"
	case in.AgeSec < mutualMinAgeSec:
		return false, "too young"
	case in.Motive < ch.WarAt:
		return false, "the quarrel is not hot enough"
	case in.Killed < warPainFloorISK || in.RecentKill <= 0:
		return false, "has not drawn blood"
	}
	return true, fmt.Sprintf("%s killed, %s lost, and wants more", iskString(in.Killed), iskString(in.Lost))
}

// mutualFlagWanted is what the core's flag should read: this side's consent and
// the other's (a bot side's own row, or for a side no bot runs, the flag as it
// stands). Pure.
func mutualFlagWanted(ours, otherBot, otherConsent, flag bool) bool {
	if !ours {
		return false
	}
	if otherBot {
		return otherConsent
	}
	return flag
}

// mutualSide is one consent row.
type mutualSide struct {
	Consent     bool
	CheckedAt   int64
	WithdrawnAt int64
	Found       bool
}

func (m *Mod) mutualSideOf(ctx context.Context, warID, ownerID int64) mutualSide {
	var s mutualSide
	var c int
	err := m.db.QueryRowContext(ctx, `
		SELECT consent, checkedAt, withdrawnAt FROM rolebot_war_mutual
		 WHERE warID = ? AND ownerID = ?`, warID, ownerID).Scan(&c, &s.CheckedAt, &s.WithdrawnAt)
	if err != nil {
		return s
	}
	s.Consent, s.Found = c != 0, true
	return s
}

// mutualSideWrite stores this side's consent. since moves only when the
// consent changes; withdrawnAt only when it goes from yes to no.
func (m *Mod) mutualSideWrite(ctx context.Context, warID, ownerID int64, was mutualSide, consent bool, why string) {
	now := clock.Unix()
	c := 0
	if consent {
		c = 1
	}
	withdrawn := was.WithdrawnAt
	if was.Consent && !consent {
		withdrawn = now
	}
	since := now
	if was.Found && was.Consent == consent {
		_, _ = m.db.ExecContext(ctx,
			`UPDATE rolebot_war_mutual SET checkedAt = ? WHERE warID = ? AND ownerID = ?`, now, warID, ownerID)
		return
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_war_mutual (warID, ownerID, consent, since, checkedAt, withdrawnAt, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE consent = VALUES(consent), since = VALUES(since),
			checkedAt = VALUES(checkedAt), withdrawnAt = VALUES(withdrawnAt), reason = VALUES(reason)`,
		warID, ownerID, c, since, now, withdrawn, truncate(why, 255))
}

// mutualDeciderCorp is the corporation whose CEO speaks for an owner: the
// corporation itself, or an alliance's executor.
func (m *Mod) mutualDeciderCorp(ctx context.Context, ownerID int64) int64 {
	if exec := m.allianceExecutor(ctx, ownerID); exec != 0 {
		return exec
	}
	return ownerID
}

// botRunCorp reports whether a bot CEO runs this corporation and is thinking.
func (m *Mod) botRunCorp(ctx context.Context, corpID int64) bool {
	var paused int
	if err := m.db.QueryRowContext(ctx,
		`SELECT paused FROM rolebot_ceos WHERE corporationID = ?`, corpID).Scan(&paused); err != nil {
		return false
	}
	return paused == 0
}

// ceoWarMutual is one think's pass over every live war this CEO speaks for.
// It runs after ceoWarSettle, so a war that just ended is not weighed.
func (m *Mod) ceoWarMutual(ctx context.Context, c *corpCEO, ch Charter) error {
	hook := modapi.Corp().SetWarMutual
	if hook == nil {
		return nil
	}
	us := c.CorpID
	if aid := m.corpAllianceID(ctx, c.CorpID); aid != 0 {
		if m.allianceExecutor(ctx, aid) != c.CorpID {
			return nil // a member corporation: the executor speaks for the alliance
		}
		us = aid
	}
	on := mutualWarsOn() && ceoWarsOn() && relationsEnabled()
	wars, err := evedb.ListWars(ctx, m.db)
	if err != nil {
		return nil
	}
	_, _ = m.db.ExecContext(ctx, `
		DELETE mu FROM rolebot_war_mutual mu
		  JOIN wars w ON w.warID = mu.warID
		 WHERE mu.ownerID = ? AND w.timeFinished IS NOT NULL`, us)

	now := clock.Unix()
	var cands map[int64]*warCand
	for _, w := range wars {
		if w.TimeFinished.Valid || (w.DeclaredByID != us && w.AgainstID != us) {
			continue
		}
		other := w.AgainstID
		if other == us {
			other = w.DeclaredByID
		}
		was := m.mutualSideOf(ctx, w.WarID, us)
		flag := w.Mutual != 0
		if !on {
			// Switched off: hand back every mutual war this side agreed to; a human's
			// mutual war is not this side's to undo.
			if was.Consent {
				m.mutualSideWrite(ctx, w.WarID, us, was, false, "mutual wars are switched off")
				m.withdrawMutualLetters(ctx, c.CorpID, w.WarID, "mutual wars are switched off")
				if flag {
					m.flipMutual(ctx, c, hook, us, other, w.WarID, false, "mutual wars are switched off")
				}
			}
			continue
		}
		if cands == nil {
			if cands, err = m.ceoWarCandidates(ctx, c, ch); err != nil {
				return err
			}
		}
		motive := 0.0
		for _, x := range cands {
			if x.Owner == other && x.Motive > motive {
				motive = x.Motive
			}
		}
		declared := win32ToUnix(w.TimeDeclared)
		killed, lost := m.warScore(ctx, c.CorpID, other, declared)
		rk, rl := m.warScore(ctx, c.CorpID, other, now-mutualQuietSec)
		withdrawn := int64(-1)
		if was.WithdrawnAt != 0 {
			withdrawn = now - was.WithdrawnAt
		}
		want, why := ceoWarMutualConsent(warMutualIn{
			Live:       w.HostileNow && !w.Retracted.Valid,
			Contract:   m.contractWar(ctx, w.WarID),
			Combat:     ceoCombatOriented(ch),
			Motive:     motive,
			AgeSec:     now - declared,
			Killed:     killed,
			Lost:       lost,
			RecentKill: rk,
			RecentLost: rl,
			Consenting: was.Consent,
			Withdrawn:  withdrawn,
		}, ch)
		m.mutualSideWrite(ctx, w.WarID, us, was, want, why)
		if want && !was.Consent {
			m.ceoLog(ctx, c, powerWar, "mutualoffer", other, 0, "would take the war with %s mutual — %s",
				ownerNameOf(ctx, m.db, other), why)
		}

		otherCorp := m.mutualDeciderCorp(ctx, other)
		otherBot := m.botRunCorp(ctx, otherCorp)
		if !want && was.Consent {
			m.withdrawMutualLetters(ctx, c.CorpID, w.WarID, why)
		}
		if want && !otherBot && !flag {
			m.offerMutualLetter(ctx, c, w.WarID, otherCorp)
		}
		theirs := m.mutualSideOf(ctx, w.WarID, other)
		otherConsent := theirs.Consent && now-theirs.CheckedAt <= mutualConsentTTL
		target := mutualFlagWanted(want, otherBot, otherConsent, flag)
		if target == flag {
			if !want && was.Consent && !flag {
				m.ceoLog(ctx, c, powerWar, "unmutual", other, 0, "withdrew its offer of a mutual war with %s — %s",
					ownerNameOf(ctx, m.db, other), why)
			}
			continue
		}
		reason := why
		if !want && flag && !was.Consent {
			reason = "declined a mutual war it never agreed to"
		} else if want && !target {
			reason = fmt.Sprintf("%s no longer agrees", ownerNameOf(ctx, m.db, other))
		}
		m.flipMutual(ctx, c, hook, us, other, w.WarID, target, reason)
	}
	return nil
}

// flipMutual sets the core's flag and records it.
func (m *Mod) flipMutual(ctx context.Context, c *corpCEO, hook func(int64, int64, bool) error,
	us, other, warID int64, mutual bool, why string) {
	if err := hook(us, warID, mutual); err != nil {
		m.logBot(c.CharID, logInfo, "ceo", "war: mutual=%v on %d refused: %v", mutual, warID, err)
		return
	}
	if mutual {
		m.ceoLog(ctx, c, powerWar, "mutual", other, 0, "made the war with %s mutual — %s",
			ownerNameOf(ctx, m.db, other), why)
		return
	}
	m.ceoLog(ctx, c, powerWar, "unmutual", other, 0, "ended mutuality in the war with %s — %s",
		ownerNameOf(ctx, m.db, other), why)
}

// contractWar reports whether a war is a mercenary's contract war.
func (m *Mod) contractWar(ctx context.Context, warID int64) bool {
	var id sql.NullInt64
	_ = m.db.QueryRowContext(ctx,
		`SELECT contractID FROM rolebot_ceo_wars WHERE warID = ? AND endedAt = 0`, warID).Scan(&id)
	return id.Valid && id.Int64 != 0
}

// ── the letter to a human side ──────────────────────────────────────────────

// offerMutualLetter puts a mutual-war letter on a human corporation's desk,
// paced to one per war per mutualLetterEverySec.
func (m *Mod) offerMutualLetter(ctx context.Context, c *corpCEO, warID, toCorp int64) {
	if !relLettersOn() || toCorp == 0 {
		return
	}
	now := clock.Unix()
	var n int
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_letters
		 WHERE fromCorpID = ? AND kind = ? AND warID = ?
		   AND (status IN (?, ?, ?, ?) OR draftedAt > ?)`,
		c.CorpID, letterMutual, warID, letterDrafted, letterSent, letterDelivered, letterAccepted,
		now-mutualLetterEverySec).Scan(&n); err != nil || n > 0 {
		return
	}
	toName := corpNameOf(ctx, m.db, toCorp)
	actor := c.CharID
	if a, ok := staffActorOf(ctx); ok && a.CharID != 0 {
		actor = a.CharID
	}
	l := &corpLetter{
		FromCorpID: c.CorpID, ToCorpID: toCorp, Kind: letterMutual, WarID: warID,
		Status: letterDrafted, PostedBy: actor, DraftedAt: now, ExpiresAt: now + letterExpireSec,
		Terms: letterTerms(letterMutual, corpNameOf(ctx, m.db, c.CorpID), toName),
	}
	if err := m.insertLetter(ctx, l); err != nil {
		m.logBot(c.CharID, logInfo, "ceo", "war: mutual letter to %s: %v", toName, err)
		return
	}
	m.ceoLog(ctx, c, powerWar, "mutualletter", toCorp, 0, "wrote to %s proposing to make the war mutual", toName)
	m.deliverLetter(ctx, l, c.CharID)
}

// withdrawMutualLetters pulls every open mutual letter this corporation has
// out for a war.
func (m *Mod) withdrawMutualLetters(ctx context.Context, fromCorp, warID int64, why string) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_letters SET status = ?, answeredAt = ?, reason = ?
		 WHERE fromCorpID = ? AND kind = ? AND warID = ? AND status IN (?, ?, ?)`,
		letterWithdrawn, clock.Unix(), truncate("withdrawn: "+why, 255),
		fromCorp, letterMutual, warID, letterDrafted, letterSent, letterDelivered)
}

// mutualLetterLive checks a mutual letter can still be accepted (the war is live
// and unretracted, the sender a party to it and still consenting) and returns
// the answering side's owner, on whose behalf the flag is set.
func (m *Mod) mutualLetterLive(ctx context.Context, l *corpLetter) (int64, error) {
	w, live := warActive(ctx, m.db, l.WarID)
	if !live || w.Retracted.Valid {
		return 0, fmt.Errorf("%w: the war is over", errMutualLapsed)
	}
	from := l.FromCorpID
	if aid := m.corpAllianceID(ctx, l.FromCorpID); aid != 0 {
		from = aid
	}
	var to int64
	switch from {
	case w.DeclaredByID:
		to = w.AgainstID
	case w.AgainstID:
		to = w.DeclaredByID
	default:
		return 0, fmt.Errorf("%w: they are no longer a party to the war", errMutualLapsed)
	}
	s := m.mutualSideOf(ctx, l.WarID, from)
	if !s.Consent || clock.Unix()-s.CheckedAt > mutualConsentTTL {
		return 0, fmt.Errorf("%w: they no longer want it", errMutualLapsed)
	}
	return to, nil
}

// acceptMutualLetter sets the flag on the answering side's behalf.
func (m *Mod) acceptMutualLetter(ctx context.Context, l *corpLetter, owner int64) {
	hook := modapi.Corp().SetWarMutual
	if hook == nil || owner == 0 {
		return
	}
	if w, live := warActive(ctx, m.db, l.WarID); !live || w.Mutual != 0 {
		return
	}
	if err := hook(owner, l.WarID, true); err != nil {
		rlogf("rolebots: mutual letter %d: set war %d mutual: %v", l.ID, l.WarID, err)
	}
}
