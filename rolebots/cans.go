package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	"gurimoon/modapi"
)

// Jetcans: the container half of a "cans" mining op. A miner that stays on its belt fills
// its hold and jettisons the load into a cargo container beside it; the gang's hauler
// empties the cans into a station. A can is the same object a player's jettison makes:
// entity typeID 23, locationID = solarSystemID, flag 0, singleton 1 (db.CreateLootContainer),
// contents as entity rows with locationID = canItemID, ball BallDesc{ModeRigid, SubType 2,
// Radius 14} + SlimItem{typeID 23, groupID 12, categoryID 2}. Taking the last item despawns
// the can here, and ownership moves to the taker or the owner-filtered cargo view hides the
// loot. The core deletes every can in space at boot (CleanupSpaceLootContainers) and
// expires registered cans at 2 h; module cans mirror both.

const (
	canCapacityM3 = 27_500.0 // invTypes 23 capacity; server never enforces it, we do
	canTTLSec     = 2 * 3600 // match the core's lootContainerTTL
	// Where a fresh can lands relative to the jettisoning ship: the core Jettison's +2500 m
	// X-only offset, at the client's open-cargo range gate.
	canJettisonOffsetM = 2500.0

	// canRolloverMarginSec is how long before the 2 h expiry a can still holding ore is rolled
	// into a fresh container (sweepCans, rolloverCan), because the core deletes a container with
	// its contents. It must exceed the roster pass interval and the core's expiry sweeps.
	canRolloverMarginSec = 20 * 60
	// canRolloverAgeSec is the age at which a can stops taking ore
	// (openCanFor) and is rolled if it holds any.
	canRolloverAgeSec = canTTLSec - canRolloverMarginSec
	// canRolloverMax bounds how many times one line of containers is rolled. After four
	// rollovers (about seven hours) no hauler is coming, and the ore is booked lost (bookCanLoss).
	canRolloverMax = 4
)

// gangCan is one container a hauler could service: a module-made can from
// the registry, or a player member's own jettisoned can found by type scan.
type gangCan struct {
	CanID      int64
	GangID     int64
	OwnerID    int64
	SystemID   int64
	BeltItemID int64 // 0 for player jetsam: position unknown to the op
	CreatedAt  int64
	UsedM3     float64
	Rollovers  int // how many containers this one has already been rolled out of
	// FirstCreatedAt is when the ore was first canned, carried through every rollover.
	// CreatedAt is this container's own clock. 0 means not recorded, read as CreatedAt.
	FirstCreatedAt int64
}

// queuedAt is the age the hauler queues this can by: when its ore was first
// canned, not when the latest container around it was spawned.
func (c gangCan) queuedAt() int64 {
	if c.FirstCreatedAt > 0 && c.FirstCreatedAt < c.CreatedAt {
		return c.FirstCreatedAt
	}
	return c.CreatedAt
}

// ── creating and filling ────────────────────────────────────────────────────

