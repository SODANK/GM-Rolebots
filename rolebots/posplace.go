package rolebots

import (
	"math"
	"math/rand"
)

// Where a tower's structures stand: pure geometry, pinned by the tests. Two zones, the force
// field the line between them:
//   - inside, everything that works (harvesters, silos, arrays, hangars, hardeners), scattered
//     through a shell around the tower (placeInner). The scatter is seeded per tower, spreads
//     above and below its plane, keeps flyGap between hulls, and leaves six flight lanes empty
//     (the compass axes, straight up and down). Hangars a pilot stands beside sit close in.
//   - outside, the batteries (guns and EW): the core refuses one whose hull overlaps the field
//     (db.ForceFieldAnchorRefusal). They take compass slots (gunSlotOffset): N, E, S, W at the
//     field's edge, then stacked beside each point, then a step further out.
// Nothing anchored ever moves: a destroyed piece keeps its spot and is re-anchored there.

const (
	// flyGap is the clear space between two hulls: a battleship's width with
	// room to turn. Structures carry big collision spheres and ships bump.
	flyGap = 3000.0
	// laneHalfWidth is half a flight lane's width, before the piece's own
	// hull is added.
	laneHalfWidth = 2500.0
	// innerWallGap / outerWallGap: tower hull to the nearest structure hull,
	// and a structure hull to the field's edge.
	innerWallGap = 3000.0
	outerWallGap = 2000.0
	// serviceNear / serviceFar: a hangar's band, beyond tower hull + its own.
	serviceNear = 1500.0
	serviceFar  = 5000.0
	// scatterMaxElev is how far above or below the tower's plane a structure
	// may stand.
	scatterMaxElev = 35 * math.Pi / 180
	scatterTries   = 64

	// slotClearance is a battery's centre beyond the field's edge: its hull (up to 629 m) plus a
	// margin. slotLateralStep is the spacing beside a compass point, slotRadialStep the step outward
	// once a point's five lateral places are taken, slotReachMargin what stays inside anchoring reach.
	slotClearance   = 2500.0
	slotLateralStep = 4300.0
	slotRadialStep  = 4300.0
	slotReachMargin = 1500.0
	// slotScanMax bounds the slot search; no tower reaches it.
	slotScanMax = 120
)

// structRadius is each layout structure's hull (invTypes.radius).
var structRadius = map[int64]float64{
	typeHarvester: 1289, typeSilo: 907, typeRefiningArray: 506, typeCHA: 911, typeSMA: 726,
	typeMediumReactor: 952, typeReactor: 952, typeMobileLab: 782, typeAmmoAssembly: 952,
	typeComponentAsm: 952, typeSmallShipAsm: 1857, typeEquipmentAsm: 952,
	typeHardenerEM: 750, typeHardenerThermal: 750, typeHardenerKin: 750, typeHardenerExp: 750,
}

// towerHullRadius is each race's tower hull (the same at every size).
var towerHullRadius = map[int64]float64{
	raceGallente: 3666, raceAmarr: 3394, raceMinmatar: 3346, raceCaldari: 3275,
}

// pieceRadius is a structure's hull, conservative for an unknown type.
func pieceRadius(t int64) float64 {
	if r, ok := structRadius[t]; ok {
		return r
	}
	if gunPower[t] > 0 {
		return 400
	}
	if _, ok := ewBatteryGrid[t]; ok {
		return 629
	}
	return 1900
}

func towerHull(race int64) float64 {
	if r, ok := towerHullRadius[race]; ok {
		return r
	}
	return 3700
}

// fieldOf is a size's force-field radius and anchoring reach.
func fieldOf(size string) (shield, reach float64) {
	shield, ok := towerShield[size]
	if !ok {
		size, shield = towerSmall, towerShield[towerSmall]
	}
	return shield, towerReach[size]
}

// isServicePiece is a structure pilots must stand beside to use.
func isServicePiece(t int64) bool { return t == typeCHA || t == typeSMA }

// compassDirs is north, east, south, west as (x, z): N = +Z, E = +X.
var compassDirs = [4][2]float64{{0, 1}, {1, 0}, {0, -1}, {-1, 0}}

