package rolebots

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"

	evedb "gurimoon/db"
)

// Gate arrivals, staged.
//
// Bots jumping through the same gate in the same sim pass would land as a stack of
// brackets, while real pilots trickle through. This is the undockwave.go release
// budget at a gate, keyed by the destination gate where a player sees the clump.
// It applies in every system, watched or not, because a hub's backlog builds
// while nobody looks.

const (
	defGateWaveSize = 4
	defGateWaveSec  = 8

	// defGateWaveUnwatchedMul multiplies the window's size when no connected player
	// is on the far-side gate's grid. 0 = no cap for an unwatched far side; 1 = no
	// boost.
	defGateWaveUnwatchedMul = 4

	settingGateWaveSize         = "rolebots.gateWaveSize"
	settingGateWaveSec          = "rolebots.gateWaveSec"
	settingGateWaveUnwatchedMul = "rolebots.gateWaveUnwatchedMul"
)

var (
	gateCfgMu            sync.RWMutex
	gateWaveSize         = defGateWaveSize
	gateWaveSec          = int64(defGateWaveSec)
	gateWaveUnwatchedMul = defGateWaveUnwatchedMul
)

// loadGateSettings reads the dials at boot and every minute from
// startTrafficSettingsReload. Missing or unparseable values keep the defaults.
func (m *Mod) loadGateSettings(ctx context.Context) {
	readInt := func(name string, def int) int {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, name)
		if err != nil || !ok {
			return def
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return def
		}
		return n
	}
	size := readInt(settingGateWaveSize, defGateWaveSize)
	sec := readInt(settingGateWaveSec, defGateWaveSec)
	mul := readInt(settingGateWaveUnwatchedMul, defGateWaveUnwatchedMul)
	if sec < 1 {
		sec = 1
	}
	gateCfgMu.Lock()
	gateWaveSize, gateWaveSec, gateWaveUnwatchedMul = size, int64(sec), mul
	gateCfgMu.Unlock()
}

// gateWaveFor is the window size for one ask: the base size, or the base times
// gateWaveUnwatchedMul when nobody is on the far side's grid. unlimited is true
// when the multiplier is 0 and the far side is unwatched.
func gateWaveFor(farWatched bool) (size int, window int64, unlimited bool) {
	gateCfgMu.RLock()
	defer gateCfgMu.RUnlock()
	size, window = gateWaveSize, gateWaveSec
	if farWatched || size <= 0 {
		return size, window, false
	}
	switch mul := gateWaveUnwatchedMul; {
	case mul == 0:
		return size, window, true
	case mul > 1:
		size *= mul
	}
	return size, window, false
}

func gateWave() (size int, window int64) {
	gateCfgMu.RLock()
	defer gateCfgMu.RUnlock()
	return gateWaveSize, gateWaveSec
}

// GateWaveSettings reports the current dials for the panel.
func GateWaveSettings() (size int, windowSec int64) {
	return gateWave()
}

// ── the release budget ──────────────────────────────────────────────────────

type gateWaveEntry struct {
	windowStart int64
	released    int
	gangID      int64
	gangLeft    int
}

var (
	gateWaveMu sync.Mutex
	gateWaves  = map[int64]*gateWaveEntry{}

	// gateHeld counts refusals per destination gate since the last log line, and
	// gateHeldLogAt is when it was last printed. Both are under gateWaveMu.
	gateHeld      = map[int64]int{}
	gateHeldLogAt int64
)

// gateHeldLogEvery is how often the gate-wave line may print, in seconds.
const gateHeldLogEvery = 60

// gateHeldLine returns the once-a-minute server.log line about held jumps and
// resets the tally, or "" when it is not yet due or nothing was held. The
// top three destination gates are named; the rest are folded into the total.
// gateWaveMu must be held.
func gateHeldLine(size int, window, now int64) string {
	if now-gateHeldLogAt < gateHeldLogEvery {
		return ""
	}
	total := 0
	for _, n := range gateHeld {
		total += n
	}
	if total == 0 {
		return ""
	}
	type kv struct {
		gate int64
		n    int
	}
	top := make([]kv, 0, len(gateHeld))
	for g, n := range gateHeld {
		top = append(top, kv{g, n})
	}
	sort.Slice(top, func(i, j int) bool {
		if top[i].n != top[j].n {
			return top[i].n > top[j].n
		}
		return top[i].gate < top[j].gate
	})
	if len(top) > 3 {
		top = top[:3]
	}
	parts := make([]string, len(top))
	for i, t := range top {
		parts[i] = fmt.Sprintf("gate %d %d", t.gate, t.n)
	}
	gateHeld = map[int64]int{}
	gateHeldLogAt = now
	return fmt.Sprintf("[rolebots] gate wave — %d jump(s) held in the last %ds (top: %s; %d per %ds)",
		total, gateHeldLogEvery, strings.Join(parts, ", "), size, window)
}

