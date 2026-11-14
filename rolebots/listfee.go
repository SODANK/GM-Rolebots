package rolebots

import (
	"context"
	"sync"

	evedb "gurimoon/db"
)

// A listing is a door with a fee on it. The core refuses PlaceSellOrder when the
// wallet cannot pay the broker fee (db/marketorders.go), so listing paths test
// feeShort first instead of placing an order and reading the refusal.
//
// A stack under the 20,000 ISK order-slot floor is never listed, so a poor pilot
// can sit on stock that never converts. A pilot short of cash lists what it can
// (hurtingListMinValue).

const (
	// brokerFeeCoolSec is how long a pilot that could not pay a fee and found
	// no bid to sell into leaves its listings alone. The wallet changes on the
	// scale of trips, not of passes.
	brokerFeeCoolSec = 1800

	// hurtingListMinValue replaces the 20,000 ISK order-slot floor for a pilot
	// below its cash line. Under this a 1% fee (floored at marketMinBrokerFee)
	// eats a visible share of the order, and an order slot is a real cost.
	hurtingListMinValue = 5_000.0
)

var (
	brokerFeeCoolMu sync.Mutex
	brokerFeeCoolAt = map[int64]int64{} // charID to unix time of the last fee dead end
)

func init() {
	registerCharForget("brokerFeeCoolAt", func(id int64) {
		brokerFeeCoolMu.Lock()
		delete(brokerFeeCoolAt, id)
		brokerFeeCoolMu.Unlock()
	})
}

// feeShort is the pure test: a resting order worth orderValue costs
// rates.brokerFee(orderValue) to place, and the core refuses when the wallet is
// under it. True means the listing would be refused.
func feeShort(balance float64, rates marketRates, orderValue float64) bool {
	return balance < rates.brokerFee(orderValue)
}

// brokerFeeBlocks reports whether this pilot cannot afford to rest an order worth
// orderValue at its dock. It tries a distress sale into a standing bid through
// sell, and starts the cooldown when none buys. blocked=true means do not place
// the order; sold is what the sale moved. During the cooldown it skips the market.
func (m *Mod) brokerFeeBlocks(ctx context.Context, b *botRun, orderValue float64,
	sell func() (sold int64)) (blocked bool, sold int64) {

	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return false, 0 // an unreadable wallet is left to the core
	}
	if !feeShort(bal, m.marketRates(ctx, b.CharID, b.StationID), orderValue) {
		return false, 0
	}
	now := clock.Unix()
	brokerFeeCoolMu.Lock()
	cooling := now-brokerFeeCoolAt[b.CharID] < brokerFeeCoolSec
	brokerFeeCoolMu.Unlock()
	if cooling {
		return true, 0
	}
	if sell != nil {
		if sold = sell(); sold > 0 {
			return true, sold
		}
	}
	brokerFeeCoolMu.Lock()
	brokerFeeCoolAt[b.CharID] = now
	brokerFeeCoolMu.Unlock()
	m.logBot(b.CharID, logInfo, "market",
		"cannot pay the broker fee on a %.0f ISK listing (wallet %.0f) and nothing here bids on it — leaving the stack for %d min",
		orderValue, bal, brokerFeeCoolSec/60)
	return true, 0
}

// isHurting is a pilot below the cash line mining.go sells ore to recover
// from, or one flagged broke. Same line, so the two rules cannot disagree
// about who is poor.
func (m *Mod) isHurting(ctx context.Context, b *botRun) bool {
	if b.Broke {
		return true
	}
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	return err == nil && bal < b.Tune.MiningCashFloor
}

// listFloor is the order-slot value floor for this pilot: the shipped
// 20,000 ISK, or hurtingListMinValue when it is short of cash. Pure.
func listFloor(hurting bool, shipped float64) float64 {
	if hurting && hurtingListMinValue < shipped {
		return hurtingListMinValue
	}
	return shipped
}
