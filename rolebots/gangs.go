package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Gangs: the data layer. A gang is a standing group of pilots that works one
// activity, set on the gang itself. While a gang has an activity, every rostered
// member's own rotation is suspended and its planner runs the gang op in its
// role. Members that are not rostered rolebots (players' characters) are never
// driven; the driven members work around them.
//
// The shared blackboard (system, ore, trade deal) is JSON in rolebot_gangs.state.
// Rebooting a gang clears it: the op re-plans and the world is untouched.

// Gang roles.
const (
	RoleMiner  = "miner"
	RoleHauler = "hauler"
	RoleCombat = "combat"
	RoleTrader = "trader"
	RoleHunter = "hunter"
	// The combat-fleet seats (gangform.go's composition table): a tackler holds
	// targets down and a logi keeps the wing alive. There is no looter seat; boot
	// moves any "looter" row to RoleCombat (migrateLooterSeat).
	RoleTackler = "tackler"
	RoleLogi    = "logi"
	// RoleEwar is the electronic-warfare seat (JobEwar): it holds the other side's
	// guns down from range.
	RoleEwar = "ewar"
	// RoleEscort is a combat pilot seconded from another corporation (gangescort.go)
	// to cover a fleet with no guns. It is its own role so the welcome re-check runs
	// only on escorts and the fleet report names the lending corporation.
	RoleEscort = "escort"
	// RoleScout is the gank fleet's unarmed eye and looter (JobScout, gank.go): it
	// cargo-scans arrivals on the camped gate, calls the strike and takes the
	// wrecks. Never an aggressor.
	RoleScout = "scout"
)

// gangRoles lists the roles in display order.
var gangRoles = []string{RoleMiner, RoleHauler, RoleCombat, RoleTrader, RoleHunter,
	RoleTackler, RoleLogi, RoleEwar, RoleScout, RoleEscort}

func validRole(r string) bool {
	for _, v := range gangRoles {
		if v == r {
			return true
		}
	}
	return false
}

// Gang is one rolebot_gangs row.
type Gang struct {
	GangID   int64
	Name     string
	Enabled  bool
	Activity string // "" = standby (members fall back to their own rotations)
	Config   string // raw JSON, shape per activity (same shapes as solo configs)
	State    GangState
	// LeaderCharID is the pilot whose Leadership skill sized this gang
	// (gangform.go). 0 on operator-built gangs, which cap at the flat size setting.
	LeaderCharID int64
	// CorpID is the corporation whose chain of command sent this fleet out
	// (corprank.go), written at formation. 0 on operator-built gangs.
	CorpID int64
	// AutoFormed marks a gang the formation sweep built on its own. Only
	// auto gangs are ever torn down by the sweep; ExpiresAt is when.
	AutoFormed bool
	ExpiresAt  int64
	Created    int64
	// DoctrineKind is the fleet doctrine this fleet was raised under
	// (fleetdoctrine.go); empty when none.
	DoctrineKind doctrineKind
}

// GangMember is one membership row.
type GangMember struct {
	GangID      int64
	CharacterID int64
	Role        string
	// Driven is true when the character is an enabled rolebot, which the sim
	// animates. False is a rider, usually a player's character.
	Driven bool
	// RecruitCorpID is the corporation this member was recruited from; 0 means the
	// gang's own. Non-zero marks a seconded escort.
	RecruitCorpID int64
	// The seat's spec (fleetdoctrine_seat.go): the hull band, tank layer and range
	// band the doctrine gave this seat. HullMax 0 means no spec (operator gang,
	// mining op) and reads as unspecified.
	HullMin, HullMax HullClass
	Tank             TankPref
	Style            combatStance
}

// gangFocusPvP marks a combat fleet formed to hunt pilots rather than rats: a
// small pack that flies to where the legal-target census says the fights are
// (legalcensus.go).
const gangFocusPvP = "pvp"

// gangFocusGank marks a gank fleet (gank_op.go): an unarmed scout on a policed
// gate, disposable destroyers staged one jump away, and a strike called on a
// hold worth it.
const gangFocusGank = "gank"

