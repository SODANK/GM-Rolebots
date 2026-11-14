package rolebots

import (
	mrand "math/rand"
)

// What a Capsuleer arrives knowing. A tier is a sheet resolved per (archetype,
// tier, race), covering ships as well as skills:
//
//	Green     the character creator's package and nothing else.
//	Seasoned  racial frigate IV, cruiser II, medium weapons, engineering and
//	          navigation; traders and industrialists get hauler and trade skills.
//	Veteran   frigate line maxed, cruisers grounded, battleship I, leadership.
//
// Names are checked against invTypes (categoryID = 16); applyMaturity skips a miss.

// skillGrant is one line of a sheet: a skill by name and the level it is
// trained to. Levels are a floor: applyMaturity never lowers what the
// character creator already granted.
type skillGrant struct {
	Name  string
	Level int
}

// racialShip names the racial ship-command skill for a hull class:
// racialShip(4, "Cruiser") is "Amarr Cruiser". Race IDs are chrRaces: 1 Caldari,
// 2 Minmatar, 4 Amarr, 8 Gallente. An unknown race returns "", which buildSheet
// drops rather than grant the wrong empire's hull.
func racialShip(raceID int64, class string) string {
	switch raceID {
	case 1:
		return "Caldari " + class
	case 2:
		return "Minmatar " + class
	case 4:
		return "Amarr " + class
	case 8:
		return "Gallente " + class
	case 16:
		// Race 16 (Jove) has no hull line to buy: chargen gives it the Caldari Frigate
		// skill, so a generated Jove flies Caldari hulls.
		return "Caldari " + class
	}
	return ""
}

// turretFamily is the weapon line an empire's own hulls are bonused for, which
// is the line its pilots learn: lasers for the Amarr, projectiles for the
// Minmatar, hybrids for the Caldari and Gallente. It names the middle word of
// "Small/Medium/Large <Fam> Turret".
func turretFamily(raceID int64) string {
	switch raceID {
	case 2:
		return "Projectile"
	case 4:
		return "Energy"
	case 1, 8, 16:
		// Race 16 rides with the Caldari: its chargen hull skill is Caldari Frigate
		// (racialShip), so hybrids suit its boats.
		return "Hybrid"
	}
	return "Hybrid"
}

// missileRace reports whether this empire's pilots learn the missile line
// alongside their turrets. Caldari only: other empires' hulls are turret boats.
func missileRace(raceID int64) bool { return raceID == 1 }

// combatSheet is true for the archetypes that earn with guns. Miners are in
// here because a belt is where the rats are.
func combatSheet(key string) bool {
	return key == "combat" || key == "miner" || key == "outlaw"
}

