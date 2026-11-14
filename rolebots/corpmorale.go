package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// Morale: how a corporation treats a member, and what that does to how long the member stays
// (docs/CORPS.md section 5b). Every rostered pilot carries a morale debt D per employer. Grievances
// raise it (unpaid work, a denied SRP claim, an unwanted war, nothing to do, disliked work) and
// goodwill pays it down (a cheque, a granted claim, a promotion, a fleet seat). D decays with one half-life.
// D does not move the employer score. It drives a leave hazard h = h0 * e^(k*D) per day, with h0 bent
// by loyalty. Leaving is armed by settingCorpAttrition; while it is off the hazard is logged and counted.
// Events go into rolebot_corp_morale_events under UNIQUE(char, corp, kind, srcKey) and fold into D only
// when the insert took. Conditions (stale, idle, far, at war) are integrated from the corporation's cursor.
// A row whose corporation is not the pilot's current one reads as 0. Kinds starting with "_" are
// observations (last rank, seat or payout seen), never part of D.

const (
	settingCorpAttrition   = "rolebots.corpAttrition"
	settingCorpAttritionAt = "rolebots.corpAttritionAt"

	moraleHalfLifeDays = 21.0
	moraleDMin         = -0.30
	moraleDMax         = 1.0
	// moraleHazardFloorD: the goodwill buffer stops paying off here, so a
	// well-run corporation lengthens tenure without making it endless.
	moraleHazardFloorD = -0.05
	moraleHazardK      = 3.6

	// h0 per day at the devoted, even and mercenary ends of loyalty.
	moraleH0Devoted   = 0.0025
	moraleH0Base      = 0.005
	moraleH0Mercenary = 0.010
	moraleNPCMul      = 0.5
	moraleOfficerMul  = 0.5

	// Exit caps, applied to shadow exits exactly as they will be to real ones.
	moraleCorpWeekCapFrac = 0.10
	moraleServerDayFrac   = 0.01

	moraleSampleEverySec = 86400
	moraleSweepTicks     = 60
	moraleCorpsPerPass   = 8
	moraleEventKeepSec   = 90 * 86400
	// moraleLookbackSec bounds every event read. The unique key does the
	// de-duplication; this only keeps the scans short.
	moraleLookbackSec = 14 * 86400
	moraleIdleWindow  = 30 * 86400
	// moraleIdleFull is the 30-day record (jobs, trades, kills, hauls,
	// labour) at which a pilot no longer counts as idle at all.
	moraleIdleFull = 12.0
)

// Kinds. Continuous kinds accrue per day; event kinds are one-off weights.
const (
	mkStale       = "stale"
	mkIdle        = "idle"
	mkFarHQ       = "far_hq"
	mkXeno        = "xeno_mates"
	mkOutlawDrift = "outlaw_drift"
	mkWar         = "war_unwanted"
	mkTax         = "tax"
	mkMisfit      = "directive_misfit"

	mkSkipped    = "skipped"
	mkNoPayroll  = "no_payroll"
	mkPaid       = "paid"
	mkSRPDenied  = "srp_denied"
	mkSRPLapsed  = "srp_lapsed"
	mkSRPGranted = "srp_granted"
	mkDeath      = "death"
	mkDemoted    = "demoted"
	mkPromoted   = "promoted"
	mkDividend   = "dividend"
	mkFleetWin   = "fleet_win"
	mkSeated     = "seated"
	mkUnseated   = "unseated"
	mkBook       = "book_bought"
	mkHead       = "head_appointed"
	mkShadowExit = "shadow_exit"
	mkExit       = "exit"        // the sampler decided; corpleave.go acts at the next review
	mkLeft       = "left"        // the pilot left (corpleave.go)
	mkExitLapsed = "exit_lapsed" // flagged, but busy for ten days: dropped
	mkExitStayed = "exit_stayed" // flagged, nowhere to go
	mkWarned     = "warned"      // a human CEO was told a member's debt reached moraleWarnD

	// Observations, never summed into D.
	mkObsRank    = "_rank"
	mkObsRoles   = "_roles"
	mkObsSeat    = "_seat"
	mkObsHead    = "_head"
	mkObsPaidOut = "_paidout"
)

