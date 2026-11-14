package rolebots

import (
	"context"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/npc"
)

// Running an open site on a real grid.
//
// When a player has the system loaded the bot is a projection (a ball with the
// real combat guard on it, combat_embodied.go) and the site's rats are real
// balls built by the bridge (modapi/sitebridge_mod.go), tagged as a player's
// would be, so the core's death hook marks, pays and loots them. This file is
// the choreography: warp to the beacon, build room 1, let the combat loop fight
// (siteTargetsOnGrid), replay the trigger chain after each kill (siteOnBotKill),
// and when the room is quiet aim down the gate and build the next room.

// siteGateSpacing / siteLandingBuffer mirror the core's dungeonRoomSpacing and
// deadspaceLandingBuffer (main.go): the next room is 1.5 AU past the gate
// along its direction, anchored 2500 m ahead of the landing ship.
const (
	siteGateSpacing   = 1.5 * 149597870700.0
	siteLandingBuffer = 2500.0
	siteTickHold      = 6 * time.Second
)

// siteEmbodyTick drives one projection's run. True when the site owns the
// projection this tick (so the wander and the hold circle keep their hands
// off it); false hands the projection back to the ordinary tick.
func (m *Mod) siteEmbodyTick(eb *embodiedBot, now time.Time) bool {
	eb.mu.Lock()
	siteID := eb.siteID
	busy := eb.dead || eb.warping || eb.fighting || eb.fleeing || eb.mineCancel != nil || eb.approachID != 0
	nextAt := eb.nextActAt
	eb.mu.Unlock()
	if siteID == 0 {
		return false
	}
	if busy || now.Before(nextAt) {
		return true
	}
	if !SitesEnabled() {
		return true
	}
	site, ok := modapi.Sites().Get(siteID)
	if !ok || site.SolarSystemID != eb.systemID {
		eb.mu.Lock()
		eb.siteID, eb.siteArrived, eb.siteBuilt, eb.siteDone = 0, false, false, false
		eb.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	card, err := m.siteCardFor(ctx, site.DungeonVID)
	if err != nil || card == nil || len(card.Rooms) == 0 {
		return true
	}
	hold := func() {
		eb.mu.Lock()
		eb.nextActAt = now.Add(siteTickHold)
		eb.mu.Unlock()
	}

	// To the beacon first. ItemID is the beacon's ball, unique in its band, so the
	// arrival lane keys on it; the warp code reads only position and radius. A
	// mission pocket has no beacon ball and keys on its own lane ID.
	if !eb.siteArrived {
		laneID := int64(site.BallID)
		if site.MissionID != 0 {
			laneID = missionPocketLaneID(site.MissionID)
		}
		target := evedb.CelestialEntity{ItemID: laneID, TypeID: evedb.DungeonBeaconTypeID,
			GroupID: destiny.GroupBeacon, X: site.X, Y: site.Y, Z: site.Z, Radius: 0, Name: card.Name}
		if m.startEmbodyWarpAs(eb, target, true, warpMandatory) {
			eb.mu.Lock()
			eb.siteArrived, eb.siteBuilt, eb.siteRoomIdx = true, false, 0
			eb.siteRoomID = card.Rooms[0].RoomID
			eb.mu.Unlock()
			m.logBot(eb.charID, logInfo, "site", "warping to %s", card.Name)
		}
		return true
	}
	if eb.siteDone {
		return true
	}
	if eb.siteRoomIdx >= len(card.Rooms) {
		eb.mu.Lock()
		eb.siteDone = true
		eb.mu.Unlock()
		return true
	}
	room := &card.Rooms[eb.siteRoomIdx]

	// Landed in a room nothing has built yet: build it, then its roomEnter
	// pass. A room a player already fought is adopted, not doubled.
	if !eb.siteBuilt {
		if _, err := modapi.Sites().BuildRoom(siteID, room.RoomID, 0); err != nil {
			rlogf("rolebots: site %d build room %d: %v", siteID, room.RoomID, err)
			hold()
			return true
		}
		loot := map[int64]bool{}
		for _, w := range room.Waves {
			for _, s := range w.Structs {
				if len(s.Loot) > 0 && !s.Invulnerable {
					loot[s.ObjectID] = true
				}
			}
		}
		eb.mu.Lock()
		eb.siteBuilt = true
		eb.siteLoot = loot
		eb.mu.Unlock()
		m.siteFireEvent(eb, site, card, room, "roomEnter", 0)
		m.logBot(eb.charID, logInfo, "site", "in room %d of %d of %s", eb.siteRoomIdx+1, len(card.Rooms), card.Name)
		hold()
		return true
	}

	// Anything still standing that the combat loop wants is the combat
	// loop's; this tick only moves the projection on when the room is done.
	if len(m.siteTargetsOnGrid(eb)) > 0 || m.siteRoomRatsAlive(eb, siteID, room.RoomID, 0) {
		// Rats alive but not ours to shoot (a friendly-faction dungeon, or rats out of
		// the bubble): give the clear a chance to release, then wait.
		if len(m.siteTargetsOnGrid(eb)) == 0 {
			m.siteFireEvent(eb, site, card, room, "roomCleared", 0)
		}
		hold()
		return true
	}
	// Quiet: the clear may release a wave into it, and failing that the next
	// auto wave comes out on its own.
	if m.siteFireEvent(eb, site, card, room, "roomCleared", 0) || m.siteFireAutoWave(eb, site, card, room) {
		hold()
		return true
	}
	// The field next: this pilot's and its fleet's cans are emptied before the gate or the end.
	if m.siteSweepField(eb, now) {
		return true
	}

	// The gate, or the end.
	if room.Gate == nil || eb.siteRoomIdx+1 >= len(card.Rooms) {
		eb.mu.Lock()
		eb.siteDone = true
		eb.mu.Unlock()
		m.logBot(eb.charID, logInfo, "site", "%s is done", card.Name)
		return true
	}
	next := &card.Rooms[eb.siteRoomIdx+1]
	offset, ok := site.RoomOffsets[room.RoomID]
	if !ok {
		// Room 1 sits on the beacon; the core's roomFrame rule.
		offset = destiny.Vec3{X: site.X, Y: site.Y, Z: site.Z}
		if room.HasBeacon {
			offset = offset.Sub(room.BeaconLocal)
		}
	}
	landing := room.Gate.Local.Add(offset).Add(room.Gate.Dir.Scale(siteGateSpacing))
	anchor := siteNextAnchor(room, next, offset)
	// Record the frame BEFORE the warp so a player taking the same gate lands
	// in the room where this projection built it, and vice versa.
	modapi.Sites().RoomFrame(siteID, next.RoomID, anchor, next.BeaconLocal, next.HasBeacon)
	m.siteFireEvent(eb, site, card, room, "gateActivated", room.Gate.ObjectID)
	target := evedb.CelestialEntity{ItemID: -room.Gate.ObjectID, GroupID: destiny.GroupWarpGate,
		X: landing.X, Y: landing.Y, Z: landing.Z, Radius: 0, Name: card.Name + " gate"}
	if m.startEmbodyWarpAs(eb, target, true, warpMandatory) {
		eb.mu.Lock()
		eb.siteRoomIdx++
		eb.siteRoomID = next.RoomID
		eb.siteBuilt = false
		eb.siteLootCan, eb.siteLootSkip = 0, nil
		eb.mu.Unlock()
		m.logBot(eb.charID, logInfo, "site", "room %d of %s is done — taking the gate", eb.siteRoomIdx, card.Name)
	}
	return true
}

// The field sweep: a quiet room's cans are flown to and emptied before the
// projection takes the gate or calls the site done.
const (
	siteSweepRangeM   = 100_000.0 // cans further than this from the ship are not this room's
	siteLootReachM    = 2_400.0   // inside the 2500 m a can opens at
	siteLootApproachM = 1_000.0
	siteLootGiveUp    = 90 * time.Second
)

// siteSweepField flies the projection to the nearest can it or its fleet left in
// this room and empties it into the hold. True while the sweep owns the tick;
// false when nothing is left to take or the hold is full.
func (m *Mod) siteSweepField(eb *embodiedBot, now time.Time) bool {
	if eb.ent == nil || eb.ss == nil {
		return false
	}
	eb.mu.Lock()
	canID, since, busy, full := eb.siteLootCan, eb.siteLootSince, eb.siteLootBusy, eb.siteHoldFull
	eb.mu.Unlock()
	if busy {
		return true
	}
	if full {
		return false
	}
	if canID == 0 {
		if canID = m.siteNextCan(eb); canID == 0 {
			return false
		}
		since = now
		eb.mu.Lock()
		eb.siteLootCan, eb.siteLootSince = canID, now
		eb.mu.Unlock()
	}
	skip := func() {
		eb.mu.Lock()
		if eb.siteLootSkip == nil {
			eb.siteLootSkip = map[int64]bool{}
		}
		eb.siteLootSkip[canID] = true
		eb.siteLootCan = 0
		eb.mu.Unlock()
	}
	pos, ok := eb.ss.GetLootContainerPosition(uint32(canID))
	if !ok {
		skip() // taken by a gangmate, or despawned
		return true
	}
	if eb.ent.Pos().Sub(pos).Len() > siteLootReachM {
		if now.Sub(since) > siteLootGiveUp {
			m.logBot(eb.charID, logInfo, "site", "could not get to a can in time — leaving it")
			skip()
			return true
		}
		m.embodyApproach(eb, uint32(canID), siteLootApproachM, 0)
		return true
	}
	eb.mu.Lock()
	eb.siteLootBusy = true
	charID, shipID, sysID, gangID := eb.charID, eb.shipID, eb.systemID, eb.gangID
	eb.mu.Unlock()
	m.goSafeBot(charID, "site can", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		full := m.siteEmptyCan(ctx, charID, shipID, sysID, gangID, canID)
		eb.mu.Lock()
		eb.siteLootBusy = false
		eb.siteHoldFull = eb.siteHoldFull || full
		eb.mu.Unlock()
		skip()
	})
	return true
}