// admitGateJump reports whether this bot may jump through to destGateID now.
// The logic is admitUndock's: a per-window budget with gang priority that never
// splits a fleet across windows. farWatched says whether a player is on the far
// gate's grid; an unwatched far side gets a wider window (gateWaveFor).
func admitGateJump(destGateID, gangID int64, gangSize int, farWatched bool) bool {
	size, window, unlimited := gateWaveFor(farWatched)
	if size <= 0 || destGateID == 0 || unlimited {
		return true // pacing disabled
	}
	now := clock.Unix()

	gateWaveMu.Lock()
	ok := admitGateJumpLocked(destGateID, gangID, gangSize, size, window, now)
	if !ok {
		gateHeld[destGateID]++
	}
	line := gateHeldLine(size, window, now)
	gateWaveMu.Unlock()
	if line != "" {
		log.Print(line)
	}
	return ok
}

// admitGateJumpLocked is the window arithmetic. gateWaveMu must be held.
func admitGateJumpLocked(destGateID, gangID int64, gangSize, size int, window, now int64) bool {
	w := gateWaves[destGateID]
	if w == nil || now-w.windowStart >= window {
		w = &gateWaveEntry{windowStart: now}
		gateWaves[destGateID] = w
	}

	if gangID != 0 {
		if w.gangID == gangID && w.gangLeft > 0 {
			w.gangLeft--
			w.released++
			return true
		}
		if w.gangID != 0 && w.gangID != gangID && w.gangLeft > 0 {
			return false
		}
		if w.released >= size {
			return false
		}
		if gangSize < 1 {
			gangSize = 1
		}
		w.gangID = gangID
		w.gangLeft = gangSize - 1
		w.released++
		return true
	}

	if w.released >= size {
		return false
	}
	w.released++
	return true
}

// admitGateJumpFor asks the gate on behalf of one pilot, resolving its fleet
// headcount first so a whole fleet can be claimed at once.
func (m *Mod) admitGateJumpFor(ctx context.Context, b *botRun, destSysID, destGateID int64) bool {
	size := 0
	if b.GangID != 0 {
		if n, err := gangSize(ctx, m.db, b.GangID); err == nil {
			size = n
		}
	}
	return admitGateJump(destGateID, b.GangID, size, m.celestialGridWatched(ctx, destSysID, destGateID))
}

// jumpStep is what travelStep does with a pilot whose next hop is a gate.
type jumpStep int

const (
	jumpFlying jumpStep = iota // still on its way to the gate: hold the leg, ask nothing
	jumpHeld                   // at the gate, the wave said not yet: wait there
	jumpGo                     // at the gate and admitted: flash and jump
)

// jumpDecision orders the two questions a gate hop asks: has the pilot reached
// the gate, and may it jump. The wave is asked only once the flight is over, so a
// pilot is paced at the jump and never held at the station door it is leaving.
func jumpDecision(flying bool, admit func() bool) jumpStep {
	if flying {
		return jumpFlying
	}
	if !admit() {
		return jumpHeld
	}
	return jumpGo
}

// gateRetryDelay is how long a refused bot waits before asking again: one window
// plus up to one more, so refused bots do not all return at once.
func gateRetryDelay() int64 {
	_, window := gateWave()
	return window + rand.Int63n(window+1)
}

// sweepGateWaves drops windows nothing has used for a while.
func sweepGateWaves() {
	_, window := gateWave()
	cutoff := clock.Unix() - window*10
	gateWaveMu.Lock()
	for gateID, w := range gateWaves {
		if w.windowStart < cutoff {
			delete(gateWaves, gateID)
		}
	}
	gateWaveMu.Unlock()
}

// resolveDestGate finds the gate in toSysID whose destination is fromSysID.
// Returns 0 if none is found; the caller bypasses the wave rather than stranding
// a bot.
func (m *Mod) resolveDestGate(ctx context.Context, fromSysID, toSysID int64) int64 {
	cels, err := evedb.CachedSystemCelestials(ctx, m.db, toSysID)
	if err != nil {
		return 0
	}
	for i := range cels {
		if cels[i].GroupID == groupStargate && cels[i].DestSolarSystemID == fromSysID {
			return cels[i].ItemID
		}
	}
	return 0
}

func init() { registerSweep("gateWaves", sweepGateWaves) }
