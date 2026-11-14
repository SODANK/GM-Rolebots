package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	evedb "gurimoon/db"
)

// Requisition buy trips as order-book sweeps (sweep.go). The buyer plans a route over every
// line it holds: the cheapest stock under each line's cap first, seller after seller, until the
// lines, the hold or the stop count run out, and only then flies home.
// The buyer never pays from its own wallet: each stop tops the advance up from the corporation
// (fundReq) and buys only what the advance covers at that counter's real prices (affordableQty).

// reqSweepScope is the region set a buyer searches for its lines: its own
// neighbourhood (two regions out, the band posBuyTrip has always used) plus
// the region around every office the goods are bound for and every pinned
// seller (posreq_reach.go says why). nil = unscoped.
func (m *Mod) reqSweepScope(ctx context.Context, b *botRun, lines []requisition) map[int64]bool {
	if !Residency() || !HomeLoop() {
		return nil
	}
	scope := map[int64]bool{}
	for id := range m.regionScope(ctx, b, bandNear2) {
		scope[id] = true
	}
	seen := map[int64]bool{}
	for _, r := range lines {
		for _, st := range []int64{r.DestStationID, r.SrcStationID} {
			if st == 0 || seen[st] {
				continue
			}
			seen[st] = true
			if reg := m.stationRegionID(ctx, st); reg != 0 {
				for id := range m.scopeRegions(ctx, reg, bandAdjacent) {
					scope[id] = true
				}
			}
		}
	}
	if len(scope) == 0 {
		return nil
	}
	return scope
}

// reqLineUrgent is whether a line may pay the urgent markup: its deadline is
// inside half the patient window, or it is a tower's first fuel. Pure.
func reqLineUrgent(r requisition, now int64) bool {
	return r.Deadline-now < reqPatientSec/2 || strings.EqualFold(r.Note, "first fuel")
}

// reqLineCap is the most a unit of the line may cost on a sweep: a trade run
// its MaxPrice (the margin it was priced on), anything else the reference
// times the markup, never past what the line's budget can still pay a unit.
func (m *Mod) reqLineCap(ctx context.Context, r requisition, cfg sweepConfig, ref float64, now int64) float64 {
	if r.Requester == reqTrade {
		return sweepCap(0, 1, r.MaxPrice)
	}
	markup := cfg.Markup
	if reqLineUrgent(r, now) {
		markup = cfg.UrgentMarkup
	}
	var budgetUnit float64
	if left := r.left(); left > 0 && r.Budget > 0 {
		budgetUnit = (r.Budget - r.Wage - r.Spent) / float64(left)
		if budgetUnit <= 0 {
			budgetUnit = 0.01 // spent out: nothing under the cap
		}
	}
	return sweepCap(ref, markup, budgetUnit)
}

// reqSweepLevels reads the ladder for a set of types inside the buyer's
// sweep scope, one station-filter pass at a time (the pilot's soft
// preferences first, then everyone but the despised), stopping at the first
// pass that offers anything.
func (m *Mod) reqSweepLevels(ctx context.Context, b *botRun, typeIDs []int64, scope map[int64]bool,
	reach map[int64]int, try func([]sweepLevel) bool) error {
	look := rulesForBot(b)
	look.Regions, look.Band = scope, bandNear2
	passes := []stationFilter{{}}
	if b.Profile != nil {
		passes = stationFilterPasses(b.Profile, b.Tune)
	}
	for _, f := range passes {
		levels, err := m.sweepLevels(ctx, b, typeIDs, f.scoped(look), reach)
		if err != nil {
			return err
		}
		if try(levels) {
			return nil
		}
	}
	return nil
}

// reqShareLine is one claim on a trip's hold: a line (or a type) and the units it still wants.
type reqShareLine struct {
	ID, Want int64
	UnitM3   float64
}

// reqHoldShares splits a hold that cannot carry every claim: each claim gets the same fraction of
// what it wants, floored, and at least one unit. A hold of 0 or less, or one that fits everything,
// gives each claim all it wants. See docs/STARBASES.md#requisitions--the-procurer-posreqgo. Pure.
func reqHoldShares(claims []reqShareLine, hold float64) map[int64]int64 {
	out := map[int64]int64{}
	var total float64
	for _, c := range claims {
		if c.Want > 0 {
			out[c.ID] += c.Want
			total += float64(c.Want) * c.UnitM3
		}
	}
	if hold <= 0 || total <= hold {
		return out
	}
	f := hold / total
	for id := range out {
		out[id] = 0
	}
	for _, c := range claims {
		if c.Want > 0 {
			out[c.ID] += max(1, int64(math.Floor(float64(c.Want)*f)))
		}
	}
	return out
}

