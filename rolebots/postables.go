package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

// Bot corporations and starbases: the tables, the switches and the shared vocabulary
// (docs/STARBASES.md is the design).
// The core runs a POS end to end without a pilot (plant, online, fuel burn, harvest to silo to
// reactor, refining, lifecycle events), so this subsystem adds the management layer: who decides
// a tower goes up and what it is for (the CEO's starbase power, posceo.go), who keeps it fuelled
// (the Fueler, posfuel.go), who buys its supplies (the Procurer, posreq.go), who carries things
// between offices, towers and hubs (the haul board, poshaul.go), how it finds a moon (surveys,
// posintel.go), how it is defended (posdefense.go, possiege.go) and what the moon's output
// becomes (posindustry.go). Every table is rolebot_-prefixed and inert without the module.

// Server settings.
const settingStarbasePower = "rolebots.starbase.enabled"

var starbasePowerV atomic.Bool

// StarbasePower reports whether bot CEOs may plant and run towers.
func StarbasePower() bool { return starbasePowerV.Load() }

// loadStarbaseSettings reads the starbase power switch (default on). Ice and sieges are standing
// behaviour with no switch.
func (m *Mod) loadStarbaseSettings(ctx context.Context) {
	starbasePowerV.Store(true)
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingStarbasePower); err != nil {
		rlogf("rolebots: read %s: %v (using the default)", settingStarbasePower, err)
	} else if ok {
		starbasePowerV.Store(strings.TrimSpace(v) == "1")
	}
}

// saveStarbasePower writes the starbase power switch and updates its cache.
func (m *Mod) saveStarbasePower(ctx context.Context, on bool) error {
	val := "0"
	if on {
		val = "1"
	}
	if err := evedb.SetServerSetting(ctx, m.db, settingStarbasePower, val); err != nil {
		return err
	}
	starbasePowerV.Store(on)
	return nil
}

// Office divisions the starbase desk uses, beside the industry desk's 1-4 (corpfactory.go).
// Division 6 is the staging shelf for a tower's kit (the tower, its structures, its ammunition)
// and division 7 is Fuel.
const (
	corpDivStarbase = 120 // division 6
	corpDivFuel     = 121 // division 7
)

var starbaseDivisionNames = map[int]string{6: "Starbase Kit", 7: "Fuel"}

// Tower purposes. A tower is planted for one of these (posplan.go).
const (
	purposeMining    = "mining"
	purposeReactions = "reactions"
	purposeFactory   = "factory"
	purposeLab       = "lab"
	purposeDepot     = "depot" // retired: no new depot is planned; a standing one is reconsidered
	purposeFortress  = "fortress"
	purposeRefinery  = "refinery"
)

var purposeLabel = map[string]string{
	purposeMining:    "Moon mining",
	purposeReactions: "Reactions",
	purposeFactory:   "Factory",
	purposeLab:       "Laboratory",
	purposeDepot:     "Depot",
	purposeFortress:  "Fortress",
	purposeRefinery:  "Refinery",
}

// Tower plan states.
const (
	towerPlanned   = "planned"   // kit requisitioned, not all in hand
	towerStaging   = "staging"   // kit in hand at the staging office, deploy leg posted
	towerAnchored  = "anchored"  // tower anchored, anchoring/onlining
	towerRigging   = "rigging"   // tower online, structures being hauled/anchored
	towerRunning   = "running"   // online and doing its job
	towerUnanchor  = "unanchor"  // teardown under way
	towerGone      = "gone"      // removed, destroyed or abandoned
)

