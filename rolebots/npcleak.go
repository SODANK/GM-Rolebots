package rolebots

// The NPC gear leak: what the roster buys off NPC shelves that a solo
// industrialist could have sold instead. One 14-day aggregate over
// market_transactions, (station, type) to units, ISK and average price, for
// every purchase a rolebot made from an NPC. Readers: the build book
// (buildbook.go, evidenceLeak) and the sale (industry.go, industrySell).
// NPC sellers are found by ID band, not by clientID: PlaceBuyOrder writes the
// order's owning NPC corporation there, so seeder IDs match nothing. Every NPC
// corporation and seeder sits below evedb.MinPlayerEntityID. transactionType 1
// is the buyer's row, so a trade counts once. One read per npcLeakTTL serves
// the roster.

import (
	"context"
	"fmt"
	"math"
	"time"

	evedb "gurimoon/db"
)

const (
	// npcLeakWindowDays is how far back the ledger reads. Two weeks, so a
	// weekly rhythm (a fleet doctrine refitting, a war) is in it whole.
	npcLeakWindowDays = 14
	// npcLeakTTL is how long one read stands.
	npcLeakTTL = 30 * time.Minute

	// leakMinISK is the least leaked ISK, within a pilot's reach, that counts
	// as evidence. Under it the row is noise: one pilot buying one module.
	leakMinISK = 1e6
	// leakISKRef is the leak, within reach over the window, at which the
	// evidence keeps its full weight (leakPull). Per (region, type) the live
	// leak runs 10M–100M; a type leaking 100M+ is a hub staple.
	leakISKRef = 100e6
	// leakPullFloor is what a leak at leakMinISK keeps.
	leakPullFloor = 0.5

	// leakUndercut is how far under the NPC price a leaked type is valued and
	// listed: a buyer standing at the NPC shelf takes the bot ask beside it.
	leakUndercut = 0.95
)

// leakCategories are the manufactured-goods categories the ledger reads, and
// the ones the buyer's crafted edge applies to (gearCategory). Ore, minerals
// and trade goods have their own rules.
var leakCategories = map[int64]bool{catShip: true, catModule: true, catCharge: true, catDrone: true}

// leakRow is one station's leak of one type over the window.
type leakRow struct {
	StationID int64
	SystemID  int64
	RegionID  int64
	TypeID    int64
	Units     int64
	ISK       float64
}

// AvgPrice is what the roster paid per unit, on average.
func (r leakRow) AvgPrice() float64 {
	if r.Units <= 0 {
		return 0
	}
	return r.ISK / float64(r.Units)
}

// npcLeakBook is the ledger, indexed by type. Shared by every reader and
// never mutated after load.
type npcLeakBook struct {
	byType map[int64][]leakRow
}

// leakInReach is one type's leak folded over the stations a pilot can reach.
type leakInReach struct {
	ISK   float64
	Units int64
}

// AvgPrice is the reach-wide average price paid.
func (l leakInReach) AvgPrice() float64 {
	if l.Units <= 0 {
		return 0
	}
	return l.ISK / float64(l.Units)
}

// inReach sums one type's leak over the stations whose system is in reach.
// maxJumps 0 means any distance reach holds. Nil-safe.
func (lb *npcLeakBook) inReach(typeID int64, reach map[int64]int, maxJumps int) leakInReach {
	var out leakInReach
	if lb == nil {
		return out
	}
	for _, r := range lb.byType[typeID] {
		j, ok := reach[r.SystemID]
		if !ok || (maxJumps > 0 && j > maxJumps) {
			continue
		}
		out.ISK += r.ISK
		out.Units += r.Units
	}
	return out
}

// rows is one type's ledger. Nil-safe; the slice is shared and read-only.
func (lb *npcLeakBook) rows(typeID int64) []leakRow {
	if lb == nil {
		return nil
	}
	return lb.byType[typeID]
}

