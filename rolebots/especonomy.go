package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Economy overview: /rolebots/economy.
//
// The landing page of the Economy section: is the bot economy inflating or deflating, and through
// which doors. Everything is read from the ISK-flow ledger (iskflow.go), which the sweep fills
// forward once a minute. It has a KPI strip and panels on a grid; each panel is its own query that
// soft-fails into a banner naming it, and the page is memoised for a minute (?refresh=1 bypasses).
// The memo is single-flight per key, so the hub page and the two ledgers share it and a slow
// build does not block other readers.

// ── windows ─────────────────────────────────────────────────────────────────

// econWindow is a page's look-back: how far, and how it is bucketed.
type econWindow struct {
	Key, Label string
	Secs       int64
	Buckets    int
	BucketSecs int64
}

var econWindows = []econWindow{
	{Key: "24h", Label: "24 hours", Secs: 86400, Buckets: 24, BucketSecs: 3600},
	{Key: "7d", Label: "7 days", Secs: 7 * 86400, Buckets: 28, BucketSecs: 6 * 3600},
	{Key: "30d", Label: "30 days", Secs: 30 * 86400, Buckets: 30, BucketSecs: 86400},
}

// econWindowByKey resolves a key; anything unknown is the first (24h).
func econWindowByKey(key string) econWindow {
	for _, w := range econWindows {
		if w.Key == key {
			return w
		}
	}
	return econWindows[0]
}

func econWindowFrom(r *http.Request) econWindow {
	return econWindowByKey(r.URL.Query().Get("w"))
}

// econWindowPick is one entry of the picker the pages draw.
type econWindowPick struct {
	Key, Label, Href string
	On               bool
}

// econWindowPicks builds the picker for a page path, keeping extra query
// parameters (a system page's id, a feed's filter) on every link.
func econWindowPicks(path string, extra string, cur econWindow) []econWindowPick {
	out := make([]econWindowPick, 0, len(econWindows))
	for _, w := range econWindows {
		href := path + "?w=" + w.Key
		if extra != "" {
			href += "&" + extra
		}
		out = append(out, econWindowPick{Key: w.Key, Label: w.Label, Href: href, On: w.Key == cur.Key})
	}
	return out
}

// ── the memo ────────────────────────────────────────────────────────────────

const econMemoTTL = 60 // seconds

type econMemoEntry struct {
	mu      sync.Mutex
	v       any
	builtAt int64
}

var economyMemo = struct {
	mu sync.Mutex
	by map[string]*econMemoEntry
}{by: map[string]*econMemoEntry{}}

func econMemoEntryFor(key string) *econMemoEntry {
	economyMemo.mu.Lock()
	defer economyMemo.mu.Unlock()
	e := economyMemo.by[key]
	if e == nil {
		e = &econMemoEntry{}
		economyMemo.by[key] = e
	}
	return e
}

// econMemoGet serves key from the memo, building it when stale or when fresh is set. A failed
// build hands back the previous value with the error; with nothing to fall back on, the zero
// value and the error come back together.
func econMemoGet[T any](key string, fresh bool, build func() (T, error)) (T, int64, error) {
	e := econMemoEntryFor(key)
	e.mu.Lock()
	defer e.mu.Unlock()
	now := clock.Unix()
	if !fresh && e.v != nil && now-e.builtAt < econMemoTTL {
		return e.v.(T), e.builtAt, nil
	}
	v, err := build()
	if err != nil {
		if e.v != nil {
			return e.v.(T), e.builtAt, err
		}
		var zero T
		return zero, 0, err
	}
	e.v, e.builtAt = v, now
	return v, now, nil
}

// clearEconomyMemo drops every Economy page's memo. Called by the ISK-flow
// sweep after a pass and by the purges.
func clearEconomyMemo() {
	economyMemo.mu.Lock()
	economyMemo.by = map[string]*econMemoEntry{}
	economyMemo.mu.Unlock()
}

// ── shapes ──────────────────────────────────────────────────────────────────

