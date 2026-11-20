package rolebots

// Industry, the manufacturing activity. A supplier-leaning pilot reads the unmet-demand ledger
// (rolebot_market_demand), buys the blueprint from the market only, builds at a factory station (stockpile
// ore, refine, install a ramJobs run off the BPO) and lists the product there as a resting sell order.
// The supply trait sets how much of a manufacturer a pilot is (SupplyDrive, floored at 0.15), greed sets the
// margin (IndustryMargin), and make or buy follows the mining appetite (industryMinesOwnOre). A candidate needs
// a blueprint seller, skills that meet the blueprint's, and a wallet that covers the worst case with a cushion.
//
// A project is sunk capital: it lives in StateData's Ind* fields, survives re-plans through idleData and resumes
// from planSolo (industryResume). While the job cooks the bot is free; the resume hook collects when it rings.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	serverram "gurimoon/server/ram"
)

const (
	// industryClaimTTLSec is how long a claim on a demand row holds without a heartbeat, long enough for the
	// mining trips a project runs inside.
	industryClaimTTLSec = 6 * 3600
	// industryDemandMaxAgeSec ignores rows nobody has asked about in two
	// weeks: stale wants are not worth a blueprint.
	industryDemandMaxAgeSec = 14 * 86400
	// industryMaxMiningTrips caps the mine-for-materials loop before the bot buys the rest instead: a mineral
	// its belts never yield must not strip-mine forever.
	industryMaxMiningTrips = 6
	// industryMaxMiningTripsHard is the cap when nothing else is left to try: the market will not sell what is
	// missing and the materials in hand do not cover one run. Digging is then the only way to finish, so it keeps
	// digging; industryStallSec is the real end.
	industryMaxMiningTripsHard = 20
	// industryBPOAmortProjects spreads the blueprint's price over its working life for the cost-plus sale price: a
	// BPO pays for itself over about ten projects. It is per project, not per run, so the term does not grow with
	// batch size (a 200-run ammunition batch would carry four prints). See docs/comment-notes-b12.md#bpo-amortisation.
	industryBPOAmortProjects = 10
	// industryBPOBand is how far the blueprint hunt looks: the whole reachable map, where every other acquisition is
	// fenced at one region (shopLadder's buy cap). The blueprint is capital bought once and the only door into the
	// lane for a pilot with empty neighbours (industryStartProject). shopClimb still opens at the ordinary band.
	industryBPOBand = bandUniverse
	// industryDurableBatchUnits is a stocking batch of a durable (a module, rig or drone). It matches what a trader's
	// hauler moves between stations; below about ten the fixed costs of the project (blueprint trip, material run,
	// install fee) are spread over almost nothing.
	industryDurableBatchUnits = 15
	// industryHullMaxRuns is the batch a ship category reports. Hulls do not stock to a batch (industryStocksToBatch)
	// and it is no ceiling: a hull job is want-sized under the print's limit and hullFundableRuns caps it to what
	// the pilot can pay for.
	industryHullMaxRuns = 5
	// industryMaxRunsPerJob is the fallback for anything that is neither
	// (a blueprint product with an odd category).
	industryMaxRunsPerJob = 5
	// industryAmmoBatchRounds is what a batch of ammunition aims at, in rounds rather than runs: ammunition is burned,
	// so the question is how long a batch keeps a gun firing. One number covers a 100-round charge (200 runs) and a
	// T2 charge of 5000 rounds a run (4 runs); one factory cannot glut the ammunition market.
	industryAmmoBatchRounds = 20000
	// industryAmmoMaxRuns is the backstop on that arithmetic for a charge
	// with a tiny portion size.
	industryAmmoMaxRuns = 250
	// industryCrystalMaxRuns sizes the charges not consumed by the shot (frequency and mining crystals, probes, scripts).
	// They wear out slowly and sell singly, so they sit between a module and a magazine.
	industryCrystalMaxRuns = 10
	// Batch sizes above are floors for stock (industrySizeRuns), not ceilings: the print's run limit is the law, as for a player.
	// industrySellDays is the resting order's lifetime, a week at most; unsold goods return to the factory hangar.
	industrySellDays = 7
	// industryAffordMultiple is the wallet cushion over the project's
	// worst case (BPO + fees + buying every material at market).
	industryAffordMultiple = 1.25
	// industryLiquidityFloor is the fraction of the committed cash a project may not spend. Listing the output costs a
	// broker fee, so a project sized to spend the last ISK deadlocks with goods and no money to list them. It is read
	// against cash cost because the sale value is unknown at the gate. See docs/comment-notes-b12.md#liquidity-floor.
	industryLiquidityFloor = 0.05
	// industryLiquidityMin is the floor under the floor: the minimum broker fee is flat (marketMinBrokerFee), so a small
	// project's percentage would not cover a listing. Two, since a finished project often lists the output and leftovers.
	industryLiquidityMin = 2 * marketMinBrokerFee
	// industryStallSec abandons a project that has made no progress to an installed job in a day, so a claim does not
	// pin the type forever.
	industryStallSec = 24 * 3600
	// industryMaxShortInstalls is how many "insufficient materials" refusals in a row an install may collect before the
	// project is abandoned (industryInstall). The shortfall check said covered, so one is a race and three a bill it cannot read.
	industryMaxShortInstalls = 3
	// industryBeltRetrySec is the pause after a materials trip the world refused (every belt hopeless, no usable fit, no
	// money for one) before the project asks again; the distress sweep and the market move on this timescale.
	industryBeltRetrySec = 15 * 60
	// industryOvenWatchSec is how close to ringing a job must be before the pilot stops taking new work and returns to
	// the factory to wait. Longer waits are spent on other work.
	industryOvenWatchSec = 15 * 60
	// industryOvenPollSec bounds one waiting leg, so a pilot parked at its
	// factory still re-reads the world every couple of minutes instead of
	// sleeping through the entire bake in one step.
	industryOvenPollSec = 120
	// industryTripOreM3 is the ore one materials run is assumed to bring home, and industryRefineYieldGuess what survives
	// the refinery. Both are conservative estimates that size a batch and spend nothing: a batch sized short is topped up,
	// one sized long is a twenty-trip grind.
	industryTripOreM3        = 2500.0
	industryRefineYieldGuess = 0.5
	// industryBookFailCooldownSec is how long a pilot leaves manufacturing alone after a full build-book pass where nothing
	// survived the gate (IndBookFailAt). Five minutes keeps several passes of other work between retries; nothing invalidates
	// it on a reseed, and the market snapshot it races is itself 90 s old (marketSnapTTL).
	industryBookFailCooldownSec = 5 * 60
)

// ── demand ledger ───────────────────────────────────────────────────────────
// The build book lives in buildbook.go. What stays here is the claim machinery, which only the ledger tier uses: it is
// the only tier with a shared, finite row two suppliers could both answer.

// claimDemand takes the row for this pilot atomically, so two suppliers racing the same type lose cleanly (RowsAffected 0).
func (m *Mod) claimDemand(ctx context.Context, charID, typeID int64) (bool, error) {
	now := clock.Unix()
	res, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_market_demand SET claimedBy = ?, claimedAt = ?
		WHERE typeID = ? AND (claimedBy = 0 OR claimedBy = ? OR claimedAt < ?)`,
		charID, now, typeID, charID, now-industryClaimTTLSec)
	if err != nil {
		return false, fmt.Errorf("claim demand: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	// RowsAffected counts rows changed, not matched: a supplier re-claiming its own row in the same second writes the same
	// values and reads zero. Ask who holds the row instead of inferring it from the count.
	var holder int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT claimedBy FROM rolebot_market_demand WHERE typeID = ?`, typeID).Scan(&holder); err != nil {
		return false, nil
	}
	return holder == charID, nil
}

// touchClaim refreshes the heartbeat so a project spanning mining trips
// keeps its claim.
func (m *Mod) touchClaim(ctx context.Context, charID, typeID int64) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_market_demand SET claimedAt = ?
		WHERE typeID = ? AND claimedBy = ?`, clock.Unix(), typeID, charID)
}

// settleDemand marks a claim answered: the produced units come off the
// (decayed) want, the claim clears, servedAt records that supply happened.
// wanted is assigned before decayAt: the update runs left to right
// (demandledger.go).
func (m *Mod) settleDemand(ctx context.Context, charID, typeID, produced int64) {
	now := clock.Unix()
	if _, err := m.db.ExecContext(ctx, `
		UPDATE rolebot_market_demand
		SET wanted = GREATEST(0, `+demandWantedSQL("", now)+` - ?), decayAt = ?,
		    claimedBy = 0, claimedAt = 0, servedAt = ?
		WHERE typeID = ? AND claimedBy = ?`,
		produced, now, now, typeID, charID); err != nil {
		m.logBot(charID, logError, "industry", "settle demand for type %d: %v", typeID, err)
	}
}

// releaseDemand hands an abandoned claim back untouched.
func (m *Mod) releaseDemand(ctx context.Context, charID, typeID int64) {
	_, _ = m.db.ExecContext(ctx, `
		UPDATE rolebot_market_demand SET claimedBy = 0, claimedAt = 0
		WHERE typeID = ? AND claimedBy = ?`, typeID, charID)
}

// ── the activity ────────────────────────────────────────────────────────────

// industryOpportunity is the appetite probe: can this pilot run a factory
// right now, and is there anything on the ledger worth building?
func industryOpportunity(m *Mod, ctx context.Context, b *botRun) (float64, string) {
	// Capacity is the first question, not whether one job is cooking: a pilot with three lines and two running is available.
	if free, err := m.industryFreeSlots(ctx, b); err == nil && free == 0 {
		return 0, "every manufacturing line it can run is already busy"
	}
	if b.State.Data.IndJobID != 0 {
		return 0, "its manufacturing job is still cooking"
	}
	if b.State.Data.IndTypeID != 0 {
		// An in-flight project resumes through industryResume; scoring it
		// here only matters on the boardStep re-entry path.
		return 1, "picking its project back up"
	}
	// The last full read of the book found nothing it could make (industryStartProject). Checked after the in-flight
	// project so the cooldown never strands paid-for capital.
	if until := b.State.Data.IndBookFailAt; until > clock.Unix() {
		return 0, fmt.Sprintf("nothing it could build last time it looked — trying again in %dm",
			(until-clock.Unix())/60+1)
	}
	f, why := opportunityFromFits(JobTrade)(m, ctx, b)
	if f == 0 {
		return 0, why
	}
	cands, err := m.industryCandidates(ctx, b.CharID, 1)
	if err != nil {
		return 0, "build book unreadable: " + err.Error()
	}
	if len(cands) > 0 {
		return f, why
	}
	// Nothing asked for, nothing on the shelf and no print is not the end: the capability tier of the build book (buildbook.go)
	// needs none of them. The probe asks the cheap half, whether the census knows anything buildable.
	census, serr := m.capabilityLedger(ctx, 1)
	if serr != nil {
		return 0, "nothing asked for, and the capability census is unreadable: " + serr.Error()
	}
	if len(census) > 0 {
		return f * evidenceWeightCapability, "nothing asked for, but the roster burns things its factory could make"
	}
	return 0, "nothing on the demand ledger, nothing on its shelf, and the capability census is empty"
}

// industryPlan starts (or resumes) the activity from idle.
func (m *Mod) industryPlan(ctx context.Context, b *botRun) error {
	st := b.State
	if st.Data.IndTypeID != 0 {
		st.Activity = ActIndustry
		return m.industryAdvance(ctx, b)
	}
	// Capacity before commitment: installManufactureJob refuses at the cap, but by then the pilot has bought a blueprint
	// and mined a bill of materials.
	free, err := m.industryFreeSlots(ctx, b)
	if err != nil {
		return err
	}
	if free == 0 {
		// A full factory can still research the corporation's prints (corpjobs.go): the lab slots are a separate track.
		st.Activity = ActIndustry
		if handled, err := m.corpResearchArm(ctx, b); handled || err != nil {
			return err
		}
		return fmt.Errorf("every manufacturing line it can run is already busy")
	}
	st.Activity = ActIndustry
	ready, err := m.ensureActivityShip(ctx, b, ActIndustry)
	if err != nil || !ready {
		return err
	}
	return m.industryStartProject(ctx, b)
}

// industrialBootstrap promotes the first Industrial hull in the build book to the front when the factory station has
// none listed; once Industrials are on the shelf normal scoring takes over. The second return is the promoted typeID
// (0 for none); the caller pins it as the first attempt (pinFirstAttempt), because the draw ignores book order.
func (m *Mod) industrialBootstrap(ctx context.Context, b *botRun, factoryID int64, cands []indCandidate) ([]indCandidate, int64) {
	if len(cands) == 0 {
		return cands, 0
	}
	// Check whether ANY Industrial hull (groupID 28 or 380) is listed here.
	var count int64
	err := m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM market_orders o
		JOIN invTypes t ON t.typeID = o.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE o.stationID = ? AND o.bid = 0 AND o.orderState = 1
		  AND g.groupID IN (28, 380)`, factoryID).Scan(&count)
	if err != nil || count > 0 {
		return cands, 0 // already stocked or query failed
	}
	// Find the Industrials in the candidate list.
	industrials := m.hullsInGroups(ctx, cands, industrialGroupIDs)
	if len(industrials) == 0 {
		return cands, 0
	}
	// A hull whose print nobody sells cannot be bootstrapped by anybody, so each pass that finds no print rests it
	// (noteBootstrapNoPrint).
	for t := range industrials {
		if bootstrapNoPrint(t, clock.Unix()) {
			delete(industrials, t)
		}
	}
	i := pickBootstrapIndustrial(cands, industrials)
	if i < 0 {
		return cands, 0 // every Industrial in the book already has a builder
	}
	c := cands[i]
	m.logBot(b.CharID, logInfo, "industry",
		"bootstrap: no Industrial hull listed at the factory — promoting %s to front of book",
		c.TypeName)
	promoted := make([]indCandidate, 0, len(cands))
	promoted = append(promoted, c)
	promoted = append(promoted, cands[:i]...)
	promoted = append(promoted, cands[i+1:]...)
	return promoted, c.TypeID
}

// pickBootstrapIndustrial is the index of the Industrial the bootstrap should promote: the first in book order that
// nobody is building yet, or -1. Skipping a type already in an oven spreads the guarantee across the Industrial line,
// since book order is nearly the same at every factory. Pure.
func pickBootstrapIndustrial(cands []indCandidate, industrials map[int64]bool) int {
	for i, c := range cands {
		if industrials[c.TypeID] && c.Building == 0 {
			return i
		}
	}
	return -1
}

// bootstrapNoPrintSec is how long an Industrial whose print was found for
// sale nowhere is left out of the bootstrap. A print listed meanwhile is
// noticed by the ordinary draw, which never consults this.
const bootstrapNoPrintSec = 6 * 3600

var (
	bootstrapNoPrintMu    sync.Mutex
	bootstrapNoPrintUntil = map[int64]int64{}
)

// noteBootstrapNoPrint rests typeID out of the bootstrap after a pass found
// no blueprint for it anywhere.
func noteBootstrapNoPrint(typeID, now int64) {
	bootstrapNoPrintMu.Lock()
	bootstrapNoPrintUntil[typeID] = now + bootstrapNoPrintSec
	bootstrapNoPrintMu.Unlock()
}

// bootstrapNoPrint reports whether typeID is resting out of the bootstrap.
func bootstrapNoPrint(typeID, now int64) bool {
	bootstrapNoPrintMu.Lock()
	defer bootstrapNoPrintMu.Unlock()
	until, ok := bootstrapNoPrintUntil[typeID]
	if ok && until <= now {
		delete(bootstrapNoPrintUntil, typeID)
		return false
	}
	return ok
}

// pinFirstAttempt puts the bootstrap's promoted candidate at the head of the attempt order, if it survived every filter
// (it must still be in cands) and nobody else is building it: one supplier per Industrial type at a time. The attempt
// budget n is preserved.
func pinFirstAttempt(order, cands []indCandidate, pinned int64, n int) []indCandidate {
	if pin := findCandidate(cands, pinned); pin == nil || pin.Building > 0 {
		return order
	}
	return pinAttempts(order, cands, []int64{pinned}, n)
}

