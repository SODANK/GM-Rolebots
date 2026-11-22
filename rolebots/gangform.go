package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
)

// Fleet formation: corporation fleets. Only a commander seated by its corp's chain of
// command (corprank.go) may start one, and the fleet flies the activity the seat names;
// the operator's hand-built gangs (espgangs.go) are the one other way a formation exists.
// The sweep tears down auto-formed fleets whose op has wound down, counts what is left
// against the gang cap, then lets idle seated commanders take fleets out. Auto fleets
// carry an expiry; past it the sweep disbands them (mailing a player-corp CEO the
// after-action report). Operator-built gangs (autoFormed = 0) are never touched.
// Recruiting rules: docs/comment-notes-b10.md#fleet-recruiting.

// Formation has no switch: corporations fielding fleets is part of the module, gated only by the
// master switch. (A former rolebots.gangForm row in server_settings is no longer read.)

const (
	// settingMaxGangs is the operator's cap on how many gangs fly at once. It counts every
	// gang, hand-built ones included, to put a lid on formation flying. There is no cap on
	// how many pilots one gang holds (see gangSeatsUncapped).
	settingMaxGangs = "rolebots.maxGangs"

	defaultMaxGangs = 12

	// skillLeadership is the Leadership skill (groupID 258). It does not size a fleet; the
	// corporation rank pass reads it when it picks and trains commanders.
	skillLeadership = 3348

	// gangSeatsUncapped is the seat count a fleet is composed for when nothing limits it.
	// The recruiter shrinks it to the pilots the corporation (and allies) can field, so a
	// fleet's working size is the roster. A bash fleet is every capable pilot in reach.
	gangSeatsUncapped = math.MaxInt32

	// gangFormEveryTicks is the sweep cadence (ticks are one second).
	gangFormEveryTicks = 300
	// GangFormPerPass and GangLeaderScan (gangtier.go) size a pass; while the distress board
	// has live rows (beltdistress.go) a pass may form beltReliefExtra more (gangFormPass).
	// gangRecruitMaxJumps is how far from its leader a recruit may live.
	gangRecruitMaxJumps = 15
	// gangMinMembers: a gang of one is a solo trip.
	gangMinMembers = 2
	// musterTravelSlackSec covers what the per-jump budget does not: finishing
	// the leg the call-up interrupted, the approach and dock at staging, and
	// a trip to collect the seat's ship if it is parked elsewhere.
	musterTravelSlackSec = 10 * 60
	// Auto gangs live an afternoon: two to four hours, then wind down.
	gangLifeMinSec = 2 * 3600
	gangLifeMaxSec = 4 * 3600
	// gangLeadTrainPerPass is how many pilots one corporation's scarcity
	// nudge hands a Leadership book per promotion pass (corprank.go).
	gangLeadTrainPerPass = 2

	// The op lifecycle clocks. A fleet musters first (everyone gets their seat's ship and
	// meets at the leader's home station; a muster past its deadline drops stragglers), then
	// winds down at staging for the settlement round, under a deadline.
	settingMusterTimeout  = "rolebots.musterTimeoutMin"
	settingWindDownWindow = "rolebots.windDownMin"
	defaultMusterMin      = 45
	defaultWindDownMin    = 30

	// The lifecycle phases, stored in GangState.OpPhase. Empty means a legacy or
	// operator-built gang: live from birth, never mustered, deleted rather than wound down.
	gangOpMuster   = "muster"
	gangOpLive     = "live"
	gangOpWindDown = "winddown"

	// logiViableTTL is how long the "does anybody sell a remote repairer" probe is believed;
	// the market moves on the scale of industry runs, not sim passes.
	logiViableTTL = time.Hour
)

var (
	maxGangsVal  atomic.Int64
	gangFormBusy atomic.Bool

	musterTimeoutMin atomic.Int64
	windDownMin      atomic.Int64

	logiViableMu sync.Mutex
	logiViableAt time.Time
	logiViableOK bool
)

// MusterTimeoutSec / WindDownSec are the lifecycle deadlines, in seconds.
func MusterTimeoutSec() int64 { return musterTimeoutMin.Load() * 60 }
func WindDownSec() int64      { return windDownMin.Load() * 60 }
func setMusterTimeoutMin(n int) {
	if n < 1 {
		n = defaultMusterMin
	}
	musterTimeoutMin.Store(int64(n))
}
func setWindDownMin(n int) {
	if n < 1 {
		n = defaultWindDownMin
	}
	windDownMin.Store(int64(n))
}

// logiViable reports whether any remote repairer is for sale anywhere, the market probe
// behind offering logi seats at all. Composition degrades those seats to guns when none
// is (gangComposition). Cached, since the answer moves at industry speed and formation
// runs every five minutes.
func (m *Mod) logiViable(ctx context.Context) bool {
	logiViableMu.Lock()
	if !logiViableAt.IsZero() && clock.Now().Sub(logiViableAt) < logiViableTTL {
		ok := logiViableOK
		logiViableMu.Unlock()
		return ok
	}
	logiViableMu.Unlock()

	ok := false
	if cat, err := m.buyableModules(ctx); err == nil {
		for _, mm := range cat {
			if mm.GroupID == grpShieldTransporter || mm.GroupID == grpArmorRepProjector {
				ok = true
				break
			}
		}
	}
	logiViableMu.Lock()
	logiViableAt, logiViableOK = clock.Now(), ok
	logiViableMu.Unlock()
	return ok
}

func MaxGangs() int     { return int(maxGangsVal.Load()) }
func setMaxGangs(n int) { maxGangsVal.Store(int64(max(n, 0))) }

// loadGangSettings reads the gang settings at boot. Missing values keep the defaults.
func (m *Mod) loadGangSettings(ctx context.Context) {
	setMaxGangs(defaultMaxGangs)
	readInt := func(key string, set func(int)) {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, key)
		if err != nil || !ok {
			return
		}
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			set(n)
		}
	}
	readInt(settingMaxGangs, setMaxGangs)
	setMusterTimeoutMin(defaultMusterMin)
	setWindDownMin(defaultWindDownMin)
	readInt(settingMusterTimeout, setMusterTimeoutMin)
	readInt(settingWindDownWindow, setWindDownMin)
	m.loadMusterSettings(ctx)
	m.loadGangTierSettings(ctx)
	m.loadHelpSettings(ctx)
	m.loadLootDonateSettings(ctx)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingAbstractBountyRate); err == nil && ok {
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			setAbstractBountyRate(f)
		}
	}
	m.loadThreatGuard(ctx)
	m.loadDroneAttrition(ctx)
	m.loadDefendVsCriminal(ctx)
	m.loadAssist(ctx)
	m.loadAssistBlueChance(ctx)
	m.loadTransitWarChance(ctx)
	m.loadCombatDoctrine(ctx)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingBotPvP); err == nil && ok {
		setBotPvP(v == "1")
	}
	readInt(settingPirateBountyISK, setPirateBountyISK)
	m.loadFleetSettings(ctx)
	m.loadCommandSettings(ctx)
}

// gangSlot is one seat the recruiter tries to fill.
type gangSlot struct {
	role string
	job  string
}

// gangComposition is the seats behind the leader for one op of the given total size.
// Mining is the classic loop at one hauler per two miners, interleaved. Combat fleets seat
// a tackler second, a logi after every pair of gun seats and guns for the rest. A
// specialist seat degrades to a gun when nobody can fill it; logiOK false offers no logi.
func gangComposition(activity string, size int, logiOK bool) []gangSlot {
	return gangCompositionWith(activity, size, logiOK, false)
}

// gangCompositionWith is gangComposition with the EWAR seat's market answer too
// (ewarViable). A combat fleet of five or more behind its leader seats one EWAR pilot in
// place of its last gun, ten or more a second, careful pilots first (seatAffinity). It
// degrades to a gun like every specialist seat.
func gangCompositionWith(activity string, size int, logiOK, ewarOK bool) []gangSlot {
	out := gangCompositionBase(activity, size, logiOK)
	if !ewarOK || (activity != ActCombat && activity != ActSecurity && activity != ActPiracy) {
		return out
	}
	want := 0
	switch n := len(out); {
	case n >= 10:
		want = 2
	case n >= 5:
		want = 1
	}
	for i := len(out) - 1; i >= 0 && want > 0; i-- {
		if out[i].job == JobCombat || out[i].job == JobPiracy {
			out[i] = gangSlot{RoleEwar, JobEwar}
			want--
		}
	}
	return out
}

// gangCompositionPvP is the seats behind the leader of a pilot-hunting pack: the tackler
// first (without a point the target leaves), then guns and no logi, since a pack that
// stops to mend itself is not hunting. Every seat degrades to a gun when nobody can fill
// it.
func gangCompositionPvP(size int) []gangSlot {
	n := size - 1
	if n < 1 {
		return nil
	}
	out := []gangSlot{{RoleTackler, JobTackle}}
	for len(out) < n {
		out = append(out, gangSlot{RoleHunter, JobCombat})
	}
	return out
}