// moraleWeeklyCap bounds how much one kind can move D in seven days, so a
// corporation cannot farm goodwill with a hundred tiny kills or cheques.
var moraleWeeklyCap = map[string]int{
	mkFleetWin: 4,
	mkPaid:     2,
}

// moraleKindLabel is the page's wording for a kind.
var moraleKindLabel = map[string]string{
	mkStale: "restless", mkIdle: "nothing to do", mkFarHQ: "far from the corp",
	mkXeno: "disliked corpmates", mkOutlawDrift: "corp turned outlaw", mkWar: "unwanted war",
	mkTax: "tax", mkMisfit: "work it dislikes", mkSkipped: "skipped on payroll",
	mkNoPayroll: "no payroll", mkPaid: "paid", mkSRPDenied: "SRP denied",
	mkSRPLapsed: "SRP lapsed", mkSRPGranted: "SRP granted", mkDeath: "losses",
	mkDemoted: "demoted", mkPromoted: "promoted", mkDividend: "dividend",
	mkFleetWin: "fleet wins", mkSeated: "fleet seat", mkUnseated: "lost seat",
	mkBook: "book bought for it", mkHead: "made department head",
	mkShadowExit: "would have left", mkExit: "decided to leave", mkLeft: "left",
	mkExitLapsed: "leaving lapsed", mkExitStayed: "stayed", mkWarned: "CEO warned",
}

func moraleLabel(kind string) string {
	if l, ok := moraleKindLabel[kind]; ok {
		return l
	}
	return kind
}

var (
	corpAttritionOn atomic.Bool
	corpAttritionAt atomic.Int64
	moraleBusy      atomic.Bool
)

// CorpAttrition reports whether morale may move pilots. Off, the ledger
// still runs and the hazard is rolled in shadow.
func CorpAttrition() bool { return corpAttritionOn.Load() }

func (m *Mod) loadCorpAttritionSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingCorpAttrition); err != nil {
		rlogf("rolebots: read attrition setting: %v (leaving it off)", err)
	} else if ok {
		corpAttritionOn.Store(v == "1")
	}
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingCorpAttritionAt); err == nil && ok {
		var at int64
		if _, err := fmt.Sscan(strings.TrimSpace(v), &at); err == nil {
			corpAttritionAt.Store(at)
		}
	}
}

func (m *Mod) setCorpAttrition(ctx context.Context, on bool) error {
	val := "0"
	if on {
		val = "1"
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingCorpAttrition, val); err != nil {
		return err
	}
	if on && !corpAttritionOn.Load() {
		now := clock.Unix()
		if err := evedb.SetServerSetting(ctx, m.db, settingCorpAttritionAt, fmt.Sprint(now)); err != nil {
			return err
		}
		corpAttritionAt.Store(now)
	}
	corpAttritionOn.Store(on)
	return nil
}

// ── the arithmetic (pure) ───────────────────────────────────────────────────

func moraleLambda() float64 { return math.Ln2 / (moraleHalfLifeDays * 86400) }

// moraleDecay is v after dt seconds of forgetting.
func moraleDecay(v float64, dt int64) float64 {
	if dt <= 0 {
		return v
	}
	return v * math.Exp(-moraleLambda()*float64(dt))
}

// moraleAccrue is v after dt seconds of a condition accruing perDay while
// decaying: the exact solution, so the result does not depend on how often
// it is sampled.
func moraleAccrue(v, perDay float64, dt int64) float64 {
	if dt <= 0 {
		return v
	}
	l := moraleLambda()
	e := math.Exp(-l * float64(dt))
	return v*e + (perDay/86400)/l*(1-e)
}

func moraleClamp(d float64) float64 { return math.Max(moraleDMin, math.Min(moraleDMax, d)) }

// moraleH0 is the base hazard per day before morale.
func moraleH0(traits map[string]int, npc, officer bool) float64 {
	h := bend(traitScale(traits[TraitLoyalty]), moraleH0Devoted, moraleH0Base, moraleH0Mercenary)
	if npc {
		h *= moraleNPCMul
	}
	if officer {
		h *= moraleOfficerMul
	}
	return h
}

// moraleHazard is the leave hazard per day.
func moraleHazard(d, h0 float64) float64 {
	return h0 * math.Exp(moraleHazardK*math.Max(d, moraleHazardFloorD))
}