// npcLeaks is the cached ledger. A failed read is logged by the caller's
// choice; the snapshot holds the error for snapFailBackoff.
func (m *Mod) npcLeaks(ctx context.Context) (*npcLeakBook, error) {
	return snapshotFor(snapKey("npcleak"), npcLeakTTL, func() (*npcLeakBook, error) {
		sctx, cancel := snapContext(ctx)
		defer cancel()
		return m.loadNPCLeaks(sctx, clock.Unix())
	})
}

// loadNPCLeaks is the read itself, uncached, so the harness can run it on a
// fixture. now is unix seconds; transactionDateTime is Win32 FILETIME.
func (m *Mod) loadNPCLeaks(ctx context.Context, now int64) (*npcLeakBook, error) {
	since := evedb.UnixToWin32(now - npcLeakWindowDays*86400)
	cats := make([]int64, 0, len(leakCategories))
	for c := range leakCategories {
		cats = append(cats, c)
	}
	args := []any{since, evedb.MinPlayerEntityID}
	args = append(args, int64Args(cats)...)
	rows, err := m.db.QueryContext(ctx, capStatement(`
		SELECT t.stationID, s.solarSystemID, s.regionID, t.typeID,
		       SUM(t.quantity), SUM(t.price * t.quantity)
		FROM market_transactions t
		JOIN rolebot_characters rb ON rb.characterID = t.characterID
		JOIN staStations s ON s.stationID = t.stationID
		JOIN invTypes it ON it.typeID = t.typeID
		JOIN invGroups g ON g.groupID = it.groupID
		WHERE t.transactionType = 1 AND t.transactionDateTime >= ?
		  AND t.clientID < ? AND t.stationID > 0
		  AND g.categoryID IN (`+placeholders(len(cats))+`)
		GROUP BY t.stationID, s.solarSystemID, s.regionID, t.typeID`), args...)
	if err != nil {
		return nil, fmt.Errorf("npc leak: %w", err)
	}
	defer rows.Close()
	book := &npcLeakBook{byType: map[int64][]leakRow{}}
	for rows.Next() {
		var r leakRow
		if err := rows.Scan(&r.StationID, &r.SystemID, &r.RegionID, &r.TypeID, &r.Units, &r.ISK); err != nil {
			return nil, fmt.Errorf("npc leak row: %w", err)
		}
		if r.Units <= 0 || r.ISK <= 0 {
			continue
		}
		book.byType[r.TypeID] = append(book.byType[r.TypeID], r)
	}
	return book, rows.Err()
}

// leakPull is the leak evidence's strength: the floor at leakMinISK, full at
// leakISKRef and above, sqrt between so a type leaking four times as much
// pulls twice as hard. Zero under leakMinISK. Pure.
func leakPull(isk float64) float64 {
	if isk < leakMinISK {
		return 0
	}
	f := math.Sqrt(isk / leakISKRef)
	if f > 1 {
		f = 1
	}
	return leakPullFloor + (1-leakPullFloor)*f
}

// leakUnitMargin is what one unit earns built and sold into the leak: the
// NPC's average price undercut by leakUndercut, net of the sales tax and
// broker fee, less what it costs to make. A non-positive margin, or a cost
// nobody could price, is no project. Pure.
func leakUnitMargin(avgNPCPrice, buildCost float64, r marketRates) float64 {
	if avgNPCPrice <= 0 || buildCost <= 0 {
		return 0
	}
	gross := leakUndercut * avgNPCPrice
	return gross*(1-r.Tax-r.Fee) - buildCost
}

// outletMinEdgeFrac is how much better than the factory an outlet must look
// before a run is carried there: the trip, the hold and the time are real,
// and a near-tie is not worth a flight.
const outletMinEdgeFrac = 0.15

// outletStop is where a finished run is worth listing, and why.
type outletStop struct {
	StationID, SystemID int64
	Jumps               int
	LeakISK             float64 // what the roster paid NPCs here (npcleak.go)
	Pull                float64 // units shoppers here would take (stationdemand.go)
	Value               float64
}

