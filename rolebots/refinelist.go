package rolebots

// Refine a hangar full of ore and put the minerals on the market. A mining
// gang's hauler needs the same steps as miningRefineAndList (mining.go) but
// cannot use it: that function drops the pilot to idle, holds back the pilot's
// own project reservation and opens on one ore family. Both callers share the
// list step (top-up, bid dump, batch gate, trade book, broker fee).

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// refineHangarOre reprocesses every raw ore stack in the pilot's hangar at this
// station, not only the last load, so earlier partial portions top up. unitsIn 0
// with a nil error means nothing makes a whole portion yet; the caller words it.
func (m *Mod) refineHangarOre(ctx context.Context, b *botRun) (unitsIn int64, got map[int64]int64, err error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND e.singleton = 0
		  AND g.categoryID = 25`, b.CharID, b.StationID)
	if err != nil {
		return 0, nil, err
	}
	var oreStacks []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, nil, err
		}
		oreStacks = append(oreStacks, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	got = map[int64]int64{}
	for _, id := range oreStacks {
		out, rerr := m.refineStack(ctx, b, id)
		if rerr != nil {
			return 0, nil, rerr
		}
		if out == nil {
			continue
		}
		unitsIn += out.UnitsIn
		for typeID, qty := range out.Minerals {
			got[typeID] += qty
		}
	}
	// refineStack books the ore-flow counter, not this loop: industryRefine has
	// its own hangar-refine loop (industry.go).
	return unitsIn, got, nil
}

// projectReservations is what the pilot's own running project has spoken for
// and must not be sold. Empty for a pilot with no project installed, which is
// every gang hauler and most miners.
func (m *Mod) projectReservations(ctx context.Context, b *botRun) map[int64]int64 {
	reserved := map[int64]int64{}
	d := b.State.Data
	if d.IndBPTypeID == 0 || d.IndJobID != 0 {
		return reserved
	}
	skills, serr := m.charSkillsFor(ctx, b.CharID)
	if serr != nil {
		return reserved
	}
	matMul := 1.0
	if line, lerr := m.manufactureLineAt(ctx, b, d.IndFactoryID, 0); lerr == nil {
		matMul = line.MatMul
	}
	meLevel, _ := m.blueprintLevels(ctx, d.IndBPItemID)
	needs, nerr := m.industryMaterialNeeds(ctx, d.IndBPTypeID, meLevel, d.IndRuns,
		skills[skillProductionEff], matMul)
	if nerr != nil {
		return reserved
	}
	for _, n := range needs {
		reserved[n.TypeID] = n.Needed
	}
	return reserved
}

// listRefinedMinerals puts each refined mineral on this station's market: into
// the pilot's own standing order, into a standing player bid, or as a fresh
// listing once the stack is a lot worth an order slot. maxHoldSec is how long a
// stack short of a lot may wait; callers pass mineralHoldMaxSec or mineralHoldSecUnder.
func (m *Mod) listRefinedMinerals(ctx context.Context, b *botRun,
	got, reserved map[int64]int64, maxHoldSec int64) (listed, kept, held int, err error) {

	holdDays := maxHoldSec / 86400
	for typeID := range got {
		// Price off the hangar stack, not this trip's output: the batch gate leaves
		// earlier trips' minerals in the stack to be listed together.
		var itemID, qty int64
		if m.db.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity
			WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
			ORDER BY quantity DESC LIMIT 1`,
			b.CharID, b.StationID, typeID).Scan(&itemID, &qty) != nil || qty <= 0 {
			continue
		}
		if keep := reserved[typeID]; keep > 0 {
			qty -= keep
			kept++
			if qty <= 0 {
				continue
			}
		}
		// A standing order of this mineral on this dock absorbs the refine. The
		// batch gate governs only the first listing.
		if topped, terr := m.topUpOwnSellOrder(ctx, b, typeID, itemID, qty); terr != nil {
			m.logBot(b.CharID, logError, "market", "topping up its %s order: %v — listing instead",
				m.typeNameOr(ctx, typeID), terr)
		} else if topped {
			m.clearMineralHold(ctx, b.CharID, b.StationID, typeID)
			listed++
			continue
		}
		// A standing player bid (usually a factory's, industrybids.go) takes the
		// stack before the lot floor: a fill uses no order slot. See bidDump.PlayerOnly.
		if took, _, derr := m.dumpToBestBid(ctx, b, typeID, itemID, m.typeNameOr(ctx, typeID), qty,
			bidDump{
				Note:       "refined minerals sold into a standing bid — demand met at the door",
				Tag:        "minerals",
				PlayerOnly: true,
			}); derr == nil && took > 0 {
			qty -= took
			if qty <= 0 {
				m.clearMineralHold(ctx, b.CharID, b.StationID, typeID)
				listed++
				continue
			}
		}
		price, priceWhy, perr := m.mineralAsk(ctx, b, b.StationID, typeID, qty)
		if perr != nil || price <= 0 {
			continue
		}
		// A stack short of a lot waits for the next load (mineralbatch.go) unless
		// the mineral is in famine (supplyBidFamineUnits). The litter line still applies.
		famine := m.mineralShortageUnits(ctx, typeID) >= supplyBidFamineUnits(typeID)
		ready, why := mineralBatchReady(typeID, qty, price, b.Tune.MineralBatchMul,
			m.mineralHeldSince(ctx, b.CharID, b.StationID, typeID), maxHoldSec, famine)
		// A pilot below its cash line lists what clears a smaller floor (listfee.go),
		// since a stack never worth an order slot is never money.
		if !ready && why == mineralBatchLitter &&
			price*float64(qty) >= listFloor(m.isHurting(ctx, b), mineralListMinValue) {
			ready, why = true, mineralBatchAged
		}
		if !ready {
			m.noteMineralHold(ctx, b, typeID, qty)
			held++
			if why == mineralBatchLitter {
				m.logBot(b.CharID, logInfo, "market",
					"%d× %s has sat %d days and is still worth under %.0f ISK — not worth an order slot",
					qty, m.typeNameOr(ctx, typeID), holdDays, mineralListMinValue)
			}
			continue
		}
		if why == mineralBatchFamine {
			m.logBot(b.CharID, logInfo, "market",
				"listing %d× %s short of a full lot — the roster is short of it everywhere and a part load beats none",
				qty, m.typeNameOr(ctx, typeID))
		}
		// The fee is checked before the door is tried (listfee.go): a pilot that
		// cannot pay sells into a standing bid here, or leaves the stack for a while.
		if blocked, sold := m.brokerFeeBlocks(ctx, b, price*float64(qty), func() int64 {
			s, _ := m.distressSell(ctx, b, b.StationID, typeID, itemID, qty)
			return s
		}); blocked {
			if sold > 0 {
				m.clearMineralHold(ctx, b.CharID, b.StationID, typeID)
				listed++
			}
			continue
		}
		out, serr := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
			typeID, itemID, price, qty, -1 /*station*/, 1, mineralSellDays, false)
		if serr != nil {
			m.logBot(b.CharID, logError, "market", "listing minerals: %v", serr)
			continue
		}
		if out.ErrorMsg != "" {
			// A broker fee the pilot cannot pay is the illiquidity deadlock: sell into
			// any standing bid (distress.go). Any other refusal leaves the stack in the hangar.
			if brokerFeeRefusal(out.ErrorMsg) {
				if sold, _ := m.distressSell(ctx, b, b.StationID, typeID, itemID, qty); sold > 0 {
					m.clearMineralHold(ctx, b.CharID, b.StationID, typeID)
					listed++
					continue
				}
			}
			m.logBot(b.CharID, logWarn, "market",
				"mineral listing refused (%s) — leaving the stack in the hangar", out.ErrorMsg)
			continue
		}
		// A listing at or under a standing bid clears at once; that bid is often
		// another rolebot's.
		m.notePeerFills(ctx, tradeEntry{
			CharID: b.CharID, TypeID: typeID, TypeName: m.typeNameOr(ctx, typeID),
			Station: b.StationID, System: b.SystemID,
		}, out.Fills, false, "minerals")
		name := m.typeNameOr(ctx, typeID)
		m.bookBrokerFee(ctx, b, out.RestingOrderID, typeID, name, price, qty-out.FilledQty)
		// Register the resting order with the trade book so the sweep books its
		// fills, reprices it and can pull it. unitCost is 0 (tradebook.go).
		if resting := qty - out.FilledQty; resting > 0 && out.RestingOrderID != 0 {
			m.registerOrder(ctx, b.CharID, out.RestingOrderID, typeID, b.StationID, b.SystemID,
				name, false, price, 0, resting, tradeDealStock)
			m.bookTrade(ctx, tradeEntry{
				CharID: b.CharID, Kind: tradeKindList, TypeID: typeID, TypeName: name,
				Qty: resting, Price: price, Value: price * float64(resting),
				Station: b.StationID, System: b.SystemID, OrderID: out.RestingOrderID,
				Note: fmt.Sprintf("refined minerals — %s", priceWhy),
			})
		}
		// The lot is sold; the next one starts its own clock.
		m.clearMineralHold(ctx, b.CharID, b.StationID, typeID)
		m.logBot(b.CharID, logInfo, "market", "listed %d× %s at %.2f ISK — %s (%s)",
			qty, name, price, priceWhy,
			map[mineralBatchReason]string{
				mineralBatchFloor: "a full lot",
				mineralBatchAged:  fmt.Sprintf("held %d days, listing it short of a full lot", holdDays),
			}[why])
		listed++
	}
	return listed, kept, held, nil
}
