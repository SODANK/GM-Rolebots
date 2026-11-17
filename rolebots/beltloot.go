package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// How a belt hunter is paid.
//
// In a system a player has loaded, the core spawns the loot container off its own NPC death path and
// the hunter is pointed at it (noteBeltLoot). In unwatched space the abstract kill rolls the same
// tables with the same roll (evedb.RollNPCLootTables) and puts the result in the hold.
// Usable gear is listed at the pilot's ask (going rate times Tuning.LootMarkup); junk goes to the
// best bid on the dock. Every unit picked up is written to rolebot_loot_hold and the sale works from
// that, capped by what is still in the hold, so the pilot's own reload ammunition is not sold.

const (
	// A hunter goes home when the hold is this full. Short of full, so it can still pick up.
	lootHaulAtFrac = 0.75
	// The most jumps a hunter crosses for a can it left behind. Rat cans are small and despawn.
	ratCanRecoveryJumps = 2
	// How long it keeps trying to get one can open before writing it off.
	lootCanAttemptSec = 5 * 60
	// How long a listed piece of loot rests on the market. Longer than a speculative haul
	// (tradeListDays): the pilot has nothing invested and the order sweep walks the price down.
	lootListDays = 30
	// Below this value a stack is not worth an order slot. It goes to a bid or stays for the trade planner.
	lootListMinValue = 20_000.0

	// The ranking cost of a jump off the hunting ground, and how far the search looks. Steeper than the
	// trade planner's specDestJumpCost: a hunter's jumps are time not spent hunting.
	lootHubJumpCost      = 0.35
	lootMarketMaxJumps   = 15
	lootMarketCandidates = 8
)

// ── the ledger ──────────────────────────────────────────────────────────────

// lootRow is one unsold stack a pilot is carrying.
type lootRow struct {
	TypeID   int64
	TypeName string
	Units    int64
}

