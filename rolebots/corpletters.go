package rolebots

// Letters: the active half of diplomacy. A corporation that wants something from another (a non-aggression
// pact after a quarrel, an alliance with an ally, reparations for a member it killed, peace in a losing war)
// writes a letter, one rolebot_corp_letters row. An envoy carries it (corpenvoy.go) or, with envoys off, it
// goes straight to the other corporation's desk. The other side answers at its next diplomacy pass (a bot) or
// on the Corporate Portal (a human). An accepted pact is a rolebot_corp_pacts row and a floor under both
// directions of the pair for its term.
// What decides is pure (relAnswerLetter, relProposalScore); what moves ISK runs under corpOpLock in the
// sender's own pass. Letters are paced: at most one per Charter.ProposalEverySec (a day to a week, by
// patience), scaled by the director's appetite.

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// Letter kinds.
const (
	letterNAP         = "nap"
	letterAlliance    = "alliance"
	letterReparations = "reparations"
	letterPeace       = "peace"
	// letterMutual asks a side no bot runs to make a war mutual
	// (corpceo_warmutual.go). Bot sides agree through their consent rows and
	// are never written to.
	letterMutual = "mutual"
)

// Letter states.
const (
	letterDrafted   = "drafted"   // written, not yet on its way
	letterSent      = "sent"      // with an envoy
	letterDelivered = "delivered" // on the other side's desk
	letterAccepted  = "accepted"  // yes: the sender still has to honour it
	letterRejected  = "rejected"
	letterLost      = "lost" // the envoy died on the road
	letterExpired   = "expired"
	letterWithdrawn = "withdrawn"
	letterHonoured  = "honoured" // reparations paid / the war retracted
)

const (
	// letterExpireSec is how long an unanswered letter stands.
	letterExpireSec = 3 * 86400
	// letterQuarrelDays is the window a NAP proposal counts quarrels in, and
	// the window reparations look back over for a kill to pay for.
	letterQuarrelDays = 7
	// letterNAPQuarrels: a NAP is proposed over at least this many hostile incidents in the window. One shot is
	// not a quarrel worth a letter.
	letterNAPQuarrels = 2
	// letterReparationShare is what a lawful corporation offers for a kill
	// its member made: half the loss.
	letterReparationShare = 0.5
	// pactFloorNAP / pactFloorAlliance are the floors a pact puts under the
	// pair: a NAP keeps it out of rival (−2.4 sits just inside the line), an
	// alliance keeps it an ally.
	pactFloorNAP      = -2.4
	pactFloorAlliance = StandingDevoted
	// letterPeaceHostilityMax: a peace is accepted only while the other side has killed less than this (kill
	// units) in the window, so a peace offered mid-slaughter is refused.
	letterPeaceHostilityMax = 2.0
)

// corpLetter is one rolebot_corp_letters row.
type corpLetter struct {
	ID          int64
	FromCorpID  int64
	ToCorpID    int64
	Kind        string
	Terms       string
	ISK         float64
	TermDays    int
	WarID       int64
	Status      string
	EnvoyCharID int64
	PostedBy    int64
	DraftedAt   int64
	SentAt      int64
	DeliveredAt int64
	AnsweredAt  int64
	AnsweredBy  int64
	ExpiresAt   int64
	MailID      int64
	Reason      string
}

const letterCols = `letterID, fromCorpID, toCorpID, kind, terms, isk, termDays, warID, status,
	envoyCharID, postedBy, draftedAt, sentAt, deliveredAt, answeredAt, answeredBy, expiresAt, mailID, reason`

func scanLetter(sc interface{ Scan(...any) error }) (*corpLetter, error) {
	var l corpLetter
	err := sc.Scan(&l.ID, &l.FromCorpID, &l.ToCorpID, &l.Kind, &l.Terms, &l.ISK, &l.TermDays, &l.WarID,
		&l.Status, &l.EnvoyCharID, &l.PostedBy, &l.DraftedAt, &l.SentAt, &l.DeliveredAt, &l.AnsweredAt,
		&l.AnsweredBy, &l.ExpiresAt, &l.MailID, &l.Reason)
	return &l, err
}

