package rolebots

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
)

// Corporation title vocabularies: what a position is called in one corporation
// (corptitles.go decides who holds it). Every name is looked up by a key (slot kinds,
// department heads, seats, and one key per rung of each arm's ladder and of the merit
// ladder). Names come from the corporation's kind (titleKindOf, read off its tags), its
// race (corporation.raceID, else its CEO's) and a roll fixed by the corporation's ID that
// picks among the remaining synonyms. A pool is matched most-specific-first: (kind, race),
// (kind, any), (any, race), (any, any). A ladder is picked whole, one roll per arm, and
// names are unique within a corporation. The operator's overrides
// (rolebot_title_overrides) sit on top, one per key.

// ── keys ────────────────────────────────────────────────────────────────────

const (
	tkHeadSecurity = "head.security"
	tkHeadMining   = "head.mining"
	tkHeadSales    = "head.sales"
	tkHeadHR       = "head.hr"
	tkHeadInfra    = "head.infrastructure"

	tkSeatFueler   = "seat.fueler"
	tkSeatProcurer = "seat.procurer"
	tkSeatAdmin    = "seat.admin"
	tkSeatSales    = "seat.sales"

	tkMeritLadder = "merit"

	titleRungs = 6

	// titleNameMax is the longest name the module writes or accepts. The
	// columns allow more (character_.title 85, crpTitles 100); the member
	// list and the bracket do not.
	titleNameMax = 48
)

// titleSlotKeys are the slot kinds, which double as their keys.
var titleSlotKeys = []string{titleKindCEO, titleKindDirector, titleKindPersonnel,
	titleKindTreasurer, titleKindCombat, titleKindMining, titleKindFactory}

var titleHeadKeys = []string{tkHeadSecurity, tkHeadMining, tkHeadSales, tkHeadHR, tkHeadInfra}

var titleSeatKeys = []string{tkSeatFueler, tkSeatProcurer, tkSeatAdmin, tkSeatSales}

// titleLadderArms is every arm that has a ladder, in resolution order.
var titleLadderArms = []corpArm{armSecurity, armMining, armIndustry, armResearch, armSales, armInfrastructure, armHR, armFinance}

func rankLadderKey(a corpArm) string { return "rank." + string(a) }

func rankTitleKey(a corpArm, r int) string {
	return fmt.Sprintf("rank.%s.%d", a, clampRank(r))
}

func meritTitleKey(r int) string { return fmt.Sprintf("merit.%d", clampRank(r)) }

// headKeyOfPortfolio is a department head's key.
func headKeyOfPortfolio(port string) string {
	switch armOfPortfolio(port) {
	case armSecurity:
		return tkHeadSecurity
	case armMining:
		return tkHeadMining
	case armSales:
		return tkHeadSales
	case armHR:
		return tkHeadHR
	case armInfrastructure:
		return tkHeadInfra
	}
	return ""
}

// seatTitleKey is a POS or industry seat's key.
func seatTitleKey(seat string) string {
	switch seat {
	case seatFueler:
		return tkSeatFueler
	case seatProcurer:
		return tkSeatProcurer
	case seatAdmin:
		return tkSeatAdmin
	case seatSales:
		return tkSeatSales
	}
	return ""
}

// allTitleKeys is every key in resolution order: slots, ladders, heads, seats. Ladders
// come before heads and seats because they carry the most members: a head that collides
// with a rung moves, not the ladder.
func allTitleKeys() []string {
	out := append([]string{}, titleSlotKeys...)
	for _, a := range titleLadderArms {
		for r := range titleRungs {
			out = append(out, rankTitleKey(a, r))
		}
	}
	for r := range titleRungs {
		out = append(out, meritTitleKey(r))
	}
	out = append(out, titleHeadKeys...)
	return append(out, titleSeatKeys...)
}

// isPositionKey: a slot, head or seat, a post somebody was given, which the CEO's ledger
// records. Ranks are the merit pass's news.
func isPositionKey(k string) bool {
	return k != "" && !strings.HasPrefix(k, "rank.") && !strings.HasPrefix(k, "merit.")
}

// titleKeyLabel is how the operator's page names a key.
func titleKeyLabel(k string) string {
	switch k {
	case titleKindCEO:
		return "CEO"
	case titleKindDirector:
		return "Director (no department)"
	case titleKindPersonnel:
		return "Recruitment officer"
	case titleKindTreasurer:
		return "Treasurer"
	case titleKindCombat:
		return "Combat fleet commander"
	case titleKindMining:
		return "Mining fleet commander"
	case titleKindFactory:
		return "Industry director"
	case tkHeadSecurity:
		return "Head of Security"
	case tkHeadMining:
		return "Head of Mining"
	case tkHeadSales:
		return "Head of Sales"
	case tkHeadHR:
		return "Head of Personnel"
	case tkHeadInfra:
		return "Head of Infrastructure"
	case tkSeatFueler:
		return "Starbase fuel seat"
	case tkSeatProcurer:
		return "Starbase procurement seat"
	case tkSeatAdmin:
		return "Factory administrator seat"
	case tkSeatSales:
		return "Sales seat"
	}
	parts := strings.Split(k, ".")
	if len(parts) == 3 && parts[0] == "rank" {
		r, _ := strconv.Atoi(parts[2])
		return fmt.Sprintf("%s rank %d", armLabel(corpArm(parts[1])), r+1)
	}
	if len(parts) == 2 && parts[0] == "merit" {
		r, _ := strconv.Atoi(parts[1])
		return fmt.Sprintf("Member rank %d (no arm)", r+1)
	}
	return k
}

// ── kind and race ───────────────────────────────────────────────────────────

const (
	tvPirate  = "pirate"
	tvCartel  = "cartel"
	tvMerc    = "merc"
	tvNavy    = "navy"
	tvMilitia = "militia"
	tvLab     = "lab"
	tvYard    = "yard"
	tvWorks   = "works"
	tvMine    = "mine"
	tvTrade   = "trade"
	tvHouse   = "house"

	tvAmarr    = "amarr"
	tvCaldari  = "caldari"
	tvGallente = "gallente"
	tvMinmatar = "minmatar"
	tvNeutral  = "neutral"
)

