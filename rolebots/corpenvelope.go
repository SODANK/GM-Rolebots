package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
)

// Budget envelopes: Finance gives each arm its share of the week's money. Each
// week the Treasurer (the CEO when none is appointed) splits free capital
// (wallet less reserve less every open promise, corpcommit.go) between the
// justified arms into envelopes, and a request an arm makes is charged to its own
// envelope (corprequest.go: the asking arm pays, not the one doing the work). What
// an arm has used is read off the promises themselves (every requisition and
// cash-paid work order is a rolebot_corp_commit row naming the arm that asked), so
// the envelope cannot drift from the books. Infrastructure (a tower that misses a
// cycle goes dark) and Industry (raw material) are never capped but still
// budgeted and counted; every other arm over its envelope waits for next week.

const (
	envelopePeriodSec = 7 * 86400
	// envelopeFloorShare is the slice of the week every justified arm gets
	// whatever it spent last week; the rest follows last week's use and what
	// is already on order.
	envelopeFloorShare = 0.10
)

func ensureEnvelopeTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rolebot_corp_envelope (
			corporationID BIGINT NOT NULL,
			arm           VARCHAR(16) NOT NULL,
			periodStart   BIGINT NOT NULL DEFAULT 0,
			allotted      DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, arm)
		)`)
	if err != nil {
		return fmt.Errorf("ensureEnvelopeTables: %w", err)
	}
	return nil
}

// envelopeCapped is whether an arm's requests stop at its envelope. Pure.
func envelopeCapped(a corpArm) bool {
	return a != armInfrastructure && a != armIndustry
}

// envelopeSplit divides free capital between arms: a floor slice each, the
// rest by weight (last week's use plus what is on order now). Every arm gets
// something; with no weights at all the split is even. Pure.
func envelopeSplit(free float64, arms []corpArm, weight map[corpArm]float64) map[corpArm]float64 {
	out := map[corpArm]float64{}
	if len(arms) == 0 || free <= 0 {
		for _, a := range arms {
			out[a] = 0
		}
		return out
	}
	floor := free * envelopeFloorShare / float64(len(arms))
	rest := free * (1 - envelopeFloorShare)
	var total float64
	for _, a := range arms {
		total += math.Max(weight[a], 0)
	}
	for _, a := range arms {
		share := 1 / float64(len(arms))
		if total > 0 {
			share = math.Max(weight[a], 0) / total
		}
		out[a] = math.Floor(floor + rest*share)
	}
	return out
}

// armUse is what each arm has promised or paid since a moment: the larger of
// promised and spent on every requisition and cash-paid work order opened
// since then, by the arm that asked.
func (m *Mod) armUse(ctx context.Context, corpID, since int64) map[corpArm]float64 {
	out := map[corpArm]float64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.requesterArm, r.requester, SUM(GREATEST(k.amount, k.spent))
		  FROM rolebot_corp_commit k
		  JOIN rolebot_corp_requisition r ON r.reqID = k.refID
		 WHERE k.corporationID = ? AND k.refKind = 'req' AND k.openedAt >= ?
		 GROUP BY r.requesterArm, r.requester`, corpID, since)
	if err == nil {
		for rows.Next() {
			var arm, requester string
			var v float64
			if rows.Scan(&arm, &requester, &v) == nil {
				out[reqRequesterArm(requisition{RequesterArm: arm, Requester: requester})] += v
			}
		}
		rows.Close()
	}
	rows, err = m.db.QueryContext(ctx, `
		SELECT w.requesterArm, w.purpose, w.kind, SUM(GREATEST(k.amount, k.spent))
		  FROM rolebot_corp_commit k
		  JOIN rolebot_corp_work w ON w.workID = k.refID
		 WHERE k.corporationID = ? AND k.refKind = 'work' AND k.openedAt >= ?
		 GROUP BY w.requesterArm, w.purpose, w.kind`, corpID, since)
	if err == nil {
		for rows.Next() {
			var arm, purpose, kind string
			var v float64
			if rows.Scan(&arm, &purpose, &kind, &v) == nil {
				out[workRequesterArm(corpWork{RequesterArm: arm, Purpose: purpose, Kind: kind})] += v
			}
		}
		rows.Close()
	}
	return out
}

