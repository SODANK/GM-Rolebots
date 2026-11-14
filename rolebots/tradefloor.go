package rolebots

// What a haul has to clear before it is worth flying, for a pilot that has almost nothing. The flat floor
// (tradeMinProfitISK, 5,000 ISK) suits a pilot with a wallet, but a pilot holding 843 ISK cannot build a 5,000 ISK
// profit out of any lot it can pay for and would report failure every two minutes until the failsafe halts it. So the
// floor scales with what the pilot can commit and stops scaling at both ends: the flat 5,000 ISK at the top, the
// smallest sum that is a real cost in this game at the bottom.

// tradeProfitFloorFrac is the share of spendable ISK a run must return before it is worth flying, once the flat floor
// stops applying. At 2% the flat floor bites from about 250,000 ISK spendable, near the wallet floor (walletFloorISK),
// so the scaling arm covers the pilots the wallet floor already calls poor and nobody else.
const tradeProfitFloorFrac = 0.02

// tradeProfitFloorMin is the hard bottom. 100 ISK is mktMinimumFee, the core's floor on a broker fee and so the
// smallest amount this game treats as money: a run that clears less than the fee on the order that ends it earned nothing.
const tradeProfitFloorMin = 100.0

// tradeProfitFloor is what this pilot's next haul has to clear. Pure, so the harness can pin both ends of the clamp.
// It takes the trading budget (what the haul may commit, after the wallet reserve and any spend cap), passed through
// spendable (broke.go), so the floor never prices a run against ISK the wallet floor is holding back.
func tradeProfitFloor(budget float64) float64 {
	scaled := spendable(budget) * tradeProfitFloorFrac
	if scaled >= tradeMinProfitISK {
		return tradeMinProfitISK
	}
	if scaled < tradeProfitFloorMin {
		return tradeProfitFloorMin
	}
	return scaled
}
