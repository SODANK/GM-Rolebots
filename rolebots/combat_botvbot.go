package rolebots

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
	"gurimoon/seclaw"
)

// Bot-versus-bot conflict in unwatched space, run on a slow pass over the roster.
// Predation: a pirate (pack or loner) picks on a working bot in its system and demands a
// ransom; the victim pays, runs or fights (abstract-combatant arithmetic with logi and
// tackle). Consequence: a ransomed or killed victim files a bounty on every pirate involved.
// The hunt: a bounty fleet with a scram-fitted tackler, or a scram-fitted solo hunter,
// commits to an intercept; the bounty pays by damage dealt and the column zeroes.
// One single-flight periodic pass owns every mutation (the startGangFormation pattern),
// because encounters cross gang shard lines. Police are modelled in pvplaw.go.

const (
	// settingBotPvP is the master switch, on by default. Setting it to 0 gives a peaceful shard.
	settingBotPvP = "rolebots.botPvP"
	// settingPirateBountyISK is what a victim puts on each pirate's head.
	settingPirateBountyISK = "rolebots.pirateBountyISK"
	defaultPirateBountyISK = 10_000

	// botPvPEveryTicks is the pass cadence (ticks are one second).
	botPvPEveryTicks = 120
	// Per-pass work caps: conflict should accrete across an evening, not
	// resolve the whole map in one frame.
	botPvPMaxPredations = 4
	botPvPMaxHunts      = 2
	// Engagements are the widest of the three (every combat patrol is a candidate), so the cap
	// keeps conflict accreting across an evening instead of resolving the map in one frame.
	botPvPMaxEngagements = 4

	// The chance one predation pass actually produces an ambush for a
	// given pirate group with prey in reach. The leader's own nerve
	// (PatrolEncounterChance) scales it.
	botPvPAmbushBase = 0.5

	// Escape arithmetic mirrors the duel's: a fleeing side aligns out at 25% a tick unless the
	// other side has a point on the field.
	botPvPEscapePerTick = 0.25
	botPvPTackledFactor = 0.15
	botPvPMaxTicks      = 600
)

var (
	botPvPOn        atomic.Bool
	pirateBountyISK atomic.Int64
	botPvPBusy      atomic.Bool
)

func init() {
	pirateBountyISK.Store(defaultPirateBountyISK)
	// The boot read (gangform.go) overrides this only when the setting row exists, so this is
	// the default.
	botPvPOn.Store(true)
}

// BotPvP reports whether the bot-versus-bot pass runs at all.
func BotPvP() bool           { return botPvPOn.Load() }
func setBotPvP(on bool)      { botPvPOn.Store(on) }
func PirateBountyISK() int64 { return pirateBountyISK.Load() }
func setPirateBountyISK(v int) {
	if v < 0 {
		v = 0
	}
	pirateBountyISK.Store(int64(v))
}

// startBotPvP registers the pass: single-flight off the tick goroutine, like startGangFormation.
func (m *Mod) startBotPvP() {
	// The law bounty sweep rides with the pass it feeds (pvplaw.go).
	m.startLawBounty()
	m.registerPeriodic("rolebot-botpvp", botPvPEveryTicks, func(uint64) bool {
		if !Master() || !BotPvP() {
			return true
		}
		if !botPvPBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("bot-pvp sweep", func() {
			defer botPvPBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			m.botPvPPass(ctx)
		})
		return true
	})
}

func (m *Mod) botPvPPass(ctx context.Context) {
	m.piratePredationPass(ctx)
	// The lawful sibling of the predation pass (combat_engage.go): without it a combat patrol
	// in unwatched space never starts anything with a rival fleet.
	m.combatEngagementPass(ctx)
	m.bountyHuntPass(ctx)
}

// ── combatants ──────────────────────────────────────────────────────────────

// pvpCombatant is one pilot in a bot-versus-bot engagement: the duel
// combatant plus the identity the killmail needs and the two fleet numbers
// the duel never had.
type pvpCombatant struct {
	*abstractCombatant
	CharID     int64
	Name       string
	CorpID     int64
	SystemID   int64
	ShipID     int64
	ShipTypeID int64
	Role       string
	// SecStatus is this pilot's own security record, carried because the exemption ladder needs
	// it: an outlaw is a legal target for everybody, and there is no crimewatch table or slimItem here.
	SecStatus float64
	// The two ends of the tackle mechanic, both as POINTS rather than as a
	// flag, so they can be compared with each other. ScramPoints is what this
	// pilot puts on somebody else; WarpCore is what it can shrug off.
	ScramPoints float64
	WarpCore    float64
	// AlignSec is the hull's nominal align time (hulltags.go), the biggest
	// term in how long it needs to leave a fight (pvpescape.go). 0 = unread,
	// which reads as no escape window at all.
	AlignSec float64
	// RansomAsked: a demand was made and collapsed before the fight (grace_ledger.go reads it).
	RansomAsked bool
	Dealt       float64 // damage dealt this fight, for splits and killmails
	// Taken is the damage this pilot absorbed this fight. Aggression is billed only for shots
	// that landed on somebody (chargeAbstractAggression), and EHP alone cannot say, since remote
	// rep can restore a hull.
	Taken float64
	// PodChance is this pilot's own appetite for finishing a capsule; there is no botRun to read
	// a Tuning off at the kill. 0 means unread and falls back to the shard's base (pod.go).
	PodChance float64
	// Self is what this pilot needs to have an opinion of another: its employer, that employer's
	// faction and its own grudges (standings.go). The abstract prey pick works from these.
	Self standingSelf
}

// pvpCombatantFor builds one side member from a live run. Nil (no error) when the pilot has
// no measurable ship: docked, podded, mid-swap.
func (m *Mod) pvpCombatantFor(ctx context.Context, charID int64, role string) *pvpCombatant {
	b, err := m.loadRun(ctx, charID)
	if err != nil || b.ShipID == 0 || b.StationID != 0 {
		return nil
	}
	c, err := m.botCombatant(ctx, b)
	if err != nil || c == nil || c.MaxEHP <= 0 {
		return nil
	}
	pc := &pvpCombatant{
		abstractCombatant: c,
		PodChance:         b.Tune.PodKillChance,
		CharID:            charID,
		Name:              b.Name,
		CorpID:            b.CorpID,
		SystemID:          b.SystemID,
		ShipID:            b.ShipID,
		ShipTypeID:        m.entityTypeID(ctx, b.ShipID),
		Role:              role,
		SecStatus:         b.SecStatus,
		Self:              b.standingSelf(),
	}
	// Fleet numbers come from the fitted modules. Tackle is the sum of attribute 105 over the
	// group that carries it positive (mids, scramblers) and negative (lows, stabilisers).
	pc.ScramPoints = m.tacklePointsOn(ctx, b.ShipID, grpWarpScrambler, 19, 26, 1)
	pc.WarpCore = m.tacklePointsOn(ctx, b.ShipID, grpWarpCoreStab, 11, 18, -1)
	pc.AlignSec = m.shipAlignSec(ctx, b.ShipID)
	// Remote repair rides on the embedded abstractCombatant (RepPerSec, from the loadout's
	// repairers scaled by what the capacitor can sustain), so every fight credits the same number.
	return pc
}

