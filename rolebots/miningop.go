package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Mining ops are rationed so NPC corporations do not draft a large share of
// all miners at once. Two rules apply, all here or keyed off here:
//   - a corporation runs one op a day, two when industry is its main focus,
//     and as many as it likes three hours apart when mining is
//     (miningOpRationFor);
//   - only an industrial hauls (haulerSeatSpec).

// categoryAsteroid is invCategories 25: every ore and ice type.
const categoryAsteroid = 25

// miningOpDaySec is the window a corporation's daily op count is read over,
// counted from when each op went live.
const miningOpDaySec = 24 * 3600

// miningOpRation is how often one corporation may send a mining op live.
// PerDay is the most ops in any 24 h window (0 means no cap, at most 2). GapSec
// is the least time between one go-live and the next.
type miningOpRation struct {
	PerDay int
	GapSec int64
}

// miningOpRationFor is the ration for a corporation's main focus (its SCC
// sector, corpSectorFor). Mining runs ops three hours apart, Industry two a
// day, anything else one a day.
func miningOpRationFor(sector string) miningOpRation {
	switch sector {
	case "Mining":
		return miningOpRation{GapSec: 3 * 3600}
	case "Industry":
		return miningOpRation{PerDay: 2}
	}
	return miningOpRation{PerDay: 1}
}

// haulerSeatSpec is the hauler's seat in a mining op: an industrial only.
// Recruiting asks for a hull in the band (hasSeatShipIn), the fit pipeline
// follows it (seatMatchesJob), and gangSeatReady excuses any other hull.
var haulerSeatSpec = seatSpec{Role: RoleHauler, HullMin: ClassIndustrial, HullMax: ClassIndustrial}

// isHaulerRole reports whether a fleet role carries the op's ore.
func isHaulerRole(role string) bool {
	return role == RoleHauler || role == RoleTrader
}

// recruitsHaveHauler reports whether a formation's picks seat anyone to haul.
func recruitsHaveHauler(picks []gangRecruitPick) bool {
	for _, p := range picks {
		if isHaulerRole(p.role) {
			return true
		}
	}
	return false
}

// narrowToHaulerSeat cuts a seated hauler's cargo candidates to the seat's
// hull band. If none is in band the list is returned whole, and
// gangSeatReady judges the hull picked. Other pilots and jobs pass through.
func (m *Mod) narrowToHaulerSeat(ctx context.Context, charID int64, job string, cands []*fitCandidate) []*fitCandidate {
	if job != JobTrade || len(cands) == 0 {
		return cands
	}
	s, ok := m.seatSpecFor(ctx, charID)
	if !ok || !isHaulerRole(s.Role) {
		return cands
	}
	byID, _, err := m.loadHullProfiles(ctx)
	if err != nil {
		return cands
	}
	in := cands[:0:0]
	for _, c := range cands {
		if h := byID[c.t.Fit.HullTypeID]; h != nil && hullInBand(h.Class, s) {
			in = append(in, c)
		}
	}
	if len(in) == 0 {
		return cands
	}
	return in
}

