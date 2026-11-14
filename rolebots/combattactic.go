package rolebots

import (
	"context"
	"math"
	"math/rand"
)

// combatTactic is one entry of the combat trip table, priced by what the world offers now (as
// activityCatalog, appetite.go), so a new trip is one entry, not a planner edit. Doctrines
// (corpdoctrine.go) gate which employers may fly it; tactics have no satiation half-life.
type combatTactic struct {
	Slug  string
	Label string
	Task  TaskKind
	// Doctrines that may fly this trip. Empty means every doctrine with a wing.
	Doctrines []Doctrine
	// StintSec is how long one trip lasts. 0 means the ordinary sweep clock (huntSweepSec). A
	// gate hold needs its own number, as piracyCampSec does: a camp that packs up every fifteen
	// minutes is not a checkpoint.
	StintSec int64
	// Raid marks a trip that goes into somebody else's space. It is gated on Doctrine.Raids(), so
	// the navy-versus-militia line is stated once, in corpdoctrine.go.
	Raid bool
	// Focus says who the trip is about: the rats (focusPvE) or other pilots (focusPvP). It is not
	// a gate; pickCombatTactic scales the weight by the pilot's lean.
	Focus string
	// Weight is what this trip is worth to this pilot now. 0 means unavailable.
	Weight func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64
	// Site answers where the trip goes. The extra int64 is what the tactic remembers about the
	// destination (a gate, a pilot), or 0.
	Site func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error)
}

const (
	focusPvE = "pve"
	focusPvP = "pvp"

	tacticManhunt     = "manhunt"
	tacticRobbery     = "robbery"
	tacticBeltSweep   = "beltsweep"
	tacticCoverMiners = "cover"
	tacticGateHold    = "gate"
	tacticRoam        = "roam"
	tacticRunDown     = "rundown"
	// Running an open site the core seeded (siterun.go).
	tacticSite = "site"
)

// tacticRecentMax is how many trips back a pilot remembers. It is short: a long ring would
// exclude every available tactic and fall through to the default.
const tacticRecentMax = 2

// tacticRepeatPenalty is what having just flown a trip costs it. It is a penalty, not an
// exclusion, so a pilot with one allowed tactic can still fly it twice running.
const tacticRepeatPenalty = 0.35

