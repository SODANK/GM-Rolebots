package rolebots

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Moving in with the employer: after a pilot changes employer it looks once at where it lives and
// moves its clone (medical station) and, for an industrialist, its factory base to the employer's
// stations. The pull is mild for empire, stronger for fringe and player corporations, strongest for
// outlaw, and loyalty scales it. The clone and the factory are picked separately.
// See docs/comment-notes-b05.md#corp-home.

const (
	// corpHomeWindowSec is the window the first look after a join is spread across, by character ID,
	// so pilots that joined together do not all move in one pass.
	corpHomeWindowSec = 20 * 60

	// corpHomeRetrySec is how long after a look that could not move the pilot looks again.
	corpHomeRetrySec = 45 * 60

	// corpHomeHoldSec is how long a pilot already flying to its new home waits before looking again.
	corpHomeHoldSec = 120

	// Search range in raw jumps, by the bloc the pilot signed with. A floor: see corpHomeRangeCeiling.
	corpHomeRangeEmpire = 15
	corpHomeRangeFringe = 20
	corpHomeRangeOutlaw = 45

	// corpHomeRangePlayer is the range for a player corporation. It has no faction, so Bloc reads it as
	// fringe, but a person accepted the pilot, so the pilot commutes further to it.
	corpHomeRangePlayer = 30

	// corpHomeRangeCeiling is the furthest an address search runs, in raw jumps. It sits past the map
	// diameter so a distant employer loses on score (corpHomeJumpCost) instead of dropping out.
	corpHomeRangeCeiling = 100

	// corpHomeJumpShare is the cost of one jump of commute as a share of the move-out rate
	// (moveOutJumpCost), so Wanderlust scales it. At half the rate a mid pilot weighs nearby stations
	// as equals and the crowd term decides among them.
	corpHomeJumpShare = 0.5

	// corpHomeMargin is the points a winner must beat the current home by, so a move never rests on rounding.
	corpHomeMargin = 3.0

	// The pull: points a station gets for belonging to the pilot's own employer, by bloc.
	corpHomePullEmpire = 20.0
	corpHomePullFringe = 28.0
	corpHomePullOutlaw = 70.0
	// corpHomePullPlayer sits between fringe and outlaw: see corpHomeRangePlayer.
	corpHomePullPlayer = 45.0

	// Loyalty scales the pull and the range: a devoted pilot moves in, a mercenary commutes.
	corpHomePullDevoted    = 1.5
	corpHomePullMercenary  = 0.6
	corpHomeRangeDevotedMx = 1.0
	corpHomeRangeMercMx    = 0.7

	// When the employer has no station for one half of the address, the pilot settles within this many
	// jumps of the employer's space at this share of the pull. Outlaws search further.
	corpHomeHQRadius       = 6
	corpHomeHQRadiusOutlaw = 12
	corpHomeHQPullFrac     = 0.5

	// corpHomeBothBonus is what a station that is both factory and medical bay adds for an industrialist.
	corpHomeBothBonus = 8.0

	// corpHomeTries is how many candidates get the per-station checks (assembly lines, standing).
	corpHomeTries = 6

	// corpHomeTargetRadius is how far corpHomeTargets looks for the half of an address the operator's
	// chosen station cannot provide. It is wide because it carries out an instruction.
	corpHomeTargetRadius = 25

	// factoryNearSystemCap caps the systems one factory sweep names in a query, since each is a bind
	// parameter. See factoryStationsNear.
	factoryNearSystemCap = 200

	// ── drift ───────────────────────────────────────────────────────────────
	// A settled pilot re-measures now and then how far its home is from its employer's nearest station.
	// If that is too far it un-settles and looks again on the terms it joined on.

	// corpHomeDriftJumps is how far home may sit from the nearest employer station before it stops
	// counting as living near them. It is under the empire search range.
	corpHomeDriftJumps = 12

	// corpHomeDriftEverySec is how often a settled pilot re-measures, spread by character ID.
	corpHomeDriftEverySec = 4 * 3600

	// corpHomeRushSec is how long a roster-wide reconsider skips the spread window.
	corpHomeRushSec = 30 * 60
)

var (
	corpHomeMu sync.Mutex
	// When this pilot next looks.
	corpHomeNextAt = map[int64]int64{}
	// The corporation this pilot has settled its address against. Memory only: a restart re-checks
	// once per pilot and cannot move anyone twice.
	corpHomeSettledFor = map[int64]int64{}
	// When a settled pilot next re-measures the distance from its home to
	// its employer's nearest station.
	corpHomeDriftAt = map[int64]int64{}
	// While in the future, a pilot seen for the first time looks at once. Only reconsiderAllCorpHomes sets it.
	corpHomeRushUntil int64
	// The system a pilot stood in when a look decided not to move it. The search depends only on that
	// system, so a drift check from the same one would reach the same answer.
	corpHomeLookedFrom = map[int64]int64{}
)

// forgetCorpHome drops a pilot's bookkeeping so the next planning pass looks
// again from scratch. Called when it changes employer (joinCorp) and when an
// operator edits its profile by hand.
func forgetCorpHome(charID int64) {
	corpHomeMu.Lock()
	delete(corpHomeNextAt, charID)
	delete(corpHomeSettledFor, charID)
	delete(corpHomeDriftAt, charID)
	delete(corpHomeLookedFrom, charID)
	corpHomeMu.Unlock()
}

// reconsiderAllCorpHomes drops every pilot's address bookkeeping, so the next
// reconsiderAllCorpHomes drops every pilot's address bookkeeping so the next planning pass looks at
// all of them again, without a restart. Looks are scheduled at once, one per pilot per planning
// pass. It returns the number of settled pilots dropped.
func reconsiderAllCorpHomes() int {
	corpHomeMu.Lock()
	n := len(corpHomeSettledFor)
	corpHomeNextAt = map[int64]int64{}
	corpHomeSettledFor = map[int64]int64{}
	corpHomeDriftAt = map[int64]int64{}
	corpHomeLookedFrom = map[int64]int64{}
	corpHomeRushUntil = clock.Unix() + corpHomeRushSec
	corpHomeMu.Unlock()
	return n
}

