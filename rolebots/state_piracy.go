package rolebots

// PiracyState is the piracy trip in progress (piracy.go). Reset by idleData.
type PiracyState struct {
	// PiracySystemID is the hunting ground, PiracyPickedAt when it was taken, PiracyGateID the
	// stargate being camped (0 = prowling belts instead).
	PiracySystemID int64 `json:"piracySystemID,omitempty"`
	PiracyPickedAt int64 `json:"piracyPickedAt,omitempty"`
	PiracyGateID   int64 `json:"piracyGateID,omitempty"`

	// Whether the camp has been taken up: the ship parked on the gate and the one line said in
	// Local. Without the marker the step cannot tell its first lap from its hundredth and would
	// re-announce every minute.
	PiracyCampSet bool `json:"piracyCampSet,omitempty"`
}

// PiracyMemory survives a re-plan: the loot left behind, the tally, and piracyHeatUntil, which SQL
// reads (legalcensus.go) and which must stay top level under that name.
type PiracyMemory struct {
	PiracyLootID    int64 `json:"piracyLootID,omitempty"`
	PiracyLootSysID int64 `json:"piracyLootSysID,omitempty"`
	PiracyKills     int   `json:"piracyKills,omitempty"`
	PiracyRansomed  int   `json:"piracyRansomed,omitempty"`

	// PiracyHeatUntil is when this pilot judges it safe to return to policed space, set from the
	// criminal flag it earned. Carried across re-plans: a flag outlives the trip, and forgetting it
	// flies the bot back into the guns.
	PiracyHeatUntil int64 `json:"piracyHeatUntil,omitempty"`
}
