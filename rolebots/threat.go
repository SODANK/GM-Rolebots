package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// Threat assessment: the question a pilot asks before it commits. It folds a region's rat roster (npcRegionSpawns,
// npcSpawnerRules, ratThreatFor) into what a belt there puts on the field and compares it with what the bot
// brings (botCombatant). The verdict is one number:
//
//	margin = (how long the bot survives) / (how long the spawn survives)
//
// Above 1 it wins on paper; below 1 it is a suicide run. A pilot draws its own line at Tuning.EngageMargin. The
// same margin drives the hunter's site pick (combatSite), the miner's belt cost (beltCost) and the abstract
// tier's decline of a fight it cannot win (resolveAbstractEncounter), and a gang runs all of it against the
// pack's pooled guns.

const (
	// defaultEngageMargin is the line an even-tempered pilot draws: it wants
	// to outlast the spawn by a third before it commits. Traits bend it.
	defaultEngageMargin = 1.3

	// marginRefuse is the floor no amount of recklessness crosses: below it the arithmetic says the ship dies. The
	// site search skips these systems outright rather than costing them.
	marginRefuse = 0.6

	// marginOverwhelming is what the sums return when the other side cannot
	// hurt the bot at all. A real number rather than +Inf so it can be
	// compared, logged and multiplied without poisoning the arithmetic.
	marginOverwhelming = 999.0

	// threatOverwhelmedCost is what a belt this pilot cannot win on is worth flying to avoid, in jumps, at margin
	// zero. It is scaled per pilot by how much its nerve already prices a ratted belt at (Tuning.HostileCost against
	// the module default), so caution composes here.
	threatOverwhelmedCost = 45.0

	// minerEscapeSec is how long a mining hull has to stay alive to get off a belt: notice, align, spool up, warp.
	// The abstract duel gives an aligning bot a 25% chance per one-second tick (abstractEscapePerTick); the embodied
	// tier flies the align for real and is the slower, so this is sized against it.
	minerEscapeSec = 20.0

)

// spawnThreat is what a belt in one region at one security actually puts on
// the field, folded to the numbers a decision needs.
type spawnThreat struct {
	// The expected spawn: mid-sized for the security band, drawn from the region's roster by weight. EHP counts the
	// armed rats' hitpoints only: an unarmed hauler rat is a free kill once the guns around it are dead, so its hull
	// does not stand between the pilot and a clear belt.
	DPS float64
	EHP float64
	// Ships in that expected spawn, armed or not.
	Ships int
	// ArmedShips is how many of those Ships shoot: Ships scaled by the armed
	// share of the roster's weight. Zero means "not measured" and reads as
	// Ships, which is what a hand-built threat in a test means by it.
	ArmedShips float64
	// Rats is the spawn rat by rat when it is actually on the field
	// (spawnThreatOf). The regional estimate has no list and prices the
	// expected wave as ArmedShips equal rats instead (seqDamageTaken).
	Rats []ratLine

	// The single most dangerous thing in the roster. The expected spawn is a weighted mean and a mean hides tails: a
	// pilot that cannot survive this one hull alone has no business on these belts whatever the average says.
	HeaviestDPS  float64
	HeaviestEHP  float64
	HeaviestName string

	// Bounty is what clearing the expected spawn pays: every rat in it,
	// haulers included, at its attribute-481 bounty and the server rate. Read
	// by the hunt ladder (huntladder.go) to rank winnable ground by pay.
	Bounty float64

	// Known is false when the region seeds nothing here or the roster could not be read. Callers treat unknown as
	// no reason to worry, since refusing everywhere the database is quiet would ground the roster on a bad query.
	Known bool
}

// quiet reports a belt with nothing on it: no roster, or a spawner rule that
// allows no ships. Nothing to weigh.
func (t spawnThreat) quiet() bool { return !t.Known || t.DPS <= 0 }

// describe names the threat the way a scout would report it.
func (t spawnThreat) describe() string {
	if t.quiet() {
		return "quiet belts"
	}
	if t.HeaviestName != "" {
		return fmt.Sprintf("~%d rat(s), %.0f DPS / %.0f EHP, heaviest %s",
			t.Ships, t.DPS, t.EHP, t.HeaviestName)
	}
	return fmt.Sprintf("~%d rat(s), %.0f DPS / %.0f EHP", t.Ships, t.DPS, t.EHP)
}

