package rolebots

// Hangar strays: goods no ledger owns. The module deposits goods in hangars with no ledger entry:
// a module unfitted to make room (clearSlot), the old fit's leftovers stripped at an outfit stop
// (stripOffFitModules), a cancelled order's goods. This sweep disposes of them: fold into a standing
// order of the pilot's own, list what is worth an order slot, dump the rest into the local bid, and
// hand what nobody wants to the stock ledger.
// It covers categories 7 and 8 only (modules and charges). Ore, minerals, loot in transit and industry
// materials have ledgers and sweeps of their own; touching them would sell a stockpile out from under
// its owner (see industrySellSurplus's scope note). Ore's sweep is orestrays.go.

import (
	"context"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// How often one pilot bothers to tidy its hangar. Strays accumulate on the
// scale of refits, not minutes.
const hangarStraySweepSec = 3600

var (
	hangarStrayMu sync.Mutex
	hangarStrayAt = map[int64]int64{} // charID to unix of last sweep
)

// wantedTypesTTL is how long the wanted-type set (typeIsWanted) is cached. A stack other bots queue
// for (rolebot_market_demand), such as cheap ammunition, can never clear lootListMinValue, so the
// order-slot floor also asks whether a type is wanted instead of lowering the floor for everything.
const wantedTypesTTL = 5 * time.Minute

var (
	wantedMu    sync.Mutex
	wantedSet   map[int64]bool
	wantedSetAt time.Time
)

// typeIsWanted reports whether any pilot has failed to buy this type recently
// enough to still be looking. Cached process-wide: the ledger moves on the
// scale of failed shopping trips, and this is asked once per stray stack per
// pilot across ten thousand of them.
func (m *Mod) typeIsWanted(ctx context.Context, typeID int64) bool {
	wantedMu.Lock()
	if wantedSet != nil && clock.Now().Sub(wantedSetAt) < wantedTypesTTL {
		want := wantedSet[typeID]
		wantedMu.Unlock()
		return want
	}
	wantedMu.Unlock()

	rows, err := m.db.QueryContext(ctx,
		`SELECT typeID FROM rolebot_market_demand WHERE `+demandWantedSQL("", clock.Unix())+` > 0`)
	if err != nil {
		rlogf("rolebots: wanted types: %v", err)
		return false // an unreadable ledger is not a reason to list dust
	}
	set := map[int64]bool{}
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err == nil {
			set[t] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		rlogf("rolebots: wanted types: %v", err)
		return false
	}
	wantedMu.Lock()
	wantedSet, wantedSetAt = set, clock.Now()
	wantedMu.Unlock()
	return set[typeID]
}

// sellHangarStrays disposes of unledgered module/charge stacks in the
// hangar the pilot is docked in. Cheap no-op for a clean hangar; throttled
// per pilot because the common case is nothing to do.
func (m *Mod) sellHangarStrays(ctx context.Context, b *botRun) {
	if b.StationID == 0 {
		return
	}
	now := clock.Unix()
	hangarStrayMu.Lock()
	if now-hangarStrayAt[b.CharID] < hangarStraySweepSec {
		hangarStrayMu.Unlock()
		return
	}
	hangarStrayAt[b.CharID] = now
	hangarStrayMu.Unlock()

	// Dead stock (deadstock.go): what nobody buys is reprocessed, a capped number of stacks a visit.
	// Never at a larder: a factory's hangar is a bill of materials.
	cfg := m.deadStockSettings(ctx)
	dead := cfg.Enabled && cfg.MaxStacks > 0 && !warehouseLarders(b)[b.StationID]
	var (
		bg      *deadStockBudget
		reserve *gearReserve
		got     = map[int64]int64{}
		tally   deadStockOutcome
	)
	if dead {
		bg = &deadStockBudget{left: cfg.MaxStacks}
		reserve = m.pilotGearReserve(ctx, b, cfg)
		m.trimSurplusHulls(ctx, b, cfg, bg, got, &tally)
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, t.typeName, e.quantity,
		       COALESCE(t.basePrice, 0), t.groupID, g.categoryID
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID IN (7, 8)
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND e.singleton = 0
		  AND NOT EXISTS (SELECT 1 FROM rolebot_loot_hold lh
			WHERE lh.characterID = e.ownerID AND lh.typeID = e.typeID AND lh.units > 0)
		  AND NOT EXISTS (SELECT 1 FROM rolebot_trade_stock ts
			WHERE ts.characterID = e.ownerID AND ts.stationID = ? AND ts.typeID = e.typeID)`,
		b.CharID, b.StationID, b.StationID)
	if err != nil {
		rlogf("rolebots: hangar strays char=%d: %v", b.CharID, err)
		return
	}
	type stray struct {
		itemID, typeID, qty, groupID, catID int64
		name                                string
		base                                float64
	}
	var strays []stray
	for rows.Next() {
		var s stray
		if err := rows.Scan(&s.itemID, &s.typeID, &s.name, &s.qty, &s.base, &s.groupID, &s.catID); err != nil {
			rows.Close()
			rlogf("rolebots: hangar strays scan char=%d: %v", b.CharID, err)
			return
		}
		strays = append(strays, s)
	}
	rows.Close()

	listed, sold, held := 0, 0, 0
	for _, s := range strays {
		// A standing order of the pilot's own absorbs the stray whole.
		if topped, terr := m.topUpOwnSellOrder(ctx, b, s.typeID, s.itemID, s.qty); terr == nil && topped {
			listed++
			continue
		}
		ask := m.strayAsk(ctx, b, s.typeID, s.base)
		// A stray has no cost basis, so one worth more as metal than listed
		// goes to the refinery rather than onto the market.
		if dead && bg.ok() && ask > 0 && ask < m.scrapValue(ctx, s.typeID) {
			f := m.deadStockFactsFor(ctx, b, cfg, dsGear, s.typeID, s.groupID, s.catID, s.qty,
				reserve.of(s.typeID, s.catID, s.groupID), ask)
			f.NoCost = true
			if p := deadStockVerdict(f); p.Act == dsReprocess {
				if n := m.applyDeadStock(ctx, b, bg, p, s.typeID, s.itemID, s.name, got, &tally); n >= s.qty {
					continue
				} else {
					s.qty -= n
				}
			}
		}
		// Worth a slot on its value, or because somebody is short of it (typeIsWanted).
		wanted := m.typeIsWanted(ctx, s.typeID)
		worthASlot := ask*float64(s.qty) >= lootListMinValue || wanted
		// Not where the region bought none of it in a fortnight (mktdaily.go), unless the roster itself is
		// short of it.
		if worthASlot && !wanted && m.regionBoughtNone(ctx, b.StationID, s.typeID) {
			worthASlot = false
		}
		if ask > 0 && worthASlot && m.freeOrderSlots(ctx, b.CharID) > 0 {
			out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
				s.typeID, s.itemID, ask, s.qty, -1 /*station range*/, 1, lootListDays, false)
			if err == nil && out.ErrorMsg == "" {
				if resting := s.qty - out.FilledQty; resting > 0 && out.RestingOrderID != 0 {
					m.registerOrder(ctx, b.CharID, out.RestingOrderID, s.typeID, b.StationID,
						b.SystemID, s.name, false, ask, 0, resting, tradeDealStock)
					m.bookTrade(ctx, tradeEntry{
						CharID: b.CharID, Kind: tradeKindList, TypeID: s.typeID,
						TypeName: s.name, Qty: resting, Price: ask,
						Station: b.StationID, System: b.SystemID, OrderID: out.RestingOrderID,
						Note: "hangar stray — an unfitted module put back on the market",
					})
					m.bookBrokerFee(ctx, b, out.RestingOrderID, s.typeID, s.name, ask, resting)
				}
				listed++
				continue
			}
			// Refused (fee, cap): fall through to the bid.
		}
		remaining := s.qty
		if took, _, derr := m.dumpToBestBid(ctx, b, s.typeID, s.itemID, s.name, s.qty,
			bidDump{Note: "hangar stray sold into the local bid", Tag: "stray"}); derr == nil && took >= s.qty {
			sold++
			continue
		} else if derr == nil && took > 0 {
			sold++
			remaining -= took
		}
		// No bid at this counter: try a bid elsewhere in reach, a courier to an ammunition buyer, or
		// reprocessing (deadstock.go). Listing was already asked above.
		if dead && bg.ok() {
			f := m.deadStockFactsFor(ctx, b, cfg, dsGear, s.typeID, s.groupID, s.catID, remaining,
				reserve.of(s.typeID, s.catID, s.groupID), 0)
			if p := deadStockVerdict(f); p.Act == dsSellBid || p.Act == dsReprocess || p.Act == dsRelocate {
				remaining -= m.applyDeadStock(ctx, b, bg, p, s.typeID, s.itemID, s.name, got, &tally)
			}
		}
		if remaining <= 0 {
			continue
		}
		// Nobody here wants it at any price: the stock ledger takes it at cost zero, and the trade planner's
		// dust valuation decides when the pile is worth a trip.
		m.addStock(ctx, b.CharID, b.StationID, b.SystemID, s.typeID, s.name, remaining, 0)
		held++
	}
	if len(strays) > 0 {
		m.logBot(b.CharID, logInfo, "market", fmt.Sprintf(
			"tidied the hangar — %d stray stack(s): %d onto the market, %d into bids, %d shelved as stock",
			len(strays), listed, sold, held))
	}
	if dead {
		m.disposeShelvedStock(ctx, b, cfg, bg, reserve, got, &tally)
		m.sellHangarCommodities(ctx, b, cfg, bg, &tally)
		m.relocateDeadAmmo(ctx, b, &tally)
		m.listDeadStockMinerals(ctx, b, got)
		m.logDeadStock(b, tally)
	}
}

// strayAsk is what a stray or shelved stack lists at: priced like loot (the going rate times the
// pilot's envy) under the spec planner's spiral caps, and never under what the NPCs in this region ask
// (npcanchor.go). A stray priced off a drifted book or the 100 ISK stub must not reseed the drift.
func (m *Mod) strayAsk(ctx context.Context, b *botRun, typeID int64, base float64) float64 {
	ask := m.lootAskPrice(ctx, b, typeID)
	if base > 0 && ask > base*specSpiralBaseMul {
		ask = base * specSpiralBaseMul
	} else if base <= 0 && ask > specUnanchoredMaxUnit {
		ask = specUnanchoredMaxUnit
	}
	ask, _ = m.anchoredAsk(ctx, b.Tune, b.StationID, typeID, ask)
	return ask
}

func init() {
	registerCharForget("hangarStrayAt", func(id int64) {
		hangarStrayMu.Lock()
		delete(hangarStrayAt, id)
		hangarStrayMu.Unlock()
	})
}
