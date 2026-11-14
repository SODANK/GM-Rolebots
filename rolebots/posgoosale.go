package rolebots

import (
	"context"
	"fmt"
	"math"
)

// Moon-goods sale runs: a tower's goo carried to the bidder that pays for it. The
// Infrastructure arm hauls the harvest to the staging office's Production division
// (posindustry.go, haulKindBackhaul); the T2 manufacturers who buy it bid at their own stations.
// When the best bid within gooSaleMaxJumps, net of sales tax and the hauler's wage, beats the
// home listing by gooSaleMinGainISK, the corporation posts a line. A Sales member with a
// hauler loads the lot at the staging office as corporation property (no purchase, no advance),
// sells it into the bids there and hands every ISK to the corporation (sellTradeRunHere).
// The line is a Trade desk line (requester trade, refKind goosale) on the desk's rails. It
// differs in its source (posGooTrip, loadGooLine), its carrier (the Sales arm, else the whole
// roster: gooSaleAssign) and its floor: MaxPrice is the least a unit may sell for.

const (
	refGooSale = "goosale" // rolebot_corp_requisition.refKind of a moon-goods sale run

	// gooSaleMaxJumps is how far from the staging office a bidder may be.
	gooSaleMaxJumps = tradeDeskHaulMaxJumps
	// gooSaleMaxOpen caps the sale runs a corporation has in flight.
	gooSaleMaxOpen = 3
	// gooSaleMinGainISK is what a run must clear over listing the lot at
	// home, after the tax and the wage.
	gooSaleMinGainISK = 100_000
	// gooSaleBidRows bounds the bids read per type.
	gooSaleBidRows = 400
)

// gooBid is the best bid for a type at one station within reach of the
// staging office, and how many units bids at that price take.
type gooBid struct {
	StationID, SystemID int64
	Price               float64
	Vol                 int64
	Jumps               int
}

// gooSaleGain is what carrying take units to a bid earns over listing them at home: the bid
// net of the sales tax, less the hauler's wage, less the listing's gross price. The run must
// win plainly to be worth a member's trip, because a listing may rest and never fill. Pure.
func gooSaleGain(bidPrice float64, take int64, unitVol float64, jumps int, tax, listPrice float64) (gain, wage float64) {
	if take <= 0 {
		return 0, 0
	}
	wage = haulWageFor(float64(take)*unitVol, jumps)
	gain = bidPrice*float64(take)*(1-tax) - wage - listPrice*float64(take)
	return gain, wage
}

// gooSaleClears is the go/no-go rule: the run beats the listing by at least
// minGain. Pure.
func gooSaleClears(gain, minGain float64) bool {
	return gain >= minGain
}

// pickGooBid is the bid a lot of qty units goes to: the one whose run gains
// most over the listing, and the units it takes. ok is false when none
// clears. Pure.
func pickGooBid(bids []gooBid, qty int64, unitVol, tax, listPrice, minGain float64) (best gooBid, take int64, gain float64, ok bool) {
	for _, b := range bids {
		t := min(qty, b.Vol)
		if t <= 0 || b.Price <= 0 {
			continue
		}
		g, _ := gooSaleGain(b.Price, t, unitVol, b.Jumps, tax, listPrice)
		if !gooSaleClears(g, minGain) {
			continue
		}
		if !ok || g > gain || (g == gain && b.StationID < best.StationID) {
			best, take, gain, ok = b, t, g, true
		}
	}
	return best, take, gain, ok
}

// gooSaleFloor is the least a unit of the run may sell for at the far end and
// still beat the listing at home after the tax and the whole wage, to the
// cent. Pure.
func gooSaleFloor(listPrice float64, take int64, wage, tax float64) float64 {
	if take <= 0 || tax >= 1 {
		return 0
	}
	return math.Ceil((listPrice*float64(take)+wage)/(float64(take)*(1-tax))*100) / 100
}

