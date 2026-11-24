package rolebots

import (
	"context"

	evedb "gurimoon/db"
)

// The work board's trip fallback (sweep.go). The corporation feeds its build
// orders with standing bids at each Industry base (ceoBuyback), priced at the
// buyback fraction so members and miners sell into them. A mineral nobody sells
// into would never be acquired, so a bid left unfilled past workboardBidStaleSec
// is taken off the book and the unfilled remainder becomes a requisition: a Sales
// hauler sweeps the market for it (cheapest sellers first, up to the cap) and
// delivers it to the Materials division. The shortfall counts pending
// requisitions (corpShortfallWithin), so the bidder does not bid again while the
// trip is out. Deep metals stay on their bids, which are raised so miners go deep
// for them (corpdeep.go).

// workboardBidStaleSec is how long a work-board bid may sit before its
// remainder is fetched instead (reqBidStaleSec's rule for requisition bids).
const workboardBidStaleSec = reqBidStaleSec

// reqWorkboard is the requisition RefKind for work-board material trips.
const reqWorkboard = "workboard"

// workboardSweep turns the corporation's stale work-board bids into buy trips.
func (m *Mod) workboardSweep(ctx context.Context, c *corpCEO) {
	if !m.sweepSettings(ctx).Enabled {
		return
	}
	cutoff := clock.Unix() - workboardBidStaleSec
	rows, err := m.db.QueryContext(ctx, `
		SELECT r.orderID, r.stationID, r.typeID, o.volRemaining, o.charID
		  FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.bid = 1 AND r.placedAt < ? AND r.kind <> ? AND o.orderState = 1 AND o.volRemaining > 0`,
		c.CorpID, cutoff, corpOrderHuman)
	if err != nil {
		return
	}
	type stale struct{ orderID, stationID, typeID, rem, placer int64 }
	var list []stale
	for rows.Next() {
		var s stale
		if rows.Scan(&s.orderID, &s.stationID, &s.typeID, &s.rem, &s.placer) == nil {
			list = append(list, s)
		}
	}
	rows.Close()
	for _, s := range list {
		if isDeepMineral(s.typeID) || !m.workboardWants(ctx, c.CorpID, s.stationID, s.typeID) {
			continue
		}
		if m.reqPending(ctx, c.CorpID, s.typeID, s.stationID, corpDivMaterials) > 0 {
			continue
		}
		if _, err := evedb.CancelMarketOrder(ctx, m.db, s.placer, s.orderID); err != nil {
			rlogf("rolebots: cancel stale work-board bid %d: %v", s.orderID, err)
			continue
		}
		m.forgetCorpOrder(ctx, s.orderID)
		id, err := m.postRequisition(ctx, c, requisition{
			TypeID: s.typeID, Qty: s.rem, DestStationID: s.stationID, DestFlag: corpDivMaterials,
			Requester: "industry", RefKind: reqWorkboard, RefID: s.stationID,
			// Inside the patient window, so the desk sends a buyer rather than
			// placing another bid like the one that just went stale.
			Deadline: clock.Unix() + reqPatientSec/2,
			Note:     "work board: the bid went unfilled",
		})
		if err != nil || id == 0 {
			continue
		}
		m.ceoLog(ctx, c, powerProduction, "workboardtrip", s.typeID, 0,
			"%d× %s went unfilled on the bid at %s for %d days — sending a buyer to the market for it",
			s.rem, m.typeNameOr(ctx, s.typeID), m.stationLabel(ctx, s.stationID), workboardBidStaleSec/86400)
	}
}

// workboardWants is whether an open build order at a base still needs a type; a
// stale bid for something nothing needs is left to expire.
func (m *Mod) workboardWants(ctx context.Context, corpID, stationID, typeID int64) bool {
	bills, err := m.corpOrderBills(ctx, corpID, stationID)
	if err != nil {
		return false
	}
	for _, b := range bills {
		if b[typeID] > 0 {
			return true
		}
	}
	return false
}
