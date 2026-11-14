package rolebots

import (
	"context"
	"math"
)

// Pack duels: a fleet on a belt fights as a fleet.
//
// Whoever draws the spawn brings every gangmate in the same system on a patrol posture. The
// pre-fight look weighs the pooled side, the rats focus the weakest hull, the pack's guns focus
// nextRatTarget, and the bounty splits by damage dealt. A solo pilot is the one-member case of
// the same loop, so the module has one duel, not two.

// packMember is one hull on the pilot's side of an abstract fight.
type packMember struct {
	Run *botRun
	Bot *abstractCombatant
	// LandedFrac is how much of what the rats throw at this hull lands: its own electronics and
	// reach against this spawn (ewarabstract.go, rangeabstract.go). It is per member because hulls
	// on one belt do not hold the same share of the fight off.
	LandedFrac float64
	// Dealt is the damage this hull put into the rats, for the bounty split.
	Dealt float64

	// Leaving is set once this hull has decided to get out, on tick 0 with its side or at its
	// break-off line. It still shoots (abstractAligningFireFrac) and is shot at until Escaped.
	Leaving bool
	Escaped bool
	// BrokeOffAt is this hull's integrity when it left mid-fight, or -1 if it never did.
	BrokeOffAt float64

	// Commander is the hull whose nerve the side fights on (packDecider). When it crosses its
	// break-off line the whole side warps out together on one align gamble. Unset on a lone pilot.
	Commander bool
	// FleetCall is set on a hull that left on the commander's call (BrokeOffAt stays -1).
	FleetCall bool
}

// markCommander flags the deciding hull of a side of two or more, so its break-off is the
// side's (resolvePackDuel). A lone pilot needs no call.
func markCommander(pack []*packMember, decider *botRun) {
	if len(pack) < 2 || decider == nil {
		return
	}
	for _, p := range pack {
		p.Commander = p.Run == decider
	}
}

// onField reports a hull still in the fight: alive and not yet warped out.
func (p *packMember) onField() bool {
	return p != nil && p.Bot != nil && p.Bot.EHP > 0 && !p.Escaped
}

// packOutcome is what resolvePackDuel hands back.
type packOutcome struct {
	Killed int
}

// resolvePackDuel fights a spawn out second by second against a side. It is pure (the roll is
// passed in) and mutates the members and the rats in place. declined is the side's tick-0
// verdict: every hull starts aligning. See docs/comment-notes-b03.md#pack-duel-rules.
func resolvePackDuel(pack []*packMember, spawn []*ratThreat, breakOff float64,
	declined bool, roll func() float64) packOutcome {

	var out packOutcome
	hasCommander := false
	for _, p := range pack {
		p.BrokeOffAt = -1
		p.Leaving = declined
		p.FleetCall = false
		hasCommander = hasCommander || p.Commander
	}
	// A side with a commander that declines on tick 0 leaves as one fleet.
	fleetOut := declined && hasCommander

	for tick := 0; tick < abstractDuelMaxTicks; tick++ {
		alive, incoming := spawnStanding(spawn)
		if alive == 0 {
			break // the belt IS clear
		}
		target := weakestPackMember(pack)
		if target == nil {
			break // nobody left on the field
		}

		landed := incoming * target.LandedFrac
		share := 1.0
		if landed > target.Bot.EHP {
			share = target.Bot.EHP / landed
		}
		for _, r := range spawn {
			if r.EHP > 0 {
				r.Dealt += r.DPS * target.LandedFrac * share
			}
		}
		target.Bot.EHP -= landed
		if target.Bot.EHP < 0 {
			target.Bot.EHP = 0
		}

		// Outgoing fire, hull by hull, each onto whatever should die next, so a rat killed by the
		// first gun this tick is not shot again by the second.
		for _, p := range pack {
			if !p.onField() || !p.Bot.Armed || p.Bot.DPS <= 0 {
				continue
			}
			t := nextRatTarget(spawn)
			if t == nil {
				break
			}
			shot := p.Bot.DPS
			if p.Leaving {
				shot *= abstractAligningFireFrac
			}
			p.Dealt += math.Min(shot, t.EHP)
			t.EHP -= shot
			if t.EHP <= 0 {
				t.EHP = 0
				out.Killed++
			}
		}

		// Repair after the volleys: each hull's own booster or repairer on itself, and the side's
		// pooled remote repair on whoever is most wounded (the resolveTeamFight rule).
		packRepairTick(pack)

		// Getting out is a decision, then a gamble on the align.
		for _, p := range pack {
			if !p.onField() || p.Leaving {
				continue
			}
			if p.Bot.MaxEHP > 0 && p.Bot.EHP/p.Bot.MaxEHP < breakOff {
				p.Leaving = true
				p.BrokeOffAt = p.Bot.EHP / p.Bot.MaxEHP
				if p.Commander {
					fleetOut = true
				}
			}
		}
		if fleetOut {
			// The commander has called it: everyone still on the field aligns with it and the fleet warps
			// on one roll. The rats keep shooting the weakest hull while it aligns.
			anyOn := false
			for _, p := range pack {
				if p.onField() {
					anyOn = true
					if !p.Leaving {
						p.Leaving, p.FleetCall = true, true
					}
				}
			}
			if anyOn && roll() < abstractEscapePerTick {
				for _, p := range pack {
					if p.onField() {
						p.Escaped = true
					}
				}
			}
			continue
		}
		for _, p := range pack {
			if p.onField() && p.Leaving && roll() < abstractEscapePerTick {
				p.Escaped = true
			}
		}
	}
	return out
}