// gangFocusCheckpoint marks a Security or Mercenary fleet raised to hold a
// gate in its employer's region (camp.go) rather than sweep its belts.
const gangFocusCheckpoint = "checkpoint"

// GankCall is the strike the scout called: the mark, its position and cargo, and
// the strikers the arithmetic asked for. One per fleet; cleared when the mark is
// dead or the call goes stale.
type GankCall struct {
	VictimCharID int64   `json:"victimCharID"`
	VictimName   string  `json:"victimName,omitempty"`
	ShipID       int64   `json:"shipID"`
	ShipTypeID   int64   `json:"shipTypeID,omitempty"`
	EHP          float64 `json:"ehp"`
	ISK          float64 `json:"isk"`
	Ships        int     `json:"ships"`
	CalledAt     int64   `json:"calledAt"`
	X            float64 `json:"x,omitempty"`
	Y            float64 `json:"y,omitempty"`
	Z            float64 `json:"z,omitempty"`
}

type GangState struct {
	// Focus is gangFocusPvP for a pack formed to hunt pilots; empty for every
	// other fleet.
	Focus string `json:"focus,omitempty"`

	// Mining op: the system and belt the cans sit on, and the ore planned.
	SystemID   int64  `json:"systemID,omitempty"`
	SystemName string `json:"systemName,omitempty"`
	BeltItemID int64  `json:"beltItemID,omitempty"`
	OreTypeID  int64  `json:"oreTypeID,omitempty"`
	OreName    string `json:"oreName,omitempty"`
	PickedAt   int64  `json:"pickedAt,omitempty"`

	// Hunt op: the system currently being swept and when the sweep began.
	HuntSystemID   int64  `json:"huntSystemID,omitempty"`
	HuntSystemName string `json:"huntSystemName,omitempty"`
	HuntPickedAt   int64  `json:"huntPickedAt,omitempty"`
	// Recent hunt systems: the sweep prefers somewhere it has not just cleaned out.
	// Shared with the pirate op.
	HuntRecent []int64 `json:"huntRecent,omitempty"`
	// The distress the fleet was formed to answer (beltdistress.go): the hunt
	// planner's first sweep, spent once and then cleared.
	ReliefSystemID   int64  `json:"reliefSystemID,omitempty"`
	ReliefSystemName string `json:"reliefSystemName,omitempty"`
	// A department head's standing order the fleet is working (corpstaff_dept.go):
	// the ordered hunt system or ores. The CEO page shows which fleets took orders.
	OrderSystemID int64  `json:"orderSystemID,omitempty"`
	OrderOres     string `json:"orderOres,omitempty"`
	// A Director's order from fleet chat (fleetmirror_orders.go): the system the op
	// is pinned to, which the mining site picker prefers, and a counter each
	// re-siting bumps so a member mid-cycle re-plans (gangStep).
	PinSystemID int64 `json:"pinSystemID,omitempty"`
	SiteSeq     int64 `json:"siteSeq,omitempty"`
	// OrderID is the chair's order this fleet was raised for
	// (rolebot_corp_commands), 0 for a fleet the seat raised itself.
	OrderID int64 `json:"orderID,omitempty"`

	// Pirate op: the hunting ground the pack works together. A gate means the pack
	// camps that gate; 0 means everyone prowls the system's belts. Rotates on the
	// solo piracy stint clocks.
	PirSystemID   int64  `json:"pirSystemID,omitempty"`
	PirSystemName string `json:"pirSystemName,omitempty"`
	PirGateID     int64  `json:"pirGateID,omitempty"`
	PirPickedAt   int64  `json:"pirPickedAt,omitempty"`
	// Gank op (gank_op.go), on top of the pirate site: PirSystemID and PirGateID are
	// the scout's gate, GankFarSystemID is the lawless far end of the lane where
	// strikers stage, GankCall is the live strike.
	GankFarSystemID   int64     `json:"gankFarSystemID,omitempty"`
	GankFarSystemName string    `json:"gankFarSystemName,omitempty"`
	GankCall          *GankCall `json:"gankCall,omitempty"`
	// Undock gank (gank_undock.go): GankStationID != 0 puts the scout at that
	// station's undock, GankFarSystemID is the camp system, strikers lurk at the
	// stage safe and run to the escape safe after the strike.
	GankStationID    int64  `json:"gankStationID,omitempty"`
	GankStationName  string `json:"gankStationName,omitempty"`
	GankStageSafeID  int64  `json:"gankStageSafeID,omitempty"`
	GankEscapeSafeID int64  `json:"gankEscapeSafeID,omitempty"`
	// GankLootISK is what the scout has brought home this op, for the Op Board.
	GankLootISK float64 `json:"gankLootISK,omitempty"`

	// Checkpoint (camp.go): the gate the fleet holds, and when it took it.
	CheckpointSystemID   int64  `json:"checkpointSystemID,omitempty"`
	CheckpointSystemName string `json:"checkpointSystemName,omitempty"`
	CheckpointGateID     int64  `json:"checkpointGateID,omitempty"`
	CheckpointFarName    string `json:"checkpointFarName,omitempty"`
	CheckpointPickedAt   int64  `json:"checkpointPickedAt,omitempty"`
	// TollISK is what the checkpoint has collected this op, for the Op Board.
	TollISK float64 `json:"tollISK,omitempty"`

	// OpPhase is the op lifecycle (gangform.go, gangplan.go): "muster" gathers at
	// staging, "live" is the op, "winddown" returns with the haul, "settled" is
	// deleted by the sweep. Empty means "live" (operator-built gangs).
	OpPhase string `json:"opPhase,omitempty"`
	// Staging is the leader's home station, written once at formation. The muster
	// and the end-of-op settlement happen here.
	StagingStationID int64 `json:"stagingStationID,omitempty"`
	StagingSystemID  int64 `json:"stagingSystemID,omitempty"`
	// Deadlines. A muster past its deadline drops the stragglers; a wind-down past
	// its deadline settles with whoever made it back.
	MusterDeadline   int64 `json:"musterDeadline,omitempty"`
	WindDownDeadline int64 `json:"windDownDeadline,omitempty"`
	// MusterStartedAt is when the fleet began gathering; the release rule's grace
	// period runs from it (gangmuster.go).
	MusterStartedAt int64 `json:"musterStartedAt,omitempty"`
	// Tier is what the fleet was raised for and sized by (gangtier.go): hunt, roam,
	// mining, pack, order, calltoarms. Empty when unset.
	Tier string `json:"tier,omitempty"`
	// JoinBy is, for a fleet born live, until when its members are still called off
	// interruptible errands (gangCallUp).
	JoinBy int64 `json:"joinBy,omitempty"`
	// HelpCallID is the help call (rolebot_fleet_help, gangcalls.go) this fleet is
	// answering or was raised for; 0 otherwise.
	HelpCallID int64 `json:"helpCallID,omitempty"`
	// SiegeID is the siege this fleet was raised for (siegefleet.go): the siege keeps it alive
	// (leaseSiegeGangs) and its muster is judged on the siege's DPS quota. 0 otherwise.
	SiegeID int64 `json:"siegeID,omitempty"`
	// LastRunAt is when the wind-down's last collection run was called
	// (gangPlanWindDown): haulers go back for what is left in the cans. Set once,
	// and the deadline is extended once with it.
	LastRunAt int64 `json:"lastRunAt,omitempty"`
	// NoHaulerSince is when a live mining op was first seen with nobody answering in a
	// hauling seat (liveOpCoversHaul); 0 while it is covered.
	NoHaulerSince int64 `json:"noHaulerSince,omitempty"`
	// QuietSince is when a combat or security fleet first found nothing to do on its
	// ground (fleetQuiet); 0 once it has a site again.
	QuietSince int64 `json:"quietSince,omitempty"`

	// Capsuleer hunt (combat_botvbot.go): a bountied pilot this fleet has
	// committed to running down, and when the intercept matures.
	HuntTargetCharID int64 `json:"huntTargetCharID,omitempty"`
	HuntETA          int64 `json:"huntETA,omitempty"`
	// HuntArriveAt is when the slowest member reaches the mark (fleetmove.go); the
	// intercept does not resolve before it unless the whole fleet is there.
	HuntArriveAt int64 `json:"huntArriveAt,omitempty"`

	// The open site a hunt pack is running (siterun.go), set beside
	// HuntSystemID; 0 = an ordinary belt sweep.
	SiteID   int64  `json:"siteID,omitempty"`
	SiteName string `json:"siteName,omitempty"`

	// Objective is what a combat fleet is committed to (gangintent.go): every writer
	// of HuntSystemID goes through commitObjective, so one call cannot yank a fleet
	// off another. ObjSuppressed holds what was pre-empted.
	Objective     *GangObjective   `json:"objective,omitempty"`
	ObjSuppressed map[string]int64 `json:"objSuppressed,omitempty"`

	// Command is how the fleet is run while out (fleetcommand.go): the FC and its
	// succession, the call and the retreat rule. Written at formation from the
	// doctrine kind; nil on a fleet formed without one.
	Command *FleetCommand `json:"command,omitempty"`
}

