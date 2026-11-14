package rolebots

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Open sites: the settings, the claims, the memory of a bad one, and how a
// pilot finds one worth running.
//
// The core seeds dungeons as shared instances with a beacon on every overview
// (cmd/gurimoon/opensites.go). A combat pilot treats one as a trip of its
// own (combattactic.go: tacticSite) and a hunt pack as a destination
// (gangplan.go: gangSite): both ask openSiteCandidates, which reads the live
// sites through the modapi seam, grades each by its card (sitecard.go) and
// hands back the ones this side can win, best value per jump first.

const (
	settingSitesEnabled   = "rolebots.sites.enabled"
	settingSitesMaxJumps  = "rolebots.sites.maxJumps"
	settingSitesClaimTTL  = "rolebots.sites.claimTTLMin"
	settingSitesFailSkip  = "rolebots.sites.failSkipMin"
	settingSitesWeightPct = "rolebots.sites.weightPct"
	settingSitesLootStruc = "rolebots.sites.lootStructures"
	settingSitesContend   = "rolebots.sites.contenders"

	defaultSitesEnabled   = 1
	defaultSitesMaxJumps  = 6
	defaultSitesClaimTTL  = 30
	defaultSitesFailSkip  = 90
	defaultSitesWeightPct = 160
	defaultSitesLootStruc = 1
	defaultSitesContend   = 3

	// helpSite is the help-call kind a lone pilot posts when a site in reach
	// is beyond it but not beyond a hunt pack (gangcalls.go reads the kind).
	helpSite = "site"

	// siteRunMaxSec bounds one run: a site a pilot has been inside this long
	// is one it is not finishing.
	siteRunMaxSec = 45 * 60
)

var (
	sitesEnabledVal   atomic.Int64
	sitesMaxJumpsVal  atomic.Int64
	sitesClaimTTLVal  atomic.Int64
	sitesFailSkipVal  atomic.Int64
	sitesWeightPctVal atomic.Int64
	sitesLootStrucVal atomic.Int64
	sitesContendVal   atomic.Int64
)

func init() {
	registerSweep("siteClaims", sweepSiteClaims)
	sitesEnabledVal.Store(defaultSitesEnabled)
	sitesMaxJumpsVal.Store(defaultSitesMaxJumps)
	sitesClaimTTLVal.Store(defaultSitesClaimTTL)
	sitesFailSkipVal.Store(defaultSitesFailSkip)
	sitesWeightPctVal.Store(defaultSitesWeightPct)
	sitesLootStrucVal.Store(defaultSitesLootStruc)
	sitesContendVal.Store(defaultSitesContend)
}

// SitesEnabled is the master switch; it is also off on a core without the seam.
func SitesEnabled() bool { return sitesEnabledVal.Load() != 0 && modapi.CanRunSites() }

// SitesMaxJumps is how far a pilot looks for a site.
func SitesMaxJumps() int { return int(sitesMaxJumpsVal.Load()) }

// SitesClaimTTL is how long one pilot's (or pack's) claim on a site holds
// without renewal.
func SitesClaimTTL() time.Duration { return time.Duration(sitesClaimTTLVal.Load()) * time.Minute }

// SitesFailSkip is how long a site (and its dungeon) is avoided after a run
// that broke off or died.
func SitesFailSkip() time.Duration { return time.Duration(sitesFailSkipVal.Load()) * time.Minute }

// SitesWeight is the tactic's weight against the belt sweep's 1.0.
func SitesWeight() float64 { return float64(sitesWeightPctVal.Load()) / 100 }

// SitesLootStructures: 0 = structures are never shot, 1 = those carrying
// authored loot, 2 = every destructible one.
func SitesLootStructures() int { return int(sitesLootStrucVal.Load()) }

// SitesContenders is how many parties may race one site at once, the holder
// included (co-runners are the holder's and do not count). 1 = no racing.
func SitesContenders() int { return int(sitesContendVal.Load()) }

