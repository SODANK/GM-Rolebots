package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/npc"
)

// The agent-mission seam, package-main half (modapi/missionbridge_mod.go has the
// other), plus the mission branch of the site seam (rolebotsite_mod.go routes a
// negative site ID here). Every agent action goes through handleAgentDoAction with no
// connection, so a rolebot is offered, paid and judged exactly as a player is.
// Overlay build only. Design: modules/GM-RoleBot-mod/docs/MISSIONS.md.

func init() {
	modapi.InstallMissions(modapi.MissionHooks{
		Request:  modMissionRequest,
		Accept:   modMissionAccept,
		Decline:  modMissionDecline,
		Complete: modMissionComplete,
		Quit:     modMissionQuit,
	})
}

// modMissionAgentAddr is the log tag of a rolebot's agent conversation.
const modMissionAgentAddr = "rolebot"

func modMissionAct(charID, accountID, agentID, stationID int64, action int) error {
	if globalDB == nil {
		return fmt.Errorf("mission: core not up")
	}
	handleAgentDoAction(nil, accountID, modMissionAgentAddr, agentID, charID, stationID, action, "", globalDB)
	return nil
}

func modMissionRequest(charID, accountID, agentID, stationID int64) (int64, string, error) {
	if err := modMissionAct(charID, accountID, agentID, stationID, agentActionRequestMission); err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := evedb.GetActiveMission(ctx, globalDB, charID, agentID)
	if err != nil {
		return 0, "", err
	}
	if m == nil || (m.State != "offered" && m.State != "accepted") {
		// The refusals (standing, cooldown, an empty deck) are conversation text; the
		// pilot only needs to know there is no work here now.
		return 0, "the agent has nothing on offer", nil
	}
	return m.MissionID, "", nil
}

func modMissionAccept(charID, accountID, agentID, stationID int64) error {
	return modMissionTransition(charID, accountID, agentID, stationID, agentActionAccept, "offered", "accepted")
}

func modMissionDecline(charID, accountID, agentID, stationID int64) error {
	return modMissionTransition(charID, accountID, agentID, stationID, agentActionDecline, "offered", "declined")
}

func modMissionQuit(charID, accountID, agentID, stationID int64) error {
	mid, err := modMissionPending(charID, agentID, "accepted")
	if err != nil {
		return err
	}
	if err := modMissionTransition(charID, accountID, agentID, stationID, agentActionQuitMission, "accepted", "failed"); err != nil {
		return err
	}
	modMissionPocketGone(mid, "rolebot quit")
	return nil
}

func modMissionComplete(charID, accountID, agentID, stationID int64) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mid, err := modMissionPending(charID, agentID, "accepted")
	if err != nil {
		return false, "", err
	}
	if err := modMissionAct(charID, accountID, agentID, stationID, agentActionComplete); err != nil {
		return false, "", err
	}
	m, err := evedb.GetMissionByID(ctx, globalDB, mid)
	if err != nil || m == nil {
		return false, "", fmt.Errorf("mission %d: %v", mid, err)
	}
	if m.State == "completed" {
		modMissionPocketGone(mid, "rolebot turn-in")
		return true, "", nil
	}
	// Not paid: say why in the core's own words.
	why := "the agent did not accept the turn-in"
	tmpl, _ := evedb.GetTemplateByID(ctx, globalDB, m.TemplateID)
	agent, _ := evedb.GetAgentInfo(ctx, globalDB, agentID)
	if tmpl != nil && agent != nil {
		if _, reason := checkMissionComplete(ctx, tmpl, m, charID, stationID, agent.StationID, globalDB); reason != "" {
			why = reason
		}
	}
	return false, why, nil
}