var titleKindLabels = map[string]string{
	tvPirate: "pirate gang", tvCartel: "cartel", tvMerc: "mercenary company",
	tvNavy: "security force", tvMilitia: "militia", tvLab: "laboratory",
	tvYard: "shipyard", tvWorks: "manufacturer", tvMine: "mining concern",
	tvTrade: "trading house", tvHouse: "house",
}

// titleKindOf reads a corporation's kind off its tags. Outlaws are pirates whatever else
// they do; a cartel factory and a hired gun come next; then the corporation's trade
// (shipbuilding wherever it appears, otherwise the first economic tag, the CEO's
// strongest); a security or combat tag decides only an outfit that does nothing else.
func titleKindOf(tags []string) string {
	switch {
	case hasTag(tags, tagOutlaw):
		return tvPirate
	case hasTag(tags, "cartelindustry") || hasTag(tags, "chemical"):
		return tvCartel
	case hasTag(tags, tagMercenary):
		return tvMerc
	case hasTag(tags, "shipbuilding") || hasTag(tags, "construction"):
		return tvYard
	}
	for _, t := range tags {
		switch t {
		case "research", "biotech":
			return tvLab
		case "industry":
			return tvWorks
		case "mining":
			return tvMine
		case "trade", "hauling", "retail":
			return tvTrade
		}
	}
	switch {
	case hasTag(tags, tagSecurity):
		return tvNavy
	case hasTag(tags, tagCombat):
		return tvMilitia
	}
	return tvHouse
}

// titleRaceOf names a raceID's vocabulary.
func titleRaceOf(raceID int64) string {
	switch raceID {
	case raceAmarr:
		return tvAmarr
	case raceCaldari:
		return tvCaldari
	case raceGallente:
		return tvGallente
	case raceMinmatar:
		return tvMinmatar
	}
	return tvNeutral
}

// ── pools ───────────────────────────────────────────────────────────────────

// tvPool is one row of the catalogue: the kinds and races it is for (space
// separated; empty = any) and its candidates.
type tvPool struct {
	kinds, races string
	names        []string
}

type tvLadderPool struct {
	kinds, races string
	ladders      [][titleRungs]string
}

const tvEcon = "mine lab trade yard works"

func tvn(kinds, races string, names ...string) tvPool { return tvPool{kinds, races, names} }

func tvl(kinds, races string, ladders ...[titleRungs]string) tvLadderPool {
	return tvLadderPool{kinds, races, ladders}
}

type tvRungs = [titleRungs]string

