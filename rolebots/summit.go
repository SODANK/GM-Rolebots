package rolebots

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// The Summit desk: the bot world's drama on the Intergalactic Summit forum, as forum threads that read
// as player posts, about fifty a day. Design and dials: docs/SUMMIT_DESK.md.
//   - A story owns one thread (summit_story.go), keyed war:<warID>, alliance:<allianceID>,
//     pact:<a>:<b>, recruit:<corpID> or shop:<charID>. The first beat opens it, later ones reply.
//   - A beat is one post waiting to be written (summit_beats.go), filed by the module's event funnels.
//   - The desk lane runs every summitEveryTicks and posts the hottest beat the budget allows
//     (summit_budget.go), from line banks and real facts (summit_compose.go, summit_lines_*.go).
// Bots only talk about bots: a war or pact story needs bot-run corporations on both sides, no bot
// replies to a person's post, and nothing marks a post as a bot's where a player can see it. A
// capsuleer can be named in a bot story (nameCapsuleers).

const (
	settingSummitOn           = "rolebots.summit.on"
	settingSummitPerDay       = "rolebots.summit.perDay"
	settingSummitNewThreads   = "rolebots.summit.newThreadsPerDay"
	settingSummitThreadGapMin = "rolebots.summit.threadGapMin"
	settingSummitAuthorPerDay = "rolebots.summit.authorPerDay"
	settingSummitNameCaps     = "rolebots.summit.nameCapsuleers"
	settingSummitIdleDays     = "rolebots.summit.idleDays"
	settingSummitRepeatDays   = "rolebots.summit.repeatDays"
	settingSummitLastHumanAt  = "rolebots.summit.lastHumanAt"
	// settingSummitSharePrefix + category is a category's daily share of
	// perDay in percent; 0 switches the category off.
	settingSummitSharePrefix = "rolebots.summit."

	defaultSummitPerDay       = 50
	defaultSummitNewThreads   = 5
	defaultSummitThreadGapMin = 40
	defaultSummitAuthorPerDay = 3
	defaultSummitIdleDays     = 3
	defaultSummitRepeatDays   = 14

	// summitThreadPerDay caps one thread's posts in a day.
	summitThreadPerDay = 12
	// summitCarryMax is how many posts above the hour's own share one hour
	// may spend catching up.
	summitCarryMax = 2

	summitEveryTicks   = 300 // 5 minutes at 1 s ticks
	summitSweepPasses  = 12  // the hourly sweeps run every 12th pass
	summitLaneTimeout  = 60 * time.Second
	summitLoginPersist = 10 * time.Minute
)

// Beat categories. Each has its own daily share dial.
const (
	summitCatWar      = "war"
	summitCatAlliance = "alliance"
	summitCatRecruit  = "recruit"
	summitCatMarket   = "market"
)

var summitCategories = []string{summitCatWar, summitCatAlliance, summitCatRecruit, summitCatMarket}

var summitDefaultShare = map[string]int{
	summitCatWar:      50,
	summitCatAlliance: 30,
	summitCatRecruit:  10,
	summitCatMarket:   15,
}

// summitDials is every dial at once. Replaced whole on load and save.
type summitDials struct {
	On             bool
	PerDay         int
	NewThreads     int
	ThreadGapMin   int
	AuthorPerDay   int
	NameCapsuleers bool
	IdleDays       int
	RepeatDays     int
	Share          map[string]int
}

func defaultSummitDials() summitDials {
	share := map[string]int{}
	for k, v := range summitDefaultShare {
		share[k] = v
	}
	return summitDials{
		PerDay: defaultSummitPerDay, NewThreads: defaultSummitNewThreads,
		ThreadGapMin: defaultSummitThreadGapMin, AuthorPerDay: defaultSummitAuthorPerDay,
		NameCapsuleers: true, IdleDays: defaultSummitIdleDays, RepeatDays: defaultSummitRepeatDays,
		Share: share,
	}
}

var (
	summitDialsV       atomic.Pointer[summitDials]
	summitOnce         sync.Once
	summitBusy         atomic.Bool
	summitPassN        atomic.Int64
	summitHumanAt      atomic.Int64 // unix of the last capsuleer login
	summitHumanAtSaved atomic.Int64
)

