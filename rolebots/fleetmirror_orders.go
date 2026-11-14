package rolebots

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gurimoon/destiny"
	"gurimoon/server/gangsta"
	"gurimoon/server/lsc"
)

// Fleet-level orders typed in a mirrored fleet's chat (fleetmirror.go). The parser is
// conscription's (conscript_orders.go), but an order means less here: an officer aboard
// moves the op (where it mines, where it hunts, when it goes home), not the pilots one by
// one.
//
// Who is obeyed: a human aboard who holds CEO or Director in the fleet's corporation, read
// from the DB, never the sender's own word. Anyone aboard may ask for status, the roll call
// or the list. An officer who says "take command" leads the real gang until they hand it
// back (fleetmirror_command.go); while they do, only they give orders, and their calls
// (primary, secondary, hold, retreat, anchor) stand for as long as they lead.

// handleMirrorChat is one line from a mirrored fleet's chat.
func (m *Mod) handleMirrorChat(ctx context.Context, msg lsc.ChatMessage, fm *fleetMirror) {
	sender := msg.SenderCharID
	if sender == 0 || m.isSynthetic(sender) || gangsta.GangOf(sender) != fm.RealGangID {
		return
	}
	g, err := getGang(ctx, m.db, fm.ModGangID)
	if err != nil || g == nil {
		return
	}
	crew := make([]crewMember, 0, len(fm.Bots))
	for id := range fm.Bots {
		name, _ := m.charNameCorp(ctx, id)
		role := RoleCombat
		if mem, err := gangMemberOf(ctx, m, fm.ModGangID, id); err == nil && mem != nil {
			role = mem.Role
		}
		crew = append(crew, crewMember{CharID: id, Name: name, Role: role})
	}
	o, ok := parseOrderIn(msg.Text, crew, m.gangPals(ctx, fm.RealGangID))
	if !ok {
		return
	}
	if !orderRateOK(sender) {
		return
	}
	fc := fm.LeaderBotID
	say := func(urgent bool, format string, args ...any) {
		m.narrateMirror(ctx, fm, fc, urgent, format, args...)
	}
	humanFC := mirrorHumanFC(fm)

	// Questions: anyone aboard.
	switch o.Verb {
	case OrderStatus:
		say(true, "%s", m.fleetSitrep(ctx, g))
		return
	case OrderList:
		say(true, "%s", m.mirrorRoll(ctx, g))
		return
	case OrderHelp:
		m.mirrorHelp(ctx, fm, sender, o.Topic, say)
		return
	}

	officer := m.isCorpOfficer(ctx, fm.CorpID, sender)
	switch o.Verb {
	case OrderTakeCommand:
		m.mirrorTakeCommand(ctx, fm, sender, officer, say)
		return
	case OrderHandBack:
		m.mirrorHandBackBy(ctx, fm, sender, officer, say)
		return
	}
	if !officer {
		say(true, "That's the chair's call — I take fleet orders from a Director of %s.", corpNameOf(ctx, m.db, fm.CorpID))
		return
	}
	if humanFC != 0 && sender != humanFC {
		say(true, "%s has the fleet — ask them, or wait for them to hand it back.", m.charNameOr(ctx, humanFC))
		return
	}
	who := m.charNameOr(ctx, sender)
	done := func(what, ack string) {
		m.logBot(fc, logInfo, "fleet", "Director %s ordered: %s", who, what)
		m.recordChatCommand(ctx, fm.CorpID, sender, what)
		say(true, "%s", ack)
	}
	if humanFC != 0 {
		m.mirrorFCOrder(ctx, fm, g, sender, o, say, done)
		return
	}

	switch o.Verb {
	case OrderMine, OrderPickOre:
		m.mirrorMineHere(ctx, g, sender, o, say, done)

	case OrderAnchor, OrderFlee, OrderAlign, OrderStay, OrderOnCall:
		// The fleet-level calls (fleetcommand_grid.go): they reach the
		// commander's projection on the grid and stand for the fight.
		lead := fleetCommanderLive(fm.LeaderBotID)
		if lead == nil {
			say(true, "The commander is not on a grid right now — nothing to call.")
			return
		}
		switch o.Verb {
		case OrderAnchor:
			m.mirrorAnchor(ctx, lead, o, say, done)
		case OrderFlee, OrderAlign:
			m.callFleetRetreat(lead, "the chair called it")
			done("align out", "Align out — the chair called it.")
		case OrderStay, OrderOnCall:
			m.directorHold(lead, true)
			done("hold position", "Holding — the line anchors and fires on call only.")
		}

	case OrderWeaponsFree, OrderResume:
		// Lifts a hold. "Stand down" with no hold standing still winds the op down.
		if lead := fleetCommanderLive(fm.LeaderBotID); lead != nil && fleetHoldCalled(lead) {
			m.directorHold(lead, false)
			done("weapons free", "Weapons free.")
			return
		}
		if o.Verb == OrderResume {
			m.mirrorWindDown(ctx, g, say, done)
			return
		}
		say(true, "Nothing's holding us.")

	case OrderAttack, OrderDefend, OrderTackle, OrderHuntHere:
		if g.Activity != ActCombat && g.Activity != ActSecurity && g.Activity != ActPiracy {
			say(true, "This is a %s fleet — the guns aboard are an escort, not a pack.", strings.ToLower(activityLabel(g.Activity)))
			return
		}
		// "primary <pilot>" names a pilot on the commander's grid as the fleet's primary for the
		// fight (fleetcommand_grid.go). Without a name the order moves the hunt.
		if o.Verb == OrderAttack && o.Subject != 0 {
			lead := fleetCommanderLive(fm.LeaderBotID)
			if lead == nil {
				say(true, "The commander is not on a grid right now — nothing to call.")
				return
			}
			label, ok := m.directorPrimary(lead, o.Subject)
			if !ok {
				say(true, "%s is not on the commander's grid.", m.charNameOr(ctx, o.Subject))
				return
			}
			done("primary "+label, fmt.Sprintf("Primary is %s.", label))
			return
		}
		m.mirrorHuntHere(ctx, g, sender, say, done)

	case OrderGoHome, OrderDismiss, OrderDock:
		m.mirrorWindDown(ctx, g, say, done)

	default:
		say(true, "That one I'd need the fleet released for. From the chair I take: mine here, mine <ore>, hunt here, hold, weapons free, stand down — or say \"take command\" to lead us yourself.")
	}
}

