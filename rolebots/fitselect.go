package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// Which ship a bot flies: every fit is resolved against the pilot's skills and grid for the job
// (fiteffective.go), kept if it still does the work on a wallet covering the build twice over, and
// ranked by score (fitscore.go). See docs/comment-notes-b09.md#fit-selection.
const (
	// fitCostSlippage pads the estimate: the trip pays whatever the nearest
	// seller asks, not the cheapest ask in the cluster, and walks up price
	// levels for quantity.
	fitCostSlippage = 1.20
	// fitAffordMultiple is how many times over a fit must be affordable
	// before the bot picks it.
	fitAffordMultiple = 2.0
	// upgradeAffordMultiple is the (higher) bar for abandoning a ship that
	// already works. The gap between the two is hysteresis: without it one
	// good haul upgrades the bot and one bad loss downgrades it, forever.
	upgradeAffordMultiple = 3.0
	// upgradeScoreMargin is how much better a fit must score before it is worth swapping a working
	// ship for. Without a margin, two near-identical fits would trade places whenever a price moved.
	upgradeScoreMargin = 1.15
	// fitCostTTL is how long an estimate is reused. Fit contents change
	// only when an operator edits them; market prices drift slowly, and
	// this runs on every plan pass for every bot.
	fitCostTTL = 90 * time.Second
)

// fitShip is the ship a character currently holds for one fit. It is
// ownership, not permission: a row exists because procurement bought a
// hull, and it is gone the moment that hull is.
type fitShip struct {
	CharacterID int64
	FitID       int64
	ShipItemID  int64
	ShipExists  bool
}

// fitCandidate is one fit in the running for a job.
type fitCandidate struct {
	s     *fitShip
	t     *fitTemplate
	cost  float64      // filled in by selectFitForActivity as it prices each one
	judge fitJudgement // filled in the same pass
	// escrow is the listed ship that set cost, when one undercut the market (escrow_ship.go): the fit
	// is bought as that crate plus the lines it lacks.
	escrow *escrowShipStop
	// reserved marks a live ship this job leaves alone (rackReservedFor). It
	// is neither flown nor bought over; selectFit falls back to it only for
	// a pilot with no other way to work.
	reserved bool
}

type fitCostEntry struct {
	cost float64
	at   time.Time
}

var (
	fitCostMu sync.Mutex
	// Keyed by the fit and the region set it was priced against: a valuation is only true inside the
	// scope that produced it (materialize.go), so two pilots in different space must not share one.
	fitCostCache = map[fitCostKey]fitCostEntry{}
)

// fitCost estimates the ISK to build a template from scratch: hull, modules, charges, cargo and
// drones, each at the cheapest live ask this pilot can reach (basePrice when nothing in scope is
// listed), plus slippage. It prices the template as given, so an effective fit is priced only for
// what the pilot can switch on. b may be nil: the panel passes nil to price a listed fit from the catalogue.
func (m *Mod) fitCost(ctx context.Context, b *botRun, t *fitTemplate) (float64, error) {
	return m.fitCostIn(ctx, m.valuationRegions(ctx, b), t)
}

// fitCostKey is a fit-cost cache entry's identity: the fit plus the valuation scope it was priced
// in. It embeds fitCacheKey so forgetFitDerived can find a purged pilot's entries by charID.
type fitCostKey struct {
	fitCacheKey
	regions string
}

