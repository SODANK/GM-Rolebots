package rolebots

import (
	"sync"
	"sync/atomic"
	"time"
)

// settingPreloadDocked switches the docked pin: the system a player is docked in stays loaded
// and projected. Default ON. See docs/comment-notes-b15.md#preload-docked.
const settingPreloadDocked = "rolebots.preloadDocked"

// dockedSetTTL is how stale the docked-system set may be. systemProjected is asked once per
// candidate projection on every reconcile, and the answer only changes when somebody docks or undocks.
const dockedSetTTL = time.Second

var (
	// preloadDockedOn is the cached switch. Set at boot by loadPreloadSetting
	// (on unless the setting says "0") and by the panel.
	preloadDockedOn atomic.Bool

	dockedSetMu   sync.Mutex
	dockedSet     map[int64]bool
	dockedSetRead time.Time
)

// PreloadDocked reports whether a docked player's system is held open.
func PreloadDocked() bool { return preloadDockedOn.Load() }

func setPreloadDocked(on bool) { preloadDockedOn.Store(on) }

// dockedSystems is the set of solar systems with a real player docked in them, read from the
// session registry (a docked player is in no SolarSystem) and cached for dockedSetTTL.
// The returned map is shared and must not be written to.
func (m *Mod) dockedSystems() map[int64]bool {
	if m.sessions == nil {
		return nil
	}
	dockedSetMu.Lock()
	defer dockedSetMu.Unlock()
	now := clock.Now()
	if dockedSet != nil && now.Sub(dockedSetRead) < dockedSetTTL {
		return dockedSet
	}
	out := map[int64]bool{}
	for _, s := range m.sessions.Snapshot() {
		if s.Conn == nil || s.CharID == 0 || s.StationID == 0 || s.SolarSystemID == 0 {
			continue
		}
		out[s.SolarSystemID] = true
	}
	dockedSet, dockedSetRead = out, now
	return out
}

// systemHasDockedPlayer answers for one system, through the cached set.
func (m *Mod) systemHasDockedPlayer(sysID int64) bool {
	return m.dockedSystems()[sysID]
}
