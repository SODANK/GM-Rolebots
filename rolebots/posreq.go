package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
)

// Requisitions: what a seat wants delivered, and the Procurer who gets it: a type, a quantity,
// an office division, who asked and by when. Requesters: the Fueler (fuel, charters, strontium),
// the CEO (a tower's kit, survey probes), Security (ammunition), the Industry Director
// (reaction inputs). A line runs one of two ways:
//   - patient (deadline more than reqPatientSec away): a corporation buy order at the destination
//     station at the region's best ask; the fill lands in division 1 and the Procurer files it.
//   - urgent (or a patient bid that sat unfilled): a funded buy trip. The Procurer is advanced
//     the money (never past the line's budget), buys at the cheapest landed seller, carries the
//     goods as corporation property, files them and hands back what it did not spend.
// Every line's budget is a rolebot_corp_commit row, so free capital excludes it once posted.

const (
	reqPatientSec    = 3 * 86400
	reqBidStaleSec   = 2 * 86400
	reqStaleSec      = 10 * 86400
	reqBudgetMarkup  = 1.35
	reqAdvanceMarkup = 1.08
	reqBidDays       = 7
	reqTripMaxLines  = 8
	reqBidMinISK     = 100000
	lineProcure      = "procure" // corp ledger line for requisition buys
	// reqBuyerTimeoutSec is how long a buyer may hold a line without working the corporation's
	// board (no trip for it or for any of the corporation's lines) before the desk hands it on.
	reqBuyerTimeoutSec = 45 * 60
	// reqLineStallSec is how long a buying line may go without qtyDone growing before the desk takes
	// it back, whatever other trips its buyer makes (reqLineStalled).
	reqLineStallSec = 6 * 3600
	// reqOverdueGraceSec is how far past its deadline a line under half filled may run before the
	// desk takes it back after reqBuyerTimeoutSec without progress (reqLineStalled).
	reqOverdueGraceSec = 6 * 3600
	// reqDeclineLogSec throttles the "why this buyer did not buy" line per
	// buyer and requisition.
	reqDeclineLogSec = 30 * 60
	// reqNoteNoSeller marks a line nobody anywhere sells: it is held, not
	// handed round, until a seller appears (the zz_20 fuel seed, ice).
	reqNoteNoSeller = "nobody sells it anywhere"
)

// requisition is one rolebot_corp_requisition row.
type requisition struct {
	ReqID, CorpID, TypeID, Qty, QtyDone int64
	DestStationID                       int64
	DestFlag                            int64
	Requester, RefKind                  string
	RefID, Deadline                     int64
	Budget, Spent                       float64
	Status                              string
	BuyerID                             int64
	Advance                             float64
	OrderID                             int64
	OpenedAt                            int64
	Note                                string
	// The Trade desk's lines (corptrade.go) carry the seller the run was priced at and the most the
	// buyer may pay there (the trip is pinned to that seller), and the wage promised the hauler,
	// paid pro rata as cargo lands. 0 on every other line.
	SrcStationID   int64
	MaxPrice       float64
	Wage, WagePaid float64
	// AssignedAt is when the line was last handed to a buyer, TripAt when a
	// buyer last set out on a trip for it.
	AssignedAt, TripAt int64
	// RequesterArm is the arm that asked; when empty, reqRequesterArm reads the requester string.
	// WorkID is the work order a 'building' line is made under (corprequest.go), 0 for a bought line.
	RequesterArm string
	WorkID       int64
	// ProgressAt is when qtyDone last grew; 0 on older rows and new lines, read as OpenedAt.
	// StallFrom is not a column: when the desk last took the line back for a stall (reqStallFrom),
	// so the next buyer gets a full window.
	ProgressAt, StallFrom int64
}

func (r requisition) left() int64 { return max(0, r.Qty-r.QtyDone) }

// reqPatient is whether an open line may be met with a corporation buy order
// at its destination rather than a buy trip. A trade line never is: a bid at
// the sell hub is not a trade run, it is the desk buying at the price it
// meant to sell at. Pure.
func reqPatient(r requisition, now int64) bool {
	return r.Requester != reqTrade && r.Deadline-now > reqPatientSec
}

const reqCols = `reqID, corporationID, typeID, qty, qtyDone, destStationID, destFlag, requester, refKind, refID,
	deadline, budget, spent, status, buyerID, advance, orderID, openedAt, note,
	srcStationID, maxPrice, wage, wagePaid, assignedAt, tripAt, requesterArm, workID, progressAt`

func scanReq(sc interface{ Scan(...any) error }) (requisition, error) {
	var r requisition
	err := sc.Scan(&r.ReqID, &r.CorpID, &r.TypeID, &r.Qty, &r.QtyDone, &r.DestStationID, &r.DestFlag,
		&r.Requester, &r.RefKind, &r.RefID, &r.Deadline, &r.Budget, &r.Spent, &r.Status, &r.BuyerID,
		&r.Advance, &r.OrderID, &r.OpenedAt, &r.Note, &r.SrcStationID, &r.MaxPrice, &r.Wage, &r.WagePaid,
		&r.AssignedAt, &r.TripAt, &r.RequesterArm, &r.WorkID, &r.ProgressAt)
	if v, ok := reqStallFrom.Load(r.ReqID); ok {
		r.StallFrom = v.(int64)
	}
	return r, err
}

