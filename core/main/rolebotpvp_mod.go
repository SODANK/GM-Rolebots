package main

import (
	"context"
	"log"
	"strconv"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/seclaw"
	"gurimoon/server"
)

// init installs the player-combat seam (modapi.PvPHooks). Each hook is a thin
// closure over a package-main function, so module ships die, get flagged and are
// policed through the player code. Overlay build only.
func init() {
	modapi.InstallPvP(modapi.PvPHooks{
		KillPlayer:           modKillPlayer,
		NotePlayerAggression: recordPlayerAggression,
		PoliceCrime:          modPoliceCrime,
		LaunchMissile:        modLaunchMissile,
		EwarLanded:           modEwarLanded,
		CriminalSince:        modCriminalSince,
		SecStatusHit:         modSecStatusHit,
		SecKillPenalty:       modSecKillPenalty,
		SecRobberyPenalty:    modSecRobberyPenalty,
		SecAggressionPenalty: modSecAggressionPenalty,
		SecRatGain:           modSecRatGain,
		SecRatFraction:       modSecRatFraction,
		SecSettings:          securitySettingsSnapshot,
		SecSetSettings:       modSecSetSettings,
		HPChanged:            modHPChanged,
		OutlawEntered:        modOutlawEntered,
		CriminalEntered:      modCriminalEntered,
		AggressionLocked:     aggressionLocked,
		InPvP:                modInPvP,
		StampPvP:             stampPvP,
		NoteTheft:            noteLootTheftFor,
		SpawnCorpse:          modSpawnCorpse,
		StructureHit:         modStructureHit,
	})
}

// modStructureHit is structureDamaged with main's DB handle, then the structure's
// death when the hit destroyed it, as the player weapon paths do.
func modStructureHit(ss *destiny.SolarSystem, target, attacker *destiny.Entity,
	weaponTypeID int64, applied float64, destroyed bool) {

	if ss == nil || target == nil || globalDB == nil {
		return
	}
	structureDamaged(ss, target, attacker, weaponTypeID, applied, destroyed, globalDB)
	if destroyed {
		handleStructureDestroyed(ss, target, attacker)
	}
}

// modSpawnCorpse is spawnPodCorpse with main's DB handle, as handlePlayerPodKilled
// calls it. Nil-safe before main wires its DB.
func modSpawnCorpse(ss *destiny.SolarSystem, charID int64, charName string, pos destiny.Vec3) {
	if globalDB == nil {
		return
	}
	spawnPodCorpse(globalDB, ss, charID, charName, pos)
}

// modOutlawEntered is onPilotEnteredSpace for a module projection, with no login
// grace. Nil-safe before main wires its DB and NPC manager.
func modOutlawEntered(e *destiny.Entity, ss *destiny.SolarSystem) {
	if globalDB == nil || globalNPCMgr == nil {
		return
	}
	onPilotEnteredSpace(e, ss, globalDB, globalNPCMgr, entryMove)
}

// modCriminalEntered is carryCriminalFlag for a projection. The caller supplies the
// start time because the aggression sweep may have dropped the old system's table.
func modCriminalEntered(e *destiny.Entity, ss *destiny.SolarSystem, since int64) {
	if e == nil || ss == nil || since <= 0 {
		return
	}
	charID := e.Slim.CharID
	now := time.Now().Unix()
	if charID <= 0 || now-since >= int64(aggressionDuration/time.Second) {
		return
	}
	t := aggroTableFor(ss.ID)
	if t.globalCriminalSince(charID, now) >= since {
		return
	}
	t.setAt(charID, 0, since)
	log.Printf("[aggro] charID=%d carried criminal timer into %d (set %ds ago)",
		charID, ss.ID, now-since)
	broadcastAggression(ss)
	concordFollow(ss, e)
}

// modInPvP is the logoff linger's PvP test (logofflinger.go) for a module
// pilot: in PvP within crimewatch.pvpLogoffMin.
func modInPvP(charID int64) bool {
	if charID <= 0 {
		return false
	}
	window := evedb.CachedSettingDuration("crimewatch.pvpLogoffMin", time.Minute, 15, 0, 60)
	return logoffLingerFor(time.Now().Unix(), pvpTimesFor(charID).pvpAt, window, 0) > 0
}

// modKillPlayer hands a destroyed player ship or capsule to its connection
// goroutine, which runs the death sequence. Returns false for a bot projection
// (IsPlayer with no Conn); its module owns that death.
func modKillPlayer(victim *destiny.Entity, pod bool) bool {
	if victim == nil || !victim.IsPlayer || victim.Conn == nil || victim.PlayerState == nil {
		return false
	}
	ds, ok := victim.PlayerState.(*destinyConn)
	if !ok || ds == nil {
		return false
	}
	queuePlayerDeath(ds, pod)
	return true
}

// modPoliceCrime is maybeTriggerConcordResponse with main's DB handle and NPC
// manager. Nil-safe: before main wires them a crime is not policed.
func modPoliceCrime(attacker, victim *destiny.Entity, ss *destiny.SolarSystem) {
	if globalDB == nil || globalNPCMgr == nil {
		return
	}
	maybeTriggerConcordResponse(attacker, victim, ss, globalDB, globalNPCMgr)
}

// modEwarLanded is the victim side of a module ship's electronic effect:
// jamPlayerTargets for a jam, pushEwarVictimStats for debuffs and
// broadcastJamEvent for the jam bar, as the player and NPC loops do.
func modEwarLanded(attacker, victim *destiny.Entity, ss *destiny.SolarSystem,
	moduleID int64, guid, kind string, durationMs int64, start bool) {

	if attacker == nil || victim == nil || ss == nil {
		return
	}
	switch kind {
	case "jam":
		if start && victim.IsPlayer {
			jamPlayerTargets(victim, durationMs)
		}
	case "damp", "trackDisrupt", "paint":
		// Start and stop alike: the numbers moved both times.
		pushEwarVictimStats(victim, globalDB)
	}
	if jt, ok := destiny.JamBucketForFXGUID(guid); ok {
		broadcastJamEvent(ss, attacker.ID, moduleID, victim.ID, jt, durationMs, start)
	}
}

