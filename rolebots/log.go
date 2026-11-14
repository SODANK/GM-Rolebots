package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// Activity log: every call and input a rolebot makes is recorded in rolebot_log
// and painted in the module's own console window (console.go), not the server's.
// It is surfaced at /rolebots/log with level, character, category and text
// filters, a live tail, and a board of every rostered bot (what it is doing,
// where, in what) with a Reboot button for the ones that wedged.

const (
	// logDebug is per-step narration ("heading home to take stock", "hold full") that
	// a large roster says thousands of times a minute. It is written only while
	// rolebots.logDebug is on (loadLogRetention); off, the line costs the Sprintf.
	logDebug = "debug"
	logInfo  = "info"
	// logWarn is a business outcome that went against the pilot (a listing the broker
	// fee turned away, a shopping trip with no seller), not a module fault. The panel
	// tints it amber and the error filter skips it.
	logWarn  = "warn"
	logError = "error"
)

// The log pages' read budget. A cancelled handler context only stops the client
// waiting, so every read is wrapped in capStatementFor(logStatementCap, ...), a
// little under logHandlerTimeout, to make the statement die first.
const (
	logHandlerTimeout = 15 * time.Second
	logStatementCap   = 13 * time.Second
	// logQueryMaxRows bounds a free-text search to the newest N lines unless a
	// character is selected: no index serves a LIKE, so cost is the row count. The
	// floor is a logID computed once per request (boundQuery) and passed as a literal.
	logQueryMaxRows = 1_000_000
)

// How many days of activity the log keeps. It is a setting because the right
// answer depends on the roster's size, and an operator who must rebuild to
// shorten it will let it grow.
const (
	defLogRetentionDays     = 7
	settingLogRetentionDays = "rolebots.logRetentionDays"
)

var logRetentionDaysVal atomic.Int64

func logRetentionDays() int {
	if n := logRetentionDaysVal.Load(); n > 0 {
		return int(n)
	}
	return defLogRetentionDays
}

// loadLogRetention reads the setting at boot. Out-of-range or unreadable keeps
// the default; zero is not "keep for ever", because a table nobody prunes is the
// failure this prevents.
func (m *Mod) loadLogRetention(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingLogDebug); err == nil && ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "on", "yes":
			logDebugOn.Store(true)
		default:
			logDebugOn.Store(false)
		}
	}
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingLogRetentionDays)
	if err != nil || !ok {
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return
	}
	logRetentionDaysVal.Store(int64(n))
}

// settingLogDebug switches the debug level's lines on ("1"); default off.
const settingLogDebug = "rolebots.logDebug"

var logDebugOn atomic.Bool

// logBot records one rolebot action. category is a short slug ("sim", "travel",
// "mining", "market", "procure", "fit", "presence"). Repeats are folded
// (logEcho) and the rest go to a batching writer (logWriter), so a sim worker
// never waits on a database round trip.
func (m *Mod) logBot(charID int64, level, category, format string, args ...any) {
	if level == logDebug && !logDebugOn.Load() {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if !m.logEchoAllows(charID, level, category, msg) {
		return
	}
	m.logLine(charID, level, category, msg)
}

// logLine is the unconditional half: console plus the write queue. Used by
// logBot once the repeat filter has had its say, and by the filter itself
// when it flushes a rollup.
func (m *Mod) logLine(charID int64, level, category, msg string) {
	// The module's own console window, not the server's (console.go).
	emit(level, category, m.charLabel(charID), msg)
	m.queueLogRow(logRow2{
		ts: clock.Unix(), charID: charID,
		level: level, category: category, message: msg,
	})
}

// ── repeat folding ──────────────────────────────────────────────────────────
//
// The commonest lines are the same sentence from the same pilot over and over. A
// line that exactly repeats one this pilot said recently is counted instead of
// written, and the count rides out on the next copy that does get written ("...and
// 87 more like it in the last 15m0s"). It rides on the line itself because the
// panel renders newest-first and the console oldest-first, so a separate rollup
// row would sit on the wrong side in one of them. The first occurrence always goes
// out at once. wants.go does this by hand for the fit-gap line (fitGapSaid).

const (
	// logRepeatWindow is how long an identical line stays folded. Long
	// enough to cover a stuck FSM cycling every ten seconds; short enough
	// that a genuinely periodic report still lands several times an hour.
	logRepeatWindow = 15 * time.Minute
	// logEchoIdle is when a fold with nothing behind it is forgotten. The map is keyed
	// by pilot and message, and sized off the longest window any category uses, or
	// its folds would be swept out from under it.
	logEchoIdle = 2 * logPlannerRepeatWindow

	// logPlannerRepeatWindow is how long the planner's refusals (sim and procure
	// categories) fold: thousands of pilots repeat the same one per window, and an
	// hour still gives several chances a shift to notice a stuck pilot. Reporting only.
	logPlannerRepeatWindow = time.Hour
)

// logFoldWindow is how long an identical line from one pilot stays folded.
func logFoldWindow(category string) time.Duration {
	switch category {
	case "sim", "procure":
		return logPlannerRepeatWindow
	}
	return logRepeatWindow
}

type logEcho struct {
	at    int64  // when the last line was written
	count int    // lines of this shape folded since
	seen  int64  // when the last line was folded (for the idle sweep)
	last  string // the last line folded, for the sweep's flush
}

var (
	logEchoMu sync.Mutex
	logEchoes = map[string]*logEcho{}
)

// logEchoKey folds on the line's shape, the sentence with every number replaced,
// not its text: "hold full: 1,240 units" and "hold full: 1,180 units" are one
// report, and keyed on the raw text neither folded.
func logEchoKey(charID int64, level, category, msg string) string {
	return strconv.FormatInt(charID, 10) + "\x00" + level + "\x00" + category + "\x00" + logShape(msg)
}

// logShape is msg with every run of digits (and the dots and commas inside
// a number) replaced by '#'. A line with no digits is returned as it is.
func logShape(msg string) string {
	var b []byte
	inNum := false
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		digit := c >= '0' && c <= '9'
		if inNum && (digit || c == '.' || c == ',') {
			continue
		}
		inNum = false
		if digit {
			if b == nil {
				b = make([]byte, 0, len(msg))
				b = append(b, msg[:i]...)
			}
			b = append(b, '#')
			inNum = true
			continue
		}
		if b != nil {
			b = append(b, c)
		}
	}
	if b == nil {
		return msg
	}
	return string(b)
}

// logEchoAllows reports whether this line should be written. When it folds a line
// it returns false; when it lets one through it first flushes whatever was folded
// behind the previous copy. Module-wide lines (charID 0) are never folded.
func (m *Mod) logEchoAllows(charID int64, level, category, msg string) bool {
	if charID == 0 {
		return true
	}
	now := clock.Unix()
	key := logEchoKey(charID, level, category, msg)

	logEchoMu.Lock()
	e, ok := logEchoes[key]
	if !ok {
		logEchoes[key] = &logEcho{at: now, seen: now}
		logEchoMu.Unlock()
		return true
	}
	window := logFoldWindow(category)
	e.seen = now
	if now-e.at < int64(window/time.Second) {
		e.count++
		e.last = msg
		logEchoMu.Unlock()
		return false
	}
	folded := e.count
	e.count, e.at = 0, now
	logEchoMu.Unlock()

	// The window lapsed with copies behind it: this copy goes out carrying the count.
	if folded > 0 {
		m.logLine(charID, level, category,
			fmt.Sprintf("%s (and %d more like it in the last %s)", msg, folded, window))
		return false
	}
	return true
}

