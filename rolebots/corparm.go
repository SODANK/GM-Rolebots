package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"

	"gurimoon/corp"
)

// The corporation's arms: the org chart under a bot CEO.
//
// Positions are what a bot CEO appoints (officers, fleet seats, Industry seats, starbase seats).
// An arm is the layer under them: every enabled member belongs to exactly one, and it bends
// what the member does (armTerm, scoreActivities) towards the arm's work. An arm exists only
// when justified (armJustified). Arms have no switch. The word is "arm" because "division" means
// a corporation hangar division and "portfolio" is what a Director runs (armOfPortfolio).
// Humans are never placed by the pass; a position holder is recorded in that position's arm and
// joining an arm otherwise is an opt-in by mail (corparm_human.go).

// corpArm is one arm of a corporation.
type corpArm string

const (
	armSecurity corpArm = "security"
	armIndustry corpArm = "industry"
	armResearch corpArm = "research"
	armMining   corpArm = "mining"
	armSales    corpArm = "sales"
	// armInfrastructure keeps the corporation's starbases standing: fuel, strontium, guns, losses
	// and grid (posinfra.go). Justified only by a tower to run.
	armInfrastructure corpArm = "infrastructure"
	armHR             corpArm = "hr"
	armFinance        corpArm = "finance"
)

// armOrder is every arm, line arms first in the order a short-handed corporation fills them.
var armOrder = []corpArm{armIndustry, armResearch, armMining, armSecurity, armSales, armInfrastructure, armHR, armFinance}

// lineArms are the arms rank-and-file members are placed into. HR and Finance are staffed by
// their officers only: a bot in Finance would have nothing to fly.
var lineArms = []corpArm{armIndustry, armResearch, armMining, armSecurity, armSales, armInfrastructure}

func armLabel(a corpArm) string {
	switch a {
	case armSecurity:
		return "Security"
	case armIndustry:
		return "Industry"
	case armResearch:
		return "Research & Development"
	case armMining:
		return "Mining"
	case armSales:
		return "Sales"
	case armInfrastructure:
		return "Infrastructure"
	case armHR:
		return "Human Resources"
	case armFinance:
		return "Finance"
	}
	return "Pool"
}

// armTag is the short code titles and hangar names carry. It is ASCII because title text
// reaches the 2005 client, which decodes bytes as cp1252.
func armTag(a corpArm) string {
	switch a {
	case armSecurity:
		return "SEC"
	case armIndustry:
		return "IND"
	case armResearch:
		return "RND"
	case armMining:
		return "MIN"
	case armSales:
		return "SAL"
	case armInfrastructure:
		return "INF"
	case armHR:
		return "HR"
	case armFinance:
		return "FIN"
	}
	return ""
}

// armActivities is the work an arm's members are pulled towards.
func armActivities(a corpArm) []string {
	switch a {
	case armSecurity:
		return []string{ActCombat, ActPiracy}
	case armIndustry:
		return []string{ActIndustry}
	case armResearch:
		// Research is a trip of the industry umbrella, not an activity; ActResearch is the key the arm
		// and the trip share (industrytrip.go).
		return []string{ActResearch}
	case armMining:
		return []string{ActMining, ActIce}
	case armSales:
		return []string{ActTrade, ActCourier}
	case armInfrastructure:
		// Tower work is no catalogue activity: its legs pre-empt a member's plan (posErrandDue).
		// armStarbaseKey is read by armFit; armMultiplier gives the arm a hauler's lean.
		return []string{armStarbaseKey}
	}
	return nil
}

// armStarbaseKey is the Infrastructure arm's work key: no activity carries it, so no
// directive share does either.
const armStarbaseKey = "starbase"

// armOfPortfolio is the arm a Director's portfolio heads.
func armOfPortfolio(port string) corpArm {
	switch port {
	case portSecurity:
		return armSecurity
	case portOperations:
		return armMining
	case portCommerce:
		return armSales
	case portHR:
		return armHR
	case portInfrastructure:
		return armInfrastructure
	case portResearch:
		return armResearch
	}
	return ""
}

// portfolioOfArm is the portfolio that heads an arm, "" for the two arms headed by something
// else (Industry: the Industry Director; Finance: the Treasurer).
func portfolioOfArm(a corpArm) string {
	switch a {
	case armSecurity:
		return portSecurity
	case armMining:
		return portOperations
	case armSales:
		return portCommerce
	case armHR:
		return portHR
	case armInfrastructure:
		return portInfrastructure
	case armResearch:
		return portResearch
	}
	return ""
}

