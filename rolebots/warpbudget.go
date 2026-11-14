package rolebots

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// The bot-warp stamp budget limits how many bot projections may start a warp per second in a
// system a player is watching. Each hop forces a stamp per watching player per grid end it touches.
// See docs/comment-notes-b20.md#warp-stamp-budget.
const (
	// rolebots.warpBudgetPerMin is the hop allowance per minute for each watched system. 0 = off.
	// A hop costs two forced stamps on a quiet grid and one on a busy grid, so 30/min stays inside a
	// 1.00 stamp/s budget in both cases.
	defWarpBudgetPerMin = 30

	// rolebots.warpPauseLead stops bot warps in a system while a watcher's lead rise is at or above it. 0 = off.
	// It must exceed 3 (the client waits at lead 2 and fast-forwards at 3) and warpBurst*2, and stay
	// far below stampLeadResyncThreshold.
	defWarpPauseLead = 8

	settingWarpBudgetPerMin = "rolebots.warpBudgetPerMin"
	settingWarpPauseLead    = "rolebots.warpPauseLead"

	// warpBurst is the most tokens a bucket holds, so a quiet system can answer a small burst at once.
	// It is derived, not a setting: warpBurst*2 must stay below warpPauseLead, or a full bucket
	// discharged at once trips the interlock.
	warpBurst = 3

	// warpResumeLead is the hysteresis floor: the interlock releases here, not at warpPauseLead.
	// It is the client's own wait band. Releasing at the engage threshold makes a boundary grid admit
	// one hop, trip, drain and repeat.
	warpResumeLead = 2

	// warpRiseWindow is how far back the interlock looks for a watcher's lowest lead. The interlock
	// reads the rise over this window, never the lead itself (see systemWatch.worstRise). A runaway mint
	// rate climbs past warpPauseLead inside it, and a stream that stops climbing reads flat again.
	warpRiseWindow = 30 * time.Second

	// warpPauseMaxHold caps one interlock episode. warpPauseOpen is how long the gate then stays open
	// (still metered by the bucket) before it may latch again. The cap exists because a rising lead is
	// not always caused by bot departures, and refusing them then only keeps ships standing on the grid.
	warpPauseMaxHold = 60 * time.Second
	warpPauseOpen    = 20 * time.Second

	// warpRetryMinMS and warpRetryMaxMS bound how long a refused projection waits before asking again.
	// A whole bucket refill would triple a bot's dwell. The jitter keeps a refused grid from returning
	// in the same instant.
	warpRetryMinMS = 2000
	warpRetryMaxMS = 6000

	// undockBacklogWindow is how long after a discretionary refusal the grid still reads as drained to
	// the undock gate (peekWarpDrained). It must exceed warpRetryMaxMS so a live queue keeps the
	// reading dry.
	undockBacklogWindow = 10 * time.Second

	// How often one system may print a budget-refusal line.
	warpLogEvery = time.Minute

	// A gate nothing has touched for this long is dropped by sweepWarpGates.
	warpGateIdle = 10 * time.Minute
)

// warpClass says whether a caller may be turned away. It is a required argument so a new warp
// caller must decide: a ship refused a break-off warp stands there and dies.
type warpClass int

const (
	// The idle wander, a gang follow, a dry-belt hop. Refusable: the bot has
	// tens of seconds of dwell to spare and nothing is lost by reordering.
	warpDiscretionary warpClass = iota

	// A break-off under fire, a fleet order a player typed, a planned travel leg the abstract tier
	// already believes is under way. Never refused, but still debited so the budget does not understate spend.
	warpMandatory
)

var (
	warpBudgetPerMinVal atomic.Int64
	warpPauseLeadVal    atomic.Int64

	// Admissions by class since boot. A mandatory share near 100% means the classification has drifted
	// and the throttle does nothing. Mandatory means something is shooting the bot, not that it wants to leave.
	// See docs/comment-notes-b20.md#warp-class-drift.
	warpAdmitDiscretionary atomic.Int64
	warpAdmitMandatory     atomic.Int64
)

