package rolebots

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	"gurimoon/rates"
)

// Embodied mining: the real thing, in the real world.
//
// While a player has the belt's system loaded, a mining rolebot picks a real
// asteroid, approaches it, fires its fitted mining lasers (the OnSpecialFX the
// client draws for a player's beam), depletes the rock's quantity, shrinks its
// ball for everyone on grid and stacks the ore into the ship's cargo as entity
// rows. The abstract sim (mining.go) then only watches the hold fill (see
// miningStep's embodied branch). With nobody watching, the abstract timer stands in.

const (
	// How far out of the belt centre a bot will look for its ore.
	mineScanRange = 100_000.0
	// Poll cadence while waiting for room in the hold.
	mineHoldFullSleep = 15 * time.Second
)

// minerModule is one fitted mining laser, resolved to its real numbers.
type minerModule struct {
	ItemID   int64
	TypeID   int64
	Kind     minerKind
	CycleSec float64
	YieldM3  float64 // per cycle, after skills, ship bonus and server rate
	MaxRange float64
}

// startEmbodyMining launches the mining loop for a projection. It takes eb.mu
// only, so it must not be called with embodyMu held (see embodyWorkMu).
func (m *Mod) startEmbodyMining(eb *embodiedBot) {
	ctx, cancel := context.WithCancel(context.Background())
	eb.mu.Lock()
	if eb.mineCancel != nil {
		eb.mineCancel()
	}
	eb.mineCancel = cancel
	// A fresh session gets a fresh view of the belts: rocks respawn, and a stale dry
	// flag would bounce the sim out of a plan it just made.
	eb.oreDryBelts, eb.oreDry = nil, false
	corpID, sysID := eb.corpID, eb.systemID
	eb.mu.Unlock()
	// Who else is working these belts (corprelations_hooks.go).
	m.noteBeltPresence(ctx, corpID, sysID)
	// The longest-lived per-bot goroutine: it outlives several roster passes, and
	// every check in it reads through an entity the projection can lose.
	m.goSafeBot(eb.charID, "mining loop", func() { m.embodyMineLoop(ctx, eb) })
}

// stopEmbodyMining cancels the loop and kills the beams.
func (m *Mod) stopEmbodyMining(eb *embodiedBot) {
	eb.mu.Lock()
	cancel := eb.mineCancel
	eb.mineCancel = nil
	eb.mu.Unlock()
	if cancel != nil {
		cancel()
		// And the mining drones come home with the lasers off.
		m.dronesReturn(eb, grpMiningDrone)
	}
}