// sweepLogEchoes drops folds nobody has repeated lately, flushing any count still
// behind them. Registered on the module's cache sweep (cachesweep.go); without it
// the map grows with every distinct sentence.
func (m *Mod) sweepLogEchoes() {
	now := clock.Unix()
	type flush struct {
		charID          int64
		level, category string
		message         string
		count           int
	}
	var out []flush

	logEchoMu.Lock()
	for k, e := range logEchoes {
		if now-e.seen < int64(logEchoIdle/time.Second) {
			continue
		}
		if e.count > 0 {
			// Unpack the key rather than storing the parts twice: it is the
			// pilot, the level, the category and the message, in that order.
			parts := strings.SplitN(k, "\x00", 4)
			if len(parts) == 4 {
				id, _ := strconv.ParseInt(parts[0], 10, 64)
				msg := e.last
				if msg == "" {
					msg = parts[3]
				}
				out = append(out, flush{id, parts[1], parts[2], msg, e.count})
			}
		}
		delete(logEchoes, k)
	}
	logEchoMu.Unlock()

	for _, f := range out {
		m.logLine(f.charID, f.level, f.category,
			fmt.Sprintf("%s (and %d more like it before it stopped)", f.message, f.count))
	}
}

// ── the batching writer ─────────────────────────────────────────────────────

// logRow2 is one queued line. (logRow is already the ESP page's row type.)
type logRow2 struct {
	ts, charID      int64
	level, category string
	message         string
}

const (
	// logQueueDepth is how many lines may be in flight before the module
	// starts dropping them. Sized for a full sim pass's worth of a large
	// roster, so a slow database costs latency rather than lines.
	logQueueDepth = 16384
	// logBatchMax is the most rows one INSERT carries: past a couple of hundred rows
	// there is little left to win.
	logBatchMax = 200
	// logBatchWait is how long a partial batch waits for company. Below the
	// live tail's own 3s poll, so the panel never shows a line late enough
	// for anyone to notice.
	logBatchWait = 250 * time.Millisecond
)

// The queue is made at package init, not in startLogWriter, because Init logs
// before the writer is up and a channel assigned later would be a data race.
// Lines queued early wait in it.
var (
	logQ        = make(chan logRow2, logQueueDepth)
	logQOnce    sync.Once
	logDropped  atomic.Int64
	logReported atomic.Int64
)

// queueLogRow hands one line to the writer. It never blocks: a full queue drops
// the line and the writer reports the drop once per batch.
func (m *Mod) queueLogRow(r logRow2) {
	select {
	case logQ <- r:
	default:
		logDropped.Add(1)
	}
}

// startLogWriter starts the one goroutine that drains the queue. Called once
// from Init.
func (m *Mod) startLogWriter() {
	logQOnce.Do(func() { go m.runLogWriter() })
}

// runLogWriter is the one goroutine that writes rolebot_log. Everything the roster
// says funnels through here, so the retention prune can run inline without
// competing with concurrent inserts for row locks on the rows it deletes.
func (m *Mod) runLogWriter() {
	defer func() {
		if r := recover(); r != nil {
			rlogf("rolebots: log writer panicked: %v\n%s", r, debug.Stack())
			// Bring it back: a server whose module log has died is a server
			// nobody can diagnose.
			go m.runLogWriter()
		}
	}()

	batch := make([]logRow2, 0, logBatchMax)
	timer := time.NewTimer(logBatchWait)
	defer timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		m.writeLogRows(batch)
		batch = batch[:0]
		if n := logDropped.Load(); n > logReported.Load() {
			logReported.Store(n)
			rlogf("rolebots: log queue full — %d line(s) dropped so far", n)
		}
		m.maybePruneLog()
	}

	for {
		select {
		case r := <-logQ:
			batch = append(batch, r)
			if len(batch) >= logBatchMax {
				flush()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(logBatchWait)
			}
		case <-timer.C:
			flush()
			timer.Reset(logBatchWait)
		}
	}
}

