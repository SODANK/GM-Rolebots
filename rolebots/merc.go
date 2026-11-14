package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The mercenary market: corporations hiring other corporations' guns.
//
// The core's wars are two-party, so a hired merc does not join a war. It declares
// its own through modapi.CorpHooks.DeclareWar and its client pays it. Offense: the
// client wants a corporation fought and appears nowhere on the wire. Defense: the
// client was declared on, and the merc counter-declares on the aggressor. Every
// war reader (corprel.go) then sends the merc's pilots after the target.
//
// A contract runs tender, quote, shortlist, haggle, award, weekly settle, end.
// See docs/comment-notes-b13.md#mercenary-contract-life.

const (
	mercKindOffense = "offense"
	mercKindDefense = "defense"

	// mercMinPilots is the smallest crew a merc will put on a contract.
	mercMinPilots = 3
	// mercOverMatch is how much a merc wants to outweigh its target.
	mercOverMatch = 1.2
	// mercRounds is how many haggling rounds a job gets.
	mercRounds = 4
	// mercShortlist is how many quotes a client haggles over.
	mercShortlist = 2
	// mercQuoteWindowSec is how long a job waits for quotes; mercMinQuoteSec
	// how long it waits at least, even with a full shortlist in hand.
	mercQuoteWindowSec = 6 * 3600
	mercMinQuoteSec    = 3600
	// mercBonusCapFrac is the weekly bonus cap as a share of the weekly fee,
	// and mercBonusHeadroom what a client budgets per week against its
	// walk-away to cover it.
	mercBonusCapFrac  = 0.5
	mercBonusHeadroom = 1 + mercBonusCapFrac
	// mercStaleSettleSec: a merc whose client has not settled in this long ends the
	// contract itself, since a client whose CEO left never pays.
	mercStaleSettleSec = warWeekSec + 2*86400
)

// ── the pure half ───────────────────────────────────────────────────────────

// mercQuoteIn is everything a merc prices a job from.
type mercQuoteIn struct {
	Pilots      int
	Wage        float64 // per pilot per week
	WeeklyBill  float64 // CONCORD's continuation bill
	Odds        float64 // target strength ÷ what the merc commits
	StandClient float64 // the merc's standing toward the client
	StandTarget float64 // ... and toward the target
	Load        float64 // live contracts ÷ capacity
	Reputation  float64 // 0.8–1.3 (mercReputation)
	// ClientWeekly is the client's wallet spread over the contract's weeks, what a
	// greedy merc prices toward.
	ClientWeekly float64
}

// mercQuote is a merc's answer: its opening weekly ask and its private
// walk-away, or a refusal and why.
type mercQuote struct {
	Ask, Walk float64
	OK        bool
	Why       string
}

// mercPolitics is the multiplier the merc's opinions put on its labour, or
// false when it will not take the work at all.
func mercPolitics(standClient, standTarget float64) (float64, string, bool) {
	switch {
	case standClient <= ceoRelRival:
		return 0, "we do not work for them", false
	case standTarget >= ceoRelAlly:
		return 0, "they are our allies", false
	}
	pol, why := 1.0, ""
	switch {
	case standClient >= ceoRelAlly:
		pol, why = pol*0.8, "a price for an ally"
	case standClient >= ceoRelFriend:
		pol, why = pol*0.9, "a price for a friend"
	}
	switch {
	case standTarget >= ceoRelFriend:
		pol, why = pol*1.5, "they are friends of ours — it will cost"
	case standTarget <= ceoRelEnemy:
		pol, why = pol*0.75, "we would fight them for less"
	case standTarget <= ceoRelRival:
		pol, why = pol*0.85, "no love lost with them"
	}
	return pol, why, true
}

// mercQuoteFor prices one job. Pure.
// labour = pilots x wage x politics
// ask = (labour x risk x market + bill) x (1 + margin), pulled toward the
// client's wallet by greed. walk = (labour + bill) x (1 + margin), under the ask.
func mercQuoteFor(in mercQuoteIn, ch Charter) mercQuote {
	pol, why, ok := mercPolitics(in.StandClient, in.StandTarget)
	if !ok {
		return mercQuote{Why: why}
	}
	if in.Pilots < mercMinPilots {
		return mercQuote{Why: "no free pilots"}
	}
	if in.Odds > ch.MercMaxOdds {
		return mercQuote{Why: fmt.Sprintf("outgunned %.1f to 1", in.Odds)}
	}
	labour := float64(in.Pilots) * in.Wage * pol
	risk := 1 + 0.6*in.Odds
	rep := in.Reputation
	if rep <= 0 {
		rep = 1
	}
	market := (1 + 0.5*in.Load*in.Load) * rep
	margin := 1 + ch.MercMargin
	ask := (labour*risk*market + in.WeeklyBill) * margin
	walk := (labour + in.WeeklyBill) * margin
	if in.ClientWeekly > ask && ch.MercGreedPull > 0 {
		ask += (in.ClientWeekly - ask) * ch.MercGreedPull
		walk += (in.ClientWeekly - walk) * ch.MercGreedPull * 0.5
	}
	walk = math.Min(walk, ask)
	if why == "" {
		why = fmt.Sprintf("%d pilots against %.1f-to-1 odds", in.Pilots, in.Odds)
	}
	return mercQuote{Ask: ask, Walk: walk, OK: true, Why: why}
}