func (m *Mod) loadSiteSettings(ctx context.Context) {
	for _, s := range []struct {
		key      string
		val      *atomic.Int64
		min, max int64
	}{
		{settingSitesEnabled, &sitesEnabledVal, 0, 1},
		{settingSitesMaxJumps, &sitesMaxJumpsVal, 0, 30},
		{settingSitesClaimTTL, &sitesClaimTTLVal, 1, 600},
		{settingSitesFailSkip, &sitesFailSkipVal, 0, 1440},
		{settingSitesWeightPct, &sitesWeightPctVal, 0, 1000},
		{settingSitesLootStruc, &sitesLootStrucVal, 0, 2},
		{settingSitesContend, &sitesContendVal, 1, 10},
	} {
		v, ok, err := evedb.GetServerSetting(ctx, m.db, s.key)
		if err != nil || !ok {
			continue
		}
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= s.min && n <= s.max {
			s.val.Store(n)
		}
	}
}

// setSiteSetting writes one setting and re-reads them all (the ESP page).
func (m *Mod) setSiteSetting(ctx context.Context, key string, value int64) error {
	switch key {
	case settingSitesEnabled, settingSitesMaxJumps, settingSitesClaimTTL,
		settingSitesFailSkip, settingSitesWeightPct, settingSitesLootStruc, settingSitesContend:
	default:
		return fmt.Errorf("unknown site setting %q", key)
	}
	if err := evedb.SetServerSetting(ctx, m.db, key, strconv.FormatInt(value, 10)); err != nil {
		return err
	}
	m.loadSiteSettings(ctx)
	return nil
}

// ── claims ──────────────────────────────────────────────────────────────────

// A claim is one party's intent to run a site (a pilot or a pack), in memory per
// process. Modes: holder, co-run, assist (race for kills, then leave) and rival
// fight (siteetiquette.go). Humans are not in it; the verdict reads them off the grid.
type claimMode uint8

const (
	claimHold   claimMode = iota // the party running the site
	claimCoRun                   // a friendly party fighting the holder's fight
	claimAssist                  // a stranger racing the holder for the kills
	claimFight                   // a rival the rules of engagement let shoot the holder
	claimLeave                   // the verdict only: walk away, nothing is stored
)

func (c claimMode) String() string {
	switch c {
	case claimHold:
		return "hold"
	case claimCoRun:
		return "co-run"
	case claimAssist:
		return "assist"
	case claimFight:
		return "fight"
	}
	return "leave"
}

// siteParty is who asks: the pilot, its pack (0 for a lone pilot), and the
// corporation, which is what two parties are told apart by.
type siteParty struct {
	CharID, GangID, CorpID int64
	// RivalCorp is the corporation a fight-mode party came for.
	RivalCorp int64
}

func partyOf(b *botRun, gangID int64) siteParty {
	if b == nil {
		return siteParty{GangID: gangID}
	}
	return siteParty{CharID: b.CharID, GangID: gangID, CorpID: b.CorpID}
}

type siteClaim struct {
	CharID int64
	GangID int64
	CorpID int64
	Mode   claimMode
	// RivalCorp is who a claimFight party came for (siteRivalCorps).
	RivalCorp int64
	// Kills is what this party has killed on the site, for the contest
	// incident's share (siteetiquette.go).
	Kills int64
	Until time.Time
}

var (
	siteClaimMu sync.Mutex
	siteClaims  = map[int64][]siteClaim{}
	// siteCalledAt throttles the help call a lone pilot posts about a site
	// beyond it (one per claim TTL per site).
	siteCalledAt = map[int64]time.Time{}
)

func (c siteClaim) owns(charID, gangID int64) bool {
	if c.GangID != 0 && gangID != 0 {
		return c.GangID == gangID
	}
	return c.CharID == charID
}

