package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync/atomic"

	evedb "gurimoon/db"
)

// Keep or give: what a hunter does with its haul. Once per haul, when the pilot
// turns for a station (combatStartSale), it either sells on the market for
// itself or carries the haul to its corporation's office hangar for merit
// (corpmerit.go). Giving usually takes both generosity (the low pole of Greed)
// and devotion (the low pole of Loyalty, whose high pole is mercenary).
//
// Only a corporation with a real office can be given anything
// (corpDepositTarget), so NPC corporation members always sell. A raider's fence
// is not offered the choice: it sells the whole hold, with no ledger to tell the
// raid's haul from its own ammunition.

const (
	settingLootDonateScale = "rolebots.loot.donateScalePct"
	settingLootMeritPerM   = "rolebots.loot.meritPerMISK"
	settingLootMeritCap    = "rolebots.loot.meritCap"

	defaultLootDonateScalePct = 100
	defaultLootMeritPerM      = 10
	defaultLootMeritCap       = 50

	// lootDecisionTTL is how long the keep-or-donate call on a haul stands.
	// Long enough to cover the trip; short enough that a haul abandoned
	// half-way gets a fresh look.
	lootDecisionTTL = 6 * 3600
	// lootDonateMax is the ceiling on the chance, however saintly the pilot.
	lootDonateMax = 0.85
)

var (
	lootDonateScalePct atomic.Int64
	lootMeritPerM      atomic.Int64
	lootMeritCap       atomic.Int64
)

func init() {
	lootDonateScalePct.Store(defaultLootDonateScalePct)
	lootMeritPerM.Store(defaultLootMeritPerM)
	lootMeritCap.Store(defaultLootMeritCap)
}

func (m *Mod) loadLootDonateSettings(ctx context.Context) {
	for _, s := range []struct {
		key      string
		val      *atomic.Int64
		min, max int64
	}{
		{settingLootDonateScale, &lootDonateScalePct, 0, 300},
		{settingLootMeritPerM, &lootMeritPerM, 0, 1000},
		{settingLootMeritCap, &lootMeritCap, 0, 10000},
	} {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, s.key)
		if err != nil || !ok {
			continue
		}
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= s.min && n <= s.max {
			s.val.Store(n)
		}
	}
}

// lootDonateChance is the chance a pilot gives its haul to its corporation,
// from its raw Greed and Loyalty (each -1..+1). Pure. Generous only or devoted
// only is about 0.16, both about 0.74, neither about 0.04, and a mercenary is
// scaled toward nothing.
func lootDonateChance(greed, loyalty float64) float64 {
	gen, dev, merc := posPole(-greed), posPole(-loyalty), posPole(loyalty)
	p := (0.04 + 0.5*gen*dev + 0.12*dev + 0.08*gen) * (1 - 0.85*merc)
	return math.Max(0, math.Min(lootDonateMax, p))
}

// lootDonateDecision makes (or recalls) the keep-or-donate call for the haul
// aboard. It answers the corporation office to take it to, or zeros to keep
// and sell. The call is remembered on the pilot (CombatMemory) so a re-plan
// does not re-roll it.
func (m *Mod) lootDonateDecision(ctx context.Context, b *botRun) (corpID, stationID, officeID int64) {
	st := b.State
	now := clock.Unix()
	if st.Data.LootDecidedAt != 0 && now-st.Data.LootDecidedAt < lootDecisionTTL {
		if st.Data.LootDonateOffice == 0 {
			return 0, 0, 0
		}
		return b.CorpID, st.Data.LootDonateStation, st.Data.LootDonateOffice
	}
	st.Data.LootDecidedAt = now
	st.Data.LootDonateOffice, st.Data.LootDonateStation = 0, 0
	corpID, stationID, officeID = m.corpDepositTarget(ctx, b.CorpID)
	if officeID == 0 {
		return 0, 0, 0
	}
	traits, err := loadTraitsBulk(ctx, m.db, []int64{b.CharID})
	if err != nil {
		return 0, 0, 0
	}
	t := traits[b.CharID]
	p := lootDonateChance(traitScale(t[TraitGreed]), traitScale(t[TraitLoyalty])) *
		float64(lootDonateScalePct.Load()) / 100
	if rand.Float64() >= p {
		return 0, 0, 0
	}
	st.Data.LootDonateOffice, st.Data.LootDonateStation = officeID, stationID
	return corpID, stationID, officeID
}