// fitCostIn is fitCost against an explicit region set, for callers that resolve their own scope
// (the skill planner has a charID and no botRun). An empty set prices from the catalogue.
func (m *Mod) fitCostIn(ctx context.Context, regions []int64, t *fitTemplate) (float64, error) {
	fitID, fit := t.FitID, t.Fit
	key := fitCostKey{fitCacheKey: keyFor(t), regions: snapKey(regions)}
	if fitID != 0 {
		fitCostMu.Lock()
		e, ok := fitCostCache[key]
		fitCostMu.Unlock()
		if ok && clock.Now().Sub(e.at) < fitCostTTL {
			return e.cost, nil
		}
	}
	total, _, err := m.estimatePriceRefIn(ctx, regions, fit.HullTypeID)
	if err != nil {
		return 0, fmt.Errorf("fitCost hull: %w", err)
	}
	add := func(typeID, qty int64) error {
		p, _, err := m.estimatePriceRefIn(ctx, regions, typeID)
		if err != nil {
			return err
		}
		total += p * float64(qty)
		return nil
	}
	for _, mod := range fit.Modules {
		if err := add(mod.TypeID, 1); err != nil {
			return 0, fmt.Errorf("fitCost %s: %w", mod.TypeName, err)
		}
		if mod.ChargeTypeID != 0 && mod.ChargeQty > 0 {
			if err := add(mod.ChargeTypeID, mod.ChargeQty); err != nil {
				return 0, fmt.Errorf("fitCost %s: %w", mod.ChargeName, err)
			}
		}
	}
	for _, c := range fit.Cargo {
		if err := add(c.TypeID, c.Qty); err != nil {
			return 0, fmt.Errorf("fitCost %s: %w", c.TypeName, err)
		}
	}
	for _, d := range fit.Drones {
		if err := add(d.TypeID, d.Qty); err != nil {
			return 0, fmt.Errorf("fitCost %s: %w", d.TypeName, err)
		}
	}
	total *= fitCostSlippage
	if fitID != 0 {
		fitCostMu.Lock()
		fitCostCache[key] = fitCostEntry{cost: total, at: clock.Now()}
		fitCostMu.Unlock()
	}
	return total, nil
}

// hullOnlyVariant is the same hull with nothing on it. Manufacturing happens at a station console
// and the ship is only a box, so industry also considers every hull bare: it scores lower, so it
// wins only when nothing else is within reach. The FitID is kept because ownership is recorded
// against it, so a bot that gets rich finishes the fit instead of buying a second ship.
func hullOnlyVariant(t *fitTemplate) *fitTemplate {
	if t == nil || t.HullOnly {
		return nil
	}
	if len(t.Fit.Modules) == 0 && len(t.Fit.Drones) == 0 && len(t.Fit.Cargo) == 0 {
		return nil // already bare; a second identical candidate helps nobody
	}
	bare := *t.Fit
	bare.Modules, bare.Cargo, bare.Drones = nil, nil, nil
	bare.CPUUsed, bare.PGUsed = 0, 0
	out := *t
	out.Fit = &bare
	out.HullOnly = true
	out.Dropped = append(append([]string{}, t.Dropped...), "everything but the hull (hauling only)")
	return &out
}

// fitTemplate is a fit plus who it was resolved for and at what job: a working value, namely what
// the generator built, what the trimmer cut down and what the scorer judged.
type fitTemplate struct {
	FitID         int64
	Name          string
	HullTypeID    int64
	RepairTrigger string
	Fit           *Fit
	Created       string

	EffectiveFor int64    // characterID this copy was trimmed for
	Job          string   // the job it was resolved and scored against
	Dropped      []string // lines left off, in the order they came off
	// HullOnly marks the bare-hull reading: the same hull with nothing bolted to it, for a pilot too
	// poor for the fit who can still haul. It is part of the derived-cache key because a hull and the
	// fit around it price and score differently.
	HullOnly bool
	// Style is the intent the fit was generated under and Lean what its guns
	// came out as (fitstyle.go). judgeFit credits a match; the ESP fit page
	// shows both. engageAuto / leanUnknown on everything not generated.
	Style engageStyle
	Lean  combatLean
}

