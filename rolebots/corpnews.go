package rolebots

// Corporation bulletins: the Corp mail tab of a bot-run corporation. Posts go to the corporation's own
// mailbox (evemail.channelID = corpID), which the client files under the Corp tab and every member reads
// from one row (server/lsc/mailboxes.go keeps read state apart).
// A bulletin is for news that matters: every CEO event is queued as a line in rolebot_corp_news, but only a
// major line (directive, war, diplomacy, address, an officer appointed, a member joining, or anything about
// a capsuleer, aboutCharID) sends one, at most once per newsMinSec. Routine lines wait for the weekly digest,
// which is the corporation's standing state plus the routine lines summed up. /corp/bulletins lists all.
// Only corporations with a human member queue or post anything: a bot-only corporation writes zero rows. The
// test is cached (corpHasHumans) because events are queued from hot CEO paths.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Bulletin line kinds, in the order a bulletin prints them.
const (
	newsDirective = "directive"
	newsOfficers  = "officers"
	newsRanks     = "ranks"
	newsMembers   = "members"
	newsFleet     = "fleet"
	newsSeats     = "seats"
	newsWar       = "war"
	newsRelations = "relations"
	newsMoney     = "money"
	newsEstate    = "estate"
	newsOps       = "ops"
)

var newsKindOrder = []string{newsDirective, newsWar, newsRelations, newsEstate, newsOfficers, newsRanks,
	newsFleet, newsSeats, newsOps, newsMoney, newsMembers}

var newsKindHeading = map[string]string{
	newsDirective: "From the chair",
	newsWar:       "War",
	newsRelations: "Diplomacy",
	newsEstate:    "Our address",
	newsOfficers:  "Officers",
	newsRanks:     "Promotions",
	newsFleet:     "Fleet orders",
	newsSeats:     "Fleet commands",
	newsOps:       "Operations",
	newsMoney:     "Money",
	newsMembers:   "Members",
}

// newsRoutineKinds wait for the weekly digest, which sums them up. Every
// other kind is news: it makes a bulletin go out.
var newsRoutineKinds = []string{newsFleet, newsSeats, newsOps, newsMoney, newsRanks}

// newsCapsuleersHeading leads a letter with the lines about its readers.
const newsCapsuleersHeading = "Our capsuleers"

// newsMajor is whether a line is worth a bulletin of its own: a line about a
// capsuleer always is, a routine kind never is otherwise.
func newsMajor(kind string, about int64) bool {
	if about != 0 {
		return true
	}
	for _, k := range newsRoutineKinds {
		if k == kind {
			return false
		}
	}
	return true
}

// newsRoutineSQL is the routine kinds as a SQL list.
func newsRoutineSQL() string {
	q := make([]string, len(newsRoutineKinds))
	for i, k := range newsRoutineKinds {
		q[i] = "'" + k + "'"
	}
	return strings.Join(q, ",")
}

const (
	newsDigestEverySec = 7 * 86400
	newsRetentionSec   = 30 * 86400
	corpHumansTTLSec   = 300
)

// corpHasHumans caches, per corporation, whether any member is a person.
var (
	corpHumansMu sync.Mutex
	corpHumans   = map[int64]struct {
		yes bool
		at  int64
	}{}
)

func (m *Mod) corpHasHumans(ctx context.Context, corpID int64) bool {
	now := clock.Unix()
	corpHumansMu.Lock()
	e, ok := corpHumans[corpID]
	corpHumansMu.Unlock()
	if ok && now-e.at < corpHumansTTLSec {
		return e.yes
	}
	ids, err := int64Column(ctx, m, `
		SELECT ch.characterID FROM character_ ch
		  LEFT JOIN rolebot_characters b ON b.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0 AND b.characterID IS NULL`, corpID)
	if err != nil {
		return ok && e.yes
	}
	yes := false
	for _, id := range ids {
		if !m.isSynthetic(id) {
			yes = true
			break
		}
	}
	corpHumansMu.Lock()
	corpHumans[corpID] = struct {
		yes bool
		at  int64
	}{yes, now}
	corpHumansMu.Unlock()
	return yes
}

