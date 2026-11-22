package rolebots

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// The roles power: who helps run the corporation and who leads its fleets.
//   - Corporation roles: Director, Personnel Manager and Accountant, granted on
//     the member's chrCorporationRoles row (the bits the corp window writes). The
//     CEO fills a quota that grows with the corporation (Charter.DirectorPer,
//     PersonnelPer) from its most trusted members, and demotes as readily.
//     Humans are candidates too (corpmembers.go); Director and Accountant carry
//     wallet access, so nobody holds either before officerTenureDays.
//   - Fleet seats: promoteCorp (corprank.go) runs for a bot-run corporation over
//     the fleets its tags name and leaves hand-appointed seats alone.
// Titles (corptitles.go) are then synced from both.

// ceoRoleMask is the role bits this power owns. Anything else on a member's
// row is left exactly as it was.
const ceoRoleMask = corp.RoleDirector | corp.RolePersonnelManager | corp.RoleAccountant

// ceoSeatPassEverySec: the fleet-seat pass is the expensive half (a skill read
// per member), so a CEO runs it at most this often however often it thinks.
const ceoSeatPassEverySec = 24 * 3600

// officerTenureDays is how long a member must have served before it may hold
// an office with wallet access (Director, Accountant).
const officerTenureDays = 30

// ceoHierarchyFrom is the membership at which a corporation starts to have
// officers at all; below it the CEO runs everything itself.
const ceoHierarchyFrom = 5

var (
	seatPassMu sync.Mutex
	seatPassAt = map[int64]int64{}
)

// rolesRecheckSec: between Corporate Reviews the roles pass re-runs when the membership has
// changed since its last run, at most this often (rolesRecheckDue).
const rolesRecheckSec = 2 * 3600

// rolesPassMark is the membership the roles pass last dealt for, and when. In memory: the first
// think after a restart runs the pass once.
type rolesPassMark struct {
	members int
	at      int64
}

var (
	rolesPassMu   sync.Mutex
	rolesPassLast = map[int64]rolesPassMark{}
)

// rolesRecheckDue reports whether the roles pass should run outside the weekly review: never run
// since boot, or the membership moved and the last run is rolesRecheckSec old.
func rolesRecheckDue(corpID int64, members int) bool {
	rolesPassMu.Lock()
	defer rolesPassMu.Unlock()
	mk, ok := rolesPassLast[corpID]
	if !ok {
		return true
	}
	return mk.members != members && clock.Unix()-mk.at >= rolesRecheckSec
}

// noteRolesPass stamps a roles pass and answers whether the membership changed since the last.
func noteRolesPass(corpID int64, members int) (changed bool) {
	rolesPassMu.Lock()
	defer rolesPassMu.Unlock()
	mk, ok := rolesPassLast[corpID]
	rolesPassLast[corpID] = rolesPassMark{members: members, at: clock.Unix()}
	return ok && mk.members != members
}

// roleMember is one rostered member as the roles pass ranks it.
type roleMember struct {
	CharID int64
	Name   string
	Roles  uint64
	Trust  float64
	// Untenured marks a member too new for an office with wallet access.
	Untenured bool
}

// officerQuota is how many of each officer a corporation of n members keeps.
func officerQuota(n int, ch Charter) (directors, personnel, accountants int) {
	if n < ceoHierarchyFrom {
		return 0, 0, 0
	}
	directors = max(1, n/max(1, ch.DirectorPer))
	personnel = max(1, n/max(1, ch.PersonnelPer))
	return directors, personnel, 1
}

// dealOfficers hands the quota out by trust, most trusted first, one office
// per member: directors, then personnel managers, then the accountant. Pure,
// for the test.
func dealOfficers(members []roleMember, directors, personnel, accountants int) map[int64]uint64 {
	sorted := append([]roleMember(nil), members...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Trust != sorted[j].Trust {
			return sorted[i].Trust > sorted[j].Trust
		}
		return sorted[i].CharID < sorted[j].CharID
	})
	out := map[int64]uint64{}
	deal := func(n int, bit uint64, needTenure bool) {
		for _, rm := range sorted {
			if n <= 0 {
				return
			}
			if _, taken := out[rm.CharID]; taken || (needTenure && rm.Untenured) {
				continue
			}
			out[rm.CharID] = bit
			n--
		}
	}
	deal(directors, corp.RoleDirector, true)
	deal(personnel, corp.RolePersonnelManager, false)
	deal(accountants, corp.RoleAccountant, true)
	return out
}

