package rolebots

import "testing"

// Tests for the CPU passes: the oven gate (ovengate.go), the roster wake list
// (rosterwake.go) and leg catch-up (legcatchup.go).

func TestOvenGateShutsUntilEndOrRecheck(t *testing.T) {
	const char = 9_100_001
	defer func() { ovenGateMu.Lock(); delete(ovenGate, ovenGateKey{ovenKindIndustry, char}); ovenGateMu.Unlock() }()
	ids := []int64{11, 12}
	now := int64(1_000_000)
	end := evedbUnixToWin32ForTest(now + 60)
	ovenGateNote(ovenKindIndustry, char, ids, end, now)
	if !ovenGateShut(ovenKindIndustry, char, ids, now+59) {
		t.Fatal("gate should be shut before the job's end")
	}
	if ovenGateShut(ovenKindIndustry, char, ids, now+60) {
		t.Fatal("gate should open at the job's end")
	}
	if ovenGateShut(ovenKindIndustry, char, []int64{12, 11, 13}, now+1) {
		t.Fatal("a changed job list should open the gate")
	}
	if !ovenGateShut(ovenKindIndustry, char, []int64{12, 11}, now+1) {
		t.Fatal("the signature should not depend on order")
	}
	// A far end time is capped by the recheck.
	ovenGateNote(ovenKindIndustry, char, ids, evedbUnixToWin32ForTest(now+86_400), now)
	if ovenGateShut(ovenKindIndustry, char, ids, now+ovenRecheckSec) {
		t.Fatal("gate should lapse after ovenRecheckSec")
	}
	if got := ovenGateWake(ovenKindIndustry, char, ids, now+10); got != now+ovenRecheckSec {
		t.Fatalf("ovenGateWake = %d, want %d", got, now+ovenRecheckSec)
	}
}

func TestOvenRungMatchesTheOldQuery(t *testing.T) {
	const ready, due = int64(1 << 62), int64(500)
	cases := []struct {
		status, end int64
		want        bool
	}{
		{1, 900, true},   // flagged by the server ticker
		{101, 900, true}, // taken off from outside
		{102, 900, true}, // cancelled
		{0, 400, true},   // past its end, not flagged yet
		{0, 600, false},  // still running
		{0, 0, false},    // no end time
	}
	for _, c := range cases {
		if got := ovenRung(c.status, c.end, ready, due); got != c.want {
			t.Errorf("ovenRung(%d, %d) = %v, want %v", c.status, c.end, got, c.want)
		}
	}
	// With a cut, a flagged job must have ended by the cut.
	if ovenRung(1, 600, 500, 500) {
		t.Error("a flagged job after the cut should not ring")
	}
}

func TestWin32ToUnixCeil(t *testing.T) {
	ft := evedbUnixToWin32ForTest(1_000)
	if got := win32ToUnixCeil(ft); got != 1_000 {
		t.Fatalf("exact second: got %d", got)
	}
	if got := win32ToUnixCeil(ft + 1); got != 1_001 {
		t.Fatalf("a tick past the second rounds up: got %d", got)
	}
}

func TestRosterDue(t *testing.T) {
	const char = 9_100_002
	defer rosterWakeClear(char)
	now := int64(2_000_000)
	b := &botRun{CharID: char}
	rosterWakeClear(char)
	if !rosterDue(b, now) {
		t.Fatal("a pilot with no record is due")
	}
	noteRosterWake(char, now+30, now)
	if rosterDue(b, now+10) {
		t.Fatal("a pilot before its wake is not due")
	}
	if !rosterDue(b, now+30) {
		t.Fatal("a pilot at its wake is due")
	}
	noteRosterWake(char, now+500, now)
	if !rosterDue(b, now+rosterSweepSec) {
		t.Fatal("the sweep backstop should make it due")
	}
	noteRosterWake(char, now+500, now)
	if !rosterDue(&botRun{CharID: char, GangID: 7}, now+1) {
		t.Fatal("a gang member is always due")
	}
	if !rosterDue(&botRun{CharID: char, BossID: 7}, now+1) {
		t.Fatal("a conscript is always due")
	}
	if !rosterDue(&botRun{CharID: char, State: newSimState(char)}, now+1) {
		t.Fatal("an already-loaded pilot is due")
	}
	noteRosterWake(char, now+500, now)
	rosterWakeClear(char)
	if !rosterDue(b, now+1) {
		t.Fatal("a cleared record is due")
	}
}

func TestRosterWakeAtFollowsLegAndHalt(t *testing.T) {
	m := &Mod{}
	now := int64(3_000_000)
	b := &botRun{CharID: 9_100_003, StationID: 60000001, State: newSimState(9_100_003)}
	b.State.Data.LegEndsAt = now + 40
	if got := m.rosterWakeAt(b, now); got != now+40 {
		t.Fatalf("docked mid-leg: got %d, want %d", got, now+40)
	}
	b.State.Data.LegEndsAt = now + 10_000
	if got := m.rosterWakeAt(b, now); got != now+rosterSweepSec {
		t.Fatalf("a long leg is capped by the sweep: got %d", got)
	}
	b.State.Data.LegEndsAt = 0
	if got := m.rosterWakeAt(b, now); got != now {
		t.Fatalf("no timed leg polls every pass: got %d", got)
	}
	b.State.Halted, b.State.LastErrorAt = true, now-10
	if got := m.rosterWakeAt(b, now); got != now-10+haltRetrySec && got != now+rosterSweepSec {
		t.Fatalf("halted: got %d", got)
	}
}

