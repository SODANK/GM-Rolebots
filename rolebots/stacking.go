package rolebots

// A bot's hangar keeps one row per type. Queries that look for "the stack of X at this station"
// (industrySell, landLootInHangar, tradeorders) take the biggest row, so scattered stacks of one
// ore would leave the rest unlisted. Stacking follows the client's rule: merge within one
// (typeID, flag, ownerID) group, non-singleton rows only, except ships (6) and blueprints (9).
// A hull row is an identity (setFitShip stores its itemID; assembleOneHull unpacks one hull) and a
// running industry project remembers IndBPItemID, so merging either deletes a row in use. That
// is why the module has its own merge and does not call db.StackAllItems.
// Call layers: landing sites tidy what they land; tidyOnDock catches market fills, job deliveries
// and handed goods. Merging is a DB tidy with no client notification; an open window shows it on
// refresh.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

// Flags this module stacks. A hangar and a cargo hold are separate
// containers; groups are keyed on flag, so one call per flag is what keeps
// them from bleeding into each other.
const (
	flagHangar int64 = 4
	flagCargo  int64 = 5
)

// stackHangar folds every duplicate stack a bot owns in one station hangar
// into a single row. Cheap enough to call on any landing: one query, and
// writes only for the types that actually have duplicates.
func (m *Mod) stackHangar(ctx context.Context, charID, stationID int64) {
	m.stackContainer(ctx, charID, stationID, flagHangar)
}

// stackCargo does the same for a ship's cargo hold, which fills stack by stack like a hangar.
func (m *Mod) stackCargo(ctx context.Context, charID, shipID int64) {
	m.stackContainer(ctx, charID, shipID, flagCargo)
}

// tidyOnDock stacks a bot's hangar and its ship's hold when it docks. It catches goods this
// module never wrote itself: market fills from the core's matching engine, CompleteJob
// outputs, anything a GM dropped in.
func (m *Mod) tidyOnDock(ctx context.Context, b *botRun) {
	if b.StationID != 0 {
		m.stackHangar(ctx, b.CharID, b.StationID)
	}
	if b.ShipID != 0 {
		m.stackCargo(ctx, b.CharID, b.ShipID)
	}
}

// â”€â”€ the sweep â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// How often the roster-wide tidy runs, and how many containers one pass touches (tick.Manager
// runs at 1s). The cap spreads a long-scattered database over several passes.
const (
	stackSweepEveryTicks = 3600 // hourly: the scan is a GROUP BY over every bot-owned stack
	stackSweepMaxPerPass = 100  // containers
)

// stackSweepRunning is the self-overlap guard. Periodics fire synchronously
// off the tick goroutine, and the first pass on a scattered database has real
// work to do.
var stackSweepRunning atomic.Bool

// startStackSweep registers the roster-wide tidy. The landing sites and the dock cover new
// landings; this covers hangars already scattered and bots that land goods and never dock
// again. It ignores the master switch, since a roster turned off is when nothing else tidies.
func (m *Mod) startStackSweep() {
	m.registerPeriodic("rolebot-stack-sweep", stackSweepEveryTicks, func(uint64) bool {
		if !stackSweepRunning.CompareAndSwap(false, true) {
			return true
		}
		goSafe("stack sweep", func() {
			defer stackSweepRunning.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.sweepScatteredStacks(ctx)
		})
		return true
	})
}

