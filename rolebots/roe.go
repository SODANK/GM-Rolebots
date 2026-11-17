package rolebots

import (
	"context"
	"database/sql"
	"errors"
	"math/rand"
	"sync"
)

// Rules of engagement: may this pilot open fire on that one, here, now. An
// opinion (standings.go), a place (the security band) and an employer's policy
// (corpdoctrine.go) produce a verdict, read by both tiers so the abstract sim
// and the embodied projection cannot disagree. It answers no in policed space.
// It is not the crime system (piracy keeps ransom, fencing and the CONCORD
// stopwatch) and not a target picker (combat_embodied.go).

// policedSecurity is the line CONCORD answers at: the same 0.45 as the core's
// concordMinSec and inPolicedSpace (piracy.go).
const policedSecurity = 0.45

// neutralAggressionDefault is what a pilot with no opinion does about a stranger
// in nullsec: one meeting in eight becomes a fight. Hostiles need no roll, so it
// governs only the grey middle; set high, neutrals stop being neutral.
const neutralAggressionDefault = 0.12

// Stance is what one pilot is to another, before any question of where they are
// standing.
type Stance uint8

const (
	// StanceKin is the same corporation, alliance or employer faction. Never a
	// target. It is structural: two strangers of one faction stand at 0, which is
	// neutral, so a standing test cannot carry it (kinship.go).
	StanceKin Stance = iota
	// StanceFriendly is actively liked, personally or through the two employers.
	// Spared.
	StanceFriendly
	// StanceNeutral is a stranger, the only stance that consults the pilot's nerve.
	StanceNeutral
	// StanceHostile is a rival, personally or politically.
	StanceHostile
)

func (s Stance) String() string {
	switch s {
	case StanceKin:
		return "kin"
	case StanceFriendly:
		return "friendly"
	case StanceNeutral:
		return "neutral"
	}
	return "hostile"
}

// stanceOf reads one opinion into a stance. kin is structural and wins over any
// standing; retaliation is not this function's business (combat_embodied.go).
func stanceOf(standing float64, kin bool) Stance {
	switch {
	case kin:
		return StanceKin
	case standing >= StandingLiked:
		return StanceFriendly
	case standing <= StandingDisliked:
		return StanceHostile
	}
	return StanceNeutral
}

// engageStanding folds the pilot's prejudice (tolerance.go xenoStanding) into
// the opinion that decides whether to open fire, only for a doctrine that takes
// neutrals. Race hate never makes a Security or Defence pilot (NeutralMul 0)
// shoot a stranger. Real rivalries arrive in base and are untouched.
func engageStanding(base, xeno float64, doc Doctrine) float64 {
	if xeno == 0 || doc.NeutralMul() <= 0 {
		return base
	}
	return worseStanding(base, xeno)
}

// SecBand is the three kinds of space this decision cares about.
type SecBand uint8

const (
	// BandPoliced: CONCORD answers. Nothing starts a fight here except against
	// somebody already flagged.
	BandPoliced SecBand = iota
	// BandLow: no police, but a criminal record follows a pilot out.
	BandLow
	// BandNull: nobody is watching and nothing is a crime.
	BandNull
)

func (b SecBand) String() string {
	switch b {
	case BandPoliced:
		return "policed"
	case BandLow:
		return "lowsec"
	}
	return "nullsec"
}

// secBand buckets a system's true security.
func secBand(security float64) SecBand {
	switch {
	case security >= policedSecurity:
		return BandPoliced
	case security > 0:
		return BandLow
	}
	return BandNull
}

// bandNerveMul is how much of a pilot's neutral appetite the space unlocks:
// half in lowsec, where a record can still be lost, all of it in nullsec.
func (b SecBand) bandNerveMul() float64 {
	switch b {
	case BandLow:
		return 0.5
	case BandNull:
		return 1.0
	}
	return 0
}

// roeAllows is the whole table, pure (no clock, randomness or database). It
// returns whether to engage, whether the answer still needs the pilot's nerve
// rolled, and the log reason. (false, true, reason) means roll first; (true,
// false, reason) is decided.
func roeAllows(st Stance, doc Doctrine, band SecBand, targetCriminal, targetOutlaw, atWar bool) (allow bool, nerve bool, reason string) {
	// No wing, no fight.
	if doc == DoctrineNone {
		return false, false, "no combat wing"
	}
	// Kin and friendly are spared before anything about the place.
	switch st {
	case StanceKin:
		return false, false, "kin"
	case StanceFriendly:
		return false, false, "friendly"
	}

	// A pilot flagged criminal is fair game anywhere, policed space included.
	if targetCriminal {
		return true, false, "criminal"
	}
	// An outlaw (security record at or below -5.0, permanent) is a legal target
	// for any doctrine with a wing, with no stance guard: killing one costs nothing in seclaw.
	if targetOutlaw {
		return true, false, "outlaw"
	}
	// A declared war is the second path out of policed space; it needs StanceHostile
	// and sits after the kin switch. See docs/comment-notes-b16.md#roe-order.
	if atWar && st == StanceHostile {
		return true, false, "at war"
	}
	if band == BandPoliced {
		return false, false, "policed space"
	}

	switch st {
	case StanceHostile:
		return true, false, st.String() + " in " + band.String()
	case StanceNeutral:
		// A navy does not shoot neutrals: NeutralMul is zero for Security and
		// Defence. Refused here rather than left to a roll.
		if doc.NeutralMul() <= 0 {
			return false, false, "doctrine spares neutrals"
		}
		return false, true, st.String() + " in " + band.String()
	}
	return false, false, "no reason"
}