// noteLootTaken records units of a type into the pilot's loot ledger.
func (m *Mod) noteLootTaken(ctx context.Context, charID, typeID int64, typeName string, units int64) {
	if units <= 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_loot_hold (characterID, typeID, typeName, units, takenAt)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			units = units + VALUES(units),
			typeName = VALUES(typeName),
			takenAt = VALUES(takenAt)`,
		charID, typeID, typeName, units, clock.Unix()); err != nil {
		rlogf("rolebots: note loot char=%d type=%d: %v", charID, typeID, err)
		return
	}
	noteLootCarrier(charID, true)
}

// lootLedger is everything this pilot is carrying that it means to sell.
func (m *Mod) lootLedger(ctx context.Context, charID int64) ([]lootRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, typeName, units FROM rolebot_loot_hold
		WHERE characterID = ? AND units > 0
		ORDER BY takenAt`, charID)
	if err != nil {
		return nil, fmt.Errorf("loot ledger: %w", err)
	}
	defer rows.Close()
	var out []lootRow
	for rows.Next() {
		var r lootRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.Units); err != nil {
			return nil, fmt.Errorf("loot ledger scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// clearLootLedger forgets a pilot's haul once the sale pass has disposed of it.
func (m *Mod) clearLootLedger(ctx context.Context, charID int64) {
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rolebot_loot_hold WHERE characterID = ?`, charID); err != nil {
		rlogf("rolebots: clear loot ledger char=%d: %v", charID, err)
		return
	}
	noteLootCarrier(charID, false)
}

// ── the drop, unwatched ─────────────────────────────────────────────────────

// lootDrop is one rolled item, before it is anywhere.
type lootDrop struct {
	TypeID   int64
	TypeName string
	Qty      int64
	Volume   float64
}

// lootAbstractKills empties the wrecks of an unwatched fight into the hold. Only tasks that declare
// Loots (task.go) call it. The roll uses the core's tables, gates and rates.LootDrop. What the hold
// cannot take is left in one can at the ship (dropOverflowCan), as the wrecks would be on grid.
func (m *Mod) lootAbstractKills(ctx context.Context, b *botRun, spawn []*ratThreat) {
	if b.ShipID == 0 || !lootsPhase(b.State.Phase) {
		return
	}
	free, err := m.freeHoldM3(ctx, b)
	if err != nil {
		return
	}
	free = math.Max(free, 0)

	took, stacks := 0.0, 0
	var left []lootDrop
	defer func() { m.dropOverflowCan(ctx, b, left) }()
	for _, r := range spawn {
		if r == nil || r.EHP > 0 {
			continue // still flying: its wreck is somebody else's problem
		}
		drops, err := m.rollRatLoot(ctx, r.TypeID, r.Role)
		if err != nil {
			rlogf("rolebots: rat loot type=%d: %v", r.TypeID, err)
			continue
		}
		for _, d := range drops {
			units := d.Qty
			if d.Volume > 0 {
				fits := int64((free - took) / d.Volume)
				if fits < 0 {
					fits = 0
				}
				if units > fits {
					units = fits
				}
			}
			if rest := d.Qty - units; rest > 0 {
				// No room for this much; a smaller stack may still fit. The rest stays in space.
				o := d
				o.Qty = rest
				left = append(left, o)
			}
			if units <= 0 {
				continue
			}
			if err := m.addCargoStack(ctx, b, d.TypeID, units); err != nil {
				rlogf("rolebots: loot into hold char=%d type=%d: %v", b.CharID, d.TypeID, err)
				continue
			}
			m.noteLootTaken(ctx, b.CharID, d.TypeID, d.TypeName, units)
			// A fleet member's haul is its own; the after-action report still lists it (gangreport.go).
			if gid := gangIDOf(b); gid != 0 {
				m.noteGangHaul(ctx, gid, b.CharID, haulLoot, d.TypeID, d.TypeName, units, 0, 0)
			}
			took += float64(units) * d.Volume
			stacks++
		}
	}
	if stacks > 0 {
		m.logBot(b.CharID, logInfo, "combat",
			"went through the wrecks — %d stack(s), %.1f m³ into the hold", stacks, took)
	}
}

// settingOverflowCanMinISK is the value a hold's overflow must reach before it is left in a can.
// Below it the loot is lost, which bounds how many cans the abstract tier puts in space.
const (
	settingOverflowCanMinISK = "rolebots.loot.overflowCanMinISK"
	defaultOverflowCanMinISK = 1e6
)

// lootDropsValue prices a set of drops with the killboard valuation (evedb.TypeValues). Pure.
// Unpriced types count nothing.
func lootDropsValue(drops []lootDrop, prices map[int64]evedb.TypeValue) float64 {
	total := 0.0
	for _, d := range drops {
		if p, ok := prices[d.TypeID]; ok && p.Value > 0 && d.Qty > 0 {
			total += p.Value * float64(d.Qty)
		}
	}
	return total
}

// dropOverflowCan leaves the loot the hold could not take in one cargo container at the ship,
// owned by the pilot, so a scout, a thief or a player can contest it. It is registered as a module
// can (gangID 0) like dropWreckLoot's, so it is openable, reloads and expires. Overflow worth less
// than rolebots.loot.overflowCanMinISK is left to burn.
func (m *Mod) dropOverflowCan(ctx context.Context, b *botRun, left []lootDrop) {
	if len(left) == 0 || b.ShipID == 0 || b.SystemID == 0 {
		return
	}
	ids := make([]int64, 0, len(left))
	for _, d := range left {
		ids = append(ids, d.TypeID)
	}
	prices, _ := evedb.TypeValues(ctx, m.db, ids)
	value := lootDropsValue(left, prices)
	if value < evedb.CachedSettingFloat(settingOverflowCanMinISK, defaultOverflowCanMinISK, 0, 1e12) {
		return
	}
	var x, y, z float64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
		b.ShipID).Scan(&x, &y, &z); err != nil {
		return
	}
	canID, err := evedb.CreateLootContainer(ctx, m.db, b.CharID, b.SystemID, x, y, z)
	if err != nil {
		rlogf("rolebots: overflow can char=%d: %v", b.CharID, err)
		return
	}
	stacks := 0
	for _, d := range left {
		if err := evedb.CreateNPCLootItem(ctx, m.db, canID, b.CharID, d.TypeID, d.Qty); err != nil {
			rlogf("rolebots: overflow can item can=%d type=%d: %v", canID, d.TypeID, err)
			continue
		}
		stacks++
	}
	if stacks == 0 {
		if err := evedb.DeleteEntity(ctx, m.db, canID); err != nil {
			rlogf("rolebots: overflow can cleanup %d: %v", canID, err)
		}
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_gang_cans (canItemID, gangID, ownerCharID, systemID, beltItemID, createdAt)
		VALUES (?, 0, ?, ?, 0, ?)`,
		canID, b.CharID, b.SystemID, clock.Unix()); err != nil {
		rlogf("rolebots: overflow can registry %d: %v", canID, err)
	}
	if m.systems != nil {
		if ss := m.systems.Get(b.SystemID); ss != nil {
			m.registerCanBall(ss, canID, b.CharID, destiny.Vec3{X: x, Y: y, Z: z})
		}
	}
	m.logBot(b.CharID, logInfo, "combat",
		"hold full — left %d stack(s) worth %s in a can at the wrecks", stacks, fmtISK(value))
}

// rollRatLoot rolls one dead rat's authored drop table with the core's roll
// (evedb.RollNPCLootTables, the same one a watched kill's container gets): the merged type and
// group tables, the role pick, the bounty pick, the gated general haul and the max-drops cap.
// An empty role is an ordinary rat.
func (m *Mod) rollRatLoot(ctx context.Context, npcTypeID int64, role string) ([]lootDrop, error) {
	groupID := m.typeGroupOf(ctx, npcTypeID)
	if groupID == 0 {
		return nil, fmt.Errorf("rat group %d: unknown type", npcTypeID)
	}
	rolled, err := evedb.RollNPCLootTables(ctx, m.db, npcTypeID, groupID, role, evedb.GlobalLootRNG)
	if err != nil {
		return nil, fmt.Errorf("rat loot table: %w", err)
	}
	var out []lootDrop
	for _, d := range rolled {
		if d.Qty <= 0 {
			continue
		}
		var name string
		var volume float64
		if err := m.db.QueryRowContext(ctx,
			`SELECT typeName, COALESCE(volume, 0.01) FROM invTypes WHERE typeID = ?`,
			d.ItemTypeID).Scan(&name, &volume); err != nil {
			continue // a drop table pointing at a type that does not exist
		}
		out = append(out, lootDrop{TypeID: d.ItemTypeID, TypeName: name, Qty: d.Qty, Volume: volume})
	}
	return out, nil
}

// ── the drop, watched ───────────────────────────────────────────────────────

// noteBeltLoot points a hunter at the can the core spawned over its dead rat. The container is
// created asynchronously off the victim's death, so the search is deferred and keyed on position.
func (m *Mod) noteBeltLoot(eb *embodiedBot, victim *destiny.Entity) {
	if victim == nil {
		return
	}
	pos := victim.Pos()
	charID, sysID, p := eb.charID, eb.systemID, pos
	eb.mu.Lock()
	onSite := eb.siteID != 0
	eb.mu.Unlock()
	// A site runner sweeps its own field once the room is quiet (siteSweepField); a can noted here
	// would send it to the loot task and off the site.
	if onSite {
		return
	}
	m.goSafeBot(charID, "belt-loot note", func() {
		time.Sleep(4 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Read-modify-write of the pilot's state row off both lanes, so it takes the per-pilot step lock
		// (simpool.go).
		unlock := lockBotStep(charID)
		defer unlock()

		st, err := loadState(ctx, m.db, charID)
		if err != nil {
			return
		}
		// Only a task that loots, and one can at a time: a hunter re-pointed at every wreck would empty none.
		if !lootsPhase(st.Phase) || st.Data.HuntLootID != 0 {
			return
		}
		id := m.containerNear(ctx, sysID, p)
		if id == 0 {
			return
		}
		st.Data.HuntLootID, st.Data.HuntLootSysID = id, sysID
		st.Data.HuntLootAt = clock.Unix()
		if err := saveState(ctx, m.db, st); err != nil {
			rlogf("rolebots: note belt loot char=%d: %v", charID, err)
			return
		}
		m.logBot(charID, logInfo, "combat", "the rats left something worth opening")
	})
}

// ── collecting it ───────────────────────────────────────────────────────────

// combatResumeLoot puts a hunter back on a can it has not emptied.
// Reports whether it took the pass.
func (m *Mod) combatResumeLoot(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if st.Data.HuntLootID == 0 {
		return false, nil
	}
	// Gone: despawned, or somebody else was quicker.
	var one int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT 1 FROM entity WHERE itemID = ?`, st.Data.HuntLootID).Scan(&one); err != nil {
		st.Data.HuntLootID, st.Data.HuntLootSysID, st.Data.HuntLootAt = 0, 0, 0
		return false, saveState(ctx, m.db, st)
	}
	if st.Data.HuntLootSysID != b.SystemID {
		jumps, err := m.jumpsBetween(ctx, b, b.SystemID, st.Data.HuntLootSysID)
		if err != nil || jumps > ratCanRecoveryJumps {
			st.Data.HuntLootID, st.Data.HuntLootSysID, st.Data.HuntLootAt = 0, 0, 0
			return false, saveState(ctx, m.db, st)
		}
	}
	st.Activity = ActCombat
	st.Phase = string(TaskCombatLoot)
	st.Data.HuntLootAt = clock.Unix()
	if b.SystemID != st.Data.HuntLootSysID {
		return true, m.beginTravel(ctx, b, st.Data.HuntLootSysID, 0, string(TaskCombatLoot))
	}
	return true, saveState(ctx, m.db, st)
}

// combatLootStep empties one can into the hold. It runs in the abstract tier even when a projection
// exists, so an unwatched hunter is paid like a watched one; the projection adds only the flying.
func (m *Mod) combatLootStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if st.Data.HuntLootID == 0 {
		st.Phase = lootReturnPhase(st)
		return false, saveState(ctx, m.db, st)
	}
	if b.SystemID != st.Data.HuntLootSysID {
		return false, m.beginTravel(ctx, b, st.Data.HuntLootSysID, 0, string(TaskCombatLoot))
	}
	if b.ShipID == 0 {
		return false, fmt.Errorf("belt hunter has no ship to load loot into")
	}
	if clock.Unix()-st.Data.HuntLootAt > lootCanAttemptSec {
		st.Data.HuntLootID, st.Data.HuntLootSysID, st.Data.HuntLootAt = 0, 0, 0
		st.Phase = lootReturnPhase(st)
		return false, saveState(ctx, m.db, st)
	}

	free, err := m.freeHoldM3(ctx, b)
	if err != nil {
		return false, err
	}
	if free <= 0 {
		return false, m.combatStartSale(ctx, b, "the hold is full")
	}

	if !m.isEmbodied(b.CharID) {
		m.parkAtCelestialEntity(ctx, b.ShipID, st.Data.HuntLootID)
	}

	// What is in the can before and after: takeCanContents reports m3 and the ledger needs units per type.
	before, err := m.canContents(ctx, st.Data.HuntLootID)
	if err != nil {
		return false, err
	}
	taken, _, err := m.takeCanContents(ctx, b.CharID, b.ShipID, st.Data.HuntLootID, b.SystemID, free)
	if err != nil {
		return false, err
	}
	after, _ := m.canContents(ctx, st.Data.HuntLootID)
	for typeID, was := range before {
		got := was.Units - after[typeID].Units
		if got > 0 {
			m.noteLootTaken(ctx, b.CharID, typeID, was.TypeName, got)
		}
	}
	if taken > 0 {
		m.logBot(b.CharID, logInfo, "combat", "took %.1f m³ out of the can", taken)
	}

	empty, err := evedb.IsContainerEmpty(ctx, m.db, st.Data.HuntLootID)
	if err != nil || empty || taken <= 0 {
		st.Data.HuntLootID, st.Data.HuntLootSysID, st.Data.HuntLootAt = 0, 0, 0
		// Full enough to be worth the trip home; otherwise back to work.
		if full, err := m.holdMostlyFull(ctx, b); err == nil && full {
			return false, m.combatStartSale(ctx, b, "the hold is getting full")
		}
		st.Phase = lootReturnPhase(st)
		return false, saveState(ctx, m.db, st)
	}
	st.Data.LegEndsAt = clock.Unix() + b.Tune.dwell(10)
	return false, saveState(ctx, m.db, st)
}

// lootReturnPhase is where a pilot goes once a can is done: back to its site when it is running one,
// otherwise back to the belt sweep.
func lootReturnPhase(st *SimState) string {
	// A negative site is an agent mission's pocket (modapi.MissionSiteID).
	if st.Data.SiteID < 0 && st.Data.MissionID != 0 {
		return string(TaskMissionRun)
	}
	if st.Data.SiteID != 0 {
		return string(TaskCombatSite)
	}
	return string(TaskCombatSweep)
}

// canStack is one type inside a container.
type canStack struct {
	TypeName string
	Units    int64
}

// canContents reads a container as a map of type to units.
func (m *Mod) canContents(ctx context.Context, canID int64) (map[int64]canStack, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, COALESCE(t.typeName, ''), SUM(e.quantity)
		FROM entity e LEFT JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.locationID = ?
		GROUP BY e.typeID, t.typeName`, canID)
	if err != nil {
		return nil, fmt.Errorf("can contents %d: %w", canID, err)
	}
	defer rows.Close()
	out := map[int64]canStack{}
	for rows.Next() {
		var typeID, units int64
		var name string
		if err := rows.Scan(&typeID, &name, &units); err != nil {
			return nil, err
		}
		out[typeID] = canStack{TypeName: name, Units: units}
	}
	return out, rows.Err()
}

// holdMostlyFull reports whether it is time to go and sell. A pilot with no ship has no hold, so the
// answer is no, not an error, and a hunter between hulls plans normally.
func (m *Mod) holdMostlyFull(ctx context.Context, b *botRun) (bool, error) {
	if b.ShipID == 0 {
		return false, nil
	}
	var capacity float64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(t.capacity, 0) FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID WHERE e.itemID = ?`,
		b.ShipID).Scan(&capacity); err != nil {
		return false, fmt.Errorf("hold capacity ship=%d: %w", b.ShipID, err)
	}
	if capacity <= 0 {
		return false, nil
	}
	used, err := m.holdUsedM3(ctx, b.CharID, b.ShipID)
	if err != nil {
		return false, err
	}
	return used/capacity >= lootHaulAtFrac, nil
}

// ── selling it ──────────────────────────────────────────────────────────────

// combatStartSale sends the hunter to market with the haul: the best trade system it can reach and the
// best dock in it, using measured liquidity (liquidity.go) and the station score (stationscore.go),
// as the trade planner does (specDestinations). Home is the fallback when nothing is swept or no
// market is in reach.
func (m *Mod) combatStartSale(ctx context.Context, b *botRun, why string) error {
	st := b.State

	// A member of a live fleet keeps hunting with the haul aboard: it may be the op's haul and the
	// settlement round divides it. The gang gate in plan() reclaims the pilot when it next idles.
	if m.gangHoldsThisPilot(ctx, b) {
		m.logBot(b.CharID, logInfo, "gang",
			"%s, but it is on a fleet op — the haul rides along until the fleet stands down", why)
		st.Phase = string(TaskCombatSweep)
		return saveState(ctx, m.db, st)
	}
	// Keep it or give it to the corporation (lootdonate.go): one call per
	// haul, a Greed-and-Loyalty one.
	if took, err := m.combatStartDonation(ctx, b, why); took || err != nil {
		return err
	}

	station, system, mkt := m.lootMarketFor(ctx, b)
	where := ""
	if station != 0 {
		where = fmt.Sprintf("%s — best dock in %s, %s traded there lately",
			m.stationLabel(ctx, station), mkt.Name, iskShort(mkt.Liquidity))
	} else {
		station = m.homeStationFor(ctx, b.CharID)
		if station == 0 {
			// Nowhere to take it: keep hunting. The haul rides along and the operator is told once per trip.
			m.logBot(b.CharID, logWarn, "combat",
				"has a hold full of salvage, no market in reach and no home station to sell it at")
			st.Phase = string(TaskCombatSweep)
			return saveState(ctx, m.db, st)
		}
		loc, err := evedb.GetStationLocation(ctx, m.db, station)
		if err != nil {
			return fmt.Errorf("home station %d: %w", station, err)
		}
		system = loc.SolarSystemID
		where = m.stationLabel(ctx, station) + " — no market in reach, so its own home dock"
	}

	st.Activity = ActCombat
	st.Data.TargetStationID = station
	st.Data.TargetSystemID = system
	m.logBot(b.CharID, logInfo, "combat", "%s — taking the haul to %s", why, where)
	return m.beginTravel(ctx, b, system, station, string(TaskCombatSell))
}

// lootMarketFor answers where this haul is worth selling: the best market system inside the pilot's
// travel rules (rankLootMarkets) and the best dock in it. It answers (0, 0, nil) when nothing in
// reach carries a market, and the caller falls back to home.
func (m *Mod) lootMarketFor(ctx context.Context, b *botRun) (int64, int64, *marketPick) {
	markets, err := m.marketSystems(ctx)
	if err != nil || len(markets) == 0 {
		return 0, 0, nil
	}
	liq := m.liquidity(ctx)
	// Scoped (residency.go): a resident sells at its own hub. There is no ladder; an empty ranking
	// falls back to taking the load home.
	r := m.economicRules(ctx, b, defaultBand(b))
	jumps, err := m.reachFrom(ctx, b.SystemID, r)
	if err != nil {
		return 0, 0, nil
	}
	ranked := rankLootMarkets(markets, liq.System, jumps, m.marketBonus(ctx))
	if len(ranked) == 0 {
		return 0, 0, nil
	}

	// The best dock in each candidate system by station score. The order book only breaks ties. Station
	// manners are the pilot's own (tradeStationClause): no fencing at a despised faction's station.
	cfg := m.tradeConfigDerived(ctx, b)
	stationClause, stationArgs := m.tradeStationClause(ctx, b.Profile, b.Tune, cfg)
	args := append(int64Args(ranked), stationArgs...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.solarSystemID, COUNT(o.orderID) AS book
		  FROM staStations s
		  LEFT JOIN market_orders o ON o.stationID = s.stationID
		       AND o.orderState = 1 AND o.volRemaining > 0
		 WHERE s.solarSystemID IN (`+placeholders(len(ranked))+`)`+stationClause+`
		 GROUP BY s.stationID, s.solarSystemID`, args...)
	if err != nil {
		rlogf("rolebots: loot market: %v", err)
		return 0, 0, nil
	}
	defer rows.Close()
	var cands []dockCandidate
	for rows.Next() {
		var c dockCandidate
		if err := rows.Scan(&c.StationID, &c.SystemID, &c.Book); err != nil {
			return 0, 0, nil
		}
		cands = append(cands, c)
	}
	best := bestDockPerSystem(m.stationScores(ctx), cands)
	// Ranked order decides, station book only breaks the tie inside a
	// system: a huge dock in a dead system is a warehouse, not a market.
	for _, system := range ranked {
		if station := best[system]; station != 0 {
			return station, system, &marketPick{SystemID: system, Name: markets[system],
				Liquidity: liq.System[system]}
		}
	}
	return 0, 0, nil
}

