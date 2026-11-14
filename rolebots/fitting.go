package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"

	evedb "gurimoon/db"
)

// Fit data model and per-character validation (docs/bindings/fitting.md). Slot flags
// are low 11-18, mid 19-26, high 27-34 (dogma/flags.go is the authority; the /fit help
// string is inverted). Skills gate onlining, not fitting.

// Fitting-relevant attribute IDs (dogma/attributes.go).
const (
	attrPGOutput     = 11
	attrLowSlots     = 12
	attrMedSlots     = 13
	attrHiSlots      = 14
	attrPGUse        = 30
	attrCPUOutput    = 48
	attrCPUUse       = 50
	attrLauncherHard = 101
	attrTurretHard   = 102
	attrMiningAmount = 77
	attrDamageMult   = 64
)

// basicMinerTypeID is the rookie mining laser. The server exempts it from skill checks
// so a fresh character can always fit it (main.go missingFitSkills).
const basicMinerTypeID = 3651

// The four civilian starter weapons, one per empire. They have basePrice 0 and skip
// skill checks, so a pilot with nothing can be granted one. An unarmed hull has a zero
// engagement margin (threat.go) and hopeless() refuses it every belt.
const (
	civGatlingAutocannonTypeID = 3636 // Minmatar
	civGatlingPulseLaserTypeID = 3634 // Amarr
	civGatlingRailgunTypeID    = 3638 // Caldari
	civLightElectronBlasterID  = 3640 // Gallente
)

// civilianStarterWeapons is the set, for the fit generator's floor and the
// destitute grant. The pulse laser is not listed with a charge because the
// civilian variant draws no crystal.
var civilianStarterWeapons = map[int64]bool{
	civGatlingAutocannonTypeID: true,
	civGatlingPulseLaserTypeID: true,
	civGatlingRailgunTypeID:    true,
	civLightElectronBlasterID:  true,
}

// Fitting skills and their +5%/level capacity bonus (db/fittingdb.go).
const (
	skillElectronics          = 3426
	skillEngineering          = 3413
	fittingSkillBonusPerLevel = 0.05
)

// Slot flag bands (dogma/flags.go).
const (
	flagLoFirst  = 11
	flagMedFirst = 19
	flagHiFirst  = 27
	// The drone bay is not a slot band: it holds a quantity in a volume, not one module
	// per flag, so fillDroneBay is its own step (fitdrones.go).
	flagDroneBay = 87 // dogma/flags.go FlagDroneBay
)

// Module groups (invGroups, verified against this DB's static dump).
const (
	grpShieldExtender   = 38
	grpShieldBooster    = 40
	grpCapRecharger     = 43
	grpAfterburner      = 46
	grpSurveyScanner    = 49
	grpEnergyTurret     = 53
	grpMiningLaser      = 54
	grpProjTurret       = 55
	grpDamageControl    = 60
	grpArmorRepairer    = 62
	grpHybridTurret     = 74
	grpCargoExpander    = 78 // Expanded Cargohold / Overdrive share the group
	grpGyrostab         = 59
	grpHeatSink         = 205
	grpMagStab          = 302
	grpBCS              = 367
	grpStripMiner       = 464
	grpDeepCoreMiner    = 483
	grpRocketLauncher   = 507
	grpSiegeLauncher    = 508
	grpStandardLauncher = 509
	grpHeavyLauncher    = 510
	grpAssaultLauncher  = 511
	// Tackle. Group 52 is named "Jump Scrambler" in this database and holds both warp
	// scramblers and disruptors; group 65 is Stasis Web. They are matched by group because
	// the attributes are shared with non-tackle modules (a cloak has a negative speedFactor).
	grpWarpScrambler = 52
	grpStasisWeb     = 65
	// Logistics. Both remote-repair families are high-slot modules (dgmTypeEffects 12), each
	// with its own range attribute: shield 87, armour maxRange 54.
	grpShieldTransporter = 41  // remote shield rep, skill 3422 Shield Emission Systems
	grpArmorRepProjector = 325 // remote armor rep, skills 3393 + 16069
	grpEnergyTransfer    = 67  // remote cap, skill 3423 Energy Emission Systems
	// EWAR beyond tackle: the tackler's leftover mids, matched by group like tackle.
	grpECM               = 201
	grpSensorDamper      = 208
	grpTrackingDisruptor = 291
	grpTargetPainter     = 379
	// Active resists. These groups must be in the roleGroups lists, or hullAffinity answers
	// 0 and serves() refuses them on every hull.
	grpShieldHardener = 77
	grpArmorHardener  = 328
	// Armour buffer. Group 329 is the plate family; effArmorPlate (594) and attribute 335
	// are read by ComputeFitStats.
	grpArmorReinforcer = 329
	// Warp core stabiliser. The database names group 315 "Anti Warp Scrambler". See the
	// warpCoreStrength read in fitstats.go for the sign convention and the four base items
	// with strength zero.
	grpWarpCoreStab = 315
	// The gank scout's cargo scanner reads a mark's hold from 30 km (attr 126
	// cargoScanRange), which prices the strike.
	grpCargoScanner = 47
)