// tacklePointsOn sums attribute 105 over one group in one slot band of one ship, with sign
// applied so both ends of the mechanic come back positive. It reads the fitted modules
// directly, not the fit library.
func (m *Mod) tacklePointsOn(ctx context.Context, shipID, groupID int64, loFlag, hiFlag int64, sign float64) float64 {
	var total float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(COALESCE(NULLIF(a.valueFloat,0), CAST(a.valueInt AS DOUBLE))), 0)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID AND t.groupID = ?
		JOIN dgmTypeAttributes a ON a.typeID = e.typeID AND a.attributeID = ?
		WHERE e.locationID = ? AND e.flag BETWEEN ? AND ?`,
		groupID, attrWarpScrambleStr, shipID, loFlag, hiFlag).Scan(&total)
	if total *= sign; total < 0 {
		return 0
	}
	return total
}

// sideTacklePoints is how much scrambling this side can put on the field.
// Pooled, because any one point on any one hull holds the target.
func sideTacklePoints(side []*pvpCombatant) float64 {
	total := 0.0
	for _, c := range side {
		if c != nil {
			total += c.ScramPoints
		}
	}
	return total
}

// sideHasTackle is whether this side can hold anything at all, not how hard: the hunt
// refuses to fly without a point, disruptor or scrambler.
func sideHasTackle(side []*pvpCombatant) bool { return sideTacklePoints(side) > 0 }

// sideWarpCore is the side's weakest warp core strength (see the escapes closure in
// resolveTeamFight). An empty side answers zero.
func sideWarpCore(side []*pvpCombatant) float64 {
	first := true
	weakest := 0.0
	for _, c := range side {
		if c == nil {
			continue
		}
		if first || c.WarpCore < weakest {
			weakest, first = c.WarpCore, false
		}
	}
	return weakest
}

func sideAliveEHP(side []*pvpCombatant) (cur, max float64) {
	for _, c := range side {
		if c == nil {
			continue
		}
		cur += math.Max(c.EHP, 0)
		max += c.MaxEHP
	}
	return cur, max
}

// repPriority orders a side's wounded for the logi: the logi itself first (a side that loses
// its reps loses the rest one hull at a time), then the tackler, then everybody else.
func repPriority(role string) int {
	switch role {
	case RoleLogi:
		return 0
	case RoleTackler:
		return 1
	}
	return 2
}

// sideEwar pools the electronics of a side's living hulls. A dead ship's rack
// is not running.
func sideEwar(side []*pvpCombatant) []ewarEffect {
	var out []ewarEffect
	for _, c := range side {
		if c == nil || c.abstractCombatant == nil || c.EHP <= 0 {
			continue
		}
		out = append(out, c.Ewar...)
	}
	return out
}

// armedShooters counts the living hulls of a side that are putting damage out
// -- the divisor ewarSuppression spreads a rack's relief across. One jammer
// against a six-ship alpha is a sixth of the relief it is against one hull.
func armedShooters(side []*pvpCombatant) int {
	n := 0
	for _, c := range side {
		if c != nil && c.abstractCombatant != nil && c.EHP > 0 && c.DPS > 0 {
			n++
		}
	}
	return n
}

// pvpOutcome is what resolveTeamFight hands back.
type pvpOutcome struct {
	ADead, BDead []*pvpCombatant
	// Escaped reports the side that broke off and made it out (nil = the
	// fight ran to a finish or to the clock).
	BEscaped bool
	// AEscaped reports that side A broke off and got out inside the fight.
	AEscaped bool
	// Ticks is how many seconds the fight ran, the stopwatch abstractConcord
	// reads.
	Ticks int
	// ARetreat / BRetreat is the FC's reason for calling a commanded side
	// off (fleetcommand.go), empty when it fought to the end or had no
	// command.
	ARetreat, BRetreat string
}

// resolveTeamFight runs two sides against each other, one second at a time. Side A picked
// this fight; side B may be trying to leave from the first tick (bFleeing). Focus fire lands
// on the weakest living hull, pooled remote rep mends the most wounded, a side under its
// break-off fraction rolls to leave, and a point on the field collapses those rolls.
func resolveTeamFight(a, b []*pvpCombatant, aBreak, bBreak float64, bFleeing bool) pvpOutcome {
	return resolveTeamFightCmd(a, b, aBreak, bBreak, bFleeing, nil, nil)
}

// resolveTeamFightCmd is resolveTeamFight with each side's fleet command (fleetcommand.go),
// either of which may be nil. A commanded side shoots the FC's primary (fcChoosePrimary),
// mends by seat (repRank) and leaves on the FC's call (fcShouldRetreat), firing at
// abstractAligningFireFrac while it aligns. The commands are the caller's copies, written to.
func resolveTeamFightCmd(a, b []*pvpCombatant, aBreak, bBreak float64, bFleeing bool, aCmd, bCmd *FleetCommand) pvpOutcome {
	var out pvpOutcome
	aPoints, bPoints := sideTacklePoints(a), sideTacklePoints(b)
	aCore, bCore := sideWarpCore(a), sideWarpCore(b)
	aFleeing := false
	aStart, bStart := startOf(a), startOf(b)

	alive := func(side []*pvpCombatant) func(int64) bool {
		return func(id int64) bool {
			for _, c := range side {
				if c != nil && c.CharID == id && c.EHP > 0 {
					return true
				}
			}
			return false
		}
	}
	aAlive, bAlive := alive(a), alive(b)
	// commanded reports whether a side's FC is in the seat this tick,
	// promoting the succession first. A nil command is never commanded.
	commanded := func(cmd *FleetCommand, isAlive func(int64) bool) bool {
		return cmd != nil && cmd.Call != callFree && cmd.promote(isAlive)
	}

	weakest := func(side []*pvpCombatant) *pvpCombatant {
		var w *pvpCombatant
		for _, c := range side {
			if c == nil || c.EHP <= 0 {
				continue
			}
			if w == nil || c.EHP < w.EHP {
				w = c
			}
		}
		return w
	}
	mostWounded := func(side []*pvpCombatant, cmd *FleetCommand) *pvpCombatant {
		var w *pvpCombatant
		for _, c := range side {
			if c == nil || c.EHP <= 0 || c.EHP >= c.MaxEHP {
				continue
			}
			if w == nil ||
				repRank(c, cmd) < repRank(w, cmd) ||
				(repRank(c, cmd) == repRank(w, cmd) && c.EHP/c.MaxEHP < w.EHP/w.MaxEHP) {
				w = c
			}
		}
		return w
	}
	// fire lands one tick of a side's guns. Under a command the target is the FC's call,
	// otherwise the weakest. landed is what reached the first target before any rep.
	fire := func(from, onto []*pvpCombatant, cmd *FleetCommand, leaving bool, frac float64) (primary *pvpCombatant, landed float64) {
		pick := func() *pvpCombatant {
			if cmd != nil {
				return fcChoosePrimary(from, onto, leaving)
			}
			return weakest(onto)
		}
		t := pick()
		if t == nil {
			return nil, 0
		}
		primary = t
		if cmd != nil {
			cmd.Primary = t.CharID
		}
		// The defenders' electronics take a fraction off every shot (ewarabstract.go): jams, damps
		// and disruptors divided across the shooters. A side with no EWAR reads exactly 1.
		relief := 1 - ewarSuppression(sideEwar(onto), armedShooters(from))
		for _, c := range from {
			if c == nil || c.EHP <= 0 || c.DPS <= 0 {
				continue
			}
			// Applied, not nominal: tracking, range and signature against this target (application.go).
			shot := c.dpsOn(t) * relief * frac
			dmg := math.Min(shot, math.Max(t.EHP, 0))
			t.EHP -= shot
			c.Dealt += dmg
			t.Taken += dmg
			if t == primary {
				landed += shot
			}
			if t.EHP <= 0 {
				t.EHP = 0
				t = pick()
				if t == nil {
					return primary, landed
				}
			}
		}
		return primary, landed
	}
	rep := func(side []*pvpCombatant, cmd *FleetCommand) {
		var pool float64
		for _, c := range side {
			if c != nil && c.EHP > 0 {
				pool += c.RepPerSec
				// A hull's own booster or repairer mends only itself.
				if c.SelfRepPerSec > 0 {
					c.EHP = math.Min(c.MaxEHP, c.EHP+c.SelfRepPerSec)
				}
			}
		}
		if pool <= 0 {
			return
		}
		if w := mostWounded(side, cmd); w != nil {
			w.EHP = math.Min(w.MaxEHP, w.EHP+pool)
		}
	}
	// A ship is held when the scrambling exceeds its core strength. Points pool per side but
	// core strength is per pilot. See docs/comment-notes-b02.md#tackle-aggregation.
	escapes := func(fleeing bool, ticksFleeing int, window, enemyPoints, ownCore float64) bool {
		if !fleeing {
			return false
		}
		return rand.Float64() < escapeTickChance(ticksFleeing, window,
			enemyPoints, ownCore, botPvPEscapePerTick, heldEscapeFactor())
	}

	// A ship cannot leave on the tick it decides to: it must react, align and spool
	// (pvpescape.go). The clock starts when the side starts fleeing.
	aWin, bWin := sideEscapeWindow(a), sideEscapeWindow(b)
	aFleeFrom, bFleeFrom := 0, 0
	// What each side's guns put on its primary last tick, for the FC's
	// "cannot be broken" call.
	var aPrimary, bPrimary *pvpCombatant
	var aLanded, bLanded float64

	for tick := 0; tick < botPvPMaxTicks; tick++ {
		aCur, aMax := sideAliveEHP(a)
		bCur, bMax := sideAliveEHP(b)
		if aCur <= 0 || bCur <= 0 {
			break
		}
		out.Ticks = tick + 1
		// Who is in command this tick. A side whose FC died with nobody to promote is headless and
		// judges its break-off as it would without a command.
		aLed, bLed := commanded(aCmd, aAlive), commanded(bCmd, bAlive)
		if !aFleeing {
			if aLed {
				if call, why := fcShouldRetreat(aCmd, a, b, aStart, aPrimary, aLanded); call {
					aFleeing, aFleeFrom, out.ARetreat = true, tick, why
					aCmd.AlignOut, aCmd.CalledAt = true, int64(tick)
				}
			} else if aMax > 0 && aCur/aMax < aBreak {
				aFleeing, aFleeFrom = true, tick
			}
		}
		if !bFleeing {
			if bLed {
				if call, why := fcShouldRetreat(bCmd, b, a, bStart, bPrimary, bLanded); call {
					bFleeing, bFleeFrom, out.BRetreat = true, tick, why
					bCmd.AlignOut, bCmd.CalledAt = true, int64(tick)
				}
			} else if bMax > 0 && bCur/bMax < bBreak {
				bFleeing, bFleeFrom = true, tick
			}
		}
		// A fleeing side stops shooting; a winning side does not. A commanded side keeps its guns
		// on the enemy's tackle at the aligning fraction while it leaves.
		var aFCmd, bFCmd *FleetCommand
		if aLed {
			aFCmd = aCmd
		}
		if bLed {
			bFCmd = bCmd
		}
		switch {
		case !aFleeing:
			aPrimary, aLanded = fire(a, b, aFCmd, false, 1)
		case aLed:
			aPrimary, aLanded = fire(a, b, aFCmd, true, abstractAligningFireFrac)
		}
		switch {
		case !bFleeing:
			bPrimary, bLanded = fire(b, a, bFCmd, false, 1)
		case bLed:
			bPrimary, bLanded = fire(b, a, bFCmd, true, abstractAligningFireFrac)
		}
		rep(a, aFCmd)
		rep(b, bFCmd)
		if escapes(bFleeing, tick-bFleeFrom, bWin, aPoints, bCore) {
			out.BEscaped = true
			break
		}
		if escapes(aFleeing, tick-aFleeFrom, aWin, bPoints, aCore) {
			out.AEscaped = true
			break
		}
	}
	for _, c := range a {
		if c != nil && c.EHP <= 0 {
			out.ADead = append(out.ADead, c)
		}
	}
	for _, c := range b {
		if c != nil && c.EHP <= 0 {
			out.BDead = append(out.BDead, c)
		}
	}
	return out
}

// pvpKillHits folds one side's damage into the killboard's attacker rows.
func pvpKillHits(attackers []*pvpCombatant) []destiny.KillHit {
	now := clock.Unix()
	var hits []destiny.KillHit
	var top *pvpCombatant
	for _, c := range attackers {
		if c == nil || c.Dealt <= 0 {
			continue
		}
		if top == nil || c.Dealt > top.Dealt {
			top = c
		}
	}
	for _, c := range attackers {
		if c == nil || c.Dealt <= 0 {
			continue
		}
		hits = append(hits, destiny.KillHit{
			Name:       c.Name,
			CharID:     c.CharID,
			CorpID:     c.CorpID,
			ShipTypeID: c.ShipTypeID,
			Damage:     c.Dealt,
			FinalBlow:  c == top,
			Time:       now,
		})
	}
	return hits
}

// killBotShip writes one side-member's death back to the world: the killmail through
// loseHull (filed VictimIsBot), sec-status for pirates on the other side and, when the dead
// pilot carried a price, the bounty paid to the killers by damage dealt, column zeroed.
func (m *Mod) killBotShip(ctx context.Context, victim *pvpCombatant, killers []*pvpCombatant, crime bool) {
	var x, y, z float64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
		victim.ShipID).Scan(&x, &y, &z)
	killerName := "hostile pilots"
	if top := len(killers); top > 0 && killers[0] != nil {
		killerName = killers[0].Name
	}
	// The dead pilot's ledger, and the gf memo the kill line reads (grace_ledger.go).
	m.noteGraceKill(ctx, victim, killers)
	// Said in Local if anybody is there to hear it (chatter_exchange.go).
	if len(killers) > 0 && killers[0] != nil {
		k := killers[0]
		kp := partyFromID(k.CharID, k.Name, k.CorpID, k.ShipTypeID, true)
		vp := partyFromID(victim.CharID, victim.Name, victim.CorpID, victim.ShipTypeID, true)
		m.chatterKill(victim.SystemID, kp, vp, lastMotive.get(k.CharID, victim.CharID))
	}
	// A pilot killed down a site leaves it (siteetiquette.go): its claim, if it
	// ran alone, and a memory of the site for its corporation.
	m.siteDropOnDeath(ctx, victim.CharID, victim.CorpID)
	capsuleID := m.loseHull(ctx, victim.CharID, victim.Name, victim.ShipID,
		victim.SystemID, x, y, z, killerName, pvpKillHits(killers))

	// The capsule now sits where the ship was; the top killer by damage decides whether it
	// lives (as markPod does, piracy_prey.go). One decision per fleet, not a roll per attacker.
	podded := false
	if capsuleID != 0 && len(killers) > 0 && killers[0] != nil {
		chance := killers[0].PodChance
		if chance <= 0 {
			chance = podKillBase()
		}
		if rand.Float64() < chance {
			podded = true
			m.noteGracePod(ctx, victim, killers)
			m.podBotAbstract(ctx, victim.CharID, victim.Name, victim.SystemID,
				capsuleID, x, y, z, killerName, pvpKillHits(killers))
			m.logBot(killers[0].CharID, logInfo, "combat",
				"finished %s's capsule", victim.Name)
		}
	}

	// The dead pilot's price, if any, split by damage dealt; no corp tax, a player bounty is
	// personal.
	var price float64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(bounty, 0) FROM character_ WHERE characterID = ?`,
		victim.CharID).Scan(&price); err == nil && price > 0 {
		var total float64
		for _, k := range killers {
			if k != nil {
				total += k.Dealt
			}
		}
		if total > 0 {
			if _, err := m.db.ExecContext(ctx,
				`UPDATE character_ SET bounty = 0 WHERE characterID = ?`, victim.CharID); err == nil {
				for _, k := range killers {
					if k == nil || k.Dealt <= 0 {
						continue
					}
					cut := price * (k.Dealt / total)
					if err := evedb.CreditBounty(ctx, m.db, k.CharID, cut); err != nil {
						continue
					}
					if bal, err := evedb.GetCharBalance(ctx, m.db, k.CharID); err == nil {
						_ = evedb.InsertJournalEntry(ctx, m.db, k.CharID, 17,
							concordPayerID, k.CharID, cut, bal,
							evedb.JournalReason("Bounty collected on "+victim.Name))
					}
					m.logBot(k.CharID, logInfo, "combat",
						"collected %.0f ISK of the bounty on %s", cut, victim.Name)
				}
			}
		}
	}

	if crime {
		m.chargeAbstractKillers(ctx, victim, killers, false)
		if podded {
			// A second crime, billed separately: the core's law prices a pod kill on its own line
			// (DefaultPodKillPenalty), as the embodied path does through handlePodLoss.
			m.chargeAbstractKillers(ctx, victim, killers, true)
		}
	}
}

