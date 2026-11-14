package rolebots

// Which bids a seller standing in a station can actually sell into.
//
// A bid is not a station fact. Every bid has a range, and the core's match (PlaceSellOrder, db/marketorders.go
// orderReachesSeller) fills a sell against any bid in the seller's region whose range reaches the seller's
// station: -1 the bid's own station, 0 anywhere in its system, 1..32766 (a jump count) anywhere in its
// constellation, 32767 the whole region. Asking WHERE stationID = ? misses the seeded NPC ore and mineral
// demand (SeedOreDemand, core db/marketdb.go), which is placed region-wide. The jump-count case mirrors the
// core's approximation (no pathfinder, 1..32766 reads as same constellation): counting real jumps would
// predict fills the market refuses.

import (
	"context"
	"fmt"

	evedb "gurimoon/db"
)

// bidLoc is where one bid sits and how far it reaches.
type bidLoc struct {
	StationID, SystemID, ConstellationID, RegionID int64
	Range                                          int64
}

// sellerLoc is where the seller stands.
type sellerLoc struct {
	StationID, SystemID, ConstellationID, RegionID int64
}

// bidReachesSeller mirrors the core's orderReachesSeller predicate plus the
// region filter PlaceSellOrder puts in front of it. Pure.
func bidReachesSeller(o bidLoc, s sellerLoc) bool {
	if o.RegionID != s.RegionID {
		return false
	}
	switch {
	case o.Range == -1:
		return o.StationID == s.StationID
	case o.Range == 0:
		return o.SystemID == s.SystemID
	case o.Range >= 1 && o.Range <= 32766:
		return o.ConstellationID == s.ConstellationID
	case o.Range >= 32767:
		return true
	}
	return false // below -1: the core's predicate matches nothing either
}

// reachBid is one bid a seller at a station can fill.
type reachBid struct {
	OrderID   int64
	CharID    int64
	StationID int64
	Price     float64
	Volume    int64
	MinVolume int64
	Range     int64
}

// reachBidLimit bounds the region read. A (region, type) pair carries a few dozen live bids, so this guards
// against a pathological book. It reads dearest first, so what it cuts is the cheap tail.
const reachBidLimit = 400

// bidsReachingStation lists the live bids for typeID that a sell placed at stationID by sellerID would
// match, best first, in the core's order (price DESC, issued ASC). Seeder bids are included; the caller
// decides whether an NPC counter counts. The seller's own bids are left out, as in the core.
func (m *Mod) bidsReachingStation(ctx context.Context, stationID, typeID, sellerID int64) ([]reachBid, error) {
	p, err := evedb.GetStationParents(ctx, m.db, stationID)
	if err != nil {
		return nil, err
	}
	seller := sellerLoc{StationID: stationID, SystemID: p.SolarSystemID,
		ConstellationID: p.ConstellationID, RegionID: p.RegionID}
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.orderID, o.charID, o.stationID, o.solarSystemID, st.constellationID,
		       o.regionID, o.`+"`range`"+`, o.price, o.volRemaining, o.minVolume
		FROM market_orders o
		JOIN staStations st ON st.stationID = o.stationID
		WHERE o.regionID = ? AND o.typeID = ? AND o.bid = 1 AND o.orderState = 1
		  AND o.volRemaining > 0 AND o.price > 0 AND o.charID <> ?
		ORDER BY o.price DESC, o.issued ASC
		LIMIT ?`, p.RegionID, typeID, sellerID, reachBidLimit)
	if err != nil {
		return nil, fmt.Errorf("bids reaching station %d: %w", stationID, err)
	}
	defer rows.Close()
	var out []reachBid
	for rows.Next() {
		var b reachBid
		var loc bidLoc
		if err := rows.Scan(&b.OrderID, &b.CharID, &b.StationID, &loc.SystemID,
			&loc.ConstellationID, &loc.RegionID, &b.Range, &b.Price, &b.Volume, &b.MinVolume); err != nil {
			return nil, err
		}
		loc.StationID, loc.Range = b.StationID, b.Range
		if bidReachesSeller(loc, seller) {
			out = append(out, b)
		}
	}
	return out, rows.Err()
}

// bidsRaise is what selling up to units into these bids would gross, walking
// them best first the way the core's match does. Pure.
func bidsRaise(bids []reachBid, units int64) float64 {
	gross := 0.0
	for _, b := range bids {
		if units <= 0 {
			break
		}
		if b.MinVolume > units {
			continue // the core skips a bid whose minimum the rest cannot meet
		}
		take := units
		if b.Volume < take {
			take = b.Volume
		}
		gross += b.Price * float64(take)
		units -= take
	}
	return gross
}