// mirrorFCOrder is an order from the human who took command: the calls of the fight, where
// the op works, and when it ends. Warp Gang and Regroup in the client move the fleet.
func (m *Mod) mirrorFCOrder(ctx context.Context, fm *fleetMirror, g *Gang, sender int64, o ChatOrder,
	say func(bool, string, ...any), done func(string, string)) {
	switch o.Verb {
	case OrderMine, OrderPickOre:
		m.mirrorMineHere(ctx, g, sender, o, say, done)
		return
	case OrderHuntHere:
		m.mirrorHuntHere(ctx, g, sender, say, done)
		return
	case OrderFollow:
		mirrorMu.Lock()
		fm.FollowFC = true
		mirrorMu.Unlock()
		done("follow the FC", "On you — the op goes where you go.")
		return
	case OrderGoHome, OrderDismiss, OrderDock:
		m.mirrorHandBack(ctx, fm, "the op is ending")
		m.mirrorWindDown(ctx, g, say, done)
		return
	}

	lead := fleetCommanderLive(fm.LeaderBotID)
	if lead == nil {
		say(true, "%s is not on a grid right now — nothing to call.", m.charNameOr(ctx, fm.LeaderBotID))
		return
	}
	switch o.Verb {
	case OrderAttack, OrderTackle, OrderSecondary:
		id := m.mirrorCalledBall(lead, fm, sender, o)
		if id == 0 {
			say(true, "Primary what? Tag it, lock it, link it or name it.")
			return
		}
		if o.Verb == OrderSecondary {
			if label, ok := m.directorSecondary(lead, id); ok {
				done("secondary "+label, fmt.Sprintf("Secondary is %s.", label))
			}
			return
		}
		if label, ok := m.directorPrimaryBall(lead, id); ok {
			done("primary "+label, fmt.Sprintf("Primary is %s.", label))
		}
	case OrderStay, OrderOnCall, OrderHoldFire:
		m.directorHold(lead, true)
		if o.Verb == OrderHoldFire {
			m.directorClearPrimary(lead)
		}
		done("hold", "Holding — the line fires on your call only.")
	case OrderWeaponsFree, OrderResume:
		m.directorHold(lead, false)
		m.directorClearPrimary(lead)
		done("weapons free", "Weapons free.")
	case OrderFlee, OrderAlign:
		m.callFleetRetreat(lead, "the FC called it")
		done("align out", "Align out — the FC called it.")
	case OrderAnchor:
		m.mirrorAnchor(ctx, lead, o, say, done)
	default:
		say(true, "Warp Gang and Regroup move us. In chat I take: primary, secondary, anchor on, hold, weapons free, align out, follow me, hunt here, mine here, hand back.")
	}
}