var titleNamePools = map[string][]tvPool{
	titleKindCEO: {
		tvn("", "", "Chief Executive Officer", "Proprietor", "Founder"),
		tvn("", tvAmarr, "Holder", "Lord Proprietor", "High Steward"),
		tvn("", tvCaldari, "Chief Executive Officer", "Chief Executive", "Managing Director", "Executive Chairman"),
		tvn("", tvGallente, "President", "Chairman of the Board", "Founding President", "Executive President"),
		tvn("", tvMinmatar, "Chieftain", "Clan Head", "Elder Chief"),
		tvn(tvMine, "", "Mine Owner", "Prospector General", "Claim Holder"),
		tvn(tvMine, tvAmarr, "Lord of the Claim", "Holder of the Mines"),
		tvn(tvMine, tvCaldari, "Chief Extraction Officer", "Managing Director"),
		tvn(tvMine, tvGallente, "Claim President", "Chairman of the Claim"),
		tvn(tvMine, tvMinmatar, "Rock Chief", "Seam Chieftain"),
		tvn(tvYard, "", "Master Shipwright", "Yard Owner"),
		tvn(tvYard, tvAmarr, "Master of the Yards", "Lord Shipwright"),
		tvn(tvYard, tvCaldari, "Yard Director", "Chief Executive Officer"),
		tvn(tvYard, tvGallente, "Yard President", "President"),
		tvn(tvYard, tvMinmatar, "Chief Shipwright", "Hull Chief"),
		tvn(tvLab, "", "Director-General", "Chief Scientist"),
		tvn(tvLab, tvAmarr, "Lord Provost", "High Archivist"),
		tvn(tvLab, tvCaldari, "Chief Scientist", "Institute Director"),
		tvn(tvLab, tvGallente, "Chancellor", "Dean of the Institute"),
		tvn(tvLab, tvMinmatar, "Sage-Chief", "Elder Sage"),
		tvn(tvTrade, "", "Guildmaster", "Trade Magnate"),
		tvn(tvTrade, tvAmarr, "Merchant Lord", "Lord of Coin"),
		tvn(tvTrade, tvCaldari, "Managing Director", "Chief Executive Officer"),
		tvn(tvTrade, tvGallente, "Merchant Prince", "Trade Magnate"),
		tvn(tvTrade, tvMinmatar, "Trade Chief", "Caravan Lord"),
		tvn(tvNavy, "", "Commander", "Commandant"),
		tvn(tvNavy, tvAmarr, "Lord Commander", "Grand Marshal"),
		tvn(tvNavy, tvCaldari, "Commandant", "Chief of Security"),
		tvn(tvNavy, tvGallente, "Commissioner", "Director-General"),
		tvn(tvNavy, tvMinmatar, "High Marshal", "Warden-Chief"),
		tvn(tvMilitia, "", "Commodore", "Admiral"),
		tvn(tvMilitia, tvAmarr, "Lord Commander", "Holder-Commander"),
		tvn(tvMilitia, tvCaldari, "Commodore", "Fleet Director"),
		tvn(tvMilitia, tvGallente, "Admiral", "Commodore"),
		tvn(tvMilitia, tvMinmatar, "War Chief", "Warleader"),
		tvn(tvMerc, "", "Field Marshal", "Colonel"),
		tvn(tvMerc, tvAmarr, "Lord Captain", "Knight-Captain"),
		tvn(tvMerc, tvCaldari, "Colonel", "Commander"),
		tvn(tvMerc, tvGallente, "Managing Partner", "Principal"),
		tvn(tvMerc, tvMinmatar, "Warleader", "Free Captain"),
		tvn(tvPirate, "", "Pirate Lord", "Captain"),
		tvn(tvPirate, tvAmarr, "Blood Lord", "Covenant Master"),
		tvn(tvPirate, tvCaldari, "Boss", "Big Boss", "Kingpin"),
		tvn(tvPirate, tvGallente, "Patron", "Don"),
		tvn(tvPirate, tvMinmatar, "Warlord", "Raid King"),
		tvn(tvCartel, "", "Boss", "Syndicate Head"),
		tvn(tvCartel, tvAmarr, "Covenant Master", "Sanctum Lord"),
		tvn(tvCartel, tvCaldari, "Kingpin", "Syndicate Head"),
		tvn(tvCartel, tvGallente, "Patron", "Proprietor"),
		tvn(tvCartel, tvMinmatar, "Warlord", "Smuggler King"),
	},
	titleKindDirector: {
		tvn("", "", "Director", "Executive Director"),
		tvn("", tvAmarr, "Steward", "Vassal Lord"),
		tvn("", tvCaldari, "Executive Director", "Director"),
		tvn("", tvGallente, "Board Director", "Director"),
		tvn("", tvMinmatar, "Elder", "Clan Elder"),
		tvn(tvNavy, "", "Deputy Commander", "Vice Commandant"),
		tvn(tvMilitia, "", "Fleet Captain", "Vice Admiral"),
		tvn(tvMerc, "", "Partner", "Executive Officer"),
		tvn(tvPirate, "", "Underboss", "Lieutenant"),
		tvn(tvCartel, "", "Underboss", "Consigliere"),
		tvn(tvTrade, "", "Partner", "Board Member"),
		tvn(tvLab, "", "Associate Director", "Fellow of the Board"),
	},
	titleKindPersonnel: {
		tvn("", "", "Recruitment Officer", "Personnel Officer"),
		tvn("", tvAmarr, "Master of Novices", "Chamberlain"),
		tvn("", tvCaldari, "Human Resources Officer", "Personnel Officer"),
		tvn("", tvGallente, "Membership Secretary", "Recruitment Officer"),
		tvn("", tvMinmatar, "Clan Recruiter", "Kinfinder"),
		tvn("navy militia", "", "Recruiting Officer", "Enlistment Officer"),
		tvn(tvMerc, "", "Talent Officer", "Recruiter"),
		tvn("pirate cartel", "", "Press-Gang Master", "Talent Spotter"),
	},
	titleKindTreasurer: {
		tvn("", "", "Treasurer", "Bursar"),
		tvn("", tvAmarr, "Keeper of the Purse", "Tithe-Master"),
		tvn("", tvCaldari, "Chief Financial Officer", "Treasurer"),
		tvn("", tvGallente, "Treasurer", "Finance Secretary"),
		tvn("", tvMinmatar, "Purse-Keeper", "Hoard-Keeper"),
		tvn("navy militia merc", "", "Paymaster", "Purser"),
		tvn("pirate cartel", "", "Bagman", "Purser"),
		tvn(tvTrade, "", "Comptroller", "Treasurer"),
	},
	titleKindCombat: {
		tvn("", "", "Security Officer", "Head of Security"),
		tvn("", tvAmarr, "Knight-Protector", "Security Officer"),
		tvn("", tvCaldari, "Security Officer", "Security Chief"),
		tvn("", tvGallente, "Security Officer", "Chief of Security"),
		tvn("", tvMinmatar, "Battle Leader", "Guard Chief"),
		tvn(tvNavy, "", "Patrol Commander", "Patrol Captain"),
		tvn(tvNavy, tvAmarr, "Knight-Commander", "Patrol Commander"),
		tvn(tvNavy, tvMinmatar, "Battle Leader", "Patrol Commander"),
		tvn(tvMilitia, "", "Field Commander", "Wing Commander"),
		tvn(tvMerc, "", "Field Commander", "Operations Lead"),
		tvn(tvPirate, "", "Raid Leader", "Reaver Captain"),
		tvn(tvCartel, "", "Raid Leader", "Enforcer"),
	},
	titleKindMining: {
		tvn("", "", "Mining Foreman", "Shift Boss", "Crew Chief", "Belt Boss", "Dig Master"),
		tvn("", tvAmarr, "Quarry Master", "Overseer of the Seams"),
		tvn("", tvCaldari, "Extraction Supervisor", "Shift Supervisor"),
		tvn("", tvGallente, "Claim Foreman", "Mining Foreman"),
		tvn("", tvMinmatar, "Seam Boss", "Dig Chief"),
		tvn("pirate cartel", "", "Strip Boss", "Dig Boss"),
	},
	titleKindFactory: {
		tvn("", "", "Director of Industry", "Works Manager"),
		tvn("", tvAmarr, "Master of Works", "Forge Steward"),
		tvn("", tvCaldari, "Plant Director", "Director of Industry"),
		tvn("", tvGallente, "Director of Industry", "Production Director"),
		tvn("", tvMinmatar, "Forge Chief", "Smith-Chief"),
		tvn(tvYard, "", "Master Shipwright", "Yard Boss"),
		tvn("pirate cartel", "", "Head Cook", "Factory Boss"),
		tvn(tvLab, "", "Head of Engineering", "Chief Engineer"),
	},
	tkHeadSecurity: {
		tvn("", "", "Director of Security", "Head of Security"),
		tvn("", tvAmarr, "Lord Protector", "Director of Security"),
		tvn(tvNavy, "", "Marshal", "Chief of Patrols"),
		tvn(tvMilitia, "", "Fleet Marshal", "Chief of Staff"),
		tvn(tvMerc, "", "Head of Operations", "Operations Director"),
		tvn("pirate cartel", "", "Head Enforcer", "Chief Enforcer"),
		tvn(tvMine, "", "Head of Site Security", "Belt Security Chief"),
		tvn(tvLab, "", "Head of Facility Security", "Security Director"),
		tvn(tvTrade, "", "Chief of Convoy Security", "Head of Escort"),
		tvn(tvYard, "", "Head of Yard Security", "Director of Security"),
	},
	tkHeadMining: {
		tvn("", "", "Director of Mining", "Head of Mining"),
		tvn("", tvAmarr, "Steward of the Mines", "Director of Mining"),
		tvn("", tvCaldari, "Director of Extraction", "Director of Mining"),
		tvn("", tvMinmatar, "Seam Master", "Director of Mining"),
		tvn(tvMine, "", "Mine Superintendent", "Mine Boss"),
		tvn("pirate cartel", "", "Ore Boss", "Head Digger"),
	},
	tkHeadSales: {
		tvn("", "", "Director of Sales", "Director of Commerce"),
		tvn("", tvAmarr, "Master of Trade", "Director of Commerce"),
		tvn("", tvCaldari, "Chief Commercial Officer", "Director of Sales"),
		tvn("", tvGallente, "Director of Commerce", "Director of Sales"),
		tvn("", tvMinmatar, "Market Chief", "Director of Sales"),
		tvn(tvTrade, "", "Chief Merchant", "Head of Trading"),
		tvn(tvPirate, "", "Head Fence", "Chief Fence"),
		tvn(tvCartel, "", "Distributor", "Head of Distribution"),
	},
	tkHeadHR: {
		tvn("", "", "Director of Personnel", "Head of Personnel"),
		tvn("", tvAmarr, "Chamberlain", "Lord Chamberlain"),
		tvn("", tvCaldari, "Director of Human Resources", "Director of Personnel"),
		tvn("", tvMinmatar, "Kin-Keeper", "Director of Personnel"),
		tvn("navy militia", "", "Adjutant", "Personnel Adjutant"),
		tvn(tvMerc, "", "Head of Talent", "Director of Personnel"),
		tvn("pirate cartel", "", "Press-Gang Boss", "Head of Crews"),
	},
	tkHeadInfra: {
		tvn("", "", "Director of Infrastructure", "Head of Infrastructure"),
		tvn("", tvAmarr, "Master of Works", "Director of Infrastructure"),
		tvn("", tvCaldari, "Director of Facilities", "Director of Infrastructure"),
		tvn("", tvMinmatar, "Tower Chief", "Director of Infrastructure"),
		tvn(tvYard, "", "Head of Facilities", "Director of Infrastructure"),
		tvn("pirate cartel", "", "Keeper of the Keeps", "Fort Boss"),
	},
	tkSeatFueler: {
		tvn("", "", "Fuel Technician", "Tower Engineer", "Tower Technician"),
		tvn("", tvAmarr, "Keeper of the Tower", "Tower Engineer"),
		tvn("pirate cartel", "", "Tower Rat", "Fuel Monkey"),
	},
	tkSeatProcurer: {
		tvn("", "", "Procurement Officer", "Quartermaster", "Buyer"),
		tvn("", tvAmarr, "Provisioner", "Procurement Officer"),
		tvn("navy militia merc", "", "Quartermaster", "Supply Officer"),
		tvn("pirate cartel", "", "Scrounger", "Quartermaster"),
	},
	tkSeatAdmin: {
		tvn("", "", "Factory Administrator", "Line Supervisor", "Plant Manager"),
		tvn("", tvCaldari, "Plant Manager", "Production Administrator"),
		tvn(tvYard, "", "Yard Supervisor", "Slipway Manager"),
		tvn("pirate cartel", "", "Line Boss", "Cookhouse Boss"),
	},
	tkSeatSales: {
		tvn("", "", "Sales Agent", "Sales Factor", "Commercial Agent"),
		tvn("pirate cartel", "", "Fixer", "Middleman"),
	},
}

