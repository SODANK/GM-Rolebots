package rolebots

import (
	"context"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/modapi"
	"gurimoon/server/gangsta"
)

// Support modules on a projection: propulsion, local repair and remote repair. loadCombatLoadout
// keeps weapons and electronic warfare; this file is the rest of the rack.
//   - Propulsion: lit while the ship closes on, or holds range off, its target, through the server's
//     SetSpeedModifier call and classic boost formula (propBoost, fitstats.go), so a kiter flies at the
//     speed the fit lens credited. A microwarpdrive blooms the signature.
//   - Local repair: shield boosters and armour repairers mend the ship's own layer, paid from capacitor.
//   - Remote repair: shield transporters, armour projectors (and remote hull repairers) mend the
//     most-wounded fleetmate in range: a projection in the same gang or player fleet, or the commanding
//     player, whose HUD bars climb (HPChanged seam). Energy transfer arrays move capacitor likewise.
// Cap is the loadout's own pool (combatLoadout.drawCap): a logi that runs dry stops mending.

const (
	// propChaseMarginM is how far outside the range it wants a ship has to be
	// before it lights its engine to close. Inside it, the orbit or the
	// keep-at-range does the work and the capacitor stays put.
	propChaseMarginM = 3000.0
	// remoteRepWoundedFrac is the layer fraction a fleetmate must be under to
	// be worth a cycle. Not 1.0: topping off scratches is a cycle of
	// capacitor somebody else will need in a moment.
	remoteRepWoundedFrac = 0.95
	// localRepWoundedFrac is the same threshold for the ship's own repairer.
	localRepWoundedFrac = 0.90
	// speedModPropKey offsets a prop module's item ID into its own band of speed-modifier keys, so it
	// cannot collide with a web's key (the web's own item ID); both are entity item IDs.
	speedModPropKey = int64(1) << 40
)

// repKind is what a repairer mends.
type repKind int

const (
	repShield repKind = iota + 1
	repArmor
	repHull
	repCap
)

// botPropModule is the fitted propulsion module.
type botPropModule struct {
	ItemID, TypeID int64
	Name, FXGUID   string
	Spec           propSpec
	CycleSec       float64
	CapNeed        float64

	on          bool
	nextCycleAt time.Time
}

// botRepModule is one fitted repairer, local or remote.
type botRepModule struct {
	ItemID, TypeID int64
	Name, FXGUID   string
	Kind           repKind
	Remote         bool
	Amount         float64
	Range          float64
	CycleSec       float64
	CapNeed        float64

	appliedTo   uint32
	nextCycleAt time.Time
}

