package rolebots

import (
	"context"
	"fmt"
)

// The CEO page's Trade desk card (corptrade.go): the runs in flight, the
// shelf waiting to be listed, the listings resting, and what the desk and
// the trade levy brought in this week.

// ceoTradeRunRow is one requisition line as the card shows it.
type ceoTradeRunRow struct {
	What, From, To, Status, Buyer, Budget, Wage, Due string
}

// ceoTradeShelfRow is one lot in a Trade division.
type ceoTradeShelfRow struct {
	Where, What, Landed string
}

// ceoTradeOrderRow is one resting listing.
type ceoTradeOrderRow struct {
	Where, What, Price, Age string
}

// ceoTradeView is the card's data; nil for a corporation with no desk.
type ceoTradeView struct {
	Head      string
	Runs      []ceoTradeRunRow
	Shelf     []ceoTradeShelfRow
	Orders    []ceoTradeOrderRow
	SalesWeek string
	LevyWeek  string
	WagesWeek string
	Offices   int
	Carriers  int
	Hold      string
}

// ceoTradeView reads the card, or nil when the corporation's tags give it
// no Trade desk.
func (m *Mod) ceoTradeView(ctx context.Context, c *corpCEO, facts *CorpFacts) *ceoTradeView {
	if facts == nil || !tradeDeskTagged(facts.Policy.Tags) {
		return nil
	}
	s := m.tradeDeskData(ctx, c.CorpID, true)
	v := &ceoTradeView{Head: "the CEO", Offices: s.Offices, Carriers: s.Carriers, Hold: fmt.Sprintf("%.0f m³", s.Hold)}
	if s.HeadID != 0 {
		v.Head = m.charNameOr(ctx, s.HeadID)
	}
	now := clock.Unix()
	for _, r := range s.Runs {
		row := ceoTradeRunRow{
			What:   fmt.Sprintf("%d× %s", r.Qty, m.typeNameOr(ctx, r.TypeID)),
			From:   m.stationLabel(ctx, r.From),
			To:     m.stationLabel(ctx, r.To),
			Status: r.Status,
			Budget: iskString(r.Budget - r.Spent),
			Wage:   iskString(r.Wage),
			Due:    fmtDuration(max(0, r.Deadline-now)),
		}
		if r.BuyerID != 0 {
			row.Buyer = m.charNameOr(ctx, r.BuyerID)
		}
		v.Runs = append(v.Runs, row)
	}
	for _, sh := range s.Shelf {
		v.Shelf = append(v.Shelf, ceoTradeShelfRow{
			Where:  m.stationLabel(ctx, sh.StationID),
			What:   fmt.Sprintf("%d× %s", sh.Qty, m.typeNameOr(ctx, sh.TypeID)),
			Landed: iskString(sh.Landed),
		})
	}
	for _, o := range s.Orders {
		v.Orders = append(v.Orders, ceoTradeOrderRow{
			Where: m.stationLabel(ctx, o.StationID),
			What:  fmt.Sprintf("%d× %s", o.Remaining, m.typeNameOr(ctx, o.TypeID)),
			Price: iskString(o.Price),
			Age:   fmtDuration(max(0, now-o.PlacedAt)),
		})
	}
	wk := m.tradeWeek(ctx, c.CorpID, now-7*86400, now)
	v.SalesWeek, v.LevyWeek, v.WagesWeek = iskString(wk.Sales), iskString(wk.Levy), iskString(wk.Wages)
	return v
}

// tradeDeskSnap is the Trade desk as raw values, read once and drawn twice:
// as figures on the CEO card (ceoTradeView) and as bars on the Corporate
// Portal's Trade page (corptrade_igb.go), which may show no wallet figure.
type tradeDeskSnap struct {
	HeadID   int64
	Offices  int
	Carriers int     // withHold only
	Hold     float64 // withHold only
	Runs     []tradeRunSnap
	Shelf    []tradeShelfSnap
	Orders   []tradeOrderSnap
}

type tradeRunSnap struct {
	TypeID, Qty, From, To, BuyerID, Deadline int64
	Status                                   string
	Budget, Spent, Wage                      float64
}

type tradeShelfSnap struct {
	StationID, TypeID, Qty int64
	Landed                 float64
}

type tradeOrderSnap struct {
	StationID, TypeID, Remaining, PlacedAt int64
	Price                                  float64
}