// corpEnvelope is one arm's envelope this period.
type corpEnvelope struct {
	Arm         corpArm
	PeriodStart int64
	Allotted    float64
	Used        float64
}

// Room is what is left of it; never negative.
func (e corpEnvelope) Room() float64 { return math.Max(0, e.Allotted-e.Used) }

// corpEnvelopes reads a corporation's envelopes with what each arm has used,
// in arm order. Empty when Finance has not split a week yet.
func (m *Mod) corpEnvelopes(ctx context.Context, corpID int64) []corpEnvelope {
	rows, err := m.db.QueryContext(ctx, `
		SELECT arm, periodStart, allotted FROM rolebot_corp_envelope WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil
	}
	var out []corpEnvelope
	var since int64
	for rows.Next() {
		var e corpEnvelope
		var arm string
		if rows.Scan(&arm, &e.PeriodStart, &e.Allotted) == nil {
			e.Arm = corpArm(arm)
			out = append(out, e)
			since = e.PeriodStart
		}
	}
	rows.Close()
	if len(out) == 0 {
		return nil
	}
	use := m.armUse(ctx, corpID, since)
	rank := map[corpArm]int{}
	for i, a := range armOrder {
		rank[a] = i
	}
	for i := range out {
		out[i].Used = use[out[i].Arm]
	}
	sort.Slice(out, func(i, j int) bool { return rank[out[i].Arm] < rank[out[j].Arm] })
	return out
}

// refreshEnvelopes splits a new week when the last one has run out (or none
// was ever split). Runs from the treasury pass; the justified arms come from
// the arm pass's memo, so a corporation with arms off has no envelopes and
// nothing is capped.
func (m *Mod) refreshEnvelopes(ctx context.Context, c *corpCEO, ch Charter) {
	memo, ok := armMemoOf(c.CorpID)
	if !ok || len(memo.Justified) == 0 {
		return
	}
	now := clock.Unix()
	var start sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MAX(periodStart) FROM rolebot_corp_envelope WHERE corporationID = ?`, c.CorpID).Scan(&start)
	if start.Valid && now-start.Int64 < envelopePeriodSec {
		return
	}
	free, _, err := m.corpFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return
	}
	// The week's money is what is free now plus what the arms already have on
	// order: an open promise was this week's money before it was promised.
	last := now - envelopePeriodSec
	if start.Valid {
		last = start.Int64
	}
	used := m.armUse(ctx, c.CorpID, last)
	weight := map[corpArm]float64{}
	var arms []corpArm
	var onOrder float64
	for _, a := range memo.Justified {
		if a == armHR || a == armFinance {
			continue // desk work, paid by the payroll; nothing to buy
		}
		arms = append(arms, a)
		weight[a] = used[a]
		onOrder += used[a]
	}
	split := envelopeSplit(free+onOrder, arms, weight)
	var parts []string
	for _, a := range arms {
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_envelope (corporationID, arm, periodStart, allotted) VALUES (?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE periodStart = VALUES(periodStart), allotted = VALUES(allotted)`,
			c.CorpID, string(a), now, split[a]); err != nil {
			return
		}
		parts = append(parts, fmt.Sprintf("%s %s", armTag(a), iskString(split[a])))
	}
	// An arm no longer justified keeps no envelope.
	_, _ = m.db.ExecContext(ctx,
		`DELETE FROM rolebot_corp_envelope WHERE corporationID = ? AND periodStart <> ?`, c.CorpID, now)
	m.ceoLog(ctx, c, powerBusiness, "envelopes", 0, free+onOrder,
		"split the week's %s between the arms: %s", iskString(free+onOrder), joinWords(parts))
}

// envelopeRefusal is why an arm may not promise amount more this week, ""
// when it may. Uncapped arms, and a corporation with no envelopes, always may.
func (m *Mod) envelopeRefusal(ctx context.Context, corpID int64, arm corpArm, amount float64) string {
	if !envelopeCapped(arm) || amount <= 0 {
		return ""
	}
	for _, e := range m.corpEnvelopes(ctx, corpID) {
		if e.Arm != arm {
			continue
		}
		if amount > e.Room() {
			return fmt.Sprintf("%s has %s of its %s left this week", armLabel(arm), iskString(e.Room()),
				iskString(e.Allotted))
		}
		return ""
	}
	return ""
}