// mirrorCalledBall resolves what an attack order names, on the commander's grid: a linked
// pilot, a rat by name, else the first thing the FC has locked.
func (m *Mod) mirrorCalledBall(lead *embodiedBot, fm *fleetMirror, sender int64, o ChatOrder) uint32 {
	if o.Subject != 0 {
		if e := m.pilotBall(lead, o.Subject); e != nil {
			return e.ID
		}
	}
	ours := func(e *destiny.Entity) bool {
		return e.Slim.CharID != 0 && (fm.Bots[e.Slim.CharID] || gangsta.GangOf(e.Slim.CharID) == fm.RealGangID)
	}
	may := func(e *destiny.Entity) bool { return e != nil && !e.IsDestroyed() && !ours(e) }
	if id := m.namedOnGrid(lead, orderTargetName(o.Rest), may); id != 0 {
		return id
	}
	for _, id := range bossLockedTargets(sender) {
		if lead.ss != nil && may(lead.ss.GetEntity(id)) {
			return id
		}
	}
	return 0
}

// mirrorAnchor is "anchor on <name>": the parser reads a name after a friendly verb as its
// subject, a crew name before the verb as the addressee.
func (m *Mod) mirrorAnchor(ctx context.Context, lead *embodiedBot, o ChatOrder,
	say func(bool, string, ...any), done func(string, string)) {
	on := o.Subject
	if on == 0 && len(o.Targets) > 0 {
		on = o.Targets[0]
	}
	if on <= 0 {
		say(true, "Anchor on whom? Name one of the fleet.")
		return
	}
	m.directorAnchor(lead, on)
	done("anchor on "+m.charNameOr(ctx, on), fmt.Sprintf("Anchor on %s.", m.charNameOr(ctx, on)))
}

// mirrorMineHere moves a mining op to the officer's system, optionally onto one ore.
func (m *Mod) mirrorMineHere(ctx context.Context, g *Gang, sender int64, o ChatOrder,
	say func(bool, string, ...any), done func(string, string)) {
	if g.Activity != ActMining && g.Activity != ActIndMining {
		say(true, "This is a %s fleet — nobody aboard has a laser.", strings.ToLower(activityLabel(g.Activity)))
		return
	}
	sysID, sysName := m.directorSystem(sender)
	if sysID == 0 {
		say(true, "I can't see where you are, %s — undock and say it again.", m.charNameOr(ctx, sender))
		return
	}
	oreID, oreName := m.oreNamed(ctx, o.Rest)
	if err := m.mirrorResite(ctx, g, sysID, sysName, oreID, oreName, o.Verb == OrderMine); err != nil {
		return
	}
	what := "mine in " + sysName
	if oreName != "" {
		what += " — " + oreName
	}
	done(what, fmt.Sprintf("Moving the op to %s%s — miners, re-site.", sysName,
		map[bool]string{true: " for " + oreName, false: ""}[oreName != ""]))
}

// mirrorHuntHere moves a combat op's hunt (or a raid's camp) to the officer's system.
func (m *Mod) mirrorHuntHere(ctx context.Context, g *Gang, sender int64,
	say func(bool, string, ...any), done func(string, string)) {
	if g.Activity != ActCombat && g.Activity != ActSecurity && g.Activity != ActPiracy {
		say(true, "This is a %s fleet — the guns aboard are an escort, not a pack.", strings.ToLower(activityLabel(g.Activity)))
		return
	}
	sysID, sysName := m.directorSystem(sender)
	if sysID == 0 {
		say(true, "I can't see where you are, %s — undock and say it again.", m.charNameOr(ctx, sender))
		return
	}
	if err := m.mirrorResite(ctx, g, sysID, sysName, 0, "", false); err != nil {
		return
	}
	done("hunt in "+sysName, fmt.Sprintf("Moving the hunt to %s.", sysName))
}

// mirrorResite pins the op to a system: a mining op's belt system (and ore, or none when
// clearOre), a combat op's hunt, a raid's camp.
func (m *Mod) mirrorResite(ctx context.Context, g *Gang, sysID int64, sysName string,
	oreID int64, oreName string, clearOre bool) error {
	switch g.Activity {
	case ActMining, ActIndMining:
		g.State.PinSystemID = sysID
		g.State.SystemID, g.State.SystemName, g.State.BeltItemID, g.State.PickedAt = 0, "", 0, 0
		if oreID != 0 {
			g.State.OreTypeID, g.State.OreName = oreID, oreName
		} else if clearOre {
			g.State.OreTypeID, g.State.OreName = 0, ""
		}
	case ActPiracy:
		g.State.PirSystemID, g.State.PirSystemName, g.State.PirGateID, g.State.PirPickedAt = sysID, sysName, 0, clock.Unix()
	default:
		if g.State.HuntSystemID != 0 && g.State.HuntSystemID != sysID {
			g.State.HuntRecent = append([]int64{g.State.HuntSystemID}, g.State.HuntRecent...)
			if len(g.State.HuntRecent) > gangHuntRecentMax {
				g.State.HuntRecent = g.State.HuntRecent[:gangHuntRecentMax]
			}
		}
		dropSiteOffHunt(&g.State, g.GangID, sysID)
		g.State.HuntSystemID, g.State.HuntSystemName, g.State.HuntPickedAt = sysID, sysName, clock.Unix()
		g.State.OrderSystemID = sysID
	}
	g.State.SiteSeq++
	if err := saveGangState(ctx, m.db, g.GangID, &g.State); err != nil {
		rlogf("rolebots: director re-site gang %d: %v", g.GangID, err)
		return err
	}
	return nil
}

