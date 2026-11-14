package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/rand"
)

// Reach: how far a rolebot sees and how far its ship goes. A bot reads the
// whole market with no search radius; only the route bounds it. Distance is
// priced, never forbidden: each jump adds jumpPriceCost to a purchase and
// takes the same off a sale. The pilot's home region gets a small edge
// (residency.go, homeRegionSet).

const (
	// unlimitedJumps means no depth limit: the search ends when it runs out of
	// reachable systems.
	unlimitedJumps = 1 << 30

	// jumpPriceCost is what one jump costs as a fraction of the price weighed.
	// At 2% a jump, a 10% cheaper hull is worth five jumps.
	jumpPriceCost = 0.02

	// sellNetFloor keeps a very long haul from pricing out to nothing.
	sellNetFloor = 0.05

	// preferredRegionEdge is the default discount the pilot's home region gets
	// when two stops are close. The pilot's own value is Tuning.PreferredEdge.
	// The region set is derived (homeRegionSet), never stored.
	preferredRegionEdge = 0.10

	// tripBaseJumps is a shopping trip's fixed cost (undock, dock, market walk),
	// so a two-item station next door does not always beat a full-list one.
	tripBaseJumps = 3.0
)

// reachFrom returns every system the bot may legally fly to from `from`, with
// its jump distance. There is no radius. Under residency (residency.go) the
// rules may carry a region scope that shrinks the sweep; the ladder widens it.
func (m *Mod) reachFrom(ctx context.Context, from int64, r travelRules) (map[int64]int, error) {
	return m.systemsWithin(ctx, from, r, unlimitedJumps)
}

// marketLook is one rung of the residency ladder: the scan, and the rules and
// horizon it was run under, so the caller can rank against the same numbers.
type marketLook struct {
	Scan  marketScan
	Rules travelRules
	Reach map[int64]int
	// Widened is set when the pilot looked past its own band. A roster where
	// every pilot does it every pass means the scope is set wrong.
	Widened bool
}

// shopLadder is how a resident shops: it scans its own scope first and widens
// one band at a time until take returns true. softNote is logged when the
// friendly-only pass came up empty. The residency clause is scoped here so it
// joins scanStops' snapshot key (marketsnap.go).
func (m *Mod) shopLadder(ctx context.Context, b *botRun, typeIDs []int64, buying bool,
	prefer map[int64]bool, band scopeBand, softNote string,
	take func(scan marketScan) bool) (marketLook, error) {

	var out marketLook
	if len(typeIDs) == 0 {
		return out, nil
	}
	// Buying is capped by the home loop; selling climbs the full ladder.
	// See docs/comment-notes-b16.md#shop-ladder-scope.
	start, capBand := shopClimb(buying, band)
	if buying {
		capBand = frontBuyCap(b.CharID, capBand) // a front's own region may sell nothing (corpfront.go)
	}
	// startBand skips a band this pilot just found empty (residency.go). The skip
	// is clamped to the cap so a capped buyer is not promoted past its fence.
	sb := startBand(b.CharID, start)
	if sb > capBand {
		sb = capBand
	}
	r := m.economicRules(ctx, b, sb)
	for {
		reach, err := m.reachFrom(ctx, b.SystemID, r)
		if err != nil {
			return out, err
		}
		out.Rules, out.Reach = r, reach
		for _, f := range stationFilterPasses(b.Profile, b.Tune) {
			scan, err := m.scanMarket(ctx, typeIDs, buying, f.scoped(r), reach, prefer,
				botScope(b))
			if err != nil {
				return out, err
			}
			out.Scan = scan
			if take(scan) {
				return out, nil
			}
			if f.Soft && softNote != "" {
				m.logBot(b.CharID, logInfo, "standings", "%s", softNote)
			}
		}
		wider, ok := m.widen(ctx, b, r)
		if !ok || wider.Band > capBand {
			// The ceiling: a capped buy climb ends at the adjacent band and reports
			// what it found there.
			out.Scan.Fenced = ok && wider.Band > capBand
			if out.Scan.Fenced {
				// Whether the fence or the market is to blame, for noSellerReason.
				out.Scan.Elsewhere, out.Scan.Anywhere = m.soldBeyond(ctx, typeIDs, r.Regions)
			}
			return out, nil
		}
		// Logged once per pilot per band per TTL, not once per pass.
		if noteScopeFail(b.CharID, r.Band) {
			m.logBot(b.CharID, logInfo, "residency",
				"nothing it can use in %s — looking further out (%s)", m.scopeNote(ctx, r), wider.Band)
		}
		r = wider
		out.Widened = true
	}
}

