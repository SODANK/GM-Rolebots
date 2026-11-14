package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"

	evedb "gurimoon/db"
)

// Recruitment: when a rolebot goes looking for an employer, and what it does about the answer. corps.go
// decides which corporation a pilot wants; this is the trigger, the hysteresis and the call that moves
// it. Recruitment is always on. It runs at the plan step, idle and docked.
// Triggers: graduation (a generated pilot starts in its school and looks within corpGraduationWindowSec,
// spread by character ID), review (a look whose cadence is the loyalty trait, one row in
// rolebot_corp_reviews) and displacement (a pilot somebody else moved goes back to its school).
// A pilot signs with one NPC corporation and stays: heldCorpID is a commitment, and only a player
// corporation can take it off its employer. A look ranks the catalogue (rankCorpsForPilot) and moves
// only on a margin scaled by loyalty, after a move floor. It never moves a pilot recruited into a
// player corporation or joins one the operator has not opened (corpEligibility).

// Cadences, in seconds. Each is bent by the pilot's loyalty (traitScale of
// TraitLoyalty: −1 devoted, +1 mercenary), so the three numbers are the
// devoted end, the even-tempered middle and the mercenary end.
const (
	corpReviewDevotedSec   = 21 * 24 * 3600
	corpReviewBaseSec      = 7 * 24 * 3600
	corpReviewMercenarySec = 2 * 24 * 3600

	corpMoveFloorDevotedSec   = 45 * 24 * 3600
	corpMoveFloorBaseSec      = 14 * 24 * 3600
	corpMoveFloorMercenarySec = 4 * 24 * 3600

	// The margin the best offer must beat the current employer by. A
	// devoted pilot needs a much better offer to move at all; a mercenary
	// moves on a slim one. Never below 1: an equal offer is not a reason.
	corpSwitchMarginDevoted   = 2.0
	corpSwitchMarginBase      = 1.35
	corpSwitchMarginMercenary = 1.05

	// How long after a pilot first becomes eligible for a look its
	// graduation review lands, spread across the window by character ID.
	corpGraduationWindowSec = 12 * 3600

	// The absolute bar an offer clears before a pilot signs at all. Unlike the margin it does not scale with
	// the current employer, so a pilot whose employer scores nothing still refuses what it does not want.
	// A plausible outfit scores about 0.5 to 1.75 for work a pilot wants.
	corpMinAcceptScore = 0.45

	// How much colder the employer draw is than the job draw. The pilot's
	// own PickTemperature (0.15 restless … 0.7 nomadic) is scaled by this
	// before the softmax in pickCorpOffer.
	corpPickTempScale = 0.5

	// The chargen activity: a corporation that exists to turn civilians
	// into capsuleers. Being in one is what "still at school" means.
	corpActivitySchool = 18
)

// corpReviewRow is one pilot's recruitment bookkeeping.
type corpReviewRow struct {
	CharID       int64
	LastReviewAt int64
	NextReviewAt int64
	LastMoveAt   int64
	FromCorpID   int64
	// SchoolCorpID is where this pilot goes home to when it loses its place: the corporation it was created
	// in. It is stored because after a kick the chrEmployment history is the only other trace.
	SchoolCorpID int64
	// HeldCorpID is where the module last put this pilot, 0 while never placed. Set means the pilot has
	// signed and shops only player corporations (commitment). character_.corporationID disagreeing with it
	// means somebody else moved the pilot (displacement).
	HeldCorpID int64
	Moves      int
	// ExitDueAt is when the morale sampler decided this pilot leaves (corpleave.go), 0 for not.
	ExitDueAt int64
}

func getCorpReview(ctx context.Context, db *sql.DB, charID int64) (*corpReviewRow, error) {
	r := &corpReviewRow{CharID: charID}
	err := db.QueryRowContext(ctx, `
		SELECT lastReviewAt, nextReviewAt, lastMoveAt, fromCorpID, schoolCorpID, heldCorpID, moves, exitDueAt
		FROM rolebot_corp_reviews WHERE characterID = ?`, charID,
	).Scan(&r.LastReviewAt, &r.NextReviewAt, &r.LastMoveAt, &r.FromCorpID,
		&r.SchoolCorpID, &r.HeldCorpID, &r.Moves, &r.ExitDueAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getCorpReview: %w", err)
	}
	return r, nil
}

