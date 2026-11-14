package rolebots

import (
	"context"
	"fmt"
	"sync/atomic"

	evedb "gurimoon/db"
)

// Fleet size tiers: what a fleet is for decides how big it gets. A tier is a
// target, not a lid; nothing refuses a pilot that arrives later.
// See docs/comment-notes-b11.md#fleet-tiers.
type fleetTier struct {
	Name   string
	Lo, Hi int
	// Muster is false for a fleet born live: no waiting room at staging.
	Muster bool
	// Why is the trigger, for the formation log line.
	Why string
}

const (
	tierOrder      = "order"
	tierCallToArms = "calltoarms"
	tierPack       = "pack"
	tierMining     = "mining"
	tierRoam       = "roam"
	tierHunt       = "hunt"

	tierHuntLo, tierHuntHi     = 4, 6
	tierRoamLo, tierRoamHi     = 10, 20
	tierMiningLo, tierMiningHi = 8, 15

	// hunterJoinWindowSec is how long a fleet born live keeps calling up its members
	// off interruptible errands (gangCallUp).
	hunterJoinWindowSec = 15 * 60
)

// tierIn is what decides a fleet's tier. Pure data, so the rule is testable.
type tierIn struct {
	activity   string
	pack       bool
	orderSize  int
	callToArms bool

	atWar        bool // the corporation is in a hostile war
	merc         bool // it is working a mercenary contract
	rivalLowNull bool // a rival it holds hostile works low/null space in reach
	outlaw       bool // the fleet is a raid (ActPiracy)
	bigGame      bool // this combat fleet was drawn to go after the bigger rats
}

// fleetTierFor is the tier rule. Pure.
func fleetTierFor(in tierIn) fleetTier {
	switch {
	case in.callToArms:
		return fleetTier{Name: tierCallToArms, Lo: gangSeatsUncapped, Hi: gangSeatsUncapped,
			Muster: true, Why: "a call to arms"}
	case in.orderSize >= gangMinMembers:
		return fleetTier{Name: tierOrder, Lo: in.orderSize, Hi: in.orderSize, Muster: true,
			Why: "the chair's order"}
	case in.pack:
		return fleetTier{Name: tierPack, Lo: packFleetSize, Hi: packFleetSize, Why: "a hunting pack"}
	case in.activity == ActMining || in.activity == ActIndMining:
		return fleetTier{Name: tierMining, Lo: tierMiningLo, Hi: tierMiningHi, Muster: true,
			Why: "a mining op"}
	}
	roam := func(why string) fleetTier {
		return fleetTier{Name: tierRoam, Lo: tierRoamLo, Hi: tierRoamHi, Muster: true, Why: why}
	}
	switch {
	case in.outlaw:
		return roam("a raid")
	case in.atWar:
		return roam("the war")
	case in.merc:
		return roam("a mercenary contract")
	case in.rivalLowNull:
		return roam("rivals working low and null")
	case in.bigGame:
		return roam("the bigger rats")
	}
	return fleetTier{Name: tierHunt, Lo: tierHuntLo, Hi: tierHuntHi, Why: "bounty hunting"}
}

// tierTarget draws this fleet's seat count inside its tier. roll(n) answers
// [0, n), rand.Intn in production. Pure given roll.
func tierTarget(t fleetTier, roll func(int) int) int {
	if t.Hi <= t.Lo || roll == nil {
		return t.Lo
	}
	return t.Lo + roll(t.Hi-t.Lo+1)
}

// fleetTierForSeat gathers tierIn for one formation and answers the tier.
func (m *Mod) fleetTierForSeat(ctx context.Context, lb *botRun, seat fcSeat, activity string,
	order *fleetOrderSpec, roll func(int) int) fleetTier {
	in := tierIn{activity: activity, pack: seat.pack}
	if order != nil {
		in.callToArms = order.CallToArms
		in.orderSize = order.Size
	}
	switch activity {
	case ActPiracy:
		in.outlaw = true
	case ActCombat, ActSecurity:
		in.atWar = relCorpAtWar(seat.corpID)
		if !in.atWar {
			in.merc = m.mercActiveAsMerc(ctx, seat.corpID) > 0
		}
		if !in.atWar && !in.merc {
			in.rivalLowNull = m.rivalWorksLowNullNear(ctx, seat.corpID, lb.SystemID)
		}
		// The bigger rats are a hunting fleet's choice, not a patrol's: a
		// patrol's ground is its employer's own, whatever lives there.
		if activity == ActCombat && roll != nil && roll(100) < RoamSharePct() {
			in.bigGame = true
		}
	}
	return fleetTierFor(in)
}

