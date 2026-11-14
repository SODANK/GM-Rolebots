package rolebots

import (
	"context"

	evedb "gurimoon/db"
)

// A fleet member loots the field for itself. Every seat in a combat fleet is a real seat
// (guns, tackle, logistics, electronic warfare), and what a pilot kills it loots, into its
// own hold and its own ledger (rolebot_loot_hold), as a solo hunter does. When the fleet
// ends the haul is the pilot's to sell or give to its corporation (lootdonate.go).
//
// Unwatched kills go through lootAbstractKills like anybody's (the gang's combat tasks
// declare Loots). A watched kill leaves a can, noteBeltLoot marks it, and gangStep turns
// the mark into this task.

// gangSalvageStep empties one wreck the member made into its own hold, then
// sends it back to its seat through the planner. A full hold ends the looting,
// not the hunt: the haul rides along until the fleet stands down
// (combatStartSale's fleet gate).
func (m *Mod) gangSalvageStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	back := func() error {
		st.Data.HuntLootID, st.Data.HuntLootSysID, st.Data.HuntLootAt = 0, 0, 0
		st.Phase = string(TaskIdle)
		st.Data.LegEndsAt = 0
		return saveState(ctx, m.db, st)
	}
	if st.Data.HuntLootID == 0 || b.ShipID == 0 {
		return back()
	}
	if clock.Unix()-st.Data.HuntLootAt > lootCanAttemptSec {
		return back()
	}
	if b.SystemID != st.Data.HuntLootSysID {
		return m.beginTravel(ctx, b, st.Data.HuntLootSysID, 0, string(TaskGangSalvage))
	}
	free, err := m.freeHoldM3(ctx, b)
	if err != nil {
		return err
	}
	if free <= 0 {
		return back()
	}
	if !m.isEmbodied(b.CharID) {
		m.parkAtCelestialEntity(ctx, b.ShipID, st.Data.HuntLootID)
	}
	before, err := m.canContents(ctx, st.Data.HuntLootID)
	if err != nil {
		return err
	}
	taken, _, err := m.takeCanContents(ctx, b.CharID, b.ShipID, st.Data.HuntLootID, b.SystemID, free)
	if err != nil {
		return err
	}
	after, _ := m.canContents(ctx, st.Data.HuntLootID)
	for typeID, was := range before {
		if got := was.Units - after[typeID].Units; got > 0 {
			m.noteLootTaken(ctx, b.CharID, typeID, was.TypeName, got)
			if g != nil {
				m.noteGangHaul(ctx, g.GangID, b.CharID, haulLoot, typeID, was.TypeName, got, 0, 0)
			}
		}
	}
	if taken > 0 {
		m.logBot(b.CharID, logInfo, "gang", "looted its wreck — %.1f m³ into its own hold", taken)
	}
	empty, err := evedb.IsContainerEmpty(ctx, m.db, st.Data.HuntLootID)
	if err != nil || empty || taken <= 0 {
		return back()
	}
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(10)
	return saveState(ctx, m.db, st)
}
