package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	evedb "gurimoon/db"
)

// Issuing and returning kits (corparmory.go).
//
// At formation a recruit with nothing in its seat's band may be lent a kit: the recruiter's
// seat test (recruitSeatOK) falls back to the shelf and reserves a matching kit the pilot can
// fly, and once the gang row exists the reserved kits are issued. The kit moves to the station
// hangar, still the corporation's, and gangSeatReady boards it. When the pilot leaves that
// fleet it flies back, re-boards its own ship and the kit goes back on the shelf, with the
// pilot's own cargo. A kit lost in the fight is a shelf shortfall, not an SRP claim.

const (
	issueIssued  = "issued"
	issueLost    = "lost"
	issueStolen  = "stolen"
	issueWritten = "written"

	kitBotDueSec     = 6 * 3600 // a bot's kit is due back this long after the fleet's end
	issuePruneSec    = 30 * 86400
	kitReturnLogEach = 1800
)

// TaskKitReturn is a pilot flying a lent kit back to its shelf.
const TaskKitReturn TaskKind = "kit.return"

func init() {
	registerTask(TaskDef{
		Kind: TaskKitReturn, Label: "Returning a corporation ship",
		Posture: PostureDocked,
		Step:    (*Mod).kitReturnStep,
	})
}

// corpIssue is one rolebot_corp_issue row.
type corpIssue struct {
	IssueID, CorpID, CharID int64
	Human                   bool
	KitID                   int64
	Kind                    string
	ShipItemID, HullTypeID  int64
	Value                   float64
	StationID, GangID       int64
	SeatRole                string
	OwnShipID               int64
	Status                  string
	IssuedAt, DueAt         int64
	WarnedAt, ClosedAt      int64
}

const issueCols = `issueID, corporationID, characterID, human, kitID, kind, shipItemID, hullTypeID, value,
	stationID, gangID, seatRole, ownShipID, status, issuedAt, dueAt, warnedAt, closedAt`

func scanIssue(sc interface{ Scan(...any) error }) (corpIssue, error) {
	var is corpIssue
	var human int
	err := sc.Scan(&is.IssueID, &is.CorpID, &is.CharID, &human, &is.KitID, &is.Kind, &is.ShipItemID,
		&is.HullTypeID, &is.Value, &is.StationID, &is.GangID, &is.SeatRole, &is.OwnShipID, &is.Status,
		&is.IssuedAt, &is.DueAt, &is.WarnedAt, &is.ClosedAt)
	is.Human = human != 0
	return is, err
}

// openIssueFor is the kit a pilot holds, nil for none. A pilot holds at most one.
func (m *Mod) openIssueFor(ctx context.Context, charID int64) *corpIssue {
	if charID == 0 {
		return nil
	}
	is, err := scanIssue(m.db.QueryRowContext(ctx, `SELECT `+issueCols+` FROM rolebot_corp_issue
		WHERE characterID = ? AND status = ? ORDER BY issueID DESC LIMIT 1`, charID, issueIssued))
	if err != nil {
		return nil
	}
	return &is
}

// ---- the formation's shelf ----

type kitPool struct {
	corpID int64
	mu     sync.Mutex
	kits   []corpKit       // ready kits
	taken  map[int64]int64 // kitID → charID
	roles  map[int64]string
}

type kitPoolKey struct{}

// withKitPool puts the corporation's ready kits on the formation's context. Only a bot-run
// corporation lends (managedCEO): a human chair has no quartermaster.
func (m *Mod) withKitPool(ctx context.Context, corpID int64) context.Context {
	if corpID == 0 || !divisionProfilesOn.Load() {
		return ctx
	}
	if _, managed := managedCEO(corpID); !managed {
		return ctx
	}
	kits, err := m.corpKits(ctx, corpID, "status = ? AND shipItemID <> 0", kitReady)
	if err != nil || len(kits) == 0 {
		return ctx
	}
	return context.WithValue(ctx, kitPoolKey{},
		&kitPool{corpID: corpID, kits: kits, taken: map[int64]int64{}, roles: map[int64]string{}})
}