// weakestPackMember is the hull still on the field with the least left: where a wave's fire
// goes.
func weakestPackMember(pack []*packMember) *packMember {
	var w *packMember
	for _, p := range pack {
		if !p.onField() {
			continue
		}
		if w == nil || p.Bot.EHP < w.Bot.EHP {
			w = p
		}
	}
	return w
}

// packBountyShares is the side's claim on the pot: each pilot by the damage it put into the
// rats. A side that dealt nothing measurable splits evenly rather than paying nobody.
func packBountyShares(pack []*packMember) []bountyShare {
	parts := make([]bountyShare, 0, len(pack))
	total := 0.0
	for _, p := range pack {
		total += p.Dealt
	}
	for _, p := range pack {
		w := p.Dealt
		if total <= 0 {
			w = 1
		}
		if w > 0 {
			parts = append(parts, bountyShare{CharID: p.Run.CharID, Weight: w})
		}
	}
	return parts
}

// packMatesFor is everyone who fights beside b when it is jumped: gangmates in the same
// system, in space, unwatched, and on a patrol posture. It matches by system, not belt: a
// mate one belt over is a warp away, inside the one-roll-per-90 s resolution of this tier.
func (m *Mod) packMatesFor(ctx context.Context, b *botRun) []*botRun {
	if b == nil || b.State == nil || b.State.Data.GangID == 0 {
		return nil
	}
	if !postureFor(b.State.Phase).patrol() {
		return nil
	}
	roster, err := m.gangCombatRoster(ctx, b.State.Data.GangID, b.CharID)
	if err != nil {
		return nil
	}
	var out []*botRun
	for _, mate := range roster {
		if mate.SystemID != b.SystemID {
			continue
		}
		run, err := m.loadRun(ctx, mate.CharID)
		if err != nil || run == nil || run.State == nil {
			continue
		}
		if run.SystemID != b.SystemID || run.State.Data.GangID != b.State.Data.GangID {
			continue
		}
		if !postureFor(run.State.Phase).patrol() || !m.exposedInSpace(run) {
			continue
		}
		// Rats never shoot pods (combat_abstract.go), so a pod is never in the fight. This drops a
		// copy that still says it is patrolling after a pod swap (stateepoch.go).
		if m.isCapsule(ctx, run.ShipID) {
			continue
		}
		out = append(out, run)
	}
	return out
}

// packDecider is whose nerve the side fights on: the gang's commander when it is in the fight
// (one fleet, one decision, as combat_engage.go), otherwise the pilot that drew the spawn.
func packDecider(drawer *botRun, pack []*packMember, leaderID int64) *botRun {
	if leaderID != 0 {
		for _, p := range pack {
			if p.Run.CharID == leaderID {
				return p.Run
			}
		}
	}
	return drawer
}

// stampPackExposure restarts the encounter clock of every member but the one that drew, so
// their own rolls on the next pass do not fight the same wave a second time.
func stampPackExposure(pack []*packMember, drawer int64) {
	exposureMu.Lock()
	defer exposureMu.Unlock()
	for _, p := range pack {
		if p.Run.CharID == drawer {
			continue
		}
		exposureAt[p.Run.CharID] = clock.Now()
	}
}

// packRepairTick lands one second of repair on a side. Raw hitpoints become effective ones at
// each hull's resist ratio, since the duel's hitpoints are effective and repair mends raw HP.
func packRepairTick(pack []*packMember) {
	pool := 0.0
	for _, p := range pack {
		if !p.onField() {
			continue
		}
		pool += p.Bot.RepPerSec
		if p.Bot.SelfRepPerSec > 0 {
			p.Bot.EHP = math.Min(p.Bot.MaxEHP, p.Bot.EHP+p.Bot.SelfRepPerSec*ehpPerHP(p.Bot))
		}
	}
	if pool <= 0 {
		return
	}
	var worst *packMember
	worstFrac := 1.0
	for _, p := range pack {
		if !p.onField() || p.Bot.MaxEHP <= 0 {
			continue
		}
		if f := p.Bot.EHP / p.Bot.MaxEHP; f < worstFrac {
			worst, worstFrac = p, f
		}
	}
	if worst != nil {
		worst.Bot.EHP = math.Min(worst.Bot.MaxEHP, worst.Bot.EHP+pool*ehpPerHP(worst.Bot))
	}
}

// ehpPerHP is how many effective hitpoints one raw hitpoint of repair is worth on this hull:
// its resist ratio, 1 when its pools were never read.
func ehpPerHP(c *abstractCombatant) float64 {
	raw := c.MaxHP[0] + c.MaxHP[1] + c.MaxHP[2]
	if raw <= 0 || c.MaxEHP <= 0 {
		return 1
	}
	return c.MaxEHP / raw
}
