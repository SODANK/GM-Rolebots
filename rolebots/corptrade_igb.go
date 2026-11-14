package rolebots

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// The Trade department page of the Corporate Portal (/corp/trade) shows how the
// corporation's Sales department trades: the desk's runs and shelf (corptrade.go), the buy
// orders (corpbuyback.go), the freight it needs moved and the week's figures.
// The page only reads. Money is drawn as bars, never as numbers (corpportal.go keeps
// private figures off the page). Prices the market already shows stay exact: a bid, an
// ask, a posted wage, a courier reward.

// handleIGBCorpTrade serves /corp/trade.
func (m *Mod) handleIGBCorpTrade(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	charID, corpID := m.portalViewer(ctx, r)
	if corpID == 0 {
		esp.WriteCorpPortalPage(w, r, "trade", igbF3("Open this page from your own terminal to see your corporation's trade."))
		return
	}
	esp.WriteCorpPortalPage(w, r, "trade", m.tradeBody(ctx, r, corpID, charID))
}

// tradeBody builds the page: the figures, the desk's runs, the buy orders, the freight,
// the week and the department.
func (m *Mod) tradeBody(ctx context.Context, r *http.Request, corpID, viewer int64) string {
	pg := m.igbPageFor(r)
	host := esp.IGBHost()
	pr := m.portalRosterFor(ctx, corpID)
	ms := pr.ByArm[armSales]
	on, space := onDuty(ms)

	hasDesk := false
	if facts, err := getAnyCorpFacts(ctx, m.db, corpID); err == nil && facts != nil {
		hasDesk = tradeDeskTagged(facts.Policy.Tags)
	}
	var desk *tradeDeskSnap
	if hasDesk {
		desk = m.tradeDeskData(ctx, corpID, false)
	}
	bids := m.buybackRows(ctx, corpID)

	var b strings.Builder
	b.WriteString(pg.soft(`The Sales department: the goods the corporation carries from where they are cheap to where `+
		`they sell, what it is buying, and the freight it needs moved. Joining Sales is done on the `+
		`<a href="`+host+`/corp/">Overview</a>.`) + "<br><br>\n")
	runs, offices := 0, 0
	if desk != nil {
		runs, offices = len(desk.Runs), desk.Offices
	}
	pg.statStrip(&b, []igbStat{
		{Label: "Members", Value: strconv.Itoa(len(ms))},
		{Label: "On duty", Value: strconv.Itoa(on), Note: fmt.Sprintf("%d in space", space)},
		{Label: "Runs in flight", Value: strconv.Itoa(runs)},
		{Label: "Buy orders", Value: strconv.Itoa(len(bids))},
		{Label: "Trade offices", Value: strconv.Itoa(offices)},
	})
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "The desk")
	if desk == nil {
		b.WriteString(pg.soft("This corporation keeps no trade desk: its members trade on their own account.") + "<br>\n")
	} else {
		head := "the CEO"
		if desk.HeadID != 0 {
			head = esp.PilotLink(desk.HeadID, esp.IGBText(m.charNameOr(ctx, desk.HeadID)))
		}
		b.WriteString(pg.soft("The desk is run by ") + igbF3(head) +
			pg.soft(fmt.Sprintf(". It scans the market for a lane worth a trip, buys at the cheap end and lists at the dear one; "+
				"the %d member(s) of Sales carry it.", len(ms))) + "<br>\n")
		esp.PanelBand(&b, pg.pal, []esp.Panel{m.tradeRunsPanel(ctx, pg, desk)})
		b.WriteString("<br>\n")
		esp.PanelBand(&b, pg.pal, []esp.Panel{m.tradeShelfPanel(ctx, pg, desk), m.tradeListingsPanel(ctx, pg, corpID, desk)})
	}
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "We buy")
	b.WriteString(pg.soft("Sell into these at the station's market: the corporation's buy order fills like any other, and the "+
		"goods land in its hangar.") + "<br>\n")
	esp.PanelBand(&b, pg.pal, []esp.Panel{m.tradeBidsPanel(ctx, pg, bids), m.tradeDeliverPanel(ctx, pg, corpID, viewer)})
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "Freight")
	legs, _ := m.haulBoardPanel(ctx, pg, corpID, "On the haul board", "No leg is waiting.")
	legs.Note = "carried by the corporation's own haulers"
	esp.PanelBand(&b, pg.pal, []esp.Panel{legs, m.tradeCouriersPanel(ctx, pg, corpID)})
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "Trade week")
	esp.PanelBand(&b, pg.pal, []esp.Panel{m.tradeWeekPanel(ctx, pg, corpID)})
	b.WriteString("<br>\n")

	esp.Section(&b, pg.pal, "The department")
	m.writeDeptRoster(ctx, &b, pg, pr, armSales, viewer)
	m.writeArmRequests(ctx, &b, pg, corpID, armSales)
	return b.String()
}

