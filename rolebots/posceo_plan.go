package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

// Deciding a new tower: one purpose, one moon, one size, one race, one layout, and the
// money to see it through.

// maxTowersFor is how many towers a corporation of this size runs. Pure.
func maxTowersFor(members int) int {
	if members < towerMinMembers {
		return 0
	}
	return min(towerMaxPerCorp, 1+members/towerPerMembers)
}

// considerNewTower plans the next tower when the corporation has room, the appetite, a case
// for a purpose, a site and the money. The outcome line says what it decided and why: the
// think drops it, the Starbases board's "consider now" shows it.
func (m *Mod) considerNewTower(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, plans []*towerPlan) (string, error) {
	if ch.PosAppetite < 0.4 {
		return fmt.Sprintf("no appetite for towers (%.2f, the line is 0.40)", ch.PosAppetite), nil
	}
	members, _ := m.corpMembers(ctx, c.CorpID, 0)
	eligible := 0
	for _, cm := range members {
		if cm.Eligible() {
			eligible++
		}
	}
	// A tower its alliance pledged to fund (alliancesovpledge.go) may go one over the cap, in the
	// pledged system only.
	pledge := m.openSovPledgeFor(ctx, c.CorpID)
	limit := maxTowersFor(eligible)
	if pledge != nil && limit > 0 {
		limit++
	}
	// The room gates are kept as a reason rather than returned at once: a member scouting for its
	// alliance (scout) still surveys the alliance's systems below, then stops.
	blocked := ""
	active := 0
	for _, p := range plans {
		if p.active() {
			active++
			if blocked == "" && (p.Status == towerPlanned || p.Status == towerStaging) {
				// One tower coming together at a time.
				blocked = fmt.Sprintf("its %s tower at %s is still being %s; one tower comes together at a time",
					purposeLabel[p.Purpose], moonNameOf(ctx, m, p.MoonID), p.Status)
			}
		}
	}
	if blocked == "" && active >= limit {
		blocked = fmt.Sprintf("it runs %d tower(s), as many as its %d eligible member(s) can hold", active, eligible)
	}
	ambitious, _ := m.frontierAmbition(ctx, c, ch)
	allianceID := m.corpAllianceID(ctx, c.CorpID)
	pull := m.allianceFrontierSystems(ctx, allianceID)
	scout := ambitious && allianceID != 0 && len(pull) > 0
	if blocked != "" && !scout {
		return blocked, nil
	}
	hq := facts.StationID
	if hq == 0 {
		hq, _ = m.corpFactoryOffice(ctx, c.CorpID)
	}
	if hq == 0 {
		return "it has no headquarters or factory office to stage from", nil // estate first
	}
	frontier := m.unclaimedNullSystems(ctx)
	// No capital, no tower, and no survey either: a survey's wages are only worth paying a
	// corporation that could act on the answer, so the money test comes first. A scout keeps its
	// frontier and target orders: the alliance's pledge may pay for the tower. A pledge builder skips
	// the start bar; planTower's margin, pledge money included, decides.
	capitalWhy := ""
	if pledge == nil {
		keep := map[int64]bool{}
		if scout {
			for s := range frontier {
				keep[s] = true
			}
			for s := range pull {
				keep[s] = true
			}
		}
		ok, free, err := m.towerCapitalOK(ctx, c, ch, hq, keep)
		if err != nil {
			return "", err
		}
		if !ok {
			capitalWhy = fmt.Sprintf("it has %s free and tower planning starts at %s", iskString(free), iskString(towerStartISK))
			if !scout {
				return capitalWhy, nil
			}
		}
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return "", err
	}
	// A tower is staged and fuelled from the office nearest it (pickStagingStation), however
	// far: distance prices a site (siteScore), it never bars one. reach covers every system an office's gates join.
	reach, via := m.corpPlatformReach(ctx, c.CorpID, hq, u, unlimitedJumps)
	// 0.0 is measured from the headquarters, and NPC null far from it is left out (localnull.go).
	// via still names the nearest office, which stages and fuels the tower.
	reach = m.localReach(ctx, c.CorpID, hq, u, reach)
	// Moon intel is the first need of everything below; keep surveys going.
	intel, err := m.corpMoonIntel(ctx, c.CorpID, -1)
	if err != nil {
		return "", err
	}
	intel = intelWithin(intel, reach)
	floor := m.miningValueFloor(ctx, c, ch, hq)
	occupied := m.occupiedMoons(ctx)
	// Keep surveying until there are moons a tower could go on, not merely moons known: most
	// known moons are unusable R4s. Only a corporation that could plant now pays for that round.
	if blocked == "" && capitalWhy == "" && usableMoonCount(intel, occupied, floor) < surveyWantUsable {
		if _, err := m.postSurveysWith(ctx, c, ch, hq, surveyOpts{Pull: pull}); err != nil {
			return "", err
		}
		m.ensureSurveyKit(ctx, c, hq)
	}
	// A corporation reaching for unclaimed 0.0 keeps frontier orders out until it knows enough
	// moons there, whatever it knows nearer home (frontier.go): nearby NPC-null moons would
	// otherwise fill the list and the surveyors would never fly out. A scout also keeps them out
	// while no usable moon is known in its alliance's own systems.
	if ambitious && (usableFrontierCount(intel, occupied, floor, frontier) < frontierWantUsable ||
		(scout && usableFrontierCount(intel, occupied, floor, pull) == 0)) {
		if _, err := m.postSurveysWith(ctx, c, ch, hq, surveyOpts{FrontierOnly: true, Pull: pull}); err != nil {
			return "", err
		}
		m.ensureSurveyKit(ctx, c, hq)
		m.ensureFrontierSurveyKit(ctx, c, frontier)
	}
	if blocked != "" {
		return blocked, nil
	}
	if capitalWhy != "" {
		return capitalWhy, nil
	}

	sigs := m.purposeSignals(ctx, c, facts, plans, intel, floor)
	if len(sigs) == 0 {
		return fmt.Sprintf("nothing argues for a tower: no moon in reach earns %s an hour, and no trade, war, "+
			"factory, lab or reaction case", iskString(floor)), nil
	}
	tags := facts.Policy.Tags
	strength := m.corpHoldStrength(ctx, c.CorpID, tags, eligible)
	clusterSys := map[int64]bool{}
	// An amenity's work anchor is the corporation's mining towers, where its members already
	// fly, not every tower it holds.
	miningSys := map[int64]bool{}
	for _, p := range plans {
		if p.active() {
			clusterSys[p.SystemID] = true
			if p.Purpose == purposeMining {
				miningSys[p.SystemID] = true
			}
		}
	}
	// The alliance's towers cluster like the corporation's own, and the systems its executor
	// named pull a new tower toward them (alliancesov.go; both empty with the sov switch off).
	for sys := range m.allianceMateTowerSystems(ctx, allianceID, c.CorpID) {
		clusterSys[sys] = true
	}
	// A tower in 0.0 the local reach left out (far NPC null) pulls nothing after it.
	for _, set := range []map[int64]bool{clusterSys, miningSys} {
		for sys := range set {
			if _, ok := reach[sys]; !ok && u.meta[sys].Security < 0 {
				delete(set, sys)
			}
		}
	}
	// Targets and the stake both pull (alliancestake.go). While the corporation holds fewer than
	// frontierFirstTowers towers out there, a site in them is taken before any other. For
	// frontierHoldSec after the alliance names them, an ambitious member's mining and reactions
	// towers go nowhere else (strictPull), so a moon nearer home does not take the slot before the
	// surveys report. A pledge builder plants in the pledged system alone, whatever the purpose.
	targets := pull
	pullFirst := len(pull) > 0 && frontierTowerCount(plans, pull, frontier) < frontierFirstTowers
	strictPull := false
	var holdLeft int64
	if pledge != nil {
		pull = map[int64]bool{pledge.SystemID: true}
		pullFirst = true
	} else if pullFirst && ambitious {
		holdLeft = frontierHoldLeft(m.allianceFrontierSince(ctx, allianceID), clock.Unix())
		strictPull = holdLeft > 0
	}
	heldBack := false

	type choice struct {
		sig   purposeSignal
		site  siteCandidate
		score float64
	}
	var best *choice
	var tried []string
	for _, sig := range rankPurposes(sigs, tags, 1, 1) {
		tried = append(tried, purposeLabel[sig.Purpose])
		cl := clusterSys
		if isAmenityPurpose(sig.Purpose) {
			cl = miningSys
		}
		if sig.Systems != nil {
			cl = sig.Systems // the case names its systems (a fortress: its sov holds)
		}
		cands := m.siteCandidates(ctx, c, ch, sig.Purpose, sig.Hub, intel, occupied, reach, cl, u, floor)
		if pullFirst {
			strict := pledge != nil || (strictPull && (sig.Purpose == purposeMining || sig.Purpose == purposeReactions))
			before := len(cands)
			cands = sitesInFirst(cands, pull, strict)
			if strict && before > 0 && len(cands) == 0 {
				heldBack = true
			}
		}
		if len(cands) == 0 {
			continue
		}
		for i := range cands {
			cands[i].AllianceTarget = targets[cands[i].SystemID]
			cands[i].Frontier = frontier[cands[i].SystemID]
			cands[i].Office = via[cands[i].SystemID]
		}
		scores := make([]float64, len(cands))
		for i := range cands {
			v := siteScore(cands[i], sig.Purpose)
			if sig.Purpose == purposeMining || (sig.Purpose == purposeReactions && !sig.Hub) {
				v *= holdFactor(strength, cands[i].Danger)
			}
			scores[i] = v
		}
		// Softmax over the top of the list, drawn stably for the day.
		idx := topSoftmaxPick(scores, towerSitePickTop, roll01(c.CorpID, int64(len(plans)), clock.Unix()/86400))
		if idx < 0 || scores[idx] <= 0 {
			continue
		}
		total := purposeScore(sig, tags, 1, cands[idx].Danger) * scores[idx]
		if best == nil || total > best.score {
			best = &choice{sig: sig, site: cands[idx], score: total}
		}
		break // the strongest purpose with a site wins; the rest wait their turn
	}
	if best == nil {
		if pledge != nil {
			return fmt.Sprintf("has no free moon it can reach at %s, where its alliance pledged to fund a tower",
				systemNameOf(ctx, m, pledge.SystemID)), nil
		}
		if heldBack {
			return fmt.Sprintf("holds its tower slot for its alliance's targets: no surveyed moon there yet (%s left)",
				fmtDuration(holdLeft)), nil
		}
		return fmt.Sprintf("no free moon its offices can reach suits a %s tower",
			strings.Join(tried, " or ")), nil
	}
	out, err := m.planTower(ctx, c, ch, facts, best.sig, best.site, intel)
	if pledge != nil && strings.HasPrefix(out, "not yet: ") {
		out = m.sovPledgeWaitLine(ctx, pledge) + "; " + out
	}
	return out, err
}