// rankLootMarkets orders reachable market systems by what they are worth to a hunter fencing a haul,
// best first: measured liquidity discounted by the flying (rankMarkets). Jumps cost more than for a
// trader (lootHubJumpCost) and there is a hard ceiling. bonus is marketBonus (fortune.go). Pure.
func rankLootMarkets(markets map[int64]string, liq map[int64]float64, jumps map[int64]int,
	bonus func(int64) float64) []int64 {
	return rankMarkets(markets, liq, jumps, bonus, lootHubJumpCost, lootMarketMaxJumps, lootMarketCandidates)
}

// combatResumeSale picks a haul back up: a hunter that was re-planned (or
// whose activity rotated) while holding loot still has it. Reports whether
// it took the pass.
func (m *Mod) combatResumeSale(ctx context.Context, b *botRun) (bool, error) {
	// Checked before the ledger read: a fleet member keeps its haul aboard (combatStartSale), so there is
	// no sale to resume, and this saves the query on every plan pass.
	if m.gangHoldsThisPilot(ctx, b) {
		return false, nil
	}
	rows, err := m.lootLedger(ctx, b.CharID)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	full, err := m.holdMostlyFull(ctx, b)
	if err != nil {
		return false, err
	}
	if !full {
		return false, nil // a stack or two: keep hunting, sell it with the rest
	}
	return true, m.combatStartSale(ctx, b, "carrying a haul from the last sweep")
}

