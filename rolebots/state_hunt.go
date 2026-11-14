package rolebots

// HuntState is the ratting trip in progress (combatplan.go): the system and belt picked, belts
// already looked at, the tactic, the gate camp. Reset by idleData.
type HuntState struct {
	// Solo belt hunting. HuntSystemID is the system being swept, HuntPickedAt when the sweep
	// started; HuntRecent is the tail of systems already combed. A gang's version lives on the
	// gang blackboard (GangState).
	HuntSystemID int64 `json:"huntSystemID,omitempty"`
	HuntPickedAt int64 `json:"huntPickedAt,omitempty"`

	// HuntBeltID is the belt this pilot picked to work, and HuntBeltAt when it picked it
	// (combat_abstract.go). It is the hunter's equivalent of MineBeltID: without it beltForBot falls
	// back to the belt nearest the ship, which ignores the contents the system was chosen against.
	HuntBeltID int64 `json:"huntBeltID,omitempty"`
	HuntBeltAt int64 `json:"huntBeltAt,omitempty"`

	// HuntSkipBelts are belts in HuntSkipSys this pilot looked at and turned down (declined the
	// spawn, or found nothing). Without the list a patrol redraws the same persistent spawn and
	// refuses it all stint (combat_abstract.go). Cleared when the pilot works a different system.
	HuntSkipBelts []int64 `json:"huntSkipBelts,omitempty"`
	HuntSkipSys   int64   `json:"huntSkipSys,omitempty"`

	// HuntDryAt is when the sweep ran out of belts worth working in this system (nothing winnable,
	// nothing unvisited). The sweep ends its stint early on it.
	HuntDryAt int64 `json:"huntDryAt,omitempty"`

	// Which tactic this stint is (combattactic.go): sweep, distress call, gate hold, roam or mark.
	// Its recent tail is the TacticRecent ring, not the Worked map, which is keyed by activity
	// (activityHalfLife, appetite.go).
	CombatTactic string `json:"combatTactic,omitempty"`

	// UmbTrip is the industry umbrella's sub-trip (industrytrip.go): rock, ice, build or research,
	// with UmbRecent as its ring. Same reasoning as TacticRecent.
	UmbTrip      string `json:"umbTrip,omitempty"`
	CombatGateID int64  `json:"combatGateID,omitempty"`

	// Whether the gate hold has taken its spot yet: parking is done once on arrival, not every
	// pass, as PiracyCampSet does it.
	CombatCampSet bool  `json:"combatCampSet,omitempty"`
	CombatMarkID  int64 `json:"combatMarkID,omitempty"`

	// The open site this combat stint is running (siterun.go): the core's
	// dunOpenSites.siteID, which room of it the pilot is on, whether that room
	// has been entered (its roomEnter pass fired), and when the run began.
	SiteID        int64 `json:"siteID,omitempty"`
	SiteRoomIdx   int64 `json:"siteRoomIdx,omitempty"`
	SiteEntered   bool  `json:"siteEntered,omitempty"`
	SiteStartedAt int64 `json:"siteStartedAt,omitempty"`
	SiteKills     int64 `json:"siteKills,omitempty"`
}

// CombatMemory is what the combat loop remembers between trips: recent systems and tactics (so
// it spreads), and the loot can left behind that a later pass goes back for.
type CombatMemory struct {
	HuntRecent   []int64  `json:"huntRecent,omitempty"`
	TacticRecent []string `json:"tacticRecent,omitempty"`
	UmbRecent    []string `json:"umbRecent,omitempty"`

	// A can the rats left that this hunter has not emptied, and its system (beltloot.go). Kept apart
	// from the piracy pair: they point at different wrecks. Carried by idleData, since a can
	// despawns whether or not the pilot re-planned.
	HuntLootID    int64 `json:"huntLootID,omitempty"`
	HuntLootSysID int64 `json:"huntLootSysID,omitempty"`
	HuntLootAt    int64 `json:"huntLootAt,omitempty"`

	// The keep-or-donate call on the haul aboard (lootdonate.go), made once per haul so a re-plan
	// does not re-roll it. LootDonateOffice is the corporation office it goes to (0 = keep and
	// sell); LootDecidedAt lapses after lootDecisionTTL.
	LootDecidedAt     int64 `json:"lootDecidedAt,omitempty"`
	LootDonateOffice  int64 `json:"lootDonateOffice,omitempty"`
	LootDonateStation int64 `json:"lootDonateStation,omitempty"`
}
