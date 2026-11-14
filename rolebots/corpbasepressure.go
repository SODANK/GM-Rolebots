package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	evedb "gurimoon/db"
)

// Base pressure: a department noticing that its base is full, from two inputs (refusals and
// measured waits). See docs/comment-notes-b03.md#base-pressure.

const (
	// basePressureWindowSec is how far back the score looks.
	basePressureWindowSec = 72 * 3600
	// basePressureKeepSec is how long a refusal is kept at all: long enough for the portal to
	// show a week of it.
	basePressureKeepSec = 7 * 86400
	// basePressureMinRefusals and basePressureMinPilots: a base is full on this many refusals from
	// at least this many members, or on basePressureAnyRefusals from anyone.
	basePressureMinRefusals = 4
	basePressureMinPilots   = 2
	basePressureAnyRefusals = 8
	// basePressureNoteSec is how often one member's refusal at one base is counted: a pilot
	// re-planning every minute against one queue is one refusal.
	basePressureNoteSec = 3600
	// basePressureMinWaitJobs is how many of the corporation's own jobs of an activity must have
	// been installed at the base in the window before their average wait says anything.
	basePressureMinWaitJobs = 6
	// basePressureWaitCapSec: an average wait past this fills the base whatever the jobs' length.
	basePressureWaitCapSec = 4 * 3600
	// basePressureWaitFloorSec: an average wait under this never fills the base, however short the
	// jobs it waited behind.
	basePressureWaitFloorSec = 3600
)

// Pressure kinds.
const (
	pressureLab = "lab"
	pressureMfg = "mfg"
)

// pressureKindFor names the kind of line an activity runs on.
func pressureKindFor(activityID int64) string {
	if activityID == ramActivityManufacturing {
		return pressureMfg
	}
	return pressureLab
}

// lineQueuedOut is assemblyLineAt's refusal when every line of the kind at a station is queued
// past industryQueueCapSec. It is transient but is also the base-pressure signal, so it has its
// own type.
type lineQueuedOut struct {
	StationID  int64
	ActivityID int64
}

func (e lineQueuedOut) Error() string { return "every line here is queued more than a day out" }

// asLineQueuedOut reports whether err is a queued-out refusal.
func asLineQueuedOut(err error) (lineQueuedOut, bool) {
	var q lineQueuedOut
	ok := errors.As(err, &q)
	return q, ok
}

// pressureRow is one recorded refusal.
type pressureRow struct {
	CharID int64
	At     int64
}

// pressureWait is the measured queue wait of one activity at one base: the corporation's own
// jobs installed there in the window, their average wait for a line, and their run time.
type pressureWait struct {
	ActivityID int64
	Jobs       int
	AvgWaitSec float64
	AvgRunSec  float64
}

// ratio is how far the wait is toward filling the base: the larger of its share of the cap and
// its share of the run time. Under the floor it is only its share of the floor.
func (w pressureWait) ratio() float64 {
	if w.Jobs == 0 || w.AvgWaitSec <= 0 {
		return 0
	}
	if w.AvgWaitSec < basePressureWaitFloorSec {
		return min(w.AvgWaitSec/basePressureWaitFloorSec, 0.5)
	}
	r := w.AvgWaitSec / basePressureWaitCapSec
	if w.AvgRunSec > 0 {
		r = max(r, w.AvgWaitSec/w.AvgRunSec)
	}
	return r
}

// full reports whether this activity's wait alone fills the base.
func (w pressureWait) full() bool {
	return w.Jobs >= basePressureMinWaitJobs && w.AvgWaitSec >= basePressureWaitFloorSec &&
		(w.AvgWaitSec > basePressureWaitCapSec || w.AvgWaitSec > w.AvgRunSec)
}

