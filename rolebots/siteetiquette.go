package rolebots

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gurimoon/modapi"
	"gurimoon/seclaw"
)

// Site etiquette: what a party does when it finds somebody already running the
// site it came for (a shared instance, cmd/gurimoon/opensites.go). It asks one
// question, once, and keeps the answer on its claim. The verdict, all in siteArrivalVerdict:
//
//  1. a kinsman or a friend: co-run, one fight, the holder's
//  2. a rival the law lets it shoot: fight (roe.go decides, as on a belt)
//  3. a holder who can manage alone: leave them to it
//  4. a holder who looks to need it: assist, race for the kills, leave when the
//     site is done, while fewer than SitesContenders parties are racing
//  5. otherwise: leave

// siteArrivalFacts are the answers the verdict reads, gathered by
// siteArrivalFor.
type siteArrivalFacts struct {
	Kin, Friendly   bool // toward the holder: never a race, never a fight
	Fight           bool // the rules of engagement and the odds both say go
	HolderCanHandle bool // the holder, as it stands, would pass its own gate
	Full            bool // SitesContenders parties are already racing
}

// siteArrivalVerdict is the whole of the etiquette, pure.
func siteArrivalVerdict(f siteArrivalFacts) claimMode {
	switch {
	case f.Kin || f.Friendly:
		return claimCoRun
	case f.Fight:
		return claimFight
	case f.HolderCanHandle, f.Full:
		return claimLeave
	}
	return claimAssist
}

// siteRemainingEV is what a site still pays: the bounty of every rat not yet
// dead, the rats' share of the loot tables by head count, and the authored loot
// of every structure still standing.
func siteRemainingEV(card *siteCard, killed map[int64]bool) float64 {
	if card == nil {
		return 0
	}
	var bounty, structLoot float64
	alive, rats := 0, 0
	for i := range card.Rooms {
		for _, w := range card.Rooms[i].Waves {
			for _, r := range w.Rats {
				if r == nil {
					continue
				}
				rats++
				if !killed[r.SiteObjectID] {
					alive++
					bounty += r.Bounty
				}
			}
			for _, s := range w.Structs {
				if !killed[s.ObjectID] {
					structLoot += s.LootEV
				}
			}
		}
	}
	ratLoot := card.LootEV - card.StructLootEV
	if rats > 0 && ratLoot > 0 {
		ratLoot *= float64(alive) / float64(rats)
	} else {
		ratLoot = 0
	}
	return bounty + ratLoot + structLoot
}

// siteHolder is who is running a site: a bot party off the claim list, else
// a connected human on its grid.
type siteHolder struct {
	CharID, CorpID, GangID int64
	Human                  bool
}