// ── CRUD ────────────────────────────────────────────────────────────────────

func createGang(ctx context.Context, db *sql.DB, name string) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_gangs (name, enabled, createdDate, updatedAt)
		VALUES (?, 1, ?, ?)`, name, clock.Unix(), clock.Unix())
	if err != nil {
		return 0, fmt.Errorf("createGang: %w", err)
	}
	return res.LastInsertId()
}

func deleteGang(ctx context.Context, db *sql.DB, gangID int64) error {
	for _, q := range []string{
		`DELETE FROM rolebot_gang_members WHERE gangID = ?`,
		`DELETE FROM rolebot_gang_ore WHERE gangID = ?`,
		// The work log. Callers mail the after-action report before reaching here; it
		// is the only reader.
		`DELETE FROM rolebot_gang_haul WHERE gangID = ?`,
		`DELETE FROM rolebot_gangs WHERE gangID = ?`,
	} {
		if _, err := db.ExecContext(ctx, q, gangID); err != nil {
			return fmt.Errorf("deleteGang: %w", err)
		}
	}
	forgetGangBelts(gangID)
	return nil
}

func getGang(ctx context.Context, db *sql.DB, gangID int64) (*Gang, error) {
	g := &Gang{}
	var en, auto int64
	var cfg, state, kind sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT gangID, name, enabled, activity, config, state,
		       leaderCharID, corpID, autoFormed, expiresAt, createdDate, doctrineKind
		FROM rolebot_gangs WHERE gangID = ?`, gangID,
	).Scan(&g.GangID, &g.Name, &en, &g.Activity, &cfg, &state,
		&g.LeaderCharID, &g.CorpID, &auto, &g.ExpiresAt, &g.Created, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getGang: %w", err)
	}
	g.Enabled = en != 0
	g.AutoFormed = auto != 0
	g.Config = cfg.String
	g.DoctrineKind = doctrineKind(kind.String)
	if state.Valid && state.String != "" {
		// A corrupt blob starts the op over rather than wedging the gang.
		_ = json.Unmarshal([]byte(state.String), &g.State)
	}
	return g, nil
}

