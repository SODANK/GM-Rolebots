package rolebots

// The chokepoint panel: why the projects are not moving.
//
// Almost every refusal in the industry lane is recorded only as prose in rolebot_log (the one
// machine-readable state, IndBookFailAt, is a timestamp with no reason), at hundreds of thousands
// of lines a day. The panel folds them in SQL, collapsing every run of digits to '#' so station
// IDs, counts and ISK figures stop making each line a distinct key. A Go table of prefixes then
// maps each folded prefix to a named chokepoint. Unmatched prefixes stay as their own folded rows
// in a tail; never drop it, since it is how an unnamed failure mode shows up.

import (
	"context"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

// The categories worth folding. The lab lane is absent: this page is the manufacturing pipeline,
// and lab work runs on a separate slot track.
var industryChokeCats = []string{"industry", "procure", "market"}

// industryChokeWindow is one selectable lookback.
type industryChokeWindow struct {
	Key   string
	Label string
	Secs  int64
}

// The default is the short window: the fold is a full scan whose cost scales with it, an hour
// answers what is wrong now, and wider windows are one click away with their cost stated.
var industryChokeWindows = []industryChokeWindow{
	{"1h", "last hour", 3600},
	{"6h", "last 6 hours", 6 * 3600},
	{"24h", "last 24 hours", 24 * 3600},
}

func industryChokeWindowFor(key string) industryChokeWindow {
	for _, w := range industryChokeWindows {
		if w.Key == key {
			return w
		}
	}
	return industryChokeWindows[0]
}

// Kinds. A refusal and a progress line are both worth counting but are different facts: a mineral
// purchase at volume is a pipeline working, no seller in reach at volume is one stopped. Churn is
// neither: the same thing repeating, as a base flap or a station that never has stock.
const (
	chokeBlock = "block"
	chokeChurn = "churn"
	chokeWork  = "work"
	chokeOther = "other"
)

// industryChokeRule maps a folded prefix onto a named chokepoint. Cat "" matches any category. Rules match in
// order, first wins, so a specific rule goes before the general one. Prefixes are ASCII and stop short of the
// long dash the log prose uses as a clause separator, to avoid depending on the console encoding.
type industryChokeRule struct {
	Cat    string
	Prefix string
	Name   string
	Kind   string
	Note   string
	// Sub matches anywhere in the folded text instead of at the front. It exists for families a prefix
	// cannot reach, such as mineral listing lines that begin with the quantity and item name. A
	// substring rule is more aggressive, so use it only where a prefix cannot work; order still decides.
	Sub string
}

var industryChokeRules = []industryChokeRule{
	// --- industry: the project cannot proceed ---
	{Cat: "industry", Prefix: "nothing in its own region or its neighbours", Name: "Materials on sale nowhere in reach", Kind: chokeBlock, Note: "a bill line cannot be filled within reach"},
	{Cat: "industry", Prefix: "nothing anywhere in the market", Name: "Materials on sale in no region", Kind: chokeBlock, Note: "a bill line is listed in no region"},
	{Cat: "industry", Prefix: "the # outstanding material line(s) cost", Name: "Cannot afford the outstanding lines", Kind: chokeBlock, Note: "the wallet cannot cover the rest of the bill"},
	{Cat: "industry", Prefix: "the mining trip would not start", Name: "Mining trip refused", Kind: chokeBlock, Note: "no usable mining fit, or no winnable belt in reach"},
	{Cat: "industry", Prefix: "still short of", Name: "Short of a line nothing in reach sells", Kind: chokeBlock, Note: "one item short and the market has none"},
	{Cat: "industry", Prefix: "nothing in its build book it can build", Name: "Cold build book", Kind: chokeBlock, Note: "every candidate refused, activity on cooldown"},
	{Cat: "industry", Prefix: "passed over hull(s): ", Name: "Hulls passed over for something else", Kind: chokeBlock, Note: "a destroyer-or-larger candidate was refused while a smaller build went ahead"},
	{Cat: "industry", Prefix: "install came up short", Name: "Install refused, materials short", Kind: chokeBlock, Note: "reached the line and the bill did not match"},
	{Cat: "industry", Prefix: "project for", Name: "Project abandoned on the stall clock", Kind: chokeBlock, Note: "no progress in a day, sunk ISK written off"},
	{Cat: "industry", Prefix: "it has no hull here to carry materials in", Name: "No hauler at the factory", Kind: chokeBlock, Note: "nothing at this station to carry the bill"},
	{Cat: "industry", Prefix: "bootstrap: no Industrial hull listed at the", Name: "No industrial hull at the factory", Kind: chokeBlock, Note: "none on sale here, so a hauler was moved to the front of the book"},
	// --- industry: it is repeating itself ---
	{Cat: "industry", Prefix: "moving its industry base from", Name: "Factory base moved", Kind: chokeChurn, Note: "many from one pilot means flapping between two stations"},
	// --- industry: it is getting on with it ---
	{Cat: "industry", Prefix: "short # of # material line(s)", Name: "Lines the belts cannot cover", Kind: chokeWork, Note: "shopping for the part no rock yields"},
	{Cat: "industry", Prefix: "would rather dig its own", Name: "Preferred to mine, buying instead", Kind: chokeWork, Note: "wanted to dig, but no reachable belt yields it"},
	{Cat: "industry", Prefix: "buying #", Name: "Buying missing material lines", Kind: chokeWork},
	{Cat: "industry", Prefix: "bought # of # outstanding mineral line(s)", Name: "Bought outstanding mineral lines", Kind: chokeWork},
	{Cat: "industry", Prefix: "bought the", Name: "Blueprint bought", Kind: chokeWork},
	{Cat: "industry", Prefix: "reserving an attempt for its own", Name: "Own hull print pinned", Kind: chokeWork, Note: "a hull above frigate size, built from a print it holds"},
	{Cat: "industry", Prefix: "sizing ", Name: "Hull batch sized to the wallet", Kind: chokeWork, Note: "hull batch trimmed to what the pilot can afford"},
	{Cat: "industry", Prefix: "refined #", Name: "Refined ore into minerals", Kind: chokeWork},
	{Cat: "industry", Prefix: "installed", Name: "Job installed", Kind: chokeWork},
	{Cat: "industry", Prefix: "build book at", Name: "Read the build book", Kind: chokeWork, Note: "per-pass summary of what the factory considered"},

	// --- procure: the trip cannot finish ---
	{Cat: "procure", Prefix: "abandoning procurement: nothing in its own r", Name: "Procurement abandoned, nothing in reach", Kind: chokeBlock, Note: "a fit line cannot be filled within reach"},
	{Cat: "procure", Prefix: "abandoning procurement: nothing anywhere", Name: "Procurement abandoned, on sale in no region", Kind: chokeBlock, Note: "a fit hull or line is listed in no region"},
	{Cat: "procure", Prefix: "abandoning procurement: nothing on the list", Name: "Procurement abandoned, this station has none", Kind: chokeBlock, Note: "the station stocks nothing on the list"},
	{Cat: "procure", Prefix: "abandoning procurement: cannot afford", Name: "Procurement abandoned, cannot afford", Kind: chokeBlock, Note: "on sale, but the wallet cannot cover it"},
	{Cat: "procure", Prefix: "nothing anywhere in the market sells", Name: "Nothing in the game sells it", Kind: chokeBlock, Note: "no ask exists anywhere, so the line is dropped"},
	{Cat: "procure", Prefix: "giving up on the rest of the shopping (canno", Name: "Shopping cut short, cannot afford", Kind: chokeBlock},
	{Cat: "procure", Prefix: "giving up on the rest of the shopping", Name: "Shopping cut short, nothing sells it", Kind: chokeBlock},
	{Cat: "procure", Prefix: "stopping the shopping trip", Name: "Shopping trip stopped", Kind: chokeBlock},
	{Cat: "procure", Prefix: "could not finish the", Name: "Trip cut short", Kind: chokeBlock, Note: "a leg of the trip could not be supplied"},
	// --- procure: it is repeating itself ---
	{Cat: "procure", Prefix: "bought nothing at", Name: "Flew to a station and bought nothing", Kind: chokeChurn, Note: "nothing on the list was in stock on arrival"},
	{Cat: "procure", Prefix: "ship is short-fitted but still does the job", Name: "Flying short-fitted", Kind: chokeChurn, Note: "hull went to work with gaps in its fit"},
	{Cat: "procure", Prefix: "refusing to fly half-fitted", Name: "Refused to fly half-fitted", Kind: chokeChurn, Note: "combat hull kept in dock because its fit is too incomplete"},
	// --- procure: it is getting on with it ---
	{Cat: "procure", Prefix: "bought + installed", Name: "Fit line bought and installed", Kind: chokeWork},
	{Cat: "procure", Prefix: "starting procurement of fit", Name: "Procurement started", Kind: chokeWork},
	{Cat: "procure", Prefix: "ship # is missing # fit line(s)", Name: "Fit lines still missing", Kind: chokeWork},
	{Cat: "procure", Prefix: "# item(s) still missing", Name: "Items still missing on the trip", Kind: chokeWork},
	{Cat: "procure", Prefix: "slot # held item", Name: "Slot cleared for a refit", Kind: chokeWork},
	{Cat: "procure", Prefix: "flying current ship home to station", Name: "Taking its ship home", Kind: chokeWork},
	{Cat: "procure", Prefix: "left ship at home station", Name: "Left its ship at home", Kind: chokeWork},
	{Cat: "procure", Prefix: "ship complete (entity", Name: "Ship complete", Kind: chokeWork},

	// --- market ---
	{Cat: "market", Prefix: "listing refused", Name: "Listing refused", Kind: chokeBlock, Note: "refused by the broker fee or order cap, goods stay in the hangar"},
	// The mineral listing lane, the supply half of the pipeline. All four match on a substring
	// because the line opens with the quantity and the mineral's name.
	{Cat: "market", Name: "Repriced a mineral listing", Kind: chokeWork, Note: "price lowered to stay cheapest in reach", Sub: "off its listing at"},
	{Cat: "market", Name: "Listed a part lot", Kind: chokeWork, Note: "short everywhere, so a partial lot was listed", Sub: "short of a full lot"},
	{Cat: "market", Name: "Mineral stock nobody is buying", Kind: chokeChurn, Note: "sat too long to be worth an order slot", Sub: "has sat"},
	{Cat: "market", Name: "Stray minerals swept onto the market", Kind: chokeWork, Sub: "minerals were piling up in the hangar"},
	{Cat: "market", Prefix: "listed #", Name: "Listed a mineral lot", Kind: chokeWork, Note: "priced against station competition"},
	{Cat: "market", Name: "Sold a mineral into a standing bid", Kind: chokeWork, Sub: "off its standing bid at"},
	{Cat: "market", Prefix: "tidied the hangar", Name: "Hangar tidied", Kind: chokeWork},
}

// indChokeRow is one named (or unnamed) reason, folded out of the log.
type indChokeRow struct {
	Category string
	Name     string
	Note     string
	Kind     string
	Lines    int
	Pilots   int
	Named    bool
}

func (c indChokeRow) LinesStr() string  { return fmtUnits(int64(c.Lines)) }
func (c indChokeRow) PilotsStr() string { return fmtUnits(int64(c.Pilots)) }

// PerPilot separates a population from a spin. Six thousand lines from six
// hundred pilots is a handful of bots looping; eight thousand from three
// thousand is the roster doing a normal thing. The ratio is the reading and
// neither count alone gives it.
func (c indChokeRow) PerPilot() string {
	if c.Pilots == 0 {
		return "—"
	}
	return fmtUnits(int64(c.Lines / c.Pilots))
}

// industryChokes folds the log over one window and names what it can. It returns the rows, the
// line total for the window and a soft-failure string: this panel is enrichment, so a shard whose
// log table lacks REGEXP_REPLACE loses the card, not the page.
func (m *Mod) industryChokes(ctx context.Context, winSecs int64) ([]indChokeRow, int, string) {
	ph := make([]string, 0, len(industryChokeCats))
	args := make([]any, 0, len(industryChokeCats)+1)
	args = append(args, clock.Unix()-winSecs)
	for _, c := range industryChokeCats {
		ph = append(ph, "?")
		args = append(args, c)
	}
	rows, err := m.db.QueryContext(ctx, capStatementFor(industryChokeBuildTimeout-5*time.Second, `
		SELECT category,
		       REGEXP_REPLACE(LEFT(message, 44), '[0-9]+', '#') AS m,
		       COUNT(*), COUNT(DISTINCT characterID)
		  FROM rolebot_log
		 WHERE ts > ? AND category IN (`+strings.Join(ph, ",")+`)
		 GROUP BY category, m`), args...)
	if err != nil {
		m.logBot(0, logError, "industry", "industry chokepoints: %v", err)
		return nil, 0, err.Error()
	}
	defer rows.Close()

	// Named rules merge; unmatched prefixes stay separate. The merge is keyed on the rule, so every
	// base move is one row however the station names differ.
	type agg struct {
		row indChokeRow
		// A pilot count cannot be summed across folded keys without double-counting anyone who hit two,
		// so the merged figure is the largest contributing key. It understates, the safe direction.
		maxPilots int
	}
	merged := map[string]*agg{}
	var order []string
	total := 0

	for rows.Next() {
		var cat, folded string
		var lines, pilots int
		if err := rows.Scan(&cat, &folded, &lines, &pilots); err != nil {
			continue
		}
		total += lines
		rule, ok := matchChokeRule(cat, folded)
		key := cat + "\x00" + folded
		row := indChokeRow{Category: cat, Name: strings.TrimSpace(folded),
			Kind: chokeOther, Named: false}
		if ok {
			key = cat + "\x00rule\x00" + rule.Name
			row = indChokeRow{Category: cat, Name: rule.Name, Note: rule.Note,
				Kind: rule.Kind, Named: true}
		}
		a, seen := merged[key]
		if !seen {
			a = &agg{row: row}
			merged[key] = a
			order = append(order, key)
		}
		a.row.Lines += lines
		if pilots > a.maxPilots {
			a.maxPilots = pilots
		}
	}
	if err := rows.Err(); err != nil {
		return nil, total, err.Error()
	}

	out := make([]indChokeRow, 0, len(order))
	for _, k := range order {
		a := merged[k]
		a.row.Pilots = a.maxPilots
		out = append(out, a.row)
	}
	// Blocked first, then churn, then the unnamed tail, then plain progress. Within a band, loudest first.
	sort.SliceStable(out, func(i, j int) bool {
		ki, kj := chokeKindRank(out[i].Kind), chokeKindRank(out[j].Kind)
		if ki != kj {
			return ki < kj
		}
		return out[i].Lines > out[j].Lines
	})
	if len(out) > industryChokeRows {
		out = out[:industryChokeRows]
	}
	return out, total, ""
}

// ---------------------------------------------------------------------------
// The memo: stale-while-revalidate
// ---------------------------------------------------------------------------

// industryChokeMemoTTL is how long a fold is served before a fresh one starts, in seconds. The
// fold is a slow full scan that the board's own memo (industryMemoTTL) does not cover. Five
// minutes: the panel answers what is wrong now, which does not move in that time.
const industryChokeMemoTTL = 300

// industryChokeBuildTimeout bounds one background fold; the statement is
// capped a little under it so a fold that runs long dies on the server
// rather than outliving its context.
const industryChokeBuildTimeout = 90 * time.Second

type chokeMemoEntry struct {
	rows     []indChokeRow
	lines    int
	err      string
	builtAt  int64
	building bool
}

var chokeMemo struct {
	mu    sync.Mutex
	byWin map[string]*chokeMemoEntry
}

// chokeBoard is the request-path read, and the request never waits on a
// fold. A memo past its TTL is served as it stands while a goroutine rebuilds
// it; the very first request for a window (no memo at all) starts the build
// and tells the page so. One build per window at a time.
func (m *Mod) chokeBoard(win industryChokeWindow) (rows []indChokeRow, lines int, errStr string, builtAt int64, building bool) {
	chokeMemo.mu.Lock()
	defer chokeMemo.mu.Unlock()
	if chokeMemo.byWin == nil {
		chokeMemo.byWin = map[string]*chokeMemoEntry{}
	}
	e := chokeMemo.byWin[win.Key]
	if e == nil {
		e = &chokeMemoEntry{}
		chokeMemo.byWin[win.Key] = e
	}
	if !e.building && (e.builtAt == 0 || clock.Unix()-e.builtAt >= industryChokeMemoTTL) {
		e.building = true
		go m.refreshChokes(win)
	}
	return e.rows, e.lines, e.err, e.builtAt, e.building
}

// refreshChokes runs one fold off the request path and stores it. A failed
// rebuild keeps the last good fold rather than blanking the card; the next
// request past the TTL tries again.
func (m *Mod) refreshChokes(win industryChokeWindow) {
	var rows []indChokeRow
	var lines int
	var errStr string
	func() {
		defer func() {
			if r := recover(); r != nil {
				errStr = "chokepoint fold panicked (see server log)"
				rlogf("industry chokes %s: panic: %v\n%s", win.Key, r, debug.Stack())
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), industryChokeBuildTimeout)
		defer cancel()
		rows, lines, errStr = m.industryChokes(ctx, win.Secs)
	}()

	chokeMemo.mu.Lock()
	defer chokeMemo.mu.Unlock()
	if chokeMemo.byWin == nil {
		chokeMemo.byWin = map[string]*chokeMemoEntry{}
	}
	e := chokeMemo.byWin[win.Key]
	if e == nil {
		e = &chokeMemoEntry{}
		chokeMemo.byWin[win.Key] = e
	}
	e.building = false
	if errStr != "" && e.builtAt != 0 {
		return
	}
	e.rows, e.lines, e.err, e.builtAt = rows, lines, errStr, clock.Unix()
}

// clearChokeMemo drops every window's fold (the roster-purge paths, through
// clearIndustryMemo).
func clearChokeMemo() {
	chokeMemo.mu.Lock()
	chokeMemo.byWin = nil
	chokeMemo.mu.Unlock()
}

func chokeKindRank(kind string) int {
	switch kind {
	case chokeBlock:
		return 0
	case chokeChurn:
		return 1
	case chokeOther:
		return 2
	}
	return 3
}

// matchChokeRule finds the first rule whose prefix the folded text carries.
func matchChokeRule(cat, folded string) (industryChokeRule, bool) {
	for _, r := range industryChokeRules {
		if r.Cat != "" && r.Cat != cat {
			continue
		}
		if r.Sub != "" {
			if strings.Contains(folded, r.Sub) {
				return r, true
			}
			continue
		}
		if strings.HasPrefix(folded, r.Prefix) {
			return r, true
		}
	}
	return industryChokeRule{}, false
}
