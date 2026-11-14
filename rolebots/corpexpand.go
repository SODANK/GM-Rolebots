package rolebots

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	evedb "gurimoon/db"
)

// Expansion: a department that has outgrown its base takes another. The trigger
// is base pressure (corpbasepressure.go): members refused because every line of
// their kind is queued out, or the department's jobs waiting too long for a line.
// R&D needs research lines (Industry does not follow it there until it runs out
// of its own); Industry needs manufacturing lines and grows first into an office
// already held. Cheapest first: claim the purpose on an office already rented,
// else rent the nearest station with the lines within expandMaxJumps whose lines
// are not queued out. An expansion may run the office count one past the cap, and
// its rent ranks with the factory's (corpoffices.go). Switches:
// rolebots.corp.labExpand and rolebots.corp.factoryExpand, both default on.

const (
	// expandMaxJumps bounds the search from the base that filled up.
	expandMaxJumps = 5
	// expandCooldownSec is the least time between two expansions of one
	// department: the new base needs members posted and the library split
	// before its own pressure says anything.
	expandCooldownSec = 3 * 86400
	// expandRankLines is how many of the nearest candidates have their
	// queues read before the pick.
	expandRankLines = 8
)

// expandSite is one station a department could grow into.
type expandSite struct {
	StationID int64
	Name      string
	SystemID  int64
	RegionID  int64
	Security  float64
	Held      bool // the corporation already rents an office there
	HasLab    bool
	HasMfg    bool
	Jumps     int
	Free      int  // free lines of the department's kind
	QueuedOut bool // every line of the kind queued past industryQueueCapSec
}

// pickExpansion chooses where a department grows. Hard doors: the lines it
// needs, the security floor, the reach, and not queued out itself. Then an
// office already held beats one to rent; then the nearest; then the most free
// lines. Pure.
func pickExpansion(arm corpArm, sites []expandSite, floor float64, maxJumps int) (expandSite, bool) {
	var ok []expandSite
	for _, s := range sites {
		switch arm {
		case armResearch:
			if !s.HasLab {
				continue
			}
		case armIndustry:
			if !s.HasMfg {
				continue
			}
		default:
			continue
		}
		if s.QueuedOut || s.Jumps < 0 || s.Jumps > maxJumps || (!s.Held && s.Security < floor) {
			continue
		}
		ok = append(ok, s)
	}
	if len(ok) == 0 {
		return expandSite{}, false
	}
	sort.SliceStable(ok, func(i, j int) bool {
		a, b := ok[i], ok[j]
		if a.Held != b.Held {
			return a.Held
		}
		if a.Jumps != b.Jumps {
			return a.Jumps < b.Jumps
		}
		if a.Free != b.Free {
			return a.Free > b.Free
		}
		return a.StationID < b.StationID
	})
	return ok[0], true
}

// expandVerb is the CEO ledger verb for one department's expansion.
func expandVerb(arm corpArm) string { return "expand." + string(arm) }

// ceoExpandBases is the estate step: at most one department grows per think.
func (m *Mod) ceoExpandBases(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, hq int64) (bool, error) {
	if facts == nil {
		return false, nil
	}
	m.prunePressure(ctx, c.CorpID)
	for _, arm := range []corpArm{armResearch, armIndustry} {
		if arm == armResearch && !labExpandOn() || arm == armIndustry && !factoryExpandOn() {
			continue
		}
		acted, err := m.expandArm(ctx, c, facts, ch, hq, arm)
		if err != nil || acted {
			return acted, err
		}
	}
	return false, nil
}