// writeLogRows is the multi-row INSERT.
func (m *Mod) writeLogRows(rows []logRow2) {
	if len(rows) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString(`INSERT INTO rolebot_log (ts, characterID, level, category, message) VALUES `)
	args := make([]any, 0, len(rows)*5)
	for i, r := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(?,?,?,?,?)")
		args = append(args, r.ts, r.charID, r.level, r.category, r.message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := m.db.ExecContext(ctx, b.String(), args...); err != nil {
		rlogf("rolebots: log insert (%d rows): %v", len(rows), err)
	}
}

// logPruneChunk is how many expired rows one prune roll removes. The delete is
// chunked because an unbounded one is a long transaction that rolls back if it
// cannot finish in its context, so the table would never shrink.
const logPruneChunk = 2000

// logPruneEvery is how many batches pass between prune rolls. One roll (2,000
// deletable rows) per batch (up to 200 written) lets the table converge to its
// retention window; a slower roll rate would only slow its growth.
const logPruneEvery = 1

var (
	logBatchCount   int
	logPruneBackoff time.Time
)

// logPruneBackoffFor is how long the writer stops pruning after a roll fails. A
// timed-out DELETE is cancelled on the client only and keeps running on the
// server, so rolling again each batch would stack them and starve the INSERT.
const logPruneBackoffFor = 2 * time.Minute

func (m *Mod) maybePruneLog() {
	logBatchCount++
	if logBatchCount%logPruneEvery != 0 {
		return
	}
	now := time.Now()
	if now.Before(logPruneBackoff) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cutoff := now.AddDate(0, 0, -logRetentionDays()).Unix()
	// ORDER BY ts, never logID: only ts matches the index the WHERE ranges over
	// (idx_rblog_ts). By logID the plan sorts the whole expired range, minutes per roll.
	res, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_log WHERE ts < ? ORDER BY ts LIMIT ?`,
		cutoff, logPruneChunk)
	if err != nil {
		logPruneBackoff = now.Add(logPruneBackoffFor)
		rlogf("rolebots: log prune: %v — pausing prunes for %s", err, logPruneBackoffFor)
		return
	}
	// A converged table answers zero for every roll until the next row expires, so
	// five minutes between empty rolls loses nothing.
	if n, _ := res.RowsAffected(); n == 0 {
		logPruneBackoff = now.Add(logPruneIdleFor)
	}
}

// logPruneIdleFor is how long the writer rests after a roll that found
// nothing to delete.
const logPruneIdleFor = 5 * time.Minute

// charLabel is the pilot's name for the console window, which has no hyperlinks
// and no room for a 9-digit ID. Names never change under a running server, so one
// lookup per bot is enough; a failed lookup falls back to the ID.
var charNames sync.Map // int64 -> string

func (m *Mod) charLabel(charID int64) string {
	if charID == 0 {
		return ""
	}
	if v, ok := charNames.Load(charID); ok {
		return v.(string)
	}
	name := strconv.FormatInt(charID, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n string
	if err := m.db.QueryRowContext(ctx,
		`SELECT characterName FROM character_ WHERE characterID = ?`, charID).Scan(&n); err == nil && n != "" {
		name = n
	}
	charNames.Store(charID, name)
	return name
}

// ── phase labels ────────────────────────────────────────────────────────────

// phaseLabel turns a task into something readable on the panel. The prose lives
// on the task (TaskDef.Label in task.go), not in a map kept in step by hand.
func phaseLabel(st *SimState) string {
	if st == nil || st.Phase == "" {
		return "Idle"
	}
	if d, ok := taskDef(st.Phase); ok {
		return d.Label
	}
	return st.Phase
}

// ── the bot board ───────────────────────────────────────────────────────────

// boardRow is one live rolebot line under the log.
type boardRow struct {
	CharID     int64  `json:"charID"`
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	Status     string `json:"status"` // halted | working | idle | released
	StatusText string `json:"statusText"`
	Activity   string `json:"activity"`
	Doing      string `json:"doing"`
	Detail     string `json:"detail"`
	Where      string `json:"where"`
	ShipTypeID int64  `json:"shipTypeID"`
	ShipName   string `json:"shipName"`
	LastMsg    string `json:"lastMsg"`
	LastAgo    string `json:"lastAgo"`
	LastLevel  string `json:"lastLevel"`
	Error      string `json:"error"`
	Halted     bool   `json:"halted"`
	Updated    string `json:"updated"`

	// The rest is what the board needs to answer "what is it doing" without
	// the operator having to read the log line by line.
	Docked   bool   `json:"docked"`
	Embodied bool   `json:"embodied"` // a real ball on someone's grid
	Lasers   bool   `json:"lasers"`   // mining for real, on real rocks
	Presence string `json:"presence"` // one-word summary of the two above
	Dest     string `json:"dest"`     // where this leg ends
	Ore      string `json:"ore"`
	HoldPct  int    `json:"holdPct"`
	HoldText string `json:"holdText"` // "1 200 / 2 000 m³"
	Wallet   string `json:"wallet"`
}

// The board memo. botBoard is a state load and several lookups per bot, so the
// first caller past boardMemoTTL builds and anyone arriving meanwhile shares it.
// Keyed by showGen (two different boards); a panel reboot drops it.
const boardMemoTTL = 10 * time.Second

var boardMemo struct {
	mu sync.Mutex
	by map[bool]*boardMemoEntry
}

type boardMemoEntry struct {
	rows []boardRow
	err  error
	at   time.Time
	// done is non-nil while a build is in flight and is closed when it
	// lands; waiters read rows/err off this entry afterwards (never off the
	// map, which clearBoardMemo may have emptied under them).
	done chan struct{}
}

func (m *Mod) cachedBotBoard(ctx context.Context, showGen bool) ([]boardRow, error) {
	boardMemo.mu.Lock()
	if boardMemo.by == nil {
		boardMemo.by = map[bool]*boardMemoEntry{}
	}
	e := boardMemo.by[showGen]
	if e != nil && e.done == nil && clock.Now().Sub(e.at) < boardMemoTTL {
		rows := e.rows
		boardMemo.mu.Unlock()
		return rows, nil
	}
	if e != nil && e.done != nil {
		done := e.done
		boardMemo.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		boardMemo.mu.Lock()
		rows, err := e.rows, e.err
		boardMemo.mu.Unlock()
		return rows, err
	}
	e = &boardMemoEntry{done: make(chan struct{})}
	boardMemo.by[showGen] = e
	boardMemo.mu.Unlock()

	rows, err := m.botBoard(ctx, showGen)

	boardMemo.mu.Lock()
	e.rows, e.err, e.at = rows, err, clock.Now()
	close(e.done)
	e.done = nil
	if err != nil && boardMemo.by != nil && boardMemo.by[showGen] == e {
		// A failure is not cached: the next caller retries.
		delete(boardMemo.by, showGen)
	}
	boardMemo.mu.Unlock()
	return rows, err
}

func clearBoardMemo() {
	boardMemo.mu.Lock()
	boardMemo.by = nil
	boardMemo.mu.Unlock()
}

// botBoard builds the live line for every rostered rolebot. showGen matches the
// log filter beside it. Callers go through cachedBotBoard.
func (m *Mod) botBoard(ctx context.Context, showGen bool) ([]boardRow, error) {
	genFilter := "WHERE g.characterID IS NULL"
	if showGen {
		genFilter = ""
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT b.characterID, c.characterName, b.enabled,
		       COALESCE(sta.stationName, ''), COALESCE(ms.solarSystemName, ''),
		       COALESCE(t.typeID, 0), COALESCE(t.typeName, ''),
		       COALESCE(ship.itemID, 0), COALESCE(c.balance, 0)
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		LEFT JOIN staStations sta ON sta.stationID = c.stationID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = c.solarSystemID
		LEFT JOIN entity ce ON ce.itemID = c.characterID
		LEFT JOIN entity ship ON ship.itemID = ce.locationID AND ship.ownerID = c.characterID
		LEFT JOIN invTypes t ON t.typeID = ship.typeID
		LEFT JOIN rolebot_generated g ON g.characterID = b.characterID
		`+genFilter+`
		ORDER BY c.characterName`)
	if err != nil {
		return nil, fmt.Errorf("botBoard: %w", err)
	}
	defer rows.Close()

	var out []boardRow
	shipOf := map[int64]int64{}
	for rows.Next() {
		var br boardRow
		var en, shipID int64
		var balance float64
		var station, system string
		if err := rows.Scan(&br.CharID, &br.Name, &en, &station, &system,
			&br.ShipTypeID, &br.ShipName, &shipID, &balance); err != nil {
			return nil, fmt.Errorf("botBoard scan: %w", err)
		}
		br.Enabled = en != 0
		br.Docked = station != ""
		if station != "" {
			br.Where = "Docked — " + station
		} else if system != "" {
			br.Where = "In space — " + system
		} else {
			br.Where = "unknown"
		}
		if br.ShipName == "" {
			br.ShipName = "capsule"
		}
		br.Wallet = iskString(balance)
		shipOf[br.CharID] = shipID
		out = append(out, br)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Sim state per bot (the truth about what it is doing).
	for i := range out {
		st, err := loadState(ctx, m.db, out[i].CharID)
		if err != nil {
			return nil, err
		}
		br := &out[i]
		br.Activity = st.Activity
		if br.Activity == "" {
			br.Activity = "—"
		}
		br.Doing = phaseLabel(st)
		br.Detail = m.phaseDetail(ctx, st)
		br.Halted = st.Halted
		br.Error = st.LastError
		br.Dest = m.destLabel(ctx, st)
		br.Ore = oreLabel(st)
		br.Embodied = m.isEmbodied(br.CharID)
		br.Lasers = m.embodiedMining(br.CharID)
		switch {
		case br.Lasers:
			br.Presence = "on grid — lasers hot"
		case br.Embodied:
			br.Presence = "on grid"
		case br.Docked:
			br.Presence = "in station"
		default:
			br.Presence = "off grid"
		}
		// Hold fill is the number a miner is actually working toward; it
		// costs a couple of queries, so only miners pay for it.
		if st.Activity == ActMining && shipOf[br.CharID] != 0 {
			br.HoldPct, br.HoldText = m.holdGauge(ctx, br.CharID, shipOf[br.CharID])
		}
		switch {
		case !br.Enabled:
			br.Status, br.StatusText = "released", "released"
		case st.Halted:
			br.Status, br.StatusText = "halted", "HALTED"
		case !Master():
			br.Status, br.StatusText = "released", "master off"
		case st.Phase == "idle" || st.Phase == "":
			br.Status, br.StatusText = "idle", "idle"
		default:
			br.Status, br.StatusText = "working", "running"
		}
	}

	// Newest log line per bot.
	last := map[int64]struct {
		Msg   string
		Level string
		TS    int64
	}{}
	// Scoped to the board's own pilots, which is far cheaper than a whole-log GROUP BY.
	// With showGen the board is every pilot and the loose index scan wins instead.
	lastQ := `
		SELECT l.characterID, l.level, l.message, l.ts
		FROM (SELECT b.characterID, MAX(x.logID) AS mx
		        FROM rolebot_characters b
		        JOIN rolebot_log x ON x.characterID = b.characterID
		        LEFT JOIN rolebot_generated g ON g.characterID = b.characterID
		       WHERE g.characterID IS NULL
		       GROUP BY b.characterID) t
		JOIN rolebot_log l ON l.logID = t.mx`
	if showGen {
		lastQ = `
		SELECT l.characterID, l.level, l.message, l.ts
		FROM rolebot_log l
		JOIN (SELECT characterID, MAX(logID) AS mx FROM rolebot_log
		      WHERE characterID <> 0 GROUP BY characterID) t ON t.mx = l.logID`
	}
	lrows, err := m.db.QueryContext(ctx, capStatementFor(logStatementCap, lastQ))
	if err == nil {
		defer lrows.Close()
		for lrows.Next() {
			var id, ts int64
			var level, msg string
			if lrows.Scan(&id, &level, &msg, &ts) == nil {
				last[id] = struct {
					Msg   string
					Level string
					TS    int64
				}{msg, level, ts}
			}
		}
	}
	now := time.Now().Unix()
	for i := range out {
		if l, ok := last[out[i].CharID]; ok {
			out[i].LastMsg, out[i].LastLevel = l.Msg, l.Level
			out[i].LastAgo = agoString(now - l.TS)
			out[i].Updated = time.Unix(l.TS, 0).UTC().Format("15:04:05")
		} else {
			out[i].LastMsg, out[i].LastAgo = "nothing logged yet", ""
		}
	}
	return out, nil
}

// phaseDetail describes the current leg in the bot's own terms. Which description
// belongs to which task is declared on the task (TaskDef.Detail in task.go).
func (m *Mod) phaseDetail(ctx context.Context, st *SimState) string {
	var out string
	if d, ok := taskDef(st.Phase); ok && d.Detail != nil {
		out = d.Detail(m, ctx, st)
	}
	// A pilot goes free as soon as its manufacturing job is installed, so the line
	// above describes another trip. Say what is on the line regardless.
	if note := m.ovenNote(ctx, st); note != "" && !isIndustryPhase(st.Phase) {
		if out == "" {
			return note
		}
		return out + "; " + note
	}
	return out
}

// ovenNote is what this pilot has on manufacturing lines right now, in one clause;
// empty when nothing is cooking. It reads the cooking list (industry.go,
// IndCooking), not IndJobID, which is zero for almost the whole life of a job.
func (m *Mod) ovenNote(ctx context.Context, st *SimState) string {
	type oven struct {
		name  string
		left  int64
		ready bool
	}
	var ovens []oven
	add := func(jobID int64, name string, factoryID int64) {
		o := oven{name: name}
		if _, ready, endTime, err := m.industryJobStatus(ctx, jobID); err == nil {
			o.ready = ready
			if left := evedb.Win32ToUnix(endTime) - clock.Unix(); left > 0 {
				o.left = left
			}
		}
		ovens = append(ovens, o)
	}
	// A delivered job is off its line (industryOvenSweep): the goods wait in the
	// factory hangar for a listing, a different sentence from one still baking.
	listing := 0
	for _, j := range st.Data.IndCooking {
		if j.Delivered {
			listing++
			continue
		}
		add(j.JobID, j.TypeName, j.FactoryID)
	}
	if st.Data.IndJobID != 0 {
		add(st.Data.IndJobID, st.Data.IndTypeName, st.Data.IndFactoryID)
	}
	if len(ovens) == 0 {
		if listing > 0 {
			return fmt.Sprintf("%d finished job(s) in its hangar waiting to be listed", listing)
		}
		return ""
	}
	if len(ovens) == 1 {
		note := ovens[0].name + " in the oven"
		if id := factoryOf(st); id != 0 {
			note += " at " + m.stationLabel(ctx, id)
		}
		switch {
		case ovens[0].ready:
			note += " — ready to collect"
		case ovens[0].left > 0:
			note += " — ready in " + agoString(ovens[0].left)
		}
		if listing > 0 {
			note += fmt.Sprintf("; %d more waiting to be listed", listing)
		}
		return note
	}
	// Several lines running: the count and the next one to ring, because a
	// list of five product names is not a status line.
	readyNow, soonest, soonestName := 0, int64(0), ""
	for _, o := range ovens {
		if o.ready {
			readyNow++
			continue
		}
		if o.left > 0 && (soonest == 0 || o.left < soonest) {
			soonest, soonestName = o.left, o.name
		}
	}
	note := fmt.Sprintf("%d jobs on its lines", len(ovens))
	switch {
	case readyNow > 0:
		note += fmt.Sprintf(" — %d ready to collect", readyNow)
	case soonest > 0:
		note += fmt.Sprintf(" — %s in %s", soonestName, agoString(soonest))
	}
	if listing > 0 {
		note += fmt.Sprintf("; %d waiting to be listed", listing)
	}
	return note
}

// factoryOf is the factory a pilot's manufacturing is happening at, from
// whichever half of the state carries it.
func factoryOf(st *SimState) int64 {
	if st.Data.IndFactoryID != 0 {
		return st.Data.IndFactoryID
	}
	for _, j := range st.Data.IndCooking {
		if j.FactoryID != 0 {
			return j.FactoryID
		}
	}
	return 0
}

// ── task detail renderers ───────────────────────────────────────────────────
//
// Referenced from the registrations in task.go. Several tasks share one renderer
// (every procure task shows the outstanding shopping list).

func detailTravel(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	dest := ""
	if d.TargetStationID != 0 {
		dest = m.nameOfStation(ctx, d.TargetStationID)
	}
	if dest == "" && d.TargetSystemID != 0 {
		dest = m.nameOfSystem(ctx, d.TargetSystemID)
	}
	if dest == "" {
		return ""
	}
	return fmt.Sprintf("→ %s (%d jump(s) left)", dest, len(d.Path))
}

func detailMining(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	if m.embodiedMining(st.CharID) {
		return d.OreName + " — lasers firing on a real rock in space"
	}
	if d.OreName == "" {
		return ""
	}
	if d.LegEndsAt > time.Now().Unix() {
		return fmt.Sprintf("%s — hold full in %s", d.OreName,
			agoString(d.LegEndsAt-time.Now().Unix()))
	}
	return d.OreName
}

func detailOreHold(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.OreUnits > 0 {
		return fmt.Sprintf("%d× %s in the hold", st.Data.OreUnits, st.Data.OreName)
	}
	return ""
}

func detailTrade(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.TradeQty > 0 && st.Data.TradeName != "" {
		return fmt.Sprintf("%d× %s", st.Data.TradeQty, st.Data.TradeName)
	}
	return ""
}

func detailProcure(m *Mod, ctx context.Context, st *SimState) string {
	if len(st.Data.Shopping) == 0 {
		return ""
	}
	return "still to buy: " + shopSummary(st.Data.Shopping)
}

func detailIdle(m *Mod, ctx context.Context, st *SimState) string {
	// An idle bot with a job in the oven is not really idle: say so, with the ready
	// time, so the board explains why a manufacturer is off mining.
	if note := m.ovenNote(ctx, st); note != "" {
		note = "manufacturing in the background: " + note
		if st.Data.LegEndsAt > time.Now().Unix() {
			note += "; re-planning in " + agoString(st.Data.LegEndsAt-time.Now().Unix())
		}
		return note
	}
	// The planner's own reason for leaving it idle (sim.go), then the nap countdown.
	note := st.Data.IdleWhy
	if st.Data.LegEndsAt > time.Now().Unix() {
		if note != "" {
			note += "; "
		}
		note += "re-planning in " + agoString(st.Data.LegEndsAt-time.Now().Unix())
	}
	return note
}

// destLabel names where the current leg ends, for any phase that has a
// destination, not just the travel ones phaseDetail covers.
func (m *Mod) destLabel(ctx context.Context, st *SimState) string {
	if st == nil {
		return ""
	}
	if id := st.Data.TargetStationID; id != 0 {
		if s := m.nameOfStation(ctx, id); s != "" {
			return s
		}
	}
	if id := st.Data.TargetSystemID; id != 0 {
		if s := m.nameOfSystem(ctx, id); s != "" {
			return s
		}
	}
	return ""
}

// oreLabel is the ore this bot is working, with the load it is carrying.
func oreLabel(st *SimState) string {
	if st == nil || st.Data.OreName == "" {
		return ""
	}
	if st.Data.OreUnits > 0 {
		return fmt.Sprintf("%s — %d units", st.Data.OreName, st.Data.OreUnits)
	}
	return st.Data.OreName
}

// holdGauge reports the cargo hold as a percentage and a "used / capacity"
// caption. Returns 0, "" when the ship's capacity cannot be resolved.
func (m *Mod) holdGauge(ctx context.Context, charID, shipID int64) (int, string) {
	var hullTypeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, shipID).Scan(&hullTypeID); err != nil {
		return 0, ""
	}
	capacity, err := evedb.GetShipCargoCapacityFitted(ctx, m.db, hullTypeID, shipID)
	if err != nil || capacity <= 0 {
		return 0, ""
	}
	used, err := evedb.GetCargoUsedVolume(ctx, m.db, charID, shipID)
	if err != nil {
		return 0, ""
	}
	pct := int(used / capacity * 100)
	if pct > 100 {
		pct = 100
	}
	if pct < 0 {
		pct = 0
	}
	return pct, fmt.Sprintf("%.0f / %.0f m³", used, capacity)
}

