package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// Applying to a player corporation: the door the module knocks on rather than opens.
//
// Joining an NPC corporation is a database write (corpjoin.go calls JoinCorporation). A player
// corporation has a director on the other side, so a pilot only applies: it writes a real
// chrApplications row, the director accepts or rejects it in game, and this file's sweep
// completes an acceptance. The pilot keeps working in its current corporation while it waits.
// See docs/comment-notes-b03.md#corp-application-flow.

const (
	// How long a pilot waits on a director before withdrawing: long enough that a corporation
	// checked weekly still gets applicants, short enough that a dead one does not block a pilot.
	corpApplyExpirySec = 7 * 24 * 3600

	// How long a refusal holds, per (pilot, corporation): the director is not asked about this
	// pilot again for a fortnight.
	corpApplyRejectCooldownSec = 14 * 24 * 3600

	// A withdrawal is the module's own doing (nobody answered, or the door was shut), so it holds
	// for less than a refusal.
	corpApplyWithdrawCooldownSec = 3 * 24 * 3600

	// The sweep's cadence. tick.Manager runs at 1s.
	corpApplyEveryTicks = 60

	// How many applications one pilot may have pending at once.
	corpApplyMax = 3
)

// Application states, module-side. The game's status column is the authority on what the
// director did; these record what the module did about it.
const (
	corpAppPending   = "pending"
	corpAppJoined    = "joined"
	corpAppRejected  = "rejected"
	corpAppExpired   = "expired"
	corpAppWithdrawn = "withdrawn"
	corpAppFailed    = "failed"
	corpAppLeft      = "left" // the pilot left this corporation, on morale or by application (corpleave.go)
)

// corpApplyLeftCooldownSec is how long the corporation a pilot left stays shut to it.
const corpApplyLeftCooldownSec = 60 * 24 * 3600

// corpApplication is one row of the module's bookkeeping, with the facts the panel wants
// joined in.
type corpApplication struct {
	CharID    int64
	CharName  string
	CorpID    int64
	CorpName  string
	CorpTick  string
	AppliedAt int64
	ExpiresAt int64
	DecidedAt int64
	State     string
	Score     float64
	Reason    string
	// Status is the game's chrApplications.status, -1 when the row has gone. Only meaningful
	// while the module row is pending.
	Status int64
}