// ownedShipTemplate reads a ship the pilot actually owns, its hull and whatever is bolted to it,
// as a template. A ship is a fact where a fit is a plan: opening the ship stays true after a module
// is lost, sold, or never bought. Returns nil when the ship has gone.
func (m *Mod) ownedShipTemplate(ctx context.Context, charID, shipItemID, hullTypeID int64) *fitTemplate {
	if shipItemID == 0 {
		return nil
	}
	name, err := typeName(ctx, m.db, hullTypeID)
	if err != nil {
		return nil
	}
	fit := &Fit{HullTypeID: hullTypeID, HullName: name, RepairTrigger: "structure"}
	rows, err := m.db.QueryContext(ctx, `
		SELECT e.typeID, t.typeName, e.flag
		FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.locationID = ? AND e.flag BETWEEN ? AND ?
		ORDER BY e.flag`, shipItemID, flagLoFirst, flagHiFirst+7)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var mod FitModule
		if err := rows.Scan(&mod.TypeID, &mod.TypeName, &mod.Flag); err != nil {
			return nil
		}
		if attrs, err := evedb.GetTypeAttributes(ctx, m.db, mod.TypeID); err == nil {
			mod.CPU, mod.PG = attrs[attrCPUUse], attrs[attrPGUse]
		}
		fit.Modules = append(fit.Modules, mod)
		fit.CPUUsed += mod.CPU
		fit.PGUsed += mod.PG
	}
	if rows.Err() != nil {
		return nil
	}
	return &fitTemplate{
		FitID: generatedFitID(hullTypeID), Name: name, HullTypeID: hullTypeID,
		RepairTrigger: fit.RepairTrigger, Fit: fit, EffectiveFor: charID,
	}
}

// ── ownership ───────────────────────────────────────────────────────────────
//
// Ownership is keyed by fitID. A generated fit's ID is the negative of its hull type
// (generatedFitID), so a row in rolebot_fit_ships reads as this pilot's ship built on hull X.
// There is no fit pool: a fit is built for the pilot that will fly it (fitgen.go).

// shipsForChar maps fitID to shipItemID for every hull this character currently holds against a fit.
func shipsForChar(ctx context.Context, db *sql.DB, charID int64) (map[int64]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT fs.fitID, fs.shipItemID FROM rolebot_fit_ships fs
		  WHERE fs.characterID = ? AND fs.shipItemID <> 0
		    -- a ship packed in an escrow or courier crate is for sale or in
		    -- transit, not in service (two primary-key lookups)
		    AND NOT EXISTS (SELECT 1 FROM entity s
		                      JOIN entity c ON c.itemID = s.locationID
		                     WHERE s.itemID = fs.shipItemID AND c.customInfo = 'courier_escrow')`, charID)
	if err != nil {
		return nil, fmt.Errorf("shipsForChar: %w", err)
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var fitID, shipID int64
		if err := rows.Scan(&fitID, &shipID); err != nil {
			return nil, fmt.Errorf("shipsForChar scan: %w", err)
		}
		out[fitID] = shipID
	}
	return out, rows.Err()
}

// setFitShip records (or forgets, with shipItemID 0) the hull a character
// holds for a fit.
func setFitShip(ctx context.Context, db *sql.DB, charID, fitID, shipItemID int64) error {
	if shipItemID == 0 {
		_, err := db.ExecContext(ctx,
			`DELETE FROM rolebot_fit_ships WHERE characterID = ? AND fitID = ?`, charID, fitID)
		if err != nil {
			return fmt.Errorf("setFitShip clear: %w", err)
		}
		return nil
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_fit_ships (characterID, fitID, shipItemID) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE shipItemID = VALUES(shipItemID)`, charID, fitID, shipItemID)
	if err != nil {
		return fmt.Errorf("setFitShip: %w", err)
	}
	return nil
}

// entityExists is the is-this-hull-still-a-thing check the ship rows cannot answer alone: a
// destroyed ship leaves the row behind.
func entityExists(ctx context.Context, db *sql.DB, itemID, ownerID int64) bool {
	var one int64
	return db.QueryRowContext(ctx,
		`SELECT 1 FROM entity WHERE itemID = ? AND ownerID = ?`, itemID, ownerID).Scan(&one) == nil
}

// boardedShip is the ship a character is sitting in right now, or 0. character_ has no shipID
// column: the character's own entity row has the ship as its locationID, and a locationID equal to
// the station means it is unboarded. This is the read loadRunsWhere makes to fill botRun.ShipID.
func boardedShip(ctx context.Context, db *sql.DB, charID int64) int64 {
	var loc, station int64
	if db.QueryRowContext(ctx, `
		SELECT COALESCE(ce.locationID, 0), COALESCE(c.stationID, 0)
		FROM character_ c
		LEFT JOIN entity ce ON ce.itemID = c.characterID
		WHERE c.characterID = ?`, charID).Scan(&loc, &station) != nil {
		return 0
	}
	if loc == 0 || loc == station {
		return 0
	}
	if !entityExists(ctx, db, loc, charID) {
		return 0
	}
	return loc
}