// moraleLeaveProb is the chance of leaving over days at hazard h.
func moraleLeaveProb(h, days float64) float64 {
	if h <= 0 || days <= 0 {
		return 0
	}
	return 1 - math.Exp(-h*days)
}

// moraleMedianDays is the median stay at a constant hazard.
func moraleMedianDays(h float64) float64 {
	if h <= 0 {
		return math.Inf(1)
	}
	return math.Ln2 / h
}

// moraleRamp scales the hazard over the 14 days after attrition is switched
// on, so a ledger built up in shadow does not empty corporations in a day.
func moraleRamp(onAt, now int64) float64 {
	if onAt <= 0 || now <= onAt {
		return 0
	}
	return math.Min(1, float64(now-onAt)/(14*86400))
}

// moraleCorpWeekCap is the most members one corporation loses in a week.
func moraleCorpWeekCap(members int) int {
	return max(1, int(math.Floor(float64(members)*moraleCorpWeekCapFrac)))
}

// moraleCell is one folded (pilot, kind) value.
type moraleCell struct {
	Value float64
	At    int64
}

// value is the cell read at now.
func (c moraleCell) value(now int64) float64 { return moraleDecay(c.Value, now-c.At) }

// moraleTotal is D: the sum of a pilot's kinds at now, observations excluded.
func moraleTotal(cells map[string]moraleCell, now int64) float64 {
	d := 0.0
	for k, c := range cells {
		if strings.HasPrefix(k, "_") {
			continue
		}
		d += c.value(now)
	}
	return moraleClamp(d)
}

// moraleTop is the kind contributing most grievance, "" when none does.
func moraleTop(cells map[string]moraleCell, now int64) string {
	best, bestV := "", 0.01
	for k, c := range cells {
		if strings.HasPrefix(k, "_") {
			continue
		}
		if v := c.value(now); v > bestV {
			best, bestV = k, v
		}
	}
	return best
}

// ── the condition rates (pure) ──────────────────────────────────────────────

// moraleMember is one rostered pilot as a sample sees it.
type moraleMember struct {
	CharID     int64
	Name       string
	Traits     map[string]int
	Officer    bool
	LastMoveAt int64
	HomeJumps  int // jumps to the employer's nearest station, -1 unknown
	Evidence   float64
	Roles      uint64
	Race       int64
}

// moraleCorpView is what a sample knows about the employer.
type moraleCorpView struct {
	CorpID    int64
	NPC       bool
	Chaired   bool // an NPC chair runs it (npcchair.go)
	BotCEO    bool
	Human     bool
	Tags      []string
	TaxRate   float64
	AtWar     bool
	Races     []int64 // every member's race, humans included
	Directive []string
}

// moraleRates is every condition's rate per day for one pilot.
func moraleRates(cv moraleCorpView, mb moraleMember, onlineFrac float64) map[string]float64 {
	p := pilotIdentity{CharID: mb.CharID, Traits: mb.Traits, Tune: resolveTuning(mb.Traits)}
	r := map[string]float64{}
	r[mkStale] = 0.012 * bend(traitScale(mb.Traits[TraitWanderlust]), 0.7, 1, 1.4)

	idle := math.Max(0, math.Min(1, 1-mb.Evidence/moraleIdleFull))
	r[mkIdle] = 0.010 * idle * onlineFrac * bend(traitScale(mb.Traits[TraitPatience]), 0.5, 1, 1.5)

	if mb.HomeJumps < 0 || mb.HomeJumps > corpHomeDriftJumps {
		r[mkFarHQ] = 0.006
	}
	if cv.TaxRate > 0 {
		r[mkTax] = 0.02 * cv.TaxRate * bend(traitScale(mb.Traits[TraitGreed]), 0.5, 1, 1.5)
	}
	if cv.NPC {
		if cv.Chaired && len(cv.Directive) > 0 {
			r[mkMisfit] = 0.008 * math.Max(0, math.Min(1, 1-corpTagFit(p, cv.Directive)))
		}
		return r
	}
	// Player corporations only: NPC corporations have no wars, and race is
	// already a door at signing there.
	if n := len(cv.Races); n > 1 && p.Tune.Xenophobia > 0 && mb.Race != 0 {
		disliked := 0
		for _, other := range cv.Races {
			if xenoStanding(mb.Race, p.Tune.Xenophobia, other) < 0 {
				disliked++
			}
		}
		r[mkXeno] = 0.010 * float64(disliked) / float64(n)
	}
	if containsStr(cv.Tags, tagOutlaw) {
		if lp := p.lawfulPole(); lp > 0 {
			r[mkOutlawDrift] = 0.008 * lp
		}
	}
	if cv.AtWar {
		r[mkWar] = 0.010*p.pacifistPole() - 0.005*p.violentPole()
	}
	return r
}