// ensureStarbaseTables is called from ensureTables.
func ensureStarbaseTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		// One row per tower a managed corporation plans or runs. planID is the identity (a tower has no
		// itemID until anchored); layout is the JSON structure list (posplan.go towerLayout).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_towers (
			planID           BIGINT NOT NULL AUTO_INCREMENT,
			corporationID    BIGINT NOT NULL,
			towerID          BIGINT NOT NULL DEFAULT 0,
			moonID           BIGINT NOT NULL DEFAULT 0,
			systemID         BIGINT NOT NULL DEFAULT 0,
			purpose          VARCHAR(12) NOT NULL DEFAULT '',
			size             VARCHAR(1) NOT NULL DEFAULT 'S',
			towerTypeID      BIGINT NOT NULL DEFAULT 0,
			status           VARCHAR(10) NOT NULL DEFAULT 'planned',
			layout           TEXT NULL,
			stagingStationID BIGINT NOT NULL DEFAULT 0,
			plannedAt        BIGINT NOT NULL DEFAULT 0,
			anchoredAt       BIGINT NOT NULL DEFAULT 0,
			onlineAt         BIGINT NOT NULL DEFAULT 0,
			fuelerID         BIGINT NOT NULL DEFAULT 0,
			lastRunAt        BIGINT NOT NULL DEFAULT 0,
			urgentAt         BIGINT NOT NULL DEFAULT 0,
			reinforcedAt     BIGINT NOT NULL DEFAULT 0,
			losses           INT NOT NULL DEFAULT 0,
			badReviews       INT NOT NULL DEFAULT 0,
			reviewAt         BIGINT NOT NULL DEFAULT 0,
			closedAt         BIGINT NOT NULL DEFAULT 0,
			note             VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (planID),
			KEY idx_rbct_corp (corporationID, status),
			KEY idx_rbct_tower (towerID),
			KEY idx_rbct_moon (moonID)
		)`,
		// A requisition: something a seat wants delivered to an office division, bought by the Procurer
		// (posreq.go): patient lines are bid for, urgent ones bought on a funded trip.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_requisition (
			reqID         BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			typeID        BIGINT NOT NULL,
			qty           BIGINT NOT NULL DEFAULT 0,
			qtyDone       BIGINT NOT NULL DEFAULT 0,
			destStationID BIGINT NOT NULL DEFAULT 0,
			destFlag      INT NOT NULL DEFAULT 4,
			requester     VARCHAR(12) NOT NULL DEFAULT '',
			refKind       VARCHAR(12) NOT NULL DEFAULT '',
			refID         BIGINT NOT NULL DEFAULT 0,
			deadline      BIGINT NOT NULL DEFAULT 0,
			budget        DOUBLE NOT NULL DEFAULT 0,
			spent         DOUBLE NOT NULL DEFAULT 0,
			status        VARCHAR(8) NOT NULL DEFAULT 'open',
			buyerID       BIGINT NOT NULL DEFAULT 0,
			advance       DOUBLE NOT NULL DEFAULT 0,
			orderID       BIGINT NOT NULL DEFAULT 0,
			openedAt      BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (reqID),
			KEY idx_rbcr_corp (corporationID, status),
			KEY idx_rbcr_buyer (buyerID, status),
			KEY idx_rbcr_ref (refKind, refID)
		)`,
		// What a corporation knows about moons: only what it surveyed, or an ally or friend surveyed
		// (posintel.go). results is the JSON material list; value is ISK/hour a harvester array makes.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_moon_intel (
			corporationID BIGINT NOT NULL,
			moonID        BIGINT NOT NULL,
			systemID      BIGINT NOT NULL DEFAULT 0,
			results       TEXT NULL,
			bestClass     INT NOT NULL DEFAULT -1,
			value         DOUBLE NOT NULL DEFAULT 0,
			surveyedAt    BIGINT NOT NULL DEFAULT 0,
			surveyorID    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, moonID),
			KEY idx_rbcmi_sys (corporationID, systemID),
			KEY idx_rbcmi_value (corporationID, bestClass, value)
		)`,
		// A survey order: one system's moons to probe.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_survey (
			surveyID      BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			systemID      BIGINT NOT NULL,
			status        VARCHAR(8) NOT NULL DEFAULT 'open',
			surveyorID    BIGINT NOT NULL DEFAULT 0,
			claimedAt     BIGINT NOT NULL DEFAULT 0,
			moonsDone     INT NOT NULL DEFAULT 0,
			wagePerMoon   DOUBLE NOT NULL DEFAULT 0,
			postedAt      BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (surveyID),
			KEY idx_rbcs_corp (corporationID, status),
			KEY idx_rbcs_sys (systemID, status)
		)`,
		// Every tower's P&L (posledger.go): fuel, ammo and structures out;
		// product in at market value, internal transfers included.
		`CREATE TABLE IF NOT EXISTS rolebot_tower_ledger (
			entryID       BIGINT NOT NULL AUTO_INCREMENT,
			planID        BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			at            BIGINT NOT NULL DEFAULT 0,
			line          VARCHAR(12) NOT NULL DEFAULT '',
			isk           DOUBLE NOT NULL DEFAULT 0,
			typeID        BIGINT NOT NULL DEFAULT 0,
			qty           BIGINT NOT NULL DEFAULT 0,
			detail        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (entryID),
			KEY idx_rbtl_plan (planID, at),
			KEY idx_rbtl_corp (corporationID, at)
		)`,
		// The haul board (poshaul.go): a leg from a tower, silo or office division to a tower, office
		// division or market hub. items is the JSON want-list [{t, q}]; carrying is the hold now [{i, t, q, f}].
		`CREATE TABLE IF NOT EXISTS rolebot_corp_haul (
			legID         BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			kind          VARCHAR(10) NOT NULL DEFAULT '',
			planID        BIGINT NOT NULL DEFAULT 0,
			fromKind      VARCHAR(8) NOT NULL DEFAULT '',
			fromID        BIGINT NOT NULL DEFAULT 0,
			fromStationID BIGINT NOT NULL DEFAULT 0,
			fromSystemID  BIGINT NOT NULL DEFAULT 0,
			fromFlag      INT NOT NULL DEFAULT -1,
			toKind        VARCHAR(8) NOT NULL DEFAULT '',
			toID          BIGINT NOT NULL DEFAULT 0,
			toStationID   BIGINT NOT NULL DEFAULT 0,
			toSystemID    BIGINT NOT NULL DEFAULT 0,
			toFlag        INT NOT NULL DEFAULT 0,
			items         TEXT NULL,
			m3            DOUBLE NOT NULL DEFAULT 0,
			wage          DOUBLE NOT NULL DEFAULT 0,
			deadline      BIGINT NOT NULL DEFAULT 0,
			carrierID     BIGINT NOT NULL DEFAULT 0,
			claimedAt     BIGINT NOT NULL DEFAULT 0,
			status        VARCHAR(8) NOT NULL DEFAULT 'open',
			carrying      TEXT NULL,
			postedBy      VARCHAR(12) NOT NULL DEFAULT '',
			postedAt      BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			trips         INT NOT NULL DEFAULT 0,
			contractID    BIGINT NOT NULL DEFAULT 0,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (legID),
			KEY idx_rbch_corp (corporationID, status),
			KEY idx_rbch_carrier (carrierID, status),
			KEY idx_rbch_plan (planID, status)
		)`,
		// The network (posindustry.go): which tower feeds which with what.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_chain (
			corporationID BIGINT NOT NULL,
			fromPlanID    BIGINT NOT NULL,
			toPlanID      BIGINT NOT NULL,
			typeID        BIGINT NOT NULL,
			PRIMARY KEY (corporationID, fromPlanID, toPlanID, typeID)
		)`,
		// A siege of a tower (possiege.go).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_siege (
			siegeID        BIGINT NOT NULL AUTO_INCREMENT,
			towerID        BIGINT NOT NULL,
			systemID       BIGINT NOT NULL DEFAULT 0,
			attackerCorpID BIGINT NOT NULL DEFAULT 0,
			defenderCorpID BIGINT NOT NULL DEFAULT 0,
			exitAt         BIGINT NOT NULL DEFAULT 0,
			phase          VARCHAR(10) NOT NULL DEFAULT 'bash',
			result         VARCHAR(10) NOT NULL DEFAULT '',
			damage         DOUBLE NOT NULL DEFAULT 0,
			lastAt         BIGINT NOT NULL DEFAULT 0,
			openedAt       BIGINT NOT NULL DEFAULT 0,
			closedAt       BIGINT NOT NULL DEFAULT 0,
			note           VARCHAR(255) NOT NULL DEFAULT '',
			stageAt        BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (siegeID),
			KEY idx_rbcsg_tower (towerID, closedAt),
			KEY idx_rbcsg_att (attackerCorpID, closedAt),
			KEY idx_rbcsg_def (defenderCorpID, closedAt)
		)`,
		// The role bits the starbase seats were granted (posseats.go), kept apart from
		// rolebot_corp_grants (the industry pass) so neither pass takes back a bit the other granted.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_pos_grants (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			mask          BIGINT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID)
		)`,
		// The retired tower-resident cap: towers shelter whoever their force field admits.
		`DROP TABLE IF EXISTS rolebot_tower_residents`,
		// One job per tower array (posjobs.go).
		towerJobDDL,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("ensureStarbaseTables: %w", err)
		}
	}
	// Columns added after first ship (MariaDB supports IF NOT EXISTS on ADD COLUMN). The Trade desk's
	// lines (corptrade.go) carry the seller the run was priced at, the most it may pay and the wage.
	for _, s := range []string{
		`ALTER TABLE rolebot_corp_requisition ADD COLUMN IF NOT EXISTS srcStationID BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_corp_requisition ADD COLUMN IF NOT EXISTS maxPrice DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_corp_requisition ADD COLUMN IF NOT EXISTS wage DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_corp_requisition ADD COLUMN IF NOT EXISTS wagePaid DOUBLE NOT NULL DEFAULT 0`,
		// When the line was last handed to a buyer, and when a buyer last set out on a trip for it: a
		// buyer that never sets out loses the line (procurerDesk, reqBuyerTimeoutSec).
		`ALTER TABLE rolebot_corp_requisition ADD COLUMN IF NOT EXISTS assignedAt BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_corp_requisition ADD COLUMN IF NOT EXISTS tripAt BIGINT NOT NULL DEFAULT 0`,
		// The arm that asked (corprequest.go); empty on older lines, read through reqRequesterArm.
		`ALTER TABLE rolebot_corp_requisition ADD COLUMN IF NOT EXISTS requesterArm VARCHAR(16) NOT NULL DEFAULT ''`,
		// The work order a line is being built under ('building' status), 0 = bought.
		`ALTER TABLE rolebot_corp_requisition ADD COLUMN IF NOT EXISTS workID BIGINT NOT NULL DEFAULT 0`,
		// When qtyDone last grew (posreq.go reqLineStalled); 0 on older rows and new lines, read as openedAt.
		`ALTER TABLE rolebot_corp_requisition ADD COLUMN IF NOT EXISTS progressAt BIGINT NOT NULL DEFAULT 0`,
		// When a siege entered its current stage (possiege.go siegeStalled); 0 on older rows, read as openedAt.
		`ALTER TABLE rolebot_corp_siege ADD COLUMN IF NOT EXISTS stageAt BIGINT NOT NULL DEFAULT 0`,
		// What a siege is on: a tower (towerID is the tower) or a conquerable station (towerID is the
		// stationID, stationsiege.go).
		`ALTER TABLE rolebot_corp_siege ADD COLUMN IF NOT EXISTS targetKind VARCHAR(8) NOT NULL DEFAULT 'tower'`,
		// The quota a siege opened on and how its fleet is doing against it (siegequota.go), and the
		// siege's own fleet (siegefleet.go).
		`ALTER TABLE rolebot_corp_siege ADD COLUMN IF NOT EXISTS quotaDPS DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_corp_siege ADD COLUMN IF NOT EXISTS regenEHP DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_corp_siege ADD COLUMN IF NOT EXISTS fleetDPS DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_corp_siege ADD COLUMN IF NOT EXISTS lowSince BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_corp_siege ADD COLUMN IF NOT EXISTS gangID BIGINT NOT NULL DEFAULT 0`,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("ensureStarbaseTables: %w", err)
		}
	}
	return nil
}