// towerCapitalReady is the money test for starting to look for a tower: free capital (the
// wallet less the charter's reserve and every open promise) covers towerStartISK. planTower
// checks the chosen plan's own price. Pure.
func towerCapitalReady(free float64) bool {
	return free >= towerStartISK
}

// setIDs is a set's members, ascending. Pure.
func setIDs(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for id, ok := range set {
		if ok {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// towerCapitalOK gates tower planning, and with it moon surveys, on the corporation holding
// the capital to support a tower. When it does not, untaken survey orders are withdrawn, but
// for those in keep's systems, and the wait is logged once a day. Any saving left open under
// the corporation's own key is released first. free is what it found.
func (m *Mod) towerCapitalOK(ctx context.Context, c *corpCEO, ch Charter, hq int64, keep map[int64]bool) (bool, float64, error) {
	m.closeCommit(ctx, towerSaveCommit, c.CorpID)
	free, err := m.towerFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return false, 0, err
	}
	if towerCapitalReady(free) {
		return true, free, nil
	}
	if len(keep) == 0 {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_survey SET status = 'void', closedAt = ?
			 WHERE corporationID = ? AND status = 'open'`, clock.Unix(), c.CorpID)
	} else if open, err := int64Column(ctx, m, `
		SELECT surveyID FROM rolebot_corp_survey
		 WHERE corporationID = ? AND status = 'open' AND systemID NOT IN (`+int64CSV(setIDs(keep))+`)`,
		c.CorpID); err == nil && len(open) > 0 {
		_, _ = m.db.ExecContext(ctx, `
			UPDATE rolebot_corp_survey SET status = 'void', closedAt = ?
			 WHERE surveyID IN (`+int64CSV(open)+`)`, clock.Unix())
	}
	if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "towerwait") >= 86400 {
		m.ceoLog(ctx, c, powerStarbase, "towerwait", 0, towerStartISK,
			"is not looking for a tower yet: it has %s free and tower planning starts at %s",
			iskString(free), iskString(towerStartISK))
	}
	return false, free, nil
}

const (
	// towerStashJumps is how far from a plan's staging office the kit stash looks for spare
	// structures in the corporation's other offices (stashSources). It does not bound where a
	// tower stands.
	towerStashJumps = 8
	// towerSitePickTop is how many of the best sites the day's draw is made over, so a long
	// tail of equal moons does not outvote the best few.
	towerSitePickTop = 5
	// towerMiningFuelShare is the share of a Small tower's hourly fuel a moon
	// must earn before it is mined (miningValueFloor).
	towerMiningFuelShare = 0.5
	// towerMiningNeedRef is the moon value (ISK an hour) a full need of 1 stands for in the
	// mining signal, capped at 3. It is about a Small tower's fuel: an R8 that just pays it
	// argues weakly, an R64 argues at the cap.
	towerMiningNeedRef = 100000
	// refineryMinArm is the Mining arm a corporation without the mining tag
	// needs before a refinery tower is worth its fuel to it.
	refineryMinArm = 3
	// towerFactoryWeeklyJobs is how many work orders a week a corporation
	// builds before its industry alone justifies a factory tower.
	towerFactoryWeeklyJobs = 20
	// towerReviewGraceSec is how long a new tower runs before its first
	// review counts: long enough for its residents, lines and harvesters to
	// be dealt and used, so it is judged on work and not on its first fuel.
	towerReviewGraceSec = 28 * 86400
)

// signalsArgue is whether a signal list already argues for a purpose. Pure.
func signalsArgue(sigs []purposeSignal, purpose string) bool {
	for _, s := range sigs {
		if s.Purpose == purpose {
			return true
		}
	}
	return false
}

// platformReach is every system within maxJumps of any source system, with its distance to
// the nearest source and that source's station. Sources map system to station. Ties within a
// layer go to the lower station ID, so the answer does not depend on map order. Pure.
func platformReach(u *universe, sources map[int64]int64, maxJumps int) (map[int64]int, map[int64]int64) {
	dist := map[int64]int{}
	via := map[int64]int64{}
	if u == nil || len(sources) == 0 {
		return dist, via
	}
	frontier := make([]int64, 0, len(sources))
	for sys, st := range sources {
		dist[sys], via[sys] = 0, st
		frontier = append(frontier, sys)
	}
	for d := 1; d <= maxJumps && len(frontier) > 0; d++ {
		next := map[int64]int64{}
		for _, s := range frontier {
			for _, n := range u.adj[s] {
				if _, done := dist[n]; done {
					continue
				}
				if cur, ok := next[n]; !ok || via[s] < cur {
					next[n] = via[s]
				}
			}
		}
		frontier = frontier[:0]
		for n, st := range next {
			dist[n], via[n] = d, st
			frontier = append(frontier, n)
		}
	}
	return dist, via
}

// corpPlatformReach is the corporation's platform reach: every system within
// maxJumps of one of its offices, or of its headquarters station, with the
// office that serves it (platformReach).
func (m *Mod) corpPlatformReach(ctx context.Context, corpID, hq int64, u *universe, maxJumps int) (map[int64]int, map[int64]int64) {
	sources := map[int64]int64{}
	add := func(st, sys int64) {
		if st == 0 || sys == 0 {
			return
		}
		if cur, ok := sources[sys]; !ok || st < cur {
			sources[sys] = st
		}
	}
	if rows, err := m.db.QueryContext(ctx, `
		SELECT o.stationID, s.solarSystemID FROM crpoffices o JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ?`, corpID); err == nil {
		for rows.Next() {
			var st, sys int64
			if rows.Scan(&st, &sys) == nil {
				add(st, sys)
			}
		}
		rows.Close()
	}
	add(hq, m.stationSystemID(ctx, hq))
	return platformReach(u, sources, maxJumps)
}

// unclaimedNullSystems is the frontier: every 0.0 system the majority rule contests
// (sovContestable: no NPC faction on the system row) that no alliance holds (no
// solarSystemSov claim). Towers and surveys reach out for it (frontierSiteBonus,
// scoreSurveyCand). Empty when the tables cannot be read.
func (m *Mod) unclaimedNullSystems(ctx context.Context) map[int64]bool {
	ids, err := int64Column(ctx, m, `
		SELECT s.solarSystemID FROM mapSolarSystems s
		 WHERE s.security < 0 AND (s.factionID IS NULL OR s.factionID = 0)
		   AND NOT EXISTS (SELECT 1 FROM solarSystemSov v WHERE v.solarSystemID = s.solarSystemID AND v.allianceID > 0)`)
	out := make(map[int64]bool, len(ids))
	if err != nil {
		return out
	}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// intelWithin keeps the moons in systems the reach covers. Pure.
func intelWithin(intel []moonIntel, reach map[int64]int) []moonIntel {
	out := intel[:0:0]
	for _, mi := range intel {
		if _, ok := reach[mi.SystemID]; ok {
			out = append(out, mi)
		}
	}
	return out
}

// topSoftmaxPick draws among the k best scores (softmaxPick, temperature a
// quarter of the best) and returns the index into scores. Pure.
func topSoftmaxPick(scores []float64, k int, u float64) int {
	if len(scores) == 0 {
		return -1
	}
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	if k > 0 && len(idx) > k {
		idx = idx[:k]
	}
	top := make([]float64, len(idx))
	for i, j := range idx {
		top[i] = scores[j]
	}
	pick := softmaxPick(top, maxF(top)*0.25+1e-9, u)
	if pick < 0 {
		return -1
	}
	return idx[pick]
}

func maxF(v []float64) float64 {
	out := 0.0
	for _, x := range v {
		out = math.Max(out, x)
	}
	return out
}

// holdFactor is how much of a site's value a corporation can expect to keep:
// its strength against the measured danger there. Pure.
func holdFactor(strength, danger float64) float64 {
	return math.Max(0.15, math.Min(1, strength/(danger+0.25)))
}

// corpHoldStrength is a corporation's ability to defend what it plants: its
// members, weighed up for armed tags and fleet seats.
func (m *Mod) corpHoldStrength(ctx context.Context, corpID int64, tags []string, eligible int) float64 {
	armed := 0.0
	for _, t := range []string{"combat", "security", "outlaw"} {
		if containsStr(tags, t) {
			armed += 0.5
		}
	}
	var seats int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolebot_corp_ranks WHERE corporationID = ?`, corpID).Scan(&seats)
	return float64(eligible) * (1 + armed) / 10 * (1 + 0.15*float64(seats))
}

// occupiedMoons is every moon with a tower on it, anyone's.
func (m *Mod) occupiedMoons(ctx context.Context) map[int64]bool {
	out := map[int64]bool{}
	ids, _ := int64Column(ctx, m, `SELECT moonID FROM starbase`)
	for _, id := range ids {
		out[id] = true
	}
	ids, _ = int64Column(ctx, m, `SELECT moonID FROM rolebot_corp_towers WHERE status <> 'gone'`)
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// towerDanger is the measured danger to a tower in a system: the security band, player and
// bot kills there this week, other corporations' towers and how this one feels about them,
// and war enemies living there. The CEO's caution scales it (PosDangerWeight); it is never
// a line of its own.
func (m *Mod) towerDanger(ctx context.Context, corpID, sysID int64, sec float64, weight float64) float64 {
	d := 0.1
	switch {
	case sec < 0:
		d = 0.8
	case sec < 0.45:
		d = 0.45
	}
	var kills int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM killmails WHERE solarSystemID = ? AND killTime >= ? AND npcOnly = 0`,
		sysID, clock.Unix()-7*86400).Scan(&kills)
	d += math.Min(1.5, 0.05*float64(kills))
	// An alliance-mate's tower is not a stranger's (alliancesov.go).
	ownAlliance := m.corpAllianceID(ctx, corpID)
	if rows, err := m.db.QueryContext(ctx, `SELECT DISTINCT ownerCorpID FROM starbase WHERE solarSystemID = ?`, sysID); err == nil {
		for rows.Next() {
			var owner int64
			if rows.Scan(&owner) == nil && owner != corpID {
				if ownAlliance != 0 && m.corpAllianceID(ctx, owner) == ownAlliance {
					continue
				}
				d += 0.1
				if relCorpStanding(corpID, owner) < 0 || relCorpStanding(owner, corpID) < 0 {
					d += 0.4
				}
			}
		}
		rows.Close()
	}
	if relWarEnemySystem(corpID, sysID) {
		d += 1.0
	}
	return d * weight
}

// siteCandidates lists the moons a purpose could take. reach is the corporation's platform
// reach (corpPlatformReach), unbounded, so only a system no gate joins to an office is left
// out. hub is the reactions case for the Large hub, sited like a service tower; a plain
// reactions case is a miner+reactor and wants a moon of its own (reactionSites).
func (m *Mod) siteCandidates(ctx context.Context, c *corpCEO, ch Charter, purpose string, hub bool, intel []moonIntel,
	occupied map[int64]bool, reach map[int64]int, cluster map[int64]bool, u *universe, miningFloor float64) []siteCandidate {
	var out []siteCandidate
	// Friend-shared intel from the whole map puts many moons of one system on
	// the list; its danger and cluster distance are read once.
	clusterMemo := map[int64]int{}
	clusterJumps := func(sys int64) int {
		if len(cluster) == 0 {
			return -1
		}
		if j, ok := clusterMemo[sys]; ok {
			return j
		}
		best := -1
		near := bfsJumps(u, sys, 8)
		for s := range cluster {
			if j, ok := near[s]; ok && (best < 0 || j < best) {
				best = j
			}
		}
		clusterMemo[sys] = best
		return best
	}
	dangerMemo := map[int64]float64{}
	danger := func(sys int64, sec float64) float64 {
		if d, ok := dangerMemo[sys]; ok {
			return d
		}
		d := m.towerDanger(ctx, c.CorpID, sys, sec, ch.PosDangerWeight)
		dangerMemo[sys] = d
		return d
	}
	maxSec := evedb.StarbaseMaxAnchorSecurity(ctx, m.db)
	if purpose == purposeMining {
		for _, mi := range intel {
			if mi.BestClass < moonR4 || occupied[mi.MoonID] || mi.Value < miningFloor {
				continue
			}
			meta := u.meta[mi.SystemID]
			if meta.Security >= 0.45 { // a harvester will not anchor there
				continue
			}
			j, ok := reach[mi.SystemID]
			if !ok {
				continue
			}
			out = append(out, siteCandidate{MoonID: mi.MoonID, SystemID: mi.SystemID, Class: mi.BestClass,
				Value: mi.Value, Jumps: j, Security: meta.Security,
				Danger:  danger(mi.SystemID, meta.Security),
				Cluster: clusterJumps(mi.SystemID)})
		}
		return out
	}
	if purpose == purposeReactions && !hub {
		plans, _ := m.corpTowerPlans(ctx, c.CorpID, true)
		rc := m.reactionContext(ctx, c, plans)
		for _, mi := range intel {
			if occupied[mi.MoonID] {
				continue
			}
			meta := u.meta[mi.SystemID]
			j, ok := reach[mi.SystemID]
			if !ok {
				continue
			}
			rx, ok := m.reactionSite(ctx, rc, mi, meta.Security, miningFloor)
			if !ok {
				continue
			}
			out = append(out, siteCandidate{MoonID: mi.MoonID, SystemID: mi.SystemID, Class: mi.BestClass,
				Value: rx.OutValue, Jumps: j, Security: meta.Security,
				Danger:  danger(mi.SystemID, meta.Security),
				Cluster: clusterJumps(mi.SystemID), Reaction: rx.ReactionTypeID, Tier: rx.Tier,
				Harvest: reactionInputIDs(rx)})
		}
		return out
	}
	// Service towers: a quiet moon near home, empty or poor. Candidate
	// systems are the nearest ones the anchoring rules allow.
	known := map[int64]int{}
	for _, mi := range intel {
		known[mi.MoonID] = mi.BestClass
	}
	var systems []int64
	// An amenity stands where the members work, beside the corporation's towers, not beside the
	// nearest office: its candidates are the systems nearest the tower cluster.
	byCluster := isAmenityPurpose(purpose) && len(cluster) > 0
	cj := map[int64]int{}
	for s, j := range reach {
		if u.meta[s].Security >= maxSec {
			continue
		}
		if purpose == purposeFortress {
			// A fortress stands in the system whose sovereignty it holds down (cluster is the sov-hold
			// set, purposeSignals), never a jump off and never outside 0.0.
			if cluster[s] && u.meta[s].Security < 0 {
				systems = append(systems, s)
			}
			continue
		}
		if byCluster {
			if d := clusterJumps(s); d >= 0 && d <= amenityClusterJumps {
				cj[s] = d
				systems = append(systems, s)
			}
			continue
		}
		if j <= 6 {
			systems = append(systems, s)
		}
	}
	sort.Slice(systems, func(i, j int) bool {
		a, b := systems[i], systems[j]
		if byCluster && cj[a] != cj[b] {
			return cj[a] < cj[b]
		}
		return reach[a] < reach[b] || (reach[a] == reach[b] && a < b)
	})
	if len(systems) > 25 {
		systems = systems[:25]
	}
	if len(systems) == 0 {
		return nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT itemID, solarSystemID FROM mapDenormalize WHERE groupID = 8 AND solarSystemID IN (`+int64CSV(systems)+`)`)
	if err != nil {
		return nil
	}
	perSys := map[int64]int{}
	for rows.Next() {
		var moon, sys int64
		if rows.Scan(&moon, &sys) != nil || occupied[moon] || perSys[sys] >= 3 {
			continue
		}
		class, ok := known[moon]
		if !ok {
			class = -1
		}
		if !moonFitsPurpose(class, purpose) {
			continue
		}
		perSys[sys]++
		meta := u.meta[sys]
		out = append(out, siteCandidate{MoonID: moon, SystemID: sys, Class: class, Jumps: reach[sys],
			Security: meta.Security, Danger: danger(sys, meta.Security),
			Cluster: clusterJumps(sys)})
	}
	rows.Close()
	return out
}

// reactionSite is whether a surveyed moon is a miner+reactor site for the corporation: a
// moon up to R16 (richer ones are mined), where a harvester anchors (under 0.45), holding
// both inputs of a simple reaction it can use, whose output is worth at least the mining
// floor an hour. The best such reaction comes back.
func (m *Mod) reactionSite(ctx context.Context, rc reactionContext, mi moonIntel, security, miningFloor float64) (reactionChoice, bool) {
	if mi.BestClass > moonR16 || security >= 0.45 || len(mi.Materials) < 2 {
		return reactionChoice{}, false
	}
	rx, ok := m.moonReactionCase(ctx, rc, mi.Materials)
	if !ok || rx.OutValue < miningFloor {
		return reactionChoice{}, false
	}
	return rx, true
}

// bestReactionCase is the strongest miner+reactor case over the moons in
// reach: the highest tier, then the richest. intel is already limited to
// the corporation's platform reach.
func (m *Mod) bestReactionCase(ctx context.Context, rc reactionContext, intel []moonIntel, miningFloor float64) (reactionChoice, bool) {
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return reactionChoice{}, false
	}
	occupied := m.occupiedMoons(ctx)
	var best reactionChoice
	found := false
	for _, mi := range intel {
		if occupied[mi.MoonID] {
			continue
		}
		rx, ok := m.reactionSite(ctx, rc, mi, u.meta[mi.SystemID].Security, miningFloor)
		if !ok {
			continue
		}
		if !found || rx.Tier > best.Tier || (rx.Tier == best.Tier && rx.OutValue > best.OutValue) {
			best, found = rx, true
		}
	}
	return best, found
}

// reactionInputIDs is a choice's inputs as type IDs, the order of the
// recipe. Pure.
func reactionInputIDs(rx reactionChoice) []int64 {
	out := make([]int64, 0, len(rx.Inputs))
	for _, l := range rx.Inputs {
		out = append(out, l.TypeID)
	}
	return out
}

// reactionNeedBase and reactionNeedTierStep set a reactions case's strength: 1.5 for a
// reaction only the market wants, plus half a point per tier up the T2 chain, capped at 3
// like mining's best moon, divided by the reactions towers already up.
const (
	reactionNeedBase     = 1.5
	reactionNeedTierStep = 0.5
)

// reactionNeed is the strength of a reactions case by its tier and how many
// miner+reactors the corporation already runs. Pure.
func reactionNeed(tier, have int) float64 {
	return math.Min(3, reactionNeedBase+reactionNeedTierStep*float64(tier)) / float64(1+have)
}

// countReactions is the corporation's active reactions plans of one kind:
// hubs (no harvester of their own) or miner+reactors. Pure.
func countReactions(plans []*towerPlan, hubs bool) int {
	n := 0
	for _, p := range plans {
		if p.Purpose == purposeReactions && p.active() && planHarvests(p) != hubs {
			n++
		}
	}
	return n
}

// reactionTierLabel words a tier for a log line.
var reactionTierLabel = map[int]string{
	reactionTierMarket: "for the market",
	reactionTierRace:   "toward its race's components",
	reactionTierMember: "toward a member's Tech II print",
	reactionTierCorp:   "toward its own Tech II prints",
}

// miningValueFloor is the least a moon must make an hour before it is worth a harvester
// tower: towerMiningFuelShare of what a Small tower of the CEO's race burns near its
// headquarters. Most moons earn a fraction of their fuel, and a tower that cannot come close
// is torn down at its second review.
func (m *Mod) miningValueFloor(ctx context.Context, c *corpCEO, ch Charter, hq int64) float64 {
	race := pickTowerRace(m.charRace(ctx, c.CharID), ch.Xenophobia, nil)
	tt := towerTypes[race][towerSmall]
	if tt == 0 {
		return 0
	}
	batch, err := m.towerFuelBatch(ctx, 0, tt, m.stationSystemID(ctx, hq), c.CorpID, 0)
	if err != nil {
		return 0
	}
	return towerMiningFuelShare * m.batchCost(ctx, batch, 1) / 24
}

// purposeSignals reads the corporation's own need for each kind of tower.
// intel is already limited to the corporation's platform reach.
func (m *Mod) purposeSignals(ctx context.Context, c *corpCEO, facts *CorpFacts, plans []*towerPlan, intel []moonIntel,
	miningFloor float64) []purposeSignal {
	have := map[string]int{}
	for _, p := range plans {
		if p.active() {
			have[p.Purpose]++
		}
	}
	var out []purposeSignal
	// Mining: a surveyed moon in reach worth a harvester (paying at least miningFloor an hour
	// toward its fuel), outside highsec and not taken.
	best := 0.0
	for _, mi := range intel {
		if mi.BestClass >= moonR4 && mi.Value >= miningFloor && mi.Value > best {
			best = mi.Value
		}
	}
	if best > 0 {
		out = append(out, purposeSignal{Purpose: purposeMining, Need: math.Min(3, best/towerMiningNeedRef) / float64(1+have[purposeMining]),
			Why: fmt.Sprintf("the best surveyed moon in reach is worth %s an hour", iskString(best))})
	}
	tags := facts.Policy.Tags
	// There is no depot case. The amenity signals below (refinery, lab, factory) count only
	// while a mining tower is up (rigging or running), and say so when it is not.
	amenity := miningTowerUp(plans)
	// Reactions (posindustry.go). A miner+reactor needs no other tower first; the Large hub
	// needs every input of a composite already flowing from the corporation's own towers.
	rc := m.reactionContext(ctx, c, plans)
	if best, ok := m.bestReactionCase(ctx, rc, intel, miningFloor); ok {
		out = append(out, purposeSignal{Purpose: purposeReactions, Need: reactionNeed(best.Tier, countReactions(plans, false)),
			Why: fmt.Sprintf("a moon in reach supports %s (%s)", m.typeNameOr(ctx, best.ReactionTypeID), reactionTierLabel[best.Tier])})
	}
	if countReactions(plans, true) == 0 {
		if rx, ok := m.complexHubCase(ctx, rc); ok {
			out = append(out, purposeSignal{Purpose: purposeReactions, Hub: true, Need: 2.0,
				Why: fmt.Sprintf("its towers make every input of %s and its prints bill it", m.typeNameOr(ctx, rx.Outputs[0].TypeID))})
		}
	}
	// Refinery: a mining house (the tag, or a Mining arm) processing its fleets' ore locally
	// at 75% instead of hauling it to a station.
	if amenity && have[purposeRefinery] == 0 {
		if need := refineryNeed(containsStr(tags, "mining"), armCount(c.CorpID, armMining)); need > 0 {
			out = append(out, purposeSignal{Purpose: purposeRefinery, Need: need,
				Why: "its miners' ore refined where they mine it"})
		}
	}
	// Factory: work orders the station lines are not getting to.
	var backlog int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_work WHERE corporationID = ? AND status = 'open' AND postedAt < ?`,
		c.CorpID, clock.Unix()-86400).Scan(&backlog)
	if !amenity {
		backlog = 0
	}
	factoryOffice, _ := m.corpFactoryOffice(ctx, c.CorpID)
	if amenity && have[purposeFactory] == 0 && factoryOffice != 0 && m.freeStationLines(ctx, factoryOffice, 1) == 0 &&
		industryTagged(tags) {
		out = append(out, purposeSignal{Purpose: purposeFactory, Need: 1,
			Why: "its factory office has no free manufacturing line"})
	} else if backlog >= 3 && have[purposeFactory] == 0 {
		out = append(out, purposeSignal{Purpose: purposeFactory, Need: math.Min(2, float64(backlog)/5),
			Why: fmt.Sprintf("%d work orders waiting over a day", backlog)})
	} else if have[purposeFactory] == 0 {
		// ...or simply a corporation that builds a lot: its own arrays are
		// lines nobody else queues on, and what they turn out is the tower's
		// income (towerBookProduct).
		var built int
		_ = m.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM rolebot_corp_work WHERE corporationID = ? AND status = 'done' AND closedAt >= ?`,
			c.CorpID, clock.Unix()-7*86400).Scan(&built)
		if amenity && built >= towerFactoryWeeklyJobs {
			out = append(out, purposeSignal{Purpose: purposeFactory, Need: math.Min(1.5, float64(built)/40),
				Why: fmt.Sprintf("%d work orders built this week", built)})
		}
	}
	// Lab: prints in the library short of the CEO's research target.
	var unresearched int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_library l LEFT JOIN invBlueprints b ON b.blueprintID = l.itemID
		 WHERE l.corporationID = ? AND COALESCE(b.materialLevel, 0) < 10`, c.CorpID).Scan(&unresearched)
	if amenity && unresearched >= 4 && have[purposeLab] == 0 {
		need := math.Min(1.5, float64(unresearched)/8)
		why := fmt.Sprintf("%d prints waiting on research", unresearched)
		if factoryOffice != 0 && m.freeStationLines(ctx, factoryOffice, 3) == 0 {
			need = math.Min(2, need+0.5)
			why += ", and no free research line at its office"
		}
		out = append(out, purposeSignal{Purpose: purposeLab, Need: need, Why: why})
	}
	// Fortress: only to hold down sovereignty, a contestable 0.0 system where the corporation
	// and its alliance already run towers (fortressSystems). No alliance means no sov, so never.
	if have[purposeFortress] == 0 {
		if sov := m.sovHoldSystems(ctx, c.CorpID, plans, 0); len(sov) > 0 {
			names := make([]string, 0, len(sov))
			for sys := range sov {
				names = append(names, m.systemNameCached(ctx, sys))
			}
			sort.Strings(names)
			out = append(out, purposeSignal{Purpose: purposeFortress, Need: 0.9, Systems: sov,
				Why: "holding down its sovereignty in " + strings.Join(names, ", ")})
		}
	}
	return out
}

// sovHoldSystems is where a fortress has a point for this corporation (fortressSystems over
// the live towers): its own active plans bar the one numbered except, plus alliance-mates'
// active towers, per system. except lets a standing fortress be judged without counting
// itself (reviewTowers).
func (m *Mod) sovHoldSystems(ctx context.Context, corpID int64, plans []*towerPlan, except int64) map[int64]bool {
	allianceID := m.corpAllianceID(ctx, corpID)
	if allianceID == 0 {
		return nil
	}
	towers := map[int64]int{}
	for _, p := range plans {
		if p.active() && p.PlanID != except {
			towers[p.SystemID]++
		}
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.systemID, COUNT(*) FROM rolebot_corp_towers t
		  JOIN corporation c ON c.corporationID = t.corporationID
		 WHERE c.allianceID = ? AND t.corporationID <> ? AND t.status <> 'gone'
		 GROUP BY t.systemID`, allianceID, corpID)
	if err == nil {
		for rows.Next() {
			var sys int64
			var n int
			if rows.Scan(&sys, &n) == nil {
				towers[sys] += n
			}
		}
		rows.Close()
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return nil
	}
	npcNull, _ := m.npcNullSystems(ctx)
	return fortressSystems(allianceID, towers, func(sys int64) bool {
		return sovContestable(u.meta[sys].Security, npcNull[sys])
	})
}

