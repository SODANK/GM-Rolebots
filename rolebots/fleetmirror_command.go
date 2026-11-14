package rolebots

import (
	"context"

	"gurimoon/gang"
	"gurimoon/server/gangsta"
)

// A human in command of a mirrored fleet (docs/FLEET_DOCTRINE.md §2.7).
//
// "take command" makes a CEO or Director the real gang's leader; the bot commander keeps the
// module fleet and relays their calls (fleetcommand_grid.go reads mirrorFCOf). "hand back",
// leaving, logging off or passing the lead to a bot returns command; to another officer it
// moves (mirrorLeaderAction). A bot the FC kicks is benched until hand back, and "follow me"
// re-pins the op to the FC's system (mirrorFollowFC).

// mirrorFCOf is the human in command of the mirrored fleet run by this module gang, and that
// fleet's real gang. Zeros when a bot commands or there is no mirror.
func mirrorFCOf(modGangID int64) (int64, int64) {
	if modGangID == 0 {
		return 0, 0
	}
	mirrorMu.RLock()
	defer mirrorMu.RUnlock()
	fm := mirrors[modGangID]
	if fm == nil || fm.HumanFC == 0 {
		return 0, 0
	}
	return fm.HumanFC, fm.RealGangID
}

// mirrorTakeCommand is an officer's "take command".
func (m *Mod) mirrorTakeCommand(ctx context.Context, fm *fleetMirror, sender int64, officer bool,
	say func(bool, string, ...any)) {
	if !officer {
		say(true, "Command is for a Director of %s.", corpNameOf(ctx, m.db, fm.CorpID))
		return
	}
	if cur := mirrorHumanFC(fm); cur != 0 {
		if cur == sender {
			say(true, "You already have the fleet.")
		} else {
			say(true, "%s has the fleet.", m.charNameOr(ctx, cur))
		}
		return
	}
	if !gangsta.BotMakeLeader(fm.RealGangID, sender) {
		say(true, "I can't hand you the fleet — are you still in it?")
		return
	}
	m.mirrorSetFC(ctx, fm, sender)
	say(true, "You have the fleet. Warp Gang and Regroup move us; tag a target to call it. Say \"hand back\" when you're done.")
	m.recordChatCommand(ctx, fm.CorpID, sender, "take command")
	m.sendFCManual(ctx, fm.LeaderBotID, sender, manualDirector, false)
}

// mirrorSetFC records the human in command and anchors the line on them.
func (m *Mod) mirrorSetFC(ctx context.Context, fm *fleetMirror, fc int64) {
	mirrorMu.Lock()
	fm.HumanFC, fm.FCSince = fc, clock.Unix()
	mirrorMu.Unlock()
	if lead := fleetCommanderLive(fm.LeaderBotID); lead != nil {
		m.directorAnchor(lead, fc)
		m.directorHold(lead, false)
	}
	m.logBot(fm.LeaderBotID, logInfo, "fleet", "%s took command of fleet %d", m.charNameOr(ctx, fc), fm.RealGangID)
}

// mirrorHandBackBy is "hand back": the FC, or any officer while the FC is offline.
func (m *Mod) mirrorHandBackBy(ctx context.Context, fm *fleetMirror, sender int64, officer bool,
	say func(bool, string, ...any)) {
	cur := mirrorHumanFC(fm)
	if cur == 0 {
		say(true, "I have the fleet already.")
		return
	}
	online := false
	for _, id := range gangsta.HumanMembersOnline(fm.RealGangID) {
		if id == cur {
			online = true
		}
	}
	if sender != cur && (!officer || online) {
		say(true, "That's %s's to hand back.", m.charNameOr(ctx, cur))
		return
	}
	m.mirrorHandBack(ctx, fm, "handed back")
	m.recordChatCommand(ctx, fm.CorpID, sender, "hand back")
}