var titleLadderPools = map[string][]tvLadderPool{
	rankLadderKey(armSecurity): {
		tvl("", "", tvRungs{"Cadet", "Trooper", "Corporal", "Sergeant", "Staff Sergeant", "Master Sergeant"}),
		tvl("", tvAmarr, tvRungs{"Novice", "Sentinel", "Templar", "Sergeant-at-Arms", "Knight", "Paladin"}),
		tvl("", tvCaldari,
			tvRungs{"Guard Recruit", "Guardsman", "Lance Corporal", "Sergeant", "Gunnery Sergeant", "Sergeant Major"},
			tvRungs{"Cadet", "Trooper", "Corporal", "Sergeant", "Staff Sergeant", "Master Sergeant"}),
		tvl("", tvGallente, tvRungs{"Deputy", "Officer", "Senior Officer", "Sergeant", "Lieutenant", "Captain"}),
		tvl("", tvMinmatar, tvRungs{"Youngblood", "Warrior", "Raider", "Veteran Warrior", "Warband Leader", "War Elder"}),
		tvl(tvEcon, "",
			tvRungs{"Guard", "Escort", "Senior Escort", "Watchman", "Watch Lead", "Watch Captain"},
			tvRungs{"Security Trainee", "Security Guard", "Security Specialist", "Senior Guard", "Security Supervisor", "Security Chief"}),
		tvl(tvEcon, tvAmarr, tvRungs{"Novice Warden", "Warden", "Senior Warden", "Sergeant-Warden", "Warden-Captain", "Warden-Knight"}),
		tvl(tvEcon, tvCaldari, tvRungs{"Security Trainee", "Security Guard", "Security Specialist", "Senior Guard", "Security Supervisor", "Security Chief"}),
		tvl(tvEcon, tvGallente, tvRungs{"Deputy", "Constable", "Senior Constable", "Watch Sergeant", "Watch Lieutenant", "Watch Commander"}),
		tvl(tvEcon, tvMinmatar, tvRungs{"Lookout", "Warder", "Senior Warder", "Spear", "Spear Leader", "Spear Elder"}),
		tvl(tvMerc, "",
			tvRungs{"Contractor", "Operator", "Senior Operator", "Specialist Operator", "Team Lead", "Principal Operator"},
			tvRungs{"Hireling", "Soldier", "Veteran Soldier", "Sergeant", "Lieutenant", "Captain"}),
		tvl(tvPirate, "",
			tvRungs{"Deckhand", "Thug", "Gunner", "Cutthroat", "Reaver", "Corsair Captain"},
			tvRungs{"Bilge Rat", "Brawler", "Gunner", "Marauder", "Reaver", "Dread Reaver"}),
		tvl(tvCartel, "", tvRungs{"Muscle", "Heavy", "Enforcer", "Senior Enforcer", "Made Gun", "Gun Boss"}),
	},
	rankLadderKey(armMining): {
		tvl("", "",
			tvRungs{"Prospector", "Miner", "Rock Hauler", "Senior Miner", "Rock Boss", "Master Miner"},
			tvRungs{"Ore Hand", "Miner", "Driller", "Senior Driller", "Drill Boss", "Master Driller"}),
		tvl("", tvAmarr,
			tvRungs{"Laborer", "Quarrier", "Stonecutter", "Senior Quarrier", "Quarry Steward", "Master Quarrier"},
			tvRungs{"Seam Novice", "Seamworker", "Seamcutter", "Senior Seamcutter", "Seam Steward", "Master of Seams"}),
		tvl("", tvCaldari,
			tvRungs{"Extraction Trainee", "Extraction Tech", "Extraction Specialist", "Senior Extraction Specialist", "Extraction Lead", "Extraction Manager"},
			tvRungs{"Drill Trainee", "Drill Operator", "Drill Technician", "Senior Drill Technician", "Drill Supervisor", "Drill Manager"}),
		tvl("", tvGallente,
			tvRungs{"Claim Worker", "Claim Miner", "Prospector", "Senior Prospector", "Claim Boss", "Claim Master"},
			tvRungs{"Ore Hand", "Miner", "Driller", "Senior Driller", "Drill Boss", "Master Driller"}),
		tvl("", tvMinmatar,
			tvRungs{"Rock-Breaker", "Digger", "Ore Runner", "Seam Hunter", "Deep Digger", "Rock Elder"},
			tvRungs{"Pick Hand", "Rock Hauler", "Rock Splitter", "Seam Splitter", "Pit Leader", "Pit Elder"}),
		tvl("pirate cartel", "",
			tvRungs{"Rock Rat", "Scraper", "Digger", "Hard Digger", "Strip Hand", "Strip Chief"},
			tvRungs{"Dirt Hand", "Grubber", "Pit Scraper", "Hard Scraper", "Pit Boss", "Strip Master"}),
	},
	rankLadderKey(armIndustry): {
		tvl("", "", tvRungs{"Apprentice", "Technician", "Fabricator", "Senior Fabricator", "Engineer", "Master Engineer"}),
		tvl("", tvAmarr, tvRungs{"Bound Apprentice", "Journeyman", "Artisan", "Senior Artisan", "Master Artisan", "Grand Artisan"}),
		tvl("", tvCaldari,
			tvRungs{"Line Associate", "Production Tech", "Production Specialist", "Senior Production Specialist", "Plant Engineer", "Chief Engineer"},
			tvRungs{"Apprentice", "Technician", "Fabricator", "Senior Fabricator", "Engineer", "Master Engineer"}),
		tvl("", tvGallente, tvRungs{"Assembler", "Line Technician", "Fabricator", "Senior Fabricator", "Design Engineer", "Lead Designer"}),
		tvl("", tvMinmatar, tvRungs{"Tinker", "Rigger", "Smith", "Master Rigger", "Forgemaster", "Forge Elder"}),
		tvl(tvYard, "",
			tvRungs{"Yard Hand", "Riveter", "Fitter", "Hull Fitter", "Shipwright", "Master Shipwright"},
			tvRungs{"Slip Hand", "Plater", "Welder", "Senior Welder", "Hull Wright", "Master Hull Wright"}),
		tvl("pirate cartel", "", tvRungs{"Line Hand", "Cook", "Batch Mixer", "Chemist", "Senior Chemist", "Master Chemist"}),
	},
	rankLadderKey(armResearch): {
		tvl("", "", tvRungs{"Lab Assistant", "Researcher", "Associate Scientist", "Scientist", "Senior Scientist", "Principal Scientist"}),
		tvl("", tvAmarr, tvRungs{"Acolyte", "Scribe", "Scholar", "Senior Scholar", "Archivist", "Master Scholar"}),
		tvl("", tvCaldari, tvRungs{"Lab Associate", "Research Associate", "Research Engineer", "Senior Research Engineer", "Principal Engineer", "Research Fellow"}),
		tvl("", tvGallente, tvRungs{"Intern", "Junior Researcher", "Researcher", "Senior Researcher", "Lead Researcher", "Professor"}),
		tvl("", tvMinmatar, tvRungs{"Tinkerer", "Artificer's Hand", "Artificer", "Senior Artificer", "Master Artificer", "Sage"}),
		tvl("pirate cartel", "", tvRungs{"Lab Rat", "Tester", "Analyst", "Senior Analyst", "Lab Boss", "Master Cook"}),
	},
	rankLadderKey(armSales): {
		tvl("", "", tvRungs{"Clerk", "Broker", "Trader", "Senior Trader", "Merchant", "Master Merchant"}),
		tvl("", tvAmarr, tvRungs{"Apprentice Factor", "Factor", "Merchant", "Senior Merchant", "Merchant Steward", "Master of Coin"}),
		tvl("", tvCaldari,
			tvRungs{"Sales Associate", "Account Rep", "Account Manager", "Senior Account Manager", "Sales Executive", "Executive Partner"},
			tvRungs{"Sales Trainee", "Sales Rep", "Sales Officer", "Senior Sales Officer", "Sales Manager", "Senior Sales Manager"}),
		tvl("", tvGallente,
			tvRungs{"Clerk", "Broker", "Trader", "Senior Trader", "Merchant", "Master Merchant"},
			tvRungs{"Market Runner", "Broker", "Dealer", "Senior Dealer", "Merchant", "Merchant Banker"}),
		tvl("", tvMinmatar, tvRungs{"Peddler", "Haggler", "Trader", "Caravan Trader", "Caravan Master", "Trade Elder"}),
		tvl("pirate cartel", "",
			tvRungs{"Runner", "Courier", "Smuggler", "Fence", "Dealer", "Black Marketeer"},
			tvRungs{"Mule", "Carrier", "Bootlegger", "Senior Bootlegger", "Contraband Boss", "Contraband King"}),
	},
	rankLadderKey(armInfrastructure): {
		tvl("", "", tvRungs{"Rigger", "Tower Hand", "Tower Technician", "Senior Technician", "Tower Engineer", "Chief Engineer"}),
		tvl("", tvAmarr, tvRungs{"Bearer", "Keeper", "Warden", "Senior Warden", "Castellan", "High Castellan"}),
		tvl("", tvCaldari, tvRungs{"Facilities Trainee", "Facilities Tech", "Facilities Engineer", "Senior Facilities Engineer", "Facilities Manager", "Facilities Director"}),
		tvl("", tvMinmatar, tvRungs{"Hauler", "Rigger", "Tower Hand", "Senior Hand", "Tower Boss", "Tower Elder"}),
		tvl("pirate cartel", "", tvRungs{"Grunt", "Fuel Monkey", "Tower Rat", "Senior Rat", "Keep Boss", "Warlord's Engineer"}),
	},
	rankLadderKey(armHR): {
		tvl("", "", tvRungs{"Personnel Clerk", "Personnel Aide", "Recruiter", "Senior Recruiter", "Personnel Lead", "Personnel Veteran"}),
		tvl("", tvAmarr, tvRungs{"Page", "Usher", "Herald", "Senior Herald", "High Herald", "Master Herald"}),
		tvl("", tvCaldari, tvRungs{"HR Assistant", "HR Associate", "Talent Associate", "Senior Talent Associate", "HR Manager", "HR Director"}),
		tvl("", tvGallente, tvRungs{"Membership Clerk", "Membership Aide", "Recruiter", "Senior Recruiter", "Membership Lead", "Membership Director"}),
		tvl("", tvMinmatar, tvRungs{"Kin-Caller", "Kin-Seeker", "Recruiter", "Senior Recruiter", "Kin-Leader", "Kin-Elder"}),
		tvl("navy militia", "", tvRungs{"Recruiting Clerk", "Recruiting Aide", "Recruiting Agent", "Senior Recruiting Agent", "Recruiting Sergeant", "Recruiting Chief"}),
		tvl(tvMerc, "", tvRungs{"Talent Scout", "Recruiter", "Senior Recruiter", "Talent Lead", "Talent Manager", "Talent Director"}),
		tvl("pirate cartel", "", tvRungs{"Lookout", "Crimp", "Press-Ganger", "Senior Crimp", "Crimp Boss", "Head Crimp"}),
	},
	rankLadderKey(armFinance): {
		tvl("", "", tvRungs{"Ledger Clerk", "Bookkeeper", "Analyst", "Senior Analyst", "Controller", "Comptroller"}),
		tvl("", tvAmarr, tvRungs{"Tithe Clerk", "Tithe Counter", "Tithe Reckoner", "Senior Reckoner", "Tithe Steward", "Master of Tithes"}),
		tvl("", tvCaldari, tvRungs{"Junior Analyst", "Financial Analyst", "Senior Financial Analyst", "Associate Controller", "Controller", "Financial Controller"}),
		tvl("", tvGallente, tvRungs{"Accounts Clerk", "Accountant", "Senior Accountant", "Auditor", "Senior Auditor", "Chief Auditor"}),
		tvl("", tvMinmatar, tvRungs{"Tally Hand", "Tally Keeper", "Reckoner", "Senior Reckoner", "Hoard Steward", "Hoard Elder"}),
		tvl("pirate cartel", "", tvRungs{"Counter", "Bookie", "Senior Bookie", "Moneyman", "Money Boss", "Vault Keeper"}),
	},
	tkMeritLadder: {
		tvl("", "", tvRungs{"Recruit", "Associate", "Specialist", "Senior Specialist", "Lead", "Veteran"}),
		tvl("", tvAmarr, tvRungs{"Initiate", "Servant", "Retainer", "Senior Retainer", "Chief Retainer", "High Retainer"}),
		tvl("", tvCaldari, tvRungs{"Trainee", "Associate", "Senior Associate", "Supervisor", "Manager", "Senior Manager"}),
		tvl("", tvGallente, tvRungs{"Member", "Associate", "Senior Associate", "Junior Partner", "Senior Partner", "Fellow"}),
		tvl("", tvMinmatar, tvRungs{"Newcomer", "Kin", "Clan Hand", "Clan Veteran", "Elder Hand", "Honored Kin"}),
		tvl("navy militia", "", tvRungs{"Enlistee", "Private", "Private First Class", "Specialist", "Senior Specialist", "Warrant Officer"}),
		tvl(tvMerc, "", tvRungs{"Probationer", "Associate Contractor", "Full Contractor", "Veteran Contractor", "Senior Contractor", "Legionnaire"}),
		tvl("pirate cartel", "", tvRungs{"Swab", "Hand", "Crewman", "Bosun", "Mate", "First Mate"}),
	},
}

