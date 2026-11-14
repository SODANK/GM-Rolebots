package rolebots

// The words a bot-founded corporation is named from (corpbrand.go builds the
// names). Same catalogue shape as the title vocabulary (corptitlevocab.go): each
// row is for a set of kinds and races (space separated; empty = any). Matching
// rows are pooled, more specific rows weighted heavier. Absent, and to stay
// absent: every empire, faction and NPC-corporation proper noun; state words
// that read as an institution (Imperial, Federal, Republic, State, Navy,
// Federation, Kingdom, Empire, Nation, Alliance); and words that are half of a
// real player organisation's name (Brave, Band, Legion, Horde, Coalition,
// Initiative, Fraternity, Goon). brandProblem (corpbrand.go) rejects them, and
// keeping them out of this list stops it rerolling forever.

const (
	bkEcon    = "mine lab trade yard works house"
	bkMil     = "merc navy militia"
	bkOutlaw  = "pirate cartel"
	bkEconMil = bkEcon + " " + bkMil
)

// ── templates ───────────────────────────────────────────────────────────────
//
// Slots: {region} a region name leading the name; {ofregion} "of <region>";
// {adj} {noun} {org} {unit} {crew} from the pools below; {founder} the
// founder's surname; {founders} its possessive. A name repeated in a row is a
// weight.

var brandTemplatePools = []tvPool{
	tvn(bkEcon, "",
		"{region} {noun} {org}", "{region} {noun} {org}", "{region} {noun} {org}",
		"{noun} {org} {ofregion}", "{noun} {org} {ofregion}",
		"{adj} {noun} {org}", "{adj} {noun} {org}", "{adj} {noun} {org}",
		"{adj} {noun}",
		"{founder} {noun}",
		"{founder} {org}", "{founder} {org}",
		"The {region} {org}",
		"{adj} {org} {ofregion}"),
	tvn(bkMil, "",
		"{adj} {unit}", "{adj} {unit}", "{adj} {unit}",
		"{region} {unit}", "{region} {unit}",
		"The {adj} {unit}",
		"{founders} {unit}", "{founders} {unit}",
		"{unit} {ofregion}", "{unit} {ofregion}",
		"{adj} {unit} {ofregion}",
		"{adj} {noun} {org}",
		"{region} {noun} {org}"),
	tvn(bkOutlaw, "",
		"{adj} {crew}", "{adj} {crew}", "{adj} {crew}",
		"The {adj} {crew}", "The {adj} {crew}",
		"{region} {crew}", "{region} {crew}",
		"{founders} {crew}", "{founders} {crew}",
		"{crew} {ofregion}"),
	tvn("cartel", "",
		"{adj} {noun}", "{adj} {noun}",
		"{adj} {noun} {org}",
		"{region} {noun} {org}"),
}

// ── adjectives ──────────────────────────────────────────────────────────────

var brandAdjPools = []tvPool{
	tvn("", "",
		"Iron", "Silver", "Ashen", "Distant", "Hollow", "Silent", "Steadfast",
		"Crimson", "Cobalt", "Amber", "Obsidian", "Granite", "Copper", "Pale",
		"Far", "Lone", "Meridian", "Zenith", "Apex", "Summit", "Horizon",
		"Polar", "Stellar", "Starlit", "Cold Iron", "Long Reach", "High Orbit",
		"Red Dust", "Black Ice", "First Light", "Last Light", "Quiet",
		"Northstar", "Farside", "Deepspace", "Outer Mark", "Twin Suns"),
	tvn(bkEconMil, "amarr",
		"Gilded", "Hallowed", "Radiant", "Sovereign", "Exalted", "Golden",
		"Sanctified", "Righteous", "Faithful", "Vigilant", "Crowned", "Solemn",
		"Resplendent", "Burnished", "Ivory", "Sunward", "Throne-Sworn"),
	tvn(bkEconMil, "caldari",
		"Integral", "Unified", "Consolidated", "Precise", "Prime", "Sterling",
		"Tempered", "Diligent", "Optimal", "Kinetic", "Vector", "Keen",
		"Frostline", "Graphite", "Whitefrost", "Northwind", "Coldline"),
	tvn(bkEconMil, "gallente",
		"Azure", "Verdant", "Bold", "Elegant", "Civic", "Luminous", "Harmonic",
		"Rising", "Gallant", "Sunlit", "Vivid", "Candid", "Open Hand",
		"Blue Horizon", "Starbright", "Bright Harbor", "Belle"),
	tvn(bkEconMil, "minmatar",
		"Unbroken", "Rusted", "Ironclad", "Wild", "Freeborn", "Unchained",
		"Scarred", "Ember", "Stormborn", "Defiant", "Wandering", "Roaring",
		"Blackrock", "Redstone", "Ashfall", "Broken Chain", "Long Road"),
	tvn(bkMil, "",
		"Hardened", "Stalwart", "Unyielding", "Relentless", "Grey", "Last",
		"Sworn", "Bonded", "Watchful", "Ever-Ready", "Steel", "Shieldwall"),
	tvn(bkOutlaw, "",
		"Black", "Grim", "Rogue", "Feral", "Hungry", "Crooked", "Lawless",
		"Godless", "Grinning", "Wicked", "Rabid", "Ragged", "Bitter", "Rotten",
		"Smiling", "Howling", "Gutter", "Red-Handed", "Last Chance", "Bad Luck",
		"Dead Man's", "No Quarter", "Knife-Edge", "Burnt", "Scorched"),
}

