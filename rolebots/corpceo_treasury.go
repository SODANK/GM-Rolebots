package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The treasury power: what a CEO does with the corporation's money. Bounty tax
// lands in the wallet (bountypay.go) at corporation.taxRate. A CEO sets the
// rate (envy), keeps a reserve it will not spend below (caution, in months of
// office rent; an unpaid rental bill evicts, and ceoPayRent in corpoffices.go
// pays it), and spends the surplus on skillbooks for needed positions
// (Leadership for fleet-seat candidates, Corporation Management for the CEO
// when applicants are held) and on stipends to members whose wallets ran dry,
// capped per week by generosity. Every ISK moves through corpWithdraw
// (evedb.TransferISKBy): one transaction, a journal row in both wallets, and a
// debit that cannot take the corporation negative.

const (
	// ceoTaxStep is the most a CEO moves the tax rate in one change, and
	// ceoTaxEverySec how often it changes it, so members see no overnight jump.
	ceoTaxStep     = 0.02
	ceoTaxEverySec = 7 * 86400

	// refCorpWithdrawal is the journal refType a corporation payout carries.
	refCorpWithdrawal = 37

	// ceoStipendWeek is the window the stipend budget is counted over.
	ceoStipendWeek = 7 * 86400
	// ceoStipendMax caps any single stipend.
	ceoStipendMax = 5_000_000
	// ceoStipendsPerThink bounds one think's payouts.
	ceoStipendsPerThink = 3
	// ceoStipendNeedFrac: a member is "run dry" below this share of the
	// comfort line the appetite code uses (appetite.go needComfortISK).
	ceoStipendNeedFrac = 0.25
	// ceoStipendTriggerFrac: a stipend is paid only once a member's wallet is under
	// this share of the run-dry line, and tops it up to the whole line. Paying at
	// the line itself topped up hovering wallets over and over.
	ceoStipendTriggerFrac = 0.5
	// ceoStipendPerMemberWeek is how many stipends one member may draw in a
	// ceoStipendWeek; a member who runs dry more often is spending on something the
	// corporation should not fund.
	ceoStipendPerMemberWeek = 2
	// ceoStipendFloorISK is the treasury under which no stipend is paid: the surplus
	// is measured over a rent-sized reserve, so a small corporation would otherwise
	// spend itself to nothing.
	ceoStipendFloorISK = 5_000_000

	// skillCorpMgmt is Corporation Management; each level is ten openings.
	skillCorpMgmt = int64(3363)
)

// corpWithdraw pays out of a corporation's wallet as a refType-37 withdrawal,
// signed by whoever is spending it. The client prints that row with the
// signer's name; an unsigned row reads Id:None.
func (m *Mod) corpWithdraw(ctx context.Context, corpID, dstID int64, amount float64, reason string) (srcBal, dstBal float64, err error) {
	return evedb.TransferISKBy(ctx, m.db, corpID, dstID, amount, refCorpWithdrawal, reason, m.corpSigner(ctx, corpID))
}

// corpSigner is the hand on a corporation's wallet: the staff actor riding
// ctx (the Treasurer, or whoever covers the post), else the CEO. 0 if neither.
func (m *Mod) corpSigner(ctx context.Context, corpID int64) int64 {
	if a, ok := staffActorOf(ctx); ok {
		return a.CharID
	}
	if c, err := getCEO(ctx, m.db, corpID); err == nil && c != nil {
		return c.CharID
	}
	// A person's chair: what its desk pays is paid by its order.
	if id, ok := humanChaired(corpID); ok {
		return id
	}
	return 0
}

// nextOpeningBook is the member-limit book a CEO needs next: Corporation
// Management while it has none, Megacorp Management once that is at V, Empire
// Control once Megacorp is at V. False while the current rung is still training,
// since a book without its prerequisite cannot be injected.
func nextOpeningBook(levels map[int64]int) (int64, bool) {
	ladder := []int64{skillCorpMgmt, skillMegacorp, skillEmpireControl}
	for i, id := range ladder {
		lvl, has := levels[id]
		if !has {
			if i == 0 || levels[ladder[i-1]] >= 5 {
				return id, true
			}
			return 0, false
		}
		if lvl < 5 {
			return 0, false
		}
	}
	return 0, false
}