// ── resolution ──────────────────────────────────────────────────────────────

func tvMatches(list, v string) bool {
	if list == "" {
		return true
	}
	for _, x := range strings.Fields(list) {
		if x == v {
			return true
		}
	}
	return false
}

func tvSpecificity(kinds, races string) int {
	s := 0
	if kinds != "" {
		s += 2
	}
	if races != "" {
		s++
	}
	return s
}

// tvHash is the corporation's fixed roll for one key.
func tvHash(corpID int64, key string) int {
	h := fnv.New32a()
	fmt.Fprintf(h, "%d|%s", corpID, key)
	return int(h.Sum32() & 0x7fffffff)
}

// tvLevels groups a key's matching candidates by specificity, most specific
// first. Each level is the union of its matching rows, in table order.
func tvNameLevels(pools []tvPool, kind, race string) [][]string {
	var lv [4][]string
	for _, p := range pools {
		if tvMatches(p.kinds, kind) && tvMatches(p.races, race) {
			s := tvSpecificity(p.kinds, p.races)
			lv[s] = append(lv[s], p.names...)
		}
	}
	var out [][]string
	for s := 3; s >= 0; s-- {
		if len(lv[s]) > 0 {
			out = append(out, lv[s])
		}
	}
	return out
}

func tvLadderLevels(pools []tvLadderPool, kind, race string) [][]tvRungs {
	var lv [4][]tvRungs
	for _, p := range pools {
		if tvMatches(p.kinds, kind) && tvMatches(p.races, race) {
			s := tvSpecificity(p.kinds, p.races)
			lv[s] = append(lv[s], p.ladders...)
		}
	}
	var out [][]tvRungs
	for s := 3; s >= 0; s-- {
		if len(lv[s]) > 0 {
			out = append(out, lv[s])
		}
	}
	return out
}

