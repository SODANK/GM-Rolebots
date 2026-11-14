package rolebots

// The alliance power: what a bot CEO does about its corporation's alliance (corpceo.go ceoThink, after
// diplomacy and before war).
//
// An alliance here owns no wallet, hangar or roles: it is corporations run by an executor corporation's
// directors, elected one corporation one vote (db/alliancedb.go RecomputeAllianceExecutor).
// Member duties: pay the maintenance bill to CONCORD and the executor's tithes (alliancetithe.go),
// declare executor support (alliancevote.go), leave when the executor turns or bills are beyond it.
// Executor duties: answer applications, kick, and behind switches roll up relations, weigh war requests
// (alliancerel.go, alliancewar.go) and name sovereignty targets (alliancesov.go).
// An allianceless CEO applies where its allies are, or founds one (alliancefound.go).

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

const powerAlliance = "alliance"

const (
	// allianceSeekAppetite is the AllianceAppetite a CEO needs before it
	// follows its allies into an alliance at all.
	allianceSeekAppetite = 0.4
	// allianceApplyText is what a bot writes on its application.
	allianceApplyText = "Our allies fly under your banner; we ask to fly beside them."
)

// allianceSeat is what the power reads about the corporation's alliance.
type allianceSeat struct {
	AllianceID     int64
	Name, Ticker   string
	ExecutorCorpID int64
	Dictatorial    bool
	IsExecutor     bool
	JoinedAt       int64 // unix seconds (converted from the core's Win32 FILETIME)
	ChosenExec     int64
}

// allianceOf reads the corporation's alliance seat, nil when it is in none.
func (m *Mod) allianceOf(ctx context.Context, corpID int64) (*allianceSeat, error) {
	var s allianceSeat
	err := m.db.QueryRowContext(ctx, `
		SELECT a.allianceID, a.allianceName, a.shortName, a.executorCorpID, a.dictatorial,
		       COALESCE(am.joinedAt, 0), COALESCE(am.chosenExecutorID, 0)
		  FROM corporation c
		  JOIN alliances a ON a.allianceID = c.allianceID AND a.deleted = 0
		  LEFT JOIN allianceMembers am ON am.corporationID = c.corporationID
		 WHERE c.corporationID = ?`, corpID).Scan(&s.AllianceID, &s.Name, &s.Ticker, &s.ExecutorCorpID,
		&s.Dictatorial, &s.JoinedAt, &s.ChosenExec)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return nil, nil
		}
		return nil, fmt.Errorf("allianceOf %d: %w", corpID, err)
	}
	s.IsExecutor = s.ExecutorCorpID == corpID
	s.JoinedAt = allianceJoinedAtUnix(s.JoinedAt)
	return &s, nil
}

// allianceJoinedAtUnix turns allianceMembers.joinedAt into unix seconds. The core writes a Win32 FILETIME
// (JoinAlliance) while every module stamp is unix seconds; compared raw, the join announcement would
// repeat on every think. 0 stays 0.
func allianceJoinedAtUnix(ft int64) int64 {
	if ft <= 0 {
		return 0
	}
	return win32ToUnix(ft)
}

// ceoAlliance is one think's pass.
func (m *Mod) ceoAlliance(ctx context.Context, c *corpCEO, ch Charter) error {
	seat, err := m.allianceOf(ctx, c.CorpID)
	if err != nil {
		return err
	}
	if seat == nil {
		return m.allianceSeek(ctx, c, ch)
	}
	// Joined since the last think: say so once, and let the fleets and the ROE read the new bloc now
	// rather than on the next relations sweep. The book is the second memory: a 'joined' row since the
	// corporation last left this alliance means it was said already.
	if seat.JoinedAt > 0 && m.lastCEOActionAt(ctx, c.CorpID, "joined") < seat.JoinedAt &&
		!m.allianceJoinAnnounced(ctx, seat.AllianceID, c.CorpID) {
		m.ceoLog(ctx, c, powerAlliance, "joined", seat.AllianceID, 0, "joined %s [%s]", seat.Name, seat.Ticker)
		m.allianceBook(ctx, seat.AllianceID, c.CorpID, "joined", c.CorpID, 0, "%s joined", ownerNameOf(ctx, m.db, c.CorpID))
		m.refreshRelations(ctx)
	}
	if err := m.payAllianceDues(ctx, c, ch, seat); err != nil {
		return err
	}
	if err := m.payTithes(ctx, c, ch, seat.AllianceID); err != nil {
		return err
	}
	members, err := m.allianceMemberFactsOf(ctx, seat.AllianceID, c.CorpID)
	if err != nil {
		return err
	}
	if left, err := m.allianceConsiderLeaving(ctx, c, ch, seat, members); err != nil || left {
		return err
	}
	if err := m.allianceVote(ctx, c, ch, seat, members); err != nil {
		return err
	}
	// Every member answers the alliance's defend calls (gangalliance.go).
	if err := m.allianceAnswerDefend(ctx, c, seat); err != nil {
		return err
	}
	if !seat.IsExecutor {
		return nil
	}
	return m.allianceExecutorDuties(ctx, c, ch, seat, members)
}