// settleCorpHome marks this pilot as done with this employer: nothing looks
// again until it signs somewhere else, or until its address drifts far enough
// from that employer to stop meaning anything (corpHomeDrifted).
func (m *Mod) settleCorpHome(charID, corpID int64) {
	corpHomeMu.Lock()
	corpHomeSettledFor[charID] = corpID
	delete(corpHomeNextAt, charID)
	delete(corpHomeLookedFrom, charID)
	corpHomeMu.Unlock()
}

// settleAfterLook is settleCorpHome for the two endings where the pilot
// settleAfterLook is settleCorpHome for a look that left the pilot where it was. It also records the
// system it looked from, because a drift check from that spot would ask the same question.
func (m *Mod) settleAfterLook(charID, corpID, fromSysID int64) {
	m.settleCorpHome(charID, corpID)
	corpHomeMu.Lock()
	corpHomeLookedFrom[charID] = fromSysID
	corpHomeMu.Unlock()
}

// corpHomeDrifted reports whether a settled pilot's home has wandered so far
// corpHomeDrifted reports whether a settled pilot's home is too far from its employer, and if so
// un-settles it so the caller goes on to the ordinary look. It is on the hot path: the usual answer
// is one map read. Distance is raw jumps on the map, not the pilot's own travel rules.
func (m *Mod) corpHomeDrifted(ctx context.Context, b *botRun, now int64) bool {
	// A pinned address was placed, not chosen, so it cannot drift.
	if b.Profile != nil && b.Profile.HomePinned {
		return false
	}
	corpHomeMu.Lock()
	at, scheduled := corpHomeDriftAt[b.CharID]
	if !scheduled {
		corpHomeDriftAt[b.CharID] = now + b.CharID%corpHomeDriftEverySec
	}
	lookedFrom := corpHomeLookedFrom[b.CharID]
	corpHomeMu.Unlock()
	if !scheduled || now < at {
		return false
	}
	// Re-measure on the interval whatever the answer, so a far pilot does not re-measure every pass.
	corpHomeMu.Lock()
	corpHomeDriftAt[b.CharID] = now + corpHomeDriftEverySec
	corpHomeMu.Unlock()

	// It already looked from here and stayed. Nothing has changed.
	if lookedFrom != 0 && lookedFrom == b.SystemID {
		return false
	}

	// Reads the per-employer memo (distance from every system to the nearest address) so a settled
	// roster costs one map lookup each. corpAnchorStations covers NPC and player corporations.
	cr, err := m.corpReachFor(ctx, b.CorpID, 0)
	if err != nil || len(cr.owned) == 0 {
		// An employer with no address of its own cannot be near or far.
		return false
	}
	_, homeSys := m.stationOwnerAndSystem(ctx, m.homeStationFor(ctx, b.CharID))
	if homeSys == 0 {
		return false
	}

	// A player corporation's members are anchored on its headquarters (planPlayerCorpHQ), so drift is
	// measured against it. An NPC corporation is measured against everything it owns.
	from := cr.near
	what := fmt.Sprintf("anything %s has", m.corpNameOf(ctx, b.CorpID))
	if facts, ferr := getAnyCorpFacts(ctx, m.db, b.CorpID); ferr == nil && facts != nil && facts.Player {
		if hq := cr.hqFrom(); hq != nil {
			from = hq
			what = fmt.Sprintf("%s's headquarters", m.corpNameOf(ctx, b.CorpID))
		}
		// A member posted to another office drifts from that office (corpposting.go).
		if base := m.memberBase(ctx, b.CharID, b.CorpID, cr.hqStation); base != cr.hqStation {
			if _, sys := m.stationOwnerAndSystem(ctx, base); sys != 0 {
				if mp, jerr := m.corpJumpMap(ctx, sys); jerr == nil {
					from = mp
					what = fmt.Sprintf("its Base at %s", m.stationLabel(ctx, base))
				}
			}
		}
	}
	if d, ok := from[homeSys]; ok && d <= corpHomeDriftJumps {
		return false
	}

	corpHomeMu.Lock()
	delete(corpHomeSettledFor, b.CharID)
	corpHomeNextAt[b.CharID] = now // look on this pass, not the next window
	corpHomeMu.Unlock()
	m.logBot(b.CharID, logInfo, "corp",
		"its home at %s is more than %d jumps from %s — looking at where it lives again",
		m.stationLabel(ctx, m.homeStationFor(ctx, b.CharID)), corpHomeDriftJumps, what)
	return true
}