func (m *Mod) queryReqs(ctx context.Context, where string, args ...any) ([]requisition, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+reqCols+` FROM rolebot_corp_requisition `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("queryReqs: %w", err)
	}
	defer rows.Close()
	var out []requisition
	for rows.Next() {
		r, err := scanReq(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// reqPending is how much of a type is already on order for one destination
// division: open lines' undelivered quantity.
func (m *Mod) reqPending(ctx context.Context, corpID, typeID, stationID, flag int64) int64 {
	var n sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT SUM(GREATEST(qty - qtyDone, 0)) FROM rolebot_corp_requisition
		 WHERE corporationID = ? AND typeID = ? AND destStationID = ? AND destFlag = ?
		   AND status IN ('open', 'bidding', 'buying', 'building')`, corpID, typeID, stationID, flag).Scan(&n)
	return n.Int64
}

// reqPendingFresh is reqPending counting only lines that are still moving: a line being built, or one
// whose qtyDone grew (or that opened) inside reqLineStallSec. The fuel desk uses it for a dark tower,
// so a stalled line does not stand in for fuel that is not coming (reqVoidStalled).
func (m *Mod) reqPendingFresh(ctx context.Context, corpID, typeID, stationID, flag, now int64) int64 {
	var n sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `
		SELECT SUM(GREATEST(qty - qtyDone, 0)) FROM rolebot_corp_requisition
		 WHERE corporationID = ? AND typeID = ? AND destStationID = ? AND destFlag = ?
		   AND (status = 'building'
		    OR (status IN ('open', 'bidding', 'buying') AND GREATEST(progressAt, openedAt) > ?))`,
		corpID, typeID, stationID, flag, now-reqLineStallSec).Scan(&n)
	return n.Int64
}

// reqVoidStalled closes, as void with why, every line of a type for one destination division that
// reqPendingFresh does not count: open, bidding or buying with no progress inside reqLineStallSec.
// Any advance goes back to the corporation. Returns how many lines it closed.
func (m *Mod) reqVoidStalled(ctx context.Context, corpID, typeID, stationID, flag, now int64, why string) int {
	reqs, err := m.queryReqs(ctx, `
		WHERE corporationID = ? AND typeID = ? AND destStationID = ? AND destFlag = ?
		  AND status IN ('open', 'bidding', 'buying') AND GREATEST(progressAt, openedAt) <= ?`,
		corpID, typeID, stationID, flag, now-reqLineStallSec)
	if err != nil {
		return 0
	}
	for i := range reqs {
		if reqs[i].BuyerID != 0 {
			reqVoidSnubs.Store(reqVoidKey{corpID, typeID, stationID, flag}, reqVoidSnub{Buyer: reqs[i].BuyerID, At: now})
		}
		if reqs[i].Advance > 0 && reqs[i].BuyerID != 0 {
			m.reclaimAdvance(ctx, &reqs[i])
		}
		m.closeReq(ctx, &reqs[i], "void", why)
	}
	return len(reqs)
}

// postRequisition asks for qty more of a type at a division. A line already
// open for the same type, destination and reference is grown instead of
// doubled. Returns the line's ID.
func (m *Mod) postRequisition(ctx context.Context, c *corpCEO, r requisition) (int64, error) {
	if r.Qty <= 0 || r.TypeID == 0 || r.DestStationID == 0 {
		return 0, nil
	}
	unit := evedb.TypeValueOf(ctx, m.db, r.TypeID).Value
	if unit <= 0 {
		unit = 1
	}
	if r.Deadline == 0 {
		r.Deadline = clock.Unix() + reqPatientSec + 86400
	}
	markup := reqBudgetMarkup
	var existing int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT reqID FROM rolebot_corp_requisition
		 WHERE corporationID = ? AND typeID = ? AND destStationID = ? AND destFlag = ? AND refKind = ? AND refID = ?
		   AND status IN ('open', 'bidding', 'buying', 'building')
		 ORDER BY reqID LIMIT 1`, c.CorpID, r.TypeID, r.DestStationID, r.DestFlag, r.RefKind, r.RefID).Scan(&existing)
	if existing != 0 {
		_, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_requisition SET qty = qty + ?, deadline = LEAST(deadline, ?) WHERE reqID = ?`,
			r.Qty, r.Deadline, existing)
		if err == nil {
			var qty int64
			_ = m.db.QueryRowContext(ctx, `SELECT qty - qtyDone FROM rolebot_corp_requisition WHERE reqID = ?`, existing).Scan(&qty)
			commit := math.Ceil(float64(qty) * unit * markup)
			_ = m.openCommit(ctx, c.CorpID, "req", existing, commit)
			// The budget grows with the line, so a line grown by a second request does not run over
			// budget and get voided after reqStaleSec.
			_, _ = m.db.ExecContext(ctx, `
				UPDATE rolebot_corp_requisition SET budget = GREATEST(budget, spent + ?) WHERE reqID = ?`, commit, existing)
		}
		return existing, err
	}
	budget := math.Ceil(float64(r.Qty) * unit * markup)
	// The asking arm pays out of its own envelope (corpenvelope.go). A line it cannot afford this
	// week is not opened; the desk asks again next pass.
	if why := m.envelopeRefusal(ctx, c.CorpID, reqRequesterArm(r), budget); why != "" {
		if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "overbudget") >= 3600 {
			m.ceoLog(ctx, c, powerBusiness, "overbudget", r.TypeID, budget,
				"held %d× %s for %s (%s): %s", r.Qty, m.typeNameOr(ctx, r.TypeID),
				armLabel(reqRequesterArm(r)), iskString(budget), why)
		}
		return 0, nil
	}
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_requisition
			(corporationID, typeID, qty, destStationID, destFlag, requester, refKind, refID, deadline, budget, openedAt, note,
			 requesterArm)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CorpID, r.TypeID, r.Qty, r.DestStationID, r.DestFlag, r.Requester, r.RefKind, r.RefID, r.Deadline,
		budget, clock.Unix(), truncate(r.Note, 255), string(reqRequesterArm(r)))
	if err != nil {
		return 0, fmt.Errorf("postRequisition: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := m.openCommit(ctx, c.CorpID, "req", id, budget); err != nil {
		return id, err
	}
	m.ceoLog(ctx, c, powerStarbase, "requisition", r.TypeID, budget,
		"requisitioned %d× %s for %s (%s, up to %s)", r.Qty, m.typeNameOr(ctx, r.TypeID),
		m.stationLabel(ctx, r.DestStationID), r.Requester, iskString(budget))
	markPosWork(c.CorpID)
	return id, nil
}

// closeReq ends a line, releasing its commitment.
func (m *Mod) closeReq(ctx context.Context, r *requisition, status, why string) {
	if r.OrderID != 0 {
		m.cancelReqBid(ctx, r)
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_requisition SET status = ?, closedAt = ?, buyerID = 0, note = ? WHERE reqID = ?`,
		status, clock.Unix(), truncate(why, 255), r.ReqID)
	m.closeCommit(ctx, "req", r.ReqID)
	// A line being built takes its work order with it (corprequest.go).
	if r.WorkID != 0 && status != "done" {
		m.closeWork(ctx, r.WorkID, workVoid)
	}
	r.Status = status
}

// voidReqsFor closes every open line raised for a reference (a tower plan
// abandoned, a survey cancelled).
func (m *Mod) voidReqsFor(ctx context.Context, corpID int64, refKind string, refID int64, why string) {
	reqs, err := m.queryReqs(ctx, `WHERE corporationID = ? AND refKind = ? AND refID = ? AND status IN ('open','bidding','buying','building')`,
		corpID, refKind, refID)
	if err != nil {
		return
	}
	for i := range reqs {
		if reqs[i].Advance > 0 && reqs[i].BuyerID != 0 {
			m.reclaimAdvance(ctx, &reqs[i])
		}
		m.closeReq(ctx, &reqs[i], "void", why)
	}
}

// ── the Procurer's desk ─────────────────────────────────────────────────────

// procurerDesk runs every open line: books bid fills, files them, places
// patient bids, and hands urgent lines (and stale bids) to a buyer.
func (m *Mod) procurerDesk(ctx context.Context, c *corpCEO, ch Charter, buyer int64) error {
	return m.procurerDeskLines(ctx, c, ch, buyer, false)
}

// procurerDeskLines is procurerDesk, optionally leaving towers' lines alone:
// a corporation without the starbase power still buys its other lines
// (starbaseDesks).
func (m *Mod) procurerDeskLines(ctx context.Context, c *corpCEO, ch Charter, buyer int64, skipTower bool) error {
	reqs, err := m.queryReqs(ctx, `WHERE corporationID = ? AND status IN ('open','bidding','buying') ORDER BY deadline, reqID`,
		c.CorpID)
	if err != nil {
		return err
	}
	now := clock.Unix()
	free, _, _ := m.corpFreeCapital(ctx, c.CorpID, ch)
	_ = free
	// What each buyer is doing, read once per pass for the timeout.
	work := map[int64]reqBuyerWork{}
	workOf := func(buyer int64) reqBuyerWork {
		if w, ok := work[buyer]; ok {
			return w
		}
		w := m.reqBuyerWorkOf(ctx, buyer, c.CorpID)
		work[buyer] = w
		return w
	}
	// A hand-out off the seat goes to one free hauler per pass (pickFreeHauler passes over anyone
	// already holding a line), so lines for one office ride with one hauler, up to a trip's worth.
	handed := map[int64]int64{} // destStationID to this pass's off-seat buyer
	handedN := map[int64]int{}
	holds := m.newReqHoldBook(c.CorpID)
	// Lines being built by Industry (corprequest.go): done, or back to buying.
	m.sweepBuildingReqs(ctx, c)
	for i := range reqs {
		r := &reqs[i]
		if ctx.Err() != nil {
			return nil
		}
		if r.Requester == reqTrade {
			continue // the Trade desk sweeps and staffs its own lines (corptrade.go)
		}
		if skipTower && r.RefKind == "tower" {
			continue // the desk runs without the starbase power only for other lines
		}
		if r.Status == "bidding" {
			m.sweepReqBid(ctx, c, r)
		}
		if r.left() <= 0 {
			m.closeReq(ctx, r, "done", "delivered")
			continue
		}
		if now-r.OpenedAt > reqStaleSec {
			if r.Advance > 0 {
				m.reclaimAdvance(ctx, r)
			}
			m.closeReq(ctx, r, "void", "never filled")
			continue
		}
		switch r.Status {
		case "open":
			// Make it rather than buy it, when the library can and it is
			// cheaper (corprequest.go). The line then waits on its work order.
			if m.routeReqToBuild(ctx, c, ch, r) {
				continue
			}
			if reqPatient(*r, now) {
				m.placeReqBid(ctx, c, r)
			} else if !m.typeSoldAnywhere(ctx, r.TypeID) {
				// No buyer can buy what nobody sells: held with a note until
				// a seller appears, rather than handed round forever.
				m.holdReq(ctx, r, reqNoteNoSeller)
			} else if note := holds.tooBigNote(ctx, r.TypeID, buyer); note != "" {
				// Nor what no hauler in the corporation can lift: held until
				// somebody owns a hull that takes one unit.
				m.holdReq(ctx, r, note)
			} else if b := handed[r.DestStationID]; b != 0 && handedN[b] < reqTripMaxLines &&
				reqSnubOf(r, now) != b && holds.lineFits(ctx, b, r) {
				m.assignReqBuyer(ctx, r, b)
				handedN[b]++
			} else if pick := m.pickReqBuyer(ctx, c, r, buyer, holds); pick != 0 {
				m.assignReqBuyer(ctx, r, pick)
				if pick != buyer {
					handed[r.DestStationID] = pick
					handedN[pick]++
				}
			} else if need := holds.unitM3(ctx, r.TypeID); !holds.lineFits(ctx, buyer, r) && need > 0 {
				// Somebody could carry it, nobody who can is free: say so,
				// rather than leave the last refusal on the line.
				if holds.wantsCargoHull(ctx, r) {
					m.holdReq(ctx, r, reqWaitCargoHullNote)
				} else {
					m.holdReq(ctx, r, reqWaitNote(need))
				}
			}
		case "bidding":
			stale := now-r.OpenedAt > reqBidStaleSec || r.Deadline-now < reqPatientSec/2
			if stale {
				if pick := m.pickReqBuyer(ctx, c, r, buyer, holds); pick != 0 {
					m.cancelReqBid(ctx, r)
					m.assignReqBuyer(ctx, r, pick)
				}
			}
		case "buying":
			// A buyer that left the corporation or the roster loses the line.
			var corpNow int64
			_ = m.db.QueryRowContext(ctx, `SELECT corporationID FROM character_ WHERE characterID = ?`, r.BuyerID).Scan(&corpNow)
			if corpNow != c.CorpID || !m.isRostered(ctx, r.BuyerID) {
				m.reclaimAdvance(ctx, r)
				_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ?`, r.ReqID)
				continue
			}
			// A buyer that is logged off, benched or winding down to log off will not set out
			// again soon: the line goes back on the board now. Only a trip already under way keeps it.
			if away := reqBuyerAway(r.BuyerID); away != "" && !workOf(r.BuyerID).OnTrip {
				m.releaseReqBuyer(ctx, c, r, away)
				continue
			}
			// A line that stopped filling goes back on the board, however busy its buyer is.
			if reqLineStalled(*r, now) {
				m.releaseStalledReq(ctx, c, r, now)
				continue
			}
			m.timeOutReqBuyer(ctx, c, r, now, workOf(r.BuyerID))
		}
	}
	return nil
}

// reqAskNear is the best ask for a type in the destination's region, the
// price a patient bid offers.
func (m *Mod) reqAskNear(ctx context.Context, typeID, stationID int64) float64 {
	regionID, _ := evedb.GetRegionForStation(ctx, m.db, stationID)
	return m.reqAskIn(ctx, typeID, regionID, 0)
}

// reqBidPrice is what a patient bid for qty units offers: what buying them off the destination
// region's book would cost a unit (depthVWAP), not the cheapest single ask, which a bid would
// fill from nobody. Falls back to reqAskNear.
func (m *Mod) reqBidPrice(ctx context.Context, typeID, stationID, qty int64) float64 {
	regionID, _ := evedb.GetRegionForStation(ctx, m.db, stationID)
	if p := m.reqAskIn(ctx, typeID, regionID, qty); p > 0 {
		return p
	}
	return m.reqAskNear(ctx, typeID, stationID)
}

// reqAskIn is the region's ask for a type: the depth-weighted price of the
// cheapest qty units, or with qty 0 the cheapest ask; TypeValue when the
// region sells none.
func (m *Mod) reqAskIn(ctx context.Context, typeID, regionID, qty int64) float64 {
	if qty > 0 {
		rows, err := m.db.QueryContext(ctx, `
			SELECT price, SUM(volRemaining) FROM market_orders
			 WHERE regionID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0
			 GROUP BY price ORDER BY price LIMIT 200`, regionID, typeID)
		if err == nil {
			var levels []sweepLevel
			for rows.Next() {
				l := sweepLevel{TypeID: typeID}
				if rows.Scan(&l.Price, &l.Vol) == nil {
					levels = append(levels, l)
				}
			}
			rows.Close()
			if p := depthVWAP(levels, typeID, qty); p > 0 {
				return p
			}
		}
	}
	var p sql.NullFloat64
	_ = m.db.QueryRowContext(ctx, `
		SELECT MIN(price) FROM market_orders
		 WHERE regionID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0`,
		regionID, typeID).Scan(&p)
	if p.Valid && p.Float64 > 0 {
		return p.Float64
	}
	return evedb.TypeValueOf(ctx, m.db, typeID).Value
}

// placeReqBid puts a patient line on the market at its destination.
func (m *Mod) placeReqBid(ctx context.Context, c *corpCEO, r *requisition) {
	if m.officeAt(ctx, c.CorpID, r.DestStationID) == 0 {
		return // a corporation bid needs an office to deliver to
	}
	price := roundBidPrice(m.reqBidPrice(ctx, r.TypeID, r.DestStationID, r.left()))
	qty := r.left()
	if price <= 0 || qty <= 0 || price*float64(qty) < reqBidMinISK/10 {
		return
	}
	if cap := r.Budget - r.Spent; price*float64(qty) > cap && cap > 0 {
		qty = int64(cap / price)
	}
	if qty <= 0 {
		return
	}
	placer := m.corpOrderPlacer(ctx, c.CorpID, c.CharID)
	if placer == 0 {
		return
	}
	out, err := evedb.PlaceBuyOrder(ctx, m.db, placer, r.DestStationID, r.TypeID, price, qty, -1, 1, reqBidDays, true)
	if err != nil || out == nil || out.ErrorMsg != "" {
		return
	}
	filled := out.FilledQty
	if filled > 0 {
		cost := price * float64(filled)
		m.fileReqFill(ctx, c, r, filled, cost)
	}
	if out.RestingOrderID != 0 {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_requisition SET status = 'bidding', orderID = ?, buyerID = ? WHERE reqID = ?`,
			out.RestingOrderID, placer, r.ReqID)
		_, _ = m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_orders (orderID, corporationID, stationID, typeID, bid, price, volSeen, placedAt, placedBy)
			VALUES (?, ?, ?, ?, 2, ?, ?, ?, ?)`,
			out.RestingOrderID, c.CorpID, r.DestStationID, r.TypeID, price, qty-filled, clock.Unix(), placer)
		r.Status, r.OrderID, r.BuyerID = "bidding", out.RestingOrderID, placer
		m.ceoLog(ctx, c, powerStarbase, "bid", r.TypeID, price*float64(qty),
			"bid for %d× %s at %s, %s each (%s)", qty, m.typeNameOr(ctx, r.TypeID),
			m.stationLabel(ctx, r.DestStationID), iskString(price), r.Requester)
	}
}

// sweepReqBid books what a line's bid has bought since the last look.
func (m *Mod) sweepReqBid(ctx context.Context, c *corpCEO, r *requisition) {
	if r.OrderID == 0 {
		return
	}
	var seen, rem, state int64
	var price float64
	err := m.db.QueryRowContext(ctx, `
		SELECT co.volSeen, COALESCE(o.volRemaining, -1), COALESCE(o.orderState, 0), co.price
		  FROM rolebot_corp_orders co LEFT JOIN market_orders o ON o.orderID = co.orderID
		 WHERE co.orderID = ?`, r.OrderID).Scan(&seen, &rem, &state, &price)
	if err != nil {
		r.OrderID = 0
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', orderID = 0 WHERE reqID = ?`, r.ReqID)
		r.Status = "open"
		return
	}
	// A bid the book no longer has was cancelled or expired (ExpireMarketOrders deletes the
	// row and refunds the escrow): nothing more was bought since the last look. Filled corp
	// bids stay on the book until this sweep forgets them (pruneClosedBotOrders skips them).
	if rem < 0 {
		m.forgetCorpOrder(ctx, r.OrderID)
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', orderID = 0, buyerID = 0 WHERE reqID = ?`, r.ReqID)
		r.Status, r.OrderID = "open", 0
		return
	}
	if bought := seen - rem; bought > 0 {
		m.fileReqFill(ctx, c, r, bought, price*float64(bought))
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_orders SET volSeen = ? WHERE orderID = ?`, rem, r.OrderID)
	}
	if rem <= 0 || state != 1 {
		m.forgetCorpOrder(ctx, r.OrderID)
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', orderID = 0, buyerID = 0 WHERE reqID = ?`, r.ReqID)
		r.Status, r.OrderID = "open", 0
	}
}