// allianceJoinAnnounced reports whether the alliance book already holds this corporation's 'joined' row,
// newer than its last leaving of the same alliance ('left' by it, or 'kicked' with it as subject).
func (m *Mod) allianceJoinAnnounced(ctx context.Context, allianceID, corpID int64) bool {
	var joined, gone int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(CASE WHEN verb = 'joined' AND actorCorpID = ? THEN at END), 0),
		       COALESCE(MAX(CASE WHEN (verb = 'left' AND actorCorpID = ?) OR (verb = 'kicked' AND subjectID = ?) THEN at END), 0)
		  FROM rolebot_alliance_book
		 WHERE allianceID = ? AND verb IN ('joined', 'left', 'kicked')`,
		corpID, corpID, corpID, allianceID).Scan(&joined, &gone); err != nil {
		return false
	}
	return joined > 0 && joined >= gone
}

// ── member duties ───────────────────────────────────────────────────────────

// payAllianceDues pays the corporation's own open maintenance bills to
// CONCORD. One it cannot carry is left for the core's bill sweep to judge.
func (m *Mod) payAllianceDues(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT billID, amount FROM bills
		 WHERE debtorID = ? AND billTypeID = ? AND paid = 0
		 ORDER BY dueDateTime`, c.CorpID, evedb.BillTypeAllianceMaintainance)
	if err != nil {
		return nil // no bills table is no bills
	}
	type bill struct {
		id     int64
		amount float64
	}
	var bills []bill
	for rows.Next() {
		var b bill
		if rows.Scan(&b.id, &b.amount) == nil {
			bills = append(bills, b)
		}
	}
	rows.Close()
	hook := modapi.Corp().PayBill
	for _, b := range bills {
		if hook == nil {
			return nil
		}
		if !m.canPay(ctx, c.CorpID, ch, b.amount, 0) {
			m.logBot(c.CharID, logInfo, "ceo", "alliance dues %s left unpaid: the treasury cannot carry it", iskString(b.amount))
			continue
		}
		if err := hook(b.id, c.CharID); err != nil {
			m.logBot(c.CharID, logError, "ceo", "alliance dues bill %d: %v", b.id, err)
			continue
		}
		m.corpBook(ctx, c.CorpID, lineAllianceDues, -b.amount, evedb.OwnerCONCORDBills, seat.Name)
		m.ceoLog(ctx, c, powerAlliance, "dues", seat.AllianceID, b.amount, "paid %s alliance dues to CONCORD", iskString(b.amount))
	}
	return nil
}

