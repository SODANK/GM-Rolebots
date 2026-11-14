package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// Corporation posts: who holds which seat in a bot-run corporation. Three dealers hand out seats: the officers
// (corpstaff.go), the Industry department's lower seats (corpindseats.go) and the starbase seats (posseats.go).
// They share one table keyed (corporation, character, post).
// A pilot may hold one post in each family at once, and every write is scoped to the family the dealer owns:
// savePosts(famIndustry, ...) never touches the same pilot's Treasurer row. Within a family one post per pilot
// is the dealer's invariant; familyPostsOf keeps the first and says so if it sees two.
// The officers' family differs in kind: who is an officer is the role bits on chrCorporationRoles, and the row
// carries only what the module adds (a director's portfolio, the performance review), so corpStaff is built
// from the members list (loadCorpStaff). postIndex is the read side for per-pilot paths, rebuilt at boot, at
// every CEO sweep and after every savePosts, so a seat dealt mid-think is visible the same pass.

const (
	famOfficer  = "officer"
	famIndustry = "industry"
	famStarbase = "starbase"
	// famDiplomacy holds the one transient seat: an envoy carrying a letter
	// (corpenvoy.go). Dealt per letter, handed back on return.
	famDiplomacy = "diplomacy"
	seatEnvoy    = "envoy"
)

// postFamilies is which posts each dealer owns. A post name appears in
// exactly one family.
var postFamilies = map[string][]string{
	famOfficer:   {postTreasurer, postPersonnel, postDirector},
	famIndustry:  {seatAdmin, seatSales},
	famStarbase:  {seatFueler, seatProcurer},
	famDiplomacy: {seatEnvoy},
}

// corpPost is one row of rolebot_corp_posts.
type corpPost struct {
	CorpID, CharID  int64
	Post, Portfolio string
	Since           int64   // unix the holder took the post
	Perf            float64 // performance, 1 = par (reviewStaff / reviewIndustrySeats)
	ReviewAt        int64   // unix of the last review
	Metric          float64 // what the last review measured
	Credit          float64 // Sales Agents only: ISK sold since the last review
}

const corpPostCols = `corporationID, characterID, post, portfolio, since, perf, reviewAt, metric, credit`