// modMissionPending is the pilot's mission with this agent in the given state.
func modMissionPending(charID, agentID int64, state string) (int64, error) {
	if globalDB == nil {
		return 0, fmt.Errorf("mission: core not up")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := evedb.GetActiveMission(ctx, globalDB, charID, agentID)
	if err != nil {
		return 0, err
	}
	if m == nil || m.State != state {
		return 0, fmt.Errorf("no %s mission with agent %d", state, agentID)
	}
	return m.MissionID, nil
}

// modMissionTransition runs one agent action on the mission in state from and checks
// that it reached state to.
func modMissionTransition(charID, accountID, agentID, stationID int64, action int, from, to string) error {
	mid, err := modMissionPending(charID, agentID, from)
	if err != nil {
		return err
	}
	if err := modMissionAct(charID, accountID, agentID, stationID, action); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := evedb.GetMissionByID(ctx, globalDB, mid)
	if err != nil || m == nil {
		return fmt.Errorf("mission %d: %v", mid, err)
	}
	if m.State != to {
		return fmt.Errorf("mission %d is %s, not %s, after the agent action", mid, m.State, to)
	}
	return nil
}

// modMissionPocketGone drops a finished mission's live NPCs and room frames. The core's
// cleanupMissionDungeon has already removed its rows and site state.
func modMissionPocketGone(missionID int64, reason string) {
	missionPockets.Delete(missionID)
	if globalNPCMgr != nil {
		globalNPCMgr.DespawnMission(missionID, reason)
	}
}

// ── the pocket as a site ────────────────────────────────────────────────────

// missionPocket is the overlay's record of one accepted kill mission's dungeon: where
// it is and the frames its deeper rooms were laid out in. The killed and released sets
// are the core's dunSiteState, shared with the player path.
type missionPocket struct {
	missionID   int64
	sysID       int64
	vid         int64
	firstRoomID int64
	anchor      destiny.Vec3
	spawnMu     sync.Mutex

	mu      sync.Mutex
	offsets map[int64]destiny.Vec3
}

// missionPockets maps missionID to *missionPocket. An entry is dropped on turn-in, on
// quit, and when a read finds the mission no longer accepted.
var missionPockets sync.Map

// missionPocketFor returns the pocket of an accepted kill mission, or nil when the
// mission has no dungeon instance.
func missionPocketFor(missionID int64) *missionPocket {
	if v, ok := missionPockets.Load(missionID); ok {
		return v.(*missionPocket)
	}
	if globalDB == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inst, err := evedb.GetDungeonInstanceByMission(ctx, globalDB, missionID)
	if err != nil || inst == nil {
		return nil
	}
	p := &missionPocket{
		missionID: missionID, sysID: inst.SolarSystemID, vid: inst.DungeonVID,
		firstRoomID: inst.RoomID,
		anchor:      destiny.Vec3{X: inst.AnchorX, Y: inst.AnchorY, Z: inst.AnchorZ},
		offsets:     map[int64]destiny.Vec3{},
	}
	v, _ := missionPockets.LoadOrStore(missionID, p)
	return v.(*missionPocket)
}

// roomFrame is openSite.roomFrame's rule: the recorded offset, else the first room on
// the anchor's beacon, else computed (the first visitor's frame).
func (p *missionPocket) roomFrame(roomID int64, computed, beaconLocal destiny.Vec3, hasBeacon bool) destiny.Vec3 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if off, ok := p.offsets[roomID]; ok {
		return off
	}
	off := computed
	if roomID == p.firstRoomID {
		off = p.anchor
		if hasBeacon {
			off = off.Sub(beaconLocal)
		}
	}
	p.offsets[roomID] = off
	return off
}