func saveCorpReview(ctx context.Context, db *sql.DB, r *corpReviewRow) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_reviews
			(characterID, lastReviewAt, nextReviewAt, lastMoveAt, fromCorpID,
			 schoolCorpID, heldCorpID, moves, exitDueAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			lastReviewAt = VALUES(lastReviewAt), nextReviewAt = VALUES(nextReviewAt),
			lastMoveAt = VALUES(lastMoveAt), fromCorpID = VALUES(fromCorpID),
			schoolCorpID = VALUES(schoolCorpID), heldCorpID = VALUES(heldCorpID),
			moves = VALUES(moves), exitDueAt = VALUES(exitDueAt)`,
		r.CharID, r.LastReviewAt, r.NextReviewAt, r.LastMoveAt, r.FromCorpID,
		r.SchoolCorpID, r.HeldCorpID, r.Moves, r.ExitDueAt)
	if err != nil {
		return fmt.Errorf("saveCorpReview: %w", err)
	}
	return nil
}

// corpReviewEvery is how often this pilot looks at the job market.
func corpReviewEvery(traits map[string]int) int64 {
	t := traitScale(traits[TraitLoyalty])
	return int64(bend(t, corpReviewDevotedSec, corpReviewBaseSec, corpReviewMercenarySec))
}

// corpMoveFloor is the shortest time this pilot will stay somewhere.
func corpMoveFloor(traits map[string]int) int64 {
	t := traitScale(traits[TraitLoyalty])
	return int64(bend(t, corpMoveFloorDevotedSec, corpMoveFloorBaseSec, corpMoveFloorMercenarySec))
}

// corpSwitchMargin is how much better an offer has to be before this pilot
// takes it. Floored at 1.01 so no personality ever moves on an equal offer.
func corpSwitchMargin(traits map[string]int) float64 {
	t := traitScale(traits[TraitLoyalty])
	v := bend(t, corpSwitchMarginDevoted, corpSwitchMarginBase, corpSwitchMarginMercenary)
	if v < 1.01 {
		v = 1.01
	}
	return v
}

// isNPCCorp reports whether a corporation is one of the game's own. A pilot in a player corporation is
// left alone: a player recruited that character.
func isNPCCorp(ctx context.Context, db *sql.DB, corpID int64) (bool, error) {
	if corpID == 0 {
		return false, nil
	}
	var one int64
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM crpNPCCorporations WHERE corporationID = ?`, corpID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("isNPCCorp: %w", err)
	}
	return true, nil
}

// corpIsConcord reports whether a corporation belongs to the CONCORD Assembly. Nobody joins one by
// choice (corps.go shuts every CONCORD door), so a pilot inside one was put there at generation and
// keeps the seat. It reads the faction name; no faction ID is hardcoded.
func corpIsConcord(ctx context.Context, db *sql.DB, corpID int64) bool {
	return npcCorpFaction(ctx, db, corpID) == "CONCORD Assembly"
}

// corpIsSansha reports whether a corporation belongs to Sansha's Nation. A pilot in one stays for good:
// no NPC employer, player corporation, founding or operator kick. Every path that moves a rostered pilot
// asks this first (corpReview, completeApplication, foundRefusal, installCEO, kickBotsToSchool,
// applyEmploymentHistory).
func corpIsSansha(ctx context.Context, db *sql.DB, corpID int64) bool {
	return npcCorpFaction(ctx, db, corpID) == "Sansha's Nation"
}

// npcCorpFaction is the normalised faction name of an NPC corporation, "" for
// a player corporation, an unaffiliated one, or a failed read.
func npcCorpFaction(ctx context.Context, db *sql.DB, corpID int64) string {
	if corpID == 0 {
		return ""
	}
	var name string
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(f.factionName, '')
		  FROM crpNPCCorporations n
		  LEFT JOIN chrFactions f ON f.factionID = n.factionID
		 WHERE n.corporationID = ?`, corpID).Scan(&name); err != nil {
		return ""
	}
	return normFactionName(name)
}

// corpIsSchool reports whether a corporation is a chargen school.
func corpIsSchool(ctx context.Context, db *sql.DB, corpID int64) bool {
	var activity int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(mainActivityID, 0) FROM crpNPCCorporations WHERE corporationID = ?`,
		corpID).Scan(&activity); err != nil {
		return false
	}
	return activity == corpActivitySchool
}

