package rolebots

import (
	"context"
	"sync"

	"gurimoon/destiny"
	"gurimoon/modapi"
)

// CONCORD in space nobody is watching.
// The abstract tier resolves fights the core never sees. modapi.PvP().PoliceCrime is
// maybeTriggerConcordResponse with main's DB handle and NPC manager supplied
// (core/main/rolebotpvp_mod.go); it needs live entities and a solar system the abstract tier
// lacks. The module never force-loads a system: GetOrCreate registers it, and an empty
// registered system would make the next player's jump skip the real load (embody.go).
// So a crime is remembered and CONCORD is raised once the scene is real: at once when the
// system is loaded with a player in it and both parties have projections; held while a
// player is in the same constellation; dropped otherwise. The sec-status hit lands either
// way (combat_botvbot.go).

const (
	// crimeSceneHoldSec is how long a remembered crime stays warm. It is longer than the core's
	// concordLinger (90 s): linger is how long the fleet stays after its criminal is gone, this is
	// how long the crime is worth answering.
	crimeSceneHoldSec = 10 * 60
)

// crimeScene is one unwatched crime waiting for an audience.
type crimeScene struct {
	sysID      int64
	attackerID int64
	victimID   int64
	at         int64
}

var (
	crimeSceneMu sync.Mutex
	// Keyed by attacker: one pilot has one outstanding scene, matching the core's concordActive
	// ledger, so a pack robbing four victims queues one response.
	crimeScenes = map[int64]crimeScene{}
)

// notePoliceableCrime remembers a crime the abstract tier resolved, when it happened where
// CONCORD would answer and somebody might see it. A crime in lowsec or nullsec fails the
// security test, and one in empty hisec fails the constellation test.
func (m *Mod) notePoliceableCrime(ctx context.Context, sysID, attackerID, victimID int64, security float64) {
	if sysID == 0 || attackerID == 0 || victimID == 0 {
		return
	}
	// The floor concordDelayForSecurity applies: below it the core dispatches nobody.
	if security < concordMinSecurity {
		return
	}
	if !m.playerInConstellation(ctx, sysID) {
		return
	}
	crimeSceneMu.Lock()
	crimeScenes[attackerID] = crimeScene{
		sysID: sysID, attackerID: attackerID, victimID: victimID, at: clock.Unix(),
	}
	crimeSceneMu.Unlock()
	// It may already be answerable if a player is standing in the system.
	m.dispatchCrimeScenes()
}

// concordMinSecurity mirrors the core's concordMinSec: below it CONCORD does not respond.
// It is restated because the core's lives in package main.
const concordMinSecurity = 0.45

// playerInConstellation reports whether a real player is in the same constellation as
// sysID. It walks the loaded systems (the small set, since a system with no player is torn
// down), which makes the test exact.
func (m *Mod) playerInConstellation(ctx context.Context, sysID int64) bool {
	if m.systems == nil {
		return false
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return false
	}
	want := u.meta[sysID].ConstellationID
	if want == 0 {
		return false
	}
	for _, ss := range m.systems.All() {
		if ss == nil || u.meta[ss.ID].ConstellationID != want {
			continue
		}
		if systemHasRealPlayer(ss) {
			return true
		}
	}
	return false
}

// dispatchCrimeScenes raises CONCORD for every remembered crime whose scene has become real
// and forgets the ones that went cold. It is called on every embodiment reconcile, the
// module's signal that the world changed (a player arrived, bots gained projections).
func (m *Mod) dispatchCrimeScenes() {
	police := modapi.PvP().PoliceCrime
	if police == nil || m.systems == nil {
		return
	}
	now := clock.Unix()

	crimeSceneMu.Lock()
	pending := make([]crimeScene, 0, len(crimeScenes))
	for id, cs := range crimeScenes {
		if now-cs.at > crimeSceneHoldSec {
			delete(crimeScenes, id)
			continue
		}
		pending = append(pending, cs)
	}
	crimeSceneMu.Unlock()

	for _, cs := range pending {
		// The system must be loaded by somebody else: never GetOrCreate (see the header).
		ss := m.systems.Get(cs.sysID)
		if ss == nil || !systemHasRealPlayer(ss) {
			continue
		}
		attacker := projectionEntity(cs.attackerID)
		victim := projectionEntity(cs.victimID)
		if attacker == nil || victim == nil {
			// One of them is not projected onto this grid yet. Leave the scene standing for the next
			// reconcile pass.
			continue
		}
		// The scene is spent whether or not the core dispatches: maybeTriggerConcordResponse has its
		// own legality ladder and once-per-criminal ledger, so a declined scene would be declined again.
		crimeSceneMu.Lock()
		delete(crimeScenes, cs.attackerID)
		crimeSceneMu.Unlock()

		police(attacker, victim, ss)
	}
}

// projectionEntity returns a rolebot's live ball, or nil when it has none. A projection's
// entity is IsPlayer with a nil Conn (core/main/rolebotpvp_mod.go), so it passes CONCORD's
// target.IsPlayer test and counts toward GetAllPlayersInSystem.
func projectionEntity(charID int64) *destiny.Entity {
	eb := liveProjection(charID)
	if eb == nil {
		return nil
	}
	return eb.ent
}

// sweepCrimeScenes drops scenes nobody came to see. It runs on the module's cache sweep.
func sweepCrimeScenes() {
	now := clock.Unix()
	crimeSceneMu.Lock()
	for id, cs := range crimeScenes {
		if now-cs.at > crimeSceneHoldSec {
			delete(crimeScenes, id)
		}
	}
	crimeSceneMu.Unlock()
}

func init() {
	registerCharForget("crimeScenes", func(id int64) {
		crimeSceneMu.Lock()
		delete(crimeScenes, id)
		crimeSceneMu.Unlock()
	})
	registerSweep("crimeScenes", sweepCrimeScenes)
}