type threatKey struct {
	regionID int64
	band     int
}

var (
	spawnThreatMu    sync.Mutex
	spawnThreatCache = map[threatKey]spawnThreat{}
)

// threatBand buckets a security to the tenth, which is the resolution both
// spawn tables are actually written at (npcRegionSpawns.maxSec, the secMin/
// secMax on a rule). Without it the belt search would miss the cache on
// every system, because raw securities are long decimals that never repeat.
func threatBand(security float64) int {
	if security < 0 {
		security = 0
	}
	return int(math.Floor(security*10 + 0.0001))
}

// spawnThreatFor is what belts in this region at this security put on the field. Cached per region and security
// band for the life of the process: the belt search touches thousands of systems with a few hundred distinct answers.
func (m *Mod) spawnThreatFor(ctx context.Context, regionID int64, security float64) spawnThreat {
	key := threatKey{regionID, threatBand(security)}
	spawnThreatMu.Lock()
	t, ok := spawnThreatCache[key]
	spawnThreatMu.Unlock()
	if ok {
		return t
	}
	t = m.computeSpawnThreat(ctx, regionID, security)
	spawnThreatMu.Lock()
	spawnThreatCache[key] = t
	spawnThreatMu.Unlock()
	return t
}

var (
	spawnFactionMu    sync.Mutex
	spawnFactionCache = map[threatKey][]int64{}
)

