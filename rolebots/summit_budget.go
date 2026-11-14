package rolebots

import (
	"sync"
	"time"
)

// The Summit budget: the arithmetic that keeps the desk to about perDay posts
// a day, spread like a real board's traffic (summit.go is the desk).
//
// Pure: no Mod, no DB, the clock passed in. The gates run in the order the
// panel's drop counters list them, so a counter means what its label says.
// The "day" is the server's local calendar day.

// summitHourWeight is the board's traffic over a local day: quiet at night,
// busiest in the evening. Only the shape matters; it is normalised.
var summitHourWeight = [24]float64{
	0.5, 0.35, 0.25, 0.2, 0.2, 0.25, // 00-05
	0.4, 0.6, 0.75, 0.85, 0.9, 1.0, // 06-11
	1.0, 1.0, 1.0, 1.05, 1.1, 1.25, // 12-17
	1.5, 1.7, 1.8, 1.7, 1.3, 0.8, // 18-23
}

var summitHourSum = func() float64 {
	s := 0.0
	for _, w := range summitHourWeight {
		s += w
	}
	return s
}()

// summitLimits is every gate's dial.
type summitLimits struct {
	PerDay       int
	NewThreads   int
	ThreadGapSec int64
	ThreadPerDay int
	AuthorPerDay int
	CarryMax     int
	Share        map[string]int // category → percent of PerDay; 0 = off
}

// summitPostMark is one post the desk made, as the budget remembers it.
type summitPostMark struct {
	At        int64
	ThreadID  int64
	CharID    int64
	Category  string
	NewThread bool
}

// summitAsk is the post the desk wants to make. ThreadID 0 opens a thread.
type summitAsk struct {
	ThreadID   int64
	CharID     int64
	Category   string
	LastAuthor int64 // who posted last in that thread, anyone
}

// summitHourShare is the target for local hour h, in posts.
func summitHourShare(perDay, h int) float64 {
	return float64(perDay) * summitHourWeight[h%24] / summitHourSum
}

// summitDayTarget is how many posts the day should have made by now:
// every whole hour's share plus the elapsed part of this one.
func summitDayTarget(now time.Time, perDay int) float64 {
	h := now.Hour()
	t := 0.0
	for i := 0; i < h; i++ {
		t += summitHourShare(perDay, i)
	}
	frac := float64(now.Minute()*60+now.Second()) / 3600
	return t + summitHourShare(perDay, h)*frac
}

func summitDayStart(now time.Time) int64 {
	y, mo, d := now.Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, now.Location()).Unix()
}

// summitAllow answers "" when the post may go, else the first gate that
// refuses it. force (the panel's test post) skips the day and hour gates.
func summitAllow(now time.Time, marks []summitPostMark, ask summitAsk, lim summitLimits, force bool) string {
	day0 := summitDayStart(now)
	nowU := now.Unix()
	var today, lastHour, newToday, threadToday, authorToday, catToday int
	var threadLast int64
	for _, mk := range marks {
		if mk.At >= nowU-3600 {
			lastHour++
		}
		if ask.ThreadID != 0 && mk.ThreadID == ask.ThreadID && mk.At > threadLast {
			threadLast = mk.At
		}
		if mk.At < day0 {
			continue
		}
		today++
		if mk.NewThread {
			newToday++
		}
		if ask.ThreadID != 0 && mk.ThreadID == ask.ThreadID {
			threadToday++
		}
		if mk.CharID == ask.CharID {
			authorToday++
		}
		if mk.Category == ask.Category {
			catToday++
		}
	}
	if !force {
		if today >= lim.PerDay {
			return "day"
		}
		// Never ahead of the day's curve, and never more than CarryMax over
		// this hour's own share when catching up.
		if float64(today) >= summitDayTarget(now, lim.PerDay)+0.5 {
			return "hour"
		}
		if float64(lastHour) >= summitHourShare(lim.PerDay, now.Hour())+float64(lim.CarryMax) {
			return "hour"
		}
	}
	if ask.ThreadID == 0 && newToday >= lim.NewThreads {
		return "newthread"
	}
	if ask.ThreadID != 0 && threadLast != 0 && nowU-threadLast < lim.ThreadGapSec {
		return "threadgap"
	}
	if ask.ThreadID != 0 && threadToday >= lim.ThreadPerDay {
		return "threadday"
	}
	if authorToday >= lim.AuthorPerDay {
		return "author"
	}
	if ask.ThreadID != 0 && ask.LastAuthor == ask.CharID {
		return "twice"
	}
	share := lim.Share[ask.Category]
	if share <= 0 {
		return "category"
	}
	if capN := (lim.PerDay*share + 99) / 100; catToday >= capN {
		return "category"
	}
	return ""
}

// ── the desk's memory of its own posts ──────────────────────────────────────

// summitMarks is the last two days of posts, loaded from rolebot_summit_posts
// at boot (summitLoadMarks) and appended to on every post.
var (
	summitMarksMu sync.Mutex
	summitMarks   []summitPostMark
)

func summitMark(mk summitPostMark) {
	summitMarksMu.Lock()
	defer summitMarksMu.Unlock()
	summitMarks = append(summitMarks, mk)
	cut := mk.At - 2*86400
	i := 0
	for i < len(summitMarks) && summitMarks[i].At < cut {
		i++
	}
	summitMarks = summitMarks[i:]
}

func summitMarksSince(since int64) []summitPostMark {
	summitMarksMu.Lock()
	defer summitMarksMu.Unlock()
	out := make([]summitPostMark, 0, len(summitMarks))
	for _, mk := range summitMarks {
		if mk.At >= since {
			out = append(out, mk)
		}
	}
	return out
}

func resetSummitMarks(marks []summitPostMark) {
	summitMarksMu.Lock()
	summitMarks = append([]summitPostMark(nil), marks...)
	summitMarksMu.Unlock()
}
