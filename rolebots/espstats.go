package rolebots

// The Stats board: every rolebot, ranked.
//
// Most numbers are derived live from tables that keep them forever (skill points from
// entity_attributes, bounties from market_journal, jobs from ramJobs, blueprints from entity,
// kills from killmails plus the rollup that survives retention). Four are counted forward by
// statcount.go because nothing else records them: ore mined, jumps, docks and the trade record.
//
// Nothing is stored except a short memo of the assembled rows. One query is the spine and
// fails the page; every other is enrichment and soft-fails with a log line.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// statRow is one pilot, with every column the board carries.
type statRow struct {
	CharID    int64
	Name      string
	CorpName  string
	Generated bool
	Enabled   bool
	Sec       float64

	SP      int64
	Balance float64

	Bounty float64
	// Journal rows, not rats: payNPCBounties writes one row per participant per fight on the
	// biggest rat in the pot, so this counts paydays and the column is not called kills.
	BountyPayouts int64

	Built     int64 // delivered manufacturing jobs
	BuiltRuns int64 // and the runs inside them: a job of 50 is 50 ships
	BPOs      int64

	Jumps    int64
	Docks    int64
	OreUnits int64
	OreM3    float64

	Trades    int64
	Buys      int64
	Sells     int64
	Turnover  float64
	Profit    float64
	BestTrade float64
	Orders    int64
	Fees      float64

	Kills        int64
	Losses       int64
	ISKDestroyed float64
	ISKLost      float64
	Solo         int64
	FinalBlows   int64

	CorpMoves int64
}

// Money and volume render through methods because the module cannot add to the panel's
// funcMap. Each is the display half of a sort key; the raw number rides the cell's data-v.
func (r statRow) WalletISK() string    { return iskShort(r.Balance) }
func (r statRow) BountyISK() string    { return iskShort(r.Bounty) }
func (r statRow) TurnoverISK() string  { return iskShort(r.Turnover) }
func (r statRow) ProfitISK() string    { return iskShort(r.Profit) }
func (r statRow) BestTradeISK() string { return iskShort(r.BestTrade) }
func (r statRow) FeesISK() string      { return iskShort(r.Fees) }
func (r statRow) DestroyedISK() string { return iskShort(r.ISKDestroyed) }
func (r statRow) LostISK() string      { return iskShort(r.ISKLost) }
func (r statRow) OreM3Short() string   { return iskShort(r.OreM3) }
func (r statRow) SPShort() string      { return iskShort(float64(r.SP)) }
func (r statRow) SecStr() string       { return fmt.Sprintf("%.1f", r.Sec) }

// TradeSplit is the hover text under the Trades count. The two halves are
// worth having and not worth two more columns in a table this wide: a pilot
// with six hundred buys and forty sells is sitting on a warehouse.
func (r statRow) TradeSplit() string {
	return fmt.Sprintf("%d bought, %d sold", r.Buys, r.Sells)
}

// Eff is the killboard's own efficiency. Its 0/0 case is 0: a pilot who has never
// undocked is not perfectly efficient.
func (r statRow) Eff() float64 {
	total := r.ISKDestroyed + r.ISKLost
	if total <= 0 {
		return 0
	}
	return r.ISKDestroyed / total * 100
}

func (r statRow) EffStr() string { return fmt.Sprintf("%.0f", r.Eff()) }

// ── podiums ─────────────────────────────────────────────────────────────────

type podiumRow struct {
	Rank   int
	CharID int64
	Name   string
	Corp   string
	Value  string
}

type podium struct {
	Title string
	Note  string // what the number is, in a few words
	Rows  []podiumRow
	Empty bool
}

// podiumGroup is a headed run of podiums on the board.
type podiumGroup struct {
	Title   string
	Podiums []podium
}

// podiumSize is how many pilots each podium lists.
const podiumSize = 5

// topN ranks the roster by one measure. An entry whose value is zero is never placed, since
// a podium of pilots who all mined nothing reads as a result. A podium with nobody on it
// says so instead of vanishing.
func topN(rows []statRow, n int, title, note string,
	val func(statRow) float64, show func(statRow) string) podium {

	p := podium{Title: title, Note: note}
	ranked := make([]statRow, 0, len(rows))
	for _, r := range rows {
		if val(r) > 0 {
			ranked = append(ranked, r)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if val(ranked[i]) != val(ranked[j]) {
			return val(ranked[i]) > val(ranked[j])
		}
		return ranked[i].Name < ranked[j].Name
	})
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	for i, r := range ranked {
		p.Rows = append(p.Rows, podiumRow{
			Rank: i + 1, CharID: r.CharID, Name: r.Name, Corp: r.CorpName,
			Value: show(r),
		})
	}
	p.Empty = len(p.Rows) == 0
	return p
}

