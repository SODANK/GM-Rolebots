package rolebots

// The module's own console window.
//
// Rolebots log a line for every leg of every bot's day, which would drown the server's output.
// A Windows process owns one console, so the server re-executes itself with `--rolebot-console`
// (caught in init() before main()); that process is a viewer painting lines read from a pipe.
//
// The writer never blocks its caller (logBot is called inline from the sim): lines go to a
// buffered channel and are dropped, with a count, when it fills. Any failure falls back to the
// main console.

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// consoleFlag re-enters this binary as the log viewer. It is checked in init(), so it works
// whatever the server's own flag parsing does.
const consoleFlag = "--rolebot-console"

// Wire format, one line per entry, tab separated: <unix>\t<level>\t<category>\t<who>\t<message>.
// Tabs and newlines are stripped from the message so a multi-line error cannot desynchronise
// the reader.
const (
	lvlInfo = "info"
	lvlWarn = "warn" // a refusal the pilot met, not a fault (log.go logWarn)
	lvlErr  = "error"
	lvlSys  = "sys" // module diagnostics: the old bare log.Printf calls
	// lvlBye is the server's goodbye, the last line before the pipe closes. It tells the viewer a
	// shutdown from a crash, since both arrive as EOF.
	lvlBye = "bye"
)

func init() {
	for _, a := range os.Args[1:] {
		if a == consoleFlag {
			runConsoleViewer(os.Stdin)
			os.Exit(0)
		}
	}
}

// ── the server side ─────────────────────────────────────────────────────────

// consoleSink is the module's log destination. Until startConsole succeeds it is nil and
// everything goes to the main console, which is also the fallback for every failure.
type consoleSink struct {
	lines   chan string
	mirror  bool // also echo to the main server console
	dropped atomic.Int64
	once    sync.Once

	// stop is closed by shutdown; flushed is closed by pump once the queue is written and the
	// pipe closed, so the server can wait for its last lines. lines is never closed: enqueue is
	// called inline from many goroutines and a send on a closed channel panics.
	stop    chan struct{}
	flushed chan struct{}
}

var sink atomic.Pointer[consoleSink]

// consoleQueue is generous because a sim pass over a large roster bursts.
const consoleQueue = 8192

// startConsole opens the module's log window. Failure is not fatal: the module logs to the
// server console.
func (m *Mod) startConsole() {
	if !m.cfg.Console {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		log.Printf("rolebots: no separate log window (executable path: %v) — logging to this console", err)
		return
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		log.Printf("rolebots: no separate log window (pipe: %v) — logging to this console", err)
		return
	}
	cmd := exec.Command(exe, consoleFlag)
	cmd.Stdin = pr
	cmd.SysProcAttr = newConsoleSysProcAttr()
	if cmd.SysProcAttr == nil {
		// Not Windows, or no way to ask for a new console. A second window is a Windows idea.
		pr.Close()
		pw.Close()
		return
	}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		log.Printf("rolebots: no separate log window (spawn: %v) — logging to this console", err)
		return
	}
	pr.Close() // the child owns the read end now

	s := &consoleSink{
		lines:   make(chan string, consoleQueue),
		mirror:  m.cfg.ConsoleMirror,
		stop:    make(chan struct{}),
		flushed: make(chan struct{}),
	}
	sink.Store(s)
	go s.pump(pw, cmd)
	s.watchShutdown()

	log.Printf("rolebots: activity log opened in its own console window (pid %d) — "+
		"this console stays quiet%s", cmd.Process.Pid,
		map[bool]string{true: " (mirroring on)", false: ""}[s.mirror])
}

// watchShutdown says goodbye in the window before the server goes. There is no module
// shutdown hook: main() handles SIGINT and returns, taking the pipe with it. Signal delivery is
// broadcast to every channel, so listening here does not take the interrupt from main.
func (s *consoleSink) watchShutdown() {
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		sg := <-sigc
		s.shutdown(fmt.Sprintf("the server is shutting down (%v) — end of log", sg))
	}()
}

// shutdown flushes the queue into the window, closes the pipe and waits briefly. The wait is
// bounded because a wedged viewer must not block the server's exit.
func (s *consoleSink) shutdown(reason string) {
	s.once.Do(func() {
		s.enqueue(entryLine(lvlBye, "console", reason))
		close(s.stop)
	})
	select {
	case <-s.flushed:
	case <-time.After(3 * time.Second):
	}
	// The window is closed: anything the module still says during shutdown goes to the server
	// console, since a line written into a dead pipe is never seen.
	sink.CompareAndSwap(s, nil)
}