func gangCompositionBase(activity string, size int, logiOK bool) []gangSlot {
	n := size - 1 // minus the leader
	if n < 1 {
		return nil
	}
	var out []gangSlot
	switch activity {
	case ActMining, ActIndMining:
		// Haulage is one hauler per two miners (the leader counts as a miner), rounded to nearest,
		// and interleaved so every prefix holds the ratio. See docs/comment-notes-b10.md#mining-seat-ratio.
		guns := 0
		if n >= 3 {
			guns = 1
		}
		if n >= 9 {
			guns = 2
		}
		seats := n - guns           // non-combat seats behind the leader
		bodies := seats + 1         // ... plus the leader, who mines
		haulers := (bodies + 1) / 3 // == round(miners/2) at the fixed point
		if haulers > seats {
			haulers = seats
		}
		hauled, k := 0, 0
		for i := 0; len(out) < n; i++ {
			if (i == 1 && guns >= 1) || (i == 5 && guns >= 2) {
				out = append(out, gangSlot{RoleCombat, JobCombat})
				continue
			}
			if hauled*seats < (k+1)*haulers {
				out = append(out, gangSlot{RoleHauler, JobTrade})
				hauled++
			} else {
				out = append(out, gangSlot{RoleMiner, JobMining})
			}
			k++
		}
	case ActCombat, ActSecurity, ActPiracy:
		// A security patrol takes the combat shape verbatim; it differs only in how far from home
		// it flies (gangSecuritySite).
		gunRole, gunJob := RoleHunter, JobCombat
		if activity == ActPiracy {
			gunRole, gunJob = RoleCombat, JobPiracy
		}
		out = append(out, gangSlot{RoleTackler, JobTackle})
		// After the tackler: a logi, then two guns, repeating. The first logi is the fleet's
		// third ship and another follows every pair of gun seats.
		sinceLogi := 2
		for len(out) < n {
			if logiOK && sinceLogi >= 2 {
				out = append(out, gangSlot{RoleLogi, JobLogi})
				sinceLogi = 0
				continue
			}
			out = append(out, gangSlot{gunRole, gunJob})
			sinceLogi++
		}
	}
	return out
}

// gangLeaderRole is the seat the leader itself takes. The industry fleet's commander
// mines with its fleet: its baking jobs are serviced remotely mid-op (industryOvenSweep),
// so the factory does not need it standing there.
func gangLeaderRole(activity string) string {
	switch activity {
	case ActMining, ActIndMining:
		return RoleMiner
	case ActCombat, ActSecurity:
		return RoleHunter
	case ActPiracy:
		return RoleCombat
	}
	return RoleMiner
}

// ── the sweep ───────────────────────────────────────────────────────────────

// startGangFormation registers the fleet-formation sweep. Runs off the tick
// goroutine behind a self-overlap guard, because periodics fire synchronously
// and a formation pass does real fit-pool work.
func (m *Mod) startGangFormation() {
	m.registerPeriodic("rolebot-gang-form", gangFormEveryTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		if !gangFormBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("gang-form sweep", func() {
			defer gangFormBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			m.gangFormPass(ctx)
		})
		return true
	})
}

// gangFormPass forms this pass's fleets: a base budget every seat competes for on equal
// terms, and while the distress board has live rows a reserved budget on top that only a
// security seat may spend. Distress adds fleets and never displaces them. See
// docs/comment-notes-b10.md#gang-form-budget.
func (m *Mod) gangFormPass(ctx context.Context) {
	m.sweepAutoGangs(ctx)

	total, err := countGangs(ctx, m.db)
	if err != nil {
		rlogf("rolebots: gang formation: %v", err)
		return
	}
	// A live distress board is a reason to hurry: miners are locked out of belts, and a patrol
	// answers spawns no solo hull can face. The extra formations are reserved to that seat.
	base, extra := formBudget(m.anyBeltDistress(ctx), MaxGangs(), total)
	packRoom := packBudget(int(dialPackBudget.Get()), MaxGangs(), total, base+extra)

	tally := formTally{}
	// Pass zero: the calls to arms (gangcalls.go), outside the budget. It runs ahead of the budget
	// test: a full gang cap must hold back neither the page nor a siege's or a defence's fleet.
	m.formCallsToArms(ctx, &tally)
	if base+extra <= 0 && packRoom <= 0 {
		return
	}

	seats, err := m.idleFleetCommanders(ctx)
	if err != nil {
		rlogf("rolebots: gang formation: %v", err)
		return
	}
	used := make(map[int64]bool, base+extra)
	// Pass one: the base budget, in the order the draw returned it.
	m.formFleets(ctx, seats, base, used, &tally, nil)
	// Pass two: the reserved budget, security seats only.
	m.formFleets(ctx, seats, extra, used, &tally, func(seat fcSeat) bool {
		return seat.activity == ActSecurity
	})
	// Pass three: the PvP packs, on their own budget so a shard that wants fights does not
	// have to take fleets off its rats. The seats are combat ones that did not just sail.
	if packRoom > 0 && lethalityOn() {
		var packSeats []fcSeat
		for _, s := range seats {
			if s.activity == ActCombat && !used[s.charID] {
				s.pack = true
				packSeats = append(packSeats, s)
			}
			// A Predation piracy seat raises a gank fleet (gank_op.go) from this budget, only with the
			// dial on and a gank doctrine derived.
			if s.activity == ActPiracy && !used[s.charID] && gankOn() &&
				m.corpDoctrineFor(ctx, s.corpID) == DoctrinePredation {
				s.pack, s.gank = true, true
				packSeats = append(packSeats, s)
			}
		}
		// A corporation at war gets its pack first, and one more than the dial allows: a war is
		// fought by one pooled wing (warhunt.go).
		var nWar int
		packSeats, nWar = warSeatsFirst(packSeats, relCorpAtWar)
		if nWar > 0 {
			packRoom = packBudget(int(dialPackBudget.Get())+1, MaxGangs(), total, base+extra)
		}
		m.formFleets(ctx, packSeats, packRoom, used, &tally, nil)
	}
	tally.report(m)
}

// formBudget splits one pass's allowance into the base every seat competes for and the
// reserved slots only a security seat may take. gangCap is the operator's flat ceiling on
// standing fleets (0 = uncapped) and total is how many stand now. Under a tight cap the
// base is cut first, since the reserved budget has a live work order behind it.
func formBudget(distressLive bool, gangCap, total int) (base, extra int) {
	base = GangFormPerPass()
	if distressLive {
		extra = beltReliefExtra
	}
	if gangCap <= 0 {
		return base, extra
	}
	left := gangCap - total
	if left <= 0 {
		return 0, 0
	}
	if base+extra <= left {
		return base, extra
	}
	if base = left - extra; base < 0 {
		return 0, left
	}
	return base, extra
}

// packBudget is how many PvP packs one pass may raise: the dial, held inside the
// operator's flat ceiling on standing fleets (0 = uncapped) after the ordinary
// budget has taken what it needs. Pure.
func packBudget(dial, gangCap, total, ordinary int) int {
	if dial <= 0 {
		return 0
	}
	if gangCap <= 0 {
		return dial
	}
	left := gangCap - total - ordinary
	if left <= 0 {
		return 0
	}
	if dial > left {
		return left
	}
	return dial
}

// formTally is one pass's outcome, so the operator can see why a seat did not sail. Every
// refusal in formFleetAround is a silent (false, nil), so one aggregated line per pass is
// kept instead of a line per seat.
type formTally struct {
	formed   map[string]int
	refused  map[string]int
	reporter int64
}

func (t *formTally) form(seat fcSeat) {
	if t.formed == nil {
		t.formed = map[string]int{}
	}
	t.formed[seat.activity]++
	if t.reporter == 0 {
		t.reporter = seat.charID
	}
}

func (t *formTally) refuse(seat fcSeat, reason string) {
	if t.refused == nil {
		t.refused = map[string]int{}
	}
	t.refused[reason]++
	if t.reporter == 0 {
		t.reporter = seat.charID
	}
}

func (t *formTally) report(m *Mod) {
	if len(t.formed) == 0 && len(t.refused) == 0 {
		return
	}
	m.logBot(t.reporter, logInfo, "gang", "formation pass: formed %s; refused %s",
		tallyList(t.formed), tallyList(t.refused))
}