// shopClimb is the band arithmetic of a shopLadder climb: where it opens and
// the rung it must not pass. Selling runs the full ladder. Buying opens at the
// universe with the home loop off, and is capped at the adjacent band with it on.
func shopClimb(buying bool, band scopeBand) (start, cap scopeBand) {
	start, cap = band, bandUniverse
	if buying {
		if HomeLoop() {
			cap = bandAdjacent
			// A caller asking for more reach than the fence keeps it (bandNear2 for a
			// blueprint, industry.go). The climb still opens at the ordinary band.
			if band > cap {
				cap, start = band, bandAdjacent
			}
		} else {
			start = bandUniverse
		}
	}
	return start, cap
}

// marketStop is one station, anywhere in the universe, with a live order
// for something the bot cares about.
type marketStop struct {
	TypeID    int64
	StationID int64
	SystemID  int64
	RegionID  int64
	Price     float64 // best price on that side at that station
	Volume    int64   // whole book on that side, an upper bound on the fill
	Jumps     int
	Preferred bool
	// Crafted marks a buy-side stop whose best ask is bot- or player-made gear,
	// not an NPC seeder's. Set by scanStops, never on a bid; buyCost applies craftedEdge.
	Crafted bool
}

// craftedEdge is the discount a buyer gives crafted gear: 8%, about four jumps
// at the default slope. The reach cap (shopClimb) still bounds the trip, so it
// sends buyers a few jumps further for bot stock, never across the map.
const craftedEdge = 0.08

// buyCost is what this stop costs the bot: the ask plus the flying. The
// per-jump slope (Tuning.IskJumpSlope) and the preferred-region edge are the
// pilot's own. A crafted stop takes craftedEdge off on top.
func (s marketStop) buyCost(tn Tuning) float64 {
	c := s.Price * (1 + tn.IskJumpSlope*float64(s.Jumps))
	if s.Preferred {
		c *= 1 - tn.PreferredEdge
	}
	if s.Crafted {
		c *= 1 - craftedEdge
	}
	return c
}

// sellNet is what this stop is really worth: the bid minus the flying.
func (s marketStop) sellNet(tn Tuning) float64 {
	f := 1 - tn.IskJumpSlope*float64(s.Jumps)
	if f < sellNetFloor {
		f = sellNetFloor
	}
	n := s.Price * f
	if s.Preferred {
		n *= 1 + tn.PreferredEdge
	}
	return n
}

// marketScan is the answer to "who trades this, anywhere".
type marketScan struct {
	Stops []marketStop // reachable, distance-annotated
	// Blocked counts stations that exist but sit behind the bot's security
	// limits: "the market has none" versus "the rules won't let it go get one".
	Blocked     int
	BlockedNear string // a region name from the blocked pile, for the log
	// Fenced marks a scan that ended at the home loop's buy cap (shopClimb), not
	// at the edge of the map: the item is missing from the pilot's region and
	// its neighbours, not from the universe.
	Fenced bool
	// Set with Fenced, from the whole sell book (soldBeyond): Elsewhere when some region
	// outside the scanned ones lists the item, Anywhere when any region does. Fenced with
	// neither means the market has none and the fence did not cause it.
	Elsewhere bool
	Anywhere  bool
}

// soldBeyond reports, for a buy scan that stopped at the fence, whether any of the types
// has a live sell order outside the scanned regions, and whether any has one at all. It
// reads the roster's shared sell-book snapshot (allSellerStations). An unreadable book
// reads as sold beyond: the fence is then the best explanation there is.
func (m *Mod) soldBeyond(ctx context.Context, typeIDs []int64, scope map[int64]bool) (elsewhere, anywhere bool) {
	all, err := m.allSellerStations(ctx)
	if err != nil {
		return true, true
	}
	return soldBeyondIn(all, typeIDs, scope)
}

