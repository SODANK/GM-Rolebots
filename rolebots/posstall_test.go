package rolebots

import "testing"

// A deploy or rig leg is held to corpWorkMaxJumps while the officer reserve runs, then opens to
// any distance; every other leg keeps the usual radius.
func TestHaulClaimMaxJumps(t *testing.T) {
	cases := []struct {
		kind string
		age  int64
		want int
	}{
		{haulKindDeploy, 0, corpWorkMaxJumps},
		{haulKindDeploy, anchorReserveSec - 1, corpWorkMaxJumps},
		{haulKindDeploy, anchorReserveSec, unlimitedJumps},
		{haulKindRig, anchorReserveSec + 3600, unlimitedJumps},
		{haulKindFuel, anchorOfficerSec * 10, corpWorkMaxJumps},
	}
	for _, c := range cases {
		if got := haulClaimMaxJumps(c.kind, c.age); got != c.want {
			t.Errorf("haulClaimMaxJumps(%q, %d) = %d, want %d", c.kind, c.age, got, c.want)
		}
	}
}

// Moons in systems above the harvester's ceiling are dropped, whatever their display rounding:
// 0.42 shows as 0.4 but sits above the raw 0.4 limit.
func TestIntelHarvestable(t *testing.T) {
	meta := map[int64]sysMeta{1: {Security: 0.4}, 2: {Security: 0.42}, 3: {Security: 0.449}, 4: {Security: -0.3}}
	intel := []moonIntel{{MoonID: 10, SystemID: 1}, {MoonID: 20, SystemID: 2}, {MoonID: 30, SystemID: 3},
		{MoonID: 40, SystemID: 4}, {MoonID: 50, SystemID: 99}}
	got := intelHarvestable(intel, meta, 0.4)
	if len(got) != 2 || got[0].MoonID != 10 || got[1].MoonID != 40 {
		t.Fatalf("intelHarvestable kept %+v, want moons 10 and 40", got)
	}
}

// Survey candidates stop at the harvester's ceiling, not at highsec.
func TestSurveyCandidatesHarvesterCeiling(t *testing.T) {
	reach := map[int64]int{1: 1, 2: 1, 3: 1}
	meta := map[int64]sysMeta{1: {Security: 0.38}, 2: {Security: 0.43}, 3: {Security: 0.6}}
	moons := map[int64]int{1: 5, 2: 5, 3: 5}
	got := surveyCandidates(reach, meta, nil, moons, nil, nil, 0.4)
	if len(got) != 1 || got[0].SystemID != 1 {
		t.Fatalf("surveyCandidates = %+v, want only system 1", got)
	}
}

// The structures each purpose stands or falls by.
func TestPurposeWorkTypes(t *testing.T) {
	has := func(ts []int64, want int64) bool {
		for _, t := range ts {
			if t == want {
				return true
			}
		}
		return false
	}
	if ts := purposeWorkTypes(purposeMining, false); !has(ts, typeHarvester) {
		t.Errorf("mining: %v", ts)
	}
	if ts := purposeWorkTypes(purposeReactions, false); !has(ts, typeHarvester) || !has(ts, typeMediumReactor) {
		t.Errorf("miner+reactor: %v", ts)
	}
	if ts := purposeWorkTypes(purposeReactions, true); has(ts, typeHarvester) || !has(ts, typeReactor) {
		t.Errorf("hub: %v", ts)
	}
	if ts := purposeWorkTypes(purposeRefinery, false); !has(ts, typeMedRefinery) || !has(ts, typeBigRefinery) {
		t.Errorf("refinery: %v", ts)
	}
	for _, p := range []string{purposeLab, purposeFactory, purposeDepot, purposeFortress} {
		if ts := purposeWorkTypes(p, false); len(ts) != 0 {
			t.Errorf("%s: %v, want none", p, ts)
		}
	}
}