// combatSellStep disposes of the haul at the station the pilot is standing
// in, stack by stack, and goes back to work.
func (m *Mod) combatSellStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if b.StationID == 0 {
		if st.Data.TargetStationID == 0 {
			return false, m.combatStartSale(ctx, b, "nothing left to hunt with a full hold")
		}
		return false, m.beginTravel(ctx, b, st.Data.TargetSystemID, st.Data.TargetStationID,
			string(TaskCombatSell))
	}

	if _, _, _, _, err := m.sellLootLedgerHere(ctx, b, "combat"); err != nil {
		return false, err
	}

	m.clearLootLedger(ctx, b.CharID)
	// The haul is gone; the next one gets its own keep-or-donate call.
	st.Data.LootDecidedAt, st.Data.LootDonateOffice, st.Data.LootDonateStation = 0, 0, 0
	st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return false, saveState(ctx, m.db, st)
}

// sellLootLedgerHere disposes of this pilot's loot ledger at the station it is standing in, stack by
// stack, and reports what happened. combatSellStep and the home loop's meta run (metasell.go) share it.
// It carries the top-up, value floor, order-slot floor, trade-book registration and broker fee.
// The caller clears the ledger; this only sells.
func (m *Mod) sellLootLedgerHere(ctx context.Context, b *botRun, logChan string) (listed, sold, held int, gross float64, err error) {
	ledger, err := m.lootLedger(ctx, b.CharID)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if len(ledger) == 0 {
		return 0, 0, 0, 0, nil
	}

	// What the roster could use, for the whole haul in one query. It picks which stack gets an order slot.
	ids := make([]int64, 0, len(ledger))
	for _, r := range ledger {
		ids = append(ids, r.TypeID)
	}
	capable := m.capabilityCounts(ctx, ids)

	for _, r := range ledger {
		itemID, units, err := m.landLootInHangar(ctx, b, r.TypeID, r.Units)
		if err != nil {
			rlogf("rolebots: land loot char=%d type=%d: %v", b.CharID, r.TypeID, err)
			continue
		}
		if units <= 0 {
			continue // spent, reloaded or otherwise gone since it was picked up
		}
		// A looted implant that gains the pilot something goes into its
		// head instead of onto the market (implantupkeep.go).
		if m.isImplantType(ctx, r.TypeID) && m.implantKeepOrSell(ctx, b, r.TypeID, itemID) {
			continue
		}
		ask := m.lootAskPrice(ctx, b, r.TypeID)
		ask, _ = m.anchoredAsk(ctx, b.Tune, b.StationID, r.TypeID, ask)

		// A standing order of this type on this dock absorbs the stack at any size, as it does for minerals.
		// The value floor below gates only the first order.
		if topped, terr := m.topUpOwnSellOrder(ctx, b, r.TypeID, itemID, units); terr != nil {
			rlogf("rolebots: top up loot order %s: %v", r.TypeName, terr)
		} else if topped {
			listed++
			continue
		}

		// A stack worth an order slot's value gets one; capability only colours the note. A stack under the
		// floor that the demand ledger is chasing is worth a slot at any price (typeIsWanted, strays.go).
		short := m.typeIsWanted(ctx, r.TypeID)
		wanted := ask*float64(units) >= lootListMinValue || short
		// Not where the region has bought none of it in a fortnight
		// (mktdaily.go): a hub drop-off is a hub because it buys.
		if wanted && !short && m.regionBoughtNone(ctx, b.StationID, r.TypeID) {
			wanted = false
		}
		if wanted && m.freeOrderSlots(ctx, b.CharID) > 0 {
			out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
				r.TypeID, itemID, ask, units, -1 /*station range*/, 1, lootListDays, false)
			if err == nil && out.ErrorMsg == "" {
				for _, f := range out.Fills {
					gross += f.Price * float64(f.Quantity)
				}
				if resting := units - out.FilledQty; resting > 0 && out.RestingOrderID != 0 {
					m.registerOrder(ctx, b.CharID, out.RestingOrderID, r.TypeID, b.StationID,
						b.SystemID, r.TypeName, false, ask, 0, resting, tradeDealStock)
					m.bookTrade(ctx, tradeEntry{
						CharID: b.CharID, Kind: tradeKindList, TypeID: r.TypeID,
						TypeName: r.TypeName, Qty: resting, Price: ask,
						Station: b.StationID, System: b.SystemID, OrderID: out.RestingOrderID,
						Note: fmt.Sprintf("salvage listed at %.0f%% of the going rate — %d pilot(s) could use it",
							b.Tune.LootMarkup*100, capable[r.TypeID]),
					})
					m.bookBrokerFee(ctx, b, out.RestingOrderID, r.TypeID, r.TypeName, ask, resting)
					listed++
				}
				if out.FilledQty > 0 {
					m.notePeerFills(ctx, tradeEntry{
						CharID: b.CharID, TypeID: r.TypeID, TypeName: r.TypeName,
						Station: b.StationID, System: b.SystemID,
					}, out.Fills, false, "loot")
					sold++
				}
				continue
			}
			if err != nil {
				rlogf("rolebots: list loot %s: %v", r.TypeName, err)
			}
			// Listing refused: fall through and take whatever is bid.
		}

		took, value, err := m.dumpToBestBid(ctx, b, r.TypeID, itemID, r.TypeName, units,
			bidDump{Note: "salvage sold into the local bid", Tag: "loot"})
		if err != nil {
			rlogf("rolebots: dump loot %s: %v", r.TypeName, err)
		}
		if took > 0 {
			gross += value
			sold++
			continue
		}
		// Nobody bids and nobody could use it: it stays in the hangar as zero-cost stock for the trade planner.
		m.addStock(ctx, b.CharID, b.StationID, b.SystemID, r.TypeID, r.TypeName, units, 0)
		held++
	}

	switch {
	case listed > 0 || sold > 0:
		m.logBot(b.CharID, logInfo, logChan,
			"sold %d stack(s) of salvage for %.2f ISK, listed %d at its own price, "+
				"%d left in the hangar", sold, gross, listed, held)
	case held > 0:
		m.logBot(b.CharID, logInfo, logChan,
			"nobody here bids on any of it and nobody could use it — %d stack(s) in the hangar", held)
	}
	return listed, sold, held, gross, nil
}