// buildPodiumGroups ranks the roster on every measure, grouped by theme. sorted is the roster in the
// order ties fall back to.
func buildPodiumGroups(sorted []statRow) []podiumGroup {
	isk := func(f func(statRow) float64) func(statRow) string {
		return func(rr statRow) string { return iskShort(f(rr)) }
	}
	count := func(f func(statRow) int64) (func(statRow) float64, func(statRow) string) {
		return func(rr statRow) float64 { return float64(f(rr)) },
			func(rr statRow) string { return fmt.Sprintf("%d", f(rr)) }
	}
	top := func(title, note string, f func(statRow) int64) podium {
		val, show := count(f)
		return topN(sorted, podiumSize, title, note, val, show)
	}
	topISK := func(title, note string, f func(statRow) float64) podium {
		return topN(sorted, podiumSize, title, note, f, isk(f))
	}
	groups := []podiumGroup{
		{Title: "Wealth and trade", Podiums: []podium{
			topISK("Richest", "wallet balance", func(rr statRow) float64 { return rr.Balance }),
			topISK("Most profit", "profit from trading", func(rr statRow) float64 { return rr.Profit }),
			topISK("Biggest trade volume", "ISK bought and sold", func(rr statRow) float64 { return rr.Turnover }),
			topISK("Biggest single trade", "profit on one trade", func(rr statRow) float64 { return rr.BestTrade }),
			top("Most trades", "buys and sells", func(rr statRow) int64 { return rr.Trades }),
			top("Most market orders", "orders placed", func(rr statRow) int64 { return rr.Orders }),
			topISK("Most fees paid", "broker fees", func(rr statRow) float64 { return rr.Fees }),
			topISK("Most bounty collected", "ISK, lifetime", func(rr statRow) float64 { return rr.Bounty }),
		}},
		{Title: "Combat", Podiums: []podium{
			top("Most kills", "killmails taken part in", func(rr statRow) int64 { return rr.Kills }),
			top("Most solo kills", "killed alone", func(rr statRow) int64 { return rr.Solo }),
			top("Most final blows", "landed the last shot", func(rr statRow) int64 { return rr.FinalBlows }),
			topISK("Most ISK destroyed", "value of ships killed", func(rr statRow) float64 { return rr.ISKDestroyed }),
			topN(sorted, podiumSize, "Best kill ratio", "% of ISK won, 10+ kills",
				func(rr statRow) float64 {
					if rr.Kills < 10 {
						return 0
					}
					return rr.Eff()
				},
				func(rr statRow) string { return rr.EffStr() + "%" }),
			top("Most ships lost", "times killed", func(rr statRow) int64 { return rr.Losses }),
			topISK("Costliest losses", "value of ships lost", func(rr statRow) float64 { return rr.ISKLost }),
			topN(sorted, podiumSize, "Worst security status", "outlaws, lowest first",
				func(rr statRow) float64 { return -rr.Sec },
				func(rr statRow) string { return rr.SecStr() }),
			topN(sorted, podiumSize, "Best security status", "highest first",
				func(rr statRow) float64 { return rr.Sec },
				func(rr statRow) string { return rr.SecStr() }),
		}},
		{Title: "Industry and travel", Podiums: []podium{
			topISK("Most ore mined", "m³ mined", func(rr statRow) float64 { return rr.OreM3 }),
			top("Most ships built", "runs delivered, last 60 days", func(rr statRow) int64 { return rr.BuiltRuns }),
			top("Most blueprint originals", "owned now", func(rr statRow) int64 { return rr.BPOs }),
			top("Furthest travelled", "gates jumped", func(rr statRow) int64 { return rr.Jumps }),
			top("Most docked", "station dockings", func(rr statRow) int64 { return rr.Docks }),
		}},
		{Title: "Character", Podiums: []podium{
			topISK("Most skill points", "trained SP", func(rr statRow) float64 { return float64(rr.SP) }),
			top("Most corp changes", "times switched corporation", func(rr statRow) int64 { return rr.CorpMoves }),
		}},
	}
	return groups
}

// ── the memo ────────────────────────────────────────────────────────────────

