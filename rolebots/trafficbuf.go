package rolebots

// Write-behind for the traffic ledgers (traffic.go, stationscore.go) and the star map's jump
// counter (mapHistory statType 4).
//
// Every gate a bot jumps used to cost three autocommitted upserts (mapHistory, the system row
// and the lane row), every dock one more and every trade leg one or two. With tens of thousands
// of pilots that was hundreds of single-row commits a second, each its own redo write and fsync,
// for counters that are only ever read as decayed sums. The deltas are additive, so they fold
// into maps here and a tick writes each map as a few multi-row upserts in one transaction.
//
// Like the stat counters (statcount.go) an unclean exit loses at most one flush interval; the
// ledgers are measurements, not a source of truth.

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
)

const (
	// trafficFlushEveryTicks is the flush period, 30 seconds on the 1 s tick manager. Against
	// the ledgers' two-day half-life the delay is invisible.
	trafficFlushEveryTicks = 30

	// trafficPendingMax caps each pending map, so a wedged flusher costs counts rather than
	// memory. Systems and stations are bounded by the map; the cap only matters for lanes.
	trafficPendingMax = 50000

	// trafficUpsertChunk is how many rows go in one INSERT.
	trafficUpsertChunk = 500
)

type trafficSysDelta struct {
	jumps, buys, sells float64
	isk                float64
	lastAt             int64
}

type trafficLaneDelta struct {
	jumps, hauls float64
	isk          float64
	lastAt       int64
}

type trafficDockDelta struct {
	docks  float64
	lastAt int64
}

// mapJumpKey is one mapHistory cell: the system and the hour the jumps landed in.
type mapJumpKey struct {
	system int64
	bucket int64
}

type trafficPending struct {
	systems  map[int64]*trafficSysDelta
	lanes    map[[2]int64]*trafficLaneDelta
	docks    map[int64]*trafficDockDelta
	mapJumps map[mapJumpKey]int
}

func newTrafficPending() *trafficPending {
	return &trafficPending{
		systems:  map[int64]*trafficSysDelta{},
		lanes:    map[[2]int64]*trafficLaneDelta{},
		docks:    map[int64]*trafficDockDelta{},
		mapJumps: map[mapJumpKey]int{},
	}
}

func (p *trafficPending) empty() bool {
	return len(p.systems) == 0 && len(p.lanes) == 0 && len(p.docks) == 0 && len(p.mapJumps) == 0
}

var (
	trafficMu  sync.Mutex
	trafficBuf = newTrafficPending()
)

// The fold helpers take trafficMu themselves. A key beyond the cap is dropped: the ledger is a
// measurement, and a flusher that cannot keep up must not grow without bound.

func bufSystem(systemID int64, now int64, f func(*trafficSysDelta)) {
	trafficMu.Lock()
	defer trafficMu.Unlock()
	d, ok := trafficBuf.systems[systemID]
	if !ok {
		if len(trafficBuf.systems) >= trafficPendingMax {
			return
		}
		d = &trafficSysDelta{}
		trafficBuf.systems[systemID] = d
	}
	f(d)
	d.lastAt = max(d.lastAt, now)
}

func bufLane(lo, hi int64, now int64, f func(*trafficLaneDelta)) {
	trafficMu.Lock()
	defer trafficMu.Unlock()
	k := [2]int64{lo, hi}
	d, ok := trafficBuf.lanes[k]
	if !ok {
		if len(trafficBuf.lanes) >= trafficPendingMax {
			return
		}
		d = &trafficLaneDelta{}
		trafficBuf.lanes[k] = d
	}
	f(d)
	d.lastAt = max(d.lastAt, now)
}

func bufDock(stationID int64, now int64) {
	trafficMu.Lock()
	defer trafficMu.Unlock()
	d, ok := trafficBuf.docks[stationID]
	if !ok {
		if len(trafficBuf.docks) >= trafficPendingMax {
			return
		}
		d = &trafficDockDelta{}
		trafficBuf.docks[stationID] = d
	}
	d.docks++
	d.lastAt = max(d.lastAt, now)
}

// bufMapJump counts one arrival for the star map's jump overlay, in the hour it happened
// (the bucket evedb.RecordMapEvent would have used).
func bufMapJump(systemID int64) {
	k := mapJumpKey{system: systemID, bucket: time.Now().Unix() / 3600}
	trafficMu.Lock()
	defer trafficMu.Unlock()
	if _, ok := trafficBuf.mapJumps[k]; !ok && len(trafficBuf.mapJumps) >= trafficPendingMax {
		return
	}
	trafficBuf.mapJumps[k]++
}

