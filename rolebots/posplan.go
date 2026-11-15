package rolebots

import (
	"encoding/json"
	"math"
	"sort"
)

// Tower planning: what a tower is for, how big it is, whose it is and what stands around it.
// Everything here is pure: the CEO's starbase power (posceo.go) reads the world into these
// inputs and acts on the answers.

// Moon classes, by the core's index (db.MoonClassNames): R4..R64.
const (
	moonR4  = 0
	moonR8  = 1
	moonR16 = 2
	moonR32 = 3
	moonR64 = 4
)

// Tower sizes.
const (
	towerSmall  = "S"
	towerMedium = "M"
	towerLarge  = "L"
)

// Races, as invTypes.raceID / character_.raceID.
const (
	raceCaldari  = 1
	raceMinmatar = 2
	raceAmarr    = 4
	raceGallente = 8
)

// towerTypes is every shipped player control tower by race and size.
var towerTypes = map[int64]map[string]int64{
	raceAmarr:    {towerLarge: 12235, towerMedium: 20059, towerSmall: 20060},
	raceCaldari:  {towerLarge: 16213, towerMedium: 20061, towerSmall: 20062},
	raceGallente: {towerLarge: 12236, towerMedium: 20063, towerSmall: 20064},
	raceMinmatar: {towerLarge: 16214, towerMedium: 20065, towerSmall: 20066},
}

// towerRaceOf is the race of a tower type (0 for none).
func towerRaceOf(typeID int64) (race int64, size string) {
	for r, sizes := range towerTypes {
		for s, t := range sizes {
			if t == typeID {
				return r, s
			}
		}
	}
	return 0, ""
}

// raceIsotope is the isotope a race's towers burn.
var raceIsotope = map[int64]int64{
	raceAmarr:    16274, // Helium
	raceCaldari:  17888, // Nitrogen
	raceGallente: 17887, // Oxygen
	raceMinmatar: 17889, // Hydrogen
}

// towerGrid is what each size can power, before anything is anchored.
var towerGrid = map[string]struct{ Power, CPU float64 }{
	towerSmall:  {1_250_000, 1375},
	towerMedium: {2_500_000, 2750},
	towerLarge:  {5_000_000, 5500},
}

// Structure types the layouts use (category 23, all NPC-sold).
const (
	typeHarvester       = 16221 // Moon Harvesting Array (quality 1)
	typeSilo            = 14343
	typeRefiningArray   = 12238 // 35% yield: worse than a station, never planned
	typeMedRefinery     = 12239 // Medium Intensive Refining Array, 75%
	typeBigRefinery     = 19470 // Intensive Refining Array, 75%, Large towers only
	typeCHA             = 17621 // Corporate Hangar Array
	typeSMA             = 12237 // Ship Maintenance Array
	typeMediumReactor   = 20175
	typeReactor         = 16869
	typeMobileLab       = 16216
	typeAmmoAssembly    = 24658
	typeComponentAsm    = 24660
	typeSmallShipAsm    = 24574
	typeEquipmentAsm    = 16220 // Rapid Equipment Assembly Array
	typeHardenerEM      = 17187 // Photon Scattering
	typeHardenerThermal = 17186 // Heat Dissipation
	typeHardenerKin     = 17184 // Ballistic Deflection
	typeHardenerExp     = 17185 // Explosion Dampening
)

// structFit is one structure's draw on the tower grid.
type structFit struct{ Power, CPU float64 }

// structGrid is what each layout structure draws (dgmTypeAttributes 30/50).
var structGrid = map[int64]structFit{
	typeHarvester:       {10000, 500},
	typeSilo:            {50000, 500},
	typeRefiningArray:   {100000, 700},
	typeMedRefinery:     {375000, 2000},
	typeBigRefinery:     {750000, 4000},
	typeCHA:             {100000, 150},
	typeSMA:             {250000, 0},
	typeMediumReactor:   {125000, 1500},
	typeReactor:         {250000, 3000},
	typeMobileLab:       {100000, 500},
	typeAmmoAssembly:    {50000, 150},
	typeComponentAsm:    {50000, 150},
	typeSmallShipAsm:    {100000, 250},
	typeEquipmentAsm:    {110000, 200},
	typeHardenerEM:      {150000, 250},
	typeHardenerThermal: {150000, 250},
	typeHardenerKin:     {150000, 250},
	typeHardenerExp:     {150000, 250},
	// Grid boosters (grid.go): they draw a little and add output (boosterOut).
	typeAuxSmall:  {50, 200},
	typeAuxMedium: {50, 200},
	typeAuxLarge:  {50, 200},
	typeShieldGen: {50, 200},
}

