package rolebots

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gurimoon/server/gangsta"
	"gurimoon/server/lsc"
)

// The ears: fleet chat in, orders out.
//
// The core hands every plain-text line to ModuleChat on the sender's connection goroutine, so
// the handler does one map read and one non-blocking channel send. A worker drains the channel,
// not the five-second lane, which is too slow for a player watching a chat window.

// chatQueueDepth bounds the backlog. Overflow is dropped: orders are not a log and the newest
// matters.
const chatQueueDepth = 256

// orderRateGap and orderRateBurst are the per-speaker limits. They bound how much parsing one
// player can cause; they do not police conversation.
const (
	orderRateGap   = 1 * time.Second
	orderRateBurst = 20
)

// pendingOrder is an instruction waiting for the pilot's next turn. It is kept in memory, not
// on the state row: it matters for seconds, and fleets are dissolved at boot.
type pendingOrder struct {
	Verb    string
	Rest    string
	Subject int64
	GangID  int64
	BossID  int64
	At      int64
	// DestID is the first place linked in the line (a planet, a gate); RangeM the distance
	// asked for, -1 when none was.
	DestID int64
	RangeM float64
}

var (
	pendingMu     sync.Mutex
	pendingOrders = map[int64]*pendingOrder{}

	chatIn     chan lsc.ChatMessage
	chatInOnce sync.Once

	orderRateMu   sync.Mutex
	orderRateLast = map[int64]time.Time{}
	orderRateHits = map[int64][]time.Time{}
)

// postOrder hands an instruction to one pilot, replacing anything it had not acted on. Newest
// wins, since a queue would have the bot do "mine" and then "no, dock".
func postOrder(charID int64, o *pendingOrder) {
	pendingMu.Lock()
	pendingOrders[charID] = o
	pendingMu.Unlock()
}

// takeOrder removes and returns a pilot's pending instruction.
func takeOrder(charID int64) *pendingOrder {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	o := pendingOrders[charID]
	delete(pendingOrders, charID)
	return o
}

// peekOrder reports a pilot's pending instruction without consuming it: the pre-emption check
// runs every pass and must not eat an order it declines to act on.
func peekOrder(charID int64) *pendingOrder {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	return pendingOrders[charID]
}

func dropOrder(charID int64) {
	pendingMu.Lock()
	delete(pendingOrders, charID)
	pendingMu.Unlock()
}

// ── intake ──────────────────────────────────────────────────────────────────

// startChatIntake registers the hook and the worker that drains it.
func (m *Mod) startChatIntake() {
	chatInOnce.Do(func() {
		chatIn = make(chan lsc.ChatMessage, chatQueueDepth)
		lsc.ModuleChat = m.onChat
		go m.chatWorker()
	})
}

// onChat runs on the speaker's connection goroutine. It does a bounded map read or a
// non-blocking send; parsing happens on the worker.
func (m *Mod) onChat(msg lsc.ChatMessage) {
	if msg.SenderCharID == 0 || msg.Text == "" {
		return
	}
	if !Master() {
		return
	}
	// Local is heard for one thing only: a player answering a bot (chatter_retort.go). It has its
	// own queue so a busy hub cannot crowd out orders.
	if msg.SystemID != 0 {
		m.onLocalChat(msg)
		return
	}
	// A region channel is heard for market talk only (chatter_market.go), on Local's queue.
	if isRegionChannel(msg.ChannelID) {
		m.onLocalChat(msg)
		return
	}
	if msg.GangID == 0 {
		// Not fleet chat. A corporation channel a person is reading belongs to chatter
		// (chatter_reply.go); anything else is nobody's.
		if !chatterWantsCorpLine(msg) {
			return
		}
		select {
		case chatIn <- msg:
		default:
			rlogf("rolebots: chat backlog full, dropping a corp line from channel %d", msg.ChannelID)
		}
		return
	}
	// Two kinds of fleet listen: one a human conscripted bots into, and one the module opened for
	// a human (fleetmirror.go).
	if (!Conscription() || crewOf(msg.GangID) == nil) && mirrorByRealGang(msg.GangID) == nil {
		return // no bots in that fleet; nothing here is addressed to us
	}
	select {
	case chatIn <- msg:
	default:
		rlogf("rolebots: chat backlog full, dropping a line from gang %d", msg.GangID)
	}
}