// warpMandatorySharePct is the percentage of admissions since the given
// baselines that could not be refused. Returns 0 when nothing has been
// admitted, which reads correctly: no traffic is not an all-mandatory grid.
func warpMandatorySharePct(baseD, baseM int64) int {
	d := warpAdmitDiscretionary.Load() - baseD
	m := warpAdmitMandatory.Load() - baseM
	if total := d + m; total > 0 {
		return int(m * 100 / total)
	}
	return 0
}

func warpBudgetPerMin() int { return int(warpBudgetPerMinVal.Load()) }
func warpPauseLead() int    { return int(warpPauseLeadVal.Load()) }

func init() {
	warpBudgetPerMinVal.Store(defWarpBudgetPerMin)
	warpPauseLeadVal.Store(defWarpPauseLead)
}

// setWarpBudgetPerMin sets the hop allowance. 0 disables the rate limit.
func setWarpBudgetPerMin(n int) {
	if n < 0 {
		n = 0
	}
	warpBudgetPerMinVal.Store(int64(n))
}

// setWarpPauseLead sets the interlock threshold. 0 disables the interlock.
// Other values clamp to [3, 60]: below 3 it fights the client's wait band, and above 60 the
// ballpark resync net fires first.
func setWarpPauseLead(n int) {
	switch {
	case n <= 0:
		n = 0
	case n < 3:
		n = 3
	case n > 60:
		n = 60
	}
	warpPauseLeadVal.Store(int64(n))
}

// loadWarpBudgetSettings reads both dials at boot and every minute (trafficsettings.go). A missing
// or unparseable value keeps the default. It must run before the grid lane starts: embodyTick reads
// the dials per pass with no database access.
func (m *Mod) loadWarpBudgetSettings(ctx context.Context) {
	read := func(name string, def int) int {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, name)
		if err != nil || !ok {
			return def
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return def
		}
		return n
	}
	setWarpBudgetPerMin(read(settingWarpBudgetPerMin, defWarpBudgetPerMin))
	setWarpPauseLead(read(settingWarpPauseLead, defWarpPauseLead))
}

// systemWatch is what one destiny read tells the gate: the connected player count and the worst
// stamp lead and rise. The fields are plain ints so the gate never holds a *destiny.Entity or a
// destiny lock across warpGateMu.
type systemWatch struct {
	witnesses int
	worstLead int
	// worstRise is the largest climb in any watcher's lead over warpRiseWindow: its lead now minus its
	// lowest lead in the window. The interlock and the birth hold read this, not worstLead, because after
	// one overshoot recovery the lead is a fixed offset that never falls and would latch the gate.
	worstRise int
}

// watcherLead is one connected watcher's stamp lead, read from destiny.
type watcherLead struct {
	id   uint32
	lead int
}

// leadSample is one reading in a watcher's leadTrail.
type leadSample struct {
	at   time.Time
	lead int
}

// leadTrails holds each watcher's recent lead readings for worstRise, keyed by entity ID, under
// warpGateMu. cachedSystemWatch is the only writer, one sample per warpWatchTTL. A SetState drops
// the lead near zero, so a fresh ballpark reads flat.
var leadTrails = map[uint32][]leadSample{}

// foldLeadRise records each watcher's reading and returns the worst rise.
// warpGateMu must be held.
func foldLeadRise(leads []watcherLead, now time.Time) int {
	worst := 0
	cutoff := now.Add(-warpRiseWindow)
	for _, wl := range leads {
		trail := leadTrails[wl.id]
		keep := trail[:0]
		for _, s := range trail {
			if !s.at.Before(cutoff) {
				keep = append(keep, s)
			}
		}
		keep = append(keep, leadSample{at: now, lead: wl.lead})
		leadTrails[wl.id] = keep
		// A rise counts only above zero. A negative lead is a stream minting slower than the wall (a warp
		// tunnel, or just after a SetState), and its climb back to 0 is not a runaway.
		cur := max(wl.lead, 0)
		low := cur
		for _, s := range keep {
			if l := max(s.lead, 0); l < low {
				low = l
			}
		}
		if r := cur - low; r > worst {
			worst = r
		}
	}
	return worst
}

