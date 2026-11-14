package rolebots

// Liquidation: what a destitute pilot does while standing on goods it can sell.
// broke.go sends a pilot that cannot pay for wanted work to earn, and distress.go
// prices a listing to clear when the broker fee cannot be paid. Neither covers a
// pilot that is asset rich and cash dead, because each disposal path belongs to
// the activity that made the goods. This sweep ignores what the goods are.
//
// Three rules keep it from being a fire sale. It runs only below the cash floor.
// It stops once the pilot can work again. It sells through distressSell
// (distress.go) at the standing bid and its depth, because the core charges the
// broker fee only on the part of an order that rests.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

const (
	// liquidateSweepSec throttles one pilot's attempts: the hangar and the order
	// book do not change between passes.
	liquidateSweepSec = 900
	// liquidateTargetMul is how far above the floor the sale aims: two floors,
	// since landing on the floor leaves a pilot solvent by a rounding error.
	liquidateTargetMul = 2.0
	// liquidateMaxStacks bounds one pass: every stack costs a bid lookup and a
	// market call.
	liquidateMaxStacks = 12
)

var (
	liquidateMu sync.Mutex
	liquidateAt = map[int64]int64{} // charID to unix of last sweep
)

// liquidateForCash turns hangar goods into ISK for a pilot that has none. A
// cheap no-op above the floor, throttled per pilot. Called from the between-jobs
// planner and the home-loop shopping stop, where the pilot is docked with
// nothing committed.
func (m *Mod) liquidateForCash(ctx context.Context, b *botRun) {
	if b.StationID == 0 {
		return
	}
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return
	}
	// A miner's line is its own cash floor: below MiningCashFloor its ore already
	// goes to a bid (mining.go). Everyone else waits until it is skint.
	trigger, target := liquidateLine(minerPilot(b.State), bal, b.Tune.MiningCashFloor)
	if !trigger {
		return // solvent enough to work: its stockpile is a plan, not a problem
	}
	// A miner that is short but not skint sells only what it digs. Ammunition,
	// modules and corporate shares are its kit and capital.
	oreOnly := spendable(bal) > 0
	now := clock.Unix()
	liquidateMu.Lock()
	if now-liquidateAt[b.CharID] < liquidateSweepSec {
		liquidateMu.Unlock()
		return
	}
	liquidateAt[b.CharID] = now
	liquidateMu.Unlock()

	// Corporation shares go last: they are capital the corporation buys back for
	// cash at once (corpshares.go).
	if !oreOnly {
		defer m.liquidateShares(ctx, b, target)
	}

	// Stackable goods only (singleton = 0), dearest first. Blueprints and skillbooks
	// (categories 9 and 16) are excluded: they are permanent capital.
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.quantity
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4
		  AND e.singleton = 0 AND e.quantity > 0
		  AND g.categoryID NOT IN (9, 16)
		  AND (? = 0 OR g.categoryID = ? OR t.groupID = ?)
		ORDER BY e.quantity * COALESCE(t.basePrice, 0) DESC
		LIMIT ?`, b.CharID, b.StationID, oreOnly, catAsteroid, groupMineral, liquidateMaxStacks)
	if err != nil {
		rlogf("rolebots: liquidate scan char=%d: %v", b.CharID, err)
		return
	}
	type stack struct{ itemID, typeID, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.itemID, &s.typeID, &s.qty); err != nil {
			rows.Close()
			rlogf("rolebots: liquidate scan char=%d: %v", b.CharID, err)
			return
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		rlogf("rolebots: liquidate char=%d: %v", b.CharID, err)
		return
	}
	if len(stacks) == 0 {
		return
	}

	var raised float64
	var sold int
	for _, s := range stacks {
		if bal+raised >= target {
			break // it can work again; the rest is somebody else's sweep
		}
		// (0, 0) is the ordinary answer: nobody here bids on it. distressSell logs the
		// sales it does make.
		if units, gross := m.distressSell(ctx, b, b.StationID, s.typeID, s.itemID, s.qty); units > 0 {
			raised += gross
			sold++
		}
	}
	if sold == 0 {
		// The end of the road for this pilot at this station, so the board says so.
		m.logBot(b.CharID, logInfo, "market",
			"down to %s with %d stack(s) in the hangar and no bid here for any of it — nothing to sell its way out with",
			iskString(bal), len(stacks))
		return
	}
	m.logBot(b.CharID, logInfo, "market",
		"was down to %s — sold %d stack(s) out of the hangar for %s to get back to work",
		iskString(bal), sold, iskString(raised))
}

func init() {
	registerCharForget("liquidateAt", func(id int64) {
		liquidateMu.Lock()
		delete(liquidateAt, id)
		liquidateMu.Unlock()
	})
}

// liquidateLine is when the sweep runs and what it aims for. A miner runs under
// its own cash floor and aims for two of them; anyone else runs only when skint
// (spendable 0) and aims for two wallet floors. Pure.
func liquidateLine(miner bool, bal, miningFloor float64) (trigger bool, target float64) {
	if miner && miningFloor > 0 {
		return cashInjectionDue(bal, miningFloor), cashTarget(miningFloor)
	}
	return spendable(bal) <= 0, walletFloorISK * liquidateTargetMul
}

// ── the cash injection ──────────────────────────────────────────────────────
//
// A miner under its cash floor holding ore or minerals is unsold, not poor: the
// standing bids it can reach are the cash. The disposal ladder runs off a full
// hold, so ore already on a hangar floor needs this trip to a buyer.
// cashInjectionPlan finds the nearest station where a bid takes what the miner
// holds (own dock, then a hangar, then wherever its hold can go) within
// oreSellMaxJumps, flies there and sells through distressSell at the bid, only
// enough to put the wallet back on two floors. Seeder bids count here; no NPC
// bids for a mineral (mod-rebuild.ps1 step 1g patches that clause out).

const (
	// TaskCashSell is the trip to a bid that will turn a broke miner's goods
	// into a working wallet.
	TaskCashSell TaskKind = "home.cashsell"

	// cashInjectSweepSec throttles one pilot's look: a few grouped reads and a market
	// scan, on bids that move on the dynamics clock.
	cashInjectSweepSec = 900
	// cashTripMinRaise is the least a trip must raise to be worth the flying. A
	// sale where the pilot already stands has no floor.
	cashTripMinRaise = 50_000.0
	// cashTripStations bounds how many remote hangars get priced.
	cashTripStations = 8
	// cashSellMaxStacks bounds one sale's walk of a hangar.
	cashSellMaxStacks = 12
	// cashSellBidsPerStack is how many bids one stack may be sold through:
	// distressSell takes one bid's depth per call.
	cashSellBidsPerStack = 3
)

var (
	cashInjectMu sync.Mutex
	cashInjectAt = map[int64]int64{} // charID to unix of last look
)

// minerPilot reports whether this pilot is a miner for the cash rules: what
// it is doing, or what it last did, is digging rock or ice.
func minerPilot(st *SimState) bool {
	if st == nil {
		return false
	}
	for _, a := range []string{st.Activity, st.Data.LastActivity} {
		if a == ActMining || a == ActIce {
			return true
		}
	}
	return false
}

// cashInjectionDue is the trigger: a wallet under the cash floor. Pure.
func cashInjectionDue(bal, floor float64) bool { return floor > 0 && bal < floor }

// cashTarget is where a cash sale stops: two floors (liquidateTargetMul). Pure.
func cashTarget(floor float64) float64 { return floor * liquidateTargetMul }

// cashShort is how much ISK the sale has to raise. Pure.
func cashShort(bal, floor float64) float64 {
	if need := cashTarget(floor) - bal; need > 0 {
		return need
	}
	return 0
}

// cashSellUnits is how many units to put into a bid at price to raise need:
// enough, rounded up, never more than is held or than the bid takes (depth 0
// reads as unbounded). Pure.
func cashSellUnits(need, price float64, have, depth int64) int64 {
	if need <= 0 || price <= 0 || have <= 0 {
		return 0
	}
	u := int64(math.Ceil(need / price))
	if u > have {
		u = have
	}
	if depth > 0 && u > depth {
		u = depth
	}
	return u
}

// cashStop is a station the pilot could sell at, priced.
type cashStop struct {
	StationID, SystemID int64
	Jumps               int
	Raise               float64 // what the bids reaching it would pay for what it can sell there
}

// pickCashStop is the nearest stop worth the flying: raise at least
// cashTripMinRaise, or the whole shortfall when that is less. Ties go to the
// bigger raise. Pure.
func pickCashStop(stops []cashStop, need float64) *cashStop {
	bar := cashTripMinRaise
	if need < bar {
		bar = need
	}
	var best *cashStop
	for i := range stops {
		s := &stops[i]
		if s.Raise <= 0 || s.Raise < bar {
			continue
		}
		if best == nil || s.Jumps < best.Jumps || (s.Jumps == best.Jumps && s.Raise > best.Raise) {
			best = s
		}
	}
	return best
}

// cashGoodsAt is ore and minerals this pilot owns in one station hangar.
type cashGoodsAt struct {
	StationID, SystemID int64
	Units               map[int64]int64 // typeID to units
}

// cashHangarGoods lists every station hangar holding this pilot's ore or
// minerals, excluding larders (warehouseLarders): a factory's bill of materials
// is not cash.
func (m *Mod) cashHangarGoods(ctx context.Context, b *botRun) (map[int64]*cashGoodsAt, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.locationID, s.solarSystemID, e.typeID, SUM(e.quantity)
		FROM entity e
		JOIN staStations s ON s.stationID = e.locationID
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.flag = 4 AND e.singleton = 0 AND e.quantity > 0
		  AND (g.categoryID = ? OR t.groupID = ?)
		GROUP BY e.locationID, s.solarSystemID, e.typeID
		LIMIT 500`, b.CharID, catAsteroid, groupMineral)
	if err != nil {
		return nil, fmt.Errorf("cash goods: %w", err)
	}
	defer rows.Close()
	larders := warehouseLarders(b)
	out := map[int64]*cashGoodsAt{}
	for rows.Next() {
		var stationID, sysID, typeID, units int64
		if err := rows.Scan(&stationID, &sysID, &typeID, &units); err != nil {
			return nil, err
		}
		if larders[stationID] || units <= 0 {
			continue
		}
		g := out[stationID]
		if g == nil {
			g = &cashGoodsAt{StationID: stationID, SystemID: sysID, Units: map[int64]int64{}}
			out[stationID] = g
		}
		g.Units[typeID] += units
	}
	return out, rows.Err()
}

