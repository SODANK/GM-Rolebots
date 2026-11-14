package rolebots

import (
	"context"
	"sort"
	"sync"

	evedb "gurimoon/db"
)

// A fit is resolved per pilot before anything else: every line the pilot cannot online is left off,
// then what is left is trimmed until it fits that pilot's own CPU and powergrid. The result is a
// buildable, flyable fit, and it is the one scored, priced, bought and checked from then on, so the
// bot never buys a module it cannot switch on. The hull is the one hard line: a ship the pilot
// cannot board is not a degraded fit.

// modInfo is what the trimmer needs to know about a module to say how much the job would miss it.
// Static data, cached for good.
type modInfo struct {
	groupID  int64
	turret   bool
	launcher bool
	// isGrid and grid are a fitting-capacity module and what it adds (fitgrid.go). The trimmer credits
	// it before judging the fit and ranks it above everything: shedding the module that paid for the
	// rest makes the overload worse.
	isGrid bool
	grid   gridCredit
}

// tackle reports whether this module stops a victim warping off, which a piracy fit cannot be
// trimmed without.
func (mi modInfo) tackle() bool {
	return mi.groupID == grpWarpScrambler || mi.groupID == grpStasisWeb
}

var (
	modInfoMu    sync.Mutex
	modInfoCache = map[int64]modInfo{}
)

func (m *Mod) moduleInfo(ctx context.Context, typeID int64) modInfo {
	modInfoMu.Lock()
	mi, ok := modInfoCache[typeID]
	modInfoMu.Unlock()
	if ok {
		return mi
	}
	mi.groupID = m.typeGroupOf(ctx, typeID)
	rows, err := m.db.QueryContext(ctx,
		`SELECT effectID FROM dgmTypeEffects WHERE typeID = ?`, typeID)
	if err == nil {
		for rows.Next() {
			var eff int64
			if rows.Scan(&eff) == nil {
				switch eff {
				case effTurretFitted:
					mi.turret = true
				case effLauncherFitted:
					mi.launcher = true
				}
			}
		}
		rows.Close()
	}
	// A fitting-capacity module (fitgrid.go): one of the three groups and a real grid attribute, since
	// group 57 also holds capacitor relays.
	switch mi.groupID {
	case grpPowerManager, grpCPUEnhancer, grpAuxPowerCore:
		attrs := map[int64]float64{}
		if arows, aerr := m.db.QueryContext(ctx, `
			SELECT attributeID, COALESCE(valueFloat, valueInt) FROM dgmTypeAttributes
			 WHERE typeID = ? AND attributeID IN (?, ?, ?)`,
			typeID, attrCPUMultMod, attrPGMult, attrPGAddFlat); aerr == nil {
			for arows.Next() {
				var id int64
				var v float64
				if arows.Scan(&id, &v) == nil {
					attrs[id] = v
				}
			}
			arows.Close()
		}
		if c, ok := moduleGridCredit(attrs); ok {
			mi.isGrid, mi.grid = true, c
		}
	}
	modInfoMu.Lock()
	modInfoCache[typeID] = mi
	modInfoMu.Unlock()
	return mi
}

// Module value to the job in hand; high is dropped last. The trimmer sheds the least valuable
// first, and the same ship resolves differently by job: a drone boat ratting keeps its guns and
// sheds the expanded cargohold, and hauling does the opposite.
const (
	modValueSpare   = 0 // utility: nice to have, first out
	modValueSupport = 1 // cap, speed, scanners
	modValueTank    = 2 // staying alive
	modValueBoost   = 3 // multiplies the job's output
	modValueCore    = 4 // the job itself: guns, miners, holds
)

