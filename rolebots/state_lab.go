package rolebots

// LabState is the research-job errand (labwork_lane.go). It survives re-plan: the lane goes idle
// between install and collect (TestIdleDataCarriesLabJob).
type LabState struct {
	// Blueprint research trip (labwork.go): the print carried, the lab station and the factory it
	// belongs at. A lab job is a second slot track (ramJobs activityID <> 1), so the Ind* fields
	// cannot hold it. LabFailAt and LabCheckedAt are pilot-level and survive idleData.
	LabJobID        int64  `json:"labJobID,omitempty"`
	LabBPItemID     int64  `json:"labBPItemID,omitempty"`
	LabBPTypeID     int64  `json:"labBPTypeID,omitempty"`
	LabBPName       string `json:"labBPName,omitempty"`
	LabActivityID   int64  `json:"labActivityID,omitempty"`
	LabStationID    int64  `json:"labStationID,omitempty"`
	LabStationSysID int64  `json:"labStationSysID,omitempty"`
	LabFactoryID    int64  `json:"labFactoryID,omitempty"`
	LabFactorySysID int64  `json:"labFactorySysID,omitempty"`
	LabEndsAt       int64  `json:"labEndsAt,omitempty"`
	// LabCopies / LabCopyRuns are the trip's copy order when LabActivityID
	// is 5 (copying): copies to make and runs on each (bpcopy.go).
	LabCopies    int   `json:"labCopies,omitempty"`
	LabCopyRuns  int64 `json:"labCopyRuns,omitempty"`
	// LabLevels is the trip's research batch when LabActivityID is 3 or 4: how many levels the one
	// job adds (labResearchLevels). 0 reads as 1.
	LabLevels int `json:"labLevels,omitempty"`
	LabFailAt    int64 `json:"labFailAt,omitempty"`
	LabCheckedAt int64 `json:"labCheckedAt,omitempty"`

	// LabCooking is every research job this pilot has left on a lab line. An installed job leaves the
	// serial trip fields so the next install starts at once. Busy slots are counted from ramJobs
	// (labFreeSlots), so the list is bookkeeping: an ESP cancel cannot leave a slot looking occupied.
	LabCooking []LabCookingJob `json:"labCooking,omitempty"`
}