// soldBeyondIn is soldBeyond over a sell book already read. Pure.
func soldBeyondIn(all map[int64][]sysStations, typeIDs []int64, scope map[int64]bool) (elsewhere, anywhere bool) {
	for _, id := range typeIDs {
		for _, c := range all[id] {
			anywhere = true
			if !scope[c.RegionID] {
				return true, true
			}
		}
	}
	return false, anywhere
}

// scanMarket runs one query over the whole market_orders table, with no system,
// region or radius filter, and returns the best price per station for each type,
// annotated with the jumps the bot would fly.
func (m *Mod) scanMarket(ctx context.Context, typeIDs []int64, buying bool,
	f stationFilter, reach map[int64]int, prefer map[int64]bool,
	sc intelScope) (marketScan, error) {

	var out marketScan
	if len(typeIDs) == 0 {
		return out, nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return out, err
	}
	// The read is shared across the roster (marketsnap.go). The horizon,
	// preferred regions and blocked count are this pilot's, applied here.
	stops, err := m.scanStops(ctx, typeIDs, buying, f)
	if err != nil {
		return out, err
	}

	var blockedRegion int64
	blocked := map[int64]bool{}
	for _, s := range stops {
		// A till the roster just found empty of this type is not offered again for
		// a while (emptytill.go). Buying only.
		if buying && tillIsEmpty(sc, s.TypeID, s.StationID) {
			continue
		}
		j, ok := reach[s.SystemID]
		if !ok {
			blocked[s.StationID] = true
			if blockedRegion == 0 {
				blockedRegion = u.meta[s.SystemID].RegionID
			}
			continue
		}
		s.Jumps = j
		s.RegionID = u.meta[s.SystemID].RegionID
		s.Preferred = prefer[s.RegionID]
		out.Stops = append(out.Stops, s)
	}
	out.Blocked = len(blocked)
	if blockedRegion != 0 {
		out.BlockedNear = m.nameOfRegion(ctx, blockedRegion)
	}
	return out, nil
}

// noSellerReason explains a failed scan in terms an operator can act on: an
// empty market and the bot's own rules fencing it off have different fixes.
// `what` reads as a verb phrase ("sells the hull Bantam").
func (m *Mod) noSellerReason(ctx context.Context, scan marketScan, what string) string {
	if scan.Blocked > 0 {
		where := ""
		if scan.BlockedNear != "" {
			where = " (one of them in " + scan.BlockedNear + ")"
		}
		return fmt.Sprintf(
			"%d station(s) in the database %s, but none is reachable under this pilot's travel rules%s — "+
				"they sit in sealed pockets of the map, behind an outlaw's security ceiling, "+
				"or outside this activity's own security floor",
			scan.Blocked, what, where)
	}
	if scan.Fenced {
		switch {
		case scan.Elsewhere:
			// The item exists, but the home loop's buy cap (shopClimb) ends the search
			// at the pilot's neighbours.
			return "nothing in its own region or its neighbours " + what +
				" — other regions do, and the home loop's buy cap keeps its shopping at home"
		case scan.Anywhere:
			// Listed inside the fence only, by sellers the scan passed over.
			return "nothing in its own region or its neighbours " + what +
				" that it will buy from — the only sellers are ones it refuses or tills it just found empty"
		}
		// The climb stopped at the fence, and nothing lay beyond it either.
		return "nothing anywhere in the market " + what + " — not on sale in any region"
	}
	return "nothing anywhere in the market " + what
}

// bestBuy is the cheapest place to buy one type once the flying is priced
// in. Ties break randomly so a whole roster of bots does not queue at the
// same station.
func bestBuy(stops []marketStop, tn Tuning) *marketStop {
	var best *marketStop
	for i := range stops {
		s := &stops[i]
		if s.Price <= 0 {
			continue
		}
		if best == nil || s.buyCost(tn) < best.buyCost(tn) ||
			(s.buyCost(tn) == best.buyCost(tn) && rand.Intn(2) == 0) {
			best = s
		}
	}
	return best
}

