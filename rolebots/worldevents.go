package rolebots

import (
	"context"

	evedb "gurimoon/db"
)

// World events for what the roster does where the core cannot see it.
//
// The core's world-event bus (db/worldevents.go) is how optional modules observe play, and the core
// emits only from its own paths. A bot's embodied kill goes through the core's NPC death path and is
// emitted there. This file emits the rest: kills resolved by the abstract tier, and ore in either
// tier (even an embodied miner puts ore in the hold from module code, mine_embodied.go). Emitting
// costs one atomic load when nothing listens.

// emitAbstractKills reports every dead rat in a resolved spawn, one event per rat, credited to the
// participant that did the most damage (the same one-pilot body count payNPCBounties applies).
func emitAbstractKills(parts []bountyShare, spawn []*ratThreat, sysID int64) {
	if len(parts) == 0 || !evedb.WorldEventsWanted() {
		return
	}
	top := parts[0]
	for _, p := range parts {
		if p.Weight > top.Weight {
			top = p
		}
	}
	now := clock.Now()
	for _, r := range spawn {
		if r == nil || r.EHP > 0 {
			continue
		}
		evedb.EmitWorldEvent(evedb.NPCKilledEvent{
			At:            now,
			KillerCharID:  top.CharID,
			NPCTypeID:     r.TypeID,
			NPCFactionID:  r.FactionID,
			SolarSystemID: sysID,
			Bounty:        r.Bounty,
			Abstract:      true,
		})
	}
}

// emitOreMined reports ore landing in a bot's hold.
func (m *Mod) emitOreMined(ctx context.Context, charID, typeID, units int64, volume float64, sysID int64, abstract bool) {
	if units <= 0 || !evedb.WorldEventsWanted() {
		return
	}
	evedb.EmitWorldEvent(evedb.OreMinedEvent{
		At:            clock.Now(),
		CharID:        charID,
		TypeID:        typeID,
		GroupID:       m.typeGroupOf(ctx, typeID),
		Units:         units,
		Volume:        volume,
		SolarSystemID: sysID,
		Abstract:      abstract,
	})
}