// chargeAbstractKillers bills an ambush to every pirate that landed a shot, through the
// same core law the embodied path uses. Only the facts are assembled here and the core
// rules on them; the security band comes from the in-memory universe map. Every damage
// dealer pays in full: splitting one penalty would make ganging up the cheap way to murder.
func (m *Mod) chargeAbstractKillers(ctx context.Context, victim *pvpCombatant, killers []*pvpCombatant, pod bool) {
	hit := modapi.PvP().SecKillPenalty
	if hit == nil || victim == nil {
		return
	}
	security := 0.0
	if u, err := m.universe(ctx); err == nil && u != nil {
		security = u.meta[victim.SystemID].Security
	}
	for _, k := range killers {
		if k == nil || k.Dealt <= 0 {
			continue
		}
		applied, why := hit(nil, seclaw.KillFacts{
			KillerCharID:     k.CharID,
			KillerCorpID:     k.CorpID,
			KillerShipTypeID: k.ShipTypeID,
			VictimCharID:     victim.CharID,
			VictimCorpID:     victim.CorpID,
			VictimOutlaw:     victim.SecStatus <= seclaw.OutlawStatus,
			SystemID:         victim.SystemID,
			Security:         security,
			Pod:              pod,
		})
		if applied != 0 {
			m.logBot(k.CharID, logInfo, "security",
				"record docked %+.2f for killing %s", applied, victim.Name)
			m.notePoliceableCrime(ctx, victim.SystemID, k.CharID, victim.CharID, security)
		} else if why != "" {
			m.logBot(k.CharID, logInfo, "security",
				"killing %s was lawful (%s)", victim.Name, why)
		}
	}
}