// ── nouns: what the corporation does ────────────────────────────────────────

var brandNounPools = []tvPool{
	tvn("mine", "",
		"Mining", "Extraction", "Excavations", "Minerals", "Prospecting", "Ore",
		"Quarrying", "Ore Works", "Rockworks", "Mineral Rights", "Belt Mining"),
	tvn("trade", "",
		"Trading", "Freight", "Commerce", "Haulage", "Logistics", "Exchange",
		"Mercantile", "Shipping", "Cargo", "Transit", "Brokerage", "Supply",
		"Couriers"),
	tvn("works", "",
		"Manufacturing", "Fabrication", "Industries", "Foundry", "Assembly",
		"Components", "Machine Works", "Engineering", "Metalworks"),
	tvn("yard", "",
		"Shipyards", "Shipbuilding", "Hullworks", "Drydocks", "Spaceworks",
		"Yards", "Orbital Yards"),
	tvn("lab", "",
		"Research", "Sciences", "Laboratories", "Applied Sciences", "Analytics",
		"Technologies", "Bioworks"),
	tvn("house", "",
		"Ventures", "Enterprises", "Interests", "Acquisitions"),
	tvn("cartel", "",
		"Chemicals", "Compounds", "Distillers", "Imports", "Boosters",
		"Refining", "Solutions"),
	tvn(bkMil, "",
		"Security", "Defense", "Protection", "Arms", "Escorts", "Contracting"),
}

// ── organisation forms, by race ─────────────────────────────────────────────

var brandOrgPools = []tvPool{
	tvn(bkEconMil, "caldari",
		"Holdings", "Combine", "Group", "Consortium", "Partners", "Limited",
		"Works", "Trust"),
	tvn(bkEconMil, "amarr",
		"Holders", "Fellowship", "Covenant", "Household", "Charter", "Trust",
		"Guild", "Order", "Estate", "Patrimony"),
	tvn(bkEconMil, "gallente",
		"Collective", "Cooperative", "Partners", "Associates", "Union",
		"Company", "Network", "Commons", "Guild"),
	tvn(bkEconMil, "minmatar",
		"Clan", "Kin", "Freeclans", "Kinship", "Hearth", "Freehold", "Lodge",
		"Circle", "Fellowship"),
	tvn(bkEconMil, "neutral",
		"Group", "Company", "Concern", "Partners", "Enterprises"),
	tvn(bkOutlaw, "",
		"Concern", "Outfit", "Ring", "Combine", "Consortium", "Cartel"),
}

// ── military units ──────────────────────────────────────────────────────────

var brandUnitPools = []tvPool{
	tvn(bkMil, "",
		"Rangers", "Wardens", "Lancers", "Company", "Battalion", "Regiment",
		"Brigade", "Vanguard", "Rearguard", "Outriders", "Free Company",
		"Irregulars", "Volunteers", "Pathfinders", "Shieldbearers"),
	tvn(bkMil, "amarr", "Penitents", "Heralds", "Brethren", "Swordsworn", "Lancers"),
	tvn(bkMil, "caldari", "Contractors", "Response Group", "Defense Group",
		"Detachment", "Operators", "Security Detail"),
	tvn(bkMil, "gallente", "Volunteers", "Patriots", "Free Rangers", "Citizen Guard"),
	tvn(bkMil, "minmatar", "Warband", "Riders", "Skirmishers", "Freeriders",
		"Wolves", "Hounds", "Spearmen"),
}