// fileReqFill moves a bid's fill from division 1 into the line's division and
// books it.
func (m *Mod) fileReqFill(ctx context.Context, c *corpCEO, r *requisition, qty int64, cost float64) {
	office := m.officeAt(ctx, c.CorpID, r.DestStationID)
	if office != 0 && r.DestFlag != corpDivMaterials {
		_, _ = m.moveUnits(ctx, c.CorpID, office, corpDivMaterials, r.TypeID, qty, office, r.DestFlag, c.CorpID)
	}
	r.QtyDone += qty
	r.Spent += cost
	now := clock.Unix()
	if qty > 0 {
		r.ProgressAt = now
	}
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_requisition SET qtyDone = qtyDone + ?, spent = spent + ?,
		       progressAt = IF(? > 0, ?, progressAt) WHERE reqID = ?`, qty, cost, qty, now, r.ReqID)
	m.spendCommit(ctx, "req", r.ReqID, cost)
	m.corpBook(ctx, c.CorpID, reqLedgerLine(r), -cost, r.TypeID, fmt.Sprintf("%d× %s (%s)", qty, m.typeNameOr(ctx, r.TypeID), r.Requester))
	m.towerBookReq(ctx, r, cost, qty)
}

// reqLedgerLine is the ledger line a requisition's purchases are booked to: a
// starbase's structures are capex, the trade desk's buys are tradebuy, the
// rest (fuel, materials, supplies) procure. Pure.
func reqLedgerLine(r *requisition) string {
	switch {
	case r.RefKind == "tower" && r.DestFlag == corpDivStarbase:
		return lineCapex
	case r.Requester == reqTrade:
		return lineTradeBuy
	}
	return lineProcure
}

func (m *Mod) cancelReqBid(ctx context.Context, r *requisition) {
	if r.OrderID == 0 {
		return
	}
	var placer int64
	_ = m.db.QueryRowContext(ctx, `SELECT charID FROM market_orders WHERE orderID = ?`, r.OrderID).Scan(&placer)
	if placer != 0 {
		if _, err := evedb.CancelMarketOrder(ctx, m.db, placer, r.OrderID); err != nil {
			rlogf("rolebots: cancel requisition bid %d: %v", r.OrderID, err)
		}
	}
	m.forgetCorpOrder(ctx, r.OrderID)
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', orderID = 0, buyerID = 0 WHERE reqID = ?`, r.ReqID)
	r.OrderID, r.Status, r.BuyerID = 0, "open", 0
}