// moraleOnlineFrac is the share of the days in [from, to) the pilot had a
// play window on: idle is only felt on days it logged in.
func moraleOnlineFrac(charID, from, to int64) float64 {
	c := tzSettings()
	if !c.On || to <= from {
		return 1
	}
	s := tzSlotOf(charID)
	d0, _ := tzLocal(time.Unix(from, 0))
	d1, _ := tzLocal(time.Unix(to, 0))
	days, on := 0, 0
	for d := d0; d <= d1; d++ {
		days++
		if _, ok := tzDayWindow(charID, s, d, c); ok {
			on++
		}
	}
	if days == 0 {
		return 1
	}
	return float64(on) / float64(days)
}

// ── storage ─────────────────────────────────────────────────────────────────

// moraleCursor is one corporation's sampling bookkeeping.
type moraleCursor struct {
	CorpID          int64
	StartedAt       int64
	SampledAt       int64
	LastPayrollSeen int64
}

func (m *Mod) getMoraleCursor(ctx context.Context, corpID int64) (moraleCursor, error) {
	c := moraleCursor{CorpID: corpID}
	err := m.db.QueryRowContext(ctx, `
		SELECT startedAt, sampledAt, lastPayrollSeen FROM rolebot_corp_morale_cursor WHERE corporationID = ?`,
		corpID).Scan(&c.StartedAt, &c.SampledAt, &c.LastPayrollSeen)
	if err == sql.ErrNoRows {
		return c, nil
	}
	return c, err
}

func (m *Mod) saveMoraleCursor(ctx context.Context, c moraleCursor) error {
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_morale_cursor (corporationID, startedAt, sampledAt, lastPayrollSeen)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE startedAt = VALUES(startedAt), sampledAt = VALUES(sampledAt),
		  lastPayrollSeen = VALUES(lastPayrollSeen)`,
		c.CorpID, c.StartedAt, c.SampledAt, c.LastPayrollSeen)
	return err
}

// loadMoraleCells reads every cell held against a corporation, by pilot.
func (m *Mod) loadMoraleCells(ctx context.Context, corpID int64) (map[int64]map[string]moraleCell, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, kind, value, at FROM rolebot_corp_morale WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("loadMoraleCells: %w", err)
	}
	defer rows.Close()
	out := map[int64]map[string]moraleCell{}
	for rows.Next() {
		var id int64
		var k string
		var c moraleCell
		if err := rows.Scan(&id, &k, &c.Value, &c.At); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]moraleCell{}
		}
		out[id][k] = c
	}
	return out, rows.Err()
}

// saveMoraleCells writes one pilot's cells against its current corporation.
func (m *Mod) saveMoraleCells(ctx context.Context, corpID, charID int64, cells map[string]moraleCell) error {
	if len(cells) == 0 {
		return nil
	}
	var sb strings.Builder
	args := make([]any, 0, len(cells)*5)
	sb.WriteString(`INSERT INTO rolebot_corp_morale (characterID, kind, corporationID, value, at) VALUES `)
	i := 0
	for k, c := range cells {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?)")
		args = append(args, charID, k, corpID, c.Value, c.At)
		i++
	}
	sb.WriteString(` ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), value = VALUES(value), at = VALUES(at)`)
	_, err := m.db.ExecContext(ctx, sb.String(), args...)
	return err
}

// moraleEvent is one discrete input.
type moraleEvent struct {
	CharID int64
	Kind   string
	SrcKey string
	Weight float64
	Detail string
}

// recordMoraleEvent inserts an event once; true when this call wrote it.
func (m *Mod) recordMoraleEvent(ctx context.Context, corpID int64, ev moraleEvent, now int64) (bool, error) {
	detail := ev.Detail
	if len(detail) > 255 {
		detail = detail[:255]
	}
	key := ev.SrcKey
	if len(key) > 64 {
		key = key[:64]
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT IGNORE INTO rolebot_corp_morale_events
			(characterID, corporationID, kind, srcKey, weight, at, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		ev.CharID, corpID, ev.Kind, key, ev.Weight, now, detail)
	if err != nil {
		return false, fmt.Errorf("recordMoraleEvent: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// moraleWeekCounts is how many events of each capped kind every member of a
// corporation took in the last week.
func (m *Mod) moraleWeekCounts(ctx context.Context, corpID, now int64) (map[int64]map[string]int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, kind, COUNT(*) FROM rolebot_corp_morale_events
		 WHERE corporationID = ? AND at >= ? AND kind IN (?, ?)
		 GROUP BY characterID, kind`, corpID, now-7*86400, mkFleetWin, mkPaid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]map[string]int{}
	for rows.Next() {
		var id int64
		var k string
		var n int
		if err := rows.Scan(&id, &k, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]int{}
		}
		out[id][k] = n
	}
	return out, rows.Err()
}

