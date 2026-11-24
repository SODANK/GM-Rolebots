package rolebots

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The vault locks a managed corporation's blueprint originals.
// Core corp lockdown (db/corplockdown.go) stops a print being moved, taken or sold but
// leaves it usable in a job. Every original a managed corporation owns at a station is
// locked here, so a human member with Library access cannot walk off with it.
// Copies are never locked (the core refuses them).
// A flow that moves an original out of an office calls unlockPrint first and lockPrint at
// the destination (corpfactorymove.go), or refuses locked items (moveUnits, corpprintsale.go).
// A lock row outlives an ownership change until the core's boot prune.

const (
	settingCorpLockdown = "rolebots.corp.lockdown"

	vaultDeskEverySec   = 600 // the keeper's cadence per corporation
	vaultLockPerPass    = 50  // lock pushes per pass, spreads the first sweep
	vaultUnpackPerPass  = 20  // packaged originals made singleton per pass
	stationBandFirst    = 60000000
	stationBandPastLast = 64000000
)

var lockdownOn atomic.Bool

func init() { lockdownOn.Store(true) }

// lockdownEnabled is the switch. Off stops new locks only: unlocking before a move and
// clearing stale locks always run.
func lockdownEnabled() bool { return lockdownOn.Load() }

func (m *Mod) loadLockdownSettings(ctx context.Context) {
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, settingCorpLockdown); err == nil && ok {
		lockdownOn.Store(truthySetting(v))
	}
}

var vaultNoHookOnce sync.Once

// pushLockChange tells the corporation's online members (corp window, hangar drag
// refusal). Without the hook the lock still holds server-side; members see it on relog.
func pushLockChange(li evedb.LockedItem, added bool) {
	if h := modapi.Corp().LockedItemChanged; h != nil {
		h(li, added)
		return
	}
	vaultNoHookOnce.Do(func() {
		rlogf("rolebots: corp lockdown: no LockedItemChanged hook; members see locks on relog")
	})
}

// lockPrint locks one corporation original where it stands. It returns false with a nil
// error when it is already locked or the core refuses it (a copy, packaged, not at a station).
func (m *Mod) lockPrint(ctx context.Context, corpID, itemID int64) (bool, error) {
	if !lockdownEnabled() || corpID == 0 || itemID == 0 {
		return false, nil
	}
	// A human corporation's lockdowns are its members' votes.
	if _, human := humanChaired(corpID); human {
		return false, nil
	}
	if _, ok, err := evedb.GetLockedItem(ctx, m.db, corpID, itemID); err != nil || ok {
		return false, err
	}
	li, refusal, err := evedb.LockItem(ctx, m.db, corpID, itemID, 0)
	if err != nil || refusal != "" {
		return false, err
	}
	pushLockChange(li, true)
	return true, nil
}

// unlockPrint lifts the corporation's lock on itemID. It returns false with a nil error
// when there was none.
func (m *Mod) unlockPrint(ctx context.Context, corpID, itemID int64) (bool, error) {
	if corpID == 0 || itemID == 0 {
		return false, nil
	}
	if _, human := humanChaired(corpID); human {
		return false, nil
	}
	li, refusal, err := evedb.UnlockItem(ctx, m.db, corpID, itemID)
	if err != nil || refusal != "" {
		return false, err
	}
	pushLockChange(li, false)
	return true, nil
}

// lockedByVote reports whether a human corporation's members voted an item locked down: nothing the
// desks do moves it. A read that fails counts as locked.
func (m *Mod) lockedByVote(ctx context.Context, corpID, itemID int64) bool {
	if _, human := humanChaired(corpID); !human {
		return false
	}
	_, locked, err := evedb.GetLockedItem(ctx, m.db, corpID, itemID)
	return err != nil || locked
}

// ── the plan (pure) ──────────────────────────────────────────────────────────

// vaultRow is one item the keeper looks at: a corporation-owned original, or one of the
// corporation's lock rows whose item has gone or changed hands.
type vaultRow struct {
	ItemID      int64
	Exists      bool  // the entity row is there
	OwnedByCorp bool  // ownerID is the corporation
	Original    bool  // a blueprint original
	Singleton   bool  // not a packaged stack
	StationNow  int64 // office's station, or the station floor; 0 elsewhere
	Locked      bool  // this corporation holds a lock row on it
	LockedAt    int64 // the lock row's station
	// InTransit: a capsuleer claimed it for a move between bases and it was unlocked so the
	// client lets it out (corpprintmove.go).
	InTransit bool
}

// vaultPlan is what one keeper pass does.
type vaultPlan struct {
	Unlock []int64 // stale locks: item gone, sold, carried off or moved station
	Lock   []int64 // lockable originals with no lock (incl. re-locks after Unlock)
	Unpack []int64 // packaged originals that need making singleton first
}

// planLockdown mirrors LockRefusal: a lock may stand on a corporation-owned singleton
// original at a station. A lock that fails that comes off; one whose item moved station
// comes off and goes back on.
func planLockdown(rows []vaultRow) vaultPlan {
	var p vaultPlan
	for _, r := range rows {
		lockable := r.Exists && r.OwnedByCorp && r.Original && r.StationNow != 0
		if r.Locked {
			if lockable && r.Singleton && r.StationNow == r.LockedAt {
				continue
			}
			p.Unlock = append(p.Unlock, r.ItemID)
		}
		if !lockable || r.InTransit {
			continue
		}
		if r.Singleton {
			p.Lock = append(p.Lock, r.ItemID)
		} else {
			p.Unpack = append(p.Unpack, r.ItemID)
		}
	}
	return p
}