// embodyMineLoop is one bot's mining session: pick a rock, close to it, run
// cycles until it is empty or the hold is full, repeat.
func (m *Mod) embodyMineLoop(ctx context.Context, eb *embodiedBot) {
	dbCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	miners, err := m.loadMiners(dbCtx, eb.charID, eb.shipID)
	rockGroup := m.oreGroupOf(dbCtx, eb.oreTypeID)
	cancel()
	if err != nil {
		rlogf("rolebots: embodied mining %s: %v", eb.charName, err)
		return
	}
	if len(miners) == 0 {
		m.logBot(eb.charID, logWarn, "mining", "no mining lasers fitted — nothing to fire in space")
		return
	}
	// Only the modules this rock answers to fire at it (the core's mining gate,
	// minercap.go). A Miner I pointed at Mercoxit takes nothing out.
	works := miners[:0]
	for _, mm := range miners {
		if minerWorksRock(mm.Kind, rockGroup) {
			works = append(works, mm)
		}
	}
	miners = works
	if len(miners) == 0 {
		m.logBot(eb.charID, logWarn, "mining",
			"nothing fitted can work %s — the rock needs a different kind of mining module", eb.oreName)
		return
	}
	// A harvester's raw duration is not its cycle: Ice Harvesting, the barge, the upgrades and
	// the hardwirings shorten it. The abstract leg is priced on iceCycleMs, so the real cycle is too.
	if eb.iceFieldID != 0 {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		iceSec := m.iceCycleMs(cctx, &botRun{CharID: eb.charID, ShipID: eb.shipID}) / 1000
		ccancel()
		for i := range miners {
			if miners[i].Kind == minerIce && iceSec > 0 {
				miners[i].CycleSec = iceSec
			}
		}
	}
	oreVolume := 0.1
	{
		vctx, vcancel := context.WithTimeout(context.Background(), 5*time.Second)
		if v, err := evedb.GetTypeVolume(vctx, m.db, eb.oreTypeID); err == nil && v > 0 {
			oreVolume = v
		}
		vcancel()
	}
	shortestRange := miners[0].MaxRange
	cycle := miners[0].CycleSec
	var cycleYieldM3 float64
	for _, mm := range miners {
		if mm.MaxRange < shortestRange {
			shortestRange = mm.MaxRange
		}
		if mm.CycleSec > cycle {
			cycle = mm.CycleSec
		}
		cycleYieldM3 += mm.YieldM3
	}
	// The lasers reach shortestRange; the ship circles well inside that so
	// the orbit never carries it out of range mid-cycle.
	orbitDist := orbitRangeFor(shortestRange)
	reach := shortestRange * navRangeSlack

	m.logBot(eb.charID, logInfo, "mining",
		"lasers hot: %d× miner, %.0f m³ per %.0fs cycle, optimal %.0fkm — orbiting %s rocks at %.1fkm",
		len(miners), cycleYieldM3, cycle, shortestRange/1000, eb.oreName, orbitDist/1000)

	defer m.mineFX(eb, miners, 0, false, 0)

	// Rocks this session could not reach are skipped, so the bot moves on instead
	// of staring at one.
	unreachable := map[uint32]bool{}

	for {
		if ctx.Err() != nil {
			return
		}
		// Room left?
		full, err := m.holdFull(ctx, eb.charID, eb.shipID)
		if err != nil {
			rlogf("rolebots: embodied mining %s: hold check: %v", eb.charName, err)
		}
		if full {
			m.mineFX(eb, miners, 0, false, 0)
			eb.setMineTarget(0)
			m.dronesReturn(eb, grpMiningDrone)
			if !embodySleep(ctx, mineHoldFullSleep) {
				return
			}
			continue
		}

		// Stay on the rock being shot until it runs dry, then take the nearest of the
		// planned ore. Re-picking each cycle would swap targets as the orbit passes one.
		rockID, rockPos, ok := m.currentRock(eb)
		if !ok {
			rockID, rockPos, ok = m.pickRock(eb, unreachable)
		}
		if !ok && eb.iceFieldID != 0 {
			// An ice trip works one field. Off it, warp there; on it and empty, the field is
			// worked out and the abstract tier re-plans (embodiedOreDry).
			m.mineFX(eb, miners, 0, false, 0)
			eb.setMineTarget(0)
			m.iceFieldMissing(eb)
			if !embodySleep(ctx, 20*time.Second) {
				return
			}
			continue
		}
		if !ok {
			// This belt is out of the ore the bot came for: hop to the next one (the wander
			// logic warps miners belt to belt).
			m.mineFX(eb, miners, 0, false, 0)
			eb.setMineTarget(0)
			eb.mu.Lock()
			warping := eb.warping
			eb.mu.Unlock()
			if !warping {
				// Remember this belt came up dry: once they all have, the abstract tier
				// re-plans the trip.
				m.noteBeltDry(eb)
				m.logBot(eb.charID, logInfo, "mining",
					"no %s left within %.0fkm — moving to another belt", eb.oreName, mineScanRange/1000)
				// Discretionary, and a refusal costs nothing: the 20 s sleep below is this
				// loop's own retry.
				m.startEmbodyWarp(eb)
				// Different belt, different rocks: the old skips are moot.
				unreachable = map[uint32]bool{}
			}
			if !embodySleep(ctx, 20*time.Second) {
				return
			}
			continue
		}

		// Circle it like a miner would, and hold fire until the orbit has
		// actually brought the rock inside laser range.
		eb.setMineTarget(rockID)
		if !m.closeTo(ctx, eb, rockID, rockPos, orbitDist, reach) {
			if ctx.Err() != nil {
				return
			}
			m.mineFX(eb, miners, 0, false, 0)
			eb.setMineTarget(0)
			unreachable[rockID] = true
			m.logBot(eb.charID, logInfo, "mining",
				"asteroid %d never came inside %.0fkm — taking another rock", rockID, reach/1000)
			continue
		}

		// Drones onto the same rock (drones_embodied.go); they deliver into
		// the hold on their own cycle, which holdFull above already reads.
		m.droneMine(eb, rockID)

		// Beams on, then one cycle of work.
		m.mineFX(eb, miners, rockID, true, int64(cycle*1000))
		if !embodySleep(ctx, time.Duration(cycle*float64(time.Second))) {
			m.mineFX(eb, miners, rockID, false, 0)
			return
		}
		if ctx.Err() != nil {
			return
		}

		units := int64(cycleYieldM3 / oreVolume)
		if units <= 0 {
			units = 1
		}
		if err := m.mineYieldCycle(ctx, eb, miners, rockID, units, oreVolume); err != nil {
			rlogf("rolebots: embodied mining %s: %v", eb.charName, err)
			if !embodySleep(ctx, 10*time.Second) {
				return
			}
		}
	}
}