// tallyList renders a count map in descending order, ties broken by name so
// one pass's line is comparable with the next.
func tallyList(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	total := 0
	for k, n := range counts {
		keys = append(keys, k)
		total += n
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return fmt.Sprintf("%d (%s)", total, strings.Join(parts, ", "))
}

// formFleets spends up to `room` of the budget on the first seats that will
// take it, skipping any seat an earlier pass already used. want, when set,
// narrows which seats this budget may be spent on.
func (m *Mod) formFleets(ctx context.Context, seats []fcSeat, room int,
	used map[int64]bool, tally *formTally, want func(fcSeat) bool) {
	if room <= 0 {
		return
	}
	formed := 0
	for _, seat := range seats {
		if formed >= room || ctx.Err() != nil {
			return
		}
		if used[seat.charID] || (want != nil && !want(seat)) {
			continue
		}
		ok, why, err := m.formFleetAround(ctx, seat, nil)
		if err != nil {
			m.logBot(seat.charID, logError, "gang", "forming a fleet: %v", err)
			continue
		}
		used[seat.charID] = true
		if ok {
			formed++
			tally.form(seat)
			continue
		}
		tally.refuse(seat, why)
	}
}

// sweepAutoGangs retires auto-formed gangs whose time is up, whose leader is gone or that
// shrank below a gang. Retirement is a journey: the gang winds down, flies back to staging
// and the leader runs the settlement round (gangplan.go). A gang that cannot make it is
// deleted on the spot: no leader, no staging station, or wind-down past its grace.
func (m *Mod) sweepAutoGangs(ctx context.Context) {
	// A siege's fleet lives as long as its siege (siegefleet.go): its expiry is pushed on, or
	// brought to now, before the sweep reads it.
	m.leaseSiegeGangs(ctx)
	rows, err := m.db.QueryContext(ctx, `
		SELECT g.gangID, g.name, g.activity, g.leaderCharID, g.corpID, g.createdDate,
		       COALESCE(g.state, ''),
		       g.expiresAt > 0 AND g.expiresAt < ?,
		       gm.characterID IS NULL OR COALESCE(b.enabled, 0) = 0,
		       (SELECT COUNT(*) FROM rolebot_gang_members x WHERE x.gangID = g.gangID)
		FROM rolebot_gangs g
		LEFT JOIN rolebot_gang_members gm
		       ON gm.gangID = g.gangID AND gm.characterID = g.leaderCharID
		LEFT JOIN rolebot_characters b ON b.characterID = g.leaderCharID
		WHERE g.autoFormed = 1`, clock.Unix())
	if err != nil {
		rlogf("rolebots: gang sweep: %v", err)
		return
	}
	type dueGang struct {
		gangID   int64
		name     string
		activity string
		leaderID int64
		corpID   int64
		created  int64
		state    GangState
		why      string
		hardKill bool
	}
	now := clock.Unix()
	var due, stuck, settling, mining []dueGang
	asGang := func(d dueGang) *Gang {
		return &Gang{GangID: d.gangID, Name: d.name, Activity: d.activity,
			LeaderCharID: d.leaderID, CorpID: d.corpID, Created: d.created, State: d.state}
	}
	for rows.Next() {
		var d dueGang
		var rawState string
		var members int64
		var expired, leaderless bool
		if rows.Scan(&d.gangID, &d.name, &d.activity, &d.leaderID, &d.corpID, &d.created,
			&rawState, &expired, &leaderless, &members) != nil {
			continue
		}
		if rawState != "" {
			_ = json.Unmarshal([]byte(rawState), &d.state)
		}
		switch {
		case leaderless:
			// No leader, no settlement round: nobody can run it.
			d.why, d.hardKill = "its leader is gone", true
		case d.state.OpPhase == gangOpMuster && d.state.MusterDeadline > 0:
			// Still gathering: counted here every pass, since a commander not taking its muster step
			// never runs the head-count (musterResolve) and a fleet whose core is in releases early.
			stuck = append(stuck, d)
			continue
		case d.state.OpPhase == gangOpWindDown:
			// Already on its way home. Only a wind-down past its deadline plus a full grace window is
			// wedged enough to kill; the settle step settles with whoever made it back.
			if d.state.WindDownDeadline > 0 && now > d.state.WindDownDeadline+WindDownSec() {
				d.why, d.hardKill = "the wind-down never settled", true
			} else {
				// Counted here every pass for the same reason as a muster: a commander that is
				// excused or away never takes its settle step (settleResolve).
				settling = append(settling, d)
				continue
			}
		case expired:
			d.why = "the op has run its course"
		case members < gangMinMembers:
			d.why = "not enough members left"
		default:
			if d.activity == ActMining || d.activity == ActIndMining {
				mining = append(mining, d)
			}
			continue
		}
		due = append(due, d)
	}
	rows.Close()
	// The mustering fleets first: releasing one stops it becoming a disband, and a fleet
	// released here is live before anything below runs.
	for _, d := range stuck {
		if err := m.musterResolve(ctx, asGang(d)); err != nil {
			rlogf("rolebots: muster resolve gang %d: %v", d.gangID, err)
		}
	}
	for _, d := range settling {
		if _, err := m.settleResolve(ctx, asGang(d)); err != nil {
			rlogf("rolebots: settle resolve gang %d: %v", d.gangID, err)
		}
	}
	// A live mining op is watched for its haul (liveOpCoversHaul).
	for _, d := range mining {
		if err := m.liveOpCoversHaul(ctx, asGang(d)); err != nil {
			rlogf("rolebots: haul watch gang %d: %v", d.gangID, err)
		}
	}
	for _, d := range due {
		// Only a mining op with a staging station winds down (gangsettle.go); the rest disband
		// where they stand. The after-action report goes out before deleteGang erases the roster.
		if !d.hardKill && d.state.StagingStationID != 0 &&
			(d.activity == ActMining || d.activity == ActIndMining) {
			d.state.OpPhase = gangOpWindDown
			d.state.WindDownDeadline = now + WindDownSec()
			if err := saveGangState(ctx, m.db, d.gangID, &d.state); err != nil {
				rlogf("rolebots: wind down gang %d: %v", d.gangID, err)
				continue
			}
			m.logBot(d.leaderID, logInfo, "gang",
				"gang %q winds down — %s; the fleet returns to staging to settle up",
				d.name, d.why)
			continue
		}
		// A hard kill never had a settlement round and deleteGang drops the ore ledgers: pay what
		// the commander still can and write the rest off in both logs (gangindustry.go).
		if d.hardKill {
			m.settleFleetOreBeforeDisband(ctx, d.gangID, d.name)
			// Whatever is still in the op's cans is abandoned with the fleet; sweepCans books the
			// loss when they expire.
			if d.state.SystemID != 0 {
				if m3 := m.gangCanM3At(ctx, d.gangID, d.state.SystemID); m3 > 0 {
					m.logBot(0, logInfo, "gang",
						"gang %q is being disbanded with %.0f m³ still in its cans at %s — abandoned",
						d.name, m3, d.state.SystemName)
				}
			}
		}
		m.mailFleetReport(ctx, asGang(d), d.why)
		if err := deleteGang(ctx, m.db, d.gangID); err != nil {
			rlogf("rolebots: disband gang %d: %v", d.gangID, err)
			continue
		}
		// Only a fleet that fell apart counts against its corporation's next formation; one
		// that ran its course or was hard-killed says nothing about the next.
		if d.why == "not enough members left" {
			noteFleetDiedYoung(d.corpID, d.activity, d.created)
		}
		m.logBot(0, logInfo, "gang", "gang %q disbanded — %s; members return to their own rotations",
			d.name, d.why)
	}
}

// fcSeat is one idle fleet-commander seat the sweep may act on.
type fcSeat struct {
	corpID   int64
	charID   int64
	activity string
	// pack marks a seat the PvP pack budget is spending (gangFormPass): the fleet it forms
	// is a small hunting pack, not the corporation's ordinary outing.
	pack bool
	// gank marks a pack seat raising a gank fleet (gank_op.go): a Predation corporation's
	// piracy seat, under the gank doctrine, when the gank dial is on. Sized and budgeted as a
	// pack; always also pack.
	gank bool
	// forced marks a seat a GM raised by hand (/rolebots gank, slashcmd.go):
	// the dial is skipped, the doctrine and the roster are not.
	forced bool
}

// formFloor is the smallest fleet formFleetAround raises, commander included. A combat,
// security or piracy fleet the pass raises on its own needs GangFormMin, so it outlives one
// excusal (the sweep disbands below gangMinMembers). An ordered fleet is whatever size the
// order could fill, and a mining op is held to its own hauler rule instead. Pure.
func formFloor(activity string, ordered bool) int {
	if ordered {
		return gangMinMembers
	}
	switch activity {
	case ActCombat, ActSecurity, ActPiracy:
		return max(gangMinMembers, GangFormMin())
	}
	return gangMinMembers
}

// packFleetSize is a PvP pack's total strength, leader included: a tackler to
// hold the target and two guns to finish it. Past four the pack is a fleet and
// the ordinary combat seat already builds those.
const packFleetSize = 4

// idleFleetCommanders finds seated commanders who could take a fleet out right now: still
// an enabled rostered member of the corporation that seated them (the JOIN guard makes a
// stale seat inert), in no gang, not halted, not benched by the online cap.
func (m *Mod) idleFleetCommanders(ctx context.Context) ([]fcSeat, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.corporationID, r.characterID, r.activity
		FROM rolebot_corp_ranks r
		JOIN rolebot_characters b ON b.characterID = r.characterID AND b.enabled = 1
		JOIN character_ c ON c.characterID = r.characterID AND c.corporationID = r.corporationID
		LEFT JOIN rolebot_gang_members gm ON gm.characterID = r.characterID
		LEFT JOIN rolebot_state s ON s.characterID = r.characterID
		WHERE gm.characterID IS NULL AND COALESCE(s.halted, 0) = 0
		ORDER BY RAND()
		LIMIT ?`, GangLeaderScan())
	if err != nil {
		return nil, fmt.Errorf("idleFleetCommanders: %w", err)
	}
	defer rows.Close()
	var out []fcSeat
	for rows.Next() {
		var s fcSeat
		if rows.Scan(&s.corpID, &s.charID, &s.activity) == nil && !popIsOffline(s.charID) && !tzShortHours(s.charID) {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

// formFleetAround forms one fleet around one seated commander, or reports why it did not.
// The second return is a short label the pass aggregates (formTally); every refusal waits
// for the next sweep. order, when set, is the chair's own fleet order
// (corpcommanddesk.go): size, life and destination come from it.
func (m *Mod) formFleetAround(ctx context.Context, seat fcSeat, order *fleetOrderSpec) (bool, string, error) {
	lb, err := m.loadRun(ctx, seat.charID)
	if err != nil {
		return false, "", err
	}
	activity := seat.activity
	// A combat seat in an outlaw outfit is flown as a raid when its commander has the appetite
	// (PiracyAppetite); a lawful commander of the same seat takes the hunting pack.
	if order == nil && activity == ActCombat && !seat.pack && lb.Tune.PiracyAppetite > 0 &&
		m.corpDoctrineFor(ctx, seat.corpID) == DoctrinePredation {
		activity = ActPiracy
	}
	// A pack is a combat fleet of a PvP-leaning commander in a corporation whose doctrine
	// hunts pilots. The pass picks the seats; this re-checks, so a stale seat cannot raise one.
	switch {
	case seat.gank:
		// A gank squad (gank_op.go) is a Predation corporation's piracy seat under the gank dial,
		// or a GM's /rolebots gank, which skips only the dial.
		if activity != ActPiracy || !lethalityOn() {
			return false, "not a gank commander", nil
		}
		if !gankOn() && !seat.forced {
			return false, "gank dial off", nil
		}
		if m.corpDoctrineFor(ctx, seat.corpID) != DoctrinePredation {
			return false, "doctrine does not gank", nil
		}
	case seat.pack:
		if activity != ActCombat || !lethalityOn() ||
			m.jobForPilot(ctx, lb.CharID, ActCombat) != JobPvP {
			return false, "not a pack commander", nil
		}
		switch m.corpDoctrineFor(ctx, seat.corpID) {
		case DoctrineSecurity, DoctrineMercenary, DoctrinePredation:
		default:
			return false, "doctrine does not hunt pilots", nil
		}
	}
	// A corporation whose last fleet of this kind fell apart waits before raising another
	// (formbackoff.go). A chair's order and a GM's hand are not held back.
	if order == nil && !seat.forced && formBackedOff(seat.corpID, activity) > 0 {
		return false, "backing off after a fleet that fell apart", nil
	}
	// A commander excused from a seat inside the last half-hour (gangSeatReady) would be
	// excused from this one too: plan() skips gang duty while the memo holds.
	if lb.State != nil && lb.State.Data.GangUnfitUntil > clock.Unix() {
		return false, "commander excused", nil
	}
	// The commander must itself fly the op, by the probe the appetite draw uses. No fit means
	// a docked commander, not an error.
	seatJob := fleetActivityJob(activity)
	if seatJob == JobCombat {
		// The PvP-leaning commander is judged on the lens it will fly (pvplens.go).
		seatJob = m.jobForPilot(ctx, lb.CharID, activity)
	}
	if ok, err := m.canWorkJob(ctx, lb.CharID, seatJob); err != nil || !ok {
		return false, "cannot fly the op", err
	}
	// The commander must own a hull for its own seat (gangLeaderRole), not the fleet's slug,
	// or gangSeatReady would excuse it. The seat waits for ensureActivityShip.
	if ok, err := m.hasSeatShip(ctx, lb.CharID, seatActivityFor(activity, gangLeaderRole(activity))); err != nil || !ok {
		return false, "no ship", err
	}
	// The industry fleet is retired (gangindustry.go); a seat still carrying the slug waits
	// rather than sailing it.
	if activity == ActIndMining {
		return false, "indmining retired", nil
	}
	// A corporation's mining ops, NPC or player, are rationed by its main
	// focus (miningOpRationFor).
	if activity == ActMining {
		ok, err := m.corpMiningOpAllowed(ctx, seat.corpID)
		if err != nil {
			return false, "", err
		}
		if !ok {
			return false, "mining op ration spent — too soon since the last one", nil
		}
	}
	// A co-op mining fleet buys nothing (fleetore.go handBackFleetOre), so the commander needs
	// a home to stage from and hand rock back at, not a wallet.
	if fleetSharesOre(activity) && m.homeStationFor(ctx, lb.CharID) == 0 {
		return false, "no home station", nil
	}
	// What the fleet is for sizes it (gangtier.go): a target the recruiter stops asking at,
	// never a lid on who may join later. A call to arms is uncapped; a chair's order is its own size.
	tier := m.fleetTierForSeat(ctx, lb, seat, activity, order, rand.Intn)
	capacity := tierTarget(tier, rand.Intn)
	// The corporation's doctrine for this outing (fleetdoctrine_db.go). Nil means
	// gangComposition's single curve: operator gangs, mining fleets, a corporation with none.
	doc := m.fleetDoctrineForFleet(ctx, seat, activity, order)
	// A gank needs the gank doctrine (a scout and destroyers); a seat flagged for one with no
	// such doctrine waits rather than raising a raid in its name.
	if seat.gank && (doc == nil || doc.Kind != kindGank) {
		return false, "no gank doctrine", nil
	}
	// The corporation's shelf (corparmory_issue.go): a recruit with no hull in its seat's
	// band may be lent a kit, reserved here and issued once the gang row exists.
	ctx = m.withKitPool(ctx, seat.corpID)
	if order != nil && order.SiegeID != 0 && len(order.SiegeCorps) > 0 {
		// A siege fleet lends any member's shelf to any member's pilot (siegekits.go).
		ctx = m.withKitPools(ctx, order.SiegeCorps)
	}
	// The commander takes the line's gun seat (specFor("")) and must own a hull in its band
	// like every recruit (ship() in gangRecruitFrom), or it is excused at its own muster. The
	// seat waits.
	var leadSpec seatSpec
	leadAct := seatActivityFor(activity, gangLeaderRole(activity))
	leadKit := false // the commander's seat is a kit off the corporation's shelf
	if doc != nil {
		leadSpec = doc.specFor("")
		ok, err := m.hasSeatShipIn(ctx, lb.CharID, leadAct, leadSpec)
		if err != nil {
			return false, "commander has no hull in band", err
		}
		if !ok {
			ok = m.kitPoolFind(ctx, lb.CharID, gangLeaderRole(activity), leadSpec, true)
			leadKit = ok
		}
		if !ok {
			band := leadSpec.HullMax.String()
			if leadSpec.HullMin != leadSpec.HullMax {
				band = leadSpec.HullMin.String() + "–" + leadSpec.HullMax.String()
			}
			m.logBot(seat.charID, logInfo, "gang",
				"cannot command the %s doctrine — owns no %s hull; waiting for one",
				doc.Kind.Label(), band)
			return false, "commander has no hull in band", nil
		}
	}
	// The probes above pass a hull that is the right class but half-fitted. The commander is
	// held to the strict seat test every pick gets (recruitSeatOK), or gangSeatReady excuses
	// it on its first step and the fleet goes with it. A lent kit is fitted by the shelf.
	if !leadKit && !m.recruitSeatOK(ctx, lb.CharID, leadAct, leadSpec) {
		return false, "commander not seat-ready", nil
	}
	// recruitSeatOK left the commander's seat primed; a fleet that does not form forgets it.
	formed := false
	defer func() {
		if !formed {
			forgetSeatSpec(lb.CharID)
		}
	}()
	// A siege's one fleet (siegefleet.go): the alliance's patrols come off their beats into it,
	// its reach is measured from its forward staging, and only the siege's corporations join.
	siege := order != nil && order.SiegeID != 0
	if siege {
		if order.StagingStationID != 0 {
			ctx = withRecruitOrigin(ctx, m.stationSystemOf(ctx, order.StagingStationID))
		}
		ctx = withRecruitCorps(ctx, order.SiegeCorps)
		if n := m.pullPatrolsForSiege(ctx, order.SiegeCorps, order.SystemName); n > 0 {
			m.logBot(seat.charID, logInfo, "gang", "pulled %d patrol(s) off their beats into the siege muster for %s",
				n, order.SystemName)
		}
	}
	recruits, err := m.gangRecruit(ctx, lb, activity, capacity-1, seat.pack, doc)
	if err != nil {
		return false, "", err
	}
	// Then the guns, if this fleet has none of its own and may ask (gangescort.go). Escorts
	// spend the same capacity, so they are asked for after the working seats.
	if want := escortQuota(activity, recruits, capacity); want > 0 && !seat.pack {
		escorts, err := m.gangRecruitEscorts(ctx, lb, want)
		if err != nil {
			// A fleet that could not borrow guns flies unescorted; that is not worth failing over.
			m.logBot(seat.charID, logError, "gang", "could not look for an escort: %v", err)
		}
		recruits = append(recruits, escorts...)
	}
	// Then the alliance (gangalliance.go) for the seats the corporation could not fill, then
	// the mutual allies for whatever is left.
	if mates, err := m.gangRecruitAlliance(ctx, lb, activity, capacity-1, recruits, seat.pack, doc); err != nil {
		m.logBot(seat.charID, logError, "gang", "could not look for alliance pilots: %v", err)
	} else {
		recruits = append(recruits, mates...)
	}
	if allied, err := m.gangRecruitAllies(ctx, lb, activity, capacity-1, recruits, seat.pack, doc); err != nil {
		m.logBot(seat.charID, logError, "gang", "could not look for allied pilots: %v", err)
	} else {
		recruits = append(recruits, allied...)
	}
	floor := formFloor(activity, order != nil || seat.forced)
	if len(recruits)+1 < floor {
		return false, "short of crew", nil
	}
	// A siege fleet whose recruits cannot bring the quota on paper is not flown to a muster that
	// will not fill: the siege runs out its muster window and closes short (siegeStageNext).
	if siege && order.QuotaDPS > 0 {
		paper := m.pilotSiegeDPS(ctx, lb.CharID)
		for _, r := range recruits {
			paper += m.pilotSiegeDPS(ctx, r.charID)
		}
		if paper < siegeFormFirepowerFrac*order.QuotaDPS {
			return false, fmt.Sprintf("short of firepower (%.0f of %.0f DPS)", paper, order.QuotaDPS), nil
		}
	}
	// A mining op with nobody in an industrial to haul it does not form: musterCoversTheOp
	// would stand it down at the deadline after every member had travelled.
	if fleetAttributesOre(activity) && !recruitsHaveHauler(recruits) {
		return false, "no industrial hauler", nil
	}

	// The fleet flies its corporation's name, not its commander's, so every fleet the corp
	// fields reads uniformly.
	name := gangNameFor(m.corpNameOf(ctx, seat.corpID), activity)
	if seat.pack {
		name = m.corpNameOf(ctx, seat.corpID) + " Hunting Party"
	}
	if seat.gank {
		name = m.corpNameOf(ctx, seat.corpID) + " Gank Squad"
	}
	if siege {
		name = siegeFleetName(m.corpNameOf(ctx, seat.corpID))
	}
	life := int64(gangLifeMinSec + rand.Intn(gangLifeMaxSec-gangLifeMinSec+1))
	if order != nil && order.LifeSec > 0 {
		life = order.LifeSec
	}
	now := clock.Unix()

	// Every fleet musters before it works: the op starts at the leader's home station and
	// ends there (the wind-down). A leader with no home on record skips the muster and runs
	// live from birth, like a legacy row.
	var stateJSON string
	var reliefName string
	var stagingSys int64 // the chatter line names it (chatter.go)
	staging := m.homeStationFor(ctx, seat.charID)
	if siege && order.StagingStationID != 0 {
		// A siege fleet musters forward of its target, not at the commander's home.
		staging = order.StagingStationID
	}
	if staging != 0 {
		stagingSys = m.stationSystemOf(ctx, staging)
		if stagingSys != 0 {
			gs := GangState{
				OpPhase:          gangOpMuster,
				StagingStationID: staging,
				StagingSystemID:  stagingSys,
				MusterDeadline:   now + musterWindowFor(recruits),
				MusterStartedAt:  now,
				Tier:             tier.Name,
			}
			// A call to arms gathers fast: the tower has a timer, and the quorum release goes with
			// whoever is in.
			if tier.Name == tierCallToArms && gs.MusterDeadline > now+callToArmsMuster {
				gs.MusterDeadline = now + callToArmsMuster
			}
			if siege {
				// The whole alliance flies to a forward staging; the muster is judged on DPS, not a
				// quorum, so it gets the time the spread of recruits needs, within bounds.
				gs.MusterDeadline = now + siegeMusterDeadline(musterWindowFor(recruits))
				gs.SiegeID = order.SiegeID
			}
			if order != nil {
				gs.HelpCallID = order.HelpCallID
			}
			// A fleet born live has no waiting room (gangtier.go). Staging is still recorded as the
			// wind-down point, and members are called off interruptible errands for a quarter-hour.
			if !tier.Muster {
				gs.OpPhase = gangOpLive
				gs.MusterDeadline, gs.MusterStartedAt = 0, 0
				gs.JoinBy = now + hunterJoinWindowSec
			}
			// A fleet born while the distress board has rows in its scope is stamped with the loudest
			// cry (beltdistress.go). The scope differs by activity: see
			// docs/comment-notes-b10.md#relief-scope.
			var reliefScope map[int64]bool
			switch activity {
			case ActCombat:
				reliefScope = m.economicRules(ctx, lb, defaultBand(lb)).Regions
			case ActSecurity:
				reliefScope = m.economicRules(ctx, lb, bandHome).Regions
			}
			if seat.pack {
				gs.Focus = gangFocusPvP
			}
			if seat.gank {
				gs.Focus = gangFocusGank
			}
			// Some Security or Mercenary patrols hold a gate instead of sweeping belts (camp.go): a
			// checkpoint, by the share the dial sets.
			if activity == ActSecurity && !seat.pack && checkpointOn() &&
				checkpointDoctrine(m.corpDoctrineFor(ctx, seat.corpID)) &&
				rand.Float64() < dialCheckpointShare.Get() {
				gs.Focus = gangFocusCheckpoint
			}
			// The fleet's command (fleetcommand.go): the commander, the succession by Leadership, the
			// kind's call and retreat rule at the commander's nerve. Only a doctrine fleet gets one.
			if doc != nil {
				gs.Command = defaultFleetCommand(doc.Kind, seat.charID,
					m.buildSuccession(ctx, activity, recruits), fcNerve(lb.Tune))
			}
			switch {
			case order != nil && order.SystemID != 0:
				// The chair said where. A mining op is pinned there (gangPinnedMiningSite); a hunt or
				// patrol takes it as its first sweep and standing order.
				gs.OrderID = order.CommandID
				gs.OrderOres = order.OreNames
				if activity == ActMining || activity == ActIndMining {
					gs.PinSystemID = order.SystemID
					gs.OreTypeID = order.OreTypeID
				} else {
					gs.ReliefSystemID, gs.ReliefSystemName = order.SystemID, order.SystemName
					gs.OrderSystemID = order.SystemID
					reliefName = order.SystemName
				}
			case (activity == ActCombat || activity == ActSecurity) && !seat.pack:
				if sysID, sysName := m.pickReliefSystem(ctx, lb.CorpID, reliefScope); sysID != 0 {
					gs.ReliefSystemID, gs.ReliefSystemName = sysID, sysName
					reliefName = sysName
				}
			}
			if raw, err := json.Marshal(&gs); err == nil {
				stateJSON = string(raw)
			}
		}
	}

	var kind string
	if doc != nil {
		kind = string(doc.Kind)
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_gangs
			(name, enabled, activity, config, state, leaderCharID, corpID, autoFormed, expiresAt, createdDate, updatedAt, doctrineKind)
		VALUES (?, 1, ?, '', ?, ?, ?, 1, ?, ?, ?, ?)`,
		name, activity, stateJSON, seat.charID, seat.corpID, now+life, now, now, kind)
	if err != nil {
		return false, "", fmt.Errorf("form fleet: %w", err)
	}
	gangID, _ := res.LastInsertId()
	if siege {
		if _, err := m.db.ExecContext(ctx, `UPDATE rolebot_corp_siege SET gangID = ? WHERE siegeID = ?`,
			gangID, order.SiegeID); err != nil {
			rlogf("rolebots: link siege %d to fleet %d: %v", order.SiegeID, gangID, err)
		}
	}
	if doc != nil {
		m.logBot(seat.charID, logInfo, "gang", "flying the %s doctrine — %s", doc.Kind.Label(), doctrineSummary(*doc))
	}

	// The commander takes the line's gun seat under the doctrine (leadSpec) and every recruit
	// the seat it was picked for. A fleet without a doctrine writes rows with no spec.
	if err := addGangMemberWithSpec(ctx, m.db, gangID, seat.charID, gangLeaderRole(activity), 0, leadSpec); err != nil {
		_ = deleteGang(ctx, m.db, gangID)
		return false, "", err
	}
	joined := []string{lb.Name}
	var seconded []string
	for _, r := range recruits {
		if err := addGangMemberWithSpec(ctx, m.db, gangID, r.charID, r.role, r.fromCorpID, r.spec); err != nil {
			// Somebody else's fleet got them between the scan and now: a smaller outing, not a failure.
			continue
		}
		joined = append(joined, r.name)
		if r.fromCorpID != 0 {
			seconded = append(seconded, r.name)
		}
	}
	if n, _ := gangSize(ctx, m.db, gangID); n < floor {
		_ = deleteGang(ctx, m.db, gangID)
		return false, "lost its crew", nil
	}
	formed = true
	m.logBot(seat.charID, logInfo, "gang",
		"took %q out for its corp (%s, %d pilots) with %s — a %s fleet, raised for %s; winding down in about %dh%02dm",
		name, activity, len(joined), joinAnd(joined), tier.Name, tier.Why, life/3600, (life%3600)/60)
	// Said in corp chat if a person is there (chatter.go); the line offers a seat, and
	// "join" in reply goes through requestSeat like the portal's.
	fleetSlots := map[string]string{
		"fleet": name, "activity": chatterActivityWord(activity), "n": strconv.Itoa(len(joined))}
	if stagingSys != 0 {
		fleetSlots["system"] = m.nameOfSystem(ctx, stagingSys)
	}
	m.chatterNote(ctx, chatterEvent{Kind: "fleet.formed", ChannelID: seat.corpID, CorpID: seat.corpID,
		ActorID: seat.charID, Slots: fleetSlots, Ref: gangID})
	// Lent kits go out now that the gang exists (corparmory_issue.go). A Yard industrial in a
	// hauler's hands makes the hauler grant moot.
	if m.issueFleetKits(ctx, gangID, now+life) == 0 {
		// A co-op whose haulers are all in frigates: a managed corporation puts an industrial in
		// a hauler's hands (gangsupply.go).
		m.corpProvidesHauler(ctx, lb, seat.corpID, activity, recruits)
	}
	if reliefName != "" {
		m.logBot(seat.charID, logInfo, "gang",
			"formed to answer a distress signal — first sweep: %s", reliefName)
	}
	if len(seconded) > 0 {
		m.logBot(seat.charID, logInfo, "gang",
			"flying with %s, seconded from a friendly or allied outfit", joinAnd(seconded))
	}
	return true, "", nil
}

