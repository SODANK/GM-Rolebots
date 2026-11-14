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

// The facilities hunt: an industry desk's factory must carry the lines its work
// runs on. A corporation job installs at the station it is posted at
// (corpjobs.go), so build orders need manufacturing lines and the research
// orders postResearchOrders posts need research lines at the same station. With
// no factory, or a factory without research lines, the estate power looks for
// the nearest station with both (an already-rented office first) within
// corpFacilityMaxJumps of the HQ and pins it as the factory
// (rolebot_corp_policy.factoryStationID); the director or deputy carries the
// library over (corpfactorymove.go). A research outfit's lab office is essential
// (rent plus reserve); any other desk's is useful (three reserves).

const (
	// corpFacilityMaxJumps is how far from the headquarters a factory may
	// go: the builders' own reach (corpWorkArmFor refuses work further off).
	corpFacilityMaxJumps = corpWorkMaxJumps
	// corpFacilityHuntSec: a hunt that found nothing, or could not pay for
	// what it found, is not re-run on every think.
	corpFacilityHuntSec = 3600
	// corpFacilityLogSec: "found nowhere" is said once a day.
	corpFacilityLogSec = 24 * 3600
)

// facilityHuntAt remembers, per corporation, the last hunt that came to nothing.
// Memory only, so a restart looks again.
var facilityHuntAt sync.Map // corpID to unix seconds

// facilityNeeds is what a corporation's factory has to carry.
type facilityNeeds struct {
	Mfg       bool // manufacturing lines: any industry desk
	Lab       bool // research lines at the same station: any industry desk
	LabUrgent bool // a research outfit: the lab office is essential
}

// researchTagged reports whether research is one of a corporation's
// purposes rather than a side line of its factory.
func researchTagged(tags []string) bool {
	for _, t := range tags {
		if t == "research" {
			return true
		}
	}
	return false
}

// facilityNeedsFor reads the needs off the tags. Pure.
func facilityNeedsFor(tags []string) facilityNeeds {
	if !hasIndustryDesk(tags) {
		return facilityNeeds{}
	}
	return facilityNeeds{Mfg: true, Lab: true, LabUrgent: researchTagged(tags)}
}

// stationHasLab reports whether a station has material-research lines: the
// first research order the desk posts, and a marker for the rest (every station
// with ME lines has PE and copy lines).
func (m *Mod) stationHasLab(ctx context.Context, stationID int64) bool {
	return m.labStationHasLine(ctx, stationID, ramActivityMECorp)
}

// facilitySite is one station the factory could move to.
type facilitySite struct {
	estateCandidate
	SystemID int64
	Jumps    int // from the headquarters (or the members, with no HQ)
	Lines    int // manufacturing lines
}

// rankFacilities orders sites for a factory. Pure. Hard doors: the security
// floor, manufacturing lines, research lines when asked, the builders' reach.
// Then an office already rented, then distance (two jumps forgiven for a
// station members live at), then more lines.
func rankFacilities(sites []facilitySite, floor float64, needLab bool, maxJumps int) []facilitySite {
	var out []facilitySite
	for _, s := range sites {
		if s.Security < floor || !s.HasLine || (needLab && !s.HasLab) || s.Jumps < 0 || s.Jumps > maxJumps ||
			(s.Full && !s.HasOffice) {
			continue
		}
		out = append(out, s)
	}
	key := func(s facilitySite) float64 {
		k := float64(s.Jumps) - math.Min(float64(s.Members), 4)/2
		if s.HasOffice {
			k -= 1000
		}
		return k
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ki, kj := key(out[i]), key(out[j]); ki != kj {
			return ki < kj
		}
		if out[i].Lines != out[j].Lines {
			return out[i].Lines > out[j].Lines
		}
		return out[i].StationID < out[j].StationID
	})
	return out
}