// iskString renders a wallet balance the way the client does.
func iskString(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.2fb ISK", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%.2fm ISK", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk ISK", v/1e3)
	default:
		return fmt.Sprintf("%.0f ISK", v)
	}
}

func (m *Mod) nameOfStation(ctx context.Context, id int64) string {
	return m.stationNameCached(ctx, id)
}

func (m *Mod) nameOfSystem(ctx context.Context, id int64) string {
	return m.systemNameCached(ctx, id)
}

func agoString(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
	}
}

// ── /rolebots/log ───────────────────────────────────────────────────────────

const tmplLog = `{{define "content"}}
<style>
/* Two columns: the log reads down the left, the roster stands to the
   right. Each scrolls on its own so neither pushes the other off screen. */
.rb-cols { display:grid; grid-template-columns:minmax(0,1fr) 420px; gap:14px; align-items:start; }
@media (max-width:1280px) { .rb-cols { grid-template-columns:1fr; } }
.rb-cols > div { min-width:0; }
.rb-right { position:sticky; top:12px; }

.rb-live { display:flex; align-items:center; gap:10px; font-size:12px; color:#9ca3af; flex-wrap:wrap; }
.rb-dot { width:8px; height:8px; border-radius:50%; background:#22c55e; display:inline-block; }
.rb-dot.off { background:#6b7280; }
.rb-badge { display:inline-block; padding:2px 8px; border-radius:10px; font-size:11px; background:#382020; color:#9ca3af; white-space:nowrap; }
.rb-badge.working { background:#0f2a1f; color:#6ee7b7; }
.rb-badge.idle { background:#26211a; color:#d1a054; }
.rb-badge.halted { background:#3a1414; color:#fca5a5; }
.rb-err { font-size:11px; color:#fca5a5; }
.rb-err:empty { display:none; }

/* ── the log ───────────────────────────────────────────────────────────── */
/* One entry per row, every field in a fixed column, so the eye tracks
   straight down a category or a character instead of across a wide table. */
.rb-log { max-height:calc(100vh - 300px); overflow-y:auto; border-top:1px solid #2a1a1a; }
.rb-e { display:grid; grid-template-columns:66px 78px 130px minmax(0,1fr);
        gap:10px; align-items:baseline; padding:5px 8px 5px 6px;
        border-bottom:1px solid #1e1414; border-left:3px solid transparent;
        font-size:12px; line-height:1.45; }
.rb-e:hover { background:#1a1010; }
.rb-e.error { border-left-color:#ef4444; background:#241010; }
.rb-e .t { color:#6b7280; font-family:ui-monospace,Consolas,monospace; font-size:11px; }
.rb-e .who { color:#9ca3af; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.rb-e .who a { color:#9ca3af; }
.rb-e .msg { color:#d8d2cc; word-break:break-word; }
.rb-e.error .msg { color:#fca5a5; }
.rb-e.warn { border-left-color:#d97706; background:#1f1a10; }
.rb-e.warn .msg { color:#fcd34d; }
.rb-e.rb-new { animation:rbflash 1.6s ease-out; }
@keyframes rbflash { from { background:#123a25; } to { background:transparent; } }
/* Category chips, coloured so a whole class of call is findable at a glance. */
.rb-cat { display:inline-block; width:100%; text-align:center; padding:1px 0;
          border-radius:4px; font-size:10px; letter-spacing:.04em; text-transform:uppercase;
          background:#241a1a; color:#9ca3af; }
.rb-cat[data-cat="mining"]   { background:#10261c; color:#6ee7b7; }
.rb-cat[data-cat="market"]   { background:#26210f; color:#e0c068; }
.rb-cat[data-cat="procure"]  { background:#231a2c; color:#c4a5f5; }
.rb-cat[data-cat="fit"]      { background:#231a2c; color:#c4a5f5; }
.rb-cat[data-cat="travel"]   { background:#132433; color:#7fb8e6; }
.rb-cat[data-cat="embody"]   { background:#132433; color:#7fb8e6; }
.rb-cat[data-cat="sim"]      { background:#2a2016; color:#d1a054; }
.rb-cat[data-cat="presence"] { background:#1c2320; color:#9ec5b0; }

/* ── the roster ────────────────────────────────────────────────────────── */
.rb-bots { display:flex; flex-direction:column; gap:10px;
           max-height:calc(100vh - 220px); overflow-y:auto; }
.rb-bot { border:1px solid #2a1a1a; border-radius:6px; padding:10px; background:#180f0f; }
.rb-bot.halted { border-color:#5b2020; background:#1f1010; }
.rb-bot .hd { display:flex; gap:10px; align-items:center; margin-bottom:8px; }
.rb-bot .hd img { width:40px; height:40px; border-radius:4px; background:#170a0a; flex:0 0 auto; }
.rb-bot .who { flex:1; min-width:0; }
.rb-bot .who a { font-weight:600; }
.rb-bot .doing { font-size:13px; color:#e6ded6; margin:0; }
.rb-bot .detail { font-size:11px; color:#9ca3af; margin:2px 0 0; }
.rb-bot .detail:empty { display:none; }
.rb-facts { display:grid; grid-template-columns:64px minmax(0,1fr); gap:2px 8px;
            margin:8px 0 0; font-size:11px; }
.rb-facts dt { color:#6b7280; }
.rb-facts dd { margin:0; color:#c9c1ba; overflow-wrap:anywhere; }
.rb-facts .off { color:#6b7280; }
.rb-hold { margin-top:8px; }
.rb-hold .bar { height:6px; border-radius:3px; background:#2a1a1a; overflow:hidden; }
.rb-hold .bar i { display:block; height:100%; background:#6ee7b7; transition:width .4s ease; }
.rb-hold .cap { font-size:10px; color:#6b7280; margin-top:3px; }
.rb-last { font-size:11px; color:#9ca3af; margin:8px 0 0; border-top:1px solid #241616; padding-top:6px; }
.rb-last .ago { color:#6b7280; }
.rb-bot .acts { margin-top:8px; display:flex; gap:6px; }
.rb-bot .acts button { font-size:11px; padding:3px 10px; }
</style>

<h2>Rolebot Activity Log</h2>

{{if .Msg}}<div class="alert alert-success">{{.Msg}}</div>{{end}}
{{if .Err}}<div class="alert alert-error">{{.Err}}</div>{{end}}
{{if .HaltedCount}}<div class="alert alert-error">{{.HaltedCount}} rolebot(s) are HALTED — they drop the trip and re-plan on their own within {{.HaltRetryMin}} min, so this is a fault to read rather than one to clear. Reboot to bring them back now; see the board below.
  <form method="POST" action="/rolebots/log" style="display:inline;margin-left:8px">
    <input type="hidden" name="action" value="rebootall">
    <button type="submit">Reboot all halted</button>
  </form>
</div>{{end}}

<div class="rb-cols">
<div>

<div class="card">
  <div class="rb-live" style="margin-bottom:8px">
    <label><input type="checkbox" id="rbLive" checked> <span class="rb-dot" id="rbDot"></span> live</label>
    <span id="rbLiveNote">polling every 3s</span>
    <form method="POST" action="/rolebots/log" style="margin-left:auto"
          onsubmit="return confirm('Delete {{if .HasFilter}}the {{.MatchCount}} entries matching the current filter{{else}}every log entry{{end}}? This cannot be undone.')">
      <input type="hidden" name="action" value="clear">
      {{if .HasFilter}}
      <input type="hidden" name="level" value="{{.Level}}">
      <input type="hidden" name="char" value="{{.CharSel}}">
      <input type="hidden" name="cat" value="{{.Cat}}">
      <input type="hidden" name="q" value="{{.Query}}">
      <button type="submit">Clear {{.MatchCount}} filtered</button>
      {{else}}
      <button type="submit">Clear log</button>
      {{end}}
    </form>
  </div>
  <form class="search-form" method="GET" action="/rolebots/log">
    <select name="level">
      <option value="">any level</option>
      <option value="info" {{if eq .Level "info"}}selected{{end}}>info</option>
      <option value="warn" {{if eq .Level "warn"}}selected{{end}}>warn</option>
      <option value="error" {{if eq .Level "error"}}selected{{end}}>error</option>
    </select>
    <select name="char">
      <option value="0">any character</option>
      {{range .Chars}}<option value="{{.ID}}" {{if eq .ID $.CharSel}}selected{{end}}>{{.Name}}</option>{{end}}
    </select>
    <select name="cat">
      <option value="">any category</option>
      {{range .Cats}}<option value="{{.}}" {{if eq . $.Cat}}selected{{end}}>{{.}}</option>{{end}}
    </select>
    <input type="text" name="q" placeholder="message contains..." value="{{.Query}}">
    {{if .ShowGen}}<input type="hidden" name="gen" value="1">{{end}}
    <button type="submit">Filter</button>
    {{if .HasFilter}}<a href="/rolebots/log" style="font-size:12px;align-self:center">reset</a>{{end}}
  </form>
  <p style="font-size:12px;color:#6b7280;margin:6px 0 0">
    {{if .ShowGen}}Showing the generated population as well.
      <a href="/rolebots/log"><strong>Hide it</strong></a>
    {{else}}Generated pilots are hidden — this log is about the bots you built yourself.
      <a href="/rolebots/log?gen=1"><strong>Include them</strong></a>{{end}}
    &middot; picking a single character above always shows that character, generated or not.
    &middot; a free-text search covers the newest {{.QueryMaxRows}} lines{{if .QueryFloorTime}} (back to {{.QueryFloorTime}}){{end}} unless a character is selected.
  </p>

  <div class="rb-log" id="rbLogRows">
    {{range .Rows}}
    <div class="rb-e {{if eq .Level "error"}}error{{else if eq .Level "warn"}}warn{{end}}">
      <span class="t" title="{{.Full}}">{{.When}}</span>
      <span><span class="rb-cat" data-cat="{{.Category}}">{{.Category}}</span></span>
      <span class="who">{{if .CharID}}<a href="/rolebots/log?char={{.CharID}}" title="only this character">{{if .CharName}}{{.CharName}}{{else}}{{.CharID}}{{end}}</a>{{else}}—{{end}}</span>
      <span class="msg">{{.Message}}</span>
    </div>
    {{else}}<div class="rb-e"><span class="msg" style="grid-column:1/-1">Nothing logged yet (or nothing matches the filter).</span></div>{{end}}
  </div>
  <p style="font-size:11px;color:#6b7280;margin-top:8px">Last {{.Limit}} entries, newest first. Click a character to filter to it. Entries older than {{.Retention}} days are pruned automatically; the live tail honors the filters.</p>
</div>

</div>
<div class="rb-right">

<div class="card">
  <h3>Active RoleBots</h3>
  <p style="font-size:11px;color:#6b7280">What the sim has each one doing, where it is, and whether it is a real ship on someone's grid. A halted bot has hit a dead end and does nothing until rebooted.</p>
  {{if .Board}}
  <div class="rb-bots" id="rbBoardRows">
    {{range .Board}}
    <article class="rb-bot{{if .Halted}} halted{{end}}" data-char="{{.CharID}}">
      <div class="hd">
        <img src="/portrait/{{.CharID}}?size=64" alt="" onerror="this.style.visibility='hidden'">
        <div class="who">
          <a href="/rolebot/{{.CharID}}">{{.Name}}</a>
          <div><span class="rb-badge {{.Status}}" data-f="statusText">{{.StatusText}}</span></div>
        </div>
      </div>
      <p class="doing" data-f="doing">{{.Doing}}</p>
      <p class="detail" data-f="detail">{{.Detail}}</p>
      <div class="rb-err" data-f="error">{{.Error}}</div>

      <div class="rb-hold" data-f="holdBox" {{if not .HoldText}}style="display:none"{{end}}>
        <div class="bar"><i data-f="holdBar" data-pct="{{.HoldPct}}"></i></div>
        <div class="cap"><span data-f="holdText">{{.HoldText}}</span> in the hold</div>
      </div>

      <dl class="rb-facts">
        <dt>Doing</dt><dd data-f="activity">{{.Activity}}</dd>
        <dt>Presence</dt><dd data-f="presence">{{.Presence}}</dd>
        <dt>Where</dt><dd data-f="where">{{.Where}}</dd>
        <dt>Flying</dt><dd>
          <img src="/typeicon/{{.ShipTypeID}}.png" width="18" height="18" data-f="shipIcon"
               style="vertical-align:-4px;border-radius:3px;background:#170a0a" onerror="this.style.visibility='hidden'">
          <span data-f="shipName">{{.ShipName}}</span></dd>
        <dt data-l="dest" {{if not .Dest}}style="display:none"{{end}}>Heading</dt>
        <dd data-f="dest" {{if not .Dest}}style="display:none"{{end}}>{{.Dest}}</dd>
        <dt data-l="ore" {{if not .Ore}}style="display:none"{{end}}>Ore</dt>
        <dd data-f="ore" {{if not .Ore}}style="display:none"{{end}}>{{.Ore}}</dd>
        <dt>Wallet</dt><dd data-f="wallet">{{.Wallet}}</dd>
      </dl>

      <p class="rb-last"><span data-f="lastMsg">{{.LastMsg}}</span> <span class="ago" data-f="lastAgo">{{.LastAgo}}</span></p>
      <div class="acts">
        <form method="POST" action="/rolebots/log">
          <input type="hidden" name="action" value="reboot">
          <input type="hidden" name="charID" value="{{.CharID}}">
          <button type="submit">Reboot</button>
        </form>
        <a href="/rolebots/log?char={{.CharID}}" style="font-size:11px;align-self:center">its log</a>
      </div>
    </article>
    {{end}}
  </div>
  {{else}}
  <p>No characters on the roster. Add one on the <a href="/rolebots">Roster</a> page.</p>
  {{end}}
</div>

</div>
</div>

<script>
(function () {
  var lastID = {{.MaxLogID}};
  var query = {{.APIQuery}};
  var live = document.getElementById('rbLive');
  var dot = document.getElementById('rbDot');
  var note = document.getElementById('rbLiveNote');
  var logRows = document.getElementById('rbLogRows');
  var boardRows = document.getElementById('rbBoardRows');

  // Server-rendered hold bars carry their fill as data-pct: an inline width
  // would have to survive the panel's CSS-context escaping, and this does
  // not. Live updates set the width directly from then on.
  Array.prototype.forEach.call(
    document.querySelectorAll('[data-f="holdBar"]'),
    function (bar) { bar.style.width = (bar.getAttribute('data-pct') || 0) + '%'; });

  function span(cls, text) {
    var el = document.createElement('span');
    if (cls) el.className = cls;
    if (text !== undefined) el.textContent = text;
    return el;
  }

  function addLogRow(r) {
    var row = document.createElement('div');
    row.className = 'rb-e rb-new' + (r.level === 'error' ? ' error' : r.level === 'warn' ? ' warn' : '');
    var stamp = span('t', r.when);
    stamp.title = r.full || '';
    row.appendChild(stamp);

    var catWrap = span('', undefined);
    var cat = span('rb-cat', r.category);
    cat.setAttribute('data-cat', r.category || '');
    catWrap.appendChild(cat);
    row.appendChild(catWrap);

    var who = span('who');
    if (r.charID) {
      var a = document.createElement('a');
      a.href = '/rolebots/log?char=' + r.charID;
      a.textContent = r.charName || String(r.charID);
      who.appendChild(a);
    } else {
      who.textContent = '—';
    }
    row.appendChild(who);
    row.appendChild(span('msg', r.message));

    var first = logRows.firstChild;
    // Drop the "nothing logged yet" placeholder on the first real row.
    if (first && first.querySelector && first.querySelector('.t') === null) {
      logRows.innerHTML = '';
      first = null;
    }
    logRows.insertBefore(row, first);
    while (logRows.children.length > 300) logRows.removeChild(logRows.lastChild);
  }

  function paintBoard(bots) {
    bots.forEach(function (b) {
      var card = boardRows ? boardRows.querySelector('[data-char="' + b.charID + '"]') : null;
      if (!card) { window.location.reload(); return; }
      function set(f, v) {
        var el = card.querySelector('[data-f="' + f + '"]');
        if (el && el.textContent !== v) el.textContent = v;
      }
      set('statusText', b.statusText);
      set('doing', b.doing);
      set('activity', b.activity);
      set('detail', b.detail);
      set('presence', b.presence);
      set('where', b.where);
      set('shipName', b.shipName);
      // Heading and Ore are only facts while they hold a value — the label
      // hides with them so the list never shows an empty row.
      function pair(f, v) {
        set(f, v);
        var dd = card.querySelector('[data-f="' + f + '"]');
        var dt = card.querySelector('[data-l="' + f + '"]');
        if (dd) dd.style.display = v ? '' : 'none';
        if (dt) dt.style.display = v ? '' : 'none';
      }
      pair('dest', b.dest);
      pair('ore', b.ore);
      set('wallet', b.wallet);
      set('holdText', b.holdText);
      set('lastMsg', b.lastMsg);
      set('lastAgo', b.lastAgo);
      set('error', b.error);

      var badge = card.querySelector('[data-f="statusText"]');
      if (badge) badge.className = 'rb-badge ' + b.status;
      card.className = 'rb-bot' + (b.halted ? ' halted' : '');

      // A hold gauge only exists while there is a hold worth watching.
      var box = card.querySelector('[data-f="holdBox"]');
      if (box) box.style.display = b.holdText ? '' : 'none';
      var bar = card.querySelector('[data-f="holdBar"]');
      if (bar) bar.style.width = (b.holdPct || 0) + '%';

      var icon = card.querySelector('[data-f="shipIcon"]');
      if (icon && icon.getAttribute('src') !== '/typeicon/' + b.shipTypeID + '.png') {
        icon.style.visibility = 'visible';
        icon.setAttribute('src', '/typeicon/' + b.shipTypeID + '.png');
      }
    });
    if (boardRows && boardRows.children.length !== bots.length) window.location.reload();
  }

  // The board is the expensive half of a poll (a state load per bot), so the
  // tail asks for it every boardEveryNthPoll polls and takes rows alone the
  // rest of the time; the first poll always carries it.
  var boardEveryNthPoll = 4, pollNo = 0;
  function poll() {
    if (!live.checked || document.hidden) return;
    var wantBoard = (pollNo++ % boardEveryNthPoll) === 0;
    fetch('/rolebots/log/api?after=' + lastID + (wantBoard ? '&board=1' : '') + (query ? '&' + query : ''), {credentials: 'same-origin'})
      .then(function (r) { return r.json(); })
      .then(function (data) {
        (data.rows || []).slice().reverse().forEach(function (r) {
          if (r.id > lastID) lastID = r.id;
          addLogRow(r);
        });
        if (data.board) paintBoard(data.board);
        note.textContent = 'updated ' + (data.now || '');
        dot.className = 'rb-dot';
      })
      .catch(function () { dot.className = 'rb-dot off'; });
  }
  live.addEventListener('change', function () {
    dot.className = live.checked ? 'rb-dot' : 'rb-dot off';
    note.textContent = live.checked ? 'polling every 3s' : 'paused';
  });
  setInterval(poll, 3000);
})();
</script>
{{end}}`

