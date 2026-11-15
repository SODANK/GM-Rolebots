package rolebots

import (
	"context"
	"fmt"
	"sync"
	"time"

	evedb "gurimoon/db"
)

// Clone economics. A clone grade is the insurance on a pilot's skill points: each grade
// covers a ceiling (invTypes group 23, attribute 419 skillPointsSaved) and anything above it
// is uninsured. A rolebot keeps a clone covering its total SP, upgrades when it outgrows
// the grade and buys a replacement after a pod kill.
// It buys at the home station when that offers cloning, else at the nearest station within
// its travel rules that does, and moves home to match. When it cannot afford the grade it
// needs it buys the best grade it can afford and logs that.
// Prices and pod-death SP loss are the core's (BuyCloneGrade, ApplyCloneDeath); cloneReserve
// and cloneUncovered are how a bot plans for them (docs/ROLEBOT_DESIGN.md §22).

const (
	// invTypes group holding the clone grades.
	cloneGroupID = 23
	// skillPointsSaved: the SP ceiling a grade covers.
	attrSkillPointsSaved = 419
	// skillPoints: per-skill SP on the skill's entity row.
	attrSkillPoints = 276

	// How far a bot will travel to install a clone when its home station has
	// no medical facility. Generous: being uninsured is worse than the trip.
	cloneSearchMaxJumps = 25

	// Re-check cadence. Clone cover changes only when SP crosses a grade
	// boundary or a pod dies, so a bot that is already covered should not pay
	// for the SP sum on every planning pass.
	cloneCheckInterval = 15 * time.Minute
)

// cloneGrade is one purchasable grade.
type cloneGrade struct {
	TypeID int64
	Name   string
	SP     int64
	Price  float64
}

var (
	cloneGradesOnce sync.Once
	cloneGradesList []cloneGrade
	cloneGradesErr  error

	cloneCheckedMu sync.Mutex
	cloneCheckedAt = map[int64]time.Time{}

	// cloneCover is each pilot's last clone verdict: whether its grade covers its SP, and
	// the price of the grade it needs (what a re-buy after a pod death costs).
	cloneCoverMu sync.Mutex
	cloneCover   = map[int64]cloneCoverRec{}
)

// cloneCoverRec is one pilot's cloneCover entry.
type cloneCoverRec struct {
	Covered bool
	Reserve float64
}

// noteCloneCover records the verdict cloneUpkeep reached.
func noteCloneCover(charID int64, covered bool, reserve float64) {
	cloneCoverMu.Lock()
	cloneCover[charID] = cloneCoverRec{Covered: covered, Reserve: reserve}
	cloneCoverMu.Unlock()
}

// markCloneSpent records a pod death: the pilot is on Alpha until cloneUpkeep buys cover.
func markCloneSpent(charID int64) {
	cloneCoverMu.Lock()
	rec := cloneCover[charID]
	rec.Covered = false
	cloneCover[charID] = rec
	cloneCoverMu.Unlock()
}

// cloneUncovered reports a pilot whose clone does not cover its SP. A pilot not yet
// checked reads covered, so a fresh process does not ground the whole roster.
func cloneUncovered(charID int64) bool {
	cloneCoverMu.Lock()
	rec, ok := cloneCover[charID]
	cloneCoverMu.Unlock()
	return ok && !rec.Covered
}

// cloneReserve is the ISK a pilot keeps back for its next clone: the price of the grade
// covering its SP. It reads the last cloneUpkeep verdict and works it out when there is none.
func (m *Mod) cloneReserve(ctx context.Context, charID int64) float64 {
	cloneCoverMu.Lock()
	rec, ok := cloneCover[charID]
	cloneCoverMu.Unlock()
	if ok {
		return rec.Reserve
	}
	grades, err := m.cloneGrades(ctx)
	if err != nil {
		return 0
	}
	sp, err := m.characterTotalSP(ctx, charID)
	if err != nil {
		return 0
	}
	if g := requiredGrade(grades, sp); g != nil {
		return g.Price
	}
	return 0
}

// afterCloneReserve is balance less the clone reserve, never below 0.
func afterCloneReserve(balance, reserve float64) float64 {
	if balance -= reserve; balance < 0 {
		return 0
	}
	return balance
}

func forgetCloneCover(charID int64) {
	cloneCoverMu.Lock()
	delete(cloneCover, charID)
	cloneCoverMu.Unlock()
}