func (m *Mod) assignReqBuyer(ctx context.Context, r *requisition, buyer int64) {
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_requisition SET status = 'buying', buyerID = ?, assignedAt = ? WHERE reqID = ? AND status = 'open'`,
		buyer, now, r.ReqID); err == nil {
		r.Status, r.BuyerID, r.AssignedAt = "buying", buyer, now
		markPosWork(r.CorpID)
	}
}

// reqSnubs remembers, per requisition, the last buyer that held it and never
// set out, so the next hand-out goes to somebody else.
var reqSnubs sync.Map // reqID to charID

// pickReqBuyer is who buys an urgent line: the Procurement Officer's executor (seat), unless
// that pilot just sat on this very line or its hauler cannot hold one unit of it; otherwise the
// nearest free hauler of the Sales arm to the line's destination whose hold takes a unit, and
// failing that the nearest such hauler in the corporation. 0 when nobody free can carry it.
func (m *Mod) pickReqBuyer(ctx context.Context, c *corpCEO, r *requisition, seat int64, h *reqHoldBook) int64 {
	snubbed := reqSnubOf(r, clock.Unix())
	if seat != 0 && seat != snubbed && reqBuyerAway(seat) == "" && h.lineFits(ctx, seat, r) {
		return seat
	}
	id := m.pickReqBuyerOffSeat(ctx, c, r, snubbed, h)
	if id == 0 && reqUrgent(*r, clock.Unix()) {
		// Urgent and nobody near is free: anyone free in the corporation, then the CEO.
		if snubbed == 0 {
			id = m.pickHauler(ctx, c.CorpID, r.DestStationID, unlimitedJumps, 0, h.unitM3(ctx, r.TypeID),
				haulerPick{CargoHull: h.wantsCargoHull(ctx, r)}).CharID
		}
		if id == 0 && c.CharID != seat && c.CharID != snubbed && reqBuyerAway(c.CharID) == "" &&
			!m.holdsBuyingLine(ctx, c.CharID) && h.lineFits(ctx, c.CharID, r) {
			id = c.CharID
		}
	}
	return id
}

// pickReqBuyerOffSeat is pickReqBuyer past the seat: Sales, then Infrastructure, then anyone near.
func (m *Mod) pickReqBuyerOffSeat(ctx context.Context, c *corpCEO, r *requisition, snubbed int64, h *reqHoldBook) int64 {
	need := h.unitM3(ctx, r.TypeID)
	cargo := h.wantsCargoHull(ctx, r) // bulk tower fuel goes only to a pilot whose trade hull is a cargo hull
	sales := m.pickHauler(ctx, c.CorpID, r.DestStationID, tradeDeskHaulMaxJumps, snubbed, need,
		haulerPick{Arm: armSales, CargoHull: cargo})
	id, arm := sales.CharID, sales.ArmStaffed
	if id == 0 && (sales.HoldShort || !arm) {
		// Too big for Sales' hulls: Infrastructure is staffed by what its members can carry
		// (corparm_assign.go infraByLift), so heavy pieces go to its biggest hold first.
		id = m.pickHauler(ctx, c.CorpID, r.DestStationID, tradeDeskHaulMaxJumps, snubbed, need,
			haulerPick{Arm: armInfrastructure, ByHold: true, CargoHull: cargo}).CharID
	}
	if id == 0 && arm {
		id = m.pickHauler(ctx, c.CorpID, r.DestStationID, tradeDeskHaulMaxJumps, snubbed, need,
			haulerPick{CargoHull: cargo}).CharID
	}
	if id == 0 && snubbed != 0 {
		// The seat turned this very line down: anyone free in the corporation takes it, however far
		// from the office (a tower staged far from home has no member within tradeDeskHaulMaxJumps).
		id = m.pickHauler(ctx, c.CorpID, r.DestStationID, unlimitedJumps, snubbed, need,
			haulerPick{CargoHull: cargo}).CharID
	}
	return id
}

func reqSnubbedBy(reqID, buyer int64) bool {
	v, ok := reqSnubs.Load(reqID)
	return ok && v.(int64) == buyer
}

// reqVoidSnubs remembers who sat on a line the fuel desk voided, so the line posted in its place
// goes to somebody else for a day.
var reqVoidSnubs sync.Map // reqVoidKey to reqVoidSnub

type reqVoidKey struct{ CorpID, TypeID, StationID, Flag int64 }

type reqVoidSnub struct{ Buyer, At int64 }

const reqVoidSnubSec = 86400

// reqSnubOf is the buyer a line should pass over: the last one that sat on it, or on the line it replaced.
func reqSnubOf(r *requisition, now int64) int64 {
	if v, ok := reqSnubs.Load(r.ReqID); ok {
		return v.(int64)
	}
	if v, ok := reqVoidSnubs.Load(reqVoidKey{r.CorpID, r.TypeID, r.DestStationID, r.DestFlag}); ok {
		if s := v.(reqVoidSnub); now-s.At < reqVoidSnubSec {
			return s.Buyer
		}
	}
	return 0
}

// reqUrgent is whether a line is due within a day. Pure.
func reqUrgent(r requisition, now int64) bool {
	return r.Deadline > 0 && r.Deadline-now <= 86400
}

// holdsBuyingLine is whether a pilot is already buying for some line.
func (m *Mod) holdsBuyingLine(ctx context.Context, charID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_corp_requisition WHERE buyerID = ? AND status = 'buying'`, charID).Scan(&n)
	return n > 0
}

