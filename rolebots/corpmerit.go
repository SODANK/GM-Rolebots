package rolebots

// Merit: the rank a member earns in a bot-run corporation, the ladder under the positions (corpceo_roles.go,
// corprank.go, corptitles.go). Six ranks, one for every member, human and bot alike, climbed by work and time.
// Evidence is what a member did for the corporation: its ledger rows as counterparty (rolebot_corp_ledger),
// labour credit (rolebot_corp_labour, corppayroll.go), assignments completed (corpassign.go) and service (points
// per week of membership). Points accrue through a per-corporation cursor and a tenure clock
// (rolebot_corp_merit_cursor); the first pass backfills 90 days.
// A rank needs points and tenure, and a member with no new evidence for meritIdleDays drops one, never below
// Recruit. A benched member (corpwork.go) is not promoted. A rank carries roles OR'd onto the member's row
// (rankRoleMask records exactly the bits added, and only those are removed), a title, a payroll multiplier and
// officer weight. See docs/comment-notes-b05.md#merit-divisions.

import (
	"context"
	"fmt"
	"math"
	"strings"

	"gurimoon/corp"
)

// meritRank is one rung. Roles are cumulative: each rung lists the whole mask a member of that rank holds.
type meritRank struct {
	Name       string
	Points     float64
	TenureDays float64
	Pay        float64
	Roles      uint64
}

const (
	meritAssociateRoles = corp.RoleHangarCanQuery1 | corp.RoleHangarCanQuery2 |
		corp.RoleHangarCanQuery3 | corp.RoleHangarCanQuery4
	meritSpecialistRoles = meritAssociateRoles | corp.RoleFactoryManager
	meritSeniorRoles     = meritSpecialistRoles | corp.RoleHangarCanQuery6 |
		corp.RoleHangarCanQuery7 | corp.RoleHangarCanTake6 |
		corp.RoleCanRentFactorySlot | corp.RoleCanRentResearchSlot
	meritLeadRoles = meritSeniorRoles | corp.RoleTrader | corp.RoleHangarCanTake7
	// Never Take on division 1 (Materials): a deliver assignment
	// (corpassign.go) is paid for what lands there, and a member who could
	// take it back out could be paid for the same stack twice.
	meritVeteranRoles = meritLeadRoles | corp.RoleHangarCanTake3

	// meritBenchedStrip is what a benched member does not get while the bench
	// runs: the bench (corpwork.go) is a ban from corporation work.
	meritBenchedStrip = corp.RoleFactoryManager
)

var meritLadder = []meritRank{
	{"Recruit", 0, 0, 1.00, 0},
	{"Associate", 100, 3, 1.10, meritAssociateRoles},
	{"Specialist", 400, 10, 1.20, meritSpecialistRoles},
	{"Senior Specialist", 1200, 21, 1.30, meritSeniorRoles},
	{"Lead", 3000, 45, 1.40, meritLeadRoles},
	{"Veteran", 7000, 90, 1.50, meritVeteranRoles},
}

// Evidence weights: points per million ISK of the ledger line, by line.
var meritLineWeight = map[string]float64{
	// Cash wages (a human chair's board, and old rows): work done for the corporation. The payroll line is
	// absent so a cheque never earns the merit that sizes the next one.
	lineWages:  10,
	lineTax:    20, // tax is a tenth of the bounty: a kill is worth more than its tax
	lineLevy:   20, // the same cut of a miner's ore
	// The trade levy is a tenth of a trader's PROFIT, where the others are a
	// tenth of a gross: a point of levy is a point of money the corporation
	// actually kept, so it weighs the same as the harvest cut.
	lineTradeLevy: 20,
	lineShares:    3, // money put in
}

const (
	meritTenurePerWeek = 2.0
	meritSeatPerWeek   = 5.0
	meritIdleDays      = 30
	meritBackfillDays  = 90
	meritPassEverySec  = 3600
	meritRankTrustStep = 0.15
)