// chargeAbstractAggression bills the side that started a fight, for every neutral pilot on
// the other side, and only for a shot that landed, after the fight. It returns the
// aggressors charged, which abstractConcord answers.
// See docs/comment-notes-b02.md#abstract-aggression.
func (m *Mod) chargeAbstractAggression(ctx context.Context, aggressors, victims []*pvpCombatant, sysID int64) []*pvpCombatant {
	// Every exchange is a PvP act, lawful or not: crimewatch timers first.
	for _, a := range aggressors {
		if a == nil || a.CharID == 0 || a.Dealt <= 0 {
			continue
		}
		for _, v := range victims {
			if v != nil && v.CharID != 0 && v.CharID != a.CharID && v.Taken > 0 {
				stampAbstractPvP(a.CharID, v.CharID)
			}
		}
	}
	hit := modapi.PvP().SecAggressionPenalty
	if hit == nil {
		return nil
	}
	security := 0.0
	if u, err := m.universe(ctx); err == nil && u != nil {
		security = u.meta[sysID].Security
	}
	var offenders []*pvpCombatant
	for _, a := range aggressors {
		if a == nil || a.CharID == 0 || a.Dealt <= 0 {
			continue
		}
		charged := false
		for _, v := range victims {
			if v == nil || v.CharID == 0 || v.CharID == a.CharID || v.Taken <= 0 {
				continue
			}
			applied, why := hit(nil, seclaw.KillFacts{
				KillerCharID:     a.CharID,
				KillerCorpID:     a.CorpID,
				KillerShipTypeID: a.ShipTypeID,
				VictimCharID:     v.CharID,
				VictimCorpID:     v.CorpID,
				VictimOutlaw:     v.SecStatus <= seclaw.OutlawStatus,
				SystemID:         sysID,
				Security:         security,
			})
			if applied != 0 {
				m.logBot(a.CharID, logInfo, "security",
					"record docked %+.2f for opening fire on %s", applied, v.Name)
				// A recognised crime is one CONCORD would answer if anybody could see it: remembered, not
				// dispatched, since no system is loaded (crimescene.go). `applied != 0` is the gate.
				m.notePoliceableCrime(ctx, sysID, a.CharID, v.CharID, security)
				charged = true
			} else if why != "" {
				m.logBot(a.CharID, logInfo, "security",
					"opening fire on %s was lawful (%s)", v.Name, why)
			}
		}
		if charged {
			offenders = append(offenders, a)
		}
	}
	// The relations ledger hears every landed shot, lawful or not: a corporation shot at in
	// lowsec holds it against the shooter (corprelations_hooks.go).
	m.noteAggressionIncidents(ctx, aggressors, victims, sysID)
	// And each bot shot at holds it against each shooter, a little (grace_ledger.go).
	for _, v := range victims {
		if v == nil || v.Taken <= 0 {
			continue
		}
		for _, a := range aggressors {
			if a != nil && a.Dealt > 0 {
				m.noteGraceEventAsync(v.CharID, a.CharID, gkAggress, fightFacts{}, sysID, 0)
			}
		}
	}
	return offenders
}

// chargeAbstractRobbery bills a hold-up that ended in payment through the same exemption
// ladder chargeAbstractKillers uses. Every pirate in the pack pays, not just the leader,
// since the guns made the mark pay. The core decides the cost
// (modapi.PvPHooks.SecRobberyPenalty); it returns the pirates charged, for abstractConcord.
func (m *Mod) chargeAbstractRobbery(ctx context.Context, victim *pvpCombatant,
	pirates []*pvpCombatant, sysID int64) []*pvpCombatant {

	// A hold-up is a PvP act: the tackle that made it is aggression.
	if victim != nil {
		for _, p := range pirates {
			if p != nil {
				stampAbstractPvP(p.CharID, victim.CharID)
			}
		}
	}
	hit := modapi.PvP().SecRobberyPenalty
	if hit == nil || victim == nil {
		return nil
	}
	security := 0.0
	if u, err := m.universe(ctx); err == nil && u != nil {
		security = u.meta[sysID].Security
	}
	var offenders []*pvpCombatant
	for _, p := range pirates {
		if p == nil || p.CharID == 0 {
			continue
		}
		applied, why := hit(nil, seclaw.KillFacts{
			KillerCharID:     p.CharID,
			KillerCorpID:     p.CorpID,
			KillerShipTypeID: p.ShipTypeID,
			VictimCharID:     victim.CharID,
			VictimCorpID:     victim.CorpID,
			VictimOutlaw:     victim.SecStatus <= seclaw.OutlawStatus,
			SystemID:         sysID,
			Security:         security,
		})
		if applied != 0 {
			m.logBot(p.CharID, logInfo, "security",
				"record docked %+.2f for robbing %s", applied, victim.Name)
			m.notePoliceableCrime(ctx, sysID, p.CharID, victim.CharID, security)
			offenders = append(offenders, p)
		} else if why != "" {
			m.logBot(p.CharID, logInfo, "security",
				"robbing %s was lawful (%s)", victim.Name, why)
		}
	}
	return offenders
}

