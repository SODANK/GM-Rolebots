package rolebots

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
)

// The staff: the officers a bot CEO appoints, given work to do.
//
// The CEO keeps policy: the charter, tax rate, reserve, budgets, war and peace, and
// who is appointed to what. Officers execute on their own cadence (the staff desk
// sweep, every 15 minutes), in their own name, with their traits colouring how and
// never what: an officer may spend less or later than the charter allows, never more.
// A duty whose post is empty stays with the CEO's think; a post held by a human keeps
// its authority and the CEO covers the work in the human's name (staffExecutor).
// corpOpLock keeps the think and the desks from spending free capital at once.
// See docs/comment-notes-b06.md#corp-staff.

const (
	postTreasurer = "treasurer"
	postPersonnel = "personnel"
	postDirector  = "director"

	portSecurity   = "security"
	portOperations = "operations"
	portCommerce   = "commerce"
	// portHR is the Personnel department, dealt only with arms on (corparm.go) when the
	// corporation is big enough.
	portHR = "hr"
	// portDiplomacy is the Director of Diplomacy, a part-time office: the holder keeps
	// its day job and runs the corporation's relations every few hours (diplomacyDesk).
	// Dealt only when there are relations to manage (diplomacyPortfolio).
	portDiplomacy = "diplomacy"
	// portInfrastructure is the Director of Infrastructure, head of the Infrastructure
	// arm: runs the corporation's towers day to day (posinfra.go). The CEO keeps whether
	// and where to plant, what for, and when to tear down.
	portInfrastructure = "infrastructure"
	// portResearch is the Director of Research, head of the R&D arm. It has no desk: the
	// library plan (corplibplan.go) posts the orders. The head gives the department its
	// title, its letters (rndLetterFrom) and a review on research orders delivered.
	portResearch = "research"
	// diplomacyFromMembers is the size at which a Director of Diplomacy is
	// worth a Director's chair.
	diplomacyFromMembers = 8

	// staffDeskEveryTicks is the desks' cadence (1 tick = 1 s).
	staffDeskEveryTicks = 900
	// portfolioKeepSec / portfolioOutscore: a department head keeps its department
	// until it has held it this long and somebody now outscores it by this much (directorKeepSec).
	portfolioKeepSec  = 14 * 86400
	portfolioOutscore = 1.3

	// Performance: an EWMA kept in [staffPerfMin, staffPerfMax], 1 neutral,
	// reviewed weekly (reviewStaff).
	staffPerfMin        = 0.5
	staffPerfMax        = 1.5
	staffPerfAlpha      = 0.3
	staffReviewEverySec = 7 * 86400
)

// portfolioTitle is the title TEXT a department head wears. The title SLOT
// stays "Director" (corptitles.go: eight fixed slots); only character_.title
// names the department. With arms (corparm.go) a department is named for
// its arm: Operations heads Mining, Commerce heads Sales.
func portfolioTitle(port string) string {
	if t := armHeadTitle(armOfPortfolio(port)); t != "" {
		return t
	}
	switch port {
	case portSecurity:
		return "Director of Security"
	case portOperations:
		return "Director of Operations"
	case portCommerce:
		return "Director of Commerce"
	case portDiplomacy:
		return "Director of Diplomacy"
	}
	return ""
}

// portfolioTitles is every name a department head has ever worn, under
// either naming: a title the module wrote is the module's to rewrite when the
// arms switch flips.
var portfolioTitles = []string{
	"Director of Security", "Director of Operations", "Director of Commerce", "Director of Diplomacy",
	armHeadTitle(armMining), armHeadTitle(armSales), armHeadTitle(armHR), armHeadTitle(armInfrastructure),
	armHeadTitle(armResearch),
}

// ── who is acting ───────────────────────────────────────────────────────────

// staffActor is who is doing the work a ledger line records.
type staffActor struct {
	CharID int64
	Name   string
	Office string
	// For is the office holder's name when somebody else covers the post: the CEO
	// standing in for a human officer.
	For string
}

func (a staffActor) label() string {
	if a.For != "" {
		return fmt.Sprintf("%s (for %s, %s)", a.Name, a.For, a.Office)
	}
	return fmt.Sprintf("%s (%s)", a.Name, a.Office)
}

type staffActorKey struct{}

func withStaffActor(ctx context.Context, a staffActor) context.Context {
	return context.WithValue(ctx, staffActorKey{}, a)
}

