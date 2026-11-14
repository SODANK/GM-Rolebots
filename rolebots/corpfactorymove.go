package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	evedb "gurimoon/db"
)

// Moving the factory: carrying the desk's stock from an office that stopped
// being the factory to the one that is. When the estate power pins a new
// factory (corpfacility.go), the old office keeps the library, Materials and
// store stock, which the work board, buyback and store read only at the factory
// office. So the director or deputy flies to the old office, loads a hold (prints
// first), flies to the new factory and puts each item back in its division, up
// to corpMoveMaxTrips trips. Ore, assembled ships and loaded containers do not
// move; prints in a running job (flag 111) wait for it. In the hold the items
// stay the corporation's: every cargo sweep filters on the pilot's own ownerID,
// so they cannot be sold or stacked into the pilot's goods on the way.

const (
	// corpMoveClaimSec is how long a carrier's claim on an empty-handed move
	// holds before the other officer may take it.
	corpMoveClaimSec = 6 * 3600
	// corpMoveMaxTrips ends a move that keeps finding stock: a hold too small for
	// what is left beats flying the lane forever.
	corpMoveMaxTrips = 10
	// corpMoveMinRoom is the least free hold worth a trip.
	corpMoveMinRoom = 0.5
)

// corpMoveDivisions are the desk's divisions a move carries (flags).
const corpMoveDivisions = "4, 116, 117, 118"

// movedItem is one item in the carrier's hold and the division it goes back
// into. Lock marks an original that was locked at the old office: the lock comes
// off for the trip and goes back on at the new one (corpvault.go).
type movedItem struct {
	ItemID int64 `json:"i"`
	Flag   int64 `json:"f"`
	Lock   bool  `json:"l,omitempty"`
}

// corpMove is a rolebot_corp_moves row.
type corpMove struct {
	CorpID        int64
	FromOfficeID  int64
	FromStationID int64
	Status        string
	CarrierID     int64
	ClaimedAt     int64
	Carrying      []movedItem
	Trips         int
}

// openFactoryMove opens (or re-opens) the move off one office.
func (m *Mod) openFactoryMove(ctx context.Context, corpID, fromOfficeID, fromStationID int64) {
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_moves (corporationID, fromOfficeID, fromStationID, openedAt)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			openedAt = IF(status = 'open', openedAt, VALUES(openedAt)),
			trips    = IF(status = 'open', trips, 0),
			closedAt = IF(status = 'open', closedAt, 0),
			status   = 'open'`,
		corpID, fromOfficeID, fromStationID, now); err != nil {
		rlogf("rolebots: open factory move %d/%d: %v", corpID, fromOfficeID, err)
	}
}

func scanCorpMove(row interface{ Scan(...any) error }) (*corpMove, error) {
	var mv corpMove
	var carrying sql.NullString
	if err := row.Scan(&mv.CorpID, &mv.FromOfficeID, &mv.FromStationID, &mv.Status,
		&mv.CarrierID, &mv.ClaimedAt, &carrying, &mv.Trips); err != nil {
		return nil, err
	}
	if carrying.Valid && carrying.String != "" {
		_ = json.Unmarshal([]byte(carrying.String), &mv.Carrying)
	}
	return &mv, nil
}

const corpMoveCols = `corporationID, fromOfficeID, fromStationID, status, carrierID, claimedAt, carrying, trips`

// loadCorpMove reads one move, nil when there is none.
func (m *Mod) loadCorpMove(ctx context.Context, corpID, fromOfficeID int64) (*corpMove, error) {
	mv, err := scanCorpMove(m.db.QueryRowContext(ctx, `
		SELECT `+corpMoveCols+` FROM rolebot_corp_moves
		 WHERE corporationID = ? AND fromOfficeID = ?`, corpID, fromOfficeID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return mv, err
}

// saveMoveCarry writes what is in the hold (nil = nothing).
func (m *Mod) saveMoveCarry(ctx context.Context, mv *corpMove, items []movedItem) error {
	var text any
	if len(items) > 0 {
		raw, err := json.Marshal(items)
		if err != nil {
			return err
		}
		text = string(raw)
	}
	_, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_moves SET carrying = ? WHERE corporationID = ? AND fromOfficeID = ?`,
		text, mv.CorpID, mv.FromOfficeID)
	mv.Carrying = items
	return err
}

