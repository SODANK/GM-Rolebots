package rolebots

import (
	"context"
	"reflect"
	"testing"
)

func TestGapFromBookScopesAndRatios(t *testing.T) {
	book := []gapBookRow{
		// type 1: bid in region 10 beats ask in region 20 only when both are in scope.
		{TypeID: 1, RegionID: 10, Bid: 120, HasBid: true},
		{TypeID: 1, RegionID: 20, Ask: 100, HasAsk: true},
		// type 2: best bid below best ask everywhere: never a gap.
		{TypeID: 2, RegionID: 10, Bid: 50, HasBid: true, Ask: 60, HasAsk: true},
		// type 3: a zero ask passes at any margin (ratio 1e18).
		{TypeID: 3, RegionID: 10, Bid: 5, HasBid: true, Ask: 0, HasAsk: true},
		// type 4: bids only, no asks: no gap.
		{TypeID: 4, RegionID: 10, Bid: 9, HasBid: true},
		// type 5: the scope's best ask is the lower of two regions.
		{TypeID: 5, RegionID: 10, Bid: 110, HasBid: true, Ask: 200, HasAsk: true},
		{TypeID: 5, RegionID: 20, Ask: 100, HasAsk: true},
	}

	all := gapFromBook(book, nil)
	want := []gapRatio{{TypeID: 1, Ratio: 1.2}, {TypeID: 3, Ratio: 1e18}, {TypeID: 5, Ratio: 1.1}}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("whole book: got %+v, want %+v", all, want)
	}

	only10 := gapFromBook(book, []int64{10})
	if want := []gapRatio{{TypeID: 3, Ratio: 1e18}}; !reflect.DeepEqual(only10, want) {
		t.Fatalf("region 10: got %+v, want %+v", only10, want)
	}

	if got := gapAbove(all, 0.15); !reflect.DeepEqual(got, []int64{1, 3}) {
		t.Fatalf("gapAbove(0.15) = %v", got)
	}
}

func TestTrafficBufferFoldsAndSwaps(t *testing.T) {
	takeTraffic() // start clean
	ctx := context.Background()
	m := &Mod{}
	m.recordJump(ctx, 30000001, 30000002)
	m.recordJump(ctx, 30000002, 30000001) // same undirected lane, arrival in 1
	m.recordJump(ctx, 30000001, 30000001) // no-op
	m.recordMarketFlow(ctx, 30000002, true, 1000)
	m.recordMarketFlow(ctx, 30000002, false, 500)
	m.recordHaul(ctx, 30000002, 30000001, 2500)
	m.recordStationDock(ctx, 60000001)
	m.recordStationDock(ctx, 60000001)
	bufMapJump(30000002)
	bufMapJump(30000002)

	p := takeTraffic()
	if s := p.systems[30000002]; s == nil || s.jumps != 1 || s.buys != 1 || s.sells != 1 || s.isk != 1500 {
		t.Fatalf("system 2 delta = %+v", s)
	}
	if s := p.systems[30000001]; s == nil || s.jumps != 1 {
		t.Fatalf("system 1 delta = %+v", s)
	}
	if l := p.lanes[[2]int64{30000001, 30000002}]; l == nil || l.jumps != 2 || l.hauls != 1 || l.isk != 2500 {
		t.Fatalf("lane delta = %+v", l)
	}
	if len(p.lanes) != 1 {
		t.Fatalf("lanes = %d, want 1 (undirected)", len(p.lanes))
	}
	if d := p.docks[60000001]; d == nil || d.docks != 2 {
		t.Fatalf("dock delta = %+v", d)
	}
	total := 0
	for k, n := range p.mapJumps {
		if k.system != 30000002 {
			t.Fatalf("unexpected map cell %+v", k)
		}
		total += n
	}
	if total != 2 {
		t.Fatalf("map jumps = %d, want 2", total)
	}
	if !takeTraffic().empty() {
		t.Fatal("buffer not emptied by take")
	}
}

func TestSeedSavedStateSkipsUnchangedFirstSave(t *testing.T) {
	const id = 987654321
	forgetSavedState(id)
	defer forgetSavedState(id)

	st := &SimState{CharID: id, Phase: "idle"}
	blob := []byte(`{}`)
	sum := stateFingerprint(st, blob, 0)

	st.diskSum = sum
	seedLoadedState(st)
	if !stateUnchanged(id, sum) {
		t.Fatal("a save identical to the row just read should be skipped")
	}
	if stateUnchanged(id, sum+1) {
		t.Fatal("a changed save must not be skipped")
	}

	// A read never overwrites what a save recorded.
	noteStateSaved(id, sum+7)
	seedSavedState(id, sum)
	if !stateUnchanged(id, sum+7) || stateUnchanged(id, sum) {
		t.Fatal("seed replaced a save's fingerprint")
	}
}