// letterKindWords is how a letter's kind reads in a line.
func letterKindWords(kind string) string {
	switch kind {
	case letterNAP:
		return "a non-aggression pact"
	case letterAlliance:
		return "an alliance of friendship"
	case letterReparations:
		return "reparations"
	case letterPeace:
		return "peace"
	case letterMutual:
		return "a mutual war"
	}
	return kind
}

// ── the pure half ───────────────────────────────────────────────────────────

// letterAnswerIn is what the receiving corporation knows when it answers.
type letterAnswerIn struct {
	Kind            string
	Value, Baseline float64 // what we think of the sender
	RecentHostility float64 // kill units they did to us in the window
	LostISK         float64 // what their kills cost us in the window
	OfferISK        float64 // reparations offered
	AtWar           bool
}

// relAnswerLetter is the receiving CEO's yes or no, and why. Pure.
func relAnswerLetter(in letterAnswerIn, ch Charter, b diploBend) (bool, string) {
	b = b.norm()
	if in.Kind == letterMutual {
		// Only ever sent to a side no bot runs; a bot side's own war desk
		// weighs mutuality (ceoWarMutualConsent), never a letter.
		return false, "our war desk weighs that itself"
	}
	// Nothing from a corporation below the rival line, except peace, which only an enemy offers and which has its
	// own line (EnemyAt).
	if in.Value <= ch.RivalAt && in.Kind != letterPeace {
		return false, "we do not treat with them"
	}
	switch in.Kind {
	case letterNAP:
		if in.AtWar {
			return false, "there is a war between us"
		}
		if in.Value >= -1 && in.Baseline >= -2 {
			return true, "a quarrel not worth continuing"
		}
		return false, "too much between us for a pact"
	case letterAlliance:
		if in.Value >= ch.AllyAt-b.Hyst*ch.Hysteresis && in.Baseline >= 0 {
			return true, "friends already"
		}
		return false, "not close enough for an alliance"
	case letterReparations:
		if in.OfferISK <= 0 {
			return false, "an empty offer"
		}
		if in.LostISK <= 0 || in.OfferISK >= letterReparationShare*in.LostISK {
			return true, "a fair settlement"
		}
		return false, fmt.Sprintf("%s offered against %s lost", iskString(in.OfferISK), iskString(in.LostISK))
	case letterPeace:
		if !in.AtWar {
			return false, "there is no war to end"
		}
		if in.Value > ch.EnemyAt && in.RecentHostility <= letterPeaceHostilityMax {
			return true, "enough blood spilt"
		}
		return false, "the war goes on"
	}
	return false, "an unknown proposal"
}

// letterProposalIn is what the sending corporation knows about one
// counterparty when it looks for a letter to write.
type letterProposalIn struct {
	ToID                        int64
	Value, TheirValue, Baseline float64
	HostileRecent               int     // hostile incidents between us in the window, either way
	OurKillISK                  float64 // what our members' kills cost them in the window
	AtWar, Losing               bool
	HasPact, Lawful, CanPayRep  bool
}

// relProposalScore is which letter, if any, this pair calls for, how badly,
// and what it offers. Pure. Score 0 = no letter.
func relProposalScore(in letterProposalIn, ch Charter, b diploBend) (kind string, score, isk float64) {
	b = b.norm()
	consider := func(k string, s, offer float64) {
		if s > score {
			kind, score, isk = k, s, offer
		}
	}
	if !in.HasPact && !in.AtWar && in.Value >= ch.AllyAt && in.TheirValue >= ch.AllyAt {
		consider(letterAlliance, 3+(in.Value-ch.AllyAt), 0)
	}
	if !in.HasPact && !in.AtWar && in.Value >= pactFloorNAP && in.Value < ch.FriendAt &&
		in.Baseline >= -1 && in.HostileRecent >= letterNAPQuarrels {
		consider(letterNAP, 2+0.2*float64(in.HostileRecent), 0)
	}
	// Reparations outrank a pact: paying for one kill is the specific remedy, and a quarrel of five or more
	// shots is what lets the pact win.
	if in.Lawful && !in.AtWar && in.OurKillISK > 0 && in.Value >= 0 && in.CanPayRep {
		consider(letterReparations, 3.0, math.Round(letterReparationShare*in.OurKillISK))
	}
	if in.AtWar && (in.Losing || in.Value > ch.RivalAt) {
		s := 1.5
		if in.Losing {
			s += 1
		}
		consider(letterPeace, s, 0)
	}
	return kind, score * b.Appetite, isk
}