// modLaunchMissile is launchMissileBallFor (posguns.go) with the flight read from
// the database the POS path uses. Without main's DB it returns 0 and false.
func modLaunchMissile(ss *destiny.SolarSystem, shooter, target *destiny.Entity,
	chargeTypeID int64) (float64, bool) {

	if ss == nil || shooter == nil || target == nil || globalDB == nil {
		return 0, false
	}
	f := posMissileFlightFor(globalDB, chargeTypeID)
	_, flight, fizzle := launchMissileBallFor(ss, shooter, target, chargeTypeID, f)
	return flight, fizzle
}

// modCriminalSince exposes the crimewatch table's global-criminal timestamp.
func modCriminalSince(sysID, charID int64) int64 {
	if charID <= 0 {
		return 0
	}
	t, ok := aggroTables.Load(sysID)
	if !ok {
		return 0
	}
	return t.globalCriminalSince(charID, time.Now().Unix())
}

// modSecStatusHit moves a character's security status. offender may be nil for an
// offline pilot; the DB row still moves.
func modSecStatusHit(offender *destiny.Entity, charID int64, delta float64,
	eventTypeID, subjectID, sysID, shipTypeID int64) {

	if globalDB == nil || charID <= 0 || delta == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applySecStatusChange(ctx, globalDB, offender, charID, delta,
		eventTypeID, subjectID, sysID, shipTypeID)
}

// modSecKillPenalty prices a module ship's kill through applyKillPenalty, the
// function the core's death handlers call, so one function decides what is a crime.
func modSecKillPenalty(offender *destiny.Entity, f seclaw.KillFacts) (float64, string) {
	if globalDB == nil {
		return 0, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applied, why := applyKillPenalty(ctx, globalDB, offender, f)
	return applied, string(why)
}

// SettingRobberyPct is the robbery cost in percent of the pirate's distance to -10
// (seclaw.LossFromGap). It is read from server_settings on every charge, so an
// edit applies at once.
const (
	SettingRobberyPct = "security.robberyPenaltyPct"

	// Half a ship kill. Applies only when the exemption ladder found a crime.
	defaultRobberyPct = 1.0
)

// modSecRobberyPenalty charges a pirate for a mark it held up and released. It
// asks seclaw.ShotIsCrime directly, because applyKillPenalty prices a death and
// logs a kill event.
func modSecRobberyPenalty(offender *destiny.Entity, f seclaw.KillFacts) (float64, string) {
	if globalDB == nil {
		return 0, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	crime, why := seclaw.ShotIsCrime(f, seclawDeps(globalDB))
	if !crime {
		return 0, string(why)
	}
	delta := applySecStatusPenalty(ctx, globalDB, offender, f.KillerCharID,
		secPctSetting(ctx, globalDB, SettingRobberyPct, defaultRobberyPct),
		evedb.EventStandingCombatAggression, f.VictimCharID, f.SystemID, f.KillerShipTypeID, "", nil)
	if delta == 0 {
		return 0, ""
	}
	log.Printf("[secstatus] robbery: charID=%d robbed %d in system %d sec=%.2f — %+.3f",
		f.KillerCharID, f.VictimCharID, f.SystemID, f.Security, delta)
	return delta, ""
}

// modSecAggressionPenalty charges a module ship for opening fire on a neutral
// pilot, with the player ladder and curve. A hook because abstract-tier fights
// never reach a damage path.
func modSecAggressionPenalty(offender *destiny.Entity, f seclaw.KillFacts) (float64, string) {
	return applyAggressionPenalty(offender, f)
}

// modSecRatGain awards a rat kill's security status for a fight the core never saw,
// the same gain the NPC death broadcast pays on a loaded grid.
func modSecRatGain(pilot *destiny.Entity, charID int64, fraction float64,
	subjectID, sysID, shipTypeID int64) {

	if globalDB == nil || charID <= 0 || fraction == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	applySecStatusGain(ctx, globalDB, pilot, charID, fraction,
		evedb.EventStandingPirateKillSecurityStatus, subjectID, sysID, shipTypeID)
}

// modSecRatFraction returns what one NPC type is worth, through npcSecFraction. The
// bounty is looked up here because it matters only for a type with no security
// attribute.
func modSecRatFraction(typeID int64) float64 {
	if globalDB == nil || typeID <= 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return npcSecFraction(ctx, globalDB, typeID, evedb.GetNPCBounty(ctx, globalDB, typeID))
}

// modSecSetSettings persists security-law settings and reloads them in one call.
// The loader applies the sign clamps, so a pod penalty entered as a positive
// number cannot become a reward.
func modSecSetSettings(vals map[string]float64) error {
	if globalDB == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for k, v := range vals {
		if err := evedb.SetServerSetting(ctx, globalDB, k,
			strconv.FormatFloat(v, 'g', -1, 64)); err != nil {
			return err
		}
	}
	loadSecuritySettings(ctx, globalDB)
	return nil
}

// modHPChanged is broadcastDamageStateTo with a fresh stamp, for a repair the
// module applied to an entity on a live grid.
func modHPChanged(ss *destiny.SolarSystem, e *destiny.Entity) {
	if ss == nil || e == nil {
		return
	}
	dmg := e.DamageStateFrac()
	dmg.Timestamp = server.Win32TimeNow()
	broadcastDamageStateTo(ss, e.ID, dmg)
}