// corpReviewNextAt is when each pilot's next employer review falls due, as last read from its row. A
// due review drops the entry; an operator who brings one forward is seen on the next due check.
var corpReviewNextAt sync.Map // charID to unix

func init() { registerCharForget("corpReviewNextAt", func(id int64) { corpReviewNextAt.Delete(id) }) }

func (m *Mod) corpReview(ctx context.Context, b *botRun) (bool, error) {
	// This review carries recruitment (always on) and self-founding (CorpFound, corpfound.go).
	if b.StationID == 0 {
		return false, nil
	}
	now := clock.Unix()

	// The common case, not due, costs a map lookup instead of the row read.
	if v, ok := corpReviewNextAt.Load(b.CharID); ok && now < v.(int64) {
		return false, nil
	}
	rev, err := getCorpReview(ctx, m.db, b.CharID)
	if err != nil {
		return false, err
	}
	if rev.NextReviewAt > now {
		corpReviewNextAt.Store(b.CharID, rev.NextReviewAt)
	} else {
		corpReviewNextAt.Delete(b.CharID)
	}

	// First sighting: schedule the look and record where home is while the pilot stands in it. A pilot at
	// school gets the short graduation window, others a full cycle, both spread by character ID.
	if rev.NextReviewAt == 0 {
		traits, err := m.botTraits(ctx, b)
		if err != nil {
			return false, err
		}
		window := corpReviewEvery(traits)
		atSchool := corpIsSchool(ctx, m.db, b.CorpID)
		if atSchool {
			window = corpGraduationWindowSec
			rev.SchoolCorpID = b.CorpID
		} else {
			// An adopted character, or one moved before the module met it: its school is the first employer
			// chrEmployment remembers.
			rev.SchoolCorpID = firstNPCEmployer(ctx, m.db, b.CharID)
		}
		if window < 60 {
			window = 60
		}
		rev.NextReviewAt = now + b.CharID%window
		return false, saveCorpReview(ctx, m.db, rev)
	}
	if now < rev.NextReviewAt {
		return false, nil
	}

	traits, err := m.botTraits(ctx, b)
	if err != nil {
		return false, err
	}
	rev.LastReviewAt = now
	rev.NextReviewAt = now + corpReviewEvery(traits)

	// Sansha's Nation: once in, never out. Checked before displacement, which would otherwise send a pilot
	// that reached the Nation by an unrecorded route back to school. The Nation becomes its held corporation.
	if corpIsSansha(ctx, m.db, b.CorpID) {
		rev.HeldCorpID = b.CorpID
		return false, saveCorpReview(ctx, m.db, rev)
	}

	// A pilot the morale sampler flagged leaves here, where the guards can be asked (corpleave.go).
	if CorpAttrition() && rev.ExitDueAt != 0 {
		moved, err := m.considerLeaving(ctx, b, rev, traits)
		if err != nil {
			_ = saveCorpReview(ctx, m.db, rev)
			return false, err
		}
		if moved {
			return true, saveCorpReview(ctx, m.db, rev)
		}
	}

	// Displacement is checked first, because the skips below would swallow it. The core's kick lands a pilot
	// in its last NPC employer (evedb.PreviousNPCCorp), so the module sends it on to school, uncommitted.
	if rev.HeldCorpID != 0 && b.CorpID != rev.HeldCorpID {
		moved, err := m.returnToSchool(ctx, b, rev)
		if err != nil {
			_ = saveCorpReview(ctx, m.db, rev)
			return false, err
		}
		return moved, saveCorpReview(ctx, m.db, rev)
	}

	// A pilot somebody else recruited is not the module's to move. The review clock still advances, so a bot
	// switched back out of a player corporation by hand is not frozen.
	npc, err := isNPCCorp(ctx, m.db, b.CorpID)
	if err != nil {
		return false, err
	}
	if !npc {
		return false, saveCorpReview(ctx, m.db, rev)
	}

	// A CONCORD seat is held for good: every CONCORD door is shut to the job market, so generation put the
	// pilot there. It does not shop, but the review clock still advances.
	if corpIsConcord(ctx, m.db, b.CorpID) {
		return false, saveCorpReview(ctx, m.db, rev)
	}

	// A leader may found a corporation of its own instead of shopping for one
	// (corpfound.go), whatever applications it has out: the founding withdraws
	// them. Off by default; the review clock has already moved.
	founded, err := m.considerFounding(ctx, b, traits, rev)
	if err != nil {
		rlogf("rolebots: %s founding: %v", b.Name, err)
	}
	if founded {
		return true, saveCorpReview(ctx, m.db, rev)
	}

	// A pilot with corpApplyMax applications out does not look again. Below that it keeps shopping, and
	// doors it already waits at are blocked so it never doubles up (corpapply.go).
	blocked, pendingApps, err := m.corpApplyBlocked(ctx, b.CharID)
	if err != nil {
		return false, err
	}
	if pendingApps >= corpApplyMax {
		return false, saveCorpReview(ctx, m.db, rev)
	}

	pilots, err := m.loadPilotIdentities(ctx, b.CharID, false)
	if err != nil {
		return false, err
	}
	if len(pilots) == 0 {
		return false, saveCorpReview(ctx, m.db, rev)
	}
	ranked, err := m.rankCorpsForPilot(ctx, pilots[0], 0)
	if err != nil {
		return false, err
	}
	// A door this pilot was refused at or walked away from recently is closed for now. It is applied to the
	// ranking, as a refusal, so the corporation is out of the softmax draw as well as the comparison.
	for i := range ranked {
		if blocked[ranked[i].Corp.CorpID] && ranked[i].Corp.CorpID != b.CorpID {
			ranked[i].Score.Verdict = corpVerdict{Rule: "applied",
				Why: "this pilot applied here recently and is not asking again yet"}
			ranked[i].Score.Score = 0
		}
	}
	// An invitation from a corporation's Recruitment Officer (corpstaff_dept.go) raises the score. It never
	// opens a door: a refusal stays a refusal.
	if invites := m.pilotInvites(ctx, b.CharID); len(invites) > 0 {
		for i := range ranked {
			if invites[ranked[i].Corp.CorpID] && ranked[i].Score.Verdict.OK {
				ranked[i].Score.Score *= hrInviteMul
				ranked[i].Score.Why += " — and it invited this pilot"
			}
		}
	}

	// A commitment to a corporation that would refuse the pilot on identity (the race rule) is released.
	// Other refusals, such as a shut door, leave it: they concern arrivals, not pilots already inside.
	if rev.HeldCorpID != 0 {
		for i := range ranked {
			c := &ranked[i]
			if c.Corp.CorpID != b.CorpID {
				continue
			}
			if r := c.Score.Verdict.Rule; !c.Score.Verdict.OK && (r == "race" || r == "tribe") {
				rlogf("rolebots: %s can no longer be in %s (%s) — commitment released, shopping again",
					b.Name, c.Corp.Name, c.Score.Verdict.Why)
				rev.HeldCorpID = 0
			}
			break
		}
	}

	// Commitment: a pilot that has signed is done shopping NPC corporations, expressed as a refusal on the
	// ranking. Player corporations stay open to it, and its own corporation stays scored.
	if rev.HeldCorpID != 0 {
		for i := range ranked {
			c := &ranked[i]
			if c.Corp.Player || c.Corp.CorpID == b.CorpID || !c.Score.Verdict.OK {
				continue
			}
			c.Score.Verdict = corpVerdict{Rule: "committed",
				Why: "this pilot has signed with an employer and is not shopping NPC corporations again"}
			c.Score.Score = 0
		}
	}

	var best *corpCandidate
	current := 0.0
	currentName := b.Name
	for i := range ranked {
		c := &ranked[i]
		if c.Corp.CorpID == b.CorpID {
			currentName = c.Corp.Name
			// The employer it already has is scored by the same arithmetic, so better means better on the same
			// terms. A corporation the operator has shut scores nothing, which makes a school something to leave.
			if c.Score.Verdict.OK {
				current = c.Score.Score
			}
			continue
		}
		if !c.Score.Verdict.OK {
			continue
		}
		if best == nil || c.Score.Score > best.Score.Score {
			best = c
		}
	}
	if best == nil || best.Score.Score <= 0 {
		return false, saveCorpReview(ctx, m.db, rev)
	}

	// The two gates an offer must clear. The margin is relative to the current employer; the floor is
	// absolute, for a pilot whose employer scores nothing and would otherwise sign with the first offer.
	margin := corpSwitchMargin(traits)
	want := current * margin
	// The loyalist bloc (grace_ledger.go): regard for its own bot CEO raises the bar an outside offer must clear.
	if ceo, ok := managedCEO(b.CorpID); ok {
		if f := graceOpinionOf(b.CharID, ceo); f > 0 {
			want *= 1 + 0.5*math.Min(1, f/5)
		}
	}
	if want < corpMinAcceptScore {
		want = corpMinAcceptScore
	}
	if best.Score.Score < want {
		m.logBot(b.CharID, logInfo, "corp",
			"looked at the job market: the best on offer is %s at %.2f against %.2f where it is (%s) — not worth it yet, holding out",
			best.Corp.Name, best.Score.Score, current, currentName)
		return false, saveCorpReview(ctx, m.db, rev)
	}
	if floor := corpMoveFloor(traits); rev.LastMoveAt != 0 && now-rev.LastMoveAt < floor {
		m.logBot(b.CharID, logInfo, "corp",
			"%s would take it on, but it has been with %s only %s — staying put for now",
			best.Corp.Name, currentName, durationText(float64(now-rev.LastMoveAt)/60))
		return false, saveCorpReview(ctx, m.db, rev)
	}

	// Which acceptable offer it signs is a draw, not the maximum: pilots of one archetype in one station
	// rank the catalogue identically and would all walk into one corporation.
	pick := pickCorpOffer(ranked, b.CorpID, want, pilots[0].Tune.PickTemperature)
	if pick == nil { // cannot happen: best itself clears `want`
		pick = best
	}

	// A player corporation is applied to, not joined: the decision is theirs (corpapply.go). The pilot keeps
	// working until a director answers, so this is not an employment change.
	if pick.Corp.Player {
		if err := m.applyToPlayerCorp(ctx, b, pick, pilots[0]); err != nil {
			_ = saveCorpReview(ctx, m.db, rev)
			return false, err
		}
		// Fill the remaining hands from the same ranking with further player offers only, each a fresh draw.
		// An NPC corporation would end the shopping, so it is never drawn here.
		for slots := corpApplyMax - pendingApps - 1; slots > 0; slots-- {
			pick.Score.Score = 0
			pick.Score.Verdict = corpVerdict{Rule: "applied", Why: "already applied here"}
			next := pickCorpOffer(ranked, b.CorpID, want, pilots[0].Tune.PickTemperature)
			if next == nil || !next.Corp.Player {
				break
			}
			pick = next
			if err := m.applyToPlayerCorp(ctx, b, pick, pilots[0]); err != nil {
				_ = saveCorpReview(ctx, m.db, rev)
				return false, err
			}
		}
		return false, saveCorpReview(ctx, m.db, rev)
	}

	if err := m.joinCorp(ctx, b, pick); err != nil {
		// The clock still advances: a failed join is not a reason to retry
		// the same move every five seconds.
		_ = saveCorpReview(ctx, m.db, rev)
		return false, err
	}
	rev.LastMoveAt = now
	rev.FromCorpID = b.CorpID
	rev.HeldCorpID = pick.Corp.CorpID
	rev.Moves++
	b.CorpID = pick.Corp.CorpID
	return true, saveCorpReview(ctx, m.db, rev)
}