// isCapsuleer is whether an ID is a person's character: an account character no rolebot drives. Any other
// ID (a system, a bot, nothing) is false.
func (m *Mod) isCapsuleer(ctx context.Context, id int64) bool {
	if id <= 0 || m.isSynthetic(id) {
		return false
	}
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM character_ ch
		  LEFT JOIN rolebot_characters b ON b.characterID = ch.characterID
		 WHERE ch.characterID = ? AND ch.accountID > 0 AND b.characterID IS NULL`, id).Scan(&n)
	return n > 0
}

// queueCorpNews files one bulletin line for a bot-run corporation that has a
// human in it. Cheap to call from anywhere; never fails the caller. about,
// when given, is the capsuleer the line is about: it leads the letter and
// makes even a routine line news.
func (m *Mod) queueCorpNews(ctx context.Context, corpID int64, kind, text string, about ...int64) {
	if !corpNewsOn() || corpID == 0 || text == "" {
		return
	}
	if _, managed := managedCEO(corpID); !managed {
		return
	}
	if !m.corpHasHumans(ctx, corpID) {
		return
	}
	var who int64
	if len(about) > 0 {
		who = about[0]
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_news (corporationID, kind, at, line, aboutCharID) VALUES (?, ?, ?, ?, ?)`,
		corpID, kind, clock.Unix(), truncate(text, 255), who); err != nil {
		rlogf("rolebots: queue corp news %d: %v", corpID, err)
	}
}

// postCorpNews writes one letter into the corporation's mailbox and rings
// every online member's Corp tab.
func (m *Mod) postCorpNews(ctx context.Context, corpID, senderID int64, subject, body string) int64 {
	subject = mailSubject(subject)
	created := win32From(clock.Now())
	msgID, err := evedb.SendEveMail(ctx, m.db, corpID, senderID, subject, body, created)
	if err != nil {
		rlogf("rolebots: corp bulletin %d: %v", corpID, err)
		return 0
	}
	if h := modapi.Corp(); h.CorpMailPush != nil && msgID != 0 {
		h.CorpMailPush(corpID, msgID, senderID, subject, created)
	}
	return msgID
}

// newsLine is one queued line.
type newsLine struct {
	ID    int64
	Kind  string
	At    int64
	Text  string
	About int64
}