// pump drains the queue into the child. It is the only writer to the pipe, so a wedged viewer
// costs one goroutine.
func (s *consoleSink) pump(pw *os.File, cmd *exec.Cmd) {
	defer func() {
		// Closing the write end tells the viewer the server is done and ends its read loop. flushed
		// closes last, so a waiting shutdown knows the window has everything.
		pw.Close()
		close(s.flushed)
		_ = cmd.Wait()
	}()
	// Report the drop count into the window once a second, whenever it moves.
	var reported int64
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-tick.C:
				if d := s.dropped.Load(); d != reported {
					n := d - reported
					reported = d
					s.enqueue(entryLine(lvlErr, "console",
						fmt.Sprintf("%d log line(s) dropped — the window was not keeping up", n)))
				}
			case <-s.stop:
				return
			}
		}
	}()
	write := func(line string) bool {
		if _, err := io.WriteString(pw, line); err != nil {
			// The viewer is gone: say so on the main console and put the module's output back there.
			sink.Store(nil)
			log.Printf("rolebots: log window closed (%v) — logging to this console again", err)
			return false
		}
		return true
	}
	for {
		select {
		case line := <-s.lines:
			if !write(line) {
				return
			}
		case <-s.stop:
			// Everything already queued still belongs in the window: they are the last lines the bots
			// wrote. The drain does not block, since nothing unbuffered can be waiting.
			for {
				select {
				case line := <-s.lines:
					if !write(line) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

// enqueue never blocks. A full queue means the viewer stopped reading, and the sim must not
// wait for a window.
func (s *consoleSink) enqueue(line string) {
	select {
	case s.lines <- line:
	default:
		s.dropped.Add(1)
	}
}

// entryLine renders one wire line.
func entryLine(level, category, msg string) string {
	return entryLineWho(level, category, "", msg)
}

func entryLineWho(level, category, who, msg string) string {
	clean := func(s string) string {
		return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(s)
	}
	return strconv.FormatInt(time.Now().Unix(), 10) + "\t" +
		level + "\t" + clean(category) + "\t" + clean(who) + "\t" + clean(msg) + "\n"
}

// emit routes one entry to the module window, or to the server console when there is none.
func emit(level, category, who, msg string) {
	s := sink.Load()
	if s == nil {
		log.Printf("rolebots: [%s/%s] %s%s", level, category, whoPrefix(who), msg)
		return
	}
	s.enqueue(entryLineWho(level, category, who, msg))
	if s.mirror {
		log.Printf("rolebots: [%s/%s] %s%s", level, category, whoPrefix(who), msg)
	}
}

func whoPrefix(who string) string {
	if who == "" {
		return ""
	}
	return who + ": "
}

// rlogf replaces log.Printf in this module, so its diagnostics (failed queries, dropped work)
// land in the module's window.
func rlogf(format string, args ...any) {
	msg := strings.TrimPrefix(fmt.Sprintf(format, args...), "rolebots: ")
	emit(lvlSys, "module", "", msg)
}

// ── the viewer side ─────────────────────────────────────────────────────────

// ANSI colours. The viewer turns virtual-terminal processing on before printing
// (console_windows.go); where that fails the escapes are harmless on Windows 10 1909+.
const (
	cReset = "\x1b[0m"
	cDim   = "\x1b[38;5;244m"
	cWhite = "\x1b[38;5;252m"
	cRed   = "\x1b[38;5;203m"
	cAmber = "\x1b[38;5;179m"
	cGreen = "\x1b[38;5;114m"
	cBlue  = "\x1b[38;5;110m"
	cViol  = "\x1b[38;5;140m"
)

// catColour keeps a class of call findable by colour, using the /rolebots/log panel's families.
var catColour = map[string]string{
	"mining":   cGreen,
	"market":   cAmber,
	"procure":  cViol,
	"fit":      cViol,
	"travel":   cBlue,
	"embody":   cBlue,
	"sim":      cAmber,
	"presence": cGreen,
	"combat":   cRed,
	"module":   cDim,
	"console":  cDim,
}

// runConsoleViewer is the whole child process: read wire lines, paint them.
func runConsoleViewer(in *os.File) {
	out := viewerOutput()
	prepareViewerConsole(out)

	fmt.Fprintf(out, "%s%s%s\n", cGreen, strings.Repeat("─", 78), cReset)
	fmt.Fprintf(out, "%s  RoleBots — activity log%s   (this window belongs to the server; "+
		"closing it is safe)\n", cWhite, cReset)
	fmt.Fprintf(out, "%s%s%s\n", cGreen, strings.Repeat("─", 78), cReset)

	st := &viewerStats{start: time.Now()}
	rd := newLineReader(in)
	for {
		line, err := rd.read()
		if err != nil {
			break
		}
		fmt.Fprint(out, renderEntry(line, st))
	}
	fmt.Fprint(out, st.report())
	time.Sleep(viewerLinger)
}

// viewerLinger is how long the window stays up after the server has gone. The closing report
// is the only record that survives a restart, so it must stay readable.
const viewerLinger = 5 * time.Minute

// viewerStats is the session tally the closing report is built from. It lives in the viewer,
// not the server, because a killed server cannot summarise.
type viewerStats struct {
	start   time.Time
	total   int
	byLevel map[string]int
	byCat   map[string]int
	clean   bool   // the server said goodbye (lvlBye) rather than just going
	lastErr string // painted form of the most recent error line
}

func (v *viewerStats) count(level, cat string, rendered string) {
	if v.byLevel == nil {
		v.byLevel, v.byCat = map[string]int{}, map[string]int{}
	}
	v.total++
	v.byLevel[level]++
	v.byCat[cat]++
	switch level {
	case lvlErr:
		v.lastErr = strings.TrimRight(rendered, "\r\n")
	case lvlBye:
		v.clean = true
	}
}

// report is what the window shows once the pipe closes.
func (v *viewerStats) report() string {
	var b strings.Builder
	rule := strings.Repeat("─", 78)

	head, headCol := "the server shut down — end of log", cGreen
	if !v.clean {
		// No goodbye line: the process died without unwinding (killed or crashed).
		head, headCol = "the log ended WITHOUT a shutdown notice — the server was killed or crashed", cRed
	}
	fmt.Fprintf(&b, "\n%s%s%s\n", headCol, rule, cReset)
	fmt.Fprintf(&b, "%s  %s%s\n", headCol, head, cReset)
	fmt.Fprintf(&b, "%s  ran %s · %d line(s) logged%s\n",
		cDim, roundDur(time.Since(v.start)), v.total, cReset)
	if v.total > 0 {
		fmt.Fprintf(&b, "%s  %s%s\n", cDim, tally(v.byLevel, 6), cReset)
		fmt.Fprintf(&b, "%s  busiest: %s%s\n", cDim, tally(v.byCat, 6), cReset)
	}
	if v.lastErr != "" {
		fmt.Fprintf(&b, "%s  last error:%s\n  %s\n", cDim, cReset, v.lastErr)
	}
	fmt.Fprintf(&b, "%s%s%s\n", headCol, rule, cReset)
	fmt.Fprintf(&b, "%s  this window closes on its own in %s — or close it now, "+
		"nothing depends on it.%s\n", cDim, roundDur(viewerLinger), cReset)
	return b.String()
}

// tally renders the top n counters, biggest first.
func tally(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(m))
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	// Count descending, then name, so ties read the same on every run.
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	var parts []string
	for i, p := range pairs {
		if i == n {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(pairs)-n))
			break
		}
		parts = append(parts, fmt.Sprintf("%d %s", p.v, p.k))
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, " · ")
}

func roundDur(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute).String()
	case d >= time.Minute:
		return d.Round(time.Second).String()
	default:
		return d.Round(100 * time.Millisecond).String()
	}
}

