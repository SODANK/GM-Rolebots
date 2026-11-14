package rolebots

import (
	"sort"
	"strings"
	"sync"

	"gurimoon/destiny"
	"gurimoon/gang"
)

// A human commander's targeting calls: overview tags, the fire state each pilot is under, and
// the pure rule that turns them into one target (docs/FLEET_DOCTRINE.md §2.7).
//
// Tags arrive through gang.ModuleTagged; only the commander's count (gangCommander). Tag 1 is
// the primary, then 2 to 9, then 0, then the letters. The fire state (hold fire, on call, the
// slot, an NPC named in the order) lives in memory, written when the order is parsed, so the
// guns answer at once.

var (
	fcTagMu sync.Mutex
	fcTags  = map[int64]map[int64]any{} // gangID → itemID → tag
)

// onGangTag is gang.ModuleTagged: on the tagger's goroutine, a map write under a lock.
func (m *Mod) onGangTag(gangID, setter, itemID int64, tag any) {
	if gangID == 0 || itemID == 0 || setter == 0 || setter != gangCommander(gangID) {
		return
	}
	fcTagMu.Lock()
	defer fcTagMu.Unlock()
	t := fcTags[gangID]
	if tag == nil {
		delete(t, itemID)
		return
	}
	if _, ok := tagRank(tag); !ok {
		return
	}
	if t == nil {
		t = map[int64]any{}
		fcTags[gangID] = t
	}
	t[itemID] = tag
}

// gangCommander is the human whose calls a gang's bots take: a crew's commander, or the
// human FC of a mirrored fleet. 0 when there is none.
func gangCommander(gangID int64) int64 {
	if c := crewOf(gangID); c != nil {
		return c.BossCharID
	}
	if fm := mirrorByRealGang(gangID); fm != nil {
		return mirrorHumanFC(fm)
	}
	return 0
}

// installFCTags publishes the tag hook. Called from registerConscription.
func (m *Mod) installFCTags() { gang.ModuleTagged = m.onGangTag }

// pruneFCTags drops the tags of gangs that no longer have a crew or a mirror.
func pruneFCTags() {
	fcTagMu.Lock()
	ids := make([]int64, 0, len(fcTags))
	for id := range fcTags {
		ids = append(ids, id)
	}
	fcTagMu.Unlock()
	for _, id := range ids {
		if gangCommander(id) == 0 {
			fcTagMu.Lock()
			delete(fcTags, id)
			fcTagMu.Unlock()
		}
	}
}

// fcTagged is a gang's commander tags as ball IDs, in call order.
func fcTagged(gangID int64) []uint32 {
	fcTagMu.Lock()
	ids := orderedTags(fcTags[gangID])
	fcTagMu.Unlock()
	out := make([]uint32, 0, len(ids))
	for _, id := range ids {
		out = append(out, uint32(id))
	}
	return out
}

// tagRank orders tags as a commander calls them: 1-9, then 0, then A-Z. Anything else is not
// a call. Pure.
func tagRank(tag any) (int, bool) {
	switch v := tag.(type) {
	case int64:
		switch {
		case v >= 1 && v <= 9:
			return int(v), true
		case v == 0:
			return 10, true
		}
	case string:
		if len(v) == 1 {
			c := v[0] | 0x20
			if c >= 'a' && c <= 'z' {
				return 11 + int(c-'a'), true
			}
		}
	}
	return 0, false
}

// orderedTags sorts tagged item IDs by tagRank, then by ID. Pure.
func orderedTags(tags map[int64]any) []int64 {
	type tg struct {
		id   int64
		rank int
	}
	list := make([]tg, 0, len(tags))
	for id, t := range tags {
		if r, ok := tagRank(t); ok {
			list = append(list, tg{id, r})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].rank != list[j].rank {
			return list[i].rank < list[j].rank
		}
		return list[i].id < list[j].id
	})
	out := make([]int64, len(list))
	for i, t := range list {
		out[i] = t.id
	}
	return out
}

// ── fire state ──────────────────────────────────────────────────────────────

// fireCall is one pilot's standing fire orders.
type fireCall struct {
	// Known is set once any order reached this pilot; until then the state blob's hold-fire flag
	// stands.
	Known    bool
	HoldFire bool
	// OnCall: shoot only what the commander called (a tag, a lock under "attack", a name).
	OnCall bool
	// Slot is 2 for the secondary, otherwise the primary.
	Slot int
	// Name is an NPC named in the attack order ("primary the Pithi").
	Name string
}

