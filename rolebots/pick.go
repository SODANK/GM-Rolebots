package rolebots

// The shared near-tie sampler for address decisions: the module's softmax
// without-replacement draw (appetite.go pickActivity, buildbook.go drawOrder),
// index-based so corphome.go and moveout.go share it across candidate types.
//
// Sticky picks sample at all because a corp home or move-out target is written
// to the character and kept, and every pilot ranks candidates with the same
// shared numbers, so "take the best" makes the whole roster take the same best.
// Sampling among near-ties spreads them; the crowd term (moveout.go,
// homeCrowdPenalty) keeps a near-tie from becoming a pile.

import (
	"math"
	"math/rand"
)

// addressPickTempPts converts a pilot's PickTemperature (0.15 to 0.7, tuning.go)
// into a softmax temperature in hub-score points, since hub scores are not on
// the O(1) scale the knob was calibrated against. See corpPickTempScale.
const addressPickTempPts = 8.0

// softmaxOrder is the visit order over indices 0..n-1: a softmax draw
// without replacement at temperature temp, the same arithmetic as
// drawOrder (buildbook.go). Fewer than two candidates is already an order.
func softmaxOrder(n int, score func(int) float64, temp float64) []int {
	return softmaxOrderFrom(n, score, temp, rand.Float64)
}

// softmaxOrderSeeded is softmaxOrder on a private stream: the same seed and
// scores give the same order on every call. For a pick re-derived on each look
// (bestCorpAddress), where a fresh draw per look sent a pilot between stations.
func softmaxOrderSeeded(n int, score func(int) float64, temp float64, seed int64) []int {
	return softmaxOrderFrom(n, score, temp, rand.New(rand.NewSource(seed)).Float64)
}

func softmaxOrderFrom(n int, score func(int) float64, temp float64, draw func() float64) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if n < 2 {
		return order
	}
	if temp < 0.01 {
		temp = 0.01
	}
	out := make([]int, 0, n)
	pool := order
	for len(pool) > 0 {
		best := score(pool[0])
		for _, i := range pool[1:] {
			if s := score(i); s > best {
				best = s
			}
		}
		weights := make([]float64, len(pool))
		total := 0.0
		for k, i := range pool {
			weights[k] = math.Exp((score(i) - best) / temp)
			total += weights[k]
		}
		pick := len(pool) - 1
		roll := draw() * total
		for k, w := range weights {
			if roll < w {
				pick = k
				break
			}
			roll -= w
		}
		out = append(out, pool[pick])
		pool = append(pool[:pick], pool[pick+1:]...)
	}
	return out
}