// pinAttempts puts the given candidates at the head of the attempt order, in the order given. A pin that is 0, repeated
// or no longer in the book is skipped, the rest of the draw follows without duplicates, and n is preserved. Vetting a
// pin is the caller's job.
func pinAttempts(order, cands []indCandidate, pins []int64, n int) []indCandidate {
	out := make([]indCandidate, 0, len(order)+len(pins))
	pinned := map[int64]bool{}
	for _, id := range pins {
		if id == 0 || pinned[id] {
			continue
		}
		if pin := findCandidate(cands, id); pin != nil {
			out = append(out, *pin)
			pinned[id] = true
		}
	}
	if len(pinned) == 0 {
		return order
	}
	for _, c := range order {
		if !pinned[c.TypeID] {
			out = append(out, c)
		}
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func findCandidate(cands []indCandidate, typeID int64) *indCandidate {
	if typeID == 0 {
		return nil
	}
	for i := range cands {
		if cands[i].TypeID == typeID {
			return &cands[i]
		}
	}
	return nil
}

// hullCapablePerBuilder is how many pilots able to fly a hull justify one more
// supplier of it before the owned-hull pin stands aside: a cruiser 900 pilots
// can fly is worth six builders at once, one 40 pilots can fly is worth one.
const hullCapablePerBuilder = 150

// pickOwnedHullPin is the hull above a frigate that this pilot holds the print for and the roster is short of, or 0:
// the second guaranteed attempt in a pass, after the Industrial bootstrap. Guaranteed rather than scored, because the
// print is one row of a large book and lost the draw to ammunition. Short means few rivals and fewer builders than the
// headcount supports; highest score wins. Pure; owned is keyed by blueprint typeID.
func pickOwnedHullPin(cands []indCandidate, hulls, owned map[int64]bool) int64 {
	var best *indCandidate
	for i := range cands {
		c := &cands[i]
		if !hulls[c.TypeID] || !owned[c.BPTypeID] {
			continue
		}
		if c.Rivals >= combatHullUrgencyThreshold {
			continue
		}
		room := int(c.Capable / hullCapablePerBuilder)
		if room < 1 {
			room = 1
		}
		if c.Building >= room {
			continue
		}
		if best == nil || c.Score > best.Score {
			best = c
		}
	}
	if best == nil {
		return 0
	}
	return best.TypeID
}

// industryStartProject picks the factory, reads the build book (buildbook.go, four evidence tiers priced by local
// competition), then takes the first candidate that survives the gate and heads for the blueprint.
func (m *Mod) industryStartProject(ctx context.Context, b *botRun) error {
	st := b.State
	// The corporation's work first (corpjobs.go): a contract builder takes the work board's orders, and a pilot holding a
	// purchase order buys the library's print, before either starts a project of its own.
	if handled, err := m.corpWorkArm(ctx, b); handled || err != nil {
		return err
	}
	factoryID, factorySys, err := m.industryFactoryStation(ctx, b)
	if err != nil {
		return fmt.Errorf("no usable factory: %w", err)
	}
	// A standing supply bid belongs to a factory, not a project: when the factory changes, bids at the old counter are
	// stale escrow (industrybids.go).
	m.dropStaleSupplyBids(ctx, b.CharID, factoryID)

	// These rules are unscoped: the only reader below is oresReachable, which asks how deep this pilot may mine, a security
	// question and not a geography one. The blueprint search climbs the residency ladder itself (shopLadder).
	r := rulesForBot(b)
	prefer := m.homeRegionSet(ctx, b)
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return err
	}
	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return err
	}
	industrySkill := skills[skillIndustry]
	peSkill := skills[skillProductionEff]
	// Whether the minerals half of a bill is fundable in time rather than ISK depends on whether this pilot can work a
	// belt and would (industryMinesOwnOre): a pure trader promising itself it will go mining starts a project that cannot finish.
	canMine, err := m.canWorkJob(ctx, b.CharID, JobMining)
	if err != nil {
		return err
	}
	minesOwn := canMine && industryMinesOwnOre(b.Tune)

	// The whole scored book, not its top twenty: the draw below walks it with an attempt budget (industryBookAttempts), so
	// a tail row still gets tried in proportion.
	cands, err := m.buildBook(ctx, b, factoryID, factorySys, 0)
	if err != nil {
		return err
	}
	// Bootstrap: if the factory station has no Industrial hulls on the market, promote the first Industrial in the book
	// to the front.
	cands, bootstrapType := m.industrialBootstrap(ctx, b, factoryID, cands)
	// Which of these are hulls worth a mining campaign rather than a trip or two, read once for the whole book.
	campaignHulls := m.hullsInGroups(ctx, cands, m.campaignHullGroupsFor(ctx))
	m.logBot(b.CharID, logInfo, "industry", "build book at %s: %s",
		m.stationLabel(ctx, factoryID), bookSummary(cands))
	var skipped []string
	// The cheapest purely monetary refusal of the pass, if any: it leaves this function as a shortfallError, not prose.
	var cheapestNeed float64
	// The factory's own horizon, built at most once per pass and shared by the affordability filter, the obtainability gate below and
	// the supply bids at commit: what this pilot can reach from the counter it builds at.
	var factoryReach map[int64]int
	reachOfFactory := func() map[int64]int {
		if factoryReach == nil {
			if rc, rerr := m.reachFrom(ctx, factorySys, r); rerr == nil {
				factoryReach = rc
			} else {
				factoryReach = map[int64]int{}
			}
		}
		return factoryReach
	}
	// Hulls above a frigate in the book, read once: the owned-print pin and the pass-over log (hullgates.go) both ask.
	ladderHulls := m.hullsInGroups(ctx, cands, hullLadderGroupIDs)
	var passOver hullPassOver
	// Prints it could never pay for leave the draw, not the book: a book headed by cruisers would spend every attempt on
	// "needs ~34M ISK on hand" against a small wallet. Same test as the per-candidate gate below, done once. A print with a
	// claimable copy on escrow stays: the loop takes the copy before its own wallet gate.
	cands, priced, ownedPrints, droppedPrints := m.dropUnaffordablePrints(ctx, b, cands, balance, reachOfFactory)
	if len(priced) > 0 {
		for i, need := range priced {
			if cheapestNeed == 0 || need < cheapestNeed {
				cheapestNeed = need
			}
			if i < len(droppedPrints) && ladderHulls[droppedPrints[i].TypeID] {
				passOver.note(droppedPrints[i].TypeID, droppedHullNote(droppedPrints[i], need))
			}
		}
		skipped = append(skipped, fmt.Sprintf("%d print(s) beyond its wallet (cheapest needs ~%.0f ISK on hand)",
			len(priced), cheapestNeed))
	}
	// What the pilot's own prints have been researched to (labwork.go raises these), read once for the whole book. A
	// candidate it does not own is quoted at ME 0 and PE 0, as bought.
	ownedLevels, olerr := m.ownedBlueprintLevels(ctx, b.CharID, factoryID)
	if olerr != nil {
		ownedLevels = map[int64][2]int64{}
	}
	levelsOf := func(bpTypeID int64) (me, pe int64) {
		lv := ownedLevels[bpTypeID]
		return lv[0], lv[1]
	}
	// A batch fanned out over several prints takes its next lane first (copylanes.go): the next print is in the hangar,
	// on its way, or coming off the copy line.
	splitPin := industrySplitPin(&st.Data, cands)
	// Types whose original is on this factory's copy line for the pilot's
	// own lines: the copies will build them, so nothing buys a second print.
	copying := map[int64]bool{}
	for _, j := range st.Data.LabCooking {
		if j.ActivityID == ramActivityCopying && j.FactoryID == factoryID {
			copying[j.BPTypeID] = true
		}
	}
	// The pilot's free manufacturing lines: the lanes a batch may fan over.
	// Read once, and only when a candidate gets as far as asking.
	freeLines := -1
	freeSlots := func() int {
		if freeLines < 0 {
			if n, ferr := m.industryFreeSlots(ctx, b); ferr == nil {
				freeLines = n
			} else {
				freeLines = 1
			}
		}
		return freeLines
	}
	// Its own print of a hull the roster is short of gets the second
	// guaranteed attempt, behind the Industrial bootstrap (pickOwnedHullPin).
	hullPin := pickOwnedHullPin(cands, ladderHulls, ownedPrints)
	if pin := findCandidate(cands, hullPin); pin != nil {
		m.logBot(b.CharID, logInfo, "industry",
			"reserving an attempt for its own %s print — %d pilot(s) can fly one, %d being built",
			pin.TypeName, pin.Capable, pin.Building)
	}
	// Ranked for the log, drawn for the decision (drawOrder): taking the top row put ten pilots on the same charge.
	attempts := pinFirstAttempt(
		pinAttempts(drawOrderN(cands, b.Tune.PickTemperature, industryBookAttempts),
			cands, []int64{splitPin, hullPin}, industryBookAttempts),
		cands, bootstrapType, industryBookAttempts)
	// A hull refused in a pass that goes on to build something else would lose its reason with the pass: the last refusal a hull
	// candidate added is kept for the pass-over line at commit.
	var prevCand indCandidate
	prevMark := -1
	notePrevHull := func() {
		if prevMark >= 0 && ladderHulls[prevCand.TypeID] && len(skipped) > prevMark {
			passOver.note(prevCand.TypeID, skipped[len(skipped)-1])
		}
	}
	for _, cand := range attempts {
		notePrevHull()
		prevCand, prevMark = cand, len(skipped)
		candME, candPE := levelsOf(cand.BPTypeID)
		// Inert blueprint types and orphaned products (a productTypeID that is not an item) install fine and fail on delivery,
		// so they are checked before any money moves.
		bpType, err := evedb.GetBlueprintTypeInfo(ctx, m.db, cand.BPTypeID)
		if err != nil {
			skipped = append(skipped, cand.TypeName+": blueprint type unusable")
			continue
		}
		var one int64
		if m.db.QueryRowContext(ctx,
			`SELECT 1 FROM invTypes WHERE typeID = ?`, bpType.ProductTypeID).Scan(&one) != nil {
			skipped = append(skipped, cand.TypeName+": blueprint's product is not an item")
			continue
		}
		// Skills as they stand: no grants, no purchases (docs/SKILL_TRAINING.md).
		if reqs, rerr := evedb.GetActivitySkillReqs(ctx, m.db, cand.BPTypeID, 1); rerr == nil {
			short := false
			for _, req := range reqs {
				if skills[req.SkillID] < req.Level {
					short = true
					break
				}
			}
			if short {
				skipped = append(skipped, cand.TypeName+": lacks the build skills")
				continue
			}
		}
		line, err := m.manufactureLineAt(ctx, b, factoryID, bpType.ProductTypeID)
		if err != nil {
			skipped = append(skipped, cand.TypeName+": "+err.Error())
			continue
		}

		portion, err := evedb.GetTypePortionSize(ctx, m.db, bpType.ProductTypeID)
		if err != nil || portion <= 0 {
			portion = 1
		}
		runs := int((cand.Wanted + portion - 1) / portion)
		if runs < 1 {
			runs = 1
		}
		categoryID, batch := m.industryBatchCap(ctx, bpType.ProductTypeID, portion)
		runs = industrySizeRuns(runs, batch, industryStocksToBatch(categoryID), bpType.MaxProductionLimit)
		// The tail of the census is built to its headcount, not to a stocking
		// batch: a module sixteen pilots could fit is not stocked fifteen deep.
		runs = tailBatchRuns(runs, cand.Capable, cand.Wanted, portion)
		// Where the factory's region has a buying record, cap at a fortnight of it net of supply already there (mktdaily.go
		// historyCapRuns). A live ledger or bid want has its own count and keeps it.
		if cand.LedgerWanted == 0 && cand.BidWanted == 0 {
			runs = historyCapRuns(runs, cand.MktPerDay > 0, cand.MktPerDay, cand.MktSupply, portion)
		}
		// The blueprint's own run limit is the one hard law on a job's size, as for a player (ramsvc). Economics trims below
		// it: the want, the wallet, the materials it can feed, a copy's runs left.
		if bpType.MaxProductionLimit > 0 && int64(runs) > bpType.MaxProductionLimit {
			runs = int(bpType.MaxProductionLimit)
		}
		// Lines of its own already baking this make the next one a top-up, not a second lump (buildbook.go), unless those
		// lines are this batch fanned out: then the runs are what the split has left to place.
		split := st.Data.IndSplitBPTypeID == cand.BPTypeID && st.Data.IndSplitRunsLeft > 0
		if split {
			runs = st.Data.IndSplitRunsLeft
			if bpType.MaxProductionLimit > 0 && int64(runs) > bpType.MaxProductionLimit {
				runs = int(bpType.MaxProductionLimit)
			}
		} else {
			runs = industryTopUpRuns(runs, cand.Own)
		}
		// The whole batch, before a print or a lane cuts it: what is not put on this line is the split's to place next
		// (copylanes.go).
		total := runs

		// An owned print at the factory is free, else the market must sell one. A copy goes first (most runs left): the original
		// left free can make more copies (copylanes.go). A research career keeps the old order; its hangar copies are sale stock.
		copyOrder := "COALESCE(bp.copy, 0) DESC"
		if researchCareer(b.Tune) {
			copyOrder = "COALESCE(bp.copy, 0)"
		}
		var ownedBP int64
		_ = m.db.QueryRowContext(ctx, `
			SELECT e.itemID FROM entity e
			  LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
			WHERE e.ownerID = ? AND e.locationID = ? AND e.typeID = ? AND e.flag = 4
			  AND (COALESCE(bp.copy, 0) = 0 OR bp.licensedProductionRunsRemaining > 0)
			ORDER BY `+copyOrder+`, COALESCE(bp.licensedProductionRunsRemaining, -1) DESC
			LIMIT 1`, b.CharID, factoryID, cand.BPTypeID).Scan(&ownedBP)
		// Its original is on the copy line making this pilot's lanes: wait for
		// the copies rather than buy a second print.
		if ownedBP == 0 && copying[cand.BPTypeID] {
			skipped = append(skipped, cand.TypeName+": its print is on the copy line — the copies will build it")
			continue
		}
		prints := 0
		if ownedBP != 0 {
			prints = m.usablePrintsAt(ctx, b.CharID, factoryID, cand.BPTypeID)
		}
		var bpStop *marketStop
		bpoAsk := 0.0
		// A meta product is built off a print the pilot owns or not at all: named tech one, tech two and faction items have no
		// blueprint on any market (bpBuyableJoin). This catches ones that arrive some other way, before a region-wide scan.
		if ownedBP == 0 && m.metaGroupOf(ctx, cand.TypeID) != 0 {
			skipped = append(skipped, cand.TypeName+": a meta item is built only from a print it owns")
			continue
		}
		// Its own print in another hangar beats the market: collecting it is a trip and no ISK. The book only ever looked in
		// the factory's own hangar.
		var fetchID, escrowID int64
		if ownedBP == 0 {
			if id, stop := m.ownPrintElsewhere(ctx, b, cand.BPTypeID, factoryID, reachOfFactory()); id != 0 {
				fetchID, bpStop = id, stop
			}
		}
		// A copy on escrow is the cheap way in and is looked for first: no market scan and none of the wallet gate below,
		// which is sized to the whole original (bpcopy.go).
		var copyRuns int64
		if ownedBP == 0 && fetchID == 0 {
			if es := m.bestEscrowBPO(ctx, b, cand.BPTypeID, reachOfFactory(), nil, runs); es != nil && es.Copy {
				escrowID, copyRuns = es.EscrowID, es.Runs
				bpStop = &marketStop{TypeID: cand.BPTypeID, StationID: es.StationID,
					SystemID: es.SystemID, RegionID: es.RegionID, Price: es.Price, Jumps: es.Jumps}
				bpoAsk = es.Price
			}
		}
		// The split's next lane is a copy or its own print, never a second original bought off the market for the remainder of one batch.
		if split && ownedBP == 0 && fetchID == 0 && escrowID == 0 {
			industryClearSplit(&st.Data)
			skipped = append(skipped, cand.TypeName+": no further copy for the batch's next line")
			continue
		}
		// A print the wallet cannot fund is refused before the scan: the hunt reaches the whole map, and the wallet gate below
		// would refuse the same candidate after paying for it.
		if ownedBP == 0 && fetchID == 0 && escrowID == 0 {
			var bpBase sql.NullFloat64
			_ = m.db.QueryRowContext(ctx, `SELECT basePrice FROM invTypes WHERE typeID = ?`,
				cand.BPTypeID).Scan(&bpBase)
			if bpBase.Valid && bpBase.Float64 > 0 && balance/industryAffordMultiple < bpBase.Float64 {
				need := industryAffordMultiple * bpBase.Float64
				skipped = append(skipped, fmt.Sprintf("%s: needs ~%.0f ISK on hand", cand.TypeName, need))
				if cheapestNeed == 0 || need < cheapestNeed {
					cheapestNeed = need
				}
				continue
			}
		}
		if ownedBP == 0 && fetchID == 0 && escrowID == 0 {
			// The whole map, not one region: a blueprint is bought once and amortised over industryBPOAmortProjects projects, and the
			// everyday buy fence (shopLadder) leaves pilots in barren nullsec with no visible order. The climb still opens at the ordinary band.
			if _, err := m.shopLadder(ctx, b, []int64{cand.BPTypeID}, true, prefer,
				industryBPOBand, "", func(scan marketScan) bool {
					bpStop = bestBuy(scan.Stops, b.Tune)
					return bpStop != nil
				}); err != nil {
				return err
			}
			// Escrow across the factory's reach, against what the market offered: a researched print that costs more but saves more
			// per run wins, as does any print where the market had none (bpotrade.go).
			if es := m.bestEscrowBPO(ctx, b, cand.BPTypeID, reachOfFactory(), bpStop, runs); es != nil {
				escrowID = es.EscrowID
				if es.Copy {
					copyRuns = es.Runs
				}
				bpStop = &marketStop{TypeID: cand.BPTypeID, StationID: es.StationID,
					SystemID: es.SystemID, RegionID: es.RegionID, Price: es.Price, Jumps: es.Jumps}
			}
			if bpStop == nil {
				skipped = append(skipped, cand.TypeName+": no blueprint on the market")
				noteBootstrapNoPrint(cand.TypeID, clock.Unix())
				continue
			}
			bpoAsk = bpStop.Price
		}
		// A copy caps the batch at the runs it has left, or the core builds every run and deletes the copy
		// (installManufactureJob clamps too; the bill is sized here).
		switch {
		case ownedBP != 0:
			if left, isCopy := m.printRunsLeft(ctx, ownedBP); isCopy && int64(runs) > left {
				runs = int(left)
			}
		case fetchID != 0:
			if left, isCopy := m.printRunsLeft(ctx, fetchID); isCopy && int64(runs) > left {
				runs = int(left)
			}
		case copyRuns > 0 && int64(runs) > copyRuns:
			runs = int(copyRuns)
		}
		if runs < 1 {
			skipped = append(skipped, cand.TypeName+": its only print is a spent copy")
			continue
		}

		// Lanes: the batch spreads over the prints in hand (or the seller's extra copies) and the free lines; this project
		// takes one lane's share and the split carries the rest.
		lanePrints := max(prints, 1)
		var extras []escrowBPO
		if escrowID != 0 && copyRuns > 0 && bpStop != nil && total > 1 && freeSlots() > 1 {
			budget := (balance/industryAffordMultiple - bpoAsk) * industryExtraCopyBudgetFrac
			extras = m.extraCopyListings(ctx, b, cand.BPTypeID, bpStop.StationID, escrowID,
				min(freeSlots(), total)-1, budget)
			lanePrints = 1 + len(extras)
		}
		if chunk := splitChunk(total, lanePrints, freeSlots()); runs > chunk {
			runs = chunk
		}
		laneRuns := runs

		// Cash cost, per run so the wallet can size the batch: the blueprint, the line fees and the materials no asteroid yields
		// (a pilot that can mine pays for the rest in time; industryRefine digs it). All but the blueprint and install fee is linear in runs.
		perRun, err := m.industryMaterialNeeds(ctx, cand.BPTypeID, candME, 1, peSkill, line.MatMul)
		if err != nil {
			skipped = append(skipped, cand.TypeName+": "+err.Error())
			continue
		}
		// Can the bill even be assembled? Every line no rock yields must be on a counter this pilot can fly to, or already in
		// its hangar (industryOffBeltBlocker). A won Tech II patent meets this gate.
		if blocker := m.industryOffBeltBlocker(ctx, b, factoryID, perRun, reachOfFactory()); blocker != "" {
			skipped = append(skipped, fmt.Sprintf(
				"%s: needs %s, which no rock yields and nothing in reach sells", cand.TypeName, blocker))
			continue
		}
		// The batch is also capped by what the pilot can feed: the scarcest mineral it would have to dig decides the size
		// (industryFeedableRuns). A pilot that buys its bills is exempt; lines it cannot dig become mustBuy lines in the cash bill below.
		var mustBuy []matNeed
		if minesOwn {
			// A hull gets the campaign budget: industryFallBack flies up to industryMaxMiningTripsHard rather than abandon a bought
			// blueprint. Ammunition keeps the ordinary budget.
			trips := industryMaxMiningTrips
			if campaignHulls[cand.TypeID] {
				trips = industryMaxMiningTripsHard
			}
			fed, buy, blocker := m.industryFeedableRuns(ctx, b, factoryID, perRun, r, runs, trips)
			mustBuy = buy
			if fed < 1 && len(buy) == 0 {
				// Nothing to dig and nothing to buy: the bill is out of reach, not just off this pilot's belts.
				skipped = append(skipped, fmt.Sprintf(
					"%s: needs %s and nothing it would fly to yields any", cand.TypeName, blocker))
				continue
			}
			if fed < 1 {
				// Every minable line is a purchase, so digging no longer sizes the batch: the wallet does.
				fed = runs
			}
			if fed < runs {
				runs = fed
			}
		}

		cashLines := perRun
		if mineable, offBelt := m.splitMinableShortfall(ctx, perRun); minesOwn && len(mineable) > 0 {
			cashLines = append(append([]matNeed(nil), offBelt...), mustBuy...)
		}
		cashPerRun := 0.0
		for _, n := range cashLines {
			if p, perr := m.estimatePriceFor(ctx, b, n.TypeID); perr == nil {
				cashPerRun += p * float64(n.Needed)
			}
		}
		feePerRun := line.Info.CostPerHour * manufactureDurationSec(bpType, candPE, industrySkill, 1, line) / 3600
		// What the pilot must still hold when the job comes out of the oven, or the output cannot be listed (industryLiquidityFloor).
		reserve := industryLiquidityMin
		if each := cashPerRun + feePerRun; each > 0 {
			spend := balance/industryAffordMultiple - bpoAsk - line.Info.CostInstall
			if r := spend * industryLiquidityFloor; r > reserve {
				reserve = r
			}
			if afford := int((spend - reserve) / each); afford < runs {
				runs = afford
			}
		}
		if runs < 1 {
			need := industryAffordMultiple*(bpoAsk+line.Info.CostInstall+feePerRun+cashPerRun) + reserve
			skipped = append(skipped, fmt.Sprintf("%s: needs ~%.0f ISK on hand", cand.TypeName, need))
			// The one refusal here about the wallet rather than the world, kept as the cheapest: the least that gets this pilot
			// building again, which the gang whip-round and the poverty check want.
			if cheapestNeed == 0 || need < cheapestNeed {
				cheapestNeed = need
			}
			continue
		}
		// A hull is sized to what the pilot could finish buying, not what it might dig (the gate above prices minable lines at
		// zero). Smaller batches finish and put the print back in the draw. Never below one run: the project is already on.
		if categoryID == catShip && runs > 1 {
			price := make(map[int64]float64, len(perRun))
			onHand := make(map[int64]int64, len(perRun))
			for _, n := range perRun {
				if p, perr := m.estimatePriceFor(ctx, b, n.TypeID); perr == nil {
					price[n.TypeID] = p
				}
				if have, herr := evedb.GetCharMaterialQuantity(ctx, m.db, b.CharID, factoryID, n.TypeID); herr == nil {
					onHand[n.TypeID] = have
				}
			}
			spend := balance/industryAffordMultiple - bpoAsk - line.Info.CostInstall - reserve
			if fund := hullFundableRuns(perRun, price, onHand, feePerRun, spend, runs); fund < runs {
				m.logBot(b.CharID, logInfo, "industry",
					"sizing %s to %d run(s) it can pay for outright (was %d)", cand.TypeName, fund, runs)
				runs = fund
			}
		}
		// One original and free lines: copying it first may finish the batch sooner on several lines (copylanes.go). Only at
		// the factory, where the copy line is, and only for the runs the gates above left standing; with one print runs is the whole batch.
		if ownedBP != 0 && prints == 1 && !researchCareer(b.Tune) && b.StationID == factoryID {
			if note, ok := m.industrySelfCopy(ctx, b, factoryID, factorySys, cand, bpType, ownedBP, runs,
				candPE, industrySkill, skills[skillScience], line, freeSlots()); ok {
				skipped = append(skipped, cand.TypeName+": "+note)
				continue
			}
		}
		needs := make([]matNeed, len(perRun))
		for i, n := range perRun {
			needs[i] = matNeed{TypeID: n.TypeID, Needed: n.Needed * int64(runs)}
		}
		durationSec := manufactureDurationSec(bpType, candPE, industrySkill, runs, line)
		fees := line.Info.CostInstall + line.Info.CostPerHour*durationSec/3600

		// Would it sell for what it costs to make? A candidate with a live ledger want is exempt: a ledger row means nothing in
		// reach sells it, so there is no market price to compare. Everything else is traded somewhere, and building below cost bankrupts a factory.
		if cand.LedgerWanted == 0 {
			// Price what the pilot will actually pay, the same bill the affordability gate uses. A pilot that digs its own minerals pays in
			// time, not ISK; charging them here at market makes T1 manufacturing look like a loss-maker. Both gates must judge one cost.
			priced := needs
			if minesOwn {
				if mineable, offBelt := m.splitMinableShortfall(ctx, needs); len(mineable) > 0 &&
					m.oreWithinReach(ctx, mineable, r) {
					// The lines it cannot dig in its trip budget (mustBuy) are bought, and the wallet gate above charges them as cash: so
					// does this one, or a hull whose zydrine and megacyte are all purchased looks free to make.
					priced = append(append([]matNeed(nil), offBelt...), scaleNeeds(mustBuy, runs)...)
				}
			}
			matValue := 0.0
			for _, n := range priced {
				if p, perr := m.estimatePriceFor(ctx, b, n.TypeID); perr == nil {
					matValue += p * float64(n.Needed)
				}
			}
			units := int64(runs) * portion
			amort := m.bpoAmortProjectsFor(ctx, cand.TypeID)
			ask := industryUnitPrice(matValue, fees, copyAmortCost(bpoAsk, runs, copyRuns, amort), runs,
				amort, units, b.Tune.IndustryMargin)
			// Only against a real price: a product with no ask in reach and no basePrice comes back as the estimator's flat 100 ISK, which
			// would refuse every item costing more than that to make. Without a market the cost-plus listing (industrySell) is the price.
			if market, src, perr := m.estimatePriceSourceFor(ctx, b, cand.TypeID); perr == nil &&
				priceGateApplies(src) && market > 0 && ask > market {
				skipped = append(skipped, fmt.Sprintf(
					"%s: would cost %.2f/unit to make and sells for %.2f", cand.TypeName, ask, market))
				continue
			}
		}

		// Only a candidate with a live ledger want holds a claim: the shared row is what is claimed, whichever tier sourced the type
		// (buildbook.go).
		if cand.LedgerWanted > 0 {
			ok, err := m.claimDemand(ctx, b.CharID, cand.TypeID)
			if err != nil {
				return err
			}
			if !ok {
				continue // another supplier got there first
			}
		}

		if line := passOver.line(); line != "" {
			m.logBot(b.CharID, logInfo, "industry", "%s — building %s instead", line, cand.TypeName)
		}
		st.Data = idleData(st.Data)
		st.Data.IndDeadStops = nil // a new bill: every counter gets a fresh look
		st.Data.IndBuyLines = nil
		st.Data.IndTypeID = cand.TypeID
		st.Data.IndTypeName = cand.TypeName
		st.Data.IndBPTypeID = cand.BPTypeID
		st.Data.IndEvidence = cand.Evidence
		st.Data.IndFactoryID = factoryID
		st.Data.IndFactorySysID = factorySys
		st.Data.IndRuns = runs
		st.Data.IndStartedAt = clock.Unix()
		st.Data.IndExtraEscrowIDs = nil
		for _, x := range extras {
			st.Data.IndExtraEscrowIDs = append(st.Data.IndExtraEscrowIDs, x.EscrowID)
		}
		industryNoteSplit(&st.Data, cand.BPTypeID, total-laneRuns, freeSlots())
		// Money on the counter at commit, so a hauler can fly the minerals in while the pilot buys the blueprint. Bids use the
		// factory's reach, and supplyBidWalletFrac keeps escrow clear of the project's own blueprint money.
		if bidShort, serr := m.industryShortfall(ctx, b, needs); serr == nil && len(bidShort) > 0 {
			// Say what this project needs when it becomes a project, not only when it later gets stuck: the same bill known in full hours earlier
			// is a signal a career miner can act on. As at the refine seam, an unstuck project stops rewriting the row and it ages out.
			m.noteMaterialShortfall(ctx, b, factoryID, bidShort, reachOfFactory())
			m.maybeLeaveSupplyBids(ctx, b, line, needs, bidShort, reachOfFactory())
		}
		if ownedBP != 0 {
			st.Data.IndBPItemID = ownedBP
			m.logBot(b.CharID, logInfo, "industry",
				"building %d run(s) of %s at %s off a blueprint it already owns — %s",
				runs, cand.TypeName, m.stationLabel(ctx, factoryID), evidenceLabel(cand.Evidence))
			return m.beginTravel(ctx, b, factorySys, factoryID, string(TaskIndRefine))
		}
		st.Data.IndSellerStationID = bpStop.StationID
		st.Data.IndSellerSysID = bpStop.SystemID
		if escrowID != 0 {
			st.Data.IndEscrowID = escrowID
			m.logBot(b.CharID, logInfo, "industry",
				"building %d run(s) of %s (%d wanted — %s): claiming its blueprint off escrow for %.0f ISK at %s, %s, building at %s",
				runs, cand.TypeName, cand.Wanted, evidenceLabel(cand.Evidence), bpoAsk,
				m.stationLabel(ctx, bpStop.StationID),
				m.travelNote(ctx, b, bpStop.SystemID, bpStop.Jumps), m.stationLabel(ctx, factoryID))
			return m.beginTravel(ctx, b, bpStop.SystemID, bpStop.StationID, string(TaskIndBuyBPO))
		}
		if fetchID != 0 {
			st.Data.IndFetchItemID = fetchID
			m.logBot(b.CharID, logInfo, "industry",
				"building %d run(s) of %s (%d wanted — %s): collecting its own blueprint from %s, %s, building at %s",
				runs, cand.TypeName, cand.Wanted, evidenceLabel(cand.Evidence),
				m.stationLabel(ctx, bpStop.StationID),
				m.travelNote(ctx, b, bpStop.SystemID, bpStop.Jumps), m.stationLabel(ctx, factoryID))
			return m.beginTravel(ctx, b, bpStop.SystemID, bpStop.StationID, string(TaskIndBuyBPO))
		}
		m.logBot(b.CharID, logInfo, "industry",
			"building %d run(s) of %s (%d wanted — %s, %d rival seller(s) near the factory, %d other supplier(s) already on it): buying its blueprint at %.0f ISK, %s in %s, building at %s",
			runs, cand.TypeName, cand.Wanted, evidenceLabel(cand.Evidence), cand.Rivals, cand.Building, bpoAsk,
			m.stationLabel(ctx, bpStop.StationID),
			m.travelNote(ctx, b, bpStop.SystemID, bpStop.Jumps), m.stationLabel(ctx, factoryID))
		return m.beginTravel(ctx, b, bpStop.SystemID, bpStop.StationID, string(TaskIndBuyBPO))
	}
	// Every candidate refused. Nothing is broken and nothing changes in the next two minutes, so the activity goes on a cooldown and the
	// pilot returns to its appetites. Saved, because the idle error path and the boardStep path re-plan from the stored row.
	st.Data.IndBookFailAt = clock.Unix() + industryBookFailCooldownSec
	if err := saveState(ctx, m.db, st); err != nil {
		return err
	}
	if len(skipped) > 0 {
		// A refusal that is really "this pilot is broke" must say so in a form planSolo can test: the gang whip-round, brokeEarn and
		// refusalIsPassOver all gate on errors.Is(err, errCannotAfford). The prose still lists every reason.
		msg := fmt.Sprintf("nothing in its build book it can build: %s", strings.Join(skipped, "; "))
		if cheapestNeed > 0 {
			return &shortfallError{
				What: "anything in its build book", Need: cheapestNeed, Have: balance, Msg: msg,
			}
		}
		return errors.New(msg)
	}
	return errors.New("nothing in its build book needs building")
}