// resolveTitleVocab names every key for one corporation. Deterministic in
// (kind, race, corpID).
func resolveTitleVocab(kind, race string, corpID int64) map[string]string {
	out := map[string]string{}
	taken := map[string]bool{}
	take := func(k, n string) {
		out[k] = n
		taken[n] = true
	}
	pickName := func(key string) {
		for _, level := range tvNameLevels(titleNamePools[key], kind, race) {
			start := tvHash(corpID, key) % len(level)
			for i := range level {
				if n := level[(start+i)%len(level)]; !taken[n] {
					take(key, n)
					return
				}
			}
		}
		// Every candidate taken: the plain name, numbered until free.
		base := titleNamePools[key][0].names[0]
		n := base
		for i := 2; taken[n]; i++ {
			n = fmt.Sprintf("%s %d", base, i)
		}
		take(key, n)
	}
	pickLadder := func(group string, keyOf func(int) string, prefix string) {
		free := func(l tvRungs) bool {
			seen := map[string]bool{}
			for _, n := range l {
				if taken[n] || seen[n] {
					return false
				}
				seen[n] = true
			}
			return true
		}
		for _, level := range tvLadderLevels(titleLadderPools[group], kind, race) {
			start := tvHash(corpID, group) % len(level)
			for i := range level {
				if l := level[(start+i)%len(level)]; free(l) {
					for r, n := range l {
						take(keyOf(r), n)
					}
					return
				}
			}
		}
		// Nothing free: the plain ladder under the arm's name.
		for r, n := range titleLadderPools[group][0].ladders[0] {
			name := strings.TrimSpace(prefix + " " + n)
			for i := 2; taken[name]; i++ {
				name = fmt.Sprintf("%s %s %d", prefix, n, i)
			}
			take(keyOf(r), name)
		}
	}

	for _, k := range titleSlotKeys {
		pickName(k)
	}
	for _, a := range titleLadderArms {
		a := a
		pickLadder(rankLadderKey(a), func(r int) string { return rankTitleKey(a, r) }, strings.Fields(armLabel(a))[0])
	}
	pickLadder(tkMeritLadder, meritTitleKey, "Member")
	for _, k := range titleHeadKeys {
		pickName(k)
	}
	for _, k := range titleSeatKeys {
		pickName(k)
	}
	return out
}

