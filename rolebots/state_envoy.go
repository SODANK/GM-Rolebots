package rolebots

// EnvoyState is one letter's trip (corpenvoy.go): where the envoy takes it and where it goes
// home. Not carried by idleData: a trip that gave up is over, and the seat on rolebot_corp_posts
// lets the planner re-pick the letter (envoyErrandDue reads the seat, never this).
type EnvoyState struct {
	EnvoyLetterID      int64 `json:"envoyLetterID,omitempty"`
	EnvoyStationID     int64 `json:"envoyStationID,omitempty"`
	EnvoySysID         int64 `json:"envoySysID,omitempty"`
	EnvoyHomeStationID int64 `json:"envoyHomeStationID,omitempty"`
	EnvoyHomeSysID     int64 `json:"envoyHomeSysID,omitempty"`
}