// mercPilotsNeeded is the crew a merc puts on a target: enough to outweigh it
// by mercOverMatch, at least mercMinPilots, at most what it can spare.
func mercPilotsNeeded(theirs, effSelf float64, spare int) int {
	if effSelf < 0 {
		effSelf = 0.5
	}
	per := warArmedFrac * (0.5 + effSelf)
	n := int(math.Ceil(theirs * mercOverMatch / per))
	if n < mercMinPilots {
		n = mercMinPilots
	}
	if n > spare {
		n = spare
	}
	return n
}

// mercReputation is a merc's record as a multiplier on its price: from 0.8
// (nothing delivered) through 1.0 (no record) to 1.3 (every contract finished
// and more destroyed than it was paid).
func mercReputation(ended, completed int, destroyed, paid float64) float64 {
	if ended <= 0 {
		return 1
	}
	f := float64(completed) / float64(ended)
	kpi := 0.0
	if paid > 0 {
		kpi = math.Min(destroyed/paid, 1)
	}
	return math.Max(0.8, math.Min(0.8+0.3*f+0.2*kpi, 1.3))
}

// concedeFrac is how far along its concession a side is at a round: 0 at the
// opening, 1 at the deadline. exp above 1 holds out (Boulware), below 1 gives
// ground early.
func concedeFrac(round, rounds int, exp float64) float64 {
	if rounds <= 0 {
		return 1
	}
	if exp <= 0 {
		exp = 1
	}
	f := math.Max(0, math.Min(float64(round)/float64(rounds), 1))
	return math.Pow(f, exp)
}

// askAt / offerAt are the two sides' positions at a round.
func askAt(ask0, walk float64, round, rounds int, exp float64) float64 {
	return ask0 - (ask0-walk)*concedeFrac(round, rounds, exp)
}

func offerAt(open, walk float64, round, rounds int, exp float64) float64 {
	return open + (walk-open)*concedeFrac(round, rounds, exp)
}

// haggleDeal reports whether an offer meets an ask, and the price: the
// midpoint of the overlap.
func haggleDeal(ask, offer float64) (float64, bool) {
	if offer < ask {
		return 0, false
	}
	return (ask + offer) / 2, true
}

// mercBudgetTerms is what a client can agree to under its budget: the weekly
// price as agreed, the bonus cap cut down if the budget will not carry the
// full one.
func mercBudgetTerms(budget, fee, weekly float64, weeks int) (bonusCap float64, ok bool) {
	if weeks <= 0 {
		return 0, false
	}
	base := fee + weekly*float64(weeks)
	if base > budget {
		return 0, false
	}
	return math.Min(weekly*mercBonusCapFrac, (budget-base)/float64(weeks)), true
}

// ── the rows ────────────────────────────────────────────────────────────────

type mercJob struct {
	JobID, Client        int64
	Kind                 string
	TargetOwner, Target  int64
	WarID                int64
	Motive               string
	Weeks                int
	Budget, Value, Open  float64
	Round                int
	Status               string
	PostedAt, QuoteUntil int64
}

type mercBid struct {
	JobID, Merc                     int64
	Ask, Walk, Exp, BonusRate       float64
	Pilots                          int
	Credibility, LastAsk, LastOffer float64
	Status, Why                     string
}

type mercContract struct {
	ID, JobID, Client, Merc, TargetOwner, WarID int64
	Kind                                        string
	Retainer, Weekly, BonusRate, BonusCap       float64
	Weeks                                       int
	StartAt, EndAt, LastSettledAt, LastFielded  int64
	PaidTotal, BonusPaid, Destroyed             float64
	Status                                      string
}

const mercJobCols = `jobID, clientCorpID, kind, targetOwnerID, targetCorpID, warID, motive, weeks,
	budget, valueWeekly, openWeekly, round, status, postedAt, quoteUntil`

func scanMercJob(sc interface{ Scan(...any) error }) (mercJob, error) {
	var j mercJob
	err := sc.Scan(&j.JobID, &j.Client, &j.Kind, &j.TargetOwner, &j.Target, &j.WarID, &j.Motive, &j.Weeks,
		&j.Budget, &j.Value, &j.Open, &j.Round, &j.Status, &j.PostedAt, &j.QuoteUntil)
	return j, err
}