// raceGuns is each race's battery by tower size, the weapon its tower's bonuses favour. Power
// only; batteries draw no CPU.
var raceGuns = map[int64]map[string]int64{
	raceAmarr:    {towerSmall: 17408, towerMedium: 17407, towerLarge: 17406}, // pulse lasers
	raceCaldari:  {towerSmall: 16690, towerMedium: 16691, towerLarge: 16692}, // railguns
	raceGallente: {towerSmall: 17404, towerMedium: 17403, towerLarge: 17402}, // blasters
	raceMinmatar: {towerSmall: 17772, towerMedium: 17771, towerLarge: 17770}, // autocannons
}

// ewBatteryGrid is the EW batteries a tower may grow (posinfra.go), NPC-sold:
// the four racial ECMs, sensor damping, the web and the two warp jammers.
// They stand outside the field on compass slots like the guns.
var ewBatteryGrid = map[int64]structFit{
	17174: {25000, 50}, // Ion Field Projection Battery (ECM)
	17175: {25000, 50}, // Phase Inversion Battery (ECM)
	17176: {25000, 50}, // Spatial Destabilization Battery (ECM)
	17177: {25000, 50}, // White Noise Generation Battery (ECM)
	17180: {12500, 25}, // Sensor Dampening Battery
	17178: {50000, 25}, // Stasis Webification Battery
	17181: {100000, 50}, // Warp Disruption Battery
	17182: {25000, 25}, // Warp Scrambling Battery
}

const (
	typeWebBattery   = 17178
	typeScramBattery = 17182
)

// ecmBatteries is the racial ECM batteries, in the order a tower adds them.
var ecmBatteries = []int64{17177, 17174, 17175, 17176}

// isDefenseType reports whether a structure is a battery: it stands outside
// the force field on a compass slot (the core's defense groups).
func isDefenseType(t int64) bool {
	if gunPower[t] > 0 {
		return true
	}
	_, ok := ewBatteryGrid[t]
	return ok
}

// isGunType is a battery that fires ammunition (or crystals).
func isGunType(t int64) bool { return gunPower[t] > 0 }

// pieceFit is any layout structure's draw on the grid.
func pieceFit(t int64) structFit {
	if f, ok := structGrid[t]; ok {
		return f
	}
	if f, ok := ewBatteryGrid[t]; ok {
		return f
	}
	return structFit{Power: gunPower[t]}
}

var gunPower = map[int64]float64{
	17408: 78750, 17407: 157500, 17406: 315000,
	16690: 90000, 16691: 180000, 16692: 360000,
	17404: 56250, 17403: 112500, 17402: 225000,
	17772: 67500, 17771: 135000, 17770: 270000,
}

// towerGuns is how many batteries a tower of a size carries.
var towerGuns = map[string]int{towerSmall: 2, towerMedium: 3, towerLarge: 4}

// layoutPiece is one structure in a tower's plan, with where it stands
// relative to the tower and (for a harvester or silo) the material it is set
// to handle.
type layoutPiece struct {
	TypeID    int64   `json:"t"`
	DX        float64 `json:"x"`
	DY        float64 `json:"y"`
	DZ        float64 `json:"z"`
	Provision int64   `json:"p,omitempty"`
	ItemID    int64   `json:"i,omitempty"` // the anchored structure, once it stands
	// Slot is a battery's compass slot (gunSlotOffset), 1-based; 0 for a
	// working structure, or a battery placed before slots.
	Slot int `json:"s,omitempty"`
	// Off is a structure Infrastructure took offline to spare the grid or
	// the fuel (posinfra.go); refit leaves it offline.
	Off bool `json:"o,omitempty"`
	// Added is when an upgrade put the piece in the plan (unix), 0 for one
	// the tower was planned with.
	Added int64 `json:"a,omitempty"`
	// Strip is a structure a refit no longer wants (posreconsider.go): emptied, unanchored,
	// carried to the office and dropped from the plan, never re-ordered as a loss. Its JSON key must
	// not be "x" (DX's): encoding/json drops both fields when two tags share a name (repairLostX).
	Strip bool `json:"st,omitempty"`
}

