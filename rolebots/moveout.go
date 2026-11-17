package rolebots

import (
	"context"
	"math"
	"sync"
)

// Moving out of the academy.
//
// A generated pilot wakes in its school's station with its medical clone
// installed there (schools.go). This step moves it out once, early: it prices
// each reachable medical bay against the commute (wanderlust), the distance to
// a school and the crowd already in that system, plus this world's luck for the
// system (fortune.go), draws one, flies there and installs its clone. This is the
// pilot's first address; once it has an employer, the employer decides
// (corphome.go). No trade or traffic measure is read, because a hub score that
// counts homes feeds on its own moves.

const (
	// The first look is spread across this window by character ID, so pilots
	// generated together do not leave the same lobby in one pass. Shorter than the
	// graduation window.
	moveOutWindowSec = 90 * 60

	// How long after a look that could not move (nowhere better, nowhere
	// reachable) the pilot looks again.
	moveOutRetrySec = 30 * 60

	// How long a pilot already flying to its new home holds off looking again.
	// See moveOut.
	moveOutHoldSec = 120

	// How far the search runs, in jump-equivalents (the router's own currency, so
	// security aversion is priced in). Wanderlust sets it. It must reach past the
	// nursery and crowd penalties below, or a pilot has no candidate to move to.
	moveOutJumpsSettled = 8
	moveOutJumpsBase    = 18
	moveOutJumpsNomad   = 40

	// What one jump-equivalent of permanent commute costs, in the same points as
	// the dispersal penalties below. A settled pilot avoids distance, a nomad
	// barely counts it.
	moveOutJumpCostSettled = 4.0
	moveOutJumpCostBase    = 1.8
	moveOutJumpCostNomad   = 0.7

	// Points the best candidate must beat the pilot's current station by
	// before it is worth moving at all. Also the step size that makes the
	// walk finite.
	moveOutMargin = 5.0
)

// ── the two dispersal penalties ─────────────────────────────────────────────
// Both are subtracted alongside the commute and neither is a filter: the
// nursery (systems next to a school) and the crowd (see homeCrowd).
const (
	// What being next door to a nursery costs, and how many jumps it decays over.
	// Zero at moveOutNurseryReach and beyond. At 1 jump it outweighs the commute a
	// mid pilot saves by staying (1.8 a jump).
	moveOutNurseryPts   = 22.0
	moveOutNurseryReach = 4.0

	// What crowding costs: a log of the ratio to a fair share (see homeCrowd). 0 at
	// a fair share, about 22 points at four times it, capped so popularity alone
	// never rules an address out. Shared with corphome.go.
	moveOutCrowdBite = 16.0
	moveOutCrowdCap  = 45.0

	// How long the roster's home census is good for. It changes only when a
	// pilot moves in, and a stale count costs one pilot one slightly wrong
	// ranking.
	homeCrowdTTLSec = 10 * 60
)

// moveOutNurseryPenalty prices a candidate's distance from the nearest
// chargen system. jumps < 0 means no school can reach it: far, and free.
func moveOutNurseryPenalty(jumps int) float64 {
	if jumps < 0 || float64(jumps) >= moveOutNurseryReach {
		return 0
	}
	return moveOutNurseryPts * (1 - float64(jumps)/moveOutNurseryReach)
}

// homeCrowdPenalty prices how much of the roster already lives in a
// system, against its fair share (homeCrowd's baseline).
func homeCrowdPenalty(homes int, baseline float64) float64 {
	if homes <= 0 {
		return 0
	}
	if baseline < 1 {
		baseline = 1
	}
	p := moveOutCrowdBite * math.Log1p(float64(homes)/baseline-1)
	if p < 0 {
		return 0
	}
	if p > moveOutCrowdCap {
		return moveOutCrowdCap
	}
	return p
}

var (
	homeCrowdMu   sync.Mutex
	homeCrowdBySy map[int64]int
	homeCrowdBase float64
	homeCrowdAt   int64

	schoolJumpsOnce sync.Once
	schoolJumpsMap  map[int64]int
)