// bestSell is the best place to unload one type once the haul is priced in.
func bestSell(stops []marketStop, tn Tuning) *marketStop {
	var best *marketStop
	for i := range stops {
		s := &stops[i]
		if s.Price <= 0 {
			continue
		}
		if best == nil || s.sellNet(tn) > best.sellNet(tn) ||
			(s.sellNet(tn) == best.sellNet(tn) && rand.Intn(2) == 0) {
			best = s
		}
	}
	return best
}

// supplyStop is a station weighed as a shopping stop rather than as a
// single price: how much of the outstanding list it clears, and what the
// trip to it costs.
type supplyStop struct {
	StationID int64
	SystemID  int64
	Jumps     int
	// How much of the outstanding list this station can fill, in lines.
	// Fractional: a station holding a tenth of a line has not covered it. See
	// bestSupplyStopFor.
	Covered   float64
	Outlay    float64 // what this stop can deliver, at this station's prices
	// premW is the sum over lines of share times price over the scan's cheapest
	// price: premW / Covered is how dear this stop is (bestSupplyStopCapped).
	premW     float64
	preferred bool
	score     float64
}

const (
	// buyTripNearJumps is how far a shopping trip may fly on any stock at all.
	// Past it, the stop has to be worth the flying (buyTripMinCover).
	buyTripNearJumps = 5
	// buyTripMinCover is the least of the bill, in lines (supplyStop.Covered), that
	// a stop beyond buyTripNearJumps must hand over. A tiny cover scores badly but
	// still wins when it is the only stop.
	buyTripMinCover = 0.5
	// industryDeadStopSec is how long a counter that sold a project nothing
	// stays off that project's shopping list (StateData.IndDeadStops).
	industryDeadStopSec = 2 * 3600
)

// supplyStopWorthFlying is the distance test on a shopping stop: anything
// near, and only real stock far. Pure, so the harness pins it.
func supplyStopWorthFlying(s *supplyStop) bool {
	if s == nil {
		return false
	}
	return s.Jumps <= buyTripNearJumps || s.Covered >= buyTripMinCover
}

// withoutDeadStops drops every stop at a station still on the dead list at
// now. The input slice is left untouched. Pure, so the harness pins it.
func withoutDeadStops(stops []marketStop, dead map[int64]int64, now int64) []marketStop {
	if len(dead) == 0 {
		return stops
	}
	out := make([]marketStop, 0, len(stops))
	for _, s := range stops {
		if until, ok := dead[s.StationID]; ok && until > now {
			continue
		}
		out = append(out, s)
	}
	return out
}

// pruneDeadStops forgets every dead stop whose time is up.
func pruneDeadStops(dead map[int64]int64, now int64) {
	for id, until := range dead {
		if until <= now {
			delete(dead, id)
		}
	}
}

// bestSupplyStop is the quantity-blind form: any live ask counts as covering
// its line. Kept for callers with no bill in hand.
func bestSupplyStop(stops []marketStop, tn Tuning) *supplyStop {
	return bestSupplyStopFor(stops, nil, tn)
}

// bestSupplyStopFor picks the next stop on a shopping trip: the station that
// clears the most of the list for the least flying. want is the units each line
// still needs; Covered is min(1, volume/needed) per line, summed. With want nil
// or a depth unknown, a line counts whole.
func bestSupplyStopFor(stops []marketStop, want map[int64]int64, tn Tuning) *supplyStop {
	return pickSupplyStop(stops, want, nil, tn)
}

// bestSupplyStopCapped is bestSupplyStopFor with price in the picture (sweep.go).
// A stop whose ask for a line is over that line's cap does not cover it, and a
// station's score is multiplied by how dear its covered stock is against the
// cheapest stop. nil caps is bestSupplyStopFor exactly.
func bestSupplyStopCapped(stops []marketStop, want map[int64]int64, caps map[int64]float64, tn Tuning) *supplyStop {
	if caps == nil {
		return bestSupplyStopFor(stops, want, tn)
	}
	kept := make([]marketStop, 0, len(stops))
	minPrice := map[int64]float64{}
	for _, s := range stops {
		if c, ok := caps[s.TypeID]; ok && c > 0 && s.Price > c {
			continue
		}
		kept = append(kept, s)
		// The pilot's own stock enters at a token price (ownstock.go): it is free,
		// so it must not make market stops look dear.
		if s.Price == ownStockNominalPrice {
			continue
		}
		if s.Price > 0 && (minPrice[s.TypeID] == 0 || s.Price < minPrice[s.TypeID]) {
			minPrice[s.TypeID] = s.Price
		}
	}
	return pickSupplyStop(kept, want, minPrice, tn)
}