// reqBuyerWork is what a buyer is doing for its corporation's board: on a buy
// trip right now, and when it last set out for any of the corporation's lines.
type reqBuyerWork struct {
	OnTrip   bool
	LastTrip int64
}

// reqBuyerWorkOf reads a buyer's work for one corporation: the trip in its
// state (posBuyTrip writes the lines, the trip's end clears them), and the
// latest tripAt on any line of the corporation it has bought for.
func (m *Mod) reqBuyerWorkOf(ctx context.Context, buyer, corpID int64) reqBuyerWork {
	var w reqBuyerWork
	if st, err := loadState(ctx, m.db, buyer); err == nil && st != nil && len(st.Data.PosTripReqs) > 0 {
		// On a trip only while it still holds a line on it.
		var n int
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_corp_requisition
			 WHERE reqID IN (`+int64CSV(st.Data.PosTripReqs)+`) AND status = 'buying' AND buyerID = ?`,
			buyer).Scan(&n)
		w.OnTrip = n > 0
	}
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(tripAt), 0) FROM rolebot_corp_requisition WHERE buyerID = ? AND corporationID = ?`,
		buyer, corpID).Scan(&w.LastTrip)
	return w
}

// reqBuyerSatOn is the timeout's verdict on one line its buyer holds: reqBuyerTimeoutSec since
// the line was handed over, since the last trip for it, or since the last trip for any of the
// corporation's lines, whichever is latest, with no trip under way. A trip that came back without
// filling the line does not keep it forever. Pure.
func reqBuyerSatOn(assignedAt, tripAt int64, w reqBuyerWork, now int64) bool {
	if w.OnTrip {
		return false
	}
	return now-max(assignedAt, tripAt, w.LastTrip) >= reqBuyerTimeoutSec
}

// reqBuyerAway is why a buyer cannot work the board soon: logged off for its hours, benched by
// the population cap, or in its wind-down before logging off. "" when it is about.
func reqBuyerAway(charID int64) string {
	switch {
	case tzIsOff(charID):
		return "logged off"
	case popIsOffline(charID):
		return "benched"
	case tzWindingDown(charID):
		return "winding down to log off"
	}
	return ""
}

// carrierAway is reqBuyerAway for a pilot already on an errand: only logged off or benched. A
// pilot winding down finishes the errand it is on, it only starts nothing new.
func carrierAway(charID int64) string {
	switch {
	case tzIsOff(charID):
		return "logged off"
	case popIsOffline(charID):
		return "benched"
	}
	return ""
}

// releaseReqBuyer puts a buying line back on the board, reclaiming any advance, and passes the
// buyer over at the next hand-out.
func (m *Mod) releaseReqBuyer(ctx context.Context, c *corpCEO, r *requisition, why string) {
	who := m.charNameOr(ctx, r.BuyerID)
	reqSnubs.Store(r.ReqID, r.BuyerID)
	m.reclaimAdvance(ctx, r)
	note := truncate(fmt.Sprintf("%s held it but is %s", who, why), 255)
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0, note = ? WHERE reqID = ? AND status = 'buying'`,
		note, r.ReqID)
	m.logBot(c.CharID, logInfo, "ceo", "took the %d× %s line back from %s — %s",
		r.left(), m.typeNameOr(ctx, r.TypeID), who, why)
	r.Status, r.BuyerID = "open", 0
}

// timeOutReqBuyer takes a line back from a buyer that has held it reqBuyerTimeoutSec without
// working the corporation's board, reclaims any advance, and remembers the buyer so the next
// hand-out passes it over. A line with no assignedAt, or whose buyer is out on a trip for the
// corporation, starts its clock now: the clock runs only while the buyer is idle.
func (m *Mod) timeOutReqBuyer(ctx context.Context, c *corpCEO, r *requisition, now int64, w reqBuyerWork) {
	if r.Requester == reqTrade {
		return
	}
	if r.AssignedAt == 0 || (w.OnTrip && r.TripAt < r.AssignedAt) {
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET assignedAt = ? WHERE reqID = ?`, now, r.ReqID)
		r.AssignedAt = now
		return
	}
	if !reqBuyerSatOn(r.AssignedAt, r.TripAt, w, now) {
		return
	}
	who := m.charNameOr(ctx, r.BuyerID)
	reqSnubs.Store(r.ReqID, r.BuyerID)
	m.reclaimAdvance(ctx, r)
	idle := (now - max(r.AssignedAt, r.TripAt, w.LastTrip)) / 60
	note := truncate(fmt.Sprintf("%s held it %d min and never set out", who, idle), 255)
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0, note = ? WHERE reqID = ? AND status = 'buying'`,
		note, r.ReqID)
	m.logBot(c.CharID, logInfo, "ceo", "took the %d× %s line back from %s — %d min without setting out for the corporation",
		r.left(), m.typeNameOr(ctx, r.TypeID), who, idle)
	r.Status, r.BuyerID = "open", 0
}

// reqStallFrom remembers, per requisition, when the desk last took it back for a stall, so the next
// buyer gets a full window. A restart forgets it: a stalled line is taken back once more at boot.
var reqStallFrom sync.Map // reqID to unix time

// reqLineStalled is whether the desk takes a buying line back: reqLineStallSec since the latest of
// its last progress, its opening and its last stall release, whatever other trips the buyer makes,
// or only reqBuyerTimeoutSec once it is reqOverdueGraceSec past its deadline and under half filled.
// A trade desk line (corptrade.go) never stalls here. Pure.
func reqLineStalled(r requisition, now int64) bool {
	if r.Status != "buying" || r.Requester == reqTrade || r.left() <= 0 {
		return false
	}
	idle := now - max(r.ProgressAt, r.OpenedAt, r.StallFrom)
	if idle >= reqLineStallSec {
		return true
	}
	overdue := r.Deadline > 0 && now-r.Deadline >= reqOverdueGraceSec && r.QtyDone*2 < r.Qty
	return overdue && idle >= reqBuyerTimeoutSec
}

// releaseStalledReq puts a stalled line back on the board (reqLineStalled): the advance reclaimed,
// the buyer passed over at the next hand-out, the stall clock restarted for the next buyer.
func (m *Mod) releaseStalledReq(ctx context.Context, c *corpCEO, r *requisition, now int64) {
	who := m.charNameOr(ctx, r.BuyerID)
	idle := fmtDuration(now - max(r.ProgressAt, r.OpenedAt, r.StallFrom))
	reqSnubs.Store(r.ReqID, r.BuyerID)
	reqStallFrom.Store(r.ReqID, now)
	m.reclaimAdvance(ctx, r)
	note := truncate(fmt.Sprintf("%s held it with no progress in %s", who, idle), 255)
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0, note = ? WHERE reqID = ? AND status = 'buying'`,
		note, r.ReqID)
	m.logBot(c.CharID, logInfo, "ceo", "took the %d× %s line back from %s — no progress in %s",
		r.left(), m.typeNameOr(ctx, r.TypeID), who, idle)
	r.Status, r.BuyerID, r.StallFrom = "open", 0, now
}

// holdReq parks an open line with a note, written only when it changes.
func (m *Mod) holdReq(ctx context.Context, r *requisition, note string) {
	if r.Note == note {
		return
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET note = ? WHERE reqID = ?`, note, r.ReqID)
	r.Note = note
}

// typeSoldAnywhere is whether any station in the game has an ask for a type.
func (m *Mod) typeSoldAnywhere(ctx context.Context, typeID int64) bool {
	var one int
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM market_orders WHERE typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0 LIMIT 1`,
		typeID).Scan(&one)
	return err == nil
}

