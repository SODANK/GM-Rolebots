package rolebots

// Fleet doctrine: what a corporation's fleet looks like. A FleetDoctrine is a
// corporation's template for one kind of fleet as a list of seat specs plus fleet-wide
// choices: one tank layer (a logi on the wrong layer mends nobody), one range band
// (combatStance's three values) and a hull band read off the roster and treasury
// (fleetdoctrine_derive.go).
//
// This file is the pure half: the types, the catalogue of kinds and Slots(), which turns a
// doctrine into the slot list gangRecruitFrom fills and replaces gangComposition. Every
// prefix of that list keeps the ratios, because gangRecruitFrom fills seats in list order.
// Design: docs/FLEET_DOCTRINE.md section 1.

// doctrineKind is one kind of fleet a corporation may field.
type doctrineKind string

const (
	// kindBrawl is the default line for every race: short guns, webs on the
	// tackle, plates or extenders, a logi behind every third gun.
	kindBrawl doctrineKind = "brawl"
	// kindSniper holds at optimal with artillery, rails or beams. The only
	// line that fights a tower's guns from outside their reach.
	kindSniper doctrineKind = "sniper"
	// kindKite holds a range it can keep: fast hulls, long tackle, light
	// logi. Offered only when the roster's hulls are small enough to be
	// genuinely faster than what they fight.
	kindKite doctrineKind = "kite"
	// kindSkirmish is the PvP pack: frigates and destroyers, tackle-heavy, no logi.
	kindSkirmish doctrineKind = "skirmish"
	// kindSiege is structure bashing: battlecruisers and battleships, heavy logi, almost
	// no tackle (the tower does not run).
	kindSiege doctrineKind = "siege"
	// kindGuard is the Defence doctrine's belt cover and the mining escort:
	// whatever is owned, a tackler and a logi per four guns.
	kindGuard doctrineKind = "guard"
	// kindGank is the high-sec suicide gank (gank.go): a destroyer line spent on firing
	// (CONCORD ends every striker) behind an unarmed scout that prices the mark and loots
	// the wrecks. Predation only.
	kindGank doctrineKind = "gank"
)

// doctrineKinds in presentation order.
var doctrineKinds = []doctrineKind{kindBrawl, kindSniper, kindKite, kindSkirmish, kindSiege, kindGuard, kindGank}

func (k doctrineKind) Label() string {
	if d, ok := kindBySlug(k); ok {
		return d.Label
	}
	return string(k)
}

// seatSpec is one seat of a doctrine: what fills it and what it flies.
type seatSpec struct {
	Role string // RoleTackler | RoleLogi | RoleEwar | "" for the line's guns
	Job  string // the fit lens (fitjob.go); "" for the guns, where the activity decides
	// HullMin..HullMax is the hull band the seat flies in.
	HullMin, HullMax HullClass
	// Tank is the seat's layer, always the fleet's: a logi on another layer mends nobody.
	Tank TankPref
	// Style is the seat's range band: the line's for guns and logi, kite for
	// a kite fleet's long tackle, hold for electronic warfare.
	Style combatStance
	// Fixed seats come first; then one more for every Per gun seats (the
	// leader counted among the guns). Per 0 means no ratio seats.
	Fixed, Per int
	// Priority is the fill order and the logi's rep order: lower first.
	Priority int
	// Disposable marks a seat whose hull the op spends (a gank striker). Its fits rank
	// cheapest-first (fitselect.go rankDisposable) and its kit is handed over outright
	// (corparmory.go kitFit). An older row without the tag reads false.
	Disposable bool
}

// FleetDoctrine is one corporation's template for one kind of fleet.
type FleetDoctrine struct {
	CorpID      int64
	Kind        doctrineKind
	Tank        TankPref
	Style       combatStance
	HullMin     HullClass
	HullMax     HullClass
	CostCeiling float64 // ISK per hull the corporation will replace; 0 = unbounded
	Seats       []seatSpec
	DerivedAt   int64
	// Override marks a row an operator pinned (ESP); derivation leaves it
	// alone.
	Override bool
}