// combatTactics is the catalogue. Order is presentation only; the pick is a weighted draw.
var combatTactics = []combatTactic{
	{
		// The classic trip and the only one always available. Its weight is the floor every other
		// tactic is measured against.
		Slug: tacticBeltSweep, Label: "belt sweep", Task: TaskCombatSweep, Focus: focusPvE,
		Weight: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64 {
			return 1.0
		},
		Site: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
			sysID, name, err := m.combatSite(ctx, b, cfg)
			return sysID, 0, name, err
		},
	},
	{
		// Running an open site the core seeded (siterun.go). Weighed only when a site this pilot can
		// clear alone is in reach; a harder site within a hunt pack's power is posted to the board
		// (siteTacticWeight). Every doctrine flies it.
		Slug: tacticSite, Label: "running a site", Task: TaskCombatSite, Focus: focusPvE,
		StintSec: siteRunMaxSec,
		Weight: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64 {
			return m.siteTacticWeight(ctx, b, cfg)
		},
		Site: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
			return m.pickSite(ctx, b, cfg)
		},
	},
	{
		// Answering a distress call (beltdistress.go). The pilot goes straight to the distressed
		// system that combatSite already ranks first. Every doctrine flies it, defence included.
		Slug: tacticCoverMiners, Label: "belt relief", Task: TaskCombatSweep, Focus: focusPvE,
		Weight: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64 {
			if len(m.distressedBeltSystems(ctx, b.CorpID)) == 0 {
				return 0
			}
			// Worth well more than a speculative sweep: somebody is being refused a belt now.
			return 2.2
		},
		Site: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
			sysID, name := m.pickReliefSystem(ctx, b.CorpID, m.homeRegionSet(ctx, b))
			if sysID == 0 {
				// The board emptied or everything on it is out of this pilot's regions. Not an error: the
				// caller falls through to the next tactic.
				return 0, 0, "", nil
			}
			return sysID, 0, name, nil
		},
	},
	{
		// Sitting on a gate. A security corporation calls it a checkpoint and a mercenary outfit a
		// blockade; the task, posture and rules of engagement are the same. Patience owns the odds,
		// through the trait that owns a pirate's GateCampChance.
		Slug: tacticGateHold, Label: "gate hold", Task: TaskCombatGate, Focus: focusPvP,
		Doctrines: []Doctrine{DoctrineSecurity, DoctrineMercenary, DoctrinePredation},
		StintSec:  piracyCampSec,
		Weight: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64 {
			w := 2.0 * b.Tune.GateCampChance
			// A war floors the patience trait. The doctrine list stays unchanged: war lifts only the raid
			// gate (allowsDoctrine), so a defence wing answers a war by roaming.
			if relCorpAtWar(b.CorpID) && w < 2.0 {
				w = 2.0
			}
			// When the census finds lawful trouble in reach, the hold is worth at least its doctrine's
			// floor whatever the pilot's patience (legalcensus.go).
			if lethalityOn() {
				if _, score := m.legalSiteFor(ctx, b, cfg.SecMin); score > 0 {
					if floor := gateFloor(m.corpDoctrineFor(ctx, b.CorpID)); w < floor {
						w = floor
					}
				}
			}
			return w
		},
		Site: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
			sysID, name, err := m.gateHoldGround(ctx, b, cfg)
			if err != nil || sysID == 0 {
				return 0, 0, "", err
			}
			// Random among the system's gates, as piracySite does, so the roster does not pile onto one
			// gate.
			var gateID int64
			if err := m.db.QueryRowContext(ctx, `
				SELECT itemID FROM mapDenormalize
				WHERE solarSystemID = ? AND groupID = 10
				ORDER BY RAND() LIMIT 1`, sysID).Scan(&gateID); err != nil {
				// No gates resolved. A gate hold with no gate is a belt sweep, so return no site.
				return 0, 0, "", nil
			}
			return sysID, gateID, name, nil
		},
	},
	{
		// A combat pilot touring space where rival bots are working, to fight them rather than the
		// rats. It is refused inside policed space, where the rules of engagement answer no (roe.go).
		Slug: tacticRoam, Label: "roam", Task: TaskCombatRoam, Focus: focusPvP,
		Raid: true,
		Weight: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64 {
			// Policed space refuses a roam because the rules of engagement refuse it there. A war is the
			// one thing that makes highsec shootable (roeAllows), so it makes a highsec roam real.
			war := relCorpAtWar(b.CorpID)
			if cfg.SecMin >= policedSecurity && !war {
				return 0
			}
			if war {
				// A corporation at war goes looking whatever its pilots' taste for fights. Floored, not
				// multiplied, so a placid pilot in a corporation at war still flies it.
				w := 3.0 * b.Tune.NeutralAggression
				if w < 3.0 {
					w = 3.0
				}
				return w
			}
			// Appetite for a stranger prices the trip. A pilot with none flies a sweep instead.
			return 3.0 * b.Tune.NeutralAggression
		},
		Site: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
			return m.roamSite(ctx, b, cfg)
		},
	},
	{
		// Robbery. The weight is robberyWeight: lawlessness times violence (zero for every lawful
		// pilot), the capability probe, the doubled satiation half-life and the CEO directive. The trip
		// delegates to piracyPlan (combatPlan), which keeps its own site pick, so Site is never asked.
		Slug: tacticRobbery, Label: "robbery", Task: TaskPiracyProwl, Focus: focusPvP,
		Weight: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64 {
			if robberyWeightFn == nil {
				return 0
			}
			return robberyWeightFn(m, ctx, b)
		},
		Site: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
			return 0, 0, "", nil
		},
	},
	{
		// The police's roam. The census (legalcensus.go) sends it after outlaws, the flagged, war
		// enemies and priced marks in reach. It has no Raid flag, because pursuing pilots the law
		// names is not an invasion. Roam tours rivals by appetite; this goes where a lawful target is.
		Slug: tacticManhunt, Label: "manhunt", Task: TaskCombatRoam, Focus: focusPvP,
		Doctrines: []Doctrine{DoctrineSecurity, DoctrineMercenary, DoctrinePredation},
		Weight: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64 {
			if !BotPvP() || !lethalityOn() {
				return 0
			}
			_, score := m.legalSiteFor(ctx, b, cfg.SecMin)
			if score <= 0 {
				return 0
			}
			return 3.0 * math.Min(1, score/6)
		},
		Site: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
			sysID, score := m.legalSiteFor(ctx, b, cfg.SecMin)
			if sysID == 0 || score <= 0 {
				return 0, 0, "", nil
			}
			return sysID, 0, m.nameOfSystem(ctx, sysID), nil
		},
	},
	{
		// Running somebody down: a mark with a price, working somewhere reachable. It is weighed on
		// the board holding one and needs a point fitted, because bountyHuntPass refuses a hunt
		// without tackle (combat_botvbot.go).
		Slug: tacticRunDown, Label: "run down a mark", Task: TaskCombatIntercept, Focus: focusPvP,
		Doctrines: []Doctrine{DoctrineSecurity, DoctrineMercenary, DoctrinePredation},
		Weight: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) float64 {
			if !BotPvP() {
				return 0
			}
			if mark, _ := m.markForPilot(ctx, b); mark == 0 {
				return 0
			}
			return 2.5
		},
		Site: func(m *Mod, ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
			mark, sysID := m.markForPilot(ctx, b)
			if mark == 0 || sysID == 0 {
				return 0, 0, "", nil
			}
			return sysID, mark, m.nameOfSystem(ctx, sysID), nil
		},
	},
}