var compassNames = [4]string{"N", "E", "S", "W"}

// slotCompass names a 1-based slot's compass point, "" for none.
func slotCompass(slot int) string {
	if slot < 1 {
		return ""
	}
	return compassNames[(slot-1)%4]
}

// slotLateral is the five places beside a compass point, in fill order.
var slotLateral = [5]float64{0, 1, -1, 2, -2}

// gunSlotOffset is where battery slot `slot` (1-based) stands: slots 1-4 are N, E, S, W at the
// field's edge, 5-8 one step to their side, and so on; after five places per point the ring
// steps outward. ok=false once a slot would pass the anchoring reach. Pure.
func gunSlotOffset(slot int, size string) (dx, dy, dz float64, ok bool) {
	if slot < 1 {
		return 0, 0, 0, false
	}
	shield, reach := fieldOf(size)
	k := slot - 1
	dir := compassDirs[k%4]
	tier := k / 4
	lat := slotLateral[tier%len(slotLateral)] * slotLateralStep
	rad := shield + slotClearance + float64(tier/len(slotLateral))*slotRadialStep
	// Along the point's axis, and perpendicular to it in the tower's plane.
	x := dir[0]*rad + dir[1]*lat
	z := dir[1]*rad - dir[0]*lat
	if math.Hypot(x, z) > reach-slotReachMargin {
		return 0, 0, 0, false
	}
	return x, 0, z, true
}

// gunSlotCapacity is how many battery slots a size has. Pure.
func gunSlotCapacity(size string) int {
	n := 0
	for s := 1; s <= slotScanMax; s++ {
		if _, _, _, ok := gunSlotOffset(s, size); ok {
			n++
		}
	}
	return n
}

// placedBall is a hull already standing, or planned to.
type placedBall struct{ X, Y, Z, R float64 }

func (b placedBall) clear(x, y, z, r, gap float64) bool {
	dx, dy, dz := b.X-x, b.Y-y, b.Z-z
	need := b.R + r + gap
	return dx*dx+dy*dy+dz*dz >= need*need
}

func clearOfAll(x, y, z, r, gap float64, placed []placedBall) bool {
	for _, b := range placed {
		if !b.clear(x, y, z, r, gap) {
			return false
		}
	}
	return true
}

// laneClear reports whether a hull of radius r at (x, y, z) keeps out of the
// six flight lanes. A lane is a ray from the tower, so a piece behind it is
// clear. Pure.
func laneClear(x, y, z, r float64) bool {
	w := laneHalfWidth + r
	for _, d := range compassDirs {
		along := x*d[0] + z*d[1]
		if along <= 0 {
			continue
		}
		// Distance from the ray: what is left of the offset once the along-
		// axis part is removed.
		px, pz := x-d[0]*along, z-d[1]*along
		if math.Sqrt(px*px+y*y+pz*pz) < w {
			return false
		}
	}
	return math.Hypot(x, z) >= w // the vertical lanes, up and down
}

// positioned reports whether a plan piece already has its spot.
func (pc layoutPiece) positioned() bool { return pc.DX != 0 || pc.DY != 0 || pc.DZ != 0 }

// placedBalls is the tower and every positioned piece but `skip`.
func placedBalls(pieces []layoutPiece, race int64, skip int) []placedBall {
	out := []placedBall{{R: towerHull(race)}}
	for i, pc := range pieces {
		if i == skip || !pc.positioned() {
			continue
		}
		out = append(out, placedBall{pc.DX, pc.DY, pc.DZ, pieceRadius(pc.TypeID)})
	}
	return out
}