// flowRow is one line of the ledger over the window, dressed for the page.
type flowRow struct {
	Key, Label, Dir, Desc string
	// In is the credits, Out the debits, Net their difference. All are positive magnitudes except
	// Net, which carries its sign in NetV.
	In, Out, Net string
	InV, OutV    float64
	NetV         float64
	// Magnitude is what the panel ranks and bars: the whole flow for a
	// faucet or a sink, the volume moved for a transfer.
	Magnitude float64
	N         int64
	Pct       string // share of its direction's total
	Bar       int    // 0..100 against the biggest line of its direction
	BotISK    string // the pilots' share of the magnitude
	CorpISK   string // the corporation wallets' share
}

// catRow is one item category's NPC trade over the window.
type catRow struct {
	CategoryID     int64
	Name           string
	Buy, Sell, Net string
	BuyV, SellV    float64
	NetV           float64
	Units, N       int64
	Bar            int // against the biggest |net|
	NetUp          bool
}

// histCol is one bucket of the faucet-vs-sink chart.
type histCol struct {
	Label             string
	FaucetH, SinkH    int // 0..100
	Faucet, Sink, Net string
	FaucetV, SinkV    float64
	Up                bool
}

// corpRow is one corporation's capture of the faucet over the window.
type corpRow struct {
	CorpID       int64
	Name         string
	Net, In, Out string
	NetV         float64
	TopLine      string
	Bar          int
	Up           bool
}

// catalogueRow is one touch point with its window figures beside it.
type catalogueRow struct {
	iskFlowLine
	In, Out string
	N       int64
	Has     bool
}

// econStats is the KPI strip.
type econStats struct {
	Faucet, Sink, Net         string
	FaucetV, SinkV, NetV      float64
	NetUp                     bool
	TransferBot, TransferCorp string
	FromPlayers, ToPlayers    string
	BotWallets, CorpWallets   string
	BotCount, CorpCount       int64
	LedgerSince               string
	Partial                   bool
	Behind, LastSweep         string
	Cursor, Head              int64
	MarketCursor, MarketHead  int64
	Lag, MarketLag            int64
	Bootstrapped              bool
}

// econOverview is everything the page draws.
type econOverview struct {
	Window     econWindow
	Stats      econStats
	Faucets    []flowRow
	Sinks      []flowRow
	Transfers  []flowRow
	Categories []catRow
	Goods      econGoods
	Wallets    walletBoard
	Hist       []histCol
	Corps      []corpRow
	Catalogue  []catalogueRow
	Warnings   []string
	BuiltAt    int64
}

// hourFlow is one (hour, line) cell, the histogram's input.
type hourFlow struct {
	Hour   int64
	Line   string
	ISK    float64
	Inflow float64
}

// ── the build ───────────────────────────────────────────────────────────────

func (m *Mod) economyOverview(ctx context.Context, win econWindow, fresh bool) (*econOverview, int64) {
	o, at, err := econMemoGet("overview:"+win.Key, fresh, func() (*econOverview, error) {
		return m.buildEconomyOverview(ctx, win), nil
	})
	if err != nil && o == nil {
		o = &econOverview{Window: win, Warnings: []string{err.Error()}}
	}
	return o, at
}