func modMissionSiteGet(missionID int64) (modapi.SiteLive, bool) {
	if globalDB == nil {
		return modapi.SiteLive{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := evedb.GetMissionByID(ctx, globalDB, missionID)
	if err != nil || m == nil || m.State != "accepted" {
		missionPockets.Delete(missionID)
		return modapi.SiteLive{}, false
	}
	p := missionPocketFor(missionID)
	if p == nil {
		return modapi.SiteLive{}, false
	}
	v := modapi.SiteLive{
		SiteID: modapi.MissionSiteID(missionID), MissionID: missionID,
		DungeonVID: p.vid, SolarSystemID: p.sysID,
		X: p.anchor.X, Y: p.anchor.Y, Z: p.anchor.Z,
		FirstRoomID: p.firstRoomID,
		Killed:      map[int64]bool{},
		Released:    map[int64]map[int64]bool{},
	}
	p.mu.Lock()
	v.RoomOffsets = make(map[int64]destiny.Vec3, len(p.offsets))
	for id, off := range p.offsets {
		v.RoomOffsets[id] = off
	}
	p.mu.Unlock()
	copyDunSiteState(dunSiteFor(missionID, true), v.Killed, v.Released)
	if globalSysMgr != nil {
		v.Loaded = globalSysMgr.Get(p.sysID) != nil
	}
	return v, true
}

// copyDunSiteState copies a site record's dead objects and released waves.
func copyDunSiteState(st *dunSiteState, killed map[int64]bool, released map[int64]map[int64]bool) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, dead := range st.killed {
		if dead {
			killed[id] = true
		}
	}
	for room, waves := range st.waves {
		for w, out := range waves {
			if !out {
				continue
			}
			if released[room] == nil {
				released[room] = map[int64]bool{}
			}
			released[room][w] = true
		}
	}
}

// modMissionMarkKilled records a pocket object dead, first-wins. A first mark is a
// kill no NPC death reported (the module's paper fight), so it is tallied here by the
// core's rule. Every mark re-runs the killAll clear, which the core runs only for a
// connection holding the room.
func modMissionMarkKilled(missionID, objectID int64) bool {
	st := dunSiteFor(missionID, true)
	modSiteMarkMu.Lock()
	first := !st.IsKilled(objectID)
	if first {
		st.MarkKilled(objectID)
	}
	modSiteMarkMu.Unlock()
	modMissionAfterKill(missionID, objectID, first)
	return first
}

func modMissionAfterKill(missionID, objectID int64, tally bool) {
	if globalDB == nil {
		return
	}
	sqldb := globalDB
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, err := evedb.GetMissionByID(ctx, sqldb, missionID)
	if err != nil || m == nil || m.State != "accepted" {
		return
	}
	tmpl, err := evedb.GetTemplateByID(ctx, sqldb, m.TemplateID)
	if err != nil || tmpl == nil || tmpl.MissionType != "kill" {
		return
	}
	var roomID, typeID int64
	var groupID sql.NullInt64
	if err := sqldb.QueryRowContext(ctx, `
		SELECT o.roomID, o.typeID, t.groupID
		  FROM dunObjects o LEFT JOIN invTypes t ON t.typeID = o.typeID
		 WHERE o.objectID = ?`, objectID).Scan(&roomID, &typeID, &groupID); err != nil {
		log.Printf("[mission] rolebot kill missionID=%d obj=%d: %v", missionID, objectID, err)
		return
	}
	if tally {
		modMissionTally(ctx, sqldb, m, tmpl, typeID, groupID.Int64)
	}
	if tmpl.CompletionType == "killAll" {
		completeMissionSiteIfClear(sqldb, globalSessReg, missionID, roomID, nil)
	}
}

// modMissionTally is the NPC death handler's mission tally (main.go, "Mission kill
// tracking"): killAll skips structures, a targeted objective counts only its type.
// Keep the two in step.
func modMissionTally(ctx context.Context, sqldb *sql.DB, m *evedb.CharacterMission, tmpl *evedb.MissionTemplate, typeID, groupID int64) {
	if tmpl.CompletionType == "killAll" {
		if npc.IsDestructibleStructure(groupID) {
			return
		}
	} else if tmpl.TargetTypeID > 0 && typeID != tmpl.TargetTypeID {
		return
	}
	progress, required, changed, err := evedb.UpdateKillProgress(ctx, sqldb, m.MissionID, 1)
	if err != nil {
		log.Printf("[mission] rolebot kill tally missionID=%d: %v", m.MissionID, err)
		return
	}
	if !changed || required <= 0 || progress != required || tmpl.CompletionType == "killAll" {
		return
	}
	if _, err := evedb.CompleteKillObjective(ctx, sqldb, m.MissionID); err != nil {
		log.Printf("[mission] rolebot objective flag missionID=%d: %v", m.MissionID, err)
		return
	}
	log.Printf("[mission] missionID=%d objective complete by paper kill (charID=%d)", m.MissionID, m.CharID)
}

func modMissionMarkWave(missionID, roomID, wave int64) bool {
	st := dunSiteFor(missionID, true)
	modSiteMarkMu.Lock()
	defer modSiteMarkMu.Unlock()
	if st.WaveReleased(roomID, wave) {
		return false
	}
	st.MarkWaveReleased(roomID, wave)
	return true
}

// modMissionBuildRoom builds a pocket room with no connection. Its NPCs carry the
// mission tag, so their deaths reach the core's tally as a player's kills do.
func modMissionBuildRoom(missionID, roomID, wave int64) ([]uint32, error) {
	p := missionPocketFor(missionID)
	if p == nil {
		return nil, fmt.Errorf("mission %d has no pocket", missionID)
	}
	if globalNPCMgr == nil || globalSysMgr == nil {
		return nil, fmt.Errorf("mission %d: core not up", missionID)
	}
	npcMgr := globalNPCMgr
	// The rats the system build already put up (spawnMissionDungeonsForSystem) are adopted.
	standing := map[int64]uint32{}
	if ss := globalSysMgr.Get(p.sysID); ss != nil {
		for _, e := range ss.AllEntities() {
			if npcMgr.GetMissionID(e.ID) == missionID {
				if obj := npcMgr.GetDunObjectID(e.ID); obj > 0 {
					standing[obj] = e.ID
				}
			}
		}
	}
	return buildDunRoomHeadless(dunBuildTarget{
		label:     fmt.Sprintf("[mission] mission %d", missionID),
		sysID:     p.sysID,
		anchor:    p.anchor,
		state:     dunSiteFor(missionID, true),
		spawnMu:   &p.spawnMu,
		frame:     p.roomFrame,
		adopt:     func(objectID int64) uint32 { return standing[objectID] },
		tag:       func(eid uint32) { npcMgr.SetMissionID(eid, missionID) },
		missionID: missionID,
	}, roomID, wave)
}

// modMissionLiveRoomNPCs is the pocket's NPCs still alive in one room (0 = any).
func modMissionLiveRoomNPCs(missionID, roomID int64) []uint32 {
	p := missionPocketFor(missionID)
	if p == nil || globalSysMgr == nil || globalNPCMgr == nil {
		return nil
	}
	ss := globalSysMgr.Get(p.sysID)
	if ss == nil {
		return nil
	}
	var out []uint32
	for _, e := range ss.AllEntities() {
		if globalNPCMgr.GetMissionID(e.ID) != missionID {
			continue
		}
		if roomID != 0 && e.Slim.DunRoomID != roomID {
			continue
		}
		out = append(out, e.ID)
	}
	return out
}

// modMissionPilots is the connected pilots holding a room of the pocket (0 = any):
// the mission's owner flying it as a player. Their connection fires the triggers.
func modMissionPilots(missionID, roomID int64) []int64 {
	p := missionPocketFor(missionID)
	if p == nil || globalSysMgr == nil {
		return nil
	}
	ss := globalSysMgr.Get(p.sysID)
	if ss == nil {
		return nil
	}
	var out []int64
	for _, e := range ss.GetAllPlayersInSystem() {
		ds, ok := e.PlayerState.(*destinyConn)
		if !ok || ds == nil || e.Conn == nil {
			continue // a projection has no connection and fires nothing
		}
		if ds.dunMissionID == missionID && (roomID == 0 || ds.dunRoomID == roomID) && ds.charID != 0 {
			out = append(out, ds.charID)
		}
	}
	return out
}