// ── crews (outlaws) ─────────────────────────────────────────────────────────

var brandCrewPools = []tvPool{
	tvn(bkOutlaw, "",
		"Raiders", "Reavers", "Corsairs", "Brigands", "Cutthroats", "Jackals",
		"Scavengers", "Wreckers", "Buccaneers", "Freebooters", "Desperadoes",
		"Rogues", "Outlaws", "Gang", "Crew", "Mob", "Hyenas", "Knives", "Pack"),
}

// ── descriptions ────────────────────────────────────────────────────────────
//
// {place} is the region as it reads mid-sentence ("the Forge"); {founder} is
// the founder's full name.

var brandDescPools = []tvPool{
	tvn("mine", "",
		"An independent mining concern working the belts of {place}.",
		"Ore out of {place}, refined honestly and sold at a fair price.",
		"Founded by {founder} to dig where the big houses will not."),
	tvn("trade", "",
		"Freight and trade out of {place}, flown by pilots who know the lanes.",
		"Buy low, haul far, sell high. Founded in {place}.",
		"A trading house built on {founder}'s first cargo run."),
	tvn("works", "",
		"Manufacturing out of {place}: modules, ammunition and the parts between.",
		"{founder}'s workshop, grown into a production line."),
	tvn("yard", "",
		"Hulls built in {place}, sold to whoever flies them.",
		"A shipbuilding concern founded by {founder}."),
	tvn("lab", "",
		"Research and blueprint work out of {place}.",
		"Applied science for paying clients, founded by {founder}."),
	tvn("house", "",
		"An independent house founded by {founder} in {place}.",
		"Interests across {place} and beyond."),
	tvn("merc", "",
		"Guns for hire out of {place}. Contracts honoured, payment in advance.",
		"{founder}'s company. We fight for whoever pays."),
	tvn("navy militia", "",
		"Keeping the lanes of {place} safe when nobody else will.",
		"Founded by {founder} to protect honest pilots in {place}."),
	tvn("pirate", "",
		"Your cargo or your ship. {place} belongs to us.",
		"{founder}'s crew. Don't ask twice."),
	tvn("cartel", "",
		"Product moves through {place}. Questions do not.",
		"{founder}'s operation. Discreet supply, no receipts."),
}

// brandMottoPools is the optional second sentence, by race. Outlaws and hired
// guns speak for nobody's empire, so they get none.
const brandMottoKinds = bkEcon + " navy militia"

var brandMottoPools = []tvPool{
	tvn(brandMottoKinds, "amarr", "By faith and by labour.", "Service is its own reward.",
		"Order, obedience, prosperity."),
	tvn(brandMottoKinds, "caldari", "Shareholders first.", "Efficiency is loyalty.",
		"The contract is the bond."),
	tvn(brandMottoKinds, "gallente", "Every member has a voice.", "Free trade, free people.",
		"Prosperity shared is prosperity doubled."),
	tvn(brandMottoKinds, "minmatar", "No chains, no masters.", "Kin before coin.", "We remember."),
}

// ── regions ─────────────────────────────────────────────────────────────────

const (
	// regionNever: never named (code names, Jove space, anything unclassed).
	regionNever = iota
	// regionPlain: any template.
	regionPlain
	// regionArticle: a leading The, dropped before a noun and in of the X.
	regionArticle
	// regionOfOnly: a noble house, a faction or an NPC corporation shares the
	// name, so it may only follow "of" ("Kador Holdings" reads as the Kador
	// Family's property).
	regionOfOnly
	// regionWord: an ordinary English word (Catch Mining reads wrong), so of only,
	// except for an outlaw (Venal Raiders).
	regionWord
)

// brandRegion is one region's rules. Of overrides the of form when the bare
// name reads wrong after of (Outer Ring becomes of the Outer Ring).
type brandRegion struct {
	Class int
	Of    string
}

