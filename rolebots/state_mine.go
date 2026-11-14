package rolebots

// MineState is the mining leg in progress (mining.go): the rock family, the lots in the hold, the
// belt being debited. Reset by idleData.
type MineState struct {
	OreTypeID int64  `json:"oreTypeID,omitempty"`
	OreName   string `json:"oreName,omitempty"`
	OreUnits  int64  `json:"oreUnits,omitempty"` // units the current mine leg will yield

	// OreLots is what the leg took out of the rocks, per variant: OreTypeID names only the family, and
	// one belt pays several variants. Every disposal walks this list (empty means plain OreTypeID,
	// beltfield.go). Not carried by idleData, or a re-plan would deliver the last cargo twice.
	OreLots     []oreLot `json:"oreLots,omitempty"`
	MineBeltID  int64    `json:"mineBeltID,omitempty"`  // belt this leg is debiting (beltfield.go)
	YieldM3PerS float64  `json:"yieldM3PerS,omitempty"` // computed rate snapshot (for ESP display)
}

// MineMemory survives a re-plan: recent systems, the fit to re-board after hauling, the disposal
// rule, and the factory a standby trip feeds.
type MineMemory struct {
	// MiningReturnFitID is the mining fit to re-board after a hauling phase: a solo miner that
	// swaps into a trade ship for the dispose leg returns to it. Cleared on re-board.
	MiningReturnFitID int64 `json:"miningReturnFitID,omitempty"`

	// RecentSystems is the short tail of belt systems this bot has worked. The search prefers
	// anywhere else, so the bot roams its range.
	RecentSystems []int64 `json:"recentSystems,omitempty"`

	// MineDisposal is what this trip's full hold becomes, set once per mining plan by
	// resolveMiningDisposal (mining.go): "" or "sell" sells to the best bid, "refine" refines and lists
	// the minerals, "list" lists the rock. Carried by idleData, or a mid-trip refine run feeds the bid.
	MineDisposal string `json:"mineDisposal,omitempty"`

	// MineRefineStationID pins a refine trip to the pilot's own factory;
	// 0 = the anchor/nearest station at dispose time (miningDispose).
	MineRefineStationID int64 `json:"mineRefineStationID,omitempty"`

	// StandbyFactoryID marks this mining trip as standby work for a factory pilot with nothing to
	// install: it stockpiles minerals at its own factory (resolveStandbyMining, mining.go). Carried
	// by idleData like MineDisposal, or a standby load would sell to the bid instead.
	StandbyFactoryID int64 `json:"standbyFactoryID,omitempty"`
}

// IceState is the ice-harvesting leg in progress (ice.go). Reset by idleData.
type IceState struct {
	// The ice trip (ice.go): where, which Ice Field, the barge it went to
	// board, and the blocks a harvest leg is producing.
	IceSysID  int64    `json:"iceSysID,omitempty"`
	IceBeltID int64    `json:"iceBeltID,omitempty"`
	IceShipID int64    `json:"iceShipID,omitempty"`
	IceLots   []oreLot `json:"iceLots,omitempty"`
}