// mirrorHandBack returns command to the bot commander: the real gang's lead, the line's calls
// and any bots the FC benched.
func (m *Mod) mirrorHandBack(ctx context.Context, fm *fleetMirror, why string) {
	mirrorMu.Lock()
	fc := fm.HumanFC
	fm.HumanFC, fm.FCSince, fm.FollowFC = 0, 0, false
	benched := fm.Benched
	fm.Benched = nil
	for id := range benched {
		// Forgotten, so the reconcile seats them again.
		delete(fm.Bots, id)
	}
	mirrorMu.Unlock()
	if fc == 0 {
		return
	}
	if gangsta.GangOf(fm.LeaderBotID) == fm.RealGangID {
		gangsta.BotMakeLeader(fm.RealGangID, fm.LeaderBotID)
	}
	if lead := fleetCommanderLive(fm.LeaderBotID); lead != nil {
		lead.mu.Lock()
		lead.call.humanPrimary, lead.call.humanAnchor, lead.call.hold = 0, 0, false
		lead.mu.Unlock()
	}
	m.narrateMirror(ctx, fm, fm.LeaderBotID, true, "I have the fleet — %s.", why)
	m.logBot(fm.LeaderBotID, logInfo, "fleet", "command of fleet %d back from %d: %s", fm.RealGangID, fc, why)
}

// mirrorGangEvent is a player's gang change in a mirrored fleet (conscript_command.go routes it).
func (m *Mod) mirrorGangEvent(ctx context.Context, fm *fleetMirror, ev gangsta.GangEvent) {
	fc := mirrorHumanFC(fm)
	switch mirrorLeaderAction(ev, fc, fm.LeaderBotID, m.isSynthetic(ev.NewLeader),
		ev.NewLeader != 0 && m.isCorpOfficer(ctx, fm.CorpID, ev.NewLeader), fm.Bots[ev.CharID]) {
	case mirrorActHandBack:
		m.mirrorHandBack(ctx, fm, "the FC stepped away")
	case mirrorActTransfer:
		m.mirrorSetFC(ctx, fm, ev.NewLeader)
		m.narrateMirror(ctx, fm, fm.LeaderBotID, true, "%s has the fleet.", m.charNameOr(ctx, ev.NewLeader))
		m.sendFCManual(ctx, fm.LeaderBotID, ev.NewLeader, manualDirector, false)
	case mirrorActRevert:
		back := fc
		if back == 0 || gangsta.GangOf(back) != fm.RealGangID {
			back = fm.LeaderBotID
		}
		gangsta.BotMakeLeader(fm.RealGangID, back)
		m.narrateMirror(ctx, fm, fm.LeaderBotID, true, "Command of this fleet stays with the officers of %s.",
			corpNameOf(ctx, m.db, fm.CorpID))
	case mirrorActBench:
		mirrorMu.Lock()
		if fm.Benched == nil {
			fm.Benched = map[int64]bool{}
		}
		fm.Benched[ev.CharID] = true
		mirrorMu.Unlock()
		m.leaveFleetChat(fm.RealGangID, ev.CharID)
	}
}

// mirrorAct is what a gang event does to a mirrored fleet.
type mirrorAct int

const (
	mirrorActNone mirrorAct = iota
	mirrorActHandBack
	mirrorActTransfer
	mirrorActRevert
	mirrorActBench
)