// neutralNerve is the odds a pilot picks a fight with a stranger: its appetite,
// its doctrine's NeutralMul and the band, clamped to 0..1. The appetite is a bare
// number because the abstract sim reads the pilot's Tuning while a projection
// carries its fleet's (wingNerve on embodiedBot).
func neutralNerve(appetite float64, doc Doctrine, band SecBand) float64 {
	p := appetite * doc.NeutralMul() * band.bandNerveMul()
	if p <= 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// ── the sticky decision ─────────────────────────────────────────────────────
//
// A probability re-evaluated every tick becomes a certainty within seconds, so
// the roll is made once per encounter and remembered. The key is the gang ID for
// a fleet member and the character ID for a solo pilot, so one fleet makes one
// choice (the commander's). Kinship and friendship are still vetoed per member
// in the prey filters.

const (
	// roeDecisionSec is how long one decision about one pilot stands: long enough
	// to cover an engagement and the chase, short enough that meeting the same
	// stranger next week is a fresh question.
	roeDecisionSec = 5 * 60
	// roeDecisionMax is when the table is swept; it only bounds memory.
	roeDecisionMax = 4096
)

type roeDecision struct {
	engage bool
	at     int64
}

var (
	roeMu        sync.Mutex
	roeDecisions = map[[2]int64]roeDecision{}
	// assistDecisions is the same sticky table for whether to come to a blue's
	// aid (assistAllows), kept apart so helping never reads as a decision to
	// engage the attacker.
	assistDecisions = map[[2]int64]roeDecision{}
	// transitWarDecisions is the sticky fight-or-flight choice of a pilot in transit that meets
	// an enemy of its corporation's war (transitWarRoll), kept apart from the other two tables.
	transitWarDecisions = map[[2]int64]roeDecision{}
)

// roeNerveRoll answers whether this decider takes on this stranger, rolling once
// and remembering the answer. deciderID is the gang ID for a fleet member and the
// character ID otherwise; p is the probability (the leader's nerve for a fleet).
// The caller assembles both: the embodied side may not touch the database.
func roeNerveRoll(deciderID, targetID int64, p float64) bool {
	return stickyRoll(roeDecisions, deciderID, targetID, p)
}

// assistNerveRoll is roeNerveRoll for coming to a blue's aid: the same
// decider (one fleet, one choice) and lifetime, its own table.
func assistNerveRoll(deciderID, targetID int64, p float64) bool {
	return stickyRoll(assistDecisions, deciderID, targetID, p)
}

// transitWarRoll is roeNerveRoll for a pilot in transit meeting a war target: the same decider
// (one fleet, one choice) and lifetime, its own table.
func transitWarRoll(deciderID, targetID int64, p float64) bool {
	return stickyRoll(transitWarDecisions, deciderID, targetID, p)
}

// transitWarOdds is the chance a pilot in transit stands and fights a war target rather than
// flying on: the operator's base chance scaled by the pilot's (or its fleet's) nerve against the
// default nerve, so a typical pilot rolls the base and a hothead nearly always stays. Clamped 0..1.
func transitWarOdds(base, nerve float64) float64 {
	if base <= 0 || nerve <= 0 {
		return 0
	}
	return clampRange(base*nerve/neutralAggressionDefault, 0, 1)
}

// stickyRoll rolls once per [decider, target] in table and remembers the
// answer for roeDecisionSec. Every table lives under roeMu.
func stickyRoll(table map[[2]int64]roeDecision, deciderID, targetID int64, p float64) bool {
	if deciderID == 0 || targetID == 0 {
		return false
	}
	if p <= 0 {
		return false
	}
	key := [2]int64{deciderID, targetID}
	now := clock.Unix()

	roeMu.Lock()
	defer roeMu.Unlock()
	if d, ok := table[key]; ok && now-d.at < roeDecisionSec {
		return d.engage
	}
	if len(table) >= roeDecisionMax {
		for k, d := range table {
			if now-d.at >= roeDecisionSec {
				delete(table, k)
			}
		}
	}
	engage := rand.Float64() < p
	table[key] = roeDecision{engage: engage, at: now}
	return engage
}

// forgetROEChar drops every standing decision this character is party to, on
// either side (forget.go). A purged decider matters most: generated pilots are
// addressed by a sequence number a later batch reuses, so the next occupant
// would inherit its opinions.
func forgetROEChar(charID int64) {
	if charID == 0 {
		return
	}
	roeMu.Lock()
	defer roeMu.Unlock()
	for _, table := range []map[[2]int64]roeDecision{roeDecisions, assistDecisions} {
		for k := range table {
			if k[0] == charID || k[1] == charID {
				delete(table, k)
			}
		}
	}
}

// roeEngage is the two halves put together: the pure table, then the sticky
// roll if the table asked for one.
func roeEngage(st Stance, doc Doctrine, band SecBand, targetCriminal, targetOutlaw, atWar bool,
	appetite float64, deciderID, targetID int64) (bool, string) {
	allow, nerve, reason := roeAllows(st, doc, band, targetCriminal, targetOutlaw, atWar)
	if !nerve {
		return allow, reason
	}
	if roeNerveRoll(deciderID, targetID, neutralNerve(appetite, doc, band)) {
		return true, reason + " (nerve)"
	}
	return false, reason + " (passed up)"
}

// ── coming to a friend's aid ────────────────────────────────────────────────
//
// The ROE table answers may I start a fight. A friend under fire asks may I join
// a fight someone else started: more permissive about the enemy (a Security wing
// shoots the neutral shooting its corpmate) and as strict about the law, since
// crimewatch gives the victim's corp no exemption (seclaw.ShotIsCrime).

// assistRel is what the pilot under fire is to the one deciding whether to
// help.
type assistRel uint8

const (
	assistNone assistRel = iota
	// assistDuty is the same corporation or alliance. Helping is the job.
	assistDuty
	// assistBlue is a corporation this one's directors declared blue
	// (relDeclaredBlue). Helping is a choice, rolled once per fight.
	assistBlue
)

// assistAllows is the pure verdict on joining a friend's fight against one
// attacker. Same shape as roeAllows: (false, true, reason) means roll first.
func assistAllows(rel assistRel, doc Doctrine, band SecBand,
	attackerNPC, attackerCriminal, attackerOutlaw, atWar bool) (allow bool, nerve bool, reason string) {
	if rel == assistNone {
		return false, false, "not a friend"
	}
	if doc == DoctrineNone {
		return false, false, "no combat wing"
	}
	switch {
	case attackerNPC:
		reason = "hostile entity"
	case attackerCriminal:
		reason = "criminal"
	case attackerOutlaw:
		reason = "outlaw"
	case atWar:
		reason = "at war"
	case band == BandNull:
		reason = "nullsec"
	default:
		return false, false, "would be a crime"
	}
	if rel == assistBlue {
		return false, true, reason + " (blue)"
	}
	return true, false, reason
}

// ── whose nerve a fleet rolls ───────────────────────────────────────────────

// gangNerve is a fleet's appetite for a fight with a stranger: its commander's.
// If every member rolled its own, eight pilots at 12% would engage 64% of
// neutrals; the sticky table keys on the gang for the same reason. fallback is
// the asking pilot's own number when the leader is unknown.
func (m *Mod) gangNerve(ctx context.Context, gangID int64, fallback float64) float64 {
	if gangID == 0 {
		return fallback
	}
	now := clock.Unix()
	if v, ok := gangNerveCache.Load(gangID); ok {
		e := v.(gangNerveEntry)
		if now-e.at < gangNerveCacheSec {
			return e.nerve
		}
	}
	var leader int64
	err := m.db.QueryRowContext(ctx,
		"SELECT COALESCE(leaderCharID, 0) FROM rolebot_gangs WHERE gangID = ? AND enabled = 1",
		gangID).Scan(&leader)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fallback // do not cache a transient failure
	}
	if leader == 0 {
		return fallback
	}
	nerve := fallback
	if traits, err := getTraits(ctx, m.db, leader); err == nil {
		nerve = resolveTuning(traits).NeutralAggression
	}
	gangNerveCache.Store(gangID, gangNerveEntry{nerve: nerve, at: now})
	return nerve
}

// gangNerveCacheSec is short enough that a change of commander reaches the fleet
// within a pass or two, long enough to avoid re-reading the leader's traits per pilot.
const gangNerveCacheSec = 120

type gangNerveEntry struct {
	nerve float64
	at    int64
}

var gangNerveCache sync.Map

func init() {
	registerCharForget("roeDecisions", forgetROEChar)
	// Gangs are deleted and renumbered; an entry unread for two cache lives
	// belongs to a gone gang.
	registerSweep("gangNerveCacheSweep", func() {
		now := clock.Unix()
		gangNerveCache.Range(func(k, v any) bool {
			if now-v.(gangNerveEntry).at >= 2*gangNerveCacheSec {
				gangNerveCache.Delete(k)
			}
			return true
		})
	})
}