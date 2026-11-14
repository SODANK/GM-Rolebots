package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// The deep-metal carry: getting the roster's Zydrine and Megacyte to the
// corporations bidding for them. Stacks sit in hangars where no bid rests, so a
// member of a managed corporation carries them: on its visit home, with nothing
// of its own in hand and at least deepCarry.minUnits of a deep metal in the
// hangar it is docked at, it finds its own corporation's resting bid within
// corpWorkMaxJumps (valued up by its obedience as sellInToCorpBids does), loads
// what the hold takes, flies it there and sells into the bid. The errand also
// starts at the end of a warehouse run (warehouseStep) and covers Nocxium
// (deepPricedTypes). See docs/comment-notes-b04.md#deep-carry.

const (
	deepCarryMinUnitsKey     = "rolebots.deepCarry.minUnits"
	deepCarryMinUnitsDefault = 150
)

// CarryState is the deep-metal errand (corpdeepcarry.go). Not carried across a
// re-plan: a dropped errand leaves the stack in the hold, where the hold
// sweeps find it, and the next visit home starts a fresh one.
type CarryState struct {
	CarryTypeID    int64 `json:"carryTypeID,omitempty"`
	CarryUnits     int64 `json:"carryUnits,omitempty"`
	CarryStationID int64 `json:"carryStationID,omitempty"`
	CarrySysID     int64 `json:"carrySysID,omitempty"`
	CarryCorpID    int64 `json:"carryCorpID,omitempty"`
}

func deepCarryMinUnits(ctx context.Context, db *sql.DB) int64 {
	return int64(max(escrowSettingInt(ctx, db, deepCarryMinUnitsKey, deepCarryMinUnitsDefault), 1))
}

// deepBidTarget is one resting corporation bid for a deep metal.
type deepBidTarget struct {
	CorpID, TypeID, StationID, SystemID int64
	Price                               float64
	Rem                                 int64
	Jumps                               int
}

// deepCarryWorth reports whether a bid is worth carrying a stack to: the
// bid, valued up by the carrier's obedience, meets the metal's value, and
// the bid wants at least one lot of it. Pure.
func deepCarryWorth(price, nudge, value float64, rem, lot int64) bool {
	return price*nudge >= value && rem >= lot && price > 0
}