// corpMonthlyRent is what the corporation's offices will cost it this month (the
// quote the bill sweep charges each office), or, with no office yet, what the
// first would cost at the server's floor rate.
func (m *Mod) corpMonthlyRent(ctx context.Context, corpID int64) float64 {
	// Once per think (thinkctx.go): most powers read the reserve, and the
	// reserve is this times the charter. Renting an office drops it.
	tc := thinkCacheOf(ctx, corpID)
	if tc != nil {
		tc.mu.Lock()
		ok, rent := tc.rentOK, tc.rent
		tc.mu.Unlock()
		if ok {
			return rent
		}
	}
	rent := m.readCorpMonthlyRent(ctx, corpID)
	if tc != nil {
		tc.mu.Lock()
		tc.rentOK, tc.rent = true, rent
		tc.mu.Unlock()
	}
	return rent
}

// forgetThinkRent drops the carried rent after an office changes hands.
func forgetThinkRent(ctx context.Context, corpID int64) {
	if tc := thinkCacheOf(ctx, corpID); tc != nil {
		tc.mu.Lock()
		tc.rentOK = false
		tc.mu.Unlock()
	}
}

func (m *Mod) readCorpMonthlyRent(ctx context.Context, corpID int64) float64 {
	// An office at a station the corporation owns pays no rent (evedb.OpenOwnerOffice).
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID FROM crpoffices o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ? AND s.corporationID <> o.corporationID`, corpID)
	if err != nil {
		return evedb.OfficeRentCost(ctx, m.db)
	}
	var stations []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			stations = append(stations, id)
		}
	}
	rows.Close()
	if len(stations) == 0 {
		return evedb.OfficeRentCost(ctx, m.db)
	}
	total := 0.0
	for _, s := range stations {
		total += evedb.OfficeRentQuote(ctx, m.db, s)
	}
	return total
}

// corpReserve is the floor a CEO will not spend the corporation below.
func (m *Mod) corpReserve(ctx context.Context, corpID int64, ch Charter) float64 {
	return ch.ReserveMonths * m.corpMonthlyRent(ctx, corpID)
}

// lastCEOActionAt is when this corporation's CEO last took a decision of this
// verb, 0 for never. The ledger is the memory; nothing else stores it.
func (m *Mod) lastCEOActionAt(ctx context.Context, corpID int64, verb string) int64 {
	// Under a think, one GROUP BY for every verb on the first ask; ceoLog
	// keeps the map current (thinkctx.go).
	if tc := thinkCacheOf(ctx, corpID); tc != nil {
		tc.mu.Lock()
		defer tc.mu.Unlock()
		if !tc.actionsOK {
			rows, err := m.db.QueryContext(ctx, `
				SELECT verb, MAX(at) FROM rolebot_ceo_actions
				 WHERE corporationID = ? GROUP BY verb`, corpID)
			if err == nil {
				all := map[string]int64{}
				for rows.Next() {
					var v string
					var at int64
					if rows.Scan(&v, &at) == nil {
						all[v] = at
					}
				}
				rows.Close()
				if rows.Err() == nil {
					tc.actions, tc.actionsOK = all, true
				}
			}
		}
		if tc.actionsOK {
			return tc.actions[verb]
		}
	}
	var at int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(at), 0) FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND verb = ?`, corpID, verb).Scan(&at)
	return at
}

// lastCEOActionFor is when this corporation last logged this verb about one
// subject (a pilot, a station), 0 for never. Uncached: callers ask it once per
// subject, not per think.
func (m *Mod) lastCEOActionFor(ctx context.Context, corpID int64, verb string, subjectID int64) int64 {
	var at int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(at), 0) FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND verb = ? AND subjectID = ?`, corpID, verb, subjectID).Scan(&at)
	return at
}

// ceoSpentSince sums the ledger amounts of one verb since a time.
func (m *Mod) ceoSpentSince(ctx context.Context, corpID int64, verb string, since int64) float64 {
	var v float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND verb = ? AND at >= ?`, corpID, verb, since).Scan(&v)
	return v
}

