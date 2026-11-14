package rolebots

import (
	"context"
	"sort"
	"time"

	"gurimoon/server/gangsta"
)

// Who commands a conscript crew, and what a change in the gang does to it.
//
// The crew's commander (rolebot_conscripts.bossCharID, the cache's BossCharID) is the gang's
// leader while a human leads. A leaderless gang keeps its commander while that pilot is aboard,
// else the online human with the lowest character ID. A crew with no human to follow is
// released. A bot made leader hands the gang straight back: a crew does not lead itself.
//
// The core reports every player leave, kick, disconnect, disband and leadership change
// (gangsta.ModuleGangEvent); each one reconciles that gang's crew on one worker, in order. The
// conscript lane runs the same reconcile every pass as a backstop.

// gangEventQueueDepth bounds the event backlog. A full queue drops the event; the lane's
// reconcile catches up within five seconds.
const gangEventQueueDepth = 256

var gangEventIn chan gangsta.GangEvent

// startGangEvents starts the worker. Called once, from registerConscription.
func (m *Mod) startGangEvents() {
	if gangEventIn != nil {
		return
	}
	gangEventIn = make(chan gangsta.GangEvent, gangEventQueueDepth)
	go func() {
		for ev := range gangEventIn {
			func() {
				defer func() {
					if r := recover(); r != nil {
						rlogf("rolebots: gang event worker recovered: %v", r)
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				m.handleGangEvent(ctx, ev)
			}()
		}
	}()
}

// onGangEvent runs on the player's connection goroutine: a cache read and a non-blocking send.
func (m *Mod) onGangEvent(ev gangsta.GangEvent) {
	if !Master() || ev.GangID == 0 {
		return
	}
	if crewOf(ev.GangID) == nil && mirrorByRealGang(ev.GangID) == nil {
		return
	}
	select {
	case gangEventIn <- ev:
	default:
		rlogf("rolebots: gang event backlog full, dropping %+v", ev)
	}
}

// handleGangEvent routes one event to the crew or the mirrored fleet it concerns.
func (m *Mod) handleGangEvent(ctx context.Context, ev gangsta.GangEvent) {
	if fm := mirrorByRealGang(ev.GangID); fm != nil {
		m.mirrorGangEvent(ctx, fm, ev)
		return
	}
	m.reconcileCrew(ctx, ev.GangID, ev)
}

// reconcileCrew brings one crew in line with its gang: members who are no longer aboard are
// released, a bot leader hands the gang back, and the crew follows nextBoss. ev is the event
// that prompted it, or the zero event from the lane and from enlist; it only words the
// release lines.
func (m *Mod) reconcileCrew(ctx context.Context, gangID int64, ev gangsta.GangEvent) {
	crew := crewOf(gangID)
	if crew == nil {
		return
	}
	if ev.Dissolved {
		for _, k := range crew.Members {
			m.release(ctx, k.CharID, "the fleet broke up")
		}
		return
	}
	for _, k := range crew.Members {
		if gangsta.GangOf(k.CharID) == gangID {
			continue
		}
		why := "no longer in that fleet"
		if ev.Kind == gangsta.GangMemberKicked && ev.CharID == k.CharID {
			why = "kicked from the fleet"
		}
		m.release(ctx, k.CharID, why)
	}
	if crew = crewOf(gangID); crew == nil {
		return
	}

	leader := gangsta.GangLeader(gangID)
	if leader != 0 && m.isSynthetic(leader) {
		// Somebody made a bot the leader. Hand it to the human who held it, else the commander.
		back := ev.OldLeader
		if back == 0 || m.isSynthetic(back) || gangsta.GangOf(back) != gangID {
			back = crew.BossCharID
		}
		if back != 0 && gangsta.GangOf(back) == gangID && gangsta.BotMakeLeader(gangID, back) {
			m.sayFleet(ctx, gangID, leader, true, "Not ours to lead — the fleet is yours.")
			leader = back
		} else {
			leader = 0
		}
	}

	bossAboard := crew.BossCharID != 0 && gangsta.GangOf(crew.BossCharID) == gangID
	next := nextBoss(crew.BossCharID, bossAboard, leader, gangsta.HumanMembersOnline(gangID))
	switch {
	case next == 0:
		m.sayCrew(ctx, gangID, true, "Nobody left to fly for. Heading off.")
		for _, k := range crew.Members {
			m.release(ctx, k.CharID, "the fleet has nobody flying it")
		}
	case next != crew.BossCharID:
		m.repointBoss(ctx, crew, next)
	}
}

// nextBoss is who a crew follows: the gang's leader when one leads (the caller has already
// handed a bot leader back), else the current commander while aboard, else the online human
// with the lowest ID, else nobody (0). Pure.
func nextBoss(cur int64, curAboard bool, leader int64, humansOnline []int64) int64 {
	if leader != 0 {
		return leader
	}
	if cur != 0 && curAboard {
		return cur
	}
	if len(humansOnline) == 0 {
		return 0
	}
	ids := append([]int64(nil), humansOnline...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids[0]
}

// repointBoss moves a crew to a new commander. Each pilot asks itself whether it flies for
// them (willFlyFor) and leaves if not; the rest follow. The embodied tier reads the commander
// from the cache (embody.go), so no state blob is rewritten.
func (m *Mod) repointBoss(ctx context.Context, crew *conscriptCrew, boss int64) {
	bossCorp := m.charCorpID(ctx, boss)
	name := m.charNameOr(ctx, boss)
	stay := 0
	for _, k := range crew.Members {
		if why := m.willFlyFor(ctx, k.CharID, m.charCorpID(ctx, k.CharID), boss); why != "" {
			m.sayFleet(ctx, crew.GangID, k.CharID, true, "I don't fly for %s. Good luck.", name)
			m.release(ctx, k.CharID, "would not fly for the new commander")
			continue
		}
		stay++
	}
	if stay == 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_conscripts SET bossCharID = ?, bossCorpID = ?, lastOrderAt = ? WHERE gangID = ?`,
		boss, bossCorp, clock.Unix(), crew.GangID); err != nil {
		rlogf("rolebots: repoint crew of gang %d: %v", crew.GangID, err)
		return
	}
	m.refreshConscripts(ctx)
	m.sayCrew(ctx, crew.GangID, true, "Following %s.", name)
	m.logBot(speakerFor(crew), logInfo, "conscript", "crew of gang %d now follows %d", crew.GangID, boss)
}

// gangPals is the humans aboard a gang, named, for the order parser's second name pool.
func (m *Mod) gangPals(ctx context.Context, gangID int64) []crewMember {
	var out []crewMember
	for _, id := range gangsta.BotMembers(gangID) {
		if m.isSynthetic(id) {
			continue
		}
		if name, _ := m.charNameCorp(ctx, id); name != "" {
			out = append(out, crewMember{CharID: id, Name: name})
		}
	}
	return out
}

// conscriptGangIDs is every gang with a crew, from the cache.
func conscriptGangIDs() []int64 {
	conscriptMu.RLock()
	defer conscriptMu.RUnlock()
	out := make([]int64, 0, len(conscriptCrews))
	for id := range conscriptCrews {
		out = append(out, id)
	}
	return out
}
