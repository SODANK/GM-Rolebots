package rolebots

// The off-hours job claim. A pilot logged off for its hours whose factory or lab job has sat finished for
// rolebots.tz.claimAfterHours logs in briefly and takes it off the line, the paperwork the oven sweeps do for a pilot in
// its hours (industryOvenSweep, labOvenSweep). It frees the line and the blueprint and nothing else: no ball, presence,
// listing or new install, so time away still earns nothing. The goods wait in the factory hangar until the pilot's
// hours, when industryHarvest routes the listing. Notes: docs/BOT_POPULATION.md#play-hours--timezones.

import (
	"context"
	"sync/atomic"
)

const (
	// tzClaimEverySec spaces the claim passes. A job that has waited the
	// threshold loses nothing to a few more minutes.
	tzClaimEverySec = 300
	// tzClaimSoonMin: a pilot whose hours open within this many minutes is
	// left to collect the job itself.
	tzClaimSoonMin = 120
)

// tzClaimLast is the clock second of the last claim pass. simPass is
// single-flight, so only one pass reads and writes it at a time.
var tzClaimLast atomic.Int64

// tzClaimPass runs the claim for every pilot logged off for its hours. bots is
// this pass's roster. The pilots it touches took no turn this pass, so their
// copies are current unless they were reset since the load (refreshStaleRun).
func (m *Mod) tzClaimPass(ctx context.Context, bots []*botRun) {
	c := tzSettings()
	if !c.On || c.ClaimAfterH <= 0 {
		return
	}
	now := clock.Unix()
	if last := tzClaimLast.Load(); last > 0 && now-last < tzClaimEverySec {
		return
	}
	tzClaimLast.Store(now)
	ctx, cancel := context.WithTimeout(ctx, simBotBudget)
	defer cancel()
	cut := tzClaimCut(win32Now(), c.ClaimAfterH)
	for _, b := range bots {
		if ctx.Err() != nil {
			return
		}
		if !tzClaimWants(b) || !tzIsOff(b.CharID) {
			continue
		}
		st := tzNow(b.CharID)
		if !tzClaimDue(st) {
			continue
		}
		if !m.refreshStaleRun(ctx, b) || !tzClaimWants(b) {
			continue
		}
		n := m.industryOvenSweepBefore(ctx, b, cut) + m.labOvenSweepBefore(ctx, b, cut)
		if n == 0 {
			continue
		}
		if st.In {
			// In its hours but held off with its gang (tzUnitWantsOff).
			m.logBot(b.CharID, logInfo, "presence",
				"logged in briefly to take %s off the line (finished over %s ago) while its gang is logged off",
				plural(n, "job", "jobs"), tzDur(c.ClaimAfterH*60))
			continue
		}
		m.logBot(b.CharID, logInfo, "presence",
			"logged in briefly to take %s off the line (finished over %s ago); its hours start in %s",
			plural(n, "job", "jobs"), tzDur(c.ClaimAfterH*60), tzDur(st.NextMin))
	}
}

// tzClaimCut is the Win32 time a job must have finished by to be claimed:
// afterH hours before now.
func tzClaimCut(now, afterH int64) int64 {
	return now - afterH*3600*10_000_000
}

// tzClaimDue reports whether a logged-off pilot should claim now rather than
// wait for its own hours: they do not open within tzClaimSoonMin.
func tzClaimDue(st tzState) bool {
	return st.In || st.NextMin >= tzClaimSoonMin
}

// tzClaimWants reports whether a pilot has a job on either cooking list that
// may still be on its line. No query; a halted pilot is left alone.
func tzClaimWants(b *botRun) bool {
	if b == nil || b.State == nil || b.State.Halted {
		return false
	}
	for _, j := range b.State.Data.IndCooking {
		if !j.Delivered && j.JobID != 0 {
			return true
		}
	}
	for _, j := range b.State.Data.LabCooking {
		if j.JobID != 0 {
			return true
		}
	}
	return false
}