// layoutLostX reports whether a stored layout lost its X offsets: some piece carries no "x" at
// all. DX has no omitempty, so a healthy layout carries one per piece. Pure.
func layoutLostX(raw string) bool {
	if raw == "" {
		return false
	}
	var probe struct {
		Pieces []map[string]json.RawMessage `json:"pieces"`
	}
	if json.Unmarshal([]byte(raw), &probe) != nil {
		return false
	}
	for _, pc := range probe.Pieces {
		if _, ok := pc["x"]; !ok {
			return true
		}
	}
	return false
}

// rebuildLostX restores a layout whose X offsets were lost. A standing piece takes its real
// offset from space (standing maps itemID to offset from the tower); an unanchored battery takes
// its compass slot again; any other unanchored piece is placed afresh. Returns the layout and how
// many pieces no longer fit. Pure.
func rebuildLostX(pieces []layoutPiece, size string, race, seed int64, standing map[int64][3]float64) ([]layoutPiece, int) {
	out := append([]layoutPiece(nil), pieces...)
	for i := range out {
		pc := &out[i]
		if pc.ItemID != 0 {
			if o, ok := standing[pc.ItemID]; ok {
				pc.DX, pc.DY, pc.DZ = o[0], o[1], o[2]
			}
			continue // standing: where it is, or as stored when space does not say
		}
		if pc.Slot > 0 {
			if x, y, z, ok := gunSlotOffset(pc.Slot, size); ok {
				pc.DX, pc.DY, pc.DZ = x, y, z
				continue
			}
			pc.Slot = 0 // placeDefenses hands it a slot that exists
		}
		pc.DX, pc.DY, pc.DZ = 0, 0, 0
	}
	return placeLayout(out, size, race, seed)
}

// towerLayout is a tower's whole plan.
type towerLayout struct {
	Pieces []layoutPiece `json:"pieces"`
	// Infra is Infrastructure's book on the tower (posinfra.go).
	Infra towerInfraBook `json:"infra,omitempty"`
	// ReconsiderAt is when the CEO last asked whether the tower still serves
	// the corporation, and ReconsiderNote what it concluded (posreconsider.go).
	ReconsiderAt   int64  `json:"rc,omitempty"`
	ReconsiderNote string `json:"rn,omitempty"`
}

// sizeForMoonClass is the plan's rule: what a mining tower must be to hold the harvesters and
// guns its best class is worth. R64 is Large, R16 and R32 Medium, R8 and R4 Small. Pure.
func sizeForMoonClass(class int) string {
	switch {
	case class >= moonR64:
		return towerLarge
	case class >= moonR16:
		return towerMedium
	}
	return towerSmall
}

// sizeForArrays sizes a non-mining tower by need: one array Small, two or
// three Medium, more Large. Pure.
func sizeForArrays(n int) string {
	switch {
	case n <= 1:
		return towerSmall
	case n <= 3:
		return towerMedium
	}
	return towerLarge
}

// harvestersFor is how many harvesters a size powers (CPU-bound: each
// harvester wants a silo beside it). Pure.
func harvestersFor(size string) int {
	switch size {
	case towerLarge:
		return 4
	case towerMedium:
		return 2
	}
	return 1
}

// layoutInput is what a layout needs to know.
type layoutInput struct {
	Purpose string
	Size    string
	Race    int64
	// Materials is the moon's harvestable materials, best first (mining).
	Materials []int64
	// Tags is the corporation's identity (a mining outfit adds refining).
	Tags []string
	// Arrays is the arrays a factory/lab tower wants, in order.
	Arrays []int64
	// Reaction is the reaction a reactions tower's reactor runs; Materials
	// then lists its inputs and outputs, one silo each.
	Reaction int64
	// Harvest is the reaction inputs a reactions tower mines on its own moon (the Medium
	// miner+reactor): a harvester each, buffered through a silo where the grid allows
	// (reactionShape), else fed straight into the reactor. Empty for a hub.
	Harvest []int64
	// Seed scatters the structures (placeLayout): the moon, so a layout
	// replays identically and no two towers look alike.
	Seed int64
}