// reqSweepPlan plans a sweep for some of a buyer's lines from where it
// stands: the caps, the route, and the levels it was planned on.
type reqSweepResult struct {
	Plan   sweepPlan
	Caps   map[int64]float64 // reqID → cap
	ByType map[int64][]int64 // typeID → reqIDs, in deadline order
	// Avail is, per type, the units in reach under its cap at all: it separates "nobody sells it
	// under the cap" from "the hold ran out".
	Avail map[int64]int64
	Refs  map[int64]float64
}

func (m *Mod) reqSweepPlan(ctx context.Context, b *botRun, lines []requisition, remaining map[int64]int64,
	hold float64, exclude map[int64]bool, caps map[int64]float64, stopsLeft int) (reqSweepResult, error) {
	cfg := m.sweepSettings(ctx)
	now := clock.Unix()
	res := reqSweepResult{Caps: map[int64]float64{}, ByType: map[int64][]int64{}, Avail: map[int64]int64{},
		Refs: map[int64]float64{}}
	var typeIDs []int64
	for _, r := range lines {
		if remaining[r.ReqID] <= 0 {
			continue
		}
		if len(res.ByType[r.TypeID]) == 0 {
			typeIDs = append(typeIDs, r.TypeID)
		}
		res.ByType[r.TypeID] = append(res.ByType[r.TypeID], r.ReqID)
	}
	if len(typeIDs) == 0 {
		return res, nil
	}
	reach, err := m.reachFrom(ctx, b.SystemID, rulesForBot(b))
	if err != nil {
		return res, err
	}
	scope := m.reqSweepScope(ctx, b, lines)
	dist := m.sweepDist(ctx, 60)
	// A Trade desk line's pinned seller is read live: the run's margin was priced there, and the
	// cached ladder can be older than the ask the desk just checked.
	pinned := map[[2]int64]bool{}
	var live []sweepLevel
	for _, r := range lines {
		if r.SrcStationID == 0 || remaining[r.ReqID] <= 0 || exclude[r.SrcStationID] || pinned[[2]int64{r.TypeID, r.SrcStationID}] {
			continue
		}
		pinned[[2]int64{r.TypeID, r.SrcStationID}] = true
		book, err := m.localAskBook(ctx, b.CharID, r.SrcStationID, r.TypeID)
		if err != nil {
			continue
		}
		sys := m.stationSystemID(ctx, r.SrcStationID)
		j, ok := reach[sys]
		if sys == b.SystemID {
			j, ok = 0, true
		}
		if !ok {
			continue
		}
		for _, l := range book {
			live = append(live, sweepLevel{TypeID: r.TypeID, StationID: r.SrcStationID, SystemID: sys,
				Price: l.Price, Vol: l.Vol, Jumps: j})
		}
	}
	withLive := func(levels []sweepLevel) []sweepLevel {
		if len(pinned) == 0 {
			return levels
		}
		out := make([]sweepLevel, 0, len(levels)+len(live))
		for _, l := range levels {
			if !pinned[[2]int64{l.TypeID, l.StationID}] {
				out = append(out, l)
			}
		}
		return append(out, live...)
	}
	err = m.reqSweepLevels(ctx, b, typeIDs, scope, reach, func(levels []sweepLevel) bool {
		levels = withLive(levels)
		// Caps: per line, kept from the trip's start when it has one, so a
		// later stop does not re-derive a different number off a thinner book.
		need := map[int64]int64{}
		for _, r := range lines {
			need[r.TypeID] += remaining[r.ReqID]
		}
		var sl []sweepLine
		for _, t := range typeIDs {
			ref := m.sweepRef(ctx, t, need[t], levels)
			res.Refs[t] = ref
			lineCap := math.Inf(1)
			for _, id := range res.ByType[t] {
				var r requisition
				for _, x := range lines {
					if x.ReqID == id {
						r = x
					}
				}
				c, ok := caps[id]
				if !ok {
					c = m.reqLineCap(ctx, r, cfg, ref, now)
				}
				res.Caps[id] = c
				// Lines of one type share the trip's purchases, so the type is
				// capped at its strictest line.
				lineCap = math.Min(lineCap, c)
			}
			sl = append(sl, sweepLine{TypeID: t, Need: need[t], UnitM3: m.typeVolume(ctx, t), Cap: lineCap})
		}
		res.Avail = map[int64]int64{}
		for _, l := range levels {
			for _, ln := range sl {
				if ln.TypeID == l.TypeID && l.Price <= ln.Cap && !exclude[l.StationID] {
					res.Avail[l.TypeID] += l.Vol
				}
			}
		}
		// The hold is shared over what is in reach under the caps (reqHoldShares). A type with nothing in
		// reach keeps its need: it takes no hold, and planSweep still reports whether it is priced out.
		claims := make([]reqShareLine, 0, len(sl))
		for _, ln := range sl {
			claims = append(claims, reqShareLine{ID: ln.TypeID, Want: min(ln.Need, res.Avail[ln.TypeID]), UnitM3: ln.UnitM3})
		}
		shares := reqHoldShares(claims, hold)
		for i := range sl {
			if s := shares[sl[i].TypeID]; s > 0 {
				sl[i].Need = s
			}
		}
		res.Plan = planSweep(levels, sl, sweepOpts{
			Hold: hold, MaxStops: stopsLeft, MaxHop: cfg.MaxExtraJumps,
			FromSystem: b.SystemID, Dist: dist,
			JumpISK: sweepJumpISK(b.Tune.IskJumpSlope, sl, res.Refs), Exclude: exclude,
		})
		return len(res.Plan.Stops) > 0
	})
	return res, err
}

