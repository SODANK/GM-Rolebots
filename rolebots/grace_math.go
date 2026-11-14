package rolebots

import "math"

// The grace ledger's arithmetic, pure: what an event is worth to a pilot of a given grace, how
// fast it fades, and which band the client sees. grace_ledger.go stores and projects it.
// docs/GRACE.md has the tables.

type graceKind string

const (
	gkShipKill graceKind = "shipkill"
	gkPodKill  graceKind = "podkill"
	gkRobbery  graceKind = "robbery"
	gkRansomOK graceKind = "ransomok" // the pirate took the ransom and let the ship go
	gkAggress  graceKind = "aggress"  // shots landed, no kill
	gkFavour   graceKind = "favour"   // the bot's own leadership paid or stood for it
	gkRespect  graceKind = "respect"  // a fair fight, either side
)

// fightFacts is what the death sites know about a kill, enough for fairFight.
type fightFacts struct {
	Pod          bool
	VictimArmed  bool // the loser had guns fitted
	VictimFights bool // and had committed to fighting
	VictimAtWork bool // a miner or hauler on the job
	Ransom       bool // the loser refused a ransom first
	VictimN      int  // ships on the loser's side
	KillerN      int  // ships on the winner's side
	ISK          float64
	Share        float64 // this killer's share of the damage, 0 = whole
}

// fairFight: armed, willing, not at work, not podded, and outnumbered by no more than two to one.
func fairFight(f fightFacts) bool {
	if f.Pod || !f.VictimArmed || !f.VictimFights || f.VictimAtWork {
		return false
	}
	return f.KillerN <= 2*max(1, f.VictimN)
}

// graceReadsRespect: a fair fight is respect unless the loser is vengeful or worse; an unfair one
// is a grievance unless the loser is magnanimous. A pod kill is never respect.
func graceReadsRespect(tn Tuning, fair, pod bool) bool {
	if pod {
		return false
	}
	if fair {
		return tn.Vindictiveness < graceVengeful
	}
	return tn.Forgiveness >= graceMagnanimous
}

// graceKillKind is the kind the loser files for a kill.
func graceKillKind(tn Tuning, f fightFacts) graceKind {
	switch {
	case graceReadsRespect(tn, fairFight(f), f.Pod):
		return gkRespect
	case f.Pod:
		return gkPodKill
	}
	return gkShipKill
}

// Base magnitudes on the 10-point standing scale, before grace bends them.
const (
	graceShipKillBase = -2.0
	gracePodKillBase  = -1.5
	graceRobberyBase  = -1.0
	graceAggressBase  = -0.25
	graceRansomOKBase = 0.75
	graceFavourBase   = 1.0
	graceRespectBase  = 1.5
	graceRefusedMul   = 1.25 // the loser refused a ransom first

	graceGrudgeFloor  = -10.0
	graceRegardCeil   = 7.4 // regard alone never reaches Devoted
	graceRegardHLDays = 45.0
)

// graceDelta is one event's move on the two channels for this pilot. Grievance grows with the
// vindictive pole, regard with the forgiving one.
func graceDelta(kind graceKind, tn Tuning, f fightFacts, impulse float64) (dGrudge, dRegard float64) {
	t := tn.Vindictiveness - tn.Forgiveness
	grudgeMul := bend(t, 0.4, 1, 2) * impulse
	regardMul := bend(t, 1.5, 1, 0.5)
	switch kind {
	case gkShipKill:
		dGrudge = graceShipKillBase * (1 + iskScale(f.ISK))
		if f.Ransom {
			dGrudge *= graceRefusedMul
		}
	case gkPodKill:
		dGrudge = gracePodKillBase
	case gkRobbery:
		dGrudge = graceRobberyBase * (1 + iskScale(f.ISK))
	case gkAggress:
		dGrudge = graceAggressBase
	case gkRansomOK:
		dRegard = graceRansomOKBase
	case gkFavour:
		dRegard = graceFavourBase
	case gkRespect:
		dRegard = graceRespectBase
	}
	if f.Share > 0 {
		dGrudge, dRegard = dGrudge*f.Share, dRegard*f.Share
	}
	return dGrudge * grudgeMul, dRegard * regardMul
}