// supportFromEffect classifies one effect of one fitted module as propulsion
// or repair and adds it to the loadout. Reports whether it took the module, so
// the caller stops looking at the module's other effects.
func supportFromEffect(l *combatLoadout, fm evedb.FittedModule, attrs map[int64]float64,
	eff effectRow, name string) bool {

	cycle := durationSecOr(attrs[dogma.AttrDuration], 10)
	capNeed := attrs[dogma.AttrCapacitorNeed]
	switch eff.Name {
	case "speedBoost", "speedBoostMassAddition", "speedBoostMassSigRad":
		p := &botPropModule{
			ItemID: fm.ItemID, TypeID: fm.TypeID, Name: name,
			FXGUID: fxGUIDFor(eff.Name, eff.GUID), Spec: propSpecOf(attrs),
			CycleSec: cycle, CapNeed: capNeed,
		}
		// One engine runs at a time, so keep the one that boosts hardest.
		if l.prop == nil || propBoost(p.Spec, l.hullMass) > propBoost(l.prop.Spec, l.hullMass) {
			l.prop = p
		}
		return true
	case dogma.EffNameShieldBoosting, "armorRepair", "structureRepair":
		r := &botRepModule{ItemID: fm.ItemID, TypeID: fm.TypeID, Name: name,
			FXGUID: fxGUIDFor(eff.Name, eff.GUID), CycleSec: cycle, CapNeed: capNeed}
		switch eff.Name {
		case dogma.EffNameShieldBoosting:
			r.Kind, r.Amount = repShield, attrs[dogma.AttrShieldBonus]
		case "armorRepair":
			r.Kind, r.Amount = repArmor, attrs[dogma.AttrArmorDamageAmount]
		default:
			r.Kind, r.Amount = repHull, attrs[dogma.AttrStructureDamageAmount]
		}
		if r.Amount > 0 {
			l.reps = append(l.reps, r)
		}
		return true
	case dogma.EffNameShieldTransfer, "targetArmorRepair", dogma.EffNameEnergyTransfer:
		r := &botRepModule{ItemID: fm.ItemID, TypeID: fm.TypeID, Name: name, Remote: true,
			FXGUID: fxGUIDFor(eff.Name, eff.GUID), CycleSec: cycle, CapNeed: capNeed}
		switch {
		case eff.Name == dogma.EffNameShieldTransfer:
			r.Kind, r.Amount, r.Range = repShield, attrs[dogma.AttrShieldBonus], attrs[attrShieldTransferRange]
		case eff.Name == dogma.EffNameEnergyTransfer:
			r.Kind, r.Amount, r.Range = repCap, attrs[dogma.AttrPowerTransferAmount], attrs[dogma.AttrPowerTransferRange]
		case fm.GroupID == grpRemoteHullRepairer:
			// The remote hull repairer shares the armour projector's effect
			// in this database and carries its amount in the hull attribute.
			r.Kind, r.Amount, r.Range = repHull, attrs[dogma.AttrStructureDamageAmount], attrs[dogma.AttrMaxRange]
		default:
			r.Kind, r.Amount, r.Range = repArmor, attrs[dogma.AttrArmorDamageAmount], attrs[dogma.AttrMaxRange]
		}
		if r.Range <= 0 {
			r.Range = 5000
		}
		if r.Amount > 0 {
			l.reps = append(l.reps, r)
		}
		return true
	}
	return false
}

// foldLocalRepBonuses scales one local repairer by what the server gives a player's identical
// module: skill and amplifier terms (db.GetRepairModuleBonuses) and the hull's repair trait
// (HullBonuses.RepairMults, shield boosters and armour repairers only). Remote reps are not scaled,
// as on the player path. A non-positive result is refused so a bad read cannot zero a tank.
func foldLocalRepBonuses(r *botRepModule, amountMult, cycleMult, capMult float64, hull evedb.HullBonuses) {
	if r == nil || r.Remote {
		return
	}
	boostAmt, boostCap, repAmt, repCap, repDur := hull.RepairMults()
	switch r.Kind {
	case repShield:
		amountMult *= boostAmt
		capMult *= boostCap
	case repArmor:
		amountMult *= repAmt
		capMult *= repCap
		cycleMult *= repDur
	}
	if amountMult > 0 {
		r.Amount *= amountMult
	}
	if cycleMult > 0 {
		r.CycleSec *= cycleMult
	}
	if capMult > 0 {
		r.CapNeed *= capMult
	}
}

// foldRemoteRepBonuses is the remote twin: the server gives a player's remote
// rep a shorter cycle and a cheaper cycle from the skills it requires and no
// more HP per cycle, so a bot's does the same. Non-positive results are
// refused, as above.
func foldRemoteRepBonuses(r *botRepModule, cycleMult, capMult float64) {
	if r == nil || !r.Remote {
		return
	}
	if cycleMult > 0 {
		r.CycleSec *= cycleMult
	}
	if capMult > 0 {
		r.CapNeed *= capMult
	}
}

// notePassiveSpeed folds the passive half of the ship's legs (group-78 velocity modules and the
// plates' mass) into the loadout, for the abstract duel's speed (loadoutTopSpeed). These carry no
// activation effect for supportFromEffect to see.
func notePassiveSpeed(l *combatLoadout, groupID int64, attrs map[int64]float64) {
	if groupID == grpCargoExpander {
		if v := attrs[attrSpeedBonusPct]; v != 0 {
			l.velPcts = append(l.velPcts, v)
		}
	}
	if groupID == grpArmorReinforcer {
		l.massAdd += attrs[attrPropMass]
	}
}