// mirrorLeaderAction decides it. fc is the human in command (0: the bot commander leads),
// commander the bot commander, newIsBot and newIsOfficer describe ev.NewLeader, kickedBot
// whether ev.CharID is one of the fleet's bots. Pure.
func mirrorLeaderAction(ev gangsta.GangEvent, fc, commander int64, newIsBot, newIsOfficer, kickedBot bool) mirrorAct {
	switch ev.Kind {
	case gangsta.GangMemberLeft, gangsta.GangMemberDisconnected, gangsta.GangMemberKicked:
		if fc != 0 && ev.CharID == fc {
			return mirrorActHandBack
		}
		if ev.Kind == gangsta.GangMemberKicked && fc != 0 && ev.ByCharID == fc && kickedBot {
			return mirrorActBench
		}
	case gangsta.GangLeaderChanged:
		switch {
		case ev.NewLeader == 0:
			if fc != 0 {
				return mirrorActHandBack
			}
			// The bot commander never resigns; a leaderless window is taken back.
			return mirrorActRevert
		case ev.NewLeader == fc:
			return mirrorActNone
		case newIsBot || ev.NewLeader == commander:
			if fc != 0 {
				return mirrorActHandBack
			}
			return mirrorActNone
		case newIsOfficer:
			return mirrorActTransfer
		default:
			return mirrorActRevert
		}
	}
	return mirrorActNone
}

// mirrorFollowFC re-pins the op to the FC's system when "follow me" stands and the FC moved.
// Called from reconcileMirror.
func (m *Mod) mirrorFollowFC(ctx context.Context, fm *fleetMirror, g *Gang) {
	mirrorMu.RLock()
	fc, follow := fm.HumanFC, fm.FollowFC
	mirrorMu.RUnlock()
	if fc == 0 || !follow {
		return
	}
	sysID, sysName := m.directorSystem(fc)
	if sysID == 0 || sysID == mirrorOpSystem(g) {
		return
	}
	if err := m.mirrorResite(ctx, g, sysID, sysName, 0, "", false); err == nil {
		m.narrateMirror(ctx, fm, fm.LeaderBotID, false, "Following you to %s.", sysName)
	}
}

// mirrorHelp answers "help" in a mirrored fleet: the line for the asker's place in it, and the
// manual by mail (fcmanual.go).
func (m *Mod) mirrorHelp(ctx context.Context, fm *fleetMirror, sender int64, topic string,
	say func(bool, string, ...any)) {
	kind := manualRider
	if m.isCorpOfficer(ctx, fm.CorpID, sender) {
		kind = manualDirector
	}
	if topic != "" {
		for _, line := range manualChat(kind, topic) {
			say(true, "%s", line)
		}
		return
	}
	m.sendFCManual(ctx, fm.LeaderBotID, sender, kind, true)
	if kind == manualDirector {
		say(true, "Sent you the list. From the chair: mine here, hunt here, hold, weapons free, align out, stand down — or \"take command\" to lead us yourself.")
		return
	}
	say(true, "Sent you the list. Anyone can ask for status or a roll call; the officers give the orders.")
}

// ── Warp Gang ───────────────────────────────────────────────────────────────

// onGangWarp is gang.ModuleWarp: a mirrored fleet's FC moves its bots, anything else is a
// conscript crew's commander.
func (m *Mod) onGangWarp(o gang.WarpOrder) {
	if fm := mirrorByRealGang(o.GangID); fm != nil {
		m.mirrorGangWarp(o, fm)
		return
	}
	m.conscriptGangWarp(o)
}

// mirrorGangWarp carries a mirrored fleet's bots on the FC's Warp Gang: every bot of the
// fleet on the FC's grid that is not benched.
func (m *Mod) mirrorGangWarp(o gang.WarpOrder, fm *fleetMirror) {
	mirrorMu.RLock()
	fc := fm.HumanFC
	ids := make([]int64, 0, len(fm.Bots))
	for id := range fm.Bots {
		if !fm.Benched[id] {
			ids = append(ids, id)
		}
	}
	mirrorMu.RUnlock()
	if fc == 0 || o.LeaderID != fc {
		return
	}
	embodyMu.Lock()
	crew := make([]*embodiedBot, 0, len(ids))
	for _, id := range ids {
		if eb := embodyLive[id]; eb != nil {
			crew = append(crew, eb)
		}
	}
	embodyMu.Unlock()
	m.warpCrewAlong(o, crew, func(*embodiedBot) bool { return true })
}