// takeTraffic swaps the pending deltas out, so pilots keep folding into a fresh set while a
// slow flush writes the old one.
func takeTraffic() *trafficPending {
	trafficMu.Lock()
	p := trafficBuf
	trafficBuf = newTrafficPending()
	trafficMu.Unlock()
	return p
}

var trafficFlushBusy = make(chan struct{}, 1)

// startTrafficWriter registers the flush tick. Not gated on Master(): counts gathered before
// the switch went off are real.
func (m *Mod) startTrafficWriter() {
	m.registerPeriodic("rolebot-traffic-flush", trafficFlushEveryTicks, func(uint64) bool {
		select {
		case trafficFlushBusy <- struct{}{}:
		default:
			return true
		}
		goSafe("traffic flush", func() {
			defer func() { <-trafficFlushBusy }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			m.flushTraffic(ctx)
		})
		return true
	})
}

// flushTraffic writes everything pending in one transaction.
func (m *Mod) flushTraffic(ctx context.Context) {
	p := takeTraffic()
	if p.empty() {
		return
	}
	if err := writeTraffic(ctx, m.db, p); err != nil {
		rlogf("rolebots: traffic flush (%d systems, %d lanes, %d docks, %d map cells): %v",
			len(p.systems), len(p.lanes), len(p.docks), len(p.mapJumps), err)
	}
}

func writeTraffic(ctx context.Context, db *sql.DB, p *trafficPending) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := clock.Unix()

	// New rows start decayed as of now, as the single-row upserts did; an existing row keeps
	// its decayedAt and the hub sweep ages it.
	sysRows := make([][]any, 0, len(p.systems))
	for id, d := range p.systems {
		sysRows = append(sysRows, []any{id, d.jumps, d.buys, d.sells, d.isk, now, d.lastAt})
	}
	if err := upsertChunks(ctx, tx,
		`INSERT INTO rolebot_traffic_systems (systemID, jumps, buys, sells, iskMoved, decayedAt, lastAt) VALUES `,
		`(?,?,?,?,?,?,?)`,
		` ON DUPLICATE KEY UPDATE jumps = jumps + VALUES(jumps), buys = buys + VALUES(buys),
			sells = sells + VALUES(sells), iskMoved = iskMoved + VALUES(iskMoved),
			lastAt = GREATEST(lastAt, VALUES(lastAt))`, sysRows); err != nil {
		return err
	}

	laneRows := make([][]any, 0, len(p.lanes))
	for k, d := range p.lanes {
		laneRows = append(laneRows, []any{k[0], k[1], d.jumps, d.hauls, d.isk, now, d.lastAt})
	}
	if err := upsertChunks(ctx, tx,
		`INSERT INTO rolebot_traffic_lanes (fromSystemID, toSystemID, jumps, hauls, iskMoved, decayedAt, lastAt) VALUES `,
		`(?,?,?,?,?,?,?)`,
		` ON DUPLICATE KEY UPDATE jumps = jumps + VALUES(jumps), hauls = hauls + VALUES(hauls),
			iskMoved = iskMoved + VALUES(iskMoved), lastAt = GREATEST(lastAt, VALUES(lastAt))`,
		laneRows); err != nil {
		return err
	}

	dockRows := make([][]any, 0, len(p.docks))
	for id, d := range p.docks {
		dockRows = append(dockRows, []any{id, d.docks, now, d.lastAt})
	}
	if err := upsertChunks(ctx, tx,
		`INSERT INTO rolebot_traffic_stations (stationID, docks, decayedAt, lastAt) VALUES `,
		`(?,?,?,?)`,
		` ON DUPLICATE KEY UPDATE docks = docks + VALUES(docks), lastAt = GREATEST(lastAt, VALUES(lastAt))`,
		dockRows); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// mapHistory is the core's table; its own helper creates it on first use.
	if len(p.mapJumps) > 0 {
		evs := make([]evedb.MapEventCount, 0, len(p.mapJumps))
		for k, n := range p.mapJumps {
			evs = append(evs, evedb.MapEventCount{
				SolarSystemID: k.system, StatType: evedb.StatJump, HourBucket: k.bucket, Count: n})
		}
		return evedb.RecordMapEvents(ctx, db, evs)
	}
	return nil
}

// upsertChunks writes rows as multi-row INSERTs of at most trafficUpsertChunk rows each.
func upsertChunks(ctx context.Context, tx *sql.Tx, head, tuple, tail string, rows [][]any) error {
	for start := 0; start < len(rows); start += trafficUpsertChunk {
		end := min(start+trafficUpsertChunk, len(rows))
		var b strings.Builder
		b.WriteString(head)
		args := make([]any, 0, (end-start)*len(rows[start]))
		for i, r := range rows[start:end] {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(tuple)
			args = append(args, r...)
		}
		b.WriteString(tail)
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}
