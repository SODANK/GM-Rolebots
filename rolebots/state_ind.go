package rolebots

// IndState is the industry project (industry.go): the print, the factory, the job on the line,
// what it has cost. It survives re-plan: mining trips inside a project pass through idleData, and
// a dropped field burns the blueprint ISK mid-loop (TestIdleDataCarriesIndustryProject).
type IndState struct {
	// Industry project fields are about the bot, not the trip, so idleData (mining.go) carries all
	// of them. IndJobID == 0 with IndFactoryID set means the project is gathering materials; a
	// non-zero IndJobID means the job is cooking until industryResume sees it ready.
	IndTypeID   int64  `json:"indTypeID,omitempty"` // product being built
	IndTypeName string `json:"indTypeName,omitempty"`

	// IndEvidence is the build book tier that put this project on the list (buildbook.go): ledger,
	// shelf, bid or gap. The claim, the shelf write and the log need to know why it is made.
	IndEvidence     string  `json:"indEvidence,omitempty"`
	IndBPTypeID     int64   `json:"indBPTypeID,omitempty"`  // blueprint type
	IndBPItemID     int64   `json:"indBPItemID,omitempty"`  // owned BPO entity (0 = not bought yet)
	IndFactoryID    int64   `json:"indFactoryID,omitempty"` // build station
	IndFactorySysID int64   `json:"indFactorySysID,omitempty"`
	IndRuns         int     `json:"indRuns,omitempty"`
	IndJobID        int64   `json:"indJobID,omitempty"`    // ramJobs row (0 = not installed)
	IndBPOCost      float64 `json:"indBPOCost,omitempty"`  // what the BPO cost (amortized into price)
	IndMatValue     float64 `json:"indMatValue,omitempty"` // market value of consumed minerals
	IndFees         float64 `json:"indFees,omitempty"`     // install + hourly line charge

	// IndSpentISK is the cash this project has cost: blueprint, materials bought at a till, install
	// fee. It feeds the log and panel, not pricing (industryUnitPrice uses the value fields).
	// Supply-bid fills are excluded: a bid is factory capital that outlives the project (industrybids.go).
	IndSpentISK float64 `json:"indSpentISK,omitempty"`
	// IndBPOPaid is the part of IndSpentISK that bought the print, kept apart so industryCostBasis
	// charges the batch one project's share of it, since an original serves further projects.
	IndBPOPaid         float64 `json:"indBPOPaid,omitempty"`
	IndMiningTrips     int     `json:"indMiningTrips,omitempty"`
	IndSellerStationID int64   `json:"indSellerStationID,omitempty"` // BPO seller (transient target)

	// IndFetchItemID is the pilot's own blueprint in another station's hangar (IndSellerStationID)
	// that the project will collect instead of buying. Kept apart from IndBPItemID: industryAdvance
	// reads a set IndBPItemID as "the print is in hand".
	IndFetchItemID int64 `json:"indFetchItemID,omitempty"`

	// IndEscrowID is the escrow listing the project is flying to claim its
	// print from (bestEscrowBPO), at IndSellerStationID.
	IndEscrowID int64 `json:"indEscrowID,omitempty"`

	// IndBPORehunt marks a project that lost its escrowed print to another buyer and already looked
	// for a second seller once (industryBuyBPO). A second loss abandons.
	IndBPORehunt   bool  `json:"indBPORehunt,omitempty"`
	IndSellerSysID int64 `json:"indSellerSysID,omitempty"`
	IndStartedAt   int64 `json:"indStartedAt,omitempty"` // unix; soft-stall abandonment clock
	// IndShortInstalls counts installs the line refused for materials the
	// project's own arithmetic said it had (industryInstall). One is a race;
	// a third in a row is a bill the pilot cannot read and the project ends.
	IndShortInstalls int `json:"indShortInstalls,omitempty"`

	// IndWaiting marks the pilot as sitting out the tail of a bake at its own factory
	// (industryWaitOutOven), so the "heading back" and "waiting it out" lines are said once per job.
	IndWaiting bool `json:"indWaiting,omitempty"`

	// IndDeadStops are counters this project flew to and bought nothing at: stationID to unix expiry
	// (industryDeadStopSec). The shopping trip skips them. Carried by idleData like every Ind* field,
	// cleared at project start and by industryClearProject.
	IndDeadStops map[int64]int64 `json:"indDeadStops,omitempty"`

	// IndBuyLines are the material lines (typeIDs) the shopping trip in flight will buy; empty means
	// the whole bill. Make-or-buy is per line (industryRefine), so the counter must not buy a
	// left-out line just because it stocks it. Set when a trip is planned, consumed by industryBuyMats.
	IndBuyLines []int64 `json:"indBuyLines,omitempty"`

	// Order-book sweep trip (sweep.go): sellers visited, which ones, and what the hold already carries
	// per material line (ore as its refined metal), since industryShortfall reads only the factory
	// hangar. Reset when a trip is planned at the factory.
	IndSweepStops   int             `json:"indSweepStops,omitempty"`
	IndSweepVisited []int64         `json:"indSweepVisited,omitempty"`
	IndSweepCovered map[int64]int64 `json:"indSweepCovered,omitempty"`

	// IndOreWanted is the ore hierarchy a materials mining trip flies with: ores that reprocess into
	// the minerals the project is short of, richest per m3 first. Empty means the generic hierarchy.
	IndOreWanted []string `json:"indOreWanted,omitempty"`

	// IndCorpWork is the corporation work order (corpwork.go) this project carries out on the
	// corporation's print and materials; IndCorpPO the purchase order (corplibrary.go) it buys a
	// library print under. Either makes the project the corporation's.
	IndCorpWork int64 `json:"indCorpWork,omitempty"`
	IndCorpPO   int64 `json:"indCorpPO,omitempty"`

	// IndCorpMove is the old factory office a factory move is carrying the
	// desk's stock off (corpfactorymove.go).
	IndCorpMove int64 `json:"indCorpMove,omitempty"`

	// IndPrintMove is the print move (rolebot_corp_print_moves) this pilot
	// is carrying from one of its corporation's bases to another
	// (corpprintmove.go).
	IndPrintMove int64 `json:"indPrintMove,omitempty"`

	// IndCooking is every job this pilot has left baking on a line. An installed job leaves the serial
	// project fields so the next project starts at once. The list is bookkeeping: busy lines are
	// counted from ramJobs (industryFreeSlots), up to what the Mass Production skills allow.
	IndCooking []IndCookingJob `json:"indCooking,omitempty"`

	// IndBookFailAt holds the activity off (unix) after a pass where the build book had candidates
	// but none passed the gate. Like RestockAfter, it stops a market sweep per candidate on every
	// plan and an appetite draw that keeps picking manufacturing and being refused.
	IndBookFailAt int64 `json:"indBookFailAt,omitempty"`

	// IndBaseMovedAt (unix) is when this pilot last re-based its factory, to stop it moving again at
	// once. Two stations in one system tie on jumps, so without it the pilot trades between them
	// as their queues fill and empty.
	IndBaseMovedAt int64 `json:"indBaseMovedAt,omitempty"`

	// IndSplit* is a batch fanned out over several prints of one type (copylanes.go): the type, the
	// runs not yet on a line, and when the split began. The next project pass pins that type first.
	// Cleared when the runs are placed, the type leaves the book, or industrySplitTTLSec passes.
	IndSplitBPTypeID int64 `json:"indSplitBPTypeID,omitempty"`
	IndSplitRunsLeft int   `json:"indSplitRunsLeft,omitempty"`
	IndSplitAt       int64 `json:"indSplitAt,omitempty"`

	// IndExtraEscrowIDs are further copy listings of the project's type at the same seller station,
	// claimed on the same trip as IndEscrowID. IndCarryPrints are the extras in the hold, unloaded at
	// install (or at the dock if the project is abandoned) for the split's later lanes.
	IndExtraEscrowIDs []int64 `json:"indExtraEscrowIDs,omitempty"`
	IndCarryPrints    []int64 `json:"indCarryPrints,omitempty"`
}