// tradeDeskData reads the desk: its head and offices, the runs in flight,
// the shelf in the Trade divisions, and the listings resting. withHold also
// sizes the haulers' holds (tradeDeskHold), which picks every member a ship
// and is far too heavy for a page any capsuleer can reload.
func (m *Mod) tradeDeskData(ctx context.Context, corpID int64, withHold bool) *tradeDeskSnap {
	s := &tradeDeskSnap{HeadID: m.commerceHead(ctx, corpID)}
	offices, _, _ := m.tradeDeskOffices(ctx, corpID)
	s.Offices = len(offices)
	if withHold {
		s.Hold, s.Carriers = m.tradeDeskHold(ctx, corpID)
	}
	if reqs, err := m.queryReqs(ctx, `
		WHERE corporationID = ? AND requester = ? AND status IN ('open','buying') ORDER BY reqID`,
		corpID, reqTrade); err == nil {
		for _, r := range reqs {
			s.Runs = append(s.Runs, tradeRunSnap{TypeID: r.TypeID, Qty: r.left(), From: r.SrcStationID,
				To: r.DestStationID, BuyerID: r.BuyerID, Deadline: r.Deadline, Status: r.Status,
				Budget: r.Budget, Spent: r.Spent, Wage: r.Wage})
		}
	}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, e.typeID, SUM(e.quantity)
		  FROM crpoffices o
		  JOIN entity e ON e.locationID = o.itemID AND e.ownerID = o.corporationID AND e.flag = ?
		 WHERE o.corporationID = ? AND e.quantity > 0
		 GROUP BY o.stationID, e.typeID ORDER BY o.stationID, e.typeID LIMIT 40`,
		corpDivTrade, corpID); err == nil {
		for rows.Next() {
			var sh tradeShelfSnap
			if rows.Scan(&sh.StationID, &sh.TypeID, &sh.Qty) == nil {
				s.Shelf = append(s.Shelf, sh)
			}
		}
		rows.Close()
		for i := range s.Shelf {
			s.Shelf[i].Landed = m.tradeLandedCost(ctx, corpID, s.Shelf[i].StationID, s.Shelf[i].TypeID)
		}
	}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT r.stationID, r.typeID, r.price, r.placedAt, COALESCE(o.volRemaining, 0)
		  FROM rolebot_corp_orders r LEFT JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.kind = ? AND r.bid = 0 ORDER BY r.placedAt LIMIT 40`,
		corpID, corpOrderTrade); err == nil {
		for rows.Next() {
			var o tradeOrderSnap
			if rows.Scan(&o.StationID, &o.TypeID, &o.Price, &o.PlacedAt, &o.Remaining) == nil {
				s.Orders = append(s.Orders, o)
			}
		}
		rows.Close()
	}
	return s
}

// tradeWeekSnap is what trade brought in and paid out over one window.
type tradeWeekSnap struct {
	Sales, Levy, Wages float64
}

// tradeWeek reads sales, the trade levy and hauler pay booked in [from, to).
func (m *Mod) tradeWeek(ctx context.Context, corpID, from, to int64) tradeWeekSnap {
	var w tradeWeekSnap
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN line = ? THEN amount ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN line = ? THEN amount ELSE 0 END), 0)
		  FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND at >= ? AND at < ? AND amount > 0`,
		lineSales, lineTradeLevy, corpID, from, to).Scan(&w.Sales, &w.Levy)
	// Hauler pay: labour credit on the payroll (corppayroll.go), plus any
	// cash wages booked before it.
	var wages, credited float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(-SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND line = ? AND at >= ? AND at < ? AND amount < 0 AND detail LIKE 'trade run%'`,
		corpID, lineWages, from, to).Scan(&wages)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(credit), 0) FROM rolebot_corp_labour
		 WHERE corporationID = ? AND kind = 'traderun' AND at >= ? AND at < ?`, corpID, from, to).Scan(&credited)
	w.Wages = wages + credited
	return w
}

// tmplCEOTrade is the card, included in the CEO page.
const tmplCEOTrade = `{{with .Trade}}
<div class="card">
  <h3>Trade desk</h3>
  <p><b>Run by:</b> {{.Head}} · <b>Offices to sell at:</b> {{.Offices}} · <b>Haulers:</b> {{.Carriers}} (largest hold {{.Hold}})</p>
  <p><b>This week:</b> sales {{.SalesWeek}} · trade levy {{.LevyWeek}} · hauler pay {{.WagesWeek}}</p>
  {{if .Runs}}
  <table class="tbl" style="font-size:12px">
    <tr><th>Run</th><th>Buy at</th><th>Sell at</th><th>Status</th><th>Hauler</th><th>Budget left</th><th>Wage</th><th>Due in</th></tr>
    {{range .Runs}}<tr><td>{{.What}}</td><td>{{.From}}</td><td>{{.To}}</td><td>{{.Status}}</td><td>{{.Buyer}}</td><td>{{.Budget}}</td><td>{{.Wage}}</td><td>{{.Due}}</td></tr>{{end}}
  </table>
  {{else}}<p>No run in flight.</p>{{end}}
  {{if .Shelf}}<h4 style="margin-top:10px">On the shelf</h4>
  <table class="tbl" style="font-size:12px"><tr><th>Office</th><th>Stock</th><th>Landed cost</th></tr>
    {{range .Shelf}}<tr><td>{{.Where}}</td><td>{{.What}}</td><td>{{.Landed}}</td></tr>{{end}}
  </table>{{end}}
  {{if .Orders}}<h4 style="margin-top:10px">Listed</h4>
  <table class="tbl" style="font-size:12px"><tr><th>Office</th><th>Listing</th><th>Price</th><th>Resting for</th></tr>
    {{range .Orders}}<tr><td>{{.Where}}</td><td>{{.What}}</td><td>{{.Price}}</td><td>{{.Age}}</td></tr>{{end}}
  </table>{{end}}
  <p style="font-size:12px;color:#6b7280">The corporation scans the market with its own capital, advances a member the money
  to buy and haul the goods as corporation property, pays the member by the m³·jump on delivery, and lists the goods
  at its own office. Fills land in the corporation wallet as sales.</p>
</div>
{{end}}`
