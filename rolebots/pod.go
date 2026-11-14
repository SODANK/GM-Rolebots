package rolebots

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/seclaw"
)

// Death and what follows. A rolebot dies in two stages, as a player does. First the hull:
// the ship and its fittings stop existing, a capsule appears at the wreck and the pilot is
// in it. The abstract sim flies the capsule, the embodied tier projects it, and the combat
// guard runs from it (no high slots).
// If the capsule dies too, the clone is spent: the pilot wakes at its medical station with an
// Alpha clone, which no longer covers its skill points, so clone.go makes it buy cover first.
// Belt rats never pod anyone (the NPC brain skips group 29), so the capsule stage turns
// fatal only when a player kills it.

const (
	capsuleGroupID = 29
	capsuleTypeID  = 670
	// Clone Grade Alpha: what a spent clone is replaced by, and the floor
	// every pilot starts from.
	cloneGradeAlphaTypeID = 164
	// Fallback medical station for a bot with no home and no profile home: the same Todaki
	// the server's own death path uses.
	fallbackHomeStationID = 60004420
)

// ── how often the capsule dies too ──────────────────────────────────────────

// settingPodKill is the base odds a pilot finishes a capsule after killing its ship, before
// its traits move it (tuning.go). It is a live dial, so an operator changes it without a
// rebuild.
const settingPodKill = "rolebots.combat.podKillChance"

// podKillV caches the dial in hundredths. 0 means nobody has set it and the shipped default
// stands; an operator's explicit 0 is stored as -1.
var podKillV atomic.Int64

func podKillBase() float64 {
	switch v := podKillV.Load(); {
	case v < 0:
		return 0
	case v > 0:
		return float64(v) / 100
	}
	return podKillChanceDefault
}

func clampPodKill(v float64) float64 { return clampRange(v, 0, 1) }

// loadPodKillChance reads the dial at Init. Never fatal: an unreadable dial leaves the
// shipped default.
func (m *Mod) loadPodKillChance(ctx context.Context) {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingPodKill)
	if err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingPodKill, err)
		return
	}
	if !ok {
		return
	}
	f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if perr != nil || f < 0 {
		return
	}
	if f = clampPodKill(f); f == 0 {
		podKillV.Store(-1)
		return
	}
	podKillV.Store(int64(f * 100))
}

// isCapsule reports whether a boarded "ship" is actually the pilot's pod.
func (m *Mod) isCapsule(ctx context.Context, shipID int64) bool {
	var groupID int64
	err := m.db.QueryRowContext(ctx, `
		SELECT t.groupID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.itemID = ?`, shipID).Scan(&groupID)
	if err != nil {
		// Unknown is treated as a ship: losing a hull is the recoverable
		// branch, and guessing "pod" would burn a clone for nothing.
		return false
	}
	return groupID == capsuleGroupID
}

// handleShipLoss runs when a rolebot's hull is destroyed in space. The ship and
// its fittings are gone for good, a capsule takes its place at the wreck, and
// the sim is put back to planning so the bot flies home and re-procures.
func (m *Mod) handleShipLoss(ctx context.Context, eb *embodiedBot, killerID uint32) {
	// Its drones go with it. Before loseHull, which deletes the ship row the
	// core would otherwise scoop them back into at system teardown.
	m.loseDrones(eb, "their ship was destroyed")
	pos := eb.ent.Pos()
	killer := "something"
	if killerID != 0 {
		killer = fmt.Sprintf("entity %d", killerID)
	}
	// An embodied bot is IsPlayer, so the core's damage paths fill its hit table. Snapshot it
	// before loseHull deletes what it refers to.
	hits := eb.ent.KillHitsSnapshot()
	m.chargeKillerForBotDeath(eb, hits, false)
	m.noteGraceLoss(ctx, eb, hits, false)
	m.chatterOwnLoss(eb, hits)
	m.loseHull(ctx, eb.charID, eb.charName, eb.shipID, eb.systemID,
		pos.X, pos.Y, pos.Z, killer, hits)
}