// allianceMemberFactsOf reads every member of the alliance as the viewing
// corporation sees it (alliancevote.go allianceMemberFacts).
func (m *Mod) allianceMemberFactsOf(ctx context.Context, allianceID, viewer int64) ([]allianceMemberFacts, error) {
	members, err := evedb.ListAllianceMembers(ctx, m.db, allianceID)
	if err != nil {
		return nil, fmt.Errorf("alliance members: %w", err)
	}
	now := clock.Unix()
	out := make([]allianceMemberFacts, 0, len(members))
	for _, mb := range members {
		f := allianceMemberFacts{
			CorpID: mb.CorporationID, Name: mb.CorporationName, Pilots: mb.Pilots,
			IsExecutor: mb.IsExecutor, ChosenExec: mb.ChosenExecutorID, Lead: 0.5,
		}
		var joinedAt int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT c.ceoID, COALESCE(c.balance, 0), COALESCE(am.joinedAt, 0)
			  FROM corporation c LEFT JOIN allianceMembers am ON am.corporationID = c.corporationID
			 WHERE c.corporationID = ?`, mb.CorporationID).Scan(&f.CEOID, &f.Wallet, &joinedAt)
		if joinedAt = allianceJoinedAtUnix(joinedAt); joinedAt > 0 {
			f.TenureSec = now - joinedAt
		}
		if _, ok := managedCEO(mb.CorporationID); ok && f.CEOID != 0 {
			if traits, err := getTraits(ctx, m.db, f.CEOID); err == nil {
				f.Lead = posPole(traitScale(traits[TraitLeadership]))
			}
		} else {
			f.HumanCEO = true
		}
		f.OurStanding = relCorpStanding(viewer, mb.CorporationID)
		f.TheirStanding = relCorpStanding(mb.CorporationID, viewer)
		f.RefusedTithes = m.titheRefusals(ctx, allianceID, mb.CorporationID)
		out = append(out, f)
	}
	return out, nil
}

// allianceVote declares the corporation's executor support when its pick
// differs from what it last declared, no oftener than DwellSec.
func (m *Mod) allianceVote(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) error {
	if seat.Dictatorial {
		return nil // the seat is frozen; a vote is noise
	}
	pick, why := allianceSupportPick(c.CorpID, members, seat.ExecutorCorpID, ch)
	if pick == 0 || pick == seat.ChosenExec {
		return nil
	}
	if last := m.lastCEOActionAt(ctx, c.CorpID, "support"); last > 0 && clock.Unix()-last < ch.DwellSec {
		return nil
	}
	oldExec := seat.ExecutorCorpID
	newExec, err := evedb.DeclareExecutorSupport(ctx, m.db, seat.AllianceID, c.CorpID, pick)
	if err != nil {
		return fmt.Errorf("declare support: %w", err)
	}
	m.allianceChanged("member", seat.AllianceID, c.CorpID)
	m.ceoLog(ctx, c, powerAlliance, "support", pick, 0, "declared executor support for %s — %s", ownerNameOf(ctx, m.db, pick), why)
	m.allianceBook(ctx, seat.AllianceID, c.CorpID, "support", pick, 0, "%s backs %s for executor", ownerNameOf(ctx, m.db, c.CorpID), ownerNameOf(ctx, m.db, pick))
	if newExec != oldExec {
		m.allianceChanged("details", seat.AllianceID, 0)
		m.allianceInvalidateNames()
		m.ceoLog(ctx, c, powerAlliance, "executor", newExec, 0, "the executor seat passed from %s to %s",
			ownerNameOf(ctx, m.db, oldExec), ownerNameOf(ctx, m.db, newExec))
		m.allianceBook(ctx, seat.AllianceID, c.CorpID, "executor", newExec, 0, "the executor seat passed to %s", ownerNameOf(ctx, m.db, newExec))
		seat.ExecutorCorpID, seat.IsExecutor = newExec, newExec == c.CorpID
	}
	return nil
}

// allianceConsiderLeaving walks out when allianceLeaveVerdict says so.
func (m *Mod) allianceConsiderLeaving(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) (bool, error) {
	v := allianceMemberView{IsExecutor: seat.IsExecutor, Now: clock.Unix()}
	if seat.ExecutorCorpID != 0 && seat.ExecutorCorpID != c.CorpID {
		v.ExecStandingToUs = relCorpStanding(seat.ExecutorCorpID, c.CorpID)
		// When the executor's ledger turned: its row on us carries its last tier change. A human executor has
		// no ledger, so its declared standing counts at once.
		if rows, err := m.relRowsOf(ctx, seat.ExecutorCorpID); err == nil {
			if r, ok := rows[c.CorpID]; ok {
				v.ExecSouredSince = r.TierAt
			}
		}
	}
	v.RefusedTithes = m.titheRefusals(ctx, seat.AllianceID, c.CorpID)
	if n, err := evedb.CountActiveDeclaredWars(ctx, m.db, seat.AllianceID); err == nil {
		v.AllianceWars = int(n)
	}
	leave, why := allianceLeaveVerdict(v, ch)
	if !leave {
		return false, nil
	}
	left, _, err := m.allianceRemoveCorp(ctx, seat.AllianceID, c.CorpID)
	if err != nil {
		return false, fmt.Errorf("leave alliance: %w", err)
	}
	if left == 0 {
		return false, nil
	}
	m.refreshRelations(ctx)
	m.ceoLog(ctx, c, powerAlliance, "leave", seat.AllianceID, 0, "left %s — %s", seat.Name, why)
	m.allianceBook(ctx, seat.AllianceID, c.CorpID, "left", c.CorpID, 0, "%s left — %s", ownerNameOf(ctx, m.db, c.CorpID), why)
	return true, nil
}

// ── the seeker ──────────────────────────────────────────────────────────────

// allianceSeek is the allianceless CEO's pass: apply where its mutual allies
// already are, else consider founding.
func (m *Mod) allianceSeek(ctx context.Context, c *corpCEO, ch Charter) error {
	// A corporation reaching for unclaimed 0.0 needs an alliance to hold anything there, so it seeks
	// one at a lower appetite (frontier.go).
	frontier, _ := m.frontierAmbition(ctx, c, ch)
	if ch.AllianceAppetite < allianceSeekAppetite && !(frontier && ch.AllianceAppetite >= sovSeekAppetite) {
		return nil
	}
	allies := relAlliesAt(c.CorpID, allianceSeekAllyAt())
	// The alliances our allies are in, most allies first; for a frontier corporation, one founded to
	// hold 0.0 counts sovSeekPull allies more.
	count := map[int64]int{}
	for _, a := range allies {
		if aid := m.corpAllianceID(ctx, a); aid != 0 {
			count[aid]++
		}
	}
	if frontier {
		for aid := range count {
			if m.allianceSovMotive(ctx, aid) {
				count[aid] += sovSeekPull
			}
		}
	}
	// An alliance that turned us away is passed over for allianceRejectMemorySec.
	order := allianceSeekOrder(count, m.allianceNoteRejections(ctx, c), clock.Unix())
	if len(order) == 0 {
		return m.allianceConsiderFounding(ctx, c, ch, allies)
	}
	var open int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM allianceApplications
		 WHERE corporationID = ? AND deleted = 0 AND state IN (?, ?)`,
		c.CorpID, evedb.AllianceApplicationNew, evedb.AllianceApplicationAccepted).Scan(&open)
	if open > 0 {
		return nil
	}
	if last := m.lastCEOActionAt(ctx, c.CorpID, "apply"); last > 0 && clock.Unix()-last < ch.ProposalEverySec {
		return nil
	}
	for _, best := range order {
		if err := evedb.AllianceApplyRefusal(ctx, m.db, c.CorpID, best); err != nil {
			m.logBot(c.CharID, logInfo, "ceo", "alliance application to %d refused: %v", best, err)
			continue
		}
		if err := evedb.ApplyToJoinAlliance(ctx, m.db, c.CorpID, best, allianceApplyText); err != nil {
			m.logBot(c.CharID, logInfo, "ceo", "alliance application to %d: %v", best, err)
			continue
		}
		m.allianceChanged("app", best, c.CorpID)
		m.ceoLog(ctx, c, powerAlliance, "apply", best, 0, "applied to join %s, where %d of our allies fly", ownerNameOf(ctx, m.db, best), count[best])
		m.allianceBook(ctx, best, c.CorpID, "applied", c.CorpID, 0, "%s applied to join", ownerNameOf(ctx, m.db, c.CorpID))
		return nil
	}
	return nil
}