func (m *Mod) siteHolderOf(ctx context.Context, site modapi.SiteLive) (siteHolder, bool) {
	if cs := siteClaimsOf(site.SiteID); len(cs) > 0 {
		return siteHolder{CharID: cs[0].CharID, CorpID: cs[0].CorpID, GangID: cs[0].GangID}, true
	}
	if !site.Loaded || modapi.Sites().PilotsAt == nil {
		return siteHolder{}, false
	}
	for _, charID := range modapi.Sites().PilotsAt(site.SiteID) {
		var corpID int64
		if err := m.db.QueryRowContext(ctx,
			`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&corpID); err != nil {
			continue
		}
		return siteHolder{CharID: charID, CorpID: corpID, Human: true}, true
	}
	return siteHolder{}, false
}

// siteHullsNow is a party's hulls as they stand: current EHP, not the full hull
// botHull reports, since managing depends on the ship now in the site, scorched
// by the rooms it has cleared.
func (m *Mod) siteHullsNow(ctx context.Context, b *botRun) []hullPower {
	var out []hullPower
	add := func(r *botRun) {
		if r == nil || r.ShipID == 0 {
			return
		}
		c, err := m.botCombatant(ctx, r)
		if err != nil || c == nil || c.MaxEHP <= 0 {
			return
		}
		out = append(out, hullPower{DPS: c.DPS, EHP: c.EHP, MaxEHP: c.MaxEHP,
			SelfRep: c.SelfRepPerSec, RemoteRep: c.RepPerSec, PerHP: ehpPerHP(c)})
	}
	add(b)
	if b != nil && b.State != nil && b.State.Data.GangID != 0 {
		if mates, err := m.gangCombatRoster(ctx, b.State.Data.GangID, b.CharID); err == nil {
			for _, mate := range mates {
				if mate.SystemID == b.SystemID {
					add(mate)
				}
			}
		}
	}
	return out
}

// holderHulls sizes the holder up. A human is read off its boarded ship (the
// character row's own entity location) through the same combatant fold a bot
// gets: botCombatant needs a pilot and a ship and nothing of the roster.
func (m *Mod) holderHulls(ctx context.Context, h siteHolder) ([]hullPower, *botRun) {
	if !h.Human {
		run, err := m.loadRun(ctx, h.CharID)
		if err != nil || run == nil {
			return nil, nil
		}
		return m.siteHullsNow(ctx, run), run
	}
	var shipID int64
	if err := m.db.QueryRowContext(ctx,
		`SELECT locationID FROM entity WHERE itemID = ?`, h.CharID).Scan(&shipID); err != nil || shipID == 0 {
		return nil, nil
	}
	return m.siteHullsNow(ctx, &botRun{CharID: h.CharID, ShipID: shipID, CorpID: h.CorpID}), nil
}

// siteArrivalFor is a party's verdict on a site, gathered and decided. A
// party already on the site gets the mode it holds; an empty site is taken.
func (m *Mod) siteArrivalFor(ctx context.Context, b *botRun, gangID int64, site modapi.SiteLive, card *siteCard) claimMode {
	if mode, ok := siteClaimMode(site.SiteID, b.CharID, gangID); ok {
		return mode
	}
	h, ok := m.siteHolderOf(ctx, site)
	if !ok {
		return claimHold
	}
	var f siteArrivalFacts
	self := b.standingSelf()
	f.Kin = h.CorpID == b.CorpID || m.kinToPilot(ctx, self, h.CharID)
	if !f.Kin {
		f.Friendly = m.standingToPilot(ctx, self, h.CharID) >= StandingLiked
	}
	theirs, holderRun := m.holderHulls(ctx, h)
	if !f.Kin && !f.Friendly {
		f.Fight = m.siteRivalLegal(ctx, b, gangID, h, site.SolarSystemID, theirs)
	}
	// Courtesy is the safe default: a holder this side cannot read is assumed to be
	// managing. Judged on the holder's temper, or ours when it is a human.
	tn := b.Tune
	if holderRun != nil {
		tn = holderRun.Tune
	}
	f.HolderCanHandle = true
	if dps, ehp, rep, known := siteSidePower(theirs, tn.BreakOffFrac); known {
		f.HolderCanHandle = siteWouldRun(tn, siteMarginRep(dps, ehp, rep, card))
	}
	racing := racingParties(siteClaimsOf(site.SiteID))
	if h.Human {
		racing++ // the human is a party too, just not one on the list
	}
	f.Full = racing >= SitesContenders()
	return siteArrivalVerdict(f)
}

// siteRivalLegal is whether this side would open on the holder: the rules of
// engagement (roeEngage, the sticky per-pair decision a belt patrol uses) and the
// odds, this side's guns and tank against the holder party's as they stand.
func (m *Mod) siteRivalLegal(ctx context.Context, b *botRun, gangID int64, h siteHolder, sysID int64,
	theirs []hullPower) bool {
	doc := m.corpDoctrineFor(ctx, b.CorpID)
	if doc == DoctrineNone {
		return false
	}
	band := secBand(m.systemSecurity(ctx, sysID))
	read := m.standingMotiveToPilotFor(ctx, b.standingSelf(), h.CharID, true, doc)
	st := stanceOf(read.Standing, false)
	criminal := false
	if since := modapi.PvP().CriminalSince; since != nil {
		criminal = since(sysID, h.CharID) > 0
	}
	var sec float64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(securityRating, 0) FROM character_ WHERE characterID = ?`, h.CharID).Scan(&sec)
	outlaw := sec <= seclaw.OutlawStatus
	atWar := warEverywhere() && relAtWar(b.CorpID, m.corpAllianceID(ctx, b.CorpID), h.CorpID, m.corpAllianceID(ctx, h.CorpID))
	decider := b.CharID
	nerve := b.Tune.NeutralAggression
	if gangID != 0 {
		decider = -gangID
		nerve = m.gangNerve(ctx, gangID, nerve)
	}
	if ok, _ := roeEngage(st, doc, band, criminal, outlaw, atWar, nerve, decider, h.CharID); !ok {
		return false
	}
	var oDPS, oEHP, tDPS, tEHP float64
	for _, x := range m.siteHullsNow(ctx, b) {
		oDPS += x.DPS
		oEHP += x.EHP
	}
	for _, x := range theirs {
		tDPS += x.DPS
		tEHP += x.EHP
	}
	return wouldEngage(b.Tune, engageMargin(oDPS, oEHP, tDPS, tEHP))
}

// takeSiteClaim is a pick: the verdict, then the claim it allows. ok=false
// means walk away (or the place went while we decided).
func (m *Mod) takeSiteClaim(ctx context.Context, b *botRun, gangID int64, site modapi.SiteLive, card *siteCard) (claimMode, bool) {
	mode := m.siteArrivalFor(ctx, b, gangID, site, card)
	if mode == claimLeave {
		return mode, false
	}
	p := partyOf(b, gangID)
	if mode == claimFight {
		if h, ok := m.siteHolderOf(ctx, site); ok {
			p.RivalCorp = h.CorpID
		}
	}
	if !claimSite(site.SiteID, p, mode) {
		return claimLeave, false
	}
	// What is on the list is what we are: an empty site went to us as holder
	// whatever the verdict was.
	held, _ := siteClaimMode(site.SiteID, b.CharID, gangID)
	if held != claimHold {
		m.logBot(b.CharID, logInfo, "site", "%s is already being run — %s", card.Name, siteModeLine(held))
	}
	return held, true
}

func siteModeLine(mode claimMode) string {
	switch mode {
	case claimCoRun:
		return "friends of ours; joining their run"
	case claimAssist:
		return "they look to need a hand; racing them for it"
	case claimFight:
		return "rivals, and the law will not stop us"
	}
	return "leaving them to it"
}

// siteCoRunners are the friendly parties fighting this holder's fight: the pilots
// of every co-run claim on the site standing in this system, with their packs.
// The holder's duel fields them (prepareAbstractPackWith) and pays them through
// the same bounty split; their own passes wait (siteRunStep).
func (m *Mod) siteCoRunners(ctx context.Context, b *botRun, siteID int64) []*botRun {
	var out []*botRun
	seen := map[int64]bool{b.CharID: true}
	for _, c := range siteClaimsOf(siteID) {
		if c.Mode != claimCoRun || seen[c.CharID] {
			continue
		}
		run, err := m.loadRun(ctx, c.CharID)
		if err != nil || run == nil || run.State == nil || run.SystemID != b.SystemID ||
			run.State.Data.SiteID != siteID || !m.exposedInSpace(run) || m.isCapsule(ctx, run.ShipID) {
			continue
		}
		seen[run.CharID] = true
		out = append(out, run)
		for _, mate := range m.packMatesFor(ctx, run) {
			if !seen[mate.CharID] {
				seen[mate.CharID] = true
				out = append(out, mate)
			}
		}
	}
	return out
}

// fileSiteContest files the contest incident an assist leaves behind: one per site
// per corporation pair (the dedupe key), weighed by the share of the kills this
// party took. Kin and co-runners never file.
func (m *Mod) fileSiteContest(ctx context.Context, b *botRun, gangID, siteID, sysID int64, name string) {
	if b == nil || b.CorpID == 0 || !relLedgerOn() {
		return
	}
	cs := siteClaimsOf(siteID)
	var mine *siteClaim
	for i := range cs {
		if cs[i].owns(b.CharID, gangID) {
			mine = &cs[i]
		}
	}
	if mine == nil || mine.Mode != claimAssist || mine.Kills <= 0 {
		return
	}
	for _, other := range cs {
		if other.owns(b.CharID, gangID) || other.CorpID == 0 || other.CorpID == b.CorpID ||
			other.Mode == claimCoRun || other.Mode == claimFight {
			continue
		}
		share := float64(mine.Kills) / float64(mine.Kills+other.Kills)
		w := relBaseWeight(incContest, relSideMutual, 0, relParamsNow()) * share
		lo, hi := b.CorpID, other.CorpID
		if lo > hi {
			lo, hi = hi, lo
		}
		key := fmt.Sprintf("contest:%d:%d:%d", siteID, lo, hi)
		detail := "racing for a deadspace site"
		if name != "" {
			detail = "racing for " + name
		}
		m.noteCorpIncident(ctx, b.CorpID, other.CorpID, incContest, relSideMutual, w, 0, sysID, siteID, key, detail)
		m.noteCorpIncident(ctx, other.CorpID, b.CorpID, incContest, relSideMutual, w, 0, sysID, siteID, key, detail)
	}
}

// siteBeaten is whether this party's run, ending as "cleared", was really
// somebody else's: another party is on the list and this one took no more
// than a third of the kills between them.
func siteBeaten(cs []siteClaim, charID, gangID int64) bool {
	var mine, total int64
	others := false
	for _, c := range cs {
		total += c.Kills
		if c.owns(charID, gangID) {
			mine = c.Kills
		} else if c.Mode != claimCoRun {
			others = true
		}
	}
	return others && total > 0 && mine*3 <= total
}

// siteRivalCorps is who a party on a site is fighting: as a rival, the
// corporation it came for; as anybody else, every rival that came for it.
// Read on the combat goroutine (hostilesOnGrid), so it touches only the claim list.
func siteRivalCorps(siteID, charID, gangID, corpID int64) map[int64]bool {
	cs := siteClaimsOf(siteID)
	out := map[int64]bool{}
	for _, c := range cs {
		if c.owns(charID, gangID) {
			if c.Mode == claimFight && c.RivalCorp != 0 {
				out[c.RivalCorp] = true
			}
			continue
		}
		if c.Mode == claimFight && c.RivalCorp == corpID && c.CorpID != 0 {
			out[c.CorpID] = true
		}
	}
	return out
}

// siteRivalFightGap is how often one party opens on its site rival in the paper
// tier: a rival that broke off and stayed is fought again, but not every pass.
const siteRivalFightGap = 3 * time.Minute

var (
	siteRivalFoughtMu sync.Mutex
	siteRivalFought   = map[[2]int64]time.Time{} // (siteID, party key) to last fight
)

// siteRivalFight is the paper tier's fight with a site rival: a party with a rival
// (siteRivalCorps) on its site in this system fights it before the rats, by the
// lawful patrol's engageResolve with the encounter roll skipped. Reports whether a
// fight was resolved; a lost fight has reset the pilot (loseHull).
func (m *Mod) siteRivalFight(ctx context.Context, b *botRun, g *Gang, site modapi.SiteLive) bool {
	gangID := int64(0)
	leader := b.CharID
	if g != nil {
		gangID = g.GangID
		if g.LeaderCharID != 0 {
			leader = g.LeaderCharID
		}
	}
	rivals := siteRivalCorps(site.SiteID, b.CharID, gangID, b.CorpID)
	if len(rivals) == 0 {
		return false
	}
	key := [2]int64{site.SiteID, b.CharID}
	if gangID != 0 {
		key[1] = -gangID
	}
	now := clock.Now()
	siteRivalFoughtMu.Lock()
	if at, ok := siteRivalFought[key]; ok && now.Sub(at) < siteRivalFightGap {
		siteRivalFoughtMu.Unlock()
		return false
	}
	siteRivalFoughtMu.Unlock()

	var target *botRun
	for _, c := range siteClaimsOf(site.SiteID) {
		if c.owns(b.CharID, gangID) || !rivals[c.CorpID] {
			continue
		}
		run, err := m.loadRun(ctx, c.CharID)
		if err != nil || run == nil || run.SystemID != site.SolarSystemID || !m.exposedInSpace(run) {
			continue
		}
		target = run
		break
	}
	if target == nil {
		return false
	}
	lb, _, ours, ok := m.engageSide(ctx, leader, site.SolarSystemID, g, nil)
	if !ok {
		return false
	}
	var sec, bounty float64
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(securityRating, 0), COALESCE(bounty, 0) FROM character_ WHERE characterID = ?`,
		target.CharID).Scan(&sec, &bounty)
	outlaw := sec <= seclaw.OutlawStatus
	atWar := warEverywhere() && relAtWar(lb.CorpID, m.corpAllianceID(ctx, lb.CorpID),
		target.CorpID, m.corpAllianceID(ctx, target.CorpID))
	why := motiveCorpRival
	switch {
	case outlaw:
		why = motiveOutlaw
	case atWar:
		why = motiveWar
	}
	siteRivalFoughtMu.Lock()
	siteRivalFought[key] = now
	siteRivalFoughtMu.Unlock()
	m.logBot(b.CharID, logInfo, "site", "a rival party is on the site — engaging %s before the rats", target.Name)
	return m.engageResolve(ctx, lb, site.SolarSystemID, g, ours,
		[]engageCand{{charID: target.CharID, outlaw: outlaw, atWar: atWar, bounty: bounty, why: why}})
}

// siteDropOnDeath clears a dead pilot off the site it was running: a lone pilot's
// claim goes (a pack's stays, the survivors are still in there), and its
// corporation keeps away from that site for the fail window. Called from the
// abstract kill (killBotShip) before the reset.
func (m *Mod) siteDropOnDeath(ctx context.Context, charID, corpID int64) {
	st, err := loadState(ctx, m.db, charID)
	if err != nil || st == nil || st.Data.SiteID == 0 {
		return
	}
	if st.Data.GangID == 0 {
		releaseSite(st.Data.SiteID, charID, 0)
	}
	noteSiteFail(intelScope{corpID: corpID}, st.Data.SiteID, 0)
}

// sweepSiteRivalFights drops stale fight stamps (the cache sweep).
func sweepSiteRivalFights() {
	now := clock.Now()
	siteRivalFoughtMu.Lock()
	for k, at := range siteRivalFought {
		if now.Sub(at) > time.Hour {
			delete(siteRivalFought, k)
		}
	}
	siteRivalFoughtMu.Unlock()
}

func init() { registerSweep("siteRivalFights", sweepSiteRivalFights) }