// reqDeclineAt throttles reqDecline per buyer and line.
var reqDeclineAt sync.Map // [2]int64{charID, reqID} to unix time

var reqSnubsClearedAt atomic.Int64

func init() {
	// These grow per requisition and are never deleted otherwise: the decline stamps lapse after a
	// day, the stall stamps after reqLineStallSec, and the snub memory (a soft preference) is
	// dropped whole once a day.
	registerSweep("reqSnubsSweep", func() {
		now := clock.Unix()
		reqDeclineAt.Range(func(k, v any) bool {
			if now-v.(int64) >= 86400 {
				reqDeclineAt.Delete(k)
			}
			return true
		})
		// A stall stamp past reqLineStallSec no longer moves the clock (reqLineStalled).
		reqStallFrom.Range(func(k, v any) bool {
			if now-v.(int64) >= reqLineStallSec {
				reqStallFrom.Delete(k)
			}
			return true
		})
		if now-reqSnubsClearedAt.Load() >= 86400 {
			reqSnubsClearedAt.Store(now)
			reqSnubs.Range(func(k, _ any) bool { reqSnubs.Delete(k); return true })
		}
	})
	registerCharForget("reqSnubs/reqDeclineAt", func(charID int64) {
		reqSnubs.Range(func(k, v any) bool {
			if v.(int64) == charID {
				reqSnubs.Delete(k)
			}
			return true
		})
		reqDeclineAt.Range(func(k, _ any) bool {
			if k.([2]int64)[0] == charID {
				reqDeclineAt.Delete(k)
			}
			return true
		})
	})
}

// reqDecline says why a buyer holding a line did not set out for it: once
// per reqDeclineLogSec in the buyer's log, and on the line's note so the
// starbase page shows it.
func (m *Mod) reqDecline(ctx context.Context, buyer int64, r *requisition, why string) {
	key := [2]int64{buyer, r.ReqID}
	now := clock.Unix()
	if v, ok := reqDeclineAt.Load(key); ok && now-v.(int64) < reqDeclineLogSec {
		return
	}
	reqDeclineAt.Store(key, now)
	m.logBot(buyer, logInfo, "corp", "not buying the %d× %s requisition: %s", r.left(), m.typeNameOr(ctx, r.TypeID), why)
	m.holdReq(ctx, r, truncate(why, 255))
}

// reclaimAdvance takes an unspent advance back from a buyer, writing off
// what it no longer holds.
func (m *Mod) reclaimAdvance(ctx context.Context, r *requisition) {
	left := r.Advance - r.Spent
	if left < 1 || r.BuyerID == 0 {
		return
	}
	take := left
	if bal, err := evedb.GetCharBalance(ctx, m.db, r.BuyerID); err == nil {
		take = math.Min(take, bal)
	}
	if take >= 1 {
		if _, _, err := evedb.TransferISK(ctx, m.db, r.BuyerID, r.CorpID, math.Floor(take), refPlayerDonation,
			"Unspent requisition advance returned"); err != nil {
			take = 0
		}
	}
	if take < left-1 {
		m.corpBook(ctx, r.CorpID, lineProcure, -(left - take), r.BuyerID, "requisition advance written off")
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET advance = spent WHERE reqID = ?`, r.ReqID)
	r.Advance = r.Spent
}

// ── the buy trip ────────────────────────────────────────────────────────────

// posBuyDue takes a buyer with 'buying' lines on a trip: the earliest
// deadline first, plus every other line of its own the same seller stocks.
func (m *Mod) posBuyDue(ctx context.Context, b *botRun) (bool, error) {
	if b.StationID == 0 {
		return false, nil
	}
	reqs, err := m.queryReqs(ctx, `WHERE buyerID = ? AND corporationID = ? AND status = 'buying' ORDER BY deadline, reqID LIMIT 20`,
		b.CharID, b.CorpID)
	if err != nil || len(reqs) == 0 {
		return false, err
	}
	// A line one unit of which will not go into this buyer's hauler goes back on the board before
	// any trip is planned: the hand-out checks the hold, but the buyer may have changed hulls.
	hold := math.Max(m.reqHaulerHold(ctx, b.CharID), m.shipCargoCapacity(ctx, b))
	// Bulk tower fuel is bought in a cargo hull or not at all. The pilot boards its trade hull
	// here when it is parked here (boardHauler); a pilot that is still in a frigate, or whose
	// industrial is docked elsewhere, hands the line back rather than make a trip a few percent
	// of the order.
	inCargoHull, boarded := false, false
	for i := range reqs {
		r := &reqs[i]
		if reqLineWantsCargoHull(r, m.typeVolume(ctx, r.TypeID)) {
			if !boarded {
				boarded = true
				m.boardHauler(ctx, b, "its corporation's fuel")
				inCargoHull = m.shipItemIsCargoHull(ctx, b.ShipID)
			}
			if !inCargoHull {
				m.reqDecline(ctx, b.CharID, r, "bulk tower fuel needs an industrial hull, and it is not in one here")
				m.reclaimAdvance(ctx, r)
				_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ? AND buyerID = ?`,
					r.ReqID, b.CharID)
				reqSnubs.Store(r.ReqID, b.CharID)
				r.Status, r.BuyerID = "open", 0
				continue
			}
		}
		if need := m.typeVolume(ctx, r.TypeID); !reqHoldFits(need, hold) {
			m.reqDecline(ctx, b.CharID, r, fmt.Sprintf("too big for its hold: %.0f m³ a unit, its hauler holds %.0f m³", need, hold))
			m.reclaimAdvance(ctx, r)
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ? AND buyerID = ?`,
				r.ReqID, b.CharID)
			r.Status, r.BuyerID = "open", 0
		}
	}
	// A moon-goods sale run (posgoosale.go) buys nothing: it loads at the
	// staging office and goes before any shopping trip.
	if handled, err := m.posGooTrip(ctx, b, reqs); err != nil || handled {
		return handled, err
	}
	reqs = withoutGooLines(reqs)
	if len(reqs) == 0 {
		return false, nil
	}
	// The order-book sweep (posreq_sweep.go): one route over every line held.
	if m.sweepSettings(ctx).Enabled {
		return m.posSweepTrip(ctx, b, reqs)
	}
	// Each line in turn leads until one yields a trip: one line nobody near
	// sells, or the corporation cannot fund, must not hold up the rest.
	tried := map[int64]bool{}
	for i := range reqs {
		if tried[reqs[i].TypeID] || reqs[i].Status != "buying" || reqs[i].BuyerID != b.CharID {
			continue
		}
		tried[reqs[i].TypeID] = true
		if handled, err := m.posBuyTrip(ctx, b, reqs, i); err != nil || handled {
			return handled, err
		}
	}
	return false, nil
}

// posBuyTrip tries one buy trip led by reqs[li]: the lead's seller, plus every
// other line of this buyer's that seller stocks. handled = the trip began.
func (m *Mod) posBuyTrip(ctx context.Context, b *botRun, reqs []requisition, li int) (bool, error) {
	lead := reqs[li]
	var stop *marketStop
	if lead.SrcStationID != 0 {
		// A Trade desk line is pinned to the seller it was priced at (corptrade.go). If the ask has
		// moved past what the desk allowed, the run is off and the desk rescans.
		var ask sql.NullFloat64
		_ = m.db.QueryRowContext(ctx, `
			SELECT MIN(price) FROM market_orders
			 WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0`,
			lead.SrcStationID, lead.TypeID).Scan(&ask)
		if !ask.Valid || (lead.MaxPrice > 0 && ask.Float64 > lead.MaxPrice) {
			why := "the seller sold out"
			if ask.Valid {
				why = fmt.Sprintf("the ask moved to %s, past the %s the desk allowed", iskString(ask.Float64), iskString(lead.MaxPrice))
			}
			m.closeReq(ctx, &lead, "void", why)
			m.logBot(b.CharID, logInfo, "corp", "the trade run for %s is off — %s", m.typeNameOr(ctx, lead.TypeID), why)
			return false, nil
		}
		stop = &marketStop{TypeID: lead.TypeID, StationID: lead.SrcStationID,
			SystemID: m.stationSystemID(ctx, lead.SrcStationID), Price: ask.Float64}
	} else {
		if _, err := m.shopLadder(ctx, b, []int64{lead.TypeID}, true, nil, bandNear2, "", func(scan marketScan) bool {
			stop = bestBuy(scan.Stops, b.Tune)
			return stop != nil
		}); err != nil {
			return false, err
		}
		if stop == nil {
			// Nothing around the buyer's home: try around the office the
			// goods are for (posreq_reach.go).
			var err error
			if stop, err = m.shopNearDest(ctx, b, lead.TypeID, lead.DestStationID); err != nil {
				return false, err
			}
		}
	}
	if stop == nil {
		// Nobody sells it in reach: note it on the demand ledger so the roster's industrialists see
		// the want (survey probes, ice harvesters, structures nobody stocks)...
		m.noteMarketDemand(ctx, b, []ShopItem{{TypeID: lead.TypeID, Name: m.typeNameOr(ctx, lead.TypeID), Qty: lead.left()}})
		// ...and back to the board for somebody else, the next line leading.
		m.declineNoSeller(ctx, b.CharID, &lead)
		return false, nil
	}
	trip := []int64{}
	var lastErr error
	for i := range reqs {
		r := &reqs[i]
		if len(trip) >= reqTripMaxLines {
			break
		}
		if r.Status != "buying" || r.BuyerID != b.CharID {
			continue // handed back by an earlier lead's refusal this pass
		}
		price := stop.Price
		if r.TypeID != lead.TypeID {
			var p sql.NullFloat64
			_ = m.db.QueryRowContext(ctx, `
				SELECT MIN(price) FROM market_orders WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0`,
				stop.StationID, r.TypeID).Scan(&p)
			if !p.Valid {
				continue
			}
			price = p.Float64
		}
		if err := m.fundReq(ctx, r, price); err != nil {
			// The corporation could not advance it (wallet short, or the line already spent its budget):
			// back on the board with the cause, never left 'buying' against a buyer who cannot pay.
			lastErr = err
			why := "the corporation could not fund it: " + err.Error()
			if errors.Is(err, evedb.ErrInsufficientFunds) {
				why = "the corporation's wallet could not cover the advance"
			}
			m.reqDecline(ctx, b.CharID, r, why)
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ?`, r.ReqID)
			r.Status, r.BuyerID = "open", 0
			continue
		}
		trip = append(trip, r.ReqID)
	}
	if len(trip) == 0 {
		if lastErr == nil {
			m.reqDecline(ctx, b.CharID, &lead, "nothing on the trip to "+m.stationLabel(ctx, stop.StationID)+" could be bought")
		}
		return false, nil
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET tripAt = ? WHERE reqID IN (`+int64CSV(trip)+`)`, clock.Unix())
	st := b.State
	st.Activity = ActIndustry
	st.Data = idleData(st.Data)
	st.Data.PosTripReqs = trip
	st.Data.PosTripStation = stop.StationID
	m.logBot(b.CharID, logInfo, "corp", "buying %d requisition line(s) for its corporation at %s",
		len(trip), m.stationLabel(ctx, stop.StationID))
	return true, m.beginTravel(ctx, b, stop.SystemID, stop.StationID, string(TaskPosBuy))
}

// fundReq advances the buyer what the rest of a line costs at a price, never
// past the line's remaining budget.
func (m *Mod) fundReq(ctx context.Context, r *requisition, price float64) error {
	need := math.Ceil(price * float64(r.left()) * reqAdvanceMarkup)
	// The wage is part of the budget but never part of the advance: a trade
	// line's hauler is paid on delivery, out of what the buy left.
	room := r.Budget - r.Wage - r.Advance
	if need > room {
		need = room
	}
	if need < 1 {
		if r.Advance-r.Spent >= price {
			return nil // already holds enough
		}
		return errors.New("over budget")
	}
	if _, _, err := m.corpWithdraw(ctx, r.CorpID, r.BuyerID, need,
		"Advance to buy corporation supplies"); err != nil {
		return err
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET advance = advance + ? WHERE reqID = ?`, need, r.ReqID)
	r.Advance += need
	return nil
}

