package rolebots

// Alliance wars: a member's grievance becomes the executor's declaration.
//
// The core refuses an allied corporation that declares as itself (warwrites.go: Declare War routes through
// the executor's directors), so an allied CEO's war decision files a rolebot_alliance_requests row of kind
// war with its motive and score (fileAllianceWarRequest). The executor weighs pending requests against
// its chest and the alliance's war count (allianceWarVerdict, pure) and declares with the alliance as owner
// and its own corporation paying the fee. CONCORD bills the executor for the fee and the weekly bill; each
// sum is shared out by titheShares into ledger rows (alliancetithe.go). The war sits in rolebot_ceo_wars with
// ownerID = the alliance, so settlement, retraction and bills use the war power's paths.

import (
	"context"
	"encoding/json"
	"fmt"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

const (
	allianceReqWar    = "war"
	allianceReqDefend = "defend"
	allianceReqMuster = "muster"
	allianceReqRel    = "rel"
	allianceReqSov    = "sov"

	allianceReqPending = "pending"
	allianceReqTaken   = "taken"
	allianceReqRefused = "refused"
	allianceReqExpired = "expired"

	// allianceWarRequestTTLSec is how long a war request waits for the executor before it lapses: two
	// weeks, longer than any think cadence.
	allianceWarRequestTTLSec = 14 * 86400
)

// allianceWarPayload is what a war request carries.
type allianceWarPayload struct {
	Motive string  `json:"motive"`
	Score  float64 `json:"score"`
	Why    string  `json:"why"`
}

// allianceRequest is one rolebot_alliance_requests row.
type allianceRequest struct {
	ID, AllianceID       int64
	FromCorp, PosterID   int64
	Kind                 string
	TargetID, SystemID   int64
	Payload, Note        string
	PostedAt, ExpiresAt  int64
	Status, Result       string
}

// fileAllianceWarRequest is the allied CEO's war decision: one open request
// per target. Logged on the corporation's own ledger as the war power's
// "request" so the Corporate Portal and the CEO page show it.
func (m *Mod) fileAllianceWarRequest(ctx context.Context, c *corpCEO, allianceID int64, x *warCand, why string) error {
	var open int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_alliance_requests
		 WHERE allianceID = ? AND fromCorpID = ? AND kind = ? AND targetID = ? AND status = 'pending'`,
		allianceID, c.CorpID, allianceReqWar, x.Owner).Scan(&open)
	if open > 0 {
		return nil
	}
	payload, _ := json.Marshal(allianceWarPayload{Motive: x.Kind, Score: x.Motive, Why: why})
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_alliance_requests
		  (allianceID, fromCorpID, posterID, kind, targetID, systemID, payload, note, postedAt, expiresAt, status)
		VALUES (?, ?, 0, ?, ?, 0, ?, ?, ?, ?, 'pending')`,
		allianceID, c.CorpID, allianceReqWar, x.Owner, string(payload), truncate(why, 255), now, now+allianceWarRequestTTLSec); err != nil {
		return fmt.Errorf("fileAllianceWarRequest: %w", err)
	}
	m.ceoLog(ctx, c, powerWar, "request", x.Owner, x.Motive, "asked the alliance for war on %s (%s) — %s",
		ownerNameOf(ctx, m.db, x.Owner), x.Kind, why)
	m.allianceBook(ctx, allianceID, c.CorpID, "war-request", x.Owner, x.Motive, "%s asks for war on %s — %s",
		ownerNameOf(ctx, m.db, c.CorpID), ownerNameOf(ctx, m.db, x.Owner), why)
	return nil
}