// closeFactoryMove ends a move.
func (m *Mod) closeFactoryMove(ctx context.Context, mv *corpMove, status, note string) {
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_moves SET status = ?, closedAt = ?, carrierID = 0, carrying = NULL, note = ?
		 WHERE corporationID = ? AND fromOfficeID = ?`,
		status, clock.Unix(), clampText(note, 255), mv.CorpID, mv.FromOfficeID); err != nil {
		rlogf("rolebots: close factory move %d/%d: %v", mv.CorpID, mv.FromOfficeID, err)
	}
	mv.Status = status
}

// moveStock is one stack at the old office a hold could carry.
type moveStock struct {
	ItemID, TypeID, Flag, Qty int64
	Volume                    float64 // the whole stack, m³
	Value                     float64 // the whole stack, ISK
	Print                     bool
}

// movableStock is what is left to carry off the old office.
func (m *Mod) movableStock(ctx context.Context, corpID, officeID int64) ([]moveStock, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.flag, e.quantity, COALESCE(t.volume, 0),
		       (bt.blueprintTypeID IS NOT NULL)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag IN (`+corpMoveDivisions+`)
		   AND g.categoryID NOT IN (6, 25)
		   AND NOT EXISTS (SELECT 1 FROM entity x WHERE x.locationID = e.itemID)`, corpID, officeID)
	if err != nil {
		return nil, fmt.Errorf("movableStock: %w", err)
	}
	var out []moveStock
	var ids []int64
	for rows.Next() {
		var s moveStock
		var unit float64
		if err := rows.Scan(&s.ItemID, &s.TypeID, &s.Flag, &s.Qty, &unit, &s.Print); err != nil {
			rows.Close()
			return nil, err
		}
		s.Qty = max(s.Qty, 1)
		s.Volume = unit * float64(s.Qty)
		out = append(out, s)
		ids = append(ids, s.TypeID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		if vals, err := evedb.TypeValues(ctx, m.db, ids); err == nil {
			for i := range out {
				out[i].Value = vals[out[i].TypeID].Value * float64(out[i].Qty)
			}
		}
	}
	return out, nil
}

// pickMoveLoad chooses one hold's worth: prints first, then the most ISK a
// cubic metre, whole stacks only. Pure, for the test.
func pickMoveLoad(stock []moveStock, room float64) []moveStock {
	s := append([]moveStock(nil), stock...)
	density := func(x moveStock) float64 {
		if x.Volume <= 0 {
			return math.MaxFloat64
		}
		return x.Value / x.Volume
	}
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Print != s[j].Print {
			return s[i].Print
		}
		if di, dj := density(s[i]), density(s[j]); di != dj {
			return di > dj
		}
		return s[i].ItemID < s[j].ItemID
	})
	var out []moveStock
	for _, x := range s {
		if x.Volume <= room {
			out = append(out, x)
			room -= x.Volume
		}
	}
	return out
}

// moveStillPending reports whether the old office will receive more of the
// desk's stock on its own: a library print out on a job there, or a
// corporation job whose output is bound for it.
func (m *Mod) moveStillPending(ctx context.Context, corpID, officeID int64) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM entity WHERE ownerID = ? AND locationID = ? AND flag = 111)
		     + (SELECT COUNT(*) FROM ramJobs WHERE ownerID = ? AND outputLocationID = ?
		                                      AND completedStatusID IN (0, 1))`,
		corpID, officeID, corpID, officeID).Scan(&n)
	return n > 0
}

// cargoUsed is the volume already in a ship's hold, whoever owns it.
func (m *Mod) cargoUsed(ctx context.Context, shipID int64) float64 {
	var v float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(COALESCE(t.volume, 0) * GREATEST(e.quantity, 1)), 0)
		  FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.locationID = ? AND e.flag = 5`, shipID).Scan(&v)
	return v
}

