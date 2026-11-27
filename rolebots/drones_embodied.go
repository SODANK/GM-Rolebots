package rolebots

import (
	"context"
	"time"

	"gurimoon/destiny"
	"gurimoon/modapi"
)

// Drones in space for a ship with nobody at the keyboard.
// A projection launches drones as a pilot does: combat drones onto the locked rat, mining
// drones onto the rock being worked. They fly on the core's drone manager (modapi.DroneHooks),
// so they take fire, die and pay bounty as a player's would.
// A discretionary warp calls the drones home and is refused until they are in the bay or
// droneRecallGrace passes, then stragglers are lost. A break-off does not wait.
// Combat drones also engage pilots. The core raises crimewatch and CONCORD for a drone's shot
// through its master (drones.go). Against a pilot the flight is called home once armour is
// gone (droneStageArmorFrac), ahead of the break-off line (shouldBreakOff).

const (
	// droneStageArmorFrac is the armour fraction under which a fight against a pilot calls the
	// combat flight home ahead of the break-off.
	droneStageArmorFrac = 0.15

	// droneRecallGrace is how long a discretionary warp waits for drones to come home before
	// leaving them. It is longer than the mining loop's 20 s belt-hop retry.
	droneRecallGrace = 30 * time.Second
	// droneLaunchRetry spaces launch attempts, so a bay the core refuses is not asked every tick.
	droneLaunchRetry = 10 * time.Second
)

// liveDrones returns this projection's drones of one group still in space, pruning the
// ones that are not (killed, scooped).
func (eb *embodiedBot) liveDrones(group int64) []int64 {
	h := modapi.Drones()
	if h.InSpace == nil {
		return nil
	}
	inSpace := map[int64]bool{}
	for _, id := range h.InSpace(eb.shipID) {
		inSpace[id] = true
	}
	eb.mu.Lock()
	defer eb.mu.Unlock()
	var out []int64
	for id, g := range eb.droneIDs {
		if !inSpace[id] {
			delete(eb.droneIDs, id)
			continue
		}
		if g == group {
			out = append(out, id)
		}
	}
	return out
}

// launchDrones tops this projection's flight of one group up to what the pilot may launch
// from the bay. It returns the drones of that group now in space.
func (m *Mod) launchDrones(eb *embodiedBot, group int64) []int64 {
	h := modapi.Drones()
	if h.Launch == nil || h.InSpace == nil || eb.ss == nil || eb.ent == nil || eb.ent.IsDestroyed() {
		return nil
	}
	have := eb.liveDrones(group)
	eb.mu.Lock()
	if time.Now().Before(eb.droneLaunchAfter) {
		eb.mu.Unlock()
		return have
	}
	eb.droneLaunchAfter = time.Now().Add(droneLaunchRetry)
	eb.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	room := m.droneLaunchCap(ctx, eb.charID) - int64(len(h.InSpace(eb.shipID)))
	if room <= 0 {
		return have
	}
	bay, err := m.shipBayDrones(ctx, eb.shipID)
	if err != nil {
		rlogf("rolebots: drones %s: bay: %v", eb.charName, err)
		return have
	}
	picks := planDroneLaunch(bay, group, room)
	if len(picks) == 0 {
		return have
	}
	want := make(map[int64]int64, len(picks))
	for _, p := range picks {
		want[p.TypeID] += p.N
	}
	ids := h.Launch(eb.ss, eb.ent, eb.charID, eb.corpID, eb.shipID, eb.systemID, want)
	if len(ids) == 0 {
		return have
	}
	eb.mu.Lock()
	if eb.droneIDs == nil {
		eb.droneIDs = map[int64]int64{}
	}
	for _, id := range ids {
		eb.droneIDs[id] = group
	}
	eb.droneRecallSince = time.Time{}
	eb.mu.Unlock()
	what := "combat"
	if group == grpMiningDrone {
		what = "mining"
	}
	m.logBot(eb.charID, logInfo, "drones", "launched %d %s drone(s)", len(ids), what)
	return append(have, ids...)
}

