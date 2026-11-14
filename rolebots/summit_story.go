package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"strings"

	evedb "gurimoon/db"
)

// Summit stories: one story, one thread (summit.go).
//
// A story's key names its subject. Every beat about that subject lands in
// the same thread for as long as the story lives: a war's declaration, its
// kills, its tallies and its ending are one thread. The ledger row is
// written when the opening post is made. A thread a moderator locks or
// deletes closes its story, and a closed story never posts again.

const (
	summitStoryOpen   = "open"
	summitStoryClosed = "closed"
)

type summitStory struct {
	Key        string
	ThreadID   int64
	Category   string
	SideA      int64
	SideB      int64
	OpenedAt   int64
	LastPostAt int64
	Posts      int
	State      string
}

func (m *Mod) summitStoryGet(ctx context.Context, key string) (*summitStory, error) {
	var s summitStory
	err := m.db.QueryRowContext(ctx, `
		SELECT storyKey, threadID, category, sideA, sideB, openedAt, lastPostAt, posts, state
		  FROM rolebot_summit_stories WHERE storyKey = ?`, key).
		Scan(&s.Key, &s.ThreadID, &s.Category, &s.SideA, &s.SideB, &s.OpenedAt, &s.LastPostAt, &s.Posts, &s.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// summitCloseStory closes a story and drops its waiting beats.
func (m *Mod) summitCloseStory(ctx context.Context, key string) {
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_summit_stories SET state = ? WHERE storyKey = ?`, summitStoryClosed, key)
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_summit_beats SET dropped = 1, dropWhy = 'closed'
		 WHERE storyKey = ? AND postedAt = 0 AND dropped = 0`, key)
}

// summitPublish makes the post: a new thread when the story has none, a
// reply otherwise. It records the story, the post and the author's day.
func (m *Mod) summitPublish(ctx context.Context, b summitBeatRow, st *summitStory, author int64, p summitPost) (int64, error) {
	now := clock.Unix()
	var threadID int64
	if st == nil || st.ThreadID == 0 {
		id, err := evedb.CreateSummitThread(ctx, m.db, author, p.Title, p.Body)
		if err != nil {
			m.summitAuthorRefused(ctx, author, err)
			return 0, err
		}
		threadID = id
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_summit_stories
				(storyKey, threadID, category, sideA, sideB, openedAt, lastPostAt, posts, state)
			VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)
			ON DUPLICATE KEY UPDATE threadID = VALUES(threadID), lastPostAt = VALUES(lastPostAt),
				posts = posts + 1, state = VALUES(state)`,
			b.StoryKey, threadID, b.Category, b.SideA, b.SideB, now, now, summitStoryOpen); err != nil {
			rlogf("rolebots: summit story %s: %v", b.StoryKey, err)
		}
	} else {
		threadID = st.ThreadID
		if _, err := evedb.ReplySummitThread(ctx, m.db, threadID, author, p.Body); err != nil {
			if errors.Is(err, evedb.ErrSummitLocked) || errors.Is(err, evedb.ErrSummitNoThread) {
				m.summitCloseStory(ctx, b.StoryKey)
			}
			m.summitAuthorRefused(ctx, author, err)
			return 0, err
		}
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_summit_stories SET lastPostAt = ?, posts = posts + 1 WHERE storyKey = ?`,
			now, b.StoryKey)
	}
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_summit_posts (threadID, characterID, storyKey, kind, category, newThread, at, excerpt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		threadID, author, b.StoryKey, b.Kind, b.Category, st == nil || st.ThreadID == 0, now,
		truncate(firstLine(p.Body), 200))
	_, _ = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_summit_authors (characterID, signature, lastPostAt, posts)
		VALUES (?, '', ?, 1)
		ON DUPLICATE KEY UPDATE lastPostAt = VALUES(lastPostAt), posts = posts + 1`, author, now)
	m.summitTplUsed(ctx, p.Templates, now)
	return threadID, nil
}

// summitAuthorRefused benches an author the Summit has banned.
func (m *Mod) summitAuthorRefused(ctx context.Context, author int64, err error) {
	if errors.Is(err, evedb.ErrSummitBanned) {
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_summit_authors (characterID, signature, banned) VALUES (?, '', 1)
			ON DUPLICATE KEY UPDATE banned = 1`, author)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// summitLastAuthor is who posted last in a thread, a person or a bot.