// reactionMaxBoosters is how many Shield Generation Arrays a miner+reactor
// may lean on: one (12M) is cheaper than the silo it makes room for (15M);
// the stacking penalty makes a third add nothing.
const reactionMaxBoosters = 1

// reactionShape is how a miner+reactor fits its grid: how many harvested inputs get a silo
// buffer, and the boosters that make the room. It tries every input buffered, then one fewer,
// down to none, and keeps the first that fits with at most reactionMaxBoosters. An exact fit
// means the next structure added needs a booster first (addPieces, posinfra.go). Pure.
func reactionShape(race int64, size string, harvesters int) (buffers int, boosters []int64) {
	for buffers = harvesters; buffers >= 0; buffers-- {
		draw := structFit{}
		addDraw := func(t int64, n int) {
			f := pieceFit(t)
			draw.Power += f.Power * float64(n)
			draw.CPU += f.CPU * float64(n)
		}
		addDraw(typeHarvester, harvesters)
		addDraw(typeSilo, buffers+1) // buffers plus the output silo
		if size == towerLarge {
			addDraw(typeReactor, 1)
		} else {
			addDraw(typeMediumReactor, 1)
		}
		addDraw(typeCHA, 1)
		g := gridFor(race, size)
		if draw.Power <= g.Power && draw.CPU <= g.CPU {
			return buffers, nil
		}
		if boost := boostersToCover(race, size, nil, draw); len(boost) > 0 && len(boost) <= reactionMaxBoosters {
			return buffers, boost
		}
	}
	return 0, nil
}

// buildLayout lays a tower out for its purpose within its grid, in priority
// order: the purpose's structures first, then a hangar, then guns while power
// lasts. Anything that would overdraw power or CPU is dropped. Pure.
func buildLayout(in layoutInput) towerLayout {
	var want []layoutPiece
	add := func(t, prov int64) { want = append(want, layoutPiece{TypeID: t, Provision: prov}) }
	switch in.Purpose {
	case purposeMining:
		n := harvestersFor(in.Size)
		for i := 0; i < n && i < len(in.Materials); i++ {
			add(typeHarvester, in.Materials[i])
			add(typeSilo, in.Materials[i])
		}
		add(typeCHA, 0)
		// No refining array here: the plain one yields 35%, worse than any station, and refining is
		// the refinery tower's job.
	case purposeRefinery:
		if in.Size == towerLarge {
			add(typeBigRefinery, 0)
		} else {
			add(typeMedRefinery, 0)
		}
		add(typeCHA, 0)
	case purposeReactions:
		// Harvested inputs first (a harvester each, the first `buffers` with a silo before the reactor),
		// then the reactor, then a silo for every unharvested input (a hub's) and every output.
		buffers, _ := reactionShape(in.Race, in.Size, len(in.Harvest))
		buffered := map[int64]bool{}
		for i, mat := range in.Harvest {
			add(typeHarvester, mat)
			if i < buffers {
				add(typeSilo, mat)
				buffered[mat] = true
			}
		}
		if in.Size == towerLarge {
			add(typeReactor, in.Reaction)
		} else {
			add(typeMediumReactor, in.Reaction)
		}
		for _, mat := range in.Materials {
			if buffered[mat] || containsInt64(in.Harvest, mat) {
				continue
			}
			add(typeSilo, mat)
		}
		add(typeCHA, 0)
	case purposeFactory, purposeLab:
		for _, a := range in.Arrays {
			add(a, 0)
		}
		add(typeCHA, 0)
	case purposeDepot:
		add(typeCHA, 0)
		add(typeSMA, 0)
	case purposeFortress:
		add(typeSMA, 0)
		add(typeCHA, 0)
		add(typeHardenerEM, 0)
		add(typeHardenerThermal, 0)
		add(typeHardenerKin, 0)
		add(typeHardenerExp, 0)
	}
	// A purpose whose own structures overdraw the tower's grid (a Caldari refinery is power-bound,
	// a Gallente lab CPU-bound) takes boosters first, ordered ahead of what leans on them.
	if boost := boostersToCover(in.Race, in.Size, nil, planDraw(want)); len(boost) > 0 {
		pre := make([]layoutPiece, 0, len(boost)+len(want))
		for _, t := range boost {
			pre = append(pre, layoutPiece{TypeID: t})
		}
		want = append(pre, want...)
	}
	guns := towerGuns[in.Size]
	if in.Purpose == purposeFortress {
		guns *= 2
	}
	if g := raceGuns[in.Race][in.Size]; g != 0 {
		for i := 0; i < guns; i++ {
			add(g, 0)
		}
	}
	var out towerLayout
	power, cpu := 0.0, 0.0
	for _, p := range want {
		f := pieceFit(p.TypeID)
		// The grid it fits is the tower's own plus the boosters kept so far.
		g := boostedGrid(in.Race, in.Size, out.Pieces)
		if isBoosterType(p.TypeID) {
			g = boostedGrid(in.Race, in.Size, append(append([]layoutPiece(nil), out.Pieces...), p))
		}
		if power+f.Power > g.Power || cpu+f.CPU > g.CPU {
			continue
		}
		power += f.Power
		cpu += f.CPU
		out.Pieces = append(out.Pieces, p)
	}
	// Batteries on compass slots outside the force field (the core refuses one inside it,
	// evedb.ForceFieldAnchorRefusal); everything else scattered inside (posplace.go).
	out.Pieces, _ = placeLayout(out.Pieces, in.Size, in.Race, in.Seed)
	return out
}