// basePressure is a base's score. Refusals and Pilots count the window's refusals; Wait is the
// worst-waiting activity of the kind. Full is the verdict (ByRefusal and ByWait say which input
// gave it) and Score is 0..1 for the portal's bar.
type basePressure struct {
	Refusals  int
	Pilots    int
	Wait      pressureWait
	Full      bool
	ByRefusal bool
	ByWait    bool
	Score     float64
	LastAt    int64
}

// worstWait picks the activity a kind is scored on: one that fills the base over one that does
// not, then the longest wait relative to its cap and its work. Pure.
func worstWait(waits []pressureWait) pressureWait {
	var best pressureWait
	have := false
	for _, w := range waits {
		if w.Jobs == 0 {
			continue
		}
		if !have || (w.full() && !best.full()) || (w.full() == best.full() && w.ratio() > best.ratio()) {
			best, have = w, true
		}
	}
	return best
}

// scoreBasePressure scores one base from its refusals and the measured waits of the kind's
// activities there. Pure.
func scoreBasePressure(rows []pressureRow, waits []pressureWait, now int64) basePressure {
	p := scoreRefusals(rows, now)
	p.ByRefusal = p.Full
	p.Wait = worstWait(waits)
	p.ByWait = p.Wait.full()
	p.Full = p.ByRefusal || p.ByWait
	p.Score = max(p.Score, min(1, p.Wait.ratio()))
	if p.Full {
		p.Score = 1
	} else {
		p.Score = min(p.Score, 0.9)
	}
	return p
}

// scoreRefusals scores one base's refusals alone. Pure.
func scoreRefusals(rows []pressureRow, now int64) basePressure {
	var p basePressure
	pilots := map[int64]bool{}
	for _, r := range rows {
		if now-r.At > basePressureWindowSec {
			continue
		}
		p.Refusals++
		pilots[r.CharID] = true
		p.LastAt = max(p.LastAt, r.At)
	}
	p.Pilots = len(pilots)
	p.Full = (p.Refusals >= basePressureMinRefusals && p.Pilots >= basePressureMinPilots) ||
		p.Refusals >= basePressureAnyRefusals
	p.Score = min(1, float64(p.Refusals)/float64(basePressureMinRefusals))
	if p.Pilots < basePressureMinPilots && p.Refusals < basePressureAnyRefusals {
		p.Score = min(p.Score, 0.9)
	}
	if p.Full {
		p.Score = 1
	}
	return p
}

// pressureNoteAt throttles noteBasePressure per (corp, station, kind, pilot).
var pressureNoteAt sync.Map

func init() {
	registerSweep("pressureNoteAtSweep", func() {
		now := clock.Unix()
		pressureNoteAt.Range(func(k, v any) bool {
			if now-v.(int64) >= 2*basePressureNoteSec {
				pressureNoteAt.Delete(k)
			}
			return true
		})
	})
	registerCorpForget("pressureNoteAt", func(id int64) {
		pressureNoteAt.Range(func(k, _ any) bool {
			if k.(pressureKey).corpID == id {
				pressureNoteAt.Delete(k)
			}
			return true
		})
	})
}

type pressureKey struct {
	corpID, stationID, charID int64
	kind                      string
}

