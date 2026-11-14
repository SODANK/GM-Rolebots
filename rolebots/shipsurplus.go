package rolebots

// Surplus hulls: a flat cap on the ships a pilot keeps.
//
// trimSurplusHulls keeps the ship the pilot sits in, its current activity's
// fit ship, one shuttle and rolebots.ships.spares more (fit ships first, then
// the dearer hull); everything else is surplus. A surplus hull at the station
// the pilot is docked in is repackaged (the core's UnassembleItem, so its
// modules and cargo land in the hangar for the stray sweep that runs next)
// and then judged like any dead stock (deadstock.go): sold into a bid in reach,
// listed where the region buys the hull, else reprocessed. Surplus elsewhere
// waits for a visit; the warehouse survey counts it.

import (
	"context"
	"sort"
	"strings"

	evedb "gurimoon/db"
)

const (
	shipsTrimKey        = "rolebots.ships.trimEnabled"
	shipsSparesKey      = "rolebots.ships.spares"
	shipsMaxPerVisitKey = "rolebots.ships.maxPerVisit"

	shipsSparesDefault      = 3
	shipsMaxPerVisitDefault = 2

	// procurePhasePrefix starts every procurement phase (procure.go).
	procurePhasePrefix = "procure."
)

// hullUnit is one ship row the pilot owns in a station hangar.
type hullUnit struct {
	ItemID    int64
	TypeID    int64
	StationID int64
	Units     int64 // 1 for an assembled hull, the stack size for a packaged one
	Packaged  bool
	Base      float64
	Shuttle   bool
	AtHome    bool
	FitShip   bool // recorded against a fit (rolebot_fit_ships)
	Protected bool // flown, the activity's ship, claimed, issued or insured
}

// pickSurplusHulls is how many units of each row are surplus: protected rows
// are never counted, one shuttle is kept (assembled, then at home, first),
// then spares more units by rank (fit ships, then the dearer hull, then the
// older item). Pure.
func pickSurplusHulls(hulls []hullUnit, spares int) map[int64]int64 {
	type unit struct {
		h   hullUnit
		idx int
	}
	var shuttles, rest []unit
	for i, h := range hulls {
		if h.Protected || h.Units <= 0 {
			continue
		}
		if h.Shuttle {
			shuttles = append(shuttles, unit{h, i})
		} else {
			rest = append(rest, unit{h, i})
		}
	}
	out := map[int64]int64{}
	for _, u := range shuttles {
		out[u.h.ItemID] += u.h.Units
	}
	if len(shuttles) > 0 {
		sort.SliceStable(shuttles, func(i, j int) bool {
			a, b := shuttles[i].h, shuttles[j].h
			if a.Packaged != b.Packaged {
				return !a.Packaged
			}
			if a.AtHome != b.AtHome {
				return a.AtHome
			}
			return a.ItemID < b.ItemID
		})
		out[shuttles[0].h.ItemID]--
	}
	sort.SliceStable(rest, func(i, j int) bool {
		a, b := rest[i].h, rest[j].h
		if a.FitShip != b.FitShip {
			return a.FitShip
		}
		if a.Base != b.Base {
			return a.Base > b.Base
		}
		return a.ItemID < b.ItemID
	})
	keep := spares
	for _, u := range rest {
		n := u.h.Units
		k := min(int64(max(keep, 0)), n)
		keep -= int(k)
		if n-k > 0 {
			out[u.h.ItemID] += n - k
		}
	}
	for id, n := range out {
		if n <= 0 {
			delete(out, id)
		}
	}
	return out
}