// shipForActivity is the "which ship is this bot flying" view for callers
// that must not spend money to answer: the live hull it holds that is
// best at the work in hand. Returns nil when it holds none that can do
// the job at all.
func (m *Mod) shipForActivity(ctx context.Context, charID int64, activity string) (*fitShip, *fitTemplate, error) {
	return m.shipForJob(ctx, charID, m.jobForPilot(ctx, charID, activity))
}

// shipForJob is shipForActivity in the module's own terms.
func (m *Mod) shipForJob(ctx context.Context, charID int64, job string) (*fitShip, *fitTemplate, error) {
	ships, err := shipsForChar(ctx, m.db, charID)
	if err != nil {
		return nil, nil, err
	}
	if len(ships) == 0 {
		return nil, nil, nil
	}
	pool, err := m.candidatePool(ctx, charID, job)
	if err != nil {
		return nil, nil, err
	}
	skills, err := m.charSkillsFor(ctx, charID)
	if err != nil {
		return nil, nil, fmt.Errorf("skills: %w", err)
	}
	who := m.charPilotRace(ctx, charID)
	var best *fitCandidate
	for _, stored := range pool {
		shipID, ok := ships[stored.FitID]
		if !ok || !entityExists(ctx, m.db, shipID, charID) {
			continue
		}
		if rackReservedFor(job, m.shipRack(ctx, shipID), true) {
			continue // the logi seat's ship, or another job's offered to it
		}
		t, err := m.effectiveFitWithSkills(ctx, charID, job, stored, skills)
		if err != nil {
			return nil, nil, err
		}
		if t == nil {
			continue // hull out of reach: it cannot be in this ship
		}
		j, err := m.judgeFit(ctx, t, skills, who)
		if err != nil {
			return nil, nil, err
		}
		if j.Unusable {
			continue // it owns this hull, but not for this work
		}
		c := &fitCandidate{
			s:     &fitShip{CharacterID: charID, FitID: t.FitID, ShipItemID: shipID, ShipExists: true},
			t:     t,
			judge: j,
		}
		if best == nil || c.judge.Score > best.judge.Score {
			best = c
		}
	}
	if best == nil {
		return nil, nil, nil
	}
	return best.s, best.t, nil
}

// canWorkJob reports whether any fit in the module, resolved against this pilot's skills and grid,
// would still do this job. Money is not asked: this answers whether the work is open to the pilot
// at all, which the appetite draw needs (appetite.go).
func (m *Mod) canWorkJob(ctx context.Context, charID int64, job string) (bool, error) {
	pool, err := m.candidatePool(ctx, charID, job)
	if err != nil {
		return false, err
	}
	skills, err := m.charSkillsFor(ctx, charID)
	if err != nil {
		return false, fmt.Errorf("skills: %w", err)
	}
	who := m.charPilotRace(ctx, charID)
	for _, stored := range pool {
		eff, err := m.effectiveFitWithSkills(ctx, charID, job, stored, skills)
		if err != nil || eff == nil {
			continue
		}
		if j, err := m.judgeFit(ctx, eff, skills, who); err == nil && !j.Unusable && j.Score > 0 {
			return true, nil
		}
	}
	return false, nil
}

// ── selection ───────────────────────────────────────────────────────────────

// selectFitForActivity picks which fit a bot flies for an activity now. A live ship settles it
// (best score first) unless the bot is docked and can comfortably afford something clearly better,
// in which case that fit is returned with no ship and the caller's procure path buys it. A nil
// ship and template mean no fit suits this pilot and job; other dead ends are errors.
func (m *Mod) selectFitForActivity(ctx context.Context, b *botRun, activity string) (*fitShip, *fitTemplate, error) {
	return m.selectFit(ctx, b, activity, true)
}

// selectFlyableFit is the same choice for a caller that cannot buy. The upgrade arm returns a fit
// with no ship, a purchase proposal that only a caller who goes on to procure can use. gangSeatReady
// buys nothing (gangplan.go), so it must read the pilot's own ship through this function, or it
// excuses a pilot that owns a good hull.
func (m *Mod) selectFlyableFit(ctx context.Context, b *botRun, activity string) (*fitShip, *fitTemplate, error) {
	return m.selectFit(ctx, b, activity, false)
}