// stockLines are the ledger lines that buy goods the corporation means to sell
// or build with. They are capital put to work, not the cost of running the
// house, so they do not make a trade house read as starved.
var stockLines = []string{lineProcure, lineBuyback, lineBPO, lineTradeBuy}

// corpWeeklyRunningCost is a week of what keeping the corporation going
// costs: every outflow but the capital lines and the stock it buys.
func (m *Mod) corpWeeklyRunningCost(ctx context.Context, corpID int64) float64 {
	skip := append(append([]string{}, capitalLines...), stockLines...)
	args := []any{corpID, clock.Unix() - 7*86400}
	for _, l := range skip {
		args = append(args, l)
	}
	var out float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(-SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND at >= ? AND amount < 0
		   AND line NOT IN (`+placeholders(len(skip))+`)`, args...).Scan(&out)
	return math.Max(0, out)
}

// ceoTaxNext is the next tax rate: the charter's at once while the
// corporation is starved, otherwise one step toward it. Pure.
func ceoTaxNext(cur, charter float64, starved bool) float64 {
	if starved && charter > cur {
		return charter
	}
	return stepToward(cur, charter, ceoTaxStep)
}

// stepToward moves cur toward target by at most step.
func stepToward(cur, target, step float64) float64 {
	switch {
	case target > cur+step:
		return cur + step
	case target < cur-step:
		return cur - step
	}
	return target
}

func (m *Mod) ceoTreasury(ctx context.Context, c *corpCEO, ch Charter) error {
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}

	// Tax rate: a corporation whose free capital would not cover a week of its own
	// spending goes straight to the charter's rate instead of climbing.
	starved := false
	if facts.TaxRate < ch.TaxRate-0.005 {
		if free, _, ferr := m.corpFreeCapital(ctx, c.CorpID, ch); ferr == nil {
			starved = free < m.corpWeeklyRunningCost(ctx, c.CorpID)
		}
	}
	if math.Abs(facts.TaxRate-ch.TaxRate) > 0.005 &&
		(starved || clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "tax") >= ceoTaxEverySec) {
		next := math.Round(ceoTaxNext(facts.TaxRate, ch.TaxRate, starved)*1000) / 1000
		var desc, url string
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(description, ''), COALESCE(url, '') FROM corporation WHERE corporationID = ?`,
			c.CorpID).Scan(&desc, &url)
		if _, _, old, err := evedb.UpdateCorporationDetails(ctx, m.db, c.CorpID, desc, url, next); err != nil {
			return err
		} else {
			pushCorpChanged(c.CorpID, facts.Name, esp.CorpFieldChange{Column: "taxRate", Old: old, New: next})
			m.ceoLog(ctx, c, powerTreasury, "tax", c.CorpID, next,
				"set the tax rate from %.0f%% to %.0f%% (heading for %.0f%%)", old*100, next*100, ch.TaxRate*100)
		}
	}

	// CEO backing (corpceo_backing.go) runs before anything reads the surplus, so a
	// bail-out is spendable this think. It stays the CEO's even with a Treasurer.
	if err := m.ceoBackCorp(ctx, c, ch); err != nil {
		m.logBot(c.CharID, logError, "ceo", "backing its corporation: %v", err)
	}

	// Rent runs after the bail-out and before any payout: an unpaid rental bill
	// evicts. It is the CEO's duty even when a Treasurer holds the purse.
	if err := m.ceoPayRent(ctx, c); err != nil {
		m.logBot(c.CharID, logError, "ceo", "paying office rent: %v", err)
	}

	// Paying out is the Treasurer's when the corporation has one
	// (corpstaff_treasurer.go); otherwise the CEO pays to the charter exactly.
	if m.staffCovers(ctx, c, postTreasurer, "") {
		return nil
	}
	if err := m.payrollDue(ctx, c, ch); err != nil {
		m.logBot(c.CharID, logError, "ceo", "payroll: %v", err)
	}
	return m.treasuryDisburse(ctx, c, ch, neutralTreasury)
}