// noteBasePressure records one member refused at a base for want of a line. It never fails
// its caller.
func (m *Mod) noteBasePressure(ctx context.Context, corpID, stationID int64, kind string, charID int64) {
	if corpID == 0 || stationID == 0 {
		return
	}
	now := clock.Unix()
	k := pressureKey{corpID, stationID, charID, kind}
	if v, ok := pressureNoteAt.Load(k); ok && now-v.(int64) < basePressureNoteSec {
		return
	}
	pressureNoteAt.Store(k, now)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_base_pressure (corporationID, stationID, kind, characterID, at)
		VALUES (?, ?, ?, ?, ?)`, corpID, stationID, kind, charID, now); err != nil {
		rlogf("rolebots: base pressure %d/%d: %v", corpID, stationID, err)
	}
}

// noteQueuedOut records err as base pressure when it is a queued-out refusal.
func (m *Mod) noteQueuedOut(ctx context.Context, corpID, charID int64, err error) {
	if q, ok := asLineQueuedOut(err); ok {
		m.noteBasePressure(ctx, corpID, q.StationID, pressureKindFor(q.ActivityID), charID)
	}
}

// prunePressure drops refusals past keeping (the estate think calls it).
func (m *Mod) prunePressure(ctx context.Context, corpID int64) {
	_, _ = m.db.ExecContext(ctx, `
		DELETE FROM rolebot_corp_base_pressure WHERE corporationID = ? AND at < ?`, corpID, clock.Unix()-basePressureKeepSec)
}

// pressureActivities is the activities a kind's lines run.
func pressureActivities(kind string) []int64 {
	if kind == pressureMfg {
		return []int64{ramActivityManufacturing}
	}
	return []int64{ramActivityPEResearch, ramActivityMEResearch, ramActivityCopying}
}

// basePressures scores every base of the corporation for one kind, by station: the refusals
// recorded there and the waits the corporation's own jobs suffered there in the window.
// Read-only.
func (m *Mod) basePressures(ctx context.Context, corpID int64, kind string) (map[int64]basePressure, error) {
	now := clock.Unix()
	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, characterID, at FROM rolebot_corp_base_pressure
		 WHERE corporationID = ? AND kind = ? AND at >= ?`, corpID, kind, now-basePressureWindowSec)
	if err != nil {
		return nil, fmt.Errorf("basePressures: %w", err)
	}
	by := map[int64][]pressureRow{}
	for rows.Next() {
		var st int64
		var r pressureRow
		if rows.Scan(&st, &r.CharID, &r.At) == nil {
			by[st] = append(by[st], r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("basePressures: %w", err)
	}
	waits, err := m.baseWaits(ctx, corpID, kind, now)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]basePressure, len(by)+len(waits))
	for st := range by {
		out[st] = scoreBasePressure(by[st], waits[st], now)
	}
	for st := range waits {
		if _, done := out[st]; !done {
			out[st] = scoreBasePressure(nil, waits[st], now)
		}
	}
	return out, nil
}

// baseWaits measures, by station, the corporation's own jobs of the kind's activities
// installed in the window: how many, and their average wait and run. Aborted jobs are left out;
// a still-queued job counts, since its wait is stamped at install. Read-only.
func (m *Mod) baseWaits(ctx context.Context, corpID int64, kind string, now int64) (map[int64][]pressureWait, error) {
	acts := pressureActivities(kind)
	args := []any{corpID}
	marks := make([]string, len(acts))
	for i, a := range acts {
		marks[i] = "?"
		args = append(args, a)
	}
	args = append(args, evedb.UnixToWin32(now-basePressureWindowSec))
	rows, err := m.db.QueryContext(ctx, `
		SELECT containerID, activityID, COUNT(*),
		       AVG(GREATEST(beginProductionTime - installTime, 0)),
		       AVG(GREATEST(endProductionTime - beginProductionTime, 0))
		  FROM ramJobs
		 WHERE ownerID = ? AND activityID IN (`+strings.Join(marks, ",")+`)
		   AND installTime >= ? AND completedStatusID <> 102
		 GROUP BY containerID, activityID`, args...)
	if err != nil {
		return nil, fmt.Errorf("baseWaits: %w", err)
	}
	defer rows.Close()
	out := map[int64][]pressureWait{}
	for rows.Next() {
		var st int64
		var w pressureWait
		var wait, run sql.NullFloat64
		if err := rows.Scan(&st, &w.ActivityID, &w.Jobs, &wait, &run); err != nil {
			return nil, fmt.Errorf("baseWaits: %w", err)
		}
		w.AvgWaitSec = wait.Float64 / 10_000_000
		w.AvgRunSec = run.Float64 / 10_000_000
		out[st] = append(out[st], w)
	}
	return out, rows.Err()
}