// kindDef is one row of the catalogue: what a kind's line looks like and who
// may field it.
type kindDef struct {
	Kind  doctrineKind
	Label string
	Style combatStance
	// One specialist per N gun seats; 0 means the kind seats none.
	TacklePer, LogiPer, EwarPer int
	// TackleLong marks disruptor-range tackle (the kite and sniper fleets').
	TackleLong bool
	// Doctrines that may field this kind (corpdoctrine.go's ROE classes).
	Doctrines []Doctrine
	// MinBand is the smallest hull class the line makes sense in (siege wants
	// battlecruisers); MaxBand the largest (a skirmish pack is frigates and
	// destroyers, a kite wing nothing heavier than a cruiser). 0 = no bound.
	MinBand, MaxBand HullClass
	// Scout seats one fixed unarmed scout (RoleScout / JobScout) ahead of the
	// line; Disposable marks the line's gun seats as spent by the op
	// (seatSpec.Disposable). Both are the gank kind's.
	Scout, Disposable bool
}

// kindTable is the catalogue. Order is presentation only; a kind is picked
// per fleet order by kindForOrder (fleetdoctrine_derive.go).
var kindTable = []kindDef{
	{Kind: kindBrawl, Label: "brawl line", Style: stanceBrawl,
		TacklePer: 4, LogiPer: 3, EwarPer: 6,
		Doctrines: []Doctrine{DoctrineDefence, DoctrineSecurity, DoctrineMercenary, DoctrinePredation}},
	{Kind: kindSniper, Label: "sniper line", Style: stanceHold,
		TacklePer: 5, LogiPer: 4, EwarPer: 6, TackleLong: true,
		Doctrines: []Doctrine{DoctrineSecurity, DoctrineMercenary, DoctrinePredation}},
	{Kind: kindKite, Label: "kite wing", Style: stanceKite,
		TacklePer: 3, LogiPer: 5, EwarPer: 6, TackleLong: true,
		Doctrines: []Doctrine{DoctrineMercenary, DoctrinePredation},
		MaxBand:   ClassCruiser},
	{Kind: kindSkirmish, Label: "skirmish pack", Style: stanceBrawl,
		TacklePer: 2,
		Doctrines: []Doctrine{DoctrineSecurity, DoctrineMercenary, DoctrinePredation},
		MaxBand:   ClassDestroyer},
	{Kind: kindSiege, Label: "siege line", Style: stanceHold,
		TacklePer: 8, LogiPer: 2, EwarPer: 6,
		Doctrines: []Doctrine{DoctrineSecurity, DoctrineMercenary, DoctrinePredation},
		MinBand:   ClassBattlecruiser},
	{Kind: kindGuard, Label: "guard", Style: stanceBrawl,
		TacklePer: 4, LogiPer: 4,
		Doctrines: []Doctrine{DoctrineDefence, DoctrineSecurity, DoctrineMercenary, DoctrinePredation}},
	// The gank: destroyers only, no tackle (the window is the CONCORD clock), no logi, no
	// EWAR; one scout, and every gun seat disposable.
	{Kind: kindGank, Label: "gank fleet", Style: stanceBrawl,
		Doctrines: []Doctrine{DoctrinePredation},
		MinBand:   ClassDestroyer, MaxBand: ClassDestroyer,
		Scout:     true, Disposable: true},
}

func kindBySlug(k doctrineKind) (kindDef, bool) {
	for _, d := range kindTable {
		if d.Kind == k {
			return d, true
		}
	}
	return kindDef{}, false
}

// allows reports whether a corporation of this ROE doctrine may field the kind.
func (d kindDef) allows(roe Doctrine) bool {
	for _, x := range d.Doctrines {
		if x == roe {
			return true
		}
	}
	return false
}

// specFor is the doctrine's seat spec for a role; the gun spec for "" and
// for any role the doctrine does not seat.
func (d FleetDoctrine) specFor(role string) seatSpec {
	var gun seatSpec
	for _, s := range d.Seats {
		if s.Role == role {
			return s
		}
		if s.Role == "" {
			gun = s
		}
	}
	return gun
}

