package rolebots

import (
	"testing"

	"gurimoon/destiny"
)

// Each break-off rule is named, and checked in the order the pilot weighs them.
func TestBreakOffArm(t *testing.T) {
	base := breakInputs{fightsPosture: true, armed: true, hull: 1, tank: 1, fleeHullArmed: 0.4, fleeTankUnarm: 0.8}
	with := func(f func(*breakInputs)) breakInputs { in := base; f(&in); return in }
	cases := []struct {
		name string
		in   breakInputs
		want breakArm
	}{
		{"healthy armed fighter stays", base, ""},
		{"fleet called", with(func(in *breakInputs) { in.fleetCalled = true }), breakFleetCalled},
		{"under a leader, hull holding", with(func(in *breakInputs) { in.underLeader = true; in.hull = 0.3 }), ""},
		{"under a leader, hull going", with(func(in *breakInputs) { in.underLeader = true; in.hull = 0.1 }), breakFleetHullLine},
		{"leader outranks hopeless", with(func(in *breakInputs) { in.underLeader = true; in.hopeless = true }), ""},
		{"hopeless margin", with(func(in *breakInputs) { in.hopeless = true }), breakHopeless},
		{"worker below tank line", with(func(in *breakInputs) { in.fightsPosture = false; in.tank = 0.5 }), breakWorkerTank},
		{"worker above tank line", with(func(in *breakInputs) { in.fightsPosture = false; in.tank = 0.9 }), ""},
		{"unarmed fighter below tank line", with(func(in *breakInputs) { in.armed = false; in.tank = 0.5 }), breakUnarmedTank},
		{"armed fighter on structure", with(func(in *breakInputs) { in.hull = 0.0001 }), breakArmedHull},
	}
	for _, c := range cases {
		if got := breakOffArm(c.in); got != c.want {
			t.Errorf("%s: breakOffArm = %q, want %q", c.name, got, c.want)
		}
	}
}

// Only a fight nobody has brought to the pilot yet can be declined.
func TestChosenFight(t *testing.T) {
	cases := []struct {
		name string
		hs   []gridHostile
		want bool
	}{
		{"nothing", nil, false},
		{"scenery only", []gridHostile{{ID: 1}}, false},
		{"war target in transit", []gridHostile{{ID: 1, Aggro: true, Elective: true}}, true},
		{"friend's attacker", []gridHostile{{ID: 1, Aggro: true, Assist: true, Elective: true}}, true},
		{"shot at", []gridHostile{{ID: 1, Aggro: true, Provoked: true}}, false},
		{"mixed", []gridHostile{{ID: 1, Aggro: true, Elective: true}, {ID: 2, Aggro: true, Provoked: true}}, false},
	}
	for _, c := range cases {
		if got := chosenFight(c.hs); got != c.want {
			t.Errorf("%s: chosenFight = %v, want %v", c.name, got, c.want)
		}
	}
}

// A destroyed ship's damage is never written back; a live one's is, floored at zero.
func TestDamageToPersist(t *testing.T) {
	hp := destiny.HPState{MaxArmor: 1000, CurArmor: 400, MaxStructure: 500, CurStructure: 450}
	if s, a, ok := damageToPersist(hp, false); !ok || s != 50 || a != 600 {
		t.Fatalf("live ship = %v, %v, %v; want 50, 600, true", s, a, ok)
	}
	if _, _, ok := damageToPersist(hp, true); ok {
		t.Fatalf("destroyed ship persisted damage")
	}
	if _, _, ok := damageToPersist(destiny.HPState{}, false); ok {
		t.Fatalf("unfilled HP block persisted damage")
	}
	over := destiny.HPState{MaxArmor: 100, CurArmor: 150, MaxStructure: 100, CurStructure: 120}
	if s, a, _ := damageToPersist(over, false); s != 0 || a != 0 {
		t.Fatalf("over-full HP = %v, %v; want 0, 0", s, a)
	}
}

// A holed hull may spend down to the small reserve; anything else keeps the normal floor.
func TestRepairBudget(t *testing.T) {
	cases := []struct {
		name     string
		bal      float64
		critical bool
		want     float64
	}{
		{"rich, routine", 3_000_000, false, 1_000_000},
		{"rich, critical keeps the larger budget", 3_000_000, true, 2_950_000},
		{"poor, routine", 400_000, false, -300_000},
		{"poor, critical", 400_000, true, 350_000},
		{"broke, critical", 30_000, true, -20_000},
	}
	for _, c := range cases {
		if got := repairBudget(c.bal, c.critical); got != c.want {
			t.Errorf("%s: repairBudget = %.0f, want %.0f", c.name, got, c.want)
		}
	}
	if repairCritical(0.5) || !repairCritical(10) {
		t.Errorf("repairCritical: scratches must not count, structure damage must")
	}
}