// cashHoldGoods is the ore and minerals in the ship's hold.
func (m *Mod) cashHoldGoods(ctx context.Context, b *botRun) (map[int64]int64, error) {
	out := map[int64]int64{}
	if b.ShipID == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.ownerID = ? AND e.flag = 5 AND e.quantity > 0
		  AND (g.categoryID = ? OR t.groupID = ?)
		GROUP BY e.typeID`, b.ShipID, b.CharID, catAsteroid, groupMineral)
	if err != nil {
		return nil, fmt.Errorf("cash hold: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, units int64
		if err := rows.Scan(&typeID, &units); err != nil {
			return nil, err
		}
		out[typeID] += units
	}
	return out, rows.Err()
}

// cashRaiseAt is what the bids reaching stationID would pay for these units.
func (m *Mod) cashRaiseAt(ctx context.Context, b *botRun, stationID int64, units map[int64]int64) float64 {
	raise := 0.0
	for typeID, n := range units {
		bids, err := m.bidsReachingStation(ctx, stationID, typeID, b.CharID)
		if err != nil {
			continue
		}
		raise += bidsRaise(bids, n)
	}
	return raise
}

// mergeUnits adds every entry of src into dst. A nil dst is allocated.
func mergeUnits(dst, src map[int64]int64) map[int64]int64 {
	if dst == nil {
		dst = map[int64]int64{}
	}
	for t, n := range src {
		dst[t] += n
	}
	return dst
}

// cashInjectionPlan sends a broke pilot holding ore or minerals to the nearest
// bid that takes them, or sells at once when its own dock is that bid. Reports
// whether it took the pass. includeHold counts the ship's hold as goods; the
// mining plan asks only with an empty hold, since a full hold already heads to a bid.
func (m *Mod) cashInjectionPlan(ctx context.Context, b *botRun, includeHold bool) (bool, error) {
	if b == nil || b.State == nil {
		return false, nil
	}
	floor := b.Tune.MiningCashFloor
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil || !cashInjectionDue(bal, floor) {
		return false, nil
	}
	now := clock.Unix()
	cashInjectMu.Lock()
	if now-cashInjectAt[b.CharID] < cashInjectSweepSec {
		cashInjectMu.Unlock()
		return false, nil
	}
	cashInjectAt[b.CharID] = now
	cashInjectMu.Unlock()

	hangars, err := m.cashHangarGoods(ctx, b)
	if err != nil {
		return false, err
	}
	hold := map[int64]int64{}
	if includeHold {
		if hold, err = m.cashHoldGoods(ctx, b); err != nil {
			return false, err
		}
	}
	if len(hangars) == 0 && len(hold) == 0 {
		return false, nil
	}

	// Its own dock first: no flight, no floor on the raise. The hold is unloaded
	// onto the hangar floor so the sale can see it.
	if b.StationID != 0 && !warehouseLarders(b)[b.StationID] {
		var here map[int64]int64
		if g := hangars[b.StationID]; g != nil {
			here = mergeUnits(here, g.Units)
		}
		here = mergeUnits(here, hold)
		if len(here) > 0 && m.cashRaiseAt(ctx, b, b.StationID, here) > 0 {
			if len(hold) > 0 {
				m.unloadOreCargo(ctx, b)
				m.unloadMinerals(ctx, b)
				// On this hangar floor now: what is left can no longer ride a trip elsewhere.
				hold = map[int64]int64{}
			}
			raised, sold := m.cashSellHere(ctx, b, bal, floor)
			if sold > 0 {
				bal += raised
				// Nothing more reaching this dock will take what is left here.
				delete(hangars, b.StationID)
				m.logBot(b.CharID, logInfo, "market",
					"down to %s, under its %s line — sold %d lot(s) of ore and minerals into the bids reaching this dock for %s",
					iskString(bal-raised), iskString(floor), sold, iskString(raised))
				if cashShort(bal, floor) <= 0 {
					st := b.State
					st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
					st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
					return true, saveState(ctx, m.db, st)
				}
			}
		}
	}
	need := cashShort(bal, floor)
	if need <= 0 {
		return false, nil
	}

	// Somewhere else: a hangar it left rock in, or a buyer its hold can reach.
	// Distance follows the pilot's travel rules and oreSellMaxJumps.
	reach, err := m.systemsWithin(ctx, b.SystemID, rulesForBot(b), oreSellMaxJumps)
	if err != nil {
		return false, err
	}
	byStation := map[int64]*cashStop{}
	var remote []*cashGoodsAt
	for _, g := range hangars {
		if g.StationID == b.StationID {
			continue // priced above; nothing reaching this dock takes it
		}
		if _, ok := reach[g.SystemID]; ok {
			remote = append(remote, g)
		}
	}
	sort.Slice(remote, func(i, j int) bool {
		ji, jj := reach[remote[i].SystemID], reach[remote[j].SystemID]
		if ji != jj {
			return ji < jj
		}
		return remote[i].StationID < remote[j].StationID
	})
	if len(remote) > cashTripStations {
		remote = remote[:cashTripStations]
	}
	for _, g := range remote {
		// The hold rides along to any stop, so it is offered at each.
		units := mergeUnits(mergeUnits(nil, g.Units), hold)
		if r := m.cashRaiseAt(ctx, b, g.StationID, units); r > 0 {
			byStation[g.StationID] = &cashStop{StationID: g.StationID, SystemID: g.SystemID,
				Jumps: reach[g.SystemID], Raise: r}
		}
	}
	if len(hold) > 0 {
		types := make([]int64, 0, len(hold))
		for t := range hold {
			types = append(types, t)
		}
		sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
		_, err := m.shopLadder(ctx, b, types, false, m.homeRegionSet(ctx, b), defaultBand(b), "",
			func(scan marketScan) bool {
				found := false
				for _, s := range stopsWithin(scan.Stops, oreSellMaxJumps) {
					if hangars[s.StationID] != nil && byStation[s.StationID] != nil {
						continue // a remote hangar, already priced with the hold included
					}
					n := hold[s.TypeID]
					if s.Volume < n {
						n = s.Volume
					}
					if n <= 0 || s.Price <= 0 {
						continue
					}
					cs := byStation[s.StationID]
					if cs == nil {
						cs = &cashStop{StationID: s.StationID, SystemID: s.SystemID, Jumps: s.Jumps}
						byStation[s.StationID] = cs
					}
					cs.Raise += s.Price * float64(n)
					found = true
				}
				return found
			})
		if err != nil {
			return false, err
		}
	}
	stops := make([]cashStop, 0, len(byStation))
	for _, s := range byStation {
		stops = append(stops, *s)
	}
	sort.Slice(stops, func(i, j int) bool { return stops[i].StationID < stops[j].StationID })
	best := pickCashStop(stops, need)
	if best == nil {
		return false, nil
	}
	st := b.State
	st.Activity = ""
	st.Data.TargetStationID = best.StationID
	st.Data.TargetSystemID = best.SystemID
	m.logBot(b.CharID, logInfo, "market",
		"down to %s, under its %s line — a bid reaching %s (%d jumps) will pay about %s for the ore and minerals it holds; going to sell enough to get working again",
		iskString(bal), iskString(floor), m.stationLabel(ctx, best.StationID), best.Jumps, iskString(best.Raise))
	return true, m.beginTravel(ctx, b, best.SystemID, best.StationID, string(TaskCashSell))
}

// cashSellHere sells ore and minerals out of the hangar the pilot stands in,
// at the bids reaching it, until the wallet is back on two floors. Reports the
// gross and how many sales were made.
func (m *Mod) cashSellHere(ctx context.Context, b *botRun, bal, floor float64) (float64, int) {
	if b.StationID == 0 {
		return 0, 0
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.quantity
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4
		  AND e.singleton = 0 AND e.quantity > 0
		  AND (g.categoryID = ? OR t.groupID = ?)
		ORDER BY e.quantity * COALESCE(t.basePrice, 0) DESC
		LIMIT ?`, b.CharID, b.StationID, catAsteroid, groupMineral, cashSellMaxStacks)
	if err != nil {
		rlogf("rolebots: cash sale scan char=%d: %v", b.CharID, err)
		return 0, 0
	}
	type stack struct{ itemID, typeID, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.itemID, &s.typeID, &s.qty); err != nil {
			break
		}
		stacks = append(stacks, s)
	}
	rows.Close()

	raised, sold := 0.0, 0
	for _, s := range stacks {
		left := s.qty
		for i := 0; i < cashSellBidsPerStack && left > 0; i++ {
			need := cashShort(bal+raised, floor)
			if need <= 0 {
				return raised, sold
			}
			bid, depth := m.clearingBid(ctx, b.CharID, b.StationID, s.typeID)
			units := cashSellUnits(need, bid, left, depth)
			if units <= 0 {
				break
			}
			n, gross := m.distressSell(ctx, b, b.StationID, s.typeID, s.itemID, units)
			if n <= 0 {
				break
			}
			raised += gross
			sold++
			left -= n
		}
	}
	return raised, sold
}

