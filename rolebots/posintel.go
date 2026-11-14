package rolebots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

// Finding moons: surveys, not omniscience. The moon table knows what every moon holds
// (db/moondb.go) and a bot corporation never reads it. It knows only what it surveyed:
// rolebot_corp_moon_intel, one row per moon a member probed, written only by the survey
// errand below. Corporations at mutual friend standing or better (corprel.go) read each
// other's intel.
// A survey uses real consumables: the surveyor carries survey probes from the corporation's
// kit (one per moon; the core's survey resolves the nearest moon within scanRange), flies to
// the system and spends the probe's flight time per moon. Highsec is never surveyed: a
// harvester cannot be anchored there (attribute 1032, 0.4 and below).

const (
	typeSurveyProbe   = 18626 // Quest Survey Probe I
	typeProbeLauncher = 17938 // Scan Probe Launcher I

	surveyMoonsPerOrder = 14
	surveyOpenMax       = 3
	surveyClaimSec      = 8 * 3600
	surveyStaleSec      = 5 * 86400
	// surveyWageBase is what a member is paid per moon; an R32+ find pays
	// surveyBonusMul times that.
	surveyWageBase  = 12500
	surveyBonusMul  = 4
	surveyMinDelay  = 20
	surveyIntelKeep = 60 * 86400 // intel this old is re-surveyed before it is trusted
	// surveyRetrySec is how long a system whose order lapsed unfinished waits
	// before it is ordered again.
	surveyRetrySec = 2 * 86400
	// surveyWantUsable is how many usable moons (usableMoonCount) in reach a
	// corporation looking for a tower wants on its list; below it, it keeps
	// surveying.
	surveyWantUsable = 3
	// harvestUnitsPerHour is what one Harvester I takes per material per cycle (quality 1 x 100
	// units, db.HarvestUnitsPerCycle).
	harvestUnitsPerHour = 100
)

// moonIntel is one surveyed moon.
type moonIntel struct {
	CorpID     int64
	MoonID     int64
	SystemID   int64
	Materials  []moonMat
	BestClass  int
	Value      float64
	SurveyedAt int64
	SurveyorID int64
}

type moonMat struct {
	TypeID int64 `json:"t"`
	Qty    int64 `json:"q"`
	Class  int   `json:"c"`
}

// materialsByValue orders a moon's materials richest first by unit value.
func materialsByValue(mats []moonMat, unit map[int64]float64) []int64 {
	s := append([]moonMat(nil), mats...)
	sort.SliceStable(s, func(i, j int) bool {
		vi, vj := unit[s[i].TypeID], unit[s[j].TypeID]
		if vi != vj {
			return vi > vj
		}
		return s[i].TypeID < s[j].TypeID
	})
	out := make([]int64, 0, len(s))
	for _, m := range s {
		out = append(out, m.TypeID)
	}
	return out
}

// moonHourValue is what n harvesters on the n richest materials make an hour.
// Pure.
func moonHourValue(mats []moonMat, unit map[int64]float64, n int) float64 {
	vals := make([]float64, 0, len(mats))
	for _, m := range mats {
		vals = append(vals, unit[m.TypeID]*harvestUnitsPerHour)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(vals)))
	total := 0.0
	for i := 0; i < n && i < len(vals); i++ {
		total += vals[i]
	}
	return total
}