// corpNameOf is a corporation's name for a log line, and its ID when the row
// has gone.
func (m *Mod) corpNameOf(ctx context.Context, corpID int64) string {
	var name string
	if err := m.db.QueryRowContext(ctx,
		`SELECT corporationName FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&name); err != nil || name == "" {
		return fmt.Sprintf("corporation %d", corpID)
	}
	return name
}

// deferCorpHome sets when this pilot looks again.
func (m *Mod) deferCorpHome(charID, at int64) {
	corpHomeMu.Lock()
	corpHomeNextAt[charID] = at
	corpHomeMu.Unlock()
}

// corpHomeJumpCost is what one jump of commute costs this pilot choosing among its employer's
// addresses: corpHomeJumpShare of the move-out rate, so Wanderlust bends both the same way.
func corpHomeJumpCost(traits map[string]int) float64 {
	return corpHomeJumpShare * moveOutJumpCost(traits)
}

// corpPlaceScore is what an address costs before the employer's pull: the commute and the crowd
// in its system. Pure, so tests need no map (corphome_test.go).
func corpPlaceScore(jumpCost float64, jumps, homes int, crowdBase float64) float64 {
	return -jumpCost*float64(jumps) - homeCrowdPenalty(homes, crowdBase)
}

// corpHomePull is how hard the employer pulls, in the same points as commute and crowd. `player`
// is checked before the bloc because a player corporation reads as fringe through Bloc().
func corpHomePull(bloc string, player bool, traits map[string]int) float64 {
	base := corpHomePullFringe
	switch {
	case player:
		base = corpHomePullPlayer
	case bloc == blocEmpire:
		base = corpHomePullEmpire
	case bloc == blocOutlaw:
		base = corpHomePullOutlaw
	}
	t := traitScale(traits[TraitLoyalty])
	return base * bend(t, corpHomePullDevoted, 1.0, corpHomePullMercenary)
}

// corpHomeRange is how far this pilot will look for one of its employer's
// stations, in jump-equivalents.
func corpHomeRange(bloc string, player bool, traits map[string]int) int {
	base := corpHomeRangeFringe
	switch {
	case player:
		base = corpHomeRangePlayer
	case bloc == blocEmpire:
		base = corpHomeRangeEmpire
	case bloc == blocOutlaw:
		base = corpHomeRangeOutlaw
	}
	t := traitScale(traits[TraitLoyalty])
	r := int(float64(base) * bend(t, corpHomeRangeDevotedMx, 1.0, corpHomeRangeMercMx))
	if r < 3 {
		r = 3
	}
	return r
}

// corpHomeCandidate is one place this pilot could move in to.
type corpHomeCandidate struct {
	StationID int64
	SystemID  int64
	CorpID    int64
	Jumps     int
	Score     float64
	Medical   bool
	// Ours marks a station the pilot's own employer owns (or, for a player
	// corporation, rents an office in or keeps its headquarters at), as
	// against a fallback neighbour.
	Ours bool
	// HQ marks the employer's headquarters. Among the employer's own addresses it beats score (bestCorpAddress).
	HQ bool
	// Factory marks a station with a manufacturing line this pilot may use. It is independent of Medical.
	Factory bool
}

// corpAddressPlan is where a pilot's two addresses are going. A nil half means leave that one
// where it is.
type corpAddressPlan struct {
	Clone   *corpHomeCandidate
	Factory *corpHomeCandidate
	// Span is how far the search actually ran, in raw jumps. It is reported, not recomputed, so a
	// refusal names the number the search used.
	Span int
	// Fenced counts the employer's stations the pilot's hard travel rules refuse (an activity's
	// security floor or an outlaw's ceiling), as against ones that are merely too far. They need
	// different messages.
	Fenced int
	// Reachable counts the employer's stations the pilot can get to. It separates out of range from
	// reachable but without a clone bay or assembly line.
	Reachable int
	// Limit is the hard travel rule in words (travelRules.limitNote), so the refusal can name it.
	Limit string
}

// same reports whether both addresses landed on one station, which is the
// outcome to prefer and the one worth its own log line.
func (p corpAddressPlan) same() bool {
	return p.Clone != nil && p.Factory != nil && p.Clone.StationID == p.Factory.StationID
}

// travelTo is the station the pilot must fly to. Only the clone needs a journey, since arriving
// makes a station home. The factory is a pointer written where the pilot stands.
func (p corpAddressPlan) travelTo() *corpHomeCandidate {
	if p.Clone != nil {
		return p.Clone
	}
	return nil
}

// corpHomeMove runs from the plan pass: a pilot that changed employer moves its clone, and its
// factory, in with it. It reports whether the bot is now travelling. It runs on every idle plan of
// every bot, so each early return must be cheap.
func (m *Mod) corpHomeMove(ctx context.Context, b *botRun) (travelling bool, err error) {
	if b.StationID == 0 || b.CorpID == 0 || b.Profile == nil {
		return false, nil
	}
	// The operator pinned this pilot's home. Settle against the employer so drift does not ask again.
	if b.Profile.HomePinned {
		m.settleCorpHome(b.CharID, b.CorpID)
		return false, nil
	}
	now := clock.Unix()

	corpHomeMu.Lock()
	settled := corpHomeSettledFor[b.CharID] == b.CorpID
	next, scheduled := corpHomeNextAt[b.CharID]
	if !settled && !scheduled {
		// First sighting schedules the look, unless the roster was just asked to reconsider
		// (corpHomeFirstLookAt, rehome.go).
		corpHomeNextAt[b.CharID] = corpHomeFirstLookAt(b.CharID, now)
	}
	corpHomeMu.Unlock()
	if settled {
		// Settled is not permanent: work moves a pilot, so check the drift.
		if !m.corpHomeDrifted(ctx, b, now) {
			return false, nil
		}
		// Un-settled, and scheduled to look on this pass.
		settled, next, scheduled = false, now, true
	}
	if !scheduled || now < next {
		return false, nil
	}

	// An academy is somewhere to leave, not move into (moveout.go).
	if corpIsSchool(ctx, m.db, b.CorpID) {
		m.settleCorpHome(b.CharID, b.CorpID)
		return false, nil
	}

	facts, err := getAnyCorpFacts(ctx, m.db, b.CorpID)
	if err != nil || facts == nil {
		// A corporation in neither catalogue will not change its answer, so do not retry.
		m.settleCorpHome(b.CharID, b.CorpID)
		return false, nil
	}

	// Already living with the employer? A pilot that manufactures has two
	// addresses and both have to be its employer's before it is done.
	home := m.homeStationFor(ctx, b.CharID)
	homeCorp, homeSys := m.stationOwnerAndSystem(ctx, home)
	wantsFactory := m.wantsFactory(b)
	baseCorp := int64(0)
	if wantsFactory && b.Profile.IndustryStationID != 0 {
		baseCorp, _ = m.stationOwnerAndSystem(ctx, b.Profile.IndustryStationID)
	}
	if homeCorp == b.CorpID && (!wantsFactory || baseCorp == b.CorpID) {
		m.settleCorpHome(b.CharID, b.CorpID)
		return false, nil
	}

	traits, err := m.botTraits(ctx, b)
	if err != nil {
		m.deferCorpHome(b.CharID, now+corpHomeRetrySec)
		return false, err
	}

	// A player corporation is its headquarters: members move in there (planPlayerCorpHQ). An NPC
	// corporation keeps the scored search.
	var (
		plan      corpAddressPlan
		hereScore float64
		hqAnchor  bool
	)
	if facts.Player {
		p, ok, herr := m.planPlayerCorpHQ(ctx, b, facts, wantsFactory)
		if herr != nil {
			m.deferCorpHome(b.CharID, now+corpHomeRetrySec)
			return false, herr
		}
		plan, hqAnchor = p, ok
	}
	if !hqAnchor {
		plan, hereScore, err = m.planCorpAddresses(ctx, b, facts, traits, wantsFactory, homeSys)
		if err != nil {
			m.deferCorpHome(b.CharID, now+corpHomeRetrySec)
			return false, err
		}
	}
	if plan.Clone == nil && plan.Factory == nil {
		// Nowhere to live or build. Say it once and settle: retrying will not put a station in reach.
		m.settleAfterLook(b.CharID, b.CorpID, b.SystemID)
		if plan.Fenced > 0 {
			// Only an outlaw's security ceiling can fence a station (rulesForBot). Name the limit in force.
			m.logBot(b.CharID, logInfo, "corp",
				"signed with %s, but all %d of their station(s) it knows of sit in space this pilot "+
					"will not enter%s — keeping its address at %s",
				facts.Name, plan.Fenced, plan.Limit, m.stationLabel(ctx, home))
			return false, nil
		}
		// Reachable but unusable needs a different fix (a clone bay or assembly line) than a long
		// commute. One line, since this branch settles.
		if plan.Reachable > 0 {
			want := "no clone bay"
			if wantsFactory {
				want = "no clone bay and no assembly line it can use"
			}
			m.logBot(b.CharID, logInfo, "corp",
				"signed with %s and can reach %d of their station(s), but %s in or near their space — "+
					"keeping its address at %s",
				facts.Name, plan.Reachable, want, m.stationLabel(ctx, home))
			return false, nil
		}
		m.logBot(b.CharID, logInfo, "corp",
			"signed with %s but there is nothing of theirs within %d jumps it could live in — keeping its address at %s",
			facts.Name, plan.Span, m.stationLabel(ctx, home))
		return false, nil
	}
	// Already at the headquarters address. The scored path cannot answer this through homeCorp,
	// because a player corporation owns no station.
	if hqAnchor {
		cloneOK := plan.Clone == nil || plan.Clone.StationID == home
		baseOK := plan.Factory == nil || b.Profile.IndustryStationID == plan.Factory.StationID
		if cloneOK && baseOK {
			m.settleCorpHome(b.CharID, b.CorpID)
			return false, nil
		}
	}

	// The margin applies to the clone only; a factory pointer is free to move. The headquarters
	// anchor skips it: the pilot was recruited to live there.
	if !hqAnchor && plan.Clone != nil && plan.Clone.Score < hereScore+corpHomeMargin {
		m.logBot(b.CharID, logInfo, "corp",
			"looked at moving in with %s — %s is not enough better than %s to move house for",
			facts.Name, m.stationLabel(ctx, plan.Clone.StationID), m.stationLabel(ctx, home))
		plan.Clone = nil
	}

	// The factory moves whether or not there is a journey, because it is a pointer, not a bed.
	if plan.Factory != nil {
		m.rememberIndustryBase(ctx, b, plan.Factory.StationID)
	}
	if plan.Clone == nil {
		m.settleAfterLook(b.CharID, b.CorpID, b.SystemID)
		if plan.Factory != nil {
			m.logBot(b.CharID, logInfo, "corp",
				"building at %s under %s now — its clone stays at %s, which is the better bed of the two",
				m.stationLabel(ctx, plan.Factory.StationID), facts.Name, m.stationLabel(ctx, home))
		}
		return false, nil
	}

	if plan.Clone.StationID == b.StationID {
		return false, m.settleIntoCorpStation(ctx, b, facts, plan)
	}

	m.logBot(b.CharID, logInfo, "corp",
		"moving in with %s: %s is %s and %s — flying out to make it home",
		facts.Name, m.stationLabel(ctx, plan.Clone.StationID),
		m.travelNote(ctx, b, plan.Clone.SystemID, plan.Clone.Jumps), corpHomeWhy(plan))
	if err := m.beginTravel(ctx, b, plan.Clone.SystemID, plan.Clone.StationID, "idle"); err != nil {
		m.deferCorpHome(b.CharID, now+corpHomeRetrySec)
		return false, err
	}
	// Held over the trip only: the pilot looks again on docking, because arriving makes the move.
	m.deferCorpHome(b.CharID, now+corpHomeHoldSec)
	return true, nil
}

// corpHomeWhy is the half-sentence the travel log ends on.
func corpHomeWhy(p corpAddressPlan) string {
	c := p.Clone
	switch {
	case !c.Ours:
		return "as close to them as anywhere it can reach"
	case p.same():
		return "theirs, with a medical bay and a line it can build on"
	}
	return "theirs, with a medical bay"
}

// settleIntoCorpStation performs the move, standing in the clone station. The factory goes first:
// a manufacturing pilot's clone is anchored to its base (clone.go, industryAnchor), so a home set
// earlier would be dragged back.
func (m *Mod) settleIntoCorpStation(ctx context.Context, b *botRun, facts *CorpFacts,
	p corpAddressPlan) error {

	// Capture the old addresses before moving: postRelocationCouriers needs them.
	oldHome := m.homeStationFor(ctx, b.CharID)
	var oldFactory int64
	if b.Profile != nil {
		oldFactory = b.Profile.IndustryStationID
	}

	if p.Factory != nil {
		m.rememberIndustryBase(ctx, b, p.Factory.StationID)
	}
	m.setHomeStation(ctx, b, p.Clone.StationID)
	// The clone cover is unchanged but the wake-up station is not, so the next upkeep pass should look.
	forgetCloneCheck(b.CharID)
	m.settleCorpHome(b.CharID, b.CorpID)

	clone := m.stationLabel(ctx, p.Clone.StationID)
	switch {
	case p.same():
		m.logBot(b.CharID, logInfo, "corp",
			"moved in with %s at %s — clone and factory both under its own employer's roof now",
			facts.Name, clone)
	case p.Factory != nil && p.Factory.Ours && !p.Clone.Ours:
		// The employer has the line and a neighbour has the beds.
		m.logBot(b.CharID, logInfo, "corp",
			"settled around %s: building at %s, their own station, and sleeping at %s — "+
				"they keep no medical bay of their own",
			facts.Name, m.stationLabel(ctx, p.Factory.StationID), clone)
	case p.Factory != nil:
		m.logBot(b.CharID, logInfo, "corp",
			"moved in with %s — clone at %s, factory at %s",
			facts.Name, clone, m.stationLabel(ctx, p.Factory.StationID))
	case p.Clone.Ours:
		m.logBot(b.CharID, logInfo, "corp",
			"moved its clone into %s, %s's own station — it wakes up where it works now", clone, facts.Name)
	default:
		m.logBot(b.CharID, logInfo, "corp",
			"%s keeps no medical bay it can reach, so it has moved its clone to %s instead — "+
				"the nearest address to them it could hold", facts.Name, clone)
	}

	// Post courier contracts for assets stranded at the old addresses.
	newHome := p.Clone.StationID
	if oldHome != 0 && oldHome != newHome {
		m.postRelocationCouriers(ctx, b, oldHome, newHome)
	}
	newFactory := int64(0)
	if p.Factory != nil {
		newFactory = p.Factory.StationID
	}
	if oldFactory != 0 && oldFactory != newFactory && oldFactory != oldHome {
		// The old factory differed from both homes, so its assets are stranded too.
		m.postRelocationCouriers(ctx, b, oldFactory, newHome)
	}

	return nil
}

// wantsFactory reports whether this pilot's address plan needs a workplace as well as a bed.
// SupplyDrive is the manufacture disposition (tuning.go) and floors above 0, so in practice every
// pilot wants one.
func (m *Mod) wantsFactory(b *botRun) bool {
	return b.Profile.IndustryStationID != 0 || b.Tune.SupplyDrive > 0
}

// stationOwnerAndSystem is the corporation that owns a station and the system
// it sits in. Zeroes for a station that is not there.
func (m *Mod) stationOwnerAndSystem(ctx context.Context, stationID int64) (corpID, sysID int64) {
	r, ok := m.stationStaticRec(ctx, stationID)
	if !ok {
		return 0, 0
	}
	return r.CorpID, r.SystemID
}

// corpReach is the two per-employer reads the address path needs: the corporation's addresses and
// the jump distance from every system to the nearest of them. Both maps are shared and read-only.
type corpReach struct {
	owned map[int64]corpAddress // stationID to where it is, and whether it is the HQ
	near  map[int64]int         // solarSystemID to jumps to the nearest of theirs
	at    int64

	// The headquarters and the map measured from it alone. near is multi-source, which is the wrong
	// question for a player corporation whose members live at its headquarters. hqNear is nil when
	// there is one address or none; hqFrom then returns near.
	hqStation int64
	hqSystem  int64
	hqNear    map[int64]int
}

// hqFrom is the jump map measured from the employer's headquarters, or nil
// when it has none.
func (c *corpReach) hqFrom() map[int64]int {
	if c.hqSystem == 0 {
		return nil
	}
	if c.hqNear != nil {
		return c.hqNear
	}
	return c.near
}

// corpReachTTLSec bounds how stale a corpReach answer may be.
const corpReachTTLSec = 15 * 60

var (
	corpReachMu    sync.Mutex
	corpReachCache = map[int64]*corpReach{}
)

// corpReachFor answers both, memoized per corporation, because neither read depends on the pilot
// and many pilots share an employer. hqSystem is the fallback source for an outfit that owns nothing.
func (m *Mod) corpReachFor(ctx context.Context, corpID, hqSystem int64) (*corpReach, error) {
	now := clock.Unix()
	corpReachMu.Lock()
	if c, ok := corpReachCache[corpID]; ok && now-c.at < corpReachTTLSec {
		corpReachMu.Unlock()
		return c, nil
	}
	corpReachMu.Unlock()

	owned, err := m.corpAnchorStations(ctx, corpID)
	if err != nil {
		return nil, err
	}
	sources := make([]int64, 0, len(owned)+1)
	for _, a := range owned {
		sources = append(sources, a.SystemID)
	}
	if len(sources) == 0 {
		sources = append(sources, hqSystem)
	}
	near, err := m.corpJumpMap(ctx, sources...)
	if err != nil {
		return nil, err
	}
	c := &corpReach{owned: owned, near: near, at: now}
	for stationID, a := range owned {
		if a.HQ {
			c.hqStation, c.hqSystem = stationID, a.SystemID
			break
		}
	}
	// One extra walk, only where the two maps can differ: with a single address near is the headquarters map.
	if c.hqSystem != 0 && len(owned) > 1 {
		hqNear, herr := m.corpJumpMap(ctx, c.hqSystem)
		if herr != nil {
			return nil, herr
		}
		c.hqNear = hqNear
	}

	corpReachMu.Lock()
	// Each entry holds a jump map over every system, so prune stale ones. Two racing misses computing
	// the same answer is fine.
	for id, old := range corpReachCache {
		if now-old.at >= corpReachTTLSec {
			delete(corpReachCache, id)
		}
	}
	corpReachCache[corpID] = c
	corpReachMu.Unlock()
	return c, nil
}

// corpAddress is one place a corporation has, and whether it is the one the
// corporation itself calls home.
type corpAddress struct {
	SystemID int64
	// HQ marks `corporation.stationID`, the headquarters. At most one address per corporation carries it.
	HQ bool
}

// corpAnchorStations is everywhere one corporation has an address, by station ID: stations it owns
// (staStations), offices it rents (crpoffices) and its headquarters (corporation.stationID). The
// headquarters arm matters for a player corporation, which owns no station. A row that is both an
// office and the headquarters comes back twice from the UNION and is collapsed here: HQ wins.
func (m *Mod) corpAnchorStations(ctx context.Context, corpID int64) (map[int64]corpAddress, error) {
	// Shared per corporation for ten minutes (a copy): every combat plan reads this three-way UNION.
	sctx, cancel := snapContext(ctx)
	defer cancel()
	shared, err := snapshotFor(snapKey("corp-anchors", corpID), 10*time.Minute, func() (map[int64]corpAddress, error) {
		return m.readCorpAnchorStations(sctx, corpID)
	})
	if err != nil {
		return nil, err
	}
	out := make(map[int64]corpAddress, len(shared))
	for k, v := range shared {
		out[k] = v
	}
	return out, nil
}

// forgetCorpAnchors drops the shared address answer after an office or the
// headquarters changes.
func forgetCorpAnchors(corpID int64) {
	snapMu.Lock()
	delete(snapCache, snapKey("corp-anchors", corpID))
	delete(snapCache, snapKey("deposit-target", corpID))
	snapMu.Unlock()
}

func (m *Mod) readCorpAnchorStations(ctx context.Context, corpID int64) (map[int64]corpAddress, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT stationID, solarSystemID, 0 AS hq FROM staStations WHERE corporationID = ?
		UNION
		SELECT s.stationID, s.solarSystemID, 0
		  FROM crpoffices o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ?
		UNION
		SELECT s.stationID, s.solarSystemID, 1
		  FROM corporation c
		  JOIN staStations s ON s.stationID = c.stationID
		 WHERE c.corporationID = ?`, corpID, corpID, corpID)
	if err != nil {
		return nil, fmt.Errorf("corpAnchorStations: %w", err)
	}
	defer rows.Close()
	out := map[int64]corpAddress{}
	for rows.Next() {
		var stationID, sysID int64
		var hq int
		if err := rows.Scan(&stationID, &sysID, &hq); err != nil {
			return nil, err
		}
		a := out[stationID]
		a.SystemID = sysID
		a.HQ = a.HQ || hq != 0
		out[stationID] = a
	}
	return out, rows.Err()
}