// industryBaseMaxJumps is how far a pilot keeps commuting to the factory it settled on before it looks for one where it actually
// is: a base is a habit, not an anchor, as a clone follows a pilot (clone.go).
const industryBaseMaxJumps = 20

// industryBaseMoveCooldownSec is how long a pilot keeps a factory it just moved to before it may move again. A re-pick is reached
// whenever manufactureLineAt refuses the incumbent, even for a full queue, so two stations in one system traded the pilot back and
// forth. Six hours outlasts any queue; the pilot still builds meanwhile, as this holds the base and not the pass.
const industryBaseMoveCooldownSec = 6 * 3600

// industrySchoolPenaltyJumps is what building in a chargen school costs a pilot picking a factory, in jumps. Twelve is further than a
// normal commute, so any working factory in ordinary space wins, and short of industryBaseMaxJumps, so a pilot with nothing else in
// reach can still build.
const industrySchoolPenaltyJumps = 12

// industryFactoryStation resolves where this pilot builds: its profile pin first, its own remembered base second, a fresh pick (the
// nearest reachable station with a manufacturing line whose owner will deal with it) last. The first two verify lines and standing
// and fall through with a log line when they fail. The remembered base is kept only within a sane number of jumps; past that the
// pilot resettles. There is no roster-wide override.
func (m *Mod) industryFactoryStation(ctx context.Context, b *botRun) (int64, int64, error) {
	return m.industryFactoryStationIn(ctx, b, rulesForBot(b))
}

// industryFactoryStationIn is the same pick under caller-supplied travel rules. The refining miner (ensureRefineryBase, mining.go)
// needs it because rulesForBot carries no residency band, unlike economicRules, so the fresh pick is the nearest usable factory in
// the whole reachable map and is written to Profile.IndustryStationID. A factory pilot still gets the unscoped rules.
func (m *Mod) industryFactoryStationIn(ctx context.Context, b *botRun,
	r travelRules) (int64, int64, error) {

	// incumbentHeld is set when the pilot's own base failed for a reason that will not last (a full queue, a database error). The
	// pilot still builds at whatever the fresh pick finds, but does not re-base on a transient refusal.
	incumbentHeld := false
	try := func(stationID int64, whence string) (int64, int64, bool) {
		if stationID == 0 {
			return 0, 0, false
		}
		if _, err := m.manufactureLineAt(ctx, b, stationID, 0); err != nil {
			if !isLineRefusal(err) && b.Profile != nil && stationID == b.Profile.IndustryStationID {
				incumbentHeld = true
				m.logBot(b.CharID, logInfo, "industry",
					"%s station %d cannot take the work right now (%v) — building elsewhere this pass, keeping the base",
					whence, stationID, err)
				return 0, 0, false
			}
			// A posted member's base is its department's office and a miner's is the mining office it refines at; neither has an assembly
			// line, so the pilot builds at the fresh pick below and the refusal is not logged.
			if !(isLineRefusal(err) && m.postingOwnsFactory(b)) {
				m.logBot(b.CharID, logInfo, "industry", "%s station %d unusable: %v", whence, stationID, err)
			}
			return 0, 0, false
		}
		loc, err := evedb.GetStationLocation(ctx, m.db, stationID)
		if err != nil {
			return 0, 0, false
		}
		return stationID, loc.SolarSystemID, true
	}
	// A pinned base beats everything and skips the reachability and industryBaseMaxJumps tests, which stop a pilot's own pick going
	// stale. The usability check still runs: a pinned station whose lines have gone cannot be built at.
	if b.Profile != nil && b.Profile.HomePinned {
		if st, sys, ok := try(b.Profile.IndustryStationID, "its pinned industry base"); ok {
			return st, sys, nil
		}
	}

	reach, err := m.reachFrom(ctx, b.SystemID, r)
	if err != nil {
		return 0, 0, err
	}
	sch := m.schoolsSoft(ctx)
	// A research career's base is its library: where originals live, are researched and copied, and where copies are sold, so it wants
	// copy lines (bpcopy.go). A base without them is kept unless the fresh pick finds a library in reach.
	library := researchCareer(b.Tune)
	var keepBase, keepBaseSys int64
	if st, sys, ok := try(b.Profile.IndustryStationID, "its industry base"); ok {
		jumps, reachable := reach[sys]
		switch {
		case sch.Stations[st]:
			// A base in the academy is a character-creation artefact (moveout.go pushes a pilot's home out of it too): the school penalty on a
			// new pick never runs on a base already settled there, so this forces a fresh look.
			m.logBot(b.CharID, logInfo, "industry",
				"its industry base at %s is the academy it was created in — looking for a factory of its own",
				m.stationLabel(ctx, st))
		case !reachable:
			m.logBot(b.CharID, logInfo, "industry",
				"its industry base at %s is not reachable under this pilot's travel rules from where it is now — looking for a factory it can actually get to",
				m.stationLabel(ctx, st))
		case jumps > industryBaseMaxJumps:
			m.logBot(b.CharID, logInfo, "industry",
				"its industry base at %s is %d jumps behind it now — looking for a factory nearer to where it works",
				m.stationLabel(ctx, st), jumps)
		case library && !m.labStationHasLine(ctx, st, ramActivityCopying):
			keepBase, keepBaseSys = st, sys
		default:
			return st, sys, nil
		}
	}

	// A fresh pick: stations with a manufacturing line, nearest reachable first, a chargen school priced industrySchoolPenaltyJumps
	// further (a penalty, not an exclusion). A research career looks for a library first. See docs/comment-notes-b12.md#factory-pick.
	needCopyPasses := []bool{false}
	if library {
		needCopyPasses = []bool{true, false}
	}
	for _, needCopy := range needCopyPasses {
		if !needCopy && keepBase != 0 {
			return keepBase, keepBaseSys, nil
		}
		copyClause := ""
		if needCopy {
			copyClause = ` AND EXISTS (SELECT 1 FROM ramassemblylines cl
			WHERE cl.containerID = al.containerID AND cl.activityID = ` + fmt.Sprint(ramActivityCopying) + `)`
		}
		for _, f := range stationFilterPasses(b.Profile, b.Tune) {
			lines, err := m.lineStations(ctx, 1, copyClause, f)
			if err != nil {
				return 0, 0, fmt.Errorf("factory scan: %w", err)
			}
			type fc struct {
				stationID, sysID int64
				jumps            int
				// rank is jumps as this pilot weighs them: trip cost plus school cost. Sorted on, never reported.
				rank int
				// incumbent breaks ties toward the base the pilot already has: two stations in one system tie on rank, and a re-pick would
				// otherwise hand the pilot to its neighbour and back.
				incumbent bool
			}
			var finalists []fc
			for _, ln := range lines {
				c := fc{stationID: ln.stationID, sysID: ln.sysID}
				j, ok := reach[c.sysID]
				if !ok || (needCopy && j > industryBaseMaxJumps) {
					continue
				}
				c.jumps, c.rank = j, j
				if sch.Stations[c.stationID] || sch.Systems[c.sysID] {
					c.rank += industrySchoolPenaltyJumps
				}
				c.incumbent = b.Profile != nil && c.stationID == b.Profile.IndustryStationID
				finalists = append(finalists, c)
			}
			// Nearest few get the expensive per-line checks (standing, queue).
			for tries := 0; tries < 5 && len(finalists) > 0; tries++ {
				best := 0
				for i := range finalists {
					switch {
					case finalists[i].rank < finalists[best].rank:
						best = i
					case finalists[i].rank == finalists[best].rank &&
						finalists[i].incumbent && !finalists[best].incumbent:
						best = i
					}
				}
				c := finalists[best]
				finalists = append(finalists[:best], finalists[best+1:]...)
				if _, err := m.manufactureLineAt(ctx, b, c.stationID, 0); err == nil {
					// Not while the pilot lives in the academy: a base remembered from there pulls the clone after it (industryAnchor, clone.go). It
					// builds here this pass; the base waits for an address of its own (moveout.go, corphome.go).
					if !incumbentHeld && !sch.Stations[m.homeStationFor(ctx, b.CharID)] {
						m.rememberIndustryBase(ctx, b, c.stationID)
					}
					return c.stationID, c.sysID, nil
				}
			}
		}
	}
	return 0, 0, fmt.Errorf("no reachable station with a manufacturing line will deal with this pilot")
}

// rememberIndustryBase writes the factory a pilot picked for itself back to its profile, as installing a clone moves a pilot's
// home (clone.go). An operator's pin still wins: this only fills a blank or replaces a base the pilot has moved away from because
// the old one stopped taking its work.
func (m *Mod) rememberIndustryBase(ctx context.Context, b *botRun, stationID int64) {
	if b.Profile == nil || stationID == 0 || b.Profile.IndustryStationID == stationID {
		return
	}
	// A member of a bot-run corporation works where it is posted (corpposting.go postedWorkStation) and only the posting pass writes
	// its factory; other writers still build at the station they found without making it the pilot's address.
	if m.postingOwnsFactory(b) {
		return
	}
	was := b.Profile.IndustryStationID
	// Settling for the first time is free. Moving is held off behind a cooldown (industryBaseMoveCooldownSec); the caller still builds
	// at the station it found, it just does not re-home.
	wasStamp := b.State.Data.IndBaseMovedAt
	if was != 0 {
		if since := clock.Unix() - wasStamp; since < industryBaseMoveCooldownSec {
			return
		}
		b.State.Data.IndBaseMovedAt = clock.Unix()
	}
	b.Profile.IndustryStationID = stationID
	if err := setIndustryBaseByID(ctx, m.db, b.CharID, stationID); err != nil {
		b.State.Data.IndBaseMovedAt = wasStamp // the move did not happen
		rlogf("rolebots: industry: remember base %d: %v", b.CharID, err)
		return
	}
	// The stamp is saved here with the write it belongs to: the caller returns a station and most paths never save, so an unsaved
	// cooldown never held. The profile write is durable, so the pair is saved together or not at all.
	if was != 0 {
		if err := saveState(ctx, m.db, b.State); err != nil {
			rlogf("rolebots: industry: stamp base move %d: %v", b.CharID, err)
		}
	}
	if was == 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"settling on %s as its industry base", m.stationLabel(ctx, stationID))
		return
	}
	m.logBot(b.CharID, logInfo, "industry",
		"moving its industry base from %s to %s", m.stationLabel(ctx, was), m.stationLabel(ctx, stationID))
}

// setIndustryBaseByID is the row write behind rememberIndustryBase, reachable without a botRun: the corporation page's address
// buttons move the base of pilots nobody is simulating. An upsert, because a pilot whose profile was never saved has no row to update.
func setIndustryBaseByID(ctx context.Context, db *sql.DB, charID, stationID int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_profiles (characterID, industryStationID) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE industryStationID = VALUES(industryStationID)`, charID, stationID)
	if err != nil {
		return fmt.Errorf("setIndustryBaseByID: %w", err)
	}
	return nil
}

// matNeed is one material line of the job, waste included, at this pilot's
// actual Production Efficiency.
type matNeed struct {
	TypeID int64
	Needed int64
}

// industryMaterialNeeds is the real bill of materials for the job: the same CalcMaterialsNeeded/ApplyMaterialMultiplier arithmetic
// ConsumeMaterials runs, so the shortfall check and the install cannot disagree. meLevel is the blueprint's own material level
// (labwork.go researches it), since the install takes the saving and planning must too.
func (m *Mod) industryMaterialNeeds(ctx context.Context, bpTypeID int64, meLevel int64, runs, peSkill int, matMul float64) ([]matNeed, error) {
	bpType, err := evedb.GetBlueprintTypeInfo(ctx, m.db, bpTypeID)
	if err != nil {
		return nil, err
	}
	reqs, err := evedb.GetRequiredMaterials(ctx, m.db, bpTypeID, 1)
	if err != nil {
		return nil, err
	}
	var out []matNeed
	for _, r := range reqs {
		if r.DamagePerJob < 1 {
			continue // present-but-not-eaten lines are not shopped for
		}
		perRun := evedb.ApplyMaterialMultiplier(
			evedb.CalcMaterialsNeeded(r.Quantity, bpType.WasteFactor, meLevel, peSkill), matMul)
		out = append(out, matNeed{TypeID: r.TypeID, Needed: perRun * int64(runs)})
	}
	return out, nil
}

// blueprintLevels reads one blueprint item's researched ME and PE. Zero for a print with no invBlueprints row, which is how an
// unresearched original looks (labwork.go raises the levels).
func (m *Mod) blueprintLevels(ctx context.Context, bpItemID int64) (meLevel, peLevel int64) {
	if bpItemID == 0 {
		return 0, 0
	}
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(materialLevel, 0), COALESCE(productivityLevel, 0)
		  FROM invBlueprints WHERE blueprintID = ?`, bpItemID).Scan(&meLevel, &peLevel)
	return meLevel, peLevel
}

// printRunsLeft reports whether a print is a COPY and, if so, how many runs it
// has left. An original (or a print with no blueprint row) is not a copy.
func (m *Mod) printRunsLeft(ctx context.Context, bpItemID int64) (runs int64, isCopy bool) {
	var cp int
	if err := m.db.QueryRowContext(ctx, `
		SELECT copy, licensedProductionRunsRemaining FROM invBlueprints WHERE blueprintID = ?`,
		bpItemID).Scan(&cp, &runs); err != nil || cp == 0 {
		return 0, false
	}
	if runs < 0 {
		runs = 0
	}
	return runs, true
}

// copyAmortCost converts what a print cost into the figure industryUnitPrice amortises over amortProjects (bpoAmortProjects). An
// original is capital spread over many projects; a copy is consumed, so the runs this project uses carry their share in full. copyRuns
// 0 means an original. Pure.
func copyAmortCost(paid float64, runs int, copyRuns int64, amortProjects int) float64 {
	if copyRuns <= 0 || paid <= 0 {
		return paid
	}
	if amortProjects <= 0 {
		amortProjects = industryBPOAmortProjects
	}
	used := float64(runs)
	if used > float64(copyRuns) {
		used = float64(copyRuns)
	}
	return paid * used / float64(copyRuns) * float64(amortProjects)
}

// scaleNeeds is per-run material lines times runs. Pure.
func scaleNeeds(perRun []matNeed, runs int) []matNeed {
	out := make([]matNeed, len(perRun))
	for i, n := range perRun {
		out[i] = matNeed{TypeID: n.TypeID, Needed: n.Needed * int64(runs)}
	}
	return out
}

// ownedBlueprintLevels is the same question asked once for a whole candidate list: the best ME and PE this pilot owns for each
// blueprint type in its factory hangar. One query, not one per candidate, because industryStartProject prices hundreds of candidates
// a pass. The best is quoted, optimistic by at most the spread between copies of a print.
func (m *Mod) ownedBlueprintLevels(ctx context.Context, charID, stationID int64) (map[int64][2]int64, error) {
	out := map[int64][2]int64{}
	if charID == 0 || stationID == 0 {
		return out, nil
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, MAX(COALESCE(bp.materialLevel, 0)), MAX(COALESCE(bp.productivityLevel, 0))
		  FROM entity e
		  LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4
		 GROUP BY e.typeID`, charID, stationID)
	if err != nil {
		return out, fmt.Errorf("owned blueprint levels: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, me, pe int64
		if err := rows.Scan(&typeID, &me, &pe); err != nil {
			return out, err
		}
		out[typeID] = [2]int64{me, pe}
	}
	return out, rows.Err()
}

// industryBatchCap is how many runs of one product a single job may be, and the whole of the "build ammunition like ammunition"
// rule. The product's category and portion size decide, not a group list: a bulk round (portion 10, 100 or 5000) is burned and batched
// in rounds; a one-per-run charge (crystal, probe, script) wears slowly, sells singly and stays near the durable cap.
func (m *Mod) industryBatchCap(ctx context.Context, productTypeID, portion int64) (categoryID int64, runs int) {
	if err := m.db.QueryRowContext(ctx, `
		SELECT g.categoryID FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeID = ?`, productTypeID).Scan(&categoryID); err != nil {
		return 0, industryMaxRunsPerJob
	}
	return categoryID, industryBatchRuns(categoryID, portion)
}

// industryStocksToBatch says whether the batch is a floor as well as a ceiling: whether a want smaller than the batch is rounded
// up to it. True for everything sold off a shelf a few at a time (ammunition, modules, drones), since a factory answering a ledger
// row for three with exactly three has paid for a blueprint, materials and an install to put nothing on the shelf. Hulls stay want-sized.
func industryStocksToBatch(categoryID int64) bool {
	switch categoryID {
	case catCharge, catModule, catDrone:
		return true
	}
	return false
}

// dropUnaffordablePrints removes from the book every candidate whose print the pilot owns nowhere, has no claimable copy of on escrow
// in reach (hullGateCopyAffordsKey), and whose catalogue price exceeds what the wallet may commit (balance / industryAffordMultiple),
// before the draw so attempts go to candidates that can start. It returns the kept book, the ISK each dropped print needs, the
// blueprint typeIDs the pilot owns (for pickOwnedHullPin) and the dropped candidates, parallel to the needs. A read failure keeps the
// whole book.
func (m *Mod) dropUnaffordablePrints(ctx context.Context, b *botRun, cands []indCandidate, balance float64,
	reach func() map[int64]int) ([]indCandidate, []float64, map[int64]bool, []indCandidate) {
	if len(cands) == 0 {
		return cands, nil, nil, nil
	}
	charID := b.CharID
	ids := make([]int64, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.BPTypeID)
	}
	base := map[int64]float64{}
	rows, err := m.db.QueryContext(ctx, `
		SELECT typeID, COALESCE(basePrice, 0) FROM invTypes
		WHERE typeID IN (`+placeholders(len(ids))+`)`, int64Args(ids)...)
	if err != nil {
		return cands, nil, nil, nil
	}
	for rows.Next() {
		var id int64
		var p float64
		if rows.Scan(&id, &p) == nil {
			base[id] = p
		}
	}
	rows.Close()
	// A copy on escrow is the cheap way in (bpcopy.go): the per-candidate loop looks for one before its own wallet gate, so this
	// filter must not throw the candidate away first. Only asked when some print is beyond the wallet.
	var copyable map[int64]bool
	if hullGateOn(ctx, m, hullGateCopyAffordsKey) && escrowBuyingEnabled(ctx, m.db) {
		budget := balance / industryAffordMultiple
		beyond := false
		for _, c := range cands {
			if p := base[c.BPTypeID]; p > 0 && budget < p {
				beyond = true
				break
			}
		}
		if beyond {
			if all, lerr := m.escrowBPOListings(ctx); lerr == nil {
				copyable = copyableEscrowTypes(all, reach(), charID, spendable(balance))
			}
		}
	}
	owned := map[int64]bool{}
	args := append([]any{charID}, int64Args(ids)...)
	orows, err := m.db.QueryContext(ctx, `
		SELECT DISTINCT typeID FROM entity
		WHERE ownerID = ? AND typeID IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return cands, nil, nil, nil
	}
	for orows.Next() {
		var id int64
		if orows.Scan(&id) == nil {
			owned[id] = true
		}
	}
	orows.Close()
	kept, needs, dropped := filterAffordablePrints(cands, base, owned, copyable, balance)
	return kept, needs, owned, dropped
}

