package rolebots

import (
	"context"
	"math/rand"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// The gank op in a system nobody is watching.
//
// The abstract tier resolves fights from plain numbers (combat_botvbot.go). A
// gank is the scout's arithmetic (gank.go) with the strike resolved at once: the
// mark is an exposed hauler or barge in the camp system and the strike lands if
// the staged line's damage over the window covers its hitpoints. CONCORD then
// lands on every striker on the same stopwatch abstractConcord reads: each
// striker runs or brawls (gankAftermath in gank_undock.go), and a runner gets
// clear only if the strike and its exit beat the delay. On an undock the mark may dock back
// first (rolebots.gank.preyRedockPct). A striker that dies is moved onto the call point first
// (gankStrikerToCall), so the scout takes the wrecks next (gank_op.go).

// gankAbstractRefused throttles the "let it go" line per (gang, victim).
var gankAbstractRefused sync.Map // key [2]int64{gangID, victimID}, value unix seconds

// gankOnce runs one gank fleet's turn in an unwatched camp system. scoutCharID
// is the scout the predation pass found there.
func (m *Mod) gankOnce(ctx context.Context, scoutCharID, sysID int64, g *Gang) bool {
	if g == nil || !gankOn() {
		return false
	}
	gs := &g.State
	if gs.GankCall != nil || gs.PirSystemID != sysID {
		return false
	}
	scout := m.pvpCombatantFor(ctx, scoutCharID, RoleScout)
	if scout == nil {
		return false
	}
	strikers := m.gankStagedStrikers(ctx, g)
	if len(strikers) == 0 {
		return false
	}
	pirates := append([]*pvpCombatant{scout}, strikers...)
	victimID, _ := m.pickAbstractPrey(ctx, sysID, pirates, false)
	if victimID == 0 {
		return false
	}
	victim := m.pvpCombatantFor(ctx, victimID, RoleCombat)
	if victim == nil {
		return false
	}
	// A gank is for haulers and barges: hulls that carry a worthwhile hold and
	// cannot shoot back. A combat hull on the gate is the pirate pack's business.
	if byID, _, err := m.loadHullProfiles(ctx); err == nil {
		h := byID[victim.ShipTypeID]
		if h == nil || (h.Class != ClassIndustrial && h.Class != ClassBarge && h.Class != ClassFreighter) {
			return false
		}
	}
	items, err := evedb.GetShipCargoByShip(ctx, m.db, victim.ShipID)
	if err != nil {
		return false
	}
	cargo := make([]gankCargoItem, 0, len(items))
	for _, it := range items {
		cargo = append(cargo, gankCargoItem{TypeID: it.TypeID, Qty: it.Quantity})
	}
	prices, _ := evedb.TypeValues(ctx, m.db, gankTypeIDs(cargo))
	value := gankCargoValue(cargo, prices)

	sec := 0.0
	if u, err := m.universe(ctx); err == nil {
		sec = u.meta[sysID].Security
	}
	// Applied on the mark (application.go): a destroyer's guns against a hauler's signature
	// and speed at the range the embodied strike would fly.
	dps := make([]float64, 0, len(strikers))
	for _, s := range strikers {
		dps = append(dps, s.dpsOn(victim))
	}
	ships, ok := gankCallFor(value, dialGankMinCargoISK.Get(), victim.EHP, dps, sec,
		dialGankLockSec.Get(), dialGankMarginPct.Get(), gankWindowCap(gs))
	if !ok {
		key := [2]int64{g.GangID, victimID}
		if last, seen := gankAbstractRefused.Load(key); !seen || clock.Unix()-last.(int64) > 600 {
			gankAbstractRefused.Store(key, clock.Unix())
			m.logBot(scoutCharID, logInfo, "gank",
				"read %s's hold at %s, %.0f EHP — needs %d destroyer(s), %d staged; let it go",
				victim.Name, fmtISK(value), victim.EHP, ships, len(strikers))
		}
		return false
	}

	// On an undock the mark may see the line coming and dock back.
	if gs.GankStationID != 0 && gankUndockRedocks(rand.Float64(), dialGankPreyRedockPct.Get()) {
		m.logBot(scoutCharID, logInfo, "gank", "%s docked back into %s before the line landed",
			victim.Name, gs.GankStationName)
		return false
	}

	// Each striker is credited an equal share of the mark's hitpoints, so the bounty
	// split and killmail read one line; the mark took all of it.
	share := victim.MaxEHP / float64(len(strikers))
	for _, s := range strikers {
		s.Dealt = share
	}
	victim.Taken = victim.MaxEHP
	vb, _ := m.loadRun(ctx, victimID)
	victimName := victim.Name
	if vb != nil {
		m.chatterEngageAbstract(ctx, m.mustRun(ctx, g.LeaderCharID), vb, sysID, motivePiracy)
		victimName = vb.Name
	}
	noteHostileEncounter(intelScope{corpID: victim.CorpID}, sysID)
	// Log before the kill is filed: the death hook reads the log to file it as a
	// gank (gankrel.go).
	m.noteGankCalled(ctx, g.GangID, victimID, sysID, value)
	m.chargeAbstractAggression(ctx, strikers, []*pvpCombatant{victim}, sysID)
	m.killBotShip(ctx, victim, strikers, true)
	m.piracyRecordVictim(ctx, g.LeaderCharID, victimID, true)
	_ = bumpPiracyTally(ctx, m.db, g.LeaderCharID, true)
	m.fileVictimBounty(ctx, victimID, strikers)
	// The call point is the scout's ball, on the gate's arrival side or the undock: the strikers
	// jumped or warped onto it, so their wrecks lie inside the scout's loot sweep (gankLootStep).
	x, y, z := 0.0, 0.0, 0.0
	_ = m.db.QueryRowContext(ctx,
		`SELECT COALESCE(x,0), COALESCE(y,0), COALESCE(z,0) FROM entity WHERE itemID = ?`,
		scout.ShipID).Scan(&x, &y, &z)
	fightSec := gankStrikeSec(victim.EHP, dps, dialGankLockSec.Get())
	died, slipped, cops := m.gankAftermathAbstract(ctx, strikers, sysID, sec, fightSec,
		destiny.Vec3{X: x, Y: y, Z: z})
	gs.GankCall = &GankCall{
		VictimCharID: victimID, VictimName: victimName, ShipID: victim.ShipID, ShipTypeID: victim.ShipTypeID,
		EHP: victim.EHP, ISK: value, Ships: ships, CalledAt: clock.Unix(), X: x, Y: y, Z: z,
	}
	_ = saveGangState(ctx, m.db, g.GangID, gs)
	m.logBot(g.LeaderCharID, logInfo, "gank",
		"gank in %s (%.1f): %s's %s with %s in the hold broken by %d destroyer(s) in a %.0fs window; CONCORD took %d, %d slipped away, %d police frigate(s) lost",
		m.nameOfSystem(ctx, sysID), sec, victimName, m.typeNameOr(ctx, victim.ShipTypeID), fmtISK(value),
		ships, mustWindow(sec, dialGankLockSec.Get()), died, slipped, cops)
	return true
}

// mustWindow is gankWindow's window for a log line.
func mustWindow(sec, lock float64) float64 {
	w, _ := gankWindow(sec, lock)
	return w
}

// mustRun is loadRun for a caller that only wants a name on a line; an
// unreadable pilot reads as an empty run so the strike does not fail.
func (m *Mod) mustRun(ctx context.Context, charID int64) *botRun {
	if b, err := m.loadRun(ctx, charID); err == nil && b != nil {
		return b
	}
	return &botRun{CharID: charID}
}
