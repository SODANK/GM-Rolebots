package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"

	evedb "gurimoon/db"
)

// The haul board: the legwork between towers, offices and hubs. A leg is "carry these to
// there": from an office division or a structure's bay to a tower's bay, an office division
// or (for a deploy or rig leg) a moon where the cargo is anchored on arrival. It generalises
// the factory move (corpfactorymove.go). Posters: the Fueler (fuel legs), the CEO's starbase
// power (deploy and rig legs), the Trader (backhaul), the chain routing (posindustry.go).
// Any member claims through posHaulDue in plan(). For haulReserveSec after posting, a fuel
// leg is the Fueler's alone. A deploy or rig leg ends in anchoring, the officers' job
// (posanchor.go). Wages are committed when the leg is posted and paid on delivery.
// In the carrier's hold the cargo stays the corporation's (owner = corp), so no sweep over
// the pilot's own goods can sell, stack or dump it.

const (
	haulKindFuel     = "fuel"
	haulKindDeploy   = "deploy"
	haulKindRig      = "rig"
	haulKindBackhaul = "backhaul"
	haulKindGoo      = "goo"
	haulKindAmmo     = "ammo"
	haulKindScoop    = "scoop"
	haulKindFeed     = "feed" // print and materials out to a tower array (posjobs.go)

	haulFromOffice    = "office"
	haulFromStructure = "struct"
	haulFromSpace     = "space" // free structures to scoop (a teardown)
	haulToOffice      = "office"
	haulToTower       = "tower"

	haulISKPerM3Jump = 25.0
	haulMinWage      = 100000
	haulClaimSec     = 6 * 3600
	haulReserveSec   = 3600
	haulStaleSec     = 7 * 86400
	haulMaxTrips     = 12
	towerWorkSec     = 45
	haulMinRoom      = 1.0
)

// haulTooBig remembers a pilot whose hold could not take a leg's cargo, so it is not handed
// the same leg every pass. Keys are "charID|legID", values unix time.
var haulTooBig sync.Map

// haulItem is one line of what a leg is to carry, and the source division it
// is taken from (-1 = the structure's whole bay).
type haulItem struct {
	TypeID int64 `json:"t"`
	Qty    int64 `json:"q"`
	Flag   int64 `json:"f"`
	// ItemID names one free structure to scoop (haulFromSpace), or the copy
	// to prefer on an office leg with CopyOnly.
	ItemID int64 `json:"i,omitempty"`
	// CopyOnly: the line is a blueprint copy of TypeID, taken one at a time by
	// takeCopyFromOffice (the tower feed's print).
	CopyOnly bool `json:"c,omitempty"`
	// Original: the line is ONE blueprint original, ItemID, out to or home
	// from a tower laboratory for high-priority research (postowerlab.go).
	// Only the corporation's Directors and CEO carry one.
	Original bool `json:"o,omitempty"`
}

// haulCarry is what a carrier has loaded, by type.
type haulCarry struct {
	TypeID int64 `json:"t"`
	Qty    int64 `json:"q"`
}

// haulLeg is one rolebot_corp_haul row.
type haulLeg struct {
	LegID, CorpID         int64
	Kind                  string
	PlanID                int64
	FromKind              string
	FromID, FromStationID int64
	FromSystemID          int64
	FromFlag              int64
	ToKind                string
	ToID, ToStationID     int64
	ToSystemID, ToFlag    int64
	Items                 []haulItem
	M3, Wage              float64
	Deadline              int64
	CarrierID, ClaimedAt  int64
	Status                string
	Carrying              []haulCarry
	PostedBy              string
	PostedAt              int64
	Trips                 int
	Note                  string
	// SwappedOut is what the carrier took out of a full fuel bay this trip (makeBayRoom). It is
	// not stored: haulSettle sends it home.
	SwappedOut int64
	// AtPlanID is the plan of the tower the carrier just delivered to (haulDeliver). It is not
	// stored: haulSettle claims that tower's backhaul home with it (chainBackhaul).
	AtPlanID int64
}

func (l haulLeg) remaining() int64 {
	n := int64(0)
	for _, it := range l.Items {
		n += it.Qty
	}
	return n
}

func (l haulLeg) carryingAny() bool {
	for _, c := range l.Carrying {
		if c.Qty > 0 {
			return true
		}
	}
	return false
}

const haulCols = `legID, corporationID, kind, planID, fromKind, fromID, fromStationID, fromSystemID, fromFlag,
	toKind, toID, toStationID, toSystemID, toFlag, COALESCE(items, ''), m3, wage, deadline, carrierID, claimedAt,
	status, COALESCE(carrying, ''), postedBy, postedAt, trips, note`

func scanHaul(sc interface{ Scan(...any) error }) (*haulLeg, error) {
	var l haulLeg
	var items, carrying string
	if err := sc.Scan(&l.LegID, &l.CorpID, &l.Kind, &l.PlanID, &l.FromKind, &l.FromID, &l.FromStationID,
		&l.FromSystemID, &l.FromFlag, &l.ToKind, &l.ToID, &l.ToStationID, &l.ToSystemID, &l.ToFlag,
		&items, &l.M3, &l.Wage, &l.Deadline, &l.CarrierID, &l.ClaimedAt, &l.Status, &carrying,
		&l.PostedBy, &l.PostedAt, &l.Trips, &l.Note); err != nil {
		return nil, err
	}
	if items != "" {
		_ = json.Unmarshal([]byte(items), &l.Items)
	}
	if carrying != "" {
		_ = json.Unmarshal([]byte(carrying), &l.Carrying)
	}
	return &l, nil
}

