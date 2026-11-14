package rolebots

// TradeState is one buy-here-sell-there haul in flight (trade.go). Reset by idleData.
type TradeState struct {
	TradeTypeID        int64   `json:"tradeTypeID,omitempty"`
	TradeName          string  `json:"tradeName,omitempty"`
	TradeQty           int64   `json:"tradeQty,omitempty"`
	TradeBuyStationID  int64   `json:"tradeBuyStationID,omitempty"`
	TradeSellStationID int64   `json:"tradeSellStationID,omitempty"`
	TradeSellSystemID  int64   `json:"tradeSellSystemID,omitempty"`
	TradeAsk           float64 `json:"tradeAsk,omitempty"`
	TradeBid           float64 `json:"tradeBid,omitempty"`
	TradeSpent         float64 `json:"tradeSpent,omitempty"`

	// TradeKind: "" and "arb" buy off an ask here and hit a bid there, both legs immediate; "spec" is a
	// dedicated trader's speculative run (buy where goods exist, haul to where there are none, list at a
	// markup, ending with an order slot committed); "stock" starts with goods the pilot already owns.
	TradeKind string `json:"tradeKind,omitempty"`

	// The asking price a spec run intends to list at, and the goods' cost per unit, which is the cost
	// basis of the resting order and so of the profit it books.
	TradeListPrice float64 `json:"tradeListPrice,omitempty"`
	TradeUnitCost  float64 `json:"tradeUnitCost,omitempty"`

	// TradeBuyCap is the most a dip run pays a unit (dipboard.go); 0 for every
	// other run. TradeFresh marks goods bought on this run, which restock must
	// file even when the run lists on the dock it bought at.
	TradeBuyCap float64 `json:"tradeBuyCap,omitempty"`
	TradeFresh  bool    `json:"tradeFresh,omitempty"`

	// TradeItemID is the one item a run carries when it is a blueprint: a singleton that the
	// packaged-stack helpers never move (bpotrade.go).
	TradeItemID int64 `json:"tradeItemID,omitempty"`

	// The system the goods were bought in, so a completed run can be booked
	// against the lane it was actually flown over (traffic.go).
	TradeBuySystemID int64 `json:"tradeBuySystemID,omitempty"`
}

// TradeMemory is what the last run hauled and how many runs in a row, the streak that makes a
// trader look past a golden route (tradeMaxStreak). It survives re-plan.
type TradeMemory struct {
	// The commodity the last run hauled and how many runs in a row. After tradeMaxStreak of the same
	// thing the planner must look at its next-best idea, so traders spread across the map.
	TradeLastTypeID int64  `json:"tradeLastTypeID,omitempty"`
	TradeLastName   string `json:"tradeLastName,omitempty"`
	TradeStreak     int    `json:"tradeStreak,omitempty"`
	// A hauler grant from the corporation (gangsupply.go): ISK or a hull given so the pilot buys or
	// boards an industrial for a fleet's hauler seat. haulerGrantResume spends it on the next docked
	// plan and clears it once an industrial is owned or the grant is stale.
	HaulerGrantAt     int64 `json:"haulerGrantAt,omitempty"`
	HaulerGrantTypeID int64 `json:"haulerGrantTypeID,omitempty"`
}