// armOfSeatActivity is the arm a fleet seat belongs to.
func armOfSeatActivity(act string) corpArm {
	switch act {
	case ActMining, ActIndMining, ActIce:
		return armMining
	case ActCombat, ActSecurity, ActPiracy:
		return armSecurity
	}
	return ""
}

// Tiers: what a member is in its arm.
const (
	armTierMember = 0
	armTierJunior = 1
	armTierHead   = 2
)

// ── the table ───────────────────────────────────────────────────────────────

// ensureArmTables is called from ensureTables. One row per member, keyed by characterID, since
// a pilot has one corporation and one arm. grantMask is the role bits the arm pass added
// (armRoleDelta), so only those are taken back.
func ensureArmTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rolebot_corp_arm_members (
			characterID   BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			arm           VARCHAR(16) NOT NULL,
			tier          TINYINT NOT NULL DEFAULT 0,
			since         BIGINT NOT NULL DEFAULT 0,
			assignedBy    BIGINT NOT NULL DEFAULT 0,
			grantMask     BIGINT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID),
			KEY idx_arm_corp (corporationID, arm)
		)`)
	if err != nil {
		return fmt.Errorf("ensureArmTables: %w", err)
	}
	// Who has been written the arms to choose from (corparm_human.go).
	if _, err := db.ExecContext(ctx, armInviteTableSQL); err != nil {
		return fmt.Errorf("ensureArmTables invites: %w", err)
	}
	return nil
}

// armRow is one member's place in its corporation's org chart.
type armRow struct {
	CharID     int64
	CorpID     int64
	Arm        corpArm
	Tier       int
	Since      int64
	AssignedBy int64
	GrantMask  uint64
	// SetBy is "ceo" for the dealing pass's placement and "human" for one the chair made on the
	// Corporate Portal, which the pass keeps.
	SetBy string
}

func (m *Mod) loadArmRows(ctx context.Context, corpID int64) (map[int64]armRow, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT characterID, corporationID, arm, tier, since, assignedBy, grantMask, setBy
		  FROM rolebot_corp_arm_members WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("loadArmRows: %w", err)
	}
	defer rows.Close()
	out := map[int64]armRow{}
	for rows.Next() {
		var r armRow
		var arm string
		if err := rows.Scan(&r.CharID, &r.CorpID, &arm, &r.Tier, &r.Since, &r.AssignedBy, &r.GrantMask, &r.SetBy); err != nil {
			return nil, err
		}
		r.Arm = corpArm(arm)
		out[r.CharID] = r
	}
	return out, rows.Err()
}

func (m *Mod) saveArmRow(ctx context.Context, r armRow) error {
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_arm_members
			(characterID, corporationID, arm, tier, since, assignedBy, grantMask, setBy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE corporationID = VALUES(corporationID), arm = VALUES(arm), tier = VALUES(tier),
			since = VALUES(since), assignedBy = VALUES(assignedBy), grantMask = VALUES(grantMask), setBy = VALUES(setBy)`,
		r.CharID, r.CorpID, string(r.Arm), r.Tier, r.Since, r.AssignedBy, r.GrantMask, armSetByOr(r.SetBy))
	if err != nil {
		return fmt.Errorf("saveArmRow %d: %w", r.CharID, err)
	}
	return nil
}

