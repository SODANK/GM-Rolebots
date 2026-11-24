package rolebots

import (
	"reflect"
	"strings"
	"testing"

	"gurimoon/corp"
)

func headScope(arms ...corpArm) commandScope {
	s := commandScope{Arms: map[corpArm]bool{}}
	for _, a := range arms {
		s.Arms[a] = true
	}
	return s
}

var ceoScope = commandScope{CEO: true}

func TestCommandScopeKeepsAHeadToItsDivision(t *testing.T) {
	mining := headScope(armMining)
	if !mining.Commands(armMining) || mining.Commands(armIndustry) {
		t.Error("a head commands its own division and no other")
	}
	if mining.Commands("") {
		t.Error("a head commands the corporation's own settings")
	}
	if !ceoScope.Commands("") || !ceoScope.Commands(armHR) {
		t.Error("the CEO commands everything")
	}
	if (commandScope{Arms: map[corpArm]bool{}}).Officer() {
		t.Error("a Director with no division commands something")
	}
	if got := ceoScope.Divisions(); len(got) != len(armOrder) {
		t.Errorf("the CEO has %d divisions, want %d", len(got), len(armOrder))
	}
}

func TestMgDivAllowedIgnoresAHandEditedAddress(t *testing.T) {
	mining := headScope(armMining)
	for _, c := range []struct {
		name  string
		scope commandScope
		ask   string
		want  string
	}{
		{"a head's own", mining, "mining", "mining"},
		{"a head asking for another", mining, "industry", "mining"},
		{"a head asking for the settings", mining, mgCorp, "mining"},
		{"a head asking for the appointments", mining, mgHeads, "mining"},
		{"a head asking for everybody", mining, mgPeople, "mining"},
		{"a head asking for nothing", mining, "", "mining"},
		{"the CEO's desk", ceoScope, "", ""},
		{"the CEO in a division", ceoScope, "research", "research"},
		{"the CEO's appointments", ceoScope, mgHeads, mgHeads},
		{"the CEO asking for rubbish", ceoScope, "pool", ""},
		{"a Director with no division", commandScope{}, "mining", ""},
	} {
		if got := mgDivAllowed(c.scope, c.ask); got != c.want {
			t.Errorf("%s: %q → %q, want %q", c.name, c.ask, got, c.want)
		}
	}
}