func kitPoolOf(ctx context.Context) *kitPool {
	p, _ := ctx.Value(kitPoolKey{}).(*kitPool)
	return p
}

// kitKindFor is the shelf a seat borrows from: the Yard for a mining op's hauler seat, the
// Armory for everything else.
func kitKindFor(s seatSpec) string {
	if isHaulerRole(s.Role) && s.HullMin == ClassIndustrial && s.HullMax == ClassIndustrial {
		return kitYard
	}
	return kitArmory
}

// kitPoolFind is a free kit for the seat this pilot can fly; reserve takes it. Only the
// corporation's own bots borrow here: a human draws a kit through the portal
// (corparmory_human.go).
func (m *Mod) kitPoolFind(ctx context.Context, charID int64, role string, spec seatSpec, reserve bool) bool {
	pool := kitPoolOf(ctx)
	if pool == nil || !m.isSynthetic(charID) || m.charCorpID(ctx, charID) != pool.corpID {
		return false
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, ch := range pool.taken {
		if ch == charID {
			return true
		}
	}
	if m.openIssueFor(ctx, charID) != nil {
		return false // still holds one from an earlier op
	}
	kind := kitKindFor(spec)
	for _, k := range pool.kits {
		if _, used := pool.taken[k.KitID]; used || !kitMatches(k, kind, spec) {
			continue
		}
		if !m.canFlyKit(ctx, charID, k) {
			continue
		}
		if reserve {
			pool.taken[k.KitID] = charID
			pool.roles[k.KitID] = role
		}
		return true
	}
	return false
}

// canFlyKit is whether a pilot has the skills for the kit's hull and every module on it.
func (m *Mod) canFlyKit(ctx context.Context, charID int64, k corpKit) bool {
	sheets, err := m.skillSheetsFor(ctx, []int64{charID})
	if err != nil {
		return false
	}
	sk := sheets[charID]
	if ok, _ := m.pilotCanUse(ctx, k.HullTypeID, sk); !ok {
		return false
	}
	if k.Fit != nil {
		for _, mod := range k.Fit.Modules {
			if ok, _ := m.pilotCanUse(ctx, mod.TypeID, sk); !ok {
				return false
			}
		}
	}
	return true
}

// issueFleetKits hands every kit the formation reserved to the pilot it was reserved for,
// once the gang row exists. A pilot not in the gang gets nothing. It returns how many Yard kits
// went out.
func (m *Mod) issueFleetKits(ctx context.Context, gangID int64, expiresAt int64) (yard int) {
	pool := kitPoolOf(ctx)
	if pool == nil {
		return 0
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, k := range pool.kits {
		charID, ok := pool.taken[k.KitID]
		if !ok {
			continue
		}
		var inGang int
		_ = m.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM rolebot_gang_members WHERE gangID = ? AND characterID = ?`, gangID, charID).Scan(&inGang)
		if inGang == 0 {
			continue
		}
		if err := m.issueKit(ctx, k, charID, gangID, pool.roles[k.KitID], false, expiresAt+kitBotDueSec); err != nil {
			rlogf("rolebots: issue kit %d to %d: %v", k.KitID, charID, err)
			continue
		}
		if k.Kind == kitYard {
			yard++
		}
	}
	return yard
}

var errKitGone = errors.New("kit is no longer on the shelf")

// issueKit moves one kit off the shelf. A bot's hull goes to the station hangar and stays the
// corporation's; a human's is handed over outright, with contents. A disposable seat's kit
// (kitHandedOver) is handed over even to a bot, because maybeInsureHull (botinsure.go) insures
// only a hull the pilot owns. The loan is still recorded, so noteKitLost books the loss.
func (m *Mod) issueKit(ctx context.Context, k corpKit, charID, gangID int64, role string, human bool, dueAt int64) error {
	flag, ok := m.corpDivFlag(ctx, k.CorpID, kitPurpose(k.Kind))
	if !ok {
		return errKitGone
	}
	handOver := human || m.kitHandedOver(ctx, gangID, role)
	officeID, _, err := evedb.GetCorpOfficeAt(ctx, m.db, k.CorpID, k.StationID)
	if err != nil || officeID == 0 || officeID == k.StationID {
		return errKitGone
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE rolebot_corp_kit SET status = ? WHERE kitID = ? AND status = ?`, kitIssued, k.KitID, kitReady)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errKitGone
	}
	owner := k.CorpID
	if handOver {
		owner = charID
	}
	res, err = tx.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 4, ownerID = ?
		 WHERE itemID = ? AND ownerID = ? AND locationID = ? AND flag = ?`,
		k.StationID, owner, k.ShipItemID, k.CorpID, officeID, flag)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errKitGone
	}
	if handOver {
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET ownerID = ? WHERE locationID = ? AND ownerID = ?`, charID, k.ShipItemID, k.CorpID); err != nil {
			return err
		}
	}
	h := 0
	if human {
		h = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rolebot_corp_issue
			(corporationID, characterID, human, kitID, kind, shipItemID, hullTypeID, value, stationID,
			 gangID, seatRole, status, issuedAt, dueAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.CorpID, charID, h, k.KitID, k.Kind, k.ShipItemID, k.HullTypeID, k.Value, k.StationID,
		gangID, role, issueIssued, clock.Unix(), dueAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if !human {
		loanKit(k.ShipItemID, charID)
	}
	m.logBot(charID, logInfo, "gang", "drew a %s from the corporation's %s at %s for the op",
		m.typeNameOr(ctx, k.HullTypeID), kitLabel(k.Kind), m.stationLabel(ctx, k.StationID))
	return nil
}

// kitHandedOver reports whether a bot's kit for this seat is handed over rather than lent: a
// gun seat of a gank fleet, whose hull the op spends (seatSpec.Disposable). It reads the gang's
// doctrine kind, since the member row has no Disposable. A missing gang is a loan.
func (m *Mod) kitHandedOver(ctx context.Context, gangID int64, role string) bool {
	if gangID == 0 {
		return false
	}
	g, err := getGang(ctx, m.db, gangID)
	if err != nil || g == nil {
		return false
	}
	return disposableSeat(g.DoctrineKind, role)
}

// disposableSeat is the pure rule behind kitHandedOver and the fit ranking: the gun seats of a
// disposable kind. The scout keeps its hull.
func disposableSeat(kind doctrineKind, role string) bool {
	k, ok := kindBySlug(kind)
	if !ok || !k.Disposable {
		return false
	}
	switch role {
	case "", RoleCombat, RoleHunter:
		return true
	}
	return false
}

// boardIssuedKit is gangSeatReady's answer for a pilot holding a kit for this fleet: board it,
// or fly to where it waits.
func (m *Mod) boardIssuedKit(ctx context.Context, b *botRun, is *corpIssue, activity string) (bool, error) {
	if b.ShipID == is.ShipItemID {
		if k := m.kitByID(ctx, is.KitID); k != nil && k.Fit != nil {
			if _, err := m.reloadFromCargo(ctx, b, is.ShipItemID, k.Fit); err != nil {
				return false, err
			}
		}
		return true, nil
	}
	var loc int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`, is.ShipItemID).Scan(&loc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			m.closeIssue(ctx, is, issueWritten, "the kit vanished before it was boarded")
			return false, nil
		}
		return false, err
	}
	if b.StationID != 0 && loc == b.StationID {
		own := b.ShipID
		if own != 0 && m.isCapsule(ctx, own) {
			own = 0
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE rolebot_corp_issue SET ownShipID = ? WHERE issueID = ?`, own, is.IssueID); err != nil {
			return false, err
		}
		return true, m.boardShip(ctx, b, is.ShipItemID)
	}
	st, err := evedb.GetStationLocation(ctx, m.db, loc)
	if err != nil {
		return false, fmt.Errorf("kit %d is not in a reachable station: %w", is.ShipItemID, err)
	}
	m.logBot(b.CharID, logInfo, "gang", "collecting the corporation's %s from %s for the op",
		m.typeNameOr(ctx, is.HullTypeID), m.stationLabel(ctx, loc))
	b.State.Activity = activity
	return false, m.beginTravel(ctx, b, st.SolarSystemID, loc, string(TaskBoard))
}