// buildSheet is the whole tier system. It returns the grants for one
// Capsuleer; an empty slice means the character creator's package stands as-is.
func buildSheet(a archetype, mt maturity, raceID int64, rng *mrand.Rand) []skillGrant {
	if !mt.Trained {
		return nil
	}
	var out []skillGrant
	add := func(name string, level int) {
		if name == "" || level <= 0 {
			return
		}
		out = append(out, skillGrant{name, level})
	}
	fam := turretFamily(raceID)
	guns := combatSheet(a.Key)

	switch mt.Key {

	case "seasoned":
		// Hulls: everyone can fly its own empire's frigate well; gunners step up to a
		// cruiser and haulers into an industrial.
		if guns {
			add(racialShip(raceID, "Frigate"), 4)
			add(racialShip(raceID, "Cruiser"), 2)
		} else {
			add(racialShip(raceID, "Frigate"), 3)
			add(racialShip(raceID, "Industrial"), 2)
		}

		if guns {
			// Small guns properly, medium guns just started (arriving at cruisers).
			add("Small "+fam+" Turret", 3)
			add("Medium "+fam+" Turret", 2)
			add("Gunnery", 3)
			add("Rapid Firing", 2)
			add("Motion Prediction", 2)
			add("Sharpshooter", 2)
			add("Weapon Upgrades", 2)
			if missileRace(raceID) {
				add("Missile Launcher Operation", 3)
				add("Standard Missiles", 2)
			}
			// Engineering and navigation carry the cruiser.
			add("Engineering", 3)
			add("Energy Systems Operation", 2)
			add("Energy Management", 2)
			add("Shield Operation", 3)
			add("Mechanic", 3)
			add("Hull Upgrades", 2)
			add("Repair Systems", 2)
			add("Navigation", 3)
			add("Afterburner", 2)
			add("Evasive Maneuvering", 2)
			add("Warp Drive Operation", 2)
			add("Electronics", 3)
		} else {
			// Modest, not absent: a hauler still has to survive a gate.
			add("Engineering", 2)
			add("Electronics", 2)
			add("Mechanic", 2)
			add("Navigation", 2)
			add("Warp Drive Operation", 2)
		}
		add("Spaceship Command", 3)
		add("Learning", 3)

		switch a.Key {
		case "miner":
			add(racialShip(raceID, "Industrial"), 1) // miners haul ore
			add("Mining", 4)
			add("Astrogeology", 2)
			add("Refining", 3)
			add("Refinery Efficiency", 2)
			add("Industry", 3)
			add("Science", 2)
			add("Mining Upgrades", 1)
		case "industrialist":
			add("Industry", 4)
			add("Production Efficiency", 3)
			add("Mass Production", 2)
			add("Science", 3)
			add("Research", 2)
			add("Metallurgy", 2)
			add("Refining", 3)
			add("Refinery Efficiency", 2)
		case "researcher":
			// The laboratory levers, not the factory line. Laboratory Operation and the lab slot
			// skills come from the planner once the career bar is cleared (skills.go).
			add("Science", 4)
			add("Research", 3)
			add("Metallurgy", 2)
			add("Industry", 2)
		case "trader":
			add("Trade", 4)
			add("Retail", 3)
			add("Marketing", 2)
			add("Accounting", 2)
			add("Broker Relations", 2)
			add("Daytrading", 1)
			add("Procurement", 1)
			add("Social", 3)
			add("Negotiation", 2)
			add("Connections", 2)
		case "hauler":
			// Freight: the hull and the skills that keep it alive on a gate.
			add("Evasive Maneuvering", 2)
			add("Hull Upgrades", 2)
			add("Shield Operation", 2)
			add("Trade", 2)
			add("Social", 2)
			add("Negotiation", 1)
			add("Connections", 1)
		}

	case "veteran":
		// The frigate line is finished, the cruiser line is real, and the battleship
		// skill sits at I: a hull it is qualified for but cannot yet fly well.
		add(racialShip(raceID, "Frigate"), 5)
		add(racialShip(raceID, "Cruiser"), 3)
		add(racialShip(raceID, "Battleship"), 1)
		if !guns {
			add(racialShip(raceID, "Industrial"), 4)
		}

		if guns {
			add("Small "+fam+" Turret", 5)
			add("Medium "+fam+" Turret", 3)
			add("Large "+fam+" Turret", 1)
			add("Gunnery", 5)
			add("Rapid Firing", 4)
			add("Motion Prediction", 4)
			add("Sharpshooter", 4)
			add("Weapon Upgrades", 3)
			add("Controlled Bursts", 3)
			add("Surgical Strike", 2)
			add("Trajectory Analysis", 2)
			if missileRace(raceID) {
				add("Missile Launcher Operation", 4)
				add("Standard Missiles", 4)
				add("Heavy Missiles", 2)
			}
		}

		// The support line every veteran has, gunner or not.
		add("Engineering", 4)
		add("Energy Management", 3)
		add("Energy Systems Operation", 3)
		add("Shield Operation", 4)
		add("Shield Management", 3)
		add("Mechanic", 4)
		add("Hull Upgrades", 3)
		add("Repair Systems", 3)
		add("Electronics", 4)
		add("Navigation", 4)
		add("Afterburner", 3)
		add("Evasive Maneuvering", 3)
		add("Warp Drive Operation", 3)
		add("Spaceship Command", 4)
		add("Learning", 4)

		// Leadership separates a veteran from a well-trained rookie. One warfare
		// specialisation is rolled so a roster does not all take the same one.
		add("Leadership", 3)
		warfare := []string{
			"Wing Command", "Siege Warfare", "Skirmish Warfare",
			"Armored Warfare", "Information Warfare",
		}
		add(warfare[rng.Intn(len(warfare))], 1)

		// The trade and industry lines a step above Seasoned. A veteran trader
		// is not a veteran because it can shoot.
		switch a.Key {
		case "miner":
			add("Mining", 5)
			add("Astrogeology", 4)
			add("Refining", 4)
			add("Refinery Efficiency", 3)
			add("Industry", 4)
			add("Science", 3)
			add("Mining Upgrades", 2)
		case "industrialist":
			add("Industry", 5)
			add("Production Efficiency", 4)
			add("Mass Production", 3)
			add("Science", 4)
			add("Research", 3)
			add("Metallurgy", 3)
			add("Refining", 4)
			add("Refinery Efficiency", 3)
		case "researcher":
			add("Science", 5)
			add("Research", 4)
			add("Metallurgy", 3)
			add("Industry", 3)
		case "trader":
			add("Trade", 5)
			add("Retail", 4)
			add("Marketing", 3)
			add("Accounting", 3)
			add("Broker Relations", 3)
			add("Daytrading", 2)
			add("Procurement", 2)
			add("Wholesale", 1)
			add("Social", 4)
			add("Negotiation", 3)
			add("Connections", 3)
		case "hauler":
			add("Trade", 3)
			add("Social", 3)
			add("Negotiation", 2)
			add("Connections", 2)
		}
	}

	return out
}