type logRow struct {
	ID int64 `json:"id"`
	// When is the time alone: the column stays narrow, and the full stamp rides
	// along in Full for the row's tooltip.
	When     string `json:"when"`
	Full     string `json:"full"`
	CharID   int64  `json:"charID"`
	CharName string `json:"charName"`
	Level    string `json:"level"`
	Category string `json:"category"`
	Message  string `json:"message"`
}

// logFilter is the set of filters shared by the page and the live tail.
type logFilter struct {
	Level   string
	Cat     string
	Query   string
	CharSel int64
	After   int64 // only entries newer than this logID (live tail)
	// QueryFloor is the logID floor a free-text search runs down to, set by
	// boundQuery; 0 means unbounded (no term, or a character is selected).
	QueryFloor int64
	// ShowGen includes the generated population's lines. Off by default: generated
	// pilots produce far more traffic than the operator's own bots and would bury
	// them. Same switch as the roster (esp.go), ?gen=1.
	ShowGen bool
}

func logFilterFrom(r *http.Request) logFilter {
	q := r.URL.Query()
	charSel, _ := strconv.ParseInt(q.Get("char"), 10, 64)
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	return logFilter{
		Level:   q.Get("level"),
		Cat:     q.Get("cat"),
		Query:   q.Get("q"),
		CharSel: charSel,
		After:   after,
		ShowGen: q.Get("gen") == "1",
	}
}