const statsMemoTTL = 60 // seconds

var statsMemo struct {
	mu      sync.Mutex
	rows    []statRow
	builtAt int64
}

// The memo holds the WHOLE roster, generated pilots included, and the gen
// filter is applied to the copy. Caching the two variants separately would
// double the work for a filter that is a loop over a slice.
func (m *Mod) statRows(ctx context.Context, fresh bool) ([]statRow, int64) {
	statsMemo.mu.Lock()
	defer statsMemo.mu.Unlock()

	now := clock.Unix()
	if !fresh && statsMemo.rows != nil && now-statsMemo.builtAt < statsMemoTTL {
		return statsMemo.rows, statsMemo.builtAt
	}
	rows, err := m.buildStatRows(ctx, fresh)
	if err != nil {
		// Keep the last good pass rather than blank the board: the spine failing is a database
		// problem, and a stale answer labelled with its age beats none.
		if statsMemo.rows != nil {
			rlogf("rolebots: stats board: %v (serving the previous pass)", err)
			return statsMemo.rows, statsMemo.builtAt
		}
		return nil, 0
	}
	statsMemo.rows, statsMemo.builtAt = rows, now
	return rows, now
}

// clearStatsMemo drops the assembled board. The TTL expires it on its own; a purge calls
// this so the answer changes at once.
func clearStatsMemo() {
	statsMemo.mu.Lock()
	statsMemo.rows, statsMemo.builtAt = nil, 0
	statsMemo.mu.Unlock()
}

// ── the handler ─────────────────────────────────────────────────────────────

func (m *Mod) handleStats(w http.ResponseWriter, r *http.Request) {
	// A dozen aggregates over the whole roster is a slow page on a big shard,
	// and the request context dies with the browser tab.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	q := r.URL.Query()
	all, builtAt := m.statRows(ctx, q.Get("fresh") == "1")
	if all == nil {
		espError(w, "rolebot-stats", "could not read the roster")
		return
	}

	// Generated pilots are included by default, unlike the roster and character browser: those
	// are lists to act on, while a leaderboard's subject is the population.
	hideGen := q.Get("gen") == "0"
	rows := all
	if hideGen {
		rows = make([]statRow, 0, len(all))
		for _, rr := range all {
			if !rr.Generated {
				rows = append(rows, rr)
			}
		}
	}

	// Opens on skill points rather than on roster insertion order, so the
	// first thing on screen is a ranking. Every column re-sorts in the browser.
	sorted := make([]statRow, len(rows))
	copy(sorted, rows)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].SP != sorted[j].SP {
			return sorted[i].SP > sorted[j].SP
		}
		return sorted[i].Name < sorted[j].Name
	})

	groups := buildPodiumGroups(sorted)

	espRender(w, "rolebots_stats.html", map[string]any{
		"Page":     "rolebot-stats",
		"Rows":     sorted,
		"Groups":   groups,
		"HideGen":  hideGen,
		"Count":    len(sorted),
		"AgeSecs":  clock.Unix() - builtAt,
		"SlowAge":  statsSlowAge(),
		"Msg":      r.URL.Query().Get("msg"),
		"Err":      r.URL.Query().Get("err"),
	})
}

// ── assembly ────────────────────────────────────────────────────────────────

// statStep is one enrichment query: it folds its rowset into the pilots' rows. A failure costs its
// own columns and nothing else, so it is logged rather than returned.
type statStep struct {
	name string
	run  func(context.Context, map[int64]*statRow) error
}

// runStatSteps runs the steps one after another and logs any that fail or take over a second.
func runStatSteps(ctx context.Context, byID map[int64]*statRow, steps ...statStep) {
	for _, s := range steps {
		start := clock.Now()
		if err := s.run(ctx, byID); err != nil {
			rlogf("rolebots: stats board: %s: %v", s.name, err)
		}
		if d := clock.Now().Sub(start); d > time.Second {
			rlogf("rolebots: stats board: %s took %s", s.name, d.Round(time.Millisecond))
		}
	}
}

// runStatChains runs each chain of steps in its own goroutine and waits for all of them. A chain is
// the unit of ordering: steps in one chain run in order, chains run side by side. Two chains never
// write the same column of a row, and the byID map is only read, so they share it without a lock.
func runStatChains(ctx context.Context, byID map[int64]*statRow, chains ...[]statStep) {
	var wg sync.WaitGroup
	for _, c := range chains {
		wg.Add(1)
		go func(c []statStep) {
			defer wg.Done()
			runStatSteps(ctx, byID, c...)
		}(c)
	}
	wg.Wait()
}

