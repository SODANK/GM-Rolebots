package rolebots

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"time"
)

// Summit beats: posts waiting to be written (summit.go).
//
// A beat is filed by an event funnel with its facts already resolved to
// text, a heat, and a reaction delay, so a rebuttal arrives hours after the
// declaration rather than in the same minute. sourceKey is unique: one
// incident files one beat however often its funnel fires. A beat that is
// not posted by expiresAt is dropped quietly; the budget lets the hot ones
// through first.

// summitHeatHalfLife is how fast a waiting beat cools.
const summitHeatHalfLife = 12 * time.Hour

// summitBeat is what a funnel files.
type summitBeat struct {
	StoryKey  string
	Category  string
	Kind      string
	Heat      float64
	SourceKey string
	Facts     summitFacts
	SideA     int64 // the story's two parties, for observers and the panel
	SideB     int64
	SpeakCorp int64 // whose member writes it; 0 with no AuthorID = an observer
	AuthorID  int64 // a fixed author (a CEO); 0 = pick from SpeakCorp
	Opens     bool  // this beat is the story's opening post
	DelayMin  time.Duration
	DelayMax  time.Duration
	TTL       time.Duration
}

// summitBeatRow is a filed beat as the desk reads it back.
type summitBeatRow struct {
	ID        int64
	StoryKey  string
	Category  string
	Kind      string
	Heat      float64
	SourceKey string
	Facts     summitFacts
	SideA     int64
	SideB     int64
	SpeakCorp int64
	AuthorID  int64
	Opens     bool
	NotBefore int64
	ExpiresAt int64
	CreatedAt int64
}

// summitEffectiveHeat is a beat's heat cooled by its age.
func summitEffectiveHeat(heat float64, createdAt, now int64) float64 {
	age := float64(now - createdAt)
	if age <= 0 {
		return heat
	}
	return heat * math.Pow(0.5, age/summitHeatHalfLife.Seconds())
}

// summitFile files one beat. Cheap to call from any funnel; never fails the
// caller. A sourceKey already filed is ignored.
func (m *Mod) summitFile(ctx context.Context, b summitBeat) {
	if m.db == nil || !summitCategoryOn(b.Category) || b.StoryKey == "" || b.SourceKey == "" {
		return
	}
	now := clock.Unix()
	delay := b.DelayMin
	if b.DelayMax > b.DelayMin {
		delay += time.Duration(rand.Int63n(int64(b.DelayMax - b.DelayMin)))
	}
	ttl := b.TTL
	if ttl <= 0 {
		ttl = 36 * time.Hour
	}
	notBefore := now + int64(delay/time.Second)
	wctx, cancel := context.WithTimeout(context.Background(), relHookTimeout)
	defer cancel()
	if _, err := m.db.ExecContext(wctx, `
		INSERT IGNORE INTO rolebot_summit_beats
			(storyKey, category, kind, heat, sourceKey, facts, sideA, sideB, speakCorp, authorID,
			 opens, notBefore, expiresAt, createdAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.StoryKey, b.Category, b.Kind, b.Heat, truncate(b.SourceKey, 96), b.Facts.encode(),
		b.SideA, b.SideB, b.SpeakCorp, b.AuthorID, b.Opens, notBefore,
		notBefore+int64(ttl/time.Second), now); err != nil {
		rlogf("rolebots: summit beat %s: %v", b.SourceKey, err)
	}
}

// summitDueBeats is every beat eligible now, hottest first.
func (m *Mod) summitDueBeats(ctx context.Context, now int64, limit int) ([]summitBeatRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT beatID, storyKey, category, kind, heat, sourceKey, facts, sideA, sideB, speakCorp,
		       authorID, opens, notBefore, expiresAt, createdAt
		  FROM rolebot_summit_beats
		 WHERE postedAt = 0 AND dropped = 0 AND notBefore <= ? AND expiresAt > ?
		 ORDER BY heat DESC LIMIT 400`, now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []summitBeatRow
	for rows.Next() {
		var b summitBeatRow
		var facts string
		var opens int
		if err := rows.Scan(&b.ID, &b.StoryKey, &b.Category, &b.Kind, &b.Heat, &b.SourceKey, &facts,
			&b.SideA, &b.SideB, &b.SpeakCorp, &b.AuthorID, &opens, &b.NotBefore, &b.ExpiresAt, &b.CreatedAt); err != nil {
			return nil, err
		}
		b.Facts = decodeSummitFacts(facts)
		b.Opens = opens != 0
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortSummitBeats(out, now)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// sortSummitBeats orders beats hottest first after cooling; an opening beat
// goes ahead of its own story's replies at equal heat.
func sortSummitBeats(bs []summitBeatRow, now int64) {
	sort.SliceStable(bs, func(i, j int) bool {
		hi, hj := summitEffectiveHeat(bs[i].Heat, bs[i].CreatedAt, now), summitEffectiveHeat(bs[j].Heat, bs[j].CreatedAt, now)
		if hi != hj {
			return hi > hj
		}
		return bs[i].Opens && !bs[j].Opens
	})
}

// summitOpenerPending reports whether a story still has an opening beat
// waiting. A reply beat for a story with no thread waits for it; with none
// left, the reply opens the thread itself, so a story never loses its thread
// to a dropped opener.
func (m *Mod) summitOpenerPending(ctx context.Context, key string) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_summit_beats
		 WHERE storyKey = ? AND opens = 1 AND postedAt = 0 AND dropped = 0 AND expiresAt > ?`,
		key, clock.Unix()).Scan(&n)
	return n > 0
}

// summitBeatDone marks a beat posted (why "") or dropped.
func (m *Mod) summitBeatDone(ctx context.Context, id int64, why string) {
	if why == "" {
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_summit_beats SET postedAt = ? WHERE beatID = ?`, clock.Unix(), id)
		return
	}
	summitDrop(why)
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_summit_beats SET dropped = 1, dropWhy = ? WHERE beatID = ?`, why, id)
}

// summitExpireBeats drops beats past their time and prunes old rows.
func (m *Mod) summitExpireBeats(ctx context.Context) {
	now := clock.Unix()
	if res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_summit_beats SET dropped = 1, dropWhy = 'stale'
		 WHERE postedAt = 0 AND dropped = 0 AND expiresAt <= ?`, now); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			summitDropsMu.Lock()
			summitDrops["stale"] += n
			summitDropsMu.Unlock()
		}
	}
	// A fortnight of beat history is plenty for the panel and the dedupe:
	// every sourceKey carries a day or an event ID, so it never comes back.
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_summit_beats WHERE createdAt < ?`, now-14*86400)
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_summit_posts WHERE at < ?`, now-60*86400)
}
