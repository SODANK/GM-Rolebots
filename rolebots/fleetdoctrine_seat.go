package rolebots

import (
	"context"
	"sync"
)

// The seat is the spec. A member's row in rolebot_gang_members carries its seat's hull
// band, tank layer and range band (fleetdoctrine.go seatSpec), written at formation from
// the fleet's doctrine and read through seatSpecOf (the fit pipeline: seatSpecFor).
//
//   - Fits: generateFits takes the seat's style ahead of the pilot's own (styleFor) and
//     builds the hull on the seat's layer. Solo trips keep the pilot's style.
//   - Procurement: the shortlist is cut to the seat's hull band, or the whole ladder if empty.
//   - Recruiting: hasSeatShipIn asks for a hull in band for the seat.
//
// A row with no spec (an operator gang, a mining op) reads as unspecified everywhere.

// seatSpecCacheSec is how long a pilot's seat stands in memory. The fit
// pipeline asks several times a plan pass; a seat changes only at formation
// and disband.
const seatSpecCacheSec = 60

type seatSpecEntry struct {
	at   int64
	spec seatSpec
	ok   bool
}

var (
	seatSpecMu    sync.Mutex
	seatSpecCache = map[int64]seatSpecEntry{}
)

// forgetSeatSpec drops a pilot's cached seat (formation, disband, tests).
func forgetSeatSpec(charID int64) {
	seatSpecMu.Lock()
	delete(seatSpecCache, charID)
	seatSpecMu.Unlock()
}

func init() { registerCharForget("seatSpecCache", forgetSeatSpec) }

// primeSeatSpec puts a seat into the cache before the member row exists. The recruiter
// asks the strict seat test (recruitSeatOK) of a pick it has not written yet, and fits
// are generated against the pilot's seat (fitgen.go seatSpecFor). The caller forgets a
// pick that is not written.
func primeSeatSpec(charID int64, spec seatSpec) {
	seatSpecMu.Lock()
	seatSpecCache[charID] = seatSpecEntry{at: clock.Unix(), spec: spec, ok: spec.HullMax != 0}
	seatSpecMu.Unlock()
}

// seatSpecOf is the spec a member row carries, and whether it carries one at
// all: a row with no hull band is unspecified.
func seatSpecOf(mem GangMember) (seatSpec, bool) {
	if mem.HullMax == 0 {
		return seatSpec{}, false
	}
	return seatSpec{
		Role: mem.Role, HullMin: mem.HullMin, HullMax: mem.HullMax,
		Tank: mem.Tank, Style: mem.Style,
	}, true
}

// seatSpecFor is the seat the pilot currently holds, if it holds one with a
// spec. One query, cached for seatSpecCacheSec.
func (m *Mod) seatSpecFor(ctx context.Context, charID int64) (seatSpec, bool) {
	now := clock.Unix()
	seatSpecMu.Lock()
	if e, ok := seatSpecCache[charID]; ok && now-e.at < seatSpecCacheSec {
		seatSpecMu.Unlock()
		return e.spec, e.ok
	}
	seatSpecMu.Unlock()

	var mem GangMember
	var lo, hi, style int
	var tank string
	err := m.db.QueryRowContext(ctx, `
		SELECT gm.role, gm.hullMin, gm.hullMax, gm.tank, gm.style
		  FROM rolebot_gang_members gm
		  JOIN rolebot_gangs g ON g.gangID = gm.gangID AND g.enabled = 1
		 WHERE gm.characterID = ?`, charID).Scan(&mem.Role, &lo, &hi, &tank, &style)
	var spec seatSpec
	ok := false
	if err == nil {
		mem.HullMin, mem.HullMax = HullClass(lo), HullClass(hi)
		mem.Tank, mem.Style = TankPref(tank), combatStance(style)
		spec, ok = seatSpecOf(mem)
	}
	seatSpecMu.Lock()
	seatSpecCache[charID] = seatSpecEntry{at: now, spec: spec, ok: ok}
	seatSpecMu.Unlock()
	return spec, ok
}

// seatMatchesJob reports whether a seat governs fits for this job: the
// specialist seats their own lens, the gun seat (Role "") either combat lens.
// A seated pilot generating a mining or hauling fit is not steered.
func seatMatchesJob(s seatSpec, job string) bool {
	switch s.Role {
	case RoleTackler:
		return job == JobTackle
	case RoleLogi:
		return job == JobLogi
	case RoleEwar:
		return job == JobEwar
	case RoleScout:
		return job == JobScout
	case "", RoleHunter, RoleCombat, RoleEscort:
		return job == JobCombat || job == JobPiracy
	case RoleHauler, RoleTrader:
		// A mining op's hauler seat (haulerSeatSpec): the cargo fit is steered into the
		// industrial band so procurement buys one, not the frigate the pilot owns.
		return job == JobTrade
	}
	return false
}

// hullInBand reports whether a hull class sits inside the seat's band. A
// seat with no band admits everything.
func hullInBand(c HullClass, s seatSpec) bool {
	if s.HullMax == 0 {
		return true
	}
	return c >= s.HullMin && c <= s.HullMax
}

// seatIntent is the engagement intent a seat's range band asks the fit
// generator for: kite and brawl as the pilot's own styles would, hold as
// long guns without the microwarpdrive (engageHold, fitstyle.go).
func seatIntent(st combatStance) engageStyle {
	switch st {
	case stanceKite:
		return engageKite
	case stanceHold:
		return engageHold
	}
	return engageBrawl
}

// hullForSeat is the hull profile the generator assembles a seated pilot's fit on: the
// hull with its tank layer read as the seat's. It returns a copy, because hullProfileByID
// hands one pointer to every planning goroutine (hulldoctrine.go). The hull's own layer
// is the fallback for a seat on no layer.
func hullForSeat(h *HullProfile, s seatSpec) *HullProfile {
	if h == nil || s.Tank == TankNone || s.Tank == h.Tank {
		return h
	}
	c := *h
	c.Tank = s.Tank
	return &c
}

// hasSeatShipIn is hasSeatShip against a seat spec: does the pilot own a live
// hull, inside the seat's band, from the pool its seat's job draws? A seat
// with no band is hasSeatShip exactly.
func (m *Mod) hasSeatShipIn(ctx context.Context, charID int64, activity string, s seatSpec) (bool, error) {
	if s.HullMax == 0 {
		return m.hasSeatShip(ctx, charID, activity)
	}
	pool, err := m.candidatePool(ctx, charID, m.jobForPilot(ctx, charID, activity))
	if err != nil || len(pool) == 0 {
		return false, err
	}
	byID, _, err := m.loadHullProfiles(ctx)
	if err != nil {
		return false, err
	}
	ships, err := shipsForChar(ctx, m.db, charID)
	if err != nil {
		return false, err
	}
	for _, t := range pool {
		h := byID[t.HullTypeID]
		if h == nil || !hullInBand(h.Class, s) {
			continue
		}
		if shipID, ok := ships[t.FitID]; ok && shipID != 0 && entityExists(ctx, m.db, shipID, charID) &&
			m.seatRackOK(ctx, activity, shipID) {
			return true, nil
		}
	}
	return false, nil
}