// expandArm grows one department when one of its bases is full.
func (m *Mod) expandArm(ctx context.Context, c *corpCEO, facts *CorpFacts, ch Charter, hq int64, arm corpArm) (bool, error) {
	if armCount(c.CorpID, arm) == 0 {
		return false, nil
	}
	now := clock.Unix()
	if now-m.lastCEOActionAt(ctx, c.CorpID, expandVerb(arm)) < expandCooldownSec {
		return false, nil
	}
	bases := m.armBaseStations(ctx, c.CorpID, arm)
	if len(bases) == 0 {
		return false, nil // the facility and lab hunts come first
	}
	kind := pressureLab
	if arm == armIndustry {
		kind = pressureMfg
	}
	press, err := m.basePressures(ctx, c.CorpID, kind)
	if err != nil {
		return false, err
	}
	isBase := map[int64]bool{}
	var full int64
	var fullP basePressure
	for _, st := range bases {
		isBase[st] = true
		if p := press[st]; p.Full && (full == 0 || fullerBase(p, fullP)) {
			full, fullP = st, p
		}
	}
	var why string
	if full != 0 {
		why = pressureWhy(arm, fullP, stationNameOf(ctx, m, full))
	} else if arm == armIndustry {
		// Station lines rarely queue out, so Industry also grows on load: a
		// base with more queued production per line, or more posted members,
		// than one office should carry.
		var load float64
		var posted int64
		full, load, posted = m.overloadedIndustryBase(ctx, c.CorpID)
		if full != 0 {
			why = loadWhy(stationNameOf(ctx, m, full), load, posted)
		}
	}
	if full == 0 {
		return false, nil
	}
	sites, err := m.expandSites(ctx, c.CorpID, arm, full, isBase)
	if err != nil {
		return false, err
	}
	floor := m.facilityFloor(ctx, ch, hq)
	pick, ok := pickExpansion(arm, sites, floor, expandMaxJumps)
	if !ok {
		if now-m.lastCEOActionAt(ctx, c.CorpID, "noexpand") >= corpFacilityLogSec {
			m.ceoLog(ctx, c, powerEstate, "noexpand", full, 0,
				"%s; found no station within %d jumps with free %s lines", why, expandMaxJumps, kindNoun(kind))
		}
		return false, nil
	}
	bit := armBaseBit(arm)
	if pick.Held {
		m.claimOfficePurpose(ctx, c.CorpID, pick.StationID, bit)
		m.ceoLog(ctx, c, powerEstate, expandVerb(arm), pick.StationID, 0,
			"%s; %s now works out of its office at %s too", why, armLabel(arm), pick.Name)
		return true, nil
	}
	if m.officeCount(ctx, c.CorpID) >= officeCap(facts.Members)+1 {
		if now-m.lastCEOActionAt(ctx, c.CorpID, "noexpand") >= corpFacilityLogSec {
			m.ceoLog(ctx, c, powerEstate, "noexpand", full, 0,
				"%s; it holds as many offices as a corporation of %d may", why, facts.Members)
		}
		return false, nil
	}
	at := estateCandidate{StationID: pick.StationID, Name: pick.Name, RegionID: pick.RegionID, Security: pick.Security,
		HasLine: pick.HasMfg, HasLab: pick.HasLab}
	rented, err := m.ceoTryRentOffice(ctx, c, facts, ch, at, false, true,
		fmt.Sprintf("a new %s base, %d jump(s) on", armLabel(arm), pick.Jumps))
	if errors.Is(err, evedb.ErrNoFreeOffices) {
		return false, nil
	}
	if err != nil || !rented {
		return false, err
	}
	m.claimOfficePurpose(ctx, c.CorpID, pick.StationID, bit)
	m.ceoLog(ctx, c, powerEstate, expandVerb(arm), pick.StationID, 0,
		"%s; rented %s as a new %s base", why, pick.Name, armLabel(arm))
	return true, nil
}

// fullerBase orders two full bases: a refused one first (a refusal is a job
// that never started at all), then by the measured wait, then by refusals.
// Pure.
func fullerBase(a, b basePressure) bool {
	if a.ByRefusal != b.ByRefusal {
		return a.ByRefusal
	}
	if a.ByRefusal {
		return a.Refusals > b.Refusals
	}
	return a.Wait.ratio() > b.Wait.ratio()
}

// pressureWhy words a full base for the CEO ledger: which input filled it.
// Pure.
func pressureWhy(arm corpArm, p basePressure, station string) string {
	if p.ByRefusal {
		return fmt.Sprintf("%s was refused %d time(s) by %d member(s) at %s in three days — every line there is queued out",
			armLabel(arm), p.Refusals, p.Pilots, station)
	}
	return fmt.Sprintf("%s's %d %s job(s) at %s waited %.1f h on average against %.1f h of work in three days",
		armLabel(arm), p.Wait.Jobs, activityLineNoun(p.Wait.ActivityID), station,
		p.Wait.AvgWaitSec/3600, p.Wait.AvgRunSec/3600)
}