// ── the corporation's set ───────────────────────────────────────────────────

// corpTitleSet is one corporation's names, overrides applied.
type corpTitleSet struct {
	CorpID     int64
	Kind, Race string
	generated  map[string]string
	overrides  map[string]string
}

// Name is what key is called in this corporation.
func (s *corpTitleSet) Name(key string) string {
	if s == nil {
		return defaultTitleSet.Name(key)
	}
	if n, ok := s.overrides[key]; ok && n != "" {
		return n
	}
	if n, ok := s.generated[key]; ok {
		return n
	}
	return ""
}

// Generated is the vocabulary's own name for key, ignoring any override.
func (s *corpTitleSet) Generated(key string) string { return s.generated[key] }

// Override is the operator's name for key ("" for none).
func (s *corpTitleSet) Override(key string) string { return s.overrides[key] }

// RankName is a rung's name: the arm's ladder for an arm member, the merit
// ladder for anyone else.
func (s *corpTitleSet) RankName(a corpArm, r int) string {
	if _, ok := titleLadderPools[rankLadderKey(a)]; ok && a != "" {
		return s.Name(rankTitleKey(a, r))
	}
	return s.Name(meritTitleKey(r))
}

func newCorpTitleSet(corpID int64, kind, race string, overrides map[string]string) *corpTitleSet {
	if overrides == nil {
		overrides = map[string]string{}
	}
	return &corpTitleSet{CorpID: corpID, Kind: kind, Race: race,
		generated: resolveTitleVocab(kind, race, corpID), overrides: overrides}
}

// defaultTitleSet is the plain house's names, for a caller with no
// corporation.
var defaultTitleSet = newCorpTitleSet(0, tvHouse, tvNeutral, nil)

const titleSetTTL = 600

type titleSetEntry struct {
	set *corpTitleSet
	at  int64
}

var (
	titleSetMu    sync.Mutex
	titleSetCache = map[int64]titleSetEntry{}
)

// dropTitleSet forgets a corporation's cached set (an override was saved).
func dropTitleSet(corpID int64) {
	titleSetMu.Lock()
	delete(titleSetCache, corpID)
	titleSetMu.Unlock()
}

func init() {
	registerCorpForget("titleSetCache", dropTitleSet)
	registerSweep("titleSetCacheSweep", func() {
		now := clock.Unix()
		titleSetMu.Lock()
		for id, e := range titleSetCache {
			if now-e.at >= 2*titleSetTTL {
				delete(titleSetCache, id)
			}
		}
		titleSetMu.Unlock()
	})
}

// titleSetOf is a corporation's names. Never nil: a corporation that cannot
// be read gets the plain house's.
func (m *Mod) titleSetOf(ctx context.Context, corpID int64) *corpTitleSet {
	if corpID == 0 {
		return defaultTitleSet
	}
	now := clock.Unix()
	titleSetMu.Lock()
	if e, ok := titleSetCache[corpID]; ok && now-e.at < titleSetTTL {
		titleSetMu.Unlock()
		return e.set
	}
	titleSetMu.Unlock()

	facts, err := getAnyCorpFacts(ctx, m.db, corpID)
	if err != nil {
		return defaultTitleSet
	}
	s := m.titleSetFor(ctx, facts)
	titleSetMu.Lock()
	titleSetCache[corpID] = titleSetEntry{s, now}
	titleSetMu.Unlock()
	return s
}