func staffActorOf(ctx context.Context) (staffActor, bool) {
	a, ok := ctx.Value(staffActorKey{}).(staffActor)
	return a, ok && a.CharID != 0
}

// ── one hand at a time ──────────────────────────────────────────────────────

var corpOpLocks sync.Map // corpID to *sync.Mutex

func corpOpLock(corpID int64) *sync.Mutex {
	// Load first: LoadOrStore allocates its argument on every call, and this
	// is asked for on every think, desk pass and command.
	if v, ok := corpOpLocks.Load(corpID); ok {
		return v.(*sync.Mutex)
	}
	v, _ := corpOpLocks.LoadOrStore(corpID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// withOpLockHeld marks ctx as running under the corporation's corpOpLock. Every
// holder of the lock must pass a marked ctx down: the lock is a plain sync.Mutex and
// payrollDue / settleAssignment take it themselves unless told it is held.
func withOpLockHeld(ctx context.Context) context.Context {
	return context.WithValue(ctx, opLockHeldKey{}, true)
}

// ── the roster of officers ──────────────────────────────────────────────────

// staffMember is one officer.
type staffMember struct {
	CharID  int64
	Name    string
	Human   bool
	Enabled bool
	Since   int64 // corporationDateTime, win32
	Ident   pilotIdentity

	Post           string
	Portfolio      string
	PortfolioSince int64
	Perf           float64
	PerfAt         int64
	Metric         float64 // the last reviewed measurement (reviewStaff)
}

// corpStaff is one corporation's officers.
type corpStaff struct {
	CorpID    int64
	CEOID     int64
	CEOName   string
	Treasurer *staffMember
	Personnel []*staffMember
	Directors []*staffMember
}

// on reports whether a staff member is at a desk: a human always counts (the
// module never logs a player off), a bot only while logged in.
func (sm *staffMember) on() bool {
	return sm != nil && (sm.Human || !popIsOffline(sm.CharID))
}

// anyoneOn reports whether the CEO or any officer is logged in.
func (s *corpStaff) anyoneOn() bool {
	if s == nil {
		return false
	}
	if !popIsOffline(s.CEOID) {
		return true
	}
	for _, sm := range s.all() {
		if sm.on() {
			return true
		}
	}
	return false
}

// head is the department head of a portfolio, nil when nobody runs it.
func (s *corpStaff) head(port string) *staffMember {
	if s == nil {
		return nil
	}
	for _, d := range s.Directors {
		if d.Portfolio == port {
			return d
		}
	}
	return nil
}

func (s *corpStaff) all() []*staffMember {
	var out []*staffMember
	if s.Treasurer != nil {
		out = append(out, s.Treasurer)
	}
	out = append(out, s.Personnel...)
	return append(out, s.Directors...)
}

// executor is who does an office's work: its holder when that is a pilot
// the module flies, else the CEO in the holder's name.
func (s *corpStaff) executor(h *staffMember, office string) staffActor {
	if h != nil && !h.Human && h.Enabled {
		return staffActor{CharID: h.CharID, Name: h.Name, Office: office}
	}
	a := staffActor{CharID: s.CEOID, Name: s.CEOName, Office: office}
	if h != nil {
		a.For = h.Name
	}
	return a
}

// officerPost is which office a role mask holds, by the roles pass's own
// precedence (dealOfficers gives one office per member; a human with several
// bits set by hand is read by the first).
func officerPost(roles uint64) string {
	switch {
	case roles&corp.RoleDirector != 0:
		return postDirector
	case roles&corp.RolePersonnelManager != 0:
		return postPersonnel
	case roles&corp.RoleAccountant != 0:
		return postTreasurer
	}
	return ""
}

type staffRow struct {
	Portfolio      string
	PortfolioSince int64
	Perf           float64
	PerfAt         int64
	Metric         float64
}

func (m *Mod) loadStaffRows(ctx context.Context, corpID int64) map[int64]staffRow {
	out := map[int64]staffRow{}
	for id, p := range m.familyPostsOf(ctx, corpID, famOfficer) {
		out[id] = staffRow{Portfolio: p.Portfolio, PortfolioSince: p.Since, Perf: p.Perf, PerfAt: p.ReviewAt, Metric: p.Metric}
	}
	return out
}

// loadCorpStaff reads a corporation's officers off their role bits, with the
// module's portfolio and performance beside them.
func (m *Mod) loadCorpStaff(ctx context.Context, corpID, ceoID int64) (*corpStaff, error) {
	members, err := m.corpMembers(ctx, corpID, ceoID)
	if err != nil {
		return nil, err
	}
	rows := m.loadStaffRows(ctx, corpID)
	s := &corpStaff{CorpID: corpID, CEOID: ceoID, CEOName: m.charNameOr(ctx, ceoID)}
	for _, cm := range members {
		post := officerPost(cm.Roles)
		if post == "" || !cm.Eligible() {
			continue
		}
		r, ok := rows[cm.CharID]
		if !ok {
			r.Perf = 1
		}
		sm := &staffMember{
			CharID: cm.CharID, Name: cm.Name, Human: cm.Human(), Enabled: cm.Enabled, Since: cm.Since,
			Ident: m.memberIdentity(ctx, cm), Post: post,
			Perf: clampPerf(r.Perf), PerfAt: r.PerfAt, Metric: r.Metric,
		}
		if post == postDirector {
			sm.Portfolio, sm.PortfolioSince = r.Portfolio, r.PortfolioSince
		}
		switch post {
		case postTreasurer:
			if s.Treasurer == nil {
				s.Treasurer = sm
			}
		case postPersonnel:
			s.Personnel = append(s.Personnel, sm)
		case postDirector:
			s.Directors = append(s.Directors, sm)
		}
	}
	return s, nil
}

type corpStaffKey struct{}

// staffFor is the corporation's staff, read once per think or desk pass and
// carried on the context from there.
func (m *Mod) staffFor(ctx context.Context, c *corpCEO) *corpStaff {
	if s, ok := ctx.Value(corpStaffKey{}).(*corpStaff); ok && s != nil && s.CorpID == c.CorpID {
		return s
	}
	s, err := m.loadCorpStaff(ctx, c.CorpID, c.CharID)
	if err != nil {
		return &corpStaff{CorpID: c.CorpID, CEOID: c.CharID}
	}
	return s
}

func withCorpStaff(ctx context.Context, s *corpStaff) context.Context {
	return context.WithValue(ctx, corpStaffKey{}, s)
}

// staffCovers reports whether an officer holds the post a duty belongs to, in which
// case the CEO's think leaves the duty to the staff desk.
func (m *Mod) staffCovers(ctx context.Context, c *corpCEO, post, port string) bool {
	s := m.staffFor(ctx, c)
	switch post {
	case postTreasurer:
		return s.Treasurer != nil
	case postDirector:
		return s.head(port) != nil
	}
	return false
}

func clampPerf(v float64) float64 {
	if v <= 0 || math.IsNaN(v) {
		return 1
	}
	return math.Max(staffPerfMin, math.Min(staffPerfMax, v))
}

// ── departments ─────────────────────────────────────────────────────────────

// neededPortfolios is the departments a corporation has work for, in the order a
// short-handed corporation fills them: its own trade first (belts, then market), its
// guard last. Operations and Security follow the fleets dealt (corpFleetActivities);
// Commerce follows the tags, since its Director runs the business power and Trade desk.
func neededPortfolios(p CorpPolicy) []string {
	var out []string
	seen := map[string]bool{}
	add := func(port string) {
		if !seen[port] {
			seen[port] = true
			out = append(out, port)
		}
	}
	acts := corpFleetActivities(p)
	for _, act := range acts {
		if act == ActMining || act == ActIndMining {
			add(portOperations)
		}
	}
	if commerceTagged(p) {
		add(portCommerce)
	}
	for _, act := range acts {
		if act == ActCombat || act == ActSecurity || act == ActPiracy {
			add(portSecurity)
		}
	}
	return out
}

// commerceTagged is whether a corporation's tags give it a Commerce
// department: the commerce outfits, and the industrial ones that have
// output to sell. Pure.
func commerceTagged(p CorpPolicy) bool {
	return p.HasTag("trade") || p.HasTag("retail") || p.HasTag("hauling") ||
		p.HasTag("industry") || p.HasTag("shipbuilding")
}

// portfolioFit is how well a director's temperament suits a department, before
// performance. Security wants the violent and the leaders, Operations the suppliers
// and the unhurried, Commerce the envious and the travelled. High patience is
// hurried and high envy is greedy (traitCatalog).
func portfolioFit(p pilotIdentity, port string) float64 {
	v := 1.0
	switch port {
	case portSecurity:
		v += 0.6*p.trait(TraitAggression) + 0.4*p.trait(TraitLeadership)
	case portOperations:
		v += 0.6*p.trait(TraitSupply) - 0.4*p.trait(TraitPatience)
	case portCommerce:
		v += 0.5*p.trait(TraitGreed) + 0.5*p.trait(TraitWanderlust)
	case portHR:
		// Personnel wants a leader who is not a brawler: the head of the
		// department every applicant meets first.
		v += 0.5*p.trait(TraitLeadership) - 0.3*p.trait(TraitAggression)
	case portInfrastructure:
		// Infrastructure wants the supplier who does not hurry (a tower is a
		// schedule kept for months) and enough of a leader to run a crew.
		v += 0.5*p.trait(TraitSupply) - 0.3*p.trait(TraitPatience) + 0.2*p.trait(TraitLeadership)
	case portResearch:
		// Research wants a researcher by career (a print at ME 20 is weeks of
		// lab time) who does not hurry it.
		v += 0.6*p.Tune.CareerResearch - 0.3*p.trait(TraitPatience) + 0.2*p.trait(TraitLeadership)
	case portDiplomacy:
		// Diplomacy wants a talker, not a brawler, and one who means it:
		// LOW loyalty is the devoted pole (traitCatalog).
		v += 0.5*p.trait(TraitLeadership) - 0.3*p.trait(TraitLoyalty) - 0.4*p.trait(TraitAggression)
	}
	// The loyalist bloc: a member that holds its CEO in regard is dealt a department first.
	v += 0.3 * p.Favour
	return math.Max(0.05, v)
}

// portfolioCand is one director as the department deal scores it.
type portfolioCand struct {
	CharID  int64
	Fit     map[string]float64 // already × performance
	Current string
	Since   int64 // when it took Current
}

// dealPortfolios hands departments to directors. Pure. Incumbents keep theirs unless
// held for portfolioKeepSec and a director with no department outscores them by
// portfolioOutscore; then each vacancy, in `needed` order, goes to the best-fitting
// free director. A director left over runs nothing and is titled plain Director.
func dealPortfolios(cands []portfolioCand, needed []string, now int64) map[int64]string {
	out := map[int64]string{}
	needSet := map[string]bool{}
	for _, p := range needed {
		needSet[p] = true
	}
	sorted := append([]portfolioCand(nil), cands...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CharID < sorted[j].CharID })

	best := func(port string, ok func(portfolioCand) bool) *portfolioCand {
		var b *portfolioCand
		for i := range sorted {
			c := &sorted[i]
			if _, taken := out[c.CharID]; taken || !ok(*c) {
				continue
			}
			if b == nil || c.Fit[port] > b.Fit[port] {
				b = c
			}
		}
		return b
	}
	taken := map[string]bool{}
	for _, port := range needed {
		var inc *portfolioCand
		for i := range sorted {
			if sorted[i].Current == port {
				inc = &sorted[i]
				break
			}
		}
		if inc == nil {
			continue
		}
		if now-inc.Since >= portfolioKeepSec {
			challenger := best(port, func(c portfolioCand) bool {
				return c.CharID != inc.CharID && !needSet[c.Current]
			})
			if challenger != nil && challenger.Fit[port] > inc.Fit[port]*portfolioOutscore {
				continue // the seat falls vacant; the vacancy pass below fills it
			}
		}
		out[inc.CharID] = port
		taken[port] = true
	}
	for _, port := range needed {
		if taken[port] {
			continue
		}
		if b := best(port, func(portfolioCand) bool { return true }); b != nil {
			out[b.CharID] = port
			taken[port] = true
		}
	}
	return out
}

// ceoPortfolios is the roles pass's department deal: it runs after the
// officer roles are written, so the directors it reads are this think's.
// needed is the departments to staff: neededPortfolios without arms, one per
// justified Director-headed arm with them (armNeededPortfolios).
func (m *Mod) ceoPortfolios(ctx context.Context, c *corpCEO, needed []string) error {
	s, err := m.loadCorpStaff(ctx, c.CorpID, c.CharID)
	if err != nil {
		return err
	}
	now := clock.Unix()
	var cands []portfolioCand
	for _, d := range s.Directors {
		fit := map[string]float64{}
		for _, port := range []string{portSecurity, portOperations, portCommerce, portHR, portDiplomacy, portInfrastructure, portResearch} {
			fit[port] = portfolioFit(d.Ident, port) * d.Perf
		}
		cands = append(cands, portfolioCand{CharID: d.CharID, Fit: fit, Current: d.Portfolio, Since: d.PortfolioSince})
	}
	deal := dealPortfolios(cands, needed, now)

	var commerceGone int64
	for _, d := range s.Directors {
		next := deal[d.CharID]
		if next == d.Portfolio {
			continue
		}
		if d.Portfolio == portCommerce {
			commerceGone = d.CharID
		}
		prev := d.Portfolio
		d.Portfolio, d.PortfolioSince = next, now
		switch {
		case next == "":
			m.ceoLog(ctx, c, powerRoles, "department", d.CharID, 0,
				"relieved %s of the %s department", d.Name, portfolioLabel(prev))
		default:
			m.ceoLog(ctx, c, powerRoles, "department", d.CharID, 0,
				"put %s in charge of %s as %s", d.Name, portfolioLabel(next), portfolioTitle(next))
		}
	}
	// A commerce head relieved takes no corporation orders with it, as with the
	// Industry Director (ceoIndustryStaff).
	if commerceGone != 0 {
		if p, err := m.loadCorpPolicy(ctx, c.CorpID); err == nil &&
			commerceGone != p.DirectorID && commerceGone != p.DeputyID {
			m.cancelCorpOrdersOf(ctx, commerceGone)
		}
	}
	return m.saveCorpStaff(ctx, s)
}

func portfolioLabel(port string) string {
	switch port {
	case portSecurity:
		return "Security"
	case portOperations:
		return "Operations"
	case portCommerce:
		return "Commerce"
	case portDiplomacy:
		return "Diplomacy"
	case portHR:
		return "Personnel"
	case portInfrastructure:
		return "Infrastructure"
	case portResearch:
		return "Research"
	}
	return "no"
}

// saveCorpStaff writes the officer rows and drops everybody no longer one.
func (m *Mod) saveCorpStaff(ctx context.Context, s *corpStaff) error {
	want := map[int64]corpPost{}
	for _, sm := range s.all() {
		want[sm.CharID] = corpPost{CorpID: s.CorpID, CharID: sm.CharID, Post: sm.Post, Portfolio: sm.Portfolio,
			Since: sm.PortfolioSince, Perf: sm.Perf, ReviewAt: sm.PerfAt, Metric: sm.Metric}
	}
	if err := m.savePosts(ctx, s.CorpID, famOfficer, want); err != nil {
		return fmt.Errorf("saveCorpStaff: %w", err)
	}
	return nil
}

// ── the desk sweep ──────────────────────────────────────────────────────────

var staffDeskBusy atomic.Bool

// startStaffDeskSweep registers the officers' pass. Gated like the CEO sweep, on the
// master switch only.
func (m *Mod) startStaffDeskSweep() {
	m.registerPeriodic("rolebot-staff-desk", staffDeskEveryTicks, func(uint64) bool {
		if !Master() {
			return true
		}
		if !staffDeskBusy.CompareAndSwap(false, true) {
			return true
		}
		goSafe("staff desk sweep", func() {
			defer staffDeskBusy.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			m.staffDeskPass(ctx)
		})
		return true
	})
}

func (m *Mod) staffDeskPass(ctx context.Context) {
	ceos, err := listCEOs(ctx, m.db)
	if err != nil {
		rlogf("rolebots: staff desk sweep: %v", err)
		return
	}
	for _, c := range ceos {
		if ctx.Err() != nil {
			return
		}
		if c.Paused {
			continue
		}
		// Still this CEO's corporation: the think notices a lost chair on its own cadence,
		// so the staff must not outlive it meanwhile.
		var seat int64
		if err := m.db.QueryRowContext(ctx,
			`SELECT COALESCE(ceoID, 0) FROM corporation WHERE corporationID = ?`, c.CorpID).Scan(&seat); err != nil || seat != c.CharID {
			continue
		}
		lk := corpOpLock(c.CorpID)
		if !lk.TryLock() {
			continue // the CEO is thinking; next pass
		}
		func() {
			defer lk.Unlock()
			if err := m.staffDesks(withOpLockHeld(ctx), c); err != nil {
				m.logBot(c.CharID, logError, "ceo", "staff desks: %v", err)
			}
		}()
	}
}

// staffDesks runs every filled office's desk for one corporation.
func (m *Mod) staffDesks(ctx context.Context, c *corpCEO) error {
	ctx = withThinkCache(ctx, c.CorpID)
	traits, err := getTraits(ctx, m.db, c.CharID)
	if err != nil {
		return err
	}
	ch := m.chairCharter(ctx, c.CorpID, resolveCharter(pilotIdentity{CharID: c.CharID, Traits: traits, Tune: resolveTuning(traits)}))
	s, err := m.loadCorpStaff(ctx, c.CorpID, c.CharID)
	if err != nil {
		return err
	}
	ctx = withCorpStaff(ctx, s)
	// Nobody at the desks: the CEO and every officer are logged off
	// (timezone.go). The corporation waits for one of them, except for the
	// application desk, answered by the CEO's rules after its usual delay so
	// recruitment does not stall while the CEO sleeps, and tower upkeep
	// (towerUpkeep), so the towers do not go dark.
	if !s.anyoneOn() {
		if c.powerOn(powerHR) {
			if err := m.ceoHR(ctx, c, ch); err != nil {
				m.logBot(c.CharID, logError, "ceo", "application desk: %v", err)
			}
		}
		m.towerUpkeep(ctx, c, ch)
		return nil
	}
	facts, err := getAnyCorpFacts(ctx, m.db, c.CorpID)
	if err != nil {
		return err
	}

	if c.powerOn(powerHR) {
		if err := m.personnelDesk(ctx, c, ch, s, facts); err != nil {
			m.logBot(c.CharID, logError, "ceo", "personnel desk: %v", err)
		}
	}
	if c.powerOn(powerHR) {
		m.humanDesk(ctx, c)
	}
	if c.powerOn(powerRoles) {
		m.meritDesk(ctx, c)
	}
	if c.powerOn(powerProduction) {
		m.assignDesk(ctx, c, ch)
	}
	m.newsDesk(ctx, c)
	// The vault (corpvault.go): every original locked down, whatever powers the CEO holds.
	if deskDue(c.CorpID, "lockdown", vaultDeskEverySec) {
		m.vaultDesk(ctx, c)
	}
	if s.Treasurer != nil && c.powerOn(powerTreasury) && s.Treasurer.on() {
		tctx := withStaffActor(ctx, s.executor(s.Treasurer, "Treasurer"))
		if err := m.treasurerDesk(tctx, c, ch, s.Treasurer); err != nil {
			m.logBot(c.CharID, logError, "ceo", "treasurer desk: %v", err)
		}
	}
	for _, d := range s.Directors {
		if d.Portfolio == "" || ctx.Err() != nil || !d.on() {
			continue
		}
		dctx := withStaffActor(ctx, s.executor(d, portfolioTitle(d.Portfolio)))
		var err error
		switch d.Portfolio {
		case portOperations:
			if c.powerOn(powerRoles) {
				err = m.operationsDesk(dctx, c, ch, d, facts)
			}
		case portSecurity:
			if c.powerOn(powerRoles) {
				err = m.securityDesk(dctx, c, ch, d, facts)
			}
		case portCommerce:
			if c.powerOn(powerBusiness) {
				err = m.commerceDesk(dctx, c, ch, d)
			}
		case portDiplomacy:
			if c.powerOn(powerDiplomacy) {
				err = m.diplomacyDesk(dctx, c, ch, d)
			}
		case portInfrastructure:
			if c.powerOn(powerStarbase) {
				err = m.infraDesk(dctx, c, ch, d)
			}
		}
		if err != nil {
			m.logBot(c.CharID, logError, "ceo", "%s desk: %v", portfolioLabel(d.Portfolio), err)
		}
	}
	// The starbase seats' desks (posseats.go): procurement, the trader's disposals and
	// sieges always; fuel and tower defence too while no Director of Infrastructure runs them.
	m.starbaseDesks(ctx, c, ch)
	return nil
}

// deskDue is a per-(corp, desk) cadence: true, and stamps now, when at least every
// seconds have passed since the desk last ran. In memory only: a restart costs one early run.
var deskLast sync.Map // desk key to unix time of the last run

func init() {
	// Keys are "corpID|desk", so one corporation is a prefix scan.
	registerCorpForget("deskLast", func(id int64) {
		prefix := fmt.Sprintf("%d|", id)
		deskLast.Range(func(k, _ any) bool {
			if s, ok := k.(string); ok && strings.HasPrefix(s, prefix) {
				deskLast.Delete(k)
			}
			return true
		})
	})
}

func deskDue(corpID int64, desk string, every int64) bool {
	key := fmt.Sprintf("%d|%s", corpID, desk)
	now := clock.Unix()
	if v, ok := deskLast.Load(key); ok && now-v.(int64) < every {
		return false
	}
	deskLast.Store(key, now)
	return true
}

// ── performance ─────────────────────────────────────────────────────────────

// reviewStaff is the CEO's weekly look at how its officers did: one measurement per
// office (staffMeasure), folded into an EWMA that multiplies into trust and fit, so an
// officer that does its job badly loses it.
func (m *Mod) reviewStaff(ctx context.Context, c *corpCEO, ch Charter) error {
	s, err := m.loadCorpStaff(ctx, c.CorpID, c.CharID)
	if err != nil {
		return err
	}
	now := clock.Unix()
	changed := false
	for _, sm := range s.all() {
		if sm.PerfAt != 0 && now-sm.PerfAt < staffReviewEverySec {
			continue
		}
		score, metric, why := m.staffMeasure(ctx, c, ch, s, sm)
		if sm.PerfAt == 0 {
			// First review: a baseline, no verdict.
			sm.PerfAt, sm.Metric = now, metric
			changed = true
			continue
		}
		old := sm.Perf
		sm.Perf = clampPerf((1-staffPerfAlpha)*sm.Perf + staffPerfAlpha*score)
		sm.PerfAt, sm.Metric = now, metric
		changed = true
		office := staffOffice(sm)
		m.ceoLog(ctx, c, powerRoles, "review", sm.CharID, sm.Perf,
			"reviewed %s as %s: %.2f → %.2f — %s", sm.Name, office, old, sm.Perf, why)
	}
	if !changed {
		return nil
	}
	return m.saveCorpStaff(ctx, s)
}

func staffOffice(sm *staffMember) string {
	switch sm.Post {
	case postTreasurer:
		return "Treasurer"
	case postPersonnel:
		return "Recruitment Officer"
	}
	if t := portfolioTitle(sm.Portfolio); t != "" {
		return t
	}
	return "Director"
}

// staffMeasure scores one officer's week in [0.5, 1.5] around 1, with the
// raw measurement it read (kept for next week's comparison) and a reason.
func (m *Mod) staffMeasure(ctx context.Context, c *corpCEO, ch Charter, s *corpStaff, sm *staffMember) (score, metric float64, why string) {
	since := clock.Unix() - staffReviewEverySec
	acted := m.actorActions(ctx, c.CorpID, sm.CharID, since)
	switch {
	case sm.Post == postTreasurer:
		bal, _ := evedb.GetCorpBalance(ctx, m.db, c.CorpID)
		reserve := m.corpReserve(ctx, c.CorpID, ch)
		score = 1.0
		switch {
		case bal < reserve:
			score, why = 0.7, fmt.Sprintf("the wallet (%s) sits under the reserve (%s)", iskString(bal), iskString(reserve))
		case acted > 0:
			score, why = 1.2, fmt.Sprintf("%d payment(s) made, reserve kept", acted)
		default:
			why = "nothing paid, reserve kept"
		}
		return score, bal, why
	case sm.Post == postPersonnel:
		stale := m.staleApplications(ctx, c.CorpID)
		score = staffTrend(float64(stale), sm.Metric, false)
		if acted > 0 {
			score += 0.1
		}
		return score, float64(stale), fmt.Sprintf("%d application(s) over a day old, %d answered or invited", stale, acted)
	}
	switch sm.Portfolio {
	case portOperations:
		short := m.corpShortfallUnits(ctx, c.CorpID)
		return staffTrend(short, sm.Metric, false), short,
			fmt.Sprintf("factory shortfall %s units (was %s)", fmtCount(short), fmtCount(sm.Metric))
	case portSecurity:
		killed, lost := m.corpKillLoss(ctx, c.CorpID, since)
		score = 1 + 0.4*math.Tanh((killed-lost)/math.Max(killed+lost, 1e7))
		distress := float64(len(m.distressedBeltSystems(ctx, c.CorpID)))
		if distress > sm.Metric {
			score -= 0.1
		}
		return score, distress, fmt.Sprintf("killed %s, lost %s, %0.f belt system(s) in distress",
			iskString(killed), iskString(lost), distress)
	case portCommerce:
		sales := m.corpLineIn(ctx, c.CorpID, lineSales, since)
		return staffTrend(sales, sm.Metric, true), sales, fmt.Sprintf("sales %s this week (was %s)",
			iskString(sales), iskString(sm.Metric))
	case portHR:
		// The head of Personnel answers for the desk its recruiters work.
		stale := m.staleApplications(ctx, c.CorpID)
		return staffTrend(float64(stale), sm.Metric, false), float64(stale),
			fmt.Sprintf("%d application(s) over a day old (was %.0f)", stale, sm.Metric)
	case portInfrastructure:
		return m.infraMeasure(ctx, c, sm)
	case portResearch:
		done := m.corpResearchOrdersDone(ctx, c.CorpID, since)
		return staffTrend(done, sm.Metric, true), done,
			fmt.Sprintf("%.0f research and copy order(s) delivered this week (was %.0f)", done, sm.Metric)
	case portDiplomacy:
		// Did the corporation's relations move the right way this week: the
		// net drift across every pair, less a pact broken on its watch.
		net := m.relNetDrift(ctx, c.CorpID)
		broken := m.actorVerbCount(ctx, c.CorpID, "pactbreak", since)
		score = 1 + 0.4*math.Tanh(net/5) - 0.1*float64(broken)
		return score, net, fmt.Sprintf("relations moved %+.1f in all, %d pact(s) broken", net, broken)
	}
	return 1, 0, "no department"
}

// corpResearchOrdersDone counts the R&D board's orders (ME, PE, copies)
// delivered since a unix time.
func (m *Mod) corpResearchOrdersDone(ctx context.Context, corpID int64, since int64) float64 {
	var n float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_corp_work
		 WHERE corporationID = ? AND status = 'done' AND kind IN (?, ?, ?) AND closedAt >= ?`,
		corpID, workKindME, workKindPE, workKindCopy, since).Scan(&n)
	return n
}

// relNetDrift is the sum, over every pair a corporation holds, of how far
// the value moved since the last diplomacy pass.
func (m *Mod) relNetDrift(ctx context.Context, corpID int64) float64 {
	var v float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(value - prevValue), 0) FROM rolebot_corp_relations
		 WHERE corporationID = ? AND yielded = 0`, corpID).Scan(&v)
	return v
}