// careerSeat reports whether an activity is this pilot's career: the mining job for a pilot whose
// mining appetite is its strongest. Pure.
func careerSeat(tn Tuning, activity string) bool {
	if jobFor(activity) != JobMining {
		return false
	}
	a := tn.MiningAppetite
	return a > 0 && a >= tn.ViolenceAppetite && a >= tn.IndustryAppetite &&
		a >= tn.TradeAppetite && a >= tn.HaulAppetite && a >= tn.ResearchAppetite
}

// upgradeAffordFor is the wallet multiple an upgrade must clear. The ordinary bar is the hysteresis
// one (UpgradeAffordMultiple, 3x); a hull for the pilot's own career clears at the first-buy bar
// (FitAffordMultiple, 2x), so a miner can save for a mining cruiser. A combat pilot's hull swaps
// keep the hysteresis.
func upgradeAffordFor(tn Tuning, career bool) float64 {
	if career {
		return tn.FitAffordMultiple
	}
	return tn.UpgradeAffordMultiple
}

// seatIsDisposable reports whether the pilot sits in a disposable seat for
// this job: a gun seat of a gank fleet (disposableSeat), judged under a combat
// lens. The member row carries no Disposable, so it is read off the gang's
// doctrine kind. Any other pilot, job or gang reads false.
func (m *Mod) seatIsDisposable(ctx context.Context, charID int64, job string) bool {
	if job != JobCombat && job != JobPiracy {
		return false
	}
	g, mem, err := gangForChar(ctx, m.db, charID)
	if err != nil || g == nil || mem == nil {
		return false
	}
	return disposableSeat(g.DoctrineKind, mem.Role)
}

// sortDisposable orders a judged, priced candidate list cheapest-first among
// the fits that do the job, with everything unusable, unscored or unpriced
// behind them in their original order. Pure.
func sortDisposable(cands []*fitCandidate) {
	able := func(c *fitCandidate) bool { return !c.judge.Unusable && c.judge.Score > 0 && c.cost > 0 }
	sort.SliceStable(cands, func(i, j int) bool {
		ai, aj := able(cands[i]), able(cands[j])
		if ai != aj {
			return ai
		}
		if !ai {
			return false
		}
		return cands[i].cost < cands[j].cost
	})
}

