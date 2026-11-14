package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
)

// The order-book sweep: how a bot buys more than one seller holds. Most (station, type) pairs carry
// one order, so the cheapest seller of a commodity is usually a stall. A buyer therefore plans a
// sweep: the cheapest units in reach first, one seller at a time, until the need, the hold or the
// price cap runs out, all on one trip. Climbing the chart means moving between stations, not tiers.
// Three pieces, each usable alone:
//   - the ask ladder (scanAskLevels / sweepLevels): every sell level in scope, with the jumps from
//     the buyer, the empty tills it has heard of and what the roster just bought taken out;
//   - the cap (sweepRef x markup): the most a unit may cost before the sweep stops;
//   - the planner (planSweep, pure): which stations, in which order, buying how much of what.
// The trip re-plans at every stop (posreq_sweep.go): the plan is a route, not a contract.

// ── settings ─────────────────────────────────────────────────────────────────

const (
	settingSweepEnabled      = "rolebots.sweep.enabled"
	settingSweepMaxStops     = "rolebots.sweep.maxStops"
	settingSweepMaxJumps     = "rolebots.sweep.maxExtraJumps"
	settingSweepMarkup       = "rolebots.sweep.markup"
	settingSweepUrgentMarkup = "rolebots.sweep.urgentMarkup"

	// sweepSettingsTTL is how often the switches are re-read: an operator
	// change lands within a minute without a restart.
	sweepSettingsTTL = 60
)

// sweepConfig is the operator's dials.
type sweepConfig struct {
	Enabled bool
	// MaxStops is the most sellers one trip visits.
	MaxStops int
	// MaxExtraJumps is the furthest any one hop of a sweep may be from the
	// stop before it (or from the buyer, for the first).
	MaxExtraJumps int
	// Markup and UrgentMarkup multiply the reference price into the cap.
	Markup, UrgentMarkup float64
}

func defaultSweepConfig() sweepConfig {
	return sweepConfig{Enabled: true, MaxStops: 4, MaxExtraJumps: 8, Markup: 1.30, UrgentMarkup: 2.0}
}

var (
	sweepCfgV  atomic.Pointer[sweepConfig]
	sweepCfgAt atomic.Int64
)

// sweepSettings is the current dials, re-read from server_settings at most
// once a minute. Out-of-range values keep the default.
func (m *Mod) sweepSettings(ctx context.Context) sweepConfig {
	now := clock.Unix()
	if c := sweepCfgV.Load(); c != nil && now-sweepCfgAt.Load() < sweepSettingsTTL {
		return *c
	}
	c := defaultSweepConfig()
	if m != nil && m.db != nil {
		read := func(key string) (string, bool) {
			v, ok, err := evedb.GetServerSetting(ctx, m.db, key)
			return strings.TrimSpace(v), ok && err == nil
		}
		if v, ok := read(settingSweepEnabled); ok {
			c.Enabled = v != "0"
		}
		if v, ok := read(settingSweepMaxStops); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 12 {
				c.MaxStops = n
			}
		}
		if v, ok := read(settingSweepMaxJumps); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 40 {
				c.MaxExtraJumps = n
			}
		}
		if v, ok := read(settingSweepMarkup); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 1 && f <= 10 {
				c.Markup = f
			}
		}
		if v, ok := read(settingSweepUrgentMarkup); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 1 && f <= 20 {
				c.UrgentMarkup = f
			}
		}
	}
	if c.UrgentMarkup < c.Markup {
		c.UrgentMarkup = c.Markup
	}
	sweepCfgV.Store(&c)
	sweepCfgAt.Store(now)
	return c
}

// ── the ask ladder ───────────────────────────────────────────────────────────

// sweepLevel is one price level of one station's sell book for one type.
type sweepLevel struct {
	TypeID    int64
	StationID int64
	SystemID  int64
	Price     float64
	Vol       int64
	// Jumps from the buyer, filled in by sweepLevels from its reach map.
	Jumps int
}

