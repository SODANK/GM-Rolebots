package rolebots

// ProcureState is an outfit stop in progress (procure.go): the fit being bought toward, the
// shopping list, a listed ship being claimed. Reset by idleData.
type ProcureState struct {
	// Procurement for an assigned fit. ProcureSoft marks a trip the bot can walk away from: an ammo run
	// buys what it can and goes back to work, but a half-built hull has to halt.
	ProcureFitID   int64      `json:"procureFitID,omitempty"`
	ResumeActivity string     `json:"resumeActivity,omitempty"`
	Shopping       []ShopItem `json:"shopping,omitempty"`
	ProcureSoft    bool       `json:"procureSoft,omitempty"`

	// FitLines is every slot line of the fit being assembled (typeID, flag), kept beside the shopping
	// list so the outfit stop can tell a fit line from a leftover of the hull's previous fit. The
	// template is validated against an empty hull, so unclaimed modules come off first (stripOffFitModules).
	FitLines []ShopItem `json:"fitLines,omitempty"`

	// ProcureStuckAt is the station where an outfit stop last bought nothing on its list. Two strikes
	// at one station end the trip (procure.go): the market scan and the till can disagree, and the FSM
	// would otherwise retry the same station forever. Any purchase clears it.
	ProcureStuckAt int64 `json:"procureStuckAt,omitempty"`

	// ProcureEscrowID is a listed ship the trip is flying to claim for its hull, at
	// ProcureEscrowStation (escrow_ship.go); ProcureClaimedHull is the hull that claim delivered, for
	// buyhull to board instead of buying. idleData drops all three.
	ProcureEscrowID      int64 `json:"procureEscrowID,omitempty"`
	ProcureEscrowStation int64 `json:"procureEscrowStation,omitempty"`
	ProcureClaimedHull   int64 `json:"procureClaimedHull,omitempty"`

	// ProcureEscrowTried: this trip has already gone for a listing once.
	ProcureEscrowTried bool `json:"procureEscrowTried,omitempty"`

	// ShopStockRun marks a procure trip that is the home shopping pass's stock-up rather than an
	// activity's outfit: its endings stamp HomeShoppedAt and go idle instead of resuming an activity.
	// Not carried by idleData, since it is the trip.
	ShopStockRun bool `json:"shopStockRun,omitempty"`
}