// intelFriendsOf is every corporation whose intel this one reads: itself, the corporations at
// mutual friend standing or better, and, with the alliance sov switch on, its own alliance.
func intelFriendsOf(corpID int64) []int64 {
	out := []int64{corpID}
	s := loadRelSnapshot()
	if s == nil || corpID == 0 {
		return out
	}
	seen := map[int64]bool{corpID: true}
	for k, v := range s.corp {
		if k[0] != corpID || v < StandingLiked {
			continue
		}
		if s.corp[[2]int64{k[1], corpID}] >= StandingLiked && !seen[k[1]] {
			seen[k[1]] = true
			out = append(out, k[1])
		}
	}
	if ally := s.ally[corpID]; ally != 0 {
		for corp, a := range s.ally {
			if a == ally && !seen[corp] {
				seen[corp] = true
				out = append(out, corp)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func int64CSV(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// corpMoonIntel reads every moon a corporation (and its friends) knows,
// newest survey per moon, best first.
func (m *Mod) corpMoonIntel(ctx context.Context, corpID int64, minClass int) ([]moonIntel, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT corporationID, moonID, systemID, COALESCE(results, ''), bestClass, value, surveyedAt, surveyorID
		  FROM rolebot_corp_moon_intel
		 WHERE corporationID IN (`+int64CSV(intelFriendsOf(corpID))+`) AND bestClass >= ?
		 ORDER BY value DESC, surveyedAt DESC`, minClass)
	if err != nil {
		return nil, fmt.Errorf("corpMoonIntel: %w", err)
	}
	defer rows.Close()
	seen := map[int64]bool{}
	var out []moonIntel
	for rows.Next() {
		var mi moonIntel
		var raw string
		if err := rows.Scan(&mi.CorpID, &mi.MoonID, &mi.SystemID, &raw, &mi.BestClass, &mi.Value,
			&mi.SurveyedAt, &mi.SurveyorID); err != nil {
			return nil, err
		}
		if seen[mi.MoonID] {
			continue
		}
		seen[mi.MoonID] = true
		_ = json.Unmarshal([]byte(raw), &mi.Materials)
		out = append(out, mi)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	m.revalueMoonIntel(ctx, out)
	return out, nil
}

// revalueMoonIntel prices every moon at today's values. The stored value is what the moon was
// worth when surveyed, and goo prices move with the NPC bid book. One TypeValues call over the
// union of materials; on a failure the stored values stand.
func (m *Mod) revalueMoonIntel(ctx context.Context, list []moonIntel) {
	seen := map[int64]bool{}
	var ids []int64
	for _, mi := range list {
		for _, mm := range mi.Materials {
			if !seen[mm.TypeID] {
				seen[mm.TypeID] = true
				ids = append(ids, mm.TypeID)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	vals, err := evedb.TypeValues(ctx, m.db, ids)
	if err != nil {
		return
	}
	unit := make(map[int64]float64, len(ids))
	for _, id := range ids {
		unit[id] = vals[id].Value
	}
	revalueMoons(list, unit)
}

// revalueMoons re-prices each moon from its materials and re-sorts the list
// best first (newest survey breaking ties), as corpMoonIntel promises. Pure.
func revalueMoons(list []moonIntel, unit map[int64]float64) {
	for i := range list {
		if len(list[i].Materials) == 0 {
			continue
		}
		list[i].Value = moonHourValue(list[i].Materials, unit, harvestersFor(sizeForMoonClass(list[i].BestClass)))
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Value != list[j].Value {
			return list[i].Value > list[j].Value
		}
		return list[i].SurveyedAt > list[j].SurveyedAt
	})
}

// recordMoonIntel writes one survey result.
func (m *Mod) recordMoonIntel(ctx context.Context, corpID, moonID, systemID, surveyorID int64) (moonIntel, error) {
	mi := moonIntel{CorpID: corpID, MoonID: moonID, SystemID: systemID, SurveyorID: surveyorID,
		BestClass: -1, SurveyedAt: clock.Unix()}
	res, err := evedb.GetMoonResources(ctx, m.db, moonID)
	if err != nil {
		return mi, err
	}
	ids := make([]int64, 0, len(res))
	for _, r := range res {
		c := evedb.MoonClassOf(r.TypeID)
		mi.Materials = append(mi.Materials, moonMat{TypeID: r.TypeID, Qty: r.Quantity, Class: c})
		if c > mi.BestClass {
			mi.BestClass = c
		}
		ids = append(ids, r.TypeID)
	}
	if len(ids) > 0 {
		vals, err := evedb.TypeValues(ctx, m.db, ids)
		if err == nil {
			unit := map[int64]float64{}
			for _, id := range ids {
				unit[id] = vals[id].Value
			}
			mi.Value = moonHourValue(mi.Materials, unit, harvestersFor(sizeForMoonClass(mi.BestClass)))
		}
	}
	raw, _ := json.Marshal(mi.Materials)
	_, err = m.db.ExecContext(ctx, `
		INSERT INTO rolebot_corp_moon_intel (corporationID, moonID, systemID, results, bestClass, value, surveyedAt, surveyorID)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE results = VALUES(results), bestClass = VALUES(bestClass), value = VALUES(value),
			surveyedAt = VALUES(surveyedAt), surveyorID = VALUES(surveyorID), systemID = VALUES(systemID)`,
		corpID, moonID, systemID, string(raw), mi.BestClass, mi.Value, mi.SurveyedAt, surveyorID)
	return mi, err
}

// ── posting surveys ─────────────────────────────────────────────────────────

// surveyCand is one system worth surveying for a corporation: low or null security (highsec
// cannot be harvested), near its staging office, not surveyed by it or a friend within
// surveyIntelKeep. Danger is priced later (posceo.go towerDanger), not here.
type surveyCand struct {
	SystemID int64
	Jumps    int
	Security float64
	Moons    int
	Frontier bool // unclaimed 0.0 (unclaimedNullSystems)
	Score    float64
}

// surveyFrontierDepth and surveyFrontierJumpCost weigh unclaimed 0.0 in the survey hunt
// (against 1.6 for other 0.0 and 0.3 a jump): a frontier system fifteen jumps out outscores
// lowsec two jumps from the office.
const (
	surveyFrontierDepth    = 2.2
	surveyFrontierJumpCost = 0.08
)

// pullSurveyCands applies a survey round's opts to the ranked candidates: only unclaimed 0.0 with
// FrontierOnly, and the alliance's systems scored surveyPullMul higher, ranked again. Pure.
func pullSurveyCands(cands []surveyCand, opts surveyOpts) []surveyCand {
	if !opts.FrontierOnly && len(opts.Pull) == 0 {
		return cands
	}
	out := cands[:0:0]
	for _, cd := range cands {
		if opts.FrontierOnly && !cd.Frontier {
			continue
		}
		if opts.Pull[cd.SystemID] {
			cd.Score *= surveyPullMul
		}
		out = append(out, cd)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

func scoreSurveyCand(c surveyCand) float64 {
	if c.Security >= 0.45 || c.Moons == 0 {
		return 0
	}
	depth, jumpCost := 1.0, 0.3
	switch {
	case c.Frontier:
		depth, jumpCost = surveyFrontierDepth, surveyFrontierJumpCost
	case c.Security < 0:
		depth = 1.6
	case c.Security < 0.25:
		depth = 1.25
	}
	return depth * math.Min(float64(c.Moons), surveyMoonsPerOrder) / (1 + jumpCost*float64(c.Jumps))
}

// surveyCandidates is every system in reach still worth an order, best first: below highsec,
// no order out (busy), and with moons nobody friendly has surveyed. Moons is what is left
// (moons less covered), so a system one order in still scores on its remaining two thirds.
// Ties go to the nearer system, then the lower ID. frontier marks unclaimed 0.0 systems. Pure.
func surveyCandidates(reach map[int64]int, meta map[int64]sysMeta, busy map[int64]bool,
	moons, covered map[int64]int, frontier map[int64]bool) []surveyCand {
	var out []surveyCand
	for sys, j := range reach {
		mt, ok := meta[sys]
		if !ok || busy[sys] || mt.Security >= 0.45 {
			continue
		}
		left := moons[sys] - covered[sys]
		if left <= 0 {
			continue
		}
		cd := surveyCand{SystemID: sys, Jumps: j, Security: mt.Security, Moons: left,
			Frontier: frontier[sys] && mt.Security < 0}
		cd.Score = scoreSurveyCand(cd)
		out = append(out, cd)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Jumps != out[j].Jumps {
			return out[i].Jumps < out[j].Jumps
		}
		return out[i].SystemID < out[j].SystemID
	})
	return out
}

// usableMoonCount is how many moons in a corporation's intel a mining tower could actually go
// on: unoccupied and worth the mining floor. It decides whether to keep surveying, because a
// corporation that knows forty R4s and one taken R64 knows nothing it can use. Pure.
func usableMoonCount(intel []moonIntel, occupied map[int64]bool, floor float64) int {
	n := 0
	for _, mi := range intel {
		if mi.BestClass >= moonR4 && !occupied[mi.MoonID] && mi.Value >= floor {
			n++
		}
	}
	return n
}

// surveyOpts narrows a survey round: FrontierOnly posts orders for unclaimed 0.0 alone, under
// frontierSurveyOpenMax of their own; Pull multiplies the score of the systems the alliance targets
// or stakes by surveyPullMul (frontier.go).
type surveyOpts struct {
	FrontierOnly bool
	Pull         map[int64]bool
}

// postSurveys tops a corporation's open survey orders up to surveyOpenMax,
// from the best unsurveyed systems within reach of its staging office.
func (m *Mod) postSurveys(ctx context.Context, c *corpCEO, ch Charter, fromStation int64) (int, error) {
	return m.postSurveysWith(ctx, c, ch, fromStation, surveyOpts{})
}

// postSurveysWith is postSurveys narrowed by opts.
func (m *Mod) postSurveysWith(ctx context.Context, c *corpCEO, ch Charter, fromStation int64, opts surveyOpts) (int, error) {
	frontier := m.unclaimedNullSystems(ctx)
	openSys, _ := int64Column(ctx, m, `
		SELECT systemID FROM rolebot_corp_survey WHERE corporationID = ? AND status IN ('open', 'claimed')`, c.CorpID)
	open, limit := len(openSys), surveyOpenMax
	if opts.FrontierOnly {
		open, limit = 0, frontierSurveyOpenMax
		for _, sys := range openSys {
			if frontier[sys] {
				open++
			}
		}
	}
	if open >= limit || fromStation == 0 {
		return 0, nil
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return 0, err
	}
	// A tower can stand anywhere an office reaches (corpPlatformReach, unbounded), so the survey
	// hunt does too: near systems first, unclaimed 0.0 pulls surveyors out (scoreSurveyCand).
	reach, _ := m.corpPlatformReach(ctx, c.CorpID, fromStation, u, unlimitedJumps)
	// 0.0 is measured from the headquarters, and NPC null far from it is left out (localnull.go).
	reach = m.localReach(ctx, c.CorpID, fromStation, u, reach)
	// What the corporation and its friends already know, counted in moons per system: a system is
	// surveyed once every moon in it is. An order covers surveyMoonsPerOrder moons.
	covered := map[int64]int{}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT systemID, COUNT(DISTINCT moonID) FROM rolebot_corp_moon_intel
		 WHERE corporationID IN (`+int64CSV(intelFriendsOf(c.CorpID))+`) AND surveyedAt >= ?
		 GROUP BY systemID`, clock.Unix()-surveyIntelKeep); err == nil {
		for rows.Next() {
			var s int64
			var n int
			if rows.Scan(&s, &n) == nil {
				covered[s] = n
			}
		}
		rows.Close()
	}
	// A system with an order out is spoken for; one whose order lapsed waits surveyRetrySec before
	// it is asked again. A finished order blocks nothing.
	busy := map[int64]bool{}
	posted, _ := int64Column(ctx, m, `
		SELECT systemID FROM rolebot_corp_survey
		 WHERE corporationID = ? AND (status IN ('open','claimed') OR (status = 'void' AND closedAt >= ?))`,
		c.CorpID, clock.Unix()-surveyRetrySec)
	for _, id := range posted {
		busy[id] = true
	}
	var sysIDs []int64
	for sys := range reach {
		if meta, ok := u.meta[sys]; ok && !busy[sys] && meta.Security < 0.45 {
			sysIDs = append(sysIDs, sys)
		}
	}
	if len(sysIDs) == 0 {
		return 0, nil
	}
	moons := map[int64]int{}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT solarSystemID, COUNT(*) FROM mapDenormalize
		 WHERE groupID = 8 AND solarSystemID IN (`+int64CSV(sysIDs)+`) GROUP BY solarSystemID`); err == nil {
		for rows.Next() {
			var s int64
			var n int
			if rows.Scan(&s, &n) == nil {
				moons[s] = n
			}
		}
		rows.Close()
	}
	cands := pullSurveyCands(surveyCandidates(reach, u.meta, busy, moons, covered, frontier), opts)
	n := 0
	for _, cd := range cands {
		if open+n >= limit || cd.Score <= 0 {
			break
		}
		if _, err := m.db.ExecContext(ctx, `
			INSERT INTO rolebot_corp_survey (corporationID, systemID, wagePerMoon, postedAt) VALUES (?, ?, ?, ?)`,
			c.CorpID, cd.SystemID, surveyWageBase, clock.Unix()); err != nil {
			return n, err
		}
		n++
		m.ceoLog(ctx, c, powerStarbase, "survey", cd.SystemID, 0,
			"ordered a moon survey of %s (%d moons unsurveyed, %.1f, %d jumps out)",
			systemNameOf(ctx, m, cd.SystemID), cd.Moons, dispSec(cd.Security), cd.Jumps)
	}
	if n > 0 {
		markPosWork(c.CorpID)
	}
	return n, nil
}

// bfsJumps is every system within maxJumps of from, by gate. Pure over the
// universe.
func bfsJumps(u *universe, from int64, maxJumps int) map[int64]int {
	out := map[int64]int{from: 0}
	frontier := []int64{from}
	for d := 1; d <= maxJumps && len(frontier) > 0; d++ {
		var next []int64
		for _, s := range frontier {
			for _, n := range u.adj[s] {
				if _, ok := out[n]; !ok {
					out[n] = d
					next = append(next, n)
				}
			}
		}
		frontier = next
	}
	return out
}

func systemNameOf(ctx context.Context, m *Mod, sysID int64) string {
	if name := m.systemNameCached(ctx, sysID); name != "" {
		return name
	}
	return fmt.Sprintf("system %d", sysID)
}

func regionNameOf(ctx context.Context, m *Mod, regionID int64) string {
	name := m.regionNameCached(ctx, regionID)
	if name == "" {
		return fmt.Sprintf("region %d", regionID)
	}
	return name
}

// ── the survey errand ───────────────────────────────────────────────────────

// surveyOrder is one rolebot_corp_survey row.
type surveyOrder struct {
	SurveyID, CorpID, SystemID, SurveyorID int64
	Status                                 string
	MoonsDone                              int
	WagePerMoon                            float64
}

func (m *Mod) loadSurvey(ctx context.Context, id int64) (*surveyOrder, error) {
	var s surveyOrder
	err := m.db.QueryRowContext(ctx, `
		SELECT surveyID, corporationID, systemID, surveyorID, status, moonsDone, wagePerMoon
		  FROM rolebot_corp_survey WHERE surveyID = ?`, id).
		Scan(&s.SurveyID, &s.CorpID, &s.SystemID, &s.SurveyorID, &s.Status, &s.MoonsDone, &s.WagePerMoon)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

// posSurveyDue claims a survey order for a member who has nothing better to
// do, when the corporation's kit holds probes. The surveyor is anybody in the
// corporation not seated as Fueler or Procurer; the claim is a row lock on the
// order.
func (m *Mod) posSurveyDue(ctx context.Context, b *botRun) (bool, error) {
	if b.StationID == 0 {
		return false, nil
	}
	if seat := m.posSeatOf(ctx, b.CorpID, b.CharID); seat != "" {
		return false, nil
	}
	now := clock.Unix()
	var id, sys int64
	err := m.db.QueryRowContext(ctx, `
		SELECT surveyID, systemID FROM rolebot_corp_survey
		 WHERE corporationID = ? AND (status = 'open' OR (status = 'claimed' AND claimedAt < ?))
		 ORDER BY postedAt LIMIT 1`, b.CorpID, now-surveyClaimSec).Scan(&id, &sys)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// The kit nearest the pilot: probes stocked at a forward office (ensureFrontierSurveyKit) serve a
	// member out there.
	kitStation, kitOffice := m.posKitOfficeNear(ctx, b, typeSurveyProbe, corpWorkMaxJumps)
	if kitOffice == 0 {
		return false, nil
	}
	if sj := m.jumpsTo(ctx, b, sys); sj < 0 {
		return false, nil
	}
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_survey SET status = 'claimed', surveyorID = ?, claimedAt = ?
		 WHERE surveyID = ? AND (status = 'open' OR (status = 'claimed' AND claimedAt < ?))`,
		b.CharID, now, id, now-surveyClaimSec)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	st := b.State
	st.Activity = ActIndustry
	st.Data = idleData(st.Data)
	st.Data.PosSurveyID = id
	st.Data.PosSurveyKitStation = kitStation
	m.logBot(b.CharID, logInfo, "corp", "took its corporation's moon survey of %s; fetching probes at %s",
		systemNameOf(ctx, m, sys), m.stationLabel(ctx, kitStation))
	return true, m.beginTravel(ctx, b, m.stationSystemID(ctx, kitStation), kitStation, string(TaskPosSurvey))
}

// posSurveyStep is the TaskPosSurvey phase: at the kit office, load probes;
// in the system, spend one per moon on a timed leg; then go home.
func (m *Mod) posSurveyStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	finish := func(note string) (bool, error) {
		if note != "" {
			m.logBot(b.CharID, logInfo, "corp", "%s", note)
		}
		// Unused corporation probes go back on the shelf at the next office
		// the pilot docks at; until then they ride along, still corp-owned.
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return false, saveState(ctx, m.db, st)
	}
	so, err := m.loadSurvey(ctx, d.PosSurveyID)
	if err != nil {
		return false, err
	}
	if so == nil || so.Status != "claimed" || so.SurveyorID != b.CharID || so.CorpID != b.CorpID {
		return finish("its corporation's survey was closed without it")
	}
	// Fetch.
	if m.cargoUnitsOwnedBy(ctx, b.ShipID, b.CorpID, typeSurveyProbe) == 0 {
		if b.StationID != d.PosSurveyKitStation {
			return false, m.beginTravel(ctx, b, m.stationSystemID(ctx, d.PosSurveyKitStation), d.PosSurveyKitStation, string(TaskPosSurvey))
		}
		// The office at the kit station the claim chose, not the biggest stack elsewhere.
		office := m.officeAt(ctx, b.CorpID, d.PosSurveyKitStation)
		if office == 0 || m.divisionHeld(ctx, b.CorpID, office, corpDivStarbase)[typeSurveyProbe] == 0 {
			m.releaseSurvey(ctx, so)
			return finish("its corporation's kit holds no survey probes any more")
		}
		var moons int
		_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mapDenormalize WHERE groupID = 8 AND solarSystemID = ?`,
			so.SystemID).Scan(&moons)
		want := int64(min(max(moons, 1), surveyMoonsPerOrder))
		if _, ok := m.ensureRunabout(ctx, b, "a moon survey"); !ok {
			m.releaseSurvey(ctx, so)
			return finish("had no ship to carry survey probes in")
		}
		got, err := m.takeFromOffice(ctx, b.CorpID, office, corpDivStarbase, typeSurveyProbe, want, b.ShipID, 5, b.CorpID)
		if err != nil || got == 0 {
			m.releaseSurvey(ctx, so)
			return finish("could not take survey probes from its corporation's kit")
		}
		if !m.ownsLauncher(ctx, b) {
			if n, _ := m.takeFromOffice(ctx, b.CorpID, office, corpDivStarbase, typeProbeLauncher, 1, b.ShipID, 5, b.CharID); n == 0 {
				m.logBot(b.CharID, logInfo, "corp", "no probe launcher in the kit — surveying with the probes alone")
			}
		}
		m.logBot(b.CharID, logInfo, "corp", "loaded %d survey probe(s) for %s", got, systemNameOf(ctx, m, so.SystemID))
		return false, m.beginTravel(ctx, b, so.SystemID, 0, string(TaskPosSurvey))
	}
	// Survey.
	if b.SystemID != so.SystemID || b.StationID != 0 {
		return false, m.beginTravel(ctx, b, so.SystemID, 0, string(TaskPosSurvey))
	}
	moonID := m.nextUnsurveyedMoon(ctx, b.CorpID, so.SystemID)
	if moonID == 0 || so.MoonsDone >= surveyMoonsPerOrder {
		return finish(m.closeSurvey(ctx, so, "done"))
	}
	// The probe is launched from the moon it reads: a projection flies there first.
	if m.embodyTowerPrep(b.CharID, moonID) {
		d.LegEndsAt = clock.Unix() + navLegHoldSec
		return false, saveState(ctx, m.db, st)
	}
	if d.PosSurveyMoon != moonID {
		// An unwatched ship is parked at the moon, so a projection built during the flight appears
		// there and not on the gate it came in by. A live projection owns its own coordinate.
		if b.ShipID != 0 && !m.isEmbodied(b.CharID) {
			m.parkAtCelestial(ctx, b.ShipID, moonID)
		}
		// Launch: the probe flies for its explosionDelay x moons.surveyTimeMult.
		d.PosSurveyMoon = moonID
		d.LegEndsAt = clock.Unix() + m.surveyFlightSec(ctx)
		return false, saveState(ctx, m.db, st)
	}
	if !m.consumeCargoUnits(ctx, b.ShipID, b.CorpID, typeSurveyProbe, 1) {
		return finish(m.closeSurvey(ctx, so, "out of probes"))
	}
	mi, err := m.recordMoonIntel(ctx, b.CorpID, moonID, so.SystemID, b.CharID)
	if err != nil {
		return false, err
	}
	d.PosSurveyMoon = 0
	so.MoonsDone++
	_, _ = m.db.ExecContext(ctx, `UPDATE rolebot_corp_survey SET moonsDone = ? WHERE surveyID = ?`, so.MoonsDone, so.SurveyID)
	wage := so.WagePerMoon
	if mi.BestClass >= moonR32 {
		wage *= surveyBonusMul
	}
	m.payCorpWage(ctx, b.CorpID, b.CharID, wage, "moon survey")
	if mi.BestClass >= moonR16 {
		m.logBot(b.CharID, logInfo, "corp", "surveyed %s: best %s, worth %s an hour to a harvester set",
			moonNameOf(ctx, m, moonID), evedb.MoonClassNames[mi.BestClass], iskString(mi.Value))
	}
	return false, saveState(ctx, m.db, st)
}

// surveyFlightSec is a probe's flight before it reports.
func (m *Mod) surveyFlightSec(ctx context.Context) int64 {
	delay, ok, _ := evedb.GetTypeAttributeFloat(ctx, m.db, typeSurveyProbe, "explosionDelay")
	sec := surveyMinDelay * 1.0
	if ok && delay > 0 {
		sec = delay / 1000
	}
	mult := 1.0
	if v, ok, err := evedb.GetServerSetting(ctx, m.db, "moons.surveyTimeMult"); err == nil && ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			mult = f
		}
	}
	return int64(math.Max(surveyMinDelay, sec*mult))
}

// nextUnsurveyedMoon is a moon in the system neither the corporation nor a friend has fresh
// intel on, the same coverage postSurveys counts, so an order on a partly surveyed system
// starts where the last stopped.
func (m *Mod) nextUnsurveyedMoon(ctx context.Context, corpID, sysID int64) int64 {
	var moonID int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT d.itemID FROM mapDenormalize d
		 WHERE d.groupID = 8 AND d.solarSystemID = ?
		   AND NOT EXISTS (SELECT 1 FROM rolebot_corp_moon_intel i
		                    WHERE i.moonID = d.itemID AND i.surveyedAt >= ?
		                      AND i.corporationID IN (`+int64CSV(intelFriendsOf(corpID))+`))
		 ORDER BY d.itemID LIMIT 1`, sysID, clock.Unix()-surveyIntelKeep).Scan(&moonID)
	return moonID
}

func (m *Mod) releaseSurvey(ctx context.Context, so *surveyOrder) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_survey SET status = 'open', surveyorID = 0, claimedAt = 0 WHERE surveyID = ?`, so.SurveyID)
}

func (m *Mod) closeSurvey(ctx context.Context, so *surveyOrder, why string) string {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_survey SET status = 'done', closedAt = ? WHERE surveyID = ?`, clock.Unix(), so.SurveyID)
	if c, _ := getCEO(ctx, m.db, so.CorpID); c != nil {
		m.ceoLog(ctx, c, powerStarbase, "surveyed", so.SurveyorID, 0,
			"%s surveyed %d moon(s) in %s (%s)", m.charNameOr(ctx, so.SurveyorID), so.MoonsDone,
			systemNameOf(ctx, m, so.SystemID), why)
	}
	return fmt.Sprintf("finished its corporation's moon survey of %s (%d moons, %s)",
		systemNameOf(ctx, m, so.SystemID), so.MoonsDone, why)
}

// sweepSurveys expires orders nobody finished.
func (m *Mod) sweepSurveys(ctx context.Context, corpID int64) {
	// A claimed order whose surveyor is still within surveyClaimSec of taking it is under way (a
	// frontier survey can be a long flight), so it does not lapse under them.
	now := clock.Unix()
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_corp_survey SET status = 'void', closedAt = ?
		 WHERE corporationID = ? AND postedAt < ?
		   AND (status = 'open' OR (status = 'claimed' AND claimedAt < ?))`,
		now, corpID, now-surveyStaleSec, now-surveyClaimSec)
}

// ownsLauncher reports whether the pilot has a probe launcher with it.
func (m *Mod) ownsLauncher(ctx context.Context, b *botRun) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity WHERE ownerID = ? AND typeID = ? AND (locationID = ? OR locationID = ?)`,
		b.CharID, typeProbeLauncher, b.ShipID, b.StationID).Scan(&n)
	return n > 0
}

func moonNameOf(ctx context.Context, m *Mod, moonID int64) string {
	var name string
	if err := m.db.QueryRowContext(ctx, `SELECT itemName FROM mapDenormalize WHERE itemID = ?`, moonID).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("moon %d", moonID)
	}
	return name
}