// liveClaimsLocked drops a site's expired entries and returns the rest.
// siteClaimMu held.
func liveClaimsLocked(siteID int64, now time.Time) []siteClaim {
	cs := siteClaims[siteID]
	live := cs[:0]
	for _, c := range cs {
		if now.Before(c.Until) {
			live = append(live, c)
		}
	}
	if len(live) == 0 {
		delete(siteClaims, siteID)
		return nil
	}
	siteClaims[siteID] = live
	return live
}

// claimSite takes or renews a party's place on a site. A listed party keeps its
// mode; the oldest party becomes holder once the holder has gone; an empty site is
// taken as holder. Otherwise the party joins with its verdict's mode, an assist
// only while fewer than SitesContenders race, never as a second party of one corp.
func claimSite(siteID int64, p siteParty, mode claimMode) bool {
	now := clock.Now()
	siteClaimMu.Lock()
	defer siteClaimMu.Unlock()
	cs := liveClaimsLocked(siteID, now)
	until := now.Add(SitesClaimTTL())
	for i := range cs {
		if cs[i].owns(p.CharID, p.GangID) {
			cs[i].Until = until
			if cs[i].Mode != claimHold && !claimsHaveHolder(cs) {
				cs[i].Mode = claimHold // the holder left: the run is ours now
			}
			return true
		}
	}
	if len(cs) == 0 {
		siteClaims[siteID] = []siteClaim{{CharID: p.CharID, GangID: p.GangID, CorpID: p.CorpID, Mode: claimHold, Until: until}}
		return true
	}
	switch mode {
	case claimHold, claimLeave:
		return false
	case claimAssist:
		if racingParties(cs) >= SitesContenders() {
			return false
		}
	}
	for _, c := range cs {
		if p.CorpID != 0 && c.CorpID == p.CorpID && mode != claimCoRun {
			return false
		}
	}
	siteClaims[siteID] = append(cs, siteClaim{CharID: p.CharID, GangID: p.GangID, CorpID: p.CorpID, Mode: mode,
		RivalCorp: p.RivalCorp, Until: until})
	return true
}

func claimsHaveHolder(cs []siteClaim) bool {
	for _, c := range cs {
		if c.Mode == claimHold {
			return true
		}
	}
	return false
}

// racingParties counts the parties running a site on their own account (a
// co-runner is the holder's fight, not a contender).
func racingParties(cs []siteClaim) int {
	n := 0
	for _, c := range cs {
		if c.Mode != claimCoRun {
			n++
		}
	}
	return n
}

// siteClaimMode is the mode a party holds a site in (ok=false: not in it).
func siteClaimMode(siteID, charID, gangID int64) (claimMode, bool) {
	siteClaimMu.Lock()
	defer siteClaimMu.Unlock()
	for _, c := range liveClaimsLocked(siteID, clock.Now()) {
		if c.owns(charID, gangID) {
			return c.Mode, true
		}
	}
	return claimLeave, false
}

// noteSiteClaimKills credits a party's kills on a site (the contest share).
func noteSiteClaimKills(siteID, charID, gangID, n int64) {
	if n <= 0 {
		return
	}
	siteClaimMu.Lock()
	defer siteClaimMu.Unlock()
	cs := siteClaims[siteID]
	for i := range cs {
		if cs[i].owns(charID, gangID) {
			cs[i].Kills += n
			return
		}
	}
}

// releaseSite drops the claim the pilot (or pack) holds, and nobody else's.
func releaseSite(siteID, charID, gangID int64) {
	siteClaimMu.Lock()
	defer siteClaimMu.Unlock()
	cs := siteClaims[siteID]
	out := cs[:0]
	for _, c := range cs {
		if !c.owns(charID, gangID) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		delete(siteClaims, siteID)
		return
	}
	siteClaims[siteID] = out
}