// tradeRunsPanel lists the desk's runs in flight: the goods, the lane, who carries them,
// how much of the budget is spent, the wage and the deadline.
func (m *Mod) tradeRunsPanel(ctx context.Context, pg igbPage, desk *tradeDeskSnap) esp.Panel {
	p := esp.Panel{Title: "Runs in flight", Note: "budget spent in green",
		Align: []string{"", "", "", "", "right"}, Width: []string{"36", "", "150", "110", "110"},
		Empty: "No run is out: the desk has found no lane worth the trip."}
	col := armColor(armSales)
	for _, r := range desk.Runs {
		what := igbF3(fmt.Sprintf("%d&times; %s", r.Qty, esp.IGBText(m.typeNameOr(ctx, r.TypeID)))) + "<br>" +
			pg.soft(esp.IGBText(m.stationLabel(ctx, r.From))+" &rarr; "+esp.IGBText(m.stationLabel(ctx, r.To)))
		who := pg.tint(pg.pal.Amber, "waiting for a hauler")
		if r.BuyerID != 0 {
			who = igbF3(esp.IGBText(m.charNameOr(ctx, r.BuyerID)))
		}
		who += "<br>" + pg.soft(esp.IGBText(r.Status))
		budget := pg.soft("&mdash;")
		if r.Budget > 0 {
			budget = pg.ratioBar(r.Spent, r.Budget-r.Spent, 16, col, "#3a3a3a")
		}
		wage := igbF3(esp.ISKShort(r.Wage)) + " " + pg.dim("ISK") + "<br>" + pg.soft("due "+igbIn(r.Deadline))
		p.Rows = append(p.Rows, []string{esp.ShipIcon(r.TypeID, 32), what, who, budget, wage})
	}
	return p
}

// tradeShelfPanel lists what sits in the Trade divisions waiting to be listed.
func (m *Mod) tradeShelfPanel(ctx context.Context, pg igbPage, desk *tradeDeskSnap) esp.Panel {
	p := esp.Panel{Title: "On the shelf", Note: "landed, not yet listed",
		Align: []string{"", ""}, Width: []string{"36", ""}, Empty: "The shelf is bare."}
	for i, s := range desk.Shelf {
		if i == 12 {
			p.Rows = append(p.Rows, []string{"", pg.soft(fmt.Sprintf("&hellip; and %d more lots", len(desk.Shelf)-12))})
			break
		}
		p.Rows = append(p.Rows, []string{esp.ShipIcon(s.TypeID, 32),
			igbF3(fmt.Sprintf("%d&times; %s", s.Qty, esp.IGBText(m.typeNameOr(ctx, s.TypeID)))) + "<br>" +
				pg.soft(esp.IGBText(m.stationLabel(ctx, s.StationID)))})
	}
	return p
}

