package rolebots

import (
	"math"
	"testing"
	"time"

	"gurimoon/destiny"
)

// ── what counts as fighting the bot ─────────────────────────────────────────

func TestAggroIDsKeepsOnlyAggro(t *testing.T) {
	hs := []gridHostile{
		{ID: 1, Aggro: true},
		{ID: 2},
		{ID: 3, Aggro: true, Assist: true, Elective: true},
	}
	got := aggroIDs(hs)
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("aggroIDs = %v, want [1 3]", got)
	}
}

func TestElectiveOnly(t *testing.T) {
	cases := []struct {
		name string
		hs   []gridHostile
		want bool
	}{
		{"nothing", nil, false},
		{"only sightings", []gridHostile{{ID: 1}}, false},
		{"friend's attacker", []gridHostile{{ID: 1, Aggro: true, Assist: true, Elective: true}}, true},
		{"war target in transit", []gridHostile{{ID: 1, Aggro: true, Elective: true}}, true},
		{"elective plus sighting", []gridHostile{{ID: 1, Aggro: true, Elective: true}, {ID: 2}}, true},
		{"something shooting the bot", []gridHostile{{ID: 1, Aggro: true, Elective: true}, {ID: 2, Aggro: true}}, false},
		{"police on the bot", []gridHostile{{ID: 1, Aggro: true, Police: true}}, false},
	}
	for _, c := range cases {
		if got := electiveOnly(c.hs); got != c.want {
			t.Errorf("%s: electiveOnly = %v, want %v", c.name, got, c.want)
		}
	}
}

// A war target is elective but not an assist: the "came to the aid of" path must not claim it.
func TestAssistOnlyIgnoresWarTargets(t *testing.T) {
	if assistOnly([]gridHostile{{ID: 1, Aggro: true, Elective: true}}) {
		t.Fatal("assistOnly counted a transit war target as a friend's fight")
	}
}

func TestPoliceEngaged(t *testing.T) {
	if policeEngaged([]gridHostile{{ID: 1, Police: true}}) {
		t.Error("a police ball that is not aggro must not count")
	}
	if !policeEngaged([]gridHostile{{ID: 1, Aggro: true}, {ID: 2, Aggro: true, Police: true}}) {
		t.Error("an aggro police ball must count")
	}
}

// ── who may be shot ─────────────────────────────────────────────────────────

