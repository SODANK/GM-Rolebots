package rolebots

import (
	"context"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// What the market takes, and why the trade book has to know. A bot reaches the market through the same functions a
// player does: PlaceSellOrder takes sales tax out of every fill and a broker fee out of every order left resting
// (db/marketorders.go). An immediate buy pays nothing, so its cost basis is what was paid; a standing bid
// (tradebids.go) pays a broker fee at placement, booked through bookBrokerFee below. The escrow beside it is not a
// charge: it is refunded if the order is cancelled or expires. The book records net, not gross, so the feed reports
// the margin the pilot keeps. Nothing here moves money or changes a decision.

const (
	// Untrained rates, mirroring mktTransactionTax and mktCommissionPct in db/marketorders.go. Used only as the
	// fallback when a pilot's skills cannot be read: booking that sale as untaxed would be wrong.
	marketBaseTax = 0.01
	marketBaseFee = 0.01

	// marketMinBrokerFee mirrors the unexported mktMinimumFee, the one number here that cannot be read off an exported
	// symbol (tax and fee fractions come from GetMarketSkillLimits and BrokerStandingFactor). If the floor moves, small
	// orders book their fee off by the difference and nothing else is affected.
	marketMinBrokerFee = 100.0
)

// marketRates is what one pilot pays at one station.
type marketRates struct {
	Tax float64 // fraction of every sale, taken off the seller's proceeds
	Fee float64 // fraction of a resting order's value, paid at placement
}

// netSale is what actually reaches the wallet out of a gross sale.
func (r marketRates) netSale(gross float64) float64 { return gross * (1 - r.Tax) }

// brokerFee is what leaving an order of this value on the market costs.
func (r marketRates) brokerFee(orderValue float64) float64 {
	fee := r.Fee * orderValue
	if fee < marketMinBrokerFee {
		return marketMinBrokerFee
	}
	return fee
}

type marketRatesKey struct{ charID, stationID int64 }

type marketRatesEntry struct {
	at time.Time
	r  marketRates
}

var (
	marketRatesMu    sync.Mutex
	marketRatesCache = map[marketRatesKey]marketRatesEntry{}
)

// marketRatesTTL: the inputs are trained skill levels and NPC standings, which move on the scale of hours, and
// reading them costs ten queries while a busy roster sells several times a second.
const marketRatesTTL = 5 * time.Minute

// sweepMarketRatesCache drops expired entries. Read-through expiry only frees what somebody asks about again, and
// the key is per (pilot, station), so a station visited once would otherwise hold its entry for the life of the process.
func sweepMarketRatesCache() {
	now := clock.Now()
	marketRatesMu.Lock()
	for k, e := range marketRatesCache {
		if now.Sub(e.at) >= marketRatesTTL {
			delete(marketRatesCache, k)
		}
	}
	marketRatesMu.Unlock()
}

// marketRates is the read-through lookup: Accounting off the pilot's skills,
// Broker Relations bent by its standings toward whoever owns this station.
func (m *Mod) marketRates(ctx context.Context, charID, stationID int64) marketRates {
	key := marketRatesKey{charID: charID, stationID: stationID}
	now := clock.Now()

	marketRatesMu.Lock()
	if e, ok := marketRatesCache[key]; ok && now.Sub(e.at) < marketRatesTTL {
		marketRatesMu.Unlock()
		return e.r
	}
	marketRatesMu.Unlock()

	r := marketRates{Tax: marketBaseTax, Fee: marketBaseFee}
	if lim, err := evedb.GetMarketSkillLimits(ctx, m.db, charID); err == nil && lim != nil {
		r.Tax = lim.TaxRate
		r.Fee = lim.FeeRate * evedb.BrokerStandingFactor(ctx, m.db, charID, stationID)
	}

	marketRatesMu.Lock()
	marketRatesCache[key] = marketRatesEntry{at: now, r: r}
	marketRatesMu.Unlock()
	return r
}

// netSaleAt is the shorthand every booking site wants: what a gross sale at
// this station leaves in this pilot's wallet.
func (m *Mod) netSaleAt(ctx context.Context, charID, stationID int64, gross float64) float64 {
	return m.marketRates(ctx, charID, stationID).netSale(gross)
}

// bookBrokerFee records what a listing just cost to put on the market. It gets its own ledger line rather than
// being spread over later fills: the money moves at placement, and a listing that expires unsold still paid it.
func (m *Mod) bookBrokerFee(ctx context.Context, b *botRun, orderID, typeID int64,
	typeName string, price float64, resting int64) {

	if orderID == 0 || resting <= 0 {
		return
	}
	fee := m.marketRates(ctx, b.CharID, b.StationID).brokerFee(price * float64(resting))
	m.bookTrade(ctx, tradeEntry{
		CharID: b.CharID, Kind: tradeKindFee, TypeID: typeID, TypeName: typeName,
		Qty: resting, Price: price, Value: fee, Profit: -fee,
		Station: b.StationID, System: b.SystemID, OrderID: orderID,
		Note: fmt.Sprintf("broker fee on %d× resting at %.2f", resting, price),
	})
}

func init() { registerSweep("marketRatesCache", sweepMarketRatesCache) }