// homeCrowd is the roster's census: how many rolebots keep their medical clone
// in each system, and a system's fair share: the roster outside the schools
// spread over every non-school system with a medical bay (a fixed target, not
// an average over occupied systems). A failed read answers an empty census.
func (m *Mod) homeCrowd(ctx context.Context) (map[int64]int, float64) {
	now := clock.Unix()
	homeCrowdMu.Lock()
	if homeCrowdBySy != nil && now-homeCrowdAt < homeCrowdTTLSec {
		c, b := homeCrowdBySy, homeCrowdBase
		homeCrowdMu.Unlock()
		return c, b
	}
	homeCrowdMu.Unlock()

	rows, err := m.db.QueryContext(ctx, `
		SELECT s.solarSystemID, COUNT(*)
		  FROM character_ c
		  JOIN rolebot_characters b ON b.characterID = c.characterID
		  JOIN staStations s ON s.stationID = c.homeStationID
		 WHERE c.homeStationID > 0
		 GROUP BY s.solarSystemID`)
	if err != nil {
		rlogf("rolebots: move-out: home census: %v", err)
		return nil, 1
	}
	defer rows.Close()
	out := map[int64]int{}
	total := 0
	for rows.Next() {
		var sysID int64
		var n int
		if err := rows.Scan(&sysID, &n); err != nil {
			rlogf("rolebots: move-out: home census: %v", err)
			return nil, 1
		}
		out[sysID] = n
		total += n
	}
	if err := rows.Err(); err != nil {
		rlogf("rolebots: move-out: home census: %v", err)
		return nil, 1
	}
	sch := m.schoolsSoft(ctx)
	for sysID, n := range out {
		if sch.Systems[sysID] {
			total -= n
		}
	}
	livable := 0
	if bays, err := m.medicalStations(ctx); err == nil {
		for sysID := range bays {
			if !sch.Systems[sysID] {
				livable++
			}
		}
	}
	base := 1.0
	if livable > 0 {
		base = float64(total) / float64(livable)
		if base < 1 {
			base = 1
		}
	}
	homeCrowdMu.Lock()
	homeCrowdBySy, homeCrowdBase, homeCrowdAt = out, base, now
	homeCrowdMu.Unlock()
	return out, base
}

// noteHomeMove applies one move to the cached census in place, so the next
// pilot to rank addresses sees it before the census is re-read. A system of
// 0 (no previous home, or a station the map does not know) is skipped.
func (m *Mod) noteHomeMove(fromSys, toSys int64) {
	if fromSys == toSys {
		return
	}
	homeCrowdMu.Lock()
	defer homeCrowdMu.Unlock()
	if homeCrowdBySy == nil {
		return // nothing cached; the next read counts it
	}
	// Copy-on-write: homeCrowd hands its map out and callers read it without the
	// lock, so the cached map is never written in place.
	next := make(map[int64]int, len(homeCrowdBySy)+1)
	for k, v := range homeCrowdBySy {
		next[k] = v
	}
	if fromSys != 0 && next[fromSys] > 0 {
		next[fromSys]--
	}
	if toSys != 0 {
		next[toSys]++
	}
	homeCrowdBySy = next
}

// schoolJumps is hop distance from the nearest chargen system, for every system
// that can reach one. The schools are static, so it is built once from
// corpJumpMap's multi-source BFS.
func (m *Mod) schoolJumps(ctx context.Context) map[int64]int {
	schoolJumpsOnce.Do(func() {
		sch, err := m.schools(ctx)
		if err != nil || sch == nil || len(sch.Systems) == 0 {
			return
		}
		src := make([]int64, 0, len(sch.Systems))
		for id := range sch.Systems {
			src = append(src, id)
		}
		dist, err := m.corpJumpMap(ctx, src...)
		if err != nil {
			rlogf("rolebots: move-out: school distances: %v", err)
			return
		}
		schoolJumpsMap = dist
	})
	return schoolJumpsMap
}