// jettisonToCan moves every cargo stack of oreTypeID from the ship into the
// miner's open can at this belt, opening a fresh can when there is none or
// the current one is full. Returns units moved.
func (m *Mod) jettisonToCan(ctx context.Context, gangID, charID, shipID, systemID, beltItemID, oreTypeID int64) (int64, error) {
	oreVol, err := evedb.GetTypeVolume(ctx, m.db, oreTypeID)
	if err != nil || oreVol <= 0 {
		oreVol = 0.1
	}

	var moved int64
	for {
		// The stack still aboard (the cargo may hold several rows; take the
		// biggest each round).
		var stackID, stackQty int64
		err := m.db.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity
			WHERE locationID = ? AND ownerID = ? AND typeID = ? AND flag = 5
			ORDER BY quantity DESC LIMIT 1`,
			shipID, charID, oreTypeID).Scan(&stackID, &stackQty)
		if err == sql.ErrNoRows {
			// Booked once per jettison, at no value yet: the hauler's disposal is counted under the
			// fleet stage (oreflow.go).
			m.noteOreFlow(ctx, oreTypeID, oreStageJetcan, moved, 0)
			return moved, nil // hold is clear of this ore
		}
		if err != nil {
			return moved, fmt.Errorf("jettison stack: %w", err)
		}

		canID, room, err := m.openCanFor(ctx, gangID, charID, shipID, systemID, beltItemID)
		if err != nil {
			return moved, err
		}
		take := int64(room / oreVol)
		if take <= 0 {
			// A fresh can with no room means the capacity math is broken: return instead of looping.
			return moved, fmt.Errorf("can %d reports no room straight after opening", canID)
		}
		if take >= stackQty {
			// Whole stack fits: move the row into the can, then fold it into
			// any stack of the same ore already in there.
			if _, err := m.db.ExecContext(ctx, `
				UPDATE entity SET locationID = ?, flag = 0 WHERE itemID = ?`,
				canID, stackID); err != nil {
				return moved, fmt.Errorf("jettison move: %w", err)
			}
			moved += stackQty
		} else {
			// Partial: shrink the cargo stack and grow (or create) the can's.
			if _, err := m.db.ExecContext(ctx, `
				UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`,
				take, stackID); err != nil {
				return moved, fmt.Errorf("jettison split: %w", err)
			}
			if err := evedb.CreateNPCLootItem(ctx, m.db, canID, charID, oreTypeID, take); err != nil {
				return moved, fmt.Errorf("jettison fill: %w", err)
			}
			moved += take
		}
		if err := m.mergeCanStacks(ctx, canID, oreTypeID); err != nil {
			rlogf("rolebots: merge can %d stacks: %v", canID, err)
		}
	}
}

// openCanFor finds the miner's current can at this belt with room left, or
// jettisons a new one beside the ship. Returns the can and its free m³.
func (m *Mod) openCanFor(ctx context.Context, gangID, charID, shipID, systemID, beltItemID int64) (int64, float64, error) {
	// A can past its rollover age takes no more ore: it is about to be rolled (sweepCans) or,
	// if empty, to expire.
	rows, err := m.db.QueryContext(ctx, `
		SELECT canItemID FROM rolebot_gang_cans
		WHERE gangID = ? AND ownerCharID = ? AND systemID = ? AND beltItemID = ?
		  AND createdAt >= ?
		ORDER BY createdAt DESC`, gangID, charID, systemID, beltItemID,
		clock.Unix()-canRolloverAgeSec)
	if err != nil {
		return 0, 0, fmt.Errorf("openCanFor: %w", err)
	}
	var candidates []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		candidates = append(candidates, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, canID := range candidates {
		// The row may be stale: the core deletes cans at boot, expires them
		// at 2 h, and a player may have emptied one (emptying despawns it).
		var one int64
		if m.db.QueryRowContext(ctx,
			`SELECT 1 FROM entity WHERE itemID = ? AND typeID = ?`,
			canID, evedb.TypeCargoContainer).Scan(&one) != nil {
			m.forgetCan(ctx, canID)
			continue
		}
		used, err := m.canUsedM3(ctx, canID)
		if err != nil {
			continue
		}
		if canCapacityM3-used > 1 {
			return canID, canCapacityM3 - used, nil
		}
	}
	canID, err := m.spawnCan(ctx, gangID, charID, shipID, systemID, beltItemID)
	if err != nil {
		return 0, 0, err
	}
	return canID, canCapacityM3, nil
}

// spawnCan creates a fresh container beside the ship: the entity row the server's Jettison
// writes, the registry row the hauler and the expiry sweep find it by, and, when the system
// is live, the destiny ball players on the belt see.
func (m *Mod) spawnCan(ctx context.Context, gangID, charID, shipID, systemID, beltItemID int64) (int64, error) {
	// Beside the ship, with the core's own +2500 m X offset. A ship with no
	// persisted position falls back to the belt itself.
	var x, y, z float64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
		shipID).Scan(&x, &y, &z); err != nil || (x == 0 && y == 0 && z == 0) {
		if beltItemID != 0 {
			_ = m.db.QueryRowContext(ctx, `
				SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0)
				FROM mapDenormalize WHERE itemID = ?`, beltItemID).Scan(&x, &y, &z)
		}
	}
	x += canJettisonOffsetM
	canID, err := m.spawnCanAt(ctx, gangID, charID, systemID, beltItemID, destiny.Vec3{X: x, Y: y, Z: z}, 0)
	if err != nil {
		return 0, err
	}
	m.logBot(charID, logInfo, "gang", "jettisoned a fresh cargo container at belt %d (can %d)", beltItemID, canID)
	return canID, nil
}

// spawnCanAt is spawnCan at a known position. rollovers is how many containers this one
// continues (rolloverCan); 0 for a fresh jettison.
func (m *Mod) spawnCanAt(ctx context.Context, gangID, charID, systemID, beltItemID int64, pos destiny.Vec3, rollovers int) (int64, error) {
	return m.spawnCanFrom(ctx, gangID, charID, systemID, beltItemID, pos, rollovers, 0)
}

// spawnCanFrom is spawnCanAt for a container that continues an older one:
// firstAt is when its ore was first canned (0 = now, a fresh jettison).
func (m *Mod) spawnCanFrom(ctx context.Context, gangID, charID, systemID, beltItemID int64, pos destiny.Vec3, rollovers int, firstAt int64) (int64, error) {
	canID, err := evedb.CreateLootContainer(ctx, m.db, charID, systemID, pos.X, pos.Y, pos.Z)
	if err != nil {
		return 0, fmt.Errorf("spawnCan: %w", err)
	}
	now := clock.Unix()
	if firstAt <= 0 || firstAt > now {
		firstAt = now
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_gang_cans (canItemID, gangID, ownerCharID, systemID, beltItemID, createdAt, rollovers, firstCreatedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		canID, gangID, charID, systemID, beltItemID, now, rollovers, firstAt); err != nil {
		return 0, fmt.Errorf("spawnCan registry: %w", err)
	}
	if ss := m.loadedSystem(systemID); ss != nil {
		m.registerCanBall(ss, canID, charID, pos)
	}
	return canID, nil
}

// loadedSystem is the live SolarSystem, or nil when the system is not loaded or there is
// no system manager (the harness).
func (m *Mod) loadedSystem(systemID int64) *destiny.SolarSystem {
	if m.systems == nil {
		return nil
	}
	return m.systems.Get(systemID)
}

// rolloverCan continues an ageing container as a fresh one: its contents move to a new can
// at the same spot by locationID in one statement, the old can is despawned and the
// registry row carries the count. A fresh container resets both expiry clocks, the module's
// createdAt and the core's spaceLootContainers.spawnedAt.
func (m *Mod) rolloverCan(ctx context.Context, c gangCan) (int64, error) {
	var pos destiny.Vec3
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
		c.CanID).Scan(&pos.X, &pos.Y, &pos.Z); err != nil {
		return 0, fmt.Errorf("rollover can %d position: %w", c.CanID, err)
	}
	var units int64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(quantity), 0) FROM entity WHERE locationID = ?`, c.CanID).Scan(&units)
	newID, err := m.spawnCanFrom(ctx, c.GangID, c.OwnerID, c.SystemID, c.BeltItemID, pos, c.Rollovers+1, c.queuedAt())
	if err != nil {
		return 0, err
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET locationID = ? WHERE locationID = ?`, newID, c.CanID); err != nil {
		return 0, fmt.Errorf("rollover can %d contents: %w", c.CanID, err)
	}
	m.despawnCan(ctx, c.CanID, c.SystemID)
	m.logBot(c.OwnerID, logInfo, "gang",
		"rolled %d units from an ageing cargo container into a fresh one (can %d → %d, rollover %d of %d) — no hauler has emptied it",
		units, c.CanID, newID, c.Rollovers+1, canRolloverMax)
	return newID, nil
}

// bookCanLoss writes down what a container is about to take with it: one
// expired-stage row per ore type (oreflow.go), and a line in the owner's log.
// Returns the units lost.
func (m *Mod) bookCanLoss(ctx context.Context, canID, ownerID int64) int64 {
	// Ore only: wreck containers (piracy_loot.go) go through the same sweep, and their modules
	// and ammunition must not be booked as ore lost to expiry.
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, COALESCE(SUM(e.quantity), 0)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND g.categoryID = ?
		GROUP BY e.typeID`, canID, categoryAsteroid)
	if err != nil {
		rlogf("rolebots: book can %d loss: %v", canID, err)
		return 0
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var typeID, units int64
		if rows.Scan(&typeID, &units) == nil && units > 0 {
			m.noteOreFlow(ctx, typeID, oreStageExpired, units, 0)
			total += units
		}
	}
	if total > 0 && ownerID != 0 {
		m.logBot(ownerID, logInfo, "gang",
			"lost %d units with cargo container %d — it expired before any hauler emptied it", total, canID)
	}
	return total
}

