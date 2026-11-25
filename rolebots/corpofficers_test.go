package rolebots

import (
	"testing"

	"gurimoon/corp"
)

const (
	tDir = corp.RoleDirector
	tPM  = corp.RolePersonnelManager
	tAcc = corp.RoleAccountant
)

func TestOfficerUpdateLeavesWhatItDidNotGrant(t *testing.T) {
	cases := []struct {
		name                         string
		cur, grant, want, next, kept uint64
	}{
		{"a Director given by hand stands", tDir, 0, 0, tDir, 0},
		{"a Director the pass dealt is stood down", tDir, tDir, 0, 0, 0},
		{"a dealt office is recorded", 0, 0, tDir, tDir, tDir},
		{"dealing what was given does not adopt it", tDir, 0, tDir, tDir, 0},
		{"a hand grant survives a lost dealt office", tDir | tAcc, tAcc, 0, tDir, 0},
		{"a grant the member no longer holds is forgotten", 0, tDir, 0, 0, 0},
		{"a dealt office changes", tDir, tDir, tPM, tPM, tPM},
	}
	for _, c := range cases {
		next, kept := officerUpdate(c.cur, c.grant, c.want)
		if next != c.next || kept != c.kept {
			t.Errorf("%s: got %#x/%#x, want %#x/%#x", c.name, next, kept, c.next, c.kept)
		}
	}
}

func TestDealOfficersAroundHumans(t *testing.T) {
	members := []roleMember{
		{CharID: 1, Trust: 9, Human: true, HeadsIndustry: true},
		{CharID: 2, Trust: 8},
		{CharID: 3, Trust: 7, Human: true, Given: tDir},
		{CharID: 4, Trust: 6, Human: true, Given: tDir, Treasurer: true},
		{CharID: 5, Trust: 5},
	}
	got := dealOfficers(members, 1, 1, 1)
	want := map[int64]uint64{2: tDir, 5: tPM, 4: tAcc}
	if len(got) != len(want) {
		t.Fatalf("dealt %v, want %v", got, want)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("member %d dealt %#x, want %#x", id, got[id], w)
		}
	}
}

func TestHeadHoldsDirector(t *testing.T) {
	human := roleMember{CharID: 1, Human: true}
	fresh := roleMember{CharID: 1, Human: true, Untenured: true}
	treasurer := roleMember{CharID: 2, Human: true, Treasurer: true}
	cases := []struct {
		name     string
		rm       roleMember
		want     uint64
		industry int64
		holds    bool
	}{
		{"the human Director of Industry", human, 0, 1, true},
		{"a human too new for the wallet", fresh, 0, 1, false},
		{"a bot Director of Industry", roleMember{CharID: 1}, 0, 1, false},
		{"the human Treasurer", treasurer, tAcc, 1, true},
		{"a Treasurer being stood down", treasurer, 0, 1, false},
		{"a human who heads nothing", human, 0, 9, false},
	}
	for _, c := range cases {
		if got := headHoldsDirector(c.rm, c.want, c.industry); got != c.holds {
			t.Errorf("%s: %v, want %v", c.name, got, c.holds)
		}
	}
}

func TestOfficerPostOfKeepsTheTreasurer(t *testing.T) {
	if got := officerPostOf(tDir|tAcc, postTreasurer); got != postTreasurer {
		t.Errorf("a Treasurer with Director reads %q", got)
	}
	if got := officerPostOf(tDir|tAcc, postDirector); got != postDirector {
		t.Errorf("a Director with Accountant reads %q", got)
	}
	if got := officerPostOf(tDir, postTreasurer); got != postDirector {
		t.Errorf("a former Treasurer reads %q", got)
	}
}

func TestRebaseRolesKeepsAGrantMadeMeanwhile(t *testing.T) {
	const trader = corp.RoleTrader
	if got := rebaseRoles(0, trader, tDir); got != tDir|trader {
		t.Errorf("a Director given during the think was lost: %#x", got)
	}
	if got := rebaseRoles(tDir, tDir|trader, 0); got != trader {
		t.Errorf("a Director taken during the think came back: %#x", got)
	}
	if got := rebaseRoles(trader, 0, trader|tDir); got != tDir {
		t.Errorf("the caller's own removal was lost: %#x", got)
	}
}