func TestLegCatchUpPullsTheNewLegBack(t *testing.T) {
	// The last leg ended at 1000; the turn came round at 1040 and set a 60 s leg.
	st := newSimState(1)
	st.catchUp = legCatchUp{from: 1_000, lag: 40}
	st.Data.LegEndsAt = 1_040 + 60
	st.applyLegCatchUp()
	if st.Data.LegEndsAt != 1_060 {
		t.Fatalf("LegEndsAt = %d, want 1060 (prev end 1000 + 60)", st.Data.LegEndsAt)
	}
	// Applied once.
	st.Data.LegEndsAt = 1_200
	st.applyLegCatchUp()
	if st.Data.LegEndsAt != 1_200 {
		t.Fatal("catch-up should apply once")
	}
}

func TestLegCatchUpWaitsForANewLeg(t *testing.T) {
	st := newSimState(1)
	st.catchUp = legCatchUp{from: 1_000, lag: 40}
	st.Data.LegEndsAt = 1_000 // an earlier save in the turn: no new leg yet
	st.applyLegCatchUp()
	if st.Data.LegEndsAt != 1_000 || st.catchUp.lag != 40 {
		t.Fatal("a save without a new leg should leave the catch-up armed")
	}
	st.Data.LegEndsAt = 0 // a cleared leg
	st.applyLegCatchUp()
	if st.Data.LegEndsAt != 0 || st.catchUp.lag != 40 {
		t.Fatal("a cleared leg is not a new leg")
	}
}

func TestLegCatchUpKeepsMarkers(t *testing.T) {
	st := newSimState(1)
	st.catchUp = legCatchUp{from: 1_000, lag: 40}
	st.Data.UndockRunUntil = 1_070
	st.Data.LegEndsAt = 1_075 // set to outlast the run
	st.applyLegCatchUp()
	if st.Data.LegEndsAt != 1_070 {
		t.Fatalf("LegEndsAt = %d, want the undock run's end 1070", st.Data.LegEndsAt)
	}
}

func TestArmLegCatchUpBounds(t *testing.T) {
	const char = 9_100_004
	prev := legCatchUpSince.Load()
	defer legCatchUpSince.Store(prev)
	defer func() { popMu.Lock(); delete(popOnlineAt, char); popMu.Unlock() }()

	now := int64(5_000)
	b := &botRun{CharID: char, State: newSimState(char)}
	b.State.Data.LegEndsAt = now - 300

	legCatchUpSince.Store(0)
	armLegCatchUp(b, now)
	if b.State.catchUp.lag != 0 {
		t.Fatal("no sim start recorded: nothing to repay")
	}
	legCatchUpSince.Store(1)
	armLegCatchUp(b, now)
	if b.State.catchUp.lag != legCatchUpMaxSec || b.State.catchUp.from != now-300 {
		t.Fatalf("capped lag: got %+v", b.State.catchUp)
	}
	legCatchUpSince.Store(now - 20)
	armLegCatchUp(b, now)
	if b.State.catchUp.lag != 20 {
		t.Fatalf("bounded by sim start: got %d", b.State.catchUp.lag)
	}
	legCatchUpSince.Store(1)
	popMu.Lock()
	popOnlineAt[char] = now - 5
	popMu.Unlock()
	armLegCatchUp(b, now)
	if b.State.catchUp.lag != 5 {
		t.Fatalf("bounded by login: got %d", b.State.catchUp.lag)
	}
	b.fastLane = true
	armLegCatchUp(b, now)
	if b.State.catchUp.lag != 0 {
		t.Fatal("the grid lane is never caught up")
	}
	b.fastLane = false
	b.State.Data.LegEndsAt = now + 10
	armLegCatchUp(b, now)
	if b.State.catchUp.lag != 0 {
		t.Fatal("a leg not yet over is not late")
	}
}

// evedbUnixToWin32ForTest mirrors evedb.UnixToWin32 so the gate tests do not
// depend on the core package's helper.
func evedbUnixToWin32ForTest(unix int64) int64 { return (unix + 11644473600) * 10_000_000 }

func TestPresenceDirtySet(t *testing.T) {
	takePresenceDirty()
	markPresenceDirty(0) // ignored
	markPresenceDirty(5)
	markPresenceDirty(7)
	markPresenceDirty(5)
	got := takePresenceDirty()
	if len(got) != 2 {
		t.Fatalf("take returned %v, want two distinct pilots", got)
	}
	if again := takePresenceDirty(); again != nil {
		t.Fatalf("set should be empty after take, got %v", again)
	}
}

func TestTraitsCacheInvalidation(t *testing.T) {
	const char int64 = 9_100_005
	defer invalidateTraits(char)
	traitsCache.Store(char, map[string]int{"caution": 10})
	v, _ := traitsCache.Load(char)
	c := copyTraits(v.(map[string]int))
	c["caution"] = 99
	if v2, _ := traitsCache.Load(char); v2.(map[string]int)["caution"] != 10 {
		t.Fatal("a caller's copy must not write through to the cache")
	}
	gen := traitsGen.Load()
	invalidateTraits(char)
	if _, ok := traitsCache.Load(char); ok {
		t.Fatal("invalidateTraits should drop the pilot")
	}
	if traitsGen.Load() == gen {
		t.Fatal("a write must bump the generation so a racing read is not cached")
	}
	traitsCache.Store(char, map[string]int{})
	traitsCache.Store(char+1, map[string]int{})
	invalidateAllTraits()
	_, a := traitsCache.Load(char)
	_, b := traitsCache.Load(char + 1)
	if a || b {
		t.Fatal("invalidateAllTraits should drop everyone")
	}
}