// AppliedText and WaitText are the two times the panel shows: when the pilot applied, and
// what is left on the clock. They are formatted here so the template does no arithmetic.
func (a corpApplication) AppliedText() string {
	if a.AppliedAt == 0 {
		return "—"
	}
	return time.Unix(a.AppliedAt, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func (a corpApplication) WaitText() string {
	if a.State != corpAppPending {
		if a.DecidedAt == 0 {
			return "—"
		}
		return time.Unix(a.DecidedAt, 0).UTC().Format("2006-01-02 15:04 UTC")
	}
	left := a.ExpiresAt - clock.Unix()
	if left <= 0 {
		return "due to withdraw"
	}
	return durationText(float64(left) / 60)
}

// StateLabel is how the panel says the state.
func (a corpApplication) StateLabel() string {
	switch a.State {
	case corpAppPending:
		switch a.Status {
		case evedb.AppRenegotiatedByCorp:
			return "renegotiated — waiting"
		case -1:
			return "waiting (no application row)"
		}
		return "waiting on a director"
	case corpAppJoined:
		return "accepted — joined"
	case corpAppRejected:
		return "rejected"
	case corpAppExpired:
		return "expired unanswered"
	case corpAppWithdrawn:
		return "withdrawn"
	case corpAppFailed:
		return "failed to join"
	}
	return a.State
}

// ── writing one ─────────────────────────────────────────────────────────────

// applyToPlayerCorp is what a review does when the drawn offer is a player corporation. It
// writes the game's application row and the module's bookkeeping, and mails the CEO, since
// nothing pushes a live notification. It reports nothing on success: the pilot has not moved.
func (m *Mod) applyToPlayerCorp(ctx context.Context, b *botRun, pick *corpCandidate, p pilotIdentity) error {
	now := clock.Unix()
	text := corpApplicationText(p, pick.Corp, pick.Score)

	if err := evedb.InsertApplication(ctx, m.db, b.CharID, pick.Corp.CorpID, text); err != nil {
		return fmt.Errorf("applying to %s: %w", pick.Corp.Name, err)
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_applications
			(characterID, corporationID, appliedAt, expiresAt, decidedAt, state, score, reason)
		VALUES (?, ?, ?, ?, 0, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			appliedAt = VALUES(appliedAt), expiresAt = VALUES(expiresAt),
			decidedAt = 0, state = VALUES(state),
			score = VALUES(score), reason = VALUES(reason)`,
		b.CharID, pick.Corp.CorpID, now, now+corpApplyExpirySec,
		corpAppPending, pick.Score.Score, truncate(pick.Score.Why, 255)); err != nil {
		return fmt.Errorf("recording application to %s: %w", pick.Corp.Name, err)
	}

	m.mailCEOAboutApplication(ctx, b, pick.Corp, text)

	m.logBot(b.CharID, logInfo, "corp",
		"applied to %s — %s (score %.2f: fit %.2f, home x%.2f, crowd x%.2f); waiting on a director",
		pick.Corp.Name, pick.Score.Why, pick.Score.Score,
		pick.Score.Fit, pick.Score.Home, pick.Score.Crowd)
	return nil
}

// mailCEOAboutApplication is the notification the wire does not carry. It is best effort: a
// corporation whose CEO row has gone still gets the application, without a letter.
func (m *Mod) mailCEOAboutApplication(ctx context.Context, b *botRun, c CorpFacts, text string) {
	if c.CEOID == 0 || c.CEOID == b.CharID {
		return
	}
	// A bot CEO reads its applications off the desk every think (corpceo_hr.go); no letter.
	if _, managed := managedCEO(c.CorpID); managed {
		return
	}
	// <br>, not a newline: the client's mail body is EVE-HTML and a bare newline is whitespace
	// (the same trap as the fleet report, gangreport.go).
	body := fmt.Sprintf("%s<br><br>--<br>%s has applied to join %s. "+
		"The application is in your corporation window, under Applications.",
		strings.ReplaceAll(text, "\n", "<br>"), b.Name, c.Name)
	if _, err := evedb.SendEveMail(ctx, m.db, c.CEOID, b.CharID,
		mailSubject("Application to "+c.Name), body, win32From(clock.Now())); err != nil {
		m.logBot(b.CharID, logError, "corp",
			"could not mail %s's CEO about the application: %v", c.Name, err)
	}
}

// corpApplicationText is what the director reads, written from the pilot's identity (the work
// it wants and where it stands) so applications differ. It is deterministic in the character
// ID, so a re-application after a cooldown reads the same.
func corpApplicationText(p pilotIdentity, c CorpFacts, s corpScore) string {
	openers := []string{
		"I am looking for an opening.",
		"I would like to fly for you.",
		"Putting my name forward for a place with you.",
		"Looking for people to work with.",
	}
	where := "I can be where you need me."
	switch {
	case s.SameHome:
		where = "I am already based out of your station."
	case s.Jumps == 0:
		where = fmt.Sprintf("I keep a clone in %s, where you are.", c.SystemName)
	case s.Jumps > 0:
		where = fmt.Sprintf("I am %d jump(s) out from you.", s.Jumps)
	}
	work := corpBestTagLabel(p, c.Policy.Tags)
	if work == "unclassified" {
		work = "whatever needs doing"
	}
	return fmt.Sprintf("%s %s work is what I am good at. %s\n\n%s",
		openers[int(p.CharID%int64(len(openers)))],
		strings.ToUpper(work[:1])+work[1:], where, p.Name)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ── the cooldown ────────────────────────────────────────────────────────────

// corpApplyBlocked is the set of corporations this pilot may not apply to now (refused or
// withdrawn recently, or already waiting on), plus how many applications are pending. It is
// read once per review, not per candidate.
func (m *Mod) corpApplyBlocked(ctx context.Context, charID int64) (map[int64]bool, int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, state, decidedAt
		FROM rolebot_corp_applications WHERE characterID = ?`, charID)
	if err != nil {
		return nil, 0, fmt.Errorf("corpApplyBlocked: %w", err)
	}
	defer rows.Close()
	now := clock.Unix()
	out := map[int64]bool{}
	pending := 0
	for rows.Next() {
		var corpID, decidedAt int64
		var state string
		if err := rows.Scan(&corpID, &state, &decidedAt); err != nil {
			return nil, 0, err
		}
		switch state {
		case corpAppPending:
			out[corpID] = true
			pending++
		case corpAppRejected, corpAppFailed:
			if now-decidedAt < corpApplyRejectCooldownSec {
				out[corpID] = true
			}
		case corpAppExpired, corpAppWithdrawn:
			if now-decidedAt < corpApplyWithdrawCooldownSec {
				out[corpID] = true
			}
		case corpAppLeft:
			if now-decidedAt < corpApplyLeftCooldownSec {
				out[corpID] = true
			}
		}
	}
	return out, pending, rows.Err()
}

// ── the sweep ───────────────────────────────────────────────────────────────

// corpApplySweepBusy stops a slow pass overlapping the next tick: periodics fire on the tick
// goroutine and the first pass after a long downtime has real work.
var corpApplySweepBusy atomic.Bool

// startCorpApplicationSweep registers the pass that turns a director's answer into an
// employment change. It runs while the master switch is on.
func (m *Mod) startCorpApplicationSweep() {
	m.registerPeriodic("rolebot-corp-applications", corpApplyEveryTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		if !corpApplySweepBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("corp-application sweep", func() {
			defer corpApplySweepBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := m.sweepCorpApplications(ctx); err != nil {
				rlogf("rolebots: corp application sweep: %v", err)
			}
		})
		return true
	})
}

// pendingApplicant is one waiting application, with everything the sweep needs to decide it in
// a single row.
type pendingApplicant struct {
	CharID    int64
	CharName  string
	CorpID    int64
	CorpName  string
	CurCorpID int64
	ExpiresAt int64
	// Status is chrApplications.status, -1 when the row has gone. A desk refusal soft-deletes the
	// row it marks rejected (rejectApplication), so the rejected row must be read through the
	// delete or it closes as a withdrawal with the short cooldown.
	Status   int64
	OnRoster bool
	Joinable bool
}

// sweepCorpApplications decides every pending application once.
func (m *Mod) sweepCorpApplications(ctx context.Context) error {
	// The game's applications table is created lazily, so a pending module row proves it exists;
	// asking first costs one indexed count.
	var pending int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_corp_applications WHERE state = ?`,
		corpAppPending).Scan(&pending); err != nil {
		return fmt.Errorf("sweepCorpApplications count: %w", err)
	}
	if pending == 0 {
		return nil
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT a.characterID, COALESCE(ch.characterName, ''), a.corporationID,
		       COALESCE(co.corporationName, ''), COALESCE(ch.corporationID, 0),
		       a.expiresAt,
		       CASE WHEN ca.characterID IS NULL THEN -1
		            WHEN ca.status = ? THEN ca.status
		            WHEN ca.deleted <> 0 THEN -1
		            ELSE ca.status END,
		       CASE WHEN r.characterID IS NULL THEN 0 ELSE 1 END,
		       COALESCE(p.joinable, 0)
		FROM rolebot_corp_applications a
		LEFT JOIN chrApplications ca
		       ON ca.corporationID = a.corporationID AND ca.characterID = a.characterID
		LEFT JOIN character_ ch ON ch.characterID = a.characterID
		LEFT JOIN corporation co ON co.corporationID = a.corporationID
		LEFT JOIN rolebot_characters r ON r.characterID = a.characterID
		LEFT JOIN rolebot_corps p ON p.corporationID = a.corporationID
		WHERE a.state = ?`, evedb.AppRejectedByCorporation, corpAppPending)
	if err != nil {
		return fmt.Errorf("sweepCorpApplications: %w", err)
	}
	var list []pendingApplicant
	for rows.Next() {
		var a pendingApplicant
		var onRoster, joinable int64
		if err := rows.Scan(&a.CharID, &a.CharName, &a.CorpID, &a.CorpName,
			&a.CurCorpID, &a.ExpiresAt, &a.Status, &onRoster, &joinable); err != nil {
			rows.Close()
			return fmt.Errorf("sweepCorpApplications scan: %w", err)
		}
		a.OnRoster = onRoster != 0
		a.Joinable = joinable != 0
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sweepCorpApplications rows: %w", err)
	}

	now := clock.Unix()
	// A pilot accepted by two desks in one pass joins the first, and that join withdraws the
	// others. The snapshot is stale for them, so they are skipped.
	joined := map[int64]bool{}
	for _, a := range list {
		if joined[a.CharID] {
			continue
		}
		switch {
		case a.Status == evedb.AppAcceptedByCorporation:
			if m.completeApplication(ctx, a, now) {
				joined[a.CharID] = true
			}

		case a.Status == evedb.AppRejectedByCorporation:
			m.closeApplication(ctx, a, corpAppRejected, "the corporation rejected the application")
			m.logBot(a.CharID, logInfo, "corp",
				"%s turned the application down — looking elsewhere", a.CorpName)

		case a.Status == evedb.AppRejectedByCharacter || a.Status == -1:
			// The row was deleted or refused on the character's behalf (a GM, or DeleteApplication).
			m.closeApplication(ctx, a, corpAppWithdrawn, "the application row is gone")

		case !a.OnRoster:
			m.withdrawApplication(ctx, a, corpAppWithdrawn, "the pilot has left the roster")

		case !a.Joinable:
			m.withdrawApplication(ctx, a, corpAppWithdrawn,
				"the operator shut this corporation to bots")

		case a.ExpiresAt > 0 && now >= a.ExpiresAt:
			m.withdrawApplication(ctx, a, corpAppExpired, "nobody answered")
			m.logBot(a.CharID, logInfo, "corp",
				"withdrew its application to %s — nobody answered in %d days",
				a.CorpName, corpApplyExpirySec/86400)
		}
	}
	return nil
}

// completeApplication is the join a director's acceptance earned. It does what the client's
// applicant-side accept does (corp move plus the status write) with the module's after-effects
// (address, presence, review bookkeeping), as joinCorp does for an NPC move.
func (m *Mod) completeApplication(ctx context.Context, a pendingApplicant, now int64) bool {
	// A pilot now in CONCORD does not leave it (corpReview's CONCORD rule, corpjoin.go). The
	// acceptance is turned down so the director sees an answer.
	if corpIsConcord(ctx, m.db, a.CurCorpID) {
		m.withdrawApplication(ctx, a, corpAppWithdrawn,
			"the pilot holds a CONCORD seat and is not leaving it")
		m.logBot(a.CharID, logInfo, "corp",
			"withdrew its application to %s — it holds a CONCORD seat", a.CorpName)
		return false
	}
	// Sansha's Nation does not let go (corpIsSansha, corpjoin.go): the application is turned down.
	if corpIsSansha(ctx, m.db, a.CurCorpID) {
		m.withdrawApplication(ctx, a, corpAppWithdrawn,
			"the pilot belongs to Sansha's Nation and is not leaving it")
		m.logBot(a.CharID, logInfo, "corp",
			"withdrew its application to %s — it belongs to Sansha's Nation", a.CorpName)
		return false
	}
	// A full corporation (evedb.CorpHasOpening) leaves the acceptance standing and the row pending.
	// Expiry is checked here because the sweep's expiry arm never sees an accepted row.
	if ok, _, _, err := evedb.CorpHasOpening(ctx, m.db, a.CorpID); err == nil && !ok {
		if a.ExpiresAt > 0 && now >= a.ExpiresAt {
			m.withdrawApplication(ctx, a, corpAppExpired,
				"accepted, but the corporation never had an opening for it")
			m.logBot(a.CharID, logInfo, "corp",
				"withdrew its application to %s — accepted, but the corporation stayed full for %d days",
				a.CorpName, corpApplyExpirySec/86400)
		}
		return false
	}
	if _, err := evedb.JoinCorporation(ctx, m.db, a.CharID, a.CorpID, a.CurCorpID); err != nil {
		// A failed join is neither a rejection nor a retry: the row closes and the cooldown holds, so
		// the next review looks elsewhere.
		m.closeApplication(ctx, a, corpAppFailed, err.Error())
		m.logBot(a.CharID, logError, "corp",
			"%s accepted the application but the join failed: %v", a.CorpName, err)
		return false
	}
	// The applicant-side status is written after the join, as the core does: the join must not
	// be lost, and a row left at "accepted by corporation" can still be cleared.
	if err := evedb.UpdateApplication(ctx, m.db, a.CharID, a.CorpID, a.CharID,
		evedb.AppAcceptedByCharacter, ""); err != nil {
		m.logBot(a.CharID, logError, "corp",
			"joined %s but could not close the application row: %v", a.CorpName, err)
	}
	m.closeApplication(ctx, a, corpAppJoined, "accepted by a director")
	m.withdrawOtherApplications(ctx, a.CharID, a.CorpID)
	// The rows the old corporation kept on the pilot go with it, and that door shuts for a while (corpleave.go).
	m.leaveCorpSteps(ctx, a.CharID, a.CurCorpID)
	m.markLeft(ctx, a.CharID, a.CurCorpID, leftByApply)

	// Same bookkeeping as an NPC move. heldCorpID follows the pilot, so a kick is noticed and it
	// goes home to its school corporation (corpjoin.go).
	if rev, err := getCorpReview(ctx, m.db, a.CharID); err == nil {
		rev.LastMoveAt = clock.Unix()
		rev.FromCorpID = a.CurCorpID
		rev.HeldCorpID = a.CorpID
		rev.Moves++
		_ = saveCorpReview(ctx, m.db, rev)
	}

	// A new employer is a new address to consider, and presence is corp-keyed (as joinCorp).
	forgetCorpHome(a.CharID)
	m.syncPresence()

	m.logBot(a.CharID, logInfo, "corp",
		"signed with %s — its application was accepted by a director", a.CorpName)
	return true
}

// withdrawOtherApplications takes back every other pending application once one desk has
// signed the pilot, the way an expiry does, so it holds no place elsewhere.
func (m *Mod) withdrawOtherApplications(ctx context.Context, charID, keepCorpID int64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT a.corporationID, COALESCE(co.corporationName, '')
		  FROM rolebot_corp_applications a
		  LEFT JOIN corporation co ON co.corporationID = a.corporationID
		 WHERE a.characterID = ? AND a.state = ? AND a.corporationID <> ?`,
		charID, corpAppPending, keepCorpID)
	if err != nil {
		rlogf("rolebots: other applications of %d: %v", charID, err)
		return
	}
	var others []pendingApplicant
	for rows.Next() {
		o := pendingApplicant{CharID: charID}
		if err := rows.Scan(&o.CorpID, &o.CorpName); err != nil {
			rows.Close()
			rlogf("rolebots: other applications of %d: %v", charID, err)
			return
		}
		others = append(others, o)
	}
	rows.Close()
	for _, o := range others {
		m.withdrawApplication(ctx, o, corpAppWithdrawn, "the pilot signed with another corporation")
	}
}

// withdrawApplication takes the application off the director's list and closes the module
// row. The state is the caller's: "expired" and "withdrawn" carry different cooldowns.
func (m *Mod) withdrawApplication(ctx context.Context, a pendingApplicant, state, why string) {
	if err := evedb.DeleteApplication(ctx, m.db, a.CharID, a.CorpID); err != nil {
		m.logBot(a.CharID, logError, "corp",
			"could not withdraw the application to %s: %v", a.CorpName, err)
	}
	m.closeApplication(ctx, a, state, why)
}

// closeApplication records the outcome. decidedAt is what the cooldown counts from, so it is
// stamped on every terminal state and never on a pending one.
func (m *Mod) closeApplication(ctx context.Context, a pendingApplicant, state, why string) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_applications
		   SET state = ?, decidedAt = ?, reason = ?
		 WHERE characterID = ? AND corporationID = ?`,
		state, clock.Unix(), truncate(why, 255), a.CharID, a.CorpID); err != nil {
		rlogf("rolebots: closing application %d→%d: %v", a.CharID, a.CorpID, err)
	}
	m.forgetHeld(a.CorpID, a.CharID)
}

