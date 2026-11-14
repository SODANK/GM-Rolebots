package rolebots

import (
	"context"
	"strings"

	evedb "gurimoon/db"
)

// Illiquidity: what a pilot does when its stock is worth more than its wallet can afford to
// sell. A listing costs a broker fee at placement (db/marketorders.go, 1% floored at 100 ISK),
// so a pilot that spent its wallet on materials can hold goods it cannot list.
// Prevention is the liquidity floor (industry.go). This file is the cure for pilots already
// stuck. PlaceSellOrder charges the fee only on the remainder that rests, and refuses for a
// fee only when len(out.Fills) == 0, so a pilot with no ISK can still sell if it prices to
// clear rather than to rest.
// This is a fallback, not a pricing mode (mineralAsk lists above the bid for pilots that can
// afford the shelf). The ladder: try the pilot's own price; if refused for the fee and a bid
// would take the stack, sell into the bid. Seeded NPC bids count here; mineralAsk excludes them.

// brokerFeeRefusal reports whether a PlaceSellOrder refusal was the broker fee rather than
// the order cap. It matches the message because ErrorMsg is the client-facing string with no
// code; a miss only skips a retry.
func brokerFeeRefusal(msg string) bool {
	return strings.Contains(msg, "broker fee")
}

// clearingBid returns the best price that would take units of a type off a pilot's hands
// where it stands, and how many that bid would take. PlaceSellOrder matches every bid in the
// region whose range reaches this station (bidreach.go), so this reads those, seeder bids
// included.
func (m *Mod) clearingBid(ctx context.Context, sellerID, stationID, typeID int64) (price float64, vol int64) {
	bids, err := m.bidsReachingStation(ctx, stationID, typeID, sellerID)
	if err != nil || len(bids) == 0 {
		return 0, 0
	}
	return bids[0].Price, bids[0].Volume
}

// distressSell converts a stack at whatever the local counter pays, after an ordinary
// listing was refused for the broker fee. It prices at the bid, not under it: the order
// matches at the bid's price either way. The quantity is capped at the bid's depth, since a
// resting unit would bring back the fee. It returns the units sold and the gross.
func (m *Mod) distressSell(ctx context.Context, b *botRun, stationID, typeID, itemID, qty int64) (int64, float64) {
	bid, depth := m.clearingBid(ctx, b.CharID, stationID, typeID)
	if bid <= 0 {
		return 0, 0
	}
	sell := qty
	if depth < sell {
		sell = depth
	}
	if sell < 1 {
		return 0, 0
	}
	// Duration 0 makes an immediate order with no resting half, so it cannot reach the fee.
	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, stationID,
		typeID, itemID, bid, sell, -1 /*station*/, 1, 0 /*immediate*/, false)
	if err != nil || out == nil || out.ErrorMsg != "" || out.FilledQty <= 0 {
		return 0, 0
	}
	gross := 0.0
	for _, f := range out.Fills {
		gross += f.Price * float64(f.Quantity)
	}
	m.notePeerFills(ctx, tradeEntry{
		CharID: b.CharID, TypeID: typeID, TypeName: m.typeNameOr(ctx, typeID),
		Station: stationID, System: b.SystemID,
	}, out.Fills, false, "distress sale")
	m.logBot(b.CharID, logInfo, "market",
		"could not afford to list %d× %s — sold %d into the standing bid at %.2f for %.0f ISK to get the wallet moving again",
		qty, m.typeNameOr(ctx, typeID), out.FilledQty, bid, gross)
	return out.FilledQty, gross
}