// filterAffordablePrints is the arithmetic half of dropUnaffordablePrints, pure so the harness can pin it. A print the pilot owns, or
// one with a claimable copy on escrow (copyable, may be nil), is kept whatever the original costs. dropped is parallel to needs.
func filterAffordablePrints(cands []indCandidate, base map[int64]float64, owned, copyable map[int64]bool,
	balance float64) ([]indCandidate, []float64, []indCandidate) {
	budget := balance / industryAffordMultiple
	kept := cands[:0:0]
	var needs []float64
	var dropped []indCandidate
	for _, c := range cands {
		p := base[c.BPTypeID]
		if !owned[c.BPTypeID] && !copyable[c.BPTypeID] && p > 0 && budget < p {
			needs = append(needs, industryAffordMultiple*p)
			dropped = append(dropped, c)
			continue
		}
		kept = append(kept, c)
	}
	return kept, needs, dropped
}

// industrySizeRuns turns the want (in runs) into the job's run count. The ceiling is the want and the print's own run limit, the law a
// player is held to; the wallet, tail, top-up, copy runs left and feedable-materials caps trim it further. For stock (ammunition, modules,
// drones) the category batch is the floor, never above the print's limit. Hulls are want-sized. Pure.
func industrySizeRuns(wantRuns, batch int, stock bool, maxLimit int64) int {
	if wantRuns < 1 {
		wantRuns = 1
	}
	ceil := wantRuns
	if stock && ceil < batch {
		ceil = batch
	}
	if maxLimit > 0 && int64(ceil) > maxLimit {
		ceil = int(maxLimit)
	}
	return ceil
}

// hullFundableRuns is the largest batch, at most maxRuns, whose whole bill the pilot could buy outright: every per-run line not already
// in the factory hangar, at price, plus the line fee per run, within spend. Never below one: it trims a project the gates accepted.
// A line with no price counts as free. Pure.
func hullFundableRuns(perRun []matNeed, price map[int64]float64, onHand map[int64]int64,
	feePerRun, spend float64, maxRuns int) int {
	cost := func(runs int) float64 {
		total := feePerRun * float64(runs)
		for _, n := range perRun {
			if short := n.Needed*int64(runs) - onHand[n.TypeID]; short > 0 {
				total += float64(short) * price[n.TypeID]
			}
		}
		return total
	}
	best := 1
	for r := 2; r <= maxRuns; r++ {
		if cost(r) > spend {
			break
		}
		best = r
	}
	return best
}

// industryBatchRuns is the arithmetic half of it, pure so the harness can
// pin the sizes.
func industryBatchRuns(categoryID, portion int64) int {
	if portion < 1 {
		portion = 1
	}
	switch categoryID {
	case catCharge:
		// A singleton charge is a crystal; anything packed in bulk is
		// ammunition and is measured in rounds.
		if portion <= 1 {
			return industryCrystalMaxRuns
		}
		return clampRuns(int(industryAmmoBatchRounds/portion), industryAmmoMaxRuns)
	case catShip:
		return industryHullMaxRuns
	case catModule, catDrone:
		return clampRuns(int((industryDurableBatchUnits+portion-1)/portion), industryDurableBatchUnits)
	}
	return industryMaxRunsPerJob
}

func clampRuns(runs, max int) int {
	if runs < 1 {
		return 1
	}
	if runs > max {
		return max
	}
	return runs
}

// industryShortfall is what the factory hangar still lacks of the bill.
func (m *Mod) industryShortfall(ctx context.Context, b *botRun, needs []matNeed) ([]matNeed, error) {
	var short []matNeed
	for _, n := range needs {
		have, err := evedb.GetCharMaterialQuantity(ctx, m.db, b.CharID, b.State.Data.IndFactoryID, n.TypeID)
		if err != nil {
			return nil, err
		}
		if have < n.Needed {
			short = append(short, matNeed{TypeID: n.TypeID, Needed: n.Needed - have})
		}
	}
	return short, nil
}

// industryBookAttempts is how many candidates one pass offers to the gate, drawn from the whole scored book so every row keeps a
// chance proportional to its score.
const industryBookAttempts = 20

// industryTailCapable is the headcount under which a candidate is sized to
// the roster that could use it rather than to a stocking batch.
const industryTailCapable = 50

// tailBatchRuns caps a tail candidate's runs at half its want (at least one
// run): enough that the pilots who could use it find one, not a shelf nobody
// will empty. Pure, so the harness pins it.
func tailBatchRuns(runs int, capable, wanted, portion int64) int {
	if capable <= 0 || capable >= industryTailCapable {
		return runs
	}
	if portion < 1 {
		portion = 1
	}
	units := wanted / 2
	if units < portion {
		units = portion
	}
	maxRuns := int((units + portion - 1) / portion)
	if maxRuns < 1 {
		maxRuns = 1
	}
	if runs > maxRuns {
		return maxRuns
	}
	return runs
}

// ownPrintElsewhere finds the pilot's own print of a type in a station hangar other than the factory, nearest first by the factory's
// horizon (the trip is pilot, print, factory). A print on a lab errand (LabBPItemID) is the lab lane's and is left alone. Returns the
// print and a price-0 marketStop to route to, or 0.
func (m *Mod) ownPrintElsewhere(ctx context.Context, b *botRun, bpTypeID, factoryID int64,
	reach map[int64]int) (int64, *marketStop) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID, e.locationID, st.solarSystemID
		FROM entity e
		JOIN staStations st ON st.stationID = e.locationID
		LEFT JOIN invBlueprints bp ON bp.blueprintID = e.itemID
		WHERE e.ownerID = ? AND e.typeID = ? AND e.flag = 4
		  AND (COALESCE(bp.copy, 0) = 0 OR bp.licensedProductionRunsRemaining > 0)
		  AND e.locationID <> ? AND e.itemID <> ?`,
		b.CharID, bpTypeID, factoryID, b.State.Data.LabBPItemID)
	if err != nil {
		return 0, nil
	}
	defer rows.Close()
	var best int64
	var stop *marketStop
	for rows.Next() {
		var itemID, stationID, sysID int64
		if rows.Scan(&itemID, &stationID, &sysID) != nil {
			continue
		}
		j, ok := reach[sysID]
		if !ok || j > industryBaseMaxJumps {
			continue
		}
		if stop == nil || j < stop.Jumps {
			best = itemID
			stop = &marketStop{TypeID: bpTypeID, StationID: stationID, SystemID: sysID, Jumps: j}
		}
	}
	return best, stop
}

// priceGateApplies says whether a product valuation is real enough to refuse
// a build against. A live ask or a catalogue basePrice is; the estimator's
// 100 ISK guess for a type with neither is not. Pure, so the harness pins it.
func priceGateApplies(src priceSource) bool {
	return src == priceLive || src == priceCatalogue
}

// industryUnitPrice is the cost-plus sale price per unit: materials at market value, the line fees and one project's share of the
// blueprint, times the pilot's margin. runs only gates the zero cases; it does not scale the blueprint term (industryBPOAmortProjects). Pure.
func industryUnitPrice(matValue, fees, bpoCost float64, runs, amortProjects int, units int64, margin float64) float64 {
	if units <= 0 || runs <= 0 || amortProjects <= 0 {
		return 0
	}
	unitCost := (matValue + fees + bpoCost/float64(amortProjects)) / float64(units)
	price := math.Round(unitCost*margin*100) / 100
	if price < 0.01 {
		price = 0.01
	}
	return price
}

// industryCostBasis is what a finished batch cost, for booking its sales: the project's cash with the print swapped for the share
// industryUnitPrice charges (bpoCost / amortProjects: a tenth of an original, the consumed runs of a copy). bpoPaid 0 means no print was
// bought, so the cash is the basis. Pure.
func industryCostBasis(spent, bpoPaid, bpoCost float64, amortProjects int) float64 {
	if bpoPaid <= 0 || amortProjects <= 0 {
		return spent
	}
	basis := spent - bpoPaid + bpoCost/float64(amortProjects)
	if basis < 0 {
		return 0
	}
	return basis
}

// ── the oven ────────────────────────────────────────────────────────────────

// indOven is what an installed job is doing right now. The ESP factory pages can deliver or abort a job from outside, so "gone" has
// three endings: delivered (product in the hangar, still worth selling), aborted (nothing produced, claim handed back) and vanished.
type indOven int

const (
	indOvenNone      indOven = iota // no job installed
	indOvenVanished                 // the ramJobs row is gone
	indOvenCooking                  // still running
	indOvenReady                    // finished, waiting to be collected
	indOvenDelivered                // somebody else collected it
	indOvenAborted                  // cancelled from outside
)

// industryOven reads the installed job's state and how long it has left.
func (m *Mod) industryOven(ctx context.Context, b *botRun) (indOven, int64) {
	d := &b.State.Data
	if d.IndJobID == 0 {
		return indOvenNone, 0
	}
	status, ready, endTime, err := m.industryJobStatus(ctx, d.IndJobID)
	if err != nil {
		return indOvenVanished, 0
	}
	switch {
	case status == 101:
		return indOvenDelivered, 0
	case status >= 100:
		return indOvenAborted, 0
	case ready:
		return indOvenReady, 0
	}
	left := evedb.Win32ToUnix(endTime) - clock.Unix()
	if left < 0 {
		left = 0
	}
	return indOvenCooking, left
}

// industryOvenSweep takes a rung oven off the line wherever the pilot is, on every sim pass. Everything else for a finished job hangs
// off plan(), which runs only from idle, so a long trip left a rung job undelivered with its line busy and its blueprint at flag 111.
// Delivery does not need the pilot (CompleteJob puts the output in the line's station hangar); only the listing does, and industryHarvest
// routes that. Cost is one indexed read for a pilot with a line running.
func (m *Mod) industryOvenSweep(ctx context.Context, b *botRun) {
	m.industryOvenSweepBefore(ctx, b, 0)
}

// industryOvenSweepBefore is industryOvenSweep limited to jobs that finished at or before the Win32 time cut; 0 means any
// finished job. The off-hours claim pass (tzclaim.go) passes a cut in the past. Returns how many jobs it took off the line.
func (m *Mod) industryOvenSweepBefore(ctx context.Context, b *botRun, cut int64) int {
	st := b.State
	if st == nil {
		return 0
	}
	d := &st.Data
	ids := indOvenIDs(d)
	if len(ids) == 0 {
		return 0
	}
	// Nothing on the list can have rung since the last read (ovengate.go).
	if cut == 0 && ovenGateShut(ovenKindIndustry, b.CharID, ids, clock.Unix()) {
		return 0
	}
	// One query for the whole list. A job the 30s server ticker has not flagged counts as rung on its own end time, as CompleteJob
	// does (ovenRung); the earliest end time still to come is what the gate waits for.
	place := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		place[i] = "?"
		args = append(args, id)
	}
	readyCut, dueCut := int64(math.MaxInt64), win32Now()
	if cut > 0 {
		readyCut, dueCut = cut, cut
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT jobID, completedStatusID, COALESCE(endProductionTime, 0) FROM ramJobs
		 WHERE jobID IN (`+strings.Join(place, ",")+`)`, args...)
	if err != nil {
		return 0
	}
	rung := make(map[int64]int64, len(ids))
	var nextEnd int64
	scanned := true
	for rows.Next() {
		var jobID, status, end int64
		if err := rows.Scan(&jobID, &status, &end); err != nil {
			scanned = false
			break
		}
		if ovenRung(status, end, readyCut, dueCut) {
			rung[jobID] = status
		} else if end > 0 && (nextEnd == 0 || end < nextEnd) {
			nextEnd = end
		}
	}
	if rows.Err() != nil {
		scanned = false
	}
	rows.Close()
	if len(rung) == 0 {
		if cut == 0 && scanned {
			ovenGateNote(ovenKindIndustry, b.CharID, ids, nextEnd, clock.Unix())
		}
		return 0
	}

	changed, delivered := false, 0
	for i := range d.IndCooking {
		j := &d.IndCooking[i]
		status, ok := rung[j.JobID]
		if !ok || j.Delivered {
			continue
		}
		switch {
		case status == 101:
			// Somebody else (the ESP factory page) took it off the line; the goods are in the hangar either way.
			j.Delivered, changed = true, true
		case status >= 100:
			// Cancelled. industryHarvest closes the entry and hands the
			// demand claim back; nothing to deliver.
		default:
			if m.industryDeliverRemote(ctx, b, *j) {
				j.Delivered, changed = true, true
				delivered++
			}
		}
	}
	if changed {
		if err := saveState(ctx, m.db, st); err != nil {
			rlogf("rolebots: oven sweep save for %d: %v", b.CharID, err)
		}
	}
	return delivered
}

// industryDeliverRemote is the paperwork half of collecting: blueprint back to the hangar, output into the factory's hangar, line
// released. Reports whether the job is off the line. The pilot's location is not consulted; "already delivered" counts as done and
// "still running" is the status ticker racing the end time, settled next pass.
func (m *Mod) industryDeliverRemote(ctx context.Context, b *botRun, j IndCookingJob) bool {
	outs, err := evedb.CompleteJob(ctx, m.db, j.JobID, b.CharID, false)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "already delivered"):
			return true
		case strings.Contains(err.Error(), "still running"):
			return false
		}
		m.logBot(b.CharID, logError, "industry", "taking job %d off the line: %v", j.JobID, err)
		return false
	}
	// The output lands next to whatever the last project left there, so it
	// is tidied at the factory rather than wherever the pilot is standing.
	m.stackHangar(ctx, b.CharID, j.FactoryID)
	m.logBot(b.CharID, logInfo, "industry",
		"job %d rang — %d stack(s) of %s off the line and into its hangar at the factory, to be listed next time it is free",
		j.JobID, len(outs), j.TypeName)
	return true
}

// industryCollectDue is the errand a finished job becomes: fly to the factory and deliver it. It runs before gang duty (sim.go plan),
// not only from the solo planner, or a gang op would leave the blueprint at flag 111 and the demand claim going stale. Returns
// handled=true when it has routed the bot.
func (m *Mod) industryCollectDue(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	// Jobs the pilot walked away from live on the cooking list and are harvested first, where every finished job arrives.
	if handled, err := m.industryHarvest(ctx, b); err != nil || handled {
		return handled, err
	}
	if d.IndTypeID == 0 || d.IndJobID == 0 {
		return false, nil
	}
	// This is the one industry hook a gang-claimed pilot still reaches, so
	// it is also the only thing keeping the demand claim warm through an op.
	m.touchClaim(ctx, b.CharID, d.IndTypeID)
	oven, _ := m.industryOven(ctx, b)
	switch oven {
	case indOvenVanished:
		m.logBot(b.CharID, logError, "industry",
			"job %d is no longer on any line — dropping the project", d.IndJobID)
		m.industryClearProject(ctx, b, true)
		return false, saveState(ctx, m.db, st)
	case indOvenAborted:
		m.logBot(b.CharID, logInfo, "industry",
			"job %d was cancelled from outside — closing the project", d.IndJobID)
		m.industryClearProject(ctx, b, true)
		return false, saveState(ctx, m.db, st)
	case indOvenDelivered:
		// Someone else (the ESP factory page) took it off the line. The product is in the factory hangar, so the listing is still worth
		// flying back for.
		m.logBot(b.CharID, logInfo, "industry",
			"job %d was delivered from outside — going to list the %s anyway", d.IndJobID, d.IndTypeName)
		d.IndJobID, d.IndWaiting = 0, false
		st.Activity = ActIndustry
		return true, m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndSell))
	case indOvenReady:
		d.IndWaiting = false
		st.Activity = ActIndustry
		return true, m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndCollect))
	}
	return false, nil
}

// industryWaitOutOven parks a pilot for the last minutes of a bake instead
// of letting it start work it cannot finish in time. Everything longer than
// industryOvenWatchSec is real time and the pilot spends it elsewhere; this
// is only the tail.
func (m *Mod) industryWaitOutOven(ctx context.Context, b *botRun, left int64) error {
	st := b.State
	d := &st.Data
	announce := !d.IndWaiting
	d.IndWaiting = true

	if b.StationID != d.IndFactoryID {
		if announce {
			m.logBot(b.CharID, logInfo, "industry",
				"its %s job rings in %s — heading back to the factory rather than starting something it cannot finish",
				d.IndTypeName, agoString(left))
		}
		st.Activity = ActIndustry
		return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIdle))
	}
	if announce {
		m.logBot(b.CharID, logInfo, "industry",
			"waiting out the last %s of its %s job at the factory", agoString(left), d.IndTypeName)
	}
	st.Activity, st.Phase = "", string(TaskIdle)
	wait := left + 1
	if wait > industryOvenPollSec {
		wait = industryOvenPollSec
	}
	st.Data.LegEndsAt = clock.Unix() + wait
	return saveState(ctx, m.db, st)
}

// ── resume ──────────────────────────────────────────────────────────────────

// industryResume is planSolo's deterministic hook: an in-flight project is sunk capital and picks itself back up before the appetite
// draw. Returns handled=true when it has routed the bot. A job with real time left returns false; the pilot is free until the oven is
// nearly done.
func (m *Mod) industryResume(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data

	// Anything a line has finished with comes first: goods sitting on someone else's assembly line are the most idle capital in the module.
	if handled, err := m.industryHarvest(ctx, b); err != nil || handled {
		return handled, err
	}

	if d.IndTypeID == 0 {
		return m.industryIdleWithOvens(ctx, b)
	}
	m.touchClaim(ctx, b.CharID, d.IndTypeID)

	if d.IndJobID != 0 {
		handled, err := m.industryCollectDue(ctx, b)
		if err != nil || handled {
			return handled, err
		}
		if d.IndJobID == 0 {
			return false, nil // the project ended on the line
		}
		_, left := m.industryOven(ctx, b)
		if left > industryOvenWatchSec {
			// Free to do other work while it cooks.
			if d.IndWaiting {
				d.IndWaiting = false
				return false, saveState(ctx, m.db, st)
			}
			return false, nil
		}
		return true, m.industryWaitOutOven(ctx, b, left)
	}

	// No job yet. A project that has not reached its install inside a day is stuck (a vanished seller, a market that will not sell
	// minerals) and the claim must not pin the type. Idle with no job is the pre-install stretch, so refine stands in for it.
	if industryStalled(d, string(TaskIndRefine), clock.Unix()) {
		m.logBot(b.CharID, logInfo, "industry",
			"project for %s made no progress in a day — abandoning it", d.IndTypeName)
		m.industryClearProject(ctx, b, true)
		return false, saveState(ctx, m.db, st)
	}
	// A bought blueprint that no longer exists (it rode a ship that died)
	// ends the project too.
	if d.IndBPItemID != 0 {
		var one int64
		if err := m.db.QueryRowContext(ctx,
			`SELECT 1 FROM entity WHERE itemID = ?`, d.IndBPItemID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			m.logBot(b.CharID, logInfo, "industry",
				"its %s blueprint went down with a ship — abandoning the project", d.IndTypeName)
			m.industryClearProject(ctx, b, true)
			return false, saveState(ctx, m.db, st)
		}
	}
	st.Activity = ActIndustry
	if err := m.industryAdvance(ctx, b); err != nil {
		// Not fatal: log it, let the pilot do something else; the stall
		// clock ends a project that keeps failing to advance.
		m.logBot(b.CharID, logError, "industry", "resume: %v", err)
		return false, nil
	}
	return true, nil
}