// regionSpawnFactions is which factions crew the belts of a region at a security, from the roster's factionID
// column, deduplicated. A bounty hunter employed by a pirate corporation will not fight its own faction (faction.go),
// but combatSite picks systems off spawnThreatFor, which does not know who is flying; without this the hunter sweeps
// a region of colleagues and banks nothing. Cached on the same key as spawnThreatFor.
func (m *Mod) regionSpawnFactions(ctx context.Context, regionID int64, security float64) []int64 {
	key := threatKey{regionID, threatBand(security)}
	spawnFactionMu.Lock()
	ids, ok := spawnFactionCache[key]
	spawnFactionMu.Unlock()
	if ok {
		return ids
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT COALESCE(factionID, 0) FROM npcRegionSpawns
		WHERE regionID = ? AND maxSec >= ?`, regionID, security)
	if err != nil {
		// Unknown is not "friendly": an unreadable roster must not talk a
		// hunter out of a region it could have worked.
		return nil
	}
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	rows.Close()
	spawnFactionMu.Lock()
	spawnFactionCache[key] = out
	spawnFactionMu.Unlock()
	return out
}

// regionHasHuntableRats reports whether a region at this security holds anything the pilot's employer is at odds
// with. A pilot with no employer faction is at odds with everyone and this is always true, including on an empty or
// unreadable roster, so the guard only ever narrows the search for a pirate-corp pilot.
func (m *Mod) regionHasHuntableRats(ctx context.Context, b *botRun, regionID int64, security float64) bool {
	if b == nil || b.FactionID == 0 {
		return true
	}
	mx, ok := m.standingsMatrix(ctx)
	if !ok {
		return true
	}
	ids := m.regionSpawnFactions(ctx, regionID, security)
	if len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if factionHostile(mx, b.FactionID, id) {
			return true
		}
	}
	return false
}

// commanderTailChance is how likely a commander has to be before the pre-travel estimate prices belts for the worst
// commander in the region rather than the worst ordinary rat. Below it a commander is a rare event.
const commanderTailChance = 0.10

// computeSpawnThreat is the pre-travel estimate: what belts in this region at this security are likely to hold. It
// measures the composition npc.RollWave produces (a base pool of normal and hauler rows plus the commander promotion
// at its authored rate, officers excluded) rather than the whole listing. drawSpawn reads the belt's actual rats on
// arrival (npc/beltwave.go).
func (m *Mod) computeSpawnThreat(ctx context.Context, regionID int64, security float64) spawnThreat {
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, weight, role FROM npcRegionSpawns
		WHERE regionID = ? AND maxSec >= ?`, regionID, security)
	if err != nil {
		return spawnThreat{}
	}
	type candidate struct {
		typeID int64
		weight int
		role   string
	}
	var base, cmdr []candidate
	baseWeight, cmdrWeight := 0, 0
	for rows.Next() {
		var c candidate
		if rows.Scan(&c.typeID, &c.weight, &c.role) != nil {
			continue
		}
		if c.weight <= 0 {
			c.weight = 1
		}
		switch c.role {
		case "commander":
			cmdr = append(cmdr, c)
			cmdrWeight += c.weight
		case "officer":
			// Officers are never planned around: a bot cannot plan for a once-a-day regional event.
		default:
			// An empty role counts as normal, matching npc.rolePool, so one blank field in the region editor does not
			// drop a listing out of the pool.
			base = append(base, c)
			baseWeight += c.weight
		}
	}
	rows.Close()
	if len(base) == 0 || baseWeight == 0 {
		// A quiet region seeds nothing here. Known, and harmless.
		return spawnThreat{Known: true}
	}

	minShips, maxShips := m.spawnSizeFor(ctx, security)
	// The middle of the band, rounded up: half the waves are bigger than
	// this and half smaller, which is the honest thing to plan against.
	ships := (minShips + maxShips + 1) / 2
	if ships < 1 {
		ships = 1
	}

	t := spawnThreat{Ships: ships, Known: true}
	// The mean ARMED rat, and the share of the roster that is armed. A hauler
	// listing is part of what spawns but not part of the fight: it adds a
	// body to the wave and nothing to either side of the sum.
	var armedDPS, armedEHP, armedWeight float64
	// The mean bounty over EVERY listing, armed or not: a hauler adds nothing
	// to the fight but its bounty is paid like anyone's.
	var bountySum, bountyWeight float64
	for _, c := range base {
		rt, err := m.ratThreatFor(ctx, c.typeID)
		if err != nil || rt == nil {
			continue
		}
		bountySum += rt.Bounty * float64(c.weight)
		bountyWeight += float64(c.weight)
		if rt.DPS <= 0 {
			continue
		}
		w := float64(c.weight)
		armedDPS += rt.DPS * w
		armedEHP += rt.EHP * w
		armedWeight += w
		if rt.DPS > t.HeaviestDPS {
			t.HeaviestDPS, t.HeaviestEHP, t.HeaviestName = rt.DPS, rt.EHP, rt.Name
		}
	}
	var baseDPS, baseEHP float64
	if armedWeight > 0 {
		baseDPS, baseEHP = armedDPS/armedWeight, armedEHP/armedWeight
		t.ArmedShips = float64(ships) * armedWeight / float64(baseWeight)
		t.DPS = baseDPS * t.ArmedShips
		t.EHP = baseEHP * t.ArmedShips
	}
	var baseBounty float64
	if bountyWeight > 0 {
		baseBounty = bountySum / bountyWeight
		t.Bounty = baseBounty * float64(ships)
	}

	// The commander promotion replaces a slot rather than adding one, so it costs the pilot the difference between
	// a commander and an ordinary rat, weighted by how often one turns up.
	if len(cmdr) > 0 && cmdrWeight > 0 {
		p, slots := m.commanderOdds(ctx, security)
		if p > 0 && slots > 0 {
			var cmdrDPS, cmdrEHP, cmdrBounty, heaviestDPS, heaviestEHP float64
			heaviestName := ""
			for _, c := range cmdr {
				rt, err := m.ratThreatFor(ctx, c.typeID)
				if err != nil || rt == nil {
					continue
				}
				share := float64(c.weight) / float64(cmdrWeight)
				cmdrDPS += rt.DPS * share
				cmdrEHP += rt.EHP * share
				cmdrBounty += rt.Bounty * share
				if rt.DPS > heaviestDPS {
					heaviestDPS, heaviestEHP, heaviestName = rt.DPS, rt.EHP, rt.Name
				}
			}
			if slots > ships {
				slots = ships
			}
			expected := p * float64(slots)
			t.DPS += expected * math.Max(0, cmdrDPS-baseDPS)
			t.EHP += expected * math.Max(0, cmdrEHP-baseEHP)
			t.Bounty += expected * math.Max(0, cmdrBounty-baseBounty)
			// Only let a commander set the tail once it is likely enough to be
			// worth planning for.
			if expected >= commanderTailChance && heaviestDPS > t.HeaviestDPS {
				t.HeaviestDPS, t.HeaviestEHP, t.HeaviestName = heaviestDPS, heaviestEHP, heaviestName
			}
		}
	}
	return t
}