// scanAskLevels reads every sell level for a set of types under a station filter, cheapest first.
// Same snapshot cache and key discipline as scanStops (marketsnap.go): the read has no pilot in it,
// so a corporation's buyers share it. Rows are shared; callers get a copy.
func (m *Mod) scanAskLevels(ctx context.Context, typeIDs []int64, f stationFilter) ([]sweepLevel, error) {
	if len(typeIDs) == 0 {
		return nil, nil
	}
	ids := append([]int64(nil), typeIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	key := snapKey("asklevels", f.Clause, f.Args, ids)
	sctx, cancel := snapContext(ctx)
	defer cancel()
	shared, err := snapshot(key, func() ([]sweepLevel, error) {
		args := append(int64Args(ids), f.Args...)
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT o.typeID, o.stationID, s.solarSystemID, o.price, SUM(o.volRemaining)
			  FROM market_orders o
			  JOIN staStations s ON s.stationID = o.stationID
			 WHERE o.typeID IN (`+placeholders(len(ids))+`)
			   AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0`+f.Clause+`
			 GROUP BY o.typeID, o.stationID, s.solarSystemID, o.price`), args...)
		if err != nil {
			return nil, fmt.Errorf("ask levels: %w", err)
		}
		defer rows.Close()
		var out []sweepLevel
		for rows.Next() {
			var l sweepLevel
			if err := rows.Scan(&l.TypeID, &l.StationID, &l.SystemID, &l.Price, &l.Vol); err != nil {
				return nil, fmt.Errorf("ask level row: %w", err)
			}
			out = append(out, l)
		}
		return out, rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := make([]sweepLevel, len(shared))
	copy(out, shared)
	return out, nil
}

// sweepLevels is the ladder one buyer can act on: levels in its reach,
// annotated with jumps, minus the tills its corporation knows are empty and
// minus what the roster has just bought off the cached book.
func (m *Mod) sweepLevels(ctx context.Context, b *botRun, typeIDs []int64, f stationFilter,
	reach map[int64]int) ([]sweepLevel, error) {
	raw, err := m.scanAskLevels(ctx, typeIDs, f)
	if err != nil {
		return nil, err
	}
	sc := botScope(b)
	out := raw[:0]
	for _, l := range raw {
		j, ok := reach[l.SystemID]
		if l.SystemID == b.SystemID {
			j, ok = 0, true // the buyer's own system needs no route
		}
		if !ok || tillIsEmpty(sc, l.TypeID, l.StationID) {
			continue
		}
		l.Jumps = j
		out = append(out, l)
	}
	return applyDepletion(out), nil
}

// ── what the roster just bought ──────────────────────────────────────────────
//
// The ladder is a cached read (marketSnapTTL). A buyer that clears a counter knows it is clear, but
// the snapshot does not for up to its TTL, so another planner would send a buyer for what is gone.
// emptytill.go handles a till found empty; this handles one found thinner. The order book is
// public, so this is the cache catching up with the database and is not scoped.

type depletionKey struct{ typeID, stationID int64 }

type depletionMark struct {
	units int64
	until int64
}

var (
	depletionMu sync.Mutex
	depletion   = map[depletionKey]depletionMark{}
)

// noteBought records units just bought off a station's book.
func noteBought(typeID, stationID, units int64) {
	if units <= 0 || typeID == 0 || stationID == 0 {
		return
	}
	now := clock.Unix()
	k := depletionKey{typeID, stationID}
	depletionMu.Lock()
	mk := depletion[k]
	if mk.until <= now {
		mk.units = 0
	}
	mk.units += units
	mk.until = now + int64(marketSnapTTL.Seconds())
	depletion[k] = mk
	depletionMu.Unlock()
}

// applyDepletion takes recently bought units off each station's cheapest
// levels and drops levels left empty. The input's order is not assumed.
func applyDepletion(levels []sweepLevel) []sweepLevel {
	now := clock.Unix()
	depletionMu.Lock()
	marks := make(map[depletionKey]int64, len(depletion))
	for k, mk := range depletion {
		if mk.until > now {
			marks[k] = mk.units
		}
	}
	depletionMu.Unlock()
	if len(marks) == 0 {
		return levels
	}
	sort.SliceStable(levels, func(i, j int) bool { return levels[i].Price < levels[j].Price })
	out := levels[:0]
	for _, l := range levels {
		k := depletionKey{l.TypeID, l.StationID}
		if gone := marks[k]; gone > 0 {
			take := min(gone, l.Vol)
			l.Vol -= take
			marks[k] = gone - take
		}
		if l.Vol > 0 {
			out = append(out, l)
		}
	}
	return out
}

func sweepDepletion() {
	now := clock.Unix()
	depletionMu.Lock()
	for k, mk := range depletion {
		if mk.until <= now {
			delete(depletion, k)
		}
	}
	depletionMu.Unlock()
}

func init() { registerSweep("sweepDepletion", sweepDepletion) }

// ── the reference price and the cap ──────────────────────────────────────────

// sweepRef is what one unit of a type normally costs: the higher of invTypes.basePrice and the
// volume-weighted price of the cheapest `need` units in the ladder (not the cheapest single ask,
// which is the stall's price). The depth price follows the market, so the cap stops only the climb to
// thin, expensive sellers; basePrice is the floor. 0 when there is nothing to go on.
func (m *Mod) sweepRef(ctx context.Context, typeID int64, need int64, levels []sweepLevel) float64 {
	return math.Max(m.basePriceCached(ctx, typeID), depthVWAP(levels, typeID, need))
}

// basePriceCache holds invTypes.basePrice, which is static data: shopping
// lists ask for it per line on every pass.
var basePriceCache sync.Map // typeID to float64

func (m *Mod) basePriceCached(ctx context.Context, typeID int64) float64 {
	if v, ok := basePriceCache.Load(typeID); ok {
		return v.(float64)
	}
	p := m.typeBasePrice(ctx, typeID)
	basePriceCache.Store(typeID, p)
	return p
}

// shopCaps is the per-line cap a shopping list is picked under, nil when the
// sweep is switched off (bestSupplyStopCapped then picks exactly as before).
// soft is a top-up or a materials bill and pays the normal markup; a fit or a
// hull the pilot cannot fly without pays the urgent one.
func (m *Mod) shopCaps(ctx context.Context, stops []marketStop, want map[int64]int64, soft bool) map[int64]float64 {
	cfg := m.sweepSettings(ctx)
	if !cfg.Enabled || len(want) == 0 {
		return nil
	}
	markup := cfg.UrgentMarkup
	if soft {
		markup = cfg.Markup
	}
	return m.supplyCapsFromStops(ctx, stops, want, markup)
}

// depthVWAP is the volume-weighted price of the cheapest need units of one
// type across a ladder, or of the whole ladder when it holds fewer. 0 for no
// levels. Pure.
func depthVWAP(levels []sweepLevel, typeID, need int64) float64 {
	var ls []sweepLevel
	for _, l := range levels {
		if l.TypeID == typeID && l.Vol > 0 && l.Price > 0 {
			ls = append(ls, l)
		}
	}
	if len(ls) == 0 {
		return 0
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].Price < ls[j].Price })
	if need <= 0 {
		need = math.MaxInt64
	}
	var units int64
	var spend float64
	for _, l := range ls {
		take := min(l.Vol, need-units)
		units += take
		spend += float64(take) * l.Price
		if units >= need {
			break
		}
	}
	if units == 0 {
		return 0
	}
	return spend / float64(units)
}

// sweepCap is the most one unit may cost: the reference times the markup, lowered to any hard
// limit the caller has (a line's MaxPrice, what its budget can still pay). A zero reference with no
// hard limit returns +Inf, never 0. Pure.
func sweepCap(ref, markup, hardMax float64) float64 {
	c := math.Inf(1)
	if ref > 0 {
		c = ref * markup
	}
	if hardMax > 0 && hardMax < c {
		c = hardMax
	}
	return c
}

// ── the planner ──────────────────────────────────────────────────────────────

// sweepLine is one thing the trip is buying.
type sweepLine struct {
	TypeID int64
	Need   int64
	UnitM3 float64
	Cap    float64 // most a unit may cost; +Inf for none
}

// sweepBuy is one purchase at one stop.
type sweepBuy struct {
	TypeID int64
	Qty    int64
	// Ceiling is the line's cap: the till may walk the book up to it.
	Ceiling float64
	Spend   float64 // at the ladder's prices
}

// sweepStop is one station on the route.
type sweepStop struct {
	StationID, SystemID int64
	// Hop is the jumps from the previous stop (or the buyer, for the first).
	Hop  int
	Buys []sweepBuy
}

// sweepPlan is a route and what it buys.
type sweepPlan struct {
	Stops []sweepStop
	// Uncovered is, per type, what the route leaves unbought: no level under
	// the cap in reach, or no room left in the hold.
	Uncovered map[int64]int64
	// CapBound is, per type, whether a cheaper-than-cap level ran out before the need did while levels
	// above the cap were left: the remainder is priced out, not absent.
	CapBound map[int64]bool
	Spend    float64
	Units    int64
}

// stopsFor is the planned purchases at one station, nil if it is not on the
// route.
func (p sweepPlan) stopsFor(stationID int64) []sweepBuy {
	for _, s := range p.Stops {
		if s.StationID == stationID {
			return s.Buys
		}
	}
	return nil
}

// sweepOpts are the planner's limits.
type sweepOpts struct {
	// Hold is the free m³; 0 or less means no hold limit.
	Hold float64
	// MaxStops and MaxHop bound the route (sweepConfig).
	MaxStops, MaxHop int
	// FromSystem is where the buyer stands. Dist gives jumps between two
	// systems, < 0 for unreachable; nil uses each level's Jumps for every hop.
	FromSystem int64
	Dist       func(a, b int64) int
	// JumpISK is what one jump of flying is worth, in ISK, against the
	// savings a stop offers. sweepJumpISK sizes it from the bill.
	JumpISK float64
	// Exclude are stations already visited this trip.
	Exclude map[int64]bool
}

// sweepJumpISK prices a jump of flying for a bill: the pilot's per-jump slope (Tuning.IskJumpSlope,
// 2% by default) of the bill's value at the reference prices. A small line makes a jump dear per
// unit, so a thin stall far off loses to a deep book; a hull bill makes jumps nearly free. Pure.
func sweepJumpISK(slope float64, lines []sweepLine, refs map[int64]float64) float64 {
	var value float64
	for _, ln := range lines {
		value += float64(ln.Need) * refs[ln.TypeID]
	}
	if slope <= 0 {
		slope = jumpPriceCost
	}
	return slope * value
}

// planSweep is the route: stations chosen greedily by effective unit price (their stock under each
// line's cap, plus the flying to reach them at JumpISK a jump, spread over the units handed over)
// until the need, hold, stop count or cheap stock runs out. A level above its line's cap is never
// taken. Stops come out nearest-first from the route's last point. Pure.
func planSweep(levels []sweepLevel, lines []sweepLine, o sweepOpts) sweepPlan {
	plan := sweepPlan{Uncovered: map[int64]int64{}, CapBound: map[int64]bool{}}
	need := map[int64]int64{}
	line := map[int64]sweepLine{}
	for _, ln := range lines {
		if ln.Need <= 0 {
			continue
		}
		if _, dup := line[ln.TypeID]; dup {
			need[ln.TypeID] += ln.Need
			continue
		}
		line[ln.TypeID] = ln
		need[ln.TypeID] = ln.Need
	}
	// Levels by station, cheapest first, inside each line's cap.
	byStation := map[int64][]sweepLevel{}
	sysOf := map[int64]int64{}
	jumpsOf := map[int64]int{}
	overCap := map[int64]bool{}
	for _, l := range levels {
		ln, ok := line[l.TypeID]
		if !ok || l.Vol <= 0 || l.Price <= 0 || o.Exclude[l.StationID] {
			continue
		}
		if l.Price > ln.Cap {
			overCap[l.TypeID] = true
			continue
		}
		byStation[l.StationID] = append(byStation[l.StationID], l)
		sysOf[l.StationID] = l.SystemID
		jumpsOf[l.StationID] = l.Jumps
	}
	for st := range byStation {
		ls := byStation[st]
		sort.Slice(ls, func(i, j int) bool { return ls[i].Price < ls[j].Price })
	}
	hold := o.Hold
	limitHold := hold > 0
	maxStops := o.MaxStops
	if maxStops <= 0 {
		maxStops = 1
	}
	hop := func(from int64, st int64) int {
		if o.Dist == nil || from == 0 {
			return jumpsOf[st]
		}
		return o.Dist(from, sysOf[st])
	}
	// take is what one station would hand over now: units per type, spend,
	// m³. Read-only on need/hold.
	take := func(st int64) (map[int64]int64, float64, float64, int64) {
		got := map[int64]int64{}
		var spend, m3 float64
		var units int64
		room := hold
		for _, l := range byStation[st] {
			left := need[l.TypeID] - got[l.TypeID]
			if left <= 0 {
				continue
			}
			q := min(left, l.Vol)
			if u := line[l.TypeID].UnitM3; limitHold && u > 0 {
				if fit := int64(room / u); fit < q {
					q = fit
				}
			}
			if q <= 0 {
				continue
			}
			got[l.TypeID] += q
			units += q
			spend += float64(q) * l.Price
			m3 += float64(q) * line[l.TypeID].UnitM3
			room -= float64(q) * line[l.TypeID].UnitM3
		}
		return got, spend, m3, units
	}
	from := o.FromSystem
	chosen := map[int64]bool{}
	for len(plan.Stops) < maxStops {
		if limitHold && hold <= 0 {
			break
		}
		var best int64
		bestEff := math.Inf(1)
		bestHop := 0
		for st := range byStation {
			if chosen[st] {
				continue
			}
			h := hop(from, st)
			if h < 0 || (o.MaxHop > 0 && h > o.MaxHop && len(plan.Stops) > 0) {
				continue
			}
			_, spend, _, units := take(st)
			if units <= 0 {
				continue
			}
			eff := (spend + o.JumpISK*(tripBaseJumps+float64(h))) / float64(units)
			if eff < bestEff || (eff == bestEff && st < best) {
				best, bestEff, bestHop = st, eff, h
			}
		}
		if best == 0 {
			break
		}
		got, spend, m3, units := take(best)
		chosen[best] = true
		stop := sweepStop{StationID: best, SystemID: sysOf[best], Hop: bestHop}
		// Spend per type at this station, for the buy rows.
		perSpend := map[int64]float64{}
		{
			left := map[int64]int64{}
			for t, q := range got {
				left[t] = q
			}
			for _, l := range byStation[best] {
				q := min(left[l.TypeID], l.Vol)
				if q <= 0 {
					continue
				}
				left[l.TypeID] -= q
				perSpend[l.TypeID] += float64(q) * l.Price
			}
		}
		types := make([]int64, 0, len(got))
		for t := range got {
			types = append(types, t)
		}
		sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
		for _, t := range types {
			stop.Buys = append(stop.Buys, sweepBuy{TypeID: t, Qty: got[t], Ceiling: line[t].Cap, Spend: perSpend[t]})
			need[t] -= got[t]
		}
		plan.Stops = append(plan.Stops, stop)
		plan.Spend += spend
		plan.Units += units
		hold -= m3
		from = sysOf[best]
		done := true
		for _, n := range need {
			if n > 0 {
				done = false
				break
			}
		}
		if done {
			break
		}
	}
	for t, n := range need {
		if n > 0 {
			plan.Uncovered[t] = n
			if overCap[t] {
				plan.CapBound[t] = true
			}
		}
	}
	return plan
}

// sweepDist is a planner distance over the gate graph: raw jumps between two systems, BFS-cached per
// source for the life of the closure. Security aversion applies to the first hop (the buyer's reach
// map); later hops use the bare graph.
func (m *Mod) sweepDist(ctx context.Context, maxJumps int) func(a, b int64) int {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return nil
	}
	cache := map[int64]map[int64]int{}
	return func(a, b int64) int {
		if a == b {
			return 0
		}
		d, ok := cache[a]
		if !ok {
			d = bfsJumps(u, a, maxJumps)
			cache[a] = d
		}
		if j, ok := d[b]; ok {
			return j
		}
		return -1
	}
}

// ── the till ─────────────────────────────────────────────────────────────────

// buyAtStationCapped is buyAtStation with the caller's ceiling instead of its own best-ask x 1.25:
// the till walks the book up to `ceiling` and no further. An empty fill tells the buyer's corporation
// the till is empty for this type (emptytill.go); every fill comes off the cached ladder (noteBought).
func (m *Mod) buyAtStationCapped(ctx context.Context, b *botRun, stationID, typeID, qty int64,
	ceiling float64, purpose string) (itemID, filled int64, spent float64, err error) {
	if qty <= 0 {
		return 0, 0, 0, nil
	}
	var bestAsk sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, `
		SELECT MIN(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0 AND charID <> ?`,
		stationID, typeID, b.CharID).Scan(&bestAsk); err != nil {
		return 0, 0, 0, fmt.Errorf("ask lookup: %w", err)
	}
	if !bestAsk.Valid {
		noteTillEmpty(botScope(b), typeID, stationID)
		return 0, 0, 0, fmt.Errorf("%w: no sell order at this station", errNoSellerHere)
	}
	if !math.IsInf(ceiling, 1) && ceiling > 0 && bestAsk.Float64 > ceiling {
		noteTillEmpty(botScope(b), typeID, stationID)
		m.chatterMarketRefused(b.CharID, stationID, typeID, bestAsk.Float64)
		return 0, 0, 0, fmt.Errorf("%w: the cheapest ask %s is over the %s cap", errNoSellerHere,
			iskString(bestAsk.Float64), iskString(ceiling))
	}
	if math.IsInf(ceiling, 1) || ceiling <= 0 {
		ceiling = bestAsk.Float64 * 1.25
	}
	out, err := evedb.PlaceBuyOrder(ctx, m.db, b.CharID, stationID, typeID,
		ceiling, qty, -1 /*station range*/, 1, 0 /*immediate*/, false)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("buy: %w", err)
	}
	if out.ErrorMsg != "" {
		return 0, 0, 0, fmt.Errorf("buy refused: %s", out.ErrorMsg)
	}
	if out.FilledQty <= 0 {
		noteTillEmpty(botScope(b), typeID, stationID)
		return 0, 0, 0, fmt.Errorf("%w: nothing on the book filled", errNoSellerHere)
	}
	for _, f := range out.Fills {
		spent += f.Price * float64(f.Quantity)
		if len(f.ItemEntityIDs) > 0 {
			itemID = f.ItemEntityIDs[len(f.ItemEntityIDs)-1]
		}
	}
	noteBought(typeID, stationID, out.FilledQty)
	if itemID == 0 {
		return 0, out.FilledQty, spent, fmt.Errorf("buy filled but no item delivered")
	}
	if len(out.Fills) > 1 {
		if merged, err := m.mergeStacks(ctx, b.CharID, stationID, typeID); err == nil && merged != 0 {
			itemID = merged
		}
	}
	m.bookPurchase(ctx, b.CharID, stationID, typeID, out.FilledQty, spent, out.Fills, purpose)
	return itemID, out.FilledQty, spent, nil
}