const mercContractCols = `contractID, jobID, clientCorpID, mercCorpID, targetOwnerID, warID, kind,
	retainer, weekly, bonusRate, bonusWeeklyCap, weeks, startAt, endAt, lastSettledAt, lastFieldedAt,
	paidTotal, bonusPaid, iskDestroyed, status`

func scanMercContract(sc interface{ Scan(...any) error }) (mercContract, error) {
	var k mercContract
	err := sc.Scan(&k.ID, &k.JobID, &k.Client, &k.Merc, &k.TargetOwner, &k.WarID, &k.Kind,
		&k.Retainer, &k.Weekly, &k.BonusRate, &k.BonusCap, &k.Weeks, &k.StartAt, &k.EndAt,
		&k.LastSettledAt, &k.LastFielded, &k.PaidTotal, &k.BonusPaid, &k.Destroyed, &k.Status)
	return k, err
}

func (m *Mod) mercJobsWhere(ctx context.Context, where string, args ...any) ([]mercJob, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+mercJobCols+` FROM rolebot_merc_jobs WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("merc jobs: %w", err)
	}
	defer rows.Close()
	var out []mercJob
	for rows.Next() {
		j, err := scanMercJob(rows)
		if err != nil {
			return nil, fmt.Errorf("merc jobs scan: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (m *Mod) mercContractsWhere(ctx context.Context, where string, args ...any) ([]mercContract, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+mercContractCols+` FROM rolebot_merc_contracts WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("merc contracts: %w", err)
	}
	defer rows.Close()
	var out []mercContract
	for rows.Next() {
		k, err := scanMercContract(rows)
		if err != nil {
			return nil, fmt.Errorf("merc contracts scan: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (m *Mod) mercBidsFor(ctx context.Context, jobID int64, statuses ...string) ([]mercBid, error) {
	q := `SELECT jobID, mercCorpID, askWeekly, walkWeekly, concedeExp, bonusRate, pilots,
		credibility, lastAsk, lastOffer, status, why FROM rolebot_merc_bids WHERE jobID = ?`
	args := []any{jobID}
	if len(statuses) > 0 {
		q += ` AND status IN (` + placeholders(len(statuses)) + `)`
		for _, s := range statuses {
			args = append(args, s)
		}
	}
	rows, err := m.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("merc bids: %w", err)
	}
	defer rows.Close()
	var out []mercBid
	for rows.Next() {
		var b mercBid
		if err := rows.Scan(&b.JobID, &b.Merc, &b.Ask, &b.Walk, &b.Exp, &b.BonusRate, &b.Pilots,
			&b.Credibility, &b.LastAsk, &b.LastOffer, &b.Status, &b.Why); err != nil {
			return nil, fmt.Errorf("merc bids scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (m *Mod) setJobStatus(ctx context.Context, jobID int64, status, reason string) {
	closed := int64(0)
	switch status {
	case "open", "haggling":
	default:
		closed = clock.Unix()
		m.closeCommit(ctx, "mercjob", jobID)
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_merc_jobs SET status = ?, closedAt = ?, reason = ? WHERE jobID = ?`,
		status, closed, truncate(reason, 255), jobID)
}

func (m *Mod) setBidStatus(ctx context.Context, jobID, merc int64, status string) {
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_merc_bids SET status = ? WHERE jobID = ? AND mercCorpID = ?`, status, jobID, merc)
}

// mercContractLive reports whether a contract is still running.
func (m *Mod) mercContractLive(ctx context.Context, contractID int64) bool {
	var status string
	if err := m.db.QueryRowContext(ctx,
		`SELECT status FROM rolebot_merc_contracts WHERE contractID = ?`, contractID).Scan(&status); err != nil {
		return false
	}
	return status == "active"
}

// mercOpenAsClient counts a client's jobs in flight and contracts running: each
// is a war it is paying for.
func (m *Mod) mercOpenAsClient(ctx context.Context, corpID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM rolebot_merc_jobs WHERE clientCorpID = ? AND status IN ('open', 'haggling'))
		     + (SELECT COUNT(*) FROM rolebot_merc_contracts WHERE clientCorpID = ? AND status = 'active')`,
		corpID, corpID).Scan(&n)
	return n
}

// mercActiveAsMerc counts the contracts a corporation is working.
func (m *Mod) mercActiveAsMerc(ctx context.Context, corpID int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_merc_contracts WHERE mercCorpID = ? AND status = 'active'`, corpID).Scan(&n)
	return n
}

// ── the tick ────────────────────────────────────────────────────────────────

// mercTick is the market's share of one CEO's think: its jobs and contracts as a
// client, then, for a mercenary outfit, its quotes and contracts as a merc.
func (m *Mod) mercTick(ctx context.Context, c *corpCEO, ch Charter) error {
	if err := m.mercClientJobs(ctx, c, ch); err != nil {
		return err
	}
	if err := m.mercClientContracts(ctx, c, ch); err != nil {
		return err
	}
	if err := m.mercOrphanedContracts(ctx, c); err != nil {
		return err
	}
	if ch.Mercenary && (mercMarketOn() || gankHiresOn()) && relationsEnabled() {
		return m.mercQuoteJobs(ctx, c, ch)
	}
	return nil
}

// mercJobSpec is what a client wants done.
type mercJobSpec struct {
	Kind        string
	TargetOwner int64
	TargetCorp  int64
	WarID       int64
	Motive      string
	MotiveScore float64
	Why         string
}

// mercPostJob puts a contract out to tender, only when the war chest can carry
// the cheapest contract anyone could quote for it.
func (m *Mod) mercPostJob(ctx context.Context, c *corpCEO, ch Charter, spec mercJobSpec) error {
	if !mercOpenFor(spec.Motive) {
		return nil
	}
	chest, _, err := m.warChest(ctx, c.CorpID, ch)
	if err != nil {
		return err
	}
	fee := evedb.WarDeclarationCost(ctx, m.db)
	bill := evedb.WarWeeklyBillCost(ctx, m.db)
	weeks := int(math.Round(ch.WarPatienceWeeks))
	if weeks < 2 {
		weeks = 2
	}
	if weeks > 6 {
		weeks = 6
	}
	floor := fee + float64(weeks)*(bill+mercMinPilots*mercWagePerPilot())
	if chest < floor {
		return nil // it cannot afford even the cheapest merc: no tender
	}
	frac := math.Max(0.5, math.Min(spec.MotiveScore/(2*ch.WarAt), 1))
	value := (chest - fee) / (float64(weeks) * mercBonusHeadroom) * frac
	open := value * ch.OpenFrac
	now := clock.Unix()
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_merc_jobs
			(clientCorpID, kind, targetOwnerID, targetCorpID, warID, motive, weeks,
			 budget, valueWeekly, openWeekly, round, status, postedAt, quoteUntil, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 'open', ?, ?, ?)`,
		c.CorpID, spec.Kind, spec.TargetOwner, spec.TargetCorp, spec.WarID, spec.Motive, weeks,
		chest, value, open, now, now+mercQuoteWindowSec, truncate(spec.Why, 255))
	if err != nil {
		return fmt.Errorf("mercPostJob: %w", err)
	}
	jobID, _ := res.LastInsertId()
	if err := m.openCommit(ctx, c.CorpID, "mercjob", jobID, chest); err != nil {
		return err
	}
	verb := "war on"
	if spec.Kind == mercKindDefense {
		verb = "a counter-war against"
	}
	m.ceoLog(ctx, c, powerWar, "tender", spec.TargetOwner, chest,
		"put out a mercenary contract for %s %s (%d weeks, up to %s) — %s",
		verb, ownerNameOf(ctx, m.db, spec.TargetOwner), weeks, iskString(chest), spec.Why)
	return nil
}

// ── the merc's side ─────────────────────────────────────────────────────────

// mercQuoteJobs prices every open job this merc has not answered yet.
func (m *Mod) mercQuoteJobs(ctx context.Context, c *corpCEO, ch Charter) error {
	now := clock.Unix()
	// With the market shut, only gank tenders are on the board (gankrel.go).
	motives := `TRUE`
	if !mercMarketOn() {
		motives = `motive = '` + warMotiveGank + `'`
	}
	jobs, err := m.mercJobsWhere(ctx, `status = 'open' AND quoteUntil > ? AND clientCorpID <> ? AND `+motives+`
		AND NOT EXISTS (SELECT 1 FROM rolebot_merc_bids b
		                 WHERE b.jobID = rolebot_merc_jobs.jobID AND b.mercCorpID = ?)`,
		now, c.CorpID, c.CorpID)
	if err != nil || len(jobs) == 0 {
		return err
	}
	self, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	since := now - ceoRelWindowDays*86400
	minKill := m.minKillSince(ctx, since)
	effSelf := m.corpKillEfficiency(ctx, c.CorpID, minKill, since)
	active := m.mercActiveAsMerc(ctx, c.CorpID)
	capacity := self.Members / mercMinPilots
	if capacity < 1 {
		capacity = 1
	}
	busy := m.warBusyOwners(ctx, c.CorpID)
	rep := m.mercReputationOf(ctx, c.CorpID)
	bill := evedb.WarWeeklyBillCost(ctx, m.db)

	for _, j := range jobs {
		q := mercQuote{}
		pilots := 0
		cred := 0.0
		switch {
		case j.TargetOwner == c.CorpID || j.Target == c.CorpID ||
			(self.AllianceID != 0 && j.TargetOwner == self.AllianceID):
			q.Why = "the contract is on us"
		case self.AllianceID != 0:
			// An allied corporation cannot declare a war as itself (the
			// core refuses it), so it cannot take a contract that needs one.
			q.Why = "an allied corporation cannot declare on its own"
		case busy[j.TargetOwner] || busy[j.Target]:
			q.Why = "already at war with them"
		default:
			tf, err := getAnyCorpFacts(ctx, m.db, j.Target)
			if err != nil || tf == nil {
				q.Why = "no such target"
				break
			}
			if self.AllianceID != 0 && tf.AllianceID == self.AllianceID {
				q.Why = "they are our own alliance"
				break
			}
			theirs := warStrength(tf.Members, policyArmed(tf.Policy),
				m.corpKillEfficiency(ctx, tf.CorpID, minKill, since))
			spare := self.Members / (active + 1)
			pilots = mercPilotsNeeded(theirs, effSelf, spare)
			committed := warStrength(pilots, true, effSelf)
			odds := 99.0
			if committed > 0 {
				odds = theirs / committed
			}
			clientBal, _ := evedb.GetCorpBalance(ctx, m.db, j.Client)
			q = mercQuoteFor(mercQuoteIn{
				Pilots: pilots, Wage: mercWagePerPilot(), WeeklyBill: bill, Odds: odds,
				StandClient: relCorpStanding(c.CorpID, j.Client),
				StandTarget: relCorpStanding(c.CorpID, j.Target),
				Load:        float64(active) / float64(capacity),
				Reputation:  rep,
				ClientWeekly: func() float64 {
					if j.Weeks <= 0 {
						return 0
					}
					return clientBal / float64(j.Weeks)
				}(),
			}, ch)
			if odds > 0 {
				cred = math.Min(2, 1/odds) * rep
			}
		}
		status := "declined"
		if q.OK {
			status = "quoted"
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT IGNORE INTO rolebot_merc_bids
				(jobID, mercCorpID, askWeekly, walkWeekly, concedeExp, bonusRate, pilots,
				 credibility, lastAsk, status, quotedAt, why)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			j.JobID, c.CorpID, q.Ask, q.Walk, ch.ConcedeExp, ch.MercBonusRate, pilots,
			cred, q.Ask, status, now, truncate(q.Why, 255)); err != nil {
			return fmt.Errorf("mercQuoteJobs: %w", err)
		}
		if q.OK {
			m.ceoLog(ctx, c, powerWar, "quote", j.JobID, q.Ask,
				"quoted %s a week to %s for war on %s — %s",
				iskString(q.Ask), ownerNameOf(ctx, m.db, j.Client), ownerNameOf(ctx, m.db, j.TargetOwner), q.Why)
		} else {
			m.logBot(c.CharID, logInfo, "ceo", "war: passed on %s's contract against %s — %s",
				ownerNameOf(ctx, m.db, j.Client), ownerNameOf(ctx, m.db, j.TargetOwner), q.Why)
		}
	}
	return nil
}

// mercReputationOf reads a merc's record.
func (m *Mod) mercReputationOf(ctx context.Context, corpID int64) float64 {
	var ended, completed int
	var destroyed, paid float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(status = 'completed'), 0),
		       COALESCE(SUM(iskDestroyed), 0), COALESCE(SUM(paidTotal), 0)
		  FROM rolebot_merc_contracts
		 WHERE mercCorpID = ? AND status NOT IN ('active', 'failed')`, corpID).
		Scan(&ended, &completed, &destroyed, &paid)
	return mercReputation(ended, completed, destroyed, paid)
}

// mercOrphanedContracts ends, from the merc's side, any contract whose client
// has stopped settling altogether; a client whose CEO left never will.
func (m *Mod) mercOrphanedContracts(ctx context.Context, c *corpCEO) error {
	ks, err := m.mercContractsWhere(ctx, `mercCorpID = ? AND status = 'active' AND lastSettledAt < ?`,
		c.CorpID, clock.Unix()-mercStaleSettleSec)
	if err != nil {
		return err
	}
	for _, k := range ks {
		m.mercEndContract(ctx, k, "unpaid", "the client stopped settling")
	}
	return nil
}

// ── the client's side ───────────────────────────────────────────────────────

// mercClientJobs moves each of this client's open jobs one step: shortlist
// when the quotes are in, one haggling round per think, award or walk.
func (m *Mod) mercClientJobs(ctx context.Context, c *corpCEO, ch Charter) error {
	jobs, err := m.mercJobsWhere(ctx, `clientCorpID = ? AND status IN ('open', 'haggling')`, c.CorpID)
	if err != nil {
		return err
	}
	now := clock.Unix()
	for _, j := range jobs {
		if !mercOpenFor(j.Motive) {
			m.setJobStatus(ctx, j.JobID, "withdrawn", "the market is closed")
			m.ceoLog(ctx, c, powerWar, "withdraw", j.JobID, 0, "withdrew its contract on %s — the market is closed",
				ownerNameOf(ctx, m.db, j.TargetOwner))
			continue
		}
		if j.Kind == mercKindDefense {
			if _, live := warActive(ctx, m.db, j.WarID); !live {
				m.setJobStatus(ctx, j.JobID, "withdrawn", "the war is over")
				continue
			}
		}
		if j.Status == "open" {
			quoted, err := m.mercBidsFor(ctx, j.JobID, "quoted")
			if err != nil {
				return err
			}
			waited := now - j.PostedAt
			if now < j.QuoteUntil && (len(quoted) < mercShortlist || waited < mercMinQuoteSec) {
				continue
			}
			if len(quoted) == 0 {
				m.setJobStatus(ctx, j.JobID, "unfilled", "no mercenary would take it")
				m.ceoLog(ctx, c, powerWar, "unfilled", j.JobID, 0,
					"no mercenary would take its contract on %s", ownerNameOf(ctx, m.db, j.TargetOwner))
				continue
			}
			sort.SliceStable(quoted, func(a, b int) bool {
				va, vb := quoted[a].Ask/math.Max(quoted[a].Credibility, 0.01), quoted[b].Ask/math.Max(quoted[b].Credibility, 0.01)
				if va != vb {
					return va < vb
				}
				return quoted[a].Merc < quoted[b].Merc
			})
			for i, b := range quoted {
				if i < mercShortlist {
					m.setBidStatus(ctx, j.JobID, b.Merc, "shortlist")
				} else {
					m.setBidStatus(ctx, j.JobID, b.Merc, "passed")
				}
			}
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_merc_jobs SET status = 'haggling' WHERE jobID = ?`, j.JobID)
			j.Status = "haggling"
		}
		if err := m.mercHaggleRound(ctx, c, ch, j); err != nil {
			return err
		}
	}
	return nil
}

// mercHaggleRound runs one round of talks on one job.
func (m *Mod) mercHaggleRound(ctx context.Context, c *corpCEO, ch Charter, j mercJob) error {
	bids, err := m.mercBidsFor(ctx, j.JobID, "shortlist")
	if err != nil {
		return err
	}
	round := j.Round + 1
	offer := offerAt(j.Open, j.Value, round, mercRounds, ch.ConcedeExp)
	var best *mercBid
	bestPrice := 0.0
	for i := range bids {
		b := &bids[i]
		ask := askAt(b.Ask, b.Walk, round, mercRounds, b.Exp)
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_merc_bids SET lastAsk = ?, lastOffer = ? WHERE jobID = ? AND mercCorpID = ?`,
			ask, offer, j.JobID, b.Merc)
		b.LastAsk, b.LastOffer = ask, offer
		if price, ok := haggleDeal(ask, offer); ok && (best == nil || price < bestPrice) {
			best, bestPrice = b, price
		}
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_merc_jobs SET round = ? WHERE jobID = ?`, round, j.JobID)
	target := ownerNameOf(ctx, m.db, j.TargetOwner)
	if best != nil {
		return m.mercAward(ctx, c, ch, j, *best, bestPrice, bids)
	}
	if round >= mercRounds || len(bids) == 0 {
		for _, b := range bids {
			m.setBidStatus(ctx, j.JobID, b.Merc, "walked")
		}
		m.setJobStatus(ctx, j.JobID, "walked", "no price both sides could live with")
		gap := ""
		if len(bids) > 0 {
			gap = fmt.Sprintf(" (last ask %s, our offer %s)", iskString(bids[0].LastAsk), iskString(offer))
		}
		m.ceoLog(ctx, c, powerWar, "walk", j.JobID, 0,
			"walked away from talks over war on %s%s", target, gap)
		return nil
	}
	m.ceoLog(ctx, c, powerWar, "counter", j.JobID, offer,
		"offered %s a week for war on %s (round %d of %d)", iskString(offer), target, round, mercRounds)
	return nil
}

// mercAward signs a deal: the retainer paid, the war declared by the merc,
// the contract booked. Any step that fails unwinds the ones before it.
func (m *Mod) mercAward(ctx context.Context, c *corpCEO, ch Charter, j mercJob, b mercBid, weekly float64, all []mercBid) error {
	fail := func(why string) error {
		for _, x := range all {
			m.setBidStatus(ctx, j.JobID, x.Merc, "lost")
		}
		m.setJobStatus(ctx, j.JobID, "failed", why)
		m.ceoLog(ctx, c, powerWar, "hirefail", j.JobID, 0, "could not close the contract on %s — %s",
			ownerNameOf(ctx, m.db, j.TargetOwner), why)
		return nil
	}
	declare := modapi.Corp().DeclareWar
	if declare == nil {
		return fail("the server has no war hook")
	}
	mc, err := getCEO(ctx, m.db, b.Merc)
	if err != nil || mc == nil {
		return fail("the mercenary has no CEO any more")
	}
	fee := evedb.WarDeclarationCost(ctx, m.db)
	bonusCap, ok := mercBudgetTerms(j.Budget, fee, weekly, j.Weeks)
	if !ok {
		return fail("the price does not fit the budget")
	}
	retainer := fee + weekly
	if !m.canPay(ctx, c.CorpID, ch, retainer, m.commitRemaining(ctx, "mercjob", j.JobID)) {
		return fail("the treasury cannot carry the retainer")
	}
	target := ownerNameOf(ctx, m.db, j.TargetOwner)
	reason := "Mercenary retainer: war on " + target
	if _, _, err := m.corpWithdraw(ctx, c.CorpID, b.Merc, retainer, reason); err != nil {
		if errors.Is(err, evedb.ErrInsufficientFunds) {
			return fail("the treasury cannot carry the retainer")
		}
		return err
	}
	warID, err := declare(b.Merc, b.Merc, j.TargetOwner)
	if err != nil {
		if _, _, rerr := evedb.TransferISK(ctx, m.db, b.Merc, c.CorpID, retainer, refCorpWithdrawal,
			"Mercenary retainer refunded"); rerr != nil {
			rlogf("rolebots: merc retainer refund %d → %d: %v", b.Merc, c.CorpID, rerr)
		}
		return fail("the war declaration was refused: " + err.Error())
	}
	now := clock.Unix()
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_merc_contracts
			(jobID, clientCorpID, mercCorpID, kind, targetOwnerID, warID, retainer, weekly,
			 bonusRate, bonusWeeklyCap, weeks, startAt, endAt, lastSettledAt, paidTotal, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active')`,
		j.JobID, c.CorpID, b.Merc, j.Kind, j.TargetOwner, warID, retainer, weekly,
		b.BonusRate, bonusCap, j.Weeks, now, now+int64(j.Weeks)*warWeekSec, now, retainer)
	if err != nil {
		return fmt.Errorf("mercAward: %w", err)
	}
	contractID, _ := res.LastInsertId()
	if err := m.ceoWarBookAdd(ctx, warID, b.Merc, j.TargetOwner, warMotiveContract, contractID,
		"on contract for "+ownerNameOf(ctx, m.db, c.CorpID)); err != nil {
		return err
	}
	for _, x := range all {
		if x.Merc == b.Merc {
			m.setBidStatus(ctx, j.JobID, x.Merc, "won")
		} else {
			m.setBidStatus(ctx, j.JobID, x.Merc, "lost")
		}
	}
	m.setJobStatus(ctx, j.JobID, "awarded", "")
	rest := weekly*float64(j.Weeks-1) + bonusCap*float64(j.Weeks)
	if err := m.openCommit(ctx, c.CorpID, "mercctr", contractID, rest); err != nil {
		return err
	}
	m.refreshRelations(ctx)
	mercName := ownerNameOf(ctx, m.db, b.Merc)
	m.ceoLog(ctx, c, powerWar, "hire", b.Merc, retainer,
		"hired %s to make war on %s: %s a week for %d weeks plus %.0f%% of kills, %s retainer paid",
		mercName, target, iskString(weekly), j.Weeks, b.BonusRate*100, iskString(retainer))
	m.ceoLog(ctx, mc, powerWar, "contract", c.CorpID, retainer,
		"took %s's contract: war on %s for %s a week, %d weeks — declared",
		ownerNameOf(ctx, m.db, c.CorpID), target, iskString(weekly), j.Weeks)
	return nil
}

// mercClientContracts watches and pays this client's running contracts.
func (m *Mod) mercClientContracts(ctx context.Context, c *corpCEO, ch Charter) error {
	ks, err := m.mercContractsWhere(ctx, `clientCorpID = ? AND status = 'active'`, c.CorpID)
	if err != nil {
		return err
	}
	now := clock.Unix()
	for _, k := range ks {
		if _, live := warActive(ctx, m.db, k.WarID); !live {
			m.mercEndContract(ctx, k, "ended", "the war is over")
			continue
		}
		if k.Kind == mercKindDefense {
			var countered int64
			_ = m.db.QueryRowContext(ctx, `SELECT warID FROM rolebot_merc_jobs WHERE jobID = ?`, k.JobID).Scan(&countered)
			if _, live := warActive(ctx, m.db, countered); countered != 0 && !live {
				m.mercEndContract(ctx, k, "completed", "the war it answered is over")
				continue
			}
		}
		if _, ok := managedCEO(k.Merc); !ok {
			m.mercEndContract(ctx, k, "ended", "the mercenary has no CEO any more")
			continue
		}
		if m.mercFieldedNow(ctx, k.Merc) {
			k.LastFielded = now
			_, _ = m.db.ExecContext(ctx,
				`UPDATE rolebot_merc_contracts SET lastFieldedAt = ? WHERE contractID = ?`, now, k.ID)
		}
		if now-k.LastSettledAt < warWeekSec {
			continue
		}
		if err := m.mercSettle(ctx, c, ch, k, now); err != nil {
			return err
		}
	}
	return nil
}

// mercSettle is one contract's payday.
func (m *Mod) mercSettle(ctx context.Context, c *corpCEO, ch Charter, k mercContract, now int64) error {
	killed, _ := m.warScore(ctx, k.Merc, k.TargetOwner, k.LastSettledAt)
	if killed <= 0 && k.LastFielded <= k.LastSettledAt {
		m.mercEndContract(ctx, k, "unfielded", "it did not field a pilot all week")
		return nil
	}
	bonus := math.Min(killed*k.BonusRate, k.BonusCap)
	final := now >= k.EndAt
	open := m.mercContractOpen(ctx, k)
	pay := bonus
	if !final && open {
		pay += k.Weekly
	}
	if pay > 0 {
		if !m.canPay(ctx, c.CorpID, ch, pay, m.commitRemaining(ctx, "mercctr", k.ID)) {
			m.mercEndContract(ctx, k, "unpaid", "the client could not afford the next week")
			return nil
		}
		reason := "Mercenary pay: war on " + ownerNameOf(ctx, m.db, k.TargetOwner)
		if _, _, err := m.corpWithdraw(ctx, c.CorpID, k.Merc, pay, reason); err != nil {
			if errors.Is(err, evedb.ErrInsufficientFunds) {
				m.mercEndContract(ctx, k, "unpaid", "the client could not afford the next week")
				return nil
			}
			return err
		}
		m.spendCommit(ctx, "mercctr", k.ID, pay)
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_merc_contracts
		   SET lastSettledAt = ?, paidTotal = paidTotal + ?, bonusPaid = bonusPaid + ?, iskDestroyed = iskDestroyed + ?
		 WHERE contractID = ?`, now, pay, bonus, killed, k.ID)
	m.ceoLog(ctx, c, powerWar, "mercpay", k.Merc, pay,
		"paid %s %s (%s bonus on %s destroyed) for the war on %s",
		ownerNameOf(ctx, m.db, k.Merc), iskString(pay), iskString(bonus), iskString(killed),
		ownerNameOf(ctx, m.db, k.TargetOwner))
	switch {
	case final:
		m.mercEndContract(ctx, k, "completed", "the contract ran its term")
	case !open:
		m.mercEndContract(ctx, k, "ended", "the market is closed")
	}
	return nil
}

// mercFieldedNow reports whether the merc has pilots in space in a system its
// war enemies work or live in right now.
func (m *Mod) mercFieldedNow(ctx context.Context, merc int64) bool {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, solarSystemID FROM character_
		 WHERE corporationID = ? AND COALESCE(stationID, 0) = 0 AND COALESCE(solarSystemID, 0) <> 0`, merc)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var id, sys int64
		if rows.Scan(&id, &sys) == nil && !popIsOffline(id) && relWarEnemySystem(merc, sys) {
			return true
		}
	}
	return false
}