// planPlayerCorpHQ is where a pilot in a player corporation lives. It is not a search: the member's
// Base (its posted office, else the headquarters) serves both halves where it can and the nearest
// bay or line serves a half it cannot. It returns false when it does not apply, and the scored path
// runs. See docs/comment-notes-b05.md#player-corp-hq.
func (m *Mod) planPlayerCorpHQ(ctx context.Context, b *botRun, facts *CorpFacts,
	wantsFactory bool) (corpAddressPlan, bool, error) {

	var plan corpAddressPlan
	cr, err := m.corpReachFor(ctx, facts.CorpID, facts.SystemID)
	if err != nil {
		return plan, false, err
	}
	if cr.hqStation == 0 {
		// A corporation with no headquarters row at all. Nothing to anchor on.
		return plan, false, nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return plan, false, err
	}
	// Raw hops and the operator's hard rules only, as in planCorpAddresses: a pilot's nerves price a
	// trip, not where it lives.
	r := rulesForBot(b)
	r.SecAversion = 0
	plan.Limit = r.limitNote()

	// The member's Base: the office its corporation posted it to (corpposting.go). It is the
	// headquarters for everyone not posted elsewhere.
	base, baseSys := cr.hqStation, cr.hqSystem
	if bs := m.memberBase(ctx, b.CharID, facts.CorpID, cr.hqStation); bs != cr.hqStation {
		if _, sys := m.stationOwnerAndSystem(ctx, bs); sys != 0 {
			base, baseSys = bs, sys
		}
	}
	if !u.allowed(baseSys, r) {
		plan.Fenced++
		return plan, false, nil
	}

	clone, factory, err := m.corpHomeTargets(ctx, base)
	if err != nil {
		return plan, false, err
	}
	if !wantsFactory {
		factory = 0
	}
	// In a bot-run corporation the posting owns the workplace and the pass already wrote it. Echo it so
	// the plan never moves it.
	if m.postingOwnsFactory(b) {
		factory = b.Profile.IndustryStationID
	}
	if clone == 0 && factory == 0 {
		return plan, false, nil
	}

	reach, err := m.systemsWithin(ctx, b.SystemID, r, corpHomeRangeCeiling)
	if err != nil {
		return plan, false, err
	}
	if d, ok := reach[baseSys]; ok {
		plan.Span = d
	} else {
		plan.Span = corpHomeRangeCeiling
	}

	// One candidate per station, so a station serving both halves is counted
	// once and reads as one address (corpAddressPlan.same).
	built := map[int64]*corpHomeCandidate{}
	mk := func(stationID int64) *corpHomeCandidate {
		if stationID == 0 {
			return nil
		}
		if c, ok := built[stationID]; ok {
			return c
		}
		built[stationID] = nil
		owner, sysID := m.stationOwnerAndSystem(ctx, stationID)
		if sysID == 0 {
			return nil
		}
		jumps, ok := reach[sysID]
		if !ok {
			if !u.allowed(sysID, r) {
				plan.Fenced++
			}
			return nil
		}
		// CorpID is the station's owner, not the employer: a player corporation owns no station, and the
		// despise check below reads this field.
		_, ours := cr.owned[stationID]
		c := &corpHomeCandidate{
			StationID: stationID, SystemID: sysID, CorpID: owner,
			Jumps: jumps, Ours: ours, HQ: stationID == cr.hqStation,
		}
		plan.Reachable++
		built[stationID] = c
		return c
	}
	plan.Clone, plan.Factory = mk(clone), mk(factory)
	if plan.Clone != nil {
		plan.Clone.Medical = true
	}
	if plan.Factory != nil {
		plan.Factory.Factory = true
	}
	if plan.Clone == nil && plan.Factory == nil {
		return plan, false, nil
	}

	// A despised neighbour is a refusal. Hand the case to the scored path, which offers alternatives.
	for _, c := range []*corpHomeCandidate{plan.Clone, plan.Factory} {
		if c == nil || c.Ours || c.CorpID == 0 {
			continue
		}
		if m.refusesHome(ctx, b, c.CorpID) {
			return corpAddressPlan{}, false, nil
		}
	}
	return plan, true, nil
}