// supplyCapsFromStops is the per-line cap for a shopping list: the larger of
// basePrice and the depth price of the line's need across the stops, times
// markup (sweepRef's rule), with each station's cheapest ask as its book.
func (m *Mod) supplyCapsFromStops(ctx context.Context, stops []marketStop, want map[int64]int64, markup float64) map[int64]float64 {
	levels := make([]sweepLevel, 0, len(stops))
	for _, s := range stops {
		if s.Price == ownStockNominalPrice {
			continue // the pilot's own hangars are not a price
		}
		levels = append(levels, sweepLevel{TypeID: s.TypeID, StationID: s.StationID, Price: s.Price, Vol: s.Volume})
	}
	caps := make(map[int64]float64, len(want))
	for t, n := range want {
		caps[t] = sweepCap(m.sweepRef(ctx, t, n, levels), markup, 0)
	}
	return caps
}

// pickSupplyStop is the scorer behind both: minPrice nil leaves price out.
func pickSupplyStop(stops []marketStop, want map[int64]int64, minPrice map[int64]float64, tn Tuning) *supplyStop {
	byStation := map[int64]*supplyStop{}
	seen := map[[2]int64]bool{}
	for i := range stops {
		s := &stops[i]
		if s.Price <= 0 {
			continue
		}
		key := [2]int64{s.StationID, s.TypeID}
		if seen[key] {
			continue
		}
		seen[key] = true
		// What this station can really deliver on this line, in units, and the
		// share of the line that is.
		share, units := 1.0, float64(0)
		if need := want[s.TypeID]; need > 0 && s.Volume > 0 {
			units = math.Min(float64(s.Volume), float64(need))
			share = units / float64(need)
		}
		if share <= 0 {
			continue
		}
		st, ok := byStation[s.StationID]
		if !ok {
			// Preference is a property of the system, so it is safe to
			// read off any one of that station's rows.
			st = &supplyStop{StationID: s.StationID, SystemID: s.SystemID,
				Jumps: s.Jumps, preferred: s.Preferred}
			byStation[s.StationID] = st
		}
		st.Covered += share
		if mp := minPrice[s.TypeID]; mp > 0 && s.Price != ownStockNominalPrice {
			st.premW += share * s.Price / mp
		} else {
			st.premW += share
		}
		// Outlay is price times what this stop can hand over. score does not read it.
		if units > 0 {
			st.Outlay += s.Price * units
		} else {
			st.Outlay += s.Price
		}
	}
	// jumpWeight scales how heavily this pilot counts the flying: 1.0 with no
	// trait set. A greedy pilot discounts distance.
	jumpWeight := tn.IskJumpSlope / jumpPriceCost

	var best *supplyStop
	for _, st := range byStation {
		// A trip is priced on its real distance; Covered, the denominator, already
		// measures where the rest of the list is.
		st.score = (tripBaseJumps + float64(st.Jumps)*jumpWeight) / st.Covered
		if minPrice != nil && st.Covered > 0 {
			st.score *= st.premW / st.Covered // 1 when every covered line is the cheapest in the scan
		}
		if st.preferred {
			st.score *= 1 - tn.PreferredEdge
		}
		if best == nil || st.score < best.score ||
			(st.score == best.score && rand.Intn(2) == 0) {
			best = st
		}
	}
	return best
}

