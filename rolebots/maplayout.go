package rolebots

import (
	"math"
	"sort"
	"sync"
)

// Rigid grid layout for the trade maps.
//
// Real coordinates clump systems and stack their labels, so every node gets its
// own cell of a lattice: nothing overlaps, every label has room and every road
// has one of a few slopes. The picture is not to scale. The real coordinates
// seed the layout, and the relaxation only decides who gives way for a square.
//
// The topology is static map data, so a layout is computed once and cached. The
// map pages poll every few seconds, and recomputing would cost the relaxation
// each time and shift the map under the operator's mouse.

// cell is one square of the lattice: a column and a row, no units.
type cell struct {
	Col int `json:"col"`
	Row int `json:"row"`
}

// gridLayout is a finished placement: where every node sits, and how big
// the board is.
type gridLayout struct {
	At   map[int64]cell
	Cols int
	Rows int
}

var layoutCache sync.Map // layoutKey -> *gridLayout

type layoutKey struct {
	scope int64 // 0 = the galaxy by region, otherwise the regionID
	n     int   // node count, so a data change invalidates rather than lies
}

// cachedGridLayout builds a layout once per scope and hands the same one
// back after that.
func cachedGridLayout(scope int64, coords map[int64]xz, edges [][2]int64) *gridLayout {
	k := layoutKey{scope, len(coords)}
	if v, ok := layoutCache.Load(k); ok {
		return v.(*gridLayout)
	}
	l := buildGridLayout(coords, edges)
	layoutCache.Store(k, l)
	return l
}

// The relaxation is a packing, not a force-directed drawing: a gate should be
// one cell long and no two nodes closer than one cell. Repulsion is a
// short-range push that stops once a pair is apart; the springs do the rest.
const (
	layoutIters   = 600  // relaxation passes
	layoutEdgeLen = 1.0  // how far apart a gate wants its two ends, in cells
	layoutMinSep  = 1.15 // how close any two nodes may come before they push
	layoutSweeps  = 8    // post-snap tidying passes
	layoutSeedPin = 0.12 // how hard geography argues against a shorter edge
)