// systemWarpGate is one system's allowance. It is keyed by systemID, never by *SolarSystem: a
// reloaded system is a new pointer, so pointer keys would leak entries and hand the reload a full bucket.
type systemWarpGate struct {
	tokens     float64
	lastRefill time.Time

	// paused is a latch, not a level read: it releases at warpResumeLead, not at warpPauseLead.
	paused   bool
	pausedAt time.Time
	// openUntil holds the gate open after an episode hits warpPauseMaxHold. forcedOpen records that the
	// last release was that cap rather than the lead going flat (for the log line).
	openUntil  time.Time
	forcedOpen bool
	// Warps refused since the last line this system printed, folded into the
	// next one rather than logged individually.
	refusals int
	lastLog  time.Time
	// lastRefusalAt is the last time a discretionary warp was turned away. The undock gate reads it
	// (peekWarpDrained). A mandatory admission never sets it.
	lastRefusalAt time.Time

	// The last destiny read for this system, and when it was taken. See
	// cachedSystemWatch: a refusal must be cheap, and reading the watch is not.
	watch   systemWatch
	watchAt time.Time

	// Admissions by class since the last log line, for this system alone. The package-level counters
	// are lifetime and global, the wrong scope for a per-system window.
	admitDisc int
	admitMand int
	// Warps admitted unbilled since the last log line because nobody connected
	// was on the departure grid (departureWitnessed).
	admitFree int
	// Mandatory admissions by calling function since the last log line, so the log can name which
	// caller produces unrefusable warps (see warpCallerName).
	mandBy map[string]int
}

// warpWatchTTL is how long one destiny read serves the gate. A read scans every entity and locks each
// watcher's e.mu, so a refusal must not repeat it. worstLead moves at most one point per wall second,
// so a one-second cache stays inside the warpPauseLead..warpResumeLead band.
const warpWatchTTL = time.Second

var (
	warpGateMu sync.Mutex
	warpGates  = map[int64]*systemWarpGate{}
)

// readSystemWatch asks destiny who is watching this system and how far ahead the worst of them is.
// It holds no rolebots lock and releases each destiny lock (ss.mu, then each e.mu) before returning.
// Only connected players count (GetAllPlayersInSystem); headless projections are not an audience.
// A nil system reads as unwatched, never as refused.
func readSystemWatch(ss *destiny.SolarSystem) systemWatch {
	w, _ := readSystemWatchLeads(ss)
	return w
}

// readSystemWatchLeads is readSystemWatch plus each watcher's own lead, which
// cachedSystemWatch folds into the rise trails under warpGateMu. Same lock
// discipline: destiny locks only, all released before it returns.
func readSystemWatchLeads(ss *destiny.SolarSystem) (systemWatch, []watcherLead) {
	if ss == nil {
		return systemWatch{}, nil
	}
	watchers := ss.GetAllPlayersInSystem()
	out := systemWatch{witnesses: len(watchers)}
	leads := make([]watcherLead, 0, len(watchers))
	for _, p := range watchers {
		l := p.StampLead()
		leads = append(leads, watcherLead{id: p.ID, lead: l})
		if l > out.worstLead {
			out.worstLead = l
		}
	}
	return out, leads
}