// FitModule is one placed module (with optional loaded charge).
type FitModule struct {
	TypeID       int64   `json:"typeID"`
	TypeName     string  `json:"typeName"`
	Flag         int64   `json:"flag"` // slot flag 11-34
	CPU          float64 `json:"cpu"`
	PG           float64 `json:"pg"`
	ChargeTypeID int64   `json:"chargeTypeID,omitempty"`
	ChargeName   string  `json:"chargeName,omitempty"`
	ChargeQty    int64   `json:"chargeQty,omitempty"`
}

// FitCargo is loose cargo the fit ships with (ammo reloads).
type FitCargo struct {
	TypeID   int64  `json:"typeID"`
	TypeName string `json:"typeName"`
	Qty      int64  `json:"qty"`
}

// Fit is a complete generated fitting plus the build report. It says nothing about who
// flies it or what for: what a fit is good at is read off its contents (fitjob.go,
// fitscore.go).
type Fit struct {
	HullTypeID    int64       `json:"hullTypeID"`
	HullName      string      `json:"hullName"`
	RepairTrigger string      `json:"repairTrigger"` // armor | structure
	Modules       []FitModule `json:"modules"`
	Cargo         []FitCargo  `json:"cargo"`  // user-declared cargo + ammo reloads
	Drones        []FitCargo  `json:"drones"` // user-declared drone bay contents
	Report        []string    `json:"report"`
	CPUUsed       float64     `json:"cpuUsed"`
	CPUCap        float64     `json:"cpuCap"` // at all-V fitting skills (template basis)
	PGUsed        float64     `json:"pgUsed"`
	PGCap         float64     `json:"pgCap"`
	// AmmoBudget caps what the ammunition may cost (capAmmoCargo); 0 is uncapped. It rides
	// the Fit because every trimmed copy (fiteffective.go) rebuilds the reload stacks and
	// must cap them again.
	AmmoBudget float64 `json:"ammoBudget,omitempty"`
}

// ModuleCheck is one line of a per-character fit validation.
type ModuleCheck struct {
	TypeName string
	Missing  string // "" = green; otherwise the unmet skill list
}

// FitCheck is ValidateFitForChar's result.
type FitCheck struct {
	Modules  []ModuleCheck // fitted modules, then drones, then charge types
	CPULoad  float64
	CPUCap   float64
	PGLoad   float64
	PGCap    float64
	OverGrid bool
	AllGreen bool
}

// ValidateFitForChar checks a template against one character's actual skills: every
// module, drone and charge must be usable, and the fit must sit inside the character's
// CPU/PG. One red line makes the bot refuse to buy any of it.
func (m *Mod) ValidateFitForChar(ctx context.Context, charID int64, fit *Fit) (*FitCheck, error) {
	skills, err := m.charSkillsFor(ctx, charID)
	if err != nil {
		return nil, fmt.Errorf("skills: %w", err)
	}
	chk := &FitCheck{AllGreen: true}

	checkType := func(typeID int64, label string) {
		mc := ModuleCheck{TypeName: label}
		// The server exempts the Basic Miner from skill checks (main.go missingFitSkills); the
		// exemption must hold here or a mining fit built on it reds for untrained pilots.
		if typeID == basicMinerTypeID {
			chk.Modules = append(chk.Modules, mc)
			return
		}
		if reqs, err := evedb.GetRequiredSkills(ctx, m.db, typeID); err == nil {
			if missing := evedb.UnmetSkillReqs(reqs, skills, nil); len(missing) > 0 {
				mc.Missing = evedb.FormatSkillReqs(ctx, m.db, missing)
				chk.AllGreen = false
			}
		}
		chk.Modules = append(chk.Modules, mc)
	}

	// Hull first: an unboardable hull reds the whole fit.
	checkType(fit.HullTypeID, fit.HullName+" (hull)")
	for _, mod := range fit.Modules {
		checkType(mod.TypeID, mod.TypeName)
		chk.CPULoad += mod.CPU
		chk.PGLoad += mod.PG
	}
	seenCharge := map[int64]bool{}
	for _, mod := range fit.Modules {
		if mod.ChargeTypeID != 0 && !seenCharge[mod.ChargeTypeID] {
			seenCharge[mod.ChargeTypeID] = true
			checkType(mod.ChargeTypeID, mod.ChargeName+" (charge)")
		}
	}
	for _, d := range fit.Drones {
		checkType(d.TypeID, d.TypeName+" (drone)")
	}

	hullAttrs, err := evedb.GetTypeAttributes(ctx, m.db, fit.HullTypeID)
	if err != nil {
		return nil, err
	}
	chk.CPUCap = hullAttrs[attrCPUOutput] * (1 + fittingSkillBonusPerLevel*float64(skills[skillElectronics]))
	chk.PGCap = hullAttrs[attrPGOutput] * (1 + fittingSkillBonusPerLevel*float64(skills[skillEngineering]))
	// Fitted grid modules add to the output (fitgrid.go) and the server's online check
	// credits them, so the fit is checked against the grid it will have.
	credit := identityGrid()
	for _, mod := range fit.Modules {
		if mi := m.moduleInfo(ctx, mod.TypeID); mi.isGrid {
			credit = credit.plus(mi.grid)
		}
	}
	chk.CPUCap, chk.PGCap = credit.apply(chk.CPUCap, chk.PGCap)
	if chk.CPULoad > chk.CPUCap || chk.PGLoad > chk.PGCap {
		chk.OverGrid = true
		chk.AllGreen = false
	}
	return chk, nil
}

