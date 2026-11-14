package rolebots

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	evedb "gurimoon/db"
)

// settingMaxProjections is the server_settings key for the projection ceiling:
// how many rolebots may hold a ball in space at once. 0 means unlimited.
// A projection costs a grid-scan goroutine and per-second hydration (gridlane.go).
const settingMaxProjections = "rolebots.maxProjections"

const (
	// The most projections the ceiling withdraws in one pass, so a lowered cap
	// drains grids gradually (same idea as popMaxChurnPerPass).
	projMaxShedPerPass = 20
)

var (
	// projCapValue is the cached setting, read every reconcile pass.
	projCapValue atomic.Int64
	// projShedTotal counts projections the ceiling has withdrawn since boot.
	projShedTotal atomic.Int64
)

// MaxProjections reports the configured ceiling (0 = unlimited).
func MaxProjections() int { return int(projCapValue.Load()) }

func setMaxProjections(n int) {
	if n < 0 {
		n = 0
	}
	projCapValue.Store(int64(n))
}

// loadMaxProjections reads the ceiling at boot. A missing or unparseable value
// leaves it unlimited.
func (m *Mod) loadMaxProjections(ctx context.Context) {
	v, ok, err := evedb.GetServerSetting(ctx, m.db, settingMaxProjections)
	if err != nil {
		rlogf("rolebots: read projection cap: %v (leaving it unlimited)", err)
		return
	}
	if !ok {
		return
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return
	}
	setMaxProjections(n)
}

// playerSystems is the set of systems a real player is in, built once per pass.
// systemHasRealPlayer walks a system's entities under its lock, so callers use
// this set instead of asking per projection.
func (m *Mod) playerSystems() map[int64]bool {
	if m.systems == nil {
		return nil
	}
	out := map[int64]bool{}
	for _, ss := range m.systems.All() {
		if ss != nil && systemHasRealPlayer(ss) {
			out[ss.ID] = true
		}
	}
	// A docked player's system may not be loaded. Counting it widens the
	// preload ring so neighbours are projected before undock.
	if m.sessions != nil {
		for _, s := range m.sessions.Snapshot() {
			if s.Conn == nil || s.CharID == 0 || s.SolarSystemID == 0 {
				continue
			}
			out[s.SolarSystemID] = true
		}
	}
	return out
}

// projCapSeats applies the ceiling to one reconcile pass and returns the births
// that may be seated and the live projections that must give a seat back.
// Called with embodyMu held. Live projections are withdrawn only from systems
// with no real player, so a watched grid never loses a ship.
func (m *Mod) projCapSeats(births []embodyWant, live int, playerSys map[int64]bool) ([]embodyWant, []*embodiedBot) {
	// The lagging-grid gate runs before the unlimited-cap return: it does not
	// depend on a ceiling being set.
	births = m.holdBirthsOnLaggingGrids(births)

	limit := MaxProjections()
	if limit <= 0 {
		return births, nil
	}

	// Seats in watched systems are filled first, so a binding ceiling starves
	// the ring and not a grid a player is on. Within each, a bashing siege's pilots go first.
	sortSiegeBirthsFirst(births)
	sort.SliceStable(births, func(i, j int) bool {
		return playerSys[births[i].systemID] && !playerSys[births[j].systemID]
	})

	if headroom := limit - live; headroom < len(births) {
		if headroom < 0 {
			headroom = 0
		}
		if len(births) > headroom {
			rlogf("rolebots: projection cap %d holds %d birth(s) this pass (%d live)",
				limit, len(births)-headroom, live)
		}
		births = births[:headroom]
	}

	over := live - limit
	if over <= 0 {
		return births, nil
	}
	if over > projMaxShedPerPass {
		over = projMaxShedPerPass
	}

	// Candidates are ring projections that are not busy. eb.mu under embodyMu
	// follows the lock order embodyWorkMu, embodyMu, eb.mu.
	var cand []*embodiedBot
	for _, eb := range embodyLive {
		if playerSys[eb.systemID] {
			continue
		}
		eb.mu.Lock()
		busy := eb.dead || eb.fighting || eb.fleeing || eb.warping
		eb.mu.Unlock()
		if busy {
			continue
		}
		cand = append(cand, eb)
	}
	sort.SliceStable(cand, func(i, j int) bool {
		return projShedScore(cand[i]) > projShedScore(cand[j])
	})
	if len(cand) > over {
		cand = cand[:over]
	}
	if len(cand) > 0 {
		projShedTotal.Add(int64(len(cand)))
		rlogf("rolebots: projection cap %d exceeded by %d — withdrawing %d unwatched projection(s)",
			limit, live-limit, len(cand))
	}
	return births, cand
}

// sortSiegeBirthsFirst moves the pilots of a bashing siege (siegeHoldsPilot) ahead of the other
// births, keeping the order otherwise, so a capped pass seats the siege first.
func sortSiegeBirthsFirst(births []embodyWant) {
	now := clock.Unix()
	siege := make(map[int64]bool, len(births))
	for _, w := range births {
		if siegeHoldsPilot(w.corpID, w.systemID, now) {
			siege[w.charID] = true
		}
	}
	if len(siege) == 0 {
		return
	}
	sort.SliceStable(births, func(i, j int) bool {
		return siege[births[i].charID] && !siege[births[j].charID]
	})
}

// holdBirthsOnLaggingGrids refuses new projections in a system whose watchers'
// stamp stream is already behind. It gates births only; live projections stay.
// The threshold is warpPauseLead and it reads the rise, not the raw lead. A system with no fresh
// reading is left alone.
func (m *Mod) holdBirthsOnLaggingGrids(births []embodyWant) []embodyWant {
	pause := warpPauseLead()
	if pause <= 0 || len(births) == 0 {
		return births
	}
	lagging := map[int64]bool{}
	kept := births[:0]
	held := 0
	for _, w := range births {
		lag, seen := lagging[w.systemID]
		if !seen {
			rise, fresh := peekSystemRise(w.systemID)
			lag = fresh && rise >= pause
			lagging[w.systemID] = lag
		}
		if lag {
			held++
			continue
		}
		kept = append(kept, w)
	}
	if held > 0 {
		rlogf("rolebots: %d birth(s) held — those grids' watchers' stamp leads rose +%d or more in %.0fs, "+
			"so a new ball there would be paid for by a client that is falling behind",
			held, pause, warpRiseWindow.Seconds())
	}
	return kept
}

// projShedScore ranks a ring projection for withdrawal; higher goes first.
// Idle, non-gang, non-mining projections cost least to lose, and a gang member
// goes last.
func projShedScore(eb *embodiedBot) int {
	s := 0
	if eb.gangID == 0 {
		s += 200
	}
	if !eb.mining {
		s += 100
	}
	if !eb.holding {
		s += 50
	}
	return s
}

// ProjStatus is what the panel reports about the ceiling.
type ProjStatus struct {
	Cap  int // 0 = unlimited
	Live int // projections holding a ball right now
	Shed int // withdrawals by the ceiling since boot
}

// projStatus counts the live picture for the panel.
func projStatus() ProjStatus {
	embodyMu.Lock()
	live := len(embodyLive)
	embodyMu.Unlock()
	return ProjStatus{
		Cap:  MaxProjections(),
		Live: live,
		Shed: int(projShedTotal.Load()),
	}
}