// buildEconomyOverview never fails: every panel is its own query and a
// failure becomes a warning naming the panel, with the panel left empty.
func (m *Mod) buildEconomyOverview(ctx context.Context, win econWindow) *econOverview {
	o := &econOverview{Window: win, BuiltAt: clock.Unix()}
	warn := func(panel string, err error) {
		o.Warnings = append(o.Warnings, fmt.Sprintf("%s: %v", panel, err))
	}
	// lap logs a panel that took over a second to read, so a slow page names its culprit.
	lapAt := clock.Now()
	lap := func(panel string) {
		if d := clock.Now().Sub(lapAt); d > time.Second {
			rlogf("rolebots: economy page: %s took %s", panel, d.Round(time.Millisecond))
		}
		lapAt = clock.Now()
	}
	now := clock.Unix()
	sinceHour := (now - win.Secs) / 3600
	sinceDay := (now - win.Secs) / 86400

	// The ledger over the window, by actor and line.
	cells, err := m.econFlowCells(ctx, sinceHour)
	if err != nil {
		warn("flows", err)
	}
	o.Faucets, o.Sinks, o.Transfers, o.Stats = foldFlowCells(cells)
	o.Catalogue = buildCatalogue(cells)

	lap("flows")
	if st, err := m.readISKFlowStatus(ctx); err != nil {
		warn("ledger health", err)
	} else {
		o.Stats.Cursor, o.Stats.Head = st.Cursor, st.Head
		o.Stats.MarketCursor, o.Stats.MarketHead = st.MarketCursor, st.MarketHead
		o.Stats.Lag, o.Stats.MarketLag = st.Head-st.Cursor, st.MarketHead-st.MarketCursor
		o.Stats.Bootstrapped = st.BootstrappedAt > 0
		if st.BootstrappedAt > 0 {
			o.Stats.LedgerSince = time.Unix(st.BootstrappedAt, 0).UTC().Format("2006-01-02 15:04 UTC")
			o.Stats.Partial = now-win.Secs < st.BootstrappedAt
		} else {
			o.Stats.LedgerSince = "not yet — the first sweep runs within a minute of boot"
			o.Stats.Partial = true
		}
		switch {
		case st.BehindSecs > 0:
			o.Stats.Behind = agoString(st.BehindSecs) + " behind"
		case st.Head > st.Cursor:
			o.Stats.Behind = fmt.Sprintf("%d rows unread", st.Head-st.Cursor)
		default:
			o.Stats.Behind = "caught up"
		}
		if st.LastSweepAt > 0 {
			o.Stats.LastSweep = agoString(now-st.LastSweepAt) + " ago"
		} else {
			o.Stats.LastSweep = "not since boot"
		}
	}

	lap("ledger status")
	if n, isk, err := m.econWalletSum(ctx, true); err != nil {
		warn("bot wallets", err)
	} else {
		o.Stats.BotCount, o.Stats.BotWallets = n, iskShort(isk)
	}
	if n, isk, err := m.econWalletSum(ctx, false); err != nil {
		warn("corporation wallets", err)
	} else {
		o.Stats.CorpCount, o.Stats.CorpWallets = n, iskShort(isk)
	}

	lap("wallet totals")
	if bals, err := m.econWalletBalances(ctx); err != nil {
		warn("wallet spread", err)
	} else {
		o.Wallets = buildWalletBoard(bals)
	}
	lap("wallet spread")
	if cats, err := m.econCategories(ctx, sinceHour); err != nil {
		warn("NPC market by category", err)
	} else {
		o.Categories = cats
	}
	lap("categories")
	if cells, err := m.econGoodsCells(ctx, sinceHour); err != nil {
		warn("NPC market by goods", err)
	} else {
		o.Goods = foldGoods(cells)
	}
	lap("goods")
	if hours, err := m.econHourFlows(ctx, sinceHour); err != nil {
		warn("timeline", err)
	} else {
		o.Hist = buildFlowHist(hours, win, now)
	}
	lap("timeline")
	if corps, err := m.econCorps(ctx, sinceDay, 15); err != nil {
		warn("top corporations", err)
	} else {
		o.Corps = corps
	}
	lap("top corporations")
	return o
}

// flowCell is one (actor, line) sum over the window.
type flowCell struct {
	Actor, Line string
	ISK, Inflow float64
	N           int64
}