// posSweepTrip starts one sweep over every line this buyer holds.
// handled = the trip began.
func (m *Mod) posSweepTrip(ctx context.Context, b *botRun, reqs []requisition) (bool, error) {
	cfg := m.sweepSettings(ctx)
	var lines []requisition
	remaining := map[int64]int64{}
	for _, r := range reqs {
		if r.Status != "buying" || r.BuyerID != b.CharID || r.left() <= 0 {
			continue
		}
		if len(lines) >= reqTripMaxLines {
			break
		}
		lines = append(lines, r)
		remaining[r.ReqID] = r.left()
	}
	if len(lines) == 0 {
		return false, nil
	}
	hold := m.reqHaulerHold(ctx, b.CharID)
	res, err := m.reqSweepPlan(ctx, b, lines, remaining, hold, nil, nil, cfg.MaxStops)
	if err != nil {
		return false, err
	}
	// Lines the sweep cannot touch (nothing under the cap in reach) go back to the board; a trade
	// run is off instead. A line only the hold or the stop count left out stays with this buyer.
	planned := map[int64]int64{} // typeID to units on the route
	for _, s := range res.Plan.Stops {
		for _, buy := range s.Buys {
			planned[buy.TypeID] += buy.Qty
		}
	}
	var trip []int64
	for i := range lines {
		r := &lines[i]
		if planned[r.TypeID] > 0 {
			avg := 0.0
			var q int64
			var spend float64
			for _, s := range res.Plan.Stops {
				for _, buy := range s.Buys {
					if buy.TypeID == r.TypeID {
						q += buy.Qty
						spend += buy.Spend
					}
				}
			}
			if q > 0 {
				avg = spend / float64(q)
			}
			if err := m.fundReq(ctx, r, avg); err != nil {
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
			continue
		}
		if res.Avail[r.TypeID] > 0 {
			continue // sold under the cap, just not on this route: next trip
		}
		if r.Requester == reqTrade {
			m.closeReq(ctx, r, "void", "no seller in reach at or under the "+iskString(r.MaxPrice)+" the desk allowed")
			continue
		}
		m.noteMarketDemand(ctx, b, []ShopItem{{TypeID: r.TypeID, Name: m.typeNameOr(ctx, r.TypeID), Qty: r.left()}})
		if res.Plan.CapBound[r.TypeID] {
			m.reqDecline(ctx, b.CharID, r, fmt.Sprintf("nothing in reach at or under its %s cap", iskString(res.Caps[r.ReqID])))
			reqSnubs.Store(r.ReqID, b.CharID)
			m.reclaimAdvance(ctx, r)
			_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET status = 'open', buyerID = 0 WHERE reqID = ? AND buyerID = ?`,
				r.ReqID, b.CharID)
			r.Status, r.BuyerID = "open", 0
			continue
		}
		m.declineNoSeller(ctx, b.CharID, r)
	}
	if len(trip) == 0 {
		return false, nil
	}
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET tripAt = ? WHERE reqID IN (`+int64CSV(trip)+`)`, clock.Unix())
	first := res.Plan.Stops[0]
	st := b.State
	st.Activity = ActIndustry
	st.Data = idleData(st.Data)
	st.Data.PosTripReqs = trip
	st.Data.PosTripStation = first.StationID
	st.Data.PosSweep = true
	st.Data.PosSweepCaps = map[int64]float64{}
	for _, id := range trip {
		if c, ok := res.Caps[id]; ok && !math.IsInf(c, 1) {
			st.Data.PosSweepCaps[id] = c
		}
	}
	m.logBot(b.CharID, logInfo, "corp", "buying %d requisition line(s) for its corporation: %d unit(s) over %d stop(s) for ~%s, first at %s",
		len(trip), res.Plan.Units, len(res.Plan.Stops), iskString(res.Plan.Spend), m.stationLabel(ctx, first.StationID))
	return true, m.beginTravel(ctx, b, first.SystemID, first.StationID, string(TaskPosBuy))
}

// affordableQty is how many of want units a buyer can take off a book
// (cheapest first, nothing above ceiling) with cash, and what they cost.
// Pure.
func affordableQty(book []askLevel, want int64, ceiling, cash float64) (int64, float64) {
	levels := append([]askLevel(nil), book...)
	sort.Slice(levels, func(i, j int) bool { return levels[i].Price < levels[j].Price })
	var got int64
	var spend float64
	for _, l := range levels {
		if got >= want || l.Price <= 0 {
			break
		}
		if ceiling > 0 && !math.IsInf(ceiling, 1) && l.Price > ceiling {
			break
		}
		q := min(l.Vol, want-got)
		if !math.IsInf(cash, 1) {
			if afford := int64(math.Floor((cash - spend) / l.Price)); afford < q {
				q = max(0, afford)
			}
		}
		if q <= 0 {
			break
		}
		got += q
		spend += float64(q) * l.Price
	}
	return got, spend
}

// posSweepBuyHere is one stop of a sweep: buy every trip line this counter
// holds under its cap, as far as hold and advance go, then pick the next
// seller or head for delivery. Returns true when it set the pilot travelling.
func (m *Mod) posSweepBuyHere(ctx context.Context, b *botRun, reqs []requisition) (bool, error) {
	st := b.State
	d := &st.Data
	cfg := m.sweepSettings(ctx)
	var room float64
	if d.PosSweepStops == 0 {
		room = m.boardHauler(ctx, b, "its corporation's supplies")
	} else {
		room = m.shipHoldRoom(ctx, b)
	}
	if d.PosSweepGot == nil {
		d.PosSweepGot = map[int64]int64{}
	}
	here := b.StationID
	// Each line buys at most its share (reqHoldShares) of the trip's hold: the room left plus what
	// this trip already loaded. The counter walks the lines in reqID order.
	var claims []reqShareLine
	tripHold := room
	for _, r := range reqs {
		if r.Status != "buying" || r.BuyerID != b.CharID {
			continue
		}
		vol := math.Max(m.typeVolume(ctx, r.TypeID), 0.01)
		claims = append(claims, reqShareLine{ID: r.ReqID, Want: r.left(), UnitM3: vol})
		tripHold += float64(d.PosSweepGot[r.ReqID]) * vol
	}
	share := reqHoldShares(claims, tripHold)
	bought, units := 0, int64(0)
	var spentHere float64
	// What this counter was asked for and gave, for the shopper-demand ledger (stationdemand.go).
	// An advance that could not cover the price, and a trade desk line, are not a want.
	var stop []shopLine
	defer func() { m.noteShopStop(ctx, here, stop) }()
	noteLine := func(r *requisition, l shopLine) {
		if r.Requester != reqTrade {
			stop = append(stop, l)
		}
	}
	for i := range reqs {
		r := &reqs[i]
		if r.Status != "buying" || r.BuyerID != b.CharID {
			continue
		}
		want := min(r.left(), share[r.ReqID]) - d.PosSweepGot[r.ReqID]
		if want <= 0 {
			continue
		}
		vol := math.Max(m.typeVolume(ctx, r.TypeID), 0.01)
		if fit := int64(room / vol); fit < want {
			want = fit
		}
		if want <= 0 {
			continue
		}
		ceiling, ok := d.PosSweepCaps[r.ReqID]
		if !ok {
			ceiling = math.Inf(1)
		}
		book, err := m.localAskBook(ctx, b.CharID, here, r.TypeID)
		if err != nil || len(book) == 0 {
			noteTillEmpty(botScope(b), r.TypeID, here)
			noteLine(r, shopLine{TypeID: r.TypeID, Asked: want})
			continue
		}
		// Top the advance up to what this counter will cost, then buy only
		// what the advance covers: the buyer never pays from its own wallet.
		qty, cost := affordableQty(book, want, ceiling, math.Inf(1))
		if qty <= 0 {
			noteTillEmpty(botScope(b), r.TypeID, here)
			noteLine(r, shopLine{TypeID: r.TypeID, Asked: want})
			continue
		}
		if have := r.Advance - r.Spent; have < cost {
			_ = m.fundReq(ctx, r, cost/float64(qty))
		}
		qty, _ = affordableQty(book, qty, ceiling, r.Advance-r.Spent)
		if qty <= 0 {
			m.reqDecline(ctx, b.CharID, r, "its advance could not cover the price here")
			continue
		}
		_, filled, spent, err := m.buyAtStationCapped(ctx, b, here, r.TypeID, qty, ceiling, "corporation supplies")
		if err == nil || !isMoneyRefusal(err) {
			noteLine(r, shopLine{TypeID: r.TypeID, Asked: want, Filled: max(filled, 0)})
		}
		if err != nil || filled <= 0 {
			continue
		}
		moved, _ := m.moveUnits(ctx, b.CharID, here, 4, r.TypeID, filled, b.ShipID, 5, b.CorpID)
		m.settleCorpBuyRemainder(ctx, b, here, r.TypeID, filled-moved)
		room -= float64(moved) * vol
		r.Spent += spent
		// What is aboard is what this sweep delivers; a remainder that did
		// not fit went to the office here (settleCorpBuyRemainder).
		d.PosSweepGot[r.ReqID] += moved
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_requisition SET spent = spent + ? WHERE reqID = ?`, spent, r.ReqID)
		m.spendCommit(ctx, "req", r.ReqID, spent)
		m.corpBook(ctx, r.CorpID, reqLedgerLine(r), -spent, r.TypeID, fmt.Sprintf("%d× %s (%s)", filled, m.typeNameOr(ctx, r.TypeID), r.Requester))
		m.towerBookReq(ctx, r, spent, filled)
		bought++
		units += filled
		spentHere += spent
	}
	d.PosSweepStops++
	d.PosSweepVisited = append(d.PosSweepVisited, here)
	if bought > 0 {
		m.logBot(b.CharID, logInfo, "corp", "stop %d: bought %d unit(s) across %d line(s) for %s at %s",
			d.PosSweepStops, units, bought, iskString(spentHere), m.stationLabel(ctx, here))
	}
	// Next seller, while lines, hold and stops remain.
	d.PosTripStation = 0
	if d.PosSweepStops < cfg.MaxStops && room > 0 {
		remaining := map[int64]int64{}
		var lines []requisition
		for _, r := range reqs {
			if r.Status != "buying" || r.BuyerID != b.CharID {
				continue
			}
			if left := min(r.left(), share[r.ReqID]) - d.PosSweepGot[r.ReqID]; left > 0 {
				remaining[r.ReqID] = left
				lines = append(lines, r)
			}
		}
		if len(lines) > 0 {
			exclude := map[int64]bool{}
			for _, s := range d.PosSweepVisited {
				exclude[s] = true
			}
			res, err := m.reqSweepPlan(ctx, b, lines, remaining, room, exclude, d.PosSweepCaps, cfg.MaxStops-d.PosSweepStops)
			if err != nil {
				return false, err
			}
			if len(res.Plan.Stops) > 0 {
				next := res.Plan.Stops[0]
				d.PosTripStation = next.StationID
				if err := saveState(ctx, m.db, st); err != nil {
					return false, err
				}
				m.logBot(b.CharID, logInfo, "corp", "next stop %s for %d more unit(s)",
					m.stationLabel(ctx, next.StationID), res.Plan.Units)
				return true, m.beginTravel(ctx, b, next.SystemID, next.StationID, string(TaskPosBuy))
			}
		}
	}
	if err := saveState(ctx, m.db, st); err != nil {
		return false, err
	}
	return false, nil
}

// sweepTripGotAnything is whether a sweep bought anything on any stop.
func sweepTripGotAnything(got map[int64]int64) bool {
	for _, n := range got {
		if n > 0 {
			return true
		}
	}
	return false
}
