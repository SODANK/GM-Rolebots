package rolebots

// PosState is a starbase errand in progress (postask.go and friends). Reset by idleData; the
// staging stamp lives in StickyState.
type PosState struct {
	// Starbase errands (poshaul.go, posreq.go, posintel.go): the haul leg in hand, the requisition
	// lines a buy trip fills and where it buys, the survey order with its kit office and target moon,
	// the working leg at a tower, and the m3 loaded this trip (for the wage share).
	PosLegID            int64   `json:"posLegID,omitempty"`
	PosTripReqs         []int64 `json:"posTripReqs,omitempty"`
	PosTripStation      int64   `json:"posTripStation,omitempty"`
	PosSurveyID         int64   `json:"posSurveyID,omitempty"`
	PosSurveyKitStation int64   `json:"posSurveyKit,omitempty"`
	PosSurveyMoon       int64   `json:"posSurveyMoon,omitempty"`
	PosAtTowerUntil     int64   `json:"posAtTower,omitempty"`
	PosTripM3           float64 `json:"posTripM3,omitempty"`

	// A requisition buy trip planned as an order-book sweep (posreq_sweep.go); PosSweep is set when it
	// began as one. PosSweepStops counts sellers visited, PosSweepVisited names them, PosSweepCaps is
	// each line's price cap (reqID to ISK a unit), PosSweepGot what each line bought and has not delivered.
	PosSweep        bool              `json:"posSweep,omitempty"`
	PosSweepStops   int               `json:"posSweepStops,omitempty"`
	PosSweepVisited []int64           `json:"posSweepVisited,omitempty"`
	PosSweepCaps    map[int64]float64 `json:"posSweepCaps,omitempty"`
	PosSweepGot     map[int64]int64   `json:"posSweepGot,omitempty"`

	// A tower resident's staging visit (posresident.go): which tower. Carried by idleData because it
	// paces the visits.
	PosStagePlanID int64 `json:"posStagePlan,omitempty"`

	// The tower array a builder is flying out to install a job at (posjobs.go).
	PosTowerJobArray int64 `json:"posTowerJob,omitempty"`

	// The corporation refinery array this pilot is dropping ore at or collecting minerals from
	// (posrefinery.go); negative once the minerals are aboard and it is carrying them home.
	PosRefineArray int64 `json:"posRefineArr,omitempty"`
}