// setGangLeader records who leads. 0 clears the seat, so an operator-built gang
// caps at the flat size setting.
func setGangLeader(ctx context.Context, db *sql.DB, gangID, charID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE rolebot_gangs SET leaderCharID = ?, updatedAt = ? WHERE gangID = ?`,
		charID, clock.Unix(), gangID)
	if err != nil {
		return fmt.Errorf("setGangLeader: %w", err)
	}
	return nil
}

// countGangs is how many gangs exist, enabled or not.
func countGangs(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_gangs`).Scan(&n); err != nil {
		return 0, fmt.Errorf("countGangs: %w", err)
	}
	return n, nil
}

// gangSize is the current member headcount of one gang.
func gangSize(ctx context.Context, db *sql.DB, gangID int64) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rolebot_gang_members WHERE gangID = ?`, gangID).Scan(&n); err != nil {
		return 0, fmt.Errorf("gangSize: %w", err)
	}
	return n, nil
}

func saveGangState(ctx context.Context, db *sql.DB, gangID int64, st *GangState) error {
	blob, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("saveGangState marshal: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE rolebot_gangs SET state = ?, updatedAt = ? WHERE gangID = ?`,
		string(blob), clock.Unix(), gangID); err != nil {
		return fmt.Errorf("saveGangState: %w", err)
	}
	return nil
}