func (m *Mod) actorVerbCount(ctx context.Context, corpID int64, verb string, since int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND verb = ? AND at >= ?`, corpID, verb, since).Scan(&n)
	return n
}

// staffTrend scores a measurement against last week's: better is higher
// when up is true, lower when it is false. ±50% moves the score ±0.3.
func staffTrend(now, before float64, up bool) float64 {
	if before <= 0 && now <= 0 {
		return 1
	}
	base := math.Max(before, 1)
	d := (now - before) / base
	if !up {
		d = -d
	}
	return 1 + 0.6*math.Tanh(d)
}

func fmtCount(v float64) string { return fmt.Sprintf("%.0f", v) }

func (m *Mod) actorActions(ctx context.Context, corpID, actorID int64, since int64) int {
	var n int
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_ceo_actions
		 WHERE corporationID = ? AND actorID = ? AND at >= ?`, corpID, actorID, since).Scan(&n)
	return n
}

// staleApplications counts applications left on the desk over a day.
func (m *Mod) staleApplications(ctx context.Context, corpID int64) int {
	var n int
	cutoff := win32Now() - 86400*10_000_000
	_ = m.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM chrApplications
		 WHERE corporationID = ? AND deleted = 0 AND status = 0 AND applicationDateTime < ?`,
		corpID, cutoff).Scan(&n)
	return n
}

// corpLineIn sums one ledger line's income since a time.
func (m *Mod) corpLineIn(ctx context.Context, corpID int64, line string, since int64) float64 {
	var v float64
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM rolebot_corp_ledger
		 WHERE corporationID = ? AND line = ? AND at >= ? AND amount > 0`, corpID, line, since).Scan(&v)
	return v
}

// corpKillLoss is the ISK a corporation's members killed and lost since a unix time,
// off the killboard in the war code's indexed shape (corpKillEfficiency): attacker
// rows by killID range, never by time.
func (m *Mod) corpKillLoss(ctx context.Context, corpID int64, since int64) (killed, lost float64) {
	if minKill := m.minKillSince(ctx, since); minKill > 0 {
		_ = m.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(k.totalValue), 0)
			  FROM killmails k
			  JOIN (SELECT DISTINCT killID FROM killmail_attackers
			         WHERE corpID = ? AND killID >= ?) a ON a.killID = k.killID
			 WHERE k.victimCorpID <> ?`, corpID, minKill, corpID).Scan(&killed)
	}
	_ = m.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(totalValue), 0) FROM killmails WHERE victimCorpID = ? AND killTime >= ?`,
		corpID, since).Scan(&lost)
	return killed, lost
}