// towerShield is each tower size's force-field radius (shieldRadius, 680) and towerReach its
// anchoring range (maxStructureDistance, 650); both are the same for every race in this data.
var (
	towerShield = map[string]float64{towerSmall: 15000, towerMedium: 21215, towerLarge: 30000}
	towerReach  = map[string]float64{towerSmall: 25000, towerMedium: 35000, towerLarge: 50000}
)

// ── purpose ─────────────────────────────────────────────────────────────────

// purposeTags is which corporation tags weight each purpose, and by how much. Moon mining is
// open to every tag, at full weight only for a mining outfit. Reactions are the research house's
// tower first, then the chemical and manufacturing trades; mining, trade and combat carry none.
var purposeTags = map[string]map[string]float64{
	purposeMining:    {"mining": 1},
	purposeReactions: {"research": 1.5, "chemical": 1.5, "industry": 1},
	purposeFactory:   {"industry": 1, "shipbuilding": 1, "cartelindustry": 1},
	purposeLab:       {"research": 1},
	purposeDepot:     {"trade": 1, "hauling": 1},
	purposeFortress:  {"combat": 1, "security": 1, "outlaw": 1},
	purposeRefinery:  {"mining": 1, "industry": 1},
}

// tagAppeal is how strongly a corporation's tags pull it to a purpose: the
// sum of its tags' weights. Mining keeps a floor for everyone. Pure.
func tagAppeal(purpose string, tags []string) float64 {
	v := 0.0
	for t, w := range purposeTags[purpose] {
		if containsStr(tags, t) {
			v += w
		}
	}
	if purpose == purposeMining && v == 0 {
		return 0.35
	}
	return v
}

// purposeSignal is one purpose's case this think: the corporation's own need
// for it (0 = none) and why.
type purposeSignal struct {
	Purpose string
	Need    float64
	Why     string
	// Systems, when set, is the only systems the case stands in: a fortress goes where the
	// sovereignty it holds down is, nowhere else. nil means anywhere the site rules allow.
	Systems map[int64]bool
	// Hub marks a reactions case for the Large complex-reaction hub fed by
	// the corporation's other towers, as against a miner+reactor on a moon
	// of its own (posindustry.go).
	Hub bool
}

// fortressMinSystemTowers is how many towers the corporation and its alliance-mates must
// already run in a 0.0 system before a fortress is added to hold the sovereignty count there.
// A fortress earns nothing, so it is never an early tower in a system.
const fortressMinSystemTowers = 3

