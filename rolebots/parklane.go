package rolebots

// Park lanes: where the abstract tier leaves a ship standing.
//
// The abstract tier writes one coordinate to the ship's entity row when a bot
// jumps, picks a belt or pulls up to dock (parkAtGate, parkAtCelestial,
// parkAtBelt), and the embodied tier later materialises the projection there
// (embarkPosition). Each point is a pure function of the object, so every bot
// parking at one gate gets the identical point. A ShipSector ball is a free
// ball that destiny.dll overlap-resolves each tick, and two on one coordinate
// have a degenerate separation vector that the client throws apart at thousands
// of m/s. So parks use a lane, as undocks, gate landings and warp landings do.

import (
	"context"
	"math"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

const (
	// How long a park slot is held. A parked ship can stand at a gate or on a belt
	// until the roster pass returns to it, so this is generous: a slot released
	// under a ship still in it hands the same coordinate to the next arrival.
	parkSlotHoldSec = 30 * 60

	// How close two parked ships must be to count as one pile. A metre, not exact
	// float equality, since near-identical points are as degenerate as identical
	// ones; the lane steps hundreds of metres, so nothing separated is merged.
	parkPileToleranceM = 1.0

	// Deterministic per-ship scatter on top of the lane slot, so ships that share a
	// slot are never identical. It must stay small relative to the lane (25 m).
	// See docs/comment-notes-b14.md#park-jitter-size.
	parkJitterM = 25.0
)

// parkLaneOffset is the offset to add to a computed park point so that no two
// bot ships parked at one object share a coordinate. dir is the axis the point
// was offset along; zero falls back to jitter alone. Slots come from
// claimUndockSlot (undockwave.go), keyed by itemID with the ship as holder.
func parkLaneOffset(objectID, shipID int64, dir destiny.Vec3) destiny.Vec3 {
	off := parkJitter(shipID)
	if objectID == 0 || dir.IsZero() {
		return off
	}
	slot := claimUndockSlot(objectID, shipID, parkSlotHoldSec)
	return destiny.UndockLaneOffset(dir, slot).Add(off)
}

// parkJitter is a fixed-length displacement derived from the ship's own id. It
// is deterministic because the abstract and embodied tiers resolve a parked
// ship's position independently, and a random nudge would desync them.
func parkJitter(shipID int64) destiny.Vec3 {
	h := uint64(shipID)*0x9E3779B97F4A7C15 + 0x632BE59BD9B4E019
	h ^= h >> 29
	h *= 0xBF58476D1CE4E5B9
	h ^= h >> 32
	theta := float64(h%65536) / 65536 * 2 * math.Pi
	z := float64((h>>16)%65536)/65536*2 - 1
	r := math.Sqrt(1 - z*z)
	return destiny.Vec3{
		X: r * math.Cos(theta) * parkJitterM,
		Y: r * math.Sin(theta) * parkJitterM,
		Z: z * parkJitterM,
	}
}

// ── repair for ships parked on one coordinate ───────────────────────────────

// destackParkedShips spreads bot ships parked on top of one another. The lane
// governs only new parks and a parked ship has no clock of its own, so existing
// stacks stay put. Runs at boot beside the crash sweep, ungated on the master
// switch. It re-parks duplicates only, so a second run finds nothing.
func (m *Mod) destackParkedShips(ctx context.Context) {
	type parked struct {
		shipID  int64
		sys     int64
		x, y, z float64
	}

	// Read the whole worklist and close the cursor before writing: an UPDATE
	// mid-stream takes another pooled connection and waits on rows this one holds.
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, c.solarSystemID, e.x, e.y, e.z
		  FROM rolebot_characters b
		  JOIN character_ c   ON c.characterID = b.characterID
		  JOIN entity      ce ON ce.itemID = c.characterID
		  JOIN entity      e  ON e.itemID = ce.locationID
		 WHERE (c.stationID IS NULL OR c.stationID = 0)
		   AND e.x IS NOT NULL AND e.y IS NOT NULL AND e.z IS NOT NULL
		 ORDER BY c.solarSystemID, e.x, e.y, e.z, e.itemID`)
	if err != nil {
		rlogf("rolebots: destack parked ships: %v", err)
		return
	}
	var list []parked
	for rows.Next() {
		var p parked
		if err := rows.Scan(&p.shipID, &p.sys, &p.x, &p.y, &p.z); err != nil {
			rows.Close()
			rlogf("rolebots: destack parked ships: %v", err)
			return
		}
		list = append(list, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		rlogf("rolebots: destack parked ships: %v", err)
		return
	}

	// The ordering groups every pile into consecutive rows. Each is compared with
	// the run's first row, not its predecessor, so a shallow drift of near-identical
	// points cannot chain a spread-out line of ships into one pile.
	var moved, piles int
	for i := 0; i < len(list); {
		j := i + 1
		for j < len(list) && list[j].sys == list[i].sys &&
			samePile(list[j].x, list[j].y, list[j].z, list[i].x, list[i].y, list[i].z) {
			j++
		}
		if j-i > 1 {
			piles++
			// Toward the star, the side every park standoff is on. The gate lane's true axis
			// is not recoverable from a coordinate, but any consistent spread ends the degeneracy.
			dir := destiny.Vec3{X: -list[i].x, Y: -list[i].y, Z: -list[i].z}.Normalize()
			for k := i + 1; k < j; k++ {
				p := list[k]
				off := parkJitter(p.shipID)
				if !dir.IsZero() {
					off = destiny.UndockLaneOffset(dir, k-i).Add(off)
				}
				if err := evedb.UpdateEntityPosition(ctx, m.db, p.shipID,
					p.x+off.X, p.y+off.Y, p.z+off.Z); err != nil {
					rlogf("rolebots: destack ship %d: %v", p.shipID, err)
					continue
				}
				moved++
			}
		}
		i = j
	}
	if moved > 0 {
		rlogf("rolebots: destacked %d parked ship(s) off %d shared coordinate(s)", moved, piles)
	}
}

// How often the hull sweep runs once the server is up: twice the undock burn,
// so a ship forgotten by a slow roster pass spends seconds in the model, not
// minutes, while the sweep does not compete with the burn.
const stationHullSweepTicks = 20 // tick.Manager ticks are one second

// startStationHullSweep keeps resettleStationHullShips running after boot. It
// has its own overlap guard, like the grid lane's: a pass that ran long must
// not have a second one pile in behind it.
func (m *Mod) startStationHullSweep() {
	m.registerPeriodic("rolebots-hullsweep", stationHullSweepTicks, func(uint64) bool {
		if !m.hullSweepBusy.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer m.hullSweepBusy.Store(false)
			m.resettleStationHullShips(context.Background())
		}()
		return true
	})
}

// samePile reports whether two parked ships are close enough to count as one
// pile (see parkPileToleranceM).
func samePile(ax, ay, az, bx, by, bz float64) bool {
	dx, dy, dz := ax-bx, ay-by, az-bz
	return dx*dx+dy*dy+dz*dz <= parkPileToleranceM*parkPileToleranceM
}

// ── repair for ships parked inside station hulls ────────────────────────────

// stationHull is one station's static geometry: where it is, which way its door faces and how
// far its model reaches.
type stationHull struct {
	stationID int64
	pos       destiny.Vec3
	orient    destiny.Vec3
	radius    float64
}

var (
	stationHullsMu sync.Mutex
	stationHullsBy map[int64][]stationHull // solarSystemID -> stations, stationID ascending
)

// stationHulls is every station with a model radius, by solar system, read once: staStations,
// staStationTypes and mapDenormalize never change while the server runs. A failed read is not
// kept, so the next call tries again. The map is shared and read-only.
func (m *Mod) stationHulls(ctx context.Context) (map[int64][]stationHull, error) {
	stationHullsMu.Lock()
	defer stationHullsMu.Unlock()
	if stationHullsBy != nil {
		return stationHullsBy, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.solarSystemID, s.stationID, s.x, s.y, s.z,
		       t.dockOrientationX, t.dockOrientationY, t.dockOrientationZ, d.radius
		  FROM staStations s
		  JOIN staStationTypes t ON t.stationTypeID = s.stationTypeID
		  JOIN mapDenormalize  d ON d.itemID = s.stationID
		 WHERE d.radius > 0
		 ORDER BY s.solarSystemID, s.stationID`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[int64][]stationHull{}
	for rows.Next() {
		var sysID int64
		var h stationHull
		if err := rows.Scan(&sysID, &h.stationID, &h.pos.X, &h.pos.Y, &h.pos.Z,
			&h.orient.X, &h.orient.Y, &h.orient.Z, &h.radius); err != nil {
			return nil, err
		}
		by[sysID] = append(by[sysID], h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	stationHullsBy = by
	return by, nil
}

// undockRunsLive reports, for the given pilots, which still have an undock burn running (their
// state's undockRunUntil is in the future). A pilot with no state row has none.
func (m *Mod) undockRunsLive(ctx context.Context, charIDs []int64) (map[int64]bool, error) {
	now := clock.Unix()
	live := map[int64]bool{}
	err := forEachChunk(charIDs, func(where string, args []any) error {
		rows, err := m.db.QueryContext(ctx, `
			SELECT characterID, COALESCE(JSON_EXTRACT(data, '$.undockRunUntil'), 0) + 0
			  FROM rolebot_state WHERE characterID IN `+where, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var until float64
			if err := rows.Scan(&id, &until); err != nil {
				return err
			}
			if until > float64(now) {
				live[id] = true
			}
		}
		return rows.Err()
	})
	return live, err
}

// resettleStationHullShips moves bot ships parked inside a station's model out
// to the door standoff. Runs at boot and on a timer, ungated on the master
// switch, and is idempotent.
// See docs/comment-notes-b14.md#station-hull-sweep.
func (m *Mod) resettleStationHullShips(ctx context.Context) {
	type stranded struct {
		shipID, charID, stationID int64
		station, orient           destiny.Vec3
		radius                    float64
	}

	// Read the whole worklist and close the cursor before writing, as in destackParkedShips.
	// Only the ships are read from the database: the stations are static (stationHullsIn), and
	// the inside-the-hull test runs here with the same arithmetic the query used (squared
	// distance against the squared radius), so the database no longer pairs every ship in
	// space with every station in its system every sweep.
	stationsBySys, err := m.stationHulls(ctx)
	if err != nil {
		rlogf("rolebots: resettle station hull ships: %v", err)
		return
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, b.characterID, c.solarSystemID, e.x, e.y, e.z
		  FROM rolebot_characters b
		  JOIN character_        c  ON c.characterID = b.characterID
		  JOIN entity            ce ON ce.itemID = c.characterID
		  JOIN entity            e  ON e.itemID = ce.locationID
		 WHERE (c.stationID IS NULL OR c.stationID = 0)
		   AND e.x IS NOT NULL AND e.y IS NOT NULL AND e.z IS NOT NULL
		 ORDER BY e.itemID`)
	if err != nil {
		rlogf("rolebots: resettle station hull ships: %v", err)
		return
	}
	var list []stranded
	for rows.Next() {
		var shipID, charID, sysID int64
		var x, y, z float64
		if err := rows.Scan(&shipID, &charID, &sysID, &x, &y, &z); err != nil {
			rows.Close()
			rlogf("rolebots: resettle station hull ships: %v", err)
			return
		}
		// Station radii reach 56 km, so two stations can both claim a ship. The first
		// answer will do: it must end up outside one and be moved once.
		for _, s := range stationsBySys[sysID] {
			dx, dy, dz := x-s.pos.X, y-s.pos.Y, z-s.pos.Z
			if dx*dx+dy*dy+dz*dz < s.radius*s.radius {
				list = append(list, stranded{shipID: shipID, charID: charID, stationID: s.stationID,
					station: s.pos, orient: s.orient, radius: s.radius})
				break
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		rlogf("rolebots: resettle station hull ships: %v", err)
		return
	}
	// A ship whose launch burn is still live is left alone: it is meant to be in the bay. Read
	// for the handful inside a hull only, not for every ship in space.
	if len(list) > 0 {
		ids := make([]int64, len(list))
		for i, p := range list {
			ids[i] = p.charID
		}
		live, err := m.undockRunsLive(ctx, ids)
		if err != nil {
			rlogf("rolebots: resettle station hull ships: %v", err)
			return
		}
		kept := list[:0]
		for _, p := range list {
			if !live[p.charID] {
				kept = append(kept, p)
			}
		}
		list = kept
	}

	var moved int
	for _, p := range list {
		// The standoff lane, not the station's own: these ships are stranded in
		// a hull, not undocking. Same reason as parkAtStationDoor.
		lane := destiny.StandoffLaneObject(p.stationID)
		slot := claimUndockSlot(lane, p.shipID, parkSlotHoldSec)
		pos, ok := destiny.StationStandoffPoint(p.station, p.orient, p.radius, parkStandoff, slot)
		if !ok {
			releaseUndockSlot(lane, p.shipID)
			continue
		}
		pos = pos.Add(parkJitter(p.shipID))
		if err := evedb.UpdateEntityPosition(ctx, m.db, p.shipID, pos.X, pos.Y, pos.Z); err != nil {
			rlogf("rolebots: resettle ship %d: %v", p.shipID, err)
			continue
		}
		moved++
	}
	if moved > 0 {
		rlogf("rolebots: moved %d ship(s) out of the station hull they were parked in", moved)
	}
}