// fileVictimBounty makes pirates huntable: the victim puts a flat price on each head
// involved out of its own wallet (AddToBounty debits only if affordable, so a broke victim
// files nothing), with a journal row. character_.bounty is what the Most Wanted list reads
// and the hunt pass scans.
func (m *Mod) fileVictimBounty(ctx context.Context, victimCharID int64, pirates []*pvpCombatant) {
	price := float64(PirateBountyISK())
	if price <= 0 {
		return
	}
	for _, p := range pirates {
		if p == nil || p.EHP <= 0 { // no bounty on the already-dead
			continue
		}
		if err := evedb.AddToBounty(ctx, m.db, victimCharID, p.CharID, price); err != nil {
			continue // insufficient balance, most likely: the victim is broke
		}
		if bal, err := evedb.GetCharBalance(ctx, m.db, victimCharID); err == nil {
			_ = evedb.InsertJournalEntry(ctx, m.db, victimCharID, 17,
				victimCharID, p.CharID, -price, bal,
				evedb.JournalReason("Bounty placed on "+p.Name))
		}
		m.logBot(victimCharID, logInfo, "combat",
			"put %.0f ISK on %s's head", price, p.Name)
	}
}

// ── predation ───────────────────────────────────────────────────────────────

// piratePredationPass finds every pirate prowling or camping an unwatched
// system and gives it the evening it went out for.
func (m *Mod) piratePredationPass(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID, c.solarSystemID
		FROM rolebot_state s
		JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = s.characterID
		WHERE s.phase IN (?, ?, ?, ?, ?) AND COALESCE(c.stationID, 0) = 0
		ORDER BY RAND()`,
		string(TaskPiracyProwl), string(TaskPiracyCamp),
		string(TaskGangPirateProwl), string(TaskGangPirateCamp),
		string(TaskGangGankScout))
	if err != nil {
		rlogf("rolebots: botpvp predation scan: %v", err)
		return
	}
	type prowler struct {
		charID, sysID int64
	}
	var prowlers []prowler
	for rows.Next() {
		var p prowler
		// A logged-off pirate hunts nothing (timezone.go, popcap.go).
		if rows.Scan(&p.charID, &p.sysID) == nil && !popIsOffline(p.charID) {
			prowlers = append(prowlers, p)
		}
	}
	rows.Close()

	done := 0
	seenGroup := map[int64]bool{} // gangID, or -charID for loners
	for _, p := range prowlers {
		if done >= botPvPMaxPredations || ctx.Err() != nil {
			return
		}
		if m.systemProjected(p.sysID) {
			continue // a projected grid already has the embodied machinery
		}
		g, _, err := gangForChar(ctx, m.db, p.charID)
		if err != nil {
			continue
		}
		groupKey := -p.charID
		if g != nil {
			groupKey = g.GangID
		}
		if seenGroup[groupKey] {
			continue
		}
		seenGroup[groupKey] = true
		// A gank fleet's scout in an unwatched system runs the gank resolver (gank_abstract.go),
		// never the pirate ambush: it is unarmed.
		if g != nil && g.State.Focus == gangFocusGank {
			if m.gankOnce(ctx, p.charID, p.sysID, g) {
				done++
			}
			continue
		}
		if m.predateOnce(ctx, p.charID, p.sysID, g) {
			done++
		}
	}
}

// predateOnce runs one pirate group's ambush attempt in one system.
// Reports whether an engagement actually happened.
func (m *Mod) predateOnce(ctx context.Context, leaderCharID, sysID int64, g *Gang) bool {
	lb, err := m.loadRun(ctx, leaderCharID)
	if err != nil {
		return false
	}

	// The pirate side: the leader plus its pack-mates standing in the same
	// system with hulls under them.
	var pirates []*pvpCombatant
	if lead := m.pvpCombatantFor(ctx, leaderCharID, RoleCombat); lead != nil {
		pirates = append(pirates, lead)
	} else {
		return false
	}
	if g != nil {
		members, err := gangMembers(ctx, m.db, g.GangID)
		if err == nil {
			for _, mem := range members {
				if !mem.Driven || mem.CharacterID == leaderCharID {
					continue
				}
				pc := m.pvpCombatantFor(ctx, mem.CharacterID, mem.Role)
				if pc == nil || pc.SystemID != sysID {
					continue
				}
				pirates = append(pirates, pc)
			}
		}
	}

	// The prey: a working, exposed bot in the same system that is not ours, not corp family and
	// not under this pass's amnesty. Outside policed space a site runner is the pick (raid.go).
	siteBias := raidOn() && secBand(m.systemSecurity(ctx, sysID)) != BandPoliced
	victimID, siteVictim := m.pickAbstractPrey(ctx, sysID, pirates, siteBias)
	if victimID == 0 {
		return false
	}
	// The ambush roll: how often prowling corners somebody, scaled by the leader's hunger for
	// trouble; a pilot pinned in a deadspace pocket is easier to corner.
	chance := botPvPAmbushBase * (lb.Tune.PatrolEncounterChance / abstractPatrolEncounterChance)
	if siteVictim {
		chance *= dialRaidAmbushMul.Get()
	}
	if rand.Float64() >= chance {
		return false
	}

	vb, err := m.loadRun(ctx, victimID)
	if err != nil {
		return false
	}
	victim := m.pvpCombatantFor(ctx, victimID, RoleCombat)
	if victim == nil {
		return false
	}
	// The victim's own friends in the system stand with it.
	var victimSide []*pvpCombatant
	victimSide = append(victimSide, victim)
	if vb.State != nil && vb.State.Data.GangID != 0 {
		mates, err := m.gangCombatRoster(ctx, vb.State.Data.GangID, victimID)
		if err == nil {
			for _, mate := range mates {
				if mate.SystemID != sysID {
					continue
				}
				if pc := m.pvpCombatantFor(ctx, mate.CharID, RoleCombat); pc != nil {
					victimSide = append(victimSide, pc)
				}
			}
		}
	}

	// The grudge belongs to whoever was jumped, not the pack: this map is where a bot got
	// attacked working (postureFor().patrol() draws the same line). Each victim's employer learns it.
	for _, v := range victimSide {
		if v != nil {
			noteHostileEncounter(intelScope{corpID: v.CorpID}, sysID)
		}
	}

	// The demand. The leader decides whether this is a robbery or an
	// execution; the victim decides whether it is worth its money, its
	// hull, or its heels.
	var pDPS, pEHP, vDPS, vEHP float64
	for _, c := range pirates {
		pDPS += c.DPS
		pEHP += c.EHP
	}
	for _, c := range victimSide {
		vDPS += c.DPS
		vEHP += c.EHP
	}
	victimMargin := engageMargin(vDPS, vEHP, pDPS, pEHP)
	victimFights := wouldEngage(vb.Tune, victimMargin)
	// Said in Local if a human is in it (chatter_exchange.go).
	m.chatterEngageAbstract(ctx, lb, vb, sysID, motivePiracy)

	if !victimFights && rand.Float64() < lb.Tune.RansomChance {
		// A hold worth far more than the price is asked for outright, and a bot that would rather
		// lose the cargo than the ship hands it over (camp.go abstractCargoDemand).
		if m.abstractCargoDemand(ctx, lb, vb, victim, pirates, sysID) {
			return true
		}
		price := ransomBaseISK * lb.Tune.RansomMul
		if cargo, err := m.holdUsedM3(ctx, victimID, victim.ShipID); err == nil && cargo > 0 {
			price *= 1 + math.Sqrt(cargo/100)
		}
		price = math.Round(price/10_000) * 10_000
		bal, _ := evedb.GetCharBalance(ctx, m.db, victimID)
		if price > 0 && bal >= price*1.2 {
			// The victim pays: cheaper than the hull, and the pirate's
			// amnesty means it stays paid for the evening.
			if err := m.handISK(ctx, victimID, leaderCharID, price); err == nil {
				m.logBot(leaderCharID, logInfo, "piracy",
					"held up %s in %s — %.0f ISK, paid", vb.Name, m.nameOfSystem(ctx, sysID), price)
				m.logBot(victimID, logInfo, "piracy",
					"paid %.0f ISK to keep the ship — cheaper than the fight", price)
				// The pack's cut, evenly, from the leader's hand.
				if n := len(pirates) - 1; n > 0 {
					each := price / float64(n+1)
					for _, p := range pirates[1:] {
						_ = m.handISK(ctx, leaderCharID, p.CharID, each)
					}
				}
				grantAmnesty(victimID)
				m.noteGraceEventAsync(victimID, leaderCharID, gkRansomOK, fightFacts{ISK: price}, sysID, 0)
				m.piracyRecordVictim(ctx, leaderCharID, victimID, false)
				_ = bumpPiracyTally(ctx, m.db, leaderCharID, false)
				m.fileVictimBounty(ctx, victimID, pirates)
				m.abstractConcord(ctx, m.chargeAbstractRobbery(ctx, victim, pirates, sysID), sysID, 0, false)
				var pirateCorps []int64
				for _, p := range pirates {
					if p != nil {
						pirateCorps = append(pirateCorps, p.CorpID)
					}
				}
				m.noteRobberyIncident(ctx, victimID, victim.CorpID, vb.Name, pirateCorps, price, sysID)
				return true
			}
		}
		// Could not pay (or the transfer failed): the demand collapses
		// into the chase below.
		victim.RansomAsked = true
	}

	// No deal: the victim runs if it would not fight; the fight comes to it either way if the
	// tackle holds. Commands are copies (fleetcommand.go); a lost commander is replaced and committed.
	pirateCmd, pirateBefore := fleetCommandOf(g), fleetCommandOf(g)
	victimCmd, victimGang := m.fleetCommandForChar(ctx, victimID)
	var victimBefore *FleetCommand
	if victimCmd != nil {
		c := *victimCmd
		victimBefore = &c
	}
	out := resolveTeamFightCmd(pirates, victimSide,
		lb.Tune.BreakOffFrac, vb.Tune.BreakOffFrac, !victimFights, pirateCmd, victimCmd)
	if g != nil {
		m.commitFleetCommand(ctx, g.GangID, pirateBefore, pirateCmd)
	}
	m.commitFleetCommand(ctx, victimGang, victimBefore, victimCmd)
	// Billed after the fight and only for shots that landed (chargeAbstractAggression);
	// CONCORD answers the offenders it names once the dead are filed below.
	offenders := m.chargeAbstractAggression(ctx, pirates, victimSide, sysID)
	defer m.abstractConcord(ctx, offenders, sysID, float64(out.Ticks), out.AEscaped)

	// The dead, both sides. Pirate deaths pay their own bounties out to
	// the victim's side inside killBotShip.
	for _, dead := range out.BDead {
		m.killBotShip(ctx, dead, pirates, true)
		m.piracyRecordVictim(ctx, leaderCharID, dead.CharID, true)
		_ = bumpPiracyTally(ctx, m.db, leaderCharID, true)
	}
	for _, dead := range out.ADead {
		m.killBotShip(ctx, dead, victimSide, false)
	}
	m.logBot(leaderCharID, logInfo, "combat",
		"engagement result: %s; ambush window=%.0fs held=%t",
		pvpOutcomeLabel(out), sideEscapeWindow(victimSide), sideTacklePoints(pirates) > sideWarpCore(victimSide))
	m.pvpDroneAttrition(ctx, pirates, false)
	m.pvpDroneAttrition(ctx, victimSide, out.BEscaped)
	// A victim that died, or was held and shaken down for a fight it then lost, files on
	// everyone still flying.
	victimDied := false
	for _, dead := range out.BDead {
		if dead.CharID == victimID {
			victimDied = true
		}
	}
	if victimDied {
		m.fileVictimBounty(ctx, victimID, pirates)
		// The pack goes back for the wreck (raid.go): an abstract kill leaves a container.
		if canID, canSys := takeLastWreck(victimID); canID != 0 {
			m.pointPirateAtWreck(leaderCharID, canID, canSys)
		}
	} else if out.BEscaped {
		m.logBot(victimID, logInfo, "piracy", "slipped a pirate ambush in %s",
			m.nameOfSystem(ctx, sysID))
	}
	return true
}

// pickAbstractPrey finds one working bot worth robbing in the system: exposed, hull under
// it, not in the pirates' gang or corporation, not under amnesty. Random among candidates so
// a busy belt spreads the pain. siteBias weighs a site runner up (raid.go); siteVictim
// reports that the pick was one.
func (m *Mod) pickAbstractPrey(ctx context.Context, sysID int64, pirates []*pvpCombatant,
	siteBias bool) (pick int64, siteVictim bool) {
	pirateChars := map[int64]bool{}
	pirateCorps := map[int64]bool{}
	for _, p := range pirates {
		if p != nil {
			pirateChars[p.CharID] = true
			pirateCorps[p.CorpID] = true
		}
	}
	var phases []string
	for kind, def := range taskRegistry {
		if def.Exposed {
			phases = append(phases, string(kind))
		}
	}
	args := []interface{}{}
	for _, p := range phases {
		args = append(args, p)
	}
	args = append(args, sysID)
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID, c.corporationID, s.phase
		FROM rolebot_state s
		JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = s.characterID
		WHERE s.phase IN (`+placeholders(len(phases))+`)
		  AND c.solarSystemID = ? AND COALESCE(c.stationID, 0) = 0
		ORDER BY RAND()`, args...)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	// Weighted reservoir over every acceptable victim: uniform among strangers, but a victim
	// from a corporation the pack's own has marked (preyMarkWeight) is several times likelier.
	total := 0.0
	for rows.Next() {
		var charID, corpID int64
		var phase string
		// A logged-off ship is not in space to be caught.
		if rows.Scan(&charID, &corpID, &phase) != nil || popIsOffline(charID) {
			continue
		}
		if pirateChars[charID] || pirateCorps[corpID] || underAmnesty(charID) {
			continue
		}
		// Kin: the same alliance or the same employer faction (kinship.go). Asked of every pirate
		// in the pack, so one Guristas pilot in the gang spares a Guristas colleague.
		kin := false
		for _, p := range pirates {
			if p != nil && m.kinToPilot(ctx, p.Self, charID) {
				kin = true
				break
			}
		}
		if kin {
			continue
		}
		// Somebody the pack thinks well of is not prey (wouldRob, piracy_prey.go); one pack member's
		// friendship is enough. A friend is spared, a stranger of an opposed flag is not (standings.go).
		spared := false
		for _, p := range pirates {
			if p != nil && m.standingToPilot(ctx, p.Self, charID) >= StandingLiked {
				spared = true
				break
			}
		}
		if spared {
			continue
		}
		rw := raidPreyWeight(siteBias, phase)
		w := preyMarkWeight(pirateCorps, corpID) * rw
		total += w
		if rand.Float64()*total < w {
			pick, siteVictim = charID, rw > 1
		}
	}
	return pick, siteVictim
}

