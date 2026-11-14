package rolebots

// RndState is the R&D patent errand (research.go). It survives re-plan, or a won patent mid-trip
// would be stranded (TestIdleDataCarriesPatentTrip).
type RndState struct {
	// R&D patent lottery (research.go). RndOfferID is the won Tech II patent being collected; the rest is
	// the round trip to the granting agent and home to the factory. RndFieldID is the field the pilot
	// means to collaborate in, written early for the skill planner; RndCheckedAt throttles a lookup.
	RndOfferID      int64  `json:"rndOfferID,omitempty"`
	RndAgentID      int64  `json:"rndAgentID,omitempty"`
	RndStationID    int64  `json:"rndStationID,omitempty"`
	RndStationSysID int64  `json:"rndStationSysID,omitempty"`
	RndBPTypeID     int64  `json:"rndBPTypeID,omitempty"`
	RndBPName       string `json:"rndBPName,omitempty"`
	RndFactoryID    int64  `json:"rndFactoryID,omitempty"`
	RndFactorySysID int64  `json:"rndFactorySysID,omitempty"`
	RndFieldID      int64  `json:"rndFieldID,omitempty"`
	RndCheckedAt    int64  `json:"rndCheckedAt,omitempty"`
}