// amenityClusterJumps is how far from the corporation's nearest tower an
// amenity may stand.
const amenityClusterJumps = 4

// isAmenityPurpose is a tower members come to use: a refinery, a lab, a
// factory. Pure.
func isAmenityPurpose(purpose string) bool {
	switch purpose {
	case purposeRefinery, purposeLab, purposeFactory:
		return true
	}
	return false
}

// miningTowerUp reports whether a corporation has a mining tower rigging or
// running: the gate amenity towers wait on. Pure.
func miningTowerUp(plans []*towerPlan) bool {
	for _, p := range plans {
		if p.Purpose == purposeMining && (p.Status == towerRigging || p.Status == towerRunning) {
			return true
		}
	}
	return false
}

// refineryNeed is a mining house's case for a refinery tower: the mining tag
// or a Mining arm of refineryMinArm or more, rising with the arm to a cap of
// 2. Pure.
func refineryNeed(miningTag bool, miningArm int) float64 {
	if !miningTag && miningArm < refineryMinArm {
		return 0
	}
	return math.Min(2, 0.8+0.1*float64(miningArm))
}

// freeStationLines is how many of a station's assembly lines of an activity (1 manufacturing,
// 3 PE research, 4 ME research, 5 copying; 3 stands for every research activity) have no
// undelivered job on them.
func (m *Mod) freeStationLines(ctx context.Context, stationID int64, activity int) int {
	acts := strconv.Itoa(activity)
	if activity == 3 {
		acts = "3,4,5"
	}
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ramassemblylines l
		 WHERE l.containerID = ? AND l.activityID IN (`+acts+`)
		   AND NOT EXISTS (SELECT 1 FROM ramJobs j WHERE j.assemblyLineID = l.assemblyLineID AND j.completedStatusID IN (0, 1))`,
		stationID).Scan(&n)
	return n
}

// planTower writes the plan for a chosen site, if the corporation can afford to build and
// keep it, and says which it did (considerNewTower).
func (m *Mod) planTower(ctx context.Context, c *corpCEO, ch Charter, facts *CorpFacts, sig purposeSignal,
	site siteCandidate, intel []moonIntel) (string, error) {
	var mats []int64
	unit := map[int64]float64{}
	if sig.Purpose == purposeMining {
		for _, mi := range intel {
			if mi.MoonID == site.MoonID {
				ids := make([]int64, 0, len(mi.Materials))
				for _, mm := range mi.Materials {
					ids = append(ids, mm.TypeID)
				}
				if vals, err := evedb.TypeValues(ctx, m.db, ids); err == nil {
					for _, id := range ids {
						unit[id] = vals[id].Value
					}
				}
				mats = materialsByValue(mi.Materials, unit)
			}
		}
	}
	var reaction int64
	var harvest []int64
	if sig.Purpose == purposeReactions {
		if sig.Hub {
			plans, _ := m.corpTowerPlans(ctx, c.CorpID, true)
			rx, ok := m.complexHubCase(ctx, m.reactionContext(ctx, c, plans))
			if !ok {
				return "its towers no longer make every input of a composite its prints bill", nil
			}
			if ok, _ := evedb.ReactorAccepts(ctx, m.db, typeReactor, rx.ReactionTypeID); !ok {
				return "the Reactor Array does not run " + m.typeNameOr(ctx, rx.ReactionTypeID), nil
			}
			reaction, mats = reactionLayoutInputs(rx)
		} else {
			// The site carries its reaction (reactionSites): a miner+reactor
			// harvests the inputs on its own moon.
			r, ok := m.recipeFor(ctx, site.Reaction)
			if !ok {
				return "the moon's reaction is no longer on sale", nil
			}
			reaction = r.Reaction
			mats = append(append([]int64(nil), r.Inputs...), r.Output)
			harvest = site.Harvest
		}
	}
	size := sizeForMoonClass(site.Class)
	arrays := m.purposeArrays(ctx, c.CorpID, sig.Purpose, facts.Policy.Tags)
	if sig.Purpose != purposeMining {
		size = sizeForArrays(len(arrays))
		if sig.Purpose == purposeReactions {
			// A hub's Reactor Array (3,000 tf) and its silos want a Large tower; a miner+reactor is a
			// Medium: two harvesters, a Medium Reactor Array and its silos (reactionShape).
			size = towerLarge
			if !sig.Hub {
				size = towerMedium
			}
		}
		if sig.Purpose == purposeRefinery {
			size = refinerySize(sig.Need)
		}
	}
	race := pickTowerRace(m.charRace(ctx, c.CharID), ch.Xenophobia, m.isotopeCostNear(ctx, site.SystemID))
	free, err := m.towerFreeCapital(ctx, c.CorpID, ch)
	if err != nil {
		return "", err
	}
	// What each size costs to build and keep fuelled for fuelReserveWeeks. A mining tower steps down
	// to the largest size the corporation can carry: a Small tower on an R64 still pays its fuel.
	preferred := size
	sizes := []string{size}
	switch sig.Purpose {
	case purposeMining:
		sizes = towerSizesFrom(size)
	case purposeRefinery:
		// Never Small: a Small tower's 1,375 CPU cannot power a Medium
		// Intensive Refining Array (2,000).
		sizes = sizesAtLeast(towerSizesFrom(size), towerMedium)
	}
	var towerType int64
	var layout towerLayout
	var keep, need float64
	for _, sz := range sizes {
		tt, lay, kit, k, err := m.towerBuildCost(ctx, layoutInput{Purpose: sig.Purpose, Size: sz, Race: race, Materials: mats,
			Tags: facts.Policy.Tags, Arrays: arrays, Reaction: reaction, Harvest: harvest, Seed: site.MoonID},
			site.SystemID, c.CorpID)
		if err != nil {
			return "", err
		}
		size, towerType, layout, keep, need = sz, tt, lay, k, kit+k
		if free >= need*towerNeedMargin {
			break
		}
	}
	if free < need*towerNeedMargin {
		// Not yet. Nothing is set aside: the corporation keeps trading and plans the tower on the
		// think its free capital clears the price (releaseTowerSavings).
		wait := fmt.Sprintf("its %s %s tower at %s costs %s to build and keep fuelled; it plans at %s free and has %s (%s)",
			sizeLabel(size), purposeLabel[sig.Purpose], moonNameOf(ctx, m, site.MoonID), iskString(need),
			iskString(need*towerNeedMargin), iskString(free), sig.Why)
		if clock.Unix()-m.lastCEOActionAt(ctx, c.CorpID, "towersave") >= 86400 {
			m.ceoLog(ctx, c, powerStarbase, "towersave", site.MoonID, need, "is not planning yet: %s", wait)
		}
		return "not yet: " + wait, nil
	}
	// The office whose reach put the moon on the list is its platform: the kit is staged and
	// the fuel delivered there (advancePlanned keeps it while the office stands).
	p := &towerPlan{CorpID: c.CorpID, MoonID: site.MoonID, SystemID: site.SystemID, Purpose: sig.Purpose,
		Size: size, TowerTypeID: towerType, Status: towerPlanned, Layout: layout, PlannedAt: clock.Unix(),
		StagingStationID: site.Office, Note: sig.Why}
	if err := m.saveTowerPlan(ctx, p); err != nil {
		return "", err
	}
	// A plan in the system the alliance pledged for takes the pledge's money with it
	// (alliancesovpledge.go): the commit closes and the pledge waits on the tower.
	m.linkSovPledge(ctx, c, p)
	// Four weeks of fuel is a standing commitment for the life of the tower, so no other power
	// spends the corporation below it. The kit is committed line by line by its requisitions.
	_ = m.openCommit(ctx, c.CorpID, towerSaveCommit, p.PlanID, keep)
	why := sig.Why
	if size != preferred {
		why += fmt.Sprintf("; a %s tower for now, as it cannot yet afford the %s the moon is worth",
			sizeLabel(size), sizeLabel(preferred))
	}
	done := fmt.Sprintf("planned a %s %s %s tower at %s (%s, %d structures; %s to build and keep fuelled), supplied from %s %d jump(s) away — %s",
		sizeLabel(size), raceLabel(race), purposeLabel[sig.Purpose], moonNameOf(ctx, m, site.MoonID),
		systemNameOf(ctx, m, site.SystemID), len(layout.Pieces), iskString(need), m.stationLabel(ctx, site.Office),
		site.Jumps, why)
	m.ceoLog(ctx, c, powerStarbase, "plan", site.MoonID, need, "%s", done)
	// A tower is the biggest single spend a CEO makes: the members hear of it
	// in the next bulletin and the weekly report, like an HQ move.
	m.queueCorpNews(ctx, c.CorpID, newsEstate, fmt.Sprintf("We are building a %s %s tower at %s, staged from %s.",
		sizeLabel(size), purposeLabel[sig.Purpose], moonNameOf(ctx, m, site.MoonID), m.stationLabel(ctx, site.Office)))
	markPosWork(c.CorpID)
	// The role pass ran before this power in the think: wake the CEO so the
	// next sweep staffs Infrastructure for the tower rather than hours from now.
	wakeCEO(c.CorpID)
	return done, nil
}

// towerBuildCost prices one tower: its type for the layout's race and size, the layout, the kit
// (the tower, each piece and a reactor's first reaction) and fuelReserveWeeks of fuel where it
// would stand. planTower and the alliance's sov pledge (sovPledgeCost) both price through it.
func (m *Mod) towerBuildCost(ctx context.Context, in layoutInput, systemID, corpID int64) (int64, towerLayout, float64, float64, error) {
	tt := towerTypes[in.Race][in.Size]
	lay := buildLayout(in)
	kit := evedb.TypeValueOf(ctx, m.db, tt).Value
	for _, pc := range lay.Pieces {
		kit += evedb.TypeValueOf(ctx, m.db, pc.TypeID).Value
		if isReactorType(pc.TypeID) && pc.Provision != 0 {
			kit += evedb.TypeValueOf(ctx, m.db, pc.Provision).Value
		}
	}
	batch, err := m.towerFuelBatch(ctx, 0, tt, systemID, corpID, 0)
	if err != nil {
		return 0, towerLayout{}, 0, 0, err
	}
	return tt, lay, kit, m.batchCost(ctx, batch, float64(7*fuelReserveWeeks)), nil
}

// towerSizesFrom is a size and every smaller one, largest first: the order a
// mining tower steps down in when the corporation cannot afford the size its
// moon is worth. Pure.
func towerSizesFrom(size string) []string {
	switch size {
	case towerLarge:
		return []string{towerLarge, towerMedium, towerSmall}
	case towerMedium:
		return []string{towerMedium, towerSmall}
	}
	return []string{towerSmall}
}

// refinerySize is the tower a refinery case asks for: Large (the Intensive
// array, 200,000 m³ a cycle) for a big mining house, Medium otherwise. Pure.
func refinerySize(need float64) string {
	if need >= 1.5 {
		return towerLarge
	}
	return towerMedium
}

// sizesAtLeast keeps the sizes at or above a floor, in order. Pure.
func sizesAtLeast(sizes []string, floor string) []string {
	rank := map[string]int{towerSmall: 0, towerMedium: 1, towerLarge: 2}
	var out []string
	for _, s := range sizes {
		if rank[s] >= rank[floor] {
			out = append(out, s)
		}
	}
	return out
}

func sizeLabel(s string) string {
	switch s {
	case towerLarge:
		return "Large"
	case towerMedium:
		return "Medium"
	}
	return "Small"
}

func raceLabel(r int64) string {
	switch r {
	case raceAmarr:
		return "Amarr"
	case raceCaldari:
		return "Caldari"
	case raceGallente:
		return "Gallente"
	case raceMinmatar:
		return "Minmatar"
	}
	return "unknown"
}

// isotopeCostNear is each race's isotope at its cheapest ask within reach of a system, plus
// a per-jump haul allowance: what a tolerant CEO compares.
func (m *Mod) isotopeCostNear(ctx context.Context, sysID int64) map[int64]float64 {
	out := map[int64]float64{}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return out
	}
	near := bfsJumps(u, sysID, 15)
	sysIDs := make([]int64, 0, len(near))
	for s := range near {
		sysIDs = append(sysIDs, s)
	}
	for race, iso := range raceIsotope {
		rows, err := m.db.QueryContext(ctx, `
			SELECT solarSystemID, MIN(price) FROM market_orders
			 WHERE typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0
			   AND solarSystemID IN (`+int64CSV(sysIDs)+`)
			 GROUP BY solarSystemID`, iso)
		if err != nil {
			continue
		}
		best := 0.0
		for rows.Next() {
			var s int64
			var p float64
			if rows.Scan(&s, &p) == nil {
				landed := p * (1 + 0.02*float64(near[s]))
				if best == 0 || landed < best {
					best = landed
				}
			}
		}
		rows.Close()
		out[race] = best
	}
	return out
}

// purposeArrays is what a factory or lab tower anchors, by the corporation's
// tags.
func (m *Mod) purposeArrays(ctx context.Context, corpID int64, purpose string, tags []string) []int64 {
	switch purpose {
	case purposeFactory:
		var out []int64
		if containsStr(tags, "shipbuilding") {
			out = append(out, typeSmallShipAsm)
		}
		out = append(out, typeEquipmentAsm)
		if m.corpHoldsT2Prints(ctx, corpID) {
			out = append(out, typeComponentAsm)
		}
		out = append(out, typeAmmoAssembly)
		return out
	case purposeLab:
		return []int64{typeMobileLab, typeMobileLab}
	}
	return nil
}

// ensureSurveyKit keeps probes (and a launcher) on the shelf while surveys
// are open.
func (m *Mod) ensureSurveyKit(ctx context.Context, c *corpCEO, station int64) {
	var open int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_survey WHERE corporationID = ? AND status IN ('open','claimed')`,
		c.CorpID).Scan(&open)
	if open == 0 {
		return
	}
	office := m.officeAt(ctx, c.CorpID, station)
	if office == 0 {
		return
	}
	kit := m.divisionHeld(ctx, c.CorpID, office, corpDivStarbase)
	if want := int64(open*surveyMoonsPerOrder) - kit[typeSurveyProbe] -
		m.reqPending(ctx, c.CorpID, typeSurveyProbe, station, corpDivStarbase); want > 0 {
		_, _ = m.postRequisition(ctx, c, requisition{TypeID: typeSurveyProbe, Qty: want, DestStationID: station,
			DestFlag: corpDivStarbase, Requester: "ceo", RefKind: "survey", Deadline: clock.Unix() + 2*86400,
			Note: "survey probes"})
	}
	if kit[typeProbeLauncher] == 0 && m.reqPending(ctx, c.CorpID, typeProbeLauncher, station, corpDivStarbase) == 0 {
		_, _ = m.postRequisition(ctx, c, requisition{TypeID: typeProbeLauncher, Qty: 1, DestStationID: station,
			DestFlag: corpDivStarbase, Requester: "ceo", RefKind: "survey", Deadline: clock.Unix() + 2*86400,
			Note: "a probe launcher for the surveyors"})
	}
}