// overloadedIndustryBase is the Industry base most over the load dials
// (rolebots.corp.expandLoadHours, rolebots.corp.expandMembersPerBase), 0 for
// none, with its queued seconds per line and its posted Industry members.
func (m *Mod) overloadedIndustryBase(ctx context.Context, corpID int64) (int64, float64, int64) {
	open, err := m.openWorkFor(ctx, corpID)
	if err != nil {
		return 0, 0, 0
	}
	bases := m.buildBases(ctx, corpID, open)
	if len(bases) == 0 {
		return 0, 0, 0
	}
	posted := map[int64]int64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, COUNT(*) FROM rolebot_corp_postings
		 WHERE corporationID = ? AND arm = ? GROUP BY stationID`, corpID, string(armIndustry))
	if err == nil {
		for rows.Next() {
			var st, n int64
			if rows.Scan(&st, &n) == nil {
				posted[st] = n
			}
		}
		rows.Close()
	}
	st := overloadedBase(bases, posted, float64(expandLoadSec()), expandMembersPerBase())
	if st == 0 {
		return 0, 0, 0
	}
	for _, b := range bases {
		if b.StationID == st {
			return st, b.perLine(), posted[st]
		}
	}
	return st, 0, posted[st]
}

// overloadedBase picks the base over either dial, the one with the most
// queued production per line first. Pure.
func overloadedBase(bases []buildBase, posted map[int64]int64, loadSec float64, maxMembers int64) int64 {
	var best int64
	bestLoad := -1.0
	for _, b := range bases {
		if b.perLine() <= loadSec && posted[b.StationID] <= maxMembers {
			continue
		}
		if b.perLine() > bestLoad {
			best, bestLoad = b.StationID, b.perLine()
		}
	}
	return best
}

// loadWhy is the ledger reason for a load expansion.
func loadWhy(station string, perLineSec float64, posted int64) string {
	return fmt.Sprintf("Industry at %s carries %.0f h of queued production a line and %d posted member(s)",
		station, perLineSec/3600, posted)
}

// kindNoun names a pressure kind's lines.
func kindNoun(kind string) string {
	if kind == pressureMfg {
		return "manufacturing"
	}
	return "research"
}

// expandSites lists where a department could grow from a full base: the
// corporation's other offices, and every station within expandMaxJumps with
// the department's lines. The nearest few have their queues read.
func (m *Mod) expandSites(ctx context.Context, corpID int64, arm corpArm, from int64, isBase map[int64]bool) ([]expandSite, error) {
	fromSys := m.stationSystemID(ctx, from)
	dist, err := m.corpJumpMap(ctx, fromSys)
	if err != nil {
		return nil, err
	}
	var near []any
	for sys, d := range dist {
		if d <= expandMaxJumps {
			near = append(near, sys)
		}
	}
	act := int64(ramActivityMEResearch)
	if arm == armIndustry {
		act = ramActivityManufacturing
	}
	held := map[int64]bool{}
	if ids, err := int64Column(ctx, m, `
		SELECT o.stationID FROM crpoffices o
		  LEFT JOIN rolebot_corp_offices r ON r.corporationID = o.corporationID AND r.stationID = o.stationID
		 WHERE o.corporationID = ? AND COALESCE(r.state, 'open') <> 'dropping'`, corpID); err == nil {
		for _, id := range ids {
			held[id] = true
		}
	}
	var heldArgs []any
	for st := range held {
		if !isBase[st] {
			heldArgs = append(heldArgs, st)
		}
	}
	var where []string
	var args []any
	if len(near) > 0 {
		where = append(where, `(s.solarSystemID IN (`+placeholders(len(near))+`)
			AND EXISTS (SELECT 1 FROM ramassemblylines x WHERE x.containerID = s.stationID AND x.activityID = ?))`)
		args = append(args, near...)
		args = append(args, act)
	}
	if len(heldArgs) > 0 {
		where = append(where, `s.stationID IN (`+placeholders(len(heldArgs))+`)`)
		args = append(args, heldArgs...)
	}
	if len(where) == 0 {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.stationName, s.solarSystemID, COALESCE(ss.regionID, 0), COALESCE(ss.security, 0),
		       (SELECT COUNT(*) FROM ramassemblylines l WHERE l.containerID = s.stationID AND l.activityID = 1),
		       (SELECT COUNT(*) FROM ramassemblylines l WHERE l.containerID = s.stationID AND l.activityID = ?)
		  FROM staStations s
		  JOIN mapSolarSystems ss ON ss.solarSystemID = s.solarSystemID
		 WHERE `+strings.Join(where, " OR "), append([]any{ramActivityMEResearch}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("expandSites: %w", err)
	}
	var out []expandSite
	for rows.Next() {
		var s expandSite
		var mfg, lab int
		if rows.Scan(&s.StationID, &s.Name, &s.SystemID, &s.RegionID, &s.Security, &mfg, &lab) != nil {
			continue
		}
		if isBase[s.StationID] {
			continue
		}
		s.HasMfg, s.HasLab = mfg > 0, lab > 0
		s.Held = held[s.StationID]
		if d, ok := dist[s.SystemID]; ok {
			s.Jumps = d
		} else {
			s.Jumps = -1
		}
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Read the queues of the nearest few, held offices first.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Held != out[j].Held {
			return out[i].Held
		}
		return out[i].Jumps < out[j].Jumps
	})
	for i := range out {
		if i >= expandRankLines {
			out[i].QueuedOut = true // unread: never picked over one that was
			continue
		}
		use, err := m.stationLineUse(ctx, corpID, out[i].StationID)
		if err != nil {
			continue
		}
		b := corpBase{Lines: use}
		u := b.mfgLines()
		if arm == armResearch {
			u = b.labLines()
		}
		out[i].Free = u.Free
		out[i].QueuedOut = u.Lines == 0 || (u.Free == 0 && u.Next > industryQueueCapSec)
	}
	return out, nil
}
