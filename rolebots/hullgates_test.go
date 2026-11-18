package rolebots

import "testing"

func TestFilterAffordablePrintsKeepsCopyable(t *testing.T) {
	cands := []indCandidate{
		{TypeID: 1, TypeName: "Rifter", BPTypeID: 101},
		{TypeID: 2, TypeName: "Exequror", BPTypeID: 102},
		{TypeID: 3, TypeName: "Dominix", BPTypeID: 103},
		{TypeID: 4, TypeName: "Thorax", BPTypeID: 104},
	}
	base := map[int64]float64{101: 2_800_000, 102: 29_625_000, 103: 625_000_000, 104: 74_000_000}
	owned := map[int64]bool{104: true}
	copyable := map[int64]bool{102: true}
	kept, needs, dropped := filterAffordablePrints(cands, base, owned, copyable, 5_000_000)
	got := map[int64]bool{}
	for _, c := range kept {
		got[c.TypeID] = true
	}
	if !got[1] || !got[2] || !got[4] || got[3] {
		t.Fatalf("kept %v; want Rifter (affordable), Exequror (copy on escrow), Thorax (owned), not Dominix", kept)
	}
	if len(dropped) != 1 || dropped[0].TypeID != 3 || len(needs) != 1 || needs[0] != 625_000_000*industryAffordMultiple {
		t.Fatalf("dropped %v needs %v; want only Dominix at 1.25x its print", dropped, needs)
	}
	// Without the copy, the cruiser goes too: the old rule.
	kept, _, _ = filterAffordablePrints(cands, base, owned, nil, 5_000_000)
	for _, c := range kept {
		if c.TypeID == 2 {
			t.Fatalf("Exequror kept with no copy on escrow")
		}
	}
}

func TestCopyableEscrowTypes(t *testing.T) {
	reach := map[int64]int{30000001: 2}
	listings := []escrowBPO{
		{TypeID: 102, Copy: true, Runs: 5, SystemID: 30000001, Price: 3_000_000, IssuerID: 9},
		{TypeID: 103, Copy: true, Runs: 1, SystemID: 30000001, Price: 40_000_000, IssuerID: 9}, // over budget
		{TypeID: 104, Copy: true, Runs: 5, SystemID: 30000099, Price: 1_000_000, IssuerID: 9}, // out of reach
		{TypeID: 105, Copy: true, Runs: 0, SystemID: 30000001, Price: 1_000_000, IssuerID: 9}, // spent
		{TypeID: 106, Copy: false, Runs: -1, SystemID: 30000001, Price: 1_000_000, IssuerID: 9}, // original
		{TypeID: 107, Copy: true, Runs: 5, SystemID: 30000001, Price: 1_000_000, IssuerID: 7}, // its own
	}
	got := copyableEscrowTypes(listings, reach, 7, 10_000_000)
	if len(got) != 1 || !got[102] {
		t.Fatalf("copyable = %v, want only 102", got)
	}
}

func TestCopyAmortCostAndHullAmortisation(t *testing.T) {
	// An original passes through whatever the projects.
	if v := copyAmortCost(1_000_000, 3, 0, industryBPOAmortProjectsHull); v != 1_000_000 {
		t.Fatalf("original = %v", v)
	}
	// A copy is charged its consumed share in full: 2 of 5 runs of a 5M copy is 2M once divided by the projects.
	for _, amort := range []int{industryBPOAmortProjects, industryBPOAmortProjectsHull} {
		v := copyAmortCost(5_000_000, 2, 5, amort)
		if got := v / float64(amort); !near(got, 2_000_000, 0.01) {
			t.Fatalf("copy share at amort %d = %v, want 2M", amort, got)
		}
	}
	if bpoAmortProjects(catShip, true) != industryBPOAmortProjectsHull ||
		bpoAmortProjects(catShip, false) != industryBPOAmortProjects ||
		bpoAmortProjects(catModule, true) != industryBPOAmortProjects {
		t.Fatalf("bpoAmortProjects picks the wrong life")
	}
	// A bought Raven original: at ten projects its share alone is over the catalogue hull price; at forty it is not.
	const ravenPrint, ravenHull = 1_087_500_000.0, 108_750_000.0
	old := industryUnitPrice(0, 0, ravenPrint, 1, industryBPOAmortProjects, 1, 1.5)
	now := industryUnitPrice(0, 0, ravenPrint, 1, industryBPOAmortProjectsHull, 1, 1.5)
	if old <= ravenHull || now >= ravenHull {
		t.Fatalf("raven print share old %v now %v against hull %v", old, now, ravenHull)
	}
}