// mercEndContract closes a contract: the rest of the client's commitment is
// released, and the merc retracts its war. An unpaid bill would end it anyway;
// a retraction ends it a day from now instead of at the grace.
func (m *Mod) mercEndContract(ctx context.Context, k mercContract, status, why string) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_merc_contracts SET status = ?, endedAt = ?, endReason = ?
		 WHERE contractID = ? AND status = 'active'`, status, clock.Unix(), truncate(why, 255), k.ID)
	m.closeCommit(ctx, "mercctr", k.ID)
	if w, live := warActive(ctx, m.db, k.WarID); live && !w.Retracted.Valid {
		if err := m.retractWar(ctx, k.Merc, k.WarID); err != nil {
			rlogf("rolebots: merc contract %d: retract war %d: %v", k.ID, k.WarID, err)
		}
	}
	target := ownerNameOf(ctx, m.db, k.TargetOwner)
	if cc, err := getCEO(ctx, m.db, k.Client); err == nil && cc != nil {
		m.ceoLog(ctx, cc, powerWar, "contractend", k.Merc, k.PaidTotal,
			"contract with %s against %s ended (%s) — %s", ownerNameOf(ctx, m.db, k.Merc), target, status, why)
	}
	if mc, err := getCEO(ctx, m.db, k.Merc); err == nil && mc != nil {
		m.ceoLog(ctx, mc, powerWar, "contractend", k.Client, k.PaidTotal,
			"contract for %s against %s ended (%s) — %s; war retracted", ownerNameOf(ctx, m.db, k.Client), target, status, why)
	}
}
