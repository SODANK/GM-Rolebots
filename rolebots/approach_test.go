package rolebots

import (
	"testing"
	"time"
)

// A burn-in gives up per budget, restarts its clock after a hold or while paused, and
// keeps its retry count until the target changes.
func TestApproachStep(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := approachClock{since: now.Add(-300 * time.Second), polled: now.Add(-time.Second), startDist: 15_000, retries: 2}
	cases := []struct {
		name        string
		c           approachClock
		started     bool
		surface     float64
		paused      bool
		wantVerdict approachVerdict
		wantRetries int
		wantRestart bool // since == now afterwards
	}{
		{"inside activation range", old, true, 2_000, false, approachArrived, 0, false},
		{"fresh target", approachClock{}, false, 15_000, false, approachFlying, 0, true},
		{"new target forgets retries", old, false, 15_000, false, approachFlying, 0, true},
		{"budget run out", old, true, 9_000, false, approachGaveUp, 3, true},
		{"held leg restarts the clock", approachClock{since: now.Add(-300 * time.Second), polled: now.Add(-20 * time.Second),
			startDist: 15_000, retries: 2}, true, 9_000, false, approachFlying, 2, true},
		{"paused restarts and keeps retries", old, true, 9_000, true, approachFlying, 2, true},
		{"inside budget", approachClock{since: now.Add(-60 * time.Second), polled: now.Add(-time.Second),
			startDist: 15_000}, true, 9_000, false, approachFlying, 0, false},
	}
	for _, c := range cases {
		got, v := approachStep(c.c, c.started, c.surface, 100, c.paused, now)
		if v != c.wantVerdict || got.retries != c.wantRetries {
			t.Errorf("%s: verdict %d retries %d, want %d and %d", c.name, v, got.retries, c.wantVerdict, c.wantRetries)
		}
		if restarted := got.since.Equal(now); c.wantVerdict != approachArrived && restarted != c.wantRestart {
			t.Errorf("%s: clock restarted = %v, want %v", c.name, restarted, c.wantRestart)
		}
	}

	// The look after a give-up is a fresh budget, not another give-up.
	c, _ := approachStep(old, true, 9_000, 100, false, now)
	if _, v := approachStep(c, true, 8_900, 100, false, now.Add(time.Second)); v != approachFlying {
		t.Fatalf("look after a give-up = %d, want flying", v)
	}
}

// The budget scales with the distance actually left to fly, not the 15 km landing buffer.
func TestApproachBudgetFor(t *testing.T) {
	cases := []struct {
		dist, speed float64
		want        time.Duration
	}{
		{15_000, 100, 240 * time.Second},
		{66_000, 100, 1056 * time.Second},
		{15_000, 400, 93750 * time.Millisecond},
		{2_000, 400, navApproachMin},
		{15_000, 0, navCloseMax},
	}
	for _, c := range cases {
		if got := approachBudgetFor(c.dist, c.speed); got != c.want {
			t.Errorf("approachBudgetFor(%.0f, %.0f) = %v, want %v", c.dist, c.speed, got, c.want)
		}
	}
}

// Combat steers unless travel is burning in and the fight is past the travel hold.
func TestCombatHoldsHelm(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	recent, long := now.Add(-10*time.Second), now.Add(-(travelFightHoldMaxSec+10)*time.Second)
	cases := []struct {
		approaching, fighting bool
		since                 time.Time
		want                  bool
	}{
		{false, true, long, true},
		{true, true, recent, true},
		{true, true, long, false},
		{true, false, time.Time{}, false},
	}
	for _, c := range cases {
		if got := combatHoldsHelm(c.approaching, c.fighting, c.since, now); got != c.want {
			t.Errorf("combatHoldsHelm(%v, %v, %v) = %v, want %v", c.approaching, c.fighting, now.Sub(c.since), got, c.want)
		}
	}
}

// Docking clearance is filed only out of warp, for this station, and from the landing buffer in.
func TestDockClearanceLanded(t *testing.T) {
	cases := []struct {
		name    string
		warping bool
		dest    int64
		surface float64
		want    bool
	}{
		{"landed at the buffer", false, 60, 15_000, true},
		{"at the door", false, 60, 500, true},
		{"still warping", true, 60, 15_000, false},
		{"bound elsewhere", false, 61, 15_000, false},
		{"dragged 20 km out", false, 60, 20_000, false},
	}
	for _, c := range cases {
		if got := dockClearanceLanded(c.warping, c.dest, 60, c.surface); got != c.want {
			t.Errorf("%s: dockClearanceLanded = %v, want %v", c.name, got, c.want)
		}
	}
}