// tradeListingsPanel lists the desk's asks resting on the market. The margin bar is the
// ask's share above what the lot cost to land there.
func (m *Mod) tradeListingsPanel(ctx context.Context, pg igbPage, corpID int64, desk *tradeDeskSnap) esp.Panel {
	p := esp.Panel{Title: "Listed for sale", Note: "margin over landed cost",
		Align: []string{"", "", "right"}, Width: []string{"36", "", "110"}, Empty: "Nothing of the desk's is listed."}
	col := armColor(armSales)
	now := clock.Unix()
	for i, o := range desk.Orders {
		if i == 12 {
			p.Rows = append(p.Rows, []string{"", pg.soft(fmt.Sprintf("&hellip; and %d more", len(desk.Orders)-12)), ""})
			break
		}
		margin := ""
		if landed := m.tradeLandedCost(ctx, corpID, o.StationID, o.TypeID); landed > 0 && o.Price > 0 {
			margin = "<br>" + esp.Bar(tradeMargin(o.Price, landed), 12, col)
		}
		p.Rows = append(p.Rows, []string{esp.ShipIcon(o.TypeID, 32),
			igbF3(fmt.Sprintf("%d&times; %s", o.Remaining, esp.IGBText(m.typeNameOr(ctx, o.TypeID)))) + "<br>" +
				pg.soft(esp.IGBText(m.stationLabel(ctx, o.StationID))+" &middot; "+fmtDuration(max(0, now-o.PlacedAt))),
			igbF3(esp.ISKShort(o.Price)) + " " + pg.dim("each") + margin})
	}
	return p
}

// tradeMargin returns an ask's share above its landed cost, 0 at or under it. Pure.
func tradeMargin(price, landed float64) float64 {
	if price <= 0 || landed >= price {
		return 0
	}
	return (price - landed) / price
}

// tradeBidsPanel lists the corporation's live buy orders, each bid against the end-user
// tape of its region: what selling into it pays next to what the market has paid.
func (m *Mod) tradeBidsPanel(ctx context.Context, pg igbPage, bids []buybackRow) esp.Panel {
	p := esp.Panel{Title: "Buy orders", Note: "bid against what the region has been paying",
		Align: []string{"", "", "right"}, Width: []string{"36", "", "130"},
		Empty: "The corporation is buying nothing right now."}
	for _, r := range bids {
		what := igbF3(esp.IGBText(m.typeNameOr(ctx, r.TypeID))) + " " +
			pg.dim(fmt.Sprintf("%d wanted", r.Remaining)) + "<br>" +
			pg.soft(esp.IGBText(m.stationLabel(ctx, r.StationID)))
		price := igbF3(esp.ISKShort(r.Price)) + " " + pg.dim("each")
		if tape := m.regionTradeRef(ctx, r.StationID, r.TypeID); tape.Avg > 0 && tape.Trades > 0 {
			price += "<br>" + pg.dim(fmt.Sprintf("%.0f%% of the tape", 100*r.Price/tape.Avg))
		}
		p.Rows = append(p.Rows, []string{esp.ShipIcon(r.TypeID, 32), what, price})
	}
	return p
}

// tradeDeliverPanel lists the viewer's own deliver assignments (a bid with a premium,
// offered to them by name) and the other ways to take part.
func (m *Mod) tradeDeliverPanel(ctx context.Context, pg igbPage, corpID, viewer int64) esp.Panel {
	p := esp.Panel{Title: "Your part", Align: []string{""}}
	if assignmentsOn() && viewer != 0 {
		as, _ := m.queryAssignments(ctx, `WHERE corporationID = ? AND characterID = ? AND kind = ? AND status IN (?, ?)
			ORDER BY offeredAt DESC LIMIT 5`, corpID, viewer, assignDeliver, asOffered, asAccepted)
		for _, a := range as {
			line := fmt.Sprintf("Deliver %d %s to %s", a.Qty, m.typeNameOr(ctx, a.TypeID), m.stationLabel(ctx, a.StationID))
			state := pg.tint(pg.pal.Amber, "offered") + " " + pg.soft("&mdash; the letter carries the link")
			if a.Status == asAccepted {
				state = pg.tint(pg.pal.Green, fmt.Sprintf("%d of %d", a.Progress, a.Qty)) + " " + pg.soft(igbIn(a.ExpiresAt))
			}
			reward := ""
			if a.Reward > 0 {
				reward = " " + pg.dim(esp.ISKShort(a.Reward)+" ISK on delivery")
			}
			p.Rows = append(p.Rows, []string{igbF3(esp.IGBText(line)) + reward + "<br>" + state})
		}
	}
	if assignmentsOn() {
		p.Rows = append(p.Rows, []string{pg.soft(`Delivery jobs at a premium over our bids are on ` +
			`<a href="` + esp.IGBHost() + assignBoardPortalPath + `">Assignments</a>.`)})
	}
	p.Rows = append(p.Rows,
		[]string{pg.soft("Bring any of these goods to the bidding station and sell at the market.")},
		[]string{pg.soft("Take a courier contract the corporation put out (beside), in the Contracts window.")},
		[]string{pg.soft("Your own trades are yours: no levy is taken from them.")})
	return p
}

