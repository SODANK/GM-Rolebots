package rolebots

import (
	"context"
	"sync"
	"sync/atomic"
)

// The roster cache: the roster query's rows, kept between passes for choosing who acts.
//
// The roster query (loadRosterWhere) reads one row per enabled pilot, with two
// correlated subqueries each. Since the pass loads only the pilots due a turn
// (rosterwake.go), it comes round every five seconds, and re-reading every row every
// pass is most of what is left of its cost. Choosing who acts needs few of those
// fields (rosterDue reads the gang and the commander, gridLaneOwns the gang), and
// they change by event, so the pass chooses from a copy at most rosterCacheSec old.
//
// Nothing acts on a cached row. Before a pilot takes a turn, or the online cap or its
// hours decide whether to log it off (which reads where it is), its row is read again
// (readyRuns), and only then its state. A turn therefore sees the same fresh row it
// always did, and a pilot no longer enabled is dropped at that read.
//
// The copy is thrown away when the fields it chooses by change: a pilot enabled or
// disabled (setRolebotEnabled), purged or removed (the forget registry), a gang's
// members changing (addGangMember, removeGangMember, deleteGang, the fleet mirror's
// riders) and a conscript taken on, released or handed to a new commander. The
// age limit catches anything else, such as a pilot joining a corporation, which is
// about as stale as rosterCorpOf was when one pass took a minute.

// rosterCacheSec is the oldest copy the pass chooses from.
const rosterCacheSec = 30

var (
	rosterCacheMu   sync.Mutex
	rosterCacheRows []botRun // value copies, State nil
	rosterCacheAt   int64
	// rosterCacheDirty is set by any change the copy cannot see.
	rosterCacheDirty atomic.Bool
)

func init() {
	registerCharForget("rosterCacheRows", func(int64) { rosterCacheInvalidate() })
}

// rosterCacheInvalidate makes the next pass read the roster again.
func rosterCacheInvalidate() { rosterCacheDirty.Store(true) }

// rosterForPass is this pass's roster: fresh rows when the copy is old or dirty (and
// the copy is replaced), otherwise new runs built from the copy, marked not fresh.
// Each pass gets its own runs, so nothing loaded in one pass survives into the next.
func (m *Mod) rosterForPass(ctx context.Context) ([]*botRun, error) {
	now := clock.Unix()
	rosterCacheMu.Lock()
	usable := rosterCacheRows != nil && now-rosterCacheAt < rosterCacheSec
	rows := rosterCacheRows
	rosterCacheMu.Unlock()

	if !usable || rosterCacheDirty.Load() {
		// Cleared before the read: a change landing during it dirties the new copy.
		rosterCacheDirty.Store(false)
		runs, err := m.loadRosterWhere(ctx, "", nil)
		if err != nil {
			rosterCacheDirty.Store(true)
			return nil, err
		}
		copyRows := make([]botRun, len(runs))
		for i, b := range runs {
			copyRows[i] = *b
			copyRows[i].rowFresh = false
		}
		rosterCacheMu.Lock()
		rosterCacheRows, rosterCacheAt = copyRows, now
		rosterCacheMu.Unlock()
		return runs, nil
	}

	out := make([]*botRun, len(rows))
	for i := range rows {
		b := rows[i] // copy
		out[i] = &b
	}
	return out, nil
}

// readyRuns makes runs fit to act on: a run built from the copy has its roster row
// read again, then every run without state is loaded (hydrateMissing). A run whose
// pilot is no longer an enabled rolebot keeps a nil State; callers skip those. The
// error is a failed read; runs it could not reach also keep a nil State.
func (m *Mod) readyRuns(ctx context.Context, runs []*botRun) error {
	var stale []*botRun
	for _, b := range runs {
		if !b.rowFresh && b.State == nil {
			stale = append(stale, b)
		}
	}
	if len(stale) > 0 {
		ids := make([]int64, len(stale))
		for i, b := range stale {
			ids[i] = b.CharID
		}
		fresh := make(map[int64]*botRun, len(stale))
		err := forEachChunk(ids, func(where string, args []any) error {
			got, err := m.loadRosterWhere(ctx, " AND c.characterID IN "+where, args)
			for _, f := range got {
				fresh[f.CharID] = f
			}
			return err
		})
		if err != nil {
			return err
		}
		var found []*botRun
		for _, b := range stale {
			f, ok := fresh[b.CharID]
			if !ok {
				// Disabled or gone since the copy: rowFresh stays false and nothing is loaded.
				continue
			}
			b.Name, b.CorpID = f.Name, f.CorpID
			b.SystemID, b.StationID, b.shipCand = f.SystemID, f.StationID, f.shipCand
			b.GangID, b.BossID = f.GangID, f.BossID
			b.SecStatus, b.FactionID = f.SecStatus, f.FactionID
			b.loadedSeq, b.rowFresh = f.loadedSeq, true
			found = append(found, b)
		}
		if err := m.hydrateMissing(ctx, found); err != nil {
			return err
		}
	}
	// Runs already fresh (the copy was refreshed this pass) but not yet loaded.
	var rest []*botRun
	for _, b := range runs {
		if b.rowFresh && b.State == nil {
			rest = append(rest, b)
		}
	}
	return m.hydrateMissing(ctx, rest)
}

// loadedOnly is runs without the ones readyRuns could not load.
func loadedOnly(runs []*botRun) []*botRun {
	out := runs[:0:0]
	for _, b := range runs {
		if b.State != nil {
			out = append(out, b)
		}
	}
	return out
}