// graceHalfLifeSec is how fast each channel fades for this pilot.
func graceHalfLifeSec(tn Tuning) (grudge, regard float64) {
	days := 14.0
	switch graceStage(tn.Vindictiveness) {
	case graceStageSore:
		days = 21
	case graceStageGrudging:
		days = 30
	case graceStageVengeful:
		days = 90
	case graceStageImplacable:
		days = 365
	}
	switch {
	case tn.Forgiveness >= graceMagnanimous:
		days = 3
	case tn.Forgiveness >= graceClement:
		days = 5
	case tn.Forgiveness > 0:
		days = 7
	}
	return days * 86400, graceRegardHLDays * 86400
}

func graceDecay(v, dtSec, halfLifeSec float64) float64 {
	if dtSec <= 0 || halfLifeSec <= 0 || v == 0 {
		return v
	}
	return v * math.Pow(0.5, dtSec/halfLifeSec)
}

// graceDecayed is a row's two channels after dt seconds. An implacable pilot's feud never fades
// under the feud band.
func graceDecayed(tn Tuning, grudge, regard, dtSec float64) (float64, float64) {
	gHL, rHL := graceHalfLifeSec(tn)
	g, r := graceDecay(grudge, dtSec, gHL), graceDecay(regard, dtSec, rHL)
	if tn.Vindictiveness >= graceImplacable && grudge <= graceBandValue(-2) {
		g = math.Min(g, graceBandValue(-2))
	}
	return g, r
}

func graceEff(grudge, regard float64) float64 {
	return math.Max(-10, math.Min(10, grudge+regard))
}

// Bands: what the client is shown. −3 blood, −2 feud, −1 annoyed, 0 nothing, +1 respect, +2 favour.
func graceBand(eff float64) int {
	switch {
	case eff <= -10:
		return -3
	case eff <= -5:
		return -2
	case eff <= -2:
		return -1
	case eff >= 5:
		return 2
	case eff >= 2:
		return 1
	}
	return 0
}

func graceBandValue(band int) float64 {
	switch band {
	case -3:
		return -10
	case -2:
		return -5
	case -1:
		return -2
	case 1:
		return 2
	case 2:
		return 5
	}
	return 0
}

func graceBandLabel(band int) string {
	switch band {
	case -3:
		return "blood"
	case -2:
		return "feud"
	case -1:
		return "annoyed"
	case 1:
		return "respect"
	case 2:
		return "favour"
	}
	return ""
}

// graceEvictN is how many of a bot's n projected rows on one side must drop to admit one more
// under cap.
func graceEvictN(n, cap int) int {
	if cap <= 0 || n < cap {
		return 0
	}
	return n - cap + 1
}

// graceDisplaces: a newcomer of this strength takes the place of rows up to strongestDropped.
// Ties keep the row already shown.
func graceDisplaces(strength, strongestDropped float64) bool {
	return strength > strongestDropped
}

// graceMsg is the transactions-window line for an event.
func graceMsg(kind graceKind, ship, system string) string {
	where := ""
	if system != "" {
		where = " in " + system
	}
	switch kind {
	case gkShipKill:
		if ship == "" {
			ship = "ship"
		}
		return "Destroyed my " + ship + where
	case gkPodKill:
		return "Podded me" + where
	case gkRobbery:
		return "Robbed me" + where
	case gkRansomOK:
		return "Took my ransom and let me go" + where
	case gkAggress:
		return "Opened fire on me" + where
	case gkFavour:
		return "Stood by me"
	case gkRespect:
		return "A good fight" + where
	}
	return "Time passed"
}