// allianceRequestsPending lists an alliance's open requests of one kind.
func (m *Mod) allianceRequestsPending(ctx context.Context, allianceID int64, kind string) ([]allianceRequest, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT requestID, allianceID, fromCorpID, posterID, kind, targetID, systemID,
		       COALESCE(payload, ''), note, postedAt, expiresAt, status, result
		  FROM rolebot_alliance_requests
		 WHERE allianceID = ? AND kind = ? AND status = 'pending'
		 ORDER BY postedAt`, allianceID, kind)
	if err != nil {
		return nil, fmt.Errorf("allianceRequestsPending: %w", err)
	}
	defer rows.Close()
	var out []allianceRequest
	for rows.Next() {
		var r allianceRequest
		if err := rows.Scan(&r.ID, &r.AllianceID, &r.FromCorp, &r.PosterID, &r.Kind, &r.TargetID, &r.SystemID,
			&r.Payload, &r.Note, &r.PostedAt, &r.ExpiresAt, &r.Status, &r.Result); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// allianceRequestClose settles a request.
func (m *Mod) allianceRequestClose(ctx context.Context, id int64, status, result string) {
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_alliance_requests SET status = ?, result = ? WHERE requestID = ?`,
		status, truncate(result, 255), id)
}

// allianceExpireRequests lapses requests past their time.
func (m *Mod) allianceExpireRequests(ctx context.Context, allianceID int64) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_alliance_requests SET status = 'expired', result = 'no decision in time'
		 WHERE allianceID = ? AND status = 'pending' AND expiresAt < ?`, allianceID, clock.Unix())
}

// ── the executor's verdict ──────────────────────────────────────────────────

// allianceWarRequest is one target's case, as the verdict reads it: every
// member's request on it folded together.
type allianceWarRequest struct {
	Target     int64
	Score      float64 // the strongest single request
	Backers    int     // members asking
	Pilots     int64   // their headcount
	Kind       string
	Why        string
	ExecMotive float64 // the executor's own motive against the target (0 = none)
}

// allianceWarVerdictIn is the executor's position.
type allianceWarVerdictIn struct {
	Requests     []allianceWarRequest
	Chest        float64 // what the executor may commit (warChest)
	Fee, Runway  float64 // the declaration fee and the weeks of bills set aside
	Running      int     // alliance wars already declared
	MaxWars      int     // the executor's charter cap
	CoreMax      int     // CONCORD's cap on declarations per owner
	MemberCorps  int
}

// allianceWarVerdict picks at most one war to declare. A target's case is its strongest request, raised
// by the share of the alliance asking and by the executor's own motive. It must clear WarAt, the alliance
// must be under both caps, and fee plus runway must fit the executor's chest: it fronts the money and
// tithes it back. Pure.
func allianceWarVerdict(in allianceWarVerdictIn, ch Charter) (target int64, why string) {
	if in.Running >= in.MaxWars {
		return 0, fmt.Sprintf("the alliance already runs %d wars, the executor's limit", in.Running)
	}
	if in.CoreMax > 0 && in.Running >= in.CoreMax {
		return 0, fmt.Sprintf("CONCORD allows %d declarations and the alliance has %d", in.CoreMax, in.Running)
	}
	if in.Fee+in.Runway > in.Chest {
		return 0, fmt.Sprintf("the executor's chest (%s) cannot front a fee of %s and %s of bills",
			iskString(in.Chest), iskString(in.Fee), iskString(in.Runway))
	}
	best, bestScore := -1, 0.0
	for i, r := range in.Requests {
		s := r.Score
		if in.MemberCorps > 0 {
			s *= 1 + float64(r.Backers)/float64(in.MemberCorps)
		}
		s += 0.5 * r.ExecMotive
		if s > bestScore || (s == bestScore && best >= 0 && r.Target < in.Requests[best].Target) {
			best, bestScore = i, s
		}
	}
	if best < 0 {
		return 0, "no requests"
	}
	r := in.Requests[best]
	if bestScore < ch.WarAt {
		return 0, fmt.Sprintf("the case against %d scores %.1f, under the executor's %.1f", r.Target, bestScore, ch.WarAt)
	}
	return r.Target, fmt.Sprintf("%d of %d members ask (%s, %.1f); the executor's own motive %.1f; case %.1f ≥ %.1f",
		r.Backers, in.MemberCorps, r.Kind, r.Score, r.ExecMotive, bestScore, ch.WarAt)
}

// allianceWeighWars is the executor's pass over the pending war requests:
// lapse the old, fold the rest per target, take the verdict, declare.
func (m *Mod) allianceWeighWars(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat, members []allianceMemberFacts) error {
	if !ceoWarsOn() {
		return nil
	}
	if m.humanHoldOn(ctx, c.CorpID, "allwar") {
		return nil
	}
	m.allianceExpireRequests(ctx, seat.AllianceID)
	reqs, err := m.allianceRequestsPending(ctx, seat.AllianceID, allianceReqWar)
	if err != nil || len(reqs) == 0 {
		return err
	}
	// One step per dwell, like the war power's own declarations.
	if now := clock.Unix(); now-m.lastCEOActionAt(ctx, c.CorpID, "declare") < ch.DwellSec {
		return nil
	}
	pilots := map[int64]int64{}
	for _, f := range members {
		pilots[f.CorpID] = f.Pilots
	}
	busy := m.warBusyOwners(ctx, c.CorpID)
	byTarget := map[int64]*allianceWarRequest{}
	for _, r := range reqs {
		if busy[r.TargetID] {
			m.allianceRequestClose(ctx, r.ID, allianceReqRefused, "already at war with them")
			continue
		}
		var p allianceWarPayload
		_ = json.Unmarshal([]byte(r.Payload), &p)
		t := byTarget[r.TargetID]
		if t == nil {
			t = &allianceWarRequest{Target: r.TargetID}
			byTarget[r.TargetID] = t
		}
		t.Backers++
		t.Pilots += pilots[r.FromCorp]
		if p.Score > t.Score {
			t.Score, t.Kind, t.Why = p.Score, p.Motive, p.Why
		}
	}
	if len(byTarget) == 0 {
		return nil
	}
	cands, err := m.ceoWarCandidates(ctx, c, ch)
	if err != nil {
		return err
	}
	var list []allianceWarRequest
	for _, t := range byTarget {
		for _, x := range cands {
			if x.Owner == t.Target && x.Motive > t.ExecMotive {
				t.ExecMotive = x.Motive
			}
		}
		list = append(list, *t)
	}
	chest, _, err := m.warChest(ctx, c.CorpID, ch)
	if err != nil {
		return err
	}
	fee := evedb.WarDeclarationCost(ctx, m.db)
	runway := ch.WarRunwayWeeks * evedb.WarWeeklyBillCost(ctx, m.db)
	var running int64
	if n, err := evedb.CountActiveDeclaredWars(ctx, m.db, seat.AllianceID); err == nil {
		running = n
	}
	target, why := allianceWarVerdict(allianceWarVerdictIn{
		Requests: list, Chest: chest, Fee: fee, Runway: runway,
		Running: int(running), MaxWars: ch.MaxWars, CoreMax: int(evedb.WarMaxDeclarations(ctx, m.db)),
		MemberCorps: len(members),
	}, ch)
	if target == 0 {
		// Not now: the requests stand until their time runs out. Note the
		// reason once a day so the book says why nothing happens.
		if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "warhold") >= 86400 {
			m.ceoLog(ctx, c, powerAlliance, "warhold", 0, 0, "holds the alliance's war requests — %s", why)
		}
		return nil
	}
	return m.allianceDeclareWar(ctx, c, ch, seat, members, target, fee, runway, why)
}

// allianceDeclareWar declares in the alliance's name, books the war under
// the executor with the alliance as owner, and tithes the fee.
func (m *Mod) allianceDeclareWar(ctx context.Context, c *corpCEO, ch Charter, seat *allianceSeat,
	members []allianceMemberFacts, target int64, fee, runway float64, why string) error {
	hook := modapi.Corp().DeclareWar
	if hook == nil {
		return nil
	}
	warID, err := hook(seat.AllianceID, c.CorpID, target)
	if err != nil {
		m.logBot(c.CharID, logInfo, "ceo", "alliance war on %s refused: %v", ownerNameOf(ctx, m.db, target), err)
		return nil
	}
	kind := "grievance"
	for _, r := range mustRequests(m, ctx, seat.AllianceID, target) {
		var p allianceWarPayload
		if json.Unmarshal([]byte(r.Payload), &p) == nil && p.Motive != "" {
			kind = p.Motive
		}
		m.allianceRequestClose(ctx, r.ID, allianceReqTaken, fmt.Sprintf("war %d declared", warID))
	}
	if err := m.ceoWarBookAdd(ctx, warID, c.CorpID, target, kind, 0, why); err != nil {
		return err
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_ceo_wars SET ownerID = ? WHERE warID = ?`, seat.AllianceID, warID)
	if err := m.openCommit(ctx, c.CorpID, "war", warID, runway); err != nil {
		return err
	}
	m.corpBook(ctx, c.CorpID, lineAllianceWar, -fee, evedb.OwnerCONCORDBills, "alliance war fee on "+ownerNameOf(ctx, m.db, target))
	m.allianceTithe(ctx, c, seat, members, fee, warID, "war fee on "+ownerNameOf(ctx, m.db, target))
	m.refreshRelations(ctx)
	m.ceoLog(ctx, c, powerAlliance, "declare", target, fee, "declared war in %s's name on %s — %s; %s set aside for %.0f weeks of bills",
		seat.Name, ownerNameOf(ctx, m.db, target), why, iskString(runway), ch.WarRunwayWeeks)
	m.allianceBook(ctx, seat.AllianceID, c.CorpID, "declare", target, fee, "%s declared war on %s — %s",
		seat.Name, ownerNameOf(ctx, m.db, target), why)
	m.allianceBulletin(ctx, seat.AllianceID, c.CharID, fmt.Sprintf("%s: war declared on %s", seat.Name, ownerNameOf(ctx, m.db, target)),
		fmt.Sprintf("The executor has declared war on %s in the alliance's name. CONCORD opens hostilities in 24 hours. %s. Each member corporation will be asked for its share of the fee and the weekly bills.",
			ownerNameOf(ctx, m.db, target), why))
	return nil
}