// preyMarkWeight is how much likelier a pirate pack is to pick a victim whose corporation
// its own has declared a rival (x3) or an enemy (x5): the soft industrial outfits a pirate
// CEO marks as prey (corpceo_diplomacy.go). Pure snapshot reads; 1 for everybody else.
func preyMarkWeight(pirateCorps map[int64]bool, victimCorp int64) float64 {
	w := 1.0
	for pc := range pirateCorps {
		switch v := relCorpStanding(pc, victimCorp); {
		case v <= StandingDespised:
			w = math.Max(w, 5)
		case v <= StandingDisliked:
			w = math.Max(w, 3)
		}
	}
	return w
}

// ── the hunt ────────────────────────────────────────────────────────────────

// bountyHuntPass turns the bounty board into consequences: fleets and solo hunters on the
// bounty activity that can hold a target (a fitted point; without one it would simply warp
// off) commit to intercepting bountied pilots in their region.
func (m *Mod) bountyHuntPass(ctx context.Context) {
	// Gangs first: a live bounty fleet whose blackboard either carries a
	// matured intercept or has room for a new one.
	rows, err := m.db.QueryContext(ctx, `
		SELECT gangID, leaderCharID, COALESCE(state, '')
		FROM rolebot_gangs
		WHERE enabled = 1 AND activity = ? ORDER BY RAND()`, ActCombat)
	if err != nil {
		rlogf("rolebots: botpvp hunt scan: %v", err)
		return
	}
	type fleet struct {
		gangID, leaderID int64
		state            GangState
	}
	var fleets []fleet
	for rows.Next() {
		var f fleet
		var raw string
		if rows.Scan(&f.gangID, &f.leaderID, &raw) != nil {
			continue
		}
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &f.state)
		}
		if f.state.OpPhase == "" || f.state.OpPhase == gangOpLive {
			fleets = append(fleets, f)
		}
	}
	rows.Close()

	// One fleet per mark: two fleets of one corporation committed to the same pilot would arrive
	// as two separate fights. The set is every live fleet's standing claim, grown as this pass commits.
	claimed := map[int64]bool{}
	for _, f := range fleets {
		claimHuntMark(claimed, f.state.HuntTargetCharID)
	}

	done := 0
	for _, f := range fleets {
		if done >= botPvPMaxHunts || ctx.Err() != nil {
			return
		}
		if m.huntOnce(ctx, &f.state, f.gangID, f.leaderID, claimed) {
			done++
		}
	}
}