func (m *Mod) loadHaul(ctx context.Context, legID int64) (*haulLeg, error) {
	l, err := scanHaul(m.db.QueryRowContext(ctx, `SELECT `+haulCols+` FROM rolebot_corp_haul WHERE legID = ?`, legID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return l, err
}

func (m *Mod) queryHauls(ctx context.Context, where string, args ...any) ([]*haulLeg, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+haulCols+` FROM rolebot_corp_haul `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("queryHauls: %w", err)
	}
	defer rows.Close()
	var out []*haulLeg
	for rows.Next() {
		l, err := scanHaul(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (m *Mod) saveHaulProgress(ctx context.Context, l *haulLeg) error {
	items, _ := json.Marshal(l.Items)
	var carrying any
	if l.carryingAny() {
		raw, _ := json.Marshal(l.Carrying)
		carrying = string(raw)
	}
	_, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_haul SET items = ?, carrying = ?, status = ?, carrierID = ?, claimedAt = ?, trips = ?
		 WHERE legID = ?`, string(items), carrying, l.Status, l.CarrierID, l.ClaimedAt, l.Trips, l.LegID)
	return err
}

// haulWageFor is a leg's pay: m3 times jumps at the board rate, floored. Pure.
func haulWageFor(m3 float64, jumps int) float64 {
	return math.Max(haulMinWage, math.Ceil(m3*float64(max(jumps, 1))*haulISKPerM3Jump))
}

// postHaul puts a leg on the board and commits its wage.
func (m *Mod) postHaul(ctx context.Context, c *corpCEO, l haulLeg) (int64, error) {
	if len(l.Items) == 0 {
		return 0, nil
	}
	m3 := 0.0
	for _, it := range l.Items {
		m3 += m.typeVolume(ctx, it.TypeID) * float64(it.Qty)
	}
	l.M3 = m3
	if l.FromSystemID == 0 && l.FromStationID != 0 {
		l.FromSystemID = m.stationSystemID(ctx, l.FromStationID)
	}
	if l.ToSystemID == 0 && l.ToStationID != 0 {
		l.ToSystemID = m.stationSystemID(ctx, l.ToStationID)
	}
	jumps := 1
	if u, err := m.universe(ctx); err == nil && u != nil && l.FromSystemID != 0 && l.ToSystemID != 0 {
		if j, ok := bfsJumps(u, l.FromSystemID, 40)[l.ToSystemID]; ok {
			jumps = max(j, 1)
		}
	}
	if l.Wage == 0 {
		l.Wage = haulWageFor(m3, jumps)
	}
	if l.Deadline == 0 {
		l.Deadline = clock.Unix() + 3*86400
	}
	items, _ := json.Marshal(l.Items)
	res, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_haul
			(corporationID, kind, planID, fromKind, fromID, fromStationID, fromSystemID, fromFlag,
			 toKind, toID, toStationID, toSystemID, toFlag, items, m3, wage, deadline, postedBy, postedAt, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.CorpID, l.Kind, l.PlanID, l.FromKind, l.FromID, l.FromStationID, l.FromSystemID, l.FromFlag,
		l.ToKind, l.ToID, l.ToStationID, l.ToSystemID, l.ToFlag, string(items), m3, l.Wage, l.Deadline,
		l.PostedBy, clock.Unix(), truncate(l.Note, 255))
	if err != nil {
		return 0, fmt.Errorf("postHaul: %w", err)
	}
	id, _ := res.LastInsertId()
	_ = m.openCommit(ctx, c.CorpID, "haul", id, l.Wage)
	m.ceoLog(ctx, c, powerStarbase, "haul", 0, l.Wage,
		"posted a %s leg: %.0f m³ to %s (pays %s)", l.Kind, m3, m.haulDestLabel(ctx, &l), iskString(l.Wage))
	markPosWork(c.CorpID)
	return id, nil
}

// openHaulFor is whether a plan already has an open leg of a kind.
func (m *Mod) openHaulFor(ctx context.Context, planID int64, kind string) *haulLeg {
	legs, err := m.queryHauls(ctx, `WHERE planID = ? AND kind = ? AND status IN ('open', 'claimed') LIMIT 1`, planID, kind)
	if err != nil || len(legs) == 0 {
		return nil
	}
	return legs[0]
}

func (m *Mod) haulDestLabel(ctx context.Context, l *haulLeg) string {
	switch l.ToKind {
	case haulToTower:
		if l.ToID != 0 {
			return "a tower in " + systemNameOf(ctx, m, l.ToSystemID)
		}
		return "a moon in " + systemNameOf(ctx, m, l.ToSystemID)
	}
	return m.stationLabel(ctx, l.ToStationID)
}

// closeHaul ends a leg and settles its wage commitment.
func (m *Mod) closeHaul(ctx context.Context, l *haulLeg, status, note string) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_haul SET status = ?, closedAt = ?, carrying = NULL, note = ? WHERE legID = ?`,
		status, clock.Unix(), truncate(note, 255), l.LegID)
	m.closeCommit(ctx, "haul", l.LegID)
	l.Status = status
}

// ── claiming ────────────────────────────────────────────────────────────────

// posHaulDue is the planner's pre-emption: a carrier mid-leg finishes it; a
// docked member with nothing in hand takes the next leg it can reach.
func (m *Mod) posHaulDue(ctx context.Context, b *botRun) (bool, error) {
	// Already carrying one.
	legs, err := m.queryHauls(ctx, `WHERE carrierID = ? AND status = 'claimed' LIMIT 1`, b.CharID)
	if err != nil {
		return false, err
	}
	if len(legs) > 0 {
		st := b.State
		st.Activity = ActIndustry
		st.Data = idleData(st.Data)
		st.Data.PosLegID = legs[0].LegID
		st.Phase = string(TaskPosHaul)
		return true, saveState(ctx, m.db, st)
	}
	if b.StationID == 0 {
		return false, nil
	}
	now := clock.Unix()
	isFueler := m.posSeatOf(ctx, b.CorpID, b.CharID) == seatFueler
	isInfra := false
	if s, ok := armOf(b.CharID, b.CorpID); ok && s.Arm == armInfrastructure {
		isInfra = true
	}
	hasInfra := armCount(b.CorpID, armInfrastructure) > 0
	open, err := m.queryHauls(ctx, `
		WHERE corporationID = ? AND (status = 'open' OR (status = 'claimed' AND claimedAt < ? AND COALESCE(carrying, '') = ''))
		 ORDER BY deadline, legID LIMIT 12`, b.CorpID, now-haulClaimSec)
	if err != nil || len(open) == 0 {
		return false, err
	}
	hold := -1.0 // this pilot's hauler hold, read on the first leg that needs it
	cargoHull := -1 // 1 when its trade hull is a cargo hull, read on the first fuel leg
	for _, l := range open {
		if at, ok := haulTooBig.Load(fmt.Sprintf("%d|%d", b.CharID, l.LegID)); ok && now-at.(int64) < 86400 {
			continue
		}
		// Fuel waits for the Fueler; a leg that ends in anchoring waits for
		// the corporation's officers (posanchor.go).
		urgent := l.Deadline > 0 && l.Deadline-now <= haulUrgentSec
		if !urgent && !haulClaimOK(l.Kind, isFueler, isInfra, hasInfra, now-l.PostedAt) {
			continue
		}
		if !m.mayClaimAnchorLeg(ctx, b, l.Kind, now-l.PostedAt) {
			continue
		}
		if legCarriesOriginal(l) && !m.isCorpOfficer(ctx, b.CorpID, b.CharID) {
			continue // a blueprint original moves only in a Director's hold (postowerlab.go)
		}
		fromSys := l.FromSystemID
		if j := m.jumpsTo(ctx, b, fromSys); j < 0 || j > haulClaimMaxJumps(l.Kind, now-l.PostedAt) {
			continue
		}
		// A fuel leg is a tower's bay to fill, thousands of m³: only a pilot whose trade hull is a
		// cargo hull (reqHaulGroups) takes it. A frigate's several trips a leg are the delay.
		if l.Kind == haulKindFuel && l.M3 >= reqCargoHullLoadM3 {
			if cargoHull < 0 {
				cargoHull = 0
				if m.reqTradeHullIsCargo(ctx, b.CharID) {
					cargoHull = 1
				}
			}
			if cargoHull == 0 {
				continue
			}
		}
		// The hold, before the flight: a hull that cannot take the leg's
		// biggest single unit would fly to the source only to put it back.
		if need := m.haulBiggestUnit(ctx, l); need > 0 {
			if hold < 0 {
				hold = m.reqHaulerHold(ctx, b.CharID)
			}
			if !reqHoldFits(need, hold) {
				haulTooBig.Store(fmt.Sprintf("%d|%d", b.CharID, l.LegID), now)
				continue
			}
		}
		won, err := m.claimHaulLeg(ctx, l.LegID, b.CharID, now)
		if err != nil {
			return false, err
		}
		if !won {
			continue
		}
		st := b.State
		st.Activity = ActIndustry
		st.Data = idleData(st.Data)
		st.Data.PosLegID = l.LegID
		m.logBot(b.CharID, logInfo, "corp", "took its corporation's %s leg to %s (%.0f m³, pays %s)",
			l.Kind, m.haulDestLabel(ctx, l), l.M3, iskString(l.Wage))
		if l.FromKind == haulFromOffice {
			return true, m.beginTravel(ctx, b, l.FromSystemID, l.FromStationID, string(TaskPosHaul))
		}
		return true, m.beginTravel(ctx, b, l.FromSystemID, 0, string(TaskPosHaul))
	}
	return false, nil
}

// claimHaulLeg is the board's claim lock: one UPDATE that takes a leg only while it is open, or
// claimed so long ago (haulClaimSec) with nothing aboard that its carrier has given up. False when
// another carrier holds it, so two carriers never take the same leg.
func (m *Mod) claimHaulLeg(ctx context.Context, legID, charID, now int64) (bool, error) {
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_haul SET status = 'claimed', carrierID = ?, claimedAt = ?
		 WHERE legID = ? AND (status = 'open' OR (status = 'claimed' AND claimedAt < ? AND COALESCE(carrying, '') = ''))`,
		charID, now, legID, now-haulClaimSec)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// haulClaimOK is the board's queue: fuel is the Fueler's for the first haulReserveSec, then
// Infrastructure's for another; ammunition is Infrastructure's for the first; anybody's
// after. A corporation with no Infrastructure arm (hasInfra) keeps a one-hour Fueler
// reserve. A leg due inside haulUrgentSec skips the queue (posHaulDue). Pure.
func haulClaimOK(kind string, fueler, infra, hasInfra bool, age int64) bool {
	switch kind {
	case haulKindFuel:
		switch {
		case fueler || age >= 2*haulReserveSec:
			return true
		case age >= haulReserveSec:
			return infra || !hasInfra
		}
		return false
	case haulKindAmmo:
		return fueler || infra || !hasInfra || age >= haulReserveSec
	}
	return true
}

// haulClaimMaxJumps is how far from a leg's source a docked member may be to claim it:
// corpWorkMaxJumps, except for a leg that ends in anchoring (deploy, rig) once its officer
// reserve (anchorReserveSec) has run. Those are staged at the office nearest the moon, often a
// forward office in 0.0 where nobody docks; held to the usual radius they would sit until they
// went stale (haulStaleSec), so after the reserve anyone allowed to claim them may fly out. Pure.
func haulClaimMaxJumps(kind string, age int64) int {
	if anchorNeedFor(kind) > 0 && age >= anchorReserveSec {
		return unlimitedJumps
	}
	return corpWorkMaxJumps
}

// haulBiggestUnit is the m3 of the largest single unit a leg still has to carry; for a deploy
// leg that is the tower, which must go aboard first (haulLoad). 0 when nothing is left or no
// volume is known.
func (m *Mod) haulBiggestUnit(ctx context.Context, l *haulLeg) float64 {
	return haulBiggestOf(l.Kind, l.Items, func(t int64) float64 { return m.typeVolume(ctx, t) })
}

// haulBiggestOf is haulBiggestUnit over a volume lookup. Pure.
func haulBiggestOf(kind string, items []haulItem, vol func(int64) float64) float64 {
	best := 0.0
	for i, it := range items {
		if it.Qty <= 0 {
			continue
		}
		v := vol(it.TypeID)
		if kind == haulKindDeploy && i == 0 {
			return v
		}
		best = math.Max(best, v)
	}
	return best
}

// releaseHaul hands an empty-handed claim back to the board.
func (m *Mod) releaseHaul(ctx context.Context, l *haulLeg) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_haul SET status = 'open', carrierID = 0, claimedAt = 0
		 WHERE legID = ? AND COALESCE(carrying, '') = ''`, l.LegID)
}

// ── the errand ──────────────────────────────────────────────────────────────

// posHaulStep is TaskPosHaul: load at the source, fly, deliver, get paid.
func (m *Mod) posHaulStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	finish := func(note string) (bool, error) {
		if note != "" {
			m.logBot(b.CharID, logInfo, "corp", "%s", note)
		}
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	l, err := m.loadHaul(ctx, d.PosLegID)
	if err != nil {
		return false, err
	}
	if l == nil || l.Status != "claimed" || l.CarrierID != b.CharID {
		return finish("its corporation's haul leg was closed without it")
	}
	if !l.carryingAny() {
		return m.haulLoad(ctx, b, l, finish)
	}
	return m.haulDeliver(ctx, b, l, finish)
}

// haulLoad is the loading half.
func (m *Mod) haulLoad(ctx context.Context, b *botRun, l *haulLeg, finish func(string) (bool, error)) (bool, error) {
	if l.remaining() <= 0 {
		m.closeHaul(ctx, l, "done", "nothing left to carry")
		return finish("its corporation's leg had nothing left to carry")
	}
	switch l.FromKind {
	case haulFromOffice:
		if b.StationID != l.FromStationID {
			return false, m.beginTravel(ctx, b, l.FromSystemID, l.FromStationID, string(TaskPosHaul))
		}
	case haulFromStructure, haulFromSpace:
		if b.SystemID != l.FromSystemID || b.StationID != 0 {
			return false, m.beginTravel(ctx, b, l.FromSystemID, 0, string(TaskPosHaul))
		}
		if pl, _ := m.loadTowerPlan(ctx, l.PlanID); pl != nil && m.embodyTowerPrep(b.CharID, pl.MoonID) {
			b.State.Data.LegEndsAt = clock.Unix() + navLegHoldSec
			return false, saveState(ctx, m.db, b.State)
		}
	}
	room := m.shipHoldRoom(ctx, b)
	if b.StationID != 0 {
		room = m.boardHauler(ctx, b, "its corporation's "+l.Kind+" leg")
	}
	// A fuel leg rides in a cargo hull: a carrier that is still in a frigate, its industrial parked
	// elsewhere, gives the leg back before it loads a few percent of it.
	if l.Kind == haulKindFuel && l.M3 >= reqCargoHullLoadM3 && b.StationID != 0 && !m.shipItemIsCargoHull(ctx, b.ShipID) {
		haulTooBig.Store(fmt.Sprintf("%d|%d", b.CharID, l.LegID), clock.Unix())
		m.releaseHaul(ctx, l)
		return finish("is not in an industrial hull for its corporation's fuel leg")
	}
	if room < haulMinRoom {
		m.releaseHaul(ctx, l)
		return finish("had no hold to carry its corporation's cargo in")
	}
	office := int64(0)
	if l.FromKind == haulFromOffice {
		office = m.officeAt(ctx, b.CorpID, l.FromStationID)
		if office == 0 {
			m.closeHaul(ctx, l, "void", "the source office is gone")
			return finish("its corporation no longer has the office the cargo was in")
		}
	}
	loaded, tooBig := 0, false
	for i := range l.Items {
		it := &l.Items[i]
		if it.Qty <= 0 {
			continue
		}
		vol := math.Max(m.typeVolume(ctx, it.TypeID), 0.01)
		qty := min(it.Qty, int64(room/vol))
		if qty <= 0 {
			tooBig = true
			// A deploy leg is the tower first: without it aboard the rest
			// is a fuel run to a moon with nothing on it.
			if l.Kind == haulKindDeploy && i == 0 {
				break
			}
			continue
		}
		var got int64
		var err error
		switch l.FromKind {
		case haulFromOffice:
			if it.Original {
				got, err = m.takeOriginalFromOffice(ctx, b.CorpID, office, it.ItemID, b.ShipID)
				break
			}
			if it.CopyOnly {
				var id int64
				if id, err = m.takeCopyFromOffice(ctx, b.CorpID, office, it.TypeID, it.ItemID, b.ShipID, 5, b.CorpID); id != 0 {
					got = 1
				}
				break
			}
			got, err = m.takeFromOffice(ctx, b.CorpID, office, it.Flag, it.TypeID, qty, b.ShipID, 5, b.CorpID)
		case haulFromSpace:
			if it.ItemID != 0 {
				if err = evedb.ScoopStructureFor(ctx, m.db, it.ItemID, b.ShipID, nil); err == nil {
					got = 1
				} else {
					m.logBot(b.CharID, logInfo, "corp", "could not scoop a %s: %v", m.typeNameOr(ctx, it.TypeID), err)
					err = nil
				}
			}
		default:
			if it.ItemID != 0 {
				// One named item out of a structure: a print, original or copy, which shares its type with
				// others in the same bay.
				got, err = m.takeItemFromStructure(ctx, b.CorpID, l.FromID, it.ItemID, b.ShipID)
				break
			}
			got, err = evedb.TakeFromStarbaseBay(ctx, m.db, l.FromID, it.TypeID, qty, it.Flag, b.ShipID, 5, b.CorpID)
			if err == nil && got > 0 && (l.Kind == haulKindBackhaul || l.Kind == haulKindGoo) {
				m.towerBookProduct(ctx, l.PlanID, it.TypeID, got)
			}
		}
		if err != nil {
			return false, err
		}
		if got <= 0 {
			it.Qty = 0 // nothing of it there any more: drop the line
			continue
		}
		it.Qty -= got
		room -= float64(got) * vol
		b.State.Data.PosTripM3 += float64(got) * vol
		l.Carrying = append(l.Carrying, haulCarry{TypeID: it.TypeID, Qty: got})
		loaded++
	}
	if loaded == 0 && tooBig {
		// The cargo is there; this hull is too small for it. Leave the leg
		// to a bigger hold and do not take it again for a while.
		haulTooBig.Store(fmt.Sprintf("%d|%d", b.CharID, l.LegID), clock.Unix())
		m.releaseHaul(ctx, l)
		return finish("its hold is too small for its corporation's " + l.Kind + " leg")
	}
	if loaded == 0 {
		if l.remaining() <= 0 || l.Trips > 0 {
			m.closeHaul(ctx, l, "done", "nothing left at the source")
			return finish("found nothing left to carry for its corporation")
		}
		m.closeHaul(ctx, l, "void", "the cargo was not at the source")
		return finish("found its corporation's cargo gone from the source")
	}
	if err := m.saveHaulProgress(ctx, l); err != nil {
		return false, err
	}
	if l.ToKind == haulToTower {
		return false, m.beginTravel(ctx, b, l.ToSystemID, 0, string(TaskPosHaul))
	}
	return false, m.beginTravel(ctx, b, l.ToSystemID, l.ToStationID, string(TaskPosHaul))
}

// haulDeliver is the delivering half.
func (m *Mod) haulDeliver(ctx context.Context, b *botRun, l *haulLeg, finish func(string) (bool, error)) (bool, error) {
	st := b.State
	switch l.ToKind {
	case haulToTower:
		if b.SystemID != l.ToSystemID || b.StationID != 0 {
			return false, m.beginTravel(ctx, b, l.ToSystemID, 0, string(TaskPosHaul))
		}
		// An embodied carrier first flies to the destination's moon for real. A chain leg is booked on
		// its feeder (l.PlanID, towerBookProduct) but delivers to the hub.
		pl, _ := m.loadTowerPlan(ctx, l.PlanID)
		if rec, ok, _ := evedb.GetStarbaseStructure(ctx, m.db, l.ToID); ok {
			towerID := rec.ControlTowerID
			if towerID == 0 {
				towerID = rec.EntityID
			}
			if dp, _ := m.towerPlanByTower(ctx, towerID); dp != nil {
				pl = dp
			}
		}
		if pl != nil && m.embodyTowerPrep(b.CharID, pl.MoonID) {
			st.Data.LegEndsAt = clock.Unix() + navLegHoldSec
			return false, saveState(ctx, m.db, st)
		}
		// On grid: a short working leg at the tower, so a watcher sees the
		// ship there before the cargo moves.
		if st.Data.PosAtTowerUntil == 0 {
			m.parkAtTower(ctx, b, l)
			st.Data.PosAtTowerUntil = clock.Unix() + towerWorkSec
			st.Data.LegEndsAt = st.Data.PosAtTowerUntil
			return false, saveState(ctx, m.db, st)
		}
		st.Data.PosAtTowerUntil = 0
		if pl != nil {
			l.AtPlanID = pl.PlanID
		}
		note, err := m.deliverToTower(ctx, b, l)
		if err != nil {
			return false, err
		}
		return m.haulSettle(ctx, b, l, note, finish)
	case haulToYard:
		// Another corporation's office: a consortium program's components
		// change hands at the yard, which pays for them (corpconsortium.go).
		if b.StationID != l.ToStationID {
			return false, m.beginTravel(ctx, b, l.ToSystemID, l.ToStationID, string(TaskPosHaul))
		}
		delivered, err := m.consortiumDeliver(ctx, b, l)
		if err != nil {
			if delivered == 0 {
				m.closeHaul(ctx, l, "stuck", err.Error())
				return finish("could not unload at the yard — " + err.Error())
			}
			return false, err
		}
		return m.haulSettle(ctx, b, l, fmt.Sprintf("delivered %d unit(s) to %s's yard at %s", delivered,
			ownerNameOf(ctx, m.db, l.ToID), m.stationLabel(ctx, l.ToStationID)), finish)
	default:
		if b.StationID != l.ToStationID {
			return false, m.beginTravel(ctx, b, l.ToSystemID, l.ToStationID, string(TaskPosHaul))
		}
		office := m.officeAt(ctx, b.CorpID, l.ToStationID)
		if office == 0 {
			m.closeHaul(ctx, l, "stuck", "no office at the destination")
			return finish("its corporation has no office where the cargo was to go — it rides on in the hold")
		}
		delivered := 0
		for i := range l.Carrying {
			c := &l.Carrying[i]
			if c.Qty <= 0 {
				continue
			}
			got, err := m.moveUnits(ctx, b.CorpID, b.ShipID, 5, c.TypeID, c.Qty, office, l.ToFlag, b.CorpID)
			if err != nil {
				return false, err
			}
			c.Qty -= got
			delivered += int(got)
		}
		return m.haulSettle(ctx, b, l, fmt.Sprintf("delivered %d unit(s) to %s", delivered, m.stationLabel(ctx, l.ToStationID)), finish)
	}
}

// haulSettle pays the trip and closes or re-opens the leg.
func (m *Mod) haulSettle(ctx context.Context, b *botRun, l *haulLeg, note string, finish func(string) (bool, error)) (bool, error) {
	l.Trips++
	// Pay this trip's share: the leg's wage in proportion to the m³ this
	// trip carried, and the whole of what is left once nothing remains.
	share := l.Wage
	if l.M3 > 0 && l.remaining() > 0 {
		share = l.Wage * math.Min(1, b.State.Data.PosTripM3/l.M3)
	}
	b.State.Data.PosTripM3 = 0
	m.payCorpWage(ctx, b.CorpID, b.CharID, share, l.Kind+" haul")
	m.spendCommit(ctx, "haul", l.LegID, share)
	stuck := l.carryingAny()
	l.Carrying = nil
	switch {
	case stuck:
		// Something would not go in (a full bay, an anchor refused): the
		// leftovers ride back to the source on a fresh leg the desk posts.
		m.closeHaul(ctx, l, "stuck", "part of the cargo could not be delivered: "+note)
		m.returnLeftovers(ctx, b, l)
	case l.remaining() <= 0 || l.Trips >= haulMaxTrips:
		m.closeHaul(ctx, l, "done", note)
	default:
		l.Status, l.CarrierID, l.ClaimedAt = "open", 0, 0
		if err := m.saveHaulProgress(ctx, l); err != nil {
			return false, err
		}
	}
	if l.SwappedOut > 0 && !stuck {
		// Surplus taken out of a full bay rides home to the Fuel division.
		m.returnLeftovers(ctx, b, l)
	}
	if c, _ := getCEO(ctx, m.db, b.CorpID); c != nil {
		m.ceoLog(ctx, c, powerStarbase, "hauled", b.CharID, share, "%s ran a %s leg — %s",
			m.charNameOr(ctx, b.CharID), l.Kind, note)
	}
	if l.AtPlanID != 0 {
		// Still on the tower's grid: take its backhaul home in the same trip.
		next, err := m.chainBackhaul(ctx, b, l.AtPlanID, m.shipHoldRoom(ctx, b))
		if err != nil {
			rlogf("rolebots: chain backhaul for %d at plan %d: %v", b.CharID, l.AtPlanID, err)
		}
		if next != nil {
			m.logBot(b.CharID, logInfo, "corp", "ran its corporation's %s leg: %s", l.Kind, note)
			m.logBot(b.CharID, logInfo, "corp", "took the tower's %s leg home on the same trip (%.0f m³, pays %s)",
				next.Kind, next.M3, iskString(next.Wage))
			b.State.Data.PosLegID = next.LegID
			return m.haulLoad(ctx, b, next, finish)
		}
	}
	return finish("ran its corporation's " + l.Kind + " leg: " + note)
}

// chainBackhaul claims the leg home from the tower a carrier has just delivered to, so a fuel run
// and its backhaul are one round trip. It takes the first leg chainBackhaulOK passes that this
// pilot may carry, through the board's claim lock (claimHaulLeg). room is the hold's free m³ after
// the delivery. Nil when nothing qualifies or another carrier holds every candidate.
func (m *Mod) chainBackhaul(ctx context.Context, b *botRun, planID int64, room float64) (*haulLeg, error) {
	if planID == 0 || b.StationID != 0 || room < haulMinRoom {
		return nil, nil
	}
	now := clock.Unix()
	legs, err := m.queryHauls(ctx, `
		WHERE corporationID = ? AND planID = ? AND kind IN (?, ?) AND fromKind = ? AND toKind = ?
		  AND (status = 'open' OR (status = 'claimed' AND claimedAt < ? AND COALESCE(carrying, '') = ''))
		 ORDER BY deadline, legID LIMIT 6`,
		b.CorpID, planID, haulKindBackhaul, haulKindGoo, haulFromStructure, haulToOffice, now-haulClaimSec)
	if err != nil || len(legs) == 0 {
		return nil, err
	}
	vol := func(t int64) float64 { return m.typeVolume(ctx, t) }
	for _, l := range legs {
		if !chainBackhaulOK(l, planID, b.SystemID, now, room, vol) {
			continue
		}
		if at, ok := haulTooBig.Load(fmt.Sprintf("%d|%d", b.CharID, l.LegID)); ok && now-at.(int64) < 86400 {
			continue
		}
		if legCarriesOriginal(l) && !m.isCorpOfficer(ctx, b.CorpID, b.CharID) {
			continue // a blueprint original moves only in a Director's hold (postowerlab.go)
		}
		won, err := m.claimHaulLeg(ctx, l.LegID, b.CharID, now)
		if err != nil {
			return nil, err
		}
		if !won {
			continue
		}
		l.Status, l.CarrierID, l.ClaimedAt = "claimed", b.CharID, now
		return l, nil
	}
	return nil, nil
}

// chainBackhaulOK is whether a carrier on plan planID's tower grid in systemID, with room m³
// free, can take leg l home on the same trip: a backhaul or goo leg of that plan from a structure
// there to an office, open or given up (claimed over haulClaimSec ago with nothing aboard), not
// cargo in another ship's hold (a "return" leg), with something left whose biggest unit fits the
// hold. Pure.
func chainBackhaulOK(l *haulLeg, planID, systemID, now int64, room float64, vol func(int64) float64) bool {
	if l.Kind != haulKindBackhaul && l.Kind != haulKindGoo {
		return false
	}
	if l.PlanID != planID || l.FromKind != haulFromStructure || l.ToKind != haulToOffice ||
		l.FromSystemID != systemID || l.PostedBy == "return" {
		return false
	}
	switch l.Status {
	case "open":
	case "claimed":
		if l.ClaimedAt >= now-haulClaimSec || l.carryingAny() {
			return false
		}
	default:
		return false
	}
	if l.remaining() <= 0 || room < haulMinRoom {
		return false
	}
	return reqHoldFits(haulBiggestOf(l.Kind, l.Items, vol), room)
}

// returnLeftovers posts the cargo still in the carrier's hold back to the
// staging office it came from.
func (m *Mod) returnLeftovers(ctx context.Context, b *botRun, l *haulLeg) {
	c, _ := getCEO(ctx, m.db, b.CorpID)
	if c == nil {
		return
	}
	station := l.FromStationID
	if station == 0 {
		station, _ = m.corpFactoryOffice(ctx, b.CorpID)
	}
	if station == 0 {
		return
	}
	// The leftovers go home in the same carrier's hold: a leg from its ship.
	var items []haulItem
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, SUM(GREATEST(quantity,1)) FROM entity WHERE locationID = ? AND flag = 5 AND ownerID = ? GROUP BY typeID`,
		b.ShipID, b.CorpID)
	if err != nil {
		return
	}
	for rows.Next() {
		var t, q int64
		if rows.Scan(&t, &q) == nil && q > 0 {
			items = append(items, haulItem{TypeID: t, Qty: q, Flag: 5})
		}
	}
	rows.Close()
	if len(items) == 0 {
		return
	}
	id, err := m.postHaul(ctx, c, haulLeg{Kind: l.Kind, PlanID: l.PlanID, FromKind: haulFromStructure, FromID: b.ShipID,
		FromSystemID: b.SystemID, ToKind: haulToOffice, ToStationID: station, ToFlag: l.FromFlagOr(corpDivStarbase),
		Items: items, PostedBy: "return", Note: "leftovers from leg " + fmt.Sprint(l.LegID)})
	if err == nil && id != 0 {
		// Already in this ship's hold: the carrier keeps it.
		carry := make([]haulCarry, 0, len(items))
		for _, it := range items {
			carry = append(carry, haulCarry{TypeID: it.TypeID, Qty: it.Qty})
		}
		raw, _ := json.Marshal(carry)
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_haul SET status = 'claimed', carrierID = ?, claimedAt = ?, carrying = ?, items = '[]' WHERE legID = ?`,
			b.CharID, clock.Unix(), string(raw), id)
	}
}