// chargeKillerForBotDeath bills whoever killed this bot, through the core law that bills a
// bot for killing someone else. The core never sees this death (modKillPlayer refuses an
// entity with no connection), so without it a player pays nothing for farming bots in
// high-sec. A death with no player behind it (rats, sentries, CONCORD) charges nobody, and
// neither does a starbase gun: it fires under its owner's Defense tab, which is no crime.
func (m *Mod) chargeKillerForBotDeath(eb *embodiedBot, hits []destiny.KillHit, pod bool) {
	if eb == nil {
		return
	}
	what := "ship"
	if pod {
		what = "pod"
	}
	killer, ok := topPlayerHit(hits, eb.charID)
	if !ok {
		if sb, found := starbaseHit(hits, m.playerCorpTest()); found {
			m.logBot(eb.charID, logInfo, "security",
				"our %s fell to %q, a starbase of corp %d: no charge", what, sb.Name, sb.CorpID)
		}
		return
	}
	hit := modapi.PvP().SecKillPenalty
	if hit == nil {
		return
	}
	var victimAllianceID int64
	var victimOutlaw bool
	if eb.ent != nil {
		victimAllianceID = eb.ent.Slim.AllianceID
		// An outlaw is a legal target for everybody. A bot that put itself
		// outside the law is not a pilot anyone gets charged for killing.
		victimOutlaw = eb.ent.Slim.SecurityStatus <= seclaw.OutlawStatus
	}
	security := 0.0
	if eb.ss != nil {
		security = eb.ss.Security
	}
	var offender *destiny.Entity
	if eb.ss != nil {
		for _, p := range eb.ss.GetAllPlayersInSystem() {
			if p.Slim.CharID == killer.CharID {
				offender = p
				break
			}
		}
	}
	applied, why := hit(offender, seclaw.KillFacts{
		KillerCharID:     killer.CharID,
		KillerCorpID:     killer.CorpID,
		KillerAllianceID: killer.AllianceID,
		KillerShipTypeID: killer.ShipTypeID,
		VictimCharID:     eb.charID,
		VictimCorpID:     eb.corpID,
		VictimAllianceID: victimAllianceID,
		VictimOutlaw:     victimOutlaw,
		SystemID:         eb.systemID,
		Security:         security,
		Pod:              pod,
	})
	if applied != 0 {
		m.logBot(eb.charID, logInfo, "security",
			"killer %d docked %+.2f for our %s", killer.CharID, applied, what)
	} else if why != "" {
		m.logBot(eb.charID, logInfo, "security",
			"our %s loss was a lawful kill (%s)", what, why)
	}
}

// topPlayerHit picks the pilot to charge for a death: the final blow, else the
// biggest damage dealer. NPC hits and the victim's own entry are skipped, so a
// bot killed by rats or by the police charges nobody.
func topPlayerHit(hits []destiny.KillHit, victimCharID int64) (destiny.KillHit, bool) {
	var best destiny.KillHit
	found := false
	for _, h := range hits {
		if h.IsNPC || h.CharID <= 0 || h.CharID == victimCharID {
			continue
		}
		if h.FinalBlow {
			return h, true
		}
		if !found || h.Damage > best.Damage {
			best, found = h, true
		}
	}
	return best, found
}

// starbaseHit picks the starbase gun or battery behind a death: a hit with no pilot, recorded
// as non-player, from something a player corporation owns (core posguns.go records the gun
// with its owner's corporation). Final blow first, else the biggest damage dealer.
// isPlayerCorp tells a tower owner from a rat's faction corporation.
func starbaseHit(hits []destiny.KillHit, isPlayerCorp func(int64) bool) (destiny.KillHit, bool) {
	var best destiny.KillHit
	found := false
	for _, h := range hits {
		if !isStarbaseAttacker(h.IsNPC, h.CharID, h.CorpID, isPlayerCorp) {
			continue
		}
		if h.FinalBlow {
			return h, true
		}
		if !found || h.Damage > best.Damage {
			best, found = h, true
		}
	}
	return best, found
}