// huntOnce advances one fleet's capsuleer hunt: commit to a target and fly
// at it, follow it, or close on it once the fleet is actually there
// (bountyhunt.go). Reports whether a fight actually resolved.
func (m *Mod) huntOnce(ctx context.Context, gs *GangState, gangID, leaderID int64, claimed map[int64]bool) bool {
	lb, err := m.loadRun(ctx, leaderID)
	if err != nil {
		return false
	}

	// The fleet, with its tackle. No point on the field, no hunt.
	members, err := gangMembers(ctx, m.db, gangID)
	if err != nil {
		return false
	}
	var side []*pvpCombatant
	for _, mem := range members {
		if !mem.Driven {
			continue
		}
		if pc := m.pvpCombatantFor(ctx, mem.CharacterID, mem.Role); pc != nil {
			side = append(side, pc)
		}
	}
	now := clock.Unix()
	if gs.HuntTargetCharID == 0 {
		if len(side) == 0 || !sideHasTackle(side) {
			return false
		}
		// A fleet answering a call or running a site is not free to chase a mark (gangintent.go).
		if !objectiveMayTake(gs, objIntercept, objectivePrio(objIntercept), now) {
			return false
		}
		// The fleet commits from where it is: most of it in one system, the mark close to that
		// system. A pack strung across a region would arrive one ship at a time.
		home, gathered := huntFleetCentre(side)
		if !huntFleetGathered(gathered, len(side)) {
			return false
		}
		// Commit: a bountied pilot in this region, in unwatched space, outside the family and not
		// already another fleet's, if the margin pooled across the whole fleet says it wins.
		target, sysID, jumps := m.findBountiedTarget(ctx, lb, side, claimed, huntInterceptMaxJumps)
		if target == 0 {
			return false
		}
		if u, uerr := m.universe(ctx); uerr == nil && u != nil {
			if _, near := hopsWithin(u, home, huntCommitMaxJumps)[sysID]; !near {
				return false
			}
		}
		where := m.nameOfSystem(ctx, sysID)
		// The fleet arrives when its slowest member does (fleetmove.go), at
		// the slowest hull's own pace across a gate.
		arriveAt := fleetArrivalETA(now, m.fleetMemberJumps(ctx, lb, side, sysID), fleetSecPerJump(sideMaxAlign(side)))
		v, err := m.commitObjective(ctx, gangID, gs, GangObjective{
			Kind: objIntercept, Ref: target, SystemID: sysID,
			Prio: objectivePrio(objIntercept), Until: now + gangHuntSweepSec,
		}, func(st *GangState) {
			st.HuntTargetCharID = target
			dropSiteOffHunt(st, gangID, sysID)
			pinHuntSite(st, sysID, where, now)
			st.HuntETA = now + gangHuntSweepSec
			st.HuntArriveAt = arriveAt
		})
		if err != nil || v == objRefuse || v == objSame {
			return false
		}
		claimHuntMark(claimed, target)
		m.logBot(leaderID, logInfo, "combat",
			"the fleet has a name — a bountied pilot in %s, %d jump(s) out; the pack is flying there",
			where, jumps)
		return false
	}

	// A committed intercept. The mark is re-read every pass, because it is
	// a pilot with its own plans, not a pin on a map.
	targetID := gs.HuntTargetCharID
	drop := func(why string) bool {
		_ = m.writeHuntState(ctx, gangID, gs, func(st *GangState) {
			if st.HuntTargetCharID == targetID {
				st.HuntTargetCharID, st.HuntETA, st.HuntArriveAt = 0, 0, 0
			}
		})
		m.logBot(leaderID, logInfo, "combat", "lost the trail of a bountied pilot — %s", why)
		return false
	}
	tb, err := m.loadRun(ctx, targetID)
	if err != nil || tb == nil {
		return drop("the mark is gone")
	}
	var price float64
	if err := m.db.QueryRowContext(ctx,
		`SELECT COALESCE(bounty, 0) FROM character_ WHERE characterID = ?`,
		targetID).Scan(&price); err != nil {
		price = 0
	}
	present := huntSideInSystem(side, tb.SystemID)
	moved := tb.SystemID != gs.HuntSystemID
	inReach := false
	if moved {
		if u, uerr := m.universe(ctx); uerr == nil && u != nil {
			_, inReach = gangReachMap(u, gs.HuntSystemID, huntInterceptMaxJumps)[tb.SystemID]
		}
	}
	step := huntInterceptStep(huntView{
		Now:      now,
		Deadline: gs.HuntETA,
		Priced:   price > 0,
		// The mark may have moved while the fleet flew: a hunt committed in lowsec is no licence to
		// follow it through a highsec gate (pvplaw.go).
		Legal:         m.huntMarkLegal(ctx, lb, m.corpDoctrineFor(ctx, lb.CorpID), tb),
		Docked:        tb.StationID != 0,
		Moved:         moved,
		MovedInReach:  inReach,
		TacklePresent: sideHasTackle(present),
		ArriveAt:      gs.HuntArriveAt,
		AllPresent:    len(present) >= len(side),
	})
	switch step {
	case huntWait:
		return false
	case huntRetarget:
		where := m.nameOfSystem(ctx, tb.SystemID)
		// The same objective, somewhere else (gangintent.go). A fleet pulled onto something that
		// outranks the chase, such as a call to arms, drops the mark.
		v, err := m.commitObjective(ctx, gangID, gs, GangObjective{
			Kind: objIntercept, Ref: targetID, SystemID: tb.SystemID,
			Prio: objectivePrio(objIntercept), Until: gs.HuntETA,
		}, func(st *GangState) {
			// The deadline and the sweep clock stay one clock: the site moves,
			// its start does not.
			dropSiteOffHunt(st, gangID, tb.SystemID)
			pinHuntSite(st, tb.SystemID, where, st.HuntPickedAt)
		})
		if err != nil {
			return false
		}
		if v == objRefuse {
			return drop("the fleet is committed elsewhere")
		}
		m.logBot(leaderID, logInfo, "combat", "the mark moved on to %s — the pack follows", where)
		return false
	case huntAbandon:
		switch {
		case price <= 0:
			return drop("somebody else collected the price")
		case moved:
			return drop("it moved out of reach")
		case tb.StationID != 0:
			return drop("it stayed docked")
		}
		return drop("the fleet never closed in time")
	}

	// huntResolve: on the mark, with a point. A projected system's grid owns the fight, since
	// the arriving pack is embodied there; this tier waits it out.
	if m.systemProjected(tb.SystemID) {
		if now >= gs.HuntETA {
			return drop("the grid there took the fight out of its hands")
		}
		return false
	}
	_ = m.writeHuntState(ctx, gangID, gs, func(st *GangState) {
		if st.HuntTargetCharID == targetID {
			st.HuntTargetCharID, st.HuntETA, st.HuntArriveAt = 0, 0, 0
		}
	})
	// Only the members standing here fight, so the killmails, wrecks, pods, aggression charge
	// and CONCORD all agree on one system (tb.SystemID).
	side = present
	target := m.pvpCombatantFor(ctx, targetID, RoleCombat)
	if target == nil {
		return false
	}
	targetSide := []*pvpCombatant{target}
	if tb.State != nil && tb.State.Data.GangID != 0 {
		mates, err := m.gangCombatRoster(ctx, tb.State.Data.GangID, targetID)
		if err == nil {
			for _, mate := range mates {
				if mate.SystemID != tb.SystemID {
					continue
				}
				if pc := m.pvpCombatantFor(ctx, mate.CharID, RoleCombat); pc != nil {
					targetSide = append(targetSide, pc)
				}
			}
		}
	}
	// The mark passed the leader's rules (huntMarkLegal); each member's shot is judged on its own
	// corp and alliance, so a member who may not fire on all of them stands aside (pvplaw.go).
	side = m.lawfulAttackers(ctx, tb.SystemID, secBand(m.systemSecurity(ctx, tb.SystemID)), side, targetSide)
	if len(side) == 0 {
		return drop("nobody in the pack may lawfully open fire there")
	}

	var hDPS, hEHP, tDPS, tEHP float64
	for _, c := range side {
		hDPS += c.DPS
		hEHP += c.EHP
	}
	for _, c := range targetSide {
		tDPS += c.DPS
		tEHP += c.EHP
	}
	targetFights := wouldEngage(tb.Tune, engageMargin(tDPS, tEHP, hDPS, hEHP))

	// The hunted pilot's corp learns this system is dangerous, not the hunters'.
	noteHostileEncounter(botScope(tb), tb.SystemID)
	m.logBot(leaderID, logInfo, "combat", "ran %s down in %s — %s",
		tb.Name, m.nameOfSystem(ctx, tb.SystemID),
		map[bool]string{true: "and it turned to fight", false: "and it ran for the gate"}[targetFights])
	m.chatterEngageAbstract(ctx, lb, tb, tb.SystemID, motiveBounty)

	// The hunters started it: a bounty does not make a pilot a legal target, only an outlaw
	// record does, so the chase is a crime in empire space. Commands are copies (fleetcommand.go).
	ourCmd := fleetCommandOfState(gs)
	ourBefore := fleetCommandOfState(gs)
	theirCmd, theirGang := m.fleetCommandForChar(ctx, targetID)
	var theirBefore *FleetCommand
	if theirCmd != nil {
		c := *theirCmd
		theirBefore = &c
	}
	out := resolveTeamFightCmd(side, targetSide,
		lb.Tune.BreakOffFrac, tb.Tune.BreakOffFrac, !targetFights, ourCmd, theirCmd)
	m.commitFleetCommand(ctx, gangID, ourBefore, ourCmd)
	m.commitFleetCommand(ctx, theirGang, theirBefore, theirCmd)
	offenders := m.chargeAbstractAggression(ctx, side, targetSide, tb.SystemID)
	defer m.abstractConcord(ctx, offenders, tb.SystemID, float64(out.Ticks), out.AEscaped)
	for _, dead := range out.BDead {
		m.killBotShip(ctx, dead, side, false) // pays the bounty inside
	}
	for _, dead := range out.ADead {
		m.killBotShip(ctx, dead, targetSide, false)
	}
	m.fileCrimeBounties(ctx, out.BDead, side, offenders)
	m.logBot(leaderID, logInfo, "combat",
		"engagement result: %s; hunt window=%.0fs held=%t",
		pvpOutcomeLabel(out), sideEscapeWindow(targetSide), sideTacklePoints(side) > sideWarpCore(targetSide))
	m.pvpDroneAttrition(ctx, side, false)
	m.pvpDroneAttrition(ctx, targetSide, out.BEscaped)
	if out.BEscaped {
		m.logBot(leaderID, logInfo, "combat", "%s slipped the point and warped clear", tb.Name)
	}
	return true
}