func typeName(ctx context.Context, db *sql.DB, typeID int64) (string, error) {
	r, ok := typeRecFrom(ctx, db, typeID)
	if !ok {
		return "", fmt.Errorf("typeName %d: %w", typeID, sql.ErrNoRows)
	}
	return r.Name, nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	s := "?"
	for i := 1; i < n; i++ {
		s += ",?"
	}
	return s
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, v := range ids {
		out[i] = v
	}
	return out
}

// ── cargo bookkeeping ───────────────────────────────────────────────────────
// A fit's cargo is two things stacked: what was loaded as cargo, and the reload stacks
// implied by the charges in the guns. The generator and the per-pilot trimmer rebuild it.

// ammoReloadsFor is how many extra full loads of each charge go to cargo, sized to the
// trip the job flies. A pilot tops up at home, so the hold is the trip's magazine. It
// multiplies fitGenChargeQty (200, fitgen.go): read the two together.
func ammoReloadsFor(job string) int {
	switch job {
	case JobCombat, JobPiracy, JobTackle, JobLogi, JobEwar:
		return 4
	case JobTrade, JobFreight:
		return 1
	}
	return 2 // mining, and any job added later
}

// ammoTypes returns the set of charge types the fit's modules load. Their reload
// stacks are system-managed cargo, not user cargo.
func ammoTypes(fit *Fit) map[int64]bool {
	set := map[int64]bool{}
	for _, mod := range fit.Modules {
		if mod.ChargeTypeID != 0 {
			set[mod.ChargeTypeID] = true
		}
	}
	return set
}

// userCargo returns the cargo that is not an auto ammo reload.
func userCargo(fit *Fit) []FitCargo {
	ammo := ammoTypes(fit)
	var out []FitCargo
	for _, c := range fit.Cargo {
		if !ammo[c.TypeID] {
			out = append(out, c)
		}
	}
	return out
}

// regenAmmoCargo rebuilds Cargo as the non-ammo list plus fresh reload stacks derived
// from the loaded charges, at the depth the job calls for (ammoReloadsFor).
func regenAmmoCargo(fit *Fit, job string) {
	reloads := int64(ammoReloadsFor(job))
	user := userCargo(fit)
	reload := map[int64]*FitCargo{}
	var order []int64
	for _, mod := range fit.Modules {
		if mod.ChargeTypeID == 0 || mod.ChargeQty <= 0 {
			continue
		}
		if rc, ok := reload[mod.ChargeTypeID]; ok {
			rc.Qty += mod.ChargeQty * reloads
			continue
		}
		reload[mod.ChargeTypeID] = &FitCargo{
			TypeID: mod.ChargeTypeID, TypeName: mod.ChargeName,
			Qty: mod.ChargeQty * reloads,
		}
		order = append(order, mod.ChargeTypeID)
	}
	out := make([]FitCargo, 0, len(order)+len(user))
	for _, id := range order {
		out = append(out, *reload[id])
	}
	fit.Cargo = append(out, user...)
}

