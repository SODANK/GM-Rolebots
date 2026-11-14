package rolebots

import (
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// The dungeon trigger chain, replayed without a client.
//
// fireDungeonTriggers (cmd/gurimoon/dungeon_triggers.go) needs a connection,
// and a bot has none, so this file reimplements its spawnWave rule as a pure
// function over the card's trigger rows and a progress record. Both tiers then
// release the same waves in the same order a human would. Only spawnWave
// actions are replayed, and DelayMs is ignored. A wave no trigger names is an
// auto wave: siteAutoWave lets the lowest unreleased wave out when a quiet
// room's pass released nothing.
// See docs/comment-notes-b17.md#site-trigger-rule.

// siteProgress is where one site stands: dead objects, released waves and spent
// onceOnly triggers. Killed and Released are read from the core's site state
// each step; Fired is the module's own, since the core keeps it per connection.
type siteProgress struct {
	Killed   map[int64]bool           // objectID to dead
	Released map[int64]map[int64]bool // roomID to wave to released
	Fired    map[int64]bool           // triggerID to spent (onceOnly)
}

func newSiteProgress() *siteProgress {
	return &siteProgress{
		Killed:   map[int64]bool{},
		Released: map[int64]map[int64]bool{},
		Fired:    map[int64]bool{},
	}
}

func (p *siteProgress) released(roomID, wave int64) bool {
	if wave <= 0 {
		return true // wave 0 spawns with the room
	}
	return p.Released[roomID][wave]
}

func (p *siteProgress) release(roomID, wave int64) {
	if wave <= 0 {
		return
	}
	if p.Released[roomID] == nil {
		p.Released[roomID] = map[int64]bool{}
	}
	p.Released[roomID][wave] = true
}

// siteReleases runs one event pass over a room's triggers and reports the waves
// it releases (at most one, by the first-unfired rule), marking onceOnly
// triggers spent. It does not mark the wave released: the caller does that once
// the core's site state has been told, so a failed bridge call leaves no phantom wave.
func siteReleases(room *siteRoom, p *siteProgress, event string, targetObjectID int64) []int64 {
	if room == nil || p == nil {
		return nil
	}
	var out []int64
	waveQueued := false
	for _, t := range room.Triggers {
		if t.Event != event {
			continue
		}
		if t.TargetObjectID.Valid && t.TargetObjectID.Int64 != 0 &&
			t.TargetObjectID.Int64 != targetObjectID {
			continue
		}
		if t.OnceOnly && p.Fired[t.TriggerID] {
			continue
		}
		if t.Action == "spawnWave" {
			if waveQueued {
				continue
			}
			waveQueued = true
		}
		if t.OnceOnly {
			p.Fired[t.TriggerID] = true
		}
		if t.Action == "spawnWave" && t.SpawnWave.Valid && t.SpawnWave.Int64 > 0 {
			out = append(out, t.SpawnWave.Int64)
		}
	}
	return out
}

// siteUnreleased keeps the waves of a release that are not out yet.
func siteUnreleased(room *siteRoom, p *siteProgress, waves []int64) []int64 {
	if room == nil || p == nil {
		return nil
	}
	var out []int64
	for _, w := range waves {
		if !p.released(room.RoomID, w) {
			out = append(out, w)
		}
	}
	return out
}

// siteAutoWave is the auto wave a quiet room lets out next, or 0. Call it only
// when the room has no live rat and the pass's own triggers released nothing.
func siteAutoWave(room *siteRoom, p *siteProgress) int64 {
	if room == nil || p == nil {
		return 0
	}
	waves := make([]int64, 0, len(room.Waves))
	for i := range room.Waves {
		waves = append(waves, room.Waves[i].Wave)
	}
	return evedb.NextAutoWave(waves, room.Triggers, func(w int64) bool { return p.released(room.RoomID, w) })
}

// waveReleasedBy is the trigger that lets a wave out, or nil for a wave no
// trigger names (an auto wave, see siteAutoWave).
func waveReleasedBy(room *siteRoom, wave int64) *evedb.TriggerRow {
	if room == nil || wave <= 0 {
		return nil
	}
	for i := range room.Triggers {
		t := &room.Triggers[i]
		if t.Action == "spawnWave" && t.SpawnWave.Valid && t.SpawnWave.Int64 == wave {
			return t
		}
	}
	return nil
}

// roomLiveRats is every rat of the room that is out (its wave released) and
// not dead, as fresh copies the duel may spend. Structures are not rats: they
// are shot after the room is quiet (siteabstract.go).
func roomLiveRats(room *siteRoom, p *siteProgress) []*ratThreat {
	var out []*ratThreat
	for i := range room.Waves {
		w := &room.Waves[i]
		if !p.released(room.RoomID, w.Wave) {
			continue
		}
		for _, r := range w.Rats {
			if r == nil || p.Killed[r.SiteObjectID] {
				continue
			}
			out = append(out, copyRat(r))
		}
	}
	return out
}

// roomLiveStructs is the room's shootable, loot-bearing structures still
// standing. Invulnerable ones are never listed.
func roomLiveStructs(room *siteRoom, p *siteProgress, all bool) []siteStruct {
	var out []siteStruct
	for i := range room.Waves {
		w := &room.Waves[i]
		if !p.released(room.RoomID, w.Wave) {
			continue
		}
		for _, s := range w.Structs {
			if s.Invulnerable || p.Killed[s.ObjectID] {
				continue
			}
			if !all && len(s.Loot) == 0 {
				continue
			}
			out = append(out, s)
		}
	}
	return out
}

// roomPendingWaves reports the waves of a room that are authored and not yet
// released. Auto waves count, since they always come out.
func roomPendingWaves(room *siteRoom, p *siteProgress) []int64 {
	var out []int64
	for i := range room.Waves {
		w := &room.Waves[i]
		if w.Wave <= 0 || p.released(room.RoomID, w.Wave) {
			continue
		}
		out = append(out, w.Wave)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// copyRat is a fresh combatant off a card rat, so each run spends its own EHP
// and Dealt and the card's template rats stay intact. SiteObjectID carries the
// dunObjects objectID; BeltRatID stays zero so the belt write-back
// (settleAbstractSpawn) never sees a site rat.
func copyRat(r *ratThreat) *ratThreat {
	if r == nil {
		return nil
	}
	c := *r
	c.Dealt = 0
	c.EHP = c.MaxEHP
	return &c
}

// ── the per-site trigger ledger ─────────────────────────────────────────────

// Fired triggers per live site. In memory only, like the core's own site state:
// a restart forgets both, and a site then comes back at full strength with
// every chain re-armed, which is consistent even if it is not persistent.
var (
	siteFiredMu sync.Mutex
	siteFired   = map[int64]map[int64]bool{} // siteID to triggerID to spent
)

// siteProgressFor assembles the progress record for one site: the core's kill
// and wave state (from the bridge snapshot) and this module's fired ledger.
func siteProgressFor(siteID int64, killed map[int64]bool, released map[int64]map[int64]bool) *siteProgress {
	p := newSiteProgress()
	for id := range killed {
		p.Killed[id] = true
	}
	for room, waves := range released {
		for w := range waves {
			p.release(room, w)
		}
	}
	siteFiredMu.Lock()
	for id := range siteFired[siteID] {
		p.Fired[id] = true
	}
	siteFiredMu.Unlock()
	return p
}

// noteSiteFired writes the fired ledger back after an event pass.
func noteSiteFired(siteID int64, p *siteProgress) {
	if p == nil || len(p.Fired) == 0 {
		return
	}
	siteFiredMu.Lock()
	if siteFired[siteID] == nil {
		siteFired[siteID] = map[int64]bool{}
	}
	for id := range p.Fired {
		siteFired[siteID][id] = true
	}
	siteFiredMu.Unlock()
}

// forgetSiteFired drops a site's ledger once the site is gone.
func forgetSiteFired(siteID int64) {
	siteFiredMu.Lock()
	delete(siteFired, siteID)
	siteFiredMu.Unlock()
}