// gooSellsHere is whether a line's carrier sells at its destination: a
// moon-goods run sells at the bidder it was posted for, office there or not.
// Its remainder, redirected home, is filed instead. Pure.
func gooSellsHere(r *requisition) bool {
	return r.RefKind == refGooSale && r.DestStationID == r.RefID
}

// withoutGooLines is reqs less the moon-goods runs. Pure.
func withoutGooLines(reqs []requisition) []requisition {
	out := reqs[:0:0]
	for _, r := range reqs {
		if r.RefKind != refGooSale {
			out = append(out, r)
		}
	}
	return out
}

// gooBidsInReach is the best outside bid per station within gooSaleMaxJumps of the staging
// station, from bidders that take a lot of qty (minVolume). The staging station itself is
// left out: listTowerGoods takes its bids directly.
func (m *Mod) gooBidsInReach(ctx context.Context, corpID, typeID, staging, qty int64) []gooBid {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return nil
	}
	reach := bfsJumps(u, m.stationSystemID(ctx, staging), gooSaleMaxJumps)
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, o.solarSystemID, o.price, o.volRemaining FROM market_orders o
		 WHERE o.typeID = ? AND o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0 AND o.price > 0
		   AND o.minVolume <= ? AND o.stationID <> ?
		   AND NOT EXISTS (SELECT 1 FROM rolebot_corp_orders r WHERE r.orderID = o.orderID AND r.corporationID = ?)
		 ORDER BY o.price DESC, o.orderID LIMIT ?`, typeID, qty, staging, corpID, gooSaleBidRows)
	if err != nil {
		return nil
	}
	defer rows.Close()
	byStation := map[int64]int{}
	var out []gooBid
	for rows.Next() {
		var b gooBid
		if rows.Scan(&b.StationID, &b.SystemID, &b.Price, &b.Vol) != nil {
			continue
		}
		j, ok := reach[b.SystemID]
		if !ok {
			continue
		}
		if i, seen := byStation[b.StationID]; seen {
			if out[i].Price == b.Price {
				out[i].Vol += b.Vol // more bids at the best price there
			}
			continue
		}
		b.Jumps = j
		byStation[b.StationID] = len(out)
		out = append(out, b)
	}
	return out
}

// officeGoodsUnits is how many units of a type sit in an office's Production
// division.
func (m *Mod) officeGoodsUnits(ctx context.Context, corpID, office, typeID int64) int64 {
	var n int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(quantity), 0) FROM entity
		 WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = ? AND singleton = 0`,
		corpID, office, typeID, corpDivProduct).Scan(&n)
	return n
}