// brandRegions classes every region in mapRegions. A region missing here is
// regionNever, so a new region cannot leak into names unclassed.
var brandRegions = map[string]brandRegion{
	// Empire and lowsec.
	"Lonetrek": {Class: regionPlain}, "Metropolis": {Class: regionPlain},
	"The Citadel": {Class: regionArticle}, "Domain": {Class: regionPlain},
	"The Forge": {Class: regionArticle}, "Tash-Murkon": {Class: regionOfOnly},
	"Sinq Laison": {Class: regionPlain}, "Placid": {Class: regionPlain},
	"Heimatar": {Class: regionPlain}, "Genesis": {Class: regionPlain},
	"Kador": {Class: regionOfOnly}, "Essence": {Class: regionPlain},
	"Kor-Azor": {Class: regionOfOnly}, "Everyshore": {Class: regionPlain},
	"Aridia": {Class: regionPlain}, "Syndicate": {Class: regionOfOnly},
	"Solitude": {Class: regionPlain}, "Verge Vendor": {Class: regionPlain},
	"Molden Heath": {Class: regionPlain}, "Devoid": {Class: regionWord},
	"Derelik": {Class: regionPlain}, "The Bleak Lands": {Class: regionArticle},
	"Khanid": {Class: regionOfOnly},
	// Pirate and NPC nullsec.
	"Curse": {Class: regionWord}, "Stain": {Class: regionWord},
	"Venal": {Class: regionWord}, "Great Wildlands": {Class: regionPlain},
	"Outer Ring": {Class: regionOfOnly, Of: "of the Outer Ring"},
	// Sovereign nullsec.
	"Delve": {Class: regionPlain}, "Fountain": {Class: regionPlain},
	"Pure Blind": {Class: regionPlain}, "Feythabolis": {Class: regionPlain},
	"Scalding Pass": {Class: regionPlain}, "Immensea": {Class: regionPlain},
	"Fade": {Class: regionWord}, "Querious": {Class: regionPlain},
	"Omist": {Class: regionPlain}, "Detorid": {Class: regionPlain},
	"Insmother": {Class: regionPlain}, "Impass": {Class: regionWord},
	"Deklein": {Class: regionPlain}, "Esoteria": {Class: regionPlain},
	"Cloud Ring": {Class: regionPlain}, "Branch": {Class: regionWord},
	"Paragon Soul": {Class: regionPlain}, "Period Basis": {Class: regionPlain},
	"Wicked Creek": {Class: regionPlain}, "Tribute": {Class: regionWord},
	"Catch": {Class: regionWord}, "Vale of the Silent": {Class: regionOfOnly, Of: "of the Vale of the Silent"},
	"Cache": {Class: regionWord}, "Tenal": {Class: regionPlain},
	"Tenerifis": {Class: regionPlain}, "Geminate": {Class: regionPlain},
	"Providence": {Class: regionPlain},
	// Never: Jove space and code names (J7HZ-F, A821-A, UUA-F4, 87-1CW, S-I6VU,
	// LQ-0QN, 6HL8-L, 7-KXBJ, VU-WU2, G5KW-3, H3J8-U).
}

// ── what a name must not borrow ─────────────────────────────────────────────

// brandLoreTokens are lore proper nouns, lowercased: one in a name is rejected
// wherever it came from. NPC-corporation and faction names from the database are
// added at runtime (brandLore); this list holds without a database.
var brandLoreTokens = []string{
	"amarr", "amarrian", "caldari", "gallente", "gallentean", "minmatar", "matari",
	"jove", "jovian", "ammatar", "khanid", "intaki", "thukker", "sansha", "sansha's",
	"guristas", "serpentis", "angel", "angels", "blood", "concord", "ded", "scc",
	"interbus", "mordu", "mordu's", "sisters", "kador", "sarum", "ardishapur",
	"tash-murkon", "kor-azor", "ishukone", "kaalakiota", "sukuuvestaa", "lai",
	"hyasyoda", "wiyrkomi", "nugoeihuvi", "zainou", "ytiri", "perkone", "poksu",
	"viziam", "carthum", "duvolle", "roden", "allotek", "poteque", "egonics",
	"creodron", "quafe", "aliastra", "garoun", "luminaire", "caille", "hedion",
	"eifyr", "zoar", "ducia", "nurtura", "nefantar", "krusual", "sebiestor",
	"vherokior", "brutor", "genolution", "chemal", "fedmart", "pend", "shapeset",
	"minedrill", "echelon", "pator", "nation", "empire", "republic", "federation",
	"federal", "imperial", "kingdom", "state", "navy", "alliance", "directorate",
	"emperor", "tribe",
}

// brandNPCPhraseIgnore are NPC or faction names that are ordinary words this
// vocabulary uses: blocking "ORE" as a phrase would block every "Ore Works".
var brandNPCPhraseIgnore = map[string]bool{"ore": true, "unknown": true}