func moduleValue(job string, mi modInfo, hasCharge bool) int {
	if mi.isGrid {
		return modValueCore
	}
	isWeapon := mi.turret || mi.launcher
	isMiner := isMiningModule(mi.groupID)
	isCargo := mi.groupID == grpCargoExpander
	isTank := false
	switch mi.groupID {
	case grpShieldExtender, grpShieldBooster, grpArmorRepairer, grpDamageControl:
		isTank = true
	}
	isDamageMod := false
	switch mi.groupID {
	case grpGyrostab, grpHeatSink, grpMagStab, grpBCS:
		isDamageMod = true
	}
	// The engine. Group 46 is the whole propulsion family. Every generated fit carries one, so it is
	// not shed early as support.
	isProp := mi.groupID == grpAfterburner
	isRemoteRep := mi.groupID == grpShieldTransporter || mi.groupID == grpArmorRepProjector
	isCap := mi.groupID == grpCapRecharger || mi.groupID == groupCapBatteryID
	// The stabiliser is the hauler's mirror of the pirate's point and is shed after the tank: a courier
	// survives by leaving, so a grid-tight hauler must not drop it.
	isWarpCore := mi.groupID == grpWarpCoreStab

	switch job {
	case JobMining:
		switch {
		case isMiner:
			return modValueCore
		case isCargo, mi.groupID == grpMiningUpgrade:
			return modValueBoost
		case isTank:
			return modValueTank
		case isProp:
			return modValueTank
		}
	case JobTrade:
		switch {
		case isCargo:
			return modValueCore
		// A courier's engine outranks its tank, because a hauler leaves rather than wins the fight. The
		// stabiliser is the same argument against the module that stops it leaving.
		case isProp, isWarpCore:
			return modValueBoost
		case isTank:
			return modValueTank
		}
	case JobFreight:
		// The expanders are the lift. A stabiliser is worth nothing here (the lens never asks for one),
		// so it sheds with the rest.
		switch {
		case isCargo:
			return modValueCore
		case isProp:
			return modValueBoost
		case isTank:
			return modValueTank
		}
	case JobLogi:
		// The repairers are the job and the capacitor keeps them cycling, so cap outranks tank here. The
		// engine keeps the wing in rep range and is shed with the tank.
		switch {
		case isRemoteRep:
			return modValueCore
		case isCap:
			return modValueBoost
		case isTank:
			return modValueTank
		case isProp:
			return modValueTank
		}
	case JobEwar:
		// The electronics are the seat, the engine holds the range they work at, and the guns go first.
		switch {
		case isEwarGroup(mi.groupID):
			return modValueCore
		case isProp:
			return modValueBoost
		case isTank:
			return modValueTank
		case isWeapon:
			return modValueSpare
		}
	case JobScout:
		// The scanner is the seat; the stabilisers and engine are how it leaves and the hold is what it
		// leaves with. A gun on a scout is trimmed first.
		switch {
		case mi.groupID == grpCargoScanner:
			return modValueCore
		case isWarpCore, isProp:
			return modValueBoost
		case isCargo:
			return modValueBoost
		case isTank:
			return modValueTank
		case isWeapon:
			return modValueSpare
		}
	case JobPiracy, JobTackle:
		// The point outranks the guns: a pirate without its scrambler cannot hold prey, so tackle is shed
		// last, after the weapons it makes useful. The tackle seat sheds the same way.
		switch {
		case mi.tackle():
			return modValueCore
		case isWeapon && !hasCharge:
			return modValueSpare
		// The engine is what puts the point in range, so it is shed with
		// the guns rather than before them.
		case isProp:
			return modValueBoost
		// The pirate's scanner prices the robbery (camp.go ransomScan), so it
		// sheds with the engine, not with the scrap.
		case job == JobPiracy && mi.groupID == grpCargoScanner:
			return modValueBoost
		case isWeapon:
			return modValueBoost
		case isDamageMod:
			return modValueSupport
		case isTank:
			return modValueTank
		}
	default: // combat
		switch {
		// A gun with no ammo this pilot can load is scrap on the hull: it
		// does nothing, so it is the first thing shed rather than the last.
		case isWeapon && !hasCharge:
			return modValueSpare
		case isWeapon:
			return modValueCore
		case isDamageMod:
			return modValueBoost
		case isTank:
			return modValueTank
		case isProp:
			return modValueTank
		}
	}
	if isTank || isProp {
		return modValueTank
	}
	return modValueSupport
}

// effectiveFitWithSkills is effectiveFit for callers that already hold the pilot's skill sheet: a
// plan pass resolves the whole pool at once and must not re-read it per fit.
func (m *Mod) effectiveFitWithSkills(ctx context.Context, charID int64, job string, t *fitTemplate,
	skills map[int64]int) (*fitTemplate, error) {

	usable := func(typeID int64) bool {
		if typeID == 0 {
			return false
		}
		// The server exempts the Basic Miner from skill checks so a rookie can always fit it (main.go
		// missingFitSkills); the same exemption holds here so a fresh pilot's mining fit survives.
		if typeID == basicMinerTypeID {
			return true
		}
		reqs, err := m.requiredSkills(ctx, typeID)
		if err != nil {
			return false
		}
		return len(evedb.UnmetSkillReqs(reqs, skills, nil)) == 0
	}

	if !usable(t.Fit.HullTypeID) {
		return nil, nil // cannot board it: not this pilot's ship at all
	}

	eff := *t.Fit // shallow copy; every slice below is rebuilt
	eff.Modules = nil
	eff.Cargo = nil
	eff.Drones = nil
	out := &fitTemplate{
		FitID: t.FitID, Name: t.Name, HullTypeID: t.HullTypeID,
		RepairTrigger: t.RepairTrigger, Created: t.Created,
		EffectiveFor: charID, Job: job, Fit: &eff,
		Style: t.Style, Lean: t.Lean,
	}

	for _, mod := range t.Fit.Modules {
		if !usable(mod.TypeID) {
			out.Dropped = append(out.Dropped, mod.TypeName)
			continue
		}
		// A module it can online but whose ammo it cannot load keeps the slot and loses the charge; it
		// may still be worth having (a launcher without its missiles yet), so scoring decides.
		if mod.ChargeTypeID != 0 && !usable(mod.ChargeTypeID) {
			out.Dropped = append(out.Dropped, mod.ChargeName)
			mod.ChargeTypeID, mod.ChargeName, mod.ChargeQty = 0, "", 0
		}
		eff.Modules = append(eff.Modules, mod)
	}
	// The bay is trimmed to what this pilot can use and then to what it can launch. The template is
	// built on an all-V basis (a full rack plus spares), so a Drones I pilot would buy ten to fly one.
	maxLaunched := int64(skills[skillDrones])
	for _, d := range t.Fit.Drones {
		if !usable(d.TypeID) {
			out.Dropped = append(out.Dropped, d.TypeName)
			continue
		}
		if maxLaunched <= 0 {
			// No Drones skill: it can own drones and not launch one, so the bay is dead weight.
			out.Dropped = append(out.Dropped, d.TypeName)
			continue
		}
		if cap := maxLaunched * (1 + droneSpareSets); d.Qty > cap {
			d.Qty = cap
		}
		eff.Drones = append(eff.Drones, d)
	}
	// Operator-declared cargo survives untouched; ammo stacks are rebuilt from the charges still
	// loaded, so nothing buys reloads for a gun that came off.
	eff.Cargo = userCargo(t.Fit)
	m.regenAmmo(ctx, &eff, job)

	if err := m.trimToGrid(ctx, charID, out, skills); err != nil {
		return nil, err
	}
	return out, nil
}