// renderEntry turns one wire line into one painted console line and tallies it for the
// closing report.
func renderEntry(line string, st *viewerStats) string {
	line = strings.TrimRight(line, "\r\n")
	f := strings.SplitN(line, "\t", 5)
	if len(f) < 5 {
		// Anything that is not ours passes through untouched, since an unexpected write to this pipe
		// is a bug worth seeing.
		return line + "\n"
	}
	ts, _ := strconv.ParseInt(f[0], 10, 64)
	level, cat, who, msg := f[1], f[2], f[3], f[4]

	stamp := time.Unix(ts, 0).Format("15:04:05")
	col := catColour[cat]
	if col == "" {
		col = cDim
	}
	msgCol := cWhite
	switch level {
	case lvlErr:
		msgCol = cRed
	case lvlWarn, lvlSys:
		msgCol = cAmber
	case lvlBye:
		msgCol = cGreen
	}
	out := fmt.Sprintf("%s%s%s %s%-9s%s %s%-18s%s %s%s%s\n",
		cDim, stamp, cReset,
		col, trunc(cat, 9), cReset,
		cDim, trunc(who, 18), cReset,
		msgCol, msg, cReset)
	if st != nil {
		st.count(level, cat, out)
	}
	return out
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// lineReader reads newline-terminated records without bufio's line-length limits: a very long
// line is truncated, never fatal.
type lineReader struct {
	f    *os.File
	rest []byte
}

func newLineReader(f *os.File) *lineReader { return &lineReader{f: f} }

const maxLine = 1 << 16

func (r *lineReader) read() (string, error) {
	for {
		if i := indexByte(r.rest, '\n'); i >= 0 {
			line := string(r.rest[:i+1])
			r.rest = r.rest[i+1:]
			return line, nil
		}
		if len(r.rest) > maxLine {
			line := string(r.rest[:maxLine]) + "\n"
			r.rest = r.rest[maxLine:]
			return line, nil
		}
		chunk := make([]byte, 32*1024)
		n, err := r.f.Read(chunk)
		if n > 0 {
			r.rest = append(r.rest, chunk[:n]...)
			continue
		}
		if err != nil {
			return "", err
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
