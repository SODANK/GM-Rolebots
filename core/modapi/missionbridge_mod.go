package modapi

// The agent-mission seam, for a module whose pilots take missions from agents. Package
// main installs thin closures over handleAgentDoAction from
// cmd/gurimoon/rolebotmission_mod.go, called with no connection, so offers, rewards,
// standings, LP, chains and cooldowns follow the core's rules for a player exactly.
//
// Every call is made as a pilot docked at stationID talking to agentID. The outcome is
// read back from characterMissions, not from the conversation the core renders. The
// pocket of an accepted kill mission is reached through the site seam
// (MissionSiteID, sitebridge_mod.go).

// MissionHooks are the closures package main installs.
type MissionHooks struct {
	// Request asks the agent for a mission. It returns the offered (or already
	// accepted) missionID, or 0 and why when the agent offers nothing.
	Request func(charID, accountID, agentID, stationID int64) (missionID int64, why string, err error)
	// Accept takes the pending offer. The core conjures a courier's cargo at the
	// pickup station and lays out a kill mission's pocket.
	Accept func(charID, accountID, agentID, stationID int64) error
	// Decline turns the pending offer down, at the template's standing loss.
	Decline func(charID, accountID, agentID, stationID int64) error
	// Complete turns the mission in. done is false with the core's reason when an
	// objective is unmet; nothing is paid then.
	Complete func(charID, accountID, agentID, stationID int64) (done bool, why string, err error)
	// Quit abandons the accepted mission, at the template's standing loss. The
	// pocket and any courier cargo are removed.
	Quit func(charID, accountID, agentID, stationID int64) error
}

var missionHooks MissionHooks

// InstallMissions publishes the core's agent entry points. Package main calls it once
// from init(); a later call overwrites.
func InstallMissions(h MissionHooks) { missionHooks = h }

// Missions returns the installed seam. Every field may be nil on a core without the
// overlay; CanRunMissions reports whether it is set.
func Missions() MissionHooks { return missionHooks }

// CanRunMissions reports whether this core carries the seam.
func CanRunMissions() bool {
	h := missionHooks
	return h.Request != nil && h.Accept != nil && h.Decline != nil && h.Complete != nil && h.Quit != nil
}
