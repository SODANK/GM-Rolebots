package rolebots

// OrderState is the conscription order a player gave this pilot (conscript_plan.go). It survives
// re-plan: the order is still standing.
type OrderState struct {
	// The standing order a player's fleet gave this pilot (conscript.go), one per pilot. idleData carries
	// the whole block, or a mining trip on an order would sell the fleet's ore to the nearest bidder.
	// OrderVerb holds the persisted Order* constants (conscript_orders.go): renaming one strands a pilot.
	OrderVerb string `json:"orderVerb,omitempty"`

	// OrderAt is when it was given; OrderDeferredAt when the pilot last said it was finishing
	// something first, so it says that once.
	OrderAt         int64 `json:"orderAt,omitempty"`
	OrderDeferredAt int64 `json:"orderDeferredAt,omitempty"`

	// OrderBossID is who gave it. Kept beside the verb rather than read from the conscript row: the
	// two can disagree for one pass when a fleet changes leader mid-errand, and the pilot finishes
	// the order it was given.
	OrderBossID int64 `json:"orderBossID,omitempty"`

	// The order's arguments: a pilot to shoot or guard, the system the work is
	// pinned to, the station to dock at or stockpile in, the ore named.
	OrderTargetChar int64 `json:"orderTargetChar,omitempty"`
	OrderSystemID   int64 `json:"orderSystemID,omitempty"`
	OrderStationID  int64 `json:"orderStationID,omitempty"`
	OrderOreTypeID  int64 `json:"orderOreTypeID,omitempty"`

	// OrderHoldFire keeps the guns cold until told otherwise. It is a flag, not a verb, so it
	// survives a later order: "hold fire" then "follow me" must not re-arm it.
	OrderHoldFire bool `json:"orderHoldFire,omitempty"`
}