// commanderOdds reads the authored commander chance for a security out of the squad-tier table the spawner rolls
// against, weighted across every tier that covers the band and scaled by the operator's multiplier. It falls back
// to no promotion rather than a guess. Only reached on a cache miss (spawnThreatFor keys per region and band).
func (m *Mod) commanderOdds(ctx context.Context, security float64) (chance float64, slots int) {
	set := evedb.GetBeltSpawnSettings(ctx, m.db)
	if !set.CommandersEnabled || set.CommandersPerWave <= 0 {
		return 0, 0
	}
	var weighted, total float64
	rows, err := m.db.QueryContext(ctx, `
		SELECT weight, cmdrChancePct FROM npcSquadTiers
		WHERE secMin <= ? AND secMax >= ? AND weight > 0`, security, security)
	if err != nil {
		return 0, 0
	}
	for rows.Next() {
		var w, pct float64
		if rows.Scan(&w, &pct) != nil {
			continue
		}
		weighted += w * pct / 100
		total += w
	}
	rows.Close()
	if total <= 0 {
		return 0, 0
	}
	return weighted / total * float64(set.CommanderChancePct) / 100, set.CommandersPerWave
}

// spawnThreatOf reduces a spawn already on the field, the one the abstract tier just drew, to the same shape, so
// the decision to fight it uses the arithmetic that picked the system.
func spawnThreatOf(spawn []*ratThreat) spawnThreat {
	t := spawnThreat{Ships: len(spawn), Known: len(spawn) > 0}
	for _, r := range spawn {
		if r == nil || r.EHP <= 0 {
			continue
		}
		t.Rats = append(t.Rats, ratLine{DPS: r.DPS, EHP: r.EHP})
		if r.DPS <= 0 {
			continue // a free kill once the guns are down: see spawnThreat.EHP
		}
		t.ArmedShips++
		t.DPS += r.DPS
		t.EHP += r.EHP
		if r.DPS > t.HeaviestDPS {
			t.HeaviestDPS, t.HeaviestEHP, t.HeaviestName = r.DPS, r.EHP, r.Name
		}
	}
	return t
}

// ratLine is one rat reduced to the two numbers the sequential sum needs.
type ratLine struct {
	DPS, EHP float64
}

// seqDamageTaken is how much hitpoints a hull with botDPS loses clearing this wave when the rats die one at a time
// (the abstract duel's focus fire, nextRatTarget). Kill order is armed first, highest DPS per hitpoint first: the
// order that minimises damage taken and the order nextRatTarget shoots in. With no rat list the wave is priced as n
// equal rats, whose sum is DPS*EHP/botDPS * (n+1)/(2n); for n = 1 that equals the single-rat arithmetic exactly.
func seqDamageTaken(botDPS float64, th spawnThreat) float64 {
	if botDPS <= 0 {
		return math.Inf(1)
	}
	if len(th.Rats) > 0 {
		armed := make([]ratLine, 0, len(th.Rats))
		incoming := 0.0
		for _, r := range th.Rats {
			if r.DPS > 0 && r.EHP > 0 {
				armed = append(armed, r)
				incoming += r.DPS
			}
		}
		sort.Slice(armed, func(i, j int) bool {
			return armed[i].DPS/armed[i].EHP > armed[j].DPS/armed[j].EHP
		})
		taken := 0.0
		for _, r := range armed {
			taken += incoming * r.EHP / botDPS
			incoming -= r.DPS
		}
		return taken
	}
	n := th.ArmedShips
	if n <= 0 {
		n = float64(th.Ships)
	}
	if n < 1 {
		n = 1
	}
	return th.DPS * th.EHP / botDPS * (n + 1) / (2 * n)
}