func combatTacticBySlug(slug string) (combatTactic, bool) {
	for _, t := range combatTactics {
		if t.Slug == slug {
			return t, true
		}
	}
	return combatTactic{}, false
}

// allowsDoctrine reports whether this employer may fly this trip. atWar lifts the raid gate
// and nothing else: a corporation that has been declared on has been given a fight.
// DoctrineNone is still refused, and the doctrine list is still honoured.
func (t combatTactic) allowsDoctrine(d Doctrine, atWar bool) bool {
	if d == DoctrineNone {
		return false
	}
	if t.Raid && !d.Raids() && !atWar {
		return false
	}
	if len(t.Doctrines) == 0 {
		return true
	}
	for _, ok := range t.Doctrines {
		if ok == d {
			return true
		}
	}
	return false
}

// pickCombatTactic draws one trip for this stint. The draw is weighted, as in pickActivity, so
// pilots in the same situation vary. Recency is a penalty inside the draw, not a filter. It
// always answers, since the belt sweep's weight is a constant.
func (m *Mod) pickCombatTactic(ctx context.Context, b *botRun, cfg combatConfig, doc Doctrine) combatTactic {
	// Asked once for the whole draw: it is a property of the employer, not of the trip.
	atWar := relCorpAtWar(b.CorpID)
	lean := -1.0 // unread: focusMul leaves every weight alone
	if lethalityOn() {
		lean = pilotPvPLean(b.Tune.ViolenceAppetite, doc, atWar)
	}
	recent := map[string]bool{}
	for _, s := range b.State.Data.TacticRecent {
		recent[s] = true
	}

	type scored struct {
		t combatTactic
		w float64
	}
	var pool []scored
	total := 0.0
	for _, t := range combatTactics {
		if !t.allowsDoctrine(doc, atWar) {
			continue
		}
		w := t.Weight(m, ctx, b, cfg)
		if w <= 0 {
			continue
		}
		w *= focusMul(t.Focus, lean)
		if recent[t.Slug] {
			w *= tacticRepeatPenalty
		}
		pool = append(pool, scored{t: t, w: w})
		total += w
	}
	if total <= 0 || len(pool) == 0 {
		// No employer, or a doctrine with no trips: the sweep is the default.
		t, _ := combatTacticBySlug(tacticBeltSweep)
		return t
	}
	roll := rand.Float64() * total
	for _, s := range pool {
		roll -= s.w
		if roll <= 0 {
			return s.t
		}
	}
	return pool[len(pool)-1].t
}

// pushTacticRecent records a flown trip, newest first.
func pushTacticRecent(list []string, slug string) []string {
	out := make([]string, 0, tacticRecentMax)
	out = append(out, slug)
	for _, s := range list {
		if s == slug {
			continue
		}
		if len(out) == tacticRecentMax {
			break
		}
		out = append(out, s)
	}
	return out
}

// ── the two sites that are not combatSite ───────────────────────────────────

