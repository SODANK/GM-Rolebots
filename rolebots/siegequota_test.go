package rolebots

import (
	"math"
	"testing"
	"time"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestSiegeQuotaOutpost(t *testing.T) {
	// The conquerable outposts: 40M shield, 200,000 s recharge, no resists, no floor.
	hp := structureHP{Shield: 40_000_000, Armor: 10, Hull: 10, SRes: 1, ARes: 1, HRes: 1, Tau: 200_000}
	q := siegeQuota(hp, siegeWindowSec, siegeQuotaMargin)
	if !near(q.Regen, 500, 0.01) {
		t.Fatalf("regen = %v, want 500", q.Regen)
	}
	if !near(q.BashEHP, 40_000_000, 0.01) || q.KillEHP != 0 {
		t.Fatalf("bash/kill EHP = %v/%v", q.BashEHP, q.KillEHP)
	}
	want := (40_000_000.0/7200 + 500) * 1.15
	if !near(q.OpenDPS(), want, 0.5) {
		t.Fatalf("open DPS = %v, want %v", q.OpenDPS(), want)
	}
}

func TestSiegeQuotaTowerSplit(t *testing.T) {
	// A small Minmatar tower: 11.25M shield, 1.5M armor and hull, mean shield resonance 0.8125.
	hp := structureHP{Shield: 11_250_000, Armor: 1_500_000, Hull: 1_500_000, SRes: 0.8125, ARes: 1, HRes: 1,
		Tau: 200_000, Floor: 0.25}
	q := siegeQuota(hp, 7200, 1)
	if !near(q.BashEHP, 11_250_000*0.75/0.8125, 1) {
		t.Fatalf("bash EHP = %v", q.BashEHP)
	}
	if !near(q.KillEHP, 11_250_000*0.25/0.8125+3_000_000, 1) {
		t.Fatalf("kill EHP = %v", q.KillEHP)
	}
	if !near(q.Regen, 2.5*11_250_000/200_000/0.8125, 0.01) {
		t.Fatalf("regen = %v", q.Regen)
	}
	if q.KillDPS >= q.BashDPS {
		t.Fatalf("kill %v should be under bash %v for this tower", q.KillDPS, q.BashDPS)
	}
}

func TestSiegeQuotaEdges(t *testing.T) {
	q := siegeQuota(structureHP{Shield: 1000, Tau: 0}, 100, 1)
	if q.Regen != 0 || !near(q.BashDPS, 10, 1e-9) {
		t.Fatalf("tau 0: regen %v bash %v", q.Regen, q.BashDPS)
	}
	q = siegeQuota(structureHP{Shield: 1000, Tau: 1000, SRes: 0}, 100, 1)
	if !near(q.BashEHP, 1000, 1e-9) {
		t.Fatalf("res 0 must read as 1: %v", q.BashEHP)
	}
	if meanRes([4]float64{0, 0, 0, 0}) != 1 || !near(meanRes([4]float64{1, 1, 0.5, 0.75}), 0.8125, 1e-9) {
		t.Fatal("meanRes")
	}
}

func TestSiegeFractionsRoundTrip(t *testing.T) {
	hp := siegeHP{Shield: 10_000_000, Armor: 2_000_000, Hull: 2_000_000, SRes: 0.8, ARes: 0.5, HRes: 1, Floor: 0.25}
	for _, stage := range []string{"bash", "kill"} {
		for _, dmg := range []float64{0, 1_000_000, 3_000_000, 5_000_000, 8_000_000} {
			sh, ar, hu := siegeFractions(stage, dmg, hp)
			back := siegeDamageOf(stage, sh, ar, hu, hp)
			_, toKill := hp.ehp()
			toFloor, _ := hp.ehp()
			limit := toFloor
			if stage == "kill" {
				limit = toKill
			}
			if dmg <= limit && !near(back, dmg, 1) {
				t.Fatalf("%s %v: fractions %v/%v/%v back to %v", stage, dmg, sh, ar, hu, back)
			}
		}
	}
	// A row with no resonances read falls back on the old flat 0.5.
	old := siegeHP{Shield: 1000, Armor: 100, Hull: 100, Floor: 0.25}
	if toFloor, _ := old.ehp(); !near(toFloor, 1500, 1e-9) {
		t.Fatalf("legacy toFloor = %v", toFloor)
	}
}

func TestSiegeBashStepRegen(t *testing.T) {
	d, next := siegeBashStep("bash", 0, 100, 150, 60, 1e9, 1e9, 0)
	if d != 0 || next != siegeBashHolds {
		t.Fatalf("out-shot by regen: %v %v", d, next)
	}
	d, _ = siegeBashStep("bash", 1000, 200, 150, 10, 1e9, 1e9, 0)
	if !near(d, 1500, 1e-9) {
		t.Fatalf("net bash = %v", d)
	}
	d, next = siegeBashStep("bash", 990, 200, 100, 1, 1000, 1e9, 0)
	if next != siegeBashFloor || !near(d, 1090, 1e-9) {
		t.Fatalf("floor: %v %v", d, next)
	}
	// The kill stage regenerates only while there is shield left in it.
	d, _ = siegeBashStep("kill", 100, 200, 150, 10, 0, 1e9, 500)
	if !near(d, 600, 1e-9) {
		t.Fatalf("kill shield part = %v", d)
	}
	d, _ = siegeBashStep("kill", 600, 200, 150, 10, 0, 1e9, 500)
	if !near(d, 2600, 1e-9) {
		t.Fatalf("kill past shield = %v", d)
	}
}

func TestSiegeStageNext(t *testing.T) {
	base := siegeStageIn{OpenedAt: 1000, StageAt: 1000, Now: 1100, Regen: 500, Window: 7200}

	in := base
	in.Stage, in.FleetLive, in.FleetDPS = "muster", true, 600
	if out := siegeStageNext(in); out.Stage != "bash" || out.Abandon != "" {
		t.Fatalf("muster with DPS over regen: %+v", out)
	}
	in.FleetDPS = 400
	if out := siegeStageNext(in); out.Stage != "muster" {
		t.Fatalf("muster under regen must hold: %+v", out)
	}
	in.FleetDPS, in.FleetLive = 600, false
	if out := siegeStageNext(in); out.Stage != "muster" {
		t.Fatalf("fleet not out yet: %+v", out)
	}
	in.Now = 1000 + siegeMusterFormSec + 1
	if out := siegeStageNext(in); out.Abandon == "" {
		t.Fatal("muster past the form window must be abandoned")
	}

	in = base
	in.Stage, in.FleetDPS = "bash", 0
	out := siegeStageNext(in)
	if out.Stage != "bash" || out.LowSince != in.Now || out.Abandon != "" {
		t.Fatalf("bash never drops to muster; low clock starts: %+v", out)
	}
	in.LowSince, in.Now = 1100, 1100+siegeLowGraceSec+1
	if out := siegeStageNext(in); out.Abandon == "" {
		t.Fatal("low DPS past the grace must be abandoned")
	}
	in.FleetDPS = 900
	if out := siegeStageNext(in); out.Abandon != "" || out.LowSince != 0 {
		t.Fatalf("fleet back ahead resets the clock: %+v", out)
	}
	in.Now = 1000 + int64(7200*siegeBashOverrun) + 1
	if out := siegeStageNext(in); out.Abandon == "" {
		t.Fatal("a bash past its overrun must be abandoned")
	}
	// An older row with no stage clock counts from its opening.
	in = base
	in.Stage, in.StageAt, in.FleetLive, in.FleetDPS, in.Now = "muster", 0, false, 0, 1000+siegeMusterFormSec+5
	if out := siegeStageNext(in); out.Abandon == "" {
		t.Fatal("legacy muster must time out from openedAt")
	}
}

func TestSumSiegeCapacity(t *testing.T) {
	c := sumSiegeCapacity([]siegePilot{
		{CharID: 1, OwnedDPS: 200, Answer: 1},
		{CharID: 2, OwnedDPS: 100, Answer: 0.5},
		{CharID: 3, OwnedDPS: 0, Answer: 1},
		{CharID: 4, OwnedDPS: 300, Answer: 0},
	})
	if !near(c.Owned, 250, 1e-9) || !near(c.WithKits, 250, 1e-9) || c.Pilots != 2 {
		t.Fatalf("capacity = %+v", c)
	}
	k := sumSiegeCapacity([]siegePilot{
		{CharID: 1, OwnedDPS: 200, Answer: 1},
		{CharID: 2, KitDPS: 150, Answer: 0.5},
	})
	if !near(k.Owned, 200, 1e-9) || !near(k.WithKits, 275, 1e-9) || k.Pilots != 2 {
		t.Fatalf("with kits = %+v", k)
	}
}

func TestMateJoinsTowerSiege(t *testing.T) {
	if !mateJoinsTowerSiege(true, 0.8) || !mateJoinsTowerSiege(false, -0.2) || mateJoinsTowerSiege(false, 0.5) {
		t.Fatal("mateJoinsTowerSiege")
	}
}

func TestSiegeHullClass(t *testing.T) {
	for _, c := range []HullClass{ClassFrigate, ClassDestroyer, ClassCruiser, ClassBattlecruiser, ClassBattleship} {
		if !siegeHullClass(c) {
			t.Fatalf("%v should sit in a siege line", c)
		}
	}
	for _, c := range []HullClass{ClassIndustrial, ClassBarge, ClassFreighter, ClassRookie, ClassShuttle, ClassCapital} {
		if siegeHullClass(c) {
			t.Fatalf("%v should not", c)
		}
	}
}

func TestPickHoldCharge(t *testing.T) {
	acc := func(t int64) bool { return t != 99 }
	if got := pickHoldCharge(map[int64]int64{5: 10, 6: 50}, 5, acc); got != 5 {
		t.Fatalf("prefers the round it fired: %v", got)
	}
	if got := pickHoldCharge(map[int64]int64{5: 0, 6: 50, 7: 80, 99: 500}, 5, acc); got != 7 {
		t.Fatalf("largest accepted stack: %v", got)
	}
	if got := pickHoldCharge(map[int64]int64{99: 500}, 0, acc); got != 0 {
		t.Fatalf("nothing accepted: %v", got)
	}
}

func TestSiegeFXStale(t *testing.T) {
	at := time.Unix(1_000_000, 0)
	if siegeFXStale(0, at, at.Add(30*time.Second)) || !siegeFXStale(0, at, at.Add(siegeFXEmptyRetry)) {
		t.Fatal("an empty copy is read again after siegeFXEmptyRetry")
	}
	if siegeFXStale(3, at, at.Add(5*time.Minute)) || !siegeFXStale(3, at, at.Add(siegeFXRefresh)) {
		t.Fatal("an armed copy is read again after siegeFXRefresh")
	}
}