// droneEngage puts the combat drones on the gun target: launched if not out, re-commanded
// when the target or the flight changes. Not while the flight is staged home.
func (m *Mod) droneEngage(eb *embodiedBot, l *combatLoadout, tgt *destiny.Entity) {
	h := modapi.Drones()
	if h.Engage == nil || tgt == nil || tgt.IsDestroyed() || l.droneDPS() <= 0 || eb.dronesStaged() {
		return
	}
	ids := eb.liveDrones(grpCombatDrone)
	if int64(len(ids)) < l.combatDronesLaunchable() {
		ids = m.launchDrones(eb, grpCombatDrone)
	}
	if len(ids) == 0 {
		return
	}
	eb.mu.Lock()
	same := eb.droneTarget == tgt.ID && eb.droneEngagedN == len(ids)
	if !same {
		eb.droneTarget, eb.droneEngagedN = tgt.ID, len(ids)
	}
	eb.mu.Unlock()
	if same {
		return
	}
	if !tgt.IsPlayer {
		h.Engage(ids, eb.shipID, tgt.ID)
		return
	}
	// Sending the flight at a pilot is the aggression, ahead of the first drone hit (the core
	// flags a drone's shot only when it lands): a victim who fires back while the drones close is
	// defending. A departing ship sends its drones at nobody (crimewatch.go).
	if !eb.actOnPilot(func() {
		m.notePilotAggression(eb, tgt)
		h.Engage(ids, eb.shipID, tgt.ID)
	}) {
		eb.mu.Lock()
		eb.droneTarget, eb.droneEngagedN = 0, 0
		eb.mu.Unlock()
	}
}

// dronesStaged reports whether the combat flight has been called home for the rest of this
// engagement.
func (eb *embodiedBot) dronesStaged() bool {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	return eb.droneStaged
}

// stageDrones calls the combat flight home ahead of a break-off, once per engagement;
// clearFlee (the stand-down) lifts it.
func (m *Mod) stageDrones(eb *embodiedBot) {
	eb.mu.Lock()
	already := eb.droneStaged
	eb.droneStaged = true
	eb.mu.Unlock()
	if already {
		return
	}
	if len(eb.liveDrones(grpCombatDrone)) == 0 {
		return
	}
	m.dronesReturn(eb, grpCombatDrone)
	m.logBot(eb.charID, logInfo, "drones", "armor gone — recalling drones ahead of the break-off")
}

// dronesReturn calls one group's drones back to the bay (the manager scoops them on
// arrival). It does nothing when that group is not working, so the hold-fire branch can ask
// every tick without re-sending the order.
func (m *Mod) dronesReturn(eb *embodiedBot, group int64) {
	h := modapi.Drones()
	if h.ReturnBay == nil {
		return
	}
	eb.mu.Lock()
	working := false
	if group == grpCombatDrone {
		working = eb.droneTarget != 0
		eb.droneTarget, eb.droneEngagedN = 0, 0
	} else {
		working = eb.droneRock != 0
		eb.droneRock, eb.droneMiningN = 0, 0
	}
	eb.mu.Unlock()
	if !working {
		return
	}
	if ids := eb.liveDrones(group); len(ids) > 0 {
		h.ReturnBay(ids, eb.shipID)
	}
}

// droneKilledTarget credits the engagement's tally for a rat that died with the drones on it
// and the guns not landing the blow (landDamage never saw that kill). The core has already
// paid the bounty.
func (m *Mod) droneKilledTarget(eb *embodiedBot, ent *destiny.Entity) {
	if ent == nil || ent.IsPlayer || !ent.IsDestroyed() {
		return
	}
	// droneTarget stays set because the flight is still out and stand-down must call it home.
	eb.mu.Lock()
	ours := eb.droneTarget == ent.ID
	eb.mu.Unlock()
	if !ours {
		return
	}
	eb.npcKills.Add(1)
	m.logBot(eb.charID, logInfo, "combat", "drones finished off target entity %d", ent.ID)
}