// combatStartDonation sends the hunter to its corporation's office with the
// haul when its keep-or-donate call says give. Reports whether it took the
// pilot. Called by combatStartSale after the fleet gate.
func (m *Mod) combatStartDonation(ctx context.Context, b *botRun, why string) (bool, error) {
	_, stationID, officeID := m.lootDonateDecision(ctx, b)
	if officeID == 0 {
		return false, nil
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
	if err != nil {
		b.State.Data.LootDonateOffice, b.State.Data.LootDonateStation = 0, 0
		return false, nil
	}
	st := b.State
	st.Activity = ActCombat
	st.Data.TargetStationID = stationID
	st.Data.TargetSystemID = loc.SolarSystemID
	m.logBot(b.CharID, logInfo, "combat", "%s — taking the haul to the corporation's office at %s",
		why, m.stationLabel(ctx, stationID))
	return true, m.beginTravel(ctx, b, loc.SolarSystemID, stationID, string(TaskCombatDonate))
}

// combatDonateStep hands the loot ledger's units over to the corporation's
// hangar at the office it flew to, books the merit, and goes back to work.
// Only ledgered units move: the hold may also carry ammunition the pilot
// bought, identical to some it looted, and only the looted rounds are given.
func (m *Mod) combatDonateStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	office, station := st.Data.LootDonateOffice, st.Data.LootDonateStation
	keep := func(reason string) (bool, error) {
		st.Data.LootDonateOffice, st.Data.LootDonateStation = 0, 0
		st.Data.LootDecidedAt = clock.Unix() // a keep, for this haul
		return false, m.combatStartSale(ctx, b, reason)
	}
	if office == 0 || station == 0 {
		return keep("nothing to give it to")
	}
	if b.StationID != station {
		loc, err := evedb.GetStationLocation(ctx, m.db, station)
		if err != nil {
			return keep("the corporation's office station is unreachable")
		}
		return false, m.beginTravel(ctx, b, loc.SolarSystemID, station, string(TaskCombatDonate))
	}
	// The corporation may have given the office up while the pilot flew.
	if now, _, err := evedb.GetCorpOfficeAt(ctx, m.db, b.CorpID, station); err != nil || now != office {
		return keep("the corporation has no office here any more")
	}
	ledger, err := m.lootLedger(ctx, b.CharID)
	if err != nil {
		return false, err
	}
	// Into the Spoils division when the corporation keeps one (corpdivisions.go,
	// corpspoils.go), the default hangar otherwise.
	into := int64(flagHangar)
	if f, ok := m.corpDivFlag(ctx, b.CorpID, purSpoils); ok {
		into = f
	}
	var value float64
	stacks := 0
	for _, r := range ledger {
		moved, err := m.moveCargoUnitsIntoFlag(ctx, b, r.TypeID, r.Units, b.CorpID, office, into)
		if err != nil {
			rlogf("rolebots: donate loot char=%d type=%d: %v", b.CharID, r.TypeID, err)
			continue
		}
		if moved <= 0 {
			continue
		}
		stacks++
		if px, err := m.estimatePriceFor(ctx, b, r.TypeID); err == nil && px > 0 {
			value += px * float64(moved)
		}
	}
	m.stackContainer(ctx, b.CorpID, office, into)
	m.clearLootLedger(ctx, b.CharID)
	if stacks > 0 {
		m.logBot(b.CharID, logInfo, "combat",
			"gave its haul to the corporation — %d stack(s), about %s, into the office hangar at %s",
			stacks, iskShort(value), m.stationLabel(ctx, station))
		if pts := lootDonationMerit(value); pts > 0 && meritOn() {
			if _, managed := managedCEO(b.CorpID); managed {
				m.addMerit(ctx, b.CorpID, b.CharID, pts,
					fmt.Sprintf("gave %s of loot to the corporation", iskShort(value)))
			}
		}
	}
	st.Data.LootDonateOffice, st.Data.LootDonateStation, st.Data.LootDecidedAt = 0, 0, 0
	st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return false, saveState(ctx, m.db, st)
}

// lootDonationMerit is the merit a donation of this value earns: the setting's
// points per million, capped. Pure apart from the settings.
func lootDonationMerit(value float64) float64 {
	if value <= 0 {
		return 0
	}
	return math.Min(float64(lootMeritCap.Load()), value/1e6*float64(lootMeritPerM.Load()))
}
