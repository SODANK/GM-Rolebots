package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	evedb "gurimoon/db"
)

// What a corporation's product really sells for.
// The production board values a product from the trade tape: what buyers paid over
// corpTradeRefWindowSec, in the factory's region when that region traded it often enough,
// else across the cluster. Only a type nobody bought in that window falls back to the
// book, because a book of stale asks reads as a price nobody pays.
// The same regional reference walks aged factory output down to where it clears
// (corpstock.go agedStockReprice).

const (
	// corpTradeRefWindowSec is how far back the tape is read. Two weeks keeps a thin product
	// priced and stops a week of dumping from setting the price alone.
	corpTradeRefWindowSec = 14 * 86400
	// corpTradeRefMinRegional is how many trades a region needs before its own average
	// outranks the cluster's.
	corpTradeRefMinRegional = 5
	// corpTradeRefTTL is how long one type's tape read stands.
	corpTradeRefTTL = 15 * time.Minute
)

// tapeRef is a volume-weighted trade average and how much stood behind it.
type tapeRef struct {
	Avg    float64
	Trades int64
	Units  int64
}

// typeTapeRefs is one type's tape: the cluster's average and each region's.
type typeTapeRefs struct {
	Cluster tapeRef
	Region  map[int64]tapeRef
}

// tradeRefsOf reads one type's tape, cached per type. A failed read returns an empty
// tape, which callers treat as no trades and fall back on the book, never as a price of 0.
// It is the end-user tape: the roster's resale legs are taken off first (tradeenduse.go);
// if that read fails the tape stays as the market recorded it.
func (m *Mod) tradeRefsOf(ctx context.Context, typeID int64) typeTapeRefs {
	if typeID == 0 {
		return typeTapeRefs{}
	}
	sctx, cancel := snapContext(ctx)
	defer cancel()
	refs, err := snapshotFor(snapKey("corp-traderef", typeID), corpTradeRefTTL, func() (typeTapeRefs, error) {
		since := clock.Unix() - corpTradeRefWindowSec
		// transactionType 1 is the buyer's row: one row per trade.
		// idx_mt_type_item_date covers (type, typeID, date, qty, price, region).
		rows, err := m.db.QueryContext(sctx, capStatement(`
			SELECT regionID, COUNT(*), SUM(quantity), SUM(price * quantity)
			  FROM market_transactions
			 WHERE transactionType = 1 AND typeID = ? AND transactionDateTime >= ?
			   AND quantity > 0 AND price > 0
			 GROUP BY regionID`),
			typeID, evedb.UnixToWin32(since))
		if err != nil {
			return typeTapeRefs{}, fmt.Errorf("trade tape %d: %w", typeID, err)
		}
		defer rows.Close()
		regions := map[int64]tapeRef{}
		regionISK := map[int64]float64{}
		for rows.Next() {
			var region, n int64
			var qty sql.NullInt64
			var v sql.NullFloat64
			if err := rows.Scan(&region, &n, &qty, &v); err != nil {
				return typeTapeRefs{}, err
			}
			if qty.Int64 <= 0 || v.Float64 <= 0 {
				continue
			}
			regions[region] = tapeRef{Avg: v.Float64 / float64(qty.Int64), Trades: n, Units: qty.Int64}
			regionISK[region] = v.Float64
		}
		if err := rows.Err(); err != nil {
			return typeTapeRefs{}, err
		}
		resale, rerr := m.resaleLegsOf(sctx, typeID, since)
		if rerr != nil {
			rlogf("rolebots: %v — the tape keeps its resale legs this read", rerr)
			resale = nil
		}
		return netTape(regions, regionISK, resale), nil
	})
	if err != nil {
		return typeTapeRefs{}
	}
	return refs
}

// pickTradeValue is the valuation rule: the region's own average when it is a market,
// else the cluster's when anything traded, else the book's value (fallback). Returns the
// value and its source. Pure.
func pickTradeValue(regional, cluster tapeRef, fallback float64) (float64, string) {
	switch {
	case regional.Trades >= corpTradeRefMinRegional && regional.Avg > 0:
		return regional.Avg, "region"
	case cluster.Trades > 0 && cluster.Avg > 0:
		return cluster.Avg, "cluster"
	default:
		return fallback, "book"
	}
}

// productTradeValue is what one unit of a product is worth to a corporation selling it
// in regionID (0 = no region): the tape first, the book (TypeValueOf) only for a type
// nobody bought.
func (m *Mod) productTradeValue(ctx context.Context, typeID, regionID int64) float64 {
	refs := m.tradeRefsOf(ctx, typeID)
	if v, src := pickTradeValue(refs.Region[regionID], refs.Cluster, 0); src != "book" {
		return v
	}
	return evedb.TypeValueOf(ctx, m.db, typeID).Value
}

// regionTradeRef returns a type's tape in the region a station stands in, empty when the
// region is unknown.
func (m *Mod) regionTradeRef(ctx context.Context, stationID, typeID int64) tapeRef {
	region := m.stationRegionID(ctx, stationID)
	if region == 0 {
		return tapeRef{}
	}
	return m.tradeRefsOf(ctx, typeID).Region[region]
}

// buybackValue is what one unit of a material is worth to a corporation buying it back
// from members: the tape (pickTradeValue), never above the book, so a bid does not pay
// more than the market sells it for. The book alone for a type nobody bought. Pure.
func buybackValue(regional, cluster tapeRef, book float64) float64 {
	tape, src := pickTradeValue(regional, cluster, book)
	if src == "book" {
		return book
	}
	if book > 0 {
		return math.Min(book, tape)
	}
	return tape
}

// deepBuybackValue is buybackValue for the deep-priced minerals (corpdeep.go deepPriced):
// the tape alone, the book only for a type nobody bought. The book is not a ceiling here
// because most listed units sit in a sealed pocket nobody can buy from. Pure.
func deepBuybackValue(regional, cluster tapeRef, book float64) float64 {
	v, _ := pickTradeValue(regional, cluster, book)
	return v
}

// buybackUnitValue is buybackValue for a type bid for at one station: the tape of the
// station's region, else the cluster's, held under the book. A deep-priced mineral reads
// the tape alone (deepBuybackValue).
func (m *Mod) buybackUnitValue(ctx context.Context, stationID, typeID int64, book float64) float64 {
	refs := m.tradeRefsOf(ctx, typeID)
	regional := refs.Region[m.stationRegionID(ctx, stationID)]
	if deepPriced(typeID) {
		return deepBuybackValue(regional, refs.Cluster, book)
	}
	return buybackValue(regional, refs.Cluster, book)
}