// fortressSystems is the fortress rule, pure: the systems where a fortress has a point. A
// corporation in no alliance holds nothing under the majority rule (db/sovmajority.go) and
// never builds one. Otherwise a system qualifies when it is contestable 0.0 and the corporation
// plus its alliance-mates already run fortressMinSystemTowers towers there (towers: per system).
func fortressSystems(allianceID int64, towers map[int64]int, contestable func(sys int64) bool) map[int64]bool {
	if allianceID == 0 {
		return nil
	}
	var out map[int64]bool
	for sys, n := range towers {
		if n < fortressMinSystemTowers || !contestable(sys) {
			continue
		}
		if out == nil {
			out = map[int64]bool{}
		}
		out[sys] = true
	}
	return out
}

// purposeScore is tag appeal * need * affordability / danger, with danger at least 0.1. Pure.
func purposeScore(sig purposeSignal, tags []string, afford, danger float64) float64 {
	if sig.Need <= 0 || afford <= 0 {
		return 0
	}
	return tagAppeal(sig.Purpose, tags) * sig.Need * afford / math.Max(0.1, danger)
}

// rankPurposes orders the purposes a corporation has a case for. Pure.
func rankPurposes(sigs []purposeSignal, tags []string, afford, danger float64) []purposeSignal {
	type sc struct {
		s purposeSignal
		v float64
	}
	var list []sc
	for _, s := range sigs {
		if v := purposeScore(s, tags, afford, danger); v > 0 {
			list = append(list, sc{s, v})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].v > list[j].v })
	out := make([]purposeSignal, 0, len(list))
	for _, l := range list {
		out = append(out, l.s)
	}
	return out
}

// moonFitsPurpose is the moon-use rule: an R32 or R64 moon is towered only to mine it; a
// reactions tower may stand on anything up to R16 (a miner+reactor harvests its own R8/R16
// pair); every other purpose lives on R8, R4 or an empty moon (an R8 tower may be dual
// purpose). class < 0 is an empty or unsurveyed moon. Pure.
func moonFitsPurpose(class int, purpose string) bool {
	switch purpose {
	case purposeMining:
		return class >= moonR4
	case purposeReactions:
		return class <= moonR16
	}
	return class <= moonR8
}

// ── race ────────────────────────────────────────────────────────────────────

// pickTowerRace chooses whose tower to build. A tolerant CEO goes by economics: the race whose
// isotope is cheapest where the tower will stand (isotopeCost, ISK per unit landed there). A
// xenophobe goes by xenoItemMul (tolerance.go): its own race, or allied by stage, whatever the
// hauling costs. Ties fall to the CEO's own race. Pure, given the cost table.
func pickTowerRace(ceoRace int64, phobia float64, isotopeCost map[int64]float64) int64 {
	best, bestScore := int64(0), math.Inf(-1)
	for _, race := range []int64{raceAmarr, raceCaldari, raceGallente, raceMinmatar} {
		mul, ok := xenoItemMul(ceoRace, phobia, race)
		if !ok || mul <= 0 {
			continue
		}
		cost := isotopeCost[race]
		if cost <= 0 {
			cost = 1e9 // nobody sells it in reach: possible, never preferred
		}
		score := mul / cost
		if race == ceoRace {
			score *= 1.0001
		}
		if score > bestScore {
			best, bestScore = race, score
		}
	}
	if best == 0 {
		best = ceoRace
	}
	if _, ok := towerTypes[best]; !ok {
		best = raceAmarr
	}
	return best
}

// ── placement ───────────────────────────────────────────────────────────────

// siteCandidate is one moon a tower could go on.
type siteCandidate struct {
	MoonID   int64
	SystemID int64
	Class    int     // best class (-1 empty)
	Value    float64 // ISK per hour a harvester set would make (mining)
	Jumps    int     // from the nearest office the corporation holds (its platform)
	Office   int64   // that office's station: where the tower is staged and supplied
	Security float64
	Danger   float64 // ≥ 0
	Cluster  int     // jumps to the nearest tower the corporation already runs (-1 none)
	// AllianceTarget: the system is one the alliance's executor has named
	// for its towers (alliancesov.go). Set only with the sov switch on.
	AllianceTarget bool
	// Frontier: unclaimed 0.0 (unclaimedNullSystems), space nobody holds, which the corporation
	// reaches out for (frontierSiteBonus).
	Frontier bool
	// HoldsSov: the corporation's alliance holds sov somewhere. Border is gate jumps from the
	// nearest system it holds (0 = held), -1 when farther than frontierBorderJumps+1 or none.
	HoldsSov bool
	Border   int
	// Reaction, Tier and Harvest are a reactions site's case: the reaction the moon supports, how
	// far up the T2 chain its output sits (reactionTier*), and the inputs the tower would harvest
	// there (reactionSites). Zero for every other purpose.
	Reaction int64
	Tier     int
	Harvest  []int64
}