func TestManageFormForGivesAFormToItsDivisionOnly(t *testing.T) {
	for _, c := range []struct {
		name  string
		scope commandScope
		form  string
		arm   corpArm
		want  bool
	}{
		{"R&D researches", headScope(armResearch), "research", armResearch, true},
		{"Mining does not research", headScope(armMining), "research", armMining, false},
		{"Mining cannot post to R&D's page", headScope(armMining), "research", armResearch, false},
		{"every division requisitions", headScope(armFinance), "req", armFinance, true},
		{"a head sets no budget", headScope(armFinance), "budget", "", false},
		{"the CEO does", ceoScope, "budget", "", true},
		{"a head does not move the factory", headScope(armIndustry), "factory", armIndustry, false},
		{"the CEO does", ceoScope, "factory", armIndustry, true},
		{"the settings are on no division's page", ceoScope, "budget", armFinance, false},
		{"nothing by another name", ceoScope, "head", "", false},
	} {
		if got := manageFormFor(c.scope, c.form, c.arm) != nil; got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	seen := map[string]bool{}
	for _, f := range manageForms {
		if seen[f.Form] {
			t.Errorf("form %q is listed twice", f.Form)
		}
		seen[f.Form] = true
		if f.Write == nil || f.Parse == nil || f.Icon == nil || f.Title == "" || f.Short == "" || f.Note == "" {
			t.Errorf("form %q is not finished", f.Form)
		}
	}
	// The order desk keeps a kind in 16 characters, and every division but Finance and HR has
	// something of its own.
	for _, k := range []string{cmdKindHead, cmdKindStanding, cmdKindReq, cmdKindHaul, cmdKindSurvey, cmdKindAssign, cmdKindBid,
		cmdKindTrade, cmdKindPrintBuy, cmdKindMove, cmdKindSale, cmdKindInvite, cmdKindFactory} {
		if len(k) > 16 {
			t.Errorf("kind %q does not fit its column", k)
		}
	}
	for _, v := range []string{reqChair, reqChairBuy, refKindCommand} {
		if len(v) > 12 {
			t.Errorf("%q does not fit the requisition's column", v)
		}
	}
	if len(corpOrderHuman) > 8 {
		t.Errorf("%q does not fit the order's column", corpOrderHuman)
	}
}

func TestMayCancelCommand(t *testing.T) {
	ours := corpCommand{Arm: string(armMining)}
	ceos := corpCommand{Arm: string(armMining), PosterCEO: true}
	setting := corpCommand{}
	mining, security := headScope(armMining), headScope(armSecurity)
	if !mayCancelCommand(mining, ours) {
		t.Error("a head may withdraw its division's order")
	}
	if mayCancelCommand(security, ours) {
		t.Error("a head withdrew another division's order")
	}
	if mayCancelCommand(mining, ceos) {
		t.Error("a head withdrew the CEO's order")
	}
	if mayCancelCommand(mining, setting) {
		t.Error("a head withdrew a setting")
	}
	for _, c := range []corpCommand{ours, ceos, setting} {
		if !mayCancelCommand(ceoScope, c) {
			t.Errorf("the CEO could not withdraw %+v", c)
		}
	}
	if !commandOutranks(ceos, mining) || commandOutranks(ours, mining) || commandOutranks(ceos, ceoScope) {
		t.Error("the CEO's order stops a head's and nobody else's stops anybody's")
	}
}

func TestSameCommandTarget(t *testing.T) {
	cmd := func(kind string, p any) corpCommand { return corpCommand{Kind: kind, Payload: encodeCommandPayload(p)} }
	fleetMine := cmd(cmdKindFleet, cmdFleetPayload{Activity: ActMining})
	fleetHunt := cmd(cmdKindFleet, cmdFleetPayload{Activity: ActCombat})
	standMine := cmd(cmdKindStanding, cmdStandingPayload{Dept: portOperations})
	me := cmd(cmdKindWork, cmdWorkPayload{Kind: workKindME, BPItemID: 5})
	move := cmd(cmdKindMove, cmdBoardPayload{ItemID: 5})
	sale := cmd(cmdKindSale, cmdBoardPayload{ItemID: 6})
	build := cmd(cmdKindWork, cmdWorkPayload{Kind: workKindBuild, BPTypeID: 9})
	bid := cmd(cmdKindBid, cmdBoardPayload{TypeID: 34, StationID: 1})
	bidElse := cmd(cmdKindBid, cmdBoardPayload{TypeID: 34, StationID: 2})
	arm := cmd(cmdKindArm, cmdArmPayload{CharID: 3})
	post := cmd(cmdKindPosting, cmdPostingPayload{CharID: 3})
	for _, c := range []struct {
		name string
		a, b corpCommand
		want bool
	}{
		{"a fleet and a standing order of one kind", fleetMine, standMine, true},
		{"two kinds of fleet", fleetMine, fleetHunt, false},
		{"research on a print and moving it", me, move, true},
		{"two prints", move, sale, false},
		{"two builds", build, build, false},
		{"one bid", bid, bid, true},
		{"bids at two offices", bid, bidElse, false},
		{"a placement twice", arm, arm, true},
		{"a placement and a posting", arm, post, false},
		{"a fleet and a build", fleetMine, build, false},
	} {
		if got := sameCommandTarget(c.a, c.b); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
		if got := sameCommandTarget(c.b, c.a); got != c.want {
			t.Errorf("%s, the other way: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPlacementRefusal(t *testing.T) {
	mining := headScope(armMining)
	for _, c := range []struct {
		name     string
		scope    commandScope
		from, to corpArm
		head     bool
		byCEO    bool
		ok       bool
	}{
		{"the CEO moves anybody", ceoScope, armSecurity, armMining, false, true, true},
		{"nobody moves a head", ceoScope, armSecurity, armMining, true, false, false},
		{"a head takes in the pool", mining, "", armMining, false, false, true},
		{"a head lets go of its own", mining, armMining, "", false, false, true},
		{"a head poaches", mining, armSecurity, armMining, false, false, false},
		{"a head gives away", mining, armMining, armSecurity, false, false, false},
		{"a head moves the CEO's placement", mining, armMining, "", false, true, false},
		{"a head posts the CEO's placement", mining, armMining, armMining, false, true, true},
	} {
		if got := placementRefusal(c.scope, c.from, c.to, c.head, c.byCEO) == ""; got != c.ok {
			t.Errorf("%s: %v, want %v", c.name, got, c.ok)
		}
	}
}

func TestHeadRefusal(t *testing.T) {
	dir := memberRank{Member: true, Director: true}
	for _, c := range []struct {
		name string
		r    memberRank
		arm  corpArm
		bot  bool
		want error
	}{
		{"a Director", dir, armMining, false, nil},
		{"a Director for HR", dir, armHR, false, nil},
		{"a plain member", memberRank{Member: true}, armMining, false, errHeadNotDirector},
		{"a stranger", memberRank{}, armMining, false, errHeadNotMember},
		{"the CEO", memberRank{Member: true, Director: true, CEO: true}, armMining, false, errHeadIsCEO},
		{"no division", dir, "pool", false, errHeadNoArm},
		{"under a bot CEO", dir, armMining, true, errHeadBotCEO},
	} {
		if got := headRefusal(c.r, c.arm, c.bot); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHeadsDisplaced(t *testing.T) {
	cur := map[corpArm]corpHead{
		armMining:   {Arm: armMining, CharID: 1},
		armSecurity: {Arm: armSecurity, CharID: 2},
		armSales:    {Arm: armSales, CharID: 3},
	}
	ids := func(hs []corpHead) []int64 {
		var out []int64
		for _, h := range hs {
			out = append(out, h.CharID)
		}
		return out
	}
	for _, c := range []struct {
		name string
		h    corpHead
		want []int64
	}{
		{"a new pilot takes a held division", corpHead{Arm: armMining, CharID: 9}, []int64{1}},
		{"a head changes division", corpHead{Arm: armIndustry, CharID: 2}, []int64{2}},
		{"a head takes another's division", corpHead{Arm: armMining, CharID: 2}, []int64{1, 2}},
		{"the same again", corpHead{Arm: armMining, CharID: 1}, nil},
		{"a new pilot in an empty one", corpHead{Arm: armHR, CharID: 9}, nil},
	} {
		got := ids(headsDisplaced(cur, c.h))
		m := map[int64]bool{}
		for _, id := range got {
			m[id] = true
		}
		w := map[int64]bool{}
		for _, id := range c.want {
			w[id] = true
		}
		if !reflect.DeepEqual(m, w) {
			t.Errorf("%s: displaced %v, want %v", c.name, got, c.want)
		}
	}
}

func TestManageToken(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	const now = int64(1_000_000)
	tok := manageToken(key, 7, 98, 500, now+manageTokenSec)
	ok := func(token string, char, corpID, issued, at int64) bool {
		return manageTokenOK(key, token, char, corpID, issued, at)
	}
	if !ok(tok, 7, 98, 500, now) {
		t.Fatal("a fresh token is refused")
	}
	for name, got := range map[string]bool{
		"expired":                 ok(tok, 7, 98, 500, now+manageTokenSec),
		"another pilot":           ok(tok, 8, 98, 500, now),
		"another corporation":     ok(tok, 7, 99, 500, now),
		"a new key":               ok(tok, 7, 98, 501, now),
		"no key":                  ok(tok, 7, 98, 0, now),
		"empty":                   ok("", 7, 98, 500, now),
		"a later expiry stuck on": ok("9999999"+tok, 7, 98, 500, now),
		"no signature":            ok("1000900-", 7, 98, 500, now),
		"dated too far off":       ok(manageToken(key, 7, 98, 500, now+10*manageTokenSec), 7, 98, 500, now),
	} {
		if got {
			t.Errorf("accepted: %s", name)
		}
	}
}

func TestNonceIsUsedOnce(t *testing.T) {
	n := newNonce()
	if !useNonce(7, n) {
		t.Fatal("a fresh confirmation is refused")
	}
	if useNonce(7, n) {
		t.Error("a reload gave the order twice")
	}
	if !useNonce(8, n) {
		t.Error("one pilot's confirmation used up another's")
	}
	if useNonce(7, "") || useNonce(7, "short") {
		t.Error("something that is no confirmation passed")
	}
}

func TestHumanChairActsThroughABot(t *testing.T) {
	c := humanChairCEO(98, 42)
	if c.CharID != 42 || c.Origin != ceoOriginHuman {
		t.Fatalf("chair = %+v", c)
	}
	for _, p := range []string{powerIdentity, powerDirective, powerDiplomacy, powerAlliance, powerWar, powerHR, powerRoles,
		powerTreasury, powerEstate, powerShares} {
		if c.powerOn(p) {
			t.Errorf("a human chair's stand-in has the %s power", p)
		}
	}
	for _, p := range []string{powerProduction, powerBusiness, powerStarbase} {
		if !c.powerOn(p) {
			t.Errorf("a human chair's stand-in lacks the %s power", p)
		}
	}
}

func TestPickChairHand(t *testing.T) {
	for _, c := range []struct {
		name  string
		cands []handCand
		want  int64
	}{
		{"nobody", nil, 0},
		{"a Director over a Trader", []handCand{{CharID: 1, Rank: 1, On: true}, {CharID: 2, Rank: 2, On: true}}, 2},
		{"whoever is on", []handCand{{CharID: 1, Rank: 2}, {CharID: 2, On: true}}, 2},
		{"the division's head, asleep or not", []handCand{{CharID: 1, Rank: 2, On: true}, {CharID: 2, Rank: 2, Head: true}}, 2},
		{"the steady one on a tie", []handCand{{CharID: 9, Rank: 2, On: true}, {CharID: 3, Rank: 2, On: true}}, 3},
	} {
		if got := pickChairHand(c.cands); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestChairDeskOptionsAreOffUntilTicked(t *testing.T) {
	for _, a := range armOrder {
		flag, _ := headFlagOf(a)
		plain, ticked := chairDeskNames(corpHead{Arm: a}), chairDeskNames(corpHead{Arm: a, Flags: flag})
		if flag == 0 {
			if len(plain) == 0 {
				t.Errorf("a bot head of %s does nothing", a)
			}
			continue
		}
		if len(ticked) != len(plain)+1 {
			t.Errorf("%s: desk %v, with its option %v", a, plain, ticked)
		}
		// Another division's option is nothing to this one.
		if got := chairDeskNames(corpHead{Arm: a, Flags: ^flag}); len(got) != len(plain) {
			t.Errorf("%s: another division's option changed its desk to %v", a, got)
		}
	}
}

func TestRotateFrom(t *testing.T) {
	ids := []int64{3, 5, 9}
	for _, c := range []struct {
		from int64
		want []int64
	}{{0, []int64{3, 5, 9}}, {5, []int64{5, 9, 3}}, {6, []int64{9, 3, 5}}, {10, []int64{3, 5, 9}}} {
		if got := rotateFrom(ids, c.from); !reflect.DeepEqual(got, c.want) {
			t.Errorf("from %d: %v, want %v", c.from, got, c.want)
		}
	}
}

func TestHumanChairIndex(t *testing.T) {
	saved := humanChairIndex.Load()
	defer humanChairIndex.Store(saved)
	humanChairIndex.Store(&map[int64]int64{98: 7})
	if id, ok := humanChaired(98); !ok || id != 7 {
		t.Errorf("humanChaired = %d, %v", id, ok)
	}
	if !corpDeskRun(98) || corpDeskRun(99) || corpDeskRun(0) {
		t.Error("corpDeskRun follows the index")
	}
	if _, managed := managedCEO(98); managed {
		t.Error("a human chair reads as a bot CEO")
	}
	if payrollManaged(98) {
		t.Error("a human chair runs a payroll")
	}
}

func TestLikeEsc(t *testing.T) {
	if got, want := likeEsc(`50%_a\b`), `50\%\_a\\b`; got != want {
		t.Errorf("likeEsc = %q, want %q", got, want)
	}
}

func TestDirectorTest(t *testing.T) {
	if !holdsDirector(corp.RoleDirector|corp.RoleTrader) || holdsDirector(corp.RoleTrader) || !holdsTrader(corp.RoleTrader) {
		t.Error("role tests")
	}
}

func TestLeftoverCutoff(t *testing.T) {
	bot := func(id int64) bool { return id < 100 }
	for _, c := range []struct {
		name  string
		heads map[corpArm]corpHead
		want  int64
	}{
		{"no heads: everything a desk posted is left over", nil, 1000},
		{"a person heads: the same", map[corpArm]corpHead{armMining: {CharID: 500, Since: 200}}, 1000},
		{"the oldest bot head's desk", map[corpArm]corpHead{armMining: {CharID: 5, Since: 700}, armSales: {CharID: 6, Since: 300}}, 300},
	} {
		if got := leftoverCutoff(c.heads, bot, 1000); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestPrintSaleWithdrawnMayBeMarkedAgain(t *testing.T) {
	if !printSaleRemarkable(printSaleWithdrawn, 0, 1000) {
		t.Error("a sale the chair took back can never be ordered again")
	}
	if printSaleRemarkable(printSaleWanted, 0, 1000) || printSaleRemarkable(printSaleListed, 0, 1000) {
		t.Error("a copy still out was marked again")
	}
}

func TestLockdownsOfAHumanCorporationAreNotTheModules(t *testing.T) {
	saved := humanChairIndex.Load()
	defer humanChairIndex.Store(saved)
	humanChairIndex.Store(&map[int64]int64{98: 7})
	m := &Mod{} // no database: touching one would panic
	if ok, err := m.lockPrint(t.Context(), 98, 5); ok || err != nil {
		t.Errorf("lockPrint = %v, %v", ok, err)
	}
	if ok, err := m.unlockPrint(t.Context(), 98, 5); ok || err != nil {
		t.Errorf("unlockPrint = %v, %v", ok, err)
	}
}

func TestDirectorPlace(t *testing.T) {
	heads := map[corpArm]corpHead{armMining: {Arm: armMining, CharID: 1}}
	pending := map[int64]corpArm{2: armSecurity}
	rows := map[int64]armRow{3: {CharID: 3, Arm: armSales}, 4: {CharID: 4, Arm: "bogus"}, 1: {CharID: 1, Arm: armSales}}
	for _, c := range []struct {
		id   int64
		kind string
		arm  corpArm
	}{
		{1, "heads", armMining},     // the appointment outranks a placement row
		{2, "pending", armSecurity}, // ordered, the desk has not run
		{3, "in", armSales},         // placed as a member
		{4, "", ""},                 // a row naming no division
		{5, "", ""},
	} {
		kind, arm := directorPlace(c.id, heads, pending, rows)
		if kind != c.kind || arm != c.arm {
			t.Errorf("director %d: got %q %q, want %q %q", c.id, kind, arm, c.kind, c.arm)
		}
	}
}

func TestAssignDivisionsPins(t *testing.T) {
	need := map[divPurpose]bool{purArmory: true, purSpoils: true}
	t.Run("a pin holds the purpose where the chair put it", func(t *testing.T) {
		out, moves := assignDivisions(divNeeds{Need: need, Pin: map[divPurpose]int{purArmory: 4}})
		if out[purArmory] != 4 || len(moves) != 0 {
			t.Errorf("got %v %v", out, moves)
		}
		if out[purSpoils] == 4 || out[purSpoils] == 0 {
			t.Errorf("the other purpose should take another hangar, got %v", out)
		}
	})
	t.Run("a pin carries the stock out of the old hangar", func(t *testing.T) {
		out, moves := assignDivisions(divNeeds{Need: need, Have: map[divPurpose]int{purArmory: 2}, Pin: map[divPurpose]int{purArmory: 5}})
		if out[purArmory] != 5 || len(moves) != 1 || moves[0].From != 2 || moves[0].To != 5 {
			t.Errorf("got %v %v", out, moves)
		}
	})
	t.Run("stock stays where another purpose still stands", func(t *testing.T) {
		_, moves := assignDivisions(divNeeds{Need: need, Have: map[divPurpose]int{purArmory: 2, purSpoils: 2}, Pin: map[divPurpose]int{purArmory: 5}})
		if len(moves) != 0 {
			t.Errorf("the spoils still stand in 2, got %v", moves)
		}
	})
	t.Run("none keeps a needed purpose out", func(t *testing.T) {
		out, moves := assignDivisions(divNeeds{Need: need, Have: map[divPurpose]int{purArmory: 3}, Occupied: map[int]bool{3: true},
			Pin: map[divPurpose]int{purArmory: pinNone}})
		if _, ok := out[purArmory]; ok || len(moves) != 0 {
			t.Errorf("got %v %v", out, moves)
		}
	})
	t.Run("a pin on a claimed hangar is no pin", func(t *testing.T) {
		out, _ := assignDivisions(divNeeds{Need: need, DeskClaimed: map[int]bool{3: true}, Pin: map[divPurpose]int{purArmory: 3}})
		if out[purArmory] == 3 || out[purArmory] == 0 {
			t.Errorf("got %v", out)
		}
	})
}

func TestHangarChoiceRefusal(t *testing.T) {
	desk, named := map[int]bool{1: true, 3: true}, map[int]bool{5: true}
	for _, c := range []struct {
		v    int
		deny bool
	}{{0, false}, {pinNone, false}, {2, false}, {4, false}, {3, true}, {5, true}, {6, true}, {7, true}, {1, true}} {
		if got := hangarChoiceRefusal(purArmory, c.v, desk, named) != ""; got != c.deny {
			t.Errorf("hangar %d: refused %v, want %v", c.v, got, c.deny)
		}
	}
	if v, ok := parseHangarChoice("none"); !ok || v != pinNone {
		t.Error("none")
	}
	if v, ok := parseHangarChoice(""); !ok || v != 0 {
		t.Error("automatic")
	}
	if _, ok := parseHangarChoice("x"); ok {
		t.Error("nonsense parsed")
	}
}

func TestGroupOwed(t *testing.T) {
	rows := []owedRow{{ID: 1, CharID: 20, Amount: 100, Why: "trade run"}, {ID: 2, CharID: 10, Amount: 50, Why: "haul"},
		{ID: 3, CharID: 20, Amount: 25, Why: "haul"}}
	order, by := groupOwed(rows)
	if len(order) != 2 || order[0] != 20 || order[1] != 10 {
		t.Fatalf("the member owed longest goes first: %v", order)
	}
	if len(by[20]) != 2 || by[20][0].ID != 1 || by[20][1].ID != 3 {
		t.Errorf("a member's jobs run oldest first: %v", by[20])
	}
	if got := payslipLines(by[20]); !strings.Contains(got, "trade run") || !strings.Contains(got, "haul") {
		t.Errorf("the payslip lists the jobs: %q", got)
	}
	if wageNote(true) == "" || wageNote(false) != "" {
		t.Error("the going-rate note")
	}
}
