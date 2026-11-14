package rolebots

import "gurimoon/destiny"

// The fleet commander's primary target.
//
// A member shoots the commander's current target when its own rules of engagement
// already count it hostile and it is alive, visible and inside weapon reach.
// Otherwise the member uses its own target scorer. The commander never reads its
// own call.

// choosePrimary returns leaderTarget when it is in hostiles and usable, else 0.
// usable answers the per-ball questions (alive, visible, in reach), so the rule
// can be tested without a solar system.
func choosePrimary(leaderTarget uint32, hostiles []uint32, usable func(uint32) bool) uint32 {
	if leaderTarget == 0 {
		return 0
	}
	for _, id := range hostiles {
		if id == leaderTarget {
			if usable != nil && !usable(id) {
				return 0
			}
			return id
		}
	}
	return 0
}

// gangPrimary returns the commander's target for one member, or 0 when the
// member cannot follow a call.
func (m *Mod) gangPrimary(eb *embodiedBot, l *combatLoadout, hostiles []uint32) uint32 {
	if eb == nil || eb.ent == nil || eb.ss == nil || l == nil {
		return 0
	}
	eb.mu.Lock()
	leaderID, gangID := eb.gangLeader, eb.gangID
	eb.mu.Unlock()
	if gangID == 0 || leaderID == 0 || leaderID == eb.charID {
		return 0
	}
	embodyMu.Lock()
	lead := embodyLive[leaderID]
	embodyMu.Unlock()
	if lead == nil || lead.systemID != eb.systemID {
		return 0
	}
	lead.mu.Lock()
	call, fighting, gone := lead.combatTarget, lead.fighting, lead.dead
	// A commander under a fleet command calls its primary (fleetcommand_grid.go),
	// which outranks its own lock; under a split, this member's half may be on the secondary.
	if lead.fleetCmd != nil && lead.call.primary != 0 {
		call = lead.call.primaryFor(eb.charID)
	}
	lead.mu.Unlock()
	if !fighting || gone {
		return 0
	}

	selfPos := eb.ent.Pos()
	reach := l.targetRnge * eb.ent.EwarTotals().MaxTargetRangeMult
	return choosePrimary(call, hostiles, func(id uint32) bool {
		var e *destiny.Entity = eb.ss.GetEntity(id)
		if e == nil || e.IsDestroyed() || e.IsInWarp() || e.IsCloaked() {
			return false
		}
		return e.Pos().Sub(selfPos).Len() <= reach
	})
}