// musterWindowFor is how long the fleet waits at staging, in seconds: the operator's
// setting, or the flight time of the furthest recruit if that is longer. The setting is a
// floor, so a fleet recruited across the neighbourhood still waits out the flight it asked
// for. The budget prices the worst case: every gate at the slow end, plus slack.
func musterWindowFor(recruits []gangRecruitPick) int64 {
	window := MusterTimeoutSec()
	far := 0
	for _, r := range recruits {
		if r.jumps > far {
			far = r.jumps
		}
	}
	if far == 0 {
		return window
	}
	if budget := int64(far)*jumpLegMaxSec + musterTravelSlackSec; budget > window {
		return budget
	}
	return window
}

// gangRecruitPick is one accepted recruit.
type gangRecruitPick struct {
	charID int64
	name   string
	role   string
	// jumps from the commander's system, which is what the muster deadline
	// is sized from (musterDeadlineFor).
	jumps int
	// fromCorpID is the corporation the pilot was recruited out of; 0 means the fleet's own.
	// Only the escort pass (gangescort.go) fills it, and the membership row records it so the
	// welcome re-check knows who is a guest.
	fromCorpID int64
	// spec is the seat's hull band, layer and range band under the fleet's
	// doctrine (fleetdoctrine_seat.go); zero for a fleet without one.
	spec seatSpec
}