// ceoFundBooks pays for the skillbooks the corporation's positions need.
// Returns what it spent.
func (m *Mod) ceoFundBooks(ctx context.Context, c *corpCEO, surplus float64) (float64, error) {
	spent := 0.0
	// Member-limit ladder, while applicants are held or the limit is reached. Buy
	// the next rung (nextOpeningBook), not only the first, or the ladder stalls.
	if corpWantsOpenings(c.CorpID) {
		levels, err := evedb.TrainedSkillLevels(ctx, m.db, c.CharID)
		if err == nil {
			if next, ok := nextOpeningBook(levels); ok {
				if cost, ok := m.corpBuysBook(ctx, c, c.CharID, next, surplus-spent,
					"to take on the applicants waiting at the door"); ok {
					spent += cost
				}
			}
		}
	}
	// A capital program's books: the builder's Capital Ship Construction and
	// the pilot's kit (corpprogram.go).
	spent += m.programBooks(ctx, c, surplus-spent)
	// Leadership for fleet-seat candidates. The promotion pass buys it from the
	// member's wallet when seats outnumber commanders; a managed corporation pays.
	cands, err := m.rankCorpFCs(ctx, c.CorpID)
	if err != nil {
		return spent, nil
	}
	bought := 0
	for _, cand := range cands {
		if bought >= 1 || cand.Leadership >= 1 || cand.CharID == c.CharID || cand.CommandDrive < 1 {
			continue
		}
		levels, err := evedb.TrainedSkillLevels(ctx, m.db, cand.CharID)
		if err != nil {
			continue
		}
		if _, has := levels[skillLeadership]; has {
			continue
		}
		if cost, ok := m.corpBuysBook(ctx, c, cand.CharID, skillLeadership, surplus-spent,
			"to lead one of its fleets"); ok {
			spent += cost
			bought++
		}
	}
	return spent, nil
}

// corpBuysBook moves the price of a skillbook from the corporation to a member,
// who buys and queues it through the ordinary shop path. ok is false when the
// book is not for sale, costs more than the budget, or anything fails; the
// payout is made only once the price is known to fit.
func (m *Mod) corpBuysBook(ctx context.Context, c *corpCEO, charID, typeID int64, budget float64, why string) (float64, bool) {
	// Asked of the entity table, not TrainedSkillLevels: that map omits a book
	// injected at level 0, and the payout lands before PurchaseAndInjectSkill refuses.
	if owned, err := m.skillInjected(ctx, charID, typeID); err != nil || owned {
		return 0, false
	}
	q, err := evedb.SkillMarketPrice(ctx, m.db, typeID)
	if err != nil || q.Price <= 0 || q.Price > budget {
		return 0, false
	}
	name := skillNameOf(ctx, m, typeID)
	if _, _, err := m.corpWithdraw(ctx, c.CorpID, charID, q.Price,
		"Corporation-funded skillbook: "+name); err != nil {
		if !errors.Is(err, evedb.ErrInsufficientFunds) {
			m.logBot(c.CharID, logError, "ceo", "paying for a %s book: %v", name, err)
		}
		return 0, false
	}
	m.corpBook(ctx, c.CorpID, lineBooks, -q.Price, charID, name)
	forgetCharSkills(charID)
	if _, err := evedb.PurchaseAndInjectSkill(ctx, m.db, charID, typeID); err != nil {
		m.logBot(c.CharID, logError, "ceo", "a corp-funded %s book was not bought: %v", name, err)
		return q.Price, true // the money moved; the member keeps it
	}
	if _, err := evedb.AddSkillToQueue(ctx, m.db, charID, typeID); err != nil {
		m.logBot(charID, logError, "skills", "queueing %s: %v", name, err)
	}
	var who string
	_ = m.db.QueryRowContext(ctx, `SELECT characterName FROM character_ WHERE characterID = ?`, charID).Scan(&who)
	m.ceoLog(ctx, c, powerTreasury, "skillbook", charID, q.Price,
		"paid %s for %s's %s book, %s", iskString(q.Price), who, name, why)
	return q.Price, true
}