// bulletinBody is the pure half: the lines about capsuleers first, under
// their own heading, then the rest grouped under their kind's heading, in
// newsKindOrder, oldest first within a group. Every line ends <br>
// (gangreport.go's line/heading); a kind with no lines prints nothing.
func bulletinBody(lines []newsLine) string {
	by := map[string][]newsLine{}
	var people []newsLine
	for _, l := range lines {
		if l.About != 0 {
			people = append(people, l)
			continue
		}
		by[l.Kind] = append(by[l.Kind], l)
	}
	var b strings.Builder
	if len(people) > 0 {
		sort.SliceStable(people, func(i, j int) bool { return people[i].At < people[j].At })
		heading(&b, newsCapsuleersHeading)
		for _, l := range people {
			line(&b, "%s", l.Text)
		}
	}
	printed := map[string]bool{}
	emit := func(kind string) {
		ls := by[kind]
		if len(ls) == 0 || printed[kind] {
			return
		}
		printed[kind] = true
		sort.SliceStable(ls, func(i, j int) bool { return ls[i].At < ls[j].At })
		title := newsKindHeading[kind]
		if title == "" {
			title = "Notices"
		}
		heading(&b, title)
		for _, l := range ls {
			line(&b, "%s", l.Text)
		}
	}
	for _, k := range newsKindOrder {
		emit(k)
	}
	var rest []string
	for k := range by {
		if !printed[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		emit(k)
	}
	return b.String()
}

// newsDesk is the bulletin's turn on the staff desk sweep: post what has
// queued, and the weekly digest when it is due.
func (m *Mod) newsDesk(ctx context.Context, c *corpCEO) {
	if !corpNewsOn() || !m.corpHasHumans(ctx, c.CorpID) {
		return
	}
	var lastPost, lastDigest int64
	err := m.db.QueryRowContext(ctx,
		`SELECT lastPostAt, lastDigestAt FROM rolebot_corp_news_state WHERE corporationID = ?`,
		c.CorpID).Scan(&lastPost, &lastDigest)
	now := clock.Unix()
	if errors.Is(err, sql.ErrNoRows) {
		// First sight of this corporation: its week starts now, so switching on does not send an empty weekly report.
		m.saveNewsState(ctx, c.CorpID, now, now)
		return
	}
	if err != nil {
		return
	}

	if now-lastDigest >= newsDigestEverySec {
		if err := m.postCorpDigest(ctx, c); err != nil {
			m.logBot(c.CharID, logError, "ceo", "corp digest: %v", err)
			return
		}
		m.saveNewsState(ctx, c.CorpID, now, now)
		return
	}
	if now-lastPost < newsMinSec() {
		return
	}
	// Only news makes a bulletin; the routine lines stay queued for the digest.
	lines, err := m.pendingNews(ctx, c.CorpID)
	if err != nil || len(lines) == 0 {
		return
	}
	corpName := m.corpNameOf(ctx, c.CorpID)
	body := bulletinBody(lines) + m.signature(ctx, c.CharID, "Chief Executive Officer", corpName) +
		"<br><br>" + igbLink("/corp/bulletins", "Corporation bulletins, in full")
	if m.postCorpNews(ctx, c.CorpID, c.CharID, corpName+" - Bulletin", body) == 0 {
		return
	}
	m.markNewsPosted(ctx, lines)
	m.saveNewsState(ctx, c.CorpID, now, lastDigest)
}

// pendingNews is the unposted news (newsMajor): every line about a capsuleer and every line of a kind that
// is not routine. Routine lines are counted by routineNewsSummary, since a week of them would push the news
// past any LIMIT.
func (m *Mod) pendingNews(ctx context.Context, corpID int64) ([]newsLine, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT newsID, kind, at, line, aboutCharID FROM rolebot_corp_news
		 WHERE corporationID = ? AND posted = 0
		   AND (aboutCharID <> 0 OR kind NOT IN (`+newsRoutineSQL()+`))
		 ORDER BY at LIMIT 200`, corpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []newsLine
	for rows.Next() {
		var l newsLine
		if err := rows.Scan(&l.ID, &l.Kind, &l.At, &l.Text, &l.About); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// routineCount is one routine kind's unposted lines: how many, and how many
// of them were fleets standing down (the ops kind's bulk).
type routineCount struct {
	N, StoodDown int
}

// routineNewsSummary reads the routine lines waiting for the digest: a count
// per kind, and the money lines themselves (a payroll or a dividend is worth
// reading in full; there are a few a week).
func (m *Mod) routineNewsSummary(ctx context.Context, corpID int64) (map[string]routineCount, []string, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT kind, COUNT(*), COALESCE(SUM(line LIKE 'Fleet % stood down%'), 0)
		  FROM rolebot_corp_news
		 WHERE corporationID = ? AND posted = 0 AND aboutCharID = 0 AND kind IN (`+newsRoutineSQL()+`)
		 GROUP BY kind`, corpID)
	if err != nil {
		return nil, nil, err
	}
	counts := map[string]routineCount{}
	for rows.Next() {
		var k string
		var rc routineCount
		if err := rows.Scan(&k, &rc.N, &rc.StoodDown); err != nil {
			rows.Close()
			return nil, nil, err
		}
		counts[k] = rc
	}
	rows.Close()
	rows, err = m.db.QueryContext(ctx, `
		SELECT line FROM rolebot_corp_news
		 WHERE corporationID = ? AND posted = 0 AND aboutCharID = 0 AND kind = ?
		 ORDER BY at DESC LIMIT 4`, corpID, newsMoney)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var money []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, nil, err
		}
		money = append(money, s)
	}
	return counts, money, rows.Err()
}