// roamSite picks somewhere worth roaming: unpoliced space with rival bots working in it, or
// for a corporation at war, wherever its war enemies are out. It scores as piracySite does but
// filters the census to systems where this pilot's rules of engagement allow a shot.
func (m *Mod) roamSite(ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
	u, err := m.universe(ctx)
	if err != nil {
		return 0, 0, "", err
	}
	exposed := m.exposedBotsBySystem(ctx)
	if len(exposed) == 0 {
		return 0, 0, "", nil
	}
	rivals, warRivals := m.rivalSystems(ctx, b, exposed)
	if len(rivals) == 0 {
		return 0, 0, "", nil
	}

	// A bounded walk out from the pilot, the same shape and bound as piracySite.
	type cand struct {
		sysID int64
		score float64
	}
	// A war opens policed space (roeAllows), so a system with war enemies out is roamable. In
	// policed space only they count.
	warOpen := len(warRivals) > 0
	var best cand
	bestWar := false
	seen := map[int64]bool{b.SystemID: true}
	frontier := []int64{b.SystemID}
	for hop := 0; hop <= piracySearchHops && len(frontier) > 0; hop++ {
		for _, sysID := range frontier {
			ms, ok := u.meta[sysID]
			if !ok {
				continue
			}
			sec := dispSec(ms.Security)
			warN := warRivals[sysID]
			if sec < cfg.SecMin {
				continue
			}
			n := rivals[sysID]
			if sec >= policedSecurity {
				// Never roam into policed space without a war there.
				n = warN
			}
			if n == 0 {
				continue
			}
			if n > piracyPreyCap {
				n = piracyPreyCap
			}
			away := hop
			if away > piracyAwayCap {
				away = piracyAwayCap
			}
			// The pirate's hunting-ground score (piracy.go), including the sign on distance: it is a
			// reward, so a roam leaves home instead of grinding against neighbours' front doors.
			score := float64(n)*piracyPreyWorth +
				float64(away)*piracyAwayWorth +
				(1-sec)*piracyRoughWorth
			// A declared enemy working here is the fight the roam is for.
			score += relMarkFor(b.CorpID, sysID)
			// A war enemy out here counts again on top of being prey.
			if warN > piracyPreyCap {
				warN = piracyPreyCap
			}
			score += float64(warN) * piracyPreyWorth * warMarkMul()
			if best.sysID == 0 || score > best.score {
				best = cand{sysID: sysID, score: score}
				bestWar = warN > 0
			}
		}
		var next []int64
		for _, sysID := range frontier {
			for _, nb := range u.adj[sysID] {
				if seen[nb] {
					continue
				}
				seen[nb] = true
				next = append(next, nb)
			}
		}
		frontier = next
	}
	// When the bounded walk found no war system, the pilot goes on campaign: the nearest system
	// where war enemies are out now (warExpeditionSite).
	if warOpen && !bestWar {
		if sysID := warExpeditionSite(u, b.SystemID, cfg.SecMin, warRivals); sysID != 0 {
			return sysID, 0, m.nameOfSystem(ctx, sysID), nil
		}
	}
	if best.sysID == 0 {
		return 0, 0, "", nil
	}
	return best.sysID, 0, m.nameOfSystem(ctx, best.sysID), nil
}

// warExpeditionMaxJumps bounds a war campaign's search. A war is declared on an owner, not a
// neighbourhood, so it is wide; the bound only stops a broken graph walking forever.
const warExpeditionMaxJumps = 60

// warExpeditionPilotJumps is what one more war enemy out in a system is worth in jumps. A
// pack a few jumps further beats a lone hauler, which has usually moved on.
const warExpeditionPilotJumps = 5

