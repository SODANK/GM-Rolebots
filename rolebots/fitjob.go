package rolebots

// A job is the lens a fit is scored through: what the ship would do at that work, from what is
// fitted. Ratting and missions share JobCombat. JobPiracy is separate: a pilot can warp off, so
// the mid rack must hold a target, not only tank.
const (
	JobCombat = "combat"
	JobMining = "mining"
	JobTrade  = "trade"
	JobPiracy = "piracy"
	// JobFreight is the lifter's lens: a trade hauler with every low slot an expander and no stabiliser.
	// No activity maps to it; jobForPilot gives it to an Infrastructure-arm pilot (freightlens.go).
	// A starbase kit has single units that the trade fit's hold is too small to take.
	JobFreight = "freight"
	// The gang seats. jobFor never returns them: a fleet judges a ship for the seat it fills
	// (a tackler by holding, a logi by mending). A solo pilot never wants one.
	JobTackle = "tackle"
	JobLogi   = "logi"
	// JobEwar is the electronic-warfare seat: damps, disruptors, painters
	// and jammers on the other side's guns, so the fleet takes less fire.
	JobEwar = "ewar"
	// JobScout is the gank fleet's eye and looter (gank.go): a cargo scanner, stabilisers and a
	// hold, and no guns. It is never an aggressor, so CONCORD does not target it.
	JobScout = "scout"
)

// JobDef describes one job for the panel.
type JobDef struct {
	Key   string
	Label string
	Help  string
}

// jobCatalog is the single source of truth for what jobs exist.
var jobCatalog = []JobDef{
	{JobCombat, "combat", "Belt ratting and missions. Scored on damage — turrets, launchers, drones — carried by effective HP."},
	{JobMining, "mining", "Scored on yield per minute, lifted by how much ore it can hold between trips."},
	{JobTrade, "trade / hauling", "Scored on cargo, lifted by how quickly it aligns out."},
	{JobFreight, "freight", "The Infrastructure arm's lifter. Scored on cargo alone — every low slot an expander, no stabilisers — because what it carries is a starbase kit whose biggest single unit decides whether the trip happens at all."},
	{JobPiracy, "piracy", "Robbing other pilots. Scored like combat but multiplied by whether it can hold prey still — a scrambler or web is worth more than another gun, and a fit with neither barely scores at all. A cargo scanner lifts it again: it is what a ransom is priced on."},
	{JobTackle, "tackle", "The fleet's holding seat. Scored on the point, the web and the speed to land them — a fit with no scrambler scores nothing, however hard it hits."},
	{JobLogi, "logistics", "The fleet's mending seat. Scored on remote repair a second, sustained by capacitor — a fit with no remote repairer scores nothing. Gated on the emission skills."},
	{JobEwar, "electronic warfare", "The fleet's disruption seat. Scored on how much incoming fire its damps, disruptors and jammers take off the fleet, carried by its own survival — a fit with no EWAR module scores nothing."},
	{JobScout, "scout", "The gank fleet's eye and looter. Scored on cargo, lifted by warp core stabilisers — a fit with no cargo scanner scores nothing, and it carries no guns at all."},
}

// Seat pseudo-activities. The appetite draw never offers them (not in activityCatalog), but the
// ship pipeline is keyed on an activity slug, so a gang seat uses them to buy the right hull.
// seatActivityFor (gangplan.go) hands them out.
const (
	ActSeatTackle = "seat.tackle"
	ActSeatLogi   = "seat.logi"
	ActSeatEwar   = "seat.ewar"
	ActSeatScout  = "seat.scout"
)

// jobFor maps an activity slug to the job its ship is judged for.
func jobFor(activity string) string {
	switch activity {
	case ActMining, ActIce:
		// Ice is a mining trip.
		return JobMining
	case ActTrade:
		return JobTrade
	case ActPiracy:
		return JobPiracy
	case ActIndustry:
		// A manufacturer's flying is logistics, judged as a hauler.
		return JobTrade
	case ActSeatTackle:
		return JobTackle
	case ActSeatLogi:
		return JobLogi
	case ActSeatEwar:
		return JobEwar
	case ActSeatScout:
		return JobScout
	default:
		return JobCombat
	}
}

func jobLabel(job string) string {
	for _, d := range jobCatalog {
		if d.Key == job {
			return d.Label
		}
	}
	return job
}
