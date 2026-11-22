package rolebots

import (
	"context"
	"math"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

// Combat, first tier: a rolebot that can actually be hurt.
// A projection built without HP is scenery: destiny.Entity.ApplyDamage returns early on
// MaxStructure <= 0, so shots land for nothing. Filling the same pools registerUndock fills
// for a player's ship makes the projection a target.
// The numbers come from the same evedb helpers the server uses for players, so a bot gets
// the same tank (fitted extenders, tank skills, hull resistance traits).
// Damage carries across embodiment: armour and structure persist to the repair table a
// player's ship uses, and shields recharge on their own.

const (
	// A freshly materialised projection is off-limits to NPC locks for this long, the grace
	// registerUndock gives a player still loading the grid.
	projectionNoLockSec = 12
)

// defaultPassiveBonuses is the neutral multiplier set used when a ship's passive module
// bonuses cannot be read: every field 1.0, so an unreadable row costs the bot nothing.
func defaultPassiveBonuses() *evedb.ShipPassiveBonuses {
	return &evedb.ShipPassiveBonuses{
		CPUMult: 1, PGMult: 1, CapCapMult: 1, CapRechargeMult: 1,
		ShieldCapMult: 1, ShieldRechargeMult: 1, StructureMult: 1, CargoMult: 1,
		VelocityMult: 1, AgilityMult: 1, ScanResMult: 1,
	}
}

// initProjectionTank fills e.HP and e.Resists for a rolebot's ship, mirroring
// registerUndock's block for a player entering space. A failed lookup degrades to a neutral
// multiplier and is logged, so a bot with a partly resolved tank is still a target.
func (m *Mod) initProjectionTank(ctx context.Context, e *destiny.Entity, charID, shipID, shipTypeID int64) {
	if e == nil || shipTypeID == 0 {
		return
	}

	base, err := evedb.GetEntityHPByType(ctx, m.db, shipTypeID)
	if err != nil {
		rlogf("rolebots: tank: hull HP type=%d: %v", shipTypeID, err)
		return
	}

	hullB, err := evedb.GetShipHullBonuses(ctx, m.db, charID, shipTypeID)
	if err != nil {
		rlogf("rolebots: tank: hull bonuses ship=%d: %v", shipID, err)
		hullB = nil
	}
	hullStats := hullB.StatMults()

	shieldAdd, armorAdd, err := evedb.GetShipFittingHPBonuses(ctx, m.db, shipID)
	if err != nil {
		rlogf("rolebots: tank: fitting HP ship=%d: %v", shipID, err)
		shieldAdd, armorAdd = 0, 0
	}

	shieldSkill, armorSkill, hullSkill, rechargeSkill, err := evedb.GetTankSkillMultipliers(ctx, m.db, charID)
	if err != nil {
		rlogf("rolebots: tank: skills char=%d: %v", charID, err)
		shieldSkill, armorSkill, hullSkill, rechargeSkill = 1, 1, 1, 1
	}

	pb, err := evedb.GetShipPassiveBonuses(ctx, m.db, shipID)
	if err != nil {
		rlogf("rolebots: tank: passive bonuses ship=%d: %v", shipID, err)
		pb = defaultPassiveBonuses()
	}

	maxShield := (base.ShieldHP + shieldAdd) * shieldSkill * pb.ShieldCapMult * hullStats.ShieldHP
	maxArmor := (base.ArmorHP + armorAdd) * armorSkill * hullStats.ArmorHP
	maxStructure := base.StructureHP * hullSkill * pb.StructureMult

	e.HP = destiny.HPState{
		MaxShield: maxShield, CurShield: maxShield,
		MaxArmor: maxArmor, CurArmor: maxArmor,
		MaxStructure: maxStructure, CurStructure: maxStructure,
		ShieldTau: base.ShieldTau * rechargeSkill * pb.ShieldRechargeMult,
	}

	// Carry forward the damage the last engagement left on the hull. Shields come back full:
	// they recharge passively between embodiments.
	if structDmg, armorDmg, err := evedb.GetShipCombatDamage(ctx, m.db, shipID); err != nil {
		rlogf("rolebots: tank: stored damage ship=%d: %v", shipID, err)
	} else if structDmg > 0 || armorDmg > 0 {
		e.HP.CurArmor = maxArmor - armorDmg
		if e.HP.CurArmor < 0 {
			e.HP.CurArmor = 0
		}
		e.HP.CurStructure = maxStructure - structDmg
		// Never materialise a dead ship: a hull left at wreck level comes back with a sliver, so
		// the next hit kills it on grid.
		if e.HP.CurStructure < 1 {
			e.HP.CurStructure = 1
		}
	}

	e.Resists = [3][4]float64{base.ShieldRes, base.ArmorRes, base.HullRes}
	if mult, err := evedb.GetShipResistanceMultipliers(ctx, m.db, shipID, m.fittedActiveSet(ctx, shipID)); err == nil {
		for l := 0; l < 3; l++ {
			for d := 0; d < 4; d++ {
				e.Resists[l][d] *= mult[l][d]
			}
		}
	} else {
		rlogf("rolebots: tank: resist modules ship=%d: %v", shipID, err)
	}
	// Hull resistance traits (Merlin/Moa shields, Punisher/Maller armour) scale the shield and
	// armour resonances only; structure has no trait.
	if len(hullB) > 0 {
		hullRes := hullB.ResistMults()
		for l := 0; l < 2; l++ {
			for d := 0; d < 4; d++ {
				e.Resists[l][d] *= hullRes[l][d]
			}
		}
	}

	// The grid is still assembling on every client that can see this ship.
	e.SetNoLockUntil(time.Now().Add(projectionNoLockSec * time.Second))
}

// damageToPersist is the armour and structure damage a teardown writes back, and whether
// anything should be written at all. A destroyed ship writes nothing: its hull row is gone
// or replaced, and a wreck's damage carried onto the next embodiment is a pilot born at 0%.
// An HP block that was never filled (MaxStructure 0) writes nothing either.
func damageToPersist(hp destiny.HPState, destroyed bool) (structDmg, armorDmg float64, ok bool) {
	if destroyed || hp.MaxStructure <= 0 {
		return 0, 0, false
	}
	return math.Max(0, hp.MaxStructure-hp.CurStructure), math.Max(0, hp.MaxArmor-hp.CurArmor), true
}

// persistProjectionDamage writes a projection's armour and structure damage
// back to the ship row, so a bot that de-embodies mid-fight stays hurt. Called
// on every teardown path that is not a death.
func (m *Mod) persistProjectionDamage(eb *embodiedBot) {
	if eb == nil || eb.ent == nil {
		return
	}
	structDmg, armorDmg, ok := damageToPersist(eb.ent.HPSnapshot(), eb.ent.IsDestroyed())
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := evedb.SaveShipCombatDamage(ctx, m.db, eb.shipID, structDmg, armorDmg); err != nil {
		rlogf("rolebots: persist damage ship=%d: %v", eb.shipID, err)
	}
}

// persistProjectionPosition writes the ball's live coordinate back to the ship's entity row
// as the projection is torn down, so the next embodiment starts where the ship was and not
// at its last warp landing. A ball mid-warp is skipped: Manager.Step has no navWarp branch,
// so dm.Pos stays at the departure point for the whole tunnel (armWarpMirror).
func (m *Mod) persistProjectionPosition(eb *embodiedBot) {
	if eb == nil || eb.ent == nil || eb.ent.Manager == nil || eb.shipID == 0 {
		return
	}
	eb.mu.Lock()
	warping := eb.warping
	eb.mu.Unlock()
	dm := eb.ent.Manager
	if warping || dm.CurrentBallMode() == destiny.ModeWarp {
		return
	}
	pos := eb.ent.Pos()
	// Zero is the module's not-set sentinel (see embarkPosition): writing one would read back
	// as no persisted position.
	if pos.IsZero() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.writeProjectionPosition(ctx, eb.shipID, eb.systemID, pos)
}

// writeProjectionPosition persists a projection's coordinate only while the ship row is still in
// the projection's own system. A ship that has docked or jumped carries a row the abstract tier has
// already rewritten (the hangar, or the arrival gate), and the old ball's coordinate must not land
// on top of it. See docs/comment-notes-b07.md#departure-teardown.
func (m *Mod) writeProjectionPosition(ctx context.Context, shipID, systemID int64, pos destiny.Vec3) {
	if shipID == 0 || systemID == 0 {
		return
	}
	if _, err := m.db.ExecContext(ctx,
		`UPDATE entity SET x = ?, y = ?, z = ? WHERE itemID = ? AND locationID = ?`,
		pos.X, pos.Y, pos.Z, shipID, systemID); err != nil {
		rlogf("rolebots: persist position ship=%d: %v", shipID, err)
	}
}

// tankFraction is the projection's health as one number in [0,1]: the share of
// its total pool still standing. The flee logic reads it rather than any single
// layer, because "shield is gone" means very different things on a shield
// Caracal and an armour Punisher.
func (eb *embodiedBot) tankFraction() float64 {
	if eb.ent == nil {
		return 1
	}
	hp := eb.ent.HPSnapshot()
	total := hp.MaxShield + hp.MaxArmor + hp.MaxStructure
	if total <= 0 {
		return 1
	}
	return (hp.CurShield + hp.CurArmor + hp.CurStructure) / total
}

// armorFraction is armour alone, the layer whose loss says the fight has turned before the
// hull is lost. The drone recall is staged on it (drones_embodied.go).
func (eb *embodiedBot) armorFraction() float64 {
	if eb.ent == nil {
		return 1
	}
	hp := eb.ent.HPSnapshot()
	if hp.MaxArmor <= 0 {
		return 1
	}
	return hp.CurArmor / hp.MaxArmor
}

// hullFraction is structure alone, the layer that decides whether the next volley is fatal.
func (eb *embodiedBot) hullFraction() float64 {
	if eb.ent == nil {
		return 1
	}
	hp := eb.ent.HPSnapshot()
	if hp.MaxStructure <= 0 {
		return 1
	}
	return hp.CurStructure / hp.MaxStructure
}

// fittedActiveSet is every module on a ship, as the running set the core's resist fold
// asks for. A bot runs its hardeners whenever it fights and the fit scorer credits them at
// full active strength (ComputeFitStats), so nil would read them at their passive bonus.
// Nil on a read failure.
func (m *Mod) fittedActiveSet(ctx context.Context, shipID int64) map[int64]bool {
	fitted, err := evedb.GetFittedModules(ctx, m.db, shipID)
	if err != nil || len(fitted) == 0 {
		return nil
	}
	out := make(map[int64]bool, len(fitted))
	for _, fm := range fitted {
		out[fm.ItemID] = true
	}
	return out
}
