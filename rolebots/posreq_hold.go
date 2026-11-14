package rolebots

import (
	"context"
	"fmt"
	"math"

	evedb "gurimoon/db"
)

// Hold size in the requisition hand-out. The buy trip sizes the buy to the free room
// (posBuyStep), and a Ship Maintenance Array packs to 8,000 m3 while a Hoarder holds 5,100, so
// a line handed to a pilot whose hauler cannot hold one unit would buy none and be put back.
// A line therefore goes only to a pilot whose hauler holds one unit of it, the Sales arm first
// and then anyone in the corporation; a line nobody could ever carry is held with a note.

// reqHaulGroups are the cargo hulls corporation supplies travel in. A titan's
// 60,000 m³ fleet hangar is not a hauler, and the CEO who owns one does not
// make a Ship Maintenance Array carriable.
var reqHaulGroups = []int64{28, 380, 513, 902} // Industrial, Transport Ship, Freighter, Jump Freighter

// reqCargoHullLoadM3 is the load of a tower fuel line from which only a cargo hull (reqHaulGroups)
// buys it. A week of a tower's fuel is thousands of m³; a frigate or shuttle hold moves a few
// percent of it a trip. Below this a charter or a few parts may still ride in any hull.
const reqCargoHullLoadM3 = 100.0

// reqLineWantsCargoHull is whether a requisition line is bulk tower fuel that only a cargo hull
// buys: a fuel-division line of a tower plan (fuel, strontium, charters) of at least
// reqCargoHullLoadM3 still to buy. unitM3 is one unit's volume. Pure.
func reqLineWantsCargoHull(r *requisition, unitM3 float64) bool {
	return r.RefKind == "tower" && r.DestFlag == corpDivFuel && float64(r.left())*math.Max(unitM3, 0) >= reqCargoHullLoadM3
}

// reqHoldBook caches, for one desk pass, unit volumes, buyers' holds and the corporation's
// biggest hold, each a few queries asked per line.
type reqHoldBook struct {
	m        *Mod
	corpID   int64
	unit     map[int64]float64
	hold     map[int64]float64
	cargo    map[int64]bool
	best     float64
	bestDone bool
}

func (m *Mod) newReqHoldBook(corpID int64) *reqHoldBook {
	return &reqHoldBook{m: m, corpID: corpID, unit: map[int64]float64{}, hold: map[int64]float64{}, cargo: map[int64]bool{}}
}

// wantsCargoHull is reqLineWantsCargoHull with the book's unit volumes.
func (h *reqHoldBook) wantsCargoHull(ctx context.Context, r *requisition) bool {
	return reqLineWantsCargoHull(r, h.unitM3(ctx, r.TypeID))
}

// cargoHullOf is whether the hull a pilot buys supplies in is a cargo hull.
func (h *reqHoldBook) cargoHullOf(ctx context.Context, charID int64) bool {
	if v, ok := h.cargo[charID]; ok {
		return v
	}
	v := h.m.reqTradeHullIsCargo(ctx, charID)
	h.cargo[charID] = v
	return v
}

// lineFits is fits for one line: one unit goes into the pilot's hauler and, for bulk tower fuel,
// that hauler is a cargo hull.
func (h *reqHoldBook) lineFits(ctx context.Context, charID int64, r *requisition) bool {
	if !h.fits(ctx, charID, r.TypeID) {
		return false
	}
	return !h.wantsCargoHull(ctx, r) || h.cargoHullOf(ctx, charID)
}

// unitM3 is one unit's packaged volume.
func (h *reqHoldBook) unitM3(ctx context.Context, typeID int64) float64 {
	if v, ok := h.unit[typeID]; ok {
		return v
	}
	v := h.m.typeVolume(ctx, typeID)
	h.unit[typeID] = v
	return v
}

// holdOf is the fitted hold of the hull a pilot buys supplies in.
func (h *reqHoldBook) holdOf(ctx context.Context, charID int64) float64 {
	if v, ok := h.hold[charID]; ok {
		return v
	}
	v := h.m.reqHaulerHold(ctx, charID)
	h.hold[charID] = v
	return v
}

// fits is whether one unit of the type goes into the pilot's hauler.
func (h *reqHoldBook) fits(ctx context.Context, charID, typeID int64) bool {
	if charID == 0 {
		return false
	}
	return reqHoldFits(h.unitM3(ctx, typeID), h.holdOf(ctx, charID))
}