// mineYieldCycle takes one cycle's ore out of the rock and puts it in the hold,
// in the order the player mining loop uses.
func (m *Mod) mineYieldCycle(ctx context.Context, eb *embodiedBot, miners []minerModule,
	rockID uint32, units int64, oreVolume float64) error {

	ss := eb.ss
	if ss.GetAsteroidQty(rockID) <= 0 {
		m.mineFX(eb, miners, rockID, false, 0)
		eb.setMineTarget(0)
		return nil
	}
	// Never overfill the hold: take only what fits.
	dbCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A full hold is zero room, not "no limit" (holdCapUnits).
	free, err := m.holdFreeM3(dbCtx, eb.charID, eb.shipID)
	if err != nil {
		return fmt.Errorf("hold check: %w", err)
	}
	units = holdCapUnits(units, free, oreVolume)
	if units <= 0 {
		return nil
	}

	actual := ss.DecrementAsteroidQty(rockID, units)
	if newRadius, shrank := ss.UpdateAsteroidRadius(rockID); shrank {
		m.broadcastRockRadius(ss, rockID, newRadius)
	}
	if actual <= 0 {
		ss.MarkAsteroidDepleted(rockID)
		m.mineFX(eb, miners, rockID, false, 0)
		m.removeRockFromClients(ss, rockID)
		eb.setMineTarget(0)
		return nil
	}
	// Credit the rock's own type, not the family base (destiny.BaseOreType) the
	// plan was made for: a Massive Scordite rock pays Massive Scordite.
	minedType := eb.oreTypeID
	if rockType := ss.GetAsteroidTypeID(rockID); rockType != 0 {
		minedType = destiny.MinedOreType(rockType)
	}
	if err := m.addCargoStackFor(dbCtx, eb.charID, eb.shipID, minedType, actual); err != nil {
		return fmt.Errorf("cargo stack: %w", err)
	}
	// The embodied half of the ore counter (statcount.go): a pilot mines under both
	// tiers, and the leaderboard is one number across both.
	bumpStats(eb.charID, func(d *statDelta) {
		d.OreUnits += actual
		d.OreM3 += float64(actual) * oreVolume
	})
	// The embodied half of the ore-flow ledger (oreflow.go): the abstract tier books
	// mined rock when it loads the hold, which a real cycle never passes.
	m.noteOreFlow(dbCtx, minedType, oreStageMined, actual, 0)
	m.emitOreMined(dbCtx, eb.charID, minedType, actual, float64(actual)*oreVolume, ss.ID, false)
	if ss.GetAsteroidQty(rockID) <= 0 {
		ss.MarkAsteroidDepleted(rockID)
		m.mineFX(eb, miners, rockID, false, 0)
		m.removeRockFromClients(ss, rockID)
		eb.setMineTarget(0)
		m.logBot(eb.charID, logInfo, "mining",
			"mined asteroid %d dry (+%d %s this cycle)", rockID, actual, eb.oreName)
	}
	return nil
}