// allianceRejectMemorySec is how long the seeker passes over an alliance that rejected it.
const allianceRejectMemorySec = 30 * 86400

// allianceSeekOrder ranks the alliances the seeker's allies fly in: most allies first, ties to the lower
// allianceID. An alliance in rejectedAt (allianceID to unix time of its last rejection) within
// allianceRejectMemorySec of now is left out. Pure.
func allianceSeekOrder(count map[int64]int, rejectedAt map[int64]int64, now int64) []int64 {
	var out []int64
	for aid := range count {
		if at, ok := rejectedAt[aid]; ok && now-at < allianceRejectMemorySec {
			continue
		}
		out = append(out, aid)
	}
	sort.Slice(out, func(i, j int) bool {
		if count[out[i]] != count[out[j]] {
			return count[out[i]] > count[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// allianceNoteRejections logs 'apply.rejected' (subject: the allianceID) for each answer the
// applications table holds that the ledger has not seen yet, and returns when each alliance last
// rejected this corporation. The ledger is the memory: the core overwrites the row on a new application.
func (m *Mod) allianceNoteRejections(ctx context.Context, c *corpCEO) map[int64]int64 {
	seen := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT subjectID, MAX(at) FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND verb = 'apply.rejected' GROUP BY subjectID`, c.CorpID)
	if err == nil {
		for rows.Next() {
			var aid, at int64
			if rows.Scan(&aid, &at) == nil {
				seen[aid] = at
			}
		}
		rows.Close()
	}
	type answer struct{ aid, at int64 }
	var fresh []answer
	rows, err = m.db.QueryContext(ctx, `
		SELECT allianceID, COALESCE(stateDateTime, 0) FROM allianceApplications
		 WHERE corporationID = ? AND deleted = 0 AND state = ?`, c.CorpID, evedb.AllianceApplicationRejected)
	if err != nil {
		return seen
	}
	for rows.Next() {
		var a answer
		if rows.Scan(&a.aid, &a.at) == nil {
			a.at = allianceJoinedAtUnix(a.at) // the core stamps a Win32 FILETIME
			if last, ok := seen[a.aid]; !ok || last < a.at {
				fresh = append(fresh, a)
			}
		}
	}
	rows.Close()
	for _, a := range fresh {
		m.ceoLog(ctx, c, powerAlliance, "apply.rejected", a.aid, 0, "%s rejected our application; we look elsewhere for %d days",
			ownerNameOf(ctx, m.db, a.aid), allianceRejectMemorySec/86400)
		seen[a.aid] = clock.Unix()
	}
	return seen
}

// relAlliesAt lists corpID's allies at a line: each holds the other at `at` or better. relAlliesOf is
// the same at StandingDevoted. Sorted.
func relAlliesAt(corpID int64, at float64) []int64 {
	if corpID == 0 {
		return nil
	}
	s := loadRelSnapshot()
	if s == nil {
		return nil
	}
	var out []int64
	for k, v := range s.corp {
		if k[0] != corpID || k[1] == corpID || v < at {
			continue
		}
		if back, ok := s.corp[[2]int64{k[1], corpID}]; ok && back >= at {
			out = append(out, k[1])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ── executor duties ─────────────────────────────────────────────────────────

// allianceExecutorDuties is the seat's work: applications, kicks, and the
// switched-on layers above them.
func (m *Mod) allianceExecutorDuties(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) error {
	if err := m.allianceAnswerApplications(ctx, c, ch, seat, members); err != nil {
		return err
	}
	if err := m.allianceKicks(ctx, c, ch, seat, members); err != nil {
		return err
	}
	// The switched-on layers (alliancerel.go, alliancewar.go).
	if err := m.allianceRollupRelations(ctx, c, ch, seat, members); err != nil {
		return err
	}
	if err := m.allianceWeighWars(ctx, c, ch, seat, members); err != nil {
		return err
	}
	// Capital programs shared across the members (corpconsortium.go).
	if err := m.allianceAssignPrograms(ctx, c, ch, seat, members); err != nil {
		return err
	}
	if err := m.alliancePlanSov(ctx, c, ch, seat, members); err != nil {
		return err
	}
	// A tower in a target paid for by the members (alliancesovpledge.go).
	return m.allianceSovPledge(ctx, c, ch, seat, members)
}

// allianceAnswerApplications accepts or rejects every New application. The core's own 300 s sweep makes
// an acceptance effective after its waiting period; nothing here hurries it. An applicant that is a mutual
// ally of any member corporation, not only the executor, is sponsored (MutualAlly).
func (m *Mod) allianceAnswerApplications(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) error {
	apps, err := evedb.ListAllianceApplications(ctx, m.db, seat.AllianceID)
	if err != nil {
		return fmt.Errorf("alliance applications: %w", err)
	}
	allies := map[int64]bool{}
	for _, a := range relAlliesOf(c.CorpID) {
		allies[a] = true
	}
	for _, f := range members {
		for _, a := range relAlliesOf(f.CorpID) {
			allies[a] = true
		}
	}
	for _, app := range apps {
		if app.State != evedb.AllianceApplicationNew {
			continue
		}
		facts, err := getAnyCorpFacts(ctx, m.db, app.CorporationID)
		if err != nil {
			continue
		}
		a := allianceApplicantFacts{
			CorpID: app.CorporationID, Name: app.CorporationName,
			Pilots:        int64(facts.Members + facts.Bots),
			MutualAlly:    allies[app.CorporationID],
			OurStanding:   relCorpStanding(c.CorpID, app.CorporationID),
			TheirStanding: relCorpStanding(app.CorporationID, c.CorpID),
		}
		if _, ok := managedCEO(app.CorporationID); !ok {
			a.Human = true
		}
		if atWar, err := evedb.CorpAtWarWithAlliance(ctx, m.db, app.CorporationID, seat.AllianceID); err == nil {
			a.AtWar = atWar
		}
		accept, score, why := allianceApplicantScore(a, ch)
		state, verb := int64(evedb.AllianceApplicationRejected), "reject"
		if accept {
			state, verb = evedb.AllianceApplicationAccepted, "accept"
		}
		if err := evedb.UpdateAllianceApplication(ctx, m.db, seat.AllianceID, app.CorporationID, app.ApplicationText, state); err != nil {
			m.logBot(c.CharID, logError, "ceo", "alliance application %d: %v", app.CorporationID, err)
			continue
		}
		m.allianceChanged("app", seat.AllianceID, app.CorporationID)
		m.ceoLog(ctx, c, powerAlliance, verb, app.CorporationID, score, "%sed %s's application — %s", verb, app.CorporationName, why)
		m.allianceBook(ctx, seat.AllianceID, c.CorpID, verb, app.CorporationID, score, "%s %sed %s — %s",
			ownerNameOf(ctx, m.db, c.CorpID), verb, app.CorporationName, why)
		m.mailApplicantCEO(ctx, c, seat, app, accept, why)
	}
	return nil
}

// mailApplicantCEO tells the applying corporation's CEO the answer.
func (m *Mod) mailApplicantCEO(ctx context.Context, c *corpCEO, seat *allianceSeat, app evedb.AllianceApplication, accept bool, why string) {
	var ceoID int64
	if m.db.QueryRowContext(ctx, `SELECT ceoID FROM corporation WHERE corporationID = ?`, app.CorporationID).Scan(&ceoID) != nil || ceoID == 0 {
		return
	}
	subject := fmt.Sprintf("%s: application refused", seat.Name)
	body := fmt.Sprintf("The executor of %s [%s] has declined %s's application. %s.",
		seat.Name, seat.Ticker, app.CorporationName, strings.ToUpper(why[:1])+why[1:])
	if accept {
		subject = fmt.Sprintf("%s: application accepted", seat.Name)
		body = fmt.Sprintf("The executor of %s [%s] has accepted %s's application. CONCORD registers the membership after its waiting period; your corporation will then fly under the alliance's banner.",
			seat.Name, seat.Ticker, app.CorporationName)
	}
	m.mailPilot(ctx, ceoID, c.CharID, subject, body)
}

// allianceKicks shows a member the door when allianceKickVerdict says so.
func (m *Mod) allianceKicks(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) error {
	for _, f := range members {
		if f.CorpID == c.CorpID {
			continue
		}
		kick, why := allianceKickVerdict(f, ch)
		if !kick {
			continue
		}
		left, _, err := m.allianceRemoveCorp(ctx, seat.AllianceID, f.CorpID)
		if err != nil {
			m.logBot(c.CharID, logError, "ceo", "alliance kick %d: %v", f.CorpID, err)
			continue
		}
		if left == 0 {
			continue
		}
		m.refreshRelations(ctx)
		m.ceoLog(ctx, c, powerAlliance, "kick", f.CorpID, 0, "expelled %s from %s — %s", f.Name, seat.Name, why)
		m.allianceBook(ctx, seat.AllianceID, c.CorpID, "kicked", f.CorpID, 0, "%s expelled %s — %s", ownerNameOf(ctx, m.db, c.CorpID), f.Name, why)
		if f.CEOID != 0 {
			m.mailPilot(ctx, f.CEOID, c.CharID, fmt.Sprintf("%s: membership ended", seat.Name),
				fmt.Sprintf("The executor of %s [%s] has ended %s's membership. %s.", seat.Name, seat.Ticker, f.Name, strings.ToUpper(why[:1])+why[1:]))
		}
	}
	return nil
}

// allianceWarsOf counts an alliance's running wars, either side.
func (m *Mod) allianceWarsOf(ctx context.Context, allianceID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM wars WHERE timeFinished IS NULL AND (declaredByID = ? OR againstID = ?)`,
		allianceID, allianceID).Scan(&n)
	return n
}

// allianceHeadcount is the pilots across an alliance, for the panel.
func allianceHeadcount(members []allianceMemberFacts) int64 {
	var n int64
	for _, f := range members {
		n += f.Pilots
	}
	return n
}

// allianceTitheTotal sums shares, for logs.
func allianceTitheTotal(shares map[int64]float64) float64 {
	var t float64
	for _, v := range shares {
		t += v
	}
	return math.Round(t)
}
