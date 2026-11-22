package rolebots

import (
	"math"
	"testing"

	"gurimoon/destiny"
)

// A clear point is kept; a blocked one moves to the nearest clear ring; a launch only ever
// moves square to its run; a fully boxed-in point reports failure and stays put.
func TestBirthClear(t *testing.T) {
	axis := destiny.Vec3{X: 1}
	p := destiny.Vec3{}
	if got, off, ok := birthClear(p, axis, 50, nil, false); !ok || got != p || !off.IsZero() {
		t.Fatalf("clear point = %v off %v ok %v, want unchanged", got, off, ok)
	}

	blocker := []destiny.BallFootprint{{Pos: p, Radius: 100}}
	got, off, ok := birthClear(p, axis, 50, blocker, false)
	if !ok || !destiny.PointClear(got, 50, blocker) {
		t.Fatalf("one blocker: %v ok %v, want a clear point", got, ok)
	}
	step := math.Max(2*50+destiny.ArrivalClearMargin, birthRingMin)
	if math.Abs(off.Len()-step) > 1e-6 {
		t.Fatalf("one blocker moved %.1f m, want the first ring (%.1f m)", off.Len(), step)
	}

	// Block every lateral candidate on ring 1 and the point itself: a launch takes ring 2,
	// still square to its axis.
	occ := []destiny.BallFootprint{{Pos: p, Radius: 100}}
	u, v := destiny.PerpendicularBasis(axis)
	for i := 0; i < birthBearing; i++ {
		a := 2 * math.Pi * float64(i) / birthBearing
		occ = append(occ, destiny.BallFootprint{Pos: u.Scale(step * math.Cos(a)).Add(v.Scale(step * math.Sin(a))), Radius: 10})
	}
	got, off, ok = birthClear(p, axis, 50, occ, true)
	if !ok || math.Abs(off.Dot(axis)) > 1e-6 {
		t.Fatalf("lateral-only: %v off %v ok %v, want a move square to the axis", got, off, ok)
	}

	wall := []destiny.BallFootprint{{Pos: p, Radius: 1e6}}
	if got, _, ok := birthClear(p, axis, 50, wall, false); ok || got != p {
		t.Fatalf("boxed in: %v ok %v, want unchanged and false", got, ok)
	}
}

// A run is blocked by a hull on the line, not by one beside it or past its end.
func TestSegmentClear(t *testing.T) {
	a, b := destiny.Vec3{}, destiny.Vec3{X: 10_000}
	cases := []struct {
		name string
		at   destiny.Vec3
		want bool
	}{
		{"on the line", destiny.Vec3{X: 5_000}, false},
		{"beside the line", destiny.Vec3{X: 5_000, Y: 1_000}, true},
		{"past the end", destiny.Vec3{X: 11_000}, true},
		{"grazing the start", destiny.Vec3{X: -100}, false},
	}
	for _, c := range cases {
		occ := []destiny.BallFootprint{{Pos: c.at, Radius: 100}}
		if got := segmentClear(a, b, 50, occ); got != c.want {
			t.Errorf("%s: segmentClear = %v, want %v", c.name, got, c.want)
		}
	}
}

// Same coordinate is an error, a real overlap a warning, a graze info, clear nothing.
func TestStackLevel(t *testing.T) {
	cases := []struct {
		d    float64
		want string
		ok   bool
	}{
		{0.5, logError, true},
		{30, logWarn, true},
		{300, logInfo, true},
		{600, "", false},
	}
	for _, c := range cases {
		got, ok := stackLevel(c.d, 400)
		if got != c.want || ok != c.ok {
			t.Errorf("stackLevel(%.1f, 400) = %q, %v; want %q, %v", c.d, got, ok, c.want, c.ok)
		}
	}
}