// firstNPCEmployer is the corporation a character was created in: the earliest NPC row of its employment
// history (the mirror of evedb.PreviousNPCCorp). 0 when there is none, which leaves the pilot with no
// home to be sent to.
func firstNPCEmployer(ctx context.Context, db *sql.DB, charID int64) int64 {
	var corpID int64
	err := db.QueryRowContext(ctx, `
		SELECT e.corporationID FROM chrEmployment e
		JOIN crpNPCCorporations n ON n.corporationID = e.corporationID
		WHERE e.characterID = ?
		ORDER BY e.startDate ASC LIMIT 1`, charID).Scan(&corpID)
	if err != nil {
		return 0
	}
	return corpID
}

// returnToSchool sends a displaced pilot home and clears its commitment, so the next look shops the whole
// catalogue again. The move floor goes too: it stops re-badging over rounding, and a pilot thrown out
// is not doing that.
func (m *Mod) returnToSchool(ctx context.Context, b *botRun, rev *corpReviewRow) (bool, error) {
	held := rev.HeldCorpID
	moved, err := m.sendPilotToSchool(ctx, b.CharID, b.CorpID, rev)
	if err != nil {
		return false, err
	}
	if !moved {
		m.logBot(b.CharID, logInfo, "corp",
			"is no longer with the employer it signed with — back on the job market")
		return false, nil
	}
	b.CorpID = rev.SchoolCorpID
	m.logBot(b.CharID, logInfo, "corp",
		"was moved out of the corporation it signed with (%d) and has gone back to its school corporation to look for work again", held)
	m.syncPresence()
	return true, nil
}