// routineSummaryLines says the routine week in a few sentences. Pure.
func routineSummaryLines(counts map[string]routineCount, money []string) []string {
	var out []string
	if n := counts[newsFleet].N; n > 0 {
		out = append(out, fmt.Sprintf("Our departments posted %d fleet order(s); the standing ones are listed above.", n))
	}
	if n := counts[newsSeats].N; n > 0 {
		out = append(out, fmt.Sprintf("%d change(s) of fleet command.", n))
	}
	if rc := counts[newsOps]; rc.N > 0 {
		s := ""
		if rc.StoodDown > 0 {
			s = fmt.Sprintf("%d fleet(s) went out and stood down", rc.StoodDown)
		}
		if other := rc.N - rc.StoodDown; other > 0 {
			if s != "" {
				s += "; "
			}
			s += fmt.Sprintf("%d other operations notice(s)", other)
		}
		out = append(out, s+".")
	}
	if n := counts[newsRanks].N; n > 0 {
		out = append(out, fmt.Sprintf("%d promotion(s) and reassignment(s) in the ranks.", n))
	}
	for i := len(money) - 1; i >= 0; i-- {
		out = append(out, money[i])
	}
	if n := counts[newsMoney].N - len(money); n > 0 {
		out = append(out, fmt.Sprintf("...and %d more money notice(s).", n))
	}
	return out
}

func (m *Mod) markNewsPosted(ctx context.Context, lines []newsLine) {
	if len(lines) == 0 {
		return
	}
	ids := make([]string, len(lines))
	for i, l := range lines {
		ids[i] = fmt.Sprint(l.ID)
	}
	_, _ = m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_news SET posted = 1 WHERE newsID IN (`+strings.Join(ids, ",")+`)`)
}

func (m *Mod) saveNewsState(ctx context.Context, corpID, postAt, digestAt int64) {
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_news_state (corporationID, lastPostAt, lastDigestAt) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE lastPostAt = VALUES(lastPostAt), lastDigestAt = VALUES(lastDigestAt)`,
		corpID, postAt, digestAt)
	_, _ = m.db.ExecContext(ctx,
		`DELETE FROM rolebot_corp_news WHERE corporationID = ? AND posted = 1 AND at < ?`,
		corpID, clock.Unix()-newsRetentionSec)
}

// corpSnapshot is the standing state of a corporation in words, which the digest and the /corp/bulletins
// page both print.
type corpSnapshot struct {
	Name         string
	CEOName      string
	Tags         []string
	Directive    string
	DirectiveWhy string
	Officers     [][2]string // (post, name)
	FleetOrders  []string
	Income, Cost float64
	TopWorkers   [][2]string // (name, points this week)
	Capsuleers   []string    // one line per human member: their week
	OpenWork     int
	Recent       []newsLine
}

// capsuleerWeek is one human member's week, in one line. Pure.
func capsuleerWeek(name, rank string, merit float64, jobs int) string {
	s := name
	if rank != "" {
		s += " (" + rank + ")"
	}
	parts := []string{}
	if merit > 0 {
		parts = append(parts, fmt.Sprintf("%.0f merit earned", merit))
	}
	if jobs > 0 {
		parts = append(parts, fmt.Sprintf("%d assignment(s) done", jobs))
	}
	if len(parts) == 0 {
		return s + ": a quiet week."
	}
	return s + ": " + strings.Join(parts, ", ") + "."
}

// capsuleerWeeks reads every human member's week for the digest. Best effort.
func (m *Mod) capsuleerWeeks(ctx context.Context, corpID int64, since int64) []string {
	ids, err := int64Column(ctx, m, `
		SELECT ch.characterID FROM character_ ch
		  LEFT JOIN rolebot_characters b ON b.characterID = ch.characterID
		 WHERE ch.corporationID = ? AND ch.accountID > 0 AND b.characterID IS NULL
		 ORDER BY ch.characterID`, corpID)
	if err != nil {
		return nil
	}
	var out []string
	for _, id := range ids {
		if m.isSynthetic(id) || ctx.Err() != nil {
			continue
		}
		var merit float64
		var jobs, rank int
		haveRank := m.db.QueryRowContext(ctx, `
			SELECT rankNo FROM rolebot_corp_merit WHERE corporationID = ? AND characterID = ?`,
			corpID, id).Scan(&rank) == nil
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(points), 0) FROM rolebot_corp_merit_log
			 WHERE corporationID = ? AND characterID = ? AND at >= ? AND points > 0`,
			corpID, id, since).Scan(&merit)
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_corp_assignments
			 WHERE corporationID = ? AND characterID = ? AND status = ? AND closedAt >= ?`,
			corpID, id, asDone, since).Scan(&jobs)
		rankName := ""
		if haveRank && meritOn() {
			rankName = meritRankName(clampRank(rank))
		}
		out = append(out, capsuleerWeek(m.charNameOr(ctx, id), rankName, merit, jobs))
	}
	return out
}

