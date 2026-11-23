package rolebots

import (
	"strings"

	"gurimoon/corp"
)

// Departments own hangars. The seven corporation hangar divisions are laid out
// by desk, with the war chest borrowing 2-5 where a desk leaves them free.
// Ownership follows the purpose, not the slot, so a request between arms can name
// a division. A slot is never re-flagged (haul legs, requisitions and programs
// persist flags); a borrowed slot changes owner with the borrow:
//   Materials (1), Production (3): Industry; Blueprint Library (2): Research;
//   Company Store (4), Trade (5): Sales; Starbase Kit (6), Fuel (7):
//   Infrastructure; Armory: Security; Fleet Yard: Mining; Spoils: Sales.
// A name carries its owner ([SEC] Armory, [IND] Production). A division a human
// named stays theirs, and its owner is still the slot's.

// deskDivArm is the arm a desk division belongs to, by division number.
var deskDivArm = map[int]corpArm{
	1: armIndustry,
	2: armResearch,
	3: armIndustry,
	4: armSales,
	5: armSales,
	6: armInfrastructure,
	7: armInfrastructure,
}

// warChestArm is the arm a war-chest purpose belongs to.
var warChestArm = map[divPurpose]corpArm{
	purArmory: armSecurity,
	purYard:   armMining,
	purSpoils: armSales,
}

// divisionArm is the arm that owns a division of this corporation: a borrowed
// war-chest purpose first (the first in warChestOrder when several folded into
// one), else the desk's. Pure over the profile.
func divisionArm(p *divProfile, div int) corpArm {
	if p != nil && div > 0 {
		for _, pur := range warChestOrder {
			if p.div[pur] == div {
				return warChestArm[pur]
			}
		}
	}
	return deskDivArm[div]
}

// armDivisions is every division an arm owns in this corporation, ascending.
func armDivisions(p *divProfile, a corpArm) []int {
	var out []int
	for d := 1; d <= 7; d++ {
		if divisionArm(p, d) == a {
			out = append(out, d)
		}
	}
	return out
}

// armHomeDivision is where goods meant for an arm land when the request does
// not say: Security's Armory, Sales' Trade shelf, Research's Library,
// Industry's Materials, Infrastructure's Kit, Mining's Yard. Falls back to
// Materials, where every buy order delivers. Pure over the profile.
func armHomeDivision(p *divProfile, a corpArm) int {
	switch a {
	case armSecurity:
		if d := p.Div(purArmory); d > 0 {
			return d
		}
	case armMining:
		if d := p.Div(purYard); d > 0 {
			return d
		}
	case armSales:
		return 5
	case armResearch:
		return 2
	case armInfrastructure:
		return 6
	}
	return 1
}

// armOwnRoles is the query and take bits a member of an arm holds on the
// divisions its arm owns: R&D works the Blueprint Library, Security draws from
// the Armory. A human's take on a war-chest division is still subject to
// humanGuardedTake (corpdivisions.go).
func armOwnRoles(p *divProfile, a corpArm) uint64 {
	var bits uint64
	for _, d := range armDivisions(p, a) {
		bits |= divQueryBit(d) | divTakeBit(d)
	}
	return bits
}

// armQueryAll is query on every division: what a Factory Manager holds, so
// the S&I window's Output dropdown (it lists only divisions the installer can
// query) offers every hangar a request might name.
const armQueryAll = corp.RoleHangarCanQuery1 | corp.RoleHangarCanQuery2 | corp.RoleHangarCanQuery3 |
	corp.RoleHangarCanQuery4 | corp.RoleHangarCanQuery5 | corp.RoleHangarCanQuery6 | corp.RoleHangarCanQuery7

// taggedDivName is a division's module name with its owner's tag:
// "[SEC] Armory". ASCII, well inside the 50-character column.
func taggedDivName(a corpArm, name string) string {
	if name == "" {
		return ""
	}
	if t := armTag(a); t != "" {
		return "[" + t + "] " + name
	}
	return name
}

// untagDivName strips a leading "[XYZ] " owner tag, so a tagged module name
// still reads as the module's.
func untagDivName(name string) string {
	if len(name) > 2 && name[0] == '[' {
		if i := strings.Index(name, "] "); i > 1 && i <= 4 {
			return name[i+2:]
		}
	}
	return name
}

// divDisplayName is how a division is called on a page or in a letter: its
// stored name when it has one, else its owner's tag and number.
func divDisplayName(p *divProfile, div int, stored string) string {
	if stored != "" {
		return stored
	}
	if t := armTag(divisionArm(p, div)); t != "" {
		return "[" + t + "] Division " + itoa(div)
	}
	return "Division " + itoa(div)
}