func (m *Mod) chatWorker() {
	for msg := range chatIn {
		func() {
			defer func() {
				// A parser panic must not take the worker down, or one bad line silences every fleet.
				if r := recover(); r != nil {
					rlogf("rolebots: chat worker recovered: %v", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if msg.GangID == 0 {
				m.handleCorpChat(ctx, msg)
				return
			}
			m.handleFleetChat(ctx, msg)
		}()
	}
}

// handleFleetChat is the whole path from a line to an order.
func (m *Mod) handleFleetChat(ctx context.Context, msg lsc.ChatMessage) {
	crew := crewOf(msg.GangID)
	if crew == nil {
		if fm := mirrorByRealGang(msg.GangID); fm != nil {
			m.handleMirrorChat(ctx, msg, fm)
		}
		return
	}
	// Only the crew's commander is obeyed (conscript_command.go). With two humans a bot would obey
	// both, and they could countermand each other with no record of who said what. A human leader
	// the crew has not caught up with yet is reconciled first.
	if msg.SenderCharID != crew.BossCharID {
		if gangsta.GangLeader(msg.GangID) != msg.SenderCharID {
			return
		}
		m.reconcileCrew(ctx, msg.GangID, gangsta.GangEvent{})
		if crew = crewOf(msg.GangID); crew == nil || crew.BossCharID != msg.SenderCharID {
			return
		}
	}

	members := make([]crewMember, 0, len(crew.Members))
	for _, k := range crew.Members {
		name, _ := m.charNameCorp(ctx, k.CharID)
		members = append(members, crewMember{CharID: k.CharID, Name: name, Role: k.Role})
	}

	o, ok := parseOrderIn(msg.Text, members, m.gangPals(ctx, msg.GangID))
	if !ok {
		return
	}
	// Spent only on a line that parsed, so talk does not use up the commander's orders.
	if !orderRateOK(msg.SenderCharID) {
		return
	}
	if len(o.Ambiguous) > 0 {
		m.sayCrew(ctx, msg.GangID, true, "Two of us answer to that — %s?",
			strings.Join(o.Ambiguous, " or "))
		return
	}

	targets := o.Targets
	all := len(targets) == 0
	if all {
		for _, k := range crew.Members {
			targets = append(targets, k.CharID)
		}
	}
	if len(targets) == 1 && targets[0] == -1 {
		// A role alias nobody fills: say so rather than widening it to everybody.
		m.sayCrew(ctx, msg.GangID, true, "Nobody's flying that seat.")
		return
	}

	m.dispatchOrder(ctx, msg, crew, o, targets, all)
}

// dispatchOrder routes a parsed order. Meta verbs are questions and are answered at once;
// everything else is posted for the pilot's next turn, where the interruptible rules apply.
func (m *Mod) dispatchOrder(ctx context.Context, msg lsc.ChatMessage, crew *conscriptCrew,
	o ChatOrder, targets []int64, all bool) {

	switch o.Verb {
	case OrderHelp:
		// "help <topic>" answers in chat from the manual's table (fcmanual.go); "help" mails it.
		if o.Topic != "" {
			for _, line := range manualChat(manualConscript, o.Topic) {
				m.sayCrew(ctx, msg.GangID, true, "%s", line)
			}
			return
		}
		sp := speakerFor(crew)
		m.sendBriefing(ctx, sp, msg.SenderCharID, true)
		m.sayCrew(ctx, msg.GangID, true,
			"Sent you the list. Short version: attack, hold fire, follow me, warp to, align out, mine, status, dismiss. Say \"help fighting\" or \"help moving\" for more.")
		return

	case OrderList:
		m.sayCrew(ctx, msg.GangID, true, "%s", m.crewRoll(ctx, crew))
		return

	case OrderStatus:
		m.reportStatus(ctx, msg.GangID, crew, targets, all)
		return

	case OrderSetRole:
		m.assignRole(ctx, msg.GangID, targets, o.Rest)
		return

	case OrderDismiss:
		m.sayCrew(ctx, msg.GangID, true, "Understood. Good hunting.")
		for _, id := range targets {
			m.release(ctx, id, "dismissed")
		}
		return

	case OrderTakeCommand, OrderHandBack, OrderHuntHere:
		m.sayCrew(ctx, msg.GangID, true, "You already have us. That one is for a corporation fleet.")
		return
	}

	now := clock.Unix()

	// The fire state answers at once (fctags.go); the guns read it every tick.
	switch o.Verb {
	case OrderWeaponsFree, OrderOnCall:
		free := o.Verb == OrderWeaponsFree
		for _, id := range targets {
			updateFireCall(id, func(c *fireCall) {
				c.OnCall = !free
				if free {
					c.HoldFire = false
				}
			})
		}
		m.touchOrders(ctx, targets, now)
		if free {
			m.sayCrew(ctx, msg.GangID, true, "Weapons free.")
		} else {
			m.sayCrew(ctx, msg.GangID, true, "Holding for your call — tag it, lock it or name it.")
		}
		return
	case OrderHoldFire:
		for _, id := range targets {
			updateFireCall(id, func(c *fireCall) { c.HoldFire = true })
		}
	case OrderAttack, OrderTackle, OrderDefend, OrderSecondary:
		slot := 1
		if o.Verb == OrderSecondary {
			slot = 2
		}
		name := ""
		if o.Verb != OrderDefend {
			name = orderTargetName(o.Rest)
		}
		for _, id := range targets {
			updateFireCall(id, func(c *fireCall) { c.HoldFire, c.Slot, c.Name = false, slot, name })
		}
		if o.Verb == OrderSecondary {
			o.Verb = OrderAttack
		}
	case OrderSplit:
		if !m.splitCrew(ctx, msg, crew, targets, now) {
			return
		}
		m.touchOrders(ctx, targets, now)
		m.sayCrew(ctx, msg.GangID, false, "Tackle on the secondary, guns on the primary.")
		return
	}

	for _, id := range targets {
		p := newPendingOrder(o, o.Verb, msg, now)
		// A movement order is flown from memory on the next escort tick (fcmove_embodied.go); any
		// other order but a fire order ends one.
		switch {
		case isMoveVerb(o.Verb):
			setMoveCall(id, moveCall{Verb: o.Verb, Subject: o.Subject, DestID: p.DestID, RangeM: p.RangeM, At: now})
		case o.Verb != OrderHoldFire:
			dropMoveCall(id)
		}
		postOrder(id, p)
	}
	m.touchOrders(ctx, targets, now)

	// One acknowledgement: from the fleet's speaker for a group order, from the addressed pilot
	// for a single one. Not urgent, so it is the first line dropped when the fleet is talking.
	if all || len(targets) > 1 {
		m.sayCrew(ctx, msg.GangID, false, "%s", orderAck())
	} else {
		m.sayFleet(ctx, msg.GangID, targets[0], false, "%s", orderAck())
	}
}

// newPendingOrder is a parsed order addressed to one pilot.
func newPendingOrder(o ChatOrder, verb string, msg lsc.ChatMessage, now int64) *pendingOrder {
	p := &pendingOrder{
		Verb:    verb,
		Rest:    o.Rest,
		Subject: o.Subject,
		GangID:  msg.GangID,
		BossID:  msg.SenderCharID,
		At:      now,
		RangeM:  -1,
	}
	if len(o.Places) > 0 {
		p.DestID = o.Places[0].ItemID
	}
	if o.HasRange {
		p.RangeM = o.RangeM
	}
	return p
}

// splitCrew is "split": tacklers and EWAR hold the secondary, the guns kill the primary. It
// reports false, having said why, when nobody flies tackle.
func (m *Mod) splitCrew(ctx context.Context, msg lsc.ChatMessage, crew *conscriptCrew,
	targets []int64, now int64) bool {
	role := map[int64]string{}
	for _, k := range crew.Members {
		role[k.CharID] = k.Role
	}
	tacklers := 0
	for _, id := range targets {
		if r := role[id]; r == RoleTackler || r == RoleEwar {
			tacklers++
		}
	}
	if tacklers == 0 {
		m.sayCrew(ctx, msg.GangID, true, "Nobody's flying tackle — say \"<name> you're the tackle\" first.")
		return false
	}
	for _, id := range targets {
		verb, slot := OrderAttack, 1
		if r := role[id]; r == RoleTackler || r == RoleEwar {
			verb, slot = OrderTackle, 2
		}
		updateFireCall(id, func(c *fireCall) { c.HoldFire, c.Slot, c.Name = false, slot, "" })
		postOrder(id, newPendingOrder(ChatOrder{}, verb, msg, now))
	}
	return true
}

// touchOrders stamps lastOrderAt so the idle-release sweep knows the crew is still in use.
func (m *Mod) touchOrders(ctx context.Context, ids []int64, now int64) {
	if len(ids) == 0 {
		return
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, now)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	_, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_conscripts SET lastOrderAt = ? WHERE characterID IN (`+
			strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		rlogf("rolebots: stamp conscript orders: %v", err)
	}
}

// ── the meta answers ────────────────────────────────────────────────────────

func (m *Mod) crewRoll(ctx context.Context, crew *conscriptCrew) string {
	if crew == nil || len(crew.Members) == 0 {
		return "Nobody, apparently."
	}
	parts := make([]string, 0, len(crew.Members))
	for _, k := range crew.Members {
		name, _ := m.charNameCorp(ctx, k.CharID)
		if name == "" {
			continue
		}
		if k.Role != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", name, k.Role))
			continue
		}
		parts = append(parts, name)
	}
	if len(parts) == 0 {
		return "Nobody, apparently."
	}
	return strings.Join(parts, ", ") + "."
}

// reportStatus answers what each addressed pilot is doing, in one line for a group.
func (m *Mod) reportStatus(ctx context.Context, gangID int64, crew *conscriptCrew, targets []int64, all bool) {
	if !all && len(targets) == 1 {
		id := targets[0]
		m.sayFleet(ctx, gangID, id, true, "%s.", m.whatImDoing(ctx, id))
		return
	}
	parts := make([]string, 0, len(targets))
	for _, id := range targets {
		name, _ := m.charNameCorp(ctx, id)
		if name == "" {
			continue
		}
		first, _ := splitName(name)
		parts = append(parts, fmt.Sprintf("%s: %s", first, strings.ToLower(m.whatImDoing(ctx, id))))
	}
	if len(parts) == 0 {
		return
	}
	m.sayCrew(ctx, gangID, true, "%s.", strings.Join(parts, "; "))
}

// whatImDoing is the pilot's own account of its turn: the task's label plus its detail line.
// It is the same prose the panel shows, so the two descriptions cannot drift.
func (m *Mod) whatImDoing(ctx context.Context, charID int64) string {
	st, err := loadState(ctx, m.db, charID)
	if err != nil || st == nil {
		return "Not sure, honestly"
	}
	label := phaseLabel(st)
	if d, ok := taskDef(st.Phase); ok && d.Detail != nil {
		if extra := d.Detail(m, ctx, st); extra != "" {
			return label + " — " + extra
		}
	}
	return label
}

// assignRole is "<name> you're the hauler".
func (m *Mod) assignRole(ctx context.Context, gangID int64, targets []int64, rest string) {
	role := ""
	low := strings.ToLower(rest)
	for _, r := range conscriptRoles {
		if containsWords(low, r) || containsWords(low, r+"s") {
			role = r
			break
		}
	}
	// The chat words for a seat are not always the seat's own name.
	if role == "" {
		for alias, r := range groupAliases {
			if r != "" && containsWords(low, alias) {
				role = r
				break
			}
		}
	}
	if role == "" {
		m.sayCrew(ctx, gangID, true, "%s", pickLine(confusedLines))
		return
	}
	for _, id := range targets {
		if _, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_conscripts SET role = ? WHERE characterID = ?`, role, id); err != nil {
			rlogf("rolebots: set conscript role: %v", err)
			continue
		}
		m.sayFleet(ctx, gangID, id, true, "%s it is.", role)
	}
	m.refreshConscripts(ctx)
}

// ── rate limiting ───────────────────────────────────────────────────────────

func orderRateOK(charID int64) bool {
	now := clock.Now()
	orderRateMu.Lock()
	defer orderRateMu.Unlock()
	if last, ok := orderRateLast[charID]; ok && now.Sub(last) < orderRateGap {
		return false
	}
	kept := orderRateHits[charID][:0]
	for _, t := range orderRateHits[charID] {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= orderRateBurst {
		orderRateHits[charID] = kept
		return false
	}
	orderRateLast[charID] = now
	orderRateHits[charID] = append(kept, now)
	return true
}

func sweepOrderRate() {
	now := clock.Now()
	orderRateMu.Lock()
	defer orderRateMu.Unlock()
	for id, t := range orderRateLast {
		if now.Sub(t) > 5*time.Minute {
			delete(orderRateLast, id)
			delete(orderRateHits, id)
		}
	}
}

func init() {
	registerCharForget("pendingOrders", func(id int64) {
		pendingMu.Lock()
		delete(pendingOrders, id)
		pendingMu.Unlock()
	})
	registerCharForget("orderRateLast/orderRateHits", func(id int64) {
		orderRateMu.Lock()
		delete(orderRateLast, id)
		delete(orderRateHits, id)
		orderRateMu.Unlock()
	})
}