// siteNextCan is the nearest loot can on this projection's grid that it or a
// gangmate owns and that it has not already given up on: a rival's can is
// theft (cans.go NoteTheft), so a shared room is swept by owner.
func (m *Mod) siteNextCan(eb *embodiedBot) int64 {
	eb.mu.Lock()
	gangID, skipped := eb.gangID, eb.siteLootSkip
	eb.mu.Unlock()
	here := eb.ent.Pos()
	balls, slims, _ := eb.ss.GetLootContainers()
	best, bestD := int64(0), 0.0
	for i := range balls {
		if i >= len(slims) {
			break
		}
		slim := slims[i]
		if slim.TypeID != evedb.TypeCargoContainer || skipped[slim.ItemID] {
			continue
		}
		if slim.OwnerID != eb.charID && (gangID == 0 || m.gangOfCharCached(slim.OwnerID) != gangID) {
			continue
		}
		d := here.Sub(balls[i].Pos).Len()
		if d > siteSweepRangeM || (best != 0 && d >= bestD) {
			continue
		}
		best, bestD = slim.ItemID, d
	}
	return best
}

// siteEmptyCan moves what fits of one can into the hold and writes it to the
// loot ledger (and the fleet's haul). True when the hold has no room left.
func (m *Mod) siteEmptyCan(ctx context.Context, charID, shipID, sysID, gangID, canID int64) bool {
	free, err := m.holdFreeM3(ctx, charID, shipID)
	if err != nil {
		return false
	}
	if free <= 0 {
		return true
	}
	before, err := m.canContents(ctx, canID)
	if err != nil {
		return false
	}
	taken, _, err := m.takeCanContents(ctx, charID, shipID, canID, sysID, free)
	if err != nil {
		rlogf("rolebots: site can %d char=%d: %v", canID, charID, err)
		return false
	}
	after, _ := m.canContents(ctx, canID)
	for typeID, was := range before {
		if got := was.Units - after[typeID].Units; got > 0 {
			m.noteLootTaken(ctx, charID, typeID, was.TypeName, got)
			if gangID != 0 {
				m.noteGangHaul(ctx, gangID, charID, haulLoot, typeID, was.TypeName, got, 0, 0)
			}
		}
	}
	if taken > 0 {
		m.logBot(charID, logInfo, "site", "looted a can on the field — %.1f m³ into the hold", taken)
	}
	empty, err := evedb.IsContainerEmpty(ctx, m.db, canID)
	return err == nil && !empty
}

