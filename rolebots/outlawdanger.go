package rolebots

// The outlaw's soft line.
//
// Under crimewatch 1.0 an outlaw (security status at or below -5.0) may enter
// any system and raises no flag doing it (core spaceentry.go), but in policed
// space every pilot may shoot it for free (seclaw's VictimOutlaw rung, roe.go
// "outlaw"). So policed space is priced, never closed: each hop into a system
// at or above policedSecLine costs outlawDangerJumps x (1 + ln(1+eyes)), where
// eyes are the pilots in space there (exposed rolebots from the target census
// and connected players). An empty 0.5 costs two jumps, a hub with fifty
// pilots up about ten. Lawless hops cost nothing extra.

import (
	"context"
	"math"
	"sync"
)

// policedSecLine is where CONCORD's jurisdiction begins (core concordMinSec).
const policedSecLine = 0.45

// outlawDangerJumps is what an outlaw pays per log unit of eyes on a policed
// hop, in jump-equivalents.
const outlawDangerJumps = 2.0

var (
	outlawEyesMu sync.Mutex
	outlawEyesV  map[int64]int
	outlawEyesAt int64
)

// outlawHopCost is the outlaw term of hopCost. Pure.
func outlawHopCost(danger float64, sec float64, eyes int) int {
	if danger <= 0 || sec < policedSecLine {
		return 0
	}
	return int(math.Round(danger * (1 + math.Log1p(float64(eyes)))))
}

// outlawEyesIn is the pilots in space in sysID at the last prime.
func outlawEyesIn(sysID int64) int {
	outlawEyesMu.Lock()
	defer outlawEyesMu.Unlock()
	return outlawEyesV[sysID]
}

// primeOutlawDanger refreshes the eyes count before a search that will price
// it, at most once per target census.
func (m *Mod) primeOutlawDanger(ctx context.Context, r travelRules) {
	if r.OutlawDanger <= 0 {
		return
	}
	c := m.targetCensusNow(ctx)
	outlawEyesMu.Lock()
	fresh := outlawEyesV != nil && outlawEyesAt == c.at
	outlawEyesMu.Unlock()
	if fresh {
		return
	}
	eyes := make(map[int64]int, len(c.bySys))
	for sys, ts := range c.bySys {
		eyes[sys] = len(ts)
	}
	if m.sessions != nil {
		for _, s := range m.sessions.Snapshot() {
			if !s.Bot && s.StationID == 0 && s.SolarSystemID != 0 {
				eyes[s.SolarSystemID]++
			}
		}
	}
	outlawEyesMu.Lock()
	outlawEyesV, outlawEyesAt = eyes, c.at
	outlawEyesMu.Unlock()
}

// setOutlawEyesForTest installs an eyes count as if just primed.
func setOutlawEyesForTest(v map[int64]int) {
	outlawEyesMu.Lock()
	outlawEyesV, outlawEyesAt = v, clock.Unix()
	outlawEyesMu.Unlock()
}