// cloneGrades returns every clone grade ordered by the SP it covers, cheapest cover first.
// Loaded once: the table never changes at runtime. Prices come from invTypes.basePrice,
// which the core's boot migration (MigrateCloneGradePrices) writes before any bot plans.
func (m *Mod) cloneGrades(ctx context.Context) ([]cloneGrade, error) {
	cloneGradesOnce.Do(func() {
		rows, err := m.db.QueryContext(ctx, `
			SELECT t.typeID, t.typeName,
			       COALESCE(sp.valueFloat, sp.valueInt, 0),
			       COALESCE(t.basePrice, 0)
			FROM invTypes t
			LEFT JOIN dgmTypeAttributes sp
			       ON sp.typeID = t.typeID AND sp.attributeID = ?
			WHERE t.groupID = ?
			ORDER BY COALESCE(sp.valueFloat, sp.valueInt, 0)`,
			attrSkillPointsSaved, cloneGroupID)
		if err != nil {
			cloneGradesErr = fmt.Errorf("cloneGrades: %w", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var g cloneGrade
			var sp float64
			if err := rows.Scan(&g.TypeID, &g.Name, &sp, &g.Price); err != nil {
				cloneGradesErr = err
				return
			}
			g.SP = int64(sp)
			if g.SP <= 0 {
				continue // not a real grade
			}
			cloneGradesList = append(cloneGradesList, g)
		}
		cloneGradesErr = rows.Err()
	})
	return cloneGradesList, cloneGradesErr
}

// characterTotalSP sums the skill points across every skill the pilot has trained: the
// number a clone grade has to cover.
func (m *Mod) characterTotalSP(ctx context.Context, charID int64) (int64, error) {
	var total int64
	err := m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(COALESCE(ea.valueInt, 250)), 0)
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN entity_attributes ea ON ea.itemID = e.itemID AND ea.attributeID = ?
		WHERE e.ownerID = ? AND g.categoryID = 16 AND e.flag IN (7, 61)`,
		attrSkillPoints, charID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("characterTotalSP: %w", err)
	}
	return total, nil
}

// forgetCloneCheck drops a pilot's cached "already covered" stamp, so the next
// planning pass re-evaluates immediately. Called when a pod death spends the
// clone and the answer has certainly changed.
func forgetCloneCheck(charID int64) {
	cloneCheckedMu.Lock()
	delete(cloneCheckedAt, charID)
	cloneCheckedMu.Unlock()
}

// requiredGrade is the cheapest grade that covers sp. Returns the top grade
// when the pilot has outgrown even that.
func requiredGrade(grades []cloneGrade, sp int64) *cloneGrade {
	for i := range grades {
		if grades[i].SP >= sp {
			return &grades[i]
		}
	}
	if len(grades) > 0 {
		return &grades[len(grades)-1]
	}
	return nil
}

// affordableGrade is the best grade at or below need that the wallet covers.
// Nil when the pilot cannot afford even the bottom of the ladder.
func affordableGrade(grades []cloneGrade, need *cloneGrade, balance float64) *cloneGrade {
	var best *cloneGrade
	for i := range grades {
		if need != nil && grades[i].SP > need.SP {
			break
		}
		if grades[i].Price <= balance {
			best = &grades[i]
		}
	}
	return best
}

// ── station lookup ──────────────────────────────────────────────────────────

// stationHasCloning reports whether a station offers the Cloning service.
func (m *Mod) stationHasCloning(ctx context.Context, stationID int64) bool {
	var one int64
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM staStations s
		JOIN staOperationServices os ON os.operationID = s.operationID
		WHERE s.stationID = ? AND os.serviceID = ?
		LIMIT 1`, stationID, serviceCloning).Scan(&one)
	return err == nil
}

// serviceCloning is the staServices row for a medical bay.
const serviceCloning = 512