// pilotHulls reads every ship the pilot owns in a station hangar, capsules
// left out, marking the ones nothing may touch.
func (m *Mod) pilotHulls(ctx context.Context, b *botRun) []hullUnit {
	protected := map[int64]bool{b.ShipID: true}
	if b.State != nil && b.State.Data.ProcureClaimedHull != 0 {
		protected[b.State.Data.ProcureClaimedHull] = true
	}
	if b.State != nil {
		act := b.State.Data.LastActivity
		if act == "" {
			act = b.State.Activity
		}
		if act != "" {
			if fs, _, err := m.shipForActivity(ctx, b.CharID, act); err == nil && fs != nil {
				protected[fs.ShipItemID] = true
			}
		}
	}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT shipItemID, ownShipID FROM rolebot_corp_issue WHERE characterID = ? AND status = 'issued'`,
		b.CharID); err == nil {
		for rows.Next() {
			var a, c int64
			if rows.Scan(&a, &c) == nil {
				protected[a], protected[c] = true, true
			}
		}
		rows.Close()
	}
	fitShips := map[int64]bool{}
	if ships, err := shipsForChar(ctx, m.db, b.CharID); err == nil {
		for _, id := range ships {
			fitShips[id] = true
		}
	}
	home := m.homeStationFor(ctx, b.CharID)
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.locationID, e.quantity, e.singleton, COALESCE(t.basePrice, 0), t.groupID,
		       EXISTS (SELECT 1 FROM insurance i WHERE i.shipID = e.itemID)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 6
		 WHERE e.ownerID = ? AND e.flag = 4 AND e.locationID BETWEEN 60000000 AND 64000000
		   AND t.groupID <> ?`, b.CharID, capsuleGroupID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []hullUnit
	for rows.Next() {
		var h hullUnit
		var single, group int64
		var insured bool
		if rows.Scan(&h.ItemID, &h.TypeID, &h.StationID, &h.Units, &single, &h.Base, &group, &insured) != nil {
			continue
		}
		h.Packaged = single == 0
		if !h.Packaged {
			h.Units = 1
		}
		h.Shuttle = group == shuttleGroupID
		h.AtHome = home != 0 && h.StationID == home
		h.FitShip = fitShips[h.ItemID]
		h.Protected = protected[h.ItemID] || insured
		out = append(out, h)
	}
	return out
}

// trimSurplusHulls retires surplus hulls docked here, at most
// rolebots.ships.maxPerVisit a pass and inside the dead-stock budget.
func (m *Mod) trimSurplusHulls(ctx context.Context, b *botRun, cfg deadStockConfig, bg *deadStockBudget,
	got map[int64]int64, tally *deadStockOutcome) {

	if b.StationID == 0 || !bg.ok() || escrowSettingInt(ctx, m.db, shipsTrimKey, 1) == 0 {
		return
	}
	if b.State != nil && strings.HasPrefix(b.State.Phase, procurePhasePrefix) {
		return // a pilot mid-purchase is about to board one of these
	}
	hulls := m.pilotHulls(ctx, b)
	surplus := pickSurplusHulls(hulls, max(escrowSettingInt(ctx, m.db, shipsSparesKey, shipsSparesDefault), 0))
	if len(surplus) == 0 {
		return
	}
	perVisit := max(escrowSettingInt(ctx, m.db, shipsMaxPerVisitKey, shipsMaxPerVisitDefault), 0)
	done := 0
	for _, h := range hulls {
		n := surplus[h.ItemID]
		if n <= 0 || h.StationID != b.StationID || done >= perVisit || !bg.ok() {
			continue
		}
		name := m.typeNameOr(ctx, h.TypeID)
		itemID := h.ItemID
		if !h.Packaged {
			if itemID == b.ShipID {
				continue
			}
			if _, err := evedb.UnassembleItem(ctx, m.db, b.CharID, itemID); err != nil {
				rlogf("rolebots: repackage surplus hull char=%d item=%d: %v", b.CharID, itemID, err)
				continue
			}
			_, _ = m.db.ExecContext(ctx,
				`DELETE FROM rolebot_fit_ships WHERE characterID = ? AND shipItemID = ?`, b.CharID, itemID)
			m.logBot(b.CharID, logInfo, "deadstock",
				"repackaged a spare %s (entity %d) — it keeps %d hull(s) and one shuttle", name, itemID,
				max(escrowSettingInt(ctx, m.db, shipsSparesKey, shipsSparesDefault), 0))
		}
		f := m.deadStockFactsFor(ctx, b, cfg, dsHull, h.TypeID, 0, 6, n, 0, m.strayAsk(ctx, b, h.TypeID, h.Base))
		p := deadStockVerdict(f)
		if p.Act == dsReprocess {
			p.Units = min(p.Units, n)
		}
		if m.applyDeadStock(ctx, b, bg, p, h.TypeID, itemID, name, got, tally) > 0 || !h.Packaged {
			done++
		}
	}
}