// cashSellStep is the far end of the cash trip: docked at the bid, the hold
// out onto the floor, and the sale.
func (m *Mod) cashSellStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if b.StationID == 0 || b.StationID != st.Data.TargetStationID {
		if st.Data.TargetStationID == 0 {
			st.Activity, st.Phase = "", string(TaskIdle)
			st.Data.LegEndsAt = clock.Unix() + idleRetrySec
			return false, saveState(ctx, m.db, st)
		}
		return false, m.beginTravel(ctx, b, st.Data.TargetSystemID, st.Data.TargetStationID,
			string(TaskCashSell))
	}
	m.unloadOreCargo(ctx, b)
	m.unloadMinerals(ctx, b)
	floor := b.Tune.MiningCashFloor
	if bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID); err == nil && cashInjectionDue(bal, floor) {
		raised, sold := m.cashSellHere(ctx, b, bal, floor)
		if sold > 0 {
			m.logBot(b.CharID, logInfo, "market",
				"was down to %s — sold %d lot(s) of ore and minerals into the standing bids here for %s",
				iskString(bal), sold, iskString(raised))
		} else {
			m.logBot(b.CharID, logInfo, "market",
				"reached %s to sell for cash, but the bids it came for are gone — the goods stay in the hangar",
				m.stationLabel(ctx, b.StationID))
		}
	}
	st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return false, saveState(ctx, m.db, st)
}

// detailCashSell is the bot board's second line for the trip.
func detailCashSell(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.TargetStationID == 0 {
		return "selling ore and minerals into a standing bid for cash"
	}
	return "selling ore and minerals for cash at " + m.stationLabel(ctx, st.Data.TargetStationID)
}

func init() {
	// Not interruptible: the hold may be carrying the goods it is going to
	// sell, and a muster mid-trip would fly them away from their buyer.
	registerTask(TaskDef{
		Kind: TaskCashSell, Label: "Selling ore and minerals for cash",
		Posture: PostureDocked, Detail: detailCashSell,
		Step: (*Mod).cashSellStep,
	})
	registerCharForget("cashInjectAt", func(id int64) {
		cashInjectMu.Lock()
		delete(cashInjectAt, id)
		cashInjectMu.Unlock()
	})
}