func (m *Mod) deleteArmRow(ctx context.Context, charID int64) {
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_arm_members WHERE characterID = ?`, charID)
}

// ── the index ───────────────────────────────────────────────────────────────

// armSeat is what the hot path needs to know about a pilot's arm.
type armSeat struct {
	CorpID int64
	Arm    corpArm
	Tier   int
}

// armIndex maps a pilot to its arm, the ceoIndex pattern: armTerm runs on every bot's planning
// pass, so it reads memory, not the table. It is rebuilt after every placement pass and CEO sweep.
var armIndex atomic.Pointer[map[int64]armSeat]

// armOf answers a pilot's arm in a corporation. A row left from a corporation the pilot has
// since left reads as no arm.
func armOf(charID, corpID int64) (armSeat, bool) {
	if charID == 0 || corpID == 0 {
		return armSeat{}, false
	}
	p := armIndex.Load()
	if p == nil {
		return armSeat{}, false
	}
	s, ok := (*p)[charID]
	if !ok || s.CorpID != corpID {
		return armSeat{}, false
	}
	return s, true
}

// armCount is how many pilots the index places in a corporation's arm.
func armCount(corpID int64, a corpArm) int {
	p := armIndex.Load()
	if p == nil {
		return 0
	}
	n := 0
	for _, s := range *p {
		if s.CorpID == corpID && s.Arm == a {
			n++
		}
	}
	return n
}

// researchOrdersCap is how many ME/PE orders a corporation keeps on its board:
// corpResearchOrdersBase plus one per R&D member, up to corpResearchOrdersArmMax
// (corplibplan.go).
func researchOrdersCap(corpID int64) int {
	return min(corpResearchOrdersBase+armCount(corpID, armResearch), corpResearchOrdersArmMax)
}

func (m *Mod) refreshArmIndex(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT characterID, corporationID, arm, tier FROM rolebot_corp_arm_members`)
	if err != nil {
		rlogf("rolebots: arm index: %v", err)
		return
	}
	defer rows.Close()
	idx := map[int64]armSeat{}
	for rows.Next() {
		var id int64
		var s armSeat
		var arm string
		if rows.Scan(&id, &s.CorpID, &arm, &s.Tier) == nil {
			s.Arm = corpArm(arm)
			idx[id] = s
		}
	}
	armIndex.Store(&idx)
}

// ── the planner's pull ──────────────────────────────────────────────────────

const (
	// armGain is how hard an arm pulls its members towards its own work, per unit of obedience;
	// armPullMax caps it.
	armGain    = 0.6
	armPullMax = 2.0
	// armDampPer and armDampMin: the other work a line-arm member could do is damped, never
	// closed, so a miner with an empty wallet may still haul.
	armDampPer = 0.25
	armDampMin = 0.6
	// armSupportTrade is the small lean HR and Finance staff have towards trade and hauling.
	armSupportTrade = 1.15
)

// armMultiplier is the pure arithmetic of an arm's pull on one activity.
func armMultiplier(a corpArm, slug string, obedience float64) float64 {
	acts := armActivities(a)
	if len(acts) == 0 || a == armInfrastructure {
		// Infrastructure's own work is the haul board's legs, which pre-empt the plan; between legs
		// its people lean to hauling.
		if slug == ActTrade || slug == ActCourier {
			return armSupportTrade
		}
		return 1
	}
	for _, act := range acts {
		if act == slug {
			return min(armPullMax, 1+armGain*obedience)
		}
	}
	return max(armDampMin, 1-armDampPer*obedience)
}

// armTerm is the multiplier a member's arm puts on one activity (scoreActivities). It is 1 for
// the CEO and for a pilot in no arm. A corporation without a bot CEO has arm rows only when the
// chair placed members on the Corporate Portal, and those steer too.
func (m *Mod) armTerm(b *botRun, slug string) float64 {
	if b == nil || b.CorpID == 0 {
		return 1
	}
	s, ok := armOf(b.CharID, b.CorpID)
	if !ok {
		return 1
	}
	if ceoID, managed := managedCEO(b.CorpID); managed && ceoID == b.CharID {
		return 1
	}
	return armMultiplier(s.Arm, slug, b.Tune.Obedience)
}

// armSetByOr is a row's setter, "ceo" when unsaid.
func armSetByOr(s string) string {
	if s == "" {
		return postSetCEO
	}
	return s
}

// ── ranks and titles ────────────────────────────────────────────────────────

// armRankNames is each line arm's name for the merit ladder's six rungs (corpmerit.go). The
// rung, points and pay are the ladder's; only the name says which arm it climbed in.
var armRankNames = map[corpArm][6]string{
	armSecurity:       {"Cadet", "Trooper", "Corporal", "Sergeant", "Staff Sergeant", "Master Sergeant"},
	armMining:         {"Prospector", "Miner", "Rock Hauler", "Senior Miner", "Rock Boss", "Master Miner"},
	armIndustry:       {"Apprentice", "Technician", "Fabricator", "Senior Fabricator", "Engineer", "Master Engineer"},
	armResearch:       {"Lab Assistant", "Researcher", "Associate Scientist", "Scientist", "Senior Scientist", "Principal Scientist"},
	armSales:          {"Clerk", "Broker", "Trader", "Senior Trader", "Merchant", "Master Merchant"},
	armInfrastructure: {"Rigger", "Tower Hand", "Tower Technician", "Senior Technician", "Tower Engineer", "Chief Engineer"},
	armHR:             {"Personnel Clerk", "Personnel Aide", "Recruiter", "Senior Recruiter", "Personnel Lead", "Personnel Veteran"},
	armFinance:        {"Ledger Clerk", "Bookkeeper", "Analyst", "Senior Analyst", "Controller", "Comptroller"},
}