// landLootInHangar moves one looted type out of the hold into the station hangar and reports the item
// and how many units are really there. A sell order is placed against a hangar item, never a cargo
// one. The ledger caps it: the hold may also hold ammunition the hunter bought.
func (m *Mod) landLootInHangar(ctx context.Context, b *botRun, typeID, want int64) (int64, int64, error) {
	if b.ShipID != 0 && want > 0 {
		if err := m.moveLootUnitsToHangar(ctx, b, typeID, want); err != nil {
			return 0, 0, err
		}
	}
	// One stack is enough to name: consumeItems walks every hangar stack of the type and only prefers
	// the itemID it is given.
	var itemID int64
	err := m.db.QueryRowContext(ctx, `
		SELECT itemID FROM entity
		WHERE ownerID = ? AND locationID = ? AND flag = 4 AND typeID = ? AND singleton = 0
		ORDER BY quantity DESC LIMIT 1`,
		b.CharID, b.StationID, typeID).Scan(&itemID)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("hangar stack: %w", err)
	}
	var units int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(quantity), 0) FROM entity
		WHERE ownerID = ? AND locationID = ? AND flag = 4 AND typeID = ? AND singleton = 0`,
		b.CharID, b.StationID, typeID).Scan(&units); err != nil {
		return 0, 0, fmt.Errorf("hangar units: %w", err)
	}
	if units > want {
		units = want
	}
	return itemID, units, nil
}

// moveLootUnitsToHangar moves up to want units of one type out of the hold, splitting a stack when it
// must. The cap matters: the hold can hold both bought ammunition and the identical looted ammunition.
func (m *Mod) moveLootUnitsToHangar(ctx context.Context, b *botRun, typeID, want int64) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE locationID = ? AND ownerID = ? AND flag = 5 AND typeID = ?
		ORDER BY quantity DESC`, b.ShipID, b.CharID, typeID)
	if err != nil {
		return fmt.Errorf("loot stacks in hold: %w", err)
	}
	type stack struct{ itemID, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.itemID, &s.qty); err != nil {
			rows.Close()
			return err
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	moved := int64(0)
	for _, s := range stacks {
		if moved >= want {
			break
		}
		left := want - moved
		if s.qty <= left {
			if _, err := m.db.ExecContext(ctx,
				`UPDATE entity SET locationID = ?, flag = 4 WHERE itemID = ?`,
				b.StationID, s.itemID); err != nil {
				return fmt.Errorf("land loot stack: %w", err)
			}
			moved += s.qty
			continue
		}
		// Part of a stack: decrement the hold's and land the difference as
		// a hangar stack of its own.
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`,
			left, s.itemID); err != nil {
			return fmt.Errorf("split loot stack: %w", err)
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO entity
				(itemName, typeID, ownerID, locationID, flag, contraband, singleton,
				 quantity, x, y, z, customInfo)
			SELECT typeName, typeID, ?, ?, 4, 0, 0, ?, 0, 0, 0, NULL
			FROM invTypes WHERE typeID = ?`,
			b.CharID, b.StationID, left, typeID); err != nil {
			return fmt.Errorf("land split loot: %w", err)
		}
		moved += left
	}
	// Whatever landed becomes one row per type, which makes the caller's one-stack naming true (stacking.go).
	m.stackHangar(ctx, b.CharID, b.StationID)
	return nil
}