// isStarbaseAttacker reports whether an attacker is a structure a player corporation owns:
// non-player, no character, a corporation that is not one of the game's own.
func isStarbaseAttacker(isNPC bool, charID, corpID int64, isPlayerCorp func(int64) bool) bool {
	return isNPC && charID == 0 && corpID != 0 && isPlayerCorp != nil && isPlayerCorp(corpID)
}

// playerCorpTest is isPlayerCorpCached with its own deadline, for callers with no context.
func (m *Mod) playerCorpTest() func(int64) bool {
	return func(corpID int64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), relHookTimeout)
		defer cancel()
		return m.isPlayerCorpCached(ctx, corpID)
	}
}

// loseHull is the ship-loss path both tiers share: the hull and contents stop existing, a
// capsule takes their place at the wreck, and the sim re-plans from a pilot with no ship.
// hits is the attacker snapshot and must not be empty for a death with a cause. It returns
// the capsule's itemID, or 0 when none exists (combat_botvbot.go needs it).
func (m *Mod) loseHull(ctx context.Context, charID int64, charName string,
	shipID, systemID int64, x, y, z float64, killer string, hits []destiny.KillHit) int64 {

	// The hull's type has to be read before destroyShipAndContents deletes it.
	shipTypeID := m.entityTypeID(ctx, shipID)

	// A pilot already in a capsule has no hull to lose: the kill is the pod. Return 0 so
	// callers do not roll to pod a second capsule.
	if shipTypeID == capsuleTypeID {
		m.podBotAbstract(ctx, charID, charName, systemID, shipID, x, y, z, killer, hits)
		return 0
	}

	m.logBot(charID, logError, "combat",
		"SHIP LOST — %s destroyed by %s in system %d. Ejecting into capsule.",
		m.shipLabel(ctx, shipID), killer, systemID)

	// Pend Insurance pays out while the hull row still exists (botinsure.go).
	m.claimHullInsurance(ctx, charID, shipID, shipTypeID)
	// A corporation kit lent for the op is the corporation's loss, not an
	// SRP claim (corparmory_issue.go).
	m.noteKitLost(ctx, shipID)

	// Forget the wreck on every fit this pilot held it under, so the
	// procurement FSM sees "no ship" instead of a dead itemID.
	if _, err := m.db.ExecContext(ctx, `
		DELETE FROM rolebot_fit_ships
		WHERE characterID = ? AND shipItemID = ?`, charID, shipID); err != nil {
		rlogf("rolebots: clear lost ship %d: %v", shipID, err)
	}

	// Some of it survives, in a container at the wreck (piracy_loot.go). The roll is the core's
	// own (botLootDropChance x rates.LootDrop per item), so a bot wreck pays like a player's.
	canID, dropped, destroyed := m.dropWreckLoot(ctx, charID, shipID, systemID, x, y, z)
	// The killer's pass reads it back and goes to empty it (raid.go).
	noteLastWreck(charID, canID, systemID)

	// File the killmail as for a player loss. The core never sees this death, and
	// sendKillReportMails needs a live connection.
	m.fileKillmail(charID, charName, systemID, shipTypeID, x, y, z, false, hits, dropped, destroyed)

	// Whatever did not survive. Items moved into the container above are no longer inside the
	// hull, so this deletes exactly the losses.
	m.destroyShipAndContents(ctx, charID, shipID)

	// The hold went with it, including the salvage ledger (beltloot.go): a ledger left
	// standing would send the pilot to list items that no longer exist.
	m.clearLootLedger(ctx, charID)
	// The co-op ore claims riding in this hold go too. rolebot_ore_hold stays: that ore sits
	// in a hangar, which a death does not touch.
	m.clearGangOreClaimsFor(ctx, charID)

	// Into the pod, at the wreck.
	capsuleID, err := evedb.CreateCapsuleEntity(ctx, m.db, charID, systemID, charName)
	if err != nil {
		rlogf("rolebots: create capsule for %d: %v", charID, err)
		// No pod means no ship at all: send the pilot home rather than leave a character entity
		// pointing at a deleted hull.
		m.respawnAtHome(ctx, charID, charName, false)
		return 0
	}
	if err := evedb.UpdateEntityPosition(ctx, m.db, capsuleID, x, y, z); err != nil {
		rlogf("rolebots: place capsule %d: %v", capsuleID, err)
	}
	if err := evedb.MoveCharacterIntoShip(ctx, m.db, charID, capsuleID); err != nil {
		rlogf("rolebots: board capsule %d: %v", capsuleID, err)
	}

	// Re-plan from scratch: the next pass finds a pilot in a pod with no assigned ship and
	// goes shopping.
	if err := resetBot(ctx, m.db, charID); err != nil {
		rlogf("rolebots: reset after ship loss: %v", err)
	}
	return capsuleID
}

