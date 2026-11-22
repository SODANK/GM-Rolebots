package rolebots

import (
	"gurimoon/destiny"
	"gurimoon/marshal"
	"gurimoon/server"
)

// lockNotes keeps a pilot's "locked by" list in step with what an embodied bot has
// locked. The client draws the red "attacking you" box only for a ship in that list,
// which OnTarget otheradd fills. Without it a bot's offensive OnSpecialFX leaves the
// pilot yellow while its drones, locked through the core NPC callback, go red. The
// core's NPC lock callback (main.go) and posLockNotify (posguns.go) send the same event.
//
// Keyed by the victim's ball ID; the value is the entity the add went to, so the
// matching otherlost reaches it after the ball has left the grid. Owned by the combat
// loop goroutine and not safe for concurrent use.
type lockNotes map[uint32]*destiny.Entity

// sync sends otheradd to every pilot in want not yet told and otherlost to every told
// pilot no longer in want. A pilot whose client does not hold the bot's ball yet is
// left out and tried again on the next call. Non-pilot IDs in want are ignored.
func (n lockNotes) sync(eb *embodiedBot, want ...uint32) {
	if eb.ent == nil {
		return
	}
	keep := make(map[uint32]bool, len(want))
	for _, id := range want {
		if id != 0 {
			keep[id] = true
		}
	}
	for id, t := range n {
		if !keep[id] {
			sendLockNote(eb.ent, t, false)
			delete(n, id)
		}
	}
	if eb.ss == nil || eb.ent.IsDestroyed() {
		return
	}
	for id := range keep {
		if _, told := n[id]; told {
			continue
		}
		t := eb.ss.GetEntity(id)
		if t == nil || !t.IsPlayer || t.Conn == nil || t.IsDestroyed() || !t.KnowsBall(eb.ent.ID) {
			continue
		}
		sendLockNote(eb.ent, t, true)
		n[id] = t
	}
}

// clear sends otherlost to every pilot told so far.
func (n lockNotes) clear(eb *embodiedBot) {
	n.sync(eb)
}

// lockedIDs is what the ship holds a lock on: the gun target (0 while its lock delay
// runs) and every ball an EWAR module is applied to.
func lockedIDs(l *combatLoadout, gunTarget uint32) []uint32 {
	ids := []uint32{gunTarget}
	if l != nil {
		for _, ew := range l.ewar {
			ids = append(ids, ew.appliedTo)
		}
	}
	return ids
}

// sendLockNote sends one OnTarget otheradd or otherlost naming bot to target.
func sendLockNote(bot, target *destiny.Entity, add bool) {
	if bot == nil || target == nil || target.Conn == nil || target.IsDestroyed() {
		return
	}
	ev := "otheradd"
	if !add {
		ev = "otherlost"
	} else {
		target.NoteTargetedBy(bot.ID)
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: []marshal.PyRep{marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "OnTarget"},
			marshal.PyString{Value: ev},
			marshal.PyInt{Value: uint64(bot.ID)},
			marshal.PyNone{},
		}}}},
	}}
	if err := server.SendNotification(target.Conn, int64(target.AccountID), "OnMultiEvent", "clientID", args); err != nil {
		rlogf("rolebots: lock note %s to %d: %v", ev, target.ID, err)
	}
}
