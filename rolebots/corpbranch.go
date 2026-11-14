package rolebots

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	evedb "gurimoon/db"
)

// Branch offices: a corporation's recruiting net.
//
// Recruitment prices a corporation by how far a pilot's home is from the nearest of its
// addresses (corpSystemsAll, nearestJump, corpHomeMul), so a one-office corporation is invisible
// to most of the map. A CEO with roster room and money past its reserve rents a branch in a busy
// hub with a medical bay, spread from its other offices and near the headquarters. Rules, in
// order: never before the factory or lab office (corpfacility.go); only with openings to fill
// (corpBranchMinOpenings); at most corpBranchLimit addresses (one more for a nomad), one
// branch per corpBranchEverySec; and rent plus three reserves must be affordable.

const (
	corpBranchMinOpenings = 3
	corpBranchMaxOffices  = 4
	// corpBranchMembersPer: one more address for every this many members, up to
	// corpBranchMaxOfficesBig, so a large corporation recruits wider.
	corpBranchMembersPer    = 50
	corpBranchMaxOfficesBig = 8
	corpBranchEverySec      = 3 * 24 * 3600
	corpBranchMinSpread     = 5  // jumps from every existing address
	corpBranchMaxJumps      = 15 // jumps from the headquarters
	corpBranchSystems       = 40 // best-scoring systems whose stations are read
	// corpBranchRetrySec: a branch search that found nowhere, or could not pay, waits this long.
	corpBranchRetrySec = 6 * 3600
)

// branchTryAt is the last branch search per corporation that came to nothing. In memory: a
// restart may look again.
var branchTryAt sync.Map // corpID → unix seconds

// branchSite is one system a branch could open in.
type branchSite struct {
	SystemID  int64
	Homes     int // rostered pilots homed in the system
	Security  float64
	Spread    int // jumps to the nearest existing address
	FromHQ    int // jumps from the headquarters
	StationID int64
	Name      string
}

// branchValue is what a branch in a system is worth to recruitment: the pilots who live there,
// on a log so one crowded system does not flatten the rest, and a little more for distance from
// the corporation's other addresses, up to twelve jumps. Pure.
func branchValue(s branchSite) float64 {
	homes := math.Min(1, math.Log1p(float64(s.Homes))/math.Log1p(100))
	spread := 1 + 0.25*math.Min(1, float64(s.Spread-corpBranchMinSpread)/float64(12-corpBranchMinSpread))
	return homes * spread
}

// rankBranchSites filters and orders candidate systems. Pure, for the test.
func rankBranchSites(sites []branchSite, floor float64) []branchSite {
	var out []branchSite
	for _, s := range sites {
		if s.Security < floor || s.Spread < corpBranchMinSpread || s.FromHQ < 0 || s.FromHQ > corpBranchMaxJumps {
			continue
		}
		if s.Homes == 0 {
			continue // nobody to recruit
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if vi, vj := branchValue(out[i]), branchValue(out[j]); vi != vj {
			return vi > vj
		}
		return out[i].SystemID < out[j].SystemID
	})
	return out
}

// facilitiesSatisfied reports whether an industry desk's factory needs are met, the gate a
// branch waits behind.
func (m *Mod) facilitiesSatisfied(ctx context.Context, corpID int64, tags []string) bool {
	needs := facilityNeedsFor(tags)
	if !needs.Mfg {
		return true
	}
	factory := m.corpFactoryStation(ctx, corpID)
	if factory == 0 {
		return false
	}
	return !needs.Lab || m.stationHasLab(ctx, factory)
}

// corpBranchLimit is how many addresses a corporation of members may hold before
// it stops opening branches. Pure.
func corpBranchLimit(members int) int {
	return min(corpBranchMaxOffices+members/corpBranchMembersPer, corpBranchMaxOfficesBig)
}

