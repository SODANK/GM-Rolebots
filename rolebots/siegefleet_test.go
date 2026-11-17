package rolebots

import "testing"

func TestPickForwardStaging(t *testing.T) {
	cands := []stagingCand{
		{StationID: 30, Jumps: 2},
		{StationID: 20, Jumps: 1},
		{StationID: 10, Jumps: 1, Office: true},
		{StationID: 40, Jumps: 1, Own: true},
		{StationID: 5, Jumps: 9, Own: true},
	}
	if id, ok := pickForwardStaging(cands, 5); !ok || id != 40 {
		t.Fatalf("nearest, own first: %v %v", id, ok)
	}
	if id, _ := pickForwardStaging(cands[:3], 5); id != 10 {
		t.Fatalf("office over NPC on a tie: %v", id)
	}
	if id, _ := pickForwardStaging([]stagingCand{{StationID: 9, Jumps: 1}, {StationID: 7, Jumps: 1}}, 5); id != 7 {
		t.Fatalf("lower ID on a full tie: %v", id)
	}
	if _, ok := pickForwardStaging([]stagingCand{{StationID: 5, Jumps: 9}}, 5); ok {
		t.Fatal("beyond reach")
	}
}

func TestSiegeMusterDeadline(t *testing.T) {
	if siegeMusterDeadline(60) != siegeMusterMinSec || siegeMusterDeadline(5*3600) != siegeMusterMaxSec ||
		siegeMusterDeadline(40*60) != 40*60 {
		t.Fatal("siegeMusterDeadline clamp")
	}
}

func TestSiegeGangLease(t *testing.T) {
	for _, st := range []string{"muster", "bash", "kill", "fight"} {
		if ext, dis := siegeGangLease(st, true); !ext || dis {
			t.Fatalf("%s open: extend", st)
		}
	}
	if ext, dis := siegeGangLease("timer", true); ext || !dis {
		t.Fatal("timer: disband, the exit call re-forms")
	}
	if ext, dis := siegeGangLease("bash", false); ext || !dis {
		t.Fatal("closed: disband")
	}
}

func TestSiegeMusterDoctrineRatio(t *testing.T) {
	d := siegeMusterDoctrine(1, TankArmor, 0)
	if d.Kind != kindSiege || d.HullMin != ClassFrigate || d.HullMax != ClassBattleship {
		t.Fatalf("doctrine = %+v", d)
	}
	for _, s := range d.Seats {
		if s.Role == RoleTackler || s.Role == RoleEwar {
			t.Fatal("a siege line seats no tackle or EWAR")
		}
	}
	slots := d.Slots(30, RoleHunter, JobCombat, nil)
	logi := 0
	for _, s := range slots {
		if s.role == RoleLogi {
			logi++
		}
	}
	if guns := len(slots) - logi; logi == 0 || guns < 4*logi {
		t.Fatalf("ratio: %d logi of %d", logi, len(slots))
	}
}

func TestExcessSpecialists(t *testing.T) {
	roles := []string{RoleLogi, RoleLogi, RoleHunter, RoleLogi, RoleLogi, RoleHunter, RoleLogi, RoleLogi,
		RoleLogi, RoleHunter, RoleLogi, RoleTackler}
	// 3 hunters + the leader = 4 guns: one logi allowed of 8.
	got := excessSpecialists(roles, RoleLogi, siegeLogiPer)
	if len(got) != 7 {
		t.Fatalf("surplus = %v", got)
	}
	for _, i := range got {
		if roles[i] != RoleLogi {
			t.Fatalf("index %d is not logi", i)
		}
	}
	if got[0] != 10 {
		t.Fatalf("last-picked first: %v", got)
	}
	if excessSpecialists([]string{RoleHunter, RoleLogi}, RoleLogi, siegeLogiPer) != nil {
		t.Fatal("one logi always allowed")
	}
}

