package rolebots

import (
	"context"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/modapi"
)

// Site raids: outside policed space a pirate goes after the pilots running a
// deadspace site. Four hooks: the hunting-ground pick (piracySite) favours
// systems with claimed sites, predateOnce weights site runners, raidEmbodyTick
// warps a prowling projection to a runner, and an abstract kill points the
// pirate at its wreck. Highsec is untouched (siteetiquette.go).

var (
	dialRaidOn = &pvpDial{key: "rolebots.raid.on", def: 1, lo: 0, hi: 1, step: 1,
		label: "Pirates go after site runners outside highsec (1 = on)"}
	dialRaidPreyWeight = &pvpDial{key: "rolebots.raid.preyWeight", def: 4, lo: 1, hi: 20, step: 0.5,
		label: "How much likelier a site runner is to be picked as prey"}
	dialRaidAmbushMul = &pvpDial{key: "rolebots.raid.ambushMul", def: 1.5, lo: 1, hi: 5, step: 0.1,
		label: "Ambush chance multiplier against a site runner (pinned in a pocket)"}
	dialRaidGround = &pvpDial{key: "rolebots.raid.groundWorth", def: 2, lo: 0, hi: 10, step: 0.5,
		label: "Hunting-ground score per claimed site in a system (capped at 3 sites)"}
)

func raidDials() []*pvpDial {
	return []*pvpDial{dialRaidOn, dialRaidPreyWeight, dialRaidAmbushMul, dialRaidGround}
}

func raidOn() bool { return dialRaidOn.Get() >= 0.5 }

// raidSiteCap bounds how many claimed sites count toward a system's score.
const raidSiteCap = 3

// raidPreyWeight is a candidate's weight multiplier in the abstract prey
// pick: the dial for a site runner where raids apply, 1 otherwise. Pure.
func raidPreyWeight(siteBias bool, phase string) float64 {
	if !siteBias || phase != string(TaskCombatSite) {
		return 1
	}
	return dialRaidPreyWeight.Get()
}

// claimedSitesBySystem counts, per system, the live sites a bot party holds.
// It is the raid term of the hunting-ground score; unclaimed sites hold rats only.
func claimedSitesBySystem() map[int64]int {
	out := map[int64]int{}
	if !modapi.CanRunSites() {
		return out
	}
	for _, s := range modapi.Sites().List() {
		if s.ClearedAt == 0 && len(siteClaimsOf(s.SiteID)) > 0 {
			out[s.SolarSystemID]++
		}
	}
	return out
}

// raidGroundScore is the hunting-ground term for one system. Pure.
func raidGroundScore(claimed int, policed bool) float64 {
	if policed || claimed <= 0 {
		return 0
	}
	if claimed > raidSiteCap {
		claimed = raidSiteCap
	}
	return float64(claimed) * dialRaidGround.Get()
}

// ── the wreck an abstract kill leaves ───────────────────────────────────────

// lastWreck remembers, per dead pilot, the container its abstract death
// dropped (loseHull, dropWreckLoot) until the killer's pass reads it.
var (
	lastWreckMu sync.Mutex
	lastWreck   = map[int64]struct {
		canID, sysID int64
		at           time.Time
	}{}
)

func noteLastWreck(charID, canID, sysID int64) {
	if canID == 0 {
		return
	}
	lastWreckMu.Lock()
	defer lastWreckMu.Unlock()
	now := clock.Now()
	for id, w := range lastWreck {
		if now.Sub(w.at) > 10*time.Minute {
			delete(lastWreck, id)
		}
	}
	lastWreck[charID] = struct {
		canID, sysID int64
		at           time.Time
	}{canID, sysID, now}
}

func takeLastWreck(charID int64) (canID, sysID int64) {
	lastWreckMu.Lock()
	defer lastWreckMu.Unlock()
	w, ok := lastWreck[charID]
	if !ok {
		return 0, 0
	}
	delete(lastWreck, charID)
	return w.canID, w.sysID
}

// pointPirateAtWreck sends a pirate to empty the container its victim left.
// It is the abstract twin of notePiracyLoot, with the same step lock.
func (m *Mod) pointPirateAtWreck(charID, canID, sysID int64) {
	if canID == 0 {
		return
	}
	m.goSafeBot(charID, "raid wreck note", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		unlock := lockBotStep(charID)
		defer unlock()
		st, err := loadState(ctx, m.db, charID)
		if err != nil || st.Data.PiracyLootID != 0 {
			return
		}
		st.Data.PiracyLootID, st.Data.PiracyLootSysID = canID, sysID
		if err := saveState(ctx, m.db, st); err != nil {
			rlogf("rolebots: raid wreck note char=%d: %v", charID, err)
			return
		}
		m.logBot(charID, logInfo, "piracy", "the wreck left a container worth opening")
	})
}

// ── the grid ────────────────────────────────────────────────────────────────

// raidTickGap spaces a prowler's look for site runners.
const raidTickGap = 20 * time.Second

// raidEmbodyTick sends a prowling pirate projection to a site runner in its
// system: warp to the runner's ball (the pack follows), and the prey picker
// does the rest once they share a grid. True when it took the tick.
func (m *Mod) raidEmbodyTick(eb *embodiedBot, now time.Time) bool {
	if !raidOn() || eb == nil || eb.ent == nil || eb.ss == nil {
		return false
	}
	eb.mu.Lock()
	ok := eb.prey && eb.posture == PostureAmbushBelt && eb.siteID == 0 &&
		!(eb.dead || eb.warping || eb.fighting || eb.fleeing || eb.approachID != 0) &&
		!now.Before(eb.raidNextAt)
	if ok {
		eb.raidNextAt = now.Add(raidTickGap)
	}
	sysID := eb.systemID
	eb.mu.Unlock()
	if !ok || secBand(dispSec(eb.ss.Security)) == BandPoliced {
		return false
	}
	// Somebody on our grid already: the prey picker handles it.
	if len(m.preyOnGrid(eb)) > 0 {
		return false
	}
	embodyMu.Lock()
	var others []*embodiedBot
	for _, o := range embodyLive {
		if o != eb && o.systemID == sysID && o.ent != nil {
			others = append(others, o)
		}
	}
	embodyMu.Unlock()
	var runners []*embodiedBot
	for _, o := range others {
		o.mu.Lock()
		onSite := o.siteID != 0
		o.mu.Unlock()
		if onSite {
			runners = append(runners, o)
		}
	}
	for _, o := range runners {
		if o.ent.IsDestroyed() || o.ent.Slim.GroupID == capsuleGroupID || !m.wouldRob(eb, o.ent) {
			continue
		}
		pos := o.ent.Pos()
		if pos.Sub(eb.ent.Pos()).Len() < escortWarpFloor {
			return false // on top of it already; the grid picks it up
		}
		eb.mu.Lock()
		already := eb.currentDest == int64(o.ent.ID)
		eb.mu.Unlock()
		if already {
			return true
		}
		if m.startEmbodyWarpAs(eb, evedb.CelestialEntity{
			ItemID: int64(o.ent.ID),
			X:      pos.X, Y: pos.Y, Z: pos.Z,
			Radius: escortLandingBuffer,
			Name:   o.charName,
		}, true, warpDiscretionary) {
			m.logBot(eb.charID, logInfo, "piracy", "a site runner is working this system — going for %s", o.charName)
		}
		return true
	}
	return false
}