// buildStatRows runs one set-based pass per statistic over the whole roster.
// Nothing here is per-pilot: a query per bot is a query per bot, and this page
// exists to be opened on a roster of thousands. The cheap statistics are read on every rebuild; the
// three that scan the item and journal tables are kept for ten minutes (statsSlow below).
func (m *Mod) buildStatRows(ctx context.Context, fresh bool) ([]statRow, error) {
	byID, order, err := m.statSpine(ctx)
	if err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return []statRow{}, nil
	}

	// The kill steps stay in one chain: the archive adds to what the killmails set.
	runStatChains(ctx, byID,
		[]statStep{{"industry", m.statIndustry}},
		[]statStep{{"kills", m.statKills}, {"losses", m.statLosses}, {"kill archive", m.statKillArchive}},
		[]statStep{{"counters", m.statCounters}},
		[]statStep{{"corp moves", m.statCorpMoves}},
	)
	m.applySlowStats(ctx, byID, order, fresh)

	out := make([]statRow, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}

// ── the slow statistics ─────────────────────────────────────────────────────

// Skill points, bounties and blueprints each walk a few million rows of the item and journal tables
// (several seconds apiece on a shard of ten thousand bots), and they change slowly. They are computed
// side by side, kept for statsSlowTTL, and refreshed in the background once stale, so opening the page
// never waits on them except the first time after a start (or a ?fresh=1).
const statsSlowTTL = 600 // seconds

// statSlowVals is one pilot's slow columns.
type statSlowVals struct {
	SP, BPOs, Payouts int64
	Bounty            float64
}

var statsSlow struct {
	mu         sync.Mutex
	vals       map[int64]statSlowVals
	at         int64
	refreshing bool
}

// statsSlowAge is how old the slow columns are, in seconds; 0 before the first pass.
func statsSlowAge() int64 {
	statsSlow.mu.Lock()
	defer statsSlow.mu.Unlock()
	if statsSlow.at == 0 {
		return 0
	}
	return clock.Unix() - statsSlow.at
}

// computeSlowStats reads the three slow statistics in parallel into a table keyed by character.
func (m *Mod) computeSlowStats(ctx context.Context, order []int64) map[int64]statSlowVals {
	scratch := make(map[int64]*statRow, len(order))
	for _, id := range order {
		scratch[id] = &statRow{CharID: id}
	}
	runStatChains(ctx, scratch,
		[]statStep{{"skill points", m.statSkillPoints}},
		[]statStep{{"bounties", m.statBounties}},
		[]statStep{{"blueprints", m.statBlueprints}},
	)
	out := make(map[int64]statSlowVals, len(scratch))
	for id, r := range scratch {
		out[id] = statSlowVals{SP: r.SP, BPOs: r.BPOs, Payouts: r.BountyPayouts, Bounty: r.Bounty}
	}
	return out
}

// applySlowStats fills the slow columns from the cache. With nothing cached, or when fresh is set, it
// computes them now; with a stale cache it serves it and starts one background refresh.
func (m *Mod) applySlowStats(ctx context.Context, byID map[int64]*statRow, order []int64, fresh bool) {
	statsSlow.mu.Lock()
	vals, at := statsSlow.vals, statsSlow.at
	if vals != nil && !fresh {
		if clock.Unix()-at >= statsSlowTTL && !statsSlow.refreshing {
			statsSlow.refreshing = true
			go m.refreshSlowStats(append([]int64(nil), order...))
		}
		statsSlow.mu.Unlock()
	} else {
		statsSlow.mu.Unlock()
		vals = m.computeSlowStats(ctx, order)
		statsSlow.mu.Lock()
		statsSlow.vals, statsSlow.at = vals, clock.Unix()
		statsSlow.mu.Unlock()
	}
	for id, v := range vals {
		if r := byID[id]; r != nil {
			r.SP, r.BPOs, r.BountyPayouts, r.Bounty = v.SP, v.BPOs, v.Payouts, v.Bounty
		}
	}
}

// refreshSlowStats recomputes the slow columns off the request path.
func (m *Mod) refreshSlowStats(order []int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	vals := m.computeSlowStats(ctx, order)
	statsSlow.mu.Lock()
	statsSlow.vals, statsSlow.at, statsSlow.refreshing = vals, clock.Unix(), false
	statsSlow.mu.Unlock()
}

