package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	evedb "gurimoon/db"
)

// The acquisition primitive used by the procurement FSM (procure.go), and there
// is exactly one: a purchase from a live sell order at the station through the
// real matching engine (wallet debit, journal, market_transactions, a seller
// paid). What the market cannot supply is not acquired.

// errNoSellerHere marks a till that failed for want of stock, not money: no sell
// order at all, or a book too thin to fill the line (emptytill.go). A refusal
// from the order engine or an empty wallet must not be wrapped in it.
var errNoSellerHere = errors.New("this station has no stock")

// buyAtStation buys up to qty of a type off this station's book, immediately, and
// books it. purpose ("fitting", "blueprint", "materials", "hull") goes in the
// trade book note. It returns what it got: PlaceBuyOrder commits partial fills,
// so a short fill is a purchase and only a fill of nothing is errNoSellerHere.
func (m *Mod) buyAtStation(ctx context.Context, charID, stationID, typeID, qty int64,
	purpose string) (itemID, filled int64, spent float64, err error) {
	var bestAsk sql.NullFloat64
	if err := m.db.QueryRowContext(ctx, `
		SELECT MIN(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0`,
		stationID, typeID).Scan(&bestAsk); err != nil {
		return 0, 0, 0, fmt.Errorf("ask lookup: %w", err)
	}
	if !bestAsk.Valid {
		return 0, 0, 0, fmt.Errorf("%w: no sell order at this station", errNoSellerHere)
	}
	// Ceiling generous enough to walk up a few price levels for size.
	out, err := evedb.PlaceBuyOrder(ctx, m.db, charID, stationID, typeID,
		bestAsk.Float64*1.25, qty, -1 /*station range*/, 1, 0 /*immediate*/, false)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("buy: %w", err)
	}
	if out.ErrorMsg != "" {
		return 0, 0, 0, fmt.Errorf("buy refused: %s", out.ErrorMsg)
	}
	// Nothing matched: no ISK moved and no stack was delivered, so this is the
	// empty-till answer. A short fill falls through.
	if out.FilledQty <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: nothing on the book filled", errNoSellerHere)
	}
	for _, f := range out.Fills {
		spent += f.Price * float64(f.Quantity)
		if len(f.ItemEntityIDs) > 0 {
			itemID = f.ItemEntityIDs[len(f.ItemEntityIDs)-1]
		}
	}
	noteBought(typeID, stationID, out.FilledQty) // the cached ladder catches up (sweep.go)
	if itemID == 0 {
		return 0, 0, 0, fmt.Errorf("buy filled but no item delivered")
	}
	// Multiple fills may deliver multiple stacks; merge them into one so
	// downstream fitting/loading has a single row to move.
	if len(out.Fills) > 1 {
		if merged, err := m.mergeStacks(ctx, charID, stationID, typeID); err == nil && merged != 0 {
			itemID = merged
		}
	}
	m.bookPurchase(ctx, charID, stationID, typeID, out.FilledQty, spent, out.Fills, purpose)
	return itemID, out.FilledQty, spent, nil
}

// bookPurchase puts one procurement buy in the trade book and records any
// part of it that came off another rolebot's listing.
func (m *Mod) bookPurchase(ctx context.Context, charID, stationID, typeID, qty int64,
	spent float64, fills []evedb.MarketFill, purpose string) {
	if qty <= 0 {
		return
	}
	name, err := typeName(ctx, m.db, typeID)
	if err != nil {
		name = fmt.Sprintf("type %d", typeID)
	}
	if purpose == "" {
		purpose = "procurement"
	}
	e := tradeEntry{
		CharID: charID, Kind: tradeKindBuy, TypeID: typeID, TypeName: name,
		Qty: qty, Price: spent / float64(qty), Value: spent,
		Station: stationID, System: m.stationSystemID(ctx, stationID),
		Note:   "bought for " + purpose,
		Resale: purpose == purposeOreFlip,
	}
	m.bookTrade(ctx, e)
	m.notePeerFills(ctx, e, fills, true, purpose)
	m.chatterMarketBought(charID, stationID, typeID, qty, e.Price, e.Resale)
}

// mergeStacks folds every packaged hangar stack of typeID at stationID into one
// row and returns its itemID. Ships and blueprints are excluded (stacking.go): a
// hull row is an identity that assembleOneHull and the fit point at, and an
// industry project remembers its blueprint's itemID across steps.
func (m *Mod) mergeStacks(ctx context.Context, charID, stationID, typeID int64) (int64, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.quantity FROM entity e
		  LEFT JOIN invTypes t  ON t.typeID  = e.typeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.typeID = ?
		  AND e.flag = 4 AND e.singleton = 0
		  AND COALESCE(g.categoryID, 0) NOT IN (6, 9)
		ORDER BY e.itemID ASC`, charID, stationID, typeID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var first, total int64
	var extras []int64
	for rows.Next() {
		var id, q int64
		if err := rows.Scan(&id, &q); err != nil {
			return 0, err
		}
		total += q
		if first == 0 {
			first = id
		} else {
			extras = append(extras, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if first == 0 || len(extras) == 0 {
		return first, nil
	}
	for _, id := range extras {
		if _, err := m.db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, id); err != nil {
			return 0, err
		}
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET quantity = ? WHERE itemID = ?`, total, first); err != nil {
		return 0, err
	}
	return first, nil
}