func (m *Mod) kitByID(ctx context.Context, kitID int64) *corpKit {
	k, err := scanKit(m.db.QueryRowContext(ctx, `SELECT `+kitCols+` FROM rolebot_corp_kit WHERE kitID = ?`, kitID))
	if err != nil {
		return nil
	}
	return &k
}

// ---- coming home ----

// armoryReturnResume is planSolo's seam: a bot holding a kit for a fleet it is no longer in
// takes it back before anything else.
func (m *Mod) armoryReturnResume(ctx context.Context, b *botRun) (bool, error) {
	if !holdsLoanedKit(b.CharID) {
		return false, nil
	}
	is := m.openIssueFor(ctx, b.CharID)
	if is == nil || is.Human {
		return false, nil
	}
	if g, _, err := gangForChar(ctx, m.db, b.CharID); err == nil && g != nil && g.GangID == is.GangID {
		return false, nil // still flying the op
	}
	return true, m.kitReturnStep0(ctx, b, is)
}

// kitReturnStep is the task's step: the pilot arrived or is on its way.
func (m *Mod) kitReturnStep(ctx context.Context, b *botRun) (bool, error) {
	is := m.openIssueFor(ctx, b.CharID)
	if is == nil {
		st := b.State
		st.Phase, st.Data = string(TaskIdle), idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	return false, m.kitReturnStep0(ctx, b, is)
}

func (m *Mod) kitReturnStep0(ctx context.Context, b *botRun, is *corpIssue) error {
	// The kit is gone (sold out from under it, destroyed without a death record): nothing to
	// bring back.
	var loc, owner int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID, ownerID FROM entity WHERE itemID = ?`, is.ShipItemID).Scan(&loc, &owner); err != nil {
		m.closeIssue(ctx, is, issueWritten, "the kit could not be found to return")
		return nil
	}
	if b.StationID != is.StationID {
		st, err := evedb.GetStationLocation(ctx, m.db, is.StationID)
		if err != nil {
			m.closeIssue(ctx, is, issueWritten, "the kit's station is unreachable")
			return nil
		}
		m.logBot(b.CharID, logInfo, "gang", "taking the corporation's %s back to %s",
			m.typeNameOr(ctx, is.HullTypeID), m.stationLabel(ctx, is.StationID))
		return m.beginTravel(ctx, b, st.SolarSystemID, is.StationID, string(TaskKitReturn))
	}
	if err := m.returnKit(ctx, b, is); err != nil {
		return err
	}
	st := b.State
	st.Activity, st.Phase, st.Data = "", string(TaskIdle), idleData(st.Data)
	return saveState(ctx, m.db, st)
}

// returnKit puts the kit back on the shelf at this station: the pilot steps out (into its own
// ship if here, else its capsule), its own cargo goes with it, and the hull with its fittings
// and the corporation's rounds goes back into the Armory division.
func (m *Mod) returnKit(ctx context.Context, b *botRun, is *corpIssue) error {
	flag, ok := m.corpDivFlag(ctx, is.CorpID, kitPurpose(is.Kind))
	officeID, _, err := evedb.GetCorpOfficeAt(ctx, m.db, is.CorpID, is.StationID)
	if !ok || err != nil || officeID == 0 || officeID == is.StationID {
		// No shelf to return to (the corporation lost the office or division): the hull stays parked
		// as the corporation's, the pilot steps out and the loan is written off.
		ok = false
	}
	if b.ShipID == is.ShipItemID {
		var ownHere int64
		if is.OwnShipID != 0 {
			_ = m.db.QueryRowContext(ctx, `SELECT itemID FROM entity WHERE itemID = ? AND ownerID = ? AND locationID = ? AND flag = 4`,
				is.OwnShipID, b.CharID, b.StationID).Scan(&ownHere)
		}
		if ownHere != 0 {
			if err := m.boardShip(ctx, b, ownHere); err != nil {
				return err
			}
		} else if err := m.ejectToCapsule(ctx, b); err != nil {
			return err
		}
		// The pilot's own cargo leaves the kit: into its own ship's hold, else its hangar here.
		dest, destFlag := b.StationID, int64(4)
		if ownHere != 0 {
			dest, destFlag = ownHere, 5
		}
		if _, err := m.db.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = ? WHERE locationID = ? AND ownerID = ? AND flag = 5`,
			dest, destFlag, is.ShipItemID, b.CharID); err != nil {
			return err
		}
	}
	unloanKit(is.ShipItemID)
	if !ok {
		m.closeIssue(ctx, is, issueWritten, "no shelf to return the kit to")
		_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_kit WHERE kitID = ?`, is.KitID)
		return nil
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = ?, ownerID = ? WHERE itemID = ?`,
		officeID, flag, is.CorpID, is.ShipItemID); err != nil {
		return err
	}
	// Fittings handed to a human, or to a disposable seat's bot (issueKit), come back with the
	// hull. Unconditional: a lent kit's lines are already the corporation's.
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET ownerID = ? WHERE locationID = ? AND flag <> 5`, is.CorpID, is.ShipItemID); err != nil {
		return err
	}
	// Back to planned: the chore re-fits what the op used up from the shelf, then it is ready.
	if _, err := m.db.ExecContext(ctx,
		`UPDATE rolebot_corp_kit SET status = ?, stationID = ? WHERE kitID = ?`, kitPlanned, is.StationID, is.KitID); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_issue WHERE issueID = ?`, is.IssueID); err != nil {
		return err
	}
	m.logBot(b.CharID, logInfo, "gang", "returned the corporation's %s to its %s at %s",
		m.typeNameOr(ctx, is.HullTypeID), kitLabel(is.Kind), m.stationLabel(ctx, is.StationID))
	return nil
}

