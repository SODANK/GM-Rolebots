package rolebots

import (
	"sync"
	"time"

	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/seclaw"
)

// Why a pilot opened fire: the half of an engagement that chatter needs and the rules of
// engagement discard. A standing folds four layers (personal, corporate registry, race,
// faction matrix) by taking the worst, which decides well but cannot explain: a racist and
// a bot at war with the pilot's corporation both arrive at -7.5. gridMotiveToPilotFor
// (faction.go) and standingMotiveToPilotFor (standings.go) carry the layer beside the
// value and the standing functions wrap them, so the number a bot shoots on and its stated
// motive cannot drift apart.

// motive is the motive chatter gives for an engagement.
type motive string

const (
	motiveNone      motive = ""
	motiveRace      motive = "race"      // the pilot's own prejudice (tolerance.go)
	motiveBloodline motive = "bloodline" // the final xenophobe stage: wrong blood of its own race
	motiveWar       motive = "war"       // a declared war between the two owners
	motiveCorpRival motive = "rival"     // the corporate registry, short of war
	motiveFaction   motive = "faction"   // the employers' factions are at odds
	motivePersonal  motive = "grudge"    // something this pilot did to this bot
	motiveCriminal  motive = "criminal"  // a crimewatch flag
	motiveOutlaw    motive = "outlaw"    // a security record at or below -5
	motiveBounty    motive = "bounty"    // a bounty hunter's mark
	motivePiracy    motive = "piracy"    // a pirate after the hold
	motiveDefense   motive = "defense"   // the other side shot first
	motiveFleetCall motive = "fleet"     // the fleet commander called the primary
	motiveNerve     motive = "nerve"     // a stranger, and the pilot felt like it
)

// motiveRead is one pilot's opinion of another with its motive. Racial is set whenever the
// prejudice layer reached disliked or worse, even if another layer was worse, so a
// xenophobe at war with a Minmatar pilot still says what it thinks of Minmatar.
type motiveRead struct {
	Standing float64
	Why      motive
	Racial   bool
}

// foldWhy is worseStanding with the motive carried along. A strictly worse value wins, so
// on a tie the earlier, more specific layer keeps its name.
func foldWhy(cur motiveRead, v float64, why motive) motiveRead {
	if v < cur.Standing {
		cur.Standing, cur.Why = v, why
	}
	return cur
}

// foldPrejudice is the race layer of both folds. For the engagement question it goes
// through engageStanding's doctrine rule (roe.go): a navy never reads a hated race as a
// motive to shoot, so a navy pilot firing on a criminal does not cite the race.
func foldPrejudice(r motiveRead, x float64, engage bool, doc Doctrine) motiveRead {
	// engageStanding's own test: the term counts only under a doctrine that
	// shoots neutrals at all.
	if engage && doc.NeutralMul() <= 0 {
		return r
	}
	r = foldWhy(r, x, motiveRace)
	r.Racial = x <= StandingDisliked
	return r
}

// blueFloorWhy is relBlueFloor for a read: a declared blue is the one order
// that overrides the rest, and a pilot told not to shoot has no reason to.
func blueFloorWhy(r motiveRead, blue bool) motiveRead {
	if v := relBlueFloor(r.Standing, blue); v != r.Standing {
		return motiveRead{Standing: v}
	}
	return r
}