// cachedSystemWatch is readSystemWatch behind a warpWatchTTL cache on the gate. warpGateMu is
// released around the destiny read and retaken after, never held across it. A system with no gate yet
// is not cached: admitWarpSlot creates the gate, so the first ask reads through.
func cachedSystemWatch(systemID int64, ss *destiny.SolarSystem, now time.Time) systemWatch {
	warpGateMu.Lock()
	if g := warpGates[systemID]; g != nil && !g.watchAt.IsZero() && now.Sub(g.watchAt) < warpWatchTTL {
		w := g.watch
		warpGateMu.Unlock()
		return w
	}
	warpGateMu.Unlock()

	w, leads := readSystemWatchLeads(ss)

	warpGateMu.Lock()
	w.worstRise = foldLeadRise(leads, now)
	if g := warpGates[systemID]; g != nil {
		g.watch, g.watchAt = w, now
	}
	warpGateMu.Unlock()
	return w
}

// peekSystemRise reports the gate's last cached worstRise for a system and whether it is fresh
// enough to act on. It is a pure read under warpGateMu (a leaf lock), so it is safe under embodyMu.
// Not fresh means no recent bot warp traffic, so the caller should assume no lead problem.
func peekSystemRise(systemID int64) (rise int, fresh bool) {
	now := time.Now()
	warpGateMu.Lock()
	defer warpGateMu.Unlock()
	g := warpGates[systemID]
	if g == nil || g.watchAt.IsZero() || now.Sub(g.watchAt) > warpGateIdle {
		return 0, false
	}
	return g.watch.worstRise, true
}

// peekWarpDrained reports whether the gate would refuse a discretionary warp now (empty bucket,
// latched interlock, or a refusal inside undockBacklogWindow) and whether the reading is fresh. The
// undock gate uses it. It debits nothing. Not fresh or unwatched reads as not drained. A recent refusal
// counts as dry because the bucket alone shows a spare token in the gap before a queued warp takes it.
func peekWarpDrained(systemID int64) (dry bool, fresh bool) {
	// The wall clock is used here: the gate's stamps (lastRefill, watchAt) meter a connected client's
	// real seconds (see clockExemptFiles).
	return peekWarpDrainedAt(systemID, time.Now())
}

// peekWarpDrainedAt is peekWarpDrained against a given instant, for tests.
func peekWarpDrainedAt(systemID int64, now time.Time) (dry bool, fresh bool) {
	perMin := warpBudgetPerMin()
	if perMin <= 0 {
		return false, false
	}
	warpGateMu.Lock()
	defer warpGateMu.Unlock()
	g := warpGates[systemID]
	if g == nil || g.watchAt.IsZero() || now.Sub(g.watchAt) > warpGateIdle {
		return false, false
	}
	if g.watch.witnesses == 0 {
		return false, true
	}
	if g.paused {
		return true, true
	}
	if !g.lastRefusalAt.IsZero() && now.Sub(g.lastRefusalAt) < undockBacklogWindow {
		return true, true
	}
	tokens := g.tokens
	if el := now.Sub(g.lastRefill).Seconds(); el > 0 {
		tokens += el * float64(perMin) / 60
	}
	return tokens < 1, true
}

// admitWarpSlot is the whole decision. It touches only warpGateMu, the bucket and arithmetic, so
// it tests with no destiny and no database. reason is "" on admission and names the refusal otherwise.
func admitWarpSlot(systemID int64, w systemWatch, class warpClass, now time.Time) (ok bool, reason string) {
	return admitWarpSlotFrom(systemID, w, class, "", now)
}

