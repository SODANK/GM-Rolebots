package rolebots

import (
	"context"
	"math"

	"gurimoon/destiny"
)

// Where a ship may be put down without overlapping another hull. The client overlap-resolves
// free balls and throws two overlapping ships apart, which a watching player sees as a ship
// that arrives and is flung off into the distance. Every writer of a projection's coordinate
// asks destiny.OccupancySnapshot first and moves off anything there (birthClear).

const (
	// birthScanRange is how far around a birth point the occupancy snapshot looks; well past
	// the furthest ring birthClear tries.
	birthScanRange = 10_000.0
	// undockScanRange covers a station's whole undock run (destiny.UndockRunM) and its lane.
	undockScanRange = 60_000.0
	// birthRingMin is the smallest ring step birthClear uses, for small hulls.
	birthRingMin = 250.0
	birthRings   = 4
	birthBearing = 8
)

// birthClear returns p if a hull of radius standing there overlaps none of occ, else the
// first point on a fan of rings around it that does: rings k*step (step from the hull's own
// size), birthBearing bearings each, built round axis. lateralOnly keeps every candidate in
// the plane square to axis, so a launch nudged off a blocked point stays off its own run line.
// It returns the point, the offset applied, and whether a clear point was found (p unchanged
// and ok=false when none was).
func birthClear(p, axis destiny.Vec3, radius float64, occ []destiny.BallFootprint, lateralOnly bool) (destiny.Vec3, destiny.Vec3, bool) {
	if destiny.PointClear(p, radius, occ) {
		return p, destiny.Vec3{}, true
	}
	if axis.IsZero() {
		axis = destiny.Vec3{X: 1}
	}
	n := axis.Normalize()
	u, v := destiny.PerpendicularBasis(n)
	step := math.Max(2*radius+destiny.ArrivalClearMargin, birthRingMin)
	for k := 1; k <= birthRings; k++ {
		r := float64(k) * step
		for i := 0; i < birthBearing; i++ {
			a := 2 * math.Pi * float64(i) / birthBearing
			off := u.Scale(r * math.Cos(a)).Add(v.Scale(r * math.Sin(a)))
			if c := p.Add(off); destiny.PointClear(c, radius, occ) {
				return c, off, true
			}
		}
		if !lateralOnly {
			for _, s := range []float64{1, -1} {
				off := n.Scale(s * r)
				if c := p.Add(off); destiny.PointClear(c, radius, occ) {
					return c, off, true
				}
			}
		}
	}
	return p, destiny.Vec3{}, false
}

// segmentClear reports whether a hull of radius swept from a to b passes clear of every
// footprint in occ, by the same rule as destiny.PointClear.
func segmentClear(a, b destiny.Vec3, radius float64, occ []destiny.BallFootprint) bool {
	ab := b.Sub(a)
	lenSq := ab.LenSq()
	for _, o := range occ {
		t := 0.0
		if lenSq > 0 {
			t = math.Max(0, math.Min(1, o.Pos.Sub(a).Dot(ab)/lenSq))
		}
		closest := a.Add(ab.Scale(t))
		if !destiny.PointClear(closest, radius, []destiny.BallFootprint{o}) {
			return false
		}
	}
	return true
}

// stackLevel is the log level for a projection born d metres from a hull that needs sep:
// error on the same coordinate, warn for a real overlap the client will throw apart, info for
// a graze. ok=false when the two are clear.
func stackLevel(d, sep float64) (string, bool) {
	switch {
	case d > sep:
		return "", false
	case d <= 1:
		return logError, true
	case d < sep*0.5:
		return logWarn, true
	default:
		return logInfo, true
	}
}

// hullRadiusFor is a ship's hull radius: the live projection's if it has one, else the type's
// from the database. 150 m (GetShipStats' default) when neither answers.
func (m *Mod) hullRadiusFor(ctx context.Context, charID, shipID int64) float64 {
	if eb := liveProjection(charID); eb != nil && eb.ent != nil && eb.ent.Manager != nil && eb.shipID == shipID {
		if r := eb.ent.Manager.Radius(); r > 0 {
			return r
		}
	}
	var r float64
	err := m.db.QueryRowContext(ctx, `
		SELECT t.radius FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.itemID = ?`, shipID).Scan(&r)
	if err != nil || r <= 0 {
		return 150
	}
	return r
}
