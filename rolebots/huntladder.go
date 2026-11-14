package rolebots

// Hunters pick ground safest end first. Searching nearest first answered with the
// lowest security in a mixed region, where most belt fights are unwinnable, so nothing
// died and nothing dropped. A hunter works its region down from the safest rung;
// a rung with nothing winnable adds no candidates, so the next rung down is tried.
//
// The ladder does not outrank work orders: distress calls and recent maulings are
// bucketed before security. The hopeless gate in `weigh` still runs first, so a rung
// the pilot cannot survive never enters the ranking.

import (
	"math"
	"math/rand"
)

// The candidate buckets, in rank order: distress (a workforce locked out of its belts),
// bitten (one pilot's recent mauling), plain (quiet ground) and any (survivable but
// unappealing, so a fussy pilot still has somewhere to go).
const (
	huntBucketDistress = iota
	huntBucketBitten
	huntBucketPlain
	huntBucketAny
)

// huntCand is one system the search is willing to answer with.
type huntCand struct {
	SysID  int64
	Bucket int
	Rung   int // security rung, higher is safer
	Depth  int // jumps from where the pilot is standing
	// Bounty is what the expected spawn pays (spawnThreat.Bounty), set only
	// for ground the pilot would actually engage. 0 = unread or not winnable.
	Bounty float64
}

// huntBountyBand folds a spawn's bounty into the band the ladder ranks on: one band per
// quadrupling, so only clearly richer ground outranks a safer rung. 0 for no bounty. Pure.
// Only ground the pilot can engage carries a bounty, so an unwinnable rich rung never
// beats a safe one.
func huntBountyBand(bounty float64) int {
	if bounty < 1 {
		return 0
	}
	return 1 + int(math.Floor(math.Log2(bounty)/2))
}

// huntSecRung buckets a system's true security into tenths, the granularity the client
// shows and the ore tables use. Truesec, not the rounded display value, so 0.45 and 0.44
// land in different rungs. Nullsec runs negative and keeps descending.
func huntSecRung(sec float64) int {
	r := int(sec * 10)
	if sec < 0 && float64(r) != sec*10 {
		r-- // truncation rounds toward zero; the rung floors
	}
	return r
}

// betterHuntCand reports whether a beats b: work order first, then the
// richer bounty band (with huntByBounty on), then the safest rung, then the
// shortest trip.
func betterHuntCand(a, b huntCand) bool {
	if a.Bucket != b.Bucket {
		return a.Bucket < b.Bucket
	}
	if HuntByBounty() {
		if ab, bb := huntBountyBand(a.Bounty), huntBountyBand(b.Bounty); ab != bb {
			return ab > bb
		}
	}
	if a.Rung != b.Rung {
		return a.Rung > b.Rung
	}
	return a.Depth < b.Depth
}

// pickHuntCand chooses one system out of the whole scope. Candidates tied with the winner
// on all keys are drawn at random, so hunters that share the bitten map and the graph do
// not all land on one spawn.
func pickHuntCand(cands []huntCand) (huntCand, bool) {
	if len(cands) == 0 {
		return huntCand{}, false
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if betterHuntCand(c, best) {
			best = c
		}
	}
	var tied []huntCand
	for _, c := range cands {
		if !betterHuntCand(best, c) && !betterHuntCand(c, best) {
			tied = append(tied, c)
		}
	}
	return tied[rand.Intn(len(tied))], true
}
