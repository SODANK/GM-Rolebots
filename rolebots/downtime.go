package rolebots

// The downtime ledger: the windows the server was down, so a clock that gives up on work after a
// stretch of wall time (industryStalled) counts only the time the server ran. A heartbeat writes
// the time once a minute; at boot the gap since the last beat becomes a window. See
// docs/ROLEBOT_DESIGN.md#server-downtime.

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

const (
	// settingClockAlive is the last heartbeat (unix seconds).
	settingClockAlive = "rolebots.clock.alive"
	// settingClockDowntime is the ledger, a JSON list of [from, to] unix pairs.
	settingClockDowntime = "rolebots.clock.downtime"
	// downtimeBeatTicks is the heartbeat period, one minute.
	downtimeBeatTicks = 60
	// downtimeMinGapSec is the shortest boot gap recorded. A quick restart is under it; a crash
	// loses at most one beat of server time to it.
	downtimeMinGapSec = 5 * 60
	// downtimeKeepSec drops windows that ended longer ago than this. It must exceed the
	// longest wall span any reader measures across (a stalled project's life).
	downtimeKeepSec = 60 * 86400
	// downtimeMaxWindows caps the ledger; the oldest windows go first.
	downtimeMaxWindows = 128
)

// downWindow is one stretch the server was down, [From, To) in unix seconds.
type downWindow struct {
	From int64 `json:"f"`
	To   int64 `json:"t"`
}

var downtimeWins atomic.Pointer[[]downWindow]

var downtimeBeatBusy = make(chan struct{}, 1)

// downtimeBetween is how many seconds of [from, to) the server was down. Pure over the ledger.
func downtimeBetween(from, to int64) int64 {
	p := downtimeWins.Load()
	if p == nil || to <= from {
		return 0
	}
	return downtimeOverlap(*p, from, to)
}

// downtimeOverlap sums the parts of wins inside [from, to).
func downtimeOverlap(wins []downWindow, from, to int64) int64 {
	var sum int64
	for _, w := range wins {
		if lo, hi := max(w.From, from), min(w.To, to); hi > lo {
			sum += hi - lo
		}
	}
	return sum
}

// liveSince is the seconds the server ran between start and now: wall time less downtime.
// Clocks that abandon work for want of progress read this, never now-start.
func liveSince(start, now int64) int64 {
	if start <= 0 || now <= start {
		return 0
	}
	return now - start - downtimeBetween(start, now)
}

// downtimeAppend adds the window [from, to), prunes windows that ended before
// to-downtimeKeepSec, and keeps the newest downtimeMaxWindows. Pure.
func downtimeAppend(wins []downWindow, from, to int64) []downWindow {
	out := make([]downWindow, 0, len(wins)+1)
	for _, w := range wins {
		if w.To > to-downtimeKeepSec && w.To > w.From {
			out = append(out, w)
		}
	}
	if to > from {
		out = append(out, downWindow{From: from, To: to})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	if len(out) > downtimeMaxWindows {
		out = out[len(out)-downtimeMaxWindows:]
	}
	return out
}

// recordBootDowntime loads the ledger, adds the gap since the last heartbeat when it is at least
// downtimeMinGapSec, and writes a fresh beat. Runs at boot before any pass reads a clock.
func (m *Mod) recordBootDowntime(ctx context.Context) {
	now := clock.Unix()
	var wins []downWindow
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingClockDowntime); err != nil {
		rlogf("rolebots: downtime ledger: read: %v", err)
	} else if ok && v != "" {
		if err := json.Unmarshal([]byte(v), &wins); err != nil {
			rlogf("rolebots: downtime ledger: unreadable, starting over: %v", err)
			wins = nil
		}
	}
	var alive int64
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingClockAlive); err != nil {
		rlogf("rolebots: downtime ledger: read heartbeat: %v", err)
	} else if ok {
		alive, _ = strconv.ParseInt(v, 10, 64)
	}
	gap := int64(0)
	if alive > 0 && now-alive >= downtimeMinGapSec {
		gap = now - alive
		wins = downtimeAppend(wins, alive, now)
	} else {
		wins = downtimeAppend(wins, 0, 0)
	}
	downtimeWins.Store(&wins)
	if gap > 0 {
		rlogf("rolebots: server was down %s; project clocks skip it", time.Duration(gap)*time.Second)
		if raw, err := json.Marshal(wins); err == nil {
			if err := evedb.SetServerSetting(ctx, m.db, settingClockDowntime, string(raw)); err != nil {
				rlogf("rolebots: downtime ledger: write: %v", err)
			}
		}
	}
	m.writeHeartbeat(ctx)
}

func (m *Mod) writeHeartbeat(ctx context.Context) {
	if err := evedb.SetServerSetting(ctx, m.db, settingClockAlive, strconv.FormatInt(clock.Unix(), 10)); err != nil {
		rlogf("rolebots: downtime ledger: heartbeat: %v", err)
	}
}

// startHeartbeat writes the heartbeat once a minute. Not gated on the master switch: the ledger
// must know the server was up while the roster was off.
func (m *Mod) startHeartbeat() {
	m.registerPeriodic("rolebot-heartbeat", downtimeBeatTicks, func(uint64) bool {
		select {
		case downtimeBeatBusy <- struct{}{}:
		default:
			return true
		}
		goSafe("heartbeat", func() {
			defer func() { <-downtimeBeatBusy }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			m.writeHeartbeat(ctx)
		})
		return true
	})
}