// meritRankName names a rung; out of range clamps.
func meritRankName(r int) string {
	return meritLadder[clampRank(r)].Name
}

func clampRank(r int) int {
	return min(max(r, 0), len(meritLadder)-1)
}

// meritPayMult is the pay multiplier of a rank.
func meritPayMult(r int) float64 { return meritLadder[clampRank(r)].Pay }

// meritEarned is the highest rung whose points and tenure are both met.
func meritEarned(points, tenureDays float64) int {
	r := 0
	for i, rk := range meritLadder {
		if points >= rk.Points && tenureDays >= rk.TenureDays {
			r = i
		}
	}
	return r
}

// rankFor is the pure promotion rule. cur is the rank held; idleDays is how
// long since the member's last evidence; heldDays how long cur has been
// held. A promotion is any number of rungs at once (a backfilled veteran
// should not climb one rung an hour); idle drops one rung per idle period.
func rankFor(points, tenureDays, idleDays, heldDays float64, cur int, benched bool) int {
	cur = clampRank(cur)
	if idleDays >= meritIdleDays {
		if cur > 0 && heldDays >= meritIdleDays {
			return cur - 1
		}
		return cur
	}
	earned := meritEarned(points, tenureDays)
	if earned > cur && !benched {
		return earned
	}
	if earned < cur {
		// Points never fall, so this is only a ladder retune: follow it.
		return earned
	}
	return cur
}

// rankRoles is the role mask a rank carries, less what a bench withholds.
func rankRoles(r int, benched bool) uint64 {
	mask := meritLadder[clampRank(r)].Roles
	if benched {
		mask &^= meritBenchedStrip
	}
	return mask
}

// meritRoleDelta applies a target rank mask to a member's roles. Only bits
// the ladder itself added (rankMask) are ever removed; a bit already on the
// row when the rank wanted it is not adopted, so it is never taken away.
func meritRoleDelta(current, rankMask, target uint64) (roles, newRankMask uint64) {
	add := target &^ current
	remove := rankMask &^ target
	roles = (current | add) &^ remove
	newRankMask = (rankMask & target) | add
	return roles, newRankMask
}

// meritTrustMul is the weight rank adds to an officer candidate's trust.
func meritTrustMul(r int) float64 { return 1 + meritRankTrustStep*float64(clampRank(r)) }

// ── reading ─────────────────────────────────────────────────────────────────

// meritRow is one member's standing.
type meritRow struct {
	CharID       int64
	Points       float64
	Rank         int
	RankSince    int64
	LastActiveAt int64
	WelcomedAt   int64
	RankRoleMask uint64
}

func (m *Mod) loadMerit(ctx context.Context, corpID int64) (map[int64]*meritRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, points, rankNo, rankSince, lastActiveAt, welcomedAt, rankRoleMask
		  FROM rolebot_corp_merit WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("loadMerit: %w", err)
	}
	defer rows.Close()
	out := map[int64]*meritRow{}
	for rows.Next() {
		r := &meritRow{}
		if err := rows.Scan(&r.CharID, &r.Points, &r.Rank, &r.RankSince, &r.LastActiveAt,
			&r.WelcomedAt, &r.RankRoleMask); err != nil {
			return nil, err
		}
		out[r.CharID] = r
	}
	return out, rows.Err()
}

// meritRankOf is one member's rank, 0 when the ladder is off or unknown.
func (m *Mod) meritRankOf(ctx context.Context, corpID, charID int64) int {
	if !meritOn() {
		return 0
	}
	var r int
	_ = m.db.QueryRowContext(ctx,
		`SELECT rankNo FROM rolebot_corp_merit WHERE corporationID = ? AND characterID = ?`,
		corpID, charID).Scan(&r)
	return clampRank(r)
}