// resetSiteLocked points the projection at a new site (0 = none) and forgets
// where it stood in the old one. The caller holds eb.mu.
func (eb *embodiedBot) resetSiteLocked(siteID int64) {
	eb.siteID = siteID
	eb.siteArrived, eb.siteBuilt, eb.siteDone = false, false, false
	eb.siteRoomIdx, eb.siteRoomID, eb.siteLoot = 0, 0, nil
	eb.siteLootCan, eb.siteLootSkip, eb.siteHoldFull = 0, nil, false
}

// sitePlace is where a projection born mid-site stands: its room and a point
// beside that room's beacon.
type sitePlace struct {
	Pos     destiny.Vec3
	RoomIdx int
	RoomID  int64
}

// siteBirthSpreadM is how far from the room beacon a projection born on a site
// is put, so a pack is not born stacked: siteBirthSpreadMinM plus up to
// siteBirthSpreadM more, fixed per ship.
const (
	siteBirthSpreadMinM = 1_500.0
	siteBirthSpreadM    = 3_000.0
)

// sitePlacement is where a projection of a pilot the paper run has on a site
// is born: the room the run reached (the commander's, for a pack member, whose
// own state never advances), beside its beacon. ok=false when the pilot is not
// on a live site in this system, and the birth falls back to the ship row.
func (m *Mod) sitePlacement(ctx context.Context, w embodyWant) (sitePlace, bool) {
	if w.siteID == 0 || !SitesEnabled() || !modapi.CanRunSites() {
		return sitePlace{}, false
	}
	site, ok := modapi.Sites().Get(w.siteID)
	if !ok || site.SolarSystemID != w.systemID {
		return sitePlace{}, false
	}
	card, err := m.siteCardFor(ctx, site.DungeonVID)
	if err != nil || card == nil || len(card.Rooms) == 0 {
		return sitePlace{}, false
	}
	idx := int(w.siteRoomIdx)
	if w.gangID != 0 && w.gangLeader != 0 && w.gangLeader != w.charID {
		if lst, err := loadState(ctx, m.db, w.gangLeader); err == nil && lst != nil &&
			lst.Data.SiteID == w.siteID && int(lst.Data.SiteRoomIdx) > idx {
			idx = int(lst.Data.SiteRoomIdx)
		}
	}
	if idx >= len(card.Rooms) {
		idx = len(card.Rooms) - 1
	}
	pos, idx := siteRoomBeacon(site, card, idx)
	// A fixed direction per ship (parkJitter) at a fixed distance per ship.
	dist := siteBirthSpreadMinM + float64(uint64(w.shipID)%1000)/1000*siteBirthSpreadM
	pos = pos.Add(parkJitter(w.shipID).Scale(dist / parkJitterM))
	return sitePlace{Pos: pos, RoomIdx: idx, RoomID: card.Rooms[idx].RoomID}, true
}