// asQuery re-encodes the filters for the live-tail URL.
func (f logFilter) asQuery() string {
	var parts []string
	if f.Level != "" {
		parts = append(parts, "level="+f.Level)
	}
	if f.Cat != "" {
		parts = append(parts, "cat="+f.Cat)
	}
	if f.CharSel != 0 {
		parts = append(parts, "char="+strconv.FormatInt(f.CharSel, 10))
	}
	if f.Query != "" {
		parts = append(parts, "q="+f.Query)
	}
	if f.ShowGen {
		parts = append(parts, "gen=1")
	}
	return strings.Join(parts, "&")
}

// any reports whether the filter narrows anything (the live-tail cursor aside).
func (f logFilter) any() bool {
	return f.Level != "" || f.Cat != "" || f.Query != "" || f.CharSel != 0
}

// where builds the shared SQL predicate. alias is the rolebot_log table alias
// ("l." on the joined read query, "" on the bare DELETE/COUNT).
func (f logFilter) where(alias string) (string, []any) {
	where := "1=1"
	var args []any
	if f.Level == logInfo || f.Level == logWarn || f.Level == logError {
		where += " AND " + alias + "level = ?"
		args = append(args, f.Level)
	}
	if f.CharSel != 0 {
		where += " AND " + alias + "characterID = ?"
		args = append(args, f.CharSel)
	}
	if f.Cat != "" {
		where += " AND " + alias + "category = ?"
		args = append(args, f.Cat)
	}
	if f.Query != "" {
		where += " AND " + alias + "message LIKE CONCAT('%', ?, '%')"
		args = append(args, f.Query)
		// A free-text term has no index, so it is bounded to the newest logQueryMaxRows
		// lines unless a character is selected; where() keeps the page and Clear in step.
		if f.QueryFloor > 0 {
			where += " AND " + alias + "logID >= ?"
			args = append(args, f.QueryFloor)
		}
	}
	if f.After != 0 {
		where += " AND " + alias + "logID > ?"
		args = append(args, f.After)
	}
	// An explicit character selection always wins: showing nothing for a generated
	// pilot would read as a broken log.
	if !f.ShowGen && f.CharSel == 0 {
		// Keep the correlated column qualified: unqualified, it resolves to the subquery's
		// own table (true for every row), which hides every line and makes Clear delete all.
		col := alias + "characterID"
		if alias == "" {
			col = "rolebot_log.characterID"
		}
		where += " AND NOT EXISTS (SELECT 1 FROM rolebot_generated g WHERE g.characterID = " + col + ")"
	}
	return where, args
}

