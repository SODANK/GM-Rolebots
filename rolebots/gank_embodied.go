package rolebots

import (
	"context"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// The gank op on a live grid: the scout's scan and call, the striker's target.
//
// The scout is unarmed, so it lives in the combat loop's quiet branch
// (combat_embodied.go calls gankScoutTick). Every few seconds it scans pilots in
// cargo scanner reach (the effect goes on the wire), prices the hold at the
// killboard valuation and runs the arithmetic against the strikers staged one
// jump out. A hold the line can break inside the CONCORD window becomes a call on
// the blackboard. A striker's only override is to shoot the called mark, with no
// strike-window refusal and no break-off.

// gankGridState is the per-projection memory of the scout tick (scan times,
// scanner, fleet row) and of a striker's aftermath (gank_undock.go: verdict,
// escape safe, whether it said it got clear). The aftermath resets with the strike.
type gankGridState struct {
	scanAt  time.Time
	scanned map[uint32]time.Time
	scanner *gankScanner
	gangAt  time.Time
	gang    *Gang

	verdict     gankVerdict
	escape      *evedb.CelestialEntity
	saidEscaped bool
}

// gankScanner is the cargo scanner a scout carries. found=false means the hull
// carries none and the scout only watches.
type gankScanner struct {
	found   bool
	itemID  int64
	typeID  int64
	rangeM  float64
	guid    string
	cycleMs int64
}

const (
	gankScoutTickSec = 5
	gankGangTTLSec   = 20
)

// gankGang is the fleet row, re-read every gankGangTTLSec: the scout writes the
// call and every striker reads it through here.
func (m *Mod) gankGang(ctx context.Context, eb *embodiedBot) *Gang {
	if eb.gangID == 0 {
		return nil
	}
	now := clock.Now()
	if eb.gank.gang != nil && now.Sub(eb.gank.gangAt) < gankGangTTLSec*time.Second {
		return eb.gank.gang
	}
	g, err := getGang(ctx, m.db, eb.gangID)
	if err != nil || g == nil {
		return eb.gank.gang
	}
	eb.gank.gang, eb.gank.gangAt = g, now
	return g
}

// gankScannerFor reads the scout's fitted cargo scanner once.
func (m *Mod) gankScannerFor(ctx context.Context, eb *embodiedBot) *gankScanner {
	if eb.gank.scanner != nil {
		return eb.gank.scanner
	}
	sc := &gankScanner{}
	eb.gank.scanner = sc
	fitted, err := evedb.GetFittedModules(ctx, m.db, eb.shipID)
	if err != nil {
		eb.gank.scanner = nil // try again next tick
		return sc
	}
	for _, fm := range fitted {
		if fm.GroupID != grpCargoScanner {
			continue
		}
		attrs, err := evedb.GetTypeAttributes(ctx, m.db, fm.TypeID)
		if err != nil {
			continue
		}
		sc.found, sc.itemID, sc.typeID = true, fm.ItemID, fm.TypeID
		sc.rangeM = attrs[attrCargoScanRange]
		if sc.rangeM <= 0 {
			sc.rangeM = 30_000
		}
		sc.cycleMs = int64(attrs[attrDuration])
		if effects, err := m.moduleEffects(ctx, fm.TypeID); err == nil {
			for _, ef := range effects {
				if ef.Name == "cargoScan" {
					sc.guid = fxGUIDFor(ef.Name, ef.GUID)
				}
			}
		}
		break
	}
	return sc
}

// gankScoutTick is the scout's quiet-tick work: scan, price, call.
func (m *Mod) gankScoutTick(ctx context.Context, eb *embodiedBot) {
	if eb.ent == nil || eb.ss == nil || !gankOn() {
		return
	}
	now := clock.Now()
	if now.Sub(eb.gank.scanAt) < gankScoutTickSec*time.Second {
		return
	}
	eb.gank.scanAt = now
	dbCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	g := m.gankGang(dbCtx, eb)
	if g == nil || g.State.Focus != gangFocusGank || g.State.GankCall != nil {
		return
	}
	sc := m.gankScannerFor(dbCtx, eb)
	if sc == nil || !sc.found {
		return
	}
	if eb.gank.scanned == nil {
		eb.gank.scanned = map[uint32]time.Time{}
	}
	me := eb.ent.Pos()
	var staged []*pvpCombatant
	stagedRead := false
	for _, e := range eb.ss.GetBubbleEntitiesExcept(eb.ent.ID) {
		if !e.IsPlayer || e.Slim.CharID == 0 || !m.wouldRob(eb, e) {
			continue
		}
		if e.Pos().Sub(me).Len() > sc.rangeM {
			continue
		}
		if at, ok := eb.gank.scanned[e.ID]; ok && now.Sub(at) < gankScanRecheckSec*time.Second {
			continue
		}
		eb.gank.scanned[e.ID] = now
		// The scan, on the wire and in the ledger.
		m.moduleFX(eb, sc.itemID, sc.typeID, sc.guid, e.ID, true, sc.cycleMs, false)
		items, err := evedb.GetShipCargoByShip(dbCtx, m.db, e.Slim.ItemID)
		if err != nil {
			continue
		}
		cargo := make([]gankCargoItem, 0, len(items))
		for _, it := range items {
			cargo = append(cargo, gankCargoItem{TypeID: it.TypeID, Qty: it.Quantity})
		}
		prices, _ := evedb.TypeValues(dbCtx, m.db, gankTypeIDs(cargo))
		value := gankCargoValue(cargo, prices)
		if value < dialGankMinCargoISK.Get() {
			continue
		}
		if !stagedRead {
			staged, stagedRead = m.gankStagedStrikers(dbCtx, g), true
		}
		dps := make([]float64, 0, len(staged))
		for _, s := range staged {
			dps = append(dps, s.DPS)
		}
		hp := e.HPSnapshot()
		ehp := hp.CurShield + hp.CurArmor + hp.CurStructure
		ships, ok := gankCallFor(value, dialGankMinCargoISK.Get(), ehp, dps, eb.ss.Security,
			dialGankLockSec.Get(), dialGankMarginPct.Get(), gankWindowCap(&g.State))
		if !ok {
			if eb.noteStrikeRefusal(e.Slim.CharID) {
				m.logBot(eb.charID, logInfo, "gank",
					"scanned %s: %s in the hold, %.0f EHP — needs %d destroyer(s), %d staged; let it go",
					e.CharName, fmtISK(value), ehp, ships, len(staged))
			}
			continue
		}
		p := e.Pos()
		call := GankCall{
			VictimCharID: e.Slim.CharID, VictimName: e.CharName,
			ShipID: e.Slim.ItemID, ShipTypeID: e.Slim.TypeID,
			EHP: ehp, ISK: value, Ships: ships, CalledAt: clock.Unix(),
			X: p.X, Y: p.Y, Z: p.Z,
		}
		if err := m.callGank(dbCtx, g, call); err != nil {
			rlogf("rolebots: gank call: %v", err)
			return
		}
		m.logBot(eb.charID, logInfo, "gank", "called the strike on %s — %s in the hold, %.0f EHP, %d of %d destroyers",
			e.CharName, fmtISK(value), ehp, ships, len(staged))
		m.fleetSay(eb, pickLine(gankCallLines), e.CharName, fmtISK(value), ships)
		return
	}
}

// gankStrikeTarget is the called mark's ball on this striker's grid, or 0.
func (m *Mod) gankStrikeTarget(eb *embodiedBot) uint32 {
	if eb.ent == nil || eb.ss == nil {
		return 0
	}
	dbCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	g := m.gankGang(dbCtx, eb)
	if g == nil || g.State.GankCall == nil {
		return 0
	}
	call := g.State.GankCall
	for _, e := range eb.ss.GetAllPlayerEntities() {
		if e.Slim.CharID == call.VictimCharID && !e.IsDestroyed() {
			return e.ID
		}
	}
	return 0
}

// gankVictimPos is the called mark's position as the scout saw it.
func gankVictimPos(c *GankCall) destiny.Vec3 {
	if c == nil {
		return destiny.Vec3{}
	}
	return destiny.Vec3{X: c.X, Y: c.Y, Z: c.Z}
}