// registerCanBall puts a can on the live grid: the ball, slim and grid-local AddBalls
// delivery the core's spawnLootContainerBall does. Registering it makes the can openable,
// since the client's GetInventoryFromId bind needs the system to know the container.
func (m *Mod) registerCanBall(ss *destiny.SolarSystem, canID, ownerID int64, pos destiny.Vec3) {
	if _, known := ss.GetLootContainerPosition(uint32(canID)); known {
		return // already registered (system reload path)
	}
	bd := destiny.BallDesc{
		EntityID: uint32(canID), Mode: destiny.ModeRigid, SubType: 2,
		Pos: pos, Radius: 14,
	}
	slim := destiny.SlimItem{
		BallID: canID, ItemID: canID,
		TypeID: evedb.TypeCargoContainer, GroupID: 12, CategoryID: 2,
		OwnerID: ownerID,
	}
	ss.AddLootContainer(bd, slim)
	dmg := map[uint32]destiny.DamageState{
		uint32(canID): {Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0},
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.HasBallparkState() || p.IsInWarp() || p.IsWarpPending() {
			continue
		}
		if p.Pos().Sub(pos).LenSq() > destiny.BubbleRadius*destiny.BubbleRadius {
			continue
		}
		if len(p.ClaimUnknownBalls(uint32(canID))) == 0 {
			continue
		}
		// Queued with QueueAddActionFunc: the flush that decides the stamp builds the payload, and
		// DropPendingAdd tells an add from movement. The queue's purge paths release the claim.
		p.QueueAddActionFunc(canAddKey(canID), uint32(canID), func(stamp uint32) marshal.PyRep {
			return destiny.BuildAddBallsAction(stamp,
				[]destiny.BallDesc{bd}, []destiny.SlimItem{slim}, dmg)
		})
	}
}