// startDeepCarry is the home-visit hook: begins the errand when there is one
// to run, reporting whether it did.
func (m *Mod) startDeepCarry(ctx context.Context, b *botRun) (bool, error) {
	if b == nil || b.CorpID == 0 || b.State == nil || b.State.Data.IndTypeID != 0 || b.StationID == 0 {
		return false, nil
	}
	if _, ok := managedCEO(b.CorpID); !ok {
		return false, nil
	}
	minUnits := deepCarryMinUnits(ctx, m.db)
	// The deep metal this pilot holds most of here, if a lot of it.
	var typeID, qty int64
	args := append([]any{b.CharID, b.StationID}, int64Args(deepPricedTypes)...)
	if m.db.QueryRowContext(ctx, `
		SELECT typeID, SUM(quantity) FROM entity
		 WHERE ownerID = ? AND locationID = ? AND flag = 4 AND singleton = 0
		   AND typeID IN (`+placeholders(len(deepPricedTypes))+`)
		 GROUP BY typeID ORDER BY SUM(quantity) DESC LIMIT 1`,
		args...).Scan(&typeID, &qty) != nil || qty < minUnits {
		return false, nil
	}
	// The corporation's own resting bids for it, elsewhere.
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.stationID, s.solarSystemID, o.price, o.volRemaining
		  FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		  JOIN staStations s ON s.stationID = r.stationID
		 WHERE r.corporationID = ? AND r.bid = 1 AND r.typeID = ? AND r.stationID <> ?
		   AND o.orderState = 1 AND o.volRemaining > 0`, b.CorpID, typeID, b.StationID)
	if err != nil {
		return false, nil
	}
	var bids []deepBidTarget
	for rows.Next() {
		t := deepBidTarget{CorpID: b.CorpID, TypeID: typeID}
		if rows.Scan(&t.StationID, &t.SystemID, &t.Price, &t.Rem) == nil {
			bids = append(bids, t)
		}
	}
	rows.Close()
	if len(bids) == 0 {
		return false, nil
	}
	value := evedb.TypeValueOf(ctx, m.db, typeID).Value
	nudge := 1 + corpLoyalNudge*math.Max(0, b.Tune.Obedience)
	lot := mineralListFloors[typeID]
	var best *deepBidTarget
	for i := range bids {
		t := &bids[i]
		if !deepCarryWorth(t.Price, nudge, value, t.Rem, lot) {
			continue
		}
		j, jerr := m.jumpsBetween(ctx, b, b.SystemID, t.SystemID)
		if jerr != nil || j > corpWorkMaxJumps {
			continue
		}
		t.Jumps = j
		// Nearest first; the dearer bid breaks a tie.
		if best == nil || t.Jumps < best.Jumps || (t.Jumps == best.Jumps && t.Price > best.Price) {
			best = t
		}
	}
	if best == nil {
		return false, nil
	}
	name := m.typeNameOr(ctx, typeID)
	if _, ok := m.ensureRunabout(ctx, b, "carrying "+name+" to its corporation's bid"); !ok {
		return false, nil
	}
	loaded, err := m.moveHangarToCargoN(ctx, b, typeID)
	if err != nil || loaded <= 0 {
		return false, nil
	}
	d := &b.State.Data
	d.CarryTypeID, d.CarryUnits, d.CarryStationID, d.CarrySysID, d.CarryCorpID =
		typeID, min(loaded, best.Rem), best.StationID, best.SystemID, best.CorpID
	m.logBot(b.CharID, logInfo, "corp", "carrying %d× %s %d jump(s) to its corporation's bid at %s (%s a unit)",
		loaded, name, best.Jumps, m.stationLabel(ctx, best.StationID), iskString(best.Price))
	return true, m.beginTravel(ctx, b, best.SystemID, best.StationID, string(TaskDeepCarry))
}

// deepCarryStep is the errand's docked step: arrived, unload and sell in.
func (m *Mod) deepCarryStep(ctx context.Context, b *botRun) (bool, error) {
	d := &b.State.Data
	if d.CarryTypeID == 0 || d.CarryStationID == 0 {
		return false, m.finishDeepCarry(ctx, b, "")
	}
	if b.StationID != d.CarryStationID {
		return false, m.beginTravel(ctx, b, d.CarrySysID, d.CarryStationID, string(TaskDeepCarry))
	}
	name := m.typeNameOr(ctx, d.CarryTypeID)
	if err := m.moveCargoToHangar(ctx, b, d.CarryTypeID); err != nil {
		return false, m.finishDeepCarry(ctx, b, fmt.Sprintf("could not unload the %s: %v", name, err))
	}
	var itemID, qty int64
	if m.db.QueryRowContext(ctx, `
		SELECT itemID, quantity FROM entity
		 WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
		 ORDER BY quantity DESC LIMIT 1`, b.CharID, b.StationID, d.CarryTypeID).Scan(&itemID, &qty) != nil || qty <= 0 {
		return false, m.finishDeepCarry(ctx, b, fmt.Sprintf("arrived without the %s — nothing to sell", name))
	}
	took, value, err := m.dumpToBestBid(ctx, b, d.CarryTypeID, itemID, name, qty,
		bidDump{Note: "deep minerals carried to its corporation's bid", Tag: "minerals", PlayerOnly: true})
	if err != nil || took <= 0 {
		// The bid filled or lapsed on the way. The stack is in this hangar
		// now, where the ordinary mineral sweep lists it (mineralstrays.go).
		return false, m.finishDeepCarry(ctx, b, fmt.Sprintf("the bid for %s was gone on arrival — left it in the hangar here", name))
	}
	return false, m.finishDeepCarry(ctx, b, fmt.Sprintf("sold %d× %s into its corporation's bid for %s",
		took, name, iskString(value)))
}

// finishDeepCarry ends the errand and hands the pilot back to the planner.
func (m *Mod) finishDeepCarry(ctx context.Context, b *botRun, note string) error {
	st := b.State
	if note != "" {
		m.logBot(b.CharID, logInfo, "corp", "%s", note)
	}
	st.Data.CarryState = CarryState{}
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	return saveState(ctx, m.db, st)
}

// detailDeepCarry is the panel's line for the errand.
func detailDeepCarry(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	if d.CarryTypeID == 0 {
		return ""
	}
	return fmt.Sprintf("carrying %d× %s to its corporation's bid", d.CarryUnits, m.typeNameOr(ctx, d.CarryTypeID))
}