// admitWarpSlotFrom is admitWarpSlot with the mandatory caller's name, for the per-system tally.
func admitWarpSlotFrom(systemID int64, w systemWatch, class warpClass, src string, now time.Time) (ok bool, reason string) {
	perMin := warpBudgetPerMin()
	if perMin <= 0 {
		return true, "" // rate limit disabled; the interlock goes with it
	}
	// Nobody is watching, so the hop mints into no connection. Leave the bucket alone so a dark system
	// builds no debt for the first arrival. Unlike undockwave.go, an unwatched hop leaves no backlog.
	if w.witnesses == 0 {
		return true, ""
	}

	warpGateMu.Lock()
	defer warpGateMu.Unlock()

	g := warpGates[systemID]
	if g == nil {
		g = &systemWarpGate{tokens: warpBurst, lastRefill: now}
		warpGates[systemID] = g
	}

	// Refill first, so a mandatory warp's debit is applied to an up-to-date
	// bucket and the elapsed time is never counted twice.
	if el := now.Sub(g.lastRefill).Seconds(); el > 0 {
		g.tokens += el * float64(perMin) / 60
		if g.tokens > warpBurst {
			g.tokens = warpBurst
		}
		g.lastRefill = now
	}

	// Mandatory warps are billed but never refused, before the interlock is consulted. Do not deepen
	// the -warpBurst debt floor. See docs/comment-notes-b20.md#mandatory-debt-floor.
	if class == warpMandatory {
		if g.tokens -= 1; g.tokens < -warpBurst {
			g.tokens = -warpBurst
		}
		warpAdmitMandatory.Add(1)
		g.admitMand++
		if g.mandBy == nil {
			g.mandBy = map[string]int{}
		}
		g.mandBy[src]++
		return true, ""
	}

	// The interlock is checked before the bucket, so a paused system burns no tokens. A release falls
	// through to the bucket: it lets metering resume, not a free hop. Both edges read worstRise.
	released := false
	if pause := warpPauseLead(); pause > 0 {
		if g.paused {
			capped := now.Sub(g.pausedAt) >= warpPauseMaxHold
			if w.worstRise > warpResumeLead && !capped {
				g.refusals++
				g.lastRefusalAt = now
				return false, "interlock"
			}
			g.paused = false
			g.forcedOpen = capped
			if capped {
				g.openUntil = now.Add(warpPauseOpen)
			}
			released = true
		} else if w.worstRise >= pause && !now.Before(g.openUntil) {
			g.paused, g.pausedAt = true, now
			g.refusals++
			g.lastRefusalAt = now
			return false, "engaged"
		}
	}

	if g.tokens < 1 {
		g.refusals++
		g.lastRefusalAt = now
		if released {
			// The interlock released but the budget is empty: reported separately.
			return false, "released-budget"
		}
		return false, "budget"
	}
	g.tokens--
	warpAdmitDiscretionary.Add(1)
	g.admitDisc++
	if released {
		return true, "released"
	}
	return true, ""
}

// admitBotWarp is the gate as the warp path calls it: read destiny, decide, log. It logs with
// log.Printf to server.log, not m.logBot (a database row per line, keyed by character) and not
// rlogf (the module console). A healthy grid prints nothing.
func (m *Mod) admitBotWarp(eb *embodiedBot, class warpClass) bool {
	now := time.Now()
	// Cached: a refusal must not cost a full system scan and a lock on every
	// watcher. See cachedSystemWatch.
	watch := cachedSystemWatch(eb.systemID, eb.ss, now)
	// A hop nobody on the departure grid can see is free, and is admitted
	// without touching the bucket. See departureWitnessed.
	if watch.witnesses > 0 && !departureWitnessed(eb) {
		warpGateMu.Lock()
		if g := warpGates[eb.systemID]; g != nil {
			g.admitFree++
		}
		warpGateMu.Unlock()
		return true
	}
	// Only mandatory admissions are attributed to a caller. See systemWarpGate.mandBy.
	src := ""
	if class == warpMandatory {
		src = warpCallerName()
	}
	ok, reason := admitWarpSlotFrom(eb.systemID, watch, class, src, now)

	switch reason {
	case "engaged":
		log.Printf("[rolebots] warp interlock ENGAGED system %d — watcher stamp lead rose +%d in %.0fs "+
			"(now +%d) >= %d across %d watcher(s); bot warps held until it flattens to +%d or %.0fs pass",
			eb.systemID, watch.worstRise, warpRiseWindow.Seconds(), watch.worstLead, warpPauseLead(),
			watch.witnesses, warpResumeLead, warpPauseMaxHold.Seconds())
	case "released", "released-budget":
		var held time.Duration
		forced := false
		warpGateMu.Lock()
		if g := warpGates[eb.systemID]; g != nil && !g.pausedAt.IsZero() {
			held = now.Sub(g.pausedAt)
			forced = g.forcedOpen
		}
		warpGateMu.Unlock()
		why := fmt.Sprintf("rise +%d", watch.worstRise)
		if forced {
			why = fmt.Sprintf("still rising +%d but held the %.0fs cap; open %.0fs",
				watch.worstRise, warpPauseMaxHold.Seconds(), warpPauseOpen.Seconds())
		}
		log.Printf("[rolebots] warp interlock released system %d — %s (lead +%d) after %.0fs; %s",
			eb.systemID, why, watch.worstLead, held.Seconds(),
			map[bool]string{true: "budget still empty, metering resumes", false: "bot warps resume"}[!ok])
	case "budget", "interlock":
		m.logWarpRefusals(eb.systemID, watch, now)
	}
	return ok
}