func (m *Mod) selectFit(ctx context.Context, b *botRun, activity string, mayUpgrade bool) (*fitShip, *fitTemplate, error) {
	job := m.jobForPilot(ctx, b.CharID, activity)
	pool, err := m.candidatePool(ctx, b.CharID, job)
	if err != nil {
		return nil, nil, err
	}
	if len(pool) == 0 {
		return nil, nil, nil
	}
	ships, err := shipsForChar(ctx, m.db, b.CharID)
	if err != nil {
		return nil, nil, err
	}
	skills, err := m.charSkillsFor(ctx, b.CharID)
	if err != nil {
		return nil, nil, fmt.Errorf("skills: %w", err)
	}
	who := m.charPilotRace(ctx, b.CharID)
	bal, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return nil, nil, fmt.Errorf("wallet: %w", err)
	}

	cands := make([]*fitCandidate, 0, len(pool))
	// One horizon for every listed ship this pass weighs, walked only if a
	// candidate's hull is listed at all.
	var escrowReach map[int64]int
	reach := func() map[int64]int {
		if escrowReach == nil {
			escrowReach = m.escrowShipReach(ctx, b)
		}
		return escrowReach
	}
	consider := func(t *fitTemplate) {
		c := &fitCandidate{
			t: t,
			s: &fitShip{CharacterID: b.CharID, FitID: t.FitID},
		}
		if shipID, ok := ships[t.FitID]; ok {
			c.s.ShipItemID = shipID
			c.s.ShipExists = entityExists(ctx, m.db, shipID, b.CharID)
			// One read per owned hull, and only for a pilot the rule can touch.
			if logiPilot := wantsLogiHull(b.Tune); c.s.ShipExists && (job == JobLogi || logiPilot) {
				c.reserved = rackReservedFor(job, m.shipRack(ctx, shipID), logiPilot)
			}
		}
		if j, err := m.judgeFit(ctx, t, skills, who); err == nil {
			c.judge = j
		}
		if cost, err := m.fitCost(ctx, b, t); err == nil {
			c.cost = cost
		}
		// A ship it does not own yet may be for sale ready-fitted: a cheaper way to the same ship, never a
		// better one. It moves the price the wallet gates on, not the score. A bare hull wants nothing from the crate.
		if !c.s.ShipExists && c.cost > 0 && !t.HullOnly {
			if es := m.bestEscrowShip(ctx, b, t.Fit.HullTypeID, fitNeeds(t.Fit), c.cost, reach); es != nil {
				c.cost, c.escrow = es.Outlay, es
			}
		}
		cands = append(cands, c)
	}
	for _, stored := range pool {
		// Resolve first: what is left of this fit once the lines this pilot cannot online come off and the
		// rest is trimmed to its CPU and powergrid for this job. Score, price and shopping list follow.
		t, err := m.effectiveFitWithSkills(ctx, b.CharID, job, stored, skills)
		if err != nil {
			return nil, nil, err
		}
		if t == nil {
			continue // cannot board the hull; nothing to consider
		}
		consider(t)
		// Manufacturing only asks the ship to be a box, so the bare hull is an answer for it alone: for
		// any other job an empty hull would mine or fight nothing.
		if activity == ActIndustry {
			if bare := hullOnlyVariant(t); bare != nil {
				consider(bare)
			}
		}
	}
	// Best first: every decision below reads down this list, so the first
	// match is always the best match.
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].judge.Score > cands[j].judge.Score })
	// ...except for a disposable seat, a gank striker's, where the op spends the hull and the cheapest
	// fit that still does the job is best. The one place price is a merit.
	if m.seatIsDisposable(ctx, b.CharID, job) {
		sortDisposable(cands)
	}
	cands = m.narrowToHaulerSeat(ctx, b.CharID, job, cands)

	// The ship it already has, if any. A flying hull is not re-validated against skills (they only go
	// up) but is against the job: ownership is per fitID and a fit resolves differently per job, so a
	// bare hull bought as a hauler must not be offered back as the mining ship.
	var flying *fitCandidate
	for _, c := range cands {
		if c.s.ShipExists && !c.reserved && !c.judge.Unusable && c.judge.Score > 0 {
			flying = c
			break
		}
	}

	// What it could buy: flyable, priced, covered by the wallet with a replacement fund. Unusable
	// means it does not do the job; score 0 means it could not be judged; a cooling-down fit is skipped.
	eligible := func(c *fitCandidate) bool {
		return !c.reserved && !c.judge.Unusable && c.judge.Score > 0 && c.cost > 0 &&
			!fitProcureCoolingDown(&b.State.Data, c.t.FitID)
	}
	var best, cheapest *fitCandidate
	for _, c := range cands {
		if !eligible(c) {
			continue
		}
		if cheapest == nil || c.cost < cheapest.cost {
			cheapest = c
		}
		// Two tests: the multiple is the replacement fund, and spendable is what procurement lets the pilot
		// pay out (broke.go). The first alone picks fits the trip cannot finish.
		if best == nil && bal >= c.cost*b.Tune.FitAffordMultiple && spendable(bal) >= c.cost {
			best = c
		}
	}

	// A pilot whose only way to work is the ship it set aside flies it: a logi rack is not worth a
	// pilot with no income. Never the other way round, a gun ship is no answer to the logi seat.
	if flying == nil && best == nil && job != JobLogi &&
		(cheapest == nil || spendable(bal) < cheapest.cost) {
		for _, c := range cands {
			if c.reserved && !c.judge.Unusable && c.judge.Score > 0 {
				flying = c
				break
			}
		}
	}

	if flying != nil {
		// Upgrade only from a station, only to something clearly better, and only when the wallet clears
		// the higher bar: mid-activity hull swaps and threshold flapping are worse than another lap.
		if mayUpgrade && b.StationID != 0 {
			// An upgrade is optional, so it leaves the next clone's price untouched (clone.go).
			upBal := afterCloneReserve(bal, m.cloneReserve(ctx, b.CharID))
			for _, c := range cands {
				if c.s.ShipExists || !eligible(c) {
					continue
				}
				if c.judge.Score <= flying.judge.Score*upgradeScoreMargin {
					break // sorted: nothing further down clears the margin either
				}
				if upBal >= c.cost*upgradeAffordFor(b.Tune, careerSeat(b.Tune, activity)) && spendable(upBal) >= c.cost {
					m.logBot(b.CharID, logInfo, "fit",
						"upgrading %s: wallet %.0f ISK covers %q (~%.0f ISK%s) %.1f× over and it scores %.0f against %.0f — buying it, %q stays parked",
						activity, bal, fitLabel(c.t), c.cost, m.escrowShipNote(ctx, c.escrow), bal/c.cost, c.judge.Score, flying.judge.Score, fitLabel(flying.t))
					return c.s, c.t, nil
				}
			}
		}
		return flying.s, flying.t, nil
	}

	if best != nil {
		m.logBot(b.CharID, logInfo, "fit",
			"%s: flying %q — %s (~%.0f ISK%s, wallet %.0f, affordable %.1f×)",
			activity, fitLabel(best.t), judgementText(best), best.cost, m.escrowShipNote(ctx, best.escrow), bal, bal/best.cost)
		return best.s, best.t, nil
	}
	// Nothing clears 2x. The cheapest fit it can fly is bought at face value (spendable, not the bare
	// balance), since no ship means no income. See docs/comment-notes-b09.md#cheapest-fit-floor.
	if cheapest != nil && spendable(bal) >= cheapest.cost {
		m.logBot(b.CharID, logInfo, "fit",
			"%s: only %q is within reach (~%.0f ISK%s, wallet %.0f) — buying it without a replacement fund",
			activity, fitLabel(cheapest.t), cheapest.cost, m.escrowShipNote(ctx, cheapest.escrow), bal)
		return cheapest.s, cheapest.t, nil
	}
	// Skint: the rookie floor (broke.go) grants the hull and issues its starter lines, so no estimate
	// above is a bill. It precedes the cheapest == nil arm, since an unsold rookie fit prices at nothing.
	if b.Broke {
		if c := brokeFitFallback(cands); c != nil {
			m.logBot(b.CharID, logInfo, "fit",
				"%s: nothing in reach of a %.0f ISK wallet — falling back to %q, which is free",
				activity, bal, fitLabel(c.t))
			return c.s, c.t, nil
		}
	}
	if cheapest == nil {
		// Nothing in the pool suits this pilot: no hull it can board, or nothing left of any once skills
		// and grid had their say. A roster fact, not a failure: the rotation skips the slot.
		return nil, nil, nil
	}
	// The gap reported is the price plus the floor, the balance the arm above needs to say yes
	// (floorShortfall, broke.go).
	return nil, nil, floorShortfall(
		fmt.Sprintf("any %s fit (cheapest is %q)", activity, fitLabel(cheapest.t)),
		cheapest.cost, bal)
}

// fitLabel names a fit for a log line, saying so when what is meant is
// the bare hull rather than the fit of the same name.
func fitLabel(t *fitTemplate) string {
	if t != nil && t.HullOnly {
		return t.Name + " (hull only)"
	}
	if t == nil {
		return "?"
	}
	return t.Name
}

// judgementText explains a score in the operator's language, including what the pilot leaves off
// the ship: a bot flying a half-fitted Kestrel should say so in the log.
func judgementText(c *fitCandidate) string {
	j := c.judge
	s := fmt.Sprintf("score %.0f (capability %.0f, skills %.0f%% into it", j.Score, j.Power, j.Affinity*100)
	if j.SameRace {
		s += ", own race's hull"
	}
	if n := len(c.t.Dropped); n > 0 {
		s += fmt.Sprintf(", %d line(s) left off: %s", n, droppedSummary(c.t.Dropped))
	}
	return s + ")"
}

// fitCostText renders an estimate for the panel.
func fitCostText(cost float64) string {
	if cost <= 0 {
		return "—"
	}
	return iskString(cost)
}
