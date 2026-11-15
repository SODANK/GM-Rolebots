package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"

	evedb "gurimoon/db"
)

// stationLocalMarket reports whether a station a corporation owns can carry a local market: it offers
// the Market service. Outposts do; conquerables do not, so their production ships home
// (exportFromNoMarket). The outposts' local-market desk hangs on this test.
func (m *Mod) stationLocalMarket(ctx context.Context, stationID int64) bool {
	return evedb.StationHasService(ctx, m.db, stationID, evedb.ServiceMarket)
}

// exportOutlet is where a corporation's 0.0 production is sold: its trading post (officeSales),
// else its headquarters, when that station has a market and an office of the corporation's. 0 when
// neither does.
func (m *Mod) exportOutlet(ctx context.Context, corpID int64) int64 {
	var cands []int64
	if st, err := int64Column(ctx, m, `
		SELECT stationID FROM rolebot_corp_offices
		 WHERE corporationID = ? AND (purposes & ?) <> 0 AND state <> 'dropping' ORDER BY stationID`,
		corpID, officeSales); err == nil {
		cands = append(cands, st...)
	}
	cands = append(cands, m.corpHQStation(ctx, corpID))
	for _, st := range cands {
		if st != 0 && m.officeAt(ctx, corpID, st) != 0 && evedb.StationHasService(ctx, m.db, st, evedb.ServiceMarket) {
			return st
		}
	}
	return 0
}

// exportFromNoMarket posts move legs carrying a market-less factory's Production lots (ships,
// modules, charges, drones; never what the corporation's own programs need) to its export outlet,
// outletHaulsPerPass a pass, and answers every type on its way out.
func (m *Mod) exportFromNoMarket(ctx context.Context, c *corpCEO, stationID, officeID int64, outbound map[int64]bool) map[int64]bool {
	to := m.exportOutlet(ctx, c.CorpID)
	if to == 0 || to == stationID {
		return outbound
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ? AND e.quantity > 0
		   AND g.categoryID IN (?, ?, ?, ?)
		 GROUP BY e.typeID ORDER BY e.typeID`, c.CorpID, officeID, corpDivProduct, catShip, catModule, catCharge, catDrone)
	if err != nil {
		return outbound
	}
	type lot struct{ typeID, qty int64 }
	var lots []lot
	for rows.Next() {
		var l lot
		if rows.Scan(&l.typeID, &l.qty) == nil {
			lots = append(lots, l)
		}
	}
	rows.Close()
	programParts := m.programPartTypes(ctx, c.CorpID)
	label := m.stationLabel(ctx, to)
	posted := 0
	for _, l := range lots {
		if posted >= outletHaulsPerPass {
			break
		}
		if outbound[l.typeID] || programParts[l.typeID] {
			continue
		}
		if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindMove, FromKind: haulFromOffice, FromID: officeID,
			FromStationID: stationID, FromFlag: corpDivProduct, ToKind: haulToOffice, ToStationID: to,
			ToFlag: corpDivProduct, Items: []haulItem{{TypeID: l.typeID, Qty: l.qty, Flag: corpDivProduct}},
			PostedBy: "sales", Deadline: clock.Unix() + outletLegDeadlineSec,
			Note: fmt.Sprintf("%s %s %s", m.typeNameOr(ctx, l.typeID), outletNote, label)}); err != nil {
			rlogf("rolebots: corp export haul %d: %v", c.CorpID, err)
			continue
		}
		outbound[l.typeID] = true
		posted++
		m.ceoLog(ctx, c, powerProduction, "export", l.typeID, 0,
			"shipped %d× %s home to %s: there is no market out here", l.qty, m.typeNameOr(ctx, l.typeID), label)
	}
	return outbound
}

// Corporation outlets: a corporation's goods go to where its buyers are. ceoOutput lists the Production
// division at the factory office, but a shopper comes to a counter that fills its list (reach.go), and the
// shopper-demand ledger (stationdemand.go) says which counters those are. So before the factory list pass the
// CEO looks at each durable lot in Production (modules, charges, drones) and, where one of the corporation's
// own offices in reach pulls clearly harder than the factory, posts a move leg on the haul board (poshaul.go)
// carrying the units that counter would take into that office's Production division. The tail of ceoOutput
// lists durables there. The leg is the same move the department filing posts (corprequest.go), so the board,
// the wage and courier outsourcing apply unchanged.

const (
	// outletHaulsPerPass caps the legs one output pass posts.
	outletHaulsPerPass = 3
	// outletLegDeadlineSec is how long a carrier has before the leg goes out
	// to contract (poscourier.go outsources at half).
	outletLegDeadlineSec = 4 * 86400
	// outletNote marks the legs, for the board and the log.
	outletNote = "to market at"
)

// corpOfficeSite is one of a corporation's offices.
type corpOfficeSite struct {
	StationID, OfficeID, SystemID int64
}

// corpOfficeSites is every office the corporation holds.
func (m *Mod) corpOfficeSites(ctx context.Context, corpID int64) []corpOfficeSite {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.itemID, s.solarSystemID
		  FROM crpoffices o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ?
		 ORDER BY o.stationID`, corpID)
	if err != nil {
		rlogf("rolebots: corp offices %d: %v", corpID, err)
		return nil
	}
	defer rows.Close()
	var out []corpOfficeSite
	for rows.Next() {
		var s corpOfficeSite
		if rows.Scan(&s.StationID, &s.OfficeID, &s.SystemID) == nil && s.OfficeID != s.StationID {
			out = append(out, s)
		}
	}
	return out
}

// outletCandidate is one office a lot could be carried to.
type outletCandidate struct {
	StationID int64
	Jumps     int
	Pull      float64 // units shoppers there would take
}