// ensurePostTables is called from ensureTables. A schema that still has the three dealers' own tables is
// carried here once and they are dropped.
func ensurePostTables(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rolebot_corp_posts (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			post          VARCHAR(16) NOT NULL,
			portfolio     VARCHAR(16) NOT NULL DEFAULT '',
			since         BIGINT NOT NULL DEFAULT 0,
			perf          DOUBLE NOT NULL DEFAULT 1,
			reviewAt      BIGINT NOT NULL DEFAULT 0,
			metric        DOUBLE NOT NULL DEFAULT 0,
			credit        DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID, post),
			KEY idx_rbcpost_char (characterID),
			KEY idx_rbcpost_corp_post (corporationID, post)
		)`); err != nil {
		return fmt.Errorf("ensurePostTables: %w", err)
	}
	return migrateCorpPosts(ctx, db)
}

// migrateCorpPosts carries the three predecessor tables into
// rolebot_corp_posts and drops them. Probe-guarded the way the
// rolebot_fit_assignments carry is (tables.go): a fresh install has none of
// them and nothing runs; the boot after the carry has none either.
func migrateCorpPosts(ctx context.Context, db *sql.DB) error {
	carries := []struct{ table, sel string }{
		{"rolebot_corp_staff",
			`SELECT corporationID, characterID, post, portfolio, since, perf, perfAt, metric, 0
			   FROM rolebot_corp_staff WHERE post <> ''`},
		{"rolebot_corp_ind_seats",
			`SELECT corporationID, characterID, seat, '', since, perf, reviewAt, metric, credit
			   FROM rolebot_corp_ind_seats`},
		{"rolebot_corp_pos_seats",
			`SELECT corporationID, characterID, seat, '', since, perf, reviewAt, metric, 0
			   FROM rolebot_corp_pos_seats`},
	}
	for _, c := range carries {
		ok, err := haveTable(ctx, db, c.table)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if _, err := db.ExecContext(ctx, `INSERT IGNORE INTO rolebot_corp_posts (`+corpPostCols+`) `+c.sel); err != nil {
			return fmt.Errorf("migrateCorpPosts carry %s: %w", c.table, err)
		}
		if _, err := db.ExecContext(ctx, `DROP TABLE `+c.table); err != nil {
			return fmt.Errorf("migrateCorpPosts drop %s: %w", c.table, err)
		}
	}
	return nil
}

// familyIn is the SQL fragment restricting a statement to one family's
// posts, with its arguments.
func familyIn(fam string) (string, []any) {
	posts := postFamilies[fam]
	if len(posts) == 0 {
		panic("rolebots: unknown post family " + fam)
	}
	args := make([]any, len(posts))
	for i, p := range posts {
		args[i] = p
	}
	return "post IN (" + strings.TrimSuffix(strings.Repeat("?,", len(posts)), ",") + ")", args
}

func familyOf(post string) string {
	for fam, posts := range postFamilies {
		for _, p := range posts {
			if p == post {
				return fam
			}
		}
	}
	return ""
}

func scanPosts(rows *sql.Rows) ([]corpPost, error) {
	var out []corpPost
	for rows.Next() {
		var p corpPost
		if err := rows.Scan(&p.CorpID, &p.CharID, &p.Post, &p.Portfolio, &p.Since, &p.Perf, &p.ReviewAt, &p.Metric, &p.Credit); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// corpPostsOf is every post held in a corporation, oldest holder first.
func (m *Mod) corpPostsOf(ctx context.Context, corpID int64) ([]corpPost, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+corpPostCols+` FROM rolebot_corp_posts
		WHERE corporationID = ? ORDER BY since, characterID, post`, corpID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPosts(rows)
}

// familyPostsOf is one family's posts in a corporation, by holder. A pilot
// holding two posts of one family is a dealer bug; the first by tenure wins
// and the duplicate is logged.
func (m *Mod) familyPostsOf(ctx context.Context, corpID int64, fam string) map[int64]corpPost {
	in, args := familyIn(fam)
	out := map[int64]corpPost{}
	rows, err := m.db.QueryContext(ctx, `SELECT `+corpPostCols+` FROM rolebot_corp_posts
		WHERE corporationID = ? AND `+in+` ORDER BY since, post`, append([]any{corpID}, args...)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	posts, err := scanPosts(rows)
	if err != nil {
		return out
	}
	for _, p := range posts {
		if prev, dup := out[p.CharID]; dup {
			rlogf("rolebots: corp %d pilot %d holds both %s and %s in the %s family — keeping %s",
				corpID, p.CharID, prev.Post, p.Post, fam, prev.Post)
			continue
		}
		out[p.CharID] = p
	}
	return out
}

// postHolders lists one post's holders in a corporation, oldest first.
func (m *Mod) postHolders(ctx context.Context, corpID int64, post string) []int64 {
	ids, _ := int64Column(ctx, m, `
		SELECT characterID FROM rolebot_corp_posts
		 WHERE corporationID = ? AND post = ? ORDER BY since, characterID`, corpID, post)
	return ids
}

// savePosts makes one family's posts in a corporation exactly `want`
// (by holder): rows in another family are untouched, a holder whose post
// changed within the family loses the old row, a holder absent from want
// loses its row, and nil empties the family. The index is rebuilt after.
func (m *Mod) savePosts(ctx context.Context, corpID int64, fam string, want map[int64]corpPost) error {
	posts := postFamilies[fam]
	if len(posts) == 0 {
		return fmt.Errorf("savePosts: unknown family %q", fam)
	}
	for id, p := range want {
		if familyOf(p.Post) != fam {
			return fmt.Errorf("savePosts: %s is not a %s post (pilot %d)", p.Post, fam, id)
		}
	}
	in, args := familyIn(fam)
	rows, err := m.db.QueryContext(ctx, `SELECT characterID, post FROM rolebot_corp_posts
		WHERE corporationID = ? AND `+in, append([]any{corpID}, args...)...)
	if err != nil {
		return fmt.Errorf("savePosts read: %w", err)
	}
	type cur struct {
		id   int64
		post string
	}
	var have []cur
	for rows.Next() {
		var c cur
		if rows.Scan(&c.id, &c.post) == nil {
			have = append(have, c)
		}
	}
	rows.Close()
	for _, c := range have {
		if w, ok := want[c.id]; ok && w.Post == c.post {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_posts
			WHERE corporationID = ? AND characterID = ? AND post = ?`, corpID, c.id, c.post); err != nil {
			return fmt.Errorf("savePosts delete: %w", err)
		}
	}
	for id, p := range want {
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_posts (`+corpPostCols+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE portfolio = VALUES(portfolio), since = VALUES(since),
				perf = VALUES(perf), reviewAt = VALUES(reviewAt), metric = VALUES(metric), credit = VALUES(credit)`,
			corpID, id, p.Post, p.Portfolio, p.Since, p.Perf, p.ReviewAt, p.Metric, p.Credit); err != nil {
			return fmt.Errorf("savePosts upsert: %w", err)
		}
	}
	m.refreshPostIndex(ctx)
	return nil
}