// moveOutCrowdedLookPts is how crowded a settled pilot's home must be to earn
// the one re-look described in moveOut: two thirds of the cap, reached only by
// systems the roster has piled into.
const moveOutCrowdedLookPts = moveOutCrowdCap * 2 / 3

var moveOutCrowdLooked = map[int64]bool{}

// grantCrowdedLook answers whether this settled pilot gets its one look at
// leaving a pile-up, and records that it has been offered. The record is
// memory-only like moveOutSettled, so a restart offers the look again: one
// ranking per pilot per run.
func (m *Mod) grantCrowdedLook(ctx context.Context, charID, home int64) bool {
	moveOutMu.Lock()
	looked := moveOutCrowdLooked[charID]
	moveOutMu.Unlock()
	if looked {
		return false
	}
	sysID := m.stationSystemID(ctx, home)
	if sysID == 0 {
		return false
	}
	crowd, base := m.homeCrowd(ctx)
	if homeCrowdPenalty(crowd[sysID], base) < moveOutCrowdedLookPts {
		return false
	}
	moveOutMu.Lock()
	moveOutCrowdLooked[charID] = true
	moveOutMu.Unlock()
	return true
}

// jumpsFromSchool is the census read: -1 for a system no school can reach,
// which is as far away as it gets.
func jumpsFromSchool(dist map[int64]int, sysID int64) int {
	if dist == nil {
		return -1
	}
	if d, ok := dist[sysID]; ok {
		return d
	}
	return -1
}

// moveOutRange is how far this pilot will look for somewhere to live.
func moveOutRange(traits map[string]int) int {
	t := traitScale(traits[TraitWanderlust])
	return bendInt(t, moveOutJumpsSettled, moveOutJumpsBase, moveOutJumpsNomad)
}

// moveOutJumpCost is what a jump of commute costs this pilot, in points of
// hub standing.
func moveOutJumpCost(traits map[string]int) float64 {
	t := traitScale(traits[TraitWanderlust])
	return bend(t, moveOutJumpCostSettled, moveOutJumpCostBase, moveOutJumpCostNomad)
}

var (
	moveOutMu sync.Mutex
	// When this pilot next looks. Memory-only: a restart re-spreads the window
	// and never repeats a move, because a moved pilot's home is no longer a school.
	moveOutNextAt = map[int64]int64{}
	// Pilots known to be settled, so the common case (the whole roster,
	// forever, after its first day) costs one map lookup instead of a
	// home-station read every half hour.
	moveOutSettled = map[int64]bool{}
)

// forgetMoveOut drops a pilot's move-out bookkeeping, so the next planning
// pass looks again from scratch. Called when an operator edits a profile:
// clearing a home station by hand is a request to reconsider.
func forgetMoveOut(charID int64) {
	moveOutMu.Lock()
	delete(moveOutNextAt, charID)
	delete(moveOutSettled, charID)
	delete(moveOutCrowdLooked, charID)
	moveOutMu.Unlock()
}

// medStation is one station with a medical bay: somewhere a pilot could wake up.
type medStation struct {
	StationID int64
	SystemID  int64
	CorpID    int64
}

var (
	medStationsOnce  sync.Once
	medStationsBySys map[int64][]medStation
	medStationsErr   error
)

// medicalStations is every station offering the Cloning service, by system.
// Loaded once: station services are static, and a query per system per
// candidate per pilot would be costly.
func (m *Mod) medicalStations(ctx context.Context) (map[int64][]medStation, error) {
	medStationsOnce.Do(func() {
		rows, err := m.db.QueryContext(ctx, `
			SELECT s.stationID, s.solarSystemID, COALESCE(s.corporationID, 0)
			  FROM staStations s
			  JOIN staOperationServices os ON os.operationID = s.operationID
			 WHERE os.serviceID = ?`, serviceCloning)
		if err != nil {
			medStationsErr = err
			return
		}
		defer rows.Close()
		out := map[int64][]medStation{}
		for rows.Next() {
			var s medStation
			if err := rows.Scan(&s.StationID, &s.SystemID, &s.CorpID); err != nil {
				medStationsErr = err
				return
			}
			out[s.SystemID] = append(out[s.SystemID], s)
		}
		if err := rows.Err(); err != nil {
			medStationsErr = err
			return
		}
		medStationsBySys = out
	})
	return medStationsBySys, medStationsErr
}