// bidDump is what kind of sale a dumpToBestBid call is: fencing salvage, tidying a hangar stray or
// clearing a refiner's minerals into a factory's standing bid. Only one of them avoids the NPC seeder.
type bidDump struct {
	// Note is the trade-book line.
	Note string
	// Tag is the peer-fill channel (notePeerFills), which tells the feed a fenced wreck from a mineral delivery.
	Tag string
	// PlayerOnly skips bids owned by the NPC seeder. It is for minerals only: SeedOreDemand posts a seeder
	// bid at every refinery, and an unfiltered dump would sell all refined output to the NPC. Ore is not
	// filtered because the NPC bid is the fallback for a miner's rock. See docs/comment-notes-b01.md#player-only-bids.
	PlayerOnly bool
}

// dumpToBestBid sells a stack into the best standing bid on this dock.
// Reports units sold and what they fetched; (0, 0, nil) when nobody bids.
func (m *Mod) dumpToBestBid(ctx context.Context, b *botRun, typeID, itemID int64,
	typeName string, units int64, d bidDump) (int64, float64, error) {

	q := `
		SELECT MAX(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1
		  AND volRemaining > 0 AND charID <> ?`
	args := []any{b.StationID, typeID, b.CharID}
	if d.PlayerOnly {
		q += ` AND charID NOT IN (?, ?)`
		args = append(args, evedb.SeederCharID, evedb.AdminSeederCharID)
	}
	var bid sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, q, args...).Scan(&bid); err != nil {
		return 0, 0, fmt.Errorf("bid lookup: %w", err)
	}
	if !bid.Valid || bid.Float64 <= 0 {
		return 0, 0, nil
	}
	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID, typeID, itemID,
		bid.Float64, units, -1 /*station range*/, 1, 0 /*immediate*/, false)
	if err != nil {
		return 0, 0, err
	}
	if out.ErrorMsg != "" || out.FilledQty == 0 {
		return 0, 0, nil
	}
	value := 0.0
	for _, f := range out.Fills {
		value += f.Price * float64(f.Quantity)
	}
	e := tradeEntry{
		CharID: b.CharID, Kind: tradeKindSell, TypeID: typeID, TypeName: typeName,
		Qty: out.FilledQty, Price: value / float64(out.FilledQty), Value: value,
		// The goods came out of a wreck or a rock, so the profit is the whole sale less sales tax (tradefees.go).
		Profit:  m.netSaleAt(ctx, b.CharID, b.StationID, value),
		Station: b.StationID, System: b.SystemID,
		Note: d.Note,
	}
	m.bookTrade(ctx, e)
	m.notePeerFills(ctx, e, out.Fills, false, d.Tag)
	return out.FilledQty, value, nil
}