// meritRanksOf is every ranked member of a corporation; empty when off.
func (m *Mod) meritRanksOf(ctx context.Context, corpID int64) map[int64]int {
	out := map[int64]int{}
	if !meritOn() {
		return out
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, rankNo FROM rolebot_corp_merit WHERE corporationID = ?`, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r int
		if rows.Scan(&id, &r) == nil {
			out[id] = clampRank(r)
		}
	}
	return out
}

// meritPayFor is the pay multiplier a member's rank earns in a bot-run
// corporation; 1 anywhere else or with the ladder off.
func (m *Mod) meritPayFor(ctx context.Context, corpID, charID int64) float64 {
	if !meritOn() || charID == 0 {
		return 1
	}
	if _, managed := managedCEO(corpID); !managed {
		return 1
	}
	return meritPayMult(m.meritRankOf(ctx, corpID, charID))
}

// addMerit credits points to one member now, such as a completed assignment. lastActiveAt moves with it,
// since this is evidence.
func (m *Mod) addMerit(ctx context.Context, corpID, charID int64, pts float64, reason string) {
	if pts == 0 || charID == 0 {
		return
	}
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_merit (corporationID, characterID, points, rankSince, lastActiveAt)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE points = points + VALUES(points), lastActiveAt = VALUES(lastActiveAt)`,
		corpID, charID, pts, now, now); err != nil {
		rlogf("rolebots: add merit %d/%d: %v", corpID, charID, err)
		return
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_merit_log (corporationID, characterID, at, points, reason)
		VALUES (?, ?, ?, ?, ?)`, corpID, charID, now, pts, truncate(reason, 255))
}

// ── the pass ────────────────────────────────────────────────────────────────

// meritDesk is the ladder's turn on the staff desk sweep, hourly.
func (m *Mod) meritDesk(ctx context.Context, c *corpCEO) {
	if !meritOn() || !deskDue(c.CorpID, "merit", meritPassEverySec) {
		return
	}
	if err := m.meritPass(ctx, c); err != nil {
		m.logBot(c.CharID, logError, "ceo", "merit: %v", err)
	}
}

// meritPass accrues new evidence, then re-ranks every member.
func (m *Mod) meritPass(ctx context.Context, c *corpCEO) error {
	now := clock.Unix()
	var cursor, tenureAt, labourCursor int64
	fresh := false
	if err := m.db.QueryRowContext(ctx, `
		SELECT ledgerEntryID, tenureAt, labourEntryID FROM rolebot_corp_merit_cursor WHERE corporationID = ?`,
		c.CorpID).Scan(&cursor, &tenureAt, &labourCursor); err != nil {
		fresh = true
	}

	members, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return err
	}
	inCorp := map[int64]corpMember{}
	for _, cm := range members {
		if cm.Eligible() {
			inCorp[cm.CharID] = cm
		}
	}

	// Ledger evidence since the cursor (or the last 90 days, the first time).
	gain := map[int64]float64{}
	why := map[int64][]string{}
	since := int64(0)
	if fresh {
		since = now - meritBackfillDays*86400
	}
	lines := make([]string, 0, len(meritLineWeight))
	for l := range meritLineWeight {
		lines = append(lines, "'"+l+"'")
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT counterID, line, SUM(ABS(amount)), MAX(entryID)
		  FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND entryID > ? AND at >= ? AND counterID <> 0
		   AND line IN (`+strings.Join(lines, ",")+`)
		 GROUP BY counterID, line`, c.CorpID, cursor, since)
	if err != nil {
		return fmt.Errorf("merit ledger: %w", err)
	}
	maxEntry := cursor
	for rows.Next() {
		var id, top int64
		var line string
		var isk float64
		if rows.Scan(&id, &line, &isk, &top) != nil {
			continue
		}
		maxEntry = max(maxEntry, top)
		if _, ok := inCorp[id]; !ok {
			continue
		}
		pts := isk / 1e6 * meritLineWeight[line]
		if pts < 0.5 {
			continue
		}
		gain[id] += pts
		why[id] = append(why[id], fmt.Sprintf("%s %.0f", corpLineLabel[line], pts))
	}
	rows.Close()
	if fresh {
		// The cursor must sit at the ledger's head even when this
		// corporation has no countable rows, or the next pass rereads.
		var head int64
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(entryID), 0) FROM rolebot_corp_ledger`).Scan(&head)
		maxEntry = max(maxEntry, head)
	}
	// Labour credit since its own cursor (corppayroll.go): work done for a corporation that pays by the weekly
	// payroll, at the weight wages carried when paid in cash.
	lrows, err := m.db.QueryContext(ctx, `
		SELECT characterID, SUM(credit), MAX(entryID)
		  FROM rolebot_corp_labour
		 WHERE corporationID = ? AND entryID > ? AND at >= ?
		 GROUP BY characterID`, c.CorpID, labourCursor, since)
	if err != nil {
		return fmt.Errorf("merit labour: %w", err)
	}
	maxLabour := labourCursor
	for lrows.Next() {
		var id, top int64
		var credit float64
		if lrows.Scan(&id, &credit, &top) != nil {
			continue
		}
		maxLabour = max(maxLabour, top)
		if _, ok := inCorp[id]; !ok {
			continue
		}
		pts := credit / 1e6 * meritLabourWeight
		if pts < 0.5 {
			continue
		}
		gain[id] += pts
		why[id] = append(why[id], fmt.Sprintf("labour %.0f", pts))
	}
	lrows.Close()
	if fresh {
		var head int64
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(entryID), 0) FROM rolebot_corp_labour WHERE corporationID = ?`, c.CorpID).Scan(&head)
		maxLabour = max(maxLabour, head)
	}

	active := map[int64]bool{}
	for id := range gain {
		active[id] = true
	}

	// Service: whole weeks since the tenure clock. The first pass counts
	// each member's whole service (capped to the backfill window).
	weeks := 0.0
	if fresh {
		tenureAt = now
	} else if now-tenureAt >= 7*86400 {
		weeks = math.Floor(float64(now-tenureAt) / (7 * 86400))
		tenureAt += int64(weeks) * 7 * 86400
	}
	seats := map[int64]bool{}
	if ids, err := int64Column(ctx, m,
		`SELECT characterID FROM rolebot_corp_ranks WHERE corporationID = ?`, c.CorpID); err == nil {
		for _, id := range ids {
			seats[id] = true
		}
	}
	nowW := win32Now()
	for id, cm := range inCorp {
		w := weeks
		if fresh {
			w = math.Floor(math.Min(tenureDays(cm.Since, nowW), meritBackfillDays) / 7)
		}
		if w <= 0 {
			continue
		}
		pts := w * meritTenurePerWeek
		if seats[id] {
			pts += w * meritSeatPerWeek
		}
		gain[id] += pts
		why[id] = append(why[id], fmt.Sprintf("service %.0f", pts))
	}

	// Book the gains.
	for id, pts := range gain {
		// A new row starts its activity clock now; an existing one moves only on real evidence. Service alone is
		// not activity.
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_merit (corporationID, characterID, points, rankSince, lastActiveAt)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE points = points + VALUES(points),
			  lastActiveAt = IF(?, VALUES(lastActiveAt), lastActiveAt)`,
			c.CorpID, id, pts, now, now, active[id]); err != nil {
			return fmt.Errorf("merit book: %w", err)
		}
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_merit_log (corporationID, characterID, at, points, reason)
			VALUES (?, ?, ?, ?, ?)`, c.CorpID, id, now, pts, truncate(strings.Join(why[id], ", "), 255))
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_merit_cursor (corporationID, ledgerEntryID, tenureAt, labourEntryID) VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE ledgerEntryID = VALUES(ledgerEntryID), tenureAt = VALUES(tenureAt),
		  labourEntryID = VALUES(labourEntryID)`,
		c.CorpID, maxEntry, tenureAt, maxLabour); err != nil {
		return fmt.Errorf("merit cursor: %w", err)
	}

	return m.meritRerank(ctx, c, inCorp, fresh)
}

// meritRerank applies the promotion rule and the role delta to every member.
func (m *Mod) meritRerank(ctx context.Context, c *corpCEO, inCorp map[int64]corpMember, fresh bool) error {
	merit, err := m.loadMerit(ctx, c.CorpID)
	if err != nil {
		return err
	}
	benched := map[int64]bool{}
	now := clock.Unix()
	if ids, err := int64Column(ctx, m, `
		SELECT characterID FROM rolebot_corp_bench WHERE corporationID = ? AND untilAt > ?`,
		c.CorpID, now); err == nil {
		for _, id := range ids {
			benched[id] = true
		}
	}
	nowW := win32Now()
	corpName := ""
	changedAny := false
	for id, cm := range inCorp {
		r := merit[id]
		if r == nil {
			r = &meritRow{CharID: id, RankSince: now, LastActiveAt: now}
			if _, err := m.db.ExecContext(ctx, `
				INSERT IGNORE INTO rolebot_corp_merit (corporationID, characterID, rankSince, lastActiveAt)
				VALUES (?, ?, ?, ?)`, c.CorpID, id, now, now); err != nil {
				return err
			}
		}
		idle := float64(now-r.LastActiveAt) / 86400
		if r.LastActiveAt == 0 || fresh {
			idle = 0 // never measured yet: not idle
		}
		held := float64(now-r.RankSince) / 86400
		next := rankFor(r.Points, tenureDays(cm.Since, nowW), idle, held, r.Rank, benched[id])

		target := rankRoles(next, benched[id])
		// With arms on (corparm.go), installing corporation jobs is the Industry arm's work: other arms earn the
		// hangar views but not Factory Manager.
		if s, ok := armOf(id, c.CorpID); ok && s.Arm != armIndustry && s.Arm != armResearch {
			target &^= corp.RoleFactoryManager
		}
		target = m.stripGuardedTake(ctx, c.CorpID, id, target)
		roles, mask := meritRoleDelta(cm.Roles, r.RankRoleMask, target)
		if roles != cm.Roles {
			if err := m.setMemberRoles(ctx, id, roles); err != nil {
				return fmt.Errorf("merit roles %d: %w", id, err)
			}
		}
		if next == r.Rank && mask == r.RankRoleMask {
			continue
		}
		since := r.RankSince
		if next != r.Rank {
			since = now
		}
		if _, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_merit SET rankNo = ?, rankSince = ?, rankRoleMask = ?
			 WHERE corporationID = ? AND characterID = ?`,
			next, since, mask, c.CorpID, id); err != nil {
			return fmt.Errorf("merit rank %d: %w", id, err)
		}
		if next == r.Rank {
			continue
		}
		changedAny = true
		if corpName == "" {
			corpName = m.corpNameOf(ctx, c.CorpID)
		}
		m.announceRank(ctx, c, cm, r.Rank, next, r.Points, corpName, fresh)
	}
	if changedAny {
		requestSeatPass(c.CorpID)
		// The rank is the title of a member with no position: re-sync now
		// rather than at the next think.
		m.syncCorpTitles(ctx, c.CorpID)
	}
	return nil
}