// industryIdleWithOvens is what a pilot with jobs cooking and no project in hand does, in order: with a free line it returns false
// so the appetite draw takes it back to the factory; with no free line and the soonest job ringing inside the watch window it goes and
// stands by it; otherwise it is free for other work.
func (m *Mod) industryIdleWithOvens(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	if len(d.IndCooking) == 0 {
		if d.IndWaiting {
			d.IndWaiting = false
			return false, saveState(ctx, m.db, st)
		}
		return false, nil
	}
	if free, err := m.industryFreeSlots(ctx, b); err == nil && free > 0 {
		if d.IndWaiting {
			d.IndWaiting = false
			return false, saveState(ctx, m.db, st)
		}
		return false, nil
	}
	left, stationID, sysID, ok := m.industrySoonestOven(ctx, b)
	if !ok || left > industryOvenWatchSec {
		if d.IndWaiting {
			d.IndWaiting = false
			return false, saveState(ctx, m.db, st)
		}
		return false, nil
	}
	announce := !d.IndWaiting
	d.IndWaiting = true
	if b.StationID != stationID {
		if announce {
			m.logBot(b.CharID, logInfo, "industry",
				"every line it has is running and the first rings in %s — heading back to the factory",
				agoString(left))
		}
		st.Activity = ActIndustry
		return true, m.beginTravel(ctx, b, sysID, stationID, string(TaskIdle))
	}
	if announce {
		m.logBot(b.CharID, logInfo, "industry",
			"waiting out the last %s at the factory — every line is running", agoString(left))
	}
	st.Activity, st.Phase = "", string(TaskIdle)
	wait := left + 1
	if wait > industryOvenPollSec {
		wait = industryOvenPollSec
	}
	st.Data.LegEndsAt = clock.Unix() + wait
	return true, saveState(ctx, m.db, st)
}

// industryAdvance routes an active project to its next phase.
func (m *Mod) industryAdvance(ctx context.Context, b *botRun) error {
	d := &b.State.Data
	switch {
	case d.IndCorpMove != 0:
		// A factory move knows from its own row which leg it is on.
		b.State.Phase = string(TaskIndCorpMove)
		return m.corpMoveStep(ctx, b)
	case d.IndPrintMove != 0:
		b.State.Phase = string(TaskIndPrintMove)
		return m.printMoveStep(ctx, b)
	case d.IndCorpWork != 0:
		// A corporation job resumes at its own counter, never at the personal refine hub, which would treat the corporation's print as the
		// pilot's own project (corpjobs.go).
		return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndCorpJob))
	case d.IndJobID != 0:
		_, ready, _, err := m.industryJobStatus(ctx, d.IndJobID)
		if err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("its manufacturing job is still cooking")
		}
		return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndCollect))
	case d.IndBPItemID != 0:
		return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndRefine))
	default:
		return m.beginTravel(ctx, b, d.IndSellerSysID, d.IndSellerStationID, string(TaskIndBuyBPO))
	}
}

// industryStalled is the stall clock: a project still short of its install after industryStallSec is stuck (a vanished seller, a market
// that will not sell minerals) and the claim must not pin the type. Only pre-install phases count, since IndStartedAt is never refreshed
// and a project reaching sell late is not stuck. Pure; read on every industry tick (industryStep) and from idle (industryResume).
func industryStalled(d *StateData, phase string, now int64) bool {
	if d.IndStartedAt <= 0 || d.IndJobID != 0 {
		return false
	}
	switch phase {
	case string(TaskIndBuyBPO), string(TaskIndRefine), string(TaskIndBuyMats), string(TaskIndInstall):
	default:
		return false
	}
	// Server time only (downtime.go): a day the server was off is not a day without progress.
	return liveSince(d.IndStartedAt, now) > industryStallSec
}

// industryClearProject wipes the project fields; release hands the demand claim back (a settled sale passes false: settleDemand
// already closed it).
func (m *Mod) industryClearProject(ctx context.Context, b *botRun, release bool) {
	d := &b.State.Data
	if release && d.IndTypeID != 0 {
		m.releaseDemand(ctx, b.CharID, d.IndTypeID)
	}
	d.IndTypeID, d.IndTypeName, d.IndBPTypeID, d.IndBPItemID = 0, "", 0, 0
	d.IndFactoryID, d.IndFactorySysID, d.IndRuns, d.IndJobID = 0, 0, 0, 0
	d.IndBPOCost, d.IndMatValue, d.IndFees, d.IndSpentISK, d.IndBPOPaid = 0, 0, 0, 0, 0
	d.IndMiningTrips, d.IndSellerStationID, d.IndSellerSysID, d.IndStartedAt = 0, 0, 0, 0
	d.IndShortInstalls = 0
	d.IndOreWanted, d.IndWaiting, d.IndFetchItemID, d.IndEscrowID = nil, false, 0, 0
	d.IndBPORehunt = false
	d.IndCorpWork, d.IndCorpPO, d.IndCorpMove = 0, 0, 0
	d.IndPrintMove = 0
	d.IndDeadStops = nil
	d.IndBuyLines = nil
	d.IndExtraEscrowIDs = nil
	d.IndSweepStops, d.IndSweepVisited, d.IndSweepCovered = 0, nil, nil
}

// industryAbandon logs, releases the claim, and puts the bot back to idle.
func (m *Mod) industryAbandon(ctx context.Context, b *botRun, why string) error {
	st := b.State
	what := st.Data.IndTypeName
	if what == "" {
		// A state row whose project fields did not survive: the log says "unnamed" rather than leave a blank.
		what = "unnamed"
	}
	m.logBot(b.CharID, logInfo, "industry", "abandoning its %s project: %s", what, why)
	m.unloadCarryPrints(ctx, b)
	// The bill's minerals and the rock bought in place of them come out of the hold too,
	// onto this hangar floor where the stray sweeps sell them. Undocked, plan unloads the
	// minerals at the next dock.
	if b.StationID != 0 {
		m.unloadMinerals(ctx, b)
		if err := m.moveCargoOreToHangar(ctx, b); err != nil {
			rlogf("rolebots: abandon unload ore char=%d: %v", b.CharID, err)
		}
	}
	m.industryClearProject(ctx, b, true)
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	return saveState(ctx, m.db, st)
}

// ── the phases ──────────────────────────────────────────────────────────────

// industryStep advances whichever industry phase the bot is in. All phases
// run docked.
func (m *Mod) industryStep(ctx context.Context, b *botRun) (bool, error) {
	// A phase from a state row whose project fields did not survive (a half-cleared project): abandon with a reason, since every phase
	// would otherwise resolve a line at station 0.
	if d := b.State.Data; d.IndTypeID == 0 || d.IndFactoryID == 0 {
		return false, m.industryAbandon(ctx, b, "its project no longer says what it was building or where")
	}
	d := &b.State.Data
	// The stall clock is read on every phase tick, not only from industryResume, because a project bouncing between refine and install
	// never goes idle. Pre-install phases only (industryStalled).
	if industryStalled(d, b.State.Phase, clock.Unix()) {
		return false, m.industryAbandon(ctx, b, "made no progress in a day")
	}
	// Refine and install read the project's hangar at the station the pilot stands in (the line consumes at b.StationID, ore is refined
	// there), but the materials are at the factory. Two paths can put a pilot in these phases at a seller stop, so go to the factory first.
	switch b.State.Phase {
	case string(TaskIndRefine), string(TaskIndInstall):
		if b.StationID != d.IndFactoryID {
			m.logBot(b.CharID, logInfo, "industry",
				"its %s project lives at %s — heading there before touching the bill",
				d.IndTypeName, m.stationLabel(ctx, d.IndFactoryID))
			return false, m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, b.State.Phase)
		}
	}
	switch b.State.Phase {
	case string(TaskIndBuyBPO):
		return false, m.industryBuyBPO(ctx, b)
	case string(TaskIndRefine):
		return false, m.industryRefine(ctx, b)
	case string(TaskIndBuyMats):
		return false, m.industryBuyMats(ctx, b)
	case string(TaskIndInstall):
		return false, m.industryInstall(ctx, b)
	case string(TaskIndCollect):
		return false, m.industryCollect(ctx, b)
	case string(TaskIndSell):
		return false, m.industrySell(ctx, b)
	case string(TaskIndCorpJob):
		return false, m.corpJobStep(ctx, b)
	case string(TaskIndCorpMove):
		return false, m.corpMoveStep(ctx, b)
	case string(TaskIndPrintMove):
		return false, m.printMoveStep(ctx, b)
	}
	return false, fmt.Errorf("industryStep: unexpected phase %q", b.State.Phase)
}

// industryBuyBPO buys the blueprint at the seller's station, market only, no conjuring. It rides to the factory in the ship's cargo.
func (m *Mod) industryBuyBPO(ctx context.Context, b *botRun) error {
	st := b.State
	d := &st.Data
	// Something to carry it in, before the money moves: a project outlives the hull that started it, and an industrialist killed mid-run
	// resumes in a capsule. A blueprint is at most 1 m3, so a 5,000 ISK shuttle off this station's book will do (shuttle.go).
	if _, ok := m.ensureRunabout(ctx, b, "the blueprint run"); !ok {
		return m.industryAbandon(ctx, b,
			"it has nothing to carry a blueprint in and nothing here sells a shuttle")
	}
	// A print on escrow, chosen at planning (bestEscrowBPO).
	lostClaim := false
	if d.IndEscrowID != 0 {
		escrowID := d.IndEscrowID
		d.IndEscrowID = 0
		corpID, _ := evedb.GetCharCorpID(ctx, m.db, b.CharID)
		moved, err := evedb.ClaimEscrowMission(ctx, m.db, escrowID, b.CharID, corpID, b.StationID, true)
		if err != nil {
			// Another buyer got there first. The project is still worth building, so it looks once for another seller (this station first, then
			// the market and escrow at large); a second loss abandons.
			if d.IndBPORehunt {
				return m.industryAbandon(ctx, b, fmt.Sprintf("the escrowed blueprint it came for was gone again (%v)", err))
			}
			d.IndBPORehunt = true
			lostClaim = true
			m.logBot(b.CharID, logInfo, "industry",
				"lost the escrowed %s blueprint to another buyer (%v) - looking for another seller", d.IndTypeName, err)
			var ask float64
			_ = m.db.QueryRowContext(ctx, `SELECT collateral FROM escrows WHERE escrowID = ?`, escrowID).Scan(&ask)
			d.IndBPOCost = ask // the planned price, which bounds the re-hunt
			goto stationBuy
		}
		var bpItemID int64
		for _, id := range moved {
			var t int64
			if m.db.QueryRowContext(ctx, `SELECT typeID FROM entity WHERE itemID = ?`, id).Scan(&t) == nil &&
				t == d.IndBPTypeID {
				bpItemID = id
				break
			}
		}
		if bpItemID == 0 {
			return m.industryAbandon(ctx, b, "the escrow listing held no blueprint of the type it paid for")
		}
		var paid float64
		_ = m.db.QueryRowContext(ctx, `SELECT collateral FROM escrows WHERE escrowID = ?`, escrowID).Scan(&paid)
		if err := m.moveItemToCargo(ctx, b, bpItemID); err != nil {
			return err
		}
		d.IndBPItemID, d.IndBPOCost = bpItemID, paid
		d.IndSpentISK += paid
		d.IndBPOPaid += paid
		m.claimExtraCopies(ctx, b)
		me, pe := m.blueprintLevels(ctx, bpItemID)
		if left, isCopy := m.printRunsLeft(ctx, bpItemID); isCopy && d.IndCorpPO == 0 {
			// A copy: the batch is what it carries and its price is spent on this project's runs, not spread over ten (copyAmortCost). Not for a
			// corporation's purchase order: that copy goes on the library shelf and settlePO books IndBPOCost as what the corporation paid.
			if int64(d.IndRuns) > left {
				d.IndRuns = int(left)
			}
			d.IndBPOCost = copyAmortCost(paid, d.IndRuns, left, m.bpoAmortProjectsFor(ctx, d.IndTypeID))
			m.logBot(b.CharID, logInfo, "industry",
				"bought a %s blueprint copy (%d runs, ME %d / PE %d) off escrow for %.0f ISK — building %d run(s) at %s",
				d.IndTypeName, left, me, pe, paid, d.IndRuns, m.stationLabel(ctx, d.IndFactoryID))
			return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndRefine))
		}
		m.logBot(b.CharID, logInfo, "industry",
			"bought a %s blueprint (ME %d / PE %d) off escrow for %.0f ISK — heading to its factory at %s",
			d.IndTypeName, me, pe, paid, m.stationLabel(ctx, d.IndFactoryID))
		return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndRefine))
	}
	// Its own print, collected rather than bought (industryStartProject).
	if d.IndFetchItemID != 0 {
		id := d.IndFetchItemID
		d.IndFetchItemID = 0
		var one int64
		if err := m.db.QueryRowContext(ctx, `
			SELECT 1 FROM entity WHERE itemID = ? AND ownerID = ? AND locationID = ? AND flag = 4`,
			id, b.CharID, b.StationID).Scan(&one); err != nil {
			return m.industryAbandon(ctx, b, "its own blueprint was no longer where it had left it")
		}
		if err := m.moveItemToCargo(ctx, b, id); err != nil {
			return err
		}
		d.IndBPItemID, d.IndBPOCost = id, 0
		m.logBot(b.CharID, logInfo, "industry", "picked up its own %s blueprint — heading to its factory at %s",
			d.IndTypeName, m.stationLabel(ctx, d.IndFactoryID))
		return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndRefine))
	}
stationBuy:
	// Check escrow for a researched copy before buying from the market.
	if escrowID := m.checkEscrowForBPO(ctx, b, d.IndBPTypeID, b.StationID); escrowID != 0 {
		// The print in that crate, read before the claim empties it: picking the newest print of the type afterwards could take a different
		// one, a spent copy or the pilot's own original.
		var bpItemID int64
		var paid float64
		_ = m.db.QueryRowContext(ctx, `
			SELECT i.itemID, e.collateral FROM escrows e
			  JOIN entity i ON i.locationID = e.crateContainerID
			 WHERE e.escrowID = ? AND i.typeID = ? LIMIT 1`, escrowID, d.IndBPTypeID).Scan(&bpItemID, &paid)
		if err := m.claimEscrowListing(ctx, b, escrowID); err == nil && bpItemID != 0 {
			d.IndBPItemID = bpItemID
			d.IndBPOCost = paid
			d.IndSpentISK += paid
			d.IndBPOPaid += paid
			if left, isCopy := m.printRunsLeft(ctx, bpItemID); isCopy && d.IndCorpPO == 0 {
				if int64(d.IndRuns) > left {
					d.IndRuns = int(left)
				}
				d.IndBPOCost = copyAmortCost(paid, d.IndRuns, left, m.bpoAmortProjectsFor(ctx, d.IndTypeID))
			}
			if err := m.moveItemToCargo(ctx, b, bpItemID); err != nil {
				return err
			}
			m.logBot(b.CharID, logInfo, "industry",
				"claimed a %s blueprint from escrow here for %.0f ISK — heading to its factory",
				d.IndTypeName, paid)
			return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndRefine))
		}
	}
	// Quantity 1, so a part load is impossible and the filled count is discarded. After a lost claim the planned price bounds the market
	// buy, so it goes through the re-hunt.
	if lostClaim {
		return m.industryRehuntBPO(ctx, b)
	}
	itemID, _, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID, d.IndBPTypeID, 1, "blueprint")
	if err != nil {
		return m.industryAbandon(ctx, b, fmt.Sprintf("the blueprint sale fell through (%v)", err))
	}
	d.IndBPItemID = itemID
	d.IndBPOCost = spent
	d.IndSpentISK += spent
	d.IndBPOPaid += spent
	if err := m.moveItemToCargo(ctx, b, itemID); err != nil {
		return err
	}
	m.logBot(b.CharID, logInfo, "industry", "bought the %s blueprint for %.0f ISK — heading to its factory at %s",
		d.IndTypeName, spent, m.stationLabel(ctx, d.IndFactoryID))
	return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndRefine))
}

// bpoRehuntSlack is how far over the price it planned for a project will pay
// for a print after losing its escrowed one: the batch was sized against that
// price, and the wallet gate's cushion is industryAffordMultiple.
const bpoRehuntSlack = industryAffordMultiple

// bpoRehuntAffordable reports whether a second seller's price is one the
// project can still take: within bpoRehuntSlack of what it planned to pay
// (no plan price on record means any price) and inside the wallet. Pure.
func bpoRehuntAffordable(price, planned, wallet float64) bool {
	if price <= 0 || price > wallet {
		return false
	}
	return planned <= 0 || price <= planned*bpoRehuntSlack
}

// industryRehuntBPO is the second half of a lost escrow claim: no other
// escrowed print at this station, so look across the market and escrow the
// way the planner did, and fly to the best seller. Abandons when none is in
// reach or within bpoRehuntSlack of the planned price.
func (m *Mod) industryRehuntBPO(ctx context.Context, b *botRun) error {
	d := &b.State.Data
	planned := d.IndBPOCost
	d.IndBPOCost = 0
	var stop *marketStop
	if _, err := m.shopLadder(ctx, b, []int64{d.IndBPTypeID}, true, m.homeRegionSet(ctx, b),
		industryBPOBand, "", func(scan marketScan) bool {
			stop = bestBuy(scan.Stops, b.Tune)
			return stop != nil
		}); err != nil {
		return err
	}
	if reach, rerr := m.reachFrom(ctx, d.IndFactorySysID, rulesForBot(b)); rerr == nil {
		if es := m.bestEscrowBPO(ctx, b, d.IndBPTypeID, reach, stop, d.IndRuns); es != nil {
			d.IndEscrowID = es.EscrowID
			stop = &marketStop{TypeID: d.IndBPTypeID, StationID: es.StationID,
				SystemID: es.SystemID, RegionID: es.RegionID, Price: es.Price, Jumps: es.Jumps}
		}
	}
	wallet, _ := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if stop == nil || !bpoRehuntAffordable(stop.Price, planned, spendable(wallet)) {
		d.IndEscrowID = 0
		return m.industryAbandon(ctx, b, "the escrowed blueprint it came for was gone and no other seller in reach would do")
	}
	d.IndSellerStationID, d.IndSellerSysID = stop.StationID, stop.SystemID
	m.logBot(b.CharID, logInfo, "industry", "found another %s blueprint at %s for %.0f ISK (planned %.0f) - going for it",
		d.IndTypeName, m.stationLabel(ctx, stop.StationID), stop.Price, planned)
	return m.beginTravel(ctx, b, stop.SystemID, stop.StationID, string(TaskIndBuyBPO))
}