// planCorpAddresses scores everywhere this pilot could move in to and picks its two addresses
// independently, and returns what its current home is worth on the same terms. Either half may be
// nil. One list is scored once and read twice; the line and owner checks run only on the best few,
// as industryFactoryStation does.
func (m *Mod) planCorpAddresses(ctx context.Context, b *botRun, facts *CorpFacts,
	traits map[string]int, wantsFactory bool, homeSys int64) (corpAddressPlan, float64, error) {

	var plan corpAddressPlan
	bloc := facts.Bloc()

	bays, err := m.medicalStations(ctx)
	if err != nil {
		return plan, 0, err
	}
	medical := map[int64]bool{}
	for _, list := range bays {
		for _, s := range list {
			medical[s.StationID] = true
		}
	}
	// corpAnchorStations answers for a player corporation too. It and the jump map belong to the
	// employer, so one shared memo serves every pilot. Read-only: owned and near are the cache's own maps.
	cr, err := m.corpReachFor(ctx, facts.CorpID, facts.SystemID)
	if err != nil {
		return plan, 0, err
	}
	owned, near := cr.owned, cr.near

	// Raw hops and a span stretched to cover the employer: security aversion prices a trip, not an address.
	// See docs/comment-notes-b05.md#address-search-span.
	r := rulesForBot(b)
	r.SecAversion = 0
	span := corpHomeRange(bloc, facts.Player, traits)

	// near is one multi-source BFS out of the employer's systems, used for the stretch and for the
	// neighbourhood rescue below.
	if need, ok := near[b.SystemID]; ok && need > span {
		span = need
	}
	if span > corpHomeRangeCeiling {
		span = corpHomeRangeCeiling
	}
	plan.Span, plan.Limit = span, r.limitNote()

	// The reach runs radius past the span so the rescue's neighbours, which lie further than the
	// employer, fit inside it.
	radius := corpHomeHQRadius
	if bloc == blocOutlaw {
		radius = corpHomeHQRadiusOutlaw
	}
	reach, err := m.systemsWithin(ctx, b.SystemID, r, span+radius)
	if err != nil {
		return plan, 0, err
	}
	u, err := m.universe(ctx)
	if err != nil {
		return plan, 0, err
	}
	pull := corpHomePull(bloc, facts.Player, traits)
	jumpCost := corpHomeJumpCost(traits)
	crowd, crowdBase := m.homeCrowd(ctx)

	// No region head start and no trade-hub term: the commute and the crowd (homeCrowdPenalty,
	// moveout.go) separate stations, so a corporation's members spread over all of its stations.
	place := func(sysID int64, jumps int) float64 {
		return corpPlaceScore(jumpCost, jumps, crowd[sysID], crowdBase)
	}

	// What staying put is worth. A home outside the search radius is priced at the edge of it.
	homeJumps := span
	if j, ok := reach[homeSys]; ok {
		homeJumps = j
	}
	hereScore := place(homeSys, homeJumps)
	if homeSys == 0 {
		hereScore = 0
	}

	// The employer's own addresses. The line check waits for the pick because it is a query per station.
	var ranked []corpHomeCandidate
	ownMedical := false
	for stationID, a := range owned {
		sysID := a.SystemID
		jumps, ok := reach[sysID]
		if !ok {
			// Out of range or fenced? u.allowed answers the second: it applies the hard rules only, aversion stripped.
			if !u.allowed(sysID, r) {
				plan.Fenced++
			}
			continue
		}
		c := corpHomeCandidate{
			StationID: stationID, SystemID: sysID, CorpID: facts.CorpID,
			Jumps: jumps, Medical: medical[stationID], Ours: true, HQ: a.HQ,
		}
		c.Score = place(sysID, jumps) + pull
		if wantsFactory && c.Medical {
			c.Score += corpHomeBothBonus
		}
		ownMedical = ownMedical || c.Medical
		plan.Reachable++
		ranked = append(ranked, c)
	}

	// Where the employer's space cannot hold one of the two addresses, the neighbourhood does, measured
	// from all the employer's systems (near), so it costs no extra walk.
	needNeighbours := len(ranked) == 0 || !ownMedical || wantsFactory
	if needNeighbours && len(near) > 0 {
		seen := map[int64]bool{}
		for stationID := range owned {
			seen[stationID] = true
		}
		add := func(stationID, sysID, corpID int64, hq int, isMedical bool) {
			if seen[stationID] {
				return
			}
			jumps, ok := reach[sysID]
			if !ok {
				return
			}
			seen[stationID] = true
			c := corpHomeCandidate{
				StationID: stationID, SystemID: sysID, CorpID: corpID,
				Jumps: jumps, Medical: isMedical,
			}
			c.Score = place(sysID, jumps) + pull*corpHomeHQPullFrac -
				jumpCost*float64(hq) + jumpCost*m.xenoHomeScore(ctx, b, corpID)
			ranked = append(ranked, c)
		}
		for sysID, list := range bays {
			hq, ok := near[sysID]
			if !ok || hq > radius {
				continue
			}
			for _, s := range list {
				add(s.StationID, sysID, s.CorpID, hq, true)
			}
		}
		if wantsFactory {
			lines, lerr := m.factoryStationsNear(ctx, near, radius)
			if lerr != nil {
				return plan, 0, lerr
			}
			for _, s := range lines {
				add(s.StationID, s.SystemID, s.CorpID, near[s.SystemID], medical[s.StationID])
			}
		}
	}
	if len(ranked) == 0 {
		return plan, hereScore, nil
	}

	plan.Clone = m.bestCorpAddress(ctx, b, ranked, facts.Player, func(c *corpHomeCandidate) bool {
		return c.Medical
	})
	if wantsFactory {
		plan.Factory = m.bestCorpAddress(ctx, b, ranked, facts.Player, func(c *corpHomeCandidate) bool {
			if _, err := m.manufactureLineAt(ctx, b, c.StationID, 0); err != nil {
				return false
			}
			c.Factory = true
			return true
		})
	}
	return plan, hereScore, nil
}