// loadoutTopSpeed is the ship's top speed with its engine running: the hull, its pilot's Navigation,
// its passive velocity modules and the prop module on the shared classic formula. The abstract duel
// reads it for the stance, the same number the fit lens credits (fitstats.go PropVelocity).
func loadoutTopSpeed(l *combatLoadout, hullVel float64, navLevel int) float64 {
	if l == nil || hullVel <= 0 {
		return hullVel
	}
	v := hullVel * (1 + navigationPerLevel*float64(navLevel)) * stackPercentsLocal(append([]float64(nil), l.velPcts...))
	if l.prop != nil {
		v *= propBoost(l.prop.Spec, l.hullMass+l.massAdd)
	}
	return v
}

// navigationPerLevel is Navigation's +5% top speed per level, the same step
// allVVelocity folds at level V (fitstats.go).
const navigationPerLevel = 0.05

// combatSpeed is what this projection could fly at if it lit its engine. The stance is decided on
// it, or a ship with its engine off would never decide to kite and so never light it.
func combatSpeed(eb *embodiedBot, l *combatLoadout) float64 {
	v := eb.hullSpeed()
	if l == nil || l.prop == nil || l.prop.on {
		return v
	}
	return v * propBoost(l.prop.Spec, l.hullMass+l.massAdd)
}

// runPropCycle switches the engine on or off and pays for each cycle it runs.
func (m *Mod) runPropCycle(eb *embodiedBot, l *combatLoadout, want bool) {
	if l == nil || l.prop == nil || eb.ent == nil {
		return
	}
	p := l.prop
	if !want {
		m.propOff(eb, l)
		return
	}
	now := time.Now()
	if p.on && now.Before(p.nextCycleAt) {
		return
	}
	if !l.drawCap(p.CapNeed) {
		m.propOff(eb, l)
		return
	}
	p.nextCycleAt = now.Add(time.Duration(p.CycleSec * float64(time.Second)))
	if p.on {
		return
	}
	p.on = true
	boost := propBoost(p.Spec, l.hullMass+l.massAdd)
	newSpeed := eb.ent.SetSpeedModifier(speedModPropKey+p.ItemID, boost)
	m.broadcastMaxSpeed(eb, eb.ent, newSpeed)
	if p.Spec.SigBloom > 0 {
		eb.ent.SetSigRadiusModifier(1 + p.Spec.SigBloom/100)
	}
	m.moduleFX(eb, p.ItemID, p.TypeID, p.FXGUID, 0, true, int64(p.CycleSec*1000), false)
}

// propOff lifts the engine's boost and bloom.
func (m *Mod) propOff(eb *embodiedBot, l *combatLoadout) {
	if l == nil || l.prop == nil || !l.prop.on || eb.ent == nil {
		return
	}
	p := l.prop
	p.on = false
	if newSpeed, removed := eb.ent.RemoveSpeedModifier(speedModPropKey + p.ItemID); removed {
		m.broadcastMaxSpeed(eb, eb.ent, newSpeed)
	}
	if p.Spec.SigBloom > 0 {
		eb.ent.SetSigRadiusModifier(1)
	}
	m.moduleFX(eb, p.ItemID, p.TypeID, p.FXGUID, 0, false, 0, false)
}