func (m *Mod) econFlowCells(ctx context.Context, sinceHour int64) ([]flowCell, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT actor, line, SUM(isk), SUM(inflow), SUM(n)
		  FROM rolebot_isk_flow WHERE hour >= ?
		 GROUP BY actor, line`, sinceHour)
	if err != nil {
		return nil, fmt.Errorf("isk flow read: %w", err)
	}
	defer rows.Close()
	var out []flowCell
	for rows.Next() {
		var c flowCell
		if err := rows.Scan(&c.Actor, &c.Line, &c.ISK, &c.Inflow, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// foldFlowCells turns the cells into the three panels and the KPI figures.
// Pure.
func foldFlowCells(cells []flowCell) (faucets, sinks, transfers []flowRow, st econStats) {
	type acc struct {
		in, out   float64
		n         int64
		bot, corp float64
	}
	by := map[string]*acc{}
	for _, c := range cells {
		a := by[c.Line]
		if a == nil {
			a = &acc{}
			by[c.Line] = a
		}
		out := c.Inflow - c.ISK
		a.in += c.Inflow
		a.out += out
		a.n += c.N
		mag := c.Inflow
		if l, ok := iskFlowLineByKey[c.Line]; ok && l.Dir == flowSink {
			mag = out
		}
		if c.Actor == flowActorCorp {
			a.corp += mag
		} else {
			a.bot += mag
		}
	}
	var faucetV, sinkV, tBot, tCorp, fromP, toP float64
	for _, l := range iskFlowLines {
		a := by[l.Key]
		if a == nil {
			continue
		}
		r := flowRow{Key: l.Key, Label: l.Label, Dir: l.Dir, Desc: l.Desc,
			In: iskShort(a.in), Out: iskShort(a.out), InV: a.in, OutV: a.out,
			NetV: a.in - a.out, N: a.n}
		r.Net = iskSigned(r.NetV)
		switch l.Dir {
		case flowFaucet:
			r.Magnitude = a.in
			faucetV += a.in
		case flowSink:
			r.Magnitude = a.out
			sinkV += a.out
		case flowTransfer:
			// A transfer between two tracked wallets is in the ledger twice (debit and credit), so its
			// credits alone are the ISK that moved. A transfer with a player has only the bot's side.
			switch l.Key {
			case "trade_player", "gift_player", "rent_out", "job_fees_out":
				r.Magnitude = a.in + a.out
				fromP += a.in
				toP += a.out
			case "trade_bot", "gift_bot", "bounty_pool_in", "bounty_pool_out", "courier_in", "courier_out", "escrow_sale":
				r.Magnitude = a.in
				tBot += a.in
			default:
				r.Magnitude = a.in
				tCorp += a.in
			}
		default:
			r.Magnitude = a.in + a.out
		}
		r.BotISK, r.CorpISK = iskShort(a.bot), iskShort(a.corp)
		switch l.Dir {
		case flowFaucet:
			faucets = append(faucets, r)
		case flowSink:
			sinks = append(sinks, r)
		case flowTransfer:
			transfers = append(transfers, r)
		}
	}
	rank := func(rows []flowRow, total float64) {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Magnitude > rows[j].Magnitude })
		top := 0.0
		if len(rows) > 0 {
			top = rows[0].Magnitude
		}
		for i := range rows {
			if total > 0 {
				rows[i].Pct = fmt.Sprintf("%.1f%%", 100*rows[i].Magnitude/total)
			}
			if top > 0 {
				rows[i].Bar = int(math.Round(100 * rows[i].Magnitude / top))
			}
		}
	}
	rank(faucets, faucetV)
	rank(sinks, sinkV)
	rank(transfers, tBot+tCorp+fromP+toP)
	st.FaucetV, st.SinkV, st.NetV = faucetV, sinkV, faucetV-sinkV
	st.Faucet, st.Sink, st.Net = iskShort(faucetV), iskShort(sinkV), iskSigned(st.NetV)
	st.NetUp = st.NetV >= 0
	st.TransferBot, st.TransferCorp = iskShort(tBot), iskShort(tCorp)
	st.FromPlayers, st.ToPlayers = iskShort(fromP), iskShort(toP)
	return
}

// buildCatalogue is every line in order with its window figures, zero rows
// included: the catalogue's job is to list every door, open or shut. Pure.
func buildCatalogue(cells []flowCell) []catalogueRow {
	type acc struct {
		in, out float64
		n       int64
	}
	by := map[string]*acc{}
	for _, c := range cells {
		a := by[c.Line]
		if a == nil {
			a = &acc{}
			by[c.Line] = a
		}
		a.in += c.Inflow
		a.out += c.Inflow - c.ISK
		a.n += c.N
	}
	out := make([]catalogueRow, 0, len(iskFlowLines))
	for _, l := range iskFlowLines {
		r := catalogueRow{iskFlowLine: l}
		if a := by[l.Key]; a != nil && a.n > 0 {
			r.In, r.Out, r.N, r.Has = iskShort(a.in), iskShort(a.out), a.n, true
		} else {
			r.In, r.Out = "—", "—"
		}
		out = append(out, r)
	}
	return out
}

func (m *Mod) econWalletSum(ctx context.Context, bots bool) (int64, float64, error) {
	q := `SELECT COUNT(*), COALESCE(SUM(c.balance), 0)
	        FROM rolebot_characters rb JOIN character_ c ON c.characterID = rb.characterID`
	if !bots {
		q = `SELECT COUNT(*), COALESCE(SUM(co.balance), 0)
		       FROM rolebot_ceos ce JOIN corporation co ON co.corporationID = ce.corporationID`
	}
	var n int64
	var isk float64
	if err := m.db.QueryRowContext(ctx, q).Scan(&n, &isk); err != nil {
		return 0, 0, err
	}
	return n, isk, nil
}

func (m *Mod) econCategories(ctx context.Context, sinceHour int64) ([]catRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT f.categoryID, COALESCE(ic.categoryName, CONCAT('category ', f.categoryID)),
		       COALESCE(SUM(CASE WHEN f.side = 'buy'  THEN f.isk END), 0),
		       COALESCE(SUM(CASE WHEN f.side = 'sell' THEN f.isk END), 0),
		       SUM(f.units), SUM(f.n)
		  FROM rolebot_mkt_flow f
		  LEFT JOIN invCategories ic ON ic.categoryID = f.categoryID
		 WHERE f.hour >= ? AND f.counter = 'npc'
		 GROUP BY f.categoryID, ic.categoryName`, sinceHour)
	if err != nil {
		return nil, fmt.Errorf("npc categories: %w", err)
	}
	defer rows.Close()
	var out []catRow
	top := 0.0
	for rows.Next() {
		var r catRow
		if err := rows.Scan(&r.CategoryID, &r.Name, &r.BuyV, &r.SellV, &r.Units, &r.N); err != nil {
			return nil, err
		}
		r.NetV = r.SellV - r.BuyV
		r.Buy, r.Sell, r.Net = iskShort(r.BuyV), iskShort(r.SellV), iskSigned(r.NetV)
		r.NetUp = r.NetV >= 0
		if a := math.Abs(r.NetV); a > top {
			top = a
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return math.Abs(out[i].NetV) > math.Abs(out[j].NetV) })
	for i := range out {
		if top > 0 {
			out[i].Bar = int(math.Round(100 * math.Abs(out[i].NetV) / top))
		}
	}
	return out, nil
}