// homeCandidate is one place this pilot could live, and what it is worth.
type homeCandidate struct {
	medStation
	Jumps int
	Score float64
}

// moveOut is the step: a pilot still homed in the academy looks for somewhere
// of its own and goes there. Reports whether the bot is now travelling, in
// which case the caller plans nothing else this pass. Early returns are cheap
// no-ops because this runs on every idle plan of every bot.
func (m *Mod) moveOut(ctx context.Context, b *botRun) (travelling bool, err error) {
	if b.StationID == 0 {
		return false, nil // a medical clone is installed in a station, docked
	}
	// A pinned address is the operator's, even a pin to the academy. markMovedOut
	// so nothing looks again.
	if b.Profile != nil && b.Profile.HomePinned {
		m.markMovedOut(b.CharID)
		return false, nil
	}
	now := clock.Unix()

	moveOutMu.Lock()
	settled := moveOutSettled[b.CharID]
	next, scheduled := moveOutNextAt[b.CharID]
	if !scheduled {
		// First sighting schedules the look rather than taking it.
		moveOutNextAt[b.CharID] = now + b.CharID%moveOutWindowSec
	}
	moveOutMu.Unlock()
	if settled || !scheduled || now < next {
		return false, nil
	}

	sch, err := m.schools(ctx)
	if err != nil {
		// Without the school list nobody is flown anywhere. Stop asking; a restart
		// reloads it.
		m.markMovedOut(b.CharID)
		return false, err
	}
	home := m.homeStationFor(ctx, b.CharID)
	if !sch.Stations[home] {
		// Already settled elsewhere; the one exception is a crowded-home re-look.
		// See docs/comment-notes-b14.md#move-out-crowded-relook.
		if !corpIsSchool(ctx, m.db, b.CorpID) || !m.grantCrowdedLook(ctx, b.CharID, home) {
			m.markMovedOut(b.CharID)
			return false, nil
		}
	}

	traits, err := m.botTraits(ctx, b)
	if err != nil {
		return false, err
	}
	best, here, err := m.rankHomes(ctx, b, sch, traits)
	if err != nil {
		m.deferMoveOut(b.CharID, now+moveOutRetrySec)
		return false, err
	}
	if best == nil {
		m.deferMoveOut(b.CharID, now+moveOutRetrySec)
		m.logBot(b.CharID, logInfo, "clone",
			"still living in the station it was created in, and nothing within %d jumps it would rather wake up in",
			moveOutRange(traits))
		return false, nil
	}
	const movedIn = "moved its medical clone into %s — an address of its own rather than the academy it was created in"

	// Where it stands is already a place it could live and nothing in reach is
	// enough better to move. refusesHome is checked here for this one station.
	if here != nil && best.Score < here.Score+moveOutMargin &&
		!m.refusesHome(ctx, b, here.CorpID) {
		return false, m.adoptHome(ctx, b, here.StationID, movedIn)
	}
	if best.StationID == b.StationID {
		return false, m.adoptHome(ctx, b, best.StationID, movedIn)
	}

	m.logBot(b.CharID, logInfo, "clone",
		"leaving the academy: %s is %s and the best place within %d jumps it could live — moving its clone there",
		m.stationLabel(ctx, best.StationID), m.travelNote(ctx, b, best.SystemID, best.Jumps),
		moveOutRange(traits))
	if err := m.beginTravel(ctx, b, best.SystemID, best.StationID, "idle"); err != nil {
		m.deferMoveOut(b.CharID, now+moveOutRetrySec)
		return false, err
	}
	// Held only over the trip: the pilot must look again on docking, because
	// arriving makes that station the one it moves into.
	m.deferMoveOut(b.CharID, now+moveOutHoldSec)
	return true, nil
}

