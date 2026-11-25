package rolebots

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gurimoon/corp"
)

func TestHeadsToInherit(t *testing.T) {
	row := func(a corpArm, tier int, since int64, setBy string) armRow {
		return armRow{Arm: a, Tier: tier, Since: since, SetBy: setBy}
	}
	bot := func(id int64) bool { return id < 100 }
	for _, c := range []struct {
		name  string
		cur   map[corpArm]corpHead
		rows  map[int64]armRow
		ports map[int64]string
		ind   int64
		want  map[corpArm]int64
	}{
		{"a bot CEO's head", nil, map[int64]armRow{1: row(armMining, armTierHead, 5, "ceo")}, nil, 0, map[corpArm]int64{armMining: 1}},
		{"Finance, headed by no Director", nil, map[int64]armRow{1: row(armFinance, armTierHead, 5, "ceo")}, nil, 0, map[corpArm]int64{armFinance: 1}},
		{"not a member of the division", nil, map[int64]armRow{1: row(armMining, armTierJunior, 5, "ceo")}, nil, 0, nil},
		{"not a person", nil, map[int64]armRow{500: row(armMining, armTierHead, 5, "ceo")}, nil, 0, nil},
		{"not over an appointment", map[corpArm]corpHead{armMining: {CharID: 7}}, map[int64]armRow{1: row(armMining, armTierHead, 5, "ceo")}, nil, 0, nil},
		{"not the head of another division", map[corpArm]corpHead{armSales: {CharID: 1}}, map[int64]armRow{1: row(armMining, armTierHead, 5, "ceo")}, nil, 0, nil},
		{"not one the chair has written", nil, map[int64]armRow{1: row(armFinance, armTierHead, 5, "human")}, nil, 0, nil},
		{"one the chair placed and a bot CEO put in charge", nil, map[int64]armRow{1: row(armMining, armTierHead, 5, "human")},
			map[int64]string{1: portOperations}, 0, map[corpArm]int64{armMining: 1}},
		{"the same of Industry", nil, map[int64]armRow{1: row(armIndustry, armTierHead, 5, "human")}, nil, 1, map[corpArm]int64{armIndustry: 1}},
		{"the longest there of two", nil, map[int64]armRow{1: row(armMining, armTierHead, 9, "ceo"), 2: row(armMining, armTierHead, 5, "ceo")}, nil, 0,
			map[corpArm]int64{armMining: 2}},
		{"the first on a tie", nil, map[int64]armRow{1: row(armMining, armTierHead, 5, "ceo"), 2: row(armMining, armTierHead, 5, "ceo")}, nil, 0,
			map[corpArm]int64{armMining: 1}},
	} {
		got := headsToInherit(c.cur, c.rows, func(id int64) string { return c.ports[id] }, c.ind, bot)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestInheritRefusal(t *testing.T) {
	plain, dir := memberRank{Member: true}, memberRank{Member: true, Director: true}
	treasurer := memberRank{Member: true, Accountant: true}
	for _, c := range []struct {
		name string
		r    memberRank
		arm  corpArm
		ind  bool
		want error
	}{
		{"a Director", dir, armMining, false, nil},
		{"a department's head who lost the role", plain, armMining, false, errHeadNotDirector},
		{"the Industry Director", plain, armIndustry, true, nil},
		{"somebody else in Industry", plain, armIndustry, false, errHeadNotDirector},
		{"the Industry Director, of Mining", plain, armMining, true, errHeadNotDirector},
		{"the Treasurer", treasurer, armFinance, false, nil},
		{"the Treasurer who lost the role", plain, armFinance, false, errHeadNotDirector},
		{"a Treasurer, of Sales", treasurer, armSales, false, errHeadNotDirector},
		{"one who left", memberRank{}, armIndustry, true, errHeadNotMember},
		{"the CEO", memberRank{Member: true, CEO: true, Director: true}, armIndustry, true, errHeadIsCEO},
	} {
		if got := inheritRefusal(c.r, c.arm, c.ind); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// inheritMod is the fake corporation the day after a person took a bot's chair: the arm rows as the
// bot CEO's pass left them, and nobody appointed unless heads says so.
func inheritMod(t *testing.T, rows, heads []fakeRow) (*fakeDB, *Mod) {
	t.Helper()
	f := newManageDB()
	f.answers = append([]fakeAnswer{
		{"FROM rolebot_corp_heads", func([]driver.NamedValue) []fakeRow { return heads }},
		{"FROM rolebot_corp_arm_members WHERE corporationID", func([]driver.NamedValue) []fakeRow { return rows }},
	}, f.answers...)
	db := sql.OpenDB(f)
	savedChairs, savedArms := humanChairIndex.Load(), armIndex.Load()
	savedSet, savedAt, savedOK := botMailboxSet, botMailboxAt, botMailboxEverOK
	humanChairIndex.Store(&map[int64]int64{tCorp: tCEO})
	botMailboxSet = map[int64]struct{}{}
	for id, p := range tPilots {
		if p.bot {
			botMailboxSet[id] = struct{}{}
		}
	}
	botMailboxAt, botMailboxEverOK = clock.Now(), true
	t.Cleanup(func() {
		humanChairIndex.Store(savedChairs)
		armIndex.Store(savedArms)
		botMailboxSet, botMailboxAt, botMailboxEverOK = savedSet, savedAt, savedOK
		db.Close()
	})
	return f, &Mod{db: db}
}

func headWrites(f *fakeDB, prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, w := range f.writes {
		if strings.HasPrefix(w, prefix) {
			out = append(out, w)
		}
	}
	return out
}

// A person takes the chair: the bots a bot CEO put in charge keep their divisions. A person it put in
// charge, and a bot that has lost what qualified it, do not.
func TestInheritHeadsUnderAPerson(t *testing.T) {
	arm := func(id int64, a corpArm, tier int) fakeRow {
		return fakeRow{id, tCorp, string(a), int64(tier), int64(5), int64(0), int64(0), "ceo"}
	}
	f, m := inheritMod(t, []fakeRow{
		arm(tBotDir, armIndustry, armTierHead), arm(tBotSec, armSecurity, armTierHead),
		arm(tMiner, armMining, armTierHead), arm(tBotHaul, armSales, armTierHead),
		arm(tBotLead, armSecurity, armTierMember),
	}, nil)
	m.inheritHeads(context.Background(), tCorp)
	got := headWrites(f, "INSERT INTO rolebot_corp_heads")
	if len(got) != 2 {
		t.Fatalf("kept %d heads: %v", len(got), got)
	}
	for i, want := range []string{fmt.Sprintf("|%d|industry|%d|%d|0|", tCorp, tBotDir, tBotPlain), fmt.Sprintf("|%d|security|%d|0|0|", tCorp, tBotSec)} {
		if !strings.Contains(got[i], want) {
			t.Errorf("head %d is %q, want %q", i, got[i], want)
		}
	}
	if len(headWrites(f, "INSERT INTO rolebot_corp_posts")) != 1 {
		t.Error("the Security portfolio was not written")
	}
}

// A head that answered applications under the bot CEO goes on answering them.
func TestInheritHeadsKeepsTheOption(t *testing.T) {
	f, m := inheritMod(t, []fakeRow{{tBotSec, tCorp, "hr", int64(armTierHead), int64(5), int64(0), int64(0), "ceo"}}, nil)
	m.inheritHeads(context.Background(), tCorp)
	got := headWrites(f, "INSERT INTO rolebot_corp_heads")
	if len(got) != 1 || !strings.HasSuffix(got[0], fmt.Sprintf("|%d|", headFlagApplicant)) {
		t.Errorf("kept %v", got)
	}
}

// What the chair has decided stands: an appointment is not replaced, and a head stood down stays down.
func TestInheritHeadsLeavesTheChairsWord(t *testing.T) {
	f, m := inheritMod(t,
		[]fakeRow{{tBotSec, tCorp, "security", int64(armTierHead), int64(5), int64(0), int64(0), "ceo"},
			{tBotDir, tCorp, "mining", int64(armTierMember), int64(5), int64(0), int64(0), "human"}},
		[]fakeRow{{tCorp, "security", tSpare, int64(0), tCEO, tNow() - 60, int64(0)}})
	m.inheritHeads(context.Background(), tCorp)
	if got := headWrites(f, "INSERT INTO rolebot_corp_heads"); len(got) != 0 {
		t.Errorf("appointed %v", got)
	}
}

// The Director role qualifies an appointment; a kept Treasurer stands on its post.
func TestPruneHeadsKeepsInheritedPosts(t *testing.T) {
	saved := tPilots[tBotPlain]
	defer func() { tPilots[tBotPlain] = saved }()
	for _, c := range []struct {
		name  string
		setBy int64
		roles uint64
		gone  bool
	}{
		{"a kept Treasurer", 0, corp.RoleAccountant, false},
		{"an appointed Treasurer without Director", tCEO, corp.RoleAccountant, true},
		{"a kept Treasurer whose role was taken", 0, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := saved
			p.roles = c.roles
			tPilots[tBotPlain] = p
			f, m := inheritMod(t, nil, []fakeRow{{tCorp, "finance", tBotPlain, int64(0), c.setBy, tNow() - 60, int64(0)}})
			m.pruneHeads(context.Background(), tCorp)
			if got := len(headWrites(f, "DELETE FROM rolebot_corp_heads")) > 0; got != c.gone {
				t.Errorf("stood down = %v", got)
			}
		})
	}
}

// Under a person the Accountant role alone heads nothing; a Treasurer kept as head does.
func TestTreasurerHeadsFinanceOnlyWhenKept(t *testing.T) {
	saved := tPilots[tBotHaul]
	defer func() { tPilots[tBotHaul] = saved }()
	p := saved
	p.roles = corp.RoleAccountant
	tPilots[tBotHaul] = p
	for _, kept := range []bool{false, true} {
		var heads []fakeRow
		if kept {
			heads = []fakeRow{{tCorp, "finance", tBotHaul, int64(0), int64(0), tNow() - 60, int64(0)}}
		}
		_, m := inheritMod(t, nil, heads)
		pos := m.armPositions(context.Background(), tCorp)[tBotHaul]
		if got := pos.Arm == armFinance && pos.Tier == armTierHead; got != kept {
			t.Errorf("kept %v: placed %+v", kept, pos)
		}
	}
}