func TestHistoryFloodHull(t *testing.T) {
	cases := []struct {
		name            string
		ready           bool
		perDay          float64
		listed, supply  int64
		wanted          int64
		flooded, decide bool
	}{
		{"no record", false, 0, 5, 5, 100, false, false},
		{"one unsold cruiser, none bought", true, 0, 1, 1, 100, false, true},
		{"two unsold", true, 0, 2, 2, 40, false, true},
		{"three unsold hits the floor", true, 0, 3, 3, 40, true, true},
		{"big census raises the floor", true, 0, 5, 5, 400, false, true},
		{"big census, enough listed", true, 0, 20, 20, 400, true, true},
		{"buying region judged on cover", true, 1, 20, 20, 0, true, true},
		{"buying region with room", true, 1, 5, 5, 0, false, true},
	}
	for _, c := range cases {
		fl, dec := historyFloodHull(c.ready, c.perDay, c.listed, c.supply, c.wanted)
		if fl != c.flooded || dec != c.decide {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", c.name, fl, dec, c.flooded, c.decide)
		}
	}
	// The non-hull rule is unchanged: one listed and never bought floods.
	if fl, dec := floodCover(false, true, 0, 1, 1, 100, mktBuildCoverDays); !fl || !dec {
		t.Fatalf("non-hull rule changed: (%v,%v)", fl, dec)
	}
	if fl, _ := floodCover(true, true, 0, 1, 1, 100, mktBuildCoverDays); fl {
		t.Fatalf("hull rule flooded on one unsold unit")
	}
}

func TestCostPlusCoversCruisersByDefault(t *testing.T) {
	if costPlusScopeDefault != costPlusScopeAll {
		t.Fatalf("default scope %q", costPlusScopeDefault)
	}
	if !costPlusApplies(costPlusScopeDefault, "market") || costPlusApplies(costPlusScopeDefault, corpDemandPurpose) {
		t.Fatalf("default scope should cover market rows and never the demand ledger")
	}
	if costPlusApplies(costPlusScopeInternal, "market") {
		t.Fatalf("internal scope must keep market rows on the tape")
	}
	for _, g := range []int64{420, 26, 419, 27} {
		if !costPlusHullGroups[g] {
			t.Fatalf("group %d not cost-plus", g)
		}
	}
	if costPlusHullGroups[25] {
		t.Fatalf("frigates are not cost-plus")
	}
}

func TestPoPaysBackOrWant(t *testing.T) {
	// No tape: the old rule always refuses.
	if poPaysBack(500_000, 1, 0, 0, 37_000_000) {
		t.Fatalf("tape-only payback passed with no tape")
	}
	// No tape, 400 pilots want one: 100 runs x 0.5M = 50M covers a 37M print.
	if !poPaysBackOrWant(500_000, 1, 0, 0, 400, 37_000_000) {
		t.Fatalf("want fallback refused a print it pays back")
	}
	// A small want does not.
	if poPaysBackOrWant(500_000, 1, 0, 0, 40, 37_000_000) {
		t.Fatalf("want fallback passed a print it cannot pay back")
	}
	// With tape, the tape decides and the want is ignored.
	if poPaysBackOrWant(500_000, 1, 1, 0, 100_000, 37_000_000) != poPaysBack(500_000, 1, 1, 0, 37_000_000) {
		t.Fatalf("want overrode the tape")
	}
	// No margin never pays back.
	if poPaysBackOrWant(0, 1, 0, 0, 400, 37_000_000) {
		t.Fatalf("zero margin passed")
	}
}

func TestPickOwnerCopy(t *testing.T) {
	prints := []ownerCopyPrint{
		{ItemID: 1, BPTypeID: 201, CategoryID: catShip, GroupID: 25},  // frigate: not this lane's
		{ItemID: 2, BPTypeID: 202, CategoryID: catShip, GroupID: 26},  // cruiser, in use
		{ItemID: 3, BPTypeID: 203, CategoryID: catShip, GroupID: 26},  // cruiser, stocked
		{ItemID: 4, BPTypeID: 204, CategoryID: catShip, GroupID: 27},  // battleship, on the line
		{ItemID: 5, BPTypeID: 205, CategoryID: catShip, GroupID: 419}, // battlecruiser: the pick
	}
	inUse := map[int64]bool{2: true}
	cooking := map[int64]bool{4: true}
	stock := map[int64]int{203: copyStockTarget}
	p, n, ok := pickOwnerCopy(prints, inUse, cooking, stock, nil)
	if !ok || p.ItemID != 5 || n != ownerCopyBatchMax {
		t.Fatalf("picked %+v x%d ok=%v; want item 5 x%d", p, n, ok, ownerCopyBatchMax)
	}
	if _, _, ok := pickOwnerCopy(prints[:4], inUse, cooking, stock, nil); ok {
		t.Fatalf("picked from a pool with nothing eligible")
	}
}

func TestHullPassOverKeepsFirstReasons(t *testing.T) {
	var h hullPassOver
	h.note(1, "Exequror: needs ~40000000 ISK on hand")
	h.note(1, "Exequror: again")
	h.note(2, "Osprey: b")
	h.note(3, "Omen: c")
	h.note(4, "Maller: d")
	want := hullPassOverPrefix + "Exequror: needs ~40000000 ISK on hand; Osprey: b; Omen: c"
	if got := h.line(); got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
	var empty hullPassOver
	if empty.line() != "" {
		t.Fatalf("empty pass-over logged")
	}
}