// ensureFrontierSurveyKit stocks probes at the forward office for the open frontier orders, so a
// member out there takes them (posSurveyDue picks the kit office nearest the surveyor) rather than
// the order waiting on one within corpWorkMaxJumps of the headquarters.
func (m *Mod) ensureFrontierSurveyKit(ctx context.Context, c *corpCEO, frontier map[int64]bool) {
	fwd := m.forwardOfficeOf(ctx, c.CorpID)
	if fwd == 0 {
		return
	}
	office := m.officeAt(ctx, c.CorpID, fwd)
	if office == 0 {
		return
	}
	openSys, _ := int64Column(ctx, m, `
		SELECT systemID FROM rolebot_corp_survey WHERE corporationID = ? AND status IN ('open','claimed')`, c.CorpID)
	n := 0
	for _, s := range openSys {
		if frontier[s] {
			n++
		}
	}
	if n == 0 {
		return
	}
	kit := m.divisionHeld(ctx, c.CorpID, office, corpDivStarbase)
	if want := int64(n*surveyMoonsPerOrder) - kit[typeSurveyProbe] -
		m.reqPending(ctx, c.CorpID, typeSurveyProbe, fwd, corpDivStarbase); want > 0 {
		_, _ = m.postRequisition(ctx, c, requisition{TypeID: typeSurveyProbe, Qty: want, DestStationID: fwd,
			DestFlag: corpDivStarbase, Requester: "ceo", RefKind: "survey", Deadline: clock.Unix() + 2*86400,
			Note: "survey probes for the frontier"})
	}
}

