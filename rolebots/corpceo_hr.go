package rolebots

import (
	"context"
	"fmt"
	"sort"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The HR power: a CEO answering the applications (chrApplications) on its
// corporation's desk with the status write a player director's corp window
// makes: 6 (accepted) or 4 (rejected). A bot's join completes in the
// application sweep (corpapply.go). An applicant is taken when the work the
// corporation's tags name suits it (corpTagFit) against Charter.HRBar, after the
// hard doors: record, enemy or war employer, lawful vs outlaw, race
// (playerCorpRefusal). The member limit is not a no: the desk is ranked by fit,
// the best take free openings, the next hrWaitlistSize are held (opening pressure),
// the rest are turned down. Personnel Managers (corpceo_roles.go) work the same
// desk (personnelDesk).

const (
	// ceoHRPerThink is how many applicants a CEO takes on per think. Refusals are
	// not counted: a form letter costs nothing.
	ceoHRPerThink = 12
	// hrOfficerPerPass is how many applicants one Personnel Manager takes on per
	// desk pass.
	hrOfficerPerPass = 6
	// hrOfficerDelaySec caps how long an application sits before a Personnel
	// Manager reads it; the CEO's own delay is Charter.HRDelaySec.
	hrOfficerDelaySec = 3600
	// hrInviteRankBoost lifts an invitee above every uninvited applicant
	// in the desk's ranking (fits run 0–5).
	hrInviteRankBoost = 10
	// hrDeskRead bounds one pass's read. Ranking needs the whole desk; a
	// pile bigger than this is worked from its oldest end, pass by pass.
	hrDeskRead = 400
	// ceoEnemyStanding: a corporation this CEO holds at or below this is an
	// enemy for recruitment purposes (relCorpStanding's scale is ±10).
	ceoEnemyStanding = -5.0
)

// hrWaitlistSize is how many good applicants a full corporation keeps on
// the desk for an opening: a tenth of its limit, never fewer than three. The
// applicant side reads the same number (playerCorpRefusal, rule "queue"),
// so a pilot does not queue for a corporation that would only turn it down.
func hrWaitlistSize(limit int) int {
	return max(3, limit/10)
}

// hrBarVacantMul is how much of its applicant bar an empty corporation applies.
// The bar climbs back to the charter's own with the square of how full the
// corporation is, so a near-empty outfit is not starved of applicants.
const hrBarVacantMul = 0.4

// hrEffectiveBar is the bar the desk actually applies, given how full the
// corporation is. The desk (judgeApplicant, headhunt) and the applicant side
// (corpDeskRefusal) both read it, so a pilot does not apply to a desk that
// would turn it down.
func hrEffectiveBar(bar float64, members, limit int) float64 {
	if limit <= 0 {
		return bar
	}
	fill := float64(members) / float64(limit)
	fill = min(max(fill, 0), 1)
	return bar * (hrBarVacantMul + (1-hrBarVacantMul)*fill*fill)
}

// deskBars is each managed corporation's charter bar, published by the last
// desk pass (workHRDesk) so the applicant side need not resolve a charter per
// pilot. Memory only: a restart costs one pass.
var deskBars = map[int64]float64{}

func setDeskBar(corpID int64, bar float64) {
	openingMu.Lock()
	deskBars[corpID] = bar
	openingMu.Unlock()
}

// corpDeskBar is the published charter bar, 0 when no desk has published one
// (a human-run corporation, or a managed one before its first pass).
func corpDeskBar(corpID int64) float64 {
	openingMu.Lock()
	defer openingMu.Unlock()
	return deskBars[corpID]
}

// corpDeskRefusal is the desk's own doors from the applicant's side: the law
// rule and the fit bar, for a corporation whose desk has published one
// (c.DeskBar). A pilot already inside is never refused. The CEO's race shift
// (judgeXenoApplicant's BarMove) is not mirrored. Pure on facts.
func corpDeskRefusal(p pilotIdentity, c CorpFacts) *corpVerdict {
	if c.DeskBar <= 0 || (p.CorpID != 0 && p.CorpID == c.CorpID) {
		return nil
	}
	tags := c.Policy.Tags
	if containsStr(tags, "outlaw") && p.lawfulPole() > 0.5 {
		return &corpVerdict{Rule: "law", Why: "too law-abiding for an outfit that lives outside it"}
	}
	if containsStr(tags, "security") && p.outlawPole() > 0.5 {
		return &corpVerdict{Rule: "law", Why: "an outlaw applying to a security outfit"}
	}
	bar := hrEffectiveBar(c.DeskBar, c.Members, c.MemberLimit)
	if fit := corpTagFit(p, tags); fit < bar {
		return &corpVerdict{Rule: "bar", Why: fmt.Sprintf(
			"%s's desk takes a fit of %.2f and this pilot brings %.2f", c.Name, bar, fit)}
	}
	return nil
}

// openingPressure, per corporation: how many applicants the last think held for
// want of an opening. Read by the skill planner. Memory only, recomputed every
// think.
var (
	openingMu       sync.Mutex
	openingPressure = map[int64]int{}
	// heldLogged remembers which (corp, applicant) holds were already logged,
	// so a full corporation does not write the same "held" line every think.
	heldLogged = map[[2]int64]bool{}
)

func corpOpeningPressure(corpID int64) int {
	openingMu.Lock()
	defer openingMu.Unlock()
	return openingPressure[corpID]
}

// openingsFull: corporations whose last desk pass found no free opening. Kept apart
// from pressure because a full corporation shuts its own door (playerCorpRefusal
// full), leaving pressure at 0. Memory only, recomputed every pass.
var openingsFull = map[int64]bool{}

// corpAtCap reports whether the corporation has hit its member limit.
func corpAtCap(corpID int64) bool {
	openingMu.Lock()
	defer openingMu.Unlock()
	return openingsFull[corpID]
}

func setOpeningsFull(corpID int64, capped bool) {
	openingMu.Lock()
	openingsFull[corpID] = capped
	openingMu.Unlock()
}

// corpWantsOpenings is the single gate for a CEO training or buying the member-
// limit ladder: applicants held at the door, or the limit already reached.
func corpWantsOpenings(corpID int64) bool {
	return corpOpeningPressure(corpID) > 0 || corpAtCap(corpID)
}

// hrApplicant is one application on the desk.
type hrApplicant struct {
	CharID    int64
	Name      string
	CorpID    int64
	SecStatus float64
	RaceID    int64
	// BloodlineID is what a turbo xenophobe CEO reads (tolerance.go).
	BloodlineID int64
	OnRoster    bool
	AppliedAt   int64 // win32 ticks
	// Text is what the applicant wrote. Every status write re-passes it, because
	// UpdateApplication stores what it is given and an empty string wipes the letter.
	Text string
}

// hrVerdict is the CEO's answer to one applicant.
type hrVerdict struct {
	Accept bool
	Hold   bool
	Rule   string
	Why    string
	Fit    float64
}

// judgeApplicant is the pure half of the HR power: the hard doors, then the fit
// against the bar. open says whether the corporation has room (the desk passes
// true and lets triageDesk rank openings). xeno is the CEO's tolerance applied to
// this applicant (judgeXenoApplicant).
func judgeApplicant(ch Charter, facts CorpFacts, a hrApplicant, p pilotIdentity, open bool,
	enemy bool, xeno xenoApplicant) hrVerdict {
	// A capsuleer in person meets one door: the CEO's tolerance (judgeXenoApplicant).
	// Record, employer and debt sort bots only; the fit bar does not apply to players.
	if !a.OnRoster {
		if xeno.Reject {
			return hrVerdict{Rule: xeno.Rule, Why: xeno.Why}
		}
		fit := ch.HRBar + xeno.BarMove
		if !open {
			return hrVerdict{Hold: true, Rule: "player", Fit: fit, Why: "a capsuleer, but there is no opening for them yet"}
		}
		return hrVerdict{Accept: true, Rule: "player", Fit: fit, Why: "a capsuleer applying in person"}
	}
	if a.SecStatus < ch.ApplicantMinSec {
		return hrVerdict{Rule: "record", Why: fmt.Sprintf(
			"a security status of %.1f is below what this corporation will take on (%.1f)",
			a.SecStatus, ch.ApplicantMinSec)}
	}
	if enemy {
		return hrVerdict{Rule: "enemy", Why: "works for a corporation this one counts as an enemy"}
	}
	if xeno.Reject {
		return hrVerdict{Rule: xeno.Rule, Why: xeno.Why}
	}
	tags := facts.Policy.Tags
	if a.OnRoster {
		if containsStr(tags, "outlaw") && p.lawfulPole() > 0.5 {
			return hrVerdict{Rule: "law", Why: "too law-abiding for an outfit that lives outside it"}
		}
		if containsStr(tags, "security") && p.outlawPole() > 0.5 {
			return hrVerdict{Rule: "law", Why: "an outlaw applying to a security outfit"}
		}
	}
	// full and queue are the applicant's own reasons not to apply; the desk ranks
	// openings itself.
	if v := playerCorpRefusal(p, facts); v != nil && v.Rule != "full" && v.Rule != "queue" {
		return hrVerdict{Rule: v.Rule, Why: v.Why}
	}
	// A pilot who plays the CEO's hours is worth more to it than one it
	// would never see (timezone.go).
	fit := corpTagFit(p, tags) * tzCorpMul(a.CharID, facts.CorpID)
	base := hrEffectiveBar(ch.HRBar, facts.Members, facts.MemberLimit)
	bar := base + xeno.BarMove
	if fit < bar {
		why := fmt.Sprintf("does not suit the corporation — wants different work from what it does (fit %.2f, the bar is %.2f)", fit, bar)
		if xeno.BarMove > 0 {
			why += " — " + xeno.Why
		}
		return hrVerdict{Rule: "fit", Fit: fit, Why: why}
	}
	if !open {
		return hrVerdict{Hold: true, Rule: "full", Fit: fit, Why: "a good fit, but there is no opening for them yet"}
	}
	why := fmt.Sprintf("a good fit for the work (%.2f)", fit)
	if xeno.BarMove < 0 && fit < base {
		why += " — " + xeno.Why
	}
	return hrVerdict{Accept: true, Rule: "fit", Fit: fit, Why: why}
}

// What the desk does with one judged application.
const (
	hrLeave          = iota // good, an opening is theirs, but this reader is out of acceptances
	hrAccept                // take them on
	hrHold                  // on the waiting list
	hrReject                // a door or the bar
	hrRejectStale           // waited HRHoldSec on the list and no opening came free
	hrRejectOverflow        // good, but behind a full waiting list
)

// hrJudged is one application with its verdict, as triageDesk reads it.
type hrJudged struct {
	A hrApplicant
	V hrVerdict
}

// triageDesk is the pure half of the desk: every judged application gets an
// action. Those past the doors rank by fit (older first on a tie): the best
// `room` are taken, up to `accepts` a pass, the next `waitlist` are held unless
// older than staleBefore, the rest are turned down. room < 0 reads as none.
func triageDesk(desk []hrJudged, room int64, unlimited bool, waitlist, accepts int, staleBefore int64) []int {
	out := make([]int, len(desk))
	var good []int
	for i, j := range desk {
		if j.V.Accept {
			good = append(good, i)
		} else {
			out[i] = hrReject
		}
	}
	sort.SliceStable(good, func(x, y int) bool {
		a, b := desk[good[x]], desk[good[y]]
		if a.V.Fit != b.V.Fit {
			return a.V.Fit > b.V.Fit
		}
		return a.A.AppliedAt < b.A.AppliedAt
	})
	if unlimited {
		room = int64(len(good))
	}
	if room < 0 {
		room = 0
	}
	taken := 0
	for rank, i := range good {
		switch {
		case int64(rank) < room:
			if taken < accepts {
				out[i] = hrAccept
				taken++
			} else {
				out[i] = hrLeave
			}
		case int64(rank) < room+int64(waitlist):
			if desk[i].A.AppliedAt < staleBefore {
				out[i] = hrRejectStale
			} else {
				out[i] = hrHold
			}
		default:
			out[i] = hrRejectOverflow
		}
	}
	return out
}

// hrReader is whoever is working the desk this pass: the CEO at its think,
// or one of its Recruitment Officers on the staff desk (corpstaff.go), whose
// name rides the context onto every ledger line (staffActor).
type hrReader struct {
	CharID   int64
	Accepts  int
	DelaySec int64
}

// hrDeskBusy keeps the CEO's think and an officer pass off the same desk at
// once: each counts free openings once and spends them, so two at once over-accept.
var hrDeskBusy sync.Map // corpID to struct{}

// ceoHR is the CEO working its own desk at its think.
func (m *Mod) ceoHR(ctx context.Context, c *corpCEO, ch Charter) error {
	return m.workHRDesk(ctx, c, ch, hrReader{CharID: c.CharID, Accepts: ceoHRPerThink, DelaySec: ch.HRDelaySec})
}

// workHRDesk answers the applications on the desk.
func (m *Mod) workHRDesk(ctx context.Context, c *corpCEO, ch Charter, rd hrReader) error {
	if _, busy := hrDeskBusy.LoadOrStore(c.CorpID, struct{}{}); busy {
		return nil
	}
	defer hrDeskBusy.Delete(c.CorpID)

	cutoff := win32Now() - rd.DelaySec*10_000_000
	rows, err := m.db.QueryContext(ctx, `
		SELECT a.characterID, COALESCE(ch.characterName, ''), COALESCE(ch.corporationID, 0),
		       COALESCE(ch.securityRating, 0), COALESCE(bl.raceID, 0), COALESCE(ch.bloodlineID, 0),
		       CASE WHEN r.characterID IS NULL THEN 0 ELSE 1 END, a.applicationDateTime,
		       COALESCE(a.applicationText, '')
		  FROM chrApplications a
		  JOIN character_ ch ON ch.characterID = a.characterID
		  LEFT JOIN chrBloodlines bl ON bl.bloodlineID = ch.bloodlineID
		  LEFT JOIN rolebot_characters r ON r.characterID = a.characterID
		 WHERE a.corporationID = ? AND a.deleted = 0 AND a.status = ?
		   AND (a.applicationDateTime <= ? OR r.characterID IS NULL)
		 ORDER BY a.applicationDateTime
		 LIMIT ?`, c.CorpID, evedb.AppAppliedByCharacter, cutoff, hrDeskRead)
	if err != nil {
		if isMissingTable(err) {
			return nil // nobody has ever applied anywhere on this server
		}
		return fmt.Errorf("ceoHR read: %w", err)
	}
	var desk []hrApplicant
	for rows.Next() {
		var a hrApplicant
		var onRoster int
		if err := rows.Scan(&a.CharID, &a.Name, &a.CorpID, &a.SecStatus, &a.RaceID, &a.BloodlineID,
			&onRoster, &a.AppliedAt, &a.Text); err != nil {
			rows.Close()
			return fmt.Errorf("ceoHR scan: %w", err)
		}
		a.OnRoster = onRoster != 0 || m.isSynthetic(a.CharID)
		desk = append(desk, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	// Openings are counted once and spent as the desk is answered: an accept only
	// makes an offer (the join is in corpapply.go), so per-applicant checks over-accept.
	free, unlimited, err := m.corpFreeOpenings(ctx, c.CorpID)
	if err != nil {
		return err
	}
	// Offers made past the limit are pilots waiting at the door and count as opening
	// pressure, like held applications.
	held := 0
	if !unlimited && free < 0 {
		held = int(-free)
	}
	setOpeningsFull(c.CorpID, !unlimited && free <= 0)
	setDeskBar(c.CorpID, ch.HRBar)
	if len(desk) == 0 {
		m.setOpeningPressure(c.CorpID, held)
		return nil
	}

	// The CEO's own race and bloodline, once for the whole desk: officers work to
	// the CEO's rules.
	var ceoRace, ceoBloodline int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(bl.raceID, 0), COALESCE(c.bloodlineID, 0)
		  FROM character_ c LEFT JOIN chrBloodlines bl ON bl.bloodlineID = c.bloodlineID
		 WHERE c.characterID = ?`, c.CharID).Scan(&ceoRace, &ceoBloodline)
	ceoTune := defaultTuning()
	if traits, err := getTraits(ctx, m.db, c.CharID); err == nil {
		ceoTune = resolveTuning(traits)
	}
	invited := m.corpInvitees(ctx, c.CorpID)
	judged := make([]hrJudged, 0, len(desk))
	for _, a := range desk {
		p := pilotIdentity{CharID: a.CharID, RaceID: a.RaceID, Tune: defaultTuning(), Traits: map[string]int{}}
		if a.OnRoster {
			if traits, err := getTraits(ctx, m.db, a.CharID); err == nil {
				p.Traits = traits
				p.Tune = resolveTuning(traits)
			}
		}
		enemy := a.CorpID != 0 && (relCorpStanding(c.CorpID, a.CorpID) <= ceoEnemyStanding ||
			relAtWar(c.CorpID, 0, a.CorpID, 0))
		v := judgeApplicant(ch, *facts, a, p, true, enemy,
			judgeXenoApplicant(ceoRace, ceoBloodline, ceoTune, a.RaceID, a.BloodlineID))
		// A pilot who once left holding the corporation's money is not
		// taken back (corplibrary.go purchase orders).
		if a.OnRoster && m.defaultedOnCorp(ctx, c.CorpID, a.CharID) {
			v = hrVerdict{Rule: "default", Why: "left this corporation once holding its money"}
		}
		// A pilot who walked off with an Armory kit is not taken back, human
		// or not (corparmory_human.go).
		if barred, why := m.kitBarred(ctx, c.CorpID, a.CharID); barred && why == "theft" {
			v = hrVerdict{Rule: "theft", Why: "stole from this corporation's Armory"}
		}
		// A pilot this corporation's own Recruitment Officer invited (headhunting,
		// corpstaff_dept.go) is taken on its fit and heads the queue for an opening.
		if invited[a.CharID] && v.Rule == "fit" && !v.Accept {
			v = hrVerdict{Accept: true, Rule: "invited", Fit: v.Fit,
				Why: fmt.Sprintf("invited by the corporation (fit %.2f)", v.Fit)}
		}
		if invited[a.CharID] && v.Accept {
			v.Fit += hrInviteRankBoost
		}
		// A human heads the queue like an invitee: ranking one on the default
		// personality would put them behind every rostered fit.
		if !a.OnRoster && v.Accept && !invited[a.CharID] {
			v.Fit += hrInviteRankBoost
		}
		judged = append(judged, hrJudged{A: a, V: v})
	}

	waitlist := hrWaitlistSize(facts.MemberLimit)
	staleBefore := win32Now() - (rd.DelaySec+ch.HRHoldSec)*10_000_000
	plan := triageDesk(judged, free, unlimited, waitlist, rd.Accepts, staleBefore)

	overflow := 0
	for i, j := range judged {
		a, v := j.A, j.V
		switch plan[i] {
		case hrLeave:
			// Next pass: an opening is theirs, this reader is out of acceptances.
		case hrHold:
			held++
			key := [2]int64{c.CorpID, a.CharID}
			openingMu.Lock()
			already := heldLogged[key]
			heldLogged[key] = true
			openingMu.Unlock()
			// The memory is lost on a restart: the ledger says whether this hold was
			// already announced within the patience window.
			if !already && clock.Unix()-m.lastCEOActionFor(ctx, c.CorpID, "hold", a.CharID) < ch.HRHoldSec {
				already = true
			}
			if !already {
				m.ceoLog(ctx, c, powerHR, "hold", a.CharID, v.Fit, "%s",
					fmt.Sprintf("put %s on the waiting list — a good fit (%.2f), but there is no opening for them yet", a.Name, v.Fit))
				if !a.OnRoster {
					m.mailApplicant(ctx, c, rd, a, applicantHeld, "")
				}
			}
		case hrAccept:
			if err := evedb.UpdateApplication(ctx, m.db, a.CharID, c.CorpID, rd.CharID,
				evedb.AppAcceptedByCorporation, a.Text); err != nil {
				return fmt.Errorf("ceoHR accept: %w", err)
			}
			if h := modapi.Corp().ApplicationChanged; h != nil {
				h(c.CorpID, a.CharID, evedb.AppAppliedByCharacter, evedb.AppAcceptedByCorporation, rd.CharID)
			}
			m.forgetHeld(c.CorpID, a.CharID)
			m.ceoLog(ctx, c, powerHR, "accept", a.CharID, v.Fit, "%s",
				fmt.Sprintf("accepted %s's application — %s", a.Name, v.Why))
			if !a.OnRoster {
				m.mailApplicant(ctx, c, rd, a, applicantOffered, "")
			}
		case hrRejectStale, hrRejectOverflow:
			// No opening is not a verdict on the pilot: a rostered applicant's
			// application lapses (the short cooldown), it is not refused. A
			// capsuleer's is refused in the corp window and told why by mail.
			if a.OnRoster {
				m.lapseApplication(ctx, c, facts.Name, a.CharID)
			} else {
				if err := m.rejectApplication(ctx, c, rd, a.CharID, a.Text); err != nil {
					return err
				}
				if plan[i] == hrRejectOverflow {
					m.mailApplicant(ctx, c, rd, a, applicantOverflow, "")
				} else {
					m.mailApplicant(ctx, c, rd, a, applicantStale, "")
				}
			}
			if plan[i] == hrRejectOverflow {
				overflow++ // one ledger line for the lot, below
			} else {
				m.ceoLog(ctx, c, powerHR, "lapse", a.CharID, v.Fit, "%s",
					fmt.Sprintf("let %s go — waited %s on the list and no opening came free", a.Name, fmtDuration(ch.HRHoldSec)))
			}
		default:
			if err := m.rejectApplication(ctx, c, rd, a.CharID, a.Text); err != nil {
				return err
			}
			if !a.OnRoster {
				m.mailApplicant(ctx, c, rd, a, applicantRefused, v.Why)
			}
			m.ceoLog(ctx, c, powerHR, "reject", a.CharID, v.Fit, "%s",
				fmt.Sprintf("turned down %s — %s", a.Name, v.Why))
		}
	}
	if overflow > 0 {
		m.ceoLog(ctx, c, powerHR, "lapse", 0, float64(overflow), "%s",
			fmt.Sprintf("let %d applicant(s) go — %s is full and its waiting list of %d is taken by better fits",
				overflow, facts.Name, waitlist))
	}
	m.setOpeningPressure(c.CorpID, held)
	return nil
}

// rejectApplication is the corp window's refusal: status 4, the row soft
// deleted, the applicant told live through the corp bridge.
func (m *Mod) rejectApplication(ctx context.Context, c *corpCEO, rd hrReader, charID int64, text string) error {
	if err := evedb.UpdateApplication(ctx, m.db, charID, c.CorpID, rd.CharID,
		evedb.AppRejectedByCorporation, text); err != nil {
		return fmt.Errorf("ceoHR reject: %w", err)
	}
	_ = evedb.SoftDeleteApplication(ctx, m.db, charID, c.CorpID)
	if h := modapi.Corp().ApplicationChanged; h != nil {
		h(c.CorpID, charID, evedb.AppAppliedByCharacter, evedb.AppRejectedByCorporation, rd.CharID)
	}
	m.forgetHeld(c.CorpID, charID)
	return nil
}

// lapseApplication closes a rostered applicant's application for want of an
// opening: the game row goes and the module row closes expired, whose cooldown
// is the short one (corpapply.go corpApplyBlocked), so the pilot shops again soon.
func (m *Mod) lapseApplication(ctx context.Context, c *corpCEO, corpName string, charID int64) {
	m.withdrawApplication(ctx, pendingApplicant{CharID: charID, CorpID: c.CorpID, CorpName: corpName},
		corpAppExpired, "no opening came free")
}

func (m *Mod) setOpeningPressure(corpID int64, held int) {
	openingMu.Lock()
	openingPressure[corpID] = held
	openingMu.Unlock()
}

// corpFreeOpenings is how many more applicants a corporation can take on: its
// member limit, less its members, less the offers made that have not become
// joins yet. unlimited is true for a corporation with no limit (an NPC one).
func (m *Mod) corpFreeOpenings(ctx context.Context, corpID int64) (free int64, unlimited bool, err error) {
	_, used, limit, err := evedb.CorpHasOpening(ctx, m.db, corpID)
	if err != nil {
		return 0, false, err
	}
	if limit <= 0 {
		return 0, true, nil
	}
	var offers int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM chrApplications a
		  JOIN character_ ch ON ch.characterID = a.characterID
		 WHERE a.corporationID = ? AND a.deleted = 0 AND a.status = ?
		   AND ch.corporationID <> a.corporationID`,
		corpID, evedb.AppAcceptedByCorporation).Scan(&offers); err != nil && !isMissingTable(err) {
		return 0, false, fmt.Errorf("corpFreeOpenings offers: %w", err)
	}
	return limit - used - offers, false, nil
}

func (m *Mod) forgetHeld(corpID, charID int64) {
	openingMu.Lock()
	delete(heldLogged, [2]int64{corpID, charID})
	openingMu.Unlock()
}

func init() {
	registerCharForget("heldLogged", func(id int64) {
		openingMu.Lock()
		for k := range heldLogged {
			if k[1] == id {
				delete(heldLogged, k)
			}
		}
		openingMu.Unlock()
	})
	registerCorpForget("openingPressure/openingsFull/heldLogged/deskBars", func(id int64) {
		openingMu.Lock()
		delete(openingPressure, id)
		delete(openingsFull, id)
		delete(deskBars, id)
		for k := range heldLogged {
			if k[0] == id {
				delete(heldLogged, k)
			}
		}
		openingMu.Unlock()
	})
}