// cloningStationIn returns a station in sysID that offers cloning, preferring one whose
// owner the pilot does not despise, so a bot does not put its body in the hands of a
// corporation it hates if anything else is on offer.
func (m *Mod) cloningStationIn(ctx context.Context, sysID int64, prof *Profile) int64 {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.stationID, s.corporationID FROM staStations s
		JOIN staOperationServices os ON os.operationID = s.operationID
		WHERE s.solarSystemID = ? AND os.serviceID = ?`, sysID, serviceCloning)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var fallback int64
	for rows.Next() {
		var stationID, corpID int64
		if err := rows.Scan(&stationID, &corpID); err != nil {
			return fallback
		}
		if prof != nil && m.stationRefuses(ctx, stationID, prof.CharacterID, serviceCloning) {
			continue // the owner refuses this pilot its medical bay (stationaccess.go)
		}
		if fallback == 0 {
			fallback = stationID
		}
		if !m.despisesStationOwner(ctx, prof, corpID) {
			return stationID
		}
	}
	return fallback
}

// clonesHere reports whether a station has a medical bay its owner lets this pilot use.
func (m *Mod) clonesHere(ctx context.Context, stationID, charID int64) bool {
	return m.stationHasCloning(ctx, stationID) && !m.stationRefuses(ctx, stationID, charID, serviceCloning)
}

// despisesStationOwner reports whether the pilot's standings put this owner
// beyond the pale. Best-effort: a lookup failure never blocks a clone, because
// an uninsured pilot is a worse outcome than an distasteful landlord.
func (m *Mod) despisesStationOwner(ctx context.Context, prof *Profile, corpID int64) bool {
	if prof == nil || corpID == 0 {
		return false
	}
	despised := prof.DespisedFactions()
	if len(despised) == 0 {
		return false
	}
	// An NPC corporation's faction lives in crpNPCCorporations; the corporation table has no
	// factionID column.
	factionID := m.corpFactionID(ctx, corpID)
	if factionID == 0 {
		return false
	}
	for _, id := range despised {
		if id == factionID {
			return true
		}
	}
	return false
}

// industryAnchor is where an industrialist's body belongs: at its factory when that
// station has a medical bay, else at the nearest bay to the factory. ok is false for a pilot
// with no factory. The search starts from the factory, not from where the pilot stands:
// installClone moves home to the clone, so a generic search would drag its home away.
func (m *Mod) industryAnchor(ctx context.Context, b *botRun) (stationID, systemID int64, ok bool) {
	if b.Profile == nil || b.Profile.IndustryStationID == 0 {
		return 0, 0, false
	}
	base := b.Profile.IndustryStationID
	loc, err := evedb.GetStationLocation(ctx, m.db, base)
	if err != nil {
		return 0, 0, false
	}
	if m.stationHasCloning(ctx, base) {
		return base, loc.SolarSystemID, true
	}
	// No region preference: this search chooses where home will be (installClone moves it), so
	// a head start for the current region would keep the pilot where it is (residency.go).
	sysID, err := m.nearestWhere(ctx, loc.SolarSystemID, rulesForBot(b),
		nil, cloneSearchMaxJumps,
		func(s int64, _ sysMeta) bool {
			return m.cloningStationIn(ctx, s, b.Profile) != 0
		})
	if err != nil {
		return 0, 0, false
	}
	station := m.cloningStationIn(ctx, sysID, b.Profile)
	if station == 0 {
		return 0, 0, false
	}
	return station, sysID, true
}

// cloneStationFor picks where this pilot should install: its factory anchor
// if it has one, then its own home station when that has a medical bay,
// otherwise the nearest system within the bot's travel rules that has one.
// Returns the station and its system.
func (m *Mod) cloneStationFor(ctx context.Context, b *botRun) (stationID, systemID int64, err error) {
	// A pinned address beats everything, the factory anchor included. It is checked here since
	// installClone writes home to the clone's station. If the pinned bay is gone, fall through.
	if b.Profile != nil && b.Profile.HomePinned {
		if home := m.homeStationFor(ctx, b.CharID); home != 0 && m.clonesHere(ctx, home, b.CharID) {
			loc, lerr := evedb.GetStationLocation(ctx, m.db, home)
			if lerr == nil {
				return home, loc.SolarSystemID, nil
			}
		}
	}
	if station, sysID, ok := m.industryAnchor(ctx, b); ok && !m.stationRefuses(ctx, station, b.CharID, serviceCloning) {
		return station, sysID, nil
	}
	home := m.homeStationFor(ctx, b.CharID)
	if home != 0 && m.clonesHere(ctx, home, b.CharID) {
		loc, err := evedb.GetStationLocation(ctx, m.db, home)
		if err != nil {
			return 0, 0, fmt.Errorf("home station %d: %w", home, err)
		}
		return home, loc.SolarSystemID, nil
	}

	// Unpreferred, as in industryAnchor: this picks the next home, so nothing may argue for the
	// current one.
	sysID, err := m.nearestWhere(ctx, b.SystemID, rulesForBot(b),
		nil, cloneSearchMaxJumps,
		func(s int64, _ sysMeta) bool {
			return m.cloningStationIn(ctx, s, b.Profile) != 0
		})
	if err != nil {
		return 0, 0, fmt.Errorf("no station with a medical bay within %d jumps: %w", cloneSearchMaxJumps, err)
	}
	station := m.cloningStationIn(ctx, sysID, b.Profile)
	if station == 0 {
		return 0, 0, fmt.Errorf("medical bay in system %d vanished between lookups", sysID)
	}
	return station, sysID, nil
}

// ── the upkeep step ─────────────────────────────────────────────────────────

// cloneUpkeep keeps a bot's clone in step with its skill points. It returns true when it
// has taken over this planning pass: it just bought a clone and the caller should carry on,
// or the bot is travelling to a medical station. It is the first thing a pod victim does.
func (m *Mod) cloneUpkeep(ctx context.Context, b *botRun) (travelling bool, err error) {
	// Cheap gate: summing skill points costs a query, and cover only changes when SP crosses a
	// grade or a pod dies. The pod path clears the stamp, so a fresh corpse re-checks at once.
	cloneCheckedMu.Lock()
	last := cloneCheckedAt[b.CharID]
	cloneCheckedMu.Unlock()
	if clock.Now().Sub(last) < cloneCheckInterval {
		return false, nil
	}
	docked := b.StationID != 0

	grades, err := m.cloneGrades(ctx)
	if err != nil || len(grades) == 0 {
		return false, err
	}
	sp, err := m.characterTotalSP(ctx, b.CharID)
	if err != nil {
		return false, err
	}
	current, err := evedb.GetCloneTypeID(ctx, m.db, b.CharID)
	if err != nil {
		return false, err
	}
	need := requiredGrade(grades, sp)
	if need == nil {
		return false, nil
	}

	// Already covered?
	var currentSP int64
	for _, g := range grades {
		if g.TypeID == current {
			currentSP = g.SP
		}
		if g.TypeID == current && g.SP >= sp {
			noteCloneCover(b.CharID, true, need.Price)
			cloneCheckedMu.Lock()
			cloneCheckedAt[b.CharID] = clock.Now()
			cloneCheckedMu.Unlock()
			return false, nil
		}
	}
	// Uncovered until a purchase says otherwise.
	noteCloneCover(b.CharID, false, need.Price)

	// Uncovered: buy here when standing in a medical bay, unless a factory anchor is elsewhere.
	// A player corporation member is anchored on its base (corpposting.go postedCloneStation).
	anchor, anchorSys, anchored := m.postedCloneStation(ctx, b)
	if !anchored {
		anchor, anchorSys, anchored = m.industryAnchor(ctx, b)
	}
	if docked && m.stationHasCloning(ctx, b.StationID) && (!anchored || anchor == b.StationID) {
		return false, m.installClone(ctx, b, b.StationID, grades, need, sp, currentSP)
	}
	if anchored && anchor != b.StationID {
		m.logBot(b.CharID, logInfo, "clone",
			"%d SP needs a %s clone — going home to %s rather than installing where it stands",
			sp, need.Name, m.stationLabel(ctx, anchor))
		if err := m.beginTravel(ctx, b, anchorSys, anchor, "idle"); err != nil {
			return false, err
		}
		return true, nil
	}

	station, sysID, err := m.cloneStationFor(ctx, b)
	if err != nil {
		// Nowhere to install. Say so once per interval and let the bot work;
		// flying uninsured beats halting over it.
		cloneCheckedMu.Lock()
		cloneCheckedAt[b.CharID] = clock.Now()
		cloneCheckedMu.Unlock()
		m.logBot(b.CharID, logError, "clone",
			"%d SP uninsured and %v — carrying on without cover", sp, err)
		return false, nil
	}
	if station == b.StationID {
		return false, m.installClone(ctx, b, station, grades, need, sp, currentSP)
	}

	m.logBot(b.CharID, logInfo, "clone",
		"%d SP needs a %s clone — travelling to %s to install",
		sp, need.Name, m.stationLabel(ctx, station))
	if err := m.beginTravel(ctx, b, sysID, station, "idle"); err != nil {
		return false, err
	}
	return true, nil
}

// installClone buys the clone and makes this station the pilot's medical home. currentSP is
// what the clone it wears covers; a grade no better is not bought (the core refuses it).
func (m *Mod) installClone(ctx context.Context, b *botRun, stationID int64,
	grades []cloneGrade, need *cloneGrade, sp, currentSP int64) error {

	balance, err := evedb.GetCharBalance(ctx, m.db, b.CharID)
	if err != nil {
		return fmt.Errorf("clone: balance: %w", err)
	}

	// The clone is sized against what the wallet may commit (spendable, broke.go), not the
	// whole balance, so the pilot can still re-arm and re-fit. A lower grade keeps some cover.
	budget := spendable(balance)

	buy := need
	if need.Price > budget {
		// Cannot afford full cover. Take the best partial cover on offer
		// rather than flying with an Alpha clone on millions of SP.
		buy = affordableGrade(grades, need, budget)
		if buy == nil {
			cloneCheckedMu.Lock()
			cloneCheckedAt[b.CharID] = clock.Now()
			cloneCheckedMu.Unlock()
			m.logBot(b.CharID, logWarn, "clone",
				"cannot afford any clone grade (%s costs %.0f ISK, wallet holds %.0f and must keep %.0f back) — flying uninsured on %d SP",
				need.Name, need.Price, balance, walletFloorFor(balance), sp)
			return nil
		}
		m.logBot(b.CharID, logWarn, "clone",
			"cannot afford %s (%.0f ISK) out of a %.0f ISK wallet — settling for %s, %d of %d SP covered",
			need.Name, need.Price, budget, buy.Name, buy.SP, sp)
	}

	if buy.SP <= currentSP {
		// The best it can afford is no better than what it wears. Keep earning.
		cloneCheckedMu.Lock()
		cloneCheckedAt[b.CharID] = clock.Now()
		cloneCheckedMu.Unlock()
		m.logBot(b.CharID, logWarn, "clone",
			"cannot afford better than its current clone (%s costs %.0f ISK, %.0f spendable) — %d of %d SP covered",
			need.Name, need.Price, budget, currentSP, sp)
		return nil
	}

	// The core charges, journals (Clone Activation) and installs in one transaction, as for a
	// player at the medical window.
	if _, _, err := evedb.BuyCloneGrade(ctx, m.db, b.CharID, buy.TypeID); err != nil {
		if msg, ok := evedb.AsClientMsg(err); ok {
			// A refusal (the wallet moved since the read): try again next interval.
			cloneCheckedMu.Lock()
			cloneCheckedAt[b.CharID] = clock.Now()
			cloneCheckedMu.Unlock()
			m.logBot(b.CharID, logWarn, "clone", "buying %s refused (%s): %v", buy.Name, msg.Key, err)
			return nil
		}
		return fmt.Errorf("clone: buy %s: %w", buy.Name, err)
	}
	noteCloneCover(b.CharID, buy.SP >= sp, need.Price)
	// The clone is here, so the pilot wakes up here: home moves to match.
	m.setHomeStation(ctx, b, stationID)

	cloneCheckedMu.Lock()
	cloneCheckedAt[b.CharID] = clock.Now()
	cloneCheckedMu.Unlock()

	m.logBot(b.CharID, logInfo, "clone",
		"installed %s at %s (%d SP covered, %.0f ISK) — medical station and home now the same place",
		buy.Name, m.stationLabel(ctx, stationID), buy.SP, buy.Price)
	return nil
}

// setHomeStation is the one place a rolebot's home moves, and it moves one row: the
// character's medical station. Home and clone are the same fact (homeStationFor). A home
// that is not a school station also settles the pilot, so nothing looks at it again; a
// school-station home does not, since a green pilot buying its first clone has not moved.
func (m *Mod) setHomeStation(ctx context.Context, b *botRun, stationID int64) {
	m.setHomeStationByID(ctx, b.CharID, stationID)
}

// setHomeStationByID is setHomeStation for a pilot that is not being simulated: the admin
// panel's profile editor and the corporation page's address buttons. Every home writer must
// go through one of the two, or the residency invalidation is skipped.
func (m *Mod) setHomeStationByID(ctx context.Context, charID, stationID int64) {
	was := m.homeStationFor(ctx, charID)
	if err := evedb.SetHomeStationID(ctx, m.db, charID, stationID); err != nil {
		rlogf("rolebots: clone: set medical station %d: %v", stationID, err)
	} else if was != stationID {
		// The crowd census is cached for minutes, so pilots ranking against one stale count would
		// pick the same empty system. Every home move passes here, which keeps it honest.
		m.noteHomeMove(m.stationSystemID(ctx, was), m.stationSystemID(ctx, stationID))
	}
	// A move changes the region the pilot's economy is scoped to (residency.go); this funnel
	// is the only place the cached answer can go stale.
	forgetHomeRegion(charID)
	if !m.schoolsSoft(ctx).Stations[stationID] {
		m.markMovedOut(charID)
	}
}

// adoptHome moves the pilot in and says so. why is a format string taking
// the station's label and nothing else.
func (m *Mod) adoptHome(ctx context.Context, b *botRun, stationID int64, why string) error {
	m.setHomeStation(ctx, b, stationID)
	m.logBot(b.CharID, logInfo, "clone", why, m.stationLabel(ctx, stationID))
	return nil
}

func init() {
	registerCharForget("cloneCheckedAt", forgetCloneCheck)
	registerCharForget("cloneCover", forgetCloneCover)
}