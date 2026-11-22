package rolebots

// NPC chairs: an NPC corporation run by its own NPC CEO, which keeps a
// directive from the corporation's tags, appoints a Director of Security,
// runs that desk, deals the fleet seats and buys Leadership books from a
// capped budget. Chairs live in rolebot_npc_chairs, never rolebot_ceos, and
// no core corp role is written for an NPC corporation. Why, and what a chair
// never does: docs/CORPS.md §9a.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const (
	ceoOriginNPC = "npc"

	// npcChairSweepTicks is how often the sweep looks for a due think and a
	// due desk (ticks are one second).
	npcChairSweepTicks = 60
	// npcChairThinkEverySec is the chair's think cadence: directive, the
	// department head, the seat deal.
	npcChairThinkEverySec = 6 * 3600
	// npcChairThinksPerPass bounds one sweep after a restart.
	npcChairThinksPerPass = 12
	// npcChairReconcileEverySec is how often the sweep re-reads which NPC
	// corporations hold bots.
	npcChairReconcileEverySec = 600
	// npcChairHeadReviewSec is how often the Director of Security is re-dealt.
	npcChairHeadReviewSec = 7 * 86400
	// npcChairPush scales the directive's strength: an institution pushes its
	// members less than a capsuleer CEO pushes its own.
	npcChairPush = 0.6
	// npcChairSpendWindowSec is the window the spending cap covers.
	npcChairSpendWindowSec = 7 * 86400
)

// npcChairPowersOff is fixed for every NPC chair; the panel cannot change it.
// Directive and Roles stay on.
var npcChairPowersOff = map[string]bool{
	powerIdentity: true, powerDiplomacy: true, powerAlliance: true, powerWar: true,
	powerHR: true, powerTreasury: true, powerEstate: true, powerBusiness: true,
	powerProduction: true, powerStarbase: true, powerShares: true,
}

// npcChairRow is one rolebot_npc_chairs row.
type npcChairRow struct {
	CorpID         int64
	CEOID          int64
	InstalledAt    int64
	NextThinkAt    int64
	LastSeatPassAt int64
	HeadReviewAt   int64
	SecurityHeadID int64
	OfficeReviewAt int64
}

// ceo is the in-memory chair the shared powers take.
func (r npcChairRow) ceo() *corpCEO {
	return &corpCEO{CorpID: r.CorpID, CharID: r.CEOID, Origin: ceoOriginNPC, PowersOff: npcChairPowersOff}
}

// ── the index ───────────────────────────────────────────────────────────────

// npcChairIndex maps a chaired NPC corporation to its CEO. Read on hot paths
// (the directive term), rebuilt by the sweep.
var npcChairIndex atomic.Pointer[map[int64]int64]

// npcChaired reports whether an NPC corporation is run by its NPC chair.
func npcChaired(corpID int64) bool {
	p := npcChairIndex.Load()
	if p == nil || corpID == 0 {
		return false
	}
	_, ok := (*p)[corpID]
	return ok
}

// chairOf is whoever runs a corporation: a bot CEO of a player corporation,
// or the NPC CEO of a chaired NPC corporation.
func chairOf(corpID int64) (ceoID int64, npc bool, ok bool) {
	if id, ok := managedCEO(corpID); ok {
		return id, false, true
	}
	p := npcChairIndex.Load()
	if p == nil || corpID == 0 {
		return 0, false, false
	}
	id, ok := (*p)[corpID]
	return id, true, ok
}

func (m *Mod) refreshNPCChairIndex(ctx context.Context) {
	rows, err := m.db.QueryContext(ctx, `SELECT corporationID, ceoID FROM rolebot_npc_chairs`)
	if err != nil {
		rlogf("rolebots: NPC chair index: %v", err)
		return
	}
	defer rows.Close()
	idx := map[int64]int64{}
	for rows.Next() {
		var corpID, ceoID int64
		if rows.Scan(&corpID, &ceoID) == nil {
			idx[corpID] = ceoID
		}
	}
	npcChairIndex.Store(&idx)
}

