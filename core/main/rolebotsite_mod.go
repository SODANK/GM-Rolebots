package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/npc"
)

// The open-site seam, package-main half (modapi/sitebridge_mod.go has the other).
// Thin closures over opensites.go's registry and record, plus a room's rats built
// with no client. Every closure reads its globals at call time, because init()
// runs before main() assigns them. Overlay build only.

func init() {
	modapi.InstallSites(modapi.SiteHooks{
		List:         modSiteList,
		Get:          modSiteGet,
		MarkKilled:   modSiteMarkKilled,
		MarkWave:     modSiteMarkWave,
		RoomFrame:    modSiteRoomFrame,
		BuildRoom:    modSiteBuildRoom,
		LiveRoomNPCs: modSiteLiveRoomNPCs,
		PlayerInRoom: modSitePlayerInRoom,
		PilotsAt:     modSitePilotsAt,
	})
}

func modSiteView(s *openSite) modapi.SiteLive {
	v := modapi.SiteLive{
		SiteID: s.row.SiteID, SpawnerID: s.row.SpawnerID, DungeonVID: s.row.DungeonVID,
		SolarSystemID: s.row.SolarSystemID, X: s.row.X, Y: s.row.Y, Z: s.row.Z,
		BallID: s.ballID, FirstRoomID: s.firstRoomID,
		Killed:   map[int64]bool{},
		Released: map[int64]map[int64]bool{},
	}
	s.mu.Lock()
	v.ClearedAt = s.clearedAt
	v.RoomOffsets = make(map[int64]destiny.Vec3, len(s.roomOffsets))
	for id, off := range s.roomOffsets {
		v.RoomOffsets[id] = off
	}
	s.mu.Unlock()
	copyDunSiteState(s.state, v.Killed, v.Released)
	if globalSysMgr != nil {
		v.Loaded = globalSysMgr.Get(s.row.SolarSystemID) != nil
	}
	return v
}

func modSiteList() []modapi.SiteLive {
	var out []modapi.SiteLive
	for _, s := range openSitesSnapshot() {
		if s.isGone() {
			continue
		}
		out = append(out, modSiteView(s))
	}
	return out
}

func modSiteGet(siteID int64) (modapi.SiteLive, bool) {
	if mid, ok := modapi.SiteMissionID(siteID); ok {
		return modMissionSiteGet(mid)
	}
	s := openSiteByID(siteID)
	if s == nil || s.isGone() {
		return modapi.SiteLive{}, false
	}
	return modSiteView(s), true
}

// modSiteMarkMu makes the module's marks first-wins: the test and the mark are one
// step, so of two runners reporting one rat only one hears true and is paid.
var modSiteMarkMu sync.Mutex

func modSiteMarkKilled(siteID, objectID int64) bool {
	if mid, ok := modapi.SiteMissionID(siteID); ok {
		return modMissionMarkKilled(mid, objectID)
	}
	s := openSiteByID(siteID)
	if s == nil || s.isGone() || s.state == nil {
		return false
	}
	modSiteMarkMu.Lock()
	if s.state.IsKilled(objectID) {
		modSiteMarkMu.Unlock()
		return false
	}
	s.state.MarkKilled(objectID)
	modSiteMarkMu.Unlock()
	go reevaluateOpenSite(siteID)
	return true
}

func modSiteMarkWave(siteID, roomID, wave int64) bool {
	if mid, ok := modapi.SiteMissionID(siteID); ok {
		return modMissionMarkWave(mid, roomID, wave)
	}
	s := openSiteByID(siteID)
	if s == nil || s.isGone() || s.state == nil {
		return false
	}
	modSiteMarkMu.Lock()
	if s.state.WaveReleased(roomID, wave) {
		modSiteMarkMu.Unlock()
		return false
	}
	s.state.MarkWaveReleased(roomID, wave)
	modSiteMarkMu.Unlock()
	go reevaluateOpenSite(siteID)
	return true
}

func modSiteRoomFrame(siteID, roomID int64, computed, beaconLocal destiny.Vec3, hasBeacon bool) (destiny.Vec3, bool) {
	if mid, ok := modapi.SiteMissionID(siteID); ok {
		p := missionPocketFor(mid)
		if p == nil {
			return destiny.Vec3{}, false
		}
		return p.roomFrame(roomID, computed, beaconLocal, hasBeacon), true
	}
	s := openSiteByID(siteID)
	if s == nil || s.isGone() {
		return destiny.Vec3{}, false
	}
	return s.roomFrame(roomID, computed, beaconLocal, hasBeacon), true
}