// warExpeditionSite is where a corporation at war campaigns: the system within
// warExpeditionMaxJumps of from, at or above secMin, whose war enemies (warRivals) best repay
// the trip, each worth warExpeditionPilotJumps capped like prey, less the jumps. 0 if none. Pure.
func warExpeditionSite(u *universe, from int64, secMin float64, warRivals map[int64]int) int64 {
	var pick int64
	bestScore := math.Inf(-1)
	seen := map[int64]bool{from: true}
	frontier := []int64{from}
	for hop := 0; hop <= warExpeditionMaxJumps && len(frontier) > 0; hop++ {
		for _, sysID := range frontier {
			n := warRivals[sysID]
			if n == 0 {
				continue
			}
			if ms, ok := u.meta[sysID]; !ok || dispSec(ms.Security) < secMin {
				continue
			}
			if n > piracyPreyCap {
				n = piracyPreyCap
			}
			score := float64(n*warExpeditionPilotJumps - hop)
			if score > bestScore || (score == bestScore && sysID < pick) {
				pick, bestScore = sysID, score
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
	return pick
}

// rivalSystems narrows an exposed-bot census to the systems where the pilots working are
// ones this pilot may shoot. It asks the stance question only and does not roll the nerve; that
// belongs to the encounter. war is the subset that are war enemies, counted per pilot.
func (m *Mod) rivalSystems(ctx context.Context, b *botRun, exposed map[int64]int) (out, war map[int64]int) {
	doc := m.corpDoctrineFor(ctx, b.CorpID)
	takesNeutrals := doc.NeutralMul() > 0

	rows, err := m.db.QueryContext(ctx, `
		SELECT s.characterID, c.solarSystemID, c.corporationID
		FROM rolebot_state s
		JOIN rolebot_characters rb ON rb.characterID = s.characterID AND rb.enabled = 1
		JOIN character_ c ON c.characterID = s.characterID
		WHERE COALESCE(c.stationID, 0) = 0 AND c.characterID <> ?`, b.CharID)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()

	self := b.standingSelf()
	warOpen := warEverywhere() && relCorpAtWar(b.CorpID)
	out, war = map[int64]int{}, map[int64]int{}
	for rows.Next() {
		var charID, sysID, corpID int64
		if rows.Scan(&charID, &sysID, &corpID) != nil {
			continue
		}
		if exposed[sysID] == 0 {
			continue
		}
		st := stanceOf(m.standingToPilotForEngage(ctx, self, charID, doc), m.kinToPilot(ctx, self, charID))
		switch st {
		case StanceHostile:
			out[sysID]++
			if warOpen && relAtWar(b.CorpID, 0, corpID, 0) {
				war[sysID]++
			}
		case StanceNeutral:
			if takesNeutrals {
				out[sysID]++
			}
		}
	}
	return out, war
}

// markForPilot is the bounty board's answer for one solo pilot: a bountied rolebot this one
// could reach and beat. combat_botvbot.go owns the rules; this asks at plan time.
func (m *Mod) markForPilot(ctx context.Context, b *botRun) (int64, int64) {
	pc := m.pvpCombatantFor(ctx, b.CharID, RoleHunter)
	if pc == nil {
		return 0, 0
	}
	side := []*pvpCombatant{pc}
	// No point on the field, no hunt. The same refusal huntOnce makes, so an unfitted pilot never
	// flies the trip.
	if !sideHasTackle(side) {
		return 0, 0
	}
	markID, _, _ := m.findBountiedTarget(ctx, b, side, nil, 0)
	if markID == 0 {
		return 0, 0
	}
	var sysID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT solarSystemID FROM character_ WHERE characterID = ? AND COALESCE(stationID, 0) = 0`,
		markID).Scan(&sysID); err != nil {
		return 0, 0
	}
	return markID, sysID
}

// tacticStint is how long the stint this pilot is on lasts.
func tacticStint(slug string) int64 {
	if t, ok := combatTacticBySlug(slug); ok && t.StintSec > 0 {
		return t.StintSec
	}
	return huntSweepSec
}

// focusMul scales a trip's weight by how much of this pilot's flying is about other pilots: a
// PvP trip is worth 0.4x to a pilot that leans nowhere and 2x at full lean, a PvE trip the
// reverse but never below 0.6. lean < 0 means unread and leaves the weight alone.
func focusMul(focus string, lean float64) float64 {
	if lean < 0 {
		return 1
	}
	switch focus {
	case focusPvP:
		return 0.4 + 1.6*lean
	case focusPvE:
		return math.Max(0.6, math.Min(1.2, 1.2-lean))
	}
	return 1
}

// gateFloor is the least a gate hold is worth to an employer's pilot when the census says
// there is lawful trouble in reach.
func gateFloor(d Doctrine) float64 {
	switch d {
	case DoctrineSecurity:
		return 1.2
	case DoctrineMercenary, DoctrinePredation:
		return 1.5
	}
	return 0
}

// gateHoldGround is where a gate hold sits: the census's best system if there is one, else
// the belt site combatSite picked. Falling back is not a failure.
func (m *Mod) gateHoldGround(ctx context.Context, b *botRun, cfg combatConfig) (int64, string, error) {
	if lethalityOn() {
		if sysID, score := m.legalSiteFor(ctx, b, cfg.SecMin); sysID != 0 && score > 0 {
			return sysID, m.nameOfSystem(ctx, sysID), nil
		}
	}
	return m.combatSite(ctx, b, cfg)
}

// robberyWeightFn is robberyWeight, bound in init because a package-level initialiser cannot
// close the loop catalogue, combatPlan, this table. Nil weighs 0.
var robberyWeightFn func(m *Mod, ctx context.Context, b *botRun) float64

func init() { robberyWeightFn = (*Mod).robberyWeight }

// robberyWeight prices the robbery trip with the numbers the piracy activity was chosen by:
// disposition x opportunity x satiation x the corporation's pull. 0 when the pilot has no
// taste for it, cannot fly it, or its wing is full.
func (m *Mod) robberyWeight(ctx context.Context, b *botRun) float64 {
	// Read off the pieces, not through activityDefFor: the catalogue refers to combatPlan, which
	// refers to this table, and Go will not initialise the cycle.
	drive := b.Tune.PiracyAppetite
	if drive <= 0 {
		return 0
	}
	opp, _ := piracyOpportunity(m, ctx, b)
	if opp <= 0 {
		return 0
	}
	sat := satiationFactor(b.State.Data.Worked[ActPiracy], piracyHalfLife(b.Tune))
	return drive * opp * sat * m.directiveTerm(ctx, b, ActPiracy) * m.armTerm(b, ActPiracy)
}