// FromFlagOr is the leg's source division, or def for a structure source.
func (l haulLeg) FromFlagOr(def int64) int64 {
	if l.FromKind == haulFromOffice && l.FromFlag >= 0 {
		return l.FromFlag
	}
	for _, it := range l.Items {
		if it.Flag >= 4 {
			return it.Flag
		}
	}
	return def
}

// parkAtTower puts an unwatched carrier's ship beside the tower it is
// visiting, inside the shield (it is the corporation's own).
func (m *Mod) parkAtTower(ctx context.Context, b *botRun, l *haulLeg) {
	if b.ShipID == 0 || m.isEmbodied(b.CharID) {
		return
	}
	var x, y, z float64
	towerID := l.ToID
	if towerID == 0 {
		if pl, _ := m.loadTowerPlan(ctx, l.PlanID); pl != nil {
			towerID = pl.TowerID
			if towerID == 0 {
				if _, mx, my, mz, _, err := evedb.TowerAnchorPosition(ctx, m.db, pl.MoonID); err == nil {
					x, y, z = mx, my, mz
				}
			}
		}
	}
	if towerID != 0 {
		_ = m.db.QueryRowContext(ctx, `SELECT x, y, z FROM entity WHERE itemID = ?`, towerID).Scan(&x, &y, &z)
	}
	if x == 0 && y == 0 && z == 0 {
		return
	}
	off := 2500.0 + float64(b.CharID%7)*400
	_ = evedb.UpdateEntityPosition(ctx, m.db, b.ShipID, x+off, y+300, z-off/2)
}

