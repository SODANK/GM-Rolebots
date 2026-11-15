package rolebots

// AppetiteState is the satiation bookkeeping the activity chooser reads (appetite.go). It survives
// every re-plan, since the re-plan chooses from it.
type AppetiteState struct {
	// Worked is the decayed seconds of recent work per activity, the satiation the chooser reads;
	// WorkedAt is when it was last decayed. ActivityStartedAt marks the session in progress, folded
	// into Worked at the next plan. LastActivity is the previous plan's activity (stickiness).
	Worked            map[string]float64 `json:"worked,omitempty"`
	WorkedAt          int64              `json:"workedAt,omitempty"`
	ActivityStartedAt int64              `json:"activityStartedAt,omitempty"`
	LastActivity      string             `json:"lastActivity,omitempty"`
}

// StickyState is every hold-off and remembered preference that survives a re-plan: cooldowns on
// asking the gang for money, on the next shop and on the ride home; the mining disposal choice;
// the factory a standby trip serves. Nothing here describes work in progress.
type StickyState struct {
	// SubsidyAt is when this pilot last asked its gang to cover what it could not afford (broke.go).
	// The ask has its own cooldown so it is not repriced every pass.
	SubsidyAt int64 `json:"subsidyAt,omitempty"`

	// RestockAfter holds off the next ammo run (unix). Set when one comes back empty-handed, so a
	// bot where nobody sells its charge does not repeat the errand.
	RestockAfter int64 `json:"restockAfter,omitempty"`

	// HomeShoppedAt is when the last home shopping pass finished (unix). It is the home loop's
	// re-entry guard (homeshop.go): a pilot idling at home must not re-run the pass on every idle
	// retry. Carried by idleData because it is about the pilot's visit, not one trip.
	HomeShoppedAt int64 `json:"homeShoppedAt,omitempty"`

	// GangUnfitUntil excuses this pilot from gang claims (unix) after it turned up for a seat
	// without the gear (gangSeatReady). The gang gate skips it until the memo lapses. Carried by
	// idleData, or the excuse loops forever.
	GangUnfitUntil int64 `json:"gangUnfitUntil,omitempty"`

	// ProcureFailAt is the hard-line twin of RestockAfter, per fit: fitID to the unix time before
	// which the market could not supply that fit. While cooling down the selector will not propose
	// buying it (a live ship of it still flies). Expired entries are pruned on write.
	ProcureFailAt map[int64]int64 `json:"procureFailAt,omitempty"`

	// RideFailAt holds off the ride-home errand (unix) after a pass where nothing in reach sold a
	// shuttle (shuttle.go). Same shape as RestockAfter and ProcureFailAt: it stops the market sweep
	// repeating on every plan.
	RideFailAt int64 `json:"rideFailAt,omitempty"`

	// WarehouseMemo is the warehouse run's memory of each far station (warehouse_memo.go): stationID
	// to when it may be tried again, visits in a row that disposed of nothing, and the hoard's value
	// at the last look. Persisted across restarts; pruned on write.
	WarehouseMemo map[int64]warehouseMemo `json:"warehouseMemo,omitempty"`
}