// buildGridLayout relaxes the seed coordinates into something with room in
// it, then snaps the result onto integer cells.
func buildGridLayout(coords map[int64]xz, edges [][2]int64) *gridLayout {
	ids := make([]int64, 0, len(coords))
	for id := range coords {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	n := len(ids)
	out := &gridLayout{At: map[int64]cell{}, Cols: 1, Rows: 1}
	if n == 0 {
		return out
	}
	idx := make(map[int64]int, n)
	for i, id := range ids {
		idx[id] = i
	}

	// Seed. normalise already flips EVE's z into screen y, so the board reads like
	// the in-game map.
	pos := normalise(coords)
	span := math.Sqrt(float64(n)) * layoutEdgeLen * 1.2
	x := make([]float64, n)
	y := make([]float64, n)
	sx := make([]float64, n)
	sy := make([]float64, n)
	for i, id := range ids {
		nx, ny := pos(id)
		// A deterministic nudge, because edge-on seeds collapse onto one point and
		// repulsion has no direction. Derived from the index, not a random source.
		x[i] = nx*span + math.Sin(float64(i)*12.9898)*0.05
		y[i] = ny*span + math.Cos(float64(i)*78.233)*0.05
		sx[i], sy[i] = x[i], y[i]
	}

	// Adjacency, deduped and with unknown ends dropped.
	adj := make([][]int, n)
	seen := map[[2]int]bool{}
	for _, e := range edges {
		a, oka := idx[e[0]]
		b, okb := idx[e[1]]
		if !oka || !okb || a == b {
			continue
		}
		k := [2]int{a, b}
		if a > b {
			k = [2]int{b, a}
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		adj[a] = append(adj[a], b)
		adj[b] = append(adj[b], a)
	}

	// Relaxation: gates pull, everything pushes, and the seed pulls each node back
	// toward its real position.
	dx := make([]float64, n)
	dy := make([]float64, n)
	for it := 0; it < layoutIters; it++ {
		for i := range dx {
			dx[i], dy[i] = 0, 0
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				ux, uy := x[i]-x[j], y[i]-y[j]
				d := math.Hypot(ux, uy)
				if d >= layoutMinSep {
					continue // far enough apart to be nobody's business
				}
				if d < 1e-6 {
					// Two seeds on one point. Push along a direction derived from the pair, not a
					// random one, so the same input lays out the same way.
					ux, uy = math.Cos(float64(i*31+j)), math.Sin(float64(i*31+j))
					d = 1
				}
				f := (layoutMinSep - d) * 0.5
				fx, fy := ux/d*f, uy/d*f
				dx[i] += fx
				dy[i] += fy
				dx[j] -= fx
				dy[j] -= fy
			}
		}
		for i := 0; i < n; i++ {
			for _, j := range adj[i] {
				if j < i {
					continue // each edge once
				}
				ux, uy := x[j]-x[i], y[j]-y[i]
				d := math.Hypot(ux, uy)
				if d < 1e-6 {
					continue
				}
				f := (d - layoutEdgeLen) * 0.25
				fx, fy := ux/d*f, uy/d*f
				dx[i] += fx
				dy[i] += fy
				dx[j] -= fx
				dy[j] -= fy
			}
			dx[i] += (sx[i] - x[i]) * 0.03
			dy[i] += (sy[i] - y[i]) * 0.03
		}
		// Cooling, in cells. Without it the last passes undo the
		// arrangement the first ones found.
		t := 1 - float64(it)/float64(layoutIters)
		step := 0.35*t*t + 0.02
		for i := 0; i < n; i++ {
			d := math.Hypot(dx[i], dy[i])
			if d > step {
				dx[i], dy[i] = dx[i]/d*step, dy[i]/d*step
			}
			x[i] += dx[i]
			y[i] += dy[i]
		}
	}

	// Snap. Busiest first: a hub with eight gates has the most to lose by being
	// shoved off its spot, and a leaf can sit anywhere near its one neighbour.
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		if len(adj[order[a]]) != len(adj[order[b]]) {
			return len(adj[order[a]]) > len(adj[order[b]])
		}
		return ids[order[a]] < ids[order[b]]
	})

	at := make([]cell, n)
	taken := map[cell]int{}
	for _, i := range order {
		want := cell{int(math.Round(x[i])), int(math.Round(y[i]))}
		if _, busy := taken[want]; !busy {
			at[i], taken[want] = want, i
			continue
		}
		// Nearest free square, measured against where the node actually
		// wanted to be rather than against the square it was refused.
		placed := false
		for r := 1; r < 512 && !placed; r++ {
			best := cell{}
			bestD := math.Inf(1)
			for dr := -r; dr <= r; dr++ {
				for dc := -r; dc <= r; dc++ {
					if abs(dr) != r && abs(dc) != r {
						continue // only the ring, the inside was searched
					}
					c := cell{want.Col + dc, want.Row + dr}
					if _, busy := taken[c]; busy {
						continue
					}
					d := math.Hypot(float64(c.Col)-x[i], float64(c.Row)-y[i])
					if d < bestD {
						best, bestD, placed = c, d, true
					}
				}
			}
			if placed {
				at[i], taken[best] = best, i
			}
		}
		if !placed { // unreachable with a 512-ring search; never leave a node unplaced
			at[i] = want
		}
	}

	// Tidy the snap's fixed-order choices. Cost is squared edge length, so one long
	// edge across the board costs more than several short ones and gets pulled in.
	cost := func(i int, c cell) float64 {
		s := 0.0
		for _, j := range adj[i] {
			d := math.Hypot(float64(c.Col-at[j].Col), float64(c.Row-at[j].Row))
			s += d * d
		}
		sd := math.Hypot(float64(c.Col)-sx[i], float64(c.Row)-sy[i])
		return s + layoutSeedPin*sd*sd
	}
	for sweep := 0; sweep < layoutSweeps; sweep++ {
		moved := false
		for _, i := range order {
			// Slide into a better empty square nearby.
			cur := cost(i, at[i])
			best, bestC := cur, at[i]
			for dr := -2; dr <= 2; dr++ {
				for dc := -2; dc <= 2; dc++ {
					c := cell{at[i].Col + dc, at[i].Row + dr}
					if _, busy := taken[c]; busy {
						continue
					}
					if v := cost(i, c); v < best-1e-9 {
						best, bestC = v, c
					}
				}
			}
			if bestC != at[i] {
				delete(taken, at[i])
				at[i], taken[bestC] = bestC, i
				moved = true
			}
			// Or trade squares with somebody close by.
			for dr := -3; dr <= 3; dr++ {
				for dc := -3; dc <= 3; dc++ {
					c := cell{at[i].Col + dc, at[i].Row + dr}
					j, busy := taken[c]
					if !busy || j == i {
						continue
					}
					ci, cj := at[i], at[j]
					before := cost(i, ci) + cost(j, cj)
					after := cost(i, cj) + cost(j, ci)
					if after < before-1e-9 {
						at[i], at[j] = cj, ci
						taken[cj], taken[ci] = i, j
						moved = true
					}
				}
			}
		}
		if !moved {
			break
		}
	}

	// Shift onto the origin and report the board size.
	minC, minR := at[0].Col, at[0].Row
	maxC, maxR := minC, minR
	for _, c := range at {
		minC, maxC = min(minC, c.Col), max(maxC, c.Col)
		minR, maxR = min(minR, c.Row), max(maxR, c.Row)
	}
	for i, id := range ids {
		out.At[id] = cell{at[i].Col - minC, at[i].Row - minR}
	}
	out.Cols, out.Rows = maxC-minC+1, maxR-minR+1
	return out
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