// canAddKey and canRemoveKey name the coalescing slots a container occupies on a witness's
// queue. The kinds differ so an add and a removal never overwrite each other:
// DropPendingAdd cancels the pair.
func canAddKey(canID int64) string    { return "add:" + strconv.FormatInt(canID, 10) }
func canRemoveKey(canID int64) string { return "rm:" + strconv.FormatInt(canID, 10) }

// projectCansOnLoad re-registers this system's surviving module cans when a player loads
// it, so a can made while the system was dark is visible and openable.
func (m *Mod) projectCansOnLoad(ss *destiny.SolarSystem, systemID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.canItemID, c.ownerCharID, COALESCE(e.x,0), COALESCE(e.y,0), COALESCE(e.z,0)
		FROM rolebot_gang_cans c
		JOIN entity e ON e.itemID = c.canItemID
		WHERE c.systemID = ?`, systemID)
	if err != nil {
		rlogf("rolebots: project cans system %d: %v", systemID, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var canID, ownerID int64
		var x, y, z float64
		if rows.Scan(&canID, &ownerID, &x, &y, &z) == nil {
			m.registerCanBall(ss, canID, ownerID, destiny.Vec3{X: x, Y: y, Z: z})
		}
	}
}

// ── reading and emptying ────────────────────────────────────────────────────

// canUsedM3 is the volume sitting in a container.
func (m *Mod) canUsedM3(ctx context.Context, canID int64) (float64, error) {
	var used sql.NullFloat64
	err := m.db.QueryRowContext(ctx, `
		SELECT SUM(e.quantity * COALESCE(t.volume, 0.1))
		FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.locationID = ?`, canID).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("canUsedM3: %w", err)
	}
	return used.Float64, nil
}

// gangCansAt lists every container a hauler should service in a system: module-made cans
// from the registry plus any a member jettisoned themselves (a player mining alongside the
// bots). The result is ordered by sortCansForHauler.
func (m *Mod) gangCansAt(ctx context.Context, gangID, systemID int64) ([]gangCan, error) {
	members, err := gangMemberChars(ctx, m.db, gangID)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*gangCan{}

	rows, err := m.db.QueryContext(ctx, `
		SELECT c.canItemID, c.ownerCharID, c.beltItemID, c.createdAt, c.rollovers, c.firstCreatedAt
		FROM rolebot_gang_cans c
		JOIN entity e ON e.itemID = c.canItemID
		WHERE c.gangID = ? AND c.systemID = ?`, gangID, systemID)
	if err != nil {
		return nil, fmt.Errorf("gangCansAt: %w", err)
	}
	for rows.Next() {
		var c gangCan
		if rows.Scan(&c.CanID, &c.OwnerID, &c.BeltItemID, &c.CreatedAt, &c.Rollovers, &c.FirstCreatedAt) == nil {
			c.GangID = gangID
			c.SystemID = systemID
			byID[c.CanID] = &c
		}
	}
	rows.Close()

	// Player members' own jetsam: typeID 23 in this system's space, owned
	// by any member, not already known from the registry.
	if len(members) > 0 {
		q := `SELECT itemID, ownerID FROM entity
			WHERE typeID = ? AND locationID = ? AND ownerID IN (` + placeholders(len(members)) + `)`
		args := append([]any{evedb.TypeCargoContainer, systemID}, int64Args(members)...)
		rows, err := m.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("gangCansAt jetsam: %w", err)
		}
		for rows.Next() {
			var c gangCan
			if rows.Scan(&c.CanID, &c.OwnerID) == nil && byID[c.CanID] == nil {
				c.SystemID = systemID
				byID[c.CanID] = &c
			}
		}
		rows.Close()
	}

	out := make([]gangCan, 0, len(byID))
	for _, c := range byID {
		used, err := m.canUsedM3(ctx, c.CanID)
		if err != nil {
			continue
		}
		c.UsedM3 = used
		out = append(out, *c)
	}
	sortCansForHauler(out)
	return out, nil
}

// sortCansForHauler orders the cans a hauler will empty: oldest first, and the fullest
// among cans of one age, so the ore nearest its expiry is taken first. A player member's
// jetsam has no createdAt (0) and sorts first, since the module cannot roll it.
// Age is queuedAt, when the ore was first canned: a rollover gives a fresh createdAt.
func sortCansForHauler(cans []gangCan) {
	sort.SliceStable(cans, func(i, j int) bool {
		if ai, aj := cans[i].queuedAt(), cans[j].queuedAt(); ai != aj {
			return ai < aj
		}
		return cans[i].UsedM3 > cans[j].UsedM3
	})
}

// takeCanContents moves as much of a can as fits into the hauler's hold, transferring
// ownership to the hauler as the core's take path does. An emptied can is despawned. It
// returns the m³ taken and the units per type: a can belongs to one miner (openCanFor), so
// the per-type count is that miner's delivery credit (gangindustry.go).
func (m *Mod) takeCanContents(ctx context.Context, haulerCharID, shipID, canID, systemID int64, freeM3 float64) (float64, map[int64]int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.quantity, COALESCE(t.volume, 0.1), COALESCE(e.ownerID, 0)
		FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.locationID = ?`, canID)
	if err != nil {
		return 0, nil, fmt.Errorf("takeCanContents: %w", err)
	}
	type stack struct {
		itemID, typeID, qty, owner int64
		vol                        float64
	}
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.itemID, &s.typeID, &s.qty, &s.vol, &s.owner); err != nil {
			rows.Close()
			return 0, nil, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	taken := 0.0
	perType := map[int64]int64{}
	// Taking from another owner's container is aggression (core loottheft.go): the module
	// excuses its own gang and leaves the rest to the core. One note per foreign owner per take.
	noteTheft := modapi.PvP().NoteTheft
	noted := map[int64]bool{}
	for _, s := range stacks {
		if freeM3-taken < s.vol {
			break
		}
		units := int64((freeM3 - taken) / s.vol)
		if units >= s.qty {
			units = s.qty
			// Whole stack: delete the can row and fold it into the hold.
			if _, err := m.db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.itemID); err != nil {
				return taken, perType, fmt.Errorf("take stack: %w", err)
			}
		} else {
			if _, err := m.db.ExecContext(ctx, `
				UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`,
				units, s.itemID); err != nil {
				return taken, perType, fmt.Errorf("take split: %w", err)
			}
		}
		if err := m.addCargoStackFor(ctx, haulerCharID, shipID, s.typeID, units); err != nil {
			return taken, perType, fmt.Errorf("take load: %w", err)
		}
		taken += float64(units) * s.vol
		perType[s.typeID] += units
		if noteTheft != nil && s.owner > 0 && s.owner != haulerCharID && !noted[s.owner] {
			noted[s.owner] = true
			if hg := m.gangOfCharCached(haulerCharID); hg == 0 || hg != m.gangOfCharCached(s.owner) {
				if ss := m.loadedSystem(systemID); ss != nil {
					noteTheft(ss, haulerCharID, 0, s.owner)
				}
			}
		}
	}

	if empty, err := evedb.IsContainerEmpty(ctx, m.db, canID); err == nil && empty {
		m.despawnCan(ctx, canID, systemID)
	}
	return taken, perType, nil
}