// skillInjected reports whether charID holds typeID as a skill at any level,
// level 0 included: the predicate PurchaseAndInjectSkill refuses on.
func (m *Mod) skillInjected(ctx context.Context, charID, typeID int64) (bool, error) {
	var n int
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity WHERE typeID = ? AND ownerID = ? AND flag IN (7, 61)`,
		typeID, charID).Scan(&n)
	return n > 0, err
}

// skillNameOf is a best-effort type name.
func skillNameOf(ctx context.Context, m *Mod, typeID int64) string {
	var name string
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeName FROM invTypes WHERE typeID = ?`, typeID).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("type %d", typeID)
	}
	return name
}

// ceoStipends tops up members whose wallets have run dry, human members
// included (corpmembers.go). directiveFirst (a devoted Treasurer, treasurerBend)
// queues members whose activity is on the corporation's directive first.
func (m *Mod) ceoStipends(ctx context.Context, c *corpCEO, budget float64, directiveFirst bool) error {
	floor := needComfortISK * ceoStipendNeedFrac
	order := "ch.balance"
	if directiveFirst {
		// Two literal orderings rather than a bound CASE: a placeholder in a
		// CASE is the MariaDB trap that buckets every row into one value.
		order = "(d.activity IS NULL), ch.balance"
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT ch.characterID, ch.characterName, ch.balance
		  FROM character_ ch
		  LEFT JOIN rolebot_characters b ON b.characterID = ch.characterID
		  LEFT JOIN rolebot_state s ON s.characterID = ch.characterID
		  LEFT JOIN rolebot_corp_directive d
		         ON d.corporationID = ch.corporationID AND d.activity = s.activity
		 WHERE ch.corporationID = ? AND ch.characterID <> ? AND ch.accountID > 0
		   AND (b.characterID IS NULL OR b.enabled = 1)
		   AND ch.balance < ?
		   AND (SELECT COUNT(*) FROM rolebot_corp_ledger l
		         WHERE l.corporationID = ch.corporationID AND l.at > ?
		           AND l.line = ? AND l.counterID = ch.characterID) < ?
		 ORDER BY `+order+`
		 LIMIT ?`, c.CorpID, c.CharID, floor*ceoStipendTriggerFrac,
		clock.Unix()-ceoStipendWeek, lineStipend, ceoStipendPerMemberWeek, ceoStipendsPerThink)
	if err != nil {
		return fmt.Errorf("ceoStipends: %w", err)
	}
	type broke struct {
		id   int64
		name string
		bal  float64
	}
	var list []broke
	for rows.Next() {
		var b broke
		if rows.Scan(&b.id, &b.name, &b.bal) == nil {
			list = append(list, b)
		}
	}
	rows.Close()
	for _, b := range list {
		amount := math.Min(math.Min(floor-b.bal, ceoStipendMax), budget)
		amount = math.Floor(amount)
		if amount < 1000 {
			return nil
		}
		_, bal, err := m.corpWithdraw(ctx, c.CorpID, b.id, amount,
			"Corporation stipend")
		if err != nil {
			if errors.Is(err, evedb.ErrInsufficientFunds) {
				return nil
			}
			return err
		}
		m.pushBalance(b.id, bal)
		budget -= amount
		m.corpBook(ctx, c.CorpID, lineStipend, -amount, b.id, "stipend to "+b.name)
		m.noteGraceFavour(b.id, c.CharID)
		m.ceoLog(ctx, c, powerTreasury, "stipend", b.id, amount,
			"paid %s a stipend of %s — its wallet had run down to %s", b.name, iskString(amount), iskString(b.bal))
	}
	return nil
}
