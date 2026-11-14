package rolebots

// CourierState is the courier mission accepted (courier.go). It survives re-plan: the collateral
// is on the line until delivered.
type CourierState struct {
	// Courier contract acceptance (courier_accept.go). The collateral is real ISK at risk, so every
	// field must survive re-plans: dropping one mid-haul strands the crate and forfeits the collateral.
	CourierMissionID     int64   `json:"courierMissionID,omitempty"`
	CourierPickupStation int64   `json:"courierPickupStation,omitempty"`
	CourierDropStation   int64   `json:"courierDropStation,omitempty"`
	CourierReward        float64 `json:"courierReward,omitempty"`
	CourierCollateral    float64 `json:"courierCollateral,omitempty"`
	CourierVolumeM3      float64 `json:"courierVolumeM3,omitempty"`
	CourierCrateID       int64   `json:"courierCrateID,omitempty"`
}