func (m *Mod) econHourFlows(ctx context.Context, sinceHour int64) ([]hourFlow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT hour, line, SUM(isk), SUM(inflow)
		  FROM rolebot_isk_flow WHERE hour >= ?
		 GROUP BY hour, line`, sinceHour)
	if err != nil {
		return nil, fmt.Errorf("hour flows: %w", err)
	}
	defer rows.Close()
	var out []hourFlow
	for rows.Next() {
		var h hourFlow
		if err := rows.Scan(&h.Hour, &h.Line, &h.ISK, &h.Inflow); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// buildFlowHist buckets the hourly cells into the window's columns, faucets
// up and sinks beside them, heights scaled to the tallest of either. Pure.
func buildFlowHist(rows []hourFlow, win econWindow, now int64) []histCol {
	if win.Buckets <= 0 || win.BucketSecs <= 0 {
		return nil
	}
	start := now - win.Secs
	fa := make([]float64, win.Buckets)
	si := make([]float64, win.Buckets)
	for _, r := range rows {
		l, ok := iskFlowLineByKey[r.Line]
		if !ok {
			continue
		}
		i := int((r.Hour*3600 - start) / win.BucketSecs)
		if i < 0 || i >= win.Buckets {
			continue
		}
		switch l.Dir {
		case flowFaucet:
			fa[i] += r.Inflow
		case flowSink:
			si[i] += r.Inflow - r.ISK
		}
	}
	top := 0.0
	for i := range fa {
		top = math.Max(top, math.Max(fa[i], si[i]))
	}
	out := make([]histCol, win.Buckets)
	for i := range out {
		t := time.Unix(start+int64(i)*win.BucketSecs, 0).UTC()
		label := t.Format("15:04")
		switch {
		case win.BucketSecs >= 86400:
			label = t.Format("01-02")
		case win.BucketSecs > 3600:
			label = t.Format("Mon 15h")
		}
		c := histCol{Label: label, FaucetV: fa[i], SinkV: si[i],
			Faucet: iskShort(fa[i]), Sink: iskShort(si[i]), Net: iskSigned(fa[i] - si[i]),
			Up: fa[i] >= si[i]}
		if top > 0 {
			c.FaucetH = int(math.Round(100 * fa[i] / top))
			c.SinkH = int(math.Round(100 * si[i] / top))
		}
		out[i] = c
	}
	return out
}

func (m *Mod) econCorps(ctx context.Context, sinceDay int64, limit int) ([]corpRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT f.corporationID, COALESCE(co.corporationName, CONCAT('corporation ', f.corporationID)),
		       f.line, SUM(f.isk), SUM(f.inflow)
		  FROM rolebot_isk_flow_corp f
		  LEFT JOIN corporation co ON co.corporationID = f.corporationID
		 WHERE f.day >= ?
		 GROUP BY f.corporationID, co.corporationName, f.line`, sinceDay)
	if err != nil {
		return nil, fmt.Errorf("corp flows: %w", err)
	}
	defer rows.Close()
	type acc struct {
		name    string
		in, out float64
		top     string
		topV    float64
	}
	by := map[int64]*acc{}
	for rows.Next() {
		var corpID int64
		var name, line string
		var isk, inflow float64
		if err := rows.Scan(&corpID, &name, &line, &isk, &inflow); err != nil {
			return nil, err
		}
		l, ok := iskFlowLineByKey[line]
		if !ok || (l.Dir != flowFaucet && l.Dir != flowSink) {
			continue
		}
		a := by[corpID]
		if a == nil {
			a = &acc{name: name}
			by[corpID] = a
		}
		mag := inflow
		if l.Dir == flowSink {
			mag = inflow - isk
			a.out += mag
		} else {
			a.in += mag
		}
		if mag > a.topV {
			a.top, a.topV = l.Label, mag
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]corpRow, 0, len(by))
	for id, a := range by {
		out = append(out, corpRow{CorpID: id, Name: a.name, NetV: a.in - a.out,
			In: iskShort(a.in), Out: iskShort(a.out), Net: iskSigned(a.in - a.out),
			TopLine: a.top, Up: a.in >= a.out})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NetV != out[j].NetV {
			return out[i].NetV > out[j].NetV
		}
		return out[i].CorpID < out[j].CorpID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	top := 0.0
	for _, r := range out {
		top = math.Max(top, math.Abs(r.NetV))
	}
	for i := range out {
		if top > 0 {
			out[i].Bar = int(math.Round(100 * math.Abs(out[i].NetV) / top))
		}
	}
	return out, nil
}