// ── the verdict ─────────────────────────────────────────────────────────────

// engageMargin is how many times over the bot outlasts what it is fighting: (botEHP / foeDPS) / (foeEHP / botDPS).
// Above 1 the bot is standing when the belt is clear. Everything else in this file feeds it numbers or spends its answer.
func engageMargin(botDPS, botEHP, foeDPS, foeEHP float64) float64 {
	if foeDPS <= 0 || foeEHP <= 0 {
		return marginOverwhelming // nothing there, or nothing that can hurt
	}
	if botDPS <= 0 || botEHP <= 0 {
		return 0 // a hull with no guns never wins a fight, it only survives one
	}
	timeToKill := foeEHP / botDPS
	timeToDie := botEHP / foeDPS
	margin := timeToDie / timeToKill
	if margin > marginOverwhelming {
		return marginOverwhelming
	}
	return margin
}

// threatMargin is engageMargin against a whole belt's worth of rats, taking the worse of the expected wave and the
// single heaviest hull the roster can produce. The wave reading is sequential (seqDamageTaken): for one rat it is
// exactly engageMargin, for more it stops charging the pilot for rats already dead.
func threatMargin(botDPS, botEHP float64, th spawnThreat) float64 {
	if th.quiet() {
		return marginOverwhelming
	}
	margin := marginOverwhelming
	switch {
	case botDPS <= 0 || botEHP <= 0:
		margin = 0 // a hull with no guns never wins a fight, it only survives one
	default:
		if taken := seqDamageTaken(botDPS, th); taken > 0 {
			margin = math.Min(marginOverwhelming, botEHP/taken)
		}
	}
	if th.HeaviestDPS > 0 {
		if solo := engageMargin(botDPS, botEHP, th.HeaviestDPS, th.HeaviestEHP); solo < margin {
			margin = solo
		}
	}
	return margin
}

// threatMarginRep is threatMargin with repair on the hull taking the fire: rep EHP/s put back while the field dies
// (the duel's packRepairTick). Repair cancels incoming damage and never more, so each rat is charged max(0,
// incoming - rep). Zero rep is threatMargin exactly.
func threatMarginRep(botDPS, botEHP, rep float64, th spawnThreat) float64 {
	if rep <= 0 {
		return threatMargin(botDPS, botEHP, th)
	}
	if th.quiet() {
		return marginOverwhelming
	}
	if botDPS <= 0 || botEHP <= 0 {
		return 0
	}
	margin := marginOverwhelming
	if taken := seqNetDamageTaken(botDPS, rep, th); taken > 0 {
		margin = math.Min(marginOverwhelming, botEHP/taken)
	}
	if th.HeaviestDPS > rep {
		if solo := engageMargin(botDPS, botEHP, th.HeaviestDPS-rep, th.HeaviestEHP); solo < margin {
			margin = solo
		}
	}
	return margin
}

// seqNetDamageTaken is seqDamageTaken with rep EHP/s cancelling incoming fire
// for as long as each rat lives.
func seqNetDamageTaken(botDPS, rep float64, th spawnThreat) float64 {
	if botDPS <= 0 {
		return math.Inf(1)
	}
	if len(th.Rats) > 0 {
		armed := make([]ratLine, 0, len(th.Rats))
		incoming := 0.0
		for _, r := range th.Rats {
			if r.DPS > 0 && r.EHP > 0 {
				armed = append(armed, r)
				incoming += r.DPS
			}
		}
		sort.Slice(armed, func(i, j int) bool {
			return armed[i].DPS/armed[i].EHP > armed[j].DPS/armed[j].EHP
		})
		taken := 0.0
		for _, r := range armed {
			taken += math.Max(0, incoming-rep) * r.EHP / botDPS
			incoming -= r.DPS
		}
		return taken
	}
	// No roster: the averaged wave, with the rep taken off over the time the
	// wave takes to die.
	gross := seqDamageTaken(botDPS, th)
	return math.Max(0, gross-rep*th.EHP/botDPS)
}