// pactFloorFor is the floor a pact of this kind puts under the pair.
func pactFloorFor(kind string) float64 {
	if kind == letterAlliance {
		return pactFloorAlliance
	}
	return pactFloorNAP
}

// ── the store ───────────────────────────────────────────────────────────────

func (m *Mod) lettersWhere(ctx context.Context, where string, args ...any) ([]*corpLetter, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+letterCols+` FROM rolebot_corp_letters WHERE `+where+` ORDER BY letterID`, args...)
	if err != nil {
		return nil, fmt.Errorf("letters: %w", err)
	}
	defer rows.Close()
	var out []*corpLetter
	for rows.Next() {
		l, err := scanLetter(rows)
		if err != nil {
			return nil, fmt.Errorf("letters scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (m *Mod) letterByID(ctx context.Context, id int64) (*corpLetter, error) {
	ls, err := m.lettersWhere(ctx, `letterID = ?`, id)
	if err != nil || len(ls) == 0 {
		return nil, err
	}
	return ls[0], nil
}

func (m *Mod) insertLetter(ctx context.Context, l *corpLetter) error {
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_letters
			(fromCorpID, toCorpID, kind, terms, isk, termDays, warID, status, postedBy, draftedAt, expiresAt, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.FromCorpID, l.ToCorpID, l.Kind, truncate(l.Terms, 255), l.ISK, l.TermDays, l.WarID, l.Status,
		l.PostedBy, l.DraftedAt, l.ExpiresAt, truncate(l.Reason, 255))
	if err != nil {
		return fmt.Errorf("insertLetter: %w", err)
	}
	l.ID, _ = res.LastInsertId()
	return nil
}

func (m *Mod) setLetterStatus(ctx context.Context, id int64, status string, set string, args ...any) error {
	q := `UPDATE rolebot_corp_letters SET status = ?`
	if set != "" {
		q += ", " + set
	}
	q += ` WHERE letterID = ?`
	all := append([]any{status}, args...)
	all = append(all, id)
	if _, err := m.db.ExecContext(ctx, q, all...); err != nil {
		return fmt.Errorf("setLetterStatus %s: %w", status, err)
	}
	return nil
}

// standingPact is the live pact between two corporations, if any.
func (m *Mod) standingPact(ctx context.Context, a, b int64) (kind string, floor float64, until int64, ok bool) {
	if a > b {
		a, b = b, a
	}
	err := m.db.QueryRowContext(ctx, `
		SELECT kind, floorValue, endAt FROM rolebot_corp_pacts
		 WHERE corpA = ? AND corpB = ? AND status = 'standing' AND endAt > ? ORDER BY endAt DESC LIMIT 1`,
		a, b, clock.Unix()).Scan(&kind, &floor, &until)
	if err != nil {
		return "", 0, 0, false
	}
	return kind, floor, until, true
}

// createPact writes the pact and puts its floor under both directions of
// the pair. A direction with no relation row gets one at the floor.
func (m *Mod) createPact(ctx context.Context, l *corpLetter, now int64) error {
	a, b := l.FromCorpID, l.ToCorpID
	if a > b {
		a, b = b, a
	}
	days := l.TermDays
	if days <= 0 {
		days = relPactMaxDays()
	}
	floor, until := pactFloorFor(l.Kind), now+int64(days)*86400
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_pacts (letterID, corpA, corpB, kind, floorValue, startAt, endAt, status, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'standing', ?)`,
		l.ID, a, b, l.Kind, floor, now, until, truncate(letterKindWords(l.Kind)+" by letter", 255)); err != nil {
		return fmt.Errorf("createPact: %w", err)
	}
	for _, pair := range [][2]int64{{l.FromCorpID, l.ToCorpID}, {l.ToCorpID, l.FromCorpID}} {
		if _, managed := managedCEO(pair[0]); !managed {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_relations (corporationID, toID, value, baseline, prevValue, written, tier, tierAt, decayedAt, floorValue, floorUntil)
			VALUES (?, ?, ?, 0, ?, 0, 0, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE floorValue = VALUES(floorValue), floorUntil = VALUES(floorUntil),
				value = GREATEST(value, VALUES(floorValue))`,
			pair[0], pair[1], math.Max(0, floor), math.Max(0, floor), now, now, floor, until); err != nil {
			return fmt.Errorf("createPact floor %d → %d: %w", pair[0], pair[1], err)
		}
	}
	return nil
}