// sendPilotToSchool is the move itself, without a botRun, for the displacement path and the panel's kick
// all. It mutates rev but does not save it, and syncPresence is the caller's too, because a loop of
// pilots calls it once at the end. It reports whether the pilot changed corporation; false is not a failure.
func (m *Mod) sendPilotToSchool(ctx context.Context, charID, curCorpID int64, rev *corpReviewRow) (bool, error) {
	rev.HeldCorpID = 0
	rev.LastMoveAt = 0
	rev.NextReviewAt = clock.Unix() + charID%corpGraduationWindowSec

	if rev.SchoolCorpID == 0 {
		rev.SchoolCorpID = firstNPCEmployer(ctx, m.db, charID)
	}
	if rev.SchoolCorpID == 0 || rev.SchoolCorpID == curCorpID {
		return false, nil
	}
	if _, err := evedb.JoinCorporation(ctx, m.db, charID, rev.SchoolCorpID, curCorpID); err != nil {
		return false, fmt.Errorf("returning %d to its school corporation: %w", charID, err)
	}
	rev.FromCorpID = curCorpID
	// A new employer is a new address to consider (corphome.go), as after a join.
	forgetCorpHome(charID)
	return true, nil
}

// pickCorpOffer draws one employer from everything this pilot would accept. Softmax relative to the best
// offer, so temperature means the same at any score scale, and colder than appetite.go's job draw because
// changing employer is rare. want is the bar every candidate already cleared.
func pickCorpOffer(ranked []corpCandidate, currentCorpID int64, want, temp float64) *corpCandidate {
	offers := make([]*corpCandidate, 0, 8)
	best := 0.0
	for i := range ranked {
		c := &ranked[i]
		if c.Corp.CorpID == currentCorpID || !c.Score.Verdict.OK || c.Score.Score < want {
			continue
		}
		offers = append(offers, c)
		if c.Score.Score > best {
			best = c.Score.Score
		}
	}
	if len(offers) == 0 {
		return nil
	}
	if len(offers) == 1 {
		return offers[0]
	}
	t := temp * corpPickTempScale
	if t < 0.01 {
		t = 0.01
	}
	weights := make([]float64, len(offers))
	total := 0.0
	for i, o := range offers {
		weights[i] = math.Exp((o.Score.Score - best) / t)
		total += weights[i]
	}
	roll := rand.Float64() * total
	for i, w := range weights {
		if roll < w {
			return offers[i]
		}
		roll -= w
	}
	return offers[len(offers)-1]
}

