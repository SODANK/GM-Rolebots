package rolebots

import (
	"context"
	"errors"
)

// Mining is one trip, rock or ice, with one appetite and one hull class (a
// barge mines both). This file picks a belt or an Ice Field each time the trip
// is flown.
//
// The signal is tower demand (icedemand.go). With no tower in reach the rock
// pays. Once a corporation anchors a tower, its region is short of that
// tower's isotope, Heavy Water and Liquid Ozone, and ice outranks rock for
// barge pilots in reach. A pilot with no barge mines rock in the hull it
// flies; the fit ladder (fitselect.go) buys a barge when the wallet allows.

// iceBeatsRock reports whether ice is the better mining trip right now, with
// both opportunities. Pure.
func iceBeatsRock(rockOpp, iceOpp float64) bool {
	return iceOpp > 0 && iceOpp > rockOpp
}

// miningTripOpportunity is the better of the rock's and the ice's
// opportunity, with its label. A tower short of fuel in reach of a barge
// pilot lifts the ice.
func miningTripOpportunity(m *Mod, ctx context.Context, b *botRun) (float64, string) {
	rock, rockWhy := opportunityFromFits(JobMining)(m, ctx, b)
	ice, iceWhy := iceOpportunity(m, ctx, b)
	if iceBeatsRock(rock, ice) {
		return ice, "ice: " + iceWhy
	}
	if rock > 0 {
		return rock, "rock: " + rockWhy
	}
	return 0, rockWhy + "; ice: " + iceWhy
}

// miningTripPlan flies the mining trip: to the ice when it beats the rock,
// else to a belt, and to the other when the first will not start.
func (m *Mod) miningTripPlan(ctx context.Context, b *botRun) error {
	rock, _ := opportunityFromFits(JobMining)(m, ctx, b)
	ice, iceWhy := iceOpportunity(m, ctx, b)
	type leg struct {
		label string
		plan  func(*Mod, context.Context, *botRun) error
	}
	order := []leg{{"rock", (*Mod).miningPlan}, {"ice", (*Mod).icePlan}}
	if iceBeatsRock(rock, ice) {
		order[0], order[1] = order[1], order[0]
		m.logBot(b.CharID, logInfo, "mining", "taking its barge to the ice — %s", iceWhy)
	} else if ice <= 0 {
		// The ice is closed to this pilot (no barge, no skill): rock only.
		order = order[:1]
	}
	var first error
	for _, l := range order {
		err := l.plan(m, ctx, b)
		if err == nil {
			return nil
		}
		b.State.Activity = ""
		if first == nil {
			first = err
		}
		if !errors.Is(err, errNoFitAvailable) {
			return err
		}
		m.logBot(b.CharID, logInfo, "mining", "%s passed over (%v)", l.label, err)
	}
	return first
}