// podBotAbstract finishes a capsule in the tier that has no grid. A rolebot's pod never
// dies through the core: handlePodLoss does it all from the position and hit table the
// caller already has. x/y/z are the wreck's, so the killmail lands where the ship died.
func (m *Mod) podBotAbstract(ctx context.Context, charID int64, charName string,
	systemID, capsuleID int64, x, y, z float64, killer string, hits []destiny.KillHit) {

	if capsuleID == 0 {
		return
	}
	m.logBot(charID, logError, "combat",
		"PODDED — capsule destroyed by %s in system %d.", killer, systemID)
	// The implants die with the pod and go on the report as the only items
	// a capsule carries (implantupkeep.go, through the core's own helper).
	lost := m.loseImplantsOnPod(ctx, charID)
	m.fileKillmail(charID, charName, systemID, capsuleTypeID, x, y, z, true, hits, nil, lost)
	if err := evedb.DeleteEntity(ctx, m.db, capsuleID); err != nil {
		rlogf("rolebots: delete dead capsule %d: %v", capsuleID, err)
	}
	m.leaveCorpse(charID, charName, systemID, x, y, z)
	// spendClone is true: the clone grade is the one cost of dying that insurance does not
	// cover.
	m.respawnAtHome(ctx, charID, charName, true)
}

// handlePodLoss runs when the capsule itself is destroyed. The clone is spent:
// the pilot wakes at its medical station in a fresh pod, its grade falls back
// to Alpha, and clone.go makes it buy the cover it needs before it flies again.
func (m *Mod) handlePodLoss(ctx context.Context, eb *embodiedBot, killerID uint32) {
	if killerID != 0 {
		m.logBot(eb.charID, logError, "combat",
			"PODDED — capsule destroyed by entity %d in system %d.", killerID, eb.systemID)
	} else {
		m.logBot(eb.charID, logError, "combat",
			"PODDED — capsule destroyed in system %d.", eb.systemID)
	}
	pos := eb.ent.Pos()
	hits := eb.ent.KillHitsSnapshot()
	m.chargeKillerForBotDeath(eb, hits, true)
	m.noteGraceLoss(ctx, eb, hits, true)
	lost := m.loseImplantsOnPod(ctx, eb.charID)
	m.fileKillmail(eb.charID, eb.charName, eb.systemID, capsuleTypeID,
		pos.X, pos.Y, pos.Z, true, hits, nil, lost)

	// The dead pod is not coming back.
	if err := evedb.DeleteEntity(ctx, m.db, eb.shipID); err != nil {
		rlogf("rolebots: delete dead capsule %d: %v", eb.shipID, err)
	}
	m.leaveCorpse(eb.charID, eb.charName, eb.systemID, pos.X, pos.Y, pos.Z)
	m.respawnAtHome(ctx, eb.charID, eb.charName, true)
}