// ceoBranchOffice is the estate power's last step.
func (m *Mod) ceoBranchOffice(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, hq int64, officeCount int) error {
	limit := corpBranchLimit(facts.Members)
	if ch.nomadic() {
		limit++
	}
	if officeCount == 0 || officeCount >= limit || hq == 0 {
		return nil
	}
	if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "branch") < corpBranchEverySec {
		return nil
	}
	if free, unlimited, err := m.corpFreeOpenings(ctx, c.CorpID); err != nil || unlimited || free < corpBranchMinOpenings {
		return err
	}
	if !m.facilitiesSatisfied(ctx, c.CorpID, facts.Policy.Tags) {
		return nil
	}
	now := clock.Unix()
	if v, ok := branchTryAt.Load(c.CorpID); ok && now-v.(int64) < corpBranchRetrySec {
		return nil
	}
	site, ok, err := m.pickBranchSite(ctx, c.CorpID, hq, ch.HQSecFloor)
	if err != nil || !ok {
		branchTryAt.Store(c.CorpID, now)
		return err
	}
	at := estateCandidate{StationID: site.StationID, Name: site.Name, Security: site.Security}
	why := fmt.Sprintf("a branch to recruit from — %d pilot(s) living in the system, %d jump(s) from its nearest office",
		site.Homes, site.Spread)
	rented, err := m.ceoTryRentOffice(ctx, c, facts, ch, at, false, false, why)
	if errors.Is(err, evedb.ErrNoFreeOffices) {
		// Filled since the pick: marked full, so the next try picks another station.
		err = nil
	}
	if err != nil || !rented {
		branchTryAt.Store(c.CorpID, now)
		return err
	}
	branchTryAt.Delete(c.CorpID)
	m.ceoLog(ctx, c, powerEstate, "branch", site.StationID, 0,
		"opened a branch office at %s to widen the corporation's recruiting reach", site.Name)
	return nil
}

// pickBranchSite finds the best system for a branch and the station in it: one with a medical
// bay, most pilots homed there first.
func (m *Mod) pickBranchSite(ctx context.Context, corpID, hq int64, floor float64) (branchSite, bool, error) {
	anchors, err := m.corpAnchorStations(ctx, corpID)
	if err != nil {
		return branchSite{}, false, err
	}
	var sources []int64
	var hqSys int64
	for stationID, a := range anchors {
		sources = append(sources, a.SystemID)
		if stationID == hq {
			hqSys = a.SystemID
		}
	}
	if hqSys == 0 || len(sources) == 0 {
		return branchSite{}, false, nil
	}
	spread, err := m.corpJumpMap(ctx, sources...)
	if err != nil {
		return branchSite{}, false, err
	}
	fromHQ, err := m.corpJumpMap(ctx, hqSys)
	if err != nil {
		return branchSite{}, false, err
	}
	// Candidates: every system with a medical bay (a recruit can wake up there), valued by the
	// roster's home census.
	bays, err := m.medicalStations(ctx)
	if err != nil {
		return branchSite{}, false, err
	}
	u, err := m.universe(ctx)
	if err != nil {
		return branchSite{}, false, err
	}
	crowd, _ := m.homeCrowd(ctx)
	var sites []branchSite
	for sysID := range bays {
		d, ok := fromHQ[sysID]
		if !ok {
			continue
		}
		sp, ok := spread[sysID]
		if !ok {
			sp = -1
		}
		sites = append(sites, branchSite{SystemID: sysID, Homes: crowd[sysID],
			Security: u.meta[sysID].Security, Spread: sp, FromHQ: d})
	}
	ranked := rankBranchSites(sites, floor)
	if len(ranked) > corpBranchSystems {
		ranked = ranked[:corpBranchSystems]
	}
	if len(ranked) == 0 {
		return branchSite{}, false, nil
	}
	// The best station with a medical bay in each system, in one read.
	ids := make([]any, 0, len(ranked)+2)
	ids = append(ids, serviceCloning, corpID)
	for _, s := range ranked {
		ids = append(ids, s.SystemID)
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.solarSystemID, s.stationID, s.stationName,
		       (SELECT COUNT(*) FROM character_ ch WHERE ch.homeStationID = s.stationID) AS homed
		  FROM staStations s
		  JOIN staOperationServices os ON os.operationID = s.operationID AND os.serviceID = ?
		 WHERE NOT EXISTS (SELECT 1 FROM crpoffices o WHERE o.stationID = s.stationID AND o.corporationID = ?)
		   AND s.solarSystemID IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ranked)), ",")+`)
		 ORDER BY homed DESC, s.stationID`, ids...)
	if err != nil {
		return branchSite{}, false, fmt.Errorf("pickBranchSite: %w", err)
	}
	type stationRow struct {
		sys, id int64
		name    string
	}
	var found []stationRow
	for rows.Next() {
		var r stationRow
		var homed int64
		if rows.Scan(&r.sys, &r.id, &r.name, &homed) != nil {
			continue
		}
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return branchSite{}, false, err
	}
	// The most-homed station in each system with a free office slot (officefull.go).
	best := map[int64]stationRow{}
	for _, r := range found {
		if _, seen := best[r.sys]; seen || m.stationOfficesFull(ctx, r.id) {
			continue
		}
		best[r.sys] = r
	}
	for _, s := range ranked {
		if b, ok := best[s.SystemID]; ok {
			s.StationID, s.Name = b.id, b.name
			return s, true, nil
		}
	}
	return branchSite{}, false, nil
}

func init() { registerCorpForget("branchTryAt", func(id int64) { branchTryAt.Delete(id) }) }
