package rolebots

// The mining cruiser road (goalMineCruiser): a mining-career pilot trains its
// racial Cruiser skill, and the Osprey or Scythe becomes a hull it can buy.
//
// A hull nobody can fly is dropped from candidatePool (fitgen.go), because the
// build book counts demand by who can fly a hull. This road breaks that circle
// from the skill end; noteHullDemand breaks it from the market end. One level
// per goal, exempt from the step cap.
//  1. the hull: the racial Cruiser skill to I (chainFor resolves its chain), for
//     a pilot with Mining IV that owns no barge.
//  2. the lever: the Cruiser skill to III once the pilot flies one.

const (
	mineCruiserOpenWorth = 0.70 // opening the hull: a cruiser's rack over a frigate's
	mineCruiserLeverCap  = 3
	mineCruiserLeverPer  = 0.15 // the hull bonus per Cruiser level, roughly
	mineCruiserRoadWhy   = "the road to a mining cruiser"
	mineCruiserLeverWhy  = "a bigger yield out of its mining cruiser"
	groupCruiser         = int64(26)
)

// miningCruiser is one empire's mining cruiser.
type miningCruiser struct {
	TypeID int64
	Name   string
	Mult   float64 // laser hulls 1.0, drone hulls 0.7 (hulltags.go's weight)
}

// miningCruiserByRace is the hull a pilot of each race is trained toward.
// chrRaces IDs: 1 Caldari, 2 Minmatar, 4 Amarr, 8 Gallente; the Jove (16)
// fly Caldari hulls (racialShip).
var miningCruiserByRace = map[int64]miningCruiser{
	1:  {620, "Osprey", 1.0},
	2:  {631, "Scythe", 1.0},
	4:  {628, "Arbitrator", 0.7},
	8:  {626, "Vexor", 0.7},
	16: {620, "Osprey", 1.0},
}

// miningCruiserGoals is the next step on the road, or the next lever.
func (p *skillPlanner) miningCruiserGoals() []iceGoal {
	if p.wMine <= 0 || p.levels[skillMining] < 4 || p.ownsBarge() {
		return nil
	}
	hull, ok := miningCruiserByRace[p.raceID]
	if !ok {
		return nil
	}
	cruiserID := p.skillByName(racialShip(p.raceID, "Cruiser"))
	if cruiserID == 0 {
		return nil
	}
	if p.levels[cruiserID] < 1 {
		road, ok := p.chainFor(cruiserID, 1)
		if !ok || len(road) == 0 {
			return nil
		}
		step := road[0]
		leadsTo := cruiserID
		if step.TypeID == cruiserID {
			leadsTo = 0
		}
		return []iceGoal{{step.TypeID, step.Level, leadsTo, mineCruiserRoadWhy}}
	}
	if !p.ownsHull(hull.TypeID) {
		return nil
	}
	if cur := p.levels[cruiserID]; cur < mineCruiserLeverCap {
		return []iceGoal{{cruiserID, cur + 1, 0, mineCruiserLeverWhy}}
	}
	return nil
}

// ownsHull reports whether the pilot owns a hull of this type anywhere. A
// planner with no database (the harness) owns none.
func (p *skillPlanner) ownsHull(typeID int64) bool {
	if p.m == nil || p.m.db == nil || p.ctx == nil {
		return false
	}
	var n int
	_ = p.m.db.QueryRowContext(p.ctx,
		`SELECT COUNT(*) FROM entity WHERE ownerID = ? AND typeID = ? AND singleton = 1`,
		p.charID, typeID).Scan(&n)
	return n > 0
}

// evaluateMineCruiser prices a goal on the road, or the lever.
func (p *skillPlanner) evaluateMineCruiser(g *skillGoal) {
	g.valued = true
	g.value = 0
	if p.wMine <= 0 || p.poolRef <= 0 {
		return
	}
	hull, ok := miningCruiserByRace[p.raceID]
	if !ok {
		return
	}
	if g.Why == mineCruiserLeverWhy {
		gain := mineCruiserLeverGain(mineCruiserLeverPer, g.Level-1)
		g.value = p.wMine * p.poolRef * iceLeverWorth * gain * hull.Mult
		return
	}
	cruiserID := p.skillByName(racialShip(p.raceID, "Cruiser"))
	road, ok := p.chainFor(cruiserID, 1)
	if !ok {
		return
	}
	roadMins, _, _ := p.chainCost(road)
	g.value = iceRoadValue(p.wMine, p.poolRef, mineCruiserOpenWorth*hull.Mult, 1, g.minutes, roadMins)
}

// mineCruiserLeverGain is the fractional yield gain of one more level of a
// per-level yield bonus: (1+per·(from+1))/(1+per·from) − 1. Pure.
func mineCruiserLeverGain(per float64, from int) float64 {
	if per <= 0 || from < 0 {
		return 0
	}
	return per / (1 + per*float64(from))
}