func modSiteLiveRoomNPCs(siteID, roomID int64) []uint32 {
	if globalNPCMgr == nil {
		return nil
	}
	if mid, ok := modapi.SiteMissionID(siteID); ok {
		return modMissionLiveRoomNPCs(mid, roomID)
	}
	var out []uint32
	for _, eid := range globalNPCMgr.OpenSiteEntities(siteID) {
		if roomID != 0 && modSiteRoomOf(eid) != roomID {
			continue
		}
		out = append(out, eid)
	}
	return out
}

// modSiteRoomOf is the room a site NPC was spawned into, off the slim the
// builders stamp (applyDunSlimFields).
func modSiteRoomOf(eid uint32) int64 {
	s := openSiteByID(globalNPCMgr.GetOpenSiteID(eid))
	if s == nil || globalSysMgr == nil {
		return 0
	}
	ss := globalSysMgr.Get(s.row.SolarSystemID)
	if ss == nil {
		return 0
	}
	e := ss.GetEntity(eid)
	if e == nil {
		return 0
	}
	return e.Slim.DunRoomID
}

func modSitePlayerInRoom(siteID, roomID int64) bool {
	if mid, ok := modapi.SiteMissionID(siteID); ok {
		return len(modMissionPilots(mid, roomID)) > 0
	}
	s := openSiteByID(siteID)
	if s == nil || globalSysMgr == nil {
		return false
	}
	ss := globalSysMgr.Get(s.row.SolarSystemID)
	if ss == nil {
		return false
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		ds, ok := p.PlayerState.(*destinyConn)
		if !ok || ds == nil || p.Conn == nil {
			continue // a projection has no connection and fires nothing
		}
		if ds.dunOpenSiteID == siteID && (roomID == 0 || ds.dunRoomID == roomID) {
			return true
		}
	}
	return false
}

// modSitePilotsAt is every connected pilot holding any room of the site.
func modSitePilotsAt(siteID int64) []int64 {
	if mid, ok := modapi.SiteMissionID(siteID); ok {
		return modMissionPilots(mid, 0)
	}
	s := openSiteByID(siteID)
	if s == nil || globalSysMgr == nil {
		return nil
	}
	ss := globalSysMgr.Get(s.row.SolarSystemID)
	if ss == nil {
		return nil
	}
	var out []int64
	for _, p := range ss.GetAllPlayersInSystem() {
		ds, ok := p.PlayerState.(*destinyConn)
		if !ok || ds == nil || p.Conn == nil {
			continue
		}
		if ds.dunOpenSiteID == siteID && ds.charID != 0 {
			out = append(out, ds.charID)
		}
	}
	return out
}

// modSiteBuildRoom builds one room and wave group of a site with no connection. It
// spawns system-level NPCs tagged with the site, in the site's shared room frame;
// scenery is skipped. An NPC already standing for an object is adopted, not
// doubled. Held under the site's spawnMu like the player path.
func modSiteBuildRoom(siteID, roomID, wave int64) ([]uint32, error) {
	if mid, ok := modapi.SiteMissionID(siteID); ok {
		return modMissionBuildRoom(mid, roomID, wave)
	}
	s := openSiteByID(siteID)
	if s == nil || s.isGone() {
		return nil, fmt.Errorf("site %d is not live", siteID)
	}
	if globalNPCMgr == nil {
		return nil, fmt.Errorf("site %d: core not up", siteID)
	}
	npcMgr := globalNPCMgr
	return buildDunRoomHeadless(dunBuildTarget{
		label:   fmt.Sprintf("[opensite] site %d (%s)", siteID, s.name),
		sysID:   s.row.SolarSystemID,
		anchor:  destiny.Vec3{X: s.row.X, Y: s.row.Y, Z: s.row.Z},
		state:   s.state,
		spawnMu: &s.spawnMu,
		frame:   s.roomFrame,
		adopt:   func(objectID int64) uint32 { return npcMgr.FindEntityByOpenSiteObject(siteID, objectID) },
		tag:     func(eid uint32) { npcMgr.SetOpenSiteID(eid, siteID) },
	}, roomID, wave)
}