// mirrorOpSystem is the system a mirrored op works in now, 0 when it has none pinned.
func mirrorOpSystem(g *Gang) int64 {
	switch g.Activity {
	case ActMining, ActIndMining:
		if g.State.PinSystemID != 0 {
			return g.State.PinSystemID
		}
		return g.State.SystemID
	case ActPiracy:
		return g.State.PirSystemID
	}
	return g.State.HuntSystemID
}

// mirrorWindDown sends the op home to settle.
func (m *Mod) mirrorWindDown(ctx context.Context, g *Gang,
	say func(bool, string, ...any), done func(string, string)) {
	if g.State.OpPhase == gangOpWindDown || g.State.OpPhase == gangOpSettled {
		say(true, "Already winding down.")
		return
	}
	g.State.OpPhase = gangOpWindDown
	g.State.WindDownDeadline = clock.Unix() + WindDownSec()
	g.State.SiteSeq++
	if err := saveGangState(ctx, m.db, g.GangID, &g.State); err != nil {
		rlogf("rolebots: director stand-down gang %d: %v", g.GangID, err)
		return
	}
	done("stand down", fmt.Sprintf("Understood — winding down. Back to %s to settle up.",
		m.stationLabel(ctx, g.State.StagingStationID)))
}

// directorSystem is where the Director is right now: a real client's
// session, never a presence row.
func (m *Mod) directorSystem(charID int64) (int64, string) {
	s := m.bossSession(charID)
	if s == nil || s.SolarSystemID == 0 {
		return 0, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.SolarSystemID, m.nameOfSystem(ctx, s.SolarSystemID)
}

// mirrorRoll is the roll call: every bot aboard with its seat.
func (m *Mod) mirrorRoll(ctx context.Context, g *Gang) string {
	mems, err := gangMembers(ctx, m.db, g.GangID)
	if err != nil {
		return "The roster could not be read."
	}
	var parts []string
	for _, mem := range mems {
		if !mem.Driven {
			continue
		}
		name := m.charNameOr(ctx, mem.CharacterID)
		if mem.CharacterID == g.LeaderCharID {
			name += " (commanding)"
		}
		parts = append(parts, name+" — "+roleLabel(mem.Role))
	}
	if len(parts) == 0 {
		return "Nobody aboard."
	}
	return strings.Join(parts, "; ")
}

// gangMemberOf is one membership row.
func gangMemberOf(ctx context.Context, m *Mod, gangID, charID int64) (*GangMember, error) {
	var mem GangMember
	var driven int64
	var lo, hi, style int
	var tank string
	err := m.db.QueryRowContext(ctx, `
		SELECT gm.gangID, gm.characterID, gm.role, gm.recruitCorpID, COALESCE(b.enabled, 0),
		       gm.hullMin, gm.hullMax, gm.tank, gm.style
		  FROM rolebot_gang_members gm
		  LEFT JOIN rolebot_characters b ON b.characterID = gm.characterID
		 WHERE gm.gangID = ? AND gm.characterID = ?`, gangID, charID).
		Scan(&mem.GangID, &mem.CharacterID, &mem.Role, &mem.RecruitCorpID, &driven, &lo, &hi, &tank, &style)
	if err != nil {
		return nil, err
	}
	mem.Driven = driven != 0
	mem.HullMin, mem.HullMax = HullClass(lo), HullClass(hi)
	mem.Tank, mem.Style = TankPref(tank), combatStance(style)
	return &mem, nil
}

// recordChatCommand files a chat order on the corporation's board as a
// done command, so the Operations page shows what the chair said.
func (m *Mod) recordChatCommand(ctx context.Context, corpID, posterID int64, what string) {
	payload, _ := json.Marshal(map[string]string{"what": what})
	id, err := m.postCorpCommand(ctx, corpCommand{
		CorpID: corpID, PosterID: posterID, PosterHat: m.officerHat(ctx, corpID, posterID),
		Kind: cmdKindChat, Payload: string(payload), Note: what, ExpiresAt: clock.Unix() + 3600,
	})
	if err != nil {
		return
	}
	_ = m.setCommandStatus(ctx, id, cmdDone, "carried out in fleet chat")
}

// officerHat words the chair a pilot holds in a corporation.
func (m *Mod) officerHat(ctx context.Context, corpID, charID int64) string {
	if m.corpCEOID(ctx, corpID) == charID {
		return "CEO"
	}
	if m.isCorpOfficer(ctx, corpID, charID) {
		return "Director"
	}
	return "member"
}