func TestSeatFillOrderGunsFirst(t *testing.T) {
	slots := []gangSlot{{RoleLogi, JobLogi}, {RoleHunter, JobCombat}, {RoleLogi, JobLogi}, {RoleHunter, JobCombat}}
	got := seatFillOrderFor(slots, true)
	if slots[got[0]].job != JobCombat || slots[got[1]].job != JobCombat {
		t.Fatalf("guns first: %v", got)
	}
	if got := seatFillOrderFor(slots, false); slots[got[0]].job != JobLogi {
		t.Fatalf("ordinary fill keeps logi first: %v", got)
	}
}

func TestSiegeMusterVerdict(t *testing.T) {
	if r, s := siegeMusterVerdict(7000, 0, 6900, false, 0.8); !r || s {
		t.Fatal("go early on present DPS")
	}
	if r, s := siegeMusterVerdict(3000, 5000, 6900, false, 0.8); r || s {
		t.Fatal("before the deadline it waits")
	}
	if r, s := siegeMusterVerdict(3000, 5000, 6900, true, 0.8); !r || s {
		t.Fatal("deadline: present + 0.8 en route meets it")
	}
	if r, s := siegeMusterVerdict(3000, 3000, 6900, true, 0.8); r || !s {
		t.Fatal("deadline short: stand down")
	}
	if r, _ := siegeMusterVerdict(0, 0, 0, false, 0.8); !r {
		t.Fatal("no quota never holds a fleet")
	}
}

func TestSiegeShortBackoff(t *testing.T) {
	if siegeShortBackoff(0) != 0 || siegeShortBackoff(1) != 6*3600 || siegeShortBackoff(2) != 12*3600 ||
		siegeShortBackoff(3) != 24*3600 || siegeShortBackoff(9) != siegeShortBackoffMax {
		t.Fatal("siegeShortBackoff")
	}
	if siegeAbandonResult("muster") != "short" || siegeAbandonResult("bash") != "abandoned" {
		t.Fatal("siegeAbandonResult")
	}
}

func TestSiegeRoundsPerGun(t *testing.T) {
	if got := siegeRoundsPerGun(3, 7200, 1.1); got != 2640 {
		t.Fatalf("rounds = %v", got)
	}
	if siegeRoundsPerGun(0, 7200, 1) != 0 {
		t.Fatal("no cycle, no rounds")
	}
}

func TestSiegeAmmoShortfall(t *testing.T) {
	need := map[int64]int64{1: 3000, 2: 1000, 3: 50}
	aboard := map[int64]int64{1: 1000, 2: 1000}
	vol := map[int64]float64{1: 0.01, 2: 0.01, 3: 0.0}
	got := siegeAmmoShortfall(need, aboard, 15, vol)
	if got[1] != 1500 || got[2] != 0 || got[3] != 0 {
		t.Fatalf("capped by hold, unknown volume skipped: %v", got)
	}
	got = siegeAmmoShortfall(need, aboard, 100, vol)
	if got[1] != 2000 {
		t.Fatalf("room for all of it: %v", got)
	}
}

func TestPickSiegeBattery(t *testing.T) {
	cands := []batteryCand{
		{ID: 5, Group: 449, DistM: 20_000}, // hybrid sentry
		{ID: 9, Group: 441, DistM: 30_000}, // web
		{ID: 7, Group: 443, DistM: 25_000}, // scram
		{ID: 3, Group: 439, DistM: 90_000}, // ECM, out of reach
		{ID: 1, Group: 397, DistM: 5_000},  // an assembly array: not a battery
	}
	if id, ok := pickSiegeBattery(cands, siegeBatteryReachM); !ok || id != 7 {
		t.Fatalf("scram before web and guns, ECM out of reach: %v", id)
	}
	if id, _ := pickSiegeBattery(append(cands, batteryCand{ID: 2, Group: 439, DistM: 10_000}), siegeBatteryReachM); id != 2 {
		t.Fatalf("ECM in reach goes first: %v", id)
	}
	if _, ok := pickSiegeBattery([]batteryCand{{ID: 1, Group: 397}}, siegeBatteryReachM); ok {
		t.Fatal("no battery, no pick")
	}
}