// engageMotive is the motive a projection gives for the target it has just locked. The
// order is what the target would want explained first: an attacker is being answered, a
// fleet call is an order, a pirate is after the hold, and only then an opinion. Called on
// the combat goroutine: map reads only.
func (m *Mod) engageMotive(eb *embodiedBot, tgt *destiny.Entity, aggressors []uint32) motiveRead {
	if eb == nil || tgt == nil {
		return motiveRead{}
	}
	// The engagement fold, the one wouldEngagePilot shoots on.
	read := m.gridMotiveToPilotFor(eb, tgt, true)
	for _, id := range aggressors {
		if id == tgt.ID {
			read.Why = motiveDefense
			return read
		}
	}
	if primary := fleetCalledPrimary(eb); primary != 0 && primary == tgt.ID {
		read.Why = motiveFleetCall
		return read
	}
	if eb.prey && m.wouldRob(eb, tgt) {
		read.Why = motivePiracy
		return read
	}
	if since := modapi.PvP().CriminalSince; since != nil && since(eb.systemID, tgt.Slim.CharID) > 0 {
		read.Why = motiveCriminal
		return read
	}
	if tgt.Slim.SecurityStatus <= seclaw.OutlawStatus {
		read.Why = motiveOutlaw
		return read
	}
	// The standing layers. A final-stage xenophobe that shut out a pilot of another bloodline
	// of its own race has a bloodline grudge, never a race one.
	if read.Why == motiveNone || read.Standing > StandingDisliked {
		read.Why = motiveNerve
	}
	if read.Why == motiveRace && xenoBloodlineBars(eb.tune.Xenophobia,
		raceOf(eb.charID), bloodlineOf(eb.charID), raceOf(tgt.Slim.CharID), bloodlineOf(tgt.Slim.CharID)) {
		read.Why = motiveBloodline
	}
	return read
}

// fleetCalledPrimary is the primary this pilot's fleet commander has called,
// read without calling a new one (fcGridPrimary picks, and so must not be
// asked twice). 0 for none.
func fleetCalledPrimary(eb *embodiedBot) uint32 {
	if eb == nil {
		return 0
	}
	if eb.isFleetCommander() {
		eb.mu.Lock()
		defer eb.mu.Unlock()
		return eb.call.primary
	}
	lead := fleetLeaderLive(eb)
	if lead == nil {
		return 0
	}
	lead.mu.Lock()
	defer lead.mu.Unlock()
	return lead.call.primary
}

// ── insult grudges ──────────────────────────────────────────────────────────

// A pilot who insults a bot in Local is remembered by that one bot, in memory, for a while.
// It is read as the personal layer of both folds, so a disliked pilot can become a target
// in lowsec. The module stores no opinion of named pilots (standings.go).
const (
	insultGrudgeTTL   = 2 * time.Hour
	insultGrudgeStep  = -2.5 // one insult
	insultGrudgeFloor = StandingDespised
)

type grudgeKey struct{ bot, pilot int64 }

type grudge struct {
	value float64
	until time.Time
}

var (
	grudgeMu sync.Mutex
	grudges  = map[grudgeKey]grudge{}
)

// noteInsult deepens a bot's grudge against a pilot by steps insults' worth.
func noteInsult(botID, pilotID int64, steps float64) {
	if botID == 0 || pilotID == 0 || steps <= 0 {
		return
	}
	now := clock.Now()
	grudgeMu.Lock()
	defer grudgeMu.Unlock()
	k := grudgeKey{botID, pilotID}
	g := grudges[k]
	if now.After(g.until) {
		g.value = 0
	}
	g.value += insultGrudgeStep * steps
	if g.value < insultGrudgeFloor {
		g.value = insultGrudgeFloor
	}
	g.until = now.Add(insultGrudgeTTL)
	grudges[k] = g
	if len(grudges) > 4096 {
		for k, old := range grudges {
			if now.After(old.until) {
				delete(grudges, k)
			}
		}
	}
}

// insultGrudge is a bot's live grudge against a pilot, 0 for none. Read on
// the combat goroutine in every standing fold, so the empty case must stay a
// map miss.
func insultGrudge(botID, pilotID int64) float64 {
	grudgeMu.Lock()
	defer grudgeMu.Unlock()
	if len(grudges) == 0 {
		return 0
	}
	g, ok := grudges[grudgeKey{botID, pilotID}]
	if !ok || clock.Now().After(g.until) {
		return 0
	}
	return g.value
}

func sweepGrudges(now time.Time) {
	grudgeMu.Lock()
	for k, g := range grudges {
		if now.After(g.until) {
			delete(grudges, k)
		}
	}
	grudgeMu.Unlock()
}