// lootAskPrice is what this pilot asks for a piece of salvage: the going rate (estimatePrice: the
// cheapest live ask anywhere, else base price), graded by lootgrade.go only when the rate fell through
// to the catalogue, times its envy (Tuning.LootMarkup). A live ask is already priced for that variant.
func (m *Mod) lootAskPrice(ctx context.Context, b *botRun, typeID int64) float64 {
	ref, fromMarket, err := m.estimatePriceRefFor(ctx, b, typeID)
	if err != nil || ref <= 0 {
		return 0
	}
	if !fromMarket {
		ref *= metaGradeMultiplier(m.typeGrade(ctx, typeID))
	}
	mk := b.Tune.LootMarkup
	if mk <= 0 {
		mk = lootMarkupDefault
	}
	return ref * mk
}

// detailBounty is the panel's line for a hunting pilot.
func detailBounty(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	switch TaskKind(st.Phase) {
	case TaskCombatLoot:
		return fmt.Sprintf("emptying a can in %s", m.nameOfSystem(ctx, d.HuntLootSysID))
	case TaskCombatSell:
		return "selling the haul"
	case TaskCombatDonate:
		return "giving the haul to its corporation"
	case TaskCombatSweep:
		left := (huntSweepSec - (clock.Unix() - d.HuntPickedAt)) / 60
		if left < 0 {
			left = 0
		}
		return fmt.Sprintf("combing %s, %d min left", m.nameOfSystem(ctx, d.HuntSystemID), left)
	}
	return ""
}