// ── the keeper ───────────────────────────────────────────────────────────────

// vaultRows reads the corporation's originals and its lock rows.
func (m *Mod) vaultRows(ctx context.Context, corpID int64) ([]vaultRow, error) {
	var out []vaultRow
	// Every original the corporation owns, wherever it is: an office, the station floor,
	// a hold, a tower array.
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.singleton,
		       COALESCE(o.stationID,
		                IF(e.locationID >= ? AND e.locationID < ?, e.locationID, 0)),
		       COALESCE(l.locationID, -1),
		       EXISTS (SELECT 1 FROM rolebot_corp_print_moves pm
		                WHERE pm.itemID = e.itemID AND pm.status = 'claimed' AND pm.human = 1)
		  FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		  LEFT JOIN crpoffices o ON o.itemID = e.locationID
		  LEFT JOIN crpLockedItems l ON l.itemID = e.itemID AND l.corporationID = e.ownerID
		 WHERE e.ownerID = ? AND COALESCE(b.copy, 0) = 0`,
		stationBandFirst, stationBandPastLast, corpID)
	if err != nil {
		return nil, fmt.Errorf("vault originals: %w", err)
	}
	for rows.Next() {
		r := vaultRow{Exists: true, OwnedByCorp: true, Original: true}
		var single int
		var lockedAt int64
		var transit int
		if err := rows.Scan(&r.ItemID, &single, &r.StationNow, &lockedAt, &transit); err != nil {
			rows.Close()
			return nil, err
		}
		r.Singleton = single != 0
		r.InTransit = transit != 0
		if lockedAt >= 0 {
			r.Locked, r.LockedAt = true, lockedAt
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The corporation's lock rows on items it no longer owns or that are gone.
	stale, err := m.db.QueryContext(ctx, `
		SELECT l.itemID, l.locationID, e.itemID IS NOT NULL
		  FROM crpLockedItems l
		  LEFT JOIN entity e ON e.itemID = l.itemID
		 WHERE l.corporationID = ? AND (e.itemID IS NULL OR e.ownerID <> l.corporationID)`, corpID)
	if err != nil {
		return nil, fmt.Errorf("vault stale locks: %w", err)
	}
	defer stale.Close()
	for stale.Next() {
		r := vaultRow{Locked: true}
		if err := stale.Scan(&r.ItemID, &r.LockedAt, &r.Exists); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, stale.Err()
}

// vaultDesk is the keeper's pass for one corporation: clear stale locks, unpack packaged
// originals, lock every lockable original. It is idempotent.
func (m *Mod) vaultDesk(ctx context.Context, c *corpCEO) {
	if c == nil {
		return
	}
	m.loadLockdownSettings(ctx)
	rows, err := m.vaultRows(ctx, c.CorpID)
	if err != nil {
		m.logBot(c.CharID, logError, "ceo", "vault: %v", err)
		return
	}
	p := planLockdown(rows)
	var cleared, locked int
	for _, id := range p.Unlock {
		if ok, err := m.unlockPrint(ctx, c.CorpID, id); err != nil {
			m.logBot(c.CharID, logError, "ceo", "vault unlock %d: %v", id, err)
		} else if ok {
			cleared++
		}
	}
	if lockdownEnabled() {
		for i, id := range p.Unpack {
			if i >= vaultUnpackPerPass || ctx.Err() != nil {
				break
			}
			// A stack of one becomes the singleton itself; a bigger stack has one unit split off and
			// the next pass picks the new item up.
			newID, _, _, err := evedb.EnsureBlueprintSingleton(ctx, m.db, id)
			if err != nil {
				m.logBot(c.CharID, logError, "ceo", "vault unpack %d: %v", id, err)
				continue
			}
			if newID == id {
				p.Lock = append(p.Lock, id)
			}
		}
		for _, id := range p.Lock {
			if locked >= vaultLockPerPass || ctx.Err() != nil {
				break
			}
			if ok, err := m.lockPrint(ctx, c.CorpID, id); err != nil {
				m.logBot(c.CharID, logError, "ceo", "vault lock %d: %v", id, err)
			} else if ok {
				locked++
			}
		}
	}
	if locked > 0 || cleared > 0 {
		m.ceoLog(ctx, c, powerProduction, "lockdown", 0, float64(locked),
			"locked %d of the corporation's blueprint originals down (%d stale lock(s) cleared)", locked, cleared)
	}
}

// lockArrivedPrint locks a print that just came into the corporation's keeping. Errors
// are logged only: the keeper's next pass catches anything this misses.
func (m *Mod) lockArrivedPrint(ctx context.Context, corpID, itemID int64) {
	if _, ok := managedCEO(corpID); !ok {
		return
	}
	if _, err := m.lockPrint(ctx, corpID, itemID); err != nil {
		rlogf("rolebots: lock arrived print %d (corp %d): %v", itemID, corpID, err)
	}
}