func summitDialsNow() summitDials {
	if p := summitDialsV.Load(); p != nil {
		return *p
	}
	return defaultSummitDials()
}

func setSummitDials(d summitDials) { summitDialsV.Store(&d) }

// summitOn reports whether the desk files and posts at all.
func summitOn() bool { return summitDialsNow().On }

// summitCategoryOn reports whether a category has a share above zero.
func summitCategoryOn(cat string) bool {
	d := summitDialsNow()
	return d.On && d.Share[cat] > 0
}

func (d summitDials) limits() summitLimits {
	return summitLimits{
		PerDay: d.PerDay, NewThreads: d.NewThreads, ThreadGapSec: int64(d.ThreadGapMin) * 60,
		ThreadPerDay: summitThreadPerDay, AuthorPerDay: d.AuthorPerDay, CarryMax: summitCarryMax,
		Share: d.Share,
	}
}

// loadSummitSettings reads the dials. Off by default, like chatter.
func (m *Mod) loadSummitSettings(ctx context.Context) {
	d := defaultSummitDials()
	get := func(key string) (string, bool) {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, key)
		return strings.TrimSpace(v), err == nil && ok
	}
	readInt := func(key string, dst *int) {
		if v, ok := get(key); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				*dst = n
			}
		}
	}
	readBool := func(key string, dst *bool) {
		if v, ok := get(key); ok {
			*dst = truthySetting(v)
		}
	}
	readBool(settingSummitOn, &d.On)
	readInt(settingSummitPerDay, &d.PerDay)
	readInt(settingSummitNewThreads, &d.NewThreads)
	readInt(settingSummitThreadGapMin, &d.ThreadGapMin)
	readInt(settingSummitAuthorPerDay, &d.AuthorPerDay)
	readBool(settingSummitNameCaps, &d.NameCapsuleers)
	readInt(settingSummitIdleDays, &d.IdleDays)
	readInt(settingSummitRepeatDays, &d.RepeatDays)
	for _, c := range summitCategories {
		n := d.Share[c]
		readInt(settingSummitSharePrefix+c, &n)
		if n > 100 {
			n = 100
		}
		d.Share[c] = n
	}
	setSummitDials(d)

	// The idle gate's clock. A shard that never recorded a login starts the
	// window at boot, so a fresh install is not silent forever.
	at := clock.Unix()
	if v, ok := get(settingSummitLastHumanAt); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			at = n
		}
	}
	summitHumanAt.Store(at)
	summitHumanAtSaved.Store(at)
}

// summitNoteLogin records a capsuleer selecting a character, for the idle
// gate. Called from CharacterSelectGate; persisted at most every
// summitLoginPersist so a restart remembers it.
func (m *Mod) summitNoteLogin(charID int64) {
	if charID == 0 || m.isSynthetic(charID) {
		return
	}
	now := clock.Unix()
	summitHumanAt.Store(now)
	if now-summitHumanAtSaved.Load() < int64(summitLoginPersist/time.Second) {
		return
	}
	summitHumanAtSaved.Store(now)
	goSafe("summit login mark", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = evedb.SetServerSetting(ctx, m.db, settingSummitLastHumanAt, strconv.FormatInt(now, 10))
	})
}

// summitIdle reports whether no capsuleer has logged in within idleDays.
func summitIdle(now int64, d summitDials) bool {
	if d.IdleDays <= 0 {
		return false
	}
	return now-summitHumanAt.Load() > int64(d.IdleDays)*86400
}

// ── the lane ────────────────────────────────────────────────────────────────

func (m *Mod) startSummit() {
	if m.tick == nil {
		return
	}
	summitOnce.Do(func() {
		m.summitWatchKills()
		m.registerPeriodic("rolebots-summit", summitEveryTicks, func(uint64) bool {
			if !Master() || !summitOn() {
				return true
			}
			if !summitBusy.CompareAndSwap(false, true) {
				return true
			}
			goSafe("summit pass", func() {
				defer summitBusy.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), summitLaneTimeout)
				defer cancel()
				if summitPassN.Add(1)%summitSweepPasses == 1 {
					m.summitSweeps(ctx)
				}
				m.summitPass(ctx, false)
			})
			return true
		})
	})
}