// siteRoomBeacon is the world point of room idx's beacon (its frame origin when
// it has none), walking the gates from room 1 as siteEmbodyTick lays rooms out.
// A frame no visitor has recorded yet is recorded here (RoomFrame, first visitor
// wins), so a player taking the gate later lands in the same room. A room with
// no gate onward stops the walk; the answer is then the last room reached.
func siteRoomBeacon(site modapi.SiteLive, card *siteCard, idx int) (destiny.Vec3, int) {
	first := &card.Rooms[0]
	offset := destiny.Vec3{X: site.X, Y: site.Y, Z: site.Z}
	if first.HasBeacon {
		offset = offset.Sub(first.BeaconLocal)
	}
	if o, ok := site.RoomOffsets[first.RoomID]; ok {
		offset = o
	}
	reached := 0
	for i := 0; i < idx && i+1 < len(card.Rooms); i++ {
		room, next := &card.Rooms[i], &card.Rooms[i+1]
		if o, ok := site.RoomOffsets[next.RoomID]; ok {
			offset, reached = o, i+1
			continue
		}
		if room.Gate == nil {
			break
		}
		anchor := siteNextAnchor(room, next, offset)
		if frame := modapi.Sites().RoomFrame; frame != nil {
			if got, ok := frame(site.SiteID, next.RoomID, anchor, next.BeaconLocal, next.HasBeacon); ok {
				anchor = got
			}
		}
		offset, reached = anchor, i+1
	}
	if r := &card.Rooms[reached]; r.HasBeacon {
		return offset.Add(r.BeaconLocal), reached
	}
	return offset, reached
}

// siteNextAnchor is the frame of the room past room's gate when room stands at
// offset: the core's rule, 1.5 AU down the gate and 2500 m past the landing.
func siteNextAnchor(room, next *siteRoom, offset destiny.Vec3) destiny.Vec3 {
	gatePos := room.Gate.Local.Add(offset)
	landing := gatePos.Add(room.Gate.Dir.Scale(siteGateSpacing))
	anchor := landing.Add(room.Gate.Dir.Scale(siteLandingBuffer))
	if next.HasBeacon {
		anchor = anchor.Sub(next.BeaconLocal)
	}
	return anchor
}