// rankHomes scores every medical bay this pilot could reach as a place to
// live, and returns the best of them along with the station it is standing
// in when that is a candidate in its own right (a school never is).
func (m *Mod) rankHomes(ctx context.Context, b *botRun, sch *schoolMap,
	traits map[string]int) (best, here *homeCandidate, err error) {

	bays, err := m.medicalStations(ctx)
	if err != nil {
		return nil, nil, err
	}
	reach, err := m.systemsWithin(ctx, b.SystemID, rulesForBot(b), moveOutRange(traits))
	if err != nil {
		return nil, nil, err
	}
	perJump := moveOutJumpCost(traits)
	crowd, crowdBase := m.homeCrowd(ctx)
	fromSchool := m.schoolJumps(ctx)
	luck := m.systemFortune(ctx)

	var ranked []homeCandidate
	for sysID, jumps := range reach {
		if sch.Systems[sysID] {
			continue // the starter systems are the thing being left
		}
		// Both dispersal penalties depend on the system, so they are priced once
		// here rather than per bay.
		nursery := moveOutNurseryPenalty(jumpsFromSchool(fromSchool, sysID))
		crowded := homeCrowdPenalty(crowd[sysID], crowdBase)
		// This world's luck for the system (fortune.go): per-pilot draws average out over a
		// roster, so without a per-world one every server settles the same places.
		fortune := fortuneHomePts * luck(sysID)
		for _, s := range bays[sysID] {
			if sch.Stations[s.StationID] {
				continue
			}
			// No region head start: a bonus for the home region would argue for never
			// leaving. Every term is a cost, so the softmax spreads the roster.
			c := homeCandidate{medStation: s, Jumps: jumps}
			c.Score = -perJump*float64(jumps) - nursery - crowded + fortune +
				perJump*m.xenoHomeScore(ctx, b, s.CorpID)
			ranked = append(ranked, c)
			if s.StationID == b.StationID {
				pick := c
				here = &pick
			}
		}
	}
	if len(ranked) == 0 {
		return nil, here, nil
	}

	// Softmax draw at the pilot's own temperature (softmaxOrder), then a walk down
	// the order. See docs/comment-notes-b14.md#move-out-softmax-walk.
	order := softmaxOrder(len(ranked), func(i int) float64 {
		return ranked[i].Score
	}, b.Tune.PickTemperature*addressPickTempPts)
	for _, i := range order {
		c := ranked[i]
		if m.refusesHome(ctx, b, c.CorpID) {
			continue
		}
		out := c
		return &out, here, nil
	}
	return nil, here, nil
}

// reconsiderAllMoveOuts drops the roster's move-out bookkeeping so every pilot
// re-checks whether it still lives in the academy. Cheap for settled pilots:
// one query marks them settled again. Paired with reconsiderAllCorpHomes on
// the panel.
func reconsiderAllMoveOuts() {
	moveOutMu.Lock()
	moveOutNextAt = map[int64]int64{}
	moveOutSettled = map[int64]bool{}
	// The crowded re-look is reset too, so the panel can break up a pile-up
	// without a restart.
	moveOutCrowdLooked = map[int64]bool{}
	moveOutMu.Unlock()
}

// markMovedOut settles a pilot: nothing looks at it again this run.
func (m *Mod) markMovedOut(charID int64) {
	moveOutMu.Lock()
	moveOutSettled[charID] = true
	delete(moveOutNextAt, charID)
	moveOutMu.Unlock()
}

// deferMoveOut sets when this pilot looks again.
func (m *Mod) deferMoveOut(charID, at int64) {
	moveOutMu.Lock()
	moveOutNextAt[charID] = at
	moveOutMu.Unlock()
}

func init() { registerCharForget("moveOut*", forgetMoveOut) }