// posBuyStep is TaskPosBuy: at the seller, buy and load; at a destination,
// file and settle.
func (m *Mod) posBuyStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	finish := func(note string) (bool, error) {
		if note != "" {
			m.logBot(b.CharID, logInfo, "corp", "%s", note)
		}
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	if len(d.PosTripReqs) == 0 {
		return finish("")
	}
	reqs, err := m.queryReqs(ctx, `WHERE reqID IN (`+int64CSV(d.PosTripReqs)+`) ORDER BY destStationID, reqID`)
	if err != nil {
		return false, err
	}
	// Buying.
	if d.PosTripStation != 0 && d.PosSweep {
		if b.StationID != d.PosTripStation {
			return false, m.beginTravel(ctx, b, m.stationSystemID(ctx, d.PosTripStation), d.PosTripStation, string(TaskPosBuy))
		}
		travelling, err := m.posSweepBuyHere(ctx, b, reqs)
		if err != nil || travelling {
			return false, err
		}
		if !sweepTripGotAnything(d.PosSweepGot) {
			for i := range reqs {
				m.reclaimAdvance(ctx, &reqs[i])
				_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ? AND buyerID = ?`,
					reqs[i].ReqID, b.CharID)
			}
			return finish("found nothing it could buy for its corporation on the sweep")
		}
	} else if d.PosTripStation != 0 {
		if b.StationID != d.PosTripStation {
			return false, m.beginTravel(ctx, b, m.stationSystemID(ctx, d.PosTripStation), d.PosTripStation, string(TaskPosBuy))
		}
		room := m.boardHauler(ctx, b, "its corporation's supplies")
		bought := 0
		// What this counter was asked for and gave, for the shopper-demand
		// ledger (stationdemand.go). Trade-desk lines are goods bought to sell
		// again, not a want.
		var stop []shopLine
		for i := range reqs {
			r := &reqs[i]
			if r.Status != "buying" || r.BuyerID != b.CharID || r.left() <= 0 {
				continue
			}
			if r.RefKind == refGooSale {
				moved := m.loadGooLine(ctx, b, r, room)
				room -= float64(moved) * math.Max(m.typeVolume(ctx, r.TypeID), 0.01)
				if moved > 0 {
					bought++
				}
				continue
			}
			vol := math.Max(m.typeVolume(ctx, r.TypeID), 0.01)
			qty := r.left()
			if fit := int64(room / vol); fit < qty {
				qty = fit
				if fit <= 0 {
					// Said, not skipped: the line goes back on the board below,
					// and the note is the only trace of why.
					m.reqDecline(ctx, b.CharID, r, fmt.Sprintf("no room left in its hold: %.0f m³ a unit, %.0f m³ free", vol, room))
				}
			}
			if r.Requester == reqTrade {
				// The corporation's money and nobody else's: a trade run is
				// sized to the advance, never topped up from the hauler's own
				// wallet.
				var ask sql.NullFloat64
				_ = m.db.QueryRowContext(ctx, `
					SELECT MIN(price) FROM market_orders
					 WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0`,
					b.StationID, r.TypeID).Scan(&ask)
				if !ask.Valid || ask.Float64 <= 0 {
					continue
				}
				if afford := int64((r.Advance - r.Spent) / ask.Float64); afford < qty {
					qty = afford
				}
			}
			if qty <= 0 {
				continue
			}
			_, filled, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID, r.TypeID, qty, "corporation supplies")
			if r.Requester != reqTrade && (err == nil || !isMoneyRefusal(err)) {
				stop = append(stop, shopLine{TypeID: r.TypeID, Asked: qty, Filled: max(filled, 0)})
			}
			if err != nil || filled <= 0 {
				continue
			}
			moved, _ := m.moveUnits(ctx, b.CharID, b.StationID, 4, r.TypeID, filled, b.ShipID, 5, b.CorpID)
			m.settleCorpBuyRemainder(ctx, b, b.StationID, r.TypeID, filled-moved)
			room -= float64(moved) * vol
			r.Spent += spent
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET spent = spent + ? WHERE reqID = ?`, spent, r.ReqID)
			m.spendCommit(ctx, "req", r.ReqID, spent)
			m.corpBook(ctx, r.CorpID, reqLedgerLine(r), -spent, r.TypeID, fmt.Sprintf("%d× %s (%s)", filled, m.typeNameOr(ctx, r.TypeID), r.Requester))
			m.towerBookReq(ctx, r, spent, filled)
			bought++
		}
		m.noteShopStop(ctx, b.StationID, stop)
		d.PosTripStation = 0
		if bought == 0 {
			for i := range reqs {
				m.reclaimAdvance(ctx, &reqs[i])
				_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ? AND buyerID = ?`,
					reqs[i].ReqID, b.CharID)
			}
			return finish("found nothing it could buy for its corporation at the seller")
		}
		if err := saveState(ctx, m.db, st); err != nil {
			return false, err
		}
	}
	// Delivering: the nearest destination with goods for it.
	for i := range reqs {
		r := &reqs[i]
		if !reqWantsGoods(*r) {
			continue // filled or void: nothing to deliver
		}
		if m.cargoUnitsOwnedBy(ctx, b.ShipID, b.CorpID, r.TypeID) == 0 {
			continue
		}
		if b.StationID != r.DestStationID {
			return false, m.beginTravel(ctx, b, m.stationSystemID(ctx, r.DestStationID), r.DestStationID, string(TaskPosBuy))
		}
		office := m.officeAt(ctx, b.CorpID, r.DestStationID)
		if office == 0 || gooSellsHere(r) {
			// A Trade desk run ends wherever the bid is: sold here on the hauler's account for the
			// corporation, and anything the bids would not take goes to the nearest office (corptrade.go).
			if r.Requester == reqTrade {
				to, err := m.sellTradeRunHere(ctx, b, r)
				if err != nil {
					return false, err
				}
				if to != 0 {
					return false, m.beginTravel(ctx, b, m.stationSystemID(ctx, to), to, string(TaskPosBuy))
				}
			}
			continue
		}
		moved, err := m.moveUnits(ctx, b.CorpID, b.ShipID, 5, r.TypeID, r.left(), office, r.DestFlag, b.CorpID)
		if err != nil {
			return false, err
		}
		r.QtyDone += moved
		if moved > 0 {
			r.ProgressAt = clock.Unix()
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET qtyDone = qtyDone + ?, progressAt = ? WHERE reqID = ?`,
				moved, r.ProgressAt, r.ReqID)
		}
		m.payTradeRunWage(ctx, r, b.CharID, moved)
	}
	if travelling, err := m.dropDeadTripCargo(ctx, b, reqs); err != nil || travelling {
		return false, err
	}
	// Anything still in the hold for a line whose station is here or done
	// stays corp property; settle every line on the trip.
	// A line the desk took back mid-trip (reqLineStalled) and handed on is the new buyer's: only
	// its delivery counts here.
	filled, back := 0, 0
	for i := range reqs {
		r := &reqs[i]
		if r.Status == "void" || r.Status == "done" || (r.left() > 0 && (r.Status != "buying" || r.BuyerID != b.CharID)) {
			continue
		}
		m.reclaimAdvance(ctx, r)
		if r.left() <= 0 {
			m.closeReq(ctx, r, "done", "delivered by "+m.charNameOr(ctx, b.CharID))
			filled++
			continue
		}
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ? AND status = 'buying' AND buyerID = ?`,
			r.ReqID, b.CharID)
		back++
	}
	if c, _ := getCEO(ctx, m.db, b.CorpID); c != nil {
		m.ceoLog(ctx, c, powerStarbase, "delivered", b.CharID, 0, "%s", reqRunSummary(m.charNameOr(ctx, b.CharID), filled, back))
	}
	return finish("delivered its corporation's supplies")
}

// reqWantsGoods is whether a line still takes deliveries. Pure.
func reqWantsGoods(r requisition) bool {
	return r.Status != "void" && r.Status != "done" && r.left() > 0
}

// dropDeadTripCargo unloads goods bought for lines that closed mid-trip into the office the line
// was for, so the corporation can use them. travelling is true when it set out for that office.
func (m *Mod) dropDeadTripCargo(ctx context.Context, b *botRun, reqs []requisition) (bool, error) {
	live := map[int64]bool{}
	for _, r := range reqs {
		if reqWantsGoods(r) {
			live[r.TypeID] = true
		}
	}
	var target int64
	for i := range reqs {
		r := &reqs[i]
		if live[r.TypeID] {
			continue
		}
		n := m.cargoUnitsOwnedBy(ctx, b.ShipID, b.CorpID, r.TypeID)
		if n <= 0 {
			continue
		}
		office := m.officeAt(ctx, b.CorpID, r.DestStationID)
		if office == 0 {
			continue
		}
		if b.StationID != r.DestStationID {
			if target == 0 {
				target = r.DestStationID
			}
			continue
		}
		moved, err := m.moveUnits(ctx, b.CorpID, b.ShipID, 5, r.TypeID, n, office, r.DestFlag, b.CorpID)
		if err != nil || moved <= 0 {
			return false, nil // could not unload here: keep it aboard rather than wander
		}
		m.logBot(b.CharID, logInfo, "corp", "left %d× %s at its corporation's office (the order had closed)",
			moved, m.typeNameOr(ctx, r.TypeID))
	}
	if target == 0 {
		return false, nil
	}
	return true, m.beginTravel(ctx, b, m.stationSystemID(ctx, target), target, string(TaskPosBuy))
}

// reqRunSummary words a buy run's end for the CEO's ledger: the lines it
// filled, and the ones that went back on the board unfilled. Pure.
func reqRunSummary(who string, filled, back int) string {
	s := fmt.Sprintf("%s delivered a requisition run (%d line(s) filled", who, filled)
	if back > 0 {
		s += fmt.Sprintf(", %d back on the board", back)
	}
	return s + ")"
}

// payTradeRunWage pays a Trade desk line's hauler for the units it just landed: the promised
// wage pro rata, never past the promise. It is labour credit on a bot-run corporation
// (corppayroll.go), cash on any other; the commitment the line opened covers it. A no-op on a
// line with no wage.
func (m *Mod) payTradeRunWage(ctx context.Context, r *requisition, carrier int64, moved int64) {
	if r.Wage <= 0 || moved <= 0 || r.Qty <= 0 || carrier == 0 {
		return
	}
	share := math.Floor(r.Wage * float64(moved) / float64(r.Qty))
	if left := r.Wage - r.WagePaid; share > left {
		share = math.Floor(left)
	}
	if share < 1 {
		return
	}
	if !m.payCorpWage(ctx, r.CorpID, carrier, share, "trade run") {
		return
	}
	r.WagePaid += share
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET wagePaid = wagePaid + ? WHERE reqID = ?`, share, r.ReqID)
	m.spendCommit(ctx, "req", r.ReqID, share)
	if c, _ := getCEO(ctx, m.db, r.CorpID); c != nil {
		m.ceoLog(ctx, c, powerBusiness, "tradeland", carrier, share,
			"%s landed %d× %s at %s for the trade desk and was credited %s",
			m.charNameOr(ctx, carrier), moved, m.typeNameOr(ctx, r.TypeID), m.stationLabel(ctx, r.DestStationID), iskString(share))
	}
}

// openReqSummary is the board's short list of what is on order.
func (m *Mod) openReqSummary(ctx context.Context, corpID int64) []requisition {
	reqs, _ := m.queryReqs(ctx, `WHERE corporationID = ? AND status IN ('open','bidding','buying','building') ORDER BY deadline LIMIT 40`, corpID)
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].Deadline < reqs[j].Deadline })
	return reqs
}