// ── the index ───────────────────────────────────────────────────────────────

// postIndex is charID → every post the pilot holds, rebuilt whole. It is the
// answer for the per-pilot paths (posSeatOf, salesAgentCorpOf); a dealer
// reads the table so that it sees its own writes within one think.
var postIndex atomic.Pointer[map[int64][]corpPost]

func (m *Mod) refreshPostIndex(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+corpPostCols+` FROM rolebot_corp_posts ORDER BY since, characterID, post`)
	if err != nil {
		rlogf("rolebots: post index: %v", err)
		return
	}
	defer rows.Close()
	posts, err := scanPosts(rows)
	if err != nil {
		rlogf("rolebots: post index: %v", err)
		return
	}
	idx := make(map[int64][]corpPost, len(posts))
	for _, p := range posts {
		idx[p.CharID] = append(idx[p.CharID], p)
	}
	postIndex.Store(&idx)
}

// postsOf is every post a pilot holds, in any corporation.
func postsOf(charID int64) []corpPost {
	p := postIndex.Load()
	if p == nil {
		return nil
	}
	return (*p)[charID]
}

// hasPost reports whether a pilot holds one post in one corporation.
func hasPost(charID, corpID int64, post string) bool {
	for _, p := range postsOf(charID) {
		if p.CorpID == corpID && p.Post == post {
			return true
		}
	}
	return false
}

// postIn is the post of one family a pilot holds in a corporation, "" for
// none.
func postIn(charID, corpID int64, fam string) string {
	for _, p := range postsOf(charID) {
		if p.CorpID == corpID && familyOf(p.Post) == fam {
			return p.Post
		}
	}
	return ""
}

// salesAgentCorpOf is the corporation a pilot is a Sales Agent of, which the skill planner trains Trade and
// Retail against (corpceo_skills.go).
func salesAgentCorpOf(charID int64) (int64, bool) {
	for _, p := range postsOf(charID) {
		if p.Post == seatSales {
			return p.CorpID, true
		}
	}
	return 0, false
}

// ── the bench ───────────────────────────────────────────────────────────────

// benchGraceSec is how long an expired bench row is kept so the panel and
// the merit pass can still say "was benched" before it is pruned.
const benchGraceSec = 7 * 86400

// pruneCorpBench drops bench rows (corpwork.go strikeOffPlan) that expired
// more than a week ago. Every reader compares untilAt against now, so an
// expired row already counts for nothing; it only ever accumulated.
func (m *Mod) pruneCorpBench(ctx context.Context) {
	if _, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_bench WHERE untilAt < ?`, clock.Unix()-benchGraceSec); err != nil {
		rlogf("rolebots: prune bench: %v", err)
	}
}

// sortedPostNames is every post name, for tests and the panel.
func sortedPostNames() []string {
	var out []string
	for _, posts := range postFamilies {
		out = append(out, posts...)
	}
	sort.Strings(out)
	return out
}