// Slots is the slot list behind the leader for a fleet of the given size, in fill order.
// gunRole/gunJob is the line's seat; can says whether a specialist seat may be offered.
// The leader counts as the first gun, and each step releases the highest-priority
// specialist whose ratio is due (Fixed + guns/Per), else a gun.
func (d *FleetDoctrine) Slots(size int, gunRole, gunJob string, can func(seatSpec) bool) []gangSlot {
	n := size - 1
	if n < 1 || d == nil {
		return nil
	}
	var specialists []seatSpec
	for _, s := range d.Seats {
		if s.Role == "" || (s.Fixed == 0 && s.Per == 0) {
			continue
		}
		if can != nil && !can(s) {
			continue
		}
		specialists = append(specialists, s)
	}
	// Fill order is Priority, ties in table order.
	for i := 1; i < len(specialists); i++ {
		for j := i; j > 0 && specialists[j].Priority < specialists[j-1].Priority; j-- {
			specialists[j], specialists[j-1] = specialists[j-1], specialists[j]
		}
	}
	have := make([]int, len(specialists))
	guns := 1 // the leader
	out := make([]gangSlot, 0, n)
	for len(out) < n {
		emitted := false
		for i, s := range specialists {
			if have[i] < seatWant(s, guns) {
				out = append(out, gangSlot{s.Role, s.Job})
				have[i]++
				emitted = true
				break
			}
		}
		if !emitted {
			out = append(out, gangSlot{gunRole, gunJob})
			guns++
		}
	}
	return out
}

// seatWant is how many of a specialist seat a line of this many guns asks for.
func seatWant(s seatSpec, guns int) int {
	want := s.Fixed
	if s.Per > 0 {
		want += guns / s.Per
	}
	return want
}

// seatsForKind builds a kind's seat specs for a line flying the given band,
// tank and style. Pure; derivation calls it per kind.
func seatsForKind(k kindDef, lo, hi HullClass, tank TankPref) []seatSpec {
	var out []seatSpec
	if k.Scout {
		// The scout is filled first: a gank with no scout cannot price a mark or loot it. It is
		// a frigate or the line's own destroyer, and holds on the gate it scans.
		out = append(out, seatSpec{Role: RoleScout, Job: JobScout,
			HullMin: ClassFrigate, HullMax: ClassDestroyer, Tank: tank, Style: stanceHold, Fixed: 1, Priority: 0})
	}
	if k.LogiPer > 0 {
		out = append(out, seatSpec{Role: RoleLogi, Job: JobLogi,
			HullMin: lo, HullMax: hi, Tank: tank, Style: k.Style, Per: k.LogiPer, Priority: 0})
	}
	if k.TacklePer > 0 {
		// Tackle is fast and cheap to lose: frigates, except in a siege line, which may use a
		// destroyer.
		tLo, tHi := ClassFrigate, ClassFrigate
		if k.Kind == kindSiege {
			tHi = ClassDestroyer
		}
		style := stanceBrawl
		if k.TackleLong {
			style = stanceKite
		}
		out = append(out, seatSpec{Role: RoleTackler, Job: JobTackle,
			HullMin: tLo, HullMax: tHi, Tank: tank, Style: style, Fixed: 1, Per: k.TacklePer, Priority: 1})
	}
	if k.EwarPer > 0 {
		// Electronic warfare holds at its modules' optimal, well outside the
		// line, and is never worth a battleship hull.
		eHi := hi
		if eHi > ClassCruiser {
			eHi = ClassCruiser
		}
		eLo := lo
		if eLo > eHi {
			eLo = eHi
		}
		out = append(out, seatSpec{Role: RoleEwar, Job: JobEwar,
			HullMin: eLo, HullMax: eHi, Tank: tank, Style: stanceHold, Per: k.EwarPer, Priority: 2})
	}
	out = append(out, seatSpec{HullMin: lo, HullMax: hi, Tank: tank, Style: k.Style, Priority: 3, Disposable: k.Disposable})
	return out
}
