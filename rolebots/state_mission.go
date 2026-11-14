package rolebots

// MissionState is the agent mission in hand (missions.go). It survives re-plan: the mission row
// stays accepted in the core until it is turned in, quit or expires, so dropping the fields would
// strand it. Cleared only by missionClear.
type MissionState struct {
	// MissionID is characterMissions.missionID; 0 = no mission. MissionAgentID and
	// MissionAgentStation are the agent and where it sits, MissionTemplateID the template.
	MissionID           int64 `json:"missionID,omitempty"`
	MissionAgentID      int64 `json:"missionAgentID,omitempty"`
	MissionAgentStation int64 `json:"missionAgentStation,omitempty"`
	MissionTemplateID   int64 `json:"missionTemplateID,omitempty"`
	// MissionKind is what the run does: missionKindKill, missionKindKillItem,
	// missionKindCourier or missionKindSupply (missionKindOf).
	MissionKind string `json:"missionKind,omitempty"`
	// MissionTurnIn is the station the turn-in happens at: the agent's, or the drop-off
	// for a delivery.
	MissionTurnIn int64 `json:"missionTurnIn,omitempty"`
	// MissionSource is where a supply mission buys its goods; MissionTries counts the
	// purchases that found the book empty.
	MissionSource int64 `json:"missionSource,omitempty"`
	MissionTries  int64 `json:"missionTries,omitempty"`
	// MissionStartedAt is when the agent was approached (the run record's start).
	MissionStartedAt int64 `json:"missionStartedAt,omitempty"`
}