// titleSetFor builds a set from facts already read (no cache).
func (m *Mod) titleSetFor(ctx context.Context, facts *CorpFacts) *corpTitleSet {
	raceID := facts.RaceID
	if raceID == 0 && facts.CEOID != 0 {
		raceID = m.charRace(ctx, facts.CEOID)
	}
	return newCorpTitleSet(facts.CorpID, titleKindOf(facts.Policy.Tags), titleRaceOf(raceID),
		m.loadTitleOverrides(ctx, facts.CorpID))
}

// corpTitle is shorthand for one name in one corporation.
func (m *Mod) corpTitle(ctx context.Context, corpID int64, key string) string {
	return m.titleSetOf(ctx, corpID).Name(key)
}

// ── overrides ───────────────────────────────────────────────────────────────

func (m *Mod) loadTitleOverrides(ctx context.Context, corpID int64) map[string]string {
	out := map[string]string{}
	rows, err := m.db.QueryContext(ctx,
		`SELECT titleKey, name FROM rolebot_title_overrides WHERE corporationID = ?`, corpID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k, n string
		if rows.Scan(&k, &n) == nil && n != "" {
			out[k] = n
		}
	}
	return out
}

// cleanTitleName validates an operator-typed name: printable ASCII (the
// client reads cp1252), at most titleNameMax characters.
func cleanTitleName(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > titleNameMax {
		return "", fmt.Errorf("%q is longer than %d characters", s, titleNameMax)
	}
	for _, ch := range s {
		if ch < 0x20 || ch > 0x7e {
			return "", fmt.Errorf("%q has a character the game client cannot show (ASCII only)", s)
		}
	}
	return s, nil
}

// validTitleKey reports whether k is one of the keys.
func validTitleKey(k string) bool {
	for _, x := range allTitleKeys() {
		if x == k {
			return true
		}
	}
	return false
}

// checkTitleOverrides validates a whole proposed override map against the
// corporation's generated names: no two keys may end up with one name.
func checkTitleOverrides(s *corpTitleSet, ov map[string]string) error {
	final := map[string]string{}
	for _, k := range allTitleKeys() {
		n := s.generated[k]
		if v := ov[k]; v != "" {
			n = v
		}
		if other, ok := final[n]; ok {
			return fmt.Errorf("%q would name both %s and %s", n, titleKeyLabel(other), titleKeyLabel(k))
		}
		final[n] = k
	}
	return nil
}

// saveTitleOverrides replaces a corporation's overrides.
func (m *Mod) saveTitleOverrides(ctx context.Context, corpID int64, ov map[string]string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("saveTitleOverrides: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rolebot_title_overrides WHERE corporationID = ?`, corpID); err != nil {
		return fmt.Errorf("saveTitleOverrides: %w", err)
	}
	now := clock.Unix()
	for k, n := range ov {
		if n == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rolebot_title_overrides (corporationID, titleKey, name, setAt) VALUES (?, ?, ?, ?)`,
			corpID, k, n, now); err != nil {
			return fmt.Errorf("saveTitleOverrides: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("saveTitleOverrides: %w", err)
	}
	dropTitleSet(corpID)
	return nil
}

// ── what earlier builds wrote ───────────────────────────────────────────────

// legacyTitleKeys maps every title text written before the vocabularies to the key it
// stood for. A member still wearing one is the module's to rewrite, and the key says
// whether the rewrite is news (a new post) or silent (a new vocabulary).
var legacyTitleKeys = func() map[string]string {
	out := map[string]string{
		"Chief Executive Officer":    titleKindCEO,
		"Director":                   titleKindDirector,
		"Recruitment Officer":        titleKindPersonnel,
		"Treasurer":                  titleKindTreasurer,
		"Mining Foreman":             titleKindMining,
		"Director of Industry":       titleKindFactory,
		"Industry Director":          titleKindFactory,
		"Factory Overseer":           titleKindFactory,
		"Security Officer":           titleKindCombat,
		"Patrol Commander":           titleKindCombat,
		"Field Commander":            titleKindCombat,
		"Raid Leader":                titleKindCombat,
		"Director of Security":       tkHeadSecurity,
		"Director of Operations":     tkHeadMining,
		"Director of Mining":         tkHeadMining,
		"Director of Commerce":       tkHeadSales,
		"Director of Sales":          tkHeadSales,
		"Director of Personnel":      tkHeadHR,
		"Director of Infrastructure": tkHeadInfra,
		"Fuel Technician":            tkSeatFueler,
		"Procurement Officer":        tkSeatProcurer,
		"Factory Administrator":      tkSeatAdmin,
		"Sales Agent":                tkSeatSales,
	}
	legacyArmTags := map[corpArm]string{armSecurity: "SEC", armIndustry: "IND", armResearch: "RND",
		armMining: "MIN", armSales: "SAL", armHR: "HR", armFinance: "FIN"}
	legacyArm := map[corpArm]tvRungs{
		armSecurity: {"Cadet", "Trooper", "Corporal", "Sergeant", "Staff Sergeant", "Master Sergeant"},
		armMining:   {"Prospector", "Miner", "Rock Hauler", "Senior Miner", "Rock Boss", "Master Miner"},
		armIndustry: {"Apprentice", "Technician", "Fabricator", "Senior Fabricator", "Engineer", "Master Engineer"},
		armResearch: {"Lab Assistant", "Researcher", "Associate Scientist", "Scientist", "Senior Scientist", "Principal Scientist"},
		armSales:    {"Clerk", "Broker", "Trader", "Senior Trader", "Merchant", "Master Merchant"},
		armHR:       {"Personnel Clerk", "Personnel Aide", "Recruiter", "Senior Recruiter", "Personnel Lead", "Personnel Veteran"},
		armFinance:  {"Ledger Clerk", "Bookkeeper", "Analyst", "Senior Analyst", "Controller", "Comptroller"},
	}
	for a, l := range legacyArm {
		for r, n := range l {
			out[fmt.Sprintf("[%s] %s", legacyArmTags[a], n)] = rankTitleKey(a, r)
		}
	}
	for r, n := range (tvRungs{"Recruit", "Associate", "Specialist", "Senior Specialist", "Lead", "Veteran"}) {
		out[n] = meritTitleKey(r)
	}
	return out
}()