// joinCorp performs the move. The core's JoinCorporation does the whole transition (member counts,
// character row, employment record, roles), so nothing here reimplements it.
func (m *Mod) joinCorp(ctx context.Context, b *botRun, pick *corpCandidate) error {
	from := b.CorpID
	if _, err := evedb.JoinCorporation(ctx, m.db, b.CharID, pick.Corp.CorpID, from); err != nil {
		return fmt.Errorf("joining %s: %w", pick.Corp.Name, err)
	}
	m.logBot(b.CharID, logInfo, "corp",
		"signed with %s — %s (score %.2f: fit %.2f, standing x%.2f, home x%.2f, crowd x%.2f over %d bot(s) there)",
		pick.Corp.Name, pick.Score.Why, pick.Score.Score,
		pick.Score.Fit, pick.Score.Standing, pick.Score.Home, pick.Score.Crowd, pick.Score.Bots)

	// A new employer is a new address to consider. Forgetting makes the look happen at the next plan pass,
	// docked and idle (corphome.go).
	forgetCorpHome(b.CharID)

	// Enmity is read from the employer whenever asked (outlook.go), so changing the badge is the whole
	// change: no stored faction table is rewritten.

	// Presence is corp-keyed. syncPresence reconciles the whole row, so a changed corporation is a withdraw
	// and a re-apply without this code knowing which channels were involved.
	m.syncPresence()
	return nil
}