// gangRecruit fills the composition's seats from the commander's own corporation:
// home-station colleagues first, then the wider corp by distance, each pilot checked
// against the seat's fit-pool probe (and, for a pirate fleet, PiracyAppetite).
func (m *Mod) gangRecruit(ctx context.Context, lb *botRun, activity string, seats int, pack bool, doc *FleetDoctrine) ([]gangRecruitPick, error) {
	return m.gangRecruitFrom(ctx, lb, activity, seats, []int64{lb.CorpID}, nil, pack, doc)
}

// gangAllySeatActivities are the fleets a mutual ally's pilots may take a working seat in:
// fighting fleets only. A mining op's cargo payout is booked to its own corporation, so it
// borrows allied guns through the paid escort pass (gangescort.go).
var gangAllySeatActivities = map[string]bool{ActCombat: true, ActSecurity: true, ActPiracy: true}

// gangRecruitAllies fills whatever seats the commander's own corporation left empty from
// its mutual allies (relAlliesOf). Guests are recorded with the corporation they came
// from, like seconded escorts, so gangEscortStillWelcome sends them home if the alliance
// cools.
func (m *Mod) gangRecruitAllies(ctx context.Context, lb *botRun, activity string, seats int, own []gangRecruitPick, pack bool, doc *FleetDoctrine) ([]gangRecruitPick, error) {
	if !gangAllySeatActivities[activity] || len(own) >= seats {
		return nil, nil
	}
	allies := relAlliesOf(lb.CorpID)
	if len(allies) == 0 {
		return nil, nil
	}
	return m.gangRecruitFrom(ctx, lb, activity, seats, allies, own, pack, doc)
}