// bestCorpAddress offers a scored list to ok and returns the first candidate that passes it and
// whose owner the pilot does not despise, trying at most corpHomeTries. With hqFirst (player
// corporations only) the headquarters is offered outright first, then the rest in a seeded softmax
// order. The list is not modified. See docs/comment-notes-b05.md#best-corp-address.
func (m *Mod) bestCorpAddress(ctx context.Context, b *botRun, list []corpHomeCandidate,
	hqFirst bool, ok func(*corpHomeCandidate) bool) *corpHomeCandidate {

	var hq, rest []int
	for i := range list {
		if hqFirst && list[i].Ours && list[i].HQ {
			hq = append(hq, i)
		} else {
			rest = append(rest, i)
		}
	}
	sort.Slice(hq, func(x, y int) bool { return list[hq[x]].Score > list[hq[y]].Score })
	order := make([]int, 0, len(list))
	order = append(order, hq...)
	// Seeded per (pilot, employer) over a station-ordered list, so every look gives one answer until
	// scores move. An unseeded draw re-rolled each look and the pilot never settled.
	sort.Slice(rest, func(x, y int) bool { return list[rest[x]].StationID < list[rest[y]].StationID })
	for _, k := range softmaxOrderSeeded(len(rest), func(k int) float64 {
		return list[rest[k]].Score
	}, b.Tune.PickTemperature*addressPickTempPts, b.CharID*1_000_003^b.CorpID) {
		order = append(order, rest[k])
	}

	tries := 0
	for _, i := range order {
		if tries >= corpHomeTries {
			break
		}
		tries++
		c := list[i]
		if !ok(&c) {
			continue
		}
		// A pilot will not put its body or livelihood with a corporation it despises while anything else
		// is on offer. Its own employer is exempt.
		if !c.Ours && m.refusesHome(ctx, b, c.CorpID) {
			continue
		}
		out := c
		return &out
	}
	return nil
}

