package rolebots

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Module tags: Style (the engagement range a module serves) and Size (the
// hull size it was built for), derived when the catalogue loads and never
// stored. Style is Brawl (wants the ship close), Kite (far), Logi (mends
// somebody else) or Neutral. Shared groups are split by attribute (beam vs
// pulse, scrambler vs disruptor, AB vs MWD, group 78), never by name or
// type-ID list. Size comes from chargeSize (turrets), the group (launchers),
// mass addition (prop modules) or the size word in the module name; a module
// with no size word is placed by its powergrid against its group's bands.

type modStyle uint8

const (
	styleNeutral modStyle = iota
	styleBrawl
	styleKite
	styleLogi
)

func (s modStyle) String() string {
	switch s {
	case styleBrawl:
		return "brawl"
	case styleKite:
		return "kite"
	case styleLogi:
		return "logi"
	}
	return "neutral"
}

type modSize uint8

const (
	sizeNone modSize = iota
	sizeSmall
	sizeMedium
	sizeLarge
	sizeCapital
)

func (s modSize) String() string {
	switch s {
	case sizeSmall:
		return "small"
	case sizeMedium:
		return "medium"
	case sizeLarge:
		return "large"
	case sizeCapital:
		return "capital"
	}
	return ""
}

// Attributes the tags read beyond the ones the scorer already names.
const (
	attrStructHPMult      = 150 // multiplier on structure HP (reinforced bulkheads)
	attrTrackingSpeedMult = 244 // tracking disruptor / tracking link
	attrPowerTransferAmt  = 90  // energy transfer array amount
	attrPowerTransferRng  = 91  // energy transfer array range
	attrSpeedBoostFactor  = 567 // prop module thrust
)

// The module groups the tags name that fitting.go does not.
const (
	grpEnergyVampire      = 68
	grpEnergyNeutralizer  = 71
	grpSmartBomb          = 72
	grpCapBooster         = 76
	grpECMBurst           = 80
	grpTrackingLink       = 209
	grpSignalAmp          = 210
	grpTrackingEnhancer   = 211
	grpSensorBooster      = 212
	grpTrackingComputer   = 213
	grpRemoteSensorBooster = 290
	grpCloak              = 330
	grpDefenderLauncher   = 512
	grpRemoteHullRepairer = 585
	grpCruiseLauncher     = 506
)

// moduleTagKnee is where a gun or a point stops being a close-range module.
// The same 15 km reachWorthKnee is measured against: the edge of the belt
// population's own guns.
const moduleTagKnee = reachWorthKnee

// tagModules stamps Style and Size onto every module of one catalogue. The
// turret split is measured over the catalogue it is handed, so the buyable
// catalogue copies its tags from the static all-modules one (buyableModules).
func tagModules(list []*marketModule) {
	cuts := turretStyleCuts(list)
	sizeBands := groupSizeBands(list)
	for _, mm := range list {
		mm.Style = moduleStyleOf(mm, cuts)
		mm.Size = moduleSizeOf(mm, sizeBands)
	}
}

// turretBucket is one (turret group, chargeSize) family: its guns share
// ammunition and target size, so its two turret lines differ only in
// engagement range.
type turretBucket struct {
	group int64
	size  int
}

// turretStyleCuts is, per turret bucket, the optimal range that splits the
// close line from the long one: the log-space midpoint of the largest gap
// between consecutive distinct optimals (pulse vs beam, blaster vs railgun).
// A bucket with under two distinct optimals is left out; moduleStyleOf uses the knee.
func turretStyleCuts(list []*marketModule) map[turretBucket]float64 {
	opts := map[turretBucket][]float64{}
	for _, mm := range list {
		if !mm.Turret || isMiningModule(mm.GroupID) || !isTurretGroup(mm.GroupID) {
			continue
		}
		if o := mm.Attrs[attrMaxRange]; o > 0 {
			b := turretBucket{mm.GroupID, mm.ChargeSize}
			opts[b] = append(opts[b], o)
		}
	}
	out := map[turretBucket]float64{}
	for b, vs := range opts {
		if cut, ok := largestLogGapCut(vs); ok {
			out[b] = cut
		}
	}
	return out
}

// largestLogGapCut is the log-space split point of the largest gap between
// consecutive distinct values, among cuts that leave at least a quarter of the
// values on each side. The quarter stops a few outliers (the civilian lasers)
// from forming a line of their own. Pure.
func largestLogGapCut(vs []float64) (float64, bool) {
	uniq := make([]float64, 0, len(vs))
	seen := map[float64]bool{}
	for _, v := range vs {
		if v > 0 && !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	if len(uniq) < 2 {
		return 0, false
	}
	sort.Float64s(uniq)
	n := len(uniq)
	edge := max(n/4, 1)
	best, at := -1.0, 0
	for i := edge; i <= n-edge; i++ {
		if g := math.Log(uniq[i]) - math.Log(uniq[i-1]); g > best {
			best, at = g, i
		}
	}
	if at == 0 {
		return 0, false
	}
	return math.Sqrt(uniq[at] * uniq[at-1]), true
}

// moduleStyleOf is one module's engagement style. See the file header for
// what each tag means; every arm says what it reads.
func moduleStyleOf(mm *marketModule, cuts map[turretBucket]float64) modStyle {
	if mm == nil {
		return styleNeutral
	}
	g := mm.GroupID
	switch {
	case mm.Turret && isTurretGroup(g):
		opt := mm.Attrs[attrMaxRange]
		if opt <= 0 {
			return styleNeutral
		}
		if cut, ok := cuts[turretBucket{g, mm.ChargeSize}]; ok {
			if opt >= cut {
				return styleKite
			}
			return styleBrawl
		}
		// A family too thin to measure: reach against a knee scaled to the gun's size.
		knee := moduleTagKnee / 2 * float64(max(mm.ChargeSize, 1))
		if weaponReachM(mm) >= knee {
			return styleKite
		}
		return styleBrawl
	case mm.Launcher:
		switch g {
		case grpRocketLauncher:
			return styleBrawl
		case grpStandardLauncher, grpHeavyLauncher, grpCruiseLauncher:
			return styleKite
		case grpDefenderLauncher:
			return styleNeutral
		}
		// Assault and siege launchers, and anything else: the round's reach decides
		// (light missiles at 18.75 km and torpedoes at 37.5 km both land Kite).
		if mm.ChargeReachM <= 0 {
			return styleNeutral
		}
		if mm.ChargeReachM >= moduleTagKnee {
			return styleKite
		}
		return styleBrawl
	}
	switch g {
	case grpWarpScrambler:
		// Judged by range, not strength: the strength attribute does not separate the
		// plain, Fusion/Ion and disruptor lines, but the distance they work at does.
		if mm.Attrs[attrTackleRange] >= moduleTagKnee {
			return styleKite
		}
		return styleBrawl
	case grpStasisWeb:
		return styleBrawl
	case grpAfterburner:
		if isMicrowarpdrive(mm) {
			return styleKite
		}
		return styleBrawl
	case grpCargoExpander:
		// Group 78 holds five families, told apart by what they change (attribute 616
		// is 1.0 on all of them).
		switch {
		case mm.Attrs[attrCargoMult] > 1:
			return styleNeutral // expanded cargohold
		case mm.Attrs[attrStructHPMult] > 1:
			return styleBrawl // reinforced bulkheads
		case mm.Attrs[attrSpeedBonusPct] > 0, mm.Attrs[attrAgilityPct] < 0:
			return styleKite // overdrive, nanofiber, inertia stabiliser
		}
		return styleNeutral
	case grpArmorReinforcer:
		// The plate's mass addition slows a hull under a prop module (PropVelocity,
		// fitstats.go), so it is a brawler's buffer.
		return styleBrawl
	case grpSensorDamper, grpTrackingDisruptor, grpTargetPainter:
		return styleKite
	case grpSensorBooster, grpTrackingComputer, grpTrackingEnhancer, grpSignalAmp:
		return styleKite
	case grpEnergyNeutralizer, grpEnergyVampire, grpSmartBomb, grpECMBurst:
		return styleBrawl
	case grpShieldTransporter, grpArmorRepProjector, grpEnergyTransfer,
		grpRemoteSensorBooster, grpTrackingLink, grpRemoteHullRepairer:
		return styleLogi
	}
	return styleNeutral
}

// sizeWordRE matches the size words the classic catalogue names modules
// with. Whole words only, so "Largely" or a pilot's name cannot match.
var sizeWordRE = regexp.MustCompile(`(?i)\b(micro|small|medium|large|x-large|xlarge|heavy|capital)\b`)

// plateMMRE is an armour plate's thickness, which is how plates say size.
var plateMMRE = regexp.MustCompile(`\b(\d+)mm\b`)

// sizeFromName reads a size word out of a module's name, sizeNone when there
// is none. "Micro" is a small module (it fits the same hulls), "X-Large" a
// large one, "Heavy" the large band for neutralisers, vampires and cap
// boosters, which name their battleship size that way.
func sizeFromName(name string) modSize {
	if m := sizeWordRE.FindAllString(name, -1); len(m) > 0 {
		// The last size word wins, so a stray adjective earlier in a faction name
		// does not set the size.
		switch strings.ToLower(m[len(m)-1]) {
		case "micro", "small":
			return sizeSmall
		case "medium":
			return sizeMedium
		case "large", "x-large", "xlarge", "heavy":
			return sizeLarge
		case "capital":
			return sizeCapital
		}
	}
	return sizeNone
}

// plateSize is an armour plate's size from its thickness: 50-200 mm are
// frigate plates, 400 mm the cruiser plate, 800 mm and up battleship.
func plateSize(name string) modSize {
	m := plateMMRE.FindStringSubmatch(name)
	if len(m) < 2 {
		return sizeNone
	}
	mm, err := strconv.Atoi(m[1])
	if err != nil || mm <= 0 {
		return sizeNone
	}
	switch {
	case mm <= 200:
		return sizeSmall
	case mm <= 400:
		return sizeMedium
	}
	return sizeLarge
}

// groupSizeBands is, per group, the geometric-mean powergrid of the members
// whose size the name states: what an unnamed module of that group is placed
// against.
func groupSizeBands(list []*marketModule) map[int64]map[modSize]float64 {
	type acc struct {
		logSum float64
		n      int
	}
	tmp := map[int64]map[modSize]*acc{}
	for _, mm := range list {
		if mm.Turret || mm.Launcher || mm.PG <= 0 {
			continue
		}
		s := sizeFromName(mm.Name)
		if mm.GroupID == grpArmorReinforcer {
			s = plateSize(mm.Name)
		}
		if s == sizeNone {
			continue
		}
		if tmp[mm.GroupID] == nil {
			tmp[mm.GroupID] = map[modSize]*acc{}
		}
		a := tmp[mm.GroupID][s]
		if a == nil {
			a = &acc{}
			tmp[mm.GroupID][s] = a
		}
		a.logSum += math.Log(mm.PG)
		a.n++
	}
	out := map[int64]map[modSize]float64{}
	for g, bands := range tmp {
		if len(bands) < 2 {
			continue // one band is no scale to place anything against
		}
		out[g] = map[modSize]float64{}
		for s, a := range bands {
			out[g][s] = math.Exp(a.logSum / float64(a.n))
		}
	}
	return out
}

// moduleSizeOf is one module's size band.
func moduleSizeOf(mm *marketModule, bands map[int64]map[modSize]float64) modSize {
	if mm == nil {
		return sizeNone
	}
	if mm.Turret && mm.ChargeSize > 0 {
		return modSize(min(mm.ChargeSize, int(sizeCapital)))
	}
	if mm.Launcher {
		if b := launcherBandOf(mm.GroupID); b > 0 {
			return modSize(b)
		}
		return sizeNone
	}
	if mm.GroupID == grpAfterburner {
		if c := propMassClass(mm); c > 0 {
			return modSize(min(c, int(sizeCapital)))
		}
		return sizeNone
	}
	if mm.GroupID == grpArmorReinforcer {
		if s := plateSize(mm.Name); s != sizeNone {
			return s
		}
	}
	if s := sizeFromName(mm.Name); s != sizeNone {
		return s
	}
	// Nearest band by powergrid in log space. The bands sit an order of magnitude
	// apart, so a named module never straddles two.
	gb := bands[mm.GroupID]
	if len(gb) == 0 || mm.PG <= 0 {
		return sizeNone
	}
	best, bestD := sizeNone, math.Inf(1)
	for s, pg := range gb {
		if d := math.Abs(math.Log(mm.PG) - math.Log(pg)); d < bestD {
			best, bestD = s, d
		}
	}
	return best
}

// hullModSize is the module size a hull was built to carry.
func hullModSize(c HullClass) modSize {
	if c == ClassCapital {
		return sizeCapital
	}
	return modSize(hullPropClass(c))
}

// moduleSizeFactor is the generic size penalty: doctrineOffSize per size step
// below the hull's own, doctrineOverSize per step above. Oversize is a
// penalty, not a refusal; powergrid still refuses what a hull cannot run.
// Unsized modules and unclassified hulls take 1.
func moduleSizeFactor(h *HullProfile, mm *marketModule) float64 {
	if h == nil || mm == nil || mm.Size == sizeNone {
		return 1
	}
	want := hullModSize(h.Class)
	switch {
	case want == sizeNone || mm.Size == want:
		return 1
	case mm.Size > want:
		return math.Pow(doctrineOverSize, float64(mm.Size-want))
	}
	return math.Pow(doctrineOffSize, float64(want-mm.Size))
}

// isEwarGroup reports whether a module group is one the EWAR seat flies:
// jammer, damp, tracking disruptor, painter. Neuts, vampires, smartbombs and
// ECM bursts are tagged (Brawl) but never fitted.
func isEwarGroup(groupID int64) bool {
	_, ok := ewarSpecialtyByGroup[groupID]
	return ok
}