// industryRefine is the project's hub, re-entered after every mining trip
// and mineral run: unload, refine every ore stack with the pilot's own
// yield, and either install, mine more, or go shopping for what the belts
// will not give.
func (m *Mod) industryRefine(ctx context.Context, b *botRun) error {
	st := b.State
	d := &st.Data

	// A purchase order's print goes on the corporation's library shelf, not a line (corpjobs.go), straight from the hold before an unload
	// could merge it into a hangar stack.
	if d.IndCorpPO != 0 {
		return m.corpPODeliver(ctx, b)
	}
	if d.IndCorpWork != 0 {
		return m.corpJobStep(ctx, b)
	}
	// Everything the project owns comes out of the cargo hold first.
	if err := m.moveCargoToHangar(ctx, b, d.IndBPTypeID); err != nil {
		return err
	}
	line, err := m.manufactureLineAt(ctx, b, d.IndFactoryID, 0)
	if err != nil {
		return m.industryAbandon(ctx, b, err.Error())
	}
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return err
	}
	meLevel, _ := m.blueprintLevels(ctx, d.IndBPItemID)
	needs, err := m.industryMaterialNeeds(ctx, d.IndBPTypeID, meLevel, d.IndRuns, skills[skillProductionEff], line.MatMul)
	if err != nil {
		return m.industryAbandon(ctx, b, err.Error())
	}
	for _, n := range needs {
		if err := m.moveCargoToHangar(ctx, b, n.TypeID); err != nil {
			return err
		}
	}
	// And the rock bought in place of a mineral line (industryBuyMats): left in the hold it is
	// never refined, and it takes the room the next buy trip needs.
	if err := m.moveCargoOreToHangar(ctx, b); err != nil {
		return err
	}

	// Refine every raw ore stack in the hangar. Whole portions only; the remainder stays ore for the next trip to top up.
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.itemID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = 4 AND e.singleton = 0
		  AND g.categoryID = 25`, b.CharID, b.StationID)
	if err != nil {
		return err
	}
	var oreStacks []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		oreStacks = append(oreStacks, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var unitsIn, mineralsOut int64
	for _, id := range oreStacks {
		out, err := m.refineStack(ctx, b, id)
		if err != nil {
			return err
		}
		if out != nil {
			unitsIn += out.UnitsIn
			for _, q := range out.Minerals {
				mineralsOut += q
			}
		}
	}
	if unitsIn > 0 {
		m.logBot(b.CharID, logInfo, "industry", "refined %d unit(s) of ore into %d mineral(s) at its factory", unitsIn, mineralsOut)
	}

	short, err := m.industryShortfall(ctx, b, needs)
	if err != nil {
		return err
	}
	// A line the bill now reads as covered takes its standing supply bid off the book: the pilot got there itself and the escrow does
	// nothing. Before the install branch, since full cover makes every bid redundant.
	shortSet := make(map[int64]bool, len(short))
	for _, s := range short {
		shortSet[s.TypeID] = true
	}
	var covered []int64
	for _, n := range needs {
		if !shortSet[n.TypeID] {
			covered = append(covered, n.TypeID)
		}
	}
	m.dropFilledSupplyBids(ctx, b, d.IndFactoryID, covered)
	if len(short) == 0 {
		st.Phase = string(TaskIndInstall)
		st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
		return saveState(ctx, m.db, st)
	}
	peSkill := skills[skillProductionEff]

	// Still short. What the belts could give and what only a market can are two lists, not one verdict: dig for the first, shop for the rest.
	minable, offBelt := m.splitMinableShortfall(ctx, short)

	// Say so out loud: this moment knows an exact shortfall at an exact station, and noteMaterialShortfall splits the whole bill across the
	// two ledgers whose readers (prospectors, traders) can act on their side (minerals.go).
	r := rulesForBot(b)
	reach, err := m.reachFrom(ctx, b.SystemID, r)
	if err != nil {
		return err
	}
	m.noteMaterialShortfall(ctx, b, d.IndFactoryID, short, reach)

	// And put money on the counter for the part the pilot can neither dig nor buy in reach (industrybids.go), alongside the trips: a standing
	// bid is somebody else's errand and takes as long as it takes.
	m.maybeLeaveSupplyBids(ctx, b, line, needs, short, reach)

	// Make or buy is a comparison, not a veto: the market is tried whenever it can answer the bill; the belt takes what it cannot or what is
	// dear (digBuyThreshold, splitByPrice buys the cheap lines). See docs/comment-notes-b12.md#make-or-buy.
	buyList := short
	if industryMinesOwnOre(b.Tune) && len(minable) > 0 && d.IndMiningTrips < industryMaxMiningTrips {
		th := digBuyThreshold(b.Tune.MiningAppetite)
		cheapQ, dearQ := splitByPrice(m.quoteLines(ctx, b, minable), th)
		dear := quoteNeeds(dearQ)
		switch {
		case len(dear) == 0:
			m.logBot(b.CharID, logInfo, "industry",
				"would rather dig, but the market sells its %d minable line(s) cheaper than that is worth (under the %.2fx base it would dig for) — buying",
				len(minable), th)
		case !m.industryCanDig(ctx, b, dear, r):
			m.logBot(b.CharID, logInfo, "industry",
				"would rather dig its own %d material line(s), but no belt it can work yields them — buying instead",
				len(dear))
		case len(cheapQ) == 0:
			return m.industryMineFor(ctx, b, dear, len(short))
		default:
			buyList = withoutLines(short, dear)
			m.logBot(b.CharID, logInfo, "industry",
				"buying the %d line(s) the market sells under %.2fx base (%s) and leaving %d to the belt (%s)",
				len(cheapQ), th, m.quoteNames(ctx, cheapQ), len(dear), m.quoteNames(ctx, dearQ))
		}
	}

	// Can it pay for what is left? Asked before the trip, because a bot that flies to a seller it cannot afford learns what its wallet
	// already knew; the answer to no money is to dig.
	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return err
	}
	buyCost := 0.0
	for _, s := range buyList {
		if p, perr := m.estimatePriceFor(ctx, b, s.TypeID); perr == nil {
			buyCost += p * float64(s.Needed)
		}
	}
	if buyCost > balance {
		why := fmt.Sprintf("the %d outstanding material line(s) cost about %.0f ISK and the wallet holds %.0f",
			len(buyList), buyCost, balance)
		// Not the whole bill, then, but its bargains line by line, as many as the wallet pays for outright (affordableLines); the rest is the
		// next pass's.
		cheapQ, _ := splitByPrice(m.quoteLines(ctx, b, buyList), digBuyThreshold(b.Tune.MiningAppetite))
		pick := affordableLines(cheapQ, balance)
		if len(pick) == 0 {
			return m.industryFallBack(ctx, b, line, peSkill, minable, len(short), why)
		}
		buyList = quoteNeeds(pick)
		m.logBot(b.CharID, logInfo, "industry",
			"%s — buying the %d cheap line(s) it can pay for in full (%s) and leaving the rest",
			why, len(pick), m.quoteNames(ctx, pick))
	}
	// The counter buys exactly this list (industryBuyMats). Nil is the whole
	// bill; a subset keeps it off the lines left to the belt or the wallet.
	d.IndBuyLines = nil
	if len(buyList) < len(short) {
		d.IndBuyLines = lineIDs(buyList)
	}
	// A fresh trip from the factory: the sweep's tally starts empty.
	d.IndSweepStops, d.IndSweepVisited, d.IndSweepCovered = 0, nil, nil

	// Shopping. One stop that clears the most of the list (reach.go).
	prefer := m.homeRegionSet(ctx, b)
	shortIDs := make([]int64, len(buyList))
	// How many units each line still wants, so the stop is picked on what a station can deliver rather than on how many different things
	// it lists (bestSupplyStopFor).
	want := make(map[int64]int64, len(buyList))
	for i, s := range buyList {
		shortIDs[i] = s.TypeID
		want[s.TypeID] += s.Needed
	}
	// Rock counts as the metal it becomes (oresub.go): where ore plus reprocessing is cheaper than the metal, or nothing sells it, the ore
	// joins the list beside the mineral line, which stays; whichever the pilot buys closes the same line.
	subs := m.oreSubstitutions(ctx, b, buyList, reach)
	for _, sub := range subs {
		shortIDs = append(shortIDs, sub.OreTypeID)
		want[sub.OreTypeID] += sub.OreUnits
		if sub.Forced {
			m.logBot(b.CharID, logInfo, "industry",
				"nothing in reach sells %s, so it is shopping for %d× %s and refining — %s",
				sub.MineralName, sub.OreUnits, sub.OreName, sub.Why)
		} else {
			m.logBot(b.CharID, logInfo, "industry",
				"buying %d× %s instead of %s — %s",
				sub.OreUnits, sub.OreName, sub.MineralName, sub.Why)
		}
	}
	var stop *supplyStop
	now := clock.Unix()
	pruneDeadStops(d.IndDeadStops, now)
	sawStop := false
	look, err := m.shopLadder(ctx, b, shortIDs, true, prefer, defaultBand(b), "",
		func(s marketScan) bool {
			live := withoutDeadStops(s.Stops, d.IndDeadStops, now)
			stop = bestSupplyStopCapped(live, want, m.shopCaps(ctx, live, want, true), b.Tune)
			if stop != nil {
				sawStop = true
			}
			if stop != nil && !supplyStopWorthFlying(stop) {
				stop = nil // keep climbing: a far stop must carry real stock
			}
			return stop != nil
		})
	if err != nil {
		return err
	}
	scan := look.Scan
	if stop == nil {
		reason := m.noSellerReason(ctx, scan, "sells the materials it still needs")
		if sawStop {
			reason = "nothing in reach stocks enough of the bill to be worth the trip"
		}
		return m.industryFallBack(ctx, b, line, peSkill, minable, len(short), reason)
	}
	m.logBot(b.CharID, logInfo, "industry",
		"buying %.1f of its %d missing material line(s) (%d of them off-belt) at %s in %s",
		stop.Covered, len(buyList), len(offBelt), m.stationLabel(ctx, stop.StationID),
		m.travelNote(ctx, b, stop.SystemID, stop.Jumps))
	return m.beginTravel(ctx, b, stop.SystemID, stop.StationID, string(TaskIndBuyMats))
}

// splitMinableShortfall divides an outstanding bill into what an asteroid reprocesses into and what it does not. Ore is the only
// thing a pilot can go and get; everything else has to be bought.
func (m *Mod) splitMinableShortfall(ctx context.Context, short []matNeed) (minable, offBelt []matNeed) {
	for _, s := range short {
		var one int64
		if m.db.QueryRowContext(ctx,
			`SELECT 1 FROM invorereprocessing WHERE mineralTypeID = ? LIMIT 1`, s.TypeID).Scan(&one) == nil {
			minable = append(minable, s)
		} else {
			offBelt = append(offBelt, s)
		}
	}
	return minable, offBelt
}

// industryOffBeltBlocker names the first material line the pilot can neither dig, hold nor buy, or "" when the bill is obtainable.
// It is the Tech II gate: a T2 bill has parts no asteroid yields and nothing seeds on the market, so a pilot holding a patent would shop in
// vain. It runs at candidate time and passes the blueprint over; mineral lines are the belt's business. Hangar stock counts, and the bill is
// per run: could one run be assembled at all.
func (m *Mod) industryOffBeltBlocker(ctx context.Context, b *botRun, factoryID int64,
	needs []matNeed, reach map[int64]int) string {

	_, offBelt := m.splitMinableShortfall(ctx, needs)
	if len(offBelt) == 0 {
		return ""
	}
	var wanted []matNeed
	for _, n := range offBelt {
		if n.Needed <= 0 {
			continue
		}
		have, err := evedb.GetCharMaterialQuantity(ctx, m.db, b.CharID, factoryID, n.TypeID)
		if err == nil && have >= n.Needed {
			continue
		}
		wanted = append(wanted, n)
	}
	if len(wanted) == 0 {
		return ""
	}
	ids := make([]int64, len(wanted))
	for i, n := range wanted {
		ids[i] = n.TypeID
	}
	rows, err := m.sellerStations(ctx, ids)
	if err != nil {
		// The market is unreadable, not empty. Refusing a project on a failed
		// query would be inventing a shortage.
		return ""
	}
	sells := make(map[int64]bool, len(ids))
	for _, c := range rows {
		if _, ok := reach[c.SystemID]; ok && c.Stations > 0 {
			sells[c.TypeID] = true
		}
	}
	for _, n := range wanted {
		if !sells[n.TypeID] {
			return m.typeNameOr(ctx, n.TypeID)
		}
	}
	return ""
}

// industryMinesOwnOreMin is the make-or-buy line: a pilot whose mining appetite clears it digs its own bill of materials first; below it
// the belt is a chore and the pilot buys when the wallet and a seller allow, reaching the belt only through industryFallBack. 1.0 means
// the zero-trait pilot mines its own ore.
const industryMinesOwnOreMin = 1.0

func industryMinesOwnOre(tn Tuning) bool { return tn.MiningAppetite >= industryMinesOwnOreMin }

// digBuyThreshold is the price, as a fraction of basePrice, at or under which a dig-first industrialist buys its minerals instead of
// mining them. The keener the miner, the cheaper the metal must be: appetite 1.0 buys at base, 2.0 at 0.8x, 3.0 and over at 0.6x, the
// stable floor mineral listings are held to (askFloorFraction).
func digBuyThreshold(miningAppetite float64) float64 {
	t := 1.0 - 0.2*(miningAppetite-1)
	switch {
	case t > 1.0:
		return 1.0
	case t < askFloorFraction:
		return askFloorFraction
	}
	return t
}

// matQuote is one outstanding material line priced at the counter: the
// cheapest live ask in the pilot's valuation scope (0 when nobody there sells
// it) beside the catalogue price that ask is judged against.
type matQuote struct {
	Need matNeed
	Ask  float64
	Base float64
}

// lineCheap is the make-or-buy test for ONE line, pure: a live ask at or under
// basePrice times the threshold. No live ask, or no basePrice to judge it by,
// is never cheap.
func lineCheap(q matQuote, threshold float64) bool {
	return q.Ask > 0 && q.Base > 0 && q.Ask <= q.Base*threshold
}

// splitByPrice divides a bill into the lines worth buying and the lines worth digging, order preserved. Make-or-buy is decided per
// line, so one dear line does not send the pilot to dig all of it. Pure.
func splitByPrice(q []matQuote, threshold float64) (cheap, dear []matQuote) {
	for _, l := range q {
		if lineCheap(l, threshold) {
			cheap = append(cheap, l)
		} else {
			dear = append(dear, l)
		}
	}
	return cheap, dear
}

// affordableLines picks the whole lines a budget pays for outright, best bargain (ask over base) first, skipping any that no longer fit.
// Lines with no live ask are never picked. It is the wallet's half of the per-line rule: a pilot short of the whole bill still buys the
// lines it can pay for. Pure.
func affordableLines(q []matQuote, budget float64) []matQuote {
	order := make([]matQuote, 0, len(q))
	for _, l := range q {
		if l.Ask > 0 && l.Base > 0 && l.Need.Needed > 0 {
			order = append(order, l)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		return order[i].Ask/order[i].Base < order[j].Ask/order[j].Base
	})
	var out []matQuote
	for _, l := range order {
		cost := l.Ask * float64(l.Need.Needed)
		if cost <= budget {
			out = append(out, l)
			budget -= cost
		}
	}
	return out
}

// quoteLines prices a bill's lines at the counter, in the pilot's valuation scope: the scope its shopping trip is fenced to.
func (m *Mod) quoteLines(ctx context.Context, b *botRun, lines []matNeed) []matQuote {
	out := make([]matQuote, 0, len(lines))
	for _, n := range lines {
		p, src, err := m.estimatePriceSourceFor(ctx, b, n.TypeID)
		if err != nil || src != priceLive {
			p = 0
		}
		out = append(out, matQuote{Need: n, Ask: p, Base: m.typeBasePrice(ctx, n.TypeID)})
	}
	return out
}

// quoteNeeds is the bill back out of its quotes.
func quoteNeeds(q []matQuote) []matNeed {
	out := make([]matNeed, len(q))
	for i, l := range q {
		out[i] = l.Need
	}
	return out
}

// quoteNames lists the lines' type names for a log line.
func (m *Mod) quoteNames(ctx context.Context, q []matQuote) string {
	names := make([]string, len(q))
	for i, l := range q {
		names[i] = m.typeNameOr(ctx, l.Need.TypeID)
	}
	return strings.Join(names, ", ")
}

// withoutLines is short less every line named in drop.
func withoutLines(short, drop []matNeed) []matNeed {
	skip := make(map[int64]bool, len(drop))
	for _, d := range drop {
		skip[d.TypeID] = true
	}
	out := make([]matNeed, 0, len(short))
	for _, s := range short {
		if !skip[s.TypeID] {
			out = append(out, s)
		}
	}
	return out
}

// onlyLines is short restricted to the type IDs in keep; an empty keep means
// the whole bill (StateData.IndBuyLines).
func onlyLines(short []matNeed, keep []int64) []matNeed {
	if len(keep) == 0 {
		return short
	}
	want := make(map[int64]bool, len(keep))
	for _, id := range keep {
		want[id] = true
	}
	out := make([]matNeed, 0, len(short))
	for _, s := range short {
		if want[s.TypeID] {
			out = append(out, s)
		}
	}
	return out
}

// lineIDs is the type IDs of a bill.
func lineIDs(lines []matNeed) []int64 {
	out := make([]int64, len(lines))
	for i, l := range lines {
		out[i] = l.TypeID
	}
	return out
}

// oreCoverUnits is how much of a mineral line a rock purchase closes.
func oreCoverUnits(oreUnits int64, metalPerOre float64) int64 {
	if oreUnits <= 0 || metalPerOre <= 0 {
		return 0
	}
	return int64(float64(oreUnits) * metalPerOre)
}

// industryShipWhatItHas commits the project at the runs its hangar already covers, or abandons it: industryFallBack without the belt
// arm, for a pilot the belts cannot help.
func (m *Mod) industryShipWhatItHas(ctx context.Context, b *botRun, why string) error {
	line, err := m.manufactureLineAt(ctx, b, b.State.Data.IndFactoryID, 0)
	if err != nil {
		return m.industryAbandon(ctx, b, why)
	}
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return err
	}
	return m.industryFallBack(ctx, b, line, skills[skillProductionEff], nil, 0, why)
}

func (m *Mod) industryMineFor(ctx context.Context, b *botRun, minable []matNeed, shortLines int) error {
	st := b.State
	d := &st.Data
	ids := make([]int64, len(minable))
	for i, s := range minable {
		ids[i] = s.TypeID
	}
	// industryMineFor digs for a named set of minerals, not the generic hierarchy: the trip is strict (miningConfigDerived), stockpiles at
	// the factory and is project execution, not appetite work (no Worked["mining"]).
	ores, head := m.oresYieldingMineralsHead(ctx, ids, rulesForBot(b), m.canMineMercoxit(ctx, b))
	d.IndOreWanted = ores[:head]
	d.IndMiningTrips++
	after := "the usual hierarchy"
	if len(d.IndOreWanted) > 0 {
		after = d.IndOreWanted[0]
	}
	m.logBot(b.CharID, logInfo, "industry",
		"short %d of %d material line(s) the belts can give — mining trip %d after %s, stockpiled back at the factory",
		len(minable), shortLines, d.IndMiningTrips, after)
	if err := saveState(ctx, m.db, st); err != nil {
		return err
	}
	if err := m.miningPlan(ctx, b); err != nil {
		// A trip the world refused (no belt this hull survives, no fit a seller stocks, no money) never left the hangar: un-charge the counter
		// and retry later. No mining fit at all will not change by the next pass, so ship what the hangar covers or let the project go.
		if errors.Is(err, errNoFitGenerated) {
			d.IndMiningTrips--
			return m.industryShipWhatItHas(ctx, b, "no mining fit it can fly or buy from here")
		}
		// None of the rock the bill needs is anywhere it will fly; more trips would fetch the same nothing.
		if errors.Is(err, errNoBeltForOre) {
			d.IndMiningTrips--
			return m.industryShipWhatItHas(ctx, b, "no belt it will fly to yields "+after)
		}
		if refusalIsPassOver(err) {
			d.IndMiningTrips--
			st.Activity = ActIndustry
			st.Data.LegEndsAt = clock.Unix() + industryBeltRetrySec
			m.logBot(b.CharID, logInfo, "industry",
				"the mining trip would not start (%v) — stalling the project and retrying in %dm", err, industryBeltRetrySec/60)
			return saveState(ctx, m.db, st)
		}
		return err
	}
	return nil
}

// industryFallBack is what happens when the market is not the answer: nobody sells the rest, or the wallet cannot pay. The project has
// already paid for a blueprint, so giving up is the last resort. In order: commit the job at the runs the hangar covers (a smaller job that
// ships beats a perfect one that never does), keep digging if a belt could close the gap, then abandon.
func (m *Mod) industryFallBack(ctx context.Context, b *botRun, line *manufactureLine,
	peSkill int, minable []matNeed, shortLines int, why string) error {

	runs, err := m.industryFeasibleRuns(ctx, b, line, peSkill)
	if err != nil {
		return err
	}
	if runs > 0 {
		st := b.State
		m.logBot(b.CharID, logInfo, "industry",
			"%s — committing the job at %d run(s) instead of %d and moving on",
			why, runs, st.Data.IndRuns)
		st.Data.IndRuns = runs
		// The runs were counted at the factory and the line consumes where the pilot stands, so the install happens there: beginTravel is a
		// phase set when already docked, and from a seller stop it is the trip home.
		return m.beginTravel(ctx, b, st.Data.IndFactorySysID, st.Data.IndFactoryID, string(TaskIndInstall))
	}
	if len(minable) > 0 && b.State.Data.IndMiningTrips < industryMaxMiningTripsHard {
		m.logBot(b.CharID, logInfo, "industry",
			"%s, and not a single run is covered — the belts are the only way this finishes", why)
		return m.industryMineFor(ctx, b, minable, shortLines)
	}
	return m.industryAbandon(ctx, b, why)
}

// industryCanDig answers whether there is a belt this pilot can work that yields any of these lines: some ore it will fly to reprocesses
// into one, and the ship has teeth (a toothless hull is refused every belt, threat.go). It is the cheap precondition that keeps miningPlan's
// expensive belt walk from starting for nothing, not a belt search.
func (m *Mod) industryCanDig(ctx context.Context, b *botRun, minable []matNeed, r travelRules) bool {
	if m.shipHasNoTeeth(ctx, b.ShipID) {
		return false
	}
	return m.oreWithinReach(ctx, minable, r)
}

// oreWithinReach is the geography half of industryCanDig: does any ore this pilot would fly to reprocess into any of these lines? Sourcing
// asks the armed question (an unarmed hull is refused every belt, threat.go); pricing must not, because a pilot pays for minerals in time
// whether or not it has a gun this minute (rearmToothless fits one).
func (m *Mod) oreWithinReach(ctx context.Context, minable []matNeed, r travelRules) bool {
	if len(minable) == 0 {
		return false
	}
	reachable := oresReachable(r)
	for _, n := range minable {
		if n.Needed <= 0 {
			continue
		}
		if _, ok := m.mineralPerM3Reach(ctx, n.TypeID, reachable); ok {
			return true
		}
	}
	return false
}

func (m *Mod) industryFeedableRuns(ctx context.Context, b *botRun, factoryID int64, perRun []matNeed,
	r travelRules, want, tripBudget int) (runs int, buy []matNeed, blocker string) {

	if tripBudget < 1 {
		tripBudget = industryMaxMiningTrips
	}

	minable, _ := m.splitMinableShortfall(ctx, perRun)
	if len(minable) == 0 {
		return want, nil, ""
	}
	reachOres := oresReachable(r)
	runs = want
	for _, n := range minable {
		if n.Needed <= 0 {
			continue
		}
		have, err := evedb.GetCharMaterialQuantity(ctx, m.db, b.CharID, factoryID, n.TypeID)
		if err != nil {
			return want, nil, ""
		}
		perTrip := 0.0
		if perM3, ok := m.mineralPerM3Reach(ctx, n.TypeID, reachOres); ok {
			perTrip = industryTripOreM3 * perM3 * industryRefineYieldGuess
		}
		reachable := float64(have) + float64(tripBudget)*perTrip
		covers := int(reachable / float64(n.Needed))
		// Not one run's worth in reach: this pilot cannot dig this line, so it buys it (a shopping list, not a dead candidate).
		// See docs/comment-notes-b12.md#feedable-runs.
		if covers < 1 {
			buy = append(buy, n)
			if blocker == "" {
				blocker = m.typeNameOr(ctx, n.TypeID)
			}
			continue
		}
		if covers < runs {
			runs, blocker = covers, m.typeNameOr(ctx, n.TypeID)
		}
	}
	if runs < 0 {
		runs = 0
	}
	return runs, buy, blocker
}

// industryFeasibleRuns is the biggest job the materials already in the factory hangar would cover, 0 when not even one run is. Waste is
// applied per run and then multiplied, so the bill is linear and the answer is the tightest line's stock over its per-run need.
func (m *Mod) industryFeasibleRuns(ctx context.Context, b *botRun, line *manufactureLine, peSkill int) (int, error) {
	d := &b.State.Data
	if d.IndRuns <= 1 {
		return 0, nil
	}
	meLevel, _ := m.blueprintLevels(ctx, d.IndBPItemID)
	perRun, err := m.industryMaterialNeeds(ctx, d.IndBPTypeID, meLevel, 1, peSkill, line.MatMul)
	if err != nil {
		return 0, err
	}
	runs := d.IndRuns - 1
	for _, n := range perRun {
		if n.Needed <= 0 {
			continue
		}
		have, err := evedb.GetCharMaterialQuantity(ctx, m.db, b.CharID, d.IndFactoryID, n.TypeID)
		if err != nil {
			return 0, err
		}
		if covers := int(have / n.Needed); covers < runs {
			runs = covers
		}
	}
	if runs < 1 {
		return 0, nil
	}
	return runs, nil
}

// oreBandProbes is one security value out of each band destiny.OreDistribution draws a different table for, richest space last. Probing the
// live generator avoids a restated copy of its rules going stale.
var oreBandProbes = []float64{1.0, 0.9, 0.7, 0.5, 0.4, 0.2, 0.0, -0.3, -0.6, -0.9}

// oresAtOrAbove is every ore family that spawns anywhere at or above a security line. It probes with factionID 0, the sovereignty that carries
// both lowsec specials at half weight; that overstates reach by one judgement (Hemorphite is Amarr and Gallente space, Hedbergite Caldari and
// Minmatar), which the belt search settles on real systems.
func oresAtOrAbove(sec float64) map[string]bool {
	out := make(map[string]bool, 16)
	for _, probe := range oreBandProbes {
		if probe < sec {
			continue
		}
		for _, w := range destiny.OreDistribution(probe, 0) {
			out[w.Name] = true
		}
	}
	return out
}

// oresReachable is every ore this pilot is allowed, and willing, to go and get. Personality does not keep a miner out of lower security: danger
// is measured (rats on the belt, the hull against the spawn, recent maulings, enemy corporations) and the belt search prices it. A comfort-line
// filter here would hide Zydrine and Megacyte from every miner. Profile.SecFloor is gone and rulesFor leaves SecFloor at -1, so a bot may fly anywhere.
func oresReachable(r travelRules) map[string]bool {
	return oresAtOrAbove(r.SecFloor)
}

// mineralPerM3Reach answers whether an ore this pilot may fly to yields the mineral at all, and how dense the best such ore is. Batches are sized
// on the same rock the belt search may fly to (oresReachable).
func (m *Mod) mineralPerM3Reach(ctx context.Context, mineralID int64,
	reachable map[string]bool) (float64, bool) {

	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeName,
		       r.amountPerBatch / GREATEST(t.portionSize, 1) / GREATEST(t.volume, 0.001) AS perM3
		FROM invorereprocessing r
		JOIN invTypes t ON t.typeID = r.oreTypeID
		WHERE r.mineralTypeID = ?
		ORDER BY perM3 DESC`, mineralID)
	if err != nil {
		rlogf("rolebots: ore density for mineral %d: %v", mineralID, err)
		return 0, false
	}
	defer rows.Close()
	// Rows arrive density-ordered, so the first reachable hit is the best.
	for rows.Next() {
		var name string
		var perM3 float64
		if rows.Scan(&name, &perM3) != nil || perM3 <= 0 {
			continue
		}
		if reachable[name] {
			return perM3, true
		}
	}
	return 0, false
}

