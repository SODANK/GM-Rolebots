package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	evedb "gurimoon/db"
)

// The end-user tape: a sale to a trader is not a sale. The trade tape (corptraderef.go) prices a corporation's
// products, but it was mostly the roster's own middlemen buying to sell on, so the tape takes the resale legs off
// before anything reads it. A resale leg is flagged on the trade book row when booked (tradeEntry.Resale):
//
//	trade.go       a hauler loading a run ("loading a haul for ...")
//	tradebids.go   a standing bid that filled on placement
//	tradebook.go   a standing bid filled later, into trade stock (bookFill)
//	orearb.go      ore bought to flip (purposeOreFlip)
//
// Supply bids and procurement buys are customers and stay; a type whose whole tape was resale reads as untraded, never as zero.

// purposeOreFlip is the procurement purpose orearb.go buys under: ore bought
// only to sell again, so a resale leg.
const purposeOreFlip = "ore to flip"

// The trade-book notes a resale leg carries, for the one-time backfill of rows booked without the flag. Each is
// the literal its writer uses.
const (
	resaleNoteHaul     = "loading a haul for "
	resaleNoteSpotBid  = "an ask was sitting under the bid it meant to leave"
	resaleNoteBidFill  = "somebody took its standing bid"
	resaleNoteOreFlip  = "bought for " + purposeOreFlip
	tradeResaleMarker  = "rolebots.tradeResaleBackfilled"
	tradeResaleChunk   = 50_000
	tradeResaleTimeout = 10 * time.Minute
)

// resaleLeg is one region's resale purchases of a type over the tape window.
type resaleLeg struct {
	Units int64
	ISK   float64
}

// netResale is a region's tape with its resale legs taken off. Pure. Units and ISK are subtracted outright and the
// trade count shrinks in proportion to the units left. When the ISK cannot be matched (the resale legs cost more than
// the tape holds) the original average stands for the units that are left.
func netResale(t tapeRef, isk float64, r resaleLeg) tapeRef {
	if r.Units <= 0 || t.Units <= 0 {
		return t
	}
	units := t.Units - r.Units
	if units <= 0 {
		return tapeRef{}
	}
	out := tapeRef{Units: units}
	out.Trades = int64(math.Round(float64(t.Trades) * float64(units) / float64(t.Units)))
	if out.Trades < 1 {
		out.Trades = 1
	}
	if left := isk - r.ISK; left > 0 {
		out.Avg = left / float64(units)
	} else {
		out.Avg = t.Avg
	}
	return out
}

// netTape takes every region's resale legs off a type's tape and rebuilds the
// cluster from what is left. regionISK is each region's traded ISK, which the
// tape itself keeps only as an average. Pure.
func netTape(regions map[int64]tapeRef, regionISK map[int64]float64, resale map[int64]resaleLeg) typeTapeRefs {
	out := typeTapeRefs{Region: make(map[int64]tapeRef, len(regions))}
	var isk float64
	for region, t := range regions {
		n := netResale(t, regionISK[region], resale[region])
		if n.Units <= 0 {
			continue
		}
		out.Region[region] = n
		out.Cluster.Trades += n.Trades
		out.Cluster.Units += n.Units
		isk += n.Avg * float64(n.Units)
	}
	if out.Cluster.Units > 0 {
		out.Cluster.Avg = isk / float64(out.Cluster.Units)
	}
	return out
}

// resaleLegsOf is a type's resale purchases since `since`, by region. Read
// through idx_rbtrade_type_resale: one type, one flag, a window.
func (m *Mod) resaleLegsOf(ctx context.Context, typeID, since int64) (map[int64]resaleLeg, error) {
	rows, err := m.db.QueryContext(ctx, capStatement(`
		SELECT s.regionID, SUM(t.quantity), SUM(t.value)
		  FROM rolebot_trades t
		  JOIN mapSolarSystems s ON s.solarSystemID = t.systemID
		 WHERE t.typeID = ? AND t.resale = 1 AND t.ts >= ? AND t.quantity > 0
		 GROUP BY s.regionID`), typeID, since)
	if err != nil {
		return nil, fmt.Errorf("resale legs %d: %w", typeID, err)
	}
	defer rows.Close()
	out := map[int64]resaleLeg{}
	for rows.Next() {
		var region int64
		var qty sql.NullInt64
		var isk sql.NullFloat64
		if err := rows.Scan(&region, &qty, &isk); err != nil {
			return nil, err
		}
		out[region] = resaleLeg{Units: qty.Int64, ISK: isk.Float64}
	}
	return out, rows.Err()
}

// backfillTradeResale flags the resale legs booked without the flag, over the tape window, once. It walks the book by
// tradeID in chunks and records the marker only when the whole window is done, so a cut-short boot retries. Init runs
// it in a goroutine; until it finishes the tape reads unadjusted.
func (m *Mod) backfillTradeResale() {
	ctx, cancel := context.WithTimeout(context.Background(), tradeResaleTimeout)
	defer cancel()
	if v, ok, _ := evedb.GetServerSetting(ctx, m.db, tradeResaleMarker); ok && v == "1" {
		return
	}
	since := clock.Unix() - corpTradeRefWindowSec - 86400
	var lo, hi sql.NullInt64
	if err := m.db.QueryRowContext(ctx,
		`SELECT MIN(tradeID), MAX(tradeID) FROM rolebot_trades WHERE ts >= ?`, since).Scan(&lo, &hi); err != nil {
		rlogf("rolebots: trade resale backfill: %v", err)
		return
	}
	flagged := int64(0)
	for from := lo.Int64; lo.Valid && from <= hi.Int64; from += tradeResaleChunk {
		res, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_trades SET resale = 1
			 WHERE tradeID BETWEEN ? AND ? AND resale = 0 AND ts >= ?
			   AND ((kind = ? AND (note LIKE ? OR note LIKE ? OR note = ?))
			     OR (kind = ? AND note = ?))`,
			from, from+tradeResaleChunk-1, since,
			tradeKindBuy, resaleNoteHaul+"%", resaleNoteSpotBid+"%", resaleNoteOreFlip,
			tradeKindFill, resaleNoteBidFill)
		if err != nil {
			rlogf("rolebots: trade resale backfill: %v", err)
			return
		}
		n, _ := res.RowsAffected()
		flagged += n
	}
	if err := evedb.SetServerSetting(ctx, m.db, tradeResaleMarker, "1"); err != nil {
		rlogf("rolebots: trade resale backfill marker: %v", err)
		return
	}
	rlogf("rolebots: trade resale backfill — %d leg(s) over the tape window flagged as resale", flagged)
}