// corpBest is the biggest fitted hold among the cargo hulls the
// corporation's rostered pilots own, wherever they are parked.
func (h *reqHoldBook) corpBest(ctx context.Context) float64 {
	if h.bestDone {
		return h.best
	}
	h.bestDone = true
	rows, err := h.m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN character_ c ON c.characterID = e.ownerID
		  JOIN rolebot_characters b ON b.characterID = c.characterID AND b.enabled = 1
		 WHERE c.corporationID = ? AND t.groupID IN (`+int64CSV(reqHaulGroups)+`)
		 ORDER BY t.capacity DESC LIMIT 25`, h.corpID)
	if err != nil {
		return 0
	}
	type hull struct{ item, typeID int64 }
	var hulls []hull
	for rows.Next() {
		var x hull
		if rows.Scan(&x.item, &x.typeID) == nil {
			hulls = append(hulls, x)
		}
	}
	rows.Close()
	for _, x := range hulls {
		if c, err := evedb.GetShipCargoCapacityFitted(ctx, h.m.db, x.typeID, x.item); err == nil && c > h.best {
			h.best = c
		}
	}
	return h.best
}

// tooBigNote is the hold note for a line no hauler in the corporation can
// carry one unit of, "" when one can. The seat's own hauler is asked first:
// it is cached for the pass already, and when it takes a unit the
// corporation-wide scan is not needed.
func (h *reqHoldBook) tooBigNote(ctx context.Context, typeID, seat int64) string {
	need := h.unitM3(ctx, typeID)
	if need <= 0 || h.fits(ctx, seat, typeID) {
		return ""
	}
	best := h.corpBest(ctx)
	if reqHoldFits(need, best) {
		return ""
	}
	return reqTooBigNote(need, best)
}

// reqHoldFits is whether one unit of need m³ goes into a hold. A type with no
// volume fits anything; no hold fits nothing else. Pure.
func reqHoldFits(need, hold float64) bool {
	return need <= 0 || hold >= need
}

// reqTooBigNote words the hold note. Pure.
func reqTooBigNote(need, best float64) string {
	if best <= 0 {
		return fmt.Sprintf("too big to haul: %.0f m³ a unit, and nobody in the corporation owns a cargo hull", need)
	}
	return fmt.Sprintf("too big to haul: %.0f m³ a unit, and the biggest hold in the corporation is %.0f m³", need, best)
}

// reqWaitNote is the note on a line somebody in the corporation could carry
// but no such pilot is free. Pure.
func reqWaitNote(need float64) string {
	return fmt.Sprintf("waiting for a free hauler that holds %.0f m³", need)
}

// reqWaitCargoHullNote is the note on a bulk tower fuel line somebody in the corporation could
// buy but no pilot with a cargo hull is free. Pure.
const reqWaitCargoHullNote = "waiting for a free pilot with an industrial hull"

// reqRunaboutM3 is the hold a pilot with no trade hull still has: boardHauler
// falls back to the runabout (ensureRunabout), a shuttle, which carries a
// blueprint or a few small parts and nothing a tower is built from.
const reqRunaboutM3 = 10

// reqHaulerHold is the fitted cargo hold of the hull a pilot buys
// corporation supplies in: its trade hull, which boardHauler boards at the
// seller, or a runabout's when it has none.
func (m *Mod) reqHaulerHold(ctx context.Context, charID int64) float64 {
	ship, _, err := m.shipForActivity(ctx, charID, ActTrade)
	if err != nil || ship == nil || !ship.ShipExists {
		return reqRunaboutM3
	}
	return math.Max(reqRunaboutM3, m.shipItemHold(ctx, ship.ShipItemID))
}

// reqTradeHullIsCargo is whether the hull a pilot buys corporation supplies in (its trade hull) is
// an industrial or bigger cargo hull. A pilot with no trade hull is not.
func (m *Mod) reqTradeHullIsCargo(ctx context.Context, charID int64) bool {
	ship, _, err := m.shipForActivity(ctx, charID, ActTrade)
	if err != nil || ship == nil || !ship.ShipExists {
		return false
	}
	return m.shipItemIsCargoHull(ctx, ship.ShipItemID)
}

// shipItemIsCargoHull is whether one ship's hull is in reqHaulGroups.
func (m *Mod) shipItemIsCargoHull(ctx context.Context, shipItemID int64) bool {
	var group int64
	if m.db.QueryRowContext(ctx, `
		SELECT t.groupID FROM entity e JOIN invTypes t ON t.typeID = e.typeID WHERE e.itemID = ?`,
		shipItemID).Scan(&group) != nil {
		return false
	}
	for _, g := range reqHaulGroups {
		if g == group {
			return true
		}
	}
	return false
}

// shipItemHold is one ship's fitted cargo hold.
func (m *Mod) shipItemHold(ctx context.Context, shipItemID int64) float64 {
	var typeID int64
	if m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, shipItemID).Scan(&typeID) != nil {
		return 0
	}
	c, _ := evedb.GetShipCargoCapacityFitted(ctx, m.db, typeID, shipItemID)
	return c
}