// gangStrayCanSystem finds ore the op left in a system other than its current
// site, after a belt move or a Director move. gangCansAt reads one system, so
// haulers never return to it. Returns the system with the oldest such ore and
// its m3 there, or 0 when there is none.
func (m *Mod) gangStrayCanSystem(ctx context.Context, gangID, siteSystemID int64) (int64, float64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.canItemID, c.systemID,
		       CASE WHEN c.firstCreatedAt > 0 AND c.firstCreatedAt < c.createdAt
		            THEN c.firstCreatedAt ELSE c.createdAt END AS q
		FROM rolebot_gang_cans c
		JOIN entity e ON e.itemID = c.canItemID
		WHERE c.gangID = ? AND c.systemID <> ?
		ORDER BY q`, gangID, siteSystemID)
	if err != nil {
		rlogf("rolebots: stray cans for gang %d: %v", gangID, err)
		return 0, 0
	}
	type can struct{ id, sys int64 }
	var cans []can
	for rows.Next() {
		var c can
		var q int64
		if rows.Scan(&c.id, &c.sys, &q) == nil {
			cans = append(cans, c)
		}
	}
	rows.Close()
	var pick int64
	total := 0.0
	for _, c := range cans {
		if pick != 0 && c.sys != pick {
			continue
		}
		used, err := m.canUsedM3(ctx, c.id)
		if err != nil || used <= 0 {
			continue
		}
		pick = c.sys
		total += used
	}
	return pick, total
}

// miningOpAllowed is the ration, as a pure function. A corporation with an op
// still alive waits for it; otherwise it waits out the gap since its last
// go-live and stays under its daily count. lastLiveAt and prevLiveAt are the
// last two go-lives, newest first; 0 means never.
func miningOpAllowed(r miningOpRation, lastLiveAt, prevLiveAt, now int64, haveOne bool) bool {
	if haveOne {
		return false
	}
	if lastLiveAt != 0 && now-lastLiveAt < r.GapSec {
		return false
	}
	if r.PerDay <= 0 {
		return true
	}
	today := 0
	for _, at := range []int64{lastLiveAt, prevLiveAt} {
		if at != 0 && now-at < miningOpDaySec {
			today++
		}
	}
	return today < r.PerDay
}

// corpMiningOpAllowed reads the ration for one corporation. Operator gangs
// (corp 0) are the GM's and are not rationed.
func (m *Mod) corpMiningOpAllowed(ctx context.Context, corpID int64) (bool, error) {
	if corpID == 0 {
		return true, nil
	}
	var alive int64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_gangs
		WHERE corpID = ? AND enabled = 1 AND activity IN (?, ?)`,
		corpID, ActMining, ActIndMining).Scan(&alive); err != nil {
		return false, fmt.Errorf("mining op count: %w", err)
	}
	var last, prev int64
	err := m.db.QueryRowContext(ctx, `
		SELECT lastLiveAt, prevLiveAt FROM rolebot_corp_fleet_day
		WHERE corporationID = ? AND activity = ?`, corpID, ActMining).Scan(&last, &prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("mining op day: %w", err)
	}
	// A big corporation may run several ops at once, each with its own belt and
	// haulers. The ration still spaces their go-lives.
	var roster int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		WHERE b.enabled = 1 AND c.corporationID = ?`, corpID).Scan(&roster)
	r := miningOpRationFor(m.corpSectorFor(ctx, corpID))
	return miningOpAllowed(r, last, prev, clock.Unix(), alive >= int64(miningOpConcurrency(int(roster)))), nil
}

// miningOpConcurrency is how many mining ops a corporation with this many
// enabled pilots may have alive: one per two full-size ops of roster, at least
// one, at most miningOpMaxConcurrent.
func miningOpConcurrency(roster int) int {
	n := roster / (2 * tierMiningHi)
	return max(1, min(n, miningOpMaxConcurrent))
}

// miningOpMaxConcurrent caps a corporation's simultaneous mining ops.
const miningOpMaxConcurrent = 3

// stampFleetLive records that a corporation's mining op went live, which
// spends its ration. The previous go-live shifts down a slot (MySQL assigns
// left to right, so prevLiveAt takes the old lastLiveAt). A failed stamp is
// only logged.
func (m *Mod) stampFleetLive(ctx context.Context, g *Gang) {
	if g == nil || g.CorpID == 0 || !fleetAttributesOre(g.Activity) {
		return
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_fleet_day (corporationID, activity, lastLiveAt)
		VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE prevLiveAt = lastLiveAt, lastLiveAt = VALUES(lastLiveAt)`,
		g.CorpID, ActMining, clock.Unix()); err != nil {
		rlogf("rolebots: stamp mining op day for corp %d: %v", g.CorpID, err)
	}
}