// rivalWorksLowNullNear reports whether a corporation this one holds hostile
// (relEnemySystem's set) works a low- or null-sec system near `from`. High-sec
// rivals do not count: shooting one outside a war is a crime there.
func (m *Mod) rivalWorksLowNullNear(ctx context.Context, corpID, from int64) bool {
	if corpID == 0 || from == 0 || !relHasEnemies(corpID) {
		return false
	}
	sys := relEnemySystemsOf(corpID)
	if len(sys) == 0 {
		return false
	}
	u, err := m.universe(ctx)
	if err != nil {
		return false
	}
	near := hopsWithin(u, from, gangRecruitMaxJumps)
	for _, id := range sys {
		if _, ok := near[id]; !ok {
			continue
		}
		if meta, ok := u.meta[id]; ok && meta.Security < 0.45 {
			return true
		}
	}
	return false
}

// ── throughput settings ─────────────────────────────────────────────────────
//
// Sized fleets leave pilots over, who only fly together if more fleets form:
// more formations per pass, a wider scan of idle commanders, and more commander
// seats per corporation (one per FCSeatDivisor() members).

const (
	settingFormPerPass   = "rolebots.gang.formPerPass"
	settingLeaderScan    = "rolebots.gang.leaderScan"
	settingFCSeatDivisor = "rolebots.gang.fcSeatDivisor"
	settingRoamShare     = "rolebots.gang.roamSharePct"
	// settingFCDivisorApplied is the divisor the last promotion pass dealt seats
	// under. A mismatch at boot runs the pass now, since seats are otherwise re-dealt
	// weekly.
	settingFCDivisorApplied = "rolebots.gang.fcSeatDivisorApplied"

	defaultFormPerPass   = 6
	defaultLeaderScan    = 80
	defaultFCSeatDivisor = 5
	defaultRoamSharePct  = 25
	// beltReliefExtra is how many more formations a pass may make while the distress
	// board has live rows, reserved for the security seats that answer it (formBudget).
	beltReliefExtra = 2
)

var (
	formPerPassVal   atomic.Int64
	leaderScanVal    atomic.Int64
	fcSeatDivisorVal atomic.Int64
	roamSharePctVal  atomic.Int64
)

func init() {
	formPerPassVal.Store(defaultFormPerPass)
	leaderScanVal.Store(defaultLeaderScan)
	fcSeatDivisorVal.Store(defaultFCSeatDivisor)
	roamSharePctVal.Store(defaultRoamSharePct)
}

// GangFormPerPass bounds how many fleets one formation pass may raise.
func GangFormPerPass() int { return int(formPerPassVal.Load()) }

// GangLeaderScan is how many idle commanders one pass considers.
func GangLeaderScan() int { return int(leaderScanVal.Load()) }

// FCSeatDivisor is one pooled fleet-commander seat per this many members.
func FCSeatDivisor() int { return int(fcSeatDivisorVal.Load()) }

// RoamSharePct is the chance, in percent, that a combat fleet with no war,
// contract or rival to roam for is raised as a roam for the bigger rats.
func RoamSharePct() int { return int(roamSharePctVal.Load()) }

// loadGangTierSettings reads the throughput knobs. Called from both boot
// loaders that need them (loadGangSettings, loadPromoteClock); idempotent.
func (m *Mod) loadGangTierSettings(ctx context.Context) {
	for _, s := range []struct {
		key      string
		val      *atomic.Int64
		min, max int64
	}{
		{settingFormPerPass, &formPerPassVal, 1, 50},
		{settingLeaderScan, &leaderScanVal, 1, 500},
		{settingFCSeatDivisor, &fcSeatDivisorVal, 2, 50},
		{settingRoamShare, &roamSharePctVal, 0, 100},
	} {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, s.key)
		if err != nil || !ok {
			continue
		}
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= s.min && n <= s.max {
			s.val.Store(n)
		}
	}
}

// fcDivisorChanged reports whether seats were last dealt under a different
// divisor, and records the current one. The caller re-runs the promotion pass
// on true.
func (m *Mod) fcDivisorChanged(ctx context.Context) bool {
	cur := fmt.Sprintf("%d", FCSeatDivisor())
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingFCDivisorApplied)
	if err != nil || (ok && v == cur) {
		return false
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingFCDivisorApplied, cur); err != nil {
		rlogf("rolebots: record fc seat divisor: %v", err)
		return false
	}
	return true
}