// runRepCycles cycles every repairer that is due: local ones on the ship
// itself, remote ones on the most-wounded fleetmate in range.
func (m *Mod) runRepCycles(eb *embodiedBot, l *combatLoadout) {
	if l == nil || len(l.reps) == 0 || eb.ent == nil || eb.ss == nil {
		return
	}
	now := time.Now()
	var mates []*destiny.Entity
	var friends map[uint32]bool
	matesRead := false
	for _, r := range l.reps {
		if now.Before(r.nextCycleAt) {
			continue
		}
		var target *destiny.Entity
		if r.Remote {
			if !matesRead {
				mates, friends = m.repCandidates(eb)
				matesRead = true
			}
			target = mostWoundedInRange(eb.ent, mates, r.Kind, r.Range)
		} else if layerFrac(eb.ent, r.Kind) < localRepWoundedFrac {
			target = eb.ent
		}
		if target == nil {
			if r.appliedTo != 0 {
				m.moduleFX(eb, r.ItemID, r.TypeID, r.FXGUID, r.appliedTo, false, 0, false)
				r.appliedTo = 0
			}
			continue
		}
		if !l.drawCap(r.CapNeed) {
			continue
		}
		if r.Remote && friends[target.ID] && r.appliedTo != target.ID {
			m.logBot(eb.charID, logInfo, "logi", "repairing %s under fire", target.CharName)
		}
		r.nextCycleAt = now.Add(time.Duration(r.CycleSec * float64(time.Second)))
		m.applyRepair(eb, l, r, target)
	}
}

// applyRepair lands one cycle of one repairer.
func (m *Mod) applyRepair(eb *embodiedBot, l *combatLoadout, r *botRepModule, target *destiny.Entity) {
	switch r.Kind {
	case repShield:
		target.RepairShield(r.Amount)
	case repArmor:
		target.RepairArmor(r.Amount)
	case repHull:
		target.RepairStructure(r.Amount)
	case repCap:
		// Another projection's capacitor is its loadout's, not the entity's.
		if mate := lookupProjectionByShip(target.ID); mate != nil {
			mate.mu.Lock()
			ml := mate.load
			mate.mu.Unlock()
			if ml != nil {
				ml.capGiftMu.Lock()
				ml.capGift += r.Amount
				ml.capGiftMu.Unlock()
			}
		} else if target.HasCap() {
			target.BoostCap(r.Amount)
		}
	}
	if r.Kind != repCap {
		if h := modapi.PvP().HPChanged; h != nil {
			h(eb.ss, target)
		} else {
			m.broadcastDamageState(eb, target)
		}
	}
	targetID := uint32(0)
	if r.Remote {
		targetID = target.ID
	}
	first := r.appliedTo != target.ID
	r.appliedTo = target.ID
	m.moduleFX(eb, r.ItemID, r.TypeID, r.FXGUID, targetID, true, int64(r.CycleSec*1000), false)
	if first && r.Remote {
		m.logBot(eb.charID, logInfo, "logi", "%s on entity %d", r.Name, target.ID)
	}
}

// fleetmatesOnGrid is every ship this projection should mend: other projections flying for the same
// gang or player fleet in this system, and the player commanding it. Entities only, collected under
// embodyMu and read after it is released.
func (m *Mod) fleetmatesOnGrid(eb *embodiedBot) []*destiny.Entity {
	eb.mu.Lock()
	gangID, bossID := eb.gangID, eb.bossID
	eb.mu.Unlock()
	if gangID == 0 && bossID == 0 {
		return nil
	}
	var out []*destiny.Entity
	embodyMu.Lock()
	for _, o := range embodyLive {
		if o == eb || o.ss != eb.ss || o.ent == nil {
			continue
		}
		if (gangID != 0 && o.gangID == gangID) || (bossID != 0 && o.bossID == bossID) {
			out = append(out, o.ent)
		}
	}
	embodyMu.Unlock()
	if bossID != 0 {
		if boss := m.pilotBall(eb, bossID); boss != nil {
			out = append(out, boss)
		}
		// The other humans in a commander's fleet are fleetmates too.
		if k := conscriptOf(eb.charID); k != nil {
			for _, id := range gangsta.HumanMembersOnline(k.GangID) {
				if id == bossID {
					continue
				}
				if e := m.pilotBall(eb, id); e != nil {
					out = append(out, e)
				}
			}
		}
	}
	return out
}

