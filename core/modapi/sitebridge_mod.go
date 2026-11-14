package modapi

import "gurimoon/destiny"

// The open-site seam, for a module whose pilots run the dungeons the core seeds
// (cmd/gurimoon/opensites.go). Package main installs thin closures over the
// unexported site registry from cmd/gurimoon/rolebotsite_mod.go.
//
// A kill mission's pocket is served under a negative site ID (MissionSiteID) and
// never listed.
//
// The trigger chain is not here. The core evaluates triggers per connection; the
// module replays the rule (rolebots/sitewave.go) and asks the core only to mark a
// wave released and to spawn it.

// SiteLive is one live open site as the module sees it.
type SiteLive struct {
	SiteID, SpawnerID, DungeonVID, SolarSystemID int64
	X, Y, Z                                      float64
	BallID                                       uint32
	FirstRoomID                                  int64
	ClearedAt                                    int64
	// Loaded is true when the system is live in the SystemManager, so the site's rats
	// are real balls.
	Loaded bool
	// Killed and Released are the core's site record: dead objectIDs and released
	// waves (roomID to wave). Read fresh; never cache.
	Killed   map[int64]bool
	Released map[int64]map[int64]bool
	// RoomOffsets are the frames deeper rooms were laid out in by their first visitor
	// (roomID to offset). Room 1 is on the beacon.
	RoomOffsets map[int64]destiny.Vec3
	// MissionID is set when this is a mission pocket (SiteID < 0, see MissionSiteID),
	// 0 for an open site. A mission pocket has no beacon ball (BallID 0) and is never
	// listed: only its owner runs it.
	MissionID int64
}

// MissionSiteID is the site ID a mission pocket is served under: the missionID
// negated, so it can never collide with an open site's dunOpenSites.siteID.
func MissionSiteID(missionID int64) int64 { return -missionID }

// SiteMissionID reverses MissionSiteID: the missionID behind a site ID, ok=false
// for an open site.
func SiteMissionID(siteID int64) (int64, bool) {
	if siteID < 0 {
		return -siteID, true
	}
	return 0, false
}

// SiteHooks are the closures package main installs.
type SiteHooks struct {
	// List is every live site (not despawned), in siteID order.
	List func() []SiteLive
	// Get is one site, or ok=false when it is not live.
	Get func(siteID int64) (SiteLive, bool)
	// MarkKilled records a dungeon object dead and re-runs the clear rule. True only
	// for the first mark, so two runners reporting one rat learn which of them
	// killed it.
	MarkKilled func(siteID, objectID int64) bool
	// MarkWave records a wave group released and re-runs the clear rule; a wave
	// released into a cleared site reopens it. True only for the first release.
	MarkWave func(siteID, roomID, wave int64) bool
	// RoomFrame returns the offset a room spawns at, recording computed as the frame
	// when the room has none yet (the first visitor's rule).
	RoomFrame func(siteID, roomID int64, computed, beaconLocal destiny.Vec3, hasBeacon bool) (destiny.Vec3, bool)
	// BuildRoom spawns a room's NPCs without a connection (wave 0 with wave = 0, else
	// that wave group only), adopting existing ones and skipping killed objects and
	// unreleased waves. Returns the entity IDs. The system must be loaded.
	BuildRoom func(siteID, roomID, wave int64) ([]uint32, error)
	// LiveRoomNPCs is the site's NPCs still alive in one room.
	LiveRoomNPCs func(siteID, roomID int64) []uint32
	// PlayerInRoom reports whether a connected pilot holds that room. Their connection
	// fires the core triggers, so the module must not.
	PlayerInRoom func(siteID, roomID int64) bool
	// PilotsAt returns the connected pilots inside the site, any room. Optional, and
	// CanRunSites does not need it.
	PilotsAt func(siteID int64) []int64
}

var siteHooks SiteHooks

// InstallSites publishes the core's open-site entry points. Package main calls it
// once from init(); a later call overwrites.
func InstallSites(h SiteHooks) { siteHooks = h }

// Sites returns the installed seam. Every field may be nil on a core without the
// overlay; CanRunSites reports whether it is set.
func Sites() SiteHooks { return siteHooks }

// CanRunSites reports whether this core carries the seam.
func CanRunSites() bool {
	h := siteHooks
	return h.List != nil && h.Get != nil && h.MarkKilled != nil && h.MarkWave != nil &&
		h.RoomFrame != nil && h.BuildRoom != nil && h.LiveRoomNPCs != nil && h.PlayerInRoom != nil
}