// pickRock finds the nearest non-empty asteroid of the planned ore, ignoring
// the ones this session already failed to reach.
func (m *Mod) pickRock(eb *embodiedBot, skip map[uint32]bool) (uint32, destiny.Vec3, bool) {
	if eb.ent == nil || eb.ent.Manager == nil {
		return 0, destiny.Vec3{}, false
	}
	pos := eb.ent.Pos()
	var bestID uint32
	var bestPos destiny.Vec3
	bestDist := math.MaxFloat64
	for _, a := range eb.ss.AsteroidsInRange(pos, mineScanRange) {
		if skip[a.EntityID] {
			continue
		}
		// Ice has no ore family: any grade of ice is the trip's ice.
		if eb.iceFieldID != 0 {
			if !m.isIceType(context.Background(), a.TypeID) {
				continue
			}
		} else if destiny.BaseOreType(a.TypeID) != eb.oreTypeID {
			continue
		}
		rockPos, ok := eb.ss.GetAsteroidPosition(a.EntityID)
		if !ok {
			continue
		}
		d := rockPos.Sub(pos).Len()
		if d < bestDist {
			bestID, bestPos, bestDist = a.EntityID, rockPos, d
		}
	}
	return bestID, bestPos, bestID != 0
}

// iceFieldMissing runs when an ice trip finds no ice rock in reach. Away from the trip's
// field it warps there; on the field (or with the field not in this system) it raises
// oreDry, which the abstract iceWorkStep reads to end the trip.
func (m *Mod) iceFieldMissing(eb *embodiedBot) {
	eb.mu.Lock()
	warping, field := eb.warping, eb.iceFieldID
	eb.mu.Unlock()
	if warping {
		return
	}
	if beltUnderfoot(eb) != field {
		if c := celestialByID(eb.celestials, field); c != nil {
			// Discretionary like any miner's hop: a refusal costs nothing, the caller's sleep retries.
			m.startEmbodyWarpAs(eb, *c, false, warpDiscretionary)
			return
		}
	}
	eb.mu.Lock()
	raise := !eb.oreDry
	eb.oreDry = true
	eb.mu.Unlock()
	if raise {
		m.logBot(eb.charID, logInfo, "mining", "no ice left on this field — asking the planner for somewhere else")
	}
}

// currentRock re-resolves the asteroid the bot is already shooting, as long
// as it still has ore in it.
func (m *Mod) currentRock(eb *embodiedBot) (uint32, destiny.Vec3, bool) {
	eb.mu.Lock()
	id := eb.mineTarget
	eb.mu.Unlock()
	if id == 0 || eb.ss == nil || eb.ss.GetAsteroidQty(id) <= 0 {
		return 0, destiny.Vec3{}, false
	}
	pos, ok := eb.ss.GetAsteroidPosition(id)
	if !ok {
		return 0, destiny.Vec3{}, false
	}
	return id, pos, true
}

// ── module effects ──────────────────────────────────────────────────────────