// departureWitnessed reports whether any connected player on this projection's grid holds its ball,
// the only players phase 2's forced flush mints into. The system-wide witness count over-bills: a hop
// out of a grid nobody is on costs every watcher nothing. A nil system or entity reads as unwitnessed.
func departureWitnessed(eb *embodiedBot) bool {
	if eb == nil || eb.ss == nil || eb.ent == nil {
		return false
	}
	for _, mate := range eb.ss.GetBubbleConnectedPlayersExcept(eb.ent.ID) {
		if mate.Conn != nil && mate.KnowsBall(eb.ent.ID) {
			return true
		}
	}
	return false
}

// logWarpRefusals prints at most one budget line per system per minute, folding the refusals since
// the last line into it (the same shape as logEchoAllows).
func (m *Mod) logWarpRefusals(systemID int64, w systemWatch, now time.Time) {
	warpGateMu.Lock()
	g := warpGates[systemID]
	if g == nil || now.Sub(g.lastLog) < warpLogEvery {
		warpGateMu.Unlock()
		return
	}
	n := g.refusals
	disc, mand, free := g.admitDisc, g.admitMand, g.admitFree
	by := topMandatorySources(g.mandBy, 3)
	g.refusals, g.admitDisc, g.admitMand, g.admitFree = 0, 0, 0, 0
	g.mandBy = nil
	g.lastLog = now
	warpGateMu.Unlock()

	// The share is per system and per window, matching the sentence it appears in. The package-level
	// counters are since-boot and read 100% whenever the interlock is engaged.
	sharePct := 0
	if tot := disc + mand; tot > 0 {
		sharePct = mand * 100 / tot
	}

	// The watcher count is in the line: a high count means too few projections per player, whose lever
	// is rolebots.maxProjections. A mandatory share near 100% means the warpClass split has drifted.
	log.Printf("[rolebots] warp budget system %d — %d warp(s) refused in the last %.0fs "+
		"(%d/min, %d watcher(s), worst stamp lead +%d rising +%d/%.0fs, %d admitted: %d%% unrefusable%s; "+
		"%d off-grid admitted free)",
		systemID, n, warpLogEvery.Seconds(), warpBudgetPerMin(), w.witnesses, w.worstLead,
		w.worstRise, warpRiseWindow.Seconds(), disc+mand, sharePct, by, free)
}

// warpFunnel is the fixed chain every warp passes through on its way to the budget, innermost first.
// warpCallerName skips these frames so a mandatory admission is attributed to the function that chose
// the class. They match by function name, not file, because embody.go also holds honest callers.
var warpFunnel = []string{
	"startEmbodyWarpAs",
	"startEmbodyWarpTo",
}

// warpFrameIsFunnel reports whether a "pkg.(*T).Method" frame name is one of the funnel's own. It is
// split out so the list can be tested by name without a stack, in case it goes stale as the warp path
// grows a wrapper.
func warpFrameIsFunnel(name string) bool {
	for _, fn := range warpFunnel {
		if strings.HasSuffix(name, "."+fn) {
			return true
		}
	}
	return false
}