// dropSiteOffHunt takes a pack's site off its blackboard, and its claim off the
// board, when the hunt moves to a system the site is not in (a bounty intercept,
// a distress call, a Director's order), so the claim does not linger for its TTL.
func dropSiteOffHunt(gs *GangState, gangID, sysID int64) {
	if gs == nil || gs.SiteID == 0 {
		return
	}
	if modapi.CanRunSites() {
		if s, ok := modapi.Sites().Get(gs.SiteID); ok && s.SolarSystemID == sysID {
			return
		}
	}
	releaseSite(gs.SiteID, 0, gangID)
	gs.SiteID, gs.SiteName = 0, ""
}

// siteClaimsOf is a copy of a site's live claims, holder first, for the
// arrival verdict and the board.
func siteClaimsOf(siteID int64) []siteClaim {
	siteClaimMu.Lock()
	defer siteClaimMu.Unlock()
	cs := liveClaimsLocked(siteID, clock.Now())
	out := make([]siteClaim, len(cs))
	copy(out, cs)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Mode == claimHold && out[j].Mode != claimHold })
	return out
}

// sweepSiteClaims drops expired claims and the ledgers of sites that are
// gone (the cache sweep, cachesweep.go).
func sweepSiteClaims() {
	now := clock.Now()
	live := map[int64]bool{}
	if modapi.CanRunSites() {
		for _, s := range modapi.Sites().List() {
			live[s.SiteID] = true
		}
	}
	siteClaimMu.Lock()
	for id := range siteClaims {
		if !live[id] {
			delete(siteClaims, id)
			continue
		}
		liveClaimsLocked(id, now)
	}
	for id, at := range siteCalledAt {
		if now.Sub(at) > SitesClaimTTL() || !live[id] {
			delete(siteCalledAt, id)
		}
	}
	siteClaimMu.Unlock()
	siteFiredMu.Lock()
	for id := range siteFired {
		if !live[id] {
			delete(siteFired, id)
		}
	}
	siteFiredMu.Unlock()
	siteLockMu.Lock()
	for id := range siteLocks {
		if !live[id] {
			delete(siteLocks, id) // a holder keeps its pointer; nobody new needs a dead site
		}
	}
	siteLockMu.Unlock()
	siteSkipMu.Lock()
	for k, until := range siteSkip {
		if !now.Before(until) {
			delete(siteSkip, k)
		}
	}
	siteSkipMu.Unlock()
}

// ── the per-site lock ───────────────────────────────────────────────────────

// One site is one runner's event pass at a time, per site and not sharded.
// Passes side by side would fight the same rats or release two chained waves on
// one clear. See docs/comment-notes-b17.md#site-lock.
var (
	siteLockMu sync.Mutex
	siteLocks  = map[int64]*sync.Mutex{}
)

func siteLock(siteID int64) *sync.Mutex {
	siteLockMu.Lock()
	defer siteLockMu.Unlock()
	l := siteLocks[siteID]
	if l == nil {
		l = &sync.Mutex{}
		siteLocks[siteID] = l
	}
	return l
}

// ── the memory of a bad one ─────────────────────────────────────────────────

// A run that broke off or died marks the site and its dungeon for the scope that
// flew it (hostilemem.go's intelScope: the corporation, and the pack), so the next
// pilot of that corp avoids the room until the memory fades.

type siteSkipKey struct {
	holder int64
	id     int64 // siteID, or -dungeonVID
}

var (
	siteSkipMu sync.Mutex
	siteSkip   = map[siteSkipKey]time.Time{}
)

func noteSiteFail(sc intelScope, siteID, vid int64) {
	until := clock.Now().Add(SitesFailSkip())
	siteSkipMu.Lock()
	defer siteSkipMu.Unlock()
	for _, h := range sc.holders() {
		if siteID != 0 {
			siteSkip[siteSkipKey{h, siteID}] = until
		}
		if vid != 0 {
			siteSkip[siteSkipKey{h, -vid}] = until
		}
	}
}