func (m *Mod) summitLastAuthor(ctx context.Context, threadID int64) int64 {
	if threadID == 0 {
		return 0
	}
	var id int64
	_ = m.db.QueryRowContext(ctx, `SELECT lastPostCharID FROM summitThreads WHERE threadID = ?`, threadID).Scan(&id)
	return id
}

// ── authors ─────────────────────────────────────────────────────────────────

// summitAuthorFor picks who writes a beat: a fixed author (a CEO speaking for its corporation) if
// still a bot in good standing, otherwise the bot member of the speaking corporation who posted
// least recently, never the thread's last poster. An observer beat (SideCorp 0, no author) takes a
// bot from an uninvolved corporation.
func (m *Mod) summitAuthorFor(ctx context.Context, b summitBeatRow, threadID int64) (int64, error) {
	last := m.summitLastAuthor(ctx, threadID)
	if summitBumpCategory(b.Category) {
		last = 0 // an ad's owner bumping its own thread is what ads do
	}
	if b.AuthorID != 0 && b.AuthorID != last && m.summitAuthorOK(ctx, b.AuthorID) {
		return b.AuthorID, nil
	}
	if b.SpeakCorp == 0 && b.AuthorID != 0 {
		return 0, nil // a seller's own post: nobody else writes it
	}
	var cands []int64
	var err error
	if b.SpeakCorp != 0 {
		cands, err = int64Column(ctx, m, `
			SELECT ch.characterID FROM character_ ch
			  JOIN rolebot_characters r ON r.characterID = ch.characterID
			 WHERE ch.corporationID = ?`, b.SpeakCorp)
	} else {
		// An observer: any bot whose corporation is neither side.
		cands, err = int64Column(ctx, m, `
			SELECT ch.characterID FROM character_ ch
			  JOIN rolebot_characters r ON r.characterID = ch.characterID
			 WHERE ch.corporationID NOT IN (?, ?)
			   AND NOT EXISTS (SELECT 1 FROM crpNPCCorporations n WHERE n.corporationID = ch.corporationID)
			 ORDER BY RAND() LIMIT 40`, b.SideA, b.SideB)
	}
	if err != nil {
		return 0, err
	}
	return m.summitLeastRecent(ctx, cands, last), nil
}

// summitBumpCategory is a category whose threads are one owner's ads.
func summitBumpCategory(cat string) bool { return cat == summitCatRecruit || cat == summitCatMarket }

// summitLeastRecent is the candidate that posted least recently, ties at
// random, skipping the thread's last poster, banned authors and anything
// that is not a bot.
func (m *Mod) summitLeastRecent(ctx context.Context, cands []int64, skip int64) int64 {
	rand.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	var best int64
	bestAt := int64(-1)
	for _, id := range cands {
		if id == skip || !m.isSynthetic(id) {
			continue
		}
		var at int64
		var banned int
		_ = m.db.QueryRowContext(ctx,
			`SELECT lastPostAt, banned FROM rolebot_summit_authors WHERE characterID = ?`, id).Scan(&at, &banned)
		if banned != 0 {
			continue
		}
		if bestAt < 0 || at < bestAt {
			best, bestAt = id, at
		}
		if at == 0 {
			break // never posted: as fresh as it gets
		}
	}
	return best
}

// summitAuthorOK reports whether a fixed author may still post: a bot, not
// banned by the moderators.
func (m *Mod) summitAuthorOK(ctx context.Context, charID int64) bool {
	if !m.isSynthetic(charID) {
		return false
	}
	var banned int
	_ = m.db.QueryRowContext(ctx, `SELECT banned FROM rolebot_summit_authors WHERE characterID = ?`, charID).Scan(&banned)
	return banned == 0
}

// summitLoadMarks fills the budget's memory from the post log at boot.
func (m *Mod) summitLoadMarks(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT at, threadID, characterID, category, newThread FROM rolebot_summit_posts
		 WHERE at >= ? ORDER BY at`, clock.Unix()-2*86400)
	if err != nil {
		rlogf("rolebots: summit marks: %v", err)
		return
	}
	defer rows.Close()
	var out []summitPostMark
	for rows.Next() {
		var mk summitPostMark
		var nt int
		if rows.Scan(&mk.At, &mk.ThreadID, &mk.CharID, &mk.Category, &nt) == nil {
			mk.NewThread = nt != 0
			out = append(out, mk)
		}
	}
	resetSummitMarks(out)
}