// deliverToTower does what the leg came for at the tower: fuel into the bay,
// a tower anchored at its moon, structures anchored around it.
func (m *Mod) deliverToTower(ctx context.Context, b *botRun, l *haulLeg) (string, error) {
	switch l.Kind {
	case haulKindDeploy:
		return m.deployTower(ctx, b, l)
	case haulKindRig:
		return m.rigTower(ctx, b, l)
	case haulKindAmmo:
		p, _ := m.loadTowerPlan(ctx, l.PlanID)
		if p == nil {
			return "the tower plan is gone", nil
		}
		n, err := m.loadTowerGuns(ctx, b, l, p)
		return fmt.Sprintf("loaded %d round(s) into the tower's guns", n), err
	}
	// Fuel, ammo, goo: into the tower's (or the target structure's) bay.
	target := l.ToID
	if target == 0 {
		return "no tower to deliver to", nil
	}
	flag := l.ToFlag
	if l.Kind == haulKindFuel {
		// A bay full of what it holds over its plan is emptied of that
		// first, so the line the tower is short of goes in (posbay.go).
		l.SwappedOut = m.makeBayRoom(ctx, b, l)
	}
	moved := int64(0)
	for i := range l.Carrying {
		c := &l.Carrying[i]
		for c.Qty > 0 {
			var itemID, qty int64
			var vol float64
			if err := m.db.QueryRowContext(ctx, `
				SELECT e.itemID, GREATEST(e.quantity,1), COALESCE(t.volume, 0)
				  FROM entity e JOIN invTypes t ON t.typeID = e.typeID
				 WHERE e.locationID = ? AND e.flag = 5 AND e.ownerID = ? AND e.typeID = ?
				 ORDER BY e.itemID LIMIT 1`, b.ShipID, b.CorpID, c.TypeID).Scan(&itemID, &qty, &vol); err != nil {
				c.Qty = 0 // not in the hold any more (lost on the way)
				break
			}
			take := min(qty, c.Qty)
			// Whatever of the stack fits goes in, because the core takes a stack whole or not at all.
			if capM3, used, err := evedb.StructureBayCapacity(ctx, m.db, target, flag); err == nil && capM3 > 0 && vol > 0 {
				if fit := int64(math.Floor((capM3 - used + 1e-6) / vol)); fit < take {
					take = fit
				}
			}
			if take <= 0 {
				return fmt.Sprintf("delivered %d unit(s) before the bay filled", moved), nil
			}
			if _, err := evedb.AddToStarbaseBay(ctx, m.db, target, itemID, take, flag, b.CorpID, false); err != nil {
				if errors.Is(err, evedb.ErrBayFull) {
					return fmt.Sprintf("delivered %d unit(s) before the bay filled", moved), nil
				}
				return "", err
			}
			c.Qty -= take
			moved += take
		}
	}
	if l.Kind == haulKindFuel {
		_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_towers SET lastRunAt = ?, urgentAt = 0 WHERE planID = ?`,
			clock.Unix(), l.PlanID)
	}
	if l.SwappedOut > 0 {
		return fmt.Sprintf("put %d unit(s) into the tower's bay, taking out %d it held over its plan",
			moved, l.SwappedOut), nil
	}
	return fmt.Sprintf("put %d unit(s) into the tower's bay", moved), nil
}

func init() {
	// The refusal lapses after a day (the read checks); sweep what lapsed.
	registerSweep("haulTooBigSweep", func() {
		now := clock.Unix()
		haulTooBig.Range(func(k, v any) bool {
			if now-v.(int64) >= 86400 {
				haulTooBig.Delete(k)
			}
			return true
		})
	})
	// Keys are "charID|legID" strings, so one pilot is a prefix scan.
	registerCharForget("haulTooBig", func(id int64) {
		prefix := fmt.Sprintf("%d|", id)
		haulTooBig.Range(func(k, _ any) bool {
			if s, ok := k.(string); ok && strings.HasPrefix(s, prefix) {
				haulTooBig.Delete(k)
			}
			return true
		})
	})
}