// dunBuildTarget is what buildDunRoomHeadless needs of the dungeon it builds into:
// an open site (modSiteBuildRoom) or a mission pocket (modMissionBuildRoom).
type dunBuildTarget struct {
	label   string       // log prefix
	sysID   int64        // the system the room is in
	anchor  destiny.Vec3 // the point room frames fall back to (the beacon)
	state   *dunSiteState
	spawnMu *sync.Mutex
	// frame is the room-frame rule: the recorded offset, else computed (first visitor).
	frame func(roomID int64, computed, beaconLocal destiny.Vec3, hasBeacon bool) destiny.Vec3
	// adopt returns the NPC already standing for an object, or 0.
	adopt func(objectID int64) uint32
	// tag marks a new NPC as the dungeon's, so its death reaches the dungeon's record.
	tag func(eid uint32)
	// missionID goes on the npcDungeonEntities row (0 for an open site).
	missionID int64
}

// buildDunRoomHeadless builds one room and wave group of a dungeon with no connection.
// It skips killed objects, unreleased waves and scenery, and adopts NPCs that already
// stand. The system must be loaded.
func buildDunRoomHeadless(t dunBuildTarget, roomID, wave int64) ([]uint32, error) {
	if globalSysMgr == nil || globalNPCMgr == nil || globalDB == nil {
		return nil, fmt.Errorf("%s: core not up", t.label)
	}
	ss := globalSysMgr.Get(t.sysID)
	if ss == nil {
		return nil, fmt.Errorf("%s: system %d is not loaded", t.label, t.sysID)
	}
	sqldb := globalDB
	npcMgr := globalNPCMgr
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	objs, err := evedb.GetRoomObjects(ctx, sqldb, roomID)
	if err != nil {
		return nil, err
	}
	var beaconLocal destiny.Vec3
	hasBeacon := false
	if bx, by, bz, ok, berr := evedb.GetRoomBeacon(ctx, sqldb, roomID); berr == nil && ok {
		beaconLocal = destiny.Vec3{X: bx, Y: by, Z: bz}
		hasBeacon = true
	}
	// Room 1 is on the beacon. A deeper room has its frame set through RoomFrame
	// before the build; with none yet it is placed on the anchor itself.
	offset := t.frame(roomID, t.anchor.Sub(beaconLocal), beaconLocal, hasBeacon)

	var dunFactionID, dunCorpID int64
	_ = sqldb.QueryRowContext(ctx,
		`SELECT d.factionID FROM dunRooms r JOIN dunDungeons d ON d.dungeonVID = r.dungeonVID WHERE r.roomID = ?`,
		roomID).Scan(&dunFactionID)
	if dunFactionID > 0 {
		dunCorpID = evedb.GetFactionCorpID(ctx, sqldb, dunFactionID)
	}

	t.spawnMu.Lock()
	defer t.spawnMu.Unlock()

	var out []uint32
	built := 0
	for _, o := range objs {
		if t.state.IsKilled(o.ObjectID) {
			continue
		}
		objWave := int64(0)
		if o.SpawnWave.Valid && o.SpawnWave.Int64 > 0 {
			objWave = o.SpawnWave.Int64
		}
		if wave == 0 {
			// Initial set: wave 0 plus the waves already out, as a human arriving would see.
			if objWave > 0 && !t.state.WaveReleased(roomID, objWave) {
				continue
			}
		} else if objWave != wave {
			continue
		}
		if eid := t.adopt(o.ObjectID); eid != 0 {
			out = append(out, eid)
			continue
		}
		var groupID, categoryID int64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT groupID, categoryID FROM invTypes JOIN invGroups USING(groupID) WHERE typeID = ?`,
			o.TypeID).Scan(&groupID, &categoryID); err != nil {
			continue
		}
		isStructure := npc.IsDestructibleStructure(groupID)
		if categoryID != 6 && categoryID != 11 && !isStructure {
			continue // scenery: per-connection, not ours to build
		}
		stats, statsErr := evedb.GetNPCStats(ctx, sqldb, o.TypeID)
		if statsErr != nil {
			continue
		}
		var npcHP *npc.NPCHPInit
		ehp, ehpErr := evedb.GetEntityHPByType(ctx, sqldb, o.TypeID)
		if ehpErr == nil {
			npcHP = &npc.NPCHPInit{
				ShieldHP: ehp.ShieldHP, ArmorHP: ehp.ArmorHP,
				StructureHP: ehp.StructureHP, ShieldTau: ehp.ShieldTau,
				ShieldRes: ehp.ShieldRes, ArmorRes: ehp.ArmorRes, HullRes: ehp.HullRes,
			}
			scaleDunHP(npcHP, o)
		}
		switch {
		case stats.MaxVelocity > 0:
		case isStructure && npcHP != nil && npcHP.StructureHP > 0:
		default:
			continue
		}
		weaponProfile, _ := evedb.GetNPCWeaponProfile(ctx, sqldb, o.TypeID)
		stance := npc.ParseStance(o.Stance.String)

		objFactionID, objCorpID := dunFactionID, dunCorpID
		if npc.IsInertStructure(groupID) {
			objFactionID, objCorpID = 0, npc.OwnerNone
		}
		if o.NPCFactionID.Valid && o.NPCFactionID.Int64 > 0 {
			objFactionID = o.NPCFactionID.Int64
			objCorpID = evedb.GetFactionCorpID(ctx, sqldb, o.NPCFactionID.Int64)
		}
		npcBounty := evedb.GetNPCBounty(ctx, sqldb, o.TypeID)

		spawnPos := destiny.Vec3{X: o.X + offset.X, Y: o.Y + offset.Y, Z: o.Z + offset.Z}
		spawnRadius := stats.Radius
		if isStructure && o.Radius.Valid && o.Radius.Float64 > 0 {
			spawnRadius = o.Radius.Float64
		}
		entity := npcMgr.SpawnNPC(ss, o.TypeID, dunBallName(o, stats.TypeName),
			spawnPos, stats.Mass, stats.Agility, stats.MaxVelocity, spawnRadius,
			stance, npcHP, weaponProfile, objCorpID, false, npcBounty, objFactionID, true)
		if entity == nil {
			continue
		}
		npc.SetGroupCategory(entity, stats.GroupID, stats.CategoryID)
		entity.SetInvulnerable(o.Invulnerable)
		t.tag(entity.ID)
		npcMgr.SetDunObjectID(entity.ID, o.ObjectID)
		applyDunSlimFields(entity, o)
		if o.AggressionRange.Valid {
			npcMgr.SetAggrRange(entity.ID, o.AggressionRange.Float64)
		}
		if o.LeashObjectID.Valid && o.LeashObjectID.Int64 > 0 {
			for _, lo := range objs {
				if lo.ObjectID != o.LeashObjectID.Int64 {
					continue
				}
				anchor := destiny.Vec3{X: lo.X + offset.X, Y: lo.Y + offset.Y, Z: lo.Z + offset.Z}
				orbitDist := 5000.0
				if o.OrbitDistance.Valid && o.OrbitDistance.Float64 > 0 {
					orbitDist = o.OrbitDistance.Float64
				}
				npcMgr.SetLeash(entity.ID, anchor, orbitDist)
				break
			}
		}
		sysID, missionID := t.sysID, t.missionID
		go func(eid uint32, tid int64, tn string, pos destiny.Vec3,
			fid, cid int64, bounty float64, gid, catid, dobjID, drmID int64) {
			dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer dbCancel()
			evedb.InsertDungeonNPC(dbCtx, sqldb, eid, tid, tn, sysID,
				pos.X, pos.Y, pos.Z, fid, cid, bounty, gid, catid, dobjID, drmID, missionID)
		}(entity.ID, o.TypeID, stats.TypeName, spawnPos,
			objFactionID, objCorpID, npcBounty, stats.GroupID, stats.CategoryID,
			o.ObjectID, o.RoomID)
		out = append(out, entity.ID)
		built++
	}
	if built > 0 {
		log.Printf("%s room %d wave %d: %d NPC(s) built headless in sys=%d at (%.0f,%.0f,%.0f)",
			t.label, roomID, wave, built, t.sysID, offset.X, offset.Y, offset.Z)
	}
	return out, nil
}