// repCandidates is everything a remote repairer may mend: the fleet, plus any friend under fire on
// this grid outside it (combat_assist.go). friends marks the second kind, for the log. Only
// woundedness picks between them (mostWoundedInRange).
func (m *Mod) repCandidates(eb *embodiedBot) (cands []*destiny.Entity, friends map[uint32]bool) {
	cands = m.fleetmatesOnGrid(eb)
	extra := m.assistHealTargets(eb)
	if len(extra) == 0 {
		return cands, nil
	}
	have := make(map[uint32]bool, len(cands))
	for _, e := range cands {
		have[e.ID] = true
	}
	friends = make(map[uint32]bool, len(extra))
	for _, e := range extra {
		if have[e.ID] {
			continue
		}
		have[e.ID] = true
		friends[e.ID] = true
		cands = append(cands, e)
	}
	return cands, friends
}

// mostWoundedInRange is the fleetmate whose repaired layer is lowest, under
// the wounded threshold and within reach. Pure over the entities' own state.
func mostWoundedInRange(self *destiny.Entity, mates []*destiny.Entity, kind repKind, reach float64) *destiny.Entity {
	var best *destiny.Entity
	bestFrac := remoteRepWoundedFrac
	selfPos := self.Pos()
	for _, e := range mates {
		if e == nil || e.IsDestroyed() || e.IsInWarp() || e.IsCloaked() {
			continue
		}
		if reach > 0 && e.Pos().Sub(selfPos).Len() > reach {
			continue
		}
		f := layerFrac(e, kind)
		if f < bestFrac {
			best, bestFrac = e, f
		}
	}
	return best
}

// layerFrac is how full the layer a repairer mends is, 1 when it cannot be
// read (nothing is mended on a guess).
func layerFrac(e *destiny.Entity, kind repKind) float64 {
	if e == nil {
		return 1
	}
	hp := e.HPSnapshot()
	switch kind {
	case repShield:
		if hp.MaxShield > 0 {
			return hp.CurShield / hp.MaxShield
		}
	case repArmor:
		if hp.MaxArmor > 0 {
			return hp.CurArmor / hp.MaxArmor
		}
	case repHull:
		if hp.MaxStructure > 0 {
			return hp.CurStructure / hp.MaxStructure
		}
	case repCap:
		if cur, max := e.CurrentCap(); max > 0 {
			return cur / max
		}
	}
	return 1
}

// supportLoadoutIdle is the loadout a projection needs to mend its fleet while nothing shoots at it:
// loaded once on the guard tick for a pilot flying with a fleet, or with a friend under fire on its
// grid. Any other solo pilot keeps the lazy load.
func (m *Mod) supportLoadoutIdle(eb *embodiedBot, l *combatLoadout) *combatLoadout {
	if l != nil {
		return l
	}
	eb.mu.Lock()
	inFleet := eb.gangID != 0 || eb.bossID != 0
	eb.mu.Unlock()
	if eb.ent == nil {
		return nil
	}
	// Outside a fleet, load only once a friend is under fire here (combat_assist.go): the one time a
	// solo pilot's remote repairers have somebody to mend. The loop keeps it.
	if !inFleet && len(m.assistHealTargets(eb)) == 0 {
		return nil
	}
	dbCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	nl, err := m.loadCombatLoadout(dbCtx, eb.charID, eb.shipID, eb.ent.Slim.TypeID)
	if err != nil {
		return nil
	}
	// Published like the engaged path publishes it, so a fleetmate's energy
	// transfer has somewhere to land.
	eb.mu.Lock()
	eb.load = nl
	eb.mu.Unlock()
	return nl
}

var (
	skillByNameMu    sync.Mutex
	skillByNameCache = map[string]int64{}
)

// skillIDByNameCached is skillIDByName behind a process-wide cache of static data, for paths that
// must not pay a query per pass. A miss is cached as 0 too, so an unknown name costs one query.
func (m *Mod) skillIDByNameCached(ctx context.Context, name string) (int64, bool) {
	skillByNameMu.Lock()
	id, ok := skillByNameCache[name]
	skillByNameMu.Unlock()
	if ok {
		return id, id != 0
	}
	id, _ = m.skillIDByName(ctx, name)
	skillByNameMu.Lock()
	skillByNameCache[name] = id
	skillByNameMu.Unlock()
	return id, id != 0
}