// gooSaleRun decides one type's goods at a staging office: post a sale run to the best bidder
// in reach when it beats listing at listPrice. handled means the goods are a run's (posted
// now, in flight, or waiting for a slot) and must not be listed this pass. open counts the
// corporation's runs in flight; hold is the biggest hauler hold.
func (m *Mod) gooSaleRun(ctx context.Context, c *corpCEO, staging, typeID int64, listPrice float64,
	open *int, hold func() float64) bool {
	var pending int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_requisition
		 WHERE corporationID = ? AND requester = ? AND refKind = ? AND typeID = ? AND srcStationID = ?
		   AND status IN ('open','buying')`, c.CorpID, reqTrade, refGooSale, typeID, staging).Scan(&pending)
	if pending > 0 {
		return true
	}
	office := m.officeAt(ctx, c.CorpID, staging)
	if office == 0 || listPrice <= 0 {
		return false
	}
	qty := m.officeGoodsUnits(ctx, c.CorpID, office, typeID)
	vol := math.Max(m.typeVolume(ctx, typeID), 0.01)
	if h := hold(); h > 0 {
		qty = min(qty, int64(h/vol))
	} else {
		return false // nobody in the corporation flies a hold
	}
	if qty <= 0 {
		return false
	}
	tax := m.marketRates(ctx, c.CharID, staging).Tax
	best, take, gain, ok := pickGooBid(m.gooBidsInReach(ctx, c.CorpID, typeID, staging, qty), qty, vol, tax,
		listPrice, gooSaleMinGainISK)
	if !ok {
		return false
	}
	now := clock.Unix()
	if tradeLaneCooling(c.CorpID, typeID, staging, best.StationID, now) {
		return false // posted lately and the Sales arm never took it up: list it
	}
	if *open >= gooSaleMaxOpen {
		return true // a run would pay: hold the goods for the next slot
	}
	_, wage := gooSaleGain(best.Price, take, vol, best.Jumps, tax, listPrice)
	floor := gooSaleFloor(listPrice, take, wage, tax)
	note := truncate(fmt.Sprintf("tower goods from %s, sell at %s (%d jumps)",
		m.stationLabel(ctx, staging), m.stationLabel(ctx, best.StationID), best.Jumps), 255)
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_requisition
			(corporationID, typeID, qty, destStationID, destFlag, requester, refKind, refID, deadline, budget,
			 openedAt, note, srcStationID, maxPrice, wage)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CorpID, typeID, take, best.StationID, corpDivProduct, reqTrade, refGooSale, best.StationID,
		now+tradeDeskDeadlineSec, wage, now, note, staging, floor, wage)
	if err != nil {
		rlogf("rolebots: corp %d goo sale run: %v", c.CorpID, err)
		return false
	}
	id, _ := res.LastInsertId()
	if err := m.openCommit(ctx, c.CorpID, "req", id, wage); err != nil {
		rlogf("rolebots: corp %d goo sale commit: %v", c.CorpID, err)
	}
	*open++
	m.ceoLog(ctx, c, powerBusiness, "goorun", typeID, best.Price*float64(take),
		"handed the Sales arm a run: %d× %s from its towers at %s to the %s bid at %s — %s over listing at %s after a %s wage",
		take, m.typeNameOr(ctx, typeID), m.stationLabel(ctx, staging), iskString(best.Price),
		m.stationLabel(ctx, best.StationID), iskString(gain), iskString(listPrice), iskString(wage))
	markPosWork(c.CorpID)
	if reqs, err := m.queryReqs(ctx, `WHERE reqID = ?`, id); err == nil && len(reqs) == 1 {
		m.gooSaleAssign(ctx, c, &reqs[0])
	}
	return true
}

// gooSaleAssign hands an open sale run to the Sales arm: the nearest free member to the
// staging office whose hauler holds the whole run, else one that holds a part. The whole
// roster only when the corporation has no Sales arm; a staffed arm with nobody free leaves
// the run waiting.
func (m *Mod) gooSaleAssign(ctx context.Context, c *corpCEO, r *requisition) bool {
	if r.Status != "open" || r.SrcStationID == 0 {
		return false
	}
	vol := math.Max(m.typeVolume(ctx, r.TypeID), 0.01)
	pick := haulerPick{Arm: armSales}
	p := m.pickHauler(ctx, c.CorpID, r.SrcStationID, tradeDeskHaulMaxJumps, 0, float64(r.left())*vol, pick)
	if p.CharID == 0 {
		p = m.pickHauler(ctx, c.CorpID, r.SrcStationID, tradeDeskHaulMaxJumps, 0, vol, pick)
	}
	if p.CharID == 0 {
		return false
	}
	m.assignReqBuyer(ctx, r, p.CharID)
	if r.Status != "buying" {
		return false
	}
	m.ceoLog(ctx, c, powerBusiness, "gooassign", p.CharID, r.Wage,
		"sent %s for the %d× %s run from %s (%d jumps out, %s wage)",
		m.charNameOr(ctx, p.CharID), r.left(), m.typeNameOr(ctx, r.TypeID), m.stationLabel(ctx, r.SrcStationID),
		p.Jumps, iskString(r.Wage))
	return true
}

// posGooTrip takes a buyer holding moon-goods sale runs to the staging office
// to load them: the first run whose office still holds the goods leads, and
// every other of its runs from the same office rides along. handled = the
// trip began.
func (m *Mod) posGooTrip(ctx context.Context, b *botRun, reqs []requisition) (bool, error) {
	var lead *requisition
	for i := range reqs {
		r := &reqs[i]
		if r.RefKind != refGooSale || r.Status != "buying" || r.BuyerID != b.CharID {
			continue
		}
		office := m.officeAt(ctx, b.CorpID, r.SrcStationID)
		if office == 0 || m.officeGoodsUnits(ctx, b.CorpID, office, r.TypeID) == 0 {
			m.closeReq(ctx, r, "void", "the goods are no longer in the staging office")
			continue
		}
		lead = r
		break
	}
	if lead == nil {
		return false, nil
	}
	trip := []int64{}
	for i := range reqs {
		r := &reqs[i]
		if r.RefKind == refGooSale && r.Status == "buying" && r.BuyerID == b.CharID &&
			r.SrcStationID == lead.SrcStationID && len(trip) < reqTripMaxLines {
			trip = append(trip, r.ReqID)
		}
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET tripAt = ? WHERE reqID IN (`+int64CSV(trip)+`)`, clock.Unix())
	st := b.State
	st.Activity = ActIndustry
	st.Data = idleData(st.Data)
	st.Data.PosTripReqs = trip
	st.Data.PosTripStation = lead.SrcStationID
	m.logBot(b.CharID, logInfo, "corp", "collecting its corporation's tower goods at %s to sell",
		m.stationLabel(ctx, lead.SrcStationID))
	return true, m.beginTravel(ctx, b, m.stationSystemID(ctx, lead.SrcStationID), lead.SrcStationID, string(TaskPosBuy))
}

