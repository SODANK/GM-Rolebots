package rolebots

import (
	"context"
	"fmt"
	"math"
	"math/rand"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Running an open site: the task, and the paper half of the run.
//
// A site is several fights in sequence: room by room, the walker releases what the
// trigger chain says is out (sitewave.go), fights it with the belt encounter's duel
// (resolvePackDuel), tells the core what died through the bridge, opens the wrecks,
// shoots the structures holding a prize and takes the gate. One duel per sim pass.
// The embodied half (siteembodied.go) owns a site whose system a player has loaded;
// the step here only polls the record then.

// TaskCombatSite is the solo trip (combattactic.go: tacticSite). Not Interruptible:
// a pilot half way down a site holds unpaid kills, a part-full hold and a claim
// other pilots honour.
const TaskCombatSite TaskKind = "combat.site"

func init() {
	registerTask(TaskDef{
		Kind: TaskCombatSite, Label: "Running a deadspace site",
		Posture: PostureCombatBelt, Exposed: true, Loots: true,
		Detail: detailSite,
		Step:   (*Mod).siteStep,
	})
}

func detailSite(m *Mod, ctx context.Context, st *SimState) string {
	if st.Data.SiteID == 0 {
		return ""
	}
	s, ok := modapi.Sites().Get(st.Data.SiteID)
	if !ok {
		return "the site has gone"
	}
	card, err := m.siteCardFor(ctx, s.DungeonVID)
	if err != nil || card == nil {
		return fmt.Sprintf("site %d", st.Data.SiteID)
	}
	room := int(st.Data.SiteRoomIdx) + 1
	if room > len(card.Rooms) {
		room = len(card.Rooms)
	}
	return fmt.Sprintf("%s — room %d of %d, %d kill(s) so far", card.Name, room, len(card.Rooms), st.Data.SiteKills)
}

// siteOutcome is where one pass left the run.
type siteOutcome int

const (
	siteRunning siteOutcome = iota
	siteCleared             // every room done
	siteAborted             // broke off or declined
	siteGone                // despawned or cleared under us
	siteDead                // the pilot lost its ship
	siteWaiting             // the embodied tier owns it this pass
)

// siteStep is the solo task's step. Travel, loot resumption and the sale at
// the end follow the belt sweep's shape (combatStep).
func (m *Mod) siteStep(ctx context.Context, b *botRun) (bool, error) {
	st := b.State
	if st.Data.SiteID == 0 || st.Data.HuntSystemID == 0 {
		st.Activity, st.Phase = "", string(TaskIdle)
		return false, saveState(ctx, m.db, st)
	}
	if st.Data.HuntLootID != 0 {
		if handled, err := m.combatResumeLoot(ctx, b); err != nil || handled {
			return false, err
		}
	}
	if b.SystemID != st.Data.HuntSystemID {
		return false, m.beginTravel(ctx, m.withSiteClaim(b), st.Data.HuntSystemID, 0, st.Phase)
	}
	if b.ShipID == 0 {
		return false, fmt.Errorf("site runner has no ship boarded")
	}
	out, err := m.siteRunStep(ctx, b, st.Data.SiteID, nil)
	if err != nil {
		return false, err
	}
	switch out {
	case siteRunning, siteWaiting:
		return false, saveState(ctx, m.db, st)
	case siteDead:
		return false, nil // loseHull has rewritten the state
	}
	// Over, one way or another: the haul goes home, or the pilot is idle.
	releaseSite(st.Data.SiteID, b.CharID, 0)
	st.Data.HuntRecent = pushHuntRecent(st.Data.HuntRecent, st.Data.HuntSystemID)
	st.Data.SiteID, st.Data.SiteRoomIdx, st.Data.SiteEntered = 0, 0, false
	if rows, err := m.lootLedger(ctx, b.CharID); err == nil && len(rows) > 0 {
		return false, m.combatStartSale(ctx, b, "site over")
	}
	st.Activity, st.Phase = "", string(TaskIdle)
	return false, saveState(ctx, m.db, st)
}

// withSiteClaim renews the claim while the pilot is still on its way. A place
// lost meanwhile is found out on arrival (siteRunStep).
func (m *Mod) withSiteClaim(b *botRun) *botRun {
	if b != nil && b.State != nil && b.State.Data.SiteID != 0 {
		claimSite(b.State.Data.SiteID, partyOf(b, b.State.Data.GangID), claimHold)
	}
	return b
}

// siteRunStep is one pass of a run, shared by the solo task and the hunt pack
// (gangplan.go). g is the pack, nil for a lone pilot. The caller owns what
// happens to the task on a non-running outcome; this records the run.
func (m *Mod) siteRunStep(ctx context.Context, b *botRun, siteID int64, g *Gang) (siteOutcome, error) {
	st := b.State
	gangID := int64(0)
	if g != nil {
		gangID = g.GangID
	}
	// The record read through the end of the pass is one step (siteLock): a
	// party sharing the site waits for this pass and then reads what it left.
	lk := siteLock(siteID)
	lk.Lock()
	defer lk.Unlock()
	site, ok := modapi.Sites().Get(siteID)
	if !ok || site.SolarSystemID != b.SystemID {
		m.logBot(b.CharID, logInfo, "site", "the site is gone — nothing left to run")
		m.recordSiteRun(ctx, b, siteID, 0, gangID, "gone")
		return siteGone, nil
	}
	card, err := m.siteCardFor(ctx, site.DungeonVID)
	if err != nil {
		return siteGone, err
	}
	// Renew our place, or ask again for one lost: the arrival verdict decides, and a
	// party the site has no room for leaves.
	mode, held := siteClaimMode(siteID, b.CharID, gangID)
	if !held || !claimSite(siteID, partyOf(b, gangID), mode) {
		if mode, held = m.takeSiteClaim(ctx, b, gangID, site, card); !held {
			m.logBot(b.CharID, logInfo, "site", "%s has no room for us any more — leaving it to the others", card.Name)
			m.recordSiteRun(ctx, b, siteID, card.DungeonVID, gangID, "crowded")
			return siteAborted, nil
		}
	}
	if st.Data.SiteStartedAt == 0 {
		st.Data.SiteStartedAt = clock.Unix()
	}
	if clock.Unix()-st.Data.SiteStartedAt > siteRunMaxSec {
		m.logBot(b.CharID, logInfo, "site", "%s has taken too long — leaving it", card.Name)
		noteSiteFail(botScope(b), siteID, card.DungeonVID)
		m.recordSiteRun(ctx, b, siteID, card.DungeonVID, gangID, "timeout")
		return siteAborted, nil
	}
	p := siteProgressFor(siteID, site.Killed, site.Released)
	// Done is: every rat and prize dead (siteDone); or the core stamped the site
	// cleared with nothing we would shoot alive; or the party's projections left the
	// last room. While any of them is still on the site (sweeping its cans, say) the
	// record alone does not end the run.
	gridOn, gridDone := siteGridState(siteID, b.CharID, gangID)
	if gridDone || (!gridOn && siteRunOver(card, p, site.ClearedAt != 0, site.Loaded)) {
		m.logBot(b.CharID, logInfo, "site", "%s is clear — %d kill(s) on this run", card.Name, st.Data.SiteKills)
		m.recordSiteRun(ctx, b, siteID, card.DungeonVID, gangID, "cleared")
		return siteCleared, nil
	}

	// A loaded system is the embodied tier's; here we only wait. Non-commander pack
	// members wait (packMatesFor), as does a co-runner (siteCoRunners).
	if site.Loaded || m.systemProjected(b.SystemID) || (g != nil && g.LeaderCharID != b.CharID) || mode == claimCoRun {
		st.Data.LegEndsAt = clock.Unix() + 20
		return siteWaiting, nil
	}
	// A rival on the site where the law allows it (siteetiquette.go): the
	// fight comes before the rats. A lost fight has reset the pilot already.
	if m.siteRivalFight(ctx, b, g, site) {
		if fresh, err := loadState(ctx, m.db, b.CharID); err != nil || fresh == nil || fresh.Data.SiteID != siteID {
			return siteDead, nil
		}
		st.Data.LegEndsAt = clock.Unix() + siteInterWaveSec
		return siteRunning, nil
	}
	// A pack picks its site on the whole roster's power, but the duel fights only the
	// ones on grid, so the commander holds the first room until the pack here passes the gate.
	if g != nil && st.Data.SiteRoomIdx == 0 && !st.Data.SiteEntered {
		if dps, ehp, rep, known := m.sitePackOnSite(ctx, b); known && !siteWouldRun(b.Tune, siteMarginRep(dps, ehp, rep, card)) {
			// Waiting is only worth it while the ships on the road would carry the gate;
			// otherwise leave now and remember it (the fleet only).
			if !siteFleetRunnable(b.Tune, m.effectiveHulls(ctx, b), card) {
				m.logBot(b.CharID, logInfo, "site",
					"%s is too much for this fleet even with every ship on site (%.0f DPS / %.0f EHP here) — leaving it",
					card.Name, dps, ehp)
				noteSiteFail(siteFleetScope(g), siteID, card.DungeonVID)
				m.recordSiteRun(ctx, b, siteID, card.DungeonVID, gangID, "short")
				return siteAborted, nil
			}
			if clock.Unix()-st.Data.SiteStartedAt > siteMusterMaxSec {
				m.logBot(b.CharID, logInfo, "site", "the fleet never gathered at %s — leaving it", card.Name)
				noteSiteFail(siteFleetScope(g), siteID, 0)
				m.recordSiteRun(ctx, b, siteID, card.DungeonVID, gangID, "short")
				return siteAborted, nil
			}
			m.logBot(b.CharID, logInfo, "site", "holding off %s until the pack is here — %.0f DPS / %.0f EHP on site",
				card.Name, dps, ehp)
			st.Data.LegEndsAt = clock.Unix() + 20
			return siteWaiting, nil
		}
	}
	return m.siteAbstractPass(ctx, b, site, card, p, gangID)
}

// siteGridState reads the party's projections on a site: this pilot's and, for a
// pack, every gangmate's. on is whether any is running it; done is whether every
// one of them has finished it (siteEmbodyTick sets siteDone after the last room's
// field sweep).
func siteGridState(siteID, charID, gangID int64) (on, done bool) {
	embodyMu.Lock()
	party := make([]*embodiedBot, 0, 4)
	for id, eb := range embodyLive {
		if id == charID || (gangID != 0 && eb.gangID == gangID) {
			party = append(party, eb)
		}
	}
	embodyMu.Unlock()
	done = true
	for _, eb := range party {
		eb.mu.Lock()
		if eb.siteID == siteID && !eb.dead {
			on = true
			done = done && eb.siteDone
		}
		eb.mu.Unlock()
	}
	return on, on && done
}

// siteMusterMaxSec is how long a commander holds a site's first room for its
// pack before giving the site up. The fleet notes the site (never the corp's
// pilots), so it does not answer the next call for the same site.
const siteMusterMaxSec = 10 * 60

// sitePackOnSite is the power the duel would field right now: this pilot and
// the gangmates packMatesFor puts in its fight, folded the way the duel
// fights (siteSidePower: the side's guns, the weakest hull's tank, the repair
// on it), at full hull.
func (m *Mod) sitePackOnSite(ctx context.Context, b *botRun) (dps, ehp, rep float64, known bool) {
	var hulls []hullPower
	if h, ok := m.botHull(ctx, b); ok {
		hulls = append(hulls, h)
	}
	for _, mate := range m.packMatesFor(ctx, b) {
		if h, ok := m.botHull(ctx, mate); ok {
			hulls = append(hulls, h)
		}
	}
	return siteSidePower(hulls, b.Tune.BreakOffFrac)
}

// siteDone: every room's reachable rats and shootable loot structures are
// dead and no wave can still come out.
func siteDone(card *siteCard, p *siteProgress) bool {
	if card == nil {
		return false
	}
	for i := range card.Rooms {
		room := &card.Rooms[i]
		if len(roomLiveRats(room, p)) > 0 || len(roomPendingWaves(room, p)) > 0 {
			return false
		}
		if SitesLootStructures() > 0 && len(roomLiveStructs(room, p, SitesLootStructures() == 2)) > 0 {
			return false
		}
	}
	return true
}

// siteRunOver is the paper run's end test: siteDone, or the core has stamped the
// site cleared, nothing released is alive, and no structure we would shoot stands.
// The structure half matters: the core stamps the clear the pass before the
// structure step, so without it no runner opened the loot container.
func siteRunOver(card *siteCard, p *siteProgress, cleared, loaded bool) bool {
	if siteDone(card, p) {
		return true
	}
	if !cleared || loaded || !siteQuiet(card, p) {
		return false
	}
	if lvl := SitesLootStructures(); lvl > 0 {
		for i := range card.Rooms {
			if len(roomLiveStructs(&card.Rooms[i], p, lvl == 2)) > 0 {
				return false
			}
		}
	}
	return true
}

// siteQuiet: nothing left alive that was ever released. The core may stamp a
// site cleared while a loot structure still stands (structures are not in its
// census), so the clear alone is not the end (siteRunOver).
func siteQuiet(card *siteCard, p *siteProgress) bool {
	for i := range card.Rooms {
		if len(roomLiveRats(&card.Rooms[i], p)) > 0 {
			return false
		}
	}
	return true
}

// siteAbstractPass is one step of the paper run: enter the room, fight what is
// out, shoot the structures, or take the gate: one of those per pass.
func (m *Mod) siteAbstractPass(ctx context.Context, b *botRun, site modapi.SiteLive,
	card *siteCard, p *siteProgress, gangID int64) (siteOutcome, error) {
	st := b.State
	idx := int(st.Data.SiteRoomIdx)
	if idx >= len(card.Rooms) {
		m.recordSiteRun(ctx, b, site.SiteID, card.DungeonVID, gangID, "cleared")
		return siteCleared, nil
	}
	room := &card.Rooms[idx]
	markWaves := func(waves []int64) {
		for _, w := range waves {
			if modapi.Sites().MarkWave(site.SiteID, room.RoomID, w) {
				p.release(room.RoomID, w)
				m.logBot(b.CharID, logInfo, "site", "a wave comes out in room %d of %s", idx+1, card.Name)
			}
		}
		noteSiteFired(site.SiteID, p)
	}

	if !st.Data.SiteEntered {
		st.Data.SiteEntered = true
		m.logBot(b.CharID, logInfo, "site", "entering room %d of %d in %s", idx+1, len(card.Rooms), card.Name)
		markWaves(siteReleases(room, p, "roomEnter", 0))
	}

	// The fight.
	if alive := roomLiveRats(room, p); len(alive) > 0 {
		out, won, err := m.siteDuel(ctx, b, site.SiteID, card, alive, gangID)
		if err != nil {
			return siteRunning, err
		}
		if out == siteDead {
			m.recordSiteRun(ctx, b, site.SiteID, card.DungeonVID, gangID, "lost")
			return siteDead, nil
		}
		// The duel already told the core (first wins, siteDuel); only the rats this side
		// won raise npcKilled here, since whoever won the others replayed theirs.
		killedObjs := 0
		for _, r := range alive {
			if r.EHP <= 0 {
				killedObjs++
				p.Killed[r.SiteObjectID] = true
			}
		}
		for _, r := range won {
			markWaves(siteReleases(room, p, "npcKilled", r.SiteObjectID))
		}
		st.Data.SiteKills += int64(len(won))
		noteSiteClaimKills(site.SiteID, b.CharID, gangID, int64(len(won)))
		if out == siteAborted {
			// Broke off with the field already dead: the hull is too far gone to stay, but the
			// site was not more than it looked, so it is not avoided and the run is no failure.
			outcome := "withdrew"
			if killedObjs < len(alive) {
				// On a shared site the field may hold a wave another party let
				// out: the site was more than it looked, the dungeon was not.
				vid := card.DungeonVID
				if siteOthers(site.SiteID, b.CharID, gangID) > 0 {
					vid = 0
				}
				noteSiteFail(botScope(b), site.SiteID, vid)
				outcome = "aborted"
			}
			m.recordSiteRun(ctx, b, site.SiteID, card.DungeonVID, gangID, outcome)
			return siteAborted, nil
		}
		// Between waves: a pause for the repairers (settlePackHulls wrote the damage, so
		// the gap is the only thing that mends it; siteInterWaveRepair).
		m.siteInterWaveRepair(ctx, b, siteInterWaveSec)
		st.Data.LegEndsAt = clock.Unix() + siteInterWaveSec
		return siteRunning, nil
	}

	// Quiet room: the clear may release something. A repeatable trigger names its
	// wave on every clear; one already out is no progress (siteUnreleased).
	rel := siteUnreleased(room, p, siteReleases(room, p, "roomCleared", 0))
	if len(rel) == 0 {
		// Nothing authored came out: the next auto wave, if any, does.
		if w := siteAutoWave(room, p); w > 0 {
			rel = []int64{w}
		}
	}
	if len(rel) > 0 {
		// The next wave comes out only if the pack, as hurt as it is now, would stay for
		// it: the gate graded the worst single field, not fields back to back.
		if verdict, leave := m.siteWaveTooMuch(ctx, b, site.SiteID, room, p, rel, gangID); leave {
			// The prizes first: structures do not shoot back or release a wave, so a hurt
			// pack can still crack the container. Nothing here persists the trigger ledger.
			if lvl := SitesLootStructures(); lvl > 0 {
				if structs := roomLiveStructs(room, p, lvl == 2); len(structs) > 0 {
					secs := m.siteShootStructures(ctx, b, site.SiteID, card, structs, p)
					st.Data.LegEndsAt = clock.Unix() + secs
					return siteRunning, nil
				}
			}
			m.logBot(b.CharID, logInfo, "site",
				"%s: not staying for the next wave on these hulls — leaving it (%s)", card.Name, verdict)
			// This site only, not the dungeon: the card was right, the pack spent itself.
			// Without the skip it re-picks the site at once on full-hull power (botPower).
			noteSiteFail(botScope(b), site.SiteID, 0)
			m.recordSiteRun(ctx, b, site.SiteID, card.DungeonVID, gangID, "withdrew")
			return siteAborted, nil
		}
		markWaves(rel)
		st.Data.LegEndsAt = clock.Unix() + 10
		return siteRunning, nil
	}

	// The structures the author put something in.
	if SitesLootStructures() > 0 {
		if structs := roomLiveStructs(room, p, SitesLootStructures() == 2); len(structs) > 0 {
			secs := m.siteShootStructures(ctx, b, site.SiteID, card, structs, p)
			st.Data.LegEndsAt = clock.Unix() + secs
			return siteRunning, nil
		}
	}

	// The gate, or the end.
	if room.Gate != nil && idx+1 < len(card.Rooms) {
		markWaves(siteReleases(room, p, "gateActivated", room.Gate.ObjectID))
		m.logBot(b.CharID, logInfo, "site", "room %d of %s is done — taking the gate", idx+1, card.Name)
		st.Data.SiteRoomIdx++
		st.Data.SiteEntered = false
		st.Data.LegEndsAt = clock.Unix() + 25 // the gate's warp
		return siteRunning, nil
	}
	if idx+1 < len(card.Rooms) {
		// A deeper room with no gate to it is authored unreachable; the site is
		// as done as it gets.
		m.logBot(b.CharID, logInfo, "site", "room %d of %s has no gate onward — the site is done", idx+1, card.Name)
	}
	st.Data.SiteRoomIdx = int64(len(card.Rooms))
	m.recordSiteRun(ctx, b, site.SiteID, card.DungeonVID, gangID, "cleared")
	return siteCleared, nil
}

// siteWaveTooMuch runs the duel's own engage test (siteDuel) on the field a release
// would bring out, against the pack's current hulls, before anything is released: a
// wave released into a pack that then aligns out costs hulls for nothing. An
// unreadable pack is not a reason to leave.
func (m *Mod) siteWaveTooMuch(ctx context.Context, b *botRun, siteID int64, room *siteRoom,
	p *siteProgress, waves []int64, gangID int64) (string, bool) {
	spawn := siteReleaseField(room, p, waves)
	if len(spawn) == 0 {
		return "", false
	}
	pp, err := m.prepareAbstractPackWith(ctx, b, spawn, m.siteCoRunners(ctx, b, siteID))
	if err != nil || pp == nil {
		return "", false
	}
	decider := b
	if gangID != 0 && len(pp.pack) > 1 {
		if g, _, err := gangForChar(ctx, m.db, b.CharID); err == nil && g != nil {
			decider = packDecider(b, pp.pack, g.LeaderCharID)
		}
	}
	// The gate's test, not the duel's bare nerve: the field has to die
	// before the hull the rats will focus crosses its break-off line.
	dps, ehp, rep, ok := siteSidePower(packHulls(pp.pack), decider.Tune.BreakOffFrac)
	if !ok {
		return "", false
	}
	margin := threatMarginRep(dps, ehp, rep, spawnThreatOf(spawn))
	if siteWouldRun(decider.Tune, margin) {
		return "", false
	}
	return marginVerdict(decider.Tune, margin), true
}

// siteReleaseField is the field a release would leave on grid: the room's
// rats still alive plus the not-yet-dead rats of each wave named, as fresh
// copies.
func siteReleaseField(room *siteRoom, p *siteProgress, waves []int64) []*ratThreat {
	spawn := roomLiveRats(room, p)
	for _, n := range waves {
		w := room.wave(n)
		if w == nil || p.released(room.RoomID, n) {
			continue
		}
		for _, r := range w.Rats {
			if r != nil && !p.Killed[r.SiteObjectID] {
				spawn = append(spawn, copyRat(r))
			}
		}
	}
	return spawn
}

// siteInterWaveSec is the pause between one wave and the next in the paper
// run: long enough for a repairer to matter, short enough that a site is not
// an afternoon.
const siteInterWaveSec = 30

// siteDuel fights one field with the belt encounter's own machinery (the pack,
// the commander's nerve, the pre-fight look, the duel, hulls, bounty, wrecks). It
// returns the rats this side won: each dead rat is told to the core first, and
// only those the core hears first (MarkKilled is first-wins) earn a bounty and wreck.
func (m *Mod) siteDuel(ctx context.Context, b *botRun, siteID int64, card *siteCard,
	spawn []*ratThreat, gangID int64) (siteOutcome, []*ratThreat, error) {
	pp, err := m.prepareAbstractPackWith(ctx, b, spawn, m.siteCoRunners(ctx, b, siteID))
	if err != nil {
		return siteRunning, nil, fmt.Errorf("site combatant: %w", err)
	}
	pack, self := pp.pack, pp.self
	decider := b
	if gangID != 0 && len(pack) > 1 {
		if g, _, err := gangForChar(ctx, m.db, b.CharID); err == nil && g != nil {
			decider = packDecider(b, pack, g.LeaderCharID)
		}
	}
	markCommander(pack, decider) // its break-off is the side's (resolvePackDuel)
	breakOff := decider.Tune.BreakOffFrac
	if !pp.armed {
		breakOff = decider.Tune.UnarmedBreakOffFrac
	}
	field := spawnThreatOf(spawn)
	margin := threatMargin(pp.sideDPS, pp.standEHP, field)
	declined := !wouldEngage(decider.Tune, margin)
	ratDPS := 0.0
	for _, r := range spawn {
		ratDPS += r.DPS
	}
	with := ""
	if len(pack) > 1 {
		with = fmt.Sprintf(", with %d gangmate(s)", len(pack)-1)
	}
	m.logBot(b.CharID, logInfo, "site", "%s: %s — %.0f incoming DPS against %.0f EHP, answering with %.0f DPS%s",
		card.Name, describeSpawn(spawn), ratDPS, pp.sideEHP, pp.sideDPS, with)
	if declined {
		m.logBot(b.CharID, logInfo, "threat",
			"this room is more than it looked — aligning out (%s)", marginVerdict(decider.Tune, margin))
	}

	out := resolvePackDuel(pack, spawn, breakOff, declined, rand.Float64)
	stampPackExposure(pack, b.CharID)

	won := siteWonKills(spawn, func(objID int64) bool { return modapi.Sites().MarkKilled(siteID, objID) })
	if len(won) > 0 {
		m.payNPCBounties(ctx, packBountyShares(pack), won, b.SystemID)
	}
	self = m.settlePackHulls(ctx, b, pack, spawn)
	if self == nil || self.Bot.EHP <= 0 {
		return siteDead, won, nil
	}
	// Break-off is per hull (resolvePackDuel) and the rats focus the commander, so a
	// dead field is the pack's however the commander left: take the wrecks and go on.
	fieldDead := out.Killed >= len(spawn)
	if len(won) > 0 && (!self.Escaped || fieldDead) {
		looter := b
		if self.Escaped {
			for _, p := range pack {
				if p.onField() {
					looter = p.Run
					break
				}
			}
		}
		m.lootAbstractKills(ctx, looter, won)
		m.lootSiteObjects(ctx, looter, card, won)
	}
	fleetLeft := false // the commander called it and the side warped as one
	for _, p := range pack {
		fleetLeft = fleetLeft || (p.FleetCall && p.Escaped)
	}
	switch {
	case self.Escaped && fieldDead && fleetLeft:
		m.logBot(b.CharID, logInfo, "site",
			"the fleet withdrew from %s at %.0f%% hull integrity — the field died on the way out (%d kill(s))",
			card.Name, self.Bot.EHP/self.Bot.MaxEHP*100, out.Killed)
		return siteRunning, won, nil
	case self.Escaped && fieldDead:
		m.logBot(b.CharID, logInfo, "site",
			"broke off in %s at %.0f%% hull integrity — the pack finished the room behind it (%d kill(s))",
			card.Name, self.Bot.EHP/self.Bot.MaxEHP*100, out.Killed)
		return siteRunning, won, nil
	case self.Escaped && fleetLeft:
		m.logBot(b.CharID, logInfo, "site",
			"called the withdrawal from %s at %.0f%% hull integrity — the fleet warped out together (%d kill(s))",
			card.Name, self.Bot.EHP/self.Bot.MaxEHP*100, out.Killed)
		return siteAborted, won, nil
	case self.Escaped:
		m.logBot(b.CharID, logInfo, "site",
			"disengaged and warped clear of %s at %.0f%% hull integrity (%d kill(s) on the way out)",
			card.Name, self.Bot.EHP/self.Bot.MaxEHP*100, out.Killed)
		return siteAborted, won, nil
	case out.Killed >= len(spawn):
		m.logBot(b.CharID, logInfo, "site", "the room is quiet — %d kill(s), holding at %.0f%% hull integrity",
			out.Killed, self.Bot.EHP/self.Bot.MaxEHP*100)
	default:
		m.logBot(b.CharID, logInfo, "site", "the engagement broke up — %d of %d killed, %.0f%% hull integrity left",
			out.Killed, len(spawn), self.Bot.EHP/self.Bot.MaxEHP*100)
	}
	return siteRunning, won, nil
}

// siteWonKills tells the core about every dead rat of a duel and keeps the
// ones it heard first (mark is MarkKilled: true only for the first report).
// Pure but for the callback, so the first-wins rule is testable without a core.
func siteWonKills(spawn []*ratThreat, mark func(objID int64) bool) []*ratThreat {
	var won []*ratThreat
	for _, r := range spawn {
		if r == nil || r.EHP > 0 || r.SiteObjectID == 0 {
			continue
		}
		if mark(r.SiteObjectID) {
			won = append(won, r)
		}
	}
	return won
}

// lootSiteObjects takes the authored drops of dead rats (dunObjectLoot). The core's
// spawnNPCLoot adds those on top of the type tables lootAbstractKills rolls.
func (m *Mod) lootSiteObjects(ctx context.Context, b *botRun, card *siteCard, spawn []*ratThreat) {
	var ids []int64
	for _, r := range spawn {
		if r != nil && r.EHP <= 0 && r.SiteObjectID != 0 {
			ids = append(ids, r.SiteObjectID)
		}
	}
	m.takeAuthoredLoot(ctx, b, ids)
}

// takeAuthoredLoot rolls dunObjectLoot for a set of dead objects into the
// hold with the core's per-ball roll (evedb.RollBallLoot, as rollNPCLoot does):
// dropChance a probability each, the count clamped to the ball's
// lootMinDrops/lootMaxDrops.
func (m *Mod) takeAuthoredLoot(ctx context.Context, b *botRun, objectIDs []int64) (stacks int, iskEV float64) {
	if len(objectIDs) == 0 || b.ShipID == 0 || !lootsPhase(b.State.Phase) {
		return 0, 0
	}
	free, err := m.freeHoldM3(ctx, b)
	if err != nil || free <= 0 {
		return 0, 0
	}
	took := 0.0
	rng := rand.New(rand.NewSource(rand.Int63()))
	for _, oid := range objectIDs {
		rows, err := evedb.GetObjectLoot(ctx, m.db, oid)
		if err != nil || len(rows) == 0 {
			continue
		}
		lo, hi, err := evedb.GetObjectLootDropRange(ctx, m.db, oid)
		if err != nil {
			continue
		}
		names := make(map[int64]string, len(rows))
		for _, r := range rows {
			names[r.ItemTypeID] = r.ItemName
		}
		for _, d := range evedb.RollBallLoot(rows, lo, hi, rng) {
			l := evedb.DunObjectLootRow{ItemTypeID: d.ItemTypeID, ItemName: names[d.ItemTypeID]}
			qty := d.Qty
			if qty <= 0 {
				continue
			}
			vol := m.typeVolume(ctx, l.ItemTypeID)
			if vol > 0 {
				fits := int64((free - took) / vol)
				if fits <= 0 {
					continue
				}
				if qty > fits {
					qty = fits
				}
			}
			if err := m.addCargoStack(ctx, b, l.ItemTypeID, qty); err != nil {
				rlogf("rolebots: authored loot into hold char=%d type=%d: %v", b.CharID, l.ItemTypeID, err)
				continue
			}
			m.noteLootTaken(ctx, b.CharID, l.ItemTypeID, l.ItemName, qty)
			if gid := gangIDOf(b); gid != 0 {
				m.noteGangHaul(ctx, gid, b.CharID, haulLoot, l.ItemTypeID, l.ItemName, qty, 0, 0)
			}
			took += float64(qty) * vol
			stacks++
		}
	}
	if stacks > 0 {
		m.logBot(b.CharID, logInfo, "site", "scooped what was left behind — %d stack(s), %.1f m³", stacks, took)
	}
	return stacks, iskEV
}

// siteShootStructures guns down the room's loot-bearing structures in the
// paper tier: no incoming fire (sentries were in the duel), time is EHP over
// the side's DPS, the prize is the authored loot. Returns the seconds spent.
func (m *Mod) siteShootStructures(ctx context.Context, b *botRun, siteID int64, card *siteCard,
	structs []siteStruct, p *siteProgress) int64 {
	dps, _, known := m.effectivePower(ctx, b)
	if !known || dps <= 0 {
		dps = 100
	}
	secs := 0.0
	var ids []int64
	for _, s := range structs {
		secs += s.EHP / dps
		// First to the structure takes it: a prize another party already
		// cracked is dead on the record and empty for us.
		first := modapi.Sites().MarkKilled(siteID, s.ObjectID)
		p.Killed[s.ObjectID] = true
		if !first {
			continue
		}
		ids = append(ids, s.ObjectID)
		m.logBot(b.CharID, logInfo, "site", "shot down %s in %s", s.Name, card.Name)
	}
	if b.State != nil {
		noteSiteClaimKills(siteID, b.CharID, b.State.Data.GangID, int64(len(ids)))
	}
	m.takeAuthoredLoot(ctx, b, ids)
	return int64(math.Min(math.Max(secs, 10), 300))
}

// siteInterWaveRepair credits the pause between waves to the hull: what the
// side's own repairers would have mended in that many seconds, written back
// over the damage settlePackHulls just persisted.
func (m *Mod) siteInterWaveRepair(ctx context.Context, b *botRun, secs int64) {
	c, err := m.botCombatant(ctx, b)
	if err != nil || c == nil || c.SelfRepPerSec <= 0 {
		return
	}
	mend := c.SelfRepPerSec * float64(secs)
	// Spend the mend on structure first, then armour, mirroring the order the
	// damage was laid down in reverse.
	structDmg := math.Max(0, c.MaxHP[2]-c.CurPool[2])
	armorDmg := math.Max(0, c.MaxHP[1]-c.CurPool[1])
	if structDmg > 0 {
		x := math.Min(mend, structDmg)
		structDmg -= x
		mend -= x
	}
	if mend > 0 && armorDmg > 0 {
		armorDmg -= math.Min(mend, armorDmg)
	}
	if err := evedb.SaveShipCombatDamage(ctx, m.db, b.ShipID, structDmg, armorDmg); err != nil {
		rlogf("rolebots: site repair ship=%d: %v", b.ShipID, err)
	}
}

// recordSiteRun writes one row of rolebot_site_runs: the board's grading of a
// dungeon is the history of what bots did in it.
func (m *Mod) recordSiteRun(ctx context.Context, b *botRun, siteID, vid, gangID int64, outcome string) {
	if m.db == nil || b == nil || b.State == nil {
		return
	}
	tier := "abstract"
	if m.systemProjected(b.SystemID) {
		tier = "embodied"
	}
	started := b.State.Data.SiteStartedAt
	if started == 0 {
		started = clock.Unix()
	}
	// Shared sites (siteetiquette.go): a run that "cleared" a site somebody
	// else did most of was beaten to it; an assist leaves its contest behind.
	if siteID != 0 {
		if outcome == "cleared" && siteBeaten(siteClaimsOf(siteID), b.CharID, gangID) {
			outcome = "beaten"
		}
		name := ""
		if vid != 0 {
			if card, err := m.siteCardFor(ctx, vid); err == nil && card != nil {
				name = card.Name
			}
		}
		m.fileSiteContest(ctx, b, gangID, siteID, b.SystemID, name)
	}
	if _, err := m.db.ExecContext(ctx, `
		INSERT INTO rolebot_site_runs (siteID, dungeonVID, systemID, charID, gangID, tier, startedAt, endedAt, outcome, kills)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		siteID, vid, b.SystemID, b.CharID, gangID, tier, started, clock.Unix(), outcome, b.State.Data.SiteKills); err != nil {
		rlogf("rolebots: record site run: %v", err)
	}
}

// gangSiteStep is the hunt pack's step while its blackboard names a site
// (gangplan.go gangHuntStep): every member carries the site in its state, the
// commander's pass fights the pack's paper fight, and when the site is over the
// hunt moves on as a dry belt sweep does.
func (m *Mod) gangSiteStep(ctx context.Context, b *botRun, g *Gang) error {
	st := b.State
	gs := &g.State
	if st.Data.SiteID != gs.SiteID {
		st.Data.SiteID, st.Data.SiteRoomIdx, st.Data.SiteEntered = gs.SiteID, 0, false
		st.Data.SiteStartedAt, st.Data.SiteKills = clock.Unix(), 0
	}
	out, err := m.siteRunStep(ctx, b, gs.SiteID, g)
	if err != nil {
		return err
	}
	switch out {
	case siteRunning, siteWaiting:
		// Inside the site: from here only the run's own end releases the
		// fleet (gangintent.go), not a help call or the bounty board.
		if st.Data.SiteEntered && b.CharID == g.LeaderCharID {
			if err := m.markObjectiveEngaged(ctx, g.GangID, gs, gs.SiteID); err != nil {
				return err
			}
		}
		if st.Data.LegEndsAt < clock.Unix()+5 {
			st.Data.LegEndsAt = clock.Unix() + 15
		}
		return saveState(ctx, m.db, st)
	case siteDead:
		return nil
	}
	// Over. Only the pack's blackboard is shared, so the first member to see the end
	// moves the hunt on for everyone.
	releaseSite(gs.SiteID, b.CharID, g.GangID)
	st.Data.SiteID, st.Data.SiteRoomIdx, st.Data.SiteEntered = 0, 0, false
	if fresh, err := getGang(ctx, m.db, g.GangID); err == nil && fresh != nil && fresh.State.SiteID == gs.SiteID {
		fresh.State.SiteID, fresh.State.SiteName = 0, ""
		fresh.State.HuntPickedAt = 0
		fresh.State.Objective = nil
		// A site on the blackboard beside a call is that call's site (adoptHelpCall). The
		// run is over, so is the answer; left set, the fleet would sweep the caller's belts.
		fresh.State.HelpCallID = 0
		if err := saveGangState(ctx, m.db, g.GangID, &fresh.State); err != nil {
			return err
		}
		g.State = fresh.State
		m.logBot(b.CharID, logInfo, "gang", "gang %q is done with the site — moving the hunt on", g.Name)
	}
	st.Data.HuntDryAt = clock.Unix()
	st.Activity, st.Phase = "", "idle"
	return saveState(ctx, m.db, st)
}