// announceRank tells the member (a human, by mail) and the corporation (the
// bulletin). The first pass's backfill promotes quietly to the corporation:
// a hundred "promoted" lines on the day the ladder is switched on is noise.
func (m *Mod) announceRank(ctx context.Context, c *corpCEO, cm corpMember, from, to int, points float64, corpName string, fresh bool) {
	up := to > from
	verb := "promoted"
	if !up {
		verb = "stood down"
	}
	text := fmt.Sprintf("%s %s from %s to %s.", cm.Name, verb, meritRankName(from), meritRankName(to))
	m.logBot(c.CharID, logInfo, "ceo", "merit: %s", text)
	if !fresh && up {
		var about int64
		if cm.Human() {
			about = cm.CharID
		}
		m.queueCorpNews(ctx, c.CorpID, newsRanks, text, about)
	}
	if !cm.Human() {
		return
	}
	var b strings.Builder
	line(&b, "%s,", cm.Name)
	b.WriteString("<br>")
	if up {
		line(&b, "You have been promoted to %s of %s.", meritRankName(to), corpName)
		line(&b, "Your record stands at %.0f merit.", points)
		line(&b, "The rank weighs your merit ×%.2f in the weekly payroll, and opens:", meritPayMult(to))
		for _, s := range meritRoleWords(rankRoles(to, false) &^ rankRoles(from, false)) {
			line(&b, "- %s", s)
		}
		if to+1 < len(meritLadder) {
			nx := meritLadder[to+1]
			line(&b, "Next: %s at %.0f merit and %.0f days' service.", nx.Name, nx.Points, nx.TenureDays)
		}
	} else {
		line(&b, "You have not worked for %s in %d days, and your rank has dropped to %s.",
			corpName, meritIdleDays, meritRankName(to))
		line(&b, "Work for the corporation again and it comes back as fast as you earn it.")
	}
	line(&b, "Your record: %s.", igbLink(deskViewPath(c.CorpID, cm.CharID), "your desk"))
	sender, hat := m.letterFrom(ctx, c, mailFromPersonnel)
	b.WriteString(m.signature(ctx, sender, hat, corpName))
	subject := fmt.Sprintf("%s: %s", corpName, meritRankName(to))
	m.mailPilot(ctx, cm.CharID, sender, subject, b.String())
}

