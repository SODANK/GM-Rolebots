package rolebots

import "testing"

func TestFuzzyRank(t *testing.T) {
	names := []string{"Tritanium", "Pyerite", "Mexallon", "Isogen", "Nocxium", "Zydrine", "Megacyte", "Morphite",
		"Veldspar", "Scordite", "Pyroxeres", "Plagioclase", "Damage Control I", "Damage Control II", "Damage Control I Blueprint",
		"Cargohold Expander I", "Medium Shield Extender I", "Large Shield Extender II", "Rifter", "Rifter Blueprint", "Caldari Navy Antimatter Charge S"}
	var es []findEntry
	for i, n := range names {
		es = append(es, newFindEntry(int64(i+1), n, "x"))
	}
	for _, c := range []struct {
		q, first string
		clear    bool
	}{
		{"tritanium", "Tritanium", true},
		{"trit", "Tritanium", true},
		{"tritanum", "Tritanium", true},   // a letter dropped
		{"tirtanium", "Tritanium", true},  // two letters swapped
		{"pyrite", "Pyerite", true},       // a slip near the end
		{"veld", "Veldspar", true},
		{"control damage", "Damage Control I", false}, // words in any order, more than one answer
		{"shield ext large", "Large Shield Extender II", true},
		{"antimatter navy", "Caldari Navy Antimatter Charge S", true},
		{"rifter", "Rifter", true},
		{"cargo exp", "Cargohold Expander I", true},
	} {
		hits := fuzzyRank(c.q, es, 12)
		if len(hits) == 0 || hits[0].Name != c.first {
			got := "nothing"
			if len(hits) > 0 {
				got = hits[0].Name
			}
			t.Errorf("%q: first hit %s, want %s", c.q, got, c.first)
			continue
		}
		if findClear(hits) != c.clear {
			t.Errorf("%q: clear = %v, want %v (hits %v)", c.q, findClear(hits), c.clear, hits)
		}
	}
	if hits := fuzzyRank("zzzzqq", es, 12); len(hits) != 0 {
		t.Errorf("nonsense matched %v", hits)
	}
	if hits := fuzzyRank("", es, 12); hits != nil {
		t.Error("empty matched")
	}
}

func TestEditDistance(t *testing.T) {
	for _, c := range []struct {
		a, b string
		d    int
	}{{"abc", "abc", 0}, {"abc", "abd", 1}, {"abc", "ab", 1}, {"abc", "acb", 1}, {"kitten", "sitting", 3}} {
		if got := editDistance(c.a, c.b, 5); got != c.d {
			t.Errorf("%s/%s: %d, want %d", c.a, c.b, got, c.d)
		}
	}
	if got := editDistance("abcdef", "uvwxyz", 2); got <= 2 {
		t.Errorf("far words came out at %d", got)
	}
}