func siteSkipped(sc intelScope, siteID, vid int64) bool {
	now := clock.Now()
	siteSkipMu.Lock()
	defer siteSkipMu.Unlock()
	for _, h := range sc.holders() {
		if until, ok := siteSkip[siteSkipKey{h, siteID}]; ok && now.Before(until) {
			return true
		}
		if until, ok := siteSkip[siteSkipKey{h, -vid}]; ok && now.Before(until) {
			return true
		}
	}
	return false
}

// ── candidates ──────────────────────────────────────────────────────────────

// siteCand is one live site this side could run.
type siteCand struct {
	Site   modapi.SiteLive
	Card   *siteCard
	Jumps  int
	Margin float64
	// Fleet marks a site the side's power fails the engage gate on but is not
	// hopeless against: a hunt pack's job, not a lone pilot's.
	Fleet bool
	Score float64
	// Mode is the arrival verdict: claimHold for a site nobody is running.
	Mode claimMode
}

// siteOthers counts the parties on a site other than this one.
func siteOthers(siteID, charID, gangID int64) int {
	n := 0
	for _, c := range siteClaimsOf(siteID) {
		if !c.owns(charID, gangID) {
			n++
		}
	}
	return n
}

// openSiteCandidates is every live site within reach this side could run, best
// value per jump first. dps/ehp/rep are the side's power as siteSidePower
// folds it; known=false grades nothing and returns every site in reach as a
// candidate the way the belt walk does for an unread hull.
func (m *Mod) openSiteCandidates(ctx context.Context, b *botRun, cfg combatConfig,
	dps, ehp, rep float64, known bool, sc intelScope, gangID int64) []siteCand {
	if !SitesEnabled() || b == nil {
		return nil
	}
	sites := modapi.Sites().List()
	if len(sites) == 0 {
		return nil
	}
	u, err := m.universe(ctx)
	if err != nil {
		return nil
	}
	near := hopsWithin(u, b.SystemID, SitesMaxJumps())
	mx, mxOK := m.standingsMatrix(ctx)
	mine := b.FactionID
	if !mxOK {
		mine = 0 // combatConfigDerived's rule: no matrix, no employer filter
	}
	pirates := m.pirateFactionIDs(ctx)
	huntable := map[int64]bool{} // per faction, once per call
	var out []siteCand
	for _, s := range sites {
		if s.ClearedAt != 0 {
			continue
		}
		jumps, ok := near[s.SolarSystemID]
		if !ok {
			continue
		}
		ms, ok := u.meta[s.SolarSystemID]
		if !ok || ms.Security < cfg.SecMin || ms.Security > cfg.SecMax {
			continue
		}
		if siteSkipped(sc, s.SiteID, s.DungeonVID) {
			continue
		}
		card, err := m.siteCardFor(ctx, s.DungeonVID)
		if err != nil || card == nil || card.Rats == 0 {
			continue
		}
		// Somebody may be running it already (siteetiquette.go): a party that
		// would only walk away on arrival does not fly there.
		mode := m.siteArrivalFor(ctx, b, gangID, s, card)
		if mode == claimLeave {
			continue
		}
		// Whose rats: siteFactionHuntable (faction.go). A factionless
		// dungeon is hostile to everybody.
		ok, seen := huntable[card.FactionID]
		if !seen {
			ok = siteFactionHuntable(mx, mine, card.FactionID, pirates, cfg.FactionIDs, b.Profile)
			huntable[card.FactionID] = ok
		}
		if !ok {
			continue
		}
		c := siteCand{Site: s, Card: card, Jumps: jumps, Mode: mode}
		if known {
			c.Margin = siteMarginRep(dps, ehp, rep, card)
			if hopeless(c.Margin) {
				continue
			}
			c.Fleet = !siteWouldRun(b.Tune, c.Margin)
		}
		// What is still alive, shared with whoever else is on it.
		c.Score = siteRemainingEV(card, s.Killed) / float64(1+jumps) / float64(1+siteOthers(s.SiteID, b.CharID, gangID))
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Fleet != out[j].Fleet {
			return !out[i].Fleet // solo-runnable first
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Site.SiteID < out[j].Site.SiteID
	})
	return out
}