// statSpine is the only query that may fail the page: without a roster there
// is nothing to hang a column on.
func (m *Mod) statSpine(ctx context.Context) (map[int64]*statRow, []int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT b.characterID, c.characterName,
		       COALESCE(corp.corporationName, ''),
		       COALESCE(c.balance, 0), COALESCE(c.securityRating, 0),
		       b.enabled,
		       CASE WHEN g.characterID IS NULL THEN 0 ELSE 1 END
		  FROM rolebot_characters b
		  JOIN character_ c ON c.characterID = b.characterID
		  LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		  LEFT JOIN rolebot_generated g ON g.characterID = b.characterID`)
	if err != nil {
		return nil, nil, fmt.Errorf("statSpine: %w", err)
	}
	defer rows.Close()

	byID := map[int64]*statRow{}
	var order []int64
	for rows.Next() {
		var r statRow
		var en, gen int64
		if err := rows.Scan(&r.CharID, &r.Name, &r.CorpName, &r.Balance,
			&r.Sec, &en, &gen); err != nil {
			return nil, nil, fmt.Errorf("statSpine scan: %w", err)
		}
		r.Enabled, r.Generated = en != 0, gen != 0
		byID[r.CharID] = &r
		order = append(order, r.CharID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("statSpine: %w", err)
	}
	return byID, order, nil
}

// statSkillPoints is characterTotalSP (clone.go) over the whole roster in one pass: attribute
// 276 carries a skill's points, and a skill entity with no attribute row is a fresh level 1,
// 250 SP. The enrichments below fold a character-ID rowset into its pilot and drop other rows.
func (m *Mod) statSkillPoints(ctx context.Context, byID map[int64]*statRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.ownerID, COALESCE(SUM(COALESCE(ea.valueInt, 250)), 0)
		  FROM rolebot_characters rc
		  JOIN entity    e ON e.ownerID  = rc.characterID
		  JOIN invTypes  t ON t.typeID   = e.typeID
		  JOIN invGroups g ON g.groupID  = t.groupID
		  LEFT JOIN entity_attributes ea
		         ON ea.itemID = e.itemID AND ea.attributeID = ?
		 WHERE g.categoryID = 16 AND e.flag IN (7, 61)
		 GROUP BY e.ownerID`, attrSkillPoints)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, sp int64
		if err := rows.Scan(&id, &sp); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.SP = sp
		}
	}
	return rows.Err()
}

// statBounties sums refType-17 journal rows, the payment both tiers share (bountypay.go and
// the core's embodied payout). It is driven from the roster because market_journal indexes
// only characterID: leading on refTypeID would scan every wallet. Corporation tax rows drop
// out since a corporation is never on the roster.
func (m *Mod) statBounties(ctx context.Context, byID map[int64]*statRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT j.characterID, COALESCE(SUM(j.amount), 0), COUNT(*)
		  FROM rolebot_characters rc
		  JOIN market_journal j ON j.characterID = rc.characterID
		 WHERE j.refTypeID = 17
		 GROUP BY j.characterID`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		var amt float64
		if err := rows.Scan(&id, &amt, &n); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.Bounty, r.BountyPayouts = amt, n
		}
	}
	return rows.Err()
}

// statIndustry counts delivered manufacturing jobs (completedStatusID 101; 0 running, 1 ready,
// 102 aborted). pruneBotIndustryHistory removes ramJobs rows at botJobKeepDays, so it is a
// rolling figure. Jobs is trips to the factory; runs is hulls out (a job of fifty is fifty ships).
func (m *Mod) statIndustry(ctx context.Context, byID map[int64]*statRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT j.installerID, COUNT(*), COALESCE(SUM(j.runs), 0)
		  FROM rolebot_characters rc
		  JOIN ramJobs j ON j.installerID = rc.characterID
		 WHERE j.activityID = 1 AND j.completedStatusID = 101
		 GROUP BY j.installerID`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, jobs, runs int64
		if err := rows.Scan(&id, &jobs, &runs); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.Built, r.BuiltRuns = jobs, runs
		}
	}
	return rows.Err()
}