// oresYieldingMineralsHead is oresYieldingMinerals plus where the ores this trip was sent for end and the appended hierarchy begins.
// The tail would otherwise let a search after Bistot match Veldspar at home and stop; the head count tells a caller it found what it came
// for from it found the fallback (0 means no head). canMerc lists Mercoxit only for a ship with a deep core miner, since a laser
// cannot touch it.
func (m *Mod) oresYieldingMineralsHead(ctx context.Context, mineralIDs []int64,
	r travelRules, canMerc bool) (ores []string, head int) {

	if len(mineralIDs) == 0 {
		return nil, 0
	}
	known := map[string]bool{}
	for _, n := range defaultOrePriority {
		known[n] = true
	}
	known[oreMercoxit] = canMerc

	reachable := oresReachable(r)
	tierOf := func(name string) int {
		if reachable[name] {
			return 0
		}
		return 1
	}

	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeName,
		       SUM(r.amountPerBatch) / GREATEST(t.portionSize, 1) / GREATEST(t.volume, 0.001) AS perM3
		FROM invorereprocessing r
		JOIN invTypes t ON t.typeID = r.oreTypeID
		WHERE r.mineralTypeID IN (`+placeholders(len(mineralIDs))+`)
		GROUP BY r.oreTypeID, t.typeName, t.portionSize, t.volume
		ORDER BY perM3 DESC`, int64Args(mineralIDs)...)
	if err != nil {
		rlogf("rolebots: ore lookup for minerals %v: %v", mineralIDs, err)
		return nil, 0
	}
	defer rows.Close()
	// The query hands rows back density-ordered, so bucketing by tier keeps
	// each tier density-ordered without a second sort.
	var buckets [2][]string
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		var perM3 float64
		if rows.Scan(&name, &perM3) != nil {
			continue
		}
		if !known[name] || seen[name] {
			continue
		}
		seen[name] = true
		t := tierOf(name)
		buckets[t] = append(buckets[t], name)
	}
	var out []string
	for _, b := range buckets {
		out = append(out, b...)
	}
	if len(out) == 0 {
		return nil, 0
	}
	// Where the wanted ores end and the fallback begins.
	head = len(out)
	// Everything else behind: the belts in range may hold none of the first choices, and the search should still return a belt
	// rather than an error.
	for tier := 0; tier < len(buckets); tier++ {
		for _, n := range defaultOrePriority {
			if !seen[n] && tierOf(n) == tier {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out, head
}

// industryBuyMats buys whatever of the outstanding bill this station
// stocks, loads it, and heads back to the factory.
func (m *Mod) industryBuyMats(ctx context.Context, b *botRun) error {
	st := b.State
	d := &st.Data
	line, err := m.manufactureLineAt(ctx, b, d.IndFactoryID, 0)
	if err != nil {
		return m.industryAbandon(ctx, b, err.Error())
	}
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return err
	}
	meLevel, _ := m.blueprintLevels(ctx, d.IndBPItemID)
	needs, err := m.industryMaterialNeeds(ctx, d.IndBPTypeID, meLevel, d.IndRuns, skills[skillProductionEff], line.MatMul)
	if err != nil {
		return m.industryAbandon(ctx, b, err.Error())
	}
	short, err := m.industryShortfall(ctx, b, needs)
	if err != nil {
		return err
	}
	// The trip was planned for a subset of the bill (industryRefine's per-line make-or-buy): lines left to the belt or past the wallet
	// are not this counter's business. The fallback below still measures the whole bill.
	buyList := onlyLines(short, d.IndBuyLines)
	d.IndBuyLines = nil
	// What earlier stops of this trip already put in the hold: the shortfall
	// above reads the factory hangar only (sweep.go, IndSweepCovered).
	buyList = minusCovered(buyList, d.IndSweepCovered)
	// Goods need a hold and a hull to fly home in before any buying. b.ShipID is what is boarded, not owned, so try an unboarded hull in
	// this hangar first: failing after buyAtStation spent the ISK would return past saveState and lose IndSpentISK.
	if b.ShipID == 0 {
		boarded, err := m.boardHullInHangar(ctx, b)
		if err != nil {
			return err
		}
		if !boarded {
			// Shipless at a seller stop there is no way home, and the fallback's commit would have to fly there (setInSpace returns early on a
			// pilot with no hull). What was bought is stranded either way; end the project.
			if b.StationID != d.IndFactoryID {
				return m.industryAbandon(ctx, b,
					"it has no hull here to carry materials in and its factory is elsewhere")
			}
			peSkill := skills[skillProductionEff]
			minable, _ := m.splitMinableShortfall(ctx, short)
			return m.industryFallBack(ctx, b, line, peSkill, minable, len(short),
				"it has no hull here to carry materials in")
		}
		m.logBot(b.CharID, logInfo, "industry",
			"boarded a hull from the hangar to carry its materials")
	}

	// The rock this station can answer a mineral line with, worked out as the stop was chosen (oresub.go) from the pilot's own reach,
	// since it is standing at the counter.
	var oreBuys []oreSubstitute
	{
		r := rulesForBot(b)
		if reach, rerr := m.reachFrom(ctx, b.SystemID, r); rerr == nil {
			for _, sub := range m.oreSubstitutions(ctx, b, buyList, reach) {
				oreBuys = append(oreBuys, sub)
			}
			sort.Slice(oreBuys, func(i, j int) bool {
				return oreBuys[i].OreTypeID < oreBuys[j].OreTypeID
			})
		}
	}

	bought, partial, stranded := 0, 0, 0
	// The first thing this counter said no to, so a trip that buys nothing says why in its log line.
	var firstRefusal error
	noteRefusal := func(err error) {
		if err != nil && firstRefusal == nil {
			firstRefusal = err
		}
	}
	// What the rock bought here will refine into, per mineral line. The metal loop below buys only the rest, so a station stocking both
	// does not sell the same Tritanium twice.
	covered := map[int64]int64{}
	// The hold binds before the bill does: what will not fit stays here and is not bought.
	room := m.shipHoldRoom(ctx, b)
	fitUnits := func(typeID, want int64) int64 {
		if v := m.typeVolume(ctx, typeID); v > 0 {
			if fit := int64(room / v); fit < want {
				return max(0, fit)
			}
		}
		return want
	}
	takeRoom := func(typeID, units int64) {
		room -= float64(units) * m.typeVolume(ctx, typeID)
	}
	// Rock first. It is the cheaper half of every line it answers, and buying the metal first would spend the wallet the rock was the
	// affordable answer to. industryRefine's hangar sweep reprocesses it on arrival.
	for _, sub := range oreBuys {
		oreUnits := fitUnits(sub.OreTypeID, sub.OreUnits)
		if oreUnits <= 0 {
			continue
		}
		_, filled, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID,
			sub.OreTypeID, oreUnits, "materials (ore to refine)")
		if err != nil || filled <= 0 {
			noteRefusal(err)
			continue
		}
		d.IndSpentISK += spent
		covered[sub.MineralID] += oreCoverUnits(filled, sub.MetalPerOre)
		takeRoom(sub.OreTypeID, filled)
		m.noteOreFlow(ctx, sub.OreTypeID, oreStageBought, filled, spent)
		if err := m.moveHangarToCargo(ctx, b, sub.OreTypeID); err != nil {
			m.logBot(b.CharID, logError, "industry",
				"bought %s but could not load it (%v) — left in the hangar here",
				sub.OreName, err)
			stranded++
			continue
		}
		bought++
		m.logBot(b.CharID, logInfo, "industry",
			"bought %d× %s for its %s line — %s", filled, sub.OreName, sub.MineralName, sub.Why)
	}
	// The metal lines this counter was asked for and gave, for the
	// shopper-demand ledger (stationdemand.go). Wallet refusals say nothing
	// about the market here.
	var stop []shopLine
	defer func() { m.noteShopStop(ctx, b.StationID, stop) }()
	for _, s := range buyList {
		// A short fill counts: what arrived shrinks the outstanding bill and industryShortfall re-reads the hangar next pass, so a part load
		// is progress, and the difference is what noteMineralShortfall writes to the ledger.
		need := s.Needed - covered[s.TypeID]
		if need <= 0 {
			continue // the rock already bought here covers this line
		}
		if need = fitUnits(s.TypeID, need); need <= 0 {
			continue // the hold is full: the rest waits for the next trip
		}
		_, filled, spent, err := m.buyAtStation(ctx, b.CharID, b.StationID, s.TypeID, need, "materials")
		if err != nil {
			if !isMoneyRefusal(err) {
				stop = append(stop, shopLine{TypeID: s.TypeID, Asked: need})
			}
			noteRefusal(fmt.Errorf("%s: %w", m.typeNameOr(ctx, s.TypeID), err))
			continue // this station stocks none of it; the next refine pass re-plans
		}
		stop = append(stop, shopLine{TypeID: s.TypeID, Asked: need, Filled: filled})
		d.IndSpentISK += spent
		covered[s.TypeID] += filled
		takeRoom(s.TypeID, filled)
		if err := m.moveHangarToCargo(ctx, b, s.TypeID); err != nil {
			// Paid for and safe in this station's hangar: a hold that will not take it is a reason to leave it behind, not to drop the rest of the
			// list and the record of its cost. industryShortfall re-reads the hangar next pass.
			m.logBot(b.CharID, logError, "industry",
				"bought %s but could not load it (%v) — left in the hangar here",
				m.typeNameOr(ctx, s.TypeID), err)
			stranded++
			continue
		}
		bought++
		if filled < s.Needed {
			partial++
		}
	}
	shortNote := ""
	if partial > 0 {
		shortNote = fmt.Sprintf(" (%d of them a part load)", partial)
	}
	if stranded > 0 {
		shortNote += fmt.Sprintf(" (%d left in the hangar here)", stranded)
	}
	// Bought nothing here at all: keep this counter off the project's list for
	// a while (IndDeadStops), and say what it refused.
	if bought == 0 && stranded == 0 {
		if d.IndDeadStops == nil {
			d.IndDeadStops = map[int64]int64{}
		}
		d.IndDeadStops[b.StationID] = clock.Unix() + industryDeadStopSec
		if firstRefusal != nil {
			shortNote += fmt.Sprintf(" (first refusal: %v)", firstRefusal)
		}
	}
	// The sweep: what this stop covered joins the trip's tally; while lines are short, the hold has room and stops remain, the next
	// seller is on the way (sweep.go).
	if d.IndSweepCovered == nil {
		d.IndSweepCovered = map[int64]int64{}
	}
	for t, n := range covered {
		d.IndSweepCovered[t] += n
	}
	d.IndSweepStops++
	d.IndSweepVisited = append(d.IndSweepVisited, b.StationID)
	if next, left := m.industryNextSweepStop(ctx, b, buyList, covered, room); next != nil {
		d.IndBuyLines = lineIDs(left)
		m.logBot(b.CharID, logInfo, "industry",
			"bought %d of %d outstanding mineral line(s) here%s (%.0f ISK into the project so far) — on to %s for the %d line(s) still short",
			bought, len(buyList), shortNote, d.IndSpentISK, m.stationLabel(ctx, next.StationID), len(left))
		return m.beginTravel(ctx, b, next.SystemID, next.StationID, string(TaskIndBuyMats))
	}
	m.logBot(b.CharID, logInfo, "industry", "bought %d of %d outstanding mineral line(s) here%s (%.0f ISK into the project so far) — back to the factory",
		bought, len(buyList), shortNote, d.IndSpentISK)
	return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndRefine))
}

// minusCovered takes what earlier stops of a sweep already bought off a
// bill, dropping lines it closes. Pure.
func minusCovered(bill []matNeed, covered map[int64]int64) []matNeed {
	if len(covered) == 0 {
		return bill
	}
	out := make([]matNeed, 0, len(bill))
	for _, n := range bill {
		n.Needed -= covered[n.TypeID]
		if n.Needed > 0 {
			out = append(out, n)
		}
	}
	return out
}

// industryNextSweepStop is the next seller of a materials sweep, nil when the
// trip should go home: nothing short, a full hold, the stop count spent, the
// sweep switched off, or no stop left that carries real stock under the cap.
// It returns the lines still short, which the next counter is to buy.
func (m *Mod) industryNextSweepStop(ctx context.Context, b *botRun, bill []matNeed, covered map[int64]int64,
	room float64) (*supplyStop, []matNeed) {
	cfg := m.sweepSettings(ctx)
	d := &b.State.Data
	if !cfg.Enabled || room <= 0 || d.IndSweepStops >= cfg.MaxStops {
		return nil, nil
	}
	left := minusCovered(bill, covered)
	if len(left) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(left))
	want := make(map[int64]int64, len(left))
	for i, n := range left {
		ids[i] = n.TypeID
		want[n.TypeID] = n.Needed
	}
	visited := map[int64]bool{}
	for _, s := range d.IndSweepVisited {
		visited[s] = true
	}
	now := clock.Unix()
	var next *supplyStop
	_, err := m.shopLadder(ctx, b, ids, true, m.homeRegionSet(ctx, b), defaultBand(b), "",
		func(s marketScan) bool {
			var live []marketStop
			for _, st := range withoutDeadStops(s.Stops, d.IndDeadStops, now) {
				if !visited[st.StationID] {
					live = append(live, st)
				}
			}
			next = bestSupplyStopCapped(live, want, m.shopCaps(ctx, live, want, true), b.Tune)
			if next != nil && (!supplyStopWorthFlying(next) || next.Jumps > cfg.MaxExtraJumps) {
				next = nil
			}
			return next != nil
		})
	if err != nil || next == nil {
		return nil, nil
	}
	return next, left
}

// industryInstall runs the real install. On success the bot is free: the project fields persist, the phase goes idle, and
// industryResume collects when the job is done.
func (m *Mod) industryInstall(ctx context.Context, b *botRun) error {
	st := b.State
	d := &st.Data
	// A copy builds what it has left; the books (IndCooking, the sale price)
	// must count the runs that actually go on the line.
	if left, isCopy := m.printRunsLeft(ctx, d.IndBPItemID); isCopy && left > 0 && int64(d.IndRuns) > left {
		d.IndRuns = int(left)
	}
	// The extra copies bought on the same trip go into the factory hangar,
	// where the split's later lanes find them (copylanes.go).
	m.unloadCarryPrints(ctx, b)
	jobID, installedBP, fees, matValue, err := m.installManufactureJob(ctx, b, d.IndBPItemID, d.IndBPTypeID, d.IndRuns)
	d.IndBPItemID = installedBP // the singleton split may have re-homed it
	if err != nil {
		if strings.Contains(err.Error(), "insufficient materials") {
			// Raced its own arithmetic (or the multiplier moved): go gather more. Bounded, because the shortfall check said the bill was covered:
			// a third refusal in a row is a bill this pilot cannot read, not a race.
			d.IndShortInstalls++
			if d.IndShortInstalls >= industryMaxShortInstalls {
				return m.industryAbandon(ctx, b,
					fmt.Sprintf("the line refused the install %d times running (%v)", d.IndShortInstalls, err))
			}
			m.logBot(b.CharID, logInfo, "industry", "install came up short (%v) — gathering more", err)
			return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndRefine))
		}
		return m.industryAbandon(ctx, b, err.Error())
	}
	d.IndFees = fees
	d.IndMatValue = matValue
	d.IndSpentISK += fees
	_, _, endTime, jerr := m.industryJobStatus(ctx, jobID)
	eta := ""
	if jerr == nil {
		if secs := evedb.Win32ToUnix(endTime) - clock.Unix(); secs > 0 {
			eta = fmt.Sprintf(" — ready in %dh%02dm", secs/3600, (secs%3600)/60)
		}
	}
	m.logBot(b.CharID, logInfo, "industry",
		"installed %d run(s) of %s (job %d, %.0f ISK fees, %.0f ISK of materials, %.0f ISK cash sunk)%s",
		d.IndRuns, d.IndTypeName, jobID, fees, matValue, d.IndSpentISK, eta)
	if jerr == nil {
		if secs := evedb.Win32ToUnix(endTime) - clock.Unix(); secs > 0 {
			m.chatterNoteBot(ctx, b.CharID, "ind.install", map[string]string{
				"work": plural(d.IndRuns, "run", "runs"), "what": d.IndTypeName, "eta": etaWords(secs)}, jobID)
		}
	}
	// The gathering over-shot, as gathering does: a mining trip lands a whole hold. What the job did not eat is dead capital in a hangar
	// the pilot is leaving, so it goes back to the market on the way out.
	m.industrySellSurplus(ctx, b, d.IndBPTypeID)

	// The job moves off the project onto the cooking list, which frees the project slot now rather than when the oven rings, so a pilot
	// with three lines fills the other two.
	d.IndCooking = append(d.IndCooking, IndCookingJob{
		JobID: jobID, TypeID: d.IndTypeID, TypeName: d.IndTypeName,
		BPTypeID: d.IndBPTypeID, BPItemID: d.IndBPItemID, Evidence: d.IndEvidence,
		FactoryID: d.IndFactoryID, FactorySysID: d.IndFactorySysID, Runs: d.IndRuns,
		BPOCost: d.IndBPOCost, MatValue: matValue, Fees: fees, CashSpent: d.IndSpentISK,
		BPOPaid:   d.IndBPOPaid,
		StartedAt: clock.Unix(),
	})
	cooking := d.IndCooking
	// Clear without releasing the claim: the claim belongs to the job now, and industryHarvest keeps it warm until the goods are listed.
	m.industryClearProject(ctx, b, false)
	d.IndCooking = cooking

	// A free line is work in hand, not an opportunity to be re-drawn for: the capacity is paid for and the station is under its feet, so the
	// next project starts here. Only a factory with nothing left it can build hands the pilot back to the draw.
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	free, ferr := m.industryFreeSlots(ctx, b)
	if ferr == nil && free > 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"%d manufacturing line(s) still free — starting the next batch now", free)
		st.Activity = ActIndustry
		nerr := m.industryStartProject(ctx, b)
		if nerr == nil {
			return nil
		}
		m.logBot(b.CharID, logInfo, "industry",
			"nothing to put on the %d free line(s): %v", free, nerr)
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	}
	return saveState(ctx, m.db, st)
}

// industryFreeSlots is how many manufacturing lines this pilot could still install on: its Mass Production ceiling minus what ramJobs
// says is running. Counted from the database, never from IndCooking, which is the pilot's memory and comes apart from ramJobs when a
// job is cancelled or delivered from outside (the ESP factory page does both).
func (m *Mod) industryFreeSlots(ctx context.Context, b *botRun) (int, error) {
	base, err := m.charSkillLevelFor(ctx, b.CharID, serverram.SkillMassProduction)
	if err != nil {
		return 0, err
	}
	adv, err := m.charSkillLevelFor(ctx, b.CharID, serverram.SkillAdvancedMassProduction)
	if err != nil {
		return 0, err
	}
	max := serverram.MaxConcurrentJobs(base, adv)
	active, err := evedb.CountActiveJobs(ctx, m.db, b.CharID, true)
	if err != nil {
		return 0, err
	}
	if free := max - active; free > 0 {
		return free, nil
	}
	return 0, nil
}

// industryHarvest walks the cooking list and deals with whatever the lines have finished with. A ready job takes the pilot over: it
// restores the project fields from the list entry and routes to the collect phase, so collect and sell read the fields they always
// read. Jobs that died on the line are dropped and their claims handed back. Returns handled=true when it has routed the bot.
func (m *Mod) industryHarvest(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	d := &st.Data
	if len(d.IndCooking) == 0 {
		return false, nil
	}
	kept := d.IndCooking[:0:0]
	var ready *IndCookingJob
	dirty := false
	for i := range d.IndCooking {
		j := d.IndCooking[i]
		// A claim is only released when the project ends without a listing; while the job cooks it must stay warm or another supplier takes
		// the type from a job already paid for.
		m.touchClaim(ctx, b.CharID, j.TypeID)
		if j.Delivered {
			// The sweep already took this one off the line and the goods are in the factory hangar; only the trip to list them is left.
			if ready == nil {
				ready = &d.IndCooking[i]
				dirty = true
				continue
			}
			kept = append(kept, j)
			continue
		}
		status, isReady, _, err := m.industryJobStatus(ctx, j.JobID)
		switch {
		case err != nil:
			m.logBot(b.CharID, logError, "industry",
				"job %d (%s) is no longer on any line — dropping it", j.JobID, j.TypeName)
			m.releaseDemand(ctx, b.CharID, j.TypeID)
			dirty = true
			continue
		case status >= 100 && status != 101:
			m.logBot(b.CharID, logInfo, "industry",
				"job %d (%s) was cancelled from outside — closing it", j.JobID, j.TypeName)
			m.releaseDemand(ctx, b.CharID, j.TypeID)
			dirty = true
			continue
		case status == 101 || isReady:
			// Delivered from outside counts as ready: the goods are in the factory hangar either way and the listing is still worth the trip
			// (industryCollect handles "already delivered").
			if ready == nil {
				ready = &d.IndCooking[i]
				dirty = true
				continue
			}
		}
		kept = append(kept, j)
	}
	if ready == nil {
		if dirty {
			d.IndCooking = kept
			return false, saveState(ctx, m.db, st)
		}
		return false, nil
	}
	// Hydrate the project fields from the finished job. Nothing downstream
	// has to know the pilot walked away from it.
	j := *ready
	d.IndCooking = kept
	d.IndTypeID, d.IndTypeName, d.IndEvidence = j.TypeID, j.TypeName, j.Evidence
	d.IndBPTypeID, d.IndBPItemID = j.BPTypeID, j.BPItemID
	d.IndFactoryID, d.IndFactorySysID = j.FactoryID, j.FactorySysID
	d.IndRuns, d.IndJobID = j.Runs, j.JobID
	d.IndBPOCost, d.IndMatValue, d.IndFees = j.BPOCost, j.MatValue, j.Fees
	d.IndSpentISK, d.IndBPOPaid = j.CashSpent, j.BPOPaid
	d.IndStartedAt, d.IndWaiting = clock.Unix(), false
	// A job the sweep already took off the line has nothing to collect: the trip is for the listing, and the collect phase would ask
	// CompleteJob to deliver it twice.
	next := TaskIndCollect
	if j.Delivered {
		d.IndJobID = 0
		next = TaskIndSell
	}
	st.Activity = ActIndustry
	return true, m.beginTravel(ctx, b, j.FactorySysID, j.FactoryID, string(next))
}

// industrySoonestOven is how long until the next cooking job rings, and
// where it is. ok is false when nothing is cooking.
func (m *Mod) industrySoonestOven(ctx context.Context, b *botRun) (left, stationID, sysID int64, ok bool) {
	for _, j := range b.State.Data.IndCooking {
		if j.Delivered {
			continue // off its line already: nothing to wait for
		}
		_, _, endTime, err := m.industryJobStatus(ctx, j.JobID)
		if err != nil {
			continue
		}
		l := evedb.Win32ToUnix(endTime) - clock.Unix()
		if l < 0 {
			l = 0
		}
		if !ok || l < left {
			left, stationID, sysID, ok = l, j.FactoryID, j.FactorySysID, true
		}
	}
	return left, stationID, sysID, ok
}

// industrySellSurplus dumps what the installed job left behind into the local bids, immediate-or-nothing. Scope is narrow: only the
// material types this blueprint asked for, since the hangar also holds ore and minerals stockpiled for the next project. Failures are
// logged and dropped; the stock stays in the hangar and the next project starts ahead.
func (m *Mod) industrySellSurplus(ctx context.Context, b *botRun, bpTypeID int64) {
	if b.StationID == 0 {
		return
	}
	reqs, err := evedb.GetRequiredMaterials(ctx, m.db, bpTypeID, 1)
	if err != nil {
		return
	}
	sold, gross := 0, 0.0
	for _, r := range reqs {
		if r.DamagePerJob < 1 {
			continue // never gathered for, never surplus
		}
		var itemID, qty int64
		err := m.db.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity
			WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
			ORDER BY quantity DESC LIMIT 1`,
			b.CharID, b.StationID, r.TypeID).Scan(&itemID, &qty)
		if err != nil || qty <= 0 {
			continue
		}
		// The best local bid is the price; no bid means no sale. A mineral line looks only at player bids (bidDump.PlayerOnly): selling surplus
		// minerals to the NPC seeder's bottomless bid would remove them from the roster's economy. What no player bids for stays in the hangar.
		q := `
			SELECT MAX(price) FROM market_orders
			WHERE stationID = ? AND typeID = ? AND bid = 1 AND orderState = 1 AND volRemaining > 0`
		args := []any{b.StationID, r.TypeID}
		if _, isMineral := mineralListFloors[r.TypeID]; isMineral {
			q += ` AND charID NOT IN (?, ?)`
			args = append(args, evedb.SeederCharID, evedb.AdminSeederCharID)
		}
		var bid sql.NullFloat64
		if m.db.QueryRowContext(ctx, q, args...).Scan(&bid) != nil || !bid.Valid || bid.Float64 <= 0 {
			continue
		}
		out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
			r.TypeID, itemID, bid.Float64, qty, -1 /*station*/, 1, 0 /*immediate*/, false)
		if err != nil {
			rlogf("rolebots: industry surplus sale %d: %v", r.TypeID, err)
			continue
		}
		if out.ErrorMsg != "" {
			continue
		}
		for _, f := range out.Fills {
			gross += f.Price * float64(f.Quantity)
		}
		m.notePeerFills(ctx, tradeEntry{
			CharID: b.CharID, TypeID: r.TypeID, TypeName: m.typeNameOr(ctx, r.TypeID),
			Station: b.StationID, System: b.SystemID,
		}, out.Fills, false, "surplus")
		sold++
	}
	if sold > 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"sold off %d surplus material line(s) at the factory for %.0f ISK — the job is committed, the rest is somebody else's problem",
			sold, gross)
	}
}

