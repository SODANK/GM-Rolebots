package rolebots

import "sync"

// Who has aggressed whom, for fights the core never sees.
// The core's exemption ladder lets a victim answer a pilot who aggressed them or their corp
// (seclaw.ExemptEngaged), reading the per-system crimewatch table. The abstract tier has no
// such table: a system nobody has loaded keeps none, and the core drops a table when its
// system unloads. Without this ledger a bot that was shot at in one roster pass and fired back
// in the next was billed as the aggressor. Every landed abstract exchange and every embodied
// act on a pilot (notePilotAggression) is written here under the core's keys, the victim's
// charID and corpID, for the crimewatch window, and the abstract charges ask it before they
// ask the core.

// engagementWindowSec is the crimewatch window, the core's aggressionDuration.
const engagementWindowSec = criminalFlagSec

var (
	engagedMu sync.Mutex
	// engaged[aggressorCharID][victimKey] = unix second of the last act, victimKey a charID or
	// a corpID, as in the core's table.
	engaged = map[int64]map[int64]int64{}
)

// noteEngagement records that aggressorCharID acted on victimCharID, and so on the victim's
// corporation.
func noteEngagement(aggressorCharID, victimCharID, victimCorpID int64) {
	noteEngagementAt(aggressorCharID, victimCharID, victimCorpID, clock.Unix())
}

func noteEngagementAt(aggressorCharID, victimCharID, victimCorpID, now int64) {
	if aggressorCharID <= 0 || victimCharID <= 0 || aggressorCharID == victimCharID {
		return
	}
	engagedMu.Lock()
	defer engagedMu.Unlock()
	v := engaged[aggressorCharID]
	if v == nil {
		v = map[int64]int64{}
		engaged[aggressorCharID] = v
	}
	v[victimCharID] = now
	if victimCorpID > 0 {
		v[victimCorpID] = now
	}
}

// victimHadEngaged reports whether victimCharID aggressed the shooter or the shooter's corp
// inside the window: the abstract half of the core's victimEngaged, so firing back is lawful.
func victimHadEngaged(victimCharID, shooterCharID, shooterCorpID int64) bool {
	return victimHadEngagedAt(victimCharID, shooterCharID, shooterCorpID, clock.Unix())
}

func victimHadEngagedAt(victimCharID, shooterCharID, shooterCorpID, now int64) bool {
	if victimCharID <= 0 || shooterCharID <= 0 || victimCharID == shooterCharID {
		return false
	}
	engagedMu.Lock()
	defer engagedMu.Unlock()
	v := engaged[victimCharID]
	if v == nil {
		return false
	}
	live := func(key int64) bool {
		ts, ok := v[key]
		return ok && now-ts < engagementWindowSec
	}
	return live(shooterCharID) || (shooterCorpID > 0 && live(shooterCorpID))
}

// sweepEngagements drops lapsed entries.
func sweepEngagements() {
	now := clock.Unix()
	engagedMu.Lock()
	for aggr, v := range engaged {
		for k, ts := range v {
			if now-ts >= engagementWindowSec {
				delete(v, k)
			}
		}
		if len(v) == 0 {
			delete(engaged, aggr)
		}
	}
	engagedMu.Unlock()
}

func init() {
	registerCharForget("engagements", func(id int64) {
		engagedMu.Lock()
		delete(engaged, id)
		for _, v := range engaged {
			delete(v, id)
		}
		engagedMu.Unlock()
	})
	registerSweep("engagements", sweepEngagements)
}