// corpSnapshotFor reads a corporation's standing state. Best effort: every
// part that fails to read is left empty rather than failing the whole.
func (m *Mod) corpSnapshotFor(ctx context.Context, corpID, ceoID int64) corpSnapshot {
	s := corpSnapshot{Name: m.corpNameOf(ctx, corpID), CEOName: m.charNameOr(ctx, ceoID)}
	if f, err := getAnyCorpFacts(ctx, m.db, corpID); err == nil && f != nil {
		s.Tags = f.Policy.Tags
	}
	if d, _, err := m.loadCorpDirective(ctx, corpID); err == nil && len(d) > 0 {
		s.Directive = directiveText(d)
		s.DirectiveWhy = m.corpDirectiveReason(ctx, corpID)
	}
	if st, err := m.loadCorpStaff(ctx, corpID, ceoID); err == nil && st != nil {
		if st.Treasurer != nil {
			s.Officers = append(s.Officers, [2]string{"Treasurer", st.Treasurer.Name})
		}
		for _, p := range st.Personnel {
			s.Officers = append(s.Officers, [2]string{"Recruitment Officer", p.Name})
		}
		for _, d := range st.Directors {
			post := "Director"
			if d.Portfolio != "" {
				post = portfolioTitle(d.Portfolio)
			}
			s.Officers = append(s.Officers, [2]string{post, d.Name})
		}
	}
	now := clock.Unix()
	if rows, err := m.db.QueryContext(ctx, `
		SELECT activity, IF(systemID <> 0, systemID, areaID), oreNames, reason FROM rolebot_corp_fleet_orders
		 WHERE corporationID = ? AND expiresAt > ? ORDER BY activity`, corpID, now); err == nil {
		for rows.Next() {
			var act, ores, why string
			var sys int64
			if rows.Scan(&act, &sys, &ores, &why) != nil {
				continue
			}
			s.FleetOrders = append(s.FleetOrders, fleetOrderText(act, m.placeName(ctx, sys), ores, why))
		}
		rows.Close()
	}
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN amount > 0 THEN amount ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN amount < 0 THEN -amount ELSE 0 END), 0)
		  FROM rolebot_corp_ledger WHERE corporationID = ? AND at >= ?`,
		corpID, now-newsDigestEverySec).Scan(&s.Income, &s.Cost)
	if rows, err := m.db.QueryContext(ctx, `
		SELECT l.characterID, SUM(l.points) AS p
		  FROM rolebot_corp_merit_log l
		 WHERE l.corporationID = ? AND l.at >= ? AND l.points > 0
		 GROUP BY l.characterID ORDER BY p DESC LIMIT 5`, corpID, now-newsDigestEverySec); err == nil {
		type tw struct {
			id int64
			p  float64
		}
		var top []tw
		for rows.Next() {
			var t tw
			if rows.Scan(&t.id, &t.p) == nil {
				top = append(top, t)
			}
		}
		rows.Close()
		for _, t := range top {
			s.TopWorkers = append(s.TopWorkers, [2]string{m.charNameOr(ctx, t.id), fmt.Sprintf("%.0f", t.p)})
		}
	}
	s.Capsuleers = m.capsuleerWeeks(ctx, corpID, now-newsDigestEverySec)
	if work, err := m.openWorkFor(ctx, corpID); err == nil {
		s.OpenWork = len(work)
	}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT newsID, kind, at, line FROM rolebot_corp_news
		 WHERE corporationID = ? ORDER BY at DESC LIMIT 15`, corpID); err == nil {
		for rows.Next() {
			var l newsLine
			if rows.Scan(&l.ID, &l.Kind, &l.At, &l.Text) == nil {
				s.Recent = append(s.Recent, l)
			}
		}
		rows.Close()
	}
	return s
}

// fleetOrderText says a department's standing order the way a person would.
func fleetOrderText(activity, systemName, ores, why string) string {
	label := activity
	if def, ok := activityDefFor(activity); ok {
		label = def.Label
	}
	var b strings.Builder
	b.WriteString(label)
	if systemName != "" {
		b.WriteString(" in " + systemName)
	}
	if ores != "" {
		b.WriteString(" — " + strings.ReplaceAll(ores, ",", ", "))
	}
	if why != "" {
		b.WriteString(" (" + why + ")")
	}
	return b.String()
}