// letterSignals is, per counterparty, what this corporation's ledger says happened lately: hostile incidents
// in the quarrel window, what our kills cost them and what theirs cost us. It is one grouped query with
// literal integer windows, since a bind parameter inside a grouped CASE buckets wrong on MariaDB.
type letterSignal struct {
	Hostile    int
	OurKillISK float64
	LostISK    float64
}

func (m *Mod) letterSignals(ctx context.Context, corpID int64) map[int64]letterSignal {
	out := map[int64]letterSignal{}
	now := clock.Unix()
	q := fmt.Sprintf(`
		SELECT toID,
		       SUM(CASE WHEN weight < 0 AND kind <> 'crowd' AND at >= %d THEN 1 ELSE 0 END),
		       SUM(CASE WHEN side = 'aggressor' AND kind IN ('kill', 'pod') AND at >= %d THEN isk ELSE 0 END),
		       SUM(CASE WHEN side = 'victim' AND kind IN ('kill', 'pod') THEN isk ELSE 0 END)
		  FROM rolebot_corp_incidents
		 WHERE corporationID = ? AND at >= %d
		 GROUP BY toID`,
		now-letterQuarrelDays*86400, now-letterQuarrelDays*86400, now-ceoRelWindowDays*86400)
	rows, err := m.db.QueryContext(ctx, q, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var s letterSignal
		if rows.Scan(&id, &s.Hostile, &s.OurKillISK, &s.LostISK) == nil {
			out[id] = s
		}
	}
	return out
}

// ── delivery ────────────────────────────────────────────────────────────────

// dispatchLetter puts a drafted letter on its way: with an envoy when the
// corporation can field one (corpenvoy.go), straight to the other desk
// otherwise. Never fails the pass.
func (m *Mod) dispatchLetter(ctx context.Context, c *corpCEO, l *corpLetter) {
	if relEnvoysOn() && m.dispatchEnvoy(ctx, c, l) {
		return
	}
	m.deliverLetter(ctx, l, c.CharID)
}