// ── the sweep ───────────────────────────────────────────────────────────────

// startMoraleSweep registers the ledger's lane. It runs whenever the module
// does: the ledger and the shadow rolls cost a few queries per corporation a
// day, and the soak needs them before anybody switches attrition on.
func (m *Mod) startMoraleSweep() {
	m.registerPeriodic("rolebot-morale", moraleSweepTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		if !moraleBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("morale sweep", func() {
			defer moraleBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := m.moralePass(ctx); err != nil {
				rlogf("rolebots: morale sweep: %v", err)
			}
		})
		return true
	})
}

// moraleCorpsDue lists corporations holding enabled bots whose sample is due,
// oldest first.
func (m *Mod) moraleCorpsDue(ctx context.Context, now int64, limit int) ([]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.corporationID, COALESCE(MAX(k.sampledAt), 0) AS s
		  FROM rolebot_characters b
		  JOIN character_ c ON c.characterID = b.characterID
		  LEFT JOIN rolebot_corp_morale_cursor k ON k.corporationID = c.corporationID
		 WHERE b.enabled = 1 AND c.corporationID > 0
		 GROUP BY c.corporationID
		HAVING s <= ?
		 ORDER BY s, c.corporationID
		 LIMIT ?`, now-moraleSampleEverySec, limit)
	if err != nil {
		return nil, fmt.Errorf("moraleCorpsDue: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id, s int64
		if err := rows.Scan(&id, &s); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (m *Mod) moralePass(ctx context.Context) error {
	now := clock.Unix()
	corps, err := m.moraleCorpsDue(ctx, now, moraleCorpsPerPass)
	if err != nil {
		return err
	}
	for _, corpID := range corps {
		if ctx.Err() != nil {
			return nil
		}
		if err := m.moraleSampleCorp(ctx, corpID, now); err != nil {
			rlogf("rolebots: morale %d: %v", corpID, err)
		}
	}
	if len(corps) > 0 {
		_, _ = m.db.ExecContext(ctx,
			`DELETE FROM rolebot_corp_morale_events WHERE at < ? LIMIT 5000`, now-moraleEventKeepSec)
	}
	return nil
}

// moraleExempt reports corporations nobody leaves on morale: schools (left on
// graduation), CONCORD and Sansha's Nation (never left).
func (m *Mod) moraleExempt(ctx context.Context, corpID int64) bool {
	return corpIsSchool(ctx, m.db, corpID) || corpIsConcord(ctx, m.db, corpID) || corpIsSansha(ctx, m.db, corpID)
}

// moraleSampleCorp folds one corporation's day into its members' morale and
// rolls the shadow hazard. The first sample only primes: every event already
// on file is recorded without being felt, so nobody arrives with a grudge
// from before the ledger existed.
func (m *Mod) moraleSampleCorp(ctx context.Context, corpID, now int64) error {
	cur, err := m.getMoraleCursor(ctx, corpID)
	if err != nil {
		return err
	}
	if m.moraleExempt(ctx, corpID) {
		cur.SampledAt = now
		if cur.StartedAt == 0 {
			cur.StartedAt = now
		}
		return m.saveMoraleCursor(ctx, cur)
	}
	prime := cur.SampledAt == 0
	if cur.StartedAt == 0 {
		cur.StartedAt = now
	}

	cv, members, err := m.moraleCorpRead(ctx, corpID, now)
	if err != nil {
		return err
	}
	cells, err := m.loadMoraleCells(ctx, corpID)
	if err != nil {
		return err
	}
	since := now - moraleLookbackSec
	if !prime && cur.SampledAt-86400 < since {
		since = cur.SampledAt - 86400
	}
	events, err := m.moraleEvents(ctx, cv, members, cells, &cur, since, now)
	if err != nil {
		return err
	}
	weekly, err := m.moraleWeekCounts(ctx, corpID, now)
	if err != nil {
		return err
	}

	dt := int64(0)
	if !prime {
		dt = now - cur.SampledAt
	}
	byChar := map[int64][]moraleEvent{}
	for _, ev := range events {
		byChar[ev.CharID] = append(byChar[ev.CharID], ev)
	}

	type exitCand struct {
		mb  moraleMember
		d   float64
		top string
	}
	var exits []exitCand
	for _, mb := range members {
		own := cells[mb.CharID]
		next := map[string]moraleCell{}
		for k, c := range own {
			if strings.HasPrefix(k, "_") {
				next[k] = c
				continue
			}
			next[k] = moraleCell{Value: moraleDecay(c.Value, now-c.At), At: now}
		}
		if !prime {
			frac := moraleOnlineFrac(mb.CharID, cur.SampledAt, now)
			for k, rate := range moraleRates(cv, mb, frac) {
				c := next[k]
				next[k] = moraleCell{Value: moraleAccrue(c.Value, rate, dt), At: now}
			}
		}
		for _, ev := range byChar[mb.CharID] {
			if strings.HasPrefix(ev.Kind, "_") {
				next[ev.Kind] = moraleCell{Value: ev.Weight, At: now}
				continue
			}
			if capN, ok := moraleWeeklyCap[ev.Kind]; ok && weekly[mb.CharID][ev.Kind] >= capN {
				continue
			}
			wrote, err := m.recordMoraleEvent(ctx, corpID, ev, now)
			if err != nil {
				return err
			}
			if !wrote || prime {
				continue
			}
			if weekly[mb.CharID] == nil {
				weekly[mb.CharID] = map[string]int{}
			}
			weekly[mb.CharID][ev.Kind]++
			c := next[ev.Kind]
			next[ev.Kind] = moraleCell{Value: c.Value + ev.Weight, At: now}
		}
		if err := m.saveMoraleCells(ctx, corpID, mb.CharID, next); err != nil {
			return err
		}
		if prime {
			continue
		}
		d := moraleTotal(next, now)
		h := moraleHazard(d, moraleH0(mb.Traits, cv.NPC, mb.Officer))
		if floor := corpMoveFloor(mb.Traits); mb.LastMoveAt != 0 && now-mb.LastMoveAt < floor {
			continue
		}
		if cv.Human && d >= moraleWarnD {
			m.warnHumanCEO(ctx, corpID, mb, d, now)
		}
		if rand.Float64() < moraleExitChance(h, float64(dt)/86400, CorpAttrition(), corpAttritionAt.Load(), now) {
			exits = append(exits, exitCand{mb: mb, d: d, top: moraleTop(next, now)})
		}
	}

	if len(exits) > 0 {
		sort.Slice(exits, func(i, j int) bool { return exits[i].d > exits[j].d })
		room, err := m.moraleExitRoom(ctx, corpID, len(members), now)
		if err != nil {
			return err
		}
		for i, e := range exits {
			if i >= room {
				m.logBot(e.mb.CharID, logInfo, "corp",
					"morale %.2f — would have left %s, but the exit cap holds it this week (shadow)",
					e.d, m.corpNameOf(ctx, corpID))
				continue
			}
			top := e.top
			kind := mkShadowExit
			if CorpAttrition() {
				kind = mkExit
			}
			if _, err := m.recordMoraleEvent(ctx, corpID, moraleEvent{CharID: e.mb.CharID, Kind: kind,
				SrcKey: fmt.Sprintf("day:%d", now/86400), Detail: fmt.Sprintf("D %.2f, top %s", e.d, top)}, now); err != nil {
				return err
			}
			if kind == mkExit {
				// The pilot goes at its next review (corpleave.go), where the guards can be asked.
				if err := m.flagExitDue(ctx, e.mb.CharID, now); err != nil {
					return err
				}
				m.logBot(e.mb.CharID, logInfo, "corp",
					"morale %.2f (worst: %s) — has decided to leave %s; it goes at its next review",
					e.d, moraleLabel(top), m.corpNameOf(ctx, corpID))
				continue
			}
			m.logBot(e.mb.CharID, logInfo, "corp",
				"morale %.2f (worst: %s) — would have left %s today (shadow, nobody moved)",
				e.d, moraleLabel(top), m.corpNameOf(ctx, corpID))
		}
	}

	cur.SampledAt = now
	return m.saveMoraleCursor(ctx, cur)
}

// moraleExitRoom is how many more members a corporation may lose today: the
// weekly per-corporation cap and the daily server-wide one, both read off the
// shadow exits already on file.
func (m *Mod) moraleExitRoom(ctx context.Context, corpID int64, members int, now int64) (int, error) {
	var week, today, roster int
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_morale_events
		 WHERE corporationID = ? AND kind IN (?, ?) AND at >= ?`, corpID, mkShadowExit, mkExit, now-7*86400).Scan(&week); err != nil {
		return 0, err
	}
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_morale_events WHERE kind IN (?, ?) AND at >= ?`,
		mkShadowExit, mkExit, now-86400).Scan(&today); err != nil {
		return 0, err
	}
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_characters WHERE enabled = 1`).Scan(&roster); err != nil {
		return 0, err
	}
	room := moraleCorpWeekCap(members) - week
	if srv := max(1, int(float64(roster)*moraleServerDayFrac)) - today; srv < room {
		room = srv
	}
	return max(0, room), nil
}