// digestBody is the pure half of the weekly letter: the capsuleers' week
// first, then the standing state, the week's news not yet posted, and the
// routine lines summed up.
func digestBody(s corpSnapshot, pending []newsLine, routine []string) string {
	var b strings.Builder
	line(&b, "The week at %s.", s.Name)
	if len(s.Capsuleers) > 0 {
		heading(&b, newsCapsuleersHeading)
		for _, c := range s.Capsuleers {
			line(&b, "%s", c)
		}
	}
	heading(&b, "From the chair")
	if s.Directive != "" {
		line(&b, "Where I want our effort: %s.", s.Directive)
		if s.DirectiveWhy != "" {
			line(&b, "Why: %s.", s.DirectiveWhy)
		}
	} else {
		line(&b, "No standing directive — work where you do best.")
	}
	if len(s.Officers) > 0 {
		heading(&b, "Officers")
		for _, o := range s.Officers {
			line(&b, "%s: %s", o[0], o[1])
		}
	}
	if len(s.FleetOrders) > 0 {
		heading(&b, "Fleet orders")
		for _, o := range s.FleetOrders {
			line(&b, "%s", o)
		}
	}
	heading(&b, "The books, last seven days")
	line(&b, "Came in: %s. Went out: %s.", iskString(s.Income), iskString(s.Cost))
	if len(s.TopWorkers) > 0 {
		heading(&b, "Who carried us")
		for _, t := range s.TopWorkers {
			line(&b, "%s — %s merit", t[0], t[1])
		}
	}
	if s.OpenWork > 0 {
		heading(&b, "Open work")
		line(&b, "%d order(s) on the work board, credited by the run and paid in the weekly payroll.", s.OpenWork)
	}
	if len(pending) > 0 {
		b.WriteString(bulletinBody(pending))
	}
	if len(routine) > 0 {
		heading(&b, "Around the corporation")
		for _, r := range routine {
			line(&b, "%s", r)
		}
	}
	return b.String()
}

// postCorpDigest writes the weekly letter, folding in anything queued, and retires every line it covered,
// the routine ones included, which it summed rather than listed.
func (m *Mod) postCorpDigest(ctx context.Context, c *corpCEO) error {
	cut := clock.Unix()
	s := m.corpSnapshotFor(ctx, c.CorpID, c.CharID)
	pending, err := m.pendingNews(ctx, c.CorpID)
	if err != nil {
		return err
	}
	counts, money, err := m.routineNewsSummary(ctx, c.CorpID)
	if err != nil {
		return err
	}
	body := digestBody(s, pending, routineSummaryLines(counts, money)) +
		"<br>" + igbLink("/corp/bulletins", "Corporation bulletins") + " &middot; " +
		igbLink("/corp/assignments", "Assignments") + " &middot; " +
		igbLink("/corp/industry", "Industry") +
		m.signature(ctx, c.CharID, "Chief Executive Officer", s.Name)
	if m.postCorpNews(ctx, c.CorpID, c.CharID, s.Name+" - Weekly report", body) == 0 {
		return fmt.Errorf("digest not written")
	}
	m.markNewsPosted(ctx, pending)
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_news SET posted = 1 WHERE corporationID = ? AND posted = 0 AND at <= ?`,
		c.CorpID, cut)
	return nil
}

// migrateCorpNewsAboutColumn adds rolebot_corp_news.aboutCharID. Lines queued
// before it name nobody, which is what they always did.
func migrateCorpNewsAboutColumn(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_corp_news'
		  AND COLUMN_NAME = 'aboutCharID'`).Scan(&have); err != nil {
		return fmt.Errorf("migrateCorpNewsAboutColumn probe: %w", err)
	}
	if have > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE rolebot_corp_news ADD COLUMN aboutCharID BIGINT NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("migrateCorpNewsAboutColumn: %w", err)
	}
	return nil
}

func init() {
	registerCorpForget("corpHumans", func(id int64) {
		corpHumansMu.Lock()
		delete(corpHumans, id)
		corpHumansMu.Unlock()
	})
}