// ceoRoles runs the officer pass, the fleet-seat pass when it is due, and
// the title sync.
func (m *Mod) ceoRoles(ctx context.Context, c *corpCEO, ch Charter) error {
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}
	all, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		return fmt.Errorf("ceoRoles members: %w", err)
	}
	// A membership that moved since the last pass re-deals the fleet seats too (one seat per
	// FCSeatDivisor members), held to seatRequestEverySec.
	if noteRolesPass(c.CorpID, facts.Members) {
		requestSeatPass(c.CorpID)
	}
	// Every step below is attempted: one that fails is reported, and the departments and fleet
	// seats after it are still dealt. A single bad write must not leave the corporation with no
	// departments and no fleets until next week's review.
	var errs []error
	// The weekly review first (corpstaff.go), so the trust below reads this
	// week's verdict on every officer.
	if err := m.reviewStaff(ctx, c, ch); err != nil {
		errs = append(errs, fmt.Errorf("ceoRoles review: %w", err))
	}
	staffRows := m.loadStaffRows(ctx, c.CorpID)

	var members []roleMember
	now := win32Now()
	ranks := m.meritRanksOf(ctx, c.CorpID) // empty with the ladder off
	for _, cm := range all {
		if !cm.Eligible() {
			continue
		}
		p := m.memberIdentity(ctx, cm)
		// An officer's performance multiplies its trust, so one that does its job badly
		// is outranked (a non-officer reads neutral).
		perf := 1.0
		if r, ok := staffRows[cm.CharID]; ok {
			perf = clampPerf(r.Perf)
		}
		members = append(members, roleMember{
			CharID: cm.CharID,
			Name:   cm.Name,
			Roles:  cm.Roles,
			// Rank (corpmerit.go) weighs in: a post is something a member
			// can earn into, not only be dealt.
			Trust: tenureTrust(cm.Since, now) * corpTagFit(p, facts.Policy.Tags) * p.Tune.CommandDrive * perf *
				meritTrustMul(ranks[cm.CharID]) * (1 + 0.3*p.Favour),
			// The treasury guard is for players: a bot does not walk off with
			// the wallet, and a young corporation must still have officers.
			Untenured: officerUntenured(cm.Human(), cm.Since, now),
		})
	}

	// The arms (corparm.go): a Director is appointed per justified department
	// rather than per Charter.DirectorPer members.
	armSig := m.gatherArmSignals(ctx, c, facts)
	armJust := justifiedArms(armSig)

	// The whole membership decides the size of the hierarchy.
	_, personnel, accountants := officerQuota(facts.Members, ch)
	directors := armOfficerQuota(armJust, facts.Members)
	// A Director of Diplomacy on top when the corporation has relations to manage
	// (corpstaff_dept.go): one more chair, never the first.
	diplomacy := m.diplomacyPortfolio(ctx, c.CorpID, facts.Members)
	if directors > 0 && len(diplomacy) > 0 {
		directors++
	}
	want := dealOfficers(members, directors, personnel, accountants)
	policy, _ := m.loadCorpPolicy(ctx, c.CorpID)
	for _, rm := range members {
		cur := rm.Roles & ceoRoleMask
		next := want[rm.CharID]
		if cur == next {
			continue
		}
		// A Director of Commerce stood down takes no corporation orders with it: they
		// are cancelled while its Director bit still resolves to the corporation.
		if cur&corp.RoleDirector != 0 && next&corp.RoleDirector == 0 &&
			staffRows[rm.CharID].Portfolio == portCommerce &&
			rm.CharID != policy.DirectorID && rm.CharID != policy.DeputyID {
			m.cancelCorpOrdersOf(ctx, rm.CharID)
		}
		if err := m.setMemberRoles(ctx, rm.CharID, (rm.Roles&^ceoRoleMask)|next); err != nil {
			errs = append(errs, fmt.Errorf("ceoRoles write %d: %w", rm.CharID, err))
			continue
		}
		switch {
		case next != 0:
			m.ceoLog(ctx, c, powerRoles, "promote", rm.CharID, 0,
				"appointed %s %s of %s", rm.Name, officerName(next), facts.Name)
		default:
			m.ceoLog(ctx, c, powerRoles, "demote", rm.CharID, 0,
				"stood %s down as %s of %s", rm.Name, officerName(cur), facts.Name)
		}
	}

	// The departments: which director runs what (corpstaff.go).
	needed := append(armNeededPortfolios(armJust), diplomacy...)
	if err := m.ceoPortfolios(ctx, c, needed); err != nil {
		errs = append(errs, fmt.Errorf("ceoRoles departments: %w", err))
	}

	// The fleet-seat pass, at most daily, or sooner when a department head asks
	// (seatPassWanted, corpstaff_dept.go; held to seatRequestEverySec).
	last := m.lastSeatPassAt(ctx, c.CorpID)
	seatPassMu.Lock()
	if at, ok := seatPassAt[c.CorpID]; ok && at > last {
		last = at
	}
	due := clock.Unix()-last >= ceoSeatPassEverySec || seatPassWanted(c.CorpID, last)
	if due {
		seatPassAt[c.CorpID] = clock.Unix()
	}
	seatPassMu.Unlock()
	if due {
		before, _ := m.listCorpRanks(ctx, c.CorpID)
		if _, err := m.promoteCorp(ctx, c.CorpID); err != nil {
			errs = append(errs, fmt.Errorf("ceoRoles seats: %w", err))
		} else {
			after, _ := m.listCorpRanks(ctx, c.CorpID)
			m.logSeatChanges(ctx, c, facts.Name, before, after)
		}
	}

	// The industry staff: director, deputy, contract builders (corpdirector.go).
	if err := m.ceoIndustryStaff(ctx, c, ch, facts); err != nil {
		errs = append(errs, fmt.Errorf("ceoRoles industry: %w", err))
	}
	// The starbase seats: Fueler and Procurer (posseats.go).
	if c.powerOn(powerStarbase) {
		if err := m.ceoStarbaseStaff(ctx, c, ch, facts); err != nil {
			errs = append(errs, fmt.Errorf("ceoRoles starbase: %w", err))
		}
	}
	// The arms last: every position above decides somebody's arm.
	if err := m.ceoArms(ctx, c, armSig, armJust); err != nil {
		errs = append(errs, fmt.Errorf("ceoRoles arms: %w", err))
	}
	// The postings after the arms: a member is posted to the office its
	// arm's job is done at (corpposting.go).
	if err := m.ceoPostings(ctx, c); err != nil {
		errs = append(errs, fmt.Errorf("ceoRoles postings: %w", err))
	}

	m.syncCorpTitles(ctx, c.CorpID)
	return errors.Join(errs...)
}