// mineFX starts or stops the mining beams on every bubble-mate's client.
// This is the same OnSpecialFX payload a player's module sends; the bot is
// the source of the packet since it has no Conn of its own.
func (m *Mod) mineFX(eb *embodiedBot, miners []minerModule, targetID uint32, start bool, durationMs int64) {
	if eb.ss == nil || eb.ent == nil || eb.ent.Manager == nil {
		return
	}
	guid := m.miningFXGUID()
	startVal := int64(0)
	if start {
		startVal = 1
	}
	if durationMs <= 0 {
		durationMs = 60000
	}
	var targetRep marshal.PyRep = marshal.PyNone{}
	if targetID != 0 {
		targetRep = marshal.PyInt{Value: uint64(targetID)}
	}

	var actions []marshal.PyRep
	for _, mod := range miners {
		fxArgs := []marshal.PyRep{
			marshal.PyInt{Value: uint64(eb.ent.Manager.EntityID)}, // entityID
			marshal.PyInt{Value: uint64(mod.ItemID)},              // moduleID
			marshal.PyInt{Value: uint64(mod.TypeID)},              // moduleTypeID
			targetRep,                                             // targetID
			marshal.PyNone{},                                      // otherTypeID (no crystal)
			marshal.PyList{},                                      // area
			marshal.PyString{Value: guid},                         // effect GUID
			marshal.PyInt{Value: 0},                               // isOffensive
			marshal.PyInt{Value: uint64(startVal)},                // start
			marshal.PyInt{Value: uint64(startVal)},                // active
		}
		if start {
			fxArgs = append(fxArgs,
				marshal.PyReal{Value: float64(durationMs)},
				marshal.PyInt{Value: destiny.FXRepeatCount},
			)
		}
		actions = append(actions, destiny.BuildSimpleAction("OnSpecialFX", fxArgs...))
	}
	if len(actions) == 0 {
		return
	}
	for _, mate := range eb.ss.GetBubbleConnectedPlayersExcept(eb.ent.ID) {
		if mate.Conn == nil || !mate.KnowsBall(eb.ent.ID) {
			continue
		}
		if err := mate.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionListEvents(mate.Conn, m.nodeID, mate.AccountID, stamp,
				nil, actions...)
		}); err != nil {
			rlogf("rolebots: mining FX to %s: %v", mate.Addr, err)
		}
	}
}

// miningFXGUID resolves the beam the client draws: the dgmEffects row for
// miningLaser, else the hand-written fallback (ship-fitted miners use the turret
// laser visual; effects.Mining is drone-only).
func (m *Mod) miningFXGUID() string {
	miningGUIDOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var guid string
		_ = m.db.QueryRowContext(ctx,
			`SELECT COALESCE(guid, '') FROM dgmEffects WHERE effectName = ?`,
			dogma.EffNameMiningLaser).Scan(&guid)
		if guid == "" {
			guid, _ = dogma.FallbackGUID(dogma.EffNameMiningLaser)
		}
		miningGUID = dogma.NormalizeClientGUID(guid)
	})
	return miningGUID
}

var (
	miningGUIDOnce sync.Once
	miningGUID     string
)

// ── asteroid broadcasts ─────────────────────────────────────────────────────

// broadcastRockRadius pushes a mined rock's new size to everyone holding it,
// so two pilots on the same rock never disagree about how much is left.
func (m *Mod) broadcastRockRadius(ss *destiny.SolarSystem, rockID uint32, radius float64) {
	if radius <= 0 {
		return
	}
	// System-wide scan with a KnowsBall filter: a rock is an asteroid ball, not a
	// destiny.Entity, so GetBubbleConnectedPlayersExcept returns nil for it.
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.KnowsBall(rockID) {
			continue
		}
		// Queued, so a held queue collapses a cycle's intermediate sizes into the final
		// radius, the only one worth drawing.
		p.QueueAction("rad:"+strconv.FormatUint(uint64(rockID), 10), rockID,
			destiny.BuildSimpleAction("SetBallRadius",
				marshal.PyInt{Value: uint64(rockID)},
				marshal.PyReal{Value: radius}))
	}
}

// removeRockFromClients drops a mined-out rock from every client on grid.
func (m *Mod) removeRockFromClients(ss *destiny.SolarSystem, rockID uint32) {
	// System-wide for the same reason as broadcastRockRadius above.
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.KnowsBall(rockID) {
			continue
		}
		// Any queued "rad:" shrink for the same rock is dropped first. The removal goes
		// through QueueRemoveBall, which retires the ball when the packet leaves.
		p.DropPending(rockID)
		p.QueueRemoveBall("rm:"+strconv.FormatUint(uint64(rockID), 10), rockID)
	}
}

// ── ship state ──────────────────────────────────────────────────────────────

