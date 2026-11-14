package rolebots

import (
	"context"
	"sync"

	"gurimoon/seclaw"
)

// Where the fights a pilot may have are. This is the one census the PvP trips
// read: every exposed rolebot by system, weighed by the rules of engagement
// (roe.go) with the question the engagement pass asks. A site the census calls
// good is a site the pass will open fire in, because there is one table.

// censusTarget is one exposed rolebot and the facts the rules of engagement
// read about it.
type censusTarget struct {
	corpID    int64
	secStatus float64
	bounty    float64
	heat      bool // a live criminal flag: the only one this tier can see
}

// targetCensus is every exposed rolebot by system, refreshed at most once per
// targetCensusSec.
type targetCensus struct {
	at    int64
	bySys map[int64][]censusTarget
}

const targetCensusSec = 60

var (
	targetCensusMu sync.Mutex
	targetCensusV  *targetCensus
)

// targetCensusNow reads the exposed roster in one query. The phase list comes
// from the task registry, as exposedBotsBySystem's does, so a task registered
// as exposed is prey the moment it exists.
func (m *Mod) targetCensusNow(ctx context.Context) *targetCensus {
	now := clock.Unix()
	targetCensusMu.Lock()
	if targetCensusV != nil && now-targetCensusV.at < targetCensusSec {
		c := targetCensusV
		targetCensusMu.Unlock()
		return c
	}
	targetCensusMu.Unlock()

	c := &targetCensus{at: now, bySys: map[int64][]censusTarget{}}
	var phases []string
	for kind, def := range taskRegistry {
		if def.Exposed {
			phases = append(phases, string(kind))
		}
	}
	if len(phases) == 0 {
		return c
	}
	args := make([]interface{}, len(phases))
	for i, p := range phases {
		args[i] = p
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.solarSystemID, c.corporationID, COALESCE(c.securityRating, 0), COALESCE(c.bounty, 0),
		       CAST(COALESCE(JSON_VALUE(s.data, '$.piracyHeatUntil'), '0') AS SIGNED)
		FROM rolebot_state s
		JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = s.characterID
		WHERE s.phase IN (`+placeholders(len(phases))+`) AND COALESCE(c.stationID, 0) = 0`, args...)
	if err != nil {
		// An empty census sends nobody anywhere, which is the direction a failed
		// read should fail in: the trips fall back to the belt sweep.
		return c
	}
	defer rows.Close()
	for rows.Next() {
		var sysID, corpID, heatUntil int64
		var sec, bounty float64
		if rows.Scan(&sysID, &corpID, &sec, &bounty, &heatUntil) != nil {
			continue
		}
		c.bySys[sysID] = append(c.bySys[sysID], censusTarget{
			corpID: corpID, secStatus: sec, bounty: bounty, heat: heatUntil > now,
		})
	}
	targetCensusMu.Lock()
	targetCensusV = c
	targetCensusMu.Unlock()
	return c
}

// legalWeightOf is what one target is worth to one viewer, straight off the
// rules of engagement. Pure. An allowed shot is worth 1, a nerve-roll shot the
// nerve's odds, a refused one 0. An outlaw and a price on the head add half each.
func legalWeightOf(st Stance, doc Doctrine, band SecBand, criminal, outlaw, atWar bool,
	nerveP float64, bounty float64) float64 {

	allow, nerve, _ := roeAllows(st, doc, band, criminal, outlaw, atWar)
	w := 0.0
	switch {
	case allow:
		w = 1
	case nerve:
		w = nerveP
	}
	if w <= 0 {
		return 0
	}
	if outlaw || criminal {
		w += 0.5
	}
	if bounty > 0 {
		w += 0.5
	}
	return w
}

// corpStance is how one corporation's pilots regard another's, without the
// personal terms standingToPilot adds (race, prejudice). The census asks it once
// per corporation pair, so a prejudiced pilot may still decline a counted site.
func (m *Mod) corpStance(ctx context.Context, self standingSelf, selfAlliance, otherCorp int64) Stance {
	if otherCorp == 0 || otherCorp == self.CorpID {
		return StanceKin
	}
	otherAlliance := m.corpAllianceID(ctx, otherCorp)
	otherFaction := m.corpFactionID(ctx, otherCorp)
	if sameBloc(self.CorpID, selfAlliance, self.FactionID, otherCorp, otherAlliance, otherFaction) {
		return StanceKin
	}
	view := relFold(0, self.CorpID, selfAlliance, otherCorp, otherAlliance)
	if self.FactionID != 0 && otherFaction != 0 && otherFaction != self.FactionID {
		if mx, ok := m.standingsMatrix(ctx); ok {
			view = worseStanding(view, blocStanding(mx, self.FactionID, otherFaction))
		}
	}
	return stanceOf(view, false)
}

// legalSites weighs every system by how much lawful trouble is in it for this
// pilot. The second map is the war enemies alone, for the campaign search.
func (m *Mod) legalSites(ctx context.Context, b *botRun, doc Doctrine) (all, war map[int64]float64) {
	all, war = map[int64]float64{}, map[int64]float64{}
	if doc == DoctrineNone {
		return
	}
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return
	}
	cen := m.targetCensusNow(ctx)
	self := b.standingSelf()
	selfAlliance := m.corpAllianceID(ctx, b.CorpID)
	stances := map[int64]Stance{}
	atWars := map[int64]bool{}
	warOpen := warEverywhere() && relCorpAtWar(b.CorpID)

	for sysID, targets := range cen.bySys {
		ms, ok := u.meta[sysID]
		if !ok {
			continue
		}
		band := secBand(dispSec(ms.Security))
		for _, t := range targets {
			if t.corpID == b.CorpID {
				continue
			}
			st, ok := stances[t.corpID]
			if !ok {
				st = m.corpStance(ctx, self, selfAlliance, t.corpID)
				stances[t.corpID] = st
			}
			atWar, ok := atWars[t.corpID]
			if !ok {
				if warOpen {
					atWar = relAtWar(b.CorpID, selfAlliance, t.corpID, m.corpAllianceID(ctx, t.corpID))
				}
				atWars[t.corpID] = atWar
			}
			outlaw := t.secStatus <= seclaw.OutlawStatus
			// The nerve roll is not consulted: it decides an encounter, and rolling it here
			// would burn that decision. The odds are the weight.
			w := legalWeightOf(st, doc, band, t.heat, outlaw, atWar,
				neutralNerve(b.Tune.NeutralAggression, doc, band), t.bounty)
			if w <= 0 {
				continue
			}
			all[sysID] += w
			if atWar && st == StanceHostile {
				war[sysID] += w
			}
		}
	}
	return
}

// legalSiteEntry is one pilot's census answer, kept for a short while because
// the tactic table asks for it from two weights and a site in one planning pass.
type legalSiteEntry struct {
	at        int64
	bestSys   int64
	bestScore float64
}

var legalSiteMemo sync.Map // charID -> legalSiteEntry

const legalSiteMemoSec = 30

// legalSiteFor answers the best system this pilot could lawfully hunt in and
// its score, walking out from where it stands under its security floor.
func (m *Mod) legalSiteFor(ctx context.Context, b *botRun, secMin float64) (sysID int64, score float64) {
	now := clock.Unix()
	if v, ok := legalSiteMemo.Load(b.CharID); ok {
		if e := v.(legalSiteEntry); now-e.at < legalSiteMemoSec {
			return e.bestSys, e.bestScore
		}
	}
	doc := m.corpDoctrineFor(ctx, b.CorpID)
	all, war := m.legalSites(ctx, b, doc)
	u, err := m.universe(ctx)
	if err != nil || u == nil {
		return 0, 0
	}
	sysID, score = bestLegalSite(u, b.SystemID, secMin, all, war)
	legalSiteMemo.Store(b.CharID, legalSiteEntry{at: now, bestSys: sysID, bestScore: score})
	return sysID, score
}

// bestLegalSite is the bounded outward walk, with piracySite's hunting-ground
// terms and roamSite's war premium: weight is capped like prey, distance is a
// reward, and a war enemy counts again. Pure over the graph and the two maps.
func bestLegalSite(u *universe, from int64, secMin float64, all, war map[int64]float64) (int64, float64) {
	var best int64
	bestScore := 0.0
	seen := map[int64]bool{from: true}
	frontier := []int64{from}
	for hop := 0; hop <= piracySearchHops && len(frontier) > 0; hop++ {
		for _, sysID := range frontier {
			ms, ok := u.meta[sysID]
			if !ok {
				continue
			}
			sec := dispSec(ms.Security)
			if sec < secMin {
				continue
			}
			n := all[sysID]
			if n <= 0 {
				continue
			}
			if n > piracyPreyCap {
				n = piracyPreyCap
			}
			away := hop
			if away > piracyAwayCap {
				away = piracyAwayCap
			}
			score := n*piracyPreyWorth + float64(away)*piracyAwayWorth + (1-sec)*piracyRoughWorth
			if w := war[sysID]; w > 0 {
				if w > piracyPreyCap {
					w = piracyPreyCap
				}
				score += w * piracyPreyWorth * warMarkMul()
			}
			if best == 0 || score > bestScore {
				best, bestScore = sysID, score
			}
		}
		var next []int64
		for _, sysID := range frontier {
			for _, nb := range u.adj[sysID] {
				if !seen[nb] {
					seen[nb] = true
					next = append(next, nb)
				}
			}
		}
		frontier = next
	}
	return best, bestScore
}

func init() { registerCharForget("legalSiteMemo", func(id int64) { legalSiteMemo.Delete(id) }) }