// lastSeatPassAt is when the fleet-seat pass last ran for a corporation, from
// the database: promoteCorp re-stamps updatedAt on every auto seat it confirms,
// so the newest is the last pass (seatPassAt alone is process memory). A
// corporation with no auto seats has no marker and runs the cheap pass.
func (m *Mod) lastSeatPassAt(ctx context.Context, corpID int64) int64 {
	var at int64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(updatedAt), 0) FROM rolebot_corp_ranks
		 WHERE corporationID = ? AND manual = 0`, corpID).Scan(&at)
	return at
}

// officerName is how the ledger names an office.
func officerName(bit uint64) string {
	switch {
	case bit&corp.RoleDirector != 0:
		return "a Director"
	case bit&corp.RolePersonnelManager != 0:
		return "Personnel Manager"
	case bit&corp.RoleAccountant != 0:
		return "Accountant"
	}
	return "an officer"
}

// logSeatChanges turns the difference between two readings of the chain of
// command into ledger rows. Manual seats are the operator's and are not
// reported as the CEO's decisions.
func (m *Mod) logSeatChanges(ctx context.Context, c *corpCEO, corpName string, before, after []corpRankRow) {
	was := map[int64]corpRankRow{}
	for _, r := range before {
		if !r.Manual {
			was[r.CharID] = r
		}
	}
	now := map[int64]corpRankRow{}
	for _, r := range after {
		if !r.Manual {
			now[r.CharID] = r
		}
	}
	for id, r := range now {
		prev, held := was[id]
		switch {
		case !held:
			m.ceoLog(ctx, c, powerRoles, "seat", id, r.Score,
				"gave %s the %s fleet of %s", r.Name, r.Activity, corpName)
		case prev.Activity != r.Activity:
			m.ceoLog(ctx, c, powerRoles, "seat", id, r.Score,
				"moved %s from the %s fleet to the %s fleet", r.Name, prev.Activity, r.Activity)
		}
	}
	for id, r := range was {
		if _, ok := now[id]; !ok {
			m.ceoLog(ctx, c, powerRoles, "unseat", id, r.Score,
				"took the %s fleet off %s", r.Activity, r.Name)
		}
	}
}

// setMemberRoles writes a member's whole role mask and pushes it to the
// member's session when it is online, so a player sees a new office without
// relogging. Every module role write goes through here.
func (m *Mod) setMemberRoles(ctx context.Context, charID int64, roles uint64) error {
	old, _ := evedb.GetCorporationMemberInfo(ctx, m.db, charID)
	// A human never holds take on the war chest (corpdivisions.go), whatever
	// a bundle computed for its seat.
	tc := thinkCacheAny(ctx)
	corpID, carried := int64(0), false
	if tc != nil {
		corpID, carried = tc.memberCorp(charID)
	}
	if !carried {
		corpID = m.charCorpID(ctx, charID)
	}
	roles = m.stripGuardedTake(ctx, corpID, charID, roles)
	v := int64(roles)
	if err := evedb.UpdateCorpMemberRow(ctx, m.db, charID, &evedb.CorpMemberUpdate{Roles: &v}); err != nil {
		return err
	}
	if tc != nil {
		tc.noteRoles(charID, roles)
	}
	if h := modapi.Corp().MemberRolesChanged; h != nil {
		h(charID, old)
	}
	return nil
}

// seatPassRequested reports whether a department head has asked for the fleet
// seats to follow a change in demand since the last seat pass; it brings the
// Corporate Review forward (corpceo.go reviewDue).
func seatPassRequested(corpID int64) bool {
	seatPassMu.Lock()
	last := seatPassAt[corpID]
	seatPassMu.Unlock()
	return seatPassWanted(corpID, last)
}

func init() {
	registerCorpForget("seatPassAt", func(id int64) {
		seatPassMu.Lock()
		delete(seatPassAt, id)
		seatPassMu.Unlock()
		rolesPassMu.Lock()
		delete(rolesPassLast, id)
		rolesPassMu.Unlock()
	})
}
