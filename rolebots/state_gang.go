package rolebots

// GangDuty is the gang this pilot is working for and the belt it was sent to. gangID is read by
// SQL (combat_abstract.go) and must stay top level under that name. Reset by idleData.
type GangDuty struct {
	// GangID pins the phase to the gang whose op it is running; GangBeltID is the belt the op's cans
	// cluster on (miners jettison there, the hauler collects there).
	GangID     int64 `json:"gangID,omitempty"`
	GangBeltID int64 `json:"gangBeltID,omitempty"`
	// GangSiteSeq is the gang's SiteSeq this pilot last planned against; a
	// gang whose site a Director moved has a newer one (gangStep re-plans).
	GangSiteSeq int64 `json:"gangSiteSeq,omitempty"`
	// GangOffBeltSince is when a watched gang miner was first found off its op belt with its
	// lasers cold (gangMineStep); 0 while it is working. Past OpBeltAbandonSec the op gives the
	// site up.
	GangOffBeltSince int64 `json:"gangOffBeltSince,omitempty"`
}