// ── reading a corporation ───────────────────────────────────────────────────

// moraleCorpRead builds the employer view and its rostered members.
func (m *Mod) moraleCorpRead(ctx context.Context, corpID, now int64) (moraleCorpView, []moraleMember, error) {
	cv := moraleCorpView{CorpID: corpID}
	npc, err := isNPCCorp(ctx, m.db, corpID)
	if err != nil {
		return cv, nil, err
	}
	cv.NPC = npc
	cv.Chaired = npc && npcChaired(corpID)
	_, cv.BotCEO = managedCEO(corpID)
	cv.Human = !npc && !cv.BotCEO
	if facts, err := getAnyCorpFacts(ctx, m.db, corpID); err == nil && facts != nil {
		cv.Tags = facts.Policy.Tags
	}
	// The tags the chair's directive is built from (npcDirective): the work
	// it pushes, scored for each member on the catalogue's own appeal.
	if cv.Chaired {
		for _, t := range cv.Tags {
			if len(npcTagActivities[t]) > 0 {
				cv.Directive = append(cv.Directive, t)
			}
		}
	}
	var ceoID int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(taxRate, 0), COALESCE(ceoID, 0) FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&cv.TaxRate, &ceoID)
	if !npc {
		cv.AtWar = len(relWarEnemiesOf(corpID)) > 0
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName, COALESCE(c.raceID, 0),
		       COALESCE(r.corprole, 0), COALESCE(v.lastMoveAt, 0)
		  FROM rolebot_characters b
		  JOIN character_ c ON c.characterID = b.characterID
		  LEFT JOIN chrCorporationRoles r ON r.characterID = c.characterID
		  LEFT JOIN rolebot_corp_reviews v ON v.characterID = c.characterID
		 WHERE b.enabled = 1 AND c.corporationID = ? AND c.characterID <> ?`, corpID, ceoID)
	if err != nil {
		return cv, nil, fmt.Errorf("morale members: %w", err)
	}
	var members []moraleMember
	for rows.Next() {
		var mb moraleMember
		if err := rows.Scan(&mb.CharID, &mb.Name, &mb.Race, &mb.Roles, &mb.LastMoveAt); err != nil {
			rows.Close()
			return cv, nil, err
		}
		mb.HomeJumps = -1
		members = append(members, mb)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return cv, nil, err
	}
	if len(members) == 0 {
		return cv, nil, nil
	}
	ids := make([]int64, len(members))
	for i, mb := range members {
		ids[i] = mb.CharID
	}

	traits, err := m.moraleTraits(ctx, ids)
	if err != nil {
		return cv, nil, err
	}
	evidence := m.moraleEvidence(ctx, corpID, ids, now)
	officers := m.moraleOfficers(ctx, cv, ids)
	if !npc {
		cv.Races = m.moraleRaces(ctx, corpID)
	}
	var near map[int64]int
	if cr, err := m.corpReachFor(ctx, corpID, 0); err == nil && cr != nil {
		near = cr.near
	}
	for i := range members {
		mb := &members[i]
		mb.Traits = traits[mb.CharID]
		if mb.Traits == nil {
			mb.Traits = map[string]int{}
		}
		mb.Evidence = evidence[mb.CharID]
		mb.Officer = officers[mb.CharID]
		if near != nil {
			if _, sys := m.stationOwnerAndSystem(ctx, m.homeStationFor(ctx, mb.CharID)); sys != 0 {
				if j, ok := near[sys]; ok {
					mb.HomeJumps = j
				}
			}
		}
	}
	return cv, members, nil
}

// inArgs renders an IN list of placeholders and its arguments.
func inArgs(ids []int64) (string, []any) {
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return strings.Join(ph, ","), args
}

func (m *Mod) moraleTraits(ctx context.Context, ids []int64) (map[int64]map[string]int, error) {
	ph, args := inArgs(ids)
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, trait, value FROM rolebot_traits WHERE characterID IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("morale traits: %w", err)
	}
	defer rows.Close()
	out := map[int64]map[string]int{}
	for rows.Next() {
		var id int64
		var k string
		var v int
		if err := rows.Scan(&id, &k, &v); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]int{}
		}
		out[id][k] = v
	}
	return out, rows.Err()
}

// moraleEvidence is each member's 30-day working record: industry jobs,
// market trades, kills, delivered couriers and corporate labour. Best effort:
// a failed read counts as no record, never as an error.
func (m *Mod) moraleEvidence(ctx context.Context, corpID int64, ids []int64, now int64) map[int64]float64 {
	out := map[int64]float64{}
	ph, args := inArgs(ids)
	cutUnix := now - moraleIdleWindow
	cutWin := evedb.UnixToWin32(cutUnix)
	add := func(q string, extra ...any) {
		rows, err := m.db.QueryContext(ctx, q, append(append([]any{}, args...), extra...)...)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var n float64
			if rows.Scan(&id, &n) == nil {
				out[id] += n
			}
		}
	}
	add(`SELECT installerID, COUNT(*) FROM ramJobs WHERE installerID IN (`+ph+`) AND installTime >= ? GROUP BY installerID`, cutWin)
	add(`SELECT characterID, COUNT(*) FROM market_transactions WHERE characterID IN (`+ph+`) AND transactionDateTime >= ? GROUP BY characterID`, cutWin)
	add(`SELECT a.charID, COUNT(DISTINCT a.killID) FROM killmail_attackers a JOIN killmails k ON k.killID = a.killID
	      WHERE a.charID IN (`+ph+`) AND k.killTime >= ? GROUP BY a.charID`, cutUnix)
	add(`SELECT acceptorID, COUNT(*) FROM couriermissions WHERE acceptorID IN (`+ph+`) AND state = ? AND dateIssued >= ? GROUP BY acceptorID`,
		evedb.CourierStateDelivered, cutUnix)
	add(`SELECT characterID, COUNT(*) FROM rolebot_corp_labour WHERE characterID IN (`+ph+`) AND corporationID = ? AND at >= ? GROUP BY characterID`,
		corpID, cutUnix)
	return out
}

// moraleOfficers marks members holding an office: a bot CEO's staff posts, or
// an NPC chair's Director of Security.
func (m *Mod) moraleOfficers(ctx context.Context, cv moraleCorpView, ids []int64) map[int64]bool {
	out := map[int64]bool{}
	if cv.Chaired {
		if r, ok, err := m.getNPCChair(ctx, cv.CorpID); err == nil && ok && r.SecurityHeadID != 0 {
			out[r.SecurityHeadID] = true
		}
		return out
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID FROM rolebot_corp_posts WHERE corporationID = ?`, cv.CorpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// moraleRaces is every member's race, humans included.
func (m *Mod) moraleRaces(ctx context.Context, corpID int64) []int64 {
	rows, err := m.db.QueryContext(ctx,
		`SELECT COALESCE(raceID, 0) FROM character_ WHERE corporationID = ? AND accountID > 0`, corpID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var r int64
		if rows.Scan(&r) == nil && r != 0 {
			out = append(out, r)
		}
	}
	return out
}