// loadGooLine loads a sale run's goods from the staging office's Production division into
// the carrier's hold as corporation property, as much as room takes. A run whose goods have
// gone shrinks to what was loaded, or is void when nothing was. Returns the units loaded.
func (m *Mod) loadGooLine(ctx context.Context, b *botRun, r *requisition, room float64) int64 {
	office := m.officeAt(ctx, b.CorpID, b.StationID)
	if office == 0 {
		m.closeReq(ctx, r, "void", "the corporation no longer has the staging office")
		return 0
	}
	vol := math.Max(m.typeVolume(ctx, r.TypeID), 0.01)
	qty := r.left()
	if fit := int64(room / vol); fit < qty {
		qty = fit
	}
	if qty <= 0 {
		m.reqDecline(ctx, b.CharID, r, fmt.Sprintf("no room left in its hold: %.2f m³ a unit, %.0f m³ free", vol, room))
		return 0
	}
	moved, err := m.moveUnits(ctx, b.CorpID, office, corpDivProduct, r.TypeID, qty, b.ShipID, 5, b.CorpID)
	if err != nil {
		m.logBot(b.CharID, logError, "corp", "could not load the tower goods (%s): %v", m.typeNameOr(ctx, r.TypeID), err)
		return 0
	}
	if moved < qty && m.officeGoodsUnits(ctx, b.CorpID, office, r.TypeID) == 0 {
		if moved == 0 && r.QtyDone == 0 {
			m.closeReq(ctx, r, "void", "the goods are no longer in the staging office")
			return 0
		}
		// Less there than the run was posted for: the run is what is aboard.
		r.Qty = r.QtyDone + moved
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET qty = ? WHERE reqID = ?`, r.Qty, r.ReqID)
	}
	if moved > 0 {
		m.logBot(b.CharID, logInfo, "corp", "loaded %d× %s of its corporation's tower goods to sell at %s",
			moved, m.typeNameOr(ctx, r.TypeID), m.stationLabel(ctx, r.DestStationID))
	}
	return moved
}