// siteFinishSlack is the headroom over the bare finish line: the duel runs
// in whole ticks and the last rat can outlive the arithmetic by one.
const siteFinishSlack = 1.1

// siteWouldRun is the site gate: the pilot's nerve (wouldEngage) and the margin
// to finish. The duel breaks off once hull drops under BreakOffFrac, so finishing
// takes margin >= 1/(1-BreakOffFrac), above the engage line; a site past the solo
// line goes to a pack instead.
func siteWouldRun(tn Tuning, margin float64) bool {
	if !wouldEngage(tn, margin) {
		return false
	}
	return margin >= siteFinishMargin(tn)*siteFinishSlack
}

// siteFinishMargin is the margin at which a hull still stands above its
// break-off when the field's last rat dies.
func siteFinishMargin(tn Tuning) float64 {
	b := tn.BreakOffFrac
	if b < 0 {
		b = 0
	}
	if b > 0.9 {
		b = 0.9
	}
	return 1 / (1 - b)
}

// siteTacticWeight is the site tactic's weight for this pilot: the setting when a
// site it can run alone is in reach, else nothing. When the only sites in reach are
// within a pack's power, a help call goes on the board (one per site per claim TTL).
func (m *Mod) siteTacticWeight(ctx context.Context, b *botRun, cfg combatConfig) float64 {
	if !SitesEnabled() || b.State == nil || b.State.Data.GangID != 0 {
		return 0
	}
	dps, ehp, rep, known := siteSidePower(m.effectiveHulls(ctx, b), b.Tune.BreakOffFrac)
	cands := m.openSiteCandidates(ctx, b, cfg, dps, ehp, rep, known, botScope(b), 0)
	if len(cands) == 0 {
		return 0
	}
	if !cands[0].Fleet {
		return SitesWeight()
	}
	m.callForSitePack(ctx, b, cands[0])
	return 0
}

// callForSitePack posts the help call a lone pilot raises about a site it
// cannot run alone. Note carries the site so an answering pack knows what it
// is converging on (parseSiteCall).
func (m *Mod) callForSitePack(ctx context.Context, b *botRun, c siteCand) {
	if b.CorpID == 0 {
		return
	}
	now := clock.Now()
	siteClaimMu.Lock()
	if at, ok := siteCalledAt[c.Site.SiteID]; ok && now.Sub(at) < SitesClaimTTL() {
		siteClaimMu.Unlock()
		return
	}
	siteCalledAt[c.Site.SiteID] = now
	siteClaimMu.Unlock()
	m.postHelpCall(ctx, 0, b.CorpID, c.Site.SolarSystemID, helpSite, tierHuntLo,
		fmt.Sprintf("site:%d:%s", c.Site.SiteID, c.Card.Name))
}

// parseSiteCall reads the site out of a help call's note (0 when it is not a
// site call).
func parseSiteCall(kind, note string) (siteID int64, name string) {
	if kind != helpSite || !strings.HasPrefix(note, "site:") {
		return 0, ""
	}
	parts := strings.SplitN(strings.TrimPrefix(note, "site:"), ":", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, ""
	}
	if len(parts) > 1 {
		name = parts[1]
	}
	return id, name
}

// pickSite is the tactic's Site: the best site this pilot can run alone,
// claimed. (sysID, siteID, label).
func (m *Mod) pickSite(ctx context.Context, b *botRun, cfg combatConfig) (int64, int64, string, error) {
	dps, ehp, rep, known := siteSidePower(m.effectiveHulls(ctx, b), b.Tune.BreakOffFrac)
	for _, c := range m.openSiteCandidates(ctx, b, cfg, dps, ehp, rep, known, botScope(b), 0) {
		if c.Fleet {
			break
		}
		if _, ok := m.takeSiteClaim(ctx, b, 0, c.Site, c.Card); !ok {
			continue
		}
		return c.Site.SolarSystemID, c.Site.SiteID,
			fmt.Sprintf("%s in %s", c.Card.Name, m.nameOfSystem(ctx, c.Site.SolarSystemID)), nil
	}
	return 0, 0, "", nil
}

