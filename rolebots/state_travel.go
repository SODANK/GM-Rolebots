package rolebots

// TravelState is the leg in progress: the route, when the current leg ends, what phase to enter on
// arrival, and the activity's target. Reset by idleData.
type TravelState struct {
	Path      []int64 `json:"path,omitempty"`      // systems still to enter (front = next)
	LegEndsAt int64   `json:"legEndsAt,omitempty"` // unix; current leg (jump/dock/cycle) completes
	NextPhase string  `json:"nextPhase,omitempty"` // phase to enter when travel completes

	// IdleWhy is why the planner left the pilot idle on its last pass (sim.go), shown on the
	// /rolebots/log board beside the nap countdown. plan() clears it before it decides anything.
	IdleWhy string `json:"idleWhy,omitempty"`

	// AlignSec is this ship's nominal align time (hulltags.go), stamped when the route is planned and
	// read on every unwatched leg, so a jump costs a float off loaded state instead of a hull lookup.
	// 0 means not stamped, and the leg falls back to the unknown-class figure.
	AlignSec float64 `json:"alignSec,omitempty"`

	// ConvoyHoldSince is when this fleet member began waiting at its gate for packmates behind it
	// on the same trip (convoyHolds, fleetmove.go); 0 when not waiting. Cleared at the jump.
	ConvoyHoldSince int64 `json:"convoyHoldSince,omitempty"`

	// Common activity targets.
	TargetSystemID  int64 `json:"targetSystemID,omitempty"`
	TargetStationID int64 `json:"targetStationID,omitempty"`
}

// UndockState is the door just left or the gate just crossed, read by the embodied tier to launch
// the projection as a real undock or jump-in looks. Stale after undockGraceSec.
type UndockState struct {
	// The station this bot has just left, and when. The embodied tier launches the projection out of
	// the dock from it, thrown out along the station's orientation instead of parked alongside. Stale
	// after undockGraceSec.
	UndockStationID int64 `json:"undockStationID,omitempty"`
	UndockAt        int64 `json:"undockAt,omitempty"`

	// Which slot at that station's door this ship was given (undockwave.go). Both tiers resolve the
	// same offset from it, so the persisted position and the projection's launch point match and eight
	// ships leaving one station do not share a point.
	UndockSlot int `json:"undockSlot,omitempty"`

	// Until when this ship burns straight out of the door and nothing else may steer it. The abstract
	// tier holds its leg for the same window; the one-second grid lane enforces it, so the burn survives
	// a roster pass that takes a minute.
	UndockRunUntil int64 `json:"undockRunUntil,omitempty"`

	// When set, the projection loiters outside the station it just left until this moment
	// (undockLoiterFor).
	UndockLoiterUntil int64 `json:"undockLoiterUntil,omitempty"`

	// Which station the loiter circles. It has its own field because the loiter outlives the launch:
	// settleUndock clears UndockStationID when the run ends, and a loiter reading the launch marker
	// would lose its anchor.
	UndockLoiterStationID int64 `json:"undockLoiterStationID,omitempty"`

	// The gate this bot has just come through, and when: the projection flashes it on arrival, the
	// only sign others on that gate get that someone jumped in. Stale after undockGraceSec.
	ArriveGateID int64 `json:"arriveGateID,omitempty"`
	ArriveAt     int64 `json:"arriveAt,omitempty"`
}