// trimToGrid sheds modules until the fit sits inside this pilot's CPU and powergrid (its own
// skills, not the all-V basis). Least valuable to the job goes first (moduleValue), then the
// biggest consumer, which frees the most grid for the fewest empty slots. A fit with nothing of
// the job left scores zero and is never bought.
func (m *Mod) trimToGrid(ctx context.Context, charID int64, t *fitTemplate, skills map[int64]int) error {
	hullAttrs, err := evedb.GetTypeAttributes(ctx, m.db, t.Fit.HullTypeID)
	if err != nil {
		return err
	}
	cpuCap := hullAttrs[attrCPUOutput] * (1 + fittingSkillBonusPerLevel*float64(skills[skillElectronics]))
	pgCap := hullAttrs[attrPGOutput] * (1 + fittingSkillBonusPerLevel*float64(skills[skillEngineering]))
	// Grid modules in the fit add to the pilot's output as the server credits them
	// (db.GetShipCPUPGCapacityFitted); otherwise guns are shed for want of what the unit supplies.
	credit := identityGrid()
	for _, mod := range t.Fit.Modules {
		if mi := m.moduleInfo(ctx, mod.TypeID); mi.isGrid {
			credit = credit.plus(mi.grid)
		}
	}
	cpuCap, pgCap = credit.apply(cpuCap, pgCap)

	load := func() (cpu, pg float64) {
		for _, mod := range t.Fit.Modules {
			cpu += mod.CPU
			pg += mod.PG
		}
		return
	}
	cpu, pg := load()
	if cpu <= cpuCap && pg <= pgCap {
		return nil
	}

	values := make([]int, len(t.Fit.Modules))
	draws := make([]float64, len(t.Fit.Modules))
	for i, mod := range t.Fit.Modules {
		mi := m.moduleInfo(ctx, mod.TypeID)
		values[i] = moduleValue(t.Job, mi, mod.ChargeTypeID != 0)
		draws[i] = mod.CPU + mod.PG
	}

	drop := map[int]bool{}
	for _, idx := range shedOrder(values, draws) {
		if cpu <= cpuCap && pg <= pgCap {
			break
		}
		mod := t.Fit.Modules[idx]
		drop[idx] = true
		cpu -= mod.CPU
		pg -= mod.PG
		t.Dropped = append(t.Dropped, mod.TypeName+" (over this pilot's grid)")
	}
	if len(drop) == 0 {
		return nil
	}
	kept := make([]FitModule, 0, len(t.Fit.Modules)-len(drop))
	for i, mod := range t.Fit.Modules {
		if !drop[i] {
			kept = append(kept, mod)
		}
	}
	t.Fit.Modules = kept
	t.Fit.Cargo = userCargo(t.Fit)
	m.regenAmmo(ctx, t.Fit, t.Job)
	return nil
}

// shedOrder ranks module indices by the order they should come off a fit
// that is over its pilot's grid: least valuable to the job first, and
// within a tier the heaviest draw first, because that frees the most
// grid for the fewest empty slots.
func shedOrder(values []int, draws []float64) []int {
	idx := make([]int, len(values))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ia, ib := idx[a], idx[b]
		if values[ia] != values[ib] {
			return values[ia] < values[ib]
		}
		return draws[ia] > draws[ib]
	})
	return idx
}

// droppedSummary renders what was left off, for the log and the panel.
func droppedSummary(dropped []string) string {
	if len(dropped) == 0 {
		return ""
	}
	s := ""
	for i, d := range dropped {
		if i > 0 {
			s += ", "
		}
		s += d
	}
	return s
}