// deliverLetter lands a letter on the receiving desk: status delivered, and
// a mail to the receiving CEO (a bot's is suppressed and costs nothing; a
// human's rings). The bot answers at its next pass; the human on the
// Corporate Portal.
func (m *Mod) deliverLetter(ctx context.Context, l *corpLetter, carrier int64) {
	now := clock.Unix()
	if err := m.setLetterStatus(ctx, l.ID, letterDelivered, `deliveredAt = ?, sentAt = IF(sentAt = 0, ?, sentAt)`, now, now); err != nil {
		rlogf("rolebots: deliver letter %d: %v", l.ID, err)
		return
	}
	l.Status, l.DeliveredAt = letterDelivered, now
	to, err := getAnyCorpFacts(ctx, m.db, l.ToCorpID)
	if err != nil || to == nil || to.CEOID == 0 {
		return
	}
	fromName := corpNameOf(ctx, m.db, l.FromCorpID)
	subject := fmt.Sprintf("%s proposes %s", fromName, letterKindWords(l.Kind))
	body := fmt.Sprintf("%s<br><br>%s<br><br>Answer it on %s.%s",
		l.Terms, letterOfferWords(l), igbLink(fmt.Sprintf("/corp/relations?letter=%d", l.ID), "the Corporate Portal"),
		m.signature(ctx, l.PostedBy, "Diplomacy", fromName))
	if id := m.mailPilot(ctx, to.CEOID, carrier, subject, body); id != 0 {
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_letters SET mailID = ? WHERE letterID = ?`, id, l.ID)
	}
}

// letterOfferWords is the letter's concrete offer, for the mail.
func letterOfferWords(l *corpLetter) string {
	switch l.Kind {
	case letterReparations:
		return "We offer " + iskString(l.ISK) + " in settlement."
	case letterAlliance:
		return fmt.Sprintf("We propose to stand as allies for %d days.", l.TermDays)
	case letterNAP:
		return fmt.Sprintf("We propose that neither of us fires on the other for %d days.", l.TermDays)
	case letterPeace:
		return "We propose to end the war between us."
	case letterMutual:
		return "We propose to make the war between us mutual: neither side pays CONCORD to keep it going, " +
			"and it lasts until one of us withdraws."
	}
	return ""
}

// ── answering ───────────────────────────────────────────────────────────────

// answerLetter is the yes-or-no on a delivered letter, shared by the bot pass and the Corporate Portal so both
// write the same rows: status, pact and floors, incidents, ledger line and, for a peace, the retraction of the
// answering corporation's war on the sender.
func (m *Mod) answerLetter(ctx context.Context, l *corpLetter, byCharID int64, accept bool, why string) error {
	if l.Status != letterDelivered {
		return fmt.Errorf("letter %d is %s, not awaiting an answer", l.ID, l.Status)
	}
	var mutualOwner int64
	if accept && l.Kind == letterMutual {
		owner, err := m.mutualLetterLive(ctx, l)
		if err != nil {
			_ = m.setLetterStatus(ctx, l.ID, letterWithdrawn, `answeredAt = ?, reason = ?`, clock.Unix(), truncate(err.Error(), 255))
			l.Status = letterWithdrawn
			return err
		}
		mutualOwner = owner
	}
	now := clock.Unix()
	status := letterRejected
	if accept {
		status = letterAccepted
	}
	if err := m.setLetterStatus(ctx, l.ID, status, `answeredAt = ?, answeredBy = ?, reason = ?`, now, byCharID, truncate(why, 255)); err != nil {
		return err
	}
	l.Status, l.AnsweredAt, l.AnsweredBy, l.Reason = status, now, byCharID, why
	to, _ := getCEO(ctx, m.db, l.ToCorpID)
	fromName := corpNameOf(ctx, m.db, l.FromCorpID)
	if !accept {
		if to != nil {
			m.ceoLog(ctx, to, powerDiplomacy, "answer", l.FromCorpID, 0,
				"declined %s's proposal of %s — %s", fromName, letterKindWords(l.Kind), why)
		}
		return nil
	}
	switch l.Kind {
	case letterNAP, letterAlliance:
		if err := m.createPact(ctx, l, now); err != nil {
			return err
		}
		m.noteCorpIncidentMutual(ctx, l.FromCorpID, l.ToCorpID, incPact, 0, 0, l.ID,
			fmt.Sprintf("pact:%d", l.ID), letterKindWords(l.Kind)+" with "+fromName)
	case letterPeace:
		// Our own war on them ends now; theirs ends when they honour the
		// answer (honourLetters). Then a pact keeps the peace.
		m.retractOwnWarOn(ctx, to, l.FromCorpID)
		if err := m.createPact(ctx, l, now); err != nil {
			return err
		}
		m.noteCorpIncidentMutual(ctx, l.FromCorpID, l.ToCorpID, incPeace, 0, 0, l.ID,
			fmt.Sprintf("peace:%d", l.ID), "peace with "+fromName)
	case letterReparations:
		// Accepted; the money moves in the sender's pass (honourLetters).
	case letterMutual:
		// The flag goes on now, on the answering side's behalf; the sender's
		// war desk keeps it there while it still consents.
		m.acceptMutualLetter(ctx, l, mutualOwner)
	}
	if to != nil {
		m.ceoLog(ctx, to, powerDiplomacy, "pact", l.FromCorpID, l.ISK,
			"accepted %s's proposal of %s — %s", fromName, letterKindWords(l.Kind), why)
	}
	return nil
}

// retractOwnWarOn retracts every war in this corporation's own book against
// a corporation (or its alliance). A war a human declared is not in the book
// and is not touched.
func (m *Mod) retractOwnWarOn(ctx context.Context, c *corpCEO, otherCorp int64) {
	if c == nil {
		return
	}
	book, err := m.ceoWarBook(ctx, c.CorpID)
	if err != nil {
		return
	}
	otherAlly := m.corpAllianceID(ctx, otherCorp)
	for _, b := range book {
		if b.Target != otherCorp && (otherAlly == 0 || b.Target != otherAlly) {
			continue
		}
		if err := m.retractWar(ctx, b.owner(), b.WarID); err != nil {
			m.logBot(c.CharID, logError, "ceo", "retracting war %d for peace: %v", b.WarID, err)
			continue
		}
		m.ceoWarBookEnd(ctx, b.WarID, "peace")
		m.ceoLog(ctx, c, powerWar, "retract", b.Target, 0, "retracted the war on %s — peace", corpNameOf(ctx, m.db, otherCorp))
	}
}

// answerPendingLetters is the receiving side of the pass: every letter on
// this corporation's desk is answered by the pure rule.
func (m *Mod) answerPendingLetters(ctx context.Context, c *corpCEO, ch Charter, b diploBend, rows map[int64]relState) error {
	ls, err := m.lettersWhere(ctx, `toCorpID = ? AND status = ?`, c.CorpID, letterDelivered)
	if err != nil || len(ls) == 0 {
		return err
	}
	signals := m.letterSignals(ctx, c.CorpID)
	hostility := m.relRecentHostility(ctx, c.CorpID, ceoRelWindowDays*86400)
	actor := c.CharID
	if a, ok := staffActorOf(ctx); ok && a.CharID != 0 {
		actor = a.CharID
	}
	for _, l := range ls {
		s := rows[l.FromCorpID]
		in := letterAnswerIn{
			Kind: l.Kind, Value: s.Value, Baseline: s.Baseline,
			RecentHostility: hostility[l.FromCorpID],
			LostISK:         signals[l.FromCorpID].LostISK,
			OfferISK:        l.ISK,
			AtWar:           relAtWar(c.CorpID, m.corpAllianceID(ctx, c.CorpID), l.FromCorpID, m.corpAllianceID(ctx, l.FromCorpID)),
		}
		accept, why := relAnswerLetter(in, ch, b)
		if err := m.answerLetter(ctx, l, actor, accept, why); err != nil {
			return err
		}
	}
	return nil
}

// ── honouring ───────────────────────────────────────────────────────────────

// honourLetters is the sending side's follow-through on an accepted letter:
// reparations are paid (the one ISK move in the pass, under the lock and
// the chest), a peace retracts our own war. Both end in status honoured.
func (m *Mod) honourLetters(ctx context.Context, c *corpCEO, ch Charter) error {
	ls, err := m.lettersWhere(ctx, `fromCorpID = ? AND status = ?`, c.CorpID, letterAccepted)
	if err != nil {
		return err
	}
	for _, l := range ls {
		toName := corpNameOf(ctx, m.db, l.ToCorpID)
		switch l.Kind {
		case letterReparations:
			if l.ISK <= 0 {
				_ = m.setLetterStatus(ctx, l.ID, letterHonoured, "")
				continue
			}
			if !m.canPay(ctx, c.CorpID, ch, l.ISK, 0) {
				continue // next pass; the letter stays accepted
			}
			if _, _, err := evedb.TransferISK(ctx, m.db, c.CorpID, l.ToCorpID, l.ISK, refPlayerDonation,
				evedb.JournalReason("Reparations to "+toName)); err != nil {
				m.logBot(c.CharID, logError, "ceo", "paying reparations to %s: %v", toName, err)
				continue
			}
			if err := m.setLetterStatus(ctx, l.ID, letterHonoured, ""); err != nil {
				return err
			}
			p := relParamsNow()
			m.noteCorpIncident(ctx, l.ToCorpID, c.CorpID, incReparation, relSideVictim,
				relBaseWeight(incReparation, relSideVictim, l.ISK, p), l.ISK, 0, l.ID,
				fmt.Sprintf("reparation:%d", l.ID), "reparations from "+corpNameOf(ctx, m.db, c.CorpID))
			m.noteCorpIncident(ctx, c.CorpID, l.ToCorpID, incReparation, relSideAggressor,
				relBaseWeight(incReparation, relSideAggressor, l.ISK, p), l.ISK, 0, l.ID,
				fmt.Sprintf("reparation:%d", l.ID), "paid reparations to "+toName)
			m.ceoLog(ctx, c, powerDiplomacy, "reparations", l.ToCorpID, l.ISK,
				"paid %s to %s in reparations", iskString(l.ISK), toName)
		case letterPeace:
			m.retractOwnWarOn(ctx, c, l.ToCorpID)
			if err := m.setLetterStatus(ctx, l.ID, letterHonoured, ""); err != nil {
				return err
			}
			m.ceoLog(ctx, c, powerDiplomacy, "peace", l.ToCorpID, 0, "made peace with %s", toName)
		case letterMutual:
			if err := m.setLetterStatus(ctx, l.ID, letterHonoured, ""); err != nil {
				return err
			}
			m.ceoLog(ctx, c, powerWar, "mutual", l.ToCorpID, 0, "%s agreed to make the war mutual", toName)
		default:
			if err := m.setLetterStatus(ctx, l.ID, letterHonoured, ""); err != nil {
				return err
			}
			m.ceoLog(ctx, c, powerDiplomacy, "pact", l.ToCorpID, 0, "%s accepted %s", toName, letterKindWords(l.Kind))
		}
	}
	return nil
}

// ── proposing ───────────────────────────────────────────────────────────────

// proposeLetter writes at most one letter per pass, and none inside the
// charter's pacing. Candidates are every pair on the ledger; the pure score
// picks the kind.
func (m *Mod) proposeLetter(ctx context.Context, c *corpCEO, ch Charter, b diploBend, self ceoRelParty,
	rows map[int64]relState, theirView map[int64]float64) error {
	if !relLettersOn() {
		return nil
	}
	b = b.norm()
	now := clock.Unix()
	every := int64(float64(ch.ProposalEverySec) / b.Appetite)
	if now-m.lastCEOActionAt(ctx, c.CorpID, "letter") < every {
		return nil
	}
	signals := m.letterSignals(ctx, c.CorpID)
	selfAlly := m.corpAllianceID(ctx, c.CorpID)
	bestKind, bestScore, bestISK := "", 0.0, 0.0
	var bestTo int64
	for toID, s := range rows {
		if s.Yielded {
			continue
		}
		// One open letter per counterparty at a time.
		if open, _ := m.lettersWhere(ctx, `fromCorpID = ? AND toCorpID = ? AND status IN (?, ?, ?, ?)`,
			c.CorpID, toID, letterDrafted, letterSent, letterDelivered, letterAccepted); len(open) > 0 {
			continue
		}
		_, _, _, hasPact := m.standingPact(ctx, c.CorpID, toID)
		sig := signals[toID]
		atWar := relAtWar(c.CorpID, selfAlly, toID, m.corpAllianceID(ctx, toID))
		losing := false
		if atWar {
			killed, lost := m.warScore(ctx, c.CorpID, toID, now-ceoRelWindowDays*86400)
			losing = lost > killed
		}
		in := letterProposalIn{
			ToID: toID, Value: s.Value, TheirValue: theirView[toID], Baseline: s.Baseline,
			HostileRecent: sig.Hostile, OurKillISK: sig.OurKillISK,
			AtWar: atWar, Losing: losing, HasPact: hasPact, Lawful: !self.outlaw(),
		}
		if in.OurKillISK > 0 {
			in.CanPayRep = m.canPay(ctx, c.CorpID, ch, letterReparationShare*in.OurKillISK, 0)
		}
		kind, score, isk := relProposalScore(in, ch, b)
		if score > bestScore {
			bestKind, bestScore, bestISK, bestTo = kind, score, isk, toID
		}
	}
	if bestKind == "" {
		return nil
	}
	toName := corpNameOf(ctx, m.db, bestTo)
	ourName := corpNameOf(ctx, m.db, c.CorpID)
	actor := c.CharID
	if a, ok := staffActorOf(ctx); ok && a.CharID != 0 {
		actor = a.CharID
	}
	l := &corpLetter{
		FromCorpID: c.CorpID, ToCorpID: bestTo, Kind: bestKind, ISK: bestISK,
		TermDays: relPactMaxDays(), Status: letterDrafted, PostedBy: actor,
		DraftedAt: now, ExpiresAt: now + letterExpireSec,
		Terms: letterTerms(bestKind, ourName, toName),
	}
	if err := m.insertLetter(ctx, l); err != nil {
		return err
	}
	m.ceoLog(ctx, c, powerDiplomacy, "letter", bestTo, bestISK,
		"wrote to %s proposing %s", toName, letterKindWords(bestKind))
	m.dispatchLetter(ctx, c, l)
	return nil
}

// letterTerms is the letter's opening line.
func letterTerms(kind, from, to string) string {
	switch kind {
	case letterNAP:
		return fmt.Sprintf("To %s, from %s: we have quarrelled enough. Let our pilots pass each other in peace.", to, from)
	case letterAlliance:
		return fmt.Sprintf("To %s, from %s: we have stood together long enough to call it what it is.", to, from)
	case letterReparations:
		return fmt.Sprintf("To %s, from %s: one of ours killed one of yours. We would rather pay than feud.", to, from)
	case letterPeace:
		return fmt.Sprintf("To %s, from %s: this war has cost us both enough.", to, from)
	case letterMutual:
		return fmt.Sprintf("To %s, from %s: neither of us is done with this war. Let us stop paying CONCORD to keep it going.", to, from)
	}
	return ""
}

// expireLetters lapses letters nobody answered and clears their envoys.
func (m *Mod) expireLetters(ctx context.Context, corpID int64) {
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_letters SET status = ?, answeredAt = ?
		 WHERE fromCorpID = ? AND status IN (?, ?, ?) AND expiresAt > 0 AND expiresAt < ?`,
		letterExpired, now, corpID, letterDrafted, letterSent, letterDelivered, now); err != nil {
		rlogf("rolebots: expire letters corp %d: %v", corpID, err)
	}
	// An envoy whose letter lapsed on the road hands its seat back.
	m.clearEnvoySeats(ctx, corpID)
}

// openLettersFor is the panel's read: letters to or from a corporation that
// are not closed, plus the standing pacts.
func (m *Mod) openLettersFor(ctx context.Context, corpID int64) ([]*corpLetter, error) {
	return m.lettersWhere(ctx, `(fromCorpID = ? OR toCorpID = ?) AND status IN (?, ?, ?, ?)`,
		corpID, corpID, letterDrafted, letterSent, letterDelivered, letterAccepted)
}

var _ = sql.ErrNoRows