// brandBlockPhrases are real player organisations of the original game and
// real-world names: matched as whole words, case-insensitive. A name is not
// ours to reuse because it fits the grammar.
var brandBlockPhrases = []string{
	// Player organisations.
	"goon", "goons", "goonswarm", "goonfleet", "band of brothers", "pandemic legion",
	"pandemic horde", "test alliance", "brave newbies", "brave collective",
	"northern coalition", "razor alliance", "ascendant frontier", "lotka volterra",
	"red alliance", "morsus mihi", "mercenary coalition", "free trade corp",
	"evil thug", "something awful", "brick squad", "circle of two", "dreddit",
	"black legion", "snuffed out", "hard knocks", "the initiative", "fraternity",
	"solar fleet", "against all authorities", "triumvirate", "tau ceti",
	"atlas alliance", "interstellar alcohol conglomerate", "ushra'khan", "ushrakhan",
	"huzzah", "the bastards", "curatores veritatis", "rote kapelle", "hydra reloaded",
	"shadow cartel", "kenzoku", "sniggerdly", "agony unleashed", "ev0ke", "evoke",
	"eve university", "red federation", "the kadeshi", "ministry of love",
	// Real-world and fiction.
	"weyland", "yutani", "tyrell", "umbrella corporation", "cyberdyne", "skynet",
	"rebel alliance", "starfleet", "jedi", "sith", "klingon", "borg", "spacex",
	"blue origin", "lockheed", "boeing", "halliburton", "blackwater", "wagner",
	"enron", "exxon", "microsoft", "google", "apple", "amazon", "tesla", "nike",
	"disney", "marvel", "pepsi", "coca", "nintendo", "sony",
}

// brandProfanity is a short token list over the finished name. The pools are
// curated; this is for a surname that rhymes with something.
var brandProfanity = []string{
	"fuck", "shit", "cunt", "cock", "dick", "piss", "arse", "ass", "bitch",
	"slut", "whore", "fag", "nigger", "nazi", "rape", "porn", "anal", "tits",
}

// ── logos ───────────────────────────────────────────────────────────────────
//
// Shape numbers are the ESP picker's "Shape N" (res:/…/LayerShape/N.blue,
// graphicID = 415+N). Slot 3 is the bottom layer.

// logoSlotTop / logoSlotBottom: which layers a shape may be painted on.
const (
	logoSlotTop    uint8 = 1 << iota // slots 1 and 2
	logoSlotBottom                   // slot 3
)

// logoShapeSlots is the whole shape rule set. Shapes 33-56 and 64-96 are
// bottom plates; everything else with art is a top layer. 0 is the empty
// plate and 97-99 are art-less placeholders, so they are absent.
var logoShapeSlots = func() map[int]uint8 {
	m := map[int]uint8{}
	set := func(lo, hi int, mask uint8) {
		for n := lo; n <= hi; n++ {
			m[n] = mask
		}
	}
	set(1, 32, logoSlotTop)
	set(33, 56, logoSlotBottom)
	set(57, 63, logoSlotTop)
	set(64, 96, logoSlotBottom)
	set(100, 163, logoSlotTop)
	return m
}()

const logoShapeGraphicBase = 415

// logoColourHex is the client's own LayerColor diffuse per colour graphicID (the
// same values as esp/logolib.go). 682 Thin Black has no diffuse and is left out:
// the client would raise on it.
var logoColourHex = map[int64]string{
	671: "#202020", 672: "#978059", 673: "#a9d1ff", 674: "#ffffff",
	675: "#494949", 676: "#5686ff", 677: "#ffb33c", 678: "#919a99",
	679: "#ff7800", 680: "#970000", 681: "#7e7f7e", 683: "#7e7f7e",
	684: "#e7e7e7", 685: "#ffb33c",
}

// logoPalettes are the colours each race (and the outlaws) paints with.
var logoPalettes = map[string][]int64{
	tvAmarr:    {672, 677, 685, 684, 680, 671},
	tvCaldari:  {676, 673, 681, 678, 684, 671},
	tvGallente: {676, 673, 684, 672, 681, 677},
	tvMinmatar: {680, 679, 675, 671, 678, 672},
	tvNeutral:  {671, 672, 673, 674, 675, 676, 677, 678, 679, 680, 681, 684},
	"outlaw":   {680, 671, 679, 675, 678, 684},
}