// scatterSample draws one candidate spot for a hull. Pure given rng.
func scatterSample(rng *rand.Rand, r, towerR, shield float64, service bool) (x, y, z float64, ok bool) {
	inner := towerR + r + innerWallGap
	outer := shield - r - outerWallGap
	if service {
		inner = towerR + r + serviceNear
		outer = math.Min(outer, towerR+r+serviceFar)
	}
	if outer <= inner {
		outer = inner + 1
	}
	rad := inner + (outer-inner)*math.Sqrt(rng.Float64())
	s := (rng.Float64()*2 - 1) * math.Sin(scatterMaxElev)
	el := math.Asin(s)
	h := rad * math.Cos(el)
	var az float64
	if service {
		// Beside a lane: just clear of its edge, on a random side of a
		// random compass point.
		d := laneHalfWidth + r + 300 + 1200*rng.Float64()
		if d >= h {
			return 0, 0, 0, false
		}
		k := rng.Intn(4)
		side := 1.0
		if rng.Intn(2) == 0 {
			side = -1
		}
		base := math.Atan2(compassDirs[k][1], compassDirs[k][0])
		az = base + side*math.Asin(d/h)
	} else {
		az = rng.Float64() * 2 * math.Pi
	}
	return h * math.Cos(az), rad * s, h * math.Sin(az), true
}

// placeInner gives every unpositioned working structure a spot in the shell.
// A piece that cannot be fitted with flyGap is tried once more at three
// quarters of it and otherwise left out: dropped reports how many, and the
// answer is the pieces kept, in order. Pure.
func placeInner(pieces []layoutPiece, size string, race, seed int64) (kept []layoutPiece, dropped int) {
	shield, _ := fieldOf(size)
	towerR := towerHull(race)
	out := append([]layoutPiece(nil), pieces...)
	drop := map[int]bool{}
	for i := range out {
		pc := &out[i]
		if pc.positioned() || isDefenseType(pc.TypeID) {
			continue
		}
		r := pieceRadius(pc.TypeID)
		rng := rand.New(rand.NewSource(seed*2654435761 + int64(i+1)*40503))
		placed := placedBalls(out, race, i)
		done := false
		for _, gap := range []float64{flyGap, flyGap * 0.75} {
			for t := 0; t < scatterTries && !done; t++ {
				x, y, z, ok := scatterSample(rng, r, towerR, shield, isServicePiece(pc.TypeID))
				if !ok || !laneClear(x, y, z, r) || !clearOfAll(x, y, z, r, gap, placed) {
					continue
				}
				pc.DX, pc.DY, pc.DZ = x, y, z
				done = true
			}
			if done {
				break
			}
		}
		if !done {
			drop[i] = true
		}
	}
	for i, pc := range out {
		if drop[i] {
			dropped++
			continue
		}
		kept = append(kept, pc)
	}
	return kept, dropped
}

// placeDefenses gives every battery that is not anchored and holds no slot the lowest free
// compass slot clear of every other hull. Anchored batteries keep their spot, and a battery no
// slot fits keeps the spot it has. Pure.
func placeDefenses(pieces []layoutPiece, size string) {
	used := map[int]bool{}
	for _, pc := range pieces {
		if pc.Slot > 0 {
			used[pc.Slot] = true
		}
	}
	for i := range pieces {
		pc := &pieces[i]
		if !isDefenseType(pc.TypeID) || pc.ItemID != 0 || pc.Slot > 0 {
			continue
		}
		r := pieceRadius(pc.TypeID)
		// Hulls already in space or holding a slot; a legacy battery not yet
		// anchored is about to move and does not count.
		var placed []placedBall
		for j, o := range pieces {
			if j == i || !o.positioned() || (isDefenseType(o.TypeID) && o.ItemID == 0 && o.Slot == 0) {
				continue
			}
			placed = append(placed, placedBall{o.DX, o.DY, o.DZ, pieceRadius(o.TypeID)})
		}
		for s := 1; s <= slotScanMax; s++ {
			if used[s] {
				continue
			}
			x, y, z, ok := gunSlotOffset(s, size)
			if !ok || !clearOfAll(x, y, z, r, flyGap, placed) {
				continue
			}
			pc.DX, pc.DY, pc.DZ, pc.Slot = x, y, z, s
			used[s] = true
			break
		}
	}
}

// placeLayout places whatever in a plan has no spot yet: the working
// structures scattered inside the field, the batteries on compass slots.
// Pure.
func placeLayout(pieces []layoutPiece, size string, race, seed int64) (kept []layoutPiece, dropped int) {
	kept, dropped = placeInner(pieces, size, race, seed)
	placeDefenses(kept, size)
	return kept, dropped
}