// ── the handler ─────────────────────────────────────────────────────────────

func (m *Mod) handleEconomy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	win := econWindowFrom(r)
	o, at := m.economyOverview(ctx, win, r.URL.Query().Get("refresh") == "1")
	asOf := "—"
	if at > 0 {
		asOf = time.Unix(at, 0).UTC().Format("15:04:05 UTC")
	}
	espRender(w, "rolebots_economy.html", map[string]any{
		"Page":    "rolebot-economy",
		"Tab":     "overview",
		"Window":  win,
		"Windows": econWindowPicks("/rolebots/economy", "", win),
		"O":       o,
		"AsOf":    asOf,
		"Refresh": "/rolebots/economy?w=" + win.Key + "&refresh=1",
	})
}

// ── shared helpers for the ledgers ──────────────────────────────────────────

// resolveCharName finds a pilot by name for the feed filters: an exact match
// first, then a unique prefix. 0 when nothing or more than one pilot fits.
func (m *Mod) resolveCharName(ctx context.Context, who string) int64 {
	who = strings.TrimSpace(who)
	if who == "" {
		return 0
	}
	var id int64
	err := m.db.QueryRowContext(ctx,
		`SELECT characterID FROM character_ WHERE characterName = ? LIMIT 1`, who).Scan(&id)
	if err == nil {
		return id
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID FROM character_ WHERE characterName LIKE ? LIMIT 2`, who+"%")
	if err != nil {
		return 0
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return 0
		}
		ids = append(ids, v)
	}
	if len(ids) == 1 {
		return ids[0]
	}
	return 0
}

// nullISK reads a nullable sum.
func nullISK(v sql.NullFloat64) float64 {
	if v.Valid {
		return v.Float64
	}
	return 0
}