var (
	fireCallMu sync.Mutex
	fireCalls  = map[int64]fireCall{}
)

func fireCallOf(charID int64) fireCall {
	fireCallMu.Lock()
	defer fireCallMu.Unlock()
	return fireCalls[charID]
}

func updateFireCall(charID int64, f func(*fireCall)) {
	fireCallMu.Lock()
	defer fireCallMu.Unlock()
	c := fireCalls[charID]
	c.Known = true
	f(&c)
	fireCalls[charID] = c
}

func dropFireCall(charID int64) {
	fireCallMu.Lock()
	delete(fireCalls, charID)
	fireCallMu.Unlock()
}

func init() {
	registerCharForget("fireCalls", dropFireCall)
}

// onCallOnly reports whether this projection shoots only what was called.
func (eb *embodiedBot) onCallOnly() bool {
	if eb == nil {
		return false
	}
	eb.mu.Lock()
	boss := eb.bossID
	eb.mu.Unlock()
	return boss != 0 && fireCallOf(eb.charID).OnCall
}

// targetNameStop are words after "attack" that name nothing.
var targetNameStop = map[string]bool{
	"it": true, "him": true, "her": true, "them": true, "that": true, "this": true, "the": true,
	"target": true, "one": true, "now": true, "please": true, "me": true, "my": true,
}

// orderTargetName is the NPC name an attack order carries, "" when the words name nothing.
func orderTargetName(rest string) string {
	var keep []string
	for _, w := range strings.Fields(strings.ToLower(rest)) {
		w = strings.Trim(w, ",.:;!?-")
		if w == "" || targetNameStop[w] {
			continue
		}
		keep = append(keep, w)
	}
	name := strings.Join(keep, " ")
	if len(name) < 3 {
		return ""
	}
	return name
}

// ── choosing ────────────────────────────────────────────────────────────────

// commandInputs is everything a commander has said about targets, already filtered to balls
// this pilot may shoot on its grid.
type commandInputs struct {
	Subject    uint32   // a linked pilot
	Named      uint32   // an NPC named in the order
	Tagged     []uint32 // the commander's tags in call order
	Slot       int      // 2: the secondary
	Locked     []uint32 // the commander's locks
	Aggressors []uint32 // whatever is shooting the fleet
}

// pickCommanded applies the priority: the linked pilot, the named NPC, the tag for this
// pilot's slot (a secondary with one tag takes the primary), the commander's locks, then the
// fleet's attackers. 0 when nothing was called. Pure.
func pickCommanded(in commandInputs) uint32 {
	if in.Subject != 0 {
		return in.Subject
	}
	if in.Named != 0 {
		return in.Named
	}
	if n := len(in.Tagged); n > 0 {
		if in.Slot == 2 && n > 1 {
			return in.Tagged[1]
		}
		return in.Tagged[0]
	}
	if len(in.Locked) > 0 {
		return in.Locked[0]
	}
	if len(in.Aggressors) > 0 {
		return in.Aggressors[0]
	}
	return 0
}

// called is every ball the commander pointed at, for the hostile list that starts a fight.
func (in commandInputs) called() []uint32 {
	var out []uint32
	for _, id := range []uint32{in.Subject, in.Named} {
		if id != 0 {
			out = append(out, id)
		}
	}
	out = append(out, in.Tagged...)
	return append(out, in.Locked...)
}

// verbTakesTags reports whether a pilot under this order shoots the commander's tags. An
// errand, a dock and a flight do not.
func verbTakesTags(verb string) bool {
	switch verb {
	case OrderMine, OrderPickOre, OrderJetcan, OrderHaul, OrderStockpile, OrderSell,
		OrderDock, OrderGoHome, OrderFlee:
		return false
	}
	return true
}

// namedOnGrid is the nearest ball on this grid whose label carries the name.
func (m *Mod) namedOnGrid(eb *embodiedBot, name string, may func(*destiny.Entity) bool) uint32 {
	if name == "" || eb.ss == nil || eb.ent == nil {
		return 0
	}
	self := eb.ent.Pos()
	var best uint32
	bestDist := -1.0
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if e == nil || !may(e) || !containsWords(strings.ToLower(ballLabel(e)), name) {
			continue
		}
		if d := e.Pos().Sub(self).Len(); bestDist < 0 || d < bestDist {
			best, bestDist = e.ID, d
		}
	}
	return best
}