func addGangMember(ctx context.Context, db *sql.DB, gangID, charID int64, role string) error {
	return addGangMemberFrom(ctx, db, gangID, charID, role, 0)
}

// addGangMemberFrom is addGangMember plus the corporation the pilot was
// recruited from. Pass 0 for the gang's own corporation; only the escort pass
// (gangescort.go) passes another.
func addGangMemberFrom(ctx context.Context, db *sql.DB, gangID, charID int64, role string, recruitCorpID int64) error {
	return addGangMemberWithSpec(ctx, db, gangID, charID, role, recruitCorpID, seatSpec{})
}

// addGangMemberWithSpec is addGangMemberFrom plus the seat's spec
// (fleetdoctrine_seat.go). The zero spec writes a row with no band.
func addGangMemberWithSpec(ctx context.Context, db *sql.DB, gangID, charID int64, role string, recruitCorpID int64, spec seatSpec) error {
	if !validRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	var one int64
	if err := db.QueryRowContext(ctx,
		`SELECT 1 FROM character_ WHERE characterID = ?`, charID).Scan(&one); err != nil {
		return fmt.Errorf("character %d not found", charID)
	}
	// One gang per character: a pilot cannot fly two ops at once, and a character
	// in two gangs would have two haulers fighting over its cans.
	var otherGang int64
	err := db.QueryRowContext(ctx, `
		SELECT gangID FROM rolebot_gang_members WHERE characterID = ? AND gangID <> ?`,
		charID, gangID).Scan(&otherGang)
	if err == nil {
		return fmt.Errorf("character %d is already in gang %d — remove it there first", charID, otherGang)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("addGangMember: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_gang_members (gangID, characterID, role, recruitCorpID, addedDate,
			hullMin, hullMax, tank, style)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE role = VALUES(role),
		                        recruitCorpID = VALUES(recruitCorpID),
		                        hullMin = VALUES(hullMin), hullMax = VALUES(hullMax),
		                        tank = VALUES(tank), style = VALUES(style)`,
		gangID, charID, role, recruitCorpID, clock.Unix(),
		int(spec.HullMin), int(spec.HullMax), string(spec.Tank), int(spec.Style)); err != nil {
		return fmt.Errorf("addGangMember: %w", err)
	}
	forgetSeatSpec(charID)
	return nil
}

func removeGangMember(ctx context.Context, db *sql.DB, gangID, charID int64) error {
	if _, err := db.ExecContext(ctx, `
		DELETE FROM rolebot_gang_members WHERE gangID = ? AND characterID = ?`,
		gangID, charID); err != nil {
		return fmt.Errorf("removeGangMember: %w", err)
	}
	forgetSeatSpec(charID)
	return nil
}

// leaveGangMuster removes a pilot that bowed out of its fleet from the roster,
// unless the fleet is a live mining op, where the row is evidence an excused
// hauler may still carry ore (gangHoldsThisPilot). The commander's row stays:
// removing it would turn a wind-down into a leaderless hard kill.
func (m *Mod) leaveGangMuster(ctx context.Context, gangID, charID int64) {
	if gangID == 0 || charID == 0 {
		return
	}
	var raw, activity string
	var leader int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(state, ''), leaderCharID, COALESCE(activity, '') FROM rolebot_gangs WHERE gangID = ?`,
		gangID).Scan(&raw, &leader, &activity); err != nil {
		return
	}
	if leader == charID {
		return
	}
	var gs GangState
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &gs)
	}
	oreBooks := activity == ActMining || activity == ActIndMining
	if gs.OpPhase != gangOpMuster && oreBooks {
		return
	}
	if err := removeGangMember(ctx, m.db, gangID, charID); err != nil {
		rlogf("rolebots: leave muster %d from gang %d: %v", charID, gangID, err)
	}
}

// gangIDOf is the fleet a pilot belongs to: the roster row (b.GangID) first, the
// plan state's copy as the fallback for a run loaded without it.
func gangIDOf(b *botRun) int64 {
	if b == nil {
		return 0
	}
	if b.GangID != 0 {
		return b.GangID
	}
	if b.State != nil {
		return b.State.Data.GangID
	}
	return 0
}