// siteScore is value / (distance + danger), with low and null scoring higher through what they
// are (richer moons, no charter, no neighbours). An existing tower close by pulls a new one
// toward it, so a corporation's towers cluster. A reactions site is valued like a mining one:
// by what its moon makes an hour. Pure.
func siteScore(c siteCandidate, purpose string) float64 {
	v := c.Value
	if purpose != purposeMining && purpose != purposeReactions {
		v = 1 // a service tower wants a quiet, near, empty moon
		if c.Class >= moonR16 {
			return 0
		}
		if c.Class >= moonR4 {
			v *= 0.7 // a moon worth mining is wasted under a service tower
		}
	}
	if v <= 0 {
		return 0
	}
	dist := float64(max(c.Jumps, 0))
	if c.Cluster >= 0 {
		dist = math.Min(dist, float64(c.Cluster)*0.6)
		if isAmenityPurpose(purpose) {
			// An amenity is measured from the work, not from the office:
			// members fly to it from the towers they already tend.
			dist = float64(c.Cluster)
		}
	}
	if c.Security >= 0.45 {
		if purpose == purposeMining || purpose == purposeReactions {
			v *= 0.85 // a charter on every cycle
		} else {
			v *= 0.6 // a charter on top of fuel, for a tower that earns nothing itself
		}
	}
	if c.AllianceTarget {
		v *= sovTargetSiteBonus // where the alliance means to hold
	}
	jumpCost := 0.35
	earner := purpose == purposeMining || purpose == purposeReactions
	switch {
	case c.Frontier && earner && (!c.HoldsSov || c.AllianceTarget || (c.Border >= 0 && c.Border <= frontierBorderJumps)):
		// Unclaimed 0.0 is what the corporation reaches out for: worth more, and the long haul costs
		// less per jump. Once the alliance holds sov, only the ground beside it (or a target) counts
		// this way, so it fills in around what it holds. A service tower stays a quiet moon near its work.
		v *= frontierSiteBonus
		jumpCost = frontierJumpCost
	case c.Frontier && earner:
		v *= frontierFarBonus // far frontier while the alliance has a border to fill
	case c.HoldsSov && c.Border == 0 && earner:
		jumpCost = frontierJumpCost // held sov is where the corporation lives: its haul is home
	}
	return v / (1 + dist*jumpCost + c.Danger*4)
}

const (
	// frontierSiteBonus is what a mining or reactions moon in unclaimed 0.0 scores over the same
	// moon elsewhere, and frontierJumpCost what each jump from its office costs it (0.35 elsewhere).
	// A frontier moon fifteen jumps out stands level with a lowsec one next door.
	frontierSiteBonus = 2.0
	frontierJumpCost  = 0.12
	// frontierFarBonus is the frontier bonus for unclaimed 0.0 more than frontierBorderJumps from an
	// alliance's held sov, once it holds any.
	frontierFarBonus = 1.2
)

// softmaxPick draws an index by exp(score/temp) with a draw u in [0,1). Pure.
func softmaxPick(scores []float64, temp, u float64) int {
	if len(scores) == 0 {
		return -1
	}
	if temp <= 0 {
		temp = 1
	}
	maxS := math.Inf(-1)
	for _, s := range scores {
		maxS = math.Max(maxS, s)
	}
	total := 0.0
	w := make([]float64, len(scores))
	for i, s := range scores {
		w[i] = math.Exp((s - maxS) / temp)
		total += w[i]
	}
	if total <= 0 {
		return 0
	}
	x := u * total
	for i := range w {
		x -= w[i]
		if x <= 0 {
			return i
		}
	}
	return len(w) - 1
}