// despawnCan removes a container from space: off the live grid (with
// RemoveBalls to whoever saw it), out of the database, out of the registry.
func (m *Mod) despawnCan(ctx context.Context, canID, systemID int64) {
	if ss := m.loadedSystem(systemID); ss != nil && ss.RemoveLootContainer(uint32(canID)) {
		for _, p := range ss.GetAllPlayersInSystem() {
			if p.Conn == nil || !p.KnowsBall(uint32(canID)) {
				continue
			}
			// DropPendingAdd first: a can jettisoned and looted dry within one hold window never
			// reached the client, and RemoveBalls would only log a missing ball.
			if p.DropPendingAdd(uint32(canID)) {
				p.ForgetBalls(uint32(canID))
				continue
			}
			// QueueRemoveBall keeps the can known until the removal is on the wire; other code reads
			// the known set as what the client is drawing.
			p.QueueRemoveBall(canRemoveKey(canID), uint32(canID))
		}
	}
	if err := evedb.DeleteLootContainer(ctx, m.db, canID); err != nil {
		rlogf("rolebots: delete can %d: %v", canID, err)
	}
	m.forgetCan(ctx, canID)
}

func (m *Mod) forgetCan(ctx context.Context, canID int64) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_gang_cans WHERE canItemID = ?`, canID); err != nil {
		rlogf("rolebots: forget can %d: %v", canID, err)
	}
}

// ── expiry ──────────────────────────────────────────────────────────────────

// sweepCans keeps the module's containers on the right side of the core's 2 h expiry and
// drops registry rows whose entity is gone. A can with ore nearing expiry is rolled
// (rolloverCan) while its op is alive, up to canRolloverMax times. Any other can reaching
// expiry is despawned and its contents are booked lost (bookCanLoss).
func (m *Mod) sweepCans(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.canItemID, c.gangID, c.ownerCharID, c.bossCharID, c.systemID, c.beltItemID,
		       c.createdAt, c.rollovers, c.firstCreatedAt,
		       (SELECT 1 FROM entity e WHERE e.itemID = c.canItemID LIMIT 1),
		       (SELECT 1 FROM rolebot_gangs g WHERE g.gangID = c.gangID LIMIT 1)
		FROM rolebot_gang_cans c`)
	if err != nil {
		rlogf("rolebots: sweep cans: %v", err)
		return
	}
	type row struct {
		can       gangCan
		bossID    int64
		alive     bool
		gangAlive bool
	}
	var all []row
	for rows.Next() {
		var r row
		var alive, gangAlive sql.NullInt64
		if rows.Scan(&r.can.CanID, &r.can.GangID, &r.can.OwnerID, &r.bossID, &r.can.SystemID,
			&r.can.BeltItemID, &r.can.CreatedAt, &r.can.Rollovers, &r.can.FirstCreatedAt, &alive, &gangAlive) == nil {
			r.alive, r.gangAlive = alive.Valid, gangAlive.Valid
			all = append(all, r)
		}
	}
	rows.Close()

	now := clock.Unix()
	cutoff := now - canTTLSec
	rollAt := now - canRolloverAgeSec
	for _, r := range all {
		switch {
		case !r.alive:
			m.forgetCan(ctx, r.can.CanID)
		case r.can.CreatedAt < rollAt && canMayRoll(r.can, r.bossID, r.gangAlive):
			used, err := m.canUsedM3(ctx, r.can.CanID)
			if err != nil {
				continue
			}
			if used <= 0 {
				if r.can.CreatedAt < cutoff {
					m.despawnCan(ctx, r.can.CanID, r.can.SystemID) // empty: let it go quietly
				}
				continue
			}
			if _, err := m.rolloverCan(ctx, r.can); err != nil {
				rlogf("rolebots: roll over can %d: %v", r.can.CanID, err)
			}
		case r.can.CreatedAt < cutoff:
			lost := m.bookCanLoss(ctx, r.can.CanID, r.can.OwnerID)
			// Logged under the can's owner with the fleet in the text, so a lost can can be tied to
			// its gang from the log.
			m.logBot(r.can.OwnerID, logInfo, "gang", "cargo container %d of gang %d expired after 2h — despawning (%d units lost)",
				r.can.CanID, r.can.GangID, lost)
			m.despawnCan(ctx, r.can.CanID, r.can.SystemID)
		}
	}
}

// canMayRoll reports whether an ageing container is still somebody's live
// work: a fleet's can while the fleet exists, or a conscript's can under a
// player boss, and in either case not yet rolled canRolloverMax times. A
// wreck container (gangID 0, no boss) never rolls. Pure.
func canMayRoll(c gangCan, bossID int64, gangAlive bool) bool {
	if c.Rollovers >= canRolloverMax {
		return false
	}
	return (c.GangID != 0 && gangAlive) || bossID != 0
}