// warpCallerName names the function that chose this warp's class, the first frame outside this file
// and outside warpFunnel, as "pkg.(*T).Method (file.go:LINE)". It has the same shape as destiny's
// stampCaller. The walk runs once per admitted mandatory warp.
func warpCallerName() string {
	var pcs [16]uintptr
	// Skip runtime.Callers, warpCallerName, admitBotWarp.
	n := runtime.Callers(3, pcs[:])
	if n == 0 {
		return "unknown"
	}
	frames := runtime.CallersFrames(pcs[:n])
	// Keep the funnel frame as a fallback: a caller that is the funnel itself is better named than
	// reported as "unknown".
	fallback := ""
	for {
		f, more := frames.Next()
		if f.Function == "" {
			break
		}
		name := f.Function
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		skip := filepath.Base(f.File) == "warpbudget.go" || strings.HasPrefix(f.Function, "runtime.")
		if !skip && warpFrameIsFunnel(name) {
			skip = true
			if fallback == "" {
				fallback = fmt.Sprintf("%s (%s:%d)", name, filepath.Base(f.File), f.Line)
			}
		}
		if !skip {
			return fmt.Sprintf("%s (%s:%d)", name, filepath.Base(f.File), f.Line)
		}
		if !more {
			break
		}
	}
	if fallback != "" {
		return fallback
	}
	return "unknown"
}

// topMandatorySources renders the busiest mandatory callers for the budget
// line, worst first. Empty string when there were none, so a grid with no
// mandatory traffic prints nothing extra.
func topMandatorySources(by map[string]int, top int) string {
	if len(by) == 0 {
		return ""
	}
	type row struct {
		name string
		n    int
	}
	rows := make([]row, 0, len(by))
	for k, v := range by {
		rows = append(rows, row{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].n != rows[j].n {
			return rows[i].n > rows[j].n
		}
		return rows[i].name < rows[j].name
	})
	if len(rows) > top {
		rows = rows[:top]
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s x%d", r.name, r.n))
	}
	return " — mandatory from: " + strings.Join(parts, ", ")
}

// deferWarp turns a refusal into "ask again shortly". It leaves the projection exactly as an idle one
// (the same contract as admitUndock's refusal). nextActAt is assigned an absolute future time, never
// added to, or a value already in the past leaves the bot spinning every tick.
func (m *Mod) deferWarp(eb *embodiedBot) {
	eb.mu.Lock()
	eb.nextActAt = time.Now().Add(warpRetryDelay())
	eb.mu.Unlock()
}

// warpRetryDelay is the jittered "ask again shortly" interval shared by deferWarp (an idle bot, paced
// by nextActAt) and flyToCelestial (a held travel leg, paced by the sim's LegEndsAt). One shared
// jitter keeps a refused grid from returning in the same instant.
func warpRetryDelay() time.Duration {
	return time.Duration(warpRetryMinMS+rand.Intn(warpRetryMaxMS-warpRetryMinMS)) * time.Millisecond
}

// sweepWarpGates drops gates for systems nothing has warped in for a while, so the registry does
// not only grow.
func sweepWarpGates() {
	cutoff := time.Now().Add(-warpGateIdle)
	warpGateMu.Lock()
	for sysID, g := range warpGates {
		if g.lastRefill.Before(cutoff) {
			delete(warpGates, sysID)
		}
	}
	// A trail whose newest sample is out of the window is a watcher that left
	// or docked; foldLeadRise would discard every sample anyway.
	trailCutoff := time.Now().Add(-warpRiseWindow)
	for id, trail := range leadTrails {
		if len(trail) == 0 || trail[len(trail)-1].at.Before(trailCutoff) {
			delete(leadTrails, id)
		}
	}
	warpGateMu.Unlock()
}

func init() { registerSweep("warpGates", sweepWarpGates) }