// minerMargin is the mining path's reading of the same spawn. threatMargin would refuse every rat belt for a mining
// frigate, and a miner only has to leave, so the clock decides: how many escape windows the hull survives at the
// spawn's incoming DPS. 1.0 is just getting out. A hull that can kill the spawn short-circuits to overwhelming.
func minerMargin(botDPS, botEHP float64, th spawnThreat) float64 {
	if th.quiet() {
		return marginOverwhelming
	}
	if threatMargin(botDPS, botEHP, th) >= 1 {
		return marginOverwhelming
	}
	if th.DPS <= 0 {
		return marginOverwhelming
	}
	if botEHP <= 0 {
		return 0
	}
	margin := (botEHP / th.DPS) / minerEscapeSec
	if margin > marginOverwhelming {
		return marginOverwhelming
	}
	return margin
}

// minerHopeless is hopeless() for a miner: the spawn kills the hull inside the escape window. Not marginRefuse, which
// is a fraction of a fight; this margin counts escape windows.
func minerHopeless(margin float64) bool { return margin < 1 }

// minerVerdict names a miner's margin the way the pilot would say it, since
// marginVerdict's words ("outmatched", "a fight worth having") describe a
// duel nobody in this path intends to have.
func minerVerdict(margin float64) string {
	switch {
	case margin >= marginOverwhelming:
		return "nothing there it cannot handle"
	case minerHopeless(margin):
		return fmt.Sprintf("dead before it could align (%.0fs under fire)", margin*minerEscapeSec)
	default:
		return fmt.Sprintf("survivable but hot (%.0fs under fire)", margin*minerEscapeSec)
	}
}

// wouldEngage is the pilot's own answer: does this margin clear the line its
// nerve draws? Kept separate from the margin itself so the two tiers and the
// two site searches cannot disagree about where the line is.
func wouldEngage(tn Tuning, margin float64) bool {
	return margin >= tn.EngageMargin
}

// hopeless reports a fight the arithmetic says is unsurvivable regardless of
// nerve. Recklessness lowers where a pilot commits; it does not repeal
// division.
func hopeless(margin float64) bool { return margin < marginRefuse }

// marginVerdict names a margin the way the pilot would say it.
func marginVerdict(tn Tuning, margin float64) string {
	switch {
	case margin >= marginOverwhelming:
		return "nothing there to fight"
	case margin >= tn.EngageMargin*2:
		return fmt.Sprintf("comfortably ahead (%.1f×)", margin)
	case wouldEngage(tn, margin):
		return fmt.Sprintf("a fight worth having (%.1f×)", margin)
	default:
		return fmt.Sprintf("outmatched (%.1f×)", margin)
	}
}

// threatJumpCost prices a belt's danger in jumps, the currency of scoring.go. Zero once the margin clears the
// pilot's line. It is scaled by how much this pilot's nerve prices a ratted belt at, so a careful pilot pays more to
// avoid a fight it would lose.
func threatJumpCost(tn Tuning, margin float64) float64 {
	line := tn.EngageMargin
	if line <= 0 {
		line = defaultEngageMargin
	}
	if margin >= line {
		return 0
	}
	short := 1 - margin/line // 0 at the line, 1 at a standing start
	if short > 1 {
		short = 1
	}
	nerve := 1.0
	if beltHostileCost > 0 && tn.HostileCost > 0 {
		nerve = tn.HostileCost / beltHostileCost
	}
	return threatOverwhelmedCost * short * nerve
}

// ── the bot's side of the sum ───────────────────────────────────────────────

// botPower is what this pilot brings to a fight, for searches that decide before there is anything to fight. The EHP
// is the hull at full: site selection asks about the ship, not its damage bar (the pre-fight decision uses current
// EHP). known is false when the bot has no hull to read; callers skip the assessment rather than treat that as defenceless.
func (m *Mod) botPower(ctx context.Context, b *botRun) (dps, ehp float64, known bool) {
	if b == nil || b.ShipID == 0 {
		return 0, 0, false
	}
	c, err := m.botCombatant(ctx, b)
	if err != nil || c == nil || c.MaxEHP <= 0 {
		return 0, 0, false
	}
	return c.DPS, c.MaxEHP, true
}