func TestFactionHostile(t *testing.T) {
	const caldari, guristas, gallente = 500001, 500010, 500004
	mx := map[[2]int64]float64{
		{caldari, guristas}: -9, // one-sided, as the pirate rows are
		{caldari, gallente}: 0.5,
	}
	cases := []struct {
		from, to int64
		want     bool
	}{
		{caldari, caldari, false},
		{0, caldari, true},
		{caldari, 0, true},
		{caldari, guristas, true},
		{guristas, caldari, false}, // no reverse row
		{caldari, gallente, false},
	}
	for _, c := range cases {
		if got := factionHostile(mx, c.from, c.to); got != c.want {
			t.Errorf("factionHostile(%d, %d) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
	if !factionAtOdds(mx, guristas, caldari) {
		t.Error("factionAtOdds must read the one-sided pirate row from either side")
	}
	if factionAtOdds(mx, caldari, gallente) {
		t.Error("factionAtOdds read friends as at odds")
	}
}

func TestRoeAllowsWar(t *testing.T) {
	if ok, _, _ := roeAllows(StanceHostile, DoctrineDefence, BandPoliced, false, false, true); !ok {
		t.Error("a war makes a hostile pilot shootable in policed space")
	}
	if ok, _, _ := roeAllows(StanceNeutral, DoctrineDefence, BandPoliced, false, false, true); ok {
		t.Error("a war needs a hostile stance")
	}
	if ok, _, _ := roeAllows(StanceHostile, DoctrineNone, BandNull, false, false, true); ok {
		t.Error("no combat wing, no fight")
	}
	if ok, _, _ := roeAllows(StanceKin, DoctrinePredation, BandNull, true, false, true); ok {
		t.Error("kin is spared before anything else")
	}
}

// A navy or police wing commits no crime in lowsec: a grudge is not a flag. The tester case was
// a Republic Security Services pilot opening on an unflagged pilot at a 0.4 gate.
func TestRoeAllowsLawfulDoctrineInLowsec(t *testing.T) {
	for _, doc := range []Doctrine{DoctrineSecurity, DoctrineDefence} {
		if ok, nerve, _ := roeAllows(StanceHostile, doc, BandLow, false, false, false); ok || nerve {
			t.Errorf("%s shot an unflagged hostile in lowsec", doc)
		}
		if ok, _, _ := roeAllows(StanceNeutral, doc, BandLow, false, false, false); ok {
			t.Errorf("%s shot an unflagged neutral in lowsec", doc)
		}
		if ok, _, _ := roeAllows(StanceNeutral, doc, BandLow, true, false, false); !ok {
			t.Errorf("%s must still shoot a criminal in lowsec", doc)
		}
		if ok, _, _ := roeAllows(StanceNeutral, doc, BandLow, false, true, false); !ok {
			t.Errorf("%s must still shoot an outlaw in lowsec", doc)
		}
		if ok, _, _ := roeAllows(StanceHostile, doc, BandLow, false, false, true); !ok {
			t.Errorf("%s must still shoot a war target in lowsec", doc)
		}
		if ok, _, _ := roeAllows(StanceHostile, doc, BandNull, false, false, false); !ok {
			t.Errorf("%s still fights rivals in nullsec", doc)
		}
	}
	// Guns for hire take the record for the work.
	if ok, _, _ := roeAllows(StanceHostile, DoctrineMercenary, BandLow, false, false, false); !ok {
		t.Error("a mercenary still fights a hostile in lowsec")
	}
}

func TestAssistAllows(t *testing.T) {
	if ok, _, _ := assistAllows(assistDuty, DoctrineDefence, BandPoliced, false, false, false, true); !ok {
		t.Error("a corpmate's war enemy may be engaged")
	}
	if ok, _, _ := assistAllows(assistDuty, DoctrineDefence, BandPoliced, false, false, false, false); ok {
		t.Error("joining against a neutral in policed space would be a crime")
	}
	if ok, nerve, _ := assistAllows(assistBlue, DoctrineDefence, BandNull, true, false, false, false); ok || !nerve {
		t.Error("a blue's fight is a roll, not a duty")
	}
	if ok, _, _ := assistAllows(assistDuty, DoctrineNone, BandNull, true, false, false, false); ok {
		t.Error("no combat wing never assists")
	}
}

// ── fight or flight in transit ──────────────────────────────────────────────

func TestTransitWarOdds(t *testing.T) {
	if got := transitWarOdds(0.5, neutralAggressionDefault); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("a default-nerve pilot rolls the base: got %v", got)
	}
	if got := transitWarOdds(0.5, neutralAggressionDefault*4); got != 1 {
		t.Errorf("a hothead clamps at 1: got %v", got)
	}
	if got := transitWarOdds(0.5, neutralAggressionDefault/2); math.Abs(got-0.25) > 1e-9 {
		t.Errorf("a timid pilot rolls less: got %v", got)
	}
	if transitWarOdds(0, 1) != 0 || transitWarOdds(0.5, 0) != 0 {
		t.Error("a zero base or zero nerve never fights")
	}
}

func TestTransitWarRollIsSticky(t *testing.T) {
	const decider, target = -987654, 123456
	if !transitWarRoll(decider, target, 1) {
		t.Fatal("certain odds must fight")
	}
	// Near-zero odds, not zero: zero is the operator's off switch and answers before the table.
	if !transitWarRoll(decider, target, 1e-12) {
		t.Error("the choice must stick for the encounter, whatever the odds now")
	}
	if transitWarRoll(decider, target, 0) {
		t.Error("zero odds is off, sticky choice or not")
	}
	if transitWarRoll(decider, target+1, 1e-12) {
		t.Error("near-zero odds against a new enemy must not fight")
	}
	// The other tables are separate: a transit choice is not an ROE choice.
	if roeNerveRoll(decider, target, 0) {
		t.Error("transit decisions leaked into the ROE table")
	}
}

// ── police ──────────────────────────────────────────────────────────────────

func TestFightsPolice(t *testing.T) {
	if defaultTuning().FightsPolice {
		t.Error("a pilot with default traits must leave the police alone")
	}
	cases := []struct {
		name                       string
		caution, aggression, lawls int
		want                       bool
	}{
		{"reckless violent outlaw", 100, 100, 100, true},
		{"halfway on all three", 50, 50, 50, true},
		{"reckless and violent but lawful", 100, 100, -50, false},
		{"reckless outlaw but pacifist", 100, -60, 100, false},
		{"violent outlaw but cautious", -50, 100, 100, false},
		{"barely an outlaw", 100, 100, 10, false}, // cbrt(0.1) ~ 0.46
		{"neutral on the law", 100, 100, 0, false},
		{"reckless alone", 100, 0, 0, false},
	}
	for _, c := range cases {
		tn := resolveTuning(map[string]int{
			TraitCaution: c.caution, TraitAggression: c.aggression, TraitLawfulness: c.lawls,
		})
		if tn.FightsPolice != c.want {
			t.Errorf("%s (caution %d, aggression %d, lawfulness %d): FightsPolice = %v, want %v",
				c.name, c.caution, c.aggression, c.lawls, tn.FightsPolice, c.want)
		}
	}
}

// A pacifist starts and joins no fight it could stay out of: war targets in transit, criminals on
// the grid, a friend's attacker. Everyone else may.
func TestTakesElectiveFights(t *testing.T) {
	if !takesElectiveFights(defaultTuning()) {
		t.Error("a pilot with default traits may take an elective fight")
	}
	pacifist := resolveTuning(map[string]int{TraitAggression: -100})
	if !pacifist.Pacifist || takesElectiveFights(pacifist) {
		t.Error("a pacifist must stay out of elective fights")
	}
	mild := resolveTuning(map[string]int{TraitAggression: -30}) // short of the pacifist floor
	if !takesElectiveFights(mild) {
		t.Error("a mild pilot short of pacifist may still take an elective fight")
	}
}

// ── travel waits for the fight ──────────────────────────────────────────────

func TestTravelHoldsForFight(t *testing.T) {
	now := time.Now()
	if travelHoldsForFight(false, time.Time{}, now) {
		t.Error("no fight, no hold")
	}
	if !travelHoldsForFight(true, now.Add(-10*time.Second), now) {
		t.Error("a fresh fight holds the trip")
	}
	if !travelHoldsForFight(true, time.Time{}, now) {
		t.Error("a fight with no start stamp still holds")
	}
	if travelHoldsForFight(true, now.Add(-(travelFightHoldMaxSec+1)*time.Second), now) {
		t.Error("a fight past the ceiling must release the trip")
	}
}

// ── target choice against pilots ────────────────────────────────────────────

func TestPilotDPSOn(t *testing.T) {
	now := int64(1_000_000)
	hits := []destiny.KillHit{
		{CharID: 7, Damage: 250, Time: now - 5},
		{CharID: 8, Damage: 1000, Time: now - combatAggroMemorySec - 1}, // stopped shooting
		{CharID: 0, Damage: 999, Time: now, IsNPC: true},
	}
	got := pilotDPSOn(hits, now)
	if len(got) != 1 || math.Abs(got[7]-250.0/combatAggroMemorySec) > 1e-9 {
		t.Fatalf("pilotDPSOn = %v, want only pilot 7 at %v", got, 250.0/combatAggroMemorySec)
	}
}

// A pilot that is hurting the bot outranks an idle one of the same health and range: the
// scorer only has an opinion when it has the target's output, which pilotDPSOn now supplies.
func TestCombatTargetScorePrefersShootingPilot(t *testing.T) {
	const botDPS, frac, dist, ehp = 200.0, 0.8, 10_000.0, 5_000.0
	shooting := combatTargetScore(botDPS, frac, dist, ehp, 150)
	idle := combatTargetScore(botDPS, frac, dist, ehp, 0)
	if !(shooting < idle) {
		t.Fatalf("shooting pilot scored %v, idle %v: lower must win for the shooter", shooting, idle)
	}
}
