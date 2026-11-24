package rolebots

import (
	"strings"
	"testing"
)

func TestPlaceKind(t *testing.T) {
	for id, want := range map[int64]string{10000002: placeRegion, 20000020: placeConstellation, 30000142: placeSystem, 31000001: placeSystem, 0: "", 60000004: ""} {
		if got := placeKindOf(id); got != want {
			t.Errorf("%d is %q, want %q", id, got, want)
		}
	}
	if s, a := splitPlace(10000002); s != 0 || a != 10000002 {
		t.Errorf("a region split into %d, %d", s, a)
	}
	if s, a := splitPlace(30000142); s != 30000142 || a != 0 {
		t.Errorf("a system split into %d, %d", s, a)
	}
}

func TestResolvePlace(t *testing.T) {
	for _, c := range []struct {
		words string
		id    int64 // 0 for not one place
		hits  int   // least offered when it is not
	}{
		{"alentene", 30005304, 0},
		{"Sinq Laison", 10000032, 0},
		{"sinq lasion", 10000032, 0}, // a slip of the keys
		{"sinq laison (region)", 10000032, 0},
		{"Sinq Laison region", 10000032, 0},
		{"Amevync", 0, 2},
		{"Amevync (system)", 30002900, 0},
		{"Amevync constellation", 20000300, 0},
		{"ale", 0, 2},
		{"alentene (region)", 0, 0},
		{"zzzzqq", 0, 0},
	} {
		e, ok, hits := resolvePlace(c.words, tPlaces, 12)
		if ok != (c.id != 0) || e.ID != c.id {
			t.Errorf("%q: %v %d, want %d", c.words, ok, e.ID, c.id)
		}
		if len(hits) < c.hits {
			t.Errorf("%q: %d offered, want %d", c.words, len(hits), c.hits)
		}
	}
	// What a click puts in the field means that place and no other.
	for _, e := range tPlaces {
		if got, ok, _ := resolvePlace(placeWords(e, tPlaces), tPlaces, 12); !ok || got.ID != e.ID {
			t.Errorf("%q does not mean %d", placeWords(e, tPlaces), e.ID)
		}
	}
	if w := placeWords(tPlaces[4], tPlaces); w != "Alentene" {
		t.Errorf("a system's own name is offered as %q", w)
	}
}

func TestAreaSystem(t *testing.T) {
	u := tMap()
	all := func(int64) bool { return true }
	none := func(int64) bool { return false }
	for _, c := range []struct {
		name       string
		from, area int64
		floor      float64
		skip       int64
		want       int64
	}{
		{"the one it is in", 30005304, 10000032, 0, 0, 30005304},
		{"the next when that is swept", 30005304, 10000032, 0, 30005304, 30002900},
		{"a constellation", 30005304, 20000300, 0, 0, 30002900},
		{"out of its own region", 30005304, 10000064, 0, 0, 30005305},
		{"not past the security floor", 30005304, 10000064, 0.5, 0, 0},
		{"nowhere", 30005304, 10000099, 0, 0, 0},
	} {
		got := u.areaSystem(c.from, c.area, c.floor, func(id int64) bool { return id != c.skip }, none)
		if got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	// Of two as near, the one that comes first.
	u = &universe{adj: map[int64][]int64{1: {2, 3}}, meta: map[int64]sysMeta{1: {}, 2: {RegionID: 9, Security: 1}, 3: {RegionID: 9, Security: 1}}}
	if got := u.areaSystem(1, 9, 0, all, none); got != 2 {
		t.Errorf("the nearest is %d", got)
	}
	if got := u.areaSystem(1, 9, 0, all, func(id int64) bool { return id == 3 }); got != 3 {
		t.Errorf("a distressed system came second to %d", got)
	}
	if u.areaRegion(9) != 9 || tMap().areaRegion(20000300) != 10000032 {
		t.Error("an area's region is wrong")
	}
}

func TestSortFleetLeaders(t *testing.T) {
	ls := []fleetLeader{{Name: "D", Leadership: 5, Busy: "off duty"}, {Name: "B", Leadership: 2}, {Name: "C", Leadership: 4}, {Name: "A", Leadership: 2}}
	sortFleetLeaders(ls)
	var names []string
	for _, l := range ls {
		names = append(names, l.Name)
	}
	if got := strings.Join(names, ""); got != "CABD" {
		t.Errorf("sorted %s", got)
	}
	if got := ls[0].label(); got != "C (Leadership 4)" {
		t.Errorf("labelled %q", got)
	}
}

func TestFleetOrderPlace(t *testing.T) {
	region := corpFleetOrder{AreaID: 10000032}
	if region.Place() != 10000032 || (corpFleetOrder{SystemID: 30005304}).Place() != 30005304 {
		t.Error("an order's place is wrong")
	}
	if !fleetOrderIsNew(&corpFleetOrder{AreaID: 10000064}, region) || fleetOrderIsNew(&corpFleetOrder{AreaID: 10000032}, region) {
		t.Error("a new area is not a new order, or the same one is")
	}
}