// summitSweeps files the beats nothing else raises: the daily war tally and
// the recruitment bumps. Hourly.
func (m *Mod) summitSweeps(ctx context.Context) {
	m.summitExpireBeats(ctx)
	if summitCategoryOn(summitCatWar) {
		m.summitWarTallies(ctx)
	}
	if summitCategoryOn(summitCatRecruit) {
		m.summitRecruitSweep(ctx)
	}
}

// summitPass posts at most one beat. force skips the day, hour and idle
// gates (the panel's "post next now"); every other gate still holds.
// Returns what happened, for the panel.
func (m *Mod) summitPass(ctx context.Context, force bool) string {
	d := summitDialsNow()
	now := clock.Now()
	if !force && summitIdle(now.Unix(), d) {
		summitDrop("idle")
		return "idle: no capsuleer has logged in lately"
	}
	beats, err := m.summitDueBeats(ctx, now.Unix(), 40)
	if err != nil {
		rlogf("rolebots: summit beats: %v", err)
		return "error: " + err.Error()
	}
	if len(beats) == 0 {
		return "nothing queued"
	}
	lim := d.limits()
	marks := summitMarksSince(now.Unix() - 2*86400)
	for _, b := range beats {
		if ctx.Err() != nil {
			return "timeout"
		}
		st, err := m.summitStoryGet(ctx, b.StoryKey)
		if err != nil {
			continue
		}
		if st != nil && st.State == summitStoryClosed {
			m.summitBeatDone(ctx, b.ID, "closed")
			continue
		}
		var threadID int64
		if st != nil {
			threadID = st.ThreadID
		}
		if threadID == 0 && !b.Opens {
			if m.summitOpenerPending(ctx, b.StoryKey) {
				continue // waits for its story's opening post
			}
			b.Opens = true // the opener is gone: this beat opens the thread
		}
		author, err := m.summitAuthorFor(ctx, b, threadID)
		if err != nil || author == 0 {
			m.summitBeatDone(ctx, b.ID, "author")
			continue
		}
		ask := summitAsk{ThreadID: threadID, CharID: author, Category: b.Category}
		if !summitBumpCategory(b.Category) {
			ask.LastAuthor = m.summitLastAuthor(ctx, threadID)
		}
		gate := summitAllow(now, marks, ask, lim, force)
		if gate != "" {
			summitDrop(gate)
			if gate == "day" || gate == "hour" {
				return "budget: " + gate
			}
			continue
		}
		post, ok := m.summitCompose(ctx, b, author, threadID == 0)
		if !ok {
			m.summitBeatDone(ctx, b.ID, "render")
			continue
		}
		tid, err := m.summitPublish(ctx, b, st, author, post)
		if err != nil {
			rlogf("rolebots: summit post %s (%s): %v", b.StoryKey, b.Kind, err)
			continue
		}
		m.summitBeatDone(ctx, b.ID, "")
		summitMark(summitPostMark{At: now.Unix(), ThreadID: tid, CharID: author,
			Category: b.Category, NewThread: threadID == 0})
		return "posted " + b.Kind + " in " + b.StoryKey
	}
	return "nothing eligible"
}

// ── counters ────────────────────────────────────────────────────────────────

var (
	summitDropsMu sync.Mutex
	summitDrops   = map[string]int64{}
)

func summitDrop(why string) {
	summitDropsMu.Lock()
	summitDrops[why]++
	summitDropsMu.Unlock()
}

func summitDropCounts() map[string]int64 {
	summitDropsMu.Lock()
	defer summitDropsMu.Unlock()
	out := make(map[string]int64, len(summitDrops))
	for k, v := range summitDrops {
		out[k] = v
	}
	return out
}

// summitFacts is a beat's payload: slot name to text.
type summitFacts map[string]string

func (f summitFacts) encode() string {
	b, _ := json.Marshal(f)
	return string(b)
}

func decodeSummitFacts(s string) summitFacts {
	f := summitFacts{}
	_ = json.Unmarshal([]byte(s), &f)
	return f
}