// effectivePower (below) pools this pilot's guns with every driven fighting gangmate on the op; haulers do not count
// and logi reps are read hull by hull (sitefocus.go focusRep). powerOverride replaces that reading when set; only the
// harness sets it, to hand a pilot a known weight class. nil in production.
var powerOverride func(b *botRun) (dps, ehp float64, known bool)

func (m *Mod) effectivePower(ctx context.Context, b *botRun) (dps, ehp float64, known bool) {
	if powerOverride != nil {
		return powerOverride(b)
	}
	dps, ehp, known = m.botPower(ctx, b)
	if b == nil || b.State == nil || b.State.Data.GangID == 0 {
		return dps, ehp, known
	}
	mates, err := m.gangCombatRoster(ctx, b.State.Data.GangID, b.CharID)
	if err != nil {
		return dps, ehp, known
	}
	for _, mate := range mates {
		mDPS, mEHP, ok := m.botPower(ctx, mate)
		if !ok {
			continue
		}
		dps += mDPS
		ehp += mEHP
		known = true
	}
	return dps, ehp, known
}

// gangCombatRoster is the gang's driven members whose job is to fight, wherever they currently are: the gun
// seats, and the escort a mining op borrows (gangescort.go), which is the op's only gun. Unlike
// gangmatesOnStation it does not require them on the belt already: it answers what this op can field before anybody
// has arrived. Only the fields botCombatant reads are filled.
func (m *Mod) gangCombatRoster(ctx context.Context, gangID, exceptID int64) ([]*botRun, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT gm.characterID, c.characterName, c.solarSystemID,
		       COALESCE(c.stationID, 0), COALESCE(ce.locationID, 0)
		FROM rolebot_gang_members gm
		JOIN rolebot_characters rb ON rb.characterID = gm.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = gm.characterID
		LEFT JOIN entity ce ON ce.itemID = gm.characterID
		WHERE gm.gangID = ? AND gm.characterID <> ? AND gm.role IN (?, ?, ?, ?, ?, ?)`,
		gangID, exceptID, RoleCombat, RoleHunter, RoleTackler, RoleLogi, RoleEwar, RoleEscort)
	if err != nil {
		return nil, fmt.Errorf("gangCombatRoster: %w", err)
	}
	defer rows.Close()

	var out []*botRun
	for rows.Next() {
		var charID, systemID, stationID, charLoc int64
		var name string
		if err := rows.Scan(&charID, &name, &systemID, &stationID, &charLoc); err != nil {
			return nil, err
		}
		// The character entity sits inside its boarded ship; a location that
		// is the station means it is in a hangar with no hull to bring.
		if charLoc == 0 || charLoc == stationID {
			continue
		}
		out = append(out, &botRun{CharID: charID, Name: name, ShipID: charLoc, SystemID: systemID})
	}
	return out, rows.Err()
}

// ── the embodied tier's lookup ──────────────────────────────────────────────

// ratThreatCached is ratThreatFor without the database: the combat loop runs on a tick and cannot block on queries.
// warmRatThreats fills the cache at first contact. A miss means no opinion, not harmless: pickCombatTarget falls back
// to distance ordering for anything not cached.
func ratThreatCached(typeID int64) (*ratThreat, bool) {
	ratThreatMu.Lock()
	t, ok := ratThreatCache[typeID]
	ratThreatMu.Unlock()
	return t, ok && t != nil
}
// warmRatThreats resolves the types on this grid into the cache, so the combat loop's per-tick target choice is a
// map lookup. Cached process-wide, since the data is static: a belt full of the same rat costs this once per server run.
// per server run, not once per fight.
func (m *Mod) warmRatThreats(ctx context.Context, typeIDs []int64) {
	for _, id := range typeIDs {
		if id == 0 {
			continue
		}
		if _, ok := ratThreatCached(id); ok {
			continue
		}
		if _, err := m.ratThreatFor(ctx, id); err != nil {
			// One unreadable type must not cost the bot the rest of the
			// grid's numbers; pickCombatTarget copes with a miss.
			continue
		}
	}
}
