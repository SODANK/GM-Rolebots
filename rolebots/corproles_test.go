package rolebots

import (
	"testing"

	"gurimoon/corp"
)

// ── department bundles ──────────────────────────────────────────────────────

func TestArmRolesGeneralBits(t *testing.T) {
	cases := []struct {
		name string
		arm  corpArm
		tier int
		want uint64
		not  uint64
	}{
		{"sales member trades", armSales, armTierMember, corp.RoleTrader, 0},
		{"security member is a security officer", armSecurity, armTierMember, corp.RoleSecurityOfficer, corp.RoleEquipmentConfig},
		{"security head configures equipment", armSecurity, armTierHead, corp.RoleSecurityOfficer | corp.RoleEquipmentConfig, 0},
		{"infrastructure member runs towers", armInfrastructure, armTierMember, corp.RoleStarbaseConfig, 0},
		{"industry member is a factory manager", armIndustry, armTierMember, corp.RoleFactoryManager | armQueryAll, 0},
	}
	for _, c := range cases {
		got := armRoles(c.arm, c.tier, false)
		if got&c.want != c.want {
			t.Errorf("%s: armRoles = %#x, missing %#x", c.name, got, c.want&^got)
		}
		if got&c.not != 0 {
			t.Errorf("%s: armRoles = %#x, should not hold %#x", c.name, got, got&c.not)
		}
		if got&^armRoleMask != 0 {
			t.Errorf("%s: armRoles grants %#x outside armRoleMask", c.name, got&^armRoleMask)
		}
	}
}

func TestArmOwnRolesQueryAndTake(t *testing.T) {
	p := &divProfile{CorpID: 1, div: map[divPurpose]int{purArmory: 4}}
	if got, want := armOwnRoles(p, armResearch), divQueryBit(2)|divTakeBit(2); got != want {
		t.Errorf("R&D own roles = %#x, want %#x (Blueprint Library)", got, want)
	}
	if got, want := armOwnRoles(p, armSecurity), divQueryBit(4)|divTakeBit(4); got != want {
		t.Errorf("Security own roles = %#x, want %#x (Armory in 4)", got, want)
	}
	if got := armOwnRoles(p, armSales); got&divTakeBit(4) != 0 {
		t.Errorf("Sales holds take on the borrowed Armory division: %#x", got)
	}
	if got := armOwnRoles(p, armIndustry); got != divQueryBit(1)|divTakeBit(1)|divQueryBit(3)|divTakeBit(3) {
		t.Errorf("Industry own roles = %#x, want Materials and Production", got)
	}
	if armOwnRoles(p, armSecurity)&^armRoleMask != 0 {
		t.Error("own roles fall outside armRoleMask")
	}
}

func TestOwnedTakeBitsExemptsOwnWarChest(t *testing.T) {
	p := &divProfile{CorpID: 1, div: map[divPurpose]int{purArmory: 4, purYard: 5}}
	guarded := p.guardedTakeBits()
	if got := guarded &^ ownedTakeBits(p, armSecurity); got != divTakeBit(5) {
		t.Errorf("Security still barred from %#x, want only the Yard %#x", got, divTakeBit(5))
	}
	if got := guarded &^ ownedTakeBits(p, armResearch); got != guarded {
		t.Errorf("R&D exempted from %#x of the war chest", guarded&^got)
	}
}

// ── Base scope and the location mirror ──────────────────────────────────────

func TestSplitBaseRoles(t *testing.T) {
	want := armIndustryRoles | divTakeBit(2) | corp.RoleTrader
	if g, b := splitBaseRoles(want, false); g != want || b != 0 {
		t.Errorf("off: global %#x base %#x", g, b)
	}
	g, b := splitBaseRoles(want, true)
	if b != armIndustryRoles|divTakeBit(2) {
		t.Errorf("base = %#x, want hangar bits and Factory Manager", b)
	}
	if g != corp.RoleTrader {
		t.Errorf("global = %#x, want Trader only", g)
	}
}

func TestMirrorLocationRoles(t *testing.T) {
	hand := divTakeBit(5) // ticked by hand at HQ only
	cols := [3]uint64{hand, 0, 0}
	got := mirrorLocationRoles(0, divTakeBit(1)|corp.RoleTrader, cols)
	for i, c := range got {
		if c&divTakeBit(1) == 0 {
			t.Errorf("column %d lacks the corporation-wide take", i)
		}
		if c&corp.RoleTrader != 0 {
			t.Errorf("column %d got a general role", i)
		}
	}
	if got[0]&hand == 0 {
		t.Error("the hand-set HQ bit was lost")
	}
	back := mirrorLocationRoles(divTakeBit(1)|corp.RoleTrader, corp.RoleTrader, got)
	if back != cols {
		t.Errorf("revoking gave %v, want %v", back, cols)
	}
}

// ── portal text ─────────────────────────────────────────────────────────────

func TestArmGrantShortNamesTheRoles(t *testing.T) {
	p := &divProfile{CorpID: 1, div: map[divPurpose]int{purArmory: 4}}
	cases := []struct {
		arm  corpArm
		want []string
	}{
		{armSales, []string{"Trader", "take from Trade"}},
		{armSecurity, []string{"Security Officer", "take from Armory"}},
		{armInfrastructure, []string{"Config Starbase Equipment", "take from Starbase Kit, Fuel"}},
		{armResearch, []string{"Factory Manager", "view every hangar", "take from Blueprint Library"}},
	}
	for _, c := range cases {
		got := armGrantShort(p, c.arm, armTierMember, false)
		for _, w := range c.want {
			if !contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.arm, got, w)
			}
		}
	}
	if got := armGrantShort(p, armResearch, armTierMember, true); !contains(got, "at your Base office") {
		t.Errorf("Base-scoped text %q does not say so", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ── sign-outs ───────────────────────────────────────────────────────────────

func TestSignoutVerdict(t *testing.T) {
	const grace = 72 * 3600
	cases := []struct {
		name     string
		missing  int64
		left     bool
		warnedAt int64
		now      int64
		want     string
	}{
		{"all held", 0, false, 0, 1000, watchHold},
		{"first missing warns", 1, false, 0, 1000, watchWarn},
		{"inside grace holds", 1, false, 1000, 1000 + grace, watchHold},
		{"past grace is stolen", 1, false, 1000, 1001 + grace, watchStolen},
		{"left the corporation is stolen at once", 1, true, 0, 1000, watchStolen},
	}
	for _, c := range cases {
		if got := signoutVerdict(c.missing, c.left, c.warnedAt, c.now, grace); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