// industryCollect delivers the finished job into the factory hangar.
func (m *Mod) industryCollect(ctx context.Context, b *botRun) error {
	st := b.State
	d := &st.Data
	outs, err := evedb.CompleteJob(ctx, m.db, d.IndJobID, b.CharID, false)
	if err != nil {
		if strings.Contains(err.Error(), "still running") {
			// The line has not rung after all: the travel leg outran the clock. A paid-for job is not thrown away over a minute.
			st.Activity, st.Phase = "", string(TaskIdle)
			st.Data.LegEndsAt = clock.Unix() + industryOvenPollSec
			return saveState(ctx, m.db, st)
		}
		m.logBot(b.CharID, logError, "industry", "collecting job %d failed: %v", d.IndJobID, err)
		m.industryClearProject(ctx, b, true)
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		return saveState(ctx, m.db, st)
	}
	d.IndJobID = 0
	// A multi-run job comes back as several output stacks and industrySell lists one, so stack them: one row, one order, the whole run
	// (stacking.go).
	m.stackHangar(ctx, b.CharID, b.StationID)
	m.logBot(b.CharID, logInfo, "industry", "collected %d output stack(s) of %s from the line", len(outs), d.IndTypeName)
	st.Phase = string(TaskIndSell)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return saveState(ctx, m.db, st)
}

// industrySell lists the product at the build station: cost-plus with the
// pilot's greed-bent margin, resting for a week at most. The buyers'
// market search reads the whole database, so the listing does not need to
// chase them.
func (m *Mod) industrySell(ctx context.Context, b *botRun) error {
	st := b.State
	d := &st.Data
	// A run carried to a leak station (below) arrives in the hold.
	awayFromFactory := b.StationID != d.IndFactoryID
	if awayFromFactory {
		if err := m.moveCargoToHangar(ctx, b, d.IndTypeID); err != nil {
			m.logBot(b.CharID, logError, "industry", "unloading its %s: %v", d.IndTypeName, err)
		}
	}
	var itemID, qty int64
	err := m.db.QueryRowContext(ctx, `
		SELECT itemID, quantity FROM entity
		WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = 4 AND singleton = 0
		ORDER BY quantity DESC LIMIT 1`,
		b.CharID, b.StationID, d.IndTypeID).Scan(&itemID, &qty)
	if errors.Is(err, sql.ErrNoRows) {
		if awayFromFactory && d.IndFactoryID != 0 {
			// Not carried here after all: the run is still on the factory floor.
			return m.beginTravel(ctx, b, d.IndFactorySysID, d.IndFactoryID, string(TaskIndSell))
		}
		return m.industryAbandon(ctx, b, "the finished goods are not in the hangar")
	}
	if err != nil {
		return err
	}
	price := industryUnitPrice(d.IndMatValue, d.IndFees, d.IndBPOCost,
		d.IndRuns, m.bpoAmortProjectsFor(ctx, d.IndTypeID), qty, b.Tune.IndustryMargin)
	// Sell where the buyers are: a run whose buyers have been paying an NPC a few jumps off, or finding a counter short, is carried there
	// and listed (industryHaulToOutlet). Only from the factory; once carried, a run is listed where it lands.
	if !awayFromFactory {
		// Valued at what it would ask here (cost-plus lifted to the NPC band): cost-plus alone prices a run built from the pilot's own minerals
		// at a few ISK, so no counter would look worth the trip.
		worth, _ := m.anchoredAsk(ctx, b.Tune, b.StationID, d.IndTypeID, price)
		if hauled, herr := m.industryHaulToOutlet(ctx, b, itemID, qty, worth); herr != nil || hauled {
			return herr
		}
	}
	// What the batch cost: the cash, with the print charged as the price
	// charges it (industryCostBasis).
	cost := industryCostBasis(d.IndSpentISK, d.IndBPOPaid, d.IndBPOCost, m.bpoAmortProjectsFor(ctx, d.IndTypeID))
	// Cost-plus is the floor of the ask, not the ask: a rocket built from the pilot's own minerals costs a few ISK, and listing it beside
	// an NPC selling it at 351 gives the margin away (npcanchor.go).
	price, _ = m.anchoredAsk(ctx, b.Tune, b.StationID, d.IndTypeID, price)
	// At a leak station the ask goes under the NPC beside it: the trip is to be the cheaper shelf on that dock.
	if m.isLeakStation(ctx, d.IndTypeID, b.StationID) {
		basis := 0.0
		if qty > 0 {
			basis = cost / float64(qty)
		}
		price = leakListPrice(price, m.stationNPCAsk(ctx, b.StationID, d.IndTypeID), basis)
	}
	// The same hookup miners and hunters have: a standing order of this product on this dock absorbs the run. The order keeps its own
	// price; the order sweep's repricing owns that, not the factory.
	if topped, terr := m.topUpOwnSellOrder(ctx, b, d.IndTypeID, itemID, qty); terr != nil {
		m.logBot(b.CharID, logError, "market", "topping up its %s order: %v — listing instead",
			d.IndTypeName, terr)
	} else if topped {
		m.noteShelfBuilt(ctx, b.CharID, b.StationID, d.IndTypeID, d.IndTypeName, d.IndEvidence, qty)
		m.settleDemand(ctx, b.CharID, d.IndTypeID, qty)
		m.industryClearProject(ctx, b, false)
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
		return saveState(ctx, m.db, st)
	}
	out, err := evedb.PlaceSellOrder(ctx, m.db, b.CharID, b.StationID,
		d.IndTypeID, itemID, price, qty, -1 /*station*/, 1, industrySellDays, false)
	if err != nil {
		return fmt.Errorf("sell: %w", err)
	}
	if out.ErrorMsg != "" {
		// Refused: the stock stays in the hangar and the claim goes back, not settled (nothing answered the want). A broker-fee refusal is the
		// exception: the wallet is spent on materials, so sell into the standing bid (distress.go) and settle what sold.
		if brokerFeeRefusal(out.ErrorMsg) {
			if sold, gross := m.distressSell(ctx, b, b.StationID, d.IndTypeID, itemID, qty); sold > 0 {
				m.noteShelfBuilt(ctx, b.CharID, b.StationID, d.IndTypeID, d.IndTypeName, d.IndEvidence, sold)
				m.settleDemand(ctx, b.CharID, d.IndTypeID, sold)
				m.logBot(b.CharID, logInfo, "industry",
					"could not afford to list %d× %s — sold %d into the local bid for %.0f ISK against %.0f ISK cost",
					qty, d.IndTypeName, sold, gross, cost)
				m.industryClearProject(ctx, b, sold < qty)
				st.Activity, st.Phase = "", string(TaskIdle)
				st.Data = idleData(st.Data)
				st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
				return saveState(ctx, m.db, st)
			}
		}
		m.logBot(b.CharID, logWarn, "market", "listing refused (%s) — leaving %d× %s in the hangar",
			out.ErrorMsg, qty, d.IndTypeName)
		m.industryClearProject(ctx, b, true)
		st.Activity, st.Phase = "", string(TaskIdle)
		st.Data = idleData(st.Data)
		st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
		return saveState(ctx, m.db, st)
	}
	where := "at its factory"
	if awayFromFactory {
		where = "at " + m.stationLabel(ctx, b.StationID)
	}
	m.logBot(b.CharID, logInfo, "industry",
		"listed %d× %s at %.2f ISK (%.0f ISK gross against %.0f ISK cost, %.0f ISK cash sunk, margin %.2f×) %s — good for %d days",
		qty, d.IndTypeName, price, price*float64(qty), cost, d.IndSpentISK, b.Tune.IndustryMargin, where, industrySellDays)
	m.bookBrokerFee(ctx, b, out.RestingOrderID, d.IndTypeID, d.IndTypeName, price, qty-out.FilledQty)
	// Register what is left resting so the order sweep can tell the shelf when it sells (tradebook.go). Cost basis is what the project spent,
	// with the print charged one project's share (industryCostBasis), so a fill books a real profit.
	if resting := qty - out.FilledQty; resting > 0 {
		unitCost := 0.0
		if qty > 0 {
			unitCost = cost / float64(qty)
		}
		m.registerOrder(ctx, b.CharID, out.RestingOrderID, d.IndTypeID, b.StationID, b.SystemID,
			d.IndTypeName, false, price, unitCost, resting, tradeDealBuild)
	}
	// What it just proved it can make here becomes a standing stock line (buildbook.go): the shelf is a promise about a station. Anything
	// that immediately filled counts as sold.
	m.noteShelfBuilt(ctx, b.CharID, b.StationID, d.IndTypeID, d.IndTypeName, d.IndEvidence, qty)
	if out.FilledQty > 0 {
		m.noteShelfSold(ctx, b.CharID, b.StationID, d.IndTypeID, out.FilledQty)
		// The listing crossed a bid already resting here. If that bid belongs to another rolebot, a factory sold its output to a pilot that will
		// fly it, so it goes in the peer ledger.
		m.notePeerFills(ctx, tradeEntry{
			CharID: b.CharID, TypeID: d.IndTypeID, TypeName: d.IndTypeName,
			Station: b.StationID, System: b.SystemID,
		}, out.Fills, false, "manufactured")
	}
	m.settleDemand(ctx, b.CharID, d.IndTypeID, qty)
	m.industryClearProject(ctx, b, false)
	st.Activity, st.Phase = "", string(TaskIdle)
	st.Data = idleData(st.Data)
	st.Data.LegEndsAt = clock.Unix() + m.stationDwell(b)
	return saveState(ctx, m.db, st)
}

// industryHaulToOutlet decides whether a finished run at the factory should be carried to a better counter and, if so, loads it and
// starts the trip; the sell phase resumes on arrival. Counters that qualify: a leak station (the roster has been paying an NPC for the
// goods, npcleak.go) and, for a durable run, a station where shoppers found the shelf short (stationdemand.go). hauled=false lists at
// the factory. With rolebots.outlet.enabled = 0 only leak stations count.
func (m *Mod) industryHaulToOutlet(ctx context.Context, b *botRun, itemID, qty int64, unitPrice float64) (bool, error) {
	d := &b.State.Data
	if b.ShipID == 0 || qty <= 0 {
		return false, nil
	}
	var rows []leakRow
	if leaks, err := m.npcLeaks(ctx); err == nil {
		rows = leaks.rows(d.IndTypeID)
	}
	outlets := m.outletEnabled(ctx)
	var demand []counterDemand
	durable := false
	if outlets {
		if cats, err := m.typeCategories(ctx); err == nil {
			switch cats[d.IndTypeID] {
			case catModule, catCharge, catDrone:
				durable = true
			}
		}
		if durable {
			demand = m.stationDemand(ctx).rows(d.IndTypeID)
		}
	}
	if len(rows) == 0 && len(demand) == 0 {
		return false, nil
	}
	within, err := m.systemsWithin(ctx, b.SystemID, rulesForBot(b), shopSoftMaxJumps)
	if err != nil {
		return false, nil
	}
	var stop outletStop
	if outlets {
		var ok bool
		if stop, ok = outletSellStop(rows, demand, unitPrice, qty, durable, within, b.StationID); !ok {
			return false, nil
		}
	} else {
		leak, j, ok := leakSellStop(rows, within)
		if !ok || leak.StationID == b.StationID {
			return false, nil
		}
		stop = outletStop{StationID: leak.StationID, SystemID: leak.SystemID, Jumps: j, LeakISK: leak.ISK}
	}
	jumps := stop.Jumps
	free, err := m.holdFreeM3(ctx, b.CharID, b.ShipID)
	if err != nil {
		return false, nil
	}
	vol, err := evedb.GetTypeVolume(ctx, m.db, d.IndTypeID)
	if err != nil || vol <= 0 || vol*float64(qty) > free {
		return false, nil
	}
	moved, err := m.moveHangarUnitsToCargo(ctx, b, itemID, qty, free)
	if err != nil {
		return false, err
	}
	if moved < qty {
		// The hold measured wrong; put back what went aboard and list here.
		if perr := m.moveCargoToHangar(ctx, b, d.IndTypeID); perr != nil {
			return false, perr
		}
		return false, nil
	}
	if stop.Pull > 0 {
		m.logBot(b.CharID, logInfo, "industry",
			"carrying %d× %s %d jump(s) to %s, where shoppers came for it this week and would take about %.0f "+
				"— it will list there", qty, d.IndTypeName, jumps, m.stationLabel(ctx, stop.StationID),
			stop.Pull)
	} else {
		m.logBot(b.CharID, logInfo, "industry",
			"carrying %d× %s %d jump(s) to %s, where pilots paid the NPCs %.0f ISK for it over %d days — "+
				"it will list under them there", qty, d.IndTypeName, jumps, m.stationLabel(ctx, stop.StationID),
			stop.LeakISK, npcLeakWindowDays)
	}
	return true, m.beginTravel(ctx, b, stop.SystemID, stop.StationID, string(TaskIndSell))
}

// isLeakStation reports whether the roster has been buying this type off the
// NPCs at this station (at the leakMinISK bar).
func (m *Mod) isLeakStation(ctx context.Context, typeID, stationID int64) bool {
	leaks, err := m.npcLeaks(ctx)
	if err != nil {
		return false
	}
	for _, r := range leaks.rows(typeID) {
		if r.StationID == stationID && r.ISK >= leakMinISK {
			return true
		}
	}
	return false
}

// stationNPCAsk is the cheapest NPC seeder ask for a type on this dock, else
// the cheapest in its region (npcAskAnchor), 0 when no NPC sells it.
func (m *Mod) stationNPCAsk(ctx context.Context, stationID, typeID int64) float64 {
	var p sql.NullFloat64
	err := m.db.QueryRowContext(ctx, `
		SELECT MIN(price) FROM market_orders
		WHERE stationID = ? AND typeID = ? AND bid = 0 AND orderState = 1 AND volRemaining > 0
		  AND price > 0 AND charID IN (?, ?)`,
		stationID, typeID, evedb.SeederCharID, evedb.AdminSeederCharID).Scan(&p)
	if err == nil && p.Valid && p.Float64 > 0 {
		return p.Float64
	}
	return m.npcAskAnchor(ctx, stationID, typeID)
}

// leakListPrice is the ask at a leak station: the anchored ask, or leakUndercut times the NPC ask on the dock when that is lower, never
// under the run's own cost basis. No NPC ask leaves the anchored ask alone. Pure.
func leakListPrice(anchored, npcAsk, basis float64) float64 {
	price := anchored
	if npcAsk > 0 {
		if u := roundISK(leakUndercut * npcAsk); u < price {
			price = u
		}
	}
	if basis > 0 && price < basis {
		price = roundISK(basis)
	}
	return price
}

// detailIndustry is the panel's one-line description of an industry phase.
func detailIndustry(m *Mod, ctx context.Context, st *SimState) string {
	d := st.Data
	if d.IndTypeName == "" {
		return ""
	}
	switch st.Phase {
	case string(TaskIndBuyBPO):
		return fmt.Sprintf("buying the %s blueprint", d.IndTypeName)
	case string(TaskIndRefine):
		return fmt.Sprintf("gathering materials for %d run(s) of %s (mining trip %d/%d)",
			d.IndRuns, d.IndTypeName, d.IndMiningTrips, industryMaxMiningTrips)
	case string(TaskIndBuyMats):
		return fmt.Sprintf("buying minerals for %s", d.IndTypeName)
	case string(TaskIndInstall):
		return fmt.Sprintf("installing %d run(s) of %s", d.IndRuns, d.IndTypeName)
	case string(TaskIndCollect):
		return fmt.Sprintf("collecting its %s from the line", d.IndTypeName)
	case string(TaskIndSell):
		return fmt.Sprintf("listing %s for sale", d.IndTypeName)
	case string(TaskIndCorpJob):
		return fmt.Sprintf("running %d run(s) of %s for its corporation", d.IndRuns, d.IndTypeName)
	case string(TaskIndCorpMove):
		return "moving its corporation's factory stock to the new factory"
	case string(TaskIndPrintMove):
		return fmt.Sprintf("carrying %s between its corporation's bases", d.IndTypeName)
	}
	return ""
}