// sweepScatteredStacks finds the containers holding more than one row of something and tidies
// each one. One query finds them all; a per-bot walk would cost a query per rostered character.
func (m *Mod) sweepScatteredStacks(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.ownerID, e.locationID, e.flag
		  FROM entity e
		  JOIN rolebot_characters rc ON rc.characterID = e.ownerID
		  LEFT JOIN invTypes t  ON t.typeID  = e.typeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.flag IN (?, ?) AND e.singleton = 0 AND e.quantity > 0
		   AND COALESCE(g.categoryID, 0) NOT IN (6, 9)
		 GROUP BY e.ownerID, e.locationID, e.flag, e.typeID, e.contraband
		HAVING COUNT(*) > 1`,
		flagHangar, flagCargo)
	if err != nil {
		rlogf("rolebots: stack sweep scan: %v", err)
		return
	}
	type container struct{ ownerID, locationID, flag int64 }
	seen := map[container]bool{}
	var todo []container
	for rows.Next() {
		var c container
		if err := rows.Scan(&c.ownerID, &c.locationID, &c.flag); err != nil {
			rows.Close()
			rlogf("rolebots: stack sweep scan: %v", err)
			return
		}
		// One container can be scattered in several types at once; it is
		// merged once, whole.
		if seen[c] {
			continue
		}
		seen[c] = true
		todo = append(todo, c)
		if len(todo) >= stackSweepMaxPerPass {
			break
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		rlogf("rolebots: stack sweep scan: %v", err)
		return
	}
	for _, c := range todo {
		if ctx.Err() != nil {
			return
		}
		m.stackContainer(ctx, c.ownerID, c.locationID, c.flag)
	}
	if len(todo) > 0 {
		rlogf("rolebots: stacked %d scattered container(s)", len(todo))
	}
}

// stackContainer is the shared body. Failures are logged and dropped: a
// container that did not get tidied is untidy, not broken, and no caller has
// anything better to do about it than carry on.
func (m *Mod) stackContainer(ctx context.Context, ownerID, locationID, flag int64) {
	if ownerID == 0 || locationID == 0 {
		return
	}
	// Own deadline, detached from the caller's cancellation, so a merge is never cancelled
	// halfway through.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := m.mergeContainerStacks(sctx, ownerID, locationID, flag); err != nil {
		rlogf("rolebots: stack %d flag %d for %d: %v", locationID, flag, ownerID, err)
	}
}

// mergeContainerStacks grows the oldest row (lowest itemID) of each duplicated group and
// deletes the rest. The read runs inside the transaction with FOR UPDATE: the tidy sweep and
// the sim step bots concurrently, and an unlocked merge could delete a row another worker had
// just grown.
func (m *Mod) mergeContainerStacks(ctx context.Context, ownerID, locationID, flag int64) error {
	// Nothing to merge is the normal case, so ask without locking first. The locked read below is
	// paid only for a container that holds a duplicated group.
	var anyDup int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM entity
		 WHERE locationID = ? AND ownerID = ? AND flag = ? AND singleton = 0 AND quantity > 0
		 GROUP BY typeID, contraband HAVING COUNT(*) > 1 LIMIT 1`,
		locationID, ownerID, flag).Scan(&anyDup); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// No JOIN: FOR UPDATE would lock the shared invGroups rows and deadlock with the market's sale
	// path. The category filter runs in Go below.
	rows, err := tx.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.contraband, e.quantity
		  FROM entity e
		 WHERE e.locationID = ? AND e.ownerID = ? AND e.flag = ?
		   AND e.singleton = 0 AND e.quantity > 0
		 ORDER BY e.itemID
		 FOR UPDATE`,
		locationID, ownerID, flag)
	if err != nil {
		return err
	}
	// Contraband is part of the key: a stack that is illegal here and one
	// that is not are not the same goods, whatever invTypes says.
	type key struct{ typeID, contraband int64 }
	type group struct {
		keeper, total int64
		extras        []int64
	}
	type stackRow struct{ itemID, typeID, contraband, qty int64 }
	var scanned []stackRow
	seenType := map[int64]bool{}
	for rows.Next() {
		var r stackRow
		if err := rows.Scan(&r.itemID, &r.typeID, &r.contraband, &r.qty); err != nil {
			rows.Close()
			return err
		}
		scanned = append(scanned, r)
		seenType[r.typeID] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(scanned) == 0 {
		return nil
	}

	// Ships (6) and blueprints (9) never merge: each is its own item with fittings or research a
	// quantity cannot express. Resolved here, without locks.
	mergeable, err := mergeableTypes(ctx, tx, seenType)
	if err != nil {
		return err
	}

	groups := map[key]*group{}
	var order []key
	for _, r := range scanned {
		if !mergeable[r.typeID] {
			continue
		}
		k := key{r.typeID, r.contraband}
		g := groups[k]
		if g == nil {
			groups[k] = &group{keeper: r.itemID, total: r.qty}
			order = append(order, k)
			continue
		}
		g.total += r.qty
		g.extras = append(g.extras, r.itemID)
	}

	var dup []*group
	for _, k := range order {
		if g := groups[k]; len(g.extras) > 0 {
			dup = append(dup, g)
		}
	}
	if len(dup) == 0 {
		return nil
	}

	for _, g := range dup {
		for _, id := range g.extras {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM entity WHERE itemID = ?`, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = ? WHERE itemID = ?`, g.total, g.keeper); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// mergeableTypes reports, for each typeID seen in a container, whether its stacks may be
// folded together: everything except ships (category 6) and blueprints (category 9). It reads
// without locks, so the invTypes/invGroups lookup takes none shared with other pilots. A type
// missing from invTypes counts as mergeable.
func mergeableTypes(ctx context.Context, tx *sql.Tx, types map[int64]bool) (map[int64]bool, error) {
	out := make(map[int64]bool, len(types))
	ids := make([]interface{}, 0, len(types))
	for id := range types {
		out[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT t.typeID, g.categoryID
		  FROM invTypes t
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, categoryID int64
		if err := rows.Scan(&typeID, &categoryID); err != nil {
			return nil, err
		}
		if categoryID == 6 || categoryID == 9 {
			out[typeID] = false
		}
	}
	return out, rows.Err()
}