// statBlueprints counts originals held: a blueprint item with no invBlueprints row is an
// original, while copies always carry one for their remaining runs. It is a point-in-time count.
func (m *Mod) statBlueprints(ctx context.Context, byID map[int64]*statRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.ownerID, COUNT(DISTINCT e.itemID)
		  FROM rolebot_characters rc
		  JOIN entity e ON e.ownerID = rc.characterID
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE b.blueprintID IS NULL OR b.copy = 0
		 GROUP BY e.ownerID`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.BPOs = n
		}
	}
	return rows.Err()
}

// statKills is the killboard's per-actor arithmetic taken set-wide. A participant is credited
// once per killmail, never once per row: a pilot appears once per weapon group fired, so the
// inner query collapses to one row per (pilot, killID) before anything is counted.
func (m *Mod) statKills(ctx context.Context, byID map[int64]*statRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.charID, COUNT(*), COALESCE(SUM(t.totalValue), 0),
		       COALESCE(SUM(t.solo), 0), COALESCE(SUM(t.fb), 0)
		  FROM (SELECT a.charID, k.killID,
		               MAX(k.totalValue)        AS totalValue,
		               MAX(k.attackerCount = 1) AS solo,
		               MAX(a.finalBlow)         AS fb
		          FROM rolebot_characters rc
		          JOIN killmail_attackers a ON a.charID = rc.characterID
		          JOIN killmails          k ON k.killID = a.killID
		         GROUP BY a.charID, k.killID) t
		 GROUP BY t.charID`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n, solo, fb int64
		var isk float64
		if err := rows.Scan(&id, &n, &isk, &solo, &fb); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.Kills, r.ISKDestroyed, r.Solo, r.FinalBlows = n, isk, solo, fb
		}
	}
	return rows.Err()
}

// statLosses needs no collapse: killmails is already one row per loss.
func (m *Mod) statLosses(ctx context.Context, byID map[int64]*statRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT k.victimCharID, COUNT(*), COALESCE(SUM(k.totalValue), 0)
		  FROM rolebot_characters rc
		  JOIN killmails k ON k.victimCharID = rc.characterID
		 GROUP BY k.victimCharID`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		var isk float64
		if err := rows.Scan(&id, &n, &isk); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.Losses, r.ISKLost = n, isk
		}
	}
	return rows.Err()
}

// statKillArchive adds the rolled-up half of the record. A bot's killmails are detail-purged
// after killboard.botRetentionDays, but the retention sweep credits killboard_rollup in the
// same transaction first, so the halves are disjoint and sum. Scope 1 is the per-character rollup.
func (m *Mod) statKillArchive(ctx context.Context, byID map[int64]*statRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.actorID, r.kills, r.losses, r.iskDestroyed, r.iskLost,
		       r.soloKills, r.finalBlows
		  FROM killboard_rollup r
		  JOIN rolebot_characters rc ON rc.characterID = r.actorID
		 WHERE r.scope = 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kills, losses, solo, fb int64
		var dest, lost float64
		if err := rows.Scan(&id, &kills, &losses, &dest, &lost, &solo, &fb); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.Kills += kills
			r.Losses += losses
			r.ISKDestroyed += dest
			r.ISKLost += lost
			r.Solo += solo
			r.FinalBlows += fb
		}
	}
	return rows.Err()
}

// statCounters reads the module's lifetime counters (statcount.go): the four things nothing
// else in the database records.
func (m *Mod) statCounters(ctx context.Context, byID map[int64]*statRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, jumps, docks, oreUnits, oreM3,
		       trades, buys, sells, turnover, profit, bestTrade,
		       ordersPlaced, feesPaid
		  FROM rolebot_stats`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, jumps, docks, oreUnits, trades, buys, sells, orders int64
		var oreM3, turnover, profit, best, fees float64
		if err := rows.Scan(&id, &jumps, &docks, &oreUnits, &oreM3,
			&trades, &buys, &sells, &turnover, &profit, &best,
			&orders, &fees); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.Jumps, r.Docks = jumps, docks
			r.OreUnits, r.OreM3 = oreUnits, oreM3
			r.Trades, r.Buys, r.Sells = trades, buys, sells
			r.Turnover, r.Profit, r.BestTrade = turnover, profit, best
			r.Orders, r.Fees = orders, fees
		}
	}
	return rows.Err()
}

// statCorpMoves is how many times a pilot has changed employer (corprank.go's
// review ledger). The only per-bot lifetime counter that already existed.
func (m *Mod) statCorpMoves(ctx context.Context, byID map[int64]*statRow) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, moves FROM rolebot_corp_reviews`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		if r := byID[id]; r != nil {
			r.CorpMoves = n
		}
	}
	return rows.Err()
}