// meritRoleWords says what a set of role bits lets a member do.
func meritRoleWords(mask uint64) []string {
	var out []string
	q := []uint64{corp.RoleHangarCanQuery1, corp.RoleHangarCanQuery2, corp.RoleHangarCanQuery3,
		corp.RoleHangarCanQuery4, corp.RoleHangarCanQuery5, corp.RoleHangarCanQuery6, corp.RoleHangarCanQuery7}
	t := []uint64{corp.RoleHangarCanTake1, corp.RoleHangarCanTake2, corp.RoleHangarCanTake3,
		corp.RoleHangarCanTake4, corp.RoleHangarCanTake5, corp.RoleHangarCanTake6, corp.RoleHangarCanTake7}
	var see, take []string
	for i := range q {
		if mask&q[i] != 0 {
			see = append(see, fmt.Sprint(i+1))
		}
		if mask&t[i] != 0 {
			take = append(take, fmt.Sprint(i+1))
		}
	}
	if len(see) > 0 {
		out = append(out, "view corporation hangar division(s) "+strings.Join(see, ", "))
	}
	if len(take) > 0 {
		out = append(out, "take from corporation hangar division(s) "+strings.Join(take, ", "))
	}
	if mask&corp.RoleFactoryManager != 0 {
		out = append(out, "install corporation jobs from the blueprint library (Factory Manager)")
	}
	if mask&(corp.RoleCanRentFactorySlot|corp.RoleCanRentResearchSlot) != 0 {
		out = append(out, "rent factory and research slots for the corporation")
	}
	if mask&corp.RoleTrader != 0 {
		out = append(out, "trade on the corporation's behalf (Trader)")
	}
	if len(out) == 0 {
		out = append(out, "the standing that comes with the rank")
	}
	return out
}

// isMeritTitle reports whether a title text is one of the ladder's names.
func isMeritTitle(s string) bool {
	for _, r := range meritLadder {
		if r.Name == s {
			return true
		}
	}
	return false
}

// meritBench reads whether a member is benched right now; used where a
// single answer is needed (the assignment generator).
func (m *Mod) meritBenched(ctx context.Context, corpID, charID int64) bool {
	var until int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT untilAt FROM rolebot_corp_bench WHERE corporationID = ? AND characterID = ?`,
		corpID, charID).Scan(&until)
	return until > clock.Unix()
}