// gangMembers lists a gang's members, flagging which ones the sim drives.
func gangMembers(ctx context.Context, db *sql.DB, gangID int64) ([]GangMember, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT gm.gangID, gm.characterID, gm.role, gm.recruitCorpID,
		       COALESCE(b.enabled, 0), gm.hullMin, gm.hullMax, gm.tank, gm.style
		FROM rolebot_gang_members gm
		LEFT JOIN rolebot_characters b ON b.characterID = gm.characterID
		WHERE gm.gangID = ?
		ORDER BY gm.role, gm.characterID`, gangID)
	if err != nil {
		return nil, fmt.Errorf("gangMembers: %w", err)
	}
	defer rows.Close()
	var out []GangMember
	for rows.Next() {
		var mem GangMember
		var driven int64
		var lo, hi, style int
		var tank string
		if err := rows.Scan(&mem.GangID, &mem.CharacterID, &mem.Role,
			&mem.RecruitCorpID, &driven, &lo, &hi, &tank, &style); err != nil {
			return nil, err
		}
		mem.Driven = driven != 0
		mem.HullMin, mem.HullMax = HullClass(lo), HullClass(hi)
		mem.Tank, mem.Style = TankPref(tank), combatStance(style)
		out = append(out, mem)
	}
	return out, rows.Err()
}

// gangForChar returns the enabled gang this character belongs to (nil when
// none), with the member's own role.
func gangForChar(ctx context.Context, db *sql.DB, charID int64) (*Gang, *GangMember, error) {
	var gangID, recruitCorpID int64
	var role string
	// ORDER BY: a pilot may sit in two enabled gangs, so take the lowest gangID,
	// the same MIN() loadRuns uses for the shard key (simpool.go).
	err := db.QueryRowContext(ctx, `
		SELECT gm.gangID, gm.role, gm.recruitCorpID
		FROM rolebot_gang_members gm
		JOIN rolebot_gangs g ON g.gangID = gm.gangID
		WHERE gm.characterID = ? AND g.enabled = 1
		ORDER BY gm.gangID
		LIMIT 1`, charID).Scan(&gangID, &role, &recruitCorpID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("gangForChar: %w", err)
	}
	g, err := getGang(ctx, db, gangID)
	if err != nil || g == nil {
		return nil, nil, err
	}
	return g, &GangMember{GangID: gangID, CharacterID: charID, Role: role,
		RecruitCorpID: recruitCorpID}, nil
}

// gangOfCharCached answers "which enabled gang is this character in" from a 60 s
// cache, for the combat tier, which asks about every pilot on a grid each tick.
// A stale answer errs toward not shooting a former gangmate.
func (m *Mod) gangOfCharCached(charID int64) int64 {
	if charID == 0 {
		return 0
	}
	now := clock.Unix()
	if v, ok := gangOfCache.Load(charID); ok {
		e := v.(gangOfEntry)
		if now-e.at < 60 {
			return e.gangID
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var gangID int64
	err := m.db.QueryRowContext(ctx, `
		SELECT gm.gangID FROM rolebot_gang_members gm
		JOIN rolebot_gangs g ON g.gangID = gm.gangID
		WHERE gm.characterID = ? AND g.enabled = 1
		ORDER BY gm.gangID LIMIT 1`, charID).Scan(&gangID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0 // do not cache a transient failure
	}
	gangOfCache.Store(charID, gangOfEntry{gangID: gangID, at: now})
	return gangID
}

type gangOfEntry struct {
	gangID int64
	at     int64
}

var gangOfCache sync.Map

// gangMemberChars is the member character IDs (any role), for can-ownership
// checks: a hauler empties cans owned by ANY member, player riders included.
func gangMemberChars(ctx context.Context, db *sql.DB, gangID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT characterID FROM rolebot_gang_members WHERE gangID = ?`, gangID)
	if err != nil {
		return nil, fmt.Errorf("gangMemberChars: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func init() { registerCharForget("gangOfCache", func(id int64) { gangOfCache.Delete(id) }) }