// ── the table ───────────────────────────────────────────────────────────────

const npcChairCols = `corporationID, ceoID, installedAt, nextThinkAt, lastSeatPassAt, headReviewAt, securityHeadID, officeReviewAt`

func scanNPCChair(sc interface{ Scan(...any) error }) (npcChairRow, error) {
	var r npcChairRow
	err := sc.Scan(&r.CorpID, &r.CEOID, &r.InstalledAt, &r.NextThinkAt, &r.LastSeatPassAt, &r.HeadReviewAt,
		&r.SecurityHeadID, &r.OfficeReviewAt)
	return r, err
}

func (m *Mod) listNPCChairs(ctx context.Context) ([]npcChairRow, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+npcChairCols+` FROM rolebot_npc_chairs ORDER BY corporationID`)
	if err != nil {
		return nil, fmt.Errorf("listNPCChairs: %w", err)
	}
	defer rows.Close()
	var out []npcChairRow
	for rows.Next() {
		r, err := scanNPCChair(rows)
		if err != nil {
			return nil, fmt.Errorf("listNPCChairs scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *Mod) getNPCChair(ctx context.Context, corpID int64) (npcChairRow, bool, error) {
	r, err := scanNPCChair(m.db.QueryRowContext(ctx,
		`SELECT `+npcChairCols+` FROM rolebot_npc_chairs WHERE corporationID = ?`, corpID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

func (m *Mod) saveNPCChair(ctx context.Context, r npcChairRow) error {
	_, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_npc_chairs (`+npcChairCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE ceoID = VALUES(ceoID), nextThinkAt = VALUES(nextThinkAt),
			lastSeatPassAt = VALUES(lastSeatPassAt), headReviewAt = VALUES(headReviewAt),
			securityHeadID = VALUES(securityHeadID), officeReviewAt = VALUES(officeReviewAt)`,
		r.CorpID, r.CEOID, r.InstalledAt, r.NextThinkAt, r.LastSeatPassAt, r.HeadReviewAt, r.SecurityHeadID,
		r.OfficeReviewAt)
	if err != nil {
		return fmt.Errorf("saveNPCChair %d: %w", r.CorpID, err)
	}
	return nil
}

// ── install and release ─────────────────────────────────────────────────────

// reconcileNPCChairs seats a chair in every NPC corporation holding enabled
// bots and releases the rest. With the switch off it releases every chair.
func (m *Mod) reconcileNPCChairs(ctx context.Context) error {
	have, err := m.listNPCChairs(ctx)
	if err != nil {
		return err
	}
	want := map[int64]int64{}
	if npcChairOn() {
		rows, err := m.db.QueryContext(ctx, `
			SELECT co.corporationID, co.ceoID
			FROM corporation co
			JOIN crpNPCCorporations n ON n.corporationID = co.corporationID
			WHERE co.ceoID > 0 AND EXISTS (
				SELECT 1 FROM rolebot_characters b
				JOIN character_ c ON c.characterID = b.characterID
				WHERE b.enabled = 1 AND c.corporationID = co.corporationID)`)
		if err != nil {
			return fmt.Errorf("reconcileNPCChairs: %w", err)
		}
		for rows.Next() {
			var corpID, ceoID int64
			if rows.Scan(&corpID, &ceoID) == nil {
				want[corpID] = ceoID
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("reconcileNPCChairs: %w", err)
		}
	}
	held := map[int64]bool{}
	for _, r := range have {
		held[r.CorpID] = true
		ceoID, keep := want[r.CorpID]
		switch {
		case !keep:
			m.releaseNPCChair(ctx, r.CorpID, "no rostered member left, or NPC chairs switched off")
		case ceoID != r.CEOID:
			// The game's CEO changed (a GM edit): the new one takes the chair.
			r.CEOID = ceoID
			if err := m.saveNPCChair(ctx, r); err != nil {
				return err
			}
		}
	}
	now := clock.Unix()
	for corpID, ceoID := range want {
		if held[corpID] {
			continue
		}
		// Spread the first thinks across one cadence so 130 chairs do not
		// all think on the first pass.
		first := now + int64(splitmix64(uint64(corpID))%uint64(npcChairThinkEverySec/4))
		if err := m.saveNPCChair(ctx, npcChairRow{CorpID: corpID, CEOID: ceoID, InstalledAt: now, NextThinkAt: first}); err != nil {
			return err
		}
		rlogf("rolebots: NPC chair seated in corp %d (CEO %d)", corpID, ceoID)
	}
	m.refreshNPCChairIndex(ctx)
	return nil
}

// releaseNPCChair takes the chair away and withdraws what it set: the
// directive, the desk's standing orders, the derived doctrines and its empty
// owner offices. Seats stay; the fallback pass (promotionPass) re-deals them.
func (m *Mod) releaseNPCChair(ctx context.Context, corpID int64, why string) {
	m.releaseEmptyNPCOffices(ctx, corpID)
	for _, q := range []string{
		`DELETE FROM rolebot_npc_chairs WHERE corporationID = ?`,
		`DELETE FROM rolebot_corp_directive WHERE corporationID = ?`,
		`DELETE FROM rolebot_corp_doctrine WHERE corporationID = ? AND override = 0`,
	} {
		if _, err := m.db.ExecContext(ctx, q, corpID); err != nil {
			rlogf("rolebots: release NPC chair %d: %v", corpID, err)
		}
	}
	m.clearFleetOrder(ctx, corpID, portSecurity)
	forgetCorpDirective(corpID)
	rlogf("rolebots: NPC chair released in corp %d: %s", corpID, why)
}

// ── the stand-in personality ────────────────────────────────────────────────

// npcChairTraits is the NPC CEO's personality: deterministic from its ID,
// leaning to command, and to the law unless the corporation is a cartel's.
// Pure.
func npcChairTraits(ceoID int64, outlaw bool) map[string]int {
	out := map[string]int{}
	for i, t := range traitCatalog {
		r := splitmix64(uint64(ceoID)*31 + uint64(i))
		out[t.Key] = int(r%121) - 60
	}
	out[TraitLeadership] = 40 + int(splitmix64(uint64(ceoID)+7)%41)
	if outlaw {
		out[TraitLawfulness] = -30 - int(splitmix64(uint64(ceoID)+11)%51)
	} else {
		out[TraitLawfulness] = 30 + int(splitmix64(uint64(ceoID)+11)%51)
	}
	return out
}

// ensureNPCChairTraits writes the stand-in once; a CEO with traits keeps them.
func (m *Mod) ensureNPCChairTraits(ctx context.Context, ceoID int64, outlaw bool) (map[string]int, error) {
	t, err := getTraits(ctx, m.db, ceoID)
	if err != nil {
		return nil, err
	}
	if len(t) > 0 {
		return t, nil
	}
	t = npcChairTraits(ceoID, outlaw)
	if err := saveTraits(ctx, m.db, ceoID, t); err != nil {
		return nil, err
	}
	return t, nil
}

// npcTagActivities maps an NPC corporation's tags to the activities its
// directive names, in tag order. A cartel's combat is piracy first.
var npcTagActivities = map[string][]string{
	"mining":         {ActMining},
	"industry":       {ActIndustry},
	"shipbuilding":   {ActIndustry},
	"cartelindustry": {ActIndustry},
	"research":       {ActIndustry},
	"trade":          {ActTrade},
	"hauling":        {ActCourier},
	tagCombat:        {ActCombat},
	tagSecurity:      {ActCombat},
	tagOutlaw:        {ActPiracy, ActCombat},
}

// npcDirective is the directive an NPC corporation's tags name: up to
// ceoDirectiveMax activities weighted 3:2:1 by tag order, summing to 1. Nil
// when no tag names an activity. Pure.
func npcDirective(tags []string) map[string]float64 {
	var acts []string
	seen := map[string]bool{}
	for _, t := range tags {
		for _, a := range npcTagActivities[t] {
			if !seen[a] && len(acts) < ceoDirectiveMax {
				seen[a] = true
				acts = append(acts, a)
			}
		}
	}
	if len(acts) == 0 {
		return nil
	}
	out := map[string]float64{}
	total := 0.0
	for i := range acts {
		total += float64(len(acts) - i)
	}
	for i, a := range acts {
		out[a] = float64(len(acts)-i) / total
	}
	return out
}

// npcCharter is the chair's charter: the stand-in's own, with the directive
// taken from what the corporation is, and pushed at npcChairPush.
func npcCharter(ceoID int64, traits map[string]int, tags []string) Charter {
	tn := resolveTuning(traits)
	ch := resolveCharter(pilotIdentity{CharID: ceoID, Traits: traits, Tune: tn})
	if d := npcDirective(tags); d != nil {
		ch.Directive = d
	}
	ch.DirectiveStrength = tn.CommandDrive * npcChairPush
	return ch
}

// ── the budget ──────────────────────────────────────────────────────────────

// npcChairBudgetFor is what a chair may spend in a window: a share of the
// wallet, capped. Pure.
func npcChairBudgetFor(balance, permille, capISK, spent float64) float64 {
	if balance <= 0 {
		return 0
	}
	b := math.Min(balance*permille/1000, capISK) - spent
	return math.Max(0, b)
}

// npcChairBudget is what the chair may still spend this week.
func (m *Mod) npcChairBudget(ctx context.Context, corpID int64) float64 {
	var balance, spent float64
	_ = m.db.QueryRowContext(ctx, `SELECT balance FROM corporation WHERE corporationID = ?`, corpID).Scan(&balance)
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM rolebot_ceo_actions
		WHERE corporationID = ? AND verb = 'skillbook' AND at >= ?`,
		corpID, clock.Unix()-npcChairSpendWindowSec).Scan(&spent)
	return npcChairBudgetFor(balance, float64(npcChairSpendPermille()), float64(npcChairSpendCapM())*1e6, spent)
}

// npcChairBuysBook is the corpLeadershipNudge hook: a chaired NPC corporation
// pays for its seat candidate's Leadership book while the weekly budget lasts.
// False when it did not (the member buys its own).
func (m *Mod) npcChairBuysBook(ctx context.Context, corpID, charID int64) bool {
	ceoID, npc, ok := chairOf(corpID)
	if !ok || !npc {
		return false
	}
	budget := m.npcChairBudget(ctx, corpID)
	if budget <= 0 {
		return false
	}
	c := &corpCEO{CorpID: corpID, CharID: ceoID, Origin: ceoOriginNPC, PowersOff: npcChairPowersOff}
	_, paid := m.corpBuysBook(ctx, c, charID, skillLeadership, budget, "command material for a fleet seat")
	return paid
}

// npcDoctrineCeiling is an NPC wing's hull ceiling: what the chair can still
// spend this week or what its median member holds, whichever is more, times
// doctrineCapitalFrac: a doctrine asks only for hulls its pilots can replace.
func (m *Mod) npcDoctrineCeiling(ctx context.Context, corpID int64, wing []int64) float64 {
	var wallets []float64
	if len(wing) > 0 {
		rows, err := m.db.QueryContext(ctx, `SELECT balance FROM character_ WHERE characterID IN (`+
			placeholders(len(wing))+`)`, int64Args(wing)...)
		if err == nil {
			for rows.Next() {
				var b float64
				if rows.Scan(&b) == nil {
					wallets = append(wallets, b)
				}
			}
			rows.Close()
		}
	}
	return math.Round(math.Max(m.npcChairBudget(ctx, corpID), medianOf(wallets)) * doctrineCapitalFrac)
}

// medianOf is the median of xs, 0 for none. Pure; sorts a copy.
func medianOf(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// ── the sweep ───────────────────────────────────────────────────────────────

var (
	npcChairBusy        atomic.Bool
	npcChairReconcileAt atomic.Int64
)

// startNPCChairSweep registers the chairs' pass: reconcile, due thinks, and
// each chair's Security desk on the staff desk's cadence.
func (m *Mod) startNPCChairSweep() {
	m.registerPeriodic("rolebot-npc-chair", npcChairSweepTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		if !npcChairBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("NPC chair sweep", func() {
			defer npcChairBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.npcChairPass(ctx)
		})
		return true
	})
}

func (m *Mod) npcChairPass(ctx context.Context) {
	now := clock.Unix()
	if now-npcChairReconcileAt.Load() >= npcChairReconcileEverySec {
		npcChairReconcileAt.Store(now)
		if err := m.reconcileNPCChairs(ctx); err != nil {
			rlogf("rolebots: NPC chairs: %v", err)
		}
	}
	if !npcChairOn() {
		return
	}
	chairs, err := m.listNPCChairs(ctx)
	if err != nil {
		rlogf("rolebots: NPC chairs: %v", err)
		return
	}
	thinks := 0
	for _, r := range chairs {
		if ctx.Err() != nil {
			return
		}
		lk := corpOpLock(r.CorpID)
		if !lk.TryLock() {
			continue
		}
		func() {
			defer lk.Unlock()
			lctx := withOpLockHeld(ctx)
			if r.NextThinkAt <= now && thinks < npcChairThinksPerPass {
				thinks++
				if err := m.npcChairThink(lctx, &r); err != nil {
					rlogf("rolebots: NPC chair %d think: %v", r.CorpID, err)
				}
			}
			if deskDue(r.CorpID, "npcsecurity", int64(staffDeskEveryTicks)) {
				if err := m.npcChairDesk(lctx, r); err != nil {
					rlogf("rolebots: NPC chair %d desk: %v", r.CorpID, err)
				}
			}
		}()
	}
}

// npcChairCharter reads the chair's charter for one corporation.
func (m *Mod) npcChairCharter(ctx context.Context, r npcChairRow, facts *CorpFacts) (Charter, error) {
	traits, err := m.ensureNPCChairTraits(ctx, r.CEOID, containsStr(facts.Policy.Tags, tagOutlaw))
	if err != nil {
		return Charter{}, err
	}
	return npcCharter(r.CEOID, traits, facts.Policy.Tags), nil
}

// npcChairThink is one think: the directive, the Director of Security, the
// seat deal.
func (m *Mod) npcChairThink(ctx context.Context, r *npcChairRow) error {
	facts, err := getAnyCorpFacts(ctx, m.db, r.CorpID)
	if err != nil {
		return err
	}
	ch, err := m.npcChairCharter(ctx, *r, facts)
	if err != nil {
		return err
	}
	c := r.ceo()
	if err := m.ceoDirective(ctx, c, ch); err != nil {
		rlogf("rolebots: NPC chair %d directive: %v", r.CorpID, err)
	}
	now := clock.Unix()
	if now-r.HeadReviewAt >= npcChairHeadReviewSec || !m.npcHeadStillServes(ctx, r.CorpID, r.SecurityHeadID) {
		r.SecurityHeadID = m.npcChairAppoint(ctx, c, facts, r.SecurityHeadID)
		r.HeadReviewAt = now
	}
	if now-r.OfficeReviewAt >= npcOfficeReviewSec {
		m.npcChairOfficePass(ctx, c)
		r.OfficeReviewAt = now
	}
	if now-r.LastSeatPassAt >= ceoSeatPassEverySec || seatPassWanted(r.CorpID, r.LastSeatPassAt) {
		before, _ := m.listCorpRanks(ctx, r.CorpID)
		if _, err := m.promoteCorp(ctx, r.CorpID); err != nil {
			rlogf("rolebots: NPC chair %d seats: %v", r.CorpID, err)
		} else {
			after, _ := m.listCorpRanks(ctx, r.CorpID)
			m.logSeatChanges(ctx, c, facts.Name, before, after)
			r.LastSeatPassAt = now
		}
	}
	r.NextThinkAt = now + npcChairThinkEverySec + int64(splitmix64(uint64(r.CorpID)+uint64(now))%uint64(npcChairThinkEverySec/5))
	return m.saveNPCChair(ctx, *r)
}

// npcHeadStillServes reports whether the Director of Security is still an
// enabled rostered member of the corporation.
func (m *Mod) npcHeadStillServes(ctx context.Context, corpID, headID int64) bool {
	if headID == 0 {
		return false
	}
	var ok int
	err := m.db.QueryRowContext(ctx, `
		SELECT 1 FROM rolebot_characters b
		JOIN character_ c ON c.characterID = b.characterID
		WHERE b.characterID = ? AND b.enabled = 1 AND c.corporationID = ?`, headID, corpID).Scan(&ok)
	return err == nil
}

// npcChairAppoint deals the Director of Security among the corporation's
// enabled bots with the same fit the player desks use (portfolioFit). Module
// side only: no core role is written. Answers the head, 0 when nobody fits.
func (m *Mod) npcChairAppoint(ctx context.Context, c *corpCEO, facts *CorpFacts, current int64) int64 {
	ctx = withThinkCache(ctx, c.CorpID)
	m.preloadThink(ctx, c.CorpID, c.CharID, facts)
	members, err := m.corpMembers(ctx, c.CorpID, c.CharID)
	if err != nil {
		rlogf("rolebots: NPC chair %d members: %v", c.CorpID, err)
		return current
	}
	now := clock.Unix()
	var cands []portfolioCand
	for _, cm := range members {
		if cm.Human() || !cm.Enabled {
			continue
		}
		p := m.memberIdentity(ctx, cm)
		cand := portfolioCand{CharID: cm.CharID, Fit: map[string]float64{portSecurity: portfolioFit(p, portSecurity)}}
		if cm.CharID == current {
			cand.Current, cand.Since = portSecurity, now-npcChairHeadReviewSec
		}
		cands = append(cands, cand)
	}
	var head int64
	for id, port := range dealPortfolios(cands, []string{portSecurity}, now) {
		if port == portSecurity {
			head = id
		}
	}
	if head != current {
		switch {
		case head == 0:
			m.ceoLog(ctx, c, powerRoles, "department", current, 0,
				"relieved %s of the Security department", m.charNameOr(ctx, current))
		default:
			m.ceoLog(ctx, c, powerRoles, "department", head, 0,
				"put %s in charge of Security as %s", m.charNameOr(ctx, head), portfolioTitle(portSecurity))
		}
	}
	return head
}

// npcChairDesk runs the Director of Security's desk: the standing order onto
// the corporation's distressed belts, the seat demand behind it, and the
// doctrines (securityDesk, shared with the player corporations).
func (m *Mod) npcChairDesk(ctx context.Context, r npcChairRow) error {
	facts, err := getAnyCorpFacts(ctx, m.db, r.CorpID)
	if err != nil {
		return err
	}
	ch, err := m.npcChairCharter(ctx, r, facts)
	if err != nil {
		return err
	}
	c := r.ceo()
	d := &staffMember{CharID: r.SecurityHeadID, Name: m.charNameOr(ctx, r.SecurityHeadID),
		Enabled: true, Post: postDirector, Portfolio: portSecurity}
	if r.SecurityHeadID != 0 {
		ctx = withStaffActor(ctx, staffActor{CharID: d.CharID, Name: d.Name, Office: portfolioTitle(portSecurity)})
	}
	return m.securityDesk(ctx, c, ch, d, facts)
}

// npcChairSummary is one line for the NPC corporation page.
func (m *Mod) npcChairSummary(ctx context.Context, corpID int64) string {
	r, ok, err := m.getNPCChair(ctx, corpID)
	if err != nil || !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Chaired by %s", m.charNameOr(ctx, r.CEOID))
	if r.SecurityHeadID != 0 {
		fmt.Fprintf(&b, "; %s %s", portfolioTitle(portSecurity), m.charNameOr(ctx, r.SecurityHeadID))
	}
	fmt.Fprintf(&b, "; budget left this week %s", iskString(m.npcChairBudget(ctx, corpID)))
	return b.String()
}