// regenAmmo rebuilds the reload stacks and holds them to the fit's ammunition budget.
// Every site that rebuilds the stacks goes through here, or a trimmed copy restores
// the full loads the budget took off.
func (m *Mod) regenAmmo(ctx context.Context, fit *Fit, job string) {
	regenAmmoCargo(fit, job)
	if fit.AmmoBudget > 0 {
		capAmmoCargo(fit, fit.AmmoBudget, m.chargeAsks(ctx))
	}
}

// capAmmoCargo trims the reload stacks so the whole ammunition bill (loaded rounds plus
// reloads) stays inside budget. Loaded rounds are never cut; reloads shrink in
// proportion, to nothing if the loaded rounds alone spend the budget. A charge with no
// known ask is left alone. Pure.
func capAmmoCargo(fit *Fit, budget float64, ask map[int64]float64) {
	if fit == nil || budget <= 0 || len(ask) == 0 {
		return
	}
	ammo := ammoTypes(fit)
	loaded := 0.0
	for _, mod := range fit.Modules {
		if mod.ChargeTypeID != 0 && mod.ChargeQty > 0 {
			loaded += float64(mod.ChargeQty) * ask[mod.ChargeTypeID]
		}
	}
	reloads := 0.0
	for _, c := range fit.Cargo {
		if ammo[c.TypeID] {
			reloads += float64(c.Qty) * ask[c.TypeID]
		}
	}
	if reloads <= 0 || loaded+reloads <= budget {
		return
	}
	share := (budget - loaded) / reloads
	if share < 0 {
		share = 0
	}
	kept := fit.Cargo[:0]
	for _, c := range fit.Cargo {
		if ammo[c.TypeID] && ask[c.TypeID] > 0 {
			c.Qty = int64(math.Floor(float64(c.Qty) * share))
			if c.Qty <= 0 {
				continue
			}
		}
		kept = append(kept, c)
	}
	fit.Cargo = kept
}

// chargeOpt is one loadable charge. BasePrice is a price to compare when the market has
// no ask; GroupID tells a magazine from a crystal (chargeQtyFor, fitammo.go).
type chargeOpt struct {
	ID        int64
	Name      string
	BasePrice float64
	GroupID   int64
}

// chargeOptionsCache holds the charges each module type accepts. They derive from seed
// data the server never writes, so each is computed once per process. The fit generator
// asks on every gun it fits.
var chargeOptionsCache sync.Map // moduleTypeID -> []chargeOpt

// nonOffensiveChargeGroups are charges a launcher accepts that cannot be fired at a
// ship. A Defender missile only intercepts missiles, but its explosiveDamage of 76 would
// rank it first for every launcher, so it is filtered at the source.
var nonOffensiveChargeGroups = map[int64]bool{
	88: true, // Defender Missile
}

// chargeOptions lists every charge loadable into a module type.
func (m *Mod) chargeOptions(ctx context.Context, moduleTypeID int64) ([]chargeOpt, error) {
	if v, ok := chargeOptionsCache.Load(moduleTypeID); ok {
		cached := v.([]chargeOpt)
		// A copy: callers filter and sort what they get back.
		out := make([]chargeOpt, len(cached))
		copy(out, cached)
		return out, nil
	}
	out, err := m.chargeOptionsUncached(ctx, moduleTypeID)
	if err != nil {
		return nil, err
	}
	stored := make([]chargeOpt, len(out))
	copy(stored, out)
	chargeOptionsCache.Store(moduleTypeID, stored)
	return out, nil
}

func (m *Mod) chargeOptionsUncached(ctx context.Context, moduleTypeID int64) ([]chargeOpt, error) {
	groups, err := evedb.GetChargeGroupsForModule(ctx, m.db, moduleTypeID)
	if err != nil || len(groups) == 0 {
		return nil, err
	}
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, COALESCE(t.basePrice, 0), t.groupID
		FROM invTypes t
		WHERE t.groupID IN (`+placeholders(len(groups))+`)
		  AND t.published = 1 AND t.marketGroupID > 0
		ORDER BY t.typeName ASC`, int64Args(groups)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var raw []chargeOpt
	for rows.Next() {
		var c chargeOpt
		if err := rows.Scan(&c.ID, &c.Name, &c.BasePrice, &c.GroupID); err != nil {
			return nil, err
		}
		raw = append(raw, c)
	}
	var out []chargeOpt
	for _, c := range raw {
		if nonOffensiveChargeGroups[c.GroupID] {
			continue
		}
		if evedb.ValidateChargeForModule(ctx, m.db, c.ID, moduleTypeID) == nil {
			out = append(out, c)
		}
	}
	return out, nil
}