// nearestAllowedStation is the closest station the bot will dock at, searched
// over every station rather than a radius, so a bot in a pocket of empty
// systems finds the way out.
func (m *Mod) nearestAllowedStation(ctx context.Context, b *botRun, r travelRules,
	prefer map[int64]bool) (stationID, systemID int64, err error) {

	// Residency climbs here too: a pilot in a corner of its home region with no
	// usable station must be able to leave. The scope decides where it looks first.
	for {
		st, sys, err := m.nearestStationIn(ctx, b, r, prefer)
		if err == nil {
			return st, sys, nil
		}
		wider, ok := m.widen(ctx, b, r)
		if !ok {
			return 0, 0, err
		}
		r = wider
	}
}

// nearestStationIn is one rung of that search: the nearest usable station
// under exactly these rules, with no widening.
func (m *Mod) nearestStationIn(ctx context.Context, b *botRun, r travelRules,
	prefer map[int64]bool) (stationID, systemID int64, err error) {

	u, err := m.universe(ctx)
	if err != nil {
		return 0, 0, err
	}
	reach, err := m.reachFrom(ctx, b.SystemID, r)
	if err != nil {
		return 0, 0, err
	}
	var lastErr error
	for _, f := range stationFilterPasses(b.Profile, b.Tune) {
		f = f.scoped(r)
		rows, err := m.db.QueryContext(ctx,
			`SELECT s.stationID, s.solarSystemID FROM staStations s WHERE 1=1`+f.Clause, f.Args...)
		if err != nil {
			return 0, 0, fmt.Errorf("station scan: %w", err)
		}
		bestJumps, bestPref := 1<<30, false
		var bestStation, bestSystem int64
		for rows.Next() {
			var stID, sysID int64
			if err := rows.Scan(&stID, &sysID); err != nil {
				rows.Close()
				return 0, 0, err
			}
			j, ok := reach[sysID]
			if !ok {
				continue
			}
			pref := prefer[u.meta[sysID].RegionID]
			// Nearest wins; a preferred region breaks a tie and is worth
			// one extra jump, no more.
			better := j < bestJumps ||
				(j == bestJumps && pref && !bestPref) ||
				(pref && !bestPref && j == bestJumps+1)
			if bestStation == 0 || better {
				bestStation, bestSystem, bestJumps, bestPref = stID, sysID, j, pref
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, 0, err
		}
		if bestStation != 0 {
			return bestStation, bestSystem, nil
		}
		lastErr = fmt.Errorf("no station anywhere sits in space this pilot will enter (hard space limit%s)", r.limitNote())
		if f.Soft {
			m.logBot(b.CharID, logInfo, "standings",
				"no station it can route to is free of owners it dislikes — widening to anyone but the despised")
		}
	}
	return 0, 0, lastErr
}

// nameOfRegion is the region's name for the log, empty when unknown.
func (m *Mod) nameOfRegion(ctx context.Context, id int64) string {
	var s string
	_ = m.db.QueryRowContext(ctx,
		`SELECT regionName FROM mapRegions WHERE regionID = ?`, id).Scan(&s)
	return s
}

// travelNote is the sentence the log gets when a bot commits to a haul: where
// it is going and whether that crosses regions.
func (m *Mod) travelNote(ctx context.Context, b *botRun, sysID int64, jumps int) string {
	name := m.nameOfSystem(ctx, sysID)
	if name == "" {
		name = fmt.Sprint(sysID)
	}
	u, err := m.universe(ctx)
	if err != nil {
		return fmt.Sprintf("%s (%d jumps)", name, jumps)
	}
	here, there := u.meta[b.SystemID].RegionID, u.meta[sysID].RegionID
	if here != there && there != 0 {
		return fmt.Sprintf("%s (%d jumps, out into %s)", name, jumps, m.nameOfRegion(ctx, there))
	}
	return fmt.Sprintf("%s (%d jumps)", name, jumps)
}

// reachRegions is the set of regions a travel horizon touches, sorted. A reach
// map is thousands of system IDs, too many for a SQL IN list; the regions are a
// handful, so a query narrows by region first and by system after. It reads the
// loaded universe and runs no query.
func reachRegions(m *Mod, ctx context.Context, reach map[int64]int) []int64 {
	if len(reach) == 0 {
		return nil
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return nil
	}
	set := map[int64]bool{}
	for sysID := range reach {
		if r := u.meta[sysID].RegionID; r != 0 {
			set[r] = true
		}
	}
	return regionIDs(set)
}