// armRankTitle is the title text of a member with no position: its arm's rung, tagged with
// the arm.
func armRankTitle(a corpArm, rank int) string {
	names, ok := armRankNames[a]
	if !ok {
		return ""
	}
	return fmt.Sprintf("[%s] %s", armTag(a), names[clampRank(rank)])
}

// isArmRankTitle reports whether a title text is one an arm ladder wrote.
func isArmRankTitle(s string) bool {
	for a := range armRankNames {
		for r := range 6 {
			if armRankTitle(a, r) == s {
				return true
			}
		}
	}
	return false
}

// armHeadTitle is the title text an arm's head wears. Industry's is the Industry Director's
// own (corpdirector.go) and Finance's the Treasurer slot.
func armHeadTitle(a corpArm) string {
	switch a {
	case armSecurity:
		return "Director of Security"
	case armMining:
		return "Director of Mining"
	case armSales:
		return "Director of Sales"
	case armHR:
		return "Director of Personnel"
	case armInfrastructure:
		return "Director of Infrastructure"
	case armResearch:
		return "Director of Research"
	}
	return ""
}

// ── role bundles ────────────────────────────────────────────────────────────

// armIndustryRoles is what every member of the Industry arm holds: Factory Manager plus query
// on every division, which covers the corporation-job gate (db.ResolveStationCorpJob). See
// docs/comment-notes-b03.md#arm-role-bundles.
const armIndustryRoles = corp.RoleFactoryManager | armQueryAll

// armCorpJobGate is the corporation-job gate itself (db.ResolveStationCorpJob): Factory
// Manager with query on Materials, Library and Production.
const armCorpJobGate = corp.RoleFactoryManager |
	corp.RoleHangarCanQuery1 | corp.RoleHangarCanQuery2 | corp.RoleHangarCanQuery3

// armRoleMask is every bit the arm pass ever grants; armRoleDelta never touches a bit outside
// it.
const armRoleMask = corp.RoleSecurityOfficer | corp.RoleStationManager | corp.RoleAuditor |
	corp.RoleEquipmentConfig | corp.RoleCanRentOffice | corp.RoleJuniorAccountant | corp.RoleChatManager |
	armIndustryRoles | armInfraRoles

// armInfraRoles is what every member of Infrastructure holds: query and take on the Starbase
// Kit and Fuel divisions, to load a fuel, ammo or rig leg. The head also configures starbases
// (the Fuel Technician's bit), which the anchor and online seams stand in for.
const armInfraRoles = corp.RoleHangarCanQuery6 | corp.RoleHangarCanQuery7 |
	corp.RoleHangarCanTake6 | corp.RoleHangarCanTake7 | corp.RoleStarbaseConfig

// armRoles is what a member of an arm at a tier holds by virtue of the arm. Officer bits are
// never here, and a benched member holds no Factory Manager. See
// docs/comment-notes-b03.md#arm-role-bundles.
func armRoles(a corpArm, tier int, benched bool) uint64 {
	switch a {
	case armIndustry, armResearch:
		if benched {
			return armIndustryRoles &^ corp.RoleFactoryManager
		}
		return armIndustryRoles
	case armSecurity:
		switch tier {
		case armTierHead:
			return corp.RoleSecurityOfficer | corp.RoleEquipmentConfig
		case armTierJunior:
			return corp.RoleSecurityOfficer
		}
	case armHR:
		if tier == armTierHead {
			return corp.RoleStationManager | corp.RoleCanRentOffice | corp.RoleChatManager
		}
	case armFinance:
		if tier == armTierHead {
			return corp.RoleAuditor | corp.RoleJuniorAccountant
		}
	case armInfrastructure:
		if tier == armTierMember {
			return armInfraRoles &^ corp.RoleStarbaseConfig
		}
		return armInfraRoles
	}
	return 0
}

// armRoleDelta is meritRoleDelta's rule for the arm's bits: remove only what the arm added,
// adopt nothing that was already there.
func armRoleDelta(current, grantMask, target uint64) (roles, newGrant uint64) {
	return meritRoleDelta(current, grantMask, target&armRoleMask)
}