// siteFireEvent replays one trigger event for a room and spawns what it
// releases. False when nothing came out. A connected pilot holding the room
// fires the core's own triggers, so the module stands back to avoid a double
// release.
func (m *Mod) siteFireEvent(eb *embodiedBot, site modapi.SiteLive, card *siteCard, room *siteRoom,
	event string, objectID int64) bool {
	if modapi.Sites().PlayerInRoom(site.SiteID, room.RoomID) {
		return false
	}
	// siteLock makes the ledger read and the build one step: two projections in
	// one room would otherwise both fire roomCleared and release two waves.
	lk := siteLock(site.SiteID)
	lk.Lock()
	defer lk.Unlock()
	fresh, ok := modapi.Sites().Get(site.SiteID)
	if !ok {
		return false
	}
	p := siteProgressFor(site.SiteID, fresh.Killed, fresh.Released)
	// The quiet the caller saw may be stale by now: a wave released by the
	// pass that held the lock before us is out, even if not yet on our grid.
	if event == "roomCleared" && siteRoomWavesSince(site, fresh, room.RoomID) {
		return false
	}
	waves := siteReleases(room, p, event, objectID)
	noteSiteFired(site.SiteID, p)
	out := false
	for _, w := range waves {
		if !modapi.Sites().MarkWave(site.SiteID, room.RoomID, w) {
			continue
		}
		if _, err := modapi.Sites().BuildRoom(site.SiteID, room.RoomID, w); err != nil {
			rlogf("rolebots: site %d wave %d: %v", site.SiteID, w, err)
			continue
		}
		out = true
		m.logBot(eb.charID, logInfo, "site", "a wave comes out in room %d of %s", room.Index+1, card.Name)
	}
	return out
}

// siteFireAutoWave lets a quiet room's next auto wave out (siteAutoWave) and
// builds it. Same stand-back as siteFireEvent: a connected pilot in the room
// runs the core's own chain.
func (m *Mod) siteFireAutoWave(eb *embodiedBot, site modapi.SiteLive, card *siteCard, room *siteRoom) bool {
	if modapi.Sites().PlayerInRoom(site.SiteID, room.RoomID) {
		return false
	}
	lk := siteLock(site.SiteID)
	lk.Lock()
	defer lk.Unlock()
	fresh, ok := modapi.Sites().Get(site.SiteID)
	if !ok {
		return false
	}
	if siteRoomWavesSince(site, fresh, room.RoomID) {
		return false // somebody's wave is out; the room is not quiet any more
	}
	w := siteAutoWave(room, siteProgressFor(site.SiteID, fresh.Killed, fresh.Released))
	if w == 0 || !modapi.Sites().MarkWave(site.SiteID, room.RoomID, w) {
		return false
	}
	if _, err := modapi.Sites().BuildRoom(site.SiteID, room.RoomID, w); err != nil {
		rlogf("rolebots: site %d auto wave %d: %v", site.SiteID, w, err)
		return false
	}
	m.logBot(eb.charID, logInfo, "site", "a wave comes out in room %d of %s", room.Index+1, card.Name)
	return true
}

// siteRoomWavesSince reports a wave of the room released on the record after
// the caller's snapshot, which makes the caller's quiet-room verdict stale.
func siteRoomWavesSince(then, now modapi.SiteLive, roomID int64) bool {
	for w := range now.Released[roomID] {
		if !then.Released[roomID][w] {
			return true
		}
	}
	return false
}