// gangRecruitFrom is the seat search over a set of corporations. prefilled
// are picks an earlier pass already made; their seats are taken before this
// pass looks, and only the new picks are returned. A pilot recruited from any
// corporation but the commander's own carries it as fromCorpID.
func (m *Mod) gangRecruitFrom(ctx context.Context, lb *botRun, activity string, seats int,
	corps []int64, prefilled []gangRecruitPick, pack bool, doc *FleetDoctrine) ([]gangRecruitPick, error) {
	if seats < 1 || len(corps) == 0 {
		return nil, nil
	}

	args := append([]any{lb.CharID}, int64Args(corps)...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT b.characterID, c.characterName, c.solarSystemID, COALESCE(c.homeStationID, 0),
		       c.corporationID, COALESCE(s.phase, ''), COALESCE(s.data, ''),
		       COALESCE(tt.value, 0)
		FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		LEFT JOIN rolebot_gang_members gm ON gm.characterID = b.characterID
		LEFT JOIN rolebot_state s ON s.characterID = b.characterID
		LEFT JOIN rolebot_traits tt ON tt.characterID = b.characterID AND tt.trait = '`+TraitTolerance+`'
		WHERE b.enabled = 1 AND gm.characterID IS NULL
		  AND COALESCE(s.halted, 0) = 0 AND b.characterID <> ?
		  AND c.corporationID IN (`+placeholders(len(corps))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("gangRecruit: %w", err)
	}
	type cand struct {
		charID, systemID, homeStation, corpID int64
		name                                  string
		jumps                                 int
		// free reports whether this pilot could break off what it is doing and come
		// (interruptibleState). It is a sort key, not a filter.
		free bool
		// tolerance is the candidate's own trait value, for the bloodline rule below.
		tolerance int
	}
	var cands []cand
	now := clock.Unix()
	// The SQL filter cannot see earlier passes' picks, which are not on the roster yet: an
	// alliance mate that is also a mutual ally would be counted twice.
	already := make(map[int64]bool, len(prefilled))
	for _, p := range prefilled {
		already[p.charID] = true
	}
	for rows.Next() {
		var c cand
		var phase, blob string
		if rows.Scan(&c.charID, &c.name, &c.systemID, &c.homeStation, &c.corpID, &phase, &blob, &c.tolerance) != nil {
			continue
		}
		if already[c.charID] {
			continue
		}
		st := &SimState{Phase: phase}
		if blob != "" {
			_ = json.Unmarshal([]byte(blob), &st.Data)
		}
		c.free = interruptibleState(st)
		// An excused pilot is still sorting its gear out, and seating it re-excuses it. A busy
		// pilot is recruited last unless rolebots.gang.recruitBusy is 0 (gangmuster.go).
		if st.Data.GangUnfitUntil > now || (!c.free && !RecruitBusy()) {
			continue
		}
		cands = append(cands, c)
	}
	rows.Close()

	u, err := m.universe(ctx)
	if err != nil {
		return nil, err
	}
	// A siege fleet measures reach (and with it the muster window) from its forward staging, not
	// from wherever its commander stands (siegefleet.go).
	origin := lb.SystemID
	if o := recruitOriginOf(ctx); o != 0 {
		origin = o
	}
	reach := gangReachMap(u, origin, gangRecruitMaxJumps)
	leaderHome := m.homeStationFor(ctx, lb.CharID)

	// A final-stage xenophobe shares a fleet with its own bloodline only (tolerance.go), on
	// either side of the invitation.
	leadRace, leadBlood := raceOf(lb.CharID), bloodlineOf(lb.CharID)
	kept := cands[:0]
	for _, c := range cands {
		j, ok := reach[c.systemID]
		// A pilot paged for a call to arms (armsping.go) comes from anywhere and whatever its hours:
		// it travels to the fight.
		held := armsHeld(c.charID)
		if !ok && held {
			j, ok = gangRecruitMaxJumps, true
		}
		if !ok || popIsOffline(c.charID) || (tzShortHours(c.charID) && !held) {
			continue
		}
		cRace, cBlood := raceOf(c.charID), bloodlineOf(c.charID)
		if xenoBloodlineBars(lb.Tune.Xenophobia, leadRace, leadBlood, cRace, cBlood) ||
			xenoBloodlineBars(posPole(traitScale(c.tolerance)), cRace, cBlood, leadRace, leadBlood) {
			continue
		}
		c.jumps = j
		kept = append(kept, c)
	}
	// The composition is sized to the pool, never to a number nobody could fill. The seats
	// are interleaved, so any prefix keeps the op's ratios.
	seats = min(seats, len(kept)+len(prefilled))
	if seats < 1 {
		return nil, nil
	}
	// The seats: the corporation's doctrine when the fleet has one (fleetdoctrine.go),
	// gangComposition's curve otherwise (fleetSlots).
	slots := m.fleetSlots(ctx, doc, activity, seats+1, pack)
	if len(slots) == 0 {
		return nil, nil
	}
	// Order: pilots who can come now, then home-station colleagues, then by distance. A busy
	// pilot is eligible but asked last, since it may not answer inside the muster window.
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].free != kept[j].free {
			return kept[i].free
		}
		si := leaderHome != 0 && kept[i].homeStation == leaderHome
		sj := leaderHome != 0 && kept[j].homeStation == leaderHome
		if si != sj {
			return si
		}
		return kept[i].jumps < kept[j].jumps
	})

	var picks []gangRecruitPick
	filled := make([]bool, len(slots))
	taken := make([]bool, len(kept))
	// The seats an earlier pass already filled: its role's slot first, and
	// any open slot for a pick whose slot was degraded to a gun.
	for _, p := range prefilled {
		at := -1
		for i, s := range slots {
			if !filled[i] && s.role == p.role {
				at = i
				break
			}
		}
		if at < 0 {
			for i := range slots {
				if !filled[i] {
					at = i
					break
				}
			}
		}
		if at >= 0 {
			filled[at] = true
		}
	}
	// Temperament, for every candidate at once (one query, not one per
	// pilot): what each seat ranks by, and the pacifist gate.
	ids := make([]int64, 0, len(kept))
	for _, c := range kept {
		ids = append(ids, c.charID)
	}
	traits, terr := loadTraitsBulk(ctx, m.db, ids)
	if terr != nil {
		traits = nil // unread temperament leaves every candidate at zero, so the order is unchanged
	}
	tunes := make([]Tuning, len(kept))
	for ci, c := range kept {
		tunes[ci] = resolveTuning(traits[c.charID])
	}
	leadStyle := styleFor(lb.Tune, 0)
	// canWorkJob builds a candidate pool per call; a seat-major pass asks the
	// same (pilot, job) question once per open seat of that job.
	type cwKey struct {
		ci  int
		job string
	}
	canWork := map[cwKey]bool{}
	can := func(ci int, job string) bool {
		k := cwKey{ci, job}
		if v, ok := canWork[k]; ok {
			return v
		}
		ok, err := m.canWorkJob(ctx, kept[ci].charID, job)
		v := err == nil && ok
		canWork[k] = v
		return v
	}
	// A ship for the seat, not only the skills: gangSeatReady excuses anyone with no hull, so
	// ask once per (pilot, seat activity) under the seat's spec (fleetdoctrine_seat.go).
	specOf := func(role string) seatSpec {
		// A mining op's hauler seat is an industrial seat, doctrine or not: a frigate carries too
		// little per trip and the backlog rots in the cans. The band rides the member row.
		if fleetAttributesOre(activity) && isHaulerRole(role) {
			return haulerSeatSpec
		}
		if doc == nil {
			return seatSpec{}
		}
		return doc.specFor(role)
	}
	hasShip := map[cwKey]bool{}
	ship := func(ci int, role string) bool {
		act := seatActivityFor(activity, role)
		k := cwKey{ci, act + "|" + role}
		if v, ok := hasShip[k]; ok {
			return v
		}
		ok, err := m.hasSeatShipIn(ctx, kept[ci].charID, act, specOf(role))
		v := err == nil && ok
		if !v {
			v = m.kitPoolFind(ctx, kept[ci].charID, role, specOf(role), false)
		}
		hasShip[k] = v
		return v
	}
	// Who owns an industrial: the hauler seat goes to them first (seatKey), since a hauler in
	// a frigate carries a fifth per trip and the whole op waits on the trip.
	indOwners := m.industrialOwners(ctx, ids)

	// Capable candidates only; the best temperament fit (seatAffinity) wins each open seat,
	// specialists first. Free beats busy before temperament; a tie keeps the earlier pilot.
	slotOf := map[int64]int{}
	// A siege line fills its guns first (seatFillOrderFor): it is won on DPS.
	siegeLine := doc != nil && doc.Kind == kindSiege
	fillPass := func() {
		for _, i := range seatFillOrderFor(slots, siegeLine) {
			if filled[i] || len(prefilled)+len(picks) == len(slots) {
				continue
			}
			s := slots[i]
			best, bestKey := -1, seatKey{}
			for ci, c := range kept {
				if taken[ci] {
					continue
				}
				tn := tunes[ci]
				// A pirate fleet is a crime whatever the seat, so every seat in one needs PiracyAppetite,
				// not only the JobPiracy ones.
				if activity == ActPiracy && tn.PiracyAppetite <= 0 {
					continue
				}
				// A pacifist flies the logistics seat or none.
				if !pacifistMaySit(tn, s.job) {
					continue
				}
				// A mining op's hauler seat takes a pilot that can come now: a busy one never
				// answers the muster, and the op stands down for want of a hauler.
				if fleetAttributesOre(activity) && isHaulerRole(s.role) && !c.free {
					continue
				}
				if !can(ci, s.job) || !ship(ci, s.role) {
					continue
				}
				k := seatKey{free: c.free, hull: s.job == JobTrade && indOwners[c.charID],
					aff: seatAffinityBucket(tn, s.job, leadStyle)}
				if best < 0 || k.beats(bestKey) {
					best, bestKey = ci, k
				}
			}
			if best < 0 {
				continue
			}
			c := kept[best]
			filled[i] = true
			taken[best] = true
			slotOf[c.charID] = i
			pick := gangRecruitPick{
				charID: c.charID, name: c.name, role: s.role, jumps: c.jumps, spec: specOf(s.role)}
			if c.corpID != lb.CorpID {
				pick.fromCorpID = c.corpID
			}
			picks = append(picks, pick)
		}
	}
	fillPass()

	// Seats nobody could fill degrade to the fleet's gun seat and leftover candidates get a
	// second look. Only combat fleets degrade; a mining op short a hauler is short.
	if activity == ActCombat || activity == ActSecurity || activity == ActPiracy {
		gunRole, gunJob := RoleHunter, JobCombat
		if activity == ActPiracy {
			gunRole, gunJob = RoleCombat, JobPiracy
		}
		degraded := false
		for i := range slots {
			if !filled[i] && slots[i].job != gunJob {
				slots[i] = gangSlot{gunRole, gunJob}
				degraded = true
			}
		}
		if degraded {
			fillPass()
		}
	}

	// The strict test, on the picks only: gangSeatReady also wants the seat's hull, fitted and
	// armed. A rejected pilot stays taken so the single refill pass does not re-offer the seat.
	passed := map[int64]bool{}
	for round := 0; round < 2; round++ {
		rejected := false
		keep := picks[:0]
		for _, p := range picks {
			if passed[p.charID] || m.recruitSeatOK(ctx, p.charID, seatActivityFor(activity, p.role), p.spec) ||
				m.kitPoolFind(ctx, p.charID, p.role, p.spec, true) {
				passed[p.charID] = true
				keep = append(keep, p)
				continue
			}
			if i, ok := slotOf[p.charID]; ok {
				filled[i] = false
			}
			rejected = true
		}
		picks = keep
		if !rejected || round == 1 {
			break
		}
		fillPass()
	}
	// A siege line with more logistics than its guns call for (an oversized slot list filled from
	// a thin pool) moves the surplus onto the guns where the pilot can fly one; one that cannot
	// keeps its seat, since a repairer on the line is still a body on it.
	if siegeLine {
		gunRole, gunJob := RoleHunter, JobCombat
		idx := make(map[int64]int, len(kept))
		for ci, c := range kept {
			idx[c.charID] = ci
		}
		roles := make([]string, len(picks))
		for i, p := range picks {
			roles[i] = p.role
		}
		for _, i := range excessSpecialists(roles, RoleLogi, siegeLogiPer) {
			ci, ok := idx[picks[i].charID]
			if !ok || !can(ci, gunJob) || !ship(ci, gunRole) {
				continue
			}
			if !m.recruitSeatOK(ctx, picks[i].charID, seatActivityFor(activity, gunRole), specOf(gunRole)) {
				continue
			}
			picks[i].role, picks[i].spec = gunRole, specOf(gunRole)
		}
	}
	return picks, nil
}

type recruitOriginKey struct{}
type recruitCorpsKey struct{}

// withRecruitOrigin makes the recruiter measure reach from sysID, a siege fleet's forward staging.
func withRecruitOrigin(ctx context.Context, sysID int64) context.Context {
	if sysID == 0 {
		return ctx
	}
	return context.WithValue(ctx, recruitOriginKey{}, sysID)
}

// recruitOriginOf is the system the recruiter measures reach from, 0 for the commander's own.
func recruitOriginOf(ctx context.Context) int64 {
	v, _ := ctx.Value(recruitOriginKey{}).(int64)
	return v
}

// withRecruitCorps limits the alliance pass to these corporations: a tower siege's fleet takes
// only the mates mateJoinsTowerSiege lets in.
func withRecruitCorps(ctx context.Context, corps []int64) context.Context {
	if corps == nil {
		return ctx
	}
	return context.WithValue(ctx, recruitCorpsKey{}, corps)
}

// recruitCorpsOf is the corporations the alliance pass may draw on, nil for every mate.
func recruitCorpsOf(ctx context.Context) []int64 {
	v, _ := ctx.Value(recruitCorpsKey{}).([]int64)
	return v
}

// recruitSeatOK runs the muster's strict seat test (seatVerdict) on one candidate, acting
// on nothing. An unreadable pilot is refused. It is asked under the seat (primeSeatSpec),
// because the fit a seated pilot is held to is generated against the seat's hull band;
// the cache entry is dropped on a refusal.
func (m *Mod) recruitSeatOK(ctx context.Context, charID int64, seatActivity string, spec seatSpec) bool {
	b, err := m.loadRun(ctx, charID)
	if err != nil || b == nil || b.State == nil {
		return false
	}
	primeSeatSpec(charID, spec)
	v, err := m.seatVerdict(ctx, b, seatActivity)
	ok := err == nil && v.why == ""
	if !ok {
		forgetSeatSpec(charID)
	}
	return ok
}

// gangReachMap is the BFS neighbourhood: systemID → jumps from `from`, out
// to maxJumps. Depth 0 is the leader's own system.
func gangReachMap(u *universe, from int64, maxJumps int) map[int64]int {
	out := map[int64]int{from: 0}
	frontier := []int64{from}
	for depth := 1; depth <= maxJumps && len(frontier) > 0; depth++ {
		var next []int64
		for _, sysID := range frontier {
			for _, nb := range u.adj[sysID] {
				if _, seen := out[nb]; seen {
					continue
				}
				out[nb] = depth
				next = append(next, nb)
			}
		}
		frontier = next
	}
	return out
}

// gangNameFor names the outing after its corporation: a fleet is the corp's function, so
// it reads "Deep Core Mining Inc. Mining Op" whoever commands it.
func gangNameFor(corpName, activity string) string {
	switch activity {
	case ActMining:
		return corpName + " Mining Op"
	case ActIndMining:
		return corpName + " Supply Run"
	case ActCombat:
		return corpName + " Hunting Pack"
	case ActSecurity:
		return corpName + " Security Patrol"
	case ActPiracy:
		return corpName + " Raiders"
	}
	return corpName + " Gang"
}

// ── seat temperament ─────────────────────────────────────────────────────────

// seatFillOrder is the order open seats are offered in: logistics first, then tackle and
// electronic warfare, then every other seat in table order. Specialists go first because
// they are the scarce answer: a gunner who could also fly logi must not be spent on a gun
// seat first.
func seatFillOrder(slots []gangSlot) []int {
	rank := func(job string) int {
		switch job {
		case JobLogi:
			return 0
		case JobTackle, JobEwar, JobScout:
			return 1
		}
		return 2
	}
	out := make([]int, len(slots))
	for i := range slots {
		out[i] = i
	}
	sort.SliceStable(out, func(a, b int) bool { return rank(slots[out[a]].job) < rank(slots[out[b]].job) })
	return out
}

// pacifistMaySit reports whether a pilot's temperament lets it take a seat of this job. A
// pacifist (Tuning.Pacifist) sits in a combat fleet in the logistics seat or not at all,
// never a gun, tackle or EWAR seat, and so never one a specialist seat degraded into.
// Peaceful seats are untouched.
func pacifistMaySit(tn Tuning, job string) bool {
	if !tn.Pacifist {
		return true
	}
	switch job {
	case JobCombat, JobPiracy, JobTackle, JobEwar:
		return false
	}
	return true
}

// seatAffinity is how well a pilot's temperament suits a seat. Logi reads LogiDrive,
// tackle the reckless pole, EWAR EwarDrive, and every gun seat the violence appetite. All
// are flat across an untraited roster, which leaves its order unchanged.
func seatAffinity(tn Tuning, job string) float64 {
	switch job {
	case JobLogi:
		return tn.LogiDrive
	case JobTackle:
		return tn.TackleDrive
	case JobEwar:
		return tn.EwarDrive
	case JobScout:
		return tn.ScoutDrive
	case JobCombat, JobPiracy:
		return tn.ViolenceAppetite
	}
	return 0
}

// seatStyleBonus is the soft doctrine lean: a gun seat prefers a pilot whose
// solo style matches the leader's, so a fleet tends to hold one engagement
// range. One affinity bucket, never more.
const seatStyleBonus = 0.1

// seatAffinityBucket is seatAffinity rounded to tenths, plus the style lean on gun seats.
// Rounded so two pilots of nearly the same temperament fall back on the home-station and
// distance order rather than a rounding error.
func seatAffinityBucket(tn Tuning, job string, leadStyle engageStyle) int {
	a := seatAffinity(tn, job)
	if (job == JobCombat || job == JobPiracy) && leadStyle != engageAuto && styleFor(tn, 0) == leadStyle {
		a += seatStyleBonus
	}
	return int(math.Round(a * 10))
}

// seatKey orders candidates for one seat: free beats busy, then, for a hauler's seat,
// owning an industrial, then affinity. Ties keep the earlier candidate, whose position
// already encodes home station and distance.
type seatKey struct {
	free bool
	hull bool // the seat's proper hull is already in this pilot's hangar
	aff  int
}

func (k seatKey) beats(o seatKey) bool {
	if k.free != o.free {
		return k.free
	}
	if k.hull != o.hull {
		return k.hull
	}
	return k.aff > o.aff
}

// ewarViable reports whether any electronic-warfare module is for sale anywhere, the
// market probe behind offering EWAR seats at all (like logiViable).
func (m *Mod) ewarViable(ctx context.Context) bool {
	ewarViableMu.Lock()
	if !ewarViableAt.IsZero() && clock.Now().Sub(ewarViableAt) < logiViableTTL {
		ok := ewarViableOK
		ewarViableMu.Unlock()
		return ok
	}
	ewarViableMu.Unlock()
	ok := false
	if cat, err := m.buyableModules(ctx); err == nil {
		for _, mm := range cat {
			if isEwarGroup(mm.GroupID) {
				ok = true
				break
			}
		}
	}
	ewarViableMu.Lock()
	ewarViableAt, ewarViableOK = clock.Now(), ok
	ewarViableMu.Unlock()
	return ok
}

var (
	ewarViableMu sync.Mutex
	ewarViableAt time.Time
	ewarViableOK bool
)