// logCategories is every category the module writes, for the filter's dropdown. A
// fixed set avoids a full-scan DISTINCT on each page load; logCategoryOptions
// unions in anything written in the last hour.
var logCategories = []string{
	"ceo", "chatter", "clash", "clone", "combat", "conscript", "corp", "courier", "drones",
	"embody", "escrow", "fit", "gang", "grace", "home", "industry", "lab", "logi",
	"market", "mining", "module", "piracy", "presence", "procure",
	"repair", "research", "residency", "roster", "security", "sim", "skills",
	"standings", "threat", "trade", "wallet",
}

// logCategoryOptions is the dropdown: the static list plus any category the
// last hour of the log carries that the list does not know.
func (m *Mod) logCategoryOptions(ctx context.Context) []string {
	seen := make(map[string]bool, len(logCategories)+8)
	out := make([]string, 0, len(logCategories)+8)
	for _, c := range logCategories {
		seen[c] = true
		out = append(out, c)
	}
	rows, err := m.db.QueryContext(ctx, capStatementFor(logStatementCap,
		`SELECT DISTINCT category FROM rolebot_log WHERE ts > ?`),
		clock.Unix()-3600)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var c string
			if rows.Scan(&c) == nil && c != "" && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}

// boundQuery sets the filter's QueryFloor for a free-text search and returns
// the time of the oldest line the search reaches (zero when unbounded), for
// the page to say so. Two primary-key point reads.
func (m *Mod) boundQuery(ctx context.Context, f *logFilter) time.Time {
	f.QueryFloor = 0
	if f.Query == "" || f.CharSel != 0 {
		return time.Time{}
	}
	var maxID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(logID), 0) FROM rolebot_log`).Scan(&maxID); err != nil {
		return time.Time{}
	}
	floor := maxID - logQueryMaxRows
	if floor < 1 {
		return time.Time{}
	}
	f.QueryFloor = floor
	var ts int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT ts FROM rolebot_log WHERE logID >= ? ORDER BY logID LIMIT 1`, floor).Scan(&ts); err != nil {
		return time.Time{}
	}
	return time.Unix(ts, 0).UTC()
}