// gangSiteCandidate is the site a hunt pack takes instead of a belt sweep: the
// best site the pooled power passes the gate on, claimed for the pack. Nil means
// the pack sweeps belts.
func (m *Mod) gangSiteCandidate(ctx context.Context, b *botRun, g *Gang, cfg combatConfig,
	dps, ehp float64, known bool) *siteCand {
	if !SitesEnabled() || g == nil {
		return nil
	}
	// A site is graded the way the duel fights it: the pack's guns against its
	// weakest hull (sitefocus.go), with the side's repair (focusRep) riding along.
	rep := 0.0
	if fd, fe, fr, ok := siteSidePower(m.effectiveHulls(ctx, b), b.Tune.BreakOffFrac); ok {
		dps, ehp, rep, known = fd, fe, fr, ok
	}
	for _, c := range m.openSiteCandidates(ctx, b, cfg, dps, ehp, rep, known, gangScope(g), g.GangID) {
		if c.Fleet {
			continue
		}
		if _, ok := m.takeSiteClaim(ctx, b, g.GangID, c.Site, c.Card); !ok {
			continue
		}
		c := c
		return &c
	}
	return nil
}

// siteFleetRunnable is the most a fleet can ever bring to a site: every fighting
// hull it has, wherever it is, at full hull, graded by the commander's arrival
// gate (focusPower, siteWouldRun). False means no member still on the road can
// make the difference; unknown power is false.
func siteFleetRunnable(tn Tuning, hulls []hullPower, card *siteCard) bool {
	if card == nil {
		return false
	}
	dps, ehp, rep, known := siteSidePower(hulls, tn.BreakOffFrac)
	if !known {
		return false
	}
	return siteWouldRun(tn, siteMarginRep(dps, ehp, rep, card))
}

// siteFleetScope is a fleet's own memory of a site it turned down: the fleet,
// never its corporation. gangScope would carry the corp, and a site too heavy
// for three frigates says nothing about the corp's cruiser pilots running it
// alone.
func siteFleetScope(g *Gang) intelScope {
	if g == nil {
		return intelScope{}
	}
	return intelScope{gangID: g.GangID}
}

// gangMayAnswerSiteCall is a fleet's grade on a kin pilot's site call
// (gangcalls.go gangAnswerHelp). That the caller could not run the site alone says
// nothing about this fleet, so it is graded on the commander's temper, since the
// commander's hold is the gate the fleet will meet.
func (m *Mod) gangMayAnswerSiteCall(ctx context.Context, b *botRun, g *Gang, siteID int64) bool {
	if !SitesEnabled() || g == nil || b == nil {
		return false
	}
	s, ok := modapi.Sites().Get(siteID)
	if !ok || s.ClearedAt != 0 {
		return false
	}
	if siteSkipped(gangScope(g), siteID, s.DungeonVID) {
		return false
	}
	card, err := m.siteCardFor(ctx, s.DungeonVID)
	if err != nil || card == nil || card.Rats == 0 {
		return false
	}
	if m.siteArrivalFor(ctx, b, g.GangID, s, card) == claimLeave {
		return false
	}
	tn := b.Tune
	if g.LeaderCharID != 0 && g.LeaderCharID != b.CharID {
		if lr, err := m.loadRun(ctx, g.LeaderCharID); err == nil && lr != nil {
			tn = lr.Tune
		}
	}
	return siteFleetRunnable(tn, m.effectiveHulls(ctx, b), card)
}