// leaveCorpse puts the pilot's body where its pod blew: the scoopable Frozen Corpse Biomass
// ball the core leaves for a podded player (cmd/gurimoon/corpse.go). A loaded system uses
// the core's spawn; elsewhere the row alone is the corpse and the hourly sweep expires it.
// It runs in its own goroutine, so a failure costs the body, never the respawn.
func (m *Mod) leaveCorpse(charID int64, charName string, systemID int64, x, y, z float64) {
	if systemID == 0 || (x == 0 && y == 0 && z == 0) {
		return
	}
	pos := destiny.Vec3{X: x, Y: y, Z: z}
	m.goSafeBot(charID, "pod corpse", func() {
		if spawn := modapi.PvP().SpawnCorpse; spawn != nil && m.systems != nil {
			if ss := m.systems.Get(systemID); ss != nil {
				spawn(ss, charID, charName, pos)
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := evedb.CreateCorpse(ctx, m.db, charID, systemID,
			evedb.CorpseName(charName), x, y, z); err != nil {
			rlogf("rolebots: corpse for %d: %v", charID, err)
		}
	})
}

// respawnAtHome puts a pilot back at its medical station in a fresh capsule.
// spendClone marks a real pod death, which costs the clone grade; the false
// case is the recovery path for a ship loss that could not produce a pod.
func (m *Mod) respawnAtHome(ctx context.Context, charID int64, charName string, spendClone bool) {
	homeStationID := m.homeStationFor(ctx, charID)

	loc, err := evedb.GetStationLocation(ctx, m.db, homeStationID)
	if err != nil {
		rlogf("rolebots: home station %d: %v", homeStationID, err)
		return
	}

	capsuleID, err := evedb.CreateCapsuleEntity(ctx, m.db, charID, homeStationID, charName)
	if err != nil {
		rlogf("rolebots: respawn capsule for %d: %v", charID, err)
		return
	}
	if err := evedb.MoveCharacterIntoShip(ctx, m.db, charID, capsuleID); err != nil {
		rlogf("rolebots: respawn board %d: %v", capsuleID, err)
	}
	if err := evedb.UpdateCharacterStation(ctx, m.db, charID, homeStationID,
		loc.SolarSystemID, loc.ConstellationID, loc.RegionID); err != nil {
		rlogf("rolebots: respawn location %d: %v", charID, err)
	}

	if spendClone {
		// The clone that carried the skills is used up, through the core's rule for players: SP
		// above its cap comes off the biggest skill (at most one level) and the grade drops to
		// Alpha. Training is stopped around it so the restart aims at the new level.
		var death evedb.CloneDeathResult
		if _, _, err := evedb.WithTrainingRetimed(ctx, m.db, charID, func() error {
			var derr error
			death, derr = evedb.ApplyCloneDeath(ctx, m.db, charID)
			return derr
		}); err != nil {
			rlogf("rolebots: spend clone %d: %v", charID, err)
		}
		// The cached "cover is fine" answer and the skill sheet are now wrong.
		forgetCloneCheck(charID)
		markCloneSpent(charID)
		if death.SPLost > 0 {
			forgetCharSkills(charID)
			m.logBot(charID, logError, "clone",
				"clone spent — woke up at %s on a Grade Alpha clone; %s covered too little and %d SP came off %s (level %d → %d). Buying cover before flying again.",
				m.stationLabel(ctx, homeStationID), death.CloneName, death.SPLost, death.SkillName, death.OldLevel, death.NewLevel)
		} else {
			m.logBot(charID, logError, "clone",
				"clone spent — woke up at %s on a Grade Alpha clone. Buying cover before flying again.",
				m.stationLabel(ctx, homeStationID))
		}
	} else {
		m.logBot(charID, logInfo, "clone", "recovered at %s", m.stationLabel(ctx, homeStationID))
	}

	if err := resetBot(ctx, m.db, charID); err != nil {
		rlogf("rolebots: reset after respawn: %v", err)
	}
}

// homeStationFor resolves a pilot's home: its medical clone station, where the server
// agrees the character wakes. Everything meaning "home" asks here (procure.go, beltloot.go,
// piracy_loot.go, the pod respawn), so no two steps disagree. The fallback is the server's
// own, for a character that never had a clone set.
func (m *Mod) homeStationFor(ctx context.Context, charID int64) int64 {
	if id, err := evedb.GetHomeStationID(ctx, m.db, charID); err == nil && id != 0 {
		return id
	}
	return fallbackHomeStationID
}

// destroyShipAndContents removes a dead hull and everything inside it: fitted modules,
// loaded charges and cargo. entity_attributes has a foreign key onto entity, so a plain
// DELETE FROM entity fails for any wreck with a fit. Delete an item's attributes first
// (db.DeleteEntityDeep); containers in the hold mean the walk goes all the way down.
func (m *Mod) destroyShipAndContents(ctx context.Context, charID, shipID int64) {
	var contents int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE locationID = ? AND itemID <> ?`,
		shipID, charID).Scan(&contents); err != nil {
		rlogf("rolebots: count ship contents %d: %v", shipID, err)
	}
	// The character entity rides inside the ship: keep it out of the delete, since it moves
	// into the capsule.
	for _, id := range m.itemsInside(ctx, shipID, charID) {
		if err := evedb.DeleteEntityDeep(ctx, m.db, id); err != nil {
			rlogf("rolebots: delete ship contents %d: %v", id, err)
		}
	}
	if err := evedb.DeleteEntityDeep(ctx, m.db, shipID); err != nil {
		rlogf("rolebots: delete dead ship %d: %v", shipID, err)
	}
	if contents > 0 {
		// A loss, not a fault: the ordinary end of a fight, so it is logged at info level.
		m.logBot(charID, logInfo, "combat",
			"%d item(s) went down with the ship", contents)
	}
}

// itemsInside lists everything held by a container, deepest first, so each item is deleted
// before what holds it. keep is the one itemID never returned (the pilot). Depth is bounded
// so a locationID cycle cannot loop forever.
func (m *Mod) itemsInside(ctx context.Context, containerID, keep int64) []int64 {
	var out []int64
	frontier := []int64{containerID}
	for depth := 0; depth < 8 && len(frontier) > 0; depth++ {
		var next []int64
		for _, parent := range frontier {
			rows, err := m.db.QueryContext(ctx,
				`SELECT itemID FROM entity WHERE locationID = ? AND itemID <> ?`, parent, keep)
			if err != nil {
				rlogf("rolebots: list contents of %d: %v", parent, err)
				continue
			}
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil && id != parent {
					next = append(next, id)
				}
			}
			rows.Close()
		}
		out = append(next, out...) // deeper levels end up in front
		frontier = next
	}
	return out
}

// ── labels ──────────────────────────────────────────────────────────────────

// shipLabel names a hull for the log while the row still exists.
func (m *Mod) shipLabel(ctx context.Context, shipID int64) string {
	var name string
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(NULLIF(e.itemName, ''), t.typeName) FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.itemID = ?`, shipID).Scan(&name)
	if err != nil || name == "" {
		return fmt.Sprintf("ship %d", shipID)
	}
	return name
}

// stationLabel names a station for the log.
func (m *Mod) stationLabel(ctx context.Context, stationID int64) string {
	return m.stationNameOr(ctx, stationID)
}

// podDeathGrace is how long the module waits before rebuilding a pilot after
// a death, so the client's explosion has finished playing on every screen that
// saw it before a new ball turns up.
const podDeathGrace = 3 * time.Second
