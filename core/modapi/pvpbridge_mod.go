package modapi

import (
	"gurimoon/destiny"
	"gurimoon/seclaw"
)

// PvPHooks is the player-combat seam. Package main installs it from init() through
// InstallPvP and core never reads it back. Every field may be nil, so a caller
// checks the one it needs. Overlay build only.
type PvPHooks struct {
	// KillPlayer ends a player's ship or capsule through the core death path. Returns
	// false when the victim is not a live player connection (bot, drone, NPC); the
	// caller then handles the death itself.
	KillPlayer func(victim *destiny.Entity, pod bool) bool

	// NotePlayerAggression records attacker and victim in the system's crimewatch
	// table. The victim's client needs it to paint the attacker as the aggressor.
	NotePlayerAggression func(attacker, victim *destiny.Entity, ss *destiny.SolarSystem)

	// PoliceCrime runs the legality gate: the global criminal flag in empire space and,
	// above 0.45 security, the CONCORD response. Same function the player damage
	// paths call.
	PoliceCrime func(attacker, victim *destiny.Entity, ss *destiny.SolarSystem)

	// EwarLanded delivers the victim side of an electronic effect a module ship started
	// or lifted: jam, damp, tracking disruptor, painter and the jam bar. kind is one of
	// scram, web, jam, damp, trackDisrupt, paint.
	EwarLanded func(attacker, victim *destiny.Entity, ss *destiny.SolarSystem,
		moduleID int64, guid, kind string, durationMs int64, start bool)

	// LaunchMissile draws the missile ball of a module ship's shot on every client that
	// holds both balls and returns the seconds to wait before landing damage. fizzle
	// means the shot cannot reach and lands nothing.
	LaunchMissile func(ss *destiny.SolarSystem, shooter, target *destiny.Entity,
		chargeTypeID int64) (flightSec float64, fizzle bool)

	// CriminalSince returns the unix time a character's global criminal flag was raised
	// in a system, or 0 for none. The flag lasts 15 minutes and follows the pilot.
	CriminalSince func(sysID, charID int64) int64

	// SecStatusHit moves security status by an absolute delta. eventTypeID is a
	// db.EventStanding* constant. Deprecated: use SecKillPenalty for a kill and
	// SecRatGain for a rat.
	SecStatusHit func(offender *destiny.Entity, charID int64, delta float64,
		eventTypeID, subjectID, sysID, shipTypeID int64)

	// SecKillPenalty prices one kill with the core's exemption ladder and curve, so a
	// lawful kill costs nothing. offender may be nil; the record still moves. Returns
	// the delta applied and, when none, the exemption that spared the killer.
	SecKillPenalty func(offender *destiny.Entity, f seclaw.KillFacts) (float64, string)

	// SecAggressionPenalty charges a pilot for opening fire on a neutral, with the
	// SecKillPenalty ladder. The core charges once per attacker and victim pair per
	// criminal window; a repeat call returns 0 with no exemption named.
	SecAggressionPenalty func(offender *destiny.Entity, f seclaw.KillFacts) (float64, string)

	// SecRobberyPenalty prices a robbery that took no life, with the SecKillPenalty
	// ladder at security.robberyPenaltyPct of the gap. Logged as
	// db.EventStandingCombatAggression. A percent of 0 turns the charge off.
	SecRobberyPenalty func(offender *destiny.Entity, f seclaw.KillFacts) (float64, string)

	// SecRatGain awards a rat kill's security status as a fraction of the remaining
	// gap. fraction is seclaw.RatFraction summed over the rats the pilot's share
	// covers; call once per pilot per engagement.
	SecRatGain func(pilot *destiny.Entity, charID int64, fraction float64,
		subjectID, sysID, shipTypeID int64)

	// SecRatFraction returns what one NPC type is worth to SecRatGain, 0 when it pays
	// nothing. One owner for the rule keeps modules in step with the core payout.
	SecRatFraction func(typeID int64) float64

	// SecSettings reads the live security-law settings; SecSetSettings writes and
	// reloads them. The map is keyed by server_settings key. The reload applies the
	// sign clamps and needs no restart.
	SecSettings    func() map[string]float64
	SecSetSettings func(vals map[string]float64) error

	// HPChanged tells every witness and the ship's own pilot that an entity's hitpoints
	// moved without a shot, such as a module's remote repair. Wraps the core's
	// broadcastDamageStateTo.
	HPChanged func(ss *destiny.SolarSystem, e *destiny.Entity)

	// OutlawEntered runs the core's space-entry rules (onPilotEnteredSpace) on a
	// projection that just entered: an outlaw raises no flag, a pilot with a criminal
	// flag meets CONCORD. Call it after the entity is in the system.
	OutlawEntered func(e *destiny.Entity, ss *destiny.SolarSystem)

	// CriminalEntered re-raises a global criminal flag on a projection entering a
	// system, keeping the original start time (unix seconds), and sends CONCORD in
	// police space. No-op for a lapsed or zero since.
	CriminalEntered func(e *destiny.Entity, ss *destiny.SolarSystem, since int64)

	// AggressionLocked reports whether the crimewatch lock after a PvP act still bars
	// the character from docking or jumping.
	AggressionLocked func(charID int64) bool

	// InPvP reports whether a character was in PvP, either side, within
	// crimewatch.pvpLogoffMin. A bot may not log off inside that window.
	InPvP func(charID int64) bool

	// StampPvP records a PvP act settled without entities: the aggressor's dock and
	// jump lock and both pilots' PvP time. victimCharID may be 0.
	StampPvP func(aggressorCharID, victimCharID int64)

	// NoteTheft makes thief an aggressor of the container owner and the owner's corp
	// for the crimewatch window. thiefCorpID 0 is resolved by the core, which also
	// excuses the owner, their corp and gang mates. The module excuses its own fleet.
	NoteTheft func(ss *destiny.SolarSystem, thiefCharID, thiefCorpID, ownerID int64)

	// StructureHit runs the core's rules for a hit on a player-owned structure, as a
	// player's guns do: kill credit, the tower guns' aggression memory, CONCORD, saved
	// damage and the tower's reinforcement floor. destroyed also runs the structure's
	// death. attacker may be nil for damage with no shooter.
	StructureHit func(ss *destiny.SolarSystem, target, attacker *destiny.Entity,
		weaponTypeID int64, applied float64, destroyed bool)

	// SpawnCorpse leaves a pilot's Frozen Corpse ball at pos in a loaded system, as the
	// core's spawnPodCorpse does. Blocks on a DB write; call it off any combat lock.
	SpawnCorpse func(ss *destiny.SolarSystem, charID int64, charName string, pos destiny.Vec3)
}

var pvpHooks PvPHooks

// InstallPvP publishes the core's player-combat entry points. Package main calls it
// once from init(); a later call overwrites.
func InstallPvP(h PvPHooks) { pvpHooks = h }

// PvP returns the installed hooks. Any field may be nil; check the one you need.
func PvP() PvPHooks { return pvpHooks }