// countLog is how many entries the filter currently matches (for the Clear
// button's label). Only called with a filter set: unfiltered it is a slow
// full-index count.
func (m *Mod) countLog(ctx context.Context, f logFilter) int64 {
	where, args := f.where("")
	var n int64
	_ = m.db.QueryRowContext(ctx, capStatementFor(logStatementCap,
		`SELECT COUNT(*) FROM rolebot_log WHERE `+where), args...).Scan(&n)
	return n
}

func (m *Mod) queryLog(ctx context.Context, f logFilter, limit int) ([]logRow, error) {
	where, args := f.where("l.")
	args = append(args, limit)

	rows, err := m.db.QueryContext(ctx, capStatementFor(logStatementCap, `
		SELECT l.logID, l.ts, l.characterID, COALESCE(c.characterName, ''), l.level, l.category, l.message
		FROM rolebot_log l
		LEFT JOIN character_ c ON c.characterID = l.characterID
		WHERE `+where+`
		ORDER BY l.logID DESC LIMIT ?`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []logRow
	for rows.Next() {
		var lr logRow
		var ts int64
		if err := rows.Scan(&lr.ID, &ts, &lr.CharID, &lr.CharName, &lr.Level, &lr.Category, &lr.Message); err != nil {
			return nil, err
		}
		t := time.Unix(ts, 0).UTC()
		lr.When = t.Format("15:04:05")
		lr.Full = t.Format("2006-01-02 15:04:05") + " UTC"
		out = append(out, lr)
	}
	return out, rows.Err()
}

func (m *Mod) handleLog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), logHandlerTimeout)
	defer cancel()

	if r.Method == http.MethodPost {
		m.handleLogPost(w, r, ctx)
		return
	}

	f := logFilterFrom(r)
	floorAt := m.boundQuery(ctx, &f)
	floorText := ""
	if !floorAt.IsZero() {
		floorText = floorAt.Format("2006-01-02 15:04") + " UTC"
	}
	const limit = 300
	out, err := m.queryLog(ctx, f, limit)
	if err != nil {
		espError(w, "rolebot-log", err.Error())
		return
	}
	var maxLogID int64
	if len(out) > 0 {
		maxLogID = out[0].ID
	} else {
		_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(logID), 0) FROM rolebot_log`).Scan(&maxLogID)
	}

	board, err := m.cachedBotBoard(ctx, f.ShowGen)
	if err != nil {
		espError(w, "rolebot-log", err.Error())
		return
	}
	halted := 0
	for _, b := range board {
		if b.Halted {
			halted++
		}
	}

	// Filter option sources.
	chars, _ := listNamed(ctx, m.db, `
		SELECT c.characterID, c.characterName
		FROM rolebot_characters b JOIN character_ c ON c.characterID = b.characterID
		ORDER BY c.characterName`)
	cats := m.logCategoryOptions(ctx)
	var matched int64
	if f.any() {
		matched = m.countLog(ctx, f)
	}

	espRender(w, "rolebots_log.html", map[string]any{
		"Page": "rolebot-log",
		"Rows": out, "Chars": chars, "Cats": cats,
		"Level": f.Level, "CharSel": f.CharSel, "Cat": f.Cat, "Query": f.Query,
		"Limit": limit, "Retention": logRetentionDays,
		"HasFilter": f.any(), "MatchCount": matched,
		"QueryMaxRows": fmtUnits(logQueryMaxRows), "QueryFloorTime": floorText,
		"Board": board, "HaltedCount": halted, "HaltRetryMin": haltRetrySec / 60,
		"ShowGen":  f.ShowGen,
		"MaxLogID": maxLogID, "APIQuery": f.asQuery(),
		"Msg": r.URL.Query().Get("msg"),
		"Err": r.URL.Query().Get("err"),
	})
}

// handleLogPost serves the Reboot buttons on the bot board.
func (m *Mod) handleLogPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	switch r.FormValue("action") {
	case "reboot":
		charID, _ := strconv.ParseInt(r.FormValue("charID"), 10, 64)
		if charID == 0 {
			redirect(w, r, "/rolebots/log", "", "invalid character ID")
			return
		}
		if err := m.rebootAndLog(ctx, charID); err != nil {
			redirect(w, r, "/rolebots/log", "", err.Error())
			return
		}
		redirect(w, r, "/rolebots/log", fmt.Sprintf("rolebot %d rebooted — it re-plans on the next sim pass", charID), "")
	case "rebootall":
		ids, err := haltedBots(ctx, m.db)
		if err != nil {
			redirect(w, r, "/rolebots/log", "", err.Error())
			return
		}
		for _, id := range ids {
			if err := m.rebootAndLog(ctx, id); err != nil {
				redirect(w, r, "/rolebots/log", "", err.Error())
				return
			}
		}
		redirect(w, r, "/rolebots/log", fmt.Sprintf("%d halted rolebot(s) rebooted", len(ids)), "")
	case "clear":
		// Posted filters (empty when the button cleared everything).
		charSel, _ := strconv.ParseInt(r.FormValue("char"), 10, 64)
		f := logFilter{
			Level:   r.FormValue("level"),
			Cat:     r.FormValue("cat"),
			Query:   r.FormValue("q"),
			CharSel: charSel,
		}
		// Same floor as the page that showed the count, so the delete
		// cannot reach lines the search never showed.
		m.boundQuery(ctx, &f)
		where, args := f.where("")
		res, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_log WHERE `+where, args...)
		if err != nil {
			redirect(w, r, "/rolebots/log", "", err.Error())
			return
		}
		n, _ := res.RowsAffected()
		what := "log cleared"
		if f.any() {
			what = "filtered entries cleared"
		}
		// Keep the operator's filters on the way back, alongside the notice.
		q := url.Values{}
		if f.Level != "" {
			q.Set("level", f.Level)
		}
		if f.Cat != "" {
			q.Set("cat", f.Cat)
		}
		if f.Query != "" {
			q.Set("q", f.Query)
		}
		if f.CharSel != 0 {
			q.Set("char", strconv.FormatInt(f.CharSel, 10))
		}
		q.Set("msg", fmt.Sprintf("%s — %d entr(ies) deleted", what, n))
		http.Redirect(w, r, "/rolebots/log?"+q.Encode(), http.StatusSeeOther)
	default:
		redirect(w, r, "/rolebots/log", "", "unknown action")
	}
}

// rebootAndLog resets one bot's sim state and records it in the log.
func (m *Mod) rebootAndLog(ctx context.Context, charID int64) error {
	if err := resetBot(ctx, m.db, charID); err != nil {
		return err
	}
	// The next poll must show the reset, not a memoised "halted".
	clearBoardMemo()
	m.logBot(charID, logInfo, "sim", "rebooted from the panel — state cleared, re-planning from scratch")
	return nil
}

// handleLogAPI is the live tail: new entries since ?after=<logID> and, with
// board=1, the current bot board, as JSON. The board is the expensive half, so the
// page asks for it every fourth poll (boardEveryNthPoll) and the server memoises
// it for boardMemoTTL. The row tail is one short primary-key range.
func (m *Mod) handleLogAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), logHandlerTimeout)
	defer cancel()

	f := logFilterFrom(r)
	m.boundQuery(ctx, &f)
	rows, err := m.queryLog(ctx, f, 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var board []boardRow
	if r.URL.Query().Get("board") == "1" {
		board, err = m.cachedBotBoard(ctx, f.ShowGen)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"rows":  rows,
		"board": board,
		"now":   time.Now().UTC().Format("15:04:05 UTC"),
	})
}

func init() { registerCharForget("charNames", func(id int64) { charNames.Delete(id) }) }