// findBountiedTarget scans for a bountied pilot in the hunter's region the fleet would take
// on. It returns the target, its system and a rough jump count. skip holds marks another
// fleet already claimed (nil skips nothing); maxJumps > 0 drops marks further than that from
// the leader.
func (m *Mod) findBountiedTarget(ctx context.Context, lb *botRun, side []*pvpCombatant,
	skip map[int64]bool, maxJumps int) (int64, int64, int) {
	var hDPS, hEHP float64
	for _, c := range side {
		hDPS += c.DPS
		hEHP += c.EHP
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT c.characterID, c.solarSystemID, c.bounty
		FROM character_ c
		JOIN rolebot_characters rb ON rb.characterID = c.characterID AND rb.enabled = 1
		JOIN mapSolarSystems t ON t.solarSystemID = c.solarSystemID
		JOIN mapSolarSystems h ON h.solarSystemID = ?
		WHERE c.bounty > 0 AND COALESCE(c.stationID, 0) = 0
		  AND t.regionID = h.regionID
		  AND c.corporationID <> ? AND c.characterID <> ?
		ORDER BY c.bounty DESC LIMIT 40`, lb.SystemID, lb.CorpID, lb.CharID)
	if err != nil {
		return 0, 0, 0
	}
	defer rows.Close()
	u, uerr := m.universe(ctx)
	reachJumps := 20
	if maxJumps > 0 {
		reachJumps = maxJumps
	}
	var reach map[int64]int
	if uerr == nil && u != nil {
		reach = gangReachMap(u, lb.SystemID, reachJumps)
	}
	doc := m.corpDoctrineFor(ctx, lb.CorpID)
	for rows.Next() {
		var charID, sysID int64
		var price float64
		if rows.Scan(&charID, &sysID, &price) != nil {
			continue
		}
		if skip[charID] || popIsOffline(charID) {
			continue
		}
		if _, ok := reach[sysID]; maxJumps > 0 && !ok {
			continue
		}
		// Projected, not merely watched: a mark with a ball on grid is the
		// embodied tier's to kill, ring system or not.
		if m.systemProjected(sysID) {
			continue
		}
		// Kin: same alliance, same employer faction (kinship.go). A bounty is no reason for the
		// mark's own bloc to collect; somebody outside the bloc still takes the contract.
		if m.kinToPilot(ctx, lb.standingSelf(), charID) {
			continue
		}
		// Same-gang targets rob the hunt of meaning; skip family.
		if g, _, _ := gangForChar(ctx, m.db, charID); g != nil {
			if hg, _, _ := gangForChar(ctx, m.db, lb.CharID); hg != nil && hg.GangID == g.GangID {
				continue
			}
		}
		// The rules of engagement the engagement pass keeps: a price is a reason to chase where
		// nobody polices and none where CONCORD answers (pvplaw.go huntLegal). Friends are spared.
		if m.standingToPilot(ctx, lb.standingSelf(), charID) >= StandingLiked {
			continue
		}
		mark, merr := m.loadRun(ctx, charID)
		if merr != nil || !m.huntMarkLegal(ctx, lb, doc, mark) {
			continue
		}
		target := m.pvpCombatantFor(ctx, charID, RoleCombat)
		if target == nil {
			continue
		}
		if lethalityOn() {
			// Can the fleet CATCH it, not does it outweigh it (pvpescape.go).
			markFights := wouldEngage(mark.Tune, engageMargin(target.DPS, target.EHP, hDPS, hEHP))
			if engageVerdict(side, []*pvpCombatant{target}, markFights) < dialMinKillOdds.Get() {
				continue
			}
		} else if !wouldEngage(lb.Tune, engageMargin(hDPS, hEHP, target.DPS, target.EHP)) {
			continue // the fleet does not fly at a fight it loses
		}
		jumps := 3
		if j, ok := reach[sysID]; ok {
			jumps = j
		}
		return charID, sysID, jumps
	}
	return 0, 0, 0
}