// ── reading them, for the panel ─────────────────────────────────────────────

// listCorpApplications reads the application ledger. corpID 0 is every corporation;
// onlyPending narrows it to the queue a director is sitting on.
func (m *Mod) listCorpApplications(ctx context.Context, corpID int64, onlyPending bool, limit int) ([]corpApplication, error) {
	where := "1 = 1"
	var args []any
	if corpID != 0 {
		where += " AND a.corporationID = ?"
		args = append(args, corpID)
	}
	if onlyPending {
		where += " AND a.state = '" + corpAppPending + "'"
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT a.characterID, COALESCE(ch.characterName, ''), a.corporationID,
		       COALESCE(co.corporationName, ''), COALESCE(co.tickerName, ''),
		       a.appliedAt, a.expiresAt, a.decidedAt, a.state, a.score, a.reason,
		       CASE WHEN ca.characterID IS NULL OR ca.deleted <> 0 THEN -1 ELSE ca.status END
		FROM rolebot_corp_applications a
		LEFT JOIN character_ ch ON ch.characterID = a.characterID
		LEFT JOIN corporation co ON co.corporationID = a.corporationID
		LEFT JOIN chrApplications ca
		       ON ca.corporationID = a.corporationID AND ca.characterID = a.characterID
		WHERE `+where+`
		ORDER BY a.state = '`+corpAppPending+`' DESC, a.appliedAt DESC
		LIMIT ?`, append(args, limit)...)
	if err != nil {
		// The game's applications table is created lazily: its absence means nothing ever applied.
		if isMissingTable(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listCorpApplications: %w", err)
	}
	defer rows.Close()
	var out []corpApplication
	for rows.Next() {
		var a corpApplication
		if err := rows.Scan(&a.CharID, &a.CharName, &a.CorpID, &a.CorpName, &a.CorpTick,
			&a.AppliedAt, &a.ExpiresAt, &a.DecidedAt, &a.State, &a.Score, &a.Reason,
			&a.Status); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// pendingApplicationCounts is how many applications each corporation is sitting on, for the
// catalogue's column. One query for the whole page.
func (m *Mod) pendingApplicationCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, COUNT(*) FROM rolebot_corp_applications
		 WHERE state = ? GROUP BY corporationID`, corpAppPending)
	if err != nil {
		return nil, fmt.Errorf("pendingApplicationCounts: %w", err)
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var corpID int64
		var n int
		if err := rows.Scan(&corpID, &n); err != nil {
			return nil, err
		}
		out[corpID] = n
	}
	return out, rows.Err()
}

// withdrawApplicationByID is the panel's retraction: the operator takes one bot's application
// off a director's list without waiting for expiry.
func (m *Mod) withdrawApplicationByID(ctx context.Context, charID, corpID int64) error {
	var name string
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(corporationName, '') FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&name); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	m.withdrawApplication(ctx, pendingApplicant{
		CharID: charID, CorpID: corpID, CorpName: name,
	}, corpAppWithdrawn, "withdrawn from the admin panel")
	return nil
}

// isMissingTable reports the one MySQL error (1146) this module treats as "nobody has used
// that feature yet" rather than a failure.
func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1146")
}