// pinPilotAddress moves one pilot's two addresses and pins them. A zero half is left where it is
// (corpHomeTargets). The factory goes first because a manufacturing pilot's clone is anchored to its
// base (clone.go, industryAnchor). The pilot does not fly: home takes effect on the next pod.
func (m *Mod) pinPilotAddress(ctx context.Context, charID, clone, factory int64) error {
	if factory != 0 {
		if err := setIndustryBaseByID(ctx, m.db, charID, factory); err != nil {
			return err
		}
	}
	if clone != 0 {
		m.setHomeStationByID(ctx, charID, clone)
	}
	if err := setHomePinned(ctx, m.db, charID, true); err != nil {
		return err
	}
	// The same resets the profile editor fires, so the home steps weigh the new address.
	forgetCloneCheck(charID)
	forgetMoveOut(charID)
	forgetCorpHome(charID)
	return nil
}

// stationHasLine reports whether a station has a manufacturing assembly line. A line is a
// ramassemblylines row; the Factory service bit only says the S&I window opens there.
func (m *Mod) stationHasLine(ctx context.Context, stationID int64) bool {
	var one int64
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM ramassemblylines
		 WHERE containerID = ? AND activityID = 1 LIMIT 1`, stationID).Scan(&one)
	return err == nil
}

// corpHomeTargets turns one station an operator picked into the two addresses a pilot needs. The
// station wins both halves where it can, else the nearest station that can; 0 means leave that
// address alone. It uses raw jumps and no travel rules: this is an instruction, so every pilot in
// the corporation must get the same answer.
func (m *Mod) corpHomeTargets(ctx context.Context, stationID int64) (clone, factory int64, err error) {
	_, sysID := m.stationOwnerAndSystem(ctx, stationID)
	if sysID == 0 {
		return 0, 0, fmt.Errorf("station %d is not in the map", stationID)
	}
	if m.stationHasCloning(ctx, stationID) {
		clone = stationID
	}
	if m.stationHasLine(ctx, stationID) {
		factory = stationID
	}
	if clone != 0 && factory != 0 {
		return clone, factory, nil
	}

	// One walk of the gate graph, read by both halves.
	near, err := m.corpJumpMap(ctx, sysID)
	if err != nil {
		return 0, 0, err
	}
	if clone == 0 {
		bays, berr := m.medicalStations(ctx)
		if berr != nil {
			return 0, 0, berr
		}
		best := -1
		for bySys, list := range bays {
			d, ok := near[bySys]
			if !ok || d > corpHomeTargetRadius || (best >= 0 && d > best) {
				continue
			}
			for _, s := range list {
				// Ties go to the lowest station ID, so pressing the button twice gives the same answer.
				if d != best || clone == 0 || s.StationID < clone {
					best, clone = d, s.StationID
				}
			}
		}
	}
	if factory == 0 {
		lines, lerr := m.factoryStationsNear(ctx, near, corpHomeTargetRadius)
		if lerr != nil {
			return 0, 0, lerr
		}
		best := -1
		for _, s := range lines {
			d, ok := near[s.SystemID]
			if !ok {
				continue
			}
			if best < 0 || d < best || (d == best && s.StationID < factory) {
				best, factory = d, s.StationID
			}
		}
	}
	return clone, factory, nil
}

// factoryStationsNear is every station with a manufacturing line inside the given jump map, in one
// query. The per-line test (queue, standing) runs later on the few that score.
func (m *Mod) factoryStationsNear(ctx context.Context, near map[int64]int, radius int) ([]medStation, error) {
	var sysIDs []int64
	for sysID, hops := range near {
		if hops <= radius {
			sysIDs = append(sysIDs, sysID)
		}
	}
	if len(sysIDs) == 0 {
		return nil, nil
	}
	// near covers all the employer's systems, so keep the closest: each system is a placeholder in the
	// IN() below and the far tail would not win.
	if len(sysIDs) > factoryNearSystemCap {
		sort.Slice(sysIDs, func(i, j int) bool { return near[sysIDs[i]] < near[sysIDs[j]] })
		sysIDs = sysIDs[:factoryNearSystemCap]
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT s.stationID, s.solarSystemID, COALESCE(s.corporationID, 0)
		  FROM ramassemblylines al
		  JOIN staStations s ON s.stationID = al.containerID
		 WHERE al.activityID = 1
		   AND s.solarSystemID IN (`+placeholders(len(sysIDs))+`)`, int64Args(sysIDs)...)
	if err != nil {
		return nil, fmt.Errorf("factoryStationsNear: %w", err)
	}
	defer rows.Close()
	var out []medStation
	for rows.Next() {
		var s medStation
		if err := rows.Scan(&s.StationID, &s.SystemID, &s.CorpID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func init() {
	registerCharForget("corpHome*", forgetCorpHome)
	registerCorpForget("corpReachCache", func(id int64) {
		corpReachMu.Lock()
		delete(corpReachCache, id)
		corpReachMu.Unlock()
	})
}