// outletSellStop picks where a finished run should be listed: the reach station
// (`within`: system to jumps) with the most leaked NPC ISK plus shopper pull,
// shaved by jumpPriceCost a jump. It must beat the factory (`here`) by
// outletMinEdgeFrac and reach leakMinISK; hulls count leaks only. Pure.
func outletSellStop(leaks []leakRow, demand []counterDemand, unitPrice float64, qty int64,
	durable bool, within map[int64]int, here int64) (outletStop, bool) {

	stops := map[int64]*outletStop{}
	at := func(stationID, systemID int64) *outletStop {
		s, ok := stops[stationID]
		if !ok {
			s = &outletStop{StationID: stationID, SystemID: systemID}
			stops[stationID] = s
		}
		return s
	}
	for _, r := range leaks {
		if r.ISK >= leakMinISK {
			at(r.StationID, r.SystemID).LeakISK += r.ISK
		}
	}
	if durable && unitPrice > 0 {
		for _, r := range demand {
			if p := r.Pull(); p > 0 {
				at(r.StationID, r.SystemID).Pull += p
			}
		}
	}
	var home float64
	var best outletStop
	found := false
	for _, s := range stops {
		j, ok := within[s.SystemID]
		if !ok {
			continue
		}
		f := 1 - jumpPriceCost*float64(j)
		if f < sellNetFloor {
			f = sellNetFloor
		}
		s.Jumps = j
		s.Value = (s.LeakISK + min(float64(qty), s.Pull)*unitPrice) * f
		if s.StationID == here {
			home = s.Value
			continue
		}
		if s.Value < leakMinISK {
			continue
		}
		if !found || s.Value > best.Value || (s.Value == best.Value &&
			(s.Jumps < best.Jumps || (s.Jumps == best.Jumps && s.StationID < best.StationID))) {
			best, found = *s, true
		}
	}
	if !found || best.Value <= home*(1+outletMinEdgeFrac) {
		return outletStop{}, false
	}
	return best, true
}

// leakSellStop picks where a finished run should be listed: the leak station
// in reach (`within`: system to jumps) with the most leaked ISK, shaved by
// jumpPriceCost a jump. The factory competes at its own distance. ok=false
// when nothing in reach leaked leakMinISK. Pure.
func leakSellStop(rows []leakRow, within map[int64]int) (leakRow, int, bool) {
	var best leakRow
	bestJ, bestV, found := 0, 0.0, false
	for _, r := range rows {
		if r.ISK < leakMinISK {
			continue
		}
		j, ok := within[r.SystemID]
		if !ok {
			continue
		}
		f := 1 - jumpPriceCost*float64(j)
		if f < sellNetFloor {
			f = sellNetFloor
		}
		v := r.ISK * f
		if !found || v > bestV || (v == bestV && j < bestJ) {
			best, bestJ, bestV, found = r, j, v, true
		}
	}
	return best, bestJ, found
}

// ── the type to category map ────────────────────────────────────────────────

// typeCategoryTTL: the catalogue is static for the life of the process; the
// TTL only lets a hand-edited invGroups row reach the roster eventually.
const typeCategoryTTL = 6 * time.Hour

// typeCategories is every type's categoryID, one read for the roster. The
// returned map is shared and must not be modified.
func (m *Mod) typeCategories(ctx context.Context) (map[int64]int64, error) {
	return snapshotFor(snapKey("type-category"), typeCategoryTTL, func() (map[int64]int64, error) {
		sctx, cancel := snapContext(ctx)
		defer cancel()
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT t.typeID, g.categoryID
			FROM invTypes t JOIN invGroups g ON g.groupID = t.groupID`))
		if err != nil {
			return nil, fmt.Errorf("type categories: %w", err)
		}
		defer rows.Close()
		out := map[int64]int64{}
		for rows.Next() {
			var typeID, cat int64
			if err := rows.Scan(&typeID, &cat); err != nil {
				return nil, err
			}
			out[typeID] = cat
		}
		return out, rows.Err()
	})
}

// gearCategory reports whether a type is manufactured gear (a hull, module,
// charge or drone). False when the catalogue cannot be read.
func (m *Mod) gearCategory(ctx context.Context, typeID int64) bool {
	cats, err := m.typeCategories(ctx)
	if err != nil {
		return false
	}
	return leakCategories[cats[typeID]]
}