// siteOnBotKill is landDamage's hook: a site NPC this projection killed
// raises npcKilled and, when the room has nothing else standing, roomCleared.
func (m *Mod) siteOnBotKill(eb *embodiedBot, target *destiny.Entity) {
	if eb == nil || target == nil || m.npc == nil || !SitesEnabled() {
		return
	}
	siteID := m.npc.GetOpenSiteID(target.ID)
	missionID := int64(0)
	if siteID == 0 {
		// A mission pocket's NPC carries the mission tag instead (modapi.MissionSiteID).
		if missionID = m.npc.GetMissionID(target.ID); missionID > 0 {
			siteID = modapi.MissionSiteID(missionID)
		}
	}
	if siteID == 0 {
		return
	}
	objectID := m.npc.GetDunObjectID(target.ID)
	if missionID > 0 && objectID > 0 {
		// The core's death handler recorded the kill and tallied it, but only clears a
		// killAll pocket for a connection holding the room; the mark re-runs that clear.
		m.goSafeBot(eb.charID, "mission kill", func() { modapi.Sites().MarkKilled(siteID, objectID) })
	}
	roomID := target.Slim.DunRoomID
	victim := target.ID
	eb.mu.Lock()
	gangID := eb.gangID
	eb.mu.Unlock()
	noteSiteClaimKills(siteID, eb.charID, gangID, 1)
	m.goSafeBot(eb.charID, "site kill", func() {
		site, ok := modapi.Sites().Get(siteID)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		card, err := m.siteCardFor(ctx, site.DungeonVID)
		if err != nil || card == nil {
			return
		}
		var room *siteRoom
		for i := range card.Rooms {
			if card.Rooms[i].RoomID == roomID {
				room = &card.Rooms[i]
			}
		}
		if room == nil {
			return
		}
		m.siteFireEvent(eb, site, card, room, "npcKilled", objectID)
		if m.siteRoomRatsAlive(eb, siteID, roomID, victim) {
			return // a rat still stands; the clear waits
		}
		m.siteFireEvent(eb, site, card, room, "roomCleared", 0)
	})
}

// siteRoomRatsAlive is whether any rat of the room still stands, except aside
// (a kill being reported). Structures never count: invulnerable scenery cannot
// die and the core's roomCleared census skips structures, so the room is clear
// when its rats are dead and the prize is shot after.
func (m *Mod) siteRoomRatsAlive(eb *embodiedBot, siteID, roomID int64, except uint32) bool {
	for _, id := range modapi.Sites().LiveRoomNPCs(siteID, roomID) {
		if id == except {
			continue
		}
		if eb != nil && eb.ss != nil {
			if e := eb.ss.GetEntity(id); e != nil && (e.IsDestroyed() || npc.IsInertStructure(e.Slim.GroupID)) {
				continue
			}
		}
		return true
	}
	return false
}

// siteRivalsOnGrid is the ships on this projection's grid of every
// corporation it is fighting for its site (siteRivalCorps). It reads the claim
// list and grid only, since it runs on the combat goroutine.
func (m *Mod) siteRivalsOnGrid(eb *embodiedBot) []uint32 {
	if eb == nil || eb.ent == nil || eb.ss == nil {
		return nil
	}
	eb.mu.Lock()
	siteID, gangID, corpID := eb.siteID, eb.gangID, eb.corpID
	eb.mu.Unlock()
	if siteID == 0 {
		return nil
	}
	rivals := siteRivalCorps(siteID, eb.charID, gangID, corpID)
	if len(rivals) == 0 {
		return nil
	}
	var out []uint32
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if e == nil || !e.IsPlayer || e.IsDestroyed() || e.Slim.GroupID == capsuleGroupID {
			continue
		}
		if rivals[e.Slim.CorpID] {
			out = append(out, e.ID)
		}
	}
	return out
}

// siteTargetsOnGrid is what a projection running a site shoots beyond what
// the grid already offers it: the site's rats in its room (whether or not
// they have noticed it) and the structures the author put a prize in.
func (m *Mod) siteTargetsOnGrid(eb *embodiedBot) []uint32 {
	if eb == nil || m.npc == nil || eb.ss == nil || eb.ent == nil {
		return nil
	}
	eb.mu.Lock()
	siteID, roomID, loot := eb.siteID, eb.siteRoomID, eb.siteLoot
	eb.mu.Unlock()
	if siteID == 0 {
		return nil
	}
	// Structures are offered only once no rat of the room is left standing, so a
	// projection never grinds a container down under fire (siterun.go does the same).
	var rats, structs []uint32
	ratsAlive := false
	for _, id := range m.npc.OpenSiteEntities(siteID) {
		e := eb.ss.GetEntity(id)
		if e == nil || e.IsDestroyed() {
			continue
		}
		if roomID != 0 && e.Slim.DunRoomID != roomID {
			continue
		}
		if npc.IsInertStructure(e.Slim.GroupID) {
			if e.IsInvulnerable() {
				continue // scenery: nothing can grind it down
			}
			switch SitesLootStructures() {
			case 2:
			case 1:
				if !loot[e.Slim.DunObjectID] {
					continue
				}
			default:
				continue
			}
			structs = append(structs, id)
			continue
		}
		ratsAlive = true
		if lawEnforcementGroup(e.Slim.GroupID) || !m.gridNPCHostileTo(eb, e) {
			continue
		}
		rats = append(rats, id)
	}
	if ratsAlive {
		return rats
	}
	return structs
}
