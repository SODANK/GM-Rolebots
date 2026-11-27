package rolebots

import (
	"slices"
	"testing"
)

func TestFactoryMiningJustifiesMining(t *testing.T) {
	cases := []struct {
		name string
		s    armSignals
		want bool
	}{
		{"industry house mines beside its factory",
			armSignals{Members: 20, Tags: []string{"industry"}, HasFactory: true}, true},
		{"shipbuilder mines with no factory yet",
			armSignals{Members: 20, Tags: []string{"shipbuilding"}}, true},
		{"industry house too small for Industry",
			armSignals{Members: 9, Tags: []string{"industry"}}, false},
		{"laboratory without a staffed Industry arm",
			armSignals{Members: 20, Tags: []string{"research"}, HasFactory: true}, false},
		{"laboratory with Industry staffed",
			armSignals{Members: 20, Tags: []string{"research"}, HasFactory: true, IndustryStaffed: 2}, true},
		{"small laboratory with Industry staffed",
			armSignals{Members: 12, Tags: []string{"research"}, HasFactory: true, IndustryStaffed: 3}, false},
		{"research-led beside industry still waits on Industry",
			armSignals{Members: 20, Tags: []string{"research", "industry"}}, false},
		{"industry-led beside research mines at once",
			armSignals{Members: 20, Tags: []string{"industry", "research"}}, true},
		{"combat house with a factory does not",
			armSignals{Members: 20, Tags: []string{tagCombat}, HasFactory: true}, false},
	}
	for _, c := range cases {
		if got := armJustified(armMining, c.s); got != c.want {
			t.Errorf("%s: armJustified(mining) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIndustryHouseArmsIncludeMining(t *testing.T) {
	s := armSignals{Members: 20, Tags: []string{"industry"}, HasFactory: true}
	got := justifiedArms(s)
	if !slices.Contains(got, armIndustry) || !slices.Contains(got, armMining) {
		t.Fatalf("justified = %v, want Industry and Mining", got)
	}
	// The Operations Director comes with it.
	if !slices.Contains(armNeededPortfolios(got), portOperations) {
		t.Errorf("needed portfolios %v lack Operations", armNeededPortfolios(got))
	}
}

func TestArmFleetActivities(t *testing.T) {
	if got := armFleetActivities(nil, []corpArm{armIndustry, armMining}); !slices.Equal(got, []string{ActMining}) {
		t.Errorf("laboratory with Mining: got %v", got)
	}
	if got := armFleetActivities([]string{ActCombat}, []corpArm{armIndustry}); !slices.Equal(got, []string{ActCombat}) {
		t.Errorf("no Mining arm: got %v", got)
	}
	in := []string{ActMining, ActCombat}
	if got := armFleetActivities(in, []corpArm{armMining}); !slices.Equal(got, in) {
		t.Errorf("already seated: got %v", got)
	}
}

func TestPickOreRefiner(t *testing.T) {
	bots := map[int64]bool{10: true, 20: true}
	isBot := func(id int64) bool { return bots[id] }
	if got := pickOreRefiner(10, 20, isBot); got != 10 {
		t.Errorf("bot deputy: got %d", got)
	}
	if got := pickOreRefiner(99, 20, isBot); got != 20 {
		t.Errorf("human deputy: got %d, want the director", got)
	}
	if got := pickOreRefiner(0, 20, isBot); got != 20 {
		t.Errorf("no deputy: got %d, want the director", got)
	}
	if got := pickOreRefiner(99, 98, isBot); got != 0 {
		t.Errorf("both human: got %d", got)
	}
}