// pickCorpOutlet chooses where a lot of qty units worth unitValue each (m3 each) is best listed: the factory's
// own shopper pull against each office's pull less the haul wage. ok=false keeps the lot at the factory when
// nothing beats it by outletMinEdgeFrac or nothing is worth leakMinISK. units is what the chosen counter would
// take. Pure.
func pickCorpOutlet(homePull float64, cands []outletCandidate, qty int64, unitValue, m3 float64) (
	best outletCandidate, units int64, ok bool) {

	if qty <= 0 || unitValue <= 0 {
		return outletCandidate{}, 0, false
	}
	home := min(float64(qty), homePull) * unitValue
	bestV := 0.0
	for _, c := range cands {
		if c.Pull <= 0 {
			continue
		}
		n := min(qty, int64(math.Ceil(c.Pull)))
		v := float64(n)*unitValue - haulWageFor(m3*float64(n), c.Jumps)
		if v < leakMinISK || v <= home*(1+outletMinEdgeFrac) {
			continue
		}
		if !ok || v > bestV || (v == bestV && (c.Jumps < best.Jumps ||
			(c.Jumps == best.Jumps && c.StationID < best.StationID))) {
			best, units, bestV, ok = c, n, v, true
		}
	}
	return best, units, ok
}

// ceoOutletHauls posts the move legs for this pass and answers every type
// that is, or is now, on its way out of the factory office, so the factory
// list pass leaves those lots for the carrier.
func (m *Mod) ceoOutletHauls(ctx context.Context, c *corpCEO, stationID, officeID int64) map[int64]bool {
	outbound := map[int64]bool{}
	legs, err := m.queryHauls(ctx, `WHERE corporationID = ? AND kind = ? AND fromKind = ? AND fromID = ?
		AND status IN ('open', 'claimed', 'contract')`, c.CorpID, haulKindMove, haulFromOffice, officeID)
	if err == nil {
		for _, l := range legs {
			for _, it := range l.Items {
				outbound[it.TypeID] = true
			}
		}
	}
	// A factory at a station with no market (a conquerable capital) lists nothing: everything it
	// builds ships home to be sold (exportFromNoMarket).
	if !evedb.StationHasService(ctx, m.db, stationID, evedb.ServiceMarket) {
		return m.exportFromNoMarket(ctx, c, stationID, officeID, outbound)
	}
	if !m.outletEnabled(ctx) {
		return outbound
	}
	offices := m.corpOfficeSites(ctx, c.CorpID)
	if len(offices) < 2 {
		return outbound
	}
	jumps, err := m.corpJumpMap(ctx, m.stationSystemID(ctx, stationID))
	if err != nil {
		return outbound
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ? AND e.quantity > 0
		   AND g.categoryID IN (?, ?, ?)
		 GROUP BY e.typeID`, c.CorpID, officeID, corpDivProduct, catModule, catCharge, catDrone)
	if err != nil {
		rlogf("rolebots: corp outlet lots %d: %v", c.CorpID, err)
		return outbound
	}
	type lot struct{ typeID, qty int64 }
	var lots []lot
	for rows.Next() {
		var l lot
		if rows.Scan(&l.typeID, &l.qty) == nil {
			lots = append(lots, l)
		}
	}
	rows.Close()
	sort.Slice(lots, func(i, j int) bool { return lots[i].typeID < lots[j].typeID })

	demand := m.stationDemand(ctx)
	programParts := m.programPartTypes(ctx, c.CorpID)
	region := m.stationRegionID(ctx, stationID)
	posted := 0
	for _, l := range lots {
		if posted >= outletHaulsPerPass {
			break
		}
		if outbound[l.typeID] || programParts[l.typeID] {
			continue
		}
		value := m.productTradeValue(ctx, l.typeID, region)
		if value <= 0 {
			value = m.corpUnitCost(ctx, c.CorpID, l.typeID)
		}
		homePull := 0.0
		if r, ok := demand.row(l.typeID, stationID); ok {
			homePull = r.Pull()
		}
		var cands []outletCandidate
		for _, o := range offices {
			if o.StationID == stationID {
				continue
			}
			j, ok := jumps[o.SystemID]
			if !ok || j > tradePostMaxJumps {
				continue
			}
			if r, ok := demand.row(l.typeID, o.StationID); ok {
				cands = append(cands, outletCandidate{StationID: o.StationID, Jumps: j, Pull: r.Pull()})
			}
		}
		best, units, ok := pickCorpOutlet(homePull, cands, l.qty, value, m.typeVolume(ctx, l.typeID))
		if !ok {
			continue
		}
		label := m.stationLabel(ctx, best.StationID)
		if _, err := m.postHaul(ctx, c, haulLeg{Kind: haulKindMove, FromKind: haulFromOffice, FromID: officeID,
			FromStationID: stationID, FromFlag: corpDivProduct, ToKind: haulToOffice, ToStationID: best.StationID,
			ToFlag: corpDivProduct, Items: []haulItem{{TypeID: l.typeID, Qty: units, Flag: corpDivProduct}},
			PostedBy: "sales", Deadline: clock.Unix() + outletLegDeadlineSec,
			Note: fmt.Sprintf("%s %s %s", m.typeNameOr(ctx, l.typeID), outletNote, label)}); err != nil {
			rlogf("rolebots: corp outlet haul %d: %v", c.CorpID, err)
			continue
		}
		outbound[l.typeID] = true
		posted++
		m.ceoLog(ctx, c, powerProduction, "outlet", l.typeID, value*float64(units),
			"sent %d× %s %d jump(s) to our office at %s, where shoppers came for it this week and would take about %.0f",
			units, m.typeNameOr(ctx, l.typeID), best.Jumps, label, best.Pull)
	}
	return outbound
}