// corpMoveDue is the planner's pre-emption for a factory move: the
// corporation's director or deputy, with no project of its own in hand, goes
// and carries the next hold. A pilot already carrying corporation stock
// finishes that delivery before anything else.
func (m *Mod) corpMoveDue(ctx context.Context, b *botRun) (bool, error) {
	if b == nil || b.CorpID == 0 || b.State == nil || b.State.Data.IndTypeID != 0 {
		return false, nil
	}
	if _, ok := managedCEO(b.CorpID); !ok {
		return false, nil
	}
	// Which corporations have a move open at all, shared for a minute: every
	// managed member read its corporation's policy row per plan to find none.
	open, _ := snapshotFor(snapKey("corp-moves-open"), time.Minute, func() (map[int64]bool, error) {
		return m.corpIDsWhere(ctx, `SELECT DISTINCT corporationID FROM rolebot_corp_moves WHERE status = 'open'`)
	})
	if !open[b.CorpID] {
		return false, nil
	}
	p, err := m.loadCorpPolicy(ctx, b.CorpID)
	if err != nil || (b.CharID != p.DirectorID && b.CharID != p.DeputyID) {
		return false, err
	}
	now := clock.Unix()
	mv, err := scanCorpMove(m.db.QueryRowContext(ctx, `
		SELECT `+corpMoveCols+` FROM rolebot_corp_moves
		 WHERE corporationID = ? AND status = 'open'
		   AND (carrierID = ? OR ((carrierID = 0 OR claimedAt < ?) AND COALESCE(carrying, '') = ''))
		 ORDER BY (carrierID = ?) DESC, openedAt
		 LIMIT 1`, b.CorpID, b.CharID, now-corpMoveClaimSec, b.CharID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	factory, factoryOffice := m.corpFactoryOffice(ctx, b.CorpID)
	if len(mv.Carrying) == 0 {
		if factoryOffice == 0 {
			return false, nil
		}
		if mv.FromOfficeID == factoryOffice {
			m.closeFactoryMove(ctx, mv, "done", "the office is the factory again")
			return false, nil
		}
		stock, err := m.movableStock(ctx, b.CorpID, mv.FromOfficeID)
		if err != nil {
			return false, err
		}
		if len(stock) == 0 {
			if !m.moveStillPending(ctx, b.CorpID, mv.FromOfficeID) {
				m.closeFactoryMove(ctx, mv, "done", "nothing left at the old office")
			}
			return false, nil
		}
		fromSys, _ := evedb.GetSolarSystemForStation(ctx, m.db, mv.FromStationID)
		if b.StationID != mv.FromStationID {
			if j := m.jumpsTo(ctx, b, fromSys); j < 0 || j > corpWorkMaxJumps {
				return false, nil
			}
		}
		res, err := m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_moves SET carrierID = ?, claimedAt = ?
			 WHERE corporationID = ? AND fromOfficeID = ? AND status = 'open'
			   AND (carrierID = 0 OR carrierID = ? OR claimedAt < ?)`,
			b.CharID, now, b.CorpID, mv.FromOfficeID, b.CharID, now-corpMoveClaimSec)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return false, nil
		}
		mv.CarrierID = b.CharID
		m.logBot(b.CharID, logInfo, "industry",
			"fetching its corporation's factory stock from %s for the new factory at %s",
			m.stationLabel(ctx, mv.FromStationID), m.stationLabel(ctx, factory))
		m.stampCorpMove(ctx, b, mv, stock[0].TypeID, factory)
		return true, m.beginTravel(ctx, b, fromSys, mv.FromStationID, string(TaskIndCorpMove))
	}
	// Already carrying: finish the delivery (corpMoveStep picks the target).
	m.stampCorpMove(ctx, b, mv, 0, factory)
	b.State.Phase = string(TaskIndCorpMove)
	return true, m.corpMoveStep(ctx, b)
}

// stampCorpMove writes the project fields a move rides on. industryStep
// wants a type and a factory; the type is only a label here.
func (m *Mod) stampCorpMove(ctx context.Context, b *botRun, mv *corpMove, typeID, factory int64) {
	st := b.State
	st.Activity = ActIndustry
	st.Data = idleData(st.Data)
	d := &st.Data
	d.IndCorpMove = mv.FromOfficeID
	if typeID == 0 && len(mv.Carrying) > 0 {
		_ = m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, mv.Carrying[0].ItemID).Scan(&typeID)
	}
	if typeID == 0 {
		typeID = 1 // a label only; industryStep refuses a zero
	}
	d.IndTypeID = typeID
	d.IndTypeName = "its corporation's factory stock"
	if factory == 0 {
		factory = mv.FromStationID
	}
	d.IndFactoryID = factory
	d.IndFactorySysID, _ = evedb.GetSolarSystemForStation(ctx, m.db, factory)
	d.IndSellerStationID = mv.FromStationID
	d.IndSellerSysID, _ = evedb.GetSolarSystemForStation(ctx, m.db, mv.FromStationID)
	d.IndStartedAt = clock.Unix()
}

// corpMoveStep is the TaskIndCorpMove phase: at the old office, load; at the
// factory, unload.
func (m *Mod) corpMoveStep(ctx context.Context, b *botRun) error {
	d := &b.State.Data
	finish := func(note string) error {
		if note != "" {
			m.logBot(b.CharID, logInfo, "industry", "%s", note)
		}
		m.industryClearProject(ctx, b, false)
		b.State.Activity, b.State.Phase = "", string(TaskIdle)
		b.State.Data = idleData(b.State.Data)
		return saveState(ctx, m.db, b.State)
	}
	mv, err := m.loadCorpMove(ctx, b.CorpID, d.IndCorpMove)
	if err != nil {
		return err
	}
	if mv == nil || mv.Status != "open" || mv.CarrierID != b.CharID {
		return finish("its corporation's factory move was finished without it")
	}
	factory, factoryOffice := m.corpFactoryOffice(ctx, b.CorpID)
	c, _ := getCEO(ctx, m.db, b.CorpID)

	if len(mv.Carrying) == 0 {
		// ── loading ──
		if factoryOffice == 0 || factoryOffice == mv.FromOfficeID {
			m.closeFactoryMove(ctx, mv, "done", "the old office is the factory again")
			return finish("its corporation's factory is back where the stock is")
		}
		if b.StationID != mv.FromStationID {
			sys, _ := evedb.GetSolarSystemForStation(ctx, m.db, mv.FromStationID)
			return m.beginTravel(ctx, b, sys, mv.FromStationID, string(TaskIndCorpMove))
		}
		if _, ok := m.ensureRunabout(ctx, b, "the factory move"); !ok {
			m.releaseMoveClaim(ctx, mv)
			return finish("had no ship to carry its corporation's factory stock in")
		}
		room := m.shipCargoCapacity(ctx, b) - m.cargoUsed(ctx, b.ShipID)
		stock, err := m.movableStock(ctx, b.CorpID, mv.FromOfficeID)
		if err != nil {
			return err
		}
		if len(stock) == 0 {
			m.releaseMoveClaim(ctx, mv)
			if !m.moveStillPending(ctx, b.CorpID, mv.FromOfficeID) {
				m.closeFactoryMove(ctx, mv, "done", "nothing left at the old office")
			}
			return finish("found nothing left to carry at its corporation's old factory")
		}
		load := pickMoveLoad(stock, room)
		if room < corpMoveMinRoom || len(load) == 0 {
			m.closeFactoryMove(ctx, mv, "stuck",
				fmt.Sprintf("%d stack(s) left that no hold it flies can take", len(stock)))
			return finish("could not fit its corporation's remaining factory stock in its hold")
		}
		var carried []movedItem
		for _, x := range load {
			res, err := m.db.ExecContext(ctx, `
				UPDATE entity SET locationID = ?, flag = 5
				 WHERE itemID = ? AND ownerID = ? AND locationID = ?`,
				b.ShipID, x.ItemID, b.CorpID, mv.FromOfficeID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				it := movedItem{ItemID: x.ItemID, Flag: x.Flag}
				if x.Print {
					// Moved first, then unlocked: in a hold the print is no
					// longer lockable, so the keeper cannot re-lock it between.
					unlocked, err := m.unlockPrint(ctx, b.CorpID, x.ItemID)
					if err != nil {
						m.logBot(b.CharID, logError, "industry", "factory move: unlock print %d: %v", x.ItemID, err)
					}
					it.Lock = unlocked
				}
				carried = append(carried, it)
			}
		}
		if len(carried) == 0 {
			m.releaseMoveClaim(ctx, mv)
			return finish("the corporation's stock at the old factory moved before it could load it")
		}
		if err := m.saveMoveCarry(ctx, mv, carried); err != nil {
			return err
		}
		m.logBot(b.CharID, logInfo, "industry",
			"loaded %d stack(s) of its corporation's factory stock at %s for %s",
			len(carried), m.stationLabel(ctx, mv.FromStationID), m.stationLabel(ctx, factory))
		sys, _ := evedb.GetSolarSystemForStation(ctx, m.db, factory)
		d.IndFactoryID, d.IndFactorySysID = factory, sys
		return m.beginTravel(ctx, b, sys, factory, string(TaskIndCorpMove))
	}

	// ── delivering ── to the factory, or back where it came from when the
	// corporation lost its factory on the way.
	target, targetOffice := factory, factoryOffice
	if targetOffice == 0 {
		target, targetOffice = mv.FromStationID, mv.FromOfficeID
	}
	if b.StationID != target {
		sys, _ := evedb.GetSolarSystemForStation(ctx, m.db, target)
		return m.beginTravel(ctx, b, sys, target, string(TaskIndCorpMove))
	}
	delivered, lost := 0, 0
	for _, it := range mv.Carrying {
		// The item must still be in one of this pilot's ships docked here; a hull
		// swapped mid-trip leaves the stock in the old hull's hold at this station.
		var loc int64
		var shipOwner, shipAt int64
		err := m.db.QueryRowContext(ctx, `
			SELECT e.locationID, COALESCE(s.ownerID, 0), COALESCE(s.locationID, 0)
			  FROM entity e LEFT JOIN entity s ON s.itemID = e.locationID
			 WHERE e.itemID = ? AND e.ownerID = ? AND e.flag = 5`, it.ItemID, b.CorpID).Scan(&loc, &shipOwner, &shipAt)
		if err != nil || shipOwner != b.CharID || (loc != b.ShipID && shipAt != b.StationID) {
			lost++
			continue
		}
		if _, err := m.db.ExecContext(ctx, `
			UPDATE entity SET locationID = ?, flag = ? WHERE itemID = ? AND ownerID = ? AND locationID = ?`,
			targetOffice, it.Flag, it.ItemID, b.CorpID, loc); err != nil {
			return err
		}
		if it.Lock {
			if _, err := m.lockPrint(ctx, b.CorpID, it.ItemID); err != nil {
				m.logBot(b.CharID, logError, "industry", "factory move: relock print %d: %v", it.ItemID, err)
			}
		}
		delivered++
	}
	trips := mv.Trips + 1
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_moves SET carrying = NULL, carrierID = 0, trips = ?
		 WHERE corporationID = ? AND fromOfficeID = ?`, trips, mv.CorpID, mv.FromOfficeID); err != nil {
		return err
	}
	mv.Carrying, mv.Trips, mv.CarrierID = nil, trips, 0
	if c != nil {
		m.ceoLog(ctx, c, powerEstate, "haul", b.CharID, 0,
			"%s carried %d stack(s) of the desk's stock from %s to the factory at %s%s",
			m.charNameOr(ctx, b.CharID), delivered, stationNameOf(ctx, m, mv.FromStationID),
			stationNameOf(ctx, m, target), map[bool]string{true: fmt.Sprintf(" (%d lost on the way)", lost), false: ""}[lost > 0])
	}
	if targetOffice == mv.FromOfficeID {
		m.closeFactoryMove(ctx, mv, "void", "the corporation lost its factory mid-move")
	} else if left, _ := m.movableStock(ctx, b.CorpID, mv.FromOfficeID); len(left) == 0 && !m.moveStillPending(ctx, b.CorpID, mv.FromOfficeID) {
		m.closeFactoryMove(ctx, mv, "done", fmt.Sprintf("moved in %d trip(s)", trips))
	} else if trips >= corpMoveMaxTrips {
		m.closeFactoryMove(ctx, mv, "stuck", fmt.Sprintf("%d stack(s) still at the old office after %d trips", len(left), trips))
	}
	return finish(fmt.Sprintf("delivered %d stack(s) of its corporation's factory stock to %s",
		delivered, m.stationLabel(ctx, target)))
}

// releaseMoveClaim frees an empty-handed claim so the other officer may take
// the move.
func (m *Mod) releaseMoveClaim(ctx context.Context, mv *corpMove) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_moves SET carrierID = 0
		 WHERE corporationID = ? AND fromOfficeID = ? AND COALESCE(carrying, '') = ''`,
		mv.CorpID, mv.FromOfficeID)
}