// tradeCouriersPanel lists the courier contracts the corporation has put out, the one
// leg of its freight any capsuleer can take.
func (m *Mod) tradeCouriersPanel(ctx context.Context, pg igbPage, corpID int64) esp.Panel {
	p := esp.Panel{Title: "Couriers open to you", Note: "accept in the Contracts window",
		Align: []string{"", "right"}, Width: []string{"", "120"}, Empty: "The corporation has no courier contract out."}
	rows, err := m.db.QueryContext(ctx, `
		SELECT pickupStationID, dropStationID, volume, totalReward, collateral
		  FROM couriermissions
		 WHERE issuerCorpID = ? AND state = ? AND kind = 1
		 ORDER BY totalReward DESC LIMIT 12`, corpID, evedb.CourierStatePosted)
	if err != nil {
		return p
	}
	defer rows.Close()
	for rows.Next() {
		var from, to int64
		var vol, reward, collateral float64
		if rows.Scan(&from, &to, &vol, &reward, &collateral) != nil {
			continue
		}
		route := igbF3(esp.IGBText(m.stationLabel(ctx, from))+" &rarr; "+esp.IGBText(m.stationLabel(ctx, to))) + "<br>" +
			pg.soft(fmt.Sprintf("%.0f m3", vol))
		if collateral > 0 {
			route += " " + pg.dim(esp.ISKShort(collateral)+" ISK collateral")
		}
		p.Rows = append(p.Rows, []string{route, igbF3(esp.ISKShort(reward)) + " " + pg.dim("ISK")})
	}
	return p
}

// tradeWeekPanel compares this week with last: sales, the trade levy and hauler pay as
// bars against the largest of them.
func (m *Mod) tradeWeekPanel(ctx context.Context, pg igbPage, corpID int64) esp.Panel {
	now := clock.Unix()
	this := m.tradeWeek(ctx, corpID, now-7*86400, now)
	last := m.tradeWeek(ctx, corpID, now-14*86400, now-7*86400)
	p := esp.Panel{Title: "This week and last", Note: "against each other, not to scale with anything else",
		Align: []string{"", "", ""}, Width: []string{"150", "", ""}}
	most := max(this.Sales, this.Levy, this.Wages, last.Sales, last.Levy, last.Wages)
	col := armColor(armSales)
	p.Rows = append(p.Rows, []string{"", pg.dim3("this week"), pg.dim3("last week")})
	for _, row := range []struct {
		label    string
		now, was float64
	}{
		{"Goods sold", this.Sales, last.Sales},
		{"Trade levy", this.Levy, last.Levy},
		{"Paid to haulers", this.Wages, last.Wages},
	} {
		p.Rows = append(p.Rows, []string{pg.dim3(row.label),
			relBar(row.now, most, 20, col), relBar(row.was, most, 20, igbSoftText)})
	}
	var tax float64
	_ = m.db.QueryRowContext(ctx, `SELECT COALESCE(taxRate, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&tax)
	if rate := tradeLevyRate(tax); rate > 0 {
		p.Rows = append(p.Rows, []string{pg.dim3("The levy"),
			pg.soft(fmt.Sprintf("%.0f%% of its career traders' profit goes to the corporation", rate*100)), ""})
	}
	return p
}