// ejectToCapsule puts a docked pilot in a fresh capsule at its station.
func (m *Mod) ejectToCapsule(ctx context.Context, b *botRun) error {
	pod, err := evedb.CreateCapsuleEntity(ctx, m.db, b.CharID, b.StationID, b.Name)
	if err != nil {
		return err
	}
	_, err = m.db.ExecContext(ctx, `UPDATE entity SET locationID = ? WHERE itemID = ?`, pod, b.CharID)
	if err == nil {
		b.ShipID = pod
	}
	return err
}

// closeIssue ends a loan in a terminal state.
func (m *Mod) closeIssue(ctx context.Context, is *corpIssue, status, note string) {
	unloanKit(is.ShipItemID)
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_issue SET status = ?, closedAt = ?, note = ? WHERE issueID = ? AND status = ?`,
		status, clock.Unix(), truncate(note, 255), is.IssueID, issueIssued)
	if status != issueIssued {
		_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_kit WHERE kitID = ?`, is.KitID)
	}
}

// noteKitLost is loseHull's seam: a lent kit destroyed is a shelf shortfall, not a member's
// loss.
func (m *Mod) noteKitLost(ctx context.Context, shipID int64) {
	if shipID == 0 || loanedTo(shipID) == 0 {
		return
	}
	is, err := scanIssue(m.db.QueryRowContext(ctx, `SELECT `+issueCols+` FROM rolebot_corp_issue
		WHERE shipItemID = ? AND status = ? LIMIT 1`, shipID, issueIssued))
	if err != nil {
		return
	}
	m.closeIssue(ctx, &is, issueLost, "destroyed in the field")
	if ceoID, ok := managedCEO(is.CorpID); ok {
		m.ceoLog(ctx, &corpCEO{CorpID: is.CorpID, CharID: ceoID}, powerProduction, "kit.lost", is.HullTypeID, is.Value,
			"lost an %s %s flown by %s", kitLabel(is.Kind), m.typeNameOr(ctx, is.HullTypeID), m.charNameOr(ctx, is.CharID))
	}
}

// pruneIssues drops terminal loans older than issuePruneSec.
func (m *Mod) pruneIssues(ctx context.Context) {
	_, _ = m.db.ExecContext(ctx, `DELETE FROM rolebot_corp_issue WHERE status <> ? AND closedAt > 0 AND closedAt < ?`,
		issueIssued, clock.Unix()-issuePruneSec)
}