// droneMine puts the mining drones on the rock the lasers are working.
func (m *Mod) droneMine(eb *embodiedBot, rockID uint32) {
	h := modapi.Drones()
	if h.Mine == nil || rockID == 0 || eb.ss == nil {
		return
	}
	// The core sends a mining drone straight home off ice or Mercoxit.
	if g := eb.ss.GetAsteroidGroupID(rockID); g == destiny.GroupIce || g == destiny.GroupMercoxit {
		return
	}
	// Topped up every cycle; launchDrones is rate-limited, so this is one bay read per cycle.
	ids := m.launchDrones(eb, grpMiningDrone)
	if len(ids) == 0 {
		return
	}
	eb.mu.Lock()
	same := eb.droneRock == rockID && eb.droneMiningN == len(ids)
	if !same {
		eb.droneRock, eb.droneMiningN = rockID, len(ids)
	}
	eb.mu.Unlock()
	if !same {
		h.Mine(ids, eb.shipID, rockID, true)
	}
}

// dronesClearForWarp is the drone half of leaving a grid, asked by startEmbodyWarpAs first.
// True means go. A discretionary warp calls the drones home and is refused until they are in
// the bay or the grace has run out, then leaves the stragglers. A mandatory warp leaves them
// at once.
func (m *Mod) dronesClearForWarp(eb *embodiedBot, class warpClass) bool {
	h := modapi.Drones()
	if h.InSpace == nil {
		return true
	}
	out := h.InSpace(eb.shipID)
	if len(out) == 0 {
		eb.mu.Lock()
		eb.droneRecallSince = time.Time{}
		eb.mu.Unlock()
		return true
	}
	eb.mu.Lock()
	fleeing := eb.fleeing
	since := eb.droneRecallSince
	if since.IsZero() {
		eb.droneRecallSince = time.Now()
	}
	eb.mu.Unlock()

	if class == warpMandatory || fleeing {
		m.loseDrones(eb, "left behind in the break-off")
		return true
	}
	if since.IsZero() {
		if h.ReturnBay != nil {
			h.ReturnBay(out, eb.shipID)
		}
		m.logBot(eb.charID, logInfo, "drones", "recalling %d drone(s) before warping", len(out))
		return false
	}
	if time.Since(since) < droneRecallGrace {
		return false
	}
	m.loseDrones(eb, "did not make it back before the warp")
	return true
}

// loseDrones abandons every drone this projection has out.
func (m *Mod) loseDrones(eb *embodiedBot, why string) {
	h := modapi.Drones()
	if h.Lose == nil {
		return
	}
	n := h.Lose(eb.shipID)
	eb.mu.Lock()
	eb.droneIDs = nil
	eb.droneTarget, eb.droneEngagedN = 0, 0
	eb.droneRock, eb.droneMiningN = 0, 0
	eb.droneRecallSince = time.Time{}
	eb.droneStaged = false
	eb.mu.Unlock()
	if n > 0 {
		m.logBot(eb.charID, logInfo, "drones", "lost %d drone(s) — %s", n, why)
	}
}

// recallDronesInstant puts every drone this projection has out straight back into the bay,
// for a projection leaving space for the abstract tier.
func (m *Mod) recallDronesInstant(eb *embodiedBot) {
	h := modapi.Drones()
	if h.Recall == nil {
		return
	}
	h.Recall(eb.shipID)
	eb.mu.Lock()
	eb.droneIDs = nil
	eb.droneTarget, eb.droneEngagedN = 0, 0
	eb.droneRock, eb.droneMiningN = 0, 0
	eb.droneRecallSince = time.Time{}
	eb.droneStaged = false
	eb.mu.Unlock()
}