// facilityFloor is the security floor for a factory: the CEO's own, never
// above the headquarters' system (factorySecFloor).
func (m *Mod) facilityFloor(ctx context.Context, ch Charter, hq int64) float64 {
	floor := ch.HQSecFloor
	var hqSec float64
	if err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(ss.security, 0) FROM staStations s
		  JOIN mapSolarSystems ss ON ss.solarSystemID = s.solarSystemID
		 WHERE s.stationID = ?`, hq).Scan(&hqSec); err == nil {
		floor = factorySecFloor(floor, hqSec)
	}
	return floor
}

// facilitySites lists every station with manufacturing lines within reach of the
// headquarters (or of where members live, with no HQ), with distance, lines,
// labs, members and whether the corporation rents there. The jump map is walked
// first so the line table is read only for systems in reach.
func (m *Mod) facilitySites(ctx context.Context, corpID, hq int64, cands []estateCandidate, floor float64) ([]facilitySite, error) {
	members := map[int64]int{}
	for _, c := range cands {
		members[c.StationID] = c.Members
	}
	var sources []int64
	if hq != 0 {
		if sys, err := evedb.GetSolarSystemForStation(ctx, m.db, hq); err == nil && sys != 0 {
			sources = append(sources, sys)
		}
	}
	if len(sources) == 0 {
		for _, c := range cands {
			if sys, err := evedb.GetSolarSystemForStation(ctx, m.db, c.StationID); err == nil && sys != 0 {
				sources = append(sources, sys)
			}
		}
	}
	if len(sources) == 0 {
		return nil, nil
	}
	dist, err := m.corpJumpMap(ctx, sources...)
	if err != nil {
		return nil, err
	}
	var near []any
	for sys, d := range dist {
		if d <= corpFacilityMaxJumps {
			near = append(near, sys)
		}
	}
	if len(near) == 0 {
		return nil, nil
	}
	args := append([]any{corpID, floor}, near...)
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.stationName, COALESCE(ss.regionID, 0), COALESCE(ss.security, 0),
		       s.solarSystemID,
		       SUM(l.activityID = 1), SUM(l.activityID = ?),
		       (SELECT COUNT(*) FROM crpoffices o WHERE o.stationID = s.stationID AND o.corporationID = ?)
		  FROM staStations s
		  JOIN mapSolarSystems ss ON ss.solarSystemID = s.solarSystemID
		  JOIN ramassemblylines l ON l.containerID = s.stationID
		 WHERE COALESCE(ss.security, 0) >= ?
		   AND s.solarSystemID IN (`+strings.TrimSuffix(strings.Repeat("?,", len(near)), ",")+`)
		 GROUP BY s.stationID, s.stationName, ss.regionID, ss.security, s.solarSystemID
		HAVING SUM(l.activityID = 1) > 0`,
		append([]any{ramActivityMECorp}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("facilitySites: %w", err)
	}
	defer rows.Close()
	var out []facilitySite
	for rows.Next() {
		var s facilitySite
		var labs, office int
		if err := rows.Scan(&s.StationID, &s.Name, &s.RegionID, &s.Security, &s.SystemID,
			&s.Lines, &labs, &office); err != nil {
			return nil, err
		}
		s.HasLine = s.Lines > 0
		s.HasLab = labs > 0
		s.HasOffice = office > 0
		s.Members = members[s.StationID]
		s.Jumps = dist[s.SystemID]
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Only stations that pass the hard doors are worth a slot count.
	for i := range out {
		s := &out[i]
		if !s.HasOffice && s.HasLine && s.Jumps <= corpFacilityMaxJumps {
			s.Full = m.stationOfficesFull(ctx, s.StationID)
		}
	}
	return out, nil
}

// ceoSecureFacilities is the estate power's factory step. acted = true means
// it rented or re-pinned this think and the rest of estate waits.
func (m *Mod) ceoSecureFacilities(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter,
	cands []estateCandidate, hq int64) (bool, error) {
	needs := facilityNeedsFor(facts.Policy.Tags)
	if !needs.Mfg {
		return false, nil
	}
	factory := m.corpFactoryStation(ctx, c.CorpID)
	if factory != 0 {
		m.ensureFactoryPinned(ctx, c.CorpID, factory)
		m.noteStrandedDesk(ctx, c, factory)
	}
	hasLab := factory != 0 && m.stationHasLab(ctx, factory)
	if factory != 0 && (hasLab || !needs.Lab) {
		return false, nil
	}
	now := clock.Unix()
	if v, ok := facilityHuntAt.Load(c.CorpID); ok && now-v.(int64) < corpFacilityHuntSec {
		return false, nil
	}
	floor := m.facilityFloor(ctx, ch, hq)
	sites, err := m.facilitySites(ctx, c.CorpID, hq, cands, floor)
	if err != nil {
		return false, err
	}
	var pick *facilitySite
	withLab := true
	if r := rankFacilities(sites, floor, true, corpFacilityMaxJumps); len(r) > 0 {
		pick = &r[0]
	} else if factory == 0 {
		// Somewhere to build beats nowhere; the lab hunt resumes once there
		// is a factory to move.
		if r := rankFacilities(sites, floor, false, corpFacilityMaxJumps); len(r) > 0 {
			pick, withLab = &r[0], false
		}
	}
	what := "manufacturing and research lines"
	if !withLab {
		what = "manufacturing lines"
	}
	if pick == nil {
		facilityHuntAt.Store(c.CorpID, now)
		if now-m.lastCEOActionAt(ctx, c.CorpID, "nofacility") >= corpFacilityLogSec {
			m.ceoLog(ctx, c, powerEstate, "nofacility", factory, 0,
				"found no station with %s within %d jumps of its headquarters at %.1f security or better",
				what, corpFacilityMaxJumps, floor)
		}
		return false, nil
	}
	if pick.HasOffice {
		return true, m.pinFactory(ctx, c, factory, *pick, what)
	}
	essential := factory == 0 || needs.LabUrgent
	why := fmt.Sprintf("a factory with %s, %d jump(s) from the headquarters", what, pick.Jumps)
	if factory != 0 {
		why = fmt.Sprintf("its factory at %s has no research lines; this one has both, %d jump(s) from the headquarters",
			stationNameOf(ctx, m, factory), pick.Jumps)
	}
	rented, err := m.ceoTryRentOffice(ctx, c, facts, ch, pick.estateCandidate, false, essential, why)
	if errors.Is(err, evedb.ErrNoFreeOffices) {
		// Filled since the slot count: marked full, so the next hunt ranks it out.
		facilityHuntAt.Store(c.CorpID, now)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !rented {
		facilityHuntAt.Store(c.CorpID, now)
		return false, nil
	}
	return true, m.pinFactory(ctx, c, factory, *pick, what)
}

// pinFactory makes a station the corporation's factory and, when that moves it
// off another, opens the move of the old office's stock and takes the
// corporation's bids there off the book, since a bid left at the old factory
// fills an office the desk no longer reads.
func (m *Mod) pinFactory(ctx context.Context, c *corpCEO, oldStation int64, at facilitySite, what string) error {
	forgetCorpFactory(c.CorpID)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_policy (corporationID, factoryStationID, updatedAt) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE factoryStationID = VALUES(factoryStationID), updatedAt = VALUES(updatedAt)`,
		c.CorpID, at.StationID, clock.Unix()); err != nil {
		return fmt.Errorf("pin factory: %w", err)
	}
	facilityHuntAt.Delete(c.CorpID)
	if oldStation == 0 || oldStation == at.StationID {
		m.ceoLog(ctx, c, powerEstate, "factory", at.StationID, 0,
			"made %s its factory (%s)", at.Name, what)
		return nil
	}
	if officeID, _, _ := evedb.GetCorpOfficeAt(ctx, m.db, c.CorpID, oldStation); officeID != 0 && officeID != oldStation {
		m.openFactoryMove(ctx, c.CorpID, officeID, oldStation)
	}
	m.cancelCorpBidsAt(ctx, c, oldStation)
	m.ceoLog(ctx, c, powerEstate, "factory", at.StationID, 0,
		"moved its factory from %s to %s (%s) — the desk's stock follows",
		stationNameOf(ctx, m, oldStation), at.Name, what)
	return nil
}

// ensureFactoryPinned pins whatever corpFactoryStation answers, silently:
// idempotent while the pin is good, a repair when the pinned office is gone. It
// stops a branch office at a busy hub (corpbranch.go) taking over as the
// factory away from the library.
func (m *Mod) ensureFactoryPinned(ctx context.Context, corpID, factory int64) {
	forgetCorpFactory(corpID)
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_policy (corporationID, factoryStationID, updatedAt) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE factoryStationID = VALUES(factoryStationID)`,
		corpID, factory, clock.Unix()); err != nil {
		rlogf("rolebots: pin factory of %d: %v", corpID, err)
	}
}

// noteStrandedDesk opens a move for any office that is not the factory, nor
// another base, but holds the library's prints (a factory moved earlier, or a
// job that finished at the old station after the move closed).
func (m *Mod) noteStrandedDesk(ctx context.Context, c *corpCEO, factory int64) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT o.itemID, o.stationID
		  FROM crpoffices o
		  JOIN rolebot_corp_library l ON l.corporationID = o.corporationID AND l.stationID = o.stationID
		 WHERE o.corporationID = ? AND o.stationID <> ?`, c.CorpID, factory)
	if err != nil {
		return
	}
	type office struct{ id, station int64 }
	var list []office
	for rows.Next() {
		var o office
		if rows.Scan(&o.id, &o.station) == nil {
			list = append(list, o)
		}
	}
	rows.Close()
	// An R&D base keeps its share of the library (corpbases.go); only an office that
	// is no base of anything is a stranded desk.
	keep := map[int64]bool{}
	for _, st := range m.armBaseStations(ctx, c.CorpID, armResearch) {
		keep[st] = true
	}
	for _, st := range m.armBaseStations(ctx, c.CorpID, armIndustry) {
		keep[st] = true
	}
	for _, o := range list {
		if keep[o.station] {
			continue
		}
		m.openFactoryMove(ctx, c.CorpID, o.id, o.station)
	}
}

// cancelCorpBidsAt takes the corporation's open bids at one station off the
// book, escrow back to the corporation. What they already bought is booked
// first (sweepCorpBids), or the ledger would lose it with the row.
func (m *Mod) cancelCorpBidsAt(ctx context.Context, c *corpCEO, stationID int64) {
	m.sweepCorpBids(ctx, c)
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.orderID, o.charID FROM rolebot_corp_orders r
		  JOIN market_orders o ON o.orderID = r.orderID
		 WHERE r.corporationID = ? AND r.stationID = ? AND r.bid = 1 AND o.orderState = 1`,
		c.CorpID, stationID)
	if err != nil {
		return
	}
	type order struct{ id, char int64 }
	var list []order
	for rows.Next() {
		var o order
		if rows.Scan(&o.id, &o.char) == nil {
			list = append(list, o)
		}
	}
	rows.Close()
	for _, o := range list {
		if _, err := evedb.CancelMarketOrder(ctx, m.db, o.char, o.id); err != nil {
			rlogf("rolebots: cancel corp bid %d at old factory: %v", o.id, err)
			continue
		}
		m.forgetCorpOrder(ctx, o.id)
	}
}

func init() { registerCorpForget("facilityHuntAt", func(id int64) { facilityHuntAt.Delete(id) }) }