// mustRequests is the pending war requests on one target.
func mustRequests(m *Mod, ctx context.Context, allianceID, target int64) []allianceRequest {
	reqs, _ := m.allianceRequestsPending(ctx, allianceID, allianceReqWar)
	var out []allianceRequest
	for _, r := range reqs {
		if r.TargetID == target {
			out = append(out, r)
		}
	}
	return out
}

// allianceTithe shares a sum the executor fronted into ledger rows.
func (m *Mod) allianceTithe(ctx context.Context, c *corpCEO, seat *allianceSeat, members []allianceMemberFacts, total float64, refID int64, reason string) {
	shares := titheShares(members, c.CorpID, total)
	if err := m.titheIssue(ctx, seat.AllianceID, c.CorpID, shares, titheKindWar, refID, reason); err != nil {
		rlogf("rolebots: alliance tithe: %v", err)
		return
	}
	owed := allianceTitheTotal(shares) - shares[c.CorpID]
	if owed > 0 {
		m.allianceBook(ctx, seat.AllianceID, c.CorpID, "tithed", refID, owed, "%s billed the members %s between them — %s",
			seat.Name, iskString(owed), reason)
	}
}

// allianceWarBillPaid is called by the war power when the executor pays a
// weekly bill on an alliance war: the bill is shared out like the fee.
func (m *Mod) allianceWarBillPaid(ctx context.Context, c *corpCEO, warID int64, amount float64) {
	var ownerID, target int64
	if m.db.QueryRowContext(ctx, `SELECT ownerID, targetOwnerID FROM rolebot_ceo_wars WHERE warID = ?`, warID).Scan(&ownerID, &target) != nil || ownerID == 0 {
		return
	}
	seat, err := m.allianceOf(ctx, c.CorpID)
	if err != nil || seat == nil || seat.AllianceID != ownerID || !seat.IsExecutor {
		return
	}
	members, err := m.allianceMemberFactsOf(ctx, seat.AllianceID, c.CorpID)
	if err != nil {
		return
	}
	m.corpBook(ctx, c.CorpID, lineAllianceWar, -amount, evedb.OwnerCONCORDBills, "alliance war bill on "+ownerNameOf(ctx, m.db, target))
	m.allianceTithe(ctx, c, seat, members, amount, warID, "weekly bill, war on "+ownerNameOf(ctx, m.db, target))
}
