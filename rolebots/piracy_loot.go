package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/rates"
)

// The wages: what a robbery is actually worth.
//
// Piracy that ends at the kill is vandalism. This file is the other half: the
// container a dead ship leaves, the pirate flying back to empty it, and the trip
// home to sell it. A player's ship already drops properly: the core's death path
// rolls every module and cargo stack against lootDropChance into a container
// (spawnDeathLoot), which a pirate empties like a jetcan. A rolebot's ship rolls
// the same way with the same helpers (evedb.CreateLootContainer,
// GetShipChildItems, MoveItemIntoContainer), or killing a bot would pay nothing.

const (
	// The share of a destroyed ship's items that survive, matching the core's
	// lootDropChance for a player death. Scaled by rates.LootDrop like everything
	// else that drops.
	botLootDropChance = 0.5
	// A pirate will fly this far back for a wreck it made. Past that the
	// haul is not worth the trip and the container is left to expire.
	lootRecoveryJumps = 3
	// How long a pirate keeps trying to empty one container before writing it off:
	// containers despawn on their own, and a bot that cannot reach this one must not
	// spend its life on it.
	lootAttemptSec = 8 * 60
)

// ── the drop ────────────────────────────────────────────────────────────────

// dropWreckLoot rolls a destroyed rolebot's contents into a cargo container at
// the wreck, as the core does for a player. Returns the container (0 when none
// survived) and the dropped and destroyed lists the killmail needs. It deletes
// nothing: the caller's destroyShipAndContents removes what is left in the hull.
func (m *Mod) dropWreckLoot(ctx context.Context, charID, shipID, systemID int64,
	x, y, z float64) (containerID int64, dropped, destroyed []evedb.EntityRow) {

	children, err := evedb.GetShipChildItems(ctx, m.db, shipID)
	if err != nil {
		rlogf("rolebots: wreck loot children ship=%d: %v", shipID, err)
		return 0, nil, nil
	}
	var survivors []int64
	for _, ch := range children {
		if rand.Float64() < botLootDropChance*rates.LootDrop {
			survivors = append(survivors, ch.ItemID)
			dropped = append(dropped, ch)
		} else {
			destroyed = append(destroyed, ch)
		}
	}
	if len(survivors) == 0 {
		return 0, nil, destroyed
	}

	containerID, err = evedb.CreateLootContainer(ctx, m.db, charID, systemID, x, y, z)
	if err != nil {
		rlogf("rolebots: wreck container char=%d: %v", charID, err)
		// The roll happened but the can did not: everything burns. Reclassify so the
		// killmail matches the grid, as the core's spawnDeathLoot does.
		return 0, nil, children
	}
	moved := 0
	for _, itemID := range survivors {
		if err := evedb.MoveItemIntoContainer(ctx, m.db, itemID, containerID); err != nil {
			rlogf("rolebots: wreck loot move item=%d: %v", itemID, err)
			continue
		}
		moved++
	}
	if moved == 0 {
		if err := evedb.DeleteEntity(ctx, m.db, containerID); err != nil {
			rlogf("rolebots: wreck container cleanup %d: %v", containerID, err)
		}
		return 0, nil, children
	}

	// Registered as a module container so the can is openable (the client's inventory
	// bind needs the system to know it), reloads (projectCansOnLoad) and expires (sweepCans).
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_gang_cans (canItemID, gangID, ownerCharID, systemID, beltItemID, createdAt)
		VALUES (?, 0, ?, ?, 0, ?)`,
		containerID, charID, systemID, clock.Unix()); err != nil {
		rlogf("rolebots: wreck container registry %d: %v", containerID, err)
	}
	if m.systems != nil {
		if ss := m.systems.Get(systemID); ss != nil {
			m.registerCanBall(ss, containerID, charID, destiny.Vec3{X: x, Y: y, Z: z})
		}
	}
	m.logBot(charID, logInfo, "combat",
		"%d item(s) survived the wreck and are floating in a container", moved)
	return containerID, dropped, destroyed
}

// notePiracyLoot points the killer's sim at the container its victim just
// dropped, so the next planning pass empties it. The container may be the core's
// (player kill) or the module's (rolebot kill); the position is the only key, so
// this looks for a container near where the victim died.
func (m *Mod) notePiracyLoot(ctx context.Context, eb *embodiedBot, victim *destiny.Entity) {
	if victim == nil {
		return
	}
	pos := victim.Pos()
	// The core spawns its container asynchronously after a three-second explosion
	// delay, so the search is deferred, not retried: waiting would block a combat tick.
	charID, sysID, p := eb.charID, eb.systemID, pos
	m.goSafeBot(charID, "piracy-loot note", func() {
		time.Sleep(6 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		id := m.containerNear(ctx, sysID, p)
		if id == 0 {
			return
		}
		// As noteBeltLoot: a deferred read-modify-write of the state row takes the
		// pilot's step lock, or it races the turn planning from it.
		unlock := lockBotStep(charID)
		defer unlock()
		st, err := loadState(ctx, m.db, charID)
		if err != nil {
			return
		}
		if st.Data.PiracyLootID != 0 {
			return // already has a wreck to strip; one at a time
		}
		st.Data.PiracyLootID, st.Data.PiracyLootSysID = id, sysID
		if err := saveState(ctx, m.db, st); err != nil {
			rlogf("rolebots: note loot char=%d: %v", charID, err)
			return
		}
		m.logBot(charID, logInfo, "piracy", "wreck left a container worth opening")
	})
}

// containerNear finds a cargo container within a few kilometres of a position,
// the wreck's spoils. Nearest wins, because two kills on one belt is a normal
// evening for a pirate.
func (m *Mod) containerNear(ctx context.Context, systemID int64, p destiny.Vec3) int64 {
	const radiusM = 25_000.0
	var id int64
	err := m.db.QueryRowContext(ctx, `
		SELECT itemID FROM entity
		WHERE locationID = ? AND typeID = ?
		  AND POWER(x - ?, 2) + POWER(y - ?, 2) + POWER(z - ?, 2) < POWER(?, 2)
		ORDER BY POWER(x - ?, 2) + POWER(y - ?, 2) + POWER(z - ?, 2)
		LIMIT 1`,
		systemID, evedb.TypeCargoContainer,
		p.X, p.Y, p.Z, radiusM,
		p.X, p.Y, p.Z).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		rlogf("rolebots: container search sys=%d: %v", systemID, err)
	}
	return id
}

// containerNearOwnedBy is containerNear limited to cans the given owners
// left: a room several parties share is no place to take the nearest one.
func (m *Mod) containerNearOwnedBy(ctx context.Context, systemID int64, p destiny.Vec3, owners []int64) int64 {
	if len(owners) == 0 {
		return 0
	}
	const radiusM = 25_000.0
	args := []any{systemID, evedb.TypeCargoContainer}
	args = append(args, int64Args(owners)...)
	args = append(args, p.X, p.Y, p.Z, radiusM, p.X, p.Y, p.Z)
	var id int64
	err := m.db.QueryRowContext(ctx, `
		SELECT itemID FROM entity
		WHERE locationID = ? AND typeID = ? AND ownerID IN (`+placeholders(len(owners))+`)
		  AND POWER(x - ?, 2) + POWER(y - ?, 2) + POWER(z - ?, 2) < POWER(?, 2)
		ORDER BY POWER(x - ?, 2) + POWER(y - ?, 2) + POWER(z - ?, 2)
		LIMIT 1`, args...).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		rlogf("rolebots: owned container search sys=%d: %v", systemID, err)
	}
	return id
}

// lootOwnersFor is the pilot and, when it flies with one, its fleet, whose cans a
// pilot may open without it being theft (cans.go excuses the gang).
func (m *Mod) lootOwnersFor(ctx context.Context, charID, gangID int64) []int64 {
	out := []int64{charID}
	if gangID == 0 {
		return out
	}
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID FROM rolebot_gang_members WHERE gangID = ? AND characterID <> ?`, gangID, charID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// ── collecting it ───────────────────────────────────────────────────────────

// piracyResumeLoot puts the pilot back on a wreck it has not finished
// stripping. Reports whether it took the pass.
func (m *Mod) piracyResumeLoot(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if st.Data.PiracyLootID == 0 {
		return false, nil
	}
	// Gone (expired, or somebody else got there first).
	var one int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT 1 FROM entity WHERE itemID = ?`, st.Data.PiracyLootID).Scan(&one); err != nil {
		st.Data.PiracyLootID, st.Data.PiracyLootSysID = 0, 0
		return false, saveState(ctx, m.db, st)
	}
	// Too far to be worth it. A pirate does not cross the map for a wreck.
	if st.Data.PiracyLootSysID != b.SystemID {
		jumps, err := m.jumpsBetween(ctx, b, b.SystemID, st.Data.PiracyLootSysID)
		if err != nil || jumps > lootRecoveryJumps {
			m.logBot(b.CharID, logInfo, "piracy",
				"leaving the wreck where it is — too far to go back for")
			st.Data.PiracyLootID, st.Data.PiracyLootSysID = 0, 0
			return false, saveState(ctx, m.db, st)
		}
	}
	st.Activity = ActPiracy
	st.Phase = string(TaskPiracyLoot)
	st.Data.PiracyPickedAt = clock.Unix()
	if b.SystemID != st.Data.PiracyLootSysID {
		return true, m.beginTravel(ctx, b, st.Data.PiracyLootSysID, 0, string(TaskPiracyLoot))
	}
	return true, saveState(ctx, m.db, st)
}

// piracyLootStep empties the container into the hold. It runs in the abstract
// tier even when a projection exists: moving items between rows is the same
// operation either way, so an unwatched pirate is paid like a watched one. The
// projection only adds the flying.
func (m *Mod) piracyLootStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if st.Data.PiracyLootID == 0 {
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}
	if b.SystemID != st.Data.PiracyLootSysID {
		return false, m.beginTravel(ctx, b, st.Data.PiracyLootSysID, 0, string(TaskPiracyLoot))
	}
	if b.ShipID == 0 {
		return false, fmt.Errorf("pirate has no ship to load stolen goods into")
	}
	// Gave up on it.
	if clock.Unix()-st.Data.PiracyPickedAt > lootAttemptSec {
		st.Data.PiracyLootID, st.Data.PiracyLootSysID = 0, 0
		st.Activity, st.Phase = "", string(TaskIdle)
		m.logBot(b.CharID, logWarn, "piracy", "could not get the wreck open in time")
		return false, saveState(ctx, m.db, st)
	}

	free, err := m.freeHoldM3(ctx, b)
	if err != nil {
		return false, err
	}
	if free <= 0 {
		// Full hold: go and sell before robbing anybody else.
		m.logBot(b.CharID, logInfo, "piracy", "hold is full — taking this lot home")
		return false, m.piracyStartFence(ctx, b)
	}

	// The ship must be next to it. In watched space the projection flies there;
	// unwatched, the abstract ship is put on the spot.
	if !m.isEmbodied(b.CharID) {
		m.parkAtCelestialEntity(ctx, b.ShipID, st.Data.PiracyLootID)
	}

	taken, _, err := m.takeCanContents(ctx, b.CharID, b.ShipID, st.Data.PiracyLootID, b.SystemID, free)
	if err != nil {
		return false, err
	}
	if taken > 0 {
		m.logBot(b.CharID, logInfo, "piracy", "stripped %.1f m³ out of the wreck", taken)
	}

	empty, err := evedb.IsContainerEmpty(ctx, m.db, st.Data.PiracyLootID)
	if err != nil || empty || taken <= 0 {
		st.Data.PiracyLootID, st.Data.PiracyLootSysID = 0, 0
		// Something in the hold means a trip to a market; nothing means back
		// to work with no time wasted.
		if used, err := m.holdUsedM3(ctx, b.CharID, b.ShipID); err == nil && used > 0 {
			return false, m.piracyStartFence(ctx, b)
		}
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(10)
	return false, saveState(ctx, m.db, st)
}

// ── selling it ──────────────────────────────────────────────────────────────

// piracyStartFence sends the pilot home with the haul. Home, not the best market:
// a pirate's security status and criminal timer keep it out of the empire
// stations its cargo is worth most at, and the home station (homeStationFor) is
// the one place unambiguously its own.
func (m *Mod) piracyStartFence(ctx context.Context, b *botRun) error {
	st := b.State
	// Same rule as the hunter's fence (combatStartSale), with more at stake:
	// piracyFenceStep sells the whole hold, so a live fleet's member must not reach it.
	if m.gangHoldsThisPilot(ctx, b) {
		m.logBot(b.CharID, logInfo, "gang",
			"on a fleet op — the haul stays in the hold until the raid stands down")
		st.Phase = string(TaskPiracyProwl)
		return saveState(ctx, m.db, st)
	}
	home := m.homeStationFor(ctx, b.CharID)
	if home == 0 {
		st.Activity, st.Phase = "", string(TaskIdle)
		m.logBot(b.CharID, logWarn, "piracy", "nowhere to fence a haul — this pilot has no home station")
		return saveState(ctx, m.db, st)
	}
	loc, err := evedb.GetStationLocation(ctx, m.db, home)
	if err != nil {
		return fmt.Errorf("fence station %d: %w", home, err)
	}
	st.Activity = ActPiracy
	st.Data.TargetStationID = home
	st.Data.TargetSystemID = loc.SolarSystemID
	m.logBot(b.CharID, logInfo, "piracy", "running the haul back to %s", m.stationLabel(ctx, home))
	return m.beginTravel(ctx, b, loc.SolarSystemID, home, string(TaskPiracyFence))
}

// piracyResumeFence picks a haul back up: a pirate that logged off (or was
// re-planned) holding stolen goods still has them.
func (m *Mod) piracyResumeFence(ctx context.Context, b *botRun) (bool, error) {
	if b.ShipID == 0 {
		return false, nil
	}
	// Every raider keeps what it took (lootdonate.go), and piracyStartFence's own gate
	// holds a live fleet's member on the op rather than sending it to a fence.
	used, err := m.holdUsedM3(ctx, b.CharID, b.ShipID)
	if err != nil || used <= 0 {
		return false, err
	}
	// A live fleet's member keeps the haul aboard and plans the op. Taking the pass here would
	// hand it to piracyStartFence's gate on every plan, and it would never reach the raid.
	if m.gangHoldsThisPilot(ctx, b) {
		return false, nil
	}
	return true, m.piracyStartFence(ctx, b)
}

// piracyFenceStep sells everything in the hold at the station the pilot is
// standing in. Immediate sell orders against whatever bids exist, stack by
// stack; anything nobody bids on stays in the hangar for the trade planner to
// pick up later (tradeorders.go).
func (m *Mod) piracyFenceStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if b.StationID == 0 {
		if st.Data.TargetStationID == 0 {
			return false, m.piracyStartFence(ctx, b)
		}
		return false, m.beginTravel(ctx, b, st.Data.TargetSystemID, st.Data.TargetStationID,
			string(TaskPiracyFence))
	}

	// Everything out of the hold and into the hangar first: a sell order is placed
	// against a hangar item, not a cargo one.
	if _, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 4
		WHERE locationID = ? AND ownerID = ? AND flag = 5`,
		b.StationID, b.ShipID, b.CharID); err != nil {
		return false, fmt.Errorf("unload the haul: %w", err)
	}
	// A haul is many small stacks of the same few types; stack them first or the
	// fence places one order per wreck (stacking.go).
	m.stackHangar(ctx, b.CharID, b.StationID)

	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.quantity, t.typeName
		FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND e.singleton = 0
		  AND e.quantity > 0`,
		b.CharID, b.StationID)
	if err != nil {
		return false, fmt.Errorf("fence inventory: %w", err)
	}
	type stack struct {
		itemID, typeID, qty int64
		name                string
	}
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.itemID, &s.typeID, &s.qty, &s.name); err != nil {
			rows.Close()
			return false, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}

	gross, sold, unsold := 0.0, 0, 0
	for _, s := range stacks {
		// A stolen implant the pirate can use goes in rather than to the fence.
		if m.isImplantType(ctx, s.typeID) && m.implantKeepOrSell(ctx, b, s.typeID, s.itemID) {
			continue
		}
		var bid sql.NullFloat64
		_ = m.db.QueryRowContext(ctx, `
			SELECT MAX(price) FROM market_orders
			WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`,
			b.StationID, s.typeID).Scan(&bid)
		if !bid.Valid || bid.Float64 <= 0 {
			unsold++
			continue
		}
		out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
			s.typeID, s.itemID, bid.Float64, s.qty, -1 /*station range*/, 1, 0 /*immediate*/, false)
		if err != nil {
			rlogf("rolebots: fence %s: %v", s.name, err)
			unsold++
			continue
		}
		if out.ErrorMsg != "" || out.FilledQty == 0 {
			unsold++
			continue
		}
		for _, f := range out.Fills {
			gross += f.Price * float64(f.Quantity)
		}
		m.notePeerFills(ctx, tradeEntry{
			CharID: b.CharID, TypeID: s.typeID, TypeName: s.name,
			Station: b.StationID, System: b.SystemID,
		}, out.Fills, false, "loot")
		sold++
	}

	switch {
	case sold > 0 && unsold > 0:
		m.logBot(b.CharID, logInfo, "piracy",
			"fenced %d stolen stack(s) for %.2f ISK; %d nobody would buy here", sold, gross, unsold)
	case sold > 0:
		m.logBot(b.CharID, logInfo, "piracy", "fenced the haul for %.2f ISK", gross)
	case unsold > 0:
		m.logBot(b.CharID, logWarn, "piracy",
			"nobody here bids on any of it — %d stack(s) left in the hangar", unsold)
	}

	st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return false, saveState(ctx, m.db, st)
}

// ── small helpers ───────────────────────────────────────────────────────────

// freeHoldM3 is how much room is left in this ship's cargo hold: the fitted hold
// (holdFreeM3), not the bare invTypes.capacity, which overstates it on a hull
// fitted with a cargo penalty.
func (m *Mod) freeHoldM3(ctx context.Context, b *botRun) (float64, error) {
	return m.holdFreeM3(ctx, b.CharID, b.ShipID)
}

// parkAtCelestialEntity puts an abstract ship on top of an entity (a container),
// as opposed to a mapDenormalize celestial. parkAtCelestial reads the static map;
// a wreck's spoils are an entity row, so the position comes from there.
func (m *Mod) parkAtCelestialEntity(ctx context.Context, shipID, itemID int64) {
	var x, y, z float64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
		itemID).Scan(&x, &y, &z); err != nil {
		return
	}
	m.persistPosition(ctx, shipID, itemID, x, y, z, 0)
}

// jumpsBetween is the pilot's own route length between two systems.
func (m *Mod) jumpsBetween(ctx context.Context, b *botRun, from, to int64) (int, error) {
	if from == to {
		return 0, nil
	}
	path, err := m.route(ctx, from, to, rulesForBot(b))
	if err != nil {
		return 0, err
	}
	return len(path), nil
}