// loadMiners resolves the ship's fitted mining lasers into real numbers:
// cycle time, optimal range and per-cycle yield at the pilot's own skills.
func (m *Mod) loadMiners(ctx context.Context, charID, shipID int64) ([]minerModule, error) {
	fitted, err := evedb.GetFittedModules(ctx, m.db, shipID)
	if err != nil {
		return nil, fmt.Errorf("fitted modules: %w", err)
	}
	skills, err := m.charSkillsFor(ctx, charID)
	if err != nil {
		return nil, fmt.Errorf("skills: %w", err)
	}
	skillMult := (1 + 0.05*float64(skills[skillMining])) * (1 + 0.05*float64(skills[skillAstrogeology]))
	// The mining hardwirings, the term the core's activation path folds.
	skillMult *= evedb.GetCharImplantMults(ctx, m.db, charID).MiningYield
	shipMult := 1.0
	if bonus, err := evedb.GetMiningYieldBonus(ctx, m.db, shipID); err == nil && bonus > 0 {
		shipMult = bonus
	}
	shipMult *= m.hullMiningMult(ctx, charID, shipID)

	var out []minerModule
	for _, fm := range fitted {
		if !isMiningModule(fm.GroupID) {
			continue
		}
		attrs, err := evedb.GetTypeAttributes(ctx, m.db, fm.TypeID)
		if err != nil {
			continue
		}
		amount := attrs[dogma.AttrMiningAmount]
		if amount <= 0 {
			continue
		}
		mod := minerModule{
			ItemID:   fm.ItemID,
			TypeID:   fm.TypeID,
			Kind:     minerKindOf(attrs),
			CycleSec: attrs[dogma.AttrDuration] / 1000,
			MaxRange: attrs[dogma.AttrMaxRange],
			YieldM3:  amount * skillMult * shipMult * rates.MiningYield,
		}
		if mod.CycleSec <= 0 {
			mod.CycleSec = miningCycleSec
		}
		if mod.MaxRange <= 0 {
			mod.MaxRange = 15000
		}
		out = append(out, mod)
	}
	return out, nil
}

// holdFull reports whether the cargo hold has no usable room left.
func (m *Mod) holdFull(ctx context.Context, charID, shipID int64) (bool, error) {
	free, err := m.holdFreeM3(ctx, charID, shipID)
	if err != nil {
		return false, err
	}
	return free <= 0.01, nil
}

// holdFreeM3 is the ship's remaining cargo volume.
func (m *Mod) holdFreeM3(ctx context.Context, charID, shipID int64) (float64, error) {
	var hullTypeID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, shipID).Scan(&hullTypeID); err != nil {
		return 0, fmt.Errorf("hull type: %w", err)
	}
	capacity, err := evedb.GetShipCargoCapacityFitted(ctx, m.db, hullTypeID, shipID)
	if err != nil {
		return 0, fmt.Errorf("cargo capacity: %w", err)
	}
	used, err := evedb.GetCargoUsedVolume(ctx, m.db, charID, shipID)
	if err != nil {
		return 0, fmt.Errorf("cargo used: %w", err)
	}
	// An overfilled hold (a jettison that did not fit, a hull swap under a full
	// load) has zero room, not a negative volume.
	if free := capacity - used; free > 0 {
		return free, nil
	}
	return 0, nil
}

// holdCapUnits is the most of units, at unitVol m³ each, that free m³ of
// hold takes. Zero room takes nothing; a unit with no known volume is not
// limited. Pure, so the harness pins it.
func holdCapUnits(units int64, free, unitVol float64) int64 {
	if units <= 0 {
		return 0
	}
	if unitVol <= 0 {
		return units
	}
	if free <= 0 {
		return 0
	}
	if fit := int64(math.Floor(free/unitVol + 1e-9)); fit < units {
		return fit
	}
	return units
}

func (eb *embodiedBot) setMineTarget(id uint32) {
	eb.mu.Lock()
	eb.mineTarget = id
	eb.mu.Unlock()
}