// ── valuation ───────────────────────────────────────────────────────────────
//
// What a pilot thinks something is worth, apart from where it can buy one
// (shopLadder, reach.go). The price is the cheapest live ask in the pilot's own
// scope, the residency band the shopping search is fenced to (residency.go),
// then the catalogue. There is no global rung: a price nobody within reach asks
// is not a price this pilot knows. The 100 ISK stub is a last resort for a type
// with no price anywhere. Read the live flag, not the number, because
// lootgrade.go grades only catalogue prices.

// priceRef is one cached valuation: what the scope asks for a type, and
// whether that came from a live order or from the catalogue.
type priceRef struct {
	Price float64
	Live  bool
}

// valuationRegions is the region set a pilot prices against: its economic-rule
// regions, or the region it stands in when residency is off or its home cannot
// be resolved.
func (m *Mod) valuationRegions(ctx context.Context, b *botRun) []int64 {
	if b != nil {
		if r := m.economicRules(ctx, b, defaultBand(b)); len(r.Regions) > 0 {
			return regionIDs(r.Regions)
		}
		if u, err := m.universe(ctx); err == nil && u != nil {
			if reg := u.meta[b.SystemID].RegionID; reg != 0 {
				return []int64{reg}
			}
		}
	}
	return nil
}

// valuationRegionsForChar is valuationRegions for a caller with only a character
// ID, such as the skill planner. It resolves the home region and neighbours
// directly, not through economicRules: a travel rule is not a claim that the
// pilot cannot see prices there, and catalogue pricing under-sizes its reserve.
func (m *Mod) valuationRegionsForChar(ctx context.Context, charID int64) []int64 {
	home := m.homeRegionByChar(ctx, charID)
	if home == 0 {
		return nil
	}
	return regionIDs(m.scopeRegions(ctx, home, bandAdjacent))
}

// estimatePriceFor: cheapest live ask inside the pilot's scope, else
// basePrice, else 100.
func (m *Mod) estimatePriceFor(ctx context.Context, b *botRun, typeID int64) (float64, error) {
	p, _, err := m.estimatePriceRefFor(ctx, b, typeID)
	return p, err
}

// estimatePriceRefFor is estimatePriceFor with its source reported: true for a
// live ask this pilot could reach, false for the catalogue. A caller that adjusts
// the number needs it: lootgrade.go grading a live ask would compound its
// multiplier on every re-list.
func (m *Mod) estimatePriceRefFor(ctx context.Context, b *botRun, typeID int64) (float64, bool, error) {
	return m.estimatePriceRefIn(ctx, m.valuationRegions(ctx, b), typeID)
}

// estimatePriceRefIn is the valuation against an explicit region set, for
// callers with a gang or a station rather than a pilot. It is shared and cached
// across the roster (marketsnap.go). An empty region set prices from the
// catalogue alone, not from the whole map.
func (m *Mod) estimatePriceRefIn(ctx context.Context, regions []int64, typeID int64) (float64, bool, error) {
	p, src, err := m.estimatePriceSourceIn(ctx, regions, typeID)
	return p, src == priceLive, err
}

// priceSource says where a valuation came from. The 100 ISK floor is not a
// price at all.
type priceSource int

const (
	// priceLive is a sell order inside the region set.
	priceLive priceSource = iota
	// priceCatalogue is invTypes.basePrice: stale, but a real number.
	priceCatalogue
	// priceGuess is the 100 ISK fallback for a type with neither. Fine as a
	// ballpark for sizing a reserve; wrong as something to hold a cost against.
	priceGuess
)

// estimatePriceSourceFor is estimatePriceSourceIn over the pilot's own valuation
// regions, the scope estimatePriceFor uses.
func (m *Mod) estimatePriceSourceFor(ctx context.Context, b *botRun, typeID int64) (float64, priceSource, error) {
	return m.estimatePriceSourceIn(ctx, m.valuationRegions(ctx, b), typeID)
}

// estimatePriceSourceIn is the valuation with its provenance. Every other
// estimatePrice* is a wrapper of it, so there is one ladder.
func (m *Mod) estimatePriceSourceIn(ctx context.Context, regions []int64, typeID int64) (float64, priceSource, error) {
	if len(regions) > 0 {
		key := snapKey("askprice", regions, typeID)
		sctx, cancel := snapContext(ctx)
		ref, err := snapshot(key, func() (priceRef, error) {
			args := append([]any{typeID}, int64Args(regions)...)
			var ask sql.NullFloat64
			if err := m.db.QueryRowContext(sctx, `
				SELECT MIN(price) FROM market_orders
				WHERE typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0
				  AND regionID IN (`+placeholders(len(regions))+`)`, args...).Scan(&ask); err != nil {
				return priceRef{}, err
			}
			if ask.Valid && ask.Float64 > 0 {
				return priceRef{Price: ask.Float64, Live: true}, nil
			}
			return priceRef{}, nil
		})
		cancel()
		if err != nil {
			return 0, priceGuess, err
		}
		if ref.Live {
			return ref.Price, priceLive, nil
		}
	}
	var base sql.NullFloat64
	if err := m.db.QueryRowContext(ctx,
		`SELECT basePrice FROM invTypes WHERE typeID = ?`, typeID).Scan(&base); err != nil {
		return 0, priceGuess, err
	}
	if base.Valid && base.Float64 > 0 {
		return base.Float64, priceCatalogue, nil
	}
	return 100, priceGuess, nil